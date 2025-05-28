package util

import (
	"errors"
	"fmt"

	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// ListenFdsEnvKey is the environment variable key used to pass listening file descriptors.
	ListenFdsEnvKey = "LISTEN_FDS"
	// ReadinessPipeEnvKey is the environment variable key for the readiness pipe FD.
	ReadinessPipeEnvKey = "READINESS_PIPE_FD"
)

// isCloexecSet checks if the FD_CLOEXEC flag is set on the given file descriptor.
func isCloexecSet(fd uintptr) (bool, error) {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
	if errno != 0 {
		return false, fmt.Errorf("fcntl F_GETFD failed for fd %d: %w", fd, errno)
	}
	return (flags & syscall.FD_CLOEXEC) != 0, nil
}

// ErrPipeFDEnvVarNotSet indicates that the expected environment variable for a pipe FD was not set.
var ErrPipeFDEnvVarNotSet = errors.New("pipe FD environment variable not set")

// SetCloexec sets or clears the close-on-exec flag for a file descriptor.
// This is crucial for ensuring that inherited file descriptors (like listening sockets)
// are not closed when a new process is exec'd during a zero-downtime upgrade.
// On POSIX systems, FD_CLOEXEC determines if the file descriptor should be
// closed when execve(2) is called. We want to clear this flag for sockets
// that need to be passed to a child process.
func SetCloexec(fd uintptr, enabled bool) error {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
	if errno != 0 {
		return fmt.Errorf("fcntl F_GETFD failed: %w", errno)
	}

	if enabled {
		flags |= syscall.FD_CLOEXEC // Set FD_CLOEXEC
	} else {
		flags &^= syscall.FD_CLOEXEC // Clear FD_CLOEXEC
	}

	_, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETFD, flags)
	if errno != 0 {
		return fmt.Errorf("fcntl F_SETFD failed: %w", errno)
	}
	return nil
}

// getFileFromListener attempts to get the underlying *os.File from a net.Listener.
// This is needed to access the raw file descriptor.
// The returned *os.File is a duplicate of the listener's file descriptor
// and MUST be closed by the caller.
func getFileFromListener(l net.Listener) (*os.File, error) {
	switch typedListener := l.(type) {
	case *net.TCPListener:
		return typedListener.File()
	case *net.UnixListener:
		return typedListener.File()
	// Add other common listener types if they become relevant for the server.
	default:
		return nil, fmt.Errorf("unsupported listener type for GetFile: %T", typedListener)
	}
}

// NewListenerFromFD creates a net.Listener from an inherited file descriptor.
// It ensures that the file descriptor managed by the returned listener
// does NOT have FD_CLOEXEC set, making it suitable for further handoff
// in subsequent zero-downtime restarts.
// The provided fd uintptr is assumed to be a valid, open file descriptor
// for a listening socket.
func NewListenerFromFD(fd uintptr) (net.Listener, error) {
	// Step 1: Ensure FD_CLOEXEC is cleared on the input FD *before* creating os.File or net.Listener.
	// This makes the FD suitable for inheritance by net.FileListener.
	if err := SetCloexec(fd, false); err != nil {
		return nil, fmt.Errorf("failed to clear FD_CLOEXEC on input FD %d: %w", fd, err)
	}

	// Step 2: Create an os.File from the raw file descriptor.
	// The name is arbitrary and primarily for debugging.
	file := os.NewFile(fd, fmt.Sprintf("listener-from-fd-%d", fd))
	if file == nil {
		// This can happen if fd is not a valid descriptor, though SetCloexec might have caught it.
		return nil, fmt.Errorf("os.NewFile returned nil for FD %d after clearing CLOEXEC", fd)
	}

	// Step 3: Create a net.Listener from the os.File.
	// net.FileListener takes ownership of the file's descriptor if successful.
	// Since FD_CLOEXEC was cleared on `fd` *before* `os.NewFile`, the `file`
	// (and thus the `listener`) will operate on an FD without CLOEXEC.
	listener, err := net.FileListener(file)
	if err != nil {
		// If net.FileListener fails, we must close the os.File, as it still owns the FD.
		// The original fd (uintptr) is managed by 'file' now.
		file.Close()
		return nil, fmt.Errorf("net.FileListener failed for FD %d: %w", fd, err)
	}
	// If net.FileListener succeeds, 'listener' now owns the FD from 'file'.
	// 'file' itself should not be closed directly by us from this point,
	// as closing 'listener' will handle closing the underlying FD.

	return listener, nil
}

// CreateListener creates a net.Listener on the given address.
// It ensures that the underlying file descriptor does NOT have FD_CLOEXEC set,
// making it suitable for passing to a child process during zero-downtime restarts.
func CreateListener(network, address string) (net.Listener, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("unsupported network type: %s, only 'tcp', 'tcp4', or 'tcp6' are supported for CreateListener", network)
	}

	// Step 1: Create a temporary listener.
	// The Go standard library net.Listen functions (as of Go 1.11+)
	// automatically set FD_CLOEXEC on new sockets.
	tempListener, err := net.Listen(network, address)
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary listener on %s %s: %w", network, address, err)
	}

	// Step 2: Get the *os.File (and its FD) from the temporary listener.
	// This *os.File is a duplicate of the listener's file descriptor.
	listenerFile, errFile := getFileFromListener(tempListener)
	if errFile != nil {
		tempListener.Close() // Clean up the temporary listener
		return nil, fmt.Errorf("failed to get *os.File from temporary listener: %w", errFile)
	}
	// listenerFile now owns a duplicated FD.

	fd := listenerFile.Fd()

	// Step 3: Close the temporary listener.
	// This closes the *original* FD that tempListener was using.
	// After this, listenerFile (and its fd) is the only reference to the socket binding.
	if errCloseTemp := tempListener.Close(); errCloseTemp != nil {
		listenerFile.Close() // Clean up the duplicated FD as well
		return nil, fmt.Errorf("failed to close temporary listener on %s %s: %w", network, address, errCloseTemp)
	}

	// Step 4: Clear FD_CLOEXEC on the (duplicated, now sole) FD.
	if errSetCloexec := SetCloexec(fd, false); errSetCloexec != nil {
		listenerFile.Close() // Close the FD if SetCloexec fails
		return nil, fmt.Errorf("failed to clear FD_CLOEXEC for listener (fd %d): %w", fd, errSetCloexec)
	}

	// Step 5: Create the final net.Listener from the os.File.
	// This finalListener will use 'fd', which now has FD_CLOEXEC cleared.
	// net.FileListener takes ownership of listenerFile's FD if successful.
	finalListener, errFileListener := net.FileListener(listenerFile)
	if errFileListener != nil {
		listenerFile.Close() // Close listenerFile if net.FileListener fails
		return nil, fmt.Errorf("net.FileListener failed for fd %d: %w", fd, errFileListener)
	}
	// If net.FileListener succeeds, finalListener owns the FD.
	// listenerFile should not be closed by us anymore.

	return finalListener, nil
}

// GetInheritedListeners retrieves listening socket file descriptors passed via
// the LISTEN_FDS environment variable and converts them into net.Listener objects.
// It expects FDs to be colon-separated numbers.
// It also ensures FD_CLOEXEC is NOT set on these FDs.
func GetInheritedListeners() ([]net.Listener, error) {
	fdsEnv := os.Getenv(ListenFdsEnvKey)
	if fdsEnv == "" {
		return nil, nil // No inherited FDs
	}

	fdStrings := strings.Split(fdsEnv, ":")
	listeners := make([]net.Listener, 0, len(fdStrings))

	for _, fdStr := range fdStrings {
		fdInt, err := strconv.Atoi(fdStr)
		if err != nil {
			// Clean up already created listeners if one FD is invalid
			for _, l := range listeners {
				l.Close()
			}
			return nil, fmt.Errorf("invalid FD in %s: %s (%w)", ListenFdsEnvKey, fdStr, err)
		}
		fd := uintptr(fdInt)

		// Ensure FD_CLOEXEC is not set (it should have been cleared by the parent)
		// This is more of a sanity check or a re-application if needed.
		if err := SetCloexec(fd, false); err != nil {
			// Clean up
			for _, l := range listeners {
				l.Close()
			}
			// Close the current FD as well; os.NewFile will dup it, so closing the original int FD is complex.
			// Best to let the OS handle it on exit if this path is hit, or rely on FileListener failing.
			return nil, fmt.Errorf("failed to clear FD_CLOEXEC for inherited FD %d: %w", fd, err)
		}

		// Create a file from the FD. The file takes ownership.
		// The name "inherited" is arbitrary.
		file := os.NewFile(fd, fmt.Sprintf("inherited-listener-%d", fd))
		if file == nil {
			// This should not happen if fd is valid
			for _, l := range listeners {
				l.Close()
			}
			return nil, fmt.Errorf("os.NewFile returned nil for FD %d", fd)
		}

		listener, err := net.FileListener(file)
		// If FileListener succeeds, it takes ownership of file's FD.
		// If it fails, we need to close the file to release the FD.
		if err != nil {
			file.Close() // Important to close file if FileListener fails
			for _, l := range listeners {
				l.Close()
			}
			return nil, fmt.Errorf("failed to create net.Listener from inherited FD %d: %w", fd, err)
		}
		listeners = append(listeners, listener)
	}

	return listeners, nil
}

// ParseInheritedListenerFDs retrieves a list of file descriptor numbers
// passed via the specified environment variable.
// It expects FDs to be colon-separated numbers.
func ParseInheritedListenerFDs(envVarName string) ([]uintptr, error) {
	fdsEnv := os.Getenv(envVarName)
	if fdsEnv == "" {
		return nil, nil // No inherited FDs specified by this variable
	}

	fdStrings := strings.Split(fdsEnv, ":")
	fds := make([]uintptr, 0, len(fdStrings))

	for _, fdStr := range fdStrings {
		fdInt, err := strconv.Atoi(fdStr)
		if err != nil {
			// Return nil for the FDs list if any part of the string is invalid
			return nil, fmt.Errorf("invalid FD number in environment variable %s (value: %q): %s (%w)", envVarName, fdsEnv, fdStr, err)
		}
		if fdInt < 0 { // File descriptors are non-negative.
			return nil, fmt.Errorf("invalid negative FD number in environment variable %s (value: %q): %d", envVarName, fdsEnv, fdInt)
		}
		fds = append(fds, uintptr(fdInt))
	}
	return fds, nil
}

// PrepareExecEnv prepares environment variables for the child process,
// including the listening socket FDs and readiness pipe FD.
// It takes a slice of net.Listener and the write end of the readiness pipe (as an *os.File).
// The returned slice of strings is suitable for `os.ProcAttr.Env`.
// The FDs for listeners and pipe must have FD_CLOEXEC cleared before calling this.
func PrepareExecEnv(currentEnv []string, listeners []net.Listener, readinessPipeWrite *os.File) ([]string, error) {
	newEnv := make([]string, 0, len(currentEnv)+2) // +2 for LISTEN_FDS and READINESS_PIPE_FD

	// Copy existing environment, but filter out our specific keys to avoid duplication/confusion
	// if they somehow exist from a grandparent.
	for _, envVar := range currentEnv {
		if !strings.HasPrefix(envVar, ListenFdsEnvKey+"=") &&
			!strings.HasPrefix(envVar, ReadinessPipeEnvKey+"=") {
			newEnv = append(newEnv, envVar)
		}
	}

	var fdStrings []string
	for _, l := range listeners {
		// Get the file descriptor from the listener
		// This requires a type assertion to TCPListener, UDPConn, or UnixListener.
		// Assuming TCPListener for now as it's most common for HTTP servers.
		var file *os.File
		var err error
		switch typedListener := l.(type) {
		case *net.TCPListener:
			file, err = typedListener.File()
		// Add cases for UnixListener, etc., if needed
		default:
			return nil, fmt.Errorf("unsupported listener type: %T", l)
		}

		if err != nil {
			return nil, fmt.Errorf("failed to get file from listener: %w", err)
		}
		// Defer closing the duplicated FD from .File() call
		// We only need the FD number, not to keep the os.File object around for the child.
		// The original listener's FD remains open.
		defer file.Close()
		fdStrings = append(fdStrings, fmt.Sprintf("%d", file.Fd()))
	}

	if len(fdStrings) > 0 {
		newEnv = append(newEnv, fmt.Sprintf("%s=%s", ListenFdsEnvKey, strings.Join(fdStrings, ":")))
	}

	if readinessPipeWrite != nil {
		// The readinessPipeWrite FD itself should have FD_CLOEXEC cleared too.
		// This is usually done when the pipe is created.
		newEnv = append(newEnv, fmt.Sprintf("%s=%d", ReadinessPipeEnvKey, readinessPipeWrite.Fd()))
	}

	return newEnv, nil
}

// CreateListenerAndGetFD creates a net.Listener on the given address (assuming "tcp" network)
// and returns it along with its raw file descriptor.
// FD_CLOEXEC is cleared on this descriptor so it can be inherited by a child process.
// The caller (parent process) uses the net.Listener, and can pass the uintptr FD
// number to a child process (which would then typically use net.FileListener on its end).

// CreateListenerAndGetFD creates a net.Listener on the given address (assuming "tcp" network)
// and returns it along with its raw file descriptor.
// FD_CLOEXEC is cleared on this descriptor so it can be inherited by a child process.
// The caller (parent process) uses the net.Listener, and can pass the uintptr FD
// number to a child process (which would then typically use net.FileListener on its end).
var CreateListenerAndGetFD = func(address string) (net.Listener, uintptr, error) {
	network := "tcp" // Defaulting to "tcp" for an HTTP server

	// Step 1: Create a temporary listener.
	// The Go standard library net.Listen functions (as of Go 1.11+)
	// automatically set FD_CLOEXEC on new sockets.
	tempListener, err := net.Listen(network, address)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create temporary listener on %s %s: %w", network, address, err)
	}

	// Step 2: Get the file descriptor from the temporary listener.
	// This requires type assertion. Supporting TCPListener as it's standard for HTTP.
	tcpListener, ok := tempListener.(*net.TCPListener)
	if !ok {
		// If it's not a TCPListener, we can't call .File() in this simple way.
		// Other listener types (like UnixListener) also have a File() method.
		// A more robust implementation might use a type switch or interface check.
		tempListener.Close()
		return nil, 0, fmt.Errorf("listener on %s %s is not a TCPListener (type: %T), cannot get FD", network, address, tempListener)
	}

	file, err := tcpListener.File() // This duplicates the file descriptor.
	if err != nil {
		tempListener.Close()
		return nil, 0, fmt.Errorf("failed to get listener file from TCPListener on %s %s: %w", network, address, err)
	}
	// `file` now owns the duplicated FD.

	// Step 3: Close the temporary listener.
	// This is crucial. It closes the *original* FD associated with tempListener.
	// If we don't do this, and then create a new listener from `file.Fd()`,
	// we might have two listeners on the same port, or `net.FileListener` might fail
	// with "address already in use" if the OS hasn't fully released the original socket.
	if err := tempListener.Close(); err != nil {
		file.Close() // Clean up the duplicated FD as well
		// Log this error, as it's unusual but might not be fatal for the overall goal if FileListener later works.
		// However, for robustness, treat as failure.
		return nil, 0, fmt.Errorf("failed to close temporary listener on %s %s: %w", network, address, err)
	}
	// Now, only `file` (with its duplicated FD) potentially holds the port binding.

	// Step 4: Get the raw FD from the os.File.
	listenerFD := file.Fd()

	// Step 5: Clear FD_CLOEXEC on the duplicated FD.
	// This ensures the FD can be inherited by child processes.
	if err := SetCloexec(listenerFD, false); err != nil {
		file.Close() // Close the duplicated FD if SetCloexec fails.
		return nil, 0, fmt.Errorf("failed to clear FD_CLOEXEC for FD %d from %s %s: %w", listenerFD, network, address, err)
	}

	// Step 6: Create the final net.Listener from the os.File (which owns listenerFD).
	// This finalListener will use listenerFD, which now has FD_CLOEXEC cleared.
	finalListener, err := net.FileListener(file)
	if err != nil {
		file.Close() // Close the duplicated FD if FileListener fails.
		return nil, 0, fmt.Errorf("failed to create final listener from file (FD %d) for %s %s: %w", listenerFD, network, address, err)
	}
	// If net.FileListener succeeds, it takes ownership of `file`'s FD.
	// `file.Close()` should NOT be called by us after this point if successful,
	// as closing `finalListener` will close the underlying FD.

	// Step 7: Return the final listener and its FD.
	return finalListener, listenerFD, nil
}

// CreateReadinessPipe creates a pipe for readiness signaling.
// The parent process receives the read-end of the pipe (*os.File) to wait on.
// The child process receives the file descriptor number (uintptr) of the write-end.
// FD_CLOEXEC is set on the parent's read-end and cleared on the child's write-end FD.
// The child signals readiness by closing its write-end FD.
// The *os.File for the write-end is closed by this function after its FD is extracted
// and configured.
func CreateReadinessPipe() (parentReadPipe *os.File, childWriteFD uintptr, err error) {
	r, w, errPipe := os.Pipe()
	if errPipe != nil {
		return nil, 0, fmt.Errorf("failed to create readiness pipe: %w", errPipe)
	}

	// Ensure cleanup on error.
	// If any step fails, both pipe ends (r and w) should be closed.

	// Parent's read end should be closed on exec (FD_CLOEXEC = true)
	if errSetCloexecRead := SetCloexec(r.Fd(), true); errSetCloexecRead != nil {
		r.Close()
		w.Close()
		return nil, 0, fmt.Errorf("failed to set FD_CLOEXEC on readiness pipe read end: %w", errSetCloexecRead)
	}

	// Child's write end FD must be inherited (FD_CLOEXEC = false)
	fdForChild := w.Fd()
	if errSetCloexecWrite := SetCloexec(fdForChild, false); errSetCloexecWrite != nil {
		r.Close() // Close parent's read end
		w.Close() // Close child's write end *os.File
		return nil, 0, fmt.Errorf("failed to clear FD_CLOEXEC on readiness pipe write end FD %d: %w", fdForChild, errSetCloexecWrite)
	}

	// The child process only needs the FD number.
	// Close the *os.File wrapper for the write end in the parent,
	// as the FD itself (fdForChild) will be passed.
	if errCloseW := w.Close(); errCloseW != nil {
		// If w.Close() fails, it's problematic. The FD might still be valid for the child,
		// but resources in the parent might not be cleaned up correctly.
		// Parent should still close its read end.
		r.Close()
		// Since SetCloexec on fdForChild succeeded, the child could potentially use it.
		// However, this state is unclean. Treat as an error.
		return nil, 0, fmt.Errorf("failed to close child's write end os.File (FD %d) after SetCloexec: %w", fdForChild, errCloseW)
	}

	return r, fdForChild, nil
}

// GetInheritedReadinessPipeFD retrieves the readiness pipe FD from environment.
// The child process uses this to signal readiness by closing it.
func GetInheritedReadinessPipeFD() (fd uintptr, found bool, err error) {
	fdStr := os.Getenv(ReadinessPipeEnvKey)
	if fdStr == "" {
		return 0, false, nil
	}

	fdInt, errConv := strconv.Atoi(fdStr)
	if errConv != nil {
		return 0, true, fmt.Errorf("invalid FD for readiness pipe in env %s: %s (%w)", ReadinessPipeEnvKey, fdStr, errConv)
	}
	if fdInt < 0 { // Technically FDs are non-negative, but defensive.
		return 0, true, fmt.Errorf("invalid negative FD for readiness pipe: %d", fdInt)
	}
	return uintptr(fdInt), true, nil
}

// GetChildWritePipeFD retrieves a file descriptor number from the specified environment variable.
// It is intended for child processes to get pipe FDs (e.g., for readiness signaling).
// If the environment variable is not set, it returns ErrPipeFDEnvVarNotSet.
// If the value is not a valid non-negative integer, it returns an error detailing the issue.
func GetChildWritePipeFD(envVarName string) (uintptr, error) {
	fdStr := os.Getenv(envVarName)
	if fdStr == "" {
		return 0, fmt.Errorf("%w: %s", ErrPipeFDEnvVarNotSet, envVarName)
	}

	fdInt, err := strconv.Atoi(fdStr)
	if err != nil {
		return 0, fmt.Errorf("invalid integer value for FD in environment variable %s (%q): %w", envVarName, fdStr, err)
	}

	if fdInt < 0 { // FDs are non-negative.
		return 0, fmt.Errorf("invalid negative FD value in environment variable %s: %d", envVarName, fdInt)
	}
	return uintptr(fdInt), nil
}

// IsAddrInUse checks if the error indicates an "address already in use" condition.
func IsAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	// Check for syscall.EADDRINUSE specifically
	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		if sysErr.Err == syscall.EADDRINUSE {
			return true
		}
	}
	// Fallback for other error types or wrapped errors that might present as string
	// This is less reliable than checking the syscall error code.
	// Go's net package often wraps these, e.g. *net.OpError
	return strings.Contains(strings.ToLower(err.Error()), "address already in use")
}

// SignalChildReadyByClosingFD closes the given file descriptor.
// This is used by a child process to signal readiness to its parent,
// typically by closing the write-end of a readiness pipe.
func SignalChildReadyByClosingFD(fd uintptr) error {
	// syscall.Close directly takes an int, so we cast uintptr to int.
	// On POSIX systems, file descriptors are small non-negative integers.
	// This cast is generally safe for valid FDs.
	err := syscall.Close(int(fd))
	if err != nil {
		return fmt.Errorf("failed to close readiness FD %d: %w", fd, err)
	}
	return nil
}

// WaitForChildReadyPipeClose waits for the child process to signal readiness by closing
// its end of the readiness pipe. This function is called by the parent process.
// It reads from the parent's read-end of the pipe. An EOF indicates the child
// has closed its write-end. The function returns an error if the timeout is reached
// or if any other read error (except EOF) occurs.
func WaitForChildReadyPipeClose(parentReadPipe *os.File, timeout time.Duration) error {
	if parentReadPipe == nil {
		return errors.New("parentReadPipe cannot be nil")
	}

	readDone := make(chan error, 1) // Buffered channel to prevent goroutine leak

	go func() {
		// A small buffer is sufficient as we are only looking for EOF or an error.
		// The actual data read doesn't matter.
		buf := make([]byte, 1)
		_, err := parentReadPipe.Read(buf)
		readDone <- err // Send the error (which will be io.EOF on success)
	}()

	select {
	case err := <-readDone:
		if err == io.EOF {
			return nil // Child closed the pipe, success
		}
		// Any other error from read (or nil if read 0 bytes without EOF, though unlikely for a pipe)
		return fmt.Errorf("error reading from readiness pipe: %w", err)
	case <-time.After(timeout):
		// Ensure the goroutine doesn't leak if timeout occurs first.
		// This is a bit tricky as parentReadPipe.Read() might block indefinitely.
		// A more robust solution might involve setting a deadline on the pipe read if possible,
		// or closing the pipe from this side (though that might have other implications).
		// For now, relying on the goroutine exiting once the Read unblocks eventually.
		// This simple timeout handling is common but has this potential goroutine leak.
		// A common pattern is to also try to close the pipe or set a read deadline.
		// Since os.File doesn't directly support SetReadDeadline for pipes in a portable way
		// like net.Conn, this is a known limitation of simpler approaches.
		// For this specific use case (parent waiting for child to close pipe), it's usually okay
		// as the child *will* close it or die, unblocking the Read.
		return fmt.Errorf("timeout waiting for child readiness signal (waited %v)", timeout)
	}
}

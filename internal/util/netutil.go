package util

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const (
	// ListenFdsEnvKey is the environment variable key used to pass listening file descriptors.
	ListenFdsEnvKey = "LISTEN_FDS"
	// ReadinessPipeEnvKey is the environment variable key for the readiness pipe FD.
	ReadinessPipeEnvKey = "READINESS_PIPE_FD"
)

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
	// Step 1: Create an os.File from the raw file descriptor.
	// The name is arbitrary and primarily for debugging.
	file := os.NewFile(fd, fmt.Sprintf("listener-from-fd-%d", fd))
	if file == nil {
		// This can happen if fd is not a valid descriptor.
		return nil, fmt.Errorf("os.NewFile returned nil for FD %d", fd)
	}

	// Step 2: Create a net.Listener from the os.File.
	// net.FileListener takes ownership of the file's descriptor if successful.
	listener, err := net.FileListener(file)
	if err != nil {
		file.Close() // Must close the os.File if net.FileListener fails, as it still owns the FD.
		return nil, fmt.Errorf("net.FileListener failed for FD %d: %w", fd, err)
	}
	// If net.FileListener succeeds, 'listener' now owns the FD from 'file'.
	// 'file' itself should not be closed directly by us from this point,
	// as closing 'listener' will handle closing the underlying FD.

	// Step 3: Ensure FD_CLOEXEC is cleared on the FD actually managed by the listener.
	// The task specification implies that net.FileListener might set FD_CLOEXEC.
	// To counteract this, we get a (duplicate) *os.File from the listener,
	// get its FD, clear CLOEXEC on that FD, and then close this temporary *os.File.
	// The original listener will continue to use its internal FD, which should now
	// reflect the cleared CLOEXEC status.
	tempListenerFile, err := getFileFromListener(listener)
	if err != nil {
		// The listener was created, but we can't confirm/set its FD_CLOEXEC status.
		// To be safe, close the listener and report the error.
		listener.Close()
		return nil, fmt.Errorf("failed to get file from listener (for FD %d) to clear CLOEXEC: %w", fd, err)
	}
	// tempListenerFile must be closed to release the duplicated FD.
	defer tempListenerFile.Close()

	listenerInternalFD := tempListenerFile.Fd()
	if err := SetCloexec(listenerInternalFD, false); err != nil {
		listener.Close() // Close the main listener as we failed to configure its FD properly.
		return nil, fmt.Errorf("failed to clear FD_CLOEXEC on listener's internal FD %d (from original FD %d): %w", listenerInternalFD, fd, err)
	}

	return listener, nil
}

// CreateListener creates a net.Listener on the given address.
// It ensures that the underlying file descriptor does NOT have FD_CLOEXEC set,
// making it suitable for passing to a child process during zero-downtime restarts.
func CreateListener(network, address string) (net.Listener, error) {
	// The Go standard library net.Listen functions (as of Go 1.11+)
	// automatically set FD_CLOEXEC on new sockets.
	// To prevent this for sockets we want to inherit, we'd typically need to:
	// 1. Create the socket using syscalls directly (socket, bind, listen).
	// 2. Call SetCloexec(fd, false) on it.
	// 3. Convert the FD to a net.Listener using net.FileListener.

	// However, a simpler approach if `net.ListenConfig.Control` is available (Go 1.11+)
	// is to use it to clear FD_CLOEXEC right after the socket is created by the stdlib.
	// For older Go versions, manual socket creation is needed. Assuming Go 1.11+ for simplicity.
	var listener net.Listener
	var err error

	// Temporary listener to get the FD
	tempListener, err := net.Listen(network, address)
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary listener: %w", err)
	}

	// Get the file descriptor
	tcpListener, ok := tempListener.(*net.TCPListener)
	if !ok {
		tempListener.Close()
		return nil, fmt.Errorf("listener is not a TCPListener, cannot get FD")
	}
	file, err := tcpListener.File()
	if err != nil {
		tempListener.Close()
		return nil, fmt.Errorf("failed to get listener file: %w", err)
	}
	// Important: Closing tempListener does not close the file descriptor `file.Fd()`,
	// as `File()` duplicates it. The `file` now owns the FD.
	// We must close tempListener to release the original listening port binding
	// *before* we create the final listener from `file`.
	tempListener.Close()

	fd := file.Fd()

	// Clear FD_CLOEXEC on the duplicated FD
	if err := SetCloexec(fd, false); err != nil {
		file.Close() // Close the FD if we can't set cloexec
		return nil, fmt.Errorf("failed to clear FD_CLOEXEC: %w", err)
	}

	// Create the final listener from the file descriptor that has FD_CLOEXEC cleared.
	// The `file` (and its FD) will be managed by this new listener.
	listener, err = net.FileListener(file)
	if err != nil {
		file.Close() // Close the FD if FileListener fails
		return nil, fmt.Errorf("failed to create listener from file: %w", err)
	}
	// file.Close() should not be called here if net.FileListener succeeds,
	// as the listener now owns the FD.

	return listener, nil
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
func CreateListenerAndGetFD(address string) (net.Listener, uintptr, error) {
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

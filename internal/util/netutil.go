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

// CreateReadinessPipe creates a pipe for readiness signaling.
// The read end is for the parent to wait on, the write end for the child to close.
// It ensures FD_CLOEXEC is set on the read end (parent's side) and cleared on
// the write end (child's side to be inherited).
func CreateReadinessPipe() (parentRead *os.File, childWrite *os.File, err error) {
	r, w, errPipe := os.Pipe()
	if errPipe != nil {
		return nil, nil, fmt.Errorf("failed to create readiness pipe: %w", errPipe)
	}

	// Parent's read end should be closed on exec if parent execs something else (unlikely here, but good practice)
	if err := SetCloexec(r.Fd(), true); err != nil {
		r.Close()
		w.Close()
		return nil, nil, fmt.Errorf("failed to set FD_CLOEXEC on readiness pipe read end: %w", err)
	}

	// Child's write end must be inherited, so clear FD_CLOEXEC
	if err := SetCloexec(w.Fd(), false); err != nil {
		r.Close()
		w.Close()
		return nil, nil, fmt.Errorf("failed to clear FD_CLOEXEC on readiness pipe write end: %w", err)
	}

	return r, w, nil
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

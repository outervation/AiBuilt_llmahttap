package util

import (
	"net"
	"os"
	"path/filepath"
	"runtime"

	"testing"
	"time"
)

// createTestTCPListener creates a TCP listener for testing.
// If addr is empty or ":0", it listens on a random available port on localhost.
// It returns the listener and the address string it's listening on.
// The caller is responsible for closing the listener.
func createTestTCPListener(t *testing.T, addr string) (net.Listener, string) {
	t.Helper()
	if addr == "" {
		addr = "127.0.0.1:0" // Listen on a random port on localhost
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("Failed to create TCP listener on %s: %v", addr, err)
	}
	return ln, ln.Addr().String()
}

// createTestUnixListener creates a Unix domain socket listener for testing.
// It returns the listener and the path to the socket file.
// The caller is responsible for closing the listener and removing the socket file.
// A common pattern for cleanup is:
//
//	defer ln.Close()
//	defer os.Remove(socketPath)
func createTestUnixListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	// Create a temporary file path for the Unix socket
	// Using t.TempDir() ensures cleanup of the directory, but the socket file itself
	// might need explicit removal if not automatically handled by listener.Close() on all OSes.
	// For Unix domain sockets, os.Remove is typically needed.
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "test.sock")

	// Ensure the path is not too long for a Unix socket.
	// This check is OS-dependent; on Linux, it's often 108 chars including null terminator.
	// For portability and simplicity in a test, we won't implement a precise check here
	// but rely on the system call to fail if the path is too long.
	if runtime.GOOS != "windows" { // Unix domain sockets are not fully featured or common on Windows for this.
		ln, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatalf("Failed to create Unix listener at %s: %v", socketPath, err)
		}
		return ln, socketPath
	}
	// For windows, we can't reliably test unix domain sockets in this generic way.
	// We can skip tests that require this or return a dummy/error.
	// For now, let's make it fail if called on Windows.
	t.Fatalf("createTestUnixListener is not supported on Windows for this test setup")
	return nil, ""
}

// getFdFromListener extracts the raw file descriptor (uintptr) from a net.Listener.
// It calls t.Fatalf if it fails to get the FD.
func getFdFromListener(t *testing.T, l net.Listener) uintptr {
	t.Helper()
	var file *os.File
	var err error

	switch typedListener := l.(type) {
	case *net.TCPListener:
		file, err = typedListener.File()
	case *net.UnixListener:
		// UnixListener.File() might not always be available or behave as expected
		// depending on the Go version and OS, especially if the socket was
		// created in a certain way. However, for listeners created by net.Listen,
		// it should generally work.
		file, err = typedListener.File()
	default:
		t.Fatalf("Unsupported listener type: %T", l)
	}

	if err != nil {
		t.Fatalf("Failed to get *os.File from listener: %v", err)
	}
	if file == nil {
		t.Fatalf("Listener's File() method returned a nil *os.File")
	}

	fd := file.Fd()
	// The *os.File returned by File() is a duplicate. We must close it.
	if err := file.Close(); err != nil {
		t.Logf("Warning: failed to close temporary file from listener.File(): %v", err)
		// Continue, as we got the FD, but log the issue.
	}
	return fd
}

// withTempEnvVar temporarily sets an environment variable for the duration of fn.
// It restores the original value (or unsets if it was not set) after fn completes.
func withTempEnvVar(t *testing.T, key, value string, fn func()) {
	t.Helper()
	originalValue, wasSet := os.LookupEnv(key)

	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("Failed to set temporary environment variable %s: %v", key, err)
	}

	defer func() {
		if wasSet {
			if err := os.Setenv(key, originalValue); err != nil {
				// Log error, as test cleanup failure shouldn't mask test failure.
				t.Logf("Error restoring environment variable %s to '%s': %v", key, originalValue, err)
			}
		} else {
			if err := os.Unsetenv(key); err != nil {
				t.Logf("Error unsetting environment variable %s: %v", key, err)
			}
		}
	}()

	fn()
}

func TestSetCloexec(t *testing.T) {
	// Create a pipe. Pipe FDs are typically not CLOEXEC by default on creation,
	// but this can vary. We'll explicitly set and clear it.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	defer r.Close()
	defer w.Close()

	fd := r.Fd() // Test with the read-end of the pipe

	// Test 1: Enable CLOEXEC
	err = SetCloexec(fd, true)
	if err != nil {
		t.Fatalf("SetCloexec(fd, true) failed: %v", err)
	}
	isSet, errCheck := isCloexecSet(fd)
	if errCheck != nil {
		t.Fatalf("isCloexecSet check failed after enabling: %v", errCheck)
	}
	if !isSet {
		t.Errorf("Expected FD_CLOEXEC to be set, but it was not")
	}

	// Test 2: Disable CLOEXEC
	err = SetCloexec(fd, false)
	if err != nil {
		t.Fatalf("SetCloexec(fd, false) failed: %v", err)
	}
	isSet, errCheck = isCloexecSet(fd)
	if errCheck != nil {
		t.Fatalf("isCloexecSet check failed after disabling: %v", errCheck)
	}
	if isSet {
		t.Errorf("Expected FD_CLOEXEC to be clear, but it was set")
	}

	// Test with an invalid FD (e.g., a large number unlikely to be an open FD)
	// This behavior might be OS-dependent, but fcntl should return an error.
	invalidFD := uintptr(99999)
	err = SetCloexec(invalidFD, true)
	if err == nil {
		t.Errorf("SetCloexec(invalidFD, true) expected to fail, but got nil")
	}
}

func TestCreateListenerAndGetFD(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping TestCreateListenerAndGetFD on Windows due to differences in FD_CLOEXEC handling and typical socket behavior.")
	}

	// 1. Call CreateListenerAndGetFD
	addr := "127.0.0.1:0" // Listen on a random available port
	listener, fd, err := CreateListenerAndGetFD(addr)
	if err != nil {
		t.Fatalf("CreateListenerAndGetFD failed: %v", err)
	}
	if listener == nil {
		t.Fatalf("CreateListenerAndGetFD returned a nil listener")
	}
	defer listener.Close()

	// 2. Verify FD is valid (non-zero, though FD 0,1,2 are usually stdio/stderr)
	// A simple check is that it's positive. More robust checks are complex.
	if fd <= 0 { // Checking <= 0; a valid FD from socket operations is typically > 2
		t.Errorf("CreateListenerAndGetFD returned an invalid FD: %d", fd)
	}

	// 3. Verify FD_CLOEXEC is not set
	isSet, errCheck := isCloexecSet(fd)
	if errCheck != nil {
		t.Fatalf("isCloexecSet check failed for FD %d: %v", fd, errCheck)
	}
	if isSet {
		t.Errorf("Expected FD_CLOEXEC to be clear on FD %d from CreateListenerAndGetFD, but it was set", fd)
	}

	// 4. Verify the listener is actually listening
	listeningAddr := listener.Addr().String()
	conn, errDial := net.DialTimeout("tcp", listeningAddr, 1*time.Second)
	if errDial != nil {
		t.Fatalf("Failed to connect to listener at %s: %v", listeningAddr, errDial)
	}
	// If dial succeeded, accept the connection on the listener side
	serverConn, errAccept := listener.Accept()
	if errAccept != nil {
		t.Errorf("Listener failed to accept connection: %v", errAccept)
	}
	if serverConn != nil {
		serverConn.Close()
	}
	conn.Close()

	// 5. Closing the listener is handled by defer
}

func TestNewListenerFromFD(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping TestNewListenerFromFD on Windows due to platform differences in FD handling.")
	}

	// 1. Create an initial listener and get its FD.
	// This initial listener must have FD_CLOEXEC cleared.
	initialLn, initialFD, err := CreateListenerAndGetFD("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Setup: CreateListenerAndGetFD failed: %v", err)
	}
	defer initialLn.Close() // Close the initial listener eventually.

	// Sanity check: FD_CLOEXEC should be clear on initialFD
	isSet, errCheck := isCloexecSet(initialFD)
	if errCheck != nil {
		t.Fatalf("Setup: isCloexecSet check failed for initialFD %d: %v", initialFD, errCheck)
	}
	if isSet {
		t.Fatalf("Setup: Expected FD_CLOEXEC to be clear on initialFD %d, but it was set", initialFD)
	}

	// 2. Create a new listener from this FD.
	newListener, err := NewListenerFromFD(initialFD)
	if err != nil {
		t.Fatalf("NewListenerFromFD failed for FD %d: %v", initialFD, err)
	}
	if newListener == nil {
		t.Fatal("NewListenerFromFD returned a nil listener")
	}
	defer newListener.Close()

	// 3. Verify FD_CLOEXEC is also clear on the FD that newListener is using.
	//    Since NewListenerFromFD takes initialFD, newListener *is* using initialFD.
	//    The SetCloexec(initialFD, false) inside NewListenerFromFD ensures it's clear.
	isSetNew, errCheckNew := isCloexecSet(initialFD) // Check initialFD directly
	if errCheckNew != nil {
		t.Fatalf("isCloexecSet check failed for initialFD %d (used by newListener): %v", initialFD, errCheckNew)
	}
	if isSetNew {
		t.Errorf("Expected FD_CLOEXEC to be clear on initialFD %d (used by newListener), but it was set", initialFD)
	}

	// 4. Verify the new listener is actually listening on the same address.
	// Since initialLn is still open (until defer), trying to connect to newListener
	// should work, and accepting on newListener should succeed.
	// The address of initialLn should be the same as newListener.
	if initialLn.Addr().String() != newListener.Addr().String() {
		t.Errorf("Address mismatch: initial listener at %s, new listener at %s",
			initialLn.Addr().String(), newListener.Addr().String())
	}

	listeningAddr := newListener.Addr().String()
	conn, errDial := net.DialTimeout("tcp", listeningAddr, 1*time.Second)
	if errDial != nil {
		t.Fatalf("Failed to connect to newListener at %s: %v", listeningAddr, errDial)
	}
	defer conn.Close()

	serverConn, errAccept := newListener.Accept()
	if errAccept != nil {
		t.Errorf("newListener failed to accept connection: %v", errAccept)
	}
	if serverConn != nil {
		defer serverConn.Close()
	}

	// Test with an invalid FD
	invalidFD := uintptr(99999) // A large number unlikely to be an open FD
	_, err = NewListenerFromFD(invalidFD)
	if err == nil {
		t.Errorf("NewListenerFromFD with invalid FD %d expected to fail, but got nil", invalidFD)
	}
}

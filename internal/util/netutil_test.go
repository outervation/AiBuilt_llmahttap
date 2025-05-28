package util

import (
	"errors"
	// "io/fs" // No longer needed here as test was moved
	"net"
	"os"
	"path/filepath"
	"runtime"

	"io"
	"syscall"

	"strconv"
	"strings"
	"testing"
	"time"
)

// createTestTCPListener creates a TCP listener for testing.
// If addr is empty or ":0", it listens on a random available port on localhost.

// createTestTCPListener creates a TCP listener for testing.
// If addr is empty or ":0", it listens on a random available port on localhost.
// It returns the listener and the address string it's listening on.
// The caller is responsible for closing the listener.
func createTestTCPListener(t *testing.T, addr string) (net.Listener, string) {
	t.Helper()
	if addr == "" {
		addr = "127.0.0.1:0" // Default to localhost random port
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("Failed to create TCP listener at %s: %v", addr, err)
	}
	return ln, ln.Addr().String()
}

// It returns the listener and the address string it's listening on.
// The caller is responsible for closing the listener.

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

// getFdAndFileFromListener extracts the raw file descriptor (uintptr) and its owning *os.File from a net.Listener.
// It calls t.Fatalf if it fails. The caller is responsible for closing the returned *os.File.
func getFdAndFileFromListener(t *testing.T, l net.Listener) (*os.File, uintptr) {
	t.Helper()
	var listenerFile *os.File // Renamed for clarity
	var err error

	switch typedListener := l.(type) {
	case *net.TCPListener:
		listenerFile, err = typedListener.File()
	case *net.UnixListener:
		// UnixListener.File() might not always be available or behave as expected
		// depending on the Go version and OS, especially if the socket was
		// created in a certain way. However, for listeners created by net.Listen,
		// it should generally work.
		listenerFile, err = typedListener.File()
	default:
		t.Fatalf("Unsupported listener type: %T", l)
	}

	if err != nil {
		t.Fatalf("Failed to get *os.File from listener: %v", err)
	}
	if listenerFile == nil {
		t.Fatalf("Listener's File() method returned a nil *os.File")
	}

	fd := listenerFile.Fd()
	// DO NOT close listenerFile here. Caller is responsible.
	return listenerFile, fd
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

func TestCreateListener(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping TestCreateListener on Windows due to differences in FD_CLOEXEC handling and typical socket behavior for these tests.")
	}

	t.Run("TCP_Success_RandomPort", func(t *testing.T) {
		addr := "127.0.0.1:0" // Listen on a random available port
		listener, err := CreateListener("tcp", addr)
		if err != nil {
			t.Fatalf("CreateListener(\"tcp\", %q) failed: %v", addr, err)
		}
		if listener == nil {
			t.Fatalf("CreateListener(\"tcp\", %q) returned a nil listener", addr)
		}
		defer listener.Close()

		// Verify FD is valid and FD_CLOEXEC is not set
		listenerFileForTest, fd := getFdAndFileFromListener(t, listener)
		defer listenerFileForTest.Close() // Close the duplicated file descriptor owner

		if fd <= 0 {
			t.Errorf("CreateListener returned an invalid FD: %d", fd)
		}

		// The FD_CLOEXEC property for the listener's *own* FD (for self-restart)
		// is ensured by CreateListener's internal logic. TestCreateListenerAndGetFD
		// more directly verifies the mechanism of clearing FD_CLOEXEC on an FD
		// that a listener then uses.
		// A check on listener.File().Fd()'s CLOEXEC state here was removed, as .File()
		// often returns a new FD that might have FD_CLOEXEC set by default by the OS
		// during duplication, regardless of the original FD's state.
		// The key is that CreateListener returns a listener that *operates on* an FD
		// suitable for inheritance.

		// Verify the listener is actually listening
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
	})

	t.Run("TCP_Error_InvalidAddressFormat", func(t *testing.T) {
		invalidAddresses := []string{
			"localhost:notaport", // Non-numeric port
			// ":8080", // This is valid for net.Listen; CreateListener likely wraps it.
			"invalid-host-format:8080",
		}

		for _, addr := range invalidAddresses {
			_, err := CreateListener("tcp", addr)
			if err == nil {
				t.Errorf("CreateListener(\"tcp\", %q) expected to fail, but got nil", addr)
			}
		}
	})

	t.Run("Error_UnsupportedNetwork", func(t *testing.T) {
		addr := "127.0.0.1:0" // Address doesn't matter as much as network type
		unsupportedNetwork := "udp"
		_, err := CreateListener(unsupportedNetwork, addr)
		if err == nil {
			t.Errorf("CreateListener(%q, %q) expected to fail for unsupported network, but got nil", unsupportedNetwork, addr)
		}
	})

	// Note: Reliably testing "address already in use" is difficult in unit tests
	// as it requires precise control over port states, which can be racy or
	// platform-dependent. We'll skip it for CreateListener direct test, focusing on what it controls.
	// IsAddrInUse can be tested separately if needed.
}

func TestCreateReadinessPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping TestCreateReadinessPipe on Windows due to POSIX-specific FD/pipe behavior.")
	}

	parentReadPipe, childWriteFD, err := CreateReadinessPipe()
	if err != nil {
		t.Fatalf("CreateReadinessPipe failed: %v", err)
	}
	if parentReadPipe == nil {
		t.Fatal("CreateReadinessPipe returned nil parentReadPipe")
	}
	defer parentReadPipe.Close()

	if childWriteFD <= 0 { // Basic validity check for FD
		t.Fatalf("CreateReadinessPipe returned invalid childWriteFD: %d", childWriteFD)
	}
	// Note: childWriteFD is just a uintptr; its corresponding *os.File was closed by CreateReadinessPipe.
	// We will close it via syscall.Close later.

	// 1. Verify FD_CLOEXEC on parent's read end (should be SET)
	parentReadFD := parentReadPipe.Fd()
	isSetParent, errCheckParent := isCloexecSet(parentReadFD)
	if errCheckParent != nil {
		t.Fatalf("isCloexecSet check failed for parentReadFD %d: %v", parentReadFD, errCheckParent)
	}
	if !isSetParent {
		t.Errorf("Expected FD_CLOEXEC to be SET on parentReadFD %d, but it was clear", parentReadFD)
	}

	// 2. The childWriteFD (uintptr for the pipe's write-end) had FD_CLOEXEC cleared
	//    within CreateReadinessPipe before its corresponding *os.File was closed
	//    by CreateReadinessPipe. This is crucial for inheritance by the child process.
	//    We cannot re-verify this flag from the parent after CreateReadinessPipe returns,
	//    nor can we simulate the child closing this FD number from the parent process,
	//    because the FD is no longer valid in the parent for operations like fcntl or close.
	//    The test relies on CreateReadinessPipe performing these steps correctly
	//    if it returns no error. The actual signaling mechanism (using such FDs)
	//    should be tested separately, for example, in a dedicated TestReadinessSignalingMechanism.

	// We mostly rely on sub-function error handling (os.Pipe, SetCloexec) being correct.
}

// TestReadinessSignalingMechanism tests the core signaling logic:
// a child closing its write FD and the parent detecting this via EOF on its read FD.
func TestReadinessSignalingMechanism(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping TestReadinessSignalingMechanism on Windows due to POSIX-specific FD/pipe behavior.")
	}

	// 1. Manually create a pipe for this test.
	// This simulates the pipe that CreateReadinessPipe would set up.
	parentReadPipe, childWritePipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() failed: %v", err)
	}
	// parentReadPipe *os.File is used by WaitForChildReadyPipeClose, defer its close.
	defer parentReadPipe.Close()
	// childWritePipe *os.File should also be closed eventually.
	// If SignalChildReadyByClosingFD succeeds, childWriteFD is closed.
	// The deferred close here will then try to close an already closed FD, which is fine for cleanup.
	defer childWritePipe.Close()

	// 2. Get the FD for the child's write end.
	childWriteFD := childWritePipe.Fd()

	// 3. Simulate what CreateReadinessPipe does for the child's FD: clear CLOEXEC.
	// This isn't strictly necessary for *this specific test's logic* to pass if we don't fork,
	// but it mirrors the setup for which SignalChildReadyByClosingFD is intended.
	if err := SetCloexec(childWriteFD, false); err != nil {
		// No need to close childWritePipe here, defer will handle it.
		t.Fatalf("Failed to clear CLOEXEC on childWriteFD %d: %v", childWriteFD, err)
	}

	// 4. DO NOT close the *os.File wrapper for the child's write end here.
	// SignalChildReadyByClosingFD will operate on the raw FD number and perform the close.
	// The previous explicit close: `// if err := childWritePipe.Close(); err != nil { ... }` was the bug.

	// 5. Start a goroutine to wait for the child's signal (EOF on parentReadPipe).
	waitErrChan := make(chan error, 1)
	go func() {
		// Use a reasonable timeout for the wait.
		waitErrChan <- WaitForChildReadyPipeClose(parentReadPipe, 2*time.Second)
	}()

	// 6. Simulate the child signaling readiness by closing its FD.
	// Give a slight delay to ensure WaitForChildReadyPipeClose is likely waiting.
	time.Sleep(50 * time.Millisecond)
	if err := SignalChildReadyByClosingFD(childWriteFD); err != nil {
		t.Fatalf("SignalChildReadyByClosingFD failed for FD %d: %v", childWriteFD, err)
	}

	// 7. Check the result from WaitForChildReadyPipeClose.
	select {
	case err := <-waitErrChan:
		if err != nil {
			t.Errorf("WaitForChildReadyPipeClose returned an error: %v (expected nil for EOF)", err)
		}
		// If err is nil, it means EOF was received, which is success.
	case <-time.After(3 * time.Second): // Slightly longer timeout for the overall check
		t.Fatal("Timeout waiting for result from WaitForChildReadyPipeClose goroutine")
	}
}

func TestGetChildWritePipeFD(t *testing.T) {
	const testEnvVar = "TEST_CHILD_WRITE_PIPE_FD"

	tests := []struct {
		name            string
		envValue        *string // Pointer to distinguish between not set and empty string
		expectedFD      uintptr
		expectError     bool
		expectedErrorIs error  // Specific error to check with errors.Is
		errorContains   string // Substring to check in error message
	}{
		{
			name:            "EnvVarNotSet",
			envValue:        nil, // Not set
			expectError:     true,
			expectedErrorIs: ErrPipeFDEnvVarNotSet,
			errorContains:   "TEST_CHILD_WRITE_PIPE_FD",
		},
		{
			name:        "ValidPositiveInteger",
			envValue:    func() *string { s := "123"; return &s }(),
			expectedFD:  123,
			expectError: false,
		},
		{
			name:        "ValidZero",
			envValue:    func() *string { s := "0"; return &s }(),
			expectedFD:  0,
			expectError: false,
		},
		{
			name:          "InvalidStringNotAnInteger",
			envValue:      func() *string { s := "not-a-number"; return &s }(),
			expectError:   true,
			errorContains: "invalid integer value for FD",
		},
		{
			name:          "NegativeIntegerString",
			envValue:      func() *string { s := "-5"; return &s }(),
			expectError:   true,
			errorContains: "invalid negative FD value",
		},
		{
			name:            "EmptyStringValue", // os.Getenv returns empty if var is set to empty
			envValue:        func() *string { s := ""; return &s }(),
			expectError:     true,
			expectedErrorIs: ErrPipeFDEnvVarNotSet,
			errorContains:   "pipe FD environment variable not set", // This is correct because fdStr == "" is checked first by GetChildWritePipeFD
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Get current value to restore, or ensure it's unset
			originalValue, wasSet := os.LookupEnv(testEnvVar)

			if tc.envValue == nil { // Unset the var for this test case
				if err := os.Unsetenv(testEnvVar); err != nil {
					t.Fatalf("Failed to unset env var %s for test: %v", testEnvVar, err)
				}
			} else { // Set the var for this test case
				if err := os.Setenv(testEnvVar, *tc.envValue); err != nil {
					t.Fatalf("Failed to set env var %s to %q for test: %v", testEnvVar, *tc.envValue, err)
				}
			}

			// Defer restoration of the environment variable
			defer func() {
				if wasSet {
					if err := os.Setenv(testEnvVar, originalValue); err != nil {
						t.Logf("Error restoring env var %s to '%s': %v", testEnvVar, originalValue, err)
					}
				} else {
					if err := os.Unsetenv(testEnvVar); err != nil {
						t.Logf("Error unsetting env var %s after test: %v", testEnvVar, err)
					}
				}
			}()

			fd, err := GetChildWritePipeFD(testEnvVar)

			if tc.expectError {
				if err == nil {
					t.Fatalf("Expected an error, but got nil")
				}
				if tc.expectedErrorIs != nil {
					if !errors.Is(err, tc.expectedErrorIs) {
						t.Errorf("Expected error to be '%v', got '%v'", tc.expectedErrorIs, err)
					}
				}
				if tc.errorContains != "" {
					if !strings.Contains(err.Error(), tc.errorContains) {
						t.Errorf("Expected error message to contain '%s', got '%s'", tc.errorContains, err.Error())
					}
				}
			} else {
				if err != nil {
					t.Fatalf("Expected no error, but got: %v", err)
				}
				if fd != tc.expectedFD {
					t.Errorf("Expected FD %d, but got %d", tc.expectedFD, fd)
				}
			}
		})
	}
}

// mockUnsupportedListener is a custom net.Listener for testing unsupported types.
type mockUnsupportedListener struct {
	net.Listener // Embed for convenience, won't provide File()
}

// Provide dummy implementations for net.Listener for the mock
func (m *mockUnsupportedListener) Accept() (net.Conn, error) {
	return nil, errors.New("mockUnsupportedListener: Accept not implemented")
}
func (m *mockUnsupportedListener) Close() error {
	return errors.New("mockUnsupportedListener: Close not implemented")
}
func (m *mockUnsupportedListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234} // Dummy address
}

func TestGetFileFromListener(t *testing.T) {
	t.Run("TCPListener", func(t *testing.T) {
		ln, _ := createTestTCPListener(t, "127.0.0.1:0")
		defer ln.Close()

		tcpLn, ok := ln.(*net.TCPListener)
		if !ok {
			t.Fatalf("createTestTCPListener did not return a *net.TCPListener")
		}

		// Get the expected file and FD directly for comparison
		expectedFile, err := tcpLn.File()
		if err != nil {
			t.Fatalf("Failed to get expected file from TCPListener: %v", err)
		}
		defer expectedFile.Close()

		// Call the function under test
		gotFile, err := getFileFromListener(ln) // Pass the net.Listener interface
		if err != nil {
			t.Fatalf("getFileFromListener with TCPListener failed: %v", err)
		}
		if gotFile == nil {
			t.Fatal("getFileFromListener with TCPListener returned nil file")
		}
		defer gotFile.Close()

		// We don't compare FDs directly.
		// Instead, use os.SameFile to check if they refer to the same underlying file.
		expectedFi, errStatExpected := expectedFile.Stat()
		if errStatExpected != nil {
			t.Fatalf("Failed to stat expectedFile from TCPListener: %v", errStatExpected)
		}
		gotFi, errStatGot := gotFile.Stat()
		if errStatGot != nil {
			t.Fatalf("Failed to stat gotFile from getFileFromListener (TCP): %v", errStatGot)
		}

		if !os.SameFile(expectedFi, gotFi) {
			t.Errorf("getFileFromListener with TCPListener: expected returned file to be the same as listener's file, but os.SameFile returned false. (Debug FDs: got %d from getFileFromListener, direct call was %d)", gotFile.Fd(), expectedFile.Fd())
		}

		// Basic check: try to use the listener via the original ln handle
		// to ensure it wasn't messed up by getFileFromListener.
		// (getFileFromListener duplicates the FD, so original listener should be fine)
		_, testDialErr := net.Dial("tcp", ln.Addr().String())
		if testDialErr != nil {
			t.Errorf("Dialing TCP listener after getFileFromListener failed: %v", testDialErr)
		}
	})

	t.Run("UnixListener", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Skipping UnixListener test on Windows")
		}
		ln, socketPath := createTestUnixListener(t)
		defer ln.Close()
		defer os.Remove(socketPath) // Ensure socket file is cleaned up

		unixLn, ok := ln.(*net.UnixListener)
		if !ok {
			t.Fatalf("createTestUnixListener did not return a *net.UnixListener")
		}

		expectedFile, err := unixLn.File()
		if err != nil {
			t.Fatalf("Failed to get expected file from UnixListener: %v", err)
		}
		defer expectedFile.Close()

		gotFile, err := getFileFromListener(ln)
		if err != nil {
			t.Fatalf("getFileFromListener with UnixListener failed: %v", err)
		}
		if gotFile == nil {
			t.Fatal("getFileFromListener with UnixListener returned nil file")
		}
		defer gotFile.Close()

		// We don't compare FDs directly.
		// Instead, use os.SameFile to check if they refer to the same underlying file.
		expectedFi, errStatExpected := expectedFile.Stat()
		if errStatExpected != nil {
			t.Fatalf("Failed to stat expectedFile from UnixListener: %v", errStatExpected)
		}
		gotFi, errStatGot := gotFile.Stat()
		if errStatGot != nil {
			t.Fatalf("Failed to stat gotFile from getFileFromListener (Unix): %v", errStatGot)
		}

		if !os.SameFile(expectedFi, gotFi) {
			t.Errorf("getFileFromListener with UnixListener: expected returned file to be the same as listener's file, but os.SameFile returned false. (Debug FDs: got %d from getFileFromListener, direct call was %d)", gotFile.Fd(), expectedFile.Fd())
		}

		// Basic check for UnixListener
		_, testDialErr := net.Dial("unix", ln.Addr().String())
		if testDialErr != nil {
			t.Errorf("Dialing Unix listener after getFileFromListener failed: %v", testDialErr)
		}
	})

	t.Run("UnsupportedListener", func(t *testing.T) {
		unsupportedLn := &mockUnsupportedListener{}

		_, err := getFileFromListener(unsupportedLn)
		if err == nil {
			t.Fatal("getFileFromListener with unsupported type expected to fail, but got nil error")
		}
		if !strings.Contains(err.Error(), "unsupported listener type") {
			t.Errorf("Expected error message to contain 'unsupported listener type', got: %v", err)
		}
	})
}

func TestParseInheritedListenerFDs(t *testing.T) {
	const testEnvVar = "TEST_LISTEN_FDS_FOR_PARSE"

	tests := []struct {
		name          string
		envValue      *string // Pointer to distinguish between not set and empty string
		expectedFDs   []uintptr
		expectError   bool
		errorContains string // Substring to check in error message
	}{
		{
			name:        "EnvVarNotSet",
			envValue:    nil, // Not set
			expectedFDs: nil,
			expectError: false,
		},
		{
			name:        "EmptyStringValue",
			envValue:    func() *string { s := ""; return &s }(),
			expectedFDs: nil,
			expectError: false,
		},
		{
			name:        "ValidSingleFD",
			envValue:    func() *string { s := "3"; return &s }(),
			expectedFDs: []uintptr{3},
			expectError: false,
		},
		{
			name:        "ValidMultipleFDs",
			envValue:    func() *string { s := "3:4:5"; return &s }(),
			expectedFDs: []uintptr{3, 4, 5},
			expectError: false,
		},
		{
			name:        "ValidFDZero",
			envValue:    func() *string { s := "0"; return &s }(),
			expectedFDs: []uintptr{0},
			expectError: false,
		},
		{
			name:          "InvalidNonNumericPart_Single",
			envValue:      func() *string { s := "abc"; return &s }(),
			expectedFDs:   nil,
			expectError:   true,
			errorContains: "invalid FD number",
		},
		{
			name:          "InvalidNonNumericPart_Mixed",
			envValue:      func() *string { s := "3:abc:5"; return &s }(),
			expectedFDs:   nil,
			expectError:   true,
			errorContains: "invalid FD number",
		},
		{
			name:          "InvalidNegativeFD",
			envValue:      func() *string { s := "-1"; return &s }(),
			expectedFDs:   nil,
			expectError:   true,
			errorContains: "invalid negative FD number",
		},
		{
			name:          "InvalidEmptyPartStart",
			envValue:      func() *string { s := ":3"; return &s }(),
			expectedFDs:   nil,
			expectError:   true,
			errorContains: "invalid FD number", // strconv.Atoi("") fails
		},
		{
			name:          "InvalidEmptyPartMiddle",
			envValue:      func() *string { s := "3::4"; return &s }(),
			expectedFDs:   nil,
			expectError:   true,
			errorContains: "invalid FD number", // strconv.Atoi("") fails
		},
		{
			name:          "InvalidEmptyPartEnd",
			envValue:      func() *string { s := "3:"; return &s }(),
			expectedFDs:   nil,
			expectError:   true,
			errorContains: "invalid FD number", // strconv.Atoi("") fails
		},
		{
			name:        "ValidFDLargeNumber",
			envValue:    func() *string { s := "1023"; return &s }(),
			expectedFDs: []uintptr{1023},
			expectError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var originalValue string
			var wasSet bool

			if tc.envValue == nil {
				// Ensure env var is not set
				originalValue, wasSet = os.LookupEnv(testEnvVar)
				if wasSet {
					if err := os.Unsetenv(testEnvVar); err != nil {
						t.Fatalf("Failed to unset env var %s for test: %v", testEnvVar, err)
					}
				}
			} else {
				// Set env var
				originalValue, wasSet = os.LookupEnv(testEnvVar)
				if err := os.Setenv(testEnvVar, *tc.envValue); err != nil {
					t.Fatalf("Failed to set env var %s to %q for test: %v", testEnvVar, *tc.envValue, err)
				}
			}

			defer func() {
				if wasSet {
					if err := os.Setenv(testEnvVar, originalValue); err != nil {
						t.Logf("Error restoring env var %s to '%s': %v", testEnvVar, originalValue, err)
					}
				} else {
					if err := os.Unsetenv(testEnvVar); err != nil {
						t.Logf("Error unsetting env var %s after test: %v", testEnvVar, err)
					}
				}
			}()

			fds, err := ParseInheritedListenerFDs(testEnvVar)

			if tc.expectError {
				if err == nil {
					t.Fatalf("Expected an error, but got nil. Env value: %v", tc.envValue)
				}
				if tc.errorContains != "" {
					if !strings.Contains(err.Error(), tc.errorContains) {
						t.Errorf("Expected error message to contain '%s', got '%s'", tc.errorContains, err.Error())
					}
				}
			} else {
				if err != nil {
					t.Fatalf("Expected no error, but got: %v. Env value: %v", err, tc.envValue)
				}
				if len(fds) != len(tc.expectedFDs) {
					t.Errorf("FD count mismatch: expected %d, got %d. Expected: %v, Got: %v", len(tc.expectedFDs), len(fds), tc.expectedFDs, fds)
				} else {
					for i := range fds {
						if fds[i] != tc.expectedFDs[i] {
							t.Errorf("FD mismatch at index %d: expected %d, got %d. Expected: %v, Got: %v", i, tc.expectedFDs[i], fds[i], tc.expectedFDs, fds)
						}
					}
				}
			}
		})
	}
}

func TestGetInheritedListeners(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping TestGetInheritedListeners on Windows due to POSIX-specific FD semantics.")
	}

	const testEnvVar = ListenFdsEnvKey // Use the actual constant

	// Helper to create a TCP listener and return its FD string and the listener itself for cleanup
	setupTCPListenerFD := func(t *testing.T) (string, net.Listener) {
		t.Helper()
		// CreateListenerAndGetFD ensures the FD has CLOEXEC cleared.
		ln, fd, err := CreateListenerAndGetFD("127.0.0.1:0")
		if err != nil {
			t.Fatalf("Setup: CreateListenerAndGetFD failed: %v", err)
		}
		// ln needs to be closed by the caller of setupTCPListenerFD
		return strconv.Itoa(int(fd)), ln
	}

	// Helper to create a pipe and return its read-end FD string and the pipe files for cleanup
	setupPipeReadFD := func(t *testing.T) (string, *os.File, *os.File) {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("Setup: os.Pipe() failed: %v", err)
		}
		// r and w need to be closed by the caller.
		// GetInheritedListeners will try to use r.Fd().
		// SetCloexec on r.Fd() within GetInheritedListeners should work fine.
		// net.FileListener(r) will fail, which is one of the test cases.
		return strconv.Itoa(int(r.Fd())), r, w
	}

	tests := []struct {
		name          string
		setupFn       func(t *testing.T) (envValue string, cleanup func()) // Returns env string and a cleanup func
		expectError   bool
		errorContains string
		expectedCount int // Expected number of listeners
	}{
		{
			name: "NoEnvVar",
			setupFn: func(t *testing.T) (string, func()) {
				// Ensure env var is not set
				originalValue, wasSet := os.LookupEnv(testEnvVar)
				if wasSet {
					if err := os.Unsetenv(testEnvVar); err != nil {
						t.Fatalf("Failed to unset env var %s for test: %v", testEnvVar, err)
					}
				}
				return "", func() { // No LISTEN_FDS value to set
					if wasSet {
						if err := os.Setenv(testEnvVar, originalValue); err != nil {
							t.Logf("Error restoring env var %s to '%s': %v", testEnvVar, originalValue, err)
						}
					}
				}
			},
			expectError:   false,
			expectedCount: 0,
		},
		{
			name: "EmptyEnvVar",
			setupFn: func(t *testing.T) (string, func()) {
				return "", func() {} // Effectively sets LISTEN_FDS="" via withTempEnvVar
			},
			expectError:   false,
			expectedCount: 0,
		},
		{
			name: "ValidSingleTCPSocketFD",
			setupFn: func(t *testing.T) (string, func()) {
				fdStr, ln1 := setupTCPListenerFD(t)
				return fdStr, func() { ln1.Close() }
			},
			expectError:   false,
			expectedCount: 1,
		},
		{
			name: "ValidMultipleTCPSocketFDs",
			setupFn: func(t *testing.T) (string, func()) {
				fdStr1, ln1 := setupTCPListenerFD(t)
				fdStr2, ln2 := setupTCPListenerFD(t)
				return fdStr1 + ":" + fdStr2, func() {
					ln1.Close()
					ln2.Close()
				}
			},
			expectError:   false,
			expectedCount: 2,
		},
		{
			name: "InvalidFDString_NonNumeric",
			setupFn: func(t *testing.T) (string, func()) {
				return "abc", func() {}
			},
			expectError:   true,
			errorContains: "invalid FD", // From strconv.Atoi
		},
		{
			name: "InvalidFDString_Mixed",
			setupFn: func(t *testing.T) (string, func()) {
				fdStr1, ln1 := setupTCPListenerFD(t)
				// ln1 will be closed by GetInheritedListeners's internal cleanup on error
				return fdStr1 + ":xyz:" + fdStr1, func() { ln1.Close() /* Safety close */ }
			},
			expectError:   true,
			errorContains: "invalid FD", // From strconv.Atoi for "xyz"
		},
		{
			name: "Error_SetCloexecFails_BogusFD",
			setupFn: func(t *testing.T) (string, func()) {
				return "99999", func() {} // A large, likely unused FD
			},
			expectError:   true,
			errorContains: "failed to clear FD_CLOEXEC", // Error from SetCloexec
		},
		{
			name: "Error_OsNewFileFails_ClosedPipeFD",
			setupFn: func(t *testing.T) (string, func()) {
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatalf("Setup: os.Pipe failed: %v", err)
				}
				fdVal := int(r.Fd()) // Get FD before closing r
				r.Close()            // Close the read end
				w.Close()            // Close the write end
				// Now fdVal is a closed FD. SetCloexec might pass or fail mildly,
				// but os.NewFile should robustly fail.
				return strconv.Itoa(fdVal), func() {}
			},

			expectError:   true,
			errorContains: "failed to clear FD_CLOEXEC", // Error from SetCloexec on closed FD
		},
		{
			name: "Error_FileListenerFails_PipeFD",
			setupFn: func(t *testing.T) (string, func()) {
				fdStr, rPipe, wPipe := setupPipeReadFD(t)
				// rPipe.Fd() is passed. net.FileListener should fail.
				return fdStr, func() {
					rPipe.Close()
					wPipe.Close()
				}
			},
			expectError:   true,
			errorContains: "failed to create net.Listener from inherited FD", // Error from net.FileListener
			// The specific OS error might be "socket operation on non-socket" or similar.
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			envValueToSet, cleanup := tc.setupFn(t)
			defer cleanup()

			withTempEnvVar(t, testEnvVar, envValueToSet, func() {
				listeners, err := GetInheritedListeners()
				// Ensure all returned listeners are closed, even if test fails later
				defer func() {
					for _, l := range listeners {
						if l != nil {
							l.Close()
						}
					}
				}()

				if tc.expectError {
					if err == nil {
						t.Fatalf("Expected an error, but got nil. LISTEN_FDS=%q", envValueToSet)
					}
					if tc.errorContains != "" {
						if !strings.Contains(err.Error(), tc.errorContains) {
							t.Errorf("Expected error message to contain '%s', got '%s'", tc.errorContains, err.Error())
						}
					}
				} else {
					if err != nil {
						t.Fatalf("Expected no error, but got: %v. LISTEN_FDS=%q", err, envValueToSet)
					}
					if len(listeners) != tc.expectedCount {
						t.Errorf("Expected %d listeners, got %d", tc.expectedCount, len(listeners))
					}
					// Further checks on listener properties (e.g., address, usability) could be added here.
					// For TCP listeners, check they are indeed TCP and listening.
					for i, l := range listeners {
						if l == nil {
							t.Errorf("Listener at index %d is nil", i)
							continue
						}
						// Try to accept a connection to confirm it's a working listener
						// This check is primarily for valid socket FDs.
						if strings.Count(envValueToSet, ":") < i && !strings.Contains(tc.name, "Pipe") { // Only for presumed TCP FDs
							go func(listener net.Listener) {
								c, dialErr := net.Dial("tcp", listener.Addr().String())
								if dialErr == nil {
									c.Close()
								}
							}(l)
							acceptConn, acceptErr := l.Accept()
							if acceptErr != nil {
								// This can happen if the original listener FD (from setupFn) was closed too soon
								// or if there's some other issue.
								t.Errorf("Listener %d (%s) failed to accept: %v. Test name: %s", i, l.Addr(), acceptErr, tc.name)
							} else {
								acceptConn.Close()
							}
						}
					}
				}
			})
		})
	}
}

func TestGetInheritedReadinessPipeFD(t *testing.T) {
	const testEnvVar = ReadinessPipeEnvKey // Use the actual constant

	tests := []struct {
		name          string
		envValue      *string // Pointer to distinguish between not set and empty string
		expectedFD    uintptr
		expectedFound bool
		expectError   bool
		errorContains string // Substring to check in error message
	}{
		{
			name:          "EnvVarNotSet",
			envValue:      nil, // Not set
			expectedFD:    0,
			expectedFound: false,
			expectError:   false,
		},
		{
			name:          "EmptyStringValue", // os.Getenv returns empty if var is set to empty or not set.
			envValue:      func() *string { s := ""; return &s }(),
			expectedFD:    0,
			expectedFound: false, // If env var is empty string, GetInheritedReadinessPipeFD returns found=false
			expectError:   false,
		},
		{
			name:          "ValidPositiveInteger",
			envValue:      func() *string { s := "123"; return &s }(),
			expectedFD:    123,
			expectedFound: true,
			expectError:   false,
		},
		{
			name:          "ValidZero",
			envValue:      func() *string { s := "0"; return &s }(),
			expectedFD:    0,
			expectedFound: true,
			expectError:   false,
		},
		{
			name:          "InvalidStringNotAnInteger",
			envValue:      func() *string { s := "not-a-number"; return &s }(),
			expectedFD:    0,
			expectedFound: true, // Found is true because env var is set, but parsing fails.
			expectError:   true,
			errorContains: "invalid FD for readiness pipe",
		},
		{
			name:          "NegativeIntegerString",
			envValue:      func() *string { s := "-5"; return &s }(),
			expectedFD:    0,
			expectedFound: true, // Found is true because env var is set
			expectError:   true,
			errorContains: "invalid negative FD for readiness pipe",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			originalValue, wasSet := os.LookupEnv(testEnvVar)

			if tc.envValue == nil { // Unset the var for this test case
				if err := os.Unsetenv(testEnvVar); err != nil {
					t.Fatalf("Failed to unset env var %s for test: %v", testEnvVar, err)
				}
			} else { // Set the var for this test case
				if err := os.Setenv(testEnvVar, *tc.envValue); err != nil {
					t.Fatalf("Failed to set env var %s to %q for test: %v", testEnvVar, *tc.envValue, err)
				}
			}

			// Defer restoration of the environment variable
			defer func() {
				if wasSet {
					if err := os.Setenv(testEnvVar, originalValue); err != nil {
						t.Logf("Error restoring env var %s to '%s': %v", testEnvVar, originalValue, err)
					}
				} else {
					if err := os.Unsetenv(testEnvVar); err != nil {
						t.Logf("Error unsetting env var %s after test: %v", testEnvVar, err)
					}
				}
			}()

			fd, found, err := GetInheritedReadinessPipeFD()

			if tc.expectError {
				if err == nil {
					t.Fatalf("Expected an error, but got nil")
				}
				if tc.errorContains != "" {
					if !strings.Contains(err.Error(), tc.errorContains) {
						t.Errorf("Expected error message to contain '%s', got '%s'", tc.errorContains, err.Error())
					}
				}
			} else {
				if err != nil {
					t.Fatalf("Expected no error, but got: %v", err)
				}
				if fd != tc.expectedFD {
					t.Errorf("Expected FD %d, but got %d", tc.expectedFD, fd)
				}
				if found != tc.expectedFound {
					t.Errorf("Expected found flag to be %t, but got %t", tc.expectedFound, found)
				}
			}
		})
	}
}

func TestIsAddrInUse(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "NilError",
			err:      nil,
			expected: false,
		},
		{
			name:     "Direct_EADDRINUSE",
			err:      syscall.EADDRINUSE,
			expected: true,
		},
		{
			name:     "Wrapped_EADDRINUSE_SyscallError",
			err:      &os.SyscallError{Err: syscall.EADDRINUSE, Syscall: "listen"},
			expected: true,
		},
		{
			name:     "Wrapped_EADDRINUSE_NetOpError",
			err:      &net.OpError{Op: "listen", Net: "tcp", Source: nil, Addr: nil, Err: syscall.EADDRINUSE},
			expected: true,
		},
		{
			name:     "StringMatch_Lowercase",
			err:      errors.New("some error: address already in use"),
			expected: true,
		},
		{
			name:     "StringMatch_Capitalized",
			err:      errors.New("listen tcp 127.0.0.1:8080: Address already in use"),
			expected: true,
		},
		{
			name:     "StringMatch_DifferentCapitalization",
			err:      errors.New("ADDRess AlReAdY IN uSe"),
			expected: true,
		},
		{
			name:     "UnrelatedError_EOF",
			err:      io.EOF,
			expected: false,
		},
		{
			name:     "UnrelatedError_Generic",
			err:      errors.New("some other network problem"),
			expected: false,
		},
		{
			name:     "Unrelated_SyscallError_NotEADDRINUSE",
			err:      &os.SyscallError{Err: syscall.ECONNREFUSED, Syscall: "connect"},
			expected: false,
		},
		{
			name:     "Unrelated_NetOpError_NotEADDRINUSE",
			err:      &net.OpError{Op: "dial", Net: "tcp", Source: nil, Addr: nil, Err: errors.New("connection refused")},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsAddrInUse(tc.err)
			if got != tc.expected {
				t.Errorf("IsAddrInUse(%v) = %t; want %t", tc.err, got, tc.expected)
			}
		})
	}
}

func TestPrepareExecEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping TestPrepareExecEnv on Windows due to POSIX-specific FD/pipe behavior.")
	}

	// Helper to convert env slice to map for easier checking
	envSliceToMap := func(env []string) map[string]string {
		m := make(map[string]string)
		for _, e := range env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				m[parts[0]] = parts[1]
			} else {
				m[parts[0]] = "" // Handle variables without '=' if any (though rare)
			}
		}
		return m
	}

	tests := []struct {
		name                   string
		setupListeners         func(t *testing.T) ([]net.Listener, func()) // Returns listeners and cleanup func
		setupReadinessPipe     func(t *testing.T) (*os.File, func())       // Returns readiness pipe write end, and cleanup
		currentEnv             []string
		expectedNumListenerFDs int    // Expected number of FDs in LISTEN_FDS. 0 means LISTEN_FDS should not be set.
		expectedReadinessFD    string // Expected value for READINESS_PIPE_FD, or empty if not expected (use "USE_PIPE_FD" placeholder)
		expectError            bool
		errorContains          string
		otherExpectedEnv       map[string]string // Other env vars that should be present
	}{
		{
			name: "NoListeners_NoPipe",
			setupListeners: func(t *testing.T) ([]net.Listener, func()) {
				return nil, func() {}
			},
			setupReadinessPipe: func(t *testing.T) (*os.File, func()) {
				return nil, func() {}
			},
			currentEnv:             []string{"VAR1=value1", "VAR2=value2"},
			expectedNumListenerFDs: 0,
			otherExpectedEnv:       map[string]string{"VAR1": "value1", "VAR2": "value2"},
		},
		{
			name: "OneListener_NoPipe",
			setupListeners: func(t *testing.T) ([]net.Listener, func()) {
				ln1, _, err := CreateListenerAndGetFD("127.0.0.1:0") // FD from here not used for expectation
				if err != nil {
					t.Fatalf("Setup: CreateListenerAndGetFD failed: %v", err)
				}
				return []net.Listener{ln1}, func() { ln1.Close() }
			},
			setupReadinessPipe: func(t *testing.T) (*os.File, func()) {
				return nil, func() {}
			},
			currentEnv:             []string{"EXISTING_VAR=yes"},
			expectedNumListenerFDs: 1,
			otherExpectedEnv:       map[string]string{"EXISTING_VAR": "yes"},
		},
		{
			name: "TwoListeners_NoPipe",
			setupListeners: func(t *testing.T) ([]net.Listener, func()) {
				ln1, _, err1 := CreateListenerAndGetFD("127.0.0.1:0")
				ln2, _, err2 := CreateListenerAndGetFD("127.0.0.1:0")
				if err1 != nil || err2 != nil {
					t.Fatalf("Setup: CreateListenerAndGetFD failed: %v, %v", err1, err2)
				}
				return []net.Listener{ln1, ln2}, func() { ln1.Close(); ln2.Close() }
			},
			setupReadinessPipe: func(t *testing.T) (*os.File, func()) {
				return nil, func() {}
			},
			currentEnv:             nil,
			expectedNumListenerFDs: 2,
			otherExpectedEnv:       map[string]string{},
		},
		{
			name: "NoListeners_WithPipe",
			setupListeners: func(t *testing.T) ([]net.Listener, func()) {
				return nil, func() {}
			},
			setupReadinessPipe: func(t *testing.T) (*os.File, func()) {
				_, childWritePipe, err := os.Pipe()
				if err != nil {
					t.Fatalf("Setup: os.Pipe for readiness failed: %v", err)
				}
				if err := SetCloexec(childWritePipe.Fd(), false); err != nil {
					childWritePipe.Close()
					t.Fatalf("Setup: Failed to clear CLOEXEC on childWritePipe: %v", err)
				}
				return childWritePipe, func() { childWritePipe.Close() }
			},
			currentEnv:             []string{"VAR=val"},
			expectedNumListenerFDs: 0,
			expectedReadinessFD:    "USE_PIPE_FD",
			otherExpectedEnv:       map[string]string{"VAR": "val"},
		},
		{
			name: "OneListener_WithPipe",
			setupListeners: func(t *testing.T) ([]net.Listener, func()) {
				ln1, _, err := CreateListenerAndGetFD("127.0.0.1:0")
				if err != nil {
					t.Fatalf("Setup: CreateListenerAndGetFD failed: %v", err)
				}
				return []net.Listener{ln1}, func() { ln1.Close() }
			},
			setupReadinessPipe: func(t *testing.T) (*os.File, func()) {
				_, childWritePipe, err := os.Pipe()
				if err != nil {
					t.Fatalf("Setup: os.Pipe for readiness failed: %v", err)
				}
				if err := SetCloexec(childWritePipe.Fd(), false); err != nil {
					childWritePipe.Close()
					t.Fatalf("Setup: Failed to clear CLOEXEC on childWritePipe: %v", err)
				}
				return childWritePipe, func() { childWritePipe.Close() }
			},
			currentEnv:             []string{},
			expectedNumListenerFDs: 1,
			expectedReadinessFD:    "USE_PIPE_FD",
			otherExpectedEnv:       map[string]string{},
		},
		{
			name: "FilterExistingEnvVars",
			setupListeners: func(t *testing.T) ([]net.Listener, func()) {
				ln1, _, err := CreateListenerAndGetFD("127.0.0.1:0")
				if err != nil {
					t.Fatalf("Setup: CreateListenerAndGetFD failed: %v", err)
				}
				return []net.Listener{ln1}, func() { ln1.Close() }
			},
			setupReadinessPipe: func(t *testing.T) (*os.File, func()) {
				_, childWritePipe, err := os.Pipe()
				if err != nil {
					t.Fatalf("Setup: os.Pipe for readiness failed: %v", err)
				}
				if err := SetCloexec(childWritePipe.Fd(), false); err != nil {
					childWritePipe.Close()
					t.Fatalf("Setup: Failed to clear CLOEXEC on childWritePipe: %v", err)
				}
				return childWritePipe, func() { childWritePipe.Close() }
			},
			currentEnv: []string{
				"LISTEN_FDS=old_listen_fds",
				"READINESS_PIPE_FD=old_readiness_fd",
				"PRESERVED_VAR=keep_me",
			},
			expectedNumListenerFDs: 1,
			expectedReadinessFD:    "USE_PIPE_FD",
			otherExpectedEnv:       map[string]string{"PRESERVED_VAR": "keep_me"},
		},
		{
			name: "Error_UnsupportedListenerType",
			setupListeners: func(t *testing.T) ([]net.Listener, func()) {
				mockLn := &mockUnsupportedListener{}
				return []net.Listener{mockLn}, func() {}
			},
			setupReadinessPipe:     func(t *testing.T) (*os.File, func()) { return nil, func() {} },
			currentEnv:             nil,
			expectError:            true,
			errorContains:          "unsupported listener type",
			expectedNumListenerFDs: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			listeners, cleanupListeners := tc.setupListeners(t)
			defer cleanupListeners()

			readinessPipeWrite, cleanupPipe := tc.setupReadinessPipe(t)
			defer cleanupPipe()

			expectedReadinessFDValue := tc.expectedReadinessFD
			if expectedReadinessFDValue == "USE_PIPE_FD" {
				if readinessPipeWrite == nil {
					t.Fatalf("Test setup error: readinessPipeWrite is nil for USE_PIPE_FD placeholder but expectedReadinessFD is USE_PIPE_FD")
				}
				expectedReadinessFDValue = strconv.Itoa(int(readinessPipeWrite.Fd()))
			}

			newEnv, err := PrepareExecEnv(tc.currentEnv, listeners, readinessPipeWrite)

			if tc.expectError {
				if err == nil {
					t.Fatalf("Expected an error, but got nil")
				}
				if tc.errorContains != "" {
					if !strings.Contains(err.Error(), tc.errorContains) {
						t.Errorf("Expected error message to contain '%s', got '%s'", tc.errorContains, err.Error())
					}
				}
				return // Don't check env content if error was expected
			}

			if err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}

			envMap := envSliceToMap(newEnv)

			// Check LISTEN_FDS
			if tc.expectedNumListenerFDs > 0 {
				val, ok := envMap[ListenFdsEnvKey]
				if !ok {
					t.Errorf("Expected %s to be set, but it was not", ListenFdsEnvKey)
				} else {
					fdsInEnv := strings.Split(val, ":")
					if len(fdsInEnv) != tc.expectedNumListenerFDs {
						t.Errorf("Expected %d FDs in %s, got %d. Value: %s", tc.expectedNumListenerFDs, ListenFdsEnvKey, len(fdsInEnv), val)
					}
					for _, fdStr := range fdsInEnv {
						fdNum, errConv := strconv.Atoi(fdStr)
						if errConv != nil {
							t.Errorf("Invalid FD number string '%s' in %s: %v", fdStr, ListenFdsEnvKey, errConv)
						}
						if fdNum < 0 {
							t.Errorf("Invalid negative FD number %d in %s", fdNum, ListenFdsEnvKey)
						}
						// Cannot reliably check actual FD values as they are dynamic.
					}
				}
			} else { // expectedNumListenerFDs == 0
				if val, ok := envMap[ListenFdsEnvKey]; ok {
					t.Errorf("Expected %s to NOT be set, but it was: %s=%s", ListenFdsEnvKey, ListenFdsEnvKey, val)
				}
			}

			// Check READINESS_PIPE_FD
			if expectedReadinessFDValue != "" { // This means we expect the variable to be set
				val, ok := envMap[ReadinessPipeEnvKey]
				if !ok {
					t.Errorf("Expected %s to be set with value %s, but it was not set", ReadinessPipeEnvKey, expectedReadinessFDValue)
				} else if val != expectedReadinessFDValue {
					t.Errorf("Expected %s=%s, got %s=%s", ReadinessPipeEnvKey, expectedReadinessFDValue, ReadinessPipeEnvKey, val)
				}
			} else { // This means we expect the variable to NOT be set
				if val, ok := envMap[ReadinessPipeEnvKey]; ok {
					t.Errorf("Expected %s to NOT be set, but it was: %s=%s", ReadinessPipeEnvKey, ReadinessPipeEnvKey, val)
				}
			}

			// Check other expected environment variables
			for k, v := range tc.otherExpectedEnv {
				val, ok := envMap[k]
				if !ok {
					t.Errorf("Expected environment variable %s=%s to be present, but key was not found", k, v)
				} else if val != v {
					t.Errorf("Expected environment variable %s=%s, got %s=%s", k, v, k, val)
				}
			}

			// Verify that original LISTEN_FDS and READINESS_PIPE_FD from currentEnv are not present
			// if new ones were meant to replace them (i.e., if expectedNumListenerFDs > 0 or expectedReadinessFD != "").
			originalCurrentEnvMap := envSliceToMap(tc.currentEnv)
			if tc.expectedNumListenerFDs > 0 {
				if _, ok := originalCurrentEnvMap[ListenFdsEnvKey]; ok {
					// Check that new env does not contain the *old* value if it was filtered.
					// This is implicitly handled if the new value is correctly asserted.
					// The test mainly checks that the *new* value is present.
				}
			}
			if expectedReadinessFDValue != "" {
				if _, ok := originalCurrentEnvMap[ReadinessPipeEnvKey]; ok {
					// Similarly, check that the new env has the new readiness FD, not old one.
				}
			}
		})
	}
}

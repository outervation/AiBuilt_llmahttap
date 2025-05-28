package util

import (
	"errors"
	// "io/fs" // No longer needed here as test was moved
	"net"
	"os"
	"path/filepath"
	"runtime"
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

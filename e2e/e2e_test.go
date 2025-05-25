package e2e

import (
	"bytes"
	"fmt"
	// "net/http" // Removed as unused for now
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	// "example.com/llmahttap/v2/e2e/testutil"    // Removed as unused for now
	// "example.com/llmahttap/v2/internal/config" // Removed as unused for now
)

var serverBinaryPath string

func TestMain(m *testing.M) {
	// Determine the output path for the compiled binary.
	// Placing it in the e2e directory for simplicity.
	tempDir, err := os.MkdirTemp("", "llmahttap_e2e_bin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create temp directory for server binary: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tempDir)

	outputName := filepath.Join(tempDir, "llmahttap_test_server_binary")
	if strings.HasSuffix(os.Args[0], ".test") { // Check if running in "go test -c" context or similar
		// On Windows, Go test binaries have .exe suffix.
		// For cross-platform compatibility, especially when building.
		if goos := os.Getenv("GOOS"); goos == "windows" {
			outputName += ".exe"
		}
	}

	// Compile the server binary.
	// The source package is example.com/llmahttap/v2/cmd/server
	cmd := exec.Command("go", "build", "-o", outputName, "example.com/llmahttap/v2/cmd/server")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to compile server binary for E2E tests:\n%s\nError: %v\n", stderr.String(), err)
		os.Exit(1)
	}

	absPath, err := filepath.Abs(outputName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get absolute path for compiled server binary %s: %v\n", outputName, err)
		os.Remove(outputName) // Attempt cleanup
		os.Exit(1)
	}
	serverBinaryPath = absPath

	// Run the tests
	exitCode := m.Run()

	// Cleanup: remove the compiled binary
	// No need to explicitly remove 'outputName' as its parent 'tempDir' will be removed by defer.

	os.Exit(exitCode)
}

// Placeholder to ensure the file compiles and TestMain is recognized.
// Actual tests will be added in subsequent steps.
func TestPlaceholder(t *testing.T) {
	if serverBinaryPath == "" {
		t.Fatal("serverBinaryPath was not set by TestMain")
	}
	_, err := os.Stat(serverBinaryPath)
	if err != nil {
		t.Fatalf("Compiled server binary not found at %s: %v", serverBinaryPath, err)
	}
	// This is just a basic check. More tests will use testutil.RunE2ETest.
	t.Logf("TestMain successfully compiled server binary to: %s", serverBinaryPath)
}

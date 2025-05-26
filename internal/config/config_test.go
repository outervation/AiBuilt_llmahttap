package config

import (
	"io/ioutil"
	"os"
	"strings"
	"testing"
)

// writeTempFile creates a temporary file with the given content and extension.
// It returns the path to the file and a cleanup function to remove the file.
func writeTempFile(t *testing.T, content string, ext string) (path string, cleanup func()) {
	t.Helper()
	tmpFile, err := ioutil.TempFile("", "test-config-*"+ext)
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}

	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		t.Fatalf("Failed to write to temp file: %v", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		t.Fatalf("Failed to close temp file: %v", err)
	}

	return tmpFile.Name(), func() {
		os.Remove(tmpFile.Name())
	}
}

// checkErrorContains checks if the error is not nil and its message contains the expected substring.
func checkErrorContains(t *testing.T, err error, expectedSubstring string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Expected an error containing %q, but got nil", expectedSubstring)
	}
	if !strings.Contains(err.Error(), expectedSubstring) {
		t.Fatalf("Expected error message to contain %q, but got: %v", expectedSubstring, err)
	}
}

package testutil

import (
	"bytes"
	"io"
	"net/http"
	"os/exec"
	"sync" // Added for ServerInstance.mu

	"example.com/llmahttap/v2/internal/config"
)

// TestRequest models an HTTP request for E2E testing.
type TestRequest struct {
	Method  string
	Path    string // Should include query string if any, e.g., "/path?query=value"
	Headers http.Header
	Body    []byte
}

// HeaderMatcher defines a way to match headers.
// For simplicity, could be exact match, regex, or presence.
// This is a placeholder for now; a more complex type might be needed.
type HeaderMatcher map[string]string // Key: header name, Value: expected value (exact match for now)

// BodyMatcher defines a way to match the response body.
// Could be exact match, regex, JSON path + value, etc.
type BodyMatcher interface {
	Match(body []byte) (bool, string) // Returns match status and a description of mismatch
}

// ExactBodyMatcher matches the body exactly.
type ExactBodyMatcher struct {
	ExpectedBody []byte
}

// Match implements BodyMatcher for ExactBodyMatcher.
func (m *ExactBodyMatcher) Match(body []byte) (bool, string) {
	if bytes.Equal(m.ExpectedBody, body) {
		return true, ""
	}
	return false, "bodies do not match exactly"
}

// StringContainsBodyMatcher checks if the body contains a specific substring.
type StringContainsBodyMatcher struct {
	Substring string
}

// Match implements BodyMatcher for StringContainsBodyMatcher.
func (m *StringContainsBodyMatcher) Match(body []byte) (bool, string) {
	if bytes.Contains(body, []byte(m.Substring)) {
		return true, ""
	}
	return false, "body does not contain substring: " + m.Substring
}

// ExpectedResponse models the expected outcome of an HTTP request.
type ExpectedResponse struct {
	StatusCode    int
	Headers       HeaderMatcher // Optional: map of headers to expected values or more complex matchers
	BodyMatcher   BodyMatcher   // Optional: for matching the response body
	ExpectNoBody  bool          // If true, BodyMatcher is ignored and body must be empty
	ErrorContains string        // If non-empty, an error is expected during the request and its message should contain this string
}

// ActualResponse stores the actual outcome of an HTTP request from a client.
type ActualResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Error      error // Any error that occurred during the request execution
}

// ServerInstance encapsulates details of a running test server.
type ServerInstance struct {
	Cmd           *exec.Cmd      // The command running the server process
	Config        *config.Config // The configuration used by the server
	Address       string         // The network address the server is listening on (e.g., "127.0.0.1:8080")
	ConfigPath    string         // Path to the temporary config file used by this instance
	LogOutput     io.ReadCloser  // Pipe to read server's stdout/stderr for logs
	cancelLogCopy func()         // Function to stop log copying goroutine
	mu            sync.Mutex     // To protect access to Cmd and other shared resources if needed
	shutdownFunc  func() error   // Function to gracefully shut down the server
	CleanupFuncs  []func() error // Functions to run to clean up (e.g., remove temp files)
}

// HTTPClientType identifies the type of HTTP client used for a test.
type HTTPClientType string

const (
	GoHTTPClient HTTPClientType = "go_http_client"
	CurlClient   HTTPClientType = "curl_client"
)

// TestRunner is an interface for an entity that can run a TestRequest
// against a ServerInstance and return an ActualResponse.
type TestRunner interface {
	Run(server *ServerInstance, req *TestRequest) (*ActualResponse, error)
	Type() HTTPClientType
}

package testutil

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

// HTTPTestClient defines an interface for making HTTP requests for testing purposes.
// This allows test cases to be written independently of the specific HTTP client
// implementation (e.g., Go standard library HTTP client, or an external tool like curl).
type HTTPTestClient interface {
	// Do executes an HTTP request against the server at serverAddr.
	// It takes the server's address (e.g., "host:port") and a TestRequest object.
	// It returns an ActualResponse object containing the server's response details
	// and any error encountered during the request execution.
	Do(serverAddr string, request TestRequest) (ActualResponse, error)
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

// CurlHTTPClient implements HTTPTestClient using the curl command-line tool.
type CurlHTTPClient struct {
	// CurlPath is the path to the curl executable. If empty, "curl" is used.
	CurlPath string
}

// NewCurlHTTPClient creates a new CurlHTTPClient.
func NewCurlHTTPClient(curlPath string) *CurlHTTPClient {
	cp := curlPath
	if cp == "" {
		cp = "curl" // Default to "curl" if path is not specified
	}
	return &CurlHTTPClient{CurlPath: cp}
}

// Type returns the client type. Useful for test runners or logging.
// Note: This method is not part of the HTTPTestClient interface as currently defined.
func (c *CurlHTTPClient) Type() HTTPClientType {
	return CurlClient
}

// Do executes an HTTP request using curl.
func (c *CurlHTTPClient) Do(serverAddr string, request TestRequest) (ActualResponse, error) {
	actualRes := ActualResponse{
		Headers: make(http.Header),
	}

	// Create temporary files for response headers and body
	respHeaderFile, err := os.CreateTemp("", "curl_resp_headers_*.txt")
	if err != nil {
		actualRes.Error = fmt.Errorf("failed to create temp file for response headers: %w", err)
		return actualRes, actualRes.Error
	}
	defer os.Remove(respHeaderFile.Name())
	// We need the path, so close it now. Curl will open and write to it.
	if err := respHeaderFile.Close(); err != nil {
		actualRes.Error = fmt.Errorf("failed to close temp file for response headers: %w", err)
		return actualRes, actualRes.Error
	}

	respBodyFile, err := os.CreateTemp("", "curl_resp_body_*.bin")
	if err != nil {
		actualRes.Error = fmt.Errorf("failed to create temp file for response body: %w", err)
		return actualRes, actualRes.Error
	}
	defer os.Remove(respBodyFile.Name())
	if err := respBodyFile.Close(); err != nil {
		actualRes.Error = fmt.Errorf("failed to close temp file for response body: %w", err)
		return actualRes, actualRes.Error
	}

	// URL construction
	scheme := "http://"
	if strings.HasPrefix(serverAddr, "http://") || strings.HasPrefix(serverAddr, "https://") {
		scheme = "" // serverAddr already has scheme
	}
	if !strings.HasPrefix(request.Path, "/") && request.Path != "" {
		request.Path = "/" + request.Path
	}
	url := fmt.Sprintf("%s%s%s", scheme, serverAddr, request.Path)

	// Method
	if request.Method == "" {
		request.Method = "GET" // Default to GET
	}

	// Construct curl arguments
	var finalArgs []string
	finalArgs = append(finalArgs, "--http2-prior-knowledge")
	finalArgs = append(finalArgs, "--silent")
	finalArgs = append(finalArgs, "--show-error") // Show curl specific errors on stderr
	finalArgs = append(finalArgs, "-X", request.Method)

	// Request Headers
	if request.Headers != nil {
		for name, values := range request.Headers {
			for _, value := range values {
				finalArgs = append(finalArgs, "-H", fmt.Sprintf("%s: %s", name, value))
			}
		}
	}

	// Request Body
	if len(request.Body) > 0 {
		finalArgs = append(finalArgs, "--data-binary", "@-")
	}

	// Output options and URL
	finalArgs = append(finalArgs,
		"-o", respBodyFile.Name(),
		"-D", respHeaderFile.Name(),
		// Using \n directly in -w string, shell usually handles escaping. Go's exec passes args directly.
		// Curl's -w format wants literal newlines if you want multi-line output for its variables.
		// Let's use a single line for now, and parse space-separated, or use a unique separator.
		// A simple %{http_code} is safest for now.
		"-w", "%{http_code}", // This will be the _only_ thing on stdout
		url,
	)

	cmd := exec.Command(c.CurlPath, finalArgs...)

	if len(request.Body) > 0 {
		cmd.Stdin = bytes.NewReader(request.Body)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	execRunErr := cmd.Run()

	stdoutStr := strings.TrimSpace(stdoutBuf.String())
	stderrStr := strings.TrimSpace(stderrBuf.String())

	if execRunErr != nil {
		actualRes.Error = fmt.Errorf("curl command execution failed: %w. Stderr: '%s'. Stdout: '%s'", execRunErr, stderrStr, stdoutStr)
		// Continue to try to parse status code if curl printed it before erroring
	} else if stderrStr != "" {
		// Some curl versions might print informational messages to stderr (e.g., redirect info)
		// Or --show-error might print "curl: (22) The requested URL returned error: 404" here and exit 0 if --fail is not set.
		// Consider if this should always be an error or just logged.
		// For now, if execRunErr is nil, we assume stderrStr is informational or a handled error response.
		// If actualRes.StatusCode later indicates an error (4xx, 5xx), this stderrStr could be useful context.
	}

	// Parse status code from stdout (from -w %{http_code})
	if stdoutStr != "" {
		statusCode, parseErr := strconv.Atoi(stdoutStr)
		if parseErr == nil {
			actualRes.StatusCode = statusCode
		} else {
			err := fmt.Errorf("failed to parse http_code '%s' from curl stdout: %w. Stderr: '%s'", stdoutStr, parseErr, stderrStr)
			if actualRes.Error == nil {
				actualRes.Error = err
			} else {
				actualRes.Error = fmt.Errorf("%v; also %w", actualRes.Error, err)
			}
		}
	} else if actualRes.Error == nil { // No status code from -w and no prior error like exec error
		actualRes.Error = fmt.Errorf("curl stdout was empty, cannot parse status code. Stderr: '%s'", stderrStr)
	}

	// Read and parse response headers from the temporary file
	headerData, readHeadErr := os.ReadFile(respHeaderFile.Name())
	if readHeadErr != nil {
		err := fmt.Errorf("failed to read response headers temp file '%s': %w", respHeaderFile.Name(), readHeadErr)
		if actualRes.Error == nil {
			actualRes.Error = err
		} else {
			actualRes.Error = fmt.Errorf("%v; also %w", actualRes.Error, err)
		}
	} else if len(headerData) > 0 {
		headerReader := bufio.NewReader(bytes.NewReader(headerData))
		// First line is status line, e.g., "HTTP/2 200" or "HTTP/1.1 404 Not Found"
		statusLine, httpErr := headerReader.ReadString('\n')
		if httpErr != nil && httpErr != io.EOF {
			err := fmt.Errorf("failed to read status line from header data: %w", httpErr)
			if actualRes.Error == nil {
				actualRes.Error = err
			} else {
				actualRes.Error = fmt.Errorf("%v; also %w", actualRes.Error, err)
			}
		} else {
			tp := textproto.NewReader(headerReader)
			mimeHeader, parseMimeErr := tp.ReadMIMEHeader()
			if parseMimeErr != nil && parseMimeErr != io.EOF { // io.EOF is fine if there are no headers.
				err := fmt.Errorf("failed to parse MIME headers: %w", parseMimeErr)
				if actualRes.Error == nil {
					actualRes.Error = err
				} else {
					actualRes.Error = fmt.Errorf("%v; also %w", actualRes.Error, err)
				}
			} else {
				actualRes.Headers = http.Header(mimeHeader)
			}

			// If status code wasn't parsed from -w (e.g. actualRes.StatusCode == 0),
			// try to parse from the status line from the header dump.
			if actualRes.StatusCode == 0 && strings.TrimSpace(statusLine) != "" {
				parts := strings.Fields(statusLine) // e.g., ["HTTP/2", "200"] or ["HTTP/1.1", "404", "Not", "Found"]
				if len(parts) >= 2 {
					sc, convErr := strconv.Atoi(parts[1])
					if convErr == nil && sc > 0 {
						actualRes.StatusCode = sc
					}
				}
			}
		}
	}

	// Read response body from the temporary file
	bodyData, readBodyErr := os.ReadFile(respBodyFile.Name())
	if readBodyErr != nil {
		err := fmt.Errorf("failed to read response body temp file '%s': %w", respBodyFile.Name(), readBodyErr)
		if actualRes.Error == nil {
			actualRes.Error = err
		} else {
			actualRes.Error = fmt.Errorf("%v; also %w", actualRes.Error, err)
		}
	} else {
		actualRes.Body = bodyData
	}

	// If StatusCode is 0 after all attempts and no other error reported, it's an issue.
	if actualRes.StatusCode == 0 && actualRes.Error == nil {
		actualRes.Error = fmt.Errorf("failed to determine status code from curl response. Stdout: '%s', Stderr: '%s', Headers: '%s'", stdoutStr, stderrStr, string(headerData))
	}

	return actualRes, actualRes.Error
}

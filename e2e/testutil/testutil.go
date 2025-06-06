package testutil

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/llmahttap/v2/internal/config"
	"github.com/BurntSushi/toml"
	"golang.org/x/net/http2"
)

// TestRequest models an HTTP request for E2E testing.
type TestRequest struct {
	UseTLS  bool
	Method  string
	Path    string // Should include query string if any, e.g., "/path?query=value"
	Headers http.Header
	Body    []byte
}

// HeaderMatcher defines a way to match headers.
type HeaderMatcher map[string]string // Key: header name, Value: expected value (exact match for now)

// BodyMatcher defines a way to match the response body.
type BodyMatcher interface {
	Match(body []byte) (bool, string) // Returns match status and a description of mismatch
}

// ExactBodyMatcher matches the body exactly.
type ExactBodyMatcher struct {
	ExpectedBody []byte
}

// Match implements BodyMatcher for ExactBodyMatcher.
func (m *ExactBodyMatcher) Match(body []byte) (bool, string) {
	if !bytes.Equal(m.ExpectedBody, body) {
		return false, fmt.Sprintf("body mismatch:\nExpected: %q\nActual:   %q", string(m.ExpectedBody), string(body))
	}
	return true, ""
}

// StringContainsBodyMatcher checks if the body contains a specific substring.
type StringContainsBodyMatcher struct {
	Substring string
}

// Match implements BodyMatcher for StringContainsBodyMatcher.
func (m *StringContainsBodyMatcher) Match(body []byte) (bool, string) {
	if !strings.Contains(string(body), m.Substring) {
		return false, fmt.Sprintf("body does not contain expected substring:\nSubstring: %q\nActual:    %q", m.Substring, string(body))
	}
	return true, ""
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
	Config        *config.Config // The configuration used by the server (can be nil if not parsed by test setup)
	Address       string         // The network address the server is listening on (e.g., "127.0.0.1:8080")
	ConfigPath    string         // Path to the temporary config file used by this instance
	LogBuffer     *bytes.Buffer  // Buffer to capture server's stdout/stderr
	logPipeReader *io.PipeReader
	logPipeWriter *io.PipeWriter
	logCopyDone   chan struct{}
	mu            sync.Mutex
	CleanupFuncs  []func() error     // Functions to run to clean up (e.g., remove temp files)
	cmdCtx        context.Context    // Context controlling the Cmd process
	cancelCtx     context.CancelFunc // To cancel the server process context
}

// AddCleanupFunc adds a function to be called when the server instance is stopped.
func (s *ServerInstance) AddCleanupFunc(f func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CleanupFuncs = append(s.CleanupFuncs, f)
}

// GetFreePort asks the kernel for a free open port that is ready to use.
func GetFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// WriteTempConfig creates a temporary configuration file in JSON or TOML format.
// It returns the path to the file and a cleanup function to remove it.
func WriteTempConfig(configData interface{}, format string) (filePath string, cleanupFunc func(), err error) {
	var data []byte
	ext := "." + format

	switch format {
	case "json":
		data, err = json.MarshalIndent(configData, "", "  ")
	case "toml":
		buf := new(bytes.Buffer)
		err = toml.NewEncoder(buf).Encode(configData)
		if err == nil {
			data = buf.Bytes()
		}
	default:
		return "", nil, fmt.Errorf("unsupported config format: %s", format)
	}

	if err != nil {
		return "", nil, fmt.Errorf("failed to marshal config data to %s: %w", format, err)
	}

	tmpFile, err := ioutil.TempFile("", "test-config-*"+ext)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp config file: %w", err)
	}

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", nil, fmt.Errorf("failed to write to temp config file: %w", err)
	}

	filePath = tmpFile.Name()
	cleanupFunc = func() {
		os.Remove(filePath)
	}
	if err := tmpFile.Close(); err != nil {
		cleanupFunc()
		return "", nil, fmt.Errorf("failed to close temp config file: %w", err)
	}

	return filePath, cleanupFunc, nil
}

// StartTestServer launches the server binary with the given configuration file and arguments.
// It waits for the server to log its actual listening address and then polls that address for readiness.
// `serverBinaryPath` must be the path to the server binary.
// `configFile` is the path to the configuration file.
// `configArgName` is the command-line flag name used to pass the config file path (e.g., "-config").
// `serverConfigAddress` is the address string AS CONFIGURED for the server (e.g., "127.0.0.1:0").
// `extraArgs` are any additional command-line arguments for the server.
// The actual listening address will be parsed from server logs and stored in ServerInstance.Address.

func StartTestServer(serverBinaryPath string, configFile string, configArgName string, serverConfigAddress string, extraArgs ...string) (*ServerInstance, error) {
	cmdCtx, cancelCtx := context.WithCancel(context.Background())
	args := append([]string{configArgName, configFile}, extraArgs...)
	cmd := exec.CommandContext(cmdCtx, serverBinaryPath, args...)

	logPipeReader, logPipeWriter := io.Pipe()
	logBuffer := new(bytes.Buffer)
	logCopyDone := make(chan struct{})

	// Tee the pipe writer to both the buffer and os.Stdout for real-time test feedback
	multiWriter := io.MultiWriter(logBuffer, os.Stdout)

	go func() {
		io.Copy(multiWriter, logPipeReader)
		close(logCopyDone)
	}()

	cmd.Stdout = logPipeWriter
	cmd.Stderr = logPipeWriter

	instance := &ServerInstance{
		Cmd:           cmd,
		ConfigPath:    configFile,
		LogBuffer:     logBuffer,
		logPipeReader: logPipeReader,
		logPipeWriter: logPipeWriter,
		logCopyDone:   logCopyDone,
		cmdCtx:        cmdCtx,
		cancelCtx:     cancelCtx,
	}
	instance.AddCleanupFunc(func() error {
		// This cleanup will be called by instance.Stop()
		logPipeWriter.Close()
		<-logCopyDone // Wait for the log copy to finish
		return nil
	})

	err := cmd.Start()
	if err != nil {
		cancelCtx()
		// Try to get some logs before returning
		logPipeWriter.Close()
		<-logCopyDone
		return instance, fmt.Errorf("failed to start server process: %w. Logs: %s", err, instance.SafeGetLogs())
	}

	// Channel to get result from waitForServerAddressLog goroutine
	addrChan := make(chan string)
	errChan := make(chan error)
	processExitChan := make(chan error, 1) // Buffered to not block the goroutine

	// Monitor process exit
	go func() {
		processExitChan <- cmd.Wait()
	}()

	// Wait for the listening address to be logged
	go func() {
		addr, err := waitForServerAddressLog(instance, 5*time.Second, processExitChan)
		if err != nil {
			errChan <- err
		} else {
			addrChan <- addr
		}
	}()

	// Wait for address or an error
	select {
	case addr := <-addrChan:
		instance.Address = addr
	case err := <-errChan:
		instance.Stop() // Try to clean up
		return instance, fmt.Errorf("error waiting for server to be ready: %w. Logs: %s", err, instance.SafeGetLogs())
	case err := <-processExitChan:
		// Process exited before it could signal readiness
		instance.Stop()
		return instance, fmt.Errorf("server process exited prematurely with error: %v. Logs: %s", err, instance.SafeGetLogs())
	case <-time.After(10 * time.Second):
		instance.Stop() // Kill the process
		return instance, fmt.Errorf("timed out waiting for server to start and log listening address. Logs: %s", instance.SafeGetLogs())
	}

	return instance, nil
}

// Stop terminates the server process.
// It first attempts a graceful shutdown (SIGINT), then SIGTERM, then forceful (SIGKILL).
// It also runs any registered cleanup functions.
func (s *ServerInstance) Stop() error {
	s.mu.Lock()
	if s.Cmd == nil || s.Cmd.Process == nil {
		s.mu.Unlock()
		return nil // Already stopped or never started
	}
	s.mu.Unlock()

	// Cancel the context, which might cause the process to exit if it's listening to it.
	s.cancelCtx()

	// Wait a bit before sending signals, giving context cancellation a chance
	time.Sleep(50 * time.Millisecond)

	// Attempt graceful shutdown
	if err := s.Cmd.Process.Signal(os.Interrupt); err != nil {
		// On some systems, os.Interrupt is not supported, or process is already gone
		// Fall through to Terminate
	} else {
		// Wait for a short period for graceful shutdown
		waitChan := make(chan error)
		go func() {
			waitChan <- s.Cmd.Wait()
		}()
		select {
		case <-waitChan:
			goto cleanup
		case <-time.After(2 * time.Second):
			// Graceful shutdown timed out
		}
	}

	// Forceful shutdown
	if err := s.Cmd.Process.Kill(); err != nil {
		if !errors.Is(err, os.ErrProcessDone) {
			fmt.Printf("Warning: failed to kill server process (PID %d): %v\n", s.Cmd.Process.Pid, err)
		}
	}
	// Wait for the process to be reaped
	s.Cmd.Wait()

cleanup:
	s.mu.Lock()
	defer s.mu.Unlock()
	var finalErr error
	// Run cleanup funcs in reverse order
	for i := len(s.CleanupFuncs) - 1; i >= 0; i-- {
		if err := s.CleanupFuncs[i](); err != nil {
			// Capture the first error, but try to run all cleanups
			if finalErr == nil {
				finalErr = err
			}
			fmt.Printf("Warning: cleanup function failed: %v\n", err)
		}
	}
	s.Cmd = nil // Mark as stopped
	return finalErr
}

// HTTPTestClient defines an interface for making HTTP requests for testing purposes.
type HTTPTestClient interface {
	Do(serverAddr string, request TestRequest) (ActualResponse, error)
}

// HTTPClientType identifies the type of HTTP client used for a test.
type HTTPClientType string

// TestRunner is an interface for an entity that can run a TestRequest
// against a ServerInstance and return an ActualResponse.
type TestRunner interface {
	Run(server *ServerInstance, req *TestRequest) (*ActualResponse, error)
	Type() HTTPClientType
}

// CurlHTTPClient implements HTTPTestClient using the curl command-line tool.
type CurlHTTPClient struct {
	CurlPath string
}

// NewCurlHTTPClient creates a new CurlHTTPClient.
func NewCurlHTTPClient(curlPath string) *CurlHTTPClient {
	if curlPath == "" {
		curlPath = "curl"
	}
	return &CurlHTTPClient{CurlPath: curlPath}
}

// Type returns the client type.
func (c *CurlHTTPClient) Type() HTTPClientType {
	return "CurlClient"
}

// Do executes an HTTP request using curl.
func (c *CurlHTTPClient) Do(serverAddr string, request TestRequest) (ActualResponse, error) {
	scheme := "http"
	args := []string{"-s", "-v"} // Common args

	if request.UseTLS {
		scheme = "https"
		// For HTTPS, we use ALPN. So we don't specify --http2-prior-knowledge.
		// We use -k (--insecure) to allow self-signed certs.
		// The server should negotiate h2 via ALPN.
		args = append(args, "-k")
	} else {
		scheme = "http"
		// For HTTP (H2C), we need --http2-prior-knowledge to speak HTTP/2 from the start.
		args = append(args, "--http2-prior-knowledge")
	}
	url := fmt.Sprintf("%s://%s%s", scheme, serverAddr, request.Path)

	args = append(args, "-X", request.Method)

	for name, values := range request.Headers {
		for _, value := range values {
			args = append(args, "-H", fmt.Sprintf("%s: %s", name, value))
		}
	}

	args = append(args, url)

	// Create command
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.CurlPath, args...)
	if len(request.Body) > 0 {
		cmd.Stdin = bytes.NewReader(request.Body)
	}

	var stderrBuf, stdoutBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	cmd.Stdout = &stdoutBuf

	err := cmd.Run()

	actual := ActualResponse{
		Headers: make(http.Header),
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return actual, fmt.Errorf("curl command timed out. Stderr: %s. Stdout: %s", stderrBuf.String(), stdoutBuf.String())
		}
		// If curl exits with an error, it might still have received a response
		// We'll proceed with parsing, but wrap the original error later.
	}

	// Parse headers and body from stderr/stdout. -v sends headers to stderr.
	stderrStr := stderrBuf.String()
	scanner := bufio.NewScanner(strings.NewReader(stderrStr))
	var statusLine string
	inHeaders := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "< HTTP/2") || strings.HasPrefix(line, "< HTTP/1.1") {
			inHeaders = true
			statusLine = line
			continue
		}
		// In curl's verbose output, response headers are prefixed with "< "
		if inHeaders && strings.HasPrefix(line, "< ") {
			// An empty line marks the end of headers
			if len(strings.TrimSpace(line)) == 1 {
				inHeaders = false
				continue
			}
			if parts := strings.SplitN(line, ": ", 2); len(parts) == 2 {
				headerName := http.CanonicalHeaderKey(strings.TrimPrefix(parts[0], "< "))
				actual.Headers.Add(headerName, parts[1])
			}
		}
	}

	if statusLine == "" {
		if err != nil {
			return actual, fmt.Errorf("curl command failed and could not parse HTTP status line. Error: %w. Stderr: %s", err, stderrStr)
		}
		return actual, fmt.Errorf("could not parse HTTP status line from curl output. Stderr: %s", stderrStr)
	}

	// Parse status code from "HTTP/2 200" or similar
	if n, _ := fmt.Sscanf(statusLine, "< HTTP/2 %d", &actual.StatusCode); n != 1 {
		if n, _ := fmt.Sscanf(statusLine, "< HTTP/1.1 %d", &actual.StatusCode); n != 1 {
			return actual, fmt.Errorf("could not parse status code from curl status line '%s'. Stderr: %s", statusLine, stderrStr)
		}
	}

	actual.Body = stdoutBuf.Bytes()
	actual.Error = err // Store original command error if there was one

	return actual, nil
}

// GoNetHTTPClient implements HTTPTestClient using Go's net/http package.
type GoNetHTTPClient struct {
	client *http.Client
}

// NewGoNetHTTPClient creates a new GoNetHTTPClient configured for both H2C and H2-over-TLS.
func NewGoNetHTTPClient() *GoNetHTTPClient {
	return &GoNetHTTPClient{
		client: &http.Client{
			Transport: &http2.Transport{
				AllowHTTP: true, // Allow cleartext HTTP/2 (H2C)
				// The custom DialTLS is removed. The transport will now correctly
				// use a regular net.Dial for http URLs and a tls.Dial for https URLs.
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // For tests, trust self-signed certs
				},
			},
		},
	}
}

// Type returns the client type.
func (c *GoNetHTTPClient) Type() HTTPClientType {
	return "GoNetHTTPClient"
}

// Do executes an HTTP request using Go's net/http client.
func (c *GoNetHTTPClient) Do(serverAddr string, request TestRequest) (ActualResponse, error) {
	scheme := "http"
	if request.UseTLS {
		scheme = "https"
	}

	url := fmt.Sprintf("%s://%s%s", scheme, serverAddr, request.Path)
	req, err := http.NewRequest(request.Method, url, bytes.NewReader(request.Body))
	if err != nil {
		return ActualResponse{}, fmt.Errorf("failed to create http request: %w", err)
	}

	if request.Headers != nil {
		req.Header = request.Headers
	}
	// The http client adds Host header from req.URL.Host

	resp, err := c.client.Do(req)
	actual := ActualResponse{
		Error: err,
	}
	if err != nil {
		// The error might be a URL error, which contains more info
		return actual, err
	}
	defer resp.Body.Close()

	bodyBytes, errRead := ioutil.ReadAll(resp.Body)
	if errRead != nil {
		actual.Error = errRead // Store read error
	}

	actual.StatusCode = resp.StatusCode
	actual.Headers = resp.Header
	actual.Body = bodyBytes

	return actual, nil
}

// Run implements the TestRunner interface for GoNetHTTPClient.
func (c *GoNetHTTPClient) Run(server *ServerInstance, req *TestRequest) (*ActualResponse, error) {
	resp, err := c.Do(server.Address, *req)
	return &resp, err
}

// Run implements the TestRunner interface for CurlHTTPClient.
func (c *CurlHTTPClient) Run(server *ServerInstance, req *TestRequest) (*ActualResponse, error) {
	resp, err := c.Do(server.Address, *req)
	return &resp, err
}

// Assertion helper functions

// AssertStatusCode checks if the actual status code matches the expected status code.
func AssertStatusCode(t *testing.T, expected ExpectedResponse, actual ActualResponse) {
	t.Helper()
	if expected.StatusCode != actual.StatusCode {
		t.Errorf("expected status code %d, got %d", expected.StatusCode, actual.StatusCode)
	}
}

// AssertHeaderEquals checks if a specific header in the actual response has the expected value.
func AssertHeaderEquals(t *testing.T, actualHeaders http.Header, headerName string, expectedValue string) {
	t.Helper()
	val := actualHeaders.Get(headerName)
	if val != expectedValue {
		t.Errorf("expected header '%s: %s', got '%s'", headerName, expectedValue, val)
	}
}

// AssertHeaderContains checks if a specific header in the actual response contains the expected substring.
func AssertHeaderContains(t *testing.T, actualHeaders http.Header, headerName string, expectedSubstring string) {
	t.Helper()
	val := actualHeaders.Get(headerName)
	if !strings.Contains(val, expectedSubstring) {
		t.Errorf("expected header '%s' to contain '%s', but got '%s'", headerName, expectedSubstring, val)
	}
}

// AssertBodyEquals checks if the actual response body matches the expected body byte-for-byte.
func AssertBodyEquals(t *testing.T, expectedBody []byte, actualBody []byte) {
	t.Helper()
	if !bytes.Equal(expectedBody, actualBody) {
		t.Errorf("body mismatch:\nExpected: %q\nActual:   %q", string(expectedBody), string(actualBody))
	}
}

// AssertBodyContains checks if the actual response body contains the expected substring.
func AssertBodyContains(t *testing.T, expectedSubstring string, actualBody []byte) {
	t.Helper()
	if !strings.Contains(string(actualBody), expectedSubstring) {
		t.Errorf("body does not contain expected substring:\nSubstring: %q\nActual:    %q", expectedSubstring, string(actualBody))
	}
}

// AssertJSONBodyFields checks if the actual JSON response body (provided as bytes) contains the expected fields
// with the specified values. It performs a shallow comparison for map[string]interface{} values.
// For nested objects or more complex structures, a more sophisticated deep comparison or dedicated library might be needed.
func AssertJSONBodyFields(t *testing.T, expectedFields map[string]interface{}, actualBodyJSON []byte) {
	t.Helper()
	var actualData map[string]interface{}
	if err := json.Unmarshal(actualBodyJSON, &actualData); err != nil {
		t.Errorf("failed to unmarshal actual body as JSON: %v. Body: %s", err, string(actualBodyJSON))
		return
	}

	if !reflect.DeepEqual(expectedFields, actualData) {
		expectedJSON, _ := json.MarshalIndent(expectedFields, "", "  ")
		actualJSON, _ := json.MarshalIndent(actualData, "", "  ")
		t.Errorf("JSON body mismatch:\n--- Expected ---\n%s\n--- Actual ---\n%s", string(expectedJSON), string(actualJSON))
	}
}

// E2ETestCase represents a single request-response pair within a larger E2E test definition.
type E2ETestCase struct {
	Name     string // Optional, for descriptive test names in output
	Request  TestRequest
	Expected ExpectedResponse
}

// E2ETestDefinition defines a complete end-to-end test scenario.
type E2ETestDefinition struct {
	Name                string        // For the overall test set, used in t.Run
	ServerBinaryPath    string        // Path to the server executable
	ServerConfigData    interface{}   // Data to be marshalled into the config file
	ServerConfigFormat  string        // "json" or "toml"
	ServerConfigArgName string        // Command-line argument name for config file (e.g., "-config")
	ServerListenAddress string        // e.g., "127.0.0.1:8080" or "127.0.0.1:0" for dynamic port
	ExtraServerArgs     []string      // Any additional command-line arguments for the server
	TestCases           []E2ETestCase // The sequence of test requests and their expected outcomes
	CurlPath            string        // Optional: path to curl executable, defaults to "curl" if empty
	SetupFunc           func(t *testing.T, def *E2ETestDefinition)
}

// RunE2ETest executes a defined E2E test scenario.
// It handles server setup, execution of test cases with multiple clients, and assertions.
func RunE2ETest(t *testing.T, def E2ETestDefinition) {
	t.Helper()

	if def.SetupFunc != nil {
		def.SetupFunc(t, &def)
	}

	configFile, cleanup, err := WriteTempConfig(def.ServerConfigData, def.ServerConfigFormat)
	if err != nil {
		t.Fatalf("[%s] Failed to write temp config: %v", def.Name, err)
	}
	defer cleanup()

	server, err := StartTestServer(def.ServerBinaryPath, configFile, def.ServerConfigArgName, def.ServerListenAddress, def.ExtraServerArgs...)
	if err != nil {
		logStr := ""
		if server != nil {
			logStr = server.SafeGetLogs()
		}
		t.Fatalf("[%s] Failed to start server: %v\nLogs:\n%s", def.Name, err, logStr)
	}
	defer server.Stop()

	clients := []struct {
		name   string
		client TestRunner
	}{
		{"GoNetHTTPClient", NewGoNetHTTPClient()},
		{"CurlClient", NewCurlHTTPClient(def.CurlPath)},
	}

	for _, client := range clients {
		t.Run(client.name, func(st *testing.T) {
			st.Helper()
			// Optional: add a cleanup to dump logs on sub-test failure
			st.Cleanup(func() {
				if st.Failed() {
					logs := server.SafeGetLogs()
					st.Logf("BEGIN Server logs for client %s, test %s (on failure):\n%s\nEND Server logs", client.name, def.Name, logs)
				}
			})

			for _, tc := range def.TestCases {
				caseName := tc.Name
				if caseName == "" {
					caseName = fmt.Sprintf("%s_%s", tc.Request.Method, tc.Request.Path)
				}
				st.Run(caseName, func(sst *testing.T) {
					sst.Helper()
					actual, err := client.client.Run(server, &tc.Request)

					if tc.Expected.ErrorContains != "" {
						if err == nil {
							sst.Errorf("expected an error containing '%s', but got no error", tc.Expected.ErrorContains)
						} else if !strings.Contains(err.Error(), tc.Expected.ErrorContains) {
							sst.Errorf("expected error to contain '%s', but got: %v", tc.Expected.ErrorContains, err)
						}
						return // Stop further checks if an error was expected
					}
					if err != nil {
						sst.Fatalf("request failed unexpectedly: %v", err)
					}

					AssertStatusCode(sst, tc.Expected, *actual)

					if tc.Expected.Headers != nil {
						for k, v := range tc.Expected.Headers {
							AssertHeaderEquals(sst, actual.Headers, k, v)
						}
					}

					if tc.Expected.ExpectNoBody {
						if len(actual.Body) > 0 {
							sst.Errorf("expected no response body, but got %d bytes: %q", len(actual.Body), string(actual.Body))
						}
					} else if tc.Expected.BodyMatcher != nil {
						if ok, msg := tc.Expected.BodyMatcher.Match(actual.Body); !ok {
							sst.Error(msg)
						}
					}
				})
			}
		})
	}
}

// JSONFieldsBodyMatcher checks if the actual JSON response body (provided as bytes) contains the expected fields
// with the specified values. It uses reflect.DeepEqual for comparison.
type JSONFieldsBodyMatcher struct {
	ExpectedFields map[string]interface{}
}

// Match implements BodyMatcher for JSONFieldsBodyMatcher.
func (m *JSONFieldsBodyMatcher) Match(body []byte) (bool, string) {
	var actualData map[string]interface{}
	if err := json.Unmarshal(body, &actualData); err != nil {
		return false, fmt.Sprintf("failed to unmarshal actual body as JSON: %v. Body: %s", err, string(body))
	}

	// Use DeepEqual for comprehensive comparison of nested structures
	if !reflect.DeepEqual(m.ExpectedFields, actualData) {
		expectedJSON, _ := json.MarshalIndent(m.ExpectedFields, "", "  ")
		actualJSON, _ := json.MarshalIndent(actualData, "", "  ")
		return false, fmt.Sprintf("JSON body mismatch:\n--- Expected ---\n%s\n--- Actual ---\n%s", string(expectedJSON), string(actualJSON))
	}

	return true, ""
}

// waitForServerAddressLog scans the server's log output for the listening address.
// It polls the instance.LogBuffer for new log lines.

func waitForServerAddressLog(instance *ServerInstance, timeout time.Duration, processExitChan <-chan error) (string, error) {
	// Example log line: `{"level":"INFO","msg":"Server listening on actual address","ts":"...","details":{"address":"127.0.0.1:45679"}}`
	re := regexp.MustCompile(`"address":"([^"]+)"`)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case err := <-processExitChan:
			return "", fmt.Errorf("process exited while waiting for address log: %w", err)
		default:
		}

		logContent := instance.SafeGetLogs()
		matches := re.FindAllStringSubmatch(logContent, -1)
		// We need the *last* match because initialization might log multiple things.
		// A better approach would be to have a very specific log message.
		// For now, let's look for a listen address with a non-zero port.
		for i := len(matches) - 1; i >= 0; i-- {
			if len(matches[i]) > 1 {
				addr := matches[i][1]
				_, port, err := net.SplitHostPort(addr)
				if err == nil && port != "0" {
					return addr, nil
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for server listening address in logs")
}

// SafeGetLogs returns the content of the LogBuffer, or a placeholder if the instance/buffer is nil.
func (s *ServerInstance) SafeGetLogs() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s == nil || s.LogBuffer == nil {
		return "[log buffer not available]"
	}
	return s.LogBuffer.String()
}

func StartingPollingAndPrintingBuffer(buffer *bytes.Buffer) {
	// This function seems unused or a duplicate.
	// Intentionally left blank.
}

// ToRawMessageBytes is a helper for E2E tests to convert a handler config struct
// into json.RawMessage bytes.
func ToRawMessageBytes(t *testing.T, v interface{}) []byte {
	t.Helper()
	bytes, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Failed to marshal handler config to json.RawMessage bytes: %v", err)
	}
	return bytes
}

// ToRawMessageWrapper is a test helper that converts a handler configuration
// struct (like config.StaticFileServerConfig) into a config.RawMessageWrapper
// by marshalling it to JSON.
func ToRawMessageWrapper(t *testing.T, handlerCfg interface{}) config.RawMessageWrapper {
	t.Helper()
	jsonData, err := json.Marshal(handlerCfg)
	if err != nil {
		t.Fatalf("ToRawMessageWrapper: failed to marshal handler config to JSON: %v", err)
	}
	return config.RawMessageWrapper(jsonData)
}

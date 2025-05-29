package testutil

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls" // Added import
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	// "path/filepath" // Removed unused import
	"reflect" // Added to fix undefined: reflect error
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"testing"

	// "crypto/tls" // crypto/tls removed as unused
	"example.com/llmahttap/v2/internal/config"
	"github.com/BurntSushi/toml"
	"golang.org/x/net/http2"
)

// TestRequest models an HTTP request for E2E testing.
type TestRequest struct {
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
	if bytes.Equal(m.ExpectedBody, body) {
		return true, ""
	}
	return false, fmt.Sprintf("bodies do not match exactly. Expected: %q, Got: %q", string(m.ExpectedBody), string(body))
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
	return false, fmt.Sprintf("body does not contain substring: %q. Body: %q", m.Substring, string(body))
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
	var ext string

	switch strings.ToLower(format) {
	case "json":
		data, err = json.MarshalIndent(configData, "", "  ")
		ext = ".json"
	case "toml":
		buf := new(bytes.Buffer)
		if err = toml.NewEncoder(buf).Encode(configData); err == nil {
			data = buf.Bytes()
		}
		ext = ".toml"
	default:
		err = fmt.Errorf("unsupported config format: %s", format)
	}

	if err != nil {
		return "", nil, fmt.Errorf("failed to marshal config data to %s: %w", format, err)
	}

	tmpFile, err := os.CreateTemp("", "testconfig-*"+ext)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp config file: %w", err)
	}

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", nil, fmt.Errorf("failed to write to temp config file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		return "", nil, fmt.Errorf("failed to close temp config file: %w", err)
	}

	filePath = tmpFile.Name()
	cleanupFunc = func() { os.Remove(filePath) }
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
	if serverBinaryPath == "" {
		return nil, fmt.Errorf("serverBinaryPath cannot be empty")
	}
	fi, err := os.Stat(serverBinaryPath)
	if err != nil {
		return nil, fmt.Errorf("server binary path '%s' error: %w", serverBinaryPath, err)
	}
	if fi.IsDir() || (fi.Mode()&0111 == 0) { // Check if it's a directory or not executable
		return nil, fmt.Errorf("server binary path '%s' is a directory or not executable", serverBinaryPath)
	}
	if configFile == "" {
		return nil, fmt.Errorf("configFile cannot be empty")
	}
	if configArgName == "" {
		return nil, fmt.Errorf("configArgName cannot be empty (e.g. -config)")
	}
	// serverConfigAddress can be "host:0", so not validating its format strictly here.

	ctx, cancel := context.WithCancel(context.Background())

	args := []string{}
	args = append(args, configArgName, configFile)
	args = append(args, extraArgs...)

	cmd := exec.CommandContext(ctx, serverBinaryPath, args...)

	logBuffer := new(bytes.Buffer)
	pipeReader, pipeWriter := io.Pipe()
	cmd.Stdout = pipeWriter
	cmd.Stderr = pipeWriter

	instance := &ServerInstance{
		Cmd:           cmd,
		ConfigPath:    configFile,
		Address:       "", // Will be populated by parsing logs
		LogBuffer:     logBuffer,
		logPipeReader: pipeReader,
		logPipeWriter: pipeWriter,
		logCopyDone:   make(chan struct{}),
		cmdCtx:        ctx, // Store the context that controls the command
		cancelCtx:     cancel,
		CleanupFuncs:  make([]func() error, 0),
	}

	// Cleanup for log pipes
	instance.AddCleanupFunc(func() error {
		var errs []string
		if instance.logPipeWriter != nil {
			if err := instance.logPipeWriter.Close(); err != nil {
				// This error can happen if reader is already closed, often not critical here
			}
		}
		if instance.logCopyDone != nil {
			select {
			case <-instance.logCopyDone:
			// Log copying finished
			case <-time.After(1 * time.Second): // Timeout for log copy to finish
				errs = append(errs, "log copy timed out")
			}
		}
		if instance.logPipeReader != nil {
			if err := instance.logPipeReader.Close(); err != nil {
				errs = append(errs, fmt.Sprintf("logPipeReader.Close: %v", err))
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		return nil
	})
	fmt.Println("Copying test server logs to logPipeReader")
	go func() {
		defer close(instance.logCopyDone)
		io.Copy(instance.LogBuffer, instance.logPipeReader)
	}()

	if err := cmd.Start(); err != nil {
		instance.Stop() // calls cleanups
		return nil, fmt.Errorf("failed to start server process '%s': %w. Logs captured:\n%s", serverBinaryPath, err, instance.LogBuffer.String())
	}

	fmt.Println("Waiting for server to log listening address")
	// Wait for server to log its actual listening address
	actualListenAddress, err := waitForServerAddressLog(instance, 5*time.Second)
	if err != nil {
		instance.Stop()
		return nil, fmt.Errorf("server did not log listening address or timed out: %w. Logs captured:\n%s", err, instance.SafeGetLogs())
	}
	instance.Address = actualListenAddress // Set the parsed actual address

	// Poll the actualListenAddress for readiness
	readyTimeout := 2 * time.Second
	pollInterval := 250 * time.Millisecond
	startTime := time.Now()
	var lastDialErr error
	for {
		if time.Since(startTime) > readyTimeout {
			instance.Stop()
			return nil, fmt.Errorf("server not ready at parsed address %s after %v. Last dial error: %v. Logs captured:\n%s", actualListenAddress, readyTimeout, lastDialErr, instance.SafeGetLogs())
		}

		conn, dialErr := net.DialTimeout("tcp", actualListenAddress, pollInterval)
		lastDialErr = dialErr
		if dialErr == nil {
			conn.Close()

			// time.Sleep(750 * time.Millisecond) // Removed this sleep
			break // Server is ready
		}

		// Check if the server process exited prematurely
		select {
		case <-instance.cmdCtx.Done(): // Use the stored context
			instance.Stop()                // Ensure resources are cleaned up
			exitErrVal := instance.Cmd.Err // Access field
			return nil, fmt.Errorf("server process exited while waiting for readiness at %s. Last dial error: %v. ExitError: %v. Logs:\n%s",
				actualListenAddress, lastDialErr, exitErrVal, instance.SafeGetLogs())
		default:
		}
		time.Sleep(pollInterval)
	}
	return instance, nil
}

// AddCleanupFunc adds a function to be called when the server instance is stopped.
func (s *ServerInstance) AddCleanupFunc(f func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CleanupFuncs = append(s.CleanupFuncs, f)
}

// Stop terminates the server process.
// It first attempts a graceful shutdown (SIGINT), then SIGTERM, then forceful (SIGKILL).
// It also runs any registered cleanup functions.
func (s *ServerInstance) Stop() error {
	s.mu.Lock()

	var errors []string

	if s.Cmd == nil {
		s.mu.Unlock() // Unlock before running cleanups if Cmd is nil
		// Run cleanups if server was never started or already fully cleaned
		for i := len(s.CleanupFuncs) - 1; i >= 0; i-- {
			if err := s.CleanupFuncs[i](); err != nil {
				errors = append(errors, fmt.Sprintf("cleanup_func_%d: %v", i, err))
			}
		}
		s.CleanupFuncs = nil // Avoid double execution
		if len(errors) > 0 {
			return fmt.Errorf("errors during cleanup for non-started/stopped server: %s", strings.Join(errors, "; "))
		}
		return nil
	}

	// If Process is nil, it means Start() failed or it already exited/was stopped.
	// Still run cleanups.
	processWasNil := s.Cmd.Process == nil
	s.mu.Unlock() // Unlock before potentially long operations like Signal/Wait

	if !processWasNil {
		// Ensure the context controlling the command is cancelled
		// This helps if the process is stuck and not responding to signals
		if s.cancelCtx != nil {
			s.cancelCtx()
		}

		// Attempt graceful shutdown: SIGINT
		// On Windows, SIGINT might not work as expected for console apps not handling it.
		// exec.Cmd.Signal handles this by sending Ctrl+Break on Windows.
		if err := s.Cmd.Process.Signal(syscall.SIGINT); err == nil {
			done := make(chan error, 1)
			go func() {
				done <- s.Cmd.Wait()
			}()
			select {
			case <-done: // Process exited
				goto cleanup
			case <-time.After(3 * time.Second):
				// SIGINT timed out
			}
		}

		// Attempt SIGTERM (more forceful than SIGINT, less than SIGKILL)
		if s.Cmd.ProcessState == nil || !s.Cmd.ProcessState.Exited() { // Check if still running
			if err := s.Cmd.Process.Signal(syscall.SIGTERM); err == nil {
				done := make(chan error, 1)
				go func() {
					done <- s.Cmd.Wait()
				}()
				select {
				case <-done: // Process exited
					goto cleanup
				case <-time.After(2 * time.Second):
					// SIGTERM timed out
				}
			}
		}

		// Forceful shutdown: SIGKILL
		if s.Cmd.ProcessState == nil || !s.Cmd.ProcessState.Exited() { // Check if still running
			if err := s.Cmd.Process.Kill(); err != nil {
				// This error might mean process already exited.
				// errors = append(errors, fmt.Sprintf("failed to kill process: %v", err))
			}
			s.Cmd.Wait() // Wait for SIGKILL to take effect, collect exit status
		}
	}

cleanup:
	s.mu.Lock() // Re-lock to safely modify s.Cmd and s.CleanupFuncs
	s.Cmd = nil // Mark as stopped / cleaned up

	// Run cleanup functions in reverse order of addition
	for i := len(s.CleanupFuncs) - 1; i >= 0; i-- {
		if err := s.CleanupFuncs[i](); err != nil {
			errors = append(errors, fmt.Sprintf("cleanup_func_%d: %v", i, err))
		}
	}
	s.CleanupFuncs = nil // Avoid double execution
	s.mu.Unlock()

	if len(errors) > 0 {
		return fmt.Errorf("server stopped, but errors occurred during stop/cleanup: %s", strings.Join(errors, "; "))
	}
	return nil
}

// HTTPTestClient defines an interface for making HTTP requests for testing purposes.
type HTTPTestClient interface {
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
	CurlPath string
}

// NewCurlHTTPClient creates a new CurlHTTPClient.
func NewCurlHTTPClient(curlPath string) *CurlHTTPClient {
	cp := curlPath
	if cp == "" {
		cp = "curl"
	}
	return &CurlHTTPClient{CurlPath: cp}
}

// Type returns the client type.
func (c *CurlHTTPClient) Type() HTTPClientType {
	return CurlClient
}

// Do executes an HTTP request using curl.
func (c *CurlHTTPClient) Do(serverAddr string, request TestRequest) (ActualResponse, error) {
	actualRes := ActualResponse{
		Headers: make(http.Header),
	}

	respHeaderFile, err := os.CreateTemp("", "curl_resp_headers_*.txt")
	if err != nil {
		actualRes.Error = fmt.Errorf("failed to create temp file for response headers: %w", err)
		return actualRes, actualRes.Error
	}
	defer os.Remove(respHeaderFile.Name())
	// Close immediately so curl can write to it.
	if err := respHeaderFile.Close(); err != nil {
		actualRes.Error = fmt.Errorf("failed to close temp file for response headers path: %w", err)
		return actualRes, actualRes.Error
	}

	respBodyFile, err := os.CreateTemp("", "curl_resp_body_*.bin")
	if err != nil {
		actualRes.Error = fmt.Errorf("failed to create temp file for response body: %w", err)
		return actualRes, actualRes.Error
	}
	defer os.Remove(respBodyFile.Name())
	if err := respBodyFile.Close(); err != nil {
		actualRes.Error = fmt.Errorf("failed to close temp file for response body path: %w", err)
		return actualRes, actualRes.Error
	}

	scheme := "http://"
	if strings.HasPrefix(serverAddr, "http://") || strings.HasPrefix(serverAddr, "https://") {
		scheme = ""
	}

	path := request.Path
	if !strings.HasPrefix(path, "/") && path != "" {
		path = "/" + path
	}
	url := fmt.Sprintf("%s%s%s", scheme, serverAddr, path)

	if request.Method == "" {
		request.Method = "GET"
	}

	var finalArgs []string
	finalArgs = append(finalArgs, "--http2-prior-knowledge")
	finalArgs = append(finalArgs, "--verbose") // More detailed output for debugging
	// finalArgs = append(finalArgs, "--silent") // Replaced by --verbose for debugging
	// finalArgs = append(finalArgs, "--show-error") // Verbose includes errors
	finalArgs = append(finalArgs, "-X", request.Method)
	finalArgs = append(finalArgs, "--noproxy", "*") // Prevent accidental proxy usage

	if request.Headers != nil {
		for name, values := range request.Headers {
			for _, value := range values {
				finalArgs = append(finalArgs, "-H", fmt.Sprintf("%s: %s", name, value))
			}
		}
	}

	if len(request.Body) > 0 {
		finalArgs = append(finalArgs, "--data-binary", "@-")
	}

	finalArgs = append(finalArgs,
		"-o", respBodyFile.Name(),
		"-D", respHeaderFile.Name(),
		"-w", "%{http_code}",
		url,
	)

	curlCmdPath := c.CurlPath
	if curlCmdPath == "" {
		curlCmdPath = "curl"
	}
	cmd := exec.Command(curlCmdPath, finalArgs...)

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
		// Preserve existing error if curl also wrote status code, otherwise use this as primary.
		// This error might be like "exit status X".
		errFmt := "curl command execution failed: %w. Stderr: '%s'. Stdout: '%s'"
		newErr := fmt.Errorf(errFmt, execRunErr, stderrStr, stdoutStr)
		if actualRes.Error == nil { // If no other error (like parsing status code first)
			actualRes.Error = newErr
		} else {
			actualRes.Error = fmt.Errorf("%v; also, %w", actualRes.Error, newErr)
		}
	}

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
	} else if actualRes.Error == nil && execRunErr == nil { // No status from -w, no exec error. This is unexpected.
		actualRes.Error = fmt.Errorf("curl stdout was empty (no http_code), and no execution error. Stderr: '%s'", stderrStr)
	}

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
		statusLine, httpErr := headerReader.ReadString('\n')
		if httpErr != nil && httpErr != io.EOF { // EOF is okay if only status line was present
			err := fmt.Errorf("failed to read status line from header data: %w", httpErr)
			if actualRes.Error == nil {
				actualRes.Error = err
			} else {
				actualRes.Error = fmt.Errorf("%v; also %w", actualRes.Error, err)
			}
		}

		// Try to parse headers even if status line read failed, tp.ReadMIMEHeader might still work.
		tp := textproto.NewReader(headerReader) // headerReader now points after statusLine
		mimeHeader, parseMimeErr := tp.ReadMIMEHeader()
		if parseMimeErr != nil && parseMimeErr != io.EOF { // io.EOF is fine if there are no headers.
			err := fmt.Errorf("failed to parse MIME headers: %w", parseMimeErr)
			if actualRes.Error == nil {
				actualRes.Error = err
			} else {
				actualRes.Error = fmt.Errorf("%v; also %w", actualRes.Error, err)
			}
		} else if mimeHeader != nil { // Only assign if mimeHeader successfully parsed
			actualRes.Headers = http.Header(mimeHeader)
		}

		// If status code wasn't parsed from -w (e.g. actualRes.StatusCode == 0),
		// try to parse from the status line from the header dump. This is a fallback.
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
		actualRes.Error = fmt.Errorf("failed to determine status code from curl response. Stdout: '%s', Stderr: '%s', HeaderFile Content: '%s'", stdoutStr, stderrStr, string(headerData))
	}
	return actualRes, actualRes.Error
}

// GoNetHTTPClient implements HTTPTestClient using Go's net/http package for H2C.
type GoNetHTTPClient struct {
	client *http.Client
}

// NewGoNetHTTPClient creates a new GoNetHTTPClient configured for HTTP/2 Cleartext (H2C).
func NewGoNetHTTPClient() *GoNetHTTPClient {

	dialer := &net.Dialer{
		Timeout:   5 * time.Second, // Timeout for dialing connection
		KeepAlive: 30 * time.Second,
	}

	transport := &http2.Transport{
		AllowHTTP: true, // Allow non-HTTPS H2
		// For H2C, we need to provide a custom dialer that performs a cleartext TCP connection.
		// TLSClientConfig should be nil for H2C.
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			// If cfg is not nil, it implies TLS is expected. For pure H2C, cfg might be nil.
			// However, http.Client might still populate it. The key is to ignore it for H2C.
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig: nil,
	}

	return &GoNetHTTPClient{
		client: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second, // Overall timeout for requests
		},
	}
}

// Type returns the client type.
func (c *GoNetHTTPClient) Type() HTTPClientType {
	return GoHTTPClient
}

// Do executes an HTTP request using Go's net/http client configured for H2C.
func (c *GoNetHTTPClient) Do(serverAddr string, request TestRequest) (ActualResponse, error) {
	actualRes := ActualResponse{
		Headers: make(http.Header),
	}

	scheme := "http://"
	// Remove existing scheme if present, as we force http for H2C
	if i := strings.Index(serverAddr, "://"); i != -1 {
		serverAddr = serverAddr[i+3:]
	}

	path := request.Path
	if !strings.HasPrefix(path, "/") && path != "" {
		path = "/" + path
	}
	url := fmt.Sprintf("%s%s%s", scheme, serverAddr, path)

	method := request.Method
	if method == "" {
		method = "GET"
	}

	var bodyReader io.Reader
	if len(request.Body) > 0 {
		bodyReader = bytes.NewReader(request.Body)
	}

	httpReq, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		actualRes.Error = fmt.Errorf("failed to create http.Request: %w", err)
		return actualRes, actualRes.Error
	}

	// http.NewRequest initializes httpReq.Header to a non-nil map.
	// Populate it from request.Headers.
	if request.Headers != nil {
		for k, vv := range request.Headers {
			for _, v := range vv {
				httpReq.Header.Add(k, v)
			}
		}
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		actualRes.Error = fmt.Errorf("http client failed to execute request: %w", err)
		return actualRes, actualRes.Error
	}
	defer resp.Body.Close()

	actualRes.StatusCode = resp.StatusCode
	actualRes.Headers = resp.Header

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		actualRes.Error = fmt.Errorf("failed to read response body: %w", readErr)
		// actualRes.Error will be returned by the function if it's the first error.
	}
	actualRes.Body = respBody

	// If readErr occurred after a successful HTTP exchange, actualRes.Error might be nil here.
	// Ensure actualRes.Error captures readErr if it happened.
	if actualRes.Error == nil && readErr != nil {
		actualRes.Error = readErr
	}

	return actualRes, actualRes.Error
}

// Run implements the TestRunner interface for GoNetHTTPClient.
func (c *GoNetHTTPClient) Run(server *ServerInstance, req *TestRequest) (*ActualResponse, error) {
	if server == nil {
		return nil, fmt.Errorf("server instance cannot be nil for GoNetHTTPClient.Run")
	}
	if req == nil {
		return nil, fmt.Errorf("TestRequest cannot be nil for GoNetHTTPClient.Run")
	}

	actualResp, err := c.Do(server.Address, *req)
	return &actualResp, err
}

// Run implements the TestRunner interface for CurlHTTPClient.
func (c *CurlHTTPClient) Run(server *ServerInstance, req *TestRequest) (*ActualResponse, error) {
	if server == nil {
		return nil, fmt.Errorf("server instance cannot be nil for CurlHTTPClient.Run")
	}
	if req == nil {
		return nil, fmt.Errorf("TestRequest cannot be nil for CurlHTTPClient.Run")
	}

	actualResp, err := c.Do(server.Address, *req)
	return &actualResp, err
}

// Assertion helper functions

// AssertStatusCode checks if the actual status code matches the expected status code.
func AssertStatusCode(t *testing.T, expected ExpectedResponse, actual ActualResponse) {
	t.Helper()
	if actual.StatusCode != expected.StatusCode {
		t.Errorf("expected status code %d, got %d. Body: %s, Error: %v",
			expected.StatusCode, actual.StatusCode, string(actual.Body), actual.Error)
	}
}

// AssertHeaderEquals checks if a specific header in the actual response has the expected value.
func AssertHeaderEquals(t *testing.T, actualHeaders http.Header, headerName string, expectedValue string) {
	t.Helper()
	actualValue := actualHeaders.Get(headerName)
	if actualValue != expectedValue {
		t.Errorf("expected header '%s' to be '%s', got '%s'", headerName, expectedValue, actualValue)
	}
}

// AssertHeaderContains checks if a specific header in the actual response contains the expected substring.
func AssertHeaderContains(t *testing.T, actualHeaders http.Header, headerName string, expectedSubstring string) {
	t.Helper()
	actualValue := actualHeaders.Get(headerName)
	if !strings.Contains(actualValue, expectedSubstring) {
		t.Errorf("expected header '%s' ('%s') to contain '%s'", headerName, actualValue, expectedSubstring)
	}
}

// AssertBodyEquals checks if the actual response body matches the expected body byte-for-byte.
func AssertBodyEquals(t *testing.T, expectedBody []byte, actualBody []byte) {
	t.Helper()
	if !bytes.Equal(expectedBody, actualBody) {
		t.Errorf("expected body '%s', got '%s'", string(expectedBody), string(actualBody))
	}
}

// AssertBodyContains checks if the actual response body contains the expected substring.
func AssertBodyContains(t *testing.T, expectedSubstring string, actualBody []byte) {
	t.Helper()
	if !strings.Contains(string(actualBody), expectedSubstring) {
		t.Errorf("expected body to contain '%s', got '%s'", expectedSubstring, string(actualBody))
	}
}

// AssertJSONBodyFields checks if the actual JSON response body (provided as bytes) contains the expected fields
// with the specified values. It performs a shallow comparison for map[string]interface{} values.
// For nested objects or more complex structures, a more sophisticated deep comparison or dedicated library might be needed.
func AssertJSONBodyFields(t *testing.T, expectedFields map[string]interface{}, actualBodyJSON []byte) {
	t.Helper()
	var actualData map[string]interface{}
	if err := json.Unmarshal(actualBodyJSON, &actualData); err != nil {
		t.Errorf("failed to unmarshal actual body JSON: %v. Body: '%s'", err, string(actualBodyJSON))
		return
	}

	for key, expectedValue := range expectedFields {
		actualValue, ok := actualData[key]
		if !ok {
			t.Errorf("expected JSON field '%s' not found in response body: %s", key, string(actualBodyJSON))
			continue
		}

		// Basic comparison. For more complex types (slices, nested maps), this might need enhancement.
		// Using Sprintf to handle various types for comparison, though this is not a perfect deep equal.
		expectedValueStr := fmt.Sprintf("%v", expectedValue)
		actualValueStr := fmt.Sprintf("%v", actualValue)

		if expectedValueStr != actualValueStr {
			// Attempt a more direct comparison for common types before relying on string representation
			// This helps with type mismatches like int vs float64 from JSON unmarshalling.
			var match bool
			switch ev := expectedValue.(type) {
			case string:
				if av, ok := actualValue.(string); ok && ev == av {
					match = true
				}
			case float64: // Numbers in JSON often become float64
				if av, ok := actualValue.(float64); ok && ev == av {
					match = true
				}
			case int: // If expected is int, try to compare with float64 if actual is float64
				if av, ok := actualValue.(float64); ok && float64(ev) == av {
					match = true
				} else if avInt, ok := actualValue.(int); ok && ev == avInt {
					match = true
				}
			case bool:
				if av, ok := actualValue.(bool); ok && ev == av {
					match = true
				}
			default:
				// For other types, fall back to string comparison or consider deep equality checks.
				// For simplicity here, if not matched above, it will fail the string comparison below.
			}

			if !match && expectedValueStr != actualValueStr {
				t.Errorf("expected JSON field '%s' to be '%v' (type %T), got '%v' (type %T) in response body: %s",
					key, expectedValue, expectedValue, actualValue, actualValue, string(actualBodyJSON))
			}
		}
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
}

// RunE2ETest executes a defined E2E test scenario.
// It handles server setup, execution of test cases with multiple clients, and assertions.
func RunE2ETest(t *testing.T, def E2ETestDefinition) {
	t.Helper()

	// configuredListenAddress is the address string AS CONFIGURED for the server
	// (e.g., "127.0.0.1:0" or ":0"). This goes into the server's config file.
	// It's NOT pre-resolved to a specific port by the test runner anymore.
	configuredListenAddress := def.ServerListenAddress

	configPath, cleanupConfig, err := WriteTempConfig(def.ServerConfigData, def.ServerConfigFormat)
	if err != nil {
		t.Fatalf("E2E test '%s': Failed to write temp config: %v", def.Name, err)
	}
	// cleanupConfig will be added to server.AddCleanupFunc below

	// StartTestServer now takes configuredListenAddress.
	// It will parse the actual listening port from server logs and store it in server.Address.
	server, err := StartTestServer(def.ServerBinaryPath, configPath, def.ServerConfigArgName, configuredListenAddress, def.ExtraServerArgs...)
	if err != nil {
		// server might be nil here if StartTestServer returns an error early.
		// If server is not nil, SafeGetLogs will try to get logs.
		logStr := ""
		if server != nil {
			logStr = server.SafeGetLogs()
		}
		t.Fatalf("E2E test '%s': Failed to start server: %v. Logs (if available):\n%s", def.Name, err, logStr)
	}
	server.AddCleanupFunc(func() error { cleanupConfig(); return nil }) // Ensure temp config file is removed
	defer server.Stop()

	testRunners := []TestRunner{
		NewGoNetHTTPClient(),
		NewCurlHTTPClient(def.CurlPath), // Uses def.CurlPath, or "curl" if empty
	}

	for i, tc := range def.TestCases {
		caseName := tc.Name
		if caseName == "" {
			caseName = fmt.Sprintf("Case%d_%s", i+1, tc.Request.Path)
		}

		t.Run(caseName, func(st *testing.T) {
			st.Helper()
			for _, runner := range testRunners {

				st.Run(fmt.Sprintf("Client_%s", runner.Type()), func(ct *testing.T) {
					ct.Helper()
					ct.Cleanup(func() {
						if ct.Failed() {
							logs := server.SafeGetLogs()
							ct.Logf("BEGIN Server logs for client %s, case %s (on failure):\n%s\nEND Server logs", runner.Type(), caseName, logs)
						}
					})

					actualResp, execErr := runner.Run(server, &tc.Request)

					if tc.Expected.ErrorContains != "" {
						if execErr == nil {
							ct.Errorf("Expected error containing '%s', but got no error. Response Status: %d, Body: %s",
								tc.Expected.ErrorContains, actualResp.StatusCode, string(actualResp.Body))
						} else if !strings.Contains(execErr.Error(), tc.Expected.ErrorContains) {
							ct.Errorf("Expected error to contain '%s', got: %v", tc.Expected.ErrorContains, execErr)
						}
						return // Test case check ends here if an error was expected
					}

					if execErr != nil {
						ct.Fatalf("Unexpected error during request: %v", execErr) // Server logs will be printed by ct.Cleanup
					}

					// Perform assertions on the actual response
					AssertStatusCode(ct, tc.Expected, *actualResp)

					if tc.Expected.Headers != nil {
						for headerName, expectedValue := range tc.Expected.Headers {
							AssertHeaderEquals(ct, actualResp.Headers, headerName, expectedValue)
						}
					}

					if tc.Expected.ExpectNoBody {
						if len(actualResp.Body) > 0 {
							ct.Errorf("Expected no response body, but got %d bytes: %s", len(actualResp.Body), string(actualResp.Body))
						}
					} else if tc.Expected.BodyMatcher != nil {
						match, desc := tc.Expected.BodyMatcher.Match(actualResp.Body)
						if !match {
							ct.Errorf("Response body mismatch: %s", desc)
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
		return false, fmt.Sprintf("failed to unmarshal actual body JSON: %v. Body: '%s'", err, string(body))
	}

	if !reflect.DeepEqual(m.ExpectedFields, actualData) {
		// For clearer error messages, marshal both expected and actual to formatted JSON.
		expectedJSON, errExp := json.MarshalIndent(m.ExpectedFields, "", "  ")
		if errExp != nil {
			expectedJSON = []byte(fmt.Sprintf("(error marshalling expected fields: %v)", errExp))
		}
		actualJSON, errAct := json.MarshalIndent(actualData, "", "  ")
		if errAct != nil {
			actualJSON = []byte(fmt.Sprintf("(error marshalling actual data: %v)", errAct))
		}
		return false, fmt.Sprintf("JSON body mismatch.\nExpected Fields (as JSON):\n%s\nActual Body (as JSON):\n%s", string(expectedJSON), string(actualJSON))
	}
	return true, ""
}

// waitForServerAddressLog scans the server's log output for the listening address.
// It polls the instance.LogBuffer for new log lines.
func waitForServerAddressLog(instance *ServerInstance, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastParseAttemptError error

	for time.Now().Before(deadline) {
		logs := instance.LogBuffer.String() // Get current snapshot of all logs
		lines := strings.Split(logs, "\n")

		for i := len(lines) - 1; i >= 0; i-- { // Scan from recent lines first
			line := lines[i]
			if line == "" {
				continue
			}

			var logEntry struct {
				Msg     string                 `json:"msg"`
				Level   string                 `json:"level"`
				Details map[string]interface{} `json:"details"`
			}

			if err := json.Unmarshal([]byte(line), &logEntry); err == nil {
				// Check for both possible log messages indicating a listener is active
				isListenerLog := (logEntry.Msg == "Successfully created new listener" || logEntry.Msg == "Successfully created listener from inherited FD") &&
					logEntry.Level == "INFO"

				if isListenerLog {
					if addrRaw, ok := logEntry.Details["localAddr"]; ok { // Changed from "address" to "localAddr"
						if addrStr, okStr := addrRaw.(string); okStr && addrStr != "" {
							// Basic validation that it looks like a host:port
							_, _, errValidate := net.SplitHostPort(addrStr)
							if errValidate == nil {
								return addrStr, nil // Successfully found and parsed
							}
							lastParseAttemptError = fmt.Errorf("parsed address '%s' from log ('%s') is invalid: %w", addrStr, line, errValidate)
						}
					}
				}
			} else {
				// Store the first parsing error of a non-empty line as a hint, but don't spam.
				// Avoid filling 'lastParseAttemptError' with generic unmarshal errors if a valid line is still possible.
				// if lastParseAttemptError == nil && len(line) > 10 { // arbitrary length to avoid tiny junk lines
				// lastParseAttemptError = fmt.Errorf("failed to parse log line: '%s', error: %w", line, err)
				// }
			}
		}

		// Check if server process exited
		if instance.Cmd != nil && instance.Cmd.ProcessState != nil && instance.Cmd.ProcessState.Exited() {
			return "", fmt.Errorf("server process exited (code: %d) while waiting for address log", instance.Cmd.ProcessState.ExitCode())
		}
		if instance.Cmd != nil && instance.cmdCtx != nil { // Check if cmdCtx is available
			select {
			case <-instance.cmdCtx.Done(): // Use the stored context
				exitErrStr := "unknown (context canceled)"
				if instance.Cmd.ProcessState != nil {
					exitErrStr = instance.Cmd.ProcessState.String()
				} else if instance.Cmd.Err != nil { // Access Err field
					exitErrStr = instance.Cmd.Err.Error()
				}
				return "", fmt.Errorf("server process context done (likely exited: %s) while waiting for address log", exitErrStr)
			default:
			}
		}

		time.Sleep(200 * time.Millisecond) // Poll interval
	}

	/* 1050 */
	if lastParseAttemptError != nil {
		return "", fmt.Errorf("timeout waiting for server address log. Last error during parsing attempt: %w. Full logs:\n%s", lastParseAttemptError, instance.SafeGetLogs())
	}
	return "", fmt.Errorf("timeout waiting for server address log (no listener success log line found or parsed correctly from available logs). Full logs:\n%s", instance.SafeGetLogs())
}

// SafeGetLogs returns the content of the LogBuffer, or a placeholder if the instance/buffer is nil.
func (s *ServerInstance) SafeGetLogs() string {
	if s == nil {
		return "<ServerInstance is nil>"
	}
	if s.LogBuffer == nil {
		return "<ServerInstance.LogBuffer is nil>"
	}
	// Ensure access to LogBuffer is safe if it's mutated concurrently, though current design copies it.
	// For String(), it's usually safe as it typically copies internal buffer.
	return s.LogBuffer.String()
}

func StartingPollingAndPrintingBuffer(buffer *bytes.Buffer) {
	if buffer == nil {
		// It's good practice to handle nil inputs, though the prompt implies
		// a valid buffer will be given.
		fmt.Fprintln(os.Stderr, "Error: pollAndPrintBuffer received a nil buffer")
		return
	}

	go func() {
		const pollingRate = 50 * time.Millisecond // Hard-coded polling rate

		for {
			// Check if there's anything to read in the buffer.
			// buffer.Len() gives the number of unread bytes.
			if buffer.Len() > 0 {
				// buffer.WriteTo reads all unread bytes from the buffer
				// and writes them to the provided writer (os.Stdout).
				// This also advances the buffer's internal read pointer,
				// effectively "consuming" the data from the buffer's perspective.
				_, err := buffer.WriteTo(os.Stdout)
				if err != nil {
					// Handle potential errors writing to stdout (e.g., pipe closed).
					// For this example, we'll print to stderr and continue polling.
					// In a real application, you might want more robust error handling
					// or a way to stop the goroutine if stdout is permanently broken.
					fmt.Fprintf(os.Stderr, "Error writing to stdout: %v\n", err)
				}
			}

			// Wait for the next poll interval.
			time.Sleep(pollingRate)
		}
	}()
}

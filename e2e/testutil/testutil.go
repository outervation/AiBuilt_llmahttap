package testutil

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	// "path/filepath" // Removed unused import
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"example.com/llmahttap/v2/internal/config"
	"github.com/BurntSushi/toml"
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
// It waits for the server to become responsive by checking if its listening port is open.
// `serverBinaryPath` must be the path to the server binary.
// `configFile` is the path to the configuration file.
// `configArgName` is the command-line flag name used to pass the config file path (e.g., "-config").
// `serverListenAddress` is the address the server is expected to listen on (e.g., "127.0.0.1:8080").
func StartTestServer(serverBinaryPath string, configFile string, configArgName string, serverListenAddress string, extraArgs ...string) (*ServerInstance, error) {
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
	if serverListenAddress == "" {
		return nil, fmt.Errorf("serverListenAddress cannot be empty (e.g. 127.0.0.1:8080)")
	}

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
		Address:       serverListenAddress,
		LogBuffer:     logBuffer,
		logPipeReader: pipeReader,
		logPipeWriter: pipeWriter,
		logCopyDone:   make(chan struct{}),
		cancelCtx:     cancel,
		CleanupFuncs:  make([]func() error, 0),
	}

	// Cleanup for log pipes
	instance.AddCleanupFunc(func() error {
		var errs []string
		if instance.logPipeWriter != nil {
			if err := instance.logPipeWriter.Close(); err != nil {
				// This error can happen if reader is already closed, often not critical here
				// errs = append(errs, fmt.Sprintf("logPipeWriter.Close: %v", err))
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

	go func() {
		defer close(instance.logCopyDone)
		// Using TeeReader to also print to os.Stdout for live test debugging if needed
		// but primarily capture to LogBuffer
		// For now, just LogBuffer:
		io.Copy(instance.LogBuffer, instance.logPipeReader)
	}()

	if err := cmd.Start(); err != nil {
		// Ensure cleanup functions (like log pipe closure) are run if Start fails
		instance.Stop()
		return nil, fmt.Errorf("failed to start server process '%s': %w. Logs captured:\n%s", serverBinaryPath, err, instance.LogBuffer.String())
	}

	// Wait for server to be ready
	_, _, err = net.SplitHostPort(serverListenAddress)
	if err != nil {
		instance.Stop()
		return nil, fmt.Errorf("invalid serverListenAddress format '%s': %w. Logs captured:\n%s", serverListenAddress, err, instance.LogBuffer.String())
	}
	// Port check is done with serverListenAddress directly

	readyTimeout := 10 * time.Second
	pollInterval := 200 * time.Millisecond
	startTime := time.Now()

	var lastDialErr error
	for {
		if time.Since(startTime) > readyTimeout {
			instance.Stop()
			return nil, fmt.Errorf("server not ready at %s after %v. Last dial error: %v. Logs captured:\n%s", serverListenAddress, readyTimeout, lastDialErr, instance.LogBuffer.String())
		}

		conn, dialErr := net.DialTimeout("tcp", serverListenAddress, pollInterval)
		lastDialErr = dialErr
		if dialErr == nil {
			conn.Close()
			break // Server is ready
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
	finalArgs = append(finalArgs, "--silent")
	finalArgs = append(finalArgs, "--show-error")
	finalArgs = append(finalArgs, "-X", request.Method)

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

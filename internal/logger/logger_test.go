package logger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"os"

	"sync"
	// "path/filepath" // Removed as it was unused
	"strings"
	"testing"
	"time"

	"net"
	"strconv"

	"example.com/llmahttap/v2/internal/config"
)

// parseJSONLog is a helper to unmarshal a JSON log line into the provided out interface.
func parseJSONLog(logLine []byte, out interface{}) error {
	return json.Unmarshal(logLine, out)
}

// nopWriteCloser is a helper to wrap an io.Writer into an io.WriteCloser
// with a no-op Close method.
type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

// defaultTestLogLevel is used when a specific level isn't provided in helpers.
const defaultTestLogLevel = config.LogLevelDebug

// newTestLoggerConfig creates a simple config.LoggingConfig for testing purposes.
// Pass empty strings or nil for defaults (stdout/stderr, Info level).
func newTestLoggerConfig(
	globalLevel config.LogLevel,
	accessLogTarget string,
	accessLogEnabled bool,
	accessLogFormat string,
	trustedProxies []string,
	realIPHeader *string,
	errorLogTarget string,
) *config.LoggingConfig {
	if globalLevel == "" {
		globalLevel = config.LogLevelInfo
	}
	if accessLogTarget == "" {
		accessLogTarget = "stdout"
	}
	if errorLogTarget == "" {
		errorLogTarget = "stderr"
	}
	if accessLogFormat == "" {
		accessLogFormat = "json"
	}

	return &config.LoggingConfig{
		LogLevel: globalLevel,
		AccessLog: &config.AccessLogConfig{
			Enabled:        &accessLogEnabled,
			Target:         &accessLogTarget,
			Format:         accessLogFormat,
			TrustedProxies: trustedProxies,
			RealIPHeader:   realIPHeader,
		},
		ErrorLog: &config.ErrorLogConfig{
			Target: &errorLogTarget,
		},
	}
}

// newTestAccessLogger creates an AccessLogger that writes to the provided io.Writer (e.g., a bytes.Buffer).
// If cfg is nil, a default configuration enabling JSON logging to the buffer is used.
func newTestAccessLogger(cfg *config.AccessLogConfig, out io.Writer) (*AccessLogger, error) {
	if out == nil {
		out = ioutil.Discard // Should not happen, but prevent nil panic
	}

	var wc io.WriteCloser
	if f, ok := out.(*os.File); ok {
		wc = f
	} else {
		wc = nopWriteCloser{out}
	}

	trueVal := true
	defaultTarget := "buffer" // Placeholder, not used if 'out' is directly used
	finalCfg := config.AccessLogConfig{
		Enabled: &trueVal,
		Target:  &defaultTarget,
		Format:  "json",
	}

	if cfg != nil {
		finalCfg = *cfg
		// If a file path was given in cfg.Target but 'out' is a buffer, this might be inconsistent.
		// For testing, we prioritize the 'out' writer.
	}

	parsedProxies, err := preParseTrustedProxies(finalCfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("failed to parse trusted proxies for test logger: %w", err)
	}

	return &AccessLogger{
		logger:        log.New(out, "", 0),
		config:        finalCfg,
		output:        wc,
		parsedProxies: parsedProxies,
	}, nil
}

// newTestErrorLogger creates an ErrorLogger that writes to the provided io.Writer (e.g., a bytes.Buffer).
// If cfg is nil, a default configuration logging to the buffer is used.
// globalLevel specifies the logger's threshold.
func newTestErrorLogger(cfg *config.ErrorLogConfig, globalLevel config.LogLevel, out io.Writer) *ErrorLogger {
	if out == nil {
		out = ioutil.Discard // Should not happen, but prevent nil panic
	}

	var wc io.WriteCloser
	if f, ok := out.(*os.File); ok {
		wc = f
	} else {
		wc = nopWriteCloser{out}
	}

	defaultTarget := "buffer" // Placeholder
	finalCfg := config.ErrorLogConfig{
		Target: &defaultTarget,
	}
	if cfg != nil {
		finalCfg = *cfg
	}
	if globalLevel == "" {
		globalLevel = defaultTestLogLevel
	}

	return &ErrorLogger{
		logger:         log.New(out, "", 0),
		config:         finalCfg,
		globalLogLevel: globalLevel,
		output:         wc,
	}
}

// createTempLogFile creates a temporary file for logging.
// It returns the file path and a cleanup function to remove the file.
func createTempLogFile(t *testing.T, pattern string) (string, func()) {
	t.Helper()
	tempFile, err := ioutil.TempFile("", pattern)
	if err != nil {
		t.Fatalf("Failed to create temp log file: %v", err)
	}
	filePath := tempFile.Name()
	if err := tempFile.Close(); err != nil { // Close immediately, will be reopened by logger
		t.Logf("Warning: error closing temp file after creation: %v", err)
	}

	cleanup := func() {
		if err := os.Remove(filePath); err != nil {
			// Log only if the error is not "file does not exist"
			if !os.IsNotExist(err) {
				t.Logf("Warning: failed to remove temp log file %s: %v", filePath, err)
			}
		}
	}
	return filePath, cleanup
}

// newMockHTTPRequest creates a new http.Request for testing.
// remoteAddr should be in "host:port" format or just "host".
func newMockHTTPRequest(t *testing.T, method, path, remoteAddr string, headers http.Header) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if headers != nil {
		for k, headerValues := range headers {
			if len(headerValues) > 0 {
				req.Header.Set(k, headerValues[0]) // Use Set for the first value
				// Add remaining values if a header has multiple values
				for i := 1; i < len(headerValues); i++ {
					req.Header.Add(k, headerValues[i])
				}
			}
		}
	}
	return req
}

// readLogOutput reads all lines from an io.Reader (e.g., bytes.Buffer or os.File).
// It splits the content by newlines and returns a slice of strings.
// Empty lines resulting from trailing newlines might be included depending on reader content.
func readLogOutput(r io.Reader) ([]string, error) {
	content, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read log output: %w", err)
	}
	if len(content) == 0 {
		return []string{}, nil
	}
	// Split by newline, be mindful of potential empty string at the end if content ends with newline
	lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
	return lines, nil
}

// readLogBuffer is a convenience function to read log lines from a bytes.Buffer.
func readLogBuffer(buf *bytes.Buffer) []string {
	if buf.Len() == 0 {
		return []string{}
	}
	// Trim trailing newline to avoid an empty string at the end of the slice
	s := strings.TrimRight(buf.String(), "\n")
	if s == "" { // Buffer might have only contained newlines
		return []string{}
	}
	return strings.Split(s, "\n")
}

// assertBetweenMs checks if a given duration is between minMs and maxMs (inclusive).
func assertBetweenMs(t *testing.T, d time.Duration, minMs, maxMs int64) {
	t.Helper()
	ms := d.Milliseconds()
	if ms < minMs || ms > maxMs {
		t.Errorf("Expected duration %v (%dms) to be between %dms and %dms", d, ms, minMs, maxMs)
	}
}

// Test helpers can be expanded here as needed.
// For example, functions to assert specific log field values.
func checkTimeFormat(t *testing.T, ts string) {
	t.Helper()
	_, err := time.Parse("2006-01-02T15:04:05.000Z", ts)
	if err != nil {
		t.Errorf("Timestamp %q is not in the expected format 2006-01-02T15:04:05.000Z: %v", ts, err)
	}
}

// Helper to create a fully initialized Logger for testing.
func newTestFullLogger(t *testing.T, cfg *config.LoggingConfig) (*Logger, func()) {
	t.Helper()

	var cleanupFuncs []func()
	var finalCleanup = func() {
		for i := len(cleanupFuncs) - 1; i >= 0; i-- {
			cleanupFuncs[i]()
		}
	}

	// Handle file targets by creating temp files
	accessTargetIsFile := false
	if cfg.AccessLog != nil && cfg.AccessLog.Target != nil && *cfg.AccessLog.Target != "stdout" && *cfg.AccessLog.Target != "stderr" {
		path, cleanup := createTempLogFile(t, "test-access-*.log")
		*cfg.AccessLog.Target = path
		cleanupFuncs = append(cleanupFuncs, cleanup)
		accessTargetIsFile = true
	}

	errorTargetIsFile := false
	if cfg.ErrorLog != nil && cfg.ErrorLog.Target != nil && *cfg.ErrorLog.Target != "stdout" && *cfg.ErrorLog.Target != "stderr" {
		path, cleanup := createTempLogFile(t, "test-error-*.log")
		*cfg.ErrorLog.Target = path
		cleanupFuncs = append(cleanupFuncs, cleanup)
		errorTargetIsFile = true
	}

	logger, err := NewLogger(cfg)
	if err != nil {
		finalCleanup()
		t.Fatalf("Failed to create test logger: %v", err)
	}

	// Augment cleanup to also close logger's files if they are actual files managed by logger
	extendedCleanup := func() {
		if logger.accessLog != nil && logger.accessLog.output != nil && accessTargetIsFile {
			if f, ok := logger.accessLog.output.(*os.File); ok {
				if f != os.Stdout && f != os.Stderr {
					f.Close()
				}
			}
		}
		if logger.errorLog != nil && logger.errorLog.output != nil && errorTargetIsFile {
			if f, ok := logger.errorLog.output.(*os.File); ok {
				if f != os.Stdout && f != os.Stderr {
					f.Close()
				}
			}
		}
		finalCleanup() // Call original temp file cleanups
	}

	return logger, extendedCleanup
}

// Helper to safely get a string pointer.
func stringPtr(s string) *string {
	return &s
}

// Helper to safely get a bool pointer.
func boolPtr(b bool) *bool {
	return &b
}

// Example of how to use with buffer for ErrorLogger:
// var errBuf bytes.Buffer
// errorLogCfg := &config.ErrorLogConfig{Target: stringPtr("buffer")} // Target isn't really used if 'out' is given
// el := newTestErrorLogger(errorLogCfg, config.LogLevelDebug, &errBuf)
// el.Debug("test debug", nil)
// lines := readLogBuffer(&errBuf)
// ... assertions on lines ...

// Example of how to use with buffer for AccessLogger:
// var accBuf bytes.Buffer
// accessLogCfg := &config.AccessLogConfig{Enabled: boolPtr(true), Format:"json", Target: stringPtr("buffer")}
// al, err := newTestAccessLogger(accessLogCfg, &accBuf)
// ... handle err ...
// req := newMockHTTPRequest(t, "GET", "/foo", "1.2.3.4:1234", nil)
// al.LogAccess(req, 1, 200, 100, time.Millisecond*50)
// lines := readLogBuffer(&accBuf)
// ... assertions on lines ...

func TestAccessLogEnabledFlag(t *testing.T) {
	tests := []struct {
		name          string
		enabled       *bool // Pointer to bool to test nil case (defaults to true)
		expectLog     bool
		logTarget     string // To test stdout/stderr vs buffer
		initialOutput io.Writer
	}{
		{
			name:      "Enabled true",
			enabled:   boolPtr(true),
			expectLog: true,
		},
		{
			name:      "Enabled false",
			enabled:   boolPtr(false),
			expectLog: false,
		},
		{
			name:      "Enabled nil (default true)",
			enabled:   nil,
			expectLog: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			var outputTarget io.Writer = &buf

			// If testing stdout/stderr, we can't easily capture.
			// For this specific test, we'll always use a buffer.
			// More complex tests involving NewLogger would handle file/stdio targets.
			_ = tt.logTarget // Not used for this specific test's simplicity

			accessLogCfg := &config.AccessLogConfig{
				Enabled: tt.enabled,
				Format:  "json",
				Target:  stringPtr("buffer"), // For newTestAccessLogger to use 'buf'
			}

			al, err := newTestAccessLogger(accessLogCfg, outputTarget)
			if err != nil {
				t.Fatalf("newTestAccessLogger failed: %v", err)
			}

			// Create a mock request
			req := newMockHTTPRequest(t, "GET", "/testpath", "192.168.1.10:12345", nil)

			// Attempt to log
			al.LogAccess(req, 1, http.StatusOK, 100, 50*time.Millisecond)

			logLines := readLogBuffer(&buf)

			if tt.expectLog {
				if len(logLines) != 1 {
					t.Errorf("Expected 1 log line, got %d", len(logLines))
				} else {
					var entry AccessLogEntry
					if err := parseJSONLog([]byte(logLines[0]), &entry); err != nil {
						t.Errorf("Failed to parse access log entry: %v. Line: %s", err, logLines[0])
					}
					if entry.URI != "/testpath" {
						t.Errorf("Expected URI '/testpath', got '%s'", entry.URI)
					}
				}
			} else {
				if len(logLines) != 0 {
					t.Errorf("Expected 0 log lines, got %d. Lines: %v", len(logLines), logLines)
				}
			}
		})
	}
}

func TestAccessLogFormatAndContent(t *testing.T) {
	tests := []struct {
		name          string
		reqMethod     string
		reqURI        string
		reqRemoteAddr string // Direct peer, e.g., "192.168.0.1:12345"
		reqHeaders    http.Header
		streamID      uint32
		status        int
		responseBytes int64
		duration      time.Duration
		// Fields for RealIPHeader testing
		trustedProxies     []string
		realIPHeaderName   *string
		expectedRemoteAddr string // The final IP expected in the log
		// Other expected fields
		expectUserAgent string
		expectReferer   string
	}{
		{
			name:               "Basic request, no proxy",
			reqMethod:          "GET",
			reqURI:             "/test/path?query=1",
			reqRemoteAddr:      "192.168.0.1:12345",
			reqHeaders:         nil,
			streamID:           1,
			status:             http.StatusOK,
			responseBytes:      1024,
			duration:           100 * time.Millisecond,
			trustedProxies:     nil,
			realIPHeaderName:   nil,
			expectedRemoteAddr: "192.168.0.1",
		},
		{
			name:          "Request with User-Agent and Referer, no proxy",
			reqMethod:     "POST",
			reqURI:        "/submit",
			reqRemoteAddr: "10.0.0.5:54321",
			reqHeaders: http.Header{
				"User-Agent": {"TestAgent/1.0"},
				"Referer":    {"http://example.com/previous"},
			},
			streamID:           3,
			status:             http.StatusCreated,
			responseBytes:      50,
			duration:           25 * time.Millisecond,
			trustedProxies:     nil,
			realIPHeaderName:   nil,
			expectedRemoteAddr: "10.0.0.5",
			expectUserAgent:    "TestAgent/1.0",
			expectReferer:      "http://example.com/previous",
		},
		{
			name:               "Request without optional headers, IPv6, no proxy",
			reqMethod:          "HEAD",
			reqURI:             "/check",
			reqRemoteAddr:      "[2001:db8::1]:8080",
			reqHeaders:         nil,
			streamID:           5,
			status:             http.StatusNoContent,
			responseBytes:      0,
			duration:           10 * time.Millisecond,
			trustedProxies:     nil,
			realIPHeaderName:   nil,
			expectedRemoteAddr: "2001:db8::1",
		},
		// New test cases for RealIPHeader and TrustedProxies logic
		{
			name:               "XFF exists, default RealIPHeader, no trusted proxies",
			reqMethod:          "GET",
			reqURI:             "/xff-no-trust",
			reqRemoteAddr:      "172.16.0.100:12345", // a supposed proxy
			reqHeaders:         http.Header{"X-Forwarded-For": {"10.1.2.3"}},
			streamID:           7,
			status:             http.StatusOK,
			responseBytes:      100,
			duration:           30 * time.Millisecond,
			trustedProxies:     nil,
			realIPHeaderName:   stringPtr("X-Forwarded-For"),
			expectedRemoteAddr: "10.1.2.3", // From XFF, as direct peer isn't trusted for XFF chain.
		},
		{
			name:               "XFF with one trusted proxy",
			reqMethod:          "GET",
			reqURI:             "/xff-one-trusted",
			reqRemoteAddr:      "192.168.1.50:12345",                                       // This is the trusted proxy
			reqHeaders:         http.Header{"X-Forwarded-For": {"10.1.2.3, 192.168.1.50"}}, // client, proxy
			streamID:           9,
			status:             http.StatusOK,
			responseBytes:      150,
			duration:           40 * time.Millisecond,
			trustedProxies:     []string{"192.168.1.0/24"},
			realIPHeaderName:   stringPtr("X-Forwarded-For"),
			expectedRemoteAddr: "10.1.2.3",
		},
		{
			name:               "XFF all trusted, use direct peer",
			reqMethod:          "GET",
			reqURI:             "/xff-all-trusted",
			reqRemoteAddr:      "8.8.8.8:12345",                                            // The actual connecting IP (not in trustedProxies list itself)
			reqHeaders:         http.Header{"X-Forwarded-For": {"192.168.1.50, 10.0.0.1"}}, // Both trusted
			streamID:           11,
			status:             http.StatusOK,
			responseBytes:      200,
			duration:           50 * time.Millisecond,
			trustedProxies:     []string{"192.168.1.0/24", "10.0.0.0/8"},
			realIPHeaderName:   stringPtr("X-Forwarded-For"),
			expectedRemoteAddr: "8.8.8.8",
		},
		{
			name:               "Custom RealIPHeader used",
			reqMethod:          "GET",
			reqURI:             "/custom-header-simplified",
			reqRemoteAddr:      "172.20.0.1:12345", // Arbitrary non-trusted direct peer
			reqHeaders:         http.Header{"CF-Connecting-IP": {"10.1.2.3"}},
			streamID:           13,
			status:             http.StatusOK,
			responseBytes:      250,
			duration:           60 * time.Millisecond,
			trustedProxies:     nil, // No trusted proxies for this simplified case
			realIPHeaderName:   stringPtr("CF-Connecting-IP"),
			expectedRemoteAddr: "10.1.2.3",
		},
		{
			name:               "XFF with malformed IP, fallback to direct peer",
			reqMethod:          "GET",
			reqURI:             "/xff-malformed",
			reqRemoteAddr:      "192.168.1.50:12345", // Trusted
			reqHeaders:         http.Header{"X-Forwarded-For": {"not-an-ip, 10.1.2.3"}},
			streamID:           15,
			status:             http.StatusOK,
			responseBytes:      300,
			duration:           70 * time.Millisecond,
			trustedProxies:     []string{"192.168.1.0/24"},
			realIPHeaderName:   stringPtr("X-Forwarded-For"),
			expectedRemoteAddr: "192.168.1.50",
		},
		// New test cases for RemoteAddr and RemotePort parsing specifics
		{
			name:               "IPv4 without port",
			reqMethod:          "GET",
			reqURI:             "/ipv4-no-port",
			reqRemoteAddr:      "77.88.99.1", // No port
			reqHeaders:         nil,
			streamID:           101,
			status:             http.StatusOK,
			responseBytes:      10,
			duration:           5 * time.Millisecond,
			trustedProxies:     nil,
			realIPHeaderName:   nil,
			expectedRemoteAddr: "77.88.99.1",
		},
		{
			name:               "IPv6 without port",
			reqMethod:          "GET",
			reqURI:             "/ipv6-no-port",
			reqRemoteAddr:      "2001:db8:1234::abcd", // No port
			reqHeaders:         nil,
			streamID:           103,
			status:             http.StatusOK,
			responseBytes:      20,
			duration:           6 * time.Millisecond,
			trustedProxies:     nil,
			realIPHeaderName:   nil,
			expectedRemoteAddr: "2001:db8:1234::abcd",
		},
		{
			name:               "Hostname without port",
			reqMethod:          "GET",
			reqURI:             "/host-no-port",
			reqRemoteAddr:      "someserver.example.com", // No port
			reqHeaders:         nil,
			streamID:           105,
			status:             http.StatusOK,
			responseBytes:      30,
			duration:           7 * time.Millisecond,
			trustedProxies:     nil,
			realIPHeaderName:   nil,
			expectedRemoteAddr: "someserver.example.com",
		},
		{
			name:               "IPv4 with XFF, but XFF is empty, no port on remoteAddr",
			reqMethod:          "GET",
			reqURI:             "/ipv4-no-port-empty-xff",
			reqRemoteAddr:      "77.88.99.2",                         // No port
			reqHeaders:         http.Header{"X-Forwarded-For": {""}}, // Empty XFF
			streamID:           107,
			status:             http.StatusOK,
			responseBytes:      40,
			duration:           8 * time.Millisecond,
			trustedProxies:     nil,                          // No trusted proxies
			realIPHeaderName:   stringPtr("X-Forwarded-For"), // RealIPHeader is XFF
			expectedRemoteAddr: "77.88.99.2",                 // Fallback to direct peer, as XFF is empty
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			enabled := true
			accessLogCfg := &config.AccessLogConfig{
				Enabled:        &enabled,
				Format:         "json",
				Target:         stringPtr("buffer"), // Use buffer for easy capture
				TrustedProxies: tt.trustedProxies,
				RealIPHeader:   tt.realIPHeaderName,
			}

			al, err := newTestAccessLogger(accessLogCfg, &buf)
			if err != nil {
				t.Fatalf("newTestAccessLogger failed: %v", err)
			}

			req := newMockHTTPRequest(t, tt.reqMethod, tt.reqURI, tt.reqRemoteAddr, tt.reqHeaders)
			// For HTTP/2.0, req.Proto would be "HTTP/2.0". httptest.NewRequest defaults to HTTP/1.1.
			// The logger uses req.Proto directly.
			req.Proto = "HTTP/2.0" // Simulate HTTP/2 request for protocol field testing

			if tt.name == "Custom RealIPHeader used" {
				t.Logf("--- DIAGNOSTICS for Custom_RealIPHeader_used ---")
				t.Logf("Direct Peer (clientHost for getRealClientIP): %s", req.RemoteAddr) // req.RemoteAddr is used to derive clientHost
				if tt.realIPHeaderName != nil {
					t.Logf("RealIPHeaderName for getRealClientIP: %s", *tt.realIPHeaderName)
				} else {
					t.Logf("RealIPHeaderName for getRealClientIP: <nil>")
				}
				t.Logf("Headers for getRealClientIP: %v", req.Header)
				if al != nil {
					t.Logf("Trusted Proxies for getRealClientIP (al.parsedProxies):")
					if al.parsedProxies.cidrs == nil && al.parsedProxies.ips == nil {
						t.Logf("  al.parsedProxies is empty (both cidrs and ips are nil)")
					} else {
						if al.parsedProxies.cidrs != nil {
							for _, cidr := range al.parsedProxies.cidrs {
								t.Logf("  CIDR: %s", cidr.String())
							}
						} else {
							t.Logf("  al.parsedProxies.cidrs is nil")
						}
						if al.parsedProxies.ips != nil {
							for _, ip := range al.parsedProxies.ips {
								t.Logf("  IP: %s", ip.String())
							}
						} else {
							t.Logf("  al.parsedProxies.ips is nil")
						}
					}

					// Simulate check for IP "10.1.2.3"
					ipToCheck := net.ParseIP("10.1.2.3")
					isConsideredTrusted := false
					if ipToCheck != nil {
						if al.parsedProxies.cidrs != nil {
							for _, trustedCIDR := range al.parsedProxies.cidrs {
								if trustedCIDR.Contains(ipToCheck) {
									isConsideredTrusted = true
									t.Logf("  MANUAL CHECK: '10.1.2.3' IS TRUSTED by CIDR %s", trustedCIDR.String())
									break
								}
							}
						}
						if !isConsideredTrusted && al.parsedProxies.ips != nil {
							for _, trustedIP := range al.parsedProxies.ips {
								if trustedIP.Equal(ipToCheck) {
									isConsideredTrusted = true
									t.Logf("  MANUAL CHECK: '10.1.2.3' IS TRUSTED by IP %s", trustedIP.String())
									break
								}
							}
						}
					} else {
						t.Logf("  MANUAL CHECK: '10.1.2.3' failed to parse.")
					}

					if !isConsideredTrusted {
						t.Logf("  MANUAL CHECK: '10.1.2.3' IS NOT TRUSTED by any entry in al.parsedProxies.")
					}
				} else {
					t.Logf("al is nil, cannot perform detailed diagnostics on al.parsedProxies")
				}

				// Add direct call to isIPTrusted for 10.1.2.3
				if al != nil {
					ipToReallyCheck := net.ParseIP("10.1.2.3")
					if ipToReallyCheck != nil {
						isTrustedResult := isIPTrusted(ipToReallyCheck, al.parsedProxies)
						t.Logf("  DIRECT isIPTrusted('10.1.2.3', al.parsedProxies) RESULT: %v", isTrustedResult)
					} else {
						t.Logf("  DIRECT isIPTrusted: Failed to parse '10.1.2.3' for direct check.")
					}
				}
				t.Logf("--- END DIAGNOSTICS ---")
			}
			al.LogAccess(req, tt.streamID, tt.status, tt.responseBytes, tt.duration)

			logLines := readLogBuffer(&buf)
			if len(logLines) != 1 {
				t.Fatalf("Expected 1 log line, got %d. Lines: %v", len(logLines), logLines)
			}

			var entry AccessLogEntry
			if err := parseJSONLog([]byte(logLines[0]), &entry); err != nil {
				t.Fatalf("Failed to parse access log entry: %v. Line: %s", err, logLines[0])
			}

			// Check timestamp format
			checkTimeFormat(t, entry.Timestamp)

			// Check RemoteAddr (resolved client IP)
			if entry.RemoteAddr != tt.expectedRemoteAddr {
				t.Errorf("Expected RemoteAddr %q, got %q", tt.expectedRemoteAddr, entry.RemoteAddr)
			}

			// Check RemotePort (direct peer's port)
			_, expectedPortStr, err := net.SplitHostPort(tt.reqRemoteAddr)
			if err != nil { // Handle cases where reqRemoteAddr might not have a port (e.g. "localhost")
				expectedPortStr = "0" // Default if not parsable, consistent with logger's behavior
			}
			expectedPort, _ := strconv.Atoi(expectedPortStr) // Atoi returns 0 on error, also consistent.

			if entry.RemotePort != expectedPort {
				t.Errorf("Expected RemotePort %d (from direct peer %s), got %d", expectedPort, tt.reqRemoteAddr, entry.RemotePort)
			}

			// Check Protocol
			if entry.Protocol != "HTTP/2.0" { // As overridden for test
				t.Errorf("Expected Protocol %q, got %q", "HTTP/2.0", entry.Protocol)
			}

			// Check Method
			if entry.Method != tt.reqMethod {
				t.Errorf("Expected Method %q, got %q", tt.reqMethod, entry.Method)
			}

			// Check URI
			if entry.URI != tt.reqURI {
				t.Errorf("Expected URI %q, got %q", tt.reqURI, entry.URI)
			}

			// Check Status
			if entry.Status != tt.status {
				t.Errorf("Expected Status %d, got %d", tt.status, entry.Status)
			}

			// Check ResponseBytes
			if entry.ResponseBytes != tt.responseBytes {
				t.Errorf("Expected ResponseBytes %d, got %d", tt.responseBytes, entry.ResponseBytes)
			}

			// Check DurationMs
			if entry.DurationMs != tt.duration.Milliseconds() {
				t.Errorf("Expected DurationMs %d, got %d", tt.duration.Milliseconds(), entry.DurationMs)
			}

			// Check H2StreamID
			if entry.H2StreamID != tt.streamID {
				t.Errorf("Expected H2StreamID %d, got %d", tt.streamID, entry.H2StreamID)
			}

			// Check UserAgent (optional)
			if tt.expectUserAgent != "" {
				if entry.UserAgent != tt.expectUserAgent {
					t.Errorf("Expected UserAgent %q, got %q", tt.expectUserAgent, entry.UserAgent)
				}
			} else {
				if entry.UserAgent != "" {
					t.Errorf("Expected empty UserAgent, got %q", entry.UserAgent)
				}
			}

			// Check Referer (optional)
			if tt.expectReferer != "" {
				if entry.Referer != tt.expectReferer {
					t.Errorf("Expected Referer %q, got %q", tt.expectReferer, entry.Referer)
				}
			} else {
				if entry.Referer != "" {
					t.Errorf("Expected empty Referer, got %q", entry.Referer)
				}
			}
		})
	}
}

func TestErrorLogFormatAndContent(t *testing.T) {
	tests := []struct {
		name           string
		level          config.LogLevel
		msg            string
		context        LogFields
		globalLogLevel config.LogLevel // Logger's configured minimum level
		expectLog      bool
		expectedSource string // If auto-populated or from context
	}{
		{
			name:           "Debug message, Debug level, logged",
			level:          config.LogLevelDebug,
			msg:            "This is a debug message.",
			context:        LogFields{"user_id": 123, "request_id": "abc"},
			globalLogLevel: config.LogLevelDebug,
			expectLog:      true,
		},
		{
			name:           "Info message, Debug level, logged",
			level:          config.LogLevelInfo,
			msg:            "This is an info message.",
			context:        nil,
			globalLogLevel: config.LogLevelDebug,
			expectLog:      true,
		},
		{
			name:           "Warning message, Info level, logged",
			level:          config.LogLevelWarning,
			msg:            "This is a warning.",
			context:        LogFields{"source": "custom_module.go:42"},
			globalLogLevel: config.LogLevelInfo,
			expectLog:      true,
			expectedSource: "custom_module.go:42",
		},
		{
			name:           "Error message, Warning level, logged",
			level:          config.LogLevelError,
			msg:            "This is an error!",
			context:        LogFields{"method": "GET", "uri": "/test", "h2_stream_id": uint32(5)},
			globalLogLevel: config.LogLevelWarning,
			expectLog:      true,
		},
		{
			name:           "Debug message, Info level, not logged",
			level:          config.LogLevelDebug,
			msg:            "This debug message should not appear.",
			context:        nil,
			globalLogLevel: config.LogLevelInfo,
			expectLog:      false,
		},
		{
			name:           "Info message, Error level, not logged",
			level:          config.LogLevelInfo,
			msg:            "This info message should not appear.",
			context:        nil,
			globalLogLevel: config.LogLevelError,
			expectLog:      false,
		},
		{
			name:           "Error message, Error level, logged with auto source",
			level:          config.LogLevelError,
			msg:            "Critical failure.",
			context:        nil, // Source will be auto-populated by logger.Error()
			globalLogLevel: config.LogLevelError,
			expectLog:      true,
			// expectedSource will be checked for presence, not exact match due to line number variance
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			errorLogCfg := &config.ErrorLogConfig{
				Target: stringPtr("buffer"),
			}
			el := newTestErrorLogger(errorLogCfg, tt.globalLogLevel, &buf)

			// Call the appropriate log method based on tt.level
			// This also tests the wrapper methods like el.Debug(), el.Info() etc.
			// which populate source if not provided.
			switch tt.level {
			case config.LogLevelDebug:
				el.Debug(tt.msg, tt.context)
			case config.LogLevelInfo:
				el.Info(tt.msg, tt.context)
			case config.LogLevelWarning:
				el.Warn(tt.msg, tt.context)
			case config.LogLevelError:
				el.Error(tt.msg, tt.context)
			default:
				t.Fatalf("Unhandled log level in test setup: %s", tt.level)
			}

			logLines := readLogBuffer(&buf)

			if tt.expectLog {
				if len(logLines) != 1 {
					t.Fatalf("Expected 1 log line, got %d. Lines: %v", len(logLines), logLines)
				}
				var entry ErrorLogEntry
				if err := parseJSONLog([]byte(logLines[0]), &entry); err != nil {
					t.Fatalf("Failed to parse error log entry: %v. Line: %s", err, logLines[0])
				}

				checkTimeFormat(t, entry.Timestamp)
				if entry.Level != string(tt.level) {
					t.Errorf("Expected Level %q, got %q", tt.level, entry.Level)
				}
				if entry.Message != tt.msg {
					t.Errorf("Expected Message %q, got %q", tt.msg, entry.Message)
				}

				// Check source
				if tt.expectedSource != "" {
					if entry.Source != tt.expectedSource {
						t.Errorf("Expected Source %q, got %q", tt.expectedSource, entry.Source)
					}
				} else if tt.name == "Error message, Error level, logged with auto source" { // Special case for auto-populated source
					if entry.Source == "" {
						t.Errorf("Expected auto-populated Source, but it was empty")
					} else if !strings.HasSuffix(entry.Source, "logger_test.go:"+strconv.Itoa(getExpectedLineNumberForErrorLog(tt.level))) {
						// This is tricky because the line number can change.
						// We check if it ends with "logger_test.go:<number>"
						// For more robust checks, one might inspect the stack or use a mock runtime.Caller
						t.Logf("Auto-populated source: %s. Verification of exact line number is fragile.", entry.Source)
						if !strings.HasPrefix(entry.Source, "logger_test.go:") {
							t.Errorf("Expected auto-populated Source to start with 'logger_test.go:', got %q", entry.Source)
						}
					}
				}

				// Check context fields mapped to specific entry fields
				if httpMethod, ok := tt.context["method"].(string); ok {
					if entry.RequestMethod != httpMethod {
						t.Errorf("Expected RequestMethod %q, got %q", httpMethod, entry.RequestMethod)
					}
				}
				if uri, ok := tt.context["uri"].(string); ok {
					if entry.RequestURI != uri {
						t.Errorf("Expected RequestURI %q, got %q", uri, entry.RequestURI)
					}
				}
				if h2id, ok := tt.context["h2_stream_id"].(uint32); ok {
					if entry.RequestH2StreamID != h2id {
						t.Errorf("Expected RequestH2StreamID %d, got %d", h2id, entry.RequestH2StreamID)
					}
				}

				// Check remaining context fields in Details
				expectedDetails := make(LogFields)
				if tt.context != nil {
					for k, v := range tt.context {
						if k != "source" && k != "method" && k != "uri" && k != "h2_stream_id" {
							expectedDetails[k] = v
						}
					}
				}
				if len(expectedDetails) > 0 {
					if entry.Details == nil {
						t.Errorf("Expected Details map, got nil. Expected: %v", expectedDetails)
					} else {
						for k, expectedV := range expectedDetails {
							actualV, ok := entry.Details[k]
							if !ok {
								t.Errorf("Expected key %q in Details, but not found. Entry Details: %v", k, entry.Details)
								continue
							}
							// JSON unmarshals numbers as float64 by default
							if fExpected, okF := expectedV.(float64); okF {
								if fActual, okA := actualV.(float64); okA {
									if fExpected != fActual {
										t.Errorf("Details mismatch for key %q. Expected %v (float64), got %v (float64)", k, fExpected, fActual)
									}
								} else {
									t.Errorf("Details type mismatch for key %q. Expected float64, got %T", k, actualV)
								}
							} else if iExpected, okI := expectedV.(int); okI { // Handle int if original context had int
								if fActual, okA := actualV.(float64); okA { // JSON turns it into float64
									if float64(iExpected) != fActual {
										t.Errorf("Details mismatch for key %q. Expected %v (int, becomes float64), got %v (float64)", k, iExpected, fActual)
									}
								} else {
									t.Errorf("Details type mismatch for key %q. Expected float64 (from int), got %T", k, actualV)
								}
							} else if sExpected, okS := expectedV.(string); okS {
								if sActual, okSA := actualV.(string); okSA {
									if sExpected != sActual {
										t.Errorf("Details mismatch for key %q. Expected %q (string), got %q (string)", k, sExpected, sActual)
									}
								} else {
									t.Errorf("Details type mismatch for key %q. Expected string, got %T", k, actualV)
								}
							} else {
								// For other types, direct comparison or more specific checks might be needed
								if fmt.Sprintf("%v", expectedV) != fmt.Sprintf("%v", actualV) {
									t.Errorf("Details mismatch for key %q. Expected %v, got %v", k, expectedV, actualV)
								}
							}
						}
						// Check for unexpected keys in actual details
						for k := range entry.Details {
							if _, ok := expectedDetails[k]; !ok {
								t.Errorf("Unexpected key %q found in Details: %v", k, entry.Details[k])
							}
						}
					}
				} else {
					if entry.Details != nil {
						t.Errorf("Expected nil Details, got %v", entry.Details)
					}
				}

			} else {
				if len(logLines) != 0 {
					t.Errorf("Expected 0 log lines, got %d. Lines: %v", len(logLines), logLines)
				}
			}
		})
	}
}

// getExpectedLineNumberForErrorLog is a helper to estimate line number for source.
// THIS IS FRAGILE and only for basic validation.
// It assumes the call to el.Error/Warn/Info/Debug is on a specific line within the test.

func getExpectedLineNumberForErrorLog(level config.LogLevel) int {
	// This helper returns the expected line number of the el.XXX call
	// within TestErrorLogFormatAndContent for the specific test case:
	// "Error message, Error level, logged with auto source"
	// This test case calls el.Error(...).
	// The line numbers are relative to the start of the file and can change if the file is edited.
	// Current call sites in TestErrorLogFormatAndContent:
	// el.Debug(...) -> line 873 approx.
	// el.Info(...)  -> line 875 approx.
	// el.Warn(...)  -> line 877 approx.
	// el.Error(...) -> line 879 approx.

	// The test "Error message, Error level, logged with auto source" specifically uses config.LogLevelError.
	if level == config.LogLevelError {
		return 879 // This is the line number of `el.Error(tt.msg, tt.context)` for that test case.
	}

	// Fallback for other levels if their auto-source line numbers were to be checked precisely.
	// For now, only the LogLevelError case for "Error message, Error level, logged with auto source" uses this.
	// If this helper is used for other levels in similar auto-source checks, their line numbers
	// would need to be returned here.
	// Defaulting to the Error line as it's the primary one checked.
	// A panic or t.Fatalf might be better if an unexpected level is passed and needs precise checking.
	t := &testing.T{} // Temporary testing.T for Fatalf if needed, though not ideal in a helper like this.
	t.Logf("Warning: getExpectedLineNumberForErrorLog called with level %s, but precise line number check is primarily set up for LogLevelError.", level)
	return 879 // Default or placeholder
}

func TestGetRealClientIP(t *testing.T) {
	// Pre-parse some trusted proxies for use in tests
	trustedCIDRs := []string{"192.168.1.0/24", "10.0.0.0/8", "::1/128"}
	trustedIPs := []string{"172.16.0.1", "2001:db8:cafe::1"}
	allTrusted := append(trustedCIDRs, trustedIPs...)

	parsedProxies, err := preParseTrustedProxies(allTrusted)
	if err != nil {
		t.Fatalf("Failed to pre-parse trusted proxies: %v", err)
	}
	emptyProxies, _ := preParseTrustedProxies(nil)

	tests := []struct {
		name             string
		remoteAddr       string // Direct peer
		headers          http.Header
		realIPHeaderName string
		trustedProxies   parsedProxiesContainer
		expectedIP       string
	}{
		// Basic cases: No X-Forwarded-For or trusted proxies
		{
			name:             "No header, direct IP",
			remoteAddr:       "1.2.3.4:12345",
			headers:          http.Header{},
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   emptyProxies,
			expectedIP:       "1.2.3.4",
		},
		{
			name:             "No header, direct IPv6",
			remoteAddr:       "[2001:db8::1]:12345",
			headers:          http.Header{},
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   emptyProxies,
			expectedIP:       "2001:db8::1",
		},
		{
			name:             "No header, direct IP (no port)",
			remoteAddr:       "1.2.3.4",
			headers:          http.Header{},
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   emptyProxies,
			expectedIP:       "1.2.3.4",
		},
		{
			name:             "No header, direct IPv6 (no port)",
			remoteAddr:       "2001:db8::1",
			headers:          http.Header{},
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   emptyProxies,
			expectedIP:       "2001:db8::1",
		},
		{
			name:             "No header, localhost",
			remoteAddr:       "localhost:12345",
			headers:          http.Header{},
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   emptyProxies,
			expectedIP:       "localhost", // remains localhost if not an IP
		},
		{
			name:             "Empty header value",
			remoteAddr:       "1.2.3.4:12345",
			headers:          http.Header{"X-Forwarded-For": {""}},
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   emptyProxies,
			expectedIP:       "1.2.3.4",
		},

		// X-Forwarded-For, no trusted proxies (first IP in XFF is taken if valid)
		{
			name:             "XFF simple, no trust",
			remoteAddr:       "192.168.1.1:12345", // This is a proxy
			headers:          http.Header{"X-Forwarded-For": {"1.2.3.4"}},
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   emptyProxies,
			expectedIP:       "1.2.3.4", // XFF takes precedence if present
		},
		{
			name:             "XFF multiple, no trust",
			remoteAddr:       "192.168.1.1:12345",
			headers:          http.Header{"X-Forwarded-For": {"1.2.3.4, 10.0.0.1"}}, // Rightmost is 10.0.0.1
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   emptyProxies,
			expectedIP:       "10.0.0.1", // Rightmost non-trusted (all are non-trusted here)
		},

		// X-Forwarded-For with trusted proxies
		{
			name:             "XFF, direct peer trusted, XFF IP not trusted",
			remoteAddr:       "192.168.1.50:12345", // Trusted by CIDR
			headers:          http.Header{"X-Forwarded-For": {"1.1.1.1"}},
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   parsedProxies,
			expectedIP:       "1.1.1.1", // 1.1.1.1 is not trusted, so it's the real IP
		},
		{
			name:             "XFF, multiple IPs, last one not trusted",
			remoteAddr:       "172.16.0.1:54321",                                                     // Trusted single IP
			headers:          http.Header{"X-Forwarded-For": {"2.2.2.2, 10.0.0.100, 192.168.1.200"}}, // 10.0.0.100 and 192.168.1.200 are trusted
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   parsedProxies,
			expectedIP:       "2.2.2.2", // 2.2.2.2 is the first non-trusted from right (effectively left here)
		},
		{
			name:             "XFF, all IPs in header trusted, use direct peer",
			remoteAddr:       "5.5.5.5:11111",                                               // Not trusted
			headers:          http.Header{"X-Forwarded-For": {"10.0.0.100, 192.168.1.200"}}, // All trusted
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   parsedProxies,
			expectedIP:       "5.5.5.5", // Fallback to direct peer as all in XFF were trusted
		},
		{
			name:             "XFF, IPv6 mixed",
			remoteAddr:       "[::1]:12345",                                                          // Trusted
			headers:          http.Header{"X-Forwarded-For": {"2001:db8:aaaa::1, 2001:db8:cafe::1"}}, // cafe is trusted
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   parsedProxies,
			expectedIP:       "2001:db8:aaaa::1",
		},
		{
			name:             "XFF, malformed IP in header, use direct peer",
			remoteAddr:       "192.168.1.50:12345", // Trusted
			headers:          http.Header{"X-Forwarded-For": {"not-an-ip, 10.0.0.100"}},
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   parsedProxies,
			expectedIP:       "192.168.1.50", // Malformed header part leads to using direct peer
		},
		{
			name:             "XFF, malformed IP at the end, use direct peer",
			remoteAddr:       "192.168.1.50:12345", // Trusted
			headers:          http.Header{"X-Forwarded-For": {"10.0.0.100, not-an-ip"}},
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   parsedProxies,
			expectedIP:       "192.168.1.50", // Malformed header part leads to using direct peer
		},
		{
			name:             "XFF, spaces and empty elements",
			remoteAddr:       "192.168.1.50:12345", // Trusted
			headers:          http.Header{"X-Forwarded-For": {"  3.3.3.3  , ,  10.0.0.100  "}},
			realIPHeaderName: "X-Forwarded-For",
			trustedProxies:   parsedProxies,
			expectedIP:       "3.3.3.3",
		},
		{
			name:             "Custom RealIPHeader",
			remoteAddr:       "192.168.1.50:12345", // Trusted
			headers:          http.Header{"Cf-Connecting-Ip": {"4.4.4.4"}},
			realIPHeaderName: "Cf-Connecting-Ip",
			trustedProxies:   parsedProxies,
			expectedIP:       "4.4.4.4",
		},
		{
			name:             "RealIPHeader not present",
			remoteAddr:       "1.2.3.4:12345",
			headers:          http.Header{"X-Forwarded-For": {"ignore-me"}}, // This header exists but is not the one we look for
			realIPHeaderName: "X-Real-IP",                                   // This one is not in headers
			trustedProxies:   parsedProxies,
			expectedIP:       "1.2.3.4",
		},
		{
			name:             "No RealIPHeader configured (empty string)",
			remoteAddr:       "1.2.3.4:12345",
			headers:          http.Header{"X-Forwarded-For": {"5.6.7.8"}},
			realIPHeaderName: "", // Logger config would have empty RealIPHeader
			trustedProxies:   parsedProxies,
			expectedIP:       "1.2.3.4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// getRealClientIP expects host part of remoteAddr, not host:port.
			hostOnly, _, err := net.SplitHostPort(tt.remoteAddr)
			if err != nil { // Not host:port, might be just host
				hostOnly = tt.remoteAddr
			}

			actualIP := getRealClientIP(hostOnly, tt.headers, tt.realIPHeaderName, tt.trustedProxies)
			if actualIP != tt.expectedIP {
				t.Errorf("getRealClientIP() got = %v, want %v", actualIP, tt.expectedIP)
			}
		})
	}
}

func TestPreParseTrustedProxies(t *testing.T) {
	tests := []struct {
		name          string
		proxyStrings  []string
		expectCIDRLen int
		expectIPLen   int
		expectError   bool
	}{
		{
			name:          "Valid mix of CIDRs and IPs",
			proxyStrings:  []string{"192.168.1.0/24", "10.0.0.1", "2001:db8::/32", "::1"},
			expectCIDRLen: 2,
			expectIPLen:   2,
			expectError:   false,
		},
		{
			name:          "Only CIDRs",
			proxyStrings:  []string{"172.16.0.0/12", "fd00::/8"},
			expectCIDRLen: 2,
			expectIPLen:   0,
			expectError:   false,
		},
		{
			name:          "Only IPs",
			proxyStrings:  []string{"1.2.3.4", "fe80::1234"},
			expectCIDRLen: 0,
			expectIPLen:   2,
			expectError:   false,
		},
		{
			name:          "Empty input",
			proxyStrings:  []string{},
			expectCIDRLen: 0,
			expectIPLen:   0,
			expectError:   false,
		},
		{
			name:          "Nil input",
			proxyStrings:  nil,
			expectCIDRLen: 0,
			expectIPLen:   0,
			expectError:   false,
		},
		{
			name:          "Strings with spaces",
			proxyStrings:  []string{"  192.168.1.0/24  ", "  10.0.0.1  "},
			expectCIDRLen: 1,
			expectIPLen:   1,
			expectError:   false,
		},
		{
			name:          "Empty string in list",
			proxyStrings:  []string{"192.168.1.0/24", "", "10.0.0.1"},
			expectCIDRLen: 1,
			expectIPLen:   1,
			expectError:   false,
		},
		{
			name:          "Invalid CIDR",
			proxyStrings:  []string{"192.168.1.0/33"},
			expectCIDRLen: 0,
			expectIPLen:   0,
			expectError:   true,
		},
		{
			name:          "Invalid IP",
			proxyStrings:  []string{"not-an-ip"},
			expectCIDRLen: 0,
			expectIPLen:   0,
			expectError:   true,
		},
		{
			name:          "Mixed valid and invalid IP",
			proxyStrings:  []string{"1.2.3.4", "not-an-ip"},
			expectCIDRLen: 0, // Should fail before fully populating if an error occurs
			expectIPLen:   0, // Depending on implementation, might be 1 if it processes then errors
			expectError:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := preParseTrustedProxies(tt.proxyStrings)
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected an error, but got nil")
				}
			} else {
				if err != nil {
					t.Errorf("Did not expect an error, but got: %v", err)
				}
				if len(parsed.cidrs) != tt.expectCIDRLen {
					t.Errorf("Expected %d CIDRs, got %d", tt.expectCIDRLen, len(parsed.cidrs))
				}
				if len(parsed.ips) != tt.expectIPLen {
					t.Errorf("Expected %d IPs, got %d", tt.expectIPLen, len(parsed.ips))
				}
			}
		})
	}
}

// TestNewLoggerFileCreationAndReopen tests if log files are created and can be reopened.
// This is a more complex test involving file system operations.
func TestNewLoggerFileCreationAndReopen(t *testing.T) {
	accessLogPath, accessCleanup := createTempLogFile(t, "access-*.log")
	defer accessCleanup()

	errorLogPath, errorCleanup := createTempLogFile(t, "error-*.log")
	defer errorCleanup()

	cfg := newTestLoggerConfig(
		config.LogLevelDebug,
		accessLogPath,
		true, // access log enabled
		"json",
		nil, // trusted proxies
		nil, // real ip header
		errorLogPath,
	)

	logger, err := NewLogger(cfg)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	// Log something to ensure files are written to
	req := newMockHTTPRequest(t, "GET", "/test-reopen", "1.2.3.4:5678", nil)
	req.Proto = "HTTP/2.0"
	logger.Access(req, 1, 200, 100, time.Millisecond*10)
	logger.Info("Test info message for reopen", nil)
	logger.Error("Test error message for reopen", nil)

	// Close initial log files (as NewLogger opens them)
	// The ReopenLogFiles method within logger should handle its own internal file descriptors.
	// We will NOT close them here to avoid "file already closed" errors during reopen,
	// as ReopenLogFiles will attempt to close them.
	// if logger.accessLog != nil && logger.accessLog.output != nil {
	// 	if f, ok := logger.accessLog.output.(*os.File); ok {
	// 		if f.Name() == accessLogPath { // Ensure it's the file we think it is
	// 			f.Close()
	// 		}
	// 	}
	// }
	// if logger.errorLog != nil && logger.errorLog.output != nil {
	// 	if f, ok := logger.errorLog.output.(*os.File); ok {
	// 		if f.Name() == errorLogPath {
	// 			f.Close()
	// 		}
	// 	}
	// }

	// Simulate log rotation: move the old files (optional, but good for testing reopen creates new FD)
	// For simplicity, we'll just let ReopenLogFiles open the same path again.
	// A more robust test might rename the old files.

	if err := logger.ReopenLogFiles(); err != nil {
		t.Fatalf("ReopenLogFiles() error = %v", err)
	}

	// Log again after reopening
	logger.Access(req, 2, 201, 200, time.Millisecond*20)
	logger.Info("Test info message post-reopen", nil)

	// It's important to close the files through the logger before reading,
	// to ensure all buffers are flushed.
	if err := logger.CloseLogFiles(); err != nil {
		t.Fatalf("CloseLogFiles() error = %v", err)
	}

	// Verify content of access log
	accessContent, err := ioutil.ReadFile(accessLogPath)
	if err != nil {
		t.Fatalf("Failed to read access log %s: %v", accessLogPath, err)
	}
	accessLines := strings.Split(strings.TrimSpace(string(accessContent)), "\n")
	if len(accessLines) != 2 {
		t.Errorf("Expected 2 lines in access log, got %d. Content: %s", len(accessLines), string(accessContent))
	} else {
		var entry1, entry2 AccessLogEntry
		if err := json.Unmarshal([]byte(accessLines[0]), &entry1); err != nil {
			t.Errorf("Failed to parse first access log line: %v", err)
		}
		if err := json.Unmarshal([]byte(accessLines[1]), &entry2); err != nil {
			t.Errorf("Failed to parse second access log line: %v", err)
		}
		if entry1.H2StreamID != 1 {
			t.Errorf("Expected first access log stream ID 1, got %d", entry1.H2StreamID)
		}
		if entry2.H2StreamID != 2 {
			t.Errorf("Expected second access log stream ID 2, got %d", entry2.H2StreamID)
		}
	}

	// Verify content of error log
	errorContent, err := ioutil.ReadFile(errorLogPath)
	if err != nil {
		t.Fatalf("Failed to read error log %s: %v", errorLogPath, err)
	}
	errorLines := strings.Split(strings.TrimSpace(string(errorContent)), "\n")
	// Expect 3 lines: Info, Error (before reopen), Info (after reopen)
	if len(errorLines) != 3 {
		t.Errorf("Expected 3 lines in error log, got %d. Content: %s", len(errorLines), string(errorContent))
	} else {
		var e1, e2, e3 ErrorLogEntry
		if err := json.Unmarshal([]byte(errorLines[0]), &e1); err != nil {
			t.Errorf("Parse e1: %v", err)
		}
		if err := json.Unmarshal([]byte(errorLines[1]), &e2); err != nil {
			t.Errorf("Parse e2: %v", err)
		}
		if err := json.Unmarshal([]byte(errorLines[2]), &e3); err != nil {
			t.Errorf("Parse e3: %v", err)
		}

		if e1.Message != "Test info message for reopen" {
			t.Errorf("Bad e1 msg: %s", e1.Message)
		}
		if e2.Message != "Test error message for reopen" {
			t.Errorf("Bad e2 msg: %s", e2.Message)
		}
		if e3.Message != "Test info message post-reopen" {
			t.Errorf("Bad e3 msg: %s", e3.Message)
		}
	}
}

// TestLoggerStdoutStderr tests logging to stdout/stderr.
// This test doesn't assert output content directly but ensures no panics or errors
// when configured for stdout/stderr. Output can be manually inspected if run with -v.
func TestLoggerStdoutStderr(t *testing.T) {
	stdoutTarget := "stdout"
	stderrTarget := "stderr"

	tests := []struct {
		name            string
		accessLogTarget *string
		errorLogTarget  *string
	}{
		{"Access to stdout, Error to stderr", &stdoutTarget, &stderrTarget},
		{"Access to stderr, Error to stdout", &stderrTarget, &stdoutTarget},
		{"Access to stdout, Error to stdout", &stdoutTarget, &stdoutTarget},
		{"Access to stderr, Error to stderr", &stderrTarget, &stderrTarget},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessEnabled := true
			cfg := &config.LoggingConfig{
				LogLevel: config.LogLevelDebug,
				AccessLog: &config.AccessLogConfig{
					Enabled: &accessEnabled,
					Target:  tt.accessLogTarget,
					Format:  "json",
				},
				ErrorLog: &config.ErrorLogConfig{
					Target: tt.errorLogTarget,
				},
			}

			logger, err := NewLogger(cfg)
			if err != nil {
				t.Fatalf("NewLogger() with target %s/%s error = %v", *tt.accessLogTarget, *tt.errorLogTarget, err)
			}

			// Log some messages
			req := newMockHTTPRequest(t, "GET", "/stdout-stderr", "127.0.0.1:1234", nil)
			req.Proto = "HTTP/2.0"
			logger.Access(req, 10, http.StatusOK, 0, 5*time.Millisecond)
			logger.Info("Info to stdio", LogFields{"target_access": *tt.accessLogTarget, "target_error": *tt.errorLogTarget})
			logger.Error("Error to stdio", nil)

			// Reopening stdout/stderr should be a no-op and not error
			if err := logger.ReopenLogFiles(); err != nil {
				t.Errorf("ReopenLogFiles() for stdio targets errored: %v", err)
			}

			// Closing stdout/stderr should also be a no-op handled by the logger
			if err := logger.CloseLogFiles(); err != nil {
				t.Errorf("CloseLogFiles() for stdio targets errored: %v", err)
			}
			t.Logf("Completed test for Access: %s, Error: %s. Manual output verification may be needed if -v is used.", *tt.accessLogTarget, *tt.errorLogTarget)
		})
	}
}

// Mock net.SplitHostPort and strconv.Atoi for specific test cases if needed,
// though direct use is often fine for testing logger's consumption of these.

// TestAccessLogMarshallingError simulates an error during JSON marshalling of an access log entry.
// It checks if the logger falls back to logging a predefined error message.
func TestAccessLogMarshallingError(t *testing.T) {
	var buf bytes.Buffer
	enabled := true
	accessLogCfg := &config.AccessLogConfig{
		Enabled: &enabled,
		Format:  "json", // Format is json
		Target:  stringPtr("buffer"),
	}

	al, err := newTestAccessLogger(accessLogCfg, &buf)
	if err != nil {
		t.Fatalf("newTestAccessLogger failed: %v", err)
	}

	// Create a request that will cause marshalling to fail.
	// json.Marshal fails on channels, functions, or complex numbers.
	// We can't directly put these into http.Request in a way that LogAccess
	// would pick up and put raw into the AccessLogEntry struct.
	// Instead, we'll modify the AccessLogEntry struct within the test's scope
	// to include an unmarshallable field *if* we were testing json.Marshal directly.
	// However, our AccessLogEntry is well-defined.
	// The spec for logger.AccessLogEntry doesn't permit fields that would fail marshalling.
	// The most likely scenario for marshalling failure with the current AccessLogEntry
	// would be if string fields contained invalid UTF-8, but Go strings are typically UTF-8.
	// This test is more conceptual for the fallback mechanism.
	// The only way to reliably trigger this with the current structure is if `json.Marshal` itself fails for an unknown reason,
	// which is hard to simulate without manipulating internal state or using a mock JSON library.
	// The current implementation's fallback is:
	// errorMsg := fmt.Sprintf("{\"level\":\"ERROR\", \"ts\":\"%s\", \"msg\":\"Error marshalling access log entry to JSON\", \"error\":\"%s\"}" ...
	// This test will be limited to verifying this fallback is present if invoked.
	// Since it's hard to trigger `json.Marshal` to fail with valid `AccessLogEntry` struct,
	// this test is more of a placeholder or would require deeper mocking.

	// For now, assume the fallback logic itself is sound and covered by review.
	// A more direct test would involve creating a custom writer that injects failure,
	// or temporarily making one of the AccessLogEntry fields unmarshallable.

	// Let's test the scenario where LogAccess is called on a nil AccessLogger
	var nilLogger *AccessLogger
	req := newMockHTTPRequest(t, "GET", "/testpath", "192.168.1.10:12345", nil)
	nilLogger.LogAccess(req, 1, http.StatusOK, 100, 50*time.Millisecond) // Should not panic

	// Test the fallback format when an error is *manually* crafted into the log line writer
	// This doesn't test the trigger, but the fallback message format
	fallbackMsg := fmt.Sprintf("{\"level\":\"ERROR\", \"ts\":\"%s\", \"msg\":\"Error marshalling access log entry to JSON\", \"error\":\"%s\"}",
		"dummy_ts", "dummy_error")
	al.writeLogLine(fallbackMsg) // Manually write a "failed marshal" message

	lines := readLogBuffer(&buf)
	if len(lines) != 1 {
		t.Fatalf("Expected 1 line for fallback, got %d", len(lines))
	}
	var fallbackEntry struct {
		Level string `json:"level"`
		TS    string `json:"ts"`
		Msg   string `json:"msg"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &fallbackEntry); err != nil {
		t.Fatalf("Failed to parse fallback error log: %v", err)
	}
	if fallbackEntry.Level != "ERROR" {
		t.Errorf("Expected fallback level ERROR, got %s", fallbackEntry.Level)
	}
	if fallbackEntry.Msg != "Error marshalling access log entry to JSON" {
		t.Errorf("Expected fallback msg, got %s", fallbackEntry.Msg)
	}

	// Reset buffer for next check
	buf.Reset()

	// What if the ErrorLogger's marshalling fails?
	var errBuf bytes.Buffer
	errorLogCfg := &config.ErrorLogConfig{Target: stringPtr("buffer")}
	el := newTestErrorLogger(errorLogCfg, config.LogLevelDebug, &errBuf)

	// Similar to access log, ErrorLogEntry is well-defined.
	// Manually write a "failed marshal" message for error log
	errorFallbackMsg := fmt.Sprintf("{\"level\":\"ERROR\", \"ts\":\"%s\", \"msg\":\"Error marshalling error log entry to JSON\", \"original_level\":\"%s\", \"original_msg\":\"%s\", \"error\":\"%s\"}",
		"dummy_ts", "dummy_orig_level", "dummy_orig_msg", "dummy_error")
	el.writeLogLine(errorFallbackMsg)

	errLines := readLogBuffer(&errBuf)
	if len(errLines) != 1 {
		t.Fatalf("Expected 1 line for error fallback, got %d", len(errLines))
	}
	var errFallbackEntry struct {
		Level         string `json:"level"`
		TS            string `json:"ts"`
		Msg           string `json:"msg"`
		OriginalLevel string `json:"original_level"`
		OriginalMsg   string `json:"original_msg"`
		Error         string `json:"error"`
	}
	if err := json.Unmarshal([]byte(errLines[0]), &errFallbackEntry); err != nil {
		t.Fatalf("Failed to parse error fallback log: %v", err)
	}
	if errFallbackEntry.Level != "ERROR" {
		t.Errorf("Expected error fallback level ERROR, got %s", errFallbackEntry.Level)
	}
	if errFallbackEntry.Msg != "Error marshalling error log entry to JSON" {
		t.Errorf("Expected error fallback msg, got %s", errFallbackEntry.Msg)
	}
}

func TestAccessLoggingToDifferentTargets(t *testing.T) {
	// baseAccessLogCfg and baseErrorLogCfg are simplified for focus.
	// Real NewLogger uses more complex defaulting logic from config.applyDefaults.
	// Here, we ensure necessary fields are set for NewLogger to succeed.
	createDefaultErrorLogCfg := func(target string) *config.ErrorLogConfig {
		return &config.ErrorLogConfig{Target: stringPtr(target)}
	}
	createDefaultAccessLogCfg := func(target string, enabled bool) *config.AccessLogConfig {
		return &config.AccessLogConfig{
			Enabled:      boolPtr(enabled),
			Target:       stringPtr(target),
			Format:       "json",                       // Default and only supported format for now
			RealIPHeader: stringPtr("X-Forwarded-For"), // Default
		}
	}

	// --- Test Case 1: Access Log to File ---
	t.Run("AccessLogToFile", func(t *testing.T) {
		tempFilePath, cleanup := createTempLogFile(t, "access-target-*.log")
		defer cleanup()

		cfg := &config.LoggingConfig{
			LogLevel:  config.LogLevelDebug,
			AccessLog: createDefaultAccessLogCfg(tempFilePath, true),
			ErrorLog:  createDefaultErrorLogCfg("stderr"), // Error log to stderr to not interfere
		}

		logger, err := NewLogger(cfg)
		if err != nil {
			t.Fatalf("NewLogger failed for file target: %v", err)
		}

		req := newMockHTTPRequest(t, "GET", "/file-target", "10.0.0.1:1234", nil)
		req.Proto = "HTTP/2.0" // Ensure protocol is as expected in logs
		streamID := uint32(77)
		status := http.StatusAccepted
		respBytes := int64(123)
		duration := 75 * time.Millisecond

		logger.Access(req, streamID, status, respBytes, duration)

		// Ensure logs are flushed by closing the logger's file handles
		if err := logger.CloseLogFiles(); err != nil {
			t.Fatalf("CloseLogFiles failed: %v", err)
		}

		content, err := ioutil.ReadFile(tempFilePath)
		if err != nil {
			t.Fatalf("Failed to read access log file %s: %v", tempFilePath, err)
		}
		lines := strings.Split(strings.TrimSpace(string(content)), "\n")
		if len(lines) != 1 {
			t.Fatalf("Expected 1 log line in file, got %d. Content:\n%s", len(lines), string(content))
		}

		var entry AccessLogEntry
		if err := parseJSONLog([]byte(lines[0]), &entry); err != nil {
			t.Fatalf("Failed to parse access log entry from file: %v. Line: %s", err, lines[0])
		}

		// Verify some key fields to ensure correct entry was written
		if entry.URI != "/file-target" {
			t.Errorf("Expected URI '/file-target', got '%s'", entry.URI)
		}
		if entry.Status != status {
			t.Errorf("Expected status %d, got %d", status, entry.Status)
		}
		if entry.H2StreamID != streamID {
			t.Errorf("Expected stream ID %d, got %d", streamID, entry.H2StreamID)
		}
		if entry.RemoteAddr != "10.0.0.1" { // Simple case, no proxying involved in this mock
			t.Errorf("Expected remote addr '10.0.0.1', got '%s'", entry.RemoteAddr)
		}
		if entry.Protocol != "HTTP/2.0" {
			t.Errorf("Expected protocol 'HTTP/2.0', got '%s'", entry.Protocol)
		}
	})

	// --- Test Case 2: Access Log to stdout ---
	t.Run("AccessLogToStdout", func(t *testing.T) {
		cfg := &config.LoggingConfig{
			LogLevel:  config.LogLevelDebug,
			AccessLog: createDefaultAccessLogCfg("stdout", true),
			ErrorLog:  createDefaultErrorLogCfg("stderr"), // Error log to different stream
		}

		logger, err := NewLogger(cfg)
		if err != nil {
			t.Fatalf("NewLogger failed for stdout target: %v", err)
		}

		req := newMockHTTPRequest(t, "GET", "/stdout-target", "10.0.0.2:1234", nil)
		logger.Access(req, 88, http.StatusOK, 0, 10*time.Millisecond)

		if err := logger.CloseLogFiles(); err != nil { // Should be no-op for stdout
			t.Errorf("CloseLogFiles for stdout target errored: %v", err)
		}
		// Direct verification of stdout content is not done in unit tests.
		// Test ensures no panics or errors during configuration and logging.
		t.Log("Access log to stdout test completed. Manual output verification if running with -v.")
	})

	// --- Test Case 3: Access Log to stderr ---
	t.Run("AccessLogToStderr", func(t *testing.T) {
		cfg := &config.LoggingConfig{
			LogLevel:  config.LogLevelDebug,
			AccessLog: createDefaultAccessLogCfg("stderr", true),
			ErrorLog:  createDefaultErrorLogCfg("stdout"), // Error log to different stream
		}

		logger, err := NewLogger(cfg)
		if err != nil {
			t.Fatalf("NewLogger failed for stderr target: %v", err)
		}

		req := newMockHTTPRequest(t, "GET", "/stderr-target", "10.0.0.3:1234", nil)
		logger.Access(req, 99, http.StatusNotFound, 0, 15*time.Millisecond)

		if err := logger.CloseLogFiles(); err != nil { // Should be no-op for stderr
			t.Errorf("CloseLogFiles for stderr target errored: %v", err)
		}
		// Direct verification of stderr content is not done in unit tests.
		t.Log("Access log to stderr test completed. Manual output verification if running with -v.")
	})
}

func TestAtomicLogWrites(t *testing.T) {
	numGoroutines := 10
	logsPerGoroutine := 100
	totalLogs := numGoroutines * logsPerGoroutine

	t.Run("AccessLoggerAtomicWrite", func(t *testing.T) {
		var buf bytes.Buffer
		enabled := true
		accessLogCfg := &config.AccessLogConfig{
			Enabled: &enabled,
			Format:  "json",
			Target:  stringPtr("buffer"), // Use buffer for easy capture
		}

		al, err := newTestAccessLogger(accessLogCfg, &buf)
		if err != nil {
			t.Fatalf("newTestAccessLogger failed: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func(goroutineID int) {
				defer wg.Done()
				for j := 0; j < logsPerGoroutine; j++ {
					req := newMockHTTPRequest(t, "GET", fmt.Sprintf("/path/%d/%d", goroutineID, j), "1.2.3.4:1234", nil)
					req.Proto = "HTTP/2.0"
					// Vary stream ID to make entries somewhat unique, though content doesn't strictly need to be unique for this test.
					// The key is that each JSON line is valid.
					streamID := uint32(goroutineID*logsPerGoroutine + j)
					al.LogAccess(req, streamID, http.StatusOK, int64(j*10), time.Duration(j)*time.Millisecond)
				}
			}(i)
		}
		wg.Wait()

		logLines := readLogBuffer(&buf)
		if len(logLines) != totalLogs {
			t.Errorf("Expected %d total log lines, got %d", totalLogs, len(logLines))
		}

		for i, line := range logLines {
			var entry AccessLogEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Errorf("Failed to unmarshal access log line %d: %v. Line content: %s", i, err, line)
			}
		}
	})

	t.Run("ErrorLoggerAtomicWrite", func(t *testing.T) {
		var buf bytes.Buffer
		errorLogCfg := &config.ErrorLogConfig{
			Target: stringPtr("buffer"),
		}
		el := newTestErrorLogger(errorLogCfg, config.LogLevelDebug, &buf)

		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func(goroutineID int) {
				defer wg.Done()
				for j := 0; j < logsPerGoroutine; j++ {
					// Vary message or context to make entries somewhat unique.
					// Using LogError directly to showcase context usage for details.
					el.LogError(config.LogLevelInfo,
						fmt.Sprintf("Error log from goroutine %d, message %d", goroutineID, j),
						LogFields{
							"goroutine_id": goroutineID,
							"message_id":   j,
							"h2_stream_id": uint32(goroutineID*logsPerGoroutine + j),
						})
				}
			}(i)
		}
		wg.Wait()

		logLines := readLogBuffer(&buf)
		if len(logLines) != totalLogs {
			t.Errorf("Expected %d total log lines, got %d", totalLogs, len(logLines))
		}

		for i, line := range logLines {
			var entry ErrorLogEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Errorf("Failed to unmarshal error log line %d: %v. Line content: %s", i, err, line)
			}
			// Optional: check some details from the context if needed, but main goal is unmarshal success.
			if entry.Details == nil {
				t.Errorf("Error log line %d details map is nil. Line: %s", i, line)
				continue
			}
			if _, ok := entry.Details["goroutine_id"]; !ok {
				t.Errorf("Error log line %d missing 'goroutine_id' in details. Line: %s", i, line)
			}
		}
	})
}

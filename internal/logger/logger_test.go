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
	// "path/filepath" // Removed as it was unused
	"strings"
	"testing"
	"time"

	"example.com/llmahttap/v2/internal/config"
)

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
		req.Header = headers
	}
	return req
}

// parseJSONLog attempts to unmarshal a single log line (as bytes) into the target struct.
func parseJSONLog(line []byte, targetStruct interface{}) error {
	return json.Unmarshal(line, targetStruct)
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

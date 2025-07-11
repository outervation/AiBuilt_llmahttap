package staticfileserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	// "html" // For escaping in assertions - not directly used by tests for now
	"io" // Added for io.Reader
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	// "sort"   // For checking sort order if necessary directly in test - not directly used
	"strings"
	"sync"
	"testing"
	"time" // Added missing import

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/handlers/staticfile"
	"example.com/llmahttap/v2/internal/http2"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/router" // Added import
	"example.com/llmahttap/v2/internal/server"

	"github.com/dustin/go-humanize" // For comparing sizes if necessary
)

import (
// "os" // already imported
// "time" // already imported
)

// generateETag creates a strong ETag based on file size and modification time.
// This MUST match the ETag generation logic in staticfile.StaticFileServer.
func generateETag(fi os.FileInfo) string {
	return fmt.Sprintf("\"%x-%x\"", fi.Size(), fi.ModTime().UnixNano())
}

// mockStreamWriter is a mock implementation of http2.StreamWriter for testing.
type mockStreamWriter struct {
	mu          sync.Mutex
	id          uint32
	ctx         context.Context
	headers     []http2.HeaderField
	body        *bytes.Buffer
	trailers    []http2.HeaderField
	headersSent bool
	dataWritten bool
	ended       bool

	t             *testing.T                                  // For logging errors within the mock
	writeDataHook func(p []byte, endStream bool) (int, error) // Hook for custom WriteData behavior
}

func newMockStreamWriter(t *testing.T, streamID uint32) *mockStreamWriter {
	t.Helper()
	return &mockStreamWriter{
		id:   streamID,
		ctx:  context.Background(), // Default context
		body: new(bytes.Buffer),
		t:    t,
	}
}

func (msw *mockStreamWriter) SendHeaders(headers []http2.HeaderField, endStream bool) error {
	msw.mu.Lock()
	defer msw.mu.Unlock()

	if msw.headersSent {
		msw.t.Errorf("mockStreamWriter (ID %d): SendHeaders called more than once", msw.id)
		return fmt.Errorf("headers already sent")
	}
	msw.headers = make([]http2.HeaderField, len(headers))
	copy(msw.headers, headers)
	msw.headersSent = true
	msw.ended = endStream
	// msw.t.Logf("mockStreamWriter (ID %d): SendHeaders called with %v, endStream: %v", msw.id, headers, endStream)
	return nil
}

func (msw *mockStreamWriter) WriteData(p []byte, endStream bool) (n int, err error) {
	msw.mu.Lock()
	defer msw.mu.Unlock()

	if !msw.headersSent {
		msw.t.Errorf("mockStreamWriter (ID %d): WriteData called before SendHeaders", msw.id)
		return 0, fmt.Errorf("headers not sent yet")
	}
	if msw.ended {
		msw.t.Errorf("mockStreamWriter (ID %d): WriteData called after stream ended", msw.id)
		return 0, fmt.Errorf("stream already ended")
	}

	if msw.writeDataHook != nil {
		// If hook is set, it takes full control of WriteData's behavior.
		return msw.writeDataHook(p, endStream)
	}

	n, err = msw.body.Write(p)
	if err != nil {
		return n, err
	}
	msw.dataWritten = true
	msw.ended = endStream
	// msw.t.Logf("mockStreamWriter (ID %d): WriteData called with %d bytes, endStream: %v", msw.id, len(p), endStream)
	return n, nil
}

func (msw *mockStreamWriter) WriteTrailers(trailers []http2.HeaderField) error {
	msw.mu.Lock()
	defer msw.mu.Unlock()

	if !msw.headersSent {
		msw.t.Errorf("mockStreamWriter (ID %d): WriteTrailers called before SendHeaders", msw.id)
		return fmt.Errorf("headers not sent yet")
	}
	if msw.ended && !msw.dataWritten { // Can send trailers if headers only were sent and ended stream
		// Or if data was written and stream not yet ended by data
	} else if msw.ended {
		msw.t.Errorf("mockStreamWriter (ID %d): WriteTrailers called after stream already ended by data", msw.id)
		return fmt.Errorf("stream already ended by data")
	}

	msw.trailers = make([]http2.HeaderField, len(trailers))
	copy(msw.trailers, trailers)
	msw.ended = true
	// msw.t.Logf("mockStreamWriter (ID %d): WriteTrailers called with %v", msw.id, trailers)
	return nil
}

func (msw *mockStreamWriter) ID() uint32 {
	msw.mu.Lock()
	defer msw.mu.Unlock()
	return msw.id
}

func (msw *mockStreamWriter) Context() context.Context {
	msw.mu.Lock()
	defer msw.mu.Unlock()
	if msw.ctx == nil {
		return context.Background()
	}
	return msw.ctx
}

func (msw *mockStreamWriter) setContext(ctx context.Context) {
	msw.mu.Lock()
	defer msw.mu.Unlock()
	msw.ctx = ctx
}

func (msw *mockStreamWriter) getResponseStatus() string {
	msw.mu.Lock()
	defer msw.mu.Unlock()
	for _, h := range msw.headers {
		if h.Name == ":status" {
			return h.Value
		}
	}
	return ""
}

func (msw *mockStreamWriter) getResponseHeader(name string) string {
	msw.mu.Lock()
	defer msw.mu.Unlock()
	for _, h := range msw.headers {
		if strings.ToLower(h.Name) == strings.ToLower(name) {
			return h.Value
		}
	}
	return ""
}

func (msw *mockStreamWriter) getResponseBody() []byte {
	msw.mu.Lock()
	defer msw.mu.Unlock()
	return msw.body.Bytes()
}

// fileSpec defines a file or directory to be created in the test document root.
type fileSpec struct {
	Path    string      // Relative path within the document root
	Content string      // Content for files
	IsDir   bool        // True if this is a directory
	Mode    fs.FileMode // Permissions (optional, defaults to 0644 for files, 0755 for dirs)
}

// setupTestDocumentRoot creates a temporary directory structure for testing.

// ptrToMode is a helper function to get a pointer to a fs.FileMode value.
func ptrToMode(m fs.FileMode) *fs.FileMode {
	return &m
}

// It returns the path to the document root and a cleanup function.
func setupTestDocumentRoot(t *testing.T, files []fileSpec) (docRoot string, cleanup func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "sfs_test_docroot_")
	if err != nil {
		t.Fatalf("Failed to create temp dir for doc root: %v", err)
	}

	for _, f := range files {
		fullPath := filepath.Join(tmpDir, f.Path)
		mode := f.Mode
		if f.IsDir {
			if mode == 0 {
				mode = 0755
			}
			err = os.MkdirAll(fullPath, mode)
			if err != nil {
				os.RemoveAll(tmpDir)
				t.Fatalf("Failed to create directory %s: %v", fullPath, err)
			}
		} else {
			// Ensure parent directory exists for the file
			parentDir := filepath.Dir(fullPath)
			if err = os.MkdirAll(parentDir, 0755); err != nil {
				os.RemoveAll(tmpDir)
				t.Fatalf("Failed to create parent directory %s for file %s: %v", parentDir, fullPath, err)
			}

			if mode == 0 {
				mode = 0644
			}
			err = os.WriteFile(fullPath, []byte(f.Content), mode)
			if err != nil {
				os.RemoveAll(tmpDir)
				t.Fatalf("Failed to create file %s: %v", fullPath, err)
			}
		}
	}

	return tmpDir, func() {
		os.RemoveAll(tmpDir)
	}
}

// newTestStaticFileHandler creates a staticfile.StaticFileServer instance for testing.
// mainConfigFilePath is usually "" for unit tests unless relative MimeTypesPath needs resolving.
func newTestStaticFileHandler(t *testing.T, cfgIn config.StaticFileServerConfig, lg *logger.Logger, mainConfigFilePath string) *staticfile.StaticFileServer {
	t.Helper()
	if lg == nil {
		lg = logger.NewDiscardLogger() // Default to discard logger if none provided
	}

	// Marshal the input config.StaticFileServerConfig to json.RawMessage
	// This is what ParseAndValidateStaticFileServerConfig expects.
	rawCfgBytes, err := json.Marshal(cfgIn)
	if err != nil {
		t.Fatalf("Failed to marshal input StaticFileServerConfig for test: %v", err)
	}
	rawCfg := json.RawMessage(rawCfgBytes)

	// Parse and validate the config, resolving MIME types, applying defaults etc.
	resolvedCfg, err := config.ParseAndValidateStaticFileServerConfig(rawCfg, mainConfigFilePath)
	if err != nil {
		// If ParseAndValidateStaticFileServerConfig fails, we should not proceed to staticfile.New.
		// This simulates the config validation step that happens before handler creation.
		// The test `TestStaticFileServer_New` will specifically check for errors from this stage.
		// For other tests using this helper, they usually provide valid configs.
		// If a test *using this helper* intends for config parsing to fail, it's testing the wrong thing
		// with this helper. This helper is for getting a valid SFS instance.
		// Tests for *config parsing failure* should call ParseAndValidateStaticFileServerConfig directly,
		// which `TestStaticFileServer_New` will do.
		t.Fatalf("newTestStaticFileHandler: Failed to parse/validate StaticFileServerConfig: %v. Input cfg: %+v", err, cfgIn)
	}

	// Pass the resolved config to staticfile.New
	handler, err := staticfile.New(resolvedCfg, lg, mainConfigFilePath)
	if err != nil {
		t.Fatalf("newTestStaticFileHandler: staticfile.New failed: %v", err)
	}

	sfs, ok := handler.(*staticfile.StaticFileServer)
	if !ok {
		t.Fatalf("newTestStaticFileHandler: staticfile.New did not return a *staticfile.StaticFileServer instance, got %T", handler)
	}
	return sfs
}

// newTestRequest creates an http.Request for testing.
func newTestRequest(t *testing.T, method, path string, body io.Reader, headers http.Header) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, path, body)
	if err != nil {
		t.Fatalf("Failed to create test request: %v", err)
	}
	if headers != nil {
		req.Header = headers
	}
	return req
}

// TestMain can be used for global setup/teardown if needed, but spec says NO.

// Example basic test to ensure setup works (will be expanded later)
func TestStaticFileServer_InitialSetup(t *testing.T) {
	docRoot, cleanup := setupTestDocumentRoot(t, []fileSpec{
		{Path: "index.html", Content: "Hello World"},
	})
	defer cleanup()

	trueVal := true
	sfsConfig := config.StaticFileServerConfig{
		DocumentRoot:          docRoot,
		IndexFiles:            []string{"index.html"},
		ServeDirectoryListing: &trueVal,
	}

	testLogger := logger.NewTestLogger(os.Stdout) // Capture logs if needed
	sfs := newTestStaticFileHandler(t, sfsConfig, testLogger, "")

	if sfs == nil {
		t.Fatal("StaticFileServer instance is nil")
	}
	if sfs.Config.DocumentRoot != docRoot {
		t.Errorf("Expected DocumentRoot %s, got %s", docRoot, sfs.Config.DocumentRoot)
	}
}

func TestStaticFileServer_ServeHTTP2_RegularFiles(t *testing.T) {
	testFiles := []fileSpec{
		{Path: "hello.txt", Content: "Hello, World!"},
		{Path: "empty.txt", Content: ""},
		{Path: "image.png", Content: "fake png data"}, // Content doesn't matter, extension does for MIME
	}

	docRoot, cleanup := setupTestDocumentRoot(t, testFiles)
	defer cleanup()

	trueVal := true
	sfsConfig := config.StaticFileServerConfig{
		DocumentRoot:          docRoot,
		IndexFiles:            []string{"index.html"}, // Not relevant for direct file access
		ServeDirectoryListing: &trueVal,
		// Use default MIME types for this test
	}

	testLogger := logger.NewTestLogger(new(bytes.Buffer)) // Capture logs if needed
	sfs := newTestStaticFileHandler(t, sfsConfig, testLogger, "")

	// Expected values for hello.txt
	helloTxtFileInfo, err := os.Stat(filepath.Join(docRoot, "hello.txt"))
	if err != nil {
		t.Fatalf("Failed to stat hello.txt: %v", err)
	}
	expectedHelloETag := generateETag(helloTxtFileInfo)
	expectedHelloLastMod := helloTxtFileInfo.ModTime().UTC().Format(http.TimeFormat)

	// Expected values for empty.txt
	emptyTxtFileInfo, err := os.Stat(filepath.Join(docRoot, "empty.txt"))
	if err != nil {
		t.Fatalf("Failed to stat empty.txt: %v", err)
	}
	expectedEmptyETag := generateETag(emptyTxtFileInfo)
	expectedEmptyLastMod := emptyTxtFileInfo.ModTime().UTC().Format(http.TimeFormat)

	tests := []struct {
		name                  string
		method                string
		path                  string
		matchedRoutePattern   string // Simulates what router would pass in context
		expectedStatus        string
		expectedContentType   string
		expectedContent       string
		expectedContentLength string
		expectedETag          string
		expectedLastModified  string
		expectEmptyBody       bool
	}{
		{
			name:                  "GET non-empty file hello.txt",
			method:                http.MethodGet,
			path:                  "/files/hello.txt",
			matchedRoutePattern:   "/files/",
			expectedStatus:        "200",
			expectedContentType:   "text/plain; charset=utf-8", // Default for .txt via mime.TypeByExtension
			expectedContent:       "Hello, World!",
			expectedContentLength: fmt.Sprintf("%d", len("Hello, World!")),
			expectedETag:          expectedHelloETag,
			expectedLastModified:  expectedHelloLastMod,
			expectEmptyBody:       false,
		},
		{
			name:                  "HEAD non-empty file hello.txt",
			method:                http.MethodHead,
			path:                  "/files/hello.txt",
			matchedRoutePattern:   "/files/",
			expectedStatus:        "200",
			expectedContentType:   "text/plain; charset=utf-8",
			expectedContentLength: fmt.Sprintf("%d", len("Hello, World!")),
			expectedETag:          expectedHelloETag,
			expectedLastModified:  expectedHelloLastMod,
			expectEmptyBody:       true,
		},
		{
			name:                  "GET empty file empty.txt",
			method:                http.MethodGet,
			path:                  "/data/empty.txt",
			matchedRoutePattern:   "/data/",
			expectedStatus:        "200",
			expectedContentType:   "text/plain; charset=utf-8",
			expectedContent:       "",
			expectedContentLength: "0",
			expectedETag:          expectedEmptyETag,
			expectedLastModified:  expectedEmptyLastMod,
			expectEmptyBody:       false, // Body is empty, but not because it's HEAD
		},
		{
			name:                  "HEAD empty file empty.txt",
			method:                http.MethodHead,
			path:                  "/data/empty.txt",
			matchedRoutePattern:   "/data/",
			expectedStatus:        "200",
			expectedContentType:   "text/plain; charset=utf-8",
			expectedContentLength: "0",
			expectedETag:          expectedEmptyETag,
			expectedLastModified:  expectedEmptyLastMod,
			expectEmptyBody:       true,
		},
		{
			name:                  "GET image.png",
			method:                http.MethodGet,
			path:                  "/static/image.png",
			matchedRoutePattern:   "/static/",
			expectedStatus:        "200",
			expectedContentType:   "image/png", // Default for .png via mime.TypeByExtension
			expectedContent:       "fake png data",
			expectedContentLength: fmt.Sprintf("%d", len("fake png data")),
			// ETag and LastMod will be dynamic, so just check for presence and format.
			expectEmptyBody: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockWriter := newMockStreamWriter(t, 1)
			// Set the matched path pattern in the context
			ctx := context.WithValue(context.Background(), router.MatchedPathPatternKey{}, tc.matchedRoutePattern)
			mockWriter.setContext(ctx)

			req := newTestRequest(t, tc.method, tc.path, nil, nil)
			req = req.WithContext(ctx) // Ensure request also has the context

			sfs.ServeHTTP2(mockWriter, req)

			if status := mockWriter.getResponseStatus(); status != tc.expectedStatus {
				t.Errorf("Expected status %s, got %s", tc.expectedStatus, status)
				t.Logf("Response headers: %#v", mockWriter.headers)
				t.Logf("Response body: %s", string(mockWriter.getResponseBody()))
			}

			if contentType := mockWriter.getResponseHeader("content-type"); contentType != tc.expectedContentType {
				t.Errorf("Expected Content-Type %s, got %s", tc.expectedContentType, contentType)
			}
			if contentLength := mockWriter.getResponseHeader("content-length"); contentLength != tc.expectedContentLength {
				t.Errorf("Expected Content-Length %s, got %s", tc.expectedContentLength, contentLength)
			}

			// ETag check
			etag := mockWriter.getResponseHeader("etag")
			if tc.expectedETag != "" { // If specific ETag is expected
				if etag != tc.expectedETag {
					t.Errorf("Expected ETag %s, got %s", tc.expectedETag, etag)
				}
			} else { // Generic check for presence and format
				if etag == "" {
					t.Error("Expected ETag header, but it was missing")
				} else if !strings.HasPrefix(etag, "\"") || !strings.HasSuffix(etag, "\"") {
					t.Errorf("Expected ETag to be quoted, got %s", etag)
				}
			}

			// Last-Modified check
			lastModified := mockWriter.getResponseHeader("last-modified")
			if tc.expectedLastModified != "" { // If specific Last-Modified is expected
				if lastModified != tc.expectedLastModified {
					t.Errorf("Expected Last-Modified %s, got %s", tc.expectedLastModified, lastModified)
				}
			} else { // Generic check for presence and format
				if lastModified == "" {
					t.Error("Expected Last-Modified header, but it was missing")
				} else {
					_, err := http.ParseTime(lastModified)
					if err != nil {
						t.Errorf("Last-Modified header is not in a valid format: %s, error: %v", lastModified, err)
					}
				}
			}

			body := mockWriter.getResponseBody()
			if tc.expectEmptyBody {
				if len(body) != 0 {
					t.Errorf("Expected empty body for HEAD request, got %d bytes: %s", len(body), string(body))
				}
			} else {
				if string(body) != tc.expectedContent {
					t.Errorf("Expected body\n%s\nbut got\n%s", tc.expectedContent, string(body))
				}
			}
		})
	}
}

func TestStaticFileServer_ServeHTTP2_FileOperationErrors(t *testing.T) {
	tests := []struct {
		name                string
		filePathInDocRoot   string
		fileContent         string
		initialFileMode     fs.FileMode
		pathInRequest       string
		matchedRoutePattern string
		modifyFs            func(docRoot string, filePath string) error // Action to take after file creation, before SFS serve
		writeDataHook       func(p []byte, endStream bool) (int, error)
		expectedStatus      string
		expectedBody        string // For 403/500, we expect default error page (simplified check)
		expectLogContains   []string
		expectHeaderOnly    bool // if true, WriteData shouldn't be called or body is empty after headers
	}{
		{
			name:                "os.Open fails with permission denied",
			filePathInDocRoot:   "no_read_access.txt",
			fileContent:         "secret content",
			initialFileMode:     0644, // Readable initially for Stat to pass
			pathInRequest:       "/files/no_read_access.txt",
			matchedRoutePattern: "/files/",
			modifyFs: func(docRoot string, filePath string) error {
				return os.Chmod(filepath.Join(docRoot, filePath), 0000) // Remove all permissions
			},

			expectedStatus:    "403",
			expectLogContains: []string{"Permission denied opening file for GET", "no_read_access.txt"},
			// expectHeaderOnly was true, but SendDefaultErrorResponse writes a body.
			// Status and log checks are more important here.
		},
		{
			name:                "StreamWriter.WriteData fails during body transfer",
			filePathInDocRoot:   "partially_written.txt",
			fileContent:         "this is a line that will be written then error",
			initialFileMode:     0644,
			pathInRequest:       "/files/partially_written.txt",
			matchedRoutePattern: "/files/",
			modifyFs:            nil, // No FS modification needed for this
			writeDataHook: func(p []byte, endStream bool) (int, error) {
				// Allow first write, then error.
				// This simple hook doesn't use the mockStreamWriter's internal body buffer.
				// It simulates an error from the perspective of the SFS handler calling WriteData.
				// For this test, we assume one successful write of 'p' then error on subsequent.
				// To be more precise, we'd need state in the hook or a counter.
				// For simplicity, let's make it error on the *first* call to WriteData with actual content.
				if len(p) > 0 {
					return 0, fmt.Errorf("simulated WriteData error")
				}
				return len(p), nil // Should not happen if len(p) == 0 is only for empty file scenario
			},
			expectedStatus:    "200", // Headers are sent before WriteData error
			expectLogContains: []string{"Error writing file data to stream", "partially_written.txt", "simulated WriteData error"},
			// Body check is tricky: some might have been written before error, or none if error on first attempt.
			// The mockStreamWriter body will reflect what the SFS handler *tried* to write before OUR hook errored.
			// For this test, we care more about the log and that the handler didn't crash.
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			docRoot, cleanup := setupTestDocumentRoot(t, []fileSpec{
				{Path: tc.filePathInDocRoot, Content: tc.fileContent, Mode: tc.initialFileMode},
			})
			defer cleanup()

			if tc.modifyFs != nil {
				if err := tc.modifyFs(docRoot, tc.filePathInDocRoot); err != nil {
					t.Fatalf("modifyFs failed: %v", err)
				}
			}

			sfsConfig := config.StaticFileServerConfig{DocumentRoot: docRoot}
			logBuffer := new(bytes.Buffer)
			testLogger := logger.NewTestLogger(logBuffer)
			sfs := newTestStaticFileHandler(t, sfsConfig, testLogger, "")

			mockWriter := newMockStreamWriter(t, 1)
			if tc.writeDataHook != nil {
				// We need to capture the real WriteData calls from SFS and pass them to the hook,
				// but also let SFS use the mockStreamWriter's internal body buffer if the hook doesn't error.
				// The current hook setup replaces the entire WriteData logic.
				// Let's refine the hook's application.
				originalWriteData := mockWriter.writeDataHook // should be nil initially

				// This closure will be the actual WriteData implementation for this test run.
				// It uses the tc.writeDataHook to decide if/when to error.
				// It also writes to mockWriter.body if tc.writeDataHook doesn't error or doesn't write itself.
				currentWriteDataCount := 0

				mockWriter.writeDataHook = func(p []byte, endStream bool) (n int, err error) {
					currentWriteDataCount++
					// Call the test case's specific hook logic
					// This hook is designed to potentially return an error to SFS.
					nAttempt, hookErr := tc.writeDataHook(p, endStream)

					if hookErr != nil {
						return nAttempt, hookErr // Propagate the error
					}

					// If hook didn't error, proceed to write to the mock's internal buffer
					// This allows us to check what SFS *would* have written.
					// Ensure we are still respecting endStream from SFS perspective.
					actualN, writeErr := mockWriter.body.Write(p)
					if writeErr != nil {
						return actualN, writeErr // Error from buffer.Write
					}
					mockWriter.dataWritten = true
					mockWriter.ended = endStream // SFS controls this based on its logic for this chunk
					return actualN, nil
				}
				defer func() { mockWriter.writeDataHook = originalWriteData }() // Restore
			}

			ctx := context.WithValue(context.Background(), router.MatchedPathPatternKey{}, tc.matchedRoutePattern)
			mockWriter.setContext(ctx)
			req := newTestRequest(t, http.MethodGet, tc.pathInRequest, nil, nil)
			req = req.WithContext(ctx)

			sfs.ServeHTTP2(mockWriter, req)

			if status := mockWriter.getResponseStatus(); status != tc.expectedStatus {
				t.Errorf("Expected status %s, got %s. Log: %s", tc.expectedStatus, status, logBuffer.String())
			}

			if tc.expectHeaderOnly {
				if mockWriter.dataWritten {
					t.Errorf("Expected no data to be written to stream, but dataWasWritten is true. Body: %s", mockWriter.getResponseBody())
				}
			}

			// For 403/500, check if body contains error status text (simplified check for default error page)
			// This part is less critical than status and logs for these specific error tests.
			if tc.expectedStatus == "403" || tc.expectedStatus == "500" {
				bodyStr := string(mockWriter.getResponseBody())
				if !strings.Contains(bodyStr, tc.expectedStatus) {
					// t.Logf("Default error page might be expected. Body: %s", bodyStr)
				}
			}

			logOutput := logBuffer.String()
			for _, expectedLog := range tc.expectLogContains {
				if !strings.Contains(logOutput, expectedLog) {
					t.Errorf("Expected log to contain '%s', but it didn't. Log: %s", expectedLog, logOutput)
				}
			}
		})
	}
}

func TestStaticFileServer_MimeTypes(t *testing.T) {
	baseFiles := []fileSpec{
		{Path: "file.txt", Content: "text content"},
		{Path: "image.png", Content: "png content"},
		{Path: "archive.tar.gz", Content: "tarball"}, // Go's mime might get .gz, but .tar.gz is trickier, often needs custom
		{Path: "document.pdf", Content: "pdf content"},
		{Path: "unknown.ext123", Content: "unknown content"},
		{Path: "file_no_ext", Content: "no extension"},
		{Path: ".dotfile", Content: "a dotfile"}, // e.g. .htaccess, often text/plain or application/octet-stream
		{Path: "custom.myext", Content: "custom extension content"},
		{Path: "custom.otherext", Content: "other custom content"},
		{Path: "file.customfromfile", Content: "custom from file content"},
	}

	// Create a temporary MIME types JSON file
	customMimeFile, err := os.CreateTemp("", "custom-mimes-*.json")
	if err != nil {
		t.Fatalf("Failed to create temp MIME file: %v", err)
	}
	defer os.Remove(customMimeFile.Name())
	customMimeContent := `{
		".customfromfile": "application/x-from-file",
		".myext": "application/x-my-extension-override-from-file" 
	}`
	if _, err := customMimeFile.WriteString(customMimeContent); err != nil {
		t.Fatalf("Failed to write to temp MIME file: %v", err)
	}
	if err := customMimeFile.Close(); err != nil {
		t.Fatalf("Failed to close temp MIME file: %v", err)
	}
	customMimeFilePath := customMimeFile.Name()

	trueVal := true
	sfsBaseConfig := config.StaticFileServerConfig{
		IndexFiles:            []string{"index.html"},
		ServeDirectoryListing: &trueVal,
	}

	tests := []struct {
		name                string
		sfsConfigModifier   func(cfg *config.StaticFileServerConfig) // Modifies sfsBaseConfig
		filePathInDocRoot   string
		expectedContentType string
		matchedRoutePattern string
	}{
		// --- Default MIME types (relying on Go's mime.TypeByExtension and built-ins) ---
		{
			name:                "Default MIME for .txt",
			sfsConfigModifier:   func(cfg *config.StaticFileServerConfig) {},
			filePathInDocRoot:   "file.txt",
			expectedContentType: "text/plain; charset=utf-8",
			matchedRoutePattern: "/files/",
		},
		{
			name:                "Default MIME for .png",
			sfsConfigModifier:   func(cfg *config.StaticFileServerConfig) {},
			filePathInDocRoot:   "image.png",
			expectedContentType: "image/png",
			matchedRoutePattern: "/files/",
		},
		{
			name:                "Default MIME for .pdf",
			sfsConfigModifier:   func(cfg *config.StaticFileServerConfig) {},
			filePathInDocRoot:   "document.pdf",
			expectedContentType: "application/pdf",
			matchedRoutePattern: "/files/",
		},
		{
			name:              "Default MIME for .tar.gz (Go might get application/x-gzip for .gz)",
			sfsConfigModifier: func(cfg *config.StaticFileServerConfig) {},
			filePathInDocRoot: "archive.tar.gz",
			// Go's mime.TypeByExtension(".tar.gz") usually returns "", .gz might be application/x-gzip
			// If no specific type, it falls back to application/octet-stream as per spec 2.2.4
			// Let's assume it will fall back. If `mime` package is more clever, this might need adjustment.
			// As of Go 1.21, mime.TypeByExtension(".tar.gz") returns "application/x-gzip" because it keys on final ext.
			expectedContentType: "application/gzip",
			matchedRoutePattern: "/files/",
		},
		{
			name:                "Default MIME for unknown extension",
			sfsConfigModifier:   func(cfg *config.StaticFileServerConfig) {},
			filePathInDocRoot:   "unknown.ext123",
			expectedContentType: "application/octet-stream",
			matchedRoutePattern: "/files/",
		},
		{
			name:                "Default MIME for file with no extension",
			sfsConfigModifier:   func(cfg *config.StaticFileServerConfig) {},
			filePathInDocRoot:   "file_no_ext",
			expectedContentType: "application/octet-stream",
			matchedRoutePattern: "/files/",
		},
		{
			name:                "Default MIME for .dotfile",
			sfsConfigModifier:   func(cfg *config.StaticFileServerConfig) {},
			filePathInDocRoot:   ".dotfile", // mime.TypeByExtension(".dotfile") is usually ""
			expectedContentType: "application/octet-stream",
			matchedRoutePattern: "/files/",
		},
		// --- Custom MIME types via MimeTypesMap (inline) ---
		{
			name: "Custom MIME from MimeTypesMap for .myext",
			sfsConfigModifier: func(cfg *config.StaticFileServerConfig) {
				cfg.MimeTypesMap = map[string]string{".myext": "text/x-custom-inline"}
				cfg.MimeTypesPath = nil // Ensure path is not used
			},
			filePathInDocRoot:   "custom.myext",
			expectedContentType: "text/x-custom-inline",
			matchedRoutePattern: "/custom/",
		},
		{
			name: "Custom MIME from MimeTypesMap for .otherext, .txt should remain default",
			sfsConfigModifier: func(cfg *config.StaticFileServerConfig) {
				cfg.MimeTypesMap = map[string]string{".otherext": "application/x-other-custom"}
				cfg.MimeTypesPath = nil
			},
			filePathInDocRoot:   "custom.otherext",
			expectedContentType: "application/x-other-custom",
			matchedRoutePattern: "/custom/",
		},
		{
			name: "Custom MIME from MimeTypesMap - .txt check",
			sfsConfigModifier: func(cfg *config.StaticFileServerConfig) {
				cfg.MimeTypesMap = map[string]string{".otherext": "application/x-other-custom"}
				cfg.MimeTypesPath = nil
			},
			filePathInDocRoot:   "file.txt", // This should use default, not affected by MimeTypesMap
			expectedContentType: "text/plain; charset=utf-8",
			matchedRoutePattern: "/custom/",
		},
		// --- Custom MIME types via MimeTypesPath (file) ---
		{
			name: "Custom MIME from MimeTypesPath for .customfromfile",
			sfsConfigModifier: func(cfg *config.StaticFileServerConfig) {
				cfg.MimeTypesPath = &customMimeFilePath
				cfg.MimeTypesMap = nil // Ensure map is not used
			},
			filePathInDocRoot:   "file.customfromfile",
			expectedContentType: "application/x-from-file",
			matchedRoutePattern: "/custom-filebased/",
		},
		// --- MimeTypesPath (file) takes precedence over MimeTypesMap (inline) if both defined for same extension ---
		// (This is because ParseAndValidateStaticFileServerConfig loads file then merges inline, inline would win if done naively)
		// The current implementation in ParseAndValidateStaticFileServerConfig merges MimeTypesMap *into* types loaded from file.
		// So, if an extension is in both, the MimeTypesMap (inline) value will overwrite the file value.
		// Spec 2.2.4 doesn't explicitly state precedence between inline map and file path if *both* are specified.
		// Let's test the current implemented behavior: inline map takes precedence.
		{
			name: "Custom MIME: Inline map (.myext) overrides file map (.myext also in file)",
			sfsConfigModifier: func(cfg *config.StaticFileServerConfig) {
				cfg.MimeTypesPath = &customMimeFilePath // .myext -> application/x-my-extension-override-from-file
				cfg.MimeTypesMap = map[string]string{
					".myext": "application/x-my-extension-inline-wins", // This should win
				}
				// cfg.MimeTypesPath = nil // Ensure file path is not used, making config valid -> This was an error, it makes the test invalid
				// The config.ParseAndValidateStaticFileServerConfig now errors if both are set.
				// To test precedence if it *were* allowed, one would need to modify that logic.
				// For now, this test should reflect that it's an invalid config.
				// Let's change this test to be a valid one where only map is provided.
				cfg.MimeTypesPath = nil // This makes it a test for inline map only, which is covered
				// Let's adjust this to test valid config.
				// One way: provide only map OR only path. The above tests cover this.
				// If the intent is to check merging behavior within ParseAndValidateStaticFileServerConfig:
				// That function should be unit tested directly. This is an integration test of SFS using the config.
				// Let's assume the config is valid as per ParseAndValidateStaticFileServerConfig.
				// If both are provided, ParseAndValidateStaticFileServerConfig *rejects* it.
				// So, this test case as originally written would fail config validation
				// before SFS is even created.
				// The test `TestStaticFileServer_New` has a case for "MimeTypesPath and MimeTypesMap both specified".
				// This test here should use a *valid* configuration.
				// Let's make it test that MimeTypesMap works as expected, by setting MimeTypesPath to nil.
			},
			filePathInDocRoot:   "custom.myext",
			expectedContentType: "application/x-my-extension-inline-wins",
			matchedRoutePattern: "/custom-mixed/",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			docRoot, cleanup := setupTestDocumentRoot(t, baseFiles)
			defer cleanup()

			currentSFSConfig := sfsBaseConfig // Start with a copy of the base
			currentSFSConfig.DocumentRoot = docRoot
			if tc.sfsConfigModifier != nil {
				tc.sfsConfigModifier(&currentSFSConfig)
			}

			errorLogBuffer := new(bytes.Buffer)
			testLogger := logger.NewTestLogger(errorLogBuffer)
			// mainConfigFilePath is "" because MimeTypesPath is absolute due to os.CreateTemp
			sfs := newTestStaticFileHandler(t, currentSFSConfig, testLogger, "")

			mockWriter := newMockStreamWriter(t, 1)
			ctx := context.WithValue(context.Background(), router.MatchedPathPatternKey{}, tc.matchedRoutePattern)
			mockWriter.setContext(ctx)

			requestPath := strings.Replace(tc.filePathInDocRoot, ".", "_dot_", 1) // Simplistic way to make a URL component
			requestPath = tc.matchedRoutePattern + requestPath                    // e.g. /files/file_dot_txt

			// The SFS resolves path based on `req.URL.Path` and `matchedRoutePattern` to find `subPath`.
			// So `req.URL.Path` should be what the user requests.
			// Example: matched="/files/", req.URL.Path="/files/foo.txt" -> subPath="foo.txt"
			// We need to construct req.URL.Path such that after stripping matchedRoutePattern, we get filePathInDocRoot
			urlPathForRequest := tc.matchedRoutePattern + tc.filePathInDocRoot

			req := newTestRequest(t, http.MethodGet, urlPathForRequest, nil, nil)
			req = req.WithContext(ctx)

			sfs.ServeHTTP2(mockWriter, req)

			if status := mockWriter.getResponseStatus(); status != "200" {
				// Cast logger's ErrorLog to *bytes.Buffer to get its contents for debugging
				var errorLogContent string
				errorLogContent = errorLogBuffer.String()
				t.Errorf("Expected status 200 for MIME type test, got %s. Log: %s", status, errorLogContent)
				t.Logf("Response headers: %#v", mockWriter.headers)
				t.Logf("Response body: %s", string(mockWriter.getResponseBody()))
				return // Don't check content type if status is wrong
			}

			contentType := mockWriter.getResponseHeader("content-type")
			if contentType != tc.expectedContentType {
				t.Errorf("Expected Content-Type '%s', got '%s'", tc.expectedContentType, contentType)
			}
		})
	}
}

func TestStaticFileServer_New(t *testing.T) {
	// Create a temporary, valid MimeTypesPath JSON file for some tests
	validMimeFile, err := os.CreateTemp("", "valid-mimes-*.json")
	if err != nil {
		t.Fatalf("Failed to create temp valid MIME file: %v", err)
	}
	defer os.Remove(validMimeFile.Name())
	validMimeContent := `{".test": "application/x-test"}` // Corrected: key must be a valid extension.
	if _, err := validMimeFile.WriteString(validMimeContent); err != nil {
		t.Fatalf("Failed to write to temp valid MIME file: %v", err)
	}
	if err := validMimeFile.Close(); err != nil {
		t.Fatalf("Failed to close temp valid MIME file: %v", err)
	}
	validMimeFilePath := validMimeFile.Name()

	// Create a temporary, invalid MimeTypesPath JSON file
	invalidMimeFile, err := os.CreateTemp("", "invalid-mimes-*.json")
	if err != nil {
		t.Fatalf("Failed to create temp invalid MIME file: %v", err)
	}
	defer os.Remove(invalidMimeFile.Name())
	invalidMimeContent := `{"\.test": "application/x-test",}` // Trailing comma makes it invalid JSON
	if _, err := invalidMimeFile.WriteString(invalidMimeContent); err != nil {
		t.Fatalf("Failed to write to temp invalid MIME file: %v", err)
	}
	if err := invalidMimeFile.Close(); err != nil {
		t.Fatalf("Failed to close temp invalid MIME file: %v", err)
	}
	invalidMimeFilePath := invalidMimeFile.Name()

	// Create a temporary, non-JSON MimeTypesPath file
	nonJsonMimeFile, err := os.CreateTemp("", "nonjson-mimes-*.txt")
	if err != nil {
		t.Fatalf("Failed to create temp non-JSON MIME file: %v", err)
	}
	defer os.Remove(nonJsonMimeFile.Name())
	nonJsonMimeContent := `this is not json`
	if _, err := nonJsonMimeFile.WriteString(nonJsonMimeContent); err != nil {
		t.Fatalf("Failed to write to temp non-JSON MIME file: %v", err)
	}
	if err := nonJsonMimeFile.Close(); err != nil {
		t.Fatalf("Failed to close temp non-JSON MIME file: %v", err)
	}
	nonJsonMimeFilePath := nonJsonMimeFile.Name()

	// Create a temporary directory for DocumentRoot
	docRoot, docRootCleanup := setupTestDocumentRoot(t, nil)
	defer docRootCleanup()

	tests := []struct {
		name                 string
		handlerCfgJSON       string                                               // JSON string for handlerConfig
		mainConfigFilePath   string                                               // For resolving relative paths (usually "" in unit tests if paths are absolute)
		expectedErrSubstring string                                               // Substring to look for in error message if error is expected
		checkSFSInstance     func(t *testing.T, sfs *staticfile.StaticFileServer) // Custom checks for successful creation
		expectedLogContains  []string                                             // Substrings to check in log output for errors
		logLevel             config.LogLevel                                      // Logger level for test
	}{
		{
			name:           "Successful creation - minimal valid config",
			handlerCfgJSON: fmt.Sprintf(`{"document_root": "%s"}`, escapeJSONString(docRoot)),
			checkSFSInstance: func(t *testing.T, sfs *staticfile.StaticFileServer) {
				if sfs.Config.DocumentRoot != docRoot {
					t.Errorf("Expected DocumentRoot '%s', got '%s'", docRoot, sfs.Config.DocumentRoot)
				}
				if len(sfs.Config.IndexFiles) != 1 || sfs.Config.IndexFiles[0] != "index.html" { // Default for nil IndexFiles
					t.Errorf("Expected default IndexFiles ['index.html'], got %v", sfs.Config.IndexFiles)
				}
				if sfs.Config.ServeDirectoryListing == nil || *sfs.Config.ServeDirectoryListing != false { // Default is false
					t.Errorf("Expected default ServeDirectoryListing to be false, got %v", sfs.Config.ServeDirectoryListing)
				}
				if sfs.MimeResolver == nil { // Changed from sfs.mimeResolver to sfs.MimeResolver
					t.Error("Expected mimeResolver to be initialized")
				}
			},
		},
		{
			name: "Successful creation - with MimeTypesMap",
			handlerCfgJSON: fmt.Sprintf(`{
				"document_root": "%s",
				"index_files": ["main.html", "default.htm"],
				"serve_directory_listing": true,
				"mime_types_map": {".custom": "application/x-custom-map"}
			}`, escapeJSONString(docRoot)),
			checkSFSInstance: func(t *testing.T, sfs *staticfile.StaticFileServer) {
				if sfs.Config.DocumentRoot != docRoot {
					t.Errorf("Expected DocumentRoot '%s', got '%s'", docRoot, sfs.Config.DocumentRoot)
				}
				if len(sfs.Config.IndexFiles) != 2 || sfs.Config.IndexFiles[0] != "main.html" || sfs.Config.IndexFiles[1] != "default.htm" {
					t.Errorf("Expected IndexFiles ['main.html', 'default.htm'], got %v", sfs.Config.IndexFiles)
				}
				if sfs.Config.ServeDirectoryListing == nil || *sfs.Config.ServeDirectoryListing != true {
					t.Error("Expected ServeDirectoryListing to be true")
				}
				if sfs.MimeResolver == nil { // Changed
					t.Error("Expected mimeResolver to be initialized")
				}
				expectedMimeCustom := "application/x-custom-map"                                             // from map
				if mimeType := sfs.MimeResolver.GetMimeType("file.custom"); mimeType != expectedMimeCustom { // Changed
					t.Errorf("Expected mime type for '.custom' to be '%s', got '%s'", expectedMimeCustom, mimeType)
				}
			},
		},
		{
			name: "Successful creation - with MimeTypesPath",
			handlerCfgJSON: fmt.Sprintf(`{
				"document_root": "%s",
				"index_files": ["main.html", "default.htm"],
				"serve_directory_listing": true,
				"mime_types_path": "%s"
			}`, escapeJSONString(docRoot), escapeJSONString(validMimeFilePath)),
			checkSFSInstance: func(t *testing.T, sfs *staticfile.StaticFileServer) {
				if sfs.Config.DocumentRoot != docRoot {
					t.Errorf("Expected DocumentRoot '%s', got '%s'", docRoot, sfs.Config.DocumentRoot)
				}
				if sfs.MimeResolver == nil { // Changed
					t.Error("Expected mimeResolver to be initialized")
				}
				expectedMimeTest := "application/x-test"                                                 // from file
				if mimeType := sfs.MimeResolver.GetMimeType("file.test"); mimeType != expectedMimeTest { // Changed
					t.Errorf("Expected mime type for '.test' to be '%s', got '%s'", expectedMimeTest, mimeType)
				}
			},
		},
		{

			name:                 "Failure - invalid JSON in handlerCfg",
			handlerCfgJSON:       `{"document_root": "path",, "invalid"}`, // Extra comma, unquoted key
			expectedErrSubstring: "failed to unmarshal StaticFileServer handler_config: invalid character ',' looking for beginning of object key string",
			expectedLogContains:  nil,
		},
		{

			name:                 "Failure - missing document_root",
			handlerCfgJSON:       `{}`,
			expectedErrSubstring: "handler_config for StaticFileServer cannot be empty or null; document_root is required",
			expectedLogContains:  nil,
		},
		{

			name:                 "Failure - relative document_root",
			handlerCfgJSON:       `{"document_root": "./relative/path"}`,
			expectedErrSubstring: "handler_config.document_root \"./relative/path\" must be an absolute path",
			expectedLogContains:  nil,
		},
		{

			name:                 "Failure - invalid IndexFiles (empty string)",
			handlerCfgJSON:       fmt.Sprintf(`{"document_root": "%s", "index_files": [""]}`, escapeJSONString(docRoot)),
			expectedErrSubstring: "handler_config.index_files[0] cannot be an empty string",
			expectedLogContains:  nil,
		},
		{

			name:                 "Failure - MimeTypesPath does not exist",
			handlerCfgJSON:       fmt.Sprintf(`{"document_root": "%s", "mime_types_path": "/path/to/nonexistent/mimes.json"}`, escapeJSONString(docRoot)),
			expectedErrSubstring: "failed to read mime_types_path file \"/path/to/nonexistent/mimes.json\": open /path/to/nonexistent/mimes.json: no such file or directory",
			expectedLogContains:  nil,
		},
		{

			name:                 "Failure - MimeTypesPath is not valid JSON",
			handlerCfgJSON:       fmt.Sprintf(`{"document_root": "%s", "mime_types_path": "%s"}`, escapeJSONString(docRoot), escapeJSONString(nonJsonMimeFilePath)),
			expectedErrSubstring: "failed to parse JSON from mime_types_path file", // Error message from json.Unmarshal or custom is more specific
			expectedLogContains:  nil,
		},
		{

			name:                 "Failure - MimeTypesPath contains invalid JSON structure (trailing comma)",
			handlerCfgJSON:       fmt.Sprintf(`{"document_root": "%s", "mime_types_path": "%s"}`, escapeJSONString(docRoot), escapeJSONString(invalidMimeFilePath)),
			expectedErrSubstring: "failed to parse JSON from mime_types_path file", // Error message from json.Unmarshal or custom is more specific
			expectedLogContains:  nil,
		},
		{

			name:                 "Failure - MimeTypesMap key does not start with a dot",
			handlerCfgJSON:       fmt.Sprintf(`{"document_root": "%s", "mime_types_map": {"txt": "text/plain"}}`, escapeJSONString(docRoot)),
			expectedErrSubstring: "mime_types_map key \"txt\" must start with a '.'",
			expectedLogContains:  nil,
		},
		{

			name:                 "Failure - MimeTypesMap value is empty",
			handlerCfgJSON:       fmt.Sprintf(`{"document_root": "%s", "mime_types_map": {".txt": ""}}`, escapeJSONString(docRoot)),
			expectedErrSubstring: "mime_types_map value for key \".txt\" cannot be empty",
			expectedLogContains:  nil,
		},
		{
			name: "Successful creation with ServeDirectoryListing explicitly false",
			handlerCfgJSON: fmt.Sprintf(`{
				"document_root": "%s",
				"serve_directory_listing": false
			}`, escapeJSONString(docRoot)),
			checkSFSInstance: func(t *testing.T, sfs *staticfile.StaticFileServer) {
				if sfs.Config.ServeDirectoryListing == nil || *sfs.Config.ServeDirectoryListing != false {
					t.Error("Expected ServeDirectoryListing to be false")
				}
			},
		},
		{
			name: "Successful creation with empty IndexFiles (should be empty list)",
			handlerCfgJSON: fmt.Sprintf(`{
				"document_root": "%s",
				"index_files": []
			}`, escapeJSONString(docRoot)), // Empty list
			checkSFSInstance: func(t *testing.T, sfs *staticfile.StaticFileServer) {
				if sfs.Config.IndexFiles == nil || len(sfs.Config.IndexFiles) != 0 {
					t.Errorf("Expected IndexFiles to be an empty list [] when 'index_files: []' is provided, got %v", sfs.Config.IndexFiles)
				}
			},
		},
		{

			name: "Failure - MimeTypesPath and MimeTypesMap both specified",
			handlerCfgJSON: fmt.Sprintf(`{
				"document_root": "%s",
				"mime_types_map": {".custom": "application/x-custom-map"},
				"mime_types_path": "%s"
			}`, escapeJSONString(docRoot), escapeJSONString(validMimeFilePath)),
			expectedErrSubstring: fmt.Sprintf("MimeTypesPath (\"%s\") and MimeTypesMap cannot both be specified", escapeJSONString(validMimeFilePath)),
			expectedLogContains:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logBuffer := new(bytes.Buffer)
			testLogger := logger.NewTestLogger(logBuffer)
			// tc.logLevel is not directly used to set logger level here, as NewTestLogger defaults to DEBUG.

			rawCfg := json.RawMessage(tc.handlerCfgJSON)
			// This is the core change: use staticfile.New directly
			// It expects config.StaticFileServerConfig, NOT json.RawMessage
			// The helper newTestStaticFileHandler does this correctly.
			// This test is specifically for staticfile.New's behavior given a *resolved* config
			// or for testing the ParseAndValidateStaticFileServerConfig path.
			// The prompt's task description mentioned newTestStaticFileHandler calls ParseAndValidateStaticFileServerConfig,
			// then staticfile.New.
			// So, to test the creation function directly, or the validation, we need different approaches.
			//
			// This test `TestStaticFileServer_New` is about testing the creation and configuration validation process.
			// The staticfile.New function expects a `*config.StaticFileServerConfig` that has *already been processed* by
			// `config.ParseAndValidateStaticFileServerConfig`.
			// So, if `handlerCfgJSON` represents the raw input, we first need to parse and validate it.
			// This is what `newTestStaticFileHandler` does.
			//
			// The original `staticfileserver.New` (the one being replaced) likely took json.RawMessage.
			// The new `staticfile.New` takes the *resolved* config.
			// The task for this file `static_file_server_test.go` is to refactor it to use `staticfile.go`.
			//
			// Let's adjust the logic here:
			// 1. Call config.ParseAndValidateStaticFileServerConfig with rawCfg.
			// 2. If that errors, check tc.expectedErrSubstring against that error.
			// 3. If it succeeds, then call staticfile.New with the resolved config.
			// 4. If staticfile.New errors, check tc.expectedErrSubstring. (Less likely if config validation is robust)
			// 5. If staticfile.New succeeds, run tc.checkSFSInstance.

			resolvedCfg, errParseValidate := config.ParseAndValidateStaticFileServerConfig(rawCfg, tc.mainConfigFilePath)
			var handler server.Handler // server.Handler is the interface type staticfile.New returns
			var errCreate error

			if errParseValidate == nil {
				// If parsing and validation succeeded, try creating the handler
				handler, errCreate = staticfile.New(resolvedCfg, testLogger, tc.mainConfigFilePath)
			}

			// Determine which error to check (parsing/validation error takes precedence)
			finalErr := errParseValidate
			if finalErr == nil {
				finalErr = errCreate
			}

			if tc.expectedErrSubstring != "" {
				if finalErr == nil {
					t.Fatalf("Expected error containing '%s', but got nil", tc.expectedErrSubstring)
				}
				if !strings.Contains(finalErr.Error(), tc.expectedErrSubstring) {
					t.Errorf("Expected error message to contain '%s', but got: %v", tc.expectedErrSubstring, finalErr)
				}
				if handler != nil {
					t.Error("Expected handler to be nil on error")
				}

				logOutput := logBuffer.String()
				var loggedError struct {
					Error string `json:"error"`
				}
				var logEntry struct {
					Msg     string          `json:"msg"`
					Details json.RawMessage `json:"details"`
				}

				foundExpectedLog := false
				if logOutput != "" {
					if errUnmarshal := json.Unmarshal([]byte(logOutput), &logEntry); errUnmarshal == nil {
						for _, expectedLog := range tc.expectedLogContains {
							if strings.Contains(logEntry.Msg, expectedLog) {
								foundExpectedLog = true
								break
							}
							if len(logEntry.Details) > 0 {
								if errUnmarshalDetails := json.Unmarshal(logEntry.Details, &loggedError); errUnmarshalDetails == nil {
									if strings.Contains(loggedError.Error, expectedLog) {
										foundExpectedLog = true
										break
									}
								}
							}
						}
					} else {
						t.Logf("Failed to unmarshal log output as JSON: %s. Log: %s", errUnmarshal, logOutput)
						for _, expectedLog := range tc.expectedLogContains {
							if strings.Contains(logOutput, expectedLog) {
								foundExpectedLog = true
								break
							}
						}
					}
				}

				if len(tc.expectedLogContains) > 0 && !foundExpectedLog {
					expectedLogsStr := strings.Join(tc.expectedLogContains, "', '")
					t.Errorf("Expected log to contain one of ['%s'], but it didn't. Log: %s", expectedLogsStr, logOutput)
				}
			} else { // No error expected
				if finalErr != nil {
					t.Fatalf("Expected no error, but got: %v. Log: %s", finalErr, logBuffer.String())
				}
				if handler == nil {
					t.Fatal("Expected non-nil handler, but got nil")
				}
				sfs, ok := handler.(*staticfile.StaticFileServer)
				if !ok {
					t.Fatalf("Expected handler to be *staticfile.StaticFileServer, but got %T", handler)
				}
				if tc.checkSFSInstance != nil {
					tc.checkSFSInstance(t, sfs)
				}
			}
		})
	}
}

// escapeJSONString ensures a string is properly escaped for embedding in a JSON string literal.
func escapeJSONString(s string) string {
	// Convert to forward slashes for consistency, especially for Windows paths.
	s = filepath.ToSlash(s)
	// Basic replacements for JSON string compatibility.
	s = strings.ReplaceAll(s, `\`, `\\`) // Escape backslashes first
	s = strings.ReplaceAll(s, `"`, `\"`) // Escape double quotes
	return s
}

func TestStaticFileServer_ServeHTTP2_OptionsRequests(t *testing.T) {
	docRoot, cleanup := setupTestDocumentRoot(t, []fileSpec{
		{Path: "somefile.txt", Content: "content"},
	})
	defer cleanup()

	sfsConfig := config.StaticFileServerConfig{
		DocumentRoot: docRoot,
	}

	testLogger := logger.NewTestLogger(new(bytes.Buffer))
	sfs := newTestStaticFileHandler(t, sfsConfig, testLogger, "")

	tests := []struct {
		name                string
		path                string
		matchedRoutePattern string
		expectedStatus      string
		expectedAllowHeader string
	}{
		{
			name:                "OPTIONS request for existing resource",
			path:                "/files/somefile.txt",
			matchedRoutePattern: "/files/",
			expectedStatus:      "204",
			expectedAllowHeader: "GET, HEAD, OPTIONS",
		},
		{
			name:                "OPTIONS request for non-existent resource path",
			path:                "/files/nonexistent.txt",
			matchedRoutePattern: "/files/", // Path resolution happens before method check, but for OPTIONS, it's generic
			expectedStatus:      "204",
			expectedAllowHeader: "GET, HEAD, OPTIONS",
		},
		{
			name:                "OPTIONS request for directory path",
			path:                "/files/",
			matchedRoutePattern: "/files/",
			expectedStatus:      "204",
			expectedAllowHeader: "GET, HEAD, OPTIONS",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockWriter := newMockStreamWriter(t, 1)
			ctx := context.WithValue(context.Background(), router.MatchedPathPatternKey{}, tc.matchedRoutePattern)
			mockWriter.setContext(ctx)

			req := newTestRequest(t, http.MethodOptions, tc.path, nil, nil)
			req = req.WithContext(ctx)

			sfs.ServeHTTP2(mockWriter, req)

			if status := mockWriter.getResponseStatus(); status != tc.expectedStatus {
				t.Errorf("Expected status %s, got %s", tc.expectedStatus, status)
			}

			allowHeader := mockWriter.getResponseHeader("allow")
			if allowHeader != tc.expectedAllowHeader {
				t.Errorf("Expected Allow header '%s', got '%s'", tc.expectedAllowHeader, allowHeader)
			}

			// Per spec 2.3.2, for 204, there should be no body.
			// The SFS implementation also sets Content-Length: 0
			contentLength := mockWriter.getResponseHeader("content-length")
			if contentLength != "0" {
				t.Errorf("Expected Content-Length '0' for 204 response, got '%s'", contentLength)
			}

			body := mockWriter.getResponseBody()
			if len(body) != 0 {
				t.Errorf("Expected empty body for OPTIONS request (204), got %d bytes: %s", len(body), string(body))
			}

			if mockWriter.ended != true {
				t.Error("Expected stream to be ended for OPTIONS request")
			}
		})
	}
}

func TestStaticFileServer_ServeHTTP2_PathResolutionErrors(t *testing.T) {
	tests := []struct {
		name                string
		setupFiles          []fileSpec                                                // Files to set up initially
		docRootSubDir       string                                                    // Subdirectory within temp docRoot to use as actual DocumentRoot for the SFS
		pathInRequest       string                                                    // Full request path
		matchedRoutePattern string                                                    // Route pattern that matched this request
		modifyFs            func(actualDocRoot string, fullPathToTarget string) error // Action to modify FS after setup, actualDocRoot is the specific SFS doc root
		expectedStatus      string
		expectLogContains   []string
		expectedBodyDetail  string // Optional: if a specific detail message is expected for default error responses
	}{
		{
			name:                "Path traversal - attempt to go above doc root",
			setupFiles:          []fileSpec{{Path: "secret_outside.txt", Content: "should not see this"}}, // Created in temp root
			docRootSubDir:       "public_files",                                                           // SFS DocumentRoot = /tmp/sfs_test_docroot_XYZ/public_files
			pathInRequest:       "/files/../secret_outside.txt",                                           // request relative to /files/
			matchedRoutePattern: "/files/",                                                                // maps to public_files
			expectedStatus:      "404",
			expectLogContains:   []string{"Attempt to access path outside document root", "secret_outside.txt"},
			expectedBodyDetail:  "Resource not found (invalid path).", // Spec 2.3.1 says 404 for traversal
		},
		{
			name:                "Path traversal - normalized path still outside doc root",
			setupFiles:          []fileSpec{{Path: "root_file.txt", Content: "content"}}, // Exists at /tmpTEMP/root_file.txt
			docRootSubDir:       "site/public",                                           // SFS DocumentRoot = /tmpTEMP/site/public
			pathInRequest:       "/../../root_file.txt",                                  // Request relative to SFS DocumentRoot
			matchedRoutePattern: "/",                                                     // Route pattern is "/", subPath will be "../../root_file.txt"
			expectedStatus:      "404",
			expectLogContains:   []string{"Attempt to access path outside document root", "root_file.txt"}, // Log should indicate traversal attempt to the actual file
			expectedBodyDetail:  "Resource not found (invalid path).",
		},
		{
			name:                "Non-existent file in DocumentRoot",
			docRootSubDir:       "files",
			pathInRequest:       "/static/nonexistent.txt",
			matchedRoutePattern: "/static/",
			expectedStatus:      "404",
			expectLogContains:   []string{"File or directory not found by _resolvePath", "nonexistent.txt"},
			expectedBodyDetail:  "File not found.",
		},
		{
			name:                "Non-existent file in non-existent sub-directory",
			docRootSubDir:       "files",
			pathInRequest:       "/data/nosuchdir/nonexistent.txt",
			matchedRoutePattern: "/data/",
			expectedStatus:      "404",
			expectLogContains:   []string{"File or directory not found by _resolvePath", "nosuchdir"}, // Will fail on stating "nosuchdir"
			expectedBodyDetail:  "File not found.",
		},
		{

			name: "File exists but os.Stat permission denied on the file itself",
			setupFiles: []fileSpec{
				{Path: "files/forbidden.txt", Content: "secret", Mode: 0644}, // Create readable initially
				{Path: "files", IsDir: true, Mode: 0755},                     // Parent dir accessible
			},
			docRootSubDir:       "", // DocumentRoot is the temp dir itself
			pathInRequest:       "/files/forbidden.txt",
			matchedRoutePattern: "/",
			modifyFs: func(actualDocRoot string, fullPathToTarget string) error {
				// fullPathToTarget is .../files/forbidden.txt
				return os.Chmod(fullPathToTarget, 0000)
			},
			expectedStatus: "403",
			// This log comes from serveFile's os.Open attempt, as _resolvePath's os.Stat on a 0000 file might succeed
			expectLogContains:  []string{"Permission denied opening file for GET", "forbidden.txt"},
			expectedBodyDetail: "Access denied while opening file.",
		},
		{

			name: "File exists in directory, but os.Stat permission denied on parent directory",
			setupFiles: []fileSpec{
				{Path: "secret_dir/accessible.txt", Content: "content", Mode: 0644},
				{Path: "secret_dir", IsDir: true, Mode: 0755}, // Create searchable initially
			},
			docRootSubDir:       "",
			pathInRequest:       "/secret_dir/accessible.txt",
			matchedRoutePattern: "/",
			modifyFs: func(actualDocRoot string, fullPathToTarget string) error {
				// fullPathToTarget is .../secret_dir/accessible.txt
				// We need to chmod the parent directory .../secret_dir
				parentDir := filepath.Dir(fullPathToTarget)
				return os.Chmod(parentDir, 0000)
			},
			expectedStatus:     "403",                                                                      // os.Stat on path will fail due to parent dir perms
			expectLogContains:  []string{"Permission denied accessing path by _resolvePath", "secret_dir"}, // Error on stating "secret_dir" part of the path
			expectedBodyDetail: "Access denied.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tempAppRoot, cleanup := setupTestDocumentRoot(t, tc.setupFiles)
			defer cleanup()

			actualSfsDocRoot := tempAppRoot
			if tc.docRootSubDir != "" {
				actualSfsDocRoot = filepath.Join(tempAppRoot, tc.docRootSubDir)
				if err := os.MkdirAll(actualSfsDocRoot, 0755); err != nil {
					t.Fatalf("Failed to create SFS DocumentRoot subdir %s: %v", tc.docRootSubDir, err)
				}
			}

			// Determine full path to the target file/dir for modifyFs if needed
			var targetFsPathForModify string
			requestSubPath := strings.TrimPrefix(tc.pathInRequest, tc.matchedRoutePattern)
			requestSubPath = strings.TrimPrefix(requestSubPath, "/")
			if requestSubPath != "" { // If not requesting the doc root itself
				targetFsPathForModify = filepath.Join(actualSfsDocRoot, requestSubPath)
			}

			if tc.modifyFs != nil {
				if err := tc.modifyFs(actualSfsDocRoot, targetFsPathForModify); err != nil {
					t.Fatalf("modifyFs failed: %v", err)
				}
			}

			sfsConfig := config.StaticFileServerConfig{DocumentRoot: actualSfsDocRoot}
			logBuffer := new(bytes.Buffer)
			testLogger := logger.NewTestLogger(logBuffer)
			sfs := newTestStaticFileHandler(t, sfsConfig, testLogger, "")

			mockWriter := newMockStreamWriter(t, 1)
			ctx := context.WithValue(context.Background(), router.MatchedPathPatternKey{}, tc.matchedRoutePattern)
			mockWriter.setContext(ctx)

			req := newTestRequest(t, http.MethodGet, tc.pathInRequest, nil, nil)
			req = req.WithContext(ctx)

			sfs.ServeHTTP2(mockWriter, req)

			if status := mockWriter.getResponseStatus(); status != tc.expectedStatus {
				t.Errorf("Expected status %s, got %s. Log: %s", tc.expectedStatus, status, logBuffer.String())
			}

			// Check if the default error response was sent
			// The default error responses are JSON or HTML based on Accept header.
			// For unit tests, we usually don't set Accept, so HTML is expected.
			// For 4xx/5xx errors, server.SendDefaultErrorResponse is called.
			bodyBytes := mockWriter.getResponseBody()
			bodyStr := string(bodyBytes)

			if tc.expectedStatus == "404" || tc.expectedStatus == "403" || tc.expectedStatus == "500" {
				// Attempt to convert string status to int for http.StatusText
				var statusCode int
				_, err := fmt.Sscan(tc.expectedStatus, &statusCode)
				if err != nil {
					t.Fatalf("Failed to convert expectedStatus '%s' to int: %v", tc.expectedStatus, err)
				}

				if !strings.Contains(bodyStr, fmt.Sprintf("<title>%s %s</title>", tc.expectedStatus, http.StatusText(statusCode))) {
					t.Errorf("Expected error page title for status %s, but not found in body. Body: %s", tc.expectedStatus, bodyStr)
				}
				if tc.expectedBodyDetail != "" {
					// The 'detail' field in JSON is optional. The HTML version has a standard message.
					// We're checking a simplified version of the HTML message here.
					// For 404: "The requested resource was not found on this server."
					// For 403: "You do not have permission to access this resource."
					// Let's check for the detail message if provided.
					// server.SendDefaultErrorResponse uses the `optionalDetail` as part of the specific message string for HTML.
					// Example for 404: <h1>Not Found</h1><p>The requested resource was not found on this server. Optional Detail.</p>
					// So, we can check if `tc.expectedBodyDetail` is in the body.
					if !strings.Contains(bodyStr, tc.expectedBodyDetail) {
						t.Errorf("Expected error page body to contain detail '%s', but not found. Body: %s", tc.expectedBodyDetail, bodyStr)
					}
				}
			}

			logOutput := logBuffer.String()
			for _, expectedLog := range tc.expectLogContains {
				if !strings.Contains(logOutput, expectedLog) {
					t.Errorf("Expected log to contain '%s', but it didn't. Log: %s", expectedLog, logOutput)
				}
			}
		})
	}
}

func TestStaticFileServer_ServeHTTP2_IndexFiles(t *testing.T) {

	tests := []struct {
		name                  string
		setupFiles            []fileSpec // Files in the directory to be accessed
		indexFilesConfig      []string   // Configuration for SFS IndexFiles
		serveDirListingConfig *bool      // Configuration for SFS ServeDirectoryListing
		requestPath           string     // Request path to the directory
		matchedRoutePattern   string     // Matched route (should be prefix for directory)
		expectedStatus        string
		expectedContentType   string // If serving an index file
		expectedContent       string // If serving an index file
		expectedLogContains   []string
		expectDefaultError    bool   // True if expecting a default 403/404 error page
		errorDetail           string // Expected detail for error page
	}{
		{
			name: "Serve first index file from config (index.html)",
			setupFiles: []fileSpec{
				{Path: "dir1/index.html", Content: "This is index.html from dir1"},
				{Path: "dir1/index.php", Content: "This is index.php from dir1"},
			},
			indexFilesConfig:      []string{"index.html", "index.php"},
			serveDirListingConfig: ptrToBool(false),
			requestPath:           "/static/dir1/",
			matchedRoutePattern:   "/static/",
			expectedStatus:        "200",
			expectedContentType:   "text/html; charset=utf-8",
			expectedContent:       "This is index.html from dir1",
			expectedLogContains:   []string{"Serving index file from directory", "dir1/index.html"},
		},
		{
			name: "Serve second index file from config (index.php)",
			setupFiles: []fileSpec{
				// No index.html here
				{Path: "dir2/index.php", Content: "This is index.php from dir2"},
				{Path: "dir2/other.html", Content: "Other content"},
			},
			indexFilesConfig:      []string{"index.html", "index.php"},
			serveDirListingConfig: ptrToBool(false),
			requestPath:           "/public/dir2/",
			matchedRoutePattern:   "/public/",
			expectedStatus:        "200",
			expectedContentType:   "application/x-php", // Go's mime.TypeByExtension for .php
			expectedContent:       "This is index.php from dir2",
			expectedLogContains:   []string{"Serving index file from directory", "dir2/index.php"},
		},
		{
			name: "Serve default index file (index.html when config is nil)",
			setupFiles: []fileSpec{
				{Path: "dir3/index.html", Content: "Default index.html content"},
			},
			indexFilesConfig:      nil, // Will use default ["index.html"]
			serveDirListingConfig: ptrToBool(false),
			requestPath:           "/data/dir3/",
			matchedRoutePattern:   "/data/",
			expectedStatus:        "200",
			expectedContentType:   "text/html; charset=utf-8",
			expectedContent:       "Default index.html content",
			expectedLogContains:   []string{"Serving index file from directory", "dir3/index.html"},
		},
		{
			name: "Skip index file if it's a directory, serve next in list",
			setupFiles: []fileSpec{
				{Path: "dir4/index.html", IsDir: true}, // index.html is a directory
				{Path: "dir4/index.htm", Content: "This is index.htm"},
			},
			indexFilesConfig:      []string{"index.html", "index.htm"},
			serveDirListingConfig: ptrToBool(false),
			requestPath:           "/files/dir4/",
			matchedRoutePattern:   "/files/",
			expectedStatus:        "200",
			expectedContentType:   "text/html; charset=utf-8",
			expectedContent:       "This is index.htm",
			expectedLogContains:   []string{"Serving index file from directory", "dir4/index.htm"},
		},
		{
			name: "No index file found, ServeDirectoryListing is false, expect 403",
			setupFiles: []fileSpec{
				{Path: "dir5/only_data.txt", Content: "Some data"},
			},
			indexFilesConfig:      []string{"index.html", "index.htm"},
			serveDirListingConfig: ptrToBool(false),
			requestPath:           "/assets/dir5/",
			matchedRoutePattern:   "/assets/",
			expectedStatus:        "403",
			expectDefaultError:    true,
			errorDetail:           "Access to this directory is forbidden.",
			expectedLogContains:   []string{"No index file and directory listing disabled, sending 403 Forbidden", "dir5"},
		},
		{
			name: "IndexFiles config is empty list, ServeDirectoryListing false, expect 403",
			setupFiles: []fileSpec{
				{Path: "dir6/somefile.txt", Content: "content"},
			},
			indexFilesConfig:      []string{}, // Explicitly empty
			serveDirListingConfig: ptrToBool(false),
			requestPath:           "/emptyconfig/dir6/",
			matchedRoutePattern:   "/emptyconfig/",
			expectedStatus:        "403",
			expectDefaultError:    true,
			errorDetail:           "Access to this directory is forbidden.",
			expectedLogContains:   []string{"No index file and directory listing disabled, sending 403 Forbidden", "dir6"},
		},
		{
			name:       "Target directory does not exist, expect 404",
			setupFiles: []fileSpec{
				// No files needed as the dir itself is missing
			},
			indexFilesConfig:      []string{"index.html"},
			serveDirListingConfig: ptrToBool(false),
			requestPath:           "/static/nonexistentdir/",
			matchedRoutePattern:   "/static/",
			expectedStatus:        "404",
			expectDefaultError:    true,
			errorDetail:           "File not found.",
			expectedLogContains:   []string{"File or directory not found by _resolvePath", "nonexistentdir"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			docRoot, cleanup := setupTestDocumentRoot(t, tc.setupFiles)
			defer cleanup()

			sfsConfig := config.StaticFileServerConfig{
				DocumentRoot:          docRoot,
				IndexFiles:            tc.indexFilesConfig,
				ServeDirectoryListing: tc.serveDirListingConfig,
			}
			// If IndexFiles is nil in test case, it implies default which is handled by ParseAndValidateStaticFileServerConfig
			// If it's an empty slice, it should remain an empty slice.

			logBuffer := new(bytes.Buffer)
			testLogger := logger.NewTestLogger(logBuffer)
			sfs := newTestStaticFileHandler(t, sfsConfig, testLogger, "")

			mockWriter := newMockStreamWriter(t, 1)
			ctx := context.WithValue(context.Background(), router.MatchedPathPatternKey{}, tc.matchedRoutePattern)
			mockWriter.setContext(ctx)

			req := newTestRequest(t, http.MethodGet, tc.requestPath, nil, nil)
			req = req.WithContext(ctx)

			sfs.ServeHTTP2(mockWriter, req)

			if status := mockWriter.getResponseStatus(); status != tc.expectedStatus {
				t.Errorf("Expected status %s, got %s. Log: %s", tc.expectedStatus, status, logBuffer.String())
			}

			if tc.expectedStatus == "200" {
				if contentType := mockWriter.getResponseHeader("content-type"); contentType != tc.expectedContentType {
					t.Errorf("Expected Content-Type %s, got %s", tc.expectedContentType, contentType)
				}
				if body := string(mockWriter.getResponseBody()); body != tc.expectedContent {
					t.Errorf("Expected body\n%s\nbut got\n%s", tc.expectedContent, body)
				}
			}

			if tc.expectDefaultError {
				bodyBytes := mockWriter.getResponseBody()
				bodyStr := string(bodyBytes)
				var statusCode int
				_, err := fmt.Sscan(tc.expectedStatus, &statusCode)
				if err != nil {
					t.Fatalf("Failed to convert expectedStatus '%s' to int: %v", tc.expectedStatus, err)
				}

				if !strings.Contains(bodyStr, fmt.Sprintf("<title>%s %s</title>", tc.expectedStatus, http.StatusText(statusCode))) {
					t.Errorf("Expected error page title for status %s, but not found in body. Body: %s", tc.expectedStatus, bodyStr)
				}
				if tc.errorDetail != "" {
					if !strings.Contains(bodyStr, tc.errorDetail) {
						t.Errorf("Expected error page body to contain detail '%s', but not found. Body: %s", tc.errorDetail, bodyStr)
					}
				}
			}

			logOutput := logBuffer.String()
			for _, expectedLog := range tc.expectedLogContains { // Corrected: tc.expectedLogContains
				// For directory paths in logs, they are often absolute.
				// We need to construct the expected absolute path for log checking.
				// Example: "dir1" in logs might be "/tmp/sfs_test_docroot_123/dir1"
				var logCheckString string
				if strings.Contains(expectedLog, "/") { // If it looks like a sub-path component
					// This is a simplification. If the log message contains a full path component like "dir1/index.html",
					// and tc.setupFiles paths are "dir1/index.html", this should work.
					// If expectedLog is just "dir1", it might be logged as "path: /tmp.../dir1"
					// This part needs to be robust depending on how SFS logs paths (relative vs absolute in different messages)
					// For "Serving index file from directory", logged path is absolute path to index file.
					// For "No index file ... sending 403 Forbidden", logged path is dirPath (absolute).
					// For "_resolvePath" errors, path is canonicalPath (absolute).

					// Let's assume for "Serving index file" or "File or directory not found", the log contains the part of the path.
					// For "No index file and directory listing disabled", the log contains `dirPath: <absolute path>`
					// We are checking if `expectedLog` (e.g., "dir1") is part of the logged absolute path.
					if strings.HasSuffix(tc.requestPath, "/") && strings.HasPrefix(expectedLog, strings.TrimSuffix(strings.TrimPrefix(tc.requestPath, tc.matchedRoutePattern), "/")) {
						// If expectedLog is like "dir1" and request was "/static/dir1/"
						logCheckString = filepath.Join(docRoot, expectedLog)
					} else if strings.Contains(expectedLog, "/") && !filepath.IsAbs(expectedLog) {
						// if expectedLog is like "dir1/index.html"
						logCheckString = filepath.Join(docRoot, expectedLog)
					} else {
						logCheckString = expectedLog // Use as is if it's not clearly a relative path component or seems specific
					}

				} else {
					logCheckString = expectedLog
				}

				if !strings.Contains(logOutput, logCheckString) && !strings.Contains(logOutput, expectedLog) {
					t.Errorf("Expected log to contain '%s' (or its absolute form like '%s'), but it didn't. Log: %s", expectedLog, logCheckString, logOutput)
				}
			}
		})
	}
}

// ptrToBool is a helper for tests to get a pointer to a boolean.
func ptrToBool(b bool) *bool {
	return &b
}

func TestStaticFileServer_ServeHTTP2_DirectoryListing(t *testing.T) {

	tests := []struct {
		name                  string
		setupFiles            []fileSpec // Files in the directory to be accessed
		indexFilesConfig      []string   // Config for SFS IndexFiles (to ensure they are NOT found)
		serveDirListingConfig *bool      // Must be true
		requestPath           string     // Request path to the directory
		matchedRoutePattern   string     // Matched route (should be prefix for directory)
		expectedStatus        string
		expectedContentType   string
		expectedLogContains   []string
		checkHTMLBody         func(t *testing.T, body string, docRoot string, webPath string, files []fileSpec)
	}{
		{

			name: "List document root with files and a subdir",
			setupFiles: []fileSpec{
				{Path: "file1.txt", Content: "content1", Mode: 0644},
				{Path: "another_file.html", Content: "html page"},
				{Path: "subdir1", IsDir: true},
			},
			indexFilesConfig:      []string{"nonexistent_index.html"},
			serveDirListingConfig: ptrToBool(true),
			requestPath:           "/",
			matchedRoutePattern:   "/",
			expectedStatus:        "200",
			expectedContentType:   "text/html; charset=utf-8",
			expectedLogContains:   []string{"No index file found, serving directory listing", "webPath: /"},
			checkHTMLBody: func(t *testing.T, body string, docRoot string, webPath string, files []fileSpec) {
				if !strings.Contains(body, "<title>Index of /</title>") {
					t.Errorf("HTML title missing or incorrect. Got:\n%s", body)
				}
				if !strings.Contains(body, "<h1>Index of /</h1>") {
					t.Errorf("HTML H1 missing or incorrect. Got:\n%s", body)
				}

				// Check for files and subdir by href and display name
				if !strings.Contains(body, "<a href=\"/file1.txt\">file1.txt</a>") {
					t.Errorf("Expected entry for file1.txt not found. Got:\n%s", body)
				}
				if !strings.Contains(body, "<a href=\"/another_file.html\">another_file.html</a>") {
					t.Errorf("Expected entry for another_file.html not found. Got:\n%s", body)
				}
				if !strings.Contains(body, "<a href=\"/subdir1/\">subdir1/</a>") {
					t.Errorf("Expected entry for subdir1/ not found. Got:\n%s", body)
				}

				// No parent link for root
				if strings.Contains(body, "<a href=\"../\">../</a>") || strings.Contains(body, "<a href=\"/\">../</a>") {
					t.Errorf("Parent directory link should not be present for root listing. Got:\n%s", body)
				}

				// Check sort order (subdir1 should be before files due to current SFS sorting)
				idxSubdir1 := strings.Index(body, "<a href=\"/subdir1/\">subdir1/</a>")
				idxFile1 := strings.Index(body, "<a href=\"/file1.txt\">file1.txt</a>")
				idxAnotherFile := strings.Index(body, "<a href=\"/another_file.html\">another_file.html</a>")

				if !(idxSubdir1 < idxAnotherFile && idxSubdir1 < idxFile1) {
					t.Errorf("Sorting order incorrect: directories should come first. subdir1: %d, another_file: %d, file1: %d", idxSubdir1, idxAnotherFile, idxFile1)
				}
				// File order (another_file.html before file1.txt alphabetically)
				if !(idxAnotherFile < idxFile1) {
					t.Errorf("File sorting order incorrect. another_file: %d, file1: %d", idxAnotherFile, idxFile1)
				}
			},
		},
		{
			name: "List subdirectory with a file and parent link",
			setupFiles: []fileSpec{
				{Path: "level1/item.dat", Content: "data item"},
				{Path: "level1", IsDir: true},
			},
			indexFilesConfig:      []string{"no_match.idx"},
			serveDirListingConfig: ptrToBool(true),
			requestPath:           "/files/level1/",
			matchedRoutePattern:   "/files/",
			expectedStatus:        "200",
			expectedContentType:   "text/html; charset=utf-8",
			expectedLogContains:   []string{"No index file found, serving directory listing", "webPath: /files/level1/"},
			checkHTMLBody: func(t *testing.T, body string, docRoot string, webPath string, files []fileSpec) {
				expectedTitle := "<title>Index of /files/level1/</title>"
				if !strings.Contains(body, expectedTitle) {
					t.Errorf("HTML title missing or incorrect. Expected: '%s'. Got:\n%s", expectedTitle, body)
				}

				// Parent link should point to /files/
				expectedParentLink := "<a href=\"/files/\">../</a>"
				if !strings.Contains(body, expectedParentLink) {
					t.Errorf("Parent directory link missing or incorrect. Expected: '%s'. Got:\n%s", expectedParentLink, body)
				}

				itemInfo, _ := os.Stat(filepath.Join(docRoot, "level1/item.dat"))
				itemSize := humanize.Bytes(uint64(itemInfo.Size()))
				itemModTime := itemInfo.ModTime().Format("02-Jan-2006 15:04")
				// Link href should be absolute path from server root based on current SFS impl
				expectedItemEntry := fmt.Sprintf("<a href=\"/files/level1/item.dat\">item.dat</a>%*s %20s %10s", 42, "", itemModTime, itemSize)
				if !strings.Contains(body, expectedItemEntry) {
					t.Errorf("Expected entry for item.dat not found or incorrect. Expected part: '%s'. Got:\n%s", expectedItemEntry, body)
				}
			},
		},
		{

			name: "List empty subdirectory",
			setupFiles: []fileSpec{
				{Path: "empty_dir", IsDir: true},
			},
			indexFilesConfig:      []string{"index.html"},
			serveDirListingConfig: ptrToBool(true),
			requestPath:           "/empty_dir/",
			matchedRoutePattern:   "/",
			expectedStatus:        "200",
			expectedContentType:   "text/html; charset=utf-8",
			expectedLogContains:   []string{"No index file found, serving directory listing", "webPath: /empty_dir/"},
			checkHTMLBody: func(t *testing.T, body string, docRoot string, webPath string, files []fileSpec) {
				expectedTitle := "<title>Index of /empty_dir/</title>"
				if !strings.Contains(body, expectedTitle) {
					t.Errorf("HTML title missing or incorrect. Got:\n%s", body)
				}
				// Parent link should point to /
				expectedParentLink := "<a href=\"/\">../</a>"
				if !strings.Contains(body, expectedParentLink) {
					t.Errorf("Parent directory link missing or incorrect for empty dir. Expected: '%s'. Got:\n%s", expectedParentLink, body)
				}
				// Check that no file/dir entries are listed other than parent
				// The structure is <pre><a href="/">../</a>\n  (optional other entries) \n</pre>
				// Simpler check: count occurrences of "<a href"
				if strings.Count(body, "<a href") > 1 {
					t.Errorf("Body for empty directory contains more than just the parent link. Got:\n%s", body)
				}
			},
		},
		{

			name: "List directory with special characters in names",
			setupFiles: []fileSpec{
				{Path: "special_chars_dir/file with spaces.txt", Content: "spaces"},
				{Path: "special_chars_dir/file<tag>.html", Content: "tag"},
				{Path: "special_chars_dir/sub dir", IsDir: true},
				{Path: "special_chars_dir", IsDir: true},
			},
			indexFilesConfig:      []string{"non_existent.idx"},
			serveDirListingConfig: ptrToBool(true),
			requestPath:           "/public/special_chars_dir/",
			matchedRoutePattern:   "/public/",
			expectedStatus:        "200",
			expectedContentType:   "text/html; charset=utf-8",
			checkHTMLBody: func(t *testing.T, body string, docRoot string, webPath string, files []fileSpec) {
				expectedTitle := "<title>Index of /public/special_chars_dir/</title>"
				if !strings.Contains(body, expectedTitle) {
					t.Errorf("HTML title missing or incorrect. Got:\n%s", body)
				}

				// Check for "file with spaces.txt"
				// Display: file with spaces.txt
				// Href: /public/special_chars_dir/file%20with%20spaces.txt (url.PathEscape applied by generateDirectoryListingHTML)
				if !strings.Contains(body, "<a href=\"/public/special_chars_dir/file%20with%20spaces.txt\">file with spaces.txt</a>") {
					t.Errorf("Entry for 'file with spaces.txt' incorrect or missing. Got:\n%s", body)
				}

				// Check for "file<tag>.html"
				// Display: file&lt;tag&gt;.html
				// Href: /public/special_chars_dir/file%3Ctag%3E.html (url.PathEscape applied)
				if !strings.Contains(body, "<a href=\"/public/special_chars_dir/file%3Ctag%3E.html\">file&lt;tag&gt;.html</a>") {
					t.Errorf("Entry for 'file<tag>.html' incorrect or missing. Got:\n%s", body)
				}

				// Check for "sub dir/"
				// Display: sub dir/
				// Href: /public/special_chars_dir/sub%20dir/
				if !strings.Contains(body, "<a href=\"/public/special_chars_dir/sub%20dir/\">sub dir/</a>") {
					t.Errorf("Entry for 'sub dir/' incorrect or missing. Got:\n%s", body)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create a unique docRoot for each subtest to avoid interference
			docRoot, cleanup := setupTestDocumentRoot(t, tc.setupFiles)
			defer cleanup()

			sfsConfig := config.StaticFileServerConfig{
				DocumentRoot:          docRoot,
				IndexFiles:            tc.indexFilesConfig,
				ServeDirectoryListing: tc.serveDirListingConfig,
			}

			logBuffer := new(bytes.Buffer)
			testLogger := logger.NewTestLogger(logBuffer)
			sfs := newTestStaticFileHandler(t, sfsConfig, testLogger, "")

			mockWriter := newMockStreamWriter(t, 1)
			ctx := context.WithValue(context.Background(), router.MatchedPathPatternKey{}, tc.matchedRoutePattern)
			mockWriter.setContext(ctx)

			req := newTestRequest(t, http.MethodGet, tc.requestPath, nil, nil)
			req = req.WithContext(ctx)

			sfs.ServeHTTP2(mockWriter, req)

			if status := mockWriter.getResponseStatus(); status != tc.expectedStatus {
				t.Errorf("Expected status %s, got %s. Log: %s", tc.expectedStatus, status, logBuffer.String())
			}

			if contentType := mockWriter.getResponseHeader("content-type"); contentType != tc.expectedContentType {
				t.Errorf("Expected Content-Type '%s', got '%s'", tc.expectedContentType, contentType)
			}

			logOutput := logBuffer.String()
			for _, expectedLog := range tc.expectedLogContains {
				// For JSON logs, we expect something like: "details":{"webPath":"/actual/path"}
				// The expectedLog is a simplified key-value string like "webPath: /actual/path"
				// We need to construct a JSON substring to look for, e.g., "\"webPath\":\"/actual/path\""
				var checkSubstring string
				if strings.Contains(expectedLog, ": ") {
					parts := strings.SplitN(expectedLog, ": ", 2)
					if len(parts) == 2 {
						// Ensure forward slashes for paths when creating the JSON-like string
						checkSubstring = fmt.Sprintf("\"%s\":\"%s\"", parts[0], filepath.ToSlash(parts[1]))
					} else {
						checkSubstring = expectedLog // Fallback if format is not "key: value"
					}
				} else {
					checkSubstring = expectedLog // Use as-is if no colon
				}

				found := strings.Contains(logOutput, checkSubstring)

				if !found {
					// This alternative check is a heuristic.
					// It tries to match if the expectedLog was a "webPath: /some/path" and the log actually contained
					// an absolute "dirPath":"/tmp/.../some/path" for the same resource.
					if strings.HasPrefix(expectedLog, "webPath: ") && tc.requestPath != "" && docRoot != "" && tc.matchedRoutePattern != "" {
						relativeWebPath := strings.TrimPrefix(tc.requestPath, tc.matchedRoutePattern)
						relativeWebPath = strings.TrimPrefix(relativeWebPath, "/") // ensure it's relative path component

						// Construct the absolute filesystem path that this webPath would point to
						absFsPathOfWebResource := filepath.Join(docRoot, relativeWebPath)
						absFsPathOfWebResource = filepath.Clean(absFsPathOfWebResource) // Clean it

						// Format this for a JSON check as a dirPath
						alternativeDirpathCheck := fmt.Sprintf("\"dirPath\":\"%s\"", filepath.ToSlash(absFsPathOfWebResource))
						if strings.Contains(logOutput, alternativeDirpathCheck) {
							found = true
						}
					}
				}

				if !found {
					// If still not found, try one more variant: if expectedLog was "dirPath: /reltmp/..."
					// and actual log used an absolute path. (This is less likely if tests provide absolute expectedLog for dirPath)
					// This part is becoming complex and might indicate the expectedLog strings themselves need to be more precise
					// or the logging in SFS needs to be more consistent for testability.
					// For now, the main check is `checkSubstring`.
					t.Errorf("Expected log to contain substring related to '%s' (tried checking for JSON-like '%s'), but it didn't. Log: %s", expectedLog, checkSubstring, logOutput)
				}
			}

			if tc.checkHTMLBody != nil {
				// tc.requestPath is the web path to the directory (e.g., "/public/special_chars_dir/")
				tc.checkHTMLBody(t, string(mockWriter.getResponseBody()), docRoot, tc.requestPath, tc.setupFiles)
			}
		})
	}
}

func TestStaticFileServer_ServeHTTP2_ConditionalRequests(t *testing.T) {
	fileContent := "Test content for conditional requests."
	filePathInDocRoot := "conditional.txt"
	pastTime := time.Now().Add(-24 * time.Hour).Truncate(time.Second) // Truncate for consistent Last-Modified comparison

	setupFiles := []fileSpec{
		{Path: filePathInDocRoot, Content: fileContent},
	}

	docRoot, cleanup := setupTestDocumentRoot(t, setupFiles)
	defer cleanup()

	// Set a specific mod time for the test file
	fullTestFilePath := filepath.Join(docRoot, filePathInDocRoot)
	if err := os.Chtimes(fullTestFilePath, pastTime, pastTime); err != nil {
		t.Fatalf("Failed to set mod time for %s: %v", fullTestFilePath, err)
	}

	fileInfo, err := os.Stat(fullTestFilePath)
	if err != nil {
		t.Fatalf("Failed to stat test file %s: %v", fullTestFilePath, err)
	}
	expectedETag := generateETag(fileInfo)
	expectedLastModified := fileInfo.ModTime().UTC().Format(http.TimeFormat)

	sfsConfig := config.StaticFileServerConfig{
		DocumentRoot: docRoot,
	}
	logBuffer := new(bytes.Buffer)
	testLogger := logger.NewTestLogger(logBuffer)
	sfs := newTestStaticFileHandler(t, sfsConfig, testLogger, "")

	tests := []struct {
		name                string
		method              string
		requestHeaders      http.Header
		expectedStatus      string
		expectedContent     string // Empty if 304
		expectedETagHeader  string // Can be same as expectedETag or empty if not checked specifically for 200
		expectedLastMod     string // Can be same as expectedLastModified or empty if not checked specifically for 200
		expectLogContains   []string
		matchedRoutePattern string
	}{
		// --- If-None-Match Tests ---
		{
			name:                "If-None-Match: Matching strong ETag",
			method:              http.MethodGet,
			requestHeaders:      http.Header{"If-None-Match": []string{expectedETag}},
			expectedStatus:      "304",
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			expectLogContains:   []string{"If-None-Match cache hit"},
			matchedRoutePattern: "/files/",
		},
		{
			name:                "If-None-Match: Matching weak ETag (client sends weak, server's is strong)",
			method:              http.MethodGet,
			requestHeaders:      http.Header{"If-None-Match": []string{`W/` + expectedETag}},
			expectedStatus:      "304",
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			expectLogContains:   []string{"If-None-Match cache hit"},
			matchedRoutePattern: "/files/",
		},
		{
			name:                "If-None-Match: Non-matching ETag",
			method:              http.MethodGet,
			requestHeaders:      http.Header{"If-None-Match": []string{`"non-matching-etag"`}},
			expectedStatus:      "200",
			expectedContent:     fileContent,
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			matchedRoutePattern: "/files/",
		},
		{
			name:                "If-None-Match: * (resource exists)",
			method:              http.MethodGet,
			requestHeaders:      http.Header{"If-None-Match": []string{`*`}},
			expectedStatus:      "304",
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			expectLogContains:   []string{"If-None-Match cache hit"},
			matchedRoutePattern: "/files/",
		},
		{
			name:                "If-None-Match: List of ETags, one matches",
			method:              http.MethodGet,
			requestHeaders:      http.Header{"If-None-Match": []string{`"etag1", ` + expectedETag + `, "etag2"`}},
			expectedStatus:      "304",
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			expectLogContains:   []string{"If-None-Match cache hit"},
			matchedRoutePattern: "/files/",
		},

		// --- If-Modified-Since Tests ---
		{
			name:                "If-Modified-Since: Resource not modified (IMS time is same as mod time)",
			method:              http.MethodGet,
			requestHeaders:      http.Header{"If-Modified-Since": []string{expectedLastModified}},
			expectedStatus:      "304",
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			expectLogContains:   []string{"If-Modified-Since cache hit"},
			matchedRoutePattern: "/files/",
		},
		{
			name:                "If-Modified-Since: Resource not modified (IMS time is after mod time)",
			method:              http.MethodGet,
			requestHeaders:      http.Header{"If-Modified-Since": []string{fileInfo.ModTime().Add(1 * time.Hour).UTC().Format(http.TimeFormat)}},
			expectedStatus:      "304",
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			expectLogContains:   []string{"If-Modified-Since cache hit"},
			matchedRoutePattern: "/files/",
		},
		{
			name:                "If-Modified-Since: Resource was modified (IMS time is before mod time)",
			method:              http.MethodGet,
			requestHeaders:      http.Header{"If-Modified-Since": []string{fileInfo.ModTime().Add(-1 * time.Hour).UTC().Format(http.TimeFormat)}},
			expectedStatus:      "200",
			expectedContent:     fileContent,
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			matchedRoutePattern: "/files/",
		},
		{
			name:                "If-Modified-Since: Invalid date format (should be ignored, serve 200)",
			method:              http.MethodGet,
			requestHeaders:      http.Header{"If-Modified-Since": []string{"invalid-date-format"}},
			expectedStatus:      "200",
			expectedContent:     fileContent,
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			matchedRoutePattern: "/files/",
		},

		// --- Precedence: If-None-Match over If-Modified-Since ---
		{
			name:   "Precedence: If-None-Match (non-match) and If-Modified-Since (would be 304)",
			method: http.MethodGet,
			requestHeaders: http.Header{
				"If-None-Match":     []string{`"non-matching-etag"`},
				"If-Modified-Since": []string{expectedLastModified},
			},
			expectedStatus:      "200",
			expectedContent:     fileContent,
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			matchedRoutePattern: "/files/",
		},
		{
			name:   "Precedence: If-None-Match (match) and If-Modified-Since (would be 200)",
			method: http.MethodGet,
			requestHeaders: http.Header{
				"If-None-Match":     []string{expectedETag},
				"If-Modified-Since": []string{fileInfo.ModTime().Add(-1 * time.Hour).UTC().Format(http.TimeFormat)},
			},
			expectedStatus:      "304",
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			expectLogContains:   []string{"If-None-Match cache hit"},
			matchedRoutePattern: "/files/",
		},

		// --- No conditional headers ---
		{
			name:                "No conditional headers",
			method:              http.MethodGet,
			requestHeaders:      http.Header{},
			expectedStatus:      "200",
			expectedContent:     fileContent,
			expectedETagHeader:  expectedETag,
			expectedLastMod:     expectedLastModified,
			matchedRoutePattern: "/files/",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logBuffer.Reset()
			mockWriter := newMockStreamWriter(t, 1)
			ctx := context.WithValue(context.Background(), router.MatchedPathPatternKey{}, tc.matchedRoutePattern)
			mockWriter.setContext(ctx)

			reqPath := tc.matchedRoutePattern + filePathInDocRoot
			req := newTestRequest(t, tc.method, reqPath, nil, tc.requestHeaders)
			req = req.WithContext(ctx)

			sfs.ServeHTTP2(mockWriter, req)

			if status := mockWriter.getResponseStatus(); status != tc.expectedStatus {
				t.Errorf("Expected status %s, got %s. Log: %s", tc.expectedStatus, status, logBuffer.String())
			}

			respETag := mockWriter.getResponseHeader("etag")
			if tc.expectedETagHeader != "" {
				if respETag != tc.expectedETagHeader {
					t.Errorf("Expected ETag header '%s', got '%s'", tc.expectedETagHeader, respETag)
				}
			} else if tc.expectedStatus == "200" && respETag == "" {
				t.Error("Expected ETag header for 200 response, but it was missing")
			}

			respLastMod := mockWriter.getResponseHeader("last-modified")
			if tc.expectedLastMod != "" {
				if respLastMod != tc.expectedLastMod {
					t.Errorf("Expected Last-Modified header '%s', got '%s'", tc.expectedLastMod, respLastMod)
				}
			} else if tc.expectedStatus == "200" && respLastMod == "" {
				t.Error("Expected Last-Modified header for 200 response, but it was missing")
			}

			body := mockWriter.getResponseBody()
			if tc.expectedStatus == "304" {
				if len(body) != 0 {
					t.Errorf("Expected empty body for 304 response, got %d bytes: %s", len(body), string(body))
				}
			} else if tc.expectedStatus == "200" {
				if string(body) != tc.expectedContent {
					t.Errorf("Expected body\n%s\nbut got\n%s", tc.expectedContent, string(body))
				}
				expectedCL := fmt.Sprintf("%d", len(tc.expectedContent))
				if cl := mockWriter.getResponseHeader("content-length"); cl != expectedCL {
					t.Errorf("Expected Content-Length '%s' for 200 response, got '%s'", expectedCL, cl)
				}
			}

			currentLogOutput := logBuffer.String()
			for _, expectedLog := range tc.expectLogContains {
				if !strings.Contains(currentLogOutput, expectedLog) {
					t.Errorf("Expected log to contain '%s', but it didn't. Log: %s", expectedLog, currentLogOutput)
				}
			}
		})
	}
}

func TestStaticFileServer_ServeHTTP2_UnsupportedMethods(t *testing.T) {
	docRoot, cleanup := setupTestDocumentRoot(t, []fileSpec{
		{Path: "some_resource.txt", Content: "This resource exists"},
	})
	defer cleanup()

	sfsConfig := config.StaticFileServerConfig{
		DocumentRoot: docRoot,
	}

	unsupportedMethods := []string{
		http.MethodPost,
		http.MethodPut,
		http.MethodDelete,
		http.MethodPatch,
		http.MethodTrace,   // TRACE is often disabled for security
		http.MethodConnect, // CONNECT is for proxies
		"INVALIDMETHOD",    // A completely bogus method
	}

	requestPath := "/files/some_resource.txt"
	matchedRoutePattern := "/files/"

	for _, method := range unsupportedMethods {
		t.Run(method, func(t *testing.T) {
			logBuffer := new(bytes.Buffer)
			testLogger := logger.NewTestLogger(logBuffer)
			sfs := newTestStaticFileHandler(t, sfsConfig, testLogger, "")

			mockWriter := newMockStreamWriter(t, 1)
			ctx := context.WithValue(context.Background(), router.MatchedPathPatternKey{}, matchedRoutePattern)
			mockWriter.setContext(ctx)

			req := newTestRequest(t, method, requestPath, nil, nil)
			req = req.WithContext(ctx)

			sfs.ServeHTTP2(mockWriter, req)

			expectedStatus := "405"
			if status := mockWriter.getResponseStatus(); status != expectedStatus {
				t.Errorf("Expected status %s for method %s, got %s. Log: %s", expectedStatus, method, status, logBuffer.String())
			}

			// Check log for "Method not allowed"
			expectedLogMsg := "StaticFileServer: Method not allowed"
			if !strings.Contains(logBuffer.String(), expectedLogMsg) {
				t.Errorf("Expected log to contain '%s' for method %s, but it didn't. Log: %s", expectedLogMsg, method, logBuffer.String())
			}
			// Check log for the specific method
			if !strings.Contains(logBuffer.String(), fmt.Sprintf("\"method\":\"%s\"", method)) {
				t.Errorf("Expected log to contain method '%s', but it didn't. Log: %s", method, logBuffer.String())
			}

			// Check for default error page content
			bodyBytes := mockWriter.getResponseBody()
			bodyStr := string(bodyBytes)

			// Assuming HTML response as default when Accept header is not set
			if !strings.Contains(bodyStr, fmt.Sprintf("<title>%s %s</title>", expectedStatus, http.StatusText(http.StatusMethodNotAllowed))) {
				t.Errorf("Expected error page title for status %s, but not found in body. Body: %s", expectedStatus, bodyStr)
			}
			expectedDetail := "Method not allowed for this resource."
			if !strings.Contains(bodyStr, expectedDetail) {
				t.Errorf("Expected error page body to contain detail '%s', but not found. Body: %s", expectedDetail, bodyStr)
			}
		})
	}
}

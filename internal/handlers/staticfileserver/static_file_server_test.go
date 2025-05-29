package staticfileserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io" // Added for io.Reader
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	// "time" // Removed as it's currently unused

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/http2"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/router" // Added import
)

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
	t           *testing.T // For logging errors within the mock
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

// newTestStaticFileServer creates a StaticFileServer instance for testing.
// mainConfigFilePath is usually "" for unit tests unless relative MimeTypesPath needs resolving.
func newTestStaticFileServer(t *testing.T, cfg config.StaticFileServerConfig, lg *logger.Logger, mainConfigFilePath string) *StaticFileServer {
	t.Helper()
	if lg == nil {
		lg = logger.NewDiscardLogger() // Default to discard logger if none provided
	}

	// Marshal the specific handler config to JSON bytes
	rawCfg, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Failed to marshal StaticFileServerConfig for test: %v", err)
	}

	handler, err := New(rawCfg, lg, mainConfigFilePath)
	if err != nil {
		t.Fatalf("New StaticFileServer failed: %v", err)
	}

	sfs, ok := handler.(*StaticFileServer)
	if !ok {
		t.Fatalf("New did not return a *StaticFileServer instance")
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
	sfs := newTestStaticFileServer(t, sfsConfig, testLogger, "")

	if sfs == nil {
		t.Fatal("StaticFileServer instance is nil")
	}
	if sfs.cfg.DocumentRoot != docRoot {
		t.Errorf("Expected DocumentRoot %s, got %s", docRoot, sfs.cfg.DocumentRoot)
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
	sfs := newTestStaticFileServer(t, sfsConfig, testLogger, "")

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

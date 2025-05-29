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

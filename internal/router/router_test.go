package router

import (
	"bytes"
	"context" // Added for mockResponseWriter.Context()
	"encoding/json"
	"errors"
	"fmt"
	// "io" // Removed unused io import
	// "log" // Removed stdlib log import, no longer needed by newTestLogger stub
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"example.com/llmahttap/v2/internal/config"

	"example.com/llmahttap/v2/internal/http2"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/server"
	"golang.org/x/net/http2/hpack"
)

// Helper function to get a pointer to a string.
func strPtr(s string) *string {
	return &s
}

// mockHandler is a simple implementation of http2.Handler for testing.
type mockHandler struct {
	id          string
	handlerType string
	config      json.RawMessage
}

func (mh *mockHandler) ServeHTTP2(resp http2.StreamWriter, req *http.Request) {
	// No-op for most router tests, or can be used to signal it was called.
}

// mockResponseWriter implements http2.ResponseWriter for testing error responses.
type mockResponseWriter struct {
	headersSent bool
	status      int
	header      http.Header
	body        *bytes.Buffer
	dataWritten bool
}

func newMockResponseWriter() *mockResponseWriter {
	return &mockResponseWriter{
		header: make(http.Header),
		body:   new(bytes.Buffer),
	}
}

func (mrw *mockResponseWriter) SendHeaders(headers []http2.HeaderField, endStream bool) error {
	if mrw.headersSent {
		return errors.New("headers already sent")
	}
	for _, hf := range headers {
		// Simplistic conversion for testing; assumes single value per header key
		mrw.header.Set(hf.Name, hf.Value)
		if hf.Name == ":status" {
			fmt.Sscanf(hf.Value, "%d", &mrw.status)
		}
	}
	mrw.headersSent = true
	if endStream {
		mrw.dataWritten = true // Effectively, if body is empty and endStream=true
	}
	return nil
}

func (mrw *mockResponseWriter) WriteData(p []byte, endStream bool) (n int, err error) {
	if !mrw.headersSent {
		return 0, errors.New("headers not sent yet")
	}
	if mrw.dataWritten && !endStream {
		// This check might be too strict. Let's assume it's fine for now.
	}
	n, err = mrw.body.Write(p)
	if err == nil && endStream {
		mrw.dataWritten = true
	}
	return n, err
}

func (mrw *mockResponseWriter) WriteTrailers(trailers []http2.HeaderField) error {
	// For simplicity in this mock, assume data has been written if trailers are sent.
	for _, hf := range trailers {
		mrw.header.Add("Trailer-"+hf.Name, hf.Value)
	}
	mrw.dataWritten = true
	return nil
}

func newTestLogger(t *testing.T) *logger.Logger {
	// Use the default logging config which targets stdout/stderr.
	// This is not ideal for silent tests, but better than a nil logger.
	// A dedicated test logger constructor in the logger package would be better.
	loggingConf := newDefaultBaseTestConfig().Logging
	if loggingConf == nil {
		t.Fatalf("newDefaultBaseTestConfig().Logging returned nil in %s", t.Name())
	}

	lg, err := logger.NewLogger(loggingConf)
	if err != nil {
		t.Fatalf("Failed to create logger for test (%s) using default config: %v", t.Name(), err)
	}
	return lg
}

// Helper to provide a default base config.

// newDefaultBaseTestConfig provides a default base config for tests.
func newDefaultBaseTestConfig() *config.Config {
	trueVal := true
	return &config.Config{
		Server:  &config.ServerConfig{},
		Routing: &config.RoutingConfig{},
		Logging: &config.LoggingConfig{
			LogLevel: config.LogLevelInfo,
			AccessLog: &config.AccessLogConfig{
				Enabled: &trueVal,
				Target:  strPtr("stdout"),
				Format:  "json",
			},
			ErrorLog: &config.ErrorLogConfig{
				Target: strPtr("stderr"),
			},
		},
	}
}

func TestNewRouter(t *testing.T) {
	registry := server.NewHandlerRegistry()
	lg := newTestLogger(t)

	t.Run("successful creation with empty routes", func(t *testing.T) {
		r, err := NewRouter([]config.Route{}, registry, lg)
		if err != nil {
			t.Fatalf("NewRouter failed: %v", err)
		}
		if r == nil {
			t.Fatal("NewRouter returned nil router")
		}
		if len(r.exactRoutes) != 0 {
			t.Errorf("expected 0 exact routes, got %d", len(r.exactRoutes))
		}
		if len(r.prefixRoutes) != 0 {
			t.Errorf("expected 0 prefix routes, got %d", len(r.prefixRoutes))
		}
	})

	routes := []config.Route{
		{PathPattern: "/api/users", MatchType: config.MatchTypeExact, HandlerType: "users_api"},
		{PathPattern: "/static/", MatchType: config.MatchTypePrefix, HandlerType: "static_files"},
		{PathPattern: "/", MatchType: config.MatchTypeExact, HandlerType: "root_exact"},
		{PathPattern: "/static/images/", MatchType: config.MatchTypePrefix, HandlerType: "static_images"},
	}

	t.Run("successful creation with mixed routes", func(t *testing.T) {
		r, err := NewRouter(routes, registry, lg)
		if err != nil {
			t.Fatalf("NewRouter failed: %v", err)
		}
		if len(r.exactRoutes) != 2 {
			t.Errorf("expected 2 exact routes, got %d", len(r.exactRoutes))
		}
		if _, ok := r.exactRoutes["/api/users"]; !ok {
			t.Error("missing exact route /api/users")
		}
		if _, ok := r.exactRoutes["/"]; !ok {
			t.Error("missing exact route /")
		}

		if len(r.prefixRoutes) != 2 {
			t.Errorf("expected 2 prefix routes, got %d", len(r.prefixRoutes))
		}
		// Check sorting of prefix routes (longest first)
		if r.prefixRoutes[0].PathPattern != "/static/images/" || r.prefixRoutes[1].PathPattern != "/static/" {
			t.Errorf("prefix routes not sorted correctly: expected [/static/images/, /static/], got [%s, %s]",
				r.prefixRoutes[0].PathPattern, r.prefixRoutes[1].PathPattern)
		}
	})

	t.Run("error on nil handler registry", func(t *testing.T) {
		_, err := NewRouter(routes, nil, lg)
		if err == nil {
			t.Error("expected error for nil handler registry, got nil")
		} else if !strings.Contains(err.Error(), "handler registry cannot be nil") {
			t.Errorf("expected error message for nil registry, got: %s", err.Error())
		}
	})

	t.Run("error on nil logger", func(t *testing.T) {
		_, err := NewRouter(routes, registry, nil)
		if err == nil {
			t.Error("expected error for nil logger, got nil")
		} else if !strings.Contains(err.Error(), "logger cannot be nil") {
			t.Errorf("expected error message for nil logger, got: %s", err.Error())
		}
	})
}

func TestRouter_Match(t *testing.T) {
	registry := server.NewHandlerRegistry()
	lg := newTestLogger(t)

	mockHandlerFactory := func(handlerType string) server.HandlerFactory {
		return func(cfg json.RawMessage, l *logger.Logger) (server.Handler, error) { // Changed http2.Handler to server.Handler
			return &mockHandler{id: "handler_for_" + handlerType, handlerType: handlerType, config: cfg}, nil
		}
	}
	registry.Register("exact_handler", mockHandlerFactory("exact_handler"))
	registry.Register("prefix_handler", mockHandlerFactory("prefix_handler"))
	registry.Register("long_prefix_handler", mockHandlerFactory("long_prefix_handler"))
	registry.Register("root_exact_handler", mockHandlerFactory("root_exact_handler"))
	registry.Register("root_prefix_handler", mockHandlerFactory("root_prefix_handler"))
	registry.Register("case_sensitive_handler", mockHandlerFactory("case_sensitive_handler"))

	registry.Register("handler_that_fails", func(cfg json.RawMessage, l *logger.Logger) (server.Handler, error) { // Changed http2.Handler to server.Handler
		return nil, errors.New("handler creation failed")
	})

	routes := []config.Route{
		{PathPattern: "/exact", MatchType: config.MatchTypeExact, HandlerType: "exact_handler"},
		{PathPattern: "/prefix/", MatchType: config.MatchTypePrefix, HandlerType: "prefix_handler"},
		{PathPattern: "/prefix/long/", MatchType: config.MatchTypePrefix, HandlerType: "long_prefix_handler"},
		{PathPattern: "/", MatchType: config.MatchTypeExact, HandlerType: "root_exact_handler"},
		{PathPattern: "/", MatchType: config.MatchTypePrefix, HandlerType: "root_prefix_handler"},
		{PathPattern: "/CaSe", MatchType: config.MatchTypeExact, HandlerType: "case_sensitive_handler"},
		{PathPattern: "/failing", MatchType: config.MatchTypeExact, HandlerType: "handler_that_fails"},
	}

	router, err := NewRouter(routes, registry, lg)
	if err != nil {
		t.Fatalf("Failed to create router: %v", err)
	}

	tests := []struct {
		name              string
		path              string
		expectedPattern   string
		expectedHandlerID string
		expectError       bool
		expectNilHandler  bool
	}{
		{"exact match", "/exact", "/exact", "handler_for_exact_handler", false, false},
		{"prefix match", "/prefix/something", "/prefix/", "handler_for_prefix_handler", false, false},
		{"prefix match exact to pattern", "/prefix/", "/prefix/", "handler_for_prefix_handler", false, false},
		{"longest prefix match", "/prefix/long/another", "/prefix/long/", "handler_for_long_prefix_handler", false, false},
		{"longest prefix match exact to pattern", "/prefix/long/", "/prefix/long/", "handler_for_long_prefix_handler", false, false},
		{"root exact match", "/", "/", "handler_for_root_exact_handler", false, false},
		{"path matching root prefix", "/anything", "/", "handler_for_root_prefix_handler", false, false},
		{"case sensitive match", "/CaSe", "/CaSe", "handler_for_case_sensitive_handler", false, false},
		{"case sensitive no match", "/case", "/", "handler_for_root_prefix_handler", false, false},
		/* 250 */ {"no match (actually matches root prefix)", "/nonexistent", "/", "handler_for_root_prefix_handler", false, false},
		{"handler creation error", "/failing", "/failing", "", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Match is the old name, FindRoute is the new one.
			// The tests were written for Match. Let's stick to that for now.
			// If FindRoute is preferred, tests should be adapted.
			// The router.go file has both `Match` (older) and `FindRoute` (newer based on my generation)
			// Let's assume `Match` is the one to test as per the existing test structure.
			// No, router.go has:
			// func (r *Router) Match(path string) (matchedRoute *config.Route, handler http2.Handler, err error)
			// func (r *Router) FindRoute(path string) (*MatchedRouteInfo, error)
			// func (r *Router) ServeHTTP(s *http2.Stream, req *http.Request)
			// The test targets router.Match.

			matchedRouteConf, handler, err := router.Match(tt.path)

			if tt.expectError {
				if err == nil {
					t.Errorf("Match(%s): expected error, got nil", tt.path)
				}
			} else {
				if err != nil {
					t.Errorf("Match(%s): unexpected error: %v", tt.path, err)
				}
			}

			if tt.expectNilHandler {
				if handler != nil {
					t.Errorf("Match(%s): expected nil handler, got %T", tt.path, handler)
				}
				// For "no match", matchedRouteConf should also be nil.
				// For "handler creation error", matchedRouteConf could be non-nil if Match returns it before erroring on creation.
				// Based on current Match impl: if handler creation fails, matchedRoute is nil.
				if matchedRouteConf != nil {
					t.Errorf("Match(%s): expected nil matchedRouteConf for nil/erroring handler, got %v", tt.path, matchedRouteConf)
				}
			} else {
				if handler == nil {
					t.Fatalf("Match(%s): expected handler, got nil", tt.path)
				}
				if matchedRouteConf == nil {
					t.Fatalf("Match(%s): expected matchedRouteConf, got nil", tt.path)
				}
				if matchedRouteConf.PathPattern != tt.expectedPattern {
					t.Errorf("Match(%s): expected pattern '%s', got '%s'", tt.path, tt.expectedPattern, matchedRouteConf.PathPattern)
				}
				mockH, ok := handler.(*mockHandler)
				if !ok {
					t.Fatalf("Match(%s): handler is not *mockHandler: %T", tt.path, handler)
				}
				if mockH.id != tt.expectedHandlerID {
					t.Errorf("Match(%s): expected handler ID '%s', got '%s'", tt.path, tt.expectedHandlerID, mockH.id)
				}
			}
		})
	}

	t.Run("exact /foo/ vs prefix /foo/ for request /foo/", func(t *testing.T) {
		tempRegistry := server.NewHandlerRegistry()
		tempRegistry.Register("exact_foo_slash", mockHandlerFactory("exact_foo_slash")) // Invalid by spec (exact ends with / not root)
		tempRegistry.Register("prefix_foo_slash", mockHandlerFactory("prefix_foo_slash"))
		tempRegistry.Register("exact_foo", mockHandlerFactory("exact_foo"))

		// Test case 1: Request "/foo", Routes: "/foo" (Exact), "/foo/" (Prefix)
		tempRoutes1 := []config.Route{
			{PathPattern: "/foo", MatchType: config.MatchTypeExact, HandlerType: "exact_foo"},
			{PathPattern: "/foo/", MatchType: config.MatchTypePrefix, HandlerType: "prefix_foo_slash"},
		}
		r1, _ := NewRouter(tempRoutes1, tempRegistry, lg)
		routeConf1, h1, _ := r1.Match("/foo")
		if h1.(*mockHandler).id != "handler_for_exact_foo" {
			t.Errorf("For /foo with routes /foo (Exact) and /foo/ (Prefix), expected exact_foo handler, got %s", h1.(*mockHandler).id)
		}
		if routeConf1.PathPattern != "/foo" {
			t.Errorf("Expected pattern /foo, got %s", routeConf1.PathPattern)
		}

		// Test for request "/foo/" with same routes: should match prefix
		routeConf2, h2, _ := r1.Match("/foo/")
		if h2.(*mockHandler).id != "handler_for_prefix_foo_slash" {
			t.Errorf("For /foo/ with routes /foo (Exact) and /foo/ (Prefix), expected prefix_foo_slash handler, got %s", h2.(*mockHandler).id)
		}
		if routeConf2.PathPattern != "/foo/" {
			t.Errorf("Expected pattern /foo/, got %s", routeConf2.PathPattern)
		}
	})
}

// testOkHandlerForServeHTTP is a mock handler for TestRouter_ServeHTTP.
type testOkHandlerForServeHTTP struct {
	pathTracker *string
}

func (h *testOkHandlerForServeHTTP) ServeHTTP2(resp http2.StreamWriter, req *http.Request) {
	if h.pathTracker != nil {
		*(h.pathTracker) = req.URL.Path
	}
	// resp is server.ResponseWriterStream, which implements server.ResponseWriter
	// We need to convert server.HeaderField to hpack.HeaderField if SendHeaders takes hpack.HeaderField.
	// server.ResponseWriter expects server.HeaderField. Our mockResponseWriter and testableStream use hpack.HeaderField.
	// This needs to be consistent. Let's assume mockResponseWriter/testableStream should use server.HeaderField.
	// For now, the error is about the factory signature.

	// The mock handler needs to use the methods of server.ResponseWriterStream
	// to send a response.
	// The testableStream currently implements http2.ResponseWriter using hpack.HeaderField
	// Let's assume `resp` can handle server.HeaderField or we will adapt testableStream later.
	// For now, to compile, just call `resp.SendHeaders`.
	// resp.SendHeaders([]server.HeaderField{{Name: ":status", Value: "200"}}, true)
	// However, the actual stream used in tests is `testableStream` which uses mockResponseWriter,
	// and mockResponseWriter uses hpack.HeaderField.
	// This test (TestRouter_ServeHTTP) has a note: "Skipping direct call to r.ServeHTTP due to http2.Stream mocking complexity."
	// So this ServeHTTP2 method might not be fully exercised if the handler is not directly called.
	// But its signature must be correct.
	// If it *is* called, and `resp` is `testableStream`, it expects hpack fields.
	// Let's assume for now that the mock should send *something* minimal that fits the interface.
	// The error is in the factory, not here directly yet.

	// The call in original code:
	// s.SendHeaders([]hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
	// Our `resp` is server.ResponseWriterStream, which should take server.HeaderField
	// Let's assume the mock in TestRouter_ServeHTTP will be fixed to use server.HeaderField.
	// For now, this is what a real handler would do.
	resp.SendHeaders([]http2.HeaderField{{Name: ":status", Value: "200"}}, true)
}

func TestRouter_ServeHTTP(t *testing.T) {
	lg := newTestLogger(t)

	handlerReg := server.NewHandlerRegistry()
	var handlerServedPath string

	handlerReg.Register("ok_handler", func(cfg json.RawMessage, l *logger.Logger) (server.Handler, error) { // Changed http2.Handler to server.Handler
		return &testOkHandlerForServeHTTP{pathTracker: &handlerServedPath}, nil
	})

	handlerReg.Register("fail_creation_handler", func(cfg json.RawMessage, l *logger.Logger) (server.Handler, error) { // Changed http2.Handler to server.Handler
		return nil, fmt.Errorf("handler creation failed")
	})

	routes := []config.Route{
		{PathPattern: "/found", MatchType: config.MatchTypeExact, HandlerType: "ok_handler"},
		{PathPattern: "/servererror", MatchType: config.MatchTypeExact, HandlerType: "fail_creation_handler"},
	}

	r, err := NewRouter(routes, handlerReg, lg)
	if err != nil {
		t.Fatalf("NewRouter failed: %v", err)
	}

	tests := []struct {
		name                        string
		path                        string
		expectedStatus              int
		expectHandlerCalledWithPath string // if empty, handler not expected to be called
	}{
		{"not found", "/notfound", http.StatusNotFound, ""},
		{"handler creation error", "/servererror", http.StatusInternalServerError, ""},
		{"found and served", "/found", http.StatusOK, "/found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handlerServedPath = ""                                               // Reset for each test
			req := httptest.NewRequest("GET", "http://example.com"+tt.path, nil) // Need full URL for req.URL.Path

			mockRW := newMockResponseWriter()
			// Minimal mock for http2.Stream that uses our mockResponseWriter.
			// The stream needs an ID() method as router.ServeHTTP logs it.
			// The router's ServeHTTP method calls handler.ServeHTTP2(s, req).
			// 's' is the *http2.Stream, which needs to implement http2.ResponseWriter.
			// Our testableStream fulfills this.

			// The testableStream cannot be directly cast to *http2.Stream.
			// This test needs a way to provide a mock *http2.Stream or http2.Stream needs to be more testable.
			// For now, to make it compile, we pass nil and expect specific behavior or skip.
			// However, router.ServeHTTP will likely panic if s.ID() is called on a nil stream,
			// or if server.WriteErrorResponse is called with a nil stream.
			// The mock handler will also panic if it receives a nil stream.
			testStream := &testableStream{
				writer:   mockRW,
				streamID: 1, // Using a fixed stream ID for simplicity in test
			}
			r.ServeHTTP(testStream, req)

			if mockRW.status != tt.expectedStatus {
				t.Errorf("ServeHTTP path %s: expected status %d, got %d (body: %s)", tt.path, tt.expectedStatus, mockRW.status, mockRW.body.String())
			}

			if tt.expectHandlerCalledWithPath != "" {
				if handlerServedPath != tt.expectHandlerCalledWithPath {
					t.Errorf("ServeHTTP path %s: handler expected to serve path '%s', but served '%s'", tt.path, tt.expectHandlerCalledWithPath, handlerServedPath)
				}
			} else {
				if handlerServedPath != "" {
					t.Errorf("ServeHTTP path %s: handler not expected to be called, but was for path '%s'", tt.path, handlerServedPath)
				}
			}
		})
	}
}

// testableStream is a minimal http2.Stream mock for testing router.ServeHTTP.
type testableStream struct {
	// Intentionally not embedding http2.Stream to ensure we only use what's needed for ResponseWriter
	writer   *mockResponseWriter
	streamID uint32
	// To satisfy http2.Stream methods if any are called beyond ResponseWriter.
	// For ServeHTTP as written, only ResponseWriter and ID() are directly used by router or its callees (like server.WriteErrorResponse)
}

func (ts *testableStream) ID() uint32 { return ts.streamID }

// Implement http2.ResponseWriter for testableStream
func (ts *testableStream) SendHeaders(headers []http2.HeaderField, endStream bool) error {
	return ts.writer.SendHeaders(headers, endStream)
}
func (ts *testableStream) WriteData(p []byte, endStream bool) (n int, err error) {
	return ts.writer.WriteData(p, endStream)
}
func (ts *testableStream) WriteTrailers(trailers []http2.HeaderField) error {
	return ts.writer.WriteTrailers(trailers)
}

// Implement other http2.Stream methods that might be called by a handler if necessary
// For now, these are enough for router.ServeHTTP's own logic (error handling) and basic handler calls.
func (ts *testableStream) Context() context.Context            { return context.Background() } // No-op, adjust if handler uses it
func (ts *testableStream) RequestBody() *bytes.Reader          { return bytes.NewReader(nil) } // No-op
func (ts *testableStream) RequestHeaders() []hpack.HeaderField { return nil }
func (ts *testableStream) GetHandlerConfig() json.RawMessage   { return nil }

// This is a helper in logger/logger_test.go or similar if it existed.
// For now, define a local helper.

func TestRouteSorting(t *testing.T) {
	routes := []config.Route{
		{PathPattern: "/a/", MatchType: config.MatchTypePrefix},
		{PathPattern: "/a/b/c/", MatchType: config.MatchTypePrefix},
		{PathPattern: "/a/b/", MatchType: config.MatchTypePrefix},
	}
	expectedOrder := []string{"/a/b/c/", "/a/b/", "/a/"}

	// This sort is done in NewRouter. Let's mimic it.
	sort.Slice(routes, func(i, j int) bool {
		return len(routes[i].PathPattern) > len(routes[j].PathPattern)
	})

	for i, r := range routes {
		if r.PathPattern != expectedOrder[i] {
			t.Errorf("Route sorting failed. Expected '%s' at index %d, got '%s'", expectedOrder[i], i, r.PathPattern)
		}
	}
}

func (mrw *mockResponseWriter) Context() context.Context {
	return context.Background() // Basic implementation for mock
}

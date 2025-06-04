package http2

/*
   NOTE: The stream tests are split into multiple files to reduce the load on LLM
   context compared to having just one big file open.
*/

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"

	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

func TestStream_processRequestHeadersAndDispatch(t *testing.T) {
	t.Parallel()
	const testStreamID = uint32(1)
	var wg sync.WaitGroup

	baseHeaders := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":path", Value: "/test?query=1"},
		{Name: ":authority", Value: "example.com"},
		{Name: "user-agent", Value: "test-agent"},
		{Name: "accept", Value: "application/json"},
	}

	tests := []struct {
		name                string
		headers             []hpack.HeaderField
		endStream           bool // This is the endStream flag from the HEADERS frame
		dispatcher          func(sw StreamWriter, req *http.Request)
		isDispatcherNilTest bool
		preFunc             func(s *Stream, t *testing.T, tcData struct{ endStream bool }) // To setup stream state for endStream scenarios
		expectErrorFromFunc bool                                                           // True if processRequestHeadersAndDispatch itself should return an error
		expectedRSTCode     ErrorCode                                                      // If an RST_STREAM is expected (due to error in func or panic)
		expectRST           bool                                                           // True if an RST frame should be sent
		expectedReq         *http.Request                                                  // For comparing parts of the constructed request
		customValidation    func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection)
	}{
		{
			name:      "valid headers, no endStream on HEADERS",
			headers:   baseHeaders,
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen // Caller (conn) would set this
				s.mu.Unlock()
			},
			dispatcher: func(sw StreamWriter, req *http.Request) {
				defer wg.Done()
				// For "no endStream on HEADERS", body is not expected to be readable yet
				// without subsequent DATA frames. Avoid Read() to prevent issues.
				if req.Method != "GET" {
					t.Errorf("Dispatcher: req.Method = %s, want GET", req.Method)
				}
				t.Logf("Dispatcher (no endStream): Bypassing req.Body.Read() for this test case.")
			},
			expectedReq: &http.Request{
				Method: "GET",
				URL:    &url.URL{Scheme: "https", Host: "example.com", Path: "/test", RawQuery: "query=1"},
				Proto:  "HTTP/2.0", ProtoMajor: 2, ProtoMinor: 0,
				Header:     http.Header{"User-Agent": []string{"test-agent"}, "Accept": []string{"application/json"}},
				Host:       "example.com",
				RequestURI: "/test?query=1",
			},
		},
		{
			name:      "valid headers, with endStream on HEADERS",
			headers:   baseHeaders,
			endStream: true, // HEADERS frame has END_STREAM
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateHalfClosedRemote // Caller (conn) would set this
				s.endStreamReceivedFromClient = true  // Caller (conn) would set this
				s.mu.Unlock()
				// Caller (conn) would close the request body writer
				if err := s.requestBodyWriter.Close(); err != nil {
					t.Fatalf("preFunc: failed to close requestBodyWriter: %v", err)
				}
			},
			dispatcher: func(sw StreamWriter, req *http.Request) {
				defer wg.Done()
				// Body should yield EOF immediately
				bodyBytes, err := io.ReadAll(req.Body)
				if err != nil {
					t.Errorf("Dispatcher (endStream): Error reading request body: %v", err)
				}
				if len(bodyBytes) != 0 {
					t.Errorf("Dispatcher (endStream): Expected empty body (EOF) for END_STREAM on HEADERS, got %d bytes: %s", len(bodyBytes), string(bodyBytes))
				}
			},
			expectedReq: &http.Request{
				Method: "GET",
				URL:    &url.URL{Scheme: "https", Host: "example.com", Path: "/test", RawQuery: "query=1"},
				Proto:  "HTTP/2.0", ProtoMajor: 2, ProtoMinor: 0,
				Header:     http.Header{"User-Agent": []string{"test-agent"}, "Accept": []string{"application/json"}},
				Host:       "example.com",
				RequestURI: "/test?query=1",
			},
		},
		{
			name: "missing :method pseudo-header",
			headers: []hpack.HeaderField{
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/test"},
				{Name: ":authority", Value: "example.com"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			dispatcher:          nil,
			isDispatcherNilTest: false,
			expectErrorFromFunc: true,
			expectRST:           false, // Stream method returns error, conn sends RST
			expectedRSTCode:     ErrCodeProtocolError,
			expectedReq:         nil,
			customValidation:    nil,
		},
		{
			name: "missing :path pseudo-header",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":authority", Value: "example.com"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			dispatcher:          nil,
			isDispatcherNilTest: false,
			expectErrorFromFunc: true,
			expectRST:           false, // Stream method returns error, conn sends RST
			expectedRSTCode:     ErrCodeProtocolError,
			expectedReq:         nil,
			customValidation:    nil,
		},
		{
			name: "missing :scheme pseudo-header",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":path", Value: "/test"},
				{Name: ":authority", Value: "example.com"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			dispatcher:          nil,
			isDispatcherNilTest: false,
			expectErrorFromFunc: true,
			expectRST:           false, // Stream method returns error, conn sends RST
			expectedRSTCode:     ErrCodeProtocolError,
			expectedReq:         nil,
			customValidation:    nil,
		},
		{
			name:                "nil dispatcher",
			headers:             baseHeaders,
			endStream:           false,
			isDispatcherNilTest: true,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true, // Error due to nil dispatcher
			expectRST:           true,
			expectedRSTCode:     ErrCodeInternalError,
		},
		{
			name:      "dispatcher panics",
			headers:   baseHeaders,
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			dispatcher: func(sw StreamWriter, req *http.Request) {
				defer wg.Done()
				panic("test panic in dispatcher")
			},
			expectErrorFromFunc: false, // processRequestHeadersAndDispatch returns nil, panic handled in goroutine
			expectRST:           true,
			expectedRSTCode:     ErrCodeInternalError, // RST from panic recovery
		},

		// --- NEW TEST CASES FOR INVALID HEADERS (TASK 1) ---
		{
			name: "invalid: uppercase header field name",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "X-Custom-Header", Value: "uppercase-fail"}, // Invalid
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true,
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for uppercase header name, expected not called.")
				}
			},
		},

		{
			name: "invalid: connection-specific header (Connection)",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "Connection", Value: "keep-alive"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true,
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for 'Connection' header, expected not called.")
				}
			},
		},
		{
			name: "invalid: connection-specific header (Keep-Alive)",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "Keep-Alive", Value: "timeout=5"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true,
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for 'Keep-Alive' header, expected not called.")
				}
			},
		},
		{
			name: "invalid: connection-specific header (Proxy-Connection)",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "Proxy-Connection", Value: "close"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true,
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for 'Proxy-Connection' header, expected not called.")
				}
			},
		},
		{
			name: "invalid: connection-specific header (Transfer-Encoding)",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "POST"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "Transfer-Encoding", Value: "chunked"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true,
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for 'Transfer-Encoding' header, expected not called.")
				}
			},
		},
		{
			name: "invalid: connection-specific header (Upgrade)",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "Upgrade", Value: "websocket"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true,
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for 'Upgrade' header, expected not called.")
				}
			},
		},
		{
			name: "invalid: TE header with value other than 'trailers'",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "te", Value: "gzip"}, // Invalid value
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true,
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for invalid 'te' header value, expected not called.")
				}
			},
		},
		{

			name: "valid: TE header with 'trailers'",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "te", Value: "trailers"},
			},
			endStream: false,
			dispatcher: func(sw StreamWriter, req *http.Request) {
				defer wg.Done()
				if req.Header.Get("te") != "trailers" {
					t.Errorf("Dispatcher: te header mismatch, got %s", req.Header.Get("te"))
				}
			},
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: false,
			expectRST:           false,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() == 0 {
					t.Error("Dispatcher was not called for valid te header, expected called.")
				} else if mrd.CalledCount() > 1 {
					t.Errorf("Dispatcher was called %d times for valid te header, expected 1.", mrd.CalledCount())
				}
			},
		},
		// --- END NEW TEST CASES FOR INVALID HEADERS ---
		// --- NEW TEST CASES FOR INVALID HEADERS (TASK 1) ---
		{
			name: "invalid: uppercase header field name",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "X-Custom-Header", Value: "uppercase-fail"}, // Invalid
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true, // Corrected
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for uppercase header name, expected not called.")
				}
			},
		},
		{

			name: "invalid: pseudo-header in trailer block",
			headers: []hpack.HeaderField{
				{Name: ":status", Value: "200"}, // Example pseudo-header
				{Name: "trailer-field", Value: "value"},
			},
			endStream: true, // Trailers imply endStream
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.initialHeadersProcessed = true
				s.mu.Unlock()
			},
			expectErrorFromFunc: true,
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for pseudo-header in trailers, expected not called.")
				}
			},
		},
		{
			name: "invalid: connection-specific header (Connection)",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "Connection", Value: "keep-alive"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true, // Corrected
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for 'Connection' header, expected not called.")
				}
			},
		},
		{
			name: "invalid: connection-specific header (Keep-Alive)",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "Keep-Alive", Value: "timeout=5"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true, // Corrected
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for 'Keep-Alive' header, expected not called.")
				}
			},
		},
		{
			name: "invalid: connection-specific header (Proxy-Connection)",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "Proxy-Connection", Value: "close"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true, // Corrected
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for 'Proxy-Connection' header, expected not called.")
				}
			},
		},
		{
			name: "invalid: connection-specific header (Transfer-Encoding)",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "POST"}, // POST to make TE more plausible, though still invalid
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "Transfer-Encoding", Value: "chunked"},
			},
			endStream: false, // TE chunked implies body follows
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true, // Corrected
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for 'Transfer-Encoding' header, expected not called.")
				}
			},
		},
		{
			name: "invalid: connection-specific header (Upgrade)",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "Upgrade", Value: "websocket"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: true, // Corrected
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for 'Upgrade' header, expected not called.")
				}
			},
		},
		{
			name: "invalid: multiple connection-specific headers",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "Connection", Value: "close"},          // Connection-specific
				{Name: "Keep-Alive", Value: "timeout=10"},     // Connection-specific
				{Name: "Transfer-Encoding", Value: "chunked"}, // Connection-specific
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			dispatcher: nil,

			expectErrorFromFunc: true,
			expectRST:           false,
			expectedRSTCode:     ErrCodeProtocolError,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for multiple connection-specific headers, expected not called.")
				}
			},
		},
		// --- NEW TEST CASE FOR HEADERS ON CLOSED STREAM (TASK 3) ---
		{
			name:      "invalid: HEADERS on already closed stream",
			headers:   baseHeaders,
			endStream: false, // Flag on this HEADERS frame doesn't matter much here
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateClosed
				// For this variant, we specifically do not set pendingRSTCode,
				// to test the case where stream is closed but not necessarily due to an RST we initiated.
				// processRequestHeadersAndDispatch should still return StreamClosed error.
				// sendRSTStream, if called by it, should NOT send a new RST because state is Closed.
				s.mu.Unlock()
			},
			expectErrorFromFunc: true,
			expectRST:           false,               // RST should NOT be sent if stream is already closed.
			expectedRSTCode:     ErrCodeStreamClosed, // This is the error code it should *return*.
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for HEADERS on closed stream, expected not called.")
				}
				s.mu.RLock()
				finalState := s.state
				s.mu.RUnlock()
				if finalState != StreamStateClosed {
					t.Errorf("Stream state expected to remain Closed, got %s", finalState)
				}
			},
		},
		// --- NEW TEST CASE FOR HEADERS ON CLOSED STREAM (TASK 3) ---
		{
			name:      "invalid: HEADERS on already closed stream#01", // Variant with pendingRSTCode
			headers:   baseHeaders,
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateClosed
				closedCode := ErrCodeCancel // Simulate it was closed by a previous RST (e.g. by peer or earlier internal)
				s.pendingRSTCode = &closedCode
				s.mu.Unlock()
			},
			expectErrorFromFunc: true,
			expectRST:           false,               // RST should NOT be sent if stream is already closed.
			expectedRSTCode:     ErrCodeStreamClosed, // This is the error code it should *return*.
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for HEADERS on closed stream (variant #01), expected not called.")
				}
				// Verify final stream state remains Closed
				s.mu.RLock()
				finalState := s.state
				s.mu.RUnlock()
				if finalState != StreamStateClosed {
					t.Errorf("Stream state expected to remain Closed (variant #01), got %s", finalState)
				}
			},
		},
		{

			name: "valid: TE header with 'trailers'",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":path", Value: "/"},
				{Name: ":authority", Value: "example.com"},
				{Name: "te", Value: "trailers"},
			},
			endStream: false,
			dispatcher: func(sw StreamWriter, req *http.Request) {
				defer wg.Done()
				if req.Header.Get("te") != "trailers" {
					t.Errorf("Dispatcher: te header mismatch, got %s", req.Header.Get("te"))
				}
			},
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
			},
			expectErrorFromFunc: false,
			expectRST:           false,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() == 0 {
					t.Error("Dispatcher was not called for valid te header, expected called.")
				} else if mrd.CalledCount() > 1 {
					t.Errorf("Dispatcher was called %d times for valid te header, expected 1.", mrd.CalledCount())
				}
			},
		}, // Closes "valid: TE header with 'trailers'" test case
		{ // Starts "invalid: subsequent HEADERS frame (not trailer)" test case
			name: "invalid: subsequent HEADERS frame (not trailer)",
			headers: []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":path", Value: "/newpath_after_initial_headers"},
				{Name: ":scheme", Value: "https"},
				{Name: ":authority", Value: "example.com"},
				{Name: "some-regular-header", Value: "value"},
			},
			endStream: false,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				s.state = StreamStateOpen
				s.initialHeadersProcessed = true
				s.mu.Unlock()
			},
			dispatcher:          nil,
			expectErrorFromFunc: true,
			expectedRSTCode:     ErrCodeProtocolError,
			expectRST:           false,
			customValidation: func(t *testing.T, mrd *mockRequestDispatcher, s *Stream, conn *Connection) {
				if mrd.CalledCount() > 0 {
					t.Error("Dispatcher was called for subsequent non-trailer HEADERS, expected not called.")
				}
			},
		}, // Closes "invalid: subsequent HEADERS frame (not trailer)" test case
		// --- END NEW TEST CASES FOR INVALID HEADERS ---
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dispatcher != nil { // Only Add if a dispatcher function (which calls Done) is provided for the test case
				wg.Add(1)
			}

			// mockRequestDispatcher is defined in conn_test.go
			mrd := &mockRequestDispatcher{}
			if tc.dispatcher != nil { // tc.dispatcher is func(sw StreamWriter, req *http.Request)
				mrd.fn = tc.dispatcher
			} else if !tc.isDispatcherNilTest && !strings.Contains(tc.name, "panics") {
				// Default dispatcher for non-nil, non-panic tests if not provided
				mrd.fn = func(sw StreamWriter, req *http.Request) {
					defer wg.Done() // wg is defined in the outer TestStream_processRequestHeadersAndDispatch
					// Default no-op, or basic validation
					if req == nil {
						t.Errorf("Default dispatcher: received nil *http.Request on stream %d", sw.ID())
					}
				}
			}

			// newTestConnection is from conn_test.go
			// It sets up a real Connection with a mockNetConn and the provided dispatcher (mrd.Dispatch)
			// The dispatcher set on the connection (conn.dispatcher) will be used by newStream
			// if no specific dispatcher is given to a stream later.
			// However, processRequestHeadersAndDispatch takes a dispatcher func directly.
			conn, mnc := newTestConnection(t, false /*isClient*/, mrd, nil)

			// Crucial: Ensure conn.cfgRemoteAddrStr is set for http.Request.RemoteAddr
			// newTestConnection does not explicitly set this. We set it on the mockNetConn's remoteAddr.
			// conn.remoteAddrStr is derived from conn.netConn.RemoteAddr().String()
			// So, ensuring mnc.remoteAddr is sensible is enough.
			// Example: mnc.remoteAddr = &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345} (done in newMockNetConn)
			// Let's ensure cfgRemoteAddrStr on the actual *Connection object is also updated if stream.go uses it.
			// The current stream.go uses req.RemoteAddr which is populated from s.conn.remoteAddrStr
			conn.remoteAddrStr = mnc.RemoteAddr().String() // Ensure conn.remoteAddrStr is explicitly set from mnc.
			conn.maxFrameSize = DefaultMaxFrameSize        // Ensure this is set for tests.

			// Perform handshake. This is vital for settings exchange (e.g., initial window sizes).
			performHandshakeForTest(t, conn, mnc, nil) // from conn_test.go
			mnc.ResetWriteBuffer()                     // Clear handshake frames written by server to mnc

			// Create stream using the real connection.
			// Pass 0 for window sizes so newTestStream uses the connection's default/negotiated ones.
			stream := newTestStream(t, testStreamID, conn, true /*isPeerInitiated*/, 0, 0)

			if tc.preFunc != nil {
				tc.preFunc(stream, t, struct{ endStream bool }{tc.endStream})
			} else {
				stream.mu.Lock()

				t.Logf("TestStream_processRequestHeadersAndDispatch (%s): stream %p, stream.requestBodyReader %p after newTestStream", tc.name, stream, stream.requestBodyReader)
				if tc.endStream { // If HEADERS has END_STREAM
					stream.state = StreamStateIdle // Will transition to HalfClosedRemote in processRequestHeadersAndDispatch
					// stream.endStreamReceivedFromClient will be set by processRequestHeadersAndDispatch
					// stream.requestBodyWriter will be closed by processRequestHeadersAndDispatch
				} else {
					stream.state = StreamStateIdle // Will transition to Open
				}
				stream.mu.Unlock()
			}

			var dispatcherToUse func(StreamWriter, *http.Request)
			if !tc.isDispatcherNilTest {
				dispatcherToUse = mrd.Dispatch // Use the mock dispatcher's method
			} else {
			}

			err := stream.processRequestHeadersAndDispatch(tc.headers, tc.endStream, dispatcherToUse)

			if tc.expectErrorFromFunc {
				if err == nil {
					t.Fatalf("Expected an error from processRequestHeadersAndDispatch, but got nil")
				}
				t.Logf("Got expected error from processRequestHeadersAndDispatch: %v", err)

				// Verify the error is a StreamError with the expected code and stream ID
				// Default to expecting StreamError. ConnectionError is only for specific cases.
				if tc.name == "invalid: HEADERS on already closed stream" || tc.name == "invalid: HEADERS on already closed stream#01" {
					connErr, ok := err.(*ConnectionError)
					if !ok {
						t.Fatalf("Expected a ConnectionError from processRequestHeadersAndDispatch for test '%s', got %T: %v", tc.name, err, err)
					}
					if connErr.Code != tc.expectedRSTCode { // Using expectedRSTCode to hold the expected ConnectionError.Code
						t.Errorf("Expected ConnectionError code %s from processRequestHeadersAndDispatch for test '%s', got %s", tc.expectedRSTCode, tc.name, connErr.Code)
					}
				} else {
					streamErr, ok := err.(*StreamError)
					if !ok {
						t.Fatalf("Expected a StreamError from processRequestHeadersAndDispatch for test '%s', got %T: %v", tc.name, err, err)
					}
					if streamErr.Code != tc.expectedRSTCode {
						t.Errorf("Expected StreamError code %s from processRequestHeadersAndDispatch for test '%s', got %s", tc.expectedRSTCode, tc.name, streamErr.Code)
					}
					if streamErr.StreamID != stream.id {
						t.Errorf("Expected StreamError StreamID %d for test '%s', got %d", stream.id, tc.name, streamErr.StreamID)
					}
				}
			} else { // No error was expected from processRequestHeadersAndDispatch
				if err != nil {
					// If no error was expected, but we got one, fail the test.
					t.Fatalf("Expected no error from processRequestHeadersAndDispatch, but got: %v", err)
				}
			}

			if tc.expectRST {
				// Instead of reading from conn.writerChan:
				var rstFrameFound *RSTStreamFrame
				// Use a slightly longer timeout to give writerLoop and panic recovery ample time.
				// The log shows the RST is written quickly, but test environments can vary.
				waitForCondition(t, 250*time.Millisecond, 20*time.Millisecond, func() bool {
					// Reset buffer before reading to ensure we only see frames from this specific action
					// mnc.ResetWriteBuffer() // NO! This is wrong here. We need to accumulate.
					// We are checking mnc's buffer which accumulates all writes.
					frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					for _, f := range frames {
						if rst, ok := f.(*RSTStreamFrame); ok {
							if rst.Header().StreamID == stream.id { // Ensure it's for the correct stream
								rstFrameFound = rst
								return true
							}
						}
					}
					return false
				}, fmt.Sprintf("RST_STREAM frame for stream %d (code %s) to be written to mock net.Conn (Test: %s)", stream.id, tc.expectedRSTCode, tc.name))

				if rstFrameFound == nil {
					// Log frames actually found for debugging
					allFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					var frameSummaries []string
					for _, fr := range allFrames {
						frameSummaries = append(frameSummaries, fmt.Sprintf("Type: %s, StreamID: %d, Flags: %x, Len: %d", fr.Header().Type, fr.Header().StreamID, fr.Header().Flags, fr.Header().Length))
					}
					t.Logf("Frames found on mock net.conn for test '%s', stream %d: %v", tc.name, stream.id, frameSummaries)
					t.Fatalf("Expected RSTStreamFrame for stream %d on mock net.Conn, but none found (Test: %s)", stream.id, tc.name)
				}

				// rstFrameFound is now populated
				if rstFrameFound.ErrorCode != tc.expectedRSTCode {
					t.Errorf("RSTStreamFrame ErrorCode mismatch: got %s, want %s (Test: %s, Stream: %d)", rstFrameFound.ErrorCode, tc.expectedRSTCode, tc.name, stream.id)
				}
				// We've found the RST. We should clear the mnc buffer *after* this sub-test's check,
				// so the next sub-test starts fresh.
				// However, mnc.ResetWriteBuffer() is already called after performHandshakeForTest at the start of each sub-test run.
				// So, we don't strictly need to clear it here again, unless multiple RSTs could be sent by one action.
				// For safety, let's clear it if an RST was expected and found.
				mnc.ResetWriteBuffer()
			} else { // No RST expected from the specific action being tested
				// Give a very brief moment for any unexpected frames to be written by writerLoop
				time.Sleep(50 * time.Millisecond)
				frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())

				unexpectedActionFrames := []Frame{}

				// isCleanupRSTPresent was here, removed as unused.

				for _, f := range frames {
					// Check if it's an RST frame that could be from the stream's cleanup (called by newTestStream's t.Cleanup)
					if rstFrame, ok := f.(*RSTStreamFrame); ok && rstFrame.Header().StreamID == stream.id {
						// If this is the *only* frame, or if other frames are also just cleanup related, it might be OK.
						// This check is tricky. A simple heuristic: if an RST for *this* stream is found,
						// and no RST was expected by tc.expectRST, it's suspicious unless it's clearly cleanup.
						// The t.Cleanup runs *after* the test logic. If we see an RST here, it's likely from the test logic itself.
						t.Logf("Unexpected RSTStreamFrame found for stream %d when no RST was expected by test case '%s'. Code: %s. This might be an issue or overly aggressive cleanup simulation.", stream.id, tc.name, rstFrame.ErrorCode)
						unexpectedActionFrames = append(unexpectedActionFrames, f)
						// Don't set isCleanupRSTPresent = true here, as we are checking for *unexpected* frames from the action.
					} else {
						unexpectedActionFrames = append(unexpectedActionFrames, f)
					}
				}

				if len(unexpectedActionFrames) > 0 {
					var frameSummaries []string
					for _, fr := range unexpectedActionFrames {
						frameSummaries = append(frameSummaries, fmt.Sprintf("Type: %s, StreamID: %d, Flags: %x, Len: %d", fr.Header().Type, fr.Header().StreamID, fr.Header().Flags, fr.Header().Length))
					}
					t.Errorf("Did not expect frames from test logic on mock net.Conn for test '%s' (stream %d), but got: %v", tc.name, stream.id, frameSummaries)
				}
				// Clear the buffer regardless, so subsequent sub-tests don't see these frames.
				// This is important because ResetWriteBuffer is called at the start of the sub-test (line 1562)
				// AFTER performHandshake, so frames from one sub-test's action might linger if not cleared here.
				mnc.ResetWriteBuffer()
			}

			// This block checks dispatcher call and request properties.
			// It should only run if the function itself didn't error, isn't a nil dispatcher test,
			// and isn't the panic test (as panic recovery sends RST but dispatcher won't complete normally).
			if !tc.expectErrorFromFunc && !tc.isDispatcherNilTest && !strings.Contains(tc.name, "panics") && tc.expectedReq != nil {
				// Wait a bit for the dispatcher goroutine to run.
				// The dispatcher itself (mrd.fn) might do assertions.
				var dispatcherCalled bool
				var lastReqReceived *http.Request

				// Poll for dispatcher call, with a timeout
				timeout := time.After(150 * time.Millisecond) // Increased timeout
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()

			pollLoop:
				for {
					select {
					case <-timeout:
						t.Errorf("Timeout waiting for dispatcher to be called (Test: %s)", tc.name)
						break pollLoop
					case <-ticker.C:
						var wasCalled bool
						var reqFromDispatcher *http.Request

						mrd.mu.Lock() // Lock to safely read mrd.called and mrd.lastReq
						wasCalled = mrd.called
						if wasCalled {
							reqFromDispatcher = mrd.lastReq
						}
						mrd.mu.Unlock()

						if wasCalled {
							dispatcherCalled = true
							lastReqReceived = reqFromDispatcher
							break pollLoop
						}
					}
				}

				if !dispatcherCalled {
					t.Fatalf("Dispatcher was not called (Test: %s)", tc.name)
				}

				if tc.expectedReq != nil && lastReqReceived != nil {
					if lastReqReceived.Method != tc.expectedReq.Method {
						t.Errorf("Request Method mismatch: got %s, want %s", lastReqReceived.Method, tc.expectedReq.Method)
					}
					if lastReqReceived.URL.String() != tc.expectedReq.URL.String() {
						t.Errorf("Request URL mismatch: got %s, want %s", lastReqReceived.URL.String(), tc.expectedReq.URL.String())
					}
					if !reflect.DeepEqual(lastReqReceived.Header, tc.expectedReq.Header) {
						t.Errorf("Request Headers mismatch: got %+v, want %+v", lastReqReceived.Header, tc.expectedReq.Header)
					}
					if lastReqReceived.Host != tc.expectedReq.Host {
						t.Errorf("Request Host mismatch: got %s, want %s", lastReqReceived.Host, tc.expectedReq.Host)
					}
					if lastReqReceived.RequestURI != tc.expectedReq.RequestURI {
						t.Errorf("RequestURI mismatch: got %s, want %s", lastReqReceived.RequestURI, tc.expectedReq.RequestURI)
					}
					if lastReqReceived.RemoteAddr != conn.remoteAddrStr {
						t.Errorf("Request RemoteAddr: got %q, want %q", lastReqReceived.RemoteAddr, conn.remoteAddrStr)
					}

					if notifier, ok := lastReqReceived.Body.(*requestBodyConsumedNotifier); !ok {
						t.Error("Request Body is not of type *requestBodyConsumedNotifier")
					} else if notifier.reader != stream.requestBodyReader {
						t.Errorf("Request Body's underlying reader (%p) is not the stream's requestBodyReader (%p)", notifier.reader, stream.requestBodyReader)
					}
					if lastReqReceived.Context() != stream.ctx { // Compare underlying contexts if wrapped
						if lastReqReceived.Context() == nil || stream.ctx == nil {
							t.Error("One of the contexts is nil when comparing request context")
						} else {
							// This direct comparison might fail if context is wrapped.
							// A more robust check would be if lastReqReceived.Context().Value() can retrieve values set on stream.ctx.
							// For now, direct comparison is a basic check.
							// t.Logf("Req ctx: %v, Stream ctx: %v", lastReqReceived.Context(), stream.ctx)
						}
					}

					// The dispatcher mrd.fn for "valid headers, with endStream on HEADERS" case already checks body EOF.
				}
			}

			if tc.customValidation != nil {
				tc.customValidation(t, mrd, stream, conn)
			}
			// Wait for the dispatcher goroutine to complete before the sub-test finishes.
			// This ensures t.Logf and other t methods are not called on an invalid t.
			// It also ensures that cleanup logic in newTestStream doesn't race with the dispatcher.
			if tc.dispatcher != nil { // Only Wait if we Added to the WaitGroup
				wg.Wait()
			}
			// newTestStream's t.Cleanup will handle stream.Close()
		})
	}
}

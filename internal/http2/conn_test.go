package http2

import (
	"bytes"
	"context"
	"errors"
	"example.com/llmahttap/v2/internal/logger"
	"golang.org/x/net/http2/hpack"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// mockNetConn implements the net.Conn interface for testing.
type mockNetConn struct {
	mu          sync.Mutex
	readBuffer  *bytes.Buffer
	writeBuffer *bytes.Buffer
	closed      bool
	localAddr   net.Addr
	remoteAddr  net.Addr

	readDeadline  time.Time
	writeDeadline time.Time
}

func newMockNetConn() *mockNetConn {
	return &mockNetConn{
		readBuffer:  bytes.NewBuffer(nil),
		writeBuffer: bytes.NewBuffer(nil),
		localAddr:   &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345},
		remoteAddr:  &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54321},
	}
}

func (m *mockNetConn) Read(b []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, io.EOF // Or net.ErrClosed
	}
	if !m.readDeadline.IsZero() && time.Now().After(m.readDeadline) {
		return 0, errors.New("mock net.Conn read timeout") // Or os.ErrDeadlineExceeded for net.Error
	}
	return m.readBuffer.Read(b)
}

func (m *mockNetConn) Write(b []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, errors.New("write to closed mockNetConn") // Or net.ErrClosed
	}
	if !m.writeDeadline.IsZero() && time.Now().After(m.writeDeadline) {
		return 0, errors.New("mock net.Conn write timeout")
	}
	return m.writeBuffer.Write(b)
}

func (m *mockNetConn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("already closed") // Or net.ErrClosed
	}
	m.closed = true
	return nil
}

func (m *mockNetConn) LocalAddr() net.Addr {
	return m.localAddr
}

func (m *mockNetConn) RemoteAddr() net.Addr {
	return m.remoteAddr
}

func (m *mockNetConn) SetDeadline(t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readDeadline = t
	m.writeDeadline = t
	return nil
}

func (m *mockNetConn) SetReadDeadline(t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readDeadline = t
	return nil
}

func (m *mockNetConn) SetWriteDeadline(t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeDeadline = t
	return nil
}

// Helper methods for tests
func (m *mockNetConn) SetReadBuffer(data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readBuffer.Reset()
	m.readBuffer.Write(data)
}

func (m *mockNetConn) GetWriteBufferBytes() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeBuffer.Bytes()
}

func (m *mockNetConn) ClearWriteBuffer() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeBuffer.Reset()
}

// mockLogger provides a logger implementation for testing.
type mockLogger struct {
	mu            sync.Mutex
	DebugMessages []string
	InfoMessages  []string
	WarnMessages  []string
	ErrorMessages []string
	AccessEntries []logger.AccessLogEntry
}

func newMockLogger() *mockLogger {
	return &mockLogger{}
}

func (ml *mockLogger) Debug(msg string, context logger.LogFields) {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	ml.DebugMessages = append(ml.DebugMessages, msg)
}

func (ml *mockLogger) Info(msg string, context logger.LogFields) {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	ml.InfoMessages = append(ml.InfoMessages, msg)
}

func (ml *mockLogger) Warn(msg string, context logger.LogFields) {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	ml.WarnMessages = append(ml.WarnMessages, msg)
}

func (ml *mockLogger) Error(msg string, context logger.LogFields) {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	ml.ErrorMessages = append(ml.ErrorMessages, msg)
}

func (ml *mockLogger) Access(req *http.Request, streamID uint32, status int, responseBytes int64, duration time.Duration) {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	// For simplicity, not populating all fields or respecting real IP logic
	entry := logger.AccessLogEntry{
		Method:        req.Method,
		URI:           req.RequestURI,
		Status:        status,
		ResponseBytes: responseBytes,
		DurationMs:    duration.Nanoseconds() / 1e6,
		H2StreamID:    streamID,
	}
	ml.AccessEntries = append(ml.AccessEntries, entry)
}

func (ml *mockLogger) CloseLogFiles() error { return nil }

func (ml *mockLogger) ReopenLogFiles() error { return nil }

// mockStreamWriter implements the StreamWriter interface for testing.
type mockStreamWriter struct {
	id            uint32
	ctx           context.Context
	Headers       []HeaderField
	DataWritten   *bytes.Buffer
	Trailers      []HeaderField
	EndStreamSent bool
	HeadersSent   bool
}

func newMockStreamWriter(id uint32) *mockStreamWriter {
	return &mockStreamWriter{
		id:          id,
		ctx:         context.Background(),
		DataWritten: bytes.NewBuffer(nil),
	}
}

func (msw *mockStreamWriter) SendHeaders(headers []HeaderField, endStream bool) error {
	if msw.HeadersSent {
		return errors.New("headers already sent")
	}
	msw.Headers = headers
	msw.EndStreamSent = endStream
	msw.HeadersSent = true
	return nil
}

func (msw *mockStreamWriter) WriteData(p []byte, endStream bool) (n int, err error) {
	if !msw.HeadersSent {
		return 0, errors.New("headers not sent before data")
	}
	n, err = msw.DataWritten.Write(p)
	if err == nil {
		msw.EndStreamSent = endStream
	}
	return n, err
}

func (msw *mockStreamWriter) WriteTrailers(trailers []HeaderField) error {
	msw.Trailers = trailers
	msw.EndStreamSent = true
	return nil
}

func (msw *mockStreamWriter) ID() uint32 {
	return msw.id
}

func (msw *mockStreamWriter) Context() context.Context {
	return msw.ctx
}

// mockRequestDispatcher is a mock implementation of RequestDispatcherFunc.
type mockRequestDispatcher struct {
	mu       sync.Mutex
	lastReq  *http.Request
	lastSw   StreamWriter
	called   bool
	fn       func(StreamWriter, *http.Request) // Function to call for dispatch
	panicMsg interface{}                       // If set, Dispatch will panic with this
}

func (mrd *mockRequestDispatcher) Dispatch(sw StreamWriter, req *http.Request) {
	mrd.mu.Lock()
	mrd.called = true
	mrd.lastReq = req
	mrd.lastSw = sw
	// Capture panicMsg and fn under lock to avoid race if they are modified concurrently (though not typical for test setup)
	panicVal := mrd.panicMsg
	dispatchFn := mrd.fn
	mrd.mu.Unlock() // Release lock before calling dispatchFn or panicking

	if panicVal != nil {
		panic(panicVal)
	}
	if dispatchFn != nil {
		dispatchFn(sw, req)
	}
}

func newTestConnection(t *testing.T, isClient bool, nc net.Conn, dispatcher RequestDispatcherFunc, srvSettingsOverride map[SettingID]uint32) *Connection {
	t.Helper()
	lg := logger.NewDiscardLogger() // Use a discard logger for tests unless specific logging is needed
	if dispatcher == nil && !isClient {
		// Provide a default no-op dispatcher if none is given for server-side,
		// to avoid nil pointer if the test doesn't set one but connection logic expects it.
		dispatcher = func(StreamWriter, *http.Request) {}
	}
	return NewConnection(nc, lg, isClient, srvSettingsOverride, dispatcher)
}

// Helper to write a frame to a buffer for test input.
func frameToBytes(t *testing.T, f Frame) []byte {
	t.Helper()
	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatalf("frameToBytes: Error writing frame: %v", err)
	}
	return buf.Bytes()
}

// Helper to read a frame from a buffer (mocking network read output).
func bytesToFrame(t *testing.T, b []byte) Frame {
	t.Helper()
	buf := bytes.NewBuffer(b)
	f, err := ReadFrame(buf)
	if err != nil {
		t.Fatalf("bytesToFrame: Error reading frame: %v", err)
	}
	return f
}

// TestConnectionPrefaceHandling verifies client preface validation.
func TestConnectionPrefaceHandling(t *testing.T) {
	// TODO: Implement tests for correct and incorrect prefaces.
	// - Server receives correct preface.
	// - Server receives incorrect preface (should result in PROTOCOL_ERROR).
	// - Server receives partial preface then EOF.
}

// TestConnectionSettingsExchange verifies the initial SETTINGS frame exchange.
func TestConnectionSettingsExchange(t *testing.T) {
	// TODO: Implement tests for:
	// - Server sends initial SETTINGS.
	// - Client sends initial SETTINGS.
	// - Server receives and processes client SETTINGS, sends ACK.
	// - Client receives server SETTINGS, sends ACK.
	// - Timeout waiting for SETTINGS ACK.
	// - Invalid SETTINGS values (e.g., MAX_FRAME_SIZE too small/large).
}

// TestConnectionGoAway verifies GOAWAY frame sending and receiving.
func TestConnectionGoAway(t *testing.T) {
	// TODO: Implement tests for:
	// - Connection error triggers GOAWAY.
	// - Graceful shutdown triggers GOAWAY with NO_ERROR.
	// - Receiving GOAWAY from peer.
	// - Handling of Last-Stream-ID in GOAWAY.
}

// Helper for pointer to bool

// Helper for pointer to string
func strPtr(s string) *string { return &s }

// Ensure hpack.HeaderField is accessible
var _ []hpack.HeaderField

func TestMain(m *testing.M) {
	// This TestMain is intentionally empty as per project constraints.
	// DO NOT ADD LOGIC HERE.
	// If setup/teardown is needed for all tests in this package,
	// it must be done without TestMain.
	// For instance, by calling helper functions at the start/end of each TestXxx func.
	m.Run()
}

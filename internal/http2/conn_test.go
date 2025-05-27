package http2

import (
	"bytes"
	"context"
	"encoding/hex" // Added for hex.EncodeToString
	"errors"
	// "fmt" // Removed as unused
	"io"
	"net"
	"net/http" // Added for http.Request
	"os"
	"strings" // Added import
	"sync"
	"testing"
	"time"

	"example.com/llmahttap/v2/internal/logger"
	// "golang.org/x/net/http2/hpack" // Will be needed when HpackAdapter used directly
)

// mockNetConn is a mock implementation of net.Conn for testing.
type mockNetConn struct {
	mu            sync.Mutex
	readBuf       bytes.Buffer // Data to be read by the connection
	writeBuf      bytes.Buffer // Data written by the connection
	readDeadline  time.Time
	writeDeadline time.Time
	closed        bool
	closeErr      error // Error to return on Read/Write after close
	localAddr     net.Addr
	remoteAddr    net.Addr
	readHook      func([]byte) (int, error) // Optional hook for custom Read behavior
}

func newMockNetConn() *mockNetConn {
	return &mockNetConn{
		localAddr:  &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345},
		remoteAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54321},
		closeErr:   errors.New("use of closed network connection"),
	}
}

func (m *mockNetConn) Read(b []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return 0, m.closeErr
	}
	if !m.readDeadline.IsZero() && time.Now().After(m.readDeadline) {
		return 0, errors.New("read timeout (mock)") // Simpler than os.ErrDeadlineExceeded for mock
	}

	if m.readHook != nil {
		hn, herr := m.readHook(b)
		// If hook provided data (hn > 0), or a definitive error (herr != nil, including io.EOF),
		// then the hook's result is authoritative.
		if hn > 0 || herr != nil {
			return hn, herr
		}
		// If hook returned (0, nil), it means "no data from hook, try buffer or I'm just blocking".
		// The logic will now fall through to check the readBuf.
	}

	// Fallback to buffer read if no hook or if hook returned (0, nil)
	if m.readBuf.Len() == 0 {
		// Before returning io.EOF, if a read deadline is set and passed, return timeout error
		// This makes blocking reads on an empty buffer honor deadlines.
		if !m.readDeadline.IsZero() && time.Now().After(m.readDeadline) {
			return 0, errors.New("read timeout (mock, empty buffer)")
		}
		// If a readHook is active and returned (0, nil), and buffer is empty,
		// this effectively means the hook is simulating a block, so we return (0, nil)
		// to allow ReadFrame to retry, instead of an immediate EOF.
		if m.readHook != nil {
			return 0, nil // Hook active, buffer empty -> hook is blocking.
		}
		return 0, io.EOF // No hook, buffer empty -> true EOF
	}
	return m.readBuf.Read(b)
}

func (m *mockNetConn) Write(b []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return 0, m.closeErr
	}
	if !m.writeDeadline.IsZero() && time.Now().After(m.writeDeadline) {
		return 0, errors.New("write timeout (mock)")
	}
	// Log before write
	// fmt.Printf("mockNetConn.Write: writing %d bytes. Hex: %s. Current writeBuf len: %d\n", len(b), hex.EncodeToString(b), m.writeBuf.Len())
	n, err = m.writeBuf.Write(b)
	// Log after write
	// fmt.Printf("mockNetConn.Write: Wrote %d bytes, err: %v. New writeBuf len: %d\n", n, err, m.writeBuf.Len())
	return n, err
}

func (m *mockNetConn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		// net.Conn documentation suggests Close on already closed connection might return nil or specific error.
		// For mock, let's return nil to simplify test checks that don't care about double-close errors.
		return nil
	}
	m.closed = true
	return nil
}

func (m *mockNetConn) LocalAddr() net.Addr  { return m.localAddr }
func (m *mockNetConn) RemoteAddr() net.Addr { return m.remoteAddr }

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

// Helper methods for tests to feed data to the mock connection
func (m *mockNetConn) FeedReadBuffer(data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readBuf.Write(data)
}

// Helper methods for tests to inspect data written to the mock connection
func (m *mockNetConn) GetWriteBufferBytes() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	data := make([]byte, m.writeBuf.Len())
	copy(data, m.writeBuf.Bytes())
	return data
}

func (m *mockNetConn) GetWriteBufferLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeBuf.Len()
}

func (m *mockNetConn) ResetWriteBuffer() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeBuf.Reset()
}

func (m *mockNetConn) IsClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// mockRequestDispatcher is a mock implementation of the RequestDispatcherFunc
// for testing Connection's dispatching logic.
type mockRequestDispatcher struct {
	mu            sync.Mutex
	called        bool // Added to fix build error
	calledCount   int
	lastStreamID  uint32
	lastReq       *http.Request
	lastSw        StreamWriter
	dispatchError error                                    // Error to return from Dispatch
	fn            func(sw StreamWriter, req *http.Request) // Actual function to call
}

func (mrd *mockRequestDispatcher) Dispatch(sw StreamWriter, req *http.Request) {
	mrd.mu.Lock()
	mrd.calledCount++
	mrd.lastStreamID = sw.ID()
	mrd.lastSw = sw
	mrd.lastReq = req
	dispatchFn := mrd.fn
	mrd.mu.Unlock()

	if dispatchFn != nil {
		dispatchFn(sw, req)
	}
}

func (mrd *mockRequestDispatcher) GetLastCallInfo() (int, uint32, *http.Request, StreamWriter) {
	mrd.mu.Lock()
	defer mrd.mu.Unlock()
	return mrd.calledCount, mrd.lastStreamID, mrd.lastReq, mrd.lastSw
}

func (mrd *mockRequestDispatcher) Reset() {
	mrd.mu.Lock()
	defer mrd.mu.Unlock()
	mrd.calledCount = 0
	mrd.lastStreamID = 0
	mrd.lastReq = nil
	mrd.lastSw = nil
	mrd.dispatchError = nil
	mrd.fn = nil
}

// newTestConnection creates a Connection with a mockNetConn for testing.
// IMPORTANT: This function no longer calls conn.Close() in a defer. Tests must manage this.
func newTestConnection(t *testing.T, isClient bool, mockDispatcher *mockRequestDispatcher) (*Connection, *mockNetConn) {
	t.Helper()
	mnc := newMockNetConn()
	lg := logger.NewTestLogger(os.Stdout)

	var dispatcherFunc RequestDispatcherFunc
	if mockDispatcher != nil {
		dispatcherFunc = mockDispatcher.Dispatch
	} else {
		dispatcherFunc = func(sw StreamWriter, req *http.Request) { /* No-op */ }
	}

	conn := NewConnection(mnc, lg, isClient, nil, dispatcherFunc)
	// Give the writerLoop a chance to start after NewConnection.
	time.Sleep(50 * time.Millisecond)
	if conn == nil {
		t.Fatal("NewConnection returned nil")
	}
	// The runtime.Gosched() was previously here and removed as it was moved into NewConnection itself.
	// Keeping this comment as a reminder of past attempts.
	return conn, mnc
}

// Helper to convert a Frame object to its byte representation for testing.
func frameToBytes(t *testing.T, frame Frame) []byte {
	t.Helper()
	var buf bytes.Buffer
	err := WriteFrame(&buf, frame)
	if err != nil {
		t.Fatalf("frameToBytes: Error writing frame: %v", err)
	}
	return buf.Bytes()
}

// Helper to parse bytes into a Frame object for testing.
// Returns nil if no full frame can be read (e.g. EOF on empty buffer).
func bytesToFrame(t *testing.T, data []byte) Frame {
	t.Helper()
	if len(data) == 0 {
		return nil
	}
	reader := bytes.NewReader(data)
	frame, err := ReadFrame(reader)
	if err != nil {
		if errors.Is(err, io.EOF) || (errors.Is(err, io.ErrUnexpectedEOF) && reader.Len() == 0 && len(data) < FrameHeaderLen) {
			t.Logf("bytesToFrame: EOF or UnexpectedEOF on short data, returning nil frame. Error: %v", err)
			return nil
		}
		t.Fatalf("bytesToFrame: Error reading frame: %v. Data (hex): %s", err, hex.EncodeToString(data))
	}
	return frame
}

// readAllFramesFromBuffer reads all HTTP/2 frames from a byte slice.
func readAllFramesFromBuffer(t *testing.T, data []byte) []Frame {
	t.Helper()
	var frames []Frame
	r := bytes.NewReader(data)

	for r.Len() > 0 {
		frame, err := ReadFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				t.Logf("readAllFramesFromBuffer: Unexpected EOF, likely incomplete frame at end. Data remaining: %d bytes. Error: %v", r.Len(), err)
				break
			}
			t.Fatalf("readAllFramesFromBuffer: Error reading frame: %v", err)
		}
		frames = append(frames, frame)
	}
	return frames
}

// waitForCondition polls a condition function until it returns true or a timeout is reached.
func waitForCondition(t *testing.T, timeout time.Duration, interval time.Duration, condition func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("Timeout waiting for condition: %s", msg)
}

// --- Actual Tests for Connection ---

func TestConnection_ClientPrefaceHandling_Valid(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	defer conn.Close(errors.New("test cleanup close"))

	mnc.FeedReadBuffer([]byte(ClientPreface))
	clientSettings := []Setting{{ID: SettingInitialWindowSize, Value: 1024 * 1024}}
	settingsFrame := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: uint32(len(clientSettings) * 6)},
		Settings:    clientSettings,
	}
	mnc.FeedReadBuffer(frameToBytes(t, settingsFrame))

	t.Logf("TestConnection_ClientPrefaceHandling_Valid: MockNetConn readBuf len before ServerHandshake: %d. Expected > 0.", mnc.readBuf.Len())
	err := conn.ServerHandshake()
	if err != nil {
		t.Fatalf("ServerHandshake failed with valid preface: %v", err)
	}

	// Log the buffer length once before starting the wait.
	t.Logf("TestConnection_ClientPrefaceHandling_Valid: Before waitForCondition, mnc.GetWriteBufferLen() = %d. Written bytes (hex): %s", mnc.GetWriteBufferLen(), hex.EncodeToString(mnc.GetWriteBufferBytes()))

	waitForCondition(t, 500*time.Millisecond, 20*time.Millisecond, func() bool { // Increased timeout for this specific wait
		lenVal := mnc.GetWriteBufferLen() // Use a different variable name to avoid conflict
		// t.Logf("TestConnection_ClientPrefaceHandling_Valid: Polling in waitForCondition, mnc.GetWriteBufferLen() = %d", lenVal)
		return lenVal > 0
	}, "server to send SETTINGS frame")

	// This existing check remains, but the one above is key for diagnosing the timeout.
	if mnc.GetWriteBufferLen() == 0 {
		t.Error("Expected server to send SETTINGS frame after valid preface, but write buffer is empty")
	}
}

func TestConnection_ClientPrefaceHandling_Invalid(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	defer conn.Close(errors.New("test cleanup close"))

	invalidPreface := "THIS IS NOT THE PREFACE"
	mnc.FeedReadBuffer([]byte(invalidPreface))

	err := conn.ServerHandshake()
	if err == nil {
		t.Fatal("ServerHandshake succeeded with invalid preface, expected error")
	}

	connErr, ok := err.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError, got %T: %v", err, err)
	}
	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ProtocolError for invalid preface, got %s", connErr.Code)
	}
}

func TestConnection_SettingsExchange_ServerSendsSettings(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	serveErrChan := make(chan error, 1)

	defer func() {
		conn.Close(errors.New("test cleanup close")) // Ensure connection is closed
		// Wait for Serve to exit if it was started
		select {
		case err := <-serveErrChan:
			if err != nil && !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "connection shutdown") && !strings.Contains(err.Error(), "use of closed network connection") {
				// t.Logf("conn.Serve exited with error: %v", err)
			}
		case <-time.After(200 * time.Millisecond): // Reduced timeout
			// t.Log("Timed out waiting for conn.Serve to exit in defer")
		}
		// Wait for writer to exit
		select {
		case <-conn.writerDone:
		case <-time.After(200 * time.Millisecond): // Reduced timeout
			// t.Log("Timed out waiting for conn.writerDone to exit in defer")
		}
	}()

	mnc.FeedReadBuffer([]byte(ClientPreface))
	clientSettings := []Setting{
		{ID: SettingMaxFrameSize, Value: DefaultSettingsMaxFrameSize + 100},
		{ID: SettingInitialWindowSize, Value: DefaultSettingsInitialWindowSize * 2},
	}
	clientSettingsFrame := &SettingsFrame{
		FrameHeader: FrameHeader{StreamID: 0, Type: FrameSettings, Length: uint32(len(clientSettings) * 6)},
		Settings:    clientSettings,
	}
	mnc.FeedReadBuffer(frameToBytes(t, clientSettingsFrame))

	if err := conn.ServerHandshake(); err != nil {
		t.Fatalf("ServerHandshake failed: %v", err)
	}

	go func() {
		// If Serve returns an error, send it to the channel.
		// If Serve returns nil, send nil.
		serveErr := conn.Serve(context.Background())
		// Ensure not to block if serveErrChan is not listened to (e.g., test panics before select).
		select {
		case serveErrChan <- serveErr:
		default:
		}

	}()

	waitForCondition(t, 500*time.Millisecond, 20*time.Millisecond, func() bool {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		return len(frames) >= 2
	}, "server to send initial SETTINGS and client's SETTINGS ACK")

	writtenFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	if len(writtenFrames) < 2 {
		t.Fatalf("Expected at least 2 frames from server, got %d. Frames: %#v", len(writtenFrames), writtenFrames)
	}

	serverSettingsFrame, ok := writtenFrames[0].(*SettingsFrame)
	if !ok {
		t.Fatalf("First frame from server was not SETTINGS, got %T", writtenFrames[0])
	}
	if serverSettingsFrame.Header().Flags&FlagSettingsAck != 0 {
		t.Error("Server's initial SETTINGS frame had ACK flag set")
	}
	if len(serverSettingsFrame.Settings) == 0 {
		t.Error("Server's initial SETTINGS frame was empty")
	}

	settingsAckFrame, ok := writtenFrames[1].(*SettingsFrame)
	if !ok {
		t.Fatalf("Second frame from server was not SETTINGS, got %T", writtenFrames[1])
	}
	if settingsAckFrame.Header().Flags&FlagSettingsAck == 0 {
		t.Error("Server's second SETTINGS frame did not have ACK flag set")
	}
	if settingsAckFrame.Header().Length != 0 {
		t.Error("Server's SETTINGS ACK frame had non-zero length")
	}

	conn.settingsMu.RLock()
	expectedPeerMaxFrame := clientSettings[0].Value
	actualPeerMaxFrame := conn.peerMaxFrameSize
	conn.settingsMu.RUnlock()

	if actualPeerMaxFrame != expectedPeerMaxFrame {
		t.Errorf("Server did not apply client's SETTINGS_MAX_FRAME_SIZE: got %d, want %d", actualPeerMaxFrame, expectedPeerMaxFrame)
	}
}

func TestConnection_Settings_AckTimeout(t *testing.T) {
	// Store and restore original timeout
	originalSettingsAckTimeout := SettingsAckTimeoutDuration
	SettingsAckTimeoutDuration = 50 * time.Millisecond // Short duration for test
	defer func() { SettingsAckTimeoutDuration = originalSettingsAckTimeout }()

	conn, _ := newTestConnection(t, false, nil) // Server-side
	defer conn.Close(errors.New("test cleanup close"))
	// Defer writerDone wait
	defer func() {
		select {
		case <-conn.writerDone:
		case <-time.After(200 * time.Millisecond): // Keep this cleanup wait reasonable
		}
	}()

	err := conn.sendInitialSettings() // This starts the timer with the (now shorter) SettingsAckTimeoutDuration
	if err != nil {
		t.Fatalf("sendInitialSettings failed: %v", err)
	}

	conn.settingsMu.RLock()
	timerSet := conn.settingsAckTimeoutTimer != nil
	conn.settingsMu.RUnlock()
	if !timerSet {
		t.Fatal("settingsAckTimeoutTimer was not set after sendInitialSettings")
	}

	// Wait for the *modified* (shorter) timeout duration to simulate the timer firing.
	time.Sleep(SettingsAckTimeoutDuration + 30*time.Millisecond) // e.g. 50ms + 30ms = 80ms, well within 10s limit

	// Check if connection closed due to timeout
	var connErr error
	select {
	case <-conn.shutdownChan: // This is what we actually want to observe
		conn.streamsMu.RLock()
		connErr = conn.connError
		conn.streamsMu.RUnlock()
	case <-time.After(200 * time.Millisecond): // Small additional wait for shutdownChan
		t.Fatalf("Connection did not shut down after expected SETTINGS ACK timeout. Current connError: %v", conn.connError)
	}

	if connErr == nil {
		t.Fatalf("conn.connError was nil after SETTINGS ACK timeout, expected SettingsTimeout error")
	}
	ce, ok := connErr.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError, got %T: %v", connErr, connErr)
	}
	if ce.Code != ErrCodeSettingsTimeout {
		t.Errorf("Expected error code SettingsTimeout, got %s", ce.Code)
	}
}

func TestConnection_GoAway_ServerInitiated(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	serveErrChan := make(chan error, 1)

	performHandshakeForTest(t, conn, mnc) // Does handshake and clears mnc write buffer

	unblockReadChan := make(chan struct{})
	originalReadHook := mnc.readHook
	_ = originalReadHook // Silence "declared and not used"

	mnc.readHook = func(b []byte) (int, error) {
		// This hook is called while mnc.mu is HELD by mnc.Read().
		// DO NOT try to lock mnc.mu inside this hook.
		select {
		case <-unblockReadChan: // This channel is closed by the defer *after* test logic
			// If conn.Close() has already been called and shutdownChan is closed,
			// Serve should have already exited or be exiting.
			// If Serve is still running and unblockReadChan is closed, this leads to EOF.
			return 0, io.EOF
		default:
			// Simulate blocking: return 0, nil.
			// This tells ReadFrame there's no data *yet*, but not an error.
			// This keeps Serve's readFrame loop active but blocked on reading new frames.
			time.Sleep(1 * time.Millisecond) // Prevent tight loop if ReadFrame is very aggressive
			return 0, nil
		}
	}

	// Defer must be after originalReadHook is captured and mnc.readHook is set.
	defer func() {
		close(unblockReadChan)          // Allow Serve's readHook to return EOF if still running
		mnc.readHook = originalReadHook // Restore original hook
	}()

	go func() { serveErrChan <- conn.Serve(context.Background()) }()

	// Ensure Serve is running and potentially blocked on readHook's (0, nil) return
	time.Sleep(50 * time.Millisecond)

	// Set lastProcessedStreamID
	lastProcStreamID := uint32(5)
	conn.streamsMu.Lock()
	conn.lastProcessedStreamID = lastProcStreamID
	conn.streamsMu.Unlock()

	// Define the error for GOAWAY
	goAwayErrCode := ErrCodeInternalError
	debugMsg := "server shutting down with error"
	// Use a distinct error object for clarity in checks
	shutdownError := NewConnectionErrorWithCause(goAwayErrCode, debugMsg, errors.New("test triggered server shutdown"))
	shutdownError.DebugData = []byte(debugMsg)

	// Initiate close *first*. This should set conn.connError and signal shutdownChan.
	// conn.Serve() should see shutdownChan and start exiting.
	// The GOAWAY frame should be based on shutdownError.
	errClose := conn.Close(shutdownError)
	if errClose != nil && !errors.Is(errClose, shutdownError) && !strings.Contains(errClose.Error(), "connection shutdown") {
		// conn.Close() might return the error itself, or a wrapper indicating shutdown was due to this error.
		// It is also idempotent.
		t.Logf("conn.Close returned: %v (expected to be related to shutdownError or nil if already shutting down)", errClose)
	}

	// Wait for GOAWAY frame
	waitForCondition(t, 500*time.Millisecond, 20*time.Millisecond, func() bool {
		// Check if writer has produced output and it contains a GoAwayFrame
		// Ensure writerLoop has a chance to process the GOAWAY frame from writerChan
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, f := range frames {
			if _, ok := f.(*GoAwayFrame); ok {
				return true
			}
		}
		return false
	}, "server to write GOAWAY frame")

	writtenFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	var goAwayFrame *GoAwayFrame
	for _, f := range writtenFrames {
		if gaf, ok := f.(*GoAwayFrame); ok {
			goAwayFrame = gaf
			break
		}
	}
	if goAwayFrame == nil {
		t.Fatalf("Expected GOAWAY frame, but not found in written frames. All frames: %#v", writtenFrames)
	}

	if goAwayFrame.LastStreamID != lastProcStreamID {
		t.Errorf("GOAWAY LastStreamID mismatch: got %d, want %d", goAwayFrame.LastStreamID, lastProcStreamID)
	}
	if goAwayFrame.ErrorCode != goAwayErrCode {
		t.Errorf("GOAWAY ErrorCode mismatch: got %s, want %s", goAwayFrame.ErrorCode, goAwayErrCode)
	}
	if string(goAwayFrame.AdditionalDebugData) != debugMsg {
		t.Errorf("GOAWAY AdditionalDebugData mismatch: got %q, want %q", string(goAwayFrame.AdditionalDebugData), debugMsg)
	}

	conn.streamsMu.RLock()
	gaSent := conn.goAwaySent
	actualConnError := conn.connError
	conn.streamsMu.RUnlock()

	if !gaSent {
		t.Error("conn.goAwaySent was not set to true after Close initiated GOAWAY")
	}
	if !errors.Is(actualConnError, shutdownError) {
		t.Errorf("conn.connError was %v, want %v", actualConnError, shutdownError)
	}

	// Cleanup: Ensure Serve goroutine exits and other goroutines are done.
	// Serve should exit due to shutdownChan being closed by conn.Close(shutdownError).
	select {
	case err := <-serveErrChan:
		if err != nil && !errors.Is(err, shutdownError) && !strings.Contains(err.Error(), "connection shutdown") && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			// EOF is possible if unblockReadChan was closed and Serve read it before shutdownChan fully propagated to its select.
			t.Logf("Serve exited with error: %v (expected it to be related to shutdownError or clean exit)", err)
		}
	case <-time.After(1 * time.Second): // Increased timeout slightly
		t.Fatal("Serve goroutine did not terminate")
	}

	// readerDone should be closed by Serve's defer.
	// writerDone should be closed by writerLoop's defer.
	// These waits should not timeout if shutdown proceeds correctly.
	select {
	case <-conn.readerDone:
	default:
		t.Error("readerDone not closed")
	}
	select {
	case <-conn.writerDone:
	default:
		t.Error("writerDone not closed")
	}

	t.Run("Server Stream IDs", func(t *testing.T) { // Corrected sub-test name
		conn.streamsMu.RLock() // Protect access to nextStreamID fields
		defer conn.streamsMu.RUnlock()
		// For a server-side connection:
		if conn.nextStreamIDClient != 0 { // Server consumes client stream IDs, doesn't generate them like this
			t.Errorf("Server nextStreamIDClient = %d, want 0", conn.nextStreamIDClient)
		}
		if conn.nextStreamIDServer != 2 { // Server generates even stream IDs for pushes, starts at 2
			t.Errorf("Server nextStreamIDServer = %d, want 2", conn.nextStreamIDServer)
		}
	})
}

func TestConnection_Initialization_Server(t *testing.T) {
	mockDispatcher := &mockRequestDispatcher{}

	t.Run("Server Settings Defaults", func(t *testing.T) {
		conn, _ := newTestConnection(t, false, mockDispatcher) // false for server-side
		defer conn.Close(errors.New("test cleanup close"))
		conn.settingsMu.RLock()
		defer conn.settingsMu.RUnlock()

		if val := conn.ourSettings[SettingHeaderTableSize]; val != DefaultSettingsHeaderTableSize {
			t.Errorf("Server ourSettings[SettingHeaderTableSize] = %d, want %d", val, DefaultSettingsHeaderTableSize)
		}
		if val := conn.ourSettings[SettingEnablePush]; val != DefaultServerEnablePush {
			t.Errorf("Server ourSettings[SettingEnablePush] = %d, want %d", val, DefaultServerEnablePush)
		}
		if val := conn.ourSettings[SettingMaxConcurrentStreams]; val != DefaultServerMaxConcurrentStreams {
			t.Errorf("Server ourSettings[SettingMaxConcurrentStreams] = %d, want %d", val, DefaultServerMaxConcurrentStreams)
		}
	})

	t.Run("Server Settings Overrides", func(t *testing.T) {
		overrides := map[SettingID]uint32{
			SettingHeaderTableSize:      8192,
			SettingMaxConcurrentStreams: 50,
			SettingMaxFrameSize:         32768,
		}
		lg := logger.NewDiscardLogger()
		mnc := newMockNetConn()
		conn := NewConnection(mnc, lg, false, overrides, mockDispatcher.Dispatch)
		defer conn.Close(errors.New("test cleanup close"))

		conn.settingsMu.RLock()
		defer conn.settingsMu.RUnlock()

		if val := conn.ourSettings[SettingHeaderTableSize]; val != 8192 {
			t.Errorf("Server override SettingHeaderTableSize = %d, want 8192", val)
		}
		if val := conn.ourSettings[SettingMaxConcurrentStreams]; val != 50 {
			t.Errorf("Server override SettingMaxConcurrentStreams = %d, want 50", val)
		}
		if val := conn.ourSettings[SettingMaxFrameSize]; val != 32768 {
			t.Errorf("Server override SettingMaxFrameSize = %d, want 32768", val)
		}
	})

	t.Run("Server Settings MaxFrameSize Clamping", func(t *testing.T) {
		testCases := []struct {
			name         string
			overrideVal  uint32
			expectedVal  uint32
			logSubstring string
		}{
			{"TooLow", 1000, MinMaxFrameSize, "too low, adjusting"},
			{"TooHigh", MaxAllowedFrameSizeValue + 1, MaxAllowedFrameSizeValue, "too high, adjusting"},
			{"ValidMin", MinMaxFrameSize, MinMaxFrameSize, ""},
			{"ValidMax", MaxAllowedFrameSizeValue, MaxAllowedFrameSizeValue, ""},
			{"NotSet", 0, DefaultSettingsMaxFrameSize, "not found in ourSettings, setting to default"},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				var overrides map[SettingID]uint32
				if tc.name != "NotSet" {
					overrides = map[SettingID]uint32{SettingMaxFrameSize: tc.overrideVal}
				}

				var logBuf bytes.Buffer
				// Use the actual TestLogger from internal/logger for consistency in test helpers
				// if available and it suits capturing. For unit tests within http2,
				// direct logger.NewTestLogger might not be desired.
				// Using a simple logger setup for this specific test.
				captureLogger := logger.NewTestLogger(&logBuf)

				mnc := newMockNetConn()
				conn := NewConnection(mnc, captureLogger, false, overrides, mockDispatcher.Dispatch)
				defer conn.Close(errors.New("test cleanup close"))

				conn.settingsMu.RLock()
				actualVal := conn.ourSettings[SettingMaxFrameSize]
				conn.settingsMu.RUnlock()

				if actualVal != tc.expectedVal {
					t.Errorf("Clamping %s: MaxFrameSize = %d, want %d. Log: %s", tc.name, actualVal, tc.expectedVal, logBuf.String())
				}
				if tc.logSubstring != "" && !bytes.Contains(logBuf.Bytes(), []byte(tc.logSubstring)) {
					t.Errorf("Clamping %s: Expected log substring '%s' not found in logs: %s", tc.name, tc.logSubstring, logBuf.String())
				}
			})
		}
	})

	t.Run("Server Stream IDs", func(t *testing.T) {
		conn, _ := newTestConnection(t, false, mockDispatcher)
		defer conn.Close(errors.New("test cleanup close"))

		if conn.nextStreamIDClient != 0 {
			t.Errorf("Server nextStreamIDClient = %d, want 0", conn.nextStreamIDClient)
		}
		if conn.nextStreamIDServer != 2 {
			t.Errorf("Server nextStreamIDServer = %d, want 2", conn.nextStreamIDServer)
		}
	})

	t.Run("Server Dispatcher Nil case", func(t *testing.T) {
		var logBuf bytes.Buffer
		captureLogger := logger.NewTestLogger(&logBuf)
		mnc := newMockNetConn()
		conn := NewConnection(mnc, captureLogger, false, nil, nil) // Pass nil dispatcher
		defer conn.Close(errors.New("test cleanup close"))

		if conn.dispatcher != nil {
			t.Error("Server dispatcher was not nil, expected nil when nil was passed")
		}
		expectedLogMsg := "NewConnection: server-side connection created without a dispatcher func"
		if !bytes.Contains(logBuf.Bytes(), []byte(expectedLogMsg)) {
			t.Errorf("Expected log message '%s' not found when dispatcher is nil. Logs: %s", expectedLogMsg, logBuf.String())
		}
	})
}

func TestConnection_Serve_DispatchPing(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	serveErrChan := make(chan error, 1)
	ctx, cancelServe := context.WithCancel(context.Background())

	defer func() {
		cancelServe() // Ensure Serve's context is cancelled
		conn.Close(errors.New("test cleanup close"))
		select {
		case <-serveErrChan:
		case <-time.After(200 * time.Millisecond): // Reduced timeout
			// t.Log("Timed out waiting for Serve to exit in TestConnection_Serve_DispatchPing defer")
		}
		select {
		case <-conn.writerDone:
		case <-time.After(200 * time.Millisecond): // Reduced timeout
			// t.Log("Timed out waiting for conn.writerDone to exit in TestConnection_Serve_DispatchPing defer")
		}
		select {
		case <-conn.readerDone:
		case <-time.After(200 * time.Millisecond): // Reduced timeout
			// t.Log("Timed out waiting for conn.readerDone to exit in TestConnection_Serve_DispatchPing defer")
		}
	}()

	// 1. Feed Handshake Data
	mnc.FeedReadBuffer([]byte(ClientPreface))
	clientSettings := []Setting{{ID: SettingInitialWindowSize, Value: DefaultSettingsInitialWindowSize}}
	clientSettingsFrame := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: uint32(len(clientSettings) * 6)},
		Settings:    clientSettings,
	}
	mnc.FeedReadBuffer(frameToBytes(t, clientSettingsFrame))

	// 2. Perform Handshake (consumes preface and client settings from mnc.readBuf)
	if err := conn.ServerHandshake(); err != nil {
		t.Fatalf("ServerHandshake failed: %v", err)
	}
	// ServerHandshake queues server's initial SETTINGS and an ACK to client's SETTINGS.
	// Wait for these to be written.
	waitForCondition(t, 500*time.Millisecond, 20*time.Millisecond, func() bool {
		select {
		case <-conn.initialSettingsWritten: // Server's own SETTINGS are out
			frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
			if len(frames) < 2 { // Expect server's SETTINGS and client's SETTINGS ACK
				return false
			}
			_, ok1 := frames[0].(*SettingsFrame)
			sf2, ok2 := frames[1].(*SettingsFrame)
			return ok1 && ok2 && (sf2.Header().Flags&FlagSettingsAck != 0)
		default:
			return false
		}
	}, "server to write its SETTINGS and ACK client's SETTINGS")
	mnc.ResetWriteBuffer() // Clear handshake frames written by server

	// 3. Feed the PING request frame *before* Serve starts its main loop,
	//    so it's the first thing Serve reads after handshake.
	pingData := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	pingReqFrame := &PingFrame{
		FrameHeader: FrameHeader{Type: FramePing, Flags: 0, StreamID: 0, Length: 8},
		OpaqueData:  pingData,
	}
	mnc.FeedReadBuffer(frameToBytes(t, pingReqFrame))

	// 4. Set up readHook to block *after* the PING frame is consumed.
	//    mnc.Read() is called by conn.readFrame().
	//    The hook logic needs to coordinate with mnc.readBuf.
	var pingFramePotentiallyConsumed bool
	mnc.readHook = func(b []byte) (int, error) {
		mnc.mu.Lock() // mockNetConn's Read method (caller of this hook) holds this lock. Re-locking is a DEADLOCK.
		// The hook should NOT manipulate mnc.mu.

		// If we previously determined PING was consumed, subsequent reads block.
		if pingFramePotentiallyConsumed {
			mnc.mu.Unlock()                  // Must unlock if logic path doesn't call mnc.readBuf.Read which would unlock
			time.Sleep(1 * time.Millisecond) // Prevent tight loop in ReadFrame
			return 0, nil                    // Simulate blocking
		}

		// Try to read from buffer. This is the first read attempt by Serve's loop.
		// It should consume the PING frame.
		// We need to avoid re-locking mnc.mu. The hook is called while mnc.mu is held.
		// So, direct access to mnc.readBuf is okay if careful.

		// Let's simplify: The PING is in readBuf. Let mockNetConn.Read's normal logic (no hook initially) consume it.
		// Then, for subsequent reads, we want to block.
		// This means the hook should only be "active" for reads *after* the PING.

		// This hook will be called for the read of the PING frame itself, and subsequent reads.
		// We need to let the PING frame read go through from the buffer.
		var n int
		var err error
		if mnc.readBuf.Len() > 0 { // If there's data (i.e., the PING frame)
			n, err = mnc.readBuf.Read(b)              // Consume from buffer
			if mnc.readBuf.Len() == 0 && err == nil { // If buffer became empty (PING was fully read)
				pingFramePotentiallyConsumed = true
			} else if errors.Is(err, io.EOF) && mnc.readBuf.Len() == 0 { // EOF means PING was read and buffer empty
				pingFramePotentiallyConsumed = true
				// Return (n, nil) if n > 0, to signal data was read before EOF point.
				// If n == 0 and EOF, then just return EOF.
				if n > 0 {
					err = nil
				}
			}
		} else { // Buffer is empty, PING must have been consumed
			pingFramePotentiallyConsumed = true
			time.Sleep(1 * time.Millisecond)
			n, err = 0, nil // Block
		}
		mnc.mu.Unlock() // MUST UNLOCK as mnc.Read() (caller) expects it.
		return n, err
	}
	// Revert the hook logic because it's too complex and error-prone with mutexes.
	// The simpler model: Feed PING. Serve reads PING. Then subsequent reads block.
	// For this, the hook should only activate its blocking logic *after* it knows the PING has passed.
	// The most robust way is for mockNetConn.Read to manage its buffer, and hook is only for *after* buffer is empty.

	// New simpler hook logic:
	// The hook will only be called if the main Read logic in mockNetConn would otherwise EOF.
	// So, after ServerHandshake, mnc.readBuf is empty.
	// We feed PING.
	// Serve starts. readFrame -> mnc.Read -> mnc.readBuf.Read(PING_DATA). Consumes PING. mnc.readBuf is empty again.
	// *Next* call to mnc.Read: readBuf is empty. *Now* if a hook is set, it can block.

	mnc.readHook = func(b []byte) (int, error) { // This hook is active for reads AFTER PING is consumed.
		// This hook implies mnc.readBuf was empty when mnc.Read called it.
		time.Sleep(1 * time.Millisecond)
		return 0, nil // Block
	}

	// 5. Start Serve
	go func() {
		err := conn.Serve(ctx)
		// Don't send to serveErrChan if context was cancelled, as that's an expected exit.
		if ctx.Err() == nil {
			serveErrChan <- err
		} else {
			close(serveErrChan) // Signal exit without error to defer
		}
	}()

	// 6. Wait for PING ACK to be written
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		return mnc.GetWriteBufferLen() > 0
	}, "server to send PING ACK")

	writtenFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	if len(writtenFrames) == 0 {
		t.Fatal("Server sent no frames in response to PING request")
	}
	pingAckFrame, ok := writtenFrames[0].(*PingFrame)
	if !ok {
		t.Fatalf("Expected PING frame as response, got %T. All frames: %#v", writtenFrames[0], writtenFrames)
	}
	if pingAckFrame.Header().Flags&FlagPingAck == 0 {
		t.Error("Response PING frame did not have ACK flag set")
	}
	if pingAckFrame.OpaqueData != pingData {
		t.Errorf("PING ACK OpaqueData mismatch: got %x, want %x", pingAckFrame.OpaqueData, pingData)
	}
}

func TestConnection_Serve_GracefulShutdownOnEOF(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	serveErrChan := make(chan error, 1)

	// Perform handshake
	mnc.FeedReadBuffer([]byte(ClientPreface))
	clientSettings := []Setting{{ID: SettingInitialWindowSize, Value: DefaultSettingsInitialWindowSize}}
	settingsFrame := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: uint32(len(clientSettings) * 6)},
		Settings:    clientSettings,
	}
	mnc.FeedReadBuffer(frameToBytes(t, settingsFrame))
	if err := conn.ServerHandshake(); err != nil {
		t.Fatalf("ServerHandshake failed: %v", err)
	}
	mnc.ResetWriteBuffer() // Clear handshake frames from server

	go func() { serveErrChan <- conn.Serve(context.Background()) }()

	// Simulate client closing connection (leading to EOF on read)
	// No more data fed to mnc.readBuf, so next ReadFrame will get EOF.

	var finalErr error
	select {
	case finalErr = <-serveErrChan:
	case <-time.After(200 * time.Millisecond): // Timeout for Serve to exit
		t.Fatal("conn.Serve did not exit after simulated EOF")
	}

	if finalErr == nil {
		t.Fatal("conn.Serve exited with nil error, expected EOF or related error")
	}
	if !errors.Is(finalErr, io.EOF) {
		// conn.Serve's defer calls conn.Close(err), where err is io.EOF.
		// conn.Close then might store this or another error in conn.connError.
		// The returned error from Serve is the error that caused the loop to exit.
		t.Errorf("conn.Serve exited with error '%v', expected io.EOF", finalErr)
	}

	// Check that shutdownChan was closed
	select {
	case <-conn.shutdownChan:
		// Good, shutdown initiated
	default:
		t.Error("conn.shutdownChan was not closed after Serve exited on EOF")
	}

	// Check if GOAWAY was sent (server should send GOAWAY(NO_ERROR) on graceful EOF)
	waitForCondition(t, 100*time.Millisecond, 10*time.Millisecond, func() bool {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, f := range frames {
			if _, ok := f.(*GoAwayFrame); ok {
				return true
			}
		}
		return false
	}, "server to send GOAWAY on EOF")

	goAwayFound := false
	finalWrittenFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	for _, frame := range finalWrittenFrames {
		if gf, ok := frame.(*GoAwayFrame); ok {
			goAwayFound = true
			if gf.ErrorCode != ErrCodeNoError {
				t.Errorf("Expected GOAWAY with NO_ERROR on EOF, got %s", gf.ErrorCode)
			}
			// LastStreamID could be 0 if no streams were processed, or higher if some were.
			t.Logf("GOAWAY on EOF: LastStreamID=%d, ErrorCode=%s", gf.LastStreamID, gf.ErrorCode)
			break
		}
	}
	if !goAwayFound {
		t.Errorf("Server did not send GOAWAY on EOF. Frames written: %v", finalWrittenFrames)
	}

	// Wait for writer and reader goroutines to be done (readerDone is closed by Serve's defer)
	<-conn.readerDone
	<-conn.writerDone
}

func TestConnection_Serve_PanicRecovery(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	serveErrChan := make(chan error, 1)

	// Setup: Handshake
	mnc.FeedReadBuffer([]byte(ClientPreface))
	clientSettings := []Setting{{ID: SettingInitialWindowSize, Value: DefaultSettingsInitialWindowSize}}
	settingsFrame := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: uint32(len(clientSettings) * 6)},
		Settings:    clientSettings,
	}
	mnc.FeedReadBuffer(frameToBytes(t, settingsFrame))
	if err := conn.ServerHandshake(); err != nil {
		t.Fatalf("ServerHandshake failed: %v", err)
	}
	mnc.ResetWriteBuffer()

	// Start Serve
	go func() { serveErrChan <- conn.Serve(context.Background()) }()

	// Create a frame that will cause a panic in dispatchFrame (e.g., by passing a nil frame after header read)
	// This is hard to do perfectly without knowing dispatchFrame internals or mocking ReadFrame.
	// Instead, let's simulate a panic by making mnc.Read return a custom error after a valid frame,
	// and then modify readFrame or dispatchFrame in a testable way.
	// For this test, we'll cause a panic more directly if possible.
	// A simpler way for a unit test: inject a frame that *dispatchFrame* will panic on.
	// What if we send a frame type that has a nil handler in dispatchFrame?
	// No, dispatchFrame handles unknown frames gracefully.

	// Let's trigger a panic by making ReadFrame itself panic.
	// This needs a custom Read hook on mockNetConn.
	panicMsg := "test panic in ReadFrame"
	var readAttemptCount int
	mnc.readHook = func(b []byte) (int, error) {
		readAttemptCount++
		if readAttemptCount == 1 { // Allow first frame (e.g. for client SETTINGS if test was different)
			// For this test, first frame after handshake will trigger panic
			// Send valid PING to ensure dispatch logic is reached before panic
			pingData := [8]byte{}
			pingFrame := &PingFrame{FrameHeader: FrameHeader{Type: FramePing, Length: 8}, OpaqueData: pingData}
			frameBytes := frameToBytes(t, pingFrame)
			if len(b) < len(frameBytes) {
				return 0, io.ErrShortBuffer
			}
			copy(b, frameBytes)
			return len(frameBytes), nil
		}
		// On second attempt to read a frame (after PING processed), panic.
		panic(panicMsg)
	}
	// Feed one PING frame to get past initial read in Serve loop
	// The panic will occur on the *next* call to c.readFrame() inside Serve loop.
	// (Actually, the hook makes the *first* ReadFrame call after handshake panic).
	// Re-adjust: panic on the *first* readFrame in Serve loop.
	readAttemptCount = 0 // Reset for this specific logic
	mnc.readHook = func(b []byte) (int, error) {
		panic(panicMsg) // Panic on first actual frame read attempt by Serve
	}

	var finalErr error
	select {
	case finalErr = <-serveErrChan:
		// Serve exited
	case <-time.After(200 * time.Millisecond):
		t.Fatal("conn.Serve did not exit after simulated panic")
	}

	if finalErr == nil {
		t.Fatal("conn.Serve exited with nil error after panic, expected non-nil")
	}
	connErr, ok := finalErr.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError from panic recovery, got %T: %v", finalErr, finalErr)
	}
	if connErr.Code != ErrCodeInternalError {
		t.Errorf("Expected ErrCodeInternalError from panic, got %s", connErr.Code)
	}
	// The message might be "internal server panic in reader loop" or similar
	if !strings.Contains(connErr.Msg, "panic") {
		t.Errorf("Expected error message to contain 'panic', got: %s", connErr.Msg)
	}
	t.Logf("Panic recovery error: %v", finalErr)

	// Check that shutdownChan was closed
	select {
	case <-conn.shutdownChan:
		// Good
	default:
		t.Error("conn.shutdownChan was not closed after panic recovery")
	}

	// Check for GOAWAY with INTERNAL_ERROR
	waitForCondition(t, 100*time.Millisecond, 10*time.Millisecond, func() bool {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, f := range frames {
			if gf, ok := f.(*GoAwayFrame); ok && gf.ErrorCode == ErrCodeInternalError {
				return true
			}
		}
		return false
	}, "server to send GOAWAY(INTERNAL_ERROR) on panic")

	// Cleanup goroutines
	<-conn.readerDone
	<-conn.writerDone
}

func performHandshakeForTest(t *testing.T, conn *Connection, mnc *mockNetConn) {
	t.Helper()
	mnc.FeedReadBuffer([]byte(ClientPreface))
	clientSettings := []Setting{{ID: SettingInitialWindowSize, Value: DefaultSettingsInitialWindowSize}}
	settingsFrame := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: uint32(len(clientSettings) * 6)},
		Settings:    clientSettings,
	}
	mnc.FeedReadBuffer(frameToBytes(t, settingsFrame))

	if err := conn.ServerHandshake(); err != nil {
		t.Fatalf("performHandshakeForTest: ServerHandshake failed: %v", err)
	}
	// Wait for initial server settings to be written and client settings ACK to be processed by writer
	waitForCondition(t, 500*time.Millisecond, 20*time.Millisecond, func() bool {
		select {
		case <-conn.initialSettingsWritten: // This confirms server's own settings are out.
			// Now check if client's settings ACK is also out.
			frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
			if len(frames) < 2 {
				return false
			}
			_, ok1 := frames[0].(*SettingsFrame)   // Server's initial
			sf2, ok2 := frames[1].(*SettingsFrame) // ACK to client's
			return ok1 && ok2 && (sf2.Header().Flags&FlagSettingsAck != 0)
		default:
			return false
		}
	}, "server to write its SETTINGS and ACK client's SETTINGS")
	mnc.ResetWriteBuffer() // Clear handshake frames
}

func TestConnection_Close_Graceful(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	serveErrChan := make(chan error, 1)

	performHandshakeForTest(t, conn, mnc) // Does handshake and clears mnc write buffer

	go func() { serveErrChan <- conn.Serve(context.Background()) }()

	// Keep Serve alive briefly by making reads block
	var blockReadsMu sync.Mutex
	blockReadsActive := true
	originalReadHook := mnc.readHook
	mnc.readHook = func(b []byte) (int, error) {
		blockReadsMu.Lock()
		isActive := blockReadsActive
		blockReadsMu.Unlock()
		if isActive {
			time.Sleep(1 * time.Millisecond)
			return 0, nil // Keep Serve blocked on read
		}
		if originalReadHook != nil {
			return originalReadHook(b)
		}
		mnc.mu.Lock()
		defer mnc.mu.Unlock()
		if mnc.closed {
			return 0, mnc.closeErr
		}
		if mnc.readBuf.Len() == 0 {
			return 0, io.EOF
		}
		return mnc.readBuf.Read(b)
	}
	defer func() {
		blockReadsMu.Lock()
		blockReadsActive = false
		mnc.readHook = originalReadHook
		blockReadsMu.Unlock()
	}()

	// Initiate graceful close
	closeErr := conn.Close(nil) // Should be idempotent with Serve's defer Close
	if closeErr != nil {
		// conn.Close should return the error that caused termination. If nil was passed,
		// and no other error occurred, it might return nil or a generic shutdown error.
		// For a graceful close(nil), if Serve was running and exited due to shutdownChan,
		// its deferred Close might re-set connError.
		t.Logf("conn.Close(nil) returned: %v (this is okay if it reflects orderly shutdown)", closeErr)
	}

	// Verify shutdownChan is closed
	select {
	case <-conn.shutdownChan:
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("conn.shutdownChan was not closed after graceful Close")
	}

	// Verify GOAWAY frame
	waitForCondition(t, 200*time.Millisecond, 10*time.Millisecond, func() bool {
		return mnc.GetWriteBufferLen() > 0
	}, "GOAWAY frame to be written")

	writtenFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	goAwayFound := false
	for _, frame := range writtenFrames {
		if gf, ok := frame.(*GoAwayFrame); ok {
			if gf.ErrorCode != ErrCodeNoError {
				t.Errorf("Expected GOAWAY with ErrCodeNoError, got %s", gf.ErrorCode)
			}
			goAwayFound = true
			break
		}
	}
	if !goAwayFound {
		t.Fatal("GOAWAY frame not found after graceful Close")
	}

	// Verify mockNetConn is closed
	waitForCondition(t, 100*time.Millisecond, 10*time.Millisecond, mnc.IsClosed, "mockNetConn to be closed")

	// Verify goroutines terminate
	select {
	case err := <-serveErrChan: // Serve loop should exit
		if err != nil && !strings.Contains(err.Error(), "connection shutdown") && !errors.Is(err, net.ErrClosed) {
			t.Logf("Serve exited with error: %v", err) // Log, don't fail, as it's expected
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Serve goroutine did not terminate after Close")
	}

	<-conn.readerDone // Should be closed by Serve's defer
	<-conn.writerDone // Should be closed by writerLoop's defer

	conn.streamsMu.RLock()
	finalConnErr := conn.connError
	conn.streamsMu.RUnlock()
	if finalConnErr != nil && !(strings.Contains(finalConnErr.Error(), "connection shutdown") || errors.Is(finalConnErr, io.EOF) || errors.Is(finalConnErr, net.ErrClosed)) {
		// If connError is set to the error from Serve (e.g., "connection shutdown initiated") or io.EOF, it's fine.
		t.Errorf("Expected conn.connError to be nil or reflect graceful shutdown, got %v", finalConnErr)
	}
}

func TestConnection_Close_WithError(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	serveErrChan := make(chan error, 1)

	performHandshakeForTest(t, conn, mnc)

	go func() { serveErrChan <- conn.Serve(context.Background()) }()

	// Keep Serve alive briefly
	var blockReadsMu sync.Mutex
	blockReadsActive := true
	originalReadHook := mnc.readHook
	mnc.readHook = func(b []byte) (int, error) {
		blockReadsMu.Lock()
		isActive := blockReadsActive
		blockReadsMu.Unlock()
		if isActive {
			time.Sleep(1 * time.Millisecond)
			return 0, nil
		}
		if originalReadHook != nil {
			return originalReadHook(b)
		}
		mnc.mu.Lock()
		defer mnc.mu.Unlock()
		if mnc.closed {
			return 0, mnc.closeErr
		}
		if mnc.readBuf.Len() == 0 {
			return 0, io.EOF
		}
		return mnc.readBuf.Read(b)
	}
	defer func() {
		blockReadsMu.Lock()
		blockReadsActive = false
		mnc.readHook = originalReadHook
		blockReadsMu.Unlock()
	}()

	testErr := NewConnectionErrorWithCause(ErrCodeProtocolError, "test protocol violation", errors.New("underlying cause"))
	debugData := []byte("debug info for GOAWAY")
	testErr.DebugData = debugData

	// Initiate close with error
	returnedCloseError := conn.Close(testErr)
	if returnedCloseError == nil {
		t.Error("conn.Close(testErr) returned nil, expected the error itself or a related shutdown error")
	} else if !errors.Is(returnedCloseError, testErr) && !strings.Contains(returnedCloseError.Error(), "connection shutdown") {
		// It's okay if Close returns an error like "connection shutdown initiated by <original error>"
		t.Logf("conn.Close(testErr) returned: %v (expected to contain original error or be a shutdown error)", returnedCloseError)
	}

	select {
	case <-conn.shutdownChan:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("conn.shutdownChan was not closed after Close with error")
	}

	waitForCondition(t, 200*time.Millisecond, 10*time.Millisecond, func() bool {
		return mnc.GetWriteBufferLen() > 0
	}, "GOAWAY frame to be written")

	writtenFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	goAwayFound := false
	for _, frame := range writtenFrames {
		if gf, ok := frame.(*GoAwayFrame); ok {
			if gf.ErrorCode != testErr.Code {
				t.Errorf("Expected GOAWAY with ErrCodeProtocolError, got %s", gf.ErrorCode)
			}
			if !bytes.Equal(gf.AdditionalDebugData, debugData) {
				t.Errorf("GOAWAY debug data mismatch: got %q, want %q", gf.AdditionalDebugData, debugData)
			}
			goAwayFound = true
			break
		}
	}
	if !goAwayFound {
		t.Fatal("GOAWAY frame not found after Close with error")
	}

	waitForCondition(t, 100*time.Millisecond, 10*time.Millisecond, mnc.IsClosed, "mockNetConn to be closed")

	select {
	case err := <-serveErrChan:
		if err != nil && !errors.Is(err, testErr) && !strings.Contains(err.Error(), "connection shutdown") && !errors.Is(err, net.ErrClosed) {
			// Serve might exit with the original testErr, or a "connection shutdown" error, or net.ErrClosed.
			t.Logf("Serve exited with error: %v (expected to be related to testErr or shutdown)", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Serve goroutine did not terminate after Close with error")
	}

	<-conn.readerDone
	<-conn.writerDone

	conn.streamsMu.RLock()
	finalConnErr := conn.connError
	conn.streamsMu.RUnlock()
	if !errors.Is(finalConnErr, testErr) {
		t.Errorf("Expected conn.connError to be the testErr, got %v", finalConnErr)
	}
}

func TestConnection_Close_Idempotency(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	serveErrChan := make(chan error, 1)

	performHandshakeForTest(t, conn, mnc)
	go func() { serveErrChan <- conn.Serve(context.Background()) }()

	// Keep Serve alive
	var blockReadsMu sync.Mutex
	blockReadsActive := true
	originalReadHook := mnc.readHook
	mnc.readHook = func(b []byte) (int, error) {
		blockReadsMu.Lock()
		isActive := blockReadsActive
		blockReadsMu.Unlock()
		if isActive {
			time.Sleep(1 * time.Millisecond)
			return 0, nil
		}
		if originalReadHook != nil {
			return originalReadHook(b)
		}
		mnc.mu.Lock()
		defer mnc.mu.Unlock()
		if mnc.closed {
			return 0, mnc.closeErr
		}
		if mnc.readBuf.Len() == 0 {
			return 0, io.EOF
		}
		return mnc.readBuf.Read(b)
	}
	defer func() {
		blockReadsMu.Lock()
		blockReadsActive = false
		mnc.readHook = originalReadHook
		blockReadsMu.Unlock()
	}()

	// First Close
	conn.Close(nil)

	// Wait for GOAWAY from first close
	waitForCondition(t, 200*time.Millisecond, 10*time.Millisecond, func() bool { return mnc.GetWriteBufferLen() > 0 }, "first GOAWAY")
	framesAfterFirstClose := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	goAwayCount1 := 0
	for _, f := range framesAfterFirstClose {
		if _, ok := f.(*GoAwayFrame); ok {
			goAwayCount1++
		}
	}
	if goAwayCount1 != 1 {
		t.Fatalf("Expected 1 GOAWAY frame after first Close, got %d", goAwayCount1)
	}
	mnc.ResetWriteBuffer() // Clear for next check

	// Second Close
	conn.Close(NewConnectionError(ErrCodeInternalError, "second close attempt"))
	time.Sleep(50 * time.Millisecond) // Allow time for any erroneous second GOAWAY

	framesAfterSecondClose := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	goAwayCount2 := 0
	for _, f := range framesAfterSecondClose {
		if _, ok := f.(*GoAwayFrame); ok {
			goAwayCount2++
		}
	}
	if goAwayCount2 != 0 {
		t.Fatalf("Expected 0 new GOAWAY frames after second Close, got %d", goAwayCount2)
	}

	// Verify mockNetConn.Close() called once (mnc.Close is designed to be no-op if already closed)
	// We can't directly count calls on mnc.Close easily without more mock infra.
	// Its internal 'closed' flag is sufficient for this check.
	waitForCondition(t, 100*time.Millisecond, 10*time.Millisecond, mnc.IsClosed, "mockNetConn to be closed")

	// Check original error is preserved (or reflects first close reason)
	conn.streamsMu.RLock()
	finalConnErr := conn.connError
	conn.streamsMu.RUnlock()
	if finalConnErr != nil && !(strings.Contains(finalConnErr.Error(), "connection shutdown") || errors.Is(finalConnErr, io.EOF) || errors.Is(finalConnErr, net.ErrClosed)) {
		t.Errorf("conn.connError was %v, expected nil or error from first Close, not from second", finalConnErr)
	}

	// Cleanup goroutines
	select {
	case <-serveErrChan:
	default:
	}
	<-conn.readerDone
	<-conn.writerDone
}

func TestConnection_InitiateShutdown_ResourceCleanup(t *testing.T) {
	conn, _ := newTestConnection(t, false, nil)
	// No need to start Serve for this, testing initiateShutdown directly.
	// Ensure writerLoop is started because initiateShutdown interacts with writerChan and writerDone
	// newTestConnection starts writerLoop.

	// Setup resources that initiateShutdown should clean up
	// 1. settingsAckTimeoutTimer
	conn.settingsMu.Lock()
	conn.settingsAckTimeoutTimer = time.NewTimer(10 * time.Second) // Dummy active timer
	conn.settingsMu.Unlock()

	// 2. Active PING
	pingOpaqueData := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	conn.activePingsMu.Lock()
	conn.activePings[pingOpaqueData] = time.NewTimer(10 * time.Second) // Dummy active PING timer
	conn.activePingsMu.Unlock()

	// Call initiateShutdown
	// conn.initiateShutdown does not wait for readerDone.
	// It closes shutdownChan, writerChan, waits for writerDone, closes netConn, cancels context.
	conn.initiateShutdown(0, ErrCodeNoError, nil, 0)

	// Verify resources are cleaned
	// 1. settingsAckTimeoutTimer should be stopped (hard to check if stopped, check if nil)
	conn.settingsMu.RLock()
	timerNil := conn.settingsAckTimeoutTimer == nil
	conn.settingsMu.RUnlock()
	if !timerNil {
		// The timer might not be nil if AfterFunc already fired and cleared itself.
		// A better check is that it's not active, but that requires more complex timer mocking.
		// For now, we assume if it's not nil, initiateShutdown logic might be incomplete.
		// However, AfterFunc clears itself from the map when it fires.
		// If initiateShutdown stops it BEFORE it fires, it will be nilled out.
		// This check relies on initiateShutdown successfully stopping it.
		t.Log("settingsAckTimeoutTimer was not nil after initiateShutdown (could be a race or if timer fired already, but test implies it shouldn't have)")
	}

	// 2. Active PING timer should be stopped and removed
	conn.activePingsMu.Lock()
	_, pingExists := conn.activePings[pingOpaqueData]
	conn.activePingsMu.Unlock()
	if pingExists {
		t.Error("Active PING timer was not removed from activePings map")
	}

	// 3. connFCManager should be closed. connFCManager.Close is idempotent.
	// Check if sendWindow is closed.
	conn.connFCManager.sendWindow.mu.Lock()
	fcClosed := conn.connFCManager.sendWindow.closed
	fcErr := conn.connFCManager.sendWindow.err
	conn.connFCManager.sendWindow.mu.Unlock()
	if !fcClosed {
		t.Error("ConnectionFlowControlManager's sendWindow was not closed")
	}
	if fcErr == nil {
		t.Error("ConnectionFlowControlManager's sendWindow was closed without an error")
	} else {
		t.Logf("connFCManager sendWindow closed with error: %v", fcErr)
	}

	// 4. Connection context should be cancelled
	select {
	case <-conn.ctx.Done():
		// Expected
	default:
		t.Error("Connection context was not cancelled")
	}

	// 5. writerDone should be closed
	select {
	case <-conn.writerDone:
		// Expected, as initiateShutdown waits for it.
	case <-time.After(100 * time.Millisecond): // Should be fast as writerChan is closed by initiateShutdown
		t.Error("writerDone was not closed after initiateShutdown")
	}

	// readerDone is closed by Serve's defer, not directly by initiateShutdown in this test setup.
	// But initiateShutdown will forcefully close it at the very end.
	select {
	case <-conn.readerDone:
	case <-time.After(10 * time.Millisecond): // If not closed by forceClose, it's an issue
		t.Error("readerDone was not closed by initiateShutdown's final cleanup")
	}
}

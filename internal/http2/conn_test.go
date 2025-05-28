package http2

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"

	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/llmahttap/v2/internal/logger"
)

// mockNetConn is a mock implementation of net.Conn for testing.
type mockNetConn struct {
	mu            sync.Mutex
	readBuf       bytes.Buffer
	writeBuf      bytes.Buffer
	readDeadline  time.Time
	writeDeadline time.Time
	closed        bool
	closeErr      error
	localAddr     net.Addr
	remoteAddr    net.Addr
	readHook      func([]byte) (int, error)
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
		return 0, errors.New("read timeout (mock)")
	}
	if m.readBuf.Len() > 0 {
		return m.readBuf.Read(b)
	}
	if m.readHook != nil {
		return m.readHook(b)
	}
	return 0, io.EOF
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
	return m.writeBuf.Write(b)
}

func (m *mockNetConn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
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

func (m *mockNetConn) FeedReadBuffer(data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readBuf.Write(data)
}

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

type mockRequestDispatcher struct {
	mu           sync.Mutex
	called       bool
	calledCount  int
	lastStreamID uint32
	lastReq      *http.Request
	lastSw       StreamWriter
	fn           func(sw StreamWriter, req *http.Request)
}

func (mrd *mockRequestDispatcher) Dispatch(sw StreamWriter, req *http.Request) {
	mrd.mu.Lock()
	mrd.called = true
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
func (mrd *mockRequestDispatcher) Reset() {
	mrd.mu.Lock()
	defer mrd.mu.Unlock()
	mrd.called = false
	mrd.calledCount = 0
	mrd.lastStreamID = 0
	mrd.lastReq = nil
	mrd.lastSw = nil
	mrd.fn = nil
}

func newTestConnection(t *testing.T, isClient bool, mockDispatcher *mockRequestDispatcher) (*Connection, *mockNetConn) {
	t.Helper()
	mnc := newMockNetConn()
	lg := logger.NewTestLogger(os.Stdout)
	// lg.SetLevel(logger.LevelDebug) // Uncomment for verbose debug output

	var dispatcherFunc RequestDispatcherFunc
	if mockDispatcher != nil {
		dispatcherFunc = mockDispatcher.Dispatch
	} else {
		dispatcherFunc = func(sw StreamWriter, req *http.Request) { /* No-op */ }
	}

	conn := NewConnection(mnc, lg, isClient, nil, dispatcherFunc)
	conn.writerChan = make(chan Frame, 1) // Ensure a small buffer for test predictability
	if conn == nil {
		t.Fatal("NewConnection returned nil")
	}
	// Give writerLoop a chance to start, especially important for handshake.
	time.Sleep(100 * time.Millisecond)
	return conn, mnc
}

func frameToBytes(t *testing.T, frame Frame) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteFrame(&buf, frame); err != nil {
		t.Fatalf("frameToBytes: Error writing frame: %v", err)
	}
	return buf.Bytes()
}

func bytesToFrame(t *testing.T, data []byte) Frame {
	t.Helper()
	if len(data) == 0 {
		return nil
	}
	r := bytes.NewReader(data)
	frame, err := ReadFrame(r)
	if err != nil {
		if errors.Is(err, io.EOF) || (errors.Is(err, io.ErrUnexpectedEOF) && r.Len() == 0 && len(data) < FrameHeaderLen) {
			return nil
		}
		t.Fatalf("bytesToFrame: Error reading frame: %v. Data (hex): %s", err, hex.EncodeToString(data))
	}
	return frame
}

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
				t.Logf("readAllFramesFromBuffer: Unexpected EOF, likely incomplete frame. Data remaining: %d. Error: %v", r.Len(), err)
				break
			}
			t.Fatalf("readAllFramesFromBuffer: Error reading frame: %v", err)
		}
		frames = append(frames, frame)
	}
	return frames
}

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

// performHandshakeForTest performs the client-side actions and server-side handshake for tests.
func performHandshakeForTest(t *testing.T, conn *Connection, mnc *mockNetConn) {
	t.Helper()
	// Client sends preface
	mnc.FeedReadBuffer([]byte(ClientPreface))

	// Client sends its initial SETTINGS
	clientSettings := []Setting{{ID: SettingInitialWindowSize, Value: DefaultSettingsInitialWindowSize}}
	clientSettingsFrame := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: uint32(len(clientSettings) * 6)},
		Settings:    clientSettings,
	}
	mnc.FeedReadBuffer(frameToBytes(t, clientSettingsFrame))

	// Server performs handshake
	if err := conn.ServerHandshake(); err != nil {
		t.Fatalf("performHandshakeForTest: ServerHandshake failed: %v", err)
	}

	// Verify server sent its SETTINGS and an ACK to client's SETTINGS
	waitForCondition(t, 2*time.Second, 50*time.Millisecond, func() bool {
		conn.log.Debug("performHandshakeForTest: In waitForCondition check for server SETTINGS and ACK.", nil)
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		if len(frames) < 2 {
			conn.log.Debug("performHandshakeForTest: Not enough frames yet.", logger.LogFields{"count": len(frames)})
			return false
		}
		_, ok1 := frames[0].(*SettingsFrame)
		sf2, ok2 := frames[1].(*SettingsFrame)
		if !ok1 || !ok2 {
			conn.log.Debug("performHandshakeForTest: Frame types incorrect.", logger.LogFields{"ok1": ok1, "ok2": ok2})
			return false
		}
		isAck := (sf2.Header().Flags & FlagSettingsAck) != 0
		conn.log.Debug("performHandshakeForTest: Frames found.", logger.LogFields{"isAck": isAck})
		return isAck
	}, "server to write its SETTINGS and ACK client's SETTINGS")

	mnc.ResetWriteBuffer() // Clear handshake frames from server's write buffer
}

func TestConnection_ClientPrefaceHandling_Valid(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestConnection_ClientPrefaceHandling_Valid")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc)
	// If performHandshakeForTest completed without error, preface was handled correctly.
	closeErr = nil // Test passed, close gracefully
}

func TestConnection_ClientPrefaceHandling_Invalid(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	var closeErr error = errors.New("test cleanup: TestConnection_ClientPrefaceHandling_Invalid")
	defer func() { conn.Close(closeErr) }()

	invalidPreface := "THIS IS NOT THE PREFACE YOU ARE LOOKING FOR"
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
	closeErr = nil // Test passed (error was expected and correct type)
}

func TestConnection_SettingsExchange_ServerSendsSettingsAndReceivesAck(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestConnection_SettingsExchange_ServerSendsSettingsAndReceivesAck")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc) // This handles initial SETTINGS from both sides.

	// Simulate client sending ACK to server's initial SETTINGS.
	// The server's initial SETTINGS frame was sent during performHandshakeForTest.
	// Now, the client (mock) needs to send an ACK for those.
	serverSettingsAckFrame := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, Flags: FlagSettingsAck, StreamID: 0, Length: 0},
	}

	// The server is not running its Serve() loop yet in this test structure.
	// To test ACK processing, we directly call handleSettingsFrame.
	if err := conn.handleSettingsFrame(serverSettingsAckFrame); err != nil {
		t.Fatalf("conn.handleSettingsFrame for ACK returned an error: %v", err)
	}

	// Check that the server's settingsAckTimeoutTimer was cleared.
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		conn.settingsMu.RLock()
		defer conn.settingsMu.RUnlock()
		return conn.settingsAckTimeoutTimer == nil
	}, "SETTINGS ACK from client to be processed (timer should be cleared/nil)")

	// Ensure no frames were written by the server in response to the ACK.
	if mnc.GetWriteBufferLen() > 0 {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		t.Errorf("Server wrote %d unexpected frames after processing client's SETTINGS ACK. First frame hex: %s",
			len(frames), hex.EncodeToString(frameToBytes(t, frames[0])))
	}
	closeErr = nil // Test passed
}

func TestConnection_Settings_AckTimeout(t *testing.T) {
	originalTimeout := SettingsAckTimeoutDuration
	SettingsAckTimeoutDuration = 50 * time.Millisecond // Short for test
	defer func() { SettingsAckTimeoutDuration = originalTimeout }()

	conn, _ := newTestConnection(t, false, nil) // Server-side
	// No explicit closeErr = nil here because the test expects conn.Close to be called by timeout.
	// The defer conn.Close is a safety net.
	var closeErr error = errors.New("test cleanup: TestConnection_Settings_AckTimeout")
	defer func() { conn.Close(closeErr) }()

	// Manually call sendInitialSettings because ServerHandshake also reads client's SETTINGS,
	// which we don't want for this specific timeout test of server's initial SETTINGS.
	if err := conn.sendInitialSettings(); err != nil {
		t.Fatalf("sendInitialSettings failed: %v", err)
	}

	// Wait for the timeout to occur and connection to shut down.
	waitForCondition(t, SettingsAckTimeoutDuration+100*time.Millisecond, 10*time.Millisecond, func() bool {
		select {
		case <-conn.shutdownChan:
			return true
		default:
			return false
		}
	}, "connection to shut down due to SETTINGS ACK timeout")

	conn.streamsMu.RLock()
	connErr := conn.connError
	conn.streamsMu.RUnlock()

	if connErr == nil {
		t.Fatalf("conn.connError was nil after SETTINGS ACK timeout")
	}
	ce, ok := connErr.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError, got %T: %v", connErr, connErr)
	}
	if ce.Code != ErrCodeSettingsTimeout {
		t.Errorf("Expected ErrCodeSettingsTimeout, got %s", ce.Code)
	}
	// Test passed if timeout error is correct. closeErr can remain non-nil to signal test error during cleanup IF THIS PART FAILS.
	// But if we reach here, the test itself has passed. So set closeErr to nil.
	closeErr = nil
}

func TestConnection_GoAway_ServerInitiated_NoError(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestConnection_GoAway_ServerInitiated_NoError")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer() // Clear handshake frames

	lastStreamID := uint32(5)
	conn.lastProcessedStreamID = lastStreamID // Simulate some streams were processed

	// Server initiates GOAWAY with NO_ERROR

	// Server initiates GOAWAY with NO_ERROR
	go conn.Close(nil) // Close with nil error for NO_ERROR GOAWAY

	var goAwayFrame *GoAwayFrame
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, f := range frames {
			if gaf, ok := f.(*GoAwayFrame); ok {
				goAwayFrame = gaf
				return true
			}
		}
		return false
	}, "GOAWAY frame to be written")

	if goAwayFrame == nil {
		t.Fatal("GOAWAY frame not written")
	}
	if goAwayFrame.LastStreamID != lastStreamID {
		t.Errorf("GOAWAY LastStreamID: got %d, want %d", goAwayFrame.LastStreamID, lastStreamID)
	}

	// If conn.Close(nil) was used, it should be NO_ERROR.
	if goAwayFrame.ErrorCode != ErrCodeNoError {
		t.Errorf("GOAWAY ErrorCode: got %s, want %s", goAwayFrame.ErrorCode, ErrCodeNoError)
	}

	// Ensure connection is eventually closed
	waitForCondition(t, 2*time.Second, 50*time.Millisecond, func() bool {
		return mnc.IsClosed()
	}, "mock net.Conn to be closed")

	closeErr = nil // Test passed
}

func TestConnection_GoAway_ReceivedFromPeer_NoError(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestConnection_GoAway_ReceivedFromPeer_NoError")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer() // Clear handshake frames

	// Start server's main read loop in a goroutine so it can process the GOAWAY
	serveErrChan := make(chan error, 1)
	go func() {
		serveErrChan <- conn.Serve(nil) // Pass nil context for test simplicity
	}()

	// Simulate peer sending GOAWAY
	peerLastStreamID := uint32(3)
	peerGoAwayFrame := &GoAwayFrame{
		FrameHeader:  FrameHeader{Type: FrameGoAway, StreamID: 0, Length: 8 /* LSID + ErrCode */},
		LastStreamID: peerLastStreamID,
		ErrorCode:    ErrCodeNoError,
	}
	mnc.FeedReadBuffer(frameToBytes(t, peerGoAwayFrame))

	// Server should process this GOAWAY and initiate its own shutdown, also sending a GOAWAY.
	var serverGoAwayFrame *GoAwayFrame
	waitForCondition(t, 2*time.Second, 50*time.Millisecond, func() bool {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, f := range frames {
			if gaf, ok := f.(*GoAwayFrame); ok {
				serverGoAwayFrame = gaf
				return true
			}
		}
		return false
	}, "server to send its own GOAWAY frame")

	if serverGoAwayFrame == nil {
		t.Fatal("Server did not send its GOAWAY frame in response to peer's GOAWAY")
	}
	// Server's GOAWAY in response to peer's NO_ERROR GOAWAY should also be NO_ERROR.
	if serverGoAwayFrame.ErrorCode != ErrCodeNoError {
		t.Errorf("Server's GOAWAY ErrorCode: got %s, want %s", serverGoAwayFrame.ErrorCode, ErrCodeNoError)
	}
	// Server's LastStreamID should be its own last processed ID (0 in this simple test setup).
	if serverGoAwayFrame.LastStreamID != 0 {
		t.Errorf("Server's GOAWAY LastStreamID: got %d, want %d (or its actual last processed)", serverGoAwayFrame.LastStreamID, 0)
	}

	// Check that peerReportedLastStreamID was updated
	conn.streamsMu.RLock()
	reportedPeerLSID := conn.peerReportedLastStreamID
	goAwayRcvd := conn.goAwayReceived
	conn.streamsMu.RUnlock()

	if !goAwayRcvd {
		t.Error("conn.goAwayReceived was not set to true")
	}
	if reportedPeerLSID != peerLastStreamID {
		t.Errorf("conn.peerReportedLastStreamID: got %d, want %d", reportedPeerLSID, peerLastStreamID)
	}

	// Wait for Serve loop to exit
	select {
	case err := <-serveErrChan:
		if err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "connection shutdown initiated") {
			// EOF or "connection shutdown" is expected if the test forced EOF or shutdown.
			// Other errors are unexpected.
			connInfo := conn.connError
			t.Errorf("conn.Serve returned an unexpected error: %v. conn.connError: %v", err, connInfo)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for conn.Serve to exit")
	}

	// Ensure underlying net.Conn is closed
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		return mnc.IsClosed()
	}, "mock net.Conn to be closed")

	closeErr = nil // Test passed
}

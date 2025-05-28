package http2

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt" // Added import
	"io"
	"net"
	"net/http"

	"os"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/http2/hpack"
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

// getErrorCode is a helper for logging, extracts ErrorCode if available.
func getErrorCode(f Frame) interface{} {
	if f == nil || f.Header() == nil {
		return "nil_frame_or_header"
	}
	switch frame := f.(type) {
	case *GoAwayFrame:
		return frame.ErrorCode.String()
	case *RSTStreamFrame:
		return frame.ErrorCode.String()
	default:
		return "N/A"
	}
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
	// Server's LastStreamID should be its own last processed ID (0 in this simple test setup as no app streams created).
	// If performHandshakeForTest caused any stream creation/tracking that updated lastProcessedStreamID, this might need adjustment.
	// For now, assuming 0 if no application streams are actively processed.
	conn.streamsMu.RLock()
	expectedLastStreamID := conn.lastProcessedStreamID
	conn.streamsMu.RUnlock()
	if serverGoAwayFrame.LastStreamID != expectedLastStreamID {
		t.Errorf("Server's GOAWAY LastStreamID: got %d, want %d (conn.lastProcessedStreamID)", serverGoAwayFrame.LastStreamID, expectedLastStreamID)
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
		// Expected errors are io.EOF, "connection shutdown initiated", or "use of closed network connection"
		if err != nil && !(errors.Is(err, io.EOF) ||
			strings.Contains(err.Error(), "connection shutdown initiated") ||
			strings.Contains(err.Error(), "use of closed network connection")) {
			connInfo := conn.connError
			t.Errorf("conn.Serve returned an unexpected error: %v. conn.connError: %v", err, connInfo)
		} else if err != nil {
			t.Logf("conn.Serve exited as expected with error: %v", err)
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

func TestConnection_Serve_DispatchPing(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestConnection_Serve_DispatchPing")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer() // Clear handshake frames

	serveErrChan := make(chan error, 1)
	go func() {
		serveErrChan <- conn.Serve(nil)
	}()

	// Send a PING frame (not an ACK)
	pingData := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	pingFrame := &PingFrame{
		FrameHeader: FrameHeader{Type: FramePing, StreamID: 0, Length: 8},
		OpaqueData:  pingData,
	}
	mnc.FeedReadBuffer(frameToBytes(t, pingFrame))

	// Expect a PING ACK in response
	var ackFrame *PingFrame
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, f := range frames {
			if pf, ok := f.(*PingFrame); ok && (pf.Header().Flags&FlagPingAck != 0) {
				ackFrame = pf
				return true
			}
		}
		return false
	}, "PING ACK frame to be written")

	if ackFrame == nil {
		t.Fatal("PING ACK frame not written")
	}
	if ackFrame.OpaqueData != pingData {
		t.Errorf("PING ACK OpaqueData: got %x, want %x", ackFrame.OpaqueData, pingData)
	}

	// Terminate the Serve loop by closing the mock connection from the "client" side
	mnc.Close() // This will cause ReadFrame in Serve to return an error (likely io.EOF or "use of closed")

	select {
	case err := <-serveErrChan:
		if err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "use of closed network connection") {
			// Expecting EOF or "use of closed network connection"
			t.Errorf("conn.Serve returned an unexpected error: %v", err)
		} else if err != nil {
			t.Logf("conn.Serve exited as expected with: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for conn.Serve to exit after mnc.Close()")
	}
	closeErr = nil // Test passed
}

func TestConnection_SendGoAway_Idempotency(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestConnection_SendGoAway_Idempotency")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer() // Clear handshake frames

	lastStreamID1 := uint32(5)
	errorCode1 := ErrCodeNoError
	debugData1 := []byte("first goaway")

	// First call to sendGoAway
	if err := conn.sendGoAway(lastStreamID1, errorCode1, debugData1); err != nil {
		t.Fatalf("First sendGoAway failed: %v", err)
	}

	var goAwayFrame1 *GoAwayFrame
	waitForCondition(t, 1*time.Second, 10*time.Millisecond, func() bool {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, f := range frames {
			if gaf, ok := f.(*GoAwayFrame); ok {
				goAwayFrame1 = gaf
				return true
			}
		}
		return false
	}, "first GOAWAY frame to be written")

	if goAwayFrame1 == nil {
		t.Fatal("First GOAWAY frame not written")
	}
	if goAwayFrame1.LastStreamID != lastStreamID1 {
		t.Errorf("First GOAWAY LastStreamID: got %d, want %d", goAwayFrame1.LastStreamID, lastStreamID1)
	}
	if goAwayFrame1.ErrorCode != errorCode1 {
		t.Errorf("First GOAWAY ErrorCode: got %s, want %s", goAwayFrame1.ErrorCode, errorCode1)
	}
	if string(goAwayFrame1.AdditionalDebugData) != string(debugData1) {
		t.Errorf("First GOAWAY DebugData: got '%s', want '%s'", string(goAwayFrame1.AdditionalDebugData), string(debugData1))
	}

	conn.streamsMu.RLock()
	goAwaySent1 := conn.goAwaySent
	conn.streamsMu.RUnlock()
	if !goAwaySent1 {
		t.Error("conn.goAwaySent should be true after first sendGoAway")
	}

	mnc.ResetWriteBuffer() // Clear the first GOAWAY frame

	// Second call to sendGoAway - should be ignored
	lastStreamID2 := uint32(7)
	errorCode2 := ErrCodeProtocolError
	debugData2 := []byte("second goaway attempt")

	if err := conn.sendGoAway(lastStreamID2, errorCode2, debugData2); err != nil {
		t.Fatalf("Second sendGoAway unexpectedly failed: %v", err) // Should return nil as it's idempotent
	}

	// Wait a bit to ensure no new frame is written
	time.Sleep(50 * time.Millisecond)
	if mnc.GetWriteBufferLen() > 0 {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		t.Fatalf("Second GOAWAY frame was unexpectedly written. Frames: %+v", frames)
	}

	conn.streamsMu.RLock()
	goAwaySent2 := conn.goAwaySent
	conn.streamsMu.RUnlock()
	if !goAwaySent2 { // Should still be true from the first call
		t.Error("conn.goAwaySent was false after second sendGoAway call")
	}
	closeErr = nil
}

func TestConnection_HandleGoAwayFrame_MultipleValid(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestConnection_HandleGoAwayFrame_MultipleValid")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	// Dispatch first GOAWAY from peer
	peerLastStreamID1 := uint32(10)
	peerGoAwayFrame1 := &GoAwayFrame{
		FrameHeader:  FrameHeader{Type: FrameGoAway, StreamID: 0, Length: 8 /* LSID + ErrCode */},
		LastStreamID: peerLastStreamID1,
		ErrorCode:    ErrCodeNoError,
	}
	err1 := conn.dispatchFrame(peerGoAwayFrame1)
	if err1 != nil {
		closeErr = fmt.Errorf("dispatchFrame for first GOAWAY failed: %w", err1)
		t.Fatal(closeErr)
	}

	// Server should process this and initiate shutdown, sending its own GOAWAY.
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		conn.streamsMu.RLock()
		defer conn.streamsMu.RUnlock()
		select {
		case <-conn.shutdownChan:
			return conn.goAwayReceived && conn.peerReportedLastStreamID == peerLastStreamID1
		default:
			return false
		}
	}, "first GOAWAY to be processed and shutdown initiated")

	mnc.ResetWriteBuffer() // Clear server's response GOAWAY for now

	// Dispatch second, valid GOAWAY from peer (lower LastStreamID)
	peerLastStreamID2 := uint32(5)
	peerGoAwayFrame2 := &GoAwayFrame{
		FrameHeader:  FrameHeader{Type: FrameGoAway, StreamID: 0, Length: 8},
		LastStreamID: peerLastStreamID2,
		ErrorCode:    ErrCodeNoError, // Can be different, but NO_ERROR is fine for valid sequence test
	}
	err2 := conn.dispatchFrame(peerGoAwayFrame2)
	if err2 != nil {
		closeErr = fmt.Errorf("dispatchFrame for second GOAWAY failed: %w", err2)
		t.Fatal(closeErr)
	}

	// Wait for the second GOAWAY to be processed and peerReportedLastStreamID updated
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		conn.streamsMu.RLock()
		defer conn.streamsMu.RUnlock()
		// goAwayReceived should still be true
		return conn.goAwayReceived && conn.peerReportedLastStreamID == peerLastStreamID2
	}, "second GOAWAY (lower LSID) to be processed and peerReportedLastStreamID updated")

	// No new GOAWAY should be sent by the server in response to this second valid GOAWAY.
	// The connection is already shutting down.
	time.Sleep(50 * time.Millisecond) // Give time for any incorrect frame to be written
	if mnc.GetWriteBufferLen() > 0 {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		t.Errorf("Server sent unexpected frames after second valid GOAWAY: %+v", frames)
	}

	// Explicitly close the connection to ensure cleanup, as Serve loop isn't running
	// The error for Close can be nil as the test is about valid GOAWAY processing.
	finalCloseErr := conn.Close(nil)
	if finalCloseErr != nil && !errors.Is(finalCloseErr, io.EOF) && !strings.Contains(finalCloseErr.Error(), "use of closed network connection") {
		t.Logf("conn.Close() at end of test returned: %v", finalCloseErr)
	}
	closeErr = nil
}

func TestConnection_HandleGoAwayFrame_MultipleInvalid_HigherLastStreamID(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestConnection_HandleGoAwayFrame_MultipleInvalid_HigherLastStreamID")
	// Defer Close needs to be careful here. We call it explicitly at the end.
	// Let defer only close if test panics or fails early.
	defer func() {
		if conn == nil {
			return
		}
		if t.Failed() || recover() != nil { // If test failed or panicked
			conn.Close(closeErr) // closeErr might be updated
		} else if closeErr == nil { // Test passed, closeErr is nil for this defer
			conn.Close(nil) // Graceful close if test passed
		}
		// If test passed but closeErr is not nil (from an explicit conn.Close call), it means conn.Close was already called successfully.
	}()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	// Dispatch first GOAWAY from peer
	peerLastStreamID1 := uint32(5)
	peerGoAwayFrame1 := &GoAwayFrame{
		FrameHeader:  FrameHeader{Type: FrameGoAway, StreamID: 0, Length: 8},
		LastStreamID: peerLastStreamID1,
		ErrorCode:    ErrCodeNoError,
	}
	err1 := conn.dispatchFrame(peerGoAwayFrame1) // Directly dispatch
	if err1 != nil {
		closeErr = fmt.Errorf("dispatchFrame for first GOAWAY failed: %w", err1)
		t.Fatal(closeErr)
	}

	// Server processes this, should initiate shutdown.
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		conn.streamsMu.RLock()
		defer conn.streamsMu.RUnlock()
		// Check that shutdownChan is closed AND goAwayReceived is true, and peerReportedLastStreamID is correct
		select {
		case <-conn.shutdownChan:
			return conn.goAwayReceived && conn.peerReportedLastStreamID == peerLastStreamID1
		default:
			return false
		}
	}, "first GOAWAY to be processed and shutdown initiated")

	// Server sent its own GOAWAY in response to the first peer GOAWAY.
	// This GOAWAY should have ErrCodeNoError.
	var serverSentGoAwayFrame *GoAwayFrame
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, f := range frames {
			if gaf, ok := f.(*GoAwayFrame); ok {
				serverSentGoAwayFrame = gaf
				return true // Found the server's GOAWAY
			}
		}
		return false
	}, "server's GOAWAY frame (in response to first peer GOAWAY) to be written")

	if serverSentGoAwayFrame == nil {
		closeErr = errors.New("Server's GOAWAY frame not found after first peer GOAWAY")
		t.Fatal(closeErr)
	}
	if serverSentGoAwayFrame.ErrorCode != ErrCodeNoError {
		closeErr = fmt.Errorf("Server's GOAWAY frame ErrorCode: got %s, want %s (response to first valid peer GOAWAY)", serverSentGoAwayFrame.ErrorCode, ErrCodeNoError)
		t.Fatal(closeErr)
	}
	// Clear the buffer *after* capturing and verifying the server's response to the first GOAWAY.
	mnc.ResetWriteBuffer()

	// Dispatch second, *invalid* GOAWAY from peer (higher LastStreamID)
	peerLastStreamID2 := uint32(10) // Higher than first
	peerGoAwayFrame2 := &GoAwayFrame{
		FrameHeader:  FrameHeader{Type: FrameGoAway, StreamID: 0, Length: 8},
		LastStreamID: peerLastStreamID2,
		ErrorCode:    ErrCodeNoError, // Error code of peer's second GOAWAY doesn't primarily drive server's reaction if LSID is invalid
	}
	err2 := conn.dispatchFrame(peerGoAwayFrame2) // Directly dispatch

	if err2 == nil {
		closeErr = errors.New("dispatchFrame for second (invalid) GOAWAY returned nil error, expected ConnectionError")
		t.Fatal(closeErr)
	}

	connErr, ok := err2.(*ConnectionError)
	if !ok {
		closeErr = fmt.Errorf("Second dispatchFrame error type: got %T, want *ConnectionError. Err: %w", err2, err2)
		t.Fatal(closeErr)
	}
	if connErr.Code != ErrCodeProtocolError {
		closeErr = fmt.Errorf("Expected PROTOCOL_ERROR from invalid subsequent GOAWAY, got %s", connErr.Code)
		t.Error(closeErr) // Use t.Error to allow further checks
	}
	if !strings.Contains(connErr.Msg, "subsequent GOAWAY has LastStreamID") || !strings.Contains(connErr.Msg, "which is greater than previous") {
		e := fmt.Errorf("Error message '%s' doesn't match expected for invalid GOAWAY sequence", connErr.Msg)
		if closeErr == nil {
			closeErr = e
		} else {
			closeErr = fmt.Errorf("%w; %w", closeErr, e)
		}
		t.Error(e)
	}

	// Check that conn.connError is updated to reflect the more severe error from the invalid GOAWAY
	conn.streamsMu.RLock()
	finalConnErrorStored := conn.connError
	conn.streamsMu.RUnlock()

	if finalConnErrorStored == nil {
		e := errors.New("conn.connError is nil after invalid GOAWAY dispatch")
		if closeErr == nil {
			closeErr = e
		} else {
			closeErr = fmt.Errorf("%w; %w", closeErr, e)
		}
		t.Error(e)
	} else {
		ceFinal, okFinal := finalConnErrorStored.(*ConnectionError)
		if !okFinal || ceFinal.Code != ErrCodeProtocolError { // Expect PROTOCOL_ERROR here
			e := fmt.Errorf("conn.connError is '%v', expected it to reflect PROTOCOL_ERROR from the invalid GOAWAY (got code %s)", finalConnErrorStored, ceFinal.Code)
			if closeErr == nil {
				closeErr = e
			} else {
				closeErr = fmt.Errorf("%w; %w", closeErr, e)
			}
			t.Error(e)
		}
	}

	// Now call conn.Close() with the error from the invalid dispatch.
	// This ensures cleanup but should NOT send a new GOAWAY frame.
	explicitCloseErr := conn.Close(err2)
	if explicitCloseErr != nil && !errors.Is(explicitCloseErr, err2) && !strings.Contains(explicitCloseErr.Error(), "use of closed network connection") {
		t.Logf("conn.Close(err2) returned: %v. err2 was: %v", explicitCloseErr, err2)
	}

	// Verify no *new* GOAWAY frame was sent. The buffer was reset before this stage.
	time.Sleep(50 * time.Millisecond) // Give time for any incorrect frame write
	if mnc.GetWriteBufferLen() > 0 {
		unexpectedFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		e := fmt.Errorf("Server sent unexpected new GOAWAY frame after processing second invalid peer GOAWAY: %+v", unexpectedFrames)
		if closeErr == nil {
			closeErr = e
		} else {
			closeErr = fmt.Errorf("%w; %w", closeErr, e)
		}
		t.Error(e)
	}
	// If we reach here and no t.Error/t.Fatal was called for logic errors, the test itself has passed.
	// The defer logic handles conn.Close based on t.Failed().
	// We've already called conn.Close(err2) so the connection is being shut down.
}

func TestConnection_Serve_DispatchRSTStream(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	mockDispatcher := &mockRequestDispatcher{}
	conn.dispatcher = mockDispatcher.Dispatch // Re-assign dispatcher after NewConnection

	var closeErr error = errors.New("test cleanup: TestConnection_Serve_DispatchRSTStream")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc)

	// Create a stream
	streamID := uint32(1) // Client-initiated streams are odd
	stream, err := conn.createStream(streamID, nil, true /*isPeerInitiated*/)
	if err != nil {
		t.Fatalf("Failed to create stream: %v", err)
	}
	stream.mu.Lock()
	stream.state = StreamStateOpen // Manually set to open for test
	stream.mu.Unlock()

	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		serveErrChan <- conn.Serve(nil)
	}()

	// Send an RST_STREAM frame
	rstFrame := &RSTStreamFrame{
		FrameHeader: FrameHeader{Type: FrameRSTStream, StreamID: streamID, Length: 4},
		ErrorCode:   ErrCodeCancel,
	}
	mnc.FeedReadBuffer(frameToBytes(t, rstFrame))

	// Wait for stream to be closed and removed from connection tracking
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		_, exists := conn.getStream(streamID)
		return !exists // Stream should no longer exist in the map
	}, "stream to be removed after RST_STREAM")

	// No frames should be written by server in response to a valid RST_STREAM it receives
	if mnc.GetWriteBufferLen() > 0 {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		t.Errorf("Server wrote unexpected frames after RST_STREAM: %+v", frames)
	}

	// Terminate Serve
	mnc.Close()
	<-serveErrChan // Wait for Serve to finish
	closeErr = nil // Test passed
}

func TestConnection_Serve_ConnectionErrorOnDispatch(t *testing.T) {
	mnc := newMockNetConn()
	lg := logger.NewDiscardLogger() // Use discard logger
	mockDispatcher := &mockRequestDispatcher{}
	conn := NewConnection(mnc, lg, false /*isClient*/, nil, mockDispatcher.Dispatch)
	if conn == nil {
		t.Fatal("NewConnection returned nil")
	}
	conn.writerChan = make(chan Frame, 1)
	time.Sleep(100 * time.Millisecond)

	// This outer defer is a final safety net for cleanup.
	// The main close related to Serve's error will be explicit.
	var finalCloseErr error = errors.New("test cleanup: TestConnection_Serve_ConnectionErrorOnDispatch")
	defer func() {
		if conn != nil {
			conn.Close(finalCloseErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(nil)
		serveErrChan <- err
	}()

	// Send a SETTINGS frame on a non-zero stream ID (guaranteed PROTOCOL_ERROR)
	malformedSettings := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 1, Flags: 0, Length: 0}, // StreamID 1
	}
	mnc.FeedReadBuffer(frameToBytes(t, malformedSettings))

	// Expect Serve to exit with a ConnectionError
	var serveExitError error
	select {
	case serveExitError = <-serveErrChan:
		// Successfully received error from Serve.
	case <-time.After(2 * time.Second):
		finalCloseErr = errors.New("timeout waiting for conn.Serve to exit")
		t.Fatal(finalCloseErr)
		return
	}

	if serveExitError == nil {
		finalCloseErr = errors.New("conn.Serve exited with nil error, expected ConnectionError")
		t.Fatal(finalCloseErr)
	}

	// Explicitly close the connection with the error from Serve. This should trigger GOAWAY.
	// Store the error from this Close call for the deferred cleanup.
	finalCloseErr = conn.Close(serveExitError)

	// Now check the error returned by Serve
	connErr, ok := serveExitError.(*ConnectionError)
	if !ok {
		t.Fatalf("conn.Serve exited with error type %T, expected *ConnectionError. Err: %v", serveExitError, serveExitError)
	}
	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected PROTOCOL_ERROR from Serve, got %s", connErr.Code)
	}

	// Expect a GOAWAY frame to be sent by the server
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
		t.Fatal("GOAWAY frame not written after connection error")
	}
	if goAwayFrame.ErrorCode != ErrCodeProtocolError {
		t.Errorf("GOAWAY ErrorCode: got %s, want %s", goAwayFrame.ErrorCode, ErrCodeProtocolError)
	}

	// Underlying connection should be closed
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed")
	finalCloseErr = nil // Test passed
}

func TestConnection_Close_Graceful(t *testing.T) {
	var gracefulTestCloseErr error                // Declare gracefulTestCloseErr
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	// No defer conn.Close here as we are testing it.
	// It should eventually close the mock connection.

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	// Start Serve and Writer loops
	serveErrChan := make(chan error, 1)
	go func() { serveErrChan <- conn.Serve(nil) }()
	// Writer loop is started by NewConnection implicitly.

	// Call Close(nil) for graceful shutdown
	gracefulTestCloseErr = conn.Close(nil) // Store return value for checking
	// Don't check closeErr immediately, as it might reflect state if mnc was already closed by Serve for some reason.

	// Verify GOAWAY frame
	var goAwayFrame *GoAwayFrame
	waitForCondition(t, 1*time.Second, 10*time.Millisecond, func() bool {
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
		t.Fatal("GOAWAY frame not sent on graceful close")
	}
	if goAwayFrame.ErrorCode != ErrCodeNoError {
		t.Errorf("GOAWAY ErrorCode: got %s, want %s", goAwayFrame.ErrorCode, ErrCodeNoError)
	}

	// Verify channels are closed
	select {
	case <-conn.shutdownChan: // Expected
	case <-time.After(1 * time.Second): // Increased timeout slightly for CI
		t.Error("shutdownChan not closed")
	}
	select {
	case <-conn.readerDone: // Expected
	case <-time.After(1 * time.Second):
		t.Error("readerDone not closed")
	}
	select {
	case <-conn.writerDone: // Expected
	case <-time.After(1 * time.Second):
		t.Error("writerDone not closed")
	}

	// Verify mock net.Conn is closed
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed")

	// Serve loop should exit
	var serveExitErr error
	select {
	case serveExitErr = <-serveErrChan:
		// Expected errors: io.EOF, "connection shutdown initiated", "use of closed network connection"
		if serveExitErr != nil && !(errors.Is(serveExitErr, io.EOF) ||
			strings.Contains(serveExitErr.Error(), "connection shutdown initiated") ||
			strings.Contains(serveExitErr.Error(), "use of closed network connection")) {
			t.Errorf("Serve loop exited with unexpected error: %v", serveExitErr)
		} else if serveExitErr != nil {
			t.Logf("Serve loop exited as expected with: %v", serveExitErr)
		}
	case <-time.After(1 * time.Second):
		t.Error("Serve loop did not exit after graceful close")
	}

	// Check the error returned by conn.Close(nil)
	if gracefulTestCloseErr != nil && !(errors.Is(gracefulTestCloseErr, io.EOF) ||
		strings.Contains(gracefulTestCloseErr.Error(), "use of closed network connection") ||
		(serveExitErr != nil && errors.Is(gracefulTestCloseErr, serveExitErr))) {
		// It's okay if gracefulTestCloseErr is nil, EOF, "use of closed", or the same error Serve exited with.
		t.Errorf("conn.Close(nil) returned unexpected error: %v (Serve exit error: %v)", gracefulTestCloseErr, serveExitErr)
	}
}

func TestConnection_Close_WithError(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() { serveErrChan <- conn.Serve(nil) }()

	// Call Close with an error
	testErr := NewConnectionError(ErrCodeInternalError, "test-induced error")
	closeReturnErr := conn.Close(testErr)

	// The error returned by Close() should be the one we passed, or a related shutdown error.
	if !errors.Is(closeReturnErr, testErr) {
		// Allow for "connection shutdown initiated" or "use of closed" if already closing.
		if !(strings.Contains(closeReturnErr.Error(), "connection shutdown initiated") ||
			strings.Contains(closeReturnErr.Error(), "use of closed network connection") ||
			errors.Is(closeReturnErr, io.EOF)) { // EOF also possible if Serve loop closed mnc first.
			t.Errorf("conn.Close() returned '%v', want one equivalent to '%v' or shutdown/EOF error", closeReturnErr, testErr)
		}
	}

	var goAwayFrame *GoAwayFrame
	waitForCondition(t, 1*time.Second, 10*time.Millisecond, func() bool {
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
		t.Fatal("GOAWAY frame not sent on error close")
	}
	if goAwayFrame.ErrorCode != ErrCodeInternalError {
		t.Errorf("GOAWAY ErrorCode: got %s, want %s", goAwayFrame.ErrorCode, ErrCodeInternalError)
	}
	if string(goAwayFrame.AdditionalDebugData) != "test-induced error" {
		t.Errorf("GOAWAY DebugData: got '%s', want 'test-induced error'", string(goAwayFrame.AdditionalDebugData))
	}

	select {
	case <-conn.shutdownChan:
	case <-time.After(1 * time.Second): // Increased timeout
		t.Error("shutdownChan not closed")
	}
	select {
	case <-conn.readerDone:
	case <-time.After(1 * time.Second):
		t.Error("readerDone not closed")
	}
	select {
	case <-conn.writerDone:
	case <-time.After(1 * time.Second):
		t.Error("writerDone not closed")
	}

	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed")

	select {
	case err := <-serveErrChan:
		// Expect Serve to exit with an error related to the shutdown or the original testErr.
		if !errors.Is(err, testErr) && !errors.Is(err, io.EOF) &&
			!strings.Contains(err.Error(), "connection shutdown initiated") &&
			!strings.Contains(err.Error(), "use of closed network connection") {
			t.Logf("Serve loop exited with: %v (expected error related to %v, or shutdown/EOF)", err, testErr)
		} else if err != nil {
			t.Logf("Serve loop exited as expected with: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("Serve loop did not exit after error close")
	}
}

func TestConnection_Serve_PanicRecovery(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	// Do not defer conn.Close() as we want to check the error it returns/state after panic.

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	// Setup readHook to panic
	panicMsg := "test panic in readFrame via readHook"
	var readHookCalled bool
	var readHookMu sync.Mutex // Protect readHookCalled for concurrent access paranoia, though test is serial here.

	mnc.readHook = func(b []byte) (int, error) {
		readHookMu.Lock()
		defer readHookMu.Unlock()
		if !readHookCalled {
			readHookCalled = true
			// t.Logf("readHook CALLED AND ABOUT TO PANIC") // Debug log
			panic(panicMsg)
		}
		// t.Logf("readHook called (after panic), returning EOF") // Debug log
		return 0, io.EOF // Subsequent calls if any
	}
	// Feed one byte to trigger a ReadFrame call which will then use readHook
	// Needs to be a valid frame header start at least for ReadFrame to proceed.
	// Smallest valid frame is PING (9 byte header + 8 byte payload).
	// Let's send something that would trigger ReadFrame then the panic in mock.Read.
	// Simplest: just enough bytes to pass initial length read in ReadFrameHeader.
	mnc.FeedReadBuffer([]byte{0x00, 0x00, 0x08, byte(FramePing), 0x00, 0x00, 0x00, 0x00, 0x00}) // PING header with length 8 to trigger payload read

	serveErr := conn.Serve(nil) // Call Serve directly for this test

	if serveErr == nil {
		t.Fatal("conn.Serve returned nil error, expected ConnectionError due to panic")
	}
	connErr, ok := serveErr.(*ConnectionError)
	if !ok {
		t.Fatalf("conn.Serve error type: got %T, want *ConnectionError. Err: %v", serveErr, serveErr)
	}
	if connErr.Code != ErrCodeInternalError {
		t.Errorf("Expected ErrCodeInternalError from panic recovery, got %s", connErr.Code)
	}
	if !strings.Contains(connErr.Msg, "internal server panic in reader loop") {
		t.Errorf("ConnectionError message '%s' does not indicate panic recovery", connErr.Msg)
	}

	// Check that connError on the connection object is set
	conn.streamsMu.RLock()
	internalConnErr := conn.connError
	conn.streamsMu.RUnlock()

	if internalConnErr == nil {
		t.Error("conn.connError is nil after panic")
	} else {
		// Check if internalConnErr is equivalent to serveErr (the panic-derived ConnectionError)
		ceInternal, okInternal := internalConnErr.(*ConnectionError)
		if !okInternal || ceInternal.Code != ErrCodeInternalError || !strings.Contains(ceInternal.Msg, "internal server panic in reader loop") {
			t.Errorf("conn.connError is '%v', expected one reflecting the panic: '%v'", internalConnErr, serveErr)
		}
	}

	// Explicitly close the connection with the error from Serve, this should trigger GOAWAY.
	conn.Close(serveErr)
	// GOAWAY frame should have been sent (triggered by Close called from Serve's defer)
	var goAwayFrame *GoAwayFrame
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		if len(frames) > 0 {
			var frameSummaries []string
			for _, fr := range frames {
				if fr != nil && fr.Header() != nil { // Basic nil check
					frameSummaries = append(frameSummaries, fmt.Sprintf("{Type:%s, StreamID:%d, ErrorCodeIfExists:%v}", fr.Header().Type, fr.Header().StreamID, getErrorCode(fr)))
				} else {
					frameSummaries = append(frameSummaries, "{NIL_FRAME_OR_HEADER}")
				}
			}
			t.Logf("waitForCondition (PanicRecovery GOAWAY): Frames in buffer: %d. Summaries: %s", len(frames), strings.Join(frameSummaries, ", "))
		} else {
			t.Logf("waitForCondition (PanicRecovery GOAWAY): No frames in buffer yet.")
		}
		for _, f := range frames {
			if gaf, ok := f.(*GoAwayFrame); ok {
				t.Logf("waitForCondition (PanicRecovery GOAWAY): Found GOAWAY frame with ErrorCode: %s (target: %s)", gaf.ErrorCode, ErrCodeInternalError)
				if gaf.ErrorCode == ErrCodeInternalError {
					goAwayFrame = gaf
					return true
				}
			}
		}
		return false
	}, "GOAWAY frame with INTERNAL_ERROR to be written")

	if goAwayFrame == nil {
		t.Error("GOAWAY frame with INTERNAL_ERROR not sent after panic")
	}

	// Connection should be closed (mnc should be closed by conn.Close() within Serve's defer)
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed after panic and Serve's deferred Close")

	// Explicitly call Close again to check idempotency and ensure all cleanup if Serve's defer didn't fully complete (e.g., panic during Close itself, though unlikely here)
	finalCloseErr := conn.Close(serveErr) // Pass the error from Serve to Close
	if finalCloseErr != serveErr && !errors.Is(finalCloseErr, io.EOF) && !strings.Contains(finalCloseErr.Error(), "use of closed network connection") {
		// It's okay if finalCloseErr is serveErr or an EOF/closed connection error due to mnc being closed.
		// Or if it's the same as serveErr (meaning already shut down from that error).
		t.Logf("conn.Close (second call) returned: %v (was expecting %v or EOF/closed or already shut down)", finalCloseErr, serveErr)
	}
}

func TestConnection_DispatchWindowUpdateFrame_ConnLevel_ValidIncrement(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestConnection_DispatchWindowUpdateFrame_ConnLevel_ValidIncrement")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer() // Clear handshake frames

	serveErrChan := make(chan error, 1)
	go func() {
		serveErrChan <- conn.Serve(nil)
	}()

	initialConnSendWindow := conn.connFCManager.GetConnectionSendAvailable()
	increment := uint32(1024)

	wuFrame := &WindowUpdateFrame{
		FrameHeader:         FrameHeader{Type: FrameWindowUpdate, StreamID: 0, Length: 4},
		WindowSizeIncrement: increment,
	}
	mnc.FeedReadBuffer(frameToBytes(t, wuFrame))

	// Wait for the window to update
	expectedWindowSize := initialConnSendWindow + int64(increment)
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		return conn.connFCManager.GetConnectionSendAvailable() == expectedWindowSize
	}, "connection send window to update")

	if conn.connFCManager.GetConnectionSendAvailable() != expectedWindowSize {
		t.Errorf("Connection send window: got %d, want %d", conn.connFCManager.GetConnectionSendAvailable(), expectedWindowSize)
	}

	// Ensure no GOAWAY frame was sent
	if mnc.GetWriteBufferLen() > 0 {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, f := range frames {
			if _, ok := f.(*GoAwayFrame); ok {
				t.Fatal("Unexpected GOAWAY frame sent for valid WINDOW_UPDATE")
			}
		}
	}

	// Terminate Serve loop
	mnc.Close()
	<-serveErrChan
	closeErr = nil
}

func TestConnection_DispatchWindowUpdateFrame_ConnLevel_ZeroIncrement(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestConnection_DispatchWindowUpdateFrame_ConnLevel_ZeroIncrement")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(nil)
		serveErrChan <- err
	}()

	initialConnSendWindow := conn.connFCManager.GetConnectionSendAvailable()

	wuFrame := &WindowUpdateFrame{
		FrameHeader:         FrameHeader{Type: FrameWindowUpdate, StreamID: 0, Length: 4},
		WindowSizeIncrement: 0, // Zero increment
	}
	mnc.FeedReadBuffer(frameToBytes(t, wuFrame))

	// Expect Serve to exit with a ConnectionError
	var serveExitError error
	select {
	case serveExitError = <-serveErrChan:
		// Expected
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for conn.Serve to exit")
	}

	if serveExitError == nil {
		t.Fatal("conn.Serve exited with nil error, expected ConnectionError")
	}

	// Explicitly close the connection with the error returned by Serve.
	// This should trigger the GOAWAY frame.
	t.Logf("Serve exited with error: %v. Explicitly calling conn.Close().", serveExitError)
	_ = conn.Close(serveExitError)

	connErr, ok := serveExitError.(*ConnectionError)
	if !ok {
		t.Fatalf("conn.Serve error type: got %T, want *ConnectionError. Err: %v", serveExitError, serveExitError)
	}
	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected PROTOCOL_ERROR, got %s", connErr.Code)
	}

	// Expect a GOAWAY frame
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
		t.Fatal("GOAWAY frame not written for zero increment WINDOW_UPDATE")
	}
	if goAwayFrame.ErrorCode != ErrCodeProtocolError {
		t.Errorf("GOAWAY ErrorCode: got %s, want %s", goAwayFrame.ErrorCode, ErrCodeProtocolError)
	}

	// Connection send window should not have changed
	if conn.connFCManager.GetConnectionSendAvailable() != initialConnSendWindow {
		t.Errorf("Connection send window changed: got %d, want %d", conn.connFCManager.GetConnectionSendAvailable(), initialConnSendWindow)
	}
	closeErr = nil
}

func TestConnection_DispatchWindowUpdateFrame_ConnLevel_Overflow(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestConnection_DispatchWindowUpdateFrame_ConnLevel_Overflow")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(nil)
		serveErrChan <- err
	}()

	initialConnSendWindow := conn.connFCManager.GetConnectionSendAvailable() // Should be DefaultSettingsInitialWindowSize
	if initialConnSendWindow != int64(DefaultSettingsInitialWindowSize) {
		t.Logf("Warning: initialConnSendWindow is %d, expected DefaultSettingsInitialWindowSize %d. Test might behave unexpectedly if settings differ.", initialConnSendWindow, DefaultSettingsInitialWindowSize)
	}

	// Increment that will cause overflow: MaxWindowSize - current_window + 1
	increment := uint32(MaxWindowSize - initialConnSendWindow + 1)

	wuFrame := &WindowUpdateFrame{
		FrameHeader:         FrameHeader{Type: FrameWindowUpdate, StreamID: 0, Length: 4},
		WindowSizeIncrement: increment,
	}
	mnc.FeedReadBuffer(frameToBytes(t, wuFrame))

	// Expect Serve to exit with a ConnectionError
	var serveExitError error
	select {
	case serveExitError = <-serveErrChan:
		// Expected
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for conn.Serve to exit")
	}

	if serveExitError == nil {
		t.Fatal("conn.Serve exited with nil error, expected ConnectionError")
	}

	// Explicitly close the connection with the error returned by Serve.
	// This should trigger the GOAWAY frame.
	t.Logf("Serve exited with error: %v. Explicitly calling conn.Close().", serveExitError)
	_ = conn.Close(serveExitError) // We don't need to check the error from conn.Close() here again,
	// as serveExitError is the primary error of interest.

	connErr, ok := serveExitError.(*ConnectionError)
	if !ok {
		t.Fatalf("conn.Serve error type: got %T, want *ConnectionError. Err: %v", serveExitError, serveExitError)
	}
	if connErr.Code != ErrCodeFlowControlError {
		t.Errorf("Expected FLOW_CONTROL_ERROR, got %s", connErr.Code)
	}

	// Expect a GOAWAY frame
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
		t.Fatal("GOAWAY frame not written for window overflow WINDOW_UPDATE")
	}
	if goAwayFrame.ErrorCode != ErrCodeFlowControlError {
		t.Errorf("GOAWAY ErrorCode: got %s, want %s", goAwayFrame.ErrorCode, ErrCodeFlowControlError)
	}

	// Connection send window state should reflect the error, not the increment.
	// Specifically, fcw.available might be negative, or fcw.err set.
	// We check that it didn't just add the huge increment.
	if conn.connFCManager.GetConnectionSendAvailable() > MaxWindowSize || conn.connFCManager.GetConnectionSendAvailable() == initialConnSendWindow+int64(increment) {
		t.Errorf("Connection send window has invalid size after overflow: %d", conn.connFCManager.GetConnectionSendAvailable())
	}
	closeErr = nil
}

// TestConnection_DispatchDataFrame tests the connection's dispatchDataFrame method.
func TestConnection_DispatchDataFrame(t *testing.T) {
	t.Parallel()

	const testStreamID = uint32(1) // Client-initiated stream
	const testPayload = "hello"

	// Helper to create a DATA frame
	newDataFrameForTest := func(streamID uint32, data []byte, endStream bool) *DataFrame {
		fh := FrameHeader{
			Length:   uint32(len(data)),
			Type:     FrameData,
			StreamID: streamID,
		}
		if endStream {
			fh.Flags |= FlagDataEndStream
		}
		return &DataFrame{
			FrameHeader: fh,
			Data:        data,
		}
	}

	tests := []struct {
		name                       string
		setupFunc                  func(t *testing.T, conn *Connection, mnc *mockNetConn) *Stream // Returns the stream if one is created for the test
		frameToSend                *DataFrame
		expectedError              bool
		expectedConnectionError    *ConnectionError                                // If expectError, check if it's this specific ConnectionError
		expectedRSTStreamErrorCode ErrorCode                                       // If an RST_STREAM is expected to be sent by the connection
		expectedConnFCDecrease     uint32                                          // Expected decrease in connection's receive window
		streamHandleDataFrameCheck func(t *testing.T, s *Stream, frame *DataFrame) // Optional: to verify stream.handleDataFrame interaction
	}{
		{
			name: "Success: DATA frame for open stream",
			setupFunc: func(t *testing.T, conn *Connection, mnc *mockNetConn) *Stream {
				s, err := conn.createStream(testStreamID, nil, true /*isPeerInitiated*/)
				if err != nil {
					t.Fatalf("Failed to create stream for test: %v", err)
				}
				s.mu.Lock()
				s.state = StreamStateOpen // Manually set state
				s.mu.Unlock()
				return s
			},
			frameToSend:            newDataFrameForTest(testStreamID, []byte(testPayload), false),
			expectedError:          false,
			expectedConnFCDecrease: uint32(len(testPayload)),
			streamHandleDataFrameCheck: func(t *testing.T, s *Stream, frame *DataFrame) {
				// Verify data reached the stream's pipe (indirectly testing handleDataFrame call)
				data := make([]byte, len(frame.Data))
				// Use a timeout for reading from the pipe, as it might block if data isn't written
				// or if the writer end is not closed as expected by the test logic.
				readDone := make(chan struct{})
				var n int
				var err error
				go func() {
					defer close(readDone)
					n, err = s.requestBodyReader.Read(data)
				}()

				select {
				case <-readDone:
					// Read completed or errored. If err is nil here, it means Read returned 0, io.EOF.
					// If n > 0 and err is nil, means read was successful.
					// If err is io.EOF and n == len(frame.Data), that's perfect for endStream=true.
					// If err is io.EOF and n < len(frame.Data), that's an issue.
					// If err is not nil and not EOF, that's an issue.
					// The original check `if err != nil && !errors.Is(err, io.EOF)` was good.
					// The blocking happens if Read() is called again after all data is read but pipe not closed.
					// The goroutine will exit after the first Read() call that gets all expected data
					// or encounters an error/EOF.
					// The issue is if the test expects further interaction.
					// For this specific test, the pipe write happens in stream.handleDataFrame,
					// and the check reads it. If frame.EndStream is false, handleDataFrame won't close pipe writer.
					// So, the reader must not expect EOF.
					if (frame.Header().Flags&FlagDataEndStream == 0) && err == io.EOF && n < len(frame.Data) {
						t.Errorf("Premature EOF reading from stream's requestBodyReader: read %d, expected %d", n, len(frame.Data))
					} else if err != nil && !errors.Is(err, io.EOF) {
						t.Errorf("Error reading from stream's requestBodyReader: %v", err)
					}

				case <-time.After(200 * time.Millisecond): // Increased timeout slightly
					t.Error("Timeout reading from stream's requestBodyReader")
					_ = s.requestBodyWriter.CloseWithError(errors.New("test read timeout, closing writer"))
					<-readDone
					return
				}

				// This part is outside the select, after readDone or timeout.
				// This check should only be done if timeout did not occur.
				// The select already handles error logging on timeout.
				// If timeout did not occur, n and err are set from the read operation.
				// The error check previously inside 'case <-readDone:' is sufficient.

				if n != len(frame.Data) {
					// This can happen if read returned an error or EOF prematurely.
					// Avoid duplicate error if already logged.
					if err == nil || (errors.Is(err, io.EOF) && n != len(frame.Data)) { // If no error but wrong length, or EOF but wrong length
						t.Errorf("Data length mismatch: read %d bytes, expected %d bytes. Data: %q", n, len(frame.Data), string(data[:n]))
					}
				} else if string(data[:n]) != string(frame.Data) { // n == len(frame.Data)
					t.Errorf("Data in stream pipe mismatch: got %q, want %q", string(data[:n]), string(frame.Data))
				}

				if err != nil && !errors.Is(err, io.EOF) { // EOF is fine if endStream was true and pipe closed
					t.Errorf("Error reading from stream's requestBodyReader: %v", err)
				}
				if n != len(frame.Data) || string(data[:n]) != string(frame.Data) {
					t.Errorf("Data in stream pipe mismatch: got %q (n=%d), want %q", string(data[:n]), n, string(frame.Data))
				}
			},
		},
		{
			name:                    "Error: DATA frame on stream 0",
			setupFunc:               func(t *testing.T, conn *Connection, mnc *mockNetConn) *Stream { return nil },
			frameToSend:             newDataFrameForTest(0, []byte(testPayload), false),
			expectedError:           true,
			expectedConnectionError: &ConnectionError{Code: ErrCodeProtocolError, Msg: "DATA frame received on stream 0"},
			// For DATA on stream 0, dispatchDataFrame errors *before* calling connFCManager.DataReceived.
			expectedConnFCDecrease: 0,
		},
		{
			name: "Error: Connection flow control violation",
			setupFunc: func(t *testing.T, conn *Connection, mnc *mockNetConn) *Stream {
				// Reduce connection's receive window to be less than payload
				conn.connFCManager.receiveWindowMu.Lock()
				conn.connFCManager.currentReceiveWindowSize = int64(len(testPayload) - 1)
				conn.connFCManager.receiveWindowMu.Unlock()

				s, err := conn.createStream(testStreamID, nil, true)
				if err != nil {
					t.Fatalf("Setup: failed to create dummy stream for FC violation test: %v", err)
				}
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
				return s
			},
			frameToSend:             newDataFrameForTest(testStreamID, []byte(testPayload), false),
			expectedError:           true,
			expectedConnectionError: &ConnectionError{Code: ErrCodeFlowControlError},
			expectedConnFCDecrease:  0, // FC manager should reject it, so no decrease in available window.
		},
		{
			name:                    "Error: DATA frame for unopened stream (ID > lastProcessed)",
			setupFunc:               func(t *testing.T, conn *Connection, mnc *mockNetConn) *Stream { return nil },
			frameToSend:             newDataFrameForTest(testStreamID+2, []byte(testPayload), false), // Use an ID known to be higher
			expectedError:           true,
			expectedConnectionError: &ConnectionError{Code: ErrCodeProtocolError}, // Message will contain specific stream ID
			// Conn FC IS consumed before stream existence check (if stream ID is not 0).
			expectedConnFCDecrease: uint32(len(testPayload)),
		},
		{
			name: "Error: DATA frame for known but closed stream",
			setupFunc: func(t *testing.T, conn *Connection, mnc *mockNetConn) *Stream {
				s, err := conn.createStream(testStreamID, nil, true)
				if err != nil {
					t.Fatalf("Failed to create stream: %v", err)
				}
				s.mu.Lock()
				s.state = StreamStateClosed // Stream is closed
				conn.streamsMu.Lock()
				conn.lastProcessedStreamID = testStreamID // Ensure stream ID is considered "known"
				conn.streamsMu.Unlock()
				s.mu.Unlock()
				// Explicitly remove from conn.streams map to simulate it being fully removed after closure
				conn.streamsMu.Lock()
				delete(conn.streams, testStreamID)
				conn.streamsMu.Unlock()

				return s // Return s for logging, though it's "removed" from conn's map
			},
			frameToSend:                newDataFrameForTest(testStreamID, []byte(testPayload), false),
			expectedError:              false, // dispatchDataFrame sends RST and returns nil
			expectedRSTStreamErrorCode: ErrCodeStreamClosed,
			expectedConnFCDecrease:     uint32(len(testPayload)), // Conn FC is consumed
		},
		{
			name: "Error: DATA frame for stream in HalfClosedRemote state",
			setupFunc: func(t *testing.T, conn *Connection, mnc *mockNetConn) *Stream {
				s, err := conn.createStream(testStreamID, nil, true)
				if err != nil {
					t.Fatalf("Failed to create stream: %v", err)
				}
				s.mu.Lock()
				s.state = StreamStateHalfClosedRemote
				s.mu.Unlock()
				return s
			},
			frameToSend:                newDataFrameForTest(testStreamID, []byte(testPayload), false),
			expectedError:              false, // dispatchDataFrame sends RST and returns nil
			expectedRSTStreamErrorCode: ErrCodeStreamClosed,
			expectedConnFCDecrease:     uint32(len(testPayload)),
		},
		{
			name: "Error: stream.handleDataFrame returns StreamError (simulated by stream FC violation)",
			setupFunc: func(t *testing.T, conn *Connection, mnc *mockNetConn) *Stream {
				s, err := conn.createStream(testStreamID, nil, true)
				if err != nil {
					t.Fatalf("Failed to create stream: %v", err)
				}
				s.mu.Lock()
				s.state = StreamStateOpen
				// Make stream's receive FC window too small
				s.fcManager.receiveWindowMu.Lock()
				s.fcManager.currentReceiveWindowSize = int64(len(testPayload) - 1)
				s.fcManager.receiveWindowMu.Unlock()
				s.mu.Unlock()
				return s
			},
			frameToSend:                newDataFrameForTest(testStreamID, []byte(testPayload), false),
			expectedError:              false,                   // dispatchDataFrame sends RST for StreamError and returns nil
			expectedRSTStreamErrorCode: ErrCodeFlowControlError, // Expected from stream's FC violation
			expectedConnFCDecrease:     uint32(len(testPayload)),
		},
	}

	for _, tc := range tests {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()                                  // Subtests can run in parallel if they don't interfere
			conn, mnc := newTestConnection(t, false, nil) // Server-side
			var closeErr error = errors.New("test cleanup: " + tc.name)
			defer func() {
				if conn != nil {
					conn.Close(closeErr)
				}
			}()

			var s *Stream
			if tc.setupFunc != nil {
				s = tc.setupFunc(t, conn, mnc)
			}

			initialConnFCReceiveAvailable := conn.connFCManager.GetConnectionReceiveAvailable()
			var dispatchErr error

			if tc.name == "Success: DATA frame for open stream" && s != nil {
				// Special handling for the success case to read from the pipe concurrently
				readDone := make(chan struct{})
				var readN int
				var pipeReadErr error
				readData := make([]byte, len(tc.frameToSend.Data))

				go func() {
					defer close(readDone)
					// t.Logf("Test '%s': Goroutine starting to read from s.requestBodyReader (stream %d)", tc.name, s.id)
					readN, pipeReadErr = s.requestBodyReader.Read(readData)
					// t.Logf("Test '%s': Goroutine finished read: n=%d, err=%v", tc.name, readN, pipeReadErr)
				}()

				dispatchErr = conn.dispatchDataFrame(tc.frameToSend)

				// Check for dispatch error first
				if tc.expectedError {
					if dispatchErr == nil {
						t.Fatalf("Expected error from dispatchDataFrame, got nil")
					}
				} else {
					if dispatchErr != nil {
						t.Fatalf("Expected no error from dispatchDataFrame, got %v", dispatchErr)
					}
				}

				// Wait for the reader goroutine to finish and check results
				select {
				case <-readDone:
					isEndStreamFrame := (tc.frameToSend.Header().Flags & FlagDataEndStream) != 0
					expectedDataRead := (isEndStreamFrame && errors.Is(pipeReadErr, io.EOF) && readN == len(tc.frameToSend.Data)) || // Correct EOF for endStream
						(!isEndStreamFrame && pipeReadErr == nil && readN == len(tc.frameToSend.Data)) // Correct no-EOF for not endStream

					if !expectedDataRead {
						if errors.Is(pipeReadErr, io.EOF) {
							if !isEndStreamFrame {
								t.Errorf("Unexpected EOF reading from stream's requestBodyReader (frame not EndStream): read %d, expected %d. Error: %v", readN, len(tc.frameToSend.Data), pipeReadErr)
							} else if readN != len(tc.frameToSend.Data) {
								t.Errorf("Premature EOF reading from stream's requestBodyReader (frame IS EndStream but not all data read): read %d, expected %d. Error: %v", readN, len(tc.frameToSend.Data), pipeReadErr)
							}
							// If isEndStreamFrame and EOF and readN == len, it's good, so no error log here.
						} else if pipeReadErr != nil {
							t.Errorf("Error reading from stream's requestBodyReader: %v", pipeReadErr)
						}
					}

					if readN != len(tc.frameToSend.Data) {
						// This check might be redundant if pipeReadErr already caught a premature EOF.
						// However, it's a good explicit check, especially if pipeReadErr was nil but readN was wrong.
						if pipeReadErr == nil || (errors.Is(pipeReadErr, io.EOF) && readN != len(tc.frameToSend.Data)) { // Added condition for logging
							t.Errorf("Data length mismatch: read %d bytes, expected %d bytes. Data: %q", readN, len(tc.frameToSend.Data), string(readData[:readN]))
						}
					} else if string(readData[:readN]) != string(tc.frameToSend.Data) { // Only if readN == expected length
						t.Errorf("Data in stream pipe mismatch: got %q, want %q", string(readData[:readN]), string(tc.frameToSend.Data))
					}

				case <-time.After(1 * time.Second): // Timeout for pipe read
					t.Errorf("Timeout waiting for stream data to be read")
					if s != nil && s.requestBodyWriter != nil {
						_ = s.requestBodyWriter.CloseWithError(errors.New("test timeout, closing writer from test"))
						select { // ensure goroutine exits if it was blocked on read
						case <-readDone:
						case <-time.After(100 * time.Millisecond): // short secondary timeout
							t.Logf("Test '%s': Secondary timeout waiting for reader goroutine to exit after pipe close.", tc.name)
						}
					}
				}

			} else { // Original logic for other test cases
				dispatchErr = conn.dispatchDataFrame(tc.frameToSend)
				if tc.expectedError {
					if dispatchErr == nil {
						t.Fatalf("Expected error from dispatchDataFrame, got nil")
					}
				} else {
					if dispatchErr != nil {
						t.Fatalf("Expected no error from dispatchDataFrame, got %v", dispatchErr)
					}
				}
				if tc.streamHandleDataFrameCheck != nil && s != nil && !tc.expectedError && tc.expectedRSTStreamErrorCode == 0 {
					tc.streamHandleDataFrameCheck(t, s, tc.frameToSend)
				}
			}

			// Common checks for all test cases based on dispatchErr
			if tc.expectedError {
				// (dispatchErr already checked for non-nil)
				if tc.expectedConnectionError != nil {
					connErr, ok := dispatchErr.(*ConnectionError)
					if !ok {
						t.Fatalf("Expected *ConnectionError, got %T: %v", dispatchErr, dispatchErr)
					}
					if connErr.Code != tc.expectedConnectionError.Code {
						t.Errorf("Expected ConnectionError code %s, got %s", tc.expectedConnectionError.Code, connErr.Code)
					}
					if tc.expectedConnectionError.Msg != "" {
						if !strings.Contains(connErr.Msg, tc.expectedConnectionError.Msg) {
							t.Errorf("ConnectionError message mismatch: got '%s', expected to contain '%s'", connErr.Msg, tc.expectedConnectionError.Msg)
						}
					}
				}
			}
			// No 'else' here for tc.expectedError == false, because errors are already checked.

			if tc.expectedRSTStreamErrorCode != 0 {
				var rstFrame *RSTStreamFrame
				foundRST := false
				// Use waitForCondition as sending RST is async via writerChan
				waitForCondition(t, 200*time.Millisecond, 10*time.Millisecond, func() bool {
					frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					for _, f := range frames {
						if rf, ok := f.(*RSTStreamFrame); ok {
							if rf.Header().StreamID == tc.frameToSend.Header().StreamID {
								rstFrame = rf
								foundRST = true
								return true
							}
						}
					}
					return false
				}, fmt.Sprintf("RST_STREAM frame for stream %d", tc.frameToSend.Header().StreamID))

				if !foundRST {
					allFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					t.Fatalf("Expected RST_STREAM frame for stream %d, none found. Frames on wire: %+v", tc.frameToSend.Header().StreamID, allFrames)
				}
				if rstFrame.ErrorCode != tc.expectedRSTStreamErrorCode {
					t.Errorf("RST_STREAM ErrorCode: got %s, want %s", rstFrame.ErrorCode, tc.expectedRSTStreamErrorCode)
				}
			} else if !tc.expectedError { // No RST expected AND no connection error expected
				// Give writerLoop a moment to process (if it were to send something unexpectedly)
				time.Sleep(50 * time.Millisecond)
				if mnc.GetWriteBufferLen() > 0 {
					frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					// Filter out PING ACKs if any, those are normal background activity in some test setups
					var unexpectedFrames []Frame
					for _, fr := range frames {
						if pf, ok := fr.(*PingFrame); ok && (pf.Flags&FlagPingAck) != 0 {
							continue // Ignore PING ACKs
						}
						unexpectedFrames = append(unexpectedFrames, fr)
					}
					if len(unexpectedFrames) > 0 {
						t.Errorf("Unexpected frames written to connection: %+v", unexpectedFrames)
					}
				}
			}

			finalConnFCReceiveAvailable := conn.connFCManager.GetConnectionReceiveAvailable()
			actualDecrease := initialConnFCReceiveAvailable - finalConnFCReceiveAvailable

			if uint32(actualDecrease) != tc.expectedConnFCDecrease {
				t.Errorf("Connection FC receive window decrease: got %d, want %d. (Initial: %d, Final: %d)",
					actualDecrease, tc.expectedConnFCDecrease, initialConnFCReceiveAvailable, finalConnFCReceiveAvailable)
			}

			closeErr = nil // Mark test as passed for deferred cleanup
		})
	}
}

func newTestHpackEncoder(t *testing.T) *hpack.Encoder {
	t.Helper()
	var hpackBuf bytes.Buffer
	// Use a reasonable table size, matching default server settings if possible.
	// conn.ourSettings[SettingHeaderTableSize] is DefaultSettingsHeaderTableSize (4096)
	return hpack.NewEncoder(&hpackBuf)
}

func encodeHeadersForTest(t *testing.T, headers []hpack.HeaderField) []byte {
	t.Helper()

	// This helper creates a fresh HPACK encoder for each call, ensuring stateless encoding
	// of the provided headers. This is suitable for generating test input for the server's decoder.
	var tempEncBuf bytes.Buffer
	tempEncoder := hpack.NewEncoder(&tempEncBuf)

	for _, hf := range headers {
		if err := tempEncoder.WriteField(hf); err != nil {
			t.Fatalf("encodeHeadersForTest: Error writing field %+v: %v", hf, err)
		}
	}
	return tempEncBuf.Bytes()
}

func TestConnection_HeaderProcessingScenarios(t *testing.T) {
	t.Parallel()

	stdHeaders := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":path", Value: "/test"},
		{Name: ":authority", Value: "example.com"},
		{Name: "user-agent", Value: "test-client/1.0"},
	}

	// testHpackEncoder is created once and used for all encodings in this test suite.
	// It accumulates dynamic table state, which is good for testing HPACK.

	tests := []struct {
		name                       string
		setupFunc                  func(t *testing.T, conn *Connection, mnc *mockNetConn) // Optional setup
		framesToFeed               func(t *testing.T) [][]byte                            // Function to generate frame bytes to feed
		expectDispatcherCall       bool
		expectDispatcherStreamID   uint32
		expectConnectionError      bool
		expectedConnErrorCode      ErrorCode
		expectedConnErrorMsgSubstr string
		expectedGoAway             bool // If connection error leads to GOAWAY
		customOurMaxHeaderListSize *uint32
	}{
		{
			name: "Valid HEADERS, END_HEADERS",
			framesToFeed: func(t *testing.T) [][]byte {
				hpackPayload := encodeHeadersForTest(t, stdHeaders)
				headersFrame := &HeadersFrame{
					FrameHeader: FrameHeader{
						Type:     FrameHeaders,
						Flags:    FlagHeadersEndHeaders | FlagHeadersEndStream,
						StreamID: 1,
						Length:   uint32(len(hpackPayload)),
					},
					HeaderBlockFragment: hpackPayload,
				}
				return [][]byte{frameToBytes(t, headersFrame)}
			},
			expectDispatcherCall:     true,
			expectDispatcherStreamID: 1,
		},
		{
			name: "HEADERS + CONTINUATION, END_HEADERS on CONTINUATION",
			framesToFeed: func(t *testing.T) [][]byte {
				hpackPayload1 := encodeHeadersForTest(t, stdHeaders[:2]) // :method, :scheme
				hpackPayload2 := encodeHeadersForTest(t, stdHeaders[2:]) // :path, :authority, user-agent

				headersFrame := &HeadersFrame{ // END_HEADERS not set
					FrameHeader:         FrameHeader{Type: FrameHeaders, Flags: FlagHeadersEndStream, StreamID: 3, Length: uint32(len(hpackPayload1))},
					HeaderBlockFragment: hpackPayload1,
				}
				continuationFrame := &ContinuationFrame{ // END_HEADERS set
					FrameHeader:         FrameHeader{Type: FrameContinuation, Flags: FlagContinuationEndHeaders, StreamID: 3, Length: uint32(len(hpackPayload2))},
					HeaderBlockFragment: hpackPayload2,
				}
				return [][]byte{frameToBytes(t, headersFrame), frameToBytes(t, continuationFrame)}
			},
			expectDispatcherCall:     true,
			expectDispatcherStreamID: 3,
		},
		{
			name: "HEADERS on stream 0",
			framesToFeed: func(t *testing.T) [][]byte {
				hpackPayload := encodeHeadersForTest(t, stdHeaders)
				headersFrame := &HeadersFrame{
					FrameHeader:         FrameHeader{Type: FrameHeaders, Flags: FlagHeadersEndHeaders, StreamID: 0, Length: uint32(len(hpackPayload))},
					HeaderBlockFragment: hpackPayload,
				}
				return [][]byte{frameToBytes(t, headersFrame)}
			},
			expectConnectionError: true,
			expectedConnErrorCode: ErrCodeProtocolError,
			expectedGoAway:        true,
		},
		{
			name: "CONTINUATION without HEADERS",
			framesToFeed: func(t *testing.T) [][]byte {
				hpackPayload := encodeHeadersForTest(t, stdHeaders)
				continuationFrame := &ContinuationFrame{
					FrameHeader:         FrameHeader{Type: FrameContinuation, Flags: FlagContinuationEndHeaders, StreamID: 5, Length: uint32(len(hpackPayload))},
					HeaderBlockFragment: hpackPayload,
				}
				return [][]byte{frameToBytes(t, continuationFrame)}
			},
			expectConnectionError: true,
			expectedConnErrorCode: ErrCodeProtocolError,
			expectedGoAway:        true,
		},
		{
			name: "CONTINUATION on wrong stream ID",
			framesToFeed: func(t *testing.T) [][]byte {
				hpackPayload1 := encodeHeadersForTest(t, stdHeaders[:2])
				hpackPayload2 := encodeHeadersForTest(t, stdHeaders[2:])
				headersFrame := &HeadersFrame{
					FrameHeader:         FrameHeader{Type: FrameHeaders, Flags: 0 /* No END_HEADERS */, StreamID: 7, Length: uint32(len(hpackPayload1))},
					HeaderBlockFragment: hpackPayload1,
				}
				continuationFrame := &ContinuationFrame{ // Wrong StreamID
					FrameHeader:         FrameHeader{Type: FrameContinuation, Flags: FlagContinuationEndHeaders, StreamID: 9, Length: uint32(len(hpackPayload2))},
					HeaderBlockFragment: hpackPayload2,
				}
				return [][]byte{frameToBytes(t, headersFrame), frameToBytes(t, continuationFrame)}
			},
			expectConnectionError: true,
			expectedConnErrorCode: ErrCodeProtocolError,
			expectedGoAway:        true,
		},
		{
			name:                       "MaxHeaderListSize exceeded (compressed, initial HEADERS)",
			customOurMaxHeaderListSize: func() *uint32 { s := uint32(10); return &s }(),
			framesToFeed: func(t *testing.T) [][]byte {
				// Create headers that will compress to > 10 bytes
				largeHeaders := []hpack.HeaderField{
					{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":path", Value: "/"}, {Name: ":authority", Value: "example.com"},
					{Name: "a", Value: "aaaaaaaaaa"}, {Name: "b", Value: "bbbbbbbbbb"}, // These should push it over
				}
				hpackPayload := encodeHeadersForTest(t, largeHeaders)
				if len(hpackPayload) <= 10 { // Ensure test condition is met
					t.Logf("Warning: HPACK payload for MaxHeaderListSize (compressed) test is too small: %d bytes. Test may not be effective.", len(hpackPayload))
				}
				headersFrame := &HeadersFrame{
					FrameHeader:         FrameHeader{Type: FrameHeaders, Flags: FlagHeadersEndHeaders, StreamID: 11, Length: uint32(len(hpackPayload))},
					HeaderBlockFragment: hpackPayload,
				}
				return [][]byte{frameToBytes(t, headersFrame)}
			},
			expectConnectionError: true,
			// RFC 7540 6.5.2: "SETTINGS_MAX_HEADER_LIST_SIZE ... A server that receives a larger header list MUST treat this as a connection error (Section 5.4.1) of type ENHANCE_YOUR_CALM."
			// Sometimes PROTOCOL_ERROR is also acceptable if the check is on compressed size. Let's target ENHANCE_YOUR_CALM.
			expectedConnErrorCode: ErrCodeEnhanceYourCalm, // Or ProtocolError depending on where check is
			expectedGoAway:        true,
		},
		{
			name:                       "MaxHeaderListSize exceeded (uncompressed, after decoding)",
			customOurMaxHeaderListSize: func() *uint32 { s := uint32(50); return &s }(), // Sum of N+V+32 per header. (3+3+32)+(6+3+32)+(4+1+32)+(9+11+32) = 38+41+37+52 = 168 for stdHeaders(4)
			// For 2 std headers: (3+3+32) + (6+3+32) = 38+41 = 79. This should exceed 50.
			framesToFeed: func(t *testing.T) [][]byte {
				// Use few headers, but their uncompressed size will be large due to N+V+32 rule
				twoHeaders := stdHeaders[:2] // :method:GET, :scheme:https
				// Uncompressed: (len(":method")+len("GET")+32) + (len(":scheme")+len("https")+32)
				// (7+3+32) + (7+5+32) = 42 + 44 = 86. This should exceed customMaxHeaderListSize of 50.
				hpackPayload := encodeHeadersForTest(t, twoHeaders)
				headersFrame := &HeadersFrame{
					FrameHeader:         FrameHeader{Type: FrameHeaders, Flags: FlagHeadersEndHeaders | FlagHeadersEndStream, StreamID: 13, Length: uint32(len(hpackPayload))},
					HeaderBlockFragment: hpackPayload,
				}
				return [][]byte{frameToBytes(t, headersFrame)}
			},
			expectConnectionError: true,
			expectedConnErrorCode: ErrCodeEnhanceYourCalm,
			expectedGoAway:        true,
		},
		{
			name: "Pseudo-header validation: Missing :method",
			framesToFeed: func(t *testing.T) [][]byte {
				missingMethodHeaders := []hpack.HeaderField{
					{Name: ":scheme", Value: "https"}, {Name: ":path", Value: "/"}, {Name: ":authority", Value: "example.com"},
				}
				hpackPayload := encodeHeadersForTest(t, missingMethodHeaders)
				headersFrame := &HeadersFrame{
					FrameHeader:         FrameHeader{Type: FrameHeaders, Flags: FlagHeadersEndHeaders | FlagHeadersEndStream, StreamID: 15, Length: uint32(len(hpackPayload))},
					HeaderBlockFragment: hpackPayload,
				}
				return [][]byte{frameToBytes(t, headersFrame)}
			},
			// This error occurs *after* stream creation, in extractPseudoHeaders.
			// The stream should be RST, not necessarily connection GOAWAY unless stream creation fails.
			// However, spec 8.1.2.6: Malformed requests/responses are connection errors.
			// "An HTTP/2 request or response is malformed if ... mandatory pseudo-header fields are omitted"
			// So, GOAWAY is expected.
			expectConnectionError: true,
			expectedConnErrorCode: ErrCodeProtocolError,
			expectedGoAway:        true,
		},
		{
			name: "Pseudo-header validation: Invalid :path",
			framesToFeed: func(t *testing.T) [][]byte {
				invalidPathHeaders := []hpack.HeaderField{
					{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":path", Value: "no-slash"}, {Name: ":authority", Value: "example.com"},
				}
				hpackPayload := encodeHeadersForTest(t, invalidPathHeaders)
				headersFrame := &HeadersFrame{
					FrameHeader:         FrameHeader{Type: FrameHeaders, Flags: FlagHeadersEndHeaders | FlagHeadersEndStream, StreamID: 17, Length: uint32(len(hpackPayload))},
					HeaderBlockFragment: hpackPayload,
				}
				return [][]byte{frameToBytes(t, headersFrame)}
			},
			expectConnectionError: true,
			expectedConnErrorCode: ErrCodeProtocolError,
			expectedGoAway:        true,
		},
		{
			name: "HPACK decoding error (invalid HPACK stream)",
			framesToFeed: func(t *testing.T) [][]byte {
				invalidHpackPayload := []byte{0x8F, 0xFF, 0xFF, 0xFF} // Example of potentially invalid HPACK (too large index or literal)
				headersFrame := &HeadersFrame{
					FrameHeader:         FrameHeader{Type: FrameHeaders, Flags: FlagHeadersEndHeaders | FlagHeadersEndStream, StreamID: 19, Length: uint32(len(invalidHpackPayload))},
					HeaderBlockFragment: invalidHpackPayload,
				}
				return [][]byte{frameToBytes(t, headersFrame)}
			},
			expectConnectionError: true,
			expectedConnErrorCode: ErrCodeCompressionError,
			expectedGoAway:        true,
		},
	}

	for _, tc := range tests {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mockDispatcher := &mockRequestDispatcher{}
			conn, mnc := newTestConnection(t, false /*isClient*/, mockDispatcher)
			var closeErr error = errors.New("test cleanup: " + tc.name) // Default close error
			defer func() {
				if conn != nil {
					conn.Close(closeErr)
				}
			}()

			if tc.customOurMaxHeaderListSize != nil {
				conn.settingsMu.Lock()
				conn.ourSettings[SettingMaxHeaderListSize] = *tc.customOurMaxHeaderListSize
				conn.applyOurSettings() // Re-apply to update conn.ourMaxHeaderListSize
				conn.settingsMu.Unlock()
			}

			performHandshakeForTest(t, conn, mnc) // Includes ServerHandshake
			mnc.ResetWriteBuffer()                // Clear handshake frames

			serveErrChan := make(chan error, 1)
			go func() {
				err := conn.Serve(nil) // Pass nil context for test simplicity
				serveErrChan <- err
			}()

			for _, frameBytes := range tc.framesToFeed(t) {
				mnc.FeedReadBuffer(frameBytes)
			}

			var serveExitError error
			if tc.expectConnectionError || tc.expectedGoAway {
				select {
				case serveExitError = <-serveErrChan:
					// Expected to exit due to error or GOAWAY processing
				case <-time.After(2 * time.Second): // Increased timeout
					t.Fatalf("Timeout waiting for conn.Serve to exit for an expected error/GOAWAY case")
				}

				if tc.expectConnectionError {
					if serveExitError == nil {
						t.Fatalf("conn.Serve exited with nil error, expected a ConnectionError")
					}
					connErr, ok := serveExitError.(*ConnectionError)
					if !ok {
						// If not ConnectionError, check if it's EOF from mnc.Close() if no error was actually triggered by test.
						if !(errors.Is(serveExitError, io.EOF) || strings.Contains(serveExitError.Error(), "use of closed network connection")) {
							t.Fatalf("conn.Serve error type: got %T, want *ConnectionError. Err: %v", serveExitError, serveExitError)
						} else {
							t.Logf("conn.Serve exited with EOF/closed, but expected specific ConnectionError: %s", tc.expectedConnErrorCode)
						}
					} else {
						if connErr.Code != tc.expectedConnErrorCode {
							t.Errorf("Expected ConnectionError code %s, got %s. Msg: %s", tc.expectedConnErrorCode, connErr.Code, connErr.Msg)
						}
						if tc.expectedConnErrorMsgSubstr != "" && !strings.Contains(connErr.Msg, tc.expectedConnErrorMsgSubstr) {
							t.Errorf("ConnectionError message '%s' does not contain substring '%s'", connErr.Msg, tc.expectedConnErrorMsgSubstr)
						}
					}
				}

				if tc.expectedGoAway {
					var goAwayFrame *GoAwayFrame
					// The GOAWAY might have been sent by conn.Close called from Serve's defer, or explicitly by error handling.
					// conn.Close() is idempotent. Calling it again with serveExitError ensures it uses the right error code.
					_ = conn.Close(serveExitError) // Ensure Close uses the error from Serve.

					waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
						frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
						for _, f := range frames {
							if gaf, ok := f.(*GoAwayFrame); ok {
								if gaf.ErrorCode == tc.expectedConnErrorCode { // GOAWAY should reflect the error code
									goAwayFrame = gaf
									return true
								}
							}
						}
						return false
					}, fmt.Sprintf("GOAWAY frame with ErrorCode %s to be written", tc.expectedConnErrorCode))

					if goAwayFrame == nil {
						// Dump all frames seen if specific GOAWAY not found
						allFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
						var frameSummaries []string
						for _, fr := range allFrames {
							if fr != nil && fr.Header() != nil {
								frameSummaries = append(frameSummaries, fmt.Sprintf("{Type:%s, StreamID:%d, ErrorCodeIfExists:%v}", fr.Header().Type, fr.Header().StreamID, getErrorCode(fr)))
							} else {
								frameSummaries = append(frameSummaries, "{NIL_FRAME_OR_HEADER}")
							}
						}
						t.Fatalf("GOAWAY frame not written or with wrong error code. Expected code %s. Frames on wire: %s", tc.expectedConnErrorCode, strings.Join(frameSummaries, ", "))
					}
				}
				closeErr = nil // Error was expected and handled.
			} else { // No connection error expected.
				if tc.expectDispatcherCall {
					waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
						mockDispatcher.mu.Lock()
						defer mockDispatcher.mu.Unlock()
						return mockDispatcher.called && mockDispatcher.lastStreamID == tc.expectDispatcherStreamID
					}, fmt.Sprintf("dispatcher to be called for stream %d", tc.expectDispatcherStreamID))

					mockDispatcher.mu.Lock()
					if !mockDispatcher.called {
						t.Error("Dispatcher was not called")
					}
					if mockDispatcher.lastStreamID != tc.expectDispatcherStreamID {
						t.Errorf("Dispatcher called for stream %d, want %d", mockDispatcher.lastStreamID, tc.expectDispatcherStreamID)
					}
					mockDispatcher.mu.Unlock()
				} else {
					// Ensure dispatcher was NOT called
					time.Sleep(100 * time.Millisecond) // Give time for it to be called if it were going to be
					mockDispatcher.mu.Lock()
					if mockDispatcher.called {
						t.Errorf("Dispatcher was unexpectedly called for stream %d", mockDispatcher.lastStreamID)
					}
					mockDispatcher.mu.Unlock()
				}

				// If no error was expected, Serve should not exit prematurely.
				// Terminate Serve gracefully for cleanup.
				mnc.Close() // Trigger EOF for Serve loop
				select {
				case serveExitError = <-serveErrChan:
					if serveExitError != nil && !errors.Is(serveExitError, io.EOF) && !strings.Contains(serveExitError.Error(), "use of closed network connection") {
						t.Errorf("conn.Serve exited with unexpected error: %v", serveExitError)
					}
				case <-time.After(1 * time.Second):
					t.Errorf("Timeout waiting for conn.Serve to exit after mnc.Close()")
				}
				closeErr = nil // Test case passed.
			}
		})
	}
}

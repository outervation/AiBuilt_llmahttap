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

package http2

/*
   NOTE: The conn tests are split into multiple files to reduce the load on LLM
   context compared to having just one big file open.
*/


import (
	"encoding/hex"
	"errors"
	"fmt" // Added import
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/llmahttap/v2/internal/logger"
)


func TestServerHandshake_Success(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestServerHandshake_Success")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc) // This calls ServerHandshake() and does basic verification

	// Additional assertions for successful handshake:
	// 1. ServerHandshake() returned nil (implicit in performHandshakeForTest not failing)
	// 2. Server's settingsAckTimeoutTimer should be active
	conn.settingsMu.RLock()
	timerActive := conn.settingsAckTimeoutTimer != nil
	conn.settingsMu.RUnlock()
	if !timerActive {
		t.Error("Expected settingsAckTimeoutTimer to be active after server sent its initial SETTINGS")
	}

	// performHandshakeForTest already verifies server sent its SETTINGS and an ACK to client's SETTINGS.
	// So, if we reach here, the test passed.
	closeErr = nil
}

func TestServerHandshake_Failure_InvalidPrefaceContent(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	var closeErr error = errors.New("test cleanup: TestServerHandshake_Failure_InvalidPrefaceContent")
	// The defer will be a safety net. Test logic will explicitly call conn.Close().
	// closeErr will be updated by the test logic to reflect the outcome.
	defer func() {
		if conn != nil && closeErr != nil { // Only close with error if test indicated one
			conn.Close(closeErr)
		} else if conn != nil {
			conn.Close(nil) // Default close if test passed or didn't set closeErr
		}
	}()

	invalidPreface := "THIS IS NOT THE PREFACE YOU ARE LOOKING FOR"
	mnc.FeedReadBuffer([]byte(invalidPreface))

	err := conn.ServerHandshake()
	if err == nil {
		closeErr = errors.New("ServerHandshake succeeded with invalid preface, expected error")
		t.Fatal(closeErr)
	}

	connErr, ok := err.(*ConnectionError)
	if !ok {
		closeErr = fmt.Errorf("Expected ConnectionError, got %T: %v", err, err)
		t.Fatal(closeErr)
	}
	if connErr.Code != ErrCodeProtocolError {
		e := fmt.Errorf("Expected ProtocolError for invalid preface, got %s", connErr.Code)
		if closeErr == nil {
			closeErr = e
		} else {
			closeErr = fmt.Errorf("%w; %w", closeErr, e)
		}
		t.Error(e)
	}

	// Explicitly close the connection with the error from ServerHandshake.
	// This should trigger the GOAWAY frame.
	_ = conn.Close(err) // The error from this Close() itself isn't primary for this check.

	// Verify GOAWAY frame with PROTOCOL_ERROR
	var goAwayFrame *GoAwayFrame
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, f := range frames {
			if gaf, ok := f.(*GoAwayFrame); ok {
				if gaf.ErrorCode == ErrCodeProtocolError {
					goAwayFrame = gaf
					return true
				}
			}
		}
		return false
	}, "GOAWAY frame with PROTOCOL_ERROR to be written")

	if goAwayFrame == nil {
		e := errors.New("GOAWAY frame with PROTOCOL_ERROR not found")
		if closeErr == nil {
			closeErr = e
		} else {
			closeErr = fmt.Errorf("%w; %w", closeErr, e)
		}
		t.Error(e) // Use Error to allow other checks like IsClosed to run
	} else {
		// This 'else' block executes if goAwayFrame is NOT nil.
		// Now check its LastStreamID.
		if goAwayFrame.LastStreamID != 0 {
			e := fmt.Errorf("GOAWAY LastStreamID: got %d, want 0 for preface error", goAwayFrame.LastStreamID)
			// Standard pattern for accumulating errors into closeErr
			if closeErr == nil {
				closeErr = e
			} else {
				closeErr = fmt.Errorf("%w; %w", closeErr, e)
			}
			t.Error(e) // Report this specific error
		}
	}

	// Verify mockNetConn is closed
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed")

	// If all checks passed, set closeErr to nil so the defer knows the test logic was successful.
	if !t.Failed() {
		closeErr = nil
	}
}

func TestHandleSettingsFrame_ClientAckToServerSettings(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil) // Server-side
	var closeErr error = errors.New("test cleanup: TestHandleSettingsFrame_ClientAckToServerSettings")
	defer func() { conn.Close(closeErr) }()

	performHandshakeForTest(t, conn, mnc) // This handles initial SETTINGS from both sides, including server sending its initial SETTINGS.

	// Simulate client sending ACK to server's initial SETTINGS.
	// The server's initial SETTINGS frame was sent during performHandshakeForTest.
	// Now, the client (mock) needs to send an ACK for those.
	serverSettingsAckFrame := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, Flags: FlagSettingsAck, StreamID: 0, Length: 0},
	}

	// The server is not running its Serve() loop yet in this test structure.
	// To test ACK processing for server's settings, we directly call handleSettingsFrame.
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


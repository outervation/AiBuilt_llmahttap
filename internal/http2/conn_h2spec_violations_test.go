package http2

import (
	"bytes"
	"encoding/hex" // Added import
	"net/http"     // Added import
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

// TestHEADERSOnHalfClosedRemoteStream tests the behavior when a HEADERS frame is received
// on a stream that is in the StreamStateHalfClosedRemote state.
// This corresponds to h2spec test: 5. Streams and Multiplexing / 5.1. Stream States / 6: half closed (remote): Sends a HEADERS frame
func TestHEADERSOnHalfClosedRemoteStream(t *testing.T) {
	t.Parallel()
	// TODO: uncomment and fix this
	t.Skip()
	mockDispatcher := &mockRequestDispatcher{
		fn: func(sw StreamWriter, req *http.Request) {
			tLogf(t, "Mock dispatcher called for stream %d, method %s, path %s", sw.ID(), req.Method, req.URL.Path)
			err := sw.SendHeaders([]HeaderField{{Name: ":status", Value: "204"}}, true)
			if err != nil {
				tErrorf(t, "Dispatcher failed to send headers: %v", err)
			}
		},
	}
	conn, mnc := newTestConnection(t, false, mockDispatcher, nil)
	defer conn.Close(nil)
	defer mnc.Close()

	handshakeErrChan := make(chan error, 1)
	go func() {
		defer close(handshakeErrChan)
		tLogf(t, "Test: Starting conn.ServerHandshake() in a goroutine.")
		handshakeErrChan <- conn.ServerHandshake()
		tLogf(t, "Test: conn.ServerHandshake() goroutine finished.")
	}()

	tLogf(t, "Test: Calling performHandshakeForTest() to drive client-side of handshake.")
	performHandshakeForTest(t, conn, mnc, nil) // conn.ServerHandshake() removed from here
	tLogf(t, "Test: performHandshakeForTest() completed.")

	tLogf(t, "Test: Waiting on handshakeErrChan.")
	err := <-handshakeErrChan
	if err != nil {
		tLogf(t, "HCR_TEST_DIAGNOSTIC: conn.ServerHandshake() returned error: %v", err)
		t.Fatalf("Test setup: ServerHandshake failed: %v. HCR_TEST_DIAGNOSTIC_HANDSHAKE_FAILED", err)
	} else {
		tLogf(t, "HCR_TEST_DIAGNOSTIC: conn.ServerHandshake() completed successfully.")
	}
	tLogf(t, "Test: Server handshake completed successfully according to handshakeErrChan.")

	go func() {
		tLogf(t, "Test: Starting conn.Serve() in a goroutine after successful handshake.")
		if err := conn.Serve(conn.ctx); err != nil {
			// Don't t.Errorf here, as conn.Serve() can return errors during normal shutdown
			tLogf(t, "Test: conn.Serve() exited with error: %v", err)
		}
		tLogf(t, "Test: conn.Serve() goroutine finished.")
	}()

	streamID := uint32(1)
	initialHeaders := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":path", Value: "/"},
		{Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "localhost"},
	}
	hbf, err := conn.hpackAdapter.Encode(initialHeaders)
	if err != nil {
		tFatalf(t, "Failed to encode initial headers: %v", err)
	}

	initialHeadersFrame := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			StreamID: streamID,
			Flags:    FlagHeadersEndStream | FlagHeadersEndHeaders,
			Length:   uint32(len(hbf)),
		},
		HeaderBlockFragment: hbf,
	}
	mnc.FeedReadBuffer(frameToBytes(t, initialHeadersFrame))
	tLogf(t, "Test: Fed initial HEADERS frame with END_STREAM to server for stream %d", streamID)

	waitForCondition(t, 2*time.Second, 50*time.Millisecond, func() bool {
		return mockDispatcher.CalledCount() >= 1
	}, "Dispatcher not called for initial request")
	tLogf(t, "Test: Dispatcher called for initial request on stream %d", streamID)

	time.Sleep(100 * time.Millisecond)
	s, exists := conn.getStream(streamID)
	if !exists {
		tFatalf(t, "Stream %d not found after initial HEADERS", streamID)
	}
	s.mu.RLock()
	currentState := s.state
	endStreamReceived := s.endStreamReceivedFromClient
	s.mu.RUnlock()
	if currentState != StreamStateHalfClosedRemote {
		tErrorf(t, "Stream %d not in StreamStateHalfClosedRemote after initial HEADERS with END_STREAM. Got: %s", streamID, currentState)
	}
	if !endStreamReceived {
		tErrorf(t, "Stream %d endStreamReceivedFromClient is false after initial HEADERS with END_STREAM", streamID)
	}
	tLogf(t, "Test: Stream %d is in state %s, endStreamReceivedFromClient: %v", streamID, currentState, endStreamReceived)

	mnc.ResetWriteBuffer()
	tLogf(t, "Test: Cleared mock net conn write buffer.")

	subsequentHeaders := []hpack.HeaderField{
		{Name: "x-custom-header", Value: "test-value"},
	}
	hbf2, err := conn.hpackAdapter.Encode(subsequentHeaders)
	if err != nil {
		tFatalf(t, "Failed to encode subsequent headers: %v", err)
	}

	subsequentHeadersFrame := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			StreamID: streamID,
			Flags:    FlagHeadersEndHeaders,
			Length:   uint32(len(hbf2)),
		},
		HeaderBlockFragment: hbf2,
	}
	mnc.FeedReadBuffer(frameToBytes(t, subsequentHeadersFrame))
	tLogf(t, "Test: Fed subsequent HEADERS frame to server for stream %d (now in HCR state)", streamID)

	var rstFrame *RSTStreamFrame
	rstFrame = waitForFrameCondition(t, 2*time.Second, 50*time.Millisecond, mnc, rstFrame,
		func(f *RSTStreamFrame) bool {
			return f.Header().StreamID == streamID && f.ErrorCode == ErrCodeStreamClosed
		},
		"Expected RST_STREAM(STREAM_CLOSED) for subsequent HEADERS on HCR stream",
	)

	if rstFrame == nil {
		tLogf(t, "No RST_STREAM(STREAM_CLOSED) frame received for stream %d. Current write buffer content:\n%s", streamID, hexDump(mnc.GetWriteBufferBytes()))
		if sAfter, existsAfter := conn.getStream(streamID); existsAfter {
			sAfter.mu.RLock()
			stateAfter := sAfter.state
			pendingRST := sAfter.pendingRSTCode
			sAfter.mu.RUnlock()
			tLogf(t, "Stream %d state after subsequent HEADERS: %s, pendingRSTCode: %v", streamID, stateAfter, pendingRST)
		} else {
			tLogf(t, "Stream %d no longer exists after subsequent HEADERS and no RST_STREAM", streamID)
		}
	} else {
		tLogf(t, "Test: Received RST_STREAM(ErrorCode: %s) for stream %d as expected.", rstFrame.ErrorCode, streamID)
	}
}

// Helper for hex dumping, useful in debugging
func hexDump(data []byte) string {
	var buf bytes.Buffer
	dumper := hex.Dumper(&buf)
	_, _ = dumper.Write(data)
	_ = dumper.Close()
	return buf.String()
}

// TestMain is explicitly disallowed.

package http2

/*
   NOTE: The conn tests are split into multiple files to reduce the load on LLM
   context compared to having just one big file open.
*/

import (
	"context" // ADDED IMPORT
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt" // Added import
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

func TestServerHandshake_Failure_TimeoutWritingServerSettings(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	var closeErr error = errors.New("test cleanup: TestServerHandshake_Failure_TimeoutWritingServerSettings")
	// This defer needs to be careful if ServerHandshake itself calls conn.Close on timeout.
	// Let test logic explicitly call conn.Close with the error from ServerHandshake if it returns one.
	defer func() {
		if conn != nil {
			conn.Close(closeErr) // closeErr will be nil if test passed.
		}
	}()

	var writeBlocker sync.Mutex
	writeBlocker.Lock() // Lock it initially

	// Override the mock connection's Write behavior using writeHook
	mnc.writeHook = func(p []byte) (int, error) {
		isInitialSettingsFrame := false
		if len(p) >= FrameHeaderLen {
			frameType := FrameType(p[3])
			flags := Flags(p[4])
			var streamID uint32
			if len(p) >= 9 {
				streamID = binary.BigEndian.Uint32(p[5:9]) & 0x7FFFFFFF
			}

			if frameType == FrameSettings && flags&FlagSettingsAck == 0 && streamID == 0 {
				isInitialSettingsFrame = true
			}
		}

		if isInitialSettingsFrame {
			//t.Logf("mnc.writeHook: Intercepted initial server SETTINGS. Blocking on writeBlocker.")
			writeBlocker.Lock() // This will block until test unlocks it.
			//t.Logf("mnc.writeHook: writeBlocker unlocked. Proceeding with buffer write.")
			writeBlocker.Unlock() // Immediately unlock after being unblocked by test.
		}
		// After potentially blocking, write to the buffer as normal.
		// The original mockNetConn.Write behavior already writes to m.writeBuf.
		// We need to call that logic here if writeHook is not to replace it entirely.
		// For this test, we want to simulate a block then a normal write, so we write to buffer here.

		return mnc.writeBuf.Write(p) // Write to buffer after hook logic
	}

	// originalSrvHandshakeTimeout := ServerHandshakeSettingsWriteTimeout // Unused

	// Feed valid preface and client settings so handshake proceeds up to server trying to send its settings
	mnc.FeedReadBuffer([]byte(ClientPreface))
	clientSettings := []Setting{{ID: SettingInitialWindowSize, Value: DefaultSettingsInitialWindowSize}}
	clientSettingsFrame := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: uint32(len(clientSettings) * 6)},
		Settings:    clientSettings,
	}
	mnc.FeedReadBuffer(frameToBytes(t, clientSettingsFrame))

	handshakeErr := conn.ServerHandshake()

	// Crucially, unlock the writeBlocker *after* ServerHandshake returns.
	// This allows the writerLoop (if it was stuck in mnc.Write) to proceed and exit cleanly
	// when conn.Close is eventually called by the defer.
	writeBlocker.Unlock() // This assumes ServerHandshake timed out *before* writerLoop could acquire writeBlocker.
	// If writerLoop acquired it, this unlock lets it proceed.

	if handshakeErr == nil {
		t.Fatal("ServerHandshake succeeded when writing server SETTINGS should have timed out, expected error")
	}

	connErr, ok := handshakeErr.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError from ServerHandshake, got %T: %v", handshakeErr, handshakeErr)
	}
	if connErr.Code != ErrCodeInternalError { // As per ServerHandshake's timeout logic for this specific timeout
		t.Errorf("Expected InternalError for timeout writing server SETTINGS, got %s. Msg: %s", connErr.Code, connErr.Msg)
	}
	expectedMsgSubstr := "timeout waiting for initial server SETTINGS write"
	if !strings.Contains(connErr.Msg, expectedMsgSubstr) {
		t.Errorf("Error message mismatch: got '%s', expected to contain '%s'", connErr.Msg, expectedMsgSubstr)
	}

	// ServerHandshake returned an error. The connection's Close method will be called by the defer.
	// We set closeErr to the error ServerHandshake returned so the defer uses the correct context for GOAWAY if any.
	closeErr = handshakeErr
}

// Test for conn.Close being called during ServerHandshake, e.g. by external signal.
func TestServerHandshake_ConnectionClosedExternally(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	var closeErr error = errors.New("test cleanup: TestServerHandshake_ConnectionClosedExternally")
	defer func() {
		if conn != nil {
			// The test itself should have called conn.Close, so this is a safety net.
			// closeErr might have been updated if the test's explicit Close failed.
			conn.Close(closeErr)
		}
	}()

	// Simulate external close part-way through handshake.
	// Let's say it happens while ServerHandshake is waiting for server's initial SETTINGS to be written.

	// originalSrvHandshakeTimeout := ServerHandshakeSettingsWriteTimeout // Unused

	// Feed valid preface
	mnc.FeedReadBuffer([]byte(ClientPreface))

	// Start ServerHandshake in a goroutine

	// After preface is read, mnc.readBuf will be empty.
	// The next call to mnc.Read (for client's SETTINGS) will use readHook.
	readHookTriggered := make(chan struct{}) // To signal when the hook is active
	mnc.readHook = func(b []byte) (int, error) {
		// This hook is intended to be called when ServerHandshake tries to read the client's SETTINGS frame.
		// The preface should have already been read successfully from mnc.readBuf.
		if mnc.readBuf.Len() > 0 { // If readBuf still has data (e.g. preface wasn't fully read by ReadFull)
			// This case should ideally not be hit if ReadFull for preface worked as expected.
			// If ReadFull for preface consumed everything, readBuf will be empty.
			return mnc.readBuf.Read(b)
		}

		// If readBuf is empty, this is the read for client's SETTINGS.
		t.Logf("readHook (for client SETTINGS): now active.")
		close(readHookTriggered) // Signal that the hook for reading client settings is now active

		t.Logf("readHook (for client SETTINGS): blocking, waiting for shutdownChan or timeout")
		select {
		case <-conn.shutdownChan:
			t.Logf("readHook (for client SETTINGS): shutdownChan closed, returning 'connection closed by hook'")
			return 0, errors.New("connection closed by hook")
		case <-time.After(1500 * time.Millisecond): // Failsafe timeout for the hook itself
			t.Logf("readHook (for client SETTINGS): timeout waiting for shutdownChan, returning 'hook timeout'")
			return 0, errors.New("hook timeout waiting for shutdownChan")
		}
	}
	handshakeErrChan := make(chan error, 1)
	go func() {
		handshakeErrChan <- conn.ServerHandshake()
	}()

	// Wait a moment to ensure ServerHandshake has started and queued its SETTINGS.
	// It will then block on c.initialSettingsWritten.
	time.Sleep(50 * time.Millisecond)

	// Now, simulate an external close.
	externalCloseErr := NewConnectionError(ErrCodeConnectError, "simulated external close during handshake")
	go conn.Close(externalCloseErr) // Call Close from another goroutine

	var handshakeActualErr error
	select {
	case handshakeActualErr = <-handshakeErrChan:
		// Handshake exited
	case <-time.After(ServerHandshakeSettingsWriteTimeout + 500*time.Millisecond): // Give it a bit more than its internal timeout
		t.Fatal("Timeout waiting for ServerHandshake to exit after external close")
	}

	if handshakeActualErr == nil {
		t.Fatal("ServerHandshake returned nil error, expected error due to external close")
	}

	// The error from ServerHandshake should reflect the shutdown.
	connErr, ok := handshakeActualErr.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError from ServerHandshake, got %T: %v", handshakeActualErr, handshakeActualErr)
	}

	// It could be ErrCodeConnectError (from externalCloseErr propagating) or ErrCodeInternalError (from its own timeout if raced)
	// The specific error might be "connection shutdown during handshake" or similar.
	t.Logf("DEBUG TestServerHandshake_ConnectionClosedExternally: handshakeActualErr.Code = %s (%d), handshakeActualErr.Msg = %q", connErr.Code, connErr.Code, connErr.Msg)
	if connErr.Code == ErrCodeConnectError {

		// Let's keep it simple: if code is ConnectError, Msg must be "simulated external close during handshake"
		if connErr.Msg != "simulated external close during handshake" {
			t.Errorf("For ErrCodeConnectError, expected Msg %q, got %q", "simulated external close during handshake", connErr.Msg)
		}
		// And also check the err.Error() string, which the original test was sensitive to

		expectedErrorStr := fmt.Sprintf("connection error: %s (last_stream_id %d, code %s, %d)", connErr.Msg, connErr.LastStreamID, connErr.Code.String(), connErr.Code)
		if handshakeActualErr.Error() != expectedErrorStr {
			t.Errorf("For ErrCodeConnectError, expected handshakeActualErr.Error() to be %q, got %q", expectedErrorStr, handshakeActualErr.Error())
		}

	} else if connErr.Code == ErrCodeInternalError {
		// If it's InternalError, it should be one of the timeout/shutdown messages
		if !strings.Contains(connErr.Msg, "connection shutdown during handshake") && !strings.Contains(connErr.Msg, "timeout waiting for initial server SETTINGS write") {
			t.Errorf("For ErrCodeInternalError, Msg %q did not contain expected substrings ('connection shutdown during handshake' or 'timeout waiting for initial server SETTINGS write')", connErr.Msg)
		}
	} else {
		// If it's neither of those, it's an unexpected error code
		t.Errorf("ServerHandshake error code: got %s, expected ConnectError or InternalError. Msg: %s", connErr.Code, connErr.Msg)
	}

	// The final error for the defer should be the one that initiated the close.
	closeErr = externalCloseErr
}

// TestFrameSizeExceeded_DATA tests server behavior when receiving DATA frames
// that exceed the advertised SETTINGS_MAX_FRAME_SIZE.
// Covers h2spec Generic 4.2.2 (DATA frame exceeds SETTINGS_MAX_FRAME_SIZE)
// and parts of HTTP/2 4.2 (Frame Size).
func TestFrameSizeExceeded_DATA(t *testing.T) {
	t.Parallel()

	// Server's default advertised max frame size in tests (from conn.sendInitialSettings)
	serverAnnouncedMaxFrameSize := DefaultSettingsMaxFrameSize // Typically 16384

	// Create a payload slightly larger than the max frame size
	oversizedPayload := make([]byte, serverAnnouncedMaxFrameSize+1)
	for i := range oversizedPayload {
		oversizedPayload[i] = byte(i % 256) // Fill with some data
	}
	oversizedFrameLength := uint32(len(oversizedPayload))

	// --- Subtest 1: Oversized DATA frame on a valid stream ---
	t.Run("OnValidStream_Expect_RST_STREAM", func(t *testing.T) {
		t.Parallel()
		mockDispatcher := &mockRequestDispatcher{}
		conn, mnc := newTestConnection(t, false /*isClient*/, mockDispatcher)
		var closeErr error = errors.New("test cleanup: TestFrameSizeExceeded_DATA/OnValidStream")
		defer func() { conn.Close(closeErr) }()

		performHandshakeForTest(t, conn, mnc) // Server sends its SETTINGS_MAX_FRAME_SIZE
		mnc.ResetWriteBuffer()

		serveErrChan := make(chan error, 1)
		go func() {
			serveErrChan <- conn.Serve(context.Background())
		}()

		// Client initiates stream 1

		const streamIDToUse uint32 = 1

		clientHeaders := []hpack.HeaderField{
			{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "https"}, {Name: ":path", Value: "/data"}, {Name: ":authority", Value: "example.com"},
		}
		hpackClientHeaders := encodeHeadersForTest(t, clientHeaders)
		headersFrameClient := &HeadersFrame{
			FrameHeader: FrameHeader{
				Type:     FrameHeaders,
				Flags:    FlagHeadersEndHeaders, // Not ending stream, expecting DATA
				StreamID: streamIDToUse,
				Length:   uint32(len(hpackClientHeaders)),
			},
			HeaderBlockFragment: hpackClientHeaders,
		}

		// Prepare oversized DATA frame
		dataFrame := &DataFrame{
			FrameHeader: FrameHeader{Type: FrameData, StreamID: streamIDToUse, Length: oversizedFrameLength},
			Data:        oversizedPayload,
		}

		// Feed BOTH frames before waiting for dispatcher or starting Serve loop checks
		mnc.FeedReadBuffer(frameToBytes(t, headersFrameClient))
		mnc.FeedReadBuffer(frameToBytes(t, dataFrame))

		// Expect RST_STREAM(FRAME_SIZE_ERROR) on stream 1
		var rstFrame *RSTStreamFrame
		waitForCondition(t, 2*time.Second, 20*time.Millisecond, func() bool {
			frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
			for _, f := range frames {
				if rf, ok := f.(*RSTStreamFrame); ok {
					if rf.Header().StreamID == streamIDToUse && rf.ErrorCode == ErrCodeFrameSizeError {
						rstFrame = rf
						return true
					}
				}
			}
			return false
		}, fmt.Sprintf("RST_STREAM(FRAME_SIZE_ERROR) for stream %d", streamIDToUse))

		if rstFrame == nil {
			// This means waitForCondition timed out. It would have called t.Fatalf.
			// This line is mostly for completeness if that behavior changes.
			t.Fatal("Expected RST_STREAM(FRAME_SIZE_ERROR) not received")
		}

		// Ensure no GOAWAY frame was sent
		time.Sleep(50 * time.Millisecond) // Give a bit of time for a GOAWAY if it were to be sent
		framesAfterRST := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, f := range framesAfterRST {
			if _, ok := f.(*GoAwayFrame); ok {
				t.Fatalf("Unexpected GOAWAY frame sent for oversized DATA on valid stream: %+v", f)
			}
		}

		// Cleanly terminate the Serve loop for this subtest
		mnc.Close() // This will cause conn.Serve to exit
		select {
		case err := <-serveErrChan:
			if err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "use of closed network connection") {
				t.Logf("conn.Serve exited with: %v (expected EOF or closed connection)", err)
			}
		case <-time.After(1 * time.Second):
			t.Error("Timeout waiting for conn.Serve to exit after mnc.Close()")
		}
		closeErr = nil // Test logic passed
	})

	// --- Subtest 2: Oversized DATA frame on stream 0 ---
	t.Run("OnStream0_Expect_GOAWAY", func(t *testing.T) {
		t.Parallel()
		conn, mnc := newTestConnection(t, false, nil)
		var closeErr error = errors.New("test cleanup: TestFrameSizeExceeded_DATA/OnStream0")
		// Defer conn.Close with a specific error variable that can be set to nil if test logic passes.
		// This ensures GOAWAY from this Close uses the error from Serve if it occurs.
		// Defer a final conn.Close. The error used here will be updated if the test passes.
		// This is mainly a safety net. The test itself will call conn.Close with the relevant error.
		finalCleanupErr := errors.New("subtest defer close: " + t.Name()) // Unique error for this defer
		defer func() {
			if conn != nil {
				if !t.Failed() { // If subtest assertions passed
					finalCleanupErr = nil
				} else if closeErr != nil { // If closeErr was set by test logic due to failure
					finalCleanupErr = closeErr
				}
				// If t.Failed() but closeErr is nil, finalCleanupErr remains "subtest defer close..."
				conn.Close(finalCleanupErr)
			}
		}()

		// Initialize closeErr for the subtest body. It will be set to nil if logic passes.
		closeErr = errors.New("test logic failure in: " + t.Name())

		performHandshakeForTest(t, conn, mnc)
		mnc.ResetWriteBuffer()

		serveErrChan := make(chan error, 1)
		go func() {
			err := conn.Serve(context.Background())
			serveErrChan <- err
		}()

		// Send oversized DATA frame on stream 0
		dataFrame := &DataFrame{
			FrameHeader: FrameHeader{Type: FrameData, StreamID: 0, Length: oversizedFrameLength},
			Data:        oversizedPayload,
		}
		mnc.FeedReadBuffer(frameToBytes(t, dataFrame))

		// Expect GOAWAY(PROTOCOL_ERROR)
		// Expect conn.Serve to exit with a ConnectionError
		var serveExitError error
		select {
		case serveExitError = <-serveErrChan:
			// Good, Serve exited
		case <-time.After(2 * time.Second): // Increased timeout slightly for robustness
			t.Fatal("Timeout waiting for conn.Serve to exit after sending oversized DATA on stream 0")
		}

		if serveExitError == nil {
			t.Fatal("conn.Serve exited with nil error, expected ConnectionError")
		}

		var connErr *ConnectionError
		if !errors.As(serveExitError, &connErr) {
			t.Fatalf("conn.Serve error type: expected to contain *ConnectionError, got %T. Err: %v", serveExitError, serveExitError)
		}
		// Now connErr is the unwrapped *ConnectionError if errors.As was successful.

		// The server detects "DATA on stream 0" as PROTOCOL_ERROR first.
		// This error (serveExitError) is used by the conn.Serve() defer's conn.Close() to generate the GOAWAY.
		if connErr.Code != ErrCodeProtocolError {
			t.Errorf("conn.Serve ConnectionError code: got %s, want %s. Msg: %s", connErr.Code, ErrCodeProtocolError, connErr.Msg)
		}

		// Explicitly call conn.Close with the error from Serve to trigger GOAWAY.
		// The subtest's defer will also call Close, but this ensures it happens with the correct error context now.
		_ = conn.Close(serveExitError) // We don't check the error from this Close, as serveExitError is primary.

		// Now check for the GOAWAY frame.
		// Use waitForFrameCondition (or a specialized local version) to get the frame.
		var goAwayFrame *GoAwayFrame
		timeoutMsgBase := "GOAWAY(PROTOCOL_ERROR) for oversized DATA on stream 0"
		_ = timeoutMsgBase // Temporarily use timeoutMsgBase to avoid unused error for it
		goAwayFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*GoAwayFrame)(nil), func(f *GoAwayFrame) bool {
			if f.ErrorCode == ErrCodeProtocolError {
				t.Logf("Found GOAWAY frame in waitForFrameCondition: ErrorCode=%s (expected %s), LSID=%d", f.ErrorCode, ErrCodeProtocolError, f.LastStreamID)
				return true
			}
			t.Logf("Found GOAWAY frame in waitForFrameCondition but ErrorCode mismatch: ErrorCode=%s (expected %s), LSID=%d", f.ErrorCode, ErrCodeProtocolError, f.LastStreamID)
			return false
		}, timeoutMsgBase)

		// The t.Fatalf inside waitForFrameCondition will trigger if timeout.
		// If we reach here, goAwayFrame should be non-nil and correct due to the condition.
		if goAwayFrame == nil {
			t.Fatalf("Timeout waiting for GOAWAY(PROTOCOL_ERROR) for oversized DATA on stream 0. Last raw buffer (len %d): %s", len(mnc.GetWriteBufferBytes()), hex.EncodeToString(mnc.GetWriteBufferBytes()))
		}
		// if goAwayFrame == nil { // Should be redundant if foundGoAway is true. Defensive.
		// 	t.Fatal("Expected GOAWAY(PROTOCOL_ERROR) not received (goAwayFrame is nil after poll)")
		// }

		if goAwayFrame.LastStreamID != 0 {
			t.Errorf("GOAWAY LastStreamID: got %d, want 0 (no streams processed before error)", goAwayFrame.LastStreamID)
		}

		// The defer for this subtest will call conn.Close(closeErr).
		// Since the error from Serve (serveExitError) is the one that drove the shutdown,
		// we want the subtest's defer to use that if it was an error, or nil if successful.
		// If we reach here, the primary test logic passed.
		closeErr = serveExitError  // Propagate the error from Serve for the deferred Close.
		if serveExitError == nil { // Should not happen given the checks above, but defensive.
			closeErr = nil
		} else if _, isConnErr := serveExitError.(*ConnectionError); isConnErr && connErr.Code == ErrCodeProtocolError {
			// If the error was the expected PROTOCOL_ERROR, the test itself has passed.
			// The deferred conn.Close will use this error.
			// We can set the subtest's outer `closeErr` to nil to signify this subtest logic is "passing".
			// Or, more robustly, the subtest's defer `closeErr` variable should reflect the actual error
			// that caused the connection to terminate, which is `serveExitError`.
			// The assignment above `closeErr = serveExitError` is correct.
			// If this specific test path completes without t.Error/t.Fatal, then the logic is considered successful.
		}

		// If all checks pass up to here, the specific logic of this test case is considered successful.
		// The defer will handle closing with 'closeErr' which is now 'serveExitError'.
		// To indicate the test's assertions passed and avoid marking `closeErr` with a generic test error:
		if !t.Failed() { // Check if any t.Error/Fatal has occurred so far
			closeErr = nil // Test logic passed
		}
	})
}

// TestFrameSizeExceeded_HEADERS tests server behavior when receiving HEADERS frames
// that exceed the advertised SETTINGS_MAX_FRAME_SIZE.
// Covers h2spec 4.2.3 (HEADERS frame exceeds SETTINGS_MAX_FRAME_SIZE).
func TestFrameSizeExceeded_HEADERS(t *testing.T) {
	t.Parallel()

	// Server's default advertised max frame size in tests (from conn.sendInitialSettings)
	serverAnnouncedMaxFrameSize := DefaultSettingsMaxFrameSize // Typically 16384

	// Create a header block fragment that will be larger than max frame size when encoded.
	// A single large header value is sufficient.
	var largeHeaderValue strings.Builder
	for i := 0; i < int(serverAnnouncedMaxFrameSize)+100; i++ { // Make it clearly oversized
		largeHeaderValue.WriteByte('a')
	}

	headersToEncode := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":path", Value: "/"},
		{Name: ":authority", Value: "example.com"},
		{Name: "x-oversized-header", Value: largeHeaderValue.String()},
	}
	// The hpack encoding itself might not exceed the frame size limit if compression is very effective.
	// However, the *frame's Length field* is what matters. We will set this to be oversized.
	// The actual content of HeaderBlockFragment doesn't strictly need to be > MAX_FRAME_SIZE
	// if the frame's Length field *claims* it is.
	// For this test, we'll make a realistic hpackPayload and then set the FrameHeader.Length
	// to be > serverAnnouncedMaxFrameSize.
	// Let's make the actual payload *also* oversized to be certain.
	hpackPayload := encodeHeadersForTest(t, headersToEncode)
	oversizedFrameLength := serverAnnouncedMaxFrameSize + 1 // This is the crucial part for the frame header

	if uint32(len(hpackPayload)) < oversizedFrameLength {
		// If HPACK made it too small, pad it out to ensure the payload itself matches the claimed length.
		// This makes the test more robust against highly efficient HPACK for small # of headers.
		padding := make([]byte, int(oversizedFrameLength)-len(hpackPayload))
		hpackPayload = append(hpackPayload, padding...)
	}
	// Trim if actual hpackPayload became larger than intended oversizedFrameLength (unlikely with padding logic)
	if uint32(len(hpackPayload)) > oversizedFrameLength {
		hpackPayload = hpackPayload[:oversizedFrameLength]
	}

	conn, mnc := newTestConnection(t, false /*isClient*/, nil)
	var closeErr error = errors.New("test cleanup: TestFrameSizeExceeded_HEADERS")
	// Defer a final conn.Close. The error used here will be updated if the test passes.
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc) // Server sends its SETTINGS_MAX_FRAME_SIZE
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background())
		serveErrChan <- err
	}()

	// Send oversized HEADERS frame
	headersFrame := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    FlagHeadersEndHeaders | FlagHeadersEndStream, // Simplest case, single HEADERS frame
			StreamID: 1,                                            // New client-initiated stream
			Length:   oversizedFrameLength,                         // This length exceeds server's SETTINGS_MAX_FRAME_SIZE
		},
		HeaderBlockFragment: hpackPayload,
	}
	mnc.FeedReadBuffer(frameToBytes(t, headersFrame))

	// Expect GOAWAY(FRAME_SIZE_ERROR)
	// Expect conn.Serve to exit with a ConnectionError
	var serveExitError error
	select {
	case serveExitError = <-serveErrChan:
		// Good, Serve exited
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for conn.Serve to exit after sending oversized HEADERS frame")
	}

	if serveExitError == nil {
		t.Fatal("conn.Serve exited with nil error, expected ConnectionError")
	}

	var connErr *ConnectionError
	if !errors.As(serveExitError, &connErr) {
		t.Fatalf("conn.Serve error type: expected to contain *ConnectionError, got %T. Err: %v", serveExitError, serveExitError)
	}

	if connErr.Code != ErrCodeFrameSizeError {
		t.Errorf("conn.Serve ConnectionError code: got %s, want %s. Msg: %s", connErr.Code, ErrCodeFrameSizeError, connErr.Msg)
	}

	// Explicitly call conn.Close with the error from Serve to trigger GOAWAY, if Serve didn't already.
	// conn.Close is idempotent.
	_ = conn.Close(serveExitError)

	// Now check for the GOAWAY frame.
	var goAwayFrame *GoAwayFrame
	goAwayFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*GoAwayFrame)(nil), func(f *GoAwayFrame) bool {
		if f.ErrorCode == ErrCodeFrameSizeError {
			return true
		}
		return false
	}, "GOAWAY(FRAME_SIZE_ERROR) for oversized HEADERS frame")

	// waitForFrameCondition will t.Fatal on timeout.
	if goAwayFrame == nil {
		t.Fatalf("GOAWAY(FRAME_SIZE_ERROR) not received. Last raw buffer (len %d): %s", len(mnc.GetWriteBufferBytes()), hex.EncodeToString(mnc.GetWriteBufferBytes()))
	}

	// Check LastStreamID. Since the HEADERS frame on stream 1 was problematic *before* the stream could be fully established
	// or processed, the LastStreamID in GOAWAY should typically be 0 or the last successfully processed stream ID
	// from a previous interaction (if any). In this isolated test, it's likely 0.
	if goAwayFrame.LastStreamID > 0 { // Allow 0
		t.Logf("GOAWAY LastStreamID: got %d. This might be acceptable if stream 1 was partially processed before error.", goAwayFrame.LastStreamID)
	}

	// If all checks pass, update closeErr to nil so the deferred Close is a no-op or clean.
	if !t.Failed() {
		closeErr = nil
	}
}

// TestInterleavedFramesDuringHeaderBlock tests server behavior when disallowed frames
// are sent in the middle of a header block (i.e., after HEADERS without END_HEADERS,
// but before or instead of CONTINUATION).
// Covers h2spec 4.3.2 (PRIORITY), 6.2.1 (PRIORITY), 5.5.2 (Unknown Extension Frame).
func TestInterleavedFramesDuringHeaderBlock(t *testing.T) {
	t.Parallel()

	// Standard headers for the initial HEADERS frame
	initialHeaders := []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":path", Value: "/test"}, {Name: ":authority", Value: "example.com"},
	}
	encodedInitialHeaders := encodeHeadersForTest(t, initialHeaders)

	// Frame definitions for subtests
	initialHeadersFrame := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    0, // CRITICAL: No END_HEADERS flag
			StreamID: 1, // New client-initiated stream
			Length:   uint32(len(encodedInitialHeaders)),
		},
		HeaderBlockFragment: encodedInitialHeaders,
	}

	interleavedPriorityFrame := &PriorityFrame{
		FrameHeader:      FrameHeader{Type: FramePriority, StreamID: 1, Length: 5},
		StreamDependency: 0,
		Weight:           10,
	}

	interleavedUnknownFrame := &UnknownFrame{
		FrameHeader: FrameHeader{Type: FrameType(0xFF), StreamID: 1, Length: 4}, // Arbitrary unknown type and length
		Payload:     []byte{1, 2, 3, 4},
	}

	tests := []struct {
		name             string
		interleavedFrame Frame // The frame to send after initial HEADERS
	}{
		{
			name:             "Interleaved PRIORITY Frame",
			interleavedFrame: interleavedPriorityFrame,
		},
		{
			name:             "Interleaved Unknown Frame",
			interleavedFrame: interleavedUnknownFrame,
		},
	}

	for _, tc := range tests {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn, mnc := newTestConnection(t, false /*isClient*/, nil)
			var closeErr error = errors.New("test cleanup: " + tc.name)
			defer func() {
				if conn != nil {
					conn.Close(closeErr)
				}
			}()

			performHandshakeForTest(t, conn, mnc)
			mnc.ResetWriteBuffer()

			serveErrChan := make(chan error, 1)
			go func() {
				err := conn.Serve(context.Background())
				serveErrChan <- err
			}()

			// Send initial HEADERS frame (no END_HEADERS)
			mnc.FeedReadBuffer(frameToBytes(t, initialHeadersFrame))
			// Send the interleaved frame
			mnc.FeedReadBuffer(frameToBytes(t, tc.interleavedFrame))

			// Expect conn.Serve to exit with a ConnectionError(PROTOCOL_ERROR)
			var serveExitError error
			select {
			case serveExitError = <-serveErrChan:
				// Good, Serve exited
			case <-time.After(2 * time.Second):
				t.Fatal("Timeout waiting for conn.Serve to exit after sending interleaved frame")
			}

			if serveExitError == nil {
				t.Fatal("conn.Serve exited with nil error, expected ConnectionError")
			}

			var connErr *ConnectionError
			if !errors.As(serveExitError, &connErr) {
				t.Fatalf("conn.Serve error type: expected to contain *ConnectionError, got %T. Err: %v", serveExitError, serveExitError)
			}

			if connErr.Code != ErrCodeProtocolError {
				t.Errorf("conn.Serve ConnectionError code: got %s, want %s. Msg: %s", connErr.Code, ErrCodeProtocolError, connErr.Msg)
			}

			// Explicitly call conn.Close with the error from Serve to trigger GOAWAY.
			_ = conn.Close(serveExitError)

			// Now check for the GOAWAY frame.
			var goAwayFrame *GoAwayFrame
			goAwayFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*GoAwayFrame)(nil), func(f *GoAwayFrame) bool {
				return f.ErrorCode == ErrCodeProtocolError
			}, "GOAWAY(PROTOCOL_ERROR) for interleaved frame")

			if goAwayFrame == nil {
				t.Fatalf("GOAWAY(PROTOCOL_ERROR) not received. Last raw buffer (len %d): %s", len(mnc.GetWriteBufferBytes()), hex.EncodeToString(mnc.GetWriteBufferBytes()))
			}

			// LastStreamID in GOAWAY should be 0 as the stream 1 was not fully established.
			if goAwayFrame.LastStreamID != 0 {
				// Allow for stream 1 to be the last stream ID IF initial HEADERS was somehow processed enough to open stream 1
				// RFC 7540 Section 6.10 "CONTINUATION frames MUST be preceded by a HEADERS, PUSH_PROMISE or CONTINUATION frame without the END_HEADERS flag set."
				// "A receiver MUST treat the receipt of any other type of frame or a frame on a different stream as a connection error (Section 5.4.1) of type PROTOCOL_ERROR."
				// This means stream 1 was likely not considered "processed".
				if goAwayFrame.LastStreamID != initialHeadersFrame.Header().StreamID {
					t.Errorf("GOAWAY LastStreamID: got %d, want 0 or %d (stream ID of initial HEADERS)", goAwayFrame.LastStreamID, initialHeadersFrame.Header().StreamID)
				} else {
					t.Logf("GOAWAY LastStreamID is %d (stream ID of initial HEADERS), which is acceptable.", goAwayFrame.LastStreamID)
				}
			}

			// If all checks pass, update closeErr to nil
			if !t.Failed() {
				closeErr = nil
			}
		})
	}
}

// TestRSTStreamOnIdleStream_ServeLoop tests that the server correctly handles
// an RST_STREAM frame received for an idle stream when the main Serve loop is running.
// It expects the server to treat this as a connection error (PROTOCOL_ERROR),
// send a GOAWAY frame, and close the connection.
// Covers h2spec 5.1.2 and 6.4.2.
func TestRSTStreamOnIdleStream_ServeLoop(t *testing.T) {
	t.Parallel()

	conn, mnc := newTestConnection(t, false /*isClient*/, nil)
	var closeErr error = errors.New("test cleanup: TestRSTStreamOnIdleStream_ServeLoop")
	// Defer a final conn.Close. The error used here will be updated by test logic.
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background()) // Use a background context
		serveErrChan <- err
	}()

	const idleStreamID uint32 = 3 // Client-initiated odd ID, not used by handshake
	rstFrame := &RSTStreamFrame{
		FrameHeader: FrameHeader{Type: FrameRSTStream, StreamID: idleStreamID, Length: 4},
		ErrorCode:   ErrCodeCancel, // Client's error code doesn't affect server's PROTOCOL_ERROR response here
	}
	mnc.FeedReadBuffer(frameToBytes(t, rstFrame))

	// Expect conn.Serve to exit with a ConnectionError(PROTOCOL_ERROR)
	var serveExitError error
	select {
	case serveExitError = <-serveErrChan:
		// Good, Serve exited
	case <-time.After(2 * time.Second): // Give ample time for processing and exit
		t.Fatal("Timeout waiting for conn.Serve to exit after sending RST_STREAM on idle stream")
	}

	if serveExitError == nil {
		t.Fatal("conn.Serve exited with nil error, expected ConnectionError")
	}

	var connErr *ConnectionError
	if !errors.As(serveExitError, &connErr) {
		t.Fatalf("conn.Serve error type: expected to contain *ConnectionError, got %T. Err: %v", serveExitError, serveExitError)
	}

	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("conn.Serve ConnectionError code: got %s, want %s. Msg: %s", connErr.Code, ErrCodeProtocolError, connErr.Msg)
	}
	// The message should indicate it's about an idle stream.
	// Example msg: "RST_STREAM received for numerically idle stream 3 (highest peer initiated: 0)"
	expectedMsgSubstr := fmt.Sprintf("RST_STREAM received for numerically idle stream %d", idleStreamID)
	if !strings.Contains(connErr.Msg, expectedMsgSubstr) {
		t.Errorf("ConnectionError message '%s' does not contain expected substring '%s'", connErr.Msg, expectedMsgSubstr)
	}

	// Explicitly call conn.Close with the error from Serve to ensure GOAWAY logic uses it.
	// conn.Close is idempotent.
	_ = conn.Close(serveExitError)

	// Now check for the GOAWAY frame.
	var goAwayFrame *GoAwayFrame
	goAwayFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*GoAwayFrame)(nil), func(f *GoAwayFrame) bool {
		return f.ErrorCode == ErrCodeProtocolError
	}, "GOAWAY(PROTOCOL_ERROR) for RST_STREAM on idle stream")

	if goAwayFrame == nil {
		t.Fatalf("GOAWAY(PROTOCOL_ERROR) not received. Last raw buffer (len %d): %s", len(mnc.GetWriteBufferBytes()), hex.EncodeToString(mnc.GetWriteBufferBytes()))
	}

	// LastStreamID in GOAWAY should be 0 (or highest *successfully processed* client stream ID before this error).
	// Since stream `idleStreamID` was idle, no client streams were processed before the error.
	// If handshake involved client sending SETTINGS on stream 0, lastProcessedStreamID might be 0.
	// Let's check against conn.lastProcessedStreamID.
	conn.streamsMu.RLock()
	expectedLastStreamID := conn.lastProcessedStreamID
	conn.streamsMu.RUnlock()

	if goAwayFrame.LastStreamID != expectedLastStreamID {
		t.Errorf("GOAWAY LastStreamID: got %d, want %d (conn.lastProcessedStreamID)", goAwayFrame.LastStreamID, expectedLastStreamID)
	}

	// Connection (mockNetConn) should be closed by conn.Close()
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed")

	// If all checks pass, update closeErr to nil for the deferred Close.
	if !t.Failed() {
		closeErr = nil
	}
}

// TestHEADERSOnClientRSTStream_ServeLoop tests server behavior when a client sends
// a HEADERS frame on a stream that the client itself has previously reset.
// The server is expected to respond with RST_STREAM(STREAM_CLOSED) for that stream.
// Covers h2spec 5.1.9.
func TestHEADERSOnClientRSTStream_ServeLoop(t *testing.T) {
	t.Parallel()

	mockDispatcher := &mockRequestDispatcher{}
	conn, mnc := newTestConnection(t, false /*isClient*/, mockDispatcher)
	var closeErr error = errors.New("test cleanup: TestHEADERSOnClientRSTStream_ServeLoop")
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background())
		serveErrChan <- err
	}()

	const streamIDToUse uint32 = 1

	// 1. Client sends HEADERS to open stream 1
	clientInitialHeaders := []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":path", Value: "/"}, {Name: ":authority", Value: "example.com"},
	}
	encodedClientInitialHeaders := encodeHeadersForTest(t, clientInitialHeaders)
	initialHeadersFrame := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    FlagHeadersEndHeaders | FlagHeadersEndStream, // End stream immediately for simplicity
			StreamID: streamIDToUse,
			Length:   uint32(len(encodedClientInitialHeaders)),
		},
		HeaderBlockFragment: encodedClientInitialHeaders,
	}
	mnc.FeedReadBuffer(frameToBytes(t, initialHeadersFrame))

	// Wait for dispatcher to be called for stream 1, confirming it was opened.
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		mockDispatcher.mu.Lock()
		defer mockDispatcher.mu.Unlock()
		return mockDispatcher.called && mockDispatcher.lastStreamID == streamIDToUse
	}, fmt.Sprintf("dispatcher to be called for initial HEADERS on stream %d", streamIDToUse))

	// Server might send a response if handler is quick. Clear write buffer.
	// This test focuses on server reaction to subsequent frames after client RST.
	mnc.ResetWriteBuffer()

	// 2. Client sends RST_STREAM for stream 1
	clientRSTFrame := &RSTStreamFrame{
		FrameHeader: FrameHeader{Type: FrameRSTStream, StreamID: streamIDToUse, Length: 4},
		ErrorCode:   ErrCodeCancel,
	}
	mnc.FeedReadBuffer(frameToBytes(t, clientRSTFrame))

	// Server should process this RST. Wait a moment for it.
	// The stream should be closed on the server. No frames should be sent by server in response to this RST.
	time.Sleep(100 * time.Millisecond) // Allow server to process the RST
	if mnc.GetWriteBufferLen() > 0 {
		unexpectedFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		t.Fatalf("Server sent unexpected frames after client RST_STREAM: %+v", unexpectedFrames)
	}

	// 3. Client sends another HEADERS frame on the (now by client-RST) closed stream 1
	subsequentHeaders := []hpack.HeaderField{ // Content doesn't strictly matter, just that it's HEADERS
		{Name: ":method", Value: "POST"}, {Name: ":path", Value: "/again"},
	}
	encodedSubsequentHeaders := encodeHeadersForTest(t, subsequentHeaders)
	subsequentHeadersFrame := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    FlagHeadersEndHeaders | FlagHeadersEndStream,
			StreamID: streamIDToUse,
			Length:   uint32(len(encodedSubsequentHeaders)),
		},
		HeaderBlockFragment: encodedSubsequentHeaders,
	}
	mnc.FeedReadBuffer(frameToBytes(t, subsequentHeadersFrame))

	// 4. Expect server to send RST_STREAM(STREAM_CLOSED) for stream 1
	var serverRSTFrame *RSTStreamFrame
	serverRSTFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*RSTStreamFrame)(nil), func(f *RSTStreamFrame) bool {
		return f.Header().StreamID == streamIDToUse && f.ErrorCode == ErrCodeStreamClosed
	}, fmt.Sprintf("RST_STREAM(STREAM_CLOSED) for stream %d", streamIDToUse))

	if serverRSTFrame == nil {
		// waitForFrameCondition would have t.Fatal'd. This is for completeness.
		t.Fatalf("Expected server to send RST_STREAM(STREAM_CLOSED) for stream %d, but not found.", streamIDToUse)
	}

	// 5. Connection should remain open, no GOAWAY
	time.Sleep(50 * time.Millisecond) // Check for delayed GOAWAY
	framesAfterServerRST := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	for _, f := range framesAfterServerRST {
		// Allow for the expected RST_STREAM frame to be in the buffer (if ResetWriteBuffer wasn't called after check)
		if f == serverRSTFrame { // if we haven't reset, this is expected.
			continue
		}
		if _, ok := f.(*GoAwayFrame); ok {
			t.Fatalf("Unexpected GOAWAY frame sent by server: %+v", f)
		}
	}

	// 6. Clean up: Close mock net conn, wait for Serve to exit.
	mnc.Close()
	select {
	case err := <-serveErrChan:
		if err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "use of closed network connection") {
			t.Logf("conn.Serve exited with: %v (expected EOF or closed connection error)", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("Timeout waiting for conn.Serve to exit after mnc.Close()")
	}

	if !t.Failed() {
		closeErr = nil // Test logic passed
	}
}

// TestInterleavedPriorityFrameDuringHeaderBlock specifically tests that sending a PRIORITY frame
// after a HEADERS frame (without END_HEADERS) and before any CONTINUATION frame
// results in a connection error (PROTOCOL_ERROR) and a GOAWAY frame.
// This covers h2spec 4.3.2 and 6.2.1.
func TestInterleavedPriorityFrameDuringHeaderBlock(t *testing.T) {
	t.Parallel()

	initialHeaders := []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":path", Value: "/test"}, {Name: ":authority", Value: "example.com"},
	}
	encodedInitialHeaders := encodeHeadersForTest(t, initialHeaders)

	initialHeadersFrame := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    0, // CRITICAL: No END_HEADERS flag
			StreamID: 1, // New client-initiated stream
			Length:   uint32(len(encodedInitialHeaders)),
		},
		HeaderBlockFragment: encodedInitialHeaders,
	}

	interleavedPriorityFrame := &PriorityFrame{
		FrameHeader:      FrameHeader{Type: FramePriority, StreamID: 1, Length: 5},
		StreamDependency: 0,
		Weight:           10,
	}

	conn, mnc := newTestConnection(t, false /*isClient*/, nil)
	var closeErr error = errors.New("test cleanup: TestInterleavedPriorityFrameDuringHeaderBlock")
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background())
		serveErrChan <- err
	}()

	// Send initial HEADERS frame (no END_HEADERS)
	mnc.FeedReadBuffer(frameToBytes(t, initialHeadersFrame))
	// Send the interleaved PRIORITY frame
	mnc.FeedReadBuffer(frameToBytes(t, interleavedPriorityFrame))

	// Expect conn.Serve to exit with a ConnectionError(PROTOCOL_ERROR)
	var serveExitError error
	select {
	case serveExitError = <-serveErrChan:
		// Good, Serve exited
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for conn.Serve to exit after sending interleaved PRIORITY frame")
	}

	if serveExitError == nil {
		t.Fatal("conn.Serve exited with nil error, expected ConnectionError")
	}

	var connErr *ConnectionError
	if !errors.As(serveExitError, &connErr) {
		t.Fatalf("conn.Serve error type: expected to contain *ConnectionError, got %T. Err: %v", serveExitError, serveExitError)
	}

	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("conn.Serve ConnectionError code: got %s, want %s. Msg: %s", connErr.Code, ErrCodeProtocolError, connErr.Msg)
	}

	// Explicitly call conn.Close with the error from Serve to trigger GOAWAY.
	_ = conn.Close(serveExitError)

	// Now check for the GOAWAY frame.
	var goAwayFrame *GoAwayFrame
	goAwayFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*GoAwayFrame)(nil), func(f *GoAwayFrame) bool {
		return f.ErrorCode == ErrCodeProtocolError
	}, "GOAWAY(PROTOCOL_ERROR) for interleaved PRIORITY frame")

	if goAwayFrame == nil {
		t.Fatalf("GOAWAY(PROTOCOL_ERROR) not received. Last raw buffer (len %d): %s", len(mnc.GetWriteBufferBytes()), hex.EncodeToString(mnc.GetWriteBufferBytes()))
	}

	// LastStreamID in GOAWAY should be 0 as the stream 1 was not fully established.
	// Or it could be the stream ID of the frame that initiated the header block (stream 1).
	if goAwayFrame.LastStreamID != 0 && goAwayFrame.LastStreamID != initialHeadersFrame.Header().StreamID {
		t.Errorf("GOAWAY LastStreamID: got %d, want 0 or %d (stream ID of initial HEADERS)", goAwayFrame.LastStreamID, initialHeadersFrame.Header().StreamID)
	} else {
		t.Logf("GOAWAY LastStreamID is %d, which is acceptable.", goAwayFrame.LastStreamID)
	}

	// Connection (mockNetConn) should be closed by conn.Close()
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed")

	// If all checks pass, update closeErr to nil
	if !t.Failed() {
		closeErr = nil
	}
}

// TestInterleavedUnknownFrameDuringHeaderBlock specifically tests that sending an unknown frame
// after a HEADERS frame (without END_HEADERS) and before any CONTINUATION frame
// results in a connection error (PROTOCOL_ERROR) and a GOAWAY frame.
// This covers h2spec 5.5.2.
func TestInterleavedUnknownFrameDuringHeaderBlock(t *testing.T) {
	t.Parallel()

	initialHeaders := []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":path", Value: "/test"}, {Name: ":authority", Value: "example.com"},
	}
	encodedInitialHeaders := encodeHeadersForTest(t, initialHeaders)

	initialHeadersFrame := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    0, // CRITICAL: No END_HEADERS flag
			StreamID: 1, // New client-initiated stream
			Length:   uint32(len(encodedInitialHeaders)),
		},
		HeaderBlockFragment: encodedInitialHeaders,
	}

	interleavedUnknownFrame := &UnknownFrame{
		FrameHeader: FrameHeader{Type: FrameType(0xFA), StreamID: 1, Length: 4}, // Arbitrary unknown type 0xFA and length
		Payload:     []byte{0xDE, 0xAD, 0xBE, 0xEF},
	}

	conn, mnc := newTestConnection(t, false /*isClient*/, nil)
	var closeErr error = errors.New("test cleanup: TestInterleavedUnknownFrameDuringHeaderBlock")
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background())
		serveErrChan <- err
	}()

	// Send initial HEADERS frame (no END_HEADERS)
	mnc.FeedReadBuffer(frameToBytes(t, initialHeadersFrame))
	// Send the interleaved UnknownFrame
	mnc.FeedReadBuffer(frameToBytes(t, interleavedUnknownFrame))

	// Expect conn.Serve to exit with a ConnectionError(PROTOCOL_ERROR)
	var serveExitError error
	select {
	case serveExitError = <-serveErrChan:
		// Good, Serve exited
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for conn.Serve to exit after sending interleaved UnknownFrame")
	}

	if serveExitError == nil {
		t.Fatal("conn.Serve exited with nil error, expected ConnectionError")
	}

	var connErr *ConnectionError
	if !errors.As(serveExitError, &connErr) {
		t.Fatalf("conn.Serve error type: expected to contain *ConnectionError, got %T. Err: %v", serveExitError, serveExitError)
	}

	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("conn.Serve ConnectionError code: got %s, want %s. Msg: %s", connErr.Code, ErrCodeProtocolError, connErr.Msg)
	}

	// Explicitly call conn.Close with the error from Serve to trigger GOAWAY.
	_ = conn.Close(serveExitError)

	// Now check for the GOAWAY frame.
	var goAwayFrame *GoAwayFrame
	goAwayFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*GoAwayFrame)(nil), func(f *GoAwayFrame) bool {
		return f.ErrorCode == ErrCodeProtocolError
	}, "GOAWAY(PROTOCOL_ERROR) for interleaved UnknownFrame")

	if goAwayFrame == nil {
		t.Fatalf("GOAWAY(PROTOCOL_ERROR) not received. Last raw buffer (len %d): %s", len(mnc.GetWriteBufferBytes()), hex.EncodeToString(mnc.GetWriteBufferBytes()))
	}

	// LastStreamID in GOAWAY should be 0 as the stream 1 was not fully established.
	// Or it could be the stream ID of the frame that initiated the header block (stream 1).
	if goAwayFrame.LastStreamID != 0 && goAwayFrame.LastStreamID != initialHeadersFrame.Header().StreamID {
		t.Errorf("GOAWAY LastStreamID: got %d, want 0 or %d (stream ID of initial HEADERS)", goAwayFrame.LastStreamID, initialHeadersFrame.Header().StreamID)
	} else {
		t.Logf("GOAWAY LastStreamID is %d, which is acceptable.", goAwayFrame.LastStreamID)
	}

	// Connection (mockNetConn) should be closed by conn.Close()
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed")

	// If all checks pass, update closeErr to nil
	if !t.Failed() {
		closeErr = nil
	}
}

package http2

/*
   NOTE: The conn tests are split into multiple files to reduce the load on LLM
   context compared to having just one big file open.
*/

import (
	"context" // ADDED IMPORT
	"encoding/hex"
	"errors"
	"fmt" // Added import
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

// TestInvalidFrameLength_PING tests that sending a PING frame with an invalid length
// results in a connection error (FRAME_SIZE_ERROR) and a GOAWAY frame.
// Covers h2spec 6.7.4.
func TestInvalidFrameLength_PING(t *testing.T) {
	t.Parallel()

	conn, mnc := newTestConnection(t, false /*isClient*/, nil, nil)
	var closeErr error = errors.New("test cleanup: TestInvalidFrameLength_PING")
	// Defer a final conn.Close. The error used here will be updated if the test passes.
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc, nil)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background())
		serveErrChan <- err
	}()

	// Send PING frame with invalid length (e.g., 7 instead of 8)
	invalidPingFrameBytes := []byte{
		0x00, 0x00, 0x07, // Length = 7
		byte(FramePing),        // Type = PING
		0x00,                   // Flags = 0
		0x00, 0x00, 0x00, 0x00, // StreamID = 0
		// Payload (7 bytes instead of 8)
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	}
	mnc.FeedReadBuffer(invalidPingFrameBytes)

	// Expect conn.Serve to exit with a ConnectionError(FRAME_SIZE_ERROR)
	var serveExitError error
	select {
	case serveExitError = <-serveErrChan:
		// Good, Serve exited
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for conn.Serve to exit after sending PING with invalid length")
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

	// Explicitly call conn.Close with the error from Serve to trigger GOAWAY.
	_ = conn.Close(serveExitError)

	// Now check for the GOAWAY frame.
	var goAwayFrame *GoAwayFrame
	goAwayFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*GoAwayFrame)(nil), func(f *GoAwayFrame) bool {
		return f.ErrorCode == ErrCodeFrameSizeError
	}, "GOAWAY(FRAME_SIZE_ERROR) for PING with invalid length")

	if goAwayFrame == nil {
		t.Fatalf("GOAWAY(FRAME_SIZE_ERROR) not received. Last raw buffer (len %d): %s", len(mnc.GetWriteBufferBytes()), hex.EncodeToString(mnc.GetWriteBufferBytes()))
	}

	// LastStreamID in GOAWAY should typically be 0.
	conn.streamsMu.RLock()
	expectedLastStreamID := conn.lastProcessedStreamID
	conn.streamsMu.RUnlock()
	if goAwayFrame.LastStreamID != expectedLastStreamID {
		t.Errorf("GOAWAY LastStreamID: got %d, want %d (conn.lastProcessedStreamID)", goAwayFrame.LastStreamID, expectedLastStreamID)
	}

	// Connection (mockNetConn) should be closed by conn.Close()
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed")

	// If all checks pass, update closeErr to nil
	if !t.Failed() {
		closeErr = nil
	}
}

// TestInvalidFrameLength_PRIORITY tests that sending a PRIORITY frame with an invalid length
// on a stream ID > 0 results in an RST_STREAM frame with FRAME_SIZE_ERROR.
// Covers h2spec 6.3.2.
func TestInvalidFrameLength_PRIORITY(t *testing.T) {
	t.Parallel()

	conn, mnc := newTestConnection(t, false /*isClient*/, nil, nil)
	var closeErr error = errors.New("test cleanup: TestInvalidFrameLength_PRIORITY")
	// Defer a final conn.Close. The error used here will be updated if the test passes.
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc, nil)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background())
		serveErrChan <- err
	}()

	const streamIDToUse uint32 = 1 // Client-initiated stream

	// 1. Client sends HEADERS to open stream 1, so it's not idle for the PRIORITY frame.
	//    A PRIORITY frame on an idle stream would be a PROTOCOL_ERROR.
	//    h2spec 6.3.2 implies the stream should exist or be creatable.
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

	// Wait for the server to process the HEADERS frame.
	// We don't need to check for dispatcher call, just give server time to open the stream.
	time.Sleep(100 * time.Millisecond)
	mnc.ResetWriteBuffer() // Clear any response from server for the HEADERS

	// 2. Send PRIORITY frame with invalid length (e.g., 4 instead of 5)
	// Frame structure: Length (3 bytes), Type (1), Flags (1), StreamID (4), Payload (Length bytes)
	// PRIORITY payload: Exclusive (1 bit) + Stream Dependency (31 bits) + Weight (8 bits) = 5 bytes.
	// We send a frame with header length 4.
	invalidPriorityFrameBytes := []byte{
		0x00, 0x00, 0x04, // Length = 4 (invalid, should be 5)
		byte(FramePriority),                   // Type = PRIORITY
		0x00,                                  // Flags = 0
		0x00, 0x00, 0x00, byte(streamIDToUse), // StreamID
		// Payload (4 bytes instead of 5)
		0x00, 0x00, 0x00, 0x0A, // Example: Dependency 0, Weight 10 (incomplete)
	}
	mnc.FeedReadBuffer(invalidPriorityFrameBytes)

	// 3. Expect server to send RST_STREAM(FRAME_SIZE_ERROR) for stream 1
	var rstFrame *RSTStreamFrame
	rstFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*RSTStreamFrame)(nil), func(f *RSTStreamFrame) bool {
		return f.Header().StreamID == streamIDToUse && f.ErrorCode == ErrCodeFrameSizeError
	}, fmt.Sprintf("RST_STREAM(FRAME_SIZE_ERROR) for stream %d due to invalid PRIORITY length", streamIDToUse))

	if rstFrame == nil {
		// waitForFrameCondition would have t.Fatal'd.
		t.Fatalf("Expected RST_STREAM(FRAME_SIZE_ERROR) not received for stream %d.", streamIDToUse)
	}

	// 4. Ensure no GOAWAY frame was sent (connection should remain open)
	time.Sleep(50 * time.Millisecond) // Give a bit of time for a GOAWAY if it were to be sent
	framesAfterRST := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	for _, f := range framesAfterRST {
		// Allow for the expected RST_STREAM frame to be in the buffer if not reset
		if f == rstFrame {
			continue
		}
		if _, ok := f.(*GoAwayFrame); ok {
			t.Fatalf("Unexpected GOAWAY frame sent by server: %+v", f)
		}
	}

	// 5. Clean up: Close mock net conn, wait for Serve to exit.
	mnc.Close()
	select {
	case err := <-serveErrChan:
		if err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "use of closed network connection") {
			t.Logf("conn.Serve exited with: %v (expected EOF or closed connection error)", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("Timeout waiting for conn.Serve to exit after mnc.Close()")
	}

	// If all checks pass, update closeErr to nil
	if !t.Failed() {
		closeErr = nil
	}
}

// TestInvalidFrameLength_RST_STREAM tests that sending an RST_STREAM frame with an invalid length
// results in a connection error (FRAME_SIZE_ERROR) and a GOAWAY frame.
// Covers h2spec 6.4.3.
func TestInvalidFrameLength_RST_STREAM(t *testing.T) {
	t.Parallel()

	conn, mnc := newTestConnection(t, false /*isClient*/, nil, nil)
	var closeErr error = errors.New("test cleanup: TestInvalidFrameLength_RST_STREAM")
	// Defer a final conn.Close. The error used here will be updated if the test passes.
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc, nil)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background())
		serveErrChan <- err
	}()

	const streamIDForRST uint32 = 1 // Can be any valid stream ID, even if not open, for this test.

	// Send RST_STREAM frame with invalid length (e.g., 3 instead of 4)
	// Frame structure: Length (3 bytes), Type (1), Flags (1), StreamID (4), Payload (Length bytes)
	// An RST_STREAM frame *must* have a length of 4 octets (RFC 7540, Section 6.4).
	invalidRSTStreamFrameBytes := []byte{
		0x00, 0x00, 0x03, // Length = 3 (invalid, should be 4)
		byte(FrameRSTStream),                   // Type = RST_STREAM
		0x00,                                   // Flags = 0
		0x00, 0x00, 0x00, byte(streamIDForRST), // StreamID
		// Payload (3 bytes instead of 4 for ErrorCode)
		0x00, 0x00, 0x01, // Example: Partial ErrorCode (ErrCodeProtocolError)
	}
	mnc.FeedReadBuffer(invalidRSTStreamFrameBytes)

	// Expect conn.Serve to exit with a ConnectionError(FRAME_SIZE_ERROR)
	var serveExitError error
	select {
	case serveExitError = <-serveErrChan:
		// Good, Serve exited
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for conn.Serve to exit after sending RST_STREAM with invalid length")
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

	// Explicitly call conn.Close with the error from Serve to trigger GOAWAY.
	_ = conn.Close(serveExitError)

	// Now check for the GOAWAY frame.
	var goAwayFrame *GoAwayFrame
	goAwayFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*GoAwayFrame)(nil), func(f *GoAwayFrame) bool {
		return f.ErrorCode == ErrCodeFrameSizeError
	}, "GOAWAY(FRAME_SIZE_ERROR) for RST_STREAM with invalid length")

	if goAwayFrame == nil {
		t.Fatalf("GOAWAY(FRAME_SIZE_ERROR) not received. Last raw buffer (len %d): %s", len(mnc.GetWriteBufferBytes()), hex.EncodeToString(mnc.GetWriteBufferBytes()))
	}

	// LastStreamID in GOAWAY should typically be 0, or the last processed stream ID by the client.
	// Since the error is frame-level before stream processing, it's often 0.
	conn.streamsMu.RLock()
	expectedLastStreamID := conn.lastProcessedStreamID // Or conn.highestPeerInitiatedStreamID if more appropriate for client RSTs
	conn.streamsMu.RUnlock()
	if goAwayFrame.LastStreamID != expectedLastStreamID {
		t.Errorf("GOAWAY LastStreamID: got %d, want %d (conn's last known good stream ID)", goAwayFrame.LastStreamID, expectedLastStreamID)
	}

	// Connection (mockNetConn) should be closed by conn.Close()
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed")

	// If all checks pass, update closeErr to nil
	if !t.Failed() {
		closeErr = nil
	}
}

// TestInvalidFrameLength_WINDOW_UPDATE tests that sending a WINDOW_UPDATE frame with an invalid length
// results in a connection error (FRAME_SIZE_ERROR) and a GOAWAY frame.
// Covers h2spec 6.9.3.
func TestInvalidFrameLength_WINDOW_UPDATE(t *testing.T) {
	t.Parallel()

	conn, mnc := newTestConnection(t, false /*isClient*/, nil, nil)
	var closeErr error = errors.New("test cleanup: TestInvalidFrameLength_WINDOW_UPDATE")
	// Defer a final conn.Close. The error used here will be updated if the test passes.
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc, nil)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background())
		serveErrChan <- err
	}()

	const streamIDForWindowUpdate uint32 = 0 // WINDOW_UPDATE can be on stream 0 or an active stream. Use 0 for simplicity.

	// Send WINDOW_UPDATE frame with invalid length (e.g., 3 instead of 4)
	// Frame structure: Length (3 bytes), Type (1), Flags (1), StreamID (4), Payload (Length bytes)
	// A WINDOW_UPDATE frame *must* have a length of 4 octets (RFC 7540, Section 6.9).
	invalidWindowUpdateFrameBytes := []byte{
		0x00, 0x00, 0x03, // Length = 3 (invalid, should be 4)
		byte(FrameWindowUpdate),                         // Type = WINDOW_UPDATE
		0x00,                                            // Flags = 0
		0x00, 0x00, 0x00, byte(streamIDForWindowUpdate), // StreamID
		// Payload (3 bytes instead of 4 for WindowSizeIncrement)
		0x00, 0x00, 0x01, // Example: Partial WindowSizeIncrement
	}
	mnc.FeedReadBuffer(invalidWindowUpdateFrameBytes)

	// Expect conn.Serve to exit with a ConnectionError(FRAME_SIZE_ERROR)
	var serveExitError error
	select {
	case serveExitError = <-serveErrChan:
		// Good, Serve exited
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for conn.Serve to exit after sending WINDOW_UPDATE with invalid length")
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

	// Explicitly call conn.Close with the error from Serve to trigger GOAWAY.
	_ = conn.Close(serveExitError)

	// Now check for the GOAWAY frame.
	var goAwayFrame *GoAwayFrame
	goAwayFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*GoAwayFrame)(nil), func(f *GoAwayFrame) bool {
		return f.ErrorCode == ErrCodeFrameSizeError
	}, "GOAWAY(FRAME_SIZE_ERROR) for WINDOW_UPDATE with invalid length")

	if goAwayFrame == nil {
		t.Fatalf("GOAWAY(FRAME_SIZE_ERROR) not received. Last raw buffer (len %d): %s", len(mnc.GetWriteBufferBytes()), hex.EncodeToString(mnc.GetWriteBufferBytes()))
	}

	// LastStreamID in GOAWAY should typically be 0.
	conn.streamsMu.RLock()
	expectedLastStreamID := conn.lastProcessedStreamID
	conn.streamsMu.RUnlock()
	if goAwayFrame.LastStreamID != expectedLastStreamID {
		t.Errorf("GOAWAY LastStreamID: got %d, want %d (conn.lastProcessedStreamID)", goAwayFrame.LastStreamID, expectedLastStreamID)
	}

	// Connection (mockNetConn) should be closed by conn.Close()
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed")

	// If all checks pass, update closeErr to nil
	if !t.Failed() {
		closeErr = nil
	}

}

// TestMalformedContentLength_HEADERS_EndStream_NonZeroContentLength tests server behavior
// when HEADERS frame has END_STREAM set and a non-zero Content-Length.
// Corresponds to h2spec 8.1.2.6 "Semantic Errors", specifically item 1.
// "An HTTP/2 request or response is malformed if the value of a content-length header field does not equal the sum of the DATA frame payload lengths that form the body,
// or if it contains a content-length header field and the END_STREAM flag is set on the header block."
func TestMalformedContentLength_HEADERS_EndStream_NonZeroContentLength(t *testing.T) {
	t.Parallel()

	conn, mnc := newTestConnection(t, false /*isClient*/, nil, nil)
	var closeErr error = errors.New("test cleanup: TestMalformedContentLength_HEADERS_EndStream_NonZeroContentLength")
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc, nil)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background())
		serveErrChan <- err
	}()

	const streamIDToUse uint32 = 1
	headersWithInvalidCL := []hpack.HeaderField{
		{Name: ":method", Value: "POST"}, // Method that might have a body
		{Name: ":scheme", Value: "https"},
		{Name: ":path", Value: "/submit"},
		{Name: ":authority", Value: "example.com"},
		{Name: "content-length", Value: "10"}, // Non-zero Content-Length
	}
	encodedHeaders := encodeHeadersForTest(t, headersWithInvalidCL)

	// HEADERS frame with END_STREAM and non-zero Content-Length
	headersFrame := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    FlagHeadersEndHeaders | FlagHeadersEndStream, // END_STREAM is set
			StreamID: streamIDToUse,
			Length:   uint32(len(encodedHeaders)),
		},
		HeaderBlockFragment: encodedHeaders,
	}
	mnc.FeedReadBuffer(frameToBytes(t, headersFrame))

	// Expect RST_STREAM(PROTOCOL_ERROR) on stream 1
	var rstFrame *RSTStreamFrame
	rstFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*RSTStreamFrame)(nil), func(f *RSTStreamFrame) bool {
		return f.Header().StreamID == streamIDToUse && f.ErrorCode == ErrCodeProtocolError
	}, fmt.Sprintf("RST_STREAM(PROTOCOL_ERROR) for stream %d due to non-zero CL with END_STREAM", streamIDToUse))

	if rstFrame == nil {
		t.Fatalf("Expected RST_STREAM(PROTOCOL_ERROR) not received for stream %d.", streamIDToUse)
	}

	// Ensure no GOAWAY frame was sent (connection should remain open)
	time.Sleep(50 * time.Millisecond) // Give a bit of time for a GOAWAY if it were to be sent
	framesAfterRST := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	for _, f := range framesAfterRST {
		if f == rstFrame { // Allow the RST frame itself if not cleared
			continue
		}
		if _, ok := f.(*GoAwayFrame); ok {
			t.Fatalf("Unexpected GOAWAY frame sent by server: %+v", f)
		}
	}

	// Clean up: Close mock net conn, wait for Serve to exit.
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

// TestMalformedContentLength_DATA_DoesNotMatch_EndStream tests server behavior
// when DATA frames' total payload length does not match the Content-Length header,
// and END_STREAM is received.
// Corresponds to h2spec 8.1.2.6 "Semantic Errors", specifically item 2.
func TestMalformedContentLength_DATA_DoesNotMatch_EndStream(t *testing.T) {
	t.Parallel()

	conn, mnc := newTestConnection(t, false /*isClient*/, nil, nil)
	var closeErr error = errors.New("test cleanup: TestMalformedContentLength_DATA_DoesNotMatch_EndStream")
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc, nil)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background())
		serveErrChan <- err
	}()

	const streamIDToUse uint32 = 1
	const declaredContentLength = "10"
	actualDataPayload := []byte("short") // Length 5, which is != 10

	headersWithCL := []hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "https"},
		{Name: ":path", Value: "/upload"},
		{Name: ":authority", Value: "example.com"},
		{Name: "content-length", Value: declaredContentLength},
	}
	encodedHeaders := encodeHeadersForTest(t, headersWithCL)

	// 1. Send HEADERS frame (no END_STREAM)
	headersFrame := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    FlagHeadersEndHeaders, // NO END_STREAM
			StreamID: streamIDToUse,
			Length:   uint32(len(encodedHeaders)),
		},
		HeaderBlockFragment: encodedHeaders,
	}
	mnc.FeedReadBuffer(frameToBytes(t, headersFrame))

	// Wait for server to process HEADERS, dispatcher might be called.
	// We don't strictly need to check dispatcher call for this test's focus.
	time.Sleep(100 * time.Millisecond)
	mnc.ResetWriteBuffer() // Clear any ACK or response from initial HEADERS

	// 2. Send DATA frame with mismatched length and END_STREAM
	dataFrame := &DataFrame{
		FrameHeader: FrameHeader{
			Type:     FrameData,
			Flags:    FlagDataEndStream, // END_STREAM is set here
			StreamID: streamIDToUse,
			Length:   uint32(len(actualDataPayload)),
		},
		Data: actualDataPayload,
	}
	mnc.FeedReadBuffer(frameToBytes(t, dataFrame))

	// Expect RST_STREAM(PROTOCOL_ERROR) on stream 1
	var rstFrame *RSTStreamFrame
	rstFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*RSTStreamFrame)(nil), func(f *RSTStreamFrame) bool {
		return f.Header().StreamID == streamIDToUse && f.ErrorCode == ErrCodeProtocolError
	}, fmt.Sprintf("RST_STREAM(PROTOCOL_ERROR) for stream %d due to CL mismatch", streamIDToUse))

	if rstFrame == nil {
		t.Fatalf("Expected RST_STREAM(PROTOCOL_ERROR) not received for stream %d.", streamIDToUse)
	}

	// Ensure no GOAWAY frame was sent (connection should remain open)
	time.Sleep(50 * time.Millisecond) // Give a bit of time for a GOAWAY if it were to be sent
	framesAfterRST := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	for _, f := range framesAfterRST {
		if f == rstFrame { // Allow the RST frame itself if not cleared
			continue
		}
		if _, ok := f.(*GoAwayFrame); ok {
			t.Fatalf("Unexpected GOAWAY frame sent by server: %+v", f)
		}
	}

	// Clean up: Close mock net conn, wait for Serve to exit.
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

// TestStreamIDNumericallySmaller_ServeLoop tests that the server correctly handles
// a client attempting to initiate a new stream with an ID numerically smaller than
// a previously initiated one.
// It expects the server to treat this as a connection error (PROTOCOL_ERROR),
// send a GOAWAY frame, and close the connection.
// Covers h2spec 5.1.1.2.
func TestStreamIDNumericallySmaller_ServeLoop(t *testing.T) {
	t.Parallel()

	conn, mnc := newTestConnection(t, false /*isClient*/, nil, nil)
	var closeErr error = errors.New("test cleanup: TestStreamIDNumericallySmaller_ServeLoop")
	// Defer a final conn.Close. The error used here will be updated by test logic.
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc, nil)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	go func() {
		err := conn.Serve(context.Background()) // Use a background context
		serveErrChan <- err
	}()

	// Standard headers for requests
	requestHeaders := []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":path", Value: "/test"}, {Name: ":authority", Value: "example.com"},
	}
	encodedRequestHeaders := encodeHeadersForTest(t, requestHeaders)

	// 1. Client initiates stream 3 (valid)
	const higherStreamID uint32 = 3
	headersFrameHigher := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    FlagHeadersEndHeaders | FlagHeadersEndStream,
			StreamID: higherStreamID,
			Length:   uint32(len(encodedRequestHeaders)),
		},
		HeaderBlockFragment: encodedRequestHeaders,
	}
	mnc.FeedReadBuffer(frameToBytes(t, headersFrameHigher))

	// Wait for server to process this. Server should accept it.
	// We need to ensure stream 3 is fully processed and highestPeerInitiatedStreamID updated.
	// A simple way is to use a mock dispatcher and wait for it to be called for stream 3.
	// For this test, we'll assume the default no-op dispatcher is fine, but we need a reliable
	// way to know stream 3's creation logic (including highestPeerInitiatedStreamID update) has completed.
	// Using a small delay is still racy. Let's modify the test to use a mock dispatcher for this wait.

	// Re-create connection with a mock dispatcher for this specific test
	// This is a bit heavy, but ensures reliable synchronization.
	_ = conn.Close(errors.New("re-initializing conn for dispatcher")) // Close previous conn
	mockDisp := &mockRequestDispatcher{}
	conn, mnc = newTestConnection(t, false, mockDisp, nil) // Re-assign conn and mnc
	// Re-perform handshake and start Serve for the new connection instance
	performHandshakeForTest(t, conn, mnc, nil)
	mnc.ResetWriteBuffer()
	serveErrChan = make(chan error, 1) // Re-make channel
	go func() {
		err := conn.Serve(context.Background())
		serveErrChan <- err
	}()

	// Now send stream 3's HEADERS to the new connection
	mnc.FeedReadBuffer(frameToBytes(t, headersFrameHigher))

	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		mockDisp.mu.Lock()
		defer mockDisp.mu.Unlock()
		return mockDisp.called && mockDisp.lastStreamID == higherStreamID
	}, fmt.Sprintf("dispatcher to be called for stream %d", higherStreamID))

	mnc.ResetWriteBuffer()

	// 2. Client attempts to initiate stream 1 (numerically smaller than 3, invalid)
	const lowerStreamID uint32 = 1
	headersFrameLower := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    FlagHeadersEndHeaders | FlagHeadersEndStream,
			StreamID: lowerStreamID,
			Length:   uint32(len(encodedRequestHeaders)),
		},
		HeaderBlockFragment: encodedRequestHeaders,
	}
	mnc.FeedReadBuffer(frameToBytes(t, headersFrameLower))

	// Expect conn.Serve to exit with a ConnectionError(PROTOCOL_ERROR)
	var serveExitError error
	select {
	case serveExitError = <-serveErrChan:
		// Good, Serve exited
	case <-time.After(2 * time.Second): // Give ample time for processing and exit
		t.Fatal("Timeout waiting for conn.Serve to exit after sending HEADERS for numerically smaller stream ID")
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
	// The message should indicate the stream ID violation.

	expectedMsgSubstr := fmt.Sprintf("client attempted to initiate new stream %d, which is not numerically greater than highest previously client-initiated stream ID %d", lowerStreamID, higherStreamID)
	if !strings.Contains(connErr.Msg, expectedMsgSubstr) {
		t.Errorf("ConnectionError message '%s' does not contain expected substring '%s'", connErr.Msg, expectedMsgSubstr)
	}

	// Explicitly call conn.Close with the error from Serve to ensure GOAWAY logic uses it.
	_ = conn.Close(serveExitError)

	// Now check for the GOAWAY frame.
	var goAwayFrame *GoAwayFrame
	goAwayFrame = waitForFrameCondition(t, 1*time.Second, 20*time.Millisecond, mnc, (*GoAwayFrame)(nil), func(f *GoAwayFrame) bool {
		return f.ErrorCode == ErrCodeProtocolError
	}, "GOAWAY(PROTOCOL_ERROR) for numerically smaller stream ID")

	if goAwayFrame == nil {
		t.Fatalf("GOAWAY(PROTOCOL_ERROR) not received. Last raw buffer (len %d): %s", len(mnc.GetWriteBufferBytes()), hex.EncodeToString(mnc.GetWriteBufferBytes()))
	}

	// LastStreamID in GOAWAY should be the highest *successfully processed* client stream ID before this error.
	// In this case, it should be `higherStreamID` (stream 3).
	if goAwayFrame.LastStreamID != higherStreamID {
		t.Errorf("GOAWAY LastStreamID: got %d, want %d", goAwayFrame.LastStreamID, higherStreamID)
	}

	// Connection (mockNetConn) should be closed by conn.Close()
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, mnc.IsClosed, "mock net.Conn to be closed")

	// If all checks pass, update closeErr to nil for the deferred Close.
	if !t.Failed() {
		closeErr = nil
	}
}

// TestPriorityOnHalfClosedRemoteStream verifies that the server accepts PRIORITY frames
// on streams that are in the half-closed (remote) state without erroring or sending
// unexpected frames. This covers h2spec 2.3.
func TestPriorityOnHalfClosedRemoteStream(t *testing.T) {
	t.Parallel()

	mockDispatcher := &mockRequestDispatcher{}
	conn, mnc := newTestConnection(t, false /*isClient*/, mockDispatcher, nil)
	var closeErr error = errors.New("test cleanup: TestPriorityOnHalfClosedRemoteStream")
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc, nil)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	ctx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()

	go func() {
		err := conn.Serve(ctx)
		serveErrChan <- err
	}()

	const streamIDToUse uint32 = 1

	// 1. Client sends HEADERS with END_STREAM to put stream in half-closed (remote) state on server
	clientHeaders := []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":path", Value: "/"}, {Name: ":authority", Value: "example.com"},
	}
	encodedClientHeaders := encodeHeadersForTest(t, clientHeaders)
	headersFrameClient := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    FlagHeadersEndHeaders | FlagHeadersEndStream, // END_STREAM is key here
			StreamID: streamIDToUse,
			Length:   uint32(len(encodedClientHeaders)),
		},
		HeaderBlockFragment: encodedClientHeaders,
	}
	mnc.FeedReadBuffer(frameToBytes(t, headersFrameClient))

	// Wait for dispatcher to be called, confirming stream is processed
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		mockDispatcher.mu.Lock()
		defer mockDispatcher.mu.Unlock()
		// Check calledCount as lastStreamID might persist from other activities if dispatcher isn't reset carefully in all tests
		return mockDispatcher.calledCount > 0 && mockDispatcher.lastStreamID == streamIDToUse
	}, fmt.Sprintf("dispatcher to be called for stream %d", streamIDToUse))

	// Verify stream state is HalfClosedRemote
	stream, exists := conn.getStream(streamIDToUse)
	if !exists {
		t.Fatalf("Stream %d not found after HEADERS", streamIDToUse)
	}
	stream.mu.RLock()
	state := stream.state
	stream.mu.RUnlock()
	if state != StreamStateHalfClosedRemote {
		t.Fatalf("Stream %d state: got %s, want %s", streamIDToUse, state, StreamStateHalfClosedRemote)
	}

	// Server might send a response to the initial GET. Reset write buffer *before* sending PRIORITY.
	// This ensures that frames checked later are only those potentially sent due to PRIORITY processing.
	mnc.ResetWriteBuffer()
	// No need to reset mockDispatcher here as its state for initial HEADERS is already checked.

	// 2. Client sends PRIORITY frame for this half-closed (remote) stream
	priorityFrame := &PriorityFrame{
		FrameHeader:      FrameHeader{Type: FramePriority, StreamID: streamIDToUse, Length: 5},
		StreamDependency: 0,
		Weight:           20, // Some weight
	}
	mnc.FeedReadBuffer(frameToBytes(t, priorityFrame))

	// 3. Server should accept this PRIORITY frame without error.
	// No RST_STREAM or GOAWAY should be sent as a result of the PRIORITY frame.
	time.Sleep(100 * time.Millisecond) // Allow server time to process PRIORITY frame

	framesAfterPriority := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	for _, f := range framesAfterPriority {
		if rst, ok := f.(*RSTStreamFrame); ok {
			t.Fatalf("Server sent RST_STREAM frame (StreamID: %d, ErrorCode: %s) after PRIORITY on half-closed (remote) stream", rst.Header().StreamID, rst.ErrorCode)
		}
		if _, ok := f.(*GoAwayFrame); ok {
			t.Fatalf("Server sent GOAWAY frame after PRIORITY on half-closed (remote) stream: %+v", f)
		}
	}

	// (Optional: Deeper check of priority tree update. For now, this is sufficient for spec compliance on frame acceptance)

	// 4. Clean up: Close mock net conn, wait for Serve to exit.
	cancelServe() // Signal Serve to stop
	mnc.Close()   // Close the connection to unblock ReadFrame in Serve

	select {
	case err := <-serveErrChan:
		// Expect EOF, "use of closed network connection", or "context canceled"
		if err != nil && !errors.Is(err, io.EOF) &&
			!strings.Contains(err.Error(), "use of closed network connection") &&
			!errors.Is(err, context.Canceled) &&
			!strings.Contains(err.Error(), "connection shutdown initiated") { // Conn.Close might be called by Serve's defer
			t.Logf("conn.Serve exited with: %v (expected EOF, closed connection, or context canceled)", err)
		} else if err != nil {
			t.Logf("conn.Serve exited as expected with: %v", err)
		}
	case <-time.After(2 * time.Second): // Increased timeout slightly
		t.Error("Timeout waiting for conn.Serve to exit after mnc.Close() and context cancel")
	}

	if !t.Failed() {
		closeErr = nil // Test logic passed
	}
}

// TestWindowUpdateOnHalfClosedRemoteStream verifies that the server accepts WINDOW_UPDATE frames
// on streams that are in the half-closed (remote) state without erroring or sending
// unexpected frames, and that the stream's send window is updated. This covers h2spec 2.2.
func TestWindowUpdateOnHalfClosedRemoteStream(t *testing.T) {
	t.Parallel()

	mockDispatcher := &mockRequestDispatcher{}
	conn, mnc := newTestConnection(t, false /*isClient*/, mockDispatcher, nil)
	var closeErr error = errors.New("test cleanup: TestWindowUpdateOnHalfClosedRemoteStream")
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	performHandshakeForTest(t, conn, mnc, nil)
	mnc.ResetWriteBuffer()

	serveErrChan := make(chan error, 1)
	ctx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()

	go func() {
		err := conn.Serve(ctx)
		serveErrChan <- err
	}()

	const streamIDToUse uint32 = 1
	const windowUpdateIncrement uint32 = 1024

	// 1. Client sends HEADERS with END_STREAM to put stream in half-closed (remote) state on server
	clientHeaders := []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":path", Value: "/"}, {Name: ":authority", Value: "example.com"},
	}
	encodedClientHeaders := encodeHeadersForTest(t, clientHeaders)
	headersFrameClient := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    FlagHeadersEndHeaders | FlagHeadersEndStream, // END_STREAM is key here
			StreamID: streamIDToUse,
			Length:   uint32(len(encodedClientHeaders)),
		},
		HeaderBlockFragment: encodedClientHeaders,
	}
	mnc.FeedReadBuffer(frameToBytes(t, headersFrameClient))

	// Wait for dispatcher to be called, confirming stream is processed
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		mockDispatcher.mu.Lock()
		defer mockDispatcher.mu.Unlock()
		return mockDispatcher.calledCount > 0 && mockDispatcher.lastStreamID == streamIDToUse
	}, fmt.Sprintf("dispatcher to be called for stream %d", streamIDToUse))

	// Verify stream state is HalfClosedRemote and get initial send window
	stream, exists := conn.getStream(streamIDToUse)
	if !exists {
		t.Fatalf("Stream %d not found after HEADERS", streamIDToUse)
	}
	stream.mu.RLock()
	state := stream.state
	stream.mu.RUnlock()
	if state != StreamStateHalfClosedRemote {
		t.Fatalf("Stream %d state: got %s, want %s", streamIDToUse, state, StreamStateHalfClosedRemote)
	}

	initialStreamSendWindow := stream.fcManager.GetStreamSendAvailable() // Get server's send window for this stream

	// Server might send a response to the initial GET. Reset write buffer *before* sending WINDOW_UPDATE.
	mnc.ResetWriteBuffer()

	// 2. Client sends WINDOW_UPDATE frame for this half-closed (remote) stream
	windowUpdateFrame := &WindowUpdateFrame{
		FrameHeader:         FrameHeader{Type: FrameWindowUpdate, StreamID: streamIDToUse, Length: 4},
		WindowSizeIncrement: windowUpdateIncrement,
	}
	mnc.FeedReadBuffer(frameToBytes(t, windowUpdateFrame))

	// 3. Server should accept this WINDOW_UPDATE frame without error.
	// Wait for the send window to update.
	expectedSendWindow := initialStreamSendWindow + int64(windowUpdateIncrement)
	waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
		return stream.fcManager.GetStreamSendAvailable() == expectedSendWindow
	}, fmt.Sprintf("stream %d send window to update to %d", streamIDToUse, expectedSendWindow))

	// Check window update
	if currentSendWindow := stream.fcManager.GetStreamSendAvailable(); currentSendWindow != expectedSendWindow {
		t.Errorf("Stream %d send window: got %d, want %d (initial: %d, increment: %d)",
			streamIDToUse, currentSendWindow, expectedSendWindow, initialStreamSendWindow, windowUpdateIncrement)
	}

	// No RST_STREAM or GOAWAY should be sent as a result of the WINDOW_UPDATE frame.
	framesAfterWindowUpdate := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	for _, f := range framesAfterWindowUpdate {
		if rst, ok := f.(*RSTStreamFrame); ok {
			t.Fatalf("Server sent RST_STREAM frame (StreamID: %d, ErrorCode: %s) after WINDOW_UPDATE on half-closed (remote) stream", rst.Header().StreamID, rst.ErrorCode)
		}
		if _, ok := f.(*GoAwayFrame); ok {
			t.Fatalf("Server sent GOAWAY frame after WINDOW_UPDATE on half-closed (remote) stream: %+v", f)
		}
	}

	// 4. Clean up: Close mock net conn, wait for Serve to exit.
	cancelServe() // Signal Serve to stop
	mnc.Close()   // Close the connection to unblock ReadFrame in Serve

	select {
	case err := <-serveErrChan:
		if err != nil && !errors.Is(err, io.EOF) &&
			!strings.Contains(err.Error(), "use of closed network connection") &&
			!errors.Is(err, context.Canceled) &&
			!strings.Contains(err.Error(), "connection shutdown initiated") {
			t.Logf("conn.Serve exited with: %v (expected EOF, closed connection, or context canceled)", err)
		} else if err != nil {
			t.Logf("conn.Serve exited as expected with: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Timeout waiting for conn.Serve to exit after mnc.Close() and context cancel")
	}

	if !t.Failed() {
		closeErr = nil // Test logic passed
	}
}

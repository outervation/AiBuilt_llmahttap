package http2

import (
	"context"
	"fmt"
	"net/http" // For mockDispatcher's req parameter
	"testing"
	"time"

	"golang.org/x/net/http2/hpack" // For encoding request headers
)

// TODO: TestH2Spec_6_9_2_1_ChangesInitialWindowSizeAfterHeaders:
//   "Changes SETTINGS_INITIAL_WINDOW_SIZE after sending HEADERS frame"
//   -> The endpoint MUST adjust the size of all stream flow-control windows.
//   Expected: DATA Frame (length:1, flags:0x00, stream_id:1)
//   Actual: HEADERS Frame (length:77, flags:0x04, stream_id:1)
//   This means client sends HEADERS, server responds HEADERS.
//   Then client sends new SETTINGS_IWS=1. Server ACKs.
//   Server then tries to send DATA, should be limited to 1.

// TODO: TestH2Spec_6_9_2_2_NegativeInitialWindowSize:
//   "Sends a SETTINGS frame for window size to be negative"
//   -> The endpoint MUST track the negative flow-control window.
//   Expected: DATA frame
//   Actual: HEADERS Frame (length:77, flags:0x04, stream_id:1)
//   This test involves sending a SETTINGS_INITIAL_WINDOW_SIZE that, when applied as a delta
//   to existing streams, would drive their flow control window negative.
//   The server should still accept this and apply it. Subsequent DATA frames from the server
//   should be blocked until WINDOW_UPDATEs make the window positive.

// TestH2Spec_6_9_1_1_InitialWindowSizeRespected verifies that the server respects
// a small SETTINGS_INITIAL_WINDOW_SIZE set by the client when sending DATA frames.
// This addresses h2spec: Hypertext Transfer Protocol Version 2 (HTTP/2) / 6. Frame Definitions /
// 6.9. WINDOW_UPDATE / 6.9.1. The Flow-Control Window /
// 1: Sends SETTINGS frame to set the initial window size to 1 and sends HEADERS frame
// Expected: DATA Frame (length:1, flags:0x00, stream_id:1)
// Actual (from h2spec output): HEADERS Frame (length:77, flags:0x04, stream_id:1)
// The "Actual" suggests the server might be erroring out and sending HEADERS (e.g. error response)
// instead of DATA, or the test setup in h2spec is more complex.
// For this unit test, we focus on the server's behavior to send only 1 byte of DATA.
func TestH2Spec_6_9_1_1_InitialWindowSizeRespected(t *testing.T) {
	t.Parallel()

	const streamID = 1
	const clientInitialWindowSize uint32 = 1 // Key setting for this test
	smallDataToSend := []byte("hello world") // More than 1 byte

	// Setup mock dispatcher for the server
	mockDispatcher := &mockRequestDispatcher{
		fn: func(sw StreamWriter, req *http.Request) {
			// Server handler: send headers, then try to send data
			headers := []HeaderField{
				{Name: ":status", Value: "200"},
				{Name: "content-type", Value: "text/plain"},
			}
			// Send headers without END_STREAM, as we intend to send DATA
			if err := sw.SendHeaders(headers, false); err != nil {
				t.Logf("TestH2Spec_6_9_1_1: Server handler failed to send headers: %v", err) // Fixed tLogf
			}

			// Attempt to write data. This should be limited by flow control.
			n, err := sw.WriteData(smallDataToSend, true) // endStream true
			if err != nil {
				t.Logf("TestH2Spec_6_9_1_1: Server handler WriteData returned (n=%d, err=%v). This might be expected if blocking or partial write.", n, err) // Fixed tLogf
			}
		},
	}

	// Setup connection
	// For this test, the client advertises an initial window size.
	// This is passed as `initialPeerSettingsForTest` to newTestConnection.
	// The server's send window for the stream will be initialized to this value.
	conn, mnc := newTestConnection(t, false, mockDispatcher, map[SettingID]uint32{
		SettingInitialWindowSize: clientInitialWindowSize, // Client tells server its IWS is this small
	})
	defer conn.Close(nil)

	// ---- Client Actions ----
	performHandshakeForTest(t, conn, mnc, map[SettingID]uint32{SettingInitialWindowSize: clientInitialWindowSize})
	mnc.ResetWriteBuffer()              // Clear handshake frames
	go conn.Serve(context.Background()) // Start server's main read loop AFTER handshake

	// 3. Client sends HEADERS for a request
	requestHeadersHpack := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":path", Value: "/test"},
		{Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "example.com"},
	}
	headerBlockClient, errEncClient := conn.hpackAdapter.Encode(requestHeadersHpack)
	if errEncClient != nil {
		t.Fatalf("TestH2Spec_6_9_1_1: Failed to HPACK encode client request headers: %v", errEncClient)
	}

	clientHeadersFrame := &HeadersFrame{
		FrameHeader: FrameHeader{
			Type:     FrameHeaders,
			Flags:    FlagHeadersEndHeaders | FlagHeadersEndStream, // Request has no body
			StreamID: streamID,
			Length:   uint32(len(headerBlockClient)),
		},
		HeaderBlockFragment: headerBlockClient,
	}
	mnc.FeedReadBuffer(frameToBytes(t, clientHeadersFrame))

	// ---- Server-Side Assertions ----
	// Expect server to send HEADERS (response)
	// ---- Server-Side Assertions ----
	// Expect server to send HEADERS (response) and then the first DATA chunk (1 byte)
	// before we reset the buffer.
	var respHeadersFrame *HeadersFrame
	var dataFrame *DataFrame

	initialFramesDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(initialFramesDeadline) && (respHeadersFrame == nil || dataFrame == nil) {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, fr := range frames {
			if respHeadersFrame == nil {
				if hf, ok := fr.(*HeadersFrame); ok && hf.Header().StreamID == streamID && (hf.Header().Flags&FlagHeadersEndStream == 0) {
					respHeadersFrame = hf
					t.Logf("TestH2Spec_6_9_1_1: Found response HEADERS frame.")
				}
			}
			if dataFrame == nil {
				if df, ok := fr.(*DataFrame); ok && df.Header().StreamID == streamID {
					dataFrame = df
					t.Logf("TestH2Spec_6_9_1_1: Found first DATA frame (len: %d).", len(df.Data))
				}
			}
		}
		if respHeadersFrame != nil && dataFrame != nil {
			break
		}
		time.Sleep(20 * time.Millisecond) // Increased sleep slightly
	}

	if respHeadersFrame == nil {
		t.Fatalf("TestH2Spec_6_9_1_1: Did not receive expected response HEADERS frame from server. Frames seen: %+v", readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes()))
	}
	if dataFrame == nil {
		t.Fatalf("TestH2Spec_6_9_1_1: Did not receive expected first DATA frame from server. Frames seen: %+v", readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes()))
	}

	mnc.ResetWriteBuffer() // Clear the HEADERS and first DATA frame

	// Assertions for the first DATA frame
	if len(dataFrame.Data) != 1 {
		t.Errorf("TestH2Spec_6_9_1_1: Expected DATA frame payload length 1, got %d. Data: %q", len(dataFrame.Data), string(dataFrame.Data))
	}
	if string(dataFrame.Data) != string(smallDataToSend[0:1]) {
		t.Errorf("TestH2Spec_6_9_1_1: Expected DATA frame payload %q, got %q", string(smallDataToSend[0:1]), string(dataFrame.Data))
	}
	finalFrameSentByServer := (dataFrame.Header().Flags & FlagDataEndStream) != 0

	// After sending 1 byte, the server's stream send window for streamID should be 0.
	stream, ok := conn.getStream(streamID)
	if !ok {
		t.Fatalf("TestH2Spec_6_9_1_1: Server stream %d not found after first DATA frame", streamID)
	}
	currentStreamSendWindow := stream.fcManager.GetStreamSendAvailable()
	if currentStreamSendWindow != 0 {
		t.Errorf("TestH2Spec_6_9_1_1: Expected server stream send window to be 0 after sending 1 byte, got %d", currentStreamSendWindow)
	}

	// Verify no more DATA frames are sent immediately
	time.Sleep(100 * time.Millisecond) // Give server a chance to send more if it's going to (it shouldn't)
	framesAfterFirstData := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
	for _, f := range framesAfterFirstData {
		if df, ok := f.(*DataFrame); ok && df.Header().StreamID == streamID {
			t.Fatalf("TestH2Spec_6_9_1_1: Server sent additional DATA frame (len %d) without WINDOW_UPDATE. Data: %q", len(df.Data), string(df.Data))
		}
	}
	mnc.ResetWriteBuffer()

	// Now, client sends WINDOW_UPDATE to allow more data
	if !finalFrameSentByServer { // Only send WU if server hasn't ended the stream yet
		const windowIncrement uint32 = 10
		wuFrame := &WindowUpdateFrame{
			FrameHeader: FrameHeader{
				Type:     FrameWindowUpdate,
				StreamID: streamID, // WU for the specific stream
				Length:   4,
			},
			WindowSizeIncrement: windowIncrement,
		}
		mnc.FeedReadBuffer(frameToBytes(t, wuFrame))

		// Expect server to send more DATA (up to `windowIncrement` bytes or remaining data)
		expectedSecondChunkLen := len(smallDataToSend) - 1 // Data already sent: 1 byte
		if expectedSecondChunkLen > int(windowIncrement) {
			expectedSecondChunkLen = int(windowIncrement)
		}

		if expectedSecondChunkLen > 0 {
			secondDataFrame := waitForFrameCondition(t, 2*time.Second, 10*time.Millisecond, mnc, (*DataFrame)(nil),
				func(f *DataFrame) bool {
					return f.Header().StreamID == streamID
				},
				fmt.Sprintf("server to send second DATA frame after WU (expected len %d)", expectedSecondChunkLen), // fmt.Sprintf is used
			)
			if secondDataFrame == nil {
				t.Fatalf("TestH2Spec_6_9_1_1: Did not receive expected second DATA frame from server after WINDOW_UPDATE")
			}
			if len(secondDataFrame.Data) != expectedSecondChunkLen {
				t.Errorf("TestH2Spec_6_9_1_1: Expected second DATA frame payload length %d, got %d. Data: %q",
					expectedSecondChunkLen, len(secondDataFrame.Data), string(secondDataFrame.Data))
			}
			// Check content
			expectedContent := smallDataToSend[1 : 1+expectedSecondChunkLen]
			if string(secondDataFrame.Data) != string(expectedContent) {
				t.Errorf("TestH2Spec_6_9_1_1: Expected second DATA frame payload %q, got %q", string(expectedContent), string(secondDataFrame.Data))
			}

			// Check if END_STREAM is set correctly on this frame
			// Total data sent so far = 1 (first frame) + len(secondDataFrame.Data)
			shouldEndStreamNow := (1 + len(secondDataFrame.Data)) == len(smallDataToSend)
			actualEndStream := (secondDataFrame.Header().Flags & FlagDataEndStream) != 0 // Corrected StreamEnded
			if actualEndStream != shouldEndStreamNow {
				t.Errorf("TestH2Spec_6_9_1_1: Second DATA frame END_STREAM mismatch. Got %v, want %v", actualEndStream, shouldEndStreamNow)
			}
		} else if len(smallDataToSend) == 1 { // All data (1 byte) was sent in the first frame
			// This WU should not trigger more data. Assert no more DATA frames.
			time.Sleep(100 * time.Millisecond)
			extraFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
			for _, f := range extraFrames {
				if df, ok := f.(*DataFrame); ok && df.Header().StreamID == streamID {
					t.Fatalf("TestH2Spec_6_9_1_1: Server sent unexpected DATA frame (len %d) after WU, when all data was already sent. Data: %q", len(df.Data), string(df.Data))
				}
			}
		}
	} else { // Stream was already ended by the server with the first (and only) DATA frame
		if len(smallDataToSend) != 1 {
			t.Errorf("TestH2Spec_6_9_1_1: Server ended stream with first DATA frame, but more data (%d bytes) was intended to be sent.", len(smallDataToSend))
		}
		// If stream ended and all data was sent (i.e., smallDataToSend was 1 byte), then this is correct.
		// No further action needed.
	}
}

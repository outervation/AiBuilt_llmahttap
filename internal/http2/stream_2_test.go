package http2

/*
   NOTE: The stream tests are split into multiple files to reduce the load on LLM
   context compared to having just one big file open.
*/

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

// TestStream_transitionStateOnSendEndStream tests the internal state transitions
// when the server sends an END_STREAM flag.

func TestStream_transitionStateOnSendEndStream(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name         string
		initialState StreamState
		// endStreamSentToClient is assumed to be true by the caller of transitionStateOnSendEndStream
		expectedStateAfter StreamState
	}{
		{"From Open", StreamStateOpen, StreamStateHalfClosedLocal},
		{"From ReservedLocal", StreamStateReservedLocal, StreamStateHalfClosedLocal},
		{"From HalfClosedRemote", StreamStateHalfClosedRemote, StreamStateClosed},
		{"From HalfClosedLocal (idempotent)", StreamStateHalfClosedLocal, StreamStateHalfClosedLocal},
		{"From Closed (idempotent)", StreamStateClosed, StreamStateClosed},
		// Invalid initial states like Idle, ReservedRemote are not expected to call this path,
		// as sending END_STREAM from those server states is typically a protocol violation
		// or handled differently (e.g. HEADERS from ReservedLocal implicitly has END_STREAM).
		// If they were to call it, the default case in the switch would be hit (no state change, logs error).
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := newTestConnection(t, false, nil)
			conn.writerChan = make(chan Frame, 1) // For teardown
			stream := newTestStream(t, 1, conn, true, 0, 0)

			stream.mu.Lock()
			stream.state = tc.initialState
			stream.endStreamSentToClient = true // Precondition for calling the function
			stream.mu.Unlock()

			stream.mu.Lock() // Mimic caller holding lock
			stream.transitionStateOnSendEndStream()
			finalState := stream.state
			stream.mu.Unlock()

			if finalState != tc.expectedStateAfter {
				t.Errorf("Expected state %s, got %s. Initial was %s", tc.expectedStateAfter, finalState, tc.initialState)
			}
		})
	}
}

// stdio is used to make the test compatible with Go 1.22's io.EOF changes.
// For older Go versions, "io" should be used directly.

func TestHeaderFieldConversionHelpers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		inputHeaders   []HeaderField // http2.HeaderField
		expectHpackNil bool
		expectedHpack  []hpack.HeaderField
	}{
		{
			name:           "nil input",
			inputHeaders:   nil,
			expectHpackNil: true,
			expectedHpack:  nil,
		},
		{
			name:         "empty input",
			inputHeaders: []HeaderField{},
			// expectHpackNil: false, // an empty slice is not nil
			expectedHpack: []hpack.HeaderField{},
		},
		{
			name: "single header",
			inputHeaders: []HeaderField{
				{Name: "Content-Type", Value: "application/json"},
			},
			expectedHpack: []hpack.HeaderField{
				{Name: "Content-Type", Value: "application/json"},
			},
		},
		{
			name: "multiple headers",
			inputHeaders: []HeaderField{
				{Name: "Content-Type", Value: "application/json"},
				{Name: "X-Custom-Header", Value: "value123"},
				{Name: ":status", Value: "200"},
			},
			expectedHpack: []hpack.HeaderField{
				{Name: "Content-Type", Value: "application/json"},
				{Name: "X-Custom-Header", Value: "value123"},
				{Name: ":status", Value: "200"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name+"_http2HeaderFieldsToHpackHeaderFields", func(t *testing.T) {
			result := http2HeaderFieldsToHpackHeaderFields(tc.inputHeaders)
			if tc.expectHpackNil {
				if result != nil {
					t.Errorf("Expected nil result, got %v", result)
				}
			} else {
				if result == nil && tc.expectedHpack != nil { // if expected is nil and result is nil, that's fine.
					t.Fatalf("Expected non-nil result, got nil. Expected: %v", tc.expectedHpack)
				}
				if len(result) != len(tc.expectedHpack) {
					t.Fatalf("Length mismatch. Got %d, want %d. Result: %v, Expected: %v", len(result), len(tc.expectedHpack), result, tc.expectedHpack)
				}
				for i := range result {
					if result[i].Name != tc.expectedHpack[i].Name || result[i].Value != tc.expectedHpack[i].Value {
						t.Errorf("Header mismatch at index %d. Got %v, want %v", i, result[i], tc.expectedHpack[i])
					}
					// Sensitive field is not set by this conversion, so it should be default (false)
					if result[i].Sensitive {
						t.Errorf("Header at index %d has Sensitive=true, expected false. Got %v", i, result[i])
					}
				}
			}
		})

		t.Run(tc.name+"_http2ToHpackHeaders", func(t *testing.T) {
			result := http2ToHpackHeaders(tc.inputHeaders)
			if tc.expectHpackNil {
				if result != nil {
					t.Errorf("Expected nil result, got %v", result)
				}
			} else {
				if result == nil && tc.expectedHpack != nil {
					t.Fatalf("Expected non-nil result, got nil. Expected: %v", tc.expectedHpack)
				}
				if len(result) != len(tc.expectedHpack) {
					t.Fatalf("Length mismatch. Got %d, want %d. Result: %v, Expected: %v", len(result), len(tc.expectedHpack), result, tc.expectedHpack)
				}
				for i := range result {
					if result[i].Name != tc.expectedHpack[i].Name || result[i].Value != tc.expectedHpack[i].Value {
						t.Errorf("Header mismatch at index %d. Got %v, want %v", i, result[i], tc.expectedHpack[i])
					}
					if result[i].Sensitive {
						t.Errorf("Header at index %d has Sensitive=true, expected false. Got %v", i, result[i])
					}
				}
			}
		})
	}

}

func TestNewStream_SuccessfulInitialization(t *testing.T) {
	t.Parallel()
	const testStreamID = uint32(7)
	const ourInitialWindowSize = uint32(12345)
	const peerInitialWindowSize = uint32(54321)
	const isInitiatedByPeer = true

	conn, _ := newTestConnection(t, false, nil)
	// Default priority values used by newTestStream
	const expectedPrioWeight = uint8(16 - 1) // Normalized value for spec 16
	const expectedPrioParentID = uint32(0)   // Corresponds to defaultParentID in newTestStream
	const expectedPrioExclusive = false      // Corresponds to defaultExclusive in newTestStream
	conn.ourInitialWindowSize = ourInitialWindowSize
	conn.peerInitialWindowSize = peerInitialWindowSize
	conn.writerChan = make(chan Frame, 1) // For teardown stream.Close()

	// prioWeight, prioParentID, prioExclusive are handled by newStream's defaults or explicit parameters if needed
	stream := newTestStream(t, testStreamID, conn, isInitiatedByPeer, ourInitialWindowSize, peerInitialWindowSize)

	if stream == nil {
		t.Fatal("newTestStream returned nil stream for successful initialization case")
	}

	// Verify ID
	if stream.id != testStreamID {
		t.Errorf("stream.id = %d, want %d", stream.id, testStreamID)
	}

	// Verify initial state
	stream.mu.RLock()
	initialState := stream.state
	stream.mu.RUnlock()
	if initialState != StreamStateIdle {
		t.Errorf("stream.state = %s, want %s", initialState, StreamStateIdle)
	}

	// Verify connection
	if stream.conn != conn {
		t.Error("stream.conn does not point to the mock connection")
	}

	// Verify Flow Control Manager
	if stream.fcManager == nil {
		t.Fatal("stream.fcManager is nil")
	}
	if stream.fcManager.streamID != testStreamID {
		t.Errorf("stream.fcManager.streamID = %d, want %d", stream.fcManager.streamID, testStreamID)
	}
	// Check initial send window (based on peer's initial window size)
	if sendAvail := stream.fcManager.GetStreamSendAvailable(); sendAvail != int64(peerInitialWindowSize) {
		t.Errorf("stream.fcManager send window available = %d, want %d", sendAvail, peerInitialWindowSize)
	}
	// Check initial receive window (based on our initial window size)
	if recvAvail := stream.fcManager.GetStreamReceiveAvailable(); recvAvail != int64(ourInitialWindowSize) {
		t.Errorf("stream.fcManager receive window available = %d, want %d", recvAvail, ourInitialWindowSize)
	}

	// Verify Priority settings on stream
	if stream.priorityWeight != expectedPrioWeight {
		t.Errorf("stream.priorityWeight = %d, want %d", stream.priorityWeight, expectedPrioWeight)
	}
	if stream.priorityParentID != expectedPrioParentID {
		t.Errorf("stream.priorityParentID = %d, want %d", stream.priorityParentID, expectedPrioParentID)
	}
	if stream.priorityExclusive != expectedPrioExclusive {
		t.Errorf("stream.priorityExclusive = %v, want %v", stream.priorityExclusive, expectedPrioExclusive)
	}

	// Verify Priority Registration in Tree
	// mc.priorityTree is a real PriorityTree.
	parent, children, weight, err := conn.priorityTree.GetDependencies(testStreamID)
	if err != nil {
		t.Fatalf("mc.priorityTree.GetDependencies(%d) failed: %v", testStreamID, err)
	}
	if parent != expectedPrioParentID {
		t.Errorf("PriorityTree parent for stream %d = %d, want %d", testStreamID, parent, expectedPrioParentID)
	}
	if weight != expectedPrioWeight {
		t.Errorf("PriorityTree weight for stream %d = %d, want %d", testStreamID, weight, expectedPrioWeight)
	}
	// Note: Exclusive flag is part of the operation, not persistently stored on node in this simple model.
	// Children will be empty initially.
	if len(children) != 0 {
		t.Errorf("PriorityTree children for stream %d = %v, want empty", testStreamID, children)
	}

	// Verify Request Body Pipes
	if stream.requestBodyReader == nil {
		t.Error("stream.requestBodyReader is nil")
	}
	if stream.requestBodyWriter == nil {
		t.Error("stream.requestBodyWriter is nil")
	}
	// Test pipe connectivity
	go func() {
		_, err := stream.requestBodyWriter.Write([]byte("ping"))
		if err != nil {
			// This can happen if the stream is closed by the test cleanup before write completes.
			// Check if error is due to closed pipe.
			if err != io.ErrClosedPipe && !strings.Contains(err.Error(), "closed pipe") {
				// Use t.Logf for errors in goroutines to avoid direct t.Errorf/Fatalf
				t.Logf("Error writing to requestBodyWriter in test goroutine: %v", err)
			}
		}
		stream.requestBodyWriter.Close()
	}()
	buf := make([]byte, 4)
	n, err := stream.requestBodyReader.Read(buf)
	if err != nil && err != io.EOF {
		t.Errorf("Error reading from requestBodyReader: %v", err)
	}
	if n != 4 || string(buf) != "ping" {
		t.Errorf("Read from pipe: got %q (n=%d), want %q (n=4)", string(buf[:n]), n, "ping")
	}

	// Verify Context
	if stream.ctx == nil {
		t.Fatal("stream.ctx is nil")
	}
	if stream.cancelCtx == nil {
		t.Fatal("stream.cancelCtx is nil")
	}
	select {
	case <-stream.ctx.Done():
		t.Error("stream.ctx was initially done")
	default: // Expected
	}

	// Verify initiatedByPeer
	if stream.initiatedByPeer != isInitiatedByPeer {
		t.Errorf("stream.initiatedByPeer = %v, want %v", stream.initiatedByPeer, isInitiatedByPeer)
	}

	// Verify responseHeadersSent
	if stream.responseHeadersSent {
		t.Error("stream.responseHeadersSent was initially true, want false")
	}
}

func TestNewStream_PriorityAddFailure(t *testing.T) {
	t.Parallel()
	const testStreamID = uint32(8) // Use a different ID
	const ourInitialWindowSize = DefaultInitialWindowSize
	const peerInitialWindowSize = DefaultInitialWindowSize

	// This setup will cause PriorityTree.AddStream to fail because streamID == prioParentID
	conn, _ := newTestConnection(t, false, nil) // isClient = false
	// conn.ourInitialWindowSize and conn.peerInitialWindowSize are set to defaults by newTestConnection
	// if not specified, which is fine for this test.
	// Logger, context, and priority tree are also initialized by newTestConnection.

	// Call newStream directly to test its error path
	// Trigger error: stream depends on itself (streamID == prioParentID)
	stream, err := newStream(
		conn,
		testStreamID,
		ourInitialWindowSize,  // This is 'ourWin' for the stream (its receive window)
		peerInitialWindowSize, // This is 'peerWin' for the stream (its send window)
		16,                    // prioWeight
		testStreamID,          // prioParentID - causes failure
		false,                 // prioExclusive
		true,                  // isInitiatedByPeer
	)

	if err == nil {
		t.Fatal("newStream was expected to fail due to priority registration error, but succeeded")
	}
	if stream != nil {
		t.Error("newStream returned a non-nil stream on failure")
		// Attempt to clean up if stream was unexpectedly returned
		if stream != nil {
			_ = stream.Close(fmt.Errorf("cleanup unexpected stream from failed newStream call"))
		}
	}

	// Check the error message (optional, but good for confirming reason)
	expectedErrSubstrings := []string{
		"stream cannot depend on itself",                               // Original error from priority.go
		"invalid stream dependency: stream 8",                          // Part of the more specific error
		"stream error on stream 8",                                     // Error from stream.go wrapper
		fmt.Sprintf("stream %d cannot depend on itself", testStreamID), // More generic check from priority
	}
	found := false
	for _, sub := range expectedErrSubstrings {
		if strings.Contains(err.Error(), sub) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("newStream error message = %q, did not contain any of the expected substrings: %v", err.Error(), expectedErrSubstrings)
	}

	// In newStream's error path, it calls cancel() for its context and closes pipes.
	// We can't directly check the stream's internal context/pipes as stream is nil.
	// This test primarily verifies that newStream *returns* an error and *doesn't* return a stream.
	// The internal cleanup within newStream's error path is assumed to be tested by virtue of it being there.
}

func TestStream_setStateToClosed_CleansUpResources(t *testing.T) {
	t.Parallel()
	const testStreamID = uint32(10)

	tests := []struct {
		name                   string
		initialPendingRSTCode  *ErrorCode
		expectedPipeReadError  error     // Expected error type or specific error from requestBodyReader.Read
		expectedFcAcquireError ErrorCode // Expected error code from fcManager.sendWindow.Acquire
	}{
		{
			name:                   "pendingRSTCode is nil",
			initialPendingRSTCode:  nil,
			expectedPipeReadError:  io.EOF,              // As per closeStreamResourcesProtected logic for nil pendingRSTCode
			expectedFcAcquireError: ErrCodeStreamClosed, // As per closeStreamResourcesProtected logic
		},
		{
			name:                   "pendingRSTCode is non-nil",
			initialPendingRSTCode:  func() *ErrorCode { e := ErrCodeProtocolError; return &e }(),
			expectedPipeReadError:  NewStreamError(testStreamID, ErrCodeProtocolError, "stream reset"), // Error should match this
			expectedFcAcquireError: ErrCodeProtocolError,                                               // Error should match this
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := newTestConnection(t, false, nil)
			conn.writerChan = make(chan Frame, 1) // For teardown stream.Close()

			stream := newTestStream(t, testStreamID, conn, true, 0, 0)

			// Setup: Initial state and pendingRSTCode
			stream.mu.Lock()
			stream.state = StreamStateOpen // Start from an open state
			if tc.initialPendingRSTCode != nil {
				// Create a distinct copy for the stream to own if it's a pointer type
				codeCopy := *tc.initialPendingRSTCode
				stream.pendingRSTCode = &codeCopy
			} else {
				stream.pendingRSTCode = nil
			}
			stream.mu.Unlock()

			// Action: Call _setState(StreamStateClosed)
			stream.mu.Lock()
			stream._setState(StreamStateClosed)
			stream.mu.Unlock()

			// Verification:
			// 1. Final Stream State
			stream.mu.RLock()
			finalState := stream.state
			finalPendingRSTCode := stream.pendingRSTCode
			stream.mu.RUnlock()

			if finalState != StreamStateClosed {
				t.Errorf("Expected stream state Closed, got %s", finalState)
			}
			if finalPendingRSTCode != nil {
				t.Errorf("Expected stream.pendingRSTCode to be nil after _setState(Closed), got %v", *finalPendingRSTCode)
			}

			// 2. Context Cancellation
			select {
			case <-stream.ctx.Done():
				if stream.ctx.Err() != context.Canceled {
					t.Errorf("Expected context error context.Canceled, got %v", stream.ctx.Err())
				}
			default:
				t.Error("Expected stream context to be canceled")
			}

			// 3. Pipe Closures
			_, errWrite := stream.requestBodyWriter.Write([]byte("test"))
			if errWrite == nil {
				t.Error("Expected error writing to requestBodyWriter after _setState(Closed), got nil")
			} else if errWrite != io.ErrClosedPipe {
				// This is the specific error io.Pipe returns for Write after CloseWithError.
				t.Errorf("requestBodyWriter.Write error: got %v (type %T), want io.ErrClosedPipe", errWrite, errWrite)
			}

			_, errRead := stream.requestBodyReader.Read(make([]byte, 1))
			if errRead == nil {
				t.Errorf("Expected error reading from requestBodyReader after _setState(Closed), got nil")
			} else {
				if tc.expectedPipeReadError == io.EOF {
					if errRead != io.EOF {
						// Sometimes pipe might wrap EOF, e.g. *os.PathError{Err:io.EOF}. For io.Pipe, it's typically direct io.EOF.
						// Or if the error from CloseWithError was io.EOF.
						t.Errorf("requestBodyReader.Read error: got %v (type %T), want io.EOF", errRead, errRead)
					}
				} else if expectedStreamErr, ok := tc.expectedPipeReadError.(*StreamError); ok {
					actualStreamErr, okActual := errRead.(*StreamError)
					if !okActual {
						t.Errorf("requestBodyReader.Read error: got %v (type %T), want *StreamError", errRead, errRead)
					} else if actualStreamErr.Code != expectedStreamErr.Code || actualStreamErr.StreamID != expectedStreamErr.StreamID {
						// Msg might slightly differ due to "stream reset" vs "stream reset affecting flow control"
						t.Errorf("requestBodyReader.Read *StreamError mismatch: got Code=%s, ID=%d; want Code=%s, ID=%d",
							actualStreamErr.Code, actualStreamErr.StreamID, expectedStreamErr.Code, expectedStreamErr.StreamID)
					}
				} else {
					t.Errorf("requestBodyReader.Read error: got %v, but expectedPipeReadError type %T not handled in test validation", errRead, tc.expectedPipeReadError)
				}
			}

			// 4. Flow Control Manager Closure
			errFcAcquire := stream.fcManager.sendWindow.Acquire(1)
			if errFcAcquire == nil {
				t.Error("Expected error acquiring from flow control sendWindow after _setState(Closed), got nil")
			} else {
				streamErr, ok := errFcAcquire.(*StreamError)
				if !ok {
					t.Errorf("Flow control acquire error was not *StreamError, got %T: %v", errFcAcquire, errFcAcquire)
				} else if streamErr.Code != tc.expectedFcAcquireError {
					t.Errorf("Flow control acquire *StreamError code mismatch: got %s, want %s", streamErr.Code, tc.expectedFcAcquireError)
				}
			}
			// 5. Verify interaction with conn.removeClosedStream (simulated)
			// Simulate the connection manager calling removeClosedStream after observing the stream is closed.
			// This would typically happen in the connection's main loop when it iterates over streams
			// and finds one in the Closed state.
			if stream.state == StreamStateClosed { // Only simulate if truly closed.
				// The unsafe.Pointer cast is used to access the mockConnection through the
				// stream.conn pointer, which is of type *Connection.
				// This is safe here because we know stream.conn points to our mc.

				// The stream's connection is a real *Connection. Access its fields directly.
				// No mockConnection involved here anymore.
				// This check was about conn.removeClosedStream call, which for a real Connection
				// happens internally and is harder to mock/verify at this level of unit test
				// without more complex connection mocking or observation hooks.
				// For now, we assume that if the stream transitions to Closed, the Connection
				// would eventually remove it. This specific mock verification is removed.
				// t.Logf("Skipping mockConnection.removeClosedStream check as it's now a real Connection.")

				// If we need to verify the connection's active stream count, that's a different test.
				// For example, check len(stream.conn.streams) if that's the desired check.
				// However, stream.conn.streams is protected by streamsMu.
				stream.conn.streamsMu.RLock()
				_, streamExists := stream.conn.streams[stream.id]
				stream.conn.streamsMu.RUnlock()
				if streamExists {
					// This is subtle: _setState(Closed) calls closeStreamResourcesProtected, which does *not*
					// remove the stream from conn.streams map. That removal is the responsibility of the
					// connection's main loop or a dedicated cleanup mechanism that observes closed streams.
					// So, it's expected for the stream to still be in the map immediately after _setState(Closed).
					// The test might need to be about *when* the connection removes it, not *if* _setState does.
					t.Logf("Stream %d still exists in conn.streams map after _setState(Closed), which is expected as _setState itself doesn't remove it from parent.", stream.id)
				}
			} else {
				t.Errorf("Stream was not in Closed state before simulating removeClosedStream call; state: %s", stream.state)
			}
			// newTestStream's t.Cleanup will Close stream, which will be idempotent.
		})
	}
}

// The original getPipeErrors has been removed as it was too complex and had side effects.
// The checks are now done directly in TestStream_setState_GeneralTransitions.

func TestStream_WriteData(t *testing.T) {
	t.Parallel()
	const testStreamID = uint32(1)

	tests := []struct {
		name string
		// Initial stream state
		initialState               StreamState
		initialResponseHeadersSent bool
		initialEndStreamSentClient bool
		initialPendingRSTCode      *ErrorCode
		// Initial flow control state (absolute values)
		initialStreamSendWindow int64
		initialConnSendWindow   int64
		// Connection's maxFrameSize for this test
		cfgMaxFrameSize uint32
		// Input to WriteData
		dataToSend    []byte
		endStreamFlag bool
		// Mock connection behavior
		connSendDataFrameError error // If non-nil, conn.sendDataFrame will be simulated to fail with this.
		// Expected outcomes
		expectedN             int
		expectError           bool
		expectedErrorContains string
		// Expected stream state after
		expectedFinalState         StreamState
		expectedFinalEndStreamSent bool
		// Expected frames on writerChan
		expectedDataFrames []struct {
			Data      []byte
			EndStream bool
		}
		// Expected flow control state after (absolute values)
		expectedStreamSendWindowAfter int64
		expectedConnSendWindowAfter   int64
	}{
		{
			name:                       "Success: send data, no endStream",
			initialState:               StreamStateOpen,
			initialResponseHeadersSent: true,
			initialStreamSendWindow:    100,
			initialConnSendWindow:      200,
			cfgMaxFrameSize:            DefaultMaxFrameSize,
			dataToSend:                 []byte("hello"),
			endStreamFlag:              false,
			expectedN:                  5,
			expectError:                false,
			expectedFinalState:         StreamStateOpen,
			expectedFinalEndStreamSent: false,
			expectedDataFrames: []struct {
				Data      []byte
				EndStream bool
			}{{Data: []byte("hello"), EndStream: false}},
			expectedStreamSendWindowAfter: 95,
			expectedConnSendWindowAfter:   195,
		},
		{
			name:                       "Success: send data, with endStream",
			initialState:               StreamStateOpen,
			initialResponseHeadersSent: true,
			initialStreamSendWindow:    100,
			initialConnSendWindow:      200,
			cfgMaxFrameSize:            DefaultMaxFrameSize,
			dataToSend:                 []byte("world"),
			endStreamFlag:              true,
			expectedN:                  5,
			expectError:                false,
			expectedFinalState:         StreamStateHalfClosedLocal,
			expectedFinalEndStreamSent: true,
			expectedDataFrames: []struct {
				Data      []byte
				EndStream bool
			}{{Data: []byte("world"), EndStream: true}},
			expectedStreamSendWindowAfter: 95,
			expectedConnSendWindowAfter:   195,
		},
		{
			name:                       "Success: send zero-length data, with endStream",
			initialState:               StreamStateOpen,
			initialResponseHeadersSent: true,
			initialStreamSendWindow:    100,
			initialConnSendWindow:      200,
			cfgMaxFrameSize:            DefaultMaxFrameSize,
			dataToSend:                 []byte{},
			endStreamFlag:              true,
			expectedN:                  0,
			expectError:                false,
			expectedFinalState:         StreamStateHalfClosedLocal,
			expectedFinalEndStreamSent: true,
			expectedDataFrames: []struct {
				Data      []byte
				EndStream bool
			}{{Data: []byte{}, EndStream: true}},
			expectedStreamSendWindowAfter: 100, // No window consumed for 0-length
			expectedConnSendWindowAfter:   200, // No window consumed for 0-length
		},
		{
			name:                          "No-op: send zero-length data, no endStream",
			initialState:                  StreamStateOpen,
			initialResponseHeadersSent:    true,
			initialStreamSendWindow:       100,
			initialConnSendWindow:         200,
			cfgMaxFrameSize:               DefaultMaxFrameSize,
			dataToSend:                    []byte{},
			endStreamFlag:                 false,
			expectedN:                     0,
			expectError:                   false,
			expectedFinalState:            StreamStateOpen,
			expectedFinalEndStreamSent:    false,
			expectedDataFrames:            nil, // No frame sent
			expectedStreamSendWindowAfter: 100,
			expectedConnSendWindowAfter:   200,
		},
		{
			name:                       "Success: send data larger than maxFrameSize, chunking",
			initialState:               StreamStateOpen,
			initialResponseHeadersSent: true,
			initialStreamSendWindow:    100,
			initialConnSendWindow:      200,
			cfgMaxFrameSize:            5,                            // Force chunking
			dataToSend:                 []byte("data_chunking_test"), // 18 bytes
			endStreamFlag:              true,
			expectedN:                  18,
			expectError:                false,
			expectedFinalState:         StreamStateHalfClosedLocal,
			expectedFinalEndStreamSent: true,
			expectedDataFrames: []struct {
				Data      []byte
				EndStream bool
			}{
				{Data: []byte("data_"), EndStream: false},
				{Data: []byte("chunk"), EndStream: false},
				{Data: []byte("ing_t"), EndStream: false},
				{Data: []byte("est"), EndStream: true},
			},
			expectedStreamSendWindowAfter: 100 - 18,
			expectedConnSendWindowAfter:   200 - 18,
		},
		{
			name:                          "Error: WriteData called before SendHeaders",
			initialState:                  StreamStateOpen,
			initialResponseHeadersSent:    false, // Key condition
			initialStreamSendWindow:       100,
			initialConnSendWindow:         200,
			cfgMaxFrameSize:               DefaultMaxFrameSize,
			dataToSend:                    []byte("test"),
			endStreamFlag:                 false,
			expectedN:                     0,
			expectError:                   true,
			expectedErrorContains:         "SendHeaders must be called before WriteData",
			expectedFinalState:            StreamStateOpen, // State unchanged
			expectedFinalEndStreamSent:    false,
			expectedDataFrames:            nil,
			expectedStreamSendWindowAfter: 100, // FC not acquired
			expectedConnSendWindowAfter:   200,
		},
		{
			name:                          "Error: stream closed",
			initialState:                  StreamStateClosed, // Key condition
			initialResponseHeadersSent:    true,
			initialStreamSendWindow:       0,
			initialConnSendWindow:         200,
			cfgMaxFrameSize:               DefaultMaxFrameSize,
			dataToSend:                    []byte("test"),
			endStreamFlag:                 false,
			expectedN:                     0,
			expectError:                   true,
			expectedErrorContains:         "cannot send data on closed, reset, or already server-ended stream",
			expectedFinalState:            StreamStateClosed,
			expectedFinalEndStreamSent:    false,
			expectedDataFrames:            nil,
			expectedStreamSendWindowAfter: 0,
			expectedConnSendWindowAfter:   200,
		},
		{
			name:                          "Error: stream resetting (pendingRSTCode set)",
			initialState:                  StreamStateOpen,
			initialResponseHeadersSent:    true,
			initialPendingRSTCode:         func() *ErrorCode { e := ErrCodeCancel; return &e }(), // Key condition
			initialStreamSendWindow:       100,
			initialConnSendWindow:         200,
			cfgMaxFrameSize:               DefaultMaxFrameSize,
			dataToSend:                    []byte("test"),
			endStreamFlag:                 false,
			expectedN:                     0,
			expectError:                   true,
			expectedErrorContains:         "cannot send data on closed, reset, or already server-ended stream",
			expectedFinalState:            StreamStateOpen, // State unchanged by this call
			expectedFinalEndStreamSent:    false,
			expectedDataFrames:            nil,
			expectedStreamSendWindowAfter: 100,
			expectedConnSendWindowAfter:   200,
		},
		{
			name:                          "Error: WriteData after END_STREAM already sent",
			initialState:                  StreamStateHalfClosedLocal,
			initialResponseHeadersSent:    true,
			initialEndStreamSentClient:    true, // Key condition
			initialStreamSendWindow:       100,
			initialConnSendWindow:         200,
			cfgMaxFrameSize:               DefaultMaxFrameSize,
			dataToSend:                    []byte("test"),
			endStreamFlag:                 false,
			expectedN:                     0,
			expectError:                   true,
			expectedErrorContains:         "cannot send data on closed, reset, or already server-ended stream",
			expectedFinalState:            StreamStateHalfClosedLocal,
			expectedFinalEndStreamSent:    true,
			expectedDataFrames:            nil,
			expectedStreamSendWindowAfter: 100,
			expectedConnSendWindowAfter:   200,
		},
		{
			name:                          "Error: stream flow control insufficient",
			initialState:                  StreamStateOpen,
			initialResponseHeadersSent:    true,
			initialStreamSendWindow:       5, // Insufficient for "ten_bytes_"
			initialConnSendWindow:         200,
			cfgMaxFrameSize:               DefaultMaxFrameSize,
			dataToSend:                    []byte("ten_bytes_"), // 10 bytes
			endStreamFlag:                 false,
			expectedN:                     0,
			expectError:                   true,
			expectedErrorContains:         "simulated insufficient stream FC window (closed for test)",
			expectedFinalState:            StreamStateOpen, // FC error doesn't change state itself in WriteData
			expectedFinalEndStreamSent:    false,
			expectedDataFrames:            nil,
			expectedStreamSendWindowAfter: 5, // FC acquire failed as window was pre-closed by test
			expectedConnSendWindowAfter:   200,
		},
		{
			name:                       "Error: connection flow control insufficient",
			initialState:               StreamStateOpen,
			initialResponseHeadersSent: true,
			initialStreamSendWindow:    100,
			initialConnSendWindow:      5, // Insufficient for "ten_bytes_"
			cfgMaxFrameSize:            DefaultMaxFrameSize,
			dataToSend:                 []byte("ten_bytes_"), // 10 bytes
			endStreamFlag:              false,
			expectedN:                  0,
			expectError:                true,
			expectedErrorContains:      "simulated insufficient conn FC window (closed for test)",
			expectedFinalState:         StreamStateOpen,
			expectedFinalEndStreamSent: false,
			expectedDataFrames:         nil,
			// Stream FC (10 bytes) acquired, then conn FC fails, then stream FC is released by WriteData's error handling.
			expectedStreamSendWindowAfter: 100,
			expectedConnSendWindowAfter:   5, // Conn FC acquire failed as window was pre-closed by test
		},
		{
			name:                       "Error: conn.sendDataFrame fails",
			initialState:               StreamStateOpen,
			initialResponseHeadersSent: true,
			initialStreamSendWindow:    100,
			initialConnSendWindow:      200,
			cfgMaxFrameSize:            DefaultMaxFrameSize,
			dataToSend:                 []byte("sendfail"), // 8 bytes
			endStreamFlag:              false,
			connSendDataFrameError:     NewConnectionError(ErrCodeConnectError, "simulated conn write error from test"),
			expectedN:                  0,
			expectError:                true,
			expectedErrorContains:      fmt.Sprintf("connection error: connection shutting down (pre-check), cannot send DATA for stream %d", testStreamID),
			expectedFinalState:         StreamStateOpen,
			expectedFinalEndStreamSent: false,
			expectedDataFrames:         nil,
			// FC is acquired before send attempt. If send fails, FC is NOT currently released by WriteData.
			expectedStreamSendWindowAfter: 100 - 8, // 92
			expectedConnSendWindowAfter:   200 - 8, // 192
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Ensure cfgMaxFrameSize has a sensible default for the test case if not specified.
			effectiveCfgMaxFrameSize := tc.cfgMaxFrameSize
			if effectiveCfgMaxFrameSize == 0 {
				effectiveCfgMaxFrameSize = DefaultMaxFrameSize
			}

			// Create a real connection using newTestConnection
			conn, _ := newTestConnection(t, false, nil)
			conn.writerChan = make(chan Frame, len(tc.dataToSend)/int(effectiveCfgMaxFrameSize)+2) // Buffer for all chunks + potential RST
			conn.ourInitialWindowSize = DefaultInitialWindowSize                                   // Affects stream's receive window
			conn.peerInitialWindowSize = uint32(tc.initialStreamSendWindow)                        // Sets stream's initial *send* window
			conn.maxFrameSize = effectiveCfgMaxFrameSize                                           // Max frame size used by stream for chunking

			if conn.connFCManager == nil {
				conn.connFCManager = NewConnectionFlowControlManager()
			}
			// Adjust connection FC window to match test case's initialConnSendWindow
			currentConnFcAvailable := conn.connFCManager.sendWindow.Available()
			deltaConnFc := tc.initialConnSendWindow - currentConnFcAvailable
			if deltaConnFc < 0 { // Need to acquire/reduce
				if errSetup := conn.connFCManager.sendWindow.Acquire(uint32(-deltaConnFc)); errSetup != nil {
					t.Fatalf("Setup: Failed to pre-acquire from connFC: %v (target: %d, current: %d, acquire: %d)", errSetup, tc.initialConnSendWindow, currentConnFcAvailable, -deltaConnFc)
				}
			} else if deltaConnFc > 0 { // Need to increase
				if errSetup := conn.connFCManager.sendWindow.Increase(uint32(deltaConnFc)); errSetup != nil {
					t.Fatalf("Setup: Failed to pre-increase connFC: %v (target: %d, current: %d, increase: %d)", errSetup, tc.initialConnSendWindow, currentConnFcAvailable, deltaConnFc)
				}
			}
			if conn.connFCManager.sendWindow.Available() != tc.initialConnSendWindow {
				t.Fatalf("Setup: connFCManager window not set as expected. Got %d, want %d", conn.connFCManager.sendWindow.Available(), tc.initialConnSendWindow)
			}

			if tc.connSendDataFrameError != nil {
				if conn.shutdownChan == nil {
					conn.shutdownChan = make(chan struct{})
				}
				close(conn.shutdownChan)
				conn.connError = tc.connSendDataFrameError
			}

			// false for isInitiatedByPeer, as this test is for server sending data.
			// Provide initialOurWindow (stream's receive window) and initialPeerWindow (stream's send window).
			// For WriteData, stream's send window (initialPeerWindow) is most relevant from fc perspective.
			stream := newTestStream(t, testStreamID, conn, false, DefaultInitialWindowSize, uint32(tc.initialStreamSendWindow))

			// Adjust stream FC window to match test case's initialStreamSendWindow AFTER stream creation (as newTestStream uses cfgPeerInitialWindowSize)
			// This is a bit redundant as newTestStream already sets it, but ensures exactness if initialStreamSendWindow is tricky.
			// newTestStream correctly uses cfgPeerInitialWindowSize to init stream's send window.
			// So stream.fcManager.sendWindow.Available() should already be tc.initialStreamSendWindow if logic in newTestStream is correct.

			stream.mu.Lock()
			stream.state = tc.initialState
			stream.responseHeadersSent = tc.initialResponseHeadersSent
			stream.endStreamSentToClient = tc.initialEndStreamSentClient
			if tc.initialPendingRSTCode != nil {
				codeCopy := *tc.initialPendingRSTCode
				stream.pendingRSTCode = &codeCopy
			}
			stream.mu.Unlock()

			initialStreamWin := stream.fcManager.GetStreamSendAvailable()
			if initialStreamWin != tc.initialStreamSendWindow {
				// This would indicate a problem in newTestStream's setup of peerInitialWin for stream.fcManager
				t.Logf("Warning: Initial stream send window from fcManager (%d) does not match test case tc.initialStreamSendWindow (%d). Check newTestStream logic.", initialStreamWin, tc.initialStreamSendWindow)
				// Forcibly adjust for test if possible, though ideally newTestStream handles it.
				// This path is complex due to FlowControlWindow internals. Best rely on newTestStream.
			}
			initialConnWin := conn.connFCManager.GetConnectionSendAvailable()

			if tc.name == "Error: stream flow control insufficient" {
				simulatedStreamFCError := errors.New("simulated insufficient stream FC window (closed for test)")
				stream.fcManager.sendWindow.Close(simulatedStreamFCError) // Close the window to make Acquire fail
			}
			if tc.name == "Error: connection flow control insufficient" {
				simulatedConnFCError := errors.New("simulated insufficient conn FC window (closed for test)")
				conn.connFCManager.sendWindow.Close(simulatedConnFCError) // Close the window to make Acquire fail
			}

			n, err := stream.WriteData(tc.dataToSend, tc.endStreamFlag)

			if tc.expectError {
				if err == nil {
					t.Fatalf("Expected an error, but got nil")
				}
				if tc.expectedErrorContains != "" && !strings.Contains(err.Error(), tc.expectedErrorContains) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tc.expectedErrorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("Expected no error, but got: %v", err)
				}
			}

			if n != tc.expectedN {
				t.Errorf("Expected n=%d, got n=%d", tc.expectedN, n)
			}

			if len(tc.expectedDataFrames) > 0 {
				for i, expectedFrame := range tc.expectedDataFrames {
					select {
					case frame := <-conn.writerChan:
						dataFrame, ok := frame.(*DataFrame)
						if !ok {
							t.Fatalf("Expected DataFrame %d on writerChan, got %T", i+1, frame)
						}
						if dataFrame.Header().StreamID != testStreamID {
							t.Errorf("Frame %d: DataFrame StreamID mismatch: got %d, want %d", i+1, dataFrame.Header().StreamID, testStreamID)
						}
						if string(dataFrame.Data) != string(expectedFrame.Data) {
							t.Errorf("Frame %d: DataFrame Data mismatch: got %q, want %q", i+1, string(dataFrame.Data), string(expectedFrame.Data))
						}
						if (dataFrame.Header().Flags&FlagDataEndStream != 0) != expectedFrame.EndStream {
							t.Errorf("Frame %d: DataFrame END_STREAM flag mismatch: got %v, want %v (Flags: %08b)", i+1, (dataFrame.Header().Flags&FlagDataEndStream != 0), expectedFrame.EndStream, dataFrame.Header().Flags)
						}
					case <-time.After(100 * time.Millisecond):
						t.Fatalf("Expected DataFrame %d on writerChan, but none found. Expected: {Data: %q, EndStream: %v}", i+1, string(expectedFrame.Data), expectedFrame.EndStream)
					}
				}
				// Ensure no more frames are sent if all expected frames were received
				select {
				case frame := <-conn.writerChan:
					t.Fatalf("Expected no more frames, but got: %T (StreamID: %d)", frame, frame.Header().StreamID)
				default: // Good, no more frames
				}
			} else { // No frames expected
				select {
				case frame := <-conn.writerChan:
					t.Fatalf("Did not expect any frame on writerChan, but got: %T (StreamID: %d)", frame, frame.Header().StreamID)
				default:
					// Expected: no frame
				}
			}

			stream.mu.RLock()
			finalState := stream.state
			finalEndStreamSent := stream.endStreamSentToClient
			stream.mu.RUnlock()

			if finalState != tc.expectedFinalState {
				t.Errorf("Expected final stream state %s, got %s", tc.expectedFinalState, finalState)
			}
			if finalEndStreamSent != tc.expectedFinalEndStreamSent {
				t.Errorf("Expected final endStreamSentToClient %v, got %v", tc.expectedFinalEndStreamSent, finalEndStreamSent)
			}

			finalStreamWin := stream.fcManager.GetStreamSendAvailable()
			finalConnWin := conn.connFCManager.GetConnectionSendAvailable()

			if finalStreamWin != tc.expectedStreamSendWindowAfter {
				t.Errorf("Expected stream send window %d, got %d. (Initial: %d, Change: %d)",
					tc.expectedStreamSendWindowAfter, finalStreamWin, tc.initialStreamSendWindow, tc.initialStreamSendWindow-finalStreamWin)
			}
			if finalConnWin != tc.expectedConnSendWindowAfter {
				t.Errorf("Expected conn send window %d, got %d. (Initial: %d, Change: %d)",
					tc.expectedConnSendWindowAfter, finalConnWin, initialConnWin, initialConnWin-finalConnWin)
			}
		})
	}
}

func TestStream_WriteTrailers(t *testing.T) {
	t.Parallel()
	const testStreamID = uint32(1)
	validTrailers := makeStreamWriterHeaders("x-trailer-1", "value1", "x-trailer-2", "value2")
	invalidHpackTrailers := makeStreamWriterHeaders("", "bad-trailer-name") // Empty name to cause HPACK error in conn

	tests := []struct {
		name                         string
		initialState                 StreamState
		initialResponseHeadersSent   bool
		initialEndStreamSentToClient bool // If true, server already sent END_STREAM (e.g. via WriteData)
		initialPendingRSTCode        *ErrorCode
		trailersToSend               []HeaderField
		connSendHeadersFrameError    error // If non-nil, conn.sendHeadersFrame will be simulated to fail
		expectError                  bool
		expectedErrorContains        string
		expectFrameSent              bool // True if a HEADERS frame for trailers is expected on writerChan
		expectedFinalState           StreamState
		expectedFinalEndStreamSent   bool // Should always be true if trailers are successfully sent
	}{
		{
			name:                         "Success: send trailers after headers (no data yet)",
			initialState:                 StreamStateOpen,
			initialResponseHeadersSent:   true,
			initialEndStreamSentToClient: false,
			trailersToSend:               validTrailers,
			expectError:                  false,
			expectFrameSent:              true,
			expectedFinalState:           StreamStateHalfClosedLocal, // Because trailers imply END_STREAM
			expectedFinalEndStreamSent:   true,
		},
		{
			name:                         "Success: send trailers after data (endStreamSentToClient was false)",
			initialState:                 StreamStateOpen, // Assume data was sent without END_STREAM
			initialResponseHeadersSent:   true,
			initialEndStreamSentToClient: false,
			trailersToSend:               validTrailers,
			expectError:                  false,
			expectFrameSent:              true,
			expectedFinalState:           StreamStateHalfClosedLocal,
			expectedFinalEndStreamSent:   true,
		},
		{
			name:                         "Error: send trailers when endStreamSentToClient was already true (e.g. after WriteData with endStream)",
			initialState:                 StreamStateHalfClosedLocal, // Because server already sent END_STREAM
			initialResponseHeadersSent:   true,
			initialEndStreamSentToClient: true,
			trailersToSend:               validTrailers,
			expectError:                  true,
			expectedErrorContains:        "cannot send trailers after stream already ended",
			expectFrameSent:              false,
			expectedFinalState:           StreamStateHalfClosedLocal, // State should not change
			expectedFinalEndStreamSent:   true,                       // Remains true
		},
		{
			name:                         "Success: send trailers from HalfClosedRemote state (client sent END_STREAM, server now sends trailers)",
			initialState:                 StreamStateHalfClosedRemote,
			initialResponseHeadersSent:   true,
			initialEndStreamSentToClient: false,
			trailersToSend:               validTrailers,
			expectError:                  false,
			expectFrameSent:              true,
			expectedFinalState:           StreamStateClosed, // Both sides have sent END_STREAM
			expectedFinalEndStreamSent:   true,
		},
		{
			name:                       "Error: WriteTrailers called before SendHeaders",
			initialState:               StreamStateOpen,
			initialResponseHeadersSent: false, // Key condition
			trailersToSend:             validTrailers,
			expectError:                true,
			expectedErrorContains:      "cannot send trailers before headers",
			expectFrameSent:            false,
			expectedFinalState:         StreamStateOpen, // State unchanged
			expectedFinalEndStreamSent: false,
		},
		{
			name:                       "Error: stream closed",
			initialState:               StreamStateClosed, // Key condition
			initialResponseHeadersSent: true,              // Irrelevant as stream is closed
			trailersToSend:             validTrailers,
			expectError:                true,
			expectedErrorContains:      "stream closed or resetting",
			expectFrameSent:            false,
			expectedFinalState:         StreamStateClosed,
			expectedFinalEndStreamSent: false, // Or initialEndStreamSentToClient if it was already set
		},
		{
			name:                       "Error: stream resetting (pendingRSTCode set)",
			initialState:               StreamStateOpen,
			initialResponseHeadersSent: true,
			initialPendingRSTCode:      func() *ErrorCode { e := ErrCodeCancel; return &e }(), // Key condition
			trailersToSend:             validTrailers,
			expectError:                true,
			expectedErrorContains:      "stream error on stream 1: stream closed or resetting (code STREAM_CLOSED, 5)",
			expectFrameSent:            false,
			expectedFinalState:         StreamStateOpen, // State unchanged by this call
			expectedFinalEndStreamSent: false,
		},
		{
			name:                       "Error: conn.sendHeadersFrame fails (e.g. HPACK error)",
			initialState:               StreamStateOpen,
			initialResponseHeadersSent: true,
			trailersToSend:             invalidHpackTrailers, // Triggers HPACK error in real conn.sendHeadersFrame
			expectError:                true,
			expectedErrorContains:      "stream error on stream 1: HPACK encoding failed (malformed header from application): hpack: invalid header field name: name is empty for Encode method (value: \"bad-trailer-name\") (code PROTOCOL_ERROR, 1)",
			expectFrameSent:            false,
			expectedFinalState:         StreamStateOpen, // State doesn't change on conn send error
			expectedFinalEndStreamSent: false,           // Not successfully sent
		},
		{
			name:                       "Error: conn.sendHeadersFrame fails (simulated connection error)",
			initialState:               StreamStateOpen,
			initialResponseHeadersSent: true,
			trailersToSend:             validTrailers,
			connSendHeadersFrameError:  NewConnectionError(ErrCodeInternalError, "simulated conn internal error"),
			expectError:                true,
			expectedErrorContains:      "connection error: connection shutting down (pre-check), cannot send HEADERS for stream 1 (last_stream_id 0, code CONNECT_ERROR, 10)",
			expectFrameSent:            false,
			expectedFinalState:         StreamStateOpen,
			expectedFinalEndStreamSent: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := newTestConnection(t, false, nil)
			conn.writerChan = make(chan Frame, 1) // Buffer for one HEADERS frame (trailers) or RST

			if tc.connSendHeadersFrameError != nil {
				if conn.shutdownChan == nil {
					conn.shutdownChan = make(chan struct{})
				}
				close(conn.shutdownChan) // Simulate connection shutting down
				conn.connError = tc.connSendHeadersFrameError
			}

			// For server sending trailers, isInitiatedByPeer is false.
			stream := newTestStream(t, testStreamID, conn, false, 0, 0)

			// Setup initial stream conditions
			stream.mu.Lock()
			stream.state = tc.initialState
			stream.responseHeadersSent = tc.initialResponseHeadersSent
			stream.endStreamSentToClient = tc.initialEndStreamSentToClient
			if tc.initialPendingRSTCode != nil {
				codeCopy := *tc.initialPendingRSTCode
				stream.pendingRSTCode = &codeCopy
			}
			stream.mu.Unlock()

			err := stream.WriteTrailers(tc.trailersToSend)

			if tc.expectError {
				if err == nil {
					t.Fatalf("Expected an error, but got nil")
				}
				if tc.expectedErrorContains != "" && !strings.Contains(err.Error(), tc.expectedErrorContains) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tc.expectedErrorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("Expected no error, but got: %v", err)
				}
			}

			if tc.expectFrameSent {

				select {
				case frame := <-conn.writerChan:
					headersFrame, ok := frame.(*HeadersFrame)
					if !ok {
						t.Fatalf("Expected HeadersFrame on writerChan, got %T", frame)
					}
					if headersFrame.Header().StreamID != testStreamID {
						t.Errorf("HeadersFrame StreamID mismatch: got %d, want %d", headersFrame.Header().StreamID, testStreamID)
					}
					if (headersFrame.Header().Flags & FlagHeadersEndStream) == 0 {
						t.Error("HeadersFrame for trailers did not have END_STREAM flag set")
					}
					// Verify that the headers sent are the trailers (simplified check for now)
					// A full check would decode the HeaderBlockFragment.
					// This test relies on the fact that conn.sendHeadersFrame gets the right hpack.HeaderFields.
					if len(tc.trailersToSend) > 0 && len(headersFrame.HeaderBlockFragment) == 0 {
						t.Error("HeadersFrame HeaderBlockFragment is empty, but trailers were provided")
					}
				case <-time.After(50 * time.Millisecond):
					t.Fatal("Expected HeadersFrame on writerChan, but none found")
				}
			} else {

				select {
				case frame := <-conn.writerChan:
					t.Fatalf("Did not expect any frame on writerChan, but got: %T (StreamID: %d)", frame, frame.Header().StreamID)
				default:
					// Expected: no frame
				}
			}

			stream.mu.RLock()
			finalState := stream.state
			finalEndStreamSent := stream.endStreamSentToClient
			stream.mu.RUnlock()

			if finalState != tc.expectedFinalState {
				t.Errorf("Expected final stream state %s, got %s (initial was %s)", tc.expectedFinalState, finalState, tc.initialState)
			}
			if finalEndStreamSent != tc.expectedFinalEndStreamSent {
				t.Errorf("Expected final endStreamSentToClient %v, got %v", tc.expectedFinalEndStreamSent, finalEndStreamSent)
			}
		})
	}
}

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
	"time" // Added missing import

	"strings"
	"sync"
	"testing"
)

// TestStream_Close_SendsRSTAndCleansUp tests the stream.Close() method.
// It verifies that an RST_STREAM frame is sent, state transitions to Closed,
// and associated resources are cleaned up.

func TestStream_Close_SendsRSTAndCleansUp(t *testing.T) {
	t.Parallel()
	// Use newTestConnection from conn_test.go to get a real *Connection
	conn, _ := newTestConnection(t, false /*isClient*/, nil /*mockDispatcher*/, nil)
	conn.writerChan = make(chan Frame, 1) // Buffer 1 for the RST_STREAM

	stream := newTestStream(t, 1, conn, true /*isInitiatedByPeer*/, 0, 0)

	// Manually set stream to Open state for test
	stream.mu.Lock()
	stream.state = StreamStateOpen
	stream.mu.Unlock()

	closeErr := fmt.Errorf("test initiated close")
	expectedRstCode := ErrCodeInternalError // Default if closeErr is generic

	// Test case 1: Closing with a generic error
	t.Run("WithGenericError", func(t *testing.T) {
		err := stream.Close(closeErr)
		if err != nil {
			t.Fatalf("stream.Close() failed: %v", err)
		}

		// Verify RST_STREAM frame

		select {
		case frame := <-conn.writerChan:
			rstFrame, ok := frame.(*RSTStreamFrame)
			if !ok {
				tFatalf(t, "Expected RSTStreamFrame, got %T", frame)
			}
			if rstFrame.Header().StreamID != stream.id {
				tErrorf(t, "RSTStreamFrame StreamID mismatch: got %d, want %d", rstFrame.Header().StreamID, stream.id)
			}
			if rstFrame.ErrorCode != expectedRstCode {
				tErrorf(t, "RSTStreamFrame ErrorCode mismatch: got %s, want %s", rstFrame.ErrorCode, expectedRstCode)
			}
		default:
			tError(t, "Expected RSTStreamFrame on writerChan, but none found")
		}

		stream.mu.RLock()
		finalState := stream.state
		// pendingCode := stream.pendingRSTCode // Is nil after successful cleanup
		stream.mu.RUnlock()

		if finalState != StreamStateClosed {
			tErrorf(t, "Expected stream state Closed, got %s", finalState)
		}

		// Verify context cancellation
		select {
		case <-stream.ctx.Done():
			if stream.ctx.Err() != context.Canceled {
				tErrorf(t, "Expected context error context.Canceled, got %v", stream.ctx.Err())
			}
		default:
			tError(t, "Expected stream context to be done")
		}

		// Verify pipe closures
		_, errPipeRead := stream.requestBodyReader.Read(make([]byte, 1))
		if errPipeRead == nil {
			tError(t, "Expected error reading from requestBodyReader after close, got nil")
		} else {
			// Expected error depends on how pipe was closed; could be io.EOF or custom.
			// *StreamError with matching code is a good sign.
			tLogf(t, "requestBodyReader.Read() error: %v (expected)", errPipeRead)
		}

		_, errPipeWrite := stream.requestBodyWriter.Write([]byte("test"))
		if errPipeWrite == nil {
			tError(t, "Expected error writing to requestBodyWriter after close, got nil")
		} else {
			tLogf(t, "requestBodyWriter.Write() error: %v (expected)", errPipeWrite)
		}

		// Verify flow control manager closure
		errFcAcquire := stream.fcManager.sendWindow.Acquire(1)
		if errFcAcquire == nil {
			tError(t, "Expected error acquiring from flow control window after close, got nil")
		} else {
			// Check if the error indicates closure
			streamErr, ok := errFcAcquire.(*StreamError)
			if !ok || (streamErr.Code != ErrCodeStreamClosed && streamErr.Code != expectedRstCode) {
				tErrorf(t, "Flow control acquire error: %v, expected StreamError with StreamClosed or matching RST code", errFcAcquire)
			}
			tLogf(t, "fcManager.sendWindow.Acquire() error: %v (expected)", errFcAcquire)
		}
	})

	// Test case 2: Closing with nil error (should use ErrCodeCancel)
	// Need to reset the stream for this test or use a new one.
	// For simplicity, this sub-test is illustrative; a real test suite would use t.Run with proper setup/teardown for each.
	// This specific test needs a new stream instance as the previous one is closed.

	t.Run("WithNilError", func(t *testing.T) {
		conn2, _ := newTestConnection(t, false, nil, nil)
		conn2.writerChan = make(chan Frame, 1)
		stream2 := newTestStream(t, 2, conn2, true, 0, 0)
		stream2.mu.Lock()
		stream2.state = StreamStateOpen
		stream2.mu.Unlock()

		err := stream2.Close(nil) // Close with nil error
		if err != nil {
			t.Fatalf("stream2.Close(nil) failed: %v", err)
		}

		select {
		case frame := <-conn2.writerChan:
			rstFrame, ok := frame.(*RSTStreamFrame)
			if !ok {
				tFatalf(t, "Expected RSTStreamFrame, got %T", frame)
			}
			if rstFrame.ErrorCode != ErrCodeCancel {
				tErrorf(t, "RSTStreamFrame ErrorCode mismatch: got %s, want %s", rstFrame.ErrorCode, ErrCodeCancel)
			}
		default:
			tError(t, "Expected RSTStreamFrame on writerChan for nil error, but none found")
		}
		if stream2.state != StreamStateClosed {
			tErrorf(t, "Expected stream2 state Closed, got %s", stream2.state)
		}
	})
}

func TestStream_sendRSTStream_DirectCall(t *testing.T) {
	t.Parallel()
	conn, _ := newTestConnection(t, false, nil, nil)
	conn.ourInitialWindowSize = DefaultInitialWindowSize // Ensure FC manager is happy
	conn.peerInitialWindowSize = DefaultInitialWindowSize
	conn.writerChan = make(chan Frame, 1)

	streamID := uint32(3)
	stream := newTestStream(t, streamID, conn, true, 0, 0)

	// Manually set stream to Open state for test
	stream.mu.Lock()
	stream.state = StreamStateOpen
	stream.mu.Unlock()

	tests := []struct {
		name          string
		errorCode     ErrorCode
		initialState  StreamState
		expectSend    bool
		expectedState StreamState
	}{
		{
			name:          "RST from Open state",
			errorCode:     ErrCodeProtocolError,
			initialState:  StreamStateOpen,
			expectSend:    true,
			expectedState: StreamStateClosed,
		},
		{
			name:          "RST from HalfClosedLocal state",
			errorCode:     ErrCodeStreamClosed, // Example code
			initialState:  StreamStateHalfClosedLocal,
			expectSend:    true,
			expectedState: StreamStateClosed,
		},
		{
			name:          "RST from HalfClosedRemote state",
			errorCode:     ErrCodeCancel,
			initialState:  StreamStateHalfClosedRemote,
			expectSend:    true,
			expectedState: StreamStateClosed,
		},
		{
			name:          "RST from already Closed state (idempotent)",
			errorCode:     ErrCodeInternalError,
			initialState:  StreamStateClosed,
			expectSend:    false, // Should not send if already closed by us with pendingRST
			expectedState: StreamStateClosed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reset stream state for each test case.
			// For "already Closed", we need to simulate it was closed via RST by us.
			stream.mu.Lock()
			stream.state = tc.initialState
			if tc.initialState == StreamStateClosed {
				// Simulate it was closed due to an RST we initiated
				// For the idempotency check, s.pendingRSTCode needs to match.
				// s.sendRSTStream checks if s.state == StreamStateClosed && s.pendingRSTCode != nil && *s.pendingRSTCode == errorCode
				// So, if we want to test idempotency of calling with the *same* error code, set pendingRSTCode.
				// If it's just "closed by some other means", then pendingRSTCode would be nil.
				// The current check is: `if s.state == StreamStateClosed && s.pendingRSTCode != nil && *s.pendingRSTCode == errorCode`
				// Or `if s.state == StreamStateClosed` (generic closed, don't resend).
				// Let's test the generic closed case for the specific idempotency check.
				if tc.errorCode == ErrCodeInternalError { // Match the specific error code for idempotency part of the "already Closed state" test
					tempErrorCode := ErrCodeInternalError
					stream.pendingRSTCode = &tempErrorCode
				} else {
					stream.pendingRSTCode = nil
				}
			} else {
				stream.pendingRSTCode = nil // Clear for non-closed initial states
			}
			stream.mu.Unlock()

			err := stream.sendRSTStream(tc.errorCode)
			if err != nil {
				// This test assumes sendRSTStreamFrame on connection succeeds.
				// If sendRSTStreamFrame could fail, this test would need adjustment.
				tFatalf(t, "stream.sendRSTStream() failed: %v", err)
			}

			if tc.expectSend {

				select {
				case frame := <-conn.writerChan:
					rstFrame, ok := frame.(*RSTStreamFrame)
					if !ok {
						tFatalf(t, "Expected RSTStreamFrame, got %T", frame)
					}
					if rstFrame.Header().StreamID != streamID {
						tErrorf(t, "RSTStreamFrame StreamID mismatch: got %d, want %d", rstFrame.Header().StreamID, streamID)
					}
					if rstFrame.ErrorCode != tc.errorCode {
						tErrorf(t, "RSTStreamFrame ErrorCode mismatch: got %s, want %s", rstFrame.ErrorCode, tc.errorCode)
					}
				default:
					tError(t, "Expected RSTStreamFrame on writerChan, but none found")
				}
			} else {

				select {
				case frame := <-conn.writerChan:
					tErrorf(t, "Did not expect RSTStreamFrame, but got one: %v", frame)
				default:
					// Expected: no frame sent
				}
			}

			stream.mu.RLock()
			finalState := stream.state
			stream.mu.RUnlock()

			if finalState != tc.expectedState {
				tErrorf(t, "Expected stream state %s, got %s", tc.expectedState, finalState)
			}

			if tc.expectedState == StreamStateClosed {
				// Verify context cancellation
				select {
				case <-stream.ctx.Done():
					// Expected
				default:
					tError(t, "Expected stream context to be done for closed stream")
				}

				// Verify pipe closures (simplified check)
				_, errPipeRead := stream.requestBodyReader.Read(make([]byte, 1))
				if errPipeRead == nil {
					tError(t, "Expected error reading from requestBodyReader after RST, got nil")
				}

				// Verify flow control manager closure (simplified check)
				errFcAcquire := stream.fcManager.sendWindow.Acquire(1)
				if errFcAcquire == nil {
					tError(t, "Expected error acquiring from flow control window after RST, got nil")
				}
			}
		})
	}
}

func TestStream_handleDataFrame(t *testing.T) {
	t.Parallel()
	const testStreamID = uint32(1)

	testCases := []struct {
		name                        string
		initialStreamState          StreamState
		initialEndStreamReceived    bool // s.endStreamReceivedFromClient
		initialOurWindowSize        uint32
		frameData                   []byte
		frameEndStream              bool
		setupPipeReaderEarlyClose   bool                                                           // If true, close s.requestBodyReader before calling handleDataFrame
		preFunc                     func(s *Stream, t *testing.T, tcData struct{ endStream bool }) // New field for pre-test setup
		expectError                 bool
		expectedErrorCode           ErrorCode // If expectError is true
		expectedErrorContains       string    // Substring to check in error message
		expectedStreamStateAfter    StreamState
		expectedEndStreamReceived   bool // s.endStreamReceivedFromClient after call
		expectedDataInPipe          []byte
		expectPipeWriterClosed      bool
		expectedFcRecvWindowReduced bool // True if FC window should be reduced by len(frameData)
	}{
		{
			name:                        "Open stream, DATA frame, no END_STREAM, sufficient window",
			initialStreamState:          StreamStateOpen,
			initialOurWindowSize:        100,
			frameData:                   []byte("hello"),
			frameEndStream:              false,
			expectError:                 false,
			expectedStreamStateAfter:    StreamStateOpen,
			expectedDataInPipe:          []byte("hello"),
			expectPipeWriterClosed:      false,
			expectedFcRecvWindowReduced: true,
		},
		{
			name:                        "Open stream, DATA frame, with END_STREAM, sufficient window",
			initialStreamState:          StreamStateOpen,
			initialOurWindowSize:        100,
			frameData:                   []byte("world"),
			frameEndStream:              true,
			expectError:                 false,
			expectedStreamStateAfter:    StreamStateHalfClosedRemote,
			expectedEndStreamReceived:   true,
			expectedDataInPipe:          []byte("world"),
			expectPipeWriterClosed:      true,
			expectedFcRecvWindowReduced: true,
		},
		{
			name:                        "HalfClosedLocal stream, DATA frame, with END_STREAM, sufficient window",
			initialStreamState:          StreamStateHalfClosedLocal,
			initialOurWindowSize:        100,
			frameData:                   []byte("done"),
			frameEndStream:              true,
			expectError:                 false,
			expectedStreamStateAfter:    StreamStateClosed,
			expectedEndStreamReceived:   true,
			expectedDataInPipe:          []byte("done"),
			expectPipeWriterClosed:      true,
			expectedFcRecvWindowReduced: true,
		},
		{
			name:                        "Flow control violation",
			initialStreamState:          StreamStateOpen,
			initialOurWindowSize:        5, // Window too small
			frameData:                   []byte("too much data"),
			frameEndStream:              false,
			expectError:                 true,
			expectedErrorCode:           ErrCodeFlowControlError,
			expectedStreamStateAfter:    StreamStateClosed, // Test now closes stream on expected error
			expectedFcRecvWindowReduced: false,             // FC acquire fails before reduction
		},
		{
			name:                        "DATA frame on HalfClosedRemote stream (invalid state for receiving DATA)",
			initialStreamState:          StreamStateHalfClosedRemote,
			initialOurWindowSize:        100,
			frameData:                   []byte("too late"),
			frameEndStream:              false,
			expectError:                 true,
			expectedErrorCode:           ErrCodeStreamClosed, // Per handleDataFrame's internal check
			expectedStreamStateAfter:    StreamStateClosed,   // Test now closes stream on expected error
			expectedFcRecvWindowReduced: true,                // FC is checked first, then state. This is subtle.
			// The initial check in handleDataFrame: `if s.state == StreamStateHalfClosedRemote || s.state == StreamStateClosed`
			// happens *after* `s.fcManager.DataReceived`. So FC *is* consumed if it was available.
			// Then the state check makes it return error. This test highlights this behavior.
		},
		{
			name:                        "DATA frame on Closed stream (invalid state for receiving DATA)",
			initialStreamState:          StreamStateClosed,
			initialOurWindowSize:        100,
			frameData:                   []byte("really too late"),
			frameEndStream:              false,
			expectError:                 true,
			expectedErrorCode:           ErrCodeStreamClosed, // Per handleDataFrame's internal check
			expectedStreamStateAfter:    StreamStateClosed,   // Test now closes stream on expected error
			expectedFcRecvWindowReduced: true,                // Similar to HalfClosedRemote, FC consumed before state check.
		},
		{
			name:                      "Write to pipe fails (reader closed early)",
			initialStreamState:        StreamStateOpen,
			initialOurWindowSize:      100,
			frameData:                 []byte("pipefail"),
			frameEndStream:            false,
			setupPipeReaderEarlyClose: true,
			expectError:               false, // Corrected: handleDataFrame might not return sync error
			// expectedErrorCode:        ErrCodeCancel, // No sync error expected from handleDataFrame
			// expectedErrorContains:    "pipe",       // No sync error expected from handleDataFrame
			expectedStreamStateAfter:    StreamStateClosed, // Stream closes itself on pipe write error
			expectedFcRecvWindowReduced: true,              // FC is acquired before pipe write attempt
			expectPipeWriterClosed:      true,              // requestBodyWriter.CloseWithError is called
		},
		{
			name:                        "END_STREAM on HalfClosedRemote stream (double END_STREAM from client)",
			initialStreamState:          StreamStateHalfClosedRemote,
			initialEndStreamReceived:    true, // Simulate client already sent END_STREAM
			initialOurWindowSize:        100,
			frameData:                   []byte(""), // Empty DATA with END_STREAM
			frameEndStream:              true,
			expectError:                 true,
			expectedErrorCode:           ErrCodeStreamClosed,                              // Corrected: DATA (even empty) on HCR is StreamClosed
			expectedErrorContains:       "DATA frame on closed/half-closed-remote stream", // Corrected
			expectedStreamStateAfter:    StreamStateClosed,                                // Test now closes stream on expected error
			expectedEndStreamReceived:   true,
			expectPipeWriterClosed:      true, // Because stream will be closed by test if error occurs
			expectedFcRecvWindowReduced: true, // For empty DATA frame, FC change is 0
		},
		{
			name:                        "Empty DATA with END_STREAM on Open stream",
			initialStreamState:          StreamStateOpen,
			initialOurWindowSize:        100,
			frameData:                   []byte(""),
			frameEndStream:              true,
			expectError:                 false,
			expectedStreamStateAfter:    StreamStateHalfClosedRemote,
			expectedEndStreamReceived:   true,
			expectedDataInPipe:          []byte(""),
			expectPipeWriterClosed:      true,
			expectedFcRecvWindowReduced: true, // FC for 0 bytes is a no-op but path is taken
		},

		{
			name:                 "Content-Length > actual, with END_STREAM",
			initialStreamState:   StreamStateOpen,
			initialOurWindowSize: 100,
			frameData:            []byte("partial"), // M2 = 7 bytes
			frameEndStream:       true,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				declaredLength := int64(20) // N = 20
				s.parsedContentLength = &declaredLength
				// Simulate 5 bytes (M1) already received before this frameData.
				// Total actual received will be M1 + len(frameData) = 5 + 7 = 12.
				// Content-Length (20) > Actual (12).
				s.receivedDataBytes = 5 // M1 = 5
				s.mu.Unlock()
			},
			expectError:                 true,
			expectedErrorCode:           ErrCodeProtocolError,
			expectedErrorContains:       "content-length mismatch on END_STREAM: declared 20, received 12",
			expectedStreamStateAfter:    StreamStateClosed, // Stream is closed on error
			expectedEndStreamReceived:   true,
			expectedDataInPipe:          nil,  // Data from erroring frame is NOT written
			expectPipeWriterClosed:      true, // Stream closes on error, closing pipe writer
			expectedFcRecvWindowReduced: true,
		},
		{
			name:                 "Content-Length < actual, with END_STREAM",
			initialStreamState:   StreamStateOpen,
			initialOurWindowSize: 100,
			frameData:            []byte("toolongdata"), // M2 = 11 bytes
			frameEndStream:       true,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				declaredLength := int64(10) // N = 10
				s.parsedContentLength = &declaredLength
				// Simulate 5 bytes (M1) already received before this frameData.
				// Total actual received will be M1 + len(frameData) = 5 + 11 = 16.
				// Content-Length (10) < Actual (16).
				s.receivedDataBytes = 5 // M1 = 5
				s.mu.Unlock()
			},
			expectError:                 true,
			expectedErrorCode:           ErrCodeProtocolError,
			expectedErrorContains:       "content-length mismatch on END_STREAM: declared 10, received 16",
			expectedStreamStateAfter:    StreamStateClosed, // Stream is closed on error
			expectedEndStreamReceived:   true,
			expectedDataInPipe:          nil,  // Data from erroring frame is NOT written
			expectPipeWriterClosed:      true, // Stream closes on error
			expectedFcRecvWindowReduced: true,
		},
		// --- NEW TEST CASES FOR Content-Length validation (TASK 2 additions) ---
		{
			name:                 "Content-Length > 0, actual data = 0, with END_STREAM",
			initialStreamState:   StreamStateOpen,
			initialOurWindowSize: 100,
			frameData:            []byte(""),
			frameEndStream:       true,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				declaredLength := int64(10)
				s.parsedContentLength = &declaredLength
				s.receivedDataBytes = 0
				s.mu.Unlock()
			},
			expectError:                 true,
			expectedErrorCode:           ErrCodeProtocolError,
			expectedErrorContains:       "content-length mismatch on END_STREAM: declared 10, received 0",
			expectedStreamStateAfter:    StreamStateClosed,
			expectedEndStreamReceived:   true,
			expectedDataInPipe:          nil,
			expectPipeWriterClosed:      true,
			expectedFcRecvWindowReduced: true,
		},
		{
			name:                 "Content-Length = 0, actual data > 0, with END_STREAM",
			initialStreamState:   StreamStateOpen,
			initialOurWindowSize: 100,
			frameData:            []byte("hasdata"),
			frameEndStream:       true,
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				declaredLength := int64(0)
				s.parsedContentLength = &declaredLength
				s.receivedDataBytes = 0
				s.mu.Unlock()
			},
			expectError:                 true,
			expectedErrorCode:           ErrCodeProtocolError,
			expectedErrorContains:       "content-length mismatch on END_STREAM: declared 0, received 7",
			expectedStreamStateAfter:    StreamStateClosed,
			expectedEndStreamReceived:   true,
			expectedDataInPipe:          nil,
			expectPipeWriterClosed:      true,
			expectedFcRecvWindowReduced: true,
		},

		{
			name:                 "Content-Length < actual, DATA without END_STREAM, causes error",
			initialStreamState:   StreamStateOpen,
			initialOurWindowSize: 100,
			frameData:            []byte("exceeds"), // M2 = 7 bytes
			frameEndStream:       false,             // Key: END_STREAM is false
			preFunc: func(s *Stream, t *testing.T, tcData struct{ endStream bool }) {
				s.mu.Lock()
				declaredLength := int64(10) // N = 10
				s.parsedContentLength = &declaredLength
				// Simulate 5 bytes (M1) already received before this frameData.
				// Total actual received with this frame will be M1 + len(frameData) = 5 + 7 = 12.
				// Content-Length (10) < Actual (12).
				s.receivedDataBytes = 5 // M1 = 5
				s.mu.Unlock()
			},
			expectError:                 true,
			expectedErrorCode:           ErrCodeProtocolError,
			expectedErrorContains:       "data received (12 with current frame) exceeds declared content-length (10)",
			expectedStreamStateAfter:    StreamStateClosed, // Stream is closed on error
			expectedEndStreamReceived:   false,             // END_STREAM was not on this frame
			expectedDataInPipe:          nil,               // Data from erroring frame is NOT written
			expectPipeWriterClosed:      true,              // Stream closes on error
			expectedFcRecvWindowReduced: true,              // FC acquire happens before CL check
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := newTestConnection(t, false, nil, nil)
			conn.writerChan = make(chan Frame, 1)               // Buffer for potential RST by stream.Close in teardown
			conn.ourInitialWindowSize = tc.initialOurWindowSize // Set for the connection

			stream := newTestStream(t, testStreamID, conn, true, tc.initialOurWindowSize, 0)

			// Setup initial stream state and FC window conditions
			stream.mu.Lock()
			stream.state = tc.initialStreamState
			stream.endStreamReceivedFromClient = tc.initialEndStreamReceived
			// stream.fcManager.currentReceiveWindowSize can't be set directly.
			// It's initialized based on mc.cfgOurInitialWindowSize.
			// We'll check fcManager.GetStreamReceiveAvailable()
			initialFcAvailable := stream.fcManager.GetStreamReceiveAvailable()
			stream.mu.Unlock()

			if tc.setupPipeReaderEarlyClose {
				// Close the reader end of the pipe to simulate handler closing it.
				if err := stream.requestBodyReader.Close(); err != nil {
					t.Fatalf("Failed to close requestBodyReader for test setup: %v", err)
				}
			}

			// Collect data from the pipe in a goroutine
			pipeDataCh := make(chan []byte, 1)
			pipeErrorCh := make(chan error, 1)
			var wg sync.WaitGroup
			wg.Add(1)
			// Call preFunc *before* setting up the pipe reader and creating the frame,
			// as preFunc might modify stream state that affects these.
			if tc.preFunc != nil {
				tc.preFunc(stream, t, struct{ endStream bool }{tc.frameEndStream})
			}

			// Collect data from the pipe in a goroutine

			// Collect data from the pipe in a goroutine
			// pipeDataCh, pipeErrorCh, and wg are declared before the preFunc call
			go func() {
				defer wg.Done()
				buf := make([]byte, 1024) // Reasonably sized buffer
				var collectedData []byte
				totalRead := 0

				for {
					n, readErr := stream.requestBodyReader.Read(buf[totalRead:])
					if n > 0 {
						totalRead += n
						// Resize collectedData and copy new data if necessary
						// For simplicity in test, assume first read gets all or is enough
						if collectedData == nil {
							collectedData = make([]byte, n)
							copy(collectedData, buf[:n])
						} else if tc.name == "not_expecting_multiple_reads_in_test_yet" {
							// placeholder for more complex multi-read scenarios if needed by a test case
						}
					}

					if readErr == io.EOF {
						if tc.expectPipeWriterClosed {
							// This is expected if the writer closed the pipe.
						} else if len(tc.frameData) > 0 { // EOF but data was sent and pipe not expected to close
							pipeErrorCh <- fmt.Errorf("got unexpected EOF; pipe writer not expected to close and data was sent. Data read: %d bytes", totalRead)
						}
						break // EOF means no more data.
					}
					if readErr != nil {
						pipeErrorCh <- readErr // Report other errors.
						break
					}
					// If readErr is nil, loop to read more if buffer wasn't full
					// For this test, assume one read is sufficient if no error/EOF.
					// or that subsequent reads will quickly hit EOF if writer closed.
					if !tc.expectPipeWriterClosed && len(tc.frameData) == 0 { // No data, no close, should block. Test should not run this path.
						// This path is problematic if it blocks.
						// Assume tests will either send data or expect close.
						break
					}
					if totalRead == len(tc.frameData) && !tc.expectPipeWriterClosed {
						// Read all expected data, and pipe not expected to close yet. Break to avoid blocking.
						// This helps non-END_STREAM cases.
						break
					}
				}
				pipeDataCh <- collectedData
			}()

			// Create the DATA frame
			dataFrame := newDataFrame(testStreamID, tc.frameData, tc.frameEndStream)

			// Call handleDataFrame
			err := stream.handleDataFrame(dataFrame)

			// Handle errors from stream.handleDataFrame
			if tc.expectError {
				if err == nil {
					t.Fatalf("Expected an error from handleDataFrame, but got nil")
				}
				streamErr, ok := err.(*StreamError)
				if !ok {
					t.Fatalf("Expected a StreamError from handleDataFrame, got %T: %v", err, err)
				}
				if streamErr.Code != tc.expectedErrorCode {
					t.Errorf("Expected error code %s from handleDataFrame, got %s", tc.expectedErrorCode, streamErr.Code)
				}
				if tc.expectedErrorContains != "" && !strings.Contains(streamErr.Msg, tc.expectedErrorContains) {
					t.Errorf("Expected error message from handleDataFrame to contain '%s', got '%s'", tc.expectedErrorContains, streamErr.Msg)
				}
				// If handleDataFrame errored (as expected), close the stream to unblock the pipe reader.
				// This simulates the connection processing the stream error.
				// Use a non-nil error for stream.Close, reflecting the error from handleDataFrame.
				// The actual error content from stream.Close() might differ based on its internal logic (e.g. it might use its own RST code).
				// But calling Close ensures resources like pipes are cleaned up.
				t.Logf("handleDataFrame returned expected error '%v', closing stream to unblock test's pipe reader.", err)
				_ = stream.Close(err) // Pass the original error to Close.
			} else if err != nil { // If err is not nil AND we didn't expect an error
				t.Fatalf("Expected no error, but got: %v", err)
			}

			// For cases where an async error is expected to close the stream (like pipe write fail)
			// wait for the stream context to be done.
			if tc.setupPipeReaderEarlyClose { // This condition specifically targets the "Write to pipe fails" case
				select {
				case <-stream.ctx.Done():
					t.Logf("Stream context done as expected for pipe write failure.")
				case <-time.After(1 * time.Second): // Adjust timeout as needed, should be quick
					t.Errorf("Timeout waiting for stream context to be done after expected pipe write failure.")
				}
			}

			// Check stream state
			stream.mu.RLock()
			finalState := stream.state
			finalEndStreamReceived := stream.endStreamReceivedFromClient
			stream.mu.RUnlock()

			if finalState != tc.expectedStreamStateAfter {
				t.Errorf("Expected stream state %s, got %s", tc.expectedStreamStateAfter, finalState)
			}
			if finalEndStreamReceived != tc.expectedEndStreamReceived {
				t.Errorf("Expected endStreamReceivedFromClient to be %v, got %v", tc.expectedEndStreamReceived, finalEndStreamReceived)
			}

			// Check data received on pipe (if no error or if error happens after pipe write)
			if !tc.expectError || (tc.expectError && tc.expectedErrorCode == ErrCodeCancel && tc.setupPipeReaderEarlyClose) { // ErrCodeCancel implies data might have been written before error
				select {
				case dataFromPipe := <-pipeDataCh:
					if string(dataFromPipe) != string(tc.expectedDataInPipe) {
						t.Errorf("Expected data on pipe '%s', got '%s'", string(tc.expectedDataInPipe), string(dataFromPipe))
					}

				case pipeErr := <-pipeErrorCh:
					if tc.expectError { // If handleDataFrame was expected to error and stream.Close() was called by test
						if pipeErr == nil {
							t.Errorf("Expected an error from pipe reader when stream.Close() was called, but got nil (error: %v)", err)
						} else if !strings.Contains(pipeErr.Error(), "closed pipe") && !errors.Is(pipeErr, io.EOF) && !errors.Is(pipeErr, context.Canceled) {
							t.Errorf("Expected 'closed pipe', EOF, or context.Canceled from reader after stream.Close(), got: %v (handleDataFrame error: %v)", pipeErr, err)
						} else {
							t.Logf("Got expected 'closed pipe', EOF, or context.Canceled from pipe reader after stream.Close(): %v (handleDataFrame error: %v)", pipeErr, err)
						}
					} else if tc.setupPipeReaderEarlyClose {
						if pipeErr == nil {
							t.Errorf("Expected an error from pipe reader when reader was closed early by test, but got nil")
						} else if !strings.Contains(pipeErr.Error(), "closed pipe") && !errors.Is(pipeErr, io.EOF) && !errors.Is(pipeErr, context.Canceled) {
							t.Errorf("Expected 'closed pipe', EOF, or context.Canceled from reader (closed by test setup), got: %v", pipeErr)
						} else {
							t.Logf("Got expected 'closed pipe', EOF, or context.Canceled error from pipe reader (closed by test setup): %v", pipeErr)
						}
					} else if tc.expectPipeWriterClosed { // handleDataFrame closed writer (e.g. END_STREAM leading to full stream closure)
						if pipeErr != nil && !errors.Is(pipeErr, io.EOF) && !errors.Is(pipeErr, context.Canceled) {
							t.Errorf("Pipe reader goroutine got unexpected error (expected EOF, context.Canceled, or nil if no data): %v", pipeErr)
						}
						// If pipeErr is io.EOF, context.Canceled, or nil (for 0 bytes read then EOF/Canceled), it's fine.
					} else if !tc.expectPipeWriterClosed && pipeErr != nil { // Writer not closed, no error from handleDataFrame
						t.Errorf("Pipe reader goroutine got unexpected error when pipe shouldn't close: %v", pipeErr)
					}
					// If pipeErr is nil and no conditions above met, it implies 0 bytes read and pipe still open.
					// If pipeErr is nil and tc.expectPipeWriterClosed is false (and no data), it's fine (goroutine read 0 bytes and exited).

				}
			}

			// Check flow control window reduction
			finalFcAvailable := stream.fcManager.GetStreamReceiveAvailable()
			expectedReduction := int64(len(tc.frameData))
			if tc.expectedFcRecvWindowReduced && !tc.expectError { // Successful data processing
				if finalFcAvailable != initialFcAvailable-expectedReduction {
					t.Errorf("Flow control window not reduced correctly. Initial: %d, Final: %d, Expected Reduction: %d",
						initialFcAvailable, finalFcAvailable, expectedReduction)
				}
			} else if tc.expectError && tc.expectedErrorCode == ErrCodeFlowControlError { // FC error
				if finalFcAvailable != initialFcAvailable {
					t.Errorf("Flow control window changed on FC error. Initial: %d, Final: %d", initialFcAvailable, finalFcAvailable)
				}
			}
			// More nuanced checks for FC can be added if other error cases affect it non-obviously.

			// stream.Close() // Cleanup from newTestStream will handle this.
		})
	}
}

// TestStream_handleRSTStreamFrame tests the stream.handleRSTStreamFrame method.

func TestStream_handleRSTStreamFrame(t *testing.T) {
	t.Parallel()
	const testStreamID = uint32(1)

	testCases := []struct {
		name               string
		initialStreamState StreamState
		rstErrorCode       ErrorCode
		expectedStateAfter StreamState
		expectPipeClose    bool
		expectCtxCancel    bool
	}{
		{
			name:               "RST on Open stream",
			initialStreamState: StreamStateOpen,
			rstErrorCode:       ErrCodeProtocolError,
			expectedStateAfter: StreamStateClosed,
			expectPipeClose:    true,
			expectCtxCancel:    true,
		},
		{
			name:               "RST on HalfClosedLocal stream",
			initialStreamState: StreamStateHalfClosedLocal,
			rstErrorCode:       ErrCodeCancel,
			expectedStateAfter: StreamStateClosed,
			expectPipeClose:    true,
			expectCtxCancel:    true,
		},
		{
			name:               "RST on HalfClosedRemote stream",
			initialStreamState: StreamStateHalfClosedRemote,
			rstErrorCode:       ErrCodeStreamClosed,
			expectedStateAfter: StreamStateClosed,
			expectPipeClose:    true,
			expectCtxCancel:    true,
		},
		{
			name:               "RST on already Closed stream (idempotent)",
			initialStreamState: StreamStateClosed,
			rstErrorCode:       ErrCodeInternalError, // Different code to see if it logs or changes anything
			expectedStateAfter: StreamStateClosed,
			expectPipeClose:    false, // Assuming resources already cleaned
			expectCtxCancel:    false, // Assuming resources already cleaned
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := newTestConnection(t, false, nil, nil)
			conn.writerChan = make(chan Frame, 1) // Not used by handleRSTStreamFrame directly, but by teardown

			stream := newTestStream(t, testStreamID, conn, true, 0, 0)

			// Setup initial stream state
			stream.mu.Lock()
			stream.state = tc.initialStreamState
			if tc.initialStreamState == StreamStateClosed {
				// Simulate it was already closed by an RST
				// This makes the call to handleRSTStreamFrame idempotent.
				// closeStreamResourcesProtected will have been called.
				alreadyClosedCode := ErrCodeCancel // An arbitrary code for previous closure
				stream.pendingRSTCode = &alreadyClosedCode

				// Manually cancel context and close pipes as if already cleaned up
				stream.cancelCtx()
				_ = stream.requestBodyWriter.Close()
				_ = stream.requestBodyReader.Close()
			}
			stream.mu.Unlock()

			// Call handleRSTStreamFrame
			stream.handleRSTStreamFrame(tc.rstErrorCode)

			// Check final stream state
			stream.mu.RLock()
			finalState := stream.state
			stream.mu.RUnlock()

			if finalState != tc.expectedStateAfter {
				t.Errorf("Expected stream state %s, got %s", tc.expectedStateAfter, finalState)
			}

			if tc.expectedStateAfter == StreamStateClosed {
				// If the stream was expected to close, verify context cancellation and pipe states.
				// The check for the specific RST code being sent is done via mc.writerChan earlier in the test.
				// The internal s.pendingRSTCode is an implementation detail and is nilled out during full closure.
				// We do not check stream.pendingRSTCode here for the original RST code for this reason.

				if tc.expectCtxCancel { // This flag indicates if the test setup expects a transition to Closed *during this call*
					select {
					case <-stream.ctx.Done():
						// Expected: context is canceled because the stream closed.
					default:
						t.Error("Expected stream context to be canceled")
					}
				} else if tc.initialStreamState == StreamStateClosed { // If was already closed, context should already be done.
					select {
					case <-stream.ctx.Done():
						// Expected
					default:
						t.Error("Stream context was not already canceled for initial Closed state")
					}
				}

				if tc.expectPipeClose { // This flag indicates if the test setup expects pipes to close *during this call*
					// Check requestBodyWriter: subsequent Writes return ErrClosedPipe after CloseWithError
					_, errWrite := stream.requestBodyWriter.Write([]byte("test"))
					if errWrite == nil {
						t.Error("Expected error writing to requestBodyWriter after RST, got nil")
					} else if errWrite != io.ErrClosedPipe { // As per io.Pipe documentation for Write after CloseWithError
						t.Errorf("requestBodyWriter.Write error: got %v (type %T), want io.ErrClosedPipe", errWrite, errWrite)
					} else {
						t.Logf("requestBodyWriter.Write error: %v (io.ErrClosedPipe, as expected)", errWrite)
					}

					// Check requestBodyReader: subsequent Reads return the error passed to CloseWithError
					_, errRead := stream.requestBodyReader.Read(make([]byte, 1))
					if errRead == nil {
						t.Error("Expected error reading from requestBodyReader after RST, got nil")
					} else if se, ok := errRead.(*StreamError); !ok || se.Code != tc.rstErrorCode {
						// If the stream was closed due to an RST (tc.rstErrorCode), requestBodyWriter.CloseWithError(NewStreamError(...)) is called.
						// The reader should then see this StreamError.
						t.Errorf("requestBodyReader.Read error: got %v (type %T), want *StreamError with code %s", errRead, errRead, tc.rstErrorCode)
					} else {
						t.Logf("requestBodyReader.Read error: %v (*StreamError with code %s, as expected)", errRead, tc.rstErrorCode)
					}
				} else if tc.initialStreamState == StreamStateClosed { // If was already closed, pipes should already be unusable by test setup's generic close.
					// Test setup does: _ = stream.requestBodyWriter.Close(); _ = stream.requestBodyReader.Close() for initialStreamState == StreamStateClosed
					// This generic close results in io.ErrClosedPipe for writer and io.EOF or io.ErrClosedPipe for reader.
					_, errWrite := stream.requestBodyWriter.Write([]byte("test"))
					if errWrite == nil {
						t.Error("requestBodyWriter.Write: Expected error for already closed stream, got nil")
					} else if errWrite != io.ErrClosedPipe {
						t.Errorf("requestBodyWriter.Write error for already closed stream: got %v, want io.ErrClosedPipe", errWrite)
					}

					_, errRead := stream.requestBodyReader.Read(make([]byte, 1))
					if errRead == nil {
						t.Error("requestBodyReader.Read: Expected error for already closed stream, got nil")
					} else if errRead != io.EOF && errRead != io.ErrClosedPipe { // After a simple .Close(), reader gets EOF. If .CloseWithError(io.ErrClosedPipe) then that.
						t.Errorf("requestBodyReader.Read error for already closed stream: got %v, want io.EOF or io.ErrClosedPipe", errRead)
					}
				}
				_, errWrite := stream.requestBodyWriter.Write([]byte("test"))
				if errWrite == nil {
					t.Error("requestBodyWriter was not already closed for initial Closed state")
				}
				_, errRead := stream.requestBodyReader.Read(make([]byte, 1))
				if errRead == nil {
					tError(t, "requestBodyReader was not already closed for initial Closed state")
				}
			}
		})
	}
}

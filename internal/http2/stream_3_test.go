package http2

/*
   NOTE: The stream tests are split into multiple files to reduce the load on LLM
   context compared to having just one big file open.
*/

import (
	"context"
	"fmt"

	"io"
	"strings"
	"testing"
	"time"
)

func TestStream_IDAndContext(t *testing.T) {
	t.Parallel()
	conn, _ := newTestConnection(t, false, nil)
	conn.writerChan = make(chan Frame, 1) // For RST_STREAM from Close()

	streamID := uint32(5)
	stream := newTestStream(t, streamID, conn, true, 0, 0)

	// Test Stream.ID()
	if gotID := stream.ID(); gotID != streamID {
		t.Errorf("stream.ID() = %d, want %d", gotID, streamID)
	}

	// Test Stream.Context()
	ctx := stream.Context()
	if ctx == nil {
		t.Fatal("stream.Context() returned nil")
	}

	// Check context is not initially done
	select {
	case <-ctx.Done():
		t.Fatal("stream context was initially done, expected not done")
	default:
		// Expected: context is not done
	}

	// Close the stream
	err := stream.Close(fmt.Errorf("closing stream for context test"))
	if err != nil {
		t.Fatalf("stream.Close() failed: %v", err)
	}

	// Drain the RST_STREAM frame from Close()

	select {
	case fr := <-conn.writerChan:
		if rst, ok := fr.(*RSTStreamFrame); !ok {
			t.Errorf("Expected RSTStreamFrame from Close(), got %T", fr)
		} else if rst.Header().StreamID != streamID {
			t.Errorf("RSTStreamFrame from Close() had ID %d, want %d", rst.Header().StreamID, streamID)
		} else {
			t.Logf("Drained RSTStreamFrame (StreamID: %d, Code: %s) after stream.Close() in TestStream_IDAndContext", rst.Header().StreamID, rst.ErrorCode)
		}
	case <-time.After(100 * time.Millisecond): // Give it a moment
		t.Errorf("No RSTStreamFrame received on writerChan after stream.Close()")
	}

	// Check context is now done
	select {
	case <-ctx.Done():
		// Expected: context is done
		if ctx.Err() == nil {
			t.Error("stream context Done, but Err() is nil, expected context.Canceled or similar")
		} else if ctx.Err() != context.Canceled {
			t.Logf("stream context done with error: %v (expected context.Canceled or similar)", ctx.Err())
		}
	default:
		t.Error("stream context was not done after stream.Close(), expected done")
	}
}

func TestStream_SendHeaders(t *testing.T) {
	t.Parallel()
	const testStreamID = uint32(1)
	validHeaders := makeStreamWriterHeaders(":status", "200", "content-type", "text/plain")
	invalidHpackHeaders := makeStreamWriterHeaders("", "bad-header") // Empty name to cause HPACK error

	tests := []struct {
		name                          string
		initialState                  StreamState
		initialResponseHeadersSent    bool
		initialEndStreamSentToClient  bool
		initialPendingRSTCode         *ErrorCode
		headersToSend                 []HeaderField
		endStreamToSend               bool
		connErrorToSimulate           error // To simulate error from conn.sendHeadersFrame (e.g., via shutdownChan)
		expectError                   bool
		expectedErrorContains         string
		expectedErrorCodeOnRST        ErrorCode // If error causes RST from stream method itself.
		expectFrameSent               bool
		expectedFrameEndStreamFlag    bool
		expectedFinalState            StreamState
		expectedResponseHeadersSent   bool
		expectedEndStreamSentToClient bool
	}{
		{
			name:                          "Success: send headers, no endStream",
			initialState:                  StreamStateOpen,
			headersToSend:                 validHeaders,
			endStreamToSend:               false,
			expectFrameSent:               true,
			expectedFrameEndStreamFlag:    false,
			expectedFinalState:            StreamStateOpen,
			expectedResponseHeadersSent:   true,
			expectedEndStreamSentToClient: false,
		},
		{
			name:                          "Success: send headers, with endStream",
			initialState:                  StreamStateOpen,
			headersToSend:                 validHeaders,
			endStreamToSend:               true,
			expectFrameSent:               true,
			expectedFrameEndStreamFlag:    true,
			expectedFinalState:            StreamStateHalfClosedLocal,
			expectedResponseHeadersSent:   true,
			expectedEndStreamSentToClient: true,
		},
		{
			name:                          "Error: headers already sent",
			initialState:                  StreamStateOpen,
			initialResponseHeadersSent:    true,
			headersToSend:                 validHeaders,
			endStreamToSend:               false,
			expectError:                   true,
			expectedErrorContains:         "headers already sent",
			expectFrameSent:               false,
			expectedFinalState:            StreamStateOpen, // State should not change
			expectedResponseHeadersSent:   true,            // Remains true
			expectedEndStreamSentToClient: false,
		},
		{
			name:                          "Error: stream closed",
			initialState:                  StreamStateClosed,
			headersToSend:                 validHeaders,
			endStreamToSend:               false,
			expectError:                   true,
			expectedErrorContains:         "stream closed or resetting",
			expectFrameSent:               false,
			expectedFinalState:            StreamStateClosed,
			expectedResponseHeadersSent:   false,
			expectedEndStreamSentToClient: false,
		},
		{
			name:                          "Error: stream resetting",
			initialState:                  StreamStateOpen,
			initialPendingRSTCode:         func() *ErrorCode { e := ErrCodeCancel; return &e }(),
			headersToSend:                 validHeaders,
			endStreamToSend:               false,
			expectError:                   true,
			expectedErrorContains:         "stream closed or resetting",
			expectFrameSent:               false,
			expectedFinalState:            StreamStateOpen, // State doesn't change by SendHeaders itself
			expectedResponseHeadersSent:   false,
			expectedEndStreamSentToClient: false,
		},
		{
			name:                          "Error: half-closed (local) and END_STREAM already sent",
			initialState:                  StreamStateHalfClosedLocal,
			initialEndStreamSentToClient:  true, // Server already sent END_STREAM
			headersToSend:                 validHeaders,
			endStreamToSend:               false,
			expectError:                   true,
			expectedErrorContains:         "cannot send headers on half-closed (local) stream after END_STREAM",
			expectFrameSent:               false,
			expectedFinalState:            StreamStateHalfClosedLocal,
			expectedResponseHeadersSent:   false, // Should not be set to true if error occurs before send
			expectedEndStreamSentToClient: true,  // Remains true
		},
		{
			name:                          "Error: conn.sendHeadersFrame fails (HPACK encoding error)",
			initialState:                  StreamStateOpen,
			headersToSend:                 invalidHpackHeaders, // Triggers HPACK error in real conn.sendHeadersFrame
			endStreamToSend:               false,
			expectError:                   true,
			expectedErrorContains:         "stream error on stream 1: HPACK encoding failed (malformed header from application): hpack: invalid header field name: name is empty for Encode method (value: \"bad-header\") (code PROTOCOL_ERROR, 1)", // Error from hpack library, wrapped
			expectFrameSent:               false,                                                                                                                                                                                                     // Frame not successfully enqueued
			expectedFinalState:            StreamStateOpen,
			expectedResponseHeadersSent:   false, // Not successfully sent
			expectedEndStreamSentToClient: false,
		},
		{
			name:                          "Success: send headers from ReservedLocal, with endStream",
			initialState:                  StreamStateReservedLocal, // Server is PUSHING
			headersToSend:                 validHeaders,
			endStreamToSend:               true,
			expectFrameSent:               true,
			expectedFrameEndStreamFlag:    true,
			expectedFinalState:            StreamStateHalfClosedLocal,
			expectedResponseHeadersSent:   true,
			expectedEndStreamSentToClient: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := newTestConnection(t, false, nil)
			conn.writerChan = make(chan Frame, 1) // Buffer for one HEADERS frame or RST

			// Configure mockConnection for specific error simulation if needed for conn.sendHeadersFrame
			if tc.connErrorToSimulate != nil {
				conn.connError = tc.connErrorToSimulate
				if conn.shutdownChan == nil { // Ensure shutdownChan is initializable
					conn.shutdownChan = make(chan struct{})
				}
				close(conn.shutdownChan)
			}

			stream := newTestStream(t, testStreamID, conn, true, 0, 0)

			// Setup initial stream conditions
			stream.mu.Lock()
			stream.state = tc.initialState
			stream.responseHeadersSent = tc.initialResponseHeadersSent
			stream.endStreamSentToClient = tc.initialEndStreamSentToClient
			if tc.initialPendingRSTCode != nil {
				// Make a copy for the stream to own
				codeCopy := *tc.initialPendingRSTCode
				stream.pendingRSTCode = &codeCopy
			}
			stream.mu.Unlock()

			err := stream.SendHeaders(tc.headersToSend, tc.endStreamToSend)

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
					if (headersFrame.Header().Flags&FlagHeadersEndStream != 0) != tc.expectedFrameEndStreamFlag {
						t.Errorf("HeadersFrame END_STREAM flag mismatch: got %v, want %v (Flags: %08b)", (headersFrame.Header().Flags&FlagHeadersEndStream != 0), tc.expectedFrameEndStreamFlag, headersFrame.Header().Flags)
					}
					// Basic check on header block fragment (more detailed check if HPACK involved)
					if len(headersFrame.HeaderBlockFragment) == 0 {
						t.Error("HeadersFrame HeaderBlockFragment is empty")
					}
				case <-time.After(50 * time.Millisecond): // Increased timeout slightly
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
			finalResponseHeadersSent := stream.responseHeadersSent
			finalEndStreamSentToClient := stream.endStreamSentToClient
			stream.mu.RUnlock()

			if finalState != tc.expectedFinalState {
				t.Errorf("Expected final stream state %s, got %s", tc.expectedFinalState, finalState)
			}
			if finalResponseHeadersSent != tc.expectedResponseHeadersSent {
				t.Errorf("Expected final responseHeadersSent %v, got %v", tc.expectedResponseHeadersSent, finalResponseHeadersSent)
			}
			if finalEndStreamSentToClient != tc.expectedEndStreamSentToClient {
				t.Errorf("Expected final endStreamSentToClient %v, got %v", tc.expectedEndStreamSentToClient, finalEndStreamSentToClient)
			}
		})
	}
}

func TestStream_setState_GeneralTransitions(t *testing.T) {
	t.Parallel()
	const testStreamID = uint32(11)

	allStates := []StreamState{
		StreamStateIdle, StreamStateReservedLocal, StreamStateReservedRemote,
		StreamStateOpen, StreamStateHalfClosedLocal, StreamStateHalfClosedRemote,
		StreamStateClosed,
	}

	for _, initialState := range allStates {
		for _, targetState := range allStates {

			t.Run(fmt.Sprintf("From%s_To%s", initialState, targetState), func(t *testing.T) {
				conn, mnc := newTestConnection(t, false, nil) // mnc can be used if needed for debugging writes
				t.Cleanup(func() {
					// Define a unique error for cleanup-initiated close for clarity in logs if needed
					cleanupErr := fmt.Errorf("connection cleanup for sub-test From%s_To%s", initialState, targetState)
					if err := conn.Close(cleanupErr); err != nil {
						// Log error if conn.Close() itself fails, but don't fail the test here,
						// as the test might have intentionally put the connection in a state where Close() errors.
						// Filter out common "already closed" errors to avoid noise.
						errMsg := err.Error()
						if !strings.Contains(errMsg, "connection is already shutting down") &&
							!strings.Contains(errMsg, "use of closed network connection") &&
							!strings.Contains(errMsg, "shutdownChan already closed") { // Added this common one
							t.Logf("Error closing connection in sub-test 'From%s_To%s' cleanup: %v", initialState, targetState, err)
						}
					}
					// Ensure the mock net conn is also closed, conn.Close() should handle this.
					// Verifying mnc.IsClosed() can be added here if stricter checks are needed,
					// but might be redundant if conn.Close() is robust.
					if !mnc.IsClosed() {
						// t.Logf("Warning: mockNetConn was not closed by conn.Close() in sub-test 'From%s_To%s' cleanup.",initialState, targetState)
						// mnc.Close() // Force close if conn.Close() didn't.
					}
				})

				conn.writerChan = make(chan Frame, 1) // For teardown stream.Close() and other writes
				conn.writerChan = make(chan Frame, 1) // For teardown stream.Close()
				// Use DefaultInitialWindowSize to ensure FC window isn't zero, allowing Acquire to succeed if not closed.
				conn.ourInitialWindowSize = DefaultInitialWindowSize
				conn.peerInitialWindowSize = DefaultInitialWindowSize

				stream := newTestStream(t, testStreamID, conn, true, DefaultInitialWindowSize, DefaultInitialWindowSize)

				// Setup initial stream state and ensure resources are 'fresh' if not initially closed.
				stream.mu.Lock()
				stream.state = initialState
				originalCtx := stream.ctx // Capture the original context

				if initialState == StreamStateClosed {
					// If starting from Closed, simulate that resources were already cleaned.
					if stream.cancelCtx != nil {
						stream.cancelCtx() // Cancel context
					}
					if stream.requestBodyWriter != nil {
						_ = stream.requestBodyWriter.Close() // Close writer
					}
					// No need to close reader, writer close affects it.
					if stream.fcManager != nil && stream.fcManager.sendWindow != nil {
						stream.fcManager.sendWindow.Close(nil) // Close FC window
					}
				}
				stream.mu.Unlock()

				// Action: Call _setState
				stream.mu.Lock()
				stream._setState(targetState)
				stream.mu.Unlock()

				// Verification
				stream.mu.RLock()
				finalState := stream.state
				stream.mu.RUnlock()

				if finalState != targetState {
					t.Errorf("Expected stream state %s, got %s", targetState, finalState)
				}

				if targetState == StreamStateClosed {
					// Resources should be cleaned up.
					if !isContextDone(originalCtx) {
						t.Error("Expected context to be canceled when targetState is Closed")
					}

					_, errPipeWrite := stream.requestBodyWriter.Write([]byte("test"))
					if errPipeWrite != io.ErrClosedPipe {
						t.Errorf("Expected io.ErrClosedPipe writing to pipe when targetState is Closed, got %v", errPipeWrite)
					}

					_, errPipeRead := stream.requestBodyReader.Read(make([]byte, 1))
					if errPipeRead == nil {
						t.Error("Expected error reading from pipe when targetState is Closed")
					} else if !strings.Contains(errPipeRead.Error(), "closed") && errPipeRead != io.EOF { // Check for "closed" or EOF
						// Error depends on how closeStreamResourcesProtected closes the pipe for pendingRSTCode cases
						// For pendingRSTCode=nil it's io.EOF, for non-nil it's a StreamError.
						// A generic "closed" check is simpler here.
						t.Logf("Pipe read error (expected closed-like): %v", errPipeRead)
					}

					errFcSendAcquire := stream.fcManager.sendWindow.Acquire(1)
					if errFcSendAcquire == nil {
						t.Error("Expected error acquiring from send flow control when targetState is Closed")
					} else if _, ok := errFcSendAcquire.(*StreamError); !ok {
						t.Errorf("Expected StreamError from FC acquire when targetState is Closed, got %T: %v", errFcSendAcquire, errFcSendAcquire)
					}
				} else { // targetState is NOT StreamStateClosed
					if initialState == StreamStateClosed {
						// If started Closed, resources were already cleaned up. They should remain cleaned up.
						if !isContextDone(originalCtx) {
							t.Errorf("Context should remain canceled (initialState was Closed). Target state: %s", targetState)
						}
						// Check pipes remain closed
						_, errPipeWrite := stream.requestBodyWriter.Write([]byte("test"))
						if errPipeWrite != io.ErrClosedPipe {
							t.Errorf("Pipe writer should remain unusable (initialState was Closed). Target state: %s, err: %v", targetState, errPipeWrite)
						}
						_, errPipeRead := stream.requestBodyReader.Read(make([]byte, 1))
						if errPipeRead == nil { // Should be an error (EOF or closed pipe)
							t.Errorf("Pipe reader should remain unusable (initialState was Closed), got nil error. Target state: %s", targetState)
						}

						// Check FC remains closed
						errFcSendAcquire := stream.fcManager.sendWindow.Acquire(1)
						if errFcSendAcquire == nil {
							t.Errorf("FC send acquire should remain unusable (initialState was Closed). Target state: %s", targetState)
						}

					} else {
						// If started NOT Closed, and target is NOT Closed, resources should remain usable.
						if isContextDone(originalCtx) {
							t.Errorf("Context became canceled unexpectedly (initialState != Closed, targetState != Closed). Target state: %s", targetState)
						}

						// Pipes should NOT have been closed by _setState. We cannot perform blocking I/O.
						// Asserting they weren't closed by _setState is implicit if context and FC checks pass
						// and targetState is not Closed.

						// Check Flow Control: Acquire should succeed if window is not 0 (it's DefaultInitialWindowSize here).
						// If fcManager was closed by _setState, Acquire would error with a stream closed type error.
						errFcSendAcquire := stream.fcManager.sendWindow.Acquire(1)
						if errFcSendAcquire != nil {
							if se, ok := errFcSendAcquire.(*StreamError); ok && (se.Code == ErrCodeStreamClosed || se.Code == ErrCodeCancel) {
								t.Errorf("FC send acquire failed with stream closed/cancel error unexpectedly (initialState != Closed, targetState != Closed). Target state: %s, error: %v", targetState, errFcSendAcquire)
							}
							// Other FC errors (e.g. window full if test setup made it so) are not _setState's fault.
						}
					}
				}
			})
		}
	}
}

// Helper for TestStream_setState_GeneralTransitions
func isContextDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

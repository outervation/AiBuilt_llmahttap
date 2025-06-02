package http2

/*
   NOTE: The conn tests are split into multiple files to reduce the load on LLM
   context compared to having just one big file open.
*/

import (
	"errors"
	"fmt" // Added import
	"io"
	"strings"
	"sync/atomic" // ADDED MISSING IMPORT
	"testing"
	"time"
)

func TestConnection_DispatchPriorityFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                       string
		setupFunc                  func(t *testing.T, conn *Connection) // Optional setup like creating streams
		priorityFrame              *PriorityFrame
		expectedError              bool
		expectedConnectionError    *ConnectionError // If expectedError is true and it's a ConnectionError
		expectedRSTStreamErrorCode ErrorCode        // If an RST_STREAM is expected
		expectedRSTStreamID        uint32
	}{
		{
			name: "Valid PRIORITY frame",
			priorityFrame: &PriorityFrame{
				FrameHeader:      FrameHeader{Type: FramePriority, StreamID: 1, Length: 5},
				StreamDependency: 0,
				Weight:           15, // Effective weight 16
				Exclusive:        false,
			},
			expectedError: false,
		},
		{
			name: "PRIORITY frame on stream 0",
			priorityFrame: &PriorityFrame{
				FrameHeader:      FrameHeader{Type: FramePriority, StreamID: 0, Length: 5},
				StreamDependency: 1,
				Weight:           15,
			},
			expectedError:           true,
			expectedConnectionError: &ConnectionError{Code: ErrCodeProtocolError, Msg: "PRIORITY frame received on stream 0"},
		},
		{
			name: "PRIORITY frame causing self-dependency",
			setupFunc: func(t *testing.T, conn *Connection) {
				// Ensure stream 1 exists in priority tree, or ProcessPriorityFrame will create it.
				// If stream 1 did not exist and a PRIORITY frame tried to make it depend on itself,
				// getOrCreateNodeNoLock in UpdatePriority would create it, then the self-dependency check would fail.
			},
			priorityFrame: &PriorityFrame{
				FrameHeader:      FrameHeader{Type: FramePriority, StreamID: 1, Length: 5},
				StreamDependency: 1, // Stream 1 depends on Stream 1
				Weight:           15,
				Exclusive:        false,
			},
			expectedError:              false, // dispatchPriorityFrame handles StreamError by sending RST and returning nil
			expectedRSTStreamErrorCode: ErrCodeProtocolError,
			expectedRSTStreamID:        1,
		},
		// Note: A case for "PriorityTree.ProcessPriorityFrame returns a generic error" is harder to trigger
		// reliably without specific knowledge of PriorityTree internal states that lead to non-StreamErrors.
		// The current dispatchPriorityFrame logic handles StreamError vs. any other error from ProcessPriorityFrame.
	}

	for _, tc := range tests {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn, mnc := newTestConnection(t, false, nil) // Server-side
			var closeErr error = errors.New("test cleanup: " + tc.name)
			defer func() { conn.Close(closeErr) }() // Ensure writerLoop and other resources are cleaned up

			// performHandshakeForTest(t, conn, mnc) // Not strictly needed as PRIORITY can arrive any time
			// mnc.ResetWriteBuffer()

			if tc.setupFunc != nil {
				tc.setupFunc(t, conn)
			}

			dispatchErr := conn.dispatchPriorityFrame(tc.priorityFrame)

			if tc.expectedError {
				if dispatchErr == nil {
					t.Fatalf("Expected error from dispatchPriorityFrame, got nil")
				}
				if tc.expectedConnectionError != nil {
					connErr, ok := dispatchErr.(*ConnectionError)
					if !ok {
						t.Fatalf("Expected *ConnectionError, got %T: %v", dispatchErr, dispatchErr)
					}
					if connErr.Code != tc.expectedConnectionError.Code {
						t.Errorf("Expected ConnectionError code %s, got %s", tc.expectedConnectionError.Code, connErr.Code)
					}
					if tc.expectedConnectionError.Msg != "" && !strings.Contains(connErr.Msg, tc.expectedConnectionError.Msg) {
						t.Errorf("ConnectionError message mismatch: got '%s', expected to contain '%s'", connErr.Msg, tc.expectedConnectionError.Msg)
					}
				}
				// If it's an expected error, no RST should be sent by dispatchPriorityFrame itself,
				// as connection error implies GOAWAY later.
			} else { // No error expected from dispatchPriorityFrame
				if dispatchErr != nil {
					t.Fatalf("Expected no error from dispatchPriorityFrame, got %v", dispatchErr)
				}
			}

			if tc.expectedRSTStreamErrorCode != 0 {
				var rstFrame *RSTStreamFrame
				foundRST := false
				waitForCondition(t, 200*time.Millisecond, 10*time.Millisecond, func() bool {
					frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					for _, f := range frames {
						if rf, ok := f.(*RSTStreamFrame); ok {
							if rf.Header().StreamID == tc.expectedRSTStreamID {
								rstFrame = rf
								foundRST = true
								return true
							}
						}
					}
					return false
				}, fmt.Sprintf("RST_STREAM frame for stream %d with code %s", tc.expectedRSTStreamID, tc.expectedRSTStreamErrorCode))

				if !foundRST {
					allFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					t.Fatalf("Expected RST_STREAM frame for stream %d, none found. Frames on wire: %+v", tc.expectedRSTStreamID, allFrames)
				}
				if rstFrame.ErrorCode != tc.expectedRSTStreamErrorCode {
					t.Errorf("RST_STREAM ErrorCode: got %s, want %s", rstFrame.ErrorCode, tc.expectedRSTStreamErrorCode)
				}
			} else if !tc.expectedError { // No RST expected AND no connection error expected
				time.Sleep(50 * time.Millisecond) // Give writerLoop a moment
				if mnc.GetWriteBufferLen() > 0 {
					// Filter out PING ACKs if any default test setup sends them
					unexpectedFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					var actualUnexpectedFrames []Frame
					for _, fr := range unexpectedFrames {
						if pf, ok := fr.(*PingFrame); ok && (pf.Flags&FlagPingAck) != 0 {
							continue
						}
						actualUnexpectedFrames = append(actualUnexpectedFrames, fr)
					}
					if len(actualUnexpectedFrames) > 0 {
						t.Errorf("Unexpected frames written to connection: %+v", actualUnexpectedFrames)
					}
				}
			}
			closeErr = nil // Test logic passed for this subtest
		})
	}
}

func TestConnection_DispatchRSTStreamFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                       string
		setupFunc                  func(t *testing.T, conn *Connection) *Stream // Returns the stream if one is created for the test
		rstStreamFrame             *RSTStreamFrame
		expectedError              bool
		expectedConnectionError    *ConnectionError // If expectedError is true and it's a ConnectionError
		checkStreamStateAfter      bool             // If true, checks the stream's state and existence
		expectedStreamClosed       bool             // If checkStreamStateAfter, expect stream to be closed
		expectedStreamRemoved      bool             // If checkStreamStateAfter, expect stream to be removed from conn.streams
		expectGoAwayFromConnClose  bool             // If true, expects a GOAWAY if conn.Close is called with returned error
		expectedGoAwayErrorCode    ErrorCode        // If expectGoAwayFromConnClose, this is the expected code
		expectedFramesFromDispatch int              // Number of frames expected to be written directly by dispatchRSTStreamFrame or its callees (e.g., no RST for valid peer RST)
	}{
		{
			name: "Valid RST_STREAM for open stream",
			setupFunc: func(t *testing.T, conn *Connection) *Stream {
				s, err := conn.createStream(1, nil, true /*isPeerInitiated*/)
				if err != nil {
					t.Fatalf("Setup: Failed to create stream: %v", err)
				}
				s.mu.Lock()
				s.state = StreamStateOpen
				s.mu.Unlock()
				return s
			},
			rstStreamFrame: &RSTStreamFrame{
				FrameHeader: FrameHeader{Type: FrameRSTStream, StreamID: 1, Length: 4},
				ErrorCode:   ErrCodeCancel,
			},
			expectedError:         false,
			checkStreamStateAfter: true,
			expectedStreamClosed:  true,
			expectedStreamRemoved: true,
		},
		{
			name: "RST_STREAM on stream 0",
			rstStreamFrame: &RSTStreamFrame{
				FrameHeader: FrameHeader{Type: FrameRSTStream, StreamID: 0, Length: 4},
				ErrorCode:   ErrCodeCancel,
			},
			expectedError:             true,
			expectedConnectionError:   &ConnectionError{Code: ErrCodeProtocolError, Msg: "RST_STREAM frame received on stream 0"},
			expectGoAwayFromConnClose: true,
			expectedGoAwayErrorCode:   ErrCodeProtocolError,
		},
		{
			name: "RST_STREAM for unknown stream (never existed or already removed)",
			rstStreamFrame: &RSTStreamFrame{
				FrameHeader: FrameHeader{Type: FrameRSTStream, StreamID: 99, Length: 4}, // Stream 99 does not exist
				ErrorCode:   ErrCodeStreamClosed,
			},
			expectedError: true, // Now expect an error as per h2spec 5.1/2 & 6.4/2

			expectedConnectionError:   &ConnectionError{Code: ErrCodeProtocolError, Msg: "RST_STREAM received for numerically idle stream 99"},
			expectGoAwayFromConnClose: true, // Since it's a connection error
			expectedGoAwayErrorCode:   ErrCodeProtocolError,
		},
		{
			name: "RST_STREAM for known stream that is already closed (but still in map temporarily for test)",
			setupFunc: func(t *testing.T, conn *Connection) *Stream {
				s, err := conn.createStream(3, nil, true)
				if err != nil {
					t.Fatalf("Setup: Failed to create stream: %v", err)
				}
				s.mu.Lock()
				s.state = StreamStateClosed // Stream is already closed
				s.mu.Unlock()
				return s
			},
			rstStreamFrame: &RSTStreamFrame{
				FrameHeader: FrameHeader{Type: FrameRSTStream, StreamID: 3, Length: 4},
				ErrorCode:   ErrCodeCancel,
			},
			expectedError:         false, // Still processed to ensure cleanup, but effectively "ignored" in terms of protocol response
			checkStreamStateAfter: true,
			expectedStreamClosed:  true, // Should remain closed
			expectedStreamRemoved: true, // Should be removed
		},
	}

	for _, tc := range tests {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn, mnc := newTestConnection(t, false, nil)
			var closeErr error = errors.New("test cleanup: " + tc.name)
			// Defer Close needs to be conditional or handled carefully if the test itself calls conn.Close.
			// Let's assume for these direct dispatch tests, the test is responsible for final conn.Close if checking GOAWAY.
			// Otherwise, this deferred Close cleans up.
			defer func() {
				if !tc.expectGoAwayFromConnClose { // If test doesn't handle Close itself
					if conn != nil {
						conn.Close(closeErr)
					}
				} else if conn != nil && closeErr != nil { // If test handles Close but an error occurred before that point
					conn.Close(closeErr) // Close with the setup/test error
				}
			}()

			var streamToCheck *Stream
			if tc.setupFunc != nil {
				streamToCheck = tc.setupFunc(t, conn)
			}

			dispatchErr := conn.dispatchRSTStreamFrame(tc.rstStreamFrame)

			if tc.expectedError {
				if dispatchErr == nil {
					closeErr = errors.New("Expected error from dispatchRSTStreamFrame, got nil")
					t.Fatal(closeErr)
				}
				if tc.expectedConnectionError != nil {
					connErr, ok := dispatchErr.(*ConnectionError)
					if !ok {
						closeErr = fmt.Errorf("Expected *ConnectionError, got %T: %v", dispatchErr, dispatchErr)
						t.Fatal(closeErr)
					}
					if connErr.Code != tc.expectedConnectionError.Code {
						e := fmt.Errorf("Expected ConnectionError code %s, got %s", tc.expectedConnectionError.Code, connErr.Code)
						if closeErr == nil {
							closeErr = e
						} else {
							closeErr = fmt.Errorf("%w; %w", closeErr, e)
						}
						t.Error(e)
					}
					if tc.expectedConnectionError.Msg != "" && !strings.Contains(connErr.Msg, tc.expectedConnectionError.Msg) {
						e := fmt.Errorf("ConnectionError message mismatch: got '%s', expected to contain '%s'", connErr.Msg, tc.expectedConnectionError.Msg)
						if closeErr == nil {
							closeErr = e
						} else {
							closeErr = fmt.Errorf("%w; %w", closeErr, e)
						}
						t.Error(e)
					}
				}
			} else { // No error expected from dispatchRSTStreamFrame
				if dispatchErr != nil {
					closeErr = fmt.Errorf("Expected no error from dispatchRSTStreamFrame, got %v", dispatchErr)
					t.Fatal(closeErr)
				}
			}

			if tc.checkStreamStateAfter && streamToCheck != nil {
				streamToCheck.mu.RLock()
				state := streamToCheck.state
				streamToCheck.mu.RUnlock()

				if tc.expectedStreamClosed && state != StreamStateClosed {
					e := fmt.Errorf("Stream %d: expected state Closed, got %s", streamToCheck.id, state)
					if closeErr == nil {
						closeErr = e
					} else {
						closeErr = fmt.Errorf("%w; %w", closeErr, e)
					}
					t.Error(e)
				}

				if tc.expectedStreamRemoved {
					_, exists := conn.getStream(streamToCheck.id)
					if exists {
						e := fmt.Errorf("Stream %d: expected to be removed from connection, but still exists", streamToCheck.id)
						if closeErr == nil {
							closeErr = e
						} else {
							closeErr = fmt.Errorf("%w; %w", closeErr, e)
						}
						t.Error(e)
					}
				}
			}

			// Check for GOAWAY if dispatchRSTStreamFrame returned a ConnectionError.
			// This simulates the Serve loop calling conn.Close(dispatchErr).
			if tc.expectGoAwayFromConnClose && tc.expectedError && dispatchErr != nil {
				// Perform handshake if not already done (mnc needs to be able to write)
				// To simplify, ensure writerLoop is running. newTestConnection starts it.

				// Now explicitly call conn.Close with the error from dispatchRSTStreamFrame
				_ = conn.Close(dispatchErr) // Error from this Close is not primary for this test part

				var goAwayFrame *GoAwayFrame
				waitForCondition(t, 1*time.Second, 20*time.Millisecond, func() bool {
					frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					for _, f := range frames {
						if gaf, ok := f.(*GoAwayFrame); ok {
							if gaf.ErrorCode == tc.expectedGoAwayErrorCode {
								goAwayFrame = gaf
								return true
							}
						}
					}
					return false
				}, fmt.Sprintf("GOAWAY frame with ErrorCode %s after RST_STREAM on stream 0", tc.expectedGoAwayErrorCode))

				if goAwayFrame == nil {
					allFrames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					e := fmt.Errorf("GOAWAY frame not written or with wrong error code. Expected code %s. Frames on wire: %+v", tc.expectedGoAwayErrorCode, allFrames)
					if closeErr == nil {
						closeErr = e
					} else {
						closeErr = fmt.Errorf("%w; %w", closeErr, e)
					}
					t.Error(e) // Use Error to not stop other subtests if parallel
				}
				// Mark closeErr as nil for the defer if this path is successful.
				// This indicates the test logic itself passed, and the deferred conn.Close(nil) is fine.
				if t.Failed() == false { // If no t.Error/Fatal so far in this branch
					closeErr = nil
				}

			} else { // No GOAWAY expected as a direct consequence of this dispatch
				// Give writerLoop a moment to ensure no unexpected frames are sent.
				time.Sleep(50 * time.Millisecond)
				writtenBytes := mnc.GetWriteBufferBytes()
				if len(writtenBytes) > tc.expectedFramesFromDispatch*FrameHeaderLen { // Crude check, allow for some minimum frame size
					// More precise: check if any frames OTHER than expected ones (e.g., PING ACKs from other activities) were written.
					// For this test, assume expectedFramesFromDispatch is 0 if not checking GOAWAY.
					frames := readAllFramesFromBuffer(t, writtenBytes)
					var unexpectedFrames []string
					for _, f := range frames {
						// Basic PING ACK filter, assuming other tests might trigger these.
						// This test itself should not cause PING ACKs.
						if pf, ok := f.(*PingFrame); ok && (pf.Flags&FlagPingAck) != 0 {
							continue
						}
						unexpectedFrames = append(unexpectedFrames, fmt.Sprintf("%T", f))
					}
					if len(unexpectedFrames) > tc.expectedFramesFromDispatch {
						e := fmt.Errorf("Unexpected frames written to connection: %v. Expected %d frames.", unexpectedFrames, tc.expectedFramesFromDispatch)
						if closeErr == nil {
							closeErr = e
						} else {
							closeErr = fmt.Errorf("%w; %w", closeErr, e)
						}
						t.Error(e)
					}
				}
				if t.Failed() == false {
					closeErr = nil
				}
			}
		})
	}
}

func TestConnection_HandleSettingsFrame(t *testing.T) {
	t.Parallel()

	defaultPeerSettingsMap := func() map[SettingID]uint32 {
		return map[SettingID]uint32{
			SettingHeaderTableSize:      DefaultSettingsHeaderTableSize,
			SettingEnablePush:           DefaultServerEnablePush, // Assuming peer is a server by default for initial values
			SettingInitialWindowSize:    DefaultSettingsInitialWindowSize,
			SettingMaxFrameSize:         DefaultSettingsMaxFrameSize,
			SettingMaxConcurrentStreams: 0xffffffff, // Default "unlimited"
			SettingMaxHeaderListSize:    0xffffffff, // Default "unlimited"
		}
	}

	tests := []struct {
		name                        string
		setupFunc                   func(t *testing.T, conn *Connection, mnc *mockNetConn) // Optional setup
		settingsFrameToProcess      *SettingsFrame
		expectError                 bool
		expectedConnectionErrorCode ErrorCode
		expectedConnectionErrorMsg  string    // Substring to check in error message
		expectAckSent               bool      // True if a SETTINGS ACK should be sent by the server
		expectedGoAwayOnError       bool      // NEW: If true, expects a GOAWAY if expectError is true and error is ConnectionError
		expectedGoAwayErrorCode     ErrorCode // NEW: If expectGoAwayOnError, this is the expected code
		checkPeerSettings           bool      // True to verify conn.peerSettings map after processing
		expectedPeerSettings        map[SettingID]uint32
		checkOperationalValues      func(t *testing.T, conn *Connection) // Custom checks for conn.peerMaxFrameSize, etc.
		checkStreamUpdates          func(t *testing.T, conn *Connection) // Custom checks for stream updates (e.g., FCW)
	}{
		{
			name: "Valid SETTINGS frame from peer (not ACK)",
			settingsFrameToProcess: &SettingsFrame{
				FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: 18 /* 3 settings * 6 bytes */},
				Settings: []Setting{
					{ID: SettingInitialWindowSize, Value: 123456},
					{ID: SettingMaxFrameSize, Value: 32768},
					{ID: SettingHeaderTableSize, Value: 8192},
				},
			},
			expectError:       false,
			expectAckSent:     true,
			checkPeerSettings: true,
			expectedPeerSettings: func() map[SettingID]uint32 {
				ps := defaultPeerSettingsMap()
				ps[SettingInitialWindowSize] = 123456
				ps[SettingMaxFrameSize] = 32768
				ps[SettingHeaderTableSize] = 8192
				return ps
			}(),
			checkOperationalValues: func(t *testing.T, conn *Connection) {
				conn.settingsMu.RLock()
				defer conn.settingsMu.RUnlock()
				if conn.peerInitialWindowSize != 123456 {
					t.Errorf("conn.peerInitialWindowSize: got %d, want %d", conn.peerInitialWindowSize, 123456)
				}
				if conn.peerMaxFrameSize != 32768 {
					t.Errorf("conn.peerMaxFrameSize: got %d, want %d", conn.peerMaxFrameSize, 32768)
				}
				if conn.hpackAdapter == nil {
					t.Error("conn.hpackAdapter is nil")
				} else {
					// This check assumes direct setting. HPACK adapter might have internal limits or behavior.
					// For now, assume direct update of its configured max table size.
					// The hpackAdapter.SetMaxEncoderDynamicTableSize is called by applyPeerSettings.
					// No direct way to get current max encoder table size from hpackAdapter, it's internal to hpack.Encoder.
					// We trust applyPeerSettings calls it.
				}
			},
		},
		{
			name: "Valid SETTINGS ACK from peer",
			settingsFrameToProcess: &SettingsFrame{
				FrameHeader: FrameHeader{Type: FrameSettings, Flags: FlagSettingsAck, StreamID: 0, Length: 0},
			},
			setupFunc: func(t *testing.T, conn *Connection, mnc *mockNetConn) {
				// Simulate server having sent SETTINGS and is awaiting ACK
				conn.settingsMu.Lock()
				conn.settingsAckTimeoutTimer = time.NewTimer(10 * time.Second) // Dummy timer
				conn.settingsMu.Unlock()
			},
			expectError:       false,
			expectAckSent:     false,
			checkPeerSettings: false, // ACK doesn't change settings
			checkOperationalValues: func(t *testing.T, conn *Connection) {
				conn.settingsMu.RLock()
				defer conn.settingsMu.RUnlock()
				if conn.settingsAckTimeoutTimer != nil {
					// Timer should be stopped and set to nil upon receiving ACK.
					// This check might be racy if timer fires exactly as test runs.
					// The waitForCondition approach in earlier tests is better for this.
					// For direct handleSettingsFrame call, timer should be nil.
					t.Error("settingsAckTimeoutTimer was not cleared after ACK")
				}
			},
		},
		{
			name: "SETTINGS ACK with non-zero payload length (h2spec 6.5.1)",
			settingsFrameToProcess: &SettingsFrame{
				FrameHeader: FrameHeader{Type: FrameSettings, Flags: FlagSettingsAck, StreamID: 0, Length: 6}, // Non-zero length
				Settings:    nil,                                                                              // Payload for ACK must be empty. `Settings` items are ignored if Length and ACK flag lead to error.
			},
			expectError:                 true,
			expectedConnectionErrorCode: ErrCodeFrameSizeError,
			expectedConnectionErrorMsg:  "SETTINGS ACK frame received with non-zero length",
			expectAckSent:               false,
			expectedGoAwayOnError:       true,
			expectedGoAwayErrorCode:     ErrCodeFrameSizeError,
			checkPeerSettings:           false, // Settings are not applied on error
		},
		{
			name: "SETTINGS frame with non-zero StreamID",
			settingsFrameToProcess: &SettingsFrame{
				FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 1, Length: 0}, // StreamID = 1
			},
			expectError:                 true,
			expectedConnectionErrorCode: ErrCodeProtocolError,
			expectedConnectionErrorMsg:  "SETTINGS frame received with non-zero stream ID",
			expectAckSent:               false,
		},
		{
			name: "SETTINGS (not ACK) with length not multiple of 6",
			settingsFrameToProcess: &SettingsFrame{
				FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: 7}, // Length = 7
			},
			expectError:                 true,
			expectedConnectionErrorCode: ErrCodeFrameSizeError,
			expectedConnectionErrorMsg:  "SETTINGS frame received with length not a multiple of 6",
			expectAckSent:               false,
		},
		{
			name: "SETTINGS with invalid SETTINGS_ENABLE_PUSH value",
			settingsFrameToProcess: &SettingsFrame{
				FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: 6},
				Settings:    []Setting{{ID: SettingEnablePush, Value: 2}}, // Invalid value 2
			},
			expectError:                 true,
			expectedConnectionErrorCode: ErrCodeProtocolError,
			expectedConnectionErrorMsg:  "invalid SETTINGS_ENABLE_PUSH value: 2",
			expectAckSent:               false, // Error occurs before ACK can be sent
		},
		{
			name: "SETTINGS with SETTINGS_INITIAL_WINDOW_SIZE > MaxWindowSize",
			settingsFrameToProcess: &SettingsFrame{
				FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: 6},
				Settings:    []Setting{{ID: SettingInitialWindowSize, Value: MaxWindowSize + 1}},
			},
			expectError:                 true,
			expectedConnectionErrorCode: ErrCodeFlowControlError,
			expectedConnectionErrorMsg:  fmt.Sprintf("invalid SETTINGS_INITIAL_WINDOW_SIZE value %d exceeds maximum %d", MaxWindowSize+1, MaxWindowSize),
			expectAckSent:               false,
		},
		{
			name: "SETTINGS with SETTINGS_MAX_FRAME_SIZE too low",
			settingsFrameToProcess: &SettingsFrame{
				FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: 6},
				Settings:    []Setting{{ID: SettingMaxFrameSize, Value: MinMaxFrameSize - 1}},
			},
			expectError:                 true,
			expectedConnectionErrorCode: ErrCodeProtocolError,
			expectedConnectionErrorMsg:  fmt.Sprintf("invalid SETTINGS_MAX_FRAME_SIZE value: %d", MinMaxFrameSize-1),
			expectAckSent:               false,
		},
		{
			name: "SETTINGS with SETTINGS_MAX_FRAME_SIZE too high",
			settingsFrameToProcess: &SettingsFrame{
				FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: 6},
				Settings:    []Setting{{ID: SettingMaxFrameSize, Value: MaxAllowedFrameSizeValue + 1}},
			},
			expectError:                 true,
			expectedConnectionErrorCode: ErrCodeProtocolError,
			expectedConnectionErrorMsg:  fmt.Sprintf("invalid SETTINGS_MAX_FRAME_SIZE value: %d", MaxAllowedFrameSizeValue+1),
			expectAckSent:               false,
		},
		{
			name: "SETTINGS with unknown setting ID (should be ignored)",
			settingsFrameToProcess: &SettingsFrame{
				FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: 12 /* 2 settings */},
				Settings: []Setting{
					{ID: SettingInitialWindowSize, Value: 99999}, // Known
					{ID: SettingID(0xFFFF), Value: 123},          // Unknown ID
				},
			},
			expectError:       false,
			expectAckSent:     true,
			checkPeerSettings: true,
			expectedPeerSettings: func() map[SettingID]uint32 {
				ps := defaultPeerSettingsMap()
				ps[SettingInitialWindowSize] = 99999
				// Unknown setting 0xFFFF should not be in ps
				return ps
			}(),
		},
		{
			name: "SETTINGS changes SETTINGS_INITIAL_WINDOW_SIZE, updates streams",
			setupFunc: func(t *testing.T, conn *Connection, mnc *mockNetConn) {
				// Create a stream
				_, err := conn.createStream(1, nil, true /*isPeerInitiated*/)
				if err != nil {
					t.Fatalf("Setup: Failed to create stream: %v", err)
				}
			},
			settingsFrameToProcess: &SettingsFrame{
				FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: 6},
				Settings:    []Setting{{ID: SettingInitialWindowSize, Value: 70000}},
			},
			expectError:   false,
			expectAckSent: true,
			checkStreamUpdates: func(t *testing.T, conn *Connection) {
				stream, exists := conn.getStream(1)
				if !exists {
					t.Fatal("Stream 1 not found after SETTINGS update")
				}
				// fcManager.HandlePeerSettingsInitialWindowSizeChange updates sendWindow.initialWindowSize and adjusts 'available'
				stream.fcManager.sendWindow.mu.Lock()
				newInitialSend := stream.fcManager.sendWindow.initialWindowSize
				stream.fcManager.sendWindow.mu.Unlock()

				if newInitialSend != 70000 {
					t.Errorf("Stream 1 sendWindow.initialWindowSize: got %d, want %d", newInitialSend, 70000)
				}
				// Check `available` is more complex due to potential previous subtractions.
				// The core check is that initialWindowSize itself on the FlowControlWindow object is updated.
			},
		},
	}

	for _, tc := range tests {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn, mnc := newTestConnection(t, false, nil) // Server-side
			var closeErr error = errors.New("test cleanup: " + tc.name)
			defer func() {
				if conn != nil {
					// If an error was expected and occurred, close with that error.
					// This might trigger a GOAWAY if the test expects it.
					if tc.expectError && conn.connError != nil {
						conn.Close(conn.connError)
					} else {
						conn.Close(closeErr) // Otherwise, close with test-specific or nil.
					}
				}
			}()

			// performHandshakeForTest(t, conn, mnc) // Not strictly needed for direct handleSettingsFrame calls
			// mnc.ResetWriteBuffer()

			// Apply initial default peer settings for baseline
			conn.settingsMu.Lock()
			conn.peerSettings = defaultPeerSettingsMap()
			conn.applyPeerSettings()
			conn.settingsMu.Unlock()

			if tc.setupFunc != nil {
				tc.setupFunc(t, conn, mnc)
			}

			// Store initial HPACK encoder table size for comparison if settings change it.
			// Note: cannot directly query hpack.Encoder.MaxDynamicTableSize(), rely on conn.peerSettings and applyPeerSettings logic.
			// Initial peer settings are default, so hpackAdapter's encoder max table size reflects default.

			err := conn.handleSettingsFrame(tc.settingsFrameToProcess)

			if tc.expectError {
				if err == nil {
					t.Fatalf("Expected error, got nil")
				}
				connErr, ok := err.(*ConnectionError)
				if !ok {
					t.Fatalf("Expected *ConnectionError, got %T: %v", err, err)
				}
				if connErr.Code != tc.expectedConnectionErrorCode {
					t.Errorf("Expected ConnectionError code %s, got %s", tc.expectedConnectionErrorCode, connErr.Code)
				}
				if tc.expectedConnectionErrorMsg != "" && !strings.Contains(connErr.Msg, tc.expectedConnectionErrorMsg) {
					t.Errorf("ConnectionError message '%s' does not contain substring '%s'", connErr.Msg, tc.expectedConnectionErrorMsg)
				}
				// Store the error on conn so deferred Close can use it for GOAWAY check
				conn.streamsMu.Lock()
				conn.connError = err
				conn.streamsMu.Unlock()

			} else { // No error expected
				if err != nil {
					t.Fatalf("Expected no error, got %v", err)
				}
			}

			if tc.expectAckSent {
				var ackFrame *SettingsFrame
				waitForCondition(t, 200*time.Millisecond, 10*time.Millisecond, func() bool {
					frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					for _, f := range frames {
						if sf, ok := f.(*SettingsFrame); ok && (sf.Header().Flags&FlagSettingsAck != 0) {
							ackFrame = sf
							return true
						}
					}
					return false
				}, "SETTINGS ACK frame to be written")

				if ackFrame == nil {
					t.Fatal("Expected SETTINGS ACK to be sent, but not found")
				}
				if ackFrame.Header().Length != 0 {
					t.Errorf("SETTINGS ACK frame length: got %d, want 0", ackFrame.Header().Length)
				}
			} else {
				// Ensure no ACK (or any other frame) was sent if not expected
				time.Sleep(50 * time.Millisecond) // Give writer a chance
				if mnc.GetWriteBufferLen() > 0 {
					frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
					t.Errorf("Unexpected frames written: %+v", frames)
				}
			}

			if tc.checkPeerSettings {
				conn.settingsMu.RLock()
				currentPeerSettings := make(map[SettingID]uint32)
				for k, v := range conn.peerSettings { // Make a copy for comparison
					currentPeerSettings[k] = v
				}
				conn.settingsMu.RUnlock()

				if len(currentPeerSettings) != len(tc.expectedPeerSettings) {
					t.Errorf("Peer settings map length mismatch: got %d, want %d. Got: %+v, Want: %+v",
						len(currentPeerSettings), len(tc.expectedPeerSettings), currentPeerSettings, tc.expectedPeerSettings)
				}
				for id, expectedVal := range tc.expectedPeerSettings {
					if actualVal, ok := currentPeerSettings[id]; !ok {
						t.Errorf("Expected peer setting ID %s not found", id)
					} else if actualVal != expectedVal {
						t.Errorf("Peer setting ID %s: got value %d, want %d", id, actualVal, expectedVal)
					}
				}
				// Also check for unexpected settings
				for id, actualVal := range currentPeerSettings {
					if _, ok := tc.expectedPeerSettings[id]; !ok {
						t.Errorf("Unexpected peer setting ID %s found with value %d", id, actualVal)
					}
				}
			}

			if tc.checkOperationalValues != nil {
				tc.checkOperationalValues(t, conn)
			}
			if tc.checkStreamUpdates != nil {
				tc.checkStreamUpdates(t, conn)
			}
			closeErr = nil // Test logic for this subtest passed
		})
	}
}

func TestServerHandshake_Failure_ClientDisconnectsBeforePreface(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	var closeErr error = errors.New("test cleanup: TestServerHandshake_Failure_ClientDisconnectsBeforePreface")
	defer func() { conn.Close(closeErr) }()

	// Simulate immediate EOF from client by not feeding any data
	// and mockNetConn's Read will return io.EOF if readBuf is empty.
	mnc.readHook = func(b []byte) (int, error) { // Ensure EOF is the only outcome if buffer empty
		return 0, io.EOF
	}

	err := conn.ServerHandshake()
	if err == nil {
		t.Fatal("ServerHandshake succeeded when client disconnected before preface, expected error")
	}

	connErr, ok := err.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError, got %T: %v", err, err)
	}
	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ProtocolError for client disconnect before preface, got %s", connErr.Code)
	}
	// Check for specific message content
	if !strings.Contains(connErr.Msg, "client disconnected before sending full preface") &&
		!strings.Contains(connErr.Msg, "error reading client connection preface") { // Allow for generic read error
		t.Errorf("Unexpected error message: %s. Expected it to relate to preface read failure or disconnect.", connErr.Msg)
	}
	closeErr = nil
}

func TestServerHandshake_Failure_ClientSendsIncompletePreface(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	var closeErr error = errors.New("test cleanup: TestServerHandshake_Failure_ClientSendsIncompletePreface")
	defer func() { conn.Close(closeErr) }()

	incompletePreface := ClientPreface[:len(ClientPreface)-5]
	mnc.FeedReadBuffer([]byte(incompletePreface))
	// After this, mnc.Read will hit EOF as readBuf will be exhausted by ReadFull.

	err := conn.ServerHandshake()
	if err == nil {
		t.Fatal("ServerHandshake succeeded with incomplete preface, expected error")
	}

	connErr, ok := err.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError, got %T: %v", err, err)
	}
	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ProtocolError for incomplete preface, got %s", connErr.Code)
	}
	if !strings.Contains(connErr.Msg, "client disconnected before sending full preface") &&
		!strings.Contains(connErr.Msg, "error reading client connection preface") {
		t.Errorf("Unexpected error message for incomplete preface: %s", connErr.Msg)
	}
	closeErr = nil
}

func TestServerHandshake_Failure_ClientSendsWrongFrameAfterPreface(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	var closeErr error = errors.New("test cleanup: TestServerHandshake_Failure_ClientSendsWrongFrameAfterPreface")
	defer func() { conn.Close(closeErr) }()

	mnc.FeedReadBuffer([]byte(ClientPreface))

	pingFrame := &PingFrame{
		FrameHeader: FrameHeader{Type: FramePing, StreamID: 0, Length: 8},
		OpaqueData:  [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
	}
	mnc.FeedReadBuffer(frameToBytes(t, pingFrame))

	err := conn.ServerHandshake()
	if err == nil {
		t.Fatal("ServerHandshake succeeded when client sent PING instead of SETTINGS, expected error")
	}

	connErr, ok := err.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError, got %T: %v", err, err)
	}
	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ProtocolError for wrong frame after preface, got %s", connErr.Code)
	}
	expectedMsgSubstr := "expected client's first frame (post-preface) to be SETTINGS, got PING"
	if !strings.Contains(connErr.Msg, expectedMsgSubstr) {
		t.Errorf("Error message mismatch: got '%s', expected to contain '%s'", connErr.Msg, expectedMsgSubstr)
	}
	closeErr = nil
}

func TestServerHandshake_Failure_ClientInitialSettingsHasAck(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	var closeErr error = errors.New("test cleanup: TestServerHandshake_Failure_ClientInitialSettingsHasAck")
	defer func() { conn.Close(closeErr) }()

	mnc.FeedReadBuffer([]byte(ClientPreface))

	clientSettingsFrameWithAck := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, Flags: FlagSettingsAck, StreamID: 0, Length: 0},
	}
	mnc.FeedReadBuffer(frameToBytes(t, clientSettingsFrameWithAck))

	err := conn.ServerHandshake()
	if err == nil {
		t.Fatal("ServerHandshake succeeded when client's initial SETTINGS had ACK, expected error")
	}

	connErr, ok := err.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError, got %T: %v", err, err)
	}
	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ProtocolError for client's initial SETTINGS with ACK, got %s", connErr.Code)
	}
	expectedMsgSubstr := "client's initial SETTINGS frame (post-preface) must not have ACK flag set"
	if !strings.Contains(connErr.Msg, expectedMsgSubstr) {
		t.Errorf("Error message mismatch: got '%s', expected to contain '%s'", connErr.Msg, expectedMsgSubstr)
	}
	closeErr = nil
}

func TestServerHandshake_Failure_TimeoutReadingClientSettings(t *testing.T) {
	conn, mnc := newTestConnection(t, false, nil)
	var closeErr error = errors.New("test cleanup: TestServerHandshake_Failure_TimeoutReadingClientSettings")
	// Defer a final conn.Close. The error used here will be updated by test logic.
	defer func() {
		if conn != nil {
			conn.Close(closeErr)
		}
	}()

	mnc.FeedReadBuffer([]byte(ClientPreface)) // Feed preface first

	// Set the readHook *after* feeding the preface.
	// ServerHandshake will read the preface from readBuf.
	// When it tries to read the client's SETTINGS frame, readBuf will be empty,
	// and mnc.Read will call this hook.
	var hookCalledAtomic int32 // Use atomic for safe check in logs, though test is serial here
	mnc.readHook = func(b []byte) (int, error) {
		atomic.StoreInt32(&hookCalledAtomic, 1)
		t.Logf("TestServerHandshake_Failure_TimeoutReadingClientSettings: mnc.readHook called, returning timeoutError.")
		return 0, timeoutError{} // Simulate timeout when trying to read client's SETTINGS
	}

	err := conn.ServerHandshake()

	if atomic.LoadInt32(&hookCalledAtomic) == 0 {
		t.Log("Warning: mnc.readHook was not called. The test might not be exercising the intended timeout path for client SETTINGS read.")
	}

	if err == nil {
		t.Fatal("ServerHandshake succeeded when timeout reading client SETTINGS, expected error")
	}

	connErr, ok := err.(*ConnectionError)
	if !ok {
		// Check if the direct error is our timeoutError, implying it wasn't wrapped as expected
		if _, isTimeout := err.(timeoutError); isTimeout {
			t.Fatalf("ServerHandshake returned raw timeoutError, expected it to be wrapped in ConnectionError. Err: %v", err)
		}
		t.Fatalf("Expected ConnectionError, got %T: %v", err, err)
	}

	// When timeout occurs while waiting for client's SETTINGS frame (after preface & server settings sent)
	// the error code should be PROTOCOL_ERROR, and message should reflect timeout reading client settings.
	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ProtocolError for timeout reading client SETTINGS, got %s. Msg: %s", connErr.Code, connErr.Msg)
	}
	// The ServerHandshake has a specific check and message for this:
	// "timeout waiting for client SETTINGS frame" or "error reading client's initial SETTINGS frame"
	expectedMsgSubstr1 := "timeout waiting for client SETTINGS frame"
	expectedMsgSubstr2 := "error reading client's initial SETTINGS frame" // if ReadFrame itself fails
	if !strings.Contains(connErr.Msg, expectedMsgSubstr1) && !strings.Contains(connErr.Msg, expectedMsgSubstr2) {
		t.Errorf("Error message mismatch: got '%s', expected to contain '%s' or '%s'", connErr.Msg, expectedMsgSubstr1, expectedMsgSubstr2)
	}

	// Check underlying cause
	if unwrapped := errors.Unwrap(connErr); unwrapped == nil {
		t.Error("Expected ConnectionError to have an underlying cause for timeout")
	} else if _, isTimeout := unwrapped.(timeoutError); !isTimeout {
		// Further unwrap if it's wrapped by fmt.Errorf by ReadFrame
		if unwrappedNested := errors.Unwrap(unwrapped); unwrappedNested != nil {
			if _, isTimeoutNested := unwrappedNested.(timeoutError); !isTimeoutNested {
				t.Errorf("Underlying cause of ConnectionError is %T (after one unwrap) / %T (after two unwraps), expected timeoutError", unwrapped, unwrappedNested)
			}
		} else {
			t.Errorf("Underlying cause of ConnectionError is %T, expected timeoutError", unwrapped)
		}
	}

	// If the test logic has passed, set closeErr to nil for the deferred Close.
	// Otherwise, the deferred Close will use the initial "test cleanup..." error.
	if !t.Failed() {
		closeErr = nil
	}
}

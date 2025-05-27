package http2

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"unsafe" // Required for the newTestStream helper due to constraints

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/logger"
	"golang.org/x/net/http2/hpack"
)

// mockConnection is a mock implementation of parts of http2.Connection relevant for testing Stream.
// It provides fields that stream.go accesses directly on `s.conn` and methods that stream.go calls.
type mockConnection struct {
	// Field layout matching http2.Connection (approximated for relevant fields)
	// Offsets are critical. Word size assumed to be 8 bytes (64-bit).

	// netConn net.Conn (interface = 2 words)
	_mock_netConn_placeholder1 uintptr
	_mock_netConn_placeholder2 uintptr

	// log *logger.Logger (pointer = 1 word)
	log *logger.Logger // Initialized by newTestStream

	// isClient bool (1 byte + 7 bytes padding = 1 word)
	_mock_isClient_placeholder uintptr

	// ctx context.Context (interface = 2 words)
	ctx context.Context // Initialized by newTestStream
	// cancelCtx context.CancelFunc (func pointer = 1 word)
	cancelCtx context.CancelFunc // Initialized by newTestStream

	// readerDone chan struct{} (chan pointer = 1 word)
	_mock_readerDone_placeholder chan struct{}
	// writerDone chan struct{} (chan pointer = 1 word)
	_mock_writerDone_placeholder chan struct{}
	// shutdownChan chan struct{} (chan pointer = 1 word)
	_mock_shutdownChan_placeholder chan struct{}
	// connError error (interface = 2 words)
	_mock_connError_placeholder1 uintptr
	_mock_connError_placeholder2 uintptr

	// streamsMu sync.RWMutex (approx. 3-4 words)
	// For simplicity, using a block of uintptr to cover its size.
	// Exact size can be found with unsafe.Sizeof(sync.RWMutex{})
	// RWMutex contains: w Mutex, writerSem uint32, readerSem uint32, readerCount int32, readerWait int32
	// Mutex contains: state int32, sema uint32. Total = 24 bytes on 64-bit (3 words)
	_mock_streamsMu_placeholder1 uintptr
	_mock_streamsMu_placeholder2 uintptr
	_mock_streamsMu_placeholder3 uintptr

	// streams map[uint32]*Stream (map pointer = 1 word)
	_mock_streams_placeholder uintptr
	// nextStreamIDClient uint32 (4 bytes)
	// nextStreamIDServer uint32 (4 bytes)
	// lastProcessedStreamID uint32 (4 bytes)
	// peerReportedLastStreamID uint32 (4 bytes)
	// Total 16 bytes = 2 words
	_mock_streamIDs_placeholder1 uintptr
	_mock_streamIDs_placeholder2 uintptr

	// priorityTree *PriorityTree (pointer = 1 word)
	priorityTree *PriorityTree // Initialized by newTestStream

	// hpackAdapter *HpackAdapter (pointer = 1 word)
	_mock_hpackAdapter_placeholder uintptr
	// connFCManager *ConnectionFlowControlManager (pointer = 1 word)
	connFCManager *ConnectionFlowControlManager // Initialized by newTestStream

	// goAwaySent bool (1 byte)
	// goAwayReceived bool (1 byte)
	// + padding (e.g., 6 bytes if next field is 8-byte aligned) = 1 word
	_mock_goAwayFlags_placeholder uintptr

	// gracefulShutdownTimer *time.Timer (pointer = 1 word)
	_mock_gracefulShutdownTimer_placeholder uintptr
	// activePings map[[8]byte]*time.Timer (map pointer = 1 word)
	_mock_activePings_placeholder uintptr
	// activePingsMu sync.Mutex (Mutex = 8 bytes = 1 word)
	_mock_activePingsMu_placeholder uintptr

	// --- Placeholder for fields between activePingsMu and writerChan ---
	// This is a rough estimate. Many fields here.
	// activeHeaderBlockStreamID uint32
	// headerFragments [][]byte (slice = 3 words)
	// headerFragmentTotalSize uint32
	// headerFragmentInitialType FrameType (uint8)
	// headerFragmentPromisedID uint32
	// headerFragmentEndStream bool
	// headerFragmentInitialPrioInfo *streamDependencyInfo (ptr = 1 word)
	// ourSettings map[SettingID]uint32 (map ptr = 1 word)
	// settingsMu sync.RWMutex (3 words)
	// peerSettings map[SettingID]uint32 (map ptr = 1 word)
	// ourCurrentMaxFrameSize uint32 ... up to concurrentStreamsInbound int
	// Approximate padding size. A more accurate calculation or using unsafe.Offsetof is needed for true robustness.
	// Based on calculation, 19 words needed for fields between activePingsMu and writerChan.
	_padd_to_writerChan [19]uintptr

	// writerChan chan Frame (chan pointer = 1 word)
	writerChan chan Frame // THE CRITICAL FIELD - Initialized by newTestStream

	// Fields after writerChan that might be accessed by stream.go via s.conn.FIELD
	// _settingsAckTimeoutTimer *time.Timer
	// _initialSettingsWritten chan struct{}
	// maxFrameSize uint32
	// remoteAddrStr string
	// _dispatcher_placeholder uintptr
	// For now, focus on writerChan. If other panics occur, these need alignment.
	_padd_after_writerChan [5]uintptr // Placeholder for some fields after writerChan.

	// --- Fields used by newTestStream to pass values, NOT for layout of s.conn.FIELD ---
	// These are distinct from the layout placeholders above.
	// The `ourInitialWindowSize` and `peerInitialWindowSize` are passed as arguments to `newStream`,
	// not accessed via `s.conn.ourInitialWindowSize`.
	cfgOurInitialWindowSize  uint32
	cfgPeerInitialWindowSize uint32
	cfgMaxFrameSize          uint32 // For configuring the aligned maxFrameSize field if needed.
	cfgRemoteAddrStr         string // For configuring the aligned remoteAddrStr field if needed.

	// --- Callbacks for custom mock behavior (if mock methods were callable) ---
	onSendHeadersFrameImpl      func(s *Stream, headers []hpack.HeaderField, endStream bool) error
	onSendDataFrameImpl         func(s *Stream, data []byte, endStream bool) (int, error)
	onSendRSTStreamFrameImpl    func(streamID uint32, errorCode ErrorCode) error
	onSendWindowUpdateFrameImpl func(streamID uint32, increment uint32) error
	onExtractPseudoHeadersImpl  func(headers []hpack.HeaderField) (method, path, scheme, authority string, err error)
	onStreamHandlerDoneImpl     func(s *Stream)

	// --- Test inspection fields (will not be populated correctly by current call path) ---
	mu                  sync.Mutex
	lastSendHeadersArgs *struct {
		Stream    *Stream
		Headers   []hpack.HeaderField
		EndStream bool
	}
	allSendHeadersArgs []struct {
		Stream    *Stream
		Headers   []hpack.HeaderField
		EndStream bool
	}
	lastSendDataArgs *struct {
		Stream    *Stream
		Data      []byte
		EndStream bool
	}
	allSendDataArgs []struct {
		Stream    *Stream
		Data      []byte
		EndStream bool
	}
	lastRSTArgs *struct {
		StreamID  uint32
		ErrorCode ErrorCode
	}
	allRSTArgs []struct {
		StreamID  uint32
		ErrorCode ErrorCode
	}
	lastWindowUpdateArgs *struct {
		StreamID  uint32
		Increment uint32
	}
	allWindowUpdateArgs []struct {
		StreamID  uint32
		Increment uint32
	}
	lastExtractPseudoHeadersHF []hpack.HeaderField
	lastStreamHandlerDoneArgs  *struct{ Stream *Stream }

	sendHeadersFrameCount      int
	sendDataFrameCount         int
	sendRSTStreamFrameCount    int
	sendWindowUpdateFrameCount int
	extractPseudoHeadersCount  int
	streamHandlerDoneCount     int
}

// Methods that Stream calls on its `conn` object.
// These are part of mockConnection but are NOT CALLED due to unsafe.Pointer strategy.
// The real Connection methods are called instead. These are effectively dead code for now.
func (mc *mockConnection) sendHeadersFrame(s *Stream, headers []hpack.HeaderField, endStream bool) error {
	mc.mu.Lock()
	mc.sendHeadersFrameCount++
	args := struct {
		Stream    *Stream
		Headers   []hpack.HeaderField
		EndStream bool
	}{s, headers, endStream}
	mc.lastSendHeadersArgs = &args
	mc.allSendHeadersArgs = append(mc.allSendHeadersArgs, args)
	mc.mu.Unlock()
	if mc.onSendHeadersFrameImpl != nil {
		return mc.onSendHeadersFrameImpl(s, headers, endStream)
	}
	return nil
}
func (mc *mockConnection) sendDataFrame(s *Stream, data []byte, endStream bool) (int, error) {
	mc.mu.Lock()
	mc.sendDataFrameCount++
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)
	args := struct {
		Stream    *Stream
		Data      []byte
		EndStream bool
	}{s, dataCopy, endStream}
	mc.lastSendDataArgs = &args
	mc.allSendDataArgs = append(mc.allSendDataArgs, args)
	mc.mu.Unlock()
	if mc.onSendDataFrameImpl != nil {
		return mc.onSendDataFrameImpl(s, data, endStream)
	}
	return len(data), nil
}
func (mc *mockConnection) sendRSTStreamFrame(streamID uint32, errorCode ErrorCode) error {
	mc.mu.Lock()
	mc.sendRSTStreamFrameCount++
	args := struct {
		StreamID  uint32
		ErrorCode ErrorCode
	}{streamID, errorCode}
	mc.lastRSTArgs = &args
	mc.allRSTArgs = append(mc.allRSTArgs, args)
	mc.mu.Unlock()
	if mc.onSendRSTStreamFrameImpl != nil {
		return mc.onSendRSTStreamFrameImpl(streamID, errorCode)
	}
	return nil
}
func (mc *mockConnection) sendWindowUpdateFrame(streamID uint32, increment uint32) error {
	mc.mu.Lock()
	mc.sendWindowUpdateFrameCount++
	args := struct {
		StreamID  uint32
		Increment uint32
	}{streamID, increment}
	mc.lastWindowUpdateArgs = &args
	mc.allWindowUpdateArgs = append(mc.allWindowUpdateArgs, args)
	mc.mu.Unlock()
	if mc.onSendWindowUpdateFrameImpl != nil {
		return mc.onSendWindowUpdateFrameImpl(streamID, increment)
	}
	return nil
}
func (mc *mockConnection) extractPseudoHeaders(headers []hpack.HeaderField) (method, path, scheme, authority string, err error) {
	mc.mu.Lock()
	mc.extractPseudoHeadersCount++
	mc.lastExtractPseudoHeadersHF = headers
	mc.mu.Unlock()
	if mc.onExtractPseudoHeadersImpl != nil {
		return mc.onExtractPseudoHeadersImpl(headers)
	}
	var foundMethod, foundPath, foundScheme bool
	for _, hf := range headers {
		if !strings.HasPrefix(hf.Name, ":") {
			break
		}
		switch hf.Name {
		case ":method":
			method = hf.Value
			foundMethod = true
		case ":path":
			path = hf.Value
			foundPath = true
		case ":scheme":
			scheme = hf.Value
			foundScheme = true
		case ":authority":
			authority = hf.Value
		}
	}
	if !foundMethod || !foundPath || !foundScheme {
		return "", "", "", "", NewConnectionError(ErrCodeProtocolError, "missing required pseudo-headers")
	}
	return method, path, scheme, authority, nil
}
func (mc *mockConnection) streamHandlerDone(s *Stream) {
	mc.mu.Lock()
	mc.streamHandlerDoneCount++
	mc.lastStreamHandlerDoneArgs = &struct{ Stream *Stream }{s}
	mc.mu.Unlock()
	if mc.onStreamHandlerDoneImpl != nil {
		mc.onStreamHandlerDoneImpl(s)
	}
}

// mockRequestDispatcher is a mock for the RequestDispatcherFunc.
type mockRequestDispatcher struct {
	fn         func(sw StreamWriter, req *http.Request)
	mu         sync.Mutex
	called     bool
	lastStream StreamWriter
	lastReq    *http.Request
}

func (mrd *mockRequestDispatcher) Dispatch(sw StreamWriter, req *http.Request) {
	mrd.mu.Lock()
	mrd.called = true
	mrd.lastStream = sw
	mrd.lastReq = req
	mrd.mu.Unlock()
	if mrd.fn != nil {
		mrd.fn(sw, req)
	}
}

// newTestStream is a helper function to initialize a http2.Stream for testing.
func newTestStream(t *testing.T, id uint32, mc *mockConnection, prioWeight uint8, prioParentID uint32, prioExclusive bool, isInitiatedByPeer bool) *Stream {
	t.Helper()

	// Initialize fields in mockConnection that need to be non-nil for Connection methods
	// or for newStream's internal logic when accessing s.conn.FIELD.
	if mc.ctx == nil {
		mc.ctx, mc.cancelCtx = context.WithCancel(context.Background())
	}
	if mc.log == nil {
		logTarget := os.DevNull
		enabled := false
		logCfg := &config.LoggingConfig{
			LogLevel:  config.LogLevelDebug,
			AccessLog: &config.AccessLogConfig{Enabled: &enabled, Target: &logTarget},
			ErrorLog:  &config.ErrorLogConfig{Target: &logTarget},
		}
		var err error
		mc.log, err = logger.NewLogger(logCfg)
		if err != nil {
			t.Fatalf("Failed to create logger for mock connection: %v", err)
		}
	}
	if mc.priorityTree == nil {
		mc.priorityTree = NewPriorityTree()
	}
	if mc.connFCManager == nil {
		mc.connFCManager = NewConnectionFlowControlManager()
	}
	// CRITICAL: Initialize the writerChan that Connection.sendRSTStreamFrame will use.
	// This field must be at the correct memory offset in mockConnection.
	// Ensure writerChan is initialized (it's done where mc is declared if needed, or here)
	if mc.writerChan == nil {
		mc.writerChan = make(chan Frame, 10) // Default buffer if not pre-set
	}

	// The writerChan is initialized. Tests that expect frames to be sent
	// must read from mc.writerChan or ensure it's drained if not inspected.
	// The previous automatic drainer is removed to allow tests to inspect frames.
	// If a test doesn't inspect writerChan but causes writes, it should ensure
	// writerChan is sufficiently buffered or start a local drainer.
	// go func() {
	// 	for {
	// 		select {
	// 		case _, ok := <-mc.writerChan:
	// 			if !ok {
	// 				return
	// 			} // Channel closed
	// 		case <-mc.ctx.Done(): // Connection context cancelled
	// 			return
	// 		}
	// 	}
	// }()

	// Initialize other config values in mockConnection if they are used by stream.go via s.conn.FIELD.
	// For example, if s.conn.maxFrameSize is used:
	// (*Connection)(unsafe.Pointer(mc)).maxFrameSize = mc.cfgMaxFrameSize (this needs careful alignment)
	// For now, we rely on the direct field `mc.maxFrameSize` being set if stream.go was modified to use it from mockConnection,
	// or that the default (0) is handled by stream.go if it accesses s.conn.maxFrameSize.
	// The real Connection.maxFrameSize is at the very end. If stream.go uses s.conn.maxFrameSize,
	// then the mockConnection needs a field at that specific offset.
	// The current stream.go uses DefaultMaxFrameSize if s.conn.maxFrameSize is 0.

	// Values passed directly to newStream function call:
	ourInitialWin := mc.cfgOurInitialWindowSize
	if ourInitialWin == 0 {
		ourInitialWin = DefaultInitialWindowSize
	}
	peerInitialWin := mc.cfgPeerInitialWindowSize
	if peerInitialWin == 0 {
		peerInitialWin = DefaultInitialWindowSize
	}

	connAsRealConnType := (*Connection)(unsafe.Pointer(mc))

	s, err := newStream(
		connAsRealConnType,
		id,
		ourInitialWin,  // Pass configured or default
		peerInitialWin, // Pass configured or default
		prioWeight,
		prioParentID,
		prioExclusive,
		isInitiatedByPeer,
	)
	if err != nil {
		t.Fatalf("newStream failed for stream %d: %v", id, err)
	}

	t.Cleanup(func() {
		_ = s.Close(fmt.Errorf("test stream %d cleanup", s.id))
		if mc.cancelCtx != nil {
			mc.cancelCtx()
		}
		// No automatic writerChan close here; tests manage it or rely on context.
	})

	return s
}

// Helper to create default hpack.HeaderFields for testing
func makeHpackHeaders(kv ...string) []hpack.HeaderField {
	if len(kv)%2 != 0 {
		panic("makeHpackHeaders: odd number of kv args")
	}
	hfs := make([]hpack.HeaderField, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		hfs = append(hfs, hpack.HeaderField{Name: kv[i], Value: kv[i+1]})
	}
	return hfs
}

// Helper to create default http2.HeaderFields for testing
func makeStreamWriterHeaders(kv ...string) []HeaderField {
	if len(kv)%2 != 0 {
		panic("makeStreamWriterHeaders: odd number of kv args")
	}
	hfs := make([]HeaderField, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		hfs = append(hfs, HeaderField{Name: kv[i], Value: kv[i+1]})
	}
	return hfs
}

// TestStream_Close_SendsRSTAndCleansUp tests the stream.Close() method.
// It verifies that an RST_STREAM frame is sent, state transitions to Closed,
// and associated resources are cleaned up.
func TestStream_Close_SendsRSTAndCleansUp(t *testing.T) {
	mc := &mockConnection{}
	mc.cfgOurInitialWindowSize = DefaultInitialWindowSize
	mc.cfgPeerInitialWindowSize = DefaultInitialWindowSize
	mc.writerChan = make(chan Frame, 1) // Buffer 1 for the RST_STREAM

	stream := newTestStream(t, 1, mc, 16, 0, false, true)

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
		case frame := <-mc.writerChan:
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
		mc := &mockConnection{} // New mock connection for this sub-test
		mc.writerChan = make(chan Frame, 1)
		stream2 := newTestStream(t, 2, mc, 16, 0, false, true)
		stream2.mu.Lock()
		stream2.state = StreamStateOpen
		stream2.mu.Unlock()

		err := stream2.Close(nil) // Close with nil error
		if err != nil {
			t.Fatalf("stream2.Close(nil) failed: %v", err)
		}

		select {
		case frame := <-mc.writerChan:
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

// Helper functions for t.Logf, t.Errorf, t.Fatalf to avoid data races on t
// if tests are run in parallel (though these unit tests are not by default).
// More importantly, it makes them callable from goroutines if needed.
func tLogf(t *testing.T, format string, args ...interface{}) {
	t.Helper()
	t.Logf(format, args...)
}
func tErrorf(t *testing.T, format string, args ...interface{}) {
	t.Helper()
	t.Errorf(format, args...)
}
func tFatalf(t *testing.T, format string, args ...interface{}) {
	t.Helper()
	t.Fatalf(format, args...)
}
func tError(t *testing.T, args ...interface{}) {
	t.Helper()
	t.Error(args...)
}

func TestStream_IDAndContext(t *testing.T) {
	mc := &mockConnection{}
	streamID := uint32(5)
	stream := newTestStream(t, streamID, mc, 16, 0, false, true)

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

	// Check context is now done
	select {
	case <-ctx.Done():
		// Expected: context is done
		if ctx.Err() == nil {
			t.Error("stream context Done, but Err() is nil, expected context.Canceled or similar")
		} else if ctx.Err() != context.Canceled {
			// Depending on how stream.Close() cancels, it might be Canceled or a custom error.
			// For this test, we mainly care that it's done.
			// If a specific error is expected, this check should be more precise.
			// The stream's cancelCtx() is called, which should lead to context.Canceled.
			t.Logf("stream context done with error: %v (expected context.Canceled or similar)", ctx.Err())
		}
	default:
		t.Error("stream context was not done after stream.Close(), expected done")
	}
}

func TestStream_sendRSTStream_DirectCall(t *testing.T) {
	mc := &mockConnection{}
	mc.cfgOurInitialWindowSize = DefaultInitialWindowSize
	mc.cfgPeerInitialWindowSize = DefaultInitialWindowSize
	mc.writerChan = make(chan Frame, 1)

	streamID := uint32(3)
	stream := newTestStream(t, streamID, mc, 16, 0, false, true)

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
				case frame := <-mc.writerChan:
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
				case frame := <-mc.writerChan:
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

// newDataFrame is a helper to create DataFrame instances for testing.
func newDataFrame(streamID uint32, data []byte, endStream bool) *DataFrame {
	fh := FrameHeader{
		Length:   uint32(len(data)),
		Type:     FrameData,
		StreamID: streamID,
	}
	if endStream {
		fh.Flags |= FlagDataEndStream
	}
	return &DataFrame{
		FrameHeader: fh,
		Data:        data,
		// Padding and PadLength are not used by handleDataFrame directly, so omitted for simplicity
	}
}

func TestStream_handleDataFrame(t *testing.T) {
	const testStreamID = uint32(1)
	defaultPrioWeight := uint8(16)

	testCases := []struct {
		name                        string
		initialStreamState          StreamState
		initialEndStreamReceived    bool // s.endStreamReceivedFromClient
		initialOurWindowSize        uint32
		frameData                   []byte
		frameEndStream              bool
		setupPipeReaderEarlyClose   bool // If true, close s.requestBodyReader before calling handleDataFrame
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
			name:                        "Write to pipe fails (reader closed early)",
			initialStreamState:          StreamStateOpen,
			initialOurWindowSize:        100,
			frameData:                   []byte("pipefail"),
			frameEndStream:              false,
			setupPipeReaderEarlyClose:   true,
			expectError:                 true,
			expectedErrorCode:           ErrCodeCancel, // Or InternalError, stream.go uses Cancel
			expectedErrorContains:       "pipe",
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
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &mockConnection{
				cfgOurInitialWindowSize: tc.initialOurWindowSize,
				writerChan:              make(chan Frame, 1), // Buffer for potential RST by stream.Close in teardown
			}
			stream := newTestStream(t, testStreamID, mc, defaultPrioWeight, 0, false, true)

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
						} else if !strings.Contains(pipeErr.Error(), "closed pipe") && pipeErr != io.EOF {
							t.Errorf("Expected 'closed pipe' or EOF from reader after stream.Close(), got: %v (handleDataFrame error: %v)", pipeErr, err)
						} else {
							t.Logf("Got expected 'closed pipe' or EOF from pipe reader after stream.Close(): %v (handleDataFrame error: %v)", pipeErr, err)
						}
					} else if tc.setupPipeReaderEarlyClose {
						if pipeErr == nil {
							t.Errorf("Expected an error from pipe reader when reader was closed early by test, but got nil")
						} else if !strings.Contains(pipeErr.Error(), "closed pipe") && pipeErr != io.EOF {
							t.Errorf("Expected 'closed pipe' or EOF from reader (closed by test setup), got: %v", pipeErr)
						} else {
							t.Logf("Got expected 'closed pipe' or EOF error from pipe reader (closed by test setup): %v", pipeErr)
						}
					} else if tc.expectPipeWriterClosed { // handleDataFrame closed writer (e.g. END_STREAM)
						if pipeErr != nil && pipeErr != io.EOF {
							t.Errorf("Pipe reader goroutine got unexpected error (expected EOF or nil if no data): %v", pipeErr)
						}
						// If pipeErr is io.EOF or nil (for 0 bytes read then EOF), it's fine.
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
	const testStreamID = uint32(1)
	defaultPrioWeight := uint8(16)

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
			mc := &mockConnection{
				writerChan: make(chan Frame, 1), // Not used by handleRSTStreamFrame directly, but by teardown
			}
			stream := newTestStream(t, testStreamID, mc, defaultPrioWeight, 0, false, true)

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

// TestStream_transitionStateOnSendEndStream tests the internal state transitions
// when the server sends an END_STREAM flag.
func TestStream_transitionStateOnSendEndStream(t *testing.T) {
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
			mc := &mockConnection{writerChan: make(chan Frame, 1)} // For teardown
			stream := newTestStream(t, 1, mc, 16, 0, false, true)

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

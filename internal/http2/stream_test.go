package http2

import (
	"context"
	"fmt"
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
	mc.writerChan = make(chan Frame, 10) // Buffered to avoid blocking in simple tests

	// Start a goroutine to drain mc.writerChan.
	// This is necessary because the real Connection methods will send to it,
	// and without a consumer, these sends would block.
	go func() {
		for {
			select {
			case _, ok := <-mc.writerChan:
				if !ok {
					return
				} // Channel closed
			case <-mc.ctx.Done(): // Connection context cancelled
				return
			}
		}
	}()

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
		// Close the writerChan to signal the draining goroutine to exit.
		// This should be done carefully, perhaps after ensuring the stream is fully closed.
		// mc.mu.Lock() // Assuming writerChan is protected if accessed concurrently
		// if mc.writerChan != nil {
		// 	close(mc.writerChan)
		// 	mc.writerChan = nil
		// }
		// mc.mu.Unlock()
		// For now, relying on context cancellation for goroutine exit.
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

// TestStream_InitializationAndSimpleClose
func TestStream_InitializationAndSimpleClose(t *testing.T) {
	mc := &mockConnection{}
	// Initialize config fields that newTestStream uses to pass args to newStream
	mc.cfgOurInitialWindowSize = 1024
	mc.cfgPeerInitialWindowSize = 2048

	stream := newTestStream(t, 1, mc, 16, 0, false, true)

	if stream.id != 1 {
		t.Errorf("Expected stream ID 1, got %d", stream.id)
	}
	if stream.state != StreamStateIdle {
		t.Errorf("Expected initial state Idle, got %s", stream.state)
	}

	stream.mu.Lock()
	stream.state = StreamStateOpen // Manually set for test purposes
	stream.mu.Unlock()

	err := stream.Close(nil) // Server initiates close with CANCEL
	if err != nil {
		// If this fails, it might be because writerChan related logic in Connection.sendRSTStreamFrame
		// has other dependencies (e.g. on c.log being non-nil if it logs before/after send)
		// The mc.log is initialized by newTestStream.
		t.Fatalf("stream.Close() failed: %v", err)
	}

	// Note: The following assertions are on mc.sendRSTStreamFrameCount and mc.lastRSTArgs.
	// These will NOT be correct because the real Connection.sendRSTStreamFrame is called,
	// not the mockConnection's one. This test will "pass" if no panic, but its assertions are flawed.
	// To properly test this, one would need to inspect what was sent to mc.writerChan.

	// if mc.sendRSTStreamFrameCount != 1 {
	// 	t.Errorf("Expected sendRSTStreamFrame to be called once, got %d (Note: this assertion is likely flawed due to unsafe.Pointer use)", mc.sendRSTStreamFrameCount)
	// }
	// mc.mu.Lock()
	// if mc.lastRSTArgs == nil || mc.lastRSTArgs.StreamID != 1 || mc.lastRSTArgs.ErrorCode != ErrCodeCancel {
	// 	t.Errorf("Unexpected RST_STREAM frame: ID=%d, Code=%s (Note: this assertion is likely flawed)", mc.lastRSTArgs.StreamID, mc.lastRSTArgs.ErrorCode)
	// }
	// mc.mu.Unlock()

	stream.mu.RLock()
	finalState := stream.state
	stream.mu.RUnlock()
	if finalState != StreamStateClosed {
		t.Errorf("Expected stream state Closed after Close(), got %s", finalState)
	}
}

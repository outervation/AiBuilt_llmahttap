package http2

import (
	"context"
	"encoding/json" // Added for json.RawMessage

	"fmt" // Added for fmt.Errorf
	"golang.org/x/net/http2/hpack"
	"net"
	"sync"
	"time"

	// "example.com/llmahttap/v2/internal/config" // Assuming config provides Http2Settings eventually
	"example.com/llmahttap/v2/internal/logger"
)

// Default settings values (RFC 7540 Section 6.5.2)
const (
	DefaultSettingsHeaderTableSize   uint32 = 4096
	DefaultSettingsInitialWindowSize uint32 = 65535 // (2^16 - 1)
	DefaultSettingsMaxFrameSize      uint32 = 16384 // (2^14)
	// DefaultSettingsMaxConcurrentStreams is effectively unlimited initially for peer.
	// Server should advertise a limit.
	DefaultServerMaxConcurrentStreams uint32 = 100
	// DefaultSettingsMaxHeaderListSize is effectively unlimited initially for peer.
	// Server should advertise a limit.
	DefaultServerMaxHeaderListSize uint32 = 1024 * 32 // 32KB
	DefaultClientEnablePush        uint32 = 0
	DefaultServerEnablePush        uint32 = 1
)

// Connection manages an entire HTTP/2 connection.
type Connection struct {
	netConn net.Conn
	log     *logger.Logger
	// cfg     *config.Config // Full config if needed, or specific parts

	isClient bool // True if this connection is on the client side

	// Context and lifecycle
	ctx          context.Context
	cancelCtx    context.CancelFunc
	readerDone   chan struct{} // Closed when reader goroutine exits
	writerDone   chan struct{} // Closed when writer goroutine exits
	shutdownChan chan struct{} // Closed to signal connection shutdown initiated
	connError    error         // Stores the first fatal connection error

	// HTTP/2 state
	streamsMu             sync.RWMutex
	streams               map[uint32]*Stream
	nextStreamIDClient    uint32 // Next client-initiated stream ID (odd), server consumes
	nextStreamIDServer    uint32 // Next server-initiated stream ID (even), server produces (for PUSH)
	lastProcessedStreamID uint32 // Highest stream ID processed or accepted for GOAWAY
	priorityTree          *PriorityTree
	hpackAdapter          *HpackAdapter
	connFCManager         *ConnectionFlowControlManager
	goAwaySent            bool
	goAwayReceived        bool
	gracefulShutdownTimer *time.Timer
	activePings           map[[8]byte]*time.Timer // Tracks outstanding PINGs and their timeout timers
	activePingsMu         sync.Mutex

	// Settings state
	settingsMu sync.Mutex // Protects all settings-related fields below
	// Our settings that we advertise/enforce
	ourSettings map[SettingID]uint32
	// Peer's settings that they advertise/enforce
	peerSettings map[SettingID]uint32

	// Derived operational values from settings
	// Our capabilities / limits we impose on peer:
	ourCurrentMaxFrameSize  uint32 // Our SETTINGS_MAX_FRAME_SIZE (max payload we can receive)
	ourInitialWindowSize    uint32 // Our SETTINGS_INITIAL_WINDOW_SIZE (for new streams' receive windows)
	ourMaxConcurrentStreams uint32 // Our SETTINGS_MAX_CONCURRENT_STREAMS (limit on peer creating streams)
	ourMaxHeaderListSize    uint32 // Our SETTINGS_MAX_HEADER_LIST_SIZE (limit on peer's request/response header size)
	ourEnablePush           bool   // Our SETTINGS_ENABLE_PUSH

	// Peer's capabilities / limits they impose on us:
	peerMaxFrameSize         uint32 // Peer's SETTINGS_MAX_FRAME_SIZE (max payload we can send)
	peerInitialWindowSize    uint32 // Peer's SETTINGS_INITIAL_WINDOW_SIZE (for new streams' send windows)
	peerMaxConcurrentStreams uint32 // Peer's SETTINGS_MAX_CONCURRENT_STREAMS (limit on us creating streams)
	peerMaxHeaderListSize    uint32 // Peer's SETTINGS_MAX_HEADER_LIST_SIZE (limit on our request/response header size)

	// Tracking for MAX_CONCURRENT_STREAMS
	concurrentStreamsOutbound int // Number of streams we have initiated and are not closed/reset
	concurrentStreamsInbound  int // Number of streams peer has initiated and are not closed/reset

	// Writer goroutine coordination
	writerChan              chan Frame  // Frames to be sent by the writer goroutine
	settingsAckTimeoutTimer *time.Timer // Timer for waiting for SETTINGS ACK

	// Added fields
	maxFrameSize uint32 // To satisfy stream.go, should eventually alias to peerMaxFrameSize or ourCurrentMaxFrameSize depending on context

	remoteAddrStr string // Cached remote address string
}

// NewConnection creates and initializes a new HTTP/2 Connection.
// nc: underlying network connection
// lg: logger instance
// isClientSide: boolean indicating if this is a client-side connection
// srvSettingsOverride: For server-side, specific HTTP/2 settings overrides. Can be nil.
//
//	These would typically come from config.Config.Server.Http2Settings.
func NewConnection(
	nc net.Conn,
	lg *logger.Logger,
	isClientSide bool,
	srvSettingsOverride map[SettingID]uint32,
) *Connection {
	ctx, cancel := context.WithCancel(context.Background())

	conn := &Connection{
		netConn:       nc,
		log:           lg,
		isClient:      isClientSide,
		ctx:           ctx,
		cancelCtx:     cancel,
		readerDone:    make(chan struct{}),
		writerDone:    make(chan struct{}),
		shutdownChan:  make(chan struct{}),
		streams:       make(map[uint32]*Stream),
		priorityTree:  NewPriorityTree(),
		connFCManager: NewConnectionFlowControlManager(),
		writerChan:    make(chan Frame, 64), // Increased buffer
		activePings:   make(map[[8]byte]*time.Timer),
		ourSettings:   make(map[SettingID]uint32),
		peerSettings:  make(map[SettingID]uint32),
		remoteAddrStr: nc.RemoteAddr().String(),
	}

	// Initialize client/server stream ID counters
	if isClientSide {
		conn.nextStreamIDClient = 1
		// Server-initiated stream IDs are even. Clients don't initiate with even IDs.
		// If this client were to support receiving PUSH_PROMISE, nextStreamIDServer would track expected even IDs.
		conn.nextStreamIDServer = 0
	} else { // Server side
		conn.nextStreamIDClient = 0 // Server expects client to start with stream ID 1
		conn.nextStreamIDServer = 2 // First server-initiated PUSH_PROMISE will use ID 2
	}

	// Initialize default settings values for peer (will be updated upon receiving peer's SETTINGS frame)
	conn.peerSettings[SettingHeaderTableSize] = DefaultSettingsHeaderTableSize
	conn.peerSettings[SettingEnablePush] = DefaultServerEnablePush // Assume peer server might push
	conn.peerSettings[SettingInitialWindowSize] = DefaultSettingsInitialWindowSize
	conn.peerSettings[SettingMaxFrameSize] = DefaultSettingsMaxFrameSize
	conn.peerSettings[SettingMaxConcurrentStreams] = 0xffffffff // Effectively unlimited until known
	conn.peerSettings[SettingMaxHeaderListSize] = 0xffffffff    // Effectively unlimited until known

	// Initialize our settings
	// Start with general defaults applicable to both client/server before role-specifics
	conn.ourSettings[SettingHeaderTableSize] = DefaultSettingsHeaderTableSize
	conn.ourSettings[SettingInitialWindowSize] = DefaultSettingsInitialWindowSize
	conn.ourSettings[SettingMaxFrameSize] = DefaultSettingsMaxFrameSize

	if isClientSide {
		conn.ourSettings[SettingEnablePush] = DefaultClientEnablePush
		// Clients typically don't aggressively limit server pushes via MAX_CONCURRENT_STREAMS,
		// but they can. Using a reasonably high default.
		conn.ourSettings[SettingMaxConcurrentStreams] = 100
		conn.ourSettings[SettingMaxHeaderListSize] = DefaultServerMaxHeaderListSize // Client willing to accept large headers
	} else { // Server side
		conn.ourSettings[SettingEnablePush] = DefaultServerEnablePush
		conn.ourSettings[SettingMaxConcurrentStreams] = DefaultServerMaxConcurrentStreams
		conn.ourSettings[SettingMaxHeaderListSize] = DefaultServerMaxHeaderListSize
	}

	// Apply server-specific overrides if provided (only for server-side connections)
	if !isClientSide && srvSettingsOverride != nil {
		for id, val := range srvSettingsOverride {
			// TODO: Add validation for settings values here (e.g. MaxFrameSize range, EnablePush 0 or 1)
			// For example, SETTINGS_MAX_FRAME_SIZE must be between 16384 and 16777215.
			// SETTINGS_ENABLE_PUSH must be 0 or 1.
			conn.ourSettings[id] = val
		}
	}

	// Apply initial settings to derive operational values
	// These functions are called without holding settingsMu as this is during construction.
	conn.applyOurSettings()
	conn.applyPeerSettings()

	// Initialize HPACK adapter.
	// Our decoder's table size is set by our SETTINGS_HEADER_TABLE_SIZE.
	ourHpackTableSize := conn.ourSettings[SettingHeaderTableSize]
	conn.hpackAdapter = NewHpackAdapter(ourHpackTableSize)

	// Our encoder's table size limit is initially constrained by the peer's default (assumed) SETTINGS_HEADER_TABLE_SIZE.
	// This will be updated when we receive the peer's actual SETTINGS frame.
	peerHpackTableSize := conn.peerSettings[SettingHeaderTableSize]
	conn.hpackAdapter.SetMaxEncoderDynamicTableSize(peerHpackTableSize)

	return conn
}

// applyOurSettings updates connection's operational parameters based on conn.ourSettings.
// This should be called when our settings are initialized or changed.
// Assumes settingsMu is held if called outside constructor.
func (c *Connection) applyOurSettings() {
	c.ourCurrentMaxFrameSize = c.ourSettings[SettingMaxFrameSize]
	c.ourInitialWindowSize = c.ourSettings[SettingInitialWindowSize]
	c.ourMaxConcurrentStreams = c.ourSettings[SettingMaxConcurrentStreams]
	c.ourMaxHeaderListSize = c.ourSettings[SettingMaxHeaderListSize]

	enablePushVal, ok := c.ourSettings[SettingEnablePush]
	c.ourEnablePush = (ok && enablePushVal == 1)
}

// applyPeerSettings updates connection's operational parameters based on conn.peerSettings.
// This should be called when peer's settings are initialized or changed.
// Assumes settingsMu is held if called outside constructor.
func (c *Connection) applyPeerSettings() {
	c.peerMaxFrameSize = c.peerSettings[SettingMaxFrameSize]
	c.peerInitialWindowSize = c.peerSettings[SettingInitialWindowSize]
	c.peerMaxConcurrentStreams = c.peerSettings[SettingMaxConcurrentStreams]
	c.peerMaxHeaderListSize = c.peerSettings[SettingMaxHeaderListSize]

	// Update HPACK encoder's dynamic table size limit based on peer's SettingHeaderTableSize
	if c.hpackAdapter != nil {
		peerHpackTableSize := c.peerSettings[SettingHeaderTableSize]
		c.hpackAdapter.SetMaxEncoderDynamicTableSize(peerHpackTableSize)
	}
}

// canCreateStream checks if a new stream can be created based on concurrency limits.
// isInitiatedByPeer indicates if the stream creation is initiated by the peer.
func (c *Connection) canCreateStream(isInitiatedByPeer bool) bool {
	c.settingsMu.Lock()
	c.streamsMu.RLock() // RLock for reading concurrent stream counts

	var limit uint32
	var currentCount int

	if isInitiatedByPeer {
		limit = c.ourMaxConcurrentStreams
		currentCount = c.concurrentStreamsInbound
	} else {
		limit = c.peerMaxConcurrentStreams
		currentCount = c.concurrentStreamsOutbound
	}
	// Unlock order: streamsMu first, then settingsMu
	c.streamsMu.RUnlock()
	c.settingsMu.Unlock()

	// A setting of 0 for MAX_CONCURRENT_STREAMS means no new streams of that type are allowed.
	// RFC 7540, Section 5.1.2: "A value of 0 for SETTINGS_MAX_CONCURRENT_STREAMS SHOULD NOT be treated as special by endpoints."
	// However, a common interpretation (and practical one for servers setting a limit) is that 0 means "disallow".
	// The spec also states: "SETTINGS_MAX_CONCURRENT_STREAMS (0x3): ...This limit is directional: it applies to the number of streams that the sender of the setting can create."
	// So, if WE send MAX_CONCURRENT_STREAMS = N, the PEER can open N streams.
	// If PEER sends MAX_CONCURRENT_STREAMS = M, WE can open M streams.
	// If isInitiatedByPeer is true, PEER is opening, so our limit (ourMaxConcurrentStreams) applies.
	// If isInitiatedByPeer is false, WE are opening, so PEER's limit (peerMaxConcurrentStreams) applies.

	if limit == 0 { // If the limit is explicitly set to 0, no streams allowed.
		return false
	}
	// If limit is not 0 (common case: large default or specific value), check count.
	// Note: MaxConcurrentStreams is often treated as "effectively infinite" (e.g. 2^31-1) by default if not set.
	// Our defaults handle this appropriately (0xffffffff before peer settings are known).
	return uint32(currentCount) < limit
}

// createStream creates a new stream, initializes it, and adds it to the connection.
// id: The stream ID, must be validated by the caller for parity and sequence.
// handler: The handler for server-initiated processing of client requests.
// handlerCfg: Configuration for the handler.
// prioInfo: Priority information for the new stream. If nil, default priority is used.
// isInitiatedByPeer: True if the stream is being created due to a peer's action (e.g., receiving HEADERS).
func (c *Connection) createStream(id uint32, handler Handler, handlerCfg json.RawMessage, prioInfo *streamDependencyInfo, isInitiatedByPeer bool) (*Stream, error) {
	// Check concurrency limits first, without holding the full streamsMu write lock yet.
	if !c.canCreateStream(isInitiatedByPeer) {
		return nil, NewConnectionError(ErrCodeRefusedStream, fmt.Sprintf("cannot create stream %d: max concurrent streams limit reached", id))
	}

	c.streamsMu.Lock()
	defer c.streamsMu.Unlock()

	// Re-check concurrency under the full lock, in case counts changed.
	// canCreateStream handles its own locking, so this is a fresh check.
	if !c.canCreateStream(isInitiatedByPeer) {
		return nil, NewConnectionError(ErrCodeRefusedStream, fmt.Sprintf("cannot create stream %d: max concurrent streams limit reached (re-check)", id))
	}

	if _, ok := c.streams[id]; ok {
		return nil, NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("cannot create stream %d: stream already exists", id))
	}

	// Determine priority values
	var weight uint8
	var parentID uint32
	var exclusive bool

	if prioInfo != nil {
		weight = prioInfo.Weight
		parentID = prioInfo.StreamDependency
		exclusive = prioInfo.Exclusive
	} else {
		// Default priority: weight 16 (frame value 15), parent 0, not exclusive
		weight = 15 // Default weight of 16 is represented by frame value 15
		parentID = 0
		exclusive = false
	}

	// Use current initial window sizes from settings
	c.settingsMu.Lock()
	currentOurInitialWindowSize := c.ourInitialWindowSize
	currentPeerInitialWindowSize := c.peerInitialWindowSize
	c.settingsMu.Unlock()

	stream, err := newStream(
		c, // parent connection
		id,
		currentOurInitialWindowSize,
		currentPeerInitialWindowSize,
		handler,
		handlerCfg,
		weight,
		parentID,
		exclusive,
	)
	if err != nil {
		return nil, NewConnectionError(ErrCodeInternalError, fmt.Sprintf("failed to create new stream object for ID %d: %v", id, err))
	}

	c.streams[id] = stream

	// Add to priority tree using the resolved priority info
	actualPrioInfoForTree := &streamDependencyInfo{
		StreamDependency: parentID,
		Weight:           weight,
		Exclusive:        exclusive,
	}
	if errPrio := c.priorityTree.AddStream(id, actualPrioInfoForTree); errPrio != nil {
		delete(c.streams, id) // Rollback adding to c.streams
		c.log.Error("Failed to add stream to priority tree", logger.LogFields{"streamID": id, "error": errPrio.Error()})
		return nil, NewConnectionError(ErrCodeInternalError, fmt.Sprintf("failed to add stream %d to priority tree: %v", id, errPrio))
	}

	if isInitiatedByPeer {
		c.concurrentStreamsInbound++
	} else {
		c.concurrentStreamsOutbound++
	}

	// Update lastProcessedStreamID if this stream ID is higher.
	// This is relevant for GOAWAY processing.
	if id > c.lastProcessedStreamID {
		c.lastProcessedStreamID = id
	}

	c.log.Debug("Stream created", logger.LogFields{"streamID": id, "isPeerInitiated": isInitiatedByPeer, "handlerType": fmt.Sprintf("%T", handler)})
	return stream, nil
}

// getStream retrieves an active stream by its ID.
// Returns the stream and true if found, otherwise nil and false.
func (c *Connection) getStream(id uint32) (*Stream, bool) {
	c.streamsMu.RLock()
	defer c.streamsMu.RUnlock()
	stream, ok := c.streams[id]
	return stream, ok
}

// removeStream removes a stream from the connection's active list and cleans up its resources.
// id: The ID of the stream to remove.
// initiatedByPeer: Must accurately reflect if the stream was initiated by the peer,
//
//	used for decrementing the correct concurrent stream counter.
//
// errCode: The HTTP/2 error code to use if an RST_STREAM needs to be sent or for logging the reason for removal.
func (c *Connection) removeStream(id uint32, initiatedByPeer bool, errCode ErrorCode) {
	var streamToClose *Stream
	var found bool

	c.streamsMu.Lock()
	streamToClose, found = c.streams[id]
	if found {
		delete(c.streams, id)
		if initiatedByPeer {
			if c.concurrentStreamsInbound > 0 {
				c.concurrentStreamsInbound--
			}
		} else {
			if c.concurrentStreamsOutbound > 0 {
				c.concurrentStreamsOutbound--
			}
		}
	}
	c.streamsMu.Unlock()

	if !found {
		c.log.Debug("Attempted to remove non-existent stream", logger.LogFields{"streamID": id})
		return
	}

	c.log.Debug("Removing stream", logger.LogFields{"streamID": id, "reasonCode": errCode.String()})

	// Remove from priority tree
	if err := c.priorityTree.RemoveStream(id); err != nil {
		c.log.Warn("Error removing stream from priority tree", logger.LogFields{"streamID": id, "error": err.Error()})
		// Continue with stream closure anyway
	}

	// Close the stream itself. This should handle state transitions and resource cleanup.
	// Pass a StreamError to stream.Close if an error code is provided.
	var closeErr error
	if errCode != ErrCodeNoError && errCode != ErrCodeCancel { // NoError and Cancel might imply graceful or already handled RST
		closeErr = NewStreamError(id, errCode, "stream removed by connection")
	}
	// streamToClose.Close might send an RST_STREAM if the stream isn't already closed from both ends.
	// The error from stream.Close() is typically about failures to send RST_STREAM, etc.
	if err := streamToClose.Close(closeErr); err != nil {
		c.log.Warn("Error during stream.Close() while removing stream", logger.LogFields{"streamID": id, "error": err.Error()})
	}

	// Notify the connection that the stream's handler goroutine (if any) should be considered done,
	// allowing the connection to potentially decrement a counter of active handlers.
	// This logic might be part of a more comprehensive server.activeHandlers accounting.
	// For now, this is a placeholder for where such a call would go.
	// c.streamHandlerDone(streamToClose)
}

// sendHeadersFrame sends a HEADERS frame.
// This is a stub implementation.
func (c *Connection) sendHeadersFrame(s *Stream, headers []hpack.HeaderField, endStream bool) error {
	// TODO: Implement actual frame creation and sending via c.writerChan
	c.log.Debug("sendHeadersFrame called (stub)", logger.LogFields{"streamID": s.id, "num_headers": len(headers), "endStream": endStream})
	if s == nil {
		return fmt.Errorf("sendHeadersFrame: stream is nil")
	}
	// Simulate sending by logging. In a real implementation, this would:
	// 1. Construct a HEADERS frame.
	// 2. Encode headers using HPACK (c.hpackAdapter).
	// 3. Fragment into HEADERS and CONTINUATION if necessary, respecting c.peerMaxFrameSize.
	// 4. Send frame(s) to c.writerChan.
	return nil
}

// sendDataFrame sends a DATA frame.
// This is a stub implementation.
func (c *Connection) sendDataFrame(s *Stream, data []byte, endStream bool) (int, error) {
	// TODO: Implement actual frame creation and sending via c.writerChan
	c.log.Debug("sendDataFrame called (stub)", logger.LogFields{"streamID": s.id, "data_len": len(data), "endStream": endStream})
	if s == nil {
		return 0, fmt.Errorf("sendDataFrame: stream is nil")
	}
	// Simulate sending by logging. In a real implementation, this would:
	// 1. Construct a DATA frame.
	// 2. Send frame to c.writerChan.
	// (Flow control acquisition is handled by the Stream's WriteData method before calling this)
	return len(data), nil
}

// sendRSTStreamFrame sends an RST_STREAM frame.
// This is a stub implementation.
func (c *Connection) sendRSTStreamFrame(streamID uint32, errorCode ErrorCode) error {
	c.log.Debug("sendRSTStreamFrame called (stub)", logger.LogFields{"streamID": streamID, "errorCode": errorCode.String()})
	// TODO: Implement actual frame creation and sending via c.writerChan
	// 1. Construct RST_STREAM frame.
	// 2. Send to c.writerChan.
	return nil
}

// sendWindowUpdateFrame sends a WINDOW_UPDATE frame.
// This is a stub for stream.go, which expects this method on connection.
func (c *Connection) sendWindowUpdateFrame(streamID uint32, increment uint32) error {
	c.log.Debug("sendWindowUpdateFrame called (stub)", logger.LogFields{"streamID": streamID, "increment": increment})
	// In a real implementation:
	// 1. Create a WindowUpdateFrame.
	// 2. Set its StreamID and WindowSizeIncrement.
	// 3. Send it to c.writerChan.
	return nil
}

// isTLS is a stub to satisfy stream.go
func (c *Connection) isTLS() bool {
	// TODO: Determine this properly, e.g., by checking c.netConn type or a flag.
	return true // Assuming TLS for now for default "https" scheme
}

// streamHandlerDone is a stub to satisfy stream.go
func (c *Connection) streamHandlerDone(s *Stream) {
	c.log.Debug("streamHandlerDone called (stub)", logger.LogFields{"streamID": s.id})
	// This would typically be used to manage lifecycle of handler goroutines,
	// perhaps decrementing an active handler counter for graceful shutdown.
}

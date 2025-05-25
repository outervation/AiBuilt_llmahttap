package http2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/net/http2/hpack"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/router"
	"example.com/llmahttap/v2/internal/server"
)

// ClientPreface is the connection preface string that clients must send.
const ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// SettingsAckTimeoutDuration is the default time to wait for a SETTINGS ACK.
const SettingsAckTimeoutDuration = 10 * time.Second

// Default settings values (RFC 7540 Section 6.5.2)
// MinMaxFrameSize is the minimum value for SETTINGS_MAX_FRAME_SIZE (2^14).
const MinMaxFrameSize uint32 = 1 << 14

// MaxAllowedFrameSizeValue is the maximum value for SETTINGS_MAX_FRAME_SIZE (2^24-1).
const MaxAllowedFrameSizeValue uint32 = (1 << 24) - 1

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
	streamsMu                sync.RWMutex
	streams                  map[uint32]*Stream
	nextStreamIDClient       uint32 // Next client-initiated stream ID (odd), server consumes
	nextStreamIDServer       uint32 // Next server-initiated stream ID (even), server produces (for PUSH)
	lastProcessedStreamID    uint32 // Highest stream ID processed or accepted for GOAWAY
	peerReportedLastStreamID uint32 // Highest stream ID peer reported processing in a GOAWAY frame (initially max_uint32)
	priorityTree             *PriorityTree
	hpackAdapter             *HpackAdapter
	connFCManager            *ConnectionFlowControlManager
	goAwaySent               bool
	goAwayReceived           bool
	gracefulShutdownTimer    *time.Timer
	activePings              map[[8]byte]*time.Timer // Tracks outstanding PINGs and their timeout timers
	activePingsMu            sync.Mutex

	// Header block assembly state
	activeHeaderBlockStreamID     uint32                // Stream ID of the current header block being assembled
	headerFragments               [][]byte              // Buffer for incoming header block fragments
	headerFragmentTotalSize       uint32                // Cumulative size of received fragments for current block
	headerFragmentInitialType     FrameType             // Type of the frame that started the header block (HEADERS or PUSH_PROMISE)
	headerFragmentPromisedID      uint32                // PromisedStreamID if initial frame was PUSH_PROMISE
	headerFragmentEndStream       bool                  // Records if the initial HEADERS indicated END_STREAM for the logical header block.
	headerFragmentInitialPrioInfo *streamDependencyInfo // Priority info from the initial HEADERS frame, if present
	ourSettings                   map[SettingID]uint32
	settingsMu                    sync.RWMutex // Protects ourSettings and peerSettings
	peerSettings                  map[SettingID]uint32

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

	dispatcher server.RouterInterface // For dispatching requests to application layer
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
	dispatcher server.RouterInterface, // Added dispatcher
) *Connection {
	ctx, cancel := context.WithCancel(context.Background())

	if dispatcher == nil && !isClientSide { // Dispatcher is crucial for server-side operations
		// For client side, it might be nil if client doesn't process responses in a complex way (e.g. just one request)
		// but for a server, it's required.
		lg.Error("NewConnection: server-side connection created without a dispatcher", logger.LogFields{})
		// Depending on how critical this is, might panic or return error.
		// For now, log and continue, but this setup is likely problematic.
	}

	conn := &Connection{
		netConn:                  nc,
		log:                      lg,
		isClient:                 isClientSide,
		ctx:                      ctx,
		cancelCtx:                cancel,
		readerDone:               make(chan struct{}),
		writerDone:               make(chan struct{}),
		shutdownChan:             make(chan struct{}),
		streams:                  make(map[uint32]*Stream),
		priorityTree:             NewPriorityTree(),
		connFCManager:            NewConnectionFlowControlManager(),
		writerChan:               make(chan Frame, 64), // Increased buffer
		activePings:              make(map[[8]byte]*time.Timer),
		ourSettings:              make(map[SettingID]uint32),
		peerSettings:             make(map[SettingID]uint32),
		remoteAddrStr:            nc.RemoteAddr().String(),
		dispatcher:               dispatcher, // Store dispatcher
		peerReportedLastStreamID: 0xffffffff, // Initialize to max uint32, indicating no GOAWAY received yet or peer processes all streams
	}

	// Initialize client/server stream ID counters
	if isClientSide {
		conn.nextStreamIDClient = 1
		// Server-initiated stream IDs are even. Clients don't initiate with even IDs.
		// If this client were to support receiving PUSH_PROMISE, nextStreamIDServer would track expected even IDs.
		conn.nextStreamIDServer = 0 // Will not be used by server-side Conn
	} else { // Server side
		conn.nextStreamIDClient = 0 // Server expects client to start with stream ID 1, this tracks highest processed.
		conn.nextStreamIDServer = 2 // First server-initiated PUSH_PROMISE will use ID 2
	}

	// Initialize default settings values for peer (will be updated upon receiving peer's SETTINGS frame)
	conn.peerSettings[SettingHeaderTableSize] = DefaultSettingsHeaderTableSize
	conn.peerSettings[SettingEnablePush] = DefaultServerEnablePush // Assume peer server might push if conn is client
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
func (c *Connection) createStream(id uint32, handler server.Handler, handlerCfg json.RawMessage, prioInfo *streamDependencyInfo, isInitiatedByPeer bool) (*Stream, error) {
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
		isInitiatedByPeer, // Added missing argument
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
	c.log.Debug("Queuing RST_STREAM frame for sending", logger.LogFields{"stream_id": streamID, "error_code": errorCode.String()})

	// Check if stream is 0, RST_STREAM is not allowed on stream 0.
	// RFC 7540, Section 6.4: "RST_STREAM frames MUST be associated with a stream.
	// If a RST_STREAM frame is received with a stream identifier of 0x0, the recipient MUST
	// treat this as a connection error (Section 5.4.1) of type PROTOCOL_ERROR."
	// This check is for *sending*; our server should never try to send RST_STREAM on stream 0.
	if streamID == 0 {
		errMsg := "internal error: attempted to send RST_STREAM on stream 0"
		c.log.Error(errMsg, logger.LogFields{"error_code": errorCode.String()})
		// This is an internal server error if our code tries this.
		return NewConnectionError(ErrCodeInternalError, errMsg)
	}

	rstFrame := &RSTStreamFrame{
		FrameHeader: FrameHeader{
			Type:     FrameRSTStream,
			Flags:    0, // RST_STREAM frames do not define any flags.
			StreamID: streamID,
			Length:   4, // RST_STREAM payload is always 4 octets (ErrorCode)
		},
		ErrorCode: errorCode,
	}

	// Send the frame to the writer goroutine.
	select {
	case c.writerChan <- rstFrame:
		c.log.Debug("RST_STREAM frame queued", logger.LogFields{"stream_id": streamID, "error_code": errorCode.String()})
		return nil // Successfully queued
	case <-c.shutdownChan:
		// Connection is shutting down, cannot send new frames.
		c.log.Warn("Connection shutting down, cannot send RST_STREAM frame.",
			logger.LogFields{"stream_id": streamID, "error_code": errorCode.String()})
		// Return an error indicating the connection state.
		return NewConnectionError(ErrCodeConnectError, // Using ConnectError as a general "connection unavailable"
			fmt.Sprintf("connection shutting down, cannot send RST_STREAM for stream %d", streamID))
	default:
		// writerChan is full, indicates writer is blocked or not processing. This is a critical internal issue.
		c.log.Error("Failed to queue RST_STREAM frame: writer channel full or blocked.",
			logger.LogFields{"stream_id": streamID, "error_code": errorCode.String()})
		// This implies the connection is unhealthy and likely needs to be terminated.
		return NewConnectionError(ErrCodeInternalError,
			fmt.Sprintf("writer channel congested, cannot send RST_STREAM for stream %d", streamID))
	}
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

// dispatchDataFrame handles an incoming DATA frame.
// It performs connection-level flow control accounting and then dispatches
// the frame to the appropriate stream for stream-level processing.
func (c *Connection) dispatchDataFrame(frame *DataFrame) error {
	streamID := frame.Header().StreamID
	if streamID == 0 {
		return NewConnectionError(ErrCodeProtocolError, "DATA frame received on stream 0")
	}

	// Account for frame payload length. Assumes frame.Data is the actual data after de-padding.
	payloadLen := uint32(len(frame.Data))

	// 1. Connection-level flow control update
	// This must happen regardless of the stream's state, as the bytes were received on the connection.
	if err := c.connFCManager.DataReceived(payloadLen); err != nil {
		c.log.Error("Connection flow control error on DATA frame",
			logger.LogFields{"stream_id": streamID, "payload_len": payloadLen, "error": err.Error()})
		// connFCManager.DataReceived should return a ConnectionError with FLOW_CONTROL_ERROR
		return err
	}

	// 2. Find the stream
	stream, found := c.getStream(streamID)

	if !found {
		// Stream does not exist in our active map.
		c.streamsMu.RLock() // RLock to safely read lastProcessedStreamID
		lastKnownStreamID := c.lastProcessedStreamID
		c.streamsMu.RUnlock()

		if streamID <= lastKnownStreamID {
			// Stream was known but is now closed. Peer should not send DATA.
			// Send RST_STREAM(STREAM_CLOSED). Connection FC already handled.
			c.log.Warn("DATA frame for closed stream", logger.LogFields{"stream_id": streamID})
			return c.sendRSTStreamFrame(streamID, ErrCodeStreamClosed)
		}
		// Stream ID is higher than any we've processed. Client sent DATA before HEADERS.
		// This is a connection error. Connection FC handled, but this error is fatal.
		return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("DATA frame on unopened stream %d", streamID))
	}

	// 3. Check stream state (before dispatching to stream.handleDataFrame)
	// stream.handleDataFrame will also check, but good to do a preliminary check here.
	stream.mu.RLock()
	state := stream.state
	canReceiveData := (state == StreamStateOpen || state == StreamStateHalfClosedLocal)
	stream.mu.RUnlock()

	if !canReceiveData {
		c.log.Warn("DATA frame for stream in invalid state",
			logger.LogFields{"stream_id": streamID, "state": state.String()})
		// Per RFC 7540, 6.1: "If an endpoint receives a DATA frame for a stream
		// that is not in the "open" or "half-closed (local)" state, it MUST respond
		// with a stream error (Section 5.4.2) of type STREAM_CLOSED."
		// Connection FC already handled.
		return c.sendRSTStreamFrame(streamID, ErrCodeStreamClosed)
	}

	// 4. Dispatch to stream for stream-level processing
	if err := stream.handleDataFrame(frame); err != nil {
		// stream.handleDataFrame might return a StreamError (e.g., stream FC violation)
		// or a ConnectionError if something catastrophic happened at stream level.
		if se, ok := err.(*StreamError); ok {
			c.log.Warn("Stream error handling DATA frame",
				logger.LogFields{"stream_id": se.StreamID, "code": se.Code.String(), "msg": se.Msg})
			return c.sendRSTStreamFrame(se.StreamID, se.Code)
		}
		// If it's a ConnectionError or other fatal error, propagate it.
		return err
	}

	return nil
}

// resetHeaderAssemblyState clears the state related to assembling a header block.
func (c *Connection) resetHeaderAssemblyState() {
	c.activeHeaderBlockStreamID = 0
	c.headerFragments = nil // Allow GC to collect the slices
	c.headerFragmentTotalSize = 0
	c.headerFragmentInitialType = 0
	c.headerFragmentPromisedID = 0
	c.headerFragmentEndStream = false
}

// processContinuationFrame processes an incoming CONTINUATION frame.
func (c *Connection) processContinuationFrame(frame *ContinuationFrame) error {
	header := frame.Header()

	if c.activeHeaderBlockStreamID == 0 || len(c.headerFragments) == 0 {
		return NewConnectionError(ErrCodeProtocolError, "CONTINUATION frame received without active HEADERS/PUSH_PROMISE")
	}
	if header.StreamID != c.activeHeaderBlockStreamID {
		return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("CONTINUATION frame on stream %d does not match active header block stream %d", header.StreamID, c.activeHeaderBlockStreamID))
	}

	// Max header list size check (cumulative, on compressed size)
	c.settingsMu.Lock()
	maxHeaderListSizeBytes := c.ourMaxHeaderListSize
	c.settingsMu.Unlock()

	newTotalSize := c.headerFragmentTotalSize + uint32(len(frame.HeaderBlockFragment))
	if newTotalSize > maxHeaderListSizeBytes && maxHeaderListSizeBytes > 0 {
		msg := fmt.Sprintf("CONTINUATION frame causes header block (stream %d) to exceed preliminary max size (%d > %d)",
			c.activeHeaderBlockStreamID, newTotalSize, maxHeaderListSizeBytes)
		c.log.Error(msg, logger.LogFields{})
		c.resetHeaderAssemblyState()                         // Abort assembly
		return NewConnectionError(ErrCodeProtocolError, msg) // Or ENHANCE_YOUR_CALM
	}

	c.headerFragments = append(c.headerFragments, frame.HeaderBlockFragment)
	c.headerFragmentTotalSize = newTotalSize

	if header.Flags&FlagContinuationEndHeaders != 0 {
		// END_HEADERS is set, this completes the block.
		// Use the stored priority info from the *initial* frame of this block.
		return c.finalizeHeaderBlockAndDispatch(c.headerFragmentInitialPrioInfo)
	}
	// END_HEADERS not set, expect more CONTINUATION frames.
	return nil
}

// finalizeHeaderBlockAndDispatch is called when a complete header block (HEADERS/PUSH_PROMISE + any CONTINUATIONs)
// has been received (indicated by END_HEADERS flag). It concatenates fragments, decodes,
// validates, and then dispatches the headers.
func (c *Connection) finalizeHeaderBlockAndDispatch(initialFramePrioInfo *streamDependencyInfo) error {
	if c.activeHeaderBlockStreamID == 0 || len(c.headerFragments) == 0 {
		// Should not happen if called correctly.
		c.resetHeaderAssemblyState() // Ensure clean state even if this happens
		return NewConnectionError(ErrCodeInternalError, "finalizeHeaderBlockAndDispatch called with no active header block")
	}

	// Concatenate all fragments
	totalLen := 0
	for _, frag := range c.headerFragments {
		totalLen += len(frag)
	}
	fullHeaderBlock := make([]byte, 0, totalLen)
	for _, frag := range c.headerFragments {
		fullHeaderBlock = append(fullHeaderBlock, frag...)
	}

	// Decode using HPACK
	c.hpackAdapter.ResetDecoderState() // Ensure clean state for new block
	if err := c.hpackAdapter.DecodeFragment(fullHeaderBlock); err != nil {
		c.log.Error("HPACK decoding error (fragment processing)", logger.LogFields{"stream_id": c.activeHeaderBlockStreamID, "error": err})
		c.resetHeaderAssemblyState()
		return NewConnectionError(ErrCodeCompressionError, "HPACK decode fragment error: "+err.Error())
	}
	decodedHeaders, err := c.hpackAdapter.FinishDecoding()
	if err != nil {
		c.log.Error("HPACK decoding error (finish decoding)", logger.LogFields{"stream_id": c.activeHeaderBlockStreamID, "error": err})
		c.resetHeaderAssemblyState()
		return NewConnectionError(ErrCodeCompressionError, "HPACK finish decoding error: "+err.Error())
	}

	// Check MAX_HEADER_LIST_SIZE (uncompressed)
	var uncompressedSize uint32
	for _, hf := range decodedHeaders {
		uncompressedSize += uint32(len(hf.Name)) + uint32(len(hf.Value)) + 32 // As per RFC 7540, Section 6.5.2
	}

	c.settingsMu.Lock()
	actualMaxHeaderListSize := c.ourMaxHeaderListSize
	c.settingsMu.Unlock()

	if actualMaxHeaderListSize > 0 && uncompressedSize > actualMaxHeaderListSize {
		msg := fmt.Sprintf("decoded header list size (%d) exceeds SETTINGS_MAX_HEADER_LIST_SIZE (%d) for stream %d",
			uncompressedSize, actualMaxHeaderListSize, c.activeHeaderBlockStreamID)
		c.log.Error(msg, logger.LogFields{})
		c.resetHeaderAssemblyState()
		// This is a resource limit violation. ENHANCE_YOUR_CALM or PROTOCOL_ERROR.
		return NewConnectionError(ErrCodeEnhanceYourCalm, msg)
	}

	// Store relevant state before resetting, as dispatch might be complex.
	streamID := c.activeHeaderBlockStreamID
	initialType := c.headerFragmentInitialType
	promisedID := c.headerFragmentPromisedID
	endStreamFlag := c.headerFragmentEndStream // This flag is from the *initial* HEADERS frame.

	// The prioInfo passed to this function is from the initial frame.
	// It's `initialFramePrioInfo`.

	c.resetHeaderAssemblyState() // Reset state *before* dispatching.

	switch initialType {
	case FrameHeaders:
		c.log.Debug("Dispatching assembled HEADERS", logger.LogFields{"stream_id": streamID, "num_headers": len(decodedHeaders), "end_stream_flag_on_headers": endStreamFlag})
		err = c.handleIncomingCompleteHeaders(streamID, decodedHeaders, endStreamFlag, initialFramePrioInfo)
		if err != nil {
			return err
		}

	case FramePushPromise:
		c.log.Debug("Dispatching assembled PUSH_PROMISE", logger.LogFields{"associated_stream_id": streamID, "promised_stream_id": promisedID, "num_headers": len(decodedHeaders)})
		// TODO: Implement client-side PUSH_PROMISE handling.
		// This involves:
		// 1. Validating promisedID.
		// 2. Creating a new stream in "reserved (remote)" state for promisedID.
		// 3. Storing the pushed request headers.
		// 4. Client application logic decides whether to accept or RST_STREAM(CANCEL) the pushed stream.
	default:
		// This should be unreachable if state is managed correctly.
		return NewConnectionError(ErrCodeInternalError, fmt.Sprintf("invalid initial frame type %v in finalizeHeaderBlockAndDispatch", initialType))
	}

	return nil
}

func (c *Connection) processHeadersFrame(frame *HeadersFrame) error {
	header := frame.Header()
	if header.StreamID == 0 {
		return NewConnectionError(ErrCodeProtocolError, "HEADERS frame received on stream 0")
	}
	// Server: Stream ID must be odd for client-initiated.
	if !c.isClient && (header.StreamID%2 == 0) {
		// Client should not send HEADERS on an even stream ID unless it's related to a PUSH_PROMISE
		// it initiated (which isn't a thing). Or if client is broken.
		return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("server received HEADERS on even stream ID %d", header.StreamID))
	}
	// Client: Stream ID should be odd for requests it sent, or even for server pushed responses.
	// For a HEADERS frame received by a client, it could be a response to its own request (odd ID)
	// or the start of a pushed response (even ID, after a PUSH_PROMISE for that ID).

	if c.activeHeaderBlockStreamID != 0 {
		// A new HEADERS frame arrived while another header block (possibly on a different stream)
		// was still being assembled (expecting CONTINUATION). This is a PROTOCOL_ERROR.
		// Section 6.10: "A CONTINUATION frame MUST be preceded by a HEADERS, PUSH_PROMISE or
		// CONTINUATION frame without the END_HEADERS flag set."
		// Implicitly, any other frame type terminates the sequence.
		msg := fmt.Sprintf("HEADERS frame for stream %d received while header block for stream %d is active", header.StreamID, c.activeHeaderBlockStreamID)
		c.log.Error(msg, logger.LogFields{})
		c.resetHeaderAssemblyState() // Clear previous partial state.
		return NewConnectionError(ErrCodeProtocolError, msg)
	}

	// Max header list size check (preliminary, on compressed size of this first fragment)
	c.settingsMu.Lock()
	maxHeaderListSizeBytes := c.ourMaxHeaderListSize
	c.settingsMu.Unlock()

	if uint32(len(frame.HeaderBlockFragment)) > maxHeaderListSizeBytes && maxHeaderListSizeBytes > 0 {
		msg := fmt.Sprintf("HEADERS frame fragment size (%d) exceeds preliminary max header list size (%d) for stream %d",
			len(frame.HeaderBlockFragment), maxHeaderListSizeBytes, header.StreamID)
		c.log.Error(msg, logger.LogFields{})
		// This is a fatal error for the connection, as per MAX_HEADER_LIST_SIZE description.
		return NewConnectionError(ErrCodeEnhanceYourCalm, msg) // Or PROTOCOL_ERROR
	}

	var prioInfoOnThisFrame *streamDependencyInfo
	if header.Flags&FlagHeadersPriority != 0 {
		prioInfoOnThisFrame = &streamDependencyInfo{
			StreamDependency: frame.StreamDependency,
			Weight:           frame.Weight,
			Exclusive:        frame.Exclusive,
		}
	}

	c.activeHeaderBlockStreamID = header.StreamID
	c.headerFragments = append([][]byte{}, frame.HeaderBlockFragment) // Start new list
	c.headerFragmentTotalSize = uint32(len(frame.HeaderBlockFragment))
	c.headerFragmentInitialType = FrameHeaders
	c.headerFragmentPromisedID = 0 // Not a PUSH_PROMISE
	c.headerFragmentEndStream = (header.Flags & FlagHeadersEndStream) != 0
	c.headerFragmentInitialPrioInfo = prioInfoOnThisFrame // Store priority from this frame

	if header.Flags&FlagHeadersEndHeaders != 0 {
		// END_HEADERS is set, this is a complete block.
		// Pass prioInfoOnThisFrame as it's from the current, initial frame of the block.
		return c.finalizeHeaderBlockAndDispatch(prioInfoOnThisFrame)
	}
	// END_HEADERS not set, expect CONTINUATION frames.
	return nil
}

func (c *Connection) processPushPromiseFrame(frame *PushPromiseFrame) error {
	header := frame.Header()
	if header.StreamID == 0 {
		return NewConnectionError(ErrCodeProtocolError, "PUSH_PROMISE frame received on stream 0")
	}
	if frame.PromisedStreamID == 0 || frame.PromisedStreamID%2 != 0 { // Promised ID must be non-zero and even
		return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("invalid PromisedStreamID %d in PUSH_PROMISE on stream %d", frame.PromisedStreamID, header.StreamID))
	}
	if !c.isClient { // Only clients should receive PUSH_PROMISE
		return NewConnectionError(ErrCodeProtocolError, "server received PUSH_PROMISE frame")
	}

	if c.activeHeaderBlockStreamID != 0 {
		msg := fmt.Sprintf("PUSH_PROMISE frame for stream %d (promised %d) received while header block for stream %d is active", header.StreamID, frame.PromisedStreamID, c.activeHeaderBlockStreamID)
		c.log.Error(msg, logger.LogFields{})
		c.resetHeaderAssemblyState()
		return NewConnectionError(ErrCodeProtocolError, msg)
	}

	c.settingsMu.Lock()
	serverPushEnabled := c.ourEnablePush
	maxHeaderListSizeBytes := c.ourMaxHeaderListSize
	c.settingsMu.Unlock()

	if !serverPushEnabled {
		// Client has disabled push, server should not send PUSH_PROMISE.
		// Client RSTs the *promised* stream ID.
		// Since we haven't created it, we just note the protocol violation from peer.
		c.log.Warn("Received PUSH_PROMISE when server push is disabled by client settings.", logger.LogFields{"promisedStreamID": frame.PromisedStreamID})
		// We should RST_STREAM the promised stream with CANCEL or PROTOCOL_ERROR.
		// Since the stream doesn't exist locally yet, we can't use stream.sendRSTStream.
		// The spec (8.2) says "An endpoint that receives a PUSH_PROMISE frame for which it has SETTINGS_ENABLE_PUSH set to 0 MUST treat the PUSH_PROMISE frame as a connection error (Section 5.4.1) of type PROTOCOL_ERROR."
		return NewConnectionError(ErrCodeProtocolError, "received PUSH_PROMISE when server push is disabled by client")

	}

	if uint32(len(frame.HeaderBlockFragment)) > maxHeaderListSizeBytes && maxHeaderListSizeBytes > 0 {
		msg := fmt.Sprintf("PUSH_PROMISE frame fragment size (%d) exceeds preliminary max header list size (%d) for stream %d, promised %d",
			len(frame.HeaderBlockFragment), maxHeaderListSizeBytes, header.StreamID, frame.PromisedStreamID)
		c.log.Error(msg, logger.LogFields{})
		return NewConnectionError(ErrCodeEnhanceYourCalm, msg) // Or PROTOCOL_ERROR
	}

	c.activeHeaderBlockStreamID = header.StreamID // Associated stream, NOT promised stream
	c.headerFragments = append(c.headerFragments, frame.HeaderBlockFragment)
	c.headerFragmentTotalSize = uint32(len(frame.HeaderBlockFragment))
	c.headerFragmentInitialType = FramePushPromise
	c.headerFragmentPromisedID = frame.PromisedStreamID
	c.headerFragmentEndStream = false // PUSH_PROMISE itself doesn't end the associated stream.

	if header.Flags&FlagPushPromiseEndHeaders != 0 {
		return c.finalizeHeaderBlockAndDispatch(nil) // PUSH_PROMISE frames don't have their own priority info in this context.
	}
	return nil
}

func (c *Connection) handleIncomingCompleteHeaders(streamID uint32, headers []hpack.HeaderField, endStream bool, prioInfo *streamDependencyInfo) error {
	c.log.Debug("Handling complete headers",
		logger.LogFields{
			"stream_id":         streamID,
			"num_headers":       len(headers),
			"end_stream":        endStream,
			"prio_info_present": prioInfo != nil,
			"is_client_conn":    c.isClient,
		})

	if c.isClient {
		// Client received HEADERS (response or pushed response)
		stream, exists := c.getStream(streamID)
		if !exists {
			c.log.Error("Client received HEADERS for unknown or closed stream", logger.LogFields{"stream_id": streamID})
			return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("client received HEADERS for non-existent stream %d", streamID))
		}

		errClientProcess := stream.processResponseHeaders(headers, endStream)
		if errClientProcess != nil {
			if _, ok := errClientProcess.(*ConnectionError); ok {
				return errClientProcess
			}
			c.log.Error("Error from stream.processResponseHeaders",
				logger.LogFields{"stream_id": streamID, "error": errClientProcess.Error()})
		}
		return nil

	} else {
		// Server received HEADERS (client request)
		if streamID == 0 {
			return NewConnectionError(ErrCodeProtocolError, "server received HEADERS on stream 0")
		}
		if streamID%2 == 0 { // Client-initiated stream ID must be odd
			return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("server received HEADERS on even stream ID %d from client", streamID))
		}

		// Check if stream already exists (client re-using an ID for a new request)
		c.streamsMu.RLock()
		_, exists := c.streams[streamID]
		c.streamsMu.RUnlock()

		if exists {
			c.log.Error("Server received HEADERS for an already existing stream ID from client", logger.LogFields{"stream_id": streamID})
			return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("client attempted to reuse stream ID %d", streamID))
		}

		if c.dispatcher == nil {
			c.log.Error("Dispatcher is nil, cannot route request for new stream", logger.LogFields{"stream_id": streamID})
			_ = c.sendRSTStreamFrame(streamID, ErrCodeInternalError)
			return nil
		}

		// Extract pseudo-headers to determine path for routing BEFORE creating the stream.
		_, path, _, _, pseudoErr := c.extractPseudoHeaders(headers)
		if pseudoErr != nil {
			c.log.Error("Error extracting pseudo headers for routing", logger.LogFields{"stream_id": streamID, "error": pseudoErr.Error()})
			_ = c.sendRSTStreamFrame(streamID, ErrCodeProtocolError) // Send RST for malformed request
			// If pseudoErr is a ConnectionError, it will be propagated up.
			// If it's a simple error, it might be handled locally by RSTing the stream.
			// For now, assume extractPseudoHeaders returns a ConnectionError for fatal issues.
			return pseudoErr
		}

		// Use the router to find the handler and its config based on the path.
		// The dispatcher is server.RouterInterface. Its Match method would be ideal if it existed.
		// Assuming c.dispatcher is a *router.Router which has a Match method.
		// We need to cast or ensure the interface provides this.
		// For now, let's assume a method like `Match(path string) (*config.Route, server.Handler, error)` exists.
		// This part is a bit tricky as router.Match returns (route, handler, err)
		// and the handler is already instantiated. We need the HandlerConfig to pass to newStream.
		// Let's refine this: The router should give us the route, and we get the handler from the registry *inside* createStream or similar.
		// For now, let's assume we can get HandlerType and HandlerConfig from the router.

		var routeConfig *config.Route
		var resolvedHandler server.Handler // This should be instantiated later by the stream
		// var handlerType string // Removed unused variable
		var opaqueHandlerConfig json.RawMessage

		// This is conceptual. The actual router interaction needs to be defined.
		// For now, assume c.dispatcher.Match can give us route details.
		// The router.Router.Match in router.go finds a config.Route and then creates a handler.
		// This needs rethinking. The stream needs the HandlerConfig *at creation*.
		// A possible flow:
		// 1. Router.FindRoute(path) -> returns *config.Route, error
		// 2. If route found, use route.HandlerType and route.HandlerConfig for stream creation.
		// 3. The stream.handler field will be instantiated by the stream itself using its config.

		// Quick check if dispatcher is *router.Router to access its Match method directly for now
		// This is not ideal for interface segregation but helps proceed.
		actualRouter, ok := c.dispatcher.(*router.Router)
		if !ok {
			errMsg := "Dispatcher is not of expected type *router.Router, connection cannot perform routing"
			c.log.Error(errMsg, logger.LogFields{"stream_id": streamID})
			// This is a fundamental issue with the connection's setup.
			return NewConnectionError(ErrCodeInternalError, errMsg)
		}

		matchedRoute, instantiatedHandler, errMatch := actualRouter.Match(path)
		if errMatch != nil {
			c.log.Error("Router Match failed", logger.LogFields{"stream_id": streamID, "path": path, "error": errMatch.Error()})
			// Handle router errors, e.g., if handler creation in Match failed.
			// Send 500 Internal Server Error (if handler creation failed)
			// Server.WriteErrorResponse would be used here on a temporary stream or a way to write directly.
			// For now, RST stream.
			_ = c.sendRSTStreamFrame(streamID, ErrCodeInternalError) // Or specific error from router if possible
			return nil                                               // Don't kill connection, just this stream attempt
		}
		if matchedRoute == nil { // No route matched
			c.log.Info("No route matched for path", logger.LogFields{"stream_id": streamID, "path": path})
			// Need to send a 404. This requires creating a minimal stream to send the response.
			// This is complex here. For now, let's RST. A proper 404 is better.
			// A better approach: create a "dummy" stream, send 404, then close.
			// Or, the main server loop should handle 404 if no handler is found after stream creation.
			// For now, if router.Match returns nil handler for no match:
			if instantiatedHandler == nil {
				// Create a temporary stream to send a 404 Not Found.
				// This requires a way to send an error response without a full handler.
				// This is a simplification:
				// TODO: Implement proper 404 response. This requires creating a stream,
				// sending headers and body, then closing. For now, RST the stream.
				c.log.Warn("TODO: Implement 404 response for stream", logger.LogFields{"stream_id": streamID, "path": path})
				_ = c.sendRSTStreamFrame(streamID, ErrCodeRefusedStream) // Using RefusedStream as a placeholder for "no service"
				return nil
			}
		}

		// We have a matchedRoute and an instantiatedHandler.
		// The stream needs the raw HandlerConfig.
		routeConfig = matchedRoute
		// handlerType = routeConfig.HandlerType // Removed unused variable
		opaqueHandlerConfig = routeConfig.HandlerConfig
		resolvedHandler = instantiatedHandler // This is the instantiated handler

		// Create the stream with the specific handler and its config.
		newStream, streamErr := c.createStream(streamID, resolvedHandler, opaqueHandlerConfig, prioInfo, true /*isPeerInitiated*/)
		if streamErr != nil {
			c.log.Error("Failed to create stream for incoming client HEADERS", logger.LogFields{"stream_id": streamID, "error": streamErr.Error()})
			if ce, ok := streamErr.(*ConnectionError); ok && ce.Code == ErrCodeRefusedStream {
				_ = c.sendRSTStreamFrame(streamID, ErrCodeRefusedStream)
				return nil
			}
			return streamErr
		}

		// Transition stream state based on HEADERS
		newStream.mu.Lock()
		if newStream.state != StreamStateIdle {
			c.log.Error("Newly created stream is not in Idle state before header processing", logger.LogFields{"stream_id": newStream.id, "state": newStream.state.String()})
			newStream.mu.Unlock()
			_ = newStream.Close(NewStreamError(newStream.id, ErrCodeInternalError, "stream in unexpected state"))
			return NewConnectionError(ErrCodeInternalError, "newly created stream in unexpected state")
		}

		if endStream {
			newStream.endStreamReceivedFromClient = true
			if newStream.requestBodyWriter != nil {
				_ = newStream.requestBodyWriter.Close()
			}
			newStream._setState(StreamStateHalfClosedRemote)
		} else {
			newStream._setState(StreamStateOpen)
		}
		newStream.mu.Unlock()

		// Delegate to the stream to process headers and dispatch.
		// The stream's handler is already set from createStream.
		// stream.processRequestHeadersAndDispatch will build the http.Request and call the handler.
		errDispatch := newStream.processRequestHeadersAndDispatch(headers, endStream, c.dispatcher) // dispatcher is passed for context, though stream has its own handler now.
		if errDispatch != nil {
			if _, ok := errDispatch.(*ConnectionError); ok {
				return errDispatch
			}
			c.log.Error("Error from stream.processRequestHeadersAndDispatch",
				logger.LogFields{"stream_id": newStream.id, "error": errDispatch.Error()})
		}
		return nil
	}
}

// extractPseudoHeaders extracts common pseudo-headers like :method, :path, :scheme, :authority.
// It returns the values and an error if required pseudo-headers are missing or malformed.
// This is a simplified helper. A robust implementation needs to handle case-insensitivity for values (though names are fixed)
// and potentially multiple values for other headers (though not for pseudo-headers).

// extractPseudoHeaders extracts common pseudo-headers like :method, :path, :scheme, :authority.
// It also enforces that pseudo-headers appear before regular headers and that :method, :path, and :scheme are present.
// For :authority, it's typically required but can be inferred from Host for direct origin requests (not fully handled here yet).
// extractPseudoHeaders extracts common pseudo-headers like :method, :path, :scheme, :authority.
// It also enforces that pseudo-headers appear before regular headers and that :method, :path, and :scheme are present.
// For :authority, it's typically required but can be inferred from Host for direct origin requests (not fully handled here yet).
func (c *Connection) extractPseudoHeaders(headers []hpack.HeaderField) (method, path, scheme, authority string, err error) {
	required := map[string]*string{
		":method":    &method,
		":path":      &path,
		":scheme":    &scheme,
		":authority": &authority, // :authority is often present, treat as mostly required for now
	}
	found := map[string]bool{
		":method":    false,
		":path":      false,
		":scheme":    false,
		":authority": false,
	}

	pseudoHeadersDone := false

	for _, hf := range headers {
		if !strings.HasPrefix(hf.Name, ":") {
			pseudoHeadersDone = true
			continue // Skip regular headers in this pseudo-header validation phase
		}
		if pseudoHeadersDone {
			return "", "", "", "", NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("pseudo-header %s found after regular header fields", hf.Name))
		}

		target, isRequiredPseudo := required[hf.Name] // Renamed to avoid conflict with local `isRequired`
		if !isRequiredPseudo {                        // Unknown pseudo-header
			// Allow :status for client-side response processing, but this function is primarily for server-side request validation.
			// If this is server side and we see :status, it's an error.
			if hf.Name == ":status" && c.isClient {
				// This function is less about client-side validation. If client calls, let :status pass through.
			} else {
				return "", "", "", "", NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("unknown or invalid pseudo-header: %s", hf.Name))
			}
		}

		if found[hf.Name] { // Duplicate pseudo-header
			return "", "", "", "", NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("duplicate pseudo-header: %s", hf.Name))
		}

		// Only assign if target is not nil (i.e., it's one of the pseudo-headers we track)
		if target != nil {
			*target = hf.Value
		}
		found[hf.Name] = true // Mark as found even if it was an "allowed but not tracked" one like :status on client

		// Validate :path value specifically if it's the :path header
		if hf.Name == ":path" && (hf.Value == "" || (hf.Value[0] != '/' && hf.Value != "*")) {
			return "", "", "", "", NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("invalid :path pseudo-header value: %s", hf.Value))
		}
	}

	if !found[":method"] {
		return "", "", "", "", NewConnectionError(ErrCodeProtocolError, "missing :method pseudo-header")
	}
	if !found[":path"] {
		return "", "", "", "", NewConnectionError(ErrCodeProtocolError, "missing :path pseudo-header")
	}
	if !found[":scheme"] {
		return "", "", "", "", NewConnectionError(ErrCodeProtocolError, "missing :scheme pseudo-header")
	}
	// :authority is also generally required (RFC 7540, 8.1.2.3).
	// "All HTTP/2 requests MUST include exactly one valid value for the :method, :scheme, and :path pseudo-header fields,
	// unless it is a CONNECT request (Section 8.3). An HTTP/2 request that omits mandatory pseudo-header fields is malformed (Section 8.1.2.6)."
	// :authority is mandatory if the request target is not an origin server (e.g. proxy request) or if there's no Host header.
	// For simplicity, require :authority or ensure Host header provides it (more complex logic not added here).
	if !found[":authority"] {
		// Check if 'host' header is present as an alternative (though :authority is preferred).
		// This part is simplified; a full server would handle Host header vs :authority nuances.
		hostHeaderPresent := false
		for _, hf := range headers {
			if !strings.HasPrefix(hf.Name, ":") && strings.ToLower(hf.Name) == "host" {
				if hf.Value != "" {
					authority = hf.Value // Use host header if :authority is missing
					found[":authority"] = true
					hostHeaderPresent = true
				}
				break
			}
		}
		if !hostHeaderPresent {
			return "", "", "", "", NewConnectionError(ErrCodeProtocolError, "missing :authority pseudo-header and no Host header")
		}
	}

	return method, path, scheme, authority, nil
}

// dispatchPriorityFrame handles an incoming PRIORITY frame.
// It validates the frame and updates the PriorityTree.
func (c *Connection) dispatchPriorityFrame(frame *PriorityFrame) error {
	streamID := frame.Header().StreamID

	// PRIORITY frames MUST be associated with an existing stream or a stream that could be
	// created (i.e., not stream 0).
	// RFC 7540, Section 6.3: "A PRIORITY frame with a stream identifier of 0x0 MUST be
	// treated as a connection error (Section 5.4.1) of type PROTOCOL_ERROR."
	// This check should ideally be performed by the general frame dispatcher before
	// routing to this specific handler. If it reaches here, it indicates an issue
	// in the upstream dispatch logic or an unexpected scenario.
	if streamID == 0 {
		c.log.Error("dispatchPriorityFrame called with PRIORITY frame on stream 0",
			logger.LogFields{"frame_type": frame.Header().Type.String()})
		return NewConnectionError(ErrCodeProtocolError, "PRIORITY frame received on stream 0")
	}

	// Process the priority update using the PriorityTree.
	// The PriorityTree.ProcessPriorityFrame method handles internal locking and specific
	// validations like self-dependency (a stream cannot depend on itself).
	err := c.priorityTree.ProcessPriorityFrame(frame)
	if err != nil {
		c.log.Warn("Error processing PRIORITY frame in priority tree",
			logger.LogFields{
				"stream_id":      streamID,
				"dependency":     frame.StreamDependency,
				"weight":         frame.Weight,
				"exclusive":      frame.Exclusive,
				"original_error": err.Error(),
			})

		// Check if the error from ProcessPriorityFrame is a StreamError.
		// This typically occurs if the frame specified a self-dependency.
		if se, ok := err.(*StreamError); ok {
			// The stream ID in the StreamError (se.StreamID) should match frame.StreamID.
			// Send RST_STREAM for the problematic stream.
			rstErr := c.sendRSTStreamFrame(se.StreamID, se.Code)
			if rstErr != nil {
				// Failed to send RST_STREAM. This is a more serious issue, potentially
				// indicating a problem with the connection writer.
				return NewConnectionErrorWithCause(ErrCodeInternalError,
					fmt.Sprintf("failed to send RST_STREAM (code %s) for PRIORITY processing error on stream %d: %v",
						se.Code.String(), se.StreamID, rstErr),
					err, // Include the original error from ProcessPriorityFrame as context
				)
			}
			// Successfully sent RST_STREAM. The stream error is considered handled.
			// No further error propagation to tear down the connection for this specific issue.
			return nil
		}

		// If the error is not a StreamError, it might be an internal issue within
		// the PriorityTree logic or a condition that ProcessPriorityFrame deemed
		// severe but didn't fit into a StreamError.
		// Treat such errors as internal connection errors for now.
		return NewConnectionErrorWithCause(ErrCodeInternalError,
			fmt.Sprintf("internal error processing PRIORITY frame for stream %d: %v", streamID, err),
			err,
		)
	}

	c.log.Debug("PRIORITY frame processed successfully",
		logger.LogFields{
			"stream_id":  streamID,
			"dependency": frame.StreamDependency,
			"weight":     frame.Weight,
			"exclusive":  frame.Exclusive,
		})
	return nil
}

// removeClosedStream removes a stream that is already in a closed state from the connection's tracking.
// This method assumes the stream's state transition to Closed (and associated actions like sending RST if needed)
// has already been handled by the stream itself.
func (c *Connection) removeClosedStream(s *Stream) {
	if s == nil {
		c.log.Warn("removeClosedStream called with nil stream", logger.LogFields{})
		return
	}
	id := s.ID()

	// Ensure stream is actually closed before removing.
	// This read of stream state is primarily for logging/assertion,
	// as the main decision to call this function implies the stream should be closed.
	s.mu.RLock()
	streamState := s.state
	isPeerInit := s.initiatedByPeer
	s.mu.RUnlock()

	if streamState != StreamStateClosed {
		c.log.Error("removeClosedStream called on a non-closed stream", logger.LogFields{"stream_id": id, "current_state": streamState.String()})
		// This indicates a logic error elsewhere. For robustness, one might force-close the stream here,
		// but it's better if the caller ensures the stream is closed.
		// For now, we'll proceed with removing it from connection tracking to prevent leaks.
	}

	c.streamsMu.Lock()
	_, foundInMap := c.streams[id]
	if foundInMap {
		delete(c.streams, id)
		if isPeerInit {
			if c.concurrentStreamsInbound > 0 {
				c.concurrentStreamsInbound--
			} else {
				c.log.Warn("concurrentStreamsInbound was 0 when decrementing", logger.LogFields{"stream_id": id})
			}
		} else {
			if c.concurrentStreamsOutbound > 0 {
				c.concurrentStreamsOutbound--
			} else {
				c.log.Warn("concurrentStreamsOutbound was 0 when decrementing", logger.LogFields{"stream_id": id})
			}
		}
	}
	c.streamsMu.Unlock()

	if !foundInMap {
		// Stream might have been removed by a concurrent operation or was never fully added.
		c.log.Debug("removeClosedStream: stream not found in map or already removed", logger.LogFields{"stream_id": id})
		return // No further action if not in map.
	}

	c.log.Debug("Removing closed stream from connection tracking", logger.LogFields{"stream_id": id})

	// Remove from priority tree
	if err := c.priorityTree.RemoveStream(id); err != nil {
		c.log.Warn("Error removing stream from priority tree during removeClosedStream", logger.LogFields{"stream_id": id, "error": err.Error()})
	}

	// Local resources for the stream object (pipes, context, fcManager) should have been
	// cleaned by stream.closeStreamResourcesProtected() when it transitioned to StreamStateClosed.
}

// dispatchFrame routes an incoming frame to its specific handler.
// This method is typically called by the connection's main reader loop.
func (c *Connection) dispatchFrame(frame Frame) error {
	// Common validations that apply to many frame types before specific dispatch
	// (e.g., stream ID 0 checks for frames that require non-zero ID)
	// can be done here, or left to individual handlers if they vary significantly.
	// Individual handlers currently perform many of these checks.

	switch f := frame.(type) {
	case *DataFrame:
		return c.dispatchDataFrame(f)
	case *HeadersFrame:
		return c.processHeadersFrame(f)
	case *PriorityFrame:
		return c.dispatchPriorityFrame(f)
	case *RSTStreamFrame:
		return c.dispatchRSTStreamFrame(f)
	case *SettingsFrame:
		return c.handleSettingsFrame(f)
	case *PushPromiseFrame:
		// Server-side connections typically don't process PUSH_PROMISE frames they receive.
		// Clients process PUSH_PROMISE frames sent by servers.
		if c.isClient {
			return c.processPushPromiseFrame(f)
		}
		c.log.Warn("Server received PUSH_PROMISE frame, treating as protocol error.", logger.LogFields{"stream_id": frame.Header().StreamID})
		return NewConnectionError(ErrCodeProtocolError, "server received PUSH_PROMISE frame")
	case *PingFrame:
		return c.handlePingFrame(f)
	case *GoAwayFrame:
		return c.handleGoAwayFrame(f)
	case *WindowUpdateFrame:
		return c.dispatchWindowUpdateFrame(f)
	case *ContinuationFrame:
		return c.processContinuationFrame(f)
	case *UnknownFrame:
		// RFC 7540, Section 4.1: "Implementations MUST ignore and discard
		// any frame of a type that is unknown."
		c.log.Warn("Received unknown frame type, ignoring.", logger.LogFields{"type_val": frame.Header().Type, "stream_id": frame.Header().StreamID})
		return nil
	default:
		// This case should ideally not be reached if ReadFrame correctly maps all known types
		// or returns UnknownFrame for types it doesn't specifically parse.
		// If it is reached, it implies an internal inconsistency in frame parsing or type handling.
		errMsg := fmt.Sprintf("dispatchFrame received a frame of an unexpected underlying type: %T", frame)
		c.log.Error(errMsg, logger.LogFields{"frame_header_type": frame.Header().Type.String()})
		return NewConnectionError(ErrCodeInternalError, errMsg)
	}
}

// dispatchRSTStreamFrame handles an incoming RST_STREAM frame.
// It finds the target stream, instructs it to handle the RST (which closes it),
// and then removes the stream from connection tracking.
func (c *Connection) dispatchRSTStreamFrame(frame *RSTStreamFrame) error {
	streamID := frame.Header().StreamID
	errorCode := frame.ErrorCode

	c.log.Debug("Dispatching RST_STREAM frame",
		logger.LogFields{
			"stream_id":  streamID,
			"error_code": errorCode.String(),
		})

	if streamID == 0 {
		errMsg := "RST_STREAM frame received on stream 0"
		c.log.Error(errMsg, logger.LogFields{})
		return NewConnectionError(ErrCodeProtocolError, errMsg)
	}

	stream, found := c.getStream(streamID) // getStream uses RLock

	if !found {
		// Stream does not exist or was already closed and removed.
		// RFC 7540, Section 6.4: "An endpoint that receives a RST_STREAM frame on a closed stream MUST ignore it."
		c.log.Warn("RST_STREAM received for unknown or already closed stream, ignoring.",
			logger.LogFields{
				"stream_id":  streamID,
				"error_code": errorCode.String(),
			})
		return nil
	}

	// Stream found. Delegate to the stream to handle its state transition to Closed
	// and to clean up its local resources (pipes, context, fcManager).
	stream.handleRSTStreamFrame(errorCode)

	// After the stream has processed the RST_STREAM internally (is marked as closed and resources cleaned),
	// remove it from the connection's active streams map and priority tree.
	c.removeClosedStream(stream)

	return nil
}

// handleWindowUpdateFrameConnLevel processes a WINDOW_UPDATE frame for stream ID 0.
// It increases the connection-level send flow control window.
func (c *Connection) handleWindowUpdateFrameConnLevel(frame *WindowUpdateFrame) error {
	header := frame.Header()
	if header.StreamID != 0 {
		// This function should only be called for connection-level WINDOW_UPDATEs.
		// If called with a non-zero stream ID, it's an internal logic error.
		c.log.Error("handleWindowUpdateFrameConnLevel called with non-zero stream ID",
			logger.LogFields{"stream_id": header.StreamID, "expected_stream_id": 0})
		return NewConnectionError(ErrCodeInternalError,
			fmt.Sprintf("internal: handleWindowUpdateFrameConnLevel called for stream %d", header.StreamID))
	}

	increment := frame.WindowSizeIncrement
	if increment == 0 {
		// RFC 7540, Section 6.9: "A WINDOW_UPDATE frame with a flow-control window increment of 0 MUST be treated as a connection error
		// (Section 5.4.1) of type PROTOCOL_ERROR; errors on stream 0 MUST be treated as a connection error."
		errMsg := "received connection-level WINDOW_UPDATE with zero increment"
		c.log.Error(errMsg, logger.LogFields{})
		return NewConnectionError(ErrCodeProtocolError, errMsg)
	}

	err := c.connFCManager.HandleWindowUpdateFromPeer(increment)
	if err != nil {
		// HandleWindowUpdateFromPeer can return an error if the increment causes the window to overflow.
		// This is a FLOW_CONTROL_ERROR.
		c.log.Error("Error handling connection-level WINDOW_UPDATE from peer",
			logger.LogFields{"increment": increment, "error": err.Error()})
		// err from connFCManager.HandleWindowUpdateFromPeer should already be a ConnectionError type with FLOW_CONTROL_ERROR.
		// If not, wrap it.
		if _, ok := err.(*ConnectionError); !ok {
			return NewConnectionErrorWithCause(ErrCodeFlowControlError,
				fmt.Sprintf("failed to apply connection window update increment %d: %v", increment, err), err)
		}
		return err // Propagate the ConnectionError
	}

	c.log.Debug("Connection-level WINDOW_UPDATE processed",
		logger.LogFields{"increment": increment, "new_conn_send_window": c.connFCManager.GetConnectionSendAvailable()})
	return nil
}

// dispatchWindowUpdateFrame handles an incoming WINDOW_UPDATE frame.
// It routes the frame to either connection-level or stream-level processing.
func (c *Connection) dispatchWindowUpdateFrame(frame *WindowUpdateFrame) error {
	streamID := frame.Header().StreamID

	if streamID == 0 {
		return c.handleWindowUpdateFrameConnLevel(frame)
	}

	// Stream-level WINDOW_UPDATE
	stream, found := c.getStream(streamID)
	if !found {
		// RFC 7540, Section 6.9: "WINDOW_UPDATE can be specific to a stream or to the entire connection.
		// In the former case, the frame's stream identifier indicates the affected stream; in the latter,
		// the value "0" indicates that the entire connection is the subject of the frame."
		// "An endpoint MUST treat a WINDOW_UPDATE frame on a closed stream as a connection error (Section 5.4.1) of type PROTOCOL_ERROR." (This seems to be for stream 0)
		// For non-zero streams: "Receiving a WINDOW_UPDATE on a stream in the "half-closed (remote)" or "closed" state MUST be treated as a stream error (Section 5.4.2) of type STREAM_CLOSED."
		// And "A receiver that receives a WINDOW_UPDATE frame on a stream that is not open or half-closed (local) MUST treat this as a stream error (Section 5.4.2) of type STREAM_CLOSED."

		c.streamsMu.RLock()
		lastKnownStreamID := c.lastProcessedStreamID
		c.streamsMu.RUnlock()

		if streamID <= lastKnownStreamID && streamID != 0 {
			// Stream was known but is now closed. This implies a stream error.
			c.log.Warn("WINDOW_UPDATE for closed or non-existent stream (was known)", logger.LogFields{"stream_id": streamID})
			rstErr := c.sendRSTStreamFrame(streamID, ErrCodeStreamClosed)
			if rstErr != nil {
				return NewConnectionErrorWithCause(ErrCodeInternalError, fmt.Sprintf("failed to send RST_STREAM for WINDOW_UPDATE on closed stream %d", streamID), rstErr)
			}
			return nil // RST sent, error handled.
		}
		// Stream ID is higher than any we've processed or otherwise invalid (e.g. client uses even ID for stream specific WU).
		// This is a connection error.
		return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("WINDOW_UPDATE on unknown or invalid stream ID %d", streamID))
	}

	// Stream found, delegate to stream's flow control manager.
	if err := stream.fcManager.HandleWindowUpdateFromPeer(frame.WindowSizeIncrement); err != nil {
		// fcManager.HandleWindowUpdateFromPeer returns an error for 0 increment or overflow.
		// This should be treated as a stream error.
		c.log.Error("Error handling stream-level WINDOW_UPDATE from peer",
			logger.LogFields{"stream_id": streamID, "increment": frame.WindowSizeIncrement, "error": err.Error()})

		var streamErrCode ErrorCode = ErrCodeFlowControlError
		if frame.WindowSizeIncrement == 0 { // Zero increment specifically leads to PROTOCOL_ERROR for the stream
			streamErrCode = ErrCodeProtocolError
		}

		rstErr := c.sendRSTStreamFrame(streamID, streamErrCode)
		if rstErr != nil {
			return NewConnectionErrorWithCause(ErrCodeInternalError, fmt.Sprintf("failed to send RST_STREAM for WINDOW_UPDATE error on stream %d", streamID), rstErr)
		}
		return nil // RST sent, stream error handled.
	}

	c.log.Debug("Stream-level WINDOW_UPDATE processed",
		logger.LogFields{"stream_id": streamID, "increment": frame.WindowSizeIncrement, "new_stream_send_window": stream.fcManager.GetStreamSendAvailable()})
	return nil
}

// readFrame reads a single HTTP/2 frame from the connection.
// It uses the package-level ReadFrame function.
func (c *Connection) readFrame() (Frame, error) {
	frame, err := ReadFrame(c.netConn) // ReadFrame is in the same http2 package (frame.go)
	if err != nil {
		// TODO: Differentiate between io.EOF (clean close by peer), timeout, and other errors.
		// For now, log and return.
		// c.log.Error("Error reading frame from connection", logger.LogFields{"error": err.Error(), "remote_addr": c.remoteAddrStr})
		return nil, err
	}
	c.log.Debug("Frame read from connection", logger.LogFields{"type": frame.Header().Type.String(), "stream_id": frame.Header().StreamID, "length": frame.Header().Length, "flags": frame.Header().Flags})
	return frame, nil
}

// writeFrame writes a single HTTP/2 frame to the connection.
// It uses the package-level WriteFrame function.
// This method is intended to be called by the connection's dedicated writer goroutine
// to ensure serialized access to the net.Conn.
func (c *Connection) writeFrame(frame Frame) error {
	c.log.Debug("Writing frame to connection", logger.LogFields{"type": frame.Header().Type.String(), "stream_id": frame.Header().StreamID, "length": frame.Header().Length, "flags": frame.Header().Flags})
	err := WriteFrame(c.netConn, frame) // WriteFrame is in the same http2 package (frame.go)
	if err != nil {
		// TODO: Handle specific write errors, e.g., connection closed by peer.
		// For now, log and return. The writer goroutine will likely detect this and shut down.
		// c.log.Error("Error writing frame to connection", logger.LogFields{"error": err.Error(), "remote_addr": c.remoteAddrStr})
		return err
	}
	return nil
}

// handlePingFrame processes an incoming PING frame.
// If the PING is not an ACK, it sends back a PING ACK.
// If it is an ACK, it processes it against outstanding PINGs.
func (c *Connection) handlePingFrame(frame *PingFrame) error {
	header := frame.Header()

	if header.StreamID != 0 {
		errMsg := fmt.Sprintf("PING frame received with non-zero stream ID %d", header.StreamID)
		c.log.Error(errMsg, logger.LogFields{})
		return NewConnectionError(ErrCodeProtocolError, errMsg)
	}
	if header.Length != 8 {
		errMsg := fmt.Sprintf("PING frame received with invalid length %d, expected 8", header.Length)
		c.log.Error(errMsg, logger.LogFields{})
		return NewConnectionError(ErrCodeFrameSizeError, errMsg)
	}

	if header.Flags&FlagPingAck != 0 {
		// This is an ACK for a PING we sent.
		c.activePingsMu.Lock()
		defer c.activePingsMu.Unlock()

		timer, ok := c.activePings[frame.OpaqueData]
		if ok {
			timer.Stop() // Stop the timeout timer
			delete(c.activePings, frame.OpaqueData)
			c.log.Debug("Received PING ACK", logger.LogFields{"opaque_data": fmt.Sprintf("%x", frame.OpaqueData)})
			// TODO: Optionally, calculate RTT if PINGs are used for that.
		} else {
			// Unsolicited PING ACK, or ACK for an already timed-out PING.
			c.log.Warn("Received unsolicited or late PING ACK", logger.LogFields{"opaque_data": fmt.Sprintf("%x", frame.OpaqueData)})
		}
	} else {
		// This is a PING request from the peer, send an ACK.
		c.log.Debug("Received PING request, sending ACK", logger.LogFields{"opaque_data": fmt.Sprintf("%x", frame.OpaqueData)})
		ackPingFrame := &PingFrame{
			FrameHeader: FrameHeader{
				Type:     FramePing,
				Flags:    FlagPingAck,
				StreamID: 0, // PING frames are always on stream 0
				Length:   8, // PING payload is always 8 octets
			},
			OpaqueData: frame.OpaqueData,
		}
		// Send the ACK PING frame via the writer channel.
		select {
		case c.writerChan <- ackPingFrame:
			// Successfully queued PING ACK
		case <-c.shutdownChan:
			c.log.Warn("Connection shutting down, cannot send PING ACK", logger.LogFields{"opaque_data": fmt.Sprintf("%x", frame.OpaqueData)})
			return NewConnectionError(ErrCodeConnectError, "connection shutting down, cannot send PING ACK")
		default:
			// This case indicates writerChan is full, which suggests a problem with the writer goroutine
			// or severe congestion. This is a critical state.
			c.log.Error("Failed to queue PING ACK: writer channel full or blocked", logger.LogFields{"opaque_data": fmt.Sprintf("%x", frame.OpaqueData)})
			return NewConnectionError(ErrCodeInternalError, "failed to send PING ACK: writer channel congested")
		}
	}
	return nil
}

// sendGoAway constructs and sends a GOAWAY frame.
// This method ensures that a GOAWAY frame is sent at most once,
// and updates the connection state accordingly.
func (c *Connection) sendGoAway(lastStreamID uint32, errorCode ErrorCode, debugData []byte) error {
	c.streamsMu.Lock() // Protects goAwaySent
	if c.goAwaySent {
		c.streamsMu.Unlock()
		c.log.Debug("GOAWAY frame already sent, not sending another.",
			logger.LogFields{"last_stream_id": lastStreamID, "error_code": errorCode.String()})
		return nil
	}
	c.goAwaySent = true
	c.streamsMu.Unlock()

	c.log.Info("Sending GOAWAY frame",
		logger.LogFields{
			"last_stream_id": lastStreamID,
			"error_code":     errorCode.String(),
			"debug_data_len": len(debugData),
		})

	goAwayFrame := &GoAwayFrame{
		FrameHeader: FrameHeader{
			Type:     FrameGoAway,
			Flags:    0,
			StreamID: 0,
			// Length will be set by WriteFrame based on payload
		},
		LastStreamID:        lastStreamID,
		ErrorCode:           errorCode,
		AdditionalDebugData: debugData,
	}

	select {
	case c.writerChan <- goAwayFrame:
		c.log.Debug("GOAWAY frame queued for sending.", logger.LogFields{})
		return nil
	case <-c.shutdownChan:
		c.log.Warn("Connection shutting down, cannot send GOAWAY frame.",
			logger.LogFields{"last_stream_id": lastStreamID, "error_code": errorCode.String()})
		return NewConnectionError(ErrCodeConnectError, "connection shutting down, cannot send GOAWAY")
	default:
		c.log.Error("Failed to queue GOAWAY frame: writer channel full or blocked.",
			logger.LogFields{"last_stream_id": lastStreamID, "error_code": errorCode.String()})
		return NewConnectionError(ErrCodeInternalError, "failed to send GOAWAY: writer channel congested")
	}
}

// Close initiates the shutdown of the connection.
// It determines the appropriate GOAWAY parameters based on the provided error
// and then calls initiateShutdown. It waits for the connection's goroutines to complete.
// This method is idempotent.
func (c *Connection) Close(err error) error {
	// Determine GOAWAY parameters based on the error
	var lastStreamID uint32
	var errCode ErrorCode
	var debugData []byte
	var gracefulTimeout time.Duration // For now, default to immediate if error, or short graceful if no error

	c.streamsMu.RLock()
	lastStreamID = c.lastProcessedStreamID
	c.streamsMu.RUnlock()

	if err == nil { // Graceful shutdown initiated by server logic (e.g., server stopping)
		errCode = ErrCodeNoError
		// A configured server-wide graceful timeout could be used here.
		// For now, let's assume a short default or that it's passed via initiateShutdown.
		// Let's set gracefulTimeout to a small value for clean close, or 0 for error-driven close.
		gracefulTimeout = 5 * time.Second // Example: 5 seconds for graceful stream completion

	} else if ce, ok := err.(*ConnectionError); ok {
		errCode = ce.Code
		// LastStreamID for GOAWAY is determined by c.lastProcessedStreamID.
		// ConnectionError.Code and ConnectionError.DebugData are used if present.
		debugData = ce.DebugData
		gracefulTimeout = 0 // Connection errors usually imply immediate shutdown
	} else { // Other errors
		errCode = ErrCodeProtocolError // Or InternalError if it's from our side
		if strings.Contains(err.Error(), "connection reset by peer") || strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "forcibly closed") || err == context.Canceled || err == context.DeadlineExceeded || strings.Contains(err.Error(), "EOF") {
			errCode = ErrCodeConnectError // Peer likely disappeared or connection was aborted
		}
		gracefulTimeout = 0
	}

	// Store the primary error that caused the shutdown.
	c.streamsMu.Lock() // Protect connError
	if c.connError == nil {
		c.connError = err
	}
	isShuttingDown := c.isShuttingDownLocked()
	c.streamsMu.Unlock()

	if isShuttingDown {
		c.log.Debug("Close called, but connection already shutting down.", logger.LogFields{"error": err})
		// Wait for completion anyway
	} else {
		c.log.Info("Closing connection.", logger.LogFields{"error": err, "last_stream_id": lastStreamID, "error_code": errCode.String()})
		// initiateShutdown should be called only once.
		go c.initiateShutdown(lastStreamID, errCode, debugData, gracefulTimeout)
	}

	// Wait for reader and writer goroutines to finish.
	// This wait should have a timeout to prevent hanging indefinitely if goroutines misbehave.
	// For now, direct wait.
	// TODO: Add timeout for these waits.
	if c.readerDone != nil {
		<-c.readerDone
	}
	if c.writerDone != nil {
		<-c.writerDone
	}

	c.log.Info("Connection closed completely.", logger.LogFields{"remote_addr": c.remoteAddrStr})
	return c.connError // Return the original or stored error
}

// isShuttingDownLocked checks if the shutdown process has started.
// Assumes c.streamsMu is held by the caller or called in a context where it's safe.
// For checking outside a c.streamsMu lock, use a select on c.shutdownChan directly.
func (c *Connection) isShuttingDownLocked() bool {
	select {
	case <-c.shutdownChan:
		return true
	default:
		return false
	}
}

// initiateShutdown performs the actual connection shutdown sequence.
// It sends GOAWAY, closes streams, closes the net.Conn, and cleans up resources.
// This method should be called at most once, typically by Close().
func (c *Connection) initiateShutdown(lastStreamID uint32, errCode ErrorCode, debugData []byte, gracefulStreamTimeout time.Duration) {
	c.streamsMu.Lock()
	if c.isShuttingDownLocked() {
		c.streamsMu.Unlock()
		return // Already shutting down
	}
	// Mark that shutdown has started.
	// This MUST be done before releasing streamsMu for the first time in this function,
	// to prevent race conditions with multiple calls to initiateShutdown or Close.
	close(c.shutdownChan)
	c.streamsMu.Unlock()

	c.log.Debug("Initiating connection shutdown sequence.",
		logger.LogFields{
			"last_stream_id_for_goaway": lastStreamID,
			"error_code":                errCode.String(),
			"graceful_stream_timeout":   gracefulStreamTimeout.String(),
		})

	// 1. Send GOAWAY frame if not already sent.
	// sendGoAway method itself checks c.goAwaySent.
	if err := c.sendGoAway(lastStreamID, errCode, debugData); err != nil {
		c.log.Error("Failed to send GOAWAY frame during shutdown.", logger.LogFields{"error": err})
		// Continue shutdown regardless
	}

	// 2. Graceful stream handling
	if gracefulStreamTimeout > 0 && !c.isClient { // Graceful only for server-side handling of existing streams for now
		c.log.Debug("Starting graceful stream shutdown period.", logger.LogFields{"timeout": gracefulStreamTimeout})
		startTime := time.Now()
		deadline := startTime.Add(gracefulStreamTimeout)

		for time.Now().Before(deadline) {
			c.streamsMu.RLock()
			activeStreamsBelowGoAwayID := 0
			for streamIDIter, stream := range c.streams {
				// Only consider streams the peer might expect to complete.
				// If GOAWAY was from us due to error, lastStreamID is OUR last processed.
				// If GOAWAY was from peer, their lastStreamID is relevant.
				// For simplicity, our lastStreamID in the GOAWAY we send is the limit.
				if streamIDIter <= lastStreamID {
					stream.mu.RLock()
					isStreamEffectivelyClosed := stream.state == StreamStateClosed ||
						(stream.state == StreamStateHalfClosedLocal && stream.endStreamReceivedFromClient) ||
						(stream.state == StreamStateHalfClosedRemote && stream.endStreamSentToClient)
					stream.mu.RUnlock()
					if !isStreamEffectivelyClosed {
						activeStreamsBelowGoAwayID++
					}
				}
			}
			c.streamsMu.RUnlock()

			if activeStreamsBelowGoAwayID == 0 {
				c.log.Debug("All relevant streams closed gracefully.", logger.LogFields{})
				break
			}
			c.log.Debug("Waiting for streams to close gracefully.", logger.LogFields{"active_streams_below_goaway_id": activeStreamsBelowGoAwayID, "time_remaining": deadline.Sub(time.Now())})
			// TODO: Replace time.Sleep with a more robust mechanism, e.g., a condition variable signaled by stream closures.
			// For now, simple polling.
			time.Sleep(100 * time.Millisecond) // Check interval
		}
		if time.Now().After(deadline) {
			c.log.Warn("Graceful stream shutdown period timed out.", logger.LogFields{})
		}
	}

	// 3. Forcefully close all remaining active streams
	c.log.Debug("Forcefully closing any remaining active streams.", logger.LogFields{})
	var streamsToClose []*Stream
	c.streamsMu.RLock()
	for _, stream := range c.streams {
		streamsToClose = append(streamsToClose, stream)
	}
	c.streamsMu.RUnlock()

	for _, stream := range streamsToClose {
		streamErr := NewStreamError(stream.id, ErrCodeCancel, "connection shutting down")
		// Stream.Close is idempotent and handles its own state.
		// It might send RST_STREAM if appropriate.
		if err := stream.Close(streamErr); err != nil {
			c.log.Warn("Error closing stream during connection shutdown.", logger.LogFields{"stream_id": stream.id, "error": err.Error()})
		}
	}
	// After this, removeClosedStream (called by stream.Close via state transition) should have cleaned them from c.streams.

	// 4. Close the underlying network connection.
	// This will unblock the reader goroutine if it's stuck in netConn.Read().
	c.log.Debug("Closing network connection.", logger.LogFields{"remote_addr": c.remoteAddrStr})
	if err := c.netConn.Close(); err != nil {
		c.log.Warn("Error closing network connection.", logger.LogFields{"error": err.Error()})
		// Store this error if it's the first network-related one, but don't overwrite existing primary error.
		c.streamsMu.Lock()
		if c.connError == nil {
			c.connError = err
		}
		c.streamsMu.Unlock()
	}

	// 5. Signal writer goroutine to terminate and clean up.
	// Closing writerChan signals the writerLoop to drain remaining frames and then exit.
	if c.writerChan != nil {
		// Ensure writerChan is closed only once. A select-based approach for sending to a
		// possibly already closed channel could be used, or a sync.Once around close(c.writerChan).
		// For now, assuming a single call path to here for the actual close.
		// The writer goroutine itself should handle reading from a closed channel.
		// One common pattern: use a dedicated signal channel for the writer to stop,
		// then it closes writerChan after draining. Or, check if already closed.
		// A simple way to check if a channel is closed is to try to send a non-blocking message or use recover.
		// The safest is often for the producer (here, connection methods adding to writerChan) to stop producing
		// and the consumer (writerLoop) to detect shutdown (e.g. via c.shutdownChan) and then drain and exit.
		// Forcing close of writerChan from here is fine if writerLoop correctly handles reads from a closed channel.

		// TODO: Refine writerChan closure to be safer with concurrent access if initiateShutdown could be called from writerLoop.
		// For now, direct close assuming writerLoop handles it.
		close(c.writerChan)
		c.log.Debug("Writer channel closed.", logger.LogFields{})
	}

	// 6. Cancel the connection's context.
	// This signals any operations using c.ctx to stop.
	if c.cancelCtx != nil {
		c.cancelCtx()
		c.log.Debug("Connection context cancelled.", logger.LogFields{})
	}

	// 7. Clean up other connection-level resources.
	c.log.Debug("Cleaning up connection resources.", logger.LogFields{})
	if c.connFCManager != nil {
		// Pass the primary shutdown error to flow control, so waiters can unblock with reason.
		var fcCloseErr error
		c.streamsMu.RLock() // Safely read connError
		fcCloseErr = c.connError
		c.streamsMu.RUnlock()
		if fcCloseErr == nil {
			// If no primary error, use the error code from the GOAWAY frame.
			fcCloseErr = NewConnectionError(errCode, "connection closed")
		}
		c.connFCManager.Close(fcCloseErr)
	}

	// Stop settings ACK timer
	c.settingsMu.Lock()
	if c.settingsAckTimeoutTimer != nil {
		c.settingsAckTimeoutTimer.Stop()
		c.settingsAckTimeoutTimer = nil
	}
	c.settingsMu.Unlock()

	// Stop active PING timers
	c.activePingsMu.Lock()
	for data, timer := range c.activePings {
		timer.Stop()
		delete(c.activePings, data)
	}
	c.activePingsMu.Unlock()

	// HpackAdapter doesn't currently have explicit cleanup.
	// PriorityTree also doesn't. If they did, call here.

	c.log.Info("Connection shutdown sequence complete.", logger.LogFields{"remote_addr": c.remoteAddrStr})

	// Reader/writer goroutines will signal their exit by closing readerDone/writerDone.
	// The Close() method waits for these.
	// The Close() method waits for these.
}

// handleSettingsFrame processes an incoming SETTINGS frame.
// It validates the settings, applies them to the connection, and sends an ACK if required.
func (c *Connection) handleSettingsFrame(frame *SettingsFrame) error {
	header := frame.Header()

	if header.StreamID != 0 {
		return NewConnectionError(ErrCodeProtocolError, "SETTINGS frame received with non-zero stream ID")
	}

	if header.Flags&FlagSettingsAck != 0 {
		// This is an ACK from the peer for SETTINGS we sent.
		if header.Length != 0 {
			return NewConnectionError(ErrCodeFrameSizeError, "SETTINGS ACK frame received with non-zero length")
		}
		// Process ACK
		c.settingsMu.Lock()
		if c.settingsAckTimeoutTimer != nil {
			c.settingsAckTimeoutTimer.Stop()
			c.settingsAckTimeoutTimer = nil
			c.log.Debug("Received SETTINGS ACK from peer.", logger.LogFields{})
		} else {
			// This is not necessarily an error by spec if peer sends unsolicited ACK.
			// It could be an ACK for settings we sent but whose timer already fired, or some race.
			c.log.Warn("Received SETTINGS ACK, but no outstanding SETTINGS frame was tracked by timer.", logger.LogFields{})
		}
		c.settingsMu.Unlock()
		return nil
	}

	// This is a SETTINGS frame from the peer (not an ACK), detailing their settings.
	if header.Length%6 != 0 { // Each setting is 6 octets (ID + Value)
		return NewConnectionError(ErrCodeFrameSizeError, "SETTINGS frame received with length not a multiple of 6")
	}

	var oldPeerInitialWindowSize uint32
	var newPeerInitialWindowSize uint32
	var peerInitialWindowSizeChanged bool

	c.settingsMu.Lock()
	oldPeerInitialWindowSize = c.peerInitialWindowSize // Capture current effective peer initial window size

	for _, setting := range frame.Settings {
		// Validate setting ID and value
		switch setting.ID {
		case SettingHeaderTableSize:
			// Max value is effectively bounded by uint32.
			c.peerSettings[SettingHeaderTableSize] = setting.Value
			c.log.Debug("Peer SETTINGS_HEADER_TABLE_SIZE received", logger.LogFields{"value": setting.Value})
		case SettingEnablePush:
			if setting.Value != 0 && setting.Value != 1 {
				c.settingsMu.Unlock()
				return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("invalid SETTINGS_ENABLE_PUSH value: %d (must be 0 or 1)", setting.Value))
			}
			c.peerSettings[SettingEnablePush] = setting.Value
			c.log.Debug("Peer SETTINGS_ENABLE_PUSH received", logger.LogFields{"value": setting.Value})
		case SettingMaxConcurrentStreams:
			// Max value effectively bounded by uint32.
			c.peerSettings[SettingMaxConcurrentStreams] = setting.Value
			c.log.Debug("Peer SETTINGS_MAX_CONCURRENT_STREAMS received", logger.LogFields{"value": setting.Value})
		case SettingInitialWindowSize:
			if setting.Value > MaxWindowSize { // MaxWindowSize is 2^31 - 1 from flowcontrol.go
				c.settingsMu.Unlock()
				return NewConnectionError(ErrCodeFlowControlError, fmt.Sprintf("invalid SETTINGS_INITIAL_WINDOW_SIZE value %d exceeds maximum %d", setting.Value, MaxWindowSize))
			}
			c.peerSettings[SettingInitialWindowSize] = setting.Value
			c.log.Debug("Peer SETTINGS_INITIAL_WINDOW_SIZE received", logger.LogFields{"value": setting.Value})
		case SettingMaxFrameSize:
			// MinMaxFrameSize (2^14) and MaxAllowedFrameSizeValue (2^24-1)
			if setting.Value < MinMaxFrameSize || setting.Value > MaxAllowedFrameSizeValue {
				c.settingsMu.Unlock()
				return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("invalid SETTINGS_MAX_FRAME_SIZE value: %d (must be between %d and %d)", setting.Value, MinMaxFrameSize, MaxAllowedFrameSizeValue))
			}
			c.peerSettings[SettingMaxFrameSize] = setting.Value
			c.log.Debug("Peer SETTINGS_MAX_FRAME_SIZE received", logger.LogFields{"value": setting.Value})
		case SettingMaxHeaderListSize:
			// Max value effectively bounded by uint32.
			c.peerSettings[SettingMaxHeaderListSize] = setting.Value
			c.log.Debug("Peer SETTINGS_MAX_HEADER_LIST_SIZE received", logger.LogFields{"value": setting.Value})
		default:
			// RFC 7540, 6.5.2: "An endpoint that receives a SETTINGS frame with any unknown or unsupported identifier MUST ignore that setting."
			c.log.Debug("Peer sent unknown/unsupported SETTINGS identifier, ignoring.", logger.LogFields{"id": setting.ID, "value": setting.Value})
		}
	}

	// After loop, c.peerSettings map is updated. Now apply these changes to c.peerInitialWindowSize, etc.
	c.applyPeerSettings() // This reads from c.peerSettings and updates c.peerInitialWindowSize, c.peerMaxFrameSize etc.

	newPeerInitialWindowSize = c.peerInitialWindowSize // This is the new effective value
	if newPeerInitialWindowSize != oldPeerInitialWindowSize {
		peerInitialWindowSizeChanged = true
	}
	c.settingsMu.Unlock() // Unlock before stream iterations or sending ACK

	// If SETTINGS_INITIAL_WINDOW_SIZE changed, update all streams' send flow control windows.
	if peerInitialWindowSizeChanged {
		c.log.Debug("Peer's SETTINGS_INITIAL_WINDOW_SIZE changed, updating streams.", logger.LogFields{"old_value": oldPeerInitialWindowSize, "new_value": newPeerInitialWindowSize})
		var streamsToUpdate []*Stream
		c.streamsMu.RLock()
		for _, stream := range c.streams {
			streamsToUpdate = append(streamsToUpdate, stream)
		}
		c.streamsMu.RUnlock()

		for _, stream := range streamsToUpdate {
			// HandlePeerSettingsInitialWindowSizeChange applies the delta to the stream's *send* window.
			if err := stream.fcManager.HandlePeerSettingsInitialWindowSizeChange(newPeerInitialWindowSize); err != nil {
				// This is a connection error if a stream's send window overflows.
				c.log.Error("Error updating stream send window for new peer SETTINGS_INITIAL_WINDOW_SIZE",
					logger.LogFields{"stream_id": stream.id, "new_initial_size": newPeerInitialWindowSize, "error": err.Error()})
				// The error from HandlePeerSettingsInitialWindowSizeChange should be a ConnectionError.
				return err // This will tear down the connection.
			}
		}
	}

	// Send SETTINGS ACK
	ackFrame := &SettingsFrame{
		FrameHeader: FrameHeader{
			Type:     FrameSettings,
			Flags:    FlagSettingsAck,
			StreamID: 0,
			Length:   0, // ACK has no payload length
		},
		Settings: nil, // ACK has no settings payload
	}

	select {
	case c.writerChan <- ackFrame:
		c.log.Debug("SETTINGS ACK queued for sending.", logger.LogFields{})
	case <-c.shutdownChan:
		c.log.Warn("Connection shutting down, cannot send SETTINGS ACK.", logger.LogFields{})
		// Return an error that indicates the connection is closing rather than an internal server error.
		return NewConnectionError(ErrCodeConnectError, "connection shutting down, cannot send SETTINGS ACK")
	default:
		// This case indicates writerChan is full, which suggests a problem with the writer goroutine
		// or severe congestion. This is a critical state.
		c.log.Error("Failed to queue SETTINGS ACK: writer channel full or blocked.", logger.LogFields{})
		return NewConnectionError(ErrCodeInternalError, "failed to send SETTINGS ACK: writer channel congested")
	}

	return nil
}

// handleGoAwayFrame processes an incoming GOAWAY frame from the peer.
// It logs the frame, updates connection state regarding peer's last processed stream ID,
// and initiates a graceful shutdown of the connection.
func (c *Connection) handleGoAwayFrame(frame *GoAwayFrame) error {
	c.streamsMu.Lock() // Lock for goAwayReceived, peerReportedLastStreamID, and isShuttingDownLocked checks

	if c.isShuttingDownLocked() {
		c.log.Info("Received GOAWAY frame while connection already shutting down.",
			logger.LogFields{
				"last_stream_id_from_peer": frame.LastStreamID,
				"error_code_from_peer":     frame.ErrorCode.String(),
			})
		// Peer can send multiple GOAWAY frames, each with a possibly lower LastStreamID.
		// Update our record if the new LastStreamID is more restrictive.
		if frame.LastStreamID < c.peerReportedLastStreamID {
			c.peerReportedLastStreamID = frame.LastStreamID
			c.log.Info("Updated peerReportedLastStreamID from GOAWAY during shutdown.",
				logger.LogFields{"new_peer_last_stream_id": c.peerReportedLastStreamID})
		}
		c.streamsMu.Unlock()
		return nil // Already shutting down, no new shutdown initiation needed.
	}

	if c.goAwayReceived {
		// Subsequent GOAWAY frame received.
		c.log.Warn("Subsequent GOAWAY frame received.",
			logger.LogFields{
				"new_last_stream_id":      frame.LastStreamID,
				"new_error_code":          frame.ErrorCode.String(),
				"old_peer_last_stream_id": c.peerReportedLastStreamID,
			})

		// RFC 7540, Section 6.8: "An endpoint that receives multiple GOAWAY frames MUST
		// treat a subsequent GOAWAY frame as a connection error ... of type PROTOCOL_ERROR
		// if the stream identifier in the subsequent frame is greater than the stream
		// identifier in the previous GOAWAY frame."
		if frame.LastStreamID > c.peerReportedLastStreamID {
			msg := fmt.Sprintf("subsequent GOAWAY has LastStreamID %d, which is greater than previous %d",
				frame.LastStreamID, c.peerReportedLastStreamID)
			c.log.Error(msg, logger.LogFields{})
			c.streamsMu.Unlock()
			// The reader loop should call c.Close(err) upon receiving this error.
			return NewConnectionError(ErrCodeProtocolError, msg)
		}

		// If LastStreamID is the same or lower, it's permissible. Update if lower.
		if frame.LastStreamID < c.peerReportedLastStreamID {
			c.peerReportedLastStreamID = frame.LastStreamID
		}
		c.streamsMu.Unlock()
		// No need to re-trigger initiateShutdown; the first GOAWAY processing should have done so,
		// or the connection is already in the process of shutting down due to other reasons.
		return nil
	}

	// This is the first GOAWAY frame received on this connection.
	c.goAwayReceived = true
	c.peerReportedLastStreamID = frame.LastStreamID // Store the peer's reported last stream ID

	c.log.Info("Received first GOAWAY frame from peer.",
		logger.LogFields{
			"last_stream_id_from_peer": c.peerReportedLastStreamID,
			"error_code_from_peer":     frame.ErrorCode.String(),
			"debug_data_len":           len(frame.AdditionalDebugData),
		})

	// Determine our last processed stream ID for our own GOAWAY frame (if we send one via initiateShutdown).
	ourGoAwayLastStreamID := c.lastProcessedStreamID
	c.streamsMu.Unlock() // Unlock before calling initiateShutdown

	// Determine graceful timeout based on peer's GOAWAY error code.
	var gracefulTimeout time.Duration
	if frame.ErrorCode == ErrCodeNoError {
		// TODO: Use a configured graceful shutdown timeout from server config.
		// This timeout is for our server to allow its active streams (up to our GOAWAY's LastStreamID)
		// to complete. Example value:
		gracefulTimeout = 5 * time.Second
	} else {
		// Peer indicated an error, so we might shut down more quickly.
		gracefulTimeout = 0
	}

	// Initiate our own shutdown sequence in response to receiving GOAWAY.
	// We send ErrCodeNoError in our GOAWAY as we are now gracefully closing in response to peer's signal.
	// Debug data from peer's GOAWAY is not typically propagated to our GOAWAY.
	go c.initiateShutdown(ourGoAwayLastStreamID, ErrCodeNoError, nil, gracefulTimeout)

	return nil
}

// ServerHandshake performs the server-side HTTP/2 connection handshake.
// It reads the client's connection preface, sends the server's initial SETTINGS,
// and then reads and processes the client's initial SETTINGS frame.
func (c *Connection) ServerHandshake() error {
	// 1. Read and validate client connection preface
	prefaceBuffer := make([]byte, len(ClientPreface))
	// TODO: Set a reasonable read deadline for the preface.
	// For now, rely on net.Conn's default or externally set deadlines.
	if _, err := io.ReadFull(c.netConn, prefaceBuffer); err != nil {
		c.log.Error("Failed to read client connection preface", logger.LogFields{"error": err, "remote_addr": c.remoteAddrStr})
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return NewConnectionError(ErrCodeProtocolError, "client preface incomplete or missing")
		}
		return NewConnectionErrorWithCause(ErrCodeProtocolError, "error reading client preface", err)
	}

	if string(prefaceBuffer) != ClientPreface {
		c.log.Error("Invalid client connection preface received", logger.LogFields{"received_preface": string(prefaceBuffer), "remote_addr": c.remoteAddrStr})
		return NewConnectionError(ErrCodeProtocolError, "invalid client connection preface")
	}
	c.log.Debug("Client connection preface validated.", logger.LogFields{"remote_addr": c.remoteAddrStr})

	// 2. Send server's initial SETTINGS frame.
	// c.sendInitialSettings() queues the frame and starts the ACK timer.
	if err := c.sendInitialSettings(); err != nil {
		c.log.Error("Failed to send initial server SETTINGS frame during handshake",
			logger.LogFields{"error": err, "remote_addr": c.remoteAddrStr})
		return err // sendInitialSettings already returns a ConnectionError or nil
	}
	c.log.Debug("Initial server SETTINGS frame queued for sending.", logger.LogFields{"remote_addr": c.remoteAddrStr})

	// 3. Read and process client's initial SETTINGS frame.
	// This MUST be the first frame from the client after their preface.
	// TODO: Set a reasonable read deadline for this first frame.
	frame, err := c.readFrame()
	if err != nil {
		c.log.Error("Failed to read client's initial SETTINGS frame", logger.LogFields{"error": err, "remote_addr": c.remoteAddrStr})
		return NewConnectionErrorWithCause(ErrCodeProtocolError, "error reading client's initial SETTINGS frame", err)
	}

	settingsFrame, ok := frame.(*SettingsFrame)
	if !ok {
		errMsg := fmt.Sprintf("expected client's initial frame to be SETTINGS, got %s", frame.Header().Type.String())
		c.log.Error(errMsg, logger.LogFields{"remote_addr": c.remoteAddrStr})
		return NewConnectionError(ErrCodeProtocolError, errMsg)
	}

	if settingsFrame.Header().Flags&FlagSettingsAck != 0 {
		errMsg := "client's initial SETTINGS frame must not have ACK flag set"
		c.log.Error(errMsg, logger.LogFields{"remote_addr": c.remoteAddrStr})
		return NewConnectionError(ErrCodeProtocolError, errMsg)
	}

	// Process the client's SETTINGS frame. This will apply their settings and queue an ACK from us.
	if err := c.handleSettingsFrame(settingsFrame); err != nil {
		c.log.Error("Error processing client's initial SETTINGS frame",
			logger.LogFields{"error": err, "remote_addr": c.remoteAddrStr})
		return err // handleSettingsFrame should return a ConnectionError if issues occur
	}

	c.log.Info("Server handshake completed successfully.", logger.LogFields{"remote_addr": c.remoteAddrStr})
	return nil
}

// serve is the main goroutine for reading frames from the connection.
// It runs after the handshake is complete (or immediately for server if no handshake needed beyond preface).

// serve is the main goroutine for reading frames from the connection.
// It runs after the handshake is complete (or immediately for server if no handshake needed beyond preface).
func (c *Connection) serve() {
	var err error
	// Ensure that Close is called eventually to clean up resources and signal other goroutines.
	// The actual error passed to Close will be the first fatal error encountered.
	defer func() {
		// If connError was set by dispatchFrame or another source, use that.
		// Otherwise, the 'err' from readFrame loop termination is the cause.
		finalErr := err
		c.streamsMu.RLock() // Safely read c.connError
		if c.connError != nil {
			finalErr = c.connError
		}
		c.streamsMu.RUnlock()

		c.log.Debug("Serve (reader) loop exiting.", logger.LogFields{"final_error_for_close": finalErr, "original_loop_term_error": err})

		// Ensure readerDone is closed to signal that this goroutine has finished.
		if c.readerDone != nil {
			select {
			case <-c.readerDone: // Already closed
			default:
				close(c.readerDone)
			}
		}

		// c.Close will be called with the 'finalErr' that caused the loop to terminate or was set elsewhere.
		c.Close(finalErr)
	}()

	c.log.Info("HTTP/2 connection (reader loop) serving started.", logger.LogFields{"remote_addr": c.remoteAddrStr, "is_client": c.isClient})

	// Start the writer goroutine.

	go c.writerLoop()

	// Handle connection preface and initial SETTINGS exchange.
	if !c.isClient { // Server-side
		handshakeErr := c.ServerHandshake()
		if handshakeErr != nil {
			c.log.Error("Server handshake failed, closing connection.",
				logger.LogFields{"error": handshakeErr, "remote_addr": c.remoteAddrStr})
			err = handshakeErr // This error will be used by the defer c.Close(err)

			// Store the error if it's the first one for this connection
			c.streamsMu.Lock()
			if c.connError == nil {
				c.connError = err
			}
			c.streamsMu.Unlock()
			return
		}
		c.log.Debug("Server-side handshake successful.", logger.LogFields{"remote_addr": c.remoteAddrStr})
	} else { // Client-side
		// TODO: Client-side preface logic:
		// 1. Send client magic (PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n).
		// 2. Send client's initial SETTINGS frame.
		// 3. Then proceed to read server's SETTINGS (first frame from server) and other frames.
		c.log.Debug("Client-side: TODO: Implement client preface sending and initial SETTINGS processing.", logger.LogFields{"remote_addr": c.remoteAddrStr})
	}

	// Main frame reading loop
	for {
		// Check for shutdown signal before attempting to read.
		select {
		case <-c.shutdownChan:
			c.log.Info("Serve (reader) loop: shutdown signal received, terminating.", logger.LogFields{})
			c.streamsMu.RLock()
			err = c.connError // Use error that triggered shutdown if available
			c.streamsMu.RUnlock()
			if err == nil {
				err = errors.New("connection shutdown initiated") // Generic if no specific error
			}
			return
		default:
			// Continue to read frame.
		}

		var frame Frame
		frame, err = c.readFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.log.Info("Serve (reader) loop: peer closed connection (EOF).", logger.LogFields{"remote_addr": c.remoteAddrStr})
			} else if se, ok := err.(net.Error); ok && se.Timeout() {
				c.log.Info("Serve (reader) loop: net.Conn read timeout.", logger.LogFields{"remote_addr": c.remoteAddrStr, "error": err})
				// Optional: Implement PING frames for keepalive or specific idle timeout logic.
				// For now, any read timeout is fatal for the loop.
			} else if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection") {
				c.log.Info("Serve (reader) loop: connection closed locally or context cancelled.", logger.LogFields{"error": err.Error(), "remote_addr": c.remoteAddrStr})
			} else {
				c.log.Error("Serve (reader) loop: error reading frame.", logger.LogFields{"error": err.Error(), "remote_addr": c.remoteAddrStr})
			}
			return // Exit loop, defer will handle Close with this 'err'.
		}

		dispatchErr := c.dispatchFrame(frame)
		if dispatchErr != nil {
			c.log.Error("Serve (reader) loop: error dispatching frame, terminating connection.",
				logger.LogFields{"error": dispatchErr.Error(), "frame_type": frame.Header().Type.String(), "remote_addr": c.remoteAddrStr})
			c.streamsMu.Lock()
			if c.connError == nil {
				c.connError = dispatchErr
			}
			c.streamsMu.Unlock()
			err = dispatchErr
			return
		}
	}
}

// writerLoop is the main goroutine for writing frames to the connection.
// It serializes access to the underlying net.Conn for writes.
func (c *Connection) writerLoop() {
	defer func() {
		if c.writerDone != nil {
			// Ensure writerDone is closed only once, even if panicking.
			// A sync.Once could be used, or a check like this.
			select {
			case <-c.writerDone: // Already closed
			default:
				close(c.writerDone)
			}
		}
		c.log.Debug("Writer loop exiting.", logger.LogFields{"remote_addr": c.remoteAddrStr})

		// If the writer loop exits (e.g., due to write error or shutdownChan closure),
		// it means no more frames can be sent. The connection should be fully closed.
		// c.Close() is idempotent and will handle the full shutdown sequence if not already started.
		// We need to retrieve the error that might have caused the writer to exit.
		// For now, if writer exits and shutdown is not initiated, it's an issue.
		select {
		case <-c.shutdownChan:
			// Normal shutdown path, c.Close() is managing.
		default:
			// Writer loop exited prematurely.
			c.log.Warn("Writer loop exited prematurely. Forcing connection closure.", logger.LogFields{"remote_addr": c.remoteAddrStr})
			// Use a generic error or retrieve c.connError if set by a write failure.
			// c.Close itself might call initiateShutdown which closes writerChan, leading to exit.
			// This path is more for unexpected exits.
			go c.Close(errors.New("writer loop terminated unexpectedly"))
		}
	}()

	c.log.Debug("Writer loop started.", logger.LogFields{"remote_addr": c.remoteAddrStr})

	for {
		select {
		case <-c.shutdownChan: // Primary shutdown signal
			c.log.Info("Writer loop: shutdown signal received. Draining writerChan.", logger.LogFields{"remote_addr": c.remoteAddrStr})
			// Drain any remaining frames in writerChan.
			// This loop continues until writerChan is closed (by initiateShutdown) and empty.
			for frame := range c.writerChan { // Loop until writerChan is closed
				if err := c.writeFrame(frame); err != nil {
					c.log.Error("Writer loop: error writing frame during shutdown drain.",
						logger.LogFields{"error": err, "frame_type": frame.Header().Type.String(), "remote_addr": c.remoteAddrStr})
					// Continue draining if possible, but log errors.
				}
			}
			c.log.Info("Writer loop: writerChan drained and closed. Exiting.", logger.LogFields{"remote_addr": c.remoteAddrStr})
			return

		case frame, ok := <-c.writerChan:
			if !ok { // writerChan was closed by initiateShutdown.
				c.log.Info("Writer loop: writerChan closed. Exiting.", logger.LogFields{"remote_addr": c.remoteAddrStr})
				return
			}

			if err := c.writeFrame(frame); err != nil {
				c.log.Error("Writer loop: error writing frame.",
					logger.LogFields{"error": err, "frame_type": frame.Header().Type.String(), "remote_addr": c.remoteAddrStr})

				// A write error is fatal for the connection.
				// Store the error and initiate connection closure.
				c.streamsMu.Lock()
				if c.connError == nil {
					c.connError = err
				}
				c.streamsMu.Unlock()

				// Use a goroutine for c.Close to avoid potential deadlock if Close()
				// tries to interact with the writerLoop (e.g., by closing writerChan and waiting for writerDone).
				// Since writerLoop is the one calling Close here, direct call should be fine if Close is robust
				// to being called from its own worker goroutines. However, goroutine is safer.
				go c.Close(err)
				return // Exit writer loop as the connection is now considered failed.
			}
		}
	}
}

// sendInitialSettings constructs and queues the server's initial SETTINGS frame.
// It also starts a timer to await the client's SETTINGS ACK.
// This should only be called for server-side connections.
func (c *Connection) sendInitialSettings() error {
	if c.isClient {
		return nil // Clients send a different preface.
	}

	c.settingsMu.Lock() // Protects c.ourSettings and c.settingsAckTimeoutTimer
	defer c.settingsMu.Unlock()

	if c.settingsAckTimeoutTimer != nil {
		// This implies sendInitialSettings might have been called before, which is a logic error.
		c.log.Error("sendInitialSettings called, but settingsAckTimeoutTimer already exists.", logger.LogFields{})
		return errors.New("initial SETTINGS frame likely already sent or being sent")
	}

	var settingsPayload []Setting
	for id, val := range c.ourSettings {
		settingsPayload = append(settingsPayload, Setting{ID: id, Value: val})
	}
	// Sort settings by ID for deterministic frame content (optional, but good for testing/debugging)
	// sort.Slice(settingsPayload, func(i, j int) bool { return settingsPayload[i].ID < settingsPayload[j].ID })

	initialSettingsFrame := &SettingsFrame{
		FrameHeader: FrameHeader{
			Type:     FrameSettings,
			Flags:    0, // Initial SETTINGS frame must not have ACK flag.
			StreamID: 0, // SETTINGS frames are always on stream 0.
			Length:   uint32(len(settingsPayload) * 6),
		},
		Settings: settingsPayload,
	}

	c.log.Debug("Queuing initial server SETTINGS frame.", logger.LogFields{"num_settings": len(settingsPayload)})

	// Queue the frame for sending.
	select {
	case c.writerChan <- initialSettingsFrame:
		// Successfully queued. Now start the ACK timeout timer.
		c.settingsAckTimeoutTimer = time.AfterFunc(SettingsAckTimeoutDuration, func() {
			errMsg := "timeout waiting for client's SETTINGS ACK"
			c.log.Error(errMsg, logger.LogFields{"remote_addr": c.remoteAddrStr, "timeout_duration": SettingsAckTimeoutDuration.String()})
			// Create a ConnectionError and close the connection.
			// The Close method will handle sending GOAWAY.
			connErr := NewConnectionError(ErrCodeSettingsTimeout, errMsg)
			c.Close(connErr)
		})
		return nil
	case <-c.shutdownChan:
		errMsg := "connection shutting down, cannot send initial SETTINGS frame"
		c.log.Warn(errMsg, logger.LogFields{"remote_addr": c.remoteAddrStr})
		return NewConnectionError(ErrCodeConnectError, errMsg) // Or a more specific shutdown error
	default:
		// writerChan is full. This is a critical problem, indicating the writerLoop is blocked or dead.
		errMsg := "failed to queue initial SETTINGS frame: writer channel full or blocked"
		c.log.Error(errMsg, logger.LogFields{"remote_addr": c.remoteAddrStr})
		// This situation likely means the connection is already unusable.
		return NewConnectionError(ErrCodeInternalError, errMsg)
	}
}

package http2

import (
	"context"
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

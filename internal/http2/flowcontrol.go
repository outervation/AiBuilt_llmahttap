package http2

import (
	"fmt"
	"sync"
	// ErrorCode constants like ErrorCodeFlowControlError are defined in errors.go
	// and are accessible as they are in the same package.
	// frame.DefaultInitialWindowSize is in frame.go, accessible.
)

// MaxWindowSize is the maximum value a flow control window can reach (2^31 - 1).
const MaxWindowSize = (1 << 31) - 1 // As per RFC 7540, 6.9.1

// FlowControlWindow manages the send flow control window for a stream or a connection.
// It tracks how much data this endpoint is allowed to send.
type FlowControlWindow struct {
	mu   sync.Mutex
	cond *sync.Cond // Signalled when window space becomes available or an error occurs

	available int64 // Current available window size for sending

	// initialWindowSize stores the initial window size this FCW was configured with.
	// For streams, this is SETTINGS_INITIAL_WINDOW_SIZE.
	// For connections, this is DefaultInitialWindowSize.
	// It's used by UpdateInitialWindowSize (for streams) to calculate deltas.
	initialWindowSize uint32

	closed bool  // If true, no more operations are allowed, Acquire returns error.
	err    error // Stores a terminal error (e.g., window overflow, or external closure reason)

	// Identifier fields for logging/errors
	isConnection bool
	streamID     uint32 // 0 if connection
}

// NewFlowControlWindow creates a new flow control window manager.
// initialSize is the initial window size.
// - For new streams: this should be the peer's SETTINGS_INITIAL_WINDOW_SIZE.
// - For the connection: this should be DefaultInitialWindowSize (65,535).
// For connection FCW, streamID should be 0.
func NewFlowControlWindow(initialSize uint32, isConn bool, streamID uint32) *FlowControlWindow {
	// RFC 7540, 6.5.2: SETTINGS_INITIAL_WINDOW_SIZE ... maximum value is 2^31-1.
	// The initialSize provided should already respect this.
	if initialSize > MaxWindowSize {
		initialSize = MaxWindowSize // Safeguard, though validation should happen earlier.
	}
	fcw := &FlowControlWindow{
		available:         int64(initialSize),
		initialWindowSize: initialSize,
		isConnection:      isConn,
		streamID:          streamID,
	}
	fcw.cond = sync.NewCond(&fcw.mu)
	return fcw
}

// Available returns the current available send window size.
// It can be negative if a protocol error has occurred and the window was driven negative.
func (fcw *FlowControlWindow) Available() int64 {
	fcw.mu.Lock()
	defer fcw.mu.Unlock()
	return fcw.available
}

// Acquire attempts to acquire 'n' bytes from the send window for sending data.
// It blocks until 'n' bytes are available or an error occurs (e.g., window closed, context cancelled).
// If successful, it reduces the available window by 'n'.
// Returns an error if 'n' is 0, or if the window is closed/has an error.
// Callers must ensure 'n' does not exceed SETTINGS_MAX_FRAME_SIZE for a single frame.
// TODO: Add context.Context for cancellation.
func (fcw *FlowControlWindow) Acquire(n uint32) error {
	if n == 0 {
		// Sending 0 bytes in a DATA frame payload does not consume window.
		// However, acquiring 0 bytes is a usage error for this method.
		return fmt.Errorf("cannot acquire zero bytes from flow control window")
	}
	// n > MaxWindowSize check is not strictly needed here as available can't exceed MaxWindowSize.
	// If n is very large, the loop condition `fcw.available >= int64(n)` will handle it.

	fcw.mu.Lock()
	defer fcw.mu.Unlock()

	for {
		if fcw.err != nil { // A terminal error was recorded (e.g., overflow)
			return fcw.err
		}
		if fcw.closed { // Window explicitly closed (e.g., stream reset, connection GOAWAY)
			return fmt.Errorf("flow control window (conn: %v, stream: %d) is closed", fcw.isConnection, fcw.streamID)
		}

		if fcw.available < 0 {
			// This state implies a flow control violation has already occurred.
			// fcw.err should ideally be set by the operation that caused this.
			// This is a fallback error if fcw.err wasn't set.
			var e error
			msg := fmt.Sprintf("flow control: window is negative (%d) (conn: %v, stream: %d)", fcw.available, fcw.isConnection, fcw.streamID)
			if fcw.isConnection {
				e = NewConnectionError(ErrCodeFlowControlError, msg)
			} else {
				e = NewStreamError(fcw.streamID, ErrCodeFlowControlError, msg)
			}
			fcw.setErrorLocked(e) // Ensure fcw.err is set
			return fcw.err
		}

		if fcw.available >= int64(n) {
			fcw.available -= int64(n)
			return nil
		}

		// Not enough space, wait for WINDOW_UPDATE or settings change.
		fcw.cond.Wait()
	}
}

// Increase increases the send window size by 'increment' upon receiving a WINDOW_UPDATE from the peer.
// Returns an error if the increment is 0 (for streams) or causes the window to exceed MaxWindowSize.
// If an error occurs, the window is not changed, and fcw.err is set.
func (fcw *FlowControlWindow) Increase(increment uint32) error {
	fcw.mu.Lock()
	defer fcw.mu.Unlock()

	if fcw.err != nil || fcw.closed { // Don't operate on a closed/errored window
		if fcw.err != nil {
			return fcw.err
		}
		return fmt.Errorf("flow control window (conn: %v, stream: %d) is closed", fcw.isConnection, fcw.streamID)
	}

	if increment == 0 {
		if !fcw.isConnection {
			// Stream-specific WINDOW_UPDATE with 0 increment is a PROTOCOL_ERROR. (RFC 6.9)
			// This error should cause RST_STREAM by the caller.
			return NewStreamError(fcw.streamID, ErrCodeProtocolError, "WINDOW_UPDATE increment cannot be 0 for a stream")
		}
		// For connection-level, increment 0 is a no-op. (RFC 6.9)
		return nil
	}

	newSize := fcw.available + int64(increment)
	if newSize > MaxWindowSize {
		// RFC 6.9.1: "A sender MUST NOT allow a flow-control window to exceed 2^31-1 octets.
		// If a sender receives a WINDOW_UPDATE that causes the flow-control window to exceed this maximum,
		// it MUST terminate either the stream or the connection, as appropriate."
		var err error
		msg := fmt.Sprintf("flow control window (conn: %v, stream: %d) would overflow: current %d + increment %d = %d > max %d",
			fcw.isConnection, fcw.streamID, fcw.available, increment, newSize, MaxWindowSize)
		if fcw.isConnection {
			err = NewConnectionError(ErrCodeFlowControlError, msg)
		} else {
			err = NewStreamError(fcw.streamID, ErrCodeFlowControlError, msg)
		}
		fcw.setErrorLocked(err) // Mark this FCW as terminally errored.
		return err
	}

	fcw.available = newSize
	fcw.cond.Broadcast() // Wake up any goroutines waiting in Acquire
	return nil
}

// UpdateInitialWindowSize adjusts the stream's flow control window when SETTINGS_INITIAL_WINDOW_SIZE changes.
// This method applies only to stream-level flow control windows. (RFC 6.9.2)
// Returns an error if the update causes the window to exceed MaxWindowSize (leading to connection error).
func (fcw *FlowControlWindow) UpdateInitialWindowSize(newInitialSize uint32) error {
	if fcw.isConnection {
		// Connection flow-control window is not affected by SETTINGS_INITIAL_WINDOW_SIZE.
		return nil
	}

	fcw.mu.Lock()
	defer fcw.mu.Unlock()

	if fcw.err != nil || fcw.closed {
		if fcw.err != nil {
			return fcw.err
		}
		return fmt.Errorf("flow control window (conn: %v, stream: %d) is closed", fcw.isConnection, fcw.streamID)
	}

	// newInitialSize itself should be validated against MaxWindowSize by SETTINGS processing.
	// Assuming newInitialSize <= MaxWindowSize.
	if newInitialSize > MaxWindowSize { // Values from SETTINGS_INITIAL_WINDOW_SIZE
		// RFC 6.5.2: "Values above the maximum flow-control window size of 2^31-1 MUST be treated as a connection error
		// (Section 5.4.1) of type FLOW_CONTROL_ERROR."
		// This check applies when newInitialSize comes from a peer's SETTINGS_INITIAL_WINDOW_SIZE.
		// fcw.streamID is relevant here as this method is for stream windows.
		msg := fmt.Sprintf("peer's SETTINGS_INITIAL_WINDOW_SIZE value %d (for stream %d context) exceeds MaxWindowSize %d", newInitialSize, fcw.streamID, MaxWindowSize)
		err := NewConnectionError(ErrCodeFlowControlError, msg) // Aligned with RFC 6.5.2 for invalid setting value from peer
		// The FCW itself isn't necessarily "broken" by this, but the connection is due to peer's invalid setting.
		return err
	}

	delta := int64(newInitialSize) - int64(fcw.initialWindowSize)
	newAvailable := fcw.available + delta

	// RFC 6.9.2: "An endpoint that receives a SETTINGS frame that causes a flow-control window to exceed the maximum
	// size MUST treat this as a connection error (Section 5.4.1) of type FLOW_CONTROL_ERROR."
	if newAvailable > MaxWindowSize {
		msg := fmt.Sprintf("applying SETTINGS_INITIAL_WINDOW_SIZE delta %d (new_init %d, old_init %d) to stream %d window (current %d) would result in %d, exceeding max %d",
			delta, newInitialSize, fcw.initialWindowSize, fcw.streamID, fcw.available, newAvailable, MaxWindowSize)
		// This is a connection error. The FCW state itself does not change here; the connection must terminate.
		// We can mark this FCW as errored to prevent further use, as part of the connection termination process.
		connErr := NewConnectionError(ErrCodeFlowControlError, msg)
		// fcw.setErrorLocked(connErr) // Let the connection manager decide to close stream FCWs.
		return connErr
	}

	fcw.available = newAvailable
	fcw.initialWindowSize = newInitialSize

	if delta > 0 {
		fcw.cond.Broadcast() // Wake up goroutines if window increased
	}
	return nil
}

// setErrorLocked sets a terminal error on the window and wakes up waiters.
// The caller must hold fcw.mu.
func (fcw *FlowControlWindow) setErrorLocked(err error) {
	if fcw.err == nil { // Set only the first error
		fcw.err = err
		// fcw.closed is not automatically set true here; an error might be recoverable
		// or might require specific closure actions. Close() or external logic handles fcw.closed.
		// However, for most FC errors (overflows), the FCW is unusable.
		fcw.closed = true    // For safety, if an error is set, consider it closed for new ops.
		fcw.cond.Broadcast() // Wake up waiters so they can see the error
	}
}

// Close marks the flow control window as closed, optionally with an error.
// If err is nil, a generic "closed" state is indicated for Acquire.
// Subsequent calls to Acquire will return an error.
// Outstanding waiters in Acquire will be woken up.
func (fcw *FlowControlWindow) Close(err error) {
	fcw.mu.Lock()
	defer fcw.mu.Unlock()

	if !fcw.closed {
		fcw.closed = true
		if fcw.err == nil { // Only set the provided error if no prior error exists
			fcw.err = err // Can be nil for graceful closure
		}
		fcw.cond.Broadcast() // Wake up any waiters
	}
}

// ConnectionFlowControlManager manages the send and receive flow control windows
// for the entire HTTP/2 connection.
type ConnectionFlowControlManager struct {
	// sendWindow governs how much data this endpoint can send on the connection.
	// This window is replenished by WINDOW_UPDATE frames from the peer.
	sendWindow *FlowControlWindow

	// Receive-side flow control:
	receiveWindowMu sync.Mutex
	// currentReceiveWindowSize tracks how much more data the peer is currently allowed to send us.
	// It starts at DefaultInitialWindowSize, decreases when we receive DATA frames,
	// and effectively increases when our application consumes data (allowing us to send WINDOW_UPDATE).
	currentReceiveWindowSize int64
	// bytesConsumedTotal tracks the cumulative number of bytes consumed by the application
	// from data received on this connection. Used to calculate WINDOW_UPDATE increments.
	bytesConsumedTotal uint64
	// lastWindowUpdateSentAt stores the value of bytesConsumedTotal at the time the last
	// connection-level WINDOW_UPDATE was sent.
	lastWindowUpdateSentAt uint64
	// windowUpdateThreshold is the amount of consumed data that should accumulate before
	// a new WINDOW_UPDATE frame is sent. E.g., DefaultInitialWindowSize / 2.
	windowUpdateThreshold uint32

	// logger *logger.Logger // Optional: for detailed logging
}

// NewConnectionFlowControlManager creates a new manager for connection-level flow control.
// HTTP/2 connections start with a flow control window of 65,535 octets for both sending and receiving.
func NewConnectionFlowControlManager() *ConnectionFlowControlManager {
	initialSize := DefaultInitialWindowSize // As per RFC 7540, Section 6.9.1

	cfcm := &ConnectionFlowControlManager{
		sendWindow:               NewFlowControlWindow(initialSize, true, 0), // Our send window (streamID 0 for connection)
		currentReceiveWindowSize: int64(initialSize),                         // Peer's send window (our receive capacity)
		windowUpdateThreshold:    initialSize / 2,                            // Send update when half the window is consumed
		// bytesConsumedTotal and lastWindowUpdateSentAt start at 0.
	}

	// Ensure threshold is at least 1 if initialSize is positive, to allow updates.
	if cfcm.windowUpdateThreshold == 0 && initialSize > 0 {
		cfcm.windowUpdateThreshold = 1
	}
	return cfcm
}

// AcquireSendSpace allows a data sender to request 'n' bytes of window capacity from the connection-level send window.
// It fulfills the requirements:
//  1. Request window capacity: The 'n' parameter specifies the amount.
//  2. Block if unavailable: This method blocks if 'n' bytes are not currently available in the send window,
//     waiting until space is granted by the peer (via WINDOW_UPDATE) or an error occurs.
//  3. Consume window space: Upon successful acquisition (i.e., when the method returns nil),
//     the requested 'n' bytes are immediately deducted from the available connection send window.
//     This consumption happens before the actual data transmission by the caller.
//
// If 'n' is 0, it returns nil immediately as zero-length DATA frames do not consume window space.
// Otherwise, it delegates to the underlying FlowControlWindow's Acquire method.
// An error is returned if the window is closed, an unrecoverable flow control error has occurred,
// or if 'n' is invalid for the underlying Acquire method (though 'n == 0' is handled here).
func (cfcm *ConnectionFlowControlManager) AcquireSendSpace(n uint32) error {
	if n == 0 { // Sending zero-length DATA frame does not consume window space.
		return nil
	}
	return cfcm.sendWindow.Acquire(n)
}

// HandleWindowUpdateFromPeer processes a WINDOW_UPDATE frame received from the peer
// for the connection (StreamID 0). This increases our send window.
// Returns an error if the increment is invalid (e.g., causes overflow).
func (cfcm *ConnectionFlowControlManager) HandleWindowUpdateFromPeer(increment uint32) error {
	// Validation of increment (not 0 for connection, not causing overflow)
	// is handled by sendWindow.Increase().
	return cfcm.sendWindow.Increase(increment)
}

// DataReceived is called when 'n' bytes of a DATA frame payload are received on the connection.
// It consumes 'n' bytes from the peer's send window (our receive window).
// Returns a ConnectionError of type FLOW_CONTROL_ERROR if the peer violates flow control
// (i.e., sends more data than currently allowed).
func (cfcm *ConnectionFlowControlManager) DataReceived(n uint32) error {
	if n == 0 { // Receiving a DATA frame with no payload does not affect flow control.
		return nil
	}

	cfcm.receiveWindowMu.Lock()
	defer cfcm.receiveWindowMu.Unlock()

	if int64(n) > cfcm.currentReceiveWindowSize {
		// Peer sent more data than available in its send window (our receive window).
		msg := fmt.Sprintf("connection flow control error: received %d bytes, but peer's window was only %d", n, cfcm.currentReceiveWindowSize)
		// This is a connection error of type FLOW_CONTROL_ERROR.
		// The connection manager should send GOAWAY and terminate.
		// currentReceiveWindowSize is not modified here as the connection is expected to terminate.
		return NewConnectionError(ErrCodeFlowControlError, msg)
	}

	cfcm.currentReceiveWindowSize -= int64(n)
	return nil
}

// ApplicationConsumedData is called when the application has processed 'n' bytes of data
// that were previously accounted for in DataReceived. This makes space available in our
// receive window, potentially triggering a WINDOW_UPDATE to the peer.
// It returns the size of the increment for the WINDOW_UPDATE frame if one should be sent,
// otherwise 0.
// Returns an error if 'n' is invalid or if an internal accounting error occurs.
func (cfcm *ConnectionFlowControlManager) ApplicationConsumedData(n uint32) (uint32, error) {
	if n == 0 {
		return 0, nil
	}

	// The amount consumed, 'n', cannot itself exceed the maximum window size,
	// as it might form the basis of a WINDOW_UPDATE increment.
	if n > MaxWindowSize {
		return 0, NewConnectionError(ErrCodeInternalError, fmt.Sprintf("application consumed %d bytes, which exceeds MaxWindowSize %d for a single increment step", n, MaxWindowSize))
	}

	cfcm.receiveWindowMu.Lock()
	defer cfcm.receiveWindowMu.Unlock()

	// Safely add to bytesConsumedTotal
	newBytesConsumedTotal := cfcm.bytesConsumedTotal + uint64(n)
	if newBytesConsumedTotal < cfcm.bytesConsumedTotal { // Check for uint64 overflow
		return 0, NewConnectionError(ErrCodeInternalError, "connection bytesConsumedTotal overflow during application consumption")
	}

	// Calculate the total amount of data consumed since the last WINDOW_UPDATE was sent for the connection.
	// This will be the increment for the new WINDOW_UPDATE frame.
	pendingIncrement := uint32(newBytesConsumedTotal - cfcm.lastWindowUpdateSentAt)

	// A WINDOW_UPDATE frame's increment MUST be greater than 0. (RFC 7540, 6.9)
	// If n > 0, pendingIncrement should be > 0 unless newBytesConsumedTotal == lastWindowUpdateSentAt,
	// which implies an issue or that n was effectively 0 after previous updates.
	if pendingIncrement == 0 && n > 0 {
		// This state could occur if, for example, lastWindowUpdateSentAt was already newBytesConsumedTotal
		// due to some prior logic. If n>0, this implies something is amiss or it's a no-op for now.
		// For safety, and strict adherence, if n > 0, we expect pendingIncrement to be potentially > 0.
		// However, if threshold is not met, we return 0 anyway.
		// The critical check is before returning a non-zero increment.
	}

	// The increment value for WINDOW_UPDATE must not exceed MaxWindowSize.
	if pendingIncrement > MaxWindowSize {
		// This indicates that the accumulated consumption is too large for a single WINDOW_UPDATE.
		// This situation implies an internal logic error or that the update threshold is too large
		// or not being effective.
		return 0, NewConnectionError(ErrCodeInternalError,
			fmt.Sprintf("calculated connection WINDOW_UPDATE increment %d exceeds MaxWindowSize %d", pendingIncrement, MaxWindowSize))
	}

	// Update our internal view of the receive window size.
	// currentReceiveWindowSize logically increases because we can now accept 'n' more bytes.
	newCurrentReceiveWindowSize := cfcm.currentReceiveWindowSize + int64(n)
	if newCurrentReceiveWindowSize > MaxWindowSize {
		// Our logical view of the total space we are offering the peer has exceeded the max.
		// This is an internal error; our accounting is flawed.
		return 0, NewConnectionError(ErrCodeInternalError,
			fmt.Sprintf("internal: connection receive window effective size %d would exceed MaxWindowSize %d after consumption", newCurrentReceiveWindowSize, MaxWindowSize))
	}

	cfcm.bytesConsumedTotal = newBytesConsumedTotal
	cfcm.currentReceiveWindowSize = newCurrentReceiveWindowSize

	// Determine if a WINDOW_UPDATE frame should be sent.
	// Send if the accumulated consumed data (pendingIncrement) meets the threshold.
	// Also ensure the increment is > 0, as WINDOW_UPDATE with 0 increment is a protocol error for streams
	// and a no-op (but wasteful) for connections if we were to send it.
	if pendingIncrement >= cfcm.windowUpdateThreshold && pendingIncrement > 0 {
		cfcm.lastWindowUpdateSentAt = cfcm.bytesConsumedTotal // Record that an update for this amount is being issued.
		return pendingIncrement, nil
	}

	return 0, nil // No WINDOW_UPDATE to send yet.
}

// Close signals that the connection is closing. It closes the underlying send flow control window,
// which will unblock any goroutines waiting to acquire send space.
func (cfcm *ConnectionFlowControlManager) Close(err error) {
	cfcm.sendWindow.Close(err)
	// The receive side counters don't need explicit closing beyond their mutex.
}

// GetConnectionSendAvailable returns the current available space in our connection send window.
func (cfcm *ConnectionFlowControlManager) GetConnectionSendAvailable() int64 {
	return cfcm.sendWindow.Available()
}

// GetConnectionReceiveAvailable returns the current available space in our connection receive window
// (i.e., how much more data the peer can send us before we need to send a WINDOW_UPDATE).
func (cfcm *ConnectionFlowControlManager) GetConnectionReceiveAvailable() int64 {
	cfcm.receiveWindowMu.Lock()
	defer cfcm.receiveWindowMu.Unlock()
	return cfcm.currentReceiveWindowSize
}

// StreamFlowControlManager manages send and receive flow control for a single HTTP/2 stream.
type StreamFlowControlManager struct {
	streamID uint32

	// sendWindow governs how much data this endpoint can send on this stream.
	// This window is initialized with the peer's SETTINGS_INITIAL_WINDOW_SIZE
	// and replenished by WINDOW_UPDATE frames from the peer for this stream.
	sendWindow *FlowControlWindow

	// Receive-side flow control for this stream:
	receiveWindowMu                   sync.Mutex
	currentReceiveWindowSize          int64  // How much more data the peer can send us on this stream.
	effectiveInitialReceiveWindowSize uint32 // The current SETTINGS_INITIAL_WINDOW_SIZE value applied to this stream's receive side.
	bytesConsumedTotal                uint64 // Cumulative bytes consumed by our app from this stream.
	totalBytesReceived                uint64 // Cumulative payload bytes received on this stream from DATA frames.
	lastWindowUpdateSentAt            uint64 // bytesConsumedTotal when last stream WINDOW_UPDATE was sent.
	windowUpdateThreshold             uint32 // Threshold to send WINDOW_UPDATE for this stream.

	// logger *logger.Logger // Optional: for detailed logging

	// TODO: Add fields for closed state of the stream affecting flow control operations.
}

// NewStreamFlowControlManager creates a new manager for stream-level flow control.
// - streamID: The ID of the stream this manager is for.
// - ourInitialWindowSize: This server's SETTINGS_INITIAL_WINDOW_SIZE. Used for our receive window for this stream.
// - peerInitialWindowSize: The peer's SETTINGS_INITIAL_WINDOW_SIZE. Used for our send window for this stream.
func NewStreamFlowControlManager(streamID uint32, ourInitialWindowSize, peerInitialWindowSize uint32) *StreamFlowControlManager {
	if ourInitialWindowSize > MaxWindowSize {
		ourInitialWindowSize = MaxWindowSize // Defensive, should be validated by settings handler
	}
	if peerInitialWindowSize > MaxWindowSize {
		peerInitialWindowSize = MaxWindowSize // Defensive
	}

	sfcm := &StreamFlowControlManager{
		streamID:                          streamID,
		sendWindow:                        NewFlowControlWindow(peerInitialWindowSize, false, streamID),
		currentReceiveWindowSize:          int64(ourInitialWindowSize),
		effectiveInitialReceiveWindowSize: ourInitialWindowSize, // Initialize with the current setting
		windowUpdateThreshold:             ourInitialWindowSize / 2,
		// bytesConsumedTotal and lastWindowUpdateSentAt start at 0.
	}

	// Ensure threshold is at least 1 if initialSize is positive, to allow updates.
	if sfcm.windowUpdateThreshold == 0 && ourInitialWindowSize > 0 {
		sfcm.windowUpdateThreshold = 1
	}

	return sfcm
}

// AcquireSendSpace allows a data sender to request 'n' bytes of window capacity from this stream's send window.
// It fulfills the requirements:
//  1. Request window capacity: The 'n' parameter specifies the amount.
//  2. Block if unavailable: This method blocks if 'n' bytes are not currently available in the send window,
//     waiting until space is granted by the peer (via WINDOW_UPDATE for this stream) or an error occurs.
//  3. Consume window space: Upon successful acquisition (i.e., when the method returns nil),
//     the requested 'n' bytes are immediately deducted from the available stream send window.
//     This consumption happens before the actual data transmission by the caller.
//
// If 'n' is 0, it returns nil immediately as zero-length DATA frames do not consume window space.
// Otherwise, it delegates to the underlying FlowControlWindow's Acquire method.
// An error is returned if the window is closed, an unrecoverable flow control error has occurred,
// or if 'n' is invalid for the underlying Acquire method (though 'n == 0' is handled here).
func (sfcm *StreamFlowControlManager) AcquireSendSpace(n uint32) error {
	if n == 0 { // Sending zero-length DATA frame does not consume window space.
		return nil
	}
	return sfcm.sendWindow.Acquire(n)
}

// HandleWindowUpdateFromPeer processes a WINDOW_UPDATE frame received from the peer
// for this specific stream. This increases our send window for this stream.
// Returns an error if the increment is invalid (e.g., 0, or causes overflow).
func (sfcm *StreamFlowControlManager) HandleWindowUpdateFromPeer(increment uint32) error {
	if sfcm.streamID == 0 {
		// A StreamFlowControlManager instance should always be associated with a non-zero stream ID.
		// Handling stream 0 here indicates a server-internal logic error or misconfiguration.
		// Connection-level WINDOW_UPDATE should be handled by ConnectionFlowControlManager.
		return NewConnectionError(ErrCodeInternalError, "internal error: StreamFlowControlManager.HandleWindowUpdateFromPeer called for stream ID 0")
	}

	// Validation (increment != 0 for streams, not causing overflow) is handled by sendWindow.Increase().
	// sendWindow.Increase() is aware via its `isConnection` flag (which is false for stream windows)
	// that an increment of 0 is a protocol error for streams.
	return sfcm.sendWindow.Increase(increment)
}

// DataReceived is called when 'n' bytes of a DATA frame payload are received on this stream.
// It consumes 'n' bytes from the peer's send window for this stream (our stream receive window).
// Returns a StreamError of type FLOW_CONTROL_ERROR if the peer violates flow control
// (i.e., sends more data than currently allowed on this stream).
func (sfcm *StreamFlowControlManager) DataReceived(n uint32) error {
	if n == 0 { // Receiving a DATA frame with no payload does not affect flow control.
		return nil
	}

	sfcm.receiveWindowMu.Lock()
	defer sfcm.receiveWindowMu.Unlock()

	if int64(n) > sfcm.currentReceiveWindowSize {
		// Peer sent more data than available in its stream send window (our stream receive window).
		msg := fmt.Sprintf("stream %d flow control error: received %d bytes, but peer's send allowance (our receive window) was only %d",
			sfcm.streamID, n, sfcm.currentReceiveWindowSize)
		return NewStreamError(sfcm.streamID, ErrCodeFlowControlError, msg)
	}

	sfcm.currentReceiveWindowSize -= int64(n)
	sfcm.totalBytesReceived += uint64(n) // Track total bytes received on this stream. uint64 overflow is negligible.
	return nil
}

// ApplicationConsumedData is called when the application has processed 'n' bytes of data
// from this stream that were previously accounted for in DataReceived. This makes space
// available in our stream receive window, potentially triggering a WINDOW_UPDATE to the peer for this stream.
// It returns the size of the increment for the WINDOW_UPDATE frame if one should be sent, otherwise 0.
// Returns an error if 'n' is invalid or if an internal accounting error occurs.
func (sfcm *StreamFlowControlManager) ApplicationConsumedData(n uint32) (uint32, error) {
	if n == 0 {
		return 0, nil
	}

	if n > MaxWindowSize {
		return 0, NewStreamError(sfcm.streamID, ErrCodeInternalError,
			fmt.Sprintf("application consumed %d bytes from stream %d, which exceeds MaxWindowSize %d for a single increment step", n, sfcm.streamID, MaxWindowSize))
	}

	sfcm.receiveWindowMu.Lock()
	defer sfcm.receiveWindowMu.Unlock()

	newBytesConsumedTotal := sfcm.bytesConsumedTotal + uint64(n)
	if newBytesConsumedTotal < sfcm.bytesConsumedTotal { // Check for uint64 overflow
		return 0, NewStreamError(sfcm.streamID, ErrCodeInternalError,
			fmt.Sprintf("stream %d bytesConsumedTotal overflow during application consumption", sfcm.streamID))
	}

	pendingIncrement := uint32(newBytesConsumedTotal - sfcm.lastWindowUpdateSentAt)

	if pendingIncrement == 0 && n > 0 {
		// No-op for now, threshold check will handle it.
	}

	if pendingIncrement > MaxWindowSize {
		return 0, NewStreamError(sfcm.streamID, ErrCodeInternalError,
			fmt.Sprintf("calculated stream %d WINDOW_UPDATE increment %d exceeds MaxWindowSize %d", sfcm.streamID, pendingIncrement, MaxWindowSize))
	}

	newCurrentReceiveWindowSize := sfcm.currentReceiveWindowSize + int64(n)
	if newCurrentReceiveWindowSize > MaxWindowSize {
		// This is an internal error in our accounting for the stream's receive window.
		// It means if we were to simply add 'n' to currentReceiveWindowSize, it would overflow.
		// This should ideally not happen if initial sizes and consumption are handled correctly.
		return 0, NewStreamError(sfcm.streamID, ErrCodeInternalError,
			fmt.Sprintf("internal: stream %d receive window effective size %d would exceed MaxWindowSize %d after consumption", sfcm.streamID, newCurrentReceiveWindowSize, MaxWindowSize))
	}

	sfcm.bytesConsumedTotal = newBytesConsumedTotal
	sfcm.currentReceiveWindowSize = newCurrentReceiveWindowSize

	// Send WINDOW_UPDATE if accumulated consumed data meets threshold AND increment is > 0.
	if pendingIncrement >= sfcm.windowUpdateThreshold && pendingIncrement > 0 {
		sfcm.lastWindowUpdateSentAt = sfcm.bytesConsumedTotal
		return pendingIncrement, nil
	}

	return 0, nil // No WINDOW_UPDATE to send yet for this stream.
}

// HandlePeerSettingsInitialWindowSizeChange is called when the peer's SETTINGS_INITIAL_WINDOW_SIZE changes.
// This affects our send window for this stream.
// Returns a ConnectionError if the peer's new setting would cause our send window to overflow.
func (sfcm *StreamFlowControlManager) HandlePeerSettingsInitialWindowSizeChange(newPeerInitialSize uint32) error {
	// The sendWindow.UpdateInitialWindowSize method handles the logic including overflow check.
	// The error returned by it would be a ConnectionError(FLOW_CONTROL_ERROR) if overflow occurs
	// due to the new setting value.
	return sfcm.sendWindow.UpdateInitialWindowSize(newPeerInitialSize)
}

// HandleOurSettingsInitialWindowSizeChange is called when our server's SETTINGS_INITIAL_WINDOW_SIZE changes.
// This affects our receive window for this stream.
// It adjusts the stream's current receive window size based on the delta between the new
// and previously applied initial window size for this stream.
// Returns a ConnectionError if applying this change would cause the effective receive window to exceed MaxWindowSize.
func (sfcm *StreamFlowControlManager) HandleOurSettingsInitialWindowSizeChange(newOurInitialSize uint32) error {
	sfcm.receiveWindowMu.Lock()
	defer sfcm.receiveWindowMu.Unlock()

	// newOurInitialSize should have been validated against MaxWindowSize by the SETTINGS handler
	// before this method is called. (RFC 6.5.2: SETTINGS_INITIAL_WINDOW_SIZE max is 2^31-1)
	if newOurInitialSize > MaxWindowSize {
		// This would be a programmatic error if newOurInitialSize came from an invalid setting.
		msg := fmt.Sprintf("internal error: newOurInitialSize %d for stream %d receive window adjustment exceeds MaxWindowSize %d",
			newOurInitialSize, sfcm.streamID, MaxWindowSize)
		return NewConnectionError(ErrCodeInternalError, msg)
	}

	delta := int64(newOurInitialSize) - int64(sfcm.effectiveInitialReceiveWindowSize)
	newEffectiveReceiveWindow := sfcm.currentReceiveWindowSize + delta

	// RFC 6.9.2: "An endpoint that receives a SETTINGS frame that causes a flow-control window to exceed the maximum
	// size MUST treat this as a connection error (Section 5.4.1) of type FLOW_CONTROL_ERROR."
	// This applies when WE are the receiver of the SETTINGS frame. Here, we are processing our OWN settings change.
	// The check is for our internal consistency and to ensure we don't misrepresent the window to the peer.
	// If `newEffectiveReceiveWindow` (the amount peer can send us) > MaxWindowSize, it's a problem.
	if newEffectiveReceiveWindow > MaxWindowSize {
		msg := fmt.Sprintf("adjusting stream %d receive window by delta %d (new_init %d, old_eff_init %d) to current %d would result in %d, exceeding MaxWindowSize %d",
			sfcm.streamID, delta, newOurInitialSize, sfcm.effectiveInitialReceiveWindowSize, sfcm.currentReceiveWindowSize, newEffectiveReceiveWindow, MaxWindowSize)
		// This is an internal state inconsistency that could lead to protocol violations.
		// Terminating the connection due to FLOW_CONTROL_ERROR seems appropriate as our internal state became invalid.
		return NewConnectionError(ErrCodeFlowControlError, msg)
	}
	// Note: newEffectiveReceiveWindow can be negative. The peer (sender) must handle this.

	sfcm.currentReceiveWindowSize = newEffectiveReceiveWindow
	sfcm.effectiveInitialReceiveWindowSize = newOurInitialSize

	// Update the threshold for sending WINDOW_UPDATE frames based on the new initial size.
	sfcm.windowUpdateThreshold = newOurInitialSize / 2
	if sfcm.windowUpdateThreshold == 0 && newOurInitialSize > 0 {
		sfcm.windowUpdateThreshold = 1
	}

	// If the window increased (delta > 0), it doesn't directly trigger a WINDOW_UPDATE from *this* method.
	// WINDOW_UPDATEs are triggered by ApplicationConsumedData. This change simply adjusts the baseline.
	// If delta < 0 and currentReceiveWindowSize becomes very small or negative, the peer will eventually
	// block if it tries to send data. Our ApplicationConsumedData will eventually send WINDOW_UPDATEs
	// to increase it again once space is available and threshold met.

	return nil
}

// Close signals that the stream is closing. It closes the underlying send flow control window,
// which will unblock any goroutines waiting to acquire send space for this stream.
func (sfcm *StreamFlowControlManager) Close(err error) {
	sfcm.sendWindow.Close(err)
	// Receive side counters don't need explicit closing beyond their mutex.
	// Higher-level stream state (e.g., closed, reset) will prevent further DataReceived calls.
}

// GetStreamSendAvailable returns the current available space in this stream's send window.
func (sfcm *StreamFlowControlManager) GetStreamSendAvailable() int64 {
	return sfcm.sendWindow.Available()
}

// GetStreamReceiveAvailable returns the current available space in this stream's receive window
// (i.e., how much more data the peer can send us on this stream before we need to send a WINDOW_UPDATE).
func (sfcm *StreamFlowControlManager) GetStreamReceiveAvailable() int64 {
	sfcm.receiveWindowMu.Lock()
	defer sfcm.receiveWindowMu.Unlock()
	return sfcm.currentReceiveWindowSize
}

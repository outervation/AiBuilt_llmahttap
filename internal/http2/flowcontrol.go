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
	if newInitialSize > MaxWindowSize { // Defensive check based on RFC 6.5.2 for SETTINGS_INITIAL_WINDOW_SIZE max value
		msg := fmt.Sprintf("new SETTINGS_INITIAL_WINDOW_SIZE %d exceeds MaxWindowSize %d", newInitialSize, MaxWindowSize)
		// This is a connection error because the peer sent an invalid SETTINGS value.
		err := NewConnectionError(ErrCodeProtocolError, msg)
		// fcw.setErrorLocked(err) // The FCW is not directly at fault, the setting is. Let connection handler deal.
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

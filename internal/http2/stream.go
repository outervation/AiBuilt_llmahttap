package http2

import (
	"context"

	"io"
	"net/http" // For http.Header, http.Request
	"net/url"  // For url.URL

	"runtime/debug" // For stack traces in panic recovery
	"strings"       // For strings.Index and strings.HasPrefix

	"fmt" // For error formatting, Sprintf
	// "net/url" // For url.URL (REMOVED as potentially unused or only in stubs)
	"sync"

	"example.com/llmahttap/v2/internal/logger" // For logging within stream operations
	"golang.org/x/net/http2/hpack"             // For hpack.HeaderField
)

// StreamState represents the state of an HTTP/2 stream, as defined in RFC 7540, Section 5.1.

// TODO: Remove this stub once connection.go is implemented

type InitialSettings struct { // Placeholder
	MaxFrameSize         uint32
	InitialWindowSize    uint32
	HeaderTableSize      uint32
	MaxConcurrentStreams uint32
	MaxHeaderListSize    uint32
	EnablePush           bool
}
type StreamState uint8

const (
	// StreamStateIdle indicates that the stream is not yet created or has been closed.
	// All streams start in the "idle" state.
	StreamStateIdle StreamState = iota

	// StreamStateReservedLocal indicates that the stream has been reserved by sending a PUSH_PROMISE frame.
	// Transition from "idle" by sending PUSH_PROMISE.
	StreamStateReservedLocal

	// StreamStateReservedRemote indicates that the stream has been reserved by receiving a PUSH_PROMISE frame.
	// Transition from "idle" by receiving PUSH_PROMISE.
	StreamStateReservedRemote

	// StreamStateOpen indicates that the stream is active and can be used by both peers to send frames of any type.
	// Transition from "idle" by sending or receiving a HEADERS frame.
	// Transition from "reserved (local)" by sending a HEADERS frame.
	// Transition from "reserved (remote)" by receiving a HEADERS frame.
	StreamStateOpen

	// StreamStateHalfClosedLocal indicates that this endpoint (local) has sent a frame with END_STREAM flag.
	// The local endpoint can no longer send DATA or HEADERS frames, but can receive.
	// Transition from "open" or "reserved (remote)" by sending END_STREAM.
	StreamStateHalfClosedLocal

	// StreamStateHalfClosedRemote indicates that the remote peer has sent a frame with END_STREAM flag.
	// The local endpoint can no longer receive DATA or HEADERS frames, but can send.
	// Transition from "open" or "reserved (local)" by receiving END_STREAM.
	StreamStateHalfClosedRemote

	// StreamStateClosed indicates that the stream is terminated.
	// Transition from any state by sending or receiving RST_STREAM.
	// Transition from "open" after both peers send/receive END_STREAM.
	// Transition from "half-closed (local)" by receiving END_STREAM or RST_STREAM.
	// Transition from "half-closed (remote)" by sending END_STREAM or RST_STREAM.
	StreamStateClosed
)

// String returns a string representation of the StreamState.
func (s StreamState) String() string {
	switch s {
	case StreamStateIdle:
		return "idle"
	case StreamStateReservedLocal:
		return "reserved (local)"
	case StreamStateReservedRemote:
		return "reserved (remote)"
	case StreamStateOpen:
		return "open"
	case StreamStateHalfClosedLocal:
		return "half-closed (local)"
	case StreamStateHalfClosedRemote:
		return "half-closed (remote)"
	case StreamStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// ResponseWriter defines the interface for handlers to write HTTP/2 responses.
// The Stream itself will likely implement this.

// Handler is the interface that processes requests for a given route.
// Feature Spec 1.4.2: "Each handler implementation MUST conform to a server-defined internal interface
// (e.g., a Go interface) that accepts necessary request details (like http.Request or equivalent
// structured data), a means to write the response (like http.ResponseWriter or equivalent stream
// writer), and its specific HandlerConfig (as an opaque structure to be type-asserted by the handler)."
type Handler interface {
	// ServeHTTP2 processes the request on the given stream.
	// The stream provides methods to get request details and to write the response.
	// The handlerConfig is passed to the handler when it's instantiated.
	ServeHTTP2(s StreamWriter, req *http.Request)
}

// Compile-time check to ensure *Stream implements ResponseWriter.

// Stream represents a single HTTP/2 stream.
// It manages stream state, flow control, priority, and request/response processing.
type Stream struct {
	id    uint32
	state StreamState
	mu    sync.RWMutex // Protects stream state and other mutable fields like headers, body parts.

	// Connection context
	conn *Connection // Parent connection, defined in connection.go

	// HTTP/2 specific properties
	fcManager         *StreamFlowControlManager // Stream-level flow control manager
	priorityWeight    uint8                     // Stream weight (0-255, effective 1-256)
	priorityParentID  uint32                    // Parent stream ID for priority
	priorityExclusive bool                      // Exclusive flag for priority dependency

	// Request handling
	requestHeaders    []hpack.HeaderField
	requestTrailers   []hpack.HeaderField // For storing received trailer headers
	requestBodyReader *io.PipeReader      // For handler to read incoming DATA frames
	requestBodyWriter *io.PipeWriter      // For stream to write incoming DATA frames into

	// Response handling (Stream implements ResponseWriter or provides one)
	responseHeadersSent bool                // True if response HEADERS frame has been sent
	responseTrailers    []hpack.HeaderField // Trailers to be sent with last DATA or separately

	// Stream lifecycle and state
	ctx                         context.Context    // Context for this stream, derived from connection context
	cancelCtx                   context.CancelFunc // Cancels the stream context
	endStreamReceivedFromClient bool               // True if client sent END_STREAM flag
	endStreamSentToClient       bool               // True if server sent END_STREAM flag
	pendingRSTCode              *ErrorCode         // If non-nil, an RST_STREAM with this code needs to be sent (or was received)
	initiatedByPeer             bool               // True if this stream was initiated by the peer

	// Channels for internal coordination (examples, might evolve)
	// headersComplete chan struct{} // Signals that all request headers (and CONTINUATIONs) have been received
	// dataAvailable   *sync.Cond  // Signals new data in requestBodyWriter or error
	// streamErr       error       // Records a terminal error for the stream

	// Buffer for request body if not directly piped.
	// For now, using io.Pipe which handles buffering.
	// _requestDataBuffer *bytes.Buffer

	// TODO: Add fields for server push if implementing PUSH_PROMISE
}

// NewStream creates a new stream.
// conn: The parent connection.
// id: The stream ID.
// initialOurWindowSize: Our setting for initial window size (affects stream's receive window).
// initialPeerWindowSize: Peer's setting for initial window size (affects stream's send window).
// handler: The handler for this stream.
// handlerCfg: The configuration for the handler.
// TODO: Priority information for new streams (either default or from HEADERS).

func newStream(
	conn *Connection,
	id uint32,
	initialOurWindowSize uint32,

	initialPeerWindowSize uint32,
	prioWeight uint8,
	prioParentID uint32,
	prioExclusive bool,
	isInitiatedByPeer bool, // Added parameter
) (*Stream, error) {
	ctx, cancel := context.WithCancel(conn.ctx)

	pr, pw := io.Pipe()

	s := &Stream{
		id:                          id,
		state:                       StreamStateIdle,
		conn:                        conn,
		fcManager:                   NewStreamFlowControlManager(conn, id, initialOurWindowSize, initialPeerWindowSize),
		priorityWeight:              prioWeight,
		priorityParentID:            prioParentID,
		priorityExclusive:           prioExclusive,
		requestBodyReader:           pr,
		requestBodyWriter:           pw,
		ctx:                         ctx,
		cancelCtx:                   cancel,
		endStreamReceivedFromClient: false,
		endStreamSentToClient:       false,
		initiatedByPeer:             isInitiatedByPeer, // Set the field
		responseHeadersSent:         false,
	}

	priorityInfo := &streamDependencyInfo{
		StreamDependency: prioParentID,
		Weight:           prioWeight,
		Exclusive:        prioExclusive,
	}
	if conn.priorityTree != nil { // Defensive check, stub connection might not have it
		err := conn.priorityTree.AddStream(s.id, priorityInfo)
		if err != nil {
			cancel()
			_ = pr.CloseWithError(err) // Best effort close
			_ = pw.CloseWithError(err) // Best effort close
			return nil, err
		}
	}
	return s, nil
}

// Implement ResponseWriter for Stream
// Note: These methods will need to interact with the connection to send frames.

// SendHeaders sends response headers.
func (s *Stream) SendHeaders(headers []HeaderField, endStream bool) error {
	s.mu.Lock()
	// defer s.mu.Unlock() // Unlock must be carefully managed due to calls to conn

	if s.responseHeadersSent {
		s.mu.Unlock()
		s.conn.log.Error("stream: SendHeaders called after headers already sent", logger.LogFields{"stream_id": s.id})
		return NewStreamError(s.id, ErrCodeInternalError, "headers already sent")
	}
	if s.state == StreamStateClosed || s.pendingRSTCode != nil {
		s.mu.Unlock()
		return NewStreamError(s.id, ErrCodeStreamClosed, "stream closed or being reset")
	}
	if s.state == StreamStateHalfClosedLocal && s.endStreamSentToClient {
		s.mu.Unlock()
		return NewStreamError(s.id, ErrCodeStreamClosed, "cannot send headers on half-closed (local) stream after END_STREAM")
	}
	s.mu.Unlock() // Unlock before calling conn method which might also lock

	hpackHeaders := http2HeaderFieldsToHpackHeaderFields(headers)
	s.conn.log.Debug("Stream: PRE-CALL conn.sendHeadersFrame", logger.LogFields{"stream_id": s.id, "s_conn_nil": s.conn == nil, "s_conn_log_nil": s.conn != nil && s.conn.log == nil, "num_headers": len(hpackHeaders), "end_stream": endStream})
	err := s.conn.sendHeadersFrame(s, hpackHeaders, endStream)

	s.mu.Lock() // Re-lock to update stream state
	defer s.mu.Unlock()

	if err != nil {
		// If sending headers failed, don't mark them as sent or update stream state based on send.
		// The connection/sender should handle the error (e.g., RST or close connection).
		return err
	}

	s.responseHeadersSent = true
	if endStream {
		s.endStreamSentToClient = true
		s.transitionStateOnSendEndStream() // Update state based on sending END_STREAM
	}

	return nil
}

// streamDependencyInfo is defined in priority.go, but for clarity if it were needed
// specifically for stream creation/initialization, it might look like this.
// However, we use the one from priority.go.
// type StreamPriorityInfo struct {
//  StreamDependency uint32
//  Weight           uint8
//  Exclusive        bool
// }

// Close explicitly terminates the stream from the server's perspective.
// If err is nil, ErrCodeCancel will be used to RST_STREAM, indicating a
// server-initiated cancellation. If err is a StreamError or ConnectionError,
// its code will be used. Otherwise, ErrCodeInternalError is used.
// This method ensures the stream transitions to Closed and resources are cleaned up.
// It returns any error encountered while trying to send the RST_STREAM frame.
func (s *Stream) Close(err error) error {
	s.mu.Lock() // Lock to check state and prevent races on pendingRSTCode
	// If already definitively closed and RST_STREAM processing initiated, no-op.
	if s.state == StreamStateClosed && s.pendingRSTCode != nil {
		s.mu.Unlock()
		return nil
	}
	// If it's closed but not due to our RST action, and we now want to RST, proceed.
	// If it's merely half-closed, we can still RST it.
	s.mu.Unlock() // Unlock before calling sendRSTStream which has its own locking.

	var rstCode ErrorCode
	if err == nil {
		rstCode = ErrCodeCancel
	} else {
		// Try to extract ErrorCode from the provided error
		switch e := err.(type) {
		case *StreamError:
			rstCode = e.Code
		case *ConnectionError: // Less common for a stream-specific Close, but possible
			rstCode = e.Code
		default:
			// For generic errors, use a generic code.
			// ErrCodeInternalError suggests an issue on our side.
			rstCode = ErrCodeInternalError
		}
	}

	// sendRSTStream will handle state transition to Closed and resource cleanup
	// via _setState -> closeStreamResourcesProtected.
	// closeStreamResourcesProtected calls conn.removeStream, which is assumed to handle
	// removal from the priority tree as well.
	return s.sendRSTStream(rstCode)
}

// streamDependencyInfo is defined in priority.go, but for clarity if it were needed
// specifically for stream creation/initialization, it might look like this.
// However, we use the one from priority.go.
// type StreamPriorityInfo struct {
//  StreamDependency uint32
//  Weight           uint8
//  Exclusive        bool
// }

// WriteData sends a chunk of the response body.
func (s *Stream) WriteData(p []byte, endStream bool) (n int, err error) {
	s.mu.Lock() // Initial lock for checks
	if !s.responseHeadersSent {
		s.mu.Unlock()
		s.conn.log.Error("stream: WriteData called before SendHeaders", logger.LogFields{"stream_id": s.id})
		return 0, NewStreamError(s.id, ErrCodeInternalError, "SendHeaders must be called before WriteData")
	}
	// Check if stream is in a state where sending DATA is allowed, or if it's already ended from server side.
	if (s.state == StreamStateHalfClosedLocal && s.endStreamSentToClient) || s.state == StreamStateClosed || s.pendingRSTCode != nil {
		errCode := ErrCodeStreamClosed
		if s.pendingRSTCode != nil && s.state == StreamStateClosed { // If closed due to RST, use that code.
			errCode = *s.pendingRSTCode
		}
		s.mu.Unlock()
		return 0, NewStreamError(s.id, errCode, "cannot send data on closed, reset, or already server-ended stream")
	}
	s.mu.Unlock() // Unlock before potential blocking operations or multiple send calls

	totalBytesToWrite := len(p)
	dataOffset := 0

	// Handle case: empty body with endStream=true (zero-length DATA frame with END_STREAM)
	if totalBytesToWrite == 0 {
		if !endStream {
			return 0, nil // Nothing to send, not ending stream.
		}
		// Here, len(p) == 0 AND endStream == true. Send empty DATA frame with END_STREAM.
		// No flow control needed for zero-length DATA frames.

		s.mu.Lock() // Lock for state check and update around sendDataFrame
		// Check if stream was already ended by another WriteData call concurrently (e.g. WriteTrailers)
		if s.endStreamSentToClient {
			s.mu.Unlock()
			s.conn.log.Warn("stream: WriteData with endStream=true called, but stream already marked as ended by server", logger.LogFields{"stream_id": s.id})
			return 0, NewStreamError(s.id, ErrCodeInternalError, "stream already ended by a previous write with endStream=true")
		}
		// s.state checks already done above, assuming it hasn't changed to closed by a concurrent RST *after* initial unlock.

		// A truly robust check would re-verify s.state here, but that adds complexity for a very racy condition.
		// If state changed, sendDataFrame might fail or be ignored.
		s.mu.Unlock() // Unlock for the sendDataFrame call

		// The stub sendDataFrame returns (int, error). We expect 0 bytes for an empty frame.
		s.conn.log.Debug("Stream: PRE-CALL conn.sendDataFrame (empty)", logger.LogFields{"stream_id": s.id, "s_conn_nil": s.conn == nil, "s_conn_log_nil": s.conn != nil && s.conn.log == nil, "data_len": 0, "end_stream": true})
		_, sendErr := s.conn.sendDataFrame(s, nil, true)

		s.mu.Lock() // Re-lock for state update
		if sendErr == nil {
			s.endStreamSentToClient = true
			s.transitionStateOnSendEndStream()
		} else {
			s.conn.log.Error("stream: failed to send empty DATA frame with END_STREAM", logger.LogFields{"stream_id": s.id, "error": sendErr.Error()})
		}
		s.mu.Unlock()
		return 0, sendErr
	}

	// Determine max frame payload size
	// This should be the MIN(our internal max, peer's SETTINGS_MAX_FRAME_SIZE)
	// For the stub, s.conn.maxFrameSize is assumed to be this effective value.
	maxFramePayloadSize := s.conn.maxFrameSize
	if maxFramePayloadSize == 0 {
		maxFramePayloadSize = DefaultMaxFrameSize // 16384, default from RFC 7540, 4.2 & 6.5.2
	}
	// RFC 7540 Section 4.2: "The default value is 2^14 (16,384) octets."
	// It doesn't explicitly state a *minimum* allowed frame size for DATA frames themselves, but SETTINGS_MAX_FRAME_SIZE
	// has a minimum of 2^14. So a peer *should not* advertise less than this.
	// For our own logic, ensure we don't try to send frames smaller than reasonable if config is bad.
	// However, the spec says *payload* is limited. A 0-length payload is fine.
	// This check is more about chunking logic. Using DefaultMaxFrameSize as lower bound for chunking if not set.

	s.conn.log.Debug("Stream.WriteData: Max frame payload size for chunking determined", logger.LogFields{"stream_id": s.id, "max_frame_payload_size": maxFramePayloadSize, "initial_data_len": totalBytesToWrite})
	for dataOffset < totalBytesToWrite {
		chunkLen := totalBytesToWrite - dataOffset
		if chunkLen > int(maxFramePayloadSize) {
			chunkLen = int(maxFramePayloadSize)
		}

		// Acquire flow control for this chunk (chunkLen is always > 0 in this loop)
		// Stream-level FC
		if errFc := s.fcManager.AcquireSendSpace(uint32(chunkLen)); errFc != nil {
			s.conn.log.Error("stream: failed to acquire stream flow control",
				logger.LogFields{"stream_id": s.id, "chunk_len": chunkLen, "requested": chunkLen, "available": s.fcManager.GetStreamSendAvailable(), "error": errFc.Error()})
			return dataOffset, errFc // Return bytes successfully sent so far
		}

		// Connection-level FC
		if s.conn.connFCManager != nil { // Defensive, stub might not init
			if errFc := s.conn.connFCManager.AcquireSendSpace(uint32(chunkLen)); errFc != nil {
				s.conn.log.Error("stream: failed to acquire connection flow control",
					logger.LogFields{"stream_id": s.id, "chunk_len": chunkLen, "requested": chunkLen, "conn_available": s.conn.connFCManager.GetConnectionSendAvailable(), "error": errFc.Error()})
				// CRITICAL: Stream FC was acquired but conn FC failed.
				// Release the previously acquired stream flow control.
				if releaseErr := s.fcManager.sendWindow.Increase(uint32(chunkLen)); releaseErr != nil {
					s.conn.log.Error("stream: failed to release stream flow control after conn FC failure",
						logger.LogFields{"stream_id": s.id, "chunk_len": chunkLen, "release_error": releaseErr.Error()})
					// This is a more complex error state; the connection might be unstable.
				}
				return dataOffset, errFc
			}
		}

		// Re-check stream state under lock before sending, as it could have changed (e.g., RST received)
		s.mu.Lock()
		if s.state == StreamStateClosed || s.pendingRSTCode != nil {
			s.mu.Unlock()
			s.conn.log.Warn("stream: state changed to closed/reset after FC acquire, before sending data chunk",
				logger.LogFields{"stream_id": s.id, "chunk_len": chunkLen, "state": s.state.String()})
			// Acquired FC is "lost" from this operation's view. Connection might be closing.
			return dataOffset, NewStreamError(s.id, ErrCodeStreamClosed, "stream closed/reset before send data chunk")
		}
		if s.endStreamSentToClient { // If another operation (e.g. WriteTrailers) ended the stream.
			s.mu.Unlock()
			s.conn.log.Warn("stream: stream ended by server after FC acquire, before sending data chunk",
				logger.LogFields{"stream_id": s.id, "chunk_len": chunkLen})
			return dataOffset, NewStreamError(s.id, ErrCodeInternalError, "stream ended by server before send data chunk")
		}
		s.mu.Unlock() // Unlock for the sendDataFrame call

		chunkData := p[dataOffset : dataOffset+chunkLen]
		isLastChunk := (dataOffset + chunkLen) == totalBytesToWrite
		frameEndStream := isLastChunk && endStream

		s.conn.log.Debug("Stream: PRE-CALL conn.sendDataFrame (chunk)", logger.LogFields{"stream_id": s.id, "s_conn_nil": s.conn == nil, "s_conn_log_nil": s.conn != nil && s.conn.log == nil, "chunk_len": len(chunkData), "end_stream": frameEndStream})
		bytesSentThisChunk, sendErr := s.conn.sendDataFrame(s, chunkData, frameEndStream)

		if sendErr != nil {
			s.conn.log.Error("stream: sendDataFrame failed for data chunk",
				logger.LogFields{"stream_id": s.id, "chunk_len": chunkLen, "error": sendErr.Error()})
			// dataOffset is not incremented for *this* chunk.
			// Acquired FC for this chunk is "lost" if send truly failed.
			return dataOffset, sendErr
		}

		if bytesSentThisChunk != chunkLen {
			// This implies the underlying connection write was partial but didn't error.
			// This is complex to handle gracefully; usually means connection issues.
			// For simplicity, treat as an error condition if not all bytes were "accepted" by sendDataFrame.
			s.conn.log.Error("stream: sendDataFrame reported partial write without error",
				logger.LogFields{"stream_id": s.id, "expected_len": chunkLen, "sent_len": bytesSentThisChunk})
			dataOffset += bytesSentThisChunk // Still account for what was sent
			// Use fmt.Errorf for general errors
			return dataOffset, fmt.Errorf("sendDataFrame reported partial write (%d/%d) for stream %d without error", bytesSentThisChunk, chunkLen, s.id)
		}

		dataOffset += chunkLen
	}

	// After loop, all data is sent.
	s.mu.Lock()
	if endStream {
		// This check ensures that if multiple WriteData calls occur,
		// or if WriteTrailers is called, endStreamSentToClient is set only once.
		if !s.endStreamSentToClient {
			s.endStreamSentToClient = true
			s.transitionStateOnSendEndStream()
		}
	}
	s.mu.Unlock()

	return totalBytesToWrite, nil
}

// sendRSTStream initiates sending an RST_STREAM frame from this endpoint.
func (s *Stream) sendRSTStream(errorCode ErrorCode) error {
	s.mu.Lock()
	if s.state == StreamStateClosed && s.pendingRSTCode != nil && *s.pendingRSTCode == errorCode {
		// Already actioned or being actioned for this specific error.
		s.mu.Unlock()
		return nil
	}
	if s.state == StreamStateClosed { // Generic closed state, might be from peer RST or graceful.
		s.mu.Unlock()
		return nil // Don't send RST on an already closed stream by us.
	}

	s.pendingRSTCode = &errorCode
	s.mu.Unlock()

	err := s.conn.sendRSTStreamFrame(s.id, errorCode)

	s.mu.Lock()         // Lock to update state
	defer s.mu.Unlock() // Ensure unlock even if early return from error

	if err == nil {
		// Successfully sent RST_STREAM frame
		s.pendingRSTCode = &errorCode  // Ensure this is set before calling _setState
		s._setState(StreamStateClosed) // This will call closeStreamResourcesProtected for cleanup

		// Cleanup (pipes, context, conn.removeStream) is handled by closeStreamResourcesProtected.
		return nil
	}

	// Failed to send RST_STREAM.
	s.pendingRSTCode = nil // Revert, as we didn't successfully RST.
	s.conn.log.Error("stream: failed to send RST_STREAM", logger.LogFields{"stream_id": s.id, "error_code": errorCode.String(), "send_error": err.Error()})
	return err // Return the send error
}

// WriteTrailers sends trailing headers after the response body.
func (s *Stream) WriteTrailers(trailers []HeaderField) error {
	s.mu.Lock() // Initial lock for checks
	if !s.responseHeadersSent {
		s.mu.Unlock()
		s.conn.log.Error("stream: WriteTrailers called before SendHeaders", logger.LogFields{"stream_id": s.id})
		return NewStreamError(s.id, ErrCodeInternalError, "SendHeaders must be called before WriteTrailers")
	}
	if s.state == StreamStateClosed || s.pendingRSTCode != nil {
		s.mu.Unlock()
		return NewStreamError(s.id, ErrCodeStreamClosed, "stream closed or being reset, cannot send trailers")
	}
	if s.endStreamSentToClient {
		s.conn.log.Debug("stream: WriteTrailers called after END_STREAM already sent by server. This is okay, trailers are a new HEADERS frame.", logger.LogFields{"stream_id": s.id})
	}

	s.mu.Unlock() // Unlock before calling conn method

	hpackTrailers := http2HeaderFieldsToHpackHeaderFields(trailers)
	// The trailers frame itself will have END_STREAM set.
	// The connection's sendHeadersFrame method must ensure END_STREAM is true for the frame carrying trailers.
	err := s.conn.sendHeadersFrame(s, hpackTrailers, true)

	s.mu.Lock() // Re-lock to update stream state
	defer s.mu.Unlock()

	if err != nil {
		return err
	}

	s.endStreamSentToClient = true
	s.transitionStateOnSendEndStream()

	return nil
}

// transitionStateOnSendEndStream updates the stream state when this endpoint sends END_STREAM.
func (s *Stream) transitionStateOnSendEndStream() {
	// s.mu must be held by the caller
	if !s.endStreamSentToClient {
		// This function should only be called if we actually sent END_STREAM.
		// Log an internal error if called inappropriately.
		s.conn.log.Error("stream: transitionStateOnSendEndStream called but endStreamSentToClient is false", logger.LogFields{"stream_id": s.id, "current_state": s.state.String()})
		return
	}

	switch s.state {
	case StreamStateOpen:
		s._setState(StreamStateHalfClosedLocal)
	case StreamStateReservedLocal: // Sending HEADERS with END_STREAM from ReservedLocal
		s._setState(StreamStateHalfClosedLocal)
	case StreamStateHalfClosedRemote:
		s._setState(StreamStateClosed)
	case StreamStateHalfClosedLocal, StreamStateClosed:
		// No change or already closed. If in HalfClosedLocal, it means we already sent END_STREAM.
		// If called again, it's redundant but not harmful to state.
	default:
		// This indicates an invalid state transition, possibly a bug.
		// Example: Trying to send END_STREAM from Idle or ReservedRemote (server shouldn't do this for ReservedRemote).
		// s.conn.log.Error("stream: invalid state transition on sending END_STREAM", logger.LogFields{"stream_id": s.id, "from_state": s.state.String()})
		// Consider sending RST_STREAM(INTERNAL_ERROR) or connection error.
		// For now, log and potentially let the frame send fail if the state makes it invalid.
		// If the frame send succeeds, the state machine is desynchronized.
		// This function primarily updates our *view* of the state.
	}
}

// _setState sets the stream state and performs necessary actions like cleanup on closure.
// This is an internal method and expects s.mu to be held by the caller.

func (s *Stream) _setState(newState StreamState) {
	if s.state == newState {
		return
	}
	oldState := s.state
	s.state = newState
	s.conn.log.Debug("Stream state changed", logger.LogFields{"stream_id": s.id, "old_state": oldState.String(), "new_state": newState.String()})

	if newState == StreamStateClosed {
		s.closeStreamResourcesProtected()
	}
}

// Context returns the stream's context.
// This is used to satisfy server.ResponseWriterStream if Stream implements it.
func (s *Stream) Context() context.Context {
	return s.ctx
}

// closeStreamResourcesProtected cleans up stream resources. Assumes s.mu is held.
func (s *Stream) closeStreamResourcesProtected() {
	// This function is called when stream transitions to Closed state,
	// either by explicit Close(), RST_STREAM, or graceful (both END_STREAMs).

	// Close the request body pipe. This unblocks handlers trying to read.
	if s.requestBodyWriter != nil {
		// Close with an error that reflects the closure reason if possible.
		// If pendingRSTCode is set, that's a good error. Otherwise, generic EOF or stream closed.
		var closeReason error
		if s.pendingRSTCode != nil {
			closeReason = NewStreamError(s.id, *s.pendingRSTCode, "stream reset")
		} else {
			// If closed gracefully or for other reasons not an RST, io.EOF is typical for PipeReader.
			// However, if state is Closed, it implies termination.
			closeReason = io.EOF // Or a more specific error like "stream closed"
		}
		_ = s.requestBodyWriter.CloseWithError(closeReason) // Best effort
	}
	// if s.requestBodyReader != nil {
	// 	_ = s.requestBodyReader.Close() // Reader side often just needs Close()
	// }

	// Cancel the stream's context.

	// Close flow control manager
	if s.fcManager != nil {
		// The error passed to fcManager.Close() can be used to signal waiters.
		// If pendingRSTCode is set, use that, otherwise a generic stream closed error.
		var fcCloseReason error
		if s.pendingRSTCode != nil {
			fcCloseReason = NewStreamError(s.id, *s.pendingRSTCode, "stream reset affecting flow control")
		} else {
			// Consider if it was a graceful closure (ErrCodeNoError) vs other non-RST.
			// For now, using StreamClosed for non-RST.
			fcCloseReason = NewStreamError(s.id, ErrCodeStreamClosed, "stream closed affecting flow control")
		}
		s.fcManager.Close(fcCloseReason)
	}
	if s.cancelCtx != nil {
		s.cancelCtx()
	}

	// The actual removal from conn.streams and priority tree
	// is now handled by conn.removeStream.
	// This function focuses on *local* resource cleanup for the stream object itself.

	// Mark as definitively cleaned up.
	s.pendingRSTCode = nil // Clear any pending RST intention if we reached Closed state through it.
}

// handleDataFrame processes an incoming DATA frame for this stream.
// It's responsible for:
// 1. Stream-level flow control accounting.
// 2. Passing the data to the requestBodyWriter for the handler to consume.
// 3. Handling the END_STREAM flag.
// Returns a StreamError if stream-level flow control is violated or other stream-specific
// issues occur. Returns a ConnectionError if a problem warrants closing the connection.
func (s *Stream) handleDataFrame(frame *DataFrame) error {
	s.mu.Lock() // Lock for state and flow control updates

	// 1. Stream-level flow control
	payloadLen := uint32(len(frame.Data)) // Assumes frame.Data is the actual payload after any depadding
	if err := s.fcManager.DataReceived(payloadLen); err != nil {
		s.mu.Unlock()
		s.conn.log.Error("Stream flow control error on DATA frame",
			logger.LogFields{"stream_id": s.id, "payload_len": payloadLen, "error": err.Error()})
		// This error from sfcm.DataReceived should be a *StreamError with ErrCodeFlowControlError
		// The connection will then RST the stream.
		return err
	}

	// Check stream state again under lock before writing to pipe, as it might have been closed concurrently.
	if s.state == StreamStateHalfClosedRemote || s.state == StreamStateClosed {
		s.mu.Unlock()
		s.conn.log.Warn("DATA frame received on stream that is already half-closed (remote) or closed",
			logger.LogFields{"stream_id": s.id, "state": s.state.String()})
		// The peer shouldn't send DATA in these states. Already handled by dispatchDataFrames,
		// but this is a defensive check. If reached, it means dispatchDataFrames's check was racy.
		// Send RST_STREAM with STREAM_CLOSED.
		return NewStreamError(s.id, ErrCodeStreamClosed, "DATA frame on closed/half-closed-remote stream")
	}

	s.mu.Unlock() // Unlock before writing to pipe, which can block.

	// 2. Pass data to handler via requestBodyWriter
	// If payloadLen is 0 and END_STREAM is not set, this is a no-op data-wise.
	// If payloadLen is 0 and END_STREAM is set, this effectively just signals EOF.
	if payloadLen > 0 {
		if _, err := s.requestBodyWriter.Write(frame.Data); err != nil {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.conn.log.Error("Error writing to stream requestBodyWriter",
				logger.LogFields{"stream_id": s.id, "error": err.Error()})
			// This typically means the reader (handler) side of the pipe was closed.
			// We should RST the stream because we can't process the data.
			// ErrCodeInternalError or ErrCodeCancel depending on who closed the pipe.
			// If the handler intentionally closed it, it might be okay, but if data is still incoming,
			// it's an issue. RST_STREAM with CANCEL is appropriate if handler signaled it's done.
			// If it's an unexpected pipe error, INTERNAL_ERROR.
			// For now, assume CANCEL as the handler might be done.
			_ = s.requestBodyWriter.CloseWithError(err) // Ensure writer is closed
			s._setState(StreamStateClosed)              // Force closed, resources will be cleaned.
			return NewStreamError(s.id, ErrCodeCancel, "failed to write to request body pipe: "+err.Error())
		}
	}

	s.mu.Lock() // Re-lock for END_STREAM processing and state update
	defer s.mu.Unlock()

	// 3. Handle END_STREAM flag
	if frame.Header().Flags&FlagDataEndStream != 0 {
		s.conn.log.Debug("END_STREAM received on DATA frame", logger.LogFields{"stream_id": s.id})
		s.endStreamReceivedFromClient = true
		// Close the writer end of the pipe to signal EOF to the reader (handler).
		if s.requestBodyWriter != nil {
			if err := s.requestBodyWriter.Close(); err != nil {
				s.conn.log.Warn("Error closing requestBodyWriter on END_STREAM", logger.LogFields{"stream_id": s.id, "error": err.Error()})
				// Not fatal for connection, handler might get a pipe error.
			}
		}

		// Determine new state based on current state and receiving END_STREAM
		if s.state == StreamStateOpen {
			s._setState(StreamStateHalfClosedRemote)
		} else if s.state == StreamStateHalfClosedLocal { // Server already sent END_STREAM
			s._setState(StreamStateClosed)
		} else {
			// This implies an invalid state for receiving END_STREAM (e.g., already closed, reserved).
			// The frame dispatch logic in conn.go should prevent this.
			// If it happens, it's a protocol violation or internal state error.
			// s.mu.Unlock() is not needed here because of the defer s.mu.Unlock() at the top of this locked section.
			s.conn.log.Error("END_STREAM received in unexpected stream state",
				logger.LogFields{"stream_id": s.id, "state": s.state.String()})
			return NewStreamError(s.id, ErrCodeProtocolError, // Or StreamClosed if state was already terminal
				fmt.Sprintf("END_STREAM received in unexpected state %s for stream %d", s.state.String(), s.id))
		}
	}

	// The problematic call to s.fcManager.SendWindowUpdateIfNeeded(payloadLen) has been removed.
	// The application layer will be responsible for signaling data consumption.
	return nil
}

// processRequestHeadersAndDispatch is called on the server side when a complete
// set of request headers is received for this stream. It constructs an http.Request
// and dispatches it via the provided router.

func (s *Stream) processRequestHeadersAndDispatch(headers []hpack.HeaderField, endStream bool, dispatcher func(sw StreamWriter, req *http.Request)) error {
	s.conn.log.Debug("Stream.processRequestHeadersAndDispatch: Entered", logger.LogFields{"stream_id": s.id, "num_headers_arg": len(headers), "end_stream_arg": endStream, "dispatcher_is_nil": dispatcher == nil})

	if dispatcher == nil {
		s.conn.log.Error("Stream.processRequestHeadersAndDispatch: Dispatcher is nil, cannot proceed.", logger.LogFields{"stream_id": s.id})
		_ = s.sendRSTStream(ErrCodeInternalError)
		return NewStreamError(s.id, ErrCodeInternalError, "dispatcher is nil")
	}

	s.mu.Lock()
	s.requestHeaders = headers
	s.mu.Unlock()

	pseudoMethod, pseudoPath, pseudoScheme, pseudoAuthority, pseudoErr := s.conn.extractPseudoHeaders(headers)
	if pseudoErr != nil {
		s.conn.log.Error("Failed to extract pseudo headers", logger.LogFields{"stream_id": s.id, "error": pseudoErr.Error()})
		_ = s.sendRSTStream(ErrCodeProtocolError)
		return pseudoErr
	}

	reqURL := &url.URL{
		Scheme: pseudoScheme,
		Host:   pseudoAuthority,
		Path:   pseudoPath,
	}
	if idx := strings.Index(pseudoPath, "?"); idx != -1 {
		reqURL.Path = pseudoPath[:idx]
		reqURL.RawQuery = pseudoPath[idx+1:]
	}

	httpHeaders := make(http.Header)
	for _, hf := range headers {
		if !strings.HasPrefix(hf.Name, ":") {
			httpHeaders.Add(hf.Name, hf.Value)
		}
	}

	req := &http.Request{
		Method:     pseudoMethod,
		URL:        reqURL,
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
		ProtoMinor: 0,
		Header:     httpHeaders,
		Body:       s.requestBodyReader,
		Host:       pseudoAuthority,
		RemoteAddr: s.conn.remoteAddrStr,
		RequestURI: pseudoPath,
	}
	req = req.WithContext(s.ctx)

	s.conn.log.Debug("Stream: Dispatching request to handler", logger.LogFields{"stream_id": s.id, "method": req.Method, "uri": req.RequestURI})

	s.conn.log.Debug("Stream: PRE-DISPATCH GOROUTINE SPAWN", logger.LogFields{"stream_id": s.id})
	go func() {
		s.conn.log.Debug("Stream: INSIDE DISPATCH GOROUTINE - PRE-PANIC-DEFER", logger.LogFields{"stream_id": s.id})
		defer func() {
			s.conn.log.Debug("Stream: INSIDE DISPATCH GOROUTINE - PANIC-DEFER EXECUTING", logger.LogFields{"stream_id": s.id})
			if r := recover(); r != nil {
				s.conn.log.Error("Panic in stream handler goroutine", logger.LogFields{
					"stream_id": s.id,
					"panic_val": fmt.Sprintf("%v", r), // Use fmt.Sprintf for potentially complex panic values
					"stack":     string(debug.Stack()),
				})
				// Try to send RST_STREAM if the stream/connection might still be partly alive.
				// This is a best-effort.
				_ = s.sendRSTStream(ErrCodeInternalError)
			}
			s.conn.log.Debug("Stream: INSIDE DISPATCH GOROUTINE - PRE-HANDLER-DONE", logger.LogFields{"stream_id": s.id})
			s.conn.streamHandlerDone(s)
			s.conn.log.Debug("Stream: INSIDE DISPATCH GOROUTINE - POST-HANDLER-DONE", logger.LogFields{"stream_id": s.id})
		}()
		s.conn.log.Debug("Stream: INSIDE DISPATCH GOROUTINE - PRE-DISPATCHER-CALL", logger.LogFields{"stream_id": s.id})
		dispatcher(s, req)
		s.conn.log.Debug("Stream: INSIDE DISPATCH GOROUTINE - POST-DISPATCHER-CALL", logger.LogFields{"stream_id": s.id})
	}()
	s.conn.log.Debug("Stream: POST-DISPATCH GOROUTINE SPAWN", logger.LogFields{"stream_id": s.id})
	return nil
}

// ID returns the stream's identifier.
// This is used to satisfy server.ResponseWriterStream.
func (s *Stream) ID() uint32 {
	s.mu.RLock() // Ensure thread-safe access to id, though id is usually immutable after creation.
	defer s.mu.RUnlock()
	return s.id
}

// http2HeaderFieldsToHpackHeaderFields converts []http2.HeaderField to []hpack.HeaderField.
func http2HeaderFieldsToHpackHeaderFields(http2Headers []HeaderField) []hpack.HeaderField {
	if http2Headers == nil {
		return nil
	}
	hpackHeaders := make([]hpack.HeaderField, len(http2Headers))
	for i, hf := range http2Headers {
		hpackHeaders[i] = hpack.HeaderField{Name: hf.Name, Value: hf.Value /* Sensitive not mapped */}
	}
	return hpackHeaders
}

// serverToServerHpackHeaders is a temporary placeholder for http2HeaderFieldsToHpackHeaderFields.
// This ensures that if the old name is still lingering (e.g. due to search/replace error),
// it still calls the correct function. Eventually, all calls should be to http2HeaderFieldsToHpackHeaderFields.
// For now, let's make it a direct alias to avoid an intermediate step if the replacement above worked.
// func serverToServerHpackHeaders(http2Headers []HeaderField) []hpack.HeaderField {
// 	return http2HeaderFieldsToHpackHeaderFields(http2Headers)
// }

// http2ToHpackHeaders converts []HeaderField (from http2 package) to []hpack.HeaderField.
func http2ToHpackHeaders(http2Headers []HeaderField) []hpack.HeaderField {
	if http2Headers == nil {
		return nil
	}
	hpackHeaders := make([]hpack.HeaderField, len(http2Headers))
	for i, hf := range http2Headers {
		// http2.HeaderField has Name, Value. hpack.HeaderField has Name, Value, Sensitive.
		// Assuming Sensitive defaults to false or is not relevant for this conversion path.
		hpackHeaders[i] = hpack.HeaderField{Name: hf.Name, Value: hf.Value}
	}
	return hpackHeaders
}

// handleRSTStreamFrame processes an RST_STREAM frame received from the peer.
// It transitions the stream to the Closed state and ensures resources are cleaned up.
func (s *Stream) handleRSTStreamFrame(errorCode ErrorCode) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == StreamStateClosed {
		// If already closed, nothing more to do.
		// Log if the RST code is different, might indicate a race or peer sending multiple RSTs.
		if s.pendingRSTCode != nil && *s.pendingRSTCode != errorCode {
			s.conn.log.Debug("Stream already closed, received another RST_STREAM with different code",
				logger.LogFields{"stream_id": s.id, "original_rst_code": s.pendingRSTCode.String(), "new_rst_code": errorCode.String()})
		}
		return
	}

	s.conn.log.Info("Received RST_STREAM from peer",
		logger.LogFields{
			"stream_id":  s.id,
			"error_code": errorCode.String(),
			"old_state":  s.state.String(),
		})

	// Mark that an RST was received and what the code was.
	// This is important for closeStreamResourcesProtected to potentially use in error propagation.
	s.pendingRSTCode = &errorCode

	// Transition to closed state. This will trigger resource cleanup.
	s._setState(StreamStateClosed)

	// Note: No RST_STREAM is sent back in response to a received RST_STREAM.
	// The stream is now considered terminated.
	// Cleanup of pipes, context, fcManager, and removal from conn.streams/priorityTree
	// is handled by _setState(StreamStateClosed) -> closeStreamResourcesProtected -> conn.removeClosedStream.
}

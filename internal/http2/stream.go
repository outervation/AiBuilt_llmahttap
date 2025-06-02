package http2

import (
	"context"

	"io"
	"net/http" // For http.Header, http.Request
	"net/url"  // For url.URL

	"runtime/debug" // For stack traces in panic recovery
	"strings"       // For strings.Index and strings.HasPrefix

	"strconv" // For parsing content-length

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
	requestBodyReader *io.PipeReader // For handler to read incoming DATA frames
	requestBodyWriter *io.PipeWriter // For stream to write incoming DATA frames into

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

	initialHeadersProcessed bool                // True once the first HEADERS frame (request/response) has been processed
	requestTrailers         []hpack.HeaderField // Stores received trailer headers, if any
	parsedContentLength     *int64              // Parsed Content-Length from request headers
	receivedDataBytes       uint64              // Accumulates received DATA frame payload sizes
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
		initialHeadersProcessed:     false,
		requestTrailers:             nil,
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
	s.conn.log.Debug("Stream.SendHeaders: Entered", logger.LogFields{
		"stream_id":      s.id,
		"num_headers":    len(headers),
		"end_stream_arg": endStream,
	})

	s.mu.Lock()
	// defer s.mu.Unlock() // Defer moved to ensure it covers all paths correctly

	if s.responseHeadersSent {
		s.conn.log.Error("Stream: SendHeaders called after headers already sent", logger.LogFields{"stream_id": s.id})
		s.mu.Unlock()
		return NewStreamError(s.id, ErrCodeInternalError, "headers already sent")
	}
	if s.state == StreamStateClosed || s.pendingRSTCode != nil {
		s.conn.log.Warn("Stream: SendHeaders called on closed or resetting stream", logger.LogFields{"stream_id": s.id, "state": s.state.String()})
		s.mu.Unlock()
		return NewStreamError(s.id, ErrCodeStreamClosed, "stream closed or resetting")
	}
	if s.state == StreamStateHalfClosedLocal { // We've already sent END_STREAM
		s.conn.log.Error("Stream: SendHeaders called on half-closed (local) stream where END_STREAM was already sent", logger.LogFields{"stream_id": s.id})
		s.mu.Unlock()
		return NewStreamError(s.id, ErrCodeStreamClosed, "cannot send headers on half-closed (local) stream after END_STREAM")
	}

	// Convert to hpack.HeaderField and lowercase names
	hpackHeaders := make([]hpack.HeaderField, len(headers))
	statusCode := ""
	for i, hf := range headers {
		lowerName := strings.ToLower(hf.Name)
		hpackHeaders[i] = hpack.HeaderField{Name: lowerName, Value: hf.Value}
		if lowerName == ":status" {
			statusCode = hf.Value
		}
	}

	s.conn.log.Debug("Stream.SendHeaders: Prepared HPACK headers", logger.LogFields{
		"stream_id":   s.id,
		"status_code": statusCode,
		"num_headers": len(hpackHeaders),
		"end_stream":  endStream,
	})
	s.mu.Unlock() // Unlock before calling conn.sendHeadersFrame which might block or call back

	s.conn.log.Debug("Stream: PRE-CALL conn.sendHeadersFrame", logger.LogFields{"stream_id": s.id, "num_headers": len(hpackHeaders), "end_stream": endStream, "s.conn_nil": s.conn == nil, "s.conn_log_nil": s.conn != nil && s.conn.log == nil})
	err := s.conn.sendHeadersFrame(s, hpackHeaders, endStream)

	s.mu.Lock()         // Re-lock for state updates
	defer s.mu.Unlock() // Ensure unlock for all subsequent paths

	if err != nil {
		s.conn.log.Error("Stream: conn.sendHeadersFrame failed", logger.LogFields{"stream_id": s.id, "error": err})
		return err
	}

	s.responseHeadersSent = true
	s.conn.log.Debug("Stream.SendHeaders: responseHeadersSent set to true.", logger.LogFields{"stream_id": s.id, "status_code_sent": statusCode})

	if endStream {
		if !s.endStreamSentToClient { // Check before setting, to avoid issues if called multiple times with endStream=true (though guarded by responseHeadersSent)
			s.endStreamSentToClient = true
			s.conn.log.Debug("Stream.SendHeaders: endStreamSentToClient set to true. Transitioning state.", logger.LogFields{"stream_id": s.id, "current_state_before_transition": s.state.String()})
			s.transitionStateOnSendEndStream()
			s.conn.log.Debug("Stream.SendHeaders: State after transition.", logger.LogFields{"stream_id": s.id, "new_state": s.state.String()})
		} else {
			s.conn.log.Debug("Stream.SendHeaders: endStream=true in arg, but endStreamSentToClient was already true.", logger.LogFields{"stream_id": s.id})
		}
	} else {
		s.conn.log.Debug("Stream.SendHeaders: endStream=false in arg, no state transition for stream end here.", logger.LogFields{"stream_id": s.id})
	}
	s.conn.log.Debug("Stream.SendHeaders: Exiting successfully.", logger.LogFields{"stream_id": s.id, "final_state": s.state.String(), "final_endStreamSentToClient": s.endStreamSentToClient})
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
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.responseHeadersSent {
		return NewStreamError(s.id, ErrCodeInternalError, "cannot send trailers before headers")
	}
	if s.endStreamSentToClient {
		return NewStreamError(s.id, ErrCodeStreamClosed, "cannot send trailers after stream already ended")
	}
	if s.state == StreamStateClosed || s.pendingRSTCode != nil {
		return NewStreamError(s.id, ErrCodeStreamClosed, "stream closed or resetting")
	}

	// Convert to hpack.HeaderField and lowercase names
	hpackTrailers := make([]hpack.HeaderField, len(trailers))
	for i, hf := range trailers {
		hpackTrailers[i] = hpack.HeaderField{Name: strings.ToLower(hf.Name), Value: hf.Value}
	}

	// Trailers imply end of stream.
	// Use a HEADERS frame with END_STREAM set.
	err := s.conn.sendHeadersFrame(s, hpackTrailers, true) // true for endStream
	if err != nil {
		s.conn.log.Error("Stream: conn.sendHeadersFrame for trailers failed", logger.LogFields{"stream_id": s.id, "error": err})
		return err
	}

	s.endStreamSentToClient = true     // Set this flag after successful queuing
	s.transitionStateOnSendEndStream() // Mark stream as ended from our side
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
	s.mu.Lock()
	defer s.mu.Unlock()

	// RFC 7540, 6.1: DATA frames MUST be associated with a stream. If a DATA frame is received whose stream identifier field is 0x0,
	// the recipient MUST respond with a connection error (Section 5.4.1) of type PROTOCOL_ERROR.
	// This check should ideally be done at the connection level before dispatching to stream.
	// Assuming stream ID is valid by the time it reaches here.

	// RFC 7540, 5.1: DATA frames can only be sent on streams in "open" or "half-closed (local)" state.
	// Receiving DATA on "half-closed (remote)" or "closed" is a stream error.
	if s.state == StreamStateHalfClosedRemote || s.state == StreamStateClosed {
		msg := fmt.Sprintf("DATA frame on closed/half-closed-remote stream %d", s.id)
		s.conn.log.Warn(msg, logger.LogFields{"stream_id": s.id, "state": s.state.String()})
		// This error will lead to an RST_STREAM being sent by the connection management logic
		// after this function returns an error.
		// The stream should transition to closed if not already.
		// s._setState(StreamStateClosed) // _setState is called by conn logic after error
		return NewStreamError(s.id, ErrCodeStreamClosed, msg)
	}

	// Account for data with flow control
	// Connection-level flow control is handled by the connection before calling this.
	// Stream-level flow control:
	dataLen := uint32(len(frame.Data))
	if err := s.fcManager.DataReceived(dataLen); err != nil {
		// Flow control violation
		s.conn.log.Error("Stream flow control error on receive", logger.LogFields{
			"stream_id": s.id,
			"data_len":  dataLen,
			"window":    s.fcManager.GetStreamReceiveAvailable(), // Window after error (might be negative)
			"error":     err.Error(),
		})
		// s._setState(StreamStateClosed) // _setState is called by conn logic after error
		return err // err should be a *StreamError with ErrCodeFlowControlError
	}

	// Account for data with flow control (already done before this block)

	// dataLen is already declared: uint32(len(frame.Data))

	// If END_STREAM is set, we need to perform the content-length check *before* writing to the pipe,
	// as a mismatch should lead to an error without processing the data further (like writing to pipe).
	if frame.Header().Flags&FlagDataEndStream != 0 {
		s.conn.log.Debug("END_STREAM received on DATA frame. Performing pre-pipe-write checks.", logger.LogFields{"stream_id": s.id})
		s.endStreamReceivedFromClient = true

		// Conceptual total received if this frame is included
		potentialTotalReceived := s.receivedDataBytes + uint64(dataLen)

		if s.parsedContentLength != nil {
			s.conn.log.Debug("Content-Length check (pre-pipe-write for END_STREAM)", logger.LogFields{
				"stream_id":                s.id,
				"parsedContentLength":      *s.parsedContentLength,
				"potentialTotalReceived":   potentialTotalReceived,
				"currentReceivedDataBytes": s.receivedDataBytes,
				"currentFrameDataLen":      dataLen,
			})
			if *s.parsedContentLength != int64(potentialTotalReceived) {
				msg := fmt.Sprintf("content-length mismatch on END_STREAM: declared %d, received %d (current frame %d bytes, previously %d bytes)",
					*s.parsedContentLength, potentialTotalReceived, dataLen, s.receivedDataBytes)
				s.conn.log.Warn(msg, logger.LogFields{"stream_id": s.id, "state": s.state.String()})
				// Do not write to pipe. Return error.
				return NewStreamError(s.id, ErrCodeProtocolError, msg)
			}
		}
	}

	// Write data to the pipe for the handler to read, if there's data.
	if dataLen > 0 {
		if _, err := s.requestBodyWriter.Write(frame.Data); err != nil {
			// This typically means the reader (handler) side of the pipe was closed.
			s.conn.log.Error("Error writing to stream requestBodyWriter", logger.LogFields{
				"stream_id": s.id,
				"error":     err.Error(),
			})
			return NewStreamError(s.id, ErrCodeCancel, fmt.Sprintf("pipe write failed for stream %d: %v", s.id, err))
		}
		s.receivedDataBytes += uint64(dataLen) // Update after successful write
		s.conn.log.Debug("Wrote DATA to pipe", logger.LogFields{"stream_id": s.id, "bytes": dataLen, "total_received_on_stream": s.receivedDataBytes})
	}

	// Handle END_STREAM flag actions (if no CL error occurred before for END_STREAM)
	if frame.Header().Flags&FlagDataEndStream != 0 {
		// s.endStreamReceivedFromClient was already set above if END_STREAM is present.
		s.conn.log.Debug("Processing END_STREAM post-pipe-write actions.", logger.LogFields{"stream_id": s.id})

		// Close the writer end of the pipe to signal EOF to the handler.
		if err := s.requestBodyWriter.Close(); err != nil {
			s.conn.log.Error("Error closing requestBodyWriter after END_STREAM (post-CL-check)", logger.LogFields{
				"stream_id": s.id,
				"error":     err.Error(),
			})
			return NewStreamError(s.id, ErrCodeInternalError, fmt.Sprintf("pipe close failed for stream %d: %v", s.id, err))
		}

		// Transition stream state
		switch s.state {
		case StreamStateOpen:
			s._setState(StreamStateHalfClosedRemote)
		case StreamStateHalfClosedLocal:
			s._setState(StreamStateClosed)
		default:
			s.conn.log.Error("Received END_STREAM on DATA frame in unexpected stream state (post-CL-check)", logger.LogFields{
				"stream_id": s.id,
				"state":     s.state.String(),
			})
			return NewStreamError(s.id, ErrCodeProtocolError, fmt.Sprintf("END_STREAM on DATA in unexpected state %s for stream %d", s.state, s.id))
		}
	}
	return nil
}

// processRequestHeadersAndDispatch is called on the server side when a complete
// set of request headers is received for this stream. It constructs an http.Request
// and dispatches it via the provided router.

func (s *Stream) processRequestHeadersAndDispatch(headers []hpack.HeaderField, endStream bool, dispatcher func(sw StreamWriter, req *http.Request)) error {

	s.mu.RLock()
	currentState := s.state
	// Capture the state of s.initialHeadersProcessed *before* this header block potentially modifies it.
	hasInitialHeadersBeenProcessed := s.initialHeadersProcessed
	s.mu.RUnlock()

	if currentState == StreamStateClosed {
		s.conn.log.Warn("Stream.processRequestHeadersAndDispatch: Attempt to process headers on an already closed stream.", logger.LogFields{"stream_id": s.id})
		// As per h2spec 5.1 Stream States #9 (closed: Sends a HEADERS frame after sending RST_STREAM frame)
		// "The endpoint MUST treat this as a stream error of type STREAM_CLOSED."
		if err := s.sendRSTStream(ErrCodeStreamClosed); err != nil {
			s.conn.log.Error("Stream.processRequestHeadersAndDispatch: Failed to send RST_STREAM for closed stream.", logger.LogFields{"stream_id": s.id, "error": err})
			return err // Propagate error if sending RST_STREAM fails
		}
		return nil // Stream error handled by RST
	}

	// NEW CHECK for h2spec 8.1/1 (Task Item 3.b)
	// If a second HEADERS frame is received for a stream that is currently in StreamStateOpen
	// (i.e., the first HEADERS frame for the request did not have END_STREAM set, which is implied by StreamStateOpen),
	// and this second HEADERS frame also does not have END_STREAM set, it MUST be treated as a stream error of type PROTOCOL_ERROR.
	// `hasInitialHeadersBeenProcessed` captures if this is a "second" (or subsequent) HEADERS block.
	if hasInitialHeadersBeenProcessed && currentState == StreamStateOpen && !endStream {
		s.conn.log.Warn("Stream: Subsequent HEADERS frame received on Open stream without END_STREAM flag (violates h2spec 8.1/1).",
			logger.LogFields{"stream_id": s.id, "state": currentState.String()})
		if rstErr := s.sendRSTStream(ErrCodeProtocolError); rstErr != nil {
			return rstErr // Propagate error if sending RST fails
		}
		return nil // RST_STREAM sent, stream error handled.
	}

	s.conn.log.Debug("Stream.processRequestHeadersAndDispatch: Entered (after h2spec 8.1/1 check)", logger.LogFields{"stream_id": s.id, "num_headers_arg": len(headers), "end_stream_arg": endStream, "dispatcher_is_nil": dispatcher == nil})

	if dispatcher == nil {
		s.conn.log.Error("Stream.processRequestHeadersAndDispatch: Dispatcher is nil, cannot proceed.", logger.LogFields{"stream_id": s.id})
		// Attempt to RST the stream. If this fails, the error will propagate up from Close.
		// This is an internal server error, so the connection might be torn down by the caller of Serve.
		_ = s.Close(NewStreamError(s.id, ErrCodeInternalError, "dispatcher is nil"))
		return NewStreamError(s.id, ErrCodeInternalError, "dispatcher is nil") // Signal error to caller
	}

	// --- BEGIN Original Header Validations (Task Items 1a, 1b, 1c, 1d) ---
	// `isPotentiallyTrailers` uses `hasInitialHeadersBeenProcessed` which was captured at the start of this function.
	isPotentiallyTrailers := hasInitialHeadersBeenProcessed && endStream

	for _, hf := range headers { // 'headers' are the full hpack.HeaderFields for this block
		// 1.a Uppercase header field names (h2spec 8.1.2 #1)
		// HTTP/2 header field names MUST be lowercase.
		for _, char := range hf.Name {
			if char >= 'A' && char <= 'Z' {
				errMsg := fmt.Sprintf("invalid header field name '%s' contains uppercase characters", hf.Name)
				s.conn.log.Error(errMsg, logger.LogFields{"stream_id": s.id, "header_name": hf.Name})
				if sendErr := s.sendRSTStream(ErrCodeProtocolError); sendErr != nil {
					return sendErr // Propagate connection error if RST send failed
				}
				return NewStreamError(s.id, ErrCodeProtocolError, errMsg) // Return StreamError
			}
		}

		// Use ToLower for switch consistency. Note that the check above is for ANY uppercase.
		lowerName := strings.ToLower(hf.Name)

		// 1.b Forbidden connection-specific headers (h2spec 8.1.2.2 #1)
		// HTTP/2 RFC 7540, Section 8.1.2.2.
		switch lowerName {
		case "connection", "proxy-connection", "keep-alive", "upgrade", "transfer-encoding":
			// Note: "transfer-encoding" is listed as connection-specific and forbidden.
			// The TE header field is an exception but handled separately.
			errMsg := fmt.Sprintf("connection-specific header field '%s' is forbidden in HTTP/2", hf.Name)
			s.conn.log.Error(errMsg, logger.LogFields{"stream_id": s.id, "header_name": hf.Name})
			if sendErr := s.sendRSTStream(ErrCodeProtocolError); sendErr != nil {
				return sendErr
			}
			return NewStreamError(s.id, ErrCodeProtocolError, errMsg) // Return StreamError
		}

		// 1.c TE header validation (h2spec 8.1.2.2 #2)
		if lowerName == "te" {
			if strings.ToLower(hf.Value) != "trailers" {
				errMsg := fmt.Sprintf("TE header field contains invalid value '%s', must be 'trailers'", hf.Value)
				s.conn.log.Error(errMsg, logger.LogFields{"stream_id": s.id, "header_value": hf.Value})
				if sendErr := s.sendRSTStream(ErrCodeProtocolError); sendErr != nil {
					return sendErr
				}
				return NewStreamError(s.id, ErrCodeProtocolError, errMsg) // Return StreamError
			}
		}

		// 1.d Pseudo-headers in trailers (h2spec 8.1.2.1 #3)
		// A HEADERS frame carrying trailers MUST NOT contain pseudo-header fields.
		if isPotentiallyTrailers && strings.HasPrefix(hf.Name, ":") {
			errMsg := fmt.Sprintf("pseudo-header field '%s' found in trailer block", hf.Name)
			s.conn.log.Error(errMsg, logger.LogFields{"stream_id": s.id, "header_name": hf.Name})
			if rstSendErr := s.sendRSTStream(ErrCodeProtocolError); rstSendErr != nil {
				s.conn.log.Error("Failed to send RST_STREAM for pseudo-header in trailers violation (stream.go)", logger.LogFields{"stream_id": s.id, "error": rstSendErr.Error()})
				return rstSendErr
			}
			// The spec (RFC 7540, 8.1.2.1) says "Endpoints MUST treat a request or response that contains a pseudo-header field in a trailer section as malformed (Section 8.1.2.6)."
			// Section 8.1.2.6 says "A request or response is also malformed if the value of a content-length header field does not equal the sum of the DATA frame payload lengths that form the body. A response that is defined as malformed is treated as a stream error (Section 5.4.2) of type PROTOCOL_ERROR."
			// So, this should be a stream error.
			return NewStreamError(s.id, ErrCodeProtocolError, errMsg)
		}
	}
	// --- END Original Header Validations ---

	var clHeaderValue string
	var clHeaderFound bool
	for _, hf := range headers {
		// Use ToLower for case-insensitive match, as per HTTP semantics.
		if strings.ToLower(hf.Name) == "content-length" {
			if clHeaderFound { // Duplicate content-length header
				s.conn.log.Error("Duplicate content-length header found", logger.LogFields{"stream_id": s.id, "value1": clHeaderValue, "value2": hf.Value})
				if sendErr := s.sendRSTStream(ErrCodeProtocolError); sendErr != nil {
					return sendErr
				}
				return nil // Stream error handled by RST
			}
			clHeaderValue = hf.Value
			clHeaderFound = true
		}
	}

	var parsedCL *int64
	if clHeaderFound {
		val, err := strconv.ParseInt(clHeaderValue, 10, 64)
		if err != nil || val < 0 { // Invalid format or negative value
			s.conn.log.Error("Invalid content-length header value", logger.LogFields{"stream_id": s.id, "value": clHeaderValue, "parse_error": err})
			if sendErr := s.sendRSTStream(ErrCodeProtocolError); sendErr != nil {
				return sendErr
			}
			return nil // Stream error handled by RST
		}
		parsedCL = &val
	}

	s.mu.Lock() // Lock for s.requestHeaders, s.parsedContentLength, s.initialHeadersProcessed

	// Task: Content-length validation if END_STREAM on HEADERS (h2spec 8.1.2.6 #1)
	// If END_STREAM is on HEADERS, payload length must be zero.
	// If content-length is present and non-zero, it's a PROTOCOL_ERROR.
	// 'endStream' is an argument to processRequestHeadersAndDispatch reflecting the flag on the current HEADERS frame.
	if endStream {
		// 'parsedCL' is the local variable holding the parsed content length value from headers.
		if parsedCL != nil && *parsedCL != 0 {
			errMsg := fmt.Sprintf("HEADERS frame with END_STREAM set and non-zero content-length (%d)", *parsedCL)
			s.conn.log.Error(errMsg, logger.LogFields{"stream_id": s.id})
			s.mu.Unlock() // Unlock before sendRSTStream
			if sendErr := s.sendRSTStream(ErrCodeProtocolError); sendErr != nil {
				return sendErr
			}
			return nil // Stream error handled by RST
		}
	}
	s.requestHeaders = headers
	s.parsedContentLength = parsedCL // Store the parsed content length (or nil if not present)

	if !s.initialHeadersProcessed {
		s.initialHeadersProcessed = true
	}
	s.mu.Unlock() // Unlock after modifying stream fields

	// 'headers' arg to extractPseudoHeaders is the full list from hpackAdapter.
	// It is expected to validate pseudo-header presence, order, and specific values (like :path).
	// If extractPseudoHeaders finds an error, it currently returns a ConnectionError.
	// This needs to be reconciled: pseudo-header errors are usually stream errors.
	pseudoMethod, pseudoPath, pseudoScheme, pseudoAuthority, pseudoErr := s.conn.extractPseudoHeaders(headers)
	if pseudoErr != nil {
		s.conn.log.Error("Failed to extract/validate pseudo headers", logger.LogFields{"stream_id": s.id, "error": pseudoErr.Error()})
		// Send RST_STREAM first for the stream error
		if rstSendErr := s.sendRSTStream(ErrCodeProtocolError); rstSendErr != nil {
			// If sending RST_STREAM fails, this is a more severe issue.
			// Propagate the error from sending RST_STREAM.
			return NewConnectionErrorWithCause(ErrCodeInternalError, "failed to send RST_STREAM for pseudo-header error, then failed to return ConnectionError: "+rstSendErr.Error(), rstSendErr)
		}
		// Then, return a ConnectionError to signal the connection should be torn down with GOAWAY.
		// The original pseudoErr might be a fmt.Errorf, so wrap it.
		return NewConnectionErrorWithCause(ErrCodeProtocolError, "pseudo-header validation failed: "+pseudoErr.Error(), pseudoErr)
	}

	reqURL := &url.URL{
		Scheme: pseudoScheme,
		Host:   pseudoAuthority,
		Path:   pseudoPath, // pseudoPath already contains path and query
	}
	// The pseudoPath should already be correctly formed by extractPseudoHeaders
	// (e.g. path and query separation is implicitly handled by net/url.Parse or RequestURI)
	// If pseudoPath contains '?', url.Parse (which http.NewRequest uses implicitly) will split it.
	// For direct construction:
	if idx := strings.Index(pseudoPath, "?"); idx != -1 {
		reqURL.Path = pseudoPath[:idx]
		reqURL.RawQuery = pseudoPath[idx+1:]
	} else {
		reqURL.Path = pseudoPath // No query string
	}

	httpHeaders := make(http.Header)
	for _, hf := range headers {
		if !strings.HasPrefix(hf.Name, ":") { // Filter out pseudo-headers
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
		Body:       s.requestBodyReader, // Reader part of the pipe
		Host:       pseudoAuthority,
		RemoteAddr: s.conn.remoteAddrStr, // From connection
		RequestURI: pseudoPath,           // Full path + query from :path
	}
	if parsedCL != nil { // Set ContentLength on http.Request if parsed
		req.ContentLength = *parsedCL
	} else if clHeaderFound { // If header was present but couldn't parse (should have been caught above)
		req.ContentLength = -1 // Indicate unknown or problematic
	}

	req = req.WithContext(s.ctx) // Associate stream's context with the request

	s.conn.log.Debug("Stream: Dispatching request to handler", logger.LogFields{"stream_id": s.id, "method": req.Method, "uri": req.RequestURI})

	// Spawn a new goroutine for each request handler.
	// This allows multiplexing: the connection's reader loop can continue processing
	// frames for other streams while this handler runs.
	s.conn.log.Debug("Stream: PRE-DISPATCH GOROUTINE SPAWN", logger.LogFields{"stream_id": s.id})
	go func() {
		s.conn.log.Debug("Stream: INSIDE DISPATCH GOROUTINE - PRE-PANIC-DEFER", logger.LogFields{"stream_id": s.id})
		defer func() {
			s.conn.log.Debug("Stream: INSIDE DISPATCH GOROUTINE - PANIC-DEFER EXECUTING", logger.LogFields{"stream_id": s.id})
			if r := recover(); r != nil {
				s.conn.log.Error("Panic in stream handler goroutine", logger.LogFields{
					"stream_id": s.id,
					"panic_val": fmt.Sprintf("%v", r),
					"stack":     string(debug.Stack()),
				})
				// Attempt to RST_STREAM the stream if a panic occurs in the handler.
				// This is a best-effort; if the connection is also failing, it might not succeed.
				_ = s.sendRSTStream(ErrCodeInternalError)
			}
			// Signal that this handler has finished its processing for this stream.
			// This might be used by the connection to manage active handler counts for graceful shutdown.
			s.conn.log.Debug("Stream: INSIDE DISPATCH GOROUTINE - PRE-HANDLER-DONE", logger.LogFields{"stream_id": s.id})
			s.conn.streamHandlerDone(s) // Ensure this method exists and is meaningful on Connection
			s.conn.log.Debug("Stream: INSIDE DISPATCH GOROUTINE - POST-HANDLER-DONE", logger.LogFields{"stream_id": s.id})
		}()
		s.conn.log.Debug("Stream: INSIDE DISPATCH GOROUTINE - PRE-DISPATCHER-CALL", logger.LogFields{"stream_id": s.id})
		dispatcher(s, req) // Call the actual application handler
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

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

// requestBodyConsumedNotifier wraps an io.ReadCloser (the request body pipe reader)
// and calls the stream's flow control manager to potentially send WINDOW_UPDATE
// frames after data is successfully read.
type requestBodyConsumedNotifier struct {
	stream *Stream // Points back to the stream to access its fcManager
	reader io.ReadCloser
}

// Read reads from the underlying reader and then notifies the flow control manager
// about the consumed bytes.
func (rbcn *requestBodyConsumedNotifier) Read(p []byte) (n int, err error) {
	n, err = rbcn.reader.Read(p)
	if n > 0 {
		// Notify the stream's flow control manager that 'n' bytes have been consumed by the application.
		// This might trigger sending a WINDOW_UPDATE frame for the stream.
		if notifierErr := rbcn.stream.fcManager.SendWindowUpdateIfNeeded(uint32(n)); notifierErr != nil {
			// Log the error, but the primary error from Read should be returned to the handler.
			// This error could be serious (e.g., failed to queue WINDOW_UPDATE).
			rbcn.stream.conn.log.Error("Error from SendWindowUpdateIfNeeded after request body read", logger.LogFields{
				"stream_id":      rbcn.stream.id,
				"bytes_read":     n,
				"notifier_error": notifierErr.Error(),
			})
			// Decide if this error should supersede the Read error.
			// If Read itself errored (e.g. io.EOF), that's more important for the handler.
			// If Read was successful but notifier failed, it's a background issue.
			// For now, just log it. If Read had no error, maybe return this one?
			// Standard library http.Request.Body readers don't typically surface such errors.
		}
	}
	// Also need to notify connection-level flow control
	if n > 0 && rbcn.stream != nil && rbcn.stream.conn != nil && rbcn.stream.conn.connFCManager != nil {
		connIncrement, connErr := rbcn.stream.conn.connFCManager.ApplicationConsumedData(uint32(n))
		if connErr != nil {
			rbcn.stream.conn.log.Error("Error from ConnectionFCManager.ApplicationConsumedData", logger.LogFields{
				"stream_id":     rbcn.stream.id, // Log stream_id for context
				"bytes_read":    n,
				"conn_fc_error": connErr.Error(),
			})
		} else if connIncrement > 0 {
			if sendErr := rbcn.stream.conn.sendWindowUpdateFrame(0, connIncrement); sendErr != nil {
				rbcn.stream.conn.log.Error("Failed to send connection-level WINDOW_UPDATE frame", logger.LogFields{
					"stream_id": 0, // Connection level
					"increment": connIncrement,
					"error":     sendErr.Error(),
				})
			}
		}
	}

	return n, err
}

// Close closes the underlying reader.
func (rbcn *requestBodyConsumedNotifier) Close() error {
	return rbcn.reader.Close()
}

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
	requestBodyReader *io.PipeReader  // For handler to read incoming DATA frames
	requestBodyWriter *io.PipeWriter  // For stream to write incoming DATA frames into
	dataFrameChan     chan *DataFrame // Channel for queueing incoming DATA frames for async processing

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
	receivedDataBytes       uint64              // Accumulates received DATA frame payload sizes (on the bodyWriter goroutine)
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
		dataFrameChan:               make(chan *DataFrame, 32), // Buffered channel for DATA frames
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
			// Don't try to close dataFrameChan here as bodyWriterLoop hasn't started.
			return nil, err
		}
	}

	go s.processIncomingDataFrames() // Start the goroutine to handle data frames for this stream

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
	s.mu.Lock() // Lock to check state and prevent races on pendingRSTCode
	// If already definitively closed and RST_STREAM processing initiated for this code, no-op.
	if s.state == StreamStateClosed && s.pendingRSTCode != nil && *s.pendingRSTCode == errorCode {
		s.mu.Unlock()
		return nil
	}
	// If it's closed but not due to our RST action, and we now want to RST, proceed.
	// If it's merely half-closed, we can still RST it.

	// If already closed for a *different* reason, or trying to RST an already generally closed stream,
	// it might be okay to just return nil or log, rather than re-sending RST,
	// but for now, if it's not specifically closed *for this error code*, we proceed.
	if s.state == StreamStateClosed {
		s.conn.log.Debug("sendRSTStream: Stream already closed, but not for this specific error code or pendingRSTCode was nil. Proceeding to mark.", logger.LogFields{"stream_id": s.id, "current_pending_rst": s.pendingRSTCode, "new_rst_code": errorCode})
		// Even if generally closed, if pendingRSTCode wasn't set for *this* code,
		// we update it and ensure resources are cleaned with this error context.
	}

	// Update state *before* sending the frame to make the stream locally aware of reset.
	s.pendingRSTCode = &errorCode
	s._setState(StreamStateClosed) // This calls closeStreamResourcesProtected
	s.mu.Unlock()                  // Unlock before calling conn.sendRSTStreamFrame which is potentially blocking

	err := s.conn.sendRSTStreamFrame(s.id, errorCode)

	if err != nil {
		// Failed to send RST_STREAM. The state is already 'Closed' locally.
		// Log the failure. The stream remains logically closed on our side.
		s.conn.log.Error("stream: failed to send RST_STREAM frame to peer (state already updated to Closed locally)",
			logger.LogFields{"stream_id": s.id, "error_code": errorCode.String(), "send_error": err.Error()})
		return err // Return the send error, but local state is already 'Closed'
	}

	// Successfully sent RST_STREAM frame (or queued it).
	// Stream is already marked as Closed and resources cleaned up by the _setState call above.
	s.conn.log.Debug("stream: RST_STREAM frame sent/queued successfully.", logger.LogFields{"stream_id": s.id, "error_code": errorCode.String()})
	return nil
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
	s.conn.log.Debug("closeStreamResourcesProtected: Entered", logger.LogFields{"stream_id": s.id, "current_state_in_func": s.state.String(), "pending_rst_code_at_entry": fmt.Sprintf("%v", s.pendingRSTCode)})

	// Order of operations is critical here:
	// 1. Close the requestBodyWriter pipe WITH THE INTENDED ERROR FIRST.
	//    This ensures that any handler blocked on reading the body receives the specific error
	//    rather than a generic context cancellation or premature EOF if other cleanup happens first.
	if s.requestBodyWriter != nil {
		var pipeCloseError error
		if s.pendingRSTCode != nil {
			pipeCloseError = NewStreamError(s.id, *s.pendingRSTCode, "stream reset")
			s.conn.log.Debug("closeStreamResourcesProtected: requestBodyWriter will be closed with RST error from s.pendingRSTCode", logger.LogFields{"stream_id": s.id, "rst_code_val": *s.pendingRSTCode, "error_to_pipe": pipeCloseError.Error()})
		} else {
			ctxErr := s.ctx.Err()
			if ctxErr != nil {
				pipeCloseError = ctxErr
				s.conn.log.Debug("closeStreamResourcesProtected: requestBodyWriter will be closed with context error (s.pendingRSTCode was nil)", logger.LogFields{"stream_id": s.id, "ctx_err": pipeCloseError})
			} else {
				pipeCloseError = io.EOF // Graceful close if no RST and context not done.
				s.conn.log.Debug("closeStreamResourcesProtected: requestBodyWriter will be closed with EOF (s.pendingRSTCode nil, s.ctx not done)", logger.LogFields{"stream_id": s.id})
			}
		}
		s.conn.log.Debug("closeStreamResourcesProtected: About to call s.requestBodyWriter.CloseWithError()", logger.LogFields{"stream_id": s.id, "pipe_close_error_type": fmt.Sprintf("%T", pipeCloseError), "pipe_close_error_val": pipeCloseError})
		errClosePipe := s.requestBodyWriter.CloseWithError(pipeCloseError)
		if errClosePipe != nil {
			s.conn.log.Warn("Error from s.requestBodyWriter.CloseWithError() call (possibly already closed or other issue)", logger.LogFields{"stream_id": s.id, "error": errClosePipe})
		} else {
			s.conn.log.Debug("closeStreamResourcesProtected: s.requestBodyWriter.CloseWithError() returned nil (success).", logger.LogFields{"stream_id": s.id})
		}
	} else {
		s.conn.log.Warn("closeStreamResourcesProtected: s.requestBodyWriter was nil, cannot close.", logger.LogFields{"stream_id": s.id})
	}

	// 2. Close dataFrameChan to signal processIncomingDataFrames to stop.
	//    Its defer will attempt to close requestBodyWriter again, which is fine (idempotent or will error harmlessly).
	if s.dataFrameChan != nil {
		s.conn.log.Debug("closeStreamResourcesProtected: Attempting to close dataFrameChan (after closing requestBodyWriter)", logger.LogFields{"stream_id": s.id})
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.conn.log.Debug("Recovered from panic closing dataFrameChan (likely already closed)", logger.LogFields{"stream_id": s.id, "panic_val": r})
				}
			}()
			close(s.dataFrameChan)
			s.conn.log.Debug("closeStreamResourcesProtected: dataFrameChan closed.", logger.LogFields{"stream_id": s.id})
		}()
	} else {
		s.conn.log.Debug("closeStreamResourcesProtected: s.dataFrameChan was nil or already processed.", logger.LogFields{"stream_id": s.id})
	}

	// 3. Cancel the stream's specific context.
	//    This signals any other operations using this stream's context to stop.
	if s.cancelCtx != nil {
		s.conn.log.Debug("closeStreamResourcesProtected: Calling s.cancelCtx() (after closing pipe & data chan).", logger.LogFields{"stream_id": s.id})
		s.cancelCtx()
	} else {
		s.conn.log.Warn("closeStreamResourcesProtected: s.cancelCtx was nil.", logger.LogFields{"stream_id": s.id})
	}

	// 4. Close flow control manager windows.
	if s.fcManager != nil {
		var fcCloseReason error
		if s.pendingRSTCode != nil {
			fcCloseReason = NewStreamError(s.id, *s.pendingRSTCode, "stream reset affecting flow control")
		} else {
			fcCloseReason = NewStreamError(s.id, ErrCodeStreamClosed, "stream closed affecting flow control")
		}
		s.fcManager.Close(fcCloseReason)
		s.conn.log.Debug("closeStreamResourcesProtected: fcManager closed", logger.LogFields{"stream_id": s.id})
	} else {
		s.conn.log.Warn("closeStreamResourcesProtected: s.fcManager was nil.", logger.LogFields{"stream_id": s.id})
	}

	// 5. Clear pendingRSTCode as it has been handled.
	s.pendingRSTCode = nil

	s.conn.log.Debug("closeStreamResourcesProtected: Exiting", logger.LogFields{"stream_id": s.id})
}

// handleDataFrame processes an incoming DATA frame for this stream.
// It's responsible for:
// 1. Stream-level flow control accounting.
// 2. Passing the data to the requestBodyWriter for the handler to consume.
// 3. Handling the END_STREAM flag.
// Returns a StreamError if stream-level flow control is violated or other stream-specific
// issues occur. Returns a ConnectionError if a problem warrants closing the connection.

func (s *Stream) handleDataFrame(frame *DataFrame) error {
	s.mu.RLock()
	currentState := s.state
	isPendingRST := s.pendingRSTCode != nil
	ctxErr := s.ctx.Err()
	s.conn.log.Debug("handleDataFrame: Entry state check", logger.LogFields{"stream_id": s.id, "state": currentState.String(), "pending_rst": isPendingRST, "end_stream_recv_client": s.endStreamReceivedFromClient, "ctx_err": ctxErr})
	s.mu.RUnlock()

	if currentState == StreamStateClosed || isPendingRST {
		msg := fmt.Sprintf("DATA frame on closed/resetting stream %d (state: %s, pendingRST: %v)", s.id, currentState, isPendingRST)
		s.conn.log.Warn(msg, logger.LogFields{"stream_id": s.id})
		return NewStreamError(s.id, ErrCodeStreamClosed, msg)
	}
	if ctxErr != nil {
		msg := fmt.Sprintf("DATA frame on stream %d with cancelled context: %v", s.id, ctxErr)
		s.conn.log.Warn(msg, logger.LogFields{"stream_id": s.id})
		return NewStreamError(s.id, ErrCodeCancel, msg) // Or StreamClosed if context implies that
	}

	dataLen := uint32(len(frame.Data))
	hasEndStreamFlag := frame.Header().Flags&FlagDataEndStream != 0

	s.mu.Lock() // Lock for state and fcManager access
	// Re-check state under full lock after initial RLock checks, as it might have changed.
	if s.state == StreamStateHalfClosedRemote || s.state == StreamStateClosed {
		s.mu.Unlock()
		msg := fmt.Sprintf("DATA frame on closed/half-closed-remote stream %d (re-check under lock)", s.id)
		s.conn.log.Warn(msg, logger.LogFields{"stream_id": s.id, "state": s.state.String()})
		return NewStreamError(s.id, ErrCodeStreamClosed, msg)
	}

	// Stream-level flow control (for received data)
	if errFc := s.fcManager.DataReceived(dataLen); errFc != nil {
		s.mu.Unlock()
		s.conn.log.Error("Stream flow control error on receive", logger.LogFields{
			"stream_id": s.id,
			"data_len":  dataLen,
			"window":    s.fcManager.GetStreamReceiveAvailable(),
			"error":     errFc.Error(),
		})
		return errFc // This is a StreamError, conn layer will RST
	}

	// Content-Length validation.
	if s.parsedContentLength != nil {
		clValue := *s.parsedContentLength
		potentialTotalReceivedThisFrameContext := s.receivedDataBytes + uint64(dataLen)

		if hasEndStreamFlag {
			if int64(potentialTotalReceivedThisFrameContext) != clValue {
				s.endStreamReceivedFromClient = true // Mark this even if it's an error
				s.mu.Unlock()
				msg := fmt.Sprintf("content-length mismatch on END_STREAM: declared %d, received %d",
					clValue, potentialTotalReceivedThisFrameContext)
				s.conn.log.Error(msg, logger.LogFields{"stream_id": s.id, "final_total": potentialTotalReceivedThisFrameContext, "content_length": clValue})
				return NewStreamError(s.id, ErrCodeProtocolError, msg)
			}
		} else { // Not END_STREAM on this frame
			if int64(potentialTotalReceivedThisFrameContext) > clValue {
				s.mu.Unlock()
				msg := fmt.Sprintf("data received (%d with current frame) exceeds declared content-length (%d)",
					potentialTotalReceivedThisFrameContext, clValue)
				s.conn.log.Error(msg, logger.LogFields{"stream_id": s.id, "potential_total": potentialTotalReceivedThisFrameContext, "content_length": clValue})
				return NewStreamError(s.id, ErrCodeProtocolError, msg)
			}
		}
		if dataLen > 0 {
			s.receivedDataBytes += uint64(dataLen)
		}
	} else { // s.parsedContentLength == nil (no Content-Length header was present)
		if dataLen > 0 {
			s.receivedDataBytes += uint64(dataLen)
		}
	}

	currentDataFrameChan := s.dataFrameChan
	streamCtx := s.ctx // Use the stream's context for the select below
	s.mu.Unlock()      // UNLOCK BEFORE CHANNEL OPERATIONS

	// Re-check stream context (s.ctx not currentCtx from outer scope) before attempting to queue.
	s.conn.log.Debug("handleDataFrame: Checking streamCtx (s.ctx) before select.", logger.LogFields{"stream_id": s.id, "ctx_err_before_select": fmt.Sprintf("%v", streamCtx.Err())})
	select {
	case <-streamCtx.Done():
		s.conn.log.Warn("handleDataFrame: Stream context done before queuing DATA frame (re-check). Returning error.", logger.LogFields{"stream_id": s.id, "ctx_err": streamCtx.Err()})
		return NewStreamError(s.id, ErrCodeCancel, "stream context done, cannot queue data") // Or StreamClosed
	default:
		// Context not done, proceed to queue.
	}

	if currentDataFrameChan == nil {
		s.conn.log.Warn("handleDataFrame: dataFrameChan already closed/nil, cannot queue DATA frame.", logger.LogFields{"stream_id": s.id, "data_len": dataLen, "has_end_stream": hasEndStreamFlag})
		if dataLen > 0 && !hasEndStreamFlag {
			return NewStreamError(s.id, ErrCodeStreamClosed, "attempt to send DATA on stream where END_STREAM already processed via channel closure")
		}
		// If dataLen is 0 or hasEndStreamFlag is true, and channel is nil, it's likely fine (already handled).
		// However, the top-level checks for closed/reset state should catch this earlier.
		// If it reaches here, it implies a more subtle state issue. For now, return nil if not an error condition.
		return nil
	}

	select {
	case currentDataFrameChan <- frame:
		s.conn.log.Debug("handleDataFrame: Queued DATA frame to dataFrameChan", logger.LogFields{"stream_id": s.id, "data_len": dataLen, "has_end_stream": hasEndStreamFlag})
	case <-streamCtx.Done():
		s.conn.log.Warn("handleDataFrame: Stream context done, cannot queue DATA frame (during send).", logger.LogFields{"stream_id": s.id, "ctx_err": streamCtx.Err()})
		return NewStreamError(s.id, ErrCodeCancel, "stream context done, cannot queue data")
	default: // Non-blocking check for full channel
		s.conn.log.Error("handleDataFrame: dataFrameChan full, cannot queue DATA frame. Stream may be stuck.", logger.LogFields{"stream_id": s.id})
		s.mu.Lock()
		if s.dataFrameChan != nil { // Check again under lock
			func() {
				defer func() {
					if r := recover(); r != nil {
						s.conn.log.Debug("Recovered from panic closing dataFrameChan in handleDataFrame (chan full path)",
							logger.LogFields{"stream_id": s.id, "panic": r})
					}
				}()
				close(s.dataFrameChan) // Close the channel to unblock processIncomingDataFrames
			}()
			// s.dataFrameChan = nil // Do not nil it out here
		}
		s.mu.Unlock()
		return NewStreamError(s.id, ErrCodeFlowControlError, "stream data channel full, possible deadlock or slow handler")
	}

	if hasEndStreamFlag {
		s.mu.Lock() // Lock for state and s.dataFrameChan modification
		s.endStreamReceivedFromClient = true
		s.conn.log.Debug("handleDataFrame: END_STREAM flag on DATA frame. Closing dataFrameChan.", logger.LogFields{"stream_id": s.id})
		if s.dataFrameChan != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						s.conn.log.Debug("Recovered from panic closing dataFrameChan in handleDataFrame (END_STREAM path)",
							logger.LogFields{"stream_id": s.id, "panic": r})
					}
				}()
				close(s.dataFrameChan)
			}()
			// s.dataFrameChan = nil // REMOVED: Mark as closed for this context
		}

		switch s.state {
		case StreamStateOpen:
			s._setState(StreamStateHalfClosedRemote)
		case StreamStateHalfClosedLocal:
			s._setState(StreamStateClosed)
		default:
			s.mu.Unlock() // Unlock before returning error
			s.conn.log.Error("Received END_STREAM on DATA frame in unexpected stream state", logger.LogFields{
				"stream_id": s.id,
				"state":     s.state.String(),
			})
			return NewStreamError(s.id, ErrCodeProtocolError, fmt.Sprintf("END_STREAM on DATA in unexpected state %s for stream %d", s.state, s.id))
		}
		s.mu.Unlock() // Unlock after state changes
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

		// As per h2spec 5.1/12 (closed: Sends a HEADERS frame):
		// The endpoint MUST treat this as a connection error of type STREAM_CLOSED.
		// This implies a GOAWAY(STREAM_CLOSED) from the connection.
		// By returning NewStreamError here, conn.go (specifically its frame dispatch/error handling)
		// will be responsible for sending the appropriate GOAWAY frame.
		// The direct s.sendRSTStream(ErrCodeStreamClosed) call is omitted.

		return NewConnectionError(ErrCodeStreamClosed, "headers received on closed stream")
	}

	// NEW CHECK for h2spec 8.1/1
	// If a second HEADERS frame is received for a stream that is currently in StreamStateOpen
	// (i.e., the first HEADERS frame for the request did not have END_STREAM set, which is implied by StreamStateOpen),
	// and this second HEADERS frame also does not have END_STREAM set, it MUST be treated as a stream error of type PROTOCOL_ERROR.
	// `hasInitialHeadersBeenProcessed` captures if this is a "second" (or subsequent) HEADERS block.
	if hasInitialHeadersBeenProcessed && currentState == StreamStateOpen && !endStream {
		s.conn.log.Warn("Stream: Subsequent HEADERS frame received on Open stream without END_STREAM flag (violates h2spec 8.1/1).",
			logger.LogFields{"stream_id": s.id, "state": currentState.String()})
		// s.sendRSTStream(ErrCodeProtocolError) logic removed
		return NewStreamError(s.id, ErrCodeProtocolError, "Subsequent HEADERS frame received on Open stream without END_STREAM flag (violates h2spec 8.1/1)") // Return StreamError
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

	for _, hf := range headers { // 'headers' are the full hpack.HeaderFields for this block
		// 1.a Uppercase header field names (h2spec 8.1.2 #1)
		// HTTP/2 header field names MUST be lowercase.
		for _, char := range hf.Name {
			if char >= 'A' && char <= 'Z' {
				errMsg := fmt.Sprintf("invalid header field name '%s' contains uppercase characters", hf.Name)
				s.conn.log.Error(errMsg, logger.LogFields{"stream_id": s.id, "header_name": hf.Name})
				// s.sendRSTStream(ErrCodeProtocolError) logic removed
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
			// s.sendRSTStream(ErrCodeProtocolError) logic removed
			return NewStreamError(s.id, ErrCodeProtocolError, errMsg) // Return StreamError
		}

		// 1.c TE header validation (h2spec 8.1.2.2 #2)
		if lowerName == "te" {
			if strings.ToLower(hf.Value) != "trailers" {
				errMsg := fmt.Sprintf("TE header field contains invalid value '%s', must be 'trailers'", hf.Value)
				s.conn.log.Error(errMsg, logger.LogFields{"stream_id": s.id, "header_value": hf.Value})
				// s.sendRSTStream(ErrCodeProtocolError) logic removed
				return NewStreamError(s.id, ErrCodeProtocolError, errMsg) // Return StreamError
			}
		}

		// 1.d Pseudo-headers in trailers (h2spec 8.1.2.1 #3)
		// VALIDATION MOVED TO conn.go:finalizeHeaderBlockAndDispatch
		// A HEADERS frame carrying trailers MUST NOT contain pseudo-header fields.
		// This logic is now handled in conn.go's finalizeHeaderBlockAndDispatch.
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
		// Return StreamError for pseudo-header issues. conn.go can escalate if needed.
		return NewStreamError(s.id, ErrCodeProtocolError, "pseudo-header validation failed: "+pseudoErr.Error())
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
		Body: &requestBodyConsumedNotifier{ // This line is modified
			stream: s,
			reader: s.requestBodyReader,
		}, // This closing brace is part of the Body field
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

	s.conn.log.Debug("Stream.processRequestHeadersAndDispatch: Comparing pointers", logger.LogFields{
		"stream_id":           s.id,
		"s_ptr":               fmt.Sprintf("%p", s),
		"s.requestBodyReader": fmt.Sprintf("%p", s.requestBodyReader),
		"req_body_ptr":        fmt.Sprintf("%p", req.Body),
		"req_body_type":       fmt.Sprintf("%T", req.Body),
	})
	if notifier, ok := req.Body.(*requestBodyConsumedNotifier); ok {
		s.conn.log.Debug("Stream.processRequestHeadersAndDispatch: req.Body is requestBodyConsumedNotifier", logger.LogFields{
			"stream_id":           s.id,
			"notifier.stream_ptr": fmt.Sprintf("%p", notifier.stream),
			"notifier.reader_ptr": fmt.Sprintf("%p", notifier.reader),
		})
	}

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

// processIncomingDataFrames is a goroutine that reads DATA frames from dataFrameChan
// and writes their payload to the requestBodyWriter pipe. It handles END_STREAM.
func (s *Stream) processIncomingDataFrames() {
	// Capture dataFrameChan locally to prevent issues if s.dataFrameChan is nilled out concurrently.
	// This localChan will be used in the select.
	s.mu.RLock() // RLock to safely read s.dataFrameChan
	localChan := s.dataFrameChan
	s.mu.RUnlock()

	if localChan == nil {
		s.conn.log.Warn("Stream.processIncomingDataFrames: s.dataFrameChan was nil at goroutine start. Exiting.", logger.LogFields{"stream_id": s.id})
		// Ensure pipe is closed if we exit early.
		if s.requestBodyWriter != nil {
			_ = s.requestBodyWriter.CloseWithError(fmt.Errorf("stream %d data channel was nil at start of processing", s.id))
		}
		return
	}

	defer func() {
		// Ensure pipe is closed when this goroutine exits, signaling EOF or error to reader.
		if r := recover(); r != nil {
			s.conn.log.Error("Panic in stream.processIncomingDataFrames", logger.LogFields{
				"stream_id": s.id,
				"panic_val": fmt.Sprintf("%v", r),
				"stack":     string(debug.Stack()),
			})
			// Ensure requestBodyWriter is closed with an error to unblock any handler.
			if s.requestBodyWriter != nil { // Check if nil before closing
				_ = s.requestBodyWriter.CloseWithError(fmt.Errorf("panic in body processing for stream %d: %v", s.id, r))
			}
		} else {
			// Normal exit from loop (channel closed or context done).
			// Determine the correct close reason for the pipe.
			if s.requestBodyWriter != nil { // Check if nil before closing
				select {
				case <-s.ctx.Done():
					// If context was cancelled, use that error.
					_ = s.requestBodyWriter.CloseWithError(s.ctx.Err())
				default:
					// Otherwise, it was a graceful close of dataFrameChan (END_STREAM). Signal EOF.
					_ = s.requestBodyWriter.Close()
				}
			}
		}
		s.conn.log.Debug("Stream.processIncomingDataFrames: Goroutine exiting", logger.LogFields{"stream_id": s.id})
	}()

	s.conn.log.Debug("Stream.processIncomingDataFrames: Goroutine started", logger.LogFields{"stream_id": s.id})

	for {
		s.conn.log.Debug("Stream.processIncomingDataFrames: Top of loop, before select.", logger.LogFields{"stream_id": s.id})
		select {
		case <-s.ctx.Done(): // Stream context cancelled (e.g., RST_STREAM received/sent, or conn closed)
			s.conn.log.Debug("Stream.processIncomingDataFrames: Stream context done, exiting.", logger.LogFields{"stream_id": s.id, "ctx_err": s.ctx.Err()})
			// The defer will handle closing the requestBodyWriter with s.ctx.Err().
			return
		case frame, ok := <-localChan: // Use localChan here
			s.conn.log.Debug("Stream.processIncomingDataFrames: Post-select, read from dataFrameChan.", logger.LogFields{"stream_id": s.id, "frame_is_nil_after_read": frame == nil, "ok_after_read": ok})
			// s.conn.log.Debug("Stream.processIncomingDataFrames: Read from dataFrameChan.", logger.LogFields{"stream_id": s.id, "frame_is_nil": frame == nil, "ok": ok}) // Temporarily commented out
			if !ok { // dataFrameChan was closed, means END_STREAM was processed or stream is closing forcefully.
				s.conn.log.Debug("Stream.processIncomingDataFrames: dataFrameChan closed, exiting.", logger.LogFields{"stream_id": s.id})
				// The defer will handle closing the requestBodyWriter (likely with EOF if context not done).
				return
			}

			if frame == nil { // Should not happen if chan is typed *DataFrame, but defensive
				s.conn.log.Warn("Stream.processIncomingDataFrames: Received nil frame from dataFrameChan", logger.LogFields{"stream_id": s.id})
				continue
			}

			s.conn.log.Debug("Stream.processIncomingDataFrames: Got DATA frame from channel", logger.LogFields{"stream_id": s.id, "data_len": len(frame.Data)})

			// Content-Length validation is now handled synchronously in handleDataFrame.
			// processIncomingDataFrames just writes data from the channel to the pipe.

			if len(frame.Data) > 0 {
				// Write to pipe. This can block if handler isn't reading.
				// If s.ctx is cancelled, this Write should eventually unblock due to pipe breaking.
				nWritten, err := s.requestBodyWriter.Write(frame.Data)
				if err != nil {
					s.conn.log.Error("Stream.processIncomingDataFrames: Error writing to requestBodyWriter, attempting to RST stream.", logger.LogFields{
						"stream_id": s.id,
						"error":     err.Error(),
					})
					// Error writing to pipe, probably means reader side closed or stream context cancelled.
					// Attempt to RST the stream.
					// Use ErrCodeCancel as the handler essentially cancelled its reception of the body.
					// Or ErrCodeInternalError if it's seen as an internal server problem.
					// Let's use ErrCodeCancel for now.
					if rstErr := s.sendRSTStream(ErrCodeCancel); rstErr != nil {
						s.conn.log.Error("Stream.processIncomingDataFrames: Failed to send RST_STREAM after pipe write error.", logger.LogFields{
							"stream_id":      s.id,
							"pipe_err":       err.Error(),
							"rst_stream_err": rstErr.Error(),
						})
					}
					// Exit goroutine.
					return
				}
				// s.receivedDataBytes += uint64(nWritten) // Removed: data race and CL validation is synchronous in handleDataFrame
				s.conn.log.Debug("Stream.processIncomingDataFrames: Wrote DATA to pipe", logger.LogFields{"stream_id": s.id, "bytes": nWritten})
			}
			// END_STREAM on the frame itself is handled by handleDataFrame by closing dataFrameChan.
			// This loop just processes data until chan is closed.
		}
	}
}

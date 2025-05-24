package http2

import (
	"context"
	"encoding/json"
	"io"
	"net/http" // For http.Header, if we decide to use it directly in Request struct

	"fmt"     // For error formatting, Sprintf
	"net/url" // For url.URL
	"strings" // For pseudo-header check
	"sync"

	"example.com/llmahttap/v2/internal/logger" // For logging within stream operations
	"golang.org/x/net/http2/hpack"             // For hpack.HeaderField
)

// StreamState represents the state of an HTTP/2 stream, as defined in RFC 7540, Section 5.1.

// TODO: Remove this stub once connection.go is implemented
type connection struct {
	logger *logger.Logger
	ctx    context.Context
	// Mock methods needed by Stream for now
	sendHeadersFrame     func(s *Stream, headers []hpack.HeaderField, endStream bool) error
	sendDataFrame        func(s *Stream, p []byte, endStream bool) (int, error)
	sendTrailersFrame    func(s *Stream, trailers []hpack.HeaderField) error
	sendRSTStreamFrame   func(streamID uint32, errorCode ErrorCode) error
	updateStreamPriority func(streamID uint32, depStreamID uint32, weight uint8, exclusive bool)
	removeStream         func(streamID uint32, errCode ErrorCode)
	streamHandlerDone    func(s *Stream)
	remoteAddrStr        func() string
	isTLS                func() bool
	initialSettings      InitialSettings               // Placeholder
	hpackDecoder         *HpackAdapter                 // Placeholder for HPACK decoding for requests
	peerSettings         map[SettingID]uint32          // Placeholder
	ourSettings          map[SettingID]uint32          // Placeholder
	maxFrameSize         uint32                        // Max frame size this connection can send
	connFCManager        *ConnectionFlowControlManager // Connection-level flow control
	activeStreams        map[uint32]*Stream            // Map of active streams
	priorityTree         *PriorityTree
}
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
type ResponseWriter interface {
	// SendHeaders sends response headers.
	// If endStream is true, this also signals the end of the response body (e.g., for HEAD requests or empty bodies).
	SendHeaders(headers []hpack.HeaderField, endStream bool) error

	// WriteData sends a chunk of the response body.
	// If endStream is true, this is the final chunk of the response body.
	// It returns the number of bytes written from p.
	WriteData(p []byte, endStream bool) (n int, err error)

	// WriteTrailers sends trailing headers after the response body.
	// This implicitly ends the stream.
	WriteTrailers(trailers []hpack.HeaderField) error

	// TODO: Consider adding a Flush() method if explicit control over sending buffered data is needed.
}

// Handler is the interface that processes requests for a given route.
// Feature Spec 1.4.2: "Each handler implementation MUST conform to a server-defined internal interface
// (e.g., a Go interface) that accepts necessary request details (like http.Request or equivalent
// structured data), a means to write the response (like http.ResponseWriter or equivalent stream
// writer), and its specific HandlerConfig (as an opaque structure to be type-asserted by the handler)."
type Handler interface {
	// ServeHTTP2 processes the request on the given stream.
	// The stream provides methods to get request details and to write the response.
	// The handlerConfig is passed to the handler when it's instantiated.
	ServeHTTP2(s *Stream, req *http.Request) // Using http.Request for now, may need custom struct.
}

// Stream represents a single HTTP/2 stream.
// It manages stream state, flow control, priority, and request/response processing.
type Stream struct {
	id    uint32
	state StreamState
	mu    sync.RWMutex // Protects stream state and other mutable fields like headers, body parts.

	// Connection context
	conn *connection // Parent connection, defined in connection.go

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
	handler           Handler             // Handler responsible for this stream
	handlerConfig     json.RawMessage     // Configuration for the handler

	// Response handling (Stream implements ResponseWriter or provides one)
	responseHeadersSent bool                // True if response HEADERS frame has been sent
	responseTrailers    []hpack.HeaderField // Trailers to be sent with last DATA or separately

	// Stream lifecycle and state
	ctx                         context.Context    // Context for this stream, derived from connection context
	cancelCtx                   context.CancelFunc // Cancels the stream context
	endStreamReceivedFromClient bool               // True if client sent END_STREAM flag
	endStreamSentToClient       bool               // True if server sent END_STREAM flag
	pendingRSTCode              *ErrorCode         // If non-nil, an RST_STREAM with this code needs to be sent

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
	conn *connection,
	id uint32,
	initialOurWindowSize uint32,
	initialPeerWindowSize uint32,
	handler Handler,
	handlerCfg json.RawMessage,
	prioWeight uint8,
	prioParentID uint32,
	prioExclusive bool,
) (*Stream, error) {
	ctx, cancel := context.WithCancel(conn.ctx)

	pr, pw := io.Pipe()

	s := &Stream{
		id:                          id,
		state:                       StreamStateIdle,
		conn:                        conn,
		fcManager:                   NewStreamFlowControlManager(id, initialOurWindowSize, initialPeerWindowSize),
		priorityWeight:              prioWeight,
		priorityParentID:            prioParentID,
		priorityExclusive:           prioExclusive,
		requestBodyReader:           pr,
		requestBodyWriter:           pw,
		handler:                     handler,
		handlerConfig:               handlerCfg,
		ctx:                         ctx,
		cancelCtx:                   cancel,
		endStreamReceivedFromClient: false,
		endStreamSentToClient:       false,
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
func (s *Stream) SendHeaders(headers []hpack.HeaderField, endStream bool) error {
	s.mu.Lock()
	// defer s.mu.Unlock() // Unlock must be carefully managed due to calls to conn

	if s.responseHeadersSent {
		s.mu.Unlock()
		s.conn.logger.Error("stream: SendHeaders called after headers already sent", logger.LogFields{"stream_id": s.id})
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

	err := s.conn.sendHeadersFrame(s, headers, endStream) // This method in conn must handle its own locking.

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

// WriteData sends a chunk of the response body.
func (s *Stream) WriteData(p []byte, endStream bool) (n int, err error) {
	s.mu.Lock() // Initial lock for checks
	if !s.responseHeadersSent {
		s.mu.Unlock()
		s.conn.logger.Error("stream: WriteData called before SendHeaders", logger.LogFields{"stream_id": s.id})
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
			s.conn.logger.Warn("stream: WriteData with endStream=true called, but stream already marked as ended by server", logger.LogFields{"stream_id": s.id})
			return 0, NewStreamError(s.id, ErrCodeInternalError, "stream already ended by a previous write with endStream=true")
		}
		// s.state checks already done above, assuming it hasn't changed to closed by a concurrent RST *after* initial unlock.
		// A truly robust check would re-verify s.state here, but that adds complexity for a very racy condition.
		// If state changed, sendDataFrame might fail or be ignored.
		s.mu.Unlock() // Unlock for the sendDataFrame call

		// The stub sendDataFrame returns (int, error). We expect 0 bytes for an empty frame.
		_, sendErr := s.conn.sendDataFrame(s, nil, true)

		s.mu.Lock() // Re-lock for state update
		if sendErr == nil {
			s.endStreamSentToClient = true
			s.transitionStateOnSendEndStream()
		} else {
			s.conn.logger.Error("stream: failed to send empty DATA frame with END_STREAM", logger.LogFields{"stream_id": s.id, "error": sendErr.Error()})
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
	if maxFramePayloadSize < MinAllowedFrameSize { // Ensure it's at least the protocol minimum
		maxFramePayloadSize = MinAllowedFrameSize
	}

	for dataOffset < totalBytesToWrite {
		chunkLen := totalBytesToWrite - dataOffset
		if chunkLen > int(maxFramePayloadSize) {
			chunkLen = int(maxFramePayloadSize)
		}

		// Acquire flow control for this chunk (chunkLen is always > 0 in this loop)
		// Stream-level FC
		if errFc := s.fcManager.AcquireSendSpace(uint32(chunkLen)); errFc != nil {
			s.conn.logger.Error("stream: failed to acquire stream flow control",
				logger.LogFields{"stream_id": s.id, "chunk_len": chunkLen, "requested": chunkLen, "available": s.fcManager.GetStreamSendAvailable(), "error": errFc.Error()})
			return dataOffset, errFc // Return bytes successfully sent so far
		}

		// Connection-level FC
		if s.conn.connFCManager != nil { // Defensive, stub might not init
			if errFc := s.conn.connFCManager.AcquireSendSpace(uint32(chunkLen)); errFc != nil {
				s.conn.logger.Error("stream: failed to acquire connection flow control",
					logger.LogFields{"stream_id": s.id, "chunk_len": chunkLen, "requested": chunkLen, "conn_available": s.conn.connFCManager.GetConnectionSendAvailable(), "error": errFc.Error()})
				// CRITICAL: Stream FC was acquired but conn FC failed.
				// The current FlowControlWindow has no "release" or "rollback".
				// The stream FC remains debited for this failed attempt.
				// This is a limitation of the current FCW design and needs future improvement
				// (e.g., try-acquire, or a mechanism to return acquired capacity).
				// For now, the write fails, and stream FC is effectively "lost" for this operation.
				return dataOffset, errFc
			}
		}

		// Re-check stream state under lock before sending, as it could have changed (e.g., RST received)
		s.mu.Lock()
		if s.state == StreamStateClosed || s.pendingRSTCode != nil {
			s.mu.Unlock()
			s.conn.logger.Warn("stream: state changed to closed/reset after FC acquire, before sending data chunk",
				logger.LogFields{"stream_id": s.id, "chunk_len": chunkLen, "state": s.state.String()})
			// Acquired FC is "lost" from this operation's view. Connection might be closing.
			return dataOffset, NewStreamError(s.id, ErrCodeStreamClosed, "stream closed/reset before send data chunk")
		}
		if s.endStreamSentToClient { // If another operation (e.g. WriteTrailers) ended the stream.
			s.mu.Unlock()
			s.conn.logger.Warn("stream: stream ended by server after FC acquire, before sending data chunk",
				logger.LogFields{"stream_id": s.id, "chunk_len": chunkLen})
			return dataOffset, NewStreamError(s.id, ErrCodeInternalError, "stream ended by server before send data chunk")
		}
		s.mu.Unlock() // Unlock for the sendDataFrame call

		chunkData := p[dataOffset : dataOffset+chunkLen]
		isLastChunk := (dataOffset + chunkLen) == totalBytesToWrite
		frameEndStream := isLastChunk && endStream

		bytesSentThisChunk, sendErr := s.conn.sendDataFrame(s, chunkData, frameEndStream)

		if sendErr != nil {
			s.conn.logger.Error("stream: sendDataFrame failed for data chunk",
				logger.LogFields{"stream_id": s.id, "chunk_len": chunkLen, "error": sendErr.Error()})
			// dataOffset is not incremented for *this* chunk.
			// Acquired FC for this chunk is "lost" if send truly failed.
			return dataOffset, sendErr
		}

		if bytesSentThisChunk != chunkLen {
			// This implies the underlying connection write was partial but didn't error.
			// This is complex to handle gracefully; usually means connection issues.
			// For simplicity, treat as an error condition if not all bytes were "accepted" by sendDataFrame.
			s.conn.logger.Error("stream: sendDataFrame reported partial write without error",
				logger.LogFields{"stream_id": s.id, "expected_len": chunkLen, "sent_len": bytesSentThisChunk})
			dataOffset += bytesSentThisChunk // Still account for what was sent
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

// WriteTrailers sends trailing headers.
func (s *Stream) WriteTrailers(trailers []hpack.HeaderField) error {
	s.mu.Lock()
	if !s.responseHeadersSent {
		s.mu.Unlock()
		s.conn.logger.Error("stream: WriteTrailers called before SendHeaders", logger.LogFields{"stream_id": s.id})
		return NewStreamError(s.id, ErrCodeInternalError, "SendHeaders must be called before WriteTrailers")
	}
	if s.endStreamSentToClient {
		s.mu.Unlock()
		s.conn.logger.Error("stream: WriteTrailers called after stream already ended", logger.LogFields{"stream_id": s.id})
		return NewStreamError(s.id, ErrCodeInternalError, "stream already ended by a previous write with endStream=true")
	}
	if s.state == StreamStateClosed || s.pendingRSTCode != nil {
		s.mu.Unlock()
		return NewStreamError(s.id, ErrCodeStreamClosed, "stream closed or being reset")
	}
	s.mu.Unlock()

	err := s.conn.sendTrailersFrame(s, trailers) // This method in conn must handle its own locking.

	s.mu.Lock() // Re-lock to update stream state
	defer s.mu.Unlock()

	if err != nil {
		return err
	}

	s.responseTrailers = trailers
	s.endStreamSentToClient = true
	s.transitionStateOnSendEndStream() // Trailers always end the stream for the sender.

	return nil
}

// Methods for stream processing (to be expanded)

// handleHeadersFrame is called by the connection when a HEADERS frame (potentially after merging with CONTINUATIONs)
// is received for this stream. It decodes headers, updates stream priority, transitions stream state,
// stores decoded headers, and if applicable, invokes the handler.
func (s *Stream) handleHeadersFrame(frame *HeadersFrame, hpackDecoder *HpackAdapter) error {
	s.mu.Lock()

	isInitialHeaders := false
	isTrailerHeaders := false
	currentState := s.state

	logFieldsBase := logger.LogFields{"stream_id": s.id, "state": currentState.String(), "frame_flags": frame.Header().Flags}

	switch currentState {
	case StreamStateIdle: // Client is opening a new stream
		isInitialHeaders = true
	case StreamStateOpen, StreamStateHalfClosedLocal: // Client is sending Trailing HEADERS
		if s.endStreamReceivedFromClient && len(frame.HeaderBlockFragment) > 0 { // Trailers expected only if body not fully closed
			// If END_STREAM was already on HEADERS/DATA, trailers might still arrive if they were "in flight".
			// However, if client sent END_STREAM and *then* sends more HEADERS (trailers), it's an issue.
			// For simplicity, if data part truly ended, no more HEADERS/trailers.
			// RFC 7540, Section 8.1: "A request or response containing a body that is zero octets in length
			// is PUT to a server with a final DATA frame ... trailers are ... sent as a HEADERS frame
			// following the final DATA frame"
			// If END_STREAM already received (e.g. on initial HEADERS or DATA), then receiving more HEADERS is an error.
			s.mu.Unlock()
			s.conn.logger.Error("stream: received trailer HEADERS after END_STREAM already processed", logFieldsBase)
			s.sendRSTStream(ErrCodeStreamClosed)
			return NewStreamError(s.id, ErrCodeStreamClosed, "trailer HEADERS after END_STREAM processed")
		}
		isTrailerHeaders = true
	default:
		// Invalid state to receive HEADERS (e.g., Closed, HalfClosedRemote by client, ReservedLocal by server means client shouldn't send HEADERS)
		s.mu.Unlock()
		logFieldsBase["error_msg"] = "HEADERS frame received in invalid state"
		s.conn.logger.Error("stream: HEADERS frame received in invalid state", logFieldsBase)
		// RFC 7540, 6.2: "HEADERS frames MUST be associated with a stream. If a HEADERS frame is received on a stream that is not in
		// the "idle", "reserved (remote)", "open", or "half-closed (local)" state, the endpoint MUST respond with
		// a connection error (Section 5.4.1) of type PROTOCOL_ERROR."
		// ReservedLocal is not in that list for *receiving* client HEADERS.
		return NewConnectionError(ErrCodeProtocolError, "HEADERS frame in invalid state "+currentState.String())
	}

	// Decode Headers
	// The hpackDecoder is specific to this connection. The connection layer is responsible for
	// ensuring hpackDecoder.ResetDecoderState() or similar is called before a new header block
	// (from potentially different stream) is decoded, or relying on FinishDecoding.
	if err := hpackDecoder.DecodeFragment(frame.HeaderBlockFragment); err != nil {
		s.mu.Unlock()
		logFieldsBase["hpack_error"] = err.Error()
		s.conn.logger.Error("stream: HPACK decode fragment error", logFieldsBase)
		return NewConnectionError(ErrCodeCompressionError, "HPACK decode fragment error: "+err.Error())
	}
	decodedHeaders, err := hpackDecoder.FinishDecoding() // Finishes block, resets decoder state for next block
	if err != nil {
		s.mu.Unlock()
		logFieldsBase["hpack_error"] = err.Error()
		s.conn.logger.Error("stream: HPACK finish decoding error", logFieldsBase)
		return NewConnectionError(ErrCodeCompressionError, "HPACK finish decoding error: "+err.Error())
	}

	// Process Priority (if applicable and initial HEADERS)
	if isInitialHeaders && (frame.Header().Flags&FlagHeadersPriority != 0) {
		s.priorityParentID = frame.StreamDependency
		s.priorityWeight = frame.Weight
		s.priorityExclusive = frame.Exclusive
		if s.conn.priorityTree != nil {
			// This updates the authoritative priority tree on the connection.
			if errPrio := s.conn.priorityTree.UpdatePriority(s.id, frame.StreamDependency, frame.Weight, frame.Exclusive); errPrio != nil {
				// If self-dependency in priority update from HEADERS.
				// RFC 7540, 5.3.1 "A stream cannot depend on itself. An endpoint MUST treat this as a stream error ... of type PROTOCOL_ERROR."
				s.mu.Unlock()
				logFieldsBase["priority_error"] = errPrio.Error()
				s.conn.logger.Error("stream: priority update from HEADERS failed (e.g. self-dependency)", logFieldsBase)
				s.sendRSTStream(ErrCodeProtocolError)
				return NewStreamError(s.id, ErrCodeProtocolError, "priority update failed: "+errPrio.Error())
			}
		}
	}

	// Validate and Store Headers
	if isInitialHeaders {
		// TODO: Implement robust pseudo-header validation (RFC 7540 Section 8.1.2.1, 8.1.2.2, 8.1.2.3)
		// E.g., presence, order, single value for :method, :path, :scheme.
		// For now, basic storage.
		hasMethod := false
		hasPath := false
		hasScheme := false
		for _, hf := range decodedHeaders {
			if hf.Name == ":method" {
				hasMethod = true
			}
			if hf.Name == ":path" {
				hasPath = true
			}
			if hf.Name == ":scheme" {
				hasScheme = true
			}
		}
		if !hasMethod || !hasPath || !hasScheme {
			s.mu.Unlock()
			s.conn.logger.Error("stream: missing required pseudo-headers", logFieldsBase)
			s.sendRSTStream(ErrCodeProtocolError)
			return NewStreamError(s.id, ErrCodeProtocolError, "missing required pseudo-headers")
		}

		s.requestHeaders = decodedHeaders
		if currentState == StreamStateIdle {
			s._setState(StreamStateOpen)
		}
		// Note: StreamStateReservedRemote implies client sent PUSH_PROMISE, which is not allowed.
		// The connection state check above should ideally catch this as a connection error before stream processing.
		// If we do reach here from ReservedRemote (e.g., client-side code perspective receiving server headers after PUSH_PROMISE),
		// then ReservedRemote -> Open is a valid transition. Server-side, this state implies error.
		// For server, `idle -> open` is the main path for initial HEADERS from client.
	} else if isTrailerHeaders {
		// TODO: Implement trailer validation (RFC 7540 Section 8.1.2.2 - no pseudo-headers in trailers)
		for _, hf := range decodedHeaders {
			if strings.HasPrefix(hf.Name, ":") {
				s.mu.Unlock()
				s.conn.logger.Error("stream: pseudo-header found in trailers", logFieldsBase)
				s.sendRSTStream(ErrCodeProtocolError)
				return NewStreamError(s.id, ErrCodeProtocolError, "pseudo-header in trailers")
			}
		}
		s.requestTrailers = decodedHeaders
		s.conn.logger.Debug("stream: received trailer headers", logger.LogFields{"stream_id": s.id, "num_trailers": len(decodedHeaders)})
	}

	// Handle END_STREAM
	endStream := frame.Header().Flags&FlagHeadersEndStream != 0
	if endStream {
		s.endStreamReceivedFromClient = true
		if s.requestBodyWriter != nil {
			s.requestBodyWriter.Close() // Signal EOF to handler
		}
		s.transitionStateOnReceiveEndStream() // Updates state: Open->HalfClosedRemote, HalfClosedLocal->Closed
	}

	// Unlock stream before potentially long-running handler
	s.mu.Unlock()

	// Invoke Handler (if initial HEADERS)
	// The END_HEADERS flag on the frame ensures this is the complete logical header block.
	if isInitialHeaders {
		go s.runHandler()
	}

	return nil
}

// handleDataFrame is called by the connection when a DATA frame is received for this stream.
// It handles flow control, state checks, and then passes data to processIncomingDataBody.
func (s *Stream) handleDataFrame(frame *DataFrame) error {
	// Check stream state: DATA frames are only permitted in Open or HalfClosedLocal states.
	// RFC 7540, Section 6.1: "DATA frames ... MUST be associated with a stream. If a DATA frame is
	// received whose stream is not in "open" or "half-closed (local)" state, the recipient
	// MUST respond with a stream error (Section 5.4.2) of type STREAM_CLOSED."

	s.mu.Lock()
	currentState := s.state
	if currentState != StreamStateOpen && currentState != StreamStateHalfClosedLocal {
		s.mu.Unlock()
		s.conn.logger.Error("stream: DATA frame received in invalid state",
			logger.LogFields{"stream_id": s.id, "state": currentState.String(), "frame_flags": frame.Header().Flags})
		// The connection will likely send RST_STREAM or GOAWAY based on this error.
		// If the stream is idle or reserved, it's a connection error (PROTOCOL_ERROR).
		// If closed or half-closed(remote), it's a stream error (STREAM_CLOSED).
		// For simplicity here, assume higher level handles precise error code for RST.
		// We should not process the frame further.
		s.sendRSTStream(ErrCodeStreamClosed) // Send RST_STREAM with STREAM_CLOSED
		return NewStreamError(s.id, ErrCodeStreamClosed, "DATA frame in invalid state "+currentState.String())
	}

	// If END_STREAM was already received on a previous frame (e.g., HEADERS with END_STREAM, or prior DATA with END_STREAM)
	if s.endStreamReceivedFromClient {
		s.mu.Unlock()
		s.conn.logger.Error("stream: DATA frame received after END_STREAM already processed for request",
			logger.LogFields{"stream_id": s.id, "state": currentState.String(), "frame_flags": frame.Header().Flags})
		s.sendRSTStream(ErrCodeStreamClosed) // Data after stream closure
		return NewStreamError(s.id, ErrCodeStreamClosed, "DATA frame received after END_STREAM")
	}
	s.mu.Unlock() // Unlock before potentially blocking flow control calls.

	// Account for received data in stream-level flow control.
	// This may return a StreamError if peer violates flow control (sends too much).
	dataLen := uint32(len(frame.Data))
	if err := s.fcManager.DataReceived(dataLen); err != nil {
		s.conn.logger.Error("stream: stream flow control error on data received",
			logger.LogFields{"stream_id": s.id, "data_len": dataLen, "error": err.Error()})
		s.sendRSTStream(ErrCodeFlowControlError) // Send RST_STREAM with FLOW_CONTROL_ERROR
		return err                               // err is already a StreamError
	}

	// Notify connection-level flow control about received data.
	// This may return a ConnectionError if peer violates connection flow control.
	if s.conn.connFCManager != nil { // Defensive check, stub connection might not have it
		if err := s.conn.connFCManager.DataReceived(dataLen); err != nil {
			s.conn.logger.Error("stream: connection flow control error on data received",
				logger.LogFields{"stream_id": s.id, "data_len": dataLen, "error": err.Error()})
			// Connection error implies GOAWAY will be sent by connection manager.
			// This stream will be implicitly closed as part of connection termination.
			// No need to send RST_STREAM from here, as connection is failing.
			return err // err is a ConnectionError
		}
	}

	// Pass the actual data payload and END_STREAM flag to processIncomingDataBody.
	// This method handles writing to the pipe for the handler and state transitions.
	// processIncomingDataBody requires its own locking.
	if err := s.processIncomingDataBody(frame.Data, frame.Header().Flags&FlagDataEndStream != 0); err != nil {
		// processIncomingDataBody already logs and potentially sends RST_STREAM on error.
		return err
	}

	return nil
}

// processIncomingData is called by the connection when a DATA frame is received.

// processIncomingDataBody is called by the connection when a DATA frame's payload needs to be processed,
// after flow control accounting has been done.
// It writes data to the request body pipe and handles END_STREAM.
func (s *Stream) processIncomingDataBody(data []byte, endStream bool) error {
	// This method is called after flow control windows have been debited by handleDataFrame.
	// It's responsible for writing to the pipe and handling END_STREAM.

	// No data to write and not ending stream? No-op for the body processing part.
	if len(data) == 0 && !endStream {
		return nil
	}

	s.mu.Lock() // Lock for the whole operation except pipe write

	// Critical state check before attempting to use pipeWriter or modify endStream state.
	// handleDataFrame should have already performed similar checks. This is an additional safeguard.
	if s.state != StreamStateOpen && s.state != StreamStateHalfClosedLocal {
		s.mu.Unlock()
		s.conn.logger.Error("stream: processIncomingDataBody called in invalid state", logger.LogFields{"stream_id": s.id, "state": s.state.String()})
		// RST likely already sent by handleDataFrame. If not, this indicates a deeper issue.
		return NewStreamError(s.id, ErrCodeInternalError, "processIncomingDataBody in invalid state "+s.state.String())
	}

	// Check if END_STREAM was already received. handleDataFrame should also catch this.
	if s.endStreamReceivedFromClient {
		s.mu.Unlock()
		s.conn.logger.Error("stream: processIncomingDataBody called after END_STREAM already processed for request", logger.LogFields{"stream_id": s.id})
		// RST was likely already sent by handleDataFrame.
		return NewStreamError(s.id, ErrCodeStreamClosed, "processIncomingDataBody called after END_STREAM")
	}

	currentPipeWriter := s.requestBodyWriter // Get under lock

	if len(data) > 0 {
		if currentPipeWriter == nil {
			// This means the stream is likely being closed/reset concurrently, or END_STREAM on HEADERS closed the pipe.
			s.mu.Unlock()
			s.conn.logger.Warn("stream: requestBodyWriter is nil in processIncomingDataBody; data lost",
				logger.LogFields{"stream_id": s.id, "data_len": len(data), "end_stream": endStream})

			// If data is present and this isn't the frame that's supposed to end the stream and close the pipe,
			// it's an unexpected situation (data loss).
			if !endStream {
				s.sendRSTStream(ErrCodeInternalError)
				return NewStreamError(s.id, ErrCodeInternalError, "requestBodyWriter is nil while data is pending")
			}
			// If endStream is true, the pipe being nil might be due to prior END_STREAM on HEADERS.
			// Proceed to endStream logic below. Data, if any, is lost.
		} else {
			s.mu.Unlock() // Unlock for pipe write
			_, pipeErr := currentPipeWriter.Write(data)
			s.mu.Lock() // Re-lock after pipe write

			if pipeErr != nil {
				// s.mu is already held here
				s.conn.logger.Error("stream: error writing to request body pipe", logger.LogFields{"stream_id": s.id, "error": pipeErr.Error()})
				// Unlock before sendRSTStream, as it might re-enter or call conn methods
				s.mu.Unlock()
				s.sendRSTStream(ErrCodeCancel) // Handler will see pipe error; CANCEL indicates graceful termination request.
				return NewStreamError(s.id, ErrCodeInternalError, "error writing to request body pipe: "+pipeErr.Error())
			}
		}
	}
	// If len(data) == 0, s.mu is still held from the start of the function (or after re-lock if pipeWriter was nil and data > 0 path taken).
	// If len(data) > 0 and pipe write was successful, s.mu is held.

	if endStream {
		s.endStreamReceivedFromClient = true
		if currentPipeWriter != nil {
			// Close the writer to signal EOF to the reader (handler).
			// This is safe even if it was already closed.
			currentPipeWriter.Close()
		}
		s.transitionStateOnReceiveEndStream()
	}
	s.mu.Unlock()
	return nil
}

// handleRSTStreamFrame is called by the connection when an RST_STREAM frame is received for this stream.
// It processes the RST_STREAM frame, leading to the stream's closure.
// This method itself generally doesn't return an error because receiving RST_STREAM is a terminal
// action for the stream, and errors related to the frame's validity (e.g., on stream 0)
// are typically connection-level issues handled before dispatching to the stream.
func (s *Stream) handleRSTStreamFrame(frame *RSTStreamFrame) error {
	// Log that we are handling an RST_STREAM frame at this entry point.
	// More detailed logging occurs within processIncomingRSTStream.
	s.conn.logger.Debug("stream: received RST_STREAM frame, dispatching for processing", logger.LogFields{
		"stream_id":       s.id,
		"frame_stream_id": frame.Header().StreamID, // Should match s.id
		"error_code":      frame.ErrorCode.String(),
	})

	// Validate that the frame's StreamID matches the stream's ID.
	// This should ideally be guaranteed by the dispatcher in the connection layer.
	if frame.Header().StreamID != s.id {
		msg := fmt.Sprintf("RST_STREAM frame stream ID %d in frame header does not match stream object ID %d", frame.Header().StreamID, s.id)
		s.conn.logger.Error("stream: RST_STREAM frame ID mismatch (server dispatch error)", logger.LogFields{
			"stream_id":       s.id,
			"frame_stream_id": frame.Header().StreamID,
			"error_msg":       msg,
		})
		// This is an internal server error if dispatch was incorrect.
		return NewConnectionError(ErrCodeInternalError, msg)
	}

	// RFC 7540, Section 6.4: "RST_STREAM frames MUST be associated with a stream. If a RST_STREAM frame
	// is received on a stream that is not in the "open", "reserved (local)", "reserved (remote)", or "half-closed"
	// state, the recipient MUST treat this as a connection error (Section 5.4.1) of type PROTOCOL_ERROR."
	// This check is tricky here because a stream object might be created just before this call
	// or might be in 'idle' if looked up by ID.
	// The `processIncomingRSTStream` method already handles the `StreamStateClosed` case gracefully.
	// If the connection layer dispatched this to an 'idle' stream (by ID), it would be a connection error.
	// For an existing stream object, any RST_STREAM leads to closure.
	// We rely on `processIncomingRSTStream` to correctly manage the state transition.

	s.processIncomingRSTStream(frame.ErrorCode)

	// Processing RST_STREAM is a terminal action for the stream and typically doesn't "fail"
	// in a way that requires an error return to the connection layer (unless the frame itself
	// triggered a connection error, which would be handled before this stream-specific method).
	return nil
}

// processIncomingRSTStream is called by the connection when an RST_STREAM frame is received.
func (s *Stream) processIncomingRSTStream(errorCode ErrorCode) {
	s.mu.Lock()

	if s.state == StreamStateClosed {
		s.mu.Unlock()
		return
	}

	s.conn.logger.Info("stream: received RST_STREAM", logger.LogFields{"stream_id": s.id, "error_code": errorCode.String()})

	// pipeWriter := s.requestBodyWriter // This variable is unused as cleanup is handled by closeStreamResourcesProtected

	// Keep a reference to call after unlock, if needed, but closeStreamResourcesProtected handles it.
	// cancelCtxFunc := s.cancelCtx
	// pipeWriter := s.requestBodyWriter

	s.pendingRSTCode = &errorCode  // Record why it's closing
	s._setState(StreamStateClosed) // This will call closeStreamResourcesProtected

	s.mu.Unlock() // Unlock before calling external methods that might block or re-enter

	// Resource cleanup (pipes, context) is handled by closeStreamResourcesProtected,
	// which is called by _setState(StreamStateClosed).
	// conn.removeStream is also called by closeStreamResourcesProtected.
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
	s.pendingRSTCode = nil
	s.conn.logger.Error("stream: failed to send RST_STREAM", logger.LogFields{"stream_id": s.id, "error_code": errorCode.String(), "send_error": err.Error()})
	s.mu.Unlock()
	return err // Return the send error
}

// runHandler is a goroutine that executes the stream's designated handler.
func (s *Stream) runHandler() {
	req := &http.Request{
		Method:     "",
		URL:        &url.URL{Path: ""}, // Use url.URL
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
		ProtoMinor: 0,
		Header:     make(http.Header),
		Body:       s.requestBodyReader,
		Host:       "",
		RequestURI: "",
		RemoteAddr: s.conn.remoteAddrStr(),
		// Context will be set using WithContext
	}
	req = req.WithContext(s.ctx)

	isTLS := s.conn.isTLS()
	s.mu.RLock() // Lock for reading requestHeaders
	for _, hf := range s.requestHeaders {
		switch hf.Name {
		case ":method":
			req.Method = hf.Value
		case ":path":
			req.URL.Path = hf.Value
			req.RequestURI = hf.Value
		case ":scheme":
			req.URL.Scheme = hf.Value
		case ":authority":
			req.Host = hf.Value
			req.URL.Host = hf.Value
		case "host":
			if req.Host == "" {
				req.Host = hf.Value
			}
		default:
			if !strings.HasPrefix(hf.Name, ":") {
				req.Header.Add(hf.Name, hf.Value)
			}
		}
	}
	s.mu.RUnlock()

	if req.URL.Scheme == "" {
		if isTLS {
			req.URL.Scheme = "https"
		} else {
			req.URL.Scheme = "http"
		}
	}
	if req.URL.Host == "" {
		req.URL.Host = req.Host
	}

	defer func() {
		if r := recover(); r != nil {
			s.conn.logger.Error("stream: handler panic", logger.LogFields{"stream_id": s.id, "panic": r})
			s.mu.RLock()
			headersSent := s.responseHeadersSent
			streamEnded := s.endStreamSentToClient
			s.mu.RUnlock()

			if !headersSent && !streamEnded {
				s.sendRSTStream(ErrCodeInternalError) // Or send 500 response
			} else if !streamEnded {
				s.sendRSTStream(ErrCodeInternalError)
			}
		}
		s.requestBodyWriter.Close()
		s.conn.streamHandlerDone(s)
	}()

	s.handler.ServeHTTP2(s, req)

	s.mu.Lock()
	if !s.endStreamSentToClient && s.responseHeadersSent {
		s.mu.Unlock()
		s.conn.logger.Info("stream: handler returned without ending stream, sending empty DATA frame with END_STREAM", logger.LogFields{"stream_id": s.id})
		_, err := s.WriteData(nil, true)
		if err != nil {
			s.conn.logger.Error("stream: error sending final empty DATA frame", logger.LogFields{"stream_id": s.id, "error": err.Error()})
			s.sendRSTStream(ErrCodeInternalError)
		}
	} else if !s.responseHeadersSent {
		s.mu.Unlock()
		s.conn.logger.Error("stream: handler returned without sending response headers", logger.LogFields{"stream_id": s.id})
		s.sendRSTStream(ErrCodeInternalError)
	} else {
		s.mu.Unlock()
	}
}

// transitionStateOnReceiveEndStream updates stream state when END_STREAM is received.
func (s *Stream) transitionStateOnReceiveEndStream() {
	// Assumes s.mu is held by caller.
	switch s.state {
	case StreamStateOpen:
		s.state = StreamStateHalfClosedRemote
	case StreamStateHalfClosedLocal:
		s.state = StreamStateClosed
		s.closeStreamResourcesProtected()
	default:
		s.conn.logger.Error("stream: END_STREAM received in unexpected state", logger.LogFields{"stream_id": s.id, "state": s.state.String()})
		s.state = StreamStateClosed
		s.closeStreamResourcesProtected()
	}
}

// transitionStateOnSendEndStream updates stream state when END_STREAM is sent.
func (s *Stream) transitionStateOnSendEndStream() {
	// Assumes s.mu is held by caller.
	oldState := s.state
	newState := oldState

	switch oldState {
	case StreamStateOpen:
		newState = StreamStateHalfClosedLocal
	case StreamStateHalfClosedRemote:
		newState = StreamStateClosed
	default:
		s.conn.logger.Error("stream: END_STREAM sent in unexpected state", logger.LogFields{"stream_id": s.id, "state": oldState.String()})
		newState = StreamStateClosed // Force closed on error
	}
	s._setState(newState)
}

// _setState transitions the stream to a new state.
// THIS IS A LOW-LEVEL METHOD. Callers MUST ensure the transition is valid.
// It handles resource cleanup if transitioning to Closed.
// Assumes s.mu is held by the caller.
func (s *Stream) _setState(newState StreamState) {
	if s.state == newState && newState != StreamStateClosed {
		// No change, unless re-closing to ensure cleanup (which this path doesn't explicitly handle,
		// callers ensure closeStreamResourcesProtected is called once effectively).
		return
	}

	if s.state == StreamStateClosed && newState != StreamStateClosed {
		// Cannot transition out of closed state, except to re-affirm closed (e.g. multiple RSTs)
		// This check prevents accidental re-opening.
		s.conn.logger.Warn("stream: attempt to transition out of closed state ignored", logger.LogFields{
			"stream_id": s.id,
			"old_state": s.state.String(),
			"new_state": newState.String(),
		})
		return
	}

	s.conn.logger.Debug("stream: state transition", logger.LogFields{
		"stream_id": s.id,
		"old_state": s.state.String(),
		"new_state": newState.String(),
	})
	oldState := s.state
	s.state = newState

	if newState == StreamStateClosed && oldState != StreamStateClosed {
		// Call cleanup only on the first transition to closed.
		s.closeStreamResourcesProtected()
	}
}

// closeStreamResourcesProtected performs cleanup when stream transitions to fully closed state.
// This version assumes s.mu is ALREADY HELD by the caller.
// It is responsible for ensuring cleanup happens once.
func (s *Stream) closeStreamResourcesProtected() {
	// Capture values needed after unlock, or for non-blocking operations
	cancelCtxFunc := s.cancelCtx
	pipeWriter := s.requestBodyWriter
	// pipeReader := s.requestBodyReader // Reader usually closes itself when writer closes or EOF
	streamID := s.id
	conn := s.conn

	var rstCode ErrorCode = ErrCodeNoError // Default for graceful closure
	if s.pendingRSTCode != nil {
		rstCode = *s.pendingRSTCode
	}

	// These are safe to call while holding lock
	if cancelCtxFunc != nil {
		s.cancelCtx = nil // Avoid double call
		cancelCtxFunc()
	}

	if pipeWriter != nil {
		s.requestBodyWriter = nil                                  // Avoid double call
		if rstCode != ErrCodeNoError && rstCode != ErrCodeCancel { // CANCEL is also a somewhat graceful closure from app perspective
			pipeWriter.CloseWithError(NewStreamError(streamID, rstCode, "stream closed due to RST"))
		} else {
			// For NO_ERROR or CANCEL, just close the pipe. The reader will see EOF.
			pipeWriter.Close()
		}
	}
	// if pipeReader != nil {
	// s.requestBodyReader = nil
	// pipeReader.Close() // Reader generally closes when writer does.
	// }

	if conn != nil {
		// This needs to be scheduled to run after the current critical section (s.mu lock)
		// if conn.removeStream might re-acquire s.mu or perform blocking operations.
		// For now, assume direct call is okay or conn.removeStream is designed to handle this.
		// A common pattern is to send this to a channel processed by the connection's main loop.
		go conn.removeStream(streamID, rstCode)
	}
}

// Called by connection to update stream priority based on a PRIORITY frame.
func (s *Stream) processPriorityUpdate(depStreamID uint32, weight uint8, exclusive bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.priorityParentID = depStreamID
	s.priorityWeight = weight
	s.priorityExclusive = exclusive
	// Note: This only updates the stream's record. The connection's PriorityTree
	// is updated by the connection itself when it processes the PRIORITY frame or
	// priority info from HEADERS.
}

// handlePriorityFrame is called by the connection when a PRIORITY frame is received for this stream.
// It validates the frame in the context of this stream and then delegates the priority
// update logic to the connection's PriorityTree.
func (s *Stream) handlePriorityFrame(frame *PriorityFrame) error {
	s.conn.logger.Debug("stream: received PRIORITY frame", logger.LogFields{
		"stream_id":        s.id,
		"frame_stream_id":  frame.Header().StreamID, // Should match s.id if dispatched correctly
		"frame_dependency": frame.StreamDependency,
		"frame_weight":     frame.Weight,
		"frame_exclusive":  frame.Exclusive,
	})

	// The PRIORITY frame's StreamID in its header MUST be non-zero.
	// This is checked by PriorityTree.ProcessPriorityFrame, which returns ConnectionError(PROTOCOL_ERROR).
	// If frame.Header().StreamID != s.id, it's a dispatch error by the connection layer.
	if frame.Header().StreamID != s.id {
		msg := fmt.Sprintf("PRIORITY frame stream ID %d in frame header does not match stream object ID %d", frame.Header().StreamID, s.id)
		s.conn.logger.Error("stream: PRIORITY frame ID mismatch (server dispatch error)", logger.LogFields{
			"stream_id":       s.id,
			"frame_stream_id": frame.Header().StreamID,
			"error_msg":       msg,
		})
		// This is an internal server error, not a client protocol violation directly related to this stream's PRIORITY rules.
		return NewConnectionError(ErrCodeInternalError, msg)
	}

	// RFC 7540, Section 6.3: "A PRIORITY frame can be sent on a stream in any state..."
	// No explicit state check needed here for CanReceiveFrame(FramePriority) as it's always allowed.

	if s.conn.priorityTree == nil {
		s.conn.logger.Warn("stream: priority tree not available on connection, PRIORITY frame ignored",
			logger.LogFields{"stream_id": s.id})
		return nil // Effectively ignored if no priority system is in place.
	}

	// Delegate to the connection's priority tree to handle the update.
	// PriorityTree.ProcessPriorityFrame handles:
	// - ConnectionError(PROTOCOL_ERROR) if frame.Header().StreamID == 0 (though dispatcher should prevent).
	// - StreamError(PROTOCOL_ERROR) if frame.Header().StreamID attempts to depend on itself.
	err := s.conn.priorityTree.ProcessPriorityFrame(frame)
	if err != nil {
		s.conn.logger.Error("stream: error processing PRIORITY frame via PriorityTree", logger.LogFields{
			"stream_id": s.id,
			"error":     err.Error(),
		})

		// If it's a StreamError specific to *this* stream (e.g., self-dependency from frame content),
		// the stream should be reset.
		if streamErr, ok := err.(*StreamError); ok && streamErr.StreamID == s.id {
			s.sendRSTStream(streamErr.Code) // This will handle state transition and cleanup.
		}
		// The original error (StreamError or ConnectionError) is returned to the connection
		// so it can decide on further actions (e.g., GOAWAY for ConnectionError).
		return err
	}

	// If the priority tree update was successful, update the stream's local cache of its priority.
	// This stream object might store priority info for local reference, but PriorityTree is authoritative.
	s.processPriorityUpdate(frame.StreamDependency, frame.Weight, frame.Exclusive)

	s.conn.logger.Debug("stream: PRIORITY frame processed successfully", logger.LogFields{"stream_id": s.id})
	return nil
}

// handleWindowUpdateFrame is called by the connection when a WINDOW_UPDATE frame is received for this stream.
// It updates the stream's send flow control window.
func (s *Stream) handleWindowUpdateFrame(frame *WindowUpdateFrame) error {
	s.conn.logger.Debug("stream: received WINDOW_UPDATE frame", logger.LogFields{
		"stream_id":       s.id,
		"frame_stream_id": frame.Header().StreamID, // Should match s.id
		"increment":       frame.WindowSizeIncrement,
	})

	// Validate that the frame's StreamID matches the stream's ID.
	// This should be guaranteed by the dispatcher in the connection layer.
	if frame.Header().StreamID != s.id {
		msg := fmt.Sprintf("WINDOW_UPDATE frame stream ID %d in frame header does not match stream object ID %d", frame.Header().StreamID, s.id)
		s.conn.logger.Error("stream: WINDOW_UPDATE frame ID mismatch (server dispatch error)", logger.LogFields{
			"stream_id":       s.id,
			"frame_stream_id": frame.Header().StreamID,
			"error_msg":       msg,
		})
		// This is an internal server error if dispatch was incorrect.
		return NewConnectionError(ErrCodeInternalError, msg)
	}

	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()

	// RFC 7540, Section 5.1.1: For "closed" state:
	// "WINDOW_UPDATE or RST_STREAM frames can be received in this state for a short period after a DATA or HEADERS
	// frame containing an END_STREAM flag is sent. ... Endpoints MUST ignore WINDOW_UPDATE ... frames received in this state..."
	if state == StreamStateClosed {
		s.conn.logger.Debug("stream: WINDOW_UPDATE received on closed stream, ignoring", logger.LogFields{"stream_id": s.id})
		return nil // Ignore as per RFC
	}

	// Attempt to increase the send window.
	// s.fcManager.HandleWindowUpdateFromPeer calls s.sendWindow.Increase().
	// s.sendWindow.Increase() handles:
	// - StreamError(PROTOCOL_ERROR) if increment is 0 for a stream.
	// - StreamError(FLOW_CONTROL_ERROR) if stream window overflows.
	err := s.fcManager.HandleWindowUpdateFromPeer(frame.WindowSizeIncrement)
	if err != nil {
		s.conn.logger.Error("stream: error processing WINDOW_UPDATE frame via StreamFlowControlManager", logger.LogFields{
			"stream_id": s.id,
			"increment": frame.WindowSizeIncrement,
			"error":     err.Error(),
		})

		// If the error is a StreamError, RST the stream.
		if streamErr, ok := err.(*StreamError); ok {
			// Ensure the StreamError is indeed for *this* stream.
			if streamErr.StreamID == s.id {
				s.sendRSTStream(streamErr.Code)
				// The original error (which is a StreamError) is returned for the connection
				// to know, but the stream is already being reset.
				return streamErr
			}
			// This would be an unexpected situation.
			connErr := NewConnectionError(ErrCodeInternalError,
				fmt.Sprintf("WINDOW_UPDATE on stream %d resulted in StreamError for different stream %d: %s",
					s.id, streamErr.StreamID, streamErr.Msg))
			return connErr
		}

		// If it's any other type of error (e.g., ConnectionError), propagate it.
		return err
	}

	s.conn.logger.Debug("stream: WINDOW_UPDATE frame processed successfully", logger.LogFields{"stream_id": s.id, "increment": frame.WindowSizeIncrement})
	return nil
}

// Context returns the stream's context.
func (s *Stream) Context() context.Context {
	return s.ctx
}

// ID returns the stream's ID.
func (s *Stream) ID() uint32 {
	return s.id
}

// State returns the stream's current state.
func (s *Stream) State() StreamState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// RequestBody returns an io.Reader for the request body.
func (s *Stream) RequestBody() io.Reader {
	return s.requestBodyReader
}

// RequestHeaders returns the received request headers.
// The returned slice should not be modified by the caller.
func (s *Stream) RequestHeaders() []hpack.HeaderField {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Return a copy to prevent modification of internal slice by handler
	headersCopy := make([]hpack.HeaderField, len(s.requestHeaders))
	copy(headersCopy, s.requestHeaders)
	return headersCopy
}

// GetHandlerConfig returns the raw JSON message for the handler's config.
// The handler is responsible for unmarshalling this.
func (s *Stream) GetHandlerConfig() json.RawMessage {
	// s.handlerConfig is immutable after stream creation, so direct return is fine.
	return s.handlerConfig
}

// ExclusiveStreamDependency holds priority information as specified in HEADERS or PRIORITY frames.
type ExclusiveStreamDependency struct {
	StreamDependency uint32
	Weight           uint8
	Exclusive        bool
}

// CanSendFrame checks if a frame of the given type can be sent from the current stream state.
// This does not check if the frame itself is valid (e.g., flags, payload).
// Reference: RFC 7540, Section 5.1.
func (s *Stream) CanSendFrame(ft FrameType) bool {
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()

	switch state {
	case StreamStateIdle:
		// Technically, a stream object shouldn't exist in "idle" state before first frame.
		// This case is more for theoretical validation if one were to check before creating.
		return ft == FrameHeaders || ft == FramePushPromise
	case StreamStateReservedLocal:
		return ft == FrameHeaders || ft == FrameRSTStream || ft == FramePriority || ft == FrameWindowUpdate
	case StreamStateReservedRemote:
		return ft == FrameRSTStream || ft == FramePriority || ft == FrameWindowUpdate
	case StreamStateOpen:
		return true // Any frame type
	case StreamStateHalfClosedLocal: // We sent END_STREAM.
		return ft == FrameWindowUpdate || ft == FramePriority || ft == FrameRSTStream
	case StreamStateHalfClosedRemote: // Peer sent END_STREAM. We can still send.
		return true // Any frame type (until we send END_STREAM)
	case StreamStateClosed:
		// Per RFC 7540, Section 5.1:
		// "PRIORITY frames can be sent on a stream in the "closed" state."
		// "An endpoint that receives any frame other than PRIORITY after receiving RST_STREAM
		// MUST treat that as a stream error (Section 5.4.2) of type STREAM_CLOSED."
		// "WINDOW_UPDATE or RST_STREAM frames can be received in this state [closed] for a short
		// period after a DATA or HEADERS frame containing an END_STREAM flag is sent. Until the
		// remote peer receives and processes RST_STREAM or the frame bearing the END_STREAM flag,
		// it might send frames of these types. Endpoints MUST ignore WINDOW_UPDATE or RST_STREAM
		// frames received in this state, though endpoints MAY choose to treat frames that arrive
		// excessively late as a connection error (Section 5.4.1) of type PROTOCOL_ERROR."
		// So, sending PRIORITY is okay. Sending others is generally not.
		return ft == FramePriority
	default:
		s.conn.logger.Error("stream: CanSendFrame called in unknown state", logger.LogFields{"stream_id": s.id, "state_val": uint8(state)})
		return false
	}
}

// CanReceiveFrame checks if a frame of the given type can be received in the current stream state
// without it being an immediate protocol error that requires connection termination.
// Note: Some frames might be ignored or lead to stream-level errors (like STREAM_CLOSED)
// even if this function returns true (e.g. WINDOW_UPDATE on a closed stream is ignored).
// Reference: RFC 7540, Section 5.1.
func (s *Stream) CanReceiveFrame(ft FrameType) bool {
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()

	switch state {
	case StreamStateIdle:
		// From connection perspective: HEADERS or PUSH_PROMISE expected to open/reserve.
		// Other frames on an idle stream ID -> connection error PROTOCOL_ERROR.
		// This function is for an existing stream object, which shouldn't be IDLE if active.
		return ft == FrameHeaders || ft == FramePushPromise
	case StreamStateReservedLocal: // We sent PUSH_PROMISE.
		// Client can send RST_STREAM. Other frames from client on this stream are protocol error.
		// However, PRIORITY or WINDOW_UPDATE from client are typically for streams client opens.
		// RFC 8441 (Bootstrapping WebSockets with HTTP/2) uses PUSH_PROMISE then client sends HEADERS on pushed stream.
		// RFC 7540 5.1 states for Reserved (Local):
		//   - send PUSH_PROMISE -> reserved (local)
		//   - send HEADERS -> half-closed (remote) [if END_STREAM] or open
		//   - recv RST_STREAM -> closed
		// This implies the peer (client) can send RST_STREAM.
		// For frames received *by server* on a stream it PUSH_PROMISEd:
		// RFC 7540 Section 6.6: "The PUSH_PROMISE frame ... reserves the promised stream for later use."
		// "...the server can send a HEADERS frame to begin sending the pushed resource."
		// If client sends HEADERS on a stream *it did not initiate*, it's a protocol error.
		// If client sends on a stream server promised but hasn't sent HEADERS for yet, it's tricky.
		// Generally, client should not send on server-initiated streams until server sends HEADERS.
		// Thus, only RST_STREAM is safe for client to send.
		// However, the state table also allows receiving PRIORITY/WINDOW_UPDATE.
		// These usually apply to streams the receiver has some concept of being active.
		// Let's be conservative based on who initiates:
		// if server PUSH_PROMISE (state=ReservedLocal for server), client should only send RST_STREAM.
		// The state table in 5.1 is generic.
		// "An endpoint MUST NOT send frames other than PUSH_PROMISE on a stream that is in the "idle" state."
		// "reserved (local)": "An endpoint might receive PRIORITY or WINDOW_UPDATE frames in this state."
		// So, RFC 7540 allows these.
		return ft == FrameRSTStream || ft == FramePriority || ft == FrameWindowUpdate
	case StreamStateReservedRemote: // Peer sent PUSH_PROMISE. We (client) expect HEADERS from peer.
		// If we are server, and client sent PUSH_PROMISE (not allowed for client).
		// Assuming this state means WE received PUSH_PROMISE.
		//   - recv PUSH_PROMISE -> reserved (remote)
		//   - recv HEADERS -> half-closed (local) [if END_STREAM] or open
		//   - send RST_STREAM -> closed
		// We can receive HEADERS, RST_STREAM, PRIORITY, WINDOW_UPDATE.
		return ft == FrameHeaders || ft == FrameRSTStream || ft == FramePriority || ft == FrameWindowUpdate
	case StreamStateOpen:
		return true // Any frame type
	case StreamStateHalfClosedLocal: // We sent END_STREAM. Peer can still send anything.
		return true // Any frame type
	case StreamStateHalfClosedRemote: // Peer sent END_STREAM.
		// We can send: WINDOW_UPDATE, PRIORITY, RST_STREAM.
		// We can receive: WINDOW_UPDATE, PRIORITY, RST_STREAM.
		// Receiving DATA, HEADERS, CONTINUATION, PUSH_PROMISE would be STREAM_CLOSED error.
		return ft == FrameWindowUpdate || ft == FramePriority || ft == FrameRSTStream
	case StreamStateClosed:
		// Can receive PRIORITY (processed).
		// Can receive WINDOW_UPDATE, RST_STREAM (ignored if late, unless triggers PROTOCOL_ERROR for excessive lateness).
		// Others: Stream error STREAM_CLOSED or Connection error PROTOCOL_ERROR.
		return ft == FramePriority || ft == FrameWindowUpdate || ft == FrameRSTStream
	default:
		s.conn.logger.Error("stream: CanReceiveFrame called in unknown state", logger.LogFields{"stream_id": s.id, "state_val": uint8(state)})
		return false
	}
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

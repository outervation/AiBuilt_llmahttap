package http2

import (
	"context"
	"encoding/json"
	"io"
	"net/http" // For http.Header, if we decide to use it directly in Request struct
	"net/url"  // For url.URL
	"strings"  // For pseudo-header check
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
	// WriteHeaders sends response headers.
	// If endStream is true, this also signals the end of the response body (e.g., for HEAD requests or empty bodies).
	WriteHeaders(headers []hpack.HeaderField, endStream bool) error

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
	requestBodyReader *io.PipeReader  // For handler to read incoming DATA frames
	requestBodyWriter *io.PipeWriter  // For stream to write incoming DATA frames into
	handler           Handler         // Handler responsible for this stream
	handlerConfig     json.RawMessage // Configuration for the handler

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
	// TODO: Add fields for received trailers from client, if supported/needed.
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
) *Stream {
	ctx, cancel := context.WithCancel(conn.ctx) // Assuming conn has a base context

	pr, pw := io.Pipe()

	s := &Stream{
		id:                          id,
		state:                       StreamStateIdle, // Will transition upon receiving/sending HEADERS
		conn:                        conn,
		fcManager:                   NewStreamFlowControlManager(id, initialOurWindowSize, initialPeerWindowSize),
		priorityWeight:              prioWeight,   // Default might be 16 (frame value 15)
		priorityParentID:            prioParentID, // Default might be 0
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
	// s.dataAvailable = sync.NewCond(&s.mu)
	return s
}

// Implement ResponseWriter for Stream
// Note: These methods will need to interact with the connection to send frames.

// WriteHeaders sends response headers.
func (s *Stream) WriteHeaders(headers []hpack.HeaderField, endStream bool) error {
	s.mu.Lock()
	// defer s.mu.Unlock() // Unlock must be carefully managed due to calls to conn

	if s.responseHeadersSent {
		s.mu.Unlock()
		s.conn.logger.Error("stream: WriteHeaders called after headers already sent", logger.LogFields{"stream_id": s.id})
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
	s.mu.Lock()
	if !s.responseHeadersSent {
		s.mu.Unlock()
		s.conn.logger.Error("stream: WriteData called before WriteHeaders", logger.LogFields{"stream_id": s.id})
		return 0, NewStreamError(s.id, ErrCodeInternalError, "WriteHeaders must be called before WriteData")
	}
	if (s.state == StreamStateHalfClosedLocal && s.endStreamSentToClient) || s.state == StreamStateClosed || s.pendingRSTCode != nil {
		s.mu.Unlock()
		errCode := ErrCodeStreamClosed
		if s.pendingRSTCode != nil {
			errCode = *s.pendingRSTCode
		}
		return 0, NewStreamError(s.id, errCode, "cannot send data on closed, reset, or already ended stream")
	}
	s.mu.Unlock()

	if len(p) == 0 && !endStream {
		return 0, nil // Nothing to send, not ending stream.
	}

	bytesWritten, err := s.conn.sendDataFrame(s, p, endStream) // This method in conn must handle its own locking.

	s.mu.Lock() // Re-lock to update stream state
	defer s.mu.Unlock()

	if err != nil {
		// If sending data failed, don't update stream state based on send.
		return bytesWritten, err
	}

	if endStream {
		s.endStreamSentToClient = true
		s.transitionStateOnSendEndStream() // Update state based on sending END_STREAM
	}
	return bytesWritten, nil
}

// WriteTrailers sends trailing headers.
func (s *Stream) WriteTrailers(trailers []hpack.HeaderField) error {
	s.mu.Lock()
	if !s.responseHeadersSent {
		s.mu.Unlock()
		s.conn.logger.Error("stream: WriteTrailers called before WriteHeaders", logger.LogFields{"stream_id": s.id})
		return NewStreamError(s.id, ErrCodeInternalError, "WriteHeaders must be called before WriteTrailers")
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

// processIncomingHeaders is called by the connection when HEADERS (+ CONTINUATION) are received for this stream.
func (s *Stream) processIncomingHeaders(headers []hpack.HeaderField, endStream bool, hasPriority bool, prio ExclusiveStreamDependency) error {
	s.mu.Lock()

	initialState := s.state
	isTrailer := false

	if initialState == StreamStateOpen || initialState == StreamStateHalfClosedLocal {
		if s.endStreamReceivedFromClient {
			s.mu.Unlock()
			s.conn.logger.Error("stream: received headers after END_STREAM already processed for request", logger.LogFields{"stream_id": s.id})
			s.sendRSTStream(ErrCodeStreamClosed)
			return NewStreamError(s.id, ErrCodeStreamClosed, "received headers after request end stream")
		}
		isTrailer = true // These are trailers
		s.conn.logger.Info("stream: received trailer headers", logger.LogFields{"stream_id": s.id, "num_trailers": len(headers)})
		// TODO: Handle trailers: store them, validate, etc. s.requestTrailers = headers
	} else if initialState != StreamStateIdle && initialState != StreamStateReservedRemote {
		s.mu.Unlock()
		s.conn.logger.Error("stream: received headers in invalid state", logger.LogFields{"stream_id": s.id, "state": s.state.String()})
		s.sendRSTStream(ErrCodeStreamClosed)
		return NewStreamError(s.id, ErrCodeStreamClosed, "received headers in invalid state "+s.state.String())
	}

	if !isTrailer {
		s.requestHeaders = headers
	} // Else, store in a separate field for trailers if needed.

	if initialState == StreamStateIdle || initialState == StreamStateReservedRemote {
		// Transition to open. This path is for initial HEADERS.
		// Trailers path (initialState == Open or HalfClosedLocal) doesn't change to Open.
		if !isTrailer {
			s._setState(StreamStateOpen)
		}
	}

	if hasPriority && !isTrailer { // Priority info only on initial HEADERS
		s.priorityParentID = prio.StreamDependency
		s.priorityWeight = prio.Weight
		s.priorityExclusive = prio.Exclusive
		s.conn.updateStreamPriority(s.id, prio.StreamDependency, prio.Weight, prio.Exclusive)
	}

	if endStream {
		s.endStreamReceivedFromClient = true
		if s.requestBodyWriter != nil {
			s.requestBodyWriter.Close()
		}
		s.transitionStateOnReceiveEndStream()
	}
	s.mu.Unlock()

	if !isTrailer && len(s.requestHeaders) > 0 {
		go s.runHandler()
	}

	return nil
}

// processIncomingData is called by the connection when a DATA frame is received.
func (s *Stream) processIncomingData(data []byte, endStream bool) error {
	s.mu.Lock()
	if s.endStreamReceivedFromClient {
		s.mu.Unlock()
		s.conn.logger.Error("stream: received DATA frame after END_STREAM", logger.LogFields{"stream_id": s.id})
		s.sendRSTStream(ErrCodeStreamClosed)
		return NewStreamError(s.id, ErrCodeStreamClosed, "received DATA frame after END_STREAM")
	}

	if s.state != StreamStateOpen && s.state != StreamStateHalfClosedLocal {
		s.mu.Unlock()
		s.conn.logger.Error("stream: received DATA frame in invalid state", logger.LogFields{"stream_id": s.id, "state": s.state.String()})
		s.sendRSTStream(ErrCodeStreamClosed)
		return NewStreamError(s.id, ErrCodeStreamClosed, "received DATA frame in invalid state "+s.state.String())
	}

	pipeWriter := s.requestBodyWriter
	s.mu.Unlock() // Unlock before writing to pipe

	if pipeWriter != nil {
		_, err := pipeWriter.Write(data)
		if err != nil {
			s.conn.logger.Error("stream: error writing to request body pipe", logger.LogFields{"stream_id": s.id, "error": err.Error()})
			s.sendRSTStream(ErrCodeCancel)
			return NewStreamError(s.id, ErrCodeInternalError, "error writing to request body pipe: "+err.Error())
		}
	}

	s.mu.Lock() // Re-acquire lock for state update
	if endStream {
		s.endStreamReceivedFromClient = true
		if pipeWriter != nil { // Should be s.requestBodyWriter
			pipeWriter.Close()
		}
		s.transitionStateOnReceiveEndStream()
	}
	s.mu.Unlock()
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

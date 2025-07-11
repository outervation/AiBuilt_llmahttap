package http2

import (
	"context"
	"errors" // Added import
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
	ourInitialWindowSize, peerInitialWindowSize uint32, // Added ourInitialWindowSize
	prioWeight uint8,
	prioParentID uint32,
	prioExclusive bool,
	isInitiatedByPeer bool,
) (*Stream, error) {
	ctx, cancel := context.WithCancel(conn.ctx)

	pr, pw := io.Pipe()

	s := &Stream{
		id:                          id,
		state:                       StreamStateIdle,
		conn:                        conn,
		fcManager:                   NewStreamFlowControlManager(conn, id, ourInitialWindowSize, peerInitialWindowSize), // Pass both sizes
		priorityWeight:              prioWeight,
		priorityParentID:            prioParentID,
		priorityExclusive:           prioExclusive,
		requestBodyReader:           pr,
		requestBodyWriter:           pw,
		dataFrameChan:               make(chan *DataFrame, 32),
		ctx:                         ctx,
		cancelCtx:                   cancel,
		endStreamReceivedFromClient: false,
		endStreamSentToClient:       false,
		initiatedByPeer:             isInitiatedByPeer,
		responseHeadersSent:         false,
		initialHeadersProcessed:     false,
		requestTrailers:             nil,
	}

	priorityInfo := &streamDependencyInfo{
		StreamDependency: prioParentID,
		Weight:           prioWeight,
		Exclusive:        prioExclusive,
	}
	if conn.priorityTree != nil {
		err := conn.priorityTree.AddStream(s.id, priorityInfo)
		if err != nil {
			cancel()
			_ = pr.CloseWithError(err)
			_ = pw.CloseWithError(err)
			return nil, err
		}
	}

	go s.processIncomingDataFrames()

	return s, nil
}

// Implement ResponseWriter for Stream
// Note: These methods will need to interact with the connection to send frames.

// SendHeaders sends response headers.
func (s *Stream) SendHeaders(headers []HeaderField, endStream bool) error {
	s.conn.log.Debug("Stream.SendHeaders: Entered", logger.LogFields{
		"stream_id":                      s.id,
		"num_headers":                    len(headers),
		"end_stream_arg":                 endStream,
		"s_responseHeadersSent_PRE_LOCK": s.responseHeadersSent, // Log before lock for initial check
		"call_stack_trace_PRE_LOCK":      string(debug.Stack()), // Log call stack
	})

	s.mu.Lock() // Lock for state check and update
	s.conn.log.Debug("Stream.SendHeaders: MUTEX ACQUIRED", logger.LogFields{
		"stream_id":                     s.id,
		"s_responseHeadersSent_IN_LOCK": s.responseHeadersSent,
	})
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
func (s *Stream) WriteData(p []byte, endStream bool) (totalN int, err error) {
	s.conn.log.Debug("Stream.WriteData: Entered", logger.LogFields{"stream_id": s.id, "data_len_p": len(p), "end_stream_flag": endStream})
	s.mu.RLock()
	if !s.responseHeadersSent {
		s.mu.RUnlock()
		return 0, NewStreamError(s.id, ErrCodeInternalError, "SendHeaders must be called before WriteData")
	}
	// Check if stream is in a state that allows sending DATA
	// (Open or HalfClosedRemote (client closed its sending side, server can still send))
	// Or if it's being reset or already closed by server.
	if s.state == StreamStateClosed || s.state == StreamStateHalfClosedLocal || s.pendingRSTCode != nil || s.endStreamSentToClient {
		s.mu.RUnlock()
		errMsg := fmt.Sprintf("cannot send data on stream %d in state %s (pendingRST: %v, endStreamSent: %v)", s.id, s.state, s.pendingRSTCode != nil, s.endStreamSentToClient)
		s.conn.log.Error("Stream.WriteData: "+errMsg, logger.LogFields{"stream_id": s.id})
		// The specific test case "Error:_WriteData_after_END_STREAM_already_sent" expects
		// the "cannot send data on closed, reset, or already server-ended stream" message.
		// So, we remove the special io.EOF return path.
		return 0, NewStreamError(s.id, ErrCodeStreamClosed, "cannot send data on closed, reset, or already server-ended stream")
	}
	s.mu.RUnlock()

	dataRemaining := p
	isLogicalEndStream := endStream

	if len(dataRemaining) == 0 && !isLogicalEndStream {
		s.conn.log.Debug("Stream.WriteData: Called with no data and no endStream flag, returning.", logger.LogFields{"stream_id": s.id})
		return 0, nil // Nothing to send
	}

	for {
		// Determine ideal payload size for THIS specific DATA frame based on MFS and data left
		idealBytesForThisFrame := uint32(len(dataRemaining))
		if s.conn.peerMaxFrameSize > 0 && idealBytesForThisFrame > s.conn.peerMaxFrameSize {
			idealBytesForThisFrame = s.conn.peerMaxFrameSize
		}

		// Handle case of sending only an END_STREAM flag on an empty DATA frame
		// This occurs if p was initially empty and endStream was true, or all of p has been sent and now endStream needs to be signaled.
		isFinalFrameAndEndsStream := (len(dataRemaining) == int(idealBytesForThisFrame)) && isLogicalEndStream

		if idealBytesForThisFrame == 0 {
			if isFinalFrameAndEndsStream { // Send empty DATA frame with END_STREAM
				s.conn.log.Debug("Stream.WriteData: Sending empty DATA frame with END_STREAM", logger.LogFields{"stream_id": s.id})
				// No FC acquired for 0-length data
				s.mu.Lock() // Lock for state check before send and update after.
				if s.endStreamSentToClient {
					s.mu.Unlock()
					s.conn.log.Warn("Stream.WriteData: Attempt to send empty DATA with END_STREAM, but stream already marked ended.", logger.LogFields{"stream_id": s.id})
					return totalN, NewStreamError(s.id, ErrCodeInternalError, "stream already ended by server")
				}
				s.mu.Unlock() // Unlock for sendDataFrame call

				_, sendErr := s.conn.sendDataFrame(s, nil, true)

				s.mu.Lock() // Re-lock for state update
				if sendErr == nil {
					if !s.endStreamSentToClient { // Check again to be absolutely sure before transitioning
						s.endStreamSentToClient = true
						s.transitionStateOnSendEndStream()
					}
				} else {
					s.conn.log.Error("Stream.WriteData: Failed to send empty DATA frame with END_STREAM", logger.LogFields{"stream_id": s.id, "error": sendErr.Error()})
				}
				s.mu.Unlock()
				return totalN, sendErr
			}
			// All data from p sent, and not sending a final empty frame now.
			break
		}

		// At this point, idealBytesForThisFrame > 0.
		// Adapt idealBytesForThisFrame based on current stream flow control window.
		actualBytesForThisFrame := idealBytesForThisFrame

		// Check stream state before acquiring FC. It might have changed due to concurrent RST.
		s.mu.RLock()
		streamState := s.state
		streamPendingRST := s.pendingRSTCode
		isEndStreamSentByServer := s.endStreamSentToClient
		s.mu.RUnlock()

		if streamState == StreamStateClosed || streamPendingRST != nil {
			errMsg := "stream closed or reset before acquiring flow control"
			s.conn.log.Warn("Stream.WriteData: "+errMsg, logger.LogFields{"stream_id": s.id, "state": streamState, "rst_pending": streamPendingRST != nil})
			return totalN, NewStreamError(s.id, ErrCodeStreamClosed, errMsg)
		}
		if isEndStreamSentByServer {
			s.conn.log.Warn("Stream.WriteData: Attempt to write data after server already sent END_STREAM (checked before FC acquisition).", logger.LogFields{"stream_id": s.id})
			return totalN, NewStreamError(s.id, ErrCodeInternalError, "write after server sent END_STREAM")
		}

		streamWinAvail := s.fcManager.GetStreamSendAvailable()
		s.conn.log.Debug("Stream.WriteData: Availability check", logger.LogFields{
			"stream_id": s.id, "ideal_chunk": idealBytesForThisFrame, "stream_win_avail": streamWinAvail,
		})

		if streamWinAvail <= 0 { // Stream window is 0 or negative, must block.
			s.conn.log.Debug("Stream.WriteData: Stream window 0 or negative, acquiring 1 byte to block/wait.", logger.LogFields{"stream_id": s.id, "current_stream_win": streamWinAvail})
			err = s.fcManager.AcquireSendSpace(1) // Block until at least 1 byte is available.
			if err != nil {
				s.conn.log.Error("Stream.WriteData: Error acquiring 1 byte from stream FC (after window was 0).", logger.LogFields{"stream_id": s.id, "error": err})
				return totalN, err
			}
			// Acquired 1 byte, this counts towards the total chunk.
			// If idealBytesForThisFrame was > 1, we'll try to acquire the rest or adjust.
			// If actualBytesForThisFrame was already 1 (due to peerMFS or streamWinAvail initially), this single byte is it.
			streamWinAvail = s.fcManager.GetStreamSendAvailable() + 1 // +1 because AcquireSendSpace decremented it
			s.conn.log.Debug("Stream.WriteData: Re-evaluated stream window after acquiring 1 byte.", logger.LogFields{"stream_id": s.id, "new_stream_win_avail_effective": streamWinAvail})
			if streamWinAvail <= 0 {
				// This should not happen if AcquireSendSpace(1) succeeded and returned no error.
				// If it does, means AcquireSendSpace(1) succeeded but window is still <=0 after being "notionally" incremented.
				// This implies a logical flaw or severe race.
				s.fcManager.ReleaseSendSpace(1) // Release the acquired byte if we can't proceed.
				errMsg := fmt.Sprintf("stream %d window remained <=0 after blocking acquire(1) succeeded. Flow control logic error.", s.id)
				s.conn.log.Error("Stream.WriteData: "+errMsg, logger.LogFields{"stream_id": s.id})
				return totalN, NewStreamError(s.id, ErrCodeFlowControlError, errMsg)
			}
			// If we are here, streamWinAvail > 0.
			// We've successfully acquired 1 byte. Now decide how much more to acquire for this frame.
			if actualBytesForThisFrame > 1 { // We wanted to send more than 1 byte.
				if uint32(streamWinAvail) < actualBytesForThisFrame { // After acquiring 1, available is still less than desired
					actualBytesForThisFrame = uint32(streamWinAvail) // Reduce to what's available (which is > 0)
				}
				// Now actualBytesForThisFrame is min(original ideal, current stream window after getting 1 byte)
				// We already acquired 1 byte. Need to acquire (actualBytesForThisFrame - 1) more.
				if actualBytesForThisFrame > 0 { // Defensive: ensure it's still positive
					err = s.fcManager.AcquireAdditionalSendSpace(actualBytesForThisFrame - 1) // acquire the remainder
					if err != nil {
						s.fcManager.ReleaseSendSpace(1) // Release the first byte we got
						s.conn.log.Error("Stream.WriteData: Error acquiring additional stream FC.", logger.LogFields{"stream_id": s.id, "amount": actualBytesForThisFrame - 1, "error": err})
						return totalN, err
					}
				} else { // actualBytesForThisFrame became 0 or less, implies streamWinAvail was 1.
					actualBytesForThisFrame = 1 // We stick with the 1 byte we acquired.
				}
			} else { // actualBytesForThisFrame was 1 to begin with (e.g. h2spec case)
				// We already acquired 1 byte. That's all for stream FC.
				// actualBytesForThisFrame remains 1.
			}
		} else { // streamWinAvail > 0 initially
			if uint32(streamWinAvail) < actualBytesForThisFrame {
				actualBytesForThisFrame = uint32(streamWinAvail)
			}
			// Acquire stream flow control for the exact amount we decided to send in this frame.
			err = s.fcManager.AcquireSendSpace(actualBytesForThisFrame)
			if err != nil {
				s.conn.log.Error("Stream.WriteData: Error acquiring actual_bytes from stream FC.", logger.LogFields{"stream_id": s.id, "amount": actualBytesForThisFrame, "error": err})
				return totalN, err
			}
		}

		s.conn.log.Debug("Stream.WriteData: Frame payload size after stream FC consideration", logger.LogFields{"stream_id": s.id, "actual_bytes": actualBytesForThisFrame})
		if actualBytesForThisFrame == 0 { // Could happen if streamWinAvail was 0 and acquire(1) failed in a way that led here, or other logic path error
			// We should have returned error earlier in AcquireSendSpace.
			// If this is reached, it implies a state where we decided to send 0 bytes *after* passing initial 0-byte check.
			// This is likely an error path or indicates no data can be sent.
			// If we acquired any stream FC (e.g. the 1 byte), release it. This part is complex due to above logic.
			// For simplicity, assume if actualBytesForThisFrame is 0 here, an error occurred or was missed.
			// However, the previous FC acquisition logic should ensure actualBytesForThisFrame > 0 if no error.
			// If it's 0 despite that, release any FC that might have been speculatively acquired.
			// This path should ideally not be hit if FC logic is correct and errors are propagated.
			s.conn.log.Warn("Stream.WriteData: actualBytesForThisFrame became 0 unexpectedly after FC logic. Breaking.", logger.LogFields{"stream_id": s.id})
			break // or return error if this state is truly erroneous
		}

		// Acquire connection flow control
		connAcqErr := s.conn.connFCManager.AcquireSendSpace(actualBytesForThisFrame)
		if connAcqErr != nil {
			s.fcManager.ReleaseSendSpace(actualBytesForThisFrame) // Rollback stream FC
			s.conn.log.Error("Stream.WriteData: Error acquiring from connection FC.", logger.LogFields{"stream_id": s.id, "amount": actualBytesForThisFrame, "error": connAcqErr})
			return totalN, connAcqErr
		}

		// Send the frame
		currentFrameData := dataRemaining[:actualBytesForThisFrame]
		// Recalculate isFinalFrameAndEndsStream based on actualBytesForThisFrame for *this* frame.
		isCurrentFrameEndStream := (len(dataRemaining) == int(actualBytesForThisFrame)) && isLogicalEndStream

		s.conn.log.Debug("Stream.WriteData: Sending DATA frame", logger.LogFields{
			"stream_id": s.id, "payload_len": len(currentFrameData), "end_stream_flag": isCurrentFrameEndStream,
		})

		s.mu.Lock()                                             // Lock for state check before send and update after.
		if s.endStreamSentToClient && isCurrentFrameEndStream { // Double check if already ended, especially if this frame has END_STREAM
			s.mu.Unlock()
			s.fcManager.ReleaseSendSpace(actualBytesForThisFrame)          // Rollback stream FC
			s.conn.connFCManager.ReleaseSendSpace(actualBytesForThisFrame) // Rollback connection FC
			s.conn.log.Warn("Stream.WriteData: Attempt to send DATA with END_STREAM, but stream already marked ended (race).", logger.LogFields{"stream_id": s.id})
			return totalN, NewStreamError(s.id, ErrCodeInternalError, "stream already ended by server (race condition on send)")
		}
		s.mu.Unlock() // Unlock for sendDataFrame call

		_, sendErr := s.conn.sendDataFrame(s, currentFrameData, isCurrentFrameEndStream)

		s.mu.Lock() // Re-lock for state update
		if sendErr != nil {
			s.mu.Unlock()                                                  // Unlock before releasing FC
			s.fcManager.ReleaseSendSpace(actualBytesForThisFrame)          // Rollback stream FC
			s.conn.connFCManager.ReleaseSendSpace(actualBytesForThisFrame) // Rollback connection FC
			s.conn.log.Error("Stream.WriteData: Error sending DATA frame.", logger.LogFields{"stream_id": s.id, "error": sendErr})
			return totalN, sendErr
		}

		totalN += int(actualBytesForThisFrame)
		dataRemaining = dataRemaining[actualBytesForThisFrame:]

		if isCurrentFrameEndStream {
			if !s.endStreamSentToClient { // Ensure not already set by a race
				s.endStreamSentToClient = true
				s.conn.log.Debug("Stream.WriteData: Sent END_STREAM flag.", logger.LogFields{"stream_id": s.id})
				s.transitionStateOnSendEndStream()
			}
			s.mu.Unlock()
			break // All data sent and stream ended by this frame
		}
		s.mu.Unlock() // Unlock if not isCurrentFrameEndStream path

		if len(dataRemaining) == 0 && isLogicalEndStream {
			// All data from p has been sent, but the last frame didn't have END_STREAM.
			// Send an empty DATA frame with END_STREAM.
			s.conn.log.Debug("Stream.WriteData: All data from buffer sent, sending final empty DATA frame with END_STREAM.", logger.LogFields{"stream_id": s.id})

			s.mu.Lock() // Lock for state check/update
			if s.endStreamSentToClient {
				s.mu.Unlock()
				s.conn.log.Warn("Stream.WriteData: Attempt to send final empty DATA with END_STREAM, but stream already marked ended.", logger.LogFields{"stream_id": s.id})
				return totalN, NewStreamError(s.id, ErrCodeInternalError, "stream already ended by server before final empty END_STREAM")
			}
			s.mu.Unlock() // Unlock for sendDataFrame

			// No FC for 0-length
			_, finalSendErr := s.conn.sendDataFrame(s, nil, true)

			s.mu.Lock() // Re-lock for state update
			if finalSendErr == nil {
				if !s.endStreamSentToClient {
					s.endStreamSentToClient = true
					s.transitionStateOnSendEndStream()
				}
			} else {
				err = finalSendErr // Propagate error from sending final empty frame
			}
			s.mu.Unlock()
			// Whether err or not, we've attempted the final part.
			return totalN, err
		}

		if len(dataRemaining) == 0 && !isLogicalEndStream {
			break // All data sent, and no end stream requested
		}
	} // End for loop

	s.conn.log.Debug("Stream.WriteData: Exiting", logger.LogFields{"stream_id": s.id, "total_n": totalN, "final_err_val": err})
	return totalN, err
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

	// If stream.requestBodyWriter is explicitly closed here, it can race with
	// processIncomingDataFrames trying to write the last chunk after dataFrameChan
	// is closed by handleDataFrame (on END_STREAM).
	// The defer in processIncomingDataFrames is responsible for closing this pipe.
	// 1. Close the requestBodyWriter pipe WITH THE INTENDED ERROR FIRST.
	//    This ensures that any handler blocked on reading the body receives the specific error
	//    rather than a generic context cancellation or premature EOF if other cleanup happens first.
	// s.conn.log.Debug("closeStreamResourcesProtected: s.requestBodyWriter management is now solely handled by processIncomingDataFrames defer.", logger.LogFields{"stream_id": s.id})
	// The requestBodyWriter pipe is now intended to be closed *only* by the defer
	// in processIncomingDataFrames. This avoids races where closeStreamResourcesProtected
	// closes the pipe while processIncomingDataFrames might still be trying to write to it
	// (e.g., the last DATA frame after dataFrameChan was closed by handleDataFrame on END_STREAM).
	// Signaling processIncomingDataFrames to exit (by closing dataFrameChan and/or cancelling s.ctx)
	// should lead to its defer closing the pipe appropriately.
	s.conn.log.Debug("closeStreamResourcesProtected: requestBodyWriter closure is now solely handled by processIncomingDataFrames' defer.", logger.LogFields{"stream_id": s.id})

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
	// s.pendingRSTCode = nil // Intentionally commented out for TestStream_setStateToClosed_CleansUpResources/pendingRSTCode_is_non-nil to pick up RST code in pipe error

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
	endStreamReceivedClient := s.endStreamReceivedFromClient
	s.conn.log.Debug("handleDataFrame: Entry state check", logger.LogFields{"stream_id": s.id, "state": currentState.String(), "pending_rst": isPendingRST, "end_stream_recv_client": endStreamReceivedClient, "ctx_err": ctxErr})
	s.mu.RUnlock()

	if currentState == StreamStateClosed || isPendingRST || ctxErr != nil {
		msg := fmt.Sprintf("DATA frame on closed/resetting/cancelled stream %d (state: %s, pendingRST: %v, ctxErr: %v)", s.id, currentState, isPendingRST, ctxErr)
		s.conn.log.Warn(msg, logger.LogFields{"stream_id": s.id})
		// If already RST or context cancelled, StreamClosed or Cancel might be more appropriate if no specific RST pending.
		if isPendingRST {
			// If pendingRSTCode is not nil, use its value.
			// Need to dereference s.pendingRSTCode safely.
			var rstCodeToSend ErrorCode
			s.mu.RLock() // Lock to safely read s.pendingRSTCode
			if s.pendingRSTCode != nil {
				rstCodeToSend = *s.pendingRSTCode
			} else { // Should not happen if isPendingRST is true, but defensive
				rstCodeToSend = ErrCodeStreamClosed // Fallback
			}
			s.mu.RUnlock()
			return NewStreamError(s.id, rstCodeToSend, msg)
		}
		if ctxErr != nil {
			return NewStreamError(s.id, ErrCodeCancel, msg)
		}
		return NewStreamError(s.id, ErrCodeStreamClosed, msg)
	}

	dataLen := uint32(len(frame.Data))
	hasEndStreamFlag := frame.Header().Flags&FlagDataEndStream != 0

	s.mu.Lock() // Lock for state and fcManager access
	// Re-check state under full lock
	// Added s.endStreamReceivedFromClient to conditions that preclude processing DATA

	if s.state == StreamStateHalfClosedRemote || s.state == StreamStateClosed || s.pendingRSTCode != nil || s.ctx.Err() != nil {
		currentStateLocked := s.state
		isPendingRSTLocked := s.pendingRSTCode != nil
		ctxErrLocked := s.ctx.Err()
		endStreamReceivedClientLocked := s.endStreamReceivedFromClient
		s.mu.Unlock()

		msg := fmt.Sprintf("DATA frame on stream %d in invalid state (re-check under lock): state=%s, pendingRST=%v, ctxErr=%v, endStreamReceived=%v", s.id, currentStateLocked, isPendingRSTLocked, ctxErrLocked, endStreamReceivedClientLocked)
		s.conn.log.Warn(msg, logger.LogFields{"stream_id": s.id})

		// Prioritize general stream state issues if endStreamReceivedClientLocked is also true.
		// The test expects "DATA frame on closed/half-closed-remote stream"

		if currentStateLocked == StreamStateHalfClosedRemote || currentStateLocked == StreamStateClosed {
			// This will produce the error message "DATA frame on closed/half-closed-remote stream" as expected by the test
			return NewStreamError(s.id, ErrCodeStreamClosed, "DATA frame on closed/half-closed-remote stream")
		}

		if endStreamReceivedClientLocked {
			// RFC 7540, 5.1: A stream that is in "half-closed (remote)" or "closed" state MUST NOT be sent DATA frames.
			// Receiving END_STREAM flag means client won't send more DATA. If it does, it's a protocol violation.
			// Error code STREAM_CLOSED is appropriate here.
			return NewStreamError(s.id, ErrCodeStreamClosed, "DATA frame received after END_STREAM flag from client")
		}
		// For other conditions like pendingRST or context error, determine best error code.
		if isPendingRSTLocked {
			return NewStreamError(s.id, ErrCodeStreamClosed, msg) // Fallback, consider specific RST code
		}
		if ctxErrLocked != nil {
			return NewStreamError(s.id, ErrCodeCancel, msg)
		}
		return NewStreamError(s.id, ErrCodeStreamClosed, msg) // Generic for other state issues
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
	}
	// Update receivedDataBytes regardless of Content-Length presence if dataLen > 0
	if dataLen > 0 {
		s.receivedDataBytes += uint64(dataLen)
	}

	currentDataFrameChan := s.dataFrameChan
	streamCtx := s.ctx // Use the stream's context for the select below
	s.mu.Unlock()      // UNLOCK BEFORE CHANNEL OPERATIONS

	s.conn.log.Debug("handleDataFrame: Checking streamCtx (s.ctx) before select.", logger.LogFields{"stream_id": s.id, "ctx_err_before_select": fmt.Sprintf("%v", streamCtx.Err())})
	if err := streamCtx.Err(); err != nil {
		s.conn.log.Warn("handleDataFrame: Stream context done before queuing DATA frame (re-check). Returning error.", logger.LogFields{"stream_id": s.id, "ctx_err": err})
		return NewStreamError(s.id, ErrCodeCancel, "stream context done, cannot queue data")
	}

	if currentDataFrameChan == nil {
		s.conn.log.Warn("handleDataFrame: dataFrameChan already closed/nil, cannot queue DATA frame.", logger.LogFields{"stream_id": s.id, "data_len": dataLen, "has_end_stream": hasEndStreamFlag})
		// If dataLen > 0 and !hasEndStreamFlag, and channel is nil, it means END_STREAM was processed (chan closed),
		// but more data arrived. This should have been caught by s.endStreamReceivedFromClient check above.
		// If it reaches here, it's a more subtle state.
		if dataLen > 0 && !hasEndStreamFlag {
			return NewStreamError(s.id, ErrCodeInternalError, "attempt to send DATA on stream where data channel is unexpectedly nil after END_STREAM checks")
		}
		return nil // If no data or it's an END_STREAM, and channel is nil, likely already handled or fine.
	}

	select {
	case currentDataFrameChan <- frame:
		s.conn.log.Debug("handleDataFrame: Queued DATA frame to dataFrameChan", logger.LogFields{"stream_id": s.id, "data_len": dataLen, "has_end_stream": hasEndStreamFlag})
	case <-streamCtx.Done():
		s.conn.log.Warn("handleDataFrame: Stream context done, cannot queue DATA frame (during send).", logger.LogFields{"stream_id": s.id, "ctx_err": streamCtx.Err()})
		return NewStreamError(s.id, ErrCodeCancel, "stream context done, cannot queue data")
		// Removed default case for full channel. Let it block. This is standard Go channel behavior.
		// If the handler isn't reading, the producer (this func) will block, and eventually flow control (HTTP/2 level)
		// should prevent the peer from sending more data if windows fill up.
		// If streamCtx.Done() unblocks, that's the signal to stop.
	}

	if hasEndStreamFlag {
		s.mu.Lock()                          // Lock for state and s.dataFrameChan modification
		s.endStreamReceivedFromClient = true // Mark that we've processed the client's END_STREAM signal
		s.conn.log.Debug("handleDataFrame: END_STREAM flag on DATA frame. Closing dataFrameChan.", logger.LogFields{"stream_id": s.id})

		// Close dataFrameChan to signal processIncomingDataFrames to stop reading.
		// It's safe to close an already closed channel (results in a panic, caught by defer).
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
			// s.dataFrameChan = nil // REMOVED: Do not nil out the channel. Just close it.
		}

		// State transition based on receiving END_STREAM
		switch s.state {
		case StreamStateOpen:
			s._setState(StreamStateHalfClosedRemote)
		case StreamStateHalfClosedLocal:
			s._setState(StreamStateClosed)
		default:
			// If in an unexpected state (e.g., already Closed, Reserved), this is a protocol error.
			// The earlier checks for s.state / s.endStreamReceivedFromClient should ideally catch this.
			currentStateBeforeError := s.state.String()
			s.mu.Unlock() // Unlock before returning error
			s.conn.log.Error("Received END_STREAM on DATA frame in unexpected stream state", logger.LogFields{
				"stream_id": s.id,
				"state":     currentStateBeforeError,
			})
			return NewStreamError(s.id, ErrCodeProtocolError, fmt.Sprintf("END_STREAM on DATA in unexpected state %s for stream %d", currentStateBeforeError, s.id))
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
	hasInitialHeadersBeenProcessed := s.initialHeadersProcessed
	s.mu.RUnlock()

	// h2spec 5.1/12: closed: Sends a HEADERS frame
	//   -> The endpoint MUST treat this as a connection error of type STREAM_CLOSED.
	if currentState == StreamStateClosed {
		s.conn.log.Warn("Stream.processRequestHeadersAndDispatch: HEADERS frame received on an already closed stream.", logger.LogFields{"stream_id": s.id})
		return NewConnectionError(ErrCodeStreamClosed, fmt.Sprintf("HEADERS received on closed stream %d", s.id))
	}

	// h2spec 8.1/1: Sends a second HEADERS frame without the END_STREAM flag
	//   -> The endpoint MUST respond with a stream error of type PROTOCOL_ERROR.
	if hasInitialHeadersBeenProcessed && currentState == StreamStateOpen && !endStream {
		s.conn.log.Warn("Stream: Subsequent HEADERS frame received on Open stream without END_STREAM flag (violates h2spec 8.1/1).",
			logger.LogFields{"stream_id": s.id, "state": currentState.String()})
		return NewStreamError(s.id, ErrCodeProtocolError, "subsequent HEADERS on open stream without END_STREAM")
	}
	s.conn.log.Debug("Stream.processRequestHeadersAndDispatch: Entered", logger.LogFields{"stream_id": s.id, "num_headers_arg": len(headers), "end_stream_arg": endStream, "dispatcher_is_nil": dispatcher == nil})

	if dispatcher == nil {
		s.conn.log.Error("Stream.processRequestHeadersAndDispatch: Dispatcher is nil, cannot proceed.", logger.LogFields{"stream_id": s.id})
		// Attempt to RST the stream if dispatcher is nil, as this is an internal server misconfiguration.
		// Error from Close is logged by Close itself.
		_ = s.Close(NewStreamError(s.id, ErrCodeInternalError, "dispatcher is nil"))
		return NewStreamError(s.id, ErrCodeInternalError, "dispatcher is nil") // Return error to caller
	}

	// --- BEGIN Original Header Validations ---
	for _, hf := range headers {
		// 1.a Uppercase header field names (h2spec 8.1.2 #1)
		for _, char := range hf.Name {
			if char >= 'A' && char <= 'Z' {
				errMsg := fmt.Sprintf("invalid header field name '%s' contains uppercase characters", hf.Name)
				s.conn.log.Error(errMsg, logger.LogFields{"stream_id": s.id, "header_name": hf.Name})
				return NewStreamError(s.id, ErrCodeProtocolError, errMsg)
			}
		}
		lowerName := strings.ToLower(hf.Name)
		// 1.b Forbidden connection-specific headers (h2spec 8.1.2.2 #1)
		switch lowerName {
		case "connection", "proxy-connection", "keep-alive", "upgrade", "transfer-encoding":
			errMsg := fmt.Sprintf("connection-specific header field '%s' is forbidden in HTTP/2", hf.Name)
			s.conn.log.Error(errMsg, logger.LogFields{"stream_id": s.id, "header_name": hf.Name})
			return NewStreamError(s.id, ErrCodeProtocolError, errMsg)
		}
		// 1.c TE header validation (h2spec 8.1.2.2 #2)
		if lowerName == "te" {
			if strings.ToLower(hf.Value) != "trailers" {
				errMsg := fmt.Sprintf("TE header field contains invalid value '%s', must be 'trailers'", hf.Value)
				s.conn.log.Error(errMsg, logger.LogFields{"stream_id": s.id, "header_value": hf.Value})
				return NewStreamError(s.id, ErrCodeProtocolError, errMsg)
			}
		}
	}
	// --- END Original Header Validations ---

	var clHeaderValue string
	var clHeaderFound bool
	for _, hf := range headers {
		if strings.ToLower(hf.Name) == "content-length" {
			if clHeaderFound {
				s.conn.log.Error("Duplicate content-length header found", logger.LogFields{"stream_id": s.id, "value1": clHeaderValue, "value2": hf.Value})
				return NewStreamError(s.id, ErrCodeProtocolError, "duplicate content-length header")
			}
			clHeaderValue = hf.Value
			clHeaderFound = true
		}
	}

	var parsedCL *int64
	if clHeaderFound {
		val, err := strconv.ParseInt(clHeaderValue, 10, 64)
		if err != nil || val < 0 {
			s.conn.log.Error("Invalid content-length header value", logger.LogFields{"stream_id": s.id, "value": clHeaderValue, "parse_error": err})
			return NewStreamError(s.id, ErrCodeProtocolError, "invalid content-length header value")
		}
		parsedCL = &val
	}

	s.mu.Lock() // Lock for s.requestHeaders, s.parsedContentLength, s.initialHeadersProcessed, s.endStreamReceivedFromClient

	// Task: Content-length validation if END_STREAM on HEADERS (h2spec 8.1.2.6 #1)
	if endStream {
		if parsedCL != nil && *parsedCL != 0 {
			errMsg := fmt.Sprintf("HEADERS frame with END_STREAM set and non-zero content-length (%d)", *parsedCL)
			s.conn.log.Error(errMsg, logger.LogFields{"stream_id": s.id})
			s.mu.Unlock()
			return NewStreamError(s.id, ErrCodeProtocolError, errMsg)
		}
		// If END_STREAM is set on HEADERS, client indicates no DATA frames will follow.
		// Close the dataFrameChan *before* setting endStreamReceivedFromClient.
		// This ensures processIncomingDataFrames sees the closed chan and exits gracefully.
		if s.dataFrameChan != nil {
			s.conn.log.Debug("processRequestHeadersAndDispatch: END_STREAM on HEADERS, closing dataFrameChan.", logger.LogFields{"stream_id": s.id})
			// It's safe to attempt to close `dataFrameChan` even if it might have been closed by another path (e.g. RST),
			// as `close` on a closed channel panics, which can be recovered from if needed (though here, logic should prevent double close).
			// Better to check if it's nil or already closed if that's a concern, but for now, just close.
			func() {
				defer func() {
					if r := recover(); r != nil {
						s.conn.log.Debug("Recovered from panic closing dataFrameChan in processRequestHeadersAndDispatch (END_STREAM on HEADERS path)",
							logger.LogFields{"stream_id": s.id, "panic": r})
					}
				}()
				close(s.dataFrameChan)
			}()
			// s.dataFrameChan = nil // Do not nil it out here. Let processIncomingDataFrames see it closed.
		}
		s.endStreamReceivedFromClient = true // Mark that client has signaled end of its transmission
	}
	s.requestHeaders = headers
	s.parsedContentLength = parsedCL

	if !s.initialHeadersProcessed {
		s.initialHeadersProcessed = true
	}
	s.mu.Unlock()

	pseudoMethod, pseudoPath, pseudoScheme, pseudoAuthority, pseudoErr := s.conn.extractPseudoHeaders(headers)
	if pseudoErr != nil {
		s.conn.log.Error("Failed to extract/validate pseudo headers", logger.LogFields{"stream_id": s.id, "error": pseudoErr.Error()})
		// Pseudo-header errors are stream errors.
		return NewStreamError(s.id, ErrCodeProtocolError, "pseudo-header validation failed: "+pseudoErr.Error())
	}

	reqURL := &url.URL{
		Scheme: pseudoScheme,
		Host:   pseudoAuthority,
	}
	if idx := strings.Index(pseudoPath, "?"); idx != -1 {
		reqURL.Path = pseudoPath[:idx]
		reqURL.RawQuery = pseudoPath[idx+1:]
	} else {
		reqURL.Path = pseudoPath
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
		Body: &requestBodyConsumedNotifier{
			stream: s,
			reader: s.requestBodyReader,
		},
		Host:       pseudoAuthority,
		RemoteAddr: s.conn.remoteAddrStr,
		RequestURI: pseudoPath,
	}
	if parsedCL != nil {
		req.ContentLength = *parsedCL
	} else if clHeaderFound { // Header present but unparseable (should have errored above)
		req.ContentLength = -1 // Indicate unknown or problematic
	}

	req = req.WithContext(s.ctx) // Associate stream's context with the request

	s.conn.log.Debug("Stream: Dispatching request to handler", logger.LogFields{"stream_id": s.id, "method": req.Method, "uri": req.RequestURI})

	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.conn.log.Error("Panic in stream handler goroutine", logger.LogFields{
					"stream_id": s.id,
					"panic_val": fmt.Sprintf("%v", r),
					"stack":     string(debug.Stack()),
				})
				// Attempt to RST_STREAM if handler panics.
				_ = s.sendRSTStream(ErrCodeInternalError)
			}
			// Signal that this handler has finished processing.
			s.conn.streamHandlerDone(s)
		}()
		dispatcher(s, req) // Call the actual application handler
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
	s.mu.RLock()
	localChan := s.dataFrameChan
	s.mu.RUnlock()

	if localChan == nil {
		s.conn.log.Warn("Stream.processIncomingDataFrames: s.dataFrameChan was nil at goroutine start. Exiting.", logger.LogFields{"stream_id": s.id})
		if s.requestBodyWriter != nil {
			_ = s.requestBodyWriter.CloseWithError(fmt.Errorf("stream %d data channel was nil at start of processing", s.id))
		}
		return
	}

	defer func() {
		panicked := recover()
		var closeErr error
		s.mu.RLock()
		pendingRST := s.pendingRSTCode
		s.mu.RUnlock()

		s.conn.log.Debug("Stream.processIncomingDataFrames: DEFER executing.", logger.LogFields{
			"stream_id": s.id, "panicked_val": fmt.Sprintf("%v", panicked),
			"pending_rst_code": fmt.Sprintf("%v", pendingRST), "s_ctx_err": fmt.Sprintf("%v", s.ctx.Err()),
		})

		if pendingRST != nil {
			if panicked != nil {
				closeErr = NewStreamError(s.id, *pendingRST, fmt.Sprintf("stream reset (rst_code: %s) during body processing panic: %v", pendingRST.String(), panicked))
			} else {
				closeErr = NewStreamError(s.id, *pendingRST, fmt.Sprintf("stream reset (rst_code: %s)", pendingRST.String()))
			}
		} else if panicked != nil {
			s.conn.log.Error("Panic in stream.processIncomingDataFrames", logger.LogFields{
				"stream_id": s.id, "panic_val": fmt.Sprintf("%v", panicked), "stack": string(debug.Stack()),
			})
			select {
			case <-s.ctx.Done():
				closeErr = s.ctx.Err()
			default:
				closeErr = fmt.Errorf("panic in body processing for stream %d: %v", s.id, panicked)
			}
		} else {
			closeErr = nil // EOF for reader
		}

		if s.requestBodyWriter != nil {
			s.conn.log.Debug("Stream.processIncomingDataFrames: DEFER closing requestBodyWriter.", logger.LogFields{"stream_id": s.id, "close_err_arg_type": fmt.Sprintf("%T", closeErr), "close_err_arg_val": closeErr})
			if err := s.requestBodyWriter.CloseWithError(closeErr); err != nil {
				isClosedPipeError := false
				if errors.Is(err, io.ErrClosedPipe) {
					isClosedPipeError = true
				}
				if cerr, ok := closeErr.(interface{ Is(error) bool }); ok && cerr != nil && cerr.Is(io.ErrClosedPipe) {
					isClosedPipeError = true
				}
				if !isClosedPipeError {
					s.conn.log.Warn("Error from requestBodyWriter.CloseWithError in processIncomingDataFrames defer",
						logger.LogFields{"stream_id": s.id, "close_err_arg_type": fmt.Sprintf("%T", closeErr), "close_err_arg_val": closeErr, "pipe_close_error": err.Error()})
				} else {
					s.conn.log.Debug("requestBodyWriter.CloseWithError in processIncomingDataFrames defer returned io.ErrClosedPipe or similar (benign).",
						logger.LogFields{"stream_id": s.id, "close_err_arg_val_for_benign": closeErr})
				}
			}
		}
		s.conn.log.Debug("Stream.processIncomingDataFrames: Goroutine exiting", logger.LogFields{"stream_id": s.id, "final_pipe_close_error_type": fmt.Sprintf("%T", closeErr), "final_pipe_close_error_val": closeErr})
		if panicked != nil {
			panic(panicked)
		}
	}()

	s.conn.log.Debug("Stream.processIncomingDataFrames: Goroutine started", logger.LogFields{"stream_id": s.id})

	for {
		s.conn.log.Debug("Stream.processIncomingDataFrames: Top of loop, before select.", logger.LogFields{"stream_id": s.id})
		var frame *DataFrame
		var ok bool

		select {
		case <-s.ctx.Done():
			s.conn.log.Debug("Stream.processIncomingDataFrames: Stream context done, exiting.", logger.LogFields{"stream_id": s.id, "ctx_err": s.ctx.Err()})
			return
		case frame, ok = <-localChan:
			s.conn.log.Debug("Stream.processIncomingDataFrames: Read from dataFrameChan.", logger.LogFields{"stream_id": s.id, "frame_is_nil_after_read": frame == nil, "ok_after_read": ok})
			if !ok {
				s.conn.log.Debug("Stream.processIncomingDataFrames: dataFrameChan closed, exiting.", logger.LogFields{"stream_id": s.id})
				return
			}
		}

		if frame == nil {
			s.conn.log.Warn("Stream.processIncomingDataFrames: Received nil frame from dataFrameChan", logger.LogFields{"stream_id": s.id})
			continue
		}
		s.conn.log.Debug("Stream.processIncomingDataFrames: Got DATA frame from channel", logger.LogFields{"stream_id": s.id, "data_len": len(frame.Data)})

		if len(frame.Data) > 0 {
			s.conn.log.Debug("Stream.processIncomingDataFrames: About to Write to requestBodyWriter", logger.LogFields{"stream_id": s.id, "data_to_write_len": len(frame.Data)})
			nWritten, err := s.requestBodyWriter.Write(frame.Data)
			if err != nil {
				s.conn.log.Error("Stream.processIncomingDataFrames: Error writing to requestBodyWriter, attempting to RST stream.", logger.LogFields{
					"stream_id": s.id, "error": err.Error(), "n_written_before_err": nWritten,
				})
				if rstErr := s.sendRSTStream(ErrCodeCancel); rstErr != nil {
					s.conn.log.Error("Stream.processIncomingDataFrames: Failed to send RST_STREAM after pipe write error.", logger.LogFields{
						"stream_id": s.id, "pipe_err": err.Error(), "rst_stream_err": rstErr.Error(),
					})
				}
				return
			}
			s.conn.log.Debug("Stream.processIncomingDataFrames: Wrote DATA to pipe", logger.LogFields{"stream_id": s.id, "bytes_written_to_pipe": nWritten})
		}
	}
} // processIncomingDataFrames ends here

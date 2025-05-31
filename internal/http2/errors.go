package http2

import "fmt"

// ErrorCode represents an HTTP/2 error code.
type ErrorCode uint32

// HTTP/2 error codes from RFC 7540 Section 7.
const (
	// ErrCodeNoError (0x0): Graceful shutdown.
	ErrCodeNoError ErrorCode = 0x0
	// ErrCodeProtocolError (0x1): Protocol error detected.
	ErrCodeProtocolError ErrorCode = 0x1
	// ErrCodeInternalError (0x2): Implementation fault.
	ErrCodeInternalError ErrorCode = 0x2
	// ErrCodeFlowControlError (0x3): Flow-control limits exceeded.
	ErrCodeFlowControlError ErrorCode = 0x3
	// ErrCodeSettingsTimeout (0x4): Settings not acknowledged.
	ErrCodeSettingsTimeout ErrorCode = 0x4
	// ErrCodeStreamClosed (0x5): Frame received for already closed stream.
	ErrCodeStreamClosed ErrorCode = 0x5
	// ErrCodeFrameSizeError (0x6): Frame size incorrect.
	ErrCodeFrameSizeError ErrorCode = 0x6
	// ErrCodeRefusedStream (0x7): Stream not processed.
	ErrCodeRefusedStream ErrorCode = 0x7
	// ErrCodeCancel (0x8): Stream cancelled.
	ErrCodeCancel ErrorCode = 0x8
	// ErrCodeCompressionError (0x9): Compression state not maintained.
	ErrCodeCompressionError ErrorCode = 0x9
	// ErrCodeConnectError (0xa): Connection established in error.
	ErrCodeConnectError ErrorCode = 0xa
	// ErrCodeEnhanceYourCalm (0xb): Processing capacity exceeded.
	ErrCodeEnhanceYourCalm ErrorCode = 0xb
	// ErrCodeInadequateSecurity (0xc): Negotiated TLS parameters not acceptable.
	ErrCodeInadequateSecurity ErrorCode = 0xc
	// ErrCodeHTTP11Required (0xd): Use HTTP/1.1 for the request.
	ErrCodeHTTP11Required ErrorCode = 0xd
)

// String returns the string representation of the ErrorCode.
func (e ErrorCode) String() string {
	switch e {
	case ErrCodeNoError:
		return "NO_ERROR"
	case ErrCodeProtocolError:
		return "PROTOCOL_ERROR"
	case ErrCodeInternalError:
		return "INTERNAL_ERROR"
	case ErrCodeFlowControlError:
		return "FLOW_CONTROL_ERROR"
	case ErrCodeSettingsTimeout:
		return "SETTINGS_TIMEOUT"
	case ErrCodeStreamClosed:
		return "STREAM_CLOSED"
	case ErrCodeFrameSizeError:
		return "FRAME_SIZE_ERROR"
	case ErrCodeRefusedStream:
		return "REFUSED_STREAM"
	case ErrCodeCancel:
		return "CANCEL"
	case ErrCodeCompressionError:
		return "COMPRESSION_ERROR"
	case ErrCodeConnectError:
		return "CONNECT_ERROR"
	case ErrCodeEnhanceYourCalm:
		return "ENHANCE_YOUR_CALM"
	case ErrCodeInadequateSecurity:
		return "INADEQUATE_SECURITY"
	case ErrCodeHTTP11Required:
		return "HTTP_1_1_REQUIRED"
	default:
		return fmt.Sprintf("UNKNOWN_ERROR_CODE_%d", uint32(e))
	}
}

// StreamError represents an error specific to an HTTP/2 stream.
// It implements the standard Go error interface.
type StreamError struct {
	StreamID uint32
	Code     ErrorCode
	Msg      string
	Cause    error // Optional underlying cause
}

// Error returns a string representation of the StreamError.
func (e *StreamError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("stream error on stream %d: %s (code %s, %d): %s", e.StreamID, e.Msg, e.Code.String(), e.Code, e.Cause)
	}
	return fmt.Sprintf("stream error on stream %d: %s (code %s, %d)", e.StreamID, e.Msg, e.Code.String(), e.Code)
}

// Unwrap returns the underlying cause of the error, if any.
func (e *StreamError) Unwrap() error {
	return e.Cause
}

// NewStreamError creates a new StreamError.
func NewStreamError(streamID uint32, code ErrorCode, msg string) *StreamError {
	return &StreamError{StreamID: streamID, Code: code, Msg: msg}
}

// NewStreamErrorWithCause creates a new StreamError with an underlying cause.
func NewStreamErrorWithCause(streamID uint32, code ErrorCode, msg string, cause error) *StreamError {
	return &StreamError{StreamID: streamID, Code: code, Msg: msg, Cause: cause}
}

// ConnectionError represents an error that affects the entire HTTP/2 connection.
// It implements the standard Go error interface.
type ConnectionError struct {
	LastStreamID uint32
	Code         ErrorCode
	Msg          string
	Cause        error // Optional underlying cause
	// DebugData can be used for the AdditionalDebugData in a GOAWAY frame.
	// It should be human-readable and not security-sensitive.
	DebugData []byte
}

// Error returns a string representation of the ConnectionError.
func (e *ConnectionError) Error() string {

	if e.Cause != nil {
		return fmt.Sprintf("connection error: %s (last_stream_id %d, code %s, %d): %s", e.Msg, e.LastStreamID, e.Code.String(), e.Code, e.Cause)
	}
	return fmt.Sprintf("connection error: %s (last_stream_id %d, code %s, %d)", e.Msg, e.LastStreamID, e.Code.String(), e.Code)
}

// Unwrap returns the underlying cause of the error, if any.
func (e *ConnectionError) Unwrap() error {
	return e.Cause
}

// NewConnectionError creates a new ConnectionError.
func NewConnectionError(code ErrorCode, msg string) *ConnectionError {
	return &ConnectionError{
		Code: code,
		Msg:  msg,
		// DebugData will be nil by default
	}
}

// NewConnectionErrorWithCause creates a new ConnectionError with an underlying cause.
func NewConnectionErrorWithCause(code ErrorCode, msg string, cause error) *ConnectionError {
	return &ConnectionError{
		Code:  code,
		Msg:   msg,
		Cause: cause,
		// DebugData will be nil by default
	}
}

// GenerateRSTStreamFrame creates an RSTStreamFrame from a StreamError or a generic error code.
// If err is a *StreamError, its StreamID and Code are used.
// Otherwise, the provided streamID and errCode are used.
// The FrameHeader fields Type, StreamID, and Length are set.
func GenerateRSTStreamFrame(streamID uint32, errCode ErrorCode, err error) *RSTStreamFrame {
	codeToUse := errCode
	finalStreamID := streamID

	if se, ok := err.(*StreamError); ok {
		codeToUse = se.Code   // Always use StreamError's code
		if se.StreamID != 0 { // Prioritize StreamError's StreamID if it's set
			finalStreamID = se.StreamID
		}
		// If se.StreamID is 0, finalStreamID (initialized from streamID arg) is used.
	}

	// Ensure FrameHeader is fully initialized
	fh := FrameHeader{
		Type:     FrameRSTStream,
		StreamID: finalStreamID, // Use the determined stream ID
		Length:   4,             // RST_STREAM payload is always 4 bytes for ErrorCode
		Flags:    0,             // RST_STREAM frames have no flags defined
	}

	return &RSTStreamFrame{
		FrameHeader: fh,
		ErrorCode:   codeToUse,
	}
}

// GenerateGoAwayFrame creates a GoAwayFrame from a ConnectionError or generic parameters.
// If err is a *ConnectionError, its LastStreamID, Code, and DebugData are used.
// Otherwise, the provided lastStreamID, errCode, and debugData are used.
// The debugData string will be converted to bytes.
// The FrameHeader fields Type, StreamID (0), and Length are set.
func GenerateGoAwayFrame(lastStreamID uint32, errCode ErrorCode, debugStr string, err error) *GoAwayFrame {
	codeToUse := errCode
	finalLastStreamID := lastStreamID
	var debugDataBytes []byte // Initialize as nil/empty

	if ce, ok := err.(*ConnectionError); ok {
		finalLastStreamID = ce.LastStreamID // Already set before this block if not *ConnectionError
		codeToUse = ce.Code                 // Already set before this block if not *ConnectionError

		if len(ce.DebugData) > 0 {
			debugDataBytes = ce.DebugData // Prio 1 for ConnectionError
		} else if ce.Msg != "" {
			debugDataBytes = []byte(ce.Msg) // Prio 2 for ConnectionError
		} else {
			debugDataBytes = []byte(debugStr) // Prio 3 for ConnectionError (fallback to arg)
		}
	} else {
		debugDataBytes = []byte(debugStr) // If not ConnectionError, use debugStrArg
	}

	// Ensure FrameHeader is fully initialized
	fh := FrameHeader{
		Type:     FrameGoAway,
		StreamID: 0, // GOAWAY is always on stream 0
		Length:   8 + uint32(len(debugDataBytes)),
		Flags:    0, // GOAWAY frames have no flags defined
	}

	return &GoAwayFrame{
		FrameHeader:         fh,
		LastStreamID:        finalLastStreamID,
		ErrorCode:           codeToUse,
		AdditionalDebugData: debugDataBytes,
	}
}

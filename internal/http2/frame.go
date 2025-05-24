package http2

import (
	"encoding/binary"
	"fmt"
	"io"
)

// FrameType represents an HTTP/2 frame type.
type FrameType uint8

const (
	// FrameData is for DATA frames (0x0).
	FrameData FrameType = 0x0
	// FrameHeaders is for HEADERS frames (0x1).
	FrameHeaders FrameType = 0x1
	// FramePriority is for PRIORITY frames (0x2).
	FramePriority FrameType = 0x2
	// FrameRSTStream is for RST_STREAM frames (0x3).
	FrameRSTStream FrameType = 0x3
	// FrameSettings is for SETTINGS frames (0x4).
	FrameSettings FrameType = 0x4
	// FramePushPromise is for PUSH_PROMISE frames (0x5).
	FramePushPromise FrameType = 0x5
	// FramePing is for PING frames (0x6).
	FramePing FrameType = 0x6
	// FrameGoAway is for GOAWAY frames (0x7).
	FrameGoAway FrameType = 0x7
	// FrameWindowUpdate is for WINDOW_UPDATE frames (0x8).
	FrameWindowUpdate FrameType = 0x8
	// FrameContinuation is for CONTINUATION frames (0x9).
	FrameContinuation FrameType = 0x9
)

// String returns the string representation of the FrameType.
func (t FrameType) String() string {
	switch t {
	case FrameData:
		return "DATA"
	case FrameHeaders:
		return "HEADERS"
	case FramePriority:
		return "PRIORITY"
	case FrameRSTStream:
		return "RST_STREAM"
	case FrameSettings:
		return "SETTINGS"
	case FramePushPromise:
		return "PUSH_PROMISE"
	case FramePing:
		return "PING"
	case FrameGoAway:
		return "GOAWAY"
	case FrameWindowUpdate:
		return "WINDOW_UPDATE"
	case FrameContinuation:
		return "CONTINUATION"
	default:
		return fmt.Sprintf("UNKNOWN_FRAME_TYPE_%d", uint8(t))
	}
}

// Flags represents flags for an HTTP/2 frame.
type Flags uint8

// Frame header flags
const (
	// FlagDataEndStream indicates that this DATA frame is the last from the sender.
	FlagDataEndStream Flags = 0x1
	// FlagDataPadded indicates that this DATA frame is padded.
	FlagDataPadded Flags = 0x8

	// FlagHeadersEndStream indicates that this HEADERS frame is the last from the sender.
	FlagHeadersEndStream Flags = 0x1
	// FlagHeadersEndHeaders indicates that this HEADERS frame contains an entire block of header fields.
	FlagHeadersEndHeaders Flags = 0x4
	// FlagHeadersPadded indicates that this HEADERS frame is padded.
	FlagHeadersPadded Flags = 0x8
	// FlagHeadersPriority indicates that this HEADERS frame includes priority information.
	FlagHeadersPriority Flags = 0x20

	// FlagSettingsAck indicates that this SETTINGS frame acknowledges receipt and application of the peer's SETTINGS frame.
	FlagSettingsAck Flags = 0x1

	// FlagPingAck indicates that this PING frame is an acknowledgment.
	FlagPingAck Flags = 0x1

	// FlagContinuationEndHeaders indicates that this CONTINUATION frame contains the end of a header block.
	FlagContinuationEndHeaders Flags = 0x4

	// FlagPushPromiseEndHeaders indicates that this PUSH_PROMISE frame contains an entire block of header fields.
	FlagPushPromiseEndHeaders Flags = 0x4
	// FlagPushPromisePadded indicates that this PUSH_PROMISE frame is padded.
	FlagPushPromisePadded Flags = 0x8
)

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

// SettingID represents a SETTINGS parameter identifier.
type SettingID uint16

// SETTINGS parameters from RFC 7540 Section 6.5.2.
const (
	// SettingHeaderTableSize (0x1): Initial size of the HPACK header table.
	SettingHeaderTableSize SettingID = 0x1
	// SettingEnablePush (0x2): Whether server push is enabled.
	SettingEnablePush SettingID = 0x2
	// SettingMaxConcurrentStreams (0x3): Maximum number of concurrent streams.
	SettingMaxConcurrentStreams SettingID = 0x3
	// SettingInitialWindowSize (0x4): Initial window size for flow control.
	SettingInitialWindowSize SettingID = 0x4
	// SettingMaxFrameSize (0x5): Maximum size of a frame payload.
	SettingMaxFrameSize SettingID = 0x5
	// SettingMaxHeaderListSize (0x6): Maximum size of header list.
	SettingMaxHeaderListSize SettingID = 0x6
)

// String returns the string representation of the SettingID.
func (s SettingID) String() string {
	switch s {
	case SettingHeaderTableSize:
		return "SETTINGS_HEADER_TABLE_SIZE"
	case SettingEnablePush:
		return "SETTINGS_ENABLE_PUSH"
	case SettingMaxConcurrentStreams:
		return "SETTINGS_MAX_CONCURRENT_STREAMS"
	case SettingInitialWindowSize:
		return "SETTINGS_INITIAL_WINDOW_SIZE"
	case SettingMaxFrameSize:
		return "SETTINGS_MAX_FRAME_SIZE"
	case SettingMaxHeaderListSize:
		return "SETTINGS_MAX_HEADER_LIST_SIZE"
	default:
		return fmt.Sprintf("UNKNOWN_SETTING_ID_%d", uint16(s))
	}
}

const (
	// MaxFrameSize is the default maximum frame size.
	// "The size of a frame payload is limited by the maximum size that a receiver advertises in the SETTINGS_MAX_FRAME_SIZE setting.
	// This setting can have any value between 2^14 (16,384) and 2^24-1 (16,777,215) octets, inclusive."
	// We start with the minimum allowed value.
	DefaultMaxFrameSize uint32 = 16384 // 2^14
	MaxAllowedFrameSize uint32 = (1 << 24) - 1
	MinAllowedFrameSize uint32 = 16384

	// FrameHeaderLen is the length of the HTTP/2 frame header.
	FrameHeaderLen = 9

	// DefaultInitialWindowSize is the default initial window size for flow control.
	DefaultInitialWindowSize uint32 = 65535 // 2^16 - 1

	// DefaultEnablePush is the default value for SETTINGS_ENABLE_PUSH
	DefaultEnablePush uint32 = 1 // Enabled by default
)

// FrameHeader represents the 9-octet header common to all frames.
type FrameHeader struct {
	Length   uint32               // 24 bits
	Type     FrameType            // 8 bits
	Flags    Flags                // 8 bits
	StreamID uint32               // 31 bits (R is 1 bit, masked out)
	raw      [FrameHeaderLen]byte // For Reserialization if payload is opaque
}

// ReadFrameHeader reads a frame header from r.
func ReadFrameHeader(r io.Reader) (FrameHeader, error) {
	var fh FrameHeader
	_, err := io.ReadFull(r, fh.raw[:])
	if err != nil {
		return FrameHeader{}, err
	}

	fh.Length = (uint32(fh.raw[0])<<16 | uint32(fh.raw[1])<<8 | uint32(fh.raw[2]))
	fh.Type = FrameType(fh.raw[3])
	fh.Flags = Flags(fh.raw[4])
	fh.StreamID = binary.BigEndian.Uint32(fh.raw[5:]) & 0x7FFFFFFF // Mask out R bit

	return fh, nil
}

// WriteTo serializes the frame header to w.
func (fh *FrameHeader) WriteTo(w io.Writer) (int64, error) {
	// Reconstruct raw bytes if not already set (e.g. if header was built programmatically)
	// Length (24 bits)
	fh.raw[0] = byte(fh.Length >> 16 & 0xFF)
	fh.raw[1] = byte(fh.Length >> 8 & 0xFF)
	fh.raw[2] = byte(fh.Length & 0xFF)
	// Type (8 bits)
	fh.raw[3] = byte(fh.Type)
	// Flags (8 bits)
	fh.raw[4] = byte(fh.Flags)
	// StreamID (31 bits, R bit is 0)
	binary.BigEndian.PutUint32(fh.raw[5:9], fh.StreamID&0x7FFFFFFF) // Ensure R bit is 0

	n, err := w.Write(fh.raw[:])
	return int64(n), err
}

// Frame is the interface for all HTTP/2 frames.
type Frame interface {
	Header() *FrameHeader
	ParsePayload(r io.Reader, header FrameHeader) error
	WritePayload(w io.Writer) (int64, error)
}

// Concrete frame types

// DataFrame represents an HTTP/2 DATA frame.
// RFC 7540, Section 6.1
type DataFrame struct {
	FrameHeader
	PadLength uint8 // Only present if FlagDataPadded is set
	Data      []byte
	Padding   []byte // Only present if FlagDataPadded is set
}

func (f *DataFrame) Header() *FrameHeader { return &f.FrameHeader }

func (f *DataFrame) ParsePayload(r io.Reader, header FrameHeader) error {
	f.FrameHeader = header
	payloadLen := f.Length

	if f.Flags&FlagDataPadded != 0 {
		var padLenBuf [1]byte
		if _, err := io.ReadFull(r, padLenBuf[:]); err != nil {
			return fmt.Errorf("reading pad length: %w", err)
		}
		f.PadLength = padLenBuf[0]
		payloadLen-- // Subtract pad length octet
		if uint32(f.PadLength) > payloadLen {
			return fmt.Errorf("pad length %d exceeds payload length %d", f.PadLength, payloadLen)
		}
	}

	dataLen := payloadLen
	if f.Flags&FlagDataPadded != 0 {
		dataLen -= uint32(f.PadLength)
	}

	f.Data = make([]byte, dataLen)
	if _, err := io.ReadFull(r, f.Data); err != nil {
		return fmt.Errorf("reading data: %w", err)
	}

	if f.Flags&FlagDataPadded != 0 {
		f.Padding = make([]byte, f.PadLength)
		if _, err := io.ReadFull(r, f.Padding); err != nil {
			return fmt.Errorf("reading padding: %w", err)
		}
	}
	return nil
}

func (f *DataFrame) WritePayload(w io.Writer) (int64, error) {
	var totalN int64
	if f.Flags&FlagDataPadded != 0 {
		n, err := w.Write([]byte{f.PadLength})
		if err != nil {
			return totalN, err
		}
		totalN += int64(n)
	}
	n, err := w.Write(f.Data)
	if err != nil {
		return totalN, err
	}
	totalN += int64(n)
	if f.Flags&FlagDataPadded != 0 {
		n, err := w.Write(f.Padding)
		if err != nil {
			return totalN, err
		}
		totalN += int64(n)
	}
	return totalN, nil
}

// HeadersFrame represents an HTTP/2 HEADERS frame.
// RFC 7540, Section 6.2
type HeadersFrame struct {
	FrameHeader
	PadLength           uint8  // Only present if FlagHeadersPadded is set
	Exclusive           bool   // E: 1 bit
	StreamDependency    uint32 // 31 bits
	Weight              uint8  // 8 bits
	HeaderBlockFragment []byte
	Padding             []byte // Only present if FlagHeadersPadded is set
}

func (f *HeadersFrame) Header() *FrameHeader { return &f.FrameHeader }

func (f *HeadersFrame) ParsePayload(r io.Reader, header FrameHeader) error {
	f.FrameHeader = header
	payloadLen := f.Length
	currentPos := 0

	if f.Flags&FlagHeadersPadded != 0 {
		var padLenBuf [1]byte
		if _, err := io.ReadFull(r, padLenBuf[:]); err != nil {
			return fmt.Errorf("reading pad length: %w", err)
		}
		f.PadLength = padLenBuf[0]
		payloadLen--
		currentPos++
		if uint32(f.PadLength) > payloadLen {
			return fmt.Errorf("pad length %d exceeds remaining payload length %d", f.PadLength, payloadLen)
		}
	}

	if f.Flags&FlagHeadersPriority != 0 {
		if payloadLen < 5 { // Need at least 5 bytes for priority fields
			return fmt.Errorf("payload too short for priority fields: %d", payloadLen)
		}
		var prioBuf [5]byte
		if _, err := io.ReadFull(r, prioBuf[:]); err != nil {
			return fmt.Errorf("reading priority fields: %w", err)
		}
		streamDepAndE := binary.BigEndian.Uint32(prioBuf[0:4])
		f.Exclusive = (streamDepAndE >> 31) == 1
		f.StreamDependency = streamDepAndE & 0x7FFFFFFF
		f.Weight = prioBuf[4]
		payloadLen -= 5
		currentPos += 5
	}

	headerBlockLen := payloadLen
	if f.Flags&FlagHeadersPadded != 0 {
		headerBlockLen -= uint32(f.PadLength)
	}

	f.HeaderBlockFragment = make([]byte, headerBlockLen)
	if _, err := io.ReadFull(r, f.HeaderBlockFragment); err != nil {
		return fmt.Errorf("reading header block fragment: %w", err)
	}
	currentPos += int(headerBlockLen)

	if f.Flags&FlagHeadersPadded != 0 {
		f.Padding = make([]byte, f.PadLength)
		if _, err := io.ReadFull(r, f.Padding); err != nil {
			return fmt.Errorf("reading padding: %w", err)
		}
		currentPos += int(f.PadLength)
	}

	// This check might be redundant if Length was already validated against MaxFrameSize
	if uint32(currentPos) != f.Length {
		// This could happen if PadLength was inconsistent with FrameHeader.Length
		// or if there was an issue with accounting for priority fields.
		return fmt.Errorf("mismatch in parsed payload length for HEADERS: expected %d, got %d (PadLength: %d, Padded: %v, Priority: %v)", f.Length, currentPos, f.PadLength, f.Flags&FlagHeadersPadded != 0, f.Flags&FlagHeadersPriority != 0)
	}

	return nil
}

func (f *HeadersFrame) WritePayload(w io.Writer) (int64, error) {
	var totalN int64
	if f.Flags&FlagHeadersPadded != 0 {
		n, err := w.Write([]byte{f.PadLength})
		if err != nil {
			return totalN, err
		}
		totalN += int64(n)
	}
	if f.Flags&FlagHeadersPriority != 0 {
		var prioBuf [5]byte
		streamDepAndE := f.StreamDependency
		if f.Exclusive {
			streamDepAndE |= (1 << 31)
		}
		binary.BigEndian.PutUint32(prioBuf[0:4], streamDepAndE)
		prioBuf[4] = f.Weight
		n, err := w.Write(prioBuf[:])
		if err != nil {
			return totalN, err
		}
		totalN += int64(n)
	}
	n, err := w.Write(f.HeaderBlockFragment)
	if err != nil {
		return totalN, err
	}
	totalN += int64(n)
	if f.Flags&FlagHeadersPadded != 0 {
		n, err := w.Write(f.Padding)
		if err != nil {
			return totalN, err
		}
		totalN += int64(n)
	}
	return totalN, nil
}

// PriorityFrame represents an HTTP/2 PRIORITY frame.
// RFC 7540, Section 6.3
type PriorityFrame struct {
	FrameHeader
	Exclusive        bool   // E: 1 bit
	StreamDependency uint32 // 31 bits
	Weight           uint8  // 8 bits
}

func (f *PriorityFrame) Header() *FrameHeader { return &f.FrameHeader }

func (f *PriorityFrame) ParsePayload(r io.Reader, header FrameHeader) error {
	f.FrameHeader = header
	if f.Length != 5 {
		return fmt.Errorf("PRIORITY frame payload must be 5 bytes, got %d", f.Length)
	}
	var payload [5]byte
	if _, err := io.ReadFull(r, payload[:]); err != nil {
		return fmt.Errorf("reading PRIORITY payload: %w", err)
	}
	streamDepAndE := binary.BigEndian.Uint32(payload[0:4])
	f.Exclusive = (streamDepAndE >> 31) == 1
	f.StreamDependency = streamDepAndE & 0x7FFFFFFF
	f.Weight = payload[4]
	return nil
}

func (f *PriorityFrame) WritePayload(w io.Writer) (int64, error) {
	var payload [5]byte
	streamDepAndE := f.StreamDependency
	if f.Exclusive {
		streamDepAndE |= (1 << 31)
	}
	binary.BigEndian.PutUint32(payload[0:4], streamDepAndE)
	payload[4] = f.Weight
	n, err := w.Write(payload[:])
	return int64(n), err
}

// RSTStreamFrame represents an HTTP/2 RST_STREAM frame.
// RFC 7540, Section 6.4
type RSTStreamFrame struct {
	FrameHeader
	ErrorCode ErrorCode
}

func (f *RSTStreamFrame) Header() *FrameHeader { return &f.FrameHeader }

func (f *RSTStreamFrame) ParsePayload(r io.Reader, header FrameHeader) error {
	f.FrameHeader = header
	if f.Length != 4 {
		return fmt.Errorf("RST_STREAM frame payload must be 4 bytes, got %d", f.Length)
	}
	var errCodeBuf [4]byte
	if _, err := io.ReadFull(r, errCodeBuf[:]); err != nil {
		return fmt.Errorf("reading RST_STREAM error code: %w", err)
	}
	f.ErrorCode = ErrorCode(binary.BigEndian.Uint32(errCodeBuf[:]))
	return nil
}

func (f *RSTStreamFrame) WritePayload(w io.Writer) (int64, error) {
	var errCodeBuf [4]byte
	binary.BigEndian.PutUint32(errCodeBuf[:], uint32(f.ErrorCode))
	n, err := w.Write(errCodeBuf[:])
	return int64(n), err
}

// Setting represents a single setting in a SETTINGS frame.
type Setting struct {
	ID    SettingID
	Value uint32
}

const settingEntrySize = 6 // 2 bytes for ID, 4 bytes for Value

// SettingsFrame represents an HTTP/2 SETTINGS frame.
// RFC 7540, Section 6.5
type SettingsFrame struct {
	FrameHeader
	Settings []Setting
}

func (f *SettingsFrame) Header() *FrameHeader { return &f.FrameHeader }

func (f *SettingsFrame) ParsePayload(r io.Reader, header FrameHeader) error {
	f.FrameHeader = header
	if f.Flags&FlagSettingsAck != 0 && f.Length != 0 {
		return fmt.Errorf("SETTINGS ACK frame must have a payload length of 0, got %d", f.Length)
	}
	if f.Length%settingEntrySize != 0 {
		return fmt.Errorf("SETTINGS frame payload length %d is not a multiple of %d", f.Length, settingEntrySize)
	}

	numSettings := f.Length / settingEntrySize
	f.Settings = make([]Setting, 0, numSettings)

	// If f.Length is 0 (e.g., for an ACK frame or a non-ACK frame with no settings),
	// buf will be empty, and io.ReadFull will correctly do nothing and return nil error.
	// The subsequent loop for parsing settings will not run as numSettings will be 0.
	buf := make([]byte, f.Length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return fmt.Errorf("reading SETTINGS payload: %w", err)
	}

	for i := 0; i < int(numSettings); i++ {
		offset := i * settingEntrySize
		id := SettingID(binary.BigEndian.Uint16(buf[offset : offset+2]))
		value := binary.BigEndian.Uint32(buf[offset+2 : offset+6])
		f.Settings = append(f.Settings, Setting{ID: id, Value: value})
	}
	return nil
}

func (f *SettingsFrame) WritePayload(w io.Writer) (int64, error) {
	if f.Flags&FlagSettingsAck != 0 {
		return 0, nil // No payload for ACK
	}
	var totalN int64
	buf := make([]byte, settingEntrySize)
	for _, setting := range f.Settings {
		binary.BigEndian.PutUint16(buf[0:2], uint16(setting.ID))
		binary.BigEndian.PutUint32(buf[2:6], setting.Value)
		n, err := w.Write(buf)
		if err != nil {
			return totalN, err
		}
		totalN += int64(n)
	}
	return totalN, nil
}

// PushPromiseFrame represents an HTTP/2 PUSH_PROMISE frame.
// RFC 7540, Section 6.6
type PushPromiseFrame struct {
	FrameHeader
	PadLength           uint8  // Only present if FlagPushPromisePadded is set
	PromisedStreamID    uint32 // 31 bits (R is 1 bit)
	HeaderBlockFragment []byte
	Padding             []byte // Only present if FlagPushPromisePadded is set
}

func (f *PushPromiseFrame) Header() *FrameHeader { return &f.FrameHeader }

func (f *PushPromiseFrame) ParsePayload(r io.Reader, header FrameHeader) error {
	f.FrameHeader = header
	payloadLen := f.Length
	currentPos := 0

	if f.Flags&FlagPushPromisePadded != 0 {
		var padLenBuf [1]byte
		if _, err := io.ReadFull(r, padLenBuf[:]); err != nil {
			return fmt.Errorf("reading pad length: %w", err)
		}
		f.PadLength = padLenBuf[0]
		payloadLen--
		currentPos++
		if uint32(f.PadLength) > payloadLen { // Check against remaining payload
			return fmt.Errorf("pad length %d exceeds remaining payload length %d", f.PadLength, payloadLen)
		}
	}

	if payloadLen < 4 { // Need at least 4 bytes for PromisedStreamID
		return fmt.Errorf("payload too short for PromisedStreamID: %d", payloadLen)
	}
	var streamIDBuf [4]byte
	if _, err := io.ReadFull(r, streamIDBuf[:]); err != nil {
		return fmt.Errorf("reading promised stream ID: %w", err)
	}
	f.PromisedStreamID = binary.BigEndian.Uint32(streamIDBuf[:]) & 0x7FFFFFFF // Mask R bit
	payloadLen -= 4
	currentPos += 4

	headerBlockLen := payloadLen
	if f.Flags&FlagPushPromisePadded != 0 {
		headerBlockLen -= uint32(f.PadLength)
	}

	f.HeaderBlockFragment = make([]byte, headerBlockLen)
	if _, err := io.ReadFull(r, f.HeaderBlockFragment); err != nil {
		return fmt.Errorf("reading header block fragment: %w", err)
	}
	currentPos += int(headerBlockLen)

	if f.Flags&FlagPushPromisePadded != 0 {
		f.Padding = make([]byte, f.PadLength)
		if _, err := io.ReadFull(r, f.Padding); err != nil {
			return fmt.Errorf("reading padding: %w", err)
		}
		currentPos += int(f.PadLength)
	}

	if uint32(currentPos) != f.Length {
		return fmt.Errorf("mismatch in parsed payload length for PUSH_PROMISE: expected %d, got %d", f.Length, currentPos)
	}
	return nil
}

func (f *PushPromiseFrame) WritePayload(w io.Writer) (int64, error) {
	var totalN int64
	if f.Flags&FlagPushPromisePadded != 0 {
		n, err := w.Write([]byte{f.PadLength})
		if err != nil {
			return totalN, err
		}
		totalN += int64(n)
	}

	var streamIDBuf [4]byte
	binary.BigEndian.PutUint32(streamIDBuf[:], f.PromisedStreamID&0x7FFFFFFF) // Ensure R bit is 0
	n, err := w.Write(streamIDBuf[:])
	if err != nil {
		return totalN, err
	}
	totalN += int64(n)

	n, err = w.Write(f.HeaderBlockFragment)
	if err != nil {
		return totalN, err
	}
	totalN += int64(n)

	if f.Flags&FlagPushPromisePadded != 0 {
		n, err := w.Write(f.Padding)
		if err != nil {
			return totalN, err
		}
		totalN += int64(n)
	}
	return totalN, nil
}

// PingFrame represents an HTTP/2 PING frame.
// RFC 7540, Section 6.7
type PingFrame struct {
	FrameHeader
	OpaqueData [8]byte
}

func (f *PingFrame) Header() *FrameHeader { return &f.FrameHeader }

func (f *PingFrame) ParsePayload(r io.Reader, header FrameHeader) error {
	f.FrameHeader = header
	if f.Length != 8 {
		return fmt.Errorf("PING frame payload must be 8 bytes, got %d", f.Length)
	}
	if _, err := io.ReadFull(r, f.OpaqueData[:]); err != nil {
		return fmt.Errorf("reading PING opaque data: %w", err)
	}
	return nil
}

func (f *PingFrame) WritePayload(w io.Writer) (int64, error) {
	n, err := w.Write(f.OpaqueData[:])
	return int64(n), err
}

// GoAwayFrame represents an HTTP/2 GOAWAY frame.
// RFC 7540, Section 6.8
type GoAwayFrame struct {
	FrameHeader
	LastStreamID        uint32 // 31 bits (R is 1 bit)
	ErrorCode           ErrorCode
	AdditionalDebugData []byte
}

func (f *GoAwayFrame) Header() *FrameHeader { return &f.FrameHeader }

func (f *GoAwayFrame) ParsePayload(r io.Reader, header FrameHeader) error {
	f.FrameHeader = header
	if f.Length < 8 { // LastStreamID (4) + ErrorCode (4)
		return fmt.Errorf("GOAWAY frame payload must be at least 8 bytes, got %d", f.Length)
	}
	var fixedPart [8]byte
	if _, err := io.ReadFull(r, fixedPart[:]); err != nil {
		return fmt.Errorf("reading GOAWAY fixed part: %w", err)
	}
	f.LastStreamID = binary.BigEndian.Uint32(fixedPart[0:4]) & 0x7FFFFFFF // Mask R bit
	f.ErrorCode = ErrorCode(binary.BigEndian.Uint32(fixedPart[4:8]))

	debugDataLen := f.Length - 8
	if debugDataLen > 0 {
		f.AdditionalDebugData = make([]byte, debugDataLen)
		if _, err := io.ReadFull(r, f.AdditionalDebugData); err != nil {
			return fmt.Errorf("reading GOAWAY additional debug data: %w", err)
		}
	}
	return nil
}

func (f *GoAwayFrame) WritePayload(w io.Writer) (int64, error) {
	var totalN int64
	var fixedPart [8]byte
	binary.BigEndian.PutUint32(fixedPart[0:4], f.LastStreamID&0x7FFFFFFF) // Ensure R bit is 0
	binary.BigEndian.PutUint32(fixedPart[4:8], uint32(f.ErrorCode))

	n, err := w.Write(fixedPart[:])
	if err != nil {
		return totalN, err
	}
	totalN += int64(n)

	if len(f.AdditionalDebugData) > 0 {
		n, err := w.Write(f.AdditionalDebugData)
		if err != nil {
			return totalN, err
		}
		totalN += int64(n)
	}
	return totalN, nil
}

// WindowUpdateFrame represents an HTTP/2 WINDOW_UPDATE frame.
// RFC 7540, Section 6.9
type WindowUpdateFrame struct {
	FrameHeader
	WindowSizeIncrement uint32 // 31 bits (R is 1 bit)
}

func (f *WindowUpdateFrame) Header() *FrameHeader { return &f.FrameHeader }

func (f *WindowUpdateFrame) ParsePayload(r io.Reader, header FrameHeader) error {
	f.FrameHeader = header
	if f.Length != 4 {
		return fmt.Errorf("WINDOW_UPDATE frame payload must be 4 bytes, got %d", f.Length)
	}
	var incrementBuf [4]byte
	if _, err := io.ReadFull(r, incrementBuf[:]); err != nil {
		return fmt.Errorf("reading WINDOW_UPDATE increment: %w", err)
	}
	f.WindowSizeIncrement = binary.BigEndian.Uint32(incrementBuf[:]) & 0x7FFFFFFF // Mask R bit
	if f.WindowSizeIncrement == 0 {
		// This is a protocol error for stream-specific window updates.
		// For connection-level, it's not explicitly forbidden but usually means an issue.
		// For now, we just parse it. Validation is a higher-level concern.
		// return fmt.Errorf("WINDOW_UPDATE increment must be non-zero")
	}
	return nil
}

func (f *WindowUpdateFrame) WritePayload(w io.Writer) (int64, error) {
	var incrementBuf [4]byte
	binary.BigEndian.PutUint32(incrementBuf[:], f.WindowSizeIncrement&0x7FFFFFFF) // Ensure R bit is 0
	n, err := w.Write(incrementBuf[:])
	return int64(n), err
}

// ContinuationFrame represents an HTTP/2 CONTINUATION frame.
// RFC 7540, Section 6.10
type ContinuationFrame struct {
	FrameHeader
	HeaderBlockFragment []byte
}

func (f *ContinuationFrame) Header() *FrameHeader { return &f.FrameHeader }

func (f *ContinuationFrame) ParsePayload(r io.Reader, header FrameHeader) error {
	f.FrameHeader = header
	f.HeaderBlockFragment = make([]byte, f.Length)
	if _, err := io.ReadFull(r, f.HeaderBlockFragment); err != nil {
		return fmt.Errorf("reading CONTINUATION header block fragment: %w", err)
	}
	return nil
}

func (f *ContinuationFrame) WritePayload(w io.Writer) (int64, error) {
	n, err := w.Write(f.HeaderBlockFragment)
	return int64(n), err
}

// UnknownFrame is used for frames whose type is not recognized or supported.
// The payload is kept opaque.
type UnknownFrame struct {
	FrameHeader
	Payload []byte
}

func (f *UnknownFrame) Header() *FrameHeader { return &f.FrameHeader }

func (f *UnknownFrame) ParsePayload(r io.Reader, header FrameHeader) error {
	f.FrameHeader = header
	f.Payload = make([]byte, f.Length)
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		return fmt.Errorf("reading UnknownFrame payload: %w", err)
	}
	return nil
}

func (f *UnknownFrame) WritePayload(w io.Writer) (int64, error) {
	n, err := w.Write(f.Payload)
	return int64(n), err
}

// ReadFrame reads a full HTTP/2 frame from the reader.
// It first reads the header, then determines the frame type and reads the payload.
func ReadFrame(r io.Reader) (Frame, error) {
	fh, err := ReadFrameHeader(r)
	if err != nil {
		return nil, fmt.Errorf("reading frame header: %w", err)
	}

	var frame Frame
	switch fh.Type {
	case FrameData:
		frame = &DataFrame{}
	case FrameHeaders:
		frame = &HeadersFrame{}
	case FramePriority:
		frame = &PriorityFrame{}
	case FrameRSTStream:
		frame = &RSTStreamFrame{}
	case FrameSettings:
		frame = &SettingsFrame{}
	case FramePushPromise:
		frame = &PushPromiseFrame{}
	case FramePing:
		frame = &PingFrame{}
	case FrameGoAway:
		frame = &GoAwayFrame{}
	case FrameWindowUpdate:
		frame = &WindowUpdateFrame{}
	case FrameContinuation:
		frame = &ContinuationFrame{}
	default:
		// RFC 7540, Section 4.1: "Implementations MUST ignore and discard any frame that has a type that is unknown."
		// Further, "Unknown frame types are not errors."
		// This implementation handles this by parsing the frame into an UnknownFrame instance.
		// The UnknownFrame.ParsePayload method will read and effectively "discard" the payload
		// from the input stream by consuming it.
		// The caller of ReadFrame can then choose to type-assert for *UnknownFrame and explicitly ignore it
		// or log its occurrence if desired for debugging, without treating it as a parsing error for the frame itself.
		// If an unknown frame type is received in a context where it is not permitted (e.g., certain frames on stream 0
		// after the connection preface), it is the responsibility of higher-level logic (e.g., the connection or stream manager)
		// to identify this as a protocol violation and
		// to identify this as a protocol violation and potentially issue a CONNECTION_ERROR(PROTOCOL_ERROR).
		frame = &UnknownFrame{}
	}

	err = frame.ParsePayload(r, fh)
	if err != nil {
		return nil, fmt.Errorf("parsing %s payload: %w", fh.Type, err)
	}
	return frame, nil
}

// WriteFrame writes a full HTTP/2 frame to the writer.
// It first writes the header, then the specific frame's payload.
func WriteFrame(w io.Writer, f Frame) error {
	header := f.Header()
	// Ensure length is set correctly based on payload.
	// This requires payload to be fully defined before header is written.
	// For frames like DataFrame, HeadersFrame, etc., where payload size can vary,
	// the Length field in the header must be pre-calculated.
	// For now, we assume FrameHeader.Length is correctly set by the caller constructing the frame.

	// A better approach might be to have WritePayload return its length,
	// then construct and write the header, then write the payload.
	// However, current structure has WritePayload take io.Writer.
	// Let's assume header.Length is already correctly set by the frame construction logic.

	_, err := header.WriteTo(w)
	if err != nil {
		return fmt.Errorf("writing frame header for %s: %w", header.Type, err)
	}

	_, err = f.WritePayload(w)
	if err != nil {
		return fmt.Errorf("writing %s payload: %w", header.Type, err)
	}
	return nil
}

package http2

import (
	"encoding/binary"

	"fmt"
	"io"
	"strings" // Added import
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
	PayloadLen() uint32 // New method
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
	if header.StreamID == 0 {
		return NewConnectionError(ErrCodeProtocolError, "received DATA on stream 0")
	}

	payloadTotalDeclaredInHeader := header.Length
	var actualDataLength uint32
	var paddingOctetsToRead uint32 // How many padding octets to read from the stream

	if f.Flags&FlagDataPadded != 0 {
		if payloadTotalDeclaredInHeader == 0 {
			// If PADDED flag is set, the frame MUST contain at least one byte for PadLength.
			// So, a total payload length of 0 is invalid.
			return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("padded DATA frame for stream %d has invalid declared payload length 0", header.StreamID))
		}

		var padLenFieldAsByte [1]byte
		if _, err := io.ReadFull(r, padLenFieldAsByte[:]); err != nil {
			// This means we couldn't even read the PadLength byte itself.
			return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("DATA frame (stream %d) too short to contain PadLength field: %v", header.StreamID, err))
		}
		f.PadLength = padLenFieldAsByte[0] // Store the PadLength *value* from the 1-byte field

		// At this point, 1 byte (PadLength field) has been consumed from payloadTotalDeclaredInHeader.
		// The remaining length is payloadTotalDeclaredInHeader - 1.
		// This remaining length must accommodate f.PadLength (the padding itself) and the actual data.
		// So, (payloadTotalDeclaredInHeader - 1) must be >= f.PadLength.
		// If f.PadLength > (payloadTotalDeclaredInHeader - 1), it's an error.
		// This is equivalent to (f.PadLength + 1) > payloadTotalDeclaredInHeader.
		if uint32(f.PadLength)+1 > payloadTotalDeclaredInHeader {
			// This means PadLength value itself + 1 byte for the PadLength field > total payload.
			// Or, equivalently, f.PadLength >= payloadTotalDeclaredInHeader
			errorMsg := fmt.Sprintf("DATA frame (stream %d) invalid padding: PadLength field value %d requires %d octets for padding, but total declared payload is only %d octets (PadLength field + Data + Padding). This implies PadLength field value is too large.", header.StreamID, f.PadLength, uint32(f.PadLength)+1, payloadTotalDeclaredInHeader)
			return NewConnectionError(ErrCodeProtocolError, errorMsg)
		}

		// Now that f.PadLength value is validated against the total payload, calculate lengths
		actualDataLength = payloadTotalDeclaredInHeader - 1 - uint32(f.PadLength)
		paddingOctetsToRead = uint32(f.PadLength)

	} else { // Not Padded
		actualDataLength = payloadTotalDeclaredInHeader
		paddingOctetsToRead = 0
		f.PadLength = 0 // Ensure f.PadLength struct field is 0 if not padded.
	}

	// Read the actual data payload
	if actualDataLength > 0 {
		f.Data = make([]byte, actualDataLength)
		if _, err := io.ReadFull(r, f.Data); err != nil {
			return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("error reading DATA frame payload data for stream %d: %v", header.StreamID, err))
		}
	} else {
		f.Data = []byte{} // Ensure f.Data is empty non-nil slice if actualDataLength is 0
	}

	// Read padding if PADDED flag was set and there are padding octets specified by f.PadLength
	if f.Flags&FlagDataPadded != 0 {
		if paddingOctetsToRead > 0 {
			f.Padding = make([]byte, paddingOctetsToRead)
			if _, err := io.ReadFull(r, f.Padding); err != nil {
				return NewConnectionError(ErrCodeProtocolError, fmt.Sprintf("error reading DATA frame payload padding for stream %d: %v", header.StreamID, err))
			}
		} else {
			// PadLength field was 0 (but PADDED flag was set).
			// This means there's a PadLength field of 1 byte (value 0), and 0 padding octets.
			f.Padding = []byte{}
		}
	} else {
		f.Padding = nil // Not padded, so no padding bytes
	}
	return nil
}

func (f *DataFrame) WritePayload(w io.Writer) (int64, error) {
	var totalN int64
	var nWrite int // Use int for Write return, then cast to int64
	var err error

	if f.Flags&FlagDataPadded != 0 {
		nWrite, err = w.Write([]byte{f.PadLength})
		totalN += int64(nWrite)
		if err != nil {
			return totalN, err
		}
	}
	nWrite, err = w.Write(f.Data)
	totalN += int64(nWrite)
	if err != nil {
		return totalN, err
	}

	if f.Flags&FlagDataPadded != 0 {
		nWrite, err = w.Write(f.Padding)
		totalN += int64(nWrite)
		if err != nil {
			return totalN, err
		}
	}
	return totalN, nil
}

func (f *DataFrame) PayloadLen() uint32 {
	var length uint32
	if f.Flags&FlagDataPadded != 0 {
		length += 1                   // PadLength field
		length += uint32(f.PadLength) // Padding bytes
	}
	length += uint32(len(f.Data))
	return length
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
	var nWrite int
	var err error

	if f.Flags&FlagHeadersPadded != 0 {
		nWrite, err = w.Write([]byte{f.PadLength})
		totalN += int64(nWrite)
		if err != nil {
			return totalN, err
		}
	}
	if f.Flags&FlagHeadersPriority != 0 {
		var prioBuf [5]byte
		streamDepAndE := f.StreamDependency
		if f.Exclusive {
			streamDepAndE |= (1 << 31)
		}
		binary.BigEndian.PutUint32(prioBuf[0:4], streamDepAndE)
		prioBuf[4] = f.Weight
		nWrite, err = w.Write(prioBuf[:])
		totalN += int64(nWrite)
		if err != nil {
			return totalN, err
		}
	}
	nWrite, err = w.Write(f.HeaderBlockFragment)
	totalN += int64(nWrite)
	if err != nil {
		return totalN, err
	}
	if f.Flags&FlagHeadersPadded != 0 {
		nWrite, err = w.Write(f.Padding)
		totalN += int64(nWrite)
		if err != nil {
			return totalN, err
		}
	}
	return totalN, nil
}

// PriorityFrame represents an HTTP/2 PRIORITY frame.

func (f *HeadersFrame) PayloadLen() uint32 {
	var length uint32
	if f.Flags&FlagHeadersPadded != 0 {
		length += 1                   // PadLength field
		length += uint32(f.PadLength) // Padding bytes
	}
	if f.Flags&FlagHeadersPriority != 0 {
		length += 5 // StreamDependency, Exclusive, Weight
	}
	length += uint32(len(f.HeaderBlockFragment))
	return length
}

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
		errMsg := fmt.Sprintf("PRIORITY frame payload must be 5 bytes, got %d", f.Length)
		if header.StreamID == 0 {
			// Per task instruction for length validation failure on stream 0
			return NewConnectionError(ErrCodeFrameSizeError, errMsg)
		}
		return NewStreamError(header.StreamID, ErrCodeFrameSizeError, errMsg)
	}
	var payload [5]byte
	if _, err := io.ReadFull(r, payload[:]); err != nil {
		// This is an I/O error during payload read, not a frame size error related to declared length
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

func (f *PriorityFrame) PayloadLen() uint32 {
	return 5 // Exclusive, StreamDependency, Weight
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
		// "A RST_STREAM frame with a length other than 4 octets MUST be treated as a
		// connection error (Section 5.4.1) of type FRAME_SIZE_ERROR."
		return NewConnectionError(ErrCodeFrameSizeError, fmt.Sprintf("RST_STREAM frame payload must be 4 bytes, got %d", f.Length))
	}
	var errCodeBuf [4]byte
	if _, err := io.ReadFull(r, errCodeBuf[:]); err != nil {
		// This is an I/O error during payload read, not a frame size error related to declared length
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

func (f *RSTStreamFrame) PayloadLen() uint32 {
	return 4 // ErrorCode
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
		// "A SETTINGS frame with the ACK flag set and a length field value other than 0
		// MUST be treated as a connection error (Section 5.4.1) of type FRAME_SIZE_ERROR."
		return NewConnectionError(ErrCodeFrameSizeError, fmt.Sprintf("SETTINGS ACK frame must have a payload length of 0, got %d", f.Length))
	}
	if f.Length%settingEntrySize != 0 {
		// "SETTINGS frames with a length other than a multiple of 6 octets MUST be
		// treated as a connection error (Section 5.4.1) of type FRAME_SIZE_ERROR."
		// This aligns with RFC 7540, Section 6.5.
		return NewConnectionError(ErrCodeFrameSizeError, fmt.Sprintf("SETTINGS frame payload length %d is not a multiple of %d", f.Length, settingEntrySize))
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
	var nWrite int
	var err error
	buf := make([]byte, settingEntrySize)

	for _, setting := range f.Settings {
		binary.BigEndian.PutUint16(buf[0:2], uint16(setting.ID))
		binary.BigEndian.PutUint32(buf[2:6], setting.Value)
		nWrite, err = w.Write(buf)
		totalN += int64(nWrite)
		if err != nil {
			return totalN, err
		}
	}
	return totalN, nil
}

func (f *SettingsFrame) PayloadLen() uint32 {
	if f.Flags&FlagSettingsAck != 0 {
		return 0
	}
	return uint32(len(f.Settings) * settingEntrySize)
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
	if header.StreamID == 0 {
		return NewConnectionError(ErrCodeProtocolError, "received PUSH_PROMISE on stream 0")
	}
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
	var nWrite int
	var err error

	if f.Flags&FlagPushPromisePadded != 0 {
		nWrite, err = w.Write([]byte{f.PadLength})
		totalN += int64(nWrite)
		if err != nil {
			return totalN, err
		}
	}

	var streamIDBuf [4]byte
	binary.BigEndian.PutUint32(streamIDBuf[:], f.PromisedStreamID&0x7FFFFFFF) // Ensure R bit is 0
	nWrite, err = w.Write(streamIDBuf[:])
	totalN += int64(nWrite)
	if err != nil {
		return totalN, err
	}

	nWrite, err = w.Write(f.HeaderBlockFragment)
	totalN += int64(nWrite)
	if err != nil {
		return totalN, err
	}

	if f.Flags&FlagPushPromisePadded != 0 {
		nWrite, err = w.Write(f.Padding)
		totalN += int64(nWrite)
		if err != nil {
			return totalN, err
		}
	}
	return totalN, nil
}

func (f *PushPromiseFrame) PayloadLen() uint32 {
	var length uint32
	if f.Flags&FlagPushPromisePadded != 0 {
		length += 1                   // PadLength field
		length += uint32(f.PadLength) // Padding bytes
	}
	length += 4 // PromisedStreamID
	length += uint32(len(f.HeaderBlockFragment))
	return length
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
		return NewConnectionError(ErrCodeFrameSizeError, fmt.Sprintf("PING frame payload must be 8 bytes, got %d", f.Length))
	}
	if _, err := io.ReadFull(r, f.OpaqueData[:]); err != nil {
		// This would be an I/O error, not a frame size error related to the declared length.
		// The spec (RFC 7540, Section 6.7) says for incorrect length:
		// "A PING frame with a length field value other than 8 MUST be
		//  treated as a connection error (Section 5.4.1) of type FRAME_SIZE_ERROR."
		// This is already handled by the check above for f.Length.
		// If ReadFull fails after length check, it's an I/O issue during payload read.
		return fmt.Errorf("reading PING opaque data: %w", err)
	}
	return nil
}

func (f *PingFrame) WritePayload(w io.Writer) (int64, error) {
	n, err := w.Write(f.OpaqueData[:])
	return int64(n), err
}

func (f *PingFrame) PayloadLen() uint32 {
	return 8 // OpaqueData
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
		return NewConnectionError(ErrCodeFrameSizeError, fmt.Sprintf("GOAWAY frame payload must be at least 8 bytes, got %d", f.Length))
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
	} else {
		// Ensure AdditionalDebugData is an empty non-nil slice if no debug data is present,
		// for consistency in comparisons (e.g. reflect.DeepEqual).
		f.AdditionalDebugData = []byte{}
	}
	return nil
}

func (f *GoAwayFrame) WritePayload(w io.Writer) (int64, error) {
	var totalN int64
	var nWrite int
	var err error
	var fixedPart [8]byte

	binary.BigEndian.PutUint32(fixedPart[0:4], f.LastStreamID&0x7FFFFFFF) // Ensure R bit is 0
	binary.BigEndian.PutUint32(fixedPart[4:8], uint32(f.ErrorCode))

	nWrite, err = w.Write(fixedPart[:])
	totalN += int64(nWrite)
	if err != nil {
		return totalN, err
	}

	if len(f.AdditionalDebugData) > 0 {
		nWrite, err = w.Write(f.AdditionalDebugData)
		totalN += int64(nWrite)
		if err != nil {
			return totalN, err
		}
	}
	return totalN, nil
}

func (f *GoAwayFrame) PayloadLen() uint32 {
	return 8 + uint32(len(f.AdditionalDebugData)) // LastStreamID (4) + ErrorCode (4) + AdditionalDebugData
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
		// RFC 7540, Section 6.9: "A WINDOW_UPDATE frame with a length
		// other than 4 octets MUST be treated as a connection error
		// (Section 5.4.1) of type FRAME_SIZE_ERROR."
		return NewConnectionError(ErrCodeFrameSizeError, fmt.Sprintf("WINDOW_UPDATE frame payload must be 4 bytes, got %d", f.Length))
	}
	var incrementBuf [4]byte
	if _, err := io.ReadFull(r, incrementBuf[:]); err != nil {
		// This is an I/O error during payload read, not a frame size error related to declared length.
		return fmt.Errorf("reading WINDOW_UPDATE increment: %w", err)
	}
	f.WindowSizeIncrement = binary.BigEndian.Uint32(incrementBuf[:]) & 0x7FFFFFFF // Mask R bit
	// A WindowSizeIncrement of 0 is a PROTOCOL_ERROR if the StreamID is not 0.
	// This validation is handled at a higher level (e.g. connection or stream logic)
	// as per RFC 7540, Section 6.9.1.
	// The frame parsing layer itself considers a zero increment structurally valid if length is correct.
	return nil
}

func (f *WindowUpdateFrame) WritePayload(w io.Writer) (int64, error) {
	var incrementBuf [4]byte
	binary.BigEndian.PutUint32(incrementBuf[:], f.WindowSizeIncrement&0x7FFFFFFF) // Ensure R bit is 0
	n, err := w.Write(incrementBuf[:])
	return int64(n), err
}

func (f *WindowUpdateFrame) PayloadLen() uint32 {
	return 4 // WindowSizeIncrement
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
	if header.StreamID == 0 {
		return NewConnectionError(ErrCodeProtocolError, "received CONTINUATION on stream 0")
	}
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

func (f *ContinuationFrame) PayloadLen() uint32 {
	return uint32(len(f.HeaderBlockFragment))
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

func (f *UnknownFrame) PayloadLen() uint32 {
	return uint32(len(f.Payload))
}

// ReadFrame reads a full HTTP/2 frame from the reader.
// It first reads the header, then determines the frame type and reads the payload.
func ReadFrame(r io.Reader) (Frame, error) {
	fh, err := ReadFrameHeader(r)
	if err != nil {
		return nil, err // Propagate raw error
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
		// Specific error handling for PRIORITY frame length
		if fh.Type == FramePriority && strings.Contains(err.Error(), "PRIORITY frame payload must be 5 bytes") {
			// ParsePayload for PriorityFrame already returns NewConnectionError or NewStreamError.
			// This check is to ensure if a generic error was somehow passed up, it's typed.
			// However, the current PriorityFrame.ParsePayload returns:
			// - NewConnectionError(ErrCodeFrameSizeError, ...) if header.StreamID == 0
			// - NewStreamError(header.StreamID, ErrCodeFrameSizeError, ...) if header.StreamID > 0
			// So, err from ParsePayload should already be typed. This specific block might be redundant
			// if PriorityFrame.ParsePayload is guaranteed to return these typed errors.
			// For safety, or if ParsePayload could return a more generic fmt.Errorf, we keep it.
			// Let's assume err might be a wrapper, so we check contents.
			if fh.StreamID > 0 {
				return nil, NewStreamError(fh.StreamID, ErrCodeFrameSizeError, err.Error())
			}
			// If fh.StreamID == 0, PriorityFrame.ParsePayload should already return ConnectionError.
			// If it somehow didn't, this would become a generic error.
			// Fallthrough to generic error wrapping is okay if err isn't FrameSizeError from ParsePayload.
		}

		// Convert generic errors from ParsePayload to specific ConnectionErrors for frame size issues.
		// RSTStreamFrame.ParsePayload returns NewConnectionError directly.
		// PingFrame.ParsePayload returns NewConnectionError directly.
		// WindowUpdateFrame.ParsePayload returns NewConnectionError directly.
		// SettingsFrame.ParsePayload (for ACK with payload, or non-multiple of 6) returns NewConnectionError directly.

		// The following checks are defensive, in case ParsePayload implementations change or if err is a wrapped generic error.
		if fh.Type == FrameRSTStream && strings.Contains(err.Error(), "RST_STREAM frame payload must be 4 bytes") {
			return nil, NewConnectionError(ErrCodeFrameSizeError, err.Error())
		}
		if fh.Type == FrameWindowUpdate && strings.Contains(err.Error(), "WINDOW_UPDATE frame payload must be 4 bytes") {
			return nil, NewConnectionError(ErrCodeFrameSizeError, err.Error())
		}
		if fh.Type == FramePing && strings.Contains(err.Error(), "PING frame payload must be 8 bytes") {
			return nil, NewConnectionError(ErrCodeFrameSizeError, err.Error())
		}
		if fh.Type == FrameSettings && (fh.Flags&FlagSettingsAck != 0) && strings.Contains(err.Error(), "SETTINGS ACK frame must have a payload length of 0") {
			return nil, NewConnectionError(ErrCodeFrameSizeError, err.Error())
		}
		if fh.Type == FrameSettings && strings.Contains(err.Error(), "SETTINGS frame payload length") && strings.Contains(err.Error(), "is not a multiple of") {
			return nil, NewConnectionError(ErrCodeFrameSizeError, err.Error())
		}

		// If err is already a StreamError or ConnectionError, return it directly.
		// This is important because ParsePayload for several frames already returns these typed errors.
		if _, ok := err.(*StreamError); ok {
			return nil, err
		}
		if _, ok := err.(*ConnectionError); ok {
			return nil, err
		}

		// If not specifically converted or already typed, wrap generically.
		return nil, fmt.Errorf("parsing %s payload: %w", fh.Type, err)
	}
	return frame, nil
}

// WriteFrame writes a full HTTP/2 frame to the writer.
// It first writes the header, then the specific frame's payload.
func WriteFrame(w io.Writer, f Frame) error {
	header := f.Header()

	// Calculate payload length using the new PayloadLen method.
	// This length is what the frame *should* have as its payload.
	calculatedPayloadLen := f.PayloadLen()
	header.Length = calculatedPayloadLen // Set this in the header to be written.

	// Serialize and write the header.
	// The FrameHeader.WriteTo method uses header.Length to fill the raw header bytes.
	_, err := header.WriteTo(w)
	if err != nil {
		return fmt.Errorf("writing frame header for %s (length %d): %w", header.Type, header.Length, err)
	}

	// Serialize and write the payload.
	// The WritePayload method for each frame type is responsible for writing
	// exactly 'calculatedPayloadLen' bytes.
	writtenPayloadBytes, err := f.WritePayload(w)
	if err != nil {
		// If WritePayload itself returns an error during writing.
		return fmt.Errorf("writing %s payload (declared length %d): %w", header.Type, calculatedPayloadLen, err)
	}

	// This check ensures internal consistency: the number of bytes WritePayload
	// claims to have written must match what PayloadLen declared.
	if uint32(writtenPayloadBytes) != calculatedPayloadLen {
		return fmt.Errorf("internal: %s payload length mismatch: PayloadLen() declared %d, but WritePayload() wrote %d bytes", header.Type, calculatedPayloadLen, writtenPayloadBytes)
	}

	return nil
}

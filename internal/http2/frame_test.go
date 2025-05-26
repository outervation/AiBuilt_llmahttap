package http2_test

import (
	"bytes"
	// "encoding/binary" // Removed as not used
	"fmt"
	"io" // Needed for io.EOF, io.ErrUnexpectedEOF
	"reflect"
	"testing"

	"example.com/llmahttap/v2/internal/http2"
)

// Helper function to compare two FrameHeader structs.
// Useful because direct comparison of structs containing slices (like raw [9]byte) might not be ideal.
func assertFrameHeaderEquals(t *testing.T, expected, actual http2.FrameHeader) {
	t.Helper()
	if expected.Length != actual.Length {
		t.Errorf("FrameHeader.Length mismatch: expected %d, got %d", expected.Length, actual.Length)
	}
	if expected.Type != actual.Type {
		t.Errorf("FrameHeader.Type mismatch: expected %s, got %s", expected.Type, actual.Type)
	}
	if expected.Flags != actual.Flags {
		t.Errorf("FrameHeader.Flags mismatch: expected 0x%x, got 0x%x", expected.Flags, actual.Flags)
	}
	if expected.StreamID != actual.StreamID {
		t.Errorf("FrameHeader.StreamID mismatch: expected %d, got %d", expected.StreamID, actual.StreamID)
	}
	// The .raw field is unexported and its direct comparison is not necessary
	// if all exported fields (Length, Type, Flags, StreamID) match.
	// The correctness of serialization/deserialization of .raw is implicitly
	// tested by WriteFrameHeader and ReadFrameHeader correctly populating/using these fields.
}

// Helper to serialize a frame to bytes and then parse it back.
func testFrameSerializationLoop(t *testing.T, originalFrame http2.Frame, frameName string) http2.Frame {
	t.Helper()

	var buf bytes.Buffer
	err := http2.WriteFrame(&buf, originalFrame)
	if err != nil {
		t.Fatalf("%s WriteFrame() error = %v", frameName, err)
	}

	// Check if header length matches payload length calculation
	expectedHeaderLength := originalFrame.PayloadLen()
	if originalFrame.Header().Length != expectedHeaderLength {
		t.Errorf("%s: FrameHeader.Length (%d) does not match calculated PayloadLen() (%d)",
			frameName, originalFrame.Header().Length, expectedHeaderLength)
	}

	// Check if buffer length matches total frame length
	expectedTotalLength := http2.FrameHeaderLen + int(originalFrame.Header().Length)
	if buf.Len() != expectedTotalLength {
		t.Errorf("%s: Serialized buffer length (%d) does not match expected total frame length (%d = 9 + %d)",
			frameName, buf.Len(), expectedTotalLength, originalFrame.Header().Length)
	}

	parsedFrame, err := http2.ReadFrame(&buf)
	if err != nil {
		t.Fatalf("%s ReadFrame() error = %v", frameName, err)
	}

	if buf.Len() != 0 {
		t.Errorf("%s: Buffer not fully consumed after ReadFrame, remaining %d bytes", frameName, buf.Len())
	}
	return parsedFrame
}

// Generic test function for frame types
func testFrameType(t *testing.T, originalFrame http2.Frame, frameName string) {
	t.Helper()

	parsedFrame := testFrameSerializationLoop(t, originalFrame, frameName)

	// Type-specific comparisons
	// Use reflect.DeepEqual for payload fields, and assertFrameHeaderEquals for the header.
	// Must strip FrameHeader from comparison before DeepEqual if comparing the whole struct directly,
	// or compare fields individually.

	originalHeader := *originalFrame.Header() // Dereference to compare values
	parsedHeader := *parsedFrame.Header()

	// Compare headers using the dedicated function
	assertFrameHeaderEquals(t, originalHeader, parsedHeader)

	// Compare the rest of the frame structure.
	// We create copies of the frames and zero out their FrameHeader fields
	// before using reflect.DeepEqual. This is because FrameHeader contains
	// an unexported 'raw' byte array, which can cause DeepEqual to fail
	// even if all meaningful data is identical, as the 'raw' array might not be
	// consistently zeroed or populated the same way in the original vs. parsed
	// if frames are constructed programmatically vs. via parsing.
	// The meaningful parts of FrameHeader are already compared by assertFrameHeaderEquals.

	originalPayloadComparable := deepCopyFramePayload(originalFrame)
	parsedPayloadComparable := deepCopyFramePayload(parsedFrame)

	if !reflect.DeepEqual(originalPayloadComparable, parsedPayloadComparable) {
		t.Errorf("%s structs (payload part) not equal after serialization/deserialization loop.\nOriginal: %#v\nParsed:   %#v",
			frameName, originalPayloadComparable, parsedPayloadComparable)
	}

	// Verify PayloadLen method
	if originalFrame.PayloadLen() != parsedFrame.PayloadLen() {
		t.Errorf("%s: PayloadLen() mismatch after parse. Original: %d, Parsed: %d", frameName, originalFrame.PayloadLen(), parsedFrame.PayloadLen())
	}
	if originalFrame.Header().Length != originalFrame.PayloadLen() {
		// This check is slightly redundant if testFrameSerializationLoop already covers it,
		// but confirms PayloadLen() gives the same as what was written into the header.
		t.Errorf("%s: Original frame's Header.Length (%d) doesn't match its PayloadLen() (%d)",
			frameName, originalFrame.Header().Length, originalFrame.PayloadLen())
	}
}

// deepCopyFramePayload creates a copy of the frame, effectively for comparing payload parts.
// It doesn't really copy, more like returns the same frame.
// This helper is more about illustrating the intent to compare payload structures.
// For actual comparison, individual field checks or struct copies without uncomparable fields are better.

func deepCopyFramePayload(f http2.Frame) interface{} {
	// This function creates a copy of the frame's payload-specific parts
	// by value, zeroing out the FrameHeader. This allows reflect.DeepEqual
	// to compare the meaningful payload content without being affected by
	// the unexported 'raw' field in FrameHeader or by pointer equality.
	switch ft := f.(type) {
	case *http2.DataFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{} // Zero out header for DeepEqual on payload
		// Ensure slices are copied if they are not already by value when ft is dereferenced.
		// For []byte, assignment creates a new slice header but points to the same underlying array.
		// We need a true copy for independent comparison.
		if ft.Data != nil {
			cp.Data = make([]byte, len(ft.Data))
			copy(cp.Data, ft.Data)
		}
		if ft.Padding != nil {
			cp.Padding = make([]byte, len(ft.Padding))
			copy(cp.Padding, ft.Padding)
		}
		return cp
	case *http2.HeadersFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		if ft.HeaderBlockFragment != nil {
			cp.HeaderBlockFragment = make([]byte, len(ft.HeaderBlockFragment))
			copy(cp.HeaderBlockFragment, ft.HeaderBlockFragment)
		}
		if ft.Padding != nil {
			cp.Padding = make([]byte, len(ft.Padding))
			copy(cp.Padding, ft.Padding)
		}
		return cp
	case *http2.PriorityFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		return cp
	case *http2.RSTStreamFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		return cp
	case *http2.SettingsFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		if ft.Settings != nil {
			cp.Settings = make([]http2.Setting, len(ft.Settings))
			copy(cp.Settings, ft.Settings)
		}
		return cp
	case *http2.PushPromiseFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		if ft.HeaderBlockFragment != nil {
			cp.HeaderBlockFragment = make([]byte, len(ft.HeaderBlockFragment))
			copy(cp.HeaderBlockFragment, ft.HeaderBlockFragment)
		}
		if ft.Padding != nil {
			cp.Padding = make([]byte, len(ft.Padding))
			copy(cp.Padding, ft.Padding)
		}
		return cp
	case *http2.PingFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		// OpaqueData is an array, so simple assignment copies it.
		return cp
	case *http2.GoAwayFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		if ft.AdditionalDebugData != nil {
			cp.AdditionalDebugData = make([]byte, len(ft.AdditionalDebugData))
			copy(cp.AdditionalDebugData, ft.AdditionalDebugData)
		}
		return cp
	case *http2.WindowUpdateFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		return cp
	case *http2.ContinuationFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		if ft.HeaderBlockFragment != nil {
			cp.HeaderBlockFragment = make([]byte, len(ft.HeaderBlockFragment))
			copy(cp.HeaderBlockFragment, ft.HeaderBlockFragment)
		}
		return cp
	case *http2.UnknownFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		if ft.Payload != nil {
			cp.Payload = make([]byte, len(ft.Payload))
			copy(cp.Payload, ft.Payload)
		}
		return cp
	default:
		panic(fmt.Sprintf("unknown frame type for deep copy: %T", f))
	}
}

func TestFrameHeaderSerialization(t *testing.T) {
	fh := http2.FrameHeader{
		Length:   12345,
		Type:     http2.FrameData,
		Flags:    http2.FlagDataEndStream,
		StreamID: 67890,
	}

	var writeBuf bytes.Buffer
	_, err := fh.WriteTo(&writeBuf)
	if err != nil {
		t.Fatalf("fh.WriteTo() error = %v", err)
	}

	if writeBuf.Len() != http2.FrameHeaderLen {
		t.Fatalf("fh.WriteTo() wrote %d bytes, expected %d", writeBuf.Len(), http2.FrameHeaderLen)
	}
	originalWrittenBytes := make([]byte, http2.FrameHeaderLen)
	copy(originalWrittenBytes, writeBuf.Bytes()) // Make a copy of the written bytes

	// Create a new buffer for reading from these originalWrittenBytes
	readInputBuf := bytes.NewBuffer(originalWrittenBytes)
	parsedFH, err := http2.ReadFrameHeader(readInputBuf)
	if err != nil {
		t.Fatalf("ReadFrameHeader() error = %v", err)
	}

	// 1. Compare public fields of original and parsed header
	// This also implicitly tests that ReadFrameHeader correctly parsed public fields from raw bytes.
	assertFrameHeaderEquals(t, fh, parsedFH)

	// 2. Verify that serializing parsedFH produces the same byte sequence as originalWrittenBytes.
	// This tests that ReadFrameHeader correctly populated parsedFH (internally, including its
	// unexported 'raw' field, or at least its public fields accurately) such that
	// parsedFH.WriteTo() can reconstruct the original byte sequence.
	var reSerializedBuf bytes.Buffer
	_, err = parsedFH.WriteTo(&reSerializedBuf)
	if err != nil {
		t.Fatalf("parsedFH.WriteTo() error = %v", err)
	}

	if !bytes.Equal(originalWrittenBytes, reSerializedBuf.Bytes()) {
		t.Errorf("Re-serialized parsedFH bytes mismatch original written bytes.\nOriginal: %x\nParsedThenSerialized: %x",
			originalWrittenBytes, reSerializedBuf.Bytes())
	}
}

func TestReadFrameHeader_Errors(t *testing.T) {
	tests := []struct {
		name        string
		input       []byte
		expectedErr error
	}{
		{
			name:        "EOF immediately",
			input:       []byte{},
			expectedErr: io.EOF,
		},
		{
			name:        "short read (1 byte)",
			input:       []byte{0x00},
			expectedErr: io.ErrUnexpectedEOF,
		},
		{
			name:        "short read (FrameHeaderLen - 1 bytes)",
			input:       make([]byte, http2.FrameHeaderLen-1),
			expectedErr: io.ErrUnexpectedEOF,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewBuffer(tt.input)
			_, err := http2.ReadFrameHeader(r)
			if err == nil {
				t.Fatalf("ReadFrameHeader() expected error %v, got nil", tt.expectedErr)
			}
			// Using errors.Is for future-proofing, though direct comparison works for io.EOF/ErrUnexpectedEOF
			if !isSpecificError(err, tt.expectedErr) {
				t.Errorf("ReadFrameHeader() error mismatch: expected %v, got %v", tt.expectedErr, err)
			}
		})
	}
}

// isSpecificError checks if err is equivalent to target.
// This is a simple helper; for more complex scenarios, errors.Is or errors.As might be better.
func isSpecificError(err, target error) bool {
	if err == nil && target == nil {
		return true
	}
	if err == nil || target == nil {
		return false
	}
	return err.Error() == target.Error() || err == target // Handle sentinel errors like io.EOF
}

type failingWriter struct {
	failAfterNBytes int
	writtenBytes    int
	errToReturn     error
}

func (fw *failingWriter) Write(p []byte) (n int, err error) {
	if fw.errToReturn == nil {
		fw.errToReturn = fmt.Errorf("simulated writer error") // Default error
	}
	if fw.writtenBytes >= fw.failAfterNBytes {
		return 0, fw.errToReturn
	}

	canWrite := fw.failAfterNBytes - fw.writtenBytes
	if canWrite <= 0 { // Should not happen if writtenBytes < failAfterNBytes, but as safeguard
		return 0, fw.errToReturn
	}

	if len(p) > canWrite {
		fw.writtenBytes += canWrite
		return canWrite, fw.errToReturn
	}

	fw.writtenBytes += len(p)
	return len(p), nil
}

func TestFrameHeader_WriteTo_Error(t *testing.T) {
	fh := http2.FrameHeader{
		Length:   123,
		Type:     http2.FrameData,
		Flags:    0,
		StreamID: 1,
	}

	expectedErr := fmt.Errorf("custom writer error")

	t.Run("fail immediately", func(t *testing.T) {
		fw := &failingWriter{failAfterNBytes: 0, errToReturn: expectedErr}
		n, err := fh.WriteTo(fw)
		if err == nil {
			t.Fatal("fh.WriteTo() expected an error, got nil")
		}
		if !isSpecificError(err, expectedErr) {
			t.Errorf("fh.WriteTo() error mismatch: expected %v, got %v", expectedErr, err)
		}
		if n != 0 {
			t.Errorf("fh.WriteTo() expected 0 bytes written on immediate error, got %d", n)
		}
	})

	t.Run("fail after partial write", func(t *testing.T) {
		fw := &failingWriter{failAfterNBytes: 4, errToReturn: expectedErr}
		n, err := fh.WriteTo(fw)
		if err == nil {
			t.Fatal("fh.WriteTo() with partial write expected an error, got nil")
		}
		if !isSpecificError(err, expectedErr) {
			t.Errorf("fh.WriteTo() with partial write error mismatch: expected %v, got %v", expectedErr, err)
		}
		if n != 4 {
			t.Errorf("fh.WriteTo() with partial write expected 4 bytes written, got %d", n)
		}
	})
}

func TestContinuationFrame(t *testing.T) {
	tests := []struct {
		name          string
		frame         *http2.ContinuationFrame
		expectedError bool // For specific parse/write errors not covered by generic loop
	}{
		{
			name: "basic continuation frame",
			frame: &http2.ContinuationFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameContinuation,
					Flags:    0,
					StreamID: 123,
					// Length will be set by PayloadLen
				},
				HeaderBlockFragment: []byte("some header data"),
			},
		},
		{
			name: "continuation frame with END_HEADERS flag",
			frame: &http2.ContinuationFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameContinuation,
					Flags:    http2.FlagContinuationEndHeaders,
					StreamID: 456,
				},
				HeaderBlockFragment: []byte("more header data"),
			},
		},
		{
			name: "continuation frame with empty header block fragment",
			frame: &http2.ContinuationFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameContinuation,
					Flags:    0,
					StreamID: 789,
				},
				HeaderBlockFragment: []byte{},
			},
		},
		{
			name: "continuation frame with END_HEADERS and empty fragment",
			frame: &http2.ContinuationFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameContinuation,
					Flags:    http2.FlagContinuationEndHeaders,
					StreamID: 1,
				},
				HeaderBlockFragment: []byte{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set length based on payload, WriteFrame will use this
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			testFrameType(t, tt.frame, "ContinuationFrame")
		})
	}
}

func TestContinuationFrame_ParsePayload_Errors(t *testing.T) {
	t.Run("payload too short error during read", func(t *testing.T) {
		header := http2.FrameHeader{
			Type:     http2.FrameContinuation,
			Length:   10, // Expect 10 bytes
			StreamID: 1,
		}
		// Provide only 5 bytes, ReadFull should cause ErrUnexpectedEOF
		payload := bytes.NewBuffer(make([]byte, 5))
		frame := &http2.ContinuationFrame{}

		err := frame.ParsePayload(payload, header)
		if err == nil {
			t.Fatal("ParsePayload expected an error for short payload, got nil")
		}
		// The error from ReadFull inside ParsePayload will be io.ErrUnexpectedEOF
		if !isSpecificError(err, io.ErrUnexpectedEOF) && err.Error() != "reading CONTINUATION header block fragment: unexpected EOF" {
			// The error message check is because fmt.Errorf wraps it
			t.Errorf("ParsePayload error mismatch: expected %v or wrapped version, got %v", io.ErrUnexpectedEOF, err)
		}
	})
}

func TestContinuationFrame_WritePayload_Error(t *testing.T) {
	frame := &http2.ContinuationFrame{
		FrameHeader:         http2.FrameHeader{Type: http2.FrameContinuation, StreamID: 1, Length: 5},
		HeaderBlockFragment: []byte("hello"),
	}
	expectedErr := fmt.Errorf("custom writer error for continuation")

	t.Run("fail immediately", func(t *testing.T) {
		fw := &failingWriter{failAfterNBytes: 0, errToReturn: expectedErr}
		n, err := frame.WritePayload(fw)
		if err == nil {
			t.Fatal("WritePayload expected an error, got nil")
		}
		if !isSpecificError(err, expectedErr) {
			t.Errorf("WritePayload error mismatch: expected %v, got %v", expectedErr, err)
		}
		if n != 0 {
			t.Errorf("WritePayload expected 0 bytes written on immediate error, got %d", n)
		}
	})

	t.Run("fail after partial write", func(t *testing.T) {
		fw := &failingWriter{failAfterNBytes: 2, errToReturn: expectedErr}
		n, err := frame.WritePayload(fw)
		if err == nil {
			t.Fatal("WritePayload with partial write expected an error, got nil")
		}
		if !isSpecificError(err, expectedErr) {
			t.Errorf("WritePayload with partial write error mismatch: expected %v, got %v", expectedErr, err)
		}
		// The failingWriter will return what it could write before erroring
		if n != 2 {
			t.Errorf("WritePayload with partial write expected 2 bytes written, got %d", n)
		}
	})
}

func TestDataFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *http2.DataFrame
	}{
		{
			name: "basic data frame",
			frame: &http2.DataFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameData,
					Flags:    0,
					StreamID: 1,
				},
				Data: []byte("hello world"),
			},
		},
		{
			name: "data frame with END_STREAM",
			frame: &http2.DataFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameData,
					Flags:    http2.FlagDataEndStream,
					StreamID: 2,
				},
				Data: []byte("last data"),
			},
		},
		{
			name: "data frame with PADDED flag and padding",
			frame: &http2.DataFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameData,
					Flags:    http2.FlagDataPadded,
					StreamID: 3,
				},
				PadLength: 5,
				Data:      []byte("padded data"),
				Padding:   make([]byte, 5), // Will be filled with zeros by WriteFrame if not already
			},
		},
		{
			name: "data frame with PADDED and END_STREAM flags",
			frame: &http2.DataFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameData,
					Flags:    http2.FlagDataPadded | http2.FlagDataEndStream,
					StreamID: 4,
				},
				PadLength: 3,
				Data:      []byte("final padded data"),
				Padding:   make([]byte, 3),
			},
		},
		{
			name: "data frame with empty data, no padding",
			frame: &http2.DataFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameData,
					Flags:    0,
					StreamID: 5,
				},
				Data: []byte{},
			},
		},
		{
			name: "data frame with empty data, with padding",
			frame: &http2.DataFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameData,
					Flags:    http2.FlagDataPadded,
					StreamID: 6,
				},
				PadLength: 4,
				Data:      []byte{},
				Padding:   make([]byte, 4),
			},
		},
		{
			name: "data frame with PADDED flag and PadLength 0",
			frame: &http2.DataFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameData,
					Flags:    http2.FlagDataPadded,
					StreamID: 7,
				},
				PadLength: 0,
				Data:      []byte("data with zero padlength field"),
				Padding:   []byte{}, // Empty padding
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Auto-set length for the testFrameType helper
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			testFrameType(t, tt.frame, "DataFrame")
		})
	}
}

func TestDataFrame_ParsePayload_Errors(t *testing.T) {
	baseHeader := http2.FrameHeader{Type: http2.FrameData, StreamID: 1}

	tests := []struct {
		name        string
		header      http2.FrameHeader
		payload     []byte
		expectedErr string // Substring of the expected error
	}{
		{
			name: "PADDED flag set, PadLength octet missing",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagDataPadded
				h.Length = 0 // PadLength itself is 1 byte, so 0 means it's missing
				return h
			}(),
			payload:     []byte{}, // No data to provide the PadLength octet
			expectedErr: "reading pad length: EOF",
		},
		{
			name: "PadLength too large for payload (PadLength only)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagDataPadded
				h.Length = 1 // Length for PadLength field itself
				return h
			}(),
			payload:     []byte{5}, // PadLength 5, but only 1 byte total in payload means data/padding missing
			expectedErr: "pad length 5 exceeds payload length 0",
		},
		{
			name: "PadLength too large for payload (PadLength + some data)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagDataPadded
				h.Length = 1 + 2 // PadLength + 2 bytes of (supposed) data
				return h
			}(),
			payload:     []byte{10, 'd', 'a'}, // PadLength 10, dataLen becomes (1+2)-10 = -7 (invalid)
			expectedErr: "pad length 10 exceeds payload length 2",
		},
		{
			name: "error reading data (PADDED)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagDataPadded
				h.Length = 1 + 5 + 2 // PadLength(1) + Data(5) + Padding(2)
				return h
			}(),
			payload:     []byte{2, 'd', 'a', 't'}, // PadLength=2, Data should be 5, but only 3 'dat' provided
			expectedErr: "reading data: unexpected EOF",
		},
		{
			name: "error reading data (not PADDED)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = 0
				h.Length = 5 // Data(5)
				return h
			}(),
			payload:     []byte{'d', 'a', 't'}, // Data should be 5, but only 3 'dat' provided
			expectedErr: "reading data: unexpected EOF",
		},
		{
			name: "error reading padding",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagDataPadded
				h.Length = 1 + 2 + 5 // PadLength(1) + Data(2) + Padding(5)
				return h
			}(),
			payload:     []byte{5, 'd', 'a', 'p', 'a', 'd'}, // PadLength=5, Data='da', Padding should be 5, but only 3 'pad' provided
			expectedErr: "reading padding: unexpected EOF",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewBuffer(tt.payload)
			frame := &http2.DataFrame{}
			err := frame.ParsePayload(r, tt.header)

			if err == nil {
				t.Fatalf("ParsePayload expected an error containing '%s', got nil", tt.expectedErr)
			}
			if !matchErr(err, tt.expectedErr) {
				// Allow direct match or substring match because of potential fmt.Errorf wrapping
				t.Errorf("ParsePayload error mismatch:\nExpected to contain: %s\nGot: %v", tt.expectedErr, err)
			}
		})
	}
}

// matchErr is a helper for error string matching, useful when errors are wrapped.
func matchErr(err error, substr string) bool {
	if err == nil {
		return false
	}
	return bytes.Contains([]byte(err.Error()), []byte(substr))
}

func TestDataFrame_WritePayload_Error(t *testing.T) {
	expectedErr := fmt.Errorf("custom writer error for data")

	tests := []struct {
		name          string
		frame         *http2.DataFrame
		failAfter     int // Bytes after which writer fails
		expectedN     int64
		expectedPanic bool // If construction itself is problematic, not write error
	}{
		{
			name: "fail writing PadLength",
			frame: &http2.DataFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagDataPadded, Length: 1 + 5 + 2},
				PadLength:   2, Data: []byte("hello"), Padding: make([]byte, 2),
			},
			failAfter: 0, expectedN: 0,
		},
		{
			name: "fail writing Data (PADDED)",
			frame: &http2.DataFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagDataPadded, Length: 1 + 5 + 2},
				PadLength:   2, Data: []byte("hello"), Padding: make([]byte, 2),
			},
			failAfter: 1, expectedN: 1, // Wrote PadLength
		},
		{
			name: "fail writing Data (not PADDED)",
			frame: &http2.DataFrame{
				FrameHeader: http2.FrameHeader{Flags: 0, Length: 5},
				Data:        []byte("hello"),
			},
			failAfter: 2, expectedN: 2,
		},
		{
			name: "fail writing Padding",
			frame: &http2.DataFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagDataPadded, Length: 1 + 5 + 2},
				PadLength:   2, Data: []byte("hello"), Padding: make([]byte, 2),
			},
			failAfter: 1 + 3, expectedN: 1 + 3, // Wrote PadLength + 3 bytes of Data
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fw := &failingWriter{failAfterNBytes: tt.failAfter, errToReturn: expectedErr}

			defer func() {
				if r := recover(); r != nil {
					if !tt.expectedPanic {
						t.Errorf("WritePayload panicked unexpectedly: %v", r)
					}
				} else if tt.expectedPanic {
					t.Error("WritePayload expected a panic but did not get one")
				}
			}()
			if tt.expectedPanic {
				// If we expect a panic, the call to WritePayload might not happen
				// or we might test a construction that leads to it.
				// For these tests, we assume WritePayload is called.
			}

			n, err := tt.frame.WritePayload(fw)
			if err == nil {
				t.Fatal("WritePayload expected an error, got nil")
			}
			if !isSpecificError(err, expectedErr) {
				t.Errorf("WritePayload error mismatch: expected %v, got %v", expectedErr, err)
			}
			if n != tt.expectedN {
				t.Errorf("WritePayload expected %d bytes written, got %d", tt.expectedN, n)
			}
		})
	}
}

func TestHeadersFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *http2.HeadersFrame
	}{
		{
			name: "basic headers frame, no flags, END_HEADERS implicitly true for testFrameType",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameHeaders,
					Flags:    http2.FlagHeadersEndHeaders, // Required for a single HEADERS to be valid
					StreamID: 1,
				},
				HeaderBlockFragment: []byte("header data"),
			},
		},
		{
			name: "headers frame with END_STREAM and END_HEADERS",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameHeaders,
					Flags:    http2.FlagHeadersEndStream | http2.FlagHeadersEndHeaders,
					StreamID: 3,
				},
				HeaderBlockFragment: []byte("final headers"),
			},
		},
		{
			name: "headers frame with PADDED and END_HEADERS",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameHeaders,
					Flags:    http2.FlagHeadersPadded | http2.FlagHeadersEndHeaders,
					StreamID: 7,
				},
				PadLength:           4,
				HeaderBlockFragment: []byte("padded headers"),
				Padding:             make([]byte, 4),
			},
		},
		{
			name: "headers frame with PRIORITY and END_HEADERS",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameHeaders,
					Flags:    http2.FlagHeadersPriority | http2.FlagHeadersEndHeaders,
					StreamID: 9,
				},
				Exclusive:           true,
				StreamDependency:    123,
				Weight:              200,
				HeaderBlockFragment: []byte("priority headers"),
			},
		},
		{
			name: "headers frame with PADDED, PRIORITY, and END_HEADERS",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameHeaders,
					Flags:    http2.FlagHeadersPadded | http2.FlagHeadersPriority | http2.FlagHeadersEndHeaders,
					StreamID: 11,
				},
				PadLength:           3,
				Exclusive:           false,
				StreamDependency:    456,
				Weight:              100,
				HeaderBlockFragment: []byte("padded priority headers"),
				Padding:             make([]byte, 3),
			},
		},
		{
			name: "headers frame with all common flags (END_STREAM, END_HEADERS, PADDED, PRIORITY)",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{
					Type: http2.FrameHeaders,
					Flags: http2.FlagHeadersEndStream | http2.FlagHeadersEndHeaders |
						http2.FlagHeadersPadded | http2.FlagHeadersPriority,
					StreamID: 13,
				},
				PadLength:           5,
				Exclusive:           true,
				StreamDependency:    789,
				Weight:              50,
				HeaderBlockFragment: []byte("all flags headers"),
				Padding:             make([]byte, 5),
			},
		},
		{
			name: "headers frame with empty fragment, END_HEADERS",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameHeaders,
					Flags:    http2.FlagHeadersEndHeaders,
					StreamID: 15,
				},
				HeaderBlockFragment: []byte{},
			},
		},
		{
			name: "headers frame with empty fragment, with padding, priority, END_HEADERS",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameHeaders,
					Flags:    http2.FlagHeadersPadded | http2.FlagHeadersPriority | http2.FlagHeadersEndHeaders,
					StreamID: 17,
				},
				PadLength:           2,
				Exclusive:           false,
				StreamDependency:    10,
				Weight:              1, // Weight in frame is actual weight, not weight-1
				HeaderBlockFragment: []byte{},
				Padding:             make([]byte, 2),
			},
		},
		{
			name: "headers frame with PADDED flag, PadLength 0, and END_HEADERS",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameHeaders,
					Flags:    http2.FlagHeadersPadded | http2.FlagHeadersEndHeaders,
					StreamID: 19,
				},
				PadLength:           0,
				HeaderBlockFragment: []byte("zero padlength field"),
				Padding:             []byte{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			testFrameType(t, tt.frame, "HeadersFrame")
		})
	}
}

func TestHeadersFrame_ParsePayload_Errors(t *testing.T) {
	baseHeader := http2.FrameHeader{Type: http2.FrameHeaders, StreamID: 1}
	// Valid non-zero priority stream dependency, E=0, Weight=15 (for 16 effective)
	validPriorityBytes := []byte{0x00, 0x00, 0x00, 0x01, 15}

	tests := []struct {
		name        string
		header      http2.FrameHeader
		payload     []byte
		expectedErr string
	}{
		{
			name: "PADDED flag, PadLength octet missing",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagHeadersPadded | http2.FlagHeadersEndHeaders
				h.Length = 0 // Not enough for PadLength byte
				return h
			}(),
			payload:     []byte{},
			expectedErr: "reading pad length: EOF",
		},
		{
			name: "PADDED flag, PadLength exceeds remaining payload",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagHeadersPadded | http2.FlagHeadersEndHeaders
				h.Length = 1 // Only PadLength byte itself
				return h
			}(),
			payload:     []byte{10}, // PadLength 10, but no more bytes for fragment or padding
			expectedErr: "pad length 10 exceeds remaining payload length 0",
		},
		{
			name: "PRIORITY flag, priority fields missing",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagHeadersPriority | http2.FlagHeadersEndHeaders
				h.Length = 4 // Not enough for 5 priority bytes
				return h
			}(),
			payload:     make([]byte, 4),
			expectedErr: "payload too short for priority fields: 4",
		},
		{
			name: "PRIORITY flag, error reading priority fields",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagHeadersPriority | http2.FlagHeadersEndHeaders
				h.Length = 5
				return h
			}(),
			payload:     make([]byte, 3), // Provide only 3 of 5 priority bytes
			expectedErr: "reading priority fields: unexpected EOF",
		},
		{
			name: "PADDED and PRIORITY, PadLength too large",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagHeadersPadded | http2.FlagHeadersPriority | http2.FlagHeadersEndHeaders
				h.Length = 1 + 5 // PadLength + PriorityFields
				return h
			}(),
			payload:     append([]byte{20}, validPriorityBytes...), // PadLength 20, Pri=5 bytes, no room for fragment/padding
			expectedErr: "pad length 20 exceeds remaining payload length 5",
		},
		{
			name: "error reading header block fragment (no padding/priority)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagHeadersEndHeaders
				h.Length = 10 // Expect 10 bytes of fragment
				return h
			}(),
			payload:     make([]byte, 5), // Provide only 5
			expectedErr: "reading header block fragment: unexpected EOF",
		},
		{
			name: "error reading header block fragment (with PADDED)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagHeadersPadded | http2.FlagHeadersEndHeaders
				h.Length = 1 + 10 + 2 // PadL(1) + Frag(10) + Padding(2)
				return h
			}(),
			payload:     append([]byte{2}, make([]byte, 5)...), // PadLength=2, Fragment should be 10, provide 5
			expectedErr: "reading header block fragment: unexpected EOF",
		},
		{
			name: "error reading header block fragment (with PRIORITY)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagHeadersPriority | http2.FlagHeadersEndHeaders
				h.Length = 5 + 10 // Priority(5) + Frag(10)
				return h
			}(),
			payload:     append(validPriorityBytes, make([]byte, 5)...), // Priority=5, Fragment should be 10, provide 5
			expectedErr: "reading header block fragment: unexpected EOF",
		},
		{
			name: "error reading padding",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagHeadersPadded | http2.FlagHeadersEndHeaders
				h.Length = 1 + 5 + 10 // PadL(1) + Frag(5) + Padding(10)
				return h
			}(),
			payload:     append(append([]byte{10}, make([]byte, 5)...), make([]byte, 3)...), // PadL=10, Frag=5, Padding should be 10, provide 3
			expectedErr: "reading padding: unexpected EOF",
		},
		{
			name: "mismatch in parsed length (Length > accounted for)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagHeadersPadded | http2.FlagHeadersEndHeaders
				// PadL byte (1) + Fragment (5) + Padding (actual 2) = 8 bytes
				// But claim total length is 10.
				h.Length = 10
				return h
			}(),
			// PadLength=2, Data = 5 bytes. So currentPos = 1(PadL) + 5(Frag) + 2(Pad) = 8
			payload:     []byte{2, 'h', 'e', 'l', 'l', 'o', 0, 0}, // PadLength 2, Frag "hello", Padding 0,0. Total 8 bytes.
			expectedErr: "reading padding: EOF",                   // Changed from "mismatch..." as EOF occurs first
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewBuffer(tt.payload)
			frame := &http2.HeadersFrame{}
			err := frame.ParsePayload(r, tt.header)

			if err == nil {
				t.Fatalf("ParsePayload expected an error containing '%s', got nil", tt.expectedErr)
			}
			if !matchErr(err, tt.expectedErr) {
				t.Errorf("ParsePayload error mismatch:\nExpected to contain: %s\nGot: %v", tt.expectedErr, err)
			}
		})
	}
}

func TestHeadersFrame_WritePayload_Error(t *testing.T) {
	expectedErr := fmt.Errorf("custom writer error for headers")

	tests := []struct {
		name      string
		frame     *http2.HeadersFrame
		failAfter int // Bytes after which writer fails
		expectedN int64
	}{
		{
			name: "fail writing PadLength",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagHeadersPadded | http2.FlagHeadersEndHeaders},
				PadLength:   2, HeaderBlockFragment: []byte("test"), Padding: make([]byte, 2),
			},
			failAfter: 0, expectedN: 0,
		},
		{
			name: "fail writing Priority",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagHeadersPriority | http2.FlagHeadersEndHeaders},
				Exclusive:   true, StreamDependency: 1, Weight: 16, HeaderBlockFragment: []byte("test"),
			},
			failAfter: 0, expectedN: 0, // Priority is written first if PADDED is not set
		},
		{
			name: "fail writing Priority (after PadLength)",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagHeadersPadded | http2.FlagHeadersPriority | http2.FlagHeadersEndHeaders},
				PadLength:   1, Exclusive: true, StreamDependency: 1, Weight: 16, HeaderBlockFragment: []byte("test"), Padding: make([]byte, 1),
			},
			failAfter: 1, expectedN: 1, // Wrote PadLength
		},
		{
			name: "fail writing HeaderBlockFragment (no Pad/Prio)",
			frame: &http2.HeadersFrame{
				FrameHeader:         http2.FrameHeader{Flags: http2.FlagHeadersEndHeaders},
				HeaderBlockFragment: []byte("fragment data"),
			},
			failAfter: 2, expectedN: 2,
		},
		{
			name: "fail writing HeaderBlockFragment (after PadLength)",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagHeadersPadded | http2.FlagHeadersEndHeaders},
				PadLength:   1, HeaderBlockFragment: []byte("fragment data"), Padding: make([]byte, 1),
			},
			failAfter: 1 + 2, expectedN: 1 + 2, // Wrote PadLength + 2 bytes of fragment
		},
		{
			name: "fail writing HeaderBlockFragment (after Priority)",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagHeadersPriority | http2.FlagHeadersEndHeaders},
				Exclusive:   true, StreamDependency: 1, Weight: 16, HeaderBlockFragment: []byte("fragment data"),
			},
			failAfter: 5 + 2, expectedN: 5 + 2, // Wrote Priority + 2 bytes of fragment
		},
		{
			name: "fail writing Padding",
			frame: &http2.HeadersFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagHeadersPadded | http2.FlagHeadersPriority | http2.FlagHeadersEndHeaders},
				PadLength:   3, Exclusive: true, StreamDependency: 1, Weight: 16,
				HeaderBlockFragment: []byte("frag"), Padding: make([]byte, 3),
			},
			// PadL(1 byte for PadLength field) + Prio(5) + Frag(4) = 10. Fail after this.
			failAfter: 1 + 5 + 4, expectedN: 1 + 5 + 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure header length is consistent for WritePayload logic
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			fw := &failingWriter{failAfterNBytes: tt.failAfter, errToReturn: expectedErr}
			n, err := tt.frame.WritePayload(fw)

			if err == nil {
				t.Fatalf("WritePayload expected an error, got nil")
			}
			if !isSpecificError(err, expectedErr) {
				t.Errorf("WritePayload error mismatch: expected %v, got %v", expectedErr, err)
			}
			if n != tt.expectedN {
				t.Errorf("WritePayload expected %d bytes written, got %d", tt.expectedN, n)
			}
		})
	}
}

func TestPriorityFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *http2.PriorityFrame
	}{
		{
			name: "basic priority frame",
			frame: &http2.PriorityFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePriority,
					Flags:    0,
					StreamID: 1, // PRIORITY frames MUST be associated with a stream.
				},
				Exclusive:        false,
				StreamDependency: 123,
				Weight:           15, // Effective weight 16
			},
		},
		{
			name: "priority frame with E flag set",
			frame: &http2.PriorityFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePriority,
					Flags:    0,
					StreamID: 2,
				},
				Exclusive:        true,
				StreamDependency: 456,
				Weight:           255, // Effective weight 256
			},
		},
		{
			name: "priority frame with stream dependency 0",
			frame: &http2.PriorityFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePriority,
					Flags:    0,
					StreamID: 3,
				},
				Exclusive:        false,
				StreamDependency: 0, // Depends on the root
				Weight:           0, // Effective weight 1
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			testFrameType(t, tt.frame, "PriorityFrame")
		})
	}
}

func TestPriorityFrame_ParsePayload_Errors(t *testing.T) {
	baseHeader := http2.FrameHeader{Type: http2.FramePriority, StreamID: 1}

	tests := []struct {
		name        string
		header      http2.FrameHeader
		payload     []byte
		expectedErr string
	}{
		{
			name: "payload too short",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 4 // PRIORITY payload must be 5 bytes
				return h
			}(),
			payload:     make([]byte, 4),
			expectedErr: "PRIORITY frame payload must be 5 bytes, got 4",
		},
		{
			name: "payload too long",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 6 // PRIORITY payload must be 5 bytes
				return h
			}(),
			payload:     make([]byte, 6),
			expectedErr: "PRIORITY frame payload must be 5 bytes, got 6",
		},
		{
			name: "error reading payload (EOF)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 5
				return h
			}(),
			payload:     make([]byte, 3), // Provide only 3 of 5 bytes
			expectedErr: "reading PRIORITY payload: unexpected EOF",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewBuffer(tt.payload)
			frame := &http2.PriorityFrame{}
			err := frame.ParsePayload(r, tt.header)

			if err == nil {
				t.Fatalf("ParsePayload expected an error containing '%s', got nil", tt.expectedErr)
			}
			if !matchErr(err, tt.expectedErr) {
				t.Errorf("ParsePayload error mismatch:\nExpected to contain: %s\nGot: %v", tt.expectedErr, err)
			}
		})
	}
}

func TestPriorityFrame_WritePayload_Error(t *testing.T) {
	frame := &http2.PriorityFrame{
		FrameHeader:      http2.FrameHeader{Type: http2.FramePriority, StreamID: 1, Length: 5},
		Exclusive:        false,
		StreamDependency: 10,
		Weight:           100,
	}
	expectedErr := fmt.Errorf("custom writer error for priority")

	tests := []struct {
		name      string
		failAfter int // Bytes after which writer fails
		expectedN int64
	}{
		{name: "fail immediately", failAfter: 0, expectedN: 0},
		{name: "fail after partial write", failAfter: 2, expectedN: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fw := &failingWriter{failAfterNBytes: tt.failAfter, errToReturn: expectedErr}
			n, err := frame.WritePayload(fw)

			if err == nil {
				t.Fatal("WritePayload expected an error, got nil")
			}
			if !isSpecificError(err, expectedErr) {
				t.Errorf("WritePayload error mismatch: expected %v, got %v", expectedErr, err)
			}
			if n != tt.expectedN {
				t.Errorf("WritePayload expected %d bytes written, got %d", tt.expectedN, n)
			}
		})
	}
}

func TestRSTStreamFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *http2.RSTStreamFrame
	}{
		{
			name: "basic RST_STREAM frame",
			frame: &http2.RSTStreamFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameRSTStream,
					Flags:    0,
					StreamID: 123, // RST_STREAM must be on a valid stream
				},
				ErrorCode: http2.ErrCodeCancel,
			},
		},
		{
			name: "RST_STREAM frame with different error code",
			frame: &http2.RSTStreamFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameRSTStream,
					Flags:    0,
					StreamID: 456,
				},
				ErrorCode: http2.ErrCodeProtocolError,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			testFrameType(t, tt.frame, "RSTStreamFrame")
		})
	}
}

func TestRSTStreamFrame_ParsePayload_Errors(t *testing.T) {
	baseHeader := http2.FrameHeader{Type: http2.FrameRSTStream, StreamID: 1}

	tests := []struct {
		name        string
		header      http2.FrameHeader
		payload     []byte
		expectedErr string
	}{
		{
			name: "payload too short",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 3 // RST_STREAM payload must be 4 bytes
				return h
			}(),
			payload:     make([]byte, 3),
			expectedErr: "RST_STREAM frame payload must be 4 bytes, got 3",
		},
		{
			name: "payload too long",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 5 // RST_STREAM payload must be 4 bytes
				return h
			}(),
			payload:     make([]byte, 5),
			expectedErr: "RST_STREAM frame payload must be 4 bytes, got 5",
		},
		{
			name: "error reading payload (EOF)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 4
				return h
			}(),
			payload:     make([]byte, 2), // Provide only 2 of 4 bytes
			expectedErr: "reading RST_STREAM error code: unexpected EOF",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewBuffer(tt.payload)
			frame := &http2.RSTStreamFrame{}
			err := frame.ParsePayload(r, tt.header)

			if err == nil {
				t.Fatalf("ParsePayload expected an error containing '%s', got nil", tt.expectedErr)
			}
			if !matchErr(err, tt.expectedErr) {
				t.Errorf("ParsePayload error mismatch:\nExpected to contain: %s\nGot: %v", tt.expectedErr, err)
			}
		})
	}
}

func TestRSTStreamFrame_WritePayload_Error(t *testing.T) {
	frame := &http2.RSTStreamFrame{
		FrameHeader: http2.FrameHeader{Type: http2.FrameRSTStream, StreamID: 1, Length: 4},
		ErrorCode:   http2.ErrCodeStreamClosed,
	}
	expectedErr := fmt.Errorf("custom writer error for rst_stream")

	tests := []struct {
		name      string
		failAfter int // Bytes after which writer fails
		expectedN int64
	}{
		{name: "fail immediately", failAfter: 0, expectedN: 0},
		{name: "fail after partial write", failAfter: 1, expectedN: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fw := &failingWriter{failAfterNBytes: tt.failAfter, errToReturn: expectedErr}
			n, err := frame.WritePayload(fw)

			if err == nil {
				t.Fatal("WritePayload expected an error, got nil")
			}
			if !isSpecificError(err, expectedErr) {
				t.Errorf("WritePayload error mismatch: expected %v, got %v", expectedErr, err)
			}
			if n != tt.expectedN {
				t.Errorf("WritePayload expected %d bytes written, got %d", tt.expectedN, n)
			}
		})
	}
}

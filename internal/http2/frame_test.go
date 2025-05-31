package http2_test

import (
	"bytes"
	"errors"
	"strings"
	// "encoding/binary" // Removed as not used
	"fmt"
	"io" // Needed for io.EOF, io.ErrUnexpectedEOF
	"reflect"
	"testing"

	"example.com/llmahttap/v2/internal/http2"
)

var validFramesTestCases = []struct {
	name          string
	originalFrame http2.Frame
}{
	{
		name: "DataFrame basic",
		originalFrame: &http2.DataFrame{
			FrameHeader: http2.FrameHeader{Type: http2.FrameData, StreamID: 1},
			Data:        []byte("hello data"),
		},
	},
	{
		name: "DataFrame with padding and END_STREAM",
		originalFrame: &http2.DataFrame{
			FrameHeader: http2.FrameHeader{Type: http2.FrameData, Flags: http2.FlagDataPadded | http2.FlagDataEndStream, StreamID: 3},
			PadLength:   4,
			Data:        []byte("padded end data"),
			Padding:     make([]byte, 4),
		},
	},
	{
		name: "HeadersFrame basic with END_HEADERS",
		originalFrame: &http2.HeadersFrame{
			FrameHeader:         http2.FrameHeader{Type: http2.FrameHeaders, Flags: http2.FlagHeadersEndHeaders, StreamID: 5},
			HeaderBlockFragment: []byte("header data"),
		},
	},
	{
		name: "HeadersFrame with all flags",
		originalFrame: &http2.HeadersFrame{
			FrameHeader: http2.FrameHeader{
				Type: http2.FrameHeaders,
				Flags: http2.FlagHeadersEndStream | http2.FlagHeadersEndHeaders |
					http2.FlagHeadersPadded | http2.FlagHeadersPriority,
				StreamID: 7,
			},
			PadLength:           3,
			Exclusive:           true,
			StreamDependency:    1,
			Weight:              100,
			HeaderBlockFragment: []byte("full headers"),
			Padding:             make([]byte, 3),
		},
	},
	{
		name: "PriorityFrame basic",
		originalFrame: &http2.PriorityFrame{
			FrameHeader:      http2.FrameHeader{Type: http2.FramePriority, StreamID: 9},
			Exclusive:        false,
			StreamDependency: 7,
			Weight:           200,
		},
	},
	{
		name: "RSTStreamFrame basic",
		originalFrame: &http2.RSTStreamFrame{
			FrameHeader: http2.FrameHeader{Type: http2.FrameRSTStream, StreamID: 11},
			ErrorCode:   http2.ErrCodeCancel,
		},
	},
	{
		name: "SettingsFrame with settings",
		originalFrame: &http2.SettingsFrame{
			FrameHeader: http2.FrameHeader{Type: http2.FrameSettings, StreamID: 0},
			Settings: []http2.Setting{
				{ID: http2.SettingMaxFrameSize, Value: 16384},
				{ID: http2.SettingEnablePush, Value: 0},
			},
		},
	},
	{
		name: "SettingsFrame ACK",
		originalFrame: &http2.SettingsFrame{
			FrameHeader: http2.FrameHeader{Type: http2.FrameSettings, Flags: http2.FlagSettingsAck, StreamID: 0},
			Settings:    nil, // Or []http2.Setting{}
		},
	},
	{
		name: "PushPromiseFrame basic with END_HEADERS",
		originalFrame: &http2.PushPromiseFrame{
			FrameHeader:         http2.FrameHeader{Type: http2.FramePushPromise, Flags: http2.FlagPushPromiseEndHeaders, StreamID: 13},
			PromisedStreamID:    14,
			HeaderBlockFragment: []byte("promised stuff"),
		},
	},
	{
		name: "PushPromiseFrame with padding and END_HEADERS",
		originalFrame: &http2.PushPromiseFrame{
			FrameHeader: http2.FrameHeader{
				Type:     http2.FramePushPromise,
				Flags:    http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders,
				StreamID: 15,
			},
			PadLength:           2,
			PromisedStreamID:    16,
			HeaderBlockFragment: []byte("padded promise"),
			Padding:             make([]byte, 2),
		},
	},
	{
		name: "PingFrame basic",
		originalFrame: &http2.PingFrame{
			FrameHeader: http2.FrameHeader{Type: http2.FramePing, StreamID: 0},
			OpaqueData:  [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		},
	},
	{
		name: "PingFrame ACK",
		originalFrame: &http2.PingFrame{
			FrameHeader: http2.FrameHeader{Type: http2.FramePing, Flags: http2.FlagPingAck, StreamID: 0},
			OpaqueData:  [8]byte{8, 7, 6, 5, 4, 3, 2, 1},
		},
	},
	{
		name: "GoAwayFrame with debug data",
		originalFrame: &http2.GoAwayFrame{
			FrameHeader:         http2.FrameHeader{Type: http2.FrameGoAway, StreamID: 0},
			LastStreamID:        20,
			ErrorCode:           http2.ErrCodeNoError,
			AdditionalDebugData: []byte("going away"),
		},
	},
	{
		name: "GoAwayFrame no debug data",
		originalFrame: &http2.GoAwayFrame{
			FrameHeader:         http2.FrameHeader{Type: http2.FrameGoAway, StreamID: 0},
			LastStreamID:        22,
			ErrorCode:           http2.ErrCodeInternalError,
			AdditionalDebugData: []byte{},
		},
	},
	{
		name: "WindowUpdateFrame basic",
		originalFrame: &http2.WindowUpdateFrame{
			FrameHeader:         http2.FrameHeader{Type: http2.FrameWindowUpdate, StreamID: 24},
			WindowSizeIncrement: 1024,
		},
	},
	{
		name: "ContinuationFrame basic with END_HEADERS",
		originalFrame: &http2.ContinuationFrame{
			FrameHeader:         http2.FrameHeader{Type: http2.FrameContinuation, Flags: http2.FlagContinuationEndHeaders, StreamID: 26},
			HeaderBlockFragment: []byte("continued headers"),
		},
	},
	{
		name: "UnknownFrame from perspective of WriteFrame",
		originalFrame: &http2.UnknownFrame{
			FrameHeader: http2.FrameHeader{Type: 0xEE, StreamID: 28, Flags: 0xAA},
			Payload:     []byte{1, 2, 3, 4},
		},
	},
}

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

		if ft.Settings == nil {
			cp.Settings = []http2.Setting{} // Normalize nil to empty non-nil for comparison
		} else {
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
		// Normalize nil to empty slice for comparison, matching ParsePayload behavior
		if ft.AdditionalDebugData == nil {
			cp.AdditionalDebugData = []byte{}
		} else {
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
			name: "mismatch in parsed length (Header.Length > available payload, EOF on padding read)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagHeadersPadded | http2.FlagHeadersEndHeaders
				// Frame claims length 10.
				// Payload provides PadLength byte (value 2) + 7 bytes of data = 8 bytes total.
				// Parse: PadLength byte (1st byte of payload) = 2.
				//        Fragment Length = (Frame.Length - 1 (for PadLength byte) - PadLength_value) = (10 - 1 - 2) = 7.
				//        Reads 7 bytes fragment (next 7 bytes of payload). Total 1+7=8 bytes read from payload.
				//        Tries to read Padding of 2 bytes, but payload buffer is exhausted.
				h.Length = 10
				return h
			}(),
			payload:     []byte{2, 'f', 'r', 'a', 'g', 'm', 'e', 'n'}, // 8 bytes: PadLength=2, Fragment="fragmen"
			expectedErr: "reading padding: EOF",
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
	baseHeaderNonZeroStreamID := http2.FrameHeader{Type: http2.FramePriority, StreamID: 1}
	baseHeaderZeroStreamID := http2.FrameHeader{Type: http2.FramePriority, StreamID: 0}

	tests := []struct {
		name        string
		header      http2.FrameHeader
		payload     []byte
		expectedErr string // Expects exact error string for these tests
	}{
		{
			name: "payload too short, stream ID > 0",
			header: func() http2.FrameHeader {
				h := baseHeaderNonZeroStreamID
				h.Length = 4 // PRIORITY payload must be 5 bytes
				return h
			}(),
			payload:     make([]byte, 4),
			expectedErr: "stream error on stream 1: PRIORITY frame payload must be 5 bytes, got 4 (code FRAME_SIZE_ERROR, 6)",
		},
		{
			name: "payload too long, stream ID > 0",
			header: func() http2.FrameHeader {
				h := baseHeaderNonZeroStreamID
				h.Length = 6 // PRIORITY payload must be 5 bytes
				return h
			}(),
			payload:     make([]byte, 6),
			expectedErr: "stream error on stream 1: PRIORITY frame payload must be 5 bytes, got 6 (code FRAME_SIZE_ERROR, 6)",
		},
		{
			name: "payload too short, stream ID 0",
			header: func() http2.FrameHeader {
				h := baseHeaderZeroStreamID
				h.Length = 4 // PRIORITY payload must be 5 bytes
				return h
			}(),
			payload:     make([]byte, 4),
			expectedErr: "connection error: PRIORITY frame payload must be 5 bytes, got 4 (last_stream_id 0, code FRAME_SIZE_ERROR, 6)",
		},
		{
			name: "payload too long, stream ID 0",
			header: func() http2.FrameHeader {
				h := baseHeaderZeroStreamID
				h.Length = 6 // PRIORITY payload must be 5 bytes
				return h
			}(),
			payload:     make([]byte, 6),
			expectedErr: "connection error: PRIORITY frame payload must be 5 bytes, got 6 (last_stream_id 0, code FRAME_SIZE_ERROR, 6)",
		},
		{
			name: "error reading payload (EOF), stream ID > 0",
			header: func() http2.FrameHeader {
				h := baseHeaderNonZeroStreamID
				h.Length = 5
				return h
			}(),
			payload:     make([]byte, 3),                            // Provide only 3 of 5 bytes
			expectedErr: "reading PRIORITY payload: unexpected EOF", // This is an io error, not FRAME_SIZE_ERROR
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewBuffer(tt.payload)
			frame := &http2.PriorityFrame{}
			err := frame.ParsePayload(r, tt.header)

			if err == nil {
				t.Fatalf("ParsePayload expected an error '%s', got nil", tt.expectedErr)
			}
			// For these tests, we expect an exact match for the error string
			if err.Error() != tt.expectedErr {
				t.Errorf("ParsePayload error mismatch:\nExpected: %s\nGot:      %v", tt.expectedErr, err)
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
		{
			name: "RST_STREAM frame with StreamClosed error code",
			frame: &http2.RSTStreamFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameRSTStream,
					Flags:    0,
					StreamID: 789,
				},
				ErrorCode: http2.ErrCodeStreamClosed,
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

func TestSettingsFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *http2.SettingsFrame
	}{
		{
			name: "basic settings frame with one setting",
			frame: &http2.SettingsFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameSettings,
					Flags:    0,
					StreamID: 0, // SETTINGS frames must be on stream 0
				},
				Settings: []http2.Setting{
					{ID: http2.SettingMaxConcurrentStreams, Value: 100},
				},
			},
		},
		{
			name: "settings frame with multiple settings",
			frame: &http2.SettingsFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameSettings,
					Flags:    0,
					StreamID: 0,
				},
				Settings: []http2.Setting{
					{ID: http2.SettingInitialWindowSize, Value: 65535},
					{ID: http2.SettingMaxFrameSize, Value: 16384},
					{ID: http2.SettingEnablePush, Value: 0},
				},
			},
		},
		{
			name: "settings frame with ACK flag (empty payload)",
			frame: &http2.SettingsFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameSettings,
					Flags:    http2.FlagSettingsAck,
					StreamID: 0,
				},
				Settings: []http2.Setting{}, // ACK frame must have no settings
			},
		},
		{
			name: "settings frame with no settings (not ACK)",
			frame: &http2.SettingsFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameSettings,
					Flags:    0,
					StreamID: 0,
				},
				Settings: []http2.Setting{},
			},
		},
		{
			name: "settings frame with mixed value settings",
			frame: &http2.SettingsFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameSettings,
					Flags:    0,
					StreamID: 0,
				},
				Settings: []http2.Setting{
					{ID: http2.SettingHeaderTableSize, Value: 4096},
					{ID: http2.SettingEnablePush, Value: 0},                               // Disabled
					{ID: http2.SettingMaxConcurrentStreams, Value: 250},                   // A custom high value
					{ID: http2.SettingInitialWindowSize, Value: 131072},                   // 2 * DefaultInitialWindowSize
					{ID: http2.SettingMaxFrameSize, Value: http2.DefaultMaxFrameSize * 4}, // e.g., 65536
					{ID: http2.SettingMaxHeaderListSize, Value: 32768},                    // A custom limit
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			testFrameType(t, tt.frame, "SettingsFrame")
		})
	}
}

func TestSettingsFrame_ParsePayload_Errors(t *testing.T) {
	baseHeader := http2.FrameHeader{Type: http2.FrameSettings, StreamID: 0}
	validSettingBytes := []byte{0x00, byte(http2.SettingInitialWindowSize), 0x00, 0x00, 0xFF, 0xFF} // ID=INITIAL_WINDOW_SIZE, Val=65535

	tests := []struct {
		name        string
		header      http2.FrameHeader
		payload     []byte
		expectedErr string
	}{
		{
			name: "ACK flag set but payload not empty",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagSettingsAck
				h.Length = 6 // ACK must have 0 length
				return h
			}(),
			payload:     validSettingBytes,
			expectedErr: "SETTINGS ACK frame must have a payload length of 0, got 6",
		},
		{
			name: "payload length not multiple of setting entry size",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 5 // Setting entry is 6 bytes
				return h
			}(),
			payload:     make([]byte, 5),
			expectedErr: "SETTINGS frame payload length 5 is not a multiple of 6",
		},
		{
			name: "error reading payload (EOF)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 12 // Expect 2 settings
				return h
			}(),
			payload:     append(validSettingBytes, make([]byte, 3)...), // Provide 1 full setting + 3 bytes of next
			expectedErr: "reading SETTINGS payload: unexpected EOF",
		},
		{
			name: "ACK flag, payload length is 0, valid", // Non-error case, implicitly tested by main test loop
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagSettingsAck
				h.Length = 0
				return h
			}(),
			payload:     []byte{},
			expectedErr: "", // No error
		},
		{
			name: "No ACK flag, payload length is 0, valid", // Non-error case
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = 0
				h.Length = 0
				return h
			}(),
			payload:     []byte{},
			expectedErr: "", // No error
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewBuffer(tt.payload)
			frame := &http2.SettingsFrame{}
			err := frame.ParsePayload(r, tt.header)

			if tt.expectedErr == "" { // For cases that should not error
				if err != nil {
					t.Errorf("ParsePayload expected no error, got %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("ParsePayload expected an error containing '%s', got nil", tt.expectedErr)
			}
			if !matchErr(err, tt.expectedErr) {
				t.Errorf("ParsePayload error mismatch:\nExpected to contain: %s\nGot: %v", tt.expectedErr, err)
			}
		})
	}
}

func TestSettingsFrame_WritePayload_Error(t *testing.T) {
	frameWithSettings := &http2.SettingsFrame{
		FrameHeader: http2.FrameHeader{Type: http2.FrameSettings, StreamID: 0, Length: 12},
		Settings: []http2.Setting{
			{ID: http2.SettingMaxHeaderListSize, Value: 1024},
			{ID: http2.SettingHeaderTableSize, Value: 4096},
		},
	}
	frameAck := &http2.SettingsFrame{
		FrameHeader: http2.FrameHeader{Type: http2.FrameSettings, StreamID: 0, Flags: http2.FlagSettingsAck, Length: 0},
		Settings:    nil,
	}
	expectedErr := fmt.Errorf("custom writer error for settings")

	tests := []struct {
		name      string
		frame     *http2.SettingsFrame
		failAfter int // Bytes after which writer fails
		expectedN int64
	}{
		{
			name:      "fail writing first setting",
			frame:     frameWithSettings,
			failAfter: 0, expectedN: 0,
		},
		{
			name:      "fail writing part of first setting",
			frame:     frameWithSettings,
			failAfter: 3, expectedN: 3,
		},
		{
			name:      "fail writing second setting",
			frame:     frameWithSettings,
			failAfter: 6, expectedN: 6, // Wrote first setting (6 bytes)
		},
		{
			name:      "ACK frame (no payload), should not call writer for payload",
			frame:     frameAck,
			failAfter: 0, expectedN: 0, // WritePayload returns 0 for ACK
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fw := &failingWriter{failAfterNBytes: tt.failAfter, errToReturn: expectedErr}
			n, err := tt.frame.WritePayload(fw)

			if tt.frame.Flags&http2.FlagSettingsAck != 0 { // ACK frame
				if err != nil {
					t.Errorf("WritePayload for ACK frame expected no error, got %v", err)
				}
				if n != 0 {
					t.Errorf("WritePayload for ACK frame expected 0 bytes written, got %d", n)
				}
				return
			}

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

func TestPushPromiseFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *http2.PushPromiseFrame
	}{
		{
			name: "basic push_promise frame, END_HEADERS",
			frame: &http2.PushPromiseFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePushPromise,
					Flags:    http2.FlagPushPromiseEndHeaders, // Required for a single PUSH_PROMISE to be valid
					StreamID: 1,                               // PUSH_PROMISE is sent on the stream it's associated with
				},
				PromisedStreamID:    2,
				HeaderBlockFragment: []byte("promised headers"),
			},
		},
		{
			name: "push_promise frame with PADDED and END_HEADERS",
			frame: &http2.PushPromiseFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePushPromise,
					Flags:    http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders,
					StreamID: 3,
				},
				PadLength:           4,
				PromisedStreamID:    4,
				HeaderBlockFragment: []byte("padded promised headers"),
				Padding:             make([]byte, 4),
			},
		},
		{
			name: "push_promise frame with empty fragment and END_HEADERS",
			frame: &http2.PushPromiseFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePushPromise,
					Flags:    http2.FlagPushPromiseEndHeaders,
					StreamID: 5,
				},
				PromisedStreamID:    6,
				HeaderBlockFragment: []byte{},
			},
		},
		{
			name: "push_promise frame with PADDED, empty fragment, and END_HEADERS",
			frame: &http2.PushPromiseFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePushPromise,
					Flags:    http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders,
					StreamID: 7,
				},
				PadLength:           2,
				PromisedStreamID:    8,
				HeaderBlockFragment: []byte{},
				Padding:             make([]byte, 2),
			},
		},
		{
			name: "push_promise frame with PADDED flag, PadLength 0, and END_HEADERS",
			frame: &http2.PushPromiseFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePushPromise,
					Flags:    http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders,
					StreamID: 9,
				},
				PadLength:           0,
				PromisedStreamID:    10,
				HeaderBlockFragment: []byte("zero padlength field"),
				Padding:             []byte{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			testFrameType(t, tt.frame, "PushPromiseFrame")
		})
	}
}

func TestPushPromiseFrame_ParsePayload_Errors(t *testing.T) {
	baseHeader := http2.FrameHeader{Type: http2.FramePushPromise, StreamID: 1}
	validPromisedIDBytes := []byte{0x00, 0x00, 0x00, 0x02} // PromisedStreamID = 2

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
				h.Flags = http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders
				h.Length = 0 // Not enough for PadLength byte
				return h
			}(),
			payload:     []byte{},
			expectedErr: "reading pad length: EOF",
		},
		{
			name: "PADDED flag, PadLength exceeds remaining payload (no PromisedID yet)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders
				h.Length = 1 // Only PadLength byte itself
				return h
			}(),
			payload:     []byte{10}, // PadLength 10, but no room for PromisedID, fragment, or padding
			expectedErr: "pad length 10 exceeds remaining payload length 0",
		},
		{
			name: "PromisedStreamID missing (no padding)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagPushPromiseEndHeaders
				h.Length = 3 // Not enough for 4-byte PromisedStreamID
				return h
			}(),
			payload:     make([]byte, 3),
			expectedErr: "payload too short for PromisedStreamID: 3",
		},
		{
			name: "PromisedStreamID missing (with PADDED)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders
				h.Length = 1 + 2 // PadLength(1) + 2 bytes for PromisedID (not enough)
				return h
			}(),
			payload:     []byte{0, 0x00, 0x00}, // PadLength=0, then 2 bytes for PromisedID
			expectedErr: "payload too short for PromisedStreamID: 2",
		},
		{
			name: "error reading PromisedStreamID",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagPushPromiseEndHeaders
				h.Length = 4 // Length for PromisedStreamID
				return h
			}(),
			payload:     make([]byte, 2), // Provide only 2 of 4 bytes
			expectedErr: "reading promised stream ID: unexpected EOF",
		},
		{
			name: "PADDED, PadLength too large after PromisedID",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders
				h.Length = 1 + 4 // PadLength(1) + PromisedID(4)
				return h
			}(),
			payload:     append([]byte{20}, validPromisedIDBytes...), // PadL=20, PromisedID=4. pad length 20 exceeds remaining payload length 4
			expectedErr: "pad length 20 exceeds remaining payload length 4",
		},
		{
			name: "error reading header block fragment (no padding)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagPushPromiseEndHeaders
				h.Length = 4 + 10 // PromisedID(4) + Fragment(10)
				return h
			}(),
			payload:     append(validPromisedIDBytes, make([]byte, 5)...), // PromisedID + 5 bytes of fragment
			expectedErr: "reading header block fragment: unexpected EOF",
		},
		{
			name: "error reading padding",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders
				h.Length = 1 + 4 + 5 + 10 // PadL(1) + PromisedID(4) + Frag(5) + Padding(10)
				return h
			}(),
			payload:     append(append(append([]byte{10}, validPromisedIDBytes...), make([]byte, 5)...), make([]byte, 3)...),
			expectedErr: "reading padding: unexpected EOF",
		},
		{
			name: "mismatch in parsed length (Length > accounted for)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Flags = http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders
				// PadL byte (1) + PromisedID (4) + Fragment (5) + Padding (actual 2) = 12 bytes
				// But claim total length is 15.
				h.Length = 15
				return h
			}(),
			// PadLength=2, PromisedID="idid", Frag="hello", Padding="pp"
			// Payload: PadL(1) + PromisedID(4) + Frag(5) + Padding(2) = 12 bytes actual
			// currentPos = 1(PadL byte) + 4(PromisedID) + 5(Frag) + 2(Padding) = 12
			payload:     []byte{2, 0, 0, 0, 2, 'h', 'e', 'l', 'l', 'o', 0, 0}, // PadL=2, PromisedID=2, Frag="hello", Padding=2bytes. Total actual payload=12
			expectedErr: "reading header block fragment: unexpected EOF",      // Corrected: EOF on fragment read
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewBuffer(tt.payload)
			frame := &http2.PushPromiseFrame{}
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

func TestPushPromiseFrame_WritePayload_Error(t *testing.T) {
	expectedErr := fmt.Errorf("custom writer error for push_promise")

	tests := []struct {
		name      string
		frame     *http2.PushPromiseFrame
		failAfter int // Bytes after which writer fails
		expectedN int64
	}{
		{
			name: "fail writing PadLength",
			frame: &http2.PushPromiseFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders},
				PadLength:   2, PromisedStreamID: 1, HeaderBlockFragment: []byte("test"), Padding: make([]byte, 2),
			},
			failAfter: 0, expectedN: 0,
		},
		{
			name: "fail writing PromisedStreamID (no padding)",
			frame: &http2.PushPromiseFrame{
				FrameHeader:      http2.FrameHeader{Flags: http2.FlagPushPromiseEndHeaders},
				PromisedStreamID: 1, HeaderBlockFragment: []byte("test"),
			},
			failAfter: 0, expectedN: 0,
		},
		{
			name: "fail writing PromisedStreamID (after PadLength)",
			frame: &http2.PushPromiseFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders},
				PadLength:   1, PromisedStreamID: 1, HeaderBlockFragment: []byte("test"), Padding: make([]byte, 1),
			},
			failAfter: 1, expectedN: 1, // Wrote PadLength
		},
		{
			name: "fail writing HeaderBlockFragment (no padding)",
			frame: &http2.PushPromiseFrame{
				FrameHeader:      http2.FrameHeader{Flags: http2.FlagPushPromiseEndHeaders},
				PromisedStreamID: 1, HeaderBlockFragment: []byte("fragment data"),
			},
			failAfter: 4 + 2, expectedN: 4 + 2, // Wrote PromisedID + 2 bytes of fragment
		},
		{
			name: "fail writing HeaderBlockFragment (with padding)",
			frame: &http2.PushPromiseFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders},
				PadLength:   1, PromisedStreamID: 1, HeaderBlockFragment: []byte("fragment data"), Padding: make([]byte, 1),
			},
			failAfter: 1 + 4 + 2, expectedN: 1 + 4 + 2, // Wrote PadLength + PromisedID + 2 bytes of fragment
		},
		{
			name: "fail writing Padding",
			frame: &http2.PushPromiseFrame{
				FrameHeader: http2.FrameHeader{Flags: http2.FlagPushPromisePadded | http2.FlagPushPromiseEndHeaders},
				PadLength:   3, PromisedStreamID: 1, HeaderBlockFragment: []byte("frag"), Padding: make([]byte, 3),
			},
			// PadL(1 byte field) + PromisedID(4) + Frag(4) = 9. Fail after this.
			failAfter: 1 + 4 + 4, expectedN: 1 + 4 + 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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

func TestPingFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *http2.PingFrame
	}{
		{
			name: "basic PING frame (not ACK)",
			frame: &http2.PingFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePing,
					Flags:    0,
					StreamID: 0, // PING frames must be on stream 0
				},
				OpaqueData: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
			},
		},
		{
			name: "PING frame with ACK flag",
			frame: &http2.PingFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePing,
					Flags:    http2.FlagPingAck,
					StreamID: 0,
				},
				OpaqueData: [8]byte{0xA, 0xB, 0xC, 0xD, 0xE, 0xF, 0x0, 0x1},
			},
		},
		{
			name: "PING frame with ACK flag and different opaque data",
			frame: &http2.PingFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePing,
					Flags:    http2.FlagPingAck,
					StreamID: 0,
				},
				OpaqueData: [8]byte{8, 7, 6, 5, 4, 3, 2, 1},
			},
		},
		{
			name: "PING frame with all zeros OpaqueData",
			frame: &http2.PingFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FramePing,
					Flags:    0,
					StreamID: 0,
				},
				OpaqueData: [8]byte{0, 0, 0, 0, 0, 0, 0, 0},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			testFrameType(t, tt.frame, "PingFrame")
		})
	}
}

func TestPingFrame_ParsePayload_Errors(t *testing.T) {
	baseHeader := http2.FrameHeader{Type: http2.FramePing, StreamID: 0}

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
				h.Length = 7 // PING payload must be 8 bytes
				return h
			}(),
			payload:     make([]byte, 7),
			expectedErr: "PING frame payload must be 8 bytes, got 7",
		},
		{
			name: "payload too long",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 9 // PING payload must be 8 bytes
				return h
			}(),
			payload:     make([]byte, 9),
			expectedErr: "PING frame payload must be 8 bytes, got 9",
		},
		{
			name: "error reading payload (EOF)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 8
				return h
			}(),
			payload:     make([]byte, 5), // Provide only 5 of 8 bytes
			expectedErr: "reading PING opaque data: unexpected EOF",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewBuffer(tt.payload)
			frame := &http2.PingFrame{}
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

func TestPingFrame_WritePayload_Error(t *testing.T) {
	frame := &http2.PingFrame{
		FrameHeader: http2.FrameHeader{Type: http2.FramePing, StreamID: 0, Length: 8},
		OpaqueData:  [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
	}
	expectedErr := fmt.Errorf("custom writer error for ping")

	tests := []struct {
		name      string
		failAfter int // Bytes after which writer fails
		expectedN int64
	}{
		{name: "fail immediately", failAfter: 0, expectedN: 0},
		{name: "fail after partial write", failAfter: 4, expectedN: 4},
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

func TestGoAwayFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *http2.GoAwayFrame
	}{
		{
			name: "basic GOAWAY frame",
			frame: &http2.GoAwayFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameGoAway,
					Flags:    0,
					StreamID: 0, // GOAWAY frames must be on stream 0
				},
				LastStreamID:        12345,
				ErrorCode:           http2.ErrCodeNoError,
				AdditionalDebugData: []byte("graceful shutdown"),
			},
		},
		{
			name: "GOAWAY frame with no debug data",
			frame: &http2.GoAwayFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameGoAway,
					Flags:    0,
					StreamID: 0,
				},
				LastStreamID:        67890,
				ErrorCode:           http2.ErrCodeProtocolError,
				AdditionalDebugData: []byte{}, // Empty debug data
			},
		},
		{
			name: "GOAWAY frame with specific error and debug data",
			frame: &http2.GoAwayFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameGoAway,
					Flags:    0,
					StreamID: 0,
				},
				LastStreamID:        0, // Can be 0 if no streams processed
				ErrorCode:           http2.ErrCodeInternalError,
				AdditionalDebugData: []byte("internal server issue"),
			}, // Closes frame field for "specific error and debug data"
		}, // Closes "specific error and debug data" test case struct literal, ADDED COMMA
		{ // Starts "nil debug data" test case struct literal (sibling)
			name: "GOAWAY frame with nil debug data", // Should be handled same as empty slice
			frame: &http2.GoAwayFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameGoAway,
					Flags:    0,
					StreamID: 0,
				},
				LastStreamID:        999,
				ErrorCode:           http2.ErrCodeSettingsTimeout,
				AdditionalDebugData: nil, // Test nil explicitly
			},
		}, // Closes "nil debug data" test case struct literal, ADDED COMMA
		{
			name: "GOAWAY frame with max LastStreamID",
			frame: &http2.GoAwayFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameGoAway,
					Flags:    0,
					StreamID: 0,
				},
				LastStreamID:        0x7FFFFFFF, // Max 31-bit stream ID
				ErrorCode:           http2.ErrCodeEnhanceYourCalm,
				AdditionalDebugData: []byte("calm down"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			testFrameType(t, tt.frame, "GoAwayFrame")
		})
	}
}

func TestGoAwayFrame_ParsePayload_Errors(t *testing.T) {
	baseHeader := http2.FrameHeader{Type: http2.FrameGoAway, StreamID: 0}
	validFixedPart := []byte{0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00} // LastStreamID=1, ErrorCode=NoError

	tests := []struct {
		name        string
		header      http2.FrameHeader
		payload     []byte
		expectedErr string
	}{
		{
			name: "payload too short (less than 8 bytes for fixed part)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 7 // GOAWAY must be at least 8 bytes
				return h
			}(),
			payload:     make([]byte, 7),
			expectedErr: "GOAWAY frame payload must be at least 8 bytes, got 7",
		},
		{
			name: "error reading fixed part (EOF)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 8 // Length for fixed part only
				return h
			}(),
			payload:     make([]byte, 5), // Provide only 5 of 8 bytes for fixed part
			expectedErr: "reading GOAWAY fixed part: unexpected EOF",
		},
		{
			name: "error reading additional debug data (EOF)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 8 + 10 // Fixed part (8) + Debug data (10)
				return h
			}(),
			payload:     append(validFixedPart, make([]byte, 5)...), // Fixed part + 5 bytes of debug data
			expectedErr: "reading GOAWAY additional debug data: unexpected EOF",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewBuffer(tt.payload)
			frame := &http2.GoAwayFrame{}
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

func TestGoAwayFrame_WritePayload_Error(t *testing.T) {
	frameWithDebug := &http2.GoAwayFrame{
		FrameHeader:         http2.FrameHeader{Type: http2.FrameGoAway, StreamID: 0, Length: 8 + 5},
		LastStreamID:        10,
		ErrorCode:           http2.ErrCodeConnectError,
		AdditionalDebugData: []byte("debug"),
	}
	frameNoDebug := &http2.GoAwayFrame{
		FrameHeader:         http2.FrameHeader{Type: http2.FrameGoAway, StreamID: 0, Length: 8},
		LastStreamID:        20,
		ErrorCode:           http2.ErrCodeNoError,
		AdditionalDebugData: nil, // or []byte{}
	}
	expectedErr := fmt.Errorf("custom writer error for goaway")

	tests := []struct {
		name      string
		frame     *http2.GoAwayFrame
		failAfter int // Bytes after which writer fails
		expectedN int64
	}{
		{
			name:      "fail writing fixed part (no debug)",
			frame:     frameNoDebug,
			failAfter: 0, expectedN: 0,
		},
		{
			name:      "fail writing fixed part (with debug)",
			frame:     frameWithDebug,
			failAfter: 4, expectedN: 4,
		},
		{
			name:      "fail writing debug data",
			frame:     frameWithDebug,
			failAfter: 8 + 2, expectedN: 8 + 2, // Wrote fixed part (8) + 2 bytes of debug
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fw := &failingWriter{failAfterNBytes: tt.failAfter, errToReturn: expectedErr}
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

func TestWindowUpdateFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *http2.WindowUpdateFrame
	}{
		{
			name: "basic WINDOW_UPDATE frame",
			frame: &http2.WindowUpdateFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameWindowUpdate,
					Flags:    0,
					StreamID: 1, // Can be stream 0 or a specific stream
				},
				WindowSizeIncrement: 1000,
			},
		},
		{
			name: "WINDOW_UPDATE frame with max increment",
			frame: &http2.WindowUpdateFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameWindowUpdate,
					Flags:    0,
					StreamID: 0, // Connection-level update
				},
				WindowSizeIncrement: 0x7FFFFFFF, // Max 31-bit value
			},
		},
		{
			// Note: A WindowSizeIncrement of 0 is a PROTOCOL_ERROR for stream-specific updates,
			// but allowed for connection-level updates (though usually indicative of an issue).
			// The frame parsing/serialization itself should handle it.
			name: "WINDOW_UPDATE frame with zero increment",
			frame: &http2.WindowUpdateFrame{
				FrameHeader: http2.FrameHeader{
					Type:     http2.FrameWindowUpdate,
					Flags:    0,
					StreamID: 0,
				},
				WindowSizeIncrement: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			testFrameType(t, tt.frame, "WindowUpdateFrame")
		})
	}
}

func TestWindowUpdateFrame_ParsePayload_Errors(t *testing.T) {
	baseHeader := http2.FrameHeader{Type: http2.FrameWindowUpdate, StreamID: 1}

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
				h.Length = 3 // WINDOW_UPDATE payload must be 4 bytes
				return h
			}(),
			payload:     make([]byte, 3),
			expectedErr: "WINDOW_UPDATE frame payload must be 4 bytes, got 3",
		},
		{
			name: "payload too long",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 5 // WINDOW_UPDATE payload must be 4 bytes
				return h
			}(),
			payload:     make([]byte, 5),
			expectedErr: "WINDOW_UPDATE frame payload must be 4 bytes, got 5",
		},
		{
			name: "error reading payload (EOF)",
			header: func() http2.FrameHeader {
				h := baseHeader
				h.Length = 4
				return h
			}(),
			payload:     make([]byte, 2), // Provide only 2 of 4 bytes
			expectedErr: "reading WINDOW_UPDATE increment: unexpected EOF",
		},
		// WindowSizeIncrement == 0 is not a parsing error, but a protocol error handled at a higher level.
		// TestWindowUpdateFrame already covers the zero increment case for parsing.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewBuffer(tt.payload)
			frame := &http2.WindowUpdateFrame{}
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

func TestWindowUpdateFrame_WritePayload_Error(t *testing.T) {
	frame := &http2.WindowUpdateFrame{
		FrameHeader:         http2.FrameHeader{Type: http2.FrameWindowUpdate, StreamID: 0, Length: 4},
		WindowSizeIncrement: 100,
	}
	expectedErr := fmt.Errorf("custom writer error for window_update")

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

func TestUnknownFrame(t *testing.T) {
	tests := []struct {
		name  string
		frame *http2.UnknownFrame
	}{
		{
			name: "unknown frame type with some payload",
			frame: &http2.UnknownFrame{
				FrameHeader: http2.FrameHeader{
					Type:     0xFF, // An example of an unknown type
					Flags:    0xAB,
					StreamID: 12345,
				},
				Payload: []byte{0xDE, 0xAD, 0xBE, 0xEF},
			},
		},
		{
			name: "unknown frame type with empty payload",
			frame: &http2.UnknownFrame{
				FrameHeader: http2.FrameHeader{
					Type:     0x42, // Another unknown type
					Flags:    0,
					StreamID: 0,
				},
				Payload: []byte{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.frame.FrameHeader.Length = tt.frame.PayloadLen()
			testFrameType(t, tt.frame, "UnknownFrame")
		})
	}
}

func TestUnknownFrame_ParsePayload_Errors(t *testing.T) {
	t.Run("payload too short error during read", func(t *testing.T) {
		header := http2.FrameHeader{
			Type:     0xFE, // Unknown type
			Length:   10,   // Expect 10 bytes
			StreamID: 1,
		}
		// Provide only 5 bytes, ReadFull should cause ErrUnexpectedEOF
		payload := bytes.NewBuffer(make([]byte, 5))
		frame := &http2.UnknownFrame{}

		err := frame.ParsePayload(payload, header)
		if err == nil {
			t.Fatal("ParsePayload expected an error for short payload, got nil")
		}
		if !isSpecificError(err, io.ErrUnexpectedEOF) && err.Error() != "reading UnknownFrame payload: unexpected EOF" {
			t.Errorf("ParsePayload error mismatch: expected %v or wrapped version, got %v", io.ErrUnexpectedEOF, err)
		}
	})
}

func TestUnknownFrame_WritePayload_Error(t *testing.T) {
	frame := &http2.UnknownFrame{
		FrameHeader: http2.FrameHeader{Type: 0xFD, StreamID: 1, Length: 5},
		Payload:     []byte("hello"),
	}
	expectedErr := fmt.Errorf("custom writer error for unknown")

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
		if n != 2 {
			t.Errorf("WritePayload with partial write expected 2 bytes written, got %d", n)
		}
	})
}

func TestReadFrame_UnknownFrameType(t *testing.T) {
	// Construct raw bytes for an unknown frame type
	// Header: Length=4, Type=0xFF (unknown), Flags=0, StreamID=1
	// Payload: "test"
	rawFrameBytes := []byte{
		0x00, 0x00, 0x04, // Length = 4
		0xFF,                   // Type = 255 (unknown)
		0x00,                   // Flags = 0
		0x00, 0x00, 0x00, 0x01, // StreamID = 1
		't', 'e', 's', 't', // Payload
	}
	buf := bytes.NewBuffer(rawFrameBytes)

	frame, err := http2.ReadFrame(buf)
	if err != nil {
		t.Fatalf("ReadFrame() unexpected error for unknown frame type: %v", err)
	}

	unknownFrame, ok := frame.(*http2.UnknownFrame)
	if !ok {
		t.Fatalf("ReadFrame() did not return *http2.UnknownFrame, got %T", frame)
	}

	expectedHeader := http2.FrameHeader{
		Length:   4,
		Type:     0xFF,
		Flags:    0,
		StreamID: 1,
	}
	// Can't use assertFrameHeaderEquals directly because unknownFrame.FrameHeader.raw won't be populated
	// the same way as if it was read by ReadFrameHeader then written.
	// Instead, compare the fields.
	if unknownFrame.FrameHeader.Length != expectedHeader.Length ||
		unknownFrame.FrameHeader.Type != expectedHeader.Type ||
		unknownFrame.FrameHeader.Flags != expectedHeader.Flags ||
		unknownFrame.FrameHeader.StreamID != expectedHeader.StreamID {
		t.Errorf("UnknownFrame header mismatch.\nExpected: %+v\nGot:      %+v",
			expectedHeader, unknownFrame.FrameHeader)
	}

	if !bytes.Equal(unknownFrame.Payload, []byte("test")) {
		t.Errorf("UnknownFrame payload mismatch: expected %x, got %x", []byte("test"), unknownFrame.Payload)
	}

	if buf.Len() != 0 {
		t.Errorf("Buffer not fully consumed after ReadFrame for unknown type, remaining %d bytes", buf.Len())
	}
}

func TestWriteFrame_ErrorHandling(t *testing.T) {
	// Use a simple frame type for this test, e.g., PingFrame
	originalFrame := &http2.PingFrame{
		FrameHeader: http2.FrameHeader{Type: http2.FramePing, StreamID: 0},
		OpaqueData:  [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
	}
	// Length will be set by WriteFrame based on PayloadLen()
	// originalFrame.FrameHeader.Length = originalFrame.PayloadLen() // Not needed here

	expectedErr := fmt.Errorf("simulated writer failure")

	t.Run("error writing header", func(t *testing.T) {
		fw := &failingWriter{failAfterNBytes: 0, errToReturn: expectedErr} // Fails immediately
		err := http2.WriteFrame(fw, originalFrame)
		if err == nil {
			t.Fatal("WriteFrame expected error writing header, got nil")
		}
		if !matchErr(err, "writing frame header") || !matchErr(err, expectedErr.Error()) {
			t.Errorf("WriteFrame error writing header mismatch. Expected to contain 'writing frame header' and '%s', got: %v", expectedErr.Error(), err)
		}
	})

	t.Run("error writing payload", func(t *testing.T) {
		// Fail after header (9 bytes) but before full payload
		fw := &failingWriter{failAfterNBytes: int(http2.FrameHeaderLen) + 2, errToReturn: expectedErr}
		err := http2.WriteFrame(fw, originalFrame)
		if err == nil {
			t.Fatal("WriteFrame expected error writing payload, got nil")
		}
		if !matchErr(err, "writing PING payload") || !matchErr(err, expectedErr.Error()) {
			t.Errorf("WriteFrame error writing payload mismatch. Expected to contain 'writing PING payload' and '%s', got: %v", expectedErr.Error(), err)
		}
	})

	t.Run("payload length mismatch error", func(t *testing.T) {
		// Create a mock frame that misreports its payload length
		mockFrame := &mockMisreportingFrame{
			FrameHeader:        http2.FrameHeader{Type: 0xEE, StreamID: 1},
			actualPayload:      []byte("actual"), // 6 bytes
			reportedPayloadLen: 5,                // Reports 5 bytes
		}

		var buf bytes.Buffer // Use a successful writer
		err := http2.WriteFrame(&buf, mockFrame)
		if err == nil {
			t.Fatal("WriteFrame expected error for payload length mismatch, got nil")
		}
		expectedErrMsgSubstr := "payload length mismatch: PayloadLen() declared 5, but WritePayload() wrote 6 bytes"
		if !matchErr(err, expectedErrMsgSubstr) {
			t.Errorf("WriteFrame error for payload length mismatch incorrect. Expected to contain '%s', got: %v", expectedErrMsgSubstr, err)
		}
	})
}

// mockMisreportingFrame is a helper for testing WriteFrame's internal consistency check.
type mockMisreportingFrame struct {
	http2.FrameHeader
	actualPayload      []byte
	reportedPayloadLen uint32
}

func (m *mockMisreportingFrame) Header() *http2.FrameHeader { return &m.FrameHeader }
func (m *mockMisreportingFrame) ParsePayload(r io.Reader, header http2.FrameHeader) error {
	return nil /* not used */
}
func (m *mockMisreportingFrame) WritePayload(w io.Writer) (int64, error) {
	n, err := w.Write(m.actualPayload)
	return int64(n), err
}
func (m *mockMisreportingFrame) PayloadLen() uint32 { return m.reportedPayloadLen }

func TestFrameType_String(t *testing.T) {
	tests := []struct {
		name     string
		ft       http2.FrameType
		expected string
	}{
		{name: "DATA", ft: http2.FrameData, expected: "DATA"},
		{name: "HEADERS", ft: http2.FrameHeaders, expected: "HEADERS"},
		{name: "PRIORITY", ft: http2.FramePriority, expected: "PRIORITY"},
		{name: "RST_STREAM", ft: http2.FrameRSTStream, expected: "RST_STREAM"},
		{name: "SETTINGS", ft: http2.FrameSettings, expected: "SETTINGS"},
		{name: "PUSH_PROMISE", ft: http2.FramePushPromise, expected: "PUSH_PROMISE"},
		{name: "PING", ft: http2.FramePing, expected: "PING"},
		{name: "GOAWAY", ft: http2.FrameGoAway, expected: "GOAWAY"},
		{name: "WINDOW_UPDATE", ft: http2.FrameWindowUpdate, expected: "WINDOW_UPDATE"},
		{name: "CONTINUATION", ft: http2.FrameContinuation, expected: "CONTINUATION"},
		{name: "Unknown FrameType (10)", ft: http2.FrameType(10), expected: "UNKNOWN_FRAME_TYPE_10"},
		{name: "Unknown FrameType (255)", ft: http2.FrameType(255), expected: "UNKNOWN_FRAME_TYPE_255"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ft.String(); got != tt.expected {
				t.Errorf("FrameType.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestSettingID_String(t *testing.T) {
	tests := []struct {
		name     string
		sid      http2.SettingID
		expected string
	}{
		{name: "SETTINGS_HEADER_TABLE_SIZE", sid: http2.SettingHeaderTableSize, expected: "SETTINGS_HEADER_TABLE_SIZE"},
		{name: "SETTINGS_ENABLE_PUSH", sid: http2.SettingEnablePush, expected: "SETTINGS_ENABLE_PUSH"},
		{name: "SETTINGS_MAX_CONCURRENT_STREAMS", sid: http2.SettingMaxConcurrentStreams, expected: "SETTINGS_MAX_CONCURRENT_STREAMS"},
		{name: "SETTINGS_INITIAL_WINDOW_SIZE", sid: http2.SettingInitialWindowSize, expected: "SETTINGS_INITIAL_WINDOW_SIZE"},
		{name: "SETTINGS_MAX_FRAME_SIZE", sid: http2.SettingMaxFrameSize, expected: "SETTINGS_MAX_FRAME_SIZE"},
		{name: "SETTINGS_MAX_HEADER_LIST_SIZE", sid: http2.SettingMaxHeaderListSize, expected: "SETTINGS_MAX_HEADER_LIST_SIZE"},
		{name: "Unknown SettingID (0)", sid: http2.SettingID(0), expected: "UNKNOWN_SETTING_ID_0"},
		{name: "Unknown SettingID (7)", sid: http2.SettingID(7), expected: "UNKNOWN_SETTING_ID_7"},
		{name: "Unknown SettingID (65535)", sid: http2.SettingID(65535), expected: "UNKNOWN_SETTING_ID_65535"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sid.String(); got != tt.expected {
				t.Errorf("SettingID.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestReadFrame_ValidFrames(t *testing.T) {
	for _, tt := range validFramesTestCases {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure the original frame's header length is set correctly based on its payload.
			// This is crucial because WriteFrame uses FrameHeader.Length.
			tt.originalFrame.Header().Length = tt.originalFrame.PayloadLen()

			var writeBuf bytes.Buffer
			err := http2.WriteFrame(&writeBuf, tt.originalFrame)
			if err != nil {
				t.Fatalf("WriteFrame() failed to serialize original frame: %v", err)
			}

			// Create a new buffer for reading, as ReadFrame consumes the buffer.
			readBuf := bytes.NewBuffer(writeBuf.Bytes())
			parsedFrame, err := http2.ReadFrame(readBuf)

			if err != nil {
				t.Fatalf("ReadFrame() failed: %v. Serialized bytes: %x", err, writeBuf.Bytes())
			}
			if parsedFrame == nil {
				t.Fatal("ReadFrame() returned nil frame without error")
			}
			if readBuf.Len() != 0 {
				t.Errorf("ReadFrame() did not consume entire buffer, remaining: %d bytes", readBuf.Len())
			}

			// Compare headers
			assertFrameHeaderEquals(t, *tt.originalFrame.Header(), *parsedFrame.Header())

			// Compare payload parts (struct fields other than FrameHeader)
			originalPayloadComparable := deepCopyFramePayload(tt.originalFrame)
			parsedPayloadComparable := deepCopyFramePayload(parsedFrame)

			if !reflect.DeepEqual(originalPayloadComparable, parsedPayloadComparable) {
				t.Errorf("Frame payload parts not equal after ReadFrame.\nOriginal: %#v\nParsed:   %#v",
					originalPayloadComparable, parsedPayloadComparable)
			}
		})
	}
}

func TestReadFrame_ErrorConditions(t *testing.T) {
	tests := []struct {
		name           string
		inputBytes     []byte
		expectedError  error  // For sentinel errors like io.EOF, checked with errors.Is
		expectedErrStr string // For substring matching of error messages
	}{
		// --- Errors during Header Reading (delegated to ReadFrameHeader) ---
		{
			name:          "EOF reading header - empty buffer",
			inputBytes:    []byte{},
			expectedError: io.EOF,
		},
		{
			name:          "EOF reading header - partial header (4 bytes)",
			inputBytes:    []byte{0x00, 0x00, 0x01, 0x00}, // Length, Type
			expectedError: io.ErrUnexpectedEOF,
		},
		// --- Errors during Payload Parsing (after successful header read) ---
		{
			name: "Valid header, EOF reading payload for DATA frame",
			inputBytes: []byte{
				0x00, 0x00, 0x05, // Length = 5
				byte(http2.FrameData), 0x00, // Type=DATA, Flags=0
				0x00, 0x00, 0x00, 0x01, // StreamID=1
				'd', 'a', 't', // Payload: "dat" (3 bytes, expecting 5)
			},
			expectedErrStr: "parsing DATA payload: reading data: unexpected EOF",
		},
		{
			name: "PRIORITY frame, Header.Length != 5 (is 3)",
			inputBytes: []byte{
				0x00, 0x00, 0x03, // Length = 3
				byte(http2.FramePriority), 0x00, // Type=PRIORITY, Flags=0
				0x00, 0x00, 0x00, 0x01, // StreamID=1
				0x01, 0x02, 0x03, // Dummy payload
			},
			// ReadFrame wraps the error: "parsing PRIORITY payload: <original error>"
			expectedErrStr: "parsing PRIORITY payload: stream error on stream 1: PRIORITY frame payload must be 5 bytes, got 3 (code FRAME_SIZE_ERROR, 6)",
		},
		{
			name: "RST_STREAM frame, Header.Length != 4 (is 2)",
			inputBytes: []byte{
				0x00, 0x00, 0x02, // Length = 2
				byte(http2.FrameRSTStream), 0x00, // Type=RST_STREAM, Flags=0
				0x00, 0x00, 0x00, 0x01, // StreamID=1
				0x00, 0x00, // Dummy payload
			},
			expectedErrStr: "parsing RST_STREAM payload: connection error: RST_STREAM frame payload must be 4 bytes, got 2 (last_stream_id 0, code FRAME_SIZE_ERROR, 6)",
		},
		{
			name: "SETTINGS ACK frame, Header.Length != 0 (is 1)",
			inputBytes: []byte{
				0x00, 0x00, 0x01, // Length = 1
				byte(http2.FrameSettings), byte(http2.FlagSettingsAck), // Type=SETTINGS, Flags=ACK
				0x00, 0x00, 0x00, 0x00, // StreamID=0
				0xAA, // Dummy payload byte
			},
			expectedErrStr: "parsing SETTINGS payload: SETTINGS ACK frame must have a payload length of 0, got 1",
		},
		{
			name: "SETTINGS frame, Header.Length not multiple of 6 (is 5)",
			inputBytes: []byte{
				0x00, 0x00, 0x05, // Length = 5
				byte(http2.FrameSettings), 0x00, // Type=SETTINGS, Flags=0
				0x00, 0x00, 0x00, 0x00, // StreamID=0
				0x01, 0x02, 0x03, 0x04, 0x05, // Dummy payload
			},
			expectedErrStr: "parsing SETTINGS payload: SETTINGS frame payload length 5 is not a multiple of 6",
		},
		{
			name: "PING frame, Header.Length != 8 (is 7)",
			inputBytes: []byte{
				0x00, 0x00, 0x07, // Length = 7
				byte(http2.FramePing), 0x00, // Type=PING, Flags=0
				0x00, 0x00, 0x00, 0x00, // StreamID=0
				1, 2, 3, 4, 5, 6, 7, // Dummy payload
			},
			expectedErrStr: "connection error: PING frame payload must be 8 bytes, got 7 (last_stream_id 0, code FRAME_SIZE_ERROR, 6)",
		},
		{
			name: "WINDOW_UPDATE frame, Header.Length != 4 (is 3)",
			inputBytes: []byte{
				0x00, 0x00, 0x03, // Length = 3
				byte(http2.FrameWindowUpdate), 0x00, // Type=WINDOW_UPDATE, Flags=0
				0x00, 0x00, 0x00, 0x01, // StreamID=1
				1, 2, 3, // Dummy payload
			},
			expectedErrStr: "parsing WINDOW_UPDATE payload: WINDOW_UPDATE frame payload must be 4 bytes, got 3",
		},
		{
			name: "GOAWAY frame, Header.Length < 8 (is 7)",
			inputBytes: []byte{
				0x00, 0x00, 0x07, // Length = 7
				byte(http2.FrameGoAway), 0x00, // Type=GOAWAY, Flags=0
				0x00, 0x00, 0x00, 0x00, // StreamID=0
				1, 2, 3, 4, 5, 6, 7, // Dummy payload
			},
			expectedErrStr: "parsing GOAWAY payload: GOAWAY frame payload must be at least 8 bytes, got 7",
		},
		{
			name: "DataFrame PADDED, PadLength exceeds payload",
			inputBytes: []byte{
				0x00, 0x00, 0x01, // Length = 1 (for PadLength byte only)
				byte(http2.FrameData), byte(http2.FlagDataPadded), // Type=DATA, Flags=PADDED
				0x00, 0x00, 0x00, 0x01, // StreamID=1
				0x05, // PadLength = 5, but payload is only this byte
			},
			expectedErrStr: "parsing DATA payload: pad length 5 exceeds payload length 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := bytes.NewBuffer(tt.inputBytes)
			_, err := http2.ReadFrame(buf)

			if err == nil {
				t.Fatalf("ReadFrame() expected an error, got nil. Input: %x", tt.inputBytes)
			}

			if tt.expectedError != nil { // Check for sentinel errors like io.EOF
				if !errors.Is(err, tt.expectedError) {
					t.Errorf("ReadFrame() error mismatch.\nExpected (to be one of): %v\nGot:      %v (type %T)", tt.expectedError, err, err)
				}
			} else if tt.expectedErrStr != "" { // Check for specific error messages
				if !strings.Contains(err.Error(), tt.expectedErrStr) {
					t.Errorf("ReadFrame() error string mismatch.\nExpected to contain: '%s'\nGot:      '%v'", tt.expectedErrStr, err)
				}
			} else {
				// This state implies the test case is misconfigured.
				t.Fatal("Test case misconfiguration: an error was returned, but no expectedError or expectedErrStr was set.")
			}
		})
	}
}

// cloneFrame creates a deep copy of a frame instance, primarily by serializing and deserializing it.
// This is useful for getting an independent copy for testing, especially since WriteFrame
// modifies the Header.Length field of the frame it processes.
func cloneFrame(original http2.Frame, t *testing.T) http2.Frame {
	t.Helper()

	// Before serializing the 'original' frame to clone it, ensure its
	// FrameHeader.Length accurately reflects its PayloadLen().
	// The `WriteFrame` function itself does this as its first step.
	// If `original` comes from `validFramesTestCases` which are prepared for `TestReadFrame_ValidFrames`,
	// its Length is already set to PayloadLen().
	// If `original.Header().Length` was not already equal to `original.PayloadLen()`,
	// `http2.WriteFrame` would correct it before writing the header.
	// So, direct use of `http2.WriteFrame` is fine here.

	var buf bytes.Buffer
	err := http2.WriteFrame(&buf, original)
	if err != nil {
		// If the original frame itself is malformed in a way that WriteFrame rejects (e.g. inconsistent PayloadLen vs WritePayload)
		// this would be caught by other tests. Here, we assume original is a valid frame definition.
		t.Fatalf("cloneFrame: WriteFrame failed for original frame type %T: %v", original, err)
	}

	cloned, err := http2.ReadFrame(bytes.NewBuffer(buf.Bytes()))
	if err != nil {
		t.Fatalf("cloneFrame: ReadFrame failed: %v. Serialized bytes: %x", err, buf.Bytes())
	}
	if cloned == nil {
		t.Fatal("cloneFrame: ReadFrame returned nil frame without error")
	}
	return cloned
}

// TestWriteFrame_OutputVerification tests that http2.WriteFrame correctly serializes
// various frame types into the expected byte sequences.
func TestWriteFrame_OutputVerification(t *testing.T) {
	for _, tt := range validFramesTestCases {
		t.Run(tt.name, func(t *testing.T) {
			// 1. Create a pristine copy of the frame from the test case.
			// This ensures that any modifications by WriteFrame in previous iterations
			// or parallel tests (if t.Parallel were used) do not interfere.
			cleanFrameForExpectation := cloneFrame(tt.originalFrame, t)

			// 2. Determine the expected payload length and actual payload bytes.
			expectedPayloadLen := cleanFrameForExpectation.PayloadLen()
			var expectedPayloadBuf bytes.Buffer
			payloadBytesWritten, err := cleanFrameForExpectation.WritePayload(&expectedPayloadBuf)
			if err != nil {
				t.Fatalf("cleanFrameForExpectation.WritePayload() failed: %v", err)
			}
			// This check is crucial: WritePayload must write what PayloadLen reports.
			// WriteFrame relies on this consistency.
			if uint32(payloadBytesWritten) != expectedPayloadLen {
				t.Fatalf("cleanFrameForExpectation.WritePayload() wrote %d bytes, but PayloadLen() is %d. Frame: %#v",
					payloadBytesWritten, expectedPayloadLen, cleanFrameForExpectation)
			}
			expectedPayloadBytes := expectedPayloadBuf.Bytes()

			// 3. Determine the expected header bytes.
			// The header that WriteFrame *should* write will have its Length field
			// set to expectedPayloadLen.
			expectedHeaderToSerialize := *cleanFrameForExpectation.Header() // Get a copy of the header
			expectedHeaderToSerialize.Length = expectedPayloadLen           // Set the correct length

			var expectedHeaderBuf bytes.Buffer
			if _, err := expectedHeaderToSerialize.WriteTo(&expectedHeaderBuf); err != nil {
				t.Fatalf("expectedHeaderToSerialize.WriteTo() failed: %v", err)
			}
			expectedHeaderBytes := expectedHeaderBuf.Bytes()

			// 4. Construct the total expected byte sequence for the full frame.
			expectedTotalBytes := append(expectedHeaderBytes, expectedPayloadBytes...)

			// 5. Call http2.WriteFrame on another clean copy of the frame.
			// WriteFrame will modify frameToWrite.Header().Length in place.
			frameToWrite := cloneFrame(tt.originalFrame, t)
			var actualWriteBuf bytes.Buffer
			err = http2.WriteFrame(&actualWriteBuf, frameToWrite)
			if err != nil {
				t.Fatalf("http2.WriteFrame() failed: %v. Frame input: %#v", err, frameToWrite)
			}

			// 6. Compare the actual written bytes with the expected total bytes.
			if !bytes.Equal(expectedTotalBytes, actualWriteBuf.Bytes()) {
				t.Errorf("http2.WriteFrame() output mismatch for %s (%T).\nExpected: %x (%d bytes: %dH + %dP)\nActual:   %x (%d bytes)\nInput Frame (for expectation): %#v\nFrame after WriteFrame: %#v",
					tt.name, tt.originalFrame,
					expectedTotalBytes, len(expectedTotalBytes), len(expectedHeaderBytes), len(expectedPayloadBytes),
					actualWriteBuf.Bytes(), actualWriteBuf.Len(),
					cleanFrameForExpectation, frameToWrite)
			}

			// 7. Verify that WriteFrame correctly set the Header().Length on the frame it processed.
			if frameToWrite.Header().Length != expectedPayloadLen {
				t.Errorf("frameToWrite.Header().Length after WriteFrame (%d) was not set to expectedPayloadLen (%d)",
					frameToWrite.Header().Length, expectedPayloadLen)
			}
		})
	}
}

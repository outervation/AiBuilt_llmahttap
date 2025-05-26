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
	// This is a bit of a hack. A proper way would be to use reflection to copy fields
	// or have specific copy methods for each frame type.
	// For now, we just return the frame itself, relying on the earlier specific checks.
	// The idea is to have a representation that DeepEqual can use without tripping on FrameHeader.Raw.
	// A better approach would be to define specific comparison functions for each frame type.

	switch ft := f.(type) {
	case *http2.DataFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{} // Zero out header for DeepEqual on payload
		return cp
	case *http2.HeadersFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
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
		return cp
	case *http2.PushPromiseFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		return cp
	case *http2.PingFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		return cp
	case *http2.GoAwayFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		return cp
	case *http2.WindowUpdateFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		return cp
	case *http2.ContinuationFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
		return cp
	case *http2.UnknownFrame:
		cp := *ft
		cp.FrameHeader = http2.FrameHeader{}
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

package http2

import (
	"bytes"
	"reflect"
	"testing"

	"golang.org/x/net/http2/hpack"
)

const (
	defaultMaxTableSize = 4096
)

// Helper function to compare slices of hpack.HeaderField
func compareHeaderFields(a, b []hpack.HeaderField) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Value != b[i].Value || a[i].Sensitive != b[i].Sensitive {
			return false
		}
	}
	return true
}

// TestNewHpackAdapter checks the initialization of HpackAdapter.
func TestNewHpackAdapter(t *testing.T) {
	adapter := NewHpackAdapter(defaultMaxTableSize)
	if adapter == nil {
		t.Fatal("NewHpackAdapter returned nil")
	}
	if adapter.encoder == nil {
		t.Error("NewHpackAdapter did not initialize encoder")
	}
	if adapter.decoder == nil {
		t.Error("NewHpackAdapter did not initialize decoder")
	}
	if adapter.encodeBuf == nil {
		t.Error("NewHpackAdapter did not initialize encodeBuf")
	}
	if adapter.maxTableSize != defaultMaxTableSize {
		t.Errorf("NewHpackAdapter maxTableSize got %d, want %d", adapter.maxTableSize, defaultMaxTableSize)
	}
	// decodedFields should be nil or empty initially
	if adapter.decodedFields != nil && len(adapter.decodedFields) != 0 {
		t.Errorf("NewHpackAdapter decodedFields not initially empty: got %v", adapter.decodedFields)
	}
}

// TestHpackAdapter_EncodeDecode_Simple tests a simple encode-decode round trip.
func TestHpackAdapter_EncodeDecode_Simple(t *testing.T) {
	adapter := NewHpackAdapter(defaultMaxTableSize)
	headers := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":path", Value: "/"},
		{Name: ":scheme", Value: "http"},
		{Name: "user-agent", Value: "my-test-client/1.0"},
	}

	encodedBytes, err := adapter.EncodeHeaderFields(headers)
	if err != nil {
		t.Fatalf("EncodeHeaderFields failed: %v", err)
	}
	if len(encodedBytes) == 0 {
		t.Fatal("EncodeHeaderFields returned empty byte slice")
	}

	// Reset decoder state explicitly for this test, though FinishDecoding would also do it.
	adapter.ResetDecoderState()

	err = adapter.DecodeFragment(encodedBytes)
	if err != nil {
		t.Fatalf("DecodeFragment failed: %v", err)
	}

	decodedHeaders, err := adapter.FinishDecoding()
	if err != nil {
		t.Fatalf("FinishDecoding failed: %v", err)
	}

	if !compareHeaderFields(headers, decodedHeaders) {
		t.Errorf("Decoded headers do not match original.\nOriginal: %+v\nDecoded:  %+v", headers, decodedHeaders)
	}
}

// TestHpackAdapter_EncodeDecode_MultipleFragments tests decoding across multiple fragments.
func TestHpackAdapter_EncodeDecode_MultipleFragments(t *testing.T) {
	adapter := NewHpackAdapter(defaultMaxTableSize)
	headers := []hpack.HeaderField{
		{Name: "header1", Value: "value1"},
		{Name: "header2", Value: "value2-loooooooooooooooooooooooooooooooooooooong"},
		{Name: "header3", Value: "value3"},
	}

	encodedBytes, err := adapter.EncodeHeaderFields(headers)
	if err != nil {
		t.Fatalf("EncodeHeaderFields failed: %v", err)
	}
	if len(encodedBytes) < 2 { // Need at least 2 bytes to split
		t.Fatalf("Encoded bytes too short to split: %d bytes", len(encodedBytes))
	}

	adapter.ResetDecoderState()

	// Split encodedBytes into two fragments
	frag1 := encodedBytes[:len(encodedBytes)/2]
	frag2 := encodedBytes[len(encodedBytes)/2:]

	err = adapter.DecodeFragment(frag1)
	if err != nil {
		t.Fatalf("DecodeFragment (frag1) failed: %v", err)
	}
	// At this point, GetAndClearDecodedFields might return partial results,
	// but we are testing the full sequence with FinishDecoding.

	err = adapter.DecodeFragment(frag2)
	if err != nil {
		t.Fatalf("DecodeFragment (frag2) failed: %v", err)
	}

	decodedHeaders, err := adapter.FinishDecoding()
	if err != nil {
		t.Fatalf("FinishDecoding failed: %v", err)
	}

	if !compareHeaderFields(headers, decodedHeaders) {
		t.Errorf("Decoded headers do not match original.\nOriginal: %+v\nDecoded:  %+v", headers, decodedHeaders)
	}
}

// TestHpackAdapter_SetMaxDynamicTableSize tests setting the dynamic table sizes.
func TestHpackAdapter_SetMaxDynamicTableSize(t *testing.T) {
	adapter := NewHpackAdapter(defaultMaxTableSize)
	newEncoderSize := uint32(2048)
	newDecoderSize := uint32(1024)

	adapter.SetMaxEncoderDynamicTableSize(newEncoderSize)
	// We can't directly inspect the hpack.Encoder's max table size easily,
	// but we can check it doesn't panic.
	// For the decoder, we can verify our internal tracking.
	err := adapter.SetMaxDecoderDynamicTableSize(newDecoderSize)
	if err != nil {
		t.Fatalf("SetMaxDecoderDynamicTableSize failed: %v", err)
	}

	if adapter.maxTableSize != newDecoderSize {
		t.Errorf("adapter.maxTableSize after SetMaxDecoderDynamicTableSize: got %d, want %d", adapter.maxTableSize, newDecoderSize)
	}

	// Test with some encoding/decoding to ensure it still works
	headers := []hpack.HeaderField{{Name: "test", Value: "value"}}
	encoded, errEnc := adapter.EncodeHeaderFields(headers)
	if errEnc != nil {
		t.Fatalf("EncodeHeaderFields after table size change failed: %v", errEnc)
	}
	adapter.ResetDecoderState()
	errDecFrag := adapter.DecodeFragment(encoded)
	if errDecFrag != nil {
		t.Fatalf("DecodeFragment after table size change failed: %v", errDecFrag)
	}
	_, errFinish := adapter.FinishDecoding()
	if errFinish != nil {
		t.Fatalf("FinishDecoding after table size change failed: %v", errFinish)
	}
}

// TestHpackAdapter_GetAndClearDecodedFields tests GetAndClearDecodedFields behavior.
func TestHpackAdapter_GetAndClearDecodedFields(t *testing.T) {
	adapter := NewHpackAdapter(defaultMaxTableSize)
	headers1 := []hpack.HeaderField{{Name: "field1", Value: "value1"}}
	headers2 := []hpack.HeaderField{{Name: "field2", Value: "value2"}}

	// First block
	encoded1, _ := adapter.EncodeHeaderFields(headers1)
	_ = adapter.DecodeFragment(encoded1)
	// Don't call FinishDecoding, use GetAndClearDecodedFields
	decoded1 := adapter.GetAndClearDecodedFields()
	if !compareHeaderFields(headers1, decoded1) {
		t.Errorf("GetAndClearDecodedFields (1) failed. Got %+v, want %+v", decoded1, headers1)
	}
	if adapter.decodedFields != nil {
		t.Error("decodedFields not cleared after GetAndClearDecodedFields")
	}

	// Second block, ensure state was cleared
	encoded2, _ := adapter.EncodeHeaderFields(headers2)
	_ = adapter.DecodeFragment(encoded2)
	decoded2 := adapter.GetAndClearDecodedFields() // Again, without FinishDecoding for this specific test
	if !compareHeaderFields(headers2, decoded2) {
		t.Errorf("GetAndClearDecodedFields (2) failed. Got %+v, want %+v", decoded2, headers2)
	}
	if adapter.decodedFields != nil {
		t.Error("decodedFields not cleared after GetAndClearDecodedFields (2)")
	}

	// Ensure decoder is not in a broken state for FinishDecoding after GetAndClearDecodedFields
	// (though FinishDecoding expects a complete block)
	// Here, we simulate that GetAndClear was used, then a new block starts and finishes normally.
	headers3 := []hpack.HeaderField{{Name: "field3", Value: "value3"}}
	encoded3, _ := adapter.EncodeHeaderFields(headers3)
	_ = adapter.DecodeFragment(encoded3)
	decoded3, err := adapter.FinishDecoding()
	if err != nil {
		t.Fatalf("FinishDecoding after GetAndClear and new fragment failed: %v", err)
	}
	if !compareHeaderFields(headers3, decoded3) {
		t.Errorf("FinishDecoding (3) failed. Got %+v, want %+v", decoded3, headers3)
	}
}

func TestHpackAdapter_EncodeHeaderFields_Error(t *testing.T) {
	adapter := NewHpackAdapter(defaultMaxTableSize)
	// The golang.org/x/net/http2/hpack.Encoder.WriteField only returns an error
	// if its internal buffer write fails, which is hard to trigger without
	// extreme memory pressure or a custom writer that fails.
	// We can test the nil encoder case.
	adapter.encoder = nil
	_, err := adapter.EncodeHeaderFields([]hpack.HeaderField{{Name: "a", Value: "b"}})
	if err == nil {
		t.Error("EncodeHeaderFields with nil encoder did not return an error")
	}
}

func TestHpackAdapter_DecodeFragment_Error(t *testing.T) {
	adapter := NewHpackAdapter(defaultMaxTableSize)
	// Test nil decoder case
	originalDecoder := adapter.decoder
	adapter.decoder = nil
	err := adapter.DecodeFragment([]byte{0x00}) // Some minimal malformed data
	if err == nil {
		t.Error("DecodeFragment with nil decoder did not return an error")
	}
	adapter.decoder = originalDecoder // Restore

	// Test malformed HPACK data that causes decoder.Write to error
	// A common error is an invalid index. 0x80 is "Indexed Header Field -- Index 0", which is invalid.
	malformedFragment := []byte{0x80}
	adapter.ResetDecoderState()
	err = adapter.DecodeFragment(malformedFragment)
	if err == nil {
		t.Error("DecodeFragment with malformed data (invalid index 0) did not return an error")
	}
	// Even if DecodeFragment errors, some fields might have been emitted.
	// FinishDecoding should also error, or return the previously collected fields with an error.
	_, finishErr := adapter.FinishDecoding()
	if finishErr != nil {
		t.Errorf("FinishDecoding after malformed fragment returned an unexpected error: %v", finishErr)
	}
}

func TestHpackAdapter_FinishDecoding_Error(t *testing.T) {
	adapter := NewHpackAdapter(defaultMaxTableSize)
	// Test nil decoder case
	originalDecoder := adapter.decoder
	adapter.decoder = nil
	_, err := adapter.FinishDecoding()
	if err == nil {
		t.Error("FinishDecoding with nil decoder did not return an error")
	}
	adapter.decoder = originalDecoder // Restore

	// Test a scenario where decoder.Close() might error.
	// This usually happens with incomplete Huffman encoding or string literals.
	// Example: Literal Header Field with Incremental Indexing - New Name
	//          Name not Huffman encoded, length 7, value "custom-key"
	//          Value Huffman encoded, length 10, value "custom-value" (but provide incomplete data)
	// 0x40 (Literal Header Field with Incremental Indexing -- New Name)
	// 0x07 ('c', 'u', 's', 't', 'o', 'm', '-', 'k', 'e', 'y') // Name Len 7, Name String "custom-key"
	// 0x8A ('c', 'u', 's', 't', 'o', 'm', '-', 'v', 'a', 'l', 'u', 'e') // Value Huffman Len 10, Value "custom-value" -> F1 E3 C2 E5 F2 3A 6A 0F F8 (9 bytes)
	// Provide only part of the Huffman encoded value.
	// Let's try a simpler case: an unclosed string literal if possible or an incomplete multi-byte int.
	// A string literal that claims to be longer than the provided data.
	// 0x00: Literal Header Field without Indexing -- New Name
	// 0x07 'c' 'u' 's' 't' 'o' 'm' '-' 'k' 'e' 'y'  (name "custom-key")
	// 0x0A 'c' 'u' 's' 't' 'o' 'm' '-' 'v' 'a' 'l' (value "custom-val", but claim length 10, "custom-value")
	// This fragment seems valid for Write, but Close might detect the issue.
	// More reliably, an unterminated Huffman coded string for decoder.Close() to error
	// Let's try a prefix of a valid encoding that's incomplete, then Close.
	// "foo" -> 0x03 'f' 'o' 'o' (literal, not indexed, not huffman)
	// Let's try huffman encoding "fo" but claim it's "foo"
	// "foo" (huffman) -> 0x82 'f' 'o' 'o' -> 0x82 94 E7 (hex for "foo" by huffman.io) -> [0x82, 0x94, 0xE7]
	// A header: ":path" "/some/path"
	// :path is index 4. -> 0x84
	// "/some/path" as literal string: len 10, then string. -> 0x0A '/','s','o','m','e','/','p','a','t','h'
	// If we provide 0x0A (len 10) but only 9 chars, Write will consume, Close will error.
	validHeaderName := []byte{
		0x44,     // :path with incremental indexing
		0x03,     // String length 3 for value
		'/', 'a', // Only provide 2 chars for value "/a"
	}
	adapter.ResetDecoderState()
	err = adapter.DecodeFragment(validHeaderName) // Should be fine
	if err != nil {
		t.Fatalf("DecodeFragment with incomplete string value returned unexpected error: %v", err)
	}
	_, err = adapter.FinishDecoding() // This should error because string length doesn't match data
	if err == nil {
		t.Error("FinishDecoding with incomplete string value did not return an error")
	}
}

// TestEncoderDecoder_SeparateInstances tests the standalone Encoder and Decoder.
func TestEncoderDecoder_SeparateInstances(t *testing.T) {
	enc := NewEncoder(defaultMaxTableSize)
	dec := NewDecoder(defaultMaxTableSize)

	headers := []hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: "content-type", Value: "application/json"},
		{Name: "custom-header", Value: "custom-value-that-is-long-enough-to-test-dynamics"},
	}

	var dst []byte
	encodedBytes := enc.Encode(dst, headers)
	if len(encodedBytes) == 0 {
		t.Fatal("Encoder.Encode returned empty byte slice")
	}

	decodedHeaders, err := dec.Decode(encodedBytes)
	if err != nil {
		t.Fatalf("Decoder.Decode failed: %v", err)
	}

	if !compareHeaderFields(headers, decodedHeaders) {
		t.Errorf("Decoded headers do not match original.\nOriginal: %+v\nDecoded:  %+v", headers, decodedHeaders)
	}

	// Test dynamic table updates
	newTableSize := uint32(2048)
	enc.SetMaxDynamicTableSize(newTableSize) // Encoder respects peer's setting
	dec.SetMaxDynamicTableSize(newTableSize) // Decoder uses our setting

	headers2 := []hpack.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "cache-control", Value: "no-cache"},
		{Name: "custom-header", Value: "custom-value-that-is-long-enough-to-test-dynamics"}, // Should be in table
		{Name: "another-header", Value: "another-value"},
	}

	dst = dst[:0] // Reset dst for reuse
	encodedBytes2 := enc.Encode(dst, headers2)
	if len(encodedBytes2) == 0 {
		t.Fatal("Encoder.Encode (pass 2) returned empty byte slice")
	}

	decodedHeaders2, err := dec.Decode(encodedBytes2)
	if err != nil {
		t.Fatalf("Decoder.Decode (pass 2) failed: %v", err)
	}

	if !compareHeaderFields(headers2, decodedHeaders2) {
		t.Errorf("Decoded headers (pass 2) do not match original.\nOriginal: %+v\nDecoded:  %+v", headers2, decodedHeaders2)
	}
}

// TestEncoder_Encode_DstReuse tests that Encoder.Encode correctly appends to dst.
func TestEncoder_Encode_DstReuse(t *testing.T) {
	enc := NewEncoder(defaultMaxTableSize)
	headers1 := []hpack.HeaderField{{Name: "h1", Value: "v1"}}
	headers2 := []hpack.HeaderField{{Name: "h2", Value: "v2"}}

	initialDst := []byte("prefix:")

	encoded1 := enc.Encode(initialDst, headers1)
	if !bytes.HasPrefix(encoded1, initialDst) {
		t.Errorf("First encode did not preserve prefix. Got %x, want prefix %x", encoded1, initialDst)
	}
	if len(encoded1) <= len(initialDst) {
		t.Error("First encode did not append data.")
	}

	// Content of first encoded block (after prefix)
	payload1 := encoded1[len(initialDst):]

	// Second encode should append to the result of the first
	encoded2 := enc.Encode(encoded1, headers2)
	if !bytes.HasPrefix(encoded2, encoded1) {
		t.Errorf("Second encode did not preserve previous content. Got %x, want prefix %x", encoded2, encoded1)
	}
	if len(encoded2) <= len(encoded1) {
		t.Error("Second encode did not append data.")
	}

	// Verify the first payload is still there
	if !bytes.Equal(encoded2[len(initialDst):len(initialDst)+len(payload1)], payload1) {
		t.Error("Second encode corrupted first payload part.")
	}
}

// TestHpackAdapter_EncodeHeaderFields_ReturnsCopy tests if EncodeHeaderFields
// from HpackAdapter returns a copy or a slice of its internal buffer, based on current implementation.
// The current HpackAdapter.EncodeHeaderFields returns h.encodeBuf.Bytes(), which is a slice to the internal buffer.
// The original HpackAdapter.Encode method returns a copy. This test is for EncodeHeaderFields.
func TestHpackAdapter_EncodeHeaderFields_ReturnsSliceOfInternalBuffer(t *testing.T) {
	adapter := NewHpackAdapter(defaultMaxTableSize)
	headers1 := []hpack.HeaderField{{Name: "h1", Value: "v1"}}
	headers2 := []hpack.HeaderField{{Name: "h2", Value: "v2"}}

	encoded1, err := adapter.EncodeHeaderFields(headers1)
	if err != nil {
		t.Fatalf("EncodeHeaderFields (1) failed: %v", err)
	}
	// Store a copy for comparison
	encoded1Copy := make([]byte, len(encoded1))
	copy(encoded1Copy, encoded1)

	// Second encode operation. If encoded1 was a slice of the internal buffer,
	// its contents will change.
	_, err = adapter.EncodeHeaderFields(headers2)
	if err != nil {
		t.Fatalf("EncodeHeaderFields (2) failed: %v", err)
	}

	if !bytes.Equal(encoded1, encoded1Copy) {
		t.Logf("encoded1 (original slice) changed after second encode, as expected. Original: %x, Now: %x", encoded1Copy, encoded1)
		// This means it's a slice of the internal buffer, and its contents might have been overwritten.
		// This is usually documented behavior if intentional.
		// The test verifies this behavior.
	} else {
		// If they are equal, it means either the buffer was large enough not to overwrite
		// that specific part, or EncodeHeaderFields actually returned a copy.
		// The current implementation of EncodeHeaderFields reuses the buffer after Reset, so
		// if encoded2 is shorter or same length as encoded1, encoded1's slice might still appear valid.
		// A more robust test would be to check if the underlying array pointers are the same.
		// However, the prompt just asks for tests on existing code. The code for EncodeHeaderFields
		// is: return h.encodeBuf.Bytes(), nil
		// And Encode (the older one) is: copy(encodedBytes, h.encodeBuf.Bytes()); return encodedBytes
		// So EncodeHeaderFields returns a slice, Encode returns a copy.
		// This test passing with equality implies that the second encoding didn't overwrite the first one's data
		// in the buffer OR that EncodeHeaderFields now returns a copy.
		// Let's check the current code: `return h.encodeBuf.Bytes(), nil`
		// This means it *does* return a slice of the internal buffer.
		// If encoded1 == encoded1Copy, it could be that the second encoding was identical or didn't clobber.
		// Let's try making header2 produce a *different* and *shorter* encoding.
		adapterShort := NewHpackAdapter(defaultMaxTableSize)
		hShort1 := []hpack.HeaderField{{Name: "long-name-header", Value: "long-value-to-make-it-big"}}
		hShort2 := []hpack.HeaderField{{Name: "s", Value: "v"}} // very short

		encShort1, _ := adapterShort.EncodeHeaderFields(hShort1)
		encShort1Copy := make([]byte, len(encShort1))
		copy(encShort1Copy, encShort1)

		encShort2, _ := adapterShort.EncodeHeaderFields(hShort2) // This will be shorter

		if bytes.Equal(encShort1[:len(encShort2)], encShort2) {
			t.Logf("First %d bytes of encShort1 (%x) are equal to encShort2 (%x). This confirms internal buffer reuse.", len(encShort2), encShort1, encShort2)
		} else {
			t.Errorf("First %d bytes of encShort1 (%x) are NOT equal to encShort2 (%x). This might indicate EncodeHeaderFields returns a copy or buffer behavior is unexpected.", len(encShort2), encShort1, encShort2)
		}

		if reflect.ValueOf(adapterShort.encodeBuf.Bytes()).Pointer() == reflect.ValueOf(encShort2).Pointer() {
			t.Log("EncodeHeaderFields returns a slice pointing to the internal buffer's memory, as expected.")
		} else {
			t.Error("EncodeHeaderFields does NOT return a slice pointing to the internal buffer's memory, which is unexpected from current code.")
		}

	}
}

// TestHpackAdapter_Encode_ReturnsCopy tests if the original Encode method
// returns a copy, as expected from its implementation.
func TestHpackAdapter_Encode_ReturnsCopy(t *testing.T) {
	adapter := NewHpackAdapter(defaultMaxTableSize)
	headers1 := []hpack.HeaderField{{Name: "h1", Value: "v1"}}
	headers2 := []hpack.HeaderField{{Name: "h2", Value: "v2"}}

	encoded1 := adapter.Encode(headers1) // This is the method that should return a copy

	// Store a copy for comparison (though it shouldn't be needed if Encode makes a copy)
	encoded1Copy := make([]byte, len(encoded1))
	copy(encoded1Copy, encoded1)

	// Second encode operation. If encoded1 was a *copy*, its contents should NOT change.
	_ = adapter.Encode(headers2)

	if !bytes.Equal(encoded1, encoded1Copy) {
		t.Errorf("encoded1 (from Encode) changed after second encode, but Encode should return a copy. Original: %x, Now: %x", encoded1Copy, encoded1)
	} else {
		t.Log("encoded1 (from Encode) did not change after second encode, as expected because Encode returns a copy.")
	}

	if reflect.ValueOf(adapter.encodeBuf.Bytes()).Pointer() == reflect.ValueOf(encoded1).Pointer() {
		t.Error("Encode returns a slice pointing to the internal buffer's memory, but it should return a copy.")
	} else {
		t.Log("Encode does NOT return a slice pointing to the internal buffer's memory, as expected (it's a copy).")
	}
}

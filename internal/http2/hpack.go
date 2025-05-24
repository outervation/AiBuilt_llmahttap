package http2

import (
	"bytes"
	"errors"
	"fmt"

	"golang.org/x/net/http2/hpack"
)

// HpackAdapter provides a unified interface for HPACK encoding and decoding.
// It wraps golang.org/x/net/http2/hpack.Encoder and hpack.Decoder,
// managing their state and associated buffers.
type HpackAdapter struct {
	encoder       *hpack.Encoder
	decoder       *hpack.Decoder
	encodeBuf     *bytes.Buffer
	decodedFields []hpack.HeaderField // Buffer for storing decoded fields, reset per decoding operation
	maxTableSize  uint32              // Stores the current max dynamic table size
}

// emitHeaderField is the callback function for the hpack.Decoder.
// It appends decoded header fields to the HpackAdapter's decodedFields slice.
func (h *HpackAdapter) emitHeaderField(hf hpack.HeaderField) {
	// According to golang.org/x/net/http2/hpack documentation,
	// HeaderField is a struct with string fields. Strings are immutable,
	// so direct append is safe. If Name/Value were slices or pointers
	// to mutable data, a deep copy would be needed.
	h.decodedFields = append(h.decodedFields, hf)
}

// NewHpackAdapter creates a new HpackAdapter for HPACK encoding and decoding.
//
// The initialMaxTableSize parameter specifies the initial capacity for the dynamic
// header compression tables.
//
// It initializes:
//   - An internal buffer (`encodeBuf`) for HPACK encoding operations.
//   - An HPACK encoder (`hpack.Encoder`), configured to write to `encodeBuf`.
//     Its maximum dynamic table size is set to initialMaxTableSize. This value dictates
//     the maximum size of the dynamic table the encoder will use. It should generally
//     be updated via SetEncoderMaxTableSize() once the peer's SETTINGS_HEADER_TABLE_SIZE
//     is known, as the encoder must respect the peer's limit.
//   - An HPACK decoder (`hpack.Decoder`). Its dynamic table size is set to
//     initialMaxTableSize. This is the size of the dynamic table our server will allocate
//     and use for decoding incoming headers from the peer. This value should align with
//     the SETTINGS_HEADER_TABLE_SIZE our server advertises to the peer.
//     The decoder is configured with an emit function (the adapter's emitHeaderField method)
//     to collect decoded hpack.HeaderField values internally.
//
// The adapter's internal maxTableSize field (primarily reflecting the decoder's current
// table size setting) is also initialized to initialMaxTableSize.
//
// See RFC 7541 (HPACK) Section 4.2 "Dynamic Table Management" and RFC 7540 (HTTP/2)
// Section 6.5.2 "SETTINGS_HEADER_TABLE_SIZE" for more details on dynamic table sizing.
func NewHpackAdapter(initialMaxTableSize uint32) *HpackAdapter {
	adapter := &HpackAdapter{
		encodeBuf:     new(bytes.Buffer),
		decodedFields: nil, // Starts empty, will be populated during decoding
		maxTableSize:  initialMaxTableSize,
	}

	// Initialize HPACK encoder
	adapter.encoder = hpack.NewEncoder(adapter.encodeBuf)
	adapter.encoder.SetMaxDynamicTableSize(initialMaxTableSize)

	// Initialize HPACK decoder
	// The emit function is bound to this HpackAdapter instance.
	adapter.decoder = hpack.NewDecoder(initialMaxTableSize, adapter.emitHeaderField)

	return adapter
}

// DecodeFragment processes a fragment of an HPACK-encoded header block.
// Decoded header fields are accumulated internally. Call FinishDecoding
// after all fragments (e.g., from HEADERS and subsequent CONTINUATION frames)
// have been processed to get the complete list of header fields and to
// finalize the decoding state for the header block.
//
// Each call to DecodeFragment appends to an internal list of headers.
// This list is cleared by FinishDecoding or can be manually reset by
// calling ResetDecoderState before processing a new header block.
func (h *HpackAdapter) DecodeFragment(fragment []byte) error {
	if h.decoder == nil {
		// This case should ideally not be reached if NewHpackAdapter is always used.
		return errors.New("hpack: HpackAdapter.decoder not initialized")
	}
	// The h.decodedFields slice accumulates headers via h.emitHeaderField.
	// It is implicitly ready for new fields. It's cleared by FinishDecoding
	// or ResetDecoderState.
	_, err := h.decoder.Write(fragment)
	if err != nil {
		return fmt.Errorf("hpack: HpackAdapter.decoder.Write failed: %w", err)
	}
	return nil
}

// FinishDecoding finalizes the decoding of the current header block,
// returns all accumulated header fields, and resets the internal decoding state
// for the next header block. It must be called after all fragments of a
// header block have been passed to DecodeFragment. This method calls
// the underlying HPACK decoder's Close method, which can also return errors
// if the HPACK stream is malformed at its conclusion.
func (h *HpackAdapter) FinishDecoding() ([]hpack.HeaderField, error) {
	if h.decoder == nil {
		return nil, errors.New("hpack: HpackAdapter.decoder not initialized")
	}

	// Finalize current HPACK block processing by calling Close on the underlying decoder.
	// This is necessary to process any remaining state and validate the end of the block.
	err := h.decoder.Close()

	fields := h.decodedFields
	h.decodedFields = nil // Reset for the next header block

	if err != nil {
		// Even if Close() errors, return any fields that were successfully decoded before the error.
		return fields, fmt.Errorf("hpack: HpackAdapter.decoder.Close failed: %w", err)
	}
	return fields, nil
}

// GetAndClearDecodedFields returns a copy of the currently accumulated decoded header
// fields and then clears the internal buffer of decoded fields to prepare for
// decoding the next header block.
// This method is an alternative to FinishDecoding if the caller wants to manage
// the "end of header block" signal (e.g., END_HEADERS flag) and potential errors
// from decoder.Close() separately, or if they need to access intermediately
// decoded headers before the block is fully complete (though the latter is less common
// for standard HPACK usage).
// It's important to note that not calling decoder.Close() (as FinishDecoding does)
// means that any final validation or state updates performed by decoder.Close()
// will not occur when using only this method.
func (h *HpackAdapter) GetAndClearDecodedFields() []hpack.HeaderField {
	if len(h.decodedFields) == 0 {
		return nil
	}
	// Return a copy, not the slice itself, to prevent external modification
	// of the returned slice affecting future appends if the slice capacity was reused.
	fieldsCopy := make([]hpack.HeaderField, len(h.decodedFields))
	copy(fieldsCopy, h.decodedFields)

	// Clear the internal slice for the next block.
	// Setting to nil is generally preferred as it allows the underlying array to be GC'd
	// if there are no other references.
	h.decodedFields = nil
	return fieldsCopy
}

// ResetDecoderState clears any accumulated decoded header fields from the HpackAdapter.
// This is useful if a header block decoding sequence needs to be aborted
// or to ensure a clean state before starting a new header block if
// FinishDecoding was not called on the previous one.
// Note: This does not reset the HPACK decoder's dynamic table state itself,
// only the adapter's list of collected fields from the current block.
// The underlying hpack.Decoder is expected to be ready for a new block
// after Close() is called (even if it errored) or if it's freshly initialized.
func (h *HpackAdapter) ResetDecoderState() {
	h.decodedFields = nil
}

// SetDecoderMaxTableSize updates the maximum dynamic table size for the HPACK decoder.
// This should be called when the peer signals a change via a SETTINGS_HEADER_TABLE_SIZE update.
func (h *HpackAdapter) SetDecoderMaxTableSize(size uint32) {
	if h.decoder != nil {
		h.decoder.SetMaxDynamicTableSize(size)
	}
	h.maxTableSize = size // Update stored size, assuming it's for the decoder or a shared default
}

// SetEncoderMaxTableSize updates the maximum dynamic table size for the HPACK encoder.
// This reflects the maximum table size that the remote peer (our decoder) can handle,
// learned via its SETTINGS_HEADER_TABLE_SIZE.
func (h *HpackAdapter) SetEncoderMaxTableSize(size uint32) {
	if h.encoder != nil {
		h.encoder.SetMaxDynamicTableSize(size)
	}
	// If HpackAdapter.maxTableSize was intended to be specific to the encoder,
	// this would update it. For now, assuming it's decoder-related or a general default.
}

// Encode encodes a list of header fields using HPACK.
// The encoded bytes are returned as a new slice.
func (h *HpackAdapter) Encode(headers []hpack.HeaderField) []byte {
	if h.encoder == nil {
		// This case should ideally not be reached if NewHpackAdapter is always used.
		// Consider logging an error or returning an error if the API allowed.
		return nil
	}
	h.encodeBuf.Reset() // Reset internal buffer for this encoding operation
	for _, hf := range headers {
		h.encoder.WriteField(hf)
	}
	// Return a copy of the encoded bytes, as the internal buffer will be reused.
	encodedBytes := make([]byte, h.encodeBuf.Len())
	copy(encodedBytes, h.encodeBuf.Bytes())
	return encodedBytes
}

// EncodeHeaderFields encodes a list of header fields using HPACK.
// It writes the encoded header block to an internal buffer and returns its bytes.
// The returned byte slice is valid until the next call to a method that modifies
// the HpackAdapter's internal encode buffer (e.g., another call to EncodeHeaderFields
// or the existing Encode method). If the HpackAdapter's encoder is not initialized,
// it returns an error. If an error occurs during the encoding of any header field,
// it returns nil and the error.
func (h *HpackAdapter) EncodeHeaderFields(fields []hpack.HeaderField) ([]byte, error) {
	if h.encoder == nil {
		return nil, errors.New("hpack: HpackAdapter.encoder not initialized")
	}
	h.encodeBuf.Reset() // Reset internal buffer for this encoding operation
	for _, hf := range fields {
		if err := h.encoder.WriteField(hf); err != nil {
			// Return nil for bytes and the error if WriteField fails.
			return nil, fmt.Errorf("hpack: HpackAdapter.encoder.WriteField failed for header field %q: %w", hf.Name, err)
		}
	}
	// Return the bytes from the buffer. The caller should be aware that this slice
	// references the internal buffer's memory and is valid until the buffer is next modified.
	// A copy can be made by the caller if persistence beyond buffer modification is needed.
	return h.encodeBuf.Bytes(), nil
}

// Encoder wraps an hpack.Encoder.
type Encoder struct {
	hpackEncoder *hpack.Encoder
	buf          *bytes.Buffer
}

// NewEncoder creates a new HPACK encoder.
// maxDynTableSize is the maximum dynamic table size the encoder will use.
func NewEncoder(maxDynTableSize uint32) *Encoder {
	buf := new(bytes.Buffer)
	encoder := hpack.NewEncoder(buf)
	encoder.SetMaxDynamicTableSize(maxDynTableSize) // Apply the dynamic table size
	return &Encoder{
		hpackEncoder: encoder,
		buf:          buf,
	}
}

// Encode appends the HPACK encoding of headers to dst and returns the new dst.
func (e *Encoder) Encode(dst []byte, headers []hpack.HeaderField) []byte {
	e.buf.Reset() // Reset buffer for new encoding pass
	for _, hf := range headers {
		e.hpackEncoder.WriteField(hf)
	}
	return append(dst, e.buf.Bytes()...)
}

// SetMaxDynamicTableSize updates the maximum dynamic table size.
func (e *Encoder) SetMaxDynamicTableSize(size uint32) {
	e.hpackEncoder.SetMaxDynamicTableSize(size)
}

// Decoder wraps an hpack.Decoder.
type Decoder struct {
	hpackDecoder *hpack.Decoder
	maxTableSize uint32 // Store max table size for potential re-initialization if needed
}

// NewDecoder creates a new HPACK decoder.
// maxDynTableSize is the maximum dynamic table size the peer will use for encoding.
// maxMemory is currently not directly used by hpack.Decoder constructor in a way that limits total header list size explicitly.
// The hpack.Decoder limits string lengths with SetMaxStringLength, but not the count or total size of headers directly.
func NewDecoder(maxDynTableSize uint32) *Decoder {
	// The hpack.Decoder is initialized with a dynamic table size.
	// The reader is provided per-decode-operation via the DecodeRawBytes or Reset method.
	d := hpack.NewDecoder(maxDynTableSize, nil) // The 'nil' is for an optional emit function
	return &Decoder{
		hpackDecoder: d,
		maxTableSize: maxDynTableSize,
	}
}

// Decode decodes a header block from p.
// It uses an internal emit function to collect decoded HeaderFields.

// Decode decodes a header block from p.
// It uses an internal emit function to collect decoded HeaderFields.
func (d *Decoder) Decode(p []byte) ([]hpack.HeaderField, error) {
	var hfs []hpack.HeaderField
	emitFunc := func(hf hpack.HeaderField) {
		// According to golang.org/x/net/http2/hpack documentation,
		// HeaderField is a struct with string fields. Strings are immutable,
		// so direct append is safe. If Name/Value were slices or pointers
		// to mutable data, a deep copy would be needed.
		hfs = append(hfs, hf)
	}

	// Set the emit function for this specific decode operation.
	// The hpack.Decoder instance (d.hpackDecoder) is stateful regarding
	// its dynamic table, but the emit func can be changed per Decode call.
	d.hpackDecoder.SetEmitFunc(emitFunc)

	// Perform the decoding. The emitFunc will be called for each header field.

	// Perform the decoding by writing the header block to the hpack decoder.
	// The emitFunc will be called for each header field.
	if _, err := d.hpackDecoder.Write(p); err != nil {
		// If an error occurs during Write (e.g., malformed HPACK block),
		// return nil for the header fields and the error.
		return nil, err
	}

	// Close the decoder to finalize processing and catch any end-of-block errors.
	// It's crucial to call Close to ensure all buffered data is processed
	// and any errors at the end of the HPACK stream are caught.
	if err := d.hpackDecoder.Close(); err != nil {
		return nil, err
	}

	// If Write and Close return no error, hfs will contain all decoded header fields.
	return hfs, nil
}

// SetMaxDynamicTableSize updates the maximum dynamic table size the peer can use.
// This should be called when a SETTINGS_HEADER_TABLE_SIZE update is received from the peer.
func (d *Decoder) SetMaxDynamicTableSize(size uint32) {
	d.hpackDecoder.SetMaxDynamicTableSize(size)
	// Also update our stored maxTableSize if we were to re-initialize for some reason,
	// though current hpack.Decoder allows direct update.
	d.maxTableSize = size
}

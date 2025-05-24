package http2

import (
	"bytes"
	"io"

	"golang.org/x/net/http2/hpack"
)

var _ io.Reader // Use io package to avoid "imported and not used" error

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

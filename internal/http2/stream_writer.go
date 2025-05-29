package http2

import (
	"context"
)

// HeaderField represents a single HTTP header field (name-value pair).
// This is used by the StreamWriter interface.
type HeaderField struct {
	Name  string
	Value string
	// Sensitive bool // Future consideration for HPACK integration if needed here
}

// StreamWriter defines the interface for handlers to write HTTP/2 responses.
// It is implemented by an http2.Stream.
type StreamWriter interface {
	// SendHeaders sends response headers.
	// If endStream is true, this also signals the end of the response body.
	SendHeaders(headers []HeaderField, endStream bool) error

	// WriteData sends a chunk of the response body.
	// If endStream is true, this is the final chunk.
	WriteData(p []byte, endStream bool) (n int, err error)

	// WriteTrailers sends trailing headers. This implicitly ends the stream.
	WriteTrailers(trailers []HeaderField) error

	// ID returns the stream's identifier.
	ID() uint32

	// Context returns the stream's context.
	Context() context.Context
}

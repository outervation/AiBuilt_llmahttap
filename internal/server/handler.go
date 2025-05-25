package server

import (
	"context" // Added for ErrorResponseWriterStream.Context()
	"encoding/json"
	"fmt"
	"net/http" // Added for http.Request and http.Header
	"sync"

	"example.com/llmahttap/v2/internal/logger"
	// "example.com/llmahttap/v2/internal/http2" // Cycle broken
)

// HandlerFactory defines the function signature for creating handler instances.
// It now accepts a general *logger.Logger.
type HandlerFactory func(handlerConfig json.RawMessage, lg *logger.Logger) (Handler, error)

// HandlerRegistry manages the registration and retrieval of HandlerFactory instances.
// It provides a centralized and thread-safe way to map HandlerType strings
// (from configuration) to their corresponding factory functions.
type HandlerRegistry struct {
	mu        sync.RWMutex
	factories map[string]HandlerFactory
}

// NewHandlerRegistry creates and returns a new HandlerRegistry instance.
func NewHandlerRegistry() *HandlerRegistry {
	return &HandlerRegistry{
		factories: make(map[string]HandlerFactory),
	}
}

// Register associates a HandlerType string with a factory function.
// This method is thread-safe.
// It returns an error if a HandlerType is registered more than once.
func (r *HandlerRegistry) Register(handlerType string, factory HandlerFactory) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[handlerType]; exists {
		return fmt.Errorf("handler type '%s' already registered", handlerType)
	}
	r.factories[handlerType] = factory
	return nil
}

// GetFactory retrieves a registered HandlerFactory for the given handlerType.
// It returns the factory and a boolean indicating if the factory was found.
// This method is thread-safe.
func (r *HandlerRegistry) GetFactory(handlerType string) (HandlerFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	factory, ok := r.factories[handlerType]
	return factory, ok
}

// CreateHandler creates a new handler instance for the given HandlerType string
// using its registered factory. It passes the handlerConfig (opaque JSON from the
// route definition) and a general *logger.Logger to the factory.
// Returns an error if the HandlerType is not registered or if the factory itself
// encounters an error during handler instantiation (e.g., invalid config).
// This method is thread-safe.
func (r *HandlerRegistry) CreateHandler(handlerType string, handlerConfig json.RawMessage, lg *logger.Logger) (Handler, error) {
	factory, ok := r.GetFactory(handlerType)
	if !ok {
		return nil, fmt.Errorf("no handler factory registered for type '%s'", handlerType)
	}
	if lg == nil {
		// This check ensures that the logger passed down is not nil.
		// While NewRouter should ensure its logger is not nil, this adds a safeguard.
		// For production, a panic might be too harsh; an error return or a fallback default logger could be considered.
		// However, a nil logger here indicates a programming error in setup.
		return nil, fmt.Errorf("logger cannot be nil when creating handler type '%s'", handlerType)
	}
	return factory(handlerConfig, lg)
}

// ClearFactories is a utility method, primarily intended for use in tests,
// to remove all registered handler factories. This allows tests to register
// specific mock handlers or ensure a clean state between test runs.
// This method is thread-safe.
func (r *HandlerRegistry) ClearFactories() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories = make(map[string]HandlerFactory)
}

// ResponseWriter defines the interface for handlers to write HTTP/2 responses.
// This might be implemented by an http2.Stream or similar.
type ResponseWriter interface {
	// SendHeaders sends response headers.
	// If endStream is true, this also signals the end of the response body.
	SendHeaders(headers []HeaderField, endStream bool) error // Using server.HeaderField

	// WriteData sends a chunk of the response body.
	// If endStream is true, this is the final chunk.
	WriteData(p []byte, endStream bool) (n int, err error)

	// WriteTrailers sends trailing headers. This implicitly ends the stream.
	WriteTrailers(trailers []HeaderField) error // Using server.HeaderField
}

// HeaderField represents a single HTTP header field (name-value pair).
// This is defined here to avoid internal/http2 depending on golang.org/x/net/http2/hpack directly
// for interface definitions used by handlers.
type HeaderField struct {
	Name  string
	Value string
	// Sensitive bool // HPACK: true if value should never be indexed
}

// Handler is the interface that processes requests for a given route.
// Feature Spec 1.4.2: "Each handler implementation MUST conform to a server-defined internal interface
// (e.g., a Go interface) that accepts necessary request details (like http.Request or equivalent
// structured data), a means to write the response (like http.ResponseWriter or equivalent stream
// writer), and its specific HandlerConfig (as an opaque structure to be type-asserted by the handler)."
//
// The `req *http.Request` parameter provides a familiar structure for request details.
// The `stream` parameter (of an unexported type from internal/http2 or an interface it implements)
// would provide HTTP/2 specific functionalities, including the ResponseWriter.
// For now, to keep server.Handler independent of http2 internal types, we pass ResponseWriter directly.
// We might need a wrapper or a more abstract Stream interface here eventually.
type Handler interface {
	// ServeHTTP2 processes the request.
	// `resp` is the ResponseWriter for sending the HTTP/2 response.
	// `req` contains the parsed HTTP request details.
	// The handler receives its specific configuration during its instantiation via the factory.
	ServeHTTP2(resp ResponseWriterStream, req *http.Request)
}

// RouterInterface defines the interface for request routing.
// It ensures that the router can be swapped out with different implementations
// if needed, and facilitates testing by allowing mock routers.
type RouterInterface interface {
	// ServeHTTP is responsible for finding a handler for the request and invoking it.
	// If no handler is found, it should generate an appropriate error response (e.g., 404).
	// If a handler is found but fails during its execution, it should handle that error
	// (e.g., by generating a 500 response).
	ServeHTTP(s ResponseWriterStream, req *http.Request)
}

// ParsedRequest could be a simplified version of http.Request or a custom struct
// containing only the necessary fields for routing and handling, extracted from
// HTTP/2 frames.
type ParsedRequest struct {
	Method string
	Path   string // :path pseudo-header (includes query)
	Host   string // :authority pseudo-header
	Header http.Header
	// Potentially other fields like Scheme (:scheme)
}

// ResponseWriterStream combines ResponseWriter with stream-specific info like ID.
// This is what the router's ServeHTTP might expect to interact with the stream.
type ResponseWriterStream interface {
	ResponseWriter
	ID() uint32
	Context() context.Context // For inspecting Accept header, and for ErrorResponseWriterStream compatibility
}

// ErrorResponseWriterStream defines an interface for writing error responses,
// abstracting away the direct dependency on http2.Stream for server.WriteErrorResponse.
type ErrorResponseWriterStream interface {
	ResponseWriter // Includes SendHeaders, WriteData, WriteTrailers
	ID() uint32
	Context() context.Context // For inspecting Accept header, though headers are passed directly now
	// Add other methods if WriteErrorResponse needs more from the stream, e.g., for logging context.
}

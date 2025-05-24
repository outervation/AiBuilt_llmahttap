package server

import (
	"encoding/json"
	"fmt"
	"sync"

	"example.com/llmahttap/v2/internal/http2"
	"example.com/llmahttap/v2/internal/logger"
)

type HandlerFactory func(handlerConfig json.RawMessage, errorLogger *logger.ErrorLogger) (http2.Handler, error)

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
		return fmt.Errorf("handler factory for type '%s' already registered", handlerType)
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
// route definition) and an errorLogger to the factory.
// Returns an error if the HandlerType is not registered or if the factory itself
// encounters an error during handler instantiation (e.g., invalid config).
// This method is thread-safe.
func (r *HandlerRegistry) CreateHandler(handlerType string, handlerConfig json.RawMessage, errorLogger *logger.ErrorLogger) (http2.Handler, error) {
	r.mu.RLock()
	factory, exists := r.factories[handlerType]
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("no handler factory registered for type '%s'", handlerType)
	}
	return factory(handlerConfig, errorLogger)
}

// ClearFactories is a utility method, primarily intended for use in tests,
// to remove all registered handler factories. This allows tests to register
// specific mock handlers or ensure a clean state between test runs.
// This method is thread-safe.
func (r *HandlerRegistry) ClearFactories() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Create a new map to effectively clear the existing one.
	r.factories = make(map[string]HandlerFactory)
}

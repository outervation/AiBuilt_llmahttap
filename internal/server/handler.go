package server

import (
	"encoding/json"
	"fmt"
	"sync"

	"example.com/llmahttap/v2/internal/http2"
	"example.com/llmahttap/v2/internal/logger"
)

// HandlerFactory creates an instance of an http2.Handler.
// It receives the specific configuration for the handler (as raw JSON)
// and a logger instance. The logger instance is specifically an ErrorLogger
// as handlers typically log errors or significant operational messages,
// not access logs (which are handled centrally).

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

// GetFactory retrieves a registered HandlerFactory for the given handlerType.
// It returns the factory and a boolean indicating if the factory was found.
// This method is thread-safe.
func (r *HandlerRegistry) GetFactory(handlerType string) (HandlerFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	factory, ok := r.factories[handlerType]
	return factory, ok
}

type HandlerFactory func(handlerConfig json.RawMessage, errorLogger *logger.ErrorLogger) (http2.Handler, error)

var (
	// handlerFactories stores the mapping from HandlerType strings (from config)
	// to their respective factory functions.
	handlerFactories = make(map[string]HandlerFactory)
	// factoriesMutex protects access to the handlerFactories map.
	factoriesMutex = &sync.RWMutex{}
)

// RegisterHandlerFactory associates a HandlerType string with a factory function.
// This function should be called during initialization (e.g., in init() blocks
// of packages that define specific handler implementations).
// It panics if a HandlerType is registered more than once, as this indicates
// a configuration or programming error.
func RegisterHandlerFactory(handlerType string, factory HandlerFactory) {
	factoriesMutex.Lock()
	defer factoriesMutex.Unlock()

	if _, exists := handlerFactories[handlerType]; exists {
		panic(fmt.Sprintf("handler factory for type '%s' already registered", handlerType))
	}
	handlerFactories[handlerType] = factory
}

// GetHandlerInstance creates a new handler instance for the given HandlerType string
// using its registered factory. It passes the handlerConfig (opaque JSON from the
// route definition) and an errorLogger to the factory.
// Returns an error if the HandlerType is not registered or if the factory itself
// encounters an error during handler instantiation (e.g., invalid config).
func GetHandlerInstance(handlerType string, handlerConfig json.RawMessage, errorLogger *logger.ErrorLogger) (http2.Handler, error) {
	factoriesMutex.RLock()
	factory, exists := handlerFactories[handlerType]
	factoriesMutex.RUnlock()

	if !exists {
		return nil, fmt.Errorf("no handler factory registered for type '%s'", handlerType)
	}
	return factory(handlerConfig, errorLogger)
}

// ClearHandlerFactories is a utility function, primarily intended for use in tests,
// to remove all registered handler factories. This allows tests to register
// specific mock handlers or ensure a clean state between test runs.
func ClearHandlerFactories() {
	factoriesMutex.Lock()
	defer factoriesMutex.Unlock()
	// Create a new map to effectively clear the existing one.
	handlerFactories = make(map[string]HandlerFactory)
}

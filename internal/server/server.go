package server

import (
	"fmt"
	"net"
	"os"
	"sync"
	// "crypto/tls"
	// "os/signal"
	// "strings"
	// "syscall"
	// "time"

	"example.com/llmahttap/v2/internal/config"
	// "example.com/llmahttap/v2/internal/http2" // Temporarily commented out to break import cycle
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/util"
)

// Server manages the HTTP/2 server lifecycle, including listening sockets,
// connection handling, configuration reloading, and graceful shutdown.
type Server struct {
	cfg             *config.Config
	log             *logger.Logger
	router          RouterInterface  // Type defined in internal/server/handler.go
	handlerRegistry *HandlerRegistry // Type defined in internal/server/handler.go

	mu          sync.RWMutex
	listeners   []net.Listener
	listenerFDs []uintptr
	// activeConns     map[*http2.Connection]struct{} // TEMPORARY: Commented out to break import cycle. TODO: Reinstate with *http2.Connection once cycle is resolved.
	activeConns    map[interface{}]struct{} // TEMPORARY PLACEHOLDER to allow compilation.
	configFilePath string

	// Lifecycle and shutdown management
	shutdownChan  chan struct{}
	doneChan      chan struct{}
	reloadChan    chan os.Signal
	stopAccepting chan struct{}

	// For hot reload/binary upgrade
	isChild      bool
	childProcess *os.Process
}

// NewServer creates a new Server instance.
func NewServer(cfg *config.Config, lg *logger.Logger, router RouterInterface, originalCfgPath string, registry *HandlerRegistry) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if lg == nil {
		return nil, fmt.Errorf("logger cannot be nil")
	}
	if router == nil {
		return nil, fmt.Errorf("router cannot be nil")
	}
	if registry == nil {
		return nil, fmt.Errorf("handler registry cannot be nil")
	}

	s := &Server{
		cfg:             cfg,
		log:             lg,
		router:          router,
		handlerRegistry: registry,
		// activeConns:     make(map[*http2.Connection]struct{}), // TEMPORARY
		activeConns:    make(map[interface{}]struct{}), // TEMPORARY
		configFilePath: originalCfgPath,
		shutdownChan:   make(chan struct{}),
		doneChan:       make(chan struct{}),
		reloadChan:     make(chan os.Signal, 1),
		stopAccepting:  make(chan struct{}),
	}

	inheritedFDs, err := util.ParseInheritedListenerFDs(util.ListenFdsEnvKey)
	if err != nil {
		if os.Getenv(util.ListenFdsEnvKey) != "" {
			return nil, fmt.Errorf("error parsing inherited listener FDs from %s: %w", util.ListenFdsEnvKey, err)
		}
	}

	if len(inheritedFDs) > 0 {
		s.isChild = true
		s.listenerFDs = inheritedFDs
	}

	return s, nil
}

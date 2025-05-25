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

// initializeListeners sets up the server's network listeners.
// If the server is a child process (s.isChild is true), it uses inherited file descriptors
// from s.listenerFDs (parsed from LISTEN_FDS env var by NewServer).
// Otherwise, it creates new listeners based on s.cfg.Server.Address.
// All listeners will have FD_CLOEXEC cleared.
// The method populates s.listeners and s.listenerFDs.
func (s *Server) initializeListeners() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isChild {
		if len(s.listenerFDs) == 0 {
			return fmt.Errorf("server marked as child (isChild=true), but no inherited listener FDs found in s.listenerFDs")
		}
		s.log.Info("Initializing server with inherited listener FDs", logger.LogFields{"fds": s.listenerFDs})

		listeners := make([]net.Listener, len(s.listenerFDs))
		for i, fd := range s.listenerFDs {
			listener, err := util.NewListenerFromFD(fd)
			if err != nil {
				// Clean up already created listeners in this attempt
				for j := 0; j < i; j++ {
					if listeners[j] != nil {
						listeners[j].Close()
					}
				}
				return fmt.Errorf("failed to create listener from inherited FD %d: %w", fd, err)
			}
			// util.NewListenerFromFD ensures FD_CLOEXEC is cleared.
			listeners[i] = listener
			s.log.Info("Successfully created listener from inherited FD", logger.LogFields{"fd": fd, "localAddr": listener.Addr().String()})
		}
		s.listeners = listeners
		// s.listenerFDs was already populated by NewServer for a child process.
	} else {
		s.log.Info("Initializing server with new listeners (not inherited)", nil)

		var listenAddress string
		if s.cfg.Server == nil {
			return fmt.Errorf("server configuration section (server) is missing, cannot determine listen address")
		}
		if s.cfg.Server.Address == nil {
			return fmt.Errorf("server listen address (server.address) is not configured (is nil)")
		}
		if *s.cfg.Server.Address == "" {
			return fmt.Errorf("server listen address (server.address) is configured but is an empty string")
		}
		listenAddress = *s.cfg.Server.Address

		listener, fd, err := util.CreateListenerAndGetFD(listenAddress)
		if err != nil {
			return fmt.Errorf("failed to create new listener on %s: %w", listenAddress, err)
		}
		// util.CreateListenerAndGetFD ensures FD_CLOEXEC is cleared.
		s.listeners = []net.Listener{listener}
		s.listenerFDs = []uintptr{fd}
		s.log.Info("Successfully created new listener", logger.LogFields{"address": listenAddress, "fd": fd, "localAddr": listener.Addr().String()})
	}

	if len(s.listeners) == 0 {
		return fmt.Errorf("no listeners were initialized for the server")
	}

	return nil
}

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http" // For http.Request in dispatcherFunc
	"os"
	"sync"
	// "crypto/tls"
	// "os/signal"
	// "strings"
	// "syscall"
	"time"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/http2"
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

	activeConns    map[*http2.Connection]struct{}
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

		activeConns:    make(map[*http2.Connection]struct{}),
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

// StartAccepting begins listening for and accepting new connections.
// This method should be called after listeners are initialized.
func (s *Server) StartAccepting() error {
	s.mu.RLock()
	if len(s.listeners) == 0 {
		s.mu.RUnlock()
		return fmt.Errorf("no listeners initialized to start accepting on")
	}
	listeners := make([]net.Listener, len(s.listeners))
	copy(listeners, s.listeners)
	s.mu.RUnlock()

	var wg sync.WaitGroup
	for _, l := range listeners {
		wg.Add(1)
		go func(listener net.Listener) {
			defer wg.Done()
			s.acceptLoop(listener)
		}(l)
	}

	// If this is a child process that inherited FDs, signal readiness.
	if s.isChild {
		s.log.Info("Child process signaling readiness", nil)
		readinessFD, found, err := util.GetInheritedReadinessPipeFD()
		if err != nil {
			s.log.Error("Child process: Error getting readiness pipe FD", logger.LogFields{"error": err.Error()})
			// This is a problem for hot reload, but server might still function.
			// Depending on desired robustness, might os.Exit(1) or just log.
		} else if found {
			if err := util.SignalChildReadyByClosingFD(readinessFD); err != nil {
				s.log.Error("Child process: Error signaling readiness by closing pipe FD", logger.LogFields{"fd": readinessFD, "error": err.Error()})
			} else {
				s.log.Info("Child process: Successfully signaled readiness by closing pipe FD", logger.LogFields{"fd": readinessFD})
			}
		} else {
			s.log.Warn("Child process: No readiness pipe FD found in environment, cannot signal parent.", nil)
		}
	}

	// wg.Wait() // We don't wait here as acceptLoops run indefinitely.
	// They are terminated by closing listeners or s.stopAccepting.
	return nil
}

// acceptLoop continuously accepts new connections on a given listener
// and spawns goroutines to handle them.
func (s *Server) acceptLoop(l net.Listener) {
	s.log.Info("Starting accept loop", logger.LogFields{"address": l.Addr().String()})
	defer s.log.Info("Exiting accept loop", logger.LogFields{"address": l.Addr().String()})

	var tempDelay time.Duration // how long to sleep on accept failure
	for {
		select {
		case <-s.stopAccepting:
			s.log.Info("Accept loop received stop signal, ceasing to accept new connections.", logger.LogFields{"address": l.Addr().String()})
			return
		default:
		}

		conn, err := l.Accept()
		if err != nil {
			select {
			case <-s.stopAccepting: // Check again, listener might have been closed due_to stop signal
				s.log.Info("Accept loop: listener closed after stop signal.", logger.LogFields{"address": l.Addr().String()})
				return
			default:
			}

			// Check if the error is due to the listener being closed.
			if ne, ok := err.(net.Error); ok && ne.Timeout() { // Check for timeout, though Accept usually doesn't timeout unless SetDeadline used
				s.log.Warn("Accept error: timeout (should not happen with blocking Accept unless deadline set)", logger.LogFields{"address": l.Addr().String(), "error": err.Error()})
				continue // Retry
			}
			if errors.Is(err, net.ErrClosed) {
				s.log.Info("Accept loop: listener is closed, exiting.", logger.LogFields{"address": l.Addr().String()})
				return
			}

			// Handle temporary errors
			if tempDelay == 0 {
				tempDelay = 5 * time.Millisecond
			} else {
				tempDelay *= 2
			}
			if max := 1 * time.Second; tempDelay > max {
				tempDelay = max
			}
			s.log.Error("Accept error; retrying", logger.LogFields{"address": l.Addr().String(), "error": err.Error(), "delay": tempDelay.String()})
			time.Sleep(tempDelay)
			continue
		}
		tempDelay = 0 // Reset delay on successful accept

		// Check stopAccepting again *after* a successful accept, before spawning handler.
		// This is a small window, but ensures we don't start new handlers if stop was just signaled.
		select {
		case <-s.stopAccepting:
			s.log.Info("Accept loop received stop signal just after accepting a connection; closing it.", logger.LogFields{"address": l.Addr().String(), "remote": conn.RemoteAddr().String()})
			conn.Close() // Close the newly accepted connection
			return       // And exit the loop
		default:
		}

		go s.handleTCPConnection(conn)
	}
}

// handleTCPConnection sets up an HTTP/2 connection for an accepted TCP connection.
func (s *Server) handleTCPConnection(tcpConn net.Conn) {
	remoteAddr := tcpConn.RemoteAddr().String()
	s.log.Debug("Accepted new TCP connection", logger.LogFields{"remote_addr": remoteAddr})

	var srvSettingsOverride map[http2.SettingID]uint32
	if s.cfg != nil && s.cfg.Server != nil {
		// Example: srvSettingsOverride = s.cfg.Server.Http2Settings (if defined)
		// For now, this remains nil, relying on defaults in http2.NewConnection.
	}

	// Create the dispatcher function by wrapping s.router.ServeHTTP
	dispatcherFunc := func(stream http2.StreamWriter, req *http.Request) {
		// We need to adapt http2.StreamWriter to server.ResponseWriterStream for the router.
		// This is a bit of a shim. Ideally, the router would accept http2.StreamWriter directly,
		// or http2.Stream would fully implement server.ResponseWriterStream.
		// For now, we assume http2.StreamWriter also has ID() and Context() if router needs them.
		// If http2.StreamWriter is indeed an *http2.Stream, this cast would work if *http2.Stream implements server.ResponseWriterStream.

		// Check if stream implements server.ResponseWriterStream
		// This is a temporary check. The types should align eventually.
		serverStream, ok := stream.(ResponseWriterStream)
		if !ok {
			s.log.Error("Dispatcher: stream does not implement server.ResponseWriterStream", logger.LogFields{"streamID": stream.ID(), "type": fmt.Sprintf("%T", stream)})
			// Handle error: perhaps send 500 on the stream if possible, or just log and close.
			// This indicates a type mismatch that needs fixing.
			// For now, let's try to send a basic 500 if headers not sent.
			// This is simplified.
			_ = stream.SendHeaders([]http2.HeaderField{{Name: ":status", Value: "500"}}, true)
			return
		}
		s.router.ServeHTTP(serverStream, req)
	}

	h2conn := http2.NewConnection(tcpConn, s.log, false /*isClientSide*/, srvSettingsOverride, dispatcherFunc)

	s.mu.Lock()
	s.activeConns[h2conn] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.activeConns, h2conn)
		s.mu.Unlock()
		// tcpConn.Close() // http2.Connection.Close() should handle closing the underlying net.Conn
		s.log.Debug("Closed HTTP/2 connection and underlying TCP connection", logger.LogFields{"remote_addr": remoteAddr})
	}()

	// Perform the server-side HTTP/2 handshake.
	if err := h2conn.ServerHandshake(); err != nil {
		s.log.Error("HTTP/2 server handshake failed", logger.LogFields{"remote_addr": remoteAddr, "error": err.Error()})
		// Close will also remove from activeConns in its defer, but good to ensure it's called
		// with the error from handshake.
		h2conn.Close(err) // This will trigger the defer in this function to remove from activeConns
		return
	}

	// The h2conn.Serve() method is blocking and handles the frame loop.
	// It will return when the connection is closed or an unrecoverable error occurs.
	// The h2conn.Close() method is idempotent and handles cleanup.
	// The context passed to Serve can be used for cancellation signals if needed by Serve's internals.
	err := h2conn.Serve(context.Background()) // Or pass a more specific context like s.ctx
	if err != nil {
		// Check for common "expected" errors that don't need to be logged as "Error" level.
		isExpectedError := errors.Is(err, net.ErrClosed) || // Connection closed by our side
			errors.Is(err, io.EOF) || // Peer closed connection cleanly
			(err == nil) // Graceful GOAWAY scenario

		if connErr, ok := err.(*http2.ConnectionError); ok {
			if connErr.Code == http2.ErrCodeNoError || connErr.Code == http2.ErrCodeCancel {
				isExpectedError = true
			}
		}

		if !isExpectedError {
			s.log.Error("HTTP/2 connection Serve() returned an error", logger.LogFields{"remote_addr": remoteAddr, "error": err.Error()})
		} else {
			s.log.Debug("HTTP/2 connection Serve() exited", logger.LogFields{"remote_addr": remoteAddr, "reason": err})
		}
	}
	// Ensure connection is fully closed; Serve might return before Close is fully effective or if it wasn't called from within.
	h2conn.Close(err) // Pass the error from Serve to Close.
}

// dispatchRequest is a helper method for the dispatcherFunc.
// It adapts the call to the server's router.
// This method makes server.ServeHTTP compatible with http2.RequestDispatcherFunc
func (s *Server) dispatchRequest(stream http2.StreamWriter, req *http.Request) {
	// server.RouterInterface.ServeHTTP expects a server.ResponseWriterStream.
	// http2.StreamWriter is what http2.Connection provides.
	// We need to ensure that the 'stream' object passed here can be treated as
	// a server.ResponseWriterStream. This usually means that *http2.Stream
	// (which implements http2.StreamWriter) also implements server.ResponseWriterStream.

	// Type assertion:
	responseStream, ok := stream.(ResponseWriterStream)
	if !ok {
		// This is a critical type mismatch. Log an error and potentially send a 500.
		s.log.Error("dispatchRequest: provided stream does not implement server.ResponseWriterStream",
			logger.LogFields{"stream_type": fmt.Sprintf("%T", stream), "stream_id_attempt": stream.ID()}) // Assuming ID() exists for logging

		// Attempt to send a 500 error if possible. This is a fallback.
		// This assumes stream has SendHeaders.
		_ = stream.SendHeaders([]http2.HeaderField{
			{Name: ":status", Value: "500"},
			{Name: "content-type", Value: "text/plain; charset=utf-8"},
		}, false)
		_, _ = stream.WriteData([]byte("Internal Server Error: type mismatch in stream handling."), true)
		return
	}

	s.router.ServeHTTP(responseStream, req)
}

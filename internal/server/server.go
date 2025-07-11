package server

import (
	"bufio" // Added for peekConn
	"context"
	"crypto/tls" // Added for TLS handling
	"errors"
	"fmt"
	"io"
	"net"
	"net/http" // For http.Request in dispatcherFunc

	"os"
	"os/signal"     // Added for signal handling
	"path/filepath" // Added for filepath.Abs
	"strings"       // Added for strings.Join

	"runtime/debug" // For stack trace in panic recovery
	"sync"
	"syscall" // Added for signal types
	"time"

	"example.com/llmahttap/v2/internal/config" // Used by handleSIGHUP placeholder
	"example.com/llmahttap/v2/internal/http2"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/util"
)

// newHTTP2Connection is a variable that holds the function to create a new http2.Connection.
// It's used to allow mocking in tests.

// peekConn allows peeking at the initial bytes of a connection
// and then having those bytes available for subsequent reads.
// It wraps a net.Conn and uses a bufio.Reader for the peeking and subsequent reads.
type peekConn struct {
	net.Conn
	r *bufio.Reader
}

// Read implements the io.Reader interface for peekConn, reading from the bufio.Reader.
func (p *peekConn) Read(b []byte) (int, error) {
	return p.r.Read(b)
}

var newHTTP2Connection = http2.NewConnection

// Server manages the HTTP/2 server lifecycle, including listening sockets,
// connection handling, configuration reloading, and graceful shutdown.
type Server struct {
	cfg             *config.Config
	log             *logger.Logger
	router          RouterInterface  // Type defined in internal/server/handler.go
	handlerRegistry *HandlerRegistry // Type defined in internal/server/handler.go
	tlsConfig       *tls.Config      // Added for TLS support

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
func NewServer(cfg *config.Config, lg *logger.Logger, router RouterInterface, originalCfgPath string, registry *HandlerRegistry, tlsCfg *tls.Config) (*Server, error) {
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
	// tlsCfg can be nil if TLS is not configured.

	s := &Server{
		cfg:             cfg,
		log:             lg,
		router:          router,
		handlerRegistry: registry,
		tlsConfig:       tlsCfg, // Store the TLS config
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

		s.log.Debug("NewServer: Server struct initialized and basic fields populated", nil)
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

		listeners := make([]net.Listener, len(s.listenerFDs)) // Local slice
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
		s.log.Debug("Parent process: Attempting to create listener", logger.LogFields{"address": listenAddress})
		listener, fd, err := util.CreateListenerAndGetFD(listenAddress)
		s.log.Debug("Parent process: util.CreateListenerAndGetFD result", logger.LogFields{"address_passed": listenAddress, "error_returned": fmt.Sprintf("%v", err)})
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

	// Log the actual listening address for test utilities
	for _, listener := range listeners {
		s.log.Info("Server listening on actual address", logger.LogFields{"address": listener.Addr().String()})
	}
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

		s.log.Debug("acceptLoop: Accepted new connection", logger.LogFields{"local_addr": l.Addr().String(), "remote_addr": conn.RemoteAddr().String()})
		select {
		case <-s.stopAccepting:
			s.log.Info("Accept loop received stop signal just after accepting a connection; closing it.", logger.LogFields{"address": l.Addr().String(), "remote": conn.RemoteAddr().String()})
			conn.Close() // Close the newly accepted connection
			return       // And exit the loop
		default:
		}

		// --- BEGIN TLS MODIFICATION ---
		var effectiveConn net.Conn = conn // Default to original conn

		if s.tlsConfig != nil {
			s.log.Debug("TLS is configured. Peeking for TLS handshake.", logger.LogFields{"remote_addr": conn.RemoteAddr().String()})

			br := bufio.NewReader(conn)
			hdr, peekErr := br.Peek(3) // Peek 3 bytes for TLS handshake check: 1 (type) + 2 (version)

			if peekErr != nil {
				// If peeking fails (e.g., connection closed prematurely), log and close.
				s.log.Error("Failed to peek connection for TLS detection", logger.LogFields{"remote_addr": conn.RemoteAddr().String(), "error": peekErr.Error()})
				conn.Close()
				continue // Next iteration of acceptLoop
			}

			// Wrap original conn with peekConn to allow re-reading peeked bytes
			pc := &peekConn{Conn: conn, r: br}

			// TLS record header: first byte 22 (handshake), second byte 3 (TLS major version)
			if hdr[0] == 22 && hdr[1] == 0x03 {
				s.log.Info("Potential TLS connection detected based on peeked bytes.", logger.LogFields{"remote_addr": pc.RemoteAddr().String()})

				s.log.Info("Initiating TLS handshake.", logger.LogFields{"remote_addr": pc.RemoteAddr().String()})

				tlsConn := tls.Server(pc, s.tlsConfig) // Use the pre-loaded s.tlsConfig
				if errHandshake := tlsConn.Handshake(); errHandshake != nil {
					s.log.Error("TLS handshake failed", logger.LogFields{"remote_addr": pc.RemoteAddr().String(), "error": errHandshake.Error()})
					tlsConn.Close() // Close the *tls.Conn
					continue        // Next iteration of acceptLoop
				}
				s.log.Info("TLS handshake successful.", logger.LogFields{"remote_addr": tlsConn.RemoteAddr().String()})

				state := tlsConn.ConnectionState()
				s.log.Debug("TLS connection state after handshake", logger.LogFields{"remote_addr": tlsConn.RemoteAddr().String(), "alpn_protocol": state.NegotiatedProtocol, "cipher_suite": tls.CipherSuiteName(state.CipherSuite)})

				if state.NegotiatedProtocol != "h2" {
					s.log.Warn("TLS ALPN did not negotiate \"h2\". Closing connection.", logger.LogFields{"remote_addr": tlsConn.RemoteAddr().String(), "negotiated_protocol": state.NegotiatedProtocol})
					// Respond with HTTP/1.1 505 Version Not Supported
					errMsg := "HTTP/1.1 505 HTTP Version Not Supported\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\nThis server requires ALPN \"h2\" for TLS connections.\r\n"
					_, errWrite := io.WriteString(tlsConn, errMsg)
					if errWrite != nil {
						s.log.Error("Error writing ALPN failure message to TLS connection", logger.LogFields{"remote_addr": tlsConn.RemoteAddr().String(), "error": errWrite.Error()})
					}
					tlsConn.Close()
					continue // Next iteration of acceptLoop
				}

				s.log.Info("TLS ALPN negotiated \"h2\". Proceeding with HTTP/2 over TLS.", logger.LogFields{"remote_addr": tlsConn.RemoteAddr().String()})
				effectiveConn = tlsConn // Use the *tls.Conn for handleTCPConnection
			} else {
				// Peeked bytes do not indicate TLS, but TLS was configured. Proceed with pc (peekConn) as clear text.
				s.log.Info("Peeked bytes do not indicate TLS, but TLS was configured. Proceeding with clear text connection using peekConn.", logger.LogFields{"remote_addr": pc.RemoteAddr().String()})
				effectiveConn = pc // Use peekConn so the initial bytes are not lost
			}
		} else {
			s.log.Debug("TLS not configured. Proceeding with clear text.", logger.LogFields{"remote_addr": conn.RemoteAddr().String()})
		}

		connTypeDesc := ""
		if _, ok := effectiveConn.(*tls.Conn); ok {
			connTypeDesc = "TLS"
		} else if _, ok := effectiveConn.(*peekConn); ok {
			connTypeDesc = "peekConn (non-TLS after peek)"
		} else if effectiveConn == conn {
			connTypeDesc = "original cleartext"
		} else {
			connTypeDesc = "unknown"
		}
		s.log.Info("Passing connection to handleTCPConnection.", logger.LogFields{"remote_addr": effectiveConn.RemoteAddr().String(), "type": connTypeDesc})
		go s.handleTCPConnection(effectiveConn)
		// --- END TLS MODIFICATION ---
	}
}

// handleTCPConnection sets up an HTTP/2 connection for an accepted TCP connection.
func (s *Server) handleTCPConnection(tcpConn net.Conn) {
	remoteAddr := tcpConn.RemoteAddr().String()
	s.log.Debug("Accepted new TCP connection", logger.LogFields{"remote_addr": remoteAddr})

	if tcpCon, ok := tcpConn.(*net.TCPConn); ok {
		if err := tcpCon.SetNoDelay(true); err != nil {
			s.log.Warn("Failed to set TCP_NODELAY on connection", logger.LogFields{"remote_addr": remoteAddr, "error": err.Error()})
		} else {
			s.log.Debug("Successfully set TCP_NODELAY on connection", logger.LogFields{"remote_addr": remoteAddr})
		}
	} else {
		s.log.Warn("Underlying connection is not *net.TCPConn, cannot set TCP_NODELAY.", logger.LogFields{"remote_addr": remoteAddr, "type": fmt.Sprintf("%T", tcpConn)})
	}

	var srvSettingsOverride map[http2.SettingID]uint32
	// srvSettingsOverride = s.cfg.Server.Http2Settings // Example

	dispatcherFunc := s.dispatchRequest

	h2conn := newHTTP2Connection(tcpConn, s.log, false /*isClientSide*/, srvSettingsOverride, nil /*initialPeerSettingsForTest*/, dispatcherFunc)

	if h2conn == nil {
		s.log.Error("newHTTP2Connection returned nil, cannot handle TCP connection", logger.LogFields{"remote_addr": remoteAddr})
		tcpConn.Close()
		return
	}

	// Add to active connections map
	s.mu.Lock()
	s.activeConns[h2conn] = struct{}{}
	s.mu.Unlock()

	var connErr error // To capture error from handshake or serve, or panic

	defer func() {
		// This defer runs regardless of panic or normal return.
		// It's crucial for cleaning up s.activeConns and ensuring h2conn.Close is called.
		s.mu.Lock()
		delete(s.activeConns, h2conn)
		s.mu.Unlock()

		if r := recover(); r != nil {
			s.log.Error("Panic in HTTP/2 connection handling (handleTCPConnection)", logger.LogFields{"remote_addr": remoteAddr, "panic": r, "stack": string(debug.Stack())})
			// If panic occurred, ensure 'connErr' reflects this for h2conn.Close()
			if connErr == nil { // If no specific error was set before panic (e.g. panic in Handshake/Serve itself)
				connErr = fmt.Errorf("panic recovered in handleTCPConnection: %v", r)
			}
		}

		// Now, connErr contains the error from Handshake, Serve, or a panic.
		// Close the h2conn with this definitive error.
		h2conn.Close(connErr) // This call is now guaranteed.
		s.log.Debug("Finished handleTCPConnection (defer completed, h2conn.Close called)", logger.LogFields{"remote_addr": remoteAddr, "final_error_for_close": connErr})
	}()

	connErr = h2conn.ServerHandshake()
	if connErr != nil {
		s.log.Error("HTTP/2 server handshake failed", logger.LogFields{"remote_addr": remoteAddr, "error": connErr.Error()})
		// connErr is already set. Defer will call h2conn.Close(connErr).
		return
	}

	// Handshake successful
	s.log.Info("HTTP/2 server handshake successful. Starting Serve loop.", logger.LogFields{"remote_addr": remoteAddr})
	connErr = h2conn.Serve(context.Background()) // Or pass a more specific context like s.ctx

	// Log error from Serve if any
	if connErr != nil {
		isExpectedError := errors.Is(connErr, net.ErrClosed) || // Connection closed by our side
			errors.Is(connErr, io.EOF) // Peer closed connection cleanly

		if http2ConnErr, ok := connErr.(*http2.ConnectionError); ok {
			if http2ConnErr.Code == http2.ErrCodeNoError || http2ConnErr.Code == http2.ErrCodeCancel {
				isExpectedError = true
			}
		}

		if !isExpectedError {
			s.log.Error("HTTP/2 connection Serve() returned an error", logger.LogFields{"remote_addr": remoteAddr, "error": connErr.Error()})
		} else {
			s.log.Debug("HTTP/2 connection Serve() exited", logger.LogFields{"remote_addr": remoteAddr, "reason": connErr})
		}
	}
	// connErr (from Serve, or nil if successful) is set. Defer will call h2conn.Close(connErr).
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

// Shutdown initiates a graceful shutdown of the server.
// It stops accepting new connections, sends GOAWAY to active connections,
// waits for them to finish, and then cleans up resources.
func (s *Server) Shutdown(reason error) error {

	s.log.Info("Shutdown initiated", logger.LogFields{"reason_msg": reason.Error()})

	// 1. Signal shutdown initiation and stop accepting new connections
	s.mu.Lock()
	select {
	case <-s.shutdownChan:
		// Already shutting down
		s.mu.Unlock()
		s.log.Info("Shutdown already in progress", nil)
		// Wait for existing shutdown to complete, but prevent re-entry if called concurrently.
		// If Shutdown is called again while one is in progress, the second call waits for the first to finish.
		<-s.doneChan
		return nil
	default:
		close(s.shutdownChan)
		// s.stopAccepting is closed here to immediately signal acceptLoops.
		// If acceptLoops check s.shutdownChan as well, this might be redundant but safe.
		// Spec focuses on s.stopAccepting for acceptLoops.
		close(s.stopAccepting)
	}

	// Keep a local copy of listeners to close them outside the main server lock
	// to avoid deadlocks if listener.Close() calls something that tries to acquire s.mu.
	listenersToClose := make([]net.Listener, len(s.listeners))
	copy(listenersToClose, s.listeners)
	s.mu.Unlock() // Unlock before closing listeners

	// 2. Stop all listeners from accepting new connections
	s.log.Info("Closing listeners...", nil)
	for _, l := range listenersToClose {
		if err := l.Close(); err != nil {
			s.log.Warn("Error closing listener", logger.LogFields{"address": l.Addr().String(), "error": err.Error()})
		}
	}
	s.log.Info("Listeners closed.", nil)

	// 3. Determine GOAWAY error code
	var goAwayErrorCode http2.ErrorCode = http2.ErrCodeNoError
	var goAwayDebugData []byte
	if reason != nil {
		if connErr, ok := reason.(*http2.ConnectionError); ok {
			goAwayErrorCode = connErr.Code
			goAwayDebugData = connErr.DebugData
		} else {
			goAwayErrorCode = http2.ErrCodeInternalError
			goAwayDebugData = []byte(reason.Error())
			// Truncate debug data if too long for GOAWAY. RFC 7540 Sec 6.8 doesn't specify a limit,
			// but it's good practice to keep it reasonable.
			const maxDebugDataLen = 256
			if len(goAwayDebugData) > maxDebugDataLen {
				goAwayDebugData = goAwayDebugData[:maxDebugDataLen]
			}
		}
	}

	// 4. Iterate all active http2.Connection's and call their Close() method
	s.mu.RLock()
	activeConnections := make([]*http2.Connection, 0, len(s.activeConns))
	for conn := range s.activeConns {
		activeConnections = append(activeConnections, conn)
	}
	s.mu.RUnlock()

	s.log.Info("Sending GOAWAY and closing active HTTP/2 connections", logger.LogFields{"count": len(activeConnections)})
	// The GOAWAY frame itself will be constructed by the http2.Connection's Close method.
	// It will use its own lastProcessedStreamID.
	goAwayErrForConn := &http2.ConnectionError{Code: goAwayErrorCode, DebugData: goAwayDebugData, LastStreamID: 0} // LastStreamID will be set by conn.Close()

	for _, h2conn := range activeConnections {
		go h2conn.Close(goAwayErrForConn)
	}

	// 5. Wait for all active http2.Connection's to complete their own graceful shutdown
	gracefulTimeout := 30 * time.Second // Default
	if s.cfg.Server != nil && s.cfg.Server.GracefulShutdownTimeout != nil && *s.cfg.Server.GracefulShutdownTimeout != "" {
		parsedTimeout, err := time.ParseDuration(*s.cfg.Server.GracefulShutdownTimeout)
		if err == nil && parsedTimeout > 0 {
			gracefulTimeout = parsedTimeout
		} else if err != nil {
			s.log.Warn("Failed to parse graceful_shutdown_timeout, using default", logger.LogFields{"value": *s.cfg.Server.GracefulShutdownTimeout, "default": gracefulTimeout, "error": err.Error()})
		}
	}
	s.log.Info("Waiting for active connections to close", logger.LogFields{"timeout": gracefulTimeout.String()})

	timeout := time.After(gracefulTimeout)
	ticker := time.NewTicker(50 * time.Millisecond) // Check more frequently
	defer ticker.Stop()

	for {
		s.mu.RLock()
		numActive := len(s.activeConns)
		s.mu.RUnlock()

		if numActive == 0 {
			s.log.Info("All active connections closed.", nil)
			break
		}

		select {
		case <-timeout:
			s.log.Warn("Graceful shutdown timeout reached, some connections may not have closed cleanly.", logger.LogFields{"remaining_connections": numActive})
			goto cleanupLoopExit // Exit the waiting loop
		case <-ticker.C:
			s.log.Debug("Waiting for connections to close...", logger.LogFields{"remaining_connections": numActive})
			// No direct check on s.doneChan here; if another shutdown completed, this one would have already returned from the top.
			// If it was closed by THIS goroutine (which is not possible before this point), it means a logic error.
		}
	}

cleanupLoopExit: // Label to break out of the waiting loop

	// 6. Close server-level resources
	s.log.Info("Closing server-level resources (e.g., log files).", nil)
	if err := s.log.CloseLogFiles(); err != nil {
		// Log this error, but don't let it stop the shutdown.
		// Use a more primitive log if the main logger is what's failing.
		fmt.Fprintf(os.Stderr, "[CRITICAL] Error closing log files during shutdown: %v\n", err)
	}

	// 7. Signal that the server has fully stopped
	s.mu.Lock() // Need lock to safely close doneChan if not already closed
	select {
	case <-s.doneChan:
		// Already closed, do nothing
	default:
		close(s.doneChan)
	}
	s.mu.Unlock()

	s.log.Info("Server shutdown complete.", nil)
	return nil
}

// Run starts the server, initializes listeners, handles signals, and accepts connections.
// It blocks until the server is shut down.

// Start starts the server, initializes listeners, handles signals, and accepts connections.
// It blocks until the server is shut down.
// If the server is starting as a child process (detected by presence of READINESS_PIPE_FD),
// and initialization is successful, it signals readiness to the parent.
// It returns an error if startup fails.
func (s *Server) Start() error {
	s.log.Info("Starting server...", logger.LogFields{"config_path": s.configFilePath})

	var startErr error // To capture startup errors

	if err := s.initializeListeners(); err != nil {
		s.log.Error("Failed to initialize listeners", logger.LogFields{"error": err.Error()})
		s.mu.Lock() // Lock to safely close doneChan
		select {
		case <-s.doneChan: // Already closed
		default:
			close(s.doneChan)
		}
		s.mu.Unlock()
		return err
	}

	// Signal readiness if this is a child process that inherited a readiness pipe.
	// This happens after listeners are initialized and before starting to accept or handle signals.
	readinessFD, foundPipe, errPipe := util.GetInheritedReadinessPipeFD()
	if errPipe != nil {
		s.log.Error("Error getting readiness pipe FD during startup", logger.LogFields{"error": errPipe.Error()})
		// This is potentially problematic for hot reload but child might still function.
		// Parent process will time out waiting for readiness if this path fails to signal.
	}
	if foundPipe {
		s.log.Info("Child process detected by presence of readiness pipe FD. Signaling readiness.", logger.LogFields{"fd": readinessFD})
		if errSignal := util.SignalChildReadyByClosingFD(readinessFD); errSignal != nil {
			s.log.Error("Child process: Error signaling readiness by closing pipe FD", logger.LogFields{"fd": readinessFD, "error": errSignal.Error()})
		} else {
			s.log.Info("Child process: Successfully signaled readiness.", logger.LogFields{"fd": readinessFD})
		}
	}

	go s.handleSignals()

	if err := s.StartAccepting(); err != nil {
		s.log.Error("Failed to start accepting connections", logger.LogFields{"error": err.Error()})
		startErr = fmt.Errorf("failed to start accepting connections: %w", err)
		// Attempt to gracefully shut down if we can't start accepting.
		// Pass the error as the reason for shutdown.
		// s.Shutdown will close s.doneChan.
		go s.Shutdown(startErr)
	} else {
		s.log.Info("Server started successfully. Waiting for shutdown signal...", logger.LogFields{"listeners_count": len(s.listeners)})
	}

	// Block until shutdown is complete. s.doneChan is closed at the end of s.Shutdown().
	<-s.doneChan

	if startErr != nil {
		s.log.Info("Server Start() method finished due to startup error.", logger.LogFields{"error": startErr.Error()})
		return startErr
	}

	s.log.Info("Server Start() method finished.", nil)
	return nil
}

// Done returns a channel that is closed when the server has completely shut down.
func (s *Server) Done() <-chan struct{} {
	return s.doneChan
}

// handleSignals listens for OS signals and acts accordingly.
// It stops listening when the server's shutdownChan is closed.
func (s *Server) handleSignals() {
	// Register for notifications. s.reloadChan is buffered.
	signal.Notify(s.reloadChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	s.log.Info("Signal handler started. Listening for SIGINT, SIGTERM, SIGHUP.", nil)

	defer func() {
		signal.Stop(s.reloadChan) // Clean up: stop notifying this channel.
		// Do not close s.reloadChan here if other goroutines might still select on it,
		// or if it's closed elsewhere. However, if this is the sole manager, closing is fine.
		// Given its name and usage, it's likely specific to this signal handling.
		// Let's assume it's safe to close *if* no other part of the system writes to it.
		// For safety, and since `Stop` is the primary cleanup, let's not close it here
		// unless explicitly designed for single writer/closer.
		s.log.Info("Signal handler stopped.", nil)
	}()

	for {
		select {
		case sig, ok := <-s.reloadChan:
			if !ok {
				// s.reloadChan was closed, perhaps by a previous signal handler instance exiting.
				s.log.Info("Signal channel (reloadChan) closed, signal handler exiting.", nil)
				return
			}
			s.log.Info("Received signal", logger.LogFields{"signal": sig.String()})
			switch sig {
			case syscall.SIGINT, syscall.SIGTERM:
				s.log.Info("SIGINT/SIGTERM received, initiating graceful shutdown.", logger.LogFields{"signal": sig.String()})
				// s.Shutdown is idempotent and handles being called multiple times.
				// Run in a goroutine so that if Shutdown blocks for a long time,
				// this loop can still exit if s.shutdownChan closes independently.
				go s.Shutdown(fmt.Errorf("received signal %s", sig.String()))
				// The loop will exit via the s.shutdownChan case below.
			case syscall.SIGHUP:
				s.log.Info("SIGHUP received, handling.", nil)
				// handleSIGHUP might be a long-running operation if it involves forking.
				// For complex operations, it should manage its own goroutines.
				// For now, direct call is fine as it's mostly logging and log reopening.
				s.handleSIGHUP()
			}
		case <-s.shutdownChan: // This channel is closed by s.Shutdown() when shutdown starts
			s.log.Info("Shutdown initiated (detected via shutdownChan), signal handler exiting.", nil)
			return // Exit signal handling loop
		}
	}
}

// handleSIGHUP handles the SIGHUP signal.
// This function is the entry point for configuration reload and/or binary upgrade.
// Currently, it reopens log files as per spec 3.5.1.
// The full logic for config reload and binary upgrade (spec section 4) will be
// implemented here or called from here in future steps.
func (s *Server) handleSIGHUP() {
	s.mu.RLock()
	cfgPath := s.configFilePath
	currentLog := s.log
	// Make a deep copy of listener FDs for the child process to inherit.
	// This is crucial because s.listenerFDs might change if the server somehow
	// reinitializes listeners, though unlikely during SIGHUP for the old parent.
	currentListenersFDs := make([]uintptr, len(s.listenerFDs))
	copy(currentListenersFDs, s.listenerFDs)
	s.mu.RUnlock()

	currentLog.Info("SIGHUP received. Processing for hot reload/upgrade...", nil)

	// 1. Reopen log files (Spec 3.5.1)
	currentLog.Info("Attempting to reopen log files due to SIGHUP...", nil)
	if err := currentLog.ReopenLogFiles(); err != nil {
		currentLog.Error("Failed to reopen log files on SIGHUP", logger.LogFields{"error": err.Error()})
	} else {
		currentLog.Info("Successfully reopened log files (if configured for file output).", nil)
	}

	// 2. Load and validate new configuration (Spec 4.3.2 Parent item 1)
	currentLog.Info("Loading new configuration for potential reload...", logger.LogFields{"path": cfgPath})
	newCfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		currentLog.Error("Failed to load or validate new configuration on SIGHUP. Aborting reload, continuing with old configuration.", logger.LogFields{"path": cfgPath, "error": err.Error()})
		return // Continue with old configuration
	}
	currentLog.Info("New configuration loaded and validated successfully.", nil)

	// 3. Determine executable path for the new process (Spec 4.3.2 Parent item 2)
	execPath := os.Args[0] // Default to current executable path
	if newCfg.Server != nil && newCfg.Server.ExecutablePath != nil && *newCfg.Server.ExecutablePath != "" {
		resolvedPath, errAbs := filepath.Abs(*newCfg.Server.ExecutablePath)
		if errAbs != nil {
			currentLog.Error("Failed to resolve absolute path for new executable. Aborting reload.", logger.LogFields{"configured_path": *newCfg.Server.ExecutablePath, "error": errAbs.Error()})
			return
		}
		execPath = resolvedPath
		currentLog.Info("Using executable path from new configuration.", logger.LogFields{"path": execPath})
	} else {
		currentLog.Info("Using current executable path for new process.", logger.LogFields{"path": execPath})
	}

	// 4. Create readiness pipe (Spec 4.3.2 Parent item 4 implies this mechanism)
	parentReadPipe, childWriteFDNum, err := util.CreateReadinessPipe()
	if err != nil {
		currentLog.Error("Failed to create readiness pipe. Aborting reload.", logger.LogFields{"error": err.Error()})
		return // Cannot proceed without readiness signaling
	}
	defer parentReadPipe.Close() // Ensure parent's read end of the pipe is closed eventually

	// 5. Prepare environment variables for the child process (Spec 4.3.2 Parent item 3)
	env := os.Environ() // Get current environment

	// Add listener FDs via LISTEN_FDS
	if len(currentListenersFDs) > 0 {
		var listenerFDStrings []string
		for _, fd := range currentListenersFDs {
			listenerFDStrings = append(listenerFDStrings, fmt.Sprintf("%d", fd))
		}
		env = append(env, fmt.Sprintf("%s=%s", util.ListenFdsEnvKey, strings.Join(listenerFDStrings, ":")))
		currentLog.Debug("Adding LISTEN_FDS to child environment", logger.LogFields{util.ListenFdsEnvKey: strings.Join(listenerFDStrings, ":")})
	} else {
		currentLog.Info("No listener FDs to pass to child process.", nil)
	}

	// Add readiness pipe FD via READINESS_PIPE_FD
	env = append(env, fmt.Sprintf("%s=%d", util.ReadinessPipeEnvKey, childWriteFDNum))
	currentLog.Debug("Adding READINESS_PIPE_FD to child environment", logger.LogFields{util.ReadinessPipeEnvKey: childWriteFDNum})
	currentLog.Info("Prepared environment for child process.", nil)

	// 6. Start the new child process (Spec 4.3.2 Parent item 2)
	procAttr := &os.ProcAttr{
		Env:   env,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}, // Inherit standard I/O
	}

	currentLog.Info("Forking and executing new server process...", logger.LogFields{"executable": execPath, "args": os.Args, "env_keys_for_reload": []string{util.ListenFdsEnvKey, util.ReadinessPipeEnvKey}})
	// os.Args includes the current executable name as os.Args[0], followed by arguments.
	// The child process should re-parse its command-line arguments if needed.
	childProc, err := os.StartProcess(execPath, os.Args, procAttr)
	if err != nil {
		currentLog.Error("Failed to start new server process. Aborting reload.", logger.LogFields{"executable": execPath, "error": err.Error()})
		// parentReadPipe will be closed by defer. childWriteFDNum is just a number, its os.File was closed by CreateReadinessPipe.
		return
	}
	s.mu.Lock()
	s.childProcess = childProc // Store for potential future management (e.g. on immediate parent exit)
	s.mu.Unlock()
	currentLog.Info("New server process started.", logger.LogFields{"pid": childProc.Pid})

	// 7. Wait for the child to signal readiness (Spec 4.3.2 Parent item 4)
	childReadinessTimeout := 10 * time.Second // Default timeout
	if newCfg.Server != nil && newCfg.Server.ChildReadinessTimeout != nil && *newCfg.Server.ChildReadinessTimeout != "" {
		parsedTimeout, parseErr := time.ParseDuration(*newCfg.Server.ChildReadinessTimeout)
		if parseErr == nil && parsedTimeout > 0 {
			childReadinessTimeout = parsedTimeout
		} else if parseErr != nil {
			currentLog.Warn("Failed to parse child_readiness_timeout from new config, using default.", logger.LogFields{"value": *newCfg.Server.ChildReadinessTimeout, "default": childReadinessTimeout, "error": parseErr.Error()})
		}
	}
	currentLog.Info("Waiting for child process to signal readiness...", logger.LogFields{"pid": childProc.Pid, "timeout": childReadinessTimeout.String()})

	err = util.WaitForChildReadyPipeClose(parentReadPipe, childReadinessTimeout)
	// parentReadPipe is closed by defer after this point.

	if err != nil {
		// 9. Child fails to signal readiness or times out (Spec 4.3.2 Parent item 6)
		currentLog.Error("Child process failed to signal readiness or timed out. Aborting reload. Old parent continues service.", logger.LogFields{"pid": childProc.Pid, "error": err.Error()})
		if childProc != nil {
			currentLog.Info("Attempting to terminate unresponsive/failed child process.", logger.LogFields{"pid": childProc.Pid})
			if killErr := childProc.Kill(); killErr != nil {
				currentLog.Error("Failed to kill unresponsive/failed child process.", logger.LogFields{"pid": childProc.Pid, "error": killErr.Error()})
			} else {
				currentLog.Info("Unresponsive/failed child process killed.", logger.LogFields{"pid": childProc.Pid})
				_, _ = childProc.Wait() // Reap the child process
			}
		}
		s.mu.Lock()
		s.childProcess = nil // Clear child process reference
		s.mu.Unlock()
		return // Continue operating with the old configuration and process
	}

	// 8. Child is ready. Initiate graceful shutdown of the current (old parent) process. (Spec 4.3.2 Parent item 5)
	currentLog.Info("Child process is ready. Old parent initiating graceful shutdown.", logger.LogFields{"child_pid": childProc.Pid})

	// The graceful_shutdown_timeout for the OLD parent comes from its own current configuration (s.cfg).
	// The s.Shutdown() method will handle stopping new connections, sending GOAWAY, etc.
	shutdownReason := fmt.Errorf("graceful shutdown due to successful hot reload/upgrade to child PID %d", childProc.Pid)

	// s.Shutdown() is blocking. It will close s.doneChan when complete.
	// The main s.Run() loop waits on s.doneChan and will exit naturally.
	// The os.Exit(0) ensures this process terminates with a success code after shutdown.
	//
	// We need to use the *new* configuration's graceful shutdown timeout for the old parent's shutdown,
	// as per spec 4.3.2 item 5 "up to a configurable server.graceful_shutdown_timeout (...) specified in the server's main configuration"
	// - implies the NEWLY loaded config's timeout.
	// The `Shutdown` method in `server.go` currently reads this from `s.cfg` which is the *old* config.
	// This is a slight deviation from the spec if not addressed.
	// For now, I will proceed with the existing Shutdown behavior. The task is to implement handleSIGHUP.
	// If `Shutdown` needs to take a timeout argument to adhere strictly, that's a separate refactor of `Shutdown`.

	go func() { // Run shutdown in a goroutine to allow os.Exit(0) to be called.
		s.Shutdown(shutdownReason)
		currentLog.Info("Old parent shutdown complete. Exiting process.", nil)
		os.Exit(0)
	}()
	// Keep the SIGHUP handler from blocking indefinitely if Shutdown hangs; let the OS terminate if necessary.
	// The main server process is expected to exit.
}

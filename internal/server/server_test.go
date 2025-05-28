package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"os"
	"strings"
	"sync"
	// "syscall" // Not directly used in mocks, but tests might send signals
	"testing"
	"time"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/http2"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/util"
)

// --- Mock Logger ---
// newMockLogger creates a logger instance suitable for testing.
// By default, it discards output. Provide a buffer to capture output.
func newMockLogger(out io.Writer) *logger.Logger {
	if out == nil {
		out = io.Discard
	}
	return logger.NewTestLogger(out)
}

// --- Mock Router ---
type mockRouter struct {
	ServeHTTPFunc func(s ResponseWriterStream, req *http.Request)
	mu            sync.Mutex
	serveHTTPArgs []struct { // Stores arguments for each call
		stream ResponseWriterStream
		req    *http.Request
	}
	serveHTTPCount int
}

// ServeHTTP implements the RouterInterface for the mock.
// It records the call and its arguments, then calls the user-provided ServeHTTPFunc if set.
func (m *mockRouter) ServeHTTP(s ResponseWriterStream, req *http.Request) {
	m.mu.Lock()
	m.serveHTTPArgs = append(m.serveHTTPArgs, struct {
		stream ResponseWriterStream
		req    *http.Request
	}{s, req})
	m.serveHTTPCount++
	m.mu.Unlock()
	if m.ServeHTTPFunc != nil {
		m.ServeHTTPFunc(s, req)
	}
}

// GetServeHTTPCallCount returns how many times ServeHTTP was called.
func (m *mockRouter) GetServeHTTPCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.serveHTTPCount
}

// GetServeHTTPArgsForCall returns the arguments for the i-th call to ServeHTTP.
func (m *mockRouter) GetServeHTTPArgsForCall(i int) (ResponseWriterStream, *http.Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.serveHTTPArgs) {
		return nil, nil, fmt.Errorf("call index %d out of bounds (0-%d)", i, len(m.serveHTTPArgs)-1)
	}
	return m.serveHTTPArgs[i].stream, m.serveHTTPArgs[i].req, nil
}

// newMockRouter creates a new mockRouter.
func newMockRouter() *mockRouter {
	return &mockRouter{serveHTTPArgs: make([]struct {
		stream ResponseWriterStream
		req    *http.Request
	}, 0)}
}

// --- Mock Config ---
// newTestConfig creates a minimal, valid configuration for testing purposes.
func newTestConfig(addr string) *config.Config {
	if addr == "" {
		addr = "127.0.0.1:0" // Dynamic port for listener tests
	}
	trueBool := true
	logLevel := config.LogLevelDebug // Use debug for tests to capture more
	timeout := "1s"                  // Short timeouts for tests
	grace := "1s"
	return &config.Config{
		Server: &config.ServerConfig{
			Address:                 &addr,
			ChildReadinessTimeout:   &timeout,
			GracefulShutdownTimeout: &grace,
		},
		Logging: &config.LoggingConfig{
			LogLevel: logLevel,
			AccessLog: &config.AccessLogConfig{
				Enabled: &trueBool,
				Target:  strPtr("stdout"), // In tests, logger redirects this
				Format:  "json",
			},
			ErrorLog: &config.ErrorLogConfig{
				Target: strPtr("stderr"), // In tests, logger redirects this
			},
		},
		Routing: &config.RoutingConfig{
			Routes: []config.Route{}, // Add sample routes if needed
		},
	}
}
func strPtr(s string) *string { return &s }

// --- Mock net.Listener ---
type mockListener struct {
	AcceptFunc func() (net.Conn, error)
	CloseFunc  func() error
	AddrFunc   func() net.Addr
	acceptChan chan net.Conn // Channel to feed connections to Accept()
	errChan    chan error    // Channel to feed errors to Accept()
	closeOnce  sync.Once
	closed     chan struct{} // Closed when the listener's Close() is called
	addr       net.Addr
}

// newMockListener creates a new mockListener.
// addrStr is the address string it should report (e.g., "127.0.0.1:8080").
func newMockListener(addrStr string) *mockListener {
	if addrStr == "" {
		addrStr = "127.0.0.1:0" // Default to dynamic port for flexibility
	}
	addr, err := net.ResolveTCPAddr("tcp", addrStr)
	if err != nil { // Fallback if resolve fails (e.g. minimal test env)
		addr = &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
	}

	ml := &mockListener{
		acceptChan: make(chan net.Conn, 1), // Buffered to allow injecting before Accept is called
		errChan:    make(chan error, 1),
		closed:     make(chan struct{}),
		addr:       addr,
	}
	// Default implementations
	ml.AddrFunc = func() net.Addr { return ml.addr }
	ml.CloseFunc = func() error {
		ml.closeOnce.Do(func() {
			close(ml.closed)
		})
		return nil
	}
	ml.AcceptFunc = func() (net.Conn, error) {
		select {
		case conn := <-ml.acceptChan:
			return conn, nil
		case err := <-ml.errChan:
			return nil, err
		case <-ml.closed: // If listener was closed
			return nil, net.ErrClosed
		}
	}
	return ml
}
func (m *mockListener) Accept() (net.Conn, error) { return m.AcceptFunc() }
func (m *mockListener) Close() error              { return m.CloseFunc() }
func (m *mockListener) Addr() net.Addr            { return m.AddrFunc() }

// InjectConn allows tests to provide a net.Conn that Accept() will return.
func (m *mockListener) InjectConn(c net.Conn) { m.acceptChan <- c }

// InjectAcceptError allows tests to make Accept() return a specific error.
func (m *mockListener) InjectAcceptError(e error) { m.errChan <- e }

// --- Mock net.Conn ---
type mockConn struct {
	ReadFunc             func(b []byte) (n int, err error)
	WriteFunc            func(b []byte) (n int, err error)
	CloseFunc            func() error
	LocalAddrFunc        func() net.Addr
	RemoteAddrFunc       func() net.Addr
	SetDeadlineFunc      func(t time.Time) error
	SetReadDeadlineFunc  func(t time.Time) error
	SetWriteDeadlineFunc func(t time.Time) error
	SetNoDelayFunc       func(noDelay bool) error // For TCPConn cast compatibility

	localAddr   net.Addr
	remoteAddr  net.Addr
	readBuffer  *bytes.Buffer // Data this mockConn will provide on Read()
	writeBuffer *bytes.Buffer // Data written to this mockConn via Write()
	closeOnce   sync.Once
	closed      chan struct{}
	mu          sync.Mutex // Protects buffers and closed state
}

// newMockConn creates a new mockConn.
func newMockConn(localStr, remoteStr string) *mockConn {
	if localStr == "" {
		localStr = "127.0.0.1:12345"
	}
	if remoteStr == "" {
		remoteStr = "127.0.0.1:54321"
	}

	lAddr, _ := net.ResolveTCPAddr("tcp", localStr)
	rAddr, _ := net.ResolveTCPAddr("tcp", remoteStr)

	mc := &mockConn{
		localAddr:   lAddr,
		remoteAddr:  rAddr,
		readBuffer:  bytes.NewBuffer(nil),
		writeBuffer: bytes.NewBuffer(nil),
		closed:      make(chan struct{}),
	}

	// Default implementations
	mc.ReadFunc = func(b []byte) (n int, err error) {
		mc.mu.Lock()
		defer mc.mu.Unlock()
		select {
		case <-mc.closed:
			return 0, io.EOF // Standard behavior for closed connection read
		default:
			return mc.readBuffer.Read(b)
		}
	}
	mc.WriteFunc = func(b []byte) (n int, err error) {
		mc.mu.Lock()
		defer mc.mu.Unlock()
		select {
		case <-mc.closed:
			return 0, errors.New("write to closed mockConn")
		default:
			return mc.writeBuffer.Write(b)
		}
	}
	mc.CloseFunc = func() error {
		mc.closeOnce.Do(func() {
			mc.mu.Lock()
			close(mc.closed)
			mc.mu.Unlock()
		})
		return nil
	}
	mc.LocalAddrFunc = func() net.Addr { return mc.localAddr }
	mc.RemoteAddrFunc = func() net.Addr { return mc.remoteAddr }
	mc.SetNoDelayFunc = func(bool) error { return nil } // Mock SetNoDelay
	mc.SetDeadlineFunc = func(time.Time) error { return nil }
	mc.SetReadDeadlineFunc = func(time.Time) error { return nil }
	mc.SetWriteDeadlineFunc = func(time.Time) error { return nil }
	return mc
}
func (m *mockConn) Read(b []byte) (n int, err error)   { return m.ReadFunc(b) }
func (m *mockConn) Write(b []byte) (n int, err error)  { return m.WriteFunc(b) }
func (m *mockConn) Close() error                       { return m.CloseFunc() }
func (m *mockConn) LocalAddr() net.Addr                { return m.LocalAddrFunc() }
func (m *mockConn) RemoteAddr() net.Addr               { return m.RemoteAddrFunc() }
func (m *mockConn) SetDeadline(t time.Time) error      { return m.SetDeadlineFunc(t) }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return m.SetReadDeadlineFunc(t) }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return m.SetWriteDeadlineFunc(t) }

// SetReadBuffer sets the data that this mockConn will return on Read calls.
func (m *mockConn) SetReadBuffer(data []byte) {
	m.mu.Lock()
	m.readBuffer.Reset()
	m.readBuffer.Write(data)
	m.mu.Unlock()
}

// GetWriteBuffer returns all data written to this mockConn.
func (m *mockConn) GetWriteBuffer() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Return a copy to prevent race conditions if caller modifies it
	// and writerBuffer is written to concurrently (though Write is mutexed).
	dataCopy := make([]byte, m.writeBuffer.Len())
	copy(dataCopy, m.writeBuffer.Bytes())
	return dataCopy
}

// --- Mock http2.Connection ---
// This is a conceptual mock for *http2.Connection. Since server.go directly calls
// http2.NewConnection, fully unit testing handleTCPConnection with a mock *http2.Connection
// would require refactoring server.go (e.g., making http2.NewConnection a injectable function).
// This mock can be used if such refactoring is done or for testing components that take an
// http2.Connection interface (if one were defined and used).
type mockH2Conn struct {
	ServerHandshakeFunc func() error
	ServeFunc           func(ctx context.Context) error
	CloseFunc           func(err error) error

	mu              sync.Mutex
	handshakeCalled bool
	serveCalled     bool
	closeCalled     bool
	closedWithError error
}

func newMockH2Conn() *mockH2Conn {
	m := &mockH2Conn{}
	m.ServerHandshakeFunc = func() error { m.mu.Lock(); m.handshakeCalled = true; m.mu.Unlock(); return nil }
	// Default ServeFunc blocks until context is done, simulating a running connection.
	m.ServeFunc = func(ctx context.Context) error {
		m.mu.Lock()
		m.serveCalled = true
		m.mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	m.CloseFunc = func(err error) error {
		m.mu.Lock()
		m.closeCalled = true
		m.closedWithError = err
		m.mu.Unlock()
		return nil
	}
	return m
}

// Getter methods to inspect mock state
func (m *mockH2Conn) HandshakeCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.handshakeCalled
}
func (m *mockH2Conn) ServeCalled() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.serveCalled }
func (m *mockH2Conn) CloseCalled() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.closeCalled }
func (m *mockH2Conn) ClosedWithError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closedWithError
}

// --- Mock server.ResponseWriterStream ---
type mockResponseWriterStream struct {
	SendHeadersFunc   func(headers []http2.HeaderField, endStream bool) error
	WriteDataFunc     func(p []byte, endStream bool) (n int, err error)
	WriteTrailersFunc func(trailers []http2.HeaderField) error
	IDFunc            func() uint32
	ContextFunc       func() context.Context

	mu            sync.Mutex
	HeadersSent   []http2.HeaderField
	DataWritten   *bytes.Buffer
	TrailersSent  []http2.HeaderField
	EndStreamSent bool
	id            uint32
	ctx           context.Context
}

func newMockResponseWriterStream(id uint32, ctx context.Context) *mockResponseWriterStream {
	if ctx == nil {
		ctx = context.Background()
	}
	m := &mockResponseWriterStream{
		DataWritten: bytes.NewBuffer(nil),
		id:          id,
		ctx:         ctx,
	}
	// Default implementations
	m.SendHeadersFunc = func(headers []http2.HeaderField, endStream bool) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.HeadersSent = headers
		if endStream {
			m.EndStreamSent = true
		}
		return nil
	}
	m.WriteDataFunc = func(p []byte, endStream bool) (n int, err error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if endStream {
			m.EndStreamSent = true
		}
		return m.DataWritten.Write(p)
	}
	m.WriteTrailersFunc = func(trailers []http2.HeaderField) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.TrailersSent = trailers
		m.EndStreamSent = true // Trailers always end the stream
		return nil
	}
	m.IDFunc = func() uint32 { return m.id }
	m.ContextFunc = func() context.Context { return m.ctx }
	return m
}
func (m *mockResponseWriterStream) SendHeaders(headers []http2.HeaderField, endStream bool) error {
	return m.SendHeadersFunc(headers, endStream)
}
func (m *mockResponseWriterStream) WriteData(p []byte, endStream bool) (n int, err error) {
	return m.WriteDataFunc(p, endStream)
}
func (m *mockResponseWriterStream) WriteTrailers(trailers []http2.HeaderField) error {
	return m.WriteTrailersFunc(trailers)
}
func (m *mockResponseWriterStream) ID() uint32               { return m.IDFunc() }
func (m *mockResponseWriterStream) Context() context.Context { return m.ContextFunc() }

// --- Mocking os and util functions ---
// These are package-level variables in server_test.go that tests can override.
var (
	// os functions
	osStartProcessFunc func(name string, argv []string, attr *os.ProcAttr) (*os.Process, error)

	// util functions
	utilParseInheritedListenerFDsFunc   func(envVarName string) ([]uintptr, error)
	utilNewListenerFromFDFunc           func(fd uintptr) (net.Listener, error)
	utilCreateListenerAndGetFDFunc      func(address string) (net.Listener, uintptr, error)
	utilGetInheritedReadinessPipeFDFunc func() (uintptr, bool, error)
	utilSignalChildReadyByClosingFDFunc func(fd uintptr) error
	utilCreateReadinessPipeFunc         func() (parentReadPipe *os.File, childWriteFD uintptr, err error)
	utilWaitForChildReadyPipeCloseFunc  func(parentReadPipe *os.File, timeout time.Duration) error
)

// Stores original implementations to restore after tests.
var originalOSUtilFuncs struct {
	osStartProcess                  func(name string, argv []string, attr *os.ProcAttr) (*os.Process, error)
	utilParseInheritedListenerFDs   func(envVarName string) ([]uintptr, error)
	utilNewListenerFromFD           func(fd uintptr) (net.Listener, error)
	utilCreateListenerAndGetFD      func(address string) (net.Listener, uintptr, error)
	utilGetInheritedReadinessPipeFD func() (uintptr, bool, error)
	utilSignalChildReadyByClosingFD func(fd uintptr) error
	utilCreateReadinessPipe         func() (parentReadPipe *os.File, childWriteFD uintptr, err error)
	utilWaitForChildReadyPipeClose  func(parentReadPipe *os.File, timeout time.Duration) error
}

// setupMocks initializes mockable functions to their real implementations.
// Call this at the beginning of tests or test suites that need to mock these.
// Defer teardownMocks to restore them.
func setupMocks() {
	originalOSUtilFuncs.osStartProcess = os.StartProcess
	osStartProcessFunc = os.StartProcess

	originalOSUtilFuncs.utilParseInheritedListenerFDs = util.ParseInheritedListenerFDs
	utilParseInheritedListenerFDsFunc = util.ParseInheritedListenerFDs

	originalOSUtilFuncs.utilNewListenerFromFD = util.NewListenerFromFD
	utilNewListenerFromFDFunc = util.NewListenerFromFD

	originalOSUtilFuncs.utilCreateListenerAndGetFD = util.CreateListenerAndGetFD
	utilCreateListenerAndGetFDFunc = util.CreateListenerAndGetFD

	originalOSUtilFuncs.utilGetInheritedReadinessPipeFD = util.GetInheritedReadinessPipeFD
	utilGetInheritedReadinessPipeFDFunc = util.GetInheritedReadinessPipeFD

	originalOSUtilFuncs.utilSignalChildReadyByClosingFD = util.SignalChildReadyByClosingFD
	utilSignalChildReadyByClosingFDFunc = util.SignalChildReadyByClosingFD

	originalOSUtilFuncs.utilCreateReadinessPipe = util.CreateReadinessPipe
	utilCreateReadinessPipeFunc = util.CreateReadinessPipe

	originalOSUtilFuncs.utilWaitForChildReadyPipeClose = util.WaitForChildReadyPipeClose
	utilWaitForChildReadyPipeCloseFunc = util.WaitForChildReadyPipeClose
}

// teardownMocks restores the original function implementations.
func teardownMocks() {
	osStartProcessFunc = originalOSUtilFuncs.osStartProcess
	utilParseInheritedListenerFDsFunc = originalOSUtilFuncs.utilParseInheritedListenerFDs
	utilNewListenerFromFDFunc = originalOSUtilFuncs.utilNewListenerFromFD
	utilCreateListenerAndGetFDFunc = originalOSUtilFuncs.utilCreateListenerAndGetFD
	utilGetInheritedReadinessPipeFDFunc = originalOSUtilFuncs.utilGetInheritedReadinessPipeFD
	utilSignalChildReadyByClosingFDFunc = originalOSUtilFuncs.utilSignalChildReadyByClosingFD
	utilCreateReadinessPipeFunc = originalOSUtilFuncs.utilCreateReadinessPipe
	utilWaitForChildReadyPipeCloseFunc = originalOSUtilFuncs.utilWaitForChildReadyPipeClose
}

// TestServer_MockInfrastructure is a basic test to ensure the mocking setup works.
func TestServer_MockInfrastructure(t *testing.T) {
	setupMocks()          // Save originals and set up default (real) function pointers
	defer teardownMocks() // Restore originals

	// Example: Override a util function for this test
	expectedFDs := []uintptr{3, 4, 5}
	var calledEnvVarName string
	utilParseInheritedListenerFDsFunc = func(envVarName string) ([]uintptr, error) {
		calledEnvVarName = envVarName
		return expectedFDs, nil
	}

	// Call the function via the package-level variable (which server.go would do indirectly)
	// Here we call our own var directly, but server.go would call util.ParseInheritedListenerFDs.
	// To test server.go, server.go needs to use these vars, or we patch util itself.
	// For this example, we'll assume server.go is refactored or we're testing the mock setup itself.
	// If testing server.go directly without refactoring server.go to use these vars,
	// this pattern requires patching the actual util.ParseInheritedListenerFDs.
	// Let's assume for this test it means the mock func var is used correctly.

	// This test is more about the setup/teardown and ability to swap.
	// A true test of server.go would involve instantiating Server and having *it* call util functions.
	// The current `utilParseInheritedListenerFDsFunc` will only be called if code within `server_test.go`
	// calls this variable. For `server.go` to use these mocks, `server.go` would need to be
	// modified to call `server_test.go`'s vars (not feasible) or use an injection pattern.
	// The standard way for this pattern to work for unit testing `server.go` is if `server.go` itself
	// used these (e.g. `var parseFDs = util.ParseInheritedListenerFDs` in `server.go`, which tests could change).
	// Lacking that, this test shows the *mechanism* for mocking in `server_test.go`.

	testEnvKey := "LISTEN_FDS_TEST"
	fds, err := utilParseInheritedListenerFDsFunc(testEnvKey) // Direct call to the var

	if err != nil {
		t.Fatalf("Mocked utilParseInheritedListenerFDsFunc returned error: %v", err)
	}
	if calledEnvVarName != testEnvKey {
		t.Errorf("Expected env var name '%s', got '%s'", testEnvKey, calledEnvVarName)
	}
	if len(fds) != len(expectedFDs) || fds[0] != expectedFDs[0] { // Basic check
		t.Errorf("Expected FDs %v, got %v", expectedFDs, fds)
	}

	// Test teardown restores original
	teardownMocks() // Restore
	setupMocks()    // Set again for next step

	// Now utilParseInheritedListenerFDsFunc should be the real one
	// This requires os.Setenv to test the real one, which is outside scope of "mocking".
	// So, this part of the test is more illustrative of the restore mechanism.
	// For example, if we had a simple mock that always returns error:
	utilParseInheritedListenerFDsFunc = func(envVarName string) ([]uintptr, error) {
		return nil, fmt.Errorf("always error mock")
	}
	_, err = utilParseInheritedListenerFDsFunc(testEnvKey)
	if err == nil || err.Error() != "always error mock" {
		t.Errorf("Expected 'always error mock', got %v", err)
	}

	teardownMocks() // ensure it's called at end of test.
	// If real util.ParseInheritedListenerFDs was called now, it'd behave normally.
}

// TestServer_NewServer_NilArgs tests argument validation for NewServer.
func TestServer_NewServer_NilArgs(t *testing.T) {
	// Mocks for valid arguments
	lg := newMockLogger(nil)
	rt := newMockRouter()
	hr := NewHandlerRegistry()
	cfg := newTestConfig("") // A valid config

	tests := []struct {
		name        string
		cfg         *config.Config
		lg          *logger.Logger
		rt          RouterInterface
		hr          *HandlerRegistry
		path        string
		expectedErr string
	}{
		{"nil config", nil, lg, rt, hr, "test.json", "config cannot be nil"},
		{"nil logger", cfg, nil, rt, hr, "test.json", "logger cannot be nil"},
		{"nil router", cfg, lg, nil, hr, "test.json", "router cannot be nil"},
		{"nil registry", cfg, lg, rt, nil, "test.json", "handler registry cannot be nil"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This uses the real util.ParseInheritedListenerFDs etc. unless mocks are set
			// For NewServer, these are not called immediately, so default mocks are fine.
			setupMocks()
			defer teardownMocks()

			_, err := NewServer(tt.cfg, tt.lg, tt.rt, tt.path, tt.hr)
			if err == nil {
				t.Fatalf("Expected error for %s, got nil", tt.name)
			}
			if errMsg := err.Error(); errMsg != tt.expectedErr {
				t.Errorf("For %s, expected error message '%s', got '%s'", tt.name, tt.expectedErr, errMsg)
			}
		})
	}
}

func TestServer_DispatchRequest(t *testing.T) {
	cfg := newTestConfig("")
	originalCfgPath := "test_config.json" // Dummy path

	// Common setup for NewServer
	newTestServer := func(t *testing.T, lg *logger.Logger, rtr RouterInterface) *Server {
		t.Helper()
		registry := NewHandlerRegistry() // Required by NewServer
		// For these tests, we don't need SIGHUP/reload logic, so os/util mocks are not strictly configured per test.
		// NewServer itself might call util.ParseInheritedListenerFDs, ensure it's mocked if problematic.
		// Default setupMocks/teardownMocks will use real implementations if not overridden.
		// Here, we assume default (no inherited FDs) is fine for NewServer in these specific tests.
		s, err := NewServer(cfg, lg, rtr, originalCfgPath, registry)
		if err != nil {
			t.Fatalf("NewServer failed: %v", err)
		}
		return s
	}

	t.Run("ValidStream_RouterCalled", func(t *testing.T) {
		logBuf := &bytes.Buffer{}
		mockLog := newMockLogger(logBuf)
		mockRtr := newMockRouter()
		s := newTestServer(t, mockLog, mockRtr)

		mockStream := newMockResponseWriterStream(1, context.Background())
		req, _ := http.NewRequest("GET", "/test", nil)

		s.dispatchRequest(mockStream, req)

		if mockRtr.GetServeHTTPCallCount() != 1 {
			t.Errorf("Expected router.ServeHTTP to be called once, got %d", mockRtr.GetServeHTTPCallCount())
		} else {
			calledStream, calledReq, err := mockRtr.GetServeHTTPArgsForCall(0)
			if err != nil {
				t.Fatalf("GetServeHTTPArgsForCall(0) failed: %v", err)
			}
			if calledStream != mockStream {
				t.Errorf("Router called with wrong stream. Expected %p, got %p", mockStream, calledStream)
			}
			if calledReq != req {
				t.Errorf("Router called with wrong request. Expected %p, got %p", req, calledReq)
			}
		}
		// Check if the log contains the specific error message we want to avoid
		if logBuf.Len() > 0 && strings.Contains(logBuf.String(), "does not implement server.ResponseWriterStream") {
			t.Errorf("Expected no error log about stream type mismatch, but got: %s", logBuf.String())
		}
	})

	t.Run("InvalidStream_NilStream_Panics", func(t *testing.T) {
		logBuf := &bytes.Buffer{} // To check if any logging happens *before* panic
		mockLog := newMockLogger(logBuf)
		mockRtr := newMockRouter()
		s := newTestServer(t, mockLog, mockRtr)

		var nilStream http2.StreamWriter // Explicitly nil
		req, _ := http.NewRequest("GET", "/test", nil)

		var panicked bool
		var panicValue interface{}
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
					panicValue = r
				}
			}()
			// This call is expected to panic because server.dispatchRequest will
			// attempt to call methods (e.g., stream.ID()) on the nil stream
			// after the type assertion `nil.(ResponseWriterStream)` results in `ok == false`.
			s.dispatchRequest(nilStream, req)
		}()

		if !panicked {
			t.Errorf("Expected a panic when dispatchRequest is called with a nil stream, but it did not panic.")
		} else {
			// Optional: check the panic value if it's specific, e.g., related to nil pointer dereference.
			// For now, just logging it is fine for confirming the panic.
			t.Logf("Caught expected panic with nil stream: %v", panicValue)
			// Example check: if !strings.Contains(fmt.Sprintf("%v", panicValue), "nil pointer dereference") {
			//	 t.Errorf("Panic value does not seem to be a nil pointer dereference: %v", panicValue)
			// }
		}

		// With the current server.go implementation, no logging or router call will happen due to the panic.
		if mockRtr.GetServeHTTPCallCount() > 0 {
			t.Errorf("Expected router.ServeHTTP not to be called due to panic, but it was called %d times", mockRtr.GetServeHTTPCallCount())
		}
		// The specific log message "does not implement server.ResponseWriterStream" comes from
		// the block that would be executed if ok was false. The panic on stream.ID() inside that block
		// means the full log line might not be written, or the panic occurs at stream.ID().
		// Checking the log buffer for this specific message can be tricky if the panic happens mid-log.
		// For this test, the primary check is the panic itself.
		if logBuf.Len() > 0 && strings.Contains(logBuf.String(), "dispatchRequest: provided stream") {
			t.Logf("Log buffer contains initial part of error message, as expected before panic: %s", logBuf.String())
		}
	})

	// The following test case ("InvalidStream_TypeMismatch_LogsAndAttempts500") is commented out.
	// Reason: In Go, interface satisfaction is structural. The interfaces
	// `http2.StreamWriter` (from internal/http2/stream_writer.go) and
	// `server.ResponseWriterStream` (from internal/server/handler.go) are currently
	// structurally identical (same methods, same parameter/return types, including
	// both using `http2.HeaderField`).
	// Therefore, any non-nil variable that satisfies `http2.StreamWriter` will also
	// satisfy `server.ResponseWriterStream`. The type assertion
	// `stream.(server.ResponseWriterStream)` will only result in `ok == false` if `stream` is `nil`.
	// The `nil` case is covered by "InvalidStream_NilStream_Panics".
	//
	// To test the `if !ok` block in `dispatchRequest` for a non-nil stream that somehow
	// fails the type assertion, the interfaces would need to be genuinely different,
	// or `server.dispatchRequest` would need to be refactored for mockability of the
	// type assertion itself. As `server.go` stands, this specific scenario is not
	// practically constructible for a non-nil stream.
	/*
		t.Run("InvalidStream_TypeMismatch_LogsAndAttempts500", func(t *testing.T) {
			t.Skip("Skipping: Difficult to construct a non-nil http2.StreamWriter that fails server.ResponseWriterStream assertion due to identical interface structures.")

			// Hypothetical test structure if such a stream could be created:
			// logBuf := &bytes.Buffer{}
			// mockLog := newMockLogger(logBuf)
			// mockRtr := newMockRouter()
			// s := newTestServer(t, mockLog, mockRtr)

			// // 1. Create 'faultyStream': a non-nil http2.StreamWriter that would cause
			// //    `faultyStream.(server.ResponseWriterStream)` to return `_ , false`.
			// //    This mock would need to capture SendHeaders and WriteData calls.
			// var faultyStream http2.StreamWriter // = newMockFaultyStream()
			// req, _ := http.NewRequest("GET", "/test-faulty", nil)

			// s.dispatchRequest(faultyStream, req)

			// // 2. Assert router was NOT called
			// if mockRtr.GetServeHTTPCallCount() > 0 {
			// 	t.Errorf("Expected router.ServeHTTP not to be called, got %d", mockRtr.GetServeHTTPCallCount())
			// }

			// // 3. Assert error was logged
			// if !strings.Contains(logBuf.String(), "does not implement server.ResponseWriterStream") {
			// 	t.Errorf("Expected log message about stream type mismatch, got: %s", logBuf.String())
			// }

			// // 4. Assert 500-like response was attempted on faultyStream
			// //    (e.g., check headers sent to faultyStream for :status 500, and body content)
		})
	*/
}

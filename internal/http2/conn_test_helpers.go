package http2

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"example.com/llmahttap/v2/internal/logger"
)


// mockNetConn is a mock implementation of net.Conn for testing.

type mockNetConn struct {
	mu       sync.Mutex
	readBuf  bytes.Buffer
	writeBuf bytes.Buffer

	readDeadline  time.Time
	writeDeadline time.Time
	closed        bool
	closeErr      error
	localAddr     net.Addr
	remoteAddr    net.Addr
	readHook      func([]byte) (int, error)
	writeHook     func([]byte) (int, error)

	// For controlled blocking Read
	readDataAvailable chan struct{} // Signals new data in readBuf or conn closed
	readClosed        chan struct{} // Signals Read should unblock due to Close()
}

func newMockNetConn() *mockNetConn {
	return &mockNetConn{
		localAddr:         &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345},
		remoteAddr:        &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54321},
		closeErr:          errors.New("use of closed network connection"),
		readDataAvailable: make(chan struct{}, 1), // Buffered to allow Feed before first Read
		readClosed:        make(chan struct{}),
	}
}

func (m *mockNetConn) Read(b []byte) (n int, err error) {
	for { // Loop to re-evaluate after waking from readDataAvailable
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return 0, m.closeErr
		}
		if !m.readDeadline.IsZero() && time.Now().After(m.readDeadline) {
			m.mu.Unlock()
			return 0, errors.New("read timeout (mock)")
		}

		// 1. Try readBuf first
		if m.readBuf.Len() > 0 {
			n, err = m.readBuf.Read(b)
			// If buffer still has data after this read, signal for next Read if channel is empty.
			if m.readBuf.Len() > 0 {
				select {
				case m.readDataAvailable <- struct{}{}:
				default:
				}
			}
			m.mu.Unlock()
			return n, err
		}

		// 2. If readBuf is empty, then check for readHook
		if m.readHook != nil {
			hook := m.readHook
			m.mu.Unlock()
			return hook(b)
		}

		// 3. If readBuf empty and no hook, then block.
		// Unlock before blocking select.
		m.mu.Unlock()

		select {
		case <-m.readDataAvailable:
			// Loop back to top to re-check conditions under lock
			continue
		case <-m.readClosed:
			// Connection closed. Re-lock to check buffer one last time.
			m.mu.Lock()
			if m.readBuf.Len() > 0 { // Process any lingering data
				n, err = m.readBuf.Read(b)
				m.mu.Unlock()
				return n, err
			}
			closedErr := m.closeErr
			m.mu.Unlock()
			return 0, closedErr
		case <-time.After(5 * time.Second): // Test failsafe timeout
			return 0, errors.New("mockNetConn.Read test failsafe timeout (5s) - blocked waiting for data/close")
		}
	}
}

func (m *mockNetConn) Write(b []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, m.closeErr
	}
	if !m.writeDeadline.IsZero() && time.Now().After(m.writeDeadline) {
		return 0, errors.New("write timeout (mock)")
	}
	if m.writeHook != nil {
		return m.writeHook(b)
	}
	return m.writeBuf.Write(b)
}

func (m *mockNetConn) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	// Default m.closeErr is "use of closed network connection".
	// If a more specific error is needed for a test, it can be set on m.closeErr before Close() is called externally,
	// or Close() can be modified to accept an error. For now, this is standard.
	m.mu.Unlock() // Unlock before closing channel to avoid deadlock with Read

	// Signal any blocked Read calls that the connection is now closed.
	// Safely close the channel to prevent panic on multiple Close calls.
	select {
	case <-m.readClosed:
		// Already closed or being closed by another goroutine.
	default:
		close(m.readClosed)
	}
	// Also signal readDataAvailable in case Read is blocked there,
	// so it can re-check m.closed.
	select {
	case m.readDataAvailable <- struct{}{}:
	default:
	}
	return nil
}

func (m *mockNetConn) LocalAddr() net.Addr  { return m.localAddr }
func (m *mockNetConn) RemoteAddr() net.Addr { return m.remoteAddr }

func (m *mockNetConn) SetDeadline(t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readDeadline = t
	m.writeDeadline = t
	return nil
}

func (m *mockNetConn) SetReadDeadline(t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readDeadline = t
	return nil
}

func (m *mockNetConn) SetWriteDeadline(t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeDeadline = t
	return nil
}

func (m *mockNetConn) FeedReadBuffer(data []byte) {
	m.mu.Lock()
	m.readBuf.Write(data)
	m.mu.Unlock() // Unlock before channel send

	// Signal that data is available.
	// Non-blocking send to prevent FeedReadBuffer from deadlocking if Read is not ready
	// or if channel buffer is full (it's size 1, so one pending signal is okay).
	if len(data) > 0 { // Only signal if actual data was added
		select {
		case m.readDataAvailable <- struct{}{}:
			// Successfully signaled
		default:
			// Channel was full or no reader ready. This is fine, Read will eventually see the data
			// when it tries to read from the buffer or if it's already blocked on readDataAvailable.
			// Or, if Read is currently processing data from a previous signal, this new signal
			// might be dropped if the channel buffer (size 1) is full. Read's loop logic
			// should handle re-checking readBuf.Len() after processing.
		}
	}
}

func (m *mockNetConn) GetWriteBufferBytes() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	data := make([]byte, m.writeBuf.Len())
	copy(data, m.writeBuf.Bytes())
	return data
}

func (m *mockNetConn) GetWriteBufferLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeBuf.Len()
}

func (m *mockNetConn) ResetWriteBuffer() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeBuf.Reset()
}

func (m *mockNetConn) IsClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

type mockRequestDispatcher struct {
	mu           sync.Mutex
	called       bool
	calledCount  int
	lastStreamID uint32
	lastReq      *http.Request
	lastSw       StreamWriter
	fn           func(sw StreamWriter, req *http.Request)
}

func (mrd *mockRequestDispatcher) Dispatch(sw StreamWriter, req *http.Request) {
	mrd.mu.Lock()
	mrd.called = true
	mrd.calledCount++
	mrd.lastStreamID = sw.ID()
	mrd.lastSw = sw
	mrd.lastReq = req
	dispatchFn := mrd.fn
	mrd.mu.Unlock()
	if dispatchFn != nil {
		dispatchFn(sw, req)
	}
}
func (mrd *mockRequestDispatcher) Reset() {
	mrd.mu.Lock()
	defer mrd.mu.Unlock()
	mrd.called = false
	mrd.calledCount = 0
	mrd.lastStreamID = 0
	mrd.lastReq = nil
	mrd.lastSw = nil
	mrd.fn = nil
}

func (mrd *mockRequestDispatcher) CalledCount() int {
	mrd.mu.Lock()
	defer mrd.mu.Unlock()
	return mrd.calledCount
}

func newTestConnection(t *testing.T, isClient bool, mockDispatcher *mockRequestDispatcher) (*Connection, *mockNetConn) {
	t.Helper()
	mnc := newMockNetConn()
	lg := logger.NewTestLogger(os.Stdout)
	// lg.SetLevel(logger.LevelDebug) // Uncomment for verbose debug output

	var dispatcherFunc RequestDispatcherFunc
	if mockDispatcher != nil {
		dispatcherFunc = mockDispatcher.Dispatch
	} else {
		dispatcherFunc = func(sw StreamWriter, req *http.Request) { /* No-op */ }
	}

	conn := NewConnection(mnc, lg, isClient, nil, dispatcherFunc)
	if conn == nil {
		t.Fatal("NewConnection returned nil")
	}
	// Give writerLoop a chance to start, especially important for handshake.
	time.Sleep(100 * time.Millisecond)
	return conn, mnc
}

func frameToBytes(t *testing.T, frame Frame) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteFrame(&buf, frame); err != nil {
		t.Fatalf("frameToBytes: Error writing frame: %v", err)
	}
	return buf.Bytes()
}

func bytesToFrame(t *testing.T, data []byte) Frame {
	t.Helper()
	if len(data) == 0 {
		return nil
	}
	r := bytes.NewReader(data)
	frame, err := ReadFrame(r)
	if err != nil {
		if errors.Is(err, io.EOF) || (errors.Is(err, io.ErrUnexpectedEOF) && r.Len() == 0 && len(data) < FrameHeaderLen) {
			return nil
		}
		t.Fatalf("bytesToFrame: Error reading frame: %v. Data (hex): %s", err, hex.EncodeToString(data))
	}
	return frame
}

func readAllFramesFromBuffer(t *testing.T, data []byte) []Frame {
	t.Helper()
	var frames []Frame
	r := bytes.NewReader(data)
	// Log the raw data being processed
	t.Logf("readAllFramesFromBuffer: START. Processing %d bytes. Data (hex): %s", len(data), hex.EncodeToString(data))

	initialLen := r.Len()

	for r.Len() > 0 {
		currentPos := initialLen - r.Len()
		t.Logf("readAllFramesFromBuffer: Loop Iteration. r.Len() = %d (at offset %d of %d)", r.Len(), currentPos, initialLen)

		// Peek at frame header bytes without consuming them from main reader 'r'
		// This is to log the frame type/length we are *about* to parse.
		if r.Len() >= FrameHeaderLen {
			headerBytes := make([]byte, FrameHeaderLen)
			// Create a new reader for peeking to not affect 'r's position
			peekReader := bytes.NewReader(data[currentPos:])
			_, errPeek := io.ReadFull(peekReader, headerBytes)
			if errPeek == nil {
				peekedLength := (uint32(headerBytes[0])<<16 | uint32(headerBytes[1])<<8 | uint32(headerBytes[2]))
				peekedType := FrameType(headerBytes[3])
				peekedFlags := Flags(headerBytes[4])
				peekedStreamID := binary.BigEndian.Uint32(headerBytes[5:9]) & 0x7FFFFFFF
				t.Logf("readAllFramesFromBuffer: PEEK. About to parse frame starting at offset %d. Header indicates: Type=%s, Length=%d, Flags=%d, StreamID=%d",
					currentPos, peekedType, peekedLength, peekedFlags, peekedStreamID)
			} else {
				t.Logf("readAllFramesFromBuffer: PEEK. Failed to peek at frame header (not enough bytes or other error): %v", errPeek)
			}
		} else {
			t.Logf("readAllFramesFromBuffer: PEEK. Not enough bytes remaining (%d) to form a full frame header (need %d).", r.Len(), FrameHeaderLen)
		}

		frame, err := ReadFrame(r) // This ReadFrame uses 'r' and consumes data.
		if err != nil {
			t.Logf("readAllFramesFromBuffer: ReadFrame error: %v. r.Len() after error: %d. Offset where error occurred: %d", err, r.Len(), currentPos)
			if errors.Is(err, io.EOF) {
				t.Logf("readAllFramesFromBuffer: EOF encountered, breaking loop.")
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				t.Logf("readAllFramesFromBuffer: Unexpected EOF, likely incomplete frame. Original data length: %d, data processed up to this point (approx): %d. Error: %v", len(data), currentPos, err)
				break
			}
			// For other errors, log and break. Depending on test, might not want t.Fatalf here
			// as it would stop the test from checking what *was* parsed.
			t.Logf("readAllFramesFromBuffer: Non-EOF/UnexpectedEOF error reading frame: %v. Raw data being parsed: %s. Parsed frames so far: %d", err, hex.EncodeToString(data), len(frames))
			break // Stop parsing on other errors
		}
		if frame == nil { // Should not happen if err is nil
			t.Logf("readAllFramesFromBuffer: ReadFrame returned nil frame AND nil error. This is unexpected. Breaking.")
			break
		}

		t.Logf("readAllFramesFromBuffer: Successfully parsed frame. Type: %s, StreamID: %d, Length: %d, Flags: %d. r.Len() after parse: %d",
			frame.Header().Type, frame.Header().StreamID, frame.Header().Length, frame.Header().Flags, r.Len())
		frames = append(frames, frame)

	}
	t.Logf("readAllFramesFromBuffer: END. Parsed %d frames. Data remaining in reader: %d", len(frames), r.Len())
	return frames
}

func waitForCondition(t *testing.T, timeout time.Duration, interval time.Duration, condition func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("Timeout waiting for condition: %s", msg)
}

// waitForFrameCondition polls for a specific frame type and condition.
// It returns the found frame or nil if timeout.
func waitForFrameCondition[F Frame](t *testing.T, timeout time.Duration, interval time.Duration, mnc *mockNetConn, frameType F, condition func(f F) bool, msg string) F {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var zeroValue F // To return if not found
	for time.Now().Before(deadline) {
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		for _, fr := range frames {
			if typedFrame, ok := fr.(F); ok {
				if condition(typedFrame) {
					return typedFrame
				}
			}
		}
		time.Sleep(interval)
	}
	t.Fatalf("Timeout waiting for frame condition: %s. Last frames seen: %+v", msg, readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes()))
	return zeroValue
}

// getErrorCode is a helper for logging, extracts ErrorCode if available.
func getErrorCode(f Frame) interface{} {
	if f == nil || f.Header() == nil {
		return "nil_frame_or_header"
	}
	switch frame := f.(type) {
	case *GoAwayFrame:
		return frame.ErrorCode.String()
	case *RSTStreamFrame:
		return frame.ErrorCode.String()
	default:
		return "N/A"
	}
}

// performHandshakeForTest performs the client-side actions and server-side handshake for tests.
func performHandshakeForTest(t *testing.T, conn *Connection, mnc *mockNetConn) {
	t.Helper()
	// Client sends preface
	mnc.FeedReadBuffer([]byte(ClientPreface))

	// Client sends its initial SETTINGS
	clientSettings := []Setting{{ID: SettingInitialWindowSize, Value: DefaultSettingsInitialWindowSize}}
	clientSettingsFrame := &SettingsFrame{
		FrameHeader: FrameHeader{Type: FrameSettings, StreamID: 0, Length: uint32(len(clientSettings) * 6)},
		Settings:    clientSettings,
	}
	mnc.FeedReadBuffer(frameToBytes(t, clientSettingsFrame))

	// Server performs handshake
	if err := conn.ServerHandshake(); err != nil {
		t.Fatalf("performHandshakeForTest: ServerHandshake failed: %v", err)
	}

	// Verify server sent its SETTINGS and an ACK to client's SETTINGS
	waitForCondition(t, 2*time.Second, 50*time.Millisecond, func() bool {
		conn.log.Debug("performHandshakeForTest: In waitForCondition check for server SETTINGS and ACK.", nil)
		frames := readAllFramesFromBuffer(t, mnc.GetWriteBufferBytes())
		if len(frames) < 2 {
			conn.log.Debug("performHandshakeForTest: Not enough frames yet.", logger.LogFields{"count": len(frames)})
			return false
		}
		_, ok1 := frames[0].(*SettingsFrame)
		sf2, ok2 := frames[1].(*SettingsFrame)
		if !ok1 || !ok2 {
			conn.log.Debug("performHandshakeForTest: Frame types incorrect.", logger.LogFields{"ok1": ok1, "ok2": ok2})
			return false
		}
		isAck := (sf2.Header().Flags & FlagSettingsAck) != 0
		conn.log.Debug("performHandshakeForTest: Frames found.", logger.LogFields{"isAck": isAck})
		return isAck
	}, "server to write its SETTINGS and ACK client's SETTINGS")

	mnc.ResetWriteBuffer() // Clear handshake frames from server's write buffer
}

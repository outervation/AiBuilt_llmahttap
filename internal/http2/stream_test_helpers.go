package http2

import (
	"fmt"

	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

// mockConnection and its methods were removed as stream tests now use a real http2.Connection
// initialized with a mockNetConn (from conn_test.go) and a mockRequestDispatcher (also from conn_test.go).
// Helper functions like newTestConnection from conn_test.go are used for setup.

// removeClosedStream is a mock method to simulate Connection.removeClosedStream
// This function will likely be removed or adapted as mockConnection is removed.
// For now, it's a placeholder if any test still relies on this specific mock behavior.
// func (mc *mockConnection) removeClosedStream(s *Stream) {
// 	mc.mu.Lock()
// 	defer mc.mu.Unlock()
// 	mc.removeClosedStreamCallCount++
// 	mc.lastStreamRemovedByClose = s
// }

// mockRequestDispatcher is a mock for the RequestDispatcherFunc.

// newTestStream is a helper function to initialize a http2.Stream for testing.

func newTestStream(t *testing.T, id uint32, conn *Connection, isInitiatedByPeer bool, initialOurWindow, initialPeerWindow uint32) *Stream {
	t.Helper()

	// Use default priority for most stream tests, can be overridden if specific priority tests are needed.
	defaultWeight := uint8(15) // Default priority weight (normalized from spec 16)
	defaultParentID := uint32(0)
	defaultExclusive := false

	// Ensure the connection passed is not nil, as it's crucial.
	if conn == nil {
		t.Fatal("newTestStream: provided conn is nil")
	}
	if conn.log == nil {
		t.Fatal("newTestStream: conn.log is nil, connection not properly initialized for test")
	}
	if conn.ctx == nil {
		// If conn.ctx is nil, it means the connection itself wasn't fully initialized.
		// newTestConnection should set this up.
		t.Fatal("newTestStream: conn.ctx is nil. Connection likely not from newTestConnection.")
	}

	// Create the stream using the real newStream function and the provided real Connection.
	ourWin := initialOurWindow
	if ourWin == 0 { // If 0, use the connection's configured initial window size
		ourWin = conn.ourInitialWindowSize
	}
	if ourWin == 0 { // If still 0 (e.g. conn's default was also 0, or conn not fully setup), use http2 default
		ourWin = DefaultInitialWindowSize
	}

	peerWin := initialPeerWindow

	t.Logf("newTestStream: Creating stream %d with ourInitialWin=%d, peerInitialWin=%d. (conn.ourInitialWindowSize=%d, conn.peerInitialWindowSize=%d)",
		id, ourWin, peerWin, conn.ourInitialWindowSize, conn.peerInitialWindowSize)

	s, err := newStream(
		conn, // Use the real *http2.Connection
		id,
		ourWin,
		peerWin,
		defaultWeight,
		defaultParentID,
		defaultExclusive,
		isInitiatedByPeer,
	)
	if err != nil {
		// Log connection state for debugging
		conn.settingsMu.RLock()
		ourSettingsDump := make(map[SettingID]uint32)
		for k, v := range conn.ourSettings {
			ourSettingsDump[k] = v
		}
		peerSettingsDump := make(map[SettingID]uint32)
		for k, v := range conn.peerSettings {
			peerSettingsDump[k] = v
		}
		conn.settingsMu.RUnlock()

		t.Fatalf("newTestStream: newStream() failed for stream %d: %v. \n"+
			"Connection details: ourInitialWindowSize=%d, peerInitialWindowSize=%d, maxFrameSize=%d. \n"+
			"Our Settings: %v. Peer Settings: %v",
			id, err, conn.ourInitialWindowSize, conn.peerInitialWindowSize, conn.maxFrameSize,
			ourSettingsDump, peerSettingsDump)
	}

	t.Logf("newTestStream created stream %d: fcManager receiveWindow=%d, sendWindow=%d. State=%s",
		s.id, s.fcManager.GetStreamReceiveAvailable(), s.fcManager.GetStreamSendAvailable(), s.state)

	t.Cleanup(func() {
		closeErr := fmt.Errorf("test stream %d cleanup", s.id)
		if s.state != StreamStateClosed { // Avoid error if already closed
			if err := s.Close(closeErr); err != nil {
				// Log error but don't fail test if it's about already closed stream
				if !strings.Contains(err.Error(), "stream closed or being reset") && !strings.Contains(err.Error(), "already closed") {
					t.Logf("Error during stream.Close in newTestStream cleanup for stream %d: %v", s.id, err)
				}
			}
		}
		// Drain any RST frame sent by s.Close() during cleanup from the connection's writerChan.
		// This uses s.conn which is a real *Connection.

		// Drain conn.writerChan if writerLoop isn't running.
		// Loop to drain multiple frames, as stream.Close() might be preceded by other frames in some tests.
	drainLoop:
		for i := 0; i < 5; i++ { // Limit iterations to prevent infinite loop in weird cases
			select {
			case frame, ok := <-s.conn.writerChan:
				if ok {
					t.Logf("Drained frame from conn.writerChan during stream %d cleanup (iter %d): Type %s, StreamID %d", s.id, i, frame.Header().Type, frame.Header().StreamID)
				} else {
					t.Logf("conn.writerChan closed during stream %d cleanup.", s.id)
					break drainLoop
				}
			case <-time.After(10 * time.Millisecond): // Short timeout per frame
				// Assume channel is empty if we time out
				break drainLoop
			}
		}
	}) // End of t.Cleanup
	return s
}

// Helper to create default hpack.HeaderFields for testing
func makeHpackHeaders(kv ...string) []hpack.HeaderField {
	if len(kv)%2 != 0 {
		panic("makeHpackHeaders: odd number of kv args")
	}
	hfs := make([]hpack.HeaderField, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		hfs = append(hfs, hpack.HeaderField{Name: kv[i], Value: kv[i+1]})
	}
	return hfs
}

// Helper to create default http2.HeaderFields for testing
func makeStreamWriterHeaders(kv ...string) []HeaderField {
	if len(kv)%2 != 0 {
		panic("makeStreamWriterHeaders: odd number of kv args")
	}
	hfs := make([]HeaderField, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		hfs = append(hfs, HeaderField{Name: kv[i], Value: kv[i+1]})
	}
	return hfs
}

// Helper functions for t.Logf, t.Errorf, t.Fatalf to avoid data races on t
// if tests are run in parallel (though these unit tests are not by default).
// More importantly, it makes them callable from goroutines if needed.
func tLogf(t *testing.T, format string, args ...interface{}) {
	t.Helper()
	t.Logf(format, args...)
}
func tErrorf(t *testing.T, format string, args ...interface{}) {
	t.Helper()
	t.Errorf(format, args...)
}
func tFatalf(t *testing.T, format string, args ...interface{}) {
	t.Helper()
	t.Fatalf(format, args...)
}
func tError(t *testing.T, args ...interface{}) {
	t.Helper()
	t.Error(args...)
}

// newDataFrame is a helper to create DataFrame instances for testing.
func newDataFrame(streamID uint32, data []byte, endStream bool) *DataFrame {
	fh := FrameHeader{
		Length:   uint32(len(data)),
		Type:     FrameData,
		StreamID: streamID,
	}
	if endStream {
		fh.Flags |= FlagDataEndStream
	}
	return &DataFrame{
		FrameHeader: fh,
		Data:        data,
		// Padding and PadLength are not used by handleDataFrame directly, so omitted for simplicity
	}
}

package http2

import (
	"testing"

	"github.com/stretchr/testify/require" // Added
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
	// initialOurWindow: desired receive window for this stream (how much peer can send us).
	// initialPeerWindow: desired send window for this stream (how much we can send peer).

	// Default priority values for newStream
	const defaultPrioWeight = uint8(16 - 1) // Normalized for spec default 16
	const defaultPrioParentID = uint32(0)
	const defaultPrioExclusive = false

	s, err := newStream(conn, id, initialOurWindow, initialPeerWindow, defaultPrioWeight, defaultPrioParentID, defaultPrioExclusive, isInitiatedByPeer)
	require.NoError(t, err)

	// The stream's fcManager is initialized by newStream using initialOurWindow and initialPeerWindow.
	// initialOurWindow -> s.fcManager.receiveWindow.effectiveInitialReceiveWindowSize and s.fcManager.currentReceiveWindowSize
	// initialPeerWindow -> s.fcManager.sendWindow.initialWindowSize and s.fcManager.sendWindow.available

	// Verify initialization if needed (example, can be removed if tests cover this elsewhere)
	// require.Equal(t, int64(initialOurWindow), s.fcManager.GetStreamReceiveAvailable(), "Stream receive window mismatch after newStream")
	// require.Equal(t, int64(initialPeerWindow), s.fcManager.GetStreamSendAvailable(), "Stream send window mismatch after newStream")

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

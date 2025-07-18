package http2

import (
	"bytes"
	"encoding/hex" // Added import
	"testing"
	"time"
)

// Helper for hex dumping, useful in debugging
func hexDump(data []byte) string {
	var buf bytes.Buffer
	dumper := hex.Dumper(&buf)
	_, _ = dumper.Write(data)
	_ = dumper.Close()
	return buf.String()
}

// TestMain is explicitly disallowed.

func TestInvalidClientPreface(t *testing.T) {
	t.Parallel()
	// Step 1: Set up a server-side http2.Connection using newTestConnection and a mockNetConn.
	conn, mnc := newTestConnection(t, false, nil, nil) // Server-side, no specific dispatcher or peer settings needed for this test initially
	defer conn.Close(nil)
	defer mnc.Close()

	// Step 2: Simulate a client sending an invalid connection preface.
	invalidPreface := "PRI * HTTP/2.0\r\n\r\nSM\r\nBAD" // Truncated and incorrect
	mnc.FeedReadBuffer([]byte(invalidPreface))

	// Step 3: Call conn.ServerHandshake() to trigger preface processing.
	// Step 4: Assert that conn.ServerHandshake() returns a ConnectionError with ErrorCodeProtocolError.
	err := conn.ServerHandshake()
	if err == nil {
		t.Fatalf("ServerHandshake did not return an error for invalid preface")
	}

	connErr, ok := err.(*ConnectionError)
	if !ok {
		t.Fatalf("ServerHandshake error is not of type *ConnectionError, got %T: %v", err, err)
	}

	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ConnectionError with code %s, got %s", ErrCodeProtocolError, connErr.Code)
	}
	t.Logf("ServerHandshake returned expected ConnectionError: %v", connErr)

	// Explicitly close the connection with the error from ServerHandshake
	// This is necessary because ServerHandshake itself doesn't close the connection on error;
	// it returns the error, and the caller is responsible for closing.
	closeErr := conn.Close(connErr)
	// conn.Close is idempotent and should return the error that initiated the shutdown (connErr in this case).
	// If closeErr is different and not nil, it might indicate a problem in Close(), but for this test's primary
	// goal, we focus on connErr from ServerHandshake leading to the GOAWAY and mnc closure.
	if closeErr != nil && closeErr != connErr {
		// This condition might be too strict if conn.Close can wrap connErr.
		// For now, just log if they are different and non-nil.
		t.Logf("conn.Close(connErr) returned: %v. Initial error was %v.", closeErr, connErr)
	}

	// Step 5: Assert that the mockNetConn's write buffer contains a GOAWAY frame

	// Step 5: Assert that the mockNetConn's write buffer contains a GOAWAY frame with ErrorCodeProtocolError and LastStreamID of 0.
	goAwayFrame := waitForFrameCondition(t, 2*time.Second, 50*time.Millisecond, mnc, (*GoAwayFrame)(nil), func(f *GoAwayFrame) bool {
		// Log details of frames found during polling for better debugging if condition isn't met.
		if f == nil { // Should not happen if called by waitForFrameCondition for a found frame
			return false
		}
		t.Logf("waitForFrameCondition: Checking frame: Type=%s, StreamID=%d, ErrorCode=%s, LastStreamID=%d",
			f.Header().Type, f.Header().StreamID, f.ErrorCode, f.LastStreamID)
		return f.ErrorCode == ErrCodeProtocolError && f.LastStreamID == 0
	}, "GOAWAY frame with PROTOCOL_ERROR and LastStreamID 0")

	// waitForFrameCondition will t.Fatalf if the frame is not found or condition not met.
	// So, if we reach here, goAwayFrame is not nil and met the condition.
	t.Logf("Successfully found GOAWAY frame with ErrorCode: %s, LastStreamID: %d", goAwayFrame.ErrorCode, goAwayFrame.LastStreamID)

	// Step 6: Assert that the mockNetConn is eventually closed by the server.
	// The ServerHandshake, upon detecting an invalid preface, calls c.Close(),
	// which should lead to the underlying net.Conn (our mnc) being closed.
	waitForCondition(t, 2*time.Second, 50*time.Millisecond, mnc.IsClosed, "mockNetConn to be closed by server")
	t.Logf("mockNetConn successfully closed by server.")
}

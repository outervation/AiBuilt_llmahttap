package http2

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewConnectionFlowControlManager(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	require.NotNil(t, cfcm)
	require.NotNil(t, cfcm.sendWindow)
	assert.Equal(t, int64(DefaultInitialWindowSize), cfcm.sendWindow.Available())
	assert.True(t, cfcm.sendWindow.isConnection)
	assert.Equal(t, uint32(0), cfcm.sendWindow.streamID)

	assert.Equal(t, int64(DefaultInitialWindowSize), cfcm.currentReceiveWindowSize)
	assert.Equal(t, DefaultInitialWindowSize/2, cfcm.windowUpdateThreshold)
	assert.Equal(t, uint64(0), cfcm.bytesConsumedTotal)
	assert.Equal(t, uint64(0), cfcm.lastWindowUpdateSentAt)
	assert.Equal(t, uint64(0), cfcm.totalBytesReceived)

	// Test threshold adjustment for small initial size
	// To test this properly, NewConnectionFlowControlManager would need to accept the initial size
	// or a test helper would be needed. For now, we assume DefaultInitialWindowSize is respected
	// and the threshold logic `initialSize / 2` (with min 1) is correct.
	// Example of how it could be tested if constructor was adaptable:
	// cfcmSmall := NewConnectionFlowControlManagerWithInitialSize(1) // Imaginary constructor
	// assert.Equal(t, uint32(1), cfcmSmall.windowUpdateThreshold, "Threshold should be 1 if initial size is 1")

	// Test with a DefaultInitialWindowSize that is 0 or 1 if that's possible and makes sense.
	// Based on current NewConnectionFlowControlManager, it uses the global DefaultInitialWindowSize.
	// If DefaultInitialWindowSize is 0, threshold is 0. If 1, threshold is 1.
	// Let's test the case when DefaultInitialWindowSize might be set to 1 globally (though unlikely).
	// If DefaultInitialWindowSize is 1:
	// tempDefault := DefaultInitialWindowSize
	// DefaultInitialWindowSize = 1 // Can't do this to a const
	// cfcmOne := NewConnectionFlowControlManager()
	// assert.Equal(t, uint32(1), cfcmOne.windowUpdateThreshold)
	// DefaultInitialWindowSize = tempDefault // Restore

	// Current implementation of NewConnectionFlowControlManager uses DefaultInitialWindowSize.
	// If DefaultInitialWindowSize is >=2, threshold is DefaultInitialWindowSize/2.
	// If DefaultInitialWindowSize is 1, threshold is 1.
	// If DefaultInitialWindowSize is 0, threshold is 0.
	// This logic is covered by the main assertion: assert.Equal(t, DefaultInitialWindowSize/2, cfcm.windowUpdateThreshold)
	// and the subsequent check in NewConnectionFlowControlManager:
	// if cfcm.windowUpdateThreshold == 0 && initialSize > 0 { cfcm.windowUpdateThreshold = 1 }
	// This means if initialSize is 1, threshold will become 1.

	// To directly test the threshold adjustment for initial size 1:
	// Need to simulate NewConnectionFlowControlManager with initialSize=1
	// The existing code handles this:
	// if DefaultInitialWindowSize is 1, then initialSize = 1, threshold = 1/2 = 0.
	// Then, `if cfcm.windowUpdateThreshold == 0 && initialSize > 0` (0 == 0 && 1 > 0) is true, so threshold becomes 1.
	// This seems correct.
}

func TestConnectionFlowControlManager_AcquireSendSpace_Simple(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	initialSendWindow := cfcm.sendWindow.Available()

	// Acquire less than available
	err := cfcm.AcquireSendSpace(100)
	require.NoError(t, err)
	assert.Equal(t, initialSendWindow-100, cfcm.sendWindow.Available())

	// Acquire remaining
	err = cfcm.AcquireSendSpace(uint32(initialSendWindow - 100))
	require.NoError(t, err)
	assert.Equal(t, int64(0), cfcm.sendWindow.Available())
}

func TestConnectionFlowControlManager_AcquireSendSpace_BlocksAndUnblocks(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	// Exhaust the window
	err := cfcm.AcquireSendSpace(DefaultInitialWindowSize)
	require.NoError(t, err)
	assert.Equal(t, int64(0), cfcm.sendWindow.Available())

	var wg sync.WaitGroup
	wg.Add(1)
	acquired := false
	var acquireErr error

	go func() {
		defer wg.Done()
		// This should block
		acquireErr = cfcm.AcquireSendSpace(100)
		acquired = true
	}()

	// Give goroutine a chance to block
	time.Sleep(50 * time.Millisecond)
	assert.False(t, acquired, "AcquireSendSpace should be blocking")

	// Increase window
	err = cfcm.HandleWindowUpdateFromPeer(200)
	require.NoError(t, err)

	wg.Wait() // Wait for goroutine to complete
	assert.True(t, acquired, "AcquireSendSpace should have unblocked")
	require.NoError(t, acquireErr)
	assert.Equal(t, int64(100), cfcm.sendWindow.Available()) // 200 - 100 = 100
}

func TestConnectionFlowControlManager_AcquireSendSpace_ErrorOnClosed(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	closeErr := fmt.Errorf("test close error")
	cfcm.Close(closeErr)

	err := cfcm.AcquireSendSpace(100)
	require.Error(t, err)
	// The error from sendWindow.Close is what's propagated
	assert.EqualError(t, err, "test close error") // The specific error passed to Close

}

func TestConnectionFlowControlManager_AcquireSendSpace_ZeroAcquire(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	initialSendWindow := cfcm.sendWindow.Available()

	err := cfcm.AcquireSendSpace(0)
	require.NoError(t, err, "Acquiring zero bytes should not error")
	assert.Equal(t, initialSendWindow, cfcm.sendWindow.Available(), "Acquiring zero bytes should not change window")
}

func TestConnectionFlowControlManager_HandleWindowUpdateFromPeer(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	initialSendWindow := cfcm.sendWindow.Available()

	err := cfcm.HandleWindowUpdateFromPeer(1000)
	require.NoError(t, err)
	assert.Equal(t, initialSendWindow+1000, cfcm.sendWindow.Available())
}

func TestConnectionFlowControlManager_HandleWindowUpdateFromPeer_Overflow(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	// Set available to near MaxWindowSize
	cfcm.sendWindow.available = MaxWindowSize - 100

	err := cfcm.HandleWindowUpdateFromPeer(200) // This will cause overflow
	require.Error(t, err)
	_, ok := err.(*ConnectionError)
	assert.True(t, ok, "Error should be a ConnectionError")
	if ok {
		connErr := err.(*ConnectionError)
		assert.Equal(t, ErrCodeFlowControlError, connErr.Code)
	}
	assert.Contains(t, err.Error(), "flow control window (conn: true, stream: 0) would overflow")
	// Window should be marked as errored (and thus closed by setErrorLocked)
	assert.Error(t, cfcm.sendWindow.err)
	assert.True(t, cfcm.sendWindow.closed)
}

func TestConnectionFlowControlManager_HandleWindowUpdateFromPeer_Closed(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	cfcm.sendWindow.Close(fmt.Errorf("closed"))

	err := cfcm.HandleWindowUpdateFromPeer(100)
	require.Error(t, err)
	assert.EqualError(t, err, "closed") // The specific error passed to Close
}

func TestConnectionFlowControlManager_DataReceived(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	initialReceiveWindow := cfcm.GetConnectionReceiveAvailable()
	initialTotalReceived := cfcm.totalBytesReceived

	// Receive some data
	err := cfcm.DataReceived(100)
	require.NoError(t, err)
	assert.Equal(t, initialReceiveWindow-100, cfcm.GetConnectionReceiveAvailable())
	assert.Equal(t, initialTotalReceived+100, cfcm.totalBytesReceived)

	// Receive more data
	err = cfcm.DataReceived(200)
	require.NoError(t, err)
	assert.Equal(t, initialReceiveWindow-100-200, cfcm.GetConnectionReceiveAvailable())
	assert.Equal(t, initialTotalReceived+100+200, cfcm.totalBytesReceived)
}

func TestConnectionFlowControlManager_DataReceived_FlowControlError(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	initialReceiveWindow := cfcm.GetConnectionReceiveAvailable()

	// Try to receive more than available
	err := cfcm.DataReceived(uint32(initialReceiveWindow + 100))
	require.Error(t, err)
	_, ok := err.(*ConnectionError)
	assert.True(t, ok, "Error should be a ConnectionError")
	if ok {
		connErr := err.(*ConnectionError)
		assert.Equal(t, ErrCodeFlowControlError, connErr.Code)
	}
	assert.Contains(t, err.Error(), "connection flow control error: received")
	// Window should not change on error
	assert.Equal(t, initialReceiveWindow, cfcm.GetConnectionReceiveAvailable())
}

func TestConnectionFlowControlManager_DataReceived_ZeroBytes(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	initialReceiveWindow := cfcm.GetConnectionReceiveAvailable()
	initialTotalReceived := cfcm.totalBytesReceived

	err := cfcm.DataReceived(0)
	require.NoError(t, err)
	assert.Equal(t, initialReceiveWindow, cfcm.GetConnectionReceiveAvailable())
	assert.Equal(t, initialTotalReceived, cfcm.totalBytesReceived)
}

func TestConnectionFlowControlManager_ApplicationConsumedData_GeneratesUpdate(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	// Simulate some data received
	require.NoError(t, cfcm.DataReceived(cfcm.windowUpdateThreshold*2))
	initialReceiveWindow := cfcm.GetConnectionReceiveAvailable()
	initialConsumedTotal := cfcm.bytesConsumedTotal

	// Consume enough to trigger update
	increment, err := cfcm.ApplicationConsumedData(cfcm.windowUpdateThreshold)
	require.NoError(t, err)
	assert.Equal(t, cfcm.windowUpdateThreshold, increment)
	assert.Equal(t, initialReceiveWindow+int64(cfcm.windowUpdateThreshold), cfcm.GetConnectionReceiveAvailable())
	assert.Equal(t, initialConsumedTotal+uint64(cfcm.windowUpdateThreshold), cfcm.bytesConsumedTotal)
	assert.Equal(t, cfcm.bytesConsumedTotal, cfcm.lastWindowUpdateSentAt) // lastUpdateSentAt updated

	// Consume more, but less than threshold now
	increment, err = cfcm.ApplicationConsumedData(cfcm.windowUpdateThreshold - 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), increment)                                    // No update expected yet
	assert.NotEqual(t, cfcm.bytesConsumedTotal, cfcm.lastWindowUpdateSentAt) // lastUpdateSentAt not updated
}

func TestConnectionFlowControlManager_ApplicationConsumedData_NoUpdateBelowThreshold(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	// Simulate some data received
	require.NoError(t, cfcm.DataReceived(cfcm.windowUpdateThreshold*2))

	// Consume less than threshold
	increment, err := cfcm.ApplicationConsumedData(cfcm.windowUpdateThreshold - 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), increment)
}

func TestConnectionFlowControlManager_ApplicationConsumedData_ZeroConsumption(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	initialReceiveWindow := cfcm.GetConnectionReceiveAvailable()
	initialConsumedTotal := cfcm.bytesConsumedTotal

	increment, err := cfcm.ApplicationConsumedData(0)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), increment)
	assert.Equal(t, initialReceiveWindow, cfcm.GetConnectionReceiveAvailable())
	assert.Equal(t, initialConsumedTotal, cfcm.bytesConsumedTotal)
}

func TestConnectionFlowControlManager_ApplicationConsumedData_ErrorConditions(t *testing.T) {
	t.Run("Exceeds MaxWindowSize for single increment", func(t *testing.T) {
		cfcm := NewConnectionFlowControlManager()
		_, err := cfcm.ApplicationConsumedData(MaxWindowSize + 1)
		require.Error(t, err)
		connErr, ok := err.(*ConnectionError)
		require.True(t, ok)
		assert.Equal(t, ErrCodeInternalError, connErr.Code)
		assert.Contains(t, err.Error(), "exceeds MaxWindowSize")
	})

	t.Run("bytesConsumedTotal overflow", func(t *testing.T) {
		cfcm := NewConnectionFlowControlManager()
		cfcm.bytesConsumedTotal = MaxWindowSize // MaxUint64 makes this test hard, use MaxInt64 for practical purposes
		_, err := cfcm.ApplicationConsumedData(MaxWindowSize)
		require.Error(t, err)
		// Log the error details for diagnostics. Using t.Name() to see which sub-test this log belongs to if test names are mangled in output.
		t.Logf("DEBUG Error in test '%s': Type: %T, Value: %v", t.Name(), err, err)
		connErr, ok := err.(*ConnectionError)
		require.True(t, ok, "Error was expected to be of type *http2.ConnectionError, but it was %T", err)
		assert.Equal(t, ErrCodeInternalError, connErr.Code)
		assert.Contains(t, err.Error(), "calculated connection WINDOW_UPDATE increment")
		assert.Contains(t, err.Error(), "exceeds MaxWindowSize")
	})

	t.Run("Receive window effective size overflow", func(t *testing.T) {
		cfcm := NewConnectionFlowControlManager()
		cfcm.currentReceiveWindowSize = MaxWindowSize - 50
		// Consuming 100 should make newCurrentReceiveWindowSize = MaxWindowSize - 50 + 100 = MaxWindowSize + 50
		_, err := cfcm.ApplicationConsumedData(100)
		require.Error(t, err)
		var connErr *ConnectionError
		isConnErr := errors.As(err, &connErr)
		require.True(t, isConnErr, "Error was expected to be a *http2.ConnectionError, but it was %T (%v)", err, err)
		if isConnErr {
			assert.Equal(t, ErrCodeInternalError, connErr.Code)
		}
		assert.Contains(t, err.Error(), "connection receive window effective size")
		assert.Contains(t, err.Error(), "exceed MaxWindowSize")
	})
}

func TestConnectionFlowControlManager_Close(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	initialSendAvail := cfcm.GetConnectionSendAvailable()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// This should block then error out when closed
		err := cfcm.AcquireSendSpace(uint32(initialSendAvail + 100))
		require.Error(t, err)
		assert.EqualError(t, err, "connection closing")
	}()

	time.Sleep(50 * time.Millisecond) // Let goroutine block

	closeErr := fmt.Errorf("connection closing")
	cfcm.Close(closeErr)

	wg.Wait() // Ensure goroutine completes and checks error

	// Further acquires should fail
	err := cfcm.AcquireSendSpace(1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection closing")

	// Check sendWindow internal state
	assert.True(t, cfcm.sendWindow.closed)
	assert.Equal(t, closeErr, cfcm.sendWindow.err)
}

func TestConnectionFlowControlManager_Getters(t *testing.T) {
	cfcm := NewConnectionFlowControlManager()
	assert.Equal(t, int64(DefaultInitialWindowSize), cfcm.GetConnectionSendAvailable())
	assert.Equal(t, int64(DefaultInitialWindowSize), cfcm.GetConnectionReceiveAvailable())

	// Change send window
	require.NoError(t, cfcm.AcquireSendSpace(100))
	assert.Equal(t, int64(DefaultInitialWindowSize-100), cfcm.GetConnectionSendAvailable())

	// Change receive window
	require.NoError(t, cfcm.DataReceived(200))
	assert.Equal(t, int64(DefaultInitialWindowSize-200), cfcm.GetConnectionReceiveAvailable())
}

// Note: Tests for StreamFlowControlManager will be similar in structure,
// but need to account for streamID and interactions with settings changes.

const testStreamID uint32 = 1
const testOurInitialWindowSize uint32 = 65535
const testPeerInitialWindowSize uint32 = 32768

func TestNewStreamFlowControlManager(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	require.NotNil(t, sfcm)
	require.NotNil(t, sfcm.sendWindow)
	assert.Equal(t, testStreamID, sfcm.streamID)

	assert.Equal(t, int64(testPeerInitialWindowSize), sfcm.sendWindow.Available())
	assert.False(t, sfcm.sendWindow.isConnection)
	assert.Equal(t, testStreamID, sfcm.sendWindow.streamID)

	assert.Equal(t, int64(testOurInitialWindowSize), sfcm.currentReceiveWindowSize)
	assert.Equal(t, testOurInitialWindowSize, sfcm.effectiveInitialReceiveWindowSize)
	assert.Equal(t, testOurInitialWindowSize/2, sfcm.windowUpdateThreshold)
	assert.Equal(t, uint64(0), sfcm.bytesConsumedTotal)
	assert.Equal(t, uint64(0), sfcm.lastWindowUpdateSentAt)
	assert.Equal(t, uint64(0), sfcm.totalBytesReceived)

	// Test threshold adjustment for small initial size
	// Test with ourInitialWindowSize = 1, peerInitialWindowSize can be anything, e.g. 1
	sfcmSmall := NewStreamFlowControlManager(nil, testStreamID, 1 /* ourInitialWindowSize */, 1 /* peerInitialWindowSize */)
	assert.Equal(t, uint32(1), sfcmSmall.windowUpdateThreshold, "Threshold should be 1 if initial size is 1")
}

func TestStreamFlowControlManager_AcquireSendSpace_Simple(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialSendWindow := sfcm.sendWindow.Available()

	// Acquire less than available
	err := sfcm.AcquireSendSpace(100)
	require.NoError(t, err)
	assert.Equal(t, initialSendWindow-100, sfcm.sendWindow.Available())

	// Acquire remaining
	err = sfcm.AcquireSendSpace(uint32(initialSendWindow - 100))
	require.NoError(t, err)
	assert.Equal(t, int64(0), sfcm.sendWindow.Available())
}

func TestStreamFlowControlManager_AcquireSendSpace_BlocksAndUnblocks(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	// Exhaust the window
	err := sfcm.AcquireSendSpace(testPeerInitialWindowSize)
	require.NoError(t, err)
	assert.Equal(t, int64(0), sfcm.sendWindow.Available())

	var wg sync.WaitGroup
	wg.Add(1)
	acquired := false
	var acquireErr error

	go func() {
		defer wg.Done()
		// This should block
		acquireErr = sfcm.AcquireSendSpace(100)
		acquired = true
	}()

	// Give goroutine a chance to block
	time.Sleep(50 * time.Millisecond)
	assert.False(t, acquired, "AcquireSendSpace should be blocking")

	// Increase window
	err = sfcm.HandleWindowUpdateFromPeer(200)
	require.NoError(t, err)

	wg.Wait() // Wait for goroutine to complete
	assert.True(t, acquired, "AcquireSendSpace should have unblocked")
	require.NoError(t, acquireErr)
	assert.Equal(t, int64(100), sfcm.sendWindow.Available()) // 200 - 100 = 100
}

func TestStreamFlowControlManager_AcquireSendSpace_ErrorOnClosed(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	closeErr := fmt.Errorf("test close error")
	sfcm.Close(closeErr)

	err := sfcm.AcquireSendSpace(100)
	require.Error(t, err)
	assert.EqualError(t, err, "test close error") // The specific error passed to Close
}

func TestStreamFlowControlManager_AcquireSendSpace_ZeroAcquire(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialSendWindow := sfcm.sendWindow.Available()

	err := sfcm.AcquireSendSpace(0)
	require.NoError(t, err, "Acquiring zero bytes should not error")
	assert.Equal(t, initialSendWindow, sfcm.sendWindow.Available(), "Acquiring zero bytes should not change window")
}

func TestStreamFlowControlManager_HandleWindowUpdateFromPeer(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialSendWindow := sfcm.sendWindow.Available()

	err := sfcm.HandleWindowUpdateFromPeer(1000)
	require.NoError(t, err)
	assert.Equal(t, initialSendWindow+1000, sfcm.sendWindow.Available())
}

func TestStreamFlowControlManager_HandleWindowUpdateFromPeer_Overflow(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	sfcm.sendWindow.available = MaxWindowSize - 100 // Set near max

	err := sfcm.HandleWindowUpdateFromPeer(200) // This will cause overflow
	require.Error(t, err)
	streamErr, ok := err.(*StreamError)
	assert.True(t, ok, "Error should be a StreamError")
	if ok {
		assert.Equal(t, ErrCodeFlowControlError, streamErr.Code)
		assert.Equal(t, testStreamID, streamErr.StreamID)
	}
	assert.Contains(t, err.Error(), fmt.Sprintf("flow control window (conn: false, stream: %d) would overflow", testStreamID))
	assert.Error(t, sfcm.sendWindow.err)
	assert.True(t, sfcm.sendWindow.closed)
}

func TestStreamFlowControlManager_HandleWindowUpdateFromPeer_ZeroIncrementError(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	err := sfcm.HandleWindowUpdateFromPeer(0)
	require.Error(t, err)
	streamErr, ok := err.(*StreamError)
	assert.True(t, ok, "Error should be a StreamError")
	if ok {
		assert.Equal(t, ErrCodeProtocolError, streamErr.Code)
		assert.Equal(t, testStreamID, streamErr.StreamID)
	}
	assert.Contains(t, err.Error(), "WINDOW_UPDATE increment cannot be 0 for a stream")
}

func TestStreamFlowControlManager_HandleWindowUpdateFromPeer_StreamIDZeroError(t *testing.T) {
	// This test is a bit artificial, as NewStreamFlowControlManager should not be called with streamID 0.
	// However, the HandleWindowUpdateFromPeer method has a check for it.
	sfcm := &StreamFlowControlManager{streamID: 0} // Manually create to test this path
	err := sfcm.HandleWindowUpdateFromPeer(100)
	require.Error(t, err)
	connErr, ok := err.(*ConnectionError)
	require.True(t, ok)
	assert.Equal(t, ErrCodeInternalError, connErr.Code)
	assert.Contains(t, err.Error(), "StreamFlowControlManager.HandleWindowUpdateFromPeer called for stream ID 0")
}

func TestStreamFlowControlManager_HandleWindowUpdateFromPeer_Closed(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	sfcm.sendWindow.Close(fmt.Errorf("closed"))

	err := sfcm.HandleWindowUpdateFromPeer(100)
	require.Error(t, err)
	assert.EqualError(t, err, "closed") // The specific error passed to Close
}

func TestStreamFlowControlManager_DataReceived(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialReceiveWindow := sfcm.GetStreamReceiveAvailable()
	initialTotalReceived := sfcm.totalBytesReceived

	// Receive some data
	err := sfcm.DataReceived(100)
	require.NoError(t, err)
	assert.Equal(t, initialReceiveWindow-100, sfcm.GetStreamReceiveAvailable())
	assert.Equal(t, initialTotalReceived+100, sfcm.totalBytesReceived)

	// Receive more data
	err = sfcm.DataReceived(200)
	require.NoError(t, err)
	assert.Equal(t, initialReceiveWindow-100-200, sfcm.GetStreamReceiveAvailable())
	assert.Equal(t, initialTotalReceived+100+200, sfcm.totalBytesReceived)
}

func TestStreamFlowControlManager_DataReceived_FlowControlError(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialReceiveWindow := sfcm.GetStreamReceiveAvailable()

	// Try to receive more than available
	err := sfcm.DataReceived(uint32(initialReceiveWindow + 100))
	require.Error(t, err)
	streamErr, ok := err.(*StreamError)
	assert.True(t, ok, "Error should be a StreamError")
	if ok {
		assert.Equal(t, ErrCodeFlowControlError, streamErr.Code)
		assert.Equal(t, testStreamID, streamErr.StreamID)
	}
	assert.Contains(t, err.Error(), fmt.Sprintf("stream %d flow control error: received", testStreamID))
	assert.Equal(t, initialReceiveWindow, sfcm.GetStreamReceiveAvailable()) // Window should not change on error
}

func TestStreamFlowControlManager_DataReceived_ZeroBytes(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialReceiveWindow := sfcm.GetStreamReceiveAvailable()
	initialTotalReceived := sfcm.totalBytesReceived

	err := sfcm.DataReceived(0)
	require.NoError(t, err)
	assert.Equal(t, initialReceiveWindow, sfcm.GetStreamReceiveAvailable())
	assert.Equal(t, initialTotalReceived, sfcm.totalBytesReceived)
}

func TestStreamFlowControlManager_ApplicationConsumedData_GeneratesUpdate(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	// Simulate some data received
	require.NoError(t, sfcm.DataReceived(sfcm.windowUpdateThreshold*2))
	initialReceiveWindow := sfcm.GetStreamReceiveAvailable()
	initialConsumedTotal := sfcm.bytesConsumedTotal

	// Consume enough to trigger update
	increment, err := sfcm.ApplicationConsumedData(sfcm.windowUpdateThreshold)
	require.NoError(t, err)
	assert.Equal(t, sfcm.windowUpdateThreshold, increment)
	assert.Equal(t, initialReceiveWindow+int64(sfcm.windowUpdateThreshold), sfcm.GetStreamReceiveAvailable())
	assert.Equal(t, initialConsumedTotal+uint64(sfcm.windowUpdateThreshold), sfcm.bytesConsumedTotal)
	assert.Equal(t, sfcm.bytesConsumedTotal, sfcm.lastWindowUpdateSentAt) // lastUpdateSentAt updated

	// Consume more, but less than threshold now
	increment, err = sfcm.ApplicationConsumedData(sfcm.windowUpdateThreshold - 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), increment)                                    // No update expected yet
	assert.NotEqual(t, sfcm.bytesConsumedTotal, sfcm.lastWindowUpdateSentAt) // lastUpdateSentAt not updated
}

func TestStreamFlowControlManager_ApplicationConsumedData_NoUpdateBelowThreshold(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	require.NoError(t, sfcm.DataReceived(sfcm.windowUpdateThreshold*2))

	increment, err := sfcm.ApplicationConsumedData(sfcm.windowUpdateThreshold - 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), increment)
}

func TestStreamFlowControlManager_ApplicationConsumedData_ZeroConsumption(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialReceiveWindow := sfcm.GetStreamReceiveAvailable()
	initialConsumedTotal := sfcm.bytesConsumedTotal

	increment, err := sfcm.ApplicationConsumedData(0)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), increment)
	assert.Equal(t, initialReceiveWindow, sfcm.GetStreamReceiveAvailable())
	assert.Equal(t, initialConsumedTotal, sfcm.bytesConsumedTotal)
}

func TestStreamFlowControlManager_ApplicationConsumedData_ErrorConditions(t *testing.T) {
	t.Run("Exceeds MaxWindowSize for single increment", func(t *testing.T) {
		sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
		_, err := sfcm.ApplicationConsumedData(MaxWindowSize + 1)
		require.Error(t, err)
		streamErr, ok := err.(*StreamError)
		require.True(t, ok)
		assert.Equal(t, ErrCodeInternalError, streamErr.Code)
		assert.Equal(t, testStreamID, streamErr.StreamID)
		assert.Contains(t, err.Error(), "exceeds MaxWindowSize")
	})

	t.Run("Calculated increment overflow MaxWindowSize", func(t *testing.T) {
		sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
		sfcm.bytesConsumedTotal = uint64(10)
		sfcm.lastWindowUpdateSentAt = 0
		_, err := sfcm.ApplicationConsumedData(MaxWindowSize) // n is valid, but pending_increment = MaxWindowSize + 10
		require.Error(t, err)
		streamErr, ok := err.(*StreamError)
		require.True(t, ok)
		assert.Equal(t, ErrCodeInternalError, streamErr.Code)
		assert.Equal(t, testStreamID, streamErr.StreamID)
		assert.Contains(t, err.Error(), fmt.Sprintf("calculated stream %d WINDOW_UPDATE increment", testStreamID))
		assert.Contains(t, err.Error(), "exceeds MaxWindowSize")
	})

	t.Run("Receive window effective size overflow", func(t *testing.T) {
		sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
		sfcm.currentReceiveWindowSize = MaxWindowSize - 50
		_, err := sfcm.ApplicationConsumedData(100) // MaxWindowSize - 50 + 100 = MaxWindowSize + 50
		require.Error(t, err)
		streamErr, ok := err.(*StreamError)
		require.True(t, ok)
		assert.Equal(t, ErrCodeInternalError, streamErr.Code)
		assert.Equal(t, testStreamID, streamErr.StreamID)
		assert.Contains(t, err.Error(), fmt.Sprintf("internal: stream %d receive window effective size", testStreamID))
		assert.Contains(t, err.Error(), "exceed MaxWindowSize")
	})
}

func TestStreamFlowControlManager_HandlePeerSettingsInitialWindowSizeChange(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialSendAvail := sfcm.GetStreamSendAvailable() // Based on testPeerInitialWindowSize

	newPeerInitial := testPeerInitialWindowSize + 1000
	err := sfcm.HandlePeerSettingsInitialWindowSizeChange(newPeerInitial)
	require.NoError(t, err)
	// Delta is newPeerInitial - oldPeerInitial = 1000
	assert.Equal(t, initialSendAvail+1000, sfcm.GetStreamSendAvailable())
	assert.Equal(t, newPeerInitial, sfcm.sendWindow.initialWindowSize)

	// Decrease
	newPeerInitial -= 2000
	err = sfcm.HandlePeerSettingsInitialWindowSizeChange(newPeerInitial)
	require.NoError(t, err)
	// Delta is newPeerInitial - (oldPeerInitial+1000) = -2000
	assert.Equal(t, initialSendAvail+1000-2000, sfcm.GetStreamSendAvailable())
	assert.Equal(t, newPeerInitial, sfcm.sendWindow.initialWindowSize)

	// Test scenario: available becomes negative due to settings change and GetStreamSendAvailable reports it
	t.Run("send window becomes negative and GetStreamSendAvailable reports it", func(t *testing.T) {
		const subStreamID = testStreamID + 1
		const subInitialOurSize uint32 = 65535 // Example value for our side, not directly tested here
		const subInitialPeerSize uint32 = 100

		sfcmNegative := NewStreamFlowControlManager(nil, subStreamID, subInitialOurSize, subInitialPeerSize)
		require.Equal(t, int64(subInitialPeerSize), sfcmNegative.GetStreamSendAvailable(), "Initial send window should match peer's initial setting")
		require.Equal(t, subInitialPeerSize, sfcmNegative.sendWindow.initialWindowSize)

		// Acquire some data to make available != initialWindowSize
		// available = 100 - 30 = 70
		acquireAmount := uint32(30)
		err := sfcmNegative.AcquireSendSpace(acquireAmount)
		require.NoError(t, err)
		require.Equal(t, int64(subInitialPeerSize-acquireAmount), sfcmNegative.GetStreamSendAvailable()) // 70

		// Update peer's initial window size to be very small, driving available negative
		// Old initialWindowSize for sendWindow = 100. Current available = 70.
		// New peer's initial setting = 10. Delta = 10 - 100 = -90.
		// New available = current_available + delta = 70 + (-90) = -20.
		// New initialWindowSize for sendWindow becomes 10.
		newSmallPeerInitialSize := uint32(10)
		err = sfcmNegative.HandlePeerSettingsInitialWindowSizeChange(newSmallPeerInitialSize)
		require.NoError(t, err)
		assert.Equal(t, int64(-20), sfcmNegative.GetStreamSendAvailable(), "Send window should be negative after settings change")
		assert.Equal(t, newSmallPeerInitialSize, sfcmNegative.sendWindow.initialWindowSize, "sendWindow.initialWindowSize should be updated to the new small size")

		// Test scenario: available is already negative, settings change makes it more negative
		// Current available = -20. Current initialWindowSize = 10.
		// New peer's initial setting = 5. Delta = 5 - 10 = -5.
		// New available = -20 + (-5) = -25.
		newEvenSmallerPeerInitialSize := uint32(5)
		err = sfcmNegative.HandlePeerSettingsInitialWindowSizeChange(newEvenSmallerPeerInitialSize)
		require.NoError(t, err)
		assert.Equal(t, int64(-25), sfcmNegative.GetStreamSendAvailable(), "Send window should be more negative")
		assert.Equal(t, newEvenSmallerPeerInitialSize, sfcmNegative.sendWindow.initialWindowSize, "sendWindow.initialWindowSize should be updated to even smaller size")

		// Test scenario: available is negative, settings change makes it less negative (but still negative)
		// Current available = -25. Current initialWindowSize = 5.
		// New peer's initial setting = 15. Delta = 15 - 5 = 10.
		// New available = -25 + 10 = -15.
		newSlightlyLargerPeerInitialSize := uint32(15)
		err = sfcmNegative.HandlePeerSettingsInitialWindowSizeChange(newSlightlyLargerPeerInitialSize)
		require.NoError(t, err)
		assert.Equal(t, int64(-15), sfcmNegative.GetStreamSendAvailable(), "Send window should be less negative")
		assert.Equal(t, newSlightlyLargerPeerInitialSize, sfcmNegative.sendWindow.initialWindowSize, "sendWindow.initialWindowSize should be updated")

		// Test scenario: available is negative, settings change makes it positive
		// Current available = -15. Current initialWindowSize = 15.
		// New peer's initial setting = 50. Delta = 50 - 15 = 35.
		// New available = -15 + 35 = 20.
		newPositivePeerInitialSize := uint32(50)
		err = sfcmNegative.HandlePeerSettingsInitialWindowSizeChange(newPositivePeerInitialSize)
		require.NoError(t, err)
		assert.Equal(t, int64(20), sfcmNegative.GetStreamSendAvailable(), "Send window should become positive")
		assert.Equal(t, newPositivePeerInitialSize, sfcmNegative.sendWindow.initialWindowSize, "sendWindow.initialWindowSize should be updated")
	})
}

func TestStreamFlowControlManager_HandlePeerSettingsInitialWindowSizeChange_OverflowError(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, 100)
	sfcm.sendWindow.available = MaxWindowSize - 50 // Current available is high

	// New peer setting that, when delta is applied, overflows available
	// Old initial was 100. New initial is 200. Delta = 100.
	// New available = MaxWindowSize - 50 + 100 = MaxWindowSize + 50 -> overflow
	err := sfcm.HandlePeerSettingsInitialWindowSizeChange(200)
	require.Error(t, err)
	connErr, ok := err.(*ConnectionError) // This specific error comes from FlowControlWindow.UpdateInitialWindowSize
	require.True(t, ok)
	assert.Equal(t, ErrCodeFlowControlError, connErr.Code)
	assert.Contains(t, connErr.Error(), "exceeding max") // Error message from fcw.UpdateInitialWindowSize
}

func TestStreamFlowControlManager_HandlePeerSettingsInitialWindowSizeChange_InvalidSettingValueError(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	// Test with newPeerInitialSize > MaxWindowSize
	err := sfcm.HandlePeerSettingsInitialWindowSizeChange(MaxWindowSize + 1)
	require.Error(t, err)
	connErr, ok := err.(*ConnectionError)
	require.True(t, ok)
	assert.Equal(t, ErrCodeFlowControlError, connErr.Code)
	assert.Contains(t, err.Error(), "peer's SETTINGS_INITIAL_WINDOW_SIZE value")
	assert.Contains(t, err.Error(), "exceeds MaxWindowSize")
}

func TestStreamFlowControlManager_HandleOurSettingsInitialWindowSizeChange(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialReceiveAvail := sfcm.GetStreamReceiveAvailable() // Based on testOurInitialWindowSize

	newOurInitial := testOurInitialWindowSize + 1000
	err := sfcm.HandleOurSettingsInitialWindowSizeChange(newOurInitial)
	require.NoError(t, err)
	// Delta is newOurInitial - oldOurInitial = 1000
	assert.Equal(t, initialReceiveAvail+1000, sfcm.GetStreamReceiveAvailable())
	assert.Equal(t, newOurInitial, sfcm.effectiveInitialReceiveWindowSize)
	assert.Equal(t, newOurInitial/2, sfcm.windowUpdateThreshold)

	// Decrease
	newOurInitial -= 2000
	err = sfcm.HandleOurSettingsInitialWindowSizeChange(newOurInitial)
	require.NoError(t, err)
	// Delta is newOurInitial - (oldOurInitial+1000) = -2000
	assert.Equal(t, initialReceiveAvail+1000-2000, sfcm.GetStreamReceiveAvailable())
	assert.Equal(t, newOurInitial, sfcm.effectiveInitialReceiveWindowSize)
	assert.Equal(t, newOurInitial/2, sfcm.windowUpdateThreshold)
}

func TestStreamFlowControlManager_HandleOurSettingsInitialWindowSizeChange_OverflowError(t *testing.T) {
	// Choose initial sizes that make sense. Here, ourInitialWindowSize is what's being changed.
	// Peer's initial window size doesn't affect this specific test on our receive window.
	sfcm := NewStreamFlowControlManager(nil, testStreamID, 100 /* ourInitialWindowSize */, testPeerInitialWindowSize /* peerInitialWindowSize */)
	sfcm.currentReceiveWindowSize = MaxWindowSize - 50 // Current available is high

	// New our setting that, when delta is applied, overflows available
	// Old effective initial was 100. New initial is 200. Delta = 100.
	// New available = MaxWindowSize - 50 + 100 = MaxWindowSize + 50 -> overflow
	err := sfcm.HandleOurSettingsInitialWindowSizeChange(200)
	require.Error(t, err)
	connErr, ok := err.(*ConnectionError)
	require.True(t, ok)
	assert.Equal(t, ErrCodeFlowControlError, connErr.Code)
	assert.Contains(t, connErr.Error(), fmt.Sprintf("adjusting stream %d receive window by delta", testStreamID))
	assert.Contains(t, connErr.Error(), "exceeding MaxWindowSize")
}

func TestStreamFlowControlManager_HandleOurSettingsInitialWindowSizeChange_InvalidSettingValueError(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	// Test with newOurInitialSize > MaxWindowSize
	err := sfcm.HandleOurSettingsInitialWindowSizeChange(MaxWindowSize + 1)
	require.Error(t, err)
	connErr, ok := err.(*ConnectionError)
	require.True(t, ok)
	assert.Equal(t, ErrCodeInternalError, connErr.Code) // This is an internal error as our own setting is bad
	assert.Contains(t, err.Error(), "internal error: newOurInitialSize")
	assert.Contains(t, err.Error(), "exceeds MaxWindowSize")
}

func TestStreamFlowControlManager_Close(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialSendAvail := sfcm.GetStreamSendAvailable()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// This should block then error out when closed
		err := sfcm.AcquireSendSpace(uint32(initialSendAvail + 100))
		require.Error(t, err)
		assert.EqualError(t, err, "stream closing")
	}()

	time.Sleep(50 * time.Millisecond) // Let goroutine block

	closeErr := fmt.Errorf("stream closing")
	sfcm.Close(closeErr)

	wg.Wait() // Ensure goroutine completes and checks error

	// Further acquires should fail
	err := sfcm.AcquireSendSpace(1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stream closing")

	// Check sendWindow internal state
	assert.True(t, sfcm.sendWindow.closed)
	assert.Equal(t, closeErr, sfcm.sendWindow.err)
}

func TestStreamFlowControlManager_Getters(t *testing.T) {
	sfcm := NewStreamFlowControlManager(nil, testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	assert.Equal(t, int64(testPeerInitialWindowSize), sfcm.GetStreamSendAvailable())
	assert.Equal(t, int64(testOurInitialWindowSize), sfcm.GetStreamReceiveAvailable())

	// Change send window
	require.NoError(t, sfcm.AcquireSendSpace(100))
	assert.Equal(t, int64(testPeerInitialWindowSize-100), sfcm.GetStreamSendAvailable())

	// Change receive window
	require.NoError(t, sfcm.DataReceived(200))
	assert.Equal(t, int64(testOurInitialWindowSize-200), sfcm.GetStreamReceiveAvailable())
}

// --- FlowControlWindow Tests ---

func TestNewFlowControlWindow(t *testing.T) {
	t.Run("Connection window", func(t *testing.T) {
		fcw := NewFlowControlWindow(DefaultInitialWindowSize, true, 0)
		require.NotNil(t, fcw)
		assert.Equal(t, int64(DefaultInitialWindowSize), fcw.available)
		assert.Equal(t, DefaultInitialWindowSize, fcw.initialWindowSize)
		assert.True(t, fcw.isConnection)
		assert.Equal(t, uint32(0), fcw.streamID)
		assert.False(t, fcw.closed)
		assert.NoError(t, fcw.err)
	})

	t.Run("Stream window", func(t *testing.T) {
		fcw := NewFlowControlWindow(testPeerInitialWindowSize, false, testStreamID)
		require.NotNil(t, fcw)
		assert.Equal(t, int64(testPeerInitialWindowSize), fcw.available)
		assert.Equal(t, testPeerInitialWindowSize, fcw.initialWindowSize)
		assert.False(t, fcw.isConnection)
		assert.Equal(t, testStreamID, fcw.streamID)
		assert.False(t, fcw.closed)
		assert.NoError(t, fcw.err)
	})

	t.Run("Initial size exceeds MaxWindowSize", func(t *testing.T) {
		fcw := NewFlowControlWindow(MaxWindowSize+100, true, 0)
		require.NotNil(t, fcw)
		assert.Equal(t, int64(MaxWindowSize), fcw.available, "Available should be capped at MaxWindowSize")
		assert.Equal(t, uint32(MaxWindowSize), fcw.initialWindowSize, "InitialWindowSize should be capped at MaxWindowSize")
	})
}

func TestFlowControlWindow_Available(t *testing.T) {
	fcw := NewFlowControlWindow(1000, true, 0)
	assert.Equal(t, int64(1000), fcw.Available())
	fcw.available = 500 // Direct manipulation for test simplicity
	assert.Equal(t, int64(500), fcw.Available())
}

func TestFlowControlWindow_Acquire_Simple(t *testing.T) {
	fcw := NewFlowControlWindow(1000, true, 0)
	err := fcw.Acquire(100)
	require.NoError(t, err)
	assert.Equal(t, int64(900), fcw.Available())

	err = fcw.Acquire(900)
	require.NoError(t, err)
	assert.Equal(t, int64(0), fcw.Available())
}

func TestFlowControlWindow_Acquire_ZeroError(t *testing.T) {
	fcw := NewFlowControlWindow(1000, true, 0)
	err := fcw.Acquire(0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot acquire zero bytes")
	assert.Equal(t, int64(1000), fcw.Available())
}

func TestFlowControlWindow_Acquire_BlocksAndUnblocks(t *testing.T) {
	fcw := NewFlowControlWindow(100, false, testStreamID)
	err := fcw.Acquire(100)
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(1)
	acquired := false
	var acquireErr error

	go func() {
		defer wg.Done()
		acquireErr = fcw.Acquire(50)
		acquired = true
	}()

	time.Sleep(50 * time.Millisecond)
	assert.False(t, acquired, "Acquire should be blocking")

	err = fcw.Increase(70)
	require.NoError(t, err)

	wg.Wait()
	assert.True(t, acquired, "Acquire should have unblocked")
	require.NoError(t, acquireErr)
	assert.Equal(t, int64(20), fcw.Available()) // 70 - 50 = 20
}

func TestFlowControlWindow_Acquire_ErrorOnClosed(t *testing.T) {
	fcw := NewFlowControlWindow(100, true, 0)
	closeErr := fmt.Errorf("window deliberately closed")
	fcw.Close(closeErr)

	err := fcw.Acquire(10)
	require.Error(t, err)
	assert.Equal(t, closeErr, err) // Error from Close should be propagated
}

func TestFlowControlWindow_Acquire_ErrorOnPriorError(t *testing.T) {
	fcw := NewFlowControlWindow(100, true, 0)
	priorErr := NewConnectionError(ErrCodeInternalError, "prior internal error")
	fcw.mu.Lock()
	fcw.setErrorLocked(priorErr) // Simulate a prior error
	fcw.mu.Unlock()

	err := fcw.Acquire(10)
	require.Error(t, err)
	assert.Equal(t, priorErr, err)
}

func TestFlowControlWindow_Acquire_NegativeAvailableError(t *testing.T) {
	t.Run("Connection window negative blocks then unblocks", func(t *testing.T) {
		fcw := NewFlowControlWindow(100, true, 0)
		fcw.mu.Lock()
		fcw.available = -50 // Manually set to negative
		fcw.mu.Unlock()

		var wg sync.WaitGroup
		wg.Add(1)
		acquired := false
		var acquireErr error

		go func() {
			defer wg.Done()
			// This should block because fcw.available is -50
			acquireErr = fcw.Acquire(10)
			if acquireErr == nil {
				acquired = true
			}
		}()

		time.Sleep(100 * time.Millisecond) // Give goroutine time to block
		assert.False(t, acquired, "Acquire should be blocking on negative window")

		// Increase the window to make it positive and sufficient
		// -50 + 70 = 20. 20 >= 10 (acquire amount)
		err := fcw.Increase(70)
		require.NoError(t, err)

		wg.Wait() // Wait for Acquire to complete
		assert.True(t, acquired, "Acquire should have unblocked and succeeded")
		require.NoError(t, acquireErr)
		assert.Equal(t, int64(10), fcw.Available()) // -50 + 70 - 10 = 10
	})

	t.Run("Stream window negative blocks then unblocks", func(t *testing.T) {
		fcw := NewFlowControlWindow(100, false, testStreamID)
		fcw.mu.Lock()
		fcw.available = -50 // Manually set to negative
		fcw.mu.Unlock()

		var wg sync.WaitGroup
		wg.Add(1)
		acquired := false
		var acquireErr error

		go func() {
			defer wg.Done()
			// This should block
			acquireErr = fcw.Acquire(10)
			if acquireErr == nil {
				acquired = true
			}
		}()

		time.Sleep(100 * time.Millisecond) // Give goroutine time to block
		assert.False(t, acquired, "Acquire should be blocking on negative window")

		// Increase the window
		// -50 + 70 = 20. 20 >= 10
		err := fcw.Increase(70)
		require.NoError(t, err)

		wg.Wait() // Wait for Acquire to complete
		assert.True(t, acquired, "Acquire should have unblocked and succeeded")
		require.NoError(t, acquireErr)
		assert.Equal(t, int64(10), fcw.Available()) // -50 + 70 - 10 = 10
	})
}

func TestFlowControlWindow_Increase_Simple(t *testing.T) {
	fcw := NewFlowControlWindow(100, true, 0)
	err := fcw.Acquire(100)
	require.NoError(t, err)
	assert.Equal(t, int64(0), fcw.Available())

	// Signal waiter for test coverage of cond.Broadcast
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = fcw.Acquire(1) // This will block then proceed
	}()
	time.Sleep(20 * time.Millisecond) // let goroutine block

	err = fcw.Increase(50)
	require.NoError(t, err)
	assert.Equal(t, int64(50), fcw.Available())
	wg.Wait() // ensure goroutine unblocked and acquired
	assert.Equal(t, int64(49), fcw.Available())
}

func TestFlowControlWindow_Increase_ZeroIncrement(t *testing.T) {
	t.Run("Connection window zero increment (no-op)", func(t *testing.T) {
		fcw := NewFlowControlWindow(100, true, 0)
		initialAvail := fcw.Available()
		err := fcw.Increase(0)
		require.NoError(t, err)
		assert.Equal(t, initialAvail, fcw.Available())
	})
	t.Run("Stream window zero increment (protocol error)", func(t *testing.T) {
		fcw := NewFlowControlWindow(100, false, testStreamID)
		err := fcw.Increase(0)
		require.Error(t, err)
		streamErr, ok := err.(*StreamError)
		require.True(t, ok)
		assert.Equal(t, ErrCodeProtocolError, streamErr.Code)
		assert.Equal(t, testStreamID, streamErr.StreamID)
		assert.Contains(t, streamErr.Msg, "WINDOW_UPDATE increment cannot be 0 for a stream")
	})
}

func TestFlowControlWindow_Increase_OverflowError(t *testing.T) {
	t.Run("Connection window overflow", func(t *testing.T) {
		fcw := NewFlowControlWindow(MaxWindowSize-50, true, 0)
		err := fcw.Increase(100) // MaxWindowSize - 50 + 100 = MaxWindowSize + 50
		require.Error(t, err)
		connErr, ok := err.(*ConnectionError)
		require.True(t, ok)
		assert.Equal(t, ErrCodeFlowControlError, connErr.Code)
		assert.Contains(t, connErr.Msg, "would overflow")
		assert.True(t, fcw.closed, "window should be closed on error")
		assert.Equal(t, connErr, fcw.err, "window error should be set")
	})
	t.Run("Stream window overflow", func(t *testing.T) {
		fcw := NewFlowControlWindow(MaxWindowSize-50, false, testStreamID)
		err := fcw.Increase(100)
		require.Error(t, err)
		streamErr, ok := err.(*StreamError)
		require.True(t, ok)
		assert.Equal(t, ErrCodeFlowControlError, streamErr.Code)
		assert.Equal(t, testStreamID, streamErr.StreamID)
		assert.Contains(t, streamErr.Msg, "would overflow")
		assert.True(t, fcw.closed, "window should be closed on error")
		assert.Equal(t, streamErr, fcw.err, "window error should be set")
	})
}

func TestFlowControlWindow_Increase_ErrorOnClosedOrPriorError(t *testing.T) {
	closedErr := fmt.Errorf("deliberately closed")
	priorErr := NewConnectionError(ErrCodeInternalError, "prior error")

	tests := []struct {
		name        string
		setup       func(*FlowControlWindow)
		expectedErr error
	}{
		{
			name: "Closed window",
			setup: func(fcw *FlowControlWindow) {
				fcw.Close(closedErr)
			},
			expectedErr: closedErr,
		},
		{
			name: "Window with prior error",
			setup: func(fcw *FlowControlWindow) {
				fcw.mu.Lock()
				fcw.setErrorLocked(priorErr)
				fcw.mu.Unlock()
			},
			expectedErr: priorErr,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fcw := NewFlowControlWindow(100, true, 0)
			tc.setup(fcw)
			err := fcw.Increase(10)
			require.Error(t, err)
			assert.Equal(t, tc.expectedErr, err)
		})
	}
}

func TestFlowControlWindow_UpdateInitialWindowSize_Stream(t *testing.T) {
	fcw := NewFlowControlWindow(1000, false, testStreamID) // initial available & initialSize = 1000
	require.NoError(t, fcw.Acquire(200))                   // available = 800

	// Increase initial window size
	err := fcw.UpdateInitialWindowSize(1500) // newInitial = 1500. delta = 1500 - 1000 = 500
	require.NoError(t, err)
	assert.Equal(t, int64(800+500), fcw.Available())     // available = 1300
	assert.Equal(t, uint32(1500), fcw.initialWindowSize) // initialSize updated

	// Decrease initial window size
	err = fcw.UpdateInitialWindowSize(800) // newInitial = 800. delta = 800 - 1500 = -700
	require.NoError(t, err)
	assert.Equal(t, int64(1300-700), fcw.Available())   // available = 600
	assert.Equal(t, uint32(800), fcw.initialWindowSize) // initialSize updated

	// No change initial window size
	currentAvailBeforeNoChange := fcw.Available()
	currentInitialBeforeNoChange := fcw.initialWindowSize
	err = fcw.UpdateInitialWindowSize(currentInitialBeforeNoChange) // newInitial is same as old. delta = 0
	require.NoError(t, err)
	assert.Equal(t, currentAvailBeforeNoChange, fcw.Available(), "Available window should be unchanged after a no-op update")
	assert.Equal(t, currentInitialBeforeNoChange, fcw.initialWindowSize, "Initial window size should be unchanged after a no-op update")

	// Test scenario: available becomes negative due to settings change
	// Setup: initialSize = 500, available = 500
	fcwNegative := NewFlowControlWindow(500, false, testStreamID+1)
	require.Equal(t, int64(500), fcwNegative.Available())
	require.Equal(t, uint32(500), fcwNegative.initialWindowSize)

	// Acquire some data to make available != initialWindowSize
	// available = 500 - 100 = 400
	err = fcwNegative.Acquire(100)
	require.NoError(t, err)
	require.Equal(t, int64(400), fcwNegative.Available())

	// Update initial window size to be very small, driving available negative
	// Old initialWindowSize = 500. Current available = 400.
	// New initialWindowSize = 50. Delta = 50 - 500 = -450.
	// New available = current_available + delta = 400 + (-450) = -50.
	newSmallInitialSize := uint32(50)
	err = fcwNegative.UpdateInitialWindowSize(newSmallInitialSize)
	require.NoError(t, err)
	assert.Equal(t, int64(-50), fcwNegative.Available(), "Available window should be negative after settings change")
	assert.Equal(t, newSmallInitialSize, fcwNegative.initialWindowSize, "Initial window size should be updated to the new small size")

	// Test scenario: available is already negative, and settings change makes it more negative
	// Current available = -50. Current initialWindowSize = 50.
	// New initialWindowSize = 20. Delta = 20 - 50 = -30.
	// New available = current_available + delta = -50 + (-30) = -80.
	newEvenSmallerInitialSize := uint32(20)
	err = fcwNegative.UpdateInitialWindowSize(newEvenSmallerInitialSize)
	require.NoError(t, err)
	assert.Equal(t, int64(-80), fcwNegative.Available(), "Available window should be more negative")
	assert.Equal(t, newEvenSmallerInitialSize, fcwNegative.initialWindowSize, "Initial window size should be updated to even smaller size")

	// Test scenario: available is negative, settings change makes it less negative (but still negative)
	// Current available = -80. Current initialWindowSize = 20.
	// New initialWindowSize = 40. Delta = 40 - 20 = 20.
	// New available = current_available + delta = -80 + 20 = -60.
	newSlightlyLargerInitialSize := uint32(40)
	err = fcwNegative.UpdateInitialWindowSize(newSlightlyLargerInitialSize)
	require.NoError(t, err)
	assert.Equal(t, int64(-60), fcwNegative.Available(), "Available window should be less negative")
	assert.Equal(t, newSlightlyLargerInitialSize, fcwNegative.initialWindowSize, "Initial window size should be updated")

	// Test scenario: available is negative, settings change makes it positive
	// Current available = -60. Current initialWindowSize = 40.
	// New initialWindowSize = 150. Delta = 150 - 40 = 110.
	// New available = current_available + delta = -60 + 110 = 50.
	newPositiveInitialSize := uint32(150)
	err = fcwNegative.UpdateInitialWindowSize(newPositiveInitialSize)
	require.NoError(t, err)
	assert.Equal(t, int64(50), fcwNegative.Available(), "Available window should become positive")
	assert.Equal(t, newPositiveInitialSize, fcwNegative.initialWindowSize, "Initial window size should be updated")
}

func TestFlowControlWindow_UpdateInitialWindowSize_ConnectionNoOp(t *testing.T) {
	fcw := NewFlowControlWindow(1000, true, 0)
	initialAvail := fcw.Available()
	initialInitialSize := fcw.initialWindowSize

	err := fcw.UpdateInitialWindowSize(2000)
	require.NoError(t, err)
	assert.Equal(t, initialAvail, fcw.Available())
	assert.Equal(t, initialInitialSize, fcw.initialWindowSize)
}

func TestFlowControlWindow_UpdateInitialWindowSize_InvalidNewInitialSize(t *testing.T) {
	// This error is for peer's SETTINGS_INITIAL_WINDOW_SIZE being invalid
	fcw := NewFlowControlWindow(1000, false, testStreamID)
	err := fcw.UpdateInitialWindowSize(MaxWindowSize + 1)
	require.Error(t, err)
	connErr, ok := err.(*ConnectionError)
	require.True(t, ok)
	assert.Equal(t, ErrCodeFlowControlError, connErr.Code)
	assert.Contains(t, connErr.Msg, "peer's SETTINGS_INITIAL_WINDOW_SIZE value")
	assert.Contains(t, connErr.Msg, "exceeds MaxWindowSize")
}

func TestFlowControlWindow_UpdateInitialWindowSize_OverflowError(t *testing.T) {
	// This error is when applying delta causes available to overflow
	fcw := NewFlowControlWindow(100, false, testStreamID) // initialSize = 100
	fcw.available = MaxWindowSize - 50                    // available high
	err := fcw.UpdateInitialWindowSize(200)               // newInitial = 200, delta = 100. newAvail = MaxWindowSize - 50 + 100
	require.Error(t, err)
	connErr, ok := err.(*ConnectionError)
	require.True(t, ok)
	assert.Equal(t, ErrCodeFlowControlError, connErr.Code)
	assert.Contains(t, connErr.Msg, "applying SETTINGS_INITIAL_WINDOW_SIZE delta")
	assert.Contains(t, connErr.Msg, "exceeding max")
	// fcw.err is not set by UpdateInitialWindowSize directly, it returns the connection error.
}

func TestFlowControlWindow_UpdateInitialWindowSize_ErrorOnClosedOrPriorError(t *testing.T) {
	closedErr := fmt.Errorf("deliberately closed for update")
	priorErr := NewConnectionError(ErrCodeInternalError, "prior error for update")

	tests := []struct {
		name        string
		setup       func(*FlowControlWindow)
		expectedErr error
	}{
		{
			name: "Closed window",
			setup: func(fcw *FlowControlWindow) {
				fcw.Close(closedErr)
			},
			expectedErr: closedErr,
		},
		{
			name: "Window with prior error",
			setup: func(fcw *FlowControlWindow) {
				fcw.mu.Lock()
				fcw.setErrorLocked(priorErr)
				fcw.mu.Unlock()
			},
			expectedErr: priorErr,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fcw := NewFlowControlWindow(100, false, testStreamID) // Stream window for this method
			tc.setup(fcw)
			err := fcw.UpdateInitialWindowSize(200)
			require.Error(t, err)
			assert.Equal(t, tc.expectedErr, err)
		})
	}
}

func TestFlowControlWindow_Close(t *testing.T) {
	fcw := NewFlowControlWindow(100, true, 0)
	assert.False(t, fcw.closed)
	assert.NoError(t, fcw.err)

	// Signal waiter for test coverage of cond.Broadcast on close
	var wg sync.WaitGroup
	wg.Add(1)
	var acquireErrOnClose error
	go func() {
		defer wg.Done()
		acquireErrOnClose = fcw.Acquire(150) // This will block then error out
	}()
	time.Sleep(20 * time.Millisecond) // let goroutine block

	closeErr := fmt.Errorf("closing test")
	fcw.Close(closeErr)

	assert.True(t, fcw.closed)
	assert.Equal(t, closeErr, fcw.err)

	wg.Wait() // ensure goroutine unblocked
	require.Error(t, acquireErrOnClose)
	assert.Equal(t, closeErr, acquireErrOnClose)

	// Subsequent close should be no-op (error not overwritten)
	fcw.Close(fmt.Errorf("another close error"))
	assert.True(t, fcw.closed)
	assert.Equal(t, closeErr, fcw.err) // Original error should persist

	// Close with nil error when no prior error
	fcwNil := NewFlowControlWindow(100, true, 0)
	fcwNil.Close(nil)
	assert.True(t, fcwNil.closed)
	assert.NoError(t, fcwNil.err) // Error remains nil
}

func TestFlowControlWindow_setErrorLocked(t *testing.T) {
	fcw := NewFlowControlWindow(100, true, 0)
	err1 := fmt.Errorf("error1")
	err2 := fmt.Errorf("error2")

	fcw.mu.Lock()
	fcw.setErrorLocked(err1)
	fcw.mu.Unlock()

	assert.True(t, fcw.closed, "setErrorLocked should close window")
	assert.Equal(t, err1, fcw.err)

	// Try to set another error, should be ignored
	fcw.mu.Lock()
	fcw.setErrorLocked(err2)
	fcw.mu.Unlock()

	assert.True(t, fcw.closed)
	assert.Equal(t, err1, fcw.err, "Error should not be overwritten")
}

func TestFlowControlWindow_Acquire_MultipleWaiters_Increase(t *testing.T) {
	fcw := NewFlowControlWindow(10, false, testStreamID) // Initial capacity 10
	err := fcw.Acquire(10)                               // Exhaust initial capacity
	require.NoError(t, err)
	assert.Equal(t, int64(0), fcw.Available())

	numWaiters := 3
	acquireAmount := uint32(5)
	var wg sync.WaitGroup
	wg.Add(numWaiters)

	errs := make(chan error, numWaiters)

	for i := 0; i < numWaiters; i++ {
		go func(idx int) {
			defer wg.Done()
			t.Logf("Goroutine %d attempting to acquire %d bytes", idx, acquireAmount)
			err := fcw.Acquire(acquireAmount)
			if err != nil {
				t.Logf("Goroutine %d acquire error: %v", idx, err)
			} else {
				t.Logf("Goroutine %d acquired %d bytes successfully", idx, acquireAmount)
			}
			errs <- err
		}(i)
	}

	// Give goroutines a chance to block
	time.Sleep(100 * time.Millisecond) // Increased sleep to ensure goroutines likely hit cond.Wait()

	t.Logf("Main: Increasing window by %d", acquireAmount*uint32(numWaiters))
	increaseErr := fcw.Increase(acquireAmount * uint32(numWaiters)) // Increase by 5 * 3 = 15
	require.NoError(t, increaseErr)
	t.Logf("Main: Window increased. Available: %d", fcw.Available())

	wg.Wait() // Wait for all goroutines to complete
	close(errs)

	for i := 0; i < numWaiters; i++ {
		err := <-errs
		assert.NoError(t, err, "Goroutine %d should have acquired successfully", i)
	}

	// All 3 waiters should have acquired 5 bytes each, total 15.
	// Initial was 0. Increased by 15. Then 15 acquired.
	assert.Equal(t, int64(0), fcw.Available(), "Window should be fully consumed")
}

func TestFlowControlWindow_Acquire_MultipleWaiters_Close(t *testing.T) {
	fcw := NewFlowControlWindow(10, false, testStreamID) // Initial capacity 10
	err := fcw.Acquire(10)                               // Exhaust initial capacity
	require.NoError(t, err)
	assert.Equal(t, int64(0), fcw.Available())

	numWaiters := 3
	acquireAmount := uint32(5)
	var wg sync.WaitGroup
	wg.Add(numWaiters)

	errs := make(chan error, numWaiters)
	closeError := fmt.Errorf("test close error for multiple waiters")

	for i := 0; i < numWaiters; i++ {
		go func(idx int) {
			defer wg.Done()
			t.Logf("Goroutine %d attempting to acquire %d bytes, expecting error on close", idx, acquireAmount)
			err := fcw.Acquire(acquireAmount)
			if err != nil {
				t.Logf("Goroutine %d acquire error: %v", idx, err)
			} else {
				t.Logf("Goroutine %d acquired %d bytes successfully (unexpected)", idx, acquireAmount)
			}
			errs <- err
		}(i)
	}

	// Give goroutines a chance to block
	time.Sleep(100 * time.Millisecond)

	t.Logf("Main: Closing window with error: %v", closeError)
	fcw.Close(closeError)
	t.Logf("Main: Window closed. Available: %d", fcw.Available())

	wg.Wait() // Wait for all goroutines to complete
	close(errs)

	for i := 0; i < numWaiters; i++ {
		err := <-errs
		require.Error(t, err, "Goroutine %d should have received an error", i)
		assert.Equal(t, closeError, err, "Goroutine %d received incorrect error", i)
	}

	assert.Equal(t, int64(0), fcw.Available(), "Window should remain at 0 after close")
	assert.True(t, fcw.closed, "Window should be marked as closed")
	assert.Equal(t, closeError, fcw.err, "Window error should be set to the closeError")
}

func TestFlowControlWindow_Acquire_UnblockedByUpdateInitialWindowSize(t *testing.T) {
	initialSize := uint32(100)
	fcw := NewFlowControlWindow(initialSize, false, testStreamID) // Stream window

	// Exhaust the window
	err := fcw.Acquire(initialSize)
	require.NoError(t, err)
	assert.Equal(t, int64(0), fcw.Available())

	var wg sync.WaitGroup
	wg.Add(1)
	acquired := false
	var acquireErr error
	acquireAmount := uint32(50)

	go func() {
		defer wg.Done()
		t.Logf("Goroutine attempting to acquire %d bytes", acquireAmount)
		acquireErr = fcw.Acquire(acquireAmount)
		if acquireErr == nil {
			acquired = true
			t.Logf("Goroutine acquired %d bytes successfully", acquireAmount)
		} else {
			t.Logf("Goroutine acquire error: %v", acquireErr)
		}
	}()

	// Give goroutine a chance to block
	time.Sleep(50 * time.Millisecond)
	assert.False(t, acquired, "Acquire should be blocking")

	// Increase initial window size.
	// Delta = newInitialSize - oldInitialSize = 150 - 100 = 50.
	// This delta should be enough to unblock the Acquire.
	newInitialSize := initialSize + acquireAmount
	t.Logf("Main: Calling UpdateInitialWindowSize with newInitialSize %d", newInitialSize)
	updateErr := fcw.UpdateInitialWindowSize(newInitialSize)
	require.NoError(t, updateErr)
	t.Logf("Main: UpdateInitialWindowSize complete. Available: %d, InitialWindowSize: %d", fcw.Available(), fcw.initialWindowSize)

	wg.Wait() // Wait for goroutine to complete
	assert.True(t, acquired, "Acquire should have unblocked and succeeded")
	require.NoError(t, acquireErr, "Acquire should not have errored")

	// Available window: initial was 0. UpdateInitialWindowSize added delta of 50. So it became 50.
	// Goroutine acquired 50. So it should be 0.
	assert.Equal(t, int64(0), fcw.Available())
	assert.Equal(t, newInitialSize, fcw.initialWindowSize)
}

func TestStreamFlowControlManager_ApplicationConsumedData_ThresholdOne(t *testing.T) {
	// This test specifically checks ApplicationConsumedData when windowUpdateThreshold is 1.
	// This occurs when ourInitialWindowSize is 1.
	const localStreamID uint32 = 99
	// Create sfcm with ourInitialWindowSize = 1
	sfcm := NewStreamFlowControlManager(nil, localStreamID, 1 /* ourInitialWindowSize */, 1000 /* peerInitialWindowSize */)
	require.NotNil(t, sfcm)
	assert.Equal(t, uint32(1), sfcm.windowUpdateThreshold, "Window update threshold should be 1 when ourInitialWindowSize is 1")

	// Simulate data received to fill the small receive window
	err := sfcm.DataReceived(1) // Receive 1 byte, currentReceiveWindowSize becomes 0
	require.NoError(t, err)
	assert.Equal(t, int64(0), sfcm.GetStreamReceiveAvailable())

	// Consume the 1 byte of data
	increment, err := sfcm.ApplicationConsumedData(1)
	require.NoError(t, err)

	// Since threshold is 1 and 1 byte was consumed, an update should be generated.
	assert.Equal(t, uint32(1), increment, "WINDOW_UPDATE increment should be 1")
	assert.Equal(t, int64(1), sfcm.GetStreamReceiveAvailable(), "Receive window should be restored to 1 after consumption and update trigger")
	assert.Equal(t, uint64(1), sfcm.bytesConsumedTotal)
	assert.Equal(t, sfcm.bytesConsumedTotal, sfcm.lastWindowUpdateSentAt, "lastWindowUpdateSentAt should be updated")

	// Consume 0 bytes, should not trigger an update
	increment, err = sfcm.ApplicationConsumedData(0)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), increment, "Consuming 0 bytes should not generate a WINDOW_UPDATE")
}

func TestStreamFlowControlManager_SendWindow_NegativeBySettings_PositiveBySettings_Acquire(t *testing.T) {
	const streamIDForTest uint32 = 123
	const initialOurSize uint32 = 200 // Not directly relevant for send window part
	const initialPeerSize uint32 = 100

	sfcm := NewStreamFlowControlManager(nil, streamIDForTest, initialOurSize, initialPeerSize)
	require.Equal(t, int64(initialPeerSize), sfcm.GetStreamSendAvailable(), "Initial send window should match peer's initial setting")
	require.Equal(t, initialPeerSize, sfcm.sendWindow.initialWindowSize)

	// Step 1: Acquire some data to make 'available' different from 'initialWindowSize'
	acquireAmount1 := uint32(20)
	err := sfcm.AcquireSendSpace(acquireAmount1)
	require.NoError(t, err)
	require.Equal(t, int64(initialPeerSize-acquireAmount1), sfcm.GetStreamSendAvailable()) // Available = 100 - 20 = 80

	// Step 2: Change peer's SETTINGS_INITIAL_WINDOW_SIZE to drive the send window negative
	// Old initialWindowSize for sendWindow = 100. Current available = 80.
	// New peer's initial setting = 0. Delta = 0 - 100 = -100.
	// New available = 80 (current) + (-100) (delta) = -20.
	// New initialWindowSize for sendWindow becomes 0.
	newPeerInitialSize1 := uint32(0)
	err = sfcm.HandlePeerSettingsInitialWindowSizeChange(newPeerInitialSize1)
	require.NoError(t, err)
	assert.Equal(t, int64(-20), sfcm.GetStreamSendAvailable(), "Send window should be negative after settings change")
	assert.Equal(t, newPeerInitialSize1, sfcm.sendWindow.initialWindowSize, "sendWindow.initialWindowSize should be updated")

	// Step 3: Attempt to acquire space, which should block
	var wg sync.WaitGroup
	wg.Add(1)
	acquired := false
	var acquireErr error
	acquireAmount2 := uint32(10) // Amount to acquire after window becomes positive

	go func() {
		defer wg.Done()
		t.Logf("Goroutine: Attempting to acquire %d bytes from negative window...", acquireAmount2)
		acquireErr = sfcm.AcquireSendSpace(acquireAmount2)
		if acquireErr == nil {
			acquired = true
			t.Logf("Goroutine: Successfully acquired %d bytes.", acquireAmount2)
		} else {
			t.Logf("Goroutine: Acquire error: %v", acquireErr)
		}
	}()

	time.Sleep(100 * time.Millisecond) // Give goroutine time to block
	assert.False(t, acquired, "AcquireSendSpace should be blocking on negative window")

	// Step 4: Change peer's SETTINGS_INITIAL_WINDOW_SIZE again to make the window positive
	// Current available = -20. Current initialWindowSize for sendWindow = 0.
	// We need newAvailable = current_available + (new_setting - old_setting_now_0) >= acquireAmount2 (10)
	// -20 + (new_setting_2 - 0) >= 10  => new_setting_2 >= 30.
	// Let newPeerInitialSize2 = 50. Delta = 50 - 0 = 50.
	// New available = -20 + 50 = 30.
	// New initialWindowSize for sendWindow becomes 50.
	newPeerInitialSize2 := uint32(50)
	t.Logf("Main: Updating peer initial window size to %d to make window positive.", newPeerInitialSize2)
	err = sfcm.HandlePeerSettingsInitialWindowSizeChange(newPeerInitialSize2)
	require.NoError(t, err)
	t.Logf("Main: Window updated by settings. Available now: %d", sfcm.GetStreamSendAvailable()) // Should be 30 before goroutine acquires

	wg.Wait() // Wait for goroutine to complete

	assert.True(t, acquired, "AcquireSendSpace should have unblocked and succeeded")
	require.NoError(t, acquireErr, "AcquireSendSpace error")

	// Expected final available: 30 (after settings update) - 10 (acquired) = 20
	assert.Equal(t, int64(20), sfcm.GetStreamSendAvailable(), "Final send window is incorrect")
	assert.Equal(t, newPeerInitialSize2, sfcm.sendWindow.initialWindowSize, "sendWindow.initialWindowSize should be the latest setting")
}

func TestStreamFlowControlManager_SendWindow_NegativeBySettings_PositiveByWindowUpdate_Acquire(t *testing.T) {
	const streamIDForTest uint32 = 456
	const initialOurSize uint32 = 200 // Not relevant for send window
	const initialPeerSize uint32 = 100

	sfcm := NewStreamFlowControlManager(nil, streamIDForTest, initialOurSize, initialPeerSize)
	require.Equal(t, int64(initialPeerSize), sfcm.GetStreamSendAvailable())

	// Step 1: Acquire some data
	acquireAmount1 := uint32(20)
	err := sfcm.AcquireSendSpace(acquireAmount1)
	require.NoError(t, err)
	require.Equal(t, int64(initialPeerSize-acquireAmount1), sfcm.GetStreamSendAvailable()) // Available = 80

	// Step 2: Change peer's SETTINGS_INITIAL_WINDOW_SIZE to drive the send window negative
	// Old initialWindowSize = 100. Current available = 80.
	// New peer's initial setting = 0. Delta = 0 - 100 = -100.
	// New available = 80 + (-100) = -20.
	newPeerInitialSize1 := uint32(0)
	err = sfcm.HandlePeerSettingsInitialWindowSizeChange(newPeerInitialSize1)
	require.NoError(t, err)
	assert.Equal(t, int64(-20), sfcm.GetStreamSendAvailable(), "Send window should be negative")

	// Step 3: Attempt to acquire space, which should block
	var wg sync.WaitGroup
	wg.Add(1)
	acquired := false
	var acquireErr error
	acquireAmount2 := uint32(10)

	go func() {
		defer wg.Done()
		t.Logf("Goroutine: Attempting to acquire %d bytes from negative window...", acquireAmount2)
		acquireErr = sfcm.AcquireSendSpace(acquireAmount2)
		if acquireErr == nil {
			acquired = true
			t.Logf("Goroutine: Successfully acquired %d bytes.", acquireAmount2)
		} else {
			t.Logf("Goroutine: Acquire error: %v", acquireErr)
		}
	}()

	time.Sleep(100 * time.Millisecond) // Give goroutine time to block
	assert.False(t, acquired, "AcquireSendSpace should be blocking on negative window")

	// Step 4: Send a WINDOW_UPDATE from peer to make the window positive
	// Current available = -20. We need it to be >= 10 for acquireAmount2.
	// Increment by 40: New available = -20 + 40 = 20.
	windowUpdateIncrement := uint32(40)
	t.Logf("Main: Sending WINDOW_UPDATE with increment %d to make window positive.", windowUpdateIncrement)
	err = sfcm.HandleWindowUpdateFromPeer(windowUpdateIncrement)
	require.NoError(t, err)
	t.Logf("Main: Window updated by WINDOW_UPDATE. Available now: %d", sfcm.GetStreamSendAvailable()) // Should be 20 before goroutine acquires

	wg.Wait() // Wait for goroutine to complete

	assert.True(t, acquired, "AcquireSendSpace should have unblocked and succeeded")
	require.NoError(t, acquireErr, "AcquireSendSpace error")

	// Expected final available: 20 (after WINDOW_UPDATE) - 10 (acquired) = 10
	assert.Equal(t, int64(10), sfcm.GetStreamSendAvailable(), "Final send window is incorrect")
	// initialWindowSize for sendWindow should remain what the last SETTINGS set it to (0)
	assert.Equal(t, newPeerInitialSize1, sfcm.sendWindow.initialWindowSize, "sendWindow.initialWindowSize should not be affected by WINDOW_UPDATE")
}

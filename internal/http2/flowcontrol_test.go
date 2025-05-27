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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcmSmall := NewStreamFlowControlManager(testStreamID, 1, 1)
	assert.Equal(t, uint32(1), sfcmSmall.windowUpdateThreshold, "Threshold should be 1 if initial size is 1")
}

func TestStreamFlowControlManager_AcquireSendSpace_Simple(t *testing.T) {
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	closeErr := fmt.Errorf("test close error")
	sfcm.Close(closeErr)

	err := sfcm.AcquireSendSpace(100)
	require.Error(t, err)
	assert.EqualError(t, err, "test close error") // The specific error passed to Close
}

func TestStreamFlowControlManager_AcquireSendSpace_ZeroAcquire(t *testing.T) {
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialSendWindow := sfcm.sendWindow.Available()

	err := sfcm.AcquireSendSpace(0)
	require.NoError(t, err, "Acquiring zero bytes should not error")
	assert.Equal(t, initialSendWindow, sfcm.sendWindow.Available(), "Acquiring zero bytes should not change window")
}

func TestStreamFlowControlManager_HandleWindowUpdateFromPeer(t *testing.T) {
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialSendWindow := sfcm.sendWindow.Available()

	err := sfcm.HandleWindowUpdateFromPeer(1000)
	require.NoError(t, err)
	assert.Equal(t, initialSendWindow+1000, sfcm.sendWindow.Available())
}

func TestStreamFlowControlManager_HandleWindowUpdateFromPeer_Overflow(t *testing.T) {
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	sfcm.sendWindow.Close(fmt.Errorf("closed"))

	err := sfcm.HandleWindowUpdateFromPeer(100)
	require.Error(t, err)
	assert.EqualError(t, err, "closed") // The specific error passed to Close
}

func TestStreamFlowControlManager_DataReceived(t *testing.T) {
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	initialReceiveWindow := sfcm.GetStreamReceiveAvailable()
	initialTotalReceived := sfcm.totalBytesReceived

	err := sfcm.DataReceived(0)
	require.NoError(t, err)
	assert.Equal(t, initialReceiveWindow, sfcm.GetStreamReceiveAvailable())
	assert.Equal(t, initialTotalReceived, sfcm.totalBytesReceived)
}

func TestStreamFlowControlManager_ApplicationConsumedData_GeneratesUpdate(t *testing.T) {
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
	require.NoError(t, sfcm.DataReceived(sfcm.windowUpdateThreshold*2))

	increment, err := sfcm.ApplicationConsumedData(sfcm.windowUpdateThreshold - 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), increment)
}

func TestStreamFlowControlManager_ApplicationConsumedData_ZeroConsumption(t *testing.T) {
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
		sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
		_, err := sfcm.ApplicationConsumedData(MaxWindowSize + 1)
		require.Error(t, err)
		streamErr, ok := err.(*StreamError)
		require.True(t, ok)
		assert.Equal(t, ErrCodeInternalError, streamErr.Code)
		assert.Equal(t, testStreamID, streamErr.StreamID)
		assert.Contains(t, err.Error(), "exceeds MaxWindowSize")
	})

	t.Run("Calculated increment overflow MaxWindowSize", func(t *testing.T) {
		sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
		sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
}

func TestStreamFlowControlManager_HandlePeerSettingsInitialWindowSizeChange_OverflowError(t *testing.T) {
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, 100) // Small initial peer window
	sfcm.sendWindow.available = MaxWindowSize - 50                                   // Current available is high

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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, 100, testPeerInitialWindowSize) // Small initial our window
	sfcm.currentReceiveWindowSize = MaxWindowSize - 50                                // Current available is high

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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	sfcm := NewStreamFlowControlManager(testStreamID, testOurInitialWindowSize, testPeerInitialWindowSize)
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
	t.Run("Connection window negative", func(t *testing.T) {
		fcw := NewFlowControlWindow(100, true, 0)
		fcw.mu.Lock()
		fcw.available = -50 // Manually set to negative to simulate prior failure
		fcw.mu.Unlock()

		err := fcw.Acquire(10)
		require.Error(t, err)
		connErr, ok := err.(*ConnectionError)
		require.True(t, ok)
		assert.Equal(t, ErrCodeFlowControlError, connErr.Code)
		assert.Contains(t, connErr.Msg, "window is negative")
	})
	t.Run("Stream window negative", func(t *testing.T) {
		fcw := NewFlowControlWindow(100, false, testStreamID)
		fcw.mu.Lock()
		fcw.available = -50 // Manually set to negative
		fcw.mu.Unlock()

		err := fcw.Acquire(10)
		require.Error(t, err)
		streamErr, ok := err.(*StreamError)
		require.True(t, ok)
		assert.Equal(t, ErrCodeFlowControlError, streamErr.Code)
		assert.Equal(t, testStreamID, streamErr.StreamID)
		assert.Contains(t, streamErr.Msg, "window is negative")
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

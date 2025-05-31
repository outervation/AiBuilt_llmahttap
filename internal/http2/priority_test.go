package http2

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNewPriorityTree(t *testing.T) {
	pt := NewPriorityTree()
	if pt == nil {
		t.Fatal("NewPriorityTree returned nil")
	}
	if pt.nodes == nil {
		t.Fatal("NewPriorityTree().nodes is nil")
	}
	if len(pt.nodes) != 1 {
		t.Errorf("Expected 1 node (stream 0) in new tree, got %d", len(pt.nodes))
	}
	rootNode, ok := pt.nodes[0]
	if !ok {
		t.Fatal("Stream 0 (root node) not found in new tree")
	}
	if rootNode.streamID != 0 {
		t.Errorf("Root node streamID expected 0, got %d", rootNode.streamID)
	}
	if rootNode.parentID != 0 {
		t.Errorf("Root node parentID expected 0, got %d", rootNode.parentID)
	}
	if len(rootNode.childrenIDs) != 0 {
		t.Errorf("Root node childrenIDs expected empty, got %v", rootNode.childrenIDs)
		if rootNode.weight != 0 { // Weight is not strictly applicable to stream 0, but initialized to 0
			t.Errorf("Root node weight expected 0, got %d", rootNode.weight)
		}
	}
}

func TestPriorityTree_AddStream_DefaultPriority(t *testing.T) {
	pt := NewPriorityTree()
	streamID := uint32(3)

	err := pt.AddStream(streamID, nil)
	if err != nil {
		t.Fatalf("AddStream failed: %v", err)
	}

	parentID, childrenIDs, weight, err := pt.GetDependencies(streamID)
	if err != nil {
		t.Fatalf("GetDependencies for stream %d failed: %v", streamID, err)
	}

	if parentID != 0 {
		t.Errorf("Expected stream %d to be child of stream 0, got parent %d", streamID, parentID)
	}
	if weight != 15 { // Default weight 16 means frame value 15
		t.Errorf("Expected stream %d to have weight 15 (effective 16), got %d", streamID, weight)
	}
	if len(childrenIDs) != 0 {
		t.Errorf("Expected stream %d to have no children initially, got %v", streamID, childrenIDs)
	}

	// Verify stream 0 has streamID as a child
	_, stream0Children, _, err := pt.GetDependencies(0)
	if err != nil {
		t.Fatalf("GetDependencies for stream 0 failed: %v", err)
	}

	found := false
	for _, child := range stream0Children {
		if child == streamID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Stream %d not found in children of stream 0. Children: %v", streamID, stream0Children)
	}
}

func TestPriorityTree_AddStream_SpecificPriority(t *testing.T) {
	pt := NewPriorityTree()

	parentStreamID := uint32(1)
	siblingStreamID := uint32(2)
	testStreamID := uint32(3)

	// Add parentStream (1), child of 0
	err := pt.AddStream(parentStreamID, nil)
	if err != nil {
		t.Fatalf("Failed to add parentStreamID %d: %v", parentStreamID, err)
	}

	// Add siblingStream (2) as child of parentStream (1)
	siblingPrioInfo := &streamDependencyInfo{
		StreamDependency: parentStreamID,
		Weight:           10, // Specific weight for sibling
		Exclusive:        false,
	}
	err = pt.AddStream(siblingStreamID, siblingPrioInfo)
	if err != nil {
		t.Fatalf("Failed to add siblingStreamID %d: %v", siblingStreamID, err)
	}

	// Define priority for testStream (3)
	testPrioInfo := &streamDependencyInfo{
		StreamDependency: parentStreamID,
		Weight:           100,
		Exclusive:        false,
	}

	// Add testStream (3) with specific, non-exclusive priority under parentStream (1)
	err = pt.AddStream(testStreamID, testPrioInfo)
	if err != nil {
		t.Fatalf("AddStream for testStreamID %d failed: %v", testStreamID, err)
	}

	// Verify testStream (3)
	tsParentID, tsChildrenIDs, tsWeight, errGetTS := pt.GetDependencies(testStreamID)
	if errGetTS != nil {
		t.Fatalf("GetDependencies for testStreamID %d failed: %v", testStreamID, errGetTS)
	}
	if tsParentID != testPrioInfo.StreamDependency {
		t.Errorf("testStreamID %d: expected parent %d, got %d", testStreamID, testPrioInfo.StreamDependency, tsParentID)
	}
	if tsWeight != testPrioInfo.Weight {
		t.Errorf("testStreamID %d: expected weight %d, got %d", testStreamID, testPrioInfo.Weight, tsWeight)
	}
	if len(tsChildrenIDs) != 0 {
		t.Errorf("testStreamID %d: expected no children initially, got %v", testStreamID, tsChildrenIDs)
	}

	// Verify parentStream (1)
	// It should now have both siblingStream (2) and testStream (3) as children
	_, pChildren, _, errGetP := pt.GetDependencies(parentStreamID)
	if errGetP != nil {
		t.Fatalf("GetDependencies for parentStreamID %d failed: %v", parentStreamID, errGetP)
	}
	expectedParentChildren := []uint32{siblingStreamID, testStreamID}
	if !reflect.DeepEqual(sortUint32Slice(pChildren), sortUint32Slice(expectedParentChildren)) {
		t.Errorf("parentStreamID %d: expected children %v, got %v", parentStreamID, expectedParentChildren, pChildren)
	}

	// Verify siblingStream (2) - should be unaffected
	ssParentID, ssChildrenIDs, ssWeight, errGetSS := pt.GetDependencies(siblingStreamID)
	if errGetSS != nil {
		t.Fatalf("GetDependencies for siblingStreamID %d failed: %v", siblingStreamID, errGetSS)
	}
	if ssParentID != parentStreamID {
		t.Errorf("siblingStreamID %d: expected parent %d (unaffected), got %d", siblingStreamID, parentStreamID, ssParentID)
	}
	if ssWeight != siblingPrioInfo.Weight {
		t.Errorf("siblingStreamID %d: expected weight %d (unaffected), got %d", siblingStreamID, siblingPrioInfo.Weight, ssWeight)
	}
	if len(ssChildrenIDs) != 0 {
		t.Errorf("siblingStreamID %d: expected no children (unaffected), got %v", siblingStreamID, ssChildrenIDs)
	}
}

func TestPriorityTree_AddStream_ExclusivePriority(t *testing.T) {
	pt := NewPriorityTree()

	// Setup:
	// Stream 1 (parent for new exclusive stream)
	// Stream 1 will have children 2 and 3.
	// New stream 4 will be added as exclusive child of 1.
	_ = pt.AddStream(1, nil)                                                                      // Parent stream, child of 0
	_ = pt.AddStream(2, &streamDependencyInfo{StreamDependency: 1, Weight: 10, Exclusive: false}) // Child of 1
	_ = pt.AddStream(3, &streamDependencyInfo{StreamDependency: 1, Weight: 20, Exclusive: false}) // Child of 1

	// Verify initial state for stream 1
	_, s1InitialChildren, _, _ := pt.GetDependencies(1)
	if !reflect.DeepEqual(sortUint32Slice(s1InitialChildren), []uint32{2, 3}) {
		t.Fatalf("Pre-condition: Stream 1 initial children expected [2,3], got %v", s1InitialChildren)
	}

	streamID_X := uint32(4)
	prioInfo_X := &streamDependencyInfo{
		StreamDependency: 1,  // Parent is stream 1
		Weight:           50, // Weight for stream 4
		Exclusive:        true,
	}

	err := pt.AddStream(streamID_X, prioInfo_X)
	if err != nil {
		t.Fatalf("AddStream for exclusive stream %d failed: %v", streamID_X, err)
	}

	// Verify stream X (4)
	sX_parent, sX_children, sX_weight, errGetX := pt.GetDependencies(streamID_X)
	if errGetX != nil {
		t.Fatalf("GetDependencies for stream %d (X) failed: %v", streamID_X, errGetX)
	}
	if sX_parent != prioInfo_X.StreamDependency { // Parent should be 1
		t.Errorf("Stream %d (X): expected parent %d, got %d", streamID_X, prioInfo_X.StreamDependency, sX_parent)
	}
	if sX_weight != prioInfo_X.Weight {
		t.Errorf("Stream %d (X): expected weight %d, got %d", streamID_X, prioInfo_X.Weight, sX_weight)
	}
	// Children of X should be the former children of stream 1 (i.e., 2 and 3)
	expected_sX_children := []uint32{2, 3}
	if !reflect.DeepEqual(sortUint32Slice(sX_children), sortUint32Slice(expected_sX_children)) {
		t.Errorf("Stream %d (X): expected children %v, got %v", streamID_X, expected_sX_children, sX_children)
	}

	// Verify stream P (1 - parent of X)
	sP_parent, sP_children, _, errGetP := pt.GetDependencies(prioInfo_X.StreamDependency)
	if errGetP != nil {
		t.Fatalf("GetDependencies for stream %d (P) failed: %v", prioInfo_X.StreamDependency, errGetP)
	}
	if sP_parent != 0 { // Stream 1's parent is 0
		t.Errorf("Stream %d (P): expected parent 0, got %d", prioInfo_X.StreamDependency, sP_parent)
	}
	// Stream X (4) should be the sole child of P (1)
	expected_sP_children := []uint32{streamID_X}
	if !reflect.DeepEqual(sortUint32Slice(sP_children), sortUint32Slice(expected_sP_children)) {
		t.Errorf("Stream %d (P): expected children %v, got %v", prioInfo_X.StreamDependency, expected_sP_children, sP_children)
	}

	// Verify stream C1 (2 - former child of P, now child of X)
	sC1_parent, _, sC1_weight, _ := pt.GetDependencies(2)
	if sC1_parent != streamID_X {
		t.Errorf("Stream 2 (C1): expected parent %d (X), got %d", streamID_X, sC1_parent)
	}
	if sC1_weight != 10 { // Weight should be preserved
		t.Errorf("Stream 2 (C1): expected weight 10, got %d", sC1_weight)
	}

	// Verify stream C2 (3 - former child of P, now child of X)
	sC2_parent, _, sC2_weight, _ := pt.GetDependencies(3)
	if sC2_parent != streamID_X {
		t.Errorf("Stream 3 (C2): expected parent %d (X), got %d", streamID_X, sC2_parent)
	}
	if sC2_weight != 20 { // Weight should be preserved
		t.Errorf("Stream 3 (C2): expected weight 20, got %d", sC2_weight)
	}
}

func TestPriorityTree_AddStream_SelfDependencyError(t *testing.T) {
	pt := NewPriorityTree()
	streamID := uint32(3)
	prioInfo := &streamDependencyInfo{
		StreamDependency: streamID, // Self-dependency
		Weight:           100,
		Exclusive:        false,
	}

	err := pt.AddStream(streamID, prioInfo)
	if err == nil {
		t.Fatalf("AddStream with self-dependency should have failed, but didn't")
	}

	streamErr, ok := err.(*StreamError)
	if !ok {
		t.Fatalf("Expected StreamError, got %T: %v", err, err)
	}
	if streamErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ProtocolError, got %v", streamErr.Code)
	}
	if streamErr.StreamID != streamID {
		t.Errorf("Expected error for stream %d, got for stream %d", streamID, streamErr.StreamID)

		expectedMsgPart := "cannot depend on itself"
		if !strings.Contains(streamErr.Msg, expectedMsgPart) {
			t.Errorf("Expected StreamError message for stream %d to contain '%s', got '%s'", streamID, expectedMsgPart, streamErr.Msg)
		}
	}
	// t.Logf("Got expected error: %v", err)
}

func TestPriorityTree_AddStream_ParentDoesNotExist(t *testing.T) {
	pt := NewPriorityTree()

	nonExistentParentID := uint32(5)
	childStreamID := uint32(6)
	childWeight := uint8(77)

	prioInfo := &streamDependencyInfo{
		StreamDependency: nonExistentParentID,
		Weight:           childWeight,
		Exclusive:        false,
	}

	err := pt.AddStream(childStreamID, prioInfo)
	if err != nil {
		t.Fatalf("AddStream for childStreamID %d failed: %v", childStreamID, err)
	}

	// Verify childStreamID (6)
	childParent, _, childActualWeight, errGetChild := pt.GetDependencies(childStreamID)
	if errGetChild != nil {
		t.Fatalf("GetDependencies for childStreamID %d failed: %v", childStreamID, errGetChild)
	}
	if childParent != nonExistentParentID {
		t.Errorf("Child stream %d: expected parent %d, got %d", childStreamID, nonExistentParentID, childParent)
	}
	if childActualWeight != childWeight {
		t.Errorf("Child stream %d: expected weight %d, got %d", childStreamID, childWeight, childActualWeight)
	}

	// Verify nonExistentParentID (5) - should have been created
	createdParentParentID, createdParentChildren, createdParentWeight, errGetCreatedParent := pt.GetDependencies(nonExistentParentID)
	if errGetCreatedParent != nil {
		t.Fatalf("GetDependencies for created parent stream %d failed: %v", nonExistentParentID, errGetCreatedParent)
	}
	if createdParentParentID != 0 { // Implicitly created parents should depend on stream 0
		t.Errorf("Created parent stream %d: expected parent 0, got %d", nonExistentParentID, createdParentParentID)
	}
	if createdParentWeight != 15 { // Default weight for implicitly created parent
		t.Errorf("Created parent stream %d: expected default weight 15, got %d", nonExistentParentID, createdParentWeight)
	}
	expectedCreatedParentChildren := []uint32{childStreamID}
	if !reflect.DeepEqual(sortUint32Slice(createdParentChildren), sortUint32Slice(expectedCreatedParentChildren)) {
		t.Errorf("Created parent stream %d: expected children %v, got %v", nonExistentParentID, expectedCreatedParentChildren, createdParentChildren)
	}

	// Verify stream 0 has the created parent as a child
	_, stream0Children, _, errGetRoot := pt.GetDependencies(0)
	if errGetRoot != nil {
		t.Fatalf("GetDependencies for stream 0 failed: %v", errGetRoot)
	}
	found := false
	for _, child := range stream0Children {
		if child == nonExistentParentID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Created parent stream %d not found in children of stream 0. Children: %v", nonExistentParentID, stream0Children)
	}
}

func TestPriorityTree_AddStream_Stream0Error(t *testing.T) {
	pt := NewPriorityTree()

	err := pt.AddStream(0, nil) // Attempt to add stream 0
	if err == nil {
		t.Fatalf("AddStream(0, nil) should have failed, but returned no error")
	}

	connErr, ok := err.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected AddStream(0, nil) to return *ConnectionError, got %T: %v", err, err)
	}

	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ConnectionError with code ErrCodeProtocolError, got %v", connErr.Code)
	}

	expectedMsg := "cannot add or modify priority for stream 0 via AddStream"
	if !strings.Contains(connErr.Msg, expectedMsg) {
		t.Errorf("Expected ConnectionError message to contain '%s', got '%s'", expectedMsg, connErr.Msg)
	}
}

func TestPriorityTree_ProcessPriorityFrame_NonExclusive_UpdateExistingStream(t *testing.T) {
	pt := NewPriorityTree()
	// Add stream 1 and 3, both children of 0 initially
	_ = pt.AddStream(1, nil)
	_ = pt.AddStream(3, nil)

	// Stream 5 will be processed by PRIORITY frame
	// It will initially be child of 0
	_ = pt.getOrCreateNodeNoLock(5) // ensure stream 5 exists for test simplicity

	frame := &PriorityFrame{
		FrameHeader:      FrameHeader{StreamID: 5}, // PRIORITY frame for stream 5
		Exclusive:        false,
		StreamDependency: 1, // Make stream 5 dependent on stream 1
		Weight:           50,
	}

	err := pt.ProcessPriorityFrame(frame)
	if err != nil {
		t.Fatalf("ProcessPriorityFrame failed: %v", err)
	}

	parentID, children, weight, errGet := pt.GetDependencies(5)
	if errGet != nil {
		t.Fatalf("GetDependencies for stream 5 failed: %v", errGet)
	}
	if parentID != 1 {
		t.Errorf("Expected stream 5 parent to be 1, got %d", parentID)
	}
	if weight != 50 {
		t.Errorf("Expected stream 5 weight to be 50, got %d", weight)
	}
	if len(children) != 0 {
		t.Errorf("Expected stream 5 to have no children, got %v", children)
	}

	// Verify stream 1 now has 5 as a child
	_, stream1Children, _, _ := pt.GetDependencies(1)
	found := false
	for _, childID := range stream1Children {
		if childID == 5 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Stream 5 not found in children of stream 1. Children: %v", stream1Children)
	}
}

func TestPriorityTree_ProcessPriorityFrame_Exclusive(t *testing.T) {
	pt := NewPriorityTree()

	// Setup:
	// Target parent for exclusive operation: Stream 1 (parent 0)
	//   Children of Stream 1: Stream 3 (w:10), Stream 4 (w:20)
	if err := pt.AddStream(1, nil); err != nil { // 0 -> 1
		t.Fatalf("Setup: AddStream(1) failed: %v", err)
	}
	if err := pt.AddStream(3, &streamDependencyInfo{StreamDependency: 1, Weight: 10}); err != nil { // 1 -> 3
		t.Fatalf("Setup: AddStream(3) failed: %v", err)
	}
	if err := pt.AddStream(4, &streamDependencyInfo{StreamDependency: 1, Weight: 20}); err != nil { // 1 -> 4
		t.Fatalf("Setup: AddStream(4) failed: %v", err)
	}

	// Stream to be reprioritized (Stream 5):
	//   Initially: Stream 7 -> Stream 5 (w:original_s5_weight=10)
	//   Stream 5 has its own child: Stream 5 -> Stream 6 (w:original_s6_weight=5)
	originalS5Weight := uint8(10)
	originalS6Weight := uint8(5)
	if err := pt.AddStream(7, nil); err != nil { // 0 -> 7
		t.Fatalf("Setup: AddStream(7) failed: %v", err)
	}
	if err := pt.AddStream(5, &streamDependencyInfo{StreamDependency: 7, Weight: originalS5Weight}); err != nil { // 7 -> 5
		t.Fatalf("Setup: AddStream(5) failed: %v", err)
	}
	if err := pt.AddStream(6, &streamDependencyInfo{StreamDependency: 5, Weight: originalS6Weight}); err != nil { // 5 -> 6
		t.Fatalf("Setup: AddStream(6) failed: %v", err)
	}

	// PRIORITY frame for stream 5: make it exclusive child of stream 1.
	// New parent for stream 5: 1
	// New weight for stream 5: new_s5_weight=50
	newS5Weight := uint8(50)
	frame := &PriorityFrame{
		FrameHeader:      FrameHeader{StreamID: 5},
		Exclusive:        true,
		StreamDependency: 1, // New parent is stream 1
		Weight:           newS5Weight,
	}

	err := pt.ProcessPriorityFrame(frame)
	if err != nil {
		t.Fatalf("ProcessPriorityFrame failed: %v", err)
	}

	// --- Verifications ---

	// Verify Stream 5 (the exclusively reprioritized stream)
	s5Parent, s5Children, s5Weight, errGet5 := pt.GetDependencies(5)
	if errGet5 != nil {
		t.Fatalf("GetDependencies for stream 5 failed: %v", errGet5)
	}
	if s5Parent != 1 {
		t.Errorf("Stream 5: expected parent 1, got %d", s5Parent)
	}
	if s5Weight != newS5Weight {
		t.Errorf("Stream 5: expected weight %d, got %d", newS5Weight, s5Weight)
	}
	// Children should be its original child (6) and adopted children (3, 4)
	expectedS5Children := []uint32{6, 3, 4}
	if !reflect.DeepEqual(sortUint32Slice(s5Children), sortUint32Slice(expectedS5Children)) {
		t.Errorf("Stream 5: expected children %v (sorted), got %v (sorted). Original: %v",
			sortUint32Slice(expectedS5Children), sortUint32Slice(s5Children), s5Children)
	}

	// Verify Stream 1 (new parent of stream 5)
	s1Parent, s1Children, _, errGet1 := pt.GetDependencies(1)
	if errGet1 != nil {
		t.Fatalf("GetDependencies for stream 1 failed: %v", errGet1)
	}
	if s1Parent != 0 { // Stream 1's parent is 0
		t.Errorf("Stream 1: expected parent 0, got %d", s1Parent)
	}
	// Stream 5 should be the sole child of Stream 1
	expectedS1Children := []uint32{5}
	if !reflect.DeepEqual(sortUint32Slice(s1Children), sortUint32Slice(expectedS1Children)) {
		t.Errorf("Stream 1: expected children %v, got %v", expectedS1Children, s1Children)
	}

	// Verify Stream 7 (original parent of stream 5)
	_, s7Children, _, errGet7 := pt.GetDependencies(7)
	if errGet7 != nil {
		t.Fatalf("GetDependencies for stream 7 failed: %v", errGet7)
	}
	if contains(s7Children, 5) {
		t.Errorf("Stream 7: should no longer have 5 as a child, got children %v", s7Children)
	}
	// (Stream 7 might have other children if they were added, but for this test, it was only parent to 5 initially among numbered streams)

	// Verify Stream 6 (original child of stream 5)
	s6Parent, _, s6Weight, errGet6 := pt.GetDependencies(6)
	if errGet6 != nil {
		t.Fatalf("GetDependencies for stream 6 failed: %v", errGet6)
	}
	if s6Parent != 5 {
		t.Errorf("Stream 6: expected parent 5 (unchanged relationship with 5), got %d", s6Parent)
	}
	if s6Weight != originalS6Weight { // Weight should be preserved
		t.Errorf("Stream 6: expected weight %d (preserved), got %d", originalS6Weight, s6Weight)
	}

	// Verify Stream 3 (original child of stream 1, now child of stream 5)
	s3Parent, _, s3Weight, errGet3 := pt.GetDependencies(3)
	if errGet3 != nil {
		t.Fatalf("GetDependencies for stream 3 failed: %v", errGet3)
	}
	if s3Parent != 5 {
		t.Errorf("Stream 3: expected parent 5, got %d", s3Parent)
	}
	if s3Weight != 10 { // Weight should be preserved (original weight when child of 1)
		t.Errorf("Stream 3: expected weight 10 (preserved), got %d", s3Weight)
	}

	// Verify Stream 4 (original child of stream 1, now child of stream 5)
	s4Parent, _, s4Weight, errGet4 := pt.GetDependencies(4)
	if errGet4 != nil {
		t.Fatalf("GetDependencies for stream 4 failed: %v", errGet4)
	}
	if s4Parent != 5 {
		t.Errorf("Stream 4: expected parent 5, got %d", s4Parent)
	}
	if s4Weight != 20 { // Weight should be preserved (original weight when child of 1)
		t.Errorf("Stream 4: expected weight 20 (preserved), got %d", s4Weight)
	}
}

func TestPriorityTree_ProcessPriorityFrame_Stream0Error(t *testing.T) {
	pt := NewPriorityTree()
	frame := &PriorityFrame{
		FrameHeader:      FrameHeader{StreamID: 0}, // PRIORITY frame FOR stream 0
		Exclusive:        false,
		StreamDependency: 1,
		Weight:           50,
	}

	err := pt.ProcessPriorityFrame(frame)
	if err == nil {
		t.Fatalf("ProcessPriorityFrame on stream 0 should have failed")
	}
	connErr, ok := err.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError, got %T: %v", err, err)
	}
	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ProtocolError, got %v", connErr.Code)
	}
	// t.Logf("Got expected error: %v", err)
}

func TestPriorityTree_ProcessPriorityFrame_SelfDependencyError(t *testing.T) {
	pt := NewPriorityTree()
	streamID := uint32(3)
	_ = pt.AddStream(streamID, nil) // ensure stream exists

	frame := &PriorityFrame{
		FrameHeader:      FrameHeader{StreamID: streamID},
		Exclusive:        false,
		StreamDependency: streamID, // Self-dependency
		Weight:           50,
	}

	err := pt.ProcessPriorityFrame(frame)
	if err == nil {
		t.Fatalf("ProcessPriorityFrame with self-dependency should have failed")
	}
	streamErr, ok := err.(*StreamError)
	if !ok {
		t.Fatalf("Expected StreamError, got %T: %v", err, err)
	}
	if streamErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ProtocolError, got %v", streamErr.Code)
	}
	if streamErr.StreamID != streamID {

		expectedMsgPart := "cannot depend on itself in PRIORITY frame"
		if !strings.Contains(streamErr.Msg, expectedMsgPart) {
			t.Errorf("Expected StreamError message for stream %d to contain '%s', got '%s'", streamID, expectedMsgPart, streamErr.Msg)
		}
		t.Errorf("Expected error for stream %d, got for stream %d", streamID, streamErr.StreamID)
	}
	// t.Logf("Got expected error: %v", err)
}

func TestPriorityTree_ProcessPriorityFrame_StreamDoesNotExist(t *testing.T) {
	pt := NewPriorityTree()

	nonExistentStreamID := uint32(7)
	parentStreamID := uint32(1) // This stream will be the parent
	priorityWeight := uint8(66)
	isExclusive := false

	// Create the parent stream so it exists
	err := pt.AddStream(parentStreamID, nil)
	if err != nil {
		t.Fatalf("Setup: Failed to add parentStreamID %d: %v", parentStreamID, err)
	}

	frame := &PriorityFrame{
		FrameHeader:      FrameHeader{StreamID: nonExistentStreamID},
		Exclusive:        isExclusive,
		StreamDependency: parentStreamID,
		Weight:           priorityWeight,
	}

	err = pt.ProcessPriorityFrame(frame)
	if err != nil {
		t.Fatalf("ProcessPriorityFrame for non-existent stream %d failed: %v", nonExistentStreamID, err)
	}

	// Verify the new stream (7)
	pID, children, w, errGet := pt.GetDependencies(nonExistentStreamID)
	if errGet != nil {
		t.Fatalf("GetDependencies for newly created stream %d failed: %v", nonExistentStreamID, errGet)
	}

	if pID != parentStreamID {
		t.Errorf("Stream %d: expected parent %d, got %d", nonExistentStreamID, parentStreamID, pID)
	}
	if w != priorityWeight {
		t.Errorf("Stream %d: expected weight %d, got %d", nonExistentStreamID, priorityWeight, w)
	}
	if len(children) != 0 {
		t.Errorf("Stream %d: expected no children, got %v", nonExistentStreamID, children)
	}

	// Verify the parent stream (1) now has stream 7 as a child
	_, parentChildren, _, errGetParent := pt.GetDependencies(parentStreamID)
	if errGetParent != nil {
		t.Fatalf("GetDependencies for parent stream %d failed: %v", parentStreamID, errGetParent)
	}

	found := false
	for _, childID := range parentChildren {
		if childID == nonExistentStreamID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Stream %d not found in children of stream %d. Children: %v", nonExistentStreamID, parentStreamID, parentChildren)
	}
}

func TestPriorityTree_RemoveStream_Simple(t *testing.T) {
	pt := NewPriorityTree()
	// 0 -> 1
	//   -> 3 (to be removed)
	//   -> 5
	_ = pt.AddStream(1, nil)
	_ = pt.AddStream(3, nil)
	_ = pt.AddStream(5, nil)

	parentPre, childrenPre, weightPre, _ := pt.GetDependencies(3)
	if parentPre != 0 || weightPre != 15 || len(childrenPre) != 0 {
		t.Fatalf("Pre-condition for stream 3 failed: parent %d, weight %d, children %v", parentPre, weightPre, childrenPre)
	}

	_, stream0ChildrenPre, _, _ := pt.GetDependencies(0)
	if !contains(stream0ChildrenPre, 3) {
		t.Fatalf("Pre-condition: Stream 3 not child of stream 0. Children: %v", stream0ChildrenPre)
	}

	err := pt.RemoveStream(3)
	if err != nil {
		t.Fatalf("RemoveStream(3) failed: %v", err)
	}

	_, _, _, errGet := pt.GetDependencies(3)
	if errGet == nil {
		t.Errorf("Stream 3 should not be found after removal, but GetDependencies succeeded")
	} else if !strings.Contains(errGet.Error(), "not found") {
		t.Errorf("Expected 'not found' error, got: %v", errGet)
	}

	_, stream0ChildrenPost, _, _ := pt.GetDependencies(0)
	expectedStream0Children := []uint32{1, 5}
	if !reflect.DeepEqual(sortUint32Slice(stream0ChildrenPost), sortUint32Slice(expectedStream0Children)) {
		t.Errorf("Stream 0 children: expected %v, got %v", expectedStream0Children, stream0ChildrenPost)
	}
}

func TestPriorityTree_RemoveStream_WithChildrenReparenting(t *testing.T) {
	pt := NewPriorityTree()
	// Structure:
	// 0 -> 1 (to be removed)
	//      -> 2 (child of 1)
	//      -> 3 (child of 1)
	//   -> 4 (sibling of 1)

	_ = pt.AddStream(1, nil) // parent 0
	_ = pt.AddStream(2, &streamDependencyInfo{StreamDependency: 1, Weight: 10})
	_ = pt.AddStream(3, &streamDependencyInfo{StreamDependency: 1, Weight: 20})
	_ = pt.AddStream(4, nil) // parent 0

	// Verify initial state
	s1p, s1c, s1w, _ := pt.GetDependencies(1)
	if s1p != 0 || !reflect.DeepEqual(sortUint32Slice(s1c), []uint32{2, 3}) || s1w != 15 {
		t.Fatalf("Pre-Remove Stream 1: P:%d C:%v W:%d. Expected P:0 C:[2 3] W:15", s1p, s1c, s1w)
	}
	s2p, s2c, s2w, _ := pt.GetDependencies(2)
	if s2p != 1 || len(s2c) != 0 || s2w != 10 {
		t.Fatalf("Pre-Remove Stream 2: P:%d C:%v W:%d. Expected P:1 C:[] W:10", s2p, s2c, s2w)
	}
	s3p, s3c, s3w, _ := pt.GetDependencies(3)
	if s3p != 1 || len(s3c) != 0 || s3w != 20 {
		t.Fatalf("Pre-Remove Stream 3: P:%d C:%v W:%d. Expected P:1 C:[] W:20", s3p, s3c, s3w)
	}
	s4p, s4c, s4w, _ := pt.GetDependencies(4)
	if s4p != 0 || len(s4c) != 0 || s4w != 15 {
		t.Fatalf("Pre-Remove Stream 4: P:%d C:%v W:%d. Expected P:0 C:[] W:15", s4p, s4c, s4w)
	}

	err := pt.RemoveStream(1) // Remove stream 1
	if err != nil {
		t.Fatalf("RemoveStream(1) failed: %v", err)
	}

	// Stream 1 should be gone
	_, _, _, errGet1 := pt.GetDependencies(1)
	if errGet1 == nil {
		t.Errorf("Stream 1 should not be found after removal")
	}

	// Stream 2 should now be child of 0 (stream 1's parent)
	s2Parent, _, s2Weight, _ := pt.GetDependencies(2)
	if s2Parent != 0 {
		t.Errorf("Stream 2: expected parent 0, got %d", s2Parent)
	}
	if s2Weight != 10 { // Weight preserved
		t.Errorf("Stream 2: expected weight 10, got %d", s2Weight)
	}

	// Stream 3 should now be child of 0
	s3Parent, _, s3Weight, _ := pt.GetDependencies(3)
	if s3Parent != 0 {
		t.Errorf("Stream 3: expected parent 0, got %d", s3Parent)
	}
	if s3Weight != 20 { // Weight preserved
		t.Errorf("Stream 3: expected weight 20, got %d", s3Weight)
	}

	// Stream 4 should still be child of 0
	s4Parent, _, s4Weight, _ := pt.GetDependencies(4)
	if s4Parent != 0 {
		t.Errorf("Stream 4: expected parent 0, got %d", s4Parent)
	}
	if s4Weight != 15 {
		t.Errorf("Stream 4: expected weight 15, got %d", s4Weight)
	}

	// Stream 0 should now have 2, 3, 4 as children
	_, stream0Children, _, _ := pt.GetDependencies(0)
	expectedStream0Children := []uint32{2, 3, 4}
	if !reflect.DeepEqual(sortUint32Slice(stream0Children), sortUint32Slice(expectedStream0Children)) {
		t.Errorf("Stream 0 children: expected %v, got %v", expectedStream0Children, stream0Children)
	}
}

func TestPriorityTree_RemoveStream_WithIntermediateParentAndChildrenReparenting(t *testing.T) {
	pt := NewPriorityTree()

	parentStreamID := uint32(1)
	streamToBeRemovedID := uint32(2)
	childOfRemoved1ID := uint32(3)
	childOfRemoved2ID := uint32(4)
	siblingOfRemovedID := uint32(5) // Sibling to streamToBeRemovedID, child of parentStreamID

	weightR := uint8(50)
	weightC1 := uint8(10)
	weightC2 := uint8(20)
	weightS := uint8(30)

	// Setup:
	// 0 -> parentStreamID (1)
	//      -> streamToBeRemovedID (2, w:50)
	//           -> childOfRemoved1ID (3, w:10)
	//           -> childOfRemoved2ID (4, w:20)
	//      -> siblingOfRemovedID (5, w:30)

	_ = pt.AddStream(parentStreamID, nil) // 0 -> 1 (default weight 15)
	_ = pt.AddStream(streamToBeRemovedID, &streamDependencyInfo{StreamDependency: parentStreamID, Weight: weightR})
	_ = pt.AddStream(childOfRemoved1ID, &streamDependencyInfo{StreamDependency: streamToBeRemovedID, Weight: weightC1})
	_ = pt.AddStream(childOfRemoved2ID, &streamDependencyInfo{StreamDependency: streamToBeRemovedID, Weight: weightC2})
	_ = pt.AddStream(siblingOfRemovedID, &streamDependencyInfo{StreamDependency: parentStreamID, Weight: weightS})

	// Verify initial state
	// Parent Stream (1)
	p1Parent, p1Children, p1Weight, _ := pt.GetDependencies(parentStreamID)
	if p1Parent != 0 || !reflect.DeepEqual(sortUint32Slice(p1Children), []uint32{streamToBeRemovedID, siblingOfRemovedID}) || p1Weight != 15 {
		t.Fatalf("Pre-Remove ParentStream (1): P:%d C:%v W:%d. Expected P:0 C:[%d %d] W:15", p1Parent, p1Children, p1Weight, streamToBeRemovedID, siblingOfRemovedID)
	}
	// Stream To Be Removed (2)
	rParent, rChildren, rWeight, _ := pt.GetDependencies(streamToBeRemovedID)
	if rParent != parentStreamID || !reflect.DeepEqual(sortUint32Slice(rChildren), []uint32{childOfRemoved1ID, childOfRemoved2ID}) || rWeight != weightR {
		t.Fatalf("Pre-Remove StreamToBeRemoved (2): P:%d C:%v W:%d. Expected P:%d C:[%d %d] W:%d", rParent, rChildren, rWeight, parentStreamID, childOfRemoved1ID, childOfRemoved2ID, weightR)
	}
	// Child 1 of Removed (3)
	c1Parent, c1Children, c1Weight, _ := pt.GetDependencies(childOfRemoved1ID)
	if c1Parent != streamToBeRemovedID || len(c1Children) != 0 || c1Weight != weightC1 {
		t.Fatalf("Pre-Remove Child1 (3): P:%d C:%v W:%d. Expected P:%d C:[] W:%d", c1Parent, c1Children, c1Weight, streamToBeRemovedID, weightC1)
	}
	// Child 2 of Removed (4)
	c2Parent, c2Children, c2Weight, _ := pt.GetDependencies(childOfRemoved2ID)
	if c2Parent != streamToBeRemovedID || len(c2Children) != 0 || c2Weight != weightC2 {
		t.Fatalf("Pre-Remove Child2 (4): P:%d C:%v W:%d. Expected P:%d C:[] W:%d", c2Parent, c2Children, c2Weight, streamToBeRemovedID, weightC2)
	}
	// Sibling of Removed (5)
	sParent, sChildren, sWeight, _ := pt.GetDependencies(siblingOfRemovedID)
	if sParent != parentStreamID || len(sChildren) != 0 || sWeight != weightS {
		t.Fatalf("Pre-Remove Sibling (5): P:%d C:%v W:%d. Expected P:%d C:[] W:%d", sParent, sChildren, sWeight, parentStreamID, weightS)
	}

	// Remove streamToBeRemovedID (2)
	err := pt.RemoveStream(streamToBeRemovedID)
	if err != nil {
		t.Fatalf("RemoveStream(%d) failed: %v", streamToBeRemovedID, err)
	}

	// Verify final state
	// StreamToBeRemovedID (2) should be gone
	_, _, _, errGetRemoved := pt.GetDependencies(streamToBeRemovedID)
	if errGetRemoved == nil {
		t.Errorf("Stream %d should not be found after removal", streamToBeRemovedID)
	}

	// ParentStreamID (1) should now have siblingOfRemovedID, childOfRemoved1ID, childOfRemoved2ID as children
	p1ParentPost, p1ChildrenPost, p1WeightPost, _ := pt.GetDependencies(parentStreamID)
	if p1ParentPost != 0 {
		t.Errorf("ParentStream (1) post-remove: expected parent 0, got %d", p1ParentPost)
	}
	if p1WeightPost != 15 { // Weight of parent itself should be unchanged
		t.Errorf("ParentStream (1) post-remove: expected weight 15, got %d", p1WeightPost)
	}
	expectedP1ChildrenPost := []uint32{siblingOfRemovedID, childOfRemoved1ID, childOfRemoved2ID}
	if !reflect.DeepEqual(sortUint32Slice(p1ChildrenPost), sortUint32Slice(expectedP1ChildrenPost)) {
		t.Errorf("ParentStream (1) post-remove children: expected %v, got %v", expectedP1ChildrenPost, p1ChildrenPost)
	}

	// childOfRemoved1ID (3) should now be child of parentStreamID (1), weight preserved
	c1ParentPost, _, c1WeightPost, _ := pt.GetDependencies(childOfRemoved1ID)
	if c1ParentPost != parentStreamID {
		t.Errorf("Child1 (3) post-remove: expected parent %d, got %d", parentStreamID, c1ParentPost)
	}
	if c1WeightPost != weightC1 {
		t.Errorf("Child1 (3) post-remove: expected weight %d, got %d", weightC1, c1WeightPost)
	}

	// childOfRemoved2ID (4) should now be child of parentStreamID (1), weight preserved
	c2ParentPost, _, c2WeightPost, _ := pt.GetDependencies(childOfRemoved2ID)
	if c2ParentPost != parentStreamID {
		t.Errorf("Child2 (4) post-remove: expected parent %d, got %d", parentStreamID, c2ParentPost)
	}
	if c2WeightPost != weightC2 {
		t.Errorf("Child2 (4) post-remove: expected weight %d, got %d", weightC2, c2WeightPost)
	}

	// siblingOfRemovedID (5) should still be child of parentStreamID (1), properties preserved
	sParentPost, _, sWeightPost, _ := pt.GetDependencies(siblingOfRemovedID)
	if sParentPost != parentStreamID {
		t.Errorf("Sibling (5) post-remove: expected parent %d, got %d", parentStreamID, sParentPost)
	}
	if sWeightPost != weightS {
		t.Errorf("Sibling (5) post-remove: expected weight %d, got %d", weightS, sWeightPost)
	}
}

func TestPriorityTree_RemoveStream_NonExistent(t *testing.T) {
	pt := NewPriorityTree()
	err := pt.RemoveStream(99) // Stream 99 does not exist
	if err != nil {
		t.Fatalf("RemoveStream for non-existent stream should not error, got %v", err)
	}
}

func TestPriorityTree_RemoveStream_Stream0Error(t *testing.T) {
	pt := NewPriorityTree()
	err := pt.RemoveStream(0)
	if err == nil {
		t.Fatalf("RemoveStream(0) should have failed")
	}
	connErr, ok := err.(*ConnectionError)
	if !ok {
		t.Fatalf("Expected ConnectionError, got %T: %v", err, err)
	}
	if connErr.Code != ErrCodeProtocolError {
		t.Errorf("Expected ProtocolError, got %v", connErr.Code)
	}
	// t.Logf("Got expected error: %v", err)
}

func TestPriorityTree_RemoveStream_LeafNode(t *testing.T) {
	pt := NewPriorityTree()
	// Structure:
	// 0 -> 1 (parentStream)
	//      -> 2 (leafStream, to be removed)
	//   -> 3 (siblingOfParentStream)

	parentStreamID := uint32(1)
	leafStreamID := uint32(2)
	siblingStreamID := uint32(3)

	// Add streams
	_ = pt.AddStream(parentStreamID, nil)                                                               // 0 -> 1
	_ = pt.AddStream(leafStreamID, &streamDependencyInfo{StreamDependency: parentStreamID, Weight: 10}) // 1 -> 2
	_ = pt.AddStream(siblingStreamID, nil)                                                              // 0 -> 3

	// Verify initial state for parentStreamID (1)
	_, pChildrenPre, _, errGetPPre := pt.GetDependencies(parentStreamID)
	if errGetPPre != nil {
		t.Fatalf("Pre-Remove: GetDependencies for parentStreamID %d failed: %v", parentStreamID, errGetPPre)
	}
	if !contains(pChildrenPre, leafStreamID) {
		t.Fatalf("Pre-Remove: leafStreamID %d not found in children of parentStreamID %d. Children: %v", leafStreamID, parentStreamID, pChildrenPre)
	}

	// Verify initial state for leafStreamID (2)
	lParentPre, lChildrenPre, lWeightPre, errGetLPre := pt.GetDependencies(leafStreamID)
	if errGetLPre != nil {
		t.Fatalf("Pre-Remove: GetDependencies for leafStreamID %d failed: %v", leafStreamID, errGetLPre)
	}
	if lParentPre != parentStreamID {
		t.Fatalf("Pre-Remove: leafStreamID %d parent expected %d, got %d", leafStreamID, parentStreamID, lParentPre)
	}
	if len(lChildrenPre) != 0 {
		t.Fatalf("Pre-Remove: leafStreamID %d expected no children, got %v", leafStreamID, lChildrenPre)
	}
	if lWeightPre != 10 {
		t.Fatalf("Pre-Remove: leafStreamID %d expected weight 10, got %d", leafStreamID, lWeightPre)
	}

	// Remove the leaf stream
	err := pt.RemoveStream(leafStreamID)
	if err != nil {
		t.Fatalf("RemoveStream(%d) failed: %v", leafStreamID, err)
	}

	// Verify leafStreamID is removed from the tree
	_, _, _, errGetLeafPost := pt.GetDependencies(leafStreamID)
	if errGetLeafPost == nil {
		t.Errorf("leafStreamID %d should not be found after removal, but GetDependencies succeeded", leafStreamID)
	} else if !strings.Contains(errGetLeafPost.Error(), "not found") {
		t.Errorf("Expected 'not found' error for leafStreamID %d, got: %v", leafStreamID, errGetLeafPost)
	}

	// Verify parentStreamID no longer has leafStreamID as a child
	_, pChildrenPost, _, errGetPPost := pt.GetDependencies(parentStreamID)
	if errGetPPost != nil {
		t.Fatalf("Post-Remove: GetDependencies for parentStreamID %d failed: %v", parentStreamID, errGetPPost)
	}
	if contains(pChildrenPost, leafStreamID) {
		t.Errorf("Post-Remove: leafStreamID %d still found in children of parentStreamID %d. Children: %v", leafStreamID, parentStreamID, pChildrenPost)
	}
	if len(pChildrenPost) != 0 { // In this setup, parentStream (1) had only leafStream (2) as child.
		t.Errorf("Post-Remove: parentStreamID %d children expected empty after removing its only child, got %v", parentStreamID, pChildrenPost)
	}

	// Verify stream 0 still has parentStreamID and siblingStreamID as children
	_, stream0Children, _, errGetRoot := pt.GetDependencies(0)
	if errGetRoot != nil {
		t.Fatalf("Post-Remove: GetDependencies for stream 0 failed: %v", errGetRoot)
	}
	expectedStream0Children := []uint32{parentStreamID, siblingStreamID}
	if !reflect.DeepEqual(sortUint32Slice(stream0Children), sortUint32Slice(expectedStream0Children)) {
		t.Errorf("Post-Remove: Stream 0 children: expected %v, got %v", expectedStream0Children, stream0Children)
	}
}

// TestComplexScenario combines multiple operations to check tree integrity.
func TestPriorityTree_ComplexScenario(t *testing.T) {
	pt := NewPriorityTree()

	// 1. Add streams
	// Initial:
	// 0 -> 1 (w:15 def)
	//   -> 2 (w:15 def)
	_ = pt.AddStream(1, nil)
	_ = pt.AddStream(2, nil)

	// 2. Update 1's priority (make it child of 2, w:30)
	// Tree:
	// 0 -> 2 (w:15)
	//      -> 1 (w:30)
	err := pt.UpdatePriority(1, 2, 30, false)
	if err != nil {
		t.Fatalf("Step 2 UpdatePriority(1) failed: %v", err)
	}
	p, c, w, _ := pt.GetDependencies(1)
	if p != 2 || w != 30 || len(c) != 0 {
		t.Errorf("Step 2 S1: P:%d W:%d C:%v. Expected P:2 W:30 C:[]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(2)
	if p != 0 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{1}) {
		t.Errorf("Step 2 S2: P:%d W:%d C:%v. Expected P:0 W:15 C:[1]", p, w, c)
	}

	// 3. Add stream 3 as exclusive child of 2 (w:50)
	// Existing child of 2 (stream 1) should become child of 3.
	// Tree:
	// 0 -> 2 (w:15)
	//      -> 3 (w:50)
	//         -> 1 (w:30, parent changed from 2 to 3)
	err = pt.AddStream(3, &streamDependencyInfo{StreamDependency: 2, Weight: 50, Exclusive: true})
	if err != nil {
		t.Fatalf("Step 3 AddStream(3) exclusive failed: %v", err)
	}
	p, c, w, _ = pt.GetDependencies(3)
	if p != 2 || w != 50 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{1}) {
		t.Errorf("Step 3 S3: P:%d W:%d C:%v. Expected P:2 W:50 C:[1]", p, w, c)
	}
	p, _, _, _ = pt.GetDependencies(1) // Check S1's parent
	if p != 3 {
		t.Errorf("Step 3 S1: P:%d. Expected P:3", p)
	}
	p, c, w, _ = pt.GetDependencies(2) // Check S2's children
	if p != 0 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{3}) {
		t.Errorf("Step 3 S2: P:%d W:%d C:%v. Expected P:0 W:15 C:[3]", p, w, c)
	}

	// 4. Add stream 4 as child of 2 (w:25, non-exclusive)
	// Tree:
	// 0 -> 2 (w:15)
	//      -> 3 (w:50)
	//         -> 1 (w:30)
	//      -> 4 (w:25)
	err = pt.AddStream(4, &streamDependencyInfo{StreamDependency: 2, Weight: 25, Exclusive: false})
	if err != nil {
		t.Fatalf("Step 4 AddStream(4) non-exclusive failed: %v", err)
	}
	p, c, w, _ = pt.GetDependencies(4)
	if p != 2 || w != 25 || len(c) != 0 {
		t.Errorf("Step 4 S4: P:%d W:%d C:%v. Expected P:2 W:25 C:[]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(2) // Check S2's children
	if p != 0 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{3, 4}) {
		t.Errorf("Step 4 S2: P:%d W:%d C:%v. Expected P:0 W:15 C:[3 4]", p, w, c)
	}

	// Step 4.5: Update stream 4 to be an exclusive child of its current parent (stream 2).
	// Stream 3 (other child of 2) should become child of 4.
	// Tree:
	// 0 -> 2 (w:15)
	//      -> 4 (w:25)
	//         -> 3 (w:50)
	//            -> 1 (w:30)
	err = pt.UpdatePriority(4, 2, 25, true) // S4 becomes exclusive child of S2 (its current parent)
	if err != nil {
		t.Fatalf("Step 4.5 UpdatePriority(4) exclusive failed: %v", err)
	}
	p, c, w, _ = pt.GetDependencies(4) // Check S4
	if p != 2 || w != 25 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{3}) {
		t.Errorf("Step 4.5 S4: P:%d W:%d C:%v. Expected P:2 W:25 C:[3]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(3) // Check S3
	if p != 4 || w != 50 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{1}) {
		t.Errorf("Step 4.5 S3: P:%d W:%d C:%v. Expected P:4 W:50 C:[1]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(2) // Check S2
	if p != 0 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{4}) {
		t.Errorf("Step 4.5 S2: P:%d W:%d C:%v. Expected P:0 W:15 C:[4]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(1) // Check S1
	if p != 3 || w != 30 || len(c) != 0 {
		t.Errorf("Step 4.5 S1: P:%d W:%d C:%v. Expected P:3 W:30 C:[]", p, w, c)
	}

	// 5. Remove stream 3
	// Children of 3 (stream 1) should be re-parented to 4 (parent of 3).
	// Tree after 4.5:
	// 0 -> 2 (w:15)
	//      -> 4 (w:25)
	//         -> 3 (w:50)  <-- to be removed
	//            -> 1 (w:30)
	// Becomes:
	// 0 -> 2 (w:15)
	//      -> 4 (w:25)
	//         -> 1 (w:30, parent changed from 3 to 4)
	err = pt.RemoveStream(3)
	if err != nil {
		t.Fatalf("Step 5 RemoveStream(3) failed: %v", err)
	}
	_, _, _, errGet3 := pt.GetDependencies(3)
	if errGet3 == nil {
		t.Errorf("Step 5 S3: Should be removed, but GetDependencies succeeded")
	}
	p, c, w, _ = pt.GetDependencies(1) // Check S1
	if p != 4 || w != 30 || len(c) != 0 {
		t.Errorf("Step 5 S1: P:%d W:%d C:%v. Expected P:4 W:30 C:[]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(4) // Check S4
	if p != 2 || w != 25 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{1}) {
		t.Errorf("Step 5 S4: P:%d W:%d C:%v. Expected P:2 W:25 C:[1]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(2) // Check S2
	if p != 0 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{4}) {
		t.Errorf("Step 5 S2: P:%d W:%d C:%v. Expected P:0 W:15 C:[4]", p, w, c)
	}

	// 5.5. Remove stream 4
	// Children of 4 (stream 1) should be re-parented to 2 (parent of 4).
	// Tree after step 5:
	// 0 -> 2 (w:15)
	//      -> 4 (w:25)  <-- to be removed
	//         -> 1 (w:30)
	// Becomes:
	// 0 -> 2 (w:15)
	//      -> 1 (w:30, parent changed from 4 to 2)
	err = pt.RemoveStream(4)
	if err != nil {
		t.Fatalf("Step 5.5 RemoveStream(4) failed: %v", err)
	}
	_, _, _, errGet4 := pt.GetDependencies(4)
	if errGet4 == nil {
		t.Errorf("Step 5.5 S4: Should be removed, but GetDependencies succeeded")
	}
	p, c, w, _ = pt.GetDependencies(1) // Check S1
	if p != 2 || w != 30 || len(c) != 0 {
		t.Errorf("Step 5.5 S1: P:%d W:%d C:%v. Expected P:2 W:30 C:[]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(2) // Check S2
	if p != 0 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{1}) {
		t.Errorf("Step 5.5 S2: P:%d W:%d C:%v. Expected P:0 W:15 C:[1]", p, w, c)
	}

	// 6. Process PRIORITY frame for stream 2: make it child of 1 (w:60, non-exclusive)
	// Current tree after step 5.5:
	// 0 -> 2 (w:15)
	//      -> 1 (w:30)
	// Operation: S2 to depend on S1. This creates a cycle (S1 is child of S2, S2 wants to be child of S1, effectively 0 -> S1 -> S2 -> S1).
	// More precisely: S1's parent IS S2. If S2's parent becomes S1, then S2 -> S1 -> S2. This is a direct cycle.
	// RFC 7540, Section 5.3.1: "A stream cannot be dependent on any of its own dependencies."
	// This should result in a PROTOCOL_ERROR.
	priorityFrame := &PriorityFrame{
		FrameHeader:      FrameHeader{StreamID: 2}, // Stream 2
		StreamDependency: 1,                        // S2 to depend on S1
		Weight:           60,
		Exclusive:        false,
	}
	err = pt.ProcessPriorityFrame(priorityFrame)
	if err == nil {
		t.Errorf("Step 6 ProcessPriorityFrame(2) for stream 2 to depend on stream 1 should have failed due to cycle creation, but succeeded.")
		// If it succeeded, log the problematic state
		s1p_fail, s1c_fail, s1w_fail, _ := pt.GetDependencies(1)
		s2p_fail, s2c_fail, s2w_fail, _ := pt.GetDependencies(2)
		t.Logf("State after incorrect success: S1(P:%d, C:%v, W:%d), S2(P:%d, C:%v, W:%d)", s1p_fail, s1c_fail, s1w_fail, s2p_fail, s2c_fail, s2w_fail)
	} else {
		streamErr, ok := err.(*StreamError)
		if !ok {
			t.Fatalf("Step 6 Expected StreamError for cycle, got %T: %v", err, err)
		}
		if streamErr.Code != ErrCodeProtocolError {
			t.Errorf("Step 6 Expected ProtocolError for cycle, got code %v (msg: %s)", streamErr.Code, streamErr.Msg)
		}
		if streamErr.StreamID != 2 { // Error should be for stream 2
			t.Errorf("Step 6 Expected error for stream 2, got for stream %d", streamErr.StreamID)
		}
		// If an error occurred, the tree state should not have changed from end of step 5.5.
		// State before this failing operation (end of 5.5):
		// 0 -> 2 (w:15)
		//      -> 1 (w:30)
		p_after_err, c_after_err, w_after_err, _ := pt.GetDependencies(2) // Check S2
		if p_after_err != 0 || w_after_err != 15 || !reflect.DeepEqual(sortUint32Slice(c_after_err), []uint32{1}) {
			t.Errorf("Step 6 S2 (after error): P:%d W:%d C:%v. Expected P:0 W:15 C:[1] (state unchanged)", p_after_err, w_after_err, c_after_err)
		}
		p_after_err, c_after_err, w_after_err, _ = pt.GetDependencies(1) // Check S1
		if p_after_err != 2 || w_after_err != 30 || len(c_after_err) != 0 {
			t.Errorf("Step 6 S1 (after error): P:%d W:%d C:%v. Expected P:2 W:30 C:[] (state unchanged)", p_after_err, w_after_err, c_after_err)
		}

		// --- Start of Enhancement: Series of Exclusive Operations on Stream 0 ---

		// 7. Add stream 5 as a default child of stream 0.
		// Current Tree (end of 5.5):
		// 0 -> 2 (w:15)
		//      -> 1 (w:30)
		// Expected Tree after adding 5:
		// 0 -> 2 (w:15)
		//      -> 1 (w:30)
		//   -> 5 (w:15 def)
		if err := pt.AddStream(5, nil); err != nil {
			t.Fatalf("Step 7 AddStream(5) failed: %v", err)
		}
		p, c, w, _ = pt.GetDependencies(5)
		if p != 0 || w != 15 || len(c) != 0 {
			t.Errorf("Step 7 S5: P:%d W:%d C:%v. Expected P:0 W:15 C:[]", p, w, c)
		}
		_, rootChildren7, _, _ := pt.GetDependencies(0)
		expectedRootChildren7 := []uint32{2, 5} // Stream 1 is child of 2
		if !reflect.DeepEqual(sortUint32Slice(rootChildren7), sortUint32Slice(expectedRootChildren7)) {
			t.Errorf("Step 7 Root children: %v. Expected sorted %v", sortUint32Slice(rootChildren7), sortUint32Slice(expectedRootChildren7))
		}

		// 8. Make stream 5 an exclusive child of stream 0, with new weight 70.
		// Stream 2 (the other direct child of 0) should become a child of stream 5.
		// Stream 1 (child of 2) should remain child of 2.
		// Expected Tree:
		// 0 -> 5 (w:70)
		//      -> 2 (w:15, parent:5)
		//         -> 1 (w:30, parent:2)
		err = pt.UpdatePriority(5, 0, 70, true)
		if err != nil {
			t.Fatalf("Step 8 UpdatePriority(5) exclusive child of 0 failed: %v", err)
		}
		p, c, w, _ = pt.GetDependencies(5) // Check S5
		if p != 0 || w != 70 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{2}) {
			t.Errorf("Step 8 S5: P:%d W:%d C:%v. Expected P:0 W:70 C:[2]", p, w, c)
		}
		p, c, w, _ = pt.GetDependencies(2) // Check S2
		if p != 5 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{1}) {
			t.Errorf("Step 8 S2: P:%d W:%d C:%v. Expected P:5 W:15 C:[1]", p, w, c)
		}
		p, c, w, _ = pt.GetDependencies(1) // Check S1
		if p != 2 || w != 30 || len(c) != 0 {
			t.Errorf("Step 8 S1: P:%d W:%d C:%v. Expected P:2 W:30 C:[]", p, w, c)
		}
		_, rootChildren8, _, _ := pt.GetDependencies(0) // Check Root children
		if !reflect.DeepEqual(sortUint32Slice(rootChildren8), []uint32{5}) {
			t.Errorf("Step 8 Root children: %v. Expected [5]", sortUint32Slice(rootChildren8))
		}

		// 9. Add stream 6 as a default child of stream 0.
		// Expected Tree:
		// 0 -> 5 (w:70)
		//      -> 2 (w:15) -> 1 (w:30)
		//   -> 6 (w:15 def)
		if err := pt.AddStream(6, nil); err != nil {
			t.Fatalf("Step 9 AddStream(6) failed: %v", err)
		}
		p, c, w, _ = pt.GetDependencies(6)
		if p != 0 || w != 15 || len(c) != 0 {
			t.Errorf("Step 9 S6: P:%d W:%d C:%v. Expected P:0 W:15 C:[]", p, w, c)
		}
		_, rootChildren9, _, _ := pt.GetDependencies(0)
		expectedRootChildren9 := []uint32{5, 6}
		if !reflect.DeepEqual(sortUint32Slice(rootChildren9), sortUint32Slice(expectedRootChildren9)) {
			t.Errorf("Step 9 Root children: %v. Expected sorted %v", sortUint32Slice(rootChildren9), sortUint32Slice(expectedRootChildren9))
		}

		// 10. Make stream 6 an exclusive child of stream 0, with new weight 80.
		// Stream 5 (the other direct child of 0) and its subtree should become children of stream 6.
		// Expected Tree:
		// 0 -> 6 (w:80)
		//      -> 5 (w:70, parent:6)
		//         -> 2 (w:15, parent:5)
		//            -> 1 (w:30, parent:2)
		err = pt.UpdatePriority(6, 0, 80, true)
		if err != nil {
			t.Fatalf("Step 10 UpdatePriority(6) exclusive child of 0 failed: %v", err)
		}
		p, c, w, _ = pt.GetDependencies(6) // Check S6
		if p != 0 || w != 80 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{5}) {
			t.Errorf("Step 10 S6: P:%d W:%d C:%v. Expected P:0 W:80 C:[5]", p, w, c)
		}
		p, c, w, _ = pt.GetDependencies(5) // Check S5
		if p != 6 || w != 70 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{2}) {
			t.Errorf("Step 10 S5: P:%d W:%d C:%v. Expected P:6 W:70 C:[2]", p, w, c)
		}
		p, c, w, _ = pt.GetDependencies(2) // Check S2
		if p != 5 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{1}) {
			t.Errorf("Step 10 S2: P:%d W:%d C:%v. Expected P:5 W:15 C:[1]", p, w, c)
		}
		p, c, w, _ = pt.GetDependencies(1) // Check S1
		if p != 2 || w != 30 || len(c) != 0 {
			t.Errorf("Step 10 S1: P:%d W:%d C:%v. Expected P:2 W:30 C:[]", p, w, c)
		}
		_, rootChildren10, _, _ := pt.GetDependencies(0) // Check Root children
		if !reflect.DeepEqual(sortUint32Slice(rootChildren10), []uint32{6}) {
			t.Errorf("Step 10 Root children: %v. Expected [6]", sortUint32Slice(rootChildren10))
		}
		// --- End of Enhancement ---
	}

	// --- Start of Enhancement: Further exclusive re-parenting and complex interaction tests ---
	// State after Step 10 (successful exclusive update of stream 6 as child of 0):
	// 0 -> 6(w80)
	//      -> 5(w70)
	//         -> 2(w15)
	//            -> 1(w30)

	// Step 11: Add stream 20 as a non-exclusive child of stream 5.
	// Expected tree: 0 -> 6(w80) -> 5(w70) -> {2(w15), 20(w15def)}
	//                                      (1 is child of 2)
	if err := pt.AddStream(20, &streamDependencyInfo{StreamDependency: 5, Weight: 15, Exclusive: false}); err != nil {
		t.Fatalf("Step 11 AddStream(20) non-exclusive to S5 failed: %v", err)
	}
	p, c, w, _ = pt.GetDependencies(20)
	if p != 5 || w != 15 || len(c) != 0 {
		t.Errorf("Step 11 S20: P:%d W:%d C:%v. Expected P:5 W:15 C:[]", p, w, c)
	}
	_, s5Children11, _, _ := pt.GetDependencies(5)
	expectedS5Children11 := []uint32{2, 20}
	if !reflect.DeepEqual(sortUint32Slice(s5Children11), sortUint32Slice(expectedS5Children11)) {
		t.Errorf("Step 11 S5 children: %v. Expected sorted %v", sortUint32Slice(s5Children11), sortUint32Slice(expectedS5Children11))
	}

	// Step 12: Add stream 21 as an exclusive child of stream 5, weight 88.
	// Stream 2 and 20 (current children of 5) should become children of 21.
	// Expected tree: 0 -> 6(w80) -> 5(w70) -> 21(w88) -> {2(w15), 20(w15)}
	//                                                (1 is child of 2)
	if err := pt.AddStream(21, &streamDependencyInfo{StreamDependency: 5, Weight: 88, Exclusive: true}); err != nil {
		t.Fatalf("Step 12 AddStream(21) exclusive to S5 failed: %v", err)
	}
	p, c, w, _ = pt.GetDependencies(21)
	if p != 5 || w != 88 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{2, 20}) {
		t.Errorf("Step 12 S21: P:%d W:%d C:%v. Expected P:5 W:88 C:[2 20]", p, w, c)
	}
	_, s5Children12, _, _ := pt.GetDependencies(5)
	if !reflect.DeepEqual(sortUint32Slice(s5Children12), []uint32{21}) {
		t.Errorf("Step 12 S5 children: %v. Expected [21]", sortUint32Slice(s5Children12))
	}
	p20_parent, _, _, _ := pt.GetDependencies(20)
	if p20_parent != 21 {
		t.Errorf("Step 12 S20 parent: %d. Expected 21", p20_parent)
	}
	p2_parent, _, _, _ := pt.GetDependencies(2) // S2 is also child of S21 now
	if p2_parent != 21 {
		t.Errorf("Step 12 S2 parent: %d. Expected 21", p2_parent)
	}

	// Step 13: Remove stream 5.
	// Stream 5 is child of 6. Stream 21 is child of 5.
	// Children of 5 (i.e., stream 21) should be re-parented to 6 (parent of 5).
	// Expected tree: 0 -> 6(w80) -> 21(w88) -> {2(w15), 20(w15)}
	//                                      (1 is child of 2)
	if err := pt.RemoveStream(5); err != nil {
		t.Fatalf("Step 13 RemoveStream(5) failed: %v", err)
	}
	_, _, _, errGet5 := pt.GetDependencies(5)
	if errGet5 == nil {
		t.Errorf("Step 13 S5: Should be removed, but GetDependencies succeeded")
	}
	p, c, w, _ = pt.GetDependencies(21) // Check S21
	if p != 6 || w != 88 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{2, 20}) {
		t.Errorf("Step 13 S21: P:%d W:%d C:%v. Expected P:6 W:88 C:[2 20]", p, w, c)
	}
	_, s6Children13, _, _ := pt.GetDependencies(6)
	if !reflect.DeepEqual(sortUint32Slice(s6Children13), []uint32{21}) {
		t.Errorf("Step 13 S6 children: %v. Expected [21]", sortUint32Slice(s6Children13))
	}

	// Step 14: Stream 1 (deepest child) reprioritized to be non-exclusive child of root (stream 0), weight 5.
	// Original tree: 0 -> 6(w80) -> 21(w88) -> 2(w15) -> 1(w30)
	//                                         -> 20(w15)
	// Expected tree:
	// 0 -> 6(w80) -> 21(w88) -> 2(w15) (S1 no longer child of S2)
	//                         -> 20(w15)
	//   -> 1(w5) (new child of 0)
	err = pt.UpdatePriority(1, 0, 5, false)
	if err != nil {
		t.Fatalf("Step 14 UpdatePriority(1) to child of 0 failed: %v", err)
	}
	p, c, w, _ = pt.GetDependencies(1)
	if p != 0 || w != 5 || len(c) != 0 {
		t.Errorf("Step 14 S1: P:%d W:%d C:%v. Expected P:0 W:5 C:[]", p, w, c)
	}
	_, s2Children14, _, _ := pt.GetDependencies(2) // S2 should no longer have S1 as child
	if contains(s2Children14, 1) {
		t.Errorf("Step 14 S2 children: %v. Expected not to contain 1", s2Children14)
	}
	if len(s2Children14) != 0 { // S2 had only S1 as child.
		t.Errorf("Step 14 S2 children: %v. Expected empty", s2Children14)
	}

	_, rootChildren14, _, _ := pt.GetDependencies(0)
	expectedRootChildren14 := []uint32{1, 6} // S1 is now child of 0, S6 remains.
	if !reflect.DeepEqual(sortUint32Slice(rootChildren14), sortUint32Slice(expectedRootChildren14)) {
		t.Errorf("Step 14 Root children: %v. Expected sorted %v", sortUint32Slice(rootChildren14), sortUint32Slice(expectedRootChildren14))
	}

	// Step 15: Remove stream 2 (which is now a leaf node)
	// Original tree: 0 -> 6(w80) -> 21(w88) -> 2(w15)
	//                                         -> 20(w15)
	//                  -> 1(w5)
	// Expected tree:
	// 0 -> 6(w80) -> 21(w88) -> 20(w15) (S2 removed from S21's children)
	//   -> 1(w5)
	if err := pt.RemoveStream(2); err != nil {
		t.Fatalf("Step 15 RemoveStream(2) failed: %v", err)
	}
	_, _, _, errGet2 := pt.GetDependencies(2)
	if errGet2 == nil {
		t.Errorf("Step 15 S2: Should be removed, but GetDependencies succeeded")
	}
	_, s21Children15, _, _ := pt.GetDependencies(21) // S21 children should now only be S20
	if !reflect.DeepEqual(sortUint32Slice(s21Children15), []uint32{20}) {
		t.Errorf("Step 15 S21 children: %v. Expected [20]", sortUint32Slice(s21Children15))
	}

	// --- End of Further Enhancements ---
}

// Helper to sort uint32 slices for consistent comparison
func sortUint32Slice(s []uint32) []uint32 {
	sorted := make([]uint32, len(s))
	copy(sorted, s)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return sorted
}

// Helper to check if a slice contains an element
func contains(slice []uint32, val uint32) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}

func TestPriorityTree_GetDependencies_StreamNotFound(t *testing.T) {
	t.Run("non_existent_stream_id_greater_than_0_returns_error", func(t *testing.T) {
		pt := NewPriorityTree()
		nonExistentStreamID := uint32(123) // Arbitrary non-existent stream ID > 0
		_, _, _, err := pt.GetDependencies(nonExistentStreamID)

		if err == nil {
			t.Fatalf("GetDependencies(%d): expected an error for non-existent stream, but got nil", nonExistentStreamID)
		}

		expectedErrorMsg := fmt.Sprintf("stream %d not found in priority tree", nonExistentStreamID)
		if err.Error() != expectedErrorMsg {
			t.Errorf("GetDependencies(%d): unexpected error message.\nExpected: %s\nGot:      %s", nonExistentStreamID, expectedErrorMsg, err.Error())
		}
	})
}

func TestPriorityTree_GetDependencies_Stream0(t *testing.T) {
	t.Run("initial_state", func(t *testing.T) {
		pt := NewPriorityTree()
		parentID, children, weight, err := pt.GetDependencies(0)
		if err != nil {
			t.Fatalf("GetDependencies(0) initial state failed: %v", err)
		}
		if parentID != 0 {
			t.Errorf("Stream 0 initial parent: expected 0, got %d", parentID)
		}
		if weight != 0 { // As per NewPriorityTree, stream 0 has weight 0
			t.Errorf("Stream 0 initial weight: expected 0, got %d", weight)
		}
		if len(children) != 0 {
			t.Errorf("Stream 0 initial children: expected empty, got %v", children)
		}
	})

	t.Run("with_children", func(t *testing.T) {
		pt := NewPriorityTree()
		// Add some children to stream 0
		err := pt.AddStream(1, nil) // Default priority, becomes child of 0
		if err != nil {
			t.Fatalf("Failed to add stream 1: %v", err)
		}
		err = pt.AddStream(2, &streamDependencyInfo{StreamDependency: 0, Weight: 20, Exclusive: false}) // Explicitly child of 0
		if err != nil {
			t.Fatalf("Failed to add stream 2: %v", err)
		}

		parentID, children, weight, errGet := pt.GetDependencies(0)
		if errGet != nil {
			t.Fatalf("GetDependencies(0) with children failed: %v", errGet)
		}
		if parentID != 0 {
			t.Errorf("Stream 0 parent with children: expected 0, got %d", parentID)
		}
		if weight != 0 { // As per NewPriorityTree, stream 0 has weight 0
			t.Errorf("Stream 0 weight with children: expected 0, got %d", weight)
		}
		expectedChildren := []uint32{1, 2}
		// GetDependencies returns a copy of children; sort for stable comparison.
		if !reflect.DeepEqual(sortUint32Slice(children), sortUint32Slice(expectedChildren)) {
			t.Errorf("Stream 0 children: expected sorted %v, got sorted %v (original: %v)",
				sortUint32Slice(expectedChildren), sortUint32Slice(children), children)
		}
	})
}

func TestPriorityTree_GetDependencies_ComplexNode(t *testing.T) {
	pt := NewPriorityTree()

	// Setup:
	// 0 -> 1 (parentStream)
	//      -> 2 (targetStream, w:77)
	//         -> 3 (childOfTarget1)
	//         -> 4 (childOfTarget2)
	parentStreamID := uint32(1)
	targetStreamID := uint32(2)
	childOfTarget1ID := uint32(3)
	childOfTarget2ID := uint32(4)
	targetWeight := uint8(77)

	_ = pt.AddStream(parentStreamID, nil)                                                                                             // 0 -> 1
	_ = pt.AddStream(targetStreamID, &streamDependencyInfo{StreamDependency: parentStreamID, Weight: targetWeight, Exclusive: false}) // 1 -> 2
	_ = pt.AddStream(childOfTarget1ID, &streamDependencyInfo{StreamDependency: targetStreamID, Weight: 10, Exclusive: false})         // 2 -> 3
	_ = pt.AddStream(childOfTarget2ID, &streamDependencyInfo{StreamDependency: targetStreamID, Weight: 20, Exclusive: false})         // 2 -> 4

	// Test GetDependencies for targetStreamID (2)
	pID, children, w, err := pt.GetDependencies(targetStreamID)
	if err != nil {
		t.Fatalf("GetDependencies for targetStreamID %d failed: %v", targetStreamID, err)
	}

	if pID != parentStreamID {
		t.Errorf("targetStreamID %d: expected parent %d, got %d", targetStreamID, parentStreamID, pID)
	}

	if w != targetWeight {
		t.Errorf("targetStreamID %d: expected weight %d, got %d", targetStreamID, targetWeight, w)
	}

	expectedChildren := []uint32{childOfTarget1ID, childOfTarget2ID}
	if !reflect.DeepEqual(sortUint32Slice(children), sortUint32Slice(expectedChildren)) {
		t.Errorf("targetStreamID %d: expected children %v, got %v", targetStreamID, expectedChildren, children)
	}
}

func TestPriorityTree_getOrCreateNodeNoLock(t *testing.T) {
	pt := NewPriorityTree() // Has stream 0

	// Create new node
	node1 := pt.getOrCreateNodeNoLock(1)
	if node1 == nil {
		t.Fatal("getOrCreateNodeNoLock(1) returned nil")
	}
	if node1.streamID != 1 {
		t.Errorf("Expected streamID 1, got %d", node1.streamID)
	}
	if node1.parentID != 0 { // Default parent is 0
		t.Errorf("Expected parentID 0, got %d", node1.parentID)
	}
	if node1.weight != 15 { // Default weight 16 (value 15)
		t.Errorf("Expected weight 15, got %d", node1.weight)
	}
	if len(pt.nodes) != 2 { // Stream 0 + Stream 1
		t.Errorf("Expected 2 nodes, got %d", len(pt.nodes))
	}

	// Verify stream 0 has stream 1 as child
	rootNode := pt.nodes[0]
	if !contains(rootNode.childrenIDs, 1) {
		t.Errorf("Stream 0 should have stream 1 as child, children: %v", rootNode.childrenIDs)
	}

	// Get existing node
	node1Again := pt.getOrCreateNodeNoLock(1)
	if node1Again != node1 { // Should be the same instance
		t.Error("getOrCreateNodeNoLock(1) again did not return the same node instance")
	}
	if len(pt.nodes) != 2 { // Should still be 2 nodes
		t.Errorf("Expected 2 nodes after getting existing, got %d", len(pt.nodes))
	}

	// Create node that would be child of another non-0 node if specified by prio, but default is child of 0
	node2 := pt.getOrCreateNodeNoLock(2)
	if node2.parentID != 0 {
		t.Errorf("Node 2 parent should be 0 by default, got %d", node2.parentID)
	}
	if !contains(rootNode.childrenIDs, 2) {
		t.Errorf("Stream 0 should have stream 2 as child, children: %v", rootNode.childrenIDs)
	}
}

func ExamplePriorityTree() {
	// This is a conceptual example of how one might visualize the tree.
	// The PriorityTree itself doesn't have a String() method like this.
	// We can build a string representation based on GetDependencies.

	pt := NewPriorityTree()
	_ = pt.AddStream(1, nil)                                                                       // 0 -> 1
	_ = pt.AddStream(3, &streamDependencyInfo{StreamDependency: 1, Weight: 100, Exclusive: false}) // 1 -> 3
	_ = pt.AddStream(5, &streamDependencyInfo{StreamDependency: 1, Weight: 200, Exclusive: false}) // 1 -> 5
	_ = pt.AddStream(7, nil)                                                                       // 0 -> 7
	_ = pt.AddStream(9, &streamDependencyInfo{StreamDependency: 7, Weight: 50, Exclusive: true})   // 7 -> 9 (exclusive, so 7's other children become 9's)
	_ = pt.AddStream(11, &streamDependencyInfo{StreamDependency: 0, Weight: 10, Exclusive: false}) // 0 -> 11

	// Simulate tree traversal for printing
	// (Actual implementation would be more robust)
	fmt.Println("Conceptual Tree Structure:")

	var printChildren func(parentID uint32, indent string)
	printChildren = func(parentID uint32, indent string) {
		_, children, _, err := pt.GetDependencies(parentID)
		if err != nil {
			if parentID != 0 { // Stream 0 might not error if it just has no children.
				fmt.Printf("%sError getting children for %d: %v\n", indent, parentID, err)
			}
			return
		}
		for _, childID := range children {
			_, _, weight, _ := pt.GetDependencies(childID)
			fmt.Printf("%sStream %d (Weight: %d)\n", indent, childID, weight+1)
			printChildren(childID, indent+"  ")
		}
	}

	fmt.Println("Stream 0 (Root)")
	printChildren(0, "  ")

	// Output:
	// Conceptual Tree Structure:
	// Stream 0 (Root)
	//   Stream 1 (Weight: 16)
	//     Stream 3 (Weight: 101)
	//     Stream 5 (Weight: 201)
	//   Stream 7 (Weight: 16)
	//     Stream 9 (Weight: 51)
	//   Stream 11 (Weight: 11)
}

func TestPriorityTree_GetDependencies_NodeWithParentNoChildren(t *testing.T) {
	pt := NewPriorityTree()

	parentStreamID := uint32(1)
	targetStreamID := uint32(2)
	targetWeight := uint8(88)

	// Add parent stream (0 -> 1)
	// Use AddStream to ensure parent node is correctly initialized and added to tree.
	err := pt.AddStream(parentStreamID, nil)
	if err != nil {
		t.Fatalf("Failed to add parentStreamID %d: %v", parentStreamID, err)
	}

	// Add target stream (1 -> 2) with specific weight
	// This stream will be the leaf node we are testing.
	prioInfo := &streamDependencyInfo{
		StreamDependency: parentStreamID,
		Weight:           targetWeight,
		Exclusive:        false,
	}
	err = pt.AddStream(targetStreamID, prioInfo)
	if err != nil {
		t.Fatalf("Failed to add targetStreamID %d: %v", targetStreamID, err)
	}

	// Test GetDependencies for targetStreamID (2), which is a leaf node.
	pID, children, w, errGet := pt.GetDependencies(targetStreamID)
	if errGet != nil {
		t.Fatalf("GetDependencies for targetStreamID %d failed: %v", targetStreamID, errGet)
	}

	if pID != parentStreamID {
		t.Errorf("targetStreamID %d: expected parent %d, got %d", targetStreamID, parentStreamID, pID)
	}

	if w != targetWeight {
		t.Errorf("targetStreamID %d: expected weight %d, got %d", targetStreamID, targetWeight, w)
	}

	if len(children) != 0 {
		t.Errorf("targetStreamID %d: expected no children (it's a leaf node), got %v (len %d)", targetStreamID, children, len(children))
	}
}

func TestPriorityTree_UpdatePriority_WeightOnly(t *testing.T) {
	pt := NewPriorityTree()

	// Setup:
	// 0 -> 1 (parentStream)
	//      -> 2 (targetStream, initial weight 10)
	//         -> 3 (childOfTarget)
	//   -> 4 (siblingOfParent)
	parentStreamID := uint32(1)
	targetStreamID := uint32(2)
	childOfTargetID := uint32(3)
	siblingOfParentID := uint32(4)

	initialTargetWeight := uint8(10)
	newTargetWeight := uint8(77)

	// Add streams
	_ = pt.AddStream(parentStreamID, nil)                                                                                                    // 0 -> 1
	_ = pt.AddStream(targetStreamID, &streamDependencyInfo{StreamDependency: parentStreamID, Weight: initialTargetWeight, Exclusive: false}) // 1 -> 2
	_ = pt.AddStream(childOfTargetID, &streamDependencyInfo{StreamDependency: targetStreamID, Weight: 5, Exclusive: false})                  // 2 -> 3
	_ = pt.AddStream(siblingOfParentID, nil)                                                                                                 // 0 -> 4

	// Verify initial state of targetStreamID (2)
	pPre, cPre, wPre, errGetPre := pt.GetDependencies(targetStreamID)
	if errGetPre != nil || pPre != parentStreamID || wPre != initialTargetWeight || !reflect.DeepEqual(sortUint32Slice(cPre), []uint32{childOfTargetID}) {
		t.Fatalf("Pre-Update S2: P:%d W:%d C:%v. Expected P:%d W:%d C:[%d]", pPre, wPre, cPre, parentStreamID, initialTargetWeight, childOfTargetID)
	}

	// Verify initial state of parentStreamID (1)
	_, p1ChildrenPre, _, _ := pt.GetDependencies(parentStreamID)
	if !contains(p1ChildrenPre, targetStreamID) {
		t.Fatalf("Pre-Update S1: children %v, expected to contain %d", p1ChildrenPre, targetStreamID)
	}
	initialParentChildrenCount := len(p1ChildrenPre)

	// Verify initial state of childOfTargetID (3)
	p3Pre, _, w3Pre, _ := pt.GetDependencies(childOfTargetID)
	if p3Pre != targetStreamID || w3Pre != 5 {
		t.Fatalf("Pre-Update S3: P:%d W:%d. Expected P:%d W:5", p3Pre, w3Pre, targetStreamID)
	}

	// Call UpdatePriority on targetStreamID (2), changing only its weight.
	// Parent and exclusive flag remain the same as its current state implicitly.
	// Note: UpdatePriority expects the lock to be held by the caller. This test calls it directly,
	// which is okay for testing its internal logic if it worked without the lock.
	// However, since it uses getOrCreateNodeNoLock, it's better to simulate external call.
	// The public API for this through AddStream (with existing stream) or ProcessPriorityFrame.
	// Let's acquire lock for direct UpdatePriority call test.

	err := pt.UpdatePriority(targetStreamID, parentStreamID, newTargetWeight, false)

	if err != nil {
		t.Fatalf("UpdatePriority for weight change failed: %v", err)
	}

	// Verify targetStreamID (2) after update
	pPost, cPost, wPost, errGetPost := pt.GetDependencies(targetStreamID)
	if errGetPost != nil {
		t.Fatalf("Post-Update GetDependencies for targetStreamID %d failed: %v", targetStreamID, errGetPost)
	}

	if pPost != parentStreamID {
		t.Errorf("TargetStream %d: parent changed. Expected %d, got %d", targetStreamID, parentStreamID, pPost)
	}
	if wPost != newTargetWeight {
		t.Errorf("TargetStream %d: weight not updated. Expected %d, got %d", targetStreamID, newTargetWeight, wPost)
	}
	if !reflect.DeepEqual(sortUint32Slice(cPost), []uint32{childOfTargetID}) {
		t.Errorf("TargetStream %d: children changed. Expected [%d], got %v", targetStreamID, childOfTargetID, cPost)
	}

	// Verify parentStreamID (1) is unaffected (still has targetStreamID as child, same number of children)
	_, p1ChildrenPost, _, _ := pt.GetDependencies(parentStreamID)
	if !contains(p1ChildrenPost, targetStreamID) {
		t.Errorf("ParentStream %d: targetStream %d no longer a child. Children: %v", parentStreamID, targetStreamID, p1ChildrenPost)
	}
	if len(p1ChildrenPost) != initialParentChildrenCount {
		t.Errorf("ParentStream %d: number of children changed. Expected %d, got %d. Children: %v", parentStreamID, initialParentChildrenCount, len(p1ChildrenPost), p1ChildrenPost)
	}

	// Verify childOfTargetID (3) is unaffected (still child of targetStreamID)
	p3Post, _, w3Post, _ := pt.GetDependencies(childOfTargetID)
	if p3Post != targetStreamID {
		t.Errorf("ChildOfTarget %d: parent changed. Expected %d, got %d", childOfTargetID, targetStreamID, p3Post)
	}
	if w3Post != 5 { // Weight of child should be unchanged
		t.Errorf("ChildOfTarget %d: weight changed. Expected 5, got %d", childOfTargetID, w3Post)
	}

	// Verify siblingOfParentID (4) is unaffected
	p4Post, c4Post, w4Post, _ := pt.GetDependencies(siblingOfParentID)
	if p4Post != 0 || len(c4Post) != 0 || w4Post != 15 {
		t.Errorf("SiblingOfParent %d: state changed. P:%d (exp:0), C:%v (exp:[]), W:%d (exp:15)", siblingOfParentID, p4Post, c4Post, w4Post)
	}
}

func TestPriorityTree_UpdatePriority_StreamDoesNotExist(t *testing.T) {
	pt := NewPriorityTree()

	nonExistentStreamID := uint32(10)
	parentStreamID := uint32(1) // This stream will be the parent, will also be created on-the-fly
	newWeight := uint8(77)
	isExclusive := false

	// Lock is normally acquired by public methods like ProcessPriorityFrame or AddStream.
	// For direct testing of UpdatePriority's internal logic, we manage the lock.

	err := pt.UpdatePriority(nonExistentStreamID, parentStreamID, newWeight, isExclusive)

	if err != nil {
		t.Fatalf("UpdatePriority for non-existent stream %d failed: %v", nonExistentStreamID, err)
	}

	// Verify the new stream (10)
	pID, children, w, errGetNew := pt.GetDependencies(nonExistentStreamID)
	if errGetNew != nil {
		t.Fatalf("GetDependencies for newly created stream %d failed: %v", nonExistentStreamID, errGetNew)
	}

	if pID != parentStreamID {
		t.Errorf("Stream %d: expected parent %d, got %d", nonExistentStreamID, parentStreamID, pID)
	}
	if w != newWeight {
		t.Errorf("Stream %d: expected weight %d, got %d", nonExistentStreamID, newWeight, w)
	}
	if len(children) != 0 {
		t.Errorf("Stream %d: expected no children, got %v", nonExistentStreamID, children)
	}

	// Verify the parent stream (1) was created and now has stream 10 as a child
	parent_pID, parent_children, parent_w, errGetParent := pt.GetDependencies(parentStreamID)
	if errGetParent != nil {
		t.Fatalf("GetDependencies for parent stream %d failed: %v", parentStreamID, errGetParent)
	}

	if parent_pID != 0 { // Implicitly created parents depend on stream 0
		t.Errorf("Parent stream %d: expected parent 0, got %d", parentStreamID, parent_pID)
	}
	if parent_w != 15 { // Default weight for implicitly created parent
		t.Errorf("Parent stream %d: expected default weight 15, got %d", parentStreamID, parent_w)
	}
	found := false
	for _, childID := range parent_children {
		if childID == nonExistentStreamID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Stream %d not found in children of stream %d. Children: %v", nonExistentStreamID, parentStreamID, parent_children)
	}
	if len(parent_children) != 1 {
		t.Errorf("Parent stream %d: expected 1 child (%d), got %d children: %v", parentStreamID, nonExistentStreamID, len(parent_children), parent_children)
	}

	// Verify stream 0 has the parent stream (1) as a child
	_, stream0Children, _, errGetRoot := pt.GetDependencies(0)
	if errGetRoot != nil {
		t.Fatalf("GetDependencies for stream 0 failed: %v", errGetRoot)
	}
	foundRootChild := false
	for _, child := range stream0Children {
		if child == parentStreamID {
			foundRootChild = true
			break
		}
	}
	if !foundRootChild {
		t.Errorf("Parent stream %d not found in children of stream 0. Children: %v", parentStreamID, stream0Children)
	}
}

func TestPriorityTree_UpdatePriority_CycleDetection(t *testing.T) {
	pt := NewPriorityTree()

	// Setup: 0 -> 1 -> 2 -> 3
	_ = pt.AddStream(1, nil)
	_ = pt.AddStream(2, &streamDependencyInfo{StreamDependency: 1, Weight: 10})
	_ = pt.AddStream(3, &streamDependencyInfo{StreamDependency: 2, Weight: 10})

	// Attempt to make 1 depend on 3 (1 -> 3), creating a cycle 1 -> 2 -> 3 -> 1

	err := pt.UpdatePriority(1, 3, 20, false)

	if err == nil {
		t.Fatalf("UpdatePriority should have failed due to cycle detection (1 depends on 3)")
	}
	if !strings.Contains(err.Error(), "cycle detected") {
		t.Errorf("Expected cycle detection error, got: %v", err)
	}

	// Verify state hasn't changed for 1
	p1, _, w1, _ := pt.GetDependencies(1)
	if p1 != 0 || w1 != 15 { // Original parent 0, original weight 15
		t.Errorf("Stream 1 state changed after failed cycle update: P:%d (exp 0), W:%d (exp 15)", p1, w1)
	}

	// Attempt to make 2 depend on 2 (self-dependency)

	err = pt.UpdatePriority(2, 2, 20, false)
	if err == nil {
		t.Fatalf("UpdatePriority should have failed due to self-dependency (2 depends on 2)")
	}
	if !strings.Contains(err.Error(), "cannot depend on itself") {
		t.Errorf("Expected self-dependency error, got: %v", err)
	}

	// Setup for another cycle test: 0 -> 105, 105 -> 104

	// Create 105 (child of 0), then 104 (child of 105)
	// Use UpdatePriority directly for setup convenience as getOrCreateNodeNoLock is used internally.
	_ = pt.UpdatePriority(105, 0, 10, false)
	_ = pt.UpdatePriority(104, 105, 10, false)

	// Attempt to make 105 depend on 104. This would create a cycle: 105 -> 104 -> 105
	err = pt.UpdatePriority(105, 104, 10, false)

	if err == nil {
		t.Fatalf("UpdatePriority should have failed due to cycle detection (105 depends on 104)")
	}
	if !strings.Contains(err.Error(), "cycle detected") {
		t.Errorf("Expected cycle detection error for 105->104, got: %v", err)
	}

	// Verify stream 105 is still child of 0 (its state before the failing update)
	p105, _, _, _ := pt.GetDependencies(105)
	if p105 != 0 {
		t.Errorf("Stream 105 parent after failed cycle: expected 0, got %d", p105)
	}
	// Stream 104 should still be child of 105
	p104, _, _, _ := pt.GetDependencies(104)
	if p104 != 105 {
		t.Errorf("Stream 104 parent after failed cycle: expected 105, got %d", p104)
	}
}

func TestPriorityTree_UpdatePriority_ReparentExclusive_NewParentNoChildren(t *testing.T) {
	pt := NewPriorityTree()

	newParentStreamID := uint32(1)
	streamToReparentID := uint32(3)
	otherRootChildID := uint32(5) // To ensure root's children list is correctly updated

	newWeightForReparented := uint8(77)

	// Setup:
	// 0 -> newParentStreamID (1)
	// 0 -> streamToReparentID (3)
	// 0 -> otherRootChildID (5)
	_ = pt.AddStream(newParentStreamID, nil)  // Will become parent
	_ = pt.AddStream(streamToReparentID, nil) // Will be re-parented
	_ = pt.AddStream(otherRootChildID, nil)   // Just another child of root

	// Verify initial state for newParentStreamID (1)
	p1Pre, c1Pre, w1Pre, _ := pt.GetDependencies(newParentStreamID)
	if p1Pre != 0 || len(c1Pre) != 0 || w1Pre != 15 {
		t.Fatalf("Pre-Update S1: P:%d C:%v W:%d. Expected P:0 C:[] W:15", p1Pre, c1Pre, w1Pre)
	}

	// Verify initial state for streamToReparentID (3)
	p3Pre, c3Pre, w3Pre, _ := pt.GetDependencies(streamToReparentID)
	if p3Pre != 0 || len(c3Pre) != 0 || w3Pre != 15 {
		t.Fatalf("Pre-Update S3: P:%d C:%v W:%d. Expected P:0 C:[] W:15", p3Pre, c3Pre, w3Pre)
	}

	// Verify initial state for stream 0's children
	_, rootChildrenPre, _, _ := pt.GetDependencies(0)
	expectedRootChildrenPre := []uint32{newParentStreamID, streamToReparentID, otherRootChildID}
	if !reflect.DeepEqual(sortUint32Slice(rootChildrenPre), sortUint32Slice(expectedRootChildrenPre)) {
		t.Fatalf("Pre-Update Root Children: %v. Expected %v", rootChildrenPre, expectedRootChildrenPre)
	}

	// Operation: Re-parent streamToReparentID (3) to be an exclusive child of newParentStreamID (1)

	err := pt.UpdatePriority(streamToReparentID, newParentStreamID, newWeightForReparented, true)

	if err != nil {
		t.Fatalf("UpdatePriority failed: %v", err)
	}

	// --- Verifications ---

	// Verify newParentStreamID (1)
	p1Post, c1Post, w1Post, _ := pt.GetDependencies(newParentStreamID)
	if p1Post != 0 {
		t.Errorf("newParentStream (1) post-update: parent expected 0, got %d", p1Post)
	}
	if w1Post != 15 { // Weight of parent itself should be unchanged
		t.Errorf("newParentStream (1) post-update: weight expected 15, got %d", w1Post)
	}
	expectedC1Post := []uint32{streamToReparentID}
	if !reflect.DeepEqual(sortUint32Slice(c1Post), sortUint32Slice(expectedC1Post)) {
		t.Errorf("newParentStream (1) post-update: children expected %v, got %v", expectedC1Post, c1Post)
	}

	// Verify streamToReparentID (3)
	p3Post, c3Post, w3Post, _ := pt.GetDependencies(streamToReparentID)
	if p3Post != newParentStreamID {
		t.Errorf("streamToReparent (3) post-update: parent expected %d, got %d", newParentStreamID, p3Post)
	}
	if w3Post != newWeightForReparented {
		t.Errorf("streamToReparent (3) post-update: weight expected %d, got %d", newWeightForReparented, w3Post)
	}
	if len(c3Post) != 0 { // Had no children, adopted no children
		t.Errorf("streamToReparent (3) post-update: children expected [], got %v", c3Post)
	}

	// Verify otherRootChildID (5) is unaffected
	p5Post, c5Post, w5Post, _ := pt.GetDependencies(otherRootChildID)
	if p5Post != 0 || len(c5Post) != 0 || w5Post != 15 {
		t.Errorf("otherRootChild (5) post-update: state changed. P:%d (exp 0), C:%v (exp:[]), W:%d (exp:15)", p5Post, c5Post, w5Post)
	}

	// Verify stream 0's children
	_, rootChildrenPost, _, _ := pt.GetDependencies(0)
	// streamToReparentID (3) is no longer a direct child of root
	expectedRootChildrenPost := []uint32{newParentStreamID, otherRootChildID}
	if !reflect.DeepEqual(sortUint32Slice(rootChildrenPost), sortUint32Slice(expectedRootChildrenPost)) {
		t.Errorf("Post-Update Root Children: expected %v, got %v", expectedRootChildrenPost, rootChildrenPost)
	}
}

func TestPriorityTree_UpdatePriority_ReparentExclusive_NewParentHasChildren(t *testing.T) {
	pt := NewPriorityTree()

	newParentStreamID := uint32(1)
	childOfNewParent1ID := uint32(2)
	childOfNewParent2ID := uint32(3)

	streamToReparentID := uint32(4)
	childOfReparentedID := uint32(5)
	originalParentOfReparentedID := uint32(7) // An arbitrary original parent for streamToReparentID

	otherRootChildID := uint32(6) // Control stream

	weightNewParent := uint8(15) // Default for stream 1
	weightChildOfNewParent1 := uint8(10)
	weightChildOfNewParent2 := uint8(20)
	weightStreamToReparentInitial := uint8(30)
	weightChildOfReparented := uint8(40)
	newWeightForReparented := uint8(50)

	// Setup:
	// 0 -> newParentStreamID (1)
	//      -> childOfNewParent1ID (2, w:10)
	//      -> childOfNewParent2ID (3, w:20)
	// 0 -> originalParentOfReparentedID (7)
	//      -> streamToReparentID (4, w:30)
	//           -> childOfReparentedID (5, w:40)
	// 0 -> otherRootChildID (6)

	_ = pt.AddStream(newParentStreamID, nil) // Will become the new parent for stream 4
	_ = pt.AddStream(childOfNewParent1ID, &streamDependencyInfo{StreamDependency: newParentStreamID, Weight: weightChildOfNewParent1})
	_ = pt.AddStream(childOfNewParent2ID, &streamDependencyInfo{StreamDependency: newParentStreamID, Weight: weightChildOfNewParent2})

	_ = pt.AddStream(originalParentOfReparentedID, nil) // Original parent for stream 4
	_ = pt.AddStream(streamToReparentID, &streamDependencyInfo{StreamDependency: originalParentOfReparentedID, Weight: weightStreamToReparentInitial})
	_ = pt.AddStream(childOfReparentedID, &streamDependencyInfo{StreamDependency: streamToReparentID, Weight: weightChildOfReparented})

	_ = pt.AddStream(otherRootChildID, nil) // Control stream

	// Verify initial state for newParentStreamID (1)
	p1Pre, c1Pre, w1Pre, _ := pt.GetDependencies(newParentStreamID)
	if p1Pre != 0 || !reflect.DeepEqual(sortUint32Slice(c1Pre), []uint32{childOfNewParent1ID, childOfNewParent2ID}) || w1Pre != weightNewParent {
		t.Fatalf("Pre-Update S1: P:%d C:%v W:%d. Expected P:0 C:[%d %d] W:%d", p1Pre, c1Pre, w1Pre, childOfNewParent1ID, childOfNewParent2ID, weightNewParent)
	}

	// Verify initial state for streamToReparentID (4)
	p4Pre, c4Pre, w4Pre, _ := pt.GetDependencies(streamToReparentID)
	if p4Pre != originalParentOfReparentedID || !reflect.DeepEqual(sortUint32Slice(c4Pre), []uint32{childOfReparentedID}) || w4Pre != weightStreamToReparentInitial {
		t.Fatalf("Pre-Update S4: P:%d C:%v W:%d. Expected P:%d C:[%d] W:%d", p4Pre, c4Pre, w4Pre, originalParentOfReparentedID, childOfReparentedID, weightStreamToReparentInitial)
	}

	// Verify initial state for originalParentOfReparentedID (7)
	p7Pre, c7Pre, _, _ := pt.GetDependencies(originalParentOfReparentedID)
	if p7Pre != 0 || !contains(c7Pre, streamToReparentID) {
		t.Fatalf("Pre-Update S7: P:%d C:%v. Expected P:0 C to contain %d", p7Pre, c7Pre, streamToReparentID)
	}

	// Operation: Re-parent streamToReparentID (4) to be an exclusive child of newParentStreamID (1)

	err := pt.UpdatePriority(streamToReparentID, newParentStreamID, newWeightForReparented, true)

	if err != nil {
		t.Fatalf("UpdatePriority failed: %v", err)
	}

	// --- Verifications ---

	// Verify newParentStreamID (1)
	p1Post, c1Post, w1Post, _ := pt.GetDependencies(newParentStreamID)
	if p1Post != 0 {
		t.Errorf("newParentStream (1) post-update: parent expected 0, got %d", p1Post)
	}
	if w1Post != weightNewParent { // Weight of parent itself should be unchanged
		t.Errorf("newParentStream (1) post-update: weight expected %d, got %d", weightNewParent, w1Post)
	}
	expectedC1Post := []uint32{streamToReparentID} // Stream 4 is now sole child
	if !reflect.DeepEqual(sortUint32Slice(c1Post), sortUint32Slice(expectedC1Post)) {
		t.Errorf("newParentStream (1) post-update: children expected %v, got %v", expectedC1Post, c1Post)
	}

	// Verify streamToReparentID (4)
	p4Post, c4Post, w4Post, _ := pt.GetDependencies(streamToReparentID)
	if p4Post != newParentStreamID {
		t.Errorf("streamToReparent (4) post-update: parent expected %d, got %d", newParentStreamID, p4Post)
	}
	if w4Post != newWeightForReparented {
		t.Errorf("streamToReparent (4) post-update: weight expected %d, got %d", newWeightForReparented, w4Post)
	}
	// Children of 4 should be its original child (5) AND the adopted children from 1 (2, 3)
	expectedC4Post := []uint32{childOfReparentedID, childOfNewParent1ID, childOfNewParent2ID}
	if !reflect.DeepEqual(sortUint32Slice(c4Post), sortUint32Slice(expectedC4Post)) {
		t.Errorf("streamToReparent (4) post-update: children expected sorted %v, got sorted %v (original: %v)", sortUint32Slice(expectedC4Post), sortUint32Slice(c4Post), c4Post)
	}

	// Verify childOfNewParent1ID (2) - now child of stream 4
	p2Post, _, w2Post, _ := pt.GetDependencies(childOfNewParent1ID)
	if p2Post != streamToReparentID {
		t.Errorf("childOfNewParent1 (2) post-update: parent expected %d, got %d", streamToReparentID, p2Post)
	}
	if w2Post != weightChildOfNewParent1 { // Weight preserved
		t.Errorf("childOfNewParent1 (2) post-update: weight expected %d, got %d", weightChildOfNewParent1, w2Post)
	}

	// Verify childOfNewParent2ID (3) - now child of stream 4
	p3Post, _, w3Post, _ := pt.GetDependencies(childOfNewParent2ID)
	if p3Post != streamToReparentID {
		t.Errorf("childOfNewParent2 (3) post-update: parent expected %d, got %d", streamToReparentID, p3Post)
	}
	if w3Post != weightChildOfNewParent2 { // Weight preserved
		t.Errorf("childOfNewParent2 (3) post-update: weight expected %d, got %d", weightChildOfNewParent2, w3Post)
	}

	// Verify childOfReparentedID (5) - still child of stream 4
	p5Post, _, w5Post, _ := pt.GetDependencies(childOfReparentedID)
	if p5Post != streamToReparentID {
		t.Errorf("childOfReparented (5) post-update: parent expected %d, got %d", streamToReparentID, p5Post)
	}
	if w5Post != weightChildOfReparented { // Weight preserved
		t.Errorf("childOfReparented (5) post-update: weight expected %d, got %d", weightChildOfReparented, w5Post)
	}

	// Verify originalParentOfReparentedID (7) no longer has streamToReparentID (4) as a child (assuming it was its only child initially for simplicity of this check part)
	_, c7Post, _, _ := pt.GetDependencies(originalParentOfReparentedID)
	if contains(c7Post, streamToReparentID) {
		t.Errorf("originalParent (7) post-update: still has stream %d as child. Children: %v", streamToReparentID, c7Post)
	}
	if len(c7Post) != 0 { // If stream 4 was its only child
		t.Logf("Note: originalParent (7) has children %v after stream 4 was moved. This is okay if it had other children.", c7Post)
	}

	// Verify otherRootChildID (6) is unaffected
	p6Post, c6Post, w6Post, _ := pt.GetDependencies(otherRootChildID)
	if p6Post != 0 || len(c6Post) != 0 || w6Post != 15 { // Assuming default weight 15
		t.Errorf("otherRootChild (6) post-update: state changed. P:%d (exp 0), C:%v (exp:[]), W:%d (exp:15)", p6Post, c6Post, w6Post)
	}
}

func TestPriorityTree_UpdatePriority_ReparentNonExclusive(t *testing.T) {
	pt := NewPriorityTree()

	oldParentID := uint32(1)
	streamToReparentID := uint32(2)
	newParentID := uint32(3)
	siblingInOldParentID := uint32(4) // Another child of oldParentID
	childOfNewParentID := uint32(5)   // Existing child of newParentID

	initialWeight := uint8(10)
	newWeight := uint8(99)

	// Setup:
	// 0 -> oldParentID (1)
	//      -> streamToReparentID (2, w:10)
	//      -> siblingInOldParentID (4, w:15 def)
	// 0 -> newParentID (3)
	//      -> childOfNewParentID (5, w:15 def)

	_ = pt.AddStream(oldParentID, nil)
	_ = pt.AddStream(streamToReparentID, &streamDependencyInfo{StreamDependency: oldParentID, Weight: initialWeight})
	_ = pt.AddStream(siblingInOldParentID, &streamDependencyInfo{StreamDependency: oldParentID, Weight: 15}) // Default weight for sibling

	_ = pt.AddStream(newParentID, nil)
	_ = pt.AddStream(childOfNewParentID, &streamDependencyInfo{StreamDependency: newParentID, Weight: 15}) // Default weight for child of new parent

	// Verify initial state for streamToReparentID (2)
	p2Pre, c2Pre, w2Pre, _ := pt.GetDependencies(streamToReparentID)
	if p2Pre != oldParentID || len(c2Pre) != 0 || w2Pre != initialWeight {
		t.Fatalf("Pre-Update S2: P:%d C:%v W:%d. Expected P:%d C:[] W:%d", p2Pre, c2Pre, w2Pre, oldParentID, initialWeight)
	}

	// Verify initial state for oldParentID (1)
	_, oldParentChildrenPre, _, _ := pt.GetDependencies(oldParentID)
	expectedOldParentChildrenPre := []uint32{streamToReparentID, siblingInOldParentID}
	if !reflect.DeepEqual(sortUint32Slice(oldParentChildrenPre), sortUint32Slice(expectedOldParentChildrenPre)) {
		t.Fatalf("Pre-Update oldParentID (1) children: %v. Expected %v", oldParentChildrenPre, expectedOldParentChildrenPre)
	}

	// Verify initial state for newParentID (3)
	_, newParentChildrenPre, _, _ := pt.GetDependencies(newParentID)
	expectedNewParentChildrenPre := []uint32{childOfNewParentID}
	if !reflect.DeepEqual(sortUint32Slice(newParentChildrenPre), sortUint32Slice(expectedNewParentChildrenPre)) {
		t.Fatalf("Pre-Update newParentID (3) children: %v. Expected %v", newParentChildrenPre, expectedNewParentChildrenPre)
	}

	// Operation: Re-parent streamToReparentID (2) to be a non-exclusive child of newParentID (3)
	// and update its weight.

	err := pt.UpdatePriority(streamToReparentID, newParentID, newWeight, false)

	if err != nil {
		t.Fatalf("UpdatePriority failed: %v", err)
	}

	// --- Verifications ---

	// Verify streamToReparentID (2)
	p2Post, c2Post, w2Post, _ := pt.GetDependencies(streamToReparentID)
	if p2Post != newParentID {
		t.Errorf("streamToReparent (2) post-update: parent expected %d, got %d", newParentID, p2Post)
	}
	if w2Post != newWeight {
		t.Errorf("streamToReparent (2) post-update: weight expected %d, got %d", newWeight, w2Post)
	}
	if len(c2Post) != 0 { // Had no children, should still have no children
		t.Errorf("streamToReparent (2) post-update: children expected [], got %v", c2Post)
	}

	// Verify oldParentID (1)
	_, oldParentChildrenPost, _, _ := pt.GetDependencies(oldParentID)
	expectedOldParentChildrenPost := []uint32{siblingInOldParentID} // streamToReparentID (2) should be removed
	if !reflect.DeepEqual(sortUint32Slice(oldParentChildrenPost), sortUint32Slice(expectedOldParentChildrenPost)) {
		t.Errorf("oldParentID (1) post-update: children expected %v, got %v", expectedOldParentChildrenPost, oldParentChildrenPost)
	}

	// Verify newParentID (3)
	_, newParentChildrenPost, _, _ := pt.GetDependencies(newParentID)
	expectedNewParentChildrenPostAfter := []uint32{childOfNewParentID, streamToReparentID} // streamToReparentID (2) should be added
	if !reflect.DeepEqual(sortUint32Slice(newParentChildrenPost), sortUint32Slice(expectedNewParentChildrenPostAfter)) {
		t.Errorf("newParentID (3) post-update: children expected %v, got %v", expectedNewParentChildrenPostAfter, newParentChildrenPost)
	}

	// Verify siblingInOldParentID (4) is unaffected (still child of oldParentID, same weight)
	p4Post, _, w4Post, _ := pt.GetDependencies(siblingInOldParentID)
	if p4Post != oldParentID {
		t.Errorf("siblingInOldParent (4) post-update: parent expected %d, got %d", oldParentID, p4Post)
	}
	if w4Post != 15 { // Unchanged weight
		t.Errorf("siblingInOldParent (4) post-update: weight expected 15, got %d", w4Post)
	}

	// Verify childOfNewParentID (5) is unaffected (still child of newParentID, same weight)
	p5Post, _, w5Post, _ := pt.GetDependencies(childOfNewParentID)
	if p5Post != newParentID {
		t.Errorf("childOfNewParent (5) post-update: parent expected %d, got %d", newParentID, p5Post)
	}
	if w5Post != 15 { // Unchanged weight
		t.Errorf("childOfNewParent (5) post-update: weight expected 15, got %d", w5Post)
	}
}

func TestPriorityTree_UpdatePriority_SelfDependencyError(t *testing.T) {
	pt := NewPriorityTree()

	// Setup:
	// 0 -> 1 (originalParentID)
	//      -> 2 (streamID, weight 50)
	//      -> 3 (siblingID, weight 60) // Sibling to ensure originalParentID's children list is checked properly
	originalParentID := uint32(1)
	streamID := uint32(2)
	siblingID := uint32(3)

	originalWeightStreamID := uint8(50)
	originalWeightSiblingID := uint8(60)

	// Use AddStream for a clean setup respecting locks
	if err := pt.AddStream(originalParentID, nil); err != nil { // 0 -> 1
		t.Fatalf("Setup AddStream(%d) failed: %v", originalParentID, err)
	}
	if err := pt.AddStream(streamID, &streamDependencyInfo{StreamDependency: originalParentID, Weight: originalWeightStreamID}); err != nil { // 1 -> 2
		t.Fatalf("Setup AddStream(%d) failed: %v", streamID, err)
	}
	if err := pt.AddStream(siblingID, &streamDependencyInfo{StreamDependency: originalParentID, Weight: originalWeightSiblingID}); err != nil { // 1 -> 3
		t.Fatalf("Setup AddStream(%d) failed: %v", siblingID, err)
	}

	// Attempt to make streamID depend on itself. Weight in call is different to check it's not applied.
	attemptedNewWeight := uint8(100)

	err := pt.UpdatePriority(streamID, streamID, attemptedNewWeight, false)

	if err == nil {
		t.Fatalf("UpdatePriority for stream %d should have failed due to self-dependency, but did not", streamID)
	}
	// UpdatePriority now returns a sentinel error, not a formatted one with streamID.
	if !errors.Is(err, errSelfDependency) { // Check for the specific sentinel error
		t.Errorf("Expected error '%v', got: '%v'", errSelfDependency, err)
	}

	// Verify streamID's properties (parent, weight) are unchanged
	p, c, w, getErr := pt.GetDependencies(streamID)
	if getErr != nil {
		t.Fatalf("GetDependencies for stream %d failed after self-dependency attempt: %v", streamID, getErr)
	}
	if p != originalParentID {
		t.Errorf("Stream %d: parent changed. Expected %d, got %d", streamID, originalParentID, p)
	}
	if w != originalWeightStreamID {
		t.Errorf("Stream %d: weight changed. Expected %d, got %d", streamID, originalWeightStreamID, w)
	}
	if len(c) != 0 { // streamID had no children
		t.Errorf("Stream %d: children changed. Expected [], got %v", streamID, c)
	}

	// Verify originalParentID's children are unchanged
	_, opChildren, _, getErrOP := pt.GetDependencies(originalParentID)
	if getErrOP != nil {
		t.Fatalf("GetDependencies for original parent %d failed: %v", originalParentID, getErrOP)
	}
	expectedOPChildren := []uint32{streamID, siblingID}
	if !reflect.DeepEqual(sortUint32Slice(opChildren), sortUint32Slice(expectedOPChildren)) {
		t.Errorf("Original parent %d: children changed. Expected sorted %v, got sorted %v (original: %v)",
			originalParentID, sortUint32Slice(expectedOPChildren), sortUint32Slice(opChildren), opChildren)
	}

	// Verify siblingID is unaffected
	ps, cs, ws, getErrS := pt.GetDependencies(siblingID)
	if getErrS != nil {
		t.Fatalf("GetDependencies for sibling %d failed: %v", siblingID, getErrS)
	}
	if ps != originalParentID {
		t.Errorf("Sibling %d: parent changed. Expected %d, got %d", siblingID, originalParentID, ps)
	}
	if ws != originalWeightSiblingID {
		t.Errorf("Sibling %d: weight changed. Expected %d, got %d", siblingID, originalWeightSiblingID, ws)
	}
	if len(cs) != 0 { // siblingID had no children
		t.Errorf("Sibling %d: children changed. Expected [], got %v", siblingID, cs)
	}
}

func TestPriorityTree_ExclusiveChildOfStream0(t *testing.T) {
	pt := NewPriorityTree()

	// Initial streams, all direct children of stream 0
	streamA := uint32(1)
	streamB := uint32(3)
	streamC := uint32(5)
	streamToBeExclusive := streamA // Stream 1 will become the exclusive child of stream 0

	weightA := uint8(10)
	weightB := uint8(20)
	weightC := uint8(30)
	newWeightForExclusive := uint8(100) // New weight for streamA when it becomes exclusive

	// Add initial streams
	// These calls will use UpdatePriority internally, which correctly handles locking.
	if err := pt.AddStream(streamA, &streamDependencyInfo{StreamDependency: 0, Weight: weightA, Exclusive: false}); err != nil {
		t.Fatalf("Setup AddStream(%d) failed: %v", streamA, err)
	}
	if err := pt.AddStream(streamB, &streamDependencyInfo{StreamDependency: 0, Weight: weightB, Exclusive: false}); err != nil {
		t.Fatalf("Setup AddStream(%d) failed: %v", streamB, err)
	}
	if err := pt.AddStream(streamC, &streamDependencyInfo{StreamDependency: 0, Weight: weightC, Exclusive: false}); err != nil {
		t.Fatalf("Setup AddStream(%d) failed: %v", streamC, err)
	}

	// Verify initial state of stream 0
	_, rootChildrenPre, _, _ := pt.GetDependencies(0)
	expectedRootChildrenPre := []uint32{streamA, streamB, streamC}
	if !reflect.DeepEqual(sortUint32Slice(rootChildrenPre), sortUint32Slice(expectedRootChildrenPre)) {
		t.Fatalf("Pre-Update: Stream 0 children expected %v, got %v", sortUint32Slice(expectedRootChildrenPre), sortUint32Slice(rootChildrenPre))
	}

	// Make streamA an exclusive child of stream 0
	// UpdatePriority is called by AddStream or ProcessPriorityFrame which handle locking.
	// Here, we call UpdatePriority directly for testing its effect.
	// The public UpdatePriority method itself now handles locking.
	err := pt.UpdatePriority(streamToBeExclusive, 0, newWeightForExclusive, true)

	if err != nil {
		t.Fatalf("UpdatePriority to make stream %d exclusive child of stream 0 failed: %v", streamToBeExclusive, err)
	}

	// Verifications:

	// 1. streamToBeExclusive (streamA)
	//    - Parent should be 0.
	//    - Weight should be newWeightForExclusive.
	//    - Children should be streamB and streamC.
	sA_parent, sA_children, sA_weight, errGetA := pt.GetDependencies(streamToBeExclusive)
	if errGetA != nil {
		t.Fatalf("GetDependencies for stream %d (exclusive child) failed: %v", streamToBeExclusive, errGetA)
	}
	if sA_parent != 0 {
		t.Errorf("Stream %d: expected parent 0, got %d", streamToBeExclusive, sA_parent)
	}
	if sA_weight != newWeightForExclusive {
		t.Errorf("Stream %d: expected weight %d, got %d", streamToBeExclusive, newWeightForExclusive, sA_weight)
	}
	expected_sA_children := []uint32{streamB, streamC}
	if !reflect.DeepEqual(sortUint32Slice(sA_children), sortUint32Slice(expected_sA_children)) {
		t.Errorf("Stream %d: expected children %v, got %v", streamToBeExclusive, sortUint32Slice(expected_sA_children), sortUint32Slice(sA_children))
	}

	// 2. Stream 0 (root)
	//    - Parent is 0.
	//    - Children list should only contain streamToBeExclusive (streamA).
	//    - Weight is 0.
	s0_parent, s0_children, s0_weight, errGet0 := pt.GetDependencies(0)
	if errGet0 != nil {
		t.Fatalf("GetDependencies for stream 0 failed: %v", errGet0)
	}
	if s0_parent != 0 {
		t.Errorf("Stream 0: expected parent 0, got %d", s0_parent)
	}
	if s0_weight != 0 {
		t.Errorf("Stream 0: expected weight 0, got %d", s0_weight)
	}
	expected_s0_children := []uint32{streamToBeExclusive}
	if !reflect.DeepEqual(sortUint32Slice(s0_children), sortUint32Slice(expected_s0_children)) {
		t.Errorf("Stream 0: expected children %v, got %v", sortUint32Slice(expected_s0_children), sortUint32Slice(s0_children))
	}

	// 3. streamB
	//    - Parent should now be streamToBeExclusive (streamA).
	//    - Weight should be preserved (weightB).
	sB_parent, _, sB_weight, errGetB := pt.GetDependencies(streamB)
	if errGetB != nil {
		t.Fatalf("GetDependencies for stream %d failed: %v", streamB, errGetB)
	}
	if sB_parent != streamToBeExclusive {
		t.Errorf("Stream %d: expected parent %d, got %d", streamB, streamToBeExclusive, sB_parent)
	}
	if sB_weight != weightB {
		t.Errorf("Stream %d: expected weight %d (preserved), got %d", streamB, weightB, sB_weight)
	}

	// 4. streamC
	//    - Parent should now be streamToBeExclusive (streamA).
	//    - Weight should be preserved (weightC).
	sC_parent, _, sC_weight, errGetC := pt.GetDependencies(streamC)
	if errGetC != nil {
		t.Fatalf("GetDependencies for stream %d failed: %v", streamC, errGetC)
	}
	if sC_parent != streamToBeExclusive {
		t.Errorf("Stream %d: expected parent %d, got %d", streamC, streamToBeExclusive, sC_parent)
	}
	if sC_weight != weightC {
		t.Errorf("Stream %d: expected weight %d (preserved), got %d", streamC, weightC, sC_weight)
	}
}

func TestPriorityTree_UpdatePriority_Stream0_Behavior(t *testing.T) {
	pt := NewPriorityTree()
	// Add a stream that stream 0 might try to depend on
	if err := pt.AddStream(1, nil); err != nil {
		t.Fatalf("Setup: AddStream(1) failed: %v", err)
	}

	// Attempt to make stream 0 depend on stream 1. This is invalid.
	// A robust UpdatePriority should prevent this and/or return an error.
	err := pt.UpdatePriority(0, 1, 10, false)

	// Check if an error was returned (ideal behavior for UpdatePriority)
	if err != nil {
		// This is good if UpdatePriority becomes robust enough to detect this.
		t.Logf("UpdatePriority(0, 1, ...) returned error: %v", err)
		// If an error IS returned, it should ideally be a specific type.
		// e.g., if errors.Is(err, ErrCodeProtocolError) or a new specific error.
	}

	// Verify stream 0's integrity: parent must remain 0, weight must remain 0.
	s0Parent, s0Children, s0Weight, getErr0 := pt.GetDependencies(0)
	if getErr0 != nil {
		t.Fatalf("GetDependencies(0) failed: %v", getErr0)
	}
	if s0Parent != 0 {
		t.Errorf("Stream 0 parent illegally changed to %d, expected 0", s0Parent)
	}
	if s0Weight != 0 { // Stream 0's weight is defined as 0 in NewPriorityTree
		t.Errorf("Stream 0 weight changed to %d, expected 0", s0Weight)
	}

	// Verify stream 1's integrity: it should not have stream 0 as a child.
	s1Parent, s1Children, _, getErr1 := pt.GetDependencies(1)
	if getErr1 != nil {
		t.Fatalf("GetDependencies(1) failed: %v", getErr1)
	}
	if s1Parent != 0 { // Stream 1 was initially child of 0.
		t.Errorf("Stream 1 parent expected 0, got %d", s1Parent)
	}
	for _, childID := range s1Children {
		if childID == 0 {
			t.Errorf("Stream 1 illegally has stream 0 as a child. s1Children: %v", s1Children)
			break
		}
	}

	// Stream 0 should not have itself as a child
	for _, childID := range s0Children {
		if childID == 0 {
			t.Errorf("Stream 0 illegally has itself as a child. s0Children: %v", s0Children)
		}
	}

	// Attempt to make stream 0 depend on itself (which UpdatePriority should already handle via errSelfDependency)
	errSelf := pt.UpdatePriority(0, 0, 10, false)
	if !errors.Is(errSelf, errSelfDependency) {
		t.Errorf("UpdatePriority(0,0,...) expected errSelfDependency, got %v", errSelf)
	}
	// Verify stream 0's parent is still 0 after self-dependency attempt
	s0ParentAfterSelf, _, _, _ := pt.GetDependencies(0)
	if s0ParentAfterSelf != 0 {
		t.Errorf("Stream 0 parent changed to %d after self-dependency attempt, expected 0", s0ParentAfterSelf)
	}
}

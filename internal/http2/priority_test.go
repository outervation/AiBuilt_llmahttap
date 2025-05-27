package http2

import (
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

func TestPriorityTree_ProcessPriorityFrame_Valid(t *testing.T) {
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
	// Stream 1: parent 0
	// Stream 2: parent 0
	// Stream 3: parent 1 (child of stream 1)
	// Stream 4: parent 1 (child of stream 1)
	_ = pt.AddStream(1, nil)
	_ = pt.AddStream(2, nil)
	_ = pt.AddStream(3, &streamDependencyInfo{StreamDependency: 1, Weight: 10})
	_ = pt.AddStream(4, &streamDependencyInfo{StreamDependency: 1, Weight: 20})

	// Stream 5 will be processed by PRIORITY frame
	_ = pt.getOrCreateNodeNoLock(5) // ensure stream 5 exists

	// PRIORITY frame for stream 5: make it exclusive child of stream 1.
	// Streams 3 and 4 (old children of 1) should become children of 5.
	frame := &PriorityFrame{
		FrameHeader:      FrameHeader{StreamID: 5},
		Exclusive:        true,
		StreamDependency: 1,  // New parent for stream 5
		Weight:           50, // New weight for stream 5
	}

	err := pt.ProcessPriorityFrame(frame)
	if err != nil {
		t.Fatalf("ProcessPriorityFrame failed: %v", err)
	}

	// Verify stream 5
	s5Parent, s5Children, s5Weight, errGet := pt.GetDependencies(5)
	if errGet != nil {
		t.Fatalf("GetDependencies for stream 5 failed: %v", errGet)
	}
	if s5Parent != 1 {
		t.Errorf("Stream 5: expected parent 1, got %d", s5Parent)
	}
	if s5Weight != 50 {
		t.Errorf("Stream 5: expected weight 50, got %d", s5Weight)
	}
	expectedS5Children := []uint32{3, 4} // Order might not be guaranteed by append, sort for comparison
	if !reflect.DeepEqual(sortUint32Slice(s5Children), sortUint32Slice(expectedS5Children)) {
		t.Errorf("Stream 5: expected children %v, got %v", expectedS5Children, s5Children)
	}

	// Verify stream 1 (new parent of 5)
	s1Parent, s1Children, _, _ := pt.GetDependencies(1)
	if s1Parent != 0 {
		t.Errorf("Stream 1: expected parent 0, got %d", s1Parent)
	}
	expectedS1Children := []uint32{5}
	if !reflect.DeepEqual(sortUint32Slice(s1Children), sortUint32Slice(expectedS1Children)) {
		t.Errorf("Stream 1: expected children %v (only 5), got %v", expectedS1Children, s1Children)
	}

	// Verify stream 3 (now child of 5)
	s3Parent, _, s3Weight, _ := pt.GetDependencies(3)
	if s3Parent != 5 {
		t.Errorf("Stream 3: expected parent 5, got %d", s3Parent)
	}
	if s3Weight != 10 { // Weight should be preserved
		t.Errorf("Stream 3: expected weight 10, got %d", s3Weight)
	}

	// Verify stream 4 (now child of 5)
	s4Parent, _, s4Weight, _ := pt.GetDependencies(4)
	if s4Parent != 5 {
		t.Errorf("Stream 4: expected parent 5, got %d", s4Parent)
	}
	if s4Weight != 20 { // Weight should be preserved
		t.Errorf("Stream 4: expected weight 20, got %d", s4Weight)
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
		t.Errorf("Expected error for stream %d, got for stream %d", streamID, streamErr.StreamID)
	}
	// t.Logf("Got expected error: %v", err)
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

// TestComplexScenario combines multiple operations to check tree integrity.
func TestPriorityTree_ComplexScenario(t *testing.T) {
	pt := NewPriorityTree()

	// 1. Add streams
	// 0 -> 1 (w:15)
	//   -> 2 (w:15)
	_ = pt.AddStream(1, nil)
	_ = pt.AddStream(2, nil)

	// 2. Update 1's priority (make it child of 2, w:30)
	// 0 -> 2 (w:15)
	//      -> 1 (w:30)
	err := pt.UpdatePriority(1, 2, 30, false)
	if err != nil {
		t.Fatalf("UpdatePriority(1) failed: %v", err)
	}
	p, c, w, _ := pt.GetDependencies(1)
	if p != 2 || w != 30 || len(c) != 0 {
		t.Errorf("S1 post update: P:%d W:%d C:%v. Expected P:2 W:30 C:[]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(2)
	if p != 0 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{1}) {
		t.Errorf("S2 post S1-update: P:%d W:%d C:%v. Expected P:0 W:15 C:[1]", p, w, c)
	}

	// 3. Add stream 3 as exclusive child of 2 (w:50)
	// Existing child of 2 (stream 1) should become child of 3.
	// 0 -> 2 (w:15)
	//      -> 3 (w:50)
	//         -> 1 (w:30, parent changed from 2 to 3)
	_ = pt.AddStream(3, &streamDependencyInfo{StreamDependency: 2, Weight: 50, Exclusive: true})
	p, c, w, _ = pt.GetDependencies(3)
	if p != 2 || w != 50 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{1}) {
		t.Errorf("S3 post add: P:%d W:%d C:%v. Expected P:2 W:50 C:[1]", p, w, c)
	}
	p, _, _, _ = pt.GetDependencies(1)
	if p != 3 {
		t.Errorf("S1 post S3-add: P:%d. Expected P:3", p)
	}
	p, c, w, _ = pt.GetDependencies(2)
	if p != 0 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{3}) {
		t.Errorf("S2 post S3-add: P:%d W:%d C:%v. Expected P:0 W:15 C:[3]", p, w, c)
	}

	// 4. Add stream 4 as child of 2 (w:25, non-exclusive)
	// 0 -> 2 (w:15)
	//      -> 3 (w:50)
	//         -> 1 (w:30)
	//      -> 4 (w:25)
	_ = pt.AddStream(4, &streamDependencyInfo{StreamDependency: 2, Weight: 25, Exclusive: false})
	p, c, w, _ = pt.GetDependencies(4)
	if p != 2 || w != 25 || len(c) != 0 {
		t.Errorf("S4 post add: P:%d W:%d C:%v. Expected P:2 W:25 C:[]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(2)
	if p != 0 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{3, 4}) {
		t.Errorf("S2 post S4-add: P:%d W:%d C:%v. Expected P:0 W:15 C:[3 4]", p, w, c)
	}

	// 5. Remove stream 3
	// Children of 3 (stream 1) should be re-parented to 2 (parent of 3).
	// 0 -> 2 (w:15)
	//      -> 1 (w:30, parent changed from 3 to 2)
	//      -> 4 (w:25)
	err = pt.RemoveStream(3)
	if err != nil {
		t.Fatalf("RemoveStream(3) failed: %v", err)
	}
	_, _, _, errGet3 := pt.GetDependencies(3)
	if errGet3 == nil {
		t.Errorf("S3 should be removed")
	}
	p, _, _, _ = pt.GetDependencies(1)
	if p != 2 {
		t.Errorf("S1 post S3-remove: P:%d. Expected P:2", p)
	}
	p, c, w, _ = pt.GetDependencies(2)
	if p != 0 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{1, 4}) {
		t.Errorf("S2 post S3-remove: P:%d W:%d C:%v. Expected P:0 W:15 C:[1 4]", p, w, c)
	}

	// 6. Process PRIORITY frame for stream 4: make it child of 1 (w:60, non-exclusive)
	// 0 -> 2 (w:15)
	//      -> 1 (w:30)
	//         -> 4 (w:60)
	priorityFrame := &PriorityFrame{
		FrameHeader:      FrameHeader{StreamID: 4},
		StreamDependency: 1,
		Weight:           60,
		Exclusive:        false,
	}
	err = pt.ProcessPriorityFrame(priorityFrame)
	if err != nil {
		t.Fatalf("ProcessPriorityFrame(4) failed: %v", err)
	}
	p, c, w, _ = pt.GetDependencies(4)
	if p != 1 || w != 60 || len(c) != 0 {
		t.Errorf("S4 post PRIORITY: P:%d W:%d C:%v. Expected P:1 W:60 C:[]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(1)
	if p != 2 || w != 30 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{4}) {
		t.Errorf("S1 post S4-PRIORITY: P:%d W:%d C:%v. Expected P:2 W:30 C:[4]", p, w, c)
	}
	p, c, w, _ = pt.GetDependencies(2)
	if p != 0 || w != 15 || !reflect.DeepEqual(sortUint32Slice(c), []uint32{1}) {
		t.Errorf("S2 post S4-PRIORITY: P:%d W:%d C:%v. Expected P:0 W:15 C:[1]", p, w, c)
	}

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
	pt := NewPriorityTree()
	_, _, _, err := pt.GetDependencies(123) // Stream 123 does not exist
	if err == nil {
		t.Fatal("Expected error for non-existent stream, got nil")
	}
	expectedMsg := "stream 123 not found in priority tree"
	if err.Error() != expectedMsg {
		t.Errorf("Expected error message '%s', got '%s'", expectedMsg, err.Error())
	}
}

func TestPriorityTree_GetDependencies_Stream0(t *testing.T) {
	pt := NewPriorityTree()
	_ = pt.AddStream(1, nil) // Add a child to stream 0

	parentID, children, weight, err := pt.GetDependencies(0)
	if err != nil {
		t.Fatalf("GetDependencies(0) failed: %v", err)
	}
	if parentID != 0 {
		t.Errorf("Stream 0 parent: expected 0, got %d", parentID)
	}
	// Weight for stream 0 is not strictly defined in RFC as it's the root,
	// our node stores 0 by default.
	if weight != 0 {
		t.Errorf("Stream 0 weight: expected 0 (or undefined), got %d", weight)
	}
	expectedChildren := []uint32{1}
	if !reflect.DeepEqual(sortUint32Slice(children), sortUint32Slice(expectedChildren)) {
		t.Errorf("Stream 0 children: expected %v, got %v", expectedChildren, children)
	}
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

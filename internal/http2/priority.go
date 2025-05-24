package http2

import (
	"fmt"
	"sync"
)

// priorityNode stores individual stream priority information.
// As per RFC 7540 Section 5.3.
type priorityNode struct {
	// streamID is the ID of the stream this node represents.
	streamID uint32

	// weight is the stream's weight, as specified in a PRIORITY or HEADERS frame.
	// This is an 8-bit value (0-255). The effective weight used for resource
	// allocation is this value + 1 (range 1-256).
	// RFC 7540, Section 5.3.2: "A default weight of 16 is assigned..."
	// This corresponds to a frame value of 15.
	weight uint8

	// parentID is the stream ID of the parent stream.
	// A value of 0 indicates that this stream is dependent on the root (stream 0 itself).
	parentID uint32

	// childrenIDs is a list of stream IDs that are direct children of this node.
	childrenIDs []uint32
}

// PriorityTree manages all priorityNodes and stream dependencies for a connection.
// It provides thread-safe access to the priority state of streams.
// Stream 0 is the implicit root of the tree, and all streams are initially
// dependent on stream 0.
type PriorityTree struct {
	mu    sync.RWMutex
	nodes map[uint32]*priorityNode
}

// streamDependencyInfo holds priority information for a stream.
// Used internally when adding streams.
type streamDependencyInfo struct {
	StreamDependency uint32 // Stream ID of the parent stream
	Weight           uint8  // Weight (0-255, effective 1-256)
	Exclusive        bool   // Exclusive flag for the operation
}

// NewPriorityTree creates and initializes a new PriorityTree.
// It sets up stream 0 as the root of the priority tree.
func NewPriorityTree() *PriorityTree {
	rootNode := &priorityNode{
		streamID:    0,
		weight:      0, // Weight is not applicable to stream 0 itself.
		parentID:    0, // Root's parent is conventionally 0.
		childrenIDs: make([]uint32, 0),
	}
	return &PriorityTree{
		nodes: map[uint32]*priorityNode{
			0: rootNode,
		},
	}
}

// getOrCreateNodeNoLock retrieves an existing priorityNode for streamID or creates a new one
// with default dependencies if it doesn't exist.
// It ensures that stream 0 always exists.
// This method is NOT thread-safe and expects the caller to hold the lock.
func (pt *PriorityTree) getOrCreateNodeNoLock(streamID uint32) *priorityNode {
	if node, exists := pt.nodes[streamID]; exists {
		return node
	}

	// New streams are initially dependent on stream 0 (RFC 7540, Section 5.3.1).
	// Default weight is 16 (frame value 15) (RFC 7540, Section 5.3.2).
	newNode := &priorityNode{
		streamID:    streamID,
		weight:      15, // Default weight 16 (value-1)
		parentID:    0,  // Default parent is stream 0
		childrenIDs: make([]uint32, 0),
	}
	pt.nodes[streamID] = newNode

	// Add this new node as a child of its default parent (stream 0).
	// Stream 0 (rootNode) must exist.
	parentNode := pt.nodes[0]
	// Check if already a child (defensive, shouldn't be for a new node being default-parented)
	isAlreadyChild := false
	for _, childID := range parentNode.childrenIDs {
		if childID == streamID {
			isAlreadyChild = true
			break
		}
	}
	if !isAlreadyChild {
		parentNode.childrenIDs = append(parentNode.childrenIDs, streamID)
	}
	return newNode
}

// removeChild is a helper to remove a streamID from a slice of children.
// It returns a new slice. It's a non-mutating helper.
func removeChild(children []uint32, streamIDToRemove uint32) []uint32 {
	newChildren := make([]uint32, 0, len(children))
	for _, childID := range children {
		if childID != streamIDToRemove {
			newChildren = append(newChildren, childID)
		}
	}
	return newChildren
}

// AddStream adds a new stream to the priority tree or updates its priority
// if it already exists (e.g., due to being referenced as a parent earlier).
// prioInfo contains the dependency details. If nil, default priority is used.
// This function is analogous to processing priority info from a HEADERS frame.
func (pt *PriorityTree) AddStream(streamID uint32, prioInfo *streamDependencyInfo) error {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	if streamID == 0 {
		return &ConnectionError{Code: ErrCodeProtocolError, Msg: "cannot add or modify priority for stream 0"}
	}

	streamNode := pt.getOrCreateNodeNoLock(streamID)

	var newParentID uint32
	var newWeight uint8
	var isExclusiveOperation bool

	if prioInfo != nil {
		newParentID = prioInfo.StreamDependency
		newWeight = prioInfo.Weight
		isExclusiveOperation = prioInfo.Exclusive
	} else {
		// Default priority if prioInfo is nil
		newParentID = 0
		newWeight = 15 // Default weight 16
		isExclusiveOperation = false
	}

	if newParentID == streamID {
		return &StreamError{StreamID: streamID, Code: ErrCodeProtocolError, Msg: "stream cannot depend on itself"}
	}

	// Remove stream from its old parent's children list, if it had one.
	// streamNode.parentID is the ID of the current parent.
	if oldParentNode, exists := pt.nodes[streamNode.parentID]; exists {
		oldParentNode.childrenIDs = removeChild(oldParentNode.childrenIDs, streamID)
	}

	newParentNode := pt.getOrCreateNodeNoLock(newParentID)

	// Update streamNode's properties
	streamNode.parentID = newParentID
	streamNode.weight = newWeight

	if isExclusiveOperation {
		// This stream becomes the sole child of newParentNode.
		// Other children of newParentNode become children of this stream.
		previousChildrenOfNewParent := make([]uint32, len(newParentNode.childrenIDs))
		copy(previousChildrenOfNewParent, newParentNode.childrenIDs)

		newParentNode.childrenIDs = []uint32{streamID} // streamNode is now the only child

		// Preserve streamNode's existing children and append the adopted ones.
		// (RFC 7540, Section 5.3.2: "If the exclusive stream already has dependents, they remain dependents...")
		newlyAdoptedChildren := make([]uint32, 0)
		for _, childID := range previousChildrenOfNewParent {
			if childID == streamID { // Should not happen
				continue
			}
			childNode := pt.getOrCreateNodeNoLock(childID)
			childNode.parentID = streamID
			newlyAdoptedChildren = append(newlyAdoptedChildren, childID)
		}
		streamNode.childrenIDs = append(streamNode.childrenIDs, newlyAdoptedChildren...)

	} else {
		// Not exclusive, just add to new parent's children list if not already there.
		isAlreadyChild := false
		for _, childID := range newParentNode.childrenIDs {
			if childID == streamID {
				isAlreadyChild = true
				break
			}
		}
		if !isAlreadyChild {
			newParentNode.childrenIDs = append(newParentNode.childrenIDs, streamID)
		}
	}
	return nil
}

// UpdatePriority changes the priority of a stream.
// streamID: The ID of the stream whose priority is being updated.
// depStreamID: The ID of the stream that streamID will depend on.
// weight: The new weight for streamID (0-255).
// exclusive: If true, streamID becomes the sole child of depStreamID,
//
//	and previous children of depStreamID become children of streamID.
//
// This method is NOT called directly by external frame processing; it's a utility
// called by AddStream and ProcessPriorityFrame.
// The caller (AddStream/ProcessPriorityFrame) must hold the lock.
func (pt *PriorityTree) updatePriorityNoLock(streamID uint32, depStreamID uint32, weight uint8, exclusive bool) error {
	// Validation for streamID != 0 is typically done by callers (AddStream, ProcessPriorityFrame)
	// as they have specific error types (ConnectionError for stream 0 PRIORITY, StreamError for HEADERS)

	if depStreamID == streamID {
		// This check is crucial and should be here.
		// The specific error type might vary based on the caller's context (PRIORITY vs HEADERS)
		// For now, return a generic error, callers can wrap it.
		return fmt.Errorf("stream %d cannot depend on itself (new parent %d)", streamID, depStreamID)
	}

	streamNode := pt.getOrCreateNodeNoLock(streamID)

	// 1. Detach streamNode from its old parent
	if oldParentNode, exists := pt.nodes[streamNode.parentID]; exists {
		oldParentNode.childrenIDs = removeChild(oldParentNode.childrenIDs, streamID)
	}

	// 2. Get the new parent node
	newParentNode := pt.getOrCreateNodeNoLock(depStreamID)

	// 3. Update streamNode's properties
	streamNode.parentID = depStreamID
	streamNode.weight = weight // weight is 0-255 as per frame spec

	// 4. Handle exclusive flag and attachment to new parent
	if exclusive {
		// streamNode becomes the sole child of newParentNode.
		// Other children of newParentNode become children of streamNode.

		// Make a copy of newParentNode's children before modifying it
		previousChildrenOfNewParent := make([]uint32, len(newParentNode.childrenIDs))
		copy(previousChildrenOfNewParent, newParentNode.childrenIDs)

		newParentNode.childrenIDs = []uint32{streamID} // streamNode is now the only child

		// Adopt previous children of newParentNode.
		// RFC 7540, Section 5.3.3: "The dependents of the new parent are then added as
		// dependents of the exclusive stream."
		// It's implied that the exclusive stream's *own* previous dependents are preserved.
		newlyAdoptedChildren := make([]uint32, 0)
		for _, childID := range previousChildrenOfNewParent {
			if childID == streamID { // Should not happen if logic is correct, but defensive.
				continue
			}
			childNode := pt.getOrCreateNodeNoLock(childID)
			childNode.parentID = streamID // Re-parent to streamID
			newlyAdoptedChildren = append(newlyAdoptedChildren, childID)
		}
		// Prepend newly adopted children to preserve order as per RFC 7540 example,
		// though spec doesn't strictly mandate order of children.
		// Or append: streamNode.childrenIDs = append(streamNode.childrenIDs, newlyAdoptedChildren...)
		// Let's follow the spirit of "added as dependents". Appending is simpler.
		streamNode.childrenIDs = append(streamNode.childrenIDs, newlyAdoptedChildren...)

	} else {
		// Not exclusive: just add streamNode to newParentNode's children list if not already there.
		isAlreadyChild := false
		for _, childID := range newParentNode.childrenIDs {
			if childID == streamID {
				isAlreadyChild = true
				break
			}
		}
		if !isAlreadyChild {
			newParentNode.childrenIDs = append(newParentNode.childrenIDs, streamID)
		}
	}
	return nil
}

// ProcessPriorityFrame updates stream priorities based on a PRIORITY frame.
// streamID is the ID of the stream whose priority is being updated (from PRIORITY frame header).
func (pt *PriorityTree) ProcessPriorityFrame(streamID uint32, frame *PriorityFrame) error {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	if streamID == 0 {
		// PRIORITY frames MUST NOT be sent on stream 0.
		return &ConnectionError{Code: ErrCodeProtocolError, Msg: "PRIORITY frame received on stream 0"}
	}

	newParentID := frame.StreamDependency
	newWeight := frame.Weight
	isExclusiveOperation := frame.Exclusive

	if newParentID == streamID {
		// Stream cannot depend on itself.
		return &StreamError{StreamID: streamID, Code: ErrCodeProtocolError, Msg: "stream cannot depend on itself in PRIORITY frame"}
	}

	streamNode := pt.getOrCreateNodeNoLock(streamID)

	// Remove stream from its old parent's children list.
	if oldParentNode, exists := pt.nodes[streamNode.parentID]; exists {
		oldParentNode.childrenIDs = removeChild(oldParentNode.childrenIDs, streamID)
	}

	newParentNode := pt.getOrCreateNodeNoLock(newParentID)

	// Update streamNode's properties
	streamNode.parentID = newParentID
	streamNode.weight = newWeight

	if isExclusiveOperation {
		previousChildrenOfNewParent := make([]uint32, len(newParentNode.childrenIDs))
		copy(previousChildrenOfNewParent, newParentNode.childrenIDs)
		newParentNode.childrenIDs = []uint32{streamID}

		newlyAdoptedChildren := make([]uint32, 0)
		for _, childID := range previousChildrenOfNewParent {
			if childID == streamID {
				continue
			}
			childNode := pt.getOrCreateNodeNoLock(childID)
			childNode.parentID = streamID
			newlyAdoptedChildren = append(newlyAdoptedChildren, childID)
		}
		streamNode.childrenIDs = append(streamNode.childrenIDs, newlyAdoptedChildren...)
	} else {
		isAlreadyChild := false
		for _, childID := range newParentNode.childrenIDs {
			if childID == streamID {
				isAlreadyChild = true
				break
			}
		}
		if !isAlreadyChild {
			newParentNode.childrenIDs = append(newParentNode.childrenIDs, streamID)
		}
	}
	return nil
}

// RemoveStream removes a stream from the priority tree.
// Its children are re-parented to the removed stream's parent.
func (pt *PriorityTree) RemoveStream(streamID uint32) error {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	if streamID == 0 {
		return &ConnectionError{Code: ErrCodeProtocolError, Msg: "cannot remove stream 0 from priority tree"}
	}

	streamToRemoveNode, exists := pt.nodes[streamID]
	if !exists {
		return nil // Stream not in tree, nothing to remove.
	}

	// Determine the new parent for the children of the removed stream.
	// This is the parent of the stream being removed.
	newParentForChildrenNode, parentExists := pt.nodes[streamToRemoveNode.parentID]
	if !parentExists {
		// This indicates an inconsistent tree. Fallback to root (stream 0).
		newParentForChildrenNode = pt.nodes[0]
	}

	// Remove streamID from its (original) parent's children list.
	if originalParentNode, ok := pt.nodes[streamToRemoveNode.parentID]; ok {
		originalParentNode.childrenIDs = removeChild(originalParentNode.childrenIDs, streamID)
	}

	// Re-parent children of the removed stream.
	for _, childID := range streamToRemoveNode.childrenIDs {
		childNode, childExists := pt.nodes[childID]
		if !childExists {
			continue // Child already removed or tree inconsistent.
		}
		childNode.parentID = newParentForChildrenNode.streamID

		// Add childID to newParentForChildrenNode's children list if not already there.
		isAlreadyChild := false
		for _, c := range newParentForChildrenNode.childrenIDs {
			if c == childID {
				isAlreadyChild = true
				break
			}
		}
		if !isAlreadyChild {
			newParentForChildrenNode.childrenIDs = append(newParentForChildrenNode.childrenIDs, childID)
		}
	}

	delete(pt.nodes, streamID)
	return nil
}

// GetDependencies returns the parent ID, children IDs, and weight for a given stream.
// Returns an error if the stream is not found (except for stream 0).
func (pt *PriorityTree) GetDependencies(streamID uint32) (parentID uint32, childrenIDs []uint32, weight uint8, err error) {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	node, exists := pt.nodes[streamID]
	if !exists {
		// Stream 0 should always exist due to NewPriorityTree.
		// If it's requested and missing, it's an internal error.
		if streamID == 0 {
			return 0, nil, 0, &ConnectionError{Code: ErrCodeInternalError, Msg: "internal error: stream 0 node unexpectedly missing"}
		}
		return 0, nil, 0, fmt.Errorf("stream %d not found in priority tree", streamID)
	}

	childrenCopy := make([]uint32, len(node.childrenIDs))
	copy(childrenCopy, node.childrenIDs)

	return node.parentID, childrenCopy, node.weight, nil
}

// TODO: Implement methods for PriorityTree:
// - func (pt *PriorityTree) AddStream(streamID uint32, dependency StreamDependencyInfo) error
//   (StreamDependencyInfo could come from HEADERS or be default for new streams)
// - func (pt *PriorityTree) ProcessPriorityFrame(frame *PriorityFrame) error
//   (Needs stream ID from frame header as well)
// - func (pt *PriorityTree) RemoveStream(streamID uint32) error
//   (Re-parent children as per RFC 7540, Section 5.3.4)
// - func (pt *PriorityTree) GetDependencies(streamID uint32) (parentID uint32, childrenIDs []uint32, err error)
// - Internal helper: func (pt *PriorityTree) getOrCreateNode(streamID uint32) *priorityNode
//   (Handles creation of nodes for streams that are only referenced, e.g. as parents)

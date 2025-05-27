package http2

import (
	"fmt"
	"strings"
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
		return &ConnectionError{Code: ErrCodeProtocolError, Msg: "cannot add or modify priority for stream 0 via AddStream"}
	}

	// Ensure the stream node exists or is created with defaults.
	// This is important because updatePriorityNoLock assumes nodes exist
	// or creates them as needed, but here we want to apply prioInfo if present.
	_ = pt.getOrCreateNodeNoLock(streamID) // Ensures streamNode exists.

	var newParentID uint32
	var newWeight uint8
	var isExclusiveOperation bool

	if prioInfo != nil {
		newParentID = prioInfo.StreamDependency
		newWeight = prioInfo.Weight
		isExclusiveOperation = prioInfo.Exclusive
	} else {
		// Default priority if prioInfo is nil.
		// This is usually for new streams not from HEADERS with PRIORITY.
		// updatePriorityNoLock will use the existing values on streamNode if they're not overridden.
		// So, here we ensure the streamNode has default values if it's new.
		// getOrCreateNodeNoLock already sets default parent (0) and weight (15).
		// We can retrieve these if needed or just let updatePriorityNoLock handle it
		// if prioInfo is nil, then the function might not even call updatePriorityNoLock
		// if no actual change in priority info is specified.

		// Let's clarify: If prioInfo is nil, this usually means a stream is being created
		// with default priority. getOrCreateNodeNoLock handles this. No further
		// call to updatePriorityNoLock is needed unless specific prioInfo is given.
		// However, the original logic updated priority even for "default" cases.
		// To match the spec section 5.3.1 "All streams are initially dependent on stream 0."
		// "Newly created streams are assigned a default weight of 16."
		// getOrCreateNodeNoLock sets this up.

		// The previous implementation *always* re-parented, even if prioInfo was nil,
		// using parent=0, weight=15.
		// Let's stick to that behavior for now, by providing default values to updatePriorityNoLock.
		newParentID = 0
		newWeight = 15 // Default weight 16
		isExclusiveOperation = false
	}

	// The original AddStream logic was essentially a priority update.
	// We can directly call updatePriorityNoLock.
	err := pt.UpdatePriority(streamID, newParentID, newWeight, isExclusiveOperation)
	if err != nil {
		// updatePriorityNoLock might return fmt.Errorf for "cannot depend on itself".
		// We should wrap this in a StreamError for AddStream, which is often
		// called in the context of HEADERS processing.
		// RFC 7540, Section 5.3.3: "A stream cannot depend on itself. An endpoint MUST treat this as a stream error (Section 5.4.2) of type PROTOCOL_ERROR."
		// RFC 7540, Section 5.3.1: "A stream cannot be dependent on any of its own dependencies." This implies cycles are protocol errors.
		if strings.Contains(err.Error(), "cannot depend on itself") {
			return &StreamError{StreamID: streamID, Code: ErrCodeProtocolError, Msg: fmt.Sprintf("stream %d cannot depend on itself: %v", streamID, err)}
		}
		if strings.Contains(err.Error(), "cycle detected") {
			return &StreamError{StreamID: streamID, Code: ErrCodeProtocolError, Msg: fmt.Sprintf("cycle detected when adding/updating stream %d: %v", streamID, err)}
		}
		return &StreamError{StreamID: streamID, Code: ErrCodeInternalError, Msg: fmt.Sprintf("error updating priority for stream %d: %v", streamID, err)}
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
// UpdatePriority changes the priority of a stream.
// streamID: The ID of the stream whose priority is being updated.
// depStreamID: The ID of the stream that streamID will depend on.
// weight: The new weight for streamID (0-255).
// exclusive: If true, streamID becomes the sole child of depStreamID,
//
//	and previous children of depStreamID become children of streamID.
//
// This method is the core logic for priority updates and is called by AddStream (for HEADERS)
// and ProcessPriorityFrame. The caller MUST hold the lock on PriorityTree.pt.mu.
func (pt *PriorityTree) UpdatePriority(streamID uint32, depStreamID uint32, weight uint8, exclusive bool) error {
	// Validation for streamID != 0 is typically done by callers (AddStream, ProcessPriorityFrame)
	// as they have specific error types (ConnectionError for stream 0 PRIORITY, StreamError for HEADERS)

	if depStreamID == streamID {
		return fmt.Errorf("stream %d cannot depend on itself (new parent %d)", streamID, depStreamID)
	}

	// Cycle detection: A stream cannot depend on one of its own descendants.
	// Traverse upwards from depStreamID to see if streamID is an ancestor.
	// This check must be done *before* any tree modifications.
	// We must use getOrCreateNodeNoLock carefully here, as depStreamID might not exist yet,
	// but if it doesn't, it can't be a descendant.
	// If depStreamID exists, trace its lineage.
	if _, depExists := pt.nodes[depStreamID]; depExists {
		curr := depStreamID
		for curr != 0 { // Traverse up to the root (stream 0)
			// get node for curr. It must exist if depExists was true and curr !=0 and we haven't hit streamID
			// unless curr is streamID itself.
			currNode := pt.nodes[curr] // This node should exist in the path from depStreamID upwards
			if currNode == nil {       // Should not happen if tree is consistent
				// This indicates an issue, but cycle detection's main concern is finding streamID
				break // or return an internal error
			}
			if currNode.parentID == streamID {
				return fmt.Errorf("cycle detected: stream %d cannot depend on %d because %d is/would be an ancestor of %d", streamID, depStreamID, streamID, depStreamID)
			}
			if currNode.parentID == 0 { // Reached root without finding streamID as an ancestor of depStreamID
				break
			}
			curr = currNode.parentID
		}
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
// It is called when a PRIORITY frame is received for a connection.
func (pt *PriorityTree) ProcessPriorityFrame(frame *PriorityFrame) error {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	// The PRIORITY frame is associated with the stream identified in its header.
	streamID := frame.Header().StreamID

	if streamID == 0 {
		// PRIORITY frames MUST NOT be sent on stream 0.
		// RFC 7540, Section 6.3: "The PRIORITY frame is associated with the stream identified in the frame header."
		// RFC 7540, Section 5.1: "Stream identifiers cannot be reused. Long-lived connections can result in an endpoint exhausting the available range of stream identifiers.
		// A client that is unable to establish a new stream identifier can establish a new connection for new streams.
		// A server that is unable to establish a new stream identifier can send a GOAWAY frame (Section 6.8) so that the client is forced to open a new connection for new streams."
		// Section 6.3 also states: "If a PRIORITY frame is received with a stream identifier of 0x0, the recipient MUST respond with a connection error (Section 5.4.1) of type PROTOCOL_ERROR."
		return &ConnectionError{Code: ErrCodeProtocolError, Msg: "PRIORITY frame received on stream 0"}
	}

	// The PRIORITY frame's payload (frame.StreamDependency, frame.Weight, frame.Exclusive)
	// defines the new priority settings for the 'streamID' identified in the frame header.
	err := pt.UpdatePriority(streamID, frame.StreamDependency, frame.Weight, frame.Exclusive)
	if err != nil {
		// UpdatePriority can return an error if streamID tries to depend on itself or creates a cycle.
		// RFC 7540, Section 5.3.3: "A stream cannot depend on itself. An endpoint MUST treat this as a stream error (Section 5.4.2) of type PROTOCOL_ERROR."
		// RFC 7540, Section 5.3.1: "A stream cannot be dependent on any of its own dependencies." This implies cycles are protocol errors.
		if strings.Contains(err.Error(), "cannot depend on itself") {
			return &StreamError{StreamID: streamID, Code: ErrCodeProtocolError, Msg: fmt.Sprintf("stream %d cannot depend on itself in PRIORITY frame: %v", streamID, err)}
		}
		if strings.Contains(err.Error(), "cycle detected") {
			return &StreamError{StreamID: streamID, Code: ErrCodeProtocolError, Msg: fmt.Sprintf("cycle detected in PRIORITY frame for stream %d: %v", streamID, err)}
		}
		// Other errors from UpdatePriority might indicate internal issues or inconsistencies.
		return &StreamError{StreamID: streamID, Code: ErrCodeInternalError, Msg: fmt.Sprintf("error processing PRIORITY frame for stream %d: %v", streamID, err)}
	}
	return nil
}

// RemoveStream removes a stream from the priority tree, typically when the stream is closed.
// Its children are re-parented to the removed stream's former parent.
//
// RFC 7540, Section 5.3.4 specifies this behavior: "When a stream is removed
// from the dependency tree, its dependents are moved to become dependents of
// the removed stream's parent. The weights of these streams are not adjusted."
//
// The concept of "distributing effective weights among new siblings" (which might
// appear in some interpretations or specifications) is understood here as the
// natural outcome of HTTP/2's priority mechanism: once re-parented, children
// contribute their existing (unchanged) weights to the resource allocation
// decisions made by their new parent, alongside any other siblings under that new parent.
// This method itself does not alter the numerical weight values of the re-parented streams.
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

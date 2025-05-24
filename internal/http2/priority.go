package http2

import "sync"

// priorityNode stores individual stream priority information.
// As per RFC 7540 Section 5.3.
// This struct is not typically exported, as its fields are managed by PriorityTree.
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
	// The order might matter for some scheduling algorithms, but RFC 7540
	// does not specify order significance beyond weight.
	childrenIDs []uint32

	// exclusive indicates if this stream was made an exclusive child of its parent
	// when its dependency was last set. If true, it implies that when this
	// dependency was established, this stream became the sole child of parentID,
	// and any previous children of parentID became children of this stream.
	// The ongoing state of exclusivity might be complex if the parent's children
	// list is modified subsequently by other operations.
	exclusive bool

	// Note: Additional fields for scheduler optimization (e.g., pointers to parent/child nodes,
	// total child weights, active child count) could be added but are omitted here
	// to stick to the core structural definition based on the spec's primary requirements.
}

// PriorityTree manages all priorityNodes and stream dependencies for a connection.
// It provides thread-safe access to the priority state of streams.
// Stream 0 is the implicit root of the tree, and all streams are initially
// dependent on stream 0.
type PriorityTree struct {
	// mu protects access to the nodes map and the internal structure of priorityNodes
	// if they were to be modified directly by multiple goroutines (though typically
	// modifications would be serialized through PriorityTree methods).
	mu sync.RWMutex

	// nodes maps a stream ID to its priorityNode.
	// This map includes a node for stream 0, which acts as the root.
	nodes map[uint32]*priorityNode
}

// NewPriorityTree creates and initializes a new PriorityTree.
// It sets up stream 0 as the root of the priority tree.
func NewPriorityTree() *PriorityTree {
	// Stream 0 is the root of the tree. It has no parent and its weight is not relevant.
	// PRIORITY frames cannot be sent *on* stream 0.
	rootNode := &priorityNode{
		streamID:    0,
		weight:      0, // Weight is not applicable to stream 0 itself.
		parentID:    0, // Conventionally, root's parent can be 0 or a special marker.
		childrenIDs: make([]uint32, 0),
		exclusive:   false, // Exclusivity is not applicable to stream 0 itself.
	}

	return &PriorityTree{
		nodes: map[uint32]*priorityNode{
			0: rootNode,
		},
	}
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

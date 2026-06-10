package jswire

import "fmt"

type graphBuilder struct {
	nextID uint32
	nodes  []wireNode
}

func newGraphBuilder() *graphBuilder {
	return &graphBuilder{nextID: 1}
}

func (b *graphBuilder) addNode(node wireNode) (wireValue, error) {
	if b.nextID == 0 {
		return wireValue{}, fmt.Errorf("%w: node id overflow", ErrInvalidWire)
	}
	id := b.nextID
	b.nextID++
	node.ID = id
	b.nodes = append(b.nodes, node)
	return wireValue{Kind: valueRef, Ref: id}, nil
}

func (b *graphBuilder) appendGraph(raw Value) (wireValue, error) {
	graph, err := unmarshalGraph(raw)
	if err != nil {
		return wireValue{}, err
	}
	return appendGraphNodes(&b.nodes, graph, &b.nextID)
}

func (b *graphBuilder) finish(root wireValue) Value {
	return marshalGraph(wireGraph{Root: root, Nodes: b.nodes})
}

func cloneWireGraph(g wireGraph) wireGraph {
	out := wireGraph{
		Root:  g.Root,
		Nodes: make([]wireNode, len(g.Nodes)),
	}
	for i, node := range g.Nodes {
		out.Nodes[i] = copyWireNode(node)
	}
	return out
}

func copyWireNode(node wireNode) wireNode {
	out := node
	out.Props = append([]wireProp(nil), node.Props...)
	out.Slots = append([]wireSlot(nil), node.Slots...)
	out.Entries = append([]wireEntry(nil), node.Entries...)
	out.Values = append([]wireValue(nil), node.Values...)
	out.Bytes = append([]byte(nil), node.Bytes...)
	return out
}

func appendGraphNodes(dst *[]wireNode, src wireGraph, nextID *uint32) (wireValue, error) {
	if err := validateGraph(src); err != nil {
		return wireValue{}, err
	}
	remap := make(map[uint32]uint32, len(src.Nodes))
	for _, node := range src.Nodes {
		if *nextID == 0 {
			return wireValue{}, fmt.Errorf("%w: node id overflow", ErrInvalidWire)
		}
		remap[node.ID] = *nextID
		*nextID = *nextID + 1
	}
	root, err := remapWireValue(src.Root, remap)
	if err != nil {
		return wireValue{}, err
	}
	for _, node := range src.Nodes {
		remapped, err := remapWireNode(node, remap)
		if err != nil {
			return wireValue{}, err
		}
		*dst = append(*dst, remapped)
	}
	return root, nil
}

func remapWireNode(node wireNode, remap map[uint32]uint32) (wireNode, error) {
	out := copyWireNode(node)
	id, ok := remap[node.ID]
	if !ok {
		return wireNode{}, fmt.Errorf("%w: missing node id remap %d", ErrInvalidWire, node.ID)
	}
	out.ID = id
	for i, prop := range out.Props {
		value, err := remapWireValue(prop.Value, remap)
		if err != nil {
			return wireNode{}, err
		}
		out.Props[i].Value = value
	}
	for i, slot := range out.Slots {
		if !slot.Present {
			continue
		}
		value, err := remapWireValue(slot.Value, remap)
		if err != nil {
			return wireNode{}, err
		}
		out.Slots[i].Value = value
	}
	for i, entry := range out.Entries {
		key, err := remapWireValue(entry.Key, remap)
		if err != nil {
			return wireNode{}, err
		}
		value, err := remapWireValue(entry.Value, remap)
		if err != nil {
			return wireNode{}, err
		}
		out.Entries[i] = wireEntry{Key: key, Value: value}
	}
	for i, item := range out.Values {
		value, err := remapWireValue(item, remap)
		if err != nil {
			return wireNode{}, err
		}
		out.Values[i] = value
	}
	buffer, err := remapWireValue(out.Buffer, remap)
	if err != nil {
		return wireNode{}, err
	}
	out.Buffer = buffer
	promise, err := remapWireValue(out.Promise, remap)
	if err != nil {
		return wireNode{}, err
	}
	out.Promise = promise
	cause, err := remapWireValue(out.Cause, remap)
	if err != nil {
		return wireNode{}, err
	}
	out.Cause = cause
	return out, nil
}

func remapWireValue(v wireValue, remap map[uint32]uint32) (wireValue, error) {
	if v.Kind != valueRef {
		return v, nil
	}
	ref, ok := remap[v.Ref]
	if !ok {
		return wireValue{}, fmt.Errorf("%w: missing ref remap %d", ErrInvalidWire, v.Ref)
	}
	v.Ref = ref
	return v, nil
}

func nextNodeID(nodes []wireNode) uint32 {
	var max uint32
	for _, node := range nodes {
		if node.ID > max {
			max = node.ID
		}
	}
	if max == ^uint32(0) {
		return 0
	}
	return max + 1
}

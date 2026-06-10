package jswire

import (
	"fmt"
	"sort"
)

// Apply applies jswire patch operations to v. Object property updates are made
// at the wire-graph level so untouched JavaScript-native values keep their graph
// identity.
func (v Value) Apply(p Patch) (Value, error) {
	if len(p) == 0 {
		return v.Clone(), nil
	}
	current := v.Clone()
	if len(current) == 0 {
		current = marshalGraph(wireGraph{
			Root:  wireValue{Kind: valueRef, Ref: 1},
			Nodes: []wireNode{{ID: 1, Kind: nodeObject}},
		})
	}
	for i, op := range p {
		next, err := applyOnePatch(current, op)
		if err != nil {
			return nil, fmt.Errorf("patch op %d %s at %s: %w", i, op.Op, formatPath(op.Path), err)
		}
		current = next
	}
	return current, nil
}

// MergeObject sets top-level object fields on v. A nil or empty Value is treated
// as an empty object.
func (v Value) MergeObject(fields ObjectType) (Value, error) {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	patch := make(Patch, 0, len(keys))
	for _, key := range keys {
		patch = append(patch, PatchOp{
			Op:    PatchSet,
			Path:  Path{{Key: key}},
			Value: fields[key],
		})
	}
	return v.Apply(patch)
}

func applyOnePatch(raw Value, op PatchOp) (Value, error) {
	switch op.Op {
	case PatchSet:
		if len(op.Path) == 0 {
			return Encode(op.Value)
		}
		return patchSet(raw, op.Path, op.Value)
	case PatchRemove:
		if len(op.Path) == 0 {
			return nil, fmt.Errorf("remove root is not supported")
		}
		return patchRemove(raw, op.Path)
	default:
		return nil, fmt.Errorf("unknown patch op %q", op.Op)
	}
}

func patchSet(raw Value, path Path, value any) (Value, error) {
	ed, err := newGraphEditor(raw)
	if err != nil {
		return nil, err
	}
	encoded, err := Encode(value)
	if err != nil {
		return nil, err
	}
	val, err := ed.appendValue(encoded)
	if err != nil {
		return nil, err
	}
	node, key, err := ed.objectParent(path)
	if err != nil {
		return nil, err
	}
	ed.setObjectProp(node, key, val)
	return ed.finish()
}

func patchRemove(raw Value, path Path) (Value, error) {
	ed, err := newGraphEditor(raw)
	if err != nil {
		return nil, err
	}
	node, key, err := ed.objectParent(path)
	if err != nil {
		return nil, err
	}
	ed.removeObjectProp(node, key)
	return ed.finish()
}

type graphEditor struct {
	graph  wireGraph
	byID   map[uint32]int
	nextID uint32
}

func newGraphEditor(raw Value) (*graphEditor, error) {
	graph, err := unmarshalGraph(raw)
	if err != nil {
		return nil, err
	}
	graph = cloneWireGraph(graph)
	byID := make(map[uint32]int, len(graph.Nodes))
	for i, node := range graph.Nodes {
		byID[node.ID] = i
	}
	return &graphEditor{graph: graph, byID: byID, nextID: nextNodeID(graph.Nodes)}, nil
}

func (e *graphEditor) appendValue(raw Value) (wireValue, error) {
	graph, err := unmarshalGraph(raw)
	if err != nil {
		return wireValue{}, err
	}
	root, err := appendGraphNodes(&e.graph.Nodes, graph, &e.nextID)
	if err != nil {
		return wireValue{}, err
	}
	e.reindex()
	return root, nil
}

func (e *graphEditor) finish() (Value, error) {
	if err := validateGraph(e.graph); err != nil {
		return nil, err
	}
	return marshalGraph(e.graph), nil
}

func (e *graphEditor) reindex() {
	e.byID = make(map[uint32]int, len(e.graph.Nodes))
	for i, node := range e.graph.Nodes {
		e.byID[node.ID] = i
	}
}

func (e *graphEditor) objectParent(path Path) (*wireNode, string, error) {
	if len(path) == 0 {
		return nil, "", fmt.Errorf("empty path has no object parent")
	}
	for i, seg := range path {
		if seg.Index != nil {
			return nil, "", fmt.Errorf("array index path segments are not supported: %s", formatPath(path[:i+1]))
		}
	}
	node, err := e.rootObject()
	if err != nil {
		return nil, "", err
	}
	for i, seg := range path[:len(path)-1] {
		if node.Kind != nodeObject {
			return nil, "", fmt.Errorf("path %s traverses non-object", formatPath(path[:i+1]))
		}
		value, ok := objectPropValue(*node, seg.Key)
		if !ok {
			return nil, "", fmt.Errorf("path %s is missing", formatPath(path[:i+1]))
		}
		if value.Kind != valueRef {
			return nil, "", fmt.Errorf("path %s traverses non-object", formatPath(path[:i+1]))
		}
		next, err := e.node(value.Ref)
		if err != nil {
			return nil, "", err
		}
		if next.Kind != nodeObject {
			return nil, "", fmt.Errorf("path %s traverses non-object", formatPath(path[:i+1]))
		}
		node = next
	}
	if node.Kind != nodeObject {
		return nil, "", fmt.Errorf("path %s target parent is not object", formatPath(path))
	}
	return node, path[len(path)-1].Key, nil
}

func (e *graphEditor) rootObject() (*wireNode, error) {
	if e.graph.Root.Kind != valueRef {
		return nil, fmt.Errorf("root is not object")
	}
	node, err := e.node(e.graph.Root.Ref)
	if err != nil {
		return nil, err
	}
	if node.Kind != nodeObject {
		return nil, fmt.Errorf("root is not object")
	}
	return node, nil
}

func (e *graphEditor) node(ref uint32) (*wireNode, error) {
	idx, ok := e.byID[ref]
	if !ok {
		return nil, invalidWiref("missing ref %d", ref)
	}
	return &e.graph.Nodes[idx], nil
}

func (e *graphEditor) setObjectProp(node *wireNode, key string, value wireValue) {
	for i := range node.Props {
		if node.Props[i].Key == key {
			node.Props[i].Value = value
			return
		}
	}
	node.Props = append(node.Props, wireProp{Key: key, Value: value})
}

func (e *graphEditor) removeObjectProp(node *wireNode, key string) {
	for i := range node.Props {
		if node.Props[i].Key == key {
			copy(node.Props[i:], node.Props[i+1:])
			node.Props = node.Props[:len(node.Props)-1]
			return
		}
	}
}

func objectPropValue(node wireNode, key string) (wireValue, bool) {
	for _, prop := range node.Props {
		if prop.Key == key {
			return prop.Value, true
		}
	}
	return wireValue{}, false
}

package jswire

import "time"

// Decode converts jswire binary data into the typed Go model.
func Decode(raw Value) (any, error) {
	graph, err := unmarshalGraph(raw)
	if err != nil {
		return nil, err
	}
	dec := typedDecoder{
		nodes:  make(map[uint32]wireNode, len(graph.Nodes)),
		active: make(map[uint32]bool),
		seen:   make(map[uint32]bool),
	}
	for _, node := range graph.Nodes {
		dec.nodes[node.ID] = node
	}
	return dec.decodeValue(graph.Root)
}

type typedDecoder struct {
	nodes  map[uint32]wireNode
	active map[uint32]bool
	seen   map[uint32]bool
}

func (d *typedDecoder) decodeValue(v wireValue) (any, error) {
	switch v.Kind {
	case valueUndefined, valueNull:
		return nil, nil
	case valueBool:
		return v.Bool, nil
	case valueNumber:
		return v.Number, nil
	case valueString:
		return v.Text, nil
	case valueBigInt:
		if err := validateBigIntText(v.Text); err != nil {
			return nil, invalidWiref("invalid BigInt %q: %v", v.Text, err)
		}
		return BigIntType(v.Text), nil
	case valueRef:
		return d.decodeRef(v.Ref)
	default:
		return nil, invalidWiref("unknown value kind %d", v.Kind)
	}
}

func (d *typedDecoder) decodeRef(ref uint32) (any, error) {
	node, ok := d.nodes[ref]
	if !ok {
		return nil, invalidWiref("missing ref %d", ref)
	}
	if d.active[ref] {
		return nil, invalidWiref("cycle at ref %d", ref)
	}
	if d.seen[ref] {
		return nil, invalidWiref("shared ref %d cannot be represented by the typed Go model", ref)
	}
	d.seen[ref] = true
	d.active[ref] = true
	defer delete(d.active, ref)

	switch node.Kind {
	case nodeObject:
		return d.decodeObject(node)
	case nodeArray:
		return d.decodeArray(node)
	case nodeMap:
		return d.decodeMap(node)
	case nodeSet:
		return d.decodeSet(node)
	case nodeDate:
		if !node.DateValid {
			return nil, invalidWiref("invalid Date has no typed Go model timestamp")
		}
		return DateType(time.UnixMilli(node.DateMS).UTC()), nil
	case nodeRegexp:
		return RegExpType{node.TextA, node.TextB}, nil
	case nodeArrayBuffer:
		return ArrayBufferType(append([]byte(nil), node.Bytes...)), nil
	case nodeTypedArray:
		return d.decodeTypedArray(node)
	case nodePromise:
		return nil, invalidWiref("Promise has no typed Go model")
	case nodeError:
		name := node.TextA
		if name == "" {
			name = "Error"
		}
		return nil, invalidWiref("unsupported %s Error has no typed Go model", name)
	default:
		return nil, invalidWiref("unknown node kind %d", node.Kind)
	}
}

func (d *typedDecoder) decodeObject(node wireNode) (ObjectType, error) {
	if node.TextA != "" {
		return nil, invalidWiref("custom object %s has no typed Go model", node.TextA)
	}
	out := make(ObjectType, len(node.Props))
	for _, prop := range node.Props {
		value, err := d.decodeValue(prop.Value)
		if err != nil {
			return nil, err
		}
		out[prop.Key] = value
	}
	return out, nil
}

func (d *typedDecoder) decodeArray(node wireNode) (ArrayType, error) {
	out := make(ArrayType, len(node.Slots))
	for i, slot := range node.Slots {
		if !slot.Present {
			return nil, invalidWiref("array hole at index %d cannot be represented by ArrayType", i)
		}
		value, err := d.decodeValue(slot.Value)
		if err != nil {
			return nil, err
		}
		out[i] = value
	}
	return out, nil
}

func (d *typedDecoder) decodeMap(node wireNode) (MapType, error) {
	out := make(MapType, len(node.Entries))
	for i, entry := range node.Entries {
		key, err := d.decodeValue(entry.Key)
		if err != nil {
			return nil, err
		}
		value, err := d.decodeValue(entry.Value)
		if err != nil {
			return nil, err
		}
		out[i] = MapEntry{key, value}
	}
	return out, nil
}

func (d *typedDecoder) decodeSet(node wireNode) (SetType, error) {
	out := make(SetType, len(node.Values))
	for i, item := range node.Values {
		value, err := d.decodeValue(item)
		if err != nil {
			return nil, err
		}
		out[i] = value
	}
	return out, nil
}

func (d *typedDecoder) decodeTypedArray(node wireNode) (any, error) {
	if node.TextA == "DataView" {
		return nil, invalidWiref("DataView has no typed Go model")
	}
	if node.Buffer.Kind != valueRef {
		return nil, invalidWiref("typed array buffer is not a ref")
	}
	buffer, ok := d.nodes[node.Buffer.Ref]
	if !ok {
		return nil, invalidWiref("missing typed array buffer ref %d", node.Buffer.Ref)
	}
	if buffer.Kind != nodeArrayBuffer {
		return nil, invalidWiref("typed array buffer ref %d is not ArrayBuffer", node.Buffer.Ref)
	}
	raw, err := typedArrayWindowBytes(node, buffer)
	if err != nil {
		return nil, err
	}
	return typedArrayModelFromBytes(node.TextA, raw)
}

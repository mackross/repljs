package jswire

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxJSSafeInteger = int64(1<<53 - 1)

// Encode converts a supported typed Go model value into jswire binary data.
// Nil encodes as JavaScript null.
func Encode(v any) (Value, error) {
	if raw, ok := v.(Value); ok {
		return raw.Clone(), nil
	}
	enc := typedEncoder{builder: newGraphBuilder()}
	root, err := enc.encode(v, "")
	if err != nil {
		return nil, err
	}
	return enc.builder.finish(root), nil
}

type typedEncoder struct {
	builder *graphBuilder
}

func (e *typedEncoder) encode(v any, path string) (wireValue, error) {
	if v == nil {
		return wireValue{Kind: valueNull}, nil
	}
	if ctor, raw, ok := typedArrayModelBytes(v); ok {
		return e.encodeTypedArray(ctor, raw)
	}

	switch x := v.(type) {
	case Value:
		return e.builder.appendGraph(x)
	case bool:
		return wireValue{Kind: valueBool, Bool: x}, nil
	case string:
		return wireValue{Kind: valueString, Text: x}, nil
	case int:
		return numberFromInt(path, int64(x), "int")
	case int8:
		return numberFromInt(path, int64(x), "int8")
	case int16:
		return numberFromInt(path, int64(x), "int16")
	case int32:
		return numberFromInt(path, int64(x), "int32")
	case int64:
		return numberFromInt(path, x, "int64")
	case uint:
		return numberFromUint(path, uint64(x), "uint")
	case uint8:
		return numberFromUint(path, uint64(x), "uint8")
	case uint16:
		return numberFromUint(path, uint64(x), "uint16")
	case uint32:
		return numberFromUint(path, uint64(x), "uint32")
	case uint64:
		return numberFromUint(path, x, "uint64")
	case float32:
		return wireValue{Kind: valueNumber, Number: float64(x)}, nil
	case float64:
		return wireValue{Kind: valueNumber, Number: x}, nil
	case ObjectType:
		return e.encodeObject(x, path)
	case map[string]any:
		return e.encodeObject(ObjectType(x), path)
	case ArrayType:
		return e.encodeArray([]any(x), path)
	case []any:
		return e.encodeArray(x, path)
	case MapType:
		return e.encodeMap(x, path)
	case SetType:
		return e.encodeSet(x, path)
	case DateType:
		tm := time.Time(x)
		return e.builder.addNode(wireNode{Kind: nodeDate, DateValid: true, DateMS: tm.UnixMilli()})
	case time.Time:
		return e.builder.addNode(wireNode{Kind: nodeDate, DateValid: true, DateMS: x.UnixMilli()})
	case BigIntType:
		if err := validateBigIntText(string(x)); err != nil {
			return wireValue{}, encodePathError(path, "invalid BigInt %q: %v", string(x), err)
		}
		return wireValue{Kind: valueBigInt, Text: string(x)}, nil
	case RegExpType:
		if err := validateRegExpFlags(x[1]); err != nil {
			return wireValue{}, encodePathError(path, "invalid RegExp flags %q: %v", x[1], err)
		}
		return e.builder.addNode(wireNode{Kind: nodeRegexp, TextA: x[0], TextB: x[1]})
	case ArrayBufferType:
		return e.builder.addNode(wireNode{Kind: nodeArrayBuffer, Bytes: append([]byte(nil), x...)})
	default:
		return wireValue{}, unsupportedEncodeType(path, v)
	}
}

func (e *typedEncoder) encodeObject(obj ObjectType, path string) (wireValue, error) {
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	node := wireNode{Kind: nodeObject, Props: make([]wireProp, 0, len(keys))}
	for _, key := range keys {
		value, err := e.encode(obj[key], objectPath(path, key))
		if err != nil {
			return wireValue{}, err
		}
		node.Props = append(node.Props, wireProp{Key: key, Value: value})
	}
	return e.builder.addNode(node)
}

func (e *typedEncoder) encodeArray(items []any, path string) (wireValue, error) {
	node := wireNode{Kind: nodeArray, Slots: make([]wireSlot, len(items))}
	for i, item := range items {
		value, err := e.encode(item, arrayPath(path, i))
		if err != nil {
			return wireValue{}, err
		}
		node.Slots[i] = wireSlot{Present: true, Value: value}
	}
	return e.builder.addNode(node)
}

func (e *typedEncoder) encodeMap(entries MapType, path string) (wireValue, error) {
	node := wireNode{Kind: nodeMap, Entries: make([]wireEntry, 0, len(entries))}
	for i, entry := range entries {
		key, err := e.encode(entry[0], mapEntryPath(path, i, "key"))
		if err != nil {
			return wireValue{}, err
		}
		value, err := e.encode(entry[1], mapEntryPath(path, i, "value"))
		if err != nil {
			return wireValue{}, err
		}
		node.Entries = append(node.Entries, wireEntry{Key: key, Value: value})
	}
	return e.builder.addNode(node)
}

func (e *typedEncoder) encodeSet(items SetType, path string) (wireValue, error) {
	node := wireNode{Kind: nodeSet, Values: make([]wireValue, 0, len(items))}
	for i, item := range items {
		value, err := e.encode(item, setValuePath(path, i))
		if err != nil {
			return wireValue{}, err
		}
		node.Values = append(node.Values, value)
	}
	return e.builder.addNode(node)
}

func (e *typedEncoder) encodeTypedArray(ctor string, raw []byte) (wireValue, error) {
	buffer, err := e.builder.addNode(wireNode{Kind: nodeArrayBuffer, Bytes: raw})
	if err != nil {
		return wireValue{}, err
	}
	return e.builder.addNode(wireNode{
		Kind:       nodeTypedArray,
		TextA:      ctor,
		Buffer:     buffer,
		ByteLength: uint32(len(raw)),
	})
}

func numberFromInt(path string, v int64, name string) (wireValue, error) {
	if v < -maxJSSafeInteger || v > maxJSSafeInteger {
		return wireValue{}, encodePathError(path, "%s value %d is outside JavaScript safe integer range", name, v)
	}
	return wireValue{Kind: valueNumber, Number: float64(v)}, nil
}

func numberFromUint(path string, v uint64, name string) (wireValue, error) {
	if v > uint64(maxJSSafeInteger) {
		return wireValue{}, encodePathError(path, "%s value %d is outside JavaScript safe integer range", name, v)
	}
	return wireValue{Kind: valueNumber, Number: float64(v)}, nil
}

func validateBigIntText(s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	if strings.TrimSpace(s) != s {
		return fmt.Errorf("contains whitespace")
	}
	if s[0] == '-' {
		s = s[1:]
		if s == "" {
			return fmt.Errorf("missing digits")
		}
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return fmt.Errorf("contains non-decimal digit %q", r)
		}
	}
	return nil
}

func validateRegExpFlags(flags string) error {
	seen := make(map[rune]struct{}, len(flags))
	for _, r := range flags {
		switch r {
		case 'd', 'g', 'i', 'm', 's', 'u', 'v', 'y':
		default:
			return fmt.Errorf("illegal flag %q", r)
		}
		if _, ok := seen[r]; ok {
			return fmt.Errorf("duplicate flag %q", r)
		}
		seen[r] = struct{}{}
	}
	return nil
}

func unsupportedEncodeType(path string, v any) error {
	return encodePathError(path, "unsupported type %T", v)
}

func encodePathError(path, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if path == "" {
		return fmt.Errorf("encode value: %s", msg)
	}
	return fmt.Errorf("encode %s: %s", path, msg)
}

func objectPath(base, key string) string {
	if base == "" {
		return key
	}
	if key == "" {
		return base + "."
	}
	return base + "." + key
}

func arrayPath(base string, index int) string {
	label := fmt.Sprintf("array index %d", index)
	if base == "" {
		return label
	}
	return base + " " + label
}

func mapEntryPath(base string, index int, side string) string {
	label := fmt.Sprintf("map entry %d %s", index, side)
	if base == "" {
		return label
	}
	return base + " " + label
}

func setValuePath(base string, index int) string {
	label := fmt.Sprintf("set value %d", index)
	if base == "" {
		return label
	}
	return base + " " + label
}

func invalidWiref(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidWire}, args...)...)
}

func formatPath(path Path) string {
	if len(path) == 0 {
		return "<root>"
	}
	parts := make([]string, len(path))
	for i, seg := range path {
		if seg.Index != nil {
			parts[i] = "[" + strconv.Itoa(*seg.Index) + "]"
		} else {
			parts[i] = seg.Key
		}
	}
	return strings.Join(parts, ".")
}

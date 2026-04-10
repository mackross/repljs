package jswire

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Inspection is a bounded textual rendering of one bridge payload.
type Inspection struct {
	Summary string
	Full    string
}

type inspectOptions struct {
	maxDepth         int
	maxProps         int
	maxArrayItems    int
	maxMapEntries    int
	maxSetValues     int
	maxTypedElements int
	maxStringRunes   int
	maxBytes         int
}

type wireInspector struct {
	nodes    map[uint32]wireNode
	refCount map[uint32]int
	opts     inspectOptions
	labels   map[uint32]int
	next     int
	rendered map[uint32]bool
	active   map[uint32]bool
}

var (
	summaryInspectOptions = inspectOptions{
		maxDepth:         2,
		maxProps:         4,
		maxArrayItems:    4,
		maxMapEntries:    3,
		maxSetValues:     4,
		maxTypedElements: 6,
		maxStringRunes:   40,
		maxBytes:         8,
	}
	fullInspectOptions = inspectOptions{
		maxDepth:         4,
		maxProps:         8,
		maxArrayItems:    8,
		maxMapEntries:    6,
		maxSetValues:     8,
		maxTypedElements: 12,
		maxStringRunes:   160,
		maxBytes:         24,
	}
)

// Describe renders a compact summary and a fuller bounded representation of a
// versioned jswire payload without decoding it into a live JS runtime.
func Describe(raw []byte) (Inspection, error) {
	if len(raw) == 0 {
		return Inspection{}, nil
	}
	graph, err := unmarshalGraph(raw)
	if err != nil {
		return Inspection{}, err
	}
	summary := newWireInspector(graph, summaryInspectOptions).renderValue(graph.Root, 0)
	full := newWireInspector(graph, fullInspectOptions).renderValue(graph.Root, 0)
	return Inspection{Summary: summary, Full: full}, nil
}

func newWireInspector(graph wireGraph, opts inspectOptions) *wireInspector {
	nodes := make(map[uint32]wireNode, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	refCount := make(map[uint32]int, len(graph.Nodes))
	countWireValueRefs(graph.Root, refCount)
	for _, node := range graph.Nodes {
		countWireNodeRefs(node, refCount)
	}
	return &wireInspector{
		nodes:    nodes,
		refCount: refCount,
		opts:     opts,
		labels:   make(map[uint32]int),
		rendered: make(map[uint32]bool),
		active:   make(map[uint32]bool),
	}
}

func countWireNodeRefs(node wireNode, refs map[uint32]int) {
	for _, prop := range node.Props {
		countWireValueRefs(prop.Value, refs)
	}
	for _, slot := range node.Slots {
		if slot.Present {
			countWireValueRefs(slot.Value, refs)
		}
	}
	for _, entry := range node.Entries {
		countWireValueRefs(entry.Key, refs)
		countWireValueRefs(entry.Value, refs)
	}
	for _, value := range node.Values {
		countWireValueRefs(value, refs)
	}
	countWireValueRefs(node.Buffer, refs)
	countWireValueRefs(node.Cause, refs)
}

func countWireValueRefs(v wireValue, refs map[uint32]int) {
	if v.Kind == valueRef {
		refs[v.Ref]++
	}
}

func (i *wireInspector) renderValue(v wireValue, depth int) string {
	switch v.Kind {
	case valueUndefined:
		return "undefined"
	case valueNull:
		return "null"
	case valueBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case valueNumber:
		return formatNumber(v.Number)
	case valueString:
		return i.renderString(v.Text)
	case valueBigInt:
		return v.Text + "n"
	case valueRef:
		return i.renderRef(v.Ref, depth)
	default:
		return "<invalid>"
	}
}

func (i *wireInspector) renderRef(ref uint32, depth int) string {
	node, ok := i.nodes[ref]
	if !ok {
		return fmt.Sprintf("<missing-ref:%d>", ref)
	}
	if i.active[ref] {
		return "*" + strconv.Itoa(i.label(ref))
	}

	if i.refCount[ref] > 1 {
		if i.rendered[ref] {
			return "*" + strconv.Itoa(i.label(ref))
		}
		i.rendered[ref] = true
		i.active[ref] = true
		body := i.renderNode(node, depth)
		delete(i.active, ref)
		return fmt.Sprintf("&%d %s", i.label(ref), body)
	}

	i.active[ref] = true
	body := i.renderNode(node, depth)
	delete(i.active, ref)
	return body
}

func (i *wireInspector) label(ref uint32) int {
	if label, ok := i.labels[ref]; ok {
		return label
	}
	i.next++
	i.labels[ref] = i.next
	return i.next
}

func (i *wireInspector) renderNode(node wireNode, depth int) string {
	if depth >= i.opts.maxDepth {
		return i.renderCollapsedNode(node)
	}
	switch node.Kind {
	case nodeObject:
		return i.renderObject(node.Props, depth+1)
	case nodeArray:
		return i.renderArray(node, depth+1)
	case nodeMap:
		return i.renderMap(node, depth+1)
	case nodeSet:
		return i.renderSet(node, depth+1)
	case nodeDate:
		base := "Date(invalid)"
		if node.DateValid {
			base = "Date(" + time.UnixMilli(node.DateMS).UTC().Format(time.RFC3339Nano) + ")"
		}
		return i.appendProps(base, node.Props, depth+1)
	case nodeRegexp:
		return i.appendProps("/"+node.TextA+"/"+node.TextB, node.Props, depth+1)
	case nodeArrayBuffer:
		base := fmt.Sprintf("ArrayBuffer(%d bytes %s)", len(node.Bytes), formatHexBytes(node.Bytes, i.opts.maxBytes))
		return i.appendProps(base, node.Props, depth+1)
	case nodeTypedArray:
		return i.renderTypedArray(node, depth+1)
	case nodeError:
		base := renderErrorBase(i, node, depth+1)
		return i.appendProps(base, node.Props, depth+1)
	default:
		return "<invalid-node>"
	}
}

func (i *wireInspector) renderCollapsedNode(node wireNode) string {
	switch node.Kind {
	case nodeObject:
		if len(node.Props) == 0 {
			return "{}"
		}
		return "{…}"
	case nodeArray:
		return fmt.Sprintf("Array(%d)[…]", len(node.Slots))
	case nodeMap:
		return fmt.Sprintf("Map(%d){…}", len(node.Entries))
	case nodeSet:
		return fmt.Sprintf("Set(%d){…}", len(node.Values))
	case nodeDate:
		if !node.DateValid {
			return "Date(invalid)"
		}
		return "Date(" + time.UnixMilli(node.DateMS).UTC().Format(time.RFC3339Nano) + ")"
	case nodeRegexp:
		return "/" + node.TextA + "/" + node.TextB
	case nodeArrayBuffer:
		return fmt.Sprintf("ArrayBuffer(%d bytes)", len(node.Bytes))
	case nodeTypedArray:
		if node.TextA == "DataView" {
			return fmt.Sprintf("DataView(%d bytes)", node.ByteLength)
		}
		return fmt.Sprintf("%s(%d)", node.TextA, typedArrayLength(node))
	case nodeError:
		return renderErrorBase(i, node, i.opts.maxDepth)
	default:
		return "…"
	}
}

func renderErrorBase(i *wireInspector, node wireNode, depth int) string {
	name := node.TextA
	if name == "" {
		name = "Error"
	}
	base := fmt.Sprintf("%s(%s)", name, i.renderString(node.TextB))
	if node.Cause.Kind != valueUndefined && node.Cause.Kind != valueNull {
		base += " cause=" + i.renderValue(node.Cause, depth)
	}
	return base
}

func (i *wireInspector) renderObject(props []wireProp, depth int) string {
	if len(props) == 0 {
		return "{}"
	}
	return i.renderPropsBlock(props, depth)
}

func (i *wireInspector) renderArray(node wireNode, depth int) string {
	parts := make([]string, 0, minInt(len(node.Slots), i.opts.maxArrayItems)+1)
	limit := minInt(len(node.Slots), i.opts.maxArrayItems)
	for idx := 0; idx < limit; idx++ {
		slot := node.Slots[idx]
		if !slot.Present {
			parts = append(parts, "<hole>")
			continue
		}
		parts = append(parts, i.renderValue(slot.Value, depth))
	}
	if len(node.Slots) > limit {
		parts = append(parts, fmt.Sprintf("…+%d", len(node.Slots)-limit))
	}
	base := fmt.Sprintf("Array(%d)[%s]", len(node.Slots), strings.Join(parts, ", "))
	return i.appendProps(base, node.Props, depth)
}

func (i *wireInspector) renderMap(node wireNode, depth int) string {
	parts := make([]string, 0, minInt(len(node.Entries), i.opts.maxMapEntries)+1)
	limit := minInt(len(node.Entries), i.opts.maxMapEntries)
	for idx := 0; idx < limit; idx++ {
		entry := node.Entries[idx]
		parts = append(parts, i.renderValue(entry.Key, depth)+" => "+i.renderValue(entry.Value, depth))
	}
	if len(node.Entries) > limit {
		parts = append(parts, fmt.Sprintf("…+%d", len(node.Entries)-limit))
	}
	base := fmt.Sprintf("Map(%d){%s}", len(node.Entries), strings.Join(parts, ", "))
	return i.appendProps(base, node.Props, depth)
}

func (i *wireInspector) renderSet(node wireNode, depth int) string {
	parts := make([]string, 0, minInt(len(node.Values), i.opts.maxSetValues)+1)
	limit := minInt(len(node.Values), i.opts.maxSetValues)
	for idx := 0; idx < limit; idx++ {
		parts = append(parts, i.renderValue(node.Values[idx], depth))
	}
	if len(node.Values) > limit {
		parts = append(parts, fmt.Sprintf("…+%d", len(node.Values)-limit))
	}
	base := fmt.Sprintf("Set(%d){%s}", len(node.Values), strings.Join(parts, ", "))
	return i.appendProps(base, node.Props, depth)
}

func (i *wireInspector) renderTypedArray(node wireNode, depth int) string {
	if node.TextA == "DataView" {
		base := fmt.Sprintf("DataView(%d bytes %s)", node.ByteLength, formatHexBytes(i.typedArrayWindow(node), i.opts.maxBytes))
		return i.appendProps(base, node.Props, depth)
	}
	parts, omitted := renderTypedArraySample(node.TextA, i.typedArrayWindow(node), i.opts.maxTypedElements)
	if omitted > 0 {
		parts = append(parts, fmt.Sprintf("…+%d", omitted))
	}
	base := fmt.Sprintf("%s(%d)[%s]", node.TextA, typedArrayLength(node), strings.Join(parts, ", "))
	return i.appendProps(base, node.Props, depth)
}

func (i *wireInspector) typedArrayWindow(node wireNode) []byte {
	buffer := i.nodes[node.Buffer.Ref]
	start := int(node.ByteOffset)
	end := start + int(node.ByteLength)
	return buffer.Bytes[start:end]
}

func (i *wireInspector) appendProps(base string, props []wireProp, depth int) string {
	if len(props) == 0 {
		return base
	}
	return base + " " + i.renderPropsBlock(props, depth)
}

func (i *wireInspector) renderPropsBlock(props []wireProp, depth int) string {
	parts := make([]string, 0, minInt(len(props), i.opts.maxProps)+1)
	limit := minInt(len(props), i.opts.maxProps)
	for idx := 0; idx < limit; idx++ {
		prop := props[idx]
		parts = append(parts, fmt.Sprintf("%s: %s", formatPropKey(prop.Key), i.renderValue(prop.Value, depth)))
	}
	if len(props) > limit {
		parts = append(parts, fmt.Sprintf("…+%d", len(props)-limit))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func (i *wireInspector) renderString(s string) string {
	runes := []rune(s)
	if len(runes) <= i.opts.maxStringRunes {
		return strconv.Quote(s)
	}
	limit := i.opts.maxStringRunes
	if limit > 1 {
		limit--
	}
	clipped := string(runes[:limit]) + "…"
	return fmt.Sprintf("string(%d) %s", len(runes), strconv.Quote(clipped))
}

func renderTypedArraySample(ctor string, raw []byte, limit int) ([]string, int) {
	bytesPer := int(bytesPerElement(ctor))
	if bytesPer <= 0 {
		return []string{formatHexBytes(raw, limit)}, 0
	}
	total := len(raw) / bytesPer
	limit = minInt(total, limit)
	out := make([]string, 0, limit)
	for idx := 0; idx < limit; idx++ {
		start := idx * bytesPer
		end := start + bytesPer
		out = append(out, typedArrayElementString(ctor, raw[start:end]))
	}
	return out, total - limit
}

func typedArrayElementString(ctor string, raw []byte) string {
	switch ctor {
	case "Uint8Array", "Uint8ClampedArray":
		return strconv.FormatUint(uint64(raw[0]), 10)
	case "Int8Array":
		return strconv.FormatInt(int64(int8(raw[0])), 10)
	case "Uint16Array":
		return strconv.FormatUint(uint64(binary.LittleEndian.Uint16(raw)), 10)
	case "Int16Array":
		return strconv.FormatInt(int64(int16(binary.LittleEndian.Uint16(raw))), 10)
	case "Uint32Array":
		return strconv.FormatUint(uint64(binary.LittleEndian.Uint32(raw)), 10)
	case "Int32Array":
		return strconv.FormatInt(int64(int32(binary.LittleEndian.Uint32(raw))), 10)
	case "Float32Array":
		return formatNumber(float64(math.Float32frombits(binary.LittleEndian.Uint32(raw))))
	case "Float64Array":
		return formatNumber(math.Float64frombits(binary.LittleEndian.Uint64(raw)))
	case "BigInt64Array":
		return strconv.FormatInt(int64(binary.LittleEndian.Uint64(raw)), 10) + "n"
	case "BigUint64Array":
		return strconv.FormatUint(binary.LittleEndian.Uint64(raw), 10) + "n"
	default:
		return formatHexBytes(raw, len(raw))
	}
}

func typedArrayLength(node wireNode) int {
	bytesPer := bytesPerElement(node.TextA)
	if bytesPer == 0 {
		return int(node.ByteLength)
	}
	return int(node.ByteLength / bytesPer)
}

func formatHexBytes(raw []byte, max int) string {
	if len(raw) == 0 {
		return "<>"
	}
	limit := minInt(len(raw), max)
	parts := make([]string, 0, limit+1)
	for _, b := range raw[:limit] {
		parts = append(parts, fmt.Sprintf("%02x", b))
	}
	if len(raw) > limit {
		parts = append(parts, fmt.Sprintf("…+%d", len(raw)-limit))
	}
	return "<" + strings.Join(parts, " ") + ">"
}

func formatNumber(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	case v == 0 && math.Signbit(v):
		return "-0"
	case math.Trunc(v) == v:
		return strconv.FormatFloat(v, 'f', 0, 64)
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}

func formatPropKey(key string) string {
	if key == "" {
		return strconv.Quote(key)
	}
	for idx, r := range key {
		if idx == 0 {
			if r != '$' && r != '_' && !unicode.IsLetter(r) {
				return strconv.Quote(key)
			}
			continue
		}
		if r != '$' && r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return strconv.Quote(key)
		}
	}
	return key
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

package jswire

import "time"

// ObjectType represents a JavaScript ordinary object with string properties.
// It is not a JavaScript Map; use MapType for that.
type ObjectType map[string]any

// ArrayType represents a JavaScript Array whose elements are supported typed
// Go model values.
type ArrayType []any

// MapEntry is one JavaScript Map entry: [key, value].
type MapEntry [2]any

// MapType represents a real JavaScript Map. Entry order is preserved.
type MapType []MapEntry

// SetType represents a real JavaScript Set. Value order is preserved.
type SetType []any

// DateType represents a JavaScript Date. It uses JavaScript millisecond
// precision when encoded; decoded values are UTC times.
type DateType time.Time

// BigIntType is the decimal text of a JavaScript BigInt, without a trailing n.
type BigIntType string

// RegExpType is a JavaScript RegExp as [source, flags].
type RegExpType [2]string

// ArrayBufferType represents raw bytes of a JavaScript ArrayBuffer.
type ArrayBufferType []byte

// Uint8ArrayType represents a JavaScript Uint8Array.
type Uint8ArrayType []byte

// Uint8ClampedArrayType represents a JavaScript Uint8ClampedArray.
type Uint8ClampedArrayType []byte

// Int8ArrayType represents a JavaScript Int8Array.
type Int8ArrayType []int8

// Uint16ArrayType represents a JavaScript Uint16Array.
type Uint16ArrayType []uint16

// Int16ArrayType represents a JavaScript Int16Array.
type Int16ArrayType []int16

// Uint32ArrayType represents a JavaScript Uint32Array.
type Uint32ArrayType []uint32

// Int32ArrayType represents a JavaScript Int32Array.
type Int32ArrayType []int32

// BigUint64ArrayType represents a JavaScript BigUint64Array.
type BigUint64ArrayType []uint64

// BigInt64ArrayType represents a JavaScript BigInt64Array.
type BigInt64ArrayType []int64

// Float32ArrayType represents a JavaScript Float32Array.
type Float32ArrayType []float32

// Float64ArrayType represents a JavaScript Float64Array.
type Float64ArrayType []float64

// Patch is a sequence of jswire patch operations. It is JSON Patch-inspired but
// not RFC JSON Patch; values use jswire's typed Go model, not JSON-only values.
type Patch []PatchOp

// PatchOp is one jswire patch operation.
type PatchOp struct {
	Op    PatchOpKind
	Path  Path
	Value any
}

// PatchOpKind identifies a jswire patch operation.
type PatchOpKind string

const (
	// PatchSet adds or replaces an object property, or replaces the root for an
	// empty path.
	PatchSet PatchOpKind = "set"

	// PatchRemove removes an object property. Removing a missing property is a
	// no-op.
	PatchRemove PatchOpKind = "remove"
)

// Path is a jswire patch path. An empty path refers to the root. Non-empty
// paths currently address object properties only.
type Path []PathSegment

// PathSegment is one jswire patch path segment. Index is reserved for future
// array support and is currently rejected.
type PathSegment struct {
	Key   string
	Index *int
}

// MustEncode is like Encode but panics on error.
func MustEncode(v any) Value {
	raw, err := Encode(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// Decode decodes v using the typed Go model.
func (v Value) Decode() (any, error) {
	return Decode(v)
}

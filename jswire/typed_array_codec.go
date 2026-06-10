package jswire

import (
	"encoding/binary"
	"math"
)

func typedArrayModelBytes(v any) (ctor string, raw []byte, ok bool) {
	switch x := v.(type) {
	case Uint8ArrayType:
		return "Uint8Array", append([]byte(nil), x...), true
	case Uint8ClampedArrayType:
		return "Uint8ClampedArray", append([]byte(nil), x...), true
	case Int8ArrayType:
		raw := make([]byte, len(x))
		for i, n := range x {
			raw[i] = byte(n)
		}
		return "Int8Array", raw, true
	case Uint16ArrayType:
		raw := make([]byte, len(x)*2)
		for i, n := range x {
			binary.LittleEndian.PutUint16(raw[i*2:], n)
		}
		return "Uint16Array", raw, true
	case Int16ArrayType:
		raw := make([]byte, len(x)*2)
		for i, n := range x {
			binary.LittleEndian.PutUint16(raw[i*2:], uint16(n))
		}
		return "Int16Array", raw, true
	case Uint32ArrayType:
		raw := make([]byte, len(x)*4)
		for i, n := range x {
			binary.LittleEndian.PutUint32(raw[i*4:], n)
		}
		return "Uint32Array", raw, true
	case Int32ArrayType:
		raw := make([]byte, len(x)*4)
		for i, n := range x {
			binary.LittleEndian.PutUint32(raw[i*4:], uint32(n))
		}
		return "Int32Array", raw, true
	case BigUint64ArrayType:
		raw := make([]byte, len(x)*8)
		for i, n := range x {
			binary.LittleEndian.PutUint64(raw[i*8:], n)
		}
		return "BigUint64Array", raw, true
	case BigInt64ArrayType:
		raw := make([]byte, len(x)*8)
		for i, n := range x {
			binary.LittleEndian.PutUint64(raw[i*8:], uint64(n))
		}
		return "BigInt64Array", raw, true
	case Float32ArrayType:
		raw := make([]byte, len(x)*4)
		for i, n := range x {
			binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(n))
		}
		return "Float32Array", raw, true
	case Float64ArrayType:
		raw := make([]byte, len(x)*8)
		for i, n := range x {
			binary.LittleEndian.PutUint64(raw[i*8:], math.Float64bits(n))
		}
		return "Float64Array", raw, true
	default:
		return "", nil, false
	}
}

func typedArrayModelFromBytes(ctor string, raw []byte) (any, error) {
	bytesPer := bytesPerElement(ctor)
	if bytesPer == 0 {
		return nil, invalidWiref("unsupported typed array constructor %q", ctor)
	}
	if len(raw)%int(bytesPer) != 0 {
		return nil, invalidWiref("invalid %s byte length", ctor)
	}
	switch ctor {
	case "Uint8Array":
		return Uint8ArrayType(append([]byte(nil), raw...)), nil
	case "Uint8ClampedArray":
		return Uint8ClampedArrayType(append([]byte(nil), raw...)), nil
	case "Int8Array":
		out := make(Int8ArrayType, len(raw))
		for i, b := range raw {
			out[i] = int8(b)
		}
		return out, nil
	case "Uint16Array":
		out := make(Uint16ArrayType, len(raw)/2)
		for i := range out {
			out[i] = binary.LittleEndian.Uint16(raw[i*2:])
		}
		return out, nil
	case "Int16Array":
		out := make(Int16ArrayType, len(raw)/2)
		for i := range out {
			out[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
		}
		return out, nil
	case "Uint32Array":
		out := make(Uint32ArrayType, len(raw)/4)
		for i := range out {
			out[i] = binary.LittleEndian.Uint32(raw[i*4:])
		}
		return out, nil
	case "Int32Array":
		out := make(Int32ArrayType, len(raw)/4)
		for i := range out {
			out[i] = int32(binary.LittleEndian.Uint32(raw[i*4:]))
		}
		return out, nil
	case "BigUint64Array":
		out := make(BigUint64ArrayType, len(raw)/8)
		for i := range out {
			out[i] = binary.LittleEndian.Uint64(raw[i*8:])
		}
		return out, nil
	case "BigInt64Array":
		out := make(BigInt64ArrayType, len(raw)/8)
		for i := range out {
			out[i] = int64(binary.LittleEndian.Uint64(raw[i*8:]))
		}
		return out, nil
	case "Float32Array":
		out := make(Float32ArrayType, len(raw)/4)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
		}
		return out, nil
	case "Float64Array":
		out := make(Float64ArrayType, len(raw)/8)
		for i := range out {
			out[i] = math.Float64frombits(binary.LittleEndian.Uint64(raw[i*8:]))
		}
		return out, nil
	case "DataView":
		return nil, invalidWiref("DataView has no typed Go model")
	default:
		return nil, invalidWiref("unsupported typed array constructor %q", ctor)
	}
}

func typedArrayWindowBytes(node wireNode, buffer wireNode) ([]byte, error) {
	start := int(node.ByteOffset)
	end := start + int(node.ByteLength)
	if start < 0 || end < start || end > len(buffer.Bytes) {
		return nil, invalidWiref("typed array window out of bounds")
	}
	return append([]byte(nil), buffer.Bytes[start:end]...), nil
}

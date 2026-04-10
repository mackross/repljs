package jswire

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"time"
	"unsafe"

	"github.com/dop251/goja"
	"github.com/fastschema/qjs"
	"github.com/mus-format/mus-go/ord"
	"github.com/mus-format/mus-go/varint"
)

const wireVersion byte = 1

var (
	ErrUnsupportedVersion = errors.New("bridge: unsupported version")
	ErrInvalidWire        = errors.New("bridge: invalid wire payload")
	gojaKeepAliveSymbol   = goja.NewSymbol("jswire.keepalive")
)

type valueKind byte
type nodeKind byte
type numberKind byte

const (
	valueUndefined valueKind = iota
	valueNull
	valueBool
	valueNumber
	valueString
	valueBigInt
	valueRef
)

const (
	numberFinite numberKind = iota
	numberNaN
	numberPosInf
	numberNegInf
	numberNegZero
)

const (
	nodeObject nodeKind = iota
	nodeArray
	nodeMap
	nodeSet
	nodeDate
	nodeRegexp
	nodeArrayBuffer
	nodeTypedArray
	nodeError
)

type wireGraph struct {
	Root  wireValue
	Nodes []wireNode
}

type wireValue struct {
	Kind   valueKind
	Bool   bool
	Number float64
	Text   string
	Ref    uint32
}

type wireNode struct {
	ID         uint32
	Kind       nodeKind
	Props      []wireProp
	Slots      []wireSlot
	Entries    []wireEntry
	Values     []wireValue
	DateValid  bool
	DateMS     int64
	TextA      string
	TextB      string
	Bytes      []byte
	Buffer     wireValue
	ByteOffset uint32
	ByteLength uint32
	Cause      wireValue
}

type wireProp struct {
	Key   string
	Value wireValue
}

type wireSlot struct {
	Present bool
	Value   wireValue
}

type wireEntry struct {
	Key   wireValue
	Value wireValue
}

func EncodeQuickJS(v *qjs.Value) ([]byte, error) {
	if v == nil {
		return marshalGraph(wireGraph{Root: wireValue{Kind: valueUndefined}}), nil
	}
	enc := quickEncoder{
		seen:   make(map[uint64]uint32),
		nextID: 1,
	}
	root, err := enc.encodeValue(v)
	if err != nil {
		return nil, err
	}
	return marshalGraph(wireGraph{Root: root, Nodes: enc.nodes}), nil
}

func DecodeQuickJS(ctx *qjs.Context, bs []byte) (*qjs.Value, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil quickjs context", ErrInvalidWire)
	}
	graph, err := unmarshalGraph(bs)
	if err != nil {
		return nil, err
	}
	dec := quickDecoder{
		ctx:  ctx,
		refs: make(map[uint32]*qjs.Value, len(graph.Nodes)),
	}
	if err := dec.allocate(graph.Nodes); err != nil {
		dec.cleanupAll()
		return nil, err
	}
	if err := dec.fill(graph.Nodes); err != nil {
		dec.cleanupAll()
		return nil, err
	}
	return dec.decodeRoot(graph.Root)
}

func EncodeGoja(v goja.Value) ([]byte, error) {
	enc := gojaEncoder{
		seen:   make(map[uintptr]uint32),
		nextID: 1,
	}
	root, err := enc.encodeValue(v)
	if err != nil {
		return nil, err
	}
	return marshalGraph(wireGraph{Root: root, Nodes: enc.nodes}), nil
}

func DecodeGoja(vm *goja.Runtime, bs []byte) (goja.Value, error) {
	if vm == nil {
		return nil, fmt.Errorf("%w: nil goja runtime", ErrInvalidWire)
	}
	graph, err := unmarshalGraph(bs)
	if err != nil {
		return nil, err
	}
	dec := gojaDecoder{
		vm:   vm,
		refs: make(map[uint32]goja.Value, len(graph.Nodes)),
	}
	if err := dec.allocate(graph.Nodes); err != nil {
		return nil, err
	}
	if err := dec.fill(graph.Nodes); err != nil {
		return nil, err
	}
	return dec.decodeValue(graph.Root)
}

type quickEncoder struct {
	seen   map[uint64]uint32
	nextID uint32
	nodes  []wireNode
}

func (e *quickEncoder) encodeValue(v *qjs.Value) (wireValue, error) {
	if v == nil || v.IsUndefined() {
		return wireValue{Kind: valueUndefined}, nil
	}
	if v.IsNull() {
		return wireValue{Kind: valueNull}, nil
	}
	if v.IsBool() {
		return wireValue{Kind: valueBool, Bool: v.Bool()}, nil
	}
	if v.IsString() {
		return wireValue{Kind: valueString, Text: v.String()}, nil
	}
	if v.IsBigInt() {
		return wireValue{Kind: valueBigInt, Text: v.BigInt().String()}, nil
	}
	if v.IsSymbol() {
		return wireValue{}, fmt.Errorf("bridge: unsupported quickjs symbol")
	}
	if v.IsFunction() || v.IsPromise() {
		return wireValue{}, fmt.Errorf("bridge: unsupported quickjs %s", v.Type())
	}
	if v.IsNumber() {
		return wireValue{Kind: valueNumber, Number: v.Float64()}, nil
	}
	if !v.IsObject() {
		return wireValue{}, fmt.Errorf("bridge: unsupported quickjs type %s", v.Type())
	}

	key := v.Raw()
	if id, ok := e.seen[key]; ok {
		return wireValue{Kind: valueRef, Ref: id}, nil
	}
	id := e.nextID
	e.nextID++
	e.seen[key] = id

	node := wireNode{ID: id}
	e.nodes = append(e.nodes, node)
	idx := len(e.nodes) - 1

	kind := v.IntrinsicKind()

	switch {
	case kind == qjs.IntrinsicArray:
		node.Kind = nodeArray
		arr, err := v.ToArray()
		if err != nil {
			return wireValue{}, fmt.Errorf("bridge: quickjs array: %w", err)
		}
		node.Slots = make([]wireSlot, v.Len())
		for i := int64(0); i < v.Len(); i++ {
			if !arr.HasIndex(i) {
				continue
			}
			item := arr.Get(i)
			val, err := e.encodeValue(item)
			item.Free()
			if err != nil {
				return wireValue{}, err
			}
			node.Slots[i] = wireSlot{Present: true, Value: val}
		}
	case kind == qjs.IntrinsicMap:
		node.Kind = nodeMap
		var iterErr error
		v.ToMap().ForEach(func(key, value *qjs.Value) {
			if iterErr != nil {
				return
			}
			k, kerr := e.encodeValue(key)
			if kerr != nil {
				iterErr = kerr
				return
			}
			val, verr := e.encodeValue(value)
			if verr != nil {
				iterErr = verr
				return
			}
			node.Entries = append(node.Entries, wireEntry{Key: k, Value: val})
		})
		if iterErr != nil {
			return wireValue{}, iterErr
		}
	case kind == qjs.IntrinsicSet:
		node.Kind = nodeSet
		var iterErr error
		v.ToSet().ForEach(func(value *qjs.Value) {
			if iterErr != nil {
				return
			}
			val, err := e.encodeValue(value)
			if err != nil {
				iterErr = err
				return
			}
			node.Values = append(node.Values, val)
		})
		if iterErr != nil {
			return wireValue{}, iterErr
		}
	case kind == qjs.IntrinsicDate:
		node.Kind = nodeDate
		ms, err := v.InvokeJS("getTime")
		if err != nil {
			return wireValue{}, err
		}
		defer ms.Free()
		if !ms.IsNaN() {
			node.DateValid = true
			node.DateMS = int64(ms.Float64())
		}
	case kind == qjs.IntrinsicRegExp:
		node.Kind = nodeRegexp
		source := v.GetPropertyStr("source")
		flags := v.GetPropertyStr("flags")
		node.TextA = source.String()
		node.TextB = flags.String()
		source.Free()
		flags.Free()
	case kind == qjs.IntrinsicArrayBuffer:
		node.Kind = nodeArrayBuffer
		node.Bytes = mustQuickJSArrayBufferBytes(v)
	case kind.IsTypedArray():
		node.Kind = nodeTypedArray
		node.TextA = kind.String()
		node.ByteOffset = uint32(v.GetPropertyStr("byteOffset").Int64())
		node.ByteLength = uint32(v.GetPropertyStr("byteLength").Int64())
		buffer := v.GetPropertyStr("buffer")
		bufferVal, err := e.encodeValue(buffer)
		buffer.Free()
		if err != nil {
			return wireValue{}, err
		}
		node.Buffer = bufferVal
	case v.IsError():
		node.Kind = nodeError
		name := v.GetPropertyStr("name")
		msg := v.GetPropertyStr("message")
		node.TextA = name.String()
		node.TextB = msg.String()
		name.Free()
		msg.Free()
		if v.HasProperty("cause") {
			cause := v.GetPropertyStr("cause")
			causeVal, err := e.encodeValue(cause)
			cause.Free()
			if err != nil {
				return wireValue{}, err
			}
			node.Cause = causeVal
		} else {
			node.Cause = wireValue{Kind: valueUndefined}
		}
	default:
		node.Kind = nodeObject
	}

	if err := e.appendQuickEnumerableProps(v, &node); err != nil {
		return wireValue{}, err
	}

	e.nodes[idx] = node
	return wireValue{Kind: valueRef, Ref: id}, nil
}

type gojaEncoder struct {
	seen   map[uintptr]uint32
	nextID uint32
	nodes  []wireNode
}

func (e *gojaEncoder) encodeValue(v goja.Value) (wireValue, error) {
	if v == nil || goja.IsUndefined(v) {
		return wireValue{Kind: valueUndefined}, nil
	}
	if goja.IsNull(v) {
		return wireValue{Kind: valueNull}, nil
	}
	if _, ok := v.(*goja.Symbol); ok {
		return wireValue{}, fmt.Errorf("bridge: unsupported goja Symbol")
	}

	switch exp := v.Export().(type) {
	case bool:
		return wireValue{Kind: valueBool, Bool: exp}, nil
	case string:
		return wireValue{Kind: valueString, Text: exp}, nil
	case int64:
		return wireValue{Kind: valueNumber, Number: float64(exp)}, nil
	case float64:
		return wireValue{Kind: valueNumber, Number: exp}, nil
	case *big.Int:
		return wireValue{Kind: valueBigInt, Text: exp.String()}, nil
	case big.Int:
		return wireValue{Kind: valueBigInt, Text: exp.String()}, nil
	}
	obj, ok := v.(*goja.Object)
	if !ok {
		obj = v.ToObject(nil)
	}
	key := uintptr(unsafe.Pointer(obj))
	if id, ok := e.seen[key]; ok {
		return wireValue{Kind: valueRef, Ref: id}, nil
	}
	id := e.nextID
	e.nextID++
	e.seen[key] = id

	node := wireNode{ID: id}
	e.nodes = append(e.nodes, node)
	idx := len(e.nodes) - 1

	ctorName := gojaConstructorName(obj)
	className := obj.ClassName()
	implName := gojaObjectImplName(obj)
	switch {
	case className == "Function":
		return wireValue{}, fmt.Errorf("bridge: unsupported goja Function")
	case gojaIsPromiseObject(obj, implName):
		return wireValue{}, fmt.Errorf("bridge: unsupported goja Promise")
	case className == "Array":
		node.Kind = nodeArray
		length := int(obj.Get("length").ToInteger())
		node.Slots = make([]wireSlot, length)
		for i := 0; i < length; i++ {
			prop := strconv.Itoa(i)
			if !containsString(obj.GetOwnPropertyNames(), prop) {
				continue
			}
			val, err := e.encodeValue(obj.Get(prop))
			if err != nil {
				return wireValue{}, err
			}
			node.Slots[i] = wireSlot{Present: true, Value: val}
		}
	case implName == "mapObject":
		node.Kind = nodeMap
		entries, err := gojaMapEntries(obj)
		if err != nil {
			return wireValue{}, err
		}
		for _, entry := range entries {
			k, err := e.encodeValue(entry[0])
			if err != nil {
				return wireValue{}, err
			}
			val, err := e.encodeValue(entry[1])
			if err != nil {
				return wireValue{}, err
			}
			node.Entries = append(node.Entries, wireEntry{Key: k, Value: val})
		}
	case implName == "setObject":
		node.Kind = nodeSet
		values, err := gojaSetValues(obj)
		if err != nil {
			return wireValue{}, err
		}
		for _, item := range values {
			val, err := e.encodeValue(item)
			if err != nil {
				return wireValue{}, err
			}
			node.Values = append(node.Values, val)
		}
	case implName == "dateObject" || className == "Date":
		node.Kind = nodeDate
		switch dt := obj.Export().(type) {
		case time.Time:
			node.DateValid = true
			node.DateMS = dt.UnixMilli()
		case *time.Time:
			if dt != nil {
				node.DateValid = true
				node.DateMS = dt.UnixMilli()
			}
		}
	case implName == "regexpObject" || className == "RegExp":
		node.Kind = nodeRegexp
		node.TextA = obj.Get("source").String()
		node.TextB = obj.Get("flags").String()
	case implName == "arrayBufferObject":
		node.Kind = nodeArrayBuffer
		switch ab := obj.Export().(type) {
		case goja.ArrayBuffer:
			node.Bytes = append([]byte(nil), ab.Bytes()...)
		default:
			return wireValue{}, fmt.Errorf("bridge: unexpected goja ArrayBuffer export %T", ab)
		}
	case implName == "typedArrayObject" || implName == "dataViewObject":
		node.Kind = nodeTypedArray
		node.TextA = ctorName
		node.ByteOffset = uint32(obj.Get("byteOffset").ToInteger())
		node.ByteLength = uint32(obj.Get("byteLength").ToInteger())
		bufferValue := obj.Get("buffer")
		buffer, err := e.encodeValue(bufferValue)
		if err != nil {
			return wireValue{}, err
		}
		node.Buffer = buffer
	case implName == "errorObject" || className == "Error":
		node.Kind = nodeError
		node.TextA = obj.Get("name").String()
		node.TextB = obj.Get("message").String()
		cause, err := e.encodeValue(obj.Get("cause"))
		if err != nil {
			return wireValue{}, err
		}
		node.Cause = cause
	default:
		node.Kind = nodeObject
	}

	if err := e.appendGojaEnumerableProps(obj, &node); err != nil {
		return wireValue{}, err
	}

	e.nodes[idx] = node
	return wireValue{Kind: valueRef, Ref: id}, nil
}

type quickDecoder struct {
	ctx  *qjs.Context
	refs map[uint32]*qjs.Value
}

func (d *quickDecoder) cleanupAll() {
	for id, ref := range d.refs {
		if ref == nil {
			continue
		}
		ref.Free()
		d.refs[id] = ref
	}
}

func (d *quickDecoder) cleanupExcept(keep uint32) {
	for id, ref := range d.refs {
		if id == keep || ref == nil {
			continue
		}
		ref.Free()
		d.refs[id] = ref
	}
}

func (d *quickDecoder) decodeRoot(root wireValue) (*qjs.Value, error) {
	value, err := d.decodeValue(root)
	if err != nil {
		d.cleanupAll()
		return nil, err
	}
	if root.Kind == valueRef {
		d.cleanupExcept(root.Ref)
	} else {
		d.cleanupAll()
	}
	return value, nil
}

func (d *quickDecoder) allocate(nodes []wireNode) error {
	for _, node := range nodes {
		if node.Kind == nodeTypedArray {
			continue
		}
		var v *qjs.Value
		switch node.Kind {
		case nodeObject:
			v = d.ctx.NewObject()
		case nodeArray:
			v = d.ctx.NewArray().Value
		case nodeMap:
			v = d.ctx.NewMap().Value
		case nodeSet:
			v = d.ctx.NewSet().Value
		case nodeDate:
			if node.DateValid {
				tm := time.UnixMilli(node.DateMS).UTC()
				v = d.ctx.NewDate(&tm)
			} else {
				var err error
				v, err = d.ctx.Eval("date.js", qjs.Code("new Date(NaN)"))
				if err != nil {
					return err
				}
			}
		case nodeRegexp:
			var err error
			v, err = d.ctx.Eval("regexp.js", qjs.Code("new RegExp("+strconv.Quote(node.TextA)+","+strconv.Quote(node.TextB)+")"))
			if err != nil {
				return err
			}
		case nodeArrayBuffer:
			v = d.ctx.NewArrayBuffer(append([]byte(nil), node.Bytes...))
		case nodeError:
			if ctorName := quickBuiltinErrorConstructorName(node.TextA); ctorName != "" {
				global := d.ctx.Global()
				ctor := global.GetPropertyStr(ctorName)
				msg := d.ctx.NewString(node.TextB)
				v = ctor.CallConstructor(msg)
				msg.Free()
				ctor.Free()
				global.Free()
			} else {
				v = d.ctx.NewError(errors.New(node.TextB))
			}
			if !v.IsError() {
				return fmt.Errorf("%w: quickjs %s constructor failed", ErrInvalidWire, node.TextA)
			}
		default:
			return fmt.Errorf("%w: unknown node kind %d", ErrInvalidWire, node.Kind)
		}
		d.refs[node.ID] = v
	}
	for _, node := range nodes {
		if node.Kind != nodeTypedArray {
			continue
		}
		bufferValue, err := d.decodeValue(node.Buffer)
		if err != nil {
			return err
		}
		if !bufferValue.IsByteArray() {
			return fmt.Errorf("%w: quickjs typed array buffer is not ArrayBuffer", ErrInvalidWire)
		}
		bytesPer := bytesPerElement(node.TextA)
		if bytesPer == 0 || node.ByteLength%bytesPer != 0 {
			return fmt.Errorf("%w: invalid quickjs typed array length", ErrInvalidWire)
		}
		ctor := d.ctx.Global().GetPropertyStr(node.TextA)
		offset := d.ctx.NewInt64(int64(node.ByteOffset))
		lengthElems := d.ctx.NewInt64(int64(node.ByteLength / bytesPer))
		v := ctor.CallConstructor(bufferValue, offset, lengthElems)
		offset.Free()
		lengthElems.Free()
		ctor.Free()
		if v.IsError() {
			return v.Exception()
		}
		d.refs[node.ID] = v
	}
	return nil
}

func (d *quickDecoder) fill(nodes []wireNode) error {
	for _, node := range nodes {
		v := d.refs[node.ID]
		switch node.Kind {
		case nodeObject:
		case nodeArray:
			for i, slot := range node.Slots {
				if !slot.Present {
					continue
				}
				val, err := d.decodeValue(slot.Value)
				if err != nil {
					return err
				}
				v.SetPropertyIndex(int64(i), val.Clone())
			}
			ln := d.ctx.NewInt64(int64(len(node.Slots)))
			v.SetPropertyStr("length", ln)
		case nodeMap:
			m := v.ToMap()
			for _, entry := range node.Entries {
				key, err := d.decodeValue(entry.Key)
				if err != nil {
					return err
				}
				val, err := d.decodeValue(entry.Value)
				if err != nil {
					return err
				}
				m.Set(key.Clone(), val.Clone())
			}
		case nodeSet:
			s := v.ToSet()
			for _, item := range node.Values {
				val, err := d.decodeValue(item)
				if err != nil {
					return err
				}
				s.Add(val.Clone())
			}
		case nodeError:
			cause, err := d.decodeValue(node.Cause)
			if err != nil {
				return err
			}
			v.SetPropertyStr("cause", cause.Clone())
			if node.TextA != "" {
				name := d.ctx.NewString(node.TextA)
				v.SetPropertyStr("name", name)
			}
		}
		for _, prop := range node.Props {
			val, err := d.decodeValue(prop.Value)
			if err != nil {
				return err
			}
			v.SetPropertyStr(prop.Key, val.Clone())
		}
	}
	return nil
}

func (d *quickDecoder) decodeValue(v wireValue) (*qjs.Value, error) {
	switch v.Kind {
	case valueUndefined:
		return d.ctx.NewUndefined(), nil
	case valueNull:
		return d.ctx.NewNull(), nil
	case valueBool:
		return d.ctx.NewBool(v.Bool), nil
	case valueNumber:
		return d.ctx.NewFloat64(v.Number), nil
	case valueString:
		return d.ctx.NewString(v.Text), nil
	case valueBigInt:
		return d.ctx.Eval("bigint.js", qjs.Code("BigInt("+strconv.Quote(v.Text)+")"))
	case valueRef:
		ref, ok := d.refs[v.Ref]
		if !ok {
			return nil, fmt.Errorf("%w: missing quickjs ref %d", ErrInvalidWire, v.Ref)
		}
		return ref, nil
	default:
		return nil, fmt.Errorf("%w: unknown value kind %d", ErrInvalidWire, v.Kind)
	}
}

type gojaDecoder struct {
	vm   *goja.Runtime
	refs map[uint32]goja.Value
}

func (d *gojaDecoder) allocate(nodes []wireNode) error {
	for _, node := range nodes {
		var v goja.Value
		if node.Kind == nodeTypedArray {
			continue
		}
		switch node.Kind {
		case nodeObject:
			v = d.vm.NewObject()
		case nodeArray:
			v = d.vm.NewArray()
		case nodeMap:
			mapCtor := d.vm.Get("Map")
			obj, err := d.vm.New(mapCtor)
			if err != nil {
				return err
			}
			v = obj
		case nodeSet:
			setCtor := d.vm.Get("Set")
			obj, err := d.vm.New(setCtor)
			if err != nil {
				return err
			}
			v = obj
		case nodeDate:
			dateCtor := d.vm.Get("Date")
			arg := goja.NaN()
			if node.DateValid {
				arg = d.vm.ToValue(node.DateMS)
			}
			obj, err := d.vm.New(dateCtor, arg)
			if err != nil {
				return err
			}
			v = obj
		case nodeRegexp:
			reCtor := d.vm.Get("RegExp")
			obj, err := d.vm.New(reCtor, d.vm.ToValue(node.TextA), d.vm.ToValue(node.TextB))
			if err != nil {
				return err
			}
			v = obj
		case nodeArrayBuffer:
			var err error
			v, err = d.newArrayBufferValue(node.Bytes)
			if err != nil {
				return err
			}
		case nodeError:
			ctorName := node.TextA
			if ctorName == "" {
				ctorName = "Error"
			}
			ctor := d.vm.Get(ctorName)
			if ctor == nil || goja.IsUndefined(ctor) || goja.IsNull(ctor) {
				ctor = d.vm.Get("Error")
			}
			obj, err := d.vm.New(ctor, d.vm.ToValue(node.TextB))
			if err != nil && ctorName != "Error" {
				obj, err = d.vm.New(d.vm.Get("Error"), d.vm.ToValue(node.TextB))
			}
			if err != nil {
				return err
			}
			v = obj
		default:
			return fmt.Errorf("%w: unknown node kind %d", ErrInvalidWire, node.Kind)
		}
		d.refs[node.ID] = v
	}
	for _, node := range nodes {
		if node.Kind != nodeTypedArray {
			continue
		}
		bufferValue, err := d.decodeValue(node.Buffer)
		if err != nil {
			return err
		}
		bufferObject, ok := bufferValue.(*goja.Object)
		if !ok {
			return fmt.Errorf("%w: goja typed array buffer is not object", ErrInvalidWire)
		}
		if _, ok := bufferObject.Export().(goja.ArrayBuffer); !ok {
			return fmt.Errorf("%w: goja typed array buffer is not ArrayBuffer", ErrInvalidWire)
		}
		bytesPer := bytesPerElement(node.TextA)
		if bytesPer == 0 || node.ByteLength%bytesPer != 0 {
			return fmt.Errorf("%w: invalid goja typed array length", ErrInvalidWire)
		}
		ctor := d.vm.Get(node.TextA)
		obj, err := d.vm.New(
			ctor,
			bufferValue,
			d.vm.ToValue(int64(node.ByteOffset)),
			d.vm.ToValue(int64(node.ByteLength/bytesPer)),
		)
		if err != nil {
			return err
		}
		if err := obj.SetSymbol(gojaKeepAliveSymbol, bufferValue); err != nil {
			return err
		}
		d.refs[node.ID] = obj
	}
	return nil
}

func (d *gojaDecoder) fill(nodes []wireNode) error {
	for _, node := range nodes {
		obj := d.refs[node.ID].ToObject(d.vm)
		switch node.Kind {
		case nodeObject:
		case nodeArray:
			for i, slot := range node.Slots {
				if !slot.Present {
					continue
				}
				val, err := d.decodeValue(slot.Value)
				if err != nil {
					return err
				}
				if err := obj.Set(strconv.Itoa(i), val); err != nil {
					return err
				}
			}
			if err := obj.Set("length", len(node.Slots)); err != nil {
				return err
			}
		case nodeMap:
			setFn, ok := goja.AssertFunction(obj.Get("set"))
			if !ok {
				return fmt.Errorf("%w: goja map.set missing", ErrInvalidWire)
			}
			for _, entry := range node.Entries {
				key, err := d.decodeValue(entry.Key)
				if err != nil {
					return err
				}
				val, err := d.decodeValue(entry.Value)
				if err != nil {
					return err
				}
				if _, err := setFn(obj, key, val); err != nil {
					return err
				}
			}
		case nodeSet:
			addFn, ok := goja.AssertFunction(obj.Get("add"))
			if !ok {
				return fmt.Errorf("%w: goja set.add missing", ErrInvalidWire)
			}
			for _, item := range node.Values {
				val, err := d.decodeValue(item)
				if err != nil {
					return err
				}
				if _, err := addFn(obj, val); err != nil {
					return err
				}
			}
		case nodeError:
			cause, err := d.decodeValue(node.Cause)
			if err != nil {
				return err
			}
			if err := obj.Set("cause", cause); err != nil {
				return err
			}
			if node.TextA != "" {
				if err := obj.Set("name", node.TextA); err != nil {
					return err
				}
			}
		}
		for _, prop := range node.Props {
			val, err := d.decodeValue(prop.Value)
			if err != nil {
				return err
			}
			if err := obj.Set(prop.Key, val); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *gojaDecoder) decodeValue(v wireValue) (goja.Value, error) {
	switch v.Kind {
	case valueUndefined:
		return goja.Undefined(), nil
	case valueNull:
		return goja.Null(), nil
	case valueBool:
		return d.vm.ToValue(v.Bool), nil
	case valueNumber:
		switch numberKindOf(v.Number) {
		case numberNaN:
			return goja.NaN(), nil
		case numberPosInf:
			return goja.PositiveInf(), nil
		case numberNegInf:
			return goja.NegativeInf(), nil
		default:
			return d.vm.ToValue(v.Number), nil
		}
	case valueString:
		return d.vm.ToValue(v.Text), nil
	case valueBigInt:
		fn, ok := goja.AssertFunction(d.vm.Get("BigInt"))
		if !ok {
			return nil, fmt.Errorf("%w: BigInt unavailable", ErrInvalidWire)
		}
		return fn(goja.Undefined(), d.vm.ToValue(v.Text))
	case valueRef:
		ref, ok := d.refs[v.Ref]
		if !ok {
			return nil, fmt.Errorf("%w: missing goja ref %d", ErrInvalidWire, v.Ref)
		}
		return ref, nil
	default:
		return nil, fmt.Errorf("%w: unknown value kind %d", ErrInvalidWire, v.Kind)
	}
}

func (d *gojaDecoder) newArrayBufferValue(bytes []byte) (goja.Value, error) {
	ctor := d.vm.Get("ArrayBuffer")
	buffer, err := d.vm.New(ctor, d.vm.ToValue(len(bytes)))
	if err != nil {
		return nil, err
	}
	if len(bytes) == 0 {
		return buffer, nil
	}
	u8Ctor := d.vm.Get("Uint8Array")
	view, err := d.vm.New(u8Ctor, buffer)
	if err != nil {
		return nil, err
	}
	viewObj := view.ToObject(d.vm)
	for i, b := range bytes {
		if err := viewObj.Set(strconv.Itoa(i), int(b)); err != nil {
			return nil, err
		}
	}
	return buffer, nil
}

func mustQuickJSArrayBufferBytes(v *qjs.Value) []byte {
	return append([]byte(nil), v.ToByteArray()...)
}

func mustQuickJSUint8ViewBytes(v *qjs.Value) []byte {
	byteLength := v.GetPropertyStr("byteLength")
	length := int(byteLength.Int64())
	byteLength.Free()
	out := make([]byte, length)
	for i := 0; i < length; i++ {
		cell := v.GetPropertyIndex(int64(i))
		out[i] = byte(cell.Int64())
		cell.Free()
	}
	return out
}

func bytesPerElement(ctor string) uint32 {
	switch ctor {
	case "Uint8Array", "Uint8ClampedArray", "Int8Array", "DataView":
		return 1
	case "Uint16Array", "Int16Array":
		return 2
	case "Uint32Array", "Int32Array", "Float32Array":
		return 4
	case "Float64Array", "BigInt64Array", "BigUint64Array":
		return 8
	default:
		return 0
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func gojaConstructorName(obj *goja.Object) string {
	if obj == nil {
		return ""
	}
	ctor := obj.Get("constructor")
	if ctor == nil || goja.IsUndefined(ctor) || goja.IsNull(ctor) {
		return ""
	}
	ctorObj, ok := ctor.(*goja.Object)
	if !ok {
		return ""
	}
	name := ctorObj.Get("name")
	if name == nil || goja.IsUndefined(name) || goja.IsNull(name) {
		return ""
	}
	return name.String()
}

func isGojaTypedArrayConstructor(name string) bool {
	switch name {
	case "Uint8Array", "Uint8ClampedArray", "Int8Array", "Uint16Array", "Int16Array", "Uint32Array", "Int32Array", "Float32Array", "Float64Array", "BigInt64Array", "BigUint64Array", "DataView":
		return true
	default:
		return false
	}
}

func gojaMapEntries(obj *goja.Object) ([][2]goja.Value, error) {
	entriesMethod, ok := goja.AssertFunction(obj.Get("entries"))
	if !ok {
		return nil, fmt.Errorf("bridge: map.entries missing")
	}
	iterValue, err := entriesMethod(obj)
	if err != nil {
		return nil, err
	}
	iterObj, ok := iterValue.(*goja.Object)
	if !ok {
		return nil, fmt.Errorf("bridge: map iterator is not object")
	}
	nextFn, ok := goja.AssertFunction(iterObj.Get("next"))
	if !ok {
		return nil, fmt.Errorf("bridge: map iterator.next missing")
	}
	var out [][2]goja.Value
	for {
		stepValue, err := nextFn(iterObj)
		if err != nil {
			return nil, err
		}
		stepObj, ok := stepValue.(*goja.Object)
		if !ok {
			return nil, fmt.Errorf("bridge: map iterator step is not object")
		}
		if stepObj.Get("done").ToBoolean() {
			return out, nil
		}
		valueObj, ok := stepObj.Get("value").(*goja.Object)
		if !ok {
			return nil, fmt.Errorf("bridge: map iterator value is not object")
		}
		out = append(out, [2]goja.Value{valueObj.Get("0"), valueObj.Get("1")})
	}
}

func gojaSetValues(obj *goja.Object) ([]goja.Value, error) {
	valuesMethod, ok := goja.AssertFunction(obj.Get("values"))
	if !ok {
		return nil, fmt.Errorf("bridge: set.values missing")
	}
	iterValue, err := valuesMethod(obj)
	if err != nil {
		return nil, err
	}
	iterObj, ok := iterValue.(*goja.Object)
	if !ok {
		return nil, fmt.Errorf("bridge: set iterator is not object")
	}
	nextFn, ok := goja.AssertFunction(iterObj.Get("next"))
	if !ok {
		return nil, fmt.Errorf("bridge: set iterator.next missing")
	}
	var out []goja.Value
	for {
		stepValue, err := nextFn(iterObj)
		if err != nil {
			return nil, err
		}
		stepObj, ok := stepValue.(*goja.Object)
		if !ok {
			return nil, fmt.Errorf("bridge: set iterator step is not object")
		}
		if stepObj.Get("done").ToBoolean() {
			return out, nil
		}
		out = append(out, stepObj.Get("value"))
	}
}

func quickEnumerableDataKeys(v *qjs.Value) ([]string, error) {
	keys, err := v.EnumerableOwnPropertyNames()
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

func (e *quickEncoder) appendQuickEnumerableProps(v *qjs.Value, node *wireNode) error {
	keys, err := quickEnumerableDataKeys(v)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if shouldSkipBuiltinEnumerableProp(node.Kind, key) {
			continue
		}
		accessor, err := quickHasAccessorProperty(v, key)
		if err != nil {
			return err
		}
		if accessor {
			return fmt.Errorf("bridge: unsupported quickjs accessor property %q", key)
		}
		prop := v.GetPropertyStr(key)
		val, err := e.encodeValue(prop)
		prop.Free()
		if err != nil {
			return err
		}
		node.Props = append(node.Props, wireProp{Key: key, Value: val})
	}
	return nil
}

func (e *gojaEncoder) appendGojaEnumerableProps(obj *goja.Object, node *wireNode) error {
	keys, err := gojaEnumerableDataKeys(obj)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if shouldSkipBuiltinEnumerableProp(node.Kind, key) {
			continue
		}
		accessor, err := gojaHasAccessorProperty(obj, key)
		if err != nil {
			return err
		}
		if accessor {
			return fmt.Errorf("bridge: unsupported goja accessor property %q", key)
		}
		val, err := e.encodeValue(obj.Get(key))
		if err != nil {
			return err
		}
		node.Props = append(node.Props, wireProp{Key: key, Value: val})
	}
	return nil
}

func shouldSkipBuiltinEnumerableProp(kind nodeKind, key string) bool {
	switch kind {
	case nodeObject:
		return false
	case nodeArray, nodeTypedArray:
		return isArrayIndexKey(key)
	case nodeError:
		return key == "name" || key == "message" || key == "cause"
	default:
		return false
	}
}

func isArrayIndexKey(key string) bool {
	if key == "" {
		return false
	}
	n, err := strconv.ParseUint(key, 10, 32)
	if err != nil {
		return false
	}
	return strconv.FormatUint(n, 10) == key
}

func gojaEnumerableDataKeys(obj *goja.Object) ([]string, error) {
	keys := append([]string(nil), obj.Keys()...)
	sort.Strings(keys)
	return keys, nil
}

func quickHasAccessorProperty(v *qjs.Value, key string) (bool, error) {
	desc, err := v.OwnPropertyDescriptor(key)
	if err != nil {
		return false, err
	}
	return desc.IsAccessor(), nil
}

func gojaHasAccessorProperty(obj *goja.Object, key string) (bool, error) {
	prop, ok := gojaBaseObjectValue(obj, key)
	if !ok {
		return false, nil
	}
	return gojaValuePropertyIsAccessor(prop), nil
}

func quickBuiltinErrorConstructorName(name string) string {
	switch name {
	case "AggregateError", "EvalError", "RangeError", "ReferenceError", "SyntaxError", "TypeError", "URIError":
		return name
	default:
		return ""
	}
}

func gojaObjectRuntime(obj *goja.Object) *goja.Runtime {
	if obj == nil {
		return nil
	}
	field := reflect.ValueOf(obj).Elem().FieldByName("runtime")
	if !field.IsValid() {
		return nil
	}
	return *(**goja.Runtime)(unsafe.Pointer(field.UnsafeAddr()))
}

func gojaBaseObjectValue(obj *goja.Object, key string) (reflect.Value, bool) {
	if obj == nil {
		return reflect.Value{}, false
	}
	selfField := reflect.ValueOf(obj).Elem().FieldByName("self")
	if !selfField.IsValid() || selfField.IsNil() {
		return reflect.Value{}, false
	}
	self := reflect.NewAt(selfField.Type(), unsafe.Pointer(selfField.UnsafeAddr())).Elem().Elem()
	base := gojaFindEmbeddedField(self, "baseObject")
	if !base.IsValid() {
		return reflect.Value{}, false
	}
	values := base.FieldByName("values")
	if !values.IsValid() || values.IsNil() {
		return reflect.Value{}, false
	}
	mapKey := reflect.ValueOf(key).Convert(values.Type().Key())
	prop := values.MapIndex(mapKey)
	if !prop.IsValid() {
		return reflect.Value{}, false
	}
	return prop, true
}

func gojaFindEmbeddedField(v reflect.Value, name string) reflect.Value {
	return gojaFindEmbeddedFieldDepth(v, name, 0)
}

func gojaFindEmbeddedFieldDepth(v reflect.Value, name string, depth int) reflect.Value {
	if depth > 4 || !v.IsValid() {
		return reflect.Value{}
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	if v.Type().Name() == name {
		return v
	}
	if field := v.FieldByName(name); field.IsValid() {
		return field
	}
	for i := 0; i < v.NumField(); i++ {
		fieldType := v.Type().Field(i)
		if !fieldType.Anonymous {
			continue
		}
		if result := gojaFindEmbeddedFieldDepth(v.Field(i), name, depth+1); result.IsValid() {
			return result
		}
	}
	return reflect.Value{}
}

func gojaValuePropertyIsAccessor(prop reflect.Value) bool {
	if !prop.IsValid() {
		return false
	}
	if prop.Kind() == reflect.Interface && !prop.IsNil() {
		prop = prop.Elem()
	}
	if prop.Kind() == reflect.Pointer {
		if prop.IsNil() {
			return false
		}
		prop = prop.Elem()
	}
	if !prop.IsValid() || prop.Kind() != reflect.Struct || prop.Type().Name() != "valueProperty" {
		return false
	}
	accessor := prop.FieldByName("accessor")
	return accessor.IsValid() && accessor.Bool()
}

func gojaObjectImplName(obj *goja.Object) string {
	if obj == nil {
		return ""
	}
	field := reflect.ValueOf(obj).Elem().FieldByName("self")
	if !field.IsValid() || field.IsNil() {
		return ""
	}
	impl := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface()
	typ := reflect.TypeOf(impl)
	if typ == nil {
		return ""
	}
	if typ.Kind() == reflect.Ptr {
		return typ.Elem().Name()
	}
	return typ.Name()
}

func gojaIsPromiseObject(obj *goja.Object, implName string) bool {
	if implName != "Promise" {
		return false
	}
	switch obj.Export().(type) {
	case goja.Promise, *goja.Promise:
		return true
	default:
		return false
	}
}

func marshalGraph(g wireGraph) []byte {
	size := 1 + sizeValue(g.Root) + sizeLen(len(g.Nodes))
	for _, node := range g.Nodes {
		size += sizeNode(node)
	}
	buf := make([]byte, size)
	n := 0
	buf[n] = wireVersion
	n++
	n += marshalValue(g.Root, buf[n:])
	n += marshalLen(len(g.Nodes), buf[n:])
	for _, node := range g.Nodes {
		n += marshalNode(node, buf[n:])
	}
	return buf
}

func unmarshalGraph(bs []byte) (wireGraph, error) {
	if len(bs) == 0 {
		return wireGraph{}, ErrInvalidWire
	}
	if bs[0] != wireVersion {
		return wireGraph{}, ErrUnsupportedVersion
	}
	n := 1
	root, m, err := unmarshalValue(bs[n:])
	if err != nil {
		return wireGraph{}, err
	}
	n += m
	count, m, err := unmarshalLen(bs[n:])
	if err != nil {
		return wireGraph{}, err
	}
	n += m
	nodes := make([]wireNode, count)
	for i := 0; i < count; i++ {
		node, m, err := unmarshalNode(bs[n:])
		if err != nil {
			return wireGraph{}, err
		}
		n += m
		nodes[i] = node
	}
	if n != len(bs) {
		return wireGraph{}, fmt.Errorf("%w: trailing bytes", ErrInvalidWire)
	}
	graph := wireGraph{Root: root, Nodes: nodes}
	if err := validateGraph(graph); err != nil {
		return wireGraph{}, err
	}
	return graph, nil
}

func validateGraph(graph wireGraph) error {
	if err := validateGraphNodeIDs(graph.Nodes); err != nil {
		return err
	}
	nodeByID := make(map[uint32]wireNode, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodeByID[node.ID] = node
	}
	if err := validateWireValue(graph.Root, nodeByID); err != nil {
		return err
	}
	for _, node := range graph.Nodes {
		if err := validateWireNode(node, nodeByID); err != nil {
			return err
		}
	}
	return nil
}

func validateGraphNodeIDs(nodes []wireNode) error {
	seen := make(map[uint32]struct{}, len(nodes))
	for _, node := range nodes {
		if node.ID == 0 {
			return fmt.Errorf("%w: zero node id", ErrInvalidWire)
		}
		if _, ok := seen[node.ID]; ok {
			return fmt.Errorf("%w: duplicate node id %d", ErrInvalidWire, node.ID)
		}
		seen[node.ID] = struct{}{}
	}
	return nil
}

func validateWireNode(node wireNode, nodeByID map[uint32]wireNode) error {
	for _, prop := range node.Props {
		if err := validateWireValue(prop.Value, nodeByID); err != nil {
			return err
		}
	}
	switch node.Kind {
	case nodeObject:
	case nodeArray:
		for _, slot := range node.Slots {
			if !slot.Present {
				continue
			}
			if err := validateWireValue(slot.Value, nodeByID); err != nil {
				return err
			}
		}
	case nodeMap:
		for _, entry := range node.Entries {
			if err := validateWireValue(entry.Key, nodeByID); err != nil {
				return err
			}
			if err := validateWireValue(entry.Value, nodeByID); err != nil {
				return err
			}
		}
	case nodeSet:
		for _, item := range node.Values {
			if err := validateWireValue(item, nodeByID); err != nil {
				return err
			}
		}
	case nodeTypedArray:
		if node.Buffer.Kind != valueRef {
			return fmt.Errorf("%w: typed array buffer must be a ref", ErrInvalidWire)
		}
		bufferNode, ok := nodeByID[node.Buffer.Ref]
		if !ok {
			return fmt.Errorf("%w: missing typed array buffer ref %d", ErrInvalidWire, node.Buffer.Ref)
		}
		if bufferNode.Kind != nodeArrayBuffer {
			return fmt.Errorf("%w: typed array buffer ref %d is not an ArrayBuffer", ErrInvalidWire, node.Buffer.Ref)
		}
		bytesPer := bytesPerElement(node.TextA)
		if bytesPer == 0 {
			return fmt.Errorf("%w: unknown typed array constructor %q", ErrInvalidWire, node.TextA)
		}
		if node.ByteLength%bytesPer != 0 {
			return fmt.Errorf("%w: invalid typed array byte length", ErrInvalidWire)
		}
		if uint64(node.ByteOffset)+uint64(node.ByteLength) > uint64(len(bufferNode.Bytes)) {
			return fmt.Errorf("%w: typed array window out of bounds", ErrInvalidWire)
		}
	case nodeError:
		if err := validateWireValue(node.Cause, nodeByID); err != nil {
			return err
		}
	}
	return nil
}

func validateWireValue(v wireValue, nodeByID map[uint32]wireNode) error {
	if v.Kind != valueRef {
		return nil
	}
	if _, ok := nodeByID[v.Ref]; !ok {
		return fmt.Errorf("%w: missing ref %d", ErrInvalidWire, v.Ref)
	}
	return nil
}

func sizeValue(v wireValue) int {
	size := 1
	switch v.Kind {
	case valueBool:
		size += 1
	case valueNumber:
		size += 1
		if numberKindOf(v.Number) == numberFinite {
			size += varint.Float64.Size(v.Number)
		}
	case valueString, valueBigInt:
		size += ord.String.Size(v.Text)
	case valueRef:
		size += sizeU32(v.Ref)
	}
	return size
}

func marshalValue(v wireValue, bs []byte) int {
	n := 0
	n += varint.Byte.Marshal(byte(v.Kind), bs[n:])
	switch v.Kind {
	case valueBool:
		b := byte(0)
		if v.Bool {
			b = 1
		}
		n += varint.Byte.Marshal(b, bs[n:])
	case valueNumber:
		k := numberKindOf(v.Number)
		n += varint.Byte.Marshal(byte(k), bs[n:])
		if k == numberFinite {
			n += varint.Float64.Marshal(v.Number, bs[n:])
		}
	case valueString, valueBigInt:
		n += ord.String.Marshal(v.Text, bs[n:])
	case valueRef:
		n += marshalU32(v.Ref, bs[n:])
	}
	return n
}

func unmarshalValue(bs []byte) (wireValue, int, error) {
	var out wireValue
	n := 0
	kind, m, err := varint.Byte.Unmarshal(bs[n:])
	if err != nil {
		return out, 0, err
	}
	n += m
	out.Kind = valueKind(kind)
	switch out.Kind {
	case valueUndefined, valueNull:
	case valueBool:
		b, m, err := varint.Byte.Unmarshal(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		out.Bool = b != 0
	case valueNumber:
		k, m, err := varint.Byte.Unmarshal(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		switch numberKind(k) {
		case numberFinite:
			f, m, err := varint.Float64.Unmarshal(bs[n:])
			if err != nil {
				return out, 0, err
			}
			n += m
			out.Number = f
		case numberNaN:
			out.Number = math.NaN()
		case numberPosInf:
			out.Number = math.Inf(1)
		case numberNegInf:
			out.Number = math.Inf(-1)
		case numberNegZero:
			out.Number = math.Copysign(0, -1)
		default:
			return out, 0, ErrInvalidWire
		}
	case valueString, valueBigInt:
		s, m, err := ord.String.Unmarshal(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		out.Text = s
	case valueRef:
		ref, m, err := unmarshalU32(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		out.Ref = ref
	default:
		return out, 0, ErrInvalidWire
	}
	return out, n, nil
}

func sizeNode(n wireNode) int {
	size := sizeU32(n.ID) + 1
	switch n.Kind {
	case nodeObject:
	case nodeArray:
		size += sizeLen(len(n.Slots))
		for _, slot := range n.Slots {
			size += 1
			if slot.Present {
				size += sizeValue(slot.Value)
			}
		}
	case nodeMap:
		size += sizeLen(len(n.Entries))
		for _, entry := range n.Entries {
			size += sizeValue(entry.Key) + sizeValue(entry.Value)
		}
	case nodeSet:
		size += sizeLen(len(n.Values))
		for _, item := range n.Values {
			size += sizeValue(item)
		}
	case nodeDate:
		size += 1
		if n.DateValid {
			size += varint.Int64.Size(n.DateMS)
		}
	case nodeRegexp:
		size += ord.String.Size(n.TextA) + ord.String.Size(n.TextB)
	case nodeArrayBuffer:
		size += ord.ByteSlice.Size(n.Bytes)
	case nodeTypedArray:
		size += ord.String.Size(n.TextA) + sizeValue(n.Buffer) + sizeU32(n.ByteOffset) + sizeU32(n.ByteLength)
	case nodeError:
		size += ord.String.Size(n.TextA) + ord.String.Size(n.TextB) + sizeValue(n.Cause)
	}
	size += sizeProps(n.Props)
	return size
}

func marshalNode(node wireNode, bs []byte) int {
	n := 0
	n += marshalU32(node.ID, bs[n:])
	n += varint.Byte.Marshal(byte(node.Kind), bs[n:])
	switch node.Kind {
	case nodeObject:
	case nodeArray:
		n += marshalLen(len(node.Slots), bs[n:])
		for _, slot := range node.Slots {
			present := byte(0)
			if slot.Present {
				present = 1
			}
			n += varint.Byte.Marshal(present, bs[n:])
			if slot.Present {
				n += marshalValue(slot.Value, bs[n:])
			}
		}
	case nodeMap:
		n += marshalLen(len(node.Entries), bs[n:])
		for _, entry := range node.Entries {
			n += marshalValue(entry.Key, bs[n:])
			n += marshalValue(entry.Value, bs[n:])
		}
	case nodeSet:
		n += marshalLen(len(node.Values), bs[n:])
		for _, item := range node.Values {
			n += marshalValue(item, bs[n:])
		}
	case nodeDate:
		valid := byte(0)
		if node.DateValid {
			valid = 1
		}
		n += varint.Byte.Marshal(valid, bs[n:])
		if node.DateValid {
			n += varint.Int64.Marshal(node.DateMS, bs[n:])
		}
	case nodeRegexp:
		n += ord.String.Marshal(node.TextA, bs[n:])
		n += ord.String.Marshal(node.TextB, bs[n:])
	case nodeArrayBuffer:
		n += ord.ByteSlice.Marshal(node.Bytes, bs[n:])
	case nodeTypedArray:
		n += ord.String.Marshal(node.TextA, bs[n:])
		n += marshalValue(node.Buffer, bs[n:])
		n += marshalU32(node.ByteOffset, bs[n:])
		n += marshalU32(node.ByteLength, bs[n:])
	case nodeError:
		n += ord.String.Marshal(node.TextA, bs[n:])
		n += ord.String.Marshal(node.TextB, bs[n:])
		n += marshalValue(node.Cause, bs[n:])
	}
	n += marshalProps(node.Props, bs[n:])
	return n
}

func unmarshalNode(bs []byte) (wireNode, int, error) {
	var out wireNode
	n := 0
	id, m, err := unmarshalU32(bs[n:])
	if err != nil {
		return out, 0, err
	}
	n += m
	out.ID = id
	kind, m, err := varint.Byte.Unmarshal(bs[n:])
	if err != nil {
		return out, 0, err
	}
	n += m
	out.Kind = nodeKind(kind)
	switch out.Kind {
	case nodeObject:
	case nodeArray:
		count, m, err := unmarshalLen(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		out.Slots = make([]wireSlot, count)
		for i := 0; i < count; i++ {
			present, m, err := varint.Byte.Unmarshal(bs[n:])
			if err != nil {
				return out, 0, err
			}
			n += m
			if present == 0 {
				continue
			}
			val, m, err := unmarshalValue(bs[n:])
			if err != nil {
				return out, 0, err
			}
			n += m
			out.Slots[i] = wireSlot{Present: true, Value: val}
		}
	case nodeMap:
		count, m, err := unmarshalLen(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		out.Entries = make([]wireEntry, count)
		for i := 0; i < count; i++ {
			key, m, err := unmarshalValue(bs[n:])
			if err != nil {
				return out, 0, err
			}
			n += m
			val, m, err := unmarshalValue(bs[n:])
			if err != nil {
				return out, 0, err
			}
			n += m
			out.Entries[i] = wireEntry{Key: key, Value: val}
		}
	case nodeSet:
		count, m, err := unmarshalLen(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		out.Values = make([]wireValue, count)
		for i := 0; i < count; i++ {
			val, m, err := unmarshalValue(bs[n:])
			if err != nil {
				return out, 0, err
			}
			n += m
			out.Values[i] = val
		}
	case nodeDate:
		valid, m, err := varint.Byte.Unmarshal(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		out.DateValid = valid != 0
		if out.DateValid {
			ms, m, err := varint.Int64.Unmarshal(bs[n:])
			if err != nil {
				return out, 0, err
			}
			n += m
			out.DateMS = ms
		}
	case nodeRegexp:
		s, m, err := ord.String.Unmarshal(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		f, m, err := ord.String.Unmarshal(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		out.TextA = s
		out.TextB = f
	case nodeArrayBuffer:
		b, m, err := ord.ByteSlice.Unmarshal(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		out.Bytes = b
	case nodeTypedArray:
		ctor, m, err := ord.String.Unmarshal(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		buffer, m, err := unmarshalValue(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		offset, m, err := unmarshalU32(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		length, m, err := unmarshalU32(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		out.TextA = ctor
		out.Buffer = buffer
		out.ByteOffset = offset
		out.ByteLength = length
	case nodeError:
		name, m, err := ord.String.Unmarshal(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		msg, m, err := ord.String.Unmarshal(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		cause, m, err := unmarshalValue(bs[n:])
		if err != nil {
			return out, 0, err
		}
		n += m
		out.TextA = name
		out.TextB = msg
		out.Cause = cause
	default:
		return out, 0, ErrInvalidWire
	}
	props, m, err := unmarshalProps(bs[n:])
	if err != nil {
		return out, 0, err
	}
	n += m
	out.Props = props
	return out, n, nil
}

func sizeProps(props []wireProp) int {
	size := sizeLen(len(props))
	for _, prop := range props {
		size += ord.String.Size(prop.Key) + sizeValue(prop.Value)
	}
	return size
}

func marshalProps(props []wireProp, bs []byte) int {
	n := marshalLen(len(props), bs)
	for _, prop := range props {
		n += ord.String.Marshal(prop.Key, bs[n:])
		n += marshalValue(prop.Value, bs[n:])
	}
	return n
}

func unmarshalProps(bs []byte) ([]wireProp, int, error) {
	n := 0
	count, m, err := unmarshalLen(bs[n:])
	if err != nil {
		return nil, 0, err
	}
	n += m
	if count == 0 {
		return nil, n, nil
	}
	props := make([]wireProp, count)
	for i := 0; i < count; i++ {
		key, m, err := ord.String.Unmarshal(bs[n:])
		if err != nil {
			return nil, 0, err
		}
		n += m
		val, m, err := unmarshalValue(bs[n:])
		if err != nil {
			return nil, 0, err
		}
		n += m
		props[i] = wireProp{Key: key, Value: val}
	}
	return props, n, nil
}

func numberKindOf(f float64) numberKind {
	switch {
	case math.IsNaN(f):
		return numberNaN
	case math.IsInf(f, 1):
		return numberPosInf
	case math.IsInf(f, -1):
		return numberNegInf
	case f == 0 && math.Signbit(f):
		return numberNegZero
	default:
		return numberFinite
	}
}

func sizeLen(v int) int {
	return varint.PositiveInt64.Size(int64(v))
}

func marshalLen(v int, bs []byte) int {
	return varint.PositiveInt64.Marshal(int64(v), bs)
}

func unmarshalLen(bs []byte) (int, int, error) {
	v, n, err := varint.PositiveInt64.Unmarshal(bs)
	return int(v), n, err
}

func sizeU32(v uint32) int {
	return varint.Uint64.Size(uint64(v))
}

func marshalU32(v uint32, bs []byte) int {
	return varint.Uint64.Marshal(uint64(v), bs)
}

func unmarshalU32(bs []byte) (uint32, int, error) {
	v, n, err := varint.Uint64.Unmarshal(bs)
	if err != nil {
		return 0, n, err
	}
	if v > uint64(^uint32(0)) {
		return 0, n, ErrInvalidWire
	}
	return uint32(v), n, nil
}

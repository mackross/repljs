package jswire

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/fastschema/qjs"
)

const objectivePatchUnixMS = 1720000000456

const objectiveNativeSourceJS = `(() => {
	const shared = { marker: "shared" };
	const root = {
		scalar: 1,
		when: new Date(1710000000123),
		rx: /a+b/gi,
		big: 9007199254740993n,
		buf: new Uint8Array([1, 2, 3, 4]).buffer,
		u16: new Uint16Array([1, 256, 65535]),
		nested: { shared },
	};
	root.map = new Map([
		[shared, new Set([shared, "map-set"])],
		["big", 9007199254740993n],
	]);
	root.set = new Set([shared, "set-value", 3n]);
	root.mapAlias = root.map;
	root.setAlias = root.set;
	root.self = root;
	shared.root = root;
	return root;
})()`

const objectiveVerifierSupportJS = `
function checkArray(failures, name, value, ctor, expected) {
	if (!(value instanceof ctor)) {
		failures.push(name + " constructor");
		return;
	}
	if (value.length !== expected.length) {
		failures.push(name + " length " + value.length + " != " + expected.length);
		return;
	}
	if (typeof value.byteLength === "number" && typeof ctor.BYTES_PER_ELEMENT === "number" && value.byteLength !== expected.length * ctor.BYTES_PER_ELEMENT) {
		failures.push(name + " byteLength " + value.byteLength + " != " + (expected.length * ctor.BYTES_PER_ELEMENT));
	}
	for (let i = 0; i < expected.length; i += 1) {
		if (value[i] !== expected[i]) {
			failures.push(name + "[" + i + "] " + String(value[i]) + " != " + String(expected[i]));
		}
	}
}

function checkArrayBuffer(failures, name, value, expected) {
	if (!(value instanceof ArrayBuffer)) {
		failures.push(name + " constructor");
		return;
	}
	checkArray(failures, name + " bytes", new Uint8Array(value), Uint8Array, expected);
}

function hasRegExp(set, source, flags) {
	if (!(set instanceof Set)) return false;
	for (const value of set) {
		if (value instanceof RegExp && value.source === source && value.flags === flags) return true;
	}
	return false;
}

function hasDate(set, ms) {
	if (!(set instanceof Set)) return false;
	for (const value of set) {
		if (value instanceof Date && value.getTime() === ms) return true;
	}
	return false;
}

function hasTypedArray(set, ctor, expected) {
	if (!(set instanceof Set)) return false;
	outer:
	for (const value of set) {
		if (!(value instanceof ctor) || value.length !== expected.length) continue;
		for (let i = 0; i < expected.length; i += 1) {
			if (value[i] !== expected[i]) continue outer;
		}
		return true;
	}
	return false;
}

function checkTypedCapsule(failures, value) {
	if (!value || typeof value !== "object") {
		failures.push("typed capsule object");
		return;
	}
	if (value.label !== "typed-capsule") failures.push("typed label");
	if (!(value.when instanceof Date) || value.when.getTime() !== 1720000000456 || value.when.toISOString() !== "2024-07-03T09:46:40.456Z") failures.push("typed Date");
	if (value.big !== -9007199254740993n || value.big * -1n !== 9007199254740993n) failures.push("typed BigInt");
	if (!(value.rx instanceof RegExp) || value.rx.source !== "^z+$" || value.rx.flags !== "im" || !(new RegExp(value.rx.source, value.rx.flags)).test("ZZZ")) failures.push("typed RegExp");

	checkArrayBuffer(failures, "typed ArrayBuffer", value.buf, [9, 8, 7, 6]);
	checkArray(failures, "typed Uint8Array", value.u8, Uint8Array, [255, 1, 2]);
	checkArray(failures, "typed Uint8ClampedArray", value.u8c, Uint8ClampedArray, [255, 0, 128]);
	checkArray(failures, "typed Int8Array", value.i8, Int8Array, [-128, 0, 127]);
	checkArray(failures, "typed Uint16Array", value.u16, Uint16Array, [1, 256, 65535]);
	checkArray(failures, "typed Int16Array", value.i16, Int16Array, [-32768, 0, 32767]);
	checkArray(failures, "typed Uint32Array", value.u32, Uint32Array, [1, 65536, 4294967295]);
	checkArray(failures, "typed Int32Array", value.i32, Int32Array, [-2147483648, 0, 2147483647]);
	checkArray(failures, "typed BigUint64Array", value.bu64, BigUint64Array, [1n, 18446744073709551615n]);
	checkArray(failures, "typed BigInt64Array", value.bi64, BigInt64Array, [-9223372036854775808n, 0n, 9223372036854775807n]);
	checkArray(failures, "typed Float32Array", value.f32, Float32Array, [1.5, -2.25]);
	checkArray(failures, "typed Float64Array", value.f64, Float64Array, [1.25, -2.5]);

	if (!(value.map instanceof Map) || value.map.size !== 2) {
		failures.push("typed Map");
	} else {
		if (!value.map.has(2n) || !value.map.has("bytes")) failures.push("typed Map has");
		if (!hasRegExp(value.map.get(2n), "z+", "g")) failures.push("typed Map BigInt key / RegExp Set value");
		checkArray(failures, "typed Map bytes", value.map.get("bytes"), Uint8Array, [7, 8, 9]);
	}
	if (!(value.set instanceof Set) || value.set.size !== 3 || !value.set.has(5n) || !hasDate(value.set, 1720000000456) || !hasTypedArray(value.set, Uint16Array, [10, 11])) {
		failures.push("typed Set");
	}
}
`

const objectivePatchedRootVerifierJS = `(function(value) {
	const failures = [];
` + objectiveVerifierSupportJS + `
	if (!value || typeof value !== "object") failures.push("root object");
	if (value.scalar !== "patched") failures.push("patched scalar");
	if (value.self !== value) failures.push("root self cycle");

	const shared = value.nested && value.nested.shared;
	if (!shared || shared.marker !== "shared") failures.push("shared marker");
	if (shared && shared.root !== value) failures.push("shared back edge");

	if (!(value.when instanceof Date) || value.when.getTime() !== 1710000000123 || value.when.toISOString() !== "2024-03-09T16:00:00.123Z") failures.push("root Date");
	if (!(value.rx instanceof RegExp) || value.rx.source !== "a+b" || value.rx.flags !== "gi" || !(new RegExp(value.rx.source, value.rx.flags)).test("aaab")) failures.push("root RegExp");
	if (value.big !== 9007199254740993n || value.big + 1n !== 9007199254740994n) failures.push("root BigInt");
	checkArrayBuffer(failures, "root ArrayBuffer", value.buf, [1, 2, 3, 4]);
	checkArray(failures, "root Uint16Array", value.u16, Uint16Array, [1, 256, 65535]);

	if (!(value.map instanceof Map)) {
		failures.push("root Map constructor");
	} else {
		if (value.mapAlias !== value.map) failures.push("root Map repeated reference");
		if (value.map.size !== 2 || !value.map.has(shared) || !value.map.has("big")) failures.push("root Map size/has");
		const sharedSet = value.map.get(shared);
		if (!(sharedSet instanceof Set) || !sharedSet.has(shared) || !sharedSet.has("map-set")) failures.push("root Map shared Set entry");
		if (value.map.get("big") !== 9007199254740993n) failures.push("root Map BigInt value");
	}
	if (!(value.set instanceof Set) || value.setAlias !== value.set || value.set.size !== 3 || !value.set.has(shared) || !value.set.has("set-value") || !value.set.has(3n)) failures.push("root Set");

	checkTypedCapsule(failures, value.added);
	return failures.join("\n");
})`

const objectiveTypedCapsuleVerifierJS = `(function(value) {
	const failures = [];
` + objectiveVerifierSupportJS + `
	checkTypedCapsule(failures, value);
	return failures.join("\n");
})`

func TestJswireObjectiveEndToEnd_TypedModelPatchAndNativeBridge(t *testing.T) {
	// This is the objective sentinel: it fails red until the public typed model
	// and graph-preserving object merge exist, and it avoids JSON snapshots so JS
	// native behavior is what proves success.
	gojaSource := mustNewGoja(t)
	gojaOriginal := evalGoja(t, gojaSource, objectiveNativeSourceJS)

	wire, err := EncodeGoja(gojaOriginal)
	if err != nil {
		t.Fatalf("EncodeGoja() error = %v", err)
	}

	patched, err := wire.MergeObject(ObjectType{
		"scalar": "patched",
		"added":  objectiveTypedCapsule(),
	})
	if err != nil {
		t.Fatalf("MergeObject() error = %v", err)
	}

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	quickValue, err := DecodeQuickJS(quickRuntime.Context(), patched)
	if err != nil {
		t.Fatalf("DecodeQuickJS(patched) error = %v", err)
	}
	defer quickValue.Free()
	assertObjectiveQuickJS(t, quickRuntime, objectivePatchedRootVerifierJS, quickValue)

	wireBack, err := EncodeQuickJS(quickValue)
	if err != nil {
		t.Fatalf("EncodeQuickJS(patched quick value) error = %v", err)
	}

	gojaFinal := mustNewGoja(t)
	gojaValue, err := DecodeGoja(gojaFinal, wireBack)
	if err != nil {
		t.Fatalf("DecodeGoja(wireBack) error = %v", err)
	}
	assertObjectiveGoja(t, gojaFinal, objectivePatchedRootVerifierJS, gojaValue)

	typedInput := objectiveTypedCapsule()
	typedWire, err := Encode(typedInput)
	if err != nil {
		t.Fatalf("Encode(typedInput) error = %v", err)
	}
	typedDecoded, err := Decode(typedWire)
	if err != nil {
		t.Fatalf("Decode(typedWire) error = %v", err)
	}
	if !reflect.DeepEqual(typedInput, typedDecoded) {
		t.Fatalf("Decode(Encode(typedInput)) mismatch\nwant: %#v\n got: %#v", typedInput, typedDecoded)
	}

	quickTypedRuntime := mustNewQuickJS(t)
	defer quickTypedRuntime.Close()
	quickTypedValue, err := DecodeQuickJS(quickTypedRuntime.Context(), typedWire)
	if err != nil {
		t.Fatalf("DecodeQuickJS(typedWire) error = %v", err)
	}
	defer quickTypedValue.Free()
	assertObjectiveQuickJS(t, quickTypedRuntime, objectiveTypedCapsuleVerifierJS, quickTypedValue)

	gojaTypedRuntime := mustNewGoja(t)
	gojaTypedValue, err := DecodeGoja(gojaTypedRuntime, typedWire)
	if err != nil {
		t.Fatalf("DecodeGoja(typedWire) error = %v", err)
	}
	assertObjectiveGoja(t, gojaTypedRuntime, objectiveTypedCapsuleVerifierJS, gojaTypedValue)
}

func TestJswireObjectivePatchPreservesArrayBufferViewAliases(t *testing.T) {
	gojaRuntime := mustNewGoja(t)
	original := evalGoja(t, gojaRuntime, `(() => {
		const buf = new ArrayBuffer(8);
		const u8 = new Uint8Array(buf);
		u8.set([1, 2, 3, 4, 5, 6, 7, 8]);
		return {
			buf,
			u8,
			u16: new Uint16Array(buf, 2, 2),
			nested: { view: new Uint8Array(buf, 4, 2) },
		};
	})()`)
	wire, err := EncodeGoja(original)
	if err != nil {
		t.Fatalf("EncodeGoja(buffer alias graph) error = %v", err)
	}
	patched, err := wire.MergeObject(ObjectType{"patched": true})
	if err != nil {
		t.Fatalf("MergeObject(buffer alias graph) error = %v", err)
	}

	verifier := `(function(value) {
		const failures = [];
		if (value.patched !== true) failures.push("patched field");
		if (!(value.buf instanceof ArrayBuffer) || !(value.u8 instanceof Uint8Array) || !(value.u16 instanceof Uint16Array)) failures.push("constructors");
		if (value.u8.buffer !== value.buf) failures.push("u8 buffer alias");
		if (value.u16.buffer !== value.buf) failures.push("u16 buffer alias");
		if (!value.nested || value.nested.view.buffer !== value.buf) failures.push("nested view buffer alias");
		if (value.u16.byteOffset !== 2 || value.u16.byteLength !== 4) failures.push("u16 view shape");
		value.u16[0] = 0;
		if (value.u8[2] !== 0 || value.u8[3] !== 0) failures.push("u16 mutation did not flow through shared buffer");
		value.u8[4] = 77;
		if (value.nested.view[0] !== 77) failures.push("u8 mutation did not flow to nested view");
		return failures.join("\n");
	})`

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	quickValue, err := DecodeQuickJS(quickRuntime.Context(), patched)
	if err != nil {
		t.Fatalf("DecodeQuickJS(buffer alias graph) error = %v", err)
	}
	defer quickValue.Free()
	assertObjectiveQuickJS(t, quickRuntime, verifier, quickValue)

	gojaPatchedRuntime := mustNewGoja(t)
	gojaPatchedValue, err := DecodeGoja(gojaPatchedRuntime, patched)
	if err != nil {
		t.Fatalf("DecodeGoja(buffer alias graph) error = %v", err)
	}
	assertObjectiveGoja(t, gojaPatchedRuntime, verifier, gojaPatchedValue)
}

func TestJswireObjectiveQuickJSOriginPatchToGojaPreservesNativeGraph(t *testing.T) {
	sourceQuick := mustNewQuickJS(t)
	defer sourceQuick.Close()
	originalQuick := evalQuickJS(t, sourceQuick, objectiveNativeSourceJS)
	defer originalQuick.Free()
	wire, err := EncodeQuickJS(originalQuick)
	if err != nil {
		t.Fatalf("EncodeQuickJS(objective native source) error = %v", err)
	}
	patched, err := wire.MergeObject(ObjectType{
		"scalar": "patched",
		"added":  objectiveTypedCapsule(),
	})
	if err != nil {
		t.Fatalf("MergeObject(quickjs-origin value) error = %v", err)
	}

	gojaRuntime := mustNewGoja(t)
	gojaValue, err := DecodeGoja(gojaRuntime, patched)
	if err != nil {
		t.Fatalf("DecodeGoja(quickjs-origin patched value) error = %v", err)
	}
	assertObjectiveGoja(t, gojaRuntime, objectivePatchedRootVerifierJS, gojaValue)
}

func TestJswireObjectivePublicAPIShapesAndConvenienceMethods(t *testing.T) {
	t.Run("public typed model names have the required compact underlying shapes", func(t *testing.T) {
		type shapeCase struct {
			name string
			typ  reflect.Type
			kind reflect.Kind
			len  int
		}
		for _, tt := range []shapeCase{
			{name: "ObjectType", typ: reflect.TypeOf(ObjectType{}), kind: reflect.Map},
			{name: "ArrayType", typ: reflect.TypeOf(ArrayType{}), kind: reflect.Slice},
			{name: "MapEntry", typ: reflect.TypeOf(MapEntry{}), kind: reflect.Array, len: 2},
			{name: "MapType", typ: reflect.TypeOf(MapType{}), kind: reflect.Slice},
			{name: "SetType", typ: reflect.TypeOf(SetType{}), kind: reflect.Slice},
			{name: "DateType", typ: reflect.TypeOf(DateType(time.Time{})), kind: reflect.Struct},
			{name: "BigIntType", typ: reflect.TypeOf(BigIntType("")), kind: reflect.String},
			{name: "RegExpType", typ: reflect.TypeOf(RegExpType{}), kind: reflect.Array, len: 2},
			{name: "ArrayBufferType", typ: reflect.TypeOf(ArrayBufferType{}), kind: reflect.Slice},
			{name: "Uint8ArrayType", typ: reflect.TypeOf(Uint8ArrayType{}), kind: reflect.Slice},
			{name: "Uint8ClampedArrayType", typ: reflect.TypeOf(Uint8ClampedArrayType{}), kind: reflect.Slice},
			{name: "Int8ArrayType", typ: reflect.TypeOf(Int8ArrayType{}), kind: reflect.Slice},
			{name: "Uint16ArrayType", typ: reflect.TypeOf(Uint16ArrayType{}), kind: reflect.Slice},
			{name: "Int16ArrayType", typ: reflect.TypeOf(Int16ArrayType{}), kind: reflect.Slice},
			{name: "Uint32ArrayType", typ: reflect.TypeOf(Uint32ArrayType{}), kind: reflect.Slice},
			{name: "Int32ArrayType", typ: reflect.TypeOf(Int32ArrayType{}), kind: reflect.Slice},
			{name: "BigUint64ArrayType", typ: reflect.TypeOf(BigUint64ArrayType{}), kind: reflect.Slice},
			{name: "BigInt64ArrayType", typ: reflect.TypeOf(BigInt64ArrayType{}), kind: reflect.Slice},
			{name: "Float32ArrayType", typ: reflect.TypeOf(Float32ArrayType{}), kind: reflect.Slice},
			{name: "Float64ArrayType", typ: reflect.TypeOf(Float64ArrayType{}), kind: reflect.Slice},
		} {
			t.Run(tt.name, func(t *testing.T) {
				if tt.typ.Name() != tt.name {
					t.Fatalf("type name = %q, want %q", tt.typ.Name(), tt.name)
				}
				if tt.typ.Kind() != tt.kind {
					t.Fatalf("%s kind = %v, want %v", tt.name, tt.typ.Kind(), tt.kind)
				}
				if tt.kind == reflect.Array && tt.typ.Len() != tt.len {
					t.Fatalf("%s len = %d, want %d", tt.name, tt.typ.Len(), tt.len)
				}
			})
		}
		if reflect.TypeOf(ObjectType{}) == reflect.TypeOf(MapType{}) {
			t.Fatalf("ObjectType and MapType must be distinct types")
		}
	})

	t.Run("Value methods expose raw bytes clone decode describe and stable display errors", func(t *testing.T) {
		raw := MustEncode(ObjectType{
			"answer": 42,
			"big":    BigIntType("9007199254740993"),
		})
		decoded, err := raw.Decode()
		if err != nil {
			t.Fatalf("Value.Decode() error = %v", err)
		}
		if !reflect.DeepEqual(decoded, ObjectType{"answer": float64(42), "big": BigIntType("9007199254740993")}) {
			t.Fatalf("Value.Decode() = %#v", decoded)
		}

		aliased := raw.Bytes()
		if len(aliased) == 0 {
			t.Fatalf("Value.Bytes() returned empty slice for non-empty value")
		}
		originalFirst := raw[0]
		aliased[0] ^= 0xff
		if raw[0] == originalFirst {
			t.Fatalf("Value.Bytes() did not alias Value storage as documented")
		}
		aliased[0] = originalFirst

		cloned := raw.Clone()
		if !bytes.Equal(cloned, raw) {
			t.Fatalf("Value.Clone() bytes mismatch")
		}
		cloned[0] ^= 0xff
		if bytes.Equal(cloned, raw) {
			t.Fatalf("Value.Clone() returned aliased storage")
		}
		if Value(nil).Clone() != nil {
			t.Fatalf("nil Value.Clone() must return nil")
		}

		inspection, err := raw.Describe()
		if err != nil {
			t.Fatalf("Value.Describe() error = %v", err)
		}
		freeInspection, err := Describe(raw)
		if err != nil {
			t.Fatalf("Describe(Value) error = %v", err)
		}
		if inspection != freeInspection {
			t.Fatalf("Value.Describe() = %#v, Describe() = %#v", inspection, freeInspection)
		}
		if raw.DisplaySummary() != inspection.Summary {
			t.Fatalf("DisplaySummary() = %q, want %q", raw.DisplaySummary(), inspection.Summary)
		}
		if raw.DisplayFull() != inspection.Full {
			t.Fatalf("DisplayFull() = %q, want %q", raw.DisplayFull(), inspection.Full)
		}
		if !strings.Contains(Value{0xff}.DisplaySummary(), "<jswire-describe-error:") {
			t.Fatalf("DisplaySummary() for invalid wire did not return stable error marker")
		}
		if !strings.Contains(Value{0xff}.DisplayFull(), "<jswire-describe-error:") {
			t.Fatalf("DisplayFull() for invalid wire did not return stable error marker")
		}
	})

	t.Run("MustEncode panics on unsupported values", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatalf("MustEncode(func) did not panic")
			}
		}()
		_ = MustEncode(func() {})
	})
}

func TestJswireObjectiveDocumentationContract(t *testing.T) {
	docs := strings.ReplaceAll(objectivePackageSourceText(t), "`", "")
	for _, phrase := range []string{
		"binary",
		"not JSON",
		"Value",
		"typed Go model",
		"ObjectType",
		"MapType",
		"BigIntType",
		"trailing",
		"DateType",
		"millisecond",
		"ArrayBufferType",
		"copy",
		"typed array",
		"Patch",
		"not RFC",
		"JSON Patch",
		"engine bridge",
		"defined subset",
	} {
		if !strings.Contains(docs, phrase) {
			t.Fatalf("package docs/source do not mention %q", phrase)
		}
	}
}

func ExampleEncode_typedModel() {
	raw, err := Encode(ObjectType{
		"when": DateType(time.UnixMilli(1720000000456).UTC()),
		"big":  BigIntType("9007199254740993"),
		"re":   RegExpType{"a+b", "i"},
		"map": MapType{
			{"alpha", 40},
			{"beta", ObjectType{"count": 2}},
		},
		"set":   SetType{"one", "two"},
		"bytes": Uint8ArrayType{1, 2, 3},
	})
	if err != nil {
		panic(err)
	}
	_, _ = raw.Decode()
}

func ExampleValue_MergeObject() {
	argsWire := MustEncode(ObjectType{
		"a":    1,
		"when": DateType(time.UnixMilli(1720000000456).UTC()),
	})
	merged, err := argsWire.MergeObject(ObjectType{
		"a":                 10,
		"workspace_account": "work@example.com",
	})
	if err != nil {
		panic(err)
	}
	_, _ = merged.Decode()
}

func TestJswireObjectiveEdgeCases_TypedModelSemanticsCopiesAndOrdering(t *testing.T) {
	t.Run("top-level nil encodes as typed model nil", func(t *testing.T) {
		requireTypedRoundTrip(t, nil, nil)
	})

	t.Run("Value bytes are jswire binary data and not JSON", func(t *testing.T) {
		raw, err := Encode(ObjectType{
			"big": BigIntType("9007199254740993"),
			"map": MapType{
				{BigIntType("1"), "one"},
			},
		})
		if err != nil {
			t.Fatalf("Encode(native typed object) error = %v", err)
		}
		if json.Valid(raw.Bytes()) {
			t.Fatalf("jswire Value unexpectedly validated as JSON: %q", string(raw.Bytes()))
		}
	})

	t.Run("nil map slice time and primitive conveniences decode to canonical typed model", func(t *testing.T) {
		precise := time.Date(2026, 6, 3, 21, 22, 6, 987654321, time.FixedZone("odd", 90*60))
		wantTime := time.UnixMilli(precise.UnixMilli()).UTC()
		in := map[string]any{
			"nil":    nil,
			"bool":   false,
			"string": "hello",
			"array": []any{
				nil,
				true,
				"x",
				int8(-8),
				int16(-16),
				int32(-32),
				int64(-9007199254740991),
				uint(42),
				uint64(9007199254740991),
				float32(1.5),
				float64(-2.25),
			},
			"when": precise,
			"object": map[string]any{
				"nested": "ok",
			},
		}
		want := ObjectType{
			"nil":    nil,
			"bool":   false,
			"string": "hello",
			"array": ArrayType{
				nil,
				true,
				"x",
				float64(-8),
				float64(-16),
				float64(-32),
				float64(-9007199254740991),
				float64(42),
				float64(9007199254740991),
				float64(1.5),
				float64(-2.25),
			},
			"when": DateType(wantTime),
			"object": ObjectType{
				"nested": "ok",
			},
		}
		requireTypedRoundTrip(t, in, want)
	})

	t.Run("BigInt and RegExp valid boundary values round trip exactly", func(t *testing.T) {
		for _, in := range []any{
			BigIntType("0"),
			BigIntType("-1"),
			BigIntType("9007199254740993"),
			BigIntType("-123456789012345678901234567890"),
			RegExpType{"a+b", "gi"},
			RegExpType{"^z+$", ""},
			RegExpType{"line\\nbreak", "s"},
		} {
			requireTypedRoundTrip(t, in, in)
		}
	})

	t.Run("RegExp flag order is preserved or normalized with documentation", func(t *testing.T) {
		raw, err := Encode(RegExpType{"x", "ig"})
		if err != nil {
			t.Fatalf("Encode(RegExpType flag order) error = %v", err)
		}
		gotAny, err := Decode(raw)
		if err != nil {
			t.Fatalf("Decode(RegExpType flag order) error = %v", err)
		}
		got, ok := gotAny.(RegExpType)
		if !ok {
			t.Fatalf("Decode(RegExpType flag order) type = %T", gotAny)
		}
		if got[0] != "x" {
			t.Fatalf("RegExp source = %q, want x", got[0])
		}
		switch got[1] {
		case "ig":
		case "gi":
			requireDocumentationMentions(t, "RegExp", "flag")
		default:
			t.Fatalf("RegExp flags = %q, want preserved ig or normalized gi", got[1])
		}
	})

	t.Run("nil primitives object and array materialize as native JS null and ordinary objects", func(t *testing.T) {
		wire, err := Encode(ObjectType{
			"nil":    nil,
			"bool":   true,
			"number": 12.5,
			"string": "ok",
			"array":  ArrayType{nil, false, "x"},
			"object": ObjectType{"nested": nil},
		})
		if err != nil {
			t.Fatalf("Encode(nil primitive object) error = %v", err)
		}
		verifier := `(function(value) {
			const failures = [];
			if (Object.prototype.toString.call(value) !== "[object Object]") failures.push("ordinary object");
			if (value.nil !== null) failures.push("nil is not null");
			if (value.bool !== true || value.number !== 12.5 || value.string !== "ok") failures.push("primitive values");
			if (!Array.isArray(value.array) || value.array[0] !== null || value.array[1] !== false || value.array[2] !== "x") failures.push("array values");
			if (!value.object || value.object.nested !== null) failures.push("nested null");
			if (value instanceof Map) failures.push("ObjectType materialized as Map");
			return failures.join("\n");
		})`
		quickRuntime := mustNewQuickJS(t)
		defer quickRuntime.Close()
		quickValue, err := DecodeQuickJS(quickRuntime.Context(), wire)
		if err != nil {
			t.Fatalf("DecodeQuickJS(nil primitive object) error = %v", err)
		}
		defer quickValue.Free()
		assertObjectiveQuickJS(t, quickRuntime, verifier, quickValue)

		gojaRuntime := mustNewGoja(t)
		gojaValue, err := DecodeGoja(gojaRuntime, wire)
		if err != nil {
			t.Fatalf("DecodeGoja(nil primitive object) error = %v", err)
		}
		assertObjectiveGoja(t, gojaRuntime, verifier, gojaValue)
	})

	t.Run("MapType supports object and array keys without converting to ObjectType", func(t *testing.T) {
		wire, err := Encode(MapType{
			{ObjectType{"kind": "object-key"}, "object-value"},
			{ArrayType{"array-key", float64(2)}, DateType(time.UnixMilli(8).UTC())},
		})
		if err != nil {
			t.Fatalf("Encode(MapType object keys) error = %v", err)
		}
		requireDecodeEqual(t, wire, MapType{
			{ObjectType{"kind": "object-key"}, "object-value"},
			{ArrayType{"array-key", float64(2)}, DateType(time.UnixMilli(8).UTC())},
		})
		verifier := `(function(value) {
			const failures = [];
			if (!(value instanceof Map) || value.size !== 2) failures.push("MapType constructor/size");
			let sawObjectKey = false;
			let sawArrayKey = false;
			for (const [key, item] of value) {
				if (key && key.kind === "object-key" && item === "object-value") sawObjectKey = true;
				if (Array.isArray(key) && key[0] === "array-key" && key[1] === 2 && item instanceof Date && item.getTime() === 8) sawArrayKey = true;
			}
			if (!sawObjectKey) failures.push("object key entry");
			if (!sawArrayKey) failures.push("array key entry");
			return failures.join("\n");
		})`
		quickRuntime := mustNewQuickJS(t)
		defer quickRuntime.Close()
		quickValue, err := DecodeQuickJS(quickRuntime.Context(), wire)
		if err != nil {
			t.Fatalf("DecodeQuickJS(MapType object keys) error = %v", err)
		}
		defer quickValue.Free()
		assertObjectiveQuickJS(t, quickRuntime, verifier, quickValue)
	})

	t.Run("nested Value policy is either graph-spliced or explicitly documented as unsupported", func(t *testing.T) {
		child, err := Encode(ObjectType{"child": BigIntType("7")})
		if err != nil {
			t.Fatalf("Encode(child) error = %v", err)
		}
		raw, err := Encode(ObjectType{"wrapped": Value(child)})
		if err != nil {
			assertErrorContains(t, err, "Value")
			requireDocumentationMentions(t, "nested Value")
			return
		}
		requireDecodeEqual(t, raw, ObjectType{"wrapped": ObjectType{"child": BigIntType("7")}})
	})

	t.Run("Encode(Value) clones whole values instead of aliasing caller memory", func(t *testing.T) {
		wire, err := Encode(ObjectType{"x": "y", "bytes": Uint8ArrayType{1, 2, 3}})
		if err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
		cloned, err := Encode(Value(wire))
		if err != nil {
			t.Fatalf("Encode(Value) error = %v", err)
		}
		if !bytes.Equal(cloned, wire) {
			t.Fatalf("Encode(Value) bytes mismatch\nwant %x\n got %x", []byte(wire), []byte(cloned))
		}
		cloned[0] ^= 0xff
		if bytes.Equal(cloned, wire) {
			t.Fatalf("Encode(Value) returned an alias; mutating clone changed original")
		}
		if _, err := Decode(wire); err != nil {
			t.Fatalf("original Value was corrupted after mutating clone: %v", err)
		}
	})

	t.Run("typed array and ArrayBuffer encode/decode copy bytes", func(t *testing.T) {
		u8 := Uint8ArrayType{1, 2, 3}
		u8Wire, err := Encode(u8)
		if err != nil {
			t.Fatalf("Encode(Uint8ArrayType) error = %v", err)
		}
		u8[0] = 99
		u8DecodedAny, err := Decode(u8Wire)
		if err != nil {
			t.Fatalf("Decode(Uint8ArrayType wire) error = %v", err)
		}
		u8Decoded, ok := u8DecodedAny.(Uint8ArrayType)
		if !ok {
			t.Fatalf("Decode(Uint8ArrayType wire) type = %T", u8DecodedAny)
		}
		if !reflect.DeepEqual(u8Decoded, Uint8ArrayType{1, 2, 3}) {
			t.Fatalf("Uint8Array encode did not copy input: %#v", u8Decoded)
		}
		u8Decoded[1] = 88
		u8DecodedAgain, err := Decode(u8Wire)
		if err != nil {
			t.Fatalf("Decode(Uint8ArrayType wire again) error = %v", err)
		}
		if !reflect.DeepEqual(u8DecodedAgain, Uint8ArrayType{1, 2, 3}) {
			t.Fatalf("Uint8Array decode returned aliased storage: %#v", u8DecodedAgain)
		}

		buf := ArrayBufferType{4, 5, 6}
		bufWire, err := Encode(buf)
		if err != nil {
			t.Fatalf("Encode(ArrayBufferType) error = %v", err)
		}
		buf[0] = 77
		bufDecodedAny, err := Decode(bufWire)
		if err != nil {
			t.Fatalf("Decode(ArrayBufferType wire) error = %v", err)
		}
		bufDecoded, ok := bufDecodedAny.(ArrayBufferType)
		if !ok {
			t.Fatalf("Decode(ArrayBufferType wire) type = %T", bufDecodedAny)
		}
		if !reflect.DeepEqual(bufDecoded, ArrayBufferType{4, 5, 6}) {
			t.Fatalf("ArrayBuffer encode did not copy input: %#v", bufDecoded)
		}
		bufDecoded[1] = 88
		bufDecodedAgain, err := Decode(bufWire)
		if err != nil {
			t.Fatalf("Decode(ArrayBufferType wire again) error = %v", err)
		}
		if !reflect.DeepEqual(bufDecodedAgain, ArrayBufferType{4, 5, 6}) {
			t.Fatalf("ArrayBuffer decode returned aliased storage: %#v", bufDecodedAgain)
		}
	})

	t.Run("ObjectType keys are sorted deterministically while MapType entry order is preserved", func(t *testing.T) {
		objectWire, err := Encode(ObjectType{
			"z": 26,
			"b": 2,
			"a": 1,
			"m": 13,
		})
		if err != nil {
			t.Fatalf("Encode(ObjectType) error = %v", err)
		}
		objectNode := objectiveRootNode(t, objectWire)
		gotObjectKeys := make([]string, 0, len(objectNode.Props))
		for _, prop := range objectNode.Props {
			gotObjectKeys = append(gotObjectKeys, prop.Key)
		}
		if !reflect.DeepEqual(gotObjectKeys, []string{"a", "b", "m", "z"}) {
			t.Fatalf("ObjectType props are not sorted deterministically: %#v", gotObjectKeys)
		}
		for i := 0; i < 20; i++ {
			again, err := Encode(ObjectType{"z": 26, "b": 2, "a": 1, "m": 13})
			if err != nil {
				t.Fatalf("Encode(ObjectType) repeat %d error = %v", i, err)
			}
			if !bytes.Equal(objectWire, again) {
				t.Fatalf("Encode(ObjectType) repeat %d was not byte deterministic\nfirst %x\nagain %x", i, []byte(objectWire), []byte(again))
			}
		}

		mapWire, err := Encode(MapType{
			{"z", 26},
			{"a", 1},
			{"m", 13},
			{"b", 2},
		})
		if err != nil {
			t.Fatalf("Encode(MapType) error = %v", err)
		}
		mapNode := objectiveRootNode(t, mapWire)
		gotMapKeys := make([]string, 0, len(mapNode.Entries))
		for _, entry := range mapNode.Entries {
			if entry.Key.Kind != valueString {
				t.Fatalf("MapType test key kind = %v, want string", entry.Key.Kind)
			}
			gotMapKeys = append(gotMapKeys, entry.Key.Text)
		}
		if !reflect.DeepEqual(gotMapKeys, []string{"z", "a", "m", "b"}) {
			t.Fatalf("MapType entry order was not preserved: %#v", gotMapKeys)
		}
	})
}

func TestJswireObjectiveEdgeCases_NativeEnginesDecodeToTypedModelMatrix(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want any
	}{
		{
			name: "plain object array and null",
			expr: `({a: 1, b: ["x", null, true]})`,
			want: ObjectType{"a": float64(1), "b": ArrayType{"x", nil, true}},
		},
		{
			name: "Date",
			expr: `new Date(1720000000456)`,
			want: DateType(time.UnixMilli(1720000000456).UTC()),
		},
		{
			name: "BigInt",
			expr: `-9007199254740993n`,
			want: BigIntType("-9007199254740993"),
		},
		{
			name: "RegExp",
			expr: `/z+/gi`,
			want: RegExpType{"z+", "gi"},
		},
		{
			name: "Map and Set with non-string keys and native values",
			expr: `new Map([[2n, /z+/g], ["set", new Set([new Date(5), new Uint8Array([1, 2])])]])`,
			want: MapType{
				{BigIntType("2"), RegExpType{"z+", "g"}},
				{"set", SetType{DateType(time.UnixMilli(5).UTC()), Uint8ArrayType{1, 2}}},
			},
		},
		{
			name: "ArrayBuffer",
			expr: `new Uint8Array([9, 8, 7]).buffer`,
			want: ArrayBufferType{9, 8, 7},
		},
		{name: "Uint8Array", expr: `new Uint8Array([255, 1, 2])`, want: Uint8ArrayType{255, 1, 2}},
		{name: "Uint8ClampedArray", expr: `new Uint8ClampedArray([300, -2, 128])`, want: Uint8ClampedArrayType{255, 0, 128}},
		{name: "Int8Array", expr: `new Int8Array([-128, 0, 127])`, want: Int8ArrayType{-128, 0, 127}},
		{name: "Uint16Array", expr: `new Uint16Array([1, 256, 65535])`, want: Uint16ArrayType{1, 256, 65535}},
		{name: "Int16Array", expr: `new Int16Array([-32768, 0, 32767])`, want: Int16ArrayType{-32768, 0, 32767}},
		{name: "Uint32Array", expr: `new Uint32Array([1, 65536, 4294967295])`, want: Uint32ArrayType{1, 65536, 4294967295}},
		{name: "Int32Array", expr: `new Int32Array([-2147483648, 0, 2147483647])`, want: Int32ArrayType{-2147483648, 0, 2147483647}},
		{name: "BigUint64Array", expr: `new BigUint64Array([1n, 18446744073709551615n])`, want: BigUint64ArrayType{1, 18446744073709551615}},
		{name: "BigInt64Array", expr: `new BigInt64Array([-9223372036854775808n, 0n, 9223372036854775807n])`, want: BigInt64ArrayType{-9223372036854775808, 0, 9223372036854775807}},
		{name: "Float32Array", expr: `new Float32Array([1.5, -2.25])`, want: Float32ArrayType{1.5, -2.25}},
		{name: "Float64Array", expr: `new Float64Array([1.25, -2.5])`, want: Float64ArrayType{1.25, -2.5}},
	}

	gojaRuntime := mustNewGoja(t)
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	for _, tt := range cases {
		t.Run("goja/"+tt.name, func(t *testing.T) {
			wire, err := EncodeGoja(evalGoja(t, gojaRuntime, tt.expr))
			if err != nil {
				t.Fatalf("EncodeGoja(%s) error = %v", tt.expr, err)
			}
			requireDecodeEqual(t, wire, tt.want)
		})

		t.Run("quickjs/"+tt.name, func(t *testing.T) {
			value := evalQuickJS(t, quickRuntime, tt.expr)
			defer value.Free()
			wire, err := EncodeQuickJS(value)
			if err != nil {
				t.Fatalf("EncodeQuickJS(%s) error = %v", tt.expr, err)
			}
			requireDecodeEqual(t, wire, tt.want)
		})
	}
}

func TestJswireObjectiveEdgeCases_MapSetDuplicateConstructionSemantics(t *testing.T) {
	wire, err := Encode(ObjectType{
		"map": MapType{
			{"dup", "first"},
			{"dup", "second"},
			{BigIntType("1"), "one"},
			{BigIntType("1"), "one-again"},
		},
		"set": SetType{"dup", "dup", BigIntType("1"), BigIntType("1")},
	})
	if err != nil {
		t.Fatalf("Encode(duplicates) error = %v", err)
	}

	verifier := `(function(value) {
		const failures = [];
		if (!(value.map instanceof Map)) {
			failures.push("map constructor");
		} else {
			if (value.map.size !== 2) failures.push("map size " + value.map.size);
			if (value.map.get("dup") !== "second") failures.push("map duplicate string key");
			if (value.map.get(1n) !== "one-again") failures.push("map duplicate BigInt key");
		}
		if (!(value.set instanceof Set)) {
			failures.push("set constructor");
		} else {
			if (value.set.size !== 2) failures.push("set size " + value.set.size);
			if (!value.set.has("dup") || !value.set.has(1n)) failures.push("set values");
		}
		return failures.join("\n");
	})`

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	quickValue, err := DecodeQuickJS(quickRuntime.Context(), wire)
	if err != nil {
		t.Fatalf("DecodeQuickJS(duplicates) error = %v", err)
	}
	defer quickValue.Free()
	assertObjectiveQuickJS(t, quickRuntime, verifier, quickValue)

	gojaRuntime := mustNewGoja(t)
	gojaValue, err := DecodeGoja(gojaRuntime, wire)
	if err != nil {
		t.Fatalf("DecodeGoja(duplicates) error = %v", err)
	}
	assertObjectiveGoja(t, gojaRuntime, verifier, gojaValue)
}

func TestJswireObjectiveEdgeCases_ValidationErrorsHavePrecisePaths(t *testing.T) {
	t.Run("invalid BigInt syntax", func(t *testing.T) {
		for _, text := range []string{"", " ", "\t1", "+1", "--1", "1.0", "1e3", "123n", "12 3"} {
			t.Run(strings.ReplaceAll(text, "\t", "\\t"), func(t *testing.T) {
				requireEncodeErrorContains(t, BigIntType(text), "BigInt")
			})
		}
	})

	t.Run("invalid RegExp flags", func(t *testing.T) {
		for _, rx := range []RegExpType{
			{"x", "gg"},
			{"x", "ii"},
			{"x", "z"},
			{"x", " g"},
		} {
			t.Run(rx[1], func(t *testing.T) {
				requireEncodeErrorContains(t, rx, "RegExp", "flag")
			})
		}
	})

	for _, tt := range []struct {
		name  string
		value any
		parts []string
	}{
		{
			name: "nested object unsupported type path",
			value: ObjectType{
				"input": ObjectType{
					"when": func() {},
				},
			},
			parts: []string{"input.when", "func"},
		},
		{
			name: "plain map unsupported nested type path",
			value: map[string]any{
				"input": map[string]any{
					"bad": make(chan int),
				},
			},
			parts: []string{"input.bad", "chan int"},
		},
		{
			name: "array unsupported index path",
			value: ArrayType{
				"ok",
				func() {},
			},
			parts: []string{"array index 1", "func"},
		},
		{
			name: "map entry key unsupported path",
			value: MapType{
				{make(chan int), "value"},
			},
			parts: []string{"map entry 0 key", "chan int"},
		},
		{
			name: "map entry value unsupported path",
			value: MapType{
				{"key", make(chan int)},
			},
			parts: []string{"map entry 0 value", "chan int"},
		},
		{
			name: "set unsupported value path",
			value: SetType{
				"ok",
				func() {},
			},
			parts: []string{"set value 1", "func"},
		},
		{
			name: "non string plain map rejected",
			value: ObjectType{
				"plainMap": map[int]any{1: "one"},
			},
			parts: []string{"plainMap", "map[int]"},
		},
		{
			name: "integer outside JS safe range rejected",
			value: ObjectType{
				"unsafe": uint64(9007199254740992),
			},
			parts: []string{"unsafe", "uint64"},
		},
		{
			name: "negative integer outside JS safe range rejected",
			value: ObjectType{
				"unsafe": int64(-9007199254740992),
			},
			parts: []string{"unsafe", "int64"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requireEncodeErrorContains(t, tt.value, tt.parts...)
		})
	}
}

func TestJswireObjectiveEdgeCases_PatchApplyAndMergeSemantics(t *testing.T) {
	baseWire, err := Encode(ObjectType{
		"keepDate": DateType(time.UnixMilli(1000).UTC()),
		"nested": ObjectType{
			"old":     "old",
			"scalar":  float64(1),
			"survive": MapType{{"k", BigIntType("4")}},
		},
		"remove": "gone",
		"arr":    ArrayType{"zero", "one"},
		"atomicMap": MapType{
			{"k", "v"},
		},
		"atomicSet": SetType{"v"},
		"atomicU8":  Uint8ArrayType{1},
		"atomicBuf": ArrayBufferType{2},
		"atomicRx":  RegExpType{"x", ""},
	})
	if err != nil {
		t.Fatalf("Encode(base) error = %v", err)
	}

	patched, err := baseWire.Apply(Patch{
		{Op: PatchSet, Path: Path{{Key: "nested"}, {Key: "old"}}, Value: "new"},
		{Op: PatchSet, Path: Path{{Key: "nested"}, {Key: "added"}}, Value: Uint8ArrayType{4, 5}},
		{Op: PatchSet, Path: Path{{Key: ""}}, Value: "empty-key"},
		{Op: PatchRemove, Path: Path{{Key: "remove"}}},
		{Op: PatchRemove, Path: Path{{Key: "missing"}}},
	})
	if err != nil {
		t.Fatalf("Apply(object property patch) error = %v", err)
	}
	requireDecodeEqual(t, patched, ObjectType{
		"":         "empty-key",
		"keepDate": DateType(time.UnixMilli(1000).UTC()),
		"nested": ObjectType{
			"old":     "new",
			"scalar":  float64(1),
			"survive": MapType{{"k", BigIntType("4")}},
			"added":   Uint8ArrayType{4, 5},
		},
		"arr":       ArrayType{"zero", "one"},
		"atomicMap": MapType{{"k", "v"}},
		"atomicSet": SetType{"v"},
		"atomicU8":  Uint8ArrayType{1},
		"atomicBuf": ArrayBufferType{2},
		"atomicRx":  RegExpType{"x", ""},
	})

	var empty Value
	created, err := empty.MergeObject(ObjectType{"created": true, "nil": nil})
	if err != nil {
		t.Fatalf("empty Value MergeObject() error = %v", err)
	}
	requireDecodeEqual(t, created, ObjectType{"created": true, "nil": nil})

	replacedRoot, err := baseWire.Apply(Patch{{Op: PatchSet, Path: nil, Value: ArrayType{"root-replaced"}}})
	if err != nil {
		t.Fatalf("Apply(root replacement) error = %v", err)
	}
	requireDecodeEqual(t, replacedRoot, ArrayType{"root-replaced"})

	childWire, err := Encode(ObjectType{"child": RegExpType{"x", "g"}})
	if err != nil {
		t.Fatalf("Encode(child patch Value) error = %v", err)
	}
	valuePatched, err := baseWire.Apply(Patch{{Op: PatchSet, Path: Path{{Key: "valueChild"}}, Value: Value(childWire)}})
	if err != nil {
		assertErrorContains(t, err, "Value")
		requireDocumentationMentions(t, "Value", "patch")
	} else {
		requireDecodeEqual(t, valuePatched, ObjectType{
			"keepDate": DateType(time.UnixMilli(1000).UTC()),
			"nested": ObjectType{
				"old":     "old",
				"scalar":  float64(1),
				"survive": MapType{{"k", BigIntType("4")}},
			},
			"remove":     "gone",
			"arr":        ArrayType{"zero", "one"},
			"atomicMap":  MapType{{"k", "v"}},
			"atomicSet":  SetType{"v"},
			"atomicU8":   Uint8ArrayType{1},
			"atomicBuf":  ArrayBufferType{2},
			"atomicRx":   RegExpType{"x", ""},
			"valueChild": ObjectType{"child": RegExpType{"x", "g"}},
		})
	}

	nonObjectWire, err := Encode("scalar")
	if err != nil {
		t.Fatalf("Encode(scalar) error = %v", err)
	}
	_, err = nonObjectWire.MergeObject(ObjectType{"x": 1})
	assertErrorContains(t, err, "object")

	_, err = baseWire.Apply(Patch{{Op: PatchSet, Path: Path{{Key: "nested"}, {Key: "scalar"}, {Key: "x"}}, Value: 1}})
	assertErrorContains(t, err, "nested.scalar", "object")

	_, err = baseWire.Apply(Patch{{Op: PatchSet, Path: Path{{Key: "keepDate"}, {Key: "x"}}, Value: "nope"}})
	assertErrorContains(t, err, "keepDate", "object")

	for _, key := range []string{"atomicMap", "atomicSet", "atomicU8", "atomicBuf", "atomicRx"} {
		t.Run("atomic patch traversal into "+key, func(t *testing.T) {
			_, err := baseWire.Apply(Patch{{Op: PatchSet, Path: Path{{Key: key}, {Key: "x"}}, Value: "nope"}})
			assertErrorContains(t, err, key, "object")
		})
	}

	index := 0
	_, err = baseWire.Apply(Patch{{Op: PatchSet, Path: Path{{Key: "arr"}, {Index: &index}}, Value: "nope"}})
	assertErrorContains(t, err, "index")

	_, err = baseWire.Apply(Patch{{Op: PatchSet, Path: Path{{Key: "nested", Index: &index}}, Value: "nope"}})
	assertErrorContains(t, err, "path")

	_, err = baseWire.Apply(Patch{{Op: PatchSet, Path: Path{{Key: "bad"}}, Value: func() {}}})
	assertErrorContains(t, err, "bad", "func")

	_, err = baseWire.Apply(Patch{{Op: PatchOpKind("copy"), Path: Path{{Key: "x"}}, Value: 1}})
	assertErrorContains(t, err, "copy")
}

func TestJswireObjectiveEdgeCases_TypedDecodeRejectsLossyNativeGraphs(t *testing.T) {
	gojaRuntime := mustNewGoja(t)

	for _, tt := range []struct {
		name string
		val  goja.Value
		want any
	}{
		{name: "undefined is documented as nil in the typed Go model", val: goja.Undefined(), want: nil},
		{name: "null decodes nil", val: goja.Null(), want: nil},
		{name: "Date decodes UTC millisecond precision", val: evalGoja(t, gojaRuntime, "new Date(123456789)"), want: DateType(time.UnixMilli(123456789).UTC())},
		{name: "typed array view with non-zero offset decodes as standalone copied elements", val: evalGoja(t, gojaRuntime, `(() => {
			const full = new Uint16Array([513, 1027, 65535]);
			return full.subarray(1);
		})()`), want: Uint16ArrayType{1027, 65535}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wire, err := EncodeGoja(tt.val)
			if err != nil {
				t.Fatalf("EncodeGoja() error = %v", err)
			}
			requireDecodeEqual(t, wire, tt.want)
		})
	}

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	quickUndefined := evalQuickJS(t, quickRuntime, "undefined")
	defer quickUndefined.Free()
	quickUndefinedWire, err := EncodeQuickJS(quickUndefined)
	if err != nil {
		t.Fatalf("EncodeQuickJS(undefined) error = %v", err)
	}
	requireDecodeEqual(t, quickUndefinedWire, nil)

	quickOffsetView := evalQuickJS(t, quickRuntime, `(() => {
		const full = new Uint16Array([513, 1027, 65535]);
		return full.subarray(1);
	})()`)
	defer quickOffsetView.Free()
	quickOffsetWire, err := EncodeQuickJS(quickOffsetView)
	if err != nil {
		t.Fatalf("EncodeQuickJS(non-zero offset view) error = %v", err)
	}
	requireDecodeEqual(t, quickOffsetWire, Uint16ArrayType{1027, 65535})

	unknownKindWire := marshalGraph(wireGraph{
		Root: wireValue{Kind: valueRef, Ref: 1},
		Nodes: []wireNode{
			{ID: 1, Kind: nodeKind(99)},
		},
	})
	_, err = Decode(unknownKindWire)
	assertErrorContains(t, err, "invalid")

	for _, tt := range []struct {
		name  string
		expr  string
		parts []string
	}{
		{
			name:  "Error object has no typed Go model and must not degrade to plain object",
			expr:  `(() => { const err = new TypeError("boom"); err.extra = 1; return err; })()`,
			parts: []string{"unsupported", "Error"},
		},
		{
			name:  "sparse array hole cannot be represented by ArrayType",
			expr:  `(() => { const a = []; a[1] = "x"; return a; })()`,
			parts: []string{"array", "hole"},
		},
		{
			name:  "cyclic object cannot silently decode into ObjectType",
			expr:  `(() => { const o = {}; o.self = o; return o; })()`,
			parts: []string{"cycle"},
		},
		{
			name:  "shared object identity cannot be silently duplicated",
			expr:  `(() => { const shared = { v: 1 }; return { a: shared, b: shared }; })()`,
			parts: []string{"shared"},
		},
		{
			name:  "custom class instance is not a JS plain object",
			expr:  `new (class CustomThing { constructor() { this.x = 1; } })()`,
			parts: []string{"CustomThing"},
		},
		{
			name:  "DataView has no public typed Go model",
			expr:  `new DataView(new Uint8Array([1, 2, 3, 4]).buffer, 1, 2)`,
			parts: []string{"DataView"},
		},
		{
			name:  "Promise has no public typed Go model",
			expr:  `Promise.resolve(1)`,
			parts: []string{"Promise"},
		},
		{
			name:  "invalid Date has no DateType timestamp",
			expr:  `new Date(NaN)`,
			parts: []string{"Date", "invalid"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wire, err := EncodeGoja(evalGoja(t, gojaRuntime, tt.expr))
			if err != nil {
				t.Fatalf("EncodeGoja(%s) error = %v", tt.expr, err)
			}
			_, err = Decode(wire)
			assertErrorContains(t, err, tt.parts...)
		})
	}
}

func objectivePatchTime() time.Time {
	return time.UnixMilli(objectivePatchUnixMS).UTC()
}

func objectiveTypedCapsule() ObjectType {
	return ObjectType{
		"label": "typed-capsule",
		"when":  DateType(objectivePatchTime()),
		"big":   BigIntType("-9007199254740993"),
		"rx":    RegExpType{"^z+$", "im"},
		"buf":   ArrayBufferType{9, 8, 7, 6},
		"u8":    Uint8ArrayType{255, 1, 2},
		"u8c":   Uint8ClampedArrayType{255, 0, 128},
		"i8":    Int8ArrayType{-128, 0, 127},
		"u16":   Uint16ArrayType{1, 256, 65535},
		"i16":   Int16ArrayType{-32768, 0, 32767},
		"u32":   Uint32ArrayType{1, 65536, 4294967295},
		"i32":   Int32ArrayType{-2147483648, 0, 2147483647},
		"bu64":  BigUint64ArrayType{1, 18446744073709551615},
		"bi64":  BigInt64ArrayType{-9223372036854775808, 0, 9223372036854775807},
		"f32":   Float32ArrayType{1.5, -2.25},
		"f64":   Float64ArrayType{1.25, -2.5},
		"map": MapType{
			{BigIntType("2"), SetType{RegExpType{"z+", "g"}, "ok"}},
			{"bytes", Uint8ArrayType{7, 8, 9}},
		},
		"set": SetType{DateType(objectivePatchTime()), BigIntType("5"), Uint16ArrayType{10, 11}},
	}
}

func requireTypedRoundTrip(t *testing.T, input, want any) {
	t.Helper()

	wire, err := Encode(input)
	if err != nil {
		t.Fatalf("Encode(%T) error = %v", input, err)
	}
	requireDecodeEqual(t, wire, want)
}

func requireDecodeEqual(t *testing.T, wire Value, want any) {
	t.Helper()

	got, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("Decode() mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func requireEncodeErrorContains(t *testing.T, value any, parts ...string) {
	t.Helper()

	_, err := Encode(value)
	assertErrorContains(t, err, parts...)
}

func assertErrorContains(t *testing.T, err error, parts ...string) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected error containing %q, got nil", parts)
	}
	text := err.Error()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Fatalf("error %q does not contain %q", text, part)
		}
	}
}

func requireDocumentationMentions(t *testing.T, parts ...string) {
	t.Helper()

	docs := objectivePackageSourceText(t)
	for _, part := range parts {
		if !strings.Contains(docs, part) {
			t.Fatalf("documented limitation/policy does not mention %q", part)
		}
	}
}

func objectivePackageSourceText(t *testing.T) string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out strings.Builder
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		bs, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out.Write(bs)
		out.WriteByte('\n')
	}
	return out.String()
}

func objectiveRootNode(t *testing.T, raw Value) wireNode {
	t.Helper()

	graph, err := unmarshalGraph(raw)
	if err != nil {
		t.Fatalf("unmarshalGraph() error = %v", err)
	}
	if graph.Root.Kind != valueRef {
		t.Fatalf("root kind = %v, want ref", graph.Root.Kind)
	}
	for _, node := range graph.Nodes {
		if node.ID == graph.Root.Ref {
			return node
		}
	}
	t.Fatalf("root ref %d not found in graph nodes", graph.Root.Ref)
	panic("unreachable")
}

func assertObjectiveQuickJS(t *testing.T, rt *qjs.Runtime, verifier string, value *qjs.Value) {
	t.Helper()

	fn, err := rt.Context().Eval("jswire-objective-verifier.js", qjs.Code(verifier))
	if err != nil {
		t.Fatalf("quickjs verifier eval: %v", err)
	}
	defer fn.Free()

	global := rt.Context().Global()
	defer global.Free()
	arg := value.Clone()
	defer arg.Free()
	result, err := rt.Context().Invoke(fn, global, arg)
	if err != nil {
		t.Fatalf("quickjs verifier invoke: %v", err)
	}
	defer result.Free()
	if failures := result.String(); failures != "" {
		t.Fatalf("quickjs native verifier failures:\n%s", failures)
	}
}

func assertObjectiveGoja(t *testing.T, rt *goja.Runtime, verifier string, value goja.Value) {
	t.Helper()

	fnValue, err := rt.RunString("(" + verifier + ")")
	if err != nil {
		t.Fatalf("goja verifier eval: %v", err)
	}
	fn, ok := goja.AssertFunction(fnValue)
	if !ok {
		t.Fatalf("goja verifier did not evaluate to function")
	}
	result, err := fn(goja.Undefined(), value)
	if err != nil {
		t.Fatalf("goja verifier invoke: %v", err)
	}
	if failures := result.String(); failures != "" {
		t.Fatalf("goja native verifier failures:\n%s", failures)
	}
}

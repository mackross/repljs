package jswire

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/dop251/goja"
	"github.com/fastschema/qjs"
	"github.com/mus-format/mus-go/varint"
)

const snapshotJS = `
(function () {
  function numToString(n) {
    if (Number.isNaN(n)) return { type: "nan" };
    if (n === Infinity) return { type: "infinity" };
    if (n === -Infinity) return { type: "-infinity" };
    if (Object.is(n, -0)) return { type: "-0" };
    return { type: "number", value: String(n) };
  }

  function toHex(bytes) {
    let out = "";
    for (let i = 0; i < bytes.length; i += 1) {
      const b = bytes[i].toString(16);
      out += b.length === 1 ? "0" + b : b;
    }
    return out;
  }

  globalThis.__bridgeSnapshot = function __bridgeSnapshot(root) {
    const seen = new Map();
    let nextId = 1;

    function visit(value) {
      if (value === undefined) return { type: "undefined" };
      if (value === null) return { type: "null" };

      const kind = typeof value;
      if (kind === "boolean") return { type: "boolean", value: value };
      if (kind === "string") return { type: "string", value: value };
      if (kind === "number") return numToString(value);
      if (kind === "bigint") return { type: "bigint", value: value.toString() };
      if (kind === "symbol") return { type: "symbol", value: String(value) };
      if (kind === "function") return { type: "function", name: value.name || "" };

      const existing = seen.get(value);
      if (existing) return { type: "ref", id: String(existing) };

      const id = nextId++;
      seen.set(value, id);

      if (value instanceof Date) {
        return {
          type: "date",
          id: String(id),
          ms: Number.isNaN(value.getTime()) ? "NaN" : String(value.getTime()),
          iso: Number.isNaN(value.getTime()) ? null : value.toISOString()
        };
      }

      if (value instanceof RegExp) {
        return {
          type: "regexp",
          id: String(id),
          source: value.source,
          flags: value.flags
        };
      }

      if (value instanceof ArrayBuffer) {
        return {
          type: "arraybuffer",
          id: String(id),
          hex: toHex(new Uint8Array(value))
        };
      }

      if (ArrayBuffer.isView(value)) {
        return {
          type: "typedarray",
          id: String(id),
          ctor: value.constructor && value.constructor.name ? value.constructor.name : "",
          byteOffset: String(value.byteOffset),
          byteLength: String(value.byteLength),
          hex: toHex(new Uint8Array(value.buffer, value.byteOffset, value.byteLength))
        };
      }

      if (value instanceof Map) {
        const entries = [];
        for (const [k, v] of value.entries()) {
          entries.push([visit(k), visit(v)]);
        }
        return { type: "map", id: String(id), entries: entries };
      }

      if (value instanceof Set) {
        const values = [];
        for (const item of value.values()) {
          values.push(visit(item));
        }
        return { type: "set", id: String(id), values: values };
      }

      if (Array.isArray(value)) {
        const items = [];
        for (let i = 0; i < value.length; i += 1) {
          if (i in value) items.push(visit(value[i]));
          else items.push({ type: "hole" });
        }
        return { type: "array", id: String(id), length: String(value.length), items: items };
      }

      if (value instanceof Error) {
        return {
          type: "error",
          id: String(id),
          name: value.name,
          message: value.message,
          cause: "cause" in value ? visit(value.cause) : { type: "undefined" }
        };
      }

      const ctor = value && value.constructor && value.constructor !== Object ? (value.constructor.name || "") : "";
      const props = [];
      const keys = Object.keys(value).sort();
      for (const key of keys) {
        props.push([key, visit(value[key])]);
      }
      return { type: "object", id: String(id), ctor: ctor, props: props };
    }

    return visit(root);
  };

  globalThis.__bridgeSnapshotJSON = function __bridgeSnapshotJSON(root) {
    return JSON.stringify(globalThis.__bridgeSnapshot(root));
  };
})();
`

const deepMixedCycleExpr = `(() => {
	const root = {
		name: "root",
		meta: {
			createdAt: new Date("2021-02-03T04:05:06.789Z"),
			pattern: /deep-cycle/giu,
			big: 12345678901234567890123456789012345678901234567890n
		},
		levels: []
	};

	const bytes = new Uint8Array([10, 20, 30, 40, 50, 60, 70, 80]);
	root.buffer = bytes.buffer;
	root.midView = new Uint16Array(root.buffer, 2, 2);

	let previous = root;
	for (let i = 0; i < 7; i += 1) {
		const node = {
			index: i,
			parent: previous,
			arr: [root, previous, { marker: "m" + i }],
			map: new Map(),
			set: new Set()
		};
		node.map.set("self", node);
		node.map.set("root", root);
		node.map.set(previous, node.arr);
		node.set.add(root);
		node.set.add(previous);
		node.set.add(node.map);
		node.arr[2].owner = node;
		previous.child = node;
		root.levels.push(node);
		previous = node;
	}

	previous.tail = root;
	root.tailSet = new Set([previous, root.levels[1], root.levels[4]]);
	root.index = new Map(root.levels.map(level => [level, { index: level.index, next: level.child || root }]));
	root.self = root;
	root.levels[2].cross = root.levels[5];
	root.levels[5].cross = root.levels[2];
	root.levels[3].arr.push(root.index);

	return root;
})()`

const aliasLatticeExpr = `(() => {
	const sharedLeaf = {
		tag: "leaf",
		when: new Date("2023-11-12T13:14:15.000Z"),
		rx: /leaf/m
	};
	const sharedBytes = new Uint8Array([1, 3, 3, 7, 9, 9]);
	const sharedMap = new Map();
	const sharedSet = new Set();

	const branchA = { name: "A", leaf: sharedLeaf, bag: [sharedLeaf, sharedBytes] };
	const branchB = { name: "B", leaf: sharedLeaf, bag: [sharedMap, sharedSet] };
	const branchC = { name: "C", peer: branchA };

	sharedMap.set("branchA", branchA);
	sharedMap.set("branchB", branchB);
	sharedMap.set(sharedLeaf, branchC);
	sharedSet.add(branchA);
	sharedSet.add(branchB);
	sharedSet.add(sharedMap);

	branchA.partner = branchB;
	branchB.partner = branchC;
	branchC.partner = branchA;
	branchA.bytes = sharedBytes.buffer;
	branchB.view = new Uint8Array(branchA.bytes, 1, 4);
	branchC.registry = new Map([
		["leaf", sharedLeaf],
		["set", sharedSet],
		["map", sharedMap]
	]);

	const root = {
		branches: [branchA, branchB, branchC, branchA],
		lookup: new Map([
			["a", branchA],
			["b", branchB],
			["c", branchC],
			["leaf", sharedLeaf]
		]),
		group: new Set([branchA, branchB, branchC, sharedLeaf]),
		leaf: sharedLeaf
	};

	root.self = root;
	sharedLeaf.root = root;
	sharedLeaf.aliases = [branchA, branchB, branchC, root.lookup];

	return root;
})()`

func TestQuickJSToGojaToQuickJS_RoundTripSnapshots(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{
			name: "PrimitivesAndUndefined",
			expr: `({
				undef: undefined,
				nil: null,
				bool: true,
				str: "hello",
				int: 42,
				float: 1.25,
				nan: NaN,
				posInf: Infinity,
				negInf: -Infinity,
				negZero: -0
			})`,
		},
		{
			name: "DateRegExpBigIntError",
			expr: `(() => {
				const err = new TypeError("boom");
				err.cause = new Error("root");
				return {
					when: new Date("2020-01-02T03:04:05.000Z"),
					rx: /a+b/gi,
					big: 1234567890123456789012345678901234567890n,
					err
				};
			})()`,
		},
		{
			name: "MapSetWithObjectKeys",
			expr: `(() => {
				const key = { role: "key" };
				const setValue = { nested: true };
				return {
					map: new Map([[key, { same: key }], ["plain", 7]]),
					set: new Set([key, setValue, "x"])
				};
			})()`,
		},
		{
			name: "ArrayBufferAndTypedArrayView",
			expr: `(() => {
				const buf = new ArrayBuffer(8);
				const full = new Uint8Array(buf);
				full.set([0, 1, 2, 3, 4, 5, 6, 7]);
				return {
					buf,
					view: new Uint16Array(buf, 2, 2)
				};
			})()`,
		},
		{
			name: "RepeatedReferencesAndCycles",
			expr: `(() => {
				const shared = { label: "shared" };
				const root = { left: shared, right: shared, list: [shared] };
				root.self = root;
				shared.owner = root;
				return root;
			})()`,
		},
		{
			name: "SparseArray",
			expr: `(() => {
				const arr = [];
				arr[1] = "x";
				arr[3] = "y";
				return arr;
			})()`,
		},
		{
			name: "DeepMixedCycleGraph",
			expr: deepMixedCycleExpr,
		},
		{
			name: "AliasLatticeWithCrossCycles",
			expr: aliasLatticeExpr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sourceQuick := mustNewQuickJS(t)
			defer sourceQuick.Close()

			originalQuick := evalQuickJS(t, sourceQuick, tt.expr)
			defer originalQuick.Free()
			wire1, err := EncodeQuickJS(originalQuick)
			if err != nil {
				t.Fatalf("EncodeQuickJS() error = %v", err)
			}

			snapshotQuickRuntime := mustNewQuickJS(t)
			defer snapshotQuickRuntime.Close()
			loadQuickJSSnapshot(t, snapshotQuickRuntime)
			snapshotQuick := evalQuickJS(t, snapshotQuickRuntime, tt.expr)
			originalSnapshot := snapshotQuickJSValue(t, snapshotQuickRuntime, snapshotQuick)
			snapshotQuick.Free()

			gojaRuntime := mustNewGoja(t)
			loadGojaSnapshot(t, gojaRuntime)

			gojaValue, err := DecodeGoja(gojaRuntime, wire1)
			if err != nil {
				t.Fatalf("DecodeGoja() error = %v", err)
			}

			gojaSnapshot := snapshotGojaValue(t, gojaRuntime, gojaValue)
			assertDeepEqual(t, "quickjs->goja snapshot", originalSnapshot, gojaSnapshot)

			wire2, err := EncodeGoja(gojaValue)
			if err != nil {
				t.Fatalf("EncodeGoja() error = %v", err)
			}

			finalQuickRuntime := mustNewQuickJS(t)
			defer finalQuickRuntime.Close()
			finalQuick, err := DecodeQuickJS(finalQuickRuntime.Context(), wire2)
			if err != nil {
				t.Fatalf("DecodeQuickJS() error = %v", err)
			}
			defer finalQuick.Free()

			loadQuickJSSnapshot(t, finalQuickRuntime)
			finalSnapshot := snapshotQuickJSValue(t, finalQuickRuntime, finalQuick)
			assertDeepEqual(t, "quickjs final snapshot", originalSnapshot, finalSnapshot)
		})
	}
}

func TestGojaToQuickJSToGoja_RoundTripSnapshots(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{
			name: "NestedObjectGraph",
			expr: `(() => {
				const shared = { score: 9 };
				return {
					header: { ok: true, code: 200 },
					body: [shared, { copy: shared }],
					meta: new Map([["seen", new Set([shared, "done"])]])
				};
			})()`,
		},
		{
			name: "BinaryAndDate",
			expr: `(() => {
				const buf = new Uint8Array([9, 8, 7, 6]).buffer;
				return {
					createdAt: new Date("2024-06-07T08:09:10.000Z"),
					buf,
					view: new Uint8Array(buf, 1, 2),
					big: 999999999999999999999n
				};
			})()`,
		},
		{
			name: "CyclicMapKeyGraph",
			expr: `(() => {
				const obj = { name: "node" };
				const map = new Map([[obj, "value"]]);
				obj.map = map;
				return { obj, map };
			})()`,
		},
		{
			name: "DeepMixedCycleGraph",
			expr: deepMixedCycleExpr,
		},
		{
			name: "AliasLatticeWithCrossCycles",
			expr: aliasLatticeExpr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gojaRuntime := mustNewGoja(t)
			loadGojaSnapshot(t, gojaRuntime)

			originalGoja := evalGoja(t, gojaRuntime, tt.expr)
			originalSnapshot := snapshotGojaValue(t, gojaRuntime, originalGoja)

			wire1, err := EncodeGoja(originalGoja)
			if err != nil {
				t.Fatalf("EncodeGoja() error = %v", err)
			}

			quickSnapshotRuntime := mustNewQuickJS(t)
			defer quickSnapshotRuntime.Close()
			quickSnapshotValue, err := DecodeQuickJS(quickSnapshotRuntime.Context(), wire1)
			if err != nil {
				t.Fatalf("DecodeQuickJS() error = %v", err)
			}
			defer quickSnapshotValue.Free()
			loadQuickJSSnapshot(t, quickSnapshotRuntime)

			quickSnapshot := snapshotQuickJSValue(t, quickSnapshotRuntime, quickSnapshotValue)
			assertDeepEqual(t, "goja->quickjs snapshot", originalSnapshot, quickSnapshot)

			quickEncodeRuntime := mustNewQuickJS(t)
			defer quickEncodeRuntime.Close()
			quickEncodeValue, err := DecodeQuickJS(quickEncodeRuntime.Context(), wire1)
			if err != nil {
				t.Fatalf("DecodeQuickJS() error = %v", err)
			}
			defer quickEncodeValue.Free()

			wire2, err := EncodeQuickJS(quickEncodeValue)
			if err != nil {
				t.Fatalf("EncodeQuickJS() error = %v", err)
			}

			finalGoja, err := DecodeGoja(gojaRuntime, wire2)
			if err != nil {
				t.Fatalf("DecodeGoja() error = %v", err)
			}

			finalSnapshot := snapshotGojaValue(t, gojaRuntime, finalGoja)
			assertDeepEqual(t, "goja final snapshot", originalSnapshot, finalSnapshot)
		})
	}
}

func TestEncodeQuickJS_UnsupportedValues(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{name: "FunctionValue", expr: `(function nope() {})`},
		{name: "SymbolValue", expr: `Symbol("sym")`},
		{name: "PromiseValue", expr: `({ p: new Promise(() => {}) })`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quickRuntime := mustNewQuickJS(t)
			defer quickRuntime.Close()

			value := evalQuickJS(t, quickRuntime, tt.expr)
			defer value.Free()

			if _, err := EncodeQuickJS(value); err == nil {
				t.Fatal("EncodeQuickJS() error = nil, want non-nil")
			}
		})
	}
}

func TestEncodeGoja_UnsupportedValues(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{name: "FunctionValue", expr: `(function nope() {})`},
		{name: "SymbolValue", expr: `Symbol("sym")`},
		{name: "PendingPromiseValue", expr: `new Promise(() => {})`},
		{name: "RejectedPromiseValue", expr: `Promise.reject(1)`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gojaRuntime := mustNewGoja(t)
			value := evalGoja(t, gojaRuntime, tt.expr)

			if _, err := EncodeGoja(value); err == nil {
				t.Fatal("EncodeGoja() error = nil, want non-nil")
			}
		})
	}
}

func TestEncodeGoja_FulfilledPromise(t *testing.T) {
	vm := mustNewGoja(t)
	loadGojaSnapshot(t, vm)
	value := evalGoja(t, vm, `Promise.resolve({ answer: 42 })`)

	wire, err := EncodeGoja(value)
	if err != nil {
		t.Fatalf("EncodeGoja() error = %v", err)
	}

	inspection, err := Describe(wire)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if got, want := inspection.Summary, `Promise<{answer: 42}>`; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}

	decoded, err := DecodeGoja(vm, wire)
	if err != nil {
		t.Fatalf("DecodeGoja() error = %v", err)
	}
	promise, ok := decoded.Export().(*goja.Promise)
	if !ok || promise == nil {
		t.Fatalf("decoded export = %T, want *goja.Promise", decoded.Export())
	}
	if promise.State() != goja.PromiseStateFulfilled {
		t.Fatalf("promise state = %v, want fulfilled", promise.State())
	}
	got := snapshotGojaValue(t, vm, promise.Result())
	want := snapshotGojaValue(t, vm, evalGoja(t, vm, `({ answer: 42 })`))
	assertDeepEqual(t, "fulfilled promise result", want, got)
}

func TestEncodeQuickJS_SkipsEnumerableMethodProps(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	value := evalQuickJS(t, quickRuntime, `({
		ok: true,
		status: 200,
		text() { return "body"; },
		json() { return { ok: true }; }
	})`)
	defer value.Free()

	wire, err := EncodeQuickJS(value)
	if err != nil {
		t.Fatalf("EncodeQuickJS() error = %v", err)
	}

	inspection, err := Describe(wire)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if got, want := inspection.Summary, `{ok: true, status: 200}`; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got, want := inspection.Full, `{ok: true, status: 200}`; got != want {
		t.Fatalf("full = %q, want %q", got, want)
	}
}

func TestEncodeQuickJS_ShowsCustomClassName(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	value := evalQuickJS(t, quickRuntime, `(() => {
		class FetchResult {
			constructor() {
				this.ok = true;
				this.status = 200;
			}
			text() { return "body"; }
		}
		return new FetchResult();
	})()`)
	defer value.Free()

	wire, err := EncodeQuickJS(value)
	if err != nil {
		t.Fatalf("EncodeQuickJS() error = %v", err)
	}

	inspection, err := Describe(wire)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if got, want := inspection.Summary, `FetchResult {ok: true, status: 200}`; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got, want := inspection.Full, `FetchResult {ok: true, status: 200}`; got != want {
		t.Fatalf("full = %q, want %q", got, want)
	}
}

func TestEncodeGoja_FulfilledPromiseSkipsEnumerableMethodProps(t *testing.T) {
	vm := mustNewGoja(t)
	loadGojaSnapshot(t, vm)
	value := evalGoja(t, vm, `Promise.resolve({
		ok: true,
		status: 200,
		text() { return "body"; },
		json() { return { ok: true }; }
	})`)

	wire, err := EncodeGoja(value)
	if err != nil {
		t.Fatalf("EncodeGoja() error = %v", err)
	}

	inspection, err := Describe(wire)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if got, want := inspection.Summary, `Promise<{ok: true, status: 200}>`; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got, want := inspection.Full, `Promise<{ok: true, status: 200}>`; got != want {
		t.Fatalf("full = %q, want %q", got, want)
	}

	decoded, err := DecodeGoja(vm, wire)
	if err != nil {
		t.Fatalf("DecodeGoja() error = %v", err)
	}
	promise, ok := decoded.Export().(*goja.Promise)
	if !ok || promise == nil {
		t.Fatalf("decoded export = %T, want *goja.Promise", decoded.Export())
	}
	if promise.State() != goja.PromiseStateFulfilled {
		t.Fatalf("promise state = %v, want fulfilled", promise.State())
	}
	got := snapshotGojaValue(t, vm, promise.Result())
	want := snapshotGojaValue(t, vm, evalGoja(t, vm, `({ ok: true, status: 200 })`))
	assertDeepEqual(t, "fulfilled promise result without methods", want, got)
}

func TestWireGraphTypedArrayRoundTrip(t *testing.T) {
	want := wireGraph{
		Root: wireValue{Kind: valueRef, Ref: 1},
		Nodes: []wireNode{
			{
				ID:    1,
				Kind:  nodeTypedArray,
				TextA: "Uint16Array",
				Buffer: wireValue{
					Kind: valueRef,
					Ref:  2,
				},
				ByteOffset: 2,
				ByteLength: 4,
			},
			{
				ID:    2,
				Kind:  nodeArrayBuffer,
				Bytes: []byte{0, 1, 2, 3, 4, 5, 6, 7},
			},
		},
	}

	got, err := unmarshalGraph(marshalGraph(want))
	if err != nil {
		t.Fatalf("unmarshalGraph(marshalGraph(...)) error = %v", err)
	}

	assertDeepEqual(t, "typed array wire graph", want, got)
}

func TestEncodeQuickJSTypedArrayWire(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	value := evalQuickJS(t, quickRuntime, `(() => {
		const buf = new ArrayBuffer(8);
		const full = new Uint8Array(buf);
		full.set([0, 1, 2, 3, 4, 5, 6, 7]);
		return { view: new Uint16Array(buf, 2, 2) };
	})()`)
	defer value.Free()

	wire, err := EncodeQuickJS(value)
	if err != nil {
		t.Fatalf("EncodeQuickJS() error = %v", err)
	}

	graph, err := unmarshalGraph(wire)
	if err != nil {
		t.Fatalf("unmarshalGraph() error = %v", err)
	}

	if len(graph.Nodes) != 3 {
		t.Fatalf("len(graph.Nodes) = %d, want 3", len(graph.Nodes))
	}

	viewNode := graph.Nodes[1]
	if viewNode.Kind != nodeTypedArray {
		t.Fatalf("view node kind = %v, want nodeTypedArray", viewNode.Kind)
	}
	if viewNode.Buffer.Kind != valueRef || viewNode.Buffer.Ref != 3 {
		t.Fatalf("view node buffer = %#v, want ref 3", viewNode.Buffer)
	}
	if viewNode.ByteOffset != 2 || viewNode.ByteLength != 4 || viewNode.TextA != "Uint16Array" {
		t.Fatalf("view node = %#v", viewNode)
	}

	bufferNode := graph.Nodes[2]
	if bufferNode.Kind != nodeArrayBuffer {
		t.Fatalf("buffer node kind = %v, want nodeArrayBuffer", bufferNode.Kind)
	}
	if !reflect.DeepEqual(bufferNode.Bytes, []byte{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("buffer node bytes = %x, want 0001020304050607", bufferNode.Bytes)
	}
}

func TestEncodeQuickJSBufferAndTypedArrayWire(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	value := evalQuickJS(t, quickRuntime, `(() => {
		const buf = new ArrayBuffer(8);
		const full = new Uint8Array(buf);
		full.set([0, 1, 2, 3, 4, 5, 6, 7]);
		return { buf, view: new Uint16Array(buf, 2, 2) };
	})()`)
	defer value.Free()

	wire, err := EncodeQuickJS(value)
	if err != nil {
		t.Fatalf("EncodeQuickJS() error = %v", err)
	}

	graph, err := unmarshalGraph(wire)
	if err != nil {
		t.Fatalf("unmarshalGraph() error = %v", err)
	}

	if len(graph.Nodes) != 3 {
		t.Fatalf("len(graph.Nodes) = %d, want 3", len(graph.Nodes))
	}

	var (
		arrayBufferNode wireNode
		typedArrayNode  wireNode
	)
	for _, node := range graph.Nodes {
		switch node.Kind {
		case nodeArrayBuffer:
			arrayBufferNode = node
		case nodeTypedArray:
			typedArrayNode = node
		}
	}
	if arrayBufferNode.Kind != nodeArrayBuffer {
		t.Fatalf("missing nodeArrayBuffer in %#v", graph.Nodes)
	}
	if typedArrayNode.Kind != nodeTypedArray {
		t.Fatalf("missing nodeTypedArray in %#v", graph.Nodes)
	}
	if !reflect.DeepEqual(arrayBufferNode.Bytes, []byte{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("arrayBuffer bytes = %x, want 0001020304050607", arrayBufferNode.Bytes)
	}
	if typedArrayNode.Buffer.Kind != valueRef || typedArrayNode.Buffer.Ref != arrayBufferNode.ID {
		t.Fatalf("typedArray buffer = %#v, want ref %d", typedArrayNode.Buffer, arrayBufferNode.ID)
	}
}

func TestGojaDecoderAllocateTypedArray(t *testing.T) {
	vm := goja.New()
	dec := gojaDecoder{
		vm:   vm,
		refs: make(map[uint32]goja.Value),
	}
	nodes := []wireNode{
		{
			ID:    2,
			Kind:  nodeArrayBuffer,
			Bytes: []byte{0, 1, 2, 3, 4, 5, 6, 7},
		},
		{
			ID:    1,
			Kind:  nodeTypedArray,
			TextA: "Uint16Array",
			Buffer: wireValue{
				Kind: valueRef,
				Ref:  2,
			},
			ByteOffset: 2,
			ByteLength: 4,
		},
	}
	if err := dec.allocate(nodes); err != nil {
		t.Fatalf("allocate() error = %v", err)
	}
	if err := vm.GlobalObject().Set("view", dec.refs[1]); err != nil {
		t.Fatalf("set view: %v", err)
	}
	got := evalGoja(t, vm, `({
		hex: Array.from(new Uint8Array(view.buffer, view.byteOffset, view.byteLength)).map(x => x.toString(16).padStart(2, "0")).join(""),
		ctor: view.constructor.name,
		offset: view.byteOffset,
		length: view.byteLength
	})`)
	assertDeepEqual(t, "allocated goja typedarray", map[string]any{
		"hex":    "02030405",
		"ctor":   "Uint16Array",
		"offset": int64(2),
		"length": int64(4),
	}, got.Export())
}

func TestGojaDecoderFillTypedArrayObjectGraph(t *testing.T) {
	vm := goja.New()
	dec := gojaDecoder{
		vm:   vm,
		refs: make(map[uint32]goja.Value),
	}
	nodes := []wireNode{
		{
			ID:   1,
			Kind: nodeObject,
			Props: []wireProp{
				{Key: "buf", Value: wireValue{Kind: valueRef, Ref: 2}},
				{Key: "view", Value: wireValue{Kind: valueRef, Ref: 3}},
			},
		},
		{
			ID:    2,
			Kind:  nodeArrayBuffer,
			Bytes: []byte{0, 1, 2, 3, 4, 5, 6, 7},
		},
		{
			ID:         3,
			Kind:       nodeTypedArray,
			TextA:      "Uint16Array",
			Buffer:     wireValue{Kind: valueRef, Ref: 2},
			ByteOffset: 2,
			ByteLength: 4,
		},
	}
	if err := dec.allocate(nodes); err != nil {
		t.Fatalf("allocate() error = %v", err)
	}
	if err := dec.fill(nodes); err != nil {
		t.Fatalf("fill() error = %v", err)
	}
	if err := vm.GlobalObject().Set("root", dec.refs[1]); err != nil {
		t.Fatalf("set root: %v", err)
	}
	got := evalGoja(t, vm, `({
		buf: Array.from(new Uint8Array(root.buf)).map(x => x.toString(16).padStart(2, "0")).join(""),
		view: Array.from(new Uint8Array(root.view.buffer, root.view.byteOffset, root.view.byteLength)).map(x => x.toString(16).padStart(2, "0")).join(""),
		ctor: root.view.constructor.name,
		offset: root.view.byteOffset,
		length: root.view.byteLength
	})`)
	assertDeepEqual(t, "filled goja typedarray graph", map[string]any{
		"buf":    "0001020304050607",
		"view":   "02030405",
		"ctor":   "Uint16Array",
		"offset": int64(2),
		"length": int64(4),
	}, got.Export())
}

func TestQuickJSToGoja_PreservesSharedArrayBufferAlias(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	value := evalQuickJS(t, quickRuntime, `(() => {
		const buf = new ArrayBuffer(6);
		const full = new Uint8Array(buf);
		full.set([1, 2, 3, 4, 5, 6]);
		return {
			buf,
			left: new Uint8Array(buf, 1, 3),
			right: new Uint8Array(buf, 2, 2)
		};
	})()`)
	defer value.Free()

	wire, err := EncodeQuickJS(value)
	if err != nil {
		t.Fatalf("EncodeQuickJS() error = %v", err)
	}

	vm := goja.New()
	root, err := DecodeGoja(vm, wire)
	if err != nil {
		t.Fatalf("DecodeGoja() error = %v", err)
	}
	if err := vm.GlobalObject().Set("root", root); err != nil {
		t.Fatalf("set root: %v", err)
	}

	got := evalGoja(t, vm, `(() => {
		root.left[1] = 99;
		return {
			buf: Array.from(new Uint8Array(root.buf)),
			left: Array.from(root.left),
			right: Array.from(root.right),
			sameBuffer: root.left.buffer === root.right.buffer && root.left.buffer === root.buf
		};
	})()`)
	assertDeepEqual(t, "quickjs->goja shared arraybuffer alias", map[string]any{
		"buf":        []any{int64(1), int64(2), int64(99), int64(4), int64(5), int64(6)},
		"left":       []any{int64(2), int64(99), int64(4)},
		"right":      []any{int64(99), int64(4)},
		"sameBuffer": true,
	}, got.Export())
}

func TestGojaToQuickJS_PreservesSharedArrayBufferAlias(t *testing.T) {
	vm := goja.New()
	root := evalGoja(t, vm, `(() => {
		const buf = new ArrayBuffer(6);
		const full = new Uint8Array(buf);
		full.set([1, 2, 3, 4, 5, 6]);
		return {
			buf,
			left: new Uint8Array(buf, 1, 3),
			right: new Uint8Array(buf, 2, 2)
		};
	})()`)

	wire, err := EncodeGoja(root)
	if err != nil {
		t.Fatalf("EncodeGoja() error = %v", err)
	}

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	quickRoot, err := DecodeQuickJS(quickRuntime.Context(), wire)
	if err != nil {
		t.Fatalf("DecodeQuickJS() error = %v", err)
	}
	defer quickRoot.Free()

	quickRuntime.Context().Global().SetPropertyStr("root", quickRoot)
	got := evalQuickJS(t, quickRuntime, `JSON.stringify((() => {
		root.left[1] = 99;
		return {
			buf: Array.from(new Uint8Array(root.buf)),
			left: Array.from(root.left),
			right: Array.from(root.right),
			sameBuffer: root.left.buffer === root.right.buffer && root.left.buffer === root.buf
		};
	})())`)
	defer got.Free()

	var decoded any
	if err := json.Unmarshal([]byte(got.String()), &decoded); err != nil {
		t.Fatalf("quickjs alias json unmarshal: %v", err)
	}

	assertDeepEqual(t, "goja->quickjs shared arraybuffer alias", map[string]any{
		"buf":        []any{float64(1), float64(2), float64(99), float64(4), float64(5), float64(6)},
		"left":       []any{float64(2), float64(99), float64(4)},
		"right":      []any{float64(99), float64(4)},
		"sameBuffer": true,
	}, decoded)
}

func TestQuickJSToGoja_ObjectPropertiesUseEnumerableStringKeysOnly(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	value := evalQuickJS(t, quickRuntime, `(() => {
		const sym = Symbol("secret");
		const root = { visible: 1 };
		Object.defineProperty(root, "hidden", { value: 2, enumerable: false });
		root[sym] = 3;
		return root;
	})()`)
	defer value.Free()

	wire, err := EncodeQuickJS(value)
	if err != nil {
		t.Fatalf("EncodeQuickJS() error = %v", err)
	}

	vm := goja.New()
	root, err := DecodeGoja(vm, wire)
	if err != nil {
		t.Fatalf("DecodeGoja() error = %v", err)
	}
	if err := vm.GlobalObject().Set("root", root); err != nil {
		t.Fatalf("set root: %v", err)
	}

	got := evalGoja(t, vm, `({
		keys: Object.keys(root),
		hasHidden: Object.prototype.hasOwnProperty.call(root, "hidden"),
		symbolCount: Object.getOwnPropertySymbols(root).length,
		visible: root.visible
	})`)
	assertDeepEqual(t, "quickjs->goja enumerable string props", map[string]any{
		"keys":        []any{"visible"},
		"hasHidden":   false,
		"symbolCount": int64(0),
		"visible":     int64(1),
	}, got.Export())
}

func TestGojaToQuickJS_ObjectPropertiesUseEnumerableStringKeysOnly(t *testing.T) {
	vm := goja.New()
	root := evalGoja(t, vm, `(() => {
		const sym = Symbol("secret");
		const root = { visible: 1 };
		Object.defineProperty(root, "hidden", { value: 2, enumerable: false });
		root[sym] = 3;
		return root;
	})()`)

	wire, err := EncodeGoja(root)
	if err != nil {
		t.Fatalf("EncodeGoja() error = %v", err)
	}

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	quickRoot, err := DecodeQuickJS(quickRuntime.Context(), wire)
	if err != nil {
		t.Fatalf("DecodeQuickJS() error = %v", err)
	}
	defer quickRoot.Free()

	quickRuntime.Context().Global().SetPropertyStr("root", quickRoot)
	got := evalQuickJS(t, quickRuntime, `JSON.stringify({
		keys: Object.keys(root),
		hasHidden: Object.prototype.hasOwnProperty.call(root, "hidden"),
		symbolCount: Object.getOwnPropertySymbols(root).length,
		visible: root.visible
	})`)
	defer got.Free()

	var decoded any
	if err := json.Unmarshal([]byte(got.String()), &decoded); err != nil {
		t.Fatalf("quickjs enumerable props json unmarshal: %v", err)
	}

	assertDeepEqual(t, "goja->quickjs enumerable string props", map[string]any{
		"keys":        []any{"visible"},
		"hasHidden":   false,
		"symbolCount": float64(0),
		"visible":     float64(1),
	}, decoded)
}

func TestEncodeQuickJS_RejectsEnumerableAccessorProperties(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	value := evalQuickJS(t, quickRuntime, `(() => {
		const root = {};
		Object.defineProperty(root, "boom", {
			enumerable: true,
			get() { return 1; }
		});
		return root;
	})()`)
	defer value.Free()

	if _, err := EncodeQuickJS(value); err == nil {
		t.Fatal("EncodeQuickJS() error = nil, want accessor rejection")
	}
}

func TestEncodeQuickJS_IgnoresNonEnumerableAccessorProperties(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	value := evalQuickJS(t, quickRuntime, `(() => {
		const root = { visible: 1 };
		Object.defineProperty(root, "hidden", {
			enumerable: false,
			get() { throw new Error("should not run"); }
		});
		return root;
	})()`)
	defer value.Free()

	wire, err := EncodeQuickJS(value)
	if err != nil {
		t.Fatalf("EncodeQuickJS() error = %v", err)
	}

	vm := goja.New()
	root, err := DecodeGoja(vm, wire)
	if err != nil {
		t.Fatalf("DecodeGoja() error = %v", err)
	}
	if err := vm.GlobalObject().Set("root", root); err != nil {
		t.Fatalf("set root: %v", err)
	}
	got := evalGoja(t, vm, `({
		keys: Object.keys(root),
		hasHidden: Object.prototype.hasOwnProperty.call(root, "hidden"),
		visible: root.visible
	})`)
	assertDeepEqual(t, "quickjs non-enumerable accessor ignored", map[string]any{
		"keys":      []any{"visible"},
		"hasHidden": false,
		"visible":   int64(1),
	}, got.Export())
}

func TestEncodeGoja_RejectsEnumerableAccessorProperties(t *testing.T) {
	vm := goja.New()
	root := evalGoja(t, vm, `(() => {
		const root = {};
		Object.defineProperty(root, "boom", {
			enumerable: true,
			get() { return 1; }
		});
		return root;
	})()`)

	if _, err := EncodeGoja(root); err == nil {
		t.Fatal("EncodeGoja() error = nil, want accessor rejection")
	}
}

func TestEncodeGoja_IgnoresNonEnumerableAccessorProperties(t *testing.T) {
	vm := goja.New()
	root := evalGoja(t, vm, `(() => {
		const root = { visible: 1 };
		Object.defineProperty(root, "hidden", {
			enumerable: false,
			get() { throw new Error("should not run"); }
		});
		return root;
	})()`)

	wire, err := EncodeGoja(root)
	if err != nil {
		t.Fatalf("EncodeGoja() error = %v", err)
	}

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	quickRoot, err := DecodeQuickJS(quickRuntime.Context(), wire)
	if err != nil {
		t.Fatalf("DecodeQuickJS() error = %v", err)
	}
	defer quickRoot.Free()

	quickRuntime.Context().Global().SetPropertyStr("root", quickRoot)
	got := evalQuickJS(t, quickRuntime, `JSON.stringify({
		keys: Object.keys(root),
		hasHidden: Object.prototype.hasOwnProperty.call(root, "hidden"),
		visible: root.visible
	})`)
	defer got.Free()

	var decoded any
	if err := json.Unmarshal([]byte(got.String()), &decoded); err != nil {
		t.Fatalf("quickjs non-enumerable accessor json unmarshal: %v", err)
	}
	assertDeepEqual(t, "goja non-enumerable accessor ignored", map[string]any{
		"keys":      []any{"visible"},
		"hasHidden": false,
		"visible":   float64(1),
	}, decoded)
}

func TestEncodeQuickJS_NullPrototypeDictionaryPreservesDataProps(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	value := evalQuickJS(t, quickRuntime, `(() => {
		const root = Object.create(null);
		root.visible = 1;
		return root;
	})()`)
	defer value.Free()

	wire, err := EncodeQuickJS(value)
	if err != nil {
		t.Fatalf("EncodeQuickJS() error = %v", err)
	}

	vm := goja.New()
	root, err := DecodeGoja(vm, wire)
	if err != nil {
		t.Fatalf("DecodeGoja() error = %v", err)
	}
	if err := vm.GlobalObject().Set("root", root); err != nil {
		t.Fatalf("set root: %v", err)
	}

	got := evalGoja(t, vm, `({
		keys: Object.keys(root),
		visible: root.visible
	})`)
	assertDeepEqual(t, "quickjs null-prototype dictionary", map[string]any{
		"keys":    []any{"visible"},
		"visible": int64(1),
	}, got.Export())
}

func TestGojaToQuickJS_CustomErrorFallsBackToErrorConstructor(t *testing.T) {
	vm := goja.New()
	value := evalGoja(t, vm, `(() => {
		class MyError extends Error {
			constructor(message) {
				super(message);
				this.name = "MyError";
			}
		}
		return new MyError("boom");
	})()`)

	wire, err := EncodeGoja(value)
	if err != nil {
		t.Fatalf("EncodeGoja() error = %v", err)
	}

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	root, err := DecodeQuickJS(quickRuntime.Context(), wire)
	if err != nil {
		t.Fatalf("DecodeQuickJS() error = %v", err)
	}
	defer root.Free()

	fn, err := quickRuntime.Context().Eval("inspect-error.js", qjs.Code(`(function(root) {
		return JSON.stringify({
			isError: root instanceof Error,
			name: root.name,
			message: root.message
		});
	})`))
	if err != nil {
		t.Fatalf("quickjs inspect helper: %v", err)
	}
	defer fn.Free()
	global := quickRuntime.Context().Global()
	defer global.Free()
	got, err := quickRuntime.Context().Invoke(fn, global, root.Clone())
	if err != nil {
		t.Fatalf("quickjs inspect invoke: %v", err)
	}
	defer got.Free()

	var decoded any
	if err := json.Unmarshal([]byte(got.String()), &decoded); err != nil {
		t.Fatalf("custom error json unmarshal: %v", err)
	}
	assertDeepEqual(t, "goja->quickjs custom error fallback", map[string]any{
		"isError": true,
		"name":    "MyError",
		"message": "boom",
	}, decoded)
}

func TestQuickJSToGoja_ArrayPreservesOwnEnumerableProps(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	value := evalQuickJS(t, quickRuntime, `(() => {
		const root = [1, 2];
		root.meta = 7;
		return root;
	})()`)
	defer value.Free()

	wire, err := EncodeQuickJS(value)
	if err != nil {
		t.Fatalf("EncodeQuickJS() error = %v", err)
	}

	vm := goja.New()
	root, err := DecodeGoja(vm, wire)
	if err != nil {
		t.Fatalf("DecodeGoja() error = %v", err)
	}
	if err := vm.GlobalObject().Set("root", root); err != nil {
		t.Fatalf("set root: %v", err)
	}

	got := evalGoja(t, vm, `({
		length: root.length,
		first: root[0],
		meta: root.meta,
		keys: Object.keys(root)
	})`)
	assertDeepEqual(t, "quickjs->goja array own props", map[string]any{
		"length": int64(2),
		"first":  int64(1),
		"meta":   int64(7),
		"keys":   []any{"0", "1", "meta"},
	}, got.Export())
}

func TestGojaToQuickJS_RegExpPreservesOwnEnumerableProps(t *testing.T) {
	vm := goja.New()
	value := evalGoja(t, vm, `(() => {
		const root = /x/gi;
		root.label = "y";
		return root;
	})()`)

	wire, err := EncodeGoja(value)
	if err != nil {
		t.Fatalf("EncodeGoja() error = %v", err)
	}

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	root, err := DecodeQuickJS(quickRuntime.Context(), wire)
	if err != nil {
		t.Fatalf("DecodeQuickJS() error = %v", err)
	}
	defer root.Free()

	quickRuntime.Context().Global().SetPropertyStr("root", root)
	got := evalQuickJS(t, quickRuntime, `JSON.stringify({
		source: root.source,
		flags: root.flags,
		label: root.label,
		keys: Object.keys(root)
	})`)
	defer got.Free()

	var decoded any
	if err := json.Unmarshal([]byte(got.String()), &decoded); err != nil {
		t.Fatalf("regexp props json unmarshal: %v", err)
	}
	assertDeepEqual(t, "goja->quickjs regexp own props", map[string]any{
		"source": "x",
		"flags":  "gi",
		"label":  "y",
		"keys":   []any{"label"},
	}, decoded)
}

func TestQuickDecoder_ReleasesNonRootRefsAfterDecode(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	nodes := []wireNode{
		{
			ID:   1,
			Kind: nodeObject,
			Props: []wireProp{
				{Key: "child", Value: wireValue{Kind: valueRef, Ref: 2}},
			},
		},
		{
			ID:   2,
			Kind: nodeObject,
			Props: []wireProp{
				{Key: "value", Value: wireValue{Kind: valueNumber, Number: 1}},
			},
		},
	}

	dec := quickDecoder{
		ctx:  quickRuntime.Context(),
		refs: make(map[uint32]*qjs.Value, len(nodes)),
	}
	if err := dec.allocate(nodes); err != nil {
		t.Fatalf("allocate() error = %v", err)
	}
	if err := dec.fill(nodes); err != nil {
		t.Fatalf("fill() error = %v", err)
	}

	root, err := dec.decodeRoot(wireValue{Kind: valueRef, Ref: 1})
	if err != nil {
		t.Fatalf("decodeRoot() error = %v", err)
	}

	if dec.refs[2].Raw() != 0 {
		t.Fatalf("child ref raw = %d after decodeRoot(), want 0", dec.refs[2].Raw())
	}
	root.Free()
}

func TestEncodeGoja_DoesNotTrustSpoofedConstructorName(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{
			name: "FakeMap",
			expr: `(() => ({
				constructor: { name: "Map" },
				entries: 123,
				visible: 1
			}))()`,
		},
		{
			name: "FakePromise",
			expr: `(() => ({
				constructor: { name: "Promise" },
				then: "not callable",
				visible: 1
			}))()`,
		},
		{
			name: "FakeDate",
			expr: `(() => ({
				constructor: { name: "Date" },
				getTime: 123,
				visible: 1
			}))()`,
		},
		{
			name: "FakeTypedArray",
			expr: `(() => ({
				constructor: { name: "Uint8Array" },
				byteOffset: 0,
				byteLength: 4,
				buffer: new ArrayBuffer(4),
				visible: 1
			}))()`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vm := goja.New()
			value := evalGoja(t, vm, tt.expr)

			wire, err := EncodeGoja(value)
			if err != nil {
				t.Fatalf("EncodeGoja() error = %v", err)
			}

			graph, err := unmarshalGraph(wire)
			if err != nil {
				t.Fatalf("unmarshalGraph() error = %v", err)
			}

			if len(graph.Nodes) == 0 {
				t.Fatal("graph.Nodes empty")
			}
			if graph.Nodes[0].Kind != nodeObject {
				t.Fatalf("root node kind = %v, want nodeObject", graph.Nodes[0].Kind)
			}
		})
	}
}

func TestEncodeGoja_DoesNotDependOnObjectKeysMonkeypatch(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(`Object.keys = () => ["fake"]`); err != nil {
		t.Fatalf("patch Object.keys: %v", err)
	}
	value := evalGoja(t, vm, `({ a: 1 })`)

	wire, err := EncodeGoja(value)
	if err != nil {
		t.Fatalf("EncodeGoja() error = %v", err)
	}

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	root, err := DecodeQuickJS(quickRuntime.Context(), wire)
	if err != nil {
		t.Fatalf("DecodeQuickJS() error = %v", err)
	}
	defer root.Free()

	quickRuntime.Context().Global().SetPropertyStr("root", root)
	got := evalQuickJS(t, quickRuntime, `JSON.stringify({
		keys: Object.keys(root),
		a: root.a,
		hasFake: Object.prototype.hasOwnProperty.call(root, "fake"),
		bridgeKeysTargetType: typeof globalThis.__bridgeKeysTarget,
		bridgeAccessorTargetType: typeof globalThis.__bridgeAccessorTarget,
		bridgeAccessorKeyType: typeof globalThis.__bridgeAccessorKey
	})`)
	defer got.Free()

	var decoded any
	if err := json.Unmarshal([]byte(got.String()), &decoded); err != nil {
		t.Fatalf("goja Object.keys monkeypatch json unmarshal: %v", err)
	}
	assertDeepEqual(t, "goja monkeypatched Object.keys", map[string]any{
		"keys":                     []any{"a"},
		"a":                        float64(1),
		"hasFake":                  false,
		"bridgeKeysTargetType":     "undefined",
		"bridgeAccessorTargetType": "undefined",
		"bridgeAccessorKeyType":    "undefined",
	}, decoded)
}

func TestGojaToQuickJS_BuiltinErrorPreservesInstanceof(t *testing.T) {
	vm := goja.New()
	value := evalGoja(t, vm, `new TypeError("boom")`)

	wire, err := EncodeGoja(value)
	if err != nil {
		t.Fatalf("EncodeGoja() error = %v", err)
	}

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	root, err := DecodeQuickJS(quickRuntime.Context(), wire)
	if err != nil {
		t.Fatalf("DecodeQuickJS() error = %v", err)
	}
	defer root.Free()

	fn, err := quickRuntime.Context().Eval("inspect-typeerror.js", qjs.Code(`(function(root) {
		return JSON.stringify({
			isError: root instanceof Error,
			isTypeError: root instanceof TypeError,
			name: root.name,
			message: root.message
		});
	})`))
	if err != nil {
		t.Fatalf("quickjs inspect helper: %v", err)
	}
	defer fn.Free()
	global := quickRuntime.Context().Global()
	defer global.Free()
	got, err := quickRuntime.Context().Invoke(fn, global, root.Clone())
	if err != nil {
		t.Fatalf("quickjs inspect invoke: %v", err)
	}
	defer got.Free()

	var decoded any
	if err := json.Unmarshal([]byte(got.String()), &decoded); err != nil {
		t.Fatalf("builtin error json unmarshal: %v", err)
	}
	assertDeepEqual(t, "goja->quickjs builtin error", map[string]any{
		"isError":     true,
		"isTypeError": true,
		"name":        "TypeError",
		"message":     "boom",
	}, decoded)
}

func TestEncodeQuickJS_IgnoresSpoofedSymbolToStringTag(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()

	value := evalQuickJS(t, quickRuntime, `(() => {
		const root = { visible: 1 };
		Object.defineProperty(root, Symbol.toStringTag, {
			get() { throw new Error("should not run"); }
		});
		return root;
	})()`)
	defer value.Free()

	wire, err := EncodeQuickJS(value)
	if err != nil {
		t.Fatalf("EncodeQuickJS() error = %v", err)
	}

	graph, err := unmarshalGraph(wire)
	if err != nil {
		t.Fatalf("unmarshalGraph() error = %v", err)
	}
	if len(graph.Nodes) == 0 {
		t.Fatal("graph.Nodes empty")
	}
	if graph.Nodes[0].Kind != nodeObject {
		t.Fatalf("root node kind = %v, want nodeObject", graph.Nodes[0].Kind)
	}
}

func TestDecodeRejectsUnsupportedVersion(t *testing.T) {
	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	originalQuick := evalQuickJS(t, quickRuntime, `({ ok: true })`)
	defer originalQuick.Free()

	wire, err := EncodeQuickJS(originalQuick)
	if err != nil {
		t.Fatalf("EncodeQuickJS() error = %v", err)
	}
	wire[0]++

	if _, err := DecodeQuickJS(quickRuntime.Context(), wire); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("DecodeQuickJS() error = %v, want %v", err, ErrUnsupportedVersion)
	}

	gojaRuntime := mustNewGoja(t)
	if _, err := DecodeGoja(gojaRuntime, wire); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("DecodeGoja() error = %v, want %v", err, ErrUnsupportedVersion)
	}
}

func TestDecodeRejectsDanglingRef(t *testing.T) {
	wire := marshalGraph(wireGraph{
		Root: wireValue{Kind: valueRef, Ref: 99},
	})

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	if _, err := DecodeQuickJS(quickRuntime.Context(), wire); !errors.Is(err, ErrInvalidWire) {
		t.Fatalf("DecodeQuickJS() error = %v, want %v", err, ErrInvalidWire)
	}

	gojaRuntime := mustNewGoja(t)
	if _, err := DecodeGoja(gojaRuntime, wire); !errors.Is(err, ErrInvalidWire) {
		t.Fatalf("DecodeGoja() error = %v, want %v", err, ErrInvalidWire)
	}
}

func TestDecodeRejectsDuplicateNodeIDs(t *testing.T) {
	wire := marshalGraph(wireGraph{
		Root: wireValue{Kind: valueRef, Ref: 1},
		Nodes: []wireNode{
			{ID: 1, Kind: nodeObject},
			{ID: 1, Kind: nodeArrayBuffer, Bytes: []byte{1}},
		},
	})

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	if _, err := DecodeQuickJS(quickRuntime.Context(), wire); !errors.Is(err, ErrInvalidWire) {
		t.Fatalf("DecodeQuickJS() error = %v, want %v", err, ErrInvalidWire)
	}

	gojaRuntime := mustNewGoja(t)
	if _, err := DecodeGoja(gojaRuntime, wire); !errors.Is(err, ErrInvalidWire) {
		t.Fatalf("DecodeGoja() error = %v, want %v", err, ErrInvalidWire)
	}
}

func TestDecodeRejectsUnknownTypedArrayCtor(t *testing.T) {
	wire := marshalGraph(wireGraph{
		Root: wireValue{Kind: valueRef, Ref: 1},
		Nodes: []wireNode{
			{
				ID:         1,
				Kind:       nodeTypedArray,
				TextA:      "NopeArray",
				Buffer:     wireValue{Kind: valueRef, Ref: 2},
				ByteOffset: 0,
				ByteLength: 4,
			},
			{ID: 2, Kind: nodeArrayBuffer, Bytes: []byte{1, 2, 3, 4}},
		},
	})

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	if _, err := DecodeQuickJS(quickRuntime.Context(), wire); !errors.Is(err, ErrInvalidWire) {
		t.Fatalf("DecodeQuickJS() error = %v, want %v", err, ErrInvalidWire)
	}

	gojaRuntime := mustNewGoja(t)
	if _, err := DecodeGoja(gojaRuntime, wire); !errors.Is(err, ErrInvalidWire) {
		t.Fatalf("DecodeGoja() error = %v, want %v", err, ErrInvalidWire)
	}
}

func TestDecodeRejectsInvalidTypedArrayShape(t *testing.T) {
	wire := marshalGraph(wireGraph{
		Root: wireValue{Kind: valueRef, Ref: 1},
		Nodes: []wireNode{
			{
				ID:         1,
				Kind:       nodeTypedArray,
				TextA:      "Uint16Array",
				Buffer:     wireValue{Kind: valueRef, Ref: 2},
				ByteOffset: 1,
				ByteLength: 3,
			},
			{ID: 2, Kind: nodeArrayBuffer, Bytes: []byte{1, 2, 3, 4}},
		},
	})

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	if _, err := DecodeQuickJS(quickRuntime.Context(), wire); !errors.Is(err, ErrInvalidWire) {
		t.Fatalf("DecodeQuickJS() error = %v, want %v", err, ErrInvalidWire)
	}

	gojaRuntime := mustNewGoja(t)
	if _, err := DecodeGoja(gojaRuntime, wire); !errors.Is(err, ErrInvalidWire) {
		t.Fatalf("DecodeGoja() error = %v, want %v", err, ErrInvalidWire)
	}
}

func TestDecodeRejectsOversizedRefID(t *testing.T) {
	root := make([]byte, 0, 16)
	root = append(root, wireVersion)
	root = append(root, byte(valueRef))
	refBuf := make([]byte, varint.Uint64.Size(uint64(math.MaxUint32)+1))
	refN := varint.Uint64.Marshal(uint64(math.MaxUint32)+1, refBuf)
	root = append(root, refBuf[:refN]...)
	lenBuf := make([]byte, varint.PositiveInt64.Size(0))
	lenN := varint.PositiveInt64.Marshal(0, lenBuf)
	root = append(root, lenBuf[:lenN]...)

	quickRuntime := mustNewQuickJS(t)
	defer quickRuntime.Close()
	if _, err := DecodeQuickJS(quickRuntime.Context(), root); !errors.Is(err, ErrInvalidWire) {
		t.Fatalf("DecodeQuickJS() error = %v, want %v", err, ErrInvalidWire)
	}

	gojaRuntime := mustNewGoja(t)
	if _, err := DecodeGoja(gojaRuntime, root); !errors.Is(err, ErrInvalidWire) {
		t.Fatalf("DecodeGoja() error = %v, want %v", err, ErrInvalidWire)
	}
}

func mustNewQuickJS(t *testing.T) *qjs.Runtime {
	t.Helper()
	rt, err := qjs.New()
	if err != nil {
		t.Fatalf("qjs.New() error = %v", err)
	}
	return rt
}

func mustNewGoja(t *testing.T) *goja.Runtime {
	t.Helper()
	return goja.New()
}

func loadQuickJSSnapshot(t *testing.T, rt *qjs.Runtime) {
	t.Helper()
	val, err := rt.Context().Eval("snapshot.js", qjs.Code(snapshotJS))
	if err != nil {
		t.Fatalf("quickjs load snapshot helper: %v", err)
	}
	val.Free()
}

func loadGojaSnapshot(t *testing.T, rt *goja.Runtime) {
	t.Helper()
	if _, err := rt.RunString(snapshotJS); err != nil {
		t.Fatalf("goja load snapshot helper: %v", err)
	}
}

func evalQuickJS(t *testing.T, rt *qjs.Runtime, expr string) *qjs.Value {
	t.Helper()
	val, err := rt.Context().Eval("value.js", qjs.Code("("+expr+")"))
	if err != nil {
		t.Fatalf("quickjs eval %q: %v", expr, err)
	}
	return val
}

func evalGoja(t *testing.T, rt *goja.Runtime, expr string) goja.Value {
	t.Helper()
	val, err := rt.RunString("(" + expr + ")")
	if err != nil {
		t.Fatalf("goja eval %q: %v", expr, err)
	}
	return val
}

func snapshotQuickJSValue(t *testing.T, rt *qjs.Runtime, value *qjs.Value) any {
	t.Helper()

	global := rt.Context().Global()
	defer global.Free()
	fn, err := rt.Context().Eval("snapshot-invoke.js", qjs.Code(`(function(value) {
		return globalThis.__bridgeSnapshotJSON(value);
	})`))
	if err != nil {
		t.Fatalf("quickjs snapshot wrapper eval: %v", err)
	}
	defer fn.Free()
	arg := value.Clone()
	defer arg.Free()
	snapshot, err := rt.Context().Invoke(fn, global, arg)
	if err != nil {
		t.Fatalf("quickjs snapshot invoke: %v", err)
	}
	defer snapshot.Free()

	raw := snapshot.String()
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("quickjs snapshot unmarshal: %v", err)
	}
	return out
}

func snapshotGojaValue(t *testing.T, rt *goja.Runtime, value goja.Value) any {
	t.Helper()

	if err := rt.GlobalObject().Set("__bridge_test_value", value); err != nil {
		t.Fatalf("goja set snapshot value: %v", err)
	}
	snapshot, err := rt.RunString(`__bridgeSnapshot(globalThis.__bridge_test_value)`)
	if err != nil {
		t.Fatalf("goja snapshot eval: %v", err)
	}
	return snapshot.Export()
}

func assertDeepEqual(t *testing.T, label string, want, got any) {
	t.Helper()
	if reflect.DeepEqual(want, got) {
		return
	}
	t.Fatalf("%s mismatch\nwant=%s\ngot=%s", label, prettyJSON(want), prettyJSON(got))
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "<unmarshalable>"
	}
	return string(b)
}

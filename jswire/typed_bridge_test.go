package jswire

import "testing"

const typedBridgeSupportedExpr = `(() => {
	const objectKey = { kind: "object-key", n: 1 };
	return {
		title: "typed-bridge",
		nil: null,
		bool: true,
		num: 123.5,
		when: new Date("2025-01-02T03:04:05.006Z"),
		rx: /bridge\s+typed/gi,
		big: -9007199254740993n,
		buf: new Uint8Array([1, 2, 3, 4]).buffer,
		u8: new Uint8Array([255, 1, 2]),
		u16: new Uint16Array([1, 256, 65535]),
		i32: new Int32Array([-2147483648, 0, 2147483647]),
		f64: new Float64Array([1.25, -2.5]),
		nested: {
			arr: [null, "x", 3],
			plain: { count: 2 }
		},
		map: new Map([
			["date", new Date(7)],
			[2n, /z+/g],
			[objectKey, new Set(["inner", 5n])],
			["bytes", new Uint8Array([7, 8, 9])]
		]),
		set: new Set([5n, new Date(8), /set/i, new Uint16Array([10, 11])])
	};
})()`

func TestBridgeRoundTripThroughTypedModel_SupportedComplexValue(t *testing.T) {
	t.Run("quickjs origin", func(t *testing.T) {
		sourceQuick := mustNewQuickJS(t)
		defer sourceQuick.Close()
		loadQuickJSSnapshot(t, sourceQuick)

		original := evalQuickJS(t, sourceQuick, typedBridgeSupportedExpr)
		defer original.Free()
		originalSnapshot := snapshotQuickJSValue(t, sourceQuick, original)

		wire, err := EncodeQuickJS(original)
		if err != nil {
			t.Fatalf("EncodeQuickJS() error = %v", err)
		}
		model, err := Decode(wire)
		if err != nil {
			t.Fatalf("Decode(quickjs wire) error = %v", err)
		}
		typedWire, err := Encode(model)
		if err != nil {
			t.Fatalf("Encode(typed model from quickjs) error = %v", err)
		}

		gojaRuntime := mustNewGoja(t)
		loadGojaSnapshot(t, gojaRuntime)
		gojaValue, err := DecodeGoja(gojaRuntime, typedWire)
		if err != nil {
			t.Fatalf("DecodeGoja(typed wire) error = %v", err)
		}
		gojaSnapshot := snapshotGojaValue(t, gojaRuntime, gojaValue)
		assertDeepEqual(t, "quickjs->typed->goja snapshot", originalSnapshot, gojaSnapshot)

		gojaWire, err := EncodeGoja(gojaValue)
		if err != nil {
			t.Fatalf("EncodeGoja(typed-decoded value) error = %v", err)
		}
		modelAgain, err := Decode(gojaWire)
		if err != nil {
			t.Fatalf("Decode(goja wire) error = %v", err)
		}
		typedWireAgain, err := Encode(modelAgain)
		if err != nil {
			t.Fatalf("Encode(typed model from goja) error = %v", err)
		}

		finalQuick := mustNewQuickJS(t)
		defer finalQuick.Close()
		loadQuickJSSnapshot(t, finalQuick)
		finalValue, err := DecodeQuickJS(finalQuick.Context(), typedWireAgain)
		if err != nil {
			t.Fatalf("DecodeQuickJS(final typed wire) error = %v", err)
		}
		defer finalValue.Free()
		finalSnapshot := snapshotQuickJSValue(t, finalQuick, finalValue)
		assertDeepEqual(t, "quickjs->typed->goja->typed->quickjs snapshot", originalSnapshot, finalSnapshot)
	})

	t.Run("goja origin", func(t *testing.T) {
		gojaRuntime := mustNewGoja(t)
		loadGojaSnapshot(t, gojaRuntime)

		original := evalGoja(t, gojaRuntime, typedBridgeSupportedExpr)
		originalSnapshot := snapshotGojaValue(t, gojaRuntime, original)

		wire, err := EncodeGoja(original)
		if err != nil {
			t.Fatalf("EncodeGoja() error = %v", err)
		}
		model, err := Decode(wire)
		if err != nil {
			t.Fatalf("Decode(goja wire) error = %v", err)
		}
		typedWire, err := Encode(model)
		if err != nil {
			t.Fatalf("Encode(typed model from goja) error = %v", err)
		}

		quickRuntime := mustNewQuickJS(t)
		defer quickRuntime.Close()
		loadQuickJSSnapshot(t, quickRuntime)
		quickValue, err := DecodeQuickJS(quickRuntime.Context(), typedWire)
		if err != nil {
			t.Fatalf("DecodeQuickJS(typed wire) error = %v", err)
		}
		defer quickValue.Free()
		quickSnapshot := snapshotQuickJSValue(t, quickRuntime, quickValue)
		assertDeepEqual(t, "goja->typed->quickjs snapshot", originalSnapshot, quickSnapshot)

		quickWire, err := EncodeQuickJS(quickValue)
		if err != nil {
			t.Fatalf("EncodeQuickJS(typed-decoded value) error = %v", err)
		}
		modelAgain, err := Decode(quickWire)
		if err != nil {
			t.Fatalf("Decode(quickjs wire) error = %v", err)
		}
		typedWireAgain, err := Encode(modelAgain)
		if err != nil {
			t.Fatalf("Encode(typed model from quickjs) error = %v", err)
		}

		finalGoja := mustNewGoja(t)
		loadGojaSnapshot(t, finalGoja)
		finalValue, err := DecodeGoja(finalGoja, typedWireAgain)
		if err != nil {
			t.Fatalf("DecodeGoja(final typed wire) error = %v", err)
		}
		finalSnapshot := snapshotGojaValue(t, finalGoja, finalValue)
		assertDeepEqual(t, "goja->typed->quickjs->typed->goja snapshot", originalSnapshot, finalSnapshot)
	})
}

func TestBridgeFixturesTypedModelDecodeBoundaries(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{name: "Error", expr: `(() => { const err = new TypeError("boom"); err.cause = new Error("root"); return { err }; })()`},
		{name: "SparseArray", expr: `(() => { const arr = []; arr[1] = "x"; return arr; })()`},
		{name: "RepeatedReferencesAndCycles", expr: `(() => { const shared = { label: "shared" }; const root = { left: shared, right: shared }; root.self = root; return root; })()`},
		{name: "DeepMixedCycleGraph", expr: deepMixedCycleExpr},
		{name: "AliasLatticeWithCrossCycles", expr: aliasLatticeExpr},
	}

	for _, tt := range cases {
		t.Run("quickjs/"+tt.name, func(t *testing.T) {
			rt := mustNewQuickJS(t)
			defer rt.Close()
			value := evalQuickJS(t, rt, tt.expr)
			defer value.Free()
			wire, err := EncodeQuickJS(value)
			if err != nil {
				t.Fatalf("EncodeQuickJS() error = %v", err)
			}
			if _, err := Decode(wire); err == nil {
				t.Fatalf("Decode() unexpectedly accepted %s fixture", tt.name)
			}
		})

		t.Run("goja/"+tt.name, func(t *testing.T) {
			rt := mustNewGoja(t)
			value := evalGoja(t, rt, tt.expr)
			wire, err := EncodeGoja(value)
			if err != nil {
				t.Fatalf("EncodeGoja() error = %v", err)
			}
			if _, err := Decode(wire); err == nil {
				t.Fatalf("Decode() unexpectedly accepted %s fixture", tt.name)
			}
		})
	}
}

package jswire

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

func TestDescribe_TruncatesLongNestedValues(t *testing.T) {
	inspection := mustDescribeGojaExpr(t, `({
		kind: "user",
		name: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		tags: ["one", "two", "three", "four", "five"],
		meta: { active: true, score: 9, note: "hello" }
	})`)

	if !strings.Contains(inspection.Summary, `{kind: "user"`) {
		t.Fatalf("summary should include object shape, got %q", inspection.Summary)
	}
	if !strings.Contains(inspection.Summary, `string(45) "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx…"`) {
		t.Fatalf("summary should truncate long strings, got %q", inspection.Summary)
	}
	if !strings.Contains(inspection.Summary, `Array(5)["one", "two", "three", "four", …+1]`) {
		t.Fatalf("summary should truncate array examples, got %q", inspection.Summary)
	}
	if !strings.Contains(inspection.Full, `"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"`) {
		t.Fatalf("full should keep the complete string when still bounded, got %q", inspection.Full)
	}
	if strings.Contains(inspection.Full, `…+1`) {
		t.Fatalf("full should include all five tags, got %q", inspection.Full)
	}
}

func TestDescribe_ShowsSharedRefsAndCycles(t *testing.T) {
	inspection := mustDescribeGojaExpr(t, `(() => {
		const shared = { name: "root" };
		shared.self = shared;
		return { a: shared, b: shared };
	})()`)

	if !strings.Contains(inspection.Summary, `&1`) {
		t.Fatalf("summary should label shared refs, got %q", inspection.Summary)
	}
	if !strings.Contains(inspection.Summary, `self: *1`) {
		t.Fatalf("summary should mark circular refs, got %q", inspection.Summary)
	}
	if !strings.Contains(inspection.Summary, `b: *1`) {
		t.Fatalf("summary should reuse shared-ref labels, got %q", inspection.Summary)
	}
}

func TestDescribe_TypedArrayShowsShapeAndExamples(t *testing.T) {
	inspection := mustDescribeGojaExpr(t, `new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8, 9])`)

	if got, want := inspection.Summary, `Uint8Array(9)[1, 2, 3, 4, 5, 6, …+3]`; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got, want := inspection.Full, `Uint8Array(9)[1, 2, 3, 4, 5, 6, 7, 8, 9]`; got != want {
		t.Fatalf("full = %q, want %q", got, want)
	}
}

func TestDescribe_PreservesBuiltinEnumerableProps(t *testing.T) {
	inspection := mustDescribeGojaExpr(t, `(() => {
		const re = /abc/gi;
		re.label = "token";
		return re;
	})()`)

	if got, want := inspection.Summary, `/abc/gi {label: "token"}`; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got, want := inspection.Full, `/abc/gi {label: "token"}`; got != want {
		t.Fatalf("full = %q, want %q", got, want)
	}
}

func TestDescribe_FulfilledPromise(t *testing.T) {
	inspection := mustDescribeGojaExpr(t, `Promise.resolve({ answer: 42, label: "ok" })`)

	if got, want := inspection.Summary, `Promise<{answer: 42, label: "ok"}>`; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got, want := inspection.Full, `Promise<{answer: 42, label: "ok"}>`; got != want {
		t.Fatalf("full = %q, want %q", got, want)
	}
}

func TestDescribe_SkipsEnumerableMethods(t *testing.T) {
	inspection := mustDescribeGojaExpr(t, `({
		ok: true,
		status: 200,
		text() { return "body"; },
		json() { return { ok: true }; }
	})`)

	if got, want := inspection.Summary, `{ok: true, status: 200}`; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got, want := inspection.Full, `{ok: true, status: 200}`; got != want {
		t.Fatalf("full = %q, want %q", got, want)
	}
}

func TestDescribe_FulfilledPromiseSkipsEnumerableMethods(t *testing.T) {
	inspection := mustDescribeGojaExpr(t, `Promise.resolve({
		ok: true,
		status: 200,
		text() { return "body"; },
		json() { return { ok: true }; }
	})`)

	if got, want := inspection.Summary, `Promise<{ok: true, status: 200}>`; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got, want := inspection.Full, `Promise<{ok: true, status: 200}>`; got != want {
		t.Fatalf("full = %q, want %q", got, want)
	}
}

func TestDescribe_ShowsCustomClassName(t *testing.T) {
	inspection := mustDescribeGojaExpr(t, `(() => {
		class FetchResult {
			constructor() {
				this.ok = true;
				this.status = 200;
			}
			text() { return "body"; }
		}
		return new FetchResult();
	})()`)

	if got, want := inspection.Summary, `FetchResult {ok: true, status: 200}`; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got, want := inspection.Full, `FetchResult {ok: true, status: 200}`; got != want {
		t.Fatalf("full = %q, want %q", got, want)
	}
}

func TestDescribe_ReviewedShape_MixedSharedObject(t *testing.T) {
	inspection := mustDescribeGojaExpr(t, `(() => {
		const shared = { id: "shared", score: 7 };
		return {
			title: "12345678901234567890123456789012345678901234567890",
			when: new Date("2024-01-02T03:04:05.678Z"),
			rx: /abc/gi,
			list: [shared, shared, { bytes: new Uint8Array([1, 2, 3, 4, 5, 6, 7]), note: "tail" }],
			lookup: new Map([["self", shared], ["nums", new Set([1, 2, 3, 4, 5])]]),
			alias: shared
		};
	})()`)

	if got, want := inspection.Summary, `{alias: &1 {id: "shared", score: 7}, list: Array(3)[*1, *1, {…}], lookup: Map(2){"self" => *1, "nums" => Set(5){…}}, rx: /abc/gi, …+2}`; got != want {
		t.Fatalf("summary = %q\nwant    = %q", got, want)
	}
	if got, want := inspection.Full, `{alias: &1 {id: "shared", score: 7}, list: Array(3)[*1, *1, {bytes: Uint8Array(7)[1, 2, 3, 4, 5, 6, 7], note: "tail"}], lookup: Map(2){"self" => *1, "nums" => Set(5){1, 2, 3, 4, 5}}, rx: /abc/gi, title: "12345678901234567890123456789012345678901234567890", when: Date(2024-01-02T03:04:05.678Z)}`; got != want {
		t.Fatalf("full = %q\nwant = %q", got, want)
	}
}

func TestDescribe_ReviewedShape_CyclicObject(t *testing.T) {
	inspection := mustDescribeGojaExpr(t, `(() => {
		const root = { name: "root" };
		const child = { name: "child", parent: root };
		root.child = child;
		root.self = root;
		root.view = new Uint16Array(new Uint8Array([10, 0, 20, 0, 30, 0, 40, 0]).buffer);
		root.refs = new Set([root, child]);
		root.meta = { ok: true, note: "done" };
		return root;
	})()`)

	if got, want := inspection.Summary, `&1 {child: &2 {name: "child", parent: *1}, meta: {note: "done", ok: true}, name: "root", refs: Set(2){*1, *2}, …+2}`; got != want {
		t.Fatalf("summary = %q\nwant    = %q", got, want)
	}
	if got, want := inspection.Full, `&1 {child: &2 {name: "child", parent: *1}, meta: {note: "done", ok: true}, name: "root", refs: Set(2){*1, *2}, self: *1, view: Uint16Array(4)[10, 20, 30, 40]}`; got != want {
		t.Fatalf("full = %q\nwant = %q", got, want)
	}
}

func mustDescribeGojaExpr(t *testing.T, expr string) Inspection {
	t.Helper()
	vm := goja.New()
	value, err := vm.RunString(expr)
	if err != nil {
		t.Fatalf("RunString(%s): %v", expr, err)
	}
	raw, err := EncodeGoja(value)
	if err != nil {
		t.Fatalf("EncodeGoja(%s): %v", expr, err)
	}
	inspection, err := Describe(raw)
	if err != nil {
		t.Fatalf("Describe(%s): %v", expr, err)
	}
	return inspection
}

package jswire

import "testing"

func TestJswireTypesGraphFidelityDesignPending(t *testing.T) {
	// The current public typed Go model is intentionally tree-shaped:
	//
	//   Decode(Value) -> ObjectType / ArrayType / MapType / SetType / DateType / ...
	//
	// That is the right small API for control-plane code that wants ordinary
	// engine-independent values, but it is not a full graph-fidelity model. It
	// rejects cycles, shared object identity, sparse array holes, Error/Promise
	// nodes, custom class names, and DataView; typed array views decode as copied
	// standalone element slices rather than preserving a shared ArrayBuffer view.
	//
	// One possible future representation is an explicit RefType plus a small
	// graph arena/table, not implicit cycles hidden inside ObjectType/ArrayType:
	//
	//   type RefType uint32
	//   type GraphType struct {
	//       Root  any
	//       Nodes map[RefType]GraphNodeType
	//   }
	//
	// RefType alone is not enough: refs need a graph to point into. Any graph
	// fidelity design also needs to represent things the tree model cannot:
	//
	//   - sparse array slots, to distinguish holes from undefined/null values
	//   - typed-array/DataView views with Buffer ref, byteOffset, and byteLength
	//   - enumerable props attached to native nodes such as Date, RegExp, Map,
	//     Set, ArrayBuffer, typed arrays, and Error
	//   - Error name/message/cause, fulfilled Promise result if supported, and
	//     custom class/constructor labels
	//
	// Another attractive direction is not to expose a Go graph encoder/decoder at
	// all. Instead, expose a graph-fidelity renderer that emits JavaScript source
	// which reconstructs the graph:
	//
	//   Value -> JavaScript factory/source -> run in goja or QuickJS -> EncodeGoja
	//   or EncodeQuickJS
	//
	// That approach delegates object identity, cycles, Map/Set insertion
	// semantics, ArrayBuffer view aliasing, Date, RegExp, BigInt, Error, and other
	// native construction behavior back to a JavaScript engine. Encode is then
	// literally the existing bridge encode after evaluating the generated JS.
	// The tradeoff is that the generated source must be deterministic, safe to
	// embed, and explicit about unsupported host/exotic values.
	//
	// The internal wireGraph is already basically the private graph arena needed
	// to drive either design. If we expose graph fidelity later, prefer a
	// deliberate separate API (for example a JavaScript graph factory renderer)
	// over making Decode sometimes return refs and sometimes return tree values.
	// That keeps the simple typed model predictable while giving bridge tests a
	// true graph-preserving intermediate representation when the public design is
	// settled.
	t.Skip("current jswire typed Types do not provide full graph fidelity; public graph model design is intentionally pending")
}

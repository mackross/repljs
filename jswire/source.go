package jswire

import (
	"fmt"
	"strings"

	"github.com/fastschema/qjs"
)

// FromAnonJSObj evaluates a single anonymous JavaScript object expression and
// returns its jswire representation.
//
// JSON objects are valid input. JavaScript object literals may be supplied with
// or without wrapping parentheses.
func FromAnonJSObj(obj []byte) (Value, error) {
	src := strings.TrimSpace(string(obj))
	if src == "" {
		return nil, fmt.Errorf("jswire: anonymous object source is empty")
	}

	rt, err := qjs.New(qjs.Option{})
	if err != nil {
		return nil, fmt.Errorf("jswire: create quickjs runtime: %w", err)
	}
	defer rt.Close()

	value, err := rt.Context().Eval("anonymous-object.js", qjs.Code("("+src+")"))
	if err != nil {
		return nil, fmt.Errorf("jswire: evaluate anonymous object: %w", err)
	}
	defer value.Free()

	if !value.IsObject() || value.IntrinsicKind() != qjs.IntrinsicObject {
		return nil, fmt.Errorf("jswire: source must evaluate to an anonymous object")
	}

	return EncodeQuickJS(value)
}

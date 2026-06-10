// Package jswire transports JavaScript values as versioned binary jswire data,
// not JSON.
//
// A Value is raw jswire binary data. It is suitable as the ABI between supported
// JavaScript engines and preserves JavaScript-native values that JSON cannot
// represent, including Date, Map, Set, BigInt, RegExp, ArrayBuffer, and typed
// array values.
//
// The typed Go model is an engine-independent convenience layer for a defined
// subset of JavaScript values. ObjectType is a JavaScript ordinary object with
// string properties; it is not MapType, which represents a real JavaScript Map.
// BigIntType is decimal text without a trailing n. DateType uses JavaScript Date
// millisecond precision. ArrayBufferType and typed array values copy their bytes
// at API boundaries.
//
// Patch operations are jswire patch operations, not RFC JSON Patch. Patch values
// may use the typed Go model and are encoded as jswire values. The engine bridge
// APIs remain the most complete way to transport arbitrary JavaScript values;
// the typed Go model intentionally supports only a defined subset. Nested Value
// values are graph-spliced when encoded or used as patch values.
package jswire

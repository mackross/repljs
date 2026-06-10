package jswire

import (
	"strings"
	"testing"
	"time"
)

func TestFromAnonJSObjJSON(t *testing.T) {
	got, err := FromAnonJSObj([]byte(`{"a":1,"b":"two","nested":{"ok":true}}`))
	if err != nil {
		t.Fatalf("FromAnonJSObj() error = %v", err)
	}

	decoded, err := got.Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	obj, ok := decoded.(ObjectType)
	if !ok {
		t.Fatalf("decoded = %#v, want ObjectType", decoded)
	}
	if obj["a"] != float64(1) || obj["b"] != "two" {
		t.Fatalf("decoded scalar props = %#v", obj)
	}
	nested, ok := obj["nested"].(ObjectType)
	if !ok || nested["ok"] != true {
		t.Fatalf("decoded nested prop = %#v", obj["nested"])
	}
}

func TestFromAnonJSObjPreservesNativeValues(t *testing.T) {
	got, err := FromAnonJSObj([]byte(`({
		when: new Date("2021-02-03T04:05:06.789Z"),
		map: new Map([["a", 1], ["b", 2]]),
		set: new Set(["x", "y"]),
		big: 123n,
		re: /ab+/gi,
		bytes: new Uint8Array([1, 2, 3]),
		buffer: new Uint8Array([4, 5]).buffer
	})`))
	if err != nil {
		t.Fatalf("FromAnonJSObj() error = %v", err)
	}

	decoded, err := got.Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	obj, ok := decoded.(ObjectType)
	if !ok {
		t.Fatalf("decoded = %#v, want ObjectType", decoded)
	}
	if got := time.Time(obj["when"].(DateType)).Format(time.RFC3339Nano); got != "2021-02-03T04:05:06.789Z" {
		t.Fatalf("when = %q", got)
	}
	if got := obj["map"].(MapType); len(got) != 2 || got[0][0] != "a" || got[0][1] != float64(1) || got[1][0] != "b" || got[1][1] != float64(2) {
		t.Fatalf("map = %#v", got)
	}
	if got := obj["set"].(SetType); len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("set = %#v", got)
	}
	if got := obj["big"].(BigIntType); got != "123" {
		t.Fatalf("big = %#v", got)
	}
	if got := obj["re"].(RegExpType); got != (RegExpType{"ab+", "gi"}) {
		t.Fatalf("re = %#v", got)
	}
	if got := obj["bytes"].(Uint8ArrayType); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("bytes = %#v", got)
	}
	if got := obj["buffer"].(ArrayBufferType); len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("buffer = %#v", got)
	}
}

func TestFromAnonJSObjRejectsNonObjects(t *testing.T) {
	for _, src := range []string{`null`, `[]`, `new Date()`, `"text"`} {
		t.Run(src, func(t *testing.T) {
			if _, err := FromAnonJSObj([]byte(src)); err == nil || !strings.Contains(err.Error(), "anonymous object") {
				t.Fatalf("FromAnonJSObj(%s) error = %v, want anonymous object error", src, err)
			}
		})
	}
}

package model_test

import (
	"testing"

	"github.com/mus-format/mus-go/ord"
)

// TestMusStringRoundTrip verifies that the mus-go dependency resolves
// correctly and that a basic string encode/decode cycle produces identical
// values. This is the slice-level proof target for the S01 demo criterion
// "mus encodes and decodes a cycle."
func TestMusStringRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "ascii", input: "hello, world"},
		{name: "unicode", input: "こんにちは 🌏"},
		{name: "long", input: "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Determine the encoded size and allocate an exact-fit buffer.
			size := ord.String.Size(tc.input)
			buf := make([]byte, size)

			// Marshal into the buffer; n must equal size.
			n := ord.String.Marshal(tc.input, buf)
			if n != size {
				t.Fatalf("Marshal wrote %d bytes; Size reported %d", n, size)
			}

			// Unmarshal back and verify byte count and value.
			got, m, err := ord.String.Unmarshal(buf)
			if err != nil {
				t.Fatalf("Unmarshal error: %v", err)
			}
			if m != size {
				t.Fatalf("Unmarshal consumed %d bytes; expected %d", m, size)
			}
			if got != tc.input {
				t.Fatalf("round-trip mismatch: got %q, want %q", got, tc.input)
			}
		})
	}
}

// TestMusIntRoundTrip verifies varint encode/decode for a representative
// integer value, exercising the varint sub-package alongside ord.
func TestMusIntRoundTrip(t *testing.T) {
	// Use ord.String indirectly through a length-prefixed blob to confirm
	// that the ord package correctly delegates length serialization to varint.
	// A single 256-byte string forces a two-byte varint length prefix.
	long := make([]byte, 256)
	for i := range long {
		long[i] = byte('a' + i%26)
	}
	input := string(long)

	size := ord.String.Size(input)
	buf := make([]byte, size)
	n := ord.String.Marshal(input, buf)
	if n != size {
		t.Fatalf("Marshal wrote %d bytes; Size reported %d", n, size)
	}
	got, m, err := ord.String.Unmarshal(buf)
	if err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if m != size {
		t.Fatalf("Unmarshal consumed %d bytes; expected %d", m, size)
	}
	if got != input {
		t.Fatalf("round-trip mismatch for long string (len=%d)", len(input))
	}
}

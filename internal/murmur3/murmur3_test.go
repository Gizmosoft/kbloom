package murmur3

import "testing"

// TestSum128 checks the seed-0 digests against the public MurmurHash3 vectors.
func TestSum128(t *testing.T) {
	// Reference digests are the seed-0 vectors from the public MurmurHash3 suite.
	vectors := []struct {
		in     string
		h1, h2 uint64
	}{
		{"", 0x0000000000000000, 0x0000000000000000},
		{"hello", 0xcbd8a7b341bd9b02, 0x5b1e906a48ae1d19},
		{"hello, world", 0x342fac623a5ebc8e, 0x4cdcbc079642414d},
		{"19 Jan 2038 at 3:14:07 AM", 0xb89e5988b737affc, 0x664fc2950231b2cb},
		{"The quick brown fox jumps over the lazy dog.", 0xcd99481f9ee902c9, 0x695da1a38987b6e7},
	}
	for _, tc := range vectors {
		h1, h2 := Sum128([]byte(tc.in))
		if h1 != tc.h1 || h2 != tc.h2 {
			t.Errorf("Sum128(%q) = %x %x, want %x %x", tc.in, h1, h2, tc.h1, tc.h2)
		}
	}
}

// TestSum128NilAndEmpty checks that a nil slice and an empty slice hash the same.
func TestSum128NilAndEmpty(t *testing.T) {
	h1, h2 := Sum128(nil)
	e1, e2 := Sum128([]byte{})
	if h1 != e1 || h2 != e2 {
		t.Fatalf("nil and empty digests differ: %x %x vs %x %x", h1, h2, e1, e2)
	}
}

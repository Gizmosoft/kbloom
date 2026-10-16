package bloom

import (
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"sync"
	"testing"
)

// TestSize checks the documented m, k, and word-count formulas on known inputs.
func TestSize(t *testing.T) {
	cases := []struct {
		n, m, k, words uint64
		p              float64
	}{
		{n: 1000, p: 0.01, m: 9586, k: 7, words: 150},
		{n: 1_000_000, p: 0.01, m: 9585059, k: 7, words: 149767},
		{n: 100, p: 0.001, m: 1438, k: 10, words: 23},
	}
	for _, tc := range cases {
		m, k, words, err := size(tc.p, tc.n, 0)
		if err != nil {
			t.Fatalf("size(%v, %d): %v", tc.p, tc.n, err)
		}
		if m != tc.m || k != tc.k || words != tc.words {
			t.Fatalf("size(%v, %d) = m:%d k:%d words:%d, want m:%d k:%d words:%d",
				tc.p, tc.n, m, k, words, tc.m, tc.k, tc.words)
		}
	}
}

// TestSizeRejectsBadInputs covers rates, capacities, and bit caps that must not allocate.
func TestSizeRejectsBadInputs(t *testing.T) {
	if _, _, _, err := size(0, 100, 0); !errors.Is(err, ErrBadErrorRate) {
		t.Fatalf("p=0: %v", err)
	}
	if _, _, _, err := size(1, 100, 0); !errors.Is(err, ErrBadErrorRate) {
		t.Fatalf("p=1: %v", err)
	}
	if _, _, _, err := size(math.NaN(), 100, 0); !errors.Is(err, ErrBadErrorRate) {
		t.Fatalf("p=NaN: %v", err)
	}
	if _, _, _, err := size(0.01, 0, 0); !errors.Is(err, ErrBadCapacity) {
		t.Fatalf("n=0: %v", err)
	}
	if _, _, _, err := size(0.01, exactCapacityLimit+1, 0); !errors.Is(err, ErrBadCapacity) {
		t.Fatalf("huge n: %v", err)
	}
	if _, _, _, err := size(0.01, 1_000_000, 1000); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("max bits: %v", err)
	}
	// A rate near 1 can round k down to 0; the filter still needs one probe.
	_, k, _, err := size(0.99, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if k < 1 {
		t.Fatalf("k=%d", k)
	}
}

// TestBitLayout checks that bit 63 stays in word 0 and bit 64 starts word 1.
func TestBitLayout(t *testing.T) {
	f := &Filter{bits: make([]uint64, 2), m: 128, k: 1}
	if !f.setBit(63) {
		t.Fatal("bit 63 should flip")
	}
	if f.setBit(63) {
		t.Fatal("bit 63 should already be set")
	}
	if !f.hasBit(63) || f.hasBit(64) || f.hasBit(0) {
		t.Fatalf("bits[0]=%x bits[1]=%x", f.bits[0], f.bits[1])
	}
	if !f.setBit(64) {
		t.Fatal("bit 64 should flip")
	}
	if f.bits[0] != 1<<63 || f.bits[1] != 1 {
		t.Fatalf("bits[0]=%x bits[1]=%x", f.bits[0], f.bits[1])
	}
}

// TestAddAndExists checks the first insert, a repeat insert, and an empty item.
func TestAddAndExists(t *testing.T) {
	f, err := New(0.01, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	item := []byte("alpha")
	if f.Exists(item) {
		t.Fatal("missing item reported present")
	}
	if !f.Add(item) {
		t.Fatal("first add should flip a bit")
	}
	if f.Add(item) {
		t.Fatal("second add should flip nothing")
	}
	if !f.Exists(item) {
		t.Fatal("false negative")
	}
	if !f.Exists([]byte("alpha")) {
		t.Fatal("equal item missed")
	}

	empty := []byte{}
	if !f.Add(empty) || !f.Exists(nil) || !f.Exists(empty) {
		t.Fatal("empty item was not recorded")
	}
}

// TestNoFalseNegativesAndBoundedFPR inserts a fixed set and bounds random misses.
func TestNoFalseNegativesAndBoundedFPR(t *testing.T) {
	const (
		n      = 2000
		probes = 2000
		p      = 0.01
	)
	f, err := New(p, n, 0)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	var buf [8]byte
	present := make([]uint64, n)
	for i := range present {
		present[i] = rng.Uint64()
		binary.LittleEndian.PutUint64(buf[:], present[i])
		f.Add(buf[:])
	}
	for _, v := range present {
		binary.LittleEndian.PutUint64(buf[:], v)
		if !f.Exists(buf[:]) {
			t.Fatalf("false negative for %d", v)
		}
	}
	var hits int
	for i := 0; i < probes; i++ {
		binary.LittleEndian.PutUint64(buf[:], uint64(i)+1<<62)
		if f.Exists(buf[:]) {
			hits++
		}
	}
	if rate := float64(hits) / probes; rate > 0.05 {
		t.Fatalf("false-positive rate %.3f exceeds 0.05 (target %.2f, hits %d)", rate, p, hits)
	}
}

// TestConcurrentAddExists checks that parallel inserts are all visible afterwards.
func TestConcurrentAddExists(t *testing.T) {
	const items = 4000
	f, err := New(0.01, items, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	workers := 8
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			var buf [8]byte
			for i := w; i < items; i += workers {
				binary.LittleEndian.PutUint64(buf[:], uint64(i))
				f.Add(buf[:])
			}
		}(w)
	}
	wg.Wait()
	var buf [8]byte
	for i := 0; i < items; i++ {
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		if !f.Exists(buf[:]) {
			t.Fatalf("missing %d after concurrent adds", i)
		}
	}
}

// BenchmarkAdd measures a single-threaded insert into a fixed-size filter.
func BenchmarkAdd(b *testing.B) {
	f, err := New(0.01, 100_000, 0)
	if err != nil {
		b.Fatal(err)
	}
	var buf [8]byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		f.Add(buf[:])
	}
}

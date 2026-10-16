// Package bloom implements an in-memory Bloom filter.
//
// Bit arrays are packed into []uint64. Membership uses the Kirsch-Mitzenmacher
// trick: one 128-bit MurmurHash3 digest produces k bit indexes.
package bloom

import (
	"math"
	"sync"

	"github.com/kartikeyhebbar/kbloom/internal/murmur3"
)

// maxHashes bounds the probe loop. Optimal k for a representable error rate
// stays well below this; larger values are rejected as mis-sized.
const maxHashes = 1024

// exactCapacityLimit is the largest n that float64 can represent exactly.
// Beyond it the sizing formulas lose integer precision.
const exactCapacityLimit = 1 << 53

// Filter is a fixed-size Bloom filter. Add and Exists are safe for concurrent use.
// A zero Filter is not usable; create one with New.
type Filter struct {
	mu   sync.RWMutex // guards bits; Exists takes the read lock, Add the write lock
	bits []uint64     // packed bit array, 64 bits per word
	m    uint64       // bit length used by the modulo; may be shorter than len(bits)*64
	k    uint64       // number of hash probes per item
}

// New sizes a filter for capacity insertions at the given false-positive rate.
// maxBits caps the bit array; zero means no cap.
func New(errorRate float64, capacity, maxBits uint64) (*Filter, error) {
	m, k, words, err := size(errorRate, capacity, maxBits)
	if err != nil {
		return nil, err
	}
	return &Filter{
		bits: make([]uint64, words),
		m:    m,
		k:    k,
	}, nil
}

// Add sets the item's k bits. It reports whether at least one bit flipped from
// 0 to 1. A false result means the item was probably already present.
func (f *Filter) Add(item []byte) bool {
	// Hash outside the lock so concurrent Exists calls are not blocked on MurmurHash3.
	h1, h2 := hashes(item)
	f.mu.Lock()
	defer f.mu.Unlock()
	changed := false
	for i := uint64(0); i < f.k; i++ {
		// combined = hash1 + i*hash2, folded into the m-bit array.
		if f.setBit((h1 + i*h2) % f.m) {
			changed = true
		}
	}
	return changed
}

// Exists reports whether every bit for item is set.
// False means the item is definitely absent. True means it is probably present.
func (f *Filter) Exists(item []byte) bool {
	// Same index sequence as Add. The read lock keeps the probes on a stable bitset.
	h1, h2 := hashes(item)
	f.mu.RLock()
	defer f.mu.RUnlock()
	for i := uint64(0); i < f.k; i++ {
		if !f.hasBit((h1 + i*h2) % f.m) {
			return false
		}
	}
	return true
}

// hashes returns the Kirsch-Mitzenmacher base pair. hash2 is forced odd so the
// sequence hash1+i*hash2 does not collapse when hash2 shares factors with m.
func hashes(item []byte) (uint64, uint64) {
	h1, h2 := murmur3.Sum128(item)
	h2 |= 1
	return h1, h2
}

// setBit turns on bit idx and reports whether it changed from 0 to 1.
// The caller must hold f.mu. idx>>6 selects the word; idx&63 selects the bit.
func (f *Filter) setBit(idx uint64) bool {
	word := idx >> 6
	mask := uint64(1) << (idx & 63)
	if f.bits[word]&mask != 0 {
		return false
	}
	f.bits[word] |= mask
	return true
}

// hasBit reports whether bit idx is set. The caller must hold f.mu.
func (f *Filter) hasBit(idx uint64) bool {
	return f.bits[idx>>6]&(uint64(1)<<(idx&63)) != 0
}

// size chooses m, k, and the []uint64 length.
//
//	m = ceil(-n * ln(p) / (ln 2)^2)
//	k = floor(m/n * ln 2 + 0.5)
//	words = floor((m + 63) / 64)
func size(p float64, n, maxBits uint64) (m, k, words uint64, err error) {
	if n == 0 || n > exactCapacityLimit {
		return 0, 0, 0, ErrBadCapacity
	}
	// NaN fails both comparisons, so it is rejected with the out-of-range rates.
	if math.IsNaN(p) || p <= 0 || p >= 1 {
		return 0, 0, 0, ErrBadErrorRate
	}

	ln2 := math.Ln2
	mf := math.Ceil(-float64(n) * math.Log(p) / (ln2 * ln2))
	// 2^64 does not fit in uint64, so anything at or above that cannot be stored.
	if math.IsNaN(mf) || math.IsInf(mf, 0) || mf < 1 || mf >= 18446744073709551616.0 {
		return 0, 0, 0, ErrTooLarge
	}
	m = uint64(mf)
	if maxBits > 0 && m > maxBits {
		return 0, 0, 0, ErrTooLarge
	}
	// Adding 63 below must not wrap the bit length.
	if m > math.MaxUint64-63 {
		return 0, 0, 0, ErrTooLarge
	}

	kf := math.Floor(float64(m)/float64(n)*ln2 + 0.5)
	// A zero k cannot record an item. A huge k would stall every Add and Exists.
	if kf < 1 {
		kf = 1
	}
	if kf > maxHashes {
		return 0, 0, 0, ErrTooLarge
	}
	k = uint64(kf)

	words = (m + 63) / 64
	// The slice length must fit in int, and words*8 must fit in uint64.
	if words > uint64(^uint(0)>>1) || words > math.MaxUint64/8 {
		return 0, 0, 0, ErrTooLarge
	}
	return m, k, words, nil
}

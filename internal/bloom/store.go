package bloom

import "sync"

// StoreConfig limits how much memory Reserve will commit.
// A zero MaxBits or MaxBytes disables that particular cap.
type StoreConfig struct {
	MaxBits  uint64
	MaxBytes uint64
}

// Store is the process-wide registry of named filters.
// Filters are created only through Reserve and are not deleted.
type Store struct {
	mu       sync.RWMutex
	filters  map[string]*Filter // named filters; entries are never deleted
	used     uint64             // bytes currently committed to bitsets
	maxBits  uint64             // per-filter bit cap; zero means unlimited
	maxBytes uint64             // total bitset budget; zero means unlimited
}

// NewStore returns an empty registry.
func NewStore(cfg StoreConfig) *Store {
	return &Store{
		filters:  make(map[string]*Filter),
		maxBits:  cfg.MaxBits,
		maxBytes: cfg.MaxBytes,
	}
}

// Reserve creates a filter for key. It fails if key is already present or the
// sized bitset would exceed the configured budget.
func (s *Store) Reserve(key string, errorRate float64, capacity uint64) error {
	if key == "" {
		return ErrEmptyKey
	}
	// Allocate before the registry lock so a large make does not block other keys.
	f, err := New(errorRate, capacity, s.maxBits)
	if err != nil {
		return err
	}
	bytes := uint64(len(f.bits)) * 8

	s.mu.Lock()
	defer s.mu.Unlock()
	// Two reserves of the same key can both allocate; only the first insert wins.
	if _, exists := s.filters[key]; exists {
		return ErrExists
	}
	if s.overBudget(bytes) {
		return ErrTooLarge
	}
	s.filters[key] = f
	s.used += bytes
	return nil
}

// Add inserts item into the filter named key.
// The boolean is true when at least one bit changed.
func (s *Store) Add(key string, item []byte) (bool, error) {
	f, err := s.get(key)
	if err != nil {
		return false, err
	}
	return f.Add(item), nil
}

// Exists reports probable membership of item in the filter named key.
func (s *Store) Exists(key string, item []byte) (bool, error) {
	f, err := s.get(key)
	if err != nil {
		return false, err
	}
	return f.Exists(item), nil
}

// get returns the filter for key and drops the registry lock before bit operations.
// Filters are never removed, so the pointer stays valid after the unlock.
func (s *Store) get(key string) (*Filter, error) {
	s.mu.RLock()
	f := s.filters[key]
	s.mu.RUnlock()
	if f == nil {
		return nil, ErrNotFound
	}
	return f, nil
}

// overBudget reports whether committing extra bytes would pass MaxBytes.
// Caller holds s.mu. A zero budget means unlimited.
func (s *Store) overBudget(extra uint64) bool {
	if s.maxBytes == 0 {
		return false
	}
	return extra > s.maxBytes || s.used > s.maxBytes-extra
}

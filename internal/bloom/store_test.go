package bloom

import (
	"errors"
	"sync"
	"testing"
)

// TestStoreReserveAddExists covers create, duplicate create, and membership on a named filter.
func TestStoreReserveAddExists(t *testing.T) {
	s := NewStore(StoreConfig{MaxBits: 1 << 20, MaxBytes: 1 << 20})
	if err := s.Reserve("users", 0.01, 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve("users", 0.01, 1000); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate reserve: %v", err)
	}
	if err := s.Reserve("", 0.01, 1000); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("empty key: %v", err)
	}
	if _, err := s.Add("missing", []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("add missing: %v", err)
	}
	if _, err := s.Exists("missing", []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exists missing: %v", err)
	}

	added, err := s.Add("users", []byte("alice"))
	if err != nil || !added {
		t.Fatalf("add: added=%v err=%v", added, err)
	}
	added, err = s.Add("users", []byte("alice"))
	if err != nil || added {
		t.Fatalf("second add: added=%v err=%v", added, err)
	}
	ok, err := s.Exists("users", []byte("alice"))
	if err != nil || !ok {
		t.Fatalf("exists: ok=%v err=%v", ok, err)
	}
	ok, err = s.Exists("users", []byte("bob"))
	if err != nil || ok {
		t.Fatalf("absent item: ok=%v err=%v", ok, err)
	}
}

// TestStoreMemoryBudget checks that a second filter is refused once the byte cap is passed.
func TestStoreMemoryBudget(t *testing.T) {
	// n=1000, p=0.01 uses 150 words = 1200 bytes.
	s := NewStore(StoreConfig{MaxBits: 1 << 20, MaxBytes: 2000})
	if err := s.Reserve("a", 0.01, 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve("b", 0.01, 1000); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("budget: %v", err)
	}
	if s.used != 1200 {
		t.Fatalf("used=%d, want 1200", s.used)
	}
}

// TestStoreReserveRace checks that only one of many concurrent reserves of the same key succeeds.
func TestStoreReserveRace(t *testing.T) {
	s := NewStore(StoreConfig{MaxBits: 1 << 20, MaxBytes: 1 << 20})
	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- s.Reserve("same", 0.01, 100)
		}()
	}
	wg.Wait()
	close(errCh)
	var ok, exists int
	for err := range errCh {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrExists):
			exists++
		default:
			t.Fatal(err)
		}
	}
	if ok != 1 || exists != 31 {
		t.Fatalf("ok=%d exists=%d", ok, exists)
	}
}

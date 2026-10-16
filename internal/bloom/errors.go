package bloom

import "errors"

// Sentinel errors returned by the filter and the registry.
// The server maps each one onto a RESP error reply.
var (
	// ErrExists is returned when BF.RESERVE targets a key that already has a filter.
	ErrExists = errors.New("item exists")
	// ErrNotFound is returned when a command addresses a key that was never reserved.
	ErrNotFound = errors.New("not found")
	// ErrEmptyKey is returned when the key is empty.
	ErrEmptyKey = errors.New("empty key")
	// ErrBadErrorRate is returned when the false-positive rate is outside (0, 1).
	ErrBadErrorRate = errors.New("bad error rate")
	// ErrBadCapacity is returned when the expected capacity is zero or cannot be sized exactly.
	ErrBadCapacity = errors.New("bad capacity")
	// ErrTooLarge is returned when the filter would exceed the configured bit or memory budget.
	ErrTooLarge = errors.New("filter too large")
)

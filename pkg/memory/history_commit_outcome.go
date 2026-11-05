package memory

import (
	"errors"
	"fmt"
)

// ErrHistoryInputCommitted means this exact history mutation reached source NEW,
// but operational recovery is still pending. Do not replay the applied input or
// fall back to legacy receipts. Unwrap retains the actual I/O cause.
var ErrHistoryInputCommitted = errors.New("memory: history input committed with pending recovery")

// This is an outcome of the owning append, not a later getter of its tail.
// In particular, unavailable proof cannot turn a confirmed input into OLD.
type historyAppendOutcome struct {
	committed bool
	meta      SessionMeta
}

// Cleanup runs only after the selected data/metadata/witness durability phases.
// Retaining this journal is observable recovery work, but cannot revoke a live
// receipt that already has its durable NEW decision. Required sync failures
// never use this type.
type historyCleanupError struct{ cause error }

func (err *historyCleanupError) Error() string { return err.cause.Error() }
func (err *historyCleanupError) Unwrap() error { return err.cause }

func onlyHistoryCleanupError(err error) bool {
	var cleanup *historyCleanupError
	return errors.As(err, &cleanup) && err == cleanup
}

func historyCommittedError(err error) error {
	return fmt.Errorf("%w: %w", ErrHistoryInputCommitted, err)
}

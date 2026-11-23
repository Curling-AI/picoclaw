package memory

import (
	"os"
	"testing"
)

// TestMain runs the package in strict mode: a store write made without its
// session's file lock panics, so every test path also checks the lock.
func TestMain(m *testing.M) {
	sessionLockStrict = true
	os.Exit(m.Run())
}

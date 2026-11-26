package memory

// Tests that forge a session's state call write primitives of the store
// directly. In the package's strict mode a write without the session's file
// lock panics, so these helpers take the lock around the one call, as the
// store's own methods do, but without adopting the session: taking the lock
// would recover the journal an earlier forged step left, and the forged state
// is the one under test (adoptSessionLock).

import (
	"context"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
)

// holdSessionLockForTest takes key's session lock; the returned func releases it.
func holdSessionLockForTest(t testing.TB, s *JSONLStore, key string) func() {
	t.Helper()
	unlock, err := s.lockSession(key)
	if err != nil {
		t.Fatal(err)
	}
	return unlock
}

// forgeUnderSessionLock takes key's shard mutex and file lock without running
// adoptSessionLock; the returned func releases both.
func forgeUnderSessionLock(t testing.TB, s *JSONLStore, key string) func() {
	t.Helper()
	unlockShards := s.lockSessionPair(key, key)
	held, err := s.flockSessions(sessionLockWait, []string{key})
	if err != nil {
		unlockShards()
		t.Fatal(err)
	}
	return func() {
		s.releaseSessionFlocks(held)
		unlockShards()
	}
}

func writeMetaLocked(t testing.TB, s *JSONLStore, key string, meta SessionMeta) error {
	t.Helper()
	defer forgeUnderSessionLock(t, s, key)()
	return s.writeMeta(key, meta)
}

func beginHistoryWriteLocked(
	t testing.TB, s *JSONLStore, ctx context.Context, key string, recovery historyWriteRecovery,
) error {
	t.Helper()
	defer forgeUnderSessionLock(t, s, key)()
	return s.beginHistoryWrite(ctx, key, recovery)
}

func writePublicChangeLocked(t testing.TB, s *JSONLStore, key string, change publicHistoryChange) error {
	t.Helper()
	defer forgeUnderSessionLock(t, s, key)()
	return s.writePublicChange(key, change)
}

func markHistoryRecoveryRequiredLocked(t testing.TB, s *JSONLStore, key string, meta *SessionMeta) error {
	t.Helper()
	defer forgeUnderSessionLock(t, s, key)()
	return s.markHistoryRecoveryRequired(key, meta)
}

func recoverHistoryWriteDecisionLocked(t testing.TB, s *JSONLStore, key string) (historyWriteDecision, error) {
	t.Helper()
	defer forgeUnderSessionLock(t, s, key)()
	return s.recoverHistoryWriteDecision(key)
}

func recoverHistoryWriteLocked(t testing.TB, s *JSONLStore, key string) error {
	t.Helper()
	defer forgeUnderSessionLock(t, s, key)()
	return s.recoverHistoryWrite(key)
}

func writeContentCheckpointLocked(t testing.TB, s *JSONLStore, key string, meta SessionMeta) error {
	t.Helper()
	defer forgeUnderSessionLock(t, s, key)()
	return s.writeContentCheckpoint(key, meta)
}

func writeContentCheckpointAvailabilityLocked(
	t testing.TB, s *JSONLStore, key string, meta SessionMeta, unavailable bool,
) error {
	t.Helper()
	defer forgeUnderSessionLock(t, s, key)()
	return s.writeContentCheckpointAvailability(key, meta, unavailable)
}

func createContentPreimageLocked(
	t testing.TB, s *JSONLStore, ctx context.Context, image contentPreimage, data []byte,
) error {
	t.Helper()
	defer forgeUnderSessionLock(t, s, image.Key)()
	return s.createContentPreimage(ctx, image, data)
}

// sessionDirEntries lists a sessions folder without its lock folder, which
// holds no session data.
func sessionDirEntries(t testing.TB, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	kept := entries[:0]
	for _, entry := range entries {
		if entry.Name() != sessionlock.Dir {
			kept = append(kept, entry)
		}
	}
	return kept
}

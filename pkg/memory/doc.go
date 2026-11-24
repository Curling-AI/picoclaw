// Package memory stores sessions as JSONL files under a workspace's sessions/.
//
// # Two writers
//
// During a rollout two processes (the old and the new pod) share one
// sessions/ folder on a network filesystem. The rule:
//
//   - Every write and every recovery of a session file goes through JSONLStore
//     (or sessionlock, for writers outside the package). It holds the
//     session's flock in sessions/.locks/<name>.lock for the whole commit.
//     Never write a session file outside the store.
//   - Lock order: the shard mutex first, then the flock; two sessions in
//     ascending name order. A directory sweep never waits for a lock.
//   - Under the lock the disk decides: the session's cache is dropped and an
//     orphan journal is recovered before anything is written. Never trust a
//     cache another process may change without rereading it under the lock.
//   - A lost lock (ErrSessionLockLost) stops the writer and undoes nothing.
//   - The cache serves plain reads only.
//
// The two-store regression tests pin the rule, so CI enforces it: see
// TestTwoStore* in history_two_store_test.go. Change the rule only with them.
//
// # Known gaps
//
//   - The shell tool (exec) does not take the lock. It only refuses commands
//     that touch sessions/.locks; ordinary shell writes to sessions/ remain a
//     known risk, and the refusal catches accidents, not a determined agent.
//   - A listing may show a stale meta until the next write or turn in that
//     session (no TTL). A turn rereads the session's meta once at its start.
//   - JSONLBackend.RefreshSession (pkg/session) resolves aliases through the
//     cached listing: an alias another pod added may resolve to another key
//     until the listing refreshes.
//   - Check proves the lock only at its instant (see checkSessionLock), and
//     some holds never call it: the read-recovery gate (read_recovery.go) and
//     the public index preparation (public_history_index_prepare.go) can write
//     after a lease was lost.
//   - The recovery latch can stay set when the tail has no owner and at least
//     one complete line lies beyond HistoryBytes; it is never cleared.
//   - A negative lookup cache on NFS could hide a file another pod created.
//     Each acquisition revalidates sessions/ (revalidateSessionsDir); not yet
//     confirmed on EFS.
//   - Off unix there is no inter-process lock (sessionlock_other.go).
package memory

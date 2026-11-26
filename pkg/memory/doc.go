// Package memory stores sessions as JSONL files under a workspace's sessions/.
//
// # Two writers
//
// During a rollout two processes (the old and the new pod) share one
// sessions/ folder on a network filesystem. The rule:
//
//   - Never write a session file without its session lock: the session's
//     flock in sessions/.locks/<name>.lock, held for the whole commit. The
//     store (JSONLStore), the fallback SessionManager (pkg/session) and the
//     agent's file tools (pkg/tools/fs) take it; a writer outside this package
//     takes it through sessionlock. Recovery runs only in the store, under it.
//   - Lock order: the shard mutex first, then the flock; two sessions in
//     ascending name order. A directory sweep never waits for a lock.
//   - Under the lock the disk decides: the session's cache is dropped and an
//     orphan journal is recovered before anything is written. Never trust a
//     cache another process may change without rereading it under the lock.
//   - The lock is advisory and an NFS lease can be lost: every writer proves
//     it (Lock.Check) right before the rename or removal that makes its write
//     visible. A lost lock (ErrSessionLockLost) stops the writer there and
//     undoes nothing.
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
//     Each acquisition and each turn's reread (RefreshSessionMeta) revalidate
//     sessions/ (revalidateSessionsDir); not yet confirmed on EFS.
//   - The first rollout from an image without the lock is not covered: the old
//     pod takes no lock, so the new pod's lock excludes nothing until the old
//     pod is gone.
//   - A lost node keeps its sessions locked until its NFS lease ends (about 90
//     to 103 s on a Linux nfsd; not measured on EFS). Until then, writes to
//     them fail once their wait for the lock runs out.
//   - A write waiting for another process's lock holds its shard mutex: other
//     sessions of the same shard wait with it, their plain reads and listings
//     included.
//   - The launcher's session deletion (web/backend/api/session.go) removes the
//     legacy <name>.json without the lock.
//   - Off unix there is no inter-process lock (sessionlock_other.go).
package memory

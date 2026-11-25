package memory

// The per-session file lock of the store. Two processes can share one
// sessions/ folder (the two pods of one assistant during a rollout, on EFS);
// the shard mutex only orders this process, so every store write also holds
// the session's sessionlock, taken after the shard and released before it.

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
)

var (
	// sessionLockWait is how long a store write waits for a session lock that
	// another process holds before it fails with ErrSessionBusy.
	sessionLockWait = 10 * time.Second
	// sessionBusyBackoff: once a write timed out on a session, later writes to
	// that session in this process fail at once for this long, instead of each
	// holding the shard mutex for another sessionLockWait.
	sessionBusyBackoff = 5 * time.Second
	// sessionLockNow is the backoff's clock.
	sessionLockNow = time.Now
	// sessionLockStrict turns a write without the session lock into a panic.
	// The package tests set it, so every test path checks the lock.
	sessionLockStrict bool
)

var (
	// ErrSessionBusy reports that another process held the session's lock past
	// the wait. Nothing was written.
	ErrSessionBusy = sessionlock.ErrBusy
	// ErrSessionLockLost reports that this process can no longer prove it holds
	// the session's lock; it stops writing and undoes nothing.
	ErrSessionLockLost = sessionlock.ErrLost

	errSessionLockNotHeld = errors.New("memory: write without the session lock")
	// errSessionLockRequired: a read under the shard mutex alone found that it
	// must write first (recover a pending journal, set the recovery latch).
	errSessionLockRequired = errors.New("memory: read needs the session lock")
)

// heldSessionLock is one session lock this process holds. lost is the store's
// own record that the lock can no longer be proven held; adoptedDeletion, that
// this acquisition finished a deletion intent (session_adopt.go).
type heldSessionLock struct {
	lock            *sessionlock.Lock
	lost            bool
	adoptedDeletion bool
}

// lockedSession is one session of an acquisition: the key the caller named and
// the session file name its lock uses. shared marks a name whose lock file is
// the one an earlier name of the same acquisition already holds.
type lockedSession struct {
	key, name string
	lock      *sessionlock.Lock
	shared    bool
}

// lockSession takes the session's shard mutex and then its file lock, waiting
// up to sessionLockWait for another process. The returned func releases both.
func (s *JSONLStore) lockSession(key string) (func(), error) {
	return s.acquireSessionLocks(sessionLockWait, key)
}

// trySessionLock is lockSession that does not wait: ok is false, with no error,
// when another process holds the session's lock, or its shard mutex is in use
// in this process. A sweep skips that session: whoever next takes its
// lock recovers it (adoptSessionLock). A busy try does not start the busy
// backoff: only a timed-out wait does.
func (s *JSONLStore) trySessionLock(key string) (unlock func(), ok bool, err error) {
	unlock, err = s.acquireSessionLocks(0, key)
	if errors.Is(err, ErrSessionBusy) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return unlock, true, nil
}

// lockSessionPairFiles locks the two sessions of an alias pair: the shard
// mutexes in shard order (lockSessionPair), then the file locks in name order,
// the order every process uses, so two pairs crossing the same two sessions
// cannot deadlock between processes.
func (s *JSONLStore) lockSessionPairFiles(keyA, keyB string) (func(), error) {
	return s.acquireSessionLocks(sessionLockWait, keyA, keyB)
}

// acquireSessionLocks is the one acquisition path of the store: one or two
// keys, waiting up to wait (zero tries once). A try takes the shard mutexes
// only if they are free: a write of this process holds its shard while it
// waits for another process's lock, and a sweep must not wait behind it, even
// for another session of the shard. A busy shard is ErrSessionBusy, as a busy
// flock is, and starts no backoff. Once the file locks are held it runs
// adoptSessionLock for each session in name order, before the caller reads
// anything. On any error it releases what it took and returns the error.
func (s *JSONLStore) acquireSessionLocks(wait time.Duration, keys ...string) (func(), error) {
	var unlockShards func()
	if wait > 0 {
		unlockShards = s.lockSessionPair(keys[0], keys[len(keys)-1])
	} else {
		unlockShards = s.tryLockSessionPair(keys[0], keys[len(keys)-1])
	}
	if unlockShards == nil {
		return nil, fmt.Errorf("%w: %s", ErrSessionBusy, sessionLockName(keys[0]))
	}
	if s.skipFileLockForTest {
		return unlockShards, nil
	}
	held, err := s.flockSessions(wait, keys)
	if err != nil {
		unlockShards()
		return nil, err
	}
	release := func() {
		s.releaseSessionFlocks(held)
		unlockShards()
	}
	for _, session := range held {
		if err := s.adoptSessionLock(session.key); err != nil {
			release()
			return nil, err
		}
	}
	return release, nil
}

// tryLockSessionPair is lockSessionPair without waiting: in the same shard
// order, or nil, holding nothing, when either shard is in use.
func (s *JSONLStore) tryLockSessionPair(keyA, keyB string) func() {
	first, second := lockShard(keyA), lockShard(keyB)
	if first > second {
		first, second = second, first
	}
	if !s.locks[first].TryLock() {
		return nil
	}
	if first == second {
		return s.locks[first].Unlock
	}
	if !s.locks[second].TryLock() {
		s.locks[first].Unlock()
		return nil
	}
	return func() {
		s.locks[second].Unlock()
		s.locks[first].Unlock()
	}
}

// readSessionLocked runs a read of key's session under its shard mutex alone,
// so a plain read never waits for another process. A read that must write
// first returns errSessionLockRequired, and then runs once more under the
// session's file lock.
func readSessionLocked[T any](s *JSONLStore, key string, read func() (T, error)) (T, error) {
	shard := s.sessionLock(key)
	shard.Lock()
	value, err := read()
	shard.Unlock()
	if !errors.Is(err, errSessionLockRequired) {
		return value, err
	}
	unlock, err := s.lockSession(key)
	if err != nil {
		var zero T
		return zero, err
	}
	defer unlock()
	return read()
}

// holdsSessionLock reports whether this process holds key's session file lock.
func (s *JSONLStore) holdsSessionLock(key string) bool {
	return s.heldSessionLock(sessionLockName(key)) != nil
}

// applyReadRecoveryMarker is applyHistoryRecoveryMarker for readMeta: setting
// the latch writes its marker, which needs the session's file lock.
func (s *JSONLStore) applyReadRecoveryMarker(key string, meta *SessionMeta, marked bool) error {
	if !marked && meta.HistoryRecoveryRequired && !s.holdsSessionLock(key) {
		return errSessionLockRequired
	}
	return s.applyHistoryRecoveryMarker(key, meta, marked)
}

// applyReadRecovery applies to a meta readMeta loaded the recovery marker and
// the order witness. A recovery requirement either one finds, and that the
// marker does not hold yet, is latched durably in the same read: a read under
// the lock starts from the disk, so no later read would find it in the cache
// to latch it then.
func (s *JSONLStore) applyReadRecovery(key string, meta *SessionMeta, marked bool) error {
	meta.HistoryRecoveryRequired = meta.HistoryRecoveryRequired || marked
	if err := s.applyHistoryOrderWitness(key, meta); err != nil {
		return err
	}
	return s.applyReadRecoveryMarker(key, meta, marked)
}

// flockSessions takes the file locks of keys in ascending name order. Names
// whose lock is one file share one lock: the same session file name, or two
// names that differ only in case on a case-insensitive filesystem. A second
// flock(2) on another descriptor of that file would wait on this process's own lock.
func (s *JSONLStore) flockSessions(wait time.Duration, keys []string) ([]lockedSession, error) {
	byName := make(map[string]string, len(keys))
	for _, key := range keys {
		if _, ok := byName[sessionLockName(key)]; !ok {
			byName[sessionLockName(key)] = key
		}
	}
	held := make([]lockedSession, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		session := lockedSession{key: byName[name], name: name, lock: nil, shared: false}
		if earlier := s.sameLockFile(held, name); earlier >= 0 {
			session.lock, session.shared = held[earlier].lock, true
			s.shareHeldSessionLock(held[earlier].name, name)
		} else {
			lock, err := s.flockSession(name, wait)
			if err != nil {
				s.releaseSessionFlocks(held)
				return nil, err
			}
			session.lock = lock
		}
		held = append(held, session)
	}
	return held, nil
}

// sameLockFile returns the index in held of the session whose lock file is
// also name's, or -1. Only names that mayShareLockFile pairs are compared on
// disk, so a single session, or two plain names, cost no round trip to the server.
func (s *JSONLStore) sameLockFile(held []lockedSession, name string) int {
	var info fs.FileInfo
	for index, session := range held {
		if !mayShareLockFile(name, session.name) {
			continue
		}
		if info == nil {
			var err error
			if info, err = os.Stat(sessionlock.Path(s.dir, name)); err != nil {
				return -1
			}
		}
		other, statErr := os.Stat(sessionlock.Path(s.dir, session.name))
		if statErr == nil && os.SameFile(info, other) {
			return index
		}
	}
	return -1
}

// mayShareLockFile reports whether a filesystem can store a and b as one file:
// they differ only in case, or one of them is not ASCII (APFS also folds
// Unicode normalization). EFS keeps every other pair apart.
func mayShareLockFile(a, b string) bool {
	return strings.EqualFold(a, b) || !isASCII(a) || !isASCII(b)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// shareHeldSessionLock records that name is held through from's lock, with one
// lost bit for both.
func (s *JSONLStore) shareHeldSessionLock(from, name string) {
	s.fileLocksMu.Lock()
	defer s.fileLocksMu.Unlock()
	s.fileLocks[name] = s.fileLocks[from]
}

func (s *JSONLStore) flockSession(name string, wait time.Duration) (*sessionlock.Lock, error) {
	if wait > 0 && s.sessionBusy(name) {
		return nil, fmt.Errorf("%w: %s", ErrSessionBusy, name)
	}
	if s.sessionFlockForTest != nil {
		s.sessionFlockForTest(name, false)
	}
	lock, err := sessionlock.Acquire(s.dir, name, wait)
	if err != nil {
		if wait > 0 && errors.Is(err, ErrSessionBusy) {
			s.markSessionBusy(name)
		}
		return nil, err
	}
	s.fileLocksMu.Lock()
	if s.fileLocks == nil {
		s.fileLocks = make(map[string]*heldSessionLock)
	}
	s.fileLocks[name] = &heldSessionLock{lock: lock, lost: false, adoptedDeletion: false}
	s.fileLocksMu.Unlock()
	if s.sessionFlockForTest != nil {
		s.sessionFlockForTest(name, true)
	}
	return lock, nil
}

// releaseSessionFlocks releases in reverse name order. A failed release (the
// unlink of a removed session's lock file, or the close) is logged: the lock
// is dropped either way when the descriptor closes.
func (s *JSONLStore) releaseSessionFlocks(held []lockedSession) {
	for _, session := range slices.Backward(held) {
		s.fileLocksMu.Lock()
		delete(s.fileLocks, session.name)
		s.fileLocksMu.Unlock()
		if session.shared {
			continue
		}
		if err := session.lock.Release(); err != nil {
			log.Printf("memory: release session lock %s: %v", session.name, err)
		}
	}
}

// sessionBusy reports whether name is inside its backoff after a timed-out wait.
func (s *JSONLStore) sessionBusy(name string) bool {
	s.fileLocksMu.Lock()
	defer s.fileLocksMu.Unlock()
	until, ok := s.busyUntil[name]
	if ok && !sessionLockNow().Before(until) {
		delete(s.busyUntil, name)
		return false
	}
	return ok
}

func (s *JSONLStore) markSessionBusy(name string) {
	s.fileLocksMu.Lock()
	defer s.fileLocksMu.Unlock()
	if s.busyUntil == nil {
		s.busyUntil = make(map[string]time.Time)
	}
	s.busyUntil[name] = sessionLockNow().Add(sessionBusyBackoff)
}

// requireSessionLock is the check of every write primitive of the store: this
// process holds the file lock of key's session and has not lost it.
func (s *JSONLStore) requireSessionLock(key string) error {
	return s.requireSessionNameLock(sessionLockName(key))
}

// requireSessionFileLock checks the lock of the session that owns the file at
// path. A path that belongs to no session cannot be covered by a lock and fails
// the same way.
func (s *JSONLStore) requireSessionFileLock(path string) error {
	base := path[strings.LastIndexAny(path, `/\`)+1:]
	name, ok := sessionFileName(base)
	if !ok {
		return s.requireSessionNameLock(base)
	}
	return s.requireSessionNameLock(lockNameOf(name))
}

func (s *JSONLStore) requireSessionNameLock(name string) error {
	s.fileLocksMu.Lock()
	held := s.fileLocks[name]
	lost := held != nil && held.lost
	s.fileLocksMu.Unlock()
	switch {
	case held == nil && sessionLockStrict:
		panic(fmt.Sprintf("memory: write to session %s without its file lock", name))
	case held == nil:
		return fmt.Errorf("%w: %s", errSessionLockNotHeld, name)
	case lost:
		return fmt.Errorf("%w: %s", ErrSessionLockLost, name)
	}
	return nil
}

// checkSessionLock proves, on the lock's own descriptor, that this process
// still holds key's session lock, right before a step that what another holder
// wrote cannot survive:
//   - creating a history journal or a deletion intent, which a later holder
//     must recover;
//   - the first order witness, which replaces the file;
//   - the first change to the session's files after a journal (the end of
//     beginHistoryWrite: the append's truncate, the rewrite's replace);
//   - publishing the meta (writeMeta);
//   - recovery, before it starts and again before it applies its decision (the
//     OLD rollback);
//   - the recovery latch's creation, which has no reset;
//   - a prune's caller hook, and the removals of a deletion or a prune, once
//     before they start.
//
// sessionlock's Release also checks before it unlinks a removed session's lock
// file. After a lost NFS lease the lock's fd fails while writes through new
// descriptors still succeed, so requireSessionLock alone cannot see the loss. A
// failed Check sets the store's lost bit: every later write under this hold
// fails with ErrSessionLockLost before any I/O, and the journal stays for the
// next holder. Each call costs one write and one fsync on the lock file.
//
// shortcut: a Check proves the lock only at its instant; a lease lost after it
// goes unseen until the next Check. The windows left:
//   - from taking the lock to the hold's first Check: the reads, and the
//     transcript an append creates empty with O_EXCL when there is none;
//   - inside a creation: a stall between the Check and the journal's link (or
//     O_EXCL write), the intent's create, or the witness's rename still lands
//     it. A stale journal or intent then fails every later holder's recovery
//     ("matches neither OLD nor NEW", ErrContentIdentityUnavailable) until it
//     is repaired by hand; a stale witness voids the order proofs;
//   - after the end of beginHistoryWrite: the append's write at its offset, and
//     the public and content storage writes before the meta;
//   - after the meta: the content domain, the journal's removal and the
//     storage retirement;
//   - a deletion's or prune's reads between its Check and its unlinks, and the
//     unlinks themselves;
//   - Release, between its Check and the lock path's unlink.
//
// Each window lasts as long as its step: a holder that stalls inside one for
// longer than its lease can still land that step after another process took
// the lock.
func (s *JSONLStore) checkSessionLock(key string) error {
	name := sessionLockName(key)
	if err := s.requireSessionNameLock(name); err != nil {
		return err
	}
	s.fileLocksMu.Lock()
	held := s.fileLocks[name]
	s.fileLocksMu.Unlock()
	check := (*sessionlock.Lock).Check
	if s.checkLockForTest != nil {
		check = s.checkLockForTest
	}
	err := check(held.lock)
	if err == nil {
		return nil
	}
	s.fileLocksMu.Lock()
	held.lost = true
	s.fileLocksMu.Unlock()
	if !errors.Is(err, ErrSessionLockLost) {
		err = fmt.Errorf("%w: %w", ErrSessionLockLost, err)
	}
	return fmt.Errorf("%w: %s", err, name)
}

// heldSessionLock returns the file lock this process holds for the session
// file name name, or nil.
func (s *JSONLStore) heldSessionLock(name string) *sessionlock.Lock {
	s.fileLocksMu.Lock()
	defer s.fileLocksMu.Unlock()
	if held := s.fileLocks[name]; held != nil {
		return held.lock
	}
	return nil
}

// removeSessionLockFile marks the lock of key's session for removal: the
// session's files are gone, and the release unlinks the lock file while still
// holding it, so .locks does not keep one file per deleted session.
func (s *JSONLStore) removeSessionLockFile(key string) {
	if lock := s.heldSessionLock(sessionLockName(key)); lock != nil {
		lock.MarkRemove()
	}
}

// SessionFileName returns the session a file of a sessions folder belongs to,
// as the name its lock uses (the sanitized key), from the file's base name:
// <name>.meta.json, <name>.jsonl, <name>.jsonl.<suffix> (journal, decision,
// recovery latch, witnesses, public projection, preimages) and the fallback
// session manager's <name>.json. The other files there belong to no session:
// content artifacts (.pc-history-*), writes in flight (.tmp-*, session-*.tmp,
// .content-checkpoint-new.*) and the .locks folder. The files of the empty key
// (.jsonl, .meta.json) are refused too: only the store locks that session.
func SessionFileName(base string) (string, bool) {
	name, ok := sessionFileName(base)
	if !ok || name == "" || name == "." || name == ".." {
		return "", false
	}
	return name, true
}

// emptyKeyLockName is the lock of the session whose key is empty. It holds a
// ':', which sanitizeKey never leaves in a name, so no other key shares it.
const emptyKeyLockName = ":empty"

// sessionLockName is the lock name of key's session.
func sessionLockName(key string) string {
	return lockNameOf(sanitizeKey(key))
}

func lockNameOf(name string) string {
	if name == "" {
		return emptyKeyLockName
	}
	return name
}

// sessionFileName is SessionFileName that keeps the empty key's name "".
func sessionFileName(base string) (string, bool) {
	if strings.ContainsAny(base, `/\`) {
		return "", false
	}
	// The journal is the one .jsonl suffix that ends in .json; the fallback
	// manager's <name>.json may itself hold ".jsonl" in its name.
	for _, suffix := range []string{".meta.json", ".jsonl.commit.json", ".json"} {
		if name, ok := strings.CutSuffix(base, suffix); ok {
			return name, true
		}
	}
	return cutJSONLSegment(base)
}

// cutJSONLSegment returns base before its last ".jsonl" that ends the name or
// starts a suffix. No suffix of a session file holds ".jsonl", so the last one
// is the session's even when the session name holds one.
func cutJSONLSegment(base string) (string, bool) {
	const segment = ".jsonl"
	for end := len(base); end > 0; {
		i := strings.LastIndex(base[:end], segment)
		if i < 0 {
			return "", false
		}
		if rest := base[i+len(segment):]; rest == "" || rest[0] == '.' {
			return base[:i], true
		}
		end = i
	}
	return "", false
}

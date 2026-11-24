package memory

// The cache rule. The meta cache, with the marker and order witness state next
// to it, serves plain reads only: another process sharing sessions/ may have
// written the session since this one cached it. A write, a latch and a
// recovery read the disk under the session's lock, since taking the lock drops
// the session's cache (adoptSessionLock). A plain read outside the lock may
// serve a meta older than the other process's last write: a turn rereads the
// session's meta once when it starts (RefreshSessionMeta), and a listing keeps
// the cache until this process next writes or starts a turn in that session.

import (
	"context"
	"errors"
)

// forgetSessionCache drops what another process may have changed since this one
// cached it. A positive recovery latch is never cleared, so it stays.
func (s *JSONLStore) forgetSessionCache(name string) {
	s.metaMu.Lock()
	delete(s.metaCache, name)
	delete(s.historyWitnesses, name)
	if state, ok := s.historyMarkers[name]; ok && !state.present {
		delete(s.historyMarkers, name)
	}
	s.metaMu.Unlock()
}

// RefreshSessionMeta reads the session's meta from disk again, in place of the
// one this process cached. The agent calls it once when a turn starts, so the
// turn's GetHistory and GetSummary see what another process wrote. It costs one
// meta read: the marker and order witness state stay cached, since a plain read
// proves nothing, and a witness that no longer matches the meta read makes the
// read redo under the session's lock, which drops the whole cache first
// (adoptSessionLock). Like any plain read it takes that lock only when it must
// write first (recover a journal of this process, latch a recovery), and then
// waits for it up to sessionLockWait, as a write does.
func (s *JSONLStore) RefreshSessionMeta(_ context.Context, sessionKey string) error {
	_, err := readSessionLocked(s, sessionKey, func() (SessionMeta, error) {
		s.metaMu.Lock()
		delete(s.metaCache, sanitizeKey(sessionKey))
		s.metaMu.Unlock()
		return s.readMeta(sessionKey)
	})
	return err
}

// readListedMeta is readSessionLocked for a listing, which reads like a sweep:
// a read that must write first tries the session's lock once instead of
// waiting. skipped reports that the lock, or its shard, was in use; whoever
// next takes the lock recovers the session, and the listing leaves it out.
func (s *JSONLStore) readListedMeta(name string) (meta SessionMeta, skipped bool, err error) {
	shard := s.sessionLock(name)
	shard.Lock()
	meta, err = s.readMeta(name)
	shard.Unlock()
	if !errors.Is(err, errSessionLockRequired) {
		return meta, false, err
	}
	unlock, locked, err := s.trySessionLock(name)
	if err != nil || !locked {
		return SessionMeta{}, err == nil, err
	}
	defer unlock()
	meta, err = s.readMeta(name)
	return meta, false, err
}

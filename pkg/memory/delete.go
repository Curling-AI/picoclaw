package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// DeleteSession removes a session from disk: its meta, its transcript, the
// files of the legacy keys its meta names as aliases, and their cache entries.
// Deleting a session that does not exist is not an error.
//
// It holds the session's lock, so an append in flight finishes before the files
// go, and one that follows starts a new session instead of continuing a
// half-removed one.
//
// A conversation from before the canonical keys keeps its history under the
// legacy key. Left behind, it would be promoted back into the canonical session
// on the next message (PromoteAliasHistory), so the aliases go first, each under
// the pair lock that promotion takes. The main session's alias is every agent's
// shared fallback, not one conversation's, and stays.
//
// Clearing a session (empty history, empty summary) is not a delete: the meta
// and the transcript stay, and the session stays in every listing with no
// messages, where each listing keeps paying for it.
func (s *JSONLStore) DeleteSession(_ context.Context, sessionKey string) error {
	if strings.TrimSpace(sessionKey) == "" {
		return errors.New("memory: delete session: empty key")
	}

	var errs []error
	for _, alias := range s.legacyAliases(sessionKey) {
		unlock := s.lockSessionPair(sessionKey, alias)
		if err := s.removeSessionFiles(sanitizeKey(alias)); err != nil {
			errs = append(errs, err)
		}
		unlock()
	}
	// The canonical meta is the only list of the aliases: it stays until every
	// alias is gone, so a retry still finds the ones that failed.
	if len(errs) > 0 {
		return fmt.Errorf("memory: delete session: %w", errors.Join(errs...))
	}

	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()
	if err := s.removeSessionFiles(sanitizeKey(sessionKey)); err != nil {
		return fmt.Errorf("memory: delete session: %w", err)
	}
	return nil
}

// legacyAliases are the aliases in a session's meta that hold a conversation of
// their own: not the session's key, and not the main session's. A meta that
// cannot be read names none; the session itself is still deleted.
func (s *JSONLStore) legacyAliases(sessionKey string) []string {
	l := s.sessionLock(sessionKey)
	l.Lock()
	meta, err := s.readMeta(sessionKey)
	l.Unlock()
	if err != nil {
		return nil
	}
	var aliases []string
	for _, alias := range meta.Aliases {
		if alias == "" || alias == sessionKey || isMainSessionAlias(alias) {
			continue
		}
		aliases = append(aliases, alias)
	}
	return aliases
}

// PruneEmptySessions deletes the sessions that hold nothing and have not been
// written to for minIdle: no message, no summary and nothing in the transcript
// file. It returns the keys it removed. A minIdle of zero or less disables it.
//
// Sessions a clear left behind, before sessions could be deleted, are the
// target: they stay in every listing as empty conversations. The criterion is
// deliberately narrow because it deletes files in production storage. A session
// with a message, a summary, a transcript the meta does not count, a truncated
// history, an unreadable meta or a recent write is never touched. What counts
// as the last write is the files' modification time, as in PruneCronRuns, so a
// session cannot look idle through a meta that says so.
//
// beforeRemove runs before a session's files go, for whatever else is kept per
// session, and gets the meta just read from disk. When it fails the session
// stays and the next pass tries again, so the hook never ends up owning data of
// a session that is gone. A session that fails is skipped, not fatal: the
// others are still pruned and the errors come back joined. ErrKeepSession from
// the hook keeps the session without counting as a failure.
//
// The hook runs under the session's lock, which is what keeps a write from
// landing between the check and the removal. It must not call the session
// store: a call for that session, or for any session of the same lock shard,
// waits for the hook forever. It also holds off writes to that shard while it
// runs, so it has to be quick.
func (s *JSONLStore) PruneEmptySessions(
	minIdle time.Duration,
	now time.Time,
	beforeRemove func(meta SessionMeta) error,
) ([]string, error) {
	if minIdle <= 0 {
		return nil, nil
	}
	cutoff := now.Add(-minIdle)

	var removed []string
	var errs []error
	for _, meta := range s.ListSessionMetas() {
		if !isEmptyMeta(meta) {
			continue
		}
		done, err := s.pruneIfEmpty(meta.Key, cutoff, beforeRemove)
		if errors.Is(err, ErrKeepSession) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if done {
			removed = append(removed, meta.Key)
		}
	}
	return removed, errors.Join(errs...)
}

// ErrKeepSession is what a PruneEmptySessions hook returns to keep a session
// the store would remove. A clear leaves a session that looks like the husk of
// a delete, and the caller may hold something of its own for that session.
var ErrKeepSession = errors.New("memory: session kept by the caller")

func isEmptyMeta(meta SessionMeta) bool {
	return meta.Count == 0 && meta.Skip == 0 && strings.TrimSpace(meta.Summary) == ""
}

// pruneIfEmpty decides again, under the session's lock and from the files
// themselves, what PruneEmptySessions decided from the listing: a message may
// have been written in between, and the cache is not the disk.
func (s *JSONLStore) pruneIfEmpty(key string, cutoff time.Time, beforeRemove func(SessionMeta) error) (bool, error) {
	l := s.sessionLock(key)
	l.Lock()
	defer l.Unlock()

	meta, lastWrite, err := s.readMetaFile(key)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("memory: prune empty sessions: %w", err)
	}
	if meta.Key != key || !isEmptyMeta(meta) {
		return false, nil
	}

	transcript, err := os.Stat(s.jsonlPath(key))
	switch {
	case err == nil:
		if transcript.Size() > 0 {
			return false, nil
		}
		if transcript.ModTime().After(lastWrite) {
			lastWrite = transcript.ModTime()
		}
	case !errors.Is(err, os.ErrNotExist):
		return false, fmt.Errorf("memory: prune empty sessions: stat transcript: %w", err)
	}
	if !lastWrite.Before(cutoff) {
		return false, nil
	}

	if beforeRemove != nil {
		if err := beforeRemove(meta); err != nil {
			return false, fmt.Errorf("memory: prune empty sessions: %w", err)
		}
	}
	if err := s.removeSessionFiles(sanitizeKey(key)); err != nil {
		return false, fmt.Errorf("memory: prune empty sessions: %w", err)
	}
	return true, nil
}

// readMetaFile reads a session's meta from disk, bypassing the cache, with the
// file's modification time. A meta that does not decode reads as one with no
// key, which belongs to no session.
func (s *JSONLStore) readMetaFile(key string) (SessionMeta, time.Time, error) {
	f, err := os.Open(s.metaPath(key))
	if err != nil {
		return SessionMeta{}, time.Time{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return SessionMeta{}, time.Time{}, fmt.Errorf("stat meta: %w", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return SessionMeta{}, time.Time{}, fmt.Errorf("read meta: %w", err)
	}
	var meta SessionMeta
	if json.Unmarshal(data, &meta) != nil {
		return SessionMeta{}, info.ModTime(), nil
	}
	return meta, info.ModTime(), nil
}

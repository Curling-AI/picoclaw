package memory

// What taking a session's lock adopts from the disk. Another process may have
// written the session since this one last held the lock, or died in the middle
// of a write: adoptSessionLock brings this process back in line with the disk
// before the caller reads anything.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// adoptSessionLock runs right after the session's file lock is held, before
// the caller reads anything, in this order:
//   - sessions/ is revalidated before any read under the lock. An NFS client
//     keeps a name it found absent for up to acdirmax (60 s), and taking the
//     lock does not refresh it (K5): a meta, marker, witness or journal another
//     pod created would read as absent, and a latch set from that has no reset;
//   - the session's cache goes, so every read under the lock starts from the
//     disk (the cache rule, session_cache.go);
//   - a journal or decision on disk is resolved before the meta is read.
//
// A failure releases the locks and fails the call without writing.
func (s *JSONLStore) adoptSessionLock(key string) error {
	if err := s.revalidateSessionsDir(); err != nil {
		return err
	}
	s.forgetSessionCache(sanitizeKey(key))
	return s.recoverOrphanJournal(key)
}

// revalidateSessionsDir opens and closes sessions/. Opening a directory makes an
// NFS client fetch its attributes from the server, which drops the names it
// cached as absent once another client changed the folder, so a lookup after it
// sees what the other pod created (K5f). sessionlock does the same for .locks/.
func (s *JSONLStore) revalidateSessionsDir() error {
	dir, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("memory: revalidate sessions folder: %w", err)
	}
	if err = dir.Close(); err != nil {
		return fmt.Errorf("memory: revalidate sessions folder: %w", err)
	}
	if s.sessionsRevalidatedForTest != nil {
		return s.sessionsRevalidatedForTest()
	}
	return nil
}

// recoverOrphanJournal runs under the session's file lock, so a journal or a
// decision on disk belongs to a writer that released that lock or died, in this
// process or another: it is resolved before the meta is read, as the boot sweep
// resolves it. A deletion intent finishes its deletion, and the acquisition
// records that for a sweep (adoptedDeletion).
func (s *JSONLStore) recoverOrphanJournal(key string) error {
	for _, path := range []string{s.historyRecoveryPath(key), s.historyDecisionPath(key)} {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err = s.dropJournalTempLink(info); err != nil {
			return err
		}
		deletion := s.journalIsDeletion(key)
		s.markHistoryPending(key)
		if err = s.recoverHistoryWrite(key); err == nil && deletion {
			s.recordAdoptedDeletion(key)
		}
		return err
	}
	return nil
}

// dropJournalTempLink removes the temporary name a journal kept when its writer
// died between the link and the temporary file's removal (createHistoryJournal).
// Recovery opens some journals as artifacts, which refuses a second link
// (openContentArtifact): left alone, it would fail every acquisition of the
// session. Only a temporary file that is the journal itself goes, and the one
// listing of sessions/ runs only when the journal has a second link.
func (s *JSONLStore) dropJournalTempLink(journal fs.FileInfo) error {
	if !historyJournalHasSecondLink(journal) {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("memory: list history journal temporary files: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), historyJournalTempPrefix) {
			continue
		}
		tmp := filepath.Join(s.dir, entry.Name())
		if info, statErr := os.Lstat(tmp); statErr != nil || !os.SameFile(journal, info) {
			continue
		}
		if err = os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("memory: remove history journal temporary file: %w", err)
		}
	}
	return nil
}

// journalIsDeletion reports whether key's journal is a deletion intent. A
// journal it cannot read or decode is not one here; its recovery reports why.
func (s *JSONLStore) journalIsDeletion(key string) bool {
	data, err := os.ReadFile(s.historyRecoveryPath(key))
	var record historyDeletionRecord
	return err == nil && json.Unmarshal(data, &record) == nil && record.Deletion != nil
}

func (s *JSONLStore) recordAdoptedDeletion(key string) {
	s.fileLocksMu.Lock()
	defer s.fileLocksMu.Unlock()
	if held := s.fileLocks[sessionLockName(key)]; held != nil {
		held.adoptedDeletion = true
	}
}

// adoptedDeletion reports, once per acquisition, that taking key's lock
// finished a deletion intent left on disk. A sweep that finds the session that
// way counts the removal as its own, as when it resumes the intent itself.
func (s *JSONLStore) adoptedDeletion(key string) bool {
	s.fileLocksMu.Lock()
	defer s.fileLocksMu.Unlock()
	held := s.fileLocks[sessionLockName(key)]
	if held == nil || !held.adoptedDeletion {
		return false
	}
	held.adoptedDeletion = false
	return true
}

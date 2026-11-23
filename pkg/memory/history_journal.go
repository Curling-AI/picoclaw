package memory

// The history journal is created exclusively. Under the session's lock its
// name should be free, since adoptSessionLock resolved what was there. A journal
// at that name anyway is another writer's that this process could not see (an
// NFS client's cache of absent names, K5) and is never replaced: link(2) and
// O_CREAT|O_EXCL go to the server and fail with EEXIST whatever the cache says,
// where a rename would overwrite it.

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

// errForeignHistoryJournal: creating a history journal met another writer's
// journal at its name. beginHistoryWrite recovers that journal and writes
// nothing of the caller's, which then runs again from readMeta
// (redoAfterForeignJournal).
//
// shortcut: only the transcript writers redo (AddMessage, AddFullMessage,
// AppendHistoryReceipt, AppendHistoryContent, AppendHistoryContentInput,
// SetHistory, TruncateHistory, SetSummary, Compact). The other content, public
// history and read order recovery writers fail with this error and nothing
// written: the write is lost unless their caller tries it again.
var errForeignHistoryJournal = errors.New("memory: another writer's history journal was at its name")

// historyJournalTempPrefix starts the name of a journal's temporary file. It
// starts with .tmp-, which no session owns (SessionFileName).
const historyJournalTempPrefix = ".tmp-journal-"

// redoAfterForeignJournal runs write once more when it met another writer's
// journal: that journal is recovered and nothing of write's was written, so the
// second run starts again from readMeta, on what the recovery left on disk. The
// caller holds the session's lock throughout; a second foreign journal fails.
func redoAfterForeignJournal[T any](s *JSONLStore, key string, write func() (T, error)) (T, error) {
	value, err := write()
	if !errors.Is(err, errForeignHistoryJournal) {
		return value, err
	}
	s.forgetSessionCache(sanitizeKey(key))
	return write()
}

// redoWriteAfterForeignJournal is redoAfterForeignJournal for a write that
// returns only an error.
func (s *JSONLStore) redoWriteAfterForeignJournal(key string, write func() error) error {
	_, err := redoAfterForeignJournal(s, key, func() (struct{}, error) { return struct{}{}, write() })
	return err
}

// abortHistoryJournal handles a journal that could not be created. Another
// writer's journal at its name is recovered here, under the lock, and the
// caller redoes its write; a journal this call published, even in part, is
// removed, so nothing of the write remains.
func (s *JSONLStore) abortHistoryJournal(key string, published bool, err error) error {
	if errors.Is(err, errForeignHistoryJournal) {
		s.markHistoryPending(key)
		if recoveryErr := s.recoverHistoryWrite(key); recoveryErr != nil {
			return fmt.Errorf("memory: recover another writer's history journal: %w", recoveryErr)
		}
		return err
	}
	if !published {
		return err
	}
	if removeErr := os.Remove(s.historyRecoveryPath(key)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		s.markHistoryPending(key)
		return errors.Join(err, removeErr)
	}
	return err
}

// createHistoryJournal publishes data at path only if no file is there: a
// complete temporary file, synced, is linked to the name. Where the filesystem
// refuses hard links (EPERM, ENOTSUP), the journal is created with O_EXCL
// instead, which refuses the same way. published reports that path holds what
// this call wrote, complete or not.
//
// A crash between the link and the temporary file's removal leaves a journal
// with two links. Recovery opens some journals as artifacts, which refuses a
// second link, so taking the lock removes that temporary name first
// (dropJournalTempLink).
//
// shortcut: without link(2) a crash in the middle of the write can leave a
// torn journal, which recovery fails to decode: the session fails closed until
// it is repaired by hand. EFS support for link(2) is confirmed in Task 18.
func (s *JSONLStore) createHistoryJournal(path string, data []byte) (published bool, err error) {
	if err = s.requireSessionFileLock(path); err != nil {
		return false, err
	}
	if s.writeFile != nil {
		// Test seam for journal faults: it writes in place of the link, after
		// the same check that no journal is there.
		if _, statErr := os.Lstat(path); statErr == nil {
			return false, fmt.Errorf("%w: %s", errForeignHistoryJournal, path)
		}
		return true, s.writeFile(path, data, 0o600)
	}
	tmp, err := writeHistoryJournalTemp(s.dir, data)
	if err != nil {
		return false, err
	}
	link := os.Link
	if s.linkHistoryJournal != nil {
		link = s.linkHistoryJournal
	}
	linkErr := link(tmp, path)
	removeErr := os.Remove(tmp)
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	switch {
	case linkErr == nil && removeErr != nil:
		// The journal would keep a second name, and recovery refuses a journal
		// with more than one link (openContentArtifact): fail, and the caller
		// removes it.
		return true, fmt.Errorf("memory: remove history journal temporary file: %w", removeErr)
	case linkErr == nil:
		syncDirBestEffort(s.dir)
		return true, nil
	case removeErr != nil:
		log.Printf("memory: remove history journal temporary file: %v", removeErr)
	}
	switch {
	case errors.Is(linkErr, fs.ErrExist):
		return false, fmt.Errorf("%w: %s", errForeignHistoryJournal, path)
	case errors.Is(linkErr, errors.ErrUnsupported) || errors.Is(linkErr, fs.ErrPermission):
		return createHistoryJournalExclusive(path, data)
	}
	return false, fmt.Errorf("memory: link history journal: %w", linkErr)
}

// writeHistoryJournalTemp writes data to a new synced file in dir.
func writeHistoryJournalTemp(dir string, data []byte) (string, error) {
	file, err := os.CreateTemp(dir, historyJournalTempPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("memory: create history journal: %w", err)
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", errors.Join(fmt.Errorf("memory: write history journal: %w", err), os.Remove(file.Name()))
	}
	return file.Name(), nil
}

// createHistoryJournalExclusive is the journal's create without link(2).
func createHistoryJournalExclusive(path string, data []byte) (bool, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, fmt.Errorf("%w: %s", errForeignHistoryJournal, path)
	}
	if err != nil {
		return false, fmt.Errorf("memory: create history journal: %w", err)
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return true, fmt.Errorf("memory: write history journal: %w", err)
	}
	syncDirBestEffort(filepath.Dir(path))
	return true, nil
}

// syncDirBestEffort makes the new journal's name durable where the filesystem
// syncs directories, as fileutil.WriteFileAtomic did for the journal.
func syncDirBestEffort(dir string) {
	if file, err := os.Open(dir); err == nil {
		_ = file.Sync()
		_ = file.Close()
	}
}

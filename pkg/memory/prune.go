package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cronRunKeyPrefixes are the session-key prefixes of cron RUN sessions: a
// normal run (agent:cron-<job>-<uuid>) and a run pinned to a model
// (agent:cronmodel-<job>-<uuid>). Matching only the first is a known pitfall —
// the model variant has leaked into listings that filtered just "agent:cron-".
var cronRunKeyPrefixes = []string{"agent:cron-", "agent:cronmodel-"}

// isCronRunFile reports whether a session FILE belongs to a cron run. Files are
// named after the sanitized key (':' becomes '_'), so the key prefixes are
// matched in their on-disk form.
func isCronRunFile(name string) bool {
	for _, prefix := range cronRunKeyPrefixes {
		if strings.HasPrefix(name, sanitizeKey(prefix)) {
			return true
		}
	}
	return false
}

// PruneCronRuns deletes the files of cron runs whose last write is older than
// retention, and reports how many runs it removed.
//
// Every cron execution persists a session of its own, and nothing ever removed
// them: a 5-minute job leaves ~288 husks a day. One prod pod had accumulated
// 7.7k session files, and since listing sessions reads every .meta.json off
// EFS, the listing outgrew the gateway's timeout and the web sidebar came back
// empty even though the conversations were all still there.
//
// Only cron husks are considered — conversations (web and channel) never carry
// these prefixes and are never touched. Runs are expected to age out: they are
// shown under Automações (Execuções), which becomes a retention window rather
// than an unbounded log.
//
// Files are selected by modification time rather than by the meta's UpdatedAt
// so that a corrupt or unreadable meta cannot pin a husk on disk forever. An
// in-flight run is never at risk: its files were just written, so they fall
// far inside the retention window. A retention of zero or less disables
// pruning entirely.
func (s *JSONLStore) PruneCronRuns(retention time.Duration, now time.Time) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("memory: prune cron runs: %w", err)
	}

	cutoff := now.Add(-retention)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		if !isCronRunFile(entry.Name()) {
			continue
		}
		info, ierr := entry.Info()
		if ierr != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		sanitized := strings.TrimSuffix(entry.Name(), ".meta.json")
		if err := s.removeSessionFiles(sanitized); err != nil {
			continue
		}
		removed++
	}
	return removed, nil
}

// removeSessionFiles deletes both files of a session addressed by its already
// sanitized name and drops it from the meta cache, so a later listing cannot
// resurrect it from memory.
func (s *JSONLStore) removeSessionFiles(sanitized string) error {
	metaPath := filepath.Join(s.dir, sanitized+".meta.json")
	jsonlPath := filepath.Join(s.dir, sanitized+".jsonl")

	// The meta goes first: while it exists the session is still listable, so
	// removing it before the transcript avoids exposing a session whose
	// history has already gone.
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(jsonlPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	s.metaMu.Lock()
	delete(s.metaCache, sanitized)
	s.metaMu.Unlock()
	return nil
}

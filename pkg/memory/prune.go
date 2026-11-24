package memory

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// cronRunKeyPrefixes are the session-key prefixes of cron RUN sessions: a
// normal run (agent:cron-<job>-<uuid>) and a run pinned to a model
// (agent:cronmodel-<job>-<uuid>). Matching only the first is a known pitfall —
// the model variant has leaked into listings that filtered just "agent:cron-".
var cronRunKeyPrefixes = []string{"agent:cron-", "agent:cronmodel-"}

// maxCronRunsPerJob is how many runs of one automation survive the prune: a job
// that fires every few minutes keeps thousands of runs inside the retention
// window, and every run's files sit in the one directory each listing reads.
const maxCronRunsPerJob = 50

// cronRunSettleAge is how long a run's meta must go unwritten before the cap may
// remove it: manual runs overlap scheduled ones, and a run untouched for a day has ended.
const cronRunSettleAge = 24 * time.Hour

type cronRun struct {
	sanitized string
	modified  time.Time
}

// IsCronRunSessionKey reports whether a session key belongs to a cron run.
// Both variants count, for the same reason isCronRunFile matches both.
func IsCronRunSessionKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, prefix := range cronRunKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

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
// Candidates are selected by modification time rather than the meta's UpdatedAt.
// Recovery guards can retain a candidate whose history cannot safely be removed.
// An in-flight run is never at risk: its files were just written, so they fall
// far inside the retention window. Within the window, each automation keeps
// only its newest maxCronRunsPerJob runs plus any not yet settled (capCronRuns).
// A retention of zero or less disables pruning.
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
	seen := make(map[string]bool)
	recent := make(map[string][]cronRun)
	skipped := 0
	failures := cronPruneFailures{}
	defer failures.log()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		suffix := ".meta.json"
		if strings.HasSuffix(entry.Name(), ".jsonl.commit.json") {
			suffix = ".jsonl.commit.json"
		} else if strings.HasSuffix(entry.Name(), ".jsonl.commit.decision") {
			suffix = ".jsonl.commit.decision"
		} else if !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		if !isCronRunFile(entry.Name()) {
			continue
		}
		info, ierr := entry.Info()
		if ierr == nil && suffix == ".meta.json" && !info.ModTime().Before(cutoff) {
			sanitized := strings.TrimSuffix(entry.Name(), suffix)
			if job, ok := cronRunJob(sanitized); ok {
				recent[job] = append(recent[job], cronRun{sanitized: sanitized, modified: info.ModTime()})
			}
		}
		if ierr != nil || (suffix != ".jsonl.commit.json" && !info.ModTime().Before(cutoff)) {
			continue
		}
		sanitized := strings.TrimSuffix(entry.Name(), suffix)
		if seen[sanitized] {
			continue
		}
		seen[sanitized] = true
		unlock, locked, lockErr := s.trySessionLock(sanitized)
		if lockErr != nil {
			log.Printf("memory: prune cron run %s: %v", sanitized, lockErr)
			continue
		}
		if !locked {
			skipped++
			continue
		}
		pruned, pruneErr := s.pruneSessionFilesLocked(sanitized, cutoff)
		unlock()
		if pruneErr != nil {
			failures.add(sanitized, pruneErr)
		} else if pruned {
			removed++
		}
	}
	capped, capSkipped := s.capCronRuns(recent, now, failures)
	logSkippedSessions(skipped + capSkipped)
	return removed + capped, nil
}

// logSkippedSessions reports, once per sweep, the sessions it left alone
// because another process held their lock.
func logSkippedSessions(skipped int) {
	if skipped > 0 {
		log.Printf("memory: %d sessions skipped: locked by another process", skipped)
	}
}

// capCronRuns removes each automation's runs past its newest maxCronRunsPerJob,
// newest by .meta.json mtime like the age rule. Between writes an executing run
// holds no lock or journal, and a manual run can outlast 50 scheduled ones, so
// only runs settled for cronRunSettleAge go. The removal takes the session lock
// and resumes a pending deletion, and recovery guards can still retain a run.
func (s *JSONLStore) capCronRuns(
	recent map[string][]cronRun, now time.Time, failures cronPruneFailures,
) (removed, skipped int) {
	settled := now.Add(-cronRunSettleAge)
	for _, runs := range recent {
		if len(runs) <= maxCronRunsPerJob {
			continue
		}
		newestFirst := slices.SortedFunc(slices.Values(runs), func(a, b cronRun) int {
			return b.modified.Compare(a.modified)
		})
		for _, run := range newestFirst[maxCronRunsPerJob:] {
			if !run.modified.Before(settled) {
				continue
			}
			unlock, locked, lockErr := s.trySessionLock(run.sanitized)
			if lockErr != nil {
				log.Printf("memory: cap cron run %s: %v", run.sanitized, lockErr)
				continue
			}
			if !locked {
				skipped++
				continue
			}
			err := s.removeSessionFilesLocked(run.sanitized)
			unlock()
			if err != nil {
				failures.add(run.sanitized, err)
			} else {
				removed++
			}
		}
	}
	return removed, skipped
}

// cronPruneFailures counts the runs a cron prune could not remove, by kind, and
// logs one line per kind with the first run's error: a run whose session lock
// was lost, or that a guard keeps, shows up once per sweep, not once per run.
type cronPruneFailures map[string]*cronPruneFailure

type cronPruneFailure struct {
	count int
	first string
}

var cronPruneFailureKinds = []error{ErrSessionLockLost, ErrSessionBusy, ErrHistoryRecoveryRequired}

func (f cronPruneFailures) add(sanitized string, err error) {
	kind := "other"
	for _, known := range cronPruneFailureKinds {
		if errors.Is(err, known) {
			kind = known.Error()
			break
		}
	}
	if failure := f[kind]; failure != nil {
		failure.count++
		return
	}
	f[kind] = &cronPruneFailure{count: 1, first: fmt.Sprintf("%s: %v", sanitized, err)}
}

func (f cronPruneFailures) log() {
	for _, kind := range slices.Sorted(maps.Keys(f)) {
		log.Printf("memory: prune cron runs: %d not removed (%s), first %s", f[kind].count, kind, f[kind].first)
	}
}

// cronRunJob returns the automation a run file belongs to. Both prefixes share
// the job, and the run ID is the trailing UUID because job IDs may hold '-'.
func cronRunJob(sanitized string) (string, bool) {
	for _, prefix := range cronRunKeyPrefixes {
		rest, isRun := strings.CutPrefix(sanitized, sanitizeKey(prefix))
		cut := len(rest) - len(uuid.Nil.String()) - 1
		if isRun && cut > 0 && rest[cut] == '-' && uuid.Validate(rest[cut+1:]) == nil {
			return rest[:cut], true
		}
	}
	return "", false
}

func (s *JSONLStore) pruneSessionFilesLocked(sanitized string, cutoff time.Time) (bool, error) {
	if resumed, err := s.resumeHistoryDeletion(sanitized); resumed || err != nil {
		return resumed && err == nil, err
	}
	for _, path := range []string{
		s.metaPath(sanitized), s.jsonlPath(sanitized), s.historyRecoveryPath(sanitized), s.historyDecisionPath(sanitized),
		s.historyOrderWitnessPath(sanitized), s.publicHistoryPath(sanitized), s.publicIndexPath(sanitized),
		s.publicIndexPreparationPath(sanitized), s.publicIndexBuildingPath(sanitized),
	} {
		info, err := os.Stat(path)
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}
		if err == nil && !info.ModTime().Before(cutoff) {
			return false, nil
		}
	}
	if err := s.removeSessionFilesLocked(sanitized); err != nil {
		return false, err
	}
	return true, nil
}

// removeSessionFiles deletes all files of a session addressed by its already
// sanitized name and drops it from the meta cache, so a later listing cannot
// resurrect it from memory.
func (s *JSONLStore) removeSessionFiles(sanitized string) error {
	unlock, err := s.lockSession(sanitized)
	if err != nil {
		return err
	}
	defer unlock()
	return s.removeSessionFilesLocked(sanitized)
}

func (s *JSONLStore) removeSessionFilesLocked(sanitized string, purpose ...string) error {
	if err := s.requireSessionLock(sanitized); err != nil {
		return err
	}
	if resumed, err := s.resumeHistoryDeletion(sanitized); resumed || err != nil {
		return err
	}
	meta, err := s.readMeta(sanitized)
	if err != nil {
		return err
	}
	if meta, err = s.metaAfterOverdueCompaction(context.Background(), meta.Key, meta, s.readMeta); err != nil {
		return err
	}
	if operationErr := s.contentPendingGuard(meta); operationErr != nil {
		return operationErr
	}
	if operationErr := s.guardHistoryMutation(sanitized, meta); operationErr != nil {
		return operationErr
	}
	if operationErr := s.checkContentGarbageCollection(context.Background(), meta.Key, meta); operationErr != nil {
		return operationErr
	}
	images, err := s.contentImagesForDeletion(context.Background(), meta.Key, meta)
	if err != nil {
		return err
	}
	artifacts, floorImages, err := s.contentStorageForDeletion(context.Background(), meta.Key, meta)
	if err != nil {
		return err
	}
	images = append(images, floorImages...)
	if len(images)+len(artifacts) > 0 || s.requiresPublicHistoryCleanup() {
		origin := "retention"
		if len(purpose) > 0 {
			origin = purpose[0]
		}
		if err = s.beginHistoryDeletion(context.Background(), meta.Key, origin, images, artifacts); err != nil {
			return err
		}
		return s.recoverHistoryWrite(meta.Key)
	}
	// One proof of the lock before the removals start, not one per file.
	if err = s.checkSessionLock(sanitized); err != nil {
		return err
	}
	metaPath := filepath.Join(s.dir, sanitized+".meta.json")
	jsonlPath := filepath.Join(s.dir, sanitized+".jsonl")
	s.markHistoryPending(sanitized)
	if err := s.deleteContentImages(meta.Key, images); err != nil {
		return err
	}
	if err := s.deleteContentStorage(artifacts); err != nil {
		return err
	}

	// The meta goes first: while it exists the session is still listable, so
	// removing it before the transcript avoids exposing a session whose
	// history has already gone.
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(jsonlPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.historyRecoveryPath(sanitized)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.historyDecisionPath(sanitized)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.contentDomainPath(sanitized)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.publicHistoryPath(sanitized)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, path := range []string{s.publicIndexPath(sanitized), s.publicIndexPreparationPath(sanitized), s.publicIndexBuildingPath(sanitized)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(s.historyOrderWitnessPath(sanitized)); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.removeSessionLockFile(sanitized)

	s.metaMu.Lock()
	delete(s.metaCache, sanitized)
	delete(s.historyPending, sanitized)
	delete(s.historyMarkers, sanitized)
	delete(s.historyWitnesses, sanitized)
	s.metaMu.Unlock()
	return nil
}

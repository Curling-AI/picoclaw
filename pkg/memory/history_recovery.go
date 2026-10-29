package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

var ErrHistoryRecoveryRequired = errors.New("history source requires explicit recovery")

func historyRecoveryMarkerError(marker json.RawMessage) error {
	if len(marker) == 0 {
		return nil
	}
	var required *bool
	if json.Unmarshal(marker, &required) != nil || required == nil || *required {
		return ErrHistoryRecoveryRequired
	}
	return nil
}

func historyRecordRecoveryRequired(data []byte) error {
	var record struct {
		RecoveryRequired json.RawMessage `json:"history_recovery_required"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	return historyRecoveryMarkerError(record.RecoveryRequired)
}

func (s *JSONLStore) historyRecoveryRequiredPath(key string) string {
	return s.jsonlPath(key) + ".recovery-required"
}

func (s *JSONLStore) hasHistoryRecoveryMarker(key string) (bool, error) {
	_, err := os.Stat(s.historyRecoveryRequiredPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

type historyMarkerState struct {
	present bool
	synced  bool
}

// Proof boundaries recheck the durable sidecar even with warm metadata. A
// positive observation latches the cache but does not claim it has been synced.
func (s *JSONLStore) probeHistoryRecoveryMarker(key string, meta *SessionMeta) error {
	marked, err := s.hasHistoryRecoveryMarker(key)
	if err != nil {
		return err
	}
	if marked {
		s.rememberHistoryRecoveryMarker(key, true)
		meta.HistoryRecoveryRequired = true
	}
	return nil
}

func (s *JSONLStore) historyRecoveryMarkerState(key string) (bool, error) {
	sanitized := sanitizeKey(key)
	s.metaMu.RLock()
	state, known := s.historyMarkers[sanitized]
	s.metaMu.RUnlock()
	if known {
		return state.present, nil
	}
	marked, err := s.hasHistoryRecoveryMarker(key)
	if err != nil {
		return false, err
	}
	return s.rememberHistoryRecoveryMarker(key, marked), nil
}

// Only the owning writer creates the latch. Cache both the cold absence and
// durable presence; a pending journal may evict metadata but not this state.
func (s *JSONLStore) rememberHistoryRecoveryMarker(key string, marked bool) bool {
	return s.cacheHistoryRecoveryMarker(key, marked, false)
}

func (s *JSONLStore) cacheHistoryRecoveryMarker(key string, marked, synced bool) bool {
	sanitized := sanitizeKey(key)
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if s.historyMarkers == nil {
		s.historyMarkers = make(map[string]historyMarkerState)
	}
	state := s.historyMarkers[sanitized]
	state.present = state.present || marked
	state.synced = state.synced || synced
	s.historyMarkers[sanitized] = state
	marked = state.present
	if meta, ok := s.metaCache[sanitized]; ok && marked {
		meta.HistoryRecoveryRequired = true
		s.metaCache[sanitized] = meta
	}
	return marked
}

// This latch has no automatic reset. Legacy binaries can erase unknown JSON
// fields while rewriting metadata or rows; a separate durable marker survives
// those rewrites. Its existence is sufficient, even if its contents are torn.
func (s *JSONLStore) markHistoryRecoveryRequired(key string, meta *SessionMeta) error {
	exists, err := s.historyRecoveryMarkerState(key)
	if err != nil {
		return err
	}
	s.metaMu.RLock()
	synced := s.historyMarkers[sanitizeKey(key)].synced
	s.metaMu.RUnlock()
	if exists && synced {
		meta.HistoryRecoveryRequired = true
		return nil
	}
	if !exists {
		if err := s.writeSessionFile(
			s.historyRecoveryRequiredPath(key), []byte("{\"history_recovery_required\":true}\n"), 0o600,
		); err != nil {
			return err
		}
	}
	if err := s.syncHistoryFile(s.historyRecoveryRequiredPath(key)); err != nil {
		return err
	}
	meta.HistoryRecoveryRequired = true
	s.cacheHistoryRecoveryMarker(key, true, true)
	return nil
}

func (s *JSONLStore) applyHistoryRecoveryMarker(key string, meta *SessionMeta, marked bool) error {
	if marked {
		meta.HistoryRecoveryRequired = true
		return nil
	}
	if meta.HistoryRecoveryRequired {
		return s.markHistoryRecoveryRequired(key, meta)
	}
	return nil
}

// The immutable journal precedes transcript writes. A separate, small decision
// binds recovery to its exact journal without rewriting the rollback backup.
type historyWriteRecovery struct {
	Meta              SessionMeta          `json:"meta"`
	Next              SessionMeta          `json:"next"`
	AppendOffset      *int64               `json:"appendOffset,omitempty"`
	Data              []byte               `json:"data,omitempty"`
	Existed           bool                 `json:"existed,omitempty"`
	NextSummaryDigest string               `json:"nextSummaryDigest,omitempty"`
	Storage           *contentStorageWrite `json:"contentStorage,omitempty"`
}

type historyWriteDecision string

const (
	historyWriteUnknown historyWriteDecision = ""
	historyWriteOld     historyWriteDecision = "old"
	historyWriteNew     historyWriteDecision = "new"
)

type historyDecisionRecord struct {
	Journal  string               `json:"journal"`
	Decision historyWriteDecision `json:"decision"`
}

type historyRecoveryMode uint8

const (
	historyColdRecovery historyRecoveryMode = iota
	historyFailedWrite
)

func (s *JSONLStore) historyRecoveryPath(key string) string {
	return s.jsonlPath(key) + ".commit.json"
}

func (s *JSONLStore) historyDecisionPath(key string) string {
	return s.jsonlPath(key) + ".commit.decision"
}

func (s *JSONLStore) recoverPendingHistoryWrites() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".jsonl.commit.json")
		if key == entry.Name() {
			key = strings.TrimSuffix(entry.Name(), ".jsonl.commit.decision")
		}
		if key == entry.Name() || seen[key] {
			continue
		}
		seen[key] = true
		s.markHistoryPending(key)
		if recoveryErr := s.recoverHistoryWrite(key); recoveryErr != nil {
			log.Printf("memory: pending history recovery failed for %s: %v", entry.Name(), recoveryErr)
		}
	}
	return nil
}

func (s *JSONLStore) beginHistoryWrite(key string, recovery historyWriteRecovery) error {
	// NEW is only a commit fingerprint; recovery preserves its actual disk
	// metadata. Only OLD needs a summary backup, which can be large.
	// Legacy metadata-only summary/truncation changes remain valid after an
	// append journal. Semantic checkpoints bind those changes independently.
	if state := recovery.Next.ContentIdentity; state != nil &&
		(state.MutationVersion != 0 || state.storage != nil) {
		recovery.NextSummaryDigest = contentDigest([]byte(recovery.Next.Summary))
	}
	recovery.Next.Summary = ""
	data, err := json.Marshal(recovery)
	if err != nil {
		return err
	}
	if operationErr := s.writeSessionFile(s.historyRecoveryPath(key), data, 0o600); operationErr != nil {
		if removeErr := os.Remove(
			s.historyRecoveryPath(key),
		); removeErr != nil &&
			!errors.Is(removeErr, os.ErrNotExist) {
			s.markHistoryPending(key)
			return errors.Join(operationErr, removeErr)
		}
		return operationErr
	}
	s.markHistoryPending(key)
	if witnessErr := s.ensureDurableHistoryOrderWitness(key, recovery); witnessErr != nil {
		decision, recoveryErr := s.recoverHistoryWriteFor(key, historyFailedWrite)
		if decision == historyWriteOld && recoveryErr == nil {
			// No transcript or metadata publication follows a failed witness
			// phase. Only fully recovered OLD permits a content-only retry.
			return errors.Join(ErrHistoryReceiptUnavailable, witnessErr)
		}
		return fmt.Errorf("%w: witness activation outcome unconfirmed: %v",
			ErrHistoryOrderUnavailable, errors.Join(witnessErr, recoveryErr))
	}
	return nil
}

func (s *JSONLStore) markHistoryPending(key string) {
	sanitized := sanitizeKey(key)
	s.metaMu.Lock()
	s.historyPending[sanitized] = true
	delete(s.metaCache, sanitized)
	s.metaMu.Unlock()
}

func (s *JSONLStore) finishHistoryWrite(key string, cleanup ...string) error {
	for _, path := range append([]string{s.historyRecoveryPath(key)}, cleanup...) {
		if operationErr := os.Remove(path); operationErr != nil && !errors.Is(operationErr, os.ErrNotExist) {
			s.markHistoryPending(key)
			return &historyCleanupError{cause: fmt.Errorf("memory: remove history recovery: %w", operationErr)}
		}
	}
	s.metaMu.Lock()
	delete(s.historyPending, sanitizeKey(key))
	s.metaMu.Unlock()
	return nil
}

func (s *JSONLStore) recoverHistoryWrite(key string) error {
	_, operationErr := s.recoverHistoryWriteDecision(key)
	return operationErr
}

// NEW can be durable even when only journal cleanup failed. Receipt-aware
// callers must retain this distinction instead of interpreting every error as OLD.
func (s *JSONLStore) recoverHistoryWriteDecision(key string) (historyWriteDecision, error) {
	return s.recoverHistoryWriteFor(key, historyColdRecovery)
}

func (s *JSONLStore) recoverFailedHistoryWrite(key string) error {
	_, operationErr := s.recoverHistoryWriteFor(key, historyFailedWrite)
	return operationErr
}

func (s *JSONLStore) recoverHistoryWriteFor(key string, mode historyRecoveryMode) (historyWriteDecision, error) {
	data, readErr := os.ReadFile(s.historyRecoveryPath(key))
	if errors.Is(readErr, os.ErrNotExist) {
		if operationErr := s.finishHistoryWrite(key, s.historyDecisionPath(key)); operationErr != nil {
			return historyWriteUnknown, operationErr
		}
		s.metaMu.Lock()
		delete(s.metaCache, sanitizeKey(key))
		s.metaMu.Unlock()
		return historyWriteUnknown, nil
	}
	s.markHistoryPending(key)
	if readErr != nil {
		return historyWriteUnknown, fmt.Errorf("memory: read history recovery: %w", readErr)
	}
	var variant historyDeletionRecord
	if operationErr := json.Unmarshal(data, &variant); operationErr != nil {
		return historyWriteUnknown, fmt.Errorf("memory: decode history recovery: %w", operationErr)
	}
	if variant.Deletion != nil {
		return historyWriteOld, s.recoverHistoryDeletion(key, data)
	}
	_, metaErr := os.Stat(s.metaPath(key))
	_, dataErr := os.Stat(s.jsonlPath(key))
	if errors.Is(metaErr, os.ErrNotExist) && errors.Is(dataErr, os.ErrNotExist) {
		// A completed external deletion must not be undone by its old journal.
		return historyWriteOld, s.finishHistoryWrite(key, s.historyDecisionPath(key))
	}
	var recovery historyWriteRecovery
	if operationErr := json.Unmarshal(data, &recovery); operationErr != nil {
		return historyWriteUnknown, fmt.Errorf("memory: decode history recovery: %w", operationErr)
	}
	recovery.Next.summaryCheckpoint = recovery.NextSummaryDigest
	if recovery.Meta.Key == "" ||
		filepath.Base(s.historyRecoveryPath(recovery.Meta.Key)) != filepath.Base(s.historyRecoveryPath(key)) {
		return historyWriteUnknown, errors.New("invalid history recovery key")
	}
	if recovery.Next.HistoryBytes == nil || *recovery.Next.HistoryBytes < 0 ||
		(recovery.NextSummaryDigest != "" && !validHistoryDigest(recovery.NextSummaryDigest)) ||
		recovery.Next.HistoryFileGeneration == "" || recovery.Next.Count < 0 ||
		recovery.Next.Skip < 0 || recovery.Next.Skip > recovery.Next.Count ||
		(recovery.AppendOffset != nil && (*recovery.AppendOffset < 0 || *recovery.AppendOffset > *recovery.Next.HistoryBytes)) {
		return historyWriteUnknown, errors.New("invalid history recovery checkpoint")
	}
	if !validContentStorageWrite(key, recovery) {
		return historyWriteUnknown, errors.New("invalid content storage recovery checkpoint")
	}
	current, currentErr := s.readRecoveryMeta(key)
	decision, err := s.loadHistoryDecision(key, data)
	if err != nil {
		return historyWriteUnknown, err
	}
	if decision == historyWriteUnknown {
		decision, err = chooseHistoryDecision(current, currentErr, recovery, mode)
		if err != nil {
			return historyWriteUnknown, err
		}
		if operationErr := s.recordHistoryDecision(key, data, decision); operationErr != nil {
			return historyWriteUnknown, operationErr
		}
	}
	if err = s.checkContentRecoveryCheckpoint(key, recovery.Meta, recovery.Next); err != nil {
		return decision, err
	}
	if decision == historyWriteNew {
		if currentErr != nil {
			return decision, currentErr
		}
		if !historyNewCheckpointMatches(current, recovery.Next) {
			return decision, errors.New("memory: decided NEW metadata no longer matches its checkpoint")
		}
		if operationErr := s.recoverContentStorageNew(
			context.Background(),
			key,
			current,
			recovery.Storage,
		); operationErr != nil {
			if !onlyContentIdentityUnavailable(operationErr) {
				return decision, operationErr
			}
			// NEW input is decided. Losing optional identity artifacts closes
			// proof in the external floor, without reconstructing them or
			// keeping ordinary transcript writes permanently fenced.
			if operationErr = s.writeContentCheckpointAvailability(key, current, true); operationErr != nil {
				return decision, operationErr
			}
			// No artifact retirement after proof-loss recovery.
			recovery.Storage = nil
		}
		// Readable NEW metadata can follow a failed file/directory sync. Retry
		// the actual durability phases before retiring its authoritative journal.
		if recovery.Meta.ContentIdentity != nil || recovery.Next.ContentIdentity != nil {
			if operationErr := s.syncHistoryMetadata(key); operationErr != nil {
				return decision, operationErr
			}
		}
		if operationErr := s.recoverContentDomain(
			key,
			recovery.Meta,
			current,
		); operationErr != nil {
			return decision, operationErr
		}
		s.storeCachedMeta(sanitizeKey(key), current)
	} else {
		if operationErr := s.recoverContentStorageOld(recovery.Storage); operationErr != nil {
			return decision, operationErr
		}
		if recovery.AppendOffset != nil {
			err = s.rollbackHistoryAppend(key, *recovery.AppendOffset)
		} else {
			err = s.restoreRawJSONL(key, recovery.Data, recovery.Existed)
		}
		if err != nil {
			return decision, err
		}
		if operationErr := s.writeMeta(key, recovery.Meta); operationErr != nil {
			return decision, operationErr
		}
		if operationErr := s.restoreContentCheckpoint(key, recovery.Meta, recovery.Next); operationErr != nil {
			return decision, operationErr
		}
	}
	if operationErr := s.finishHistoryWrite(key, s.historyDecisionPath(key)); operationErr != nil {
		return decision, operationErr
	}
	s.retireContentStorageWrite(context.Background(), key, recovery.Storage, decision == historyWriteNew)
	return decision, nil
}

func (s *JSONLStore) readRecoveryMeta(key string) (SessionMeta, error) {
	data, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		return SessionMeta{}, err
	}
	var meta SessionMeta
	if operationErr := json.Unmarshal(data, &meta); operationErr != nil {
		return SessionMeta{}, operationErr
	}
	return meta, nil
}

func chooseHistoryDecision(
	current SessionMeta,
	err error,
	recovery historyWriteRecovery,
	mode historyRecoveryMode,
) (historyWriteDecision, error) {
	var syntax *json.SyntaxError
	var value *json.UnmarshalTypeError
	if errors.Is(err, os.ErrNotExist) ||
		(mode == historyFailedWrite && (errors.As(err, &syntax) || errors.As(err, &value))) {
		return historyWriteOld, nil
	}
	if err != nil {
		return historyWriteUnknown, fmt.Errorf("memory: uncertain recovery metadata: %w", err)
	}
	if historyNewCheckpointMatches(current, recovery.Next) {
		return historyWriteNew, nil
	}
	if historyCheckpointMatches(current, recovery.Meta) && current.Skip == recovery.Meta.Skip &&
		((current.HistoryBytes == nil && recovery.Meta.HistoryBytes == nil) ||
			(current.HistoryBytes != nil && recovery.Meta.HistoryBytes != nil &&
				*current.HistoryBytes == *recovery.Meta.HistoryBytes)) {
		return historyWriteOld, nil
	}
	return historyWriteUnknown, errors.New("memory: recovery metadata matches neither OLD nor NEW")
}

func historyNewCheckpointMatches(current, next SessionMeta) bool {
	return (next.summaryCheckpoint == "" || contentDigest([]byte(current.Summary)) == next.summaryCheckpoint) &&
		historyCheckpointMatches(current, next) && current.Skip >= next.Skip && current.Skip <= current.Count &&
		(current.HistoryBytes == nil || *current.HistoryBytes == *next.HistoryBytes)
}

func historyCheckpointMatches(current, expected SessionMeta) bool {
	return contentCheckpointMatches(current, expected) && current.HistoryGeneration == expected.HistoryGeneration &&
		current.HistoryFileGeneration == expected.HistoryFileGeneration &&
		current.HistoryTip == expected.HistoryTip && current.Count == expected.Count &&
		current.HistoryPrefix == expected.HistoryPrefix &&
		current.HistoryOrderVersion == expected.HistoryOrderVersion &&
		current.HistoryStreamID == expected.HistoryStreamID && current.HistorySequence == expected.HistorySequence &&
		current.HistoryLegacyFileGeneration == expected.HistoryLegacyFileGeneration &&
		current.HistoryLegacyBytes == expected.HistoryLegacyBytes &&
		current.HistoryOrderWitnessVersion == expected.HistoryOrderWitnessVersion &&
		current.HistoryRecoveryRequired == expected.HistoryRecoveryRequired
}

func journalDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (s *JSONLStore) loadHistoryDecision(key string, journal []byte) (historyWriteDecision, error) {
	data, err := os.ReadFile(s.historyDecisionPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return historyWriteUnknown, nil
	}
	if err != nil {
		return historyWriteUnknown, fmt.Errorf("memory: read history decision: %w", err)
	}
	var record historyDecisionRecord
	if operationErr := json.Unmarshal(data, &record); operationErr != nil {
		return historyWriteUnknown, fmt.Errorf("memory: decode history decision: %w", operationErr)
	}
	if record.Journal != journalDigest(journal) ||
		(record.Decision != historyWriteOld && record.Decision != historyWriteNew) {
		return historyWriteUnknown, errors.New("invalid history recovery decision")
	}
	if syncErr := s.syncHistoryDecision(key); syncErr != nil {
		return historyWriteUnknown, syncErr
	}
	return record.Decision, nil
}

func (s *JSONLStore) recordHistoryDecision(key string, journal []byte, decision historyWriteDecision) error {
	data, err := json.Marshal(historyDecisionRecord{Journal: journalDigest(journal), Decision: decision})
	if err != nil {
		return err
	}
	if operationErr := s.writeSessionFile(s.historyDecisionPath(key), data, 0o600); operationErr != nil {
		return operationErr
	}
	return s.syncHistoryDecision(key)
}

func (s *JSONLStore) syncHistoryDecision(key string) error {
	// WriteFileAtomic may use a direct-write fallback. Sync the decision
	// and its directory before rollback or receipt-aware confirmation of NEW.
	return s.syncHistoryFile(s.historyDecisionPath(key))
}

func (s *JSONLStore) syncHistoryFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if syncErr := errors.Join(file.Sync(), file.Close()); syncErr != nil {
		return syncErr
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

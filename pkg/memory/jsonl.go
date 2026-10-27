package memory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/messageutil"
)

const (
	// numLockShards is the fixed number of mutexes used to serialize
	// per-session access. Using a sharded array instead of a map keeps
	// memory bounded regardless of how many sessions are created over
	// the lifetime of the process — important for a long-running daemon.
	numLockShards = 64

	// maxLineSize is the maximum size of a single JSON line in a .jsonl
	// file. Tool results (read_file, web search, etc.) can be large, so
	// we set a generous limit. The scanner starts at 64 KB and grows
	// only as needed up to this cap.
	maxLineSize = 10 * 1024 * 1024 // 10 MB
)

// SessionMeta holds per-session metadata stored in a .meta.json file.
//
// Scope is stored as raw JSON so pkg/memory can stay decoupled from the
// higher-level session package while still preserving structured scope data.
type SessionMeta struct {
	HistoryEpoch            int64           `json:"history_epoch,omitempty"`
	HistoryFileEpoch        int64           `json:"history_file_epoch,omitempty"`
	HistoryGeneration       string          `json:"history_generation,omitempty"`
	HistoryFileGeneration   string          `json:"history_file_generation,omitempty"`
	HistoryPrefix           int64           `json:"history_prefix,omitempty"`
	HistoryParentGeneration string          `json:"history_parent_generation,omitempty"`
	HistoryRetainedUntil    int64           `json:"history_retained_until,omitempty"`
	HistoryMutationRunID    string          `json:"history_mutation_run_id,omitempty"`
	HistoryDigest           string          `json:"history_digest,omitempty"`
	HistoryTip              string          `json:"history_tip,omitempty"`
	HistoryBytes            *int64          `json:"history_bytes,omitempty"`
	HistoryRecoveryRequired bool            `json:"history_recovery_required,omitempty"`
	Key                     string          `json:"key"`
	Summary                 string          `json:"summary"`
	Skip                    int             `json:"skip"`
	Count                   int             `json:"count"`
	CreatedAt               time.Time       `json:"created_at"`
	UpdatedAt               time.Time       `json:"updated_at"`
	Scope                   json.RawMessage `json:"scope,omitempty"`
	Aliases                 []string        `json:"aliases,omitempty"`

	HistoryOrderVersion         uint8  `json:"history_order_version,omitempty"`
	HistoryStreamID             string `json:"history_stream_id,omitempty"`
	HistorySequence             uint64 `json:"history_sequence,omitempty"`
	HistoryLegacyFileGeneration string `json:"history_legacy_file_generation,omitempty"`
	HistoryLegacyBytes          int64  `json:"history_legacy_bytes,omitempty"`
	HistoryOrderWitnessVersion  uint8  `json:"history_order_witness_version,omitempty"`
}

// JSONLStore implements Store using append-only JSONL files.
//
// Each session is stored as two files:
//
//	{sanitized_key}.jsonl      — one JSON-encoded message per line, append-only
//	{sanitized_key}.meta.json  — session metadata (summary, logical truncation offset)
//
// Messages are never physically deleted from the JSONL file. Instead,
// TruncateHistory records a "skip" offset in the metadata file and
// GetHistory ignores lines before that offset. This keeps all writes
// append-only, which is both fast and crash-safe.
type JSONLStore struct {
	dir       string
	locks     [numLockShards]sync.Mutex
	writeFile func(string, []byte, os.FileMode) error
	// Optional private seam for deterministic origin-reader I/O failures.
	openHistoryFile func(string) (*os.File, error)

	// metaCache holds the decoded .meta.json per sanitized session key.
	// This process is the only writer of the session directory, so a
	// write-through cache is sound — and necessary: on network filesystems
	// (EFS) every meta read is a ~10-25ms round trip, and listing sessions
	// reads every meta. With ~100 sessions that made each listing cost
	// seconds; with the cache it costs one ReadDir.
	metaMu    sync.RWMutex
	metaCache map[string]SessionMeta
	// Marker state belongs to this single writer and survives metadata-cache
	// eviction while a journal is pending. A positive latch is never cleared.
	historyMarkers   map[string]historyMarkerState
	historyWitnesses map[string]historyOrderWitnessState
	// A failed rollback can outlive cache eviction. Fence every cache read
	// until the pending journal has published either the OLD or NEW state.
	historyPending map[string]bool
}

// NewJSONLStore creates a new JSONL-backed store rooted at dir.
func NewJSONLStore(dir string) (*JSONLStore, error) {
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		return nil, fmt.Errorf("memory: create directory: %w", err)
	}
	store := &JSONLStore{
		dir: dir, metaCache: make(map[string]SessionMeta), historyPending: make(map[string]bool),
		historyMarkers: make(map[string]historyMarkerState),
	}
	if err := store.recoverPendingHistoryWrites(); err != nil {
		return nil, fmt.Errorf("memory: recover interrupted history: %w", err)
	}
	return store, nil
}

// sessionLock returns a mutex for the given session key.
// Keys are mapped to a fixed pool of shards via FNV hash, so
// memory usage is O(1) regardless of total session count.
func (s *JSONLStore) sessionLock(key string) *sync.Mutex {
	return &s.locks[lockShard(key)]
}

func lockShard(key string) uint32 {
	h := fnv.New32a()
	// Listings address a session by filename; all names for that physical
	// file must share the writer's shard, including the unsanitized key.
	h.Write([]byte(sanitizeKey(key)))
	return h.Sum32() % numLockShards
}

func (s *JSONLStore) jsonlPath(key string) string {
	return filepath.Join(s.dir, sanitizeKey(key)+".jsonl")
}

func (s *JSONLStore) metaPath(key string) string {
	return filepath.Join(s.dir, sanitizeKey(key)+".meta.json")
}

// sanitizeKey converts a session key to a safe filename component.
// Mirrors pkg/session.sanitizeFilename so that migration paths match.
// Replaces ':' with '_' (session key separator) and '/' and '\' with '_'
// so composite IDs (e.g. Telegram forum "chatID/threadID", Slack "channel/thread_ts")
// do not create subdirectories or break on Windows.
func sanitizeKey(key string) string {
	s := strings.ReplaceAll(key, ":", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

// copyMeta returns a value whose mutable fields (Scope, Aliases) do not
// share backing storage with the original, so cached metas can't be
// corrupted by caller mutations (and vice versa).
func copyMeta(m SessionMeta) SessionMeta {
	m.Scope = cloneRawJSON(m.Scope)
	if m.HistoryBytes != nil {
		size := *m.HistoryBytes
		m.HistoryBytes = &size
	}
	if m.Aliases != nil {
		m.Aliases = append([]string(nil), m.Aliases...)
	}
	return m
}

func (s *JSONLStore) cachedMeta(sanitized string) (SessionMeta, bool) {
	s.metaMu.RLock()
	meta, ok := s.metaCache[sanitized]
	pending := s.historyPending[sanitized]
	s.metaMu.RUnlock()
	if !ok || pending {
		return SessionMeta{}, false
	}
	return copyMeta(meta), true
}

func (s *JSONLStore) storeCachedMeta(sanitized string, meta SessionMeta) {
	s.metaMu.Lock()
	meta.HistoryRecoveryRequired = meta.HistoryRecoveryRequired || s.historyMarkers[sanitized].present
	s.metaCache[sanitized] = copyMeta(meta)
	s.metaMu.Unlock()
}

// readMeta loads the metadata file for a session.
// Returns a zero-value sessionMeta if the file does not exist.
func (s *JSONLStore) readMeta(key string) (SessionMeta, error) {
	sanitized := sanitizeKey(key)
	s.metaMu.RLock()
	pending := s.historyPending[sanitized]
	s.metaMu.RUnlock()
	if pending {
		if err := s.recoverHistoryWrite(key); err != nil {
			return SessionMeta{}, err
		}
	}
	// Resolve our own OLD/NEW journal first. Only this process writes the
	// directory, so warm metadata and its write-through latch need no I/O.
	marked, err := s.historyRecoveryMarkerState(key)
	if err != nil {
		return SessionMeta{}, err
	}
	if meta, ok := s.cachedMeta(sanitized); ok {
		if operationErr := s.applyHistoryRecoveryMarker(key, &meta, marked); operationErr != nil {
			return SessionMeta{}, operationErr
		}
		if operationErr := s.applyHistoryOrderWitness(key, &meta); operationErr != nil {
			return SessionMeta{}, operationErr
		}
		return meta, nil
	}
	data, err := os.ReadFile(s.metaPath(key))
	if os.IsNotExist(err) {
		meta := SessionMeta{Key: key}
		if operationErr := s.applyHistoryRecoveryMarker(key, &meta, marked); operationErr != nil {
			return SessionMeta{}, operationErr
		}
		if operationErr := s.applyHistoryOrderWitness(key, &meta); operationErr != nil {
			return SessionMeta{}, operationErr
		}
		s.storeCachedMeta(sanitized, meta)
		return meta, nil
	}
	if err != nil {
		return SessionMeta{}, fmt.Errorf("memory: read meta: %w", err)
	}
	var meta SessionMeta
	err = json.Unmarshal(data, &meta)
	if err != nil {
		return SessionMeta{}, fmt.Errorf("memory: decode meta: %w", err)
	}
	if meta.Key == "" {
		meta.Key = key
	}
	if errors.Is(historyRecordRecoveryRequired(data), ErrHistoryRecoveryRequired) {
		meta.HistoryRecoveryRequired = true
	}
	if err := s.applyHistoryRecoveryMarker(key, &meta, marked); err != nil {
		return SessionMeta{}, err
	}
	if operationErr := s.applyHistoryOrderWitness(key, &meta); operationErr != nil {
		return SessionMeta{}, operationErr
	}
	s.storeCachedMeta(sanitized, meta)
	return meta, nil
}

// writeMeta atomically writes the metadata file using the project's
// standard WriteFileAtomic (temp + fsync + rename).
func (s *JSONLStore) writeMeta(key string, meta SessionMeta) error {
	if strings.TrimSpace(meta.Key) == "" {
		meta.Key = key
	}
	marked, err := s.historyRecoveryMarkerState(key)
	if err != nil {
		return err
	}
	if operationErr := s.applyHistoryRecoveryMarker(key, &meta, marked); operationErr != nil {
		return operationErr
	}
	if meta.HistoryRecoveryRequired {
		if operationErr := s.markHistoryRecoveryRequired(key, &meta); operationErr != nil {
			return operationErr
		}
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("memory: encode meta: %w", err)
	}
	if err := s.writeSessionFile(s.metaPath(key), data, 0o644); err != nil {
		return err
	}
	s.storeCachedMeta(sanitizeKey(key), meta)
	return nil
}

func (s *JSONLStore) writeSessionFile(path string, data []byte, mode os.FileMode) error {
	if s.writeFile != nil {
		return s.writeFile(path, data, mode)
	}
	return fileutil.WriteFileAtomic(path, data, mode)
}

func cloneRawJSON(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), data...)
}

func normalizeAliases(canonicalKey string, aliases []string) []string {
	if len(aliases) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(aliases))
	seen := make(map[string]struct{}, len(aliases))
	canonicalKey = strings.TrimSpace(canonicalKey)
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" || alias == canonicalKey {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		normalized = append(normalized, alias)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func (s *JSONLStore) sessionExists(key string) bool {
	if key == "" {
		return false
	}
	if _, err := os.Stat(s.jsonlPath(key)); err == nil {
		return true
	}
	if _, err := os.Stat(s.metaPath(key)); err == nil {
		return true
	}
	return false
}

// GetSessionMeta returns the current metadata snapshot for sessionKey.
func (s *JSONLStore) GetSessionMeta(_ context.Context, sessionKey string) (SessionMeta, error) {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return SessionMeta{}, err
	}
	meta.Scope = cloneRawJSON(meta.Scope)
	if len(meta.Aliases) > 0 {
		meta.Aliases = append([]string(nil), meta.Aliases...)
	}
	return meta, nil
}

// UpsertSessionMeta stores structured session metadata while preserving
// summary/count/skip timestamps maintained by the core JSONL store.
func (s *JSONLStore) UpsertSessionMeta(
	_ context.Context,
	sessionKey string,
	scope json.RawMessage,
	aliases []string,
) error {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.sessionMetaForUpsert(sessionKey)
	if err != nil {
		return err
	}
	meta.Scope = cloneRawJSON(scope)
	meta.Aliases = normalizeAliases(sessionKey, aliases)
	now := time.Now()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now

	return s.writeMeta(sessionKey, meta)
}

func (s *JSONLStore) sessionMetaForUpsert(key string) (SessionMeta, error) {
	sanitized := sanitizeKey(key)
	s.metaMu.RLock()
	pending := s.historyPending[sanitized]
	s.metaMu.RUnlock()
	if pending {
		if err := s.recoverHistoryWrite(key); err != nil {
			return SessionMeta{}, err
		}
	}
	// Launcher deletion happens outside this writer. Its next turn updates
	// scope before appending, so do not republish a deleted source's warm
	// checkpoint or recreate its deleted recovery marker during readMeta.
	for _, path := range []string{s.metaPath(key), s.jsonlPath(key)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return SessionMeta{}, err
			}
			return s.readMeta(key)
		}
	}
	marked, err := s.hasHistoryRecoveryMarker(key)
	if err != nil {
		return SessionMeta{}, err
	}
	s.metaMu.Lock()
	delete(s.metaCache, sanitized)
	if !marked {
		delete(s.historyMarkers, sanitized)
	}
	s.metaMu.Unlock()
	meta := SessionMeta{Key: key, HistoryRecoveryRequired: marked, HistoryGeneration: newHistoryID()}
	// Retire even an empty source's identity without advancing its physical
	// epoch: metadata-only sessions must still have an empty bounded page.
	ensureHistoryGeneration(&meta, key)
	return meta, nil
}

// PromoteAliasHistory atomically promotes the first non-empty alias session
// into the canonical session when the canonical session is still empty.
//
// Main-session aliases (e.g. "agent:main:main" or its opaque form) are
// skipped during promotion.  The main session is a shared global fallback
// and promoting its history into individual sessions would attach stale
// messages to every new Web UI session (issue #2972).
func (s *JSONLStore) PromoteAliasHistory(
	_ context.Context,
	sessionKey string,
	scope json.RawMessage,
	aliases []string,
) (bool, error) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return false, nil
	}

	aliases = normalizeAliases(sessionKey, aliases)
	for _, alias := range aliases {
		if isMainSessionAlias(alias) {
			continue
		}
		unlock := s.lockSessionPair(sessionKey, alias)
		promoted, err := s.promoteAliasHistoryLocked(sessionKey, alias, scope, aliases)
		unlock()
		if err != nil || promoted {
			return promoted, err
		}
	}

	return false, nil
}

// isMainSessionAlias reports whether alias is the legacy or opaque main-session
// key.  The main session ("agent:<agent>:main") is a shared fallback and should
// not have its history promoted into individual per-channel sessions.
func isMainSessionAlias(alias string) bool {
	if alias == "" {
		return false
	}
	// Legacy form: "agent:main:main" (exactly 3 colon-separated parts)
	// Must not match "agent:sales:direct:main" etc.
	if strings.HasPrefix(alias, "agent:") && strings.HasSuffix(alias, ":main") {
		parts := strings.SplitN(alias, ":", 4)
		if len(parts) == 3 {
			return true
		}
	}
	// Opaque form: "sk_v1_" + SHA256("agent:main:main")
	if strings.HasPrefix(alias, "sk_v1_") {
		for _, agentID := range []string{"main", "Main", "MAIN"} {
			legacy := "agent:" + agentID + ":main"
			hash := sha256.Sum256([]byte(legacy))
			if "sk_v1_"+hex.EncodeToString(hash[:]) == alias {
				return true
			}
		}
	}
	return false
}

// ResolveSessionKey returns the canonical session key for a candidate key.
// It short-circuits direct canonical keys when possible, then scans metadata
// once to resolve aliases or canonical metadata keys.
//
// The scan goes through ListSessionMetas (meta cache) rather than reading
// files directly: callers resolve keys once per listed session, so a raw
// directory scan here turned session listings into O(n²) EFS reads —
// observed in prod as ~9k file reads and 5.5s per ListSessions RPC with 96
// sessions, unchanged by the per-key meta cache.
func (s *JSONLStore) ResolveSessionKey(_ context.Context, sessionKey string) (string, bool, error) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return "", false, nil
	}

	if shouldShortCircuitSessionResolve(sessionKey) && s.sessionExists(sessionKey) {
		return sessionKey, true, nil
	}

	var directMetaMatch string
	for _, meta := range s.ListSessionMetas() {
		if meta.Key == sessionKey {
			directMetaMatch = meta.Key
		}

		for _, alias := range meta.Aliases {
			if alias == sessionKey && meta.Key != sessionKey {
				return meta.Key, true, nil
			}
		}
	}

	if directMetaMatch != "" {
		return directMetaMatch, true, nil
	}

	// Only fall back to a filesystem stat when the metas didn't answer —
	// the common caller (session listing) always matches a meta above.
	if s.sessionExists(sessionKey) {
		return sessionKey, true, nil
	}

	return "", false, nil
}

func shouldShortCircuitSessionResolve(sessionKey string) bool {
	sessionKey = strings.TrimSpace(strings.ToLower(sessionKey))
	if sessionKey == "" {
		return false
	}
	return !strings.ContainsAny(sessionKey, ":/\\")
}

// lockSessionPair trava as faixas das duas chaves sempre na ordem do índice da
// faixa. A ordem pela chave não serve: dois pares diferentes podem cair nas
// mesmas duas faixas em ordem trocada, e cada um ficava com uma esperando a
// outra (deadlock do agent loop inteiro, com dois crons no mesmo segundo).
// (seucaranguejo fork)
func (s *JSONLStore) lockSessionPair(keyA, keyB string) func() {
	first, second := lockShard(keyA), lockShard(keyB)
	if first == second {
		s.locks[first].Lock()
		return s.locks[first].Unlock
	}
	if first > second {
		first, second = second, first
	}
	s.locks[first].Lock()
	s.locks[second].Lock()
	return func() {
		s.locks[second].Unlock()
		s.locks[first].Unlock()
	}
}

func (s *JSONLStore) promoteAliasHistoryLocked(
	sessionKey string,
	alias string,
	scope json.RawMessage,
	aliases []string,
) (bool, error) {
	canonicalMeta, err := s.readMeta(sessionKey)
	if err != nil {
		return false, err
	}
	if operationErr := s.prepareHistoryMutation(sessionKey, &canonicalMeta); operationErr != nil {
		return false, operationErr
	}
	if operationErr := s.prepareHistoryContentOrder(sessionKey, &canonicalMeta); operationErr != nil {
		return false, operationErr
	}
	canonicalHasContent, err := s.sessionHasVisibleContentLocked(sessionKey, canonicalMeta)
	if err != nil {
		return false, err
	}
	if canonicalHasContent {
		return false, nil
	}

	aliasMeta, err := s.readMeta(alias)
	if err != nil {
		return false, err
	}
	if operationErr := s.prepareHistoryMutation(alias, &aliasMeta); operationErr != nil {
		return false, operationErr
	}
	aliasHistory, err := s.readHistoryForContentMutation(alias, &aliasMeta)
	if err != nil {
		return false, err
	}
	aliasSummary := strings.TrimSpace(aliasMeta.Summary)
	if len(aliasHistory) == 0 && aliasSummary == "" {
		return false, nil
	}
	if canonicalMeta.HistoryRecoveryRequired || aliasMeta.HistoryRecoveryRequired {
		if markerErr := s.markHistoryRecoveryRequired(sessionKey, &canonicalMeta); markerErr != nil {
			return false, markerErr
		}
		// The original marked source retains its evidence. Copy only content;
		// a foreign stream cannot become an order proof in this destination.
		for index, message := range aliasHistory {
			aliasHistory[index] = messageWithHistoryOrder(message, HistoryOrderProof{})
		}
	} else {
		// Healthy promotion allocates fresh canonical proofs while retaining
		// the canonical highwater, before changing file/count metadata.
		for index, message := range aliasHistory {
			ordered, orderErr := s.allocateHistoryOrder(sessionKey, message, &canonicalMeta)
			if orderErr != nil {
				return false, orderErr
			}
			aliasHistory[index] = ordered
		}
	}

	now := time.Now()
	if canonicalMeta.CreatedAt.IsZero() {
		canonicalMeta.CreatedAt = now
	}
	canonicalMeta.Scope = cloneRawJSON(scope)
	canonicalMeta.Aliases = normalizeAliases(sessionKey, aliases)
	canonicalMeta.Skip = 0
	canonicalMeta.HistoryPrefix = 0
	resetHistoryGeneration(&canonicalMeta)
	canonicalMeta.Count = len(aliasHistory)
	canonicalMeta.UpdatedAt = now
	if aliasMeta.HistoryRecoveryRequired || canonicalMeta.HistoryRecoveryRequired {
		if operationErr := s.markHistoryRecoveryRequired(alias, &aliasMeta); operationErr != nil {
			return false, operationErr
		}
		if operationErr := s.markHistoryRecoveryRequired(sessionKey, &canonicalMeta); operationErr != nil {
			return false, operationErr
		}
	}
	if aliasSummary != "" {
		canonicalMeta.Summary = aliasSummary
	}
	if err := s.commitHistoryRewrite(sessionKey, aliasHistory, canonicalMeta); err != nil {
		return false, err
	}
	return true, nil
}

func (s *JSONLStore) sessionHasVisibleContentLocked(sessionKey string, meta SessionMeta) (bool, error) {
	if strings.TrimSpace(meta.Summary) != "" {
		return true, nil
	}
	history, err := s.readHistoryForContentMutation(sessionKey, &meta)
	if err != nil {
		return false, err
	}
	return len(history) > 0, nil
}

func (s *JSONLStore) readRawJSONL(sessionKey string) ([]byte, bool, error) {
	data, err := os.ReadFile(s.jsonlPath(sessionKey))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("memory: read jsonl: %w", err)
	}
	return data, true, nil
}

func (s *JSONLStore) restoreRawJSONL(sessionKey string, data []byte, existed bool) error {
	path := s.jsonlPath(sessionKey)
	if !existed {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("memory: remove jsonl rollback: %w", err)
		}
		return nil
	}
	if err := fileutil.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("memory: restore jsonl rollback: %w", err)
	}
	return nil
}

// readMessages reads valid JSON lines from a .jsonl file, skipping
// the first `skip` lines without unmarshaling them. This avoids the
// cost of json.Unmarshal on logically truncated messages.
// Malformed trailing lines (e.g. from a crash) are silently skipped.
func readMessages(path string, skip int) ([]providers.Message, error) {
	return readHistoryMessages(path, skip, false)
}

// Provider/legacy readers do not consume ordering evidence. Keep readable
// message content when private proof is corrupt or from a future producer;
// Receipt readers remain strict; content mutations retain the recovery fence.
func readProviderMessages(path string, skip int) ([]providers.Message, error) {
	return readHistoryMessages(path, skip, true)
}

func readHistoryMessages(path string, skip int, providerOnly bool) ([]providers.Message, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return []providers.Message{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: open jsonl: %w", err)
	}
	defer f.Close()

	return decodeHistoryMessages(context.Background(), f, skip, providerOnly)
}

func decodeHistoryMessages(
	ctx context.Context,
	file *os.File,
	skip int,
	providerOnly bool,
) ([]providers.Message, error) {
	path := file.Name()
	var msgs []providers.Message
	scanner := bufio.NewScanner(file)
	// Allow large lines for tool results (read_file, web search, etc.).
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)

	lineNum := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := bytes.TrimRight(scanner.Bytes(), "\r\n")
		if len(line) == 0 {
			continue
		}
		lineNum++
		if lineNum <= skip {
			continue
		}
		var msg providers.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			// Corrupt line — likely a partial write from a crash.
			// Log so operators know data was skipped, but don't
			// fail the entire read; this is the standard JSONL
			// recovery pattern.
			log.Printf("memory: skipping corrupt line %d in %s: %v",
				lineNum, filepath.Base(path), err)
			continue
		}
		if messageutil.IsTransientAssistantThoughtMessage(msg) {
			continue
		}
		msg.HistoryID = decodedHistoryID(line, filepath.Base(path), int64(lineNum-1))
		msg.HistoryRunID = decodedHistoryRunID(line)
		order, orderErr := decodedHistoryOrder(line)
		if orderErr != nil && !providerOnly {
			return nil, orderErr
		}
		if orderErr != nil && providerOnly {
			order = decodedStoredContentOrder(line)
		}
		msg = messageWithHistoryOrder(msg, order)
		msgs = append(msgs, msg)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scanner.Err() != nil {
		return nil, fmt.Errorf("memory: scan jsonl: %w", scanner.Err())
	}

	if msgs == nil {
		msgs = []providers.Message{}
	}
	return msgs, nil
}

// scanRetainedMessageLines returns the total number of non-empty raw JSONL
// lines plus the raw line numbers that survive readMessages filtering.
// TruncateHistory uses this to compute keepLast against retained messages
// while preserving the raw-line skip offset stored in metadata.
func scanRetainedMessageLines(path string) (int, []int, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return 0, []int{}, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("memory: open jsonl: %w", err)
	}
	defer f.Close()

	rawCount := 0
	retained := make([]int, 0)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	for scanner.Scan() {
		line := bytes.TrimRight(scanner.Bytes(), "\r\n")
		if len(line) == 0 {
			continue
		}
		rawCount++

		var msg providers.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if messageutil.IsTransientAssistantThoughtMessage(msg) {
			continue
		}
		retained = append(retained, rawCount)
	}
	if err := scanner.Err(); err != nil {
		return 0, nil, err
	}
	return rawCount, retained, nil
}

func (s *JSONLStore) AddMessage(
	ctx context.Context, sessionKey, role, content string,
) error {
	return s.addMsg(ctx, sessionKey, providers.Message{
		Role:    role,
		Content: content,
	})
}

func (s *JSONLStore) AddFullMessage(
	ctx context.Context, sessionKey string, msg providers.Message,
) error {
	return s.addMsg(ctx, sessionKey, msg)
}

// addMsg is the shared implementation for AddMessage and AddFullMessage.
func (s *JSONLStore) addMsg(ctx context.Context, sessionKey string, msg providers.Message) error {
	if messageutil.IsTransientAssistantThoughtMessage(msg) {
		return nil
	}

	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	now := time.Now()

	message := prepareHistoryMessage(msg, now, providers.HistoryRun(ctx))
	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}
	ensureHistoryGeneration(&meta, sessionKey)

	if meta.Count == 0 && meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	return s.appendHistoryMessages(sessionKey, []providers.Message{message}, meta)
}

func (s *JSONLStore) GetHistory(
	_ context.Context, sessionKey string,
) ([]providers.Message, error) {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return nil, err
	}

	// Pass meta.Skip so readMessages skips those lines without
	// unmarshaling them — avoids wasted CPU on truncated messages.
	msgs, err := readProviderMessages(s.jsonlPath(sessionKey), meta.Skip)
	if err != nil {
		return nil, err
	}

	return msgs, nil
}

func (s *JSONLStore) GetSummary(
	_ context.Context, sessionKey string,
) (string, error) {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return "", err
	}
	return meta.Summary, nil
}

func (s *JSONLStore) SetSummary(
	_ context.Context, sessionKey, summary string,
) error {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}
	now := time.Now()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.Summary = summary
	meta.UpdatedAt = now

	return s.writeMeta(sessionKey, meta)
}

func (s *JSONLStore) TruncateHistory(
	_ context.Context, sessionKey string, keepLast int,
) error {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}
	if operationErr := s.prepareHistoryMutation(sessionKey, &meta); operationErr != nil {
		return operationErr
	}
	if operationErr := s.prepareHistoryContentOrder(sessionKey, &meta); operationErr != nil {
		return operationErr
	}

	rawCount, retainedRawLines, scanErr := scanRetainedMessageLines(s.jsonlPath(sessionKey))
	if scanErr != nil {
		return scanErr
	}
	meta.Count = rawCount
	// A later append reestablishes the byte checkpoint for this retained window.
	meta.HistoryBytes = nil
	if meta.Skip > meta.Count {
		meta.Skip = meta.Count
	}

	activeStart := sort.Search(len(retainedRawLines), func(i int) bool {
		return retainedRawLines[i] > meta.Skip
	})
	activeRetainedCount := len(retainedRawLines) - activeStart

	switch {
	case keepLast <= 0 || activeRetainedCount == 0:
		meta.Skip = meta.Count
	case keepLast < activeRetainedCount:
		activeRawLines := retainedRawLines[activeStart:]
		meta.Skip = activeRawLines[activeRetainedCount-keepLast-1]
	}
	meta.UpdatedAt = time.Now()

	return s.writeMeta(sessionKey, meta)
}

func (s *JSONLStore) SetHistory(ctx context.Context, sessionKey string, history []providers.Message) error {
	filtered := messageutil.FilterInvalidHistoryMessages(history)
	lock := s.sessionLock(sessionKey)
	lock.Lock()
	defer lock.Unlock()
	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}
	if operationErr := s.prepareHistoryMutation(sessionKey, &meta); operationErr != nil {
		return operationErr
	}
	now := time.Now()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	ensureHistoryGeneration(&meta, sessionKey)
	prepared := make([]providers.Message, len(filtered))
	for index, message := range filtered {
		prepared[index] = prepareHistoryMessage(message, now, providers.HistoryRun(ctx))
	}
	if appended, err := s.appendHistorySuffix(sessionKey, prepared, meta); appended || err != nil {
		return err
	}
	return s.replaceHistory(ctx, sessionKey, filtered, meta)
}

// Compact physically rewrites the JSONL file, dropping all logically
// skipped lines. This reclaims disk space that accumulates after
// repeated TruncateHistory calls.
//
// It is safe to call at any time; if there is nothing to compact
// (skip == 0) the method returns immediately.
func (s *JSONLStore) Compact(
	_ context.Context, sessionKey string,
) error {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}
	if operationErr := s.prepareHistoryMutation(sessionKey, &meta); operationErr != nil {
		return operationErr
	}
	if meta.Skip == 0 {
		return nil
	}

	// Read only the active messages, skipping truncated lines
	// without unmarshaling them.
	active, err := s.readHistoryForContentMutation(sessionKey, &meta)
	if err != nil {
		return err
	}
	if !meta.HistoryRecoveryRequired {
		committed, _, orderErr := s.readCommittedHistoryOrderOrigins(sessionKey, meta, meta.Skip, false)
		if orderErr != nil {
			return orderErr
		}
		if len(active) != len(committed) {
			return ErrHistoryOrderUnavailable
		}
		for index := range active {
			if !reflect.DeepEqual(active[index], committed[index]) {
				return ErrHistoryOrderUnavailable
			}
		}
	}

	retainedPrefix, err := unchangedPhysicalPrefix(s.jsonlPath(sessionKey), meta.Skip, len(active))
	if err != nil {
		return err
	}
	ensureHistoryGeneration(&meta, sessionKey)
	meta.HistoryPrefix += int64(meta.Skip)
	if meta.Count-meta.Skip != len(active) {
		// Filtering gaps moves later messages. Retire those old positions
		// through lineage, while persisting the retained occurrence IDs.
		meta.HistoryParentGeneration = meta.HistoryGeneration
		meta.HistoryRetainedUntil = meta.HistoryPrefix + int64(retainedPrefix)
		meta.HistoryMutationRunID = ""
		resetHistoryGeneration(&meta)
	} else {
		meta.HistoryFileGeneration = newHistoryID()
		meta.HistoryFileEpoch++
	}
	meta.Skip = 0
	meta.Count = len(active)
	meta.UpdatedAt = time.Now()

	return s.commitHistoryRewrite(sessionKey, active, meta)
}

func marshalHistoryJSONL(msgs []providers.Message, meta SessionMeta) ([]byte, error) {
	msgs = messageutil.FilterInvalidHistoryMessages(msgs)

	var buf bytes.Buffer
	for i, msg := range msgs {
		line, err := marshalHistoryMessage(msg, meta)
		if err != nil {
			return nil, fmt.Errorf("memory: marshal message %d: %w", i, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// ListSessions returns all known session keys by reading .meta.json files.
func (s *JSONLStore) ListSessions() []string {
	metas := s.ListSessionMetas()
	keys := make([]string, 0, len(metas))
	for _, meta := range metas {
		keys = append(keys, meta.Key)
	}
	return keys
}

// ListSessionMetas returns the persisted metadata of every session without
// touching the message files. Meta files already carry the message count and
// timestamps; callers listing sessions must NOT recover those via GetHistory
// — that reads and parses every session's full JSONL, which on EFS with
// ~100 sessions costs seconds per listing.
//
// The directory listing stays authoritative for WHICH sessions exist, but
// each meta's content is served from the write-through cache when present —
// on EFS the per-file reads, not the ReadDir, are what made every listing
// cost seconds.
func (s *JSONLStore) ListSessionMetas() []SessionMeta {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var metas []SessionMeta
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		sanitized := strings.TrimSuffix(entry.Name(), ".meta.json")
		lock := s.sessionLock(sanitized)
		lock.Lock()
		meta, readErr := s.readMeta(sanitized)
		lock.Unlock()
		if readErr == nil && meta.Key != "" {
			metas = append(metas, meta)
		}
	}
	return metas
}

func (s *JSONLStore) Close() error {
	return nil
}

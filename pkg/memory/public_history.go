package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

const (
	PublicHistoryVersion        = 1
	PublicHistoryMessageBytes   = 1 << 20
	PublicHistoryOperationBytes = 2 << 20
	PublicHistoryImportMessages = 50
)

var ErrPublicHistoryCoverage = errors.New("public history coverage unavailable")

// PublicHistoryEntry is confirmed display content. It never enters GetHistory
// or provider requests. AnchorID binds a supplement to an exact occurrence,
// including that occurrence's order proof, rather than a mutable array index.
type PublicHistoryEntry struct {
	ID                   string            `json:"id"`
	RunID                string            `json:"runId,omitempty"`
	Message              providers.Message `json:"message"`
	Order                HistoryOrderProof `json:"order"`
	LegacyBeforeStreamID string            `json:"legacyBeforeStreamId,omitempty"`
	AnchorID             string            `json:"anchorId,omitempty"`
	AnchorOrder          HistoryOrderProof `json:"anchorOrder,omitempty"`
	ImportID             string            `json:"importId,omitempty"`
	Partial              bool              `json:"partial,omitempty"`
}

type PublicHistory struct {
	Version    int                  `json:"version"`
	Generation string               `json:"generation"`
	Revision   uint64               `json:"revision"`
	Complete   bool                 `json:"complete"`
	Entries    []PublicHistoryEntry `json:"entries"`
}

// PublicHistoryImport requires exact source coverage. A retry binds ImportID to
// the same bytes; conflicting retries and obsolete views never change content.
type PublicHistoryImport struct {
	Key        string               `json:"-"`
	Generation string               `json:"generation"`
	Revision   uint64               `json:"revision"`
	BeforeID   string               `json:"beforeId,omitempty"`
	AfterID    string               `json:"afterId,omitempty"`
	Entries    []PublicHistoryEntry `json:"entries"`
}

func (s *JSONLStore) publicHistoryPath(key string) string { return s.jsonlPath(key) + ".public" }

func publicHistoryMessage(message providers.Message) (providers.Message, bool) {
	if message.Role != "user" && message.Role != "assistant" && message.Role != "tool" {
		return providers.Message{}, false
	}
	raw := strings.TrimSpace(message.Content)
	if !strings.HasPrefix(raw, "[SENDER") && (strings.HasPrefix(raw, "[System]") || strings.HasPrefix(raw, "[SYSTEM]") || strings.HasPrefix(raw, "[System: ")) {
		return providers.Message{}, false
	}
	return providers.Message{
		Role: message.Role, Content: message.Content, ModelName: message.ModelName,
		CreatedAt: message.CreatedAt, Media: message.Media, Attachments: message.Attachments,
		ToolCalls: message.ToolCalls, ToolCallID: message.ToolCallID, Delivery: message.Delivery,
	}, true
}

func publicHistoryEntries(messages []providers.Message, meta SessionMeta) []PublicHistoryEntry {
	entries := make([]PublicHistoryEntry, 0, len(messages))
	for _, message := range messages {
		public, ok := publicHistoryMessage(message)
		if !ok {
			continue
		}
		entry := PublicHistoryEntry{ID: message.HistoryID, RunID: message.HistoryRunID, Message: public, Order: messageHistoryOrder(message)}
		if entry.Order == (HistoryOrderProof{}) && meta.HistoryOrderVersion == HistoryOrderVersion {
			entry.LegacyBeforeStreamID = meta.HistoryStreamID
		}
		entries = append(entries, entry)
	}
	return entries
}

func (s *JSONLStore) readPublicEntries(key string, meta SessionMeta) ([]PublicHistoryEntry, error) {
	if meta.PublicHistoryVersion != PublicHistoryVersion {
		return nil, ErrPublicHistoryCoverage
	}
	data, err := os.ReadFile(s.publicHistoryPath(key))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != meta.PublicHistoryBytes {
		return nil, ErrPublicHistoryCoverage
	}
	var entries []PublicHistoryEntry
	digest := emptyHistoryDigest
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var entry PublicHistoryEntry
		if json.Unmarshal([]byte(line), &entry) != nil || uuid.Validate(entry.ID) != nil {
			return nil, ErrPublicHistoryCoverage
		}
		digest = appendPublicDigest(digest, []byte(line))
		entries = append(entries, entry)
	}
	if int64(len(entries)) != meta.PublicHistoryCount || digest != meta.PublicHistoryDigest {
		return nil, ErrPublicHistoryCoverage
	}
	return entries, nil
}

func publicHistoryResult(meta SessionMeta, entries []PublicHistoryEntry) PublicHistory {
	return PublicHistory{Version: PublicHistoryVersion, Generation: meta.PublicHistoryGeneration, Revision: meta.PublicHistoryRevision, Complete: meta.PublicHistoryComplete, Entries: entries}
}

func (s *JSONLStore) ReadPublicHistory(ctx context.Context, key string) (PublicHistory, error) {
	canonical, err := s.canonicalHistoryOrderKey(key)
	if err != nil {
		return PublicHistory{}, err
	}
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return PublicHistory{}, err
	}
	meta, err := s.readMeta(canonical)
	if err != nil {
		return PublicHistory{}, err
	}
	if err := validateCanonicalHistoryKey(key, canonical, meta); err != nil {
		return PublicHistory{}, err
	}
	if err := s.probeHistoryRecoveryMarker(canonical, &meta); err != nil {
		return PublicHistory{}, err
	}
	if meta.HistoryRecoveryRequired {
		return PublicHistory{}, ErrHistoryRecoveryRequired
	}
	if err := s.probeHistoryOrderWitness(canonical, &meta); err != nil {
		return PublicHistory{}, err
	}
	entries, err := s.readPublicEntries(canonical, meta)
	if err != nil {
		return PublicHistory{}, err
	}
	return publicHistoryResult(meta, entries), nil
}

func encodePublicEntries(entries []PublicHistoryEntry) ([]byte, error) {
	var data []byte
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	return data, nil
}

func validPublicEntry(entry PublicHistoryEntry) bool {
	public, ok := publicHistoryMessage(entry.Message)
	if !ok || uuid.Validate(entry.ID) != nil || entry.ImportID == "" || len(entry.ImportID) > 256 {
		return false
	}
	if entry.Order != (HistoryOrderProof{}) && !entry.Order.Valid() {
		return false
	}
	// Only the allowlisted display shape is persisted, even from the authenticated
	// importer. Audit reasoning, system parts and provider credentials stay out.
	original, _ := json.Marshal(entry.Message)
	clean, _ := json.Marshal(public)
	return string(original) == string(clean)
}

func (s *JSONLStore) ImportPublicHistory(ctx context.Context, request PublicHistoryImport) (PublicHistory, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return PublicHistory{}, err
	}
	if len(request.Entries) == 0 || len(request.Entries) > PublicHistoryImportMessages || len(payload) > PublicHistoryOperationBytes {
		return PublicHistory{}, ErrHistoryCapacity
	}
	for _, entry := range request.Entries {
		data, _ := json.Marshal(entry)
		if len(data) > PublicHistoryMessageBytes {
			return PublicHistory{}, ErrHistoryCapacity
		}
		if !validPublicEntry(entry) {
			return PublicHistory{}, ErrPublicHistoryCoverage
		}
	}
	canonical, err := s.canonicalHistoryOrderKey(request.Key)
	if err != nil {
		return PublicHistory{}, err
	}
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return PublicHistory{}, err
	}
	meta, err := s.readMeta(canonical)
	if err != nil {
		return PublicHistory{}, err
	}
	if err := validateCanonicalHistoryKey(request.Key, canonical, meta); err != nil {
		return PublicHistory{}, err
	}
	if err := s.probeHistoryRecoveryMarker(canonical, &meta); err != nil {
		return PublicHistory{}, err
	}
	if meta.HistoryRecoveryRequired {
		return PublicHistory{}, ErrHistoryRecoveryRequired
	}
	entries, err := s.readPublicEntries(canonical, meta)
	if err != nil {
		return PublicHistory{}, err
	}
	next, changed, err := mergePublicImport(entries, request, meta)
	if err != nil {
		return PublicHistory{}, err
	}
	if !changed {
		return publicHistoryResult(meta, entries), nil
	}
	next, err = s.orderPublicImports(canonical, entries, next, &meta)
	if err != nil {
		return PublicHistory{}, err
	}
	// Insertion into an already published range is structural. A cursor from
	// before that import must not discover new content inside its frozen range.
	if request.BeforeID != "" || len(entries) == 0 || request.AfterID != entries[len(entries)-1].ID {
		meta.PublicHistoryGeneration = newHistoryID()
	}
	change, err := s.replacePublicEntries(canonical, next, &meta)
	if err != nil {
		return PublicHistory{}, err
	}
	if err := s.commitPublicChange(canonical, meta, change); err != nil {
		return PublicHistory{}, err
	}
	return publicHistoryResult(meta, next), nil
}

func publicImportConflict() error {
	return fmt.Errorf("%w: import identity or anchor changed", ErrPublicHistoryCoverage)
}

func appendPublicDigest(previous string, line []byte) string {
	digest := sha256.Sum256(append([]byte(previous+":"), line...))
	return hex.EncodeToString(digest[:])
}

func publicEntriesDigest(previous string, entries []PublicHistoryEntry) (string, error) {
	digest := previous
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			return "", err
		}
		digest = appendPublicDigest(digest, line)
	}
	return digest, nil
}

func (s *JSONLStore) orderPublicImports(key string, previous, next []PublicHistoryEntry, meta *SessionMeta) ([]PublicHistoryEntry, error) {
	existing := make(map[string]bool, len(previous))
	for _, entry := range previous {
		existing[entry.ID] = true
	}
	ordered := append([]PublicHistoryEntry(nil), next...)
	for index, entry := range ordered {
		if existing[entry.ID] || entry.Partial || entry.AnchorID != "" {
			continue
		}
		if entry.Order != (HistoryOrderProof{}) {
			return nil, ErrPublicHistoryCoverage
		}
		input := entry.Message
		input.HistoryID = entry.ID
		input.HistoryRunID = entry.RunID
		message, err := s.allocateHistoryOrder(key, input, meta)
		if err != nil {
			return nil, err
		}
		entry.Order = messageHistoryOrder(message)
		ordered[index] = entry
	}
	return ordered, nil
}

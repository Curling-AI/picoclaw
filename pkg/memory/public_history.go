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
	if !strings.HasPrefix(raw, "[SENDER") &&
		(strings.HasPrefix(raw, "[System]") || strings.HasPrefix(raw, "[SYSTEM]") || strings.HasPrefix(raw, "[System: ")) {
		return providers.Message{}, false
	}
	return providers.Message{
		Role: message.Role, Content: message.Content, ModelName: message.ModelName,
		CreatedAt: message.CreatedAt, Media: message.Media, Attachments: message.Attachments,
		ToolCalls: publicToolCalls(message.ToolCalls), ToolCallID: message.ToolCallID, Delivery: message.Delivery,
	}, true
}

func publicHistoryEntries(messages []providers.Message, meta SessionMeta) []PublicHistoryEntry {
	entries := make([]PublicHistoryEntry, 0, len(messages))
	for _, message := range messages {
		input := message
		if message.HistoryPublicMessage != nil {
			input = *message.HistoryPublicMessage
		}
		public, ok := publicHistoryMessage(input)
		if !ok {
			continue
		}
		entry := PublicHistoryEntry{
			ID:      message.HistoryID,
			RunID:   message.HistoryRunID,
			Message: public,
			Order:   messageHistoryOrder(message),
		}
		if entry.Order == (HistoryOrderProof{}) && meta.HistoryOrderVersion == HistoryOrderVersion {
			entry.LegacyBeforeStreamID = meta.HistoryStreamID
		}
		entries = append(entries, entry)
	}
	return entries
}

func (s *JSONLStore) readPublicEntries(key string, meta SessionMeta) ([]PublicHistoryEntry, error) {
	if meta.PublicHistoryVersion != PublicHistoryVersion || meta.PublicHistoryPreparation != nil {
		return nil, ErrPublicHistoryCoverage
	}
	data, dataErr := os.ReadFile(s.publicHistoryPath(key))
	if dataErr != nil {
		return nil, dataErr
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
	return PublicHistory{
		Version:    PublicHistoryVersion,
		Generation: meta.PublicHistoryGeneration,
		Revision:   meta.PublicHistoryRevision,
		Complete:   meta.PublicHistoryComplete,
		Entries:    entries,
	}
}

func (s *JSONLStore) ReadPublicHistory(ctx context.Context, key string) (PublicHistory, error) {
	canonical, canonicalErr := s.canonicalHistoryOrderKey(key)
	if canonicalErr != nil {
		return PublicHistory{}, canonicalErr
	}
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if operationErr := ctx.Err(); operationErr != nil {
		return PublicHistory{}, operationErr
	}
	if _, operationErr1 := os.Stat(s.metaPath(canonical)); operationErr1 != nil {
		return PublicHistory{}, operationErr1
	}
	meta, canonicalErr := s.readMeta(canonical)
	if canonicalErr != nil {
		return PublicHistory{}, canonicalErr
	}
	if operationErr2 := validateCanonicalHistoryKey(key, canonical, meta); operationErr2 != nil {
		return PublicHistory{}, operationErr2
	}
	if operationErr3 := s.probeHistoryRecoveryMarker(canonical, &meta); operationErr3 != nil {
		return PublicHistory{}, operationErr3
	}
	if meta.HistoryRecoveryRequired {
		return PublicHistory{}, ErrHistoryRecoveryRequired
	}
	if operationErr4 := s.probeHistoryOrderWitness(canonical, &meta); operationErr4 != nil {
		return PublicHistory{}, operationErr4
	}
	entries, canonicalErr := s.readPublicEntries(canonical, meta)
	if canonicalErr != nil {
		return PublicHistory{}, canonicalErr
	}
	return publicHistoryResult(meta, entries), nil
}

func encodePublicEntries(entries []PublicHistoryEntry) ([]byte, error) {
	var data []byte
	for _, entry := range entries {
		line, lineErr := json.Marshal(entry)
		if lineErr != nil {
			return nil, lineErr
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	return data, nil
}

func validPublicEntry(entry PublicHistoryEntry) bool {
	public, ok := publicHistoryMessage(entry.Message)
	if !ok || uuid.Validate(entry.ID) != nil || (entry.RunID != "" && uuid.Validate(entry.RunID) != nil) ||
		(entry.AnchorID != "" && uuid.Validate(entry.AnchorID) != nil) ||
		entry.ImportID == "" ||
		len(entry.ImportID) > 256 {
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
	payload, payloadErr := json.Marshal(request)
	if payloadErr != nil {
		return PublicHistory{}, payloadErr
	}
	if len(request.Entries) == 0 || len(request.Entries) > PublicHistoryImportMessages ||
		len(payload) > PublicHistoryOperationBytes {
		return PublicHistory{}, ErrHistoryCapacity
	}
	for _, entry := range request.Entries {
		data, _ := json.Marshal(entry.Message)
		if len(data) > PublicHistoryMessageBytes {
			return PublicHistory{}, ErrHistoryCapacity
		}
		if !validPublicEntry(entry) {
			return PublicHistory{}, ErrPublicHistoryCoverage
		}
	}
	canonical, payloadErr := s.canonicalHistoryOrderKey(request.Key)
	if payloadErr != nil {
		return PublicHistory{}, payloadErr
	}
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if operationErr5 := ctx.Err(); operationErr5 != nil {
		return PublicHistory{}, operationErr5
	}
	if _, operationErr6 := os.Stat(s.metaPath(canonical)); operationErr6 != nil {
		return PublicHistory{}, operationErr6
	}
	meta, payloadErr := s.readMeta(canonical)
	if payloadErr != nil {
		return PublicHistory{}, payloadErr
	}
	if operationErr7 := validateCanonicalHistoryKey(request.Key, canonical, meta); operationErr7 != nil {
		return PublicHistory{}, operationErr7
	}
	if operationErr8 := s.probeHistoryRecoveryMarker(canonical, &meta); operationErr8 != nil {
		return PublicHistory{}, operationErr8
	}
	if meta.HistoryRecoveryRequired {
		return PublicHistory{}, ErrHistoryRecoveryRequired
	}
	if operationErr9 := s.probeHistoryRecoveryMarker(canonical, &meta); operationErr9 != nil {
		return PublicHistory{}, operationErr9
	}
	if operationErr10 := s.probeHistoryOrderWitness(canonical, &meta); operationErr10 != nil {
		return PublicHistory{}, operationErr10
	}
	if meta.HistoryRecoveryRequired {
		return PublicHistory{}, ErrHistoryRecoveryRequired
	}
	entries, payloadErr := s.readPublicEntries(canonical, meta)
	if payloadErr != nil {
		return PublicHistory{}, payloadErr
	}
	next, changed, payloadErr := mergePublicImport(entries, request, meta)
	if payloadErr != nil {
		return PublicHistory{}, payloadErr
	}
	if !changed {
		return publicHistoryResult(meta, entries), nil
	}
	next, payloadErr = s.orderPublicImports(canonical, entries, next, &meta)
	if payloadErr != nil {
		return PublicHistory{}, payloadErr
	}
	// Insertion into an already published range is structural. A cursor from
	// before that import must not discover new content inside its frozen range.
	if request.BeforeID != "" || len(entries) == 0 || request.AfterID != entries[len(entries)-1].ID {
		meta.PublicHistoryGeneration = newHistoryID()
	}
	change, payloadErr := s.replacePublicEntries(canonical, next, &meta)
	if payloadErr != nil {
		return PublicHistory{}, payloadErr
	}
	if operationErr11 := s.commitPublicChange(canonical, meta, change); operationErr11 != nil {
		return PublicHistory{}, operationErr11
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
		line, lineErr12 := json.Marshal(entry)
		if lineErr12 != nil {
			return "", lineErr12
		}
		digest = appendPublicDigest(digest, line)
	}
	return digest, nil
}

func (s *JSONLStore) orderPublicImports(
	key string,
	previous, next []PublicHistoryEntry,
	meta *SessionMeta,
) ([]PublicHistoryEntry, error) {
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
		message, messageErr := s.allocateHistoryOrder(key, input, meta)
		if messageErr != nil {
			return nil, messageErr
		}
		entry.Order = messageHistoryOrder(message)
		ordered[index] = entry
	}
	return ordered, nil
}

func publicToolCalls(calls []providers.ToolCall) []providers.ToolCall {
	if calls == nil {
		return nil
	}
	result := make([]providers.ToolCall, len(calls))
	for index, call := range calls {
		safe := providers.ToolCall{ID: call.ID, Type: call.Type, Name: call.Name, Arguments: call.Arguments}
		if call.Function != nil {
			safe.Function = &providers.FunctionCall{Name: call.Function.Name, Arguments: call.Function.Arguments}
		}
		result[index] = safe
	}
	return result
}

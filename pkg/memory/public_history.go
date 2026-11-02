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
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/messageutil"
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
	SourceDigest         string            `json:"sourceDigest,omitempty"`
	Ref                  *ContentRef       `json:"ref,omitempty"`
	AnchorRef            *ContentRef       `json:"anchorRef,omitempty"`
	BoundaryID           string            `json:"boundaryId,omitempty"`
	ID                   string            `json:"id"`
	RunID                string            `json:"runId,omitempty"`
	CallID               string            `json:"callId,omitempty"`
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
	return publicHistoryEntriesAt(messages, meta, meta.Count-len(messages))
}

func publicHistoryEntriesAt(messages []providers.Message, meta SessionMeta, start int) []PublicHistoryEntry {
	entries := make([]PublicHistoryEntry, 0, len(messages))
	for index, message := range messages {
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
		if providers.ValidHistoryCallID(message.HistoryCallID) {
			entry.CallID = message.HistoryCallID
		}
		if entry.Order.Valid() {
			entry.SourceDigest = publicSourceDigest(message)
		}
		entry.Ref = publicContentRef(meta, start+index+1)
		if entry.Order == (HistoryOrderProof{}) && meta.HistoryOrderVersion == HistoryOrderVersion {
			entry.LegacyBeforeStreamID = meta.HistoryStreamID
		}
		entries = append(entries, entry)
	}
	return entries
}

func publicSourceDigest(message providers.Message) string {
	createdAt := ""
	if message.CreatedAt != nil {
		createdAt = message.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	return messageutil.SourceContentDigest(createdAt, message.Content)
}

func (s *JSONLStore) readPublicEntries(key string, meta SessionMeta) ([]PublicHistoryEntry, error) {
	if meta.PublicHistoryVersion != PublicHistoryVersion || meta.PublicHistoryPreparation != nil {
		return nil, ErrPublicHistoryCoverage
	}
	data, dataErr := os.ReadFile(s.publicHistoryPath(key))
	if dataErr != nil {
		return nil, dataErr
	}
	return decodePublicEntries(data, meta)
}

func decodePublicEntries(data []byte, meta SessionMeta) ([]PublicHistoryEntry, error) {
	if int64(len(data)) != meta.PublicHistoryBytes || meta.PublicHistoryCount < 0 || meta.PublicHistoryRevision == 0 ||
		uuid.Validate(meta.PublicHistoryGeneration) != nil {
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
	if meta.publicOrderUnavailable || meta.HistoryOrderVersion != HistoryOrderVersion || meta.HistoryRecoveryRequired {
		entries = append([]PublicHistoryEntry(nil), entries...)
		for i := range entries {
			entries[i].Order = HistoryOrderProof{}
			entries[i].SourceDigest = ""
			entries[i].AnchorOrder = HistoryOrderProof{}
			entries[i].LegacyBeforeStreamID = ""
		}
	}
	if meta.publicRefUnavailable || meta.ContentIdentity == nil || meta.ContentIdentity.Unavailable {
		entries = append([]PublicHistoryEntry(nil), entries...)
		for i := range entries {
			entries[i].Ref = nil
			entries[i].AnchorRef = nil
		}
	}
	return PublicHistory{
		Version:    PublicHistoryVersion,
		Generation: meta.PublicHistoryGeneration,
		Revision:   meta.PublicHistoryRevision,
		Complete:   meta.PublicHistoryComplete,
		Entries:    entries,
	}
}

func (s *JSONLStore) ReadPublicHistory(ctx context.Context, key string) (PublicHistory, error) {
	canonical := key
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return PublicHistory{}, err
	}
	if operationErr := ctx.Err(); operationErr != nil {
		return PublicHistory{}, operationErr
	}
	if _, operationErr1 := os.Stat(s.metaPath(canonical)); operationErr1 != nil {
		return PublicHistory{}, operationErr1
	}
	meta, _, canonicalErr := s.readContentMeta(canonical)
	if canonicalErr != nil {
		return PublicHistory{}, canonicalErr
	}
	if err := validateCanonicalHistoryKey(key, canonical, meta); err != nil {
		return PublicHistory{}, err
	}
	if err := checkPublicReadFiles(ctx, os.DirFS(s.dir), sanitizeKey(canonical), &meta); err != nil {
		return PublicHistory{}, err
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
	if entry.SourceDigest != "" {
		return false
	}
	public, ok := publicHistoryMessage(entry.Message)
	if !ok || entry.BoundaryID != "" || entry.CallID != "" || (entry.Ref != nil && !entry.Ref.Valid()) ||
		(entry.AnchorRef != nil && !entry.AnchorRef.Valid()) || uuid.Validate(entry.ID) != nil ||
		(entry.RunID != "" && uuid.Validate(entry.RunID) != nil) ||
		(entry.AnchorID != "" && uuid.Validate(entry.AnchorID) != nil) ||
		entry.ImportID == "" ||
		len(entry.ImportID) > 256 {
		return false
	}
	if entry.Order != (HistoryOrderProof{}) && !entry.Order.Valid() {
		return false
	}
	if ((entry.Partial || entry.AnchorID != "") && entry.Order != (HistoryOrderProof{})) ||
		(entry.AnchorID == "" && (entry.AnchorRef != nil || entry.AnchorOrder != (HistoryOrderProof{}))) {
		return false
	}
	references := append([]string(nil), entry.Message.Media...)
	for _, attachment := range entry.Message.Attachments {
		references = append(references, attachment.Ref, attachment.URL)
	}
	for _, reference := range references {
		lower := strings.ToLower(reference)
		if strings.HasPrefix(lower, "data:") || strings.HasPrefix(lower, "blob:") {
			return false
		}
	}
	// Only the allowlisted display shape is persisted, even from the authenticated
	// importer. Audit reasoning, system parts and provider credentials stay out.
	original, _ := json.Marshal(entry.Message)
	clean, _ := json.Marshal(public)
	return string(original) == string(clean)
}

type publicImportOrigin uint8

const (
	publicImportExternal publicImportOrigin = iota
	publicImportNativeMedia
)

func (s *JSONLStore) ImportPublicHistory(ctx context.Context, request PublicHistoryImport) (PublicHistory, error) {
	return s.importPublicHistory(ctx, request, publicImportExternal)
}

func (s *JSONLStore) importPublicHistory(
	ctx context.Context,
	request PublicHistoryImport,
	origin publicImportOrigin,
) (PublicHistory, error) {
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
	canonical := request.Key
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if operationErr5 := ctx.Err(); operationErr5 != nil {
		return PublicHistory{}, operationErr5
	}
	if _, operationErr6 := os.Stat(s.metaPath(canonical)); operationErr6 != nil {
		return PublicHistory{}, operationErr6
	}
	if err := s.contentMutationWriterPreflight(ctx, canonical); err != nil {
		return PublicHistory{}, err
	}
	meta, _, payloadErr := s.readContentMeta(canonical)
	if payloadErr != nil {
		return PublicHistory{}, payloadErr
	}
	if operationErr7 := validateCanonicalHistoryKey(request.Key, canonical, meta); operationErr7 != nil {
		return PublicHistory{}, operationErr7
	}
	if operationErr8 := s.probeHistoryRecoveryMarker(canonical, &meta); operationErr8 != nil {
		return PublicHistory{}, operationErr8
	}
	if operationErr := s.prepareContentMutationOrderFence(canonical, &meta); operationErr != nil {
		return PublicHistory{}, operationErr
	}
	if operationErr := checkPublicReadFiles(ctx, os.DirFS(s.dir), sanitizeKey(canonical), &meta); operationErr != nil {
		return PublicHistory{}, operationErr
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
	if origin == publicImportExternal && meta.ContentIdentity != nil &&
		meta.ContentIdentity.ArchivePolicy == nativePublicArchivePolicy {
		meta.PublicHistoryNonLocalImports = true
	}
	next, payloadErr = preparePublicImports(entries, next, meta)
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
	if operationErr11 := s.commitPublicChange(ctx, canonical, meta, change); operationErr11 != nil {
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

func preparePublicImports(previous, next []PublicHistoryEntry, meta SessionMeta) ([]PublicHistoryEntry, error) {
	existing := make(map[string]bool, len(previous))
	for _, entry := range previous {
		existing[entry.ID] = true
	}
	prepared := append([]PublicHistoryEntry(nil), next...)
	for index, entry := range prepared {
		if existing[entry.ID] {
			continue
		}
		// Imported historical bytes cannot mint a new final-response receipt.
		if entry.Order != (HistoryOrderProof{}) {
			return nil, ErrPublicHistoryCoverage
		}
		if entry.Ref != nil &&
			(meta.publicRefUnavailable || meta.ContentIdentity == nil || entry.Ref.DomainID != meta.ContentIdentity.DomainID) {
			return nil, ErrPublicHistoryCoverage
		}
		if entry.Ref == nil && entry.AnchorID == "" && !entry.Partial && meta.ContentIdentity != nil &&
			!meta.ContentIdentity.Unavailable && !meta.publicRefUnavailable {
			entry.Ref = &ContentRef{
				DomainID:     meta.ContentIdentity.DomainID,
				OccurrenceID: newHistoryID(),
				RevisionID:   newHistoryID(),
			}
		}
		prepared[index] = entry
	}
	return prepared, nil
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

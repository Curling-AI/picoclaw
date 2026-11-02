package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func samePublicRef(left, right *ContentRef) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func publicContentRef(meta SessionMeta, line int) *ContentRef {
	state := meta.ContentIdentity
	if state == nil || state.Unavailable {
		return nil
	}
	rows := state.Rows
	if state.storage != nil && !state.storage.RowsLoaded {
		rows = state.storage.Appended
	}
	for _, row := range rows {
		if row.Line == line && row.Ref.Valid() {
			ref := row.Ref
			return &ref
		}
	}
	return nil
}

// Initialization binds the existing display bytes to the producer's exact
// source locators; it never matches a message by its body or current position.
func (s *JSONLStore) bindPublicContentMetadata(
	key string,
	image contentImage,
	next *SessionMeta,
) (publicHistoryChange, error) {
	if image.Meta.PublicHistoryVersion != PublicHistoryVersion || image.Meta.PublicHistoryPreparation != nil ||
		next.ContentIdentity == nil || next.ContentIdentity.Unavailable {
		return publicHistoryChange{}, nil
	}
	entries, err := s.readPublicEntries(key, image.Meta)
	if err != nil {
		return publicHistoryChange{}, err
	}
	refs := make(map[string]*ContentRef)
	for _, row := range image.activeRows() {
		ref := publicContentRef(*next, row.Line)
		if ref != nil {
			id := decodedHistoryID(row.Raw, filepath.Base(s.jsonlPath(key)), int64(row.Line-1))
			refs[id] = ref
		}
	}
	changed := false
	for index, entry := range entries {
		if ref := refs[entry.ID]; entry.ImportID == "" && ref != nil && !samePublicRef(entry.Ref, ref) {
			entries[index].Ref = ref
			changed = true
		}
		if ref := refs[entry.AnchorID]; entry.AnchorID != "" && ref != nil && entry.AnchorRef == nil {
			entries[index].AnchorRef = ref
			changed = true
		}
	}
	if !changed {
		return publicHistoryChange{}, nil
	}
	return s.replacePublicEntries(key, entries, next)
}

func (s *JSONLStore) rewritePublicContent(
	key string,
	original SessionMeta,
	next *SessionMeta,
	messages []providers.Message,
	entries []PublicHistoryEntry,
) (publicHistoryChange, error) {
	if next.ContentIdentity == nil || next.ContentIdentity.Unavailable {
		next.PublicHistoryVersion = -1
		return publicHistoryChange{}, nil
	}
	image, err := s.readContentImage(context.Background(), key)
	if err != nil {
		return publicHistoryChange{}, err
	}
	frame, err := s.frameForContentImage(context.Background(), key, image)
	if err != nil {
		return publicHistoryChange{}, err
	}
	if frame.IdentityStatus != ContentIdentityKnown {
		next.PublicHistoryVersion = -1
		return publicHistoryChange{}, nil
	}
	active := make(map[string]bool, len(frame.Entries))
	for _, row := range image.activeRows() {
		active[decodedHistoryID(row.Raw, filepath.Base(s.jsonlPath(key)), int64(row.Line-1))] = true
	}
	updated := make(map[string]PublicHistoryEntry)
	for _, entry := range publicHistoryEntries(messages, *next) {
		updated[entry.ID] = entry
	}
	kept := make([]PublicHistoryEntry, 0, len(entries))
	for _, entry := range entries {
		imported := entry.ImportID != ""
		if imported && active[entry.BoundaryID] {
			if _, exists := updated[entry.BoundaryID]; !exists {
				continue
			}
		}
		if imported && active[entry.AnchorID] {
			anchor, exists := updated[entry.AnchorID]
			if !exists || !samePublicRef(anchor.Ref, entry.AnchorRef) || anchor.Order != entry.AnchorOrder {
				continue
			}
		}
		if imported || !active[entry.ID] {
			kept = append(kept, entry)
			continue
		}
		replacement, exists := updated[entry.ID]
		if !exists {
			continue
		}
		if entry.Ref == nil || replacement.Ref == nil || entry.Ref.OccurrenceID != replacement.Ref.OccurrenceID {
			return publicHistoryChange{}, ErrPublicHistoryCoverage
		}
		replacement.CallID = ""
		if samePublicRef(entry.Ref, replacement.Ref) {
			replacement.CallID = entry.CallID
		}
		kept = append(kept, replacement)
	}
	if next.HistoryGeneration != original.HistoryGeneration {
		next.PublicHistoryGeneration = newHistoryID()
	}
	return s.replacePublicEntries(key, kept, next)
}

func (s *JSONLStore) verifyPublicPrefix(ctx context.Context, key string, meta SessionMeta) (resultErr error) {
	if meta.PublicHistoryVersion == 0 || meta.Skip == 0 {
		return nil
	}
	entries, err := s.readPublicEntries(key, meta)
	if err != nil {
		return err
	}
	byID := make(map[string]PublicHistoryEntry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	file, err := os.Open(s.jsonlPath(key))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, historyReadBufferBytes), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	line := 0
	for scanner.Scan() && line < meta.Skip {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		line++
		var message providers.Message
		if json.Unmarshal(raw, &message) != nil {
			return ErrPublicHistoryCoverage
		}
		if _, visible := publicHistoryMessage(message); !visible {
			continue
		}
		message.HistoryPublicMessage = decodedPublicMessage(raw)
		projected, err := s.projectLegacyPublic(message)
		if err != nil {
			return err
		}
		public, _ := publicHistoryMessage(projected)
		entry, exists := byID[decodedHistoryID(raw, filepath.Base(file.Name()), int64(line-1))]
		if !exists || contentJSONDigest(entry.Message) != contentJSONDigest(public) ||
			(meta.ContentIdentity != nil && (entry.Ref == nil || !entry.Ref.Valid() || entry.Ref.DomainID != meta.ContentIdentity.DomainID)) {
			return ErrPublicHistoryCoverage
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if line != meta.Skip {
		return ErrPublicHistoryCoverage
	}
	return nil
}

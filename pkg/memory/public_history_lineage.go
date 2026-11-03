package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
		return s.rewritePublicDisplay(key, original, next, messages, entries)
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
		return s.rewritePublicDisplay(key, original, next, messages, entries)
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

type publicArtifactVerificationError struct{ cause error }

func (e *publicArtifactVerificationError) Error() string {
	return "public history artifact verification unavailable"
}

func (e *publicArtifactVerificationError) Unwrap() error {
	return errors.Join(ErrPublicHistoryCoverage, e.cause)
}

func (s *JSONLStore) verifyPublicPrefix(ctx context.Context, key string, meta SessionMeta) (resultErr error) {
	if meta.Skip == 0 {
		return nil
	}
	file, err := os.Open(s.jsonlPath(key))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	return s.verifyPublicPrefixReader(ctx, key, meta, file, true)
}

// sourceLineage is false only for an exact Pending-owned preimage whose
// immutable descriptor and digest were already verified. This checks display
// retention, never issues a new raw continuation or native-order receipt.
func (s *JSONLStore) verifyPublicPrefixReader(
	ctx context.Context,
	key string,
	meta SessionMeta,
	reader io.Reader,
	sourceLineage bool,
) error {
	if meta.Skip == 0 {
		return nil
	}
	if meta.PublicHistoryVersion == 0 {
		return ErrPublicHistoryCoverage
	}
	ensureHistoryGeneration(&meta, key)
	entries, err := s.readPublicEntries(key, meta)
	if err != nil {
		return err
	}
	byID := make(map[string]PublicHistoryEntry, len(entries))
	projector := s.historyPublicProjector()
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	prefix := make(map[string]bool)
	scanner := bufio.NewScanner(reader)
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
		if json.Unmarshal(raw, &message) != nil || (sourceLineage && !historyLineageMatches(raw, meta, key)) {
			return ErrPublicHistoryCoverage
		}
		if _, visible := publicHistoryMessage(message); !visible {
			continue
		}
		message.HistoryPublicMessage = decodedPublicMessage(raw)
		if message.HistoryPublicMessage != nil && projector == nil {
			return ErrPublicHistoryCoverage
		}
		projected, err := s.projectLegacyPublic(message)
		if err != nil {
			return &publicArtifactVerificationError{cause: err}
		}
		public, _ := publicHistoryMessage(projected)
		entry, exists := byID[decodedHistoryID(raw, filepath.Base(s.jsonlPath(key)), int64(line-1))]
		if !exists || contentJSONDigest(entry.Message) != contentJSONDigest(public) ||
			(sourceLineage && meta.ContentIdentity != nil && !meta.ContentIdentity.Unavailable && (entry.Ref == nil || !entry.Ref.Valid() || entry.Ref.DomainID != meta.ContentIdentity.DomainID)) {
			return ErrPublicHistoryCoverage
		}
		// Existing independent order may disprove an ID association, but its
		// absence never invents a historical Ref/Order for this body copy.
		storedOrder := decodedStoredContentOrder(raw)
		if storedOrder.Valid() && entry.Order.Valid() && storedOrder != entry.Order {
			return ErrPublicHistoryCoverage
		}
		prefix[entry.ID] = true
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if line != meta.Skip {
		return ErrPublicHistoryCoverage
	}
	if projector != nil {
		// Imports may be anchored through another imported supplement. Follow
		// exact IDs transitively so narrowing the hash work never omits them.
		children := make(map[string][]string)
		for _, entry := range entries {
			for _, parent := range []string{entry.AnchorID, entry.BoundaryID} {
				if parent != "" {
					children[parent] = append(children[parent], entry.ID)
				}
			}
		}
		queue := make([]string, 0, len(prefix))
		for id := range prefix {
			queue = append(queue, id)
		}
		for position := 0; position < len(queue); position++ {
			for _, id := range children[queue[position]] {
				if !prefix[id] {
					prefix[id] = true
					queue = append(queue, id)
				}
			}
		}
		for _, entry := range entries {
			if !prefix[entry.ID] {
				continue
			}
			if err := projector.Verify(ctx, entry.Message); err != nil {
				return &publicArtifactVerificationError{cause: err}
			}
		}
	}
	return nil
}

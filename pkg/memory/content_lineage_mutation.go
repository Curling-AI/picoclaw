package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/messageutil"
)

func contentLocatorsForData(data []byte, domain string, start int, refs []ContentRef) ([]contentLocator, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	rows := make([]contentLocator, 0, len(refs))
	line := start
	for scanner.Scan() {
		raw := bytes.TrimRight(scanner.Bytes(), "\r\n")
		if len(raw) == 0 {
			continue
		}
		line++
		var message providers.Message
		if json.Unmarshal(raw, &message) != nil || messageutil.IsTransientAssistantThoughtMessage(message) {
			continue
		}
		var ref ContentRef
		if refs == nil {
			ref = ContentRef{DomainID: domain, OccurrenceID: uuid.NewString(), RevisionID: uuid.NewString()}
		} else {
			if len(rows) >= len(refs) {
				return nil, ErrContentStateChanged
			}
			ref = refs[len(rows)]
		}
		rows = append(rows, contentLocator{Ref: ref, Line: line, Digest: contentDigest(raw)})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if refs != nil && len(rows) != len(refs) {
		return nil, ErrContentStateChanged
	}
	return rows, nil
}

func (s *JSONLStore) prepareContentAppend(
	key string,
	original SessionMeta,
	next *SessionMeta,
	data []byte,
	offset int64,
) error {
	if original.ContentIdentity == nil {
		return nil
	}
	next.ContentIdentity = cloneContentState(original.ContentIdentity)
	state := next.ContentIdentity
	if state.Unavailable {
		return nil
	}
	image, err := s.readContentAppendImage(context.Background(), key, offset, original)
	if err != nil {
		return err
	}
	witness, present, err := s.readContentDomain(key)
	if err != nil && !isContentIdentityLoss(err) {
		return err
	}
	if isContentIdentityLoss(err) || !present || witness.DomainID != state.DomainID || image.Bytes != offset ||
		image.Count != original.Count ||
		!validateContentRows(state, original, image.activeRows()) {
		state.Unavailable = true
		return nil
	}
	appended, err := contentLocatorsForData(data, state.DomainID, original.Count, nil)
	if err != nil {
		return err
	}
	state.Rows = append(state.Rows, appended...)
	state.StateToken = uuid.NewString()
	state.Count = next.Count
	state.Skip = next.Skip
	state.SummaryDigest = contentDigest([]byte(next.Summary))
	return nil
}

func (s *JSONLStore) contentSummaryChanged(meta *SessionMeta) {
	if state := meta.ContentIdentity; state != nil && !state.Unavailable {
		state.StateToken = uuid.NewString()
		state.SummaryDigest = contentDigest([]byte(meta.Summary))
		state.Skip = meta.Skip
		state.Count = meta.Count
	}
}

// Legacy replacement arrays do not carry occurrence intent. They stay writable
// but cannot preserve a ref by matching body, timestamp or array position.
func (s *JSONLStore) prepareContentRewrite(key string, original SessionMeta, next *SessionMeta, data []byte) error {
	if original.ContentIdentity == nil {
		return nil
	}
	if original.ContentIdentity.Pending != nil {
		return s.contentPendingGuard(original)
	}
	if original.ContentIdentity.Unavailable {
		next.ContentIdentity = cloneContentState(original.ContentIdentity)
		return nil
	}
	prepared := next.ContentIdentity != nil && next.ContentIdentity.DomainID == original.ContentIdentity.DomainID &&
		next.ContentIdentity.StateToken != original.ContentIdentity.StateToken
	// Physical GC and checked edits already prepared their identity intent.
	// An empty physical image still belongs to that domain and retains Last.
	// Only an explicit unqualified legacy clear retires it here.
	if len(data) == 0 && !prepared {
		next.ContentIdentity = &contentIdentityState{
			Version:       ContentLineageVersion,
			DomainID:      uuid.NewString(),
			StateToken:    uuid.NewString(),
			SummaryDigest: contentDigest([]byte(next.Summary)),
			Skip:          next.Skip,
			Count:         next.Count,
			Rows:          []contentLocator{},
		}
		return nil
	}
	if next.ContentIdentity == nil || next.ContentIdentity.StateToken == original.ContentIdentity.StateToken {
		next.ContentIdentity = cloneContentState(original.ContentIdentity)
		next.ContentIdentity.Unavailable = true
		return nil
	}
	if next.ContentIdentity.Unavailable {
		return nil
	}
	refs := refsForLocators(next.ContentIdentity.Rows)
	rows, err := contentLocatorsForData(data, next.ContentIdentity.DomainID, 0, refs)
	if err != nil {
		return err
	}
	next.ContentIdentity.Rows = rows
	next.ContentIdentity.Count = next.Count
	next.ContentIdentity.Skip = next.Skip
	next.ContentIdentity.SummaryDigest = contentDigest([]byte(next.Summary))
	return nil
}

// Physical GC has an explicit unchanged selection from the current frame. It
// rewrites only locators, without assigning another occurrence or revision.
func (s *JSONLStore) prepareContentPhysicalRewrite(ctx context.Context, key string, meta *SessionMeta) error {
	if err := s.checkContentGarbageCollection(ctx, key, *meta); err != nil {
		return err
	}
	if meta.ContentIdentity == nil {
		return nil
	}
	meta.ContentIdentity = cloneContentState(meta.ContentIdentity)
	meta.ContentIdentity.StateToken = uuid.NewString()
	// A physical rewrite is a concurrent mutation for strict CAS. Occurrence
	// and revision refs remain unchanged while the opaque state token advances.
	return nil
}

func (s *JSONLStore) checkContentGarbageCollection(ctx context.Context, key string, meta SessionMeta) error {
	if meta.ContentIdentity == nil {
		_, present, err := s.readContentDomain(key)
		if err != nil {
			return err
		}
		if present {
			return ErrContentIdentityUnavailable
		}
		return nil
	}
	if meta.ContentIdentity.Unavailable {
		return ErrContentIdentityUnavailable
	}
	image, err := s.readContentImage(ctx, key)
	if err != nil {
		return err
	}
	frame, err := s.frameForContentImage(ctx, key, image)
	if err != nil {
		return err
	}
	if frame.IdentityStatus != ContentIdentityKnown {
		return ErrContentIdentityUnavailable
	}
	return nil
}

func (s *JSONLStore) AppendHistoryContent(
	ctx context.Context,
	key string,
	message providers.Message,
) (ContentAppendResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	// Activation status is deliberately ignored here: unproved identity must
	// not disable a normal durable content append. Only the metadata ownership
	// preflight is required; the writer validates refs against its safe prefix.
	if _, err := s.contentIdentityActiveLocked(ctx, key); err != nil {
		return ContentAppendResult{}, err
	}
	if messageutil.IsTransientAssistantThoughtMessage(message) {
		return ContentAppendResult{}, nil
	}
	meta, err := s.readMeta(key)
	if err != nil {
		return ContentAppendResult{}, err
	}
	if err = validateCanonicalHistoryKey(key, key, meta); err != nil {
		return ContentAppendResult{}, err
	}
	now := time.Now()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	ensureHistoryGeneration(&meta, key)
	suppliedID := uuid.Validate(message.HistoryID) == nil
	message = prepareHistoryMessage(message, now, providers.HistoryRun(ctx))
	if suppliedID {
		// Append always creates an occurrence, even if a caller accidentally
		// submits a persistence ID obtained from another message or domain.
		message.HistoryID = newHistoryID()
	}
	if runID := providers.HistoryRun(ctx); runID != "" {
		message.HistoryRunID = runID
	}
	receipts, err := s.appendHistoryMessagesFor(key, []providers.Message{message}, meta, false)
	if err != nil {
		return ContentAppendResult{}, err
	}
	result := ContentAppendResult{Entry: ContentEntry{Message: ordinaryContentMessage(message)}}
	if len(receipts) == 1 {
		result.Receipt = &receipts[0]
		result.Entry.Message = ordinaryContentMessage(receipts[0].Message)
		result.Entry.ReceiptBinding = &ContentReceiptBinding{
			SourceID: receipts[0].Message.HistoryID,
			Order:    receipts[0].Order,
		}
	}
	// We still hold the writer's lock; this selects only this exact commit's
	// locator. No callback or concurrent append can replace it with a later tail.
	current, err := s.readMeta(key)
	if err != nil {
		return ContentAppendResult{}, err
	}
	before := meta.ContentIdentity
	if state := current.ContentIdentity; state != nil && !state.Unavailable && len(state.Rows) > 0 &&
		before != nil && !before.Unavailable && before.DomainID == state.DomainID &&
		validHistoryStreamID(before.StateToken) && validHistoryStreamID(state.StateToken) &&
		before.StateToken != state.StateToken {
		ref := state.Rows[len(state.Rows)-1].Ref
		result.Entry.Ref = &ref
		result.DomainID = state.DomainID
		result.BeforeState = before.StateToken
		result.AfterState = state.StateToken
	}
	// Detach all persisted nested fields from the caller before any observer
	// can enqueue work or a later caller edit can mutate this exact result.
	data, err := json.Marshal(result.Entry.Message)
	if err != nil {
		return ContentAppendResult{}, err
	}
	var committed providers.Message
	if err = json.Unmarshal(data, &committed); err != nil {
		return ContentAppendResult{}, err
	}
	result.Entry.Message = committed
	return result, nil
}

func (s *JSONLStore) CommitHistoryContentEdit(
	ctx context.Context,
	key string,
	plan ContentEditPlan,
) (ContentFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return ContentFrame{}, err
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	image, frame, err := s.contentMutationImage(ctx, key)
	if err != nil {
		return ContentFrame{}, err
	}
	if frame.DomainID != plan.DomainID || frame.StateToken != plan.BeforeState {
		return ContentFrame{}, ErrContentStateChanged
	}
	if err = s.contentPendingGuard(image.Meta); err != nil {
		return ContentFrame{}, err
	}
	if len(plan.Entries) != len(frame.Entries) {
		return ContentFrame{}, ErrContentStateChanged
	}
	meta := copyMeta(image.Meta)
	meta.Summary = plan.Summary
	prepared := make([]providers.Message, len(plan.Entries))
	refs := make([]contentLocator, len(plan.Entries))
	active := image.activeRows()
	for i, entry := range plan.Entries {
		if entry.Ref == nil || *entry.Ref != *frame.Entries[i].Ref {
			return ContentFrame{}, ErrContentStateChanged
		}
		ref := *entry.Ref
		// Compare revisions only after the caller supplied the exact ref. Equality
		// never selects which duplicate occurrence the caller intended to edit.
		if contentJSONDigest(ordinaryContentMessage(entry.Message)) != contentJSONDigest(frame.Entries[i].Message) {
			ref.RevisionID = uuid.NewString()
		}
		refs[i] = contentLocator{Ref: ref}
		prepared[i] = entry.Message
		var raw historyRecord
		if json.Unmarshal(active[i].Raw, &raw) != nil {
			return ContentFrame{}, ErrContentIdentityUnavailable
		}
		prepared[i].HistoryID = raw.ID
		prepared[i].HistoryRunID = raw.RunID
		order, orderErr := decodedHistoryOrder(active[i].Raw)
		if orderErr == nil {
			prepared[i] = messageWithHistoryOrder(prepared[i], order)
		} else if !isHistoryProofError(orderErr) {
			return ContentFrame{}, orderErr
		}
		prepared[i] = prepareHistoryMessage(prepared[i], time.Now(), providers.HistoryRun(ctx))
	}
	if err = s.prepareReplacementHistoryOrder(key, prepared, &meta); err != nil {
		return ContentFrame{}, err
	}
	ensureHistoryGeneration(&meta, key)
	meta.HistoryParentGeneration = meta.HistoryGeneration
	meta.HistoryRetainedUntil = meta.HistoryPrefix
	meta.HistoryPrefix += int64(meta.Skip)
	resetHistoryGeneration(&meta)
	meta.Skip = 0
	meta.Count = len(prepared)
	meta.UpdatedAt = time.Now()
	meta.HistoryDigest, err = historyDigest(prepared)
	if err != nil {
		return ContentFrame{}, err
	}
	if len(prepared) > 0 {
		meta.HistoryTip = prepared[len(prepared)-1].HistoryID
	}
	meta.ContentIdentity.Rows = refs
	meta.ContentIdentity.StateToken = uuid.NewString()
	meta.ContentIdentity.SummaryDigest = contentDigest([]byte(meta.Summary))
	meta.ContentIdentity.Skip = 0
	meta.ContentIdentity.Count = meta.Count
	if err = s.commitHistoryRewrite(key, prepared, meta); err != nil {
		return ContentFrame{}, err
	}
	actual, err := s.readContentImage(ctx, key)
	if err != nil {
		return ContentFrame{}, err
	}
	return s.frameForContentImage(ctx, key, actual)
}

var (
	_ HistoryContentLineage  = (*JSONLStore)(nil)
	_ HistoryContentAppender = (*JSONLStore)(nil)
)

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
	qualified bool,
) error {
	if original.ContentIdentity == nil {
		return nil
	}
	if qualified {
		// Qualification may hydrate or fence this local state. The owning journal's
		// OLD checkpoint must remain an exact snapshot of the metadata on disk.
		original.ContentIdentity = cloneContentState(original.ContentIdentity)
		if err := s.hydrateContentStorage(context.Background(), key, &original, true, true); err != nil {
			if !onlyContentIdentityUnavailable(err) {
				return err
			}
			original.ContentIdentity.Unavailable = true
		}
	}
	next.ContentIdentity = cloneContentState(original.ContentIdentity)
	state := next.ContentIdentity
	if state.Unavailable {
		return nil
	}
	witness, present, err := s.readContentDomain(key)
	if err != nil && !isContentIdentityLoss(err) {
		return err
	}
	if isContentIdentityLoss(err) || !present || !contentWitnessMatches(witness, original) ||
		state.Count != original.Count || state.Skip != original.Skip ||
		next.Count-original.Count != bytes.Count(data, []byte{'\n'}) ||
		(original.HistoryBytes != nil && *original.HistoryBytes != offset) {
		state.Unavailable = true
		return nil
	}
	if qualified {
		image, readErr := s.readContentAppendImage(context.Background(), key, offset, original)
		if readErr != nil {
			return readErr
		}
		if image.Bytes != offset || image.Count != original.Count ||
			!validateContentRows(state, original, image.activeRows()) {
			state.Unavailable = true
			return nil
		}
	}
	appended, err := contentLocatorsForData(data, state.DomainID, original.Count, nil)
	if err != nil {
		return err
	}
	if state.storage == nil || state.storage.RowsLoaded {
		state.Rows = append(state.Rows, appended...)
	}
	if state.storage != nil {
		state.storage.Appended = appended
	}
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
			Version:         ContentLineageVersion,
			MutationVersion: original.ContentIdentity.MutationVersion,
			DomainID:        uuid.NewString(),
			StateToken:      uuid.NewString(),
			SummaryDigest:   contentDigest([]byte(next.Summary)),
			Skip:            next.Skip,
			Count:           next.Count,
			Rows:            []contentLocator{},
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
	if err := s.hydrateContentStorage(ctx, key, meta, true, true); err != nil {
		return err
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
	return s.appendHistoryContent(ctx, key, message, true)
}

// AppendHistoryContentInput accepts ordinary content without claiming to have
// revalidated the existing prefix. A caller needing refs/intervals must use the
// qualified AppendHistoryContent capability instead. This never initializes.
func (s *JSONLStore) AppendHistoryContentInput(
	ctx context.Context,
	key string,
	message providers.Message,
) (ContentAppendResult, error) {
	return s.appendHistoryContent(ctx, key, message, false)
}

func (s *JSONLStore) appendHistoryContent(
	ctx context.Context,
	key string,
	message providers.Message,
	qualified bool,
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
	message = newDeliveryOccurrence(prepareHistoryMessage(message, now, providers.HistoryRun(ctx)))
	if suppliedID {
		// Append always creates an occurrence, even if a caller accidentally
		// submits a persistence ID obtained from another message or domain.
		message.HistoryID = newHistoryID()
	}
	if runID := providers.HistoryRun(ctx); runID != "" {
		message.HistoryRunID = runID
	}
	// Detach before writing, including the body returned with a confirmed NEW
	// outcome whose operational recovery phase remains pending.
	encoded, err := json.Marshal(message)
	if err != nil {
		return ContentAppendResult{}, err
	}
	var detached providers.Message
	if err = json.Unmarshal(encoded, &detached); err != nil {
		return ContentAppendResult{}, err
	}
	message = detached
	var outcome historyAppendOutcome
	receipts, appendErr := s.appendHistoryMessagesOutcome(
		key,
		[]providers.Message{message},
		meta,
		false,
		qualified,
		&outcome,
	)
	if !outcome.committed {
		return ContentAppendResult{}, appendErr
	}
	result := ContentAppendResult{Entry: ContentEntry{Message: ordinaryContentMessage(message)}}
	// A pending operational phase cannot mint a receipt or locator. Preserve
	// the accepted body alongside its distinct, non-retryable commit error.
	if appendErr != nil {
		return result, appendErr
	}
	if len(receipts) == 1 {
		result.Receipt = &receipts[0]
		result.Entry.Message = ordinaryContentMessage(receipts[0].Message)
		// An independently committed Order can survive content proof loss.
		// Do not turn it into a qualified content binding for a fenced domain.
		identity := outcome.meta.ContentIdentity
		if qualified && (identity == nil || (!identity.Unavailable && len(identity.opaque) == 0)) {
			result.Entry.ReceiptBinding = &ContentReceiptBinding{
				SourceID: receipts[0].Message.HistoryID,
				Order:    receipts[0].Order,
			}
		}
	}
	// We still hold the writer's lock; this selects only this exact commit's
	// locator. No callback or concurrent append can replace it with a later tail.
	current := outcome.meta
	before := meta.ContentIdentity
	if state := current.ContentIdentity; qualified && state != nil && !state.Unavailable && len(state.Rows) > 0 &&
		before != nil && !before.Unavailable && before.DomainID == state.DomainID &&
		validHistoryStreamID(before.StateToken) && validHistoryStreamID(state.StateToken) &&
		before.StateToken != state.StateToken {
		ref := state.Rows[len(state.Rows)-1].Ref
		result.Entry.Ref = &ref
		result.DomainID = state.DomainID
		result.BeforeState = before.StateToken
		result.AfterState = state.StateToken
	}
	return result, nil
}

// CommitHistoryContentEdit preserves the checked-edit signature introduced by
// this capability. Its source-owned operation is discoverable by the consumer;
// another dependent edit waits for ApplicationACK. Callers without a command ID
// gain no new exactly-once guarantee from the generated local operation UUID.
func (s *JSONLStore) CommitHistoryContentEdit(
	ctx context.Context,
	key string,
	plan ContentEditPlan,
) (ContentFrame, error) {
	result, err := s.CommitHistoryContentMutation(ctx, key, ContentMutationPlan{
		MutationID: uuid.NewString(), IntentDigest: contentJSONDigest(plan),
		DomainID: plan.DomainID, BeforeState: plan.BeforeState, Kind: ContentMutationEdit,
		Edit: &ContentMutationEditIntent{Entries: plan.Entries, Summary: plan.Summary},
	})
	if err != nil {
		return ContentFrame{}, err
	}
	return s.ReadHistoryContentMutationFrame(ctx, key, result.Transition.MutationID, ContentAfter)
}

var (
	_ HistoryContentLineage  = (*JSONLStore)(nil)
	_ HistoryContentAppender = (*JSONLStore)(nil)
)

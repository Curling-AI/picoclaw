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
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/messageutil"
)

// A locator associates a random ref with bytes in this exact physical image.
// It is never used to match messages across images or exposed as an identity.
type contentLocator struct {
	Ref    ContentRef `json:"ref"`
	Line   int        `json:"line"`
	Digest string     `json:"digest"`
}

type contentSnapshot struct {
	StateToken string           `json:"state"`
	Summary    string           `json:"summary"`
	Skip       int              `json:"skip"`
	Rows       []contentLocator `json:"rows"`
}

type contentPending struct {
	Transition ContentTransition `json:"transition"`
	PlanDigest string            `json:"planDigest"`
	Before     contentSnapshot   `json:"before"`
	After      contentSnapshot   `json:"after"`
}

type contentAcknowledged struct {
	Transition ContentTransition `json:"transition"`
	PlanDigest string            `json:"planDigest"`
	AckDigest  string            `json:"ackDigest"`
}

type contentIdentityState struct {
	Version       uint8                `json:"version"`
	DomainID      string               `json:"domain"`
	StateToken    string               `json:"state"`
	SummaryDigest string               `json:"summaryDigest"`
	Skip          int                  `json:"skip"`
	Count         int                  `json:"count"`
	Rows          []contentLocator     `json:"rows"`
	Unavailable   bool                 `json:"unavailable,omitempty"`
	Pending       *contentPending      `json:"pending,omitempty"`
	Last          *contentAcknowledged `json:"last,omitempty"`
}

type contentDomainWitness struct {
	Version  uint8  `json:"version"`
	Key      string `json:"key"`
	DomainID string `json:"domain"`
}

type contentRawRow struct {
	Line    int
	Raw     []byte
	Message providers.Message
}

type contentImage struct {
	Meta           SessionMeta
	MetaBytes      []byte
	Rows           []contentRawRow
	Count          int
	Bytes          int64
	Exists         bool
	Observation    string
	WriterRecovery bool
}

func contentDigest(data []byte) string   { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func contentJSONDigest(value any) string { data, _ := json.Marshal(value); return contentDigest(data) }

func cloneContentState(state *contentIdentityState) *contentIdentityState {
	if state == nil {
		return nil
	}
	data, _ := json.Marshal(state)
	var cloned contentIdentityState
	_ = json.Unmarshal(data, &cloned)
	return &cloned
}

func ordinaryContentMessage(message providers.Message) providers.Message {
	message.HistoryID = ""
	message.HistoryRunID = ""
	message.HistoryOrderVersion = 0
	message.HistoryStreamID = ""
	message.HistorySequence = 0
	return message
}

func (s *JSONLStore) contentDomainPath(key string) string {
	return s.jsonlPath(key) + ".content-domain"
}

// No readMeta, journal recovery, witness activation, marker write or cache fill
// belongs to a content getter. Even warm callers inspect the actual checkpoint.
func (s *JSONLStore) readContentMeta(key string) (SessionMeta, []byte, error) {
	if err := s.checkContentJournalSettled(key); err != nil {
		return SessionMeta{}, nil, err
	}
	data, err := os.ReadFile(s.metaPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return SessionMeta{Key: key}, nil, nil
	}
	if err != nil {
		return SessionMeta{}, nil, fmt.Errorf("memory: read content metadata: %w", err)
	}
	var meta SessionMeta
	if err = json.Unmarshal(data, &meta); err != nil {
		return SessionMeta{}, nil, fmt.Errorf("memory: decode content metadata: %w", err)
	}
	if meta.Key == "" {
		meta.Key = key
	}
	if meta.Key != key || meta.Skip < 0 || meta.Count < 0 || meta.Skip > meta.Count {
		return SessionMeta{}, nil, ErrContentIdentityUnavailable
	}
	return meta, data, nil
}

// Readers must not classify an owning write as never initialized before its
// NEW identity metadata and witness are both published. Only a writer recovers.
func (s *JSONLStore) checkContentJournalSettled(key string) error {
	for _, path := range []string{s.historyRecoveryPath(key), s.historyDecisionPath(key)} {
		if _, err := os.Stat(path); err == nil {
			return ErrHistoryRecoveryRequired
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *JSONLStore) canonicalContentKey(ctx context.Context, key string) error {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key {
		return ErrHistoryOwnershipUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The normal canonical sk_v1 key has the same established resolver shortcut.
	if shouldShortCircuitSessionResolve(key) {
		meta, _, err := s.readContentMeta(key)
		if err != nil {
			return err
		}
		if meta.Key == key && s.sessionExists(key) {
			return nil
		}
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		physical := strings.TrimSuffix(entry.Name(), ".meta.json")
		data, readErr := os.ReadFile(s.metaPath(physical))
		if readErr != nil {
			return fmt.Errorf("%w: %w", ErrHistoryOwnershipUnavailable, readErr)
		}
		var owner historyOwnership
		if readErr = json.Unmarshal(data, &owner); readErr != nil {
			return fmt.Errorf("%w: %w", ErrHistoryOwnershipUnavailable, readErr)
		}
		if physical == sanitizeKey(key) && owner.Key != "" && owner.Key != key {
			return ErrHistoryOwnershipUnavailable
		}
		for _, alias := range owner.Aliases {
			if alias == key && owner.Key != key {
				return ErrHistoryOwnershipUnavailable
			}
		}
	}
	return nil
}

func (s *JSONLStore) readContentImage(ctx context.Context, key string) (contentImage, error) {
	if err := ctx.Err(); err != nil {
		return contentImage{}, err
	}
	meta, metaBytes, err := s.readContentMeta(key)
	if err != nil {
		return contentImage{}, err
	}
	return s.scanContentImage(ctx, key, meta, metaBytes, nil)
}

// The owning append writer has already selected offset with historyAppendOffset.
// Reuse only that committed prefix and the metadata it loaded before its write.
// Strict getter checks (a partial tail or lost Count) must not disable the
// existing content-only append recovery. Actual I/O and ownership errors remain
// errors, and inconsistent independent refs are invalidated by the caller.
func (s *JSONLStore) readContentAppendImage(
	ctx context.Context,
	key string,
	offset int64,
	meta SessionMeta,
) (contentImage, error) {
	if meta.Key != key {
		return contentImage{}, ErrHistoryOwnershipUnavailable
	}
	return s.scanContentImage(ctx, key, meta, nil, &offset)
}

func (s *JSONLStore) scanContentImage(
	ctx context.Context,
	key string,
	meta SessionMeta,
	metaBytes []byte,
	prefix *int64,
) (contentImage, error) {
	if err := ctx.Err(); err != nil {
		return contentImage{}, err
	}
	image := contentImage{Meta: meta, MetaBytes: metaBytes}
	file, err := os.Open(s.jsonlPath(key))
	if errors.Is(err, os.ErrNotExist) {
		if prefix != nil || !emptyHistoryContent(meta) {
			return contentImage{}, fmt.Errorf("memory: content source: %w", err)
		}
		image.Observation = contentJSONDigest([]any{key, metaBytes, "missing"})
		return image, nil
	}
	if err != nil {
		return contentImage{}, err
	}
	defer file.Close()
	image.Exists = true
	var reader io.Reader = file
	if prefix != nil {
		info, statErr := file.Stat()
		if statErr != nil {
			return contentImage{}, statErr
		}
		if *prefix < 0 || *prefix > info.Size() {
			return contentImage{}, ErrContentStateChanged
		}
		reader = io.LimitReader(file, *prefix)
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	hash := sha256.New()
	for scanner.Scan() {
		if err = ctx.Err(); err != nil {
			return contentImage{}, err
		}
		committed := scanner.Bytes()
		image.Bytes += int64(len(committed))
		_, _ = hash.Write(committed)
		raw := bytes.TrimRight(committed, "\r\n")
		if len(raw) == 0 {
			continue
		}
		image.Count++
		var message providers.Message
		if json.Unmarshal(raw, &message) != nil || messageutil.IsTransientAssistantThoughtMessage(message) {
			continue
		}
		image.Rows = append(
			image.Rows,
			contentRawRow{Line: image.Count, Raw: bytes.Clone(raw), Message: ordinaryContentMessage(message)},
		)
	}
	if err = scanner.Err(); err != nil {
		return contentImage{}, err
	}
	if err = ctx.Err(); err != nil {
		return contentImage{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return contentImage{}, err
	}
	expectedBytes := info.Size()
	if prefix != nil {
		if *prefix > info.Size() {
			return contentImage{}, ErrContentStateChanged
		}
		expectedBytes = *prefix
	}
	if expectedBytes != image.Bytes {
		return contentImage{}, fmt.Errorf("%w: incomplete physical record", ErrContentStateChanged)
	}
	image.Observation = contentJSONDigest([]any{key, metaBytes, hex.EncodeToString(hash.Sum(nil)), image.Bytes})
	return image, nil
}

func (image contentImage) activeRows() []contentRawRow {
	rows := make([]contentRawRow, 0, len(image.Rows))
	for _, row := range image.Rows {
		if row.Line > image.Meta.Skip {
			rows = append(rows, row)
		}
	}
	return rows
}

func (s *JSONLStore) readContentDomain(key string) (contentDomainWitness, bool, error) {
	data, err := os.ReadFile(s.contentDomainPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return contentDomainWitness{}, false, nil
	}
	if err != nil {
		return contentDomainWitness{}, false, err
	}
	var witness contentDomainWitness
	if json.Unmarshal(data, &witness) != nil || witness.Version != ContentLineageVersion || witness.Key != key ||
		!validHistoryStreamID(witness.DomainID) {
		return witness, true, ErrContentIdentityUnavailable
	}
	return witness, true, nil
}

func (s *JSONLStore) ensureContentDomain(key string, before, next *contentIdentityState) error {
	if next == nil || next.Unavailable {
		return nil
	}
	witness, present, err := s.readContentDomain(key)
	if err != nil {
		return err
	}
	if present && witness.DomainID == next.DomainID {
		return nil
	}
	// Only an explicit baseline or clear can create/retire a content domain.
	if before != nil && (before.DomainID == next.DomainID || len(next.Rows) != 0 || next.Pending != nil) {
		return ErrContentIdentityUnavailable
	}
	if present && (before == nil || witness.DomainID != before.DomainID) {
		return ErrContentIdentityUnavailable
	}
	return s.writeContentDomain(key, next.DomainID)
}

// Called only after an owning journal has durably selected and verified NEW.
// It may finish its own interrupted witness write. Without that journal, a
// missing/torn witness remains identity loss, never an automatic new domain.
func (s *JSONLStore) recoverContentDomain(key string, before, next *contentIdentityState) error {
	if next == nil || next.Unavailable {
		return nil
	}
	if next.Version != ContentLineageVersion || !validHistoryStreamID(next.DomainID) ||
		!validHistoryStreamID(next.StateToken) {
		return ErrContentIdentityUnavailable
	}
	witness, present, err := s.readContentDomain(key)
	if err != nil && !errors.Is(err, ErrContentIdentityUnavailable) {
		return err
	}
	if err == nil && present {
		if witness.DomainID == next.DomainID {
			return nil
		}
		if before == nil || witness.DomainID != before.DomainID {
			return ErrContentIdentityUnavailable
		}
	}
	return s.writeContentDomain(key, next.DomainID)
}

func (s *JSONLStore) writeContentDomain(key, domainID string) error {
	data, err := json.Marshal(contentDomainWitness{Version: ContentLineageVersion, Key: key, DomainID: domainID})
	if err != nil {
		return err
	}
	if err = s.writeSessionFile(s.contentDomainPath(key), data, 0o600); err != nil {
		return err
	}
	return s.syncHistoryFile(s.contentDomainPath(key))
}

func validateContentRows(state *contentIdentityState, meta SessionMeta, rows []contentRawRow) bool {
	if state == nil || state.Unavailable || state.Version != ContentLineageVersion ||
		!validHistoryStreamID(state.DomainID) ||
		!validHistoryStreamID(state.StateToken) ||
		state.Skip != meta.Skip ||
		state.Count != meta.Count ||
		state.SummaryDigest != contentDigest([]byte(meta.Summary)) ||
		len(state.Rows) != len(rows) {
		return false
	}
	seen := make(map[string]bool, len(rows))
	for i, locator := range state.Rows {
		if locator.Line != rows[i].Line || locator.Digest != contentDigest(rows[i].Raw) ||
			locator.Ref.DomainID != state.DomainID ||
			!validHistoryStreamID(locator.Ref.OccurrenceID) ||
			!validHistoryStreamID(locator.Ref.RevisionID) ||
			seen[locator.Ref.OccurrenceID] {
			return false
		}
		seen[locator.Ref.OccurrenceID] = true
	}
	return true
}

func (s *JSONLStore) frameForContentImage(ctx context.Context, key string, image contentImage) (ContentFrame, error) {
	if err := ctx.Err(); err != nil {
		return ContentFrame{}, err
	}
	active := image.activeRows()
	frame := ContentFrame{
		Version:        ContentLineageVersion,
		CanonicalKey:   key,
		IdentityStatus: ContentIdentityUninitialized,
		Observation:    image.Observation,
		Summary:        image.Meta.Summary,
		Entries:        make([]ContentEntry, len(active)),
	}
	witness, present, err := s.readContentDomain(key)
	if err != nil && !errors.Is(err, ErrContentIdentityUnavailable) {
		return ContentFrame{}, err
	}
	state := image.Meta.ContentIdentity
	if state != nil || present {
		frame.IdentityStatus = ContentIdentityUnavailable
	}
	known := err == nil && present && state != nil && witness.DomainID == state.DomainID &&
		image.Count == image.Meta.Count &&
		validateContentRows(state, image.Meta, active)
	if known {
		frame.IdentityStatus = ContentIdentityKnown
		frame.DomainID = state.DomainID
		frame.StateToken = state.StateToken
	}
	for i, row := range active {
		frame.Entries[i] = ContentEntry{Message: row.Message}
		if known {
			ref := state.Rows[i].Ref
			frame.Entries[i].Ref = &ref
		}
	}
	// No binding is inferred from decodedHistoryID. Only a persisted UUID and
	// independently valid v1 witness/checkpoint may expose existing receipts.
	eligible, err := s.contentReceiptEligibility(key, image)
	if err != nil {
		return ContentFrame{}, err
	}
	if eligible {
		for i, row := range active {
			var record historyRecord
			if json.Unmarshal(row.Raw, &record) != nil || uuid.Validate(record.ID) != nil {
				continue
			}
			proof, proofErr := decodedHistoryOrder(row.Raw)
			if proofErr == nil && proof.Valid() && proof.StreamID == image.Meta.HistoryStreamID &&
				proof.Sequence <= image.Meta.HistorySequence &&
				historyLineageMatches(row.Raw, image.Meta, key) {
				frame.Entries[i].ReceiptBinding = &ContentReceiptBinding{SourceID: record.ID, Order: proof}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return ContentFrame{}, err
	}
	return frame, nil
}

func (s *JSONLStore) contentReceiptEligibility(key string, image contentImage) (bool, error) {
	meta := image.Meta
	marked, err := s.hasHistoryRecoveryMarker(key)
	if err != nil {
		return false, err
	}
	if marked || meta.HistoryRecoveryRequired || meta.HistoryOrderVersion != HistoryOrderVersion ||
		meta.HistoryBytes == nil ||
		*meta.HistoryBytes != image.Bytes ||
		meta.Count != image.Count {
		return false, nil
	}
	data, err := os.ReadFile(s.historyOrderWitnessPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !validContentReceiptWitness(data, meta) {
		return false, nil
	}
	// A valid witness cannot attest a source whose records lost their proof.
	// Reuse the pure existing origin verifier; decoded fallback IDs are never
	// exported as bindings below. It neither repairs metadata nor writes fences.
	ordered, end, proofErr := s.readCommittedHistoryOrderOrigins(key, meta, 0, true)
	if isHistoryProofError(proofErr) {
		return false, nil
	}
	if proofErr != nil {
		return false, proofErr
	}
	return end == image.Bytes && validateHistoryMessageOrders(ordered, meta) == nil, nil
}

func validContentReceiptWitness(data []byte, meta SessionMeta) bool {
	var witness historyOrderWitness
	return json.Unmarshal(data, &witness) == nil && validHistoryOrderWitness(witness) &&
		witness == witnessForHistory(meta) && validateHistoryOrderState(meta) == nil
}

func (s *JSONLStore) ReadHistoryContentFrame(ctx context.Context, key string) (ContentFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return ContentFrame{}, err
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	image, err := s.readContentImage(ctx, key)
	if err != nil {
		return ContentFrame{}, err
	}
	return s.frameForContentImage(ctx, key, image)
}

func (s *JSONLStore) InitializeHistoryContentIdentity(
	ctx context.Context,
	key, observation string,
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
	// This operation is a writer: it may settle its own prior OLD/NEW journal.
	writer, err := s.readMeta(key)
	if err != nil {
		return ContentFrame{}, err
	}
	image, err := s.readContentImage(ctx, key)
	if err != nil {
		return ContentFrame{}, err
	}
	frame, err := s.frameForContentImage(ctx, key, image)
	if err != nil {
		return ContentFrame{}, err
	}
	if frame.IdentityStatus == ContentIdentityKnown {
		return frame, nil
	}
	if frame.IdentityStatus != ContentIdentityUninitialized {
		return ContentFrame{}, ErrContentIdentityUnavailable
	}
	if observation == "" || observation != image.Observation {
		return ContentFrame{}, ErrContentStateChanged
	}
	image.WriterRecovery = writer.HistoryRecoveryRequired
	// A forward content baseline is not order recovery. In particular, do
	// not replace a lost byte checkpoint with the observed file size and then
	// allow the next append to issue a receipt over an unproved prior stream.
	eligible, err := s.contentReceiptEligibility(key, image)
	if err != nil {
		return ContentFrame{}, err
	}
	if image.Count > 0 && !eligible {
		image.WriterRecovery = true
	}
	next := copyMeta(image.Meta)
	ensureHistoryGeneration(&next, key)
	next.Count = image.Count
	state := &contentIdentityState{
		Version:       ContentLineageVersion,
		DomainID:      uuid.NewString(),
		StateToken:    uuid.NewString(),
		SummaryDigest: contentDigest([]byte(next.Summary)),
		Skip:          next.Skip,
		Count:         next.Count,
		Rows:          make([]contentLocator, 0, len(frame.Entries)),
	}
	for _, row := range image.activeRows() {
		state.Rows = append(
			state.Rows,
			contentLocator{
				Ref: ContentRef{
					DomainID:     state.DomainID,
					OccurrenceID: uuid.NewString(),
					RevisionID:   uuid.NewString(),
				},
				Line:   row.Line,
				Digest: contentDigest(row.Raw),
			},
		)
	}
	next.ContentIdentity = state
	if err = s.commitContentMetadata(ctx, key, image, next); err != nil {
		return ContentFrame{}, err
	}
	image, err = s.readContentImage(ctx, key)
	if err != nil {
		return ContentFrame{}, err
	}
	return s.frameForContentImage(ctx, key, image)
}

func (s *JSONLStore) commitContentMetadata(
	ctx context.Context,
	key string,
	image contentImage,
	next SessionMeta,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	next.HistoryRecoveryRequired = next.HistoryRecoveryRequired || image.WriterRecovery
	size := image.Bytes
	next.HistoryBytes = &size
	recovery := historyWriteRecovery{Meta: image.Meta, Next: next, Existed: image.Exists}
	if image.Exists {
		recovery.AppendOffset = &size
	}
	if err := s.beginHistoryWrite(key, recovery); err != nil {
		return err
	}
	if !image.Exists {
		// An explicitly initialized empty source still needs its zero-byte
		// physical checkpoint. Publish it through the same OLD/NEW journal;
		// otherwise the first append sees metadata for a missing source.
		if err := s.writeSessionFile(s.jsonlPath(key), nil, 0o644); err != nil {
			return errors.Join(err, s.recoverFailedHistoryWrite(key))
		}
		if err := s.syncHistoryFile(s.jsonlPath(key)); err != nil {
			return errors.Join(err, s.recoverFailedHistoryWrite(key))
		}
	}
	if err := s.writeMeta(key, next); err != nil {
		committed, recoveryErr := s.recoverExpectedHistoryCommit(key, next)
		if committed {
			return recoveryErr
		}
		return errors.Join(err, recoveryErr)
	}
	if err := s.ensureContentDomain(key, image.Meta.ContentIdentity, next.ContentIdentity); err != nil {
		return err
	}
	return s.finishHistoryWrite(key)
}

func contentCheckpointMatches(current, expected SessionMeta) bool {
	if !reflect.DeepEqual(current.ContentIdentity, expected.ContentIdentity) {
		return false
	}
	if expected.ContentIdentity != nil {
		return current.Skip == expected.ContentIdentity.Skip &&
			contentDigest([]byte(current.Summary)) == expected.ContentIdentity.SummaryDigest
	}
	return true
}

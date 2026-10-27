package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/messageutil"
)

const (
	HistoryOrderVersion = uint8(1)
	MaxHistorySequence  = uint64(1<<53 - 1)
)

var (
	ErrHistoryOrderUnavailable = errors.New("history order proof unavailable or invalid")
	ErrHistoryOrderExhausted   = errors.New("history order sequence exhausted")
	// ErrHistoryReceiptUnavailable rejects a receipt append before writing its input.
	// Content-only runtime writers can persist it without claiming order.
	ErrHistoryReceiptUnavailable = fmt.Errorf("%w: %w", ErrHistoryOrderUnavailable, ErrHistoryRecoveryRequired)
	// ErrHistoryOwnershipUnavailable reports resolution failure before this append starts. Runtime callers may use
	// their legacy persistence path without claiming a canonical receipt.
	ErrHistoryOwnershipUnavailable = fmt.Errorf("%w: canonical ownership unavailable", ErrHistoryOrderUnavailable)
)

func isHistoryProofError(err error) bool {
	return errors.Is(err, ErrHistoryOrderUnavailable) || errors.Is(err, ErrHistoryRecoveryRequired) ||
		errors.Is(err, ErrHistoryCursor)
}

// HistoryOrderProof identifies a committed content event in one persistent
// canonical stream. It is independent of file positions and transcript edits.
// A completely zero proof means that a legacy record has no ordering evidence.
type HistoryOrderProof struct {
	Version  uint8  `json:"version"`
	StreamID string `json:"streamId"`
	Sequence uint64 `json:"sequence"`
}

type CommittedHistoryMessage struct {
	Message providers.Message `json:"message"`
	Order   HistoryOrderProof `json:"order"`
	// This is an atomic source-origin attestation, not a v1 proof or sequence.
	LegacyBeforeStreamID string `json:"-"`
}

func (order HistoryOrderProof) Valid() bool {
	return order.Version == HistoryOrderVersion && validHistoryStreamID(order.StreamID) &&
		order.Sequence > 0 && order.Sequence <= MaxHistorySequence
}

func validHistoryStreamID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func messageHistoryOrder(message providers.Message) HistoryOrderProof {
	return HistoryOrderProof{
		Version:  message.HistoryOrderVersion,
		StreamID: message.HistoryStreamID,
		Sequence: message.HistorySequence,
	}
}

func messageWithHistoryOrder(message providers.Message, order HistoryOrderProof) providers.Message {
	message.HistoryOrderVersion, message.HistoryStreamID, message.HistorySequence = order.Version, order.StreamID, order.Sequence
	return message
}

func persistedHistoryOrder(message providers.Message) *HistoryOrderProof {
	order := messageHistoryOrder(message)
	if order == (HistoryOrderProof{}) {
		return nil
	}
	return &order
}

func persistedLegacyHistoryStream(message providers.Message, meta SessionMeta) string {
	if messageHistoryOrder(message) == (HistoryOrderProof{}) && meta.HistoryOrderVersion == HistoryOrderVersion &&
		!meta.HistoryRecoveryRequired {
		return meta.HistoryStreamID
	}
	return ""
}

func decodedLegacyHistoryStream(data []byte) (string, error) {
	var raw struct {
		Legacy json.RawMessage `json:"history_legacy_before_stream_id"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return "", ErrHistoryOrderUnavailable
	}
	if len(raw.Legacy) == 0 {
		return "", nil
	}
	var stream string
	if json.Unmarshal(raw.Legacy, &stream) != nil || !validHistoryStreamID(stream) {
		return "", ErrHistoryOrderUnavailable
	}
	return stream, nil
}

func decodedHistoryOrder(data []byte) (HistoryOrderProof, error) {
	var raw struct {
		Order       json.RawMessage `json:"history_order"`
		Unavailable json.RawMessage `json:"history_recovery_required"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return HistoryOrderProof{}, ErrHistoryOrderUnavailable
	}
	if markerErr := historyRecoveryMarkerError(raw.Unavailable); markerErr != nil {
		return HistoryOrderProof{}, fmt.Errorf("%w: %w", ErrHistoryReceiptUnavailable, markerErr)
	}
	if len(raw.Order) == 0 {
		return HistoryOrderProof{}, nil
	}
	var order HistoryOrderProof
	if json.Unmarshal(raw.Order, &order) != nil || !order.Valid() {
		return HistoryOrderProof{}, ErrHistoryOrderUnavailable
	}
	return order, nil
}

// Content readers may retain an existing proof as private storage evidence,
// including on a fenced record. This never makes the proof receipt-eligible.
func decodedStoredContentOrder(data []byte) HistoryOrderProof {
	var raw struct {
		Order *HistoryOrderProof `json:"history_order"`
	}
	if json.Unmarshal(data, &raw) != nil || raw.Order == nil || !raw.Order.Valid() {
		return HistoryOrderProof{}
	}
	return *raw.Order
}

// Detect lost ordering evidence before a content mutation can remove it. A
// content operation may proceed after latching recovery; receipt operations
// still reject that source. Inspect skipped rows too, before compact/clear.
func (s *JSONLStore) prepareHistoryContentOrder(key string, meta *SessionMeta) error {
	if meta.HistoryRecoveryRequired {
		return nil
	}
	stored, _, err := s.readCommittedHistoryOrderOrigins(key, *meta, 0, false)
	if err == nil {
		err = validateHistoryMessageOrders(stored, *meta)
	}
	if err == nil {
		return nil
	}
	if !isHistoryProofError(err) {
		return err
	}
	return s.markHistoryRecoveryRequired(key, meta)
}

func (s *JSONLStore) readHistoryForContentMutation(key string, meta *SessionMeta) ([]providers.Message, error) {
	if err := s.prepareHistoryContentOrder(key, meta); err != nil {
		return nil, err
	}
	if meta.HistoryRecoveryRequired {
		return readProviderMessages(s.jsonlPath(key), meta.Skip)
	}
	return readMessages(s.jsonlPath(key), meta.Skip)
}

func validateHistoryOrderState(meta SessionMeta) error {
	if meta.HistoryRecoveryRequired {
		return ErrHistoryReceiptUnavailable
	}
	if meta.HistoryOrderVersion == 0 && meta.HistoryStreamID == "" && meta.HistorySequence == 0 &&
		meta.HistoryLegacyFileGeneration == "" && meta.HistoryLegacyBytes == 0 && meta.HistoryOrderWitnessVersion == 0 {
		return nil
	}
	if meta.HistoryOrderVersion != HistoryOrderVersion ||
		meta.HistoryOrderWitnessVersion != historyOrderWitnessVersion ||
		!validHistoryStreamID(meta.HistoryStreamID) ||
		meta.HistorySequence == 0 ||
		meta.HistorySequence > MaxHistorySequence {
		return ErrHistoryOrderUnavailable
	}
	if !validHistoryStreamID(meta.HistoryLegacyFileGeneration) || meta.HistoryLegacyBytes < 0 {
		return ErrHistoryOrderUnavailable
	}
	if meta.HistoryFileGeneration == meta.HistoryLegacyFileGeneration && meta.HistoryBytes != nil &&
		meta.HistoryLegacyBytes > *meta.HistoryBytes {
		return ErrHistoryOrderUnavailable
	}
	return nil
}

func validateHistoryMessageOrders(messages []providers.Message, meta SessionMeta) error {
	if err := validateHistoryOrderState(meta); err != nil {
		return err
	}
	seen := make(map[uint64]struct{})
	for _, message := range messages {
		order := messageHistoryOrder(message)
		if order == (HistoryOrderProof{}) {
			continue
		}
		if !order.Valid() || order.StreamID != meta.HistoryStreamID || order.Sequence > meta.HistorySequence {
			return ErrHistoryOrderUnavailable
		}
		if _, duplicate := seen[order.Sequence]; duplicate {
			return ErrHistoryOrderUnavailable
		}
		seen[order.Sequence] = struct{}{}
	}
	return nil
}

// Initializing a legacy stream never assigns order to old records. Check that
// no previous ordered stream is being hidden by missing metadata before minting
// the first sequence. This one-time scan is not used by normal ordered appends.
func (s *JSONLStore) ensureHistoryOrderStream(key string, meta *SessionMeta) error {
	if err := validateHistoryOrderState(*meta); err != nil {
		return err
	}
	if meta.HistoryOrderVersion != 0 {
		return nil
	}
	_, legacyBytes, err := s.readCommittedHistoryOrderOrigins(key, *meta, 0, true)
	if err != nil {
		return err
	}
	ensureHistoryGeneration(meta, key)
	meta.HistoryOrderVersion, meta.HistoryStreamID = HistoryOrderVersion, newHistoryID()
	meta.HistoryLegacyFileGeneration, meta.HistoryLegacyBytes = meta.HistoryFileGeneration, legacyBytes
	meta.HistoryOrderWitnessVersion = historyOrderWitnessVersion
	return nil
}

func (s *JSONLStore) allocateHistoryOrder(
	key string,
	message providers.Message,
	meta *SessionMeta,
) (providers.Message, error) {
	if err := s.ensureHistoryOrderStream(key, meta); err != nil {
		return providers.Message{}, err
	}
	if meta.HistorySequence == MaxHistorySequence {
		return providers.Message{}, ErrHistoryOrderExhausted
	}
	meta.HistorySequence++
	return messageWithHistoryOrder(message, HistoryOrderProof{
		Version: HistoryOrderVersion, StreamID: meta.HistoryStreamID, Sequence: meta.HistorySequence,
	}), nil
}

func (s *JSONLStore) prepareReplacementHistoryOrder(key string, prepared []providers.Message, meta *SessionMeta) error {
	if meta.HistoryRecoveryRequired {
		old, err := readProviderMessages(s.jsonlPath(key), meta.Skip)
		if err != nil {
			return err
		}
		byID := make(map[string]providers.Message, len(old))
		for _, message := range old {
			byID[message.HistoryID] = message
		}
		for index, message := range prepared {
			prepared[index] = messageWithHistoryOrder(message, HistoryOrderProof{})
			if previous, exists := byID[message.HistoryID]; exists && sameHistoryCore(previous, message) {
				prepared[index] = messageWithHistoryOrder(message, messageHistoryOrder(previous))
			}
		}
		return nil
	}
	// On a proven source only committed origin evidence can be reused.
	// Recovery-required content was handled above without allocating proofs.
	old, _, err := s.readCommittedHistoryOrderOrigins(key, *meta, meta.Skip, false)
	if err != nil {
		return err
	}
	if err := validateHistoryMessageOrders(old, *meta); err != nil {
		return err
	}
	byID := make(map[string]providers.Message, len(old))
	for _, message := range old {
		byID[message.HistoryID] = message
	}
	for index, message := range prepared {
		if previous, exists := byID[message.HistoryID]; exists && sameHistoryCore(previous, message) {
			prepared[index] = messageWithHistoryOrder(message, messageHistoryOrder(previous))
			continue
		}
		ordered, err := s.allocateHistoryOrder(key, message, meta)
		if err != nil {
			return err
		}
		prepared[index] = ordered
	}
	return validateHistoryMessageOrders(prepared, *meta)
}

// The caller holds the physical-session lock. Alias ownership is verified
// against the canonical metadata after resolution, before returning any proof.
func validateCanonicalHistoryKey(requested, canonical string, meta SessionMeta) error {
	if meta.Key != canonical {
		return ErrHistoryOrderUnavailable
	}
	if requested != canonical {
		for _, alias := range meta.Aliases {
			if alias == requested {
				return nil
			}
		}
		return ErrHistoryOrderUnavailable
	}
	return nil
}

// Format/ownership failures retain the checked legacy write path. A real I/O
// error or canceled operation does not authorize retrying through that path.
func historyOwnershipReadError(operation string, err error) error {
	var pathErr *os.PathError
	var linkErr *os.LinkError
	var syscallErr *os.SyscallError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &syscallErr) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("memory: %s: %w", operation, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrHistoryOwnershipUnavailable, operation, err)
}

func (s *JSONLStore) canonicalHistoryOrderKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", ErrHistoryOrderUnavailable
	}
	if shouldShortCircuitSessionResolve(key) && s.sessionExists(key) {
		meta, err := s.GetSessionMeta(context.Background(), key)
		if err != nil {
			return "", historyOwnershipReadError("canonical metadata", err)
		}
		if meta.Key == key {
			return key, nil
		}
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return "", fmt.Errorf("%w: canonical directory: %w", ErrHistoryOwnershipUnavailable, err)
	}
	canonical := ""
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		sanitized := strings.TrimSuffix(entry.Name(), ".meta.json")
		lock := s.sessionLock(sanitized)
		lock.Lock()
		meta, readErr := s.readMeta(sanitized)
		lock.Unlock()
		if readErr != nil {
			// Silently skipping an unreadable alias owner could mint a valid
			// receipt in a different session. Ordinary listings may skip it;
			// canonical order resolution must fail closed instead.
			return "", historyOwnershipReadError("alias ownership", readErr)
		}
		for _, alias := range meta.Aliases {
			if alias != key || meta.Key == key {
				continue
			}
			if canonical != "" && canonical != meta.Key {
				return "", ErrHistoryOrderUnavailable
			}
			canonical = meta.Key
		}
	}
	if canonical == "" {
		canonical = key
	}
	return canonical, nil
}

// AppendHistoryReceipt returns only the exact durable message from this append.
// A filtered transient thought returns a zero receipt, never an ordering proof.
func (s *JSONLStore) AppendHistoryReceipt(
	ctx context.Context,
	key string,
	input providers.Message,
) (CommittedHistoryMessage, error) {
	canonical, err := s.canonicalHistoryOrderKey(key)
	if err != nil {
		return CommittedHistoryMessage{}, err
	}
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return CommittedHistoryMessage{}, ctx.Err()
	}
	meta, err := s.readMeta(canonical)
	if err != nil {
		return CommittedHistoryMessage{}, err
	}
	if ownershipErr := validateCanonicalHistoryKey(key, canonical, meta); ownershipErr != nil {
		return CommittedHistoryMessage{}, ownershipErr
	}
	if messageutil.IsTransientAssistantThoughtMessage(input) {
		return CommittedHistoryMessage{}, nil
	}
	if markerErr := s.probeHistoryRecoveryMarker(canonical, &meta); markerErr != nil {
		return CommittedHistoryMessage{}, markerErr
	}
	if witnessErr := s.probeHistoryOrderWitness(canonical, &meta); witnessErr != nil {
		return CommittedHistoryMessage{}, witnessErr
	}
	now := time.Now()
	message := prepareHistoryMessage(input, now, providers.HistoryRun(ctx))
	if uuid.Validate(input.HistoryID) == nil {
		// Append is a new occurrence, not idempotency or transcript editing.
		// Revisions that retain identity go through SetHistory instead.
		message.HistoryID = newHistoryID()
	}
	if runID := providers.HistoryRun(ctx); runID != "" {
		message.HistoryRunID = runID
	}
	ensureHistoryGeneration(&meta, canonical)
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	receipts, err := s.appendHistoryMessagesWithReceipts(canonical, []providers.Message{message}, meta)
	if err != nil {
		return CommittedHistoryMessage{}, err
	}
	if len(receipts) != 1 {
		return CommittedHistoryMessage{}, ErrHistoryOrderUnavailable
	}
	return receipts[0], nil
}

// ReadHistoryWithReceipts returns transcript order, not sequence order. An edit
// can have a newer sequence than unchanged messages later in the transcript.
func (s *JSONLStore) ReadHistoryWithReceipts(ctx context.Context, key string) ([]CommittedHistoryMessage, error) {
	canonical, err := s.canonicalHistoryOrderKey(key)
	if err != nil {
		return nil, err
	}
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	meta, err := s.readMeta(canonical)
	if err != nil {
		return nil, err
	}
	if ownershipErr := validateCanonicalHistoryKey(key, canonical, meta); ownershipErr != nil {
		return nil, ownershipErr
	}
	if markerErr := s.probeHistoryRecoveryMarker(canonical, &meta); markerErr != nil {
		return nil, markerErr
	}
	if witnessErr := s.probeHistoryOrderWitness(canonical, &meta); witnessErr != nil {
		return nil, witnessErr
	}
	if stateErr := validateHistoryOrderState(meta); stateErr != nil {
		return nil, stateErr
	}
	var messages []providers.Message
	if _, statErr := os.Stat(s.metaPath(canonical)); errors.Is(statErr, os.ErrNotExist) {
		if validateHistoryOrderState(meta) != nil || meta.HistoryOrderVersion != 0 {
			return nil, ErrHistoryOrderUnavailable
		}
		// Old JSONL sessions can predate metadata entirely. Their content is
		// readable, but no stream/fence may be inferred. The unbound decoder
		// still rejects any residual modern proof or origin marker.
		messages, _, err = s.readHistoryOrderOrigins(canonical, SessionMeta{Key: canonical}, 0, false, true)
	} else if statErr != nil {
		return nil, statErr
	} else {
		messages, err = s.readCommittedHistory(canonical, meta, meta.Skip)
	}
	if err != nil {
		return nil, err
	}
	if err := validateHistoryMessageOrders(messages, meta); err != nil {
		return nil, err
	}
	receipts := make([]CommittedHistoryMessage, 0, len(messages))
	for _, message := range messages {
		receipt := CommittedHistoryMessage{Message: message, Order: messageHistoryOrder(message)}
		if receipt.Order == (HistoryOrderProof{}) && meta.HistoryOrderVersion == HistoryOrderVersion {
			receipt.LegacyBeforeStreamID = meta.HistoryStreamID
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func (s *JSONLStore) readCommittedHistory(key string, meta SessionMeta, skip int) ([]providers.Message, error) {
	messages, _, err := s.readCommittedHistoryOrderOrigins(key, meta, skip, true)
	return messages, err
}

func (s *JSONLStore) readCommittedHistoryOrderOrigins(
	key string,
	meta SessionMeta,
	skip int,
	requireLineage bool,
) ([]providers.Message, int64, error) {
	return s.readHistoryOrderOrigins(key, meta, skip, requireLineage, false)
}

func (s *JSONLStore) readHistoryOrderOrigins(
	key string,
	meta SessionMeta,
	skip int,
	requireLineage, unboundLegacy bool,
) ([]providers.Message, int64, error) {
	ensureHistoryGeneration(&meta, key)
	openFile := s.openHistoryFile
	if openFile == nil {
		openFile = os.Open
	}
	file, err := openFile(s.jsonlPath(key))
	if errors.Is(err, os.ErrNotExist) && meta.Count == 0 {
		return []providers.Message{}, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	var reader io.Reader = file
	if meta.HistoryBytes != nil {
		info, statErr := file.Stat()
		if statErr != nil {
			return nil, 0, statErr
		}
		if *meta.HistoryBytes < 0 || *meta.HistoryBytes > info.Size() {
			return nil, 0, ErrHistoryCursor
		}
		reader = io.LimitReader(file, *meta.HistoryBytes)
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	messages := []providers.Message{}
	line := 0
	var byteEnd int64
	for (unboundLegacy || line < meta.Count) && scanner.Scan() {
		data := scanner.Bytes()
		byteEnd += int64(len(data))
		if len(bytes.TrimRight(data, "\r\n")) == 0 {
			continue
		}
		line++
		if line <= skip {
			continue
		}
		fromFence := meta.HistoryFileGeneration == meta.HistoryLegacyFileGeneration &&
			byteEnd <= meta.HistoryLegacyBytes
		var message providers.Message
		if json.Unmarshal(data, &message) != nil {
			if meta.HistoryOrderVersion != 0 && !fromFence {
				return nil, 0, ErrHistoryOrderUnavailable
			}
			continue
		}
		if requireLineage && !historyLineageMatches(data, meta, key) {
			return nil, 0, ErrHistoryCursor
		}
		order, orderErr := decodedHistoryOrder(data)
		if orderErr != nil {
			return nil, 0, orderErr
		}
		legacyStream, legacyErr := decodedLegacyHistoryStream(data)
		if legacyErr != nil {
			return nil, 0, legacyErr
		}
		if order != (HistoryOrderProof{}) {
			if legacyStream != "" || order.StreamID != meta.HistoryStreamID || order.Sequence > meta.HistorySequence {
				return nil, 0, ErrHistoryOrderUnavailable
			}
		} else if meta.HistoryOrderVersion == HistoryOrderVersion {
			if legacyStream != meta.HistoryStreamID && (legacyStream != "" || !fromFence) {
				return nil, 0, ErrHistoryOrderUnavailable
			}
		} else if legacyStream != "" {
			return nil, 0, ErrHistoryOrderUnavailable
		}
		if messageutil.IsTransientAssistantThoughtMessage(message) {
			continue
		}
		message.HistoryID = decodedHistoryID(data, filepath.Base(file.Name()), int64(line-1))
		message.HistoryRunID = decodedHistoryRunID(data)
		messages = append(messages, messageWithHistoryOrder(message, order))
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, fmt.Errorf("read committed history: %w", err)
	}
	if !unboundLegacy && line != meta.Count {
		return nil, 0, ErrHistoryCursor
	}
	return messages, byteEnd, nil
}

// Decode the serialized write buffer so a receipt has exactly the stored
// representation and never aliases pointers/slices owned by the caller.
func committedHistoryMessages(data []byte) ([]CommittedHistoryMessage, error) {
	receipts := []CommittedHistoryMessage{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineSize)
	for scanner.Scan() {
		var record historyRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.Order == nil || !record.Order.Valid() {
			return nil, ErrHistoryOrderUnavailable
		}
		message := messageWithHistoryOrder(record.Message, *record.Order)
		message.HistoryID = record.ID
		message.HistoryRunID = decodedHistoryRunID(scanner.Bytes())
		receipts = append(receipts, CommittedHistoryMessage{Message: message, Order: *record.Order})
	}
	return receipts, scanner.Err()
}

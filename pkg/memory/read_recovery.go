package memory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
)

const (
	maxReadRecoveryBytes   = 8 << 20
	maxReadRecoveryRecords = 1000
)

// ReadOrderRecovery proves the entire original stream, including skipped rows.
// Missing replaced/collected occurrences make this proof unavailable.
type ReadOrderRecovery struct {
	StreamID string
	Frontier uint64
	Digest   string
	Messages []CommittedHistoryMessage
}

type ReadOrderRecoveryRequest struct {
	AttemptID string
	Admit     func(context.Context) error
}

type readRecoveryGate struct {
	AttemptID string `json:"attemptId"`
	Digest    string `json:"digest"`
}

func (s *JSONLStore) readRecoveryPath(key string) string {
	return s.jsonlPath(key) + ".read-recovery"
}

func (s *JSONLStore) requireNoReadRecovery(key string) error {
	_, err := os.Stat(s.readRecoveryPath(key))
	if err == nil {
		return ErrHistoryRecoveryRequired
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// WithReadOrderRecovery keeps its durable gate across lost callbacks/restarts
// until the controller finalizes the database attempt; readers remain available.
func (s *JSONLStore) WithReadOrderRecovery(
	ctx context.Context,
	key string,
	request ReadOrderRecoveryRequest,
	apply func(ReadOrderRecovery) error,
) error {
	attempt := request.AttemptID
	if uuid.Validate(attempt) != nil || attempt == uuid.Nil.String() || request.Admit == nil || apply == nil {
		return ErrHistoryOrderUnavailable
	}
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return err
	}
	releaseCall, claimErr := s.claimReadRecoveryCall(key)
	if claimErr != nil {
		return claimErr
	}
	defer releaseCall()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Admission may call the controller. No session stripe can be held across
	// callbacks: colon-key ownership resolution visits unrelated stripes too.
	if err := request.Admit(ctx); err != nil {
		return err
	}
	proof, err := s.prepareReadOrderRecovery(ctx, key, attempt)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if applyErr := apply(proof); applyErr != nil {
		return applyErr
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	return s.releaseReadOrderRecovery(key, attempt)
}

// A call claim serializes callbacks and explicit releases for one physical
// session without keeping a stripe locked during controller I/O. It exists
// only while a call runs; the durable gate fences writers after interruption.
func (s *JSONLStore) claimReadRecoveryCall(key string) (func(), error) {
	physical := sanitizeKey(key)
	s.readRecoveryMu.Lock()
	defer s.readRecoveryMu.Unlock()
	if s.readRecoveryCalls[physical] {
		return nil, ErrHistoryRecoveryRequired
	}
	if s.readRecoveryCalls == nil {
		s.readRecoveryCalls = make(map[string]bool)
	}
	s.readRecoveryCalls[physical] = true
	return func() {
		s.readRecoveryMu.Lock()
		delete(s.readRecoveryCalls, physical)
		s.readRecoveryMu.Unlock()
	}, nil
}

func (s *JSONLStore) prepareReadOrderRecovery(ctx context.Context, key, attempt string) (ReadOrderRecovery, error) {
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return ReadOrderRecovery{}, err
	}
	proof, err := s.readOrderRecoveryProof(ctx, key)
	if err != nil {
		return ReadOrderRecovery{}, err
	}
	gate := readRecoveryGate{AttemptID: attempt, Digest: proof.Digest}
	data, err := os.ReadFile(s.readRecoveryPath(key))
	switch {
	case err == nil:
		var previous readRecoveryGate
		if json.Unmarshal(data, &previous) != nil || previous != gate {
			return ReadOrderRecovery{}, ErrHistoryRecoveryRequired
		}
	case errors.Is(err, os.ErrNotExist):
		encoded, encodeErr := json.Marshal(gate)
		if encodeErr != nil {
			return ReadOrderRecovery{}, encodeErr
		}
		if writeErr := s.writeSessionFile(s.readRecoveryPath(key), encoded, 0o600); writeErr != nil {
			return ReadOrderRecovery{}, writeErr
		}
	default:
		return ReadOrderRecovery{}, err
	}
	if syncErr := s.syncHistoryFile(s.readRecoveryPath(key)); syncErr != nil {
		return ReadOrderRecovery{}, syncErr
	}
	return proof, nil
}

func (s *JSONLStore) ReleaseReadOrderRecovery(ctx context.Context, key, attempt string) error {
	if uuid.Validate(attempt) != nil {
		return ErrHistoryOrderUnavailable
	}
	releaseCall, claimErr := s.claimReadRecoveryCall(key)
	if claimErr != nil {
		return claimErr
	}
	defer releaseCall()
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.releaseReadOrderRecovery(key, attempt)
}

func (s *JSONLStore) releaseReadOrderRecovery(key, attempt string) error {
	data, err := os.ReadFile(s.readRecoveryPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var gate readRecoveryGate
	if json.Unmarshal(data, &gate) != nil || gate.AttemptID != attempt {
		return ErrHistoryRecoveryRequired
	}
	if removeErr := os.Remove(s.readRecoveryPath(key)); removeErr != nil {
		return removeErr
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s *JSONLStore) readOrderRecoveryProof(ctx context.Context, key string) (ReadOrderRecovery, error) {
	if err := ctx.Err(); err != nil {
		return ReadOrderRecovery{}, err
	}
	meta, rawMeta, err := s.readContentMeta(key)
	if err != nil {
		return ReadOrderRecovery{}, err
	}
	var ownership historyOwnership
	if len(rawMeta) == 0 || json.Unmarshal(rawMeta, &ownership) != nil || ownership.Key != key {
		return ReadOrderRecovery{}, ErrHistoryOwnershipUnavailable
	}
	if cached, ok := s.cachedMeta(sanitizeKey(key)); ok {
		meta.HistoryRecoveryRequired = meta.HistoryRecoveryRequired || cached.HistoryRecoveryRequired
	}
	marked, err := s.historyRecoveryMarkerState(key)
	if err != nil {
		return ReadOrderRecovery{}, err
	}
	meta.HistoryRecoveryRequired = meta.HistoryRecoveryRequired || marked
	if keyErr := validateCanonicalHistoryKey(key, key, meta); keyErr != nil {
		return ReadOrderRecovery{}, keyErr
	}
	if markerErr := s.probeHistoryRecoveryMarker(key, &meta); markerErr != nil {
		return ReadOrderRecovery{}, markerErr
	}
	if witnessErr := s.probeHistoryOrderWitness(key, &meta); witnessErr != nil {
		return ReadOrderRecovery{}, witnessErr
	}
	if stateErr := validateHistoryOrderState(meta); stateErr != nil {
		return ReadOrderRecovery{}, stateErr
	}
	// shortcut: a complete proof must fit the existing 1000-record/8-MiB audit window; use streaming verification for larger sources.
	if meta.HistoryOrderVersion != HistoryOrderVersion || meta.HistorySequence == 0 ||
		meta.Count > maxReadRecoveryRecords ||
		meta.HistoryBytes == nil ||
		*meta.HistoryBytes > maxReadRecoveryBytes {
		return ReadOrderRecovery{}, ErrHistoryOrderUnavailable
	}
	messages, committedEnd, err := s.readCommittedHistoryOrderOrigins(key, meta, 0, true)
	if err != nil {
		return ReadOrderRecovery{}, err
	}
	if committedEnd != *meta.HistoryBytes {
		return ReadOrderRecovery{}, ErrHistoryOrderUnavailable
	}
	if orderErr := validateHistoryMessageOrders(messages, meta); orderErr != nil {
		return ReadOrderRecovery{}, orderErr
	}
	proof := ReadOrderRecovery{
		StreamID: meta.HistoryStreamID,
		Frontier: meta.HistorySequence,
		Messages: make([]CommittedHistoryMessage, 0, len(messages)),
	}
	var count uint64
	for _, message := range messages {
		order := messageHistoryOrder(message)
		if order.Valid() {
			count++
			proof.Messages = append(proof.Messages, CommittedHistoryMessage{Message: message, Order: order})
		}
	}
	if count != proof.Frontier || len(proof.Messages) != meta.Count {
		return ReadOrderRecovery{}, ErrHistoryOrderUnavailable
	}
	file, err := os.Open(s.jsonlPath(key))
	if err != nil {
		return ReadOrderRecovery{}, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxReadRecoveryBytes+1))
	if readCloseErr := errors.Join(readErr, file.Close()); readCloseErr != nil {
		return ReadOrderRecovery{}, readCloseErr
	}
	if int64(len(raw)) != *meta.HistoryBytes {
		return ReadOrderRecovery{}, ErrHistoryOrderUnavailable
	}
	encoded, err := json.Marshal(struct {
		Meta      SessionMeta               `json:"Meta"`
		Messages  []CommittedHistoryMessage `json:"Messages"`
		RawDigest [sha256.Size]byte         `json:"RawDigest"`
	}{meta, proof.Messages, sha256.Sum256(raw)})
	if err != nil {
		return ReadOrderRecovery{}, err
	}
	proof.Digest = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return proof, nil
}

func (s *JSONLStore) ReadOrderRecoveryActive(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, err := os.Stat(s.readRecoveryPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

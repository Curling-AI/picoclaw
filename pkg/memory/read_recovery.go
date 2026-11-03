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

const maxReadRecoveryBytes = 8 << 20
const maxReadRecoveryRecords = 1000

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

// The durable gate survives a lost callback/restart. Only the controller which
// has finalized the database attempt may release it; readers remain available.
func (s *JSONLStore) WithReadOrderRecovery(ctx context.Context, key string, request ReadOrderRecoveryRequest, apply func(ReadOrderRecovery) error) error {
	attempt := request.AttemptID
	if uuid.Validate(attempt) != nil || attempt == uuid.Nil.String() || request.Admit == nil || apply == nil {
		return ErrHistoryOrderUnavailable
	}
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return err
	}
	canonical := key
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := request.Admit(ctx); err != nil {
		return err
	}
	proof, err := s.readOrderRecoveryProof(ctx, canonical)
	if err != nil {
		return err
	}
	gate := readRecoveryGate{AttemptID: attempt, Digest: proof.Digest}
	data, err := os.ReadFile(s.readRecoveryPath(canonical))
	switch {
	case err == nil:
		var previous readRecoveryGate
		if json.Unmarshal(data, &previous) != nil || previous != gate {
			return ErrHistoryRecoveryRequired
		}
	case errors.Is(err, os.ErrNotExist):
		encoded, err := json.Marshal(gate)
		if err != nil {
			return err
		}
		if err := s.writeSessionFile(s.readRecoveryPath(canonical), encoded, 0o600); err != nil {
			return err
		}
	default:
		return err
	}
	if err := s.syncHistoryFile(s.readRecoveryPath(canonical)); err != nil {
		return err
	}
	if err := apply(proof); err != nil {
		return err
	}
	return s.releaseReadOrderRecovery(canonical, attempt)
}

func (s *JSONLStore) ReleaseReadOrderRecovery(ctx context.Context, key, attempt string) error {
	if uuid.Validate(attempt) != nil {
		return ErrHistoryOrderUnavailable
	}
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
	if err := os.Remove(s.readRecoveryPath(key)); err != nil {
		return err
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
	if err := validateCanonicalHistoryKey(key, key, meta); err != nil {
		return ReadOrderRecovery{}, err
	}
	if err := s.probeHistoryRecoveryMarker(key, &meta); err != nil {
		return ReadOrderRecovery{}, err
	}
	if err := s.probeHistoryOrderWitness(key, &meta); err != nil {
		return ReadOrderRecovery{}, err
	}
	if err := validateHistoryOrderState(meta); err != nil {
		return ReadOrderRecovery{}, err
	}
	// shortcut: a complete proof must fit the existing 1000-record/8-MiB audit window; use streaming verification for larger sources.
	if meta.HistoryOrderVersion != HistoryOrderVersion || meta.HistorySequence == 0 || meta.Count > maxReadRecoveryRecords || meta.HistoryBytes == nil || *meta.HistoryBytes > maxReadRecoveryBytes {
		return ReadOrderRecovery{}, ErrHistoryOrderUnavailable
	}
	messages, committedEnd, err := s.readCommittedHistoryOrderOrigins(key, meta, 0, true)
	if err != nil {
		return ReadOrderRecovery{}, err
	}
	if committedEnd != *meta.HistoryBytes {
		return ReadOrderRecovery{}, ErrHistoryOrderUnavailable
	}
	if err := validateHistoryMessageOrders(messages, meta); err != nil {
		return ReadOrderRecovery{}, err
	}
	proof := ReadOrderRecovery{StreamID: meta.HistoryStreamID, Frontier: meta.HistorySequence, Messages: make([]CommittedHistoryMessage, 0, len(messages))}
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
	if err := errors.Join(readErr, file.Close()); err != nil {
		return ReadOrderRecovery{}, err
	}
	if int64(len(raw)) != *meta.HistoryBytes {
		return ReadOrderRecovery{}, ErrHistoryOrderUnavailable
	}
	encoded, err := json.Marshal(struct {
		Meta      SessionMeta
		Messages  []CommittedHistoryMessage
		RawDigest [sha256.Size]byte
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

package memory

import (
	"encoding/json"
	"errors"
	"os"
)

const historyOrderWitnessVersion = uint8(1)

// The witness survives writers that discard unknown JSONL/meta fields. Its
// immutable stream/origin identity does not duplicate the per-commit counter.
type historyOrderWitness struct {
	Version              uint8  `json:"version"`
	StreamID             string `json:"stream_id"`
	LegacyFileGeneration string `json:"legacy_file_generation"`
	LegacyBytes          int64  `json:"legacy_bytes"`
}

type historyOrderWitnessState struct {
	witness         historyOrderWitness
	present, synced bool
	invalid         bool
}

func (s *JSONLStore) historyOrderWitnessPath(key string) string {
	return s.jsonlPath(key) + ".order-witness"
}

func witnessForHistory(meta SessionMeta) historyOrderWitness {
	return historyOrderWitness{
		Version: historyOrderWitnessVersion, StreamID: meta.HistoryStreamID,
		LegacyFileGeneration: meta.HistoryLegacyFileGeneration, LegacyBytes: meta.HistoryLegacyBytes,
	}
}

func validHistoryOrderWitness(witness historyOrderWitness) bool {
	return witness.Version == historyOrderWitnessVersion && validHistoryStreamID(witness.StreamID) &&
		validHistoryStreamID(witness.LegacyFileGeneration) && witness.LegacyBytes >= 0
}

func (s *JSONLStore) cacheHistoryOrderWitness(key string, state historyOrderWitnessState) {
	s.metaMu.Lock()
	if s.historyWitnesses == nil {
		s.historyWitnesses = make(map[string]historyOrderWitnessState)
	}
	s.historyWitnesses[sanitizeKey(key)] = state
	s.metaMu.Unlock()
}

func (s *JSONLStore) readHistoryOrderWitness(key string, probe bool) (historyOrderWitnessState, error) {
	s.metaMu.RLock()
	previous, cached := s.historyWitnesses[sanitizeKey(key)]
	s.metaMu.RUnlock()
	if cached && !probe {
		return previous, nil
	}
	data, err := os.ReadFile(s.historyOrderWitnessPath(key))
	state := historyOrderWitnessState{}
	if errors.Is(err, os.ErrNotExist) {
		s.cacheHistoryOrderWitness(key, state)
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.present = true
	state.invalid = json.Unmarshal(data, &state.witness) != nil || !validHistoryOrderWitness(state.witness)
	state.synced = !state.invalid
	if cached && previous.present && !previous.synced && previous.witness == state.witness {
		state.synced = false
	}
	s.cacheHistoryOrderWitness(key, state)
	return state, nil
}

func historyWitnessMatchesMeta(state historyOrderWitnessState, meta SessionMeta) bool {
	if state.invalid || (state.present && !state.synced) ||
		meta.HistoryOrderWitnessVersion > historyOrderWitnessVersion {
		return false
	}
	if !state.present {
		return meta.HistoryOrderVersion == 0 && validateHistoryOrderState(meta) == nil
	}
	return validateHistoryOrderState(meta) == nil && meta.HistoryOrderVersion == HistoryOrderVersion &&
		state.witness == witnessForHistory(meta)
}

// Content reads do not require writing a recovery marker. A subsequent mutation
// must durably latch the requirement before changing data; proof APIs reject it.
func (s *JSONLStore) applyHistoryOrderWitness(key string, meta *SessionMeta) error {
	if meta.HistoryRecoveryRequired {
		return nil
	}
	state, err := s.readHistoryOrderWitness(key, false)
	if err != nil {
		return err
	}
	if !historyWitnessMatchesMeta(state, *meta) {
		meta.HistoryRecoveryRequired = true
	}
	return nil
}

func (s *JSONLStore) probeHistoryOrderWitness(key string, meta *SessionMeta) error {
	if meta.HistoryRecoveryRequired {
		return ErrHistoryReceiptUnavailable
	}
	state, err := s.readHistoryOrderWitness(key, true)
	if err != nil {
		return err
	}
	if !historyWitnessMatchesMeta(state, *meta) {
		meta.HistoryRecoveryRequired = true
		return errors.Join(ErrHistoryReceiptUnavailable, s.markHistoryRecoveryRequired(key, meta))
	}
	return nil
}

// Called only after the owning OLD/NEW journal exists. Normal commits reuse a
// synced immutable witness from the write-through cache without extra I/O.
func (s *JSONLStore) ensureDurableHistoryOrderWitness(key string, recovery historyWriteRecovery) error {
	next := recovery.Next
	if next.HistoryRecoveryRequired || next.HistoryOrderVersion == 0 {
		return nil
	}
	if next.HistoryOrderWitnessVersion != historyOrderWitnessVersion || validateHistoryOrderState(next) != nil {
		return ErrHistoryOrderUnavailable
	}
	expected := witnessForHistory(next)
	state, err := s.readHistoryOrderWitness(key, false)
	if err != nil || state.invalid || (state.present && state.witness != expected) {
		return errors.Join(ErrHistoryReceiptUnavailable, err)
	}
	// Reuse a cold witness only when OLD records its durable activation.
	// An interrupted first commit may leave readable bytes without a completed
	// sync; finish that phase before publishing the new stream.
	if state.present && state.synced && recovery.Meta.HistoryOrderWitnessVersion == historyOrderWitnessVersion {
		return nil
	}
	if !state.present && recovery.Meta.HistoryOrderWitnessVersion != 0 {
		return ErrHistoryReceiptUnavailable
	}
	if err := s.syncHistoryFile(s.historyRecoveryPath(key)); err != nil {
		return err
	}
	alreadyPresent := state.present
	state = historyOrderWitnessState{witness: expected, present: true}
	s.cacheHistoryOrderWitness(key, state)
	if !alreadyPresent {
		data, encodeErr := json.Marshal(expected)
		if encodeErr != nil {
			return encodeErr
		}
		if writeErr := s.writeSessionFile(s.historyOrderWitnessPath(key), append(data, '\n'), 0o600); writeErr != nil {
			return writeErr
		}
	}
	if syncErr := s.syncHistoryFile(s.historyOrderWitnessPath(key)); syncErr != nil {
		return syncErr
	}
	state.synced = true
	s.cacheHistoryOrderWitness(key, state)
	return nil
}

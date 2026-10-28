package memory

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

const contentCheckpointWitnessVersion = uint8(2)

// Bind logical changes even when JSONL bytes did not change, including an ACK
// or a summary-only mutation. Independent order recovery, owner scope, aliases
// and UpdatedAt are not content mutations and do not mint or invalidate proof.
func contentCheckpointDigest(meta SessionMeta) string {
	return contentJSONDigest(struct {
		State *contentIdentityState `json:"state"`
		Count int                   `json:"count"`
		Skip  int                   `json:"skip"`
		Bytes *int64                `json:"bytes"`
	}{meta.ContentIdentity, meta.Count, meta.Skip, meta.HistoryBytes})
}

// This is a separate, constant-size commitment, not an order reconstruction.
// Keeping it outside the metadata detects a lost highwater even after the
// events bearing larger sequences have been intentionally discarded.
func contentOrderCheckpointDigest(meta SessionMeta) string {
	return contentJSONDigest(struct {
		Version          uint8  `json:"version"`
		Stream           string `json:"stream"`
		Sequence         uint64 `json:"sequence"`
		LegacyGeneration string `json:"legacyGeneration"`
		LegacyBytes      int64  `json:"legacyBytes"`
		WitnessVersion   uint8  `json:"witnessVersion"`
	}{
		meta.HistoryOrderVersion, meta.HistoryStreamID, meta.HistorySequence,
		meta.HistoryLegacyFileGeneration, meta.HistoryLegacyBytes, meta.HistoryOrderWitnessVersion,
	})
}

func (s *JSONLStore) contentOrderCheckpointAvailable(key string, meta SessionMeta) (bool, error) {
	witness, present, err := s.readContentDomain(key)
	if errors.Is(err, ErrContentIdentityUnavailable) {
		// An unreadable format cannot attest a protected counter. This is a
		// pure proof result; actual filesystem errors remain errors below.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	protected := meta.ContentIdentity != nil && meta.ContentIdentity.MutationVersion != 0
	if !present {
		return !protected, nil
	}
	if witness.Version == contentCheckpointWitnessVersion {
		return witness.OrderCheckpoint != "" && witness.OrderCheckpoint == contentOrderCheckpointDigest(meta), nil
	}
	return !protected, nil
}

func contentWitnessMatches(witness contentDomainWitness, meta SessionMeta) bool {
	state := meta.ContentIdentity
	if state == nil || state.Unavailable || witness.DomainID != state.DomainID {
		return false
	}
	if state.MutationVersion == 0 {
		return witness.Version == ContentLineageVersion && witness.Checkpoint == ""
	}
	return state.MutationVersion == contentMutationVersion && witness.Version == contentCheckpointWitnessVersion &&
		witness.Checkpoint == contentCheckpointDigest(meta)
}

func (s *JSONLStore) writeContentCheckpoint(key string, meta SessionMeta) error {
	state := meta.ContentIdentity
	if state == nil || state.Unavailable || !validContentMutationState(state, key) {
		return ErrContentIdentityUnavailable
	}
	witness := contentDomainWitness{Version: ContentLineageVersion, Key: key, DomainID: state.DomainID}
	if state.MutationVersion != 0 {
		witness.Version = contentCheckpointWitnessVersion
		witness.Checkpoint = contentCheckpointDigest(meta)
		marked, markerErr := s.hasHistoryRecoveryMarker(key)
		if markerErr != nil {
			return markerErr
		}
		if !marked && !meta.HistoryRecoveryRequired {
			witness.OrderCheckpoint = contentOrderCheckpointDigest(meta)
		} else {
			// Preserve the last genuinely committed value, if one exists; an
			// unavailable counter must never be reattested from current meta.
			previous, present, readErr := s.readContentDomain(key)
			if readErr != nil && !errors.Is(readErr, ErrContentIdentityUnavailable) {
				return readErr
			}
			if readErr == nil && present && previous.Version == contentCheckpointWitnessVersion {
				witness.OrderCheckpoint = previous.OrderCheckpoint
			}
		}
	}
	data, err := json.Marshal(witness)
	if err != nil {
		return err
	}
	if s.writeFile != nil {
		if err = s.writeSessionFile(s.contentDomainPath(key), data, 0o600); err != nil {
			return err
		}
		return s.syncHistoryFile(s.contentDomainPath(key))
	}
	temporary := ".content-checkpoint-new." + uuid.NewString()
	file, dir, err := openContentArtifact(s.dir, temporary, true)
	if err != nil {
		return err
	}
	defer closeContentArtifact(file, dir)
	written, err := file.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Close()
	}
	if err != nil {
		return err
	}
	// There is no direct-write fallback for a semantic checkpoint. An
	// unsupported filesystem or an unsafe target must leave the journal fenced.
	return replaceContentArtifact(dir, temporary, filepath.Base(s.contentDomainPath(key)))
}

// Matching legible bytes are not a completed durability phase. An owning NEW
// journal must retry both syncs before it can retire its recovery evidence.
func (s *JSONLStore) syncContentCheckpoint(key string) error {
	path := s.contentDomainPath(key)
	return s.syncOwnedContentArtifact(path, s.contentCheckpointSyncFailure)
}

func (s *JSONLStore) syncHistoryMetadata(key string) error {
	return s.syncOwnedContentArtifact(s.metaPath(key), s.historyMetadataSyncFailure)
}

func (s *JSONLStore) syncOwnedContentArtifact(path string, fail func(string, string) error) error {
	file, dir, err := openContentArtifact(s.dir, filepath.Base(path), false)
	if err != nil {
		return err
	}
	defer closeContentArtifact(file, dir)
	if fail != nil {
		if err = fail(path, "file"); err != nil {
			return err
		}
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if fail != nil {
		if err = fail(path, "directory"); err != nil {
			return err
		}
	}
	return dir.Sync()
}

// A real checkpoint witness that names neither side of this journal is not
// permission to adopt stale metadata. Proof-lost content remains writable; no
// next known checkpoint can be synthesized from that path.
func (s *JSONLStore) checkContentRecoveryCheckpoint(key string, before, next SessionMeta) error {
	if (before.ContentIdentity != nil && before.ContentIdentity.Unavailable) ||
		(next.ContentIdentity != nil && next.ContentIdentity.Unavailable) {
		return nil
	}
	witness, present, err := s.readContentDomain(key)
	if err != nil && !errors.Is(err, ErrContentIdentityUnavailable) {
		return err
	}
	if err == nil && present && witness.Version == contentCheckpointWitnessVersion &&
		!contentWitnessMatches(witness, before) && !contentWitnessMatches(witness, next) {
		return ErrContentIdentityUnavailable
	}
	return nil
}

// Only the authoritative OLD decision can restore its prior semantic fence.
// The same journal restores both raw bytes and metadata before this call.
func (s *JSONLStore) restoreContentCheckpoint(key string, before, next SessionMeta) error {
	if before.ContentIdentity != nil {
		if before.ContentIdentity.Unavailable {
			return nil
		}
		if before.ContentIdentity.MutationVersion != 0 ||
			(next.ContentIdentity != nil && next.ContentIdentity.MutationVersion != 0) {
			return s.writeContentCheckpoint(key, before)
		}
		return nil
	}
	if next.ContentIdentity == nil || next.ContentIdentity.MutationVersion == 0 {
		return nil
	}
	witness, present, err := s.readContentDomain(key)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if !contentWitnessMatches(witness, next) {
		return ErrContentIdentityUnavailable
	}
	if err = os.Remove(s.contentDomainPath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

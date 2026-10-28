package memory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"
)

func contentIndexGenesis(file contentArtifactDescriptor) string {
	return contentJSONDigest([]any{contentIndexAlgorithm, file.Key, file.DomainID, file.Generation, file.ID})
}

func contentIndexStep(previous string, line []byte) (string, error) {
	prefix, err := hex.DecodeString(previous)
	if err != nil || len(prefix) != sha256.Size {
		return "", ErrContentIdentityUnavailable
	}
	hash := sha256.New()
	_, _ = hash.Write(prefix)
	_, _ = hash.Write([]byte("\x00picoclaw-content-row-v1\x00"))
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(line)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write(line)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func marshalContentIndexRows(rows []contentLocator, root string) ([]byte, string, error) {
	var data bytes.Buffer
	for _, row := range rows {
		line, err := json.Marshal(row)
		if err != nil {
			return nil, "", err
		}
		line = append(line, '\n')
		root, err = contentIndexStep(root, line)
		if err != nil {
			return nil, "", err
		}
		_, _ = data.Write(line)
	}
	return data.Bytes(), root, nil
}

func newContentArtifact(key, domain, generation, kind string) contentArtifactDescriptor {
	return contentArtifactDescriptor{
		Version:    1,
		Key:        key,
		DomainID:   domain,
		Generation: generation,
		ID:         uuid.NewString(),
		Kind:       kind,
	}
}

func (s *JSONLStore) writeContentArtifact(file contentArtifactDescriptor, data []byte) error {
	if err := s.failContentStorage(file.name(), "create"); err != nil {
		return err
	}
	fd, dir, err := openContentArtifact(s.dir, file.name(), true)
	if err != nil {
		return err
	}
	defer closeContentArtifact(fd, dir)
	if err = s.failContentStorage(file.name(), "write"); err != nil {
		return err
	}
	written, err := fd.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = s.failContentStorage(file.name(), "file-sync")
	}
	if err == nil {
		err = fd.Sync()
	}
	if err == nil {
		err = fd.Close()
	}
	if err == nil {
		err = s.failContentStorage(file.name(), "directory-sync")
	}
	if err == nil {
		err = dir.Sync()
	}
	return err
}

func (s *JSONLStore) failContentStorage(name, phase string) error {
	if s.contentStorageFailure != nil {
		return s.contentStorageFailure(name, phase)
	}
	return nil
}

func (s *JSONLStore) writeContentBlob(
	key, domain, generation, kind string,
	value any,
) (contentArtifactDescriptor, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return contentArtifactDescriptor{}, err
	}
	file := newContentArtifact(key, domain, generation, kind)
	file.Bytes = int64(len(data))
	file.Digest = contentDigest(data)
	return file, s.writeContentArtifact(file, data)
}

func (s *JSONLStore) writeContentIndex(
	key, domain, generation string,
	rows []contentLocator,
) (contentIndexDescriptor, error) {
	file := newContentArtifact(key, domain, generation, "rows")
	data, root, err := marshalContentIndexRows(rows, contentIndexGenesis(file))
	if err != nil {
		return contentIndexDescriptor{}, err
	}
	file.Bytes, file.Digest = int64(len(data)), root
	index := contentIndexDescriptor{File: file, Algorithm: contentIndexAlgorithm, Records: len(rows), Count: len(rows)}
	return index, s.writeContentArtifact(file, data)
}

func (s *JSONLStore) readContentBlob(ctx context.Context, file contentArtifactDescriptor, value any) error {
	if err := s.failContentStorage(file.name(), "read"); err != nil {
		return err
	}
	fd, dir, err := openContentArtifact(s.dir, file.name(), false)
	if errors.Is(err, os.ErrNotExist) {
		return ErrContentIdentityUnavailable
	}
	if err != nil {
		return err
	}
	defer closeContentArtifact(fd, dir)
	info, err := fd.Stat()
	if err != nil {
		return err
	}
	if info.Size() != file.Bytes {
		return ErrContentIdentityUnavailable
	}
	hash := sha256.New()
	reader := io.TeeReader(io.LimitReader(fd, file.Bytes+1), hash)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(value); err != nil {
		return classifyContentStorageDecode(err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return classifyContentStorageDecode(err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != file.Digest {
		return ErrContentIdentityUnavailable
	}
	return nil
}

func classifyContentStorageDecode(err error) error {
	if err == nil {
		return ErrContentIdentityUnavailable
	}
	var path *os.PathError
	if errors.As(err, &path) {
		return err
	}
	if wrapped, ok := err.(interface{ Unwrap() []error }); ok {
		causes := wrapped.Unwrap()
		if len(causes) == 0 {
			return err
		}
		for _, cause := range causes {
			if classifyContentStorageDecode(cause) != ErrContentIdentityUnavailable {
				return err
			}
		}
		return ErrContentIdentityUnavailable
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if classifyContentStorageDecode(wrapped.Unwrap()) == ErrContentIdentityUnavailable {
			return ErrContentIdentityUnavailable
		}
		return err
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF || err == ErrHistoryCursor || err == bufio.ErrTooLong {
		return ErrContentIdentityUnavailable
	}
	var syntax *json.SyntaxError
	var value *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &value) {
		return ErrContentIdentityUnavailable
	}
	// DisallowUnknownFields is an unsupported schema, not recoverable I/O.
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		return ErrContentIdentityUnavailable
	}
	return err
}

func (s *JSONLStore) readContentIndex(ctx context.Context, index contentIndexDescriptor) ([]contentLocator, error) {
	if index.Records < 0 || index.File.Bytes < 0 || int64(index.Records) > index.File.Bytes ||
		index.Start < 0 || index.Count < 0 || index.Start > index.Records || index.Count > index.Records-index.Start {
		return nil, ErrContentIdentityUnavailable
	}
	if err := s.failContentStorage(index.File.name(), "read"); err != nil {
		return nil, err
	}
	fd, dir, err := openContentArtifact(s.dir, index.File.name(), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrContentIdentityUnavailable
	}
	if err != nil {
		return nil, err
	}
	defer closeContentArtifact(fd, dir)
	info, err := fd.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() != index.File.Bytes {
		return nil, ErrContentIdentityUnavailable
	}
	scanner := bufio.NewScanner(io.LimitReader(fd, index.File.Bytes+1))
	scanner.Buffer(make([]byte, 0, 4096), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	root := contentIndexGenesis(index.File)
	// Count is untrusted until the full stream's digest/extent is checked.
	// Grow only from actual decoded records, never allocate its claimed size.
	rows := make([]contentLocator, 0)
	count := 0
	var size int64
	for scanner.Scan() {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		line := scanner.Bytes()
		size += int64(len(line))
		root, err = contentIndexStep(root, line)
		if err != nil {
			return nil, err
		}
		var row contentLocator
		if decodeContentWire(line, &row) != nil || !row.Ref.Valid() || row.Ref.DomainID != index.File.DomainID ||
			row.Line <= 0 ||
			!validHistoryDigest(row.Digest) {
			return nil, ErrContentIdentityUnavailable
		}
		if count >= index.Start && count-index.Start < index.Count {
			rows = append(rows, row)
		}
		count++
	}
	if err = scanner.Err(); err != nil {
		return nil, classifyContentStorageDecode(err)
	}
	if root != index.File.Digest || count != index.Records || size != index.File.Bytes || len(rows) != index.Count {
		return nil, ErrContentIdentityUnavailable
	}
	return rows, nil
}

func (s *JSONLStore) verifyContentArtifact(ctx context.Context, artifact contentArtifactDescriptor) error {
	if !validContentArtifact(artifact, artifact.Key, artifact.DomainID, artifact.Generation, artifact.Kind) ||
		(artifact.Kind != "rows" && artifact.Kind != "pending" && artifact.Kind != "last") {
		return ErrContentIdentityUnavailable
	}
	file, dir, err := openContentArtifact(s.dir, artifact.name(), false)
	if err != nil {
		return err
	}
	defer closeContentArtifact(file, dir)
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != artifact.Bytes {
		return ErrContentIdentityUnavailable
	}
	if artifact.Kind != "rows" {
		hash := sha256.New()
		if _, err = io.Copy(hash, io.LimitReader(file, artifact.Bytes+1)); err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != artifact.Digest {
			return ErrContentIdentityUnavailable
		}
		return ctx.Err()
	}
	scanner := bufio.NewScanner(io.LimitReader(file, artifact.Bytes+1))
	scanner.Buffer(make([]byte, 0, 4096), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	root := contentIndexGenesis(artifact)
	var size int64
	for scanner.Scan() {
		if err = ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		size += int64(len(line))
		root, err = contentIndexStep(root, line)
		if err != nil {
			return err
		}
	}
	if err = scanner.Err(); err != nil {
		return classifyContentStorageDecode(err)
	}
	if root != artifact.Digest || size != artifact.Bytes {
		return ErrContentIdentityUnavailable
	}
	return nil
}

// A strict caller explicitly hydrates; ordinary cache/append keeps capsules.
// Metadata replay may request slots alone and never touches an old raw/index.
func (s *JSONLStore) hydrateContentStorage(ctx context.Context, key string, meta *SessionMeta, rows, slots bool) error {
	state := meta.ContentIdentity
	if state == nil || state.storage == nil {
		return nil
	}
	if !validContentStorageHeader(state, key) {
		return ErrContentIdentityUnavailable
	}
	storage := state.storage
	if slots && !storage.SlotsLoaded {
		h := storage.Header
		if h.Pending != nil {
			var pending contentPending
			if err := s.readContentBlob(ctx, h.Pending.File, &pending); err != nil {
				return err
			}
			if !contentSlotMatches(state, h.Pending, &pending, nil) {
				return ErrContentIdentityUnavailable
			}
			if pending.Mutation != nil &&
				contentJSONDigest(pending.Mutation.Before.Image) != contentJSONDigest(h.BeforeImage) {
				return ErrContentIdentityUnavailable
			}
			state.Pending = &pending
		}
		if h.Last != nil {
			var last contentAcknowledged
			if err := s.readContentBlob(ctx, h.Last.File, &last); err != nil {
				return err
			}
			if !contentSlotMatches(state, h.Last, nil, &last) {
				return ErrContentIdentityUnavailable
			}
			if last.Mutation != nil && contentJSONDigest(last.Mutation.Retired) != contentJSONDigest(h.RetiredImage) {
				return ErrContentIdentityUnavailable
			}
			state.Last = &last
		}
		storage.SlotsLoaded = true
	}
	if rows && !storage.RowsLoaded {
		loaded, err := s.readContentIndex(ctx, storage.Header.Index)
		if err != nil {
			return err
		}
		state.Rows = loaded
		storage.RowsLoaded = true
	}
	return nil
}

func contentSlotMatches(
	state *contentIdentityState,
	slot *contentSlotDescriptor,
	pending *contentPending,
	last *contentAcknowledged,
) bool {
	copyState := *state
	copyState.storage = nil
	copyState.Pending = pending
	copyState.Last = last
	if !validContentMutationState(&copyState, slot.File.Key) {
		return false
	}
	actual := contentSlotCapsule(slot.File, pending, last)
	return contentJSONDigest(actual) == contentJSONDigest(slot)
}

func contentSlotCapsule(
	file contentArtifactDescriptor,
	pending *contentPending,
	last *contentAcknowledged,
) *contentSlotDescriptor {
	slot := &contentSlotDescriptor{File: file}
	if pending != nil {
		if m := pending.Mutation; m != nil {
			t := m.Transition
			slot.Typed = true
			slot.Kind = t.Kind
			slot.MutationID = t.MutationID
			slot.Intent = t.IntentDigest
			slot.PlanDigest = m.PlanDigest
			slot.BeforeState = t.BeforeState
			slot.AfterState = t.AfterState
			slot.ExpiresAt = t.ExpiresAt
		} else {
			t := pending.Transition
			slot.MutationID = t.MutationID
			slot.PlanDigest = pending.PlanDigest
			slot.BeforeState = t.BeforeState
			slot.AfterState = t.AfterState
			slot.ExpiresAt = t.ExpiresAt
		}
	} else if last != nil {
		if m := last.Mutation; m != nil {
			t := m.Transition
			slot.Typed = true
			slot.Kind = t.Kind
			slot.MutationID = t.MutationID
			slot.Intent = t.IntentDigest
			slot.PlanDigest = m.PlanDigest
			slot.BeforeState = t.BeforeState
			slot.AfterState = t.AfterState
			slot.ExpiresAt = t.ExpiresAt
			slot.AckDigest = m.AckDigest
			slot.CommitID = m.Ack.AppliedCommitID
		} else {
			t := last.Transition
			slot.MutationID = t.MutationID
			slot.PlanDigest = last.PlanDigest
			slot.BeforeState = t.BeforeState
			slot.AfterState = t.AfterState
			slot.ExpiresAt = t.ExpiresAt
			slot.AckDigest = last.AckDigest
		}
	}
	return slot
}

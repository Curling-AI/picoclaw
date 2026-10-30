package memory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"

	"github.com/google/uuid"
)

// Only this journal authorizes an index extent change. The delta contains new
// records, never a second copy of the existing vector or a claimed prefix proof.
type contentIndexAppend struct {
	Before contentIndexDescriptor `json:"before"`
	After  contentIndexDescriptor `json:"after"`
	Data   []byte                 `json:"data"`
}

type contentStorageWrite struct {
	Append  *contentIndexAppend         `json:"append,omitempty"`
	Created []contentArtifactDescriptor `json:"created,omitempty"`
	Retire  []contentArtifactDescriptor `json:"retire,omitempty"`
}

func (s *JSONLStore) prepareContentStorage(
	key string,
	before SessionMeta,
	next *SessionMeta,
	appendInput bool,
) (*contentStorageWrite, error) {
	state := next.ContentIdentity
	if state == nil || state.Unavailable || len(state.opaque) != 0 {
		return nil, nil
	}
	write := new(contentStorageWrite)
	if state.storage != nil && state.storage.InlineTarget {
		// Explicit conversion is the sole path which can lower the disk floor.
		// The caller already qualified every expanded row and retained slot.
		write.Retire = contentStorageRoots(before.ContentIdentity)
		state.storage = nil
		return write, nil
	}
	if state.storage == nil {
		// A surviving disk2 floor forbids treating a rolled-back inline header
		// as a new migration. Ordinary content is kept, without new identities.
		floor, present, err := s.readContentDomain(key)
		if err != nil && !onlyContentIdentityUnavailable(err) {
			return nil, err
		}
		explicitEmptyDomain := err == nil && present && before.ContentIdentity != nil &&
			state.DomainID != before.ContentIdentity.DomainID && len(state.Rows) == 0 && state.Pending == nil &&
			contentWitnessMatches(floor, before)
		if err != nil || (present && floor.DiskVersion != 0 && !explicitEmptyDomain) {
			if appendInput {
				state.Unavailable = true
				return nil, nil
			}
			return nil, ErrContentIdentityUnavailable
		}
		state.storage = &contentStorageState{
			Header:     contentStorageHeader{Version: contentStorageVersion, Key: key, Generation: uuid.NewString()},
			RowsLoaded: true, SlotsLoaded: true,
		}
		index, err := s.writeContentIndex(key, state.DomainID, state.storage.Header.Generation, state.Rows)
		if err != nil {
			return nil, err
		}
		state.storage.Header.Index = index
		write.Created = append(write.Created, index.File)
	} else {
		if !validContentStorageHeader(state, key) {
			return nil, ErrContentIdentityUnavailable
		}
		storage := state.storage
		oldIndex := storage.Header.Index
		if len(storage.Appended) != 0 {
			// A missing or mismatched optional index closes identity before the
			// source write. It does not disable ordinary durable message input.
			if err := s.probeContentIndexExtent(oldIndex); err != nil {
				if appendInput && onlyContentIdentityUnavailable(err) {
					state.Unavailable = true
					storage.Appended = nil
					return nil, nil
				}
				return nil, err
			}
			if oldIndex.Start+oldIndex.Count != oldIndex.Records {
				return nil, ErrContentIdentityUnavailable
			}
			data, root, err := marshalContentIndexRows(storage.Appended, oldIndex.File.Digest)
			if err != nil {
				return nil, err
			}
			nextIndex := oldIndex
			nextIndex.File.Bytes += int64(len(data))
			nextIndex.File.Digest = root
			nextIndex.Records += len(storage.Appended)
			nextIndex.Count += len(storage.Appended)
			write.Append = &contentIndexAppend{Before: oldIndex, After: nextIndex, Data: data}
			storage.Header.Index = nextIndex
			storage.Appended = nil
		} else if storage.RowsLoaded {
			// Destructive/qualified work already pays for a strict read. Reuse
			// the current generation or an exact suffix; never match by body.
			oldRows, err := s.readContentIndex(context.Background(), oldIndex)
			if err != nil {
				return nil, err
			}
			dropped, suffix := contentLocatorSuffix(oldRows, state.Rows)
			if suffix {
				storage.Header.Index.Start += dropped
				storage.Header.Index.Count = len(state.Rows)
			} else {
				index, err := s.writeContentIndex(key, state.DomainID, storage.Header.Generation, state.Rows)
				if err != nil {
					return nil, err
				}
				storage.Header.Index = index
				write.Created = append(write.Created, index.File)
				write.Retire = append(write.Retire, oldIndex.File)
			}
		}
	}
	storage := state.storage
	if storage.SlotsLoaded {
		var err error
		storage.Header.Pending, err = s.prepareContentSlot(
			key,
			state,
			storage.Header.Pending,
			state.Pending,
			nil,
			write,
		)
		if err != nil {
			return nil, err
		}
		storage.Header.Last, err = s.prepareContentSlot(key, state, storage.Header.Last, nil, state.Last, write)
		if err != nil {
			return nil, err
		}
		storage.Header.BeforeImage, storage.Header.RetiredImage = nil, nil
		if state.Pending != nil && state.Pending.Mutation != nil {
			storage.Header.BeforeImage = state.Pending.Mutation.Before.Image
		}
		if state.Last != nil && state.Last.Mutation != nil {
			storage.Header.RetiredImage = state.Last.Mutation.Retired
		}
	}
	if !validContentStorageHeader(state, key) {
		return nil, ErrContentIdentityUnavailable
	}
	return write, nil
}

func contentLocatorSuffix(before, after []contentLocator) (int, bool) {
	if len(after) > len(before) {
		return 0, false
	}
	dropped := len(before) - len(after)
	for i := range after {
		if after[i] != before[dropped+i] {
			return 0, false
		}
	}
	return dropped, true
}

func (s *JSONLStore) prepareContentSlot(
	key string,
	state *contentIdentityState,
	previous *contentSlotDescriptor,
	pending *contentPending,
	last *contentAcknowledged,
	write *contentStorageWrite,
) (*contentSlotDescriptor, error) {
	if pending == nil && last == nil {
		if previous != nil {
			write.Retire = append(write.Retire, previous.File)
		}
		return nil, nil
	}
	if previous != nil {
		candidate := contentSlotCapsule(previous.File, pending, last)
		value := any(pending)
		if last != nil {
			value = last
		}
		if contentJSONDigest(candidate) == contentJSONDigest(previous) &&
			contentJSONDigest(value) == previous.File.Digest {
			return previous, nil
		}
	}
	kind, value := "pending", any(pending)
	if last != nil {
		kind, value = "last", last
	}
	file, err := s.writeContentBlob(key, state.DomainID, state.storage.Header.Generation, kind, value)
	if err != nil {
		return nil, err
	}
	write.Created = append(write.Created, file)
	if previous != nil {
		write.Retire = append(write.Retire, previous.File)
	}
	return contentSlotCapsule(file, pending, last), nil
}

func (s *JSONLStore) probeContentIndexExtent(index contentIndexDescriptor) error {
	file, dir, err := openContentArtifact(s.dir, index.File.name(), false)
	if errors.Is(err, os.ErrNotExist) {
		return ErrContentIdentityUnavailable
	}
	if err != nil {
		return err
	}
	defer closeContentArtifact(file, dir)
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != index.File.Bytes {
		return ErrContentIdentityUnavailable
	}
	return nil
}

func (s *JSONLStore) appendContentIndex(delta *contentIndexAppend) error {
	if delta == nil {
		return nil
	}
	fd, dir, err := openContentArtifactWritable(s.dir, delta.Before.File.name())
	if err != nil {
		return err
	}
	defer closeContentArtifact(fd, dir)
	info, err := fd.Stat()
	if err != nil {
		return err
	}
	if info.Size() != delta.Before.File.Bytes {
		return ErrContentIdentityUnavailable
	}
	if err = s.failContentStorage(delta.Before.File.name(), "append-write"); err != nil {
		return err
	}
	written, err := fd.WriteAt(delta.Data, delta.Before.File.Bytes)
	if err == nil && written != len(delta.Data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = s.failContentStorage(delta.Before.File.name(), "file-sync")
	}
	if err == nil {
		err = fd.Sync()
	}
	if err == nil {
		err = s.failContentStorage(delta.Before.File.name(), "directory-sync")
	}
	if err == nil {
		err = dir.Sync()
	}
	return err
}

func (s *JSONLStore) recoverContentIndexOld(delta *contentIndexAppend) error {
	if delta == nil {
		return nil
	}
	fd, dir, err := openContentArtifactWritable(s.dir, delta.Before.File.name())
	if err != nil {
		return err
	}
	defer closeContentArtifact(fd, dir)
	info, err := fd.Stat()
	if err != nil {
		return err
	}
	if info.Size() < delta.Before.File.Bytes || info.Size() > delta.After.File.Bytes {
		return ErrContentIdentityUnavailable
	}
	tail := make([]byte, info.Size()-delta.Before.File.Bytes)
	if len(tail) != 0 {
		_, err = fd.ReadAt(tail, delta.Before.File.Bytes)
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(tail, delta.Data[:len(tail)]) {
		return ErrContentIdentityUnavailable
	}
	// Only the exact journal delta may be truncated, including a partial write.
	if err = fd.Truncate(delta.Before.File.Bytes); err != nil {
		return err
	}
	return errors.Join(fd.Sync(), dir.Sync())
}

func contentStorageRoots(state *contentIdentityState) []contentArtifactDescriptor {
	if state == nil || state.storage == nil || state.storage.InlineTarget {
		return nil
	}
	h := state.storage.Header
	roots := []contentArtifactDescriptor{h.Index.File}
	if h.Pending != nil {
		roots = append(roots, h.Pending.File)
	}
	if h.Last != nil {
		roots = append(roots, h.Last.File)
	}
	return roots
}

func validContentStorageWrite(key string, recovery historyWriteRecovery) bool {
	write := recovery.Storage
	if write == nil {
		return true
	}
	before, after := recovery.Meta.ContentIdentity, recovery.Next.ContentIdentity
	oldRoots, nextRoots := contentStorageRoots(before), contentStorageRoots(after)
	contains := func(roots []contentArtifactDescriptor, file contentArtifactDescriptor) bool {
		for _, root := range roots {
			if root == file && root.Key == key {
				return true
			}
		}
		return false
	}
	if len(write.Created) > 3 || len(write.Retire) > 3 {
		return false
	}
	for _, file := range write.Created {
		if !contains(nextRoots, file) {
			return false
		}
	}
	for _, file := range write.Retire {
		if !contains(oldRoots, file) {
			return false
		}
	}
	if delta := write.Append; delta != nil {
		if before == nil || after == nil || before.storage == nil || after.storage == nil ||
			!validContentStorageHeader(before, key) || !validContentStorageHeader(after, key) ||
			delta.Before != before.storage.Header.Index || delta.After != after.storage.Header.Index ||
			delta.After.Start != delta.Before.Start ||
			delta.After.Records-delta.Before.Records <= 0 ||
			delta.After.Records-delta.Before.Records != delta.After.Count-delta.Before.Count ||
			delta.After.File.Bytes-delta.Before.File.Bytes != int64(len(delta.Data)) {
			return false
		}
		file := delta.After.File
		file.Bytes, file.Digest = delta.Before.File.Bytes, delta.Before.File.Digest
		if file != delta.Before.File {
			return false
		}
		var rows []contentLocator
		for _, line := range bytes.SplitAfter(delta.Data, []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			if line[len(line)-1] != '\n' {
				return false
			}
			var row contentLocator
			if decodeContentWire(line, &row) != nil || !row.Ref.Valid() || row.Ref.DomainID != before.DomainID ||
				row.Line <= recovery.Meta.Count || !validHistoryDigest(row.Digest) {
				return false
			}
			rows = append(rows, row)
		}
		data, root, err := marshalContentIndexRows(rows, delta.Before.File.Digest)
		if err != nil || !bytes.Equal(data, delta.Data) || root != delta.After.File.Digest ||
			len(rows) != delta.After.Records-delta.Before.Records {
			return false
		}
	}
	return true
}

func (s *JSONLStore) appendContentStorageWrite(write *contentStorageWrite) error {
	if write == nil {
		return nil
	}
	return s.appendContentIndex(write.Append)
}

func (s *JSONLStore) recoverContentStorageOld(write *contentStorageWrite) error {
	if write == nil {
		return nil
	}
	return s.recoverContentIndexOld(write.Append)
}

func (s *JSONLStore) recoverContentStorageNew(
	ctx context.Context,
	key string,
	meta SessionMeta,
	write *contentStorageWrite,
) error {
	if meta.ContentIdentity == nil || meta.ContentIdentity.Unavailable || meta.ContentIdentity.storage == nil {
		return nil
	}
	if err := s.hydrateContentStorage(ctx, key, &meta, true, true); err != nil {
		return err
	}
	if write == nil {
		return nil
	}
	files := append([]contentArtifactDescriptor(nil), write.Created...)
	if write.Append != nil {
		files = append(files, write.Append.After.File)
	}
	for _, file := range files {
		if err := s.syncOwnedContentArtifact(
			s.dir+string(os.PathSeparator)+file.name(),
			func(_ string, phase string) error { return s.failContentStorage(file.name(), phase+"-sync") },
		); err != nil {
			return err
		}
	}
	return nil
}

// Retirement is optional maintenance after the source journal has settled. A
// different outstanding journal conservatively roots every candidate. Missing
// payload is harmless; unsafe or changed paths are preserved, never adopted.
func (s *JSONLStore) retireContentStorageWrite(
	ctx context.Context,
	key string,
	write *contentStorageWrite,
	committed bool,
) {
	if write == nil {
		return
	}
	files := write.Created
	if committed {
		files = write.Retire
	}
	if len(files) == 0 {
		return
	}
	if err := s.retireContentArtifacts(ctx, key, files); err != nil {
		log.Printf("memory: content storage retirement pending for %s: %v", key, err)
	}
}

func (s *JSONLStore) retireContentArtifacts(ctx context.Context, key string, files []contentArtifactDescriptor) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jsonl.commit.json") ||
			strings.HasSuffix(entry.Name(), ".jsonl.commit.decision") {
			return ErrHistoryRecoveryRequired
		}
	}
	meta, _, err := s.readContentMeta(key)
	if err != nil {
		return err
	}
	witness, present, err := s.readContentDomain(key)
	if err != nil {
		return err
	}
	roots := contentStorageRoots(meta.ContentIdentity)
	if present && witness.Storage != nil {
		state := &contentIdentityState{storage: &contentStorageState{Header: *witness.Storage}}
		roots = append(roots, contentStorageRoots(state)...)
	}
	live := make(map[string]bool, len(roots))
	for _, file := range roots {
		live[file.name()] = true
	}
	for _, file := range files {
		if err = ctx.Err(); err != nil {
			return err
		}
		if live[file.name()] {
			continue
		}
		if err = s.verifyContentArtifact(ctx, file); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err = removeContentArtifact(s.dir, file.name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

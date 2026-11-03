package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	historyDeletionVersion    = 1
	maxHistoryDeletionFiles   = 32
	maxHistoryDeletionJournal = 64 * 1024
)

// A cleanup record authorizes retirement of already verified source-owned
// files. It contains descriptors and file fingerprints, never message bodies.
// It shares the existing history journal discovery and pending-write fence.
type historyDeletionIntent struct {
	Version     uint8                       `json:"version"`
	Key         string                      `json:"key"`
	Purpose     string                      `json:"purpose"`
	Directory   contentFileIdentity         `json:"directory"`
	Files       []historyDeletionFile       `json:"files"`
	Fingerprint string                      `json:"fingerprint"`
	Images      []*contentPreimage          `json:"images,omitempty"`
	Artifacts   []contentArtifactDescriptor `json:"artifacts,omitempty"`
}

type contentFileIdentity struct {
	Volume uint64 `json:"volume"`
	File   uint64 `json:"file"`
}

type historyDeletionFile struct {
	Name     string                     `json:"name"`
	Identity contentFileIdentity        `json:"identity"`
	Bytes    int64                      `json:"bytes"`
	Digest   string                     `json:"digest"`
	Absent   bool                       `json:"absent,omitempty"`
	Artifact *contentArtifactDescriptor `json:"artifact,omitempty"`
	Image    *contentPreimage           `json:"image,omitempty"`
}

type historyDeletionRecord struct {
	Deletion *historyDeletionIntent `json:"deletion"`
}

func (s *JSONLStore) beginHistoryDeletion(
	ctx context.Context, key, purpose string,
	images []*contentPreimage, artifacts []contentArtifactDescriptor,
) error {
	parent, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	directory, err := contentArtifactFileIdentity(parent)
	err = errors.Join(err, parent.Close())
	if err != nil {
		return err
	}
	intent := &historyDeletionIntent{Version: historyDeletionVersion, Key: key, Purpose: purpose, Directory: directory}
	files := make(map[string]historyDeletionFile)
	for _, image := range images {
		if image == nil || !image.valid(key, image.DomainID, image.MutationID) {
			return ErrContentIdentityUnavailable
		}
		cloned := *image
		files[contentPreimageName(key, image.ID)] = historyDeletionFile{
			Name:  contentPreimageName(key, image.ID),
			Image: &cloned,
		}
	}
	for _, artifact := range artifacts {
		cloned := artifact
		files[artifact.name()] = historyDeletionFile{Name: artifact.name(), Artifact: &cloned}
	}
	for _, file := range files {
		if file.Image != nil {
			intent.Images = append(intent.Images, file.Image)
		}
		if file.Artifact != nil {
			intent.Artifacts = append(intent.Artifacts, *file.Artifact)
		}
	}
	sort.Slice(intent.Images, func(i, j int) bool { return intent.Images[i].ID < intent.Images[j].ID })
	sort.Slice(intent.Artifacts, func(i, j int) bool { return intent.Artifacts[i].name() < intent.Artifacts[j].name() })
	sourceOrder := make(map[string]int)
	for order, path := range s.historyDeletionSourcePaths(key) {
		name := filepath.Base(path)
		files[name] = historyDeletionFile{Name: name}
		sourceOrder[name] = order
	}
	for _, file := range files {
		actual, exists, operationErr := s.captureHistoryDeletionFile(ctx, file)
		if operationErr != nil {
			return operationErr
		}
		actual.Absent = !exists
		intent.Files = append(intent.Files, actual)
	}
	sort.Slice(intent.Files, func(i, j int) bool {
		// Complete private payloads retire before the listing and transcript.
		a, b := intent.Files[i], intent.Files[j]
		ap, bp := a.Artifact != nil || a.Image != nil, b.Artifact != nil || b.Image != nil
		if ap != bp {
			return ap
		}
		if !ap {
			return sourceOrder[a.Name] < sourceOrder[b.Name]
		}
		return a.Name < b.Name
	})
	intent.Fingerprint = historyDeletionFingerprint(intent)
	if err = s.validHistoryDeletion(key, intent); err != nil {
		return err
	}
	data, err := json.Marshal(historyDeletionRecord{Deletion: intent})
	if err != nil {
		return err
	}
	if len(data) > maxHistoryDeletionJournal {
		return ErrContentIdentityUnavailable
	}
	if err = s.failContentStorage(filepath.Base(s.historyRecoveryPath(key)), "delete-intent-create"); err != nil {
		return err
	}
	file, dir, err := openContentArtifact(s.dir, filepath.Base(s.historyRecoveryPath(key)), true)
	if err != nil {
		return err
	}
	s.markHistoryPending(key)
	defer closeContentArtifact(file, dir)
	if err = s.failContentStorage(filepath.Base(s.historyRecoveryPath(key)), "delete-intent-write"); err != nil {
		return errors.Join(err, s.abortHistoryDeletionCreate(file, key))
	}
	n, err := file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return errors.Join(err, s.abortHistoryDeletionCreate(file, key))
	}
	// Recovery repeats these durability phases before any retirement, including
	// when the original call wrote readable bytes but did not confirm Sync.
	return s.syncHistoryDeletionIntent(file, dir, key)
}

func (s *JSONLStore) abortHistoryDeletionCreate(file *os.File, key string) error {
	identity, err := contentArtifactFileIdentity(file)
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = removeContentArtifactIdentity(s.dir, filepath.Base(s.historyRecoveryPath(key)), &identity); err != nil {
		return err
	}
	s.metaMu.Lock()
	delete(s.historyPending, sanitizeKey(key))
	s.metaMu.Unlock()
	return nil
}

func historyDeletionFingerprint(intent *historyDeletionIntent) string {
	return contentJSONDigest(
		[]any{
			intent.Version,
			intent.Key,
			intent.Purpose,
			intent.Directory,
			intent.Files,
			intent.Images,
			intent.Artifacts,
		},
	)
}

func (s *JSONLStore) historyDeletionSourcePaths(key string) []string {
	return []string{
		s.metaPath(key), s.jsonlPath(key), s.historyDecisionPath(key),
		s.historyRecoveryRequiredPath(key), s.contentDomainPath(key), s.historyOrderWitnessPath(key),
	}
}

func (s *JSONLStore) validHistoryDeletion(key string, intent *historyDeletionIntent) error {
	if intent == nil || intent.Version != historyDeletionVersion || intent.Key == "" ||
		filepath.Base(s.historyRecoveryPath(intent.Key)) != filepath.Base(s.historyRecoveryPath(key)) ||
		intent.Directory.File == 0 || len(intent.Files) > maxHistoryDeletionFiles ||
		!validHistoryDigest(intent.Fingerprint) || intent.Fingerprint != historyDeletionFingerprint(intent) ||
		(intent.Purpose != "owner" && intent.Purpose != "retention" && intent.Purpose != "empty") {
		return ErrContentIdentityUnavailable
	}
	sources := make(map[string]bool)
	for _, path := range s.historyDeletionSourcePaths(intent.Key) {
		sources[filepath.Base(path)] = true
	}
	seen := make(map[string]bool)
	rootImages := make(map[string]*contentPreimage)
	rootArtifacts := make(map[string]contentArtifactDescriptor)
	for _, image := range intent.Images {
		if image == nil || !image.valid(intent.Key, image.DomainID, image.MutationID) {
			return ErrContentIdentityUnavailable
		}
		name := contentPreimageName(intent.Key, image.ID)
		if rootImages[name] != nil {
			return ErrContentIdentityUnavailable
		}
		rootImages[name] = image
	}
	for _, artifact := range intent.Artifacts {
		name := artifact.name()
		if _, exists := rootArtifacts[name]; exists {
			return ErrContentIdentityUnavailable
		}
		rootArtifacts[name] = artifact
	}
	private := false
	for _, file := range intent.Files {
		if file.Name == "" || filepath.Base(file.Name) != file.Name || seen[file.Name] || file.Bytes < 0 {
			return ErrContentIdentityUnavailable
		}
		if file.Absent {
			if file.Identity != (contentFileIdentity{}) || file.Bytes != 0 || file.Digest != "" {
				return ErrContentIdentityUnavailable
			}
		} else if file.Identity.File == 0 || !validHistoryDigest(file.Digest) {
			return ErrContentIdentityUnavailable
		}
		seen[file.Name] = true
		switch {
		case file.Artifact != nil && file.Image == nil:
			private = true
			if !validContentArtifact(
				*file.Artifact,
				intent.Key,
				file.Artifact.DomainID,
				file.Artifact.Generation,
				file.Artifact.Kind,
			) ||
				(file.Artifact.Kind != "rows" && file.Artifact.Kind != "pending" && file.Artifact.Kind != "last") ||
				file.Name != file.Artifact.name() {
				return ErrContentIdentityUnavailable
			}
			root, exists := rootArtifacts[file.Name]
			if !exists || root != *file.Artifact {
				return ErrContentIdentityUnavailable
			}
		case file.Image != nil && file.Artifact == nil:
			private = true
			if !file.Image.valid(intent.Key, file.Image.DomainID, file.Image.MutationID) ||
				file.Name != contentPreimageName(intent.Key, file.Image.ID) {
				return ErrContentIdentityUnavailable
			}
			root := rootImages[file.Name]
			if root == nil || *root != *file.Image {
				return ErrContentIdentityUnavailable
			}
		case file.Artifact == nil && file.Image == nil:
			if !sources[file.Name] {
				return ErrContentIdentityUnavailable
			}
		default:
			return ErrContentIdentityUnavailable
		}
	}
	if !private {
		return ErrContentIdentityUnavailable
	}
	for name := range sources {
		if !seen[name] {
			return ErrContentIdentityUnavailable
		}
	}
	for name := range rootImages {
		if !seen[name] {
			return ErrContentIdentityUnavailable
		}
	}
	for name := range rootArtifacts {
		if !seen[name] {
			return ErrContentIdentityUnavailable
		}
	}
	return nil
}

func (s *JSONLStore) captureHistoryDeletionFile(
	ctx context.Context,
	file historyDeletionFile,
) (historyDeletionFile, bool, error) {
	if err := ctx.Err(); err != nil {
		return file, false, err
	}
	fd, dir, err := openContentArtifact(s.dir, file.Name, false)
	if errors.Is(err, os.ErrNotExist) {
		return file, false, nil
	}
	if err != nil {
		return file, false, err
	}
	defer closeContentArtifact(fd, dir)
	info, err := fd.Stat()
	if err != nil {
		return file, false, err
	}
	identity, err := contentArtifactFileIdentity(fd)
	if err != nil {
		return file, false, err
	}
	hash := sha256.New()
	bytes, err := io.Copy(hash, fd)
	if err != nil {
		return file, false, err
	}
	if bytes != info.Size() {
		return file, false, ErrContentIdentityUnavailable
	}
	file.Identity, file.Bytes, file.Digest = identity, info.Size(), hex.EncodeToString(hash.Sum(nil))
	return file, true, ctx.Err()
}

func (s *JSONLStore) syncHistoryDeletionIntent(file, dir *os.File, key string) error {
	name := filepath.Base(s.historyRecoveryPath(key))
	if err := s.failContentStorage(name, "delete-intent-file-sync"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := s.failContentStorage(name, "delete-intent-directory-sync"); err != nil {
		return err
	}
	return dir.Sync()
}

func (s *JSONLStore) recoverHistoryDeletion(key string, data []byte) error {
	if len(data) > maxHistoryDeletionJournal {
		return ErrContentIdentityUnavailable
	}
	var record historyDeletionRecord
	if err := decodeContentWire(data, &record); err != nil {
		return err
	}
	if err := s.validHistoryDeletion(key, record.Deletion); err != nil {
		return err
	}
	intent := record.Deletion
	journal, dir, err := openContentArtifact(s.dir, filepath.Base(s.historyRecoveryPath(key)), false)
	if err != nil {
		return err
	}
	defer closeContentArtifact(journal, dir)
	directory, err := contentArtifactFileIdentity(dir)
	if err != nil {
		return err
	}
	if directory != intent.Directory {
		return ErrContentIdentityUnavailable
	}
	actual, err := io.ReadAll(io.LimitReader(journal, maxHistoryDeletionJournal+1))
	if err != nil {
		return err
	}
	if contentDigest(actual) != contentDigest(data) {
		return ErrContentIdentityUnavailable
	}
	if err = s.syncHistoryDeletionIntent(journal, dir, key); err != nil {
		return err
	}
	journalIdentity, err := contentArtifactFileIdentity(journal)
	if err != nil {
		return err
	}
	// Validate the entire remaining set before unlinking any file. Missing
	// entries are allowed only by this already durable, exact authorization.
	for _, file := range intent.Files {
		current, present, operationErr := s.captureHistoryDeletionFile(context.Background(), file)
		if operationErr != nil {
			return operationErr
		}
		if present &&
			(file.Absent || current.Identity != file.Identity || current.Bytes != file.Bytes || current.Digest != file.Digest) {
			return ErrContentIdentityUnavailable
		}
	}
	for _, file := range intent.Files {
		if file.Absent {
			continue
		}
		private := file.Artifact != nil || file.Image != nil
		phase := "delete-source"
		if private {
			phase = "remove"
		}
		if err = s.failContentStorage(file.Name, phase); err != nil {
			return err
		}
		err = removeContentArtifactIdentity(
			s.dir,
			file.Name,
			&file.Identity,
			func() error { return s.failContentStorage(file.Name, phase+"-directory-sync") },
		)
		if errors.Is(err, os.ErrNotExist) {
			if err = s.failContentStorage(file.Name, phase+"-directory-sync"); err == nil {
				err = dir.Sync()
			}
		}
		if err != nil {
			return err
		}
	}
	// The journal remains the discovery/authorization root until the complete
	// deletion is durable. Retire content caches but retain its pending fence.
	s.forgetDeletedHistory(key, true)
	if err = s.failContentStorage(filepath.Base(s.historyRecoveryPath(key)), "delete-intent-remove"); err != nil {
		return err
	}
	if err = journal.Close(); err != nil {
		return err
	}
	if err = removeContentArtifactIdentity(
		s.dir,
		filepath.Base(s.historyRecoveryPath(key)),
		&journalIdentity,
		func() error {
			return s.failContentStorage(
				filepath.Base(s.historyRecoveryPath(key)),
				"delete-intent-remove-directory-sync",
			)
		},
	); err != nil {
		return err
	}
	s.forgetDeletedHistory(key, false)
	return nil
}

func (s *JSONLStore) forgetDeletedHistory(key string, pending bool) {
	sanitized := sanitizeKey(key)
	s.metaMu.Lock()
	delete(s.metaCache, sanitized)
	delete(s.historyMarkers, sanitized)
	delete(s.historyWitnesses, sanitized)
	if pending {
		s.historyPending[sanitized] = true
	} else {
		delete(s.historyPending, sanitized)
	}
	s.metaMu.Unlock()
}

// Automatic pruners can retry their previously authorized deletion without
// rerunning a GC proof that intentionally removed one of its own artifacts.
func (s *JSONLStore) resumeHistoryDeletion(key string) (bool, error) {
	data, err := os.ReadFile(s.historyRecoveryPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record historyDeletionRecord
	if err = json.Unmarshal(data, &record); err != nil {
		return false, fmt.Errorf("memory: decode deletion recovery: %w", err)
	}
	if record.Deletion == nil {
		return false, nil
	}
	s.markHistoryPending(key)
	return true, s.recoverHistoryDeletion(key, data)
}

// Discovery grants nothing: only a known, bounded, intact deletion variant is
// a candidate. Normal/opaque/unreadable journals retain their existing fences.
// The actual retry revalidates the complete authorization and source identity
// under the session lock before retiring any remaining file.
func (s *JSONLStore) pendingHistoryDeletions(purpose string) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl.commit.json") {
			continue
		}
		file, dir, openErr := openContentArtifact(s.dir, entry.Name(), false)
		if openErr != nil {
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxHistoryDeletionJournal+1))
		readErr = errors.Join(readErr, closeContentArtifact(file, dir))
		if readErr != nil || len(data) > maxHistoryDeletionJournal {
			continue
		}
		var record historyDeletionRecord
		if decodeContentWire(data, &record) != nil || record.Deletion == nil || record.Deletion.Purpose != purpose {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".jsonl.commit.json")
		if s.validHistoryDeletion(key, record.Deletion) != nil {
			continue
		}
		keys = append(keys, record.Deletion.Key)
	}
	return keys, nil
}

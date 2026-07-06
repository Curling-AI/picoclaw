package media

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// CleanupPolicy controls how the MediaStore treats the underlying file when
// a ref is released or expires.
type CleanupPolicy string

const (
	// CleanupPolicyDeleteOnCleanup means the file is store-managed and may be
	// deleted once the final ref for that path is gone.
	CleanupPolicyDeleteOnCleanup CleanupPolicy = "delete_on_cleanup"
	// CleanupPolicyForgetOnly means the store should only drop ref mappings and
	// must never delete the underlying file.
	CleanupPolicyForgetOnly CleanupPolicy = "forget_only"
)

// MediaMeta holds metadata about a stored media file.
type MediaMeta struct {
	Filename      string
	ContentType   string
	Source        string        // "telegram", "discord", "tool:image-gen", etc.
	CleanupPolicy CleanupPolicy // defaults to CleanupPolicyDeleteOnCleanup
}

// MediaStore manages the lifecycle of media files associated with processing scopes.
type MediaStore interface {
	// Store registers an existing local file under the given scope.
	// Returns a ref identifier (e.g. "media://<id>").
	// Without a durable dir configured, Store does not move or copy the
	// file; it only records the mapping. With one, store-managed files
	// (delete_on_cleanup) are ingested into the durable dir — resolve the
	// ref instead of reusing the original path.
	// If meta.CleanupPolicy is empty, CleanupPolicyDeleteOnCleanup is assumed.
	Store(localPath string, meta MediaMeta, scope string) (ref string, err error)

	// Resolve returns the local file path for a given ref.
	Resolve(ref string) (localPath string, err error)

	// ResolveWithMeta returns the local file path and metadata for a given ref.
	ResolveWithMeta(ref string) (localPath string, meta MediaMeta, err error)

	// ReleaseAll deletes all files registered under the given scope
	// and removes the mapping entries. File-not-exist errors are ignored.
	ReleaseAll(scope string) error
}

// mediaEntry holds the path and metadata for a stored media file.
type mediaEntry struct {
	path     string
	meta     MediaMeta
	storedAt time.Time
}

type pathRefState struct {
	refCount       int
	deleteEligible bool
}

// MediaCleanerConfig configures the background TTL cleanup.
type MediaCleanerConfig struct {
	Enabled  bool
	MaxAge   time.Duration
	Interval time.Duration
}

// FileMediaStore is a pure in-memory implementation of MediaStore.
// Files are expected to already exist on disk (e.g. in /tmp/picoclaw_media/).
type FileMediaStore struct {
	mu          sync.RWMutex
	refs        map[string]mediaEntry
	scopeToRefs map[string]map[string]struct{}
	refToScope  map[string]string
	refToPath   map[string]string
	pathStates  map[string]pathRefState

	cleanerCfg MediaCleanerConfig
	stop       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
	nowFunc    func() time.Time // for testing

	// durableDir, when set, is the store's own directory: store-managed
	// files are ingested (moved) into it at Store() time and the ref index
	// is snapshotted to disk there on every mutation and reloaded on
	// startup. Media referenced by persisted session history is part of the
	// conversation record, not scratch — with a durable home, media:// refs
	// survive process restarts. Empty = upstream behavior (files stay where
	// producers wrote them, index in memory only).
	durableDir string
}

// persistedEntry is the on-disk form of one ref mapping.
type persistedEntry struct {
	Ref           string    `json:"ref"`
	Path          string    `json:"path"`
	Scope         string    `json:"scope"`
	StoredAt      time.Time `json:"stored_at"`
	Filename      string    `json:"filename,omitempty"`
	ContentType   string    `json:"content_type,omitempty"`
	Source        string    `json:"source,omitempty"`
	CleanupPolicy string    `json:"cleanup_policy,omitempty"`
}

// DurableDirEnv names the env var that gives the media store a durable home
// (e.g. a persistent volume). See FileMediaStore.durableDir.
const DurableDirEnv = "PICOCLAW_MEDIA_STORE_DIR"

// DurableDirFromEnv returns the configured durable media dir ("" = unset).
func DurableDirFromEnv() string {
	return strings.TrimSpace(os.Getenv(DurableDirEnv))
}

// indexPath returns the on-disk location of the ref index snapshot.
func (s *FileMediaStore) indexPath() string {
	if s.durableDir == "" {
		return ""
	}
	return filepath.Join(s.durableDir, "media-refs.json")
}

// WithDurableDir gives the store a durable home: store-managed files are
// ingested into dir at Store() time, the ref index is snapshotted there on
// every mutation, and any previous snapshot is loaded now (entries whose
// file no longer exists are dropped). Session history stores media:// refs;
// without a durable home every restart turns them into "unknown ref"
// dangling pointers.
func (s *FileMediaStore) WithDurableDir(dir string) *FileMediaStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.durableDir = dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.WarnCF("media", "durable dir: mkdir failed, staying in-memory", map[string]any{
			"dir":   dir,
			"error": err.Error(),
		})
		s.durableDir = ""
		return s
	}

	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		if !os.IsNotExist(err) {
			logger.WarnCF("media", "persist: failed to read ref index", map[string]any{
				"path":  s.indexPath(),
				"error": err.Error(),
			})
		}
		return s
	}
	var entries []persistedEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		logger.WarnCF("media", "persist: corrupt ref index, starting empty", map[string]any{
			"path":  s.indexPath(),
			"error": err.Error(),
		})
		return s
	}

	loaded, dropped := 0, 0
	for _, e := range entries {
		if _, err := os.Stat(e.Path); err != nil {
			dropped++
			continue
		}
		meta := MediaMeta{
			Filename:      e.Filename,
			ContentType:   e.ContentType,
			Source:        e.Source,
			CleanupPolicy: normalizeCleanupPolicy(CleanupPolicy(e.CleanupPolicy)),
		}
		s.refs[e.Ref] = mediaEntry{path: e.Path, meta: meta, storedAt: e.StoredAt}
		if s.scopeToRefs[e.Scope] == nil {
			s.scopeToRefs[e.Scope] = make(map[string]struct{})
		}
		s.scopeToRefs[e.Scope][e.Ref] = struct{}{}
		s.refToScope[e.Ref] = e.Scope
		s.refToPath[e.Ref] = e.Path

		pathState := s.pathStates[e.Path]
		if pathState.refCount == 0 {
			pathState.deleteEligible = meta.CleanupPolicy == CleanupPolicyDeleteOnCleanup
		} else if meta.CleanupPolicy == CleanupPolicyForgetOnly {
			pathState.deleteEligible = false
		}
		pathState.refCount++
		s.pathStates[e.Path] = pathState
		loaded++
	}
	if loaded > 0 || dropped > 0 {
		logger.InfoCF("media", "persist: ref index restored", map[string]any{
			"path":    s.indexPath(),
			"loaded":  loaded,
			"dropped": dropped,
		})
	}
	return s
}

// ingestExtension picks the file extension for an ingested file, preferring
// the declared filename over the scratch path (which may lack one).
func ingestExtension(localPath, filename string) string {
	if ext := filepath.Ext(filename); ext != "" {
		return ext
	}
	return filepath.Ext(localPath)
}

// moveFile moves src to dst, falling back to copy+remove when rename crosses
// filesystems (scratch in /tmp, durable dir on a mounted volume).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
		logger.WarnCF("media", "ingest: failed to remove scratch original", map[string]any{
			"path":  src,
			"error": err.Error(),
		})
	}
	return nil
}

// persistLocked snapshots the ref index to the durable dir (atomic
// tmp+rename). Callers must hold s.mu. Persistence failures are logged,
// never fatal — the in-memory store keeps working exactly as before.
func (s *FileMediaStore) persistLocked() {
	if s.durableDir == "" {
		return
	}
	entries := make([]persistedEntry, 0, len(s.refs))
	for ref, entry := range s.refs {
		entries = append(entries, persistedEntry{
			Ref:           ref,
			Path:          entry.path,
			Scope:         s.refToScope[ref],
			StoredAt:      entry.storedAt,
			Filename:      entry.meta.Filename,
			ContentType:   entry.meta.ContentType,
			Source:        entry.meta.Source,
			CleanupPolicy: string(entry.meta.CleanupPolicy),
		})
	}
	data, err := json.Marshal(entries)
	if err != nil {
		logger.WarnCF("media", "persist: marshal failed", map[string]any{"error": err.Error()})
		return
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		logger.WarnCF("media", "persist: write failed", map[string]any{"error": err.Error()})
		return
	}
	if err := os.Rename(tmp, s.indexPath()); err != nil {
		logger.WarnCF("media", "persist: rename failed", map[string]any{"error": err.Error()})
	}
}

// NewFileMediaStore creates a new FileMediaStore without background cleanup.
func NewFileMediaStore() *FileMediaStore {
	return &FileMediaStore{
		refs:        make(map[string]mediaEntry),
		scopeToRefs: make(map[string]map[string]struct{}),
		refToScope:  make(map[string]string),
		refToPath:   make(map[string]string),
		pathStates:  make(map[string]pathRefState),
		nowFunc:     time.Now,
	}
}

// NewFileMediaStoreWithCleanup creates a FileMediaStore with TTL-based background cleanup.
func NewFileMediaStoreWithCleanup(cfg MediaCleanerConfig) *FileMediaStore {
	return &FileMediaStore{
		refs:        make(map[string]mediaEntry),
		scopeToRefs: make(map[string]map[string]struct{}),
		refToScope:  make(map[string]string),
		refToPath:   make(map[string]string),
		pathStates:  make(map[string]pathRefState),
		cleanerCfg:  cfg,
		stop:        make(chan struct{}),
		nowFunc:     time.Now,
	}
}

// Store registers a local file under the given scope. The file must exist.
//
// With a durable dir configured, store-managed files (delete_on_cleanup —
// the store already owns their deletion) are ingested: moved out of the
// producer's scratch location into the durable dir, so the ref keeps
// resolving after a restart. forget_only files are borrowed paths owned by
// someone else and are never moved.
func (s *FileMediaStore) Store(localPath string, meta MediaMeta, scope string) (string, error) {
	if _, err := os.Stat(localPath); err != nil {
		return "", fmt.Errorf("media store: %s: %w", localPath, err)
	}

	id := uuid.New().String()
	ref := "media://" + id
	meta.CleanupPolicy = normalizeCleanupPolicy(meta.CleanupPolicy)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.durableDir != "" && meta.CleanupPolicy == CleanupPolicyDeleteOnCleanup {
		dest := filepath.Join(s.durableDir, id+ingestExtension(localPath, meta.Filename))
		if err := moveFile(localPath, dest); err != nil {
			// Fall back to the original location — no worse than before.
			logger.WarnCF("media", "ingest: failed to move file to durable dir", map[string]any{
				"from":  localPath,
				"to":    dest,
				"error": err.Error(),
			})
		} else {
			localPath = dest
		}
	}

	s.refs[ref] = mediaEntry{path: localPath, meta: meta, storedAt: s.nowFunc()}
	if s.scopeToRefs[scope] == nil {
		s.scopeToRefs[scope] = make(map[string]struct{})
	}
	s.scopeToRefs[scope][ref] = struct{}{}
	s.refToScope[ref] = scope
	s.refToPath[ref] = localPath

	pathState := s.pathStates[localPath]
	if pathState.refCount == 0 {
		pathState.deleteEligible = meta.CleanupPolicy == CleanupPolicyDeleteOnCleanup
	} else if meta.CleanupPolicy == CleanupPolicyForgetOnly {
		// Be conservative: once a path is borrowed externally, never let this
		// lifecycle auto-delete it even if store-managed refs also exist.
		pathState.deleteEligible = false
	}
	pathState.refCount++
	s.pathStates[localPath] = pathState

	s.persistLocked()

	return ref, nil
}

// Resolve returns the local path for the given ref.
func (s *FileMediaStore) Resolve(ref string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.refs[ref]
	if !ok {
		return "", fmt.Errorf("media store: unknown ref: %s", ref)
	}
	return entry.path, nil
}

// ResolveWithMeta returns the local path and metadata for the given ref.
func (s *FileMediaStore) ResolveWithMeta(ref string) (string, MediaMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.refs[ref]
	if !ok {
		return "", MediaMeta{}, fmt.Errorf("media store: unknown ref: %s", ref)
	}
	return entry.path, entry.meta, nil
}

// ReleaseAll removes all files under the given scope and cleans up mappings.
// Phase 1 (under lock): remove entries from maps.
// Phase 2 (no lock): delete store-managed files from disk once their final
// path ref is gone.
func (s *FileMediaStore) ReleaseAll(scope string) error {
	// Phase 1: collect paths and remove from maps under lock
	var paths []string

	s.mu.Lock()
	refs, ok := s.scopeToRefs[scope]
	if !ok {
		s.mu.Unlock()
		return nil
	}

	for ref := range refs {
		fallbackPath := ""
		if entry, exists := s.refs[ref]; exists {
			fallbackPath = entry.path
		}
		if removablePath, shouldDelete := s.releaseRefLocked(ref, fallbackPath); shouldDelete {
			paths = append(paths, removablePath)
		}
	}
	delete(s.scopeToRefs, scope)
	s.persistLocked()
	s.mu.Unlock()

	// Phase 2: delete files without holding the lock
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			logger.WarnCF("media", "release: failed to remove file", map[string]any{
				"path":  p,
				"error": err.Error(),
			})
		}
	}

	return nil
}

// CleanExpired removes all entries older than MaxAge.
// Phase 1 (under lock): identify expired entries and remove from maps.
// Phase 2 (no lock): delete store-managed files from disk to minimize lock contention.
func (s *FileMediaStore) CleanExpired() int {
	if s.cleanerCfg.MaxAge <= 0 {
		return 0
	}

	// Phase 1: collect expired entries under lock
	type expiredEntry struct {
		ref        string
		deletePath string
	}

	s.mu.Lock()
	cutoff := s.nowFunc().Add(-s.cleanerCfg.MaxAge)
	var expired []expiredEntry

	for ref, entry := range s.refs {
		if entry.storedAt.Before(cutoff) {
			if scope, ok := s.refToScope[ref]; ok {
				if scopeRefs, ok := s.scopeToRefs[scope]; ok {
					delete(scopeRefs, ref)
					if len(scopeRefs) == 0 {
						delete(s.scopeToRefs, scope)
					}
				}
			}

			expiredItem := expiredEntry{ref: ref}
			if deletePath, shouldDelete := s.releaseRefLocked(ref, entry.path); shouldDelete {
				expiredItem.deletePath = deletePath
			}
			expired = append(expired, expiredItem)
		}
	}
	if len(expired) > 0 {
		s.persistLocked()
	}
	s.mu.Unlock()

	// Phase 2: delete files without holding the lock
	for _, e := range expired {
		if e.deletePath == "" {
			continue
		}
		if err := os.Remove(e.deletePath); err != nil && !os.IsNotExist(err) {
			logger.WarnCF("media", "cleanup: failed to remove file", map[string]any{
				"path":  e.deletePath,
				"error": err.Error(),
			})
		}
	}

	return len(expired)
}

func normalizeCleanupPolicy(policy CleanupPolicy) CleanupPolicy {
	switch policy {
	case "", CleanupPolicyDeleteOnCleanup:
		return CleanupPolicyDeleteOnCleanup
	case CleanupPolicyForgetOnly:
		return CleanupPolicyForgetOnly
	default:
		return CleanupPolicyDeleteOnCleanup
	}
}

func (s *FileMediaStore) releaseRefLocked(ref, fallbackPath string) (string, bool) {
	path := fallbackPath
	if storedPath, ok := s.refToPath[ref]; ok {
		path = storedPath
		delete(s.refToPath, ref)
	}

	delete(s.refs, ref)
	delete(s.refToScope, ref)

	if path == "" {
		return "", false
	}

	pathState, ok := s.pathStates[path]
	if !ok {
		return "", false
	}
	if pathState.refCount <= 1 {
		delete(s.pathStates, path)
		return path, pathState.deleteEligible
	}

	pathState.refCount--
	s.pathStates[path] = pathState
	return "", false
}

// Start begins the background cleanup goroutine if cleanup is enabled.
// Safe to call multiple times; only the first call starts the goroutine.
func (s *FileMediaStore) Start() {
	if !s.cleanerCfg.Enabled || s.stop == nil {
		return
	}
	if s.cleanerCfg.Interval <= 0 || s.cleanerCfg.MaxAge <= 0 {
		logger.WarnCF("media", "cleanup: skipped due to invalid config", map[string]any{
			"interval": s.cleanerCfg.Interval.String(),
			"max_age":  s.cleanerCfg.MaxAge.String(),
		})
		return
	}

	s.startOnce.Do(func() {
		logger.InfoCF("media", "cleanup enabled", map[string]any{
			"interval": s.cleanerCfg.Interval.String(),
			"max_age":  s.cleanerCfg.MaxAge.String(),
		})

		go func() {
			ticker := time.NewTicker(s.cleanerCfg.Interval)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					if n := s.CleanExpired(); n > 0 {
						logger.InfoCF("media", "cleanup: removed expired entries", map[string]any{
							"count": n,
						})
					}
				case <-s.stop:
					return
				}
			}
		}()
	})
}

// Stop terminates the background cleanup goroutine.
// Safe to call multiple times; only the first call closes the channel.
func (s *FileMediaStore) Stop() {
	if s.stop == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stop)
	})
}

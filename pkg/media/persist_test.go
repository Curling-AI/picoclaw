package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeScratchMedia(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("img"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}
	return p
}

// Store-managed files are ingested out of scratch into the durable dir, and
// their refs survive a restart: a new store instance pointed at the same dir
// resolves them again.
func TestDurableDir_RefsSurviveRestart(t *testing.T) {
	scratch := t.TempDir()
	durable := t.TempDir()
	file := writeScratchMedia(t, scratch, "a.png")

	s1 := NewFileMediaStore().WithDurableDir(durable)
	ref, err := s1.Store(file, MediaMeta{Filename: "a.png", ContentType: "image/png", Source: "telegram"}, "scope-1")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	ingested, err := s1.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasPrefix(ingested, durable+string(os.PathSeparator)) {
		t.Errorf("ingested path = %q, want inside durable dir %q", ingested, durable)
	}
	if filepath.Ext(ingested) != ".png" {
		t.Errorf("ingested path = %q, want .png extension", ingested)
	}
	if _, statErr := os.Stat(file); !os.IsNotExist(statErr) {
		t.Errorf("scratch original should be moved away, stat err = %v", statErr)
	}

	s2 := NewFileMediaStore().WithDurableDir(durable)
	path, meta, err := s2.ResolveWithMeta(ref)
	if err != nil {
		t.Fatalf("ResolveWithMeta after restart: %v", err)
	}
	if path != ingested {
		t.Errorf("path = %q, want %q", path, ingested)
	}
	if meta.Filename != "a.png" || meta.ContentType != "image/png" || meta.Source != "telegram" {
		t.Errorf("meta = %+v, want original metadata", meta)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("ingested file must exist after restart: %v", err)
	}
}

// Without a durable dir the upstream contract holds: the file stays where the
// producer wrote it.
func TestNoDurableDir_FileNotMoved(t *testing.T) {
	scratch := t.TempDir()
	file := writeScratchMedia(t, scratch, "a.png")

	s := NewFileMediaStore()
	ref, err := s.Store(file, MediaMeta{}, "scope-1")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	path, err := s.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if path != file {
		t.Errorf("path = %q, want original %q", path, file)
	}
}

// forget_only files are borrowed (e.g. workspace uploads): never moved into
// the durable dir, never deleted by the store — across restarts too.
func TestDurableDir_ForgetOnlyNotIngested(t *testing.T) {
	scratch := t.TempDir()
	durable := t.TempDir()
	file := writeScratchMedia(t, scratch, "upload.pdf")

	s1 := NewFileMediaStore().WithDurableDir(durable)
	ref, err := s1.Store(file, MediaMeta{CleanupPolicy: CleanupPolicyForgetOnly}, "scope-1")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	path, _ := s1.Resolve(ref)
	if path != file {
		t.Errorf("forget_only path = %q, want original %q", path, file)
	}

	s2 := NewFileMediaStore().WithDurableDir(durable)
	if err := s2.ReleaseAll("scope-1"); err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("forget_only file must never be deleted by the store: %v", err)
	}
}

// ReleaseAll must be reflected in the snapshot — a restart must not bring
// released scopes back, and released ingested files are gone from disk.
func TestDurableDir_ReleaseAllPersisted(t *testing.T) {
	scratch := t.TempDir()
	durable := t.TempDir()
	fileA := writeScratchMedia(t, scratch, "a.png")
	fileB := writeScratchMedia(t, scratch, "b.png")

	s1 := NewFileMediaStore().WithDurableDir(durable)
	refA, _ := s1.Store(fileA, MediaMeta{}, "scope-a")
	refB, _ := s1.Store(fileB, MediaMeta{}, "scope-b")
	pathA, _ := s1.Resolve(refA)
	if err := s1.ReleaseAll("scope-a"); err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}
	if _, err := os.Stat(pathA); !os.IsNotExist(err) {
		t.Errorf("released ingested file should be deleted, stat err = %v", err)
	}

	s2 := NewFileMediaStore().WithDurableDir(durable)
	if _, err := s2.Resolve(refA); err == nil {
		t.Error("released ref must not survive restart")
	}
	if _, err := s2.Resolve(refB); err != nil {
		t.Errorf("unreleased ref must survive restart: %v", err)
	}
}

// An entry whose file vanished is kept at load (no stat per ref on the boot
// path) and pruned when resolved, so it never resolves to a dangling path and a
// later restart does not bring it back.
func TestDurableDir_MissingFilesPrunedOnResolve(t *testing.T) {
	scratch := t.TempDir()
	durable := t.TempDir()

	// One vanished file per resolver, so each one is checked on a ref it has
	// to prune itself.
	s1 := NewFileMediaStore().WithDurableDir(durable)
	refs := map[string]string{}
	for _, name := range []string{"plain.png", "meta.png"} {
		ref, err := s1.Store(writeScratchMedia(t, scratch, name), MediaMeta{}, "scope-1")
		if err != nil {
			t.Fatalf("Store: %v", err)
		}
		ingested, _ := s1.Resolve(ref)
		if err := os.Remove(ingested); err != nil {
			t.Fatalf("remove ingested file: %v", err)
		}
		refs[name] = ref
	}

	s2 := NewFileMediaStore().WithDurableDir(durable)
	if n := len(s2.refs); n != 2 {
		t.Fatalf("load kept %d refs, want 2: the file check belongs to Resolve", n)
	}
	if _, err := s2.Resolve(refs["plain.png"]); err == nil || !strings.Contains(err.Error(), "unknown ref") {
		t.Errorf("Resolve of a vanished file = %v, want unknown ref", err)
	}
	if _, _, err := s2.ResolveWithMeta(refs["meta.png"]); err == nil || !strings.Contains(err.Error(), "unknown ref") {
		t.Errorf("ResolveWithMeta of a vanished file = %v, want unknown ref", err)
	}
	if n := len(s2.refs); n != 0 {
		t.Errorf("%d vanished refs still indexed", n)
	}
	if len(s2.scopeToRefs) != 0 {
		t.Errorf("vanished refs still listed under their scope: %v", s2.scopeToRefs)
	}

	s3 := NewFileMediaStore().WithDurableDir(durable)
	if n := len(s3.refs); n != 0 {
		t.Errorf("a restart brought back %d pruned refs", n)
	}
}

// A corrupt index starts empty instead of failing the boot.
func TestDurableDir_CorruptIndexStartsEmpty(t *testing.T) {
	durable := t.TempDir()
	if err := os.WriteFile(filepath.Join(durable, "media-refs.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt index: %v", err)
	}

	s := NewFileMediaStore().WithDurableDir(durable)
	file := writeScratchMedia(t, t.TempDir(), "a.png")
	if _, err := s.Store(file, MediaMeta{}, "scope-1"); err != nil {
		t.Fatalf("Store after corrupt load: %v", err)
	}
}

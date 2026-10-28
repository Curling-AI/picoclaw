package memory

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestContentLineageActivationRetainsPhysicalWitnessEvidence(t *testing.T) {
	for _, kind := range []string{"absent", "malformed", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "dangling" && runtime.GOOS == "windows" {
				t.Skip("requires symbolic-link creation privilege")
			}
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_activation_physical_evidence"
			if err = s.AddMessage(t.Context(), key, "user", "preserved body"); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.metaPath(key))
			if err != nil {
				t.Fatal(err)
			}
			path := s.contentDomainPath(key)
			switch kind {
			case "malformed":
				err = os.WriteFile(path, []byte("{"), 0o600)
			case "dangling":
				err = os.Symlink(filepath.Join(t.TempDir(), "missing-witness-target"), path)
			}
			if err != nil {
				t.Fatal(err)
			}
			active, err := s.HistoryContentIdentityActive(t.Context(), key)
			if err != nil || active != (kind != "absent") {
				t.Fatal("physical activation evidence was classified as never initialized", kind, active, err)
			}
			frame, readErr := s.ReadHistoryContentFrame(t.Context(), key)
			if kind == "absent" {
				if readErr != nil || frame.IdentityStatus != ContentIdentityUninitialized {
					t.Fatal("legacy absence lost its existing meaning", frame, readErr)
				}
			} else if readErr == nil && frame.IdentityStatus != ContentIdentityUnavailable {
				t.Fatal("physical evidence invented known/uninitialized proof", frame, readErr)
			}
			after, err := os.ReadFile(s.metaPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("pure activation/frame getter rewrote metadata", err)
			}
			if kind == "dangling" {
				info, statErr := os.Lstat(path)
				if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("probe replaced the dangling evidence", statErr)
				}
			}
		})
	}
}

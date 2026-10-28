package session_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestJSONLBackendDeleteLegacyOwnerDoesNotDependOnForeignMetadata(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		for _, fault := range []string{"malformed", "directory", "permission"} {
			name := map[bool]string{false: "legacy/", true: "active/"}[initialized] + fault
			t.Run(name, func(t *testing.T) {
				if fault == "permission" && runtime.GOOS == "windows" {
					t.Skip("POSIX permission boundary")
				}
				dir := t.TempDir()
				store, err := memory.NewJSONLStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				backend := session.NewJSONLBackend(store)
				key := "agent:owned/delete-legacy"
				for range 4 {
					backend.AddMessage(key, "user", "owned body")
				}
				backend.SetSummary(key, "owned summary")
				if initialized {
					frame, readErr := backend.ReadHistoryContentFrame(t.Context(), key)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if _, readErr = backend.InitializeHistoryContentIdentity(
						t.Context(),
						key,
						frame.Observation,
					); readErr != nil {
						t.Fatal(readErr)
					}
				}
				base := filepath.Join(dir, strings.NewReplacer(":", "_", "/", "_", "\\", "_").Replace(key))
				body, err := os.ReadFile(base + ".jsonl")
				if err != nil {
					t.Fatal(err)
				}
				meta, err := os.ReadFile(base + ".meta.json")
				if err != nil {
					t.Fatal(err)
				}
				foreign := filepath.Join(dir, "another-session.meta.json")
				if fault == "directory" {
					err = os.Mkdir(foreign, 0o700)
				} else {
					payload := []byte("{")
					if fault == "permission" {
						payload = []byte(`{"key":"another-session"}`)
					}
					err = os.WriteFile(foreign, payload, 0o600)
					if err == nil && fault == "permission" {
						err = os.Chmod(foreign, 0)
						t.Cleanup(func() { _ = os.Chmod(foreign, 0o600) })
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				// The negative legacy probe is not authority for strict append.
				if _, err = backend.AppendHistoryContent(
					t.Context(),
					key,
					providers.Message{Role: "user", Content: "must not append"},
				); err == nil {
					t.Fatal("legacy negative probe bypassed append ownership")
				}
				err = backend.DeleteSession(key)
				if initialized {
					if err == nil {
						t.Fatal("active source bypassed strict ownership")
					}
					gotBody, _ := os.ReadFile(base + ".jsonl")
					gotMeta, _ := os.ReadFile(base + ".meta.json")
					if !bytes.Equal(gotBody, body) || !bytes.Equal(gotMeta, meta) {
						t.Fatal("refused active DELETE partially cleared source")
					}
				} else {
					if err != nil {
						t.Fatal("unrelated metadata blocked explicit legacy owner DELETE", err)
					}
					for _, suffix := range []string{".jsonl", ".meta.json"} {
						if _, statErr := os.Stat(base + suffix); !errors.Is(statErr, os.ErrNotExist) {
							t.Fatal("legacy content survived owner DELETE", suffix, statErr)
						}
					}
				}
				if _, statErr := os.Lstat(foreign); statErr != nil {
					t.Fatal("DELETE touched the unrelated source", statErr)
				}
			})
		}
	}
}

func TestJSONLBackendDeleteLegacyPreservesOwnMetadataFailures(t *testing.T) {
	for _, fault := range []string{"malformed", "wrong-owner", "permission"} {
		t.Run(fault, func(t *testing.T) {
			if fault == "permission" && runtime.GOOS == "windows" {
				t.Skip("POSIX permission boundary")
			}
			dir := t.TempDir()
			store, err := memory.NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			backend := session.NewJSONLBackend(store)
			key := "agent:own/failed-metadata"
			backend.AddMessage(key, "user", "must survive")
			backend.SetSummary(key, "must survive too")
			base := filepath.Join(dir, strings.NewReplacer(":", "_", "/", "_").Replace(key))
			body, err := os.ReadFile(base + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			path := base + ".meta.json"
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "malformed" {
				data = []byte("{")
			}
			if fault == "wrong-owner" {
				var meta map[string]any
				if err = json.Unmarshal(data, &meta); err != nil {
					t.Fatal(err)
				}
				meta["key"] = "agent:another-owner"
				data, err = json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if fault == "permission" {
				if err = os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
			}
			if err = backend.DeleteSession(key); err == nil {
				t.Fatal("own failure was classified as an unrelated legacy file")
			}
			got, err := os.ReadFile(base + ".jsonl")
			if err != nil || !bytes.Equal(got, body) {
				t.Fatal("own failure partially deleted durable body", err)
			}
			if fault != "permission" {
				got, err = os.ReadFile(path)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatal("own failure rewrote metadata", err)
				}
			}
		})
	}
}

func TestJSONLBackendDeleteRetiresTypedMutationPreimage(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := session.NewJSONLBackend(store)
	key := "sk_v1_backend_typed_delete"
	for range 4 {
		backend.AddMessage(key, "user", "owned durable message")
	}
	frame, err := backend.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	frame, err = backend.InitializeHistoryContentIdentity(t.Context(), key, frame.Observation)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := backend.CommitHistoryContentMutation(t.Context(), key, memory.ContentMutationPlan{
		MutationID: uuid.NewString(), IntentDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("owner request"))),
		DomainID: frame.DomainID, BeforeState: frame.StateToken, Kind: memory.ContentMutationRetainPrefix,
		RetainPrefix: &memory.ContentMutationRetainPrefixIntent{Retained: []memory.ContentRef{*frame.Entries[0].Ref}},
	})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	image := meta.ContentIdentity.Pending.Mutation.Before.Image
	path := filepath.Join(dir, key+".jsonl.content-preimage."+image.ID)
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err = backend.DeleteSession(key); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("backend owner DELETE left a typed raw preimage", err)
	}
	if got := backend.GetHistory(key); len(got) != 0 || backend.GetSummary(key) != "" {
		t.Fatal("warm backend resurrected deleted typed content", got)
	}
	if err = backend.AcknowledgeHistoryContentMutation(t.Context(), key, memory.ContentMutationAppliedAck{
		DomainID: pending.Transition.DomainID, MutationID: pending.Transition.MutationID,
		IntentDigest: pending.Transition.IntentDigest, Kind: pending.Transition.Kind,
		BeforeState: pending.Transition.BeforeState, AfterState: pending.Transition.AfterState,
		Before: pending.Transition.Before, After: pending.Transition.After, AppliedCommitID: uuid.NewString(),
	}); err == nil {
		t.Fatal("old application ACK revived an owner-deleted source")
	}
}

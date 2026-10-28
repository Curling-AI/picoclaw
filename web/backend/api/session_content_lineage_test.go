package api

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestHandleDeleteSession_RetiresTypedMutationArtifactsOrRefusesBeforeClear(t *testing.T) {
	for _, unsafeImage := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned-image", true: "unsafe-image"}[unsafeImage], func(t *testing.T) {
			configPath, cleanup := setupOAuthTestEnv(t)
			defer cleanup()
			dir := sessionsTestDir(t, configPath)
			store, err := memory.NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			key := legacyPicoSessionPrefix + "typed-content-delete"
			for range 4 {
				if err = store.AddMessage(t.Context(), key, "assistant", "owned source"); err != nil {
					t.Fatal(err)
				}
			}
			if err = store.SetSummary(t.Context(), key, "summary preserved on refusal"); err != nil {
				t.Fatal(err)
			}
			frame, err := store.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			frame, err = store.InitializeHistoryContentIdentity(t.Context(), key, frame.Observation)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.CommitHistoryContentMutation(t.Context(), key, memory.ContentMutationPlan{
				MutationID:   uuid.NewString(),
				IntentDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("owned request"))),
				DomainID:     frame.DomainID,
				BeforeState:  frame.StateToken,
				Kind:         memory.ContentMutationRetainPrefix,
				RetainPrefix: &memory.ContentMutationRetainPrefixIntent{
					Retained: []memory.ContentRef{*frame.Entries[0].Ref},
				},
			}); err != nil {
				t.Fatal(err)
			}
			meta, err := store.GetSessionMeta(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			image := meta.ContentIdentity.Pending.Mutation.Before.Image
			base := filepath.Join(dir, sanitizeSessionKey(key))
			imagePath := base + ".jsonl.content-preimage." + image.ID
			if unsafeImage {
				if err = os.Remove(imagePath); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(imagePath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before := make(map[string][]byte)
			for _, path := range []string{base + ".jsonl", base + ".meta.json", base + ".jsonl.content-domain"} {
				before[path], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			mux := http.NewServeMux()
			NewHandler(configPath).RegisterRoutes(mux)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/typed-content-delete", nil))
			if unsafeImage {
				if response.Code != http.StatusInternalServerError {
					t.Fatal("HTTP DELETE hid the producer's typed refusal", response.Code, response.Body.String())
				}
				for path, want := range before {
					got, readErr := os.ReadFile(path)
					if readErr != nil || !bytes.Equal(got, want) {
						t.Fatal("refused HTTP DELETE partially cleared body/summary/fence", path, readErr)
					}
				}
			} else {
				if response.Code != http.StatusNoContent {
					t.Fatal("owner HTTP DELETE did not retire typed source", response.Code, response.Body.String())
				}
				if _, statErr := os.Stat(imagePath); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("HTTP DELETE left the producer's raw preimage", statErr)
				}
			}
		})
	}
}

func TestHandleDeleteSession_RetiresPendingContentIdentity(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	dir := sessionsTestDir(t, configPath)
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := legacyPicoSessionPrefix + "content-lineage-delete"
	for range 4 {
		if err = store.AddMessage(t.Context(), key, "assistant", "same"); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := store.InitializeHistoryContentIdentity(t.Context(), key, before.Observation)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]memory.ContentRef, len(frame.Entries))
	for i, entry := range frame.Entries {
		refs[i] = *entry.Ref
	}
	plan := memory.ContentCompactionPlan{
		MutationID:    uuid.NewString(),
		DomainID:      frame.DomainID,
		BeforeState:   frame.StateToken,
		SummaryInputs: refs[:2],
		Retained:      refs[2:],
		Summary:       "old summary",
	}
	transition, err := store.CommitHistoryCompaction(t.Context(), key, plan)
	if err != nil {
		t.Fatal(err)
	}
	oldAck := memory.ContentArchiveAck{
		DomainID:        transition.DomainID,
		MutationID:      transition.MutationID,
		BeforeState:     transition.BeforeState,
		AfterState:      transition.AfterState,
		Archived:        transition.Dropped,
		ArchiveCommitID: uuid.NewString(),
	}
	mux := http.NewServeMux()
	NewHandler(configPath).RegisterRoutes(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/content-lineage-delete", nil))
	if response.Code != http.StatusNoContent {
		t.Fatal("explicit owner deletion was blocked by pending retention", response.Code, response.Body.String())
	}
	base := filepath.Join(dir, sanitizeSessionKey(key))
	for _, suffix := range []string{".jsonl", ".meta.json", ".jsonl.commit.json", ".jsonl.commit.decision", ".jsonl.content-domain", ".jsonl.order-witness"} {
		if _, err = os.Stat(base + suffix); !os.IsNotExist(err) {
			t.Fatal("explicit deletion left a prior domain file", suffix, err)
		}
	}
	if err = store.AcknowledgeHistoryCompaction(
		t.Context(),
		key,
		oldAck,
	); !errors.Is(
		err,
		memory.ErrContentIdentityUnavailable,
	) {
		t.Fatal("old archive confirmation recreated deleted identity", err)
	}
	if _, err = os.Stat(base + ".jsonl"); !os.IsNotExist(err) {
		t.Fatal("old ACK recreated deleted content", err)
	}
	newMessage, err := store.AppendHistoryContent(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "same"},
	)
	if err != nil || newMessage.Entry.Ref != nil {
		t.Fatal("fresh content inherited the deleted domain", newMessage, err)
	}
	newBefore, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := store.InitializeHistoryContentIdentity(t.Context(), key, newBefore.Observation)
	if err != nil || fresh.DomainID == frame.DomainID || len(fresh.Entries) != 1 || *fresh.Entries[0].Ref == refs[0] ||
		fresh.Summary != "" {
		t.Fatal("fresh baseline remapped deleted identity", fresh, err)
	}
	if err = store.AcknowledgeHistoryCompaction(
		t.Context(),
		key,
		oldAck,
	); !errors.Is(
		err,
		memory.ErrContentArchiveAck,
	) {
		t.Fatal("old ACK was accepted by a new domain", err)
	}
	if _, err = store.CommitHistoryCompaction(t.Context(), key, plan); !errors.Is(err, memory.ErrContentStateChanged) {
		t.Fatal("old compaction was replayed into the recreated source", err)
	}
}

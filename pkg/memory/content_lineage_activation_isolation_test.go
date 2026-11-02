package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentLineageLegacyActivationDoesNotRequireOtherMetadata(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "agent:cron-job-run-1"
	if err = s.AddMessage(t.Context(), key, "user", "legacy content"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "other.meta.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	metaBefore, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		t.Fatal(err)
	}
	active, err := s.HistoryContentIdentityActive(t.Context(), key)
	if err != nil || active {
		t.Fatal("unrelated broken metadata activated or blocked a never-initialized source", active, err)
	}
	metaAfter, err := os.ReadFile(s.metaPath(key))
	if err != nil || !bytes.Equal(metaBefore, metaAfter) || len(s.metaCache) != 0 {
		t.Fatal("negative activation probe mutated source or populated writer cache", err)
	}
	for _, marker := range []string{"null", "witness", "journal"} {
		t.Run(marker, func(t *testing.T) {
			path := s.contentDomainPath(key)
			data := []byte("{}")
			switch marker {
			case "null":
				path = s.metaPath(key)
				var marked map[string]json.RawMessage
				if err = json.Unmarshal(metaBefore, &marked); err != nil {
					t.Fatal(err)
				}
				marked["content_identity"] = json.RawMessage("null")
				data, err = json.Marshal(marked)
				if err != nil {
					t.Fatal(err)
				}
			case "journal":
				path = s.historyRecoveryPath(key)
			}
			if err = os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			active, probeErr := s.HistoryContentIdentityActive(t.Context(), key)
			if probeErr == nil && !active {
				t.Fatal("evidence of prior activation fell back to destructive legacy behavior")
			}
			if marker == "null" {
				if err = os.WriteFile(path, metaBefore, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err = os.Remove(s.metaPath(key)); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(s.metaPath(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err = s.HistoryContentIdentityActive(t.Context(), key); err == nil {
		t.Fatal("own metadata I/O was converted into proof of no activation")
	}
}

func TestContentLineageNegativeActivationDoesNotProveAliasOwnership(t *testing.T) {
	for _, broken := range []string{"truncated", "directory"} {
		t.Run(broken, func(t *testing.T) { testContentAliasOwnershipUnavailable(t, broken) })
	}
}

func testContentAliasOwnershipUnavailable(t *testing.T, broken string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	alias, owner := "agent:legacy", "sk_v1_actual_owner"
	if err = s.AddMessage(t.Context(), alias, "user", "old physical legacy file"); err != nil {
		t.Fatal(err)
	}
	if err = s.AddMessage(t.Context(), owner, "user", "real owner"); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertSessionMeta(t.Context(), owner, nil, []string{alias}); err != nil {
		t.Fatal(err)
	}
	initializedContent(t, s, owner)
	if broken == "truncated" {
		if err = os.WriteFile(s.metaPath(owner), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		if err = os.Remove(s.metaPath(owner)); err != nil {
			t.Fatal(err)
		}
		if err = os.Mkdir(s.metaPath(owner), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(s.jsonlPath(alias))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadHistoryContentFrame(t.Context(), alias); err == nil {
		t.Fatal("broken possible alias owner was ignored by qualified frame reader")
	}
	if _, err = s.InitializeHistoryContentIdentity(t.Context(), alias, "unproved observation"); err == nil {
		t.Fatal("negative activation probe initialized a possibly aliased source")
	}
	if _, err = s.AppendHistoryContent(
		t.Context(),
		alias,
		providers.Message{Role: "user", Content: "wrong source"},
	); err == nil {
		t.Fatal("negative activation probe allowed a typed append without alias ownership")
	}
	after, err := os.ReadFile(s.jsonlPath(alias))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused alias operation changed durable legacy source", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.HistoryContentIdentityActive(ctx, alias); !errors.Is(err, context.Canceled) {
		t.Fatal("negative activation hid cancellation", err)
	}
}

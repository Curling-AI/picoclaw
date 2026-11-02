package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

type legacyOnlyContentSessions struct{ session.SessionStore }

func TestContentLineageUninitializedSourceKeepsLegacyCompaction(t *testing.T) {
	for _, broken := range []string{"partial-tail", "lost-count"} {
		for _, optional := range []bool{false, true} {
			t.Run(
				broken+map[bool]string{false: "/legacy-control", true: "/optional-uninitialized"}[optional],
				func(t *testing.T) {
					al := newCMTestAgentLoop(testConfig(t))
					t.Cleanup(al.Close)
					dir := t.TempDir()
					s, err := memory.NewJSONLStore(dir)
					if err != nil {
						t.Fatal(err)
					}
					key := "sk_v1_uninitialized_compaction"
					b := session.NewJSONLBackend(s)
					for i := 0; i < 6; i++ {
						role := "user"
						if i%2 == 1 {
							role = "assistant"
						}
						if err = b.AppendMessage(key, providers.Message{Role: role, Content: "same"}); err != nil {
							t.Fatal(err)
						}
					}
					if broken == "lost-count" {
						if err = s.TruncateHistory(t.Context(), key, 4); err != nil {
							t.Fatal(err)
						}
						meta, getErr := s.GetSessionMeta(t.Context(), key)
						if getErr != nil {
							t.Fatal(getErr)
						}
						meta.Count = 0
						data, encodeErr := json.Marshal(meta)
						if encodeErr != nil {
							t.Fatal(encodeErr)
						}
						if err = os.WriteFile(filepath.Join(dir, key+".meta.json"), data, 0o600); err != nil {
							t.Fatal(err)
						}
					} else {
						p := filepath.Join(dir, key+".jsonl")
						f, e := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
						if e != nil {
							t.Fatal(e)
						}
						_, e = f.WriteString(`{"content":"partial`)
						if e != nil {
							t.Fatal(e)
						}
						if e = f.Close(); e != nil {
							t.Fatal(e)
						}
					}
					s, err = memory.NewJSONLStore(dir)
					if err != nil {
						t.Fatal(err)
					}
					b = session.NewJSONLBackend(s)
					if optional {
						al.registry.GetDefaultAgent().Sessions = b
					} else {
						al.registry.GetDefaultAgent().Sessions = legacyOnlyContentSessions{b}
					}
					if err = al.contextManager.Compact(
						t.Context(),
						&CompactRequest{SessionKey: key, Reason: ContextCompressReasonRetry},
					); err != nil {
						t.Fatal("interface presence activated a stricter flow without opt-in", err)
					}
					if len(b.GetHistory(key)) > 4 {
						t.Fatal("legacy compaction stopped working")
					}
				},
			)
		}
	}
}

func TestContentLineageActivatedIdentityLossNeverUsesLegacyCut(t *testing.T) {
	for _, loss := range []string{"witness", "metadata", "null-marker"} {
		t.Run(loss, func(t *testing.T) {
			al := newCMTestAgentLoop(testConfig(t))
			t.Cleanup(al.Close)
			dir := t.TempDir()
			store, err := memory.NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_activated_identity_loss"
			backend := session.NewJSONLBackend(store)
			al.registry.GetDefaultAgent().Sessions = backend
			for i := range 6 {
				role := "user"
				if i%2 == 1 {
					role = "assistant"
				}
				if err = backend.AppendMessage(key, providers.Message{Role: role, Content: "same"}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := backend.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = backend.InitializeHistoryContentIdentity(t.Context(), key, before.Observation); err != nil {
				t.Fatal(err)
			}
			metaPath := filepath.Join(dir, key+".meta.json")
			witnessPath := filepath.Join(dir, key+".jsonl.content-domain")
			if loss != "metadata" {
				if err = os.Remove(witnessPath); err != nil {
					t.Fatal(err)
				}
			}
			if loss != "witness" {
				data, readErr := os.ReadFile(metaPath)
				if readErr != nil {
					t.Fatal(readErr)
				}
				var meta map[string]json.RawMessage
				if err = json.Unmarshal(data, &meta); err != nil {
					t.Fatal(err)
				}
				delete(meta, "content_identity")
				if loss == "null-marker" {
					meta["content_identity"] = json.RawMessage("null")
				}
				data, err = json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(metaPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			physicalPath := filepath.Join(dir, key+".jsonl")
			physical, err := os.ReadFile(physicalPath)
			if err != nil {
				t.Fatal(err)
			}
			metaBefore, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatal(err)
			}
			if err = al.contextManager.Compact(
				t.Context(),
				&CompactRequest{SessionKey: key, Reason: ContextCompressReasonRetry},
			); !errors.Is(
				err,
				memory.ErrContentIdentityUnavailable,
			) {
				t.Fatal("previously activated source fell back to an unproved legacy cut", err)
			}
			after, err := os.ReadFile(physicalPath)
			if err != nil || !bytes.Equal(physical, after) {
				t.Fatal("lost identity discarded durable content", err)
			}
			metaAfter, err := os.ReadFile(metaPath)
			if err != nil || !bytes.Equal(metaBefore, metaAfter) {
				t.Fatal("lost identity partially changed summary/metadata", err)
			}
		})
	}
}

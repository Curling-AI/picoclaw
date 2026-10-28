package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestContentLineageCompactRejectsUncertainActivationJournal(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(map[bool]string{false: "warm", true: "cold"}[reopen], func(t *testing.T) {
			al := newCMTestAgentLoop(testConfig(t))
			t.Cleanup(al.Close)
			dir := t.TempDir()
			s, err := memory.NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_pending_activation_compact"
			base := filepath.Join(dir, key)
			for i := 0; i < 6; i++ {
				role := "user"
				if i%2 == 1 {
					role = "assistant"
				}
				if err = s.AddFullMessage(
					t.Context(),
					key,
					providers.Message{Role: role, Content: "durable"},
				); err != nil {
					t.Fatal(err)
				}
			}
			oldMeta, err := os.ReadFile(base + ".meta.json")
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.InitializeHistoryContentIdentity(t.Context(), key, before.Observation); err != nil {
				t.Fatal(err)
			}
			newMeta, err := os.ReadFile(base + ".meta.json")
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(base + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			journal, err := json.Marshal(
				map[string]any{
					"meta":         json.RawMessage(oldMeta),
					"next":         json.RawMessage(newMeta),
					"appendOffset": info.Size(),
					"existed":      true,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := json.Marshal(
				map[string]string{"journal": fmt.Sprintf("%x", sha256.Sum256(journal)), "decision": "new"},
			)
			if err != nil {
				t.Fatal(err)
			}
			for suffix, data := range map[string][]byte{".meta.json": oldMeta, ".jsonl.commit.json": journal, ".jsonl.commit.decision": decision} {
				if err = os.WriteFile(base+suffix, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err = os.Remove(base + ".jsonl.content-domain"); err != nil {
				t.Fatal(err)
			}
			if reopen {
				s, err = memory.NewJSONLStore(dir)
				if err != nil {
					t.Fatal(err)
				}
			}
			al.registry.GetDefaultAgent().Sessions = session.NewJSONLBackend(s)
			physical, err := os.ReadFile(base + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			if err = al.contextManager.Compact(
				t.Context(),
				&CompactRequest{SessionKey: key, Reason: ContextCompressReasonRetry},
			); !errors.Is(
				err,
				memory.ErrHistoryRecoveryRequired,
			) {
				t.Error("uncertain activation was reported as a healthy legacy no-op", err)
			}
			after, err := os.ReadFile(base + ".jsonl")
			if err != nil || !bytes.Equal(physical, after) {
				t.Fatal("probe/compact changed uncertain durable source", err)
			}
		})
	}
}

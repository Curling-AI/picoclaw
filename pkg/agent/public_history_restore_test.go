package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestRestoreSessionKeepsModelContextAvailableWithUnavailablePublicSource(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "after-context-compaction"}[truncated], func(t *testing.T) {
			dir, key := t.TempDir(), "sk_v1_public_restore"
			store, err := memory.NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, body := range []string{"original request", "original answer"} {
				if err = store.AddMessage(t.Context(), key, "assistant", body); err != nil {
					t.Fatal(err)
				}
			}
			backend := session.NewJSONLBackend(store)
			snapshot := backend.GetHistory(key)
			if !truncated {
				reshaped := backend.GetHistory(key)
				reshaped[1].Content = "context retry representation"
				if err = store.SetHistory(t.Context(), key, reshaped); err != nil {
					t.Fatal(err)
				}
			}
			if truncated {
				if err = store.TruncateHistory(t.Context(), key, 1); err != nil {
					t.Fatal(err)
				}
			}
			if err = store.AddMessage(t.Context(), key, "user", "aborted prompt"); err != nil {
				t.Fatal(err)
			}
			meta, err := store.GetSessionMeta(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			meta.PublicHistoryVersion = -1
			data, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, key+".meta.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, key+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			store, err = memory.NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			backend = session.NewJSONLBackend(store)
			agent := &AgentInstance{Sessions: backend}
			ts := &turnState{sessionKey: key, restorePointHistory: snapshot, restorePointSummary: "snapshot summary"}
			restored, err := ts.restoreSession(agent)
			if err != nil || !restored {
				t.Fatal("public proof blocked real snapshot restore", restored, err)
			}
			after := backend.GetHistory(key)
			if len(after) != len(snapshot) {
				t.Fatal("aborted input survived restore", after)
			}
			for i := range snapshot {
				if after[i].Content != snapshot[i].Content || after[i].HistoryID != snapshot[i].HistoryID {
					t.Fatal("snapshot occurrence changed", after[i], snapshot[i])
				}
			}
			if backend.GetSummary(key) != "snapshot summary" {
				t.Fatal("summary restore failed")
			}
			current, err := store.GetSessionMeta(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if current.Skip != meta.Skip || current.PublicHistoryVersion != -1 || current.PublicHistoryComplete {
				t.Fatal("restore adopted public proof or collected prefix", current)
			}
			if _, err = store.ReadPublicHistory(t.Context(), key); !errors.Is(err, memory.ErrPublicHistoryCoverage) {
				t.Fatal("unavailable public source was reattested", err)
			}
			if truncated {
				prefix := bytes.SplitAfter(before, []byte{'\n'})[0]
				raw, readErr := os.ReadFile(filepath.Join(dir, key+".jsonl"))
				if readErr != nil || !bytes.HasPrefix(raw, prefix) {
					t.Fatal("restore collected skipped source", readErr)
				}
				if _, err = store.ReadHistoryPage(t.Context(), memory.HistoryPageRequest{Key: key}); err == nil {
					t.Fatal("mixed raw generation granted continuation")
				}
				if err = store.Compact(t.Context(), key); !errors.Is(err, memory.ErrPublicHistoryCoverage) {
					t.Fatal("unknown display authorized GC", err)
				}
				retained, readErr := os.ReadFile(filepath.Join(dir, key+".jsonl"))
				if readErr != nil || !reflect.DeepEqual(retained, raw) {
					t.Fatal("refused GC changed source", readErr)
				}
			}
		})
	}
}

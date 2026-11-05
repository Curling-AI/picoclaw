package memory

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

func TestContentLineageActiveTruncateValidatesBeforeDiscard(t *testing.T) {
	for _, typed := range []bool{false, true} {
		for _, appended := range []bool{false, true} {
			name := map[bool]string{false: "initial/", true: "typed/"}[typed] +
				map[bool]string{false: "no-intervening-append", true: "after-append"}[appended]
			t.Run(name, func(t *testing.T) {
				s, key, frame := typedMutationSource(t)
				if typed {
					result, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 4))
					if err != nil {
						t.Fatal(err)
					}
					if err = s.AcknowledgeHistoryContentMutation(
						t.Context(),
						key,
						typedAppliedAck(result.Transition),
					); err != nil {
						t.Fatal(err)
					}
				}
				data, err := os.ReadFile(s.jsonlPath(key))
				if err != nil {
					t.Fatal(err)
				}
				tampered := bytes.Replace(data, []byte(`"content":"same"`), []byte(`"content":"evil"`), 1)
				if bytes.Equal(data, tampered) || len(data) != len(tampered) {
					t.Fatal("fixture did not change an earlier row at equal size")
				}
				if err = os.WriteFile(s.jsonlPath(key), tampered, 0o600); err != nil {
					t.Fatal(err)
				}
				if appended {
					if err = s.AddMessage(t.Context(), key, "assistant", "new body remains accepted"); err != nil {
						t.Fatal(err)
					}
				}
				beforeBody, err := os.ReadFile(s.jsonlPath(key))
				if err != nil {
					t.Fatal(err)
				}
				beforeMeta, err := os.ReadFile(s.metaPath(key))
				if err != nil {
					t.Fatal(err)
				}
				if err = s.TruncateHistory(t.Context(), key, 2); !errors.Is(err, ErrContentIdentityUnavailable) {
					t.Fatal("unqualified earlier bytes authorized a destructive suffix cut", err)
				}
				afterBody, _ := os.ReadFile(s.jsonlPath(key))
				afterMeta, _ := os.ReadFile(s.metaPath(key))
				if !bytes.Equal(beforeBody, afterBody) || !bytes.Equal(beforeMeta, afterMeta) {
					t.Fatal("refused truncation changed source bytes/metadata")
				}
				// With truncation refused, Skip is still zero: Compact may
				// return its existing no-work result, but must not rewrite bytes.
				if err = s.Compact(t.Context(), key); err != nil && !errors.Is(err, ErrContentIdentityUnavailable) {
					t.Fatal("unexpected GC failure", err)
				}
				afterBody, _ = os.ReadFile(s.jsonlPath(key))
				if !bytes.Equal(beforeBody, afterBody) {
					t.Fatal("GC rewrote an unproved source after refused truncation")
				}
			})
		}
	}
}

func TestContentLineageActiveTruncateKeepsHealthyAndNeverInitializedBehavior(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "active"}[initialized], func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "agent:truncate-control"
			for range 4 {
				if err = s.AddMessage(t.Context(), key, "assistant", "owned"); err != nil {
					t.Fatal(err)
				}
			}
			if initialized {
				initializedContent(t, s, key)
			}
			if err = s.TruncateHistory(t.Context(), key, 2); err != nil {
				t.Fatal("healthy suffix intent regressed", err)
			}
			history, err := s.GetHistory(t.Context(), key)
			if err != nil || len(history) != 2 {
				t.Fatal("valid truncate lost retained content", history, err)
			}
		})
	}
}

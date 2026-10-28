package memory

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func appendStatesForTest(t *testing.T, result ContentAppendResult) (string, string, string) {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		DomainID    string `json:"domainId"`
		BeforeState string `json:"beforeState"`
		AfterState  string `json:"afterState"`
	}
	if err = json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state.DomainID, state.BeforeState, state.AfterState
}

func TestContentLineageAppendStatesRequireContentProofIndependentlyOfOrder(t *testing.T) {
	for _, mode := range []string{"uninitialized", "content loss", "order loss", "both lost"} {
		t.Run(mode, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_append_state_proof_loss"
			if err = s.AddMessage(t.Context(), key, "assistant", "before"); err != nil {
				t.Fatal(err)
			}
			var before ContentFrame
			if mode != "uninitialized" {
				before = initializedContent(t, s, key)
			}
			if mode == "content loss" || mode == "both lost" {
				if err = os.Remove(s.contentDomainPath(key)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "order loss" || mode == "both lost" {
				if err = os.Remove(s.historyOrderWitnessPath(key)); err != nil {
					t.Fatal(err)
				}
			}
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.AppendHistoryContent(t.Context(), key, providers.Message{
				Role: "assistant", Content: "after", Delivery: &providers.Delivery{Origin: "cron:job:proof-loss"},
			})
			if err != nil || result.Entry.Message.Delivery.Origin != "cron:job:proof-loss" {
				t.Fatal("missing proofs disabled exact content append", result, err)
			}
			if mode == "order loss" {
				if result.Entry.Ref == nil || result.DomainID != before.DomainID ||
					result.BeforeState != before.StateToken ||
					result.AfterState == "" ||
					result.Receipt != nil ||
					result.Entry.ReceiptBinding != nil {
					t.Fatal("lost Order discarded independent content states or minted a receipt", result)
				}
			} else if result.Entry.Ref != nil || result.DomainID != "" || result.BeforeState != "" || result.AfterState != "" {
				t.Fatal("unproved content identity acquired a state interval", result)
			}
		})
	}
}

func TestContentLineageAppendStatesBelongToOwnCommit(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_append_state_AB"
	before := initializedContent(t, s, key)
	at := time.Now().UTC()
	a, err := s.AppendHistoryContent(
		t.Context(),
		key,
		providers.Message{
			Role:      "assistant",
			Content:   "same",
			CreatedAt: &at,
			Delivery:  &providers.Delivery{Origin: "cron:job:run-A", At: at},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	afterA, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AppendHistoryContent(
		t.Context(),
		key,
		providers.Message{
			Role:      "assistant",
			Content:   "same",
			CreatedAt: &at,
			Delivery:  &providers.Delivery{Origin: "cron:job:run-B", At: at},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	afterB, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	domainA, beforeA, stateA := appendStatesForTest(t, a)
	domainB, beforeB, stateB := appendStatesForTest(t, b)
	if domainA != before.DomainID || beforeA != before.StateToken || stateA != afterA.StateToken ||
		stateA == afterB.StateToken {
		t.Fatal(
			"append A lacks its exact own state interval",
			domainA,
			beforeA,
			stateA,
			afterA.StateToken,
			afterB.StateToken,
		)
	}
	if domainB != domainA || beforeB != stateA || stateB != afterB.StateToken || a.Entry.Ref == nil ||
		b.Entry.Ref == nil ||
		*a.Entry.Ref == *b.Entry.Ref {
		t.Fatal("interleaved append state or occurrence was relabeled", a, b)
	}
	if _, err = s.CommitHistoryContentEdit(
		t.Context(),
		key,
		ContentEditPlan{DomainID: domainA, BeforeState: stateA, Entries: afterA.Entries, Summary: afterA.Summary},
	); !errors.Is(
		err,
		ErrContentStateChanged,
	) {
		t.Fatal("A state was incorrectly accepted over newer B", err)
	}
}

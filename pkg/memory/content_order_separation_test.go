package memory

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentRollbackPreservesIndependentOrderAndLostProofFallback(t *testing.T) {
	for _, qualified := range []bool{false, true} {
		lane := "input"
		if qualified {
			lane = "qualified"
		}
		t.Run(lane, func(t *testing.T) {
			for _, mode := range []string{"content-only", "counter-lost", "witness-lost"} {
				t.Run(mode, func(t *testing.T) {
					s, key, before := typedMutationSource(t)
					old, e := s.readRecoveryMeta(key)
					if e != nil {
						t.Fatal(e)
					}
					if _, e = s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(before, 2)); e != nil {
						t.Fatal(e)
					}
					current, e := s.readRecoveryMeta(key)
					if e != nil {
						t.Fatal(e)
					}
					if current.HistorySequence == 0 || current.HistoryStreamID == "" {
						t.Fatal("fixturehasnoknownorder")
					}
					rolled := copyMeta(current)
					rolled.ContentIdentity = cloneContentState(old.ContentIdentity)
					if mode == "counter-lost" {
						rolled.HistorySequence--
					}
					encoded, e := json.Marshal(rolled)
					if e != nil {
						t.Fatal(e)
					}
					if e = os.WriteFile(s.metaPath(key), encoded, 0o600); e != nil {
						t.Fatal(e)
					}
					if mode == "witness-lost" {
						if e = os.Remove(s.historyOrderWitnessPath(key)); e != nil {
							t.Fatal(e)
						}
					}
					s, e = NewJSONLStore(s.dir)
					if e != nil {
						t.Fatal(e)
					}
					// The current physical prefix retains only sequences1/2, whereas the
					// independently committed highwater remains4. Never reconstruct from2.
					if mode == "content-only" {
						prior, readErr := s.ReadHistoryWithReceipts(t.Context(), key)
						if readErr != nil || len(prior) != 2 || prior[1].Order.Sequence != 2 {
							t.Fatal("knowncountercontrol", prior, readErr)
						}
					}
					var input ContentAppendResult
					message := providers.Message{
						Role:    "assistant",
						Content: "durable after independent content rollback",
					}
					if qualified {
						input, e = s.AppendHistoryContent(t.Context(), key, message)
					} else {
						input, e = s.AppendHistoryContentInput(t.Context(), key, message)
					}
					if e != nil {
						t.Fatal(e)
					}
					if input.Entry.Ref != nil || input.Entry.ReceiptBinding != nil || input.DomainID != "" ||
						input.BeforeState != "" ||
						input.AfterState != "" {
						t.Fatal("contentrollbackinventedref/binding/interval", input)
					}
					meta, e := s.readRecoveryMeta(key)
					if e != nil {
						t.Fatal(e)
					}
					if mode == "content-only" {
						if input.Receipt == nil || meta.HistorySequence != current.HistorySequence+1 ||
							meta.HistoryStreamID != current.HistoryStreamID ||
							meta.HistoryRecoveryRequired {
							t.Fatal(
								"content-onlylossunnecessarilyerasedgenuinecounter",
								input.Receipt,
								meta.HistorySequence,
								meta.HistoryRecoveryRequired,
							)
						}
						fresh, openErr := NewJSONLStore(s.dir)
						if openErr != nil {
							t.Fatal(openErr)
						}
						actual, readErr := fresh.ReadHistoryWithReceipts(t.Context(), key)
						if readErr != nil || len(actual) != 3 || actual[2].Order != input.Receipt.Order ||
							actual[2].Message.HistoryID != input.Receipt.Message.HistoryID {
							t.Fatal("receiptisnotexactdurablenewoccurrence", actual, readErr)
						}
					} else {
						if input.Receipt != nil || !meta.HistoryRecoveryRequired {
							t.Fatal("lostorderproofinventedreceipt", mode, input.Receipt, meta.HistoryRecoveryRequired)
						}
					}
					frame, e := s.ReadHistoryContentFrame(t.Context(), key)
					if e != nil || len(frame.Entries) != 3 || frame.IdentityStatus != ContentIdentityUnavailable {
						t.Fatal("bodylostafterproofrollback", mode, len(frame.Entries), e)
					}
					t.Logf(
						"%s/%s: body once; Ref/Binding/interval absent; receipt=%t, orderfenced=%t, sequence=%d",
						lane,
						mode,
						input.Receipt != nil,
						meta.HistoryRecoveryRequired,
						meta.HistorySequence,
					)
				})
			}
		})
	}
}

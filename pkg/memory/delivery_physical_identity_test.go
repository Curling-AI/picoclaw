package memory

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

const (
	physicalDeliveryID       = "550e8400-e29b-41d4-a716-446655440000"
	uniquePhysicalDeliveryID = "650e8400-e29b-41d4-a716-446655440000"
)

func physicalDeliverySource(t *testing.T, filtered string, skip int) (*JSONLStore, string, []byte) {
	t.Helper()
	store := newTestStore(t)
	key := "sk_v1_physical_delivery_identity"
	data := []byte(
		filtered + "\n" + `{"role":"assistant","content":"durable duplicate","delivery":{"origin":"cron:duplicate","id":"` + physicalDeliveryID + `"}}` + "\n" + `{"role":"assistant","content":"durable unique","delivery":{"origin":"cron:unique","id":"` + uniquePhysicalDeliveryID + `"}}` + "\n",
	)
	if writeErr := os.WriteFile(store.jsonlPath(key), data, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if writeErr := store.writeMeta(key, SessionMeta{Key: key, Count: 3, Skip: skip}); writeErr != nil {
		t.Fatal(writeErr)
	}
	return store, key, data
}

func requirePhysicalDeliveryMessages(t *testing.T, messages []providers.Message) {
	t.Helper()
	if len(messages) != 2 || messages[0].Content != "durable duplicate" || messages[1].Content != "durable unique" {
		t.Fatal("physical identity qualification changed readable content", messages)
	}
	if messages[0].Delivery == nil || messages[0].Delivery.Origin != "cron:duplicate" || messages[0].Delivery.ID != "" {
		t.Fatal("filtered physical duplicate gained identity or lost origin", messages[0])
	}
	if messages[1].Delivery == nil || messages[1].Delivery.Origin != "cron:unique" ||
		messages[1].Delivery.ID != uniquePhysicalDeliveryID {
		t.Fatal("unrelated unique delivery lost identity or origin", messages[1])
	}
}

func TestDeliveryPhysicalDuplicatesAcrossFiltersAndSkip(t *testing.T) {
	for name, filtered := range map[string]string{
		"malformed body":    `{"role":"assistant","content":37,"delivery":{"id":"` + physicalDeliveryID + `"}}`,
		"transient thought": `{"role":"assistant","reasoning_content":"private thought","delivery":{"id":"` + physicalDeliveryID + `"}}`,
	} {
		for _, skip := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/skip%d", name, skip), func(t *testing.T) {
				store, key, before := physicalDeliverySource(t, filtered, skip)
				t.Run("canonical", func(t *testing.T) {
					messages, readErr := store.ReadHistoryContent(t.Context(), key)
					if readErr != nil {
						t.Fatal(readErr)
					}
					requirePhysicalDeliveryMessages(t, messages)
				})
				t.Run("legacy read", func(t *testing.T) {
					messages, readErr := store.GetHistory(t.Context(), key)
					if readErr != nil {
						t.Fatal(readErr)
					}
					requirePhysicalDeliveryMessages(t, messages)
				})
				t.Run("receipts", func(t *testing.T) {
					receipts, readErr := store.ReadHistoryWithReceipts(t.Context(), key)
					if readErr != nil {
						t.Fatal(readErr)
					}
					messages := make([]providers.Message, len(receipts))
					for i, receipt := range receipts {
						messages[i] = receipt.Message
						if receipt.Order.Valid() {
							t.Fatal("legacy content gained Order", receipt)
						}
					}
					requirePhysicalDeliveryMessages(t, messages)
				})
				t.Run("frame", func(t *testing.T) {
					frame, readErr := store.ReadHistoryContentFrame(t.Context(), key)
					if readErr != nil {
						t.Fatal(readErr)
					}
					messages := make([]providers.Message, len(frame.Entries))
					for i, entry := range frame.Entries {
						messages[i] = entry.Message
					}
					requirePhysicalDeliveryMessages(t, messages)
				})
				t.Run("bounded page", func(t *testing.T) {
					page, readErr := store.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
					if readErr != nil {
						t.Fatal(readErr)
					}
					if len(page.Entries) != 2 || page.LinesRead > HistoryPageMessages ||
						page.BytesRead > len(before)+historyReadBufferBytes {
						t.Fatal("bounded page changed filtering or I/O budget", page)
					}
					for _, entry := range page.Entries {
						if entry.Message.Delivery == nil || entry.Message.Delivery.ID != "" {
							t.Fatal("page exposed unqualified identity", entry)
						}
					}
				})
				after, readErr := os.ReadFile(store.jsonlPath(key))
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatal("identity read changed durable physical rows", readErr)
				}
			})
		}
	}
}

func TestDeliveryPhysicalDuplicatesInFrozenFrames(t *testing.T) {
	for _, filtered := range []string{
		`{"role":"assistant","content":37,"delivery":{"id":"` + physicalDeliveryID + `"}}`,
		`{"role":"assistant","reasoning_content":"private thought","delivery":{"id":"` + physicalDeliveryID + `"}}`,
	} {
		for _, mode := range []string{"compaction", "mutation"} {
			t.Run(mode+filtered, func(t *testing.T) {
				store, key, _ := physicalDeliverySource(t, filtered, 0)
				frame := initializedContent(t, store, key)
				if mode == "compaction" {
					transition, commitErr := store.CommitHistoryCompaction(
						t.Context(),
						key,
						ContentCompactionPlan{
							MutationID:  uuid.NewString(),
							DomainID:    frame.DomainID,
							BeforeState: frame.StateToken,
							Retained:    frameRefs(frame),
							Summary:     "summary",
						},
					)
					if commitErr != nil {
						t.Fatal(commitErr)
					}
					for _, side := range []ContentTransitionSide{ContentBefore, ContentAfter} {
						frozen, readErr := store.ReadHistoryCompactionFrame(
							t.Context(),
							key,
							transition.MutationID,
							side,
						)
						if readErr != nil {
							t.Fatal(readErr)
						}
						messages := make([]providers.Message, len(frozen.Entries))
						for i, entry := range frozen.Entries {
							messages[i] = entry.Message
						}
						requirePhysicalDeliveryMessages(t, messages)
					}
				} else {
					result, commitErr := store.CommitHistoryContentMutation(
						t.Context(),
						key,
						typedPrefixPlan(frame, len(frame.Entries)),
					)
					if commitErr != nil {
						t.Fatal(commitErr)
					}
					frozen, readErr := store.ReadHistoryContentMutationFrame(
						t.Context(),
						key,
						result.Transition.MutationID,
						ContentBefore,
					)
					if readErr != nil {
						t.Fatal(readErr)
					}
					messages := make([]providers.Message, len(frozen.Entries))
					for i, entry := range frozen.Entries {
						messages[i] = entry.Message
					}
					requirePhysicalDeliveryMessages(t, messages)
				}
			})
		}
	}
}

func TestDeliveryPhysicalMalformedIdentityKeepsReadableContent(t *testing.T) {
	for _, value := range []string{`37`, `{"future":true}`, `null`, `"not-a-uuid"`, `"550E8400-E29B-41D4-A716-446655440000"`} {
		t.Run(value, func(t *testing.T) {
			first := `{"role":"assistant","content":"retained body","delivery":{"origin":"cron:invalid","future_field":true,"id":` + value + `}}`
			store, key, _ := physicalDeliverySource(t, first, 0)
			messages, readErr := store.ReadHistoryContent(t.Context(), key)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(messages) != 3 || messages[0].Content != "retained body" || messages[0].Delivery == nil ||
				messages[0].Delivery.Origin != "cron:invalid" ||
				messages[0].Delivery.ID != "" ||
				messages[1].Delivery.ID != physicalDeliveryID ||
				messages[2].Delivery.ID != uniquePhysicalDeliveryID {
				t.Fatal("malformed identity changed content or unrelated canonical IDs", messages)
			}
			frame, frameErr := store.ReadHistoryContentFrame(t.Context(), key)
			if frameErr != nil {
				t.Fatal(frameErr)
			}
			if len(frame.Entries) != 3 || frame.Entries[0].Message.Content != "retained body" ||
				frame.Entries[0].Message.Delivery.ID != "" ||
				frame.Entries[1].Message.Delivery.ID != physicalDeliveryID ||
				frame.Entries[2].Message.Delivery.ID != uniquePhysicalDeliveryID {
				t.Fatal("frame identity validation changed readable content or unrelated IDs", frame)
			}
		})
	}
}

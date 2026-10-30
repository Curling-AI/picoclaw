package memory

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func deliveryID(t *testing.T, message providers.Message) string {
	t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Delivery struct {
			ID string `json:"id"`
		} `json:"delivery"`
	}
	if operationErr := json.Unmarshal(data, &wire); operationErr != nil {
		t.Fatal(operationErr)
	}
	return wire.Delivery.ID
}

func TestDeliveryIdentityNewAppendsAreDistinctAndSurviveProofLoss(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_delivery_identity"
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	input := providers.Message{
		Role:     "assistant",
		Content:  "same",
		Delivery: &providers.Delivery{Origin: "cron:job", At: at},
	}
	first, err := store.AppendHistoryContentInput(t.Context(), key, input)
	if err != nil {
		t.Fatal(err)
	}
	id := deliveryID(t, first.Entry.Message)
	if uuid.Validate(id) != nil {
		t.Fatal("new durable delivery lacks occurrence identity", id)
	}
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if operationErr := store.markHistoryRecoveryRequired(key, &meta); operationErr != nil {
		t.Fatal(operationErr)
	}
	second, err := store.AppendHistoryContentInput(t.Context(), key, first.Entry.Message)
	if err != nil {
		t.Fatal(err)
	}
	if second.Receipt != nil || deliveryID(t, second.Entry.Message) == id ||
		uuid.Validate(deliveryID(t, second.Entry.Message)) != nil {
		t.Fatal("new proofless append reused occurrence identity or granted Order", second)
	}
	if deliveryID(t, input) != "" {
		t.Fatal("append mutated caller's delivery")
	}
	reopened, err := NewJSONLStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	history, err := reopened.ReadHistoryContent(t.Context(), key)
	if err != nil || len(history) != 2 {
		t.Fatal(history, err)
	}
	if deliveryID(t, history[0]) != id || deliveryID(t, history[1]) != deliveryID(t, second.Entry.Message) {
		t.Fatal("reopen changed delivery identities", history)
	}
	if _, err := reopened.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, ErrHistoryOrderUnavailable) {
		t.Fatal("delivery identity recovered Order", err)
	}
}

func TestDeliveryIdentityLegacyRewriteNeverBackfills(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_delivery_legacy"
	message := providers.Message{
		Role:     "assistant",
		Content:  "legacy",
		Delivery: &providers.Delivery{Origin: "cron:legacy"},
	}
	if operationErr := store.SetHistory(t.Context(), key, []providers.Message{message}); operationErr != nil {
		t.Fatal(operationErr)
	}
	history, err := store.GetHistory(t.Context(), key)
	if err != nil || len(history) != 1 || deliveryID(t, history[0]) != "" {
		t.Fatal("replacement backfilled delivery identity", history, err)
	}
	if operationErr := store.AddFullMessage(t.Context(), key, message); operationErr != nil {
		t.Fatal(operationErr)
	}
	history, err = store.GetHistory(t.Context(), key)
	if err != nil || len(history) != 2 || uuid.Validate(deliveryID(t, history[1])) != nil {
		t.Fatal("ordinary append lacks identity", history, err)
	}
	id := deliveryID(t, history[1])
	history[1].Content = "edited"
	if operationErr := store.SetHistory(t.Context(), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := store.TruncateHistory(t.Context(), key, 1); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := store.Compact(t.Context(), key); operationErr != nil {
		t.Fatal(operationErr)
	}
	history, err = store.ReadHistoryContent(t.Context(), key)
	if err != nil || len(history) != 1 || deliveryID(t, history[0]) != id || history[0].Content != "edited" {
		t.Fatal("rewrite changed occurrence identity", history, err)
	}
}

func TestDeliveryIdentityInvalidAndDuplicateKeepContent(t *testing.T) {
	for _, invalid := range []string{"not-a-uuid", "550E8400-E29B-41D4-A716-446655440000", "550e8400e29b41d4a716446655440000", "550e8400-e29b-41d4-a716-446655440000"} {
		t.Run(invalid, func(t *testing.T) {
			store := newTestStore(t)
			key := "sk_v1_delivery_untrusted"
			message := `{"role":"assistant","content":"durable","delivery":{"origin":"cron:job","id":"` + invalid + `"}}` + "\n"
			if operationErr := os.WriteFile(store.jsonlPath(key), []byte(message+message), 0o600); operationErr != nil {
				t.Fatal(operationErr)
			}
			if operationErr := store.writeMeta(key, SessionMeta{Key: key, Count: 2}); operationErr != nil {
				t.Fatal(operationErr)
			}
			history, err := store.ReadHistoryContent(t.Context(), key)
			if err != nil || len(history) != 2 {
				t.Fatal(history, err)
			}
			frame, err := store.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || len(frame.Entries) != 2 {
				t.Fatal(frame, err)
			}
			for i, msg := range history {
				if deliveryID(t, msg) != "" || deliveryID(t, frame.Entries[i].Message) != "" ||
					msg.Content != "durable" ||
					msg.Delivery.Origin != "cron:job" {
					t.Fatal("untrusted identity gained authority or lost content", msg, frame)
				}
			}
		})
	}
}

func TestDeliveryIdentityMalformedOptionalFieldKeepsBody(t *testing.T) {
	for _, value := range []string{`null`, `37`, `true`, `[]`, `{}`, `""`, `"00000000-0000-0000-0000-000000000000"`} {
		t.Run(value, func(t *testing.T) {
			store := newTestStore(t)
			key := "sk_v1_delivery_shape"
			raw := `{"role":"assistant","content":"durable","delivery":{"origin":"cron:job","id":` + value + `}}` + "\n"
			if operationErr := os.WriteFile(store.jsonlPath(key), []byte(raw), 0o600); operationErr != nil {
				t.Fatal(operationErr)
			}
			if operationErr := store.writeMeta(key, SessionMeta{Key: key, Count: 1}); operationErr != nil {
				t.Fatal(operationErr)
			}
			history, err := store.ReadHistoryContent(t.Context(), key)
			if err != nil || len(history) != 1 || history[0].Content != "durable" ||
				history[0].Delivery.Origin != "cron:job" ||
				deliveryID(t, history[0]) != "" {
				t.Fatal("malformed optional ID discarded durable body", history, err)
			}
		})
	}
}

func TestDeliveryIdentityDuplicateScopeIncludesSkippedRows(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_delivery_scope"
	raw := `{"role":"assistant","content":"durable","delivery":{"origin":"cron:job","id":"550e8400-e29b-41d4-a716-446655440000"}}` + "\n"
	if operationErr := os.WriteFile(store.jsonlPath(key), []byte(raw+raw), 0o600); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := store.writeMeta(key, SessionMeta{Key: key, Count: 2, Skip: 1}); operationErr != nil {
		t.Fatal(operationErr)
	}
	history, err := store.ReadHistoryContent(t.Context(), key)
	if err != nil || len(history) != 1 || deliveryID(t, history[0]) != "" {
		t.Fatal("skipped duplicate gained authority", history, err)
	}
	frame, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || len(frame.Entries) != 1 || deliveryID(t, frame.Entries[0].Message) != "" {
		t.Fatal("frame ignored skipped duplicate", frame, err)
	}
	other := "sk_v1_delivery_other"
	if operationErr := os.WriteFile(store.jsonlPath(other), []byte(raw), 0o600); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := store.writeMeta(other, SessionMeta{Key: other, Count: 1}); operationErr != nil {
		t.Fatal(operationErr)
	}
	history, err = store.ReadHistoryContent(t.Context(), other)
	if err != nil || len(history) != 1 || deliveryID(t, history[0]) == "" {
		t.Fatal("unrelated session invalidated a unique ID", history, err)
	}
}

func TestDeliveryIdentityCheckedEditKeepsOccurrenceAndAdvancesRevision(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_delivery_checked_edit"
	first, err := store.AppendHistoryReceipt(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "before", Delivery: &providers.Delivery{Origin: "cron:job"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	frame := initializedContent(t, store, key)
	ref := *frame.Entries[0].Ref
	id := deliveryID(t, frame.Entries[0].Message)
	if id != deliveryID(t, first.Message) || !validDeliveryID(id) {
		t.Fatal("frame lost durable delivery identity", frame)
	}
	frame.Entries[0].Message.Content = "after"
	after, err := store.CommitHistoryContentEdit(
		t.Context(),
		key,
		ContentEditPlan{
			DomainID:    frame.DomainID,
			BeforeState: frame.StateToken,
			Entries:     frame.Entries,
			Summary:     frame.Summary,
		},
	)
	if err != nil || len(after.Entries) != 1 || deliveryID(t, after.Entries[0].Message) != id ||
		after.Entries[0].Ref.OccurrenceID != ref.OccurrenceID ||
		after.Entries[0].Ref.RevisionID == ref.RevisionID {
		t.Fatal("checked edit replaced delivery occurrence or retained old revision", after, err)
	}
}

func TestDeliveryIdentityPageOmitsUnqualifiedIDWithoutChangingSource(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_delivery_page"
	receipt, err := store.AppendHistoryReceipt(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "durable", Delivery: &providers.Delivery{Origin: "cron:job"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	id := receipt.Message.Delivery.ID
	page, err := store.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Message.Delivery.ID != "" ||
		page.Entries[0].Message.Delivery.Origin != "cron:job" {
		t.Fatal("page exposed unqualified identity or lost content", page, err)
	}
	history, err := store.ReadHistoryContent(t.Context(), key)
	if err != nil || len(history) != 1 || history[0].Delivery.ID != id || !validDeliveryID(id) {
		t.Fatal("page projection changed durable delivery identity", history, err)
	}
}

func TestDeliveryIdentityDuplicateDoesNotInvalidateOrder(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_delivery_receipt"
	for i := 0; i < 2; i++ {
		if _, err := store.AppendHistoryReceipt(
			t.Context(),
			key,
			providers.Message{Role: "assistant", Content: "same", Delivery: &providers.Delivery{Origin: "cron:job"}},
		); err != nil {
			t.Fatal(err)
		}
	}
	history, err := store.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	second := *history[1].Delivery
	second.ID = history[0].Delivery.ID
	history[1].Delivery = &second
	if operationErr := store.SetHistory(t.Context(), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := store.TruncateHistory(t.Context(), key, 1); operationErr != nil {
		t.Fatal(operationErr)
	}
	receipts, err := store.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(receipts) != 1 || !receipts[0].Order.Valid() ||
		receipts[0].Order != messageHistoryOrder(history[1]) ||
		receipts[0].Message.Delivery.ID != "" {
		t.Fatal("duplicate identity gained authority or invalidated independent Order", receipts, err)
	}
}

func TestSkippedDeliveryIdentityDoesNotDecodeDiscardedBody(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-446655440000"
	for _, test := range []struct{ raw, want string }{
		{`{"delivery":{"id":"` + id + `"},"content":37,"tool_calls":false}`, id},
		{`{"delivery":{"id":37},"content":"ignored"}`, ""},
		{`{"delivery":null}`, ""},
		{`{broken`, ""},
	} {
		if actual := physicalDeliveryIDFromRow([]byte(test.raw)); actual != test.want {
			t.Fatal(
				"skipped delivery metadata decoded discarded content or accepted malformed identity",
				actual,
				test.want,
			)
		}
	}
}

package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestHistoryOrderReceiptIsExactPersistentAndIndependentOfInput(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:ordered"
	at := time.Unix(100, 0).UTC()
	input := providers.Message{
		Role:      "assistant",
		Content:   "same timestamp",
		CreatedAt: &at,
		HistoryID: newHistoryID(),
		Media:     []string{"original-media"},
		ToolCalls: []providers.ToolCall{
			{ID: "call", Type: "function", Function: &providers.FunctionCall{Name: "original-tool", Arguments: `{}`}},
		},
	}
	first, err := s.AppendHistoryReceipt(t.Context(), key, input)
	if err != nil || !first.Order.Valid() || first.Order.Sequence != 1 || first.Message.HistoryID == "" {
		t.Fatal("append did not return a committed proof", first, err)
	}
	input.Media[0], input.ToolCalls[0].Function.Name = "mutated-media", "mutated-tool"
	if first.Message.Media[0] != "original-media" || first.Message.ToolCalls[0].Function.Name != "original-tool" {
		t.Fatal("receipt aliases the caller's input", first)
	}
	encoded, err := json.Marshal(first.Message)
	if err != nil || bytes.Contains(encoded, []byte(first.Order.StreamID)) ||
		bytes.Contains(encoded, []byte("history_")) {
		t.Fatal("order evidence leaked into provider/session JSON", string(encoded), err)
	}
	reopened, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	read, err := reopened.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 1 || !reflect.DeepEqual(read[0], first) {
		t.Fatal("reopen lost the exact persisted receipt", read, err)
	}
	second, err := reopened.AppendHistoryReceipt(t.Context(), key, input)
	if err != nil || second.Order.StreamID != first.Order.StreamID || second.Order.Sequence != 2 {
		t.Fatal("restart/timestamp changed producer order", second, err)
	}
	if first.Message.HistoryID == input.HistoryID || second.Message.HistoryID == input.HistoryID ||
		first.Message.HistoryID == second.Message.HistoryID {
		t.Fatal("two real appends reused an occurrence identity from their argument", input.HistoryID, first, second)
	}
}

func TestHistoryOrderPreservesContentProofAndNeverRewindsOnEditRollbackOrCompact(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:ordered-edit"
	before := make([]CommittedHistoryMessage, 4)
	for index := range before {
		var err error
		before[index], err = s.AppendHistoryReceipt(
			t.Context(),
			key,
			providers.Message{Role: "assistant", Content: fmt.Sprint(index)},
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	original := append([]providers.Message(nil), history...)
	history[1].Content = "edited with the same identity"
	if operationErr := s.SetHistory(t.Context(), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	edited, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(edited) != 4 {
		t.Fatal(err)
	}
	for index, sequence := range []uint64{1, 5, 3, 4} {
		if edited[index].Order.Sequence != sequence || edited[index].Order.StreamID != before[index].Order.StreamID {
			t.Fatal("edit reused a proof or changed unchanged content's proof", edited)
		}
	}
	if edited[1].Message.HistoryID != before[1].Message.HistoryID || edited[1].Order == before[1].Order ||
		edited[1].Message.Content == before[1].Message.Content {
		t.Fatal("modified content retained the old read receipt", edited[1], before[1])
	}
	if operationErr := s.SetHistory(t.Context(), key, original[:2]); operationErr != nil {
		t.Fatal(operationErr)
	}
	restored, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(restored) != 2 || restored[1].Order.Sequence != 6 ||
		restored[1].Message.Content != original[1].Content {
		t.Fatal("logical rollback reused an earlier content proof", restored, err)
	}
	tail, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "tail"})
	if err != nil || tail.Order.Sequence != 7 {
		t.Fatal(tail, err)
	}
	if operationErr := s.TruncateHistory(t.Context(), key, 2); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.Compact(t.Context(), key); operationErr != nil {
		t.Fatal(operationErr)
	}
	compacted, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(compacted) != 2 || compacted[0].Order != restored[1].Order ||
		compacted[1].Order != tail.Order {
		t.Fatal("truncate/compaction changed content order", compacted, err)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.AppendHistoryReceipt(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "after compact/reopen"},
	)
	if err != nil || next.Order.Sequence != 8 || next.Order.StreamID != tail.Order.StreamID {
		t.Fatal("highwater rewound after compaction/reopen", next, err)
	}
}

func TestHistoryOrderPhysicalDeleteRetiresCachedStream(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:deleted-order"
	old, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "user", Content: "old"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.jsonlPath(key), s.metaPath(key)} {
		if removeErr := os.Remove(path); removeErr != nil {
			t.Fatal(removeErr)
		}
	}
	next, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "user", Content: "new"})
	if err != nil || next.Order.Sequence != 1 || next.Order.StreamID == old.Order.StreamID {
		t.Fatal("delete/recreate reused an old cached stream", next, err)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 1 || read[0].Order != next.Order {
		t.Fatal("new stream did not replace deleted content", read, err)
	}
}

func TestHistoryOrderLegacyRemainsUnproven(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:legacy-order"
	raw := []byte("{\"role\":\"user\",\"content\":\"legacy\"}\n")
	if err := os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMeta(key, SessionMeta{Key: key, Count: 1}); err != nil {
		t.Fatal(err)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 1 || read[0].Order != (HistoryOrderProof{}) {
		t.Fatal("legacy position was converted into ordering evidence", read, err)
	}
	appended, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "ordered"})
	if err != nil || appended.Order.Sequence != 1 {
		t.Fatal(appended, err)
	}
	read, err = s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 2 || read[0].Order != (HistoryOrderProof{}) || read[1].Order != appended.Order {
		t.Fatal("rollout assigned order to the legacy prefix", read, err)
	}
	stored, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.HasPrefix(stored, raw) {
		t.Fatal("rollout rewrote legacy content", err)
	}
}

func TestHistoryOrderLegacyCorruptRowsRemainFilteredAfterRollout(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:legacy-corrupt-order"
	raw := []byte(
		"{broken legacy row\n{\"role\":\"user\",\"content\":\"legacy\",\"created_at\":\"2000-01-01T00:00:00Z\"}\n",
	)
	if err := os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMeta(key, SessionMeta{Key: key, Count: 2}); err != nil {
		t.Fatal(err)
	}
	appended, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "ordered"})
	if err != nil || appended.Order.Sequence != 1 {
		t.Fatal(appended, err)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 2 || read[0].LegacyBeforeStreamID != appended.Order.StreamID ||
		read[0].Order != (HistoryOrderProof{}) || read[1].Order != appended.Order {
		t.Fatal("a proven legacy corrupt row made the retained history unavailable", read, err)
	}
	history := []providers.Message{read[0].Message, read[1].Message}
	if operationErr := s.SetHistory(t.Context(), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	rewritten, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || !reflect.DeepEqual(rewritten, read) {
		t.Fatal("filtering legacy corruption changed retained proofs", rewritten, err)
	}
}

func TestHistoryOrderImportBeyondCheckpointRequiresExplicitNewSession(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:uncommitted-import-order"
	first, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "committed"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(s.jsonlPath(key), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	data := fmt.Appendf(
		nil,
		"{\"role\":\"assistant\",\"content\":\"uncommitted old writer\",\"history_id\":%q}\n",
		newHistoryID(),
	)
	if _, writeErr := file.Write(data); writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	// The content reader can see a complete external tail. Its source stays
	// fenced; deliberate recovery into a fresh canonical session creates its
	// own proofs without guessing continuity with the old stream.
	history, err := s.GetHistory(t.Context(), key)
	if err != nil || len(history) != 2 {
		t.Fatal(history, err)
	}
	if operationErr := s.SetHistory(t.Context(), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	recoveredKey := "sk_v1_recovered_external_tail"
	if operationErr := s.SetHistory(t.Context(), recoveredKey, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), recoveredKey)
	if err != nil || len(read) != 2 || !read[0].Order.Valid() || read[0].Order.Sequence != 1 ||
		read[1].Order.Sequence != 2 || read[0].Order.StreamID != read[1].Order.StreamID ||
		read[0].Order.StreamID == first.Order.StreamID || read[1].LegacyBeforeStreamID != "" {
		t.Fatal("explicit recovery guessed old-stream continuity", read, err)
	}
	if _, readErr := s.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(readErr, ErrHistoryReceiptUnavailable) {
		t.Fatal("new-session recovery cleared the old source's fence", readErr)
	}
}

func TestHistoryOrderLegacyUpgradeDoesNotAttestAnUncommittedTail(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:legacy-uncommitted-order"
	raw := []byte(
		"{\"role\":\"assistant\",\"content\":\"committed legacy\"}\n{\"role\":\"assistant\",\"content\":\"uncommitted tail\"}\n",
	)
	if err := os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMeta(key, SessionMeta{Key: key, Count: 1}); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "next"})
	if !errors.Is(err, ErrHistoryOrderUnavailable) || receipt.Order != (HistoryOrderProof{}) {
		t.Fatal("implicit legacy upgrade confirmed a tail outside its checkpoint", receipt, err)
	}
	stored, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatal("failed upgrade changed legacy data", err)
	}
	// The rejected append latches recovery. Content stays readable; deliberate
	// import into a new session creates its own proofs for the trusted content.
	history, err := s.GetHistory(t.Context(), key)
	if err != nil || len(history) != 2 {
		t.Fatal(history, err)
	}
	if operationErr := s.SetHistory(t.Context(), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	recoveredKey := "sk_v1_recovered_legacy_tail"
	if operationErr := s.SetHistory(t.Context(), recoveredKey, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), recoveredKey)
	if err != nil || len(read) != 2 || !read[0].Order.Valid() || read[0].Order.Sequence != 1 ||
		read[1].Order.Sequence != 2 || read[0].Order.StreamID != read[1].Order.StreamID ||
		read[0].LegacyBeforeStreamID != "" || read[1].LegacyBeforeStreamID != "" {
		t.Fatal("explicit recovery laundered the tail into genuine legacy", read, err)
	}
}

func TestHistoryOrderMissingMetadataReadsOnlyUnboundLegacy(t *testing.T) {
	s := newTestStore(t)
	key := "sk_v1_written_before_audit_fields"
	raw := []byte(
		"{\"role\":\"user\",\"content\":\"before audit\"}\n{\"role\":\"assistant\",\"content\":\"legacy reply\"}\n",
	)
	if err := os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 2 || read[0].Message.Content != "before audit" ||
		read[1].Message.Content != "legacy reply" {
		t.Fatal("missing legacy metadata hid existing content", read, err)
	}
	for _, receipt := range read {
		if receipt.Order != (HistoryOrderProof{}) || receipt.LegacyBeforeStreamID != "" {
			t.Fatal("missing metadata invented legacy stream provenance", receipt)
		}
	}
	if _, err := os.Stat(s.metaPath(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reading unbound history wrote metadata", err)
	}
}

func TestHistoryOrderMissingMetadataRejectsResidualModernEvidence(t *testing.T) {
	stream := newHistoryID()
	for _, field := range []string{
		fmt.Sprintf(`"history_order":{"version":1,"streamId":%q,"sequence":1}`, stream),
		fmt.Sprintf(`"history_legacy_before_stream_id":%q`, stream),
		`"history_order":null`,
	} {
		t.Run(field, func(t *testing.T) {
			s := newTestStore(t)
			key := "legacy-without-metadata"
			raw := []byte("{\"role\":\"assistant\",\"content\":\"residual evidence\"," + field + "}\n")
			if err := os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, ErrHistoryOrderUnavailable) {
				t.Fatal("missing metadata downgraded modern evidence to unbound legacy", err)
			}
		})
	}
}

func TestHistoryOrderCompactDoesNotAttestAnUncommittedTail(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:compact-uncommitted-order"
	for _, content := range []string{"drop", "keep"} {
		if _, err := s.AppendHistoryReceipt(
			t.Context(),
			key,
			providers.Message{Role: "assistant", Content: content},
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TruncateHistory(t.Context(), key, 1); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(s.jsonlPath(key), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, writeErr := file.WriteString("{\"role\":\"assistant\",\"content\":\"uncommitted tail\"}\n"); writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if compactErr := s.Compact(t.Context(), key); compactErr != nil {
		t.Fatal("unproven order froze content compaction", compactErr)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || bytes.Count(after, []byte{'\n'}) != 2 {
		t.Fatal("content compaction did not retain the requested physical window", err)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if !errors.Is(err, ErrHistoryReceiptUnavailable) || len(read) != 0 {
		t.Fatal("content compaction forgot its recovery fence", read, err)
	}
	visible, err := s.GetHistory(t.Context(), key)
	if err != nil || len(visible) != 2 || visible[0].Content != "keep" || visible[1].Content != "uncommitted tail" {
		t.Fatal("content compaction hid durable content", visible, err)
	}
	if order := messageHistoryOrder(visible[0]); !order.Valid() || order.Sequence != 2 {
		t.Fatal("content compaction lost the retained private evidence", order)
	}
	if order := messageHistoryOrder(visible[1]); order != (HistoryOrderProof{}) {
		t.Fatal("content compaction attested an external tail", order)
	}
}

func TestHistoryOrderLegacyOriginSurvivesEditAndRemovalOfAllActiveV1(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:legacy-origin"
	raw := []byte(
		"{\"role\":\"assistant\",\"content\":\"legacy A\"}\n{\"role\":\"assistant\",\"content\":\"legacy B\"}\n",
	)
	if err := os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMeta(key, SessionMeta{Key: key, Count: 2}); err != nil {
		t.Fatal(err)
	}
	v1, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "first v1"})
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 3 || read[0].LegacyBeforeStreamID != v1.Order.StreamID ||
		read[1].LegacyBeforeStreamID != v1.Order.StreamID || read[2].LegacyBeforeStreamID != "" {
		t.Fatal("atomic legacy origin was not established by the first v1 commit", read, err)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	history[0].Content = "edited legacy A"
	if operationErr := s.SetHistory(t.Context(), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	read, err = s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || read[0].Order.Sequence != 2 || read[1].Order != (HistoryOrderProof{}) ||
		read[1].LegacyBeforeStreamID != v1.Order.StreamID || read[2].Order.Sequence != 1 {
		t.Fatal("physical [edit2, legacy, v1] inverted the legacy origin", read, err)
	}
	history, err = s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if operationErr := s.SetHistory(t.Context(), key, history[1:2]); operationErr != nil {
		t.Fatal(operationErr)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	read, err = s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 1 || read[0].Order != (HistoryOrderProof{}) ||
		read[0].LegacyBeforeStreamID != v1.Order.StreamID {
		t.Fatal("all-legacy active history lost its persisted before-v1 provenance", read, err)
	}
	encoded, err := json.Marshal(read[0])
	if err != nil || bytes.Contains(encoded, []byte(v1.Order.StreamID)) {
		t.Fatal("private legacy provenance leaked through generic JSON", string(encoded), err)
	}
	meta, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	meta.HistoryOrderVersion, meta.HistoryStreamID, meta.HistorySequence = 0, "", 0
	meta.HistoryLegacyFileGeneration, meta.HistoryLegacyBytes = "", 0
	if operationErr := s.writeMeta(key, meta); operationErr != nil {
		t.Fatal(operationErr)
	}
	if _, readErr := s.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(readErr, ErrHistoryOrderUnavailable) {
		t.Fatal("lost stream metadata silently reclassified a persisted legacy origin", readErr)
	}
}

func TestHistoryOrderMissingModernProofIsNotGenuineLegacy(t *testing.T) {
	for _, lostMeta := range []bool{false, true} {
		t.Run(fmt.Sprint(lostMeta), func(t *testing.T) {
			s := newTestStore(t)
			key := "agent:main:old-writer"
			if _, err := s.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "modern"},
			); err != nil {
				t.Fatal(err)
			}
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			if lostMeta {
				// Lost/old-writer metadata must not bootstrap a new stream over a
				// record that still carries evidence of an existing v1 stream.
				meta.HistoryOrderVersion, meta.HistoryStreamID, meta.HistorySequence = 0, "", 0
				meta.HistoryLegacyFileGeneration, meta.HistoryLegacyBytes = "", 0
			} else {
				// A modern record beyond the initial legacy fence lost its proof.
				data, readErr := os.ReadFile(s.jsonlPath(key))
				if readErr != nil {
					t.Fatal(readErr)
				}
				var record map[string]json.RawMessage
				if decodeErr := json.Unmarshal(bytes.TrimSpace(data), &record); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				delete(record, "history_order")
				data, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, '\n')
				if writeErr := os.WriteFile(s.jsonlPath(key), data, 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
				size := int64(len(data))
				meta.HistoryBytes = &size
			}
			if writeErr := s.writeMeta(key, meta); writeErr != nil {
				t.Fatal(writeErr)
			}
			if _, readErr := s.ReadHistoryWithReceipts(
				t.Context(),
				key,
			); !errors.Is(
				readErr,
				ErrHistoryOrderUnavailable,
			) {
				t.Fatal("old/missing proof was silently converted to an attested legacy entry", readErr)
			}
		})
	}
}

func TestHistoryOrderRewriteCannotReuseAnOldLegacyByteFence(t *testing.T) {
	for _, row := range []int{0, 1} {
		t.Run(fmt.Sprint(row), func(t *testing.T) {
			s := newTestStore(t)
			key := "agent:main:rewritten-legacy-fence"
			raw := []byte(
				"{\"role\":\"assistant\",\"content\":\"" + strings.Repeat(
					"x",
					2048,
				) + "\"}\n{\"role\":\"assistant\",\"content\":\"legacy\"}\n",
			)
			if err := os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.writeMeta(key, SessionMeta{Key: key, Count: 2}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "v1"},
			); err != nil {
				t.Fatal(err)
			}
			history, err := s.GetHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			history[0].Content = "short edit"
			if operationErr := s.SetHistory(t.Context(), key, history); operationErr != nil {
				t.Fatal(operationErr)
			}
			data, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
			var record map[string]json.RawMessage
			if decodeErr := json.Unmarshal(lines[row], &record); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			delete(record, "history_order")
			delete(record, "history_legacy_before_stream_id")
			lines[row], err = json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			data = append(bytes.Join(lines, []byte("\n")), '\n')
			if writeErr := os.WriteFile(s.jsonlPath(key), data, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			if meta.HistoryFileGeneration == meta.HistoryLegacyFileGeneration ||
				int64(len(lines[0])+1) > meta.HistoryLegacyBytes {
				t.Fatal("fixture does not distinguish file lineage from byte position", meta)
			}
			size := int64(len(data))
			meta.HistoryBytes = &size
			if err := s.writeMeta(key, meta); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, ErrHistoryOrderUnavailable) {
				t.Fatal("rewrite manufactured legacy origin from an old byte fence", err)
			}
		})
	}
}

func TestHistoryOrderConcurrentCommitsUseOnePersistentCounter(t *testing.T) {
	s := newTestStore(t)
	const count = 32
	const key = "agent:main:concurrent-order"
	var group sync.WaitGroup
	results := make(chan CommittedHistoryMessage, count)
	errorsOut := make(chan error, count)
	for index := range count {
		group.Add(1)
		go func() {
			defer group.Done()
			receipt, err := s.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: fmt.Sprint(index)},
			)
			results <- receipt
			errorsOut <- err
		}()
	}
	group.Wait()
	close(results)
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[uint64]bool{}
	stream := ""
	for receipt := range results {
		if stream == "" {
			stream = receipt.Order.StreamID
		}
		if !receipt.Order.Valid() || receipt.Order.StreamID != stream || seen[receipt.Order.Sequence] {
			t.Fatal("concurrent receipt collided", receipt)
		}
		seen[receipt.Order.Sequence] = true
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != count {
		t.Fatal(read, err)
	}
	for index, receipt := range read {
		if receipt.Order.Sequence != uint64(index+1) {
			t.Fatal("source append order differs from its receipts", read)
		}
	}
}

func TestHistoryReceiptUnknownCommitNeverRollsBackAConfirmedSequence(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires POSIX non-root permissions")
	}
	s := newTestStore(t)
	const key = "agent:main:unknown-receipt-commit"
	first, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "first"})
	if err != nil {
		t.Fatal(err)
	}
	metaPath := s.metaPath(key)
	t.Cleanup(func() { _ = os.Chmod(metaPath, 0o644) })
	committed := false
	sentinel := errors.New("commit happened but metadata read is unavailable")
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == metaPath {
			if !committed {
				if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
					return writeErr
				}
				committed = true
				if permissionErr := os.Chmod(path, 0); permissionErr != nil {
					return permissionErr
				}
			}
			return sentinel
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	attempt, err := s.AppendHistoryReceipt(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "committed but temporarily unknown"},
	)
	if err == nil || attempt.Order.Valid() {
		t.Fatal("unknown decision emitted a live receipt", attempt, err)
	}
	if permissionErr := os.Chmod(metaPath, 0o644); permissionErr != nil {
		t.Fatal(permissionErr)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 2 || read[1].Order.Sequence != 2 || read[1].Order.StreamID != first.Order.StreamID {
		t.Fatal("recovery rolled back a confirmed commit while its metadata was unreadable", read, err)
	}
}

func TestHistoryReceiptRequiresDurableDecisionOnCleanupFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires POSIX directory permissions for a non-root user")
	}
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint(durable), func(t *testing.T) {
			s := newTestStore(t)
			key := "agent:main:receipt-cleanup"
			if _, err := s.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "first"},
			); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(s.dir, 0o755) })
			writeFailure := errors.New("metadata write reported an uncertain result")
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
					return writeErr
				}
				if path == s.metaPath(key) {
					if durable {
						return writeFailure
					}
					return os.Chmod(s.dir, 0o555)
				}
				if durable && path == s.historyDecisionPath(key) {
					// The decision can be synced, while unlink is now forbidden.
					return os.Chmod(s.dir, 0o555)
				}
				return nil
			}
			receipt, err := s.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "confirmed once"},
			)
			if durable {
				if err != nil || !receipt.Order.Valid() || receipt.Order.Sequence != 2 ||
					receipt.Message.Content != "confirmed once" {
					t.Fatal("durable NEW cleanup failure requested a retry or lost its receipt", receipt, err)
				}
			} else if err == nil || receipt.Order != (HistoryOrderProof{}) {
				t.Fatal("readable NEW metadata was acknowledged without a durable decision", receipt, err)
			}
			if _, statErr := os.Stat(s.historyRecoveryPath(key)); statErr != nil {
				t.Fatal("cleanup failure discarded its recovery fence", statErr)
			}
			if _, cached := s.cachedMeta(sanitizeKey(key)); cached {
				t.Fatal("pending cleanup exposed the metadata cache")
			}
			if permissionErr := os.Chmod(s.dir, 0o755); permissionErr != nil {
				t.Fatal(permissionErr)
			}
			s.writeFile = nil
			next, err := s.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "after cleanup"},
			)
			if err != nil || next.Order.Sequence != 3 {
				t.Fatal("cleanup recovery reused a committed sequence", next, err)
			}
			read, err := s.ReadHistoryWithReceipts(t.Context(), key)
			if err != nil || len(read) != 3 || read[1].Message.Content != "confirmed once" ||
				read[1].Order.Sequence != 2 ||
				(durable && !reflect.DeepEqual(read[1], receipt)) {
				t.Fatal("recovery lost or duplicated the exact confirmed occurrence", read, err)
			}
		})
	}
}

func TestHistoryReceiptRecoveryRequiresTheExactOrderCheckpoint(t *testing.T) {
	for name, alter := range map[string]func(*SessionMeta){
		"version": func(meta *SessionMeta) { meta.HistoryOrderVersion = 0 },
		"stream":  func(meta *SessionMeta) { meta.HistoryStreamID = newHistoryID() },
		"counter": func(meta *SessionMeta) { meta.HistorySequence-- },
		"legacy generation": func(meta *SessionMeta) {
			meta.HistoryLegacyFileGeneration = newHistoryID()
		},
		"legacy bytes": func(meta *SessionMeta) { meta.HistoryLegacyBytes++ },
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			key := "agent:main:receipt-checkpoint"
			first, err := s.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "first"},
			)
			if err != nil {
				t.Fatal(err)
			}
			var expected []byte
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path != s.metaPath(key) {
					return fileutil.WriteFileAtomic(path, data, mode)
				}
				expected = append([]byte(nil), data...)
				var partial SessionMeta
				if decodeErr := json.Unmarshal(data, &partial); decodeErr != nil {
					return decodeErr
				}
				alter(&partial)
				changed, marshalErr := json.Marshal(partial)
				if marshalErr != nil {
					return marshalErr
				}
				return errors.Join(
					errors.New("order checkpoint was only partially written"),
					fileutil.WriteFileAtomic(path, changed, mode),
				)
			}
			receipt, err := s.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "uncertain"},
			)
			if err == nil || receipt.Order != (HistoryOrderProof{}) {
				t.Fatal("recovery acknowledged metadata that lost order provenance", receipt, err)
			}
			if _, statErr := os.Stat(s.historyRecoveryPath(key)); statErr != nil {
				t.Fatal("ambiguous order checkpoint lost its recovery journal", statErr)
			}
			s.writeFile = nil
			if writeErr := fileutil.WriteFileAtomic(s.metaPath(key), expected, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			reopened, err := NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			read, err := reopened.ReadHistoryWithReceipts(t.Context(), key)
			if err != nil || len(read) != 2 || read[0].Order != first.Order || read[1].Order.Sequence != 2 {
				t.Fatal("repair rolled back the committed content or reused its sequence", read, err)
			}
		})
	}
}

func TestHistoryOrderExhaustionDoesNotWriteOrRotateStream(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:order-capacity"
	first, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "user", Content: "first"})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	meta.HistorySequence = MaxHistorySequence
	if operationErr := s.writeMeta(key, meta); operationErr != nil {
		t.Fatal(operationErr)
	}
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "overflow"})
	if !errors.Is(err, ErrHistoryOrderExhausted) || receipt.Order.Valid() {
		t.Fatal("exhausted counter produced a receipt", receipt, err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("capacity failure changed source bytes", err)
	}
	meta, err = s.readMeta(key)
	if err != nil || meta.HistorySequence != MaxHistorySequence || meta.HistoryStreamID != first.Order.StreamID {
		t.Fatal("capacity failure reset the stream/highwater", meta, err)
	}
}

func TestHistoryReceiptFollowsJournalCommitDecision(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			s := newTestStore(t)
			key := "agent:main:receipt-failure"
			first, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "user", Content: "first"})
			if err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("metadata write failed")
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path == s.metaPath(key) {
					if committed {
						if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
							return writeErr
						}
					}
					return sentinel
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			attempt, err := s.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "attempt"},
			)
			if committed && (err != nil || !attempt.Order.Valid() || attempt.Order.Sequence != 2) {
				t.Fatal("confirmed NEW state did not return its exact receipt", attempt, err)
			}
			if !committed && (!errors.Is(err, sentinel) || attempt.Order.Valid()) {
				t.Fatal("OLD rollback emitted a receipt", attempt, err)
			}
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			read, err := s.ReadHistoryWithReceipts(t.Context(), key)
			wantCount, wantSequence := 1, uint64(2)
			if committed {
				wantCount, wantSequence = 2, 3
			}
			if err != nil || len(read) != wantCount || read[0].Order != first.Order {
				t.Fatal("recovery changed the authoritative receipt prefix", read, err)
			}
			next, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "next"})
			if err != nil || next.Order.Sequence != wantSequence || next.Order.StreamID != first.Order.StreamID {
				t.Fatal("recovery reused a committed sequence or lost the stream", next, err)
			}
		})
	}
}

func TestHistoryOrderInvalidOrForeignProofIsNotLegacyZero(t *testing.T) {
	for _, kind := range []string{"version", "foreign", "zero", "null", "overflow"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			key := "agent:main:invalid-order"
			first, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "user", Content: "first"})
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]json.RawMessage
			if decodeErr := json.Unmarshal(bytes.TrimSpace(data), &record); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			order := first.Order
			switch kind {
			case "version":
				order.Version = 2
			case "foreign":
				order.StreamID = newHistoryID()
			case "zero":
				order.Sequence = 0
			case "overflow":
				order.Sequence = MaxHistorySequence + 1
			}
			encoded, err := json.Marshal(order)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "null" {
				encoded = []byte("null")
			}
			record["history_order"] = encoded
			data, err = json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, '\n')
			if writeErr := os.WriteFile(s.jsonlPath(key), data, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			size := int64(len(data))
			meta.HistoryBytes = &size
			if writeErr := s.writeMeta(key, meta); writeErr != nil {
				t.Fatal(writeErr)
			}
			if _, readErr := s.ReadHistoryWithReceipts(
				t.Context(),
				key,
			); !errors.Is(
				readErr,
				ErrHistoryOrderUnavailable,
			) {
				t.Fatal("invalid proof was silently downgraded to a legacy record", readErr)
			}
		})
	}
}

func TestHistoryOrderAliasPromotionAllocatesCanonicalProof(t *testing.T) {
	s := newTestStore(t)
	alias, canonical := "agent:legacy:chat", "canonical-order"
	aliasReceipt, err := s.AppendHistoryReceipt(
		t.Context(),
		alias,
		providers.Message{Role: "user", Content: "alias content"},
	)
	if err != nil {
		t.Fatal(err)
	}
	oldCanonical, err := s.AppendHistoryReceipt(
		t.Context(),
		canonical,
		providers.Message{Role: "user", Content: "cleared canonical content"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if operationErr := s.TruncateHistory(t.Context(), canonical, 0); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.UpsertSessionMeta(t.Context(), canonical, nil, []string{alias}); operationErr != nil {
		t.Fatal(operationErr)
	}
	promoted, err := s.PromoteAliasHistory(t.Context(), canonical, nil, []string{alias})
	if err != nil || !promoted {
		t.Fatal(promoted, err)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), alias)
	if err != nil || len(read) != 1 || read[0].Order.StreamID != oldCanonical.Order.StreamID ||
		read[0].Order.Sequence != 2 || read[0].Order.StreamID == aliasReceipt.Order.StreamID ||
		read[0].Message.HistoryID != aliasReceipt.Message.HistoryID {
		t.Fatal("alias proof was mistaken for canonical order", read, err)
	}
	next, err := s.AppendHistoryReceipt(
		t.Context(),
		alias,
		providers.Message{Role: "assistant", Content: "canonical append"},
	)
	if err != nil || next.Order.StreamID != oldCanonical.Order.StreamID || next.Order.Sequence != 3 {
		t.Fatal("alias append did not use the canonical stream", next, err)
	}
	if operationErr := s.UpsertSessionMeta(t.Context(), "another-owner", nil, []string{alias}); operationErr != nil {
		t.Fatal(operationErr)
	}
	if _, appendErr := s.AppendHistoryReceipt(
		t.Context(),
		alias,
		providers.Message{Role: "assistant", Content: "ambiguous"},
	); !errors.Is(
		appendErr,
		ErrHistoryOrderUnavailable,
	) {
		t.Fatal("ambiguous alias ownership produced a receipt", appendErr)
	}
	providerJSON, err := json.Marshal(read[0].Message)
	if err != nil || strings.Contains(string(providerJSON), oldCanonical.Order.StreamID) {
		t.Fatal("canonical order leaked to a public/provider projection", err)
	}
}

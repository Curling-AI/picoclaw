package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestReadRecoveryIncludesSkippedProofAndSurvivesInterruptedCallback(t *testing.T) {
	s, key, receipts := witnessedHistory(t)
	meta, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	meta.Skip = 2
	if err := s.writeMeta(key, meta); err != nil {
		t.Fatal(err)
	}
	attempt := uuid.NewString()
	interrupted := errors.New("controlled callback failure")
	var digest string
	err = s.WithReadOrderRecovery(t.Context(), key, admittedReadRecovery(attempt), func(proof ReadOrderRecovery) error {
		if len(proof.Messages) != 3 || proof.Frontier != 3 || proof.StreamID != receipts[0].Order.StreamID || proof.Messages[0].Order != receipts[0].Order {
			t.Fatal("skipped source prefix was not proved", proof)
		}
		digest = proof.Digest
		return interrupted
	})
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	if _, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "must wait"}); !errors.Is(err, ErrHistoryRecoveryRequired) {
		t.Fatal("pending recovery allowed a writer", err)
	}
	if err := s.DeleteSession(t.Context(), key); !errors.Is(err, ErrHistoryRecoveryRequired) {
		t.Fatal("deletion released a pending administrative gate", err)
	}
	if content, err := s.ReadHistoryContent(t.Context(), key); err != nil || len(content) != 1 {
		t.Fatal("recovery hid readable content", content, err)
	}
	reopened, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if active, err := reopened.ReadOrderRecoveryActive(t.Context(), key); err != nil || !active {
		t.Fatal("restart lost the durable writer gate", active, err)
	}
	if err := reopened.WithReadOrderRecovery(t.Context(), key, admittedReadRecovery(uuid.NewString()), func(ReadOrderRecovery) error { t.Fatal("another attempt borrowed the gate"); return nil }); !errors.Is(err, ErrHistoryRecoveryRequired) {
		t.Fatal(err)
	}
	if err := reopened.WithReadOrderRecovery(t.Context(), key, admittedReadRecovery(attempt), func(proof ReadOrderRecovery) error {
		if proof.Digest != digest {
			t.Fatal("restart changed the frozen proof")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ReleaseReadOrderRecovery(t.Context(), key, attempt); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ReleaseReadOrderRecovery(t.Context(), key, attempt); err != nil {
		t.Fatal("release was not idempotent", err)
	}
	next, err := reopened.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "next"})
	if err != nil || next.Order.Sequence != 4 || next.Order.StreamID != receipts[0].Order.StreamID {
		t.Fatal("recovery reset the producer", next, err)
	}
}

func TestReadRecoveryRefusesIncompleteOrUnreconciledSource(t *testing.T) {
	for _, name := range []string{"missing proof", "replaced occurrence", "producer marker", "foreign owner", "corruption", "uncommitted tail", "uncounted committed tail"} {
		t.Run(name, func(t *testing.T) {
			s, key, _ := witnessedHistory(t)
			switch name {
			case "missing proof":
				data, err := os.ReadFile(s.jsonlPath(key))
				if err != nil {
					t.Fatal(err)
				}
				for index, value := range data {
					if value == '\n' {
						data = data[index+1:]
						break
					}
				}
				if err := os.WriteFile(s.jsonlPath(key), data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "replaced occurrence":
				if err := s.SetHistory(t.Context(), key, []providers.Message{{Role: "assistant", Content: "replacement"}}); err != nil {
					t.Fatal(err)
				}
			case "producer marker":
				if err := os.WriteFile(s.historyRecoveryRequiredPath(key), []byte("torn"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "foreign owner":
				meta, err := s.readMeta(key)
				if err != nil {
					t.Fatal(err)
				}
				meta.Key = "another-owner"
				data, err := json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "uncommitted tail", "uncounted committed tail":
				file, err := os.OpenFile(s.jsonlPath(key), os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteString("{\"role\":\"assistant\",\"content\":\"uncommitted\"}\n")
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatal(writeErr, closeErr)
				}
				if name == "uncounted committed tail" {
					meta, err := s.readMeta(key)
					if err != nil {
						t.Fatal(err)
					}
					info, err := os.Stat(s.jsonlPath(key))
					if err != nil {
						t.Fatal(err)
					}
					size := info.Size()
					meta.HistoryBytes = &size
					if err := s.writeMeta(key, meta); err != nil {
						t.Fatal(err)
					}
				}
			case "corruption":
				if err := os.WriteFile(s.historyOrderWitnessPath(key), []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.WithReadOrderRecovery(t.Context(), key, admittedReadRecovery(uuid.NewString()), func(ReadOrderRecovery) error { t.Fatal("incomplete proof was accepted"); return nil }); err == nil {
				t.Fatal("source recovery unexpectedly succeeded")
			}
			if _, err := os.Stat(s.readRecoveryPath(key)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("proof refusal suspended ordinary writers", err)
			}
		})
	}
}

func admittedReadRecovery(id string) ReadOrderRecoveryRequest {
	return ReadOrderRecoveryRequest{AttemptID: id, Admit: func(context.Context) error { return nil }}
}

func TestReadRecoveryCannotSuspendWritersWithoutAdministrativeAdmission(t *testing.T) {
	source, key, _ := witnessedHistory(t)
	refused := errors.New("administrative attempt missing")
	request := ReadOrderRecoveryRequest{AttemptID: uuid.NewString(), Admit: func(context.Context) error { return refused }}
	if err := source.WithReadOrderRecovery(t.Context(), key, request, func(ReadOrderRecovery) error { t.Fatal("unapproved operation verified a proof"); return nil }); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if active, err := source.ReadOrderRecoveryActive(t.Context(), key); err != nil || active {
		t.Fatal("unapproved operation installed a writer gate", active, err)
	}
}

func TestReadRecoveryDoesNotPromoteAnUnprovenLegacyPrefix(t *testing.T) {
	source := newTestStore(t)
	key := "agent:main:read-recovery-legacy"
	if err := os.WriteFile(source.jsonlPath(key), []byte("{\"role\":\"assistant\",\"content\":\"unproven legacy answer\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := source.writeMeta(key, SessionMeta{Key: key, Count: 1}); err != nil {
		t.Fatal(err)
	}
	first, err := source.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "ordered answer"})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = source.WithReadOrderRecovery(t.Context(), key, admittedReadRecovery(uuid.NewString()), func(ReadOrderRecovery) error { called = true; return nil })
	if !errors.Is(err, ErrHistoryOrderUnavailable) || called {
		t.Fatal("legacy content silently became complete private proof", called, err)
	}
	next, err := source.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "still writable"})
	if err != nil || next.Order.StreamID != first.Order.StreamID || next.Order.Sequence != 2 {
		t.Fatal("proof refusal changed content writer availability or ordering", next, err)
	}
}

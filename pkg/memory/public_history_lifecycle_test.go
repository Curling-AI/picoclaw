package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type lifecycleProjector struct {
	failCommit     bool
	rejectArtifact bool
	verify         error
}

func (p *lifecycleProjector) Commit(m providers.Message) (PublicHistoryProjectionResult, error) {
	if p.failCommit {
		return PublicHistoryProjectionResult{}, &os.PathError{Op: "open", Path: "artifact", Err: os.ErrNotExist}
	}
	return PublicHistoryProjectionResult{Message: m}, nil
}

func (p *lifecycleProjector) Legacy(m providers.Message) (PublicHistoryProjectionResult, error) {
	if p.rejectArtifact && strings.Contains(m.Content, "artifact:") {
		return PublicHistoryProjectionResult{}, ErrPublicHistoryCoverage
	}
	return PublicHistoryProjectionResult{Message: m}, nil
}
func (p *lifecycleProjector) Verify(context.Context, providers.Message) error { return p.verify }

func TestPublicLifecycleLegacyArtifactDoesNotPreventAbort(t *testing.T) {
	s, key := legacyPublicFixture(t, 0)
	raw := []byte("{\"role\":\"assistant\",\"content\":\"old [r](artifact:report.pdf)\"}\n")
	if err := os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMeta(key, SessionMeta{Key: key, Count: 1}); err != nil {
		t.Fatal(err)
	}
	s.SetPublicHistoryProjector(&lifecycleProjector{rejectArtifact: true})
	if err := s.AddMessage(t.Context(), key, "user", "aborted prompt"); err != nil {
		t.Fatal(err)
	}
	active, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	err = s.SetHistory(t.Context(), key, active[:1])
	after, _ := s.GetHistory(t.Context(), key)
	if err != nil || len(after) != 1 {
		t.Fatalf("abort refused despite Skip=0: err=%v retained=%d", err, len(after))
	}
}

func TestPublicLifecycleOptionalCommitFailureDoesNotFailCommittedTurn(t *testing.T) {
	s := newTestStore(t)
	key := "sk_v1_q149_optional"
	for _, v := range []string{"prefix", "active"} {
		if err := s.AddMessage(t.Context(), key, "assistant", v); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TruncateHistory(t.Context(), key, 1); err != nil {
		t.Fatal(err)
	}
	s.SetPublicHistoryProjector(&lifecycleProjector{failCommit: true})
	if err := s.CompactForSessionSave(t.Context(), key); err != nil {
		t.Fatalf("optional Commit failure became turn failure: %v", err)
	}
}

func TestPublicLifecycleNeverPreparedSourceCannotReleaseAck(t *testing.T) {
	for _, version := range []int{0, -1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			s := newTestStore(t)
			key := "sk_v1_q149_ack"
			for _, v := range []string{"a", "b", "c"} {
				if err := s.AddMessage(t.Context(), key, "assistant", v); err != nil {
					t.Fatal(err)
				}
			}
			frame := initializedContent(t, s, key)
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			meta.PublicHistoryVersion, meta.PublicHistoryRevision, meta.PublicHistoryBytes, meta.PublicHistoryCount = version, 0, 0, 0
			meta.PublicHistoryGeneration, meta.PublicHistoryDigest, meta.PublicHistoryComplete = "", "", false
			if err = s.writeMeta(key, meta); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(s.publicHistoryPath(key)); err != nil {
				t.Fatal(err)
			}
			refs := frameRefs(frame)
			tr, err := s.CommitHistoryCompaction(
				t.Context(),
				key,
				ContentCompactionPlan{
					MutationID:    uuid.NewString(),
					DomainID:      frame.DomainID,
					BeforeState:   frame.StateToken,
					SummaryInputs: refs[:2],
					Retained:      refs[2:],
					Summary:       "summary",
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			err = s.AcknowledgeHistoryCompaction(
				t.Context(),
				key,
				ContentArchiveAck{
					DomainID:        tr.DomainID,
					MutationID:      tr.MutationID,
					BeforeState:     tr.BeforeState,
					AfterState:      tr.AfterState,
					Archived:        tr.Dropped,
					ArchiveCommitID: "archive",
				},
			)
			pending, pendingErr := s.ReadPendingHistoryCompaction(t.Context(), key)
			if !errors.Is(err, ErrPublicHistoryCoverage) || pendingErr != nil || pending.Version == 0 {
				t.Fatalf(
					"uncovered version0 ACK released retention: ack=%v pendingVersion=%d pendingErr=%v",
					err,
					pending.Version,
					pendingErr,
				)
			}
		})
	}
}

func TestPublicLifecycleCorruptLegacyPrefixCannotBeRetired(t *testing.T) {
	s, key := legacyPublicFixture(t, 3)
	raw, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	lines[1] = "{broken"
	raw = []byte(strings.Join(lines, "\n"))
	if err = os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = s.TruncateHistory(t.Context(), key, 1); err != nil {
		t.Fatal(err)
	}
	err = s.Compact(t.Context(), key)
	after, _ := os.ReadFile(s.jsonlPath(key))
	meta, _ := s.readMeta(key)
	if err == nil || !strings.Contains(string(after), "{broken") {
		t.Fatalf("corrupt prefix retired: err=%v source=%q complete=%v", err, after, meta.PublicHistoryComplete)
	}
}

func TestPublicLifecycleRecoverableProjectionKeepsPriorCompleteCoverage(t *testing.T) {
	s := newTestStore(t)
	key := "sk_v1_q149_complete"
	for _, v := range []string{"prefix", "active"} {
		if err := s.AddMessage(t.Context(), key, "assistant", v); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TruncateHistory(t.Context(), key, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	p := &lifecycleProjector{failCommit: true}
	s.SetPublicHistoryProjector(p)
	if err := s.AddMessage(t.Context(), key, "assistant", "later confirmed"); err != nil {
		t.Fatal(err)
	}
	p.failCommit = false
	prep, err := s.PreparePublicHistory(t.Context(), key, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; !prep.Ready && i < 3; i++ {
		prep, err = s.PreparePublicHistory(t.Context(), key, prep.Cursor)
		if err != nil {
			t.Fatal(err)
		}
	}
	h, err := s.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Complete {
		t.Fatalf(
			"transient failure permanently lost Complete: entries=%d ready=%v complete=%v",
			len(h.Entries),
			prep.Ready,
			h.Complete,
		)
	}
}

func TestPublicLifecycleColdColonRecoveryUsesJournalOwner(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:x"
	if err := s.AddMessage(t.Context(), key, "assistant", "owned"); err != nil {
		t.Fatal(err)
	}
	meta, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	offset := *meta.HistoryBytes
	if err = s.beginHistoryWrite(
		t.Context(),
		key,
		historyWriteRecovery{Meta: meta, Next: meta, AppendOffset: &offset},
	); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.metaPath(key), s.jsonlPath(key)} {
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(reopened.historyRecoveryPath(key)); !os.IsNotExist(err) {
		t.Fatalf("cold sanitized-key journal not retired: %v", err)
	}
}

func TestPublicLifecycleClearKeepsDurableCleanupRoot(t *testing.T) {
	s := newTestStore(t)
	key := "sk_v1_q149_clear"
	p := &publicCleanupFixture{fail: true}
	s.SetPublicHistoryProjector(p)
	if err := s.AddMessage(t.Context(), key, "assistant", "owned"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearHistory(t.Context(), key); !errors.Is(err, syscall.EIO) {
		t.Fatal("fixture cleanup failure missing", err)
	}
	if _, err := os.Stat(s.historyRecoveryPath(key)); os.IsNotExist(err) {
		t.Fatalf("clear cleanup lost its durable retry root: %v", err)
	}
}

func TestPublicLifecycleProjectorInstallResumesOwnerDeletion(t *testing.T) {
	s := newTestStore(t)
	key := "agent:main:owner-cleanup"
	s.SetPublicHistoryProjector(&publicCleanupFixture{fail: true})
	if err := s.AddMessage(t.Context(), key, "assistant", "owned"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(t.Context(), key); !errors.Is(err, syscall.EIO) {
		t.Fatal("fixture cleanup failure missing", err)
	}
	reopened, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	healthy := &publicCleanupFixture{}
	reopened.SetPublicHistoryProjector(healthy)
	if len(healthy.called) == 0 {
		t.Fatal("install did not resume the hidden owner deletion")
	}
	if _, err = os.Stat(reopened.historyRecoveryPath(key)); !os.IsNotExist(err) {
		t.Fatal("completed cleanup retained retry root", err)
	}
}

func TestPublicLifecycleLegacyVerifierFailureDoesNotPreventReadyAbort(t *testing.T) {
	s := newTestStore(t)
	key := "sk_v1_q149_verify_abort"
	for _, v := range []string{"prefix", "retained", "abort"} {
		if err := s.AddMessage(t.Context(), key, "assistant", v); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TruncateHistory(t.Context(), key, 2); err != nil {
		t.Fatal(err)
	}
	active, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPublicHistoryProjector(&lifecycleProjector{verify: ErrPublicHistoryCoverage})
	err = s.SetHistory(t.Context(), key, active[:1])
	after, _ := s.GetHistory(t.Context(), key)
	if err != nil || len(after) != 1 {
		t.Fatalf("unavailable prefix proof prevented abort: err=%v active=%d", err, len(after))
	}
}

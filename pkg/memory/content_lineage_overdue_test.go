package memory

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory/testdata/lineagefixture"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// overdueContent leaves key with a compaction whose archive confirmation is
// past its deadline, the state that used to lock a session for good.
func overdueContent(t *testing.T, key string) (*JSONLStore, ContentTransition) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, _, transition := pendingContent(t, s, key)
	if _, err = lineagefixture.LegacyArchiveDeadline(dir, key, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if s, err = NewJSONLStore(dir); err != nil {
		t.Fatal(err)
	}
	return s, transition
}

func compactActiveWindow(t *testing.T, s *JSONLStore, key string) (ContentTransition, error) {
	t.Helper()
	if err := s.AddMessage(t.Context(), key, "user", "after the overdue archive"); err != nil {
		t.Fatal(err)
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	refs := frameRefs(frame)
	return s.CommitHistoryCompaction(t.Context(), key, ContentCompactionPlan{
		MutationID: uuid.NewString(), DomainID: frame.DomainID, BeforeState: frame.StateToken,
		SummaryInputs: refs[:1], Retained: refs[1:], Summary: "summary after release",
	})
}

func TestOverdueCompactionIsReleasedByTheOperationThatMeetsIt(t *testing.T) {
	operations := map[string]func(*testing.T, *JSONLStore, string) error{
		"compaction": func(t *testing.T, s *JSONLStore, key string) error {
			_, err := compactActiveWindow(t, s, key)
			return err
		},
		"truncate": func(t *testing.T, s *JSONLStore, key string) error {
			return s.TruncateHistory(t.Context(), key, 1)
		},
		"set history": func(t *testing.T, s *JSONLStore, key string) error {
			return s.SetHistory(t.Context(), key, nil)
		},
		"retention prune": func(t *testing.T, s *JSONLStore, key string) error {
			if err := s.removeSessionFiles(sanitizeKey(key)); err != nil {
				return err
			}
			if _, err := os.Stat(s.jsonlPath(key)); !os.IsNotExist(err) {
				t.Fatal("prune kept the session it reported removed", err)
			}
			return nil
		},
		"edit": func(t *testing.T, s *JSONLStore, key string) error {
			frame, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 1))
			return err
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			key := "sk_v1_overdue_" + uuid.NewString()[:8]
			s, overdue := overdueContent(t, key)
			if err := operation(t, s, key); err != nil {
				t.Fatal("overdue archive still refused the operation", err)
			}
			if name == "retention prune" {
				return // the operation checked the session is gone
			}
			// Read the metadata itself: an unreadable lineage also reports no
			// pending mutation ID, which is not a release.
			meta, err := s.readMeta(key)
			if err != nil || meta.ContentIdentity == nil {
				t.Fatal("the operation left the metadata unreadable", err)
			}
			if pending := meta.ContentIdentity.Pending; pending != nil && pending.Mutation == nil &&
				pending.Transition.MutationID == overdue.MutationID {
				t.Fatal("overdue archive still holds the session")
			}
		})
	}
}

func TestPendingCompactionWithinDeadlineStillHoldsTheSession(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_pending_within_deadline"
	pendingContent(t, s, key)
	if err = s.TruncateHistory(t.Context(), key, 1); err != ErrContentLineagePending {
		t.Fatal("a pending archive lost its confirmation window", err)
	}
	if _, err = compactActiveWindow(t, s, key); err != ErrContentLineagePending {
		t.Fatal("a second compaction overtook an archive within its deadline", err)
	}
}

func TestSaveKeepsTheRowsOfAReleasedCompaction(t *testing.T) {
	key := "sk_v1_overdue_save"
	s, _ := overdueContent(t, key)
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	transition, err := compactActiveWindow(t, s, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, ContentArchiveAck{
		DomainID: transition.DomainID, MutationID: transition.MutationID,
		BeforeState: transition.BeforeState, AfterState: transition.AfterState,
		Archived: transition.Dropped, ArchiveCommitID: uuid.NewString(),
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.CompactForSessionSave(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.HasPrefix(after, before) {
		t.Fatal("Save dropped rows whose archive was never confirmed", err)
	}
}

func TestRewriteAfterReleaseKeepsTheUnarchivedRows(t *testing.T) {
	key := "sk_v1_overdue_rewrite"
	s, _ := overdueContent(t, key)
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	// A restore point that diverges from the active window, as an aborted turn writes.
	if err = s.SetHistory(t.Context(), key, []providers.Message{{Role: "user", Content: "restored"}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.HasPrefix(after, before) {
		t.Fatal("rewrite dropped rows whose archive was never confirmed", err)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil || len(history) != 1 || history[0].Content != "restored" {
		t.Fatal("rewrite did not install the restore point", history, err)
	}
}

// releasedPrefix leaves key with a compaction past its archive deadline whose
// cut holds a private system row the display never shows, and returns the
// bytes of that cut.
func releasedPrefix(t *testing.T, key string) (*JSONLStore, []byte) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []providers.Message{
		{Role: "system", Content: "private note the display never shows"},
		{Role: "user", Content: "first request"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second request"},
		{Role: "assistant", Content: "second answer"},
	} {
		if err = s.AddFullMessage(t.Context(), key, m); err != nil {
			t.Fatal(err)
		}
	}
	frame := initializedContent(t, s, key)
	refs := frameRefs(frame)
	if _, err = s.CommitHistoryCompaction(t.Context(), key, ContentCompactionPlan{
		MutationID: uuid.NewString(), DomainID: frame.DomainID, BeforeState: frame.StateToken,
		SummaryInputs: refs[:3], Retained: refs[3:], Summary: "summarized first exchange",
	}); err != nil {
		t.Fatal(err)
	}
	meta, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	cut, ok := physicalRowsPrefix(raw, meta.Skip)
	if !ok || !bytes.Contains(cut, []byte("private note")) {
		t.Fatal("the compacted cut does not hold the private row", meta.Skip)
	}
	if _, err = lineagefixture.LegacyArchiveDeadline(dir, key, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if s, err = NewJSONLStore(dir); err != nil {
		t.Fatal(err)
	}
	return s, append([]byte(nil), cut...)
}

// An edit's ACK retires the preimage of the source it replaced. After a
// release, that source held the only copy of the cut's rows.
func TestEditAfterReleaseKeepsTheUnarchivedRows(t *testing.T) {
	key := "sk_v1_released_edit"
	s, cut := releasedPrefix(t, key)
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, typedAppliedAck(result.Transition)); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.HasPrefix(after, cut) {
		t.Fatal("the edit's ACK took the only copy of the released rows", err)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil || len(history) != 1 || history[0].Content != "second request" {
		t.Fatal("the edit did not apply", history, err)
	}
	frame, err = s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || frame.IdentityStatus != ContentIdentityKnown {
		t.Fatal("the edit left the identity unusable", frame.IdentityStatus, err)
	}
}

func archiveAck(transition ContentTransition) ContentArchiveAck {
	return ContentArchiveAck{
		DomainID: transition.DomainID, MutationID: transition.MutationID,
		BeforeState: transition.BeforeState, AfterState: transition.AfterState,
		Archived: transition.Dropped, ArchiveCommitID: uuid.NewString(),
	}
}

// Rows whose archive was confirmed may leave the source again: keeping every
// content session's rows let its file grow without end.
func TestSaveCollectsTheRowsOfAnAcknowledgedCompaction(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_acked_save"
	_, _, transition := pendingContent(t, s, key)
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, archiveAck(transition)); err != nil {
		t.Fatal(err)
	}
	active, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompactForSessionSave(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || len(after) >= len(before) {
		t.Fatal("Save kept rows whose archive was confirmed", len(before), len(after), err)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil || len(history) != len(active) {
		t.Fatal("Save changed the active window", history, err)
	}
	if frame, err := s.ReadHistoryContentFrame(t.Context(), key); err != nil ||
		frame.IdentityStatus != ContentIdentityKnown {
		t.Fatal("Save left the identity unusable", frame.IdentityStatus, err)
	}
}

// Metadata written before the collectable marker cannot tell a confirmed cut
// from one an older build released without a trace, so it keeps its prefix. A
// zero marker is also absent from the encoding: checkpoints written by those
// builds still match.
func TestMetadataOlderThanTheCollectableMarkerKeepsItsPrefix(t *testing.T) {
	legacy := []byte(`{"version":1,"domain":"` + uuid.NewString() + `","state":"` + uuid.NewString() +
		`","summaryDigest":"","skip":2,"count":4,"rows":[]}`)
	state := decodeContentIdentity(legacy)
	if len(state.opaque) != 0 {
		t.Fatal("fixture: the legacy identity did not decode")
	}
	if state.collectablePrefix() {
		t.Fatal("a prefix from before the marker was taken as archived")
	}
	encoded, err := json.Marshal(contentIdentityWire(state))
	if err != nil || bytes.Contains(encoded, []byte("collectableSkip")) {
		t.Fatal("a zero marker changes the encoding older builds checkpointed", string(encoded), err)
	}
}

// Retention reads a cron run by its sanitized filename; the archive release
// has to use the stored key, which keeps its colon.
func TestCronRetentionPrunesARunWithAnOverdueArchive(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "agent:cron-daily-" + uuid.NewString()
	pendingContent(t, s, key)
	if _, err = lineagefixture.LegacyArchiveDeadline(dir, key, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if s, err = NewJSONLStore(dir); err != nil {
		t.Fatal(err)
	}
	removed, err := s.PruneCronRuns(time.Hour, time.Now().Add(48*time.Hour))
	if err != nil || removed != 1 {
		t.Fatal("retention kept a cron run whose archive was overdue", removed, err)
	}
	if _, err = os.Stat(s.jsonlPath(key)); !os.IsNotExist(err) {
		t.Fatal("the pruned run is still on disk", err)
	}
}

// An import outside the native policy is not held by the pending archive.
// Releasing it there gave up a late ACK for nothing.
func TestImportDoesNotReleaseAnArchiveItIsNotHeldBy(t *testing.T) {
	key := "sk_v1_overdue_import"
	s, overdue := overdueContent(t, key)
	history, err := s.ReadPublicHistory(t.Context(), key)
	if err != nil || len(history.Entries) == 0 {
		t.Fatal(history, err)
	}
	anchor := history.Entries[len(history.Entries)-1]
	if _, err = s.ImportPublicHistory(t.Context(), PublicHistoryImport{
		Key: key, Generation: history.Generation, Revision: history.Revision, AfterID: anchor.ID,
		Entries: []PublicHistoryEntry{{
			ID: uuid.NewString(), ImportID: "legacy:overdue", AnchorID: anchor.ID,
			AnchorRef: anchor.Ref, AnchorOrder: anchor.Order,
			Message: providers.Message{Role: "assistant", Content: "legacy supplement"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, archiveAck(overdue)); err != nil {
		t.Fatal("the import gave up the late ACK of an archive it is not held by", err)
	}
}

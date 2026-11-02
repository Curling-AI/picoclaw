package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicLifecycleSuffixCutPreservesRawSkippedPrefixAndPreparation(t *testing.T) {
	for _, state := range []string{"legacy", "preparing", "ready", "unavailable"} {
		for _, skip := range []int{0, 79} {
			t.Run(state+map[int]string{0: "/active", 79: "/skipped"}[skip], func(t *testing.T) {
				s, key := legacyPublicFixture(t, 82)
				if state == "preparing" || state == "ready" {
					prep, err := s.PreparePublicHistory(t.Context(), key, "")
					if err != nil {
						t.Fatal(err)
					}
					if state == "ready" {
						for !prep.Ready {
							prep, err = s.PreparePublicHistory(t.Context(), key, prep.Cursor)
							if err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				original, err := s.readMeta(key)
				if err != nil {
					t.Fatal(err)
				}
				ensureHistoryGeneration(&original, key)
				original.Skip = skip
				if state == "unavailable" {
					original.PublicHistoryVersion = -1
				}
				if err = s.writeMeta(key, original); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(s.jsonlPath(key))
				if err != nil {
					t.Fatal(err)
				}
				active, err := s.GetHistory(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				oldCursor := preparationResult(key, original).Cursor
				s.SetPublicHistoryProjector(&lifecycleProjector{verify: ErrPublicHistoryCoverage, rejectArtifact: true})
				if err = s.SetHistory(t.Context(), key, active[:1]); err != nil {
					t.Fatal(err)
				}
				after, err := os.ReadFile(s.jsonlPath(key))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.HasPrefix(before, after) || bytes.Count(after, []byte{'\n'}) != skip+1 {
					t.Fatal("cut changed retained bytes/prefix", len(after))
				}
				current, err := s.readMeta(key)
				if err != nil {
					t.Fatal(err)
				}
				if current.Skip != skip || current.HistoryGeneration != original.HistoryGeneration ||
					current.HistoryFileGeneration != original.HistoryFileGeneration ||
					current.HistoryPrefix != original.HistoryPrefix {
					t.Fatal("cut moved retained occurrence locators", original, current)
				}
				if oldCursor != "" {
					if _, err = s.PreparePublicHistory(
						t.Context(),
						key,
						oldCursor,
					); !errors.Is(
						err,
						ErrPublicHistoryCoverage,
					) {
						t.Fatal("old cursor survived cut", err)
					}
				}
				s.SetPublicHistoryProjector(nil)
				if state == "unavailable" {
					if current.PublicHistoryVersion != -1 {
						t.Fatal("cut fabricated coverage")
					}
					return
				}
				prep, err := s.PreparePublicHistory(t.Context(), key, "")
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; !prep.Ready && i < 4; i++ {
					prep, err = s.PreparePublicHistory(t.Context(), key, prep.Cursor)
					if err != nil {
						t.Fatal(err)
					}
				}
				h, err := s.ReadPublicHistory(t.Context(), key)
				if err != nil || !h.Complete || len(h.Entries) != skip+1 {
					t.Fatal("checkpoint did not settle the exact retained source", h, err)
				}
			})
		}
	}
}

func TestPublicLifecycleSuffixCutCrashRestoresCoherentOldOrNew(t *testing.T) {
	for _, stage := range []string{"model", "public", "metadata"} {
		t.Run(stage, func(t *testing.T) {
			s, key := pagedStore(t, 4)
			if err := s.TruncateHistory(t.Context(), key, 2); err != nil {
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
			target := map[string]string{"model": s.jsonlPath(key), "public": s.publicHistoryPath(key), "metadata": s.metaPath(key)}[stage]
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
					return writeErr
				}
				if path == target {
					panic("crash after " + stage)
				}
				return nil
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("crash phase not reached")
					}
				}()
				_ = s.SetHistory(t.Context(), key, active[:1])
			}()
			reopened, err := NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(reopened.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			meta, err := reopened.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			h, err := reopened.ReadPublicHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			expected := 4
			if stage == "metadata" {
				expected = 3
			}
			if meta.Skip != 2 || meta.Count != expected || len(h.Entries) != expected ||
				!bytes.HasPrefix(before, after) {
				t.Fatal("mixed OLD/NEW rollback image", meta, h)
			}
			if expected == 4 && !bytes.Equal(before, after) {
				t.Fatal("OLD source not restored exactly")
			}
		})
	}
}

func TestPublicLifecycleEmptyRollbackPreservesAcknowledgedDisplayAndLast(t *testing.T) {
	s, key := pagedStore(t, 3)
	frame := initializedContent(t, s, key)
	refs := frameRefs(frame)
	tr, err := s.CommitHistoryCompaction(
		t.Context(),
		key,
		ContentCompactionPlan{
			MutationID:    newHistoryID(),
			DomainID:      frame.DomainID,
			BeforeState:   frame.StateToken,
			SummaryInputs: refs,
			Summary:       "confirmed summary",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ack := ContentArchiveAck{
		DomainID:        tr.DomainID,
		MutationID:      tr.MutationID,
		BeforeState:     tr.BeforeState,
		AfterState:      tr.AfterState,
		Archived:        tr.Dropped,
		ArchiveCommitID: "archive",
	}
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, ack); err != nil {
		t.Fatal(err)
	}
	if err = s.AddMessage(t.Context(), key, "user", "aborted prompt"); err != nil {
		t.Fatal(err)
	}
	before, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetHistory(t.Context(), key, nil); err != nil {
		t.Fatal(err)
	}
	active, err := s.GetHistory(t.Context(), key)
	if err != nil || len(active) != 0 {
		t.Fatal(active, err)
	}
	h, err := s.ReadPublicHistory(t.Context(), key)
	if err != nil || len(h.Entries) != 3 {
		t.Fatal("empty rollback behaved as clear", h, err)
	}
	after, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	oldLast, _ := json.Marshal(before.ContentIdentity.Last)
	newLast, _ := json.Marshal(after.ContentIdentity.Last)
	if !bytes.Equal(oldLast, newLast) || after.Skip != before.Skip || after.Summary != before.Summary ||
		!reflect.DeepEqual(after.ContentIdentity.storage, before.ContentIdentity.storage) {
		t.Fatal("cut changed ACK authority or summary")
	}
}

func TestPublicLifecycleClearCrashResumesCleanupAndClearsSummaryAtomically(t *testing.T) {
	for _, stage := range []string{"model", "metadata"} {
		t.Run(stage, func(t *testing.T) {
			s, key := pagedStore(t, 3)
			if err := s.SetSummary(t.Context(), key, "old summary"); err != nil {
				t.Fatal(err)
			}
			p := &publicCleanupFixture{}
			s.SetPublicHistoryProjector(p)
			target := map[string]string{"model": s.jsonlPath(key), "metadata": s.metaPath(key)}[stage]
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
					return writeErr
				}
				if path == target {
					panic("crash")
				}
				return nil
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("crash not reached")
					}
				}()
				_ = s.ClearHistory(t.Context(), key)
			}()
			reopened, err := NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			healthy := &publicCleanupFixture{}
			reopened.SetPublicHistoryProjector(healthy)
			meta, err := reopened.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			h, err := reopened.ReadPublicHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "model" {
				if meta.Summary != "old summary" || len(h.Entries) != 3 || len(healthy.called) != 0 {
					t.Fatal("OLD clear deleted confirmed data", meta, h, healthy.called)
				}
			} else if meta.Summary != "" || len(h.Entries) != 0 || len(healthy.called) != 1 || healthy.called[0] != key {
				t.Fatal("NEW clear lost summary/cleanup", meta, h, healthy.called)
			}
			if _, err = os.Stat(reopened.historyRecoveryPath(key)); !os.IsNotExist(err) {
				t.Fatal("settled clear left journal", err)
			}
		})
	}
}

func TestPublicLifecycleClearFailurePreservesAtomicEmptySummaryUntilCleanupRetry(t *testing.T) {
	s, key := pagedStore(t, 3)
	if err := s.SetSummary(t.Context(), key, "old summary"); err != nil {
		t.Fatal(err)
	}
	p := &publicCleanupFixture{fail: true}
	s.SetPublicHistoryProjector(p)
	if err := s.ClearHistory(t.Context(), key); err == nil {
		t.Fatal("cleanup failure hidden")
	}
	meta, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Summary != "" || meta.Count != 0 {
		t.Fatal("clear committed mixed history/summary", meta)
	}
	p.fail = false
	s.SetPublicHistoryProjector(p)
	if _, err = os.Stat(s.historyRecoveryPath(key)); !os.IsNotExist(err) {
		t.Fatal("cleanup did not settle", err)
	}
}

func TestPublicLifecycleCorruptUnpreparedSourceRemainsRetainedForOptionalSave(t *testing.T) {
	for _, preparing := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "preparing"}[preparing], func(t *testing.T) {
			s, key := legacyPublicFixture(t, 82)
			if preparing {
				if _, err := s.PreparePublicHistory(t.Context(), key, ""); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(raw, []byte{'\n'})
			lines[70] = []byte("{broken")
			raw = bytes.Join(lines, []byte{'\n'})
			if err = os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err = s.TruncateHistory(t.Context(), key, 1); err != nil {
				t.Fatal(err)
			}
			if err = s.CompactForSessionSave(t.Context(), key); err != nil {
				t.Fatal("optional coverage failure failed turn", err)
			}
			if err = s.Compact(t.Context(), key); !errors.Is(err, ErrPublicHistoryCoverage) {
				t.Fatal("explicit GC accepted corrupt source", err)
			}
			after, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("corrupt source was retired", err)
			}
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			if meta.PublicHistoryComplete {
				t.Fatal("corrupt legacy coverage marked complete")
			}
		})
	}
}

func TestPublicLifecycleCutRegrowthRetiresOpaqueCursorWithoutMovingRows(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "legacy"}[legacy], func(t *testing.T) {
			var s *JSONLStore
			var key string
			if legacy {
				s, key = legacyPublicFixture(t, 4)
			} else {
				s, key = pagedStore(t, 4)
			}
			before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
			if err != nil {
				t.Fatal(err)
			}
			active, err := s.GetHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.SetHistory(t.Context(), key, active[:2]); err != nil {
				t.Fatal(err)
			}
			for _, old := range active[2:] {
				if err = s.AddMessage(t.Context(), key, old.Role, old.Content+" replacement"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.ReadHistoryPage(
				t.Context(),
				HistoryPageRequest{Key: key, Cursor: before.ResumeCursor, Refresh: true},
			); !errors.Is(
				err,
				ErrHistoryCursor,
			) {
				t.Fatal("pre-cut cursor crossed cut/regrowth", err)
			}
			after, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
			if err != nil || len(after.Entries) != 4 {
				t.Fatal("cut lost accessible content", after, err)
			}
			if after.Generation != before.Generation || after.FileGeneration != before.FileGeneration ||
				after.FileEpoch <= before.FileEpoch {
				t.Fatal("raw occurrence generation changed", before, after)
			}
			for i := 0; i < 2; i++ {
				if after.Entries[i].ID != before.Entries[i].ID ||
					after.Entries[i].Position != before.Entries[i].Position {
					t.Fatal("retained occurrence moved")
				}
			}
		})
	}
}

func TestPublicLifecycleActiveInvalidRowFilteringDoesNotClaimCompleteCoverage(t *testing.T) {
	s, key := legacyPublicFixture(t, 3)
	raw, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(raw, []byte{'\n'})
	lines[1] = []byte("{broken")
	raw = bytes.Join(lines, []byte{'\n'})
	if err = os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	active, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	// An explicit active replacement retains legacy model-filtering semantics,
	// but cannot attest display completeness over the discarded malformed row.
	active = append(active, providers.Message{Role: "user", Content: "new"})
	if err = s.SetHistory(t.Context(), key, active); err != nil {
		t.Fatal(err)
	}
	h, err := s.ReadPublicHistory(t.Context(), key)
	if err != nil || h.Complete || len(h.Entries) != 3 {
		t.Fatal("filter manufactured complete public coverage", h, err)
	}
}

func TestPublicLifecycleUnavailableReplacementCrashPreservesRawPrefix(t *testing.T) {
	for _, stage := range []string{"model", "metadata"} {
		t.Run(stage, func(t *testing.T) {
			s, key := pagedStore(t, 3)
			snapshot, err := s.GetHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.TruncateHistory(t.Context(), key, 1); err != nil {
				t.Fatal(err)
			}
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			meta.PublicHistoryVersion = -1
			if err = s.writeMeta(key, meta); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			prefix := bytes.Join(bytes.SplitAfter(before, []byte{'\n'})[:meta.Skip], nil)
			target := map[string]string{"model": s.jsonlPath(key), "metadata": s.metaPath(key)}[stage]
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
					return writeErr
				}
				if path == target {
					panic("crash")
				}
				return nil
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("crash not reached")
					}
				}()
				_ = s.SetHistory(t.Context(), key, snapshot)
			}()
			reopened, err := NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(reopened.jsonlPath(key))
			if err != nil || !bytes.HasPrefix(raw, prefix) {
				t.Fatal("recovery collected prefix", err)
			}
			current, err := reopened.readMeta(key)
			if err != nil || current.Skip != meta.Skip || current.PublicHistoryVersion != -1 {
				t.Fatal("recovery adopted public proof", current, err)
			}
			active, err := reopened.GetHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			expected := 1
			if stage == "metadata" {
				expected = len(snapshot)
			}
			if len(active) != expected {
				t.Fatal("mixed OLD/NEW context", active)
			}
			if stage == "model" && !bytes.Equal(before, raw) {
				t.Fatal("OLD source changed")
			}
			if err = reopened.Compact(t.Context(), key); !errors.Is(err, ErrPublicHistoryCoverage) {
				t.Fatal("unknown public coverage authorized collection", err)
			}
			after, err := os.ReadFile(reopened.jsonlPath(key))
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("refused GC changed prefix", err)
			}
		})
	}
}

func TestPublicLifecycleReadyRestoreRetainsDisplayWithoutCollectingUnverifiedPrefix(t *testing.T) {
	s, key := pagedStore(t, 4)
	initializedContent(t, s, key)
	if err := s.TruncateHistory(t.Context(), key, 1); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	oldDisplay, err := s.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPublicHistoryProjector(&lifecycleProjector{verify: ErrPublicHistoryCoverage})
	if err = s.SetHistory(
		t.Context(),
		key,
		[]providers.Message{{Role: "assistant", Content: "restored model context"}},
	); err != nil {
		t.Fatal("unavailable prefix proof blocked restore", err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	prefix := bytes.Join(bytes.SplitAfter(before, []byte{'\n'})[:3], nil)
	if err != nil || !bytes.HasPrefix(after, prefix) {
		t.Fatal("restore collected prefix", err)
	}
	display, err := s.ReadPublicHistory(t.Context(), key)
	if err != nil || len(display.Entries) != 4 {
		t.Fatal("restore suppressed proven display", display, err)
	}
	for i := 0; i < 3; i++ {
		if display.Entries[i].ID != oldDisplay.Entries[i].ID ||
			display.Entries[i].Message.Content != oldDisplay.Entries[i].Message.Content ||
			display.Entries[i].Ref != nil {
			t.Fatal("retained display proof was invented or lost", display.Entries[i])
		}
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || frame.IdentityStatus != ContentIdentityUnavailable {
		t.Fatal("unqualified restore reattested Ref", frame, err)
	}
	s.SetPublicHistoryProjector(nil)
	if err = s.Compact(t.Context(), key); err == nil {
		t.Fatal("mixed prefix became collectible")
	}
	retained, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(after, retained) {
		t.Fatal("unproved prefix was retired", err)
	}
	if err = s.DeleteSession(t.Context(), key); err != nil {
		t.Fatal("fallback blocked explicit owner deletion", err)
	}
}

func TestPublicLifecycleProvenStructuralRewriteCanMaterializeReady(t *testing.T) {
	for _, preparing := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "preparing"}[preparing], func(t *testing.T) {
			s, key := legacyPublicFixture(t, 80)
			if preparing {
				if _, err := s.PreparePublicHistory(t.Context(), key, ""); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.TruncateHistory(t.Context(), key, 1); err != nil {
				t.Fatal(err)
			}
			if err := s.SetHistory(
				t.Context(),
				key,
				[]providers.Message{{Role: "assistant", Content: "edited retained"}},
			); err != nil {
				t.Fatal(err)
			}
			display, err := s.ReadPublicHistory(t.Context(), key)
			if err != nil || !display.Complete || len(display.Entries) != 80 {
				t.Fatal("proved materialization did not settle Ready", display, err)
			}
			meta, err := s.readMeta(key)
			if err != nil || meta.Skip != 0 || meta.Count != 1 {
				t.Fatal("proved rewrite did not retire covered source", meta, err)
			}
		})
	}
}

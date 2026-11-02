package memory

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicLifecycleTypedMutationCannotRetireUncoveredSkippedPrefix(t *testing.T) {
	for _, state := range []string{"negative", "zero", "preparing", "ready-artifact-unavailable", "ready"} {
		t.Run(state, func(t *testing.T) {
			s, key, _ := typedMutationSource(t)
			if err := s.TruncateHistory(t.Context(), key, 2); err != nil {
				t.Fatal(err)
			}
			frame, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "negative":
				meta.PublicHistoryVersion = -1
			case "zero":
				meta.PublicHistoryVersion = 0
				if err = os.Remove(s.publicHistoryPath(key)); err != nil {
					t.Fatal(err)
				}
			case "preparing":
				meta.PublicHistoryPreparation = &PublicHistoryPreparation{
					Generation:     meta.HistoryGeneration,
					FileGeneration: meta.HistoryFileGeneration,
					Count:          int64(meta.Count),
					Prefix:         meta.HistoryPrefix,
					Ceiling:        *meta.HistoryBytes,
				}
			case "ready-artifact-unavailable":
				s.SetPublicHistoryProjector(&lifecycleProjector{verify: ErrPublicHistoryCoverage})
			}
			if err = s.writeMeta(key, meta); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			imagesBefore, err := filepath.Glob(filepath.Join(s.dir, sanitizeKey(key)+".jsonl.content-preimage.*"))
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, len(frame.Entries)))
			if state == "ready" || state == "zero" || state == "preparing" {
				if err != nil {
					t.Fatal("covered typed mutation rejected", err)
				}
				if err = s.AcknowledgeHistoryContentMutation(
					t.Context(),
					key,
					typedAppliedAck(result.Transition),
				); err != nil {
					t.Fatal("covered typed ACK rejected", err)
				}
				return
			}
			if !errors.Is(err, ErrPublicHistoryCoverage) {
				t.Fatal("uncovered prefix accepted", result, err)
			}
			after, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refused typed mutation changed raw source", err)
			}
			current, err := s.readMeta(key)
			if err != nil || current.ContentIdentity.Pending != nil {
				t.Fatal("refused typed mutation published Pending", err)
			}
			imagesAfter, err := filepath.Glob(filepath.Join(s.dir, sanitizeKey(key)+".jsonl.content-preimage.*"))
			if err != nil || len(imagesBefore) != len(imagesAfter) {
				t.Fatal("refused guard created an orphan preimage", err)
			}
		})
	}
}

func TestPublicLifecycleTypedAckRechecksSkippedPrefixCoverageBeforeRetiringPreimage(t *testing.T) {
	for _, loss := range []string{"negative", "artifact", "file", "wrong-id", "same-body-other-occurrence"} {
		t.Run(loss, func(t *testing.T) {
			s, key, _ := typedMutationSource(t)
			if err := s.TruncateHistory(t.Context(), key, 2); err != nil {
				t.Fatal(err)
			}
			frame, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, len(frame.Entries)))
			if err != nil {
				t.Fatal(err)
			}
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			image := meta.ContentIdentity.Pending.Mutation.Before.Image
			imagePath := filepath.Join(s.dir, contentPreimageName(key, image.ID))
			public, err := os.ReadFile(s.publicHistoryPath(key))
			if err != nil {
				t.Fatal(err)
			}
			switch loss {
			case "wrong-id", "same-body-other-occurrence":
				entries, readErr := s.readPublicEntries(key, meta)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if loss == "wrong-id" {
					entries[0].ID = newHistoryID()
				} else {
					entries[0].ID, entries[2].ID = entries[2].ID, entries[0].ID
				}
				changed := copyMeta(meta)
				change, readErr := s.replacePublicEntries(key, entries, &changed)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if readErr = s.writePublicChange(key, change); readErr != nil {
					t.Fatal(readErr)
				}
				if readErr = s.writeMeta(key, changed); readErr != nil {
					t.Fatal(readErr)
				}
			case "negative":
				meta.PublicHistoryVersion = -1
				if err = s.writeMeta(key, meta); err != nil {
					t.Fatal(err)
				}
			case "artifact":
				s.SetPublicHistoryProjector(&lifecycleProjector{verify: ErrPublicHistoryCoverage})
			case "file":
				if err = os.Remove(s.publicHistoryPath(key)); err != nil {
					t.Fatal(err)
				}
			}
			ack := typedAppliedAck(result.Transition)
			if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, ack); err == nil {
				t.Fatal("typed ACK retired the only retained prefix without current coverage")
			}
			current, err := s.readMeta(key)
			if err != nil || current.ContentIdentity.Pending == nil {
				t.Fatal("failed ACK released Pending", err)
			}
			if _, err = os.Stat(imagePath); err != nil {
				t.Fatal("failed ACK retired preimage", err)
			}
			switch loss {
			case "wrong-id", "same-body-other-occurrence":
				if err = os.WriteFile(s.publicHistoryPath(key), public, 0o600); err != nil {
					t.Fatal(err)
				}
				if err = s.writeMeta(key, meta); err != nil {
					t.Fatal(err)
				}
			case "negative":
				current.PublicHistoryVersion = PublicHistoryVersion
				if err = s.writeMeta(key, current); err != nil {
					t.Fatal(err)
				}
			case "artifact":
				s.SetPublicHistoryProjector(nil)
			case "file":
				if err = os.WriteFile(s.publicHistoryPath(key), public, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, ack); err != nil {
				t.Fatal("restored exact coverage did not permit ACK", err)
			}
			if _, err = os.Stat(imagePath); !os.IsNotExist(err) {
				t.Fatal("successful ACK retained preimage", err)
			}
		})
	}
}

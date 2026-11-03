package memory

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicMediaRecoversADisabledOwnedSourceOnlyIntoPreparation(t *testing.T) {
	for _, prefix := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_public_prefix", true: "owned_public_prefix"}[prefix], func(t *testing.T) {
			store, key := newTestStore(t), "agent:web:disabled-media"
			if err := store.AddMessage(t.Context(), key, "assistant", "old canonical content"); err != nil {
				t.Fatal(err)
			}
			meta, err := store.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			meta.PublicHistoryVersion = -1
			if !prefix {
				if err = os.Remove(store.publicHistoryPath(key)); err != nil {
					t.Fatal(err)
				}
				meta.PublicHistoryBytes, meta.PublicHistoryCount, meta.PublicHistoryRevision = 0, 0, 0
				meta.PublicHistoryGeneration, meta.PublicHistoryDigest = "", ""
			}
			if err = store.writeMeta(key, meta); err != nil {
				t.Fatal(err)
			}
			run := uuid.NewString()
			ctx := providers.WithHistoryRun(t.Context(), run)
			if err = store.AddMessage(ctx, key, "user", "current authorized native run"); err != nil {
				t.Fatal(err)
			}
			if err = store.RecordPublicMedia(ctx, key, run, "[report](artifact:new.pdf)"); err != nil {
				t.Fatal("disabled display blocked a new native completed delivery", err)
			}
			if _, err = store.ReadPublicHistory(ctx, key); !errors.Is(err, ErrPublicHistoryCoverage) {
				t.Fatal("a live media record declared the disabled source ready", err)
			}
			result := finishDeferredMedia(t, store, key)
			if len(result.Entries) != 3 || result.Entries[2].AnchorID != result.Entries[1].ID ||
				result.Entries[1].RunID != run {
				t.Fatal("disabled recovery lost ownership or duplicated the old prefix", result)
			}
		})
	}
}

func TestPublicMediaDoesNotAdoptACorruptDisabledSidecar(t *testing.T) {
	store, key, run := newTestStore(t), "agent:web:corrupt-disabled", uuid.NewString()
	ctx := providers.WithHistoryRun(t.Context(), run)
	if err := store.AddMessage(ctx, key, "user", "request"); err != nil {
		t.Fatal(err)
	}
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	meta.PublicHistoryVersion = -1
	if err = store.writeMeta(key, meta); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(store.publicHistoryPath(key), []byte("unowned sidecar bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordPublicMedia(ctx, key, run, "[report](artifact:new.pdf)"); err == nil {
		t.Fatal("new media adopted a sidecar without its owned digest")
	}
	after, err := store.readMeta(key)
	if err != nil || after.PublicHistoryVersion != -1 {
		t.Fatal("failed admission promoted a corrupt source", after, err)
	}
}

type disabledOwnedPrefixProjector struct {
	unavailable bool
	frozen      bool
}

func (p *disabledOwnedPrefixProjector) Commit(message providers.Message) (PublicHistoryProjectionResult, error) {
	return PublicHistoryProjectionResult{
		Message:                message,
		HasVersionedReferences: p.frozen && message.Content == "old owned artifact",
	}, nil
}

func (p *disabledOwnedPrefixProjector) Legacy(message providers.Message) (PublicHistoryProjectionResult, error) {
	if p.unavailable && message.Content == "old owned artifact" {
		return PublicHistoryProjectionResult{}, ErrPublicHistoryCoverage
	}
	return PublicHistoryProjectionResult{Message: message}, nil
}

func (p *disabledOwnedPrefixProjector) Verify(_ context.Context, message providers.Message) error {
	if p.unavailable && message.Content == "old owned artifact" {
		return ErrPublicHistoryCoverage
	}
	return nil
}

func TestPublicMediaDisabledOwnedGapRechecksOldProofWithoutDuplicatingItsNativeRow(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		mode := "legacy"
		if frozen {
			mode = "frozen"
		}
		for _, newRAW := range []bool{false, true} {
			t.Run(
				mode+"/"+map[bool]string{false: "published_anchor", true: "new_RAW_anchor"}[newRAW],
				func(t *testing.T) {
					store, key := newTestStore(t), "agent:web:disabled-owned-gap"
					run := uuid.NewString()
					ctx := providers.WithHistoryRun(t.Context(), run)
					projector := &disabledOwnedPrefixProjector{frozen: frozen}
					store.SetPublicHistoryProjector(projector)
					if err := store.AddMessage(ctx, key, "assistant", "old owned artifact"); err != nil {
						t.Fatal(err)
					}
					meta, err := store.readMeta(key)
					if err != nil {
						t.Fatal(err)
					}
					meta.PublicHistoryVersion = -1
					// Empty JSONL lines do not create source occurrences.
					raw, err := os.ReadFile(store.jsonlPath(key))
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(raw, []byte("history_public_message")) != frozen {
						t.Fatal("fixture does not exercise the intended frozen projection path")
					}
					raw = append([]byte{'\n'}, raw...)
					if err = os.WriteFile(store.jsonlPath(key), raw, 0o600); err != nil {
						t.Fatal(err)
					}
					*meta.HistoryBytes = int64(len(raw))
					if err = store.writeMeta(key, meta); err != nil {
						t.Fatal(err)
					}
					projector.unavailable = true
					if newRAW {
						if err = store.AddMessage(ctx, key, "user", "new native run"); err != nil {
							t.Fatal(err)
						}
					}
					if err = store.RecordPublicMedia(ctx, key, run, "[new](artifact:new.pdf)"); err != nil {
						t.Fatal(err)
					}
					preparation, err := store.PreparePublicHistory(ctx, key, "")
					if err != nil {
						t.Fatal(err)
					}
					if _, err = store.PreparePublicHistory(ctx, key, preparation.Cursor); err == nil {
						t.Fatal("old owned artifact was not rechecked")
					}
					blocked, metaErr := store.readMeta(key)
					if metaErr != nil || blocked.PublicHistoryPreparation == nil || blocked.PublicHistoryComplete {
						t.Fatal("missing old proof promoted preparation", blocked, metaErr)
					}
					if _, readErr := store.ReadPublicHistory(ctx, key); !errors.Is(readErr, ErrPublicHistoryCoverage) {
						t.Fatal("missing old proof exposed a ready history", readErr)
					}
					projector.unavailable = false
					result := finishDeferredMedia(t, store, key)
					want := 2
					if newRAW {
						want++
					}
					if len(result.Entries) != want {
						t.Fatal("recovery duplicated an owned native row", len(result.Entries))
					}
				},
			)
		}
	}
}

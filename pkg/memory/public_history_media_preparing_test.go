package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicMediaDuringPreparationKeepsItsNativeAnchorAndSurvivesRestart(t *testing.T) {
	store, key := newTestStore(t), "agent:web:preparing-media"
	run := uuid.NewString()
	ctx := providers.WithHistoryRun(t.Context(), run)
	if err := store.AddMessage(ctx, key, "user", "request"); err != nil {
		t.Fatal(err)
	}
	projector := &transientPublicProjector{failing: true}
	store.SetPublicHistoryProjector(projector)
	if err := store.AddMessage(ctx, key, "assistant", "confirmed while projection failed"); err != nil {
		t.Fatal(err)
	}
	projector.failing = false
	if err := store.AddMessage(ctx, key, "tool", "completed new file"); err != nil {
		t.Fatal(err)
	}
	model, err := store.GetHistory(ctx, key)
	if err != nil || len(model) != 3 {
		t.Fatal(model, err)
	}
	anchor := model[2].HistoryID
	const content = "[report](artifact:immutable-new.pdf)"
	if err = store.RecordPublicMedia(ctx, key, run, content); err != nil {
		t.Fatal("an earlier projection gap blocked completed media", err)
	}
	if _, err = store.ReadPublicHistory(ctx, key); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatal("media declared its unprepared source complete", err)
	}
	store, err = NewJSONLStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	store.SetPublicHistoryProjector(projector)
	if err = store.RecordPublicMedia(ctx, key, run, content); err != nil {
		t.Fatal("restart lost deferred media idempotency", err)
	}
	preparation, err := store.PreparePublicHistory(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; !preparation.Ready && step < 5; step++ {
		preparation, err = store.PreparePublicHistory(ctx, key, preparation.Cursor)
		if err != nil {
			t.Fatal(err)
		}
	}
	history, err := store.ReadPublicHistory(ctx, key)
	if err != nil || !preparation.Ready || len(history.Entries) != 4 ||
		history.Entries[2].ID != anchor || history.Entries[3].AnchorID != anchor ||
		history.Entries[3].Message.Content != content {
		t.Fatal("preparation lost, duplicated or reordered deferred delivery", history, err)
	}
	after, err := store.GetHistory(ctx, key)
	if err != nil || len(after) != len(model) {
		t.Fatal("completed public media entered model context", after, err)
	}
}

func TestPreparingMediaKeepsTheExistingPerMessageBudget(t *testing.T) {
	store, key, run := newTestStore(t), "agent:web:preparing-full-media", uuid.NewString()
	ctx := providers.WithHistoryRun(t.Context(), run)
	if err := store.AddMessage(ctx, key, "user", "request"); err != nil {
		t.Fatal(err)
	}
	projector := &transientPublicProjector{failing: true}
	store.SetPublicHistoryProjector(projector)
	if err := store.AddMessage(ctx, key, "assistant", "old gap"); err != nil {
		t.Fatal(err)
	}
	projector.failing = false
	if err := store.AddMessage(ctx, key, "tool", "completed file"); err != nil {
		t.Fatal(err)
	}
	empty, err := json.Marshal(providers.Message{Role: "assistant", Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("x", PublicHistoryMessageBytes-len(empty)+1)
	if err = store.RecordPublicMedia(ctx, key, run, content); err != nil {
		t.Fatal("preparation reduced the existing valid public-message budget", err)
	}
}

func TestPreparingMediaDoesNotTurnAccumulatedHistoryIntoALiveDeliveryCap(t *testing.T) {
	store, key, run := newTestStore(t), "agent:web:preparing-many-media", uuid.NewString()
	ctx := providers.WithHistoryRun(t.Context(), run)
	if err := store.AddMessage(ctx, key, "user", "request"); err != nil {
		t.Fatal(err)
	}
	projector := &transientPublicProjector{failing: true}
	store.SetPublicHistoryProjector(projector)
	if err := store.AddMessage(ctx, key, "assistant", "old gap"); err != nil {
		t.Fatal(err)
	}
	projector.failing = false
	for index := 0; index < 5; index++ {
		if err := store.AddMessage(ctx, key, "tool", fmt.Sprintf("completed file %d", index)); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordPublicMedia(ctx, key, run, strings.Repeat("x", 512<<10)); err != nil {
			t.Fatal("previous completed deliveries blocked a valid new delivery", index, err)
		}
	}
	if err := store.AddMessage(ctx, key, "tool", strings.Repeat("z", PublicHistoryMessageBytes-2048)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicMedia(ctx, key, run, "[small](artifact:small.pdf)"); err != nil {
		t.Fatal(err)
	}
	preparation, err := store.PreparePublicHistory(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; !preparation.Ready && step < 10; step++ {
		before := preparation.Cursor
		preparation, err = store.PreparePublicHistory(ctx, key, preparation.Cursor)
		if err != nil {
			t.Fatal(err)
		}
		if !preparation.Ready && before == preparation.Cursor {
			t.Fatal("preparation did not advance")
		}
	}
	history, err := store.ReadPublicHistory(ctx, key)
	if err != nil || !preparation.Ready || len(history.Entries) != 14 {
		t.Fatal("deferred media could not settle after repairing the old projection", history, err)
	}
}

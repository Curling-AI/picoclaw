package memory

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

type publicCleanupFixture struct {
	unavailablePublicVerifier
	fail   bool
	called []string
}

func (p *publicCleanupFixture) DeleteSession(key string) error {
	p.called = append(p.called, key)
	if p.fail {
		return syscall.EIO
	}
	return nil
}

func TestPublicCleanupDeletionIntentSurvivesReopenBeforeProjectorInstallation(t *testing.T) {
	store, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "agent:main:web:cleanup-restart"
	projector := &publicCleanupFixture{fail: true}
	store.SetPublicHistoryProjector(projector)
	if operationErr := store.AddMessage(t.Context(), key, "assistant", "confirmed"); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := store.DeleteSession(t.Context(), key); !errors.Is(operationErr, syscall.EIO) {
		t.Fatal("cleanup failure was hidden", operationErr)
	}
	if _, operationErr := os.Stat(store.historyRecoveryPath(key)); operationErr != nil {
		t.Fatal("cleanup lost its durable retry root", operationErr)
	}
	reopened, err := NewJSONLStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, operationErr := os.Stat(reopened.historyRecoveryPath(key)); operationErr != nil {
		t.Fatal("constructor retired cleanup before capability installation", operationErr)
	}
	healthy := &publicCleanupFixture{}
	reopened.SetPublicHistoryProjector(healthy)
	if operationErr := reopened.DeleteSession(t.Context(), key); operationErr != nil {
		t.Fatal("cleanup retry after projector installation failed", operationErr)
	}
	if len(healthy.called) == 0 || healthy.called[0] != key {
		t.Fatal("recovery used a sanitized or missing owner", healthy.called)
	}
	if _, operationErr := os.Stat(reopened.historyRecoveryPath(key)); !errors.Is(operationErr, os.ErrNotExist) {
		t.Fatal("successful cleanup retained deletion journal", operationErr)
	}
}

func TestPublicCleanupUsesExistingCronAndEmptyPruneRecovery(t *testing.T) {
	for _, purpose := range []string{"cron", "empty"} {
		t.Run(purpose, func(t *testing.T) {
			store, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "agent:cron-cleanup-test"
			if purpose == "empty" {
				key = "sk_v1_empty_cleanup"
			}
			projector := &publicCleanupFixture{}
			store.SetPublicHistoryProjector(projector)
			if operationErr := store.AddMessage(t.Context(), key, "assistant", "confirmed"); operationErr != nil {
				t.Fatal(operationErr)
			}
			if purpose == "empty" {
				if operationErr := store.ClearHistory(t.Context(), key); operationErr != nil {
					t.Fatal(operationErr)
				}
			}
			projector.fail = true
			prune := func() (int, error) {
				if purpose == "cron" {
					return store.PruneCronRuns(time.Minute, time.Now().Add(time.Hour))
				}
				removed, err := store.PruneEmptySessions(time.Minute, time.Now().Add(time.Hour), nil)
				return len(removed), err
			}
			if removed, _ := prune(); removed != 0 {
				t.Fatal("prune claimed incomplete public cleanup", removed)
			}
			if _, operationErr := os.Stat(store.historyRecoveryPath(key)); operationErr != nil {
				t.Fatal("prune lost cleanup retry root", operationErr)
			}
			projector.fail = false
			if removed, operationErr := prune(); operationErr != nil || removed != 1 {
				t.Fatal("prune did not resume original cleanup", removed, operationErr)
			}
			if _, operationErr := os.Stat(store.historyRecoveryPath(key)); !errors.Is(operationErr, os.ErrNotExist) {
				t.Fatal("prune left its completed journal", operationErr)
			}
		})
	}
}

func TestPublicCleanupExternalDeletionRetiresOnlyJournalOwnedDisplay(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "replaced"}[changed], func(t *testing.T) {
			store, key := pagedStore(t, 2)
			meta, err := store.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			offset := *meta.HistoryBytes
			if err = store.beginHistoryWrite(
				t.Context(),
				key,
				historyWriteRecovery{Meta: meta, Next: meta, AppendOffset: &offset},
			); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{store.metaPath(key), store.jsonlPath(key)} {
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(store.publicHistoryPath(key))
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				before = []byte("unrelated display bytes")
				if err = os.WriteFile(store.publicHistoryPath(key), before, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err = store.UpsertSessionMeta(t.Context(), key, nil, nil)
			if changed {
				if !errors.Is(err, ErrPublicHistoryCoverage) {
					t.Fatal("recreation adopted an unrelated display", err)
				}
				after, readErr := os.ReadFile(store.publicHistoryPath(key))
				if readErr != nil || string(after) != string(before) {
					t.Fatal("unknown display removed", readErr)
				}
				if _, statErr := os.Stat(store.historyRecoveryPath(key)); statErr != nil {
					t.Fatal("mismatch lost retry root", statErr)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, statErr := os.Stat(store.publicHistoryPath(key)); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("fresh scope inherited old public bytes", statErr)
				}
			}
		})
	}
}

func TestPublicCleanupExternalDeletionDefersUntilCapabilityInstalled(t *testing.T) {
	store, key := pagedStore(t, 2)
	store.SetPublicHistoryProjector(&publicCleanupFixture{fail: true})
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	offset := *meta.HistoryBytes
	if err = store.beginHistoryWrite(
		t.Context(),
		key,
		historyWriteRecovery{Meta: meta, Next: meta, AppendOffset: &offset},
	); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store.metaPath(key), store.jsonlPath(key)} {
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.UpsertSessionMeta(t.Context(), key, nil, nil); !errors.Is(err, syscall.EIO) {
		t.Fatal("failed media cleanup lost its pending journal", err)
	}
	reopened, err := NewJSONLStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(reopened.historyRecoveryPath(key)); err != nil {
		t.Fatal("restart retired cleanup without capability", err)
	}
	healthy := &publicCleanupFixture{}
	reopened.SetPublicHistoryProjector(healthy)
	if err = reopened.UpsertSessionMeta(t.Context(), key, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(healthy.called) != 1 || healthy.called[0] != key {
		t.Fatal("owned cleanup was not retried", healthy.called)
	}
	if _, err = os.Stat(reopened.historyRecoveryPath(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retry journal retained", err)
	}
}

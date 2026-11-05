package memory

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type unavailablePublicVerifier struct{ err error }

func (p unavailablePublicVerifier) Commit(message providers.Message) (PublicHistoryProjectionResult, error) {
	return PublicHistoryProjectionResult{Message: message}, nil
}

func (p unavailablePublicVerifier) Legacy(message providers.Message) (PublicHistoryProjectionResult, error) {
	return PublicHistoryProjectionResult{Message: message}, nil
}
func (p unavailablePublicVerifier) Verify(context.Context, providers.Message) error { return p.err }

func TestOptionalPublicVerificationRetainsSourceWithoutFailingCommittedSave(t *testing.T) {
	for _, cause := range []error{errors.New("artifact unavailable"), &os.PathError{Op: "open", Path: "private-artifact.pdf", Err: os.ErrNotExist}} {
		t.Run(cause.Error(), func(t *testing.T) {
			store, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_optional_public"
			for _, text := range []string{"prefix", "retained"} {
				if operationErr := store.AddMessage(t.Context(), key, "assistant", text); operationErr != nil {
					t.Fatal(operationErr)
				}
			}
			if operationErr := store.TruncateHistory(t.Context(), key, 1); operationErr != nil {
				t.Fatal(operationErr)
			}
			before, err := os.ReadFile(store.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			store.SetPublicHistoryProjector(unavailablePublicVerifier{err: cause})
			if operationErr := store.CompactForSessionSave(t.Context(), key); operationErr != nil {
				t.Fatal("optional proof failed an already committed turn", operationErr)
			}
			if operationErr := store.Compact(t.Context(), key); operationErr == nil {
				t.Fatal("explicit GC accepted absent artifact proof")
			}
			after, err := os.ReadFile(store.jsonlPath(key))
			if err != nil || string(before) != string(after) {
				t.Fatal("unproven GC retired source", err)
			}
			if operationErr := os.Remove(store.publicHistoryPath(key)); operationErr != nil {
				t.Fatal(operationErr)
			}
			if operationErr := store.CompactForSessionSave(t.Context(), key); operationErr == nil {
				t.Fatal("physical public source error was hidden as optional verification")
			}
		})
	}
}

func TestPublicReadWaitsForSourceWriterBeforeCanonicalPreflight(t *testing.T) {
	store, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_public_lock"
	if operationErr := store.AddMessage(t.Context(), key, "user", "confirmed"); operationErr != nil {
		t.Fatal(operationErr)
	}
	lock := store.sessionLock(key)
	lock.Lock()
	path := store.historyRecoveryPath(key)
	if operationErr := os.WriteFile(path, []byte("writer in progress"), 0o600); operationErr != nil {
		lock.Unlock()
		t.Fatal(operationErr)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = os.Remove(path); lock.Unlock() }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() { _, err := store.ReadPublicHistory(t.Context(), key); done <- err }()
	select {
	case err := <-done:
		t.Fatal("canonical preflight raced an in-progress source writer", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

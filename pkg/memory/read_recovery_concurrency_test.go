package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestReadRecoveryCallbacksLeaveContentAndOtherConversationAvailable(t *testing.T) {
	for _, phase := range []string{"admit", "apply"} {
		t.Run(phase, func(t *testing.T) {
			store, key, _ := witnessedHistory(t)
			other := "agent:main:other-recovery"
			for lockShard(other) == lockShard(key) {
				other += "x"
			}
			if _, err := store.AppendHistoryReceipt(
				t.Context(),
				other,
				providers.Message{Role: "assistant", Content: "healthy"},
			); err != nil {
				t.Fatal(err)
			}
			entered, resume := make(chan struct{}), make(chan struct{})
			pause := func() { close(entered); <-resume }
			request := admittedReadRecovery(uuid.NewString())
			if phase == "admit" {
				request.Admit = func(context.Context) error { pause(); return nil }
			}
			done := make(chan error, 1)
			go func() {
				done <- store.WithReadOrderRecovery(t.Context(), key, request, func(ReadOrderRecovery) error {
					if phase == "apply" {
						pause()
					}
					return nil
				})
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("callback not entered")
			}
			operations := []struct {
				name string
				run  func() error
			}{
				{"own history", func() error { _, err := store.GetHistory(t.Context(), key); return err }},
				{"own content", func() error { _, err := store.ReadHistoryContent(t.Context(), key); return err }},
				{"other content", func() error { _, err := store.ReadHistoryContent(t.Context(), other); return err }},
				{
					"other receipts",
					func() error { _, err := store.ReadHistoryWithReceipts(t.Context(), other); return err },
				},
				{"other append", func() error {
					_, err := store.AppendHistoryReceipt(
						t.Context(),
						other,
						providers.Message{Role: "assistant", Content: "still healthy"},
					)
					return err
				}},
			}
			results := make(chan error, len(operations))
			for _, operation := range operations {
				go func() {
					if err := operation.run(); err != nil {
						results <- fmt.Errorf("%s: %w", operation.name, err)
					} else {
						results <- nil
					}
				}()
			}
			remaining := len(operations)
			timer := time.NewTimer(300 * time.Millisecond)
		collect:
			for remaining > 0 {
				select {
				case err := <-results:
					remaining--
					if err != nil {
						t.Error(err)
					}
				case <-timer.C:
					t.Errorf("%s callback blocked %d readers or healthy writers", phase, remaining)
					break collect
				}
			}
			timer.Stop()
			close(resume)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			for remaining > 0 {
				if err := <-results; err != nil {
					t.Error(err)
				}
				remaining--
			}
		})
	}
}

func TestReadRecoveryCallbackClaimExcludesReplayAndRelease(t *testing.T) {
	store, key, _ := witnessedHistory(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	attempt := uuid.NewString()
	interrupted := errors.New("lost callback result")
	done := make(chan error, 1)
	go func() {
		done <- store.WithReadOrderRecovery(t.Context(), key, admittedReadRecovery(attempt), func(ReadOrderRecovery) error { close(entered); <-resume; return interrupted })
	}()
	<-entered
	// Both operations must refuse without waiting on the paused callback.
	results := make(chan error, 4)
	go func() { results <- store.ReleaseReadOrderRecovery(t.Context(), key, attempt) }()
	go func() {
		results <- store.WithReadOrderRecovery(t.Context(), key, admittedReadRecovery(attempt), func(ReadOrderRecovery) error { return errors.New("replayed callback") })
	}()
	go func() { results <- store.DeleteSession(t.Context(), key) }()
	go func() {
		_, err := store.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "blocked"})
		results <- err
	}()
	remaining := 4
	timer := time.NewTimer(300 * time.Millisecond)
collect:
	for remaining > 0 {
		select {
		case err := <-results:
			remaining--
			if !errors.Is(err, ErrHistoryRecoveryRequired) {
				t.Errorf("active callback did not fence replay/release/writer: %v", err)
			}
		case <-timer.C:
			t.Error("active callback blocked competing operations instead of refusing")
			break collect
		}
	}
	timer.Stop()
	close(resume)
	if err := <-done; !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	for remaining > 0 {
		<-results
		remaining--
	}
	if active, err := store.ReadOrderRecoveryActive(t.Context(), key); err != nil || !active {
		t.Fatal("uncertain callback lost durable gate", active, err)
	}
	if err := store.ReleaseReadOrderRecovery(t.Context(), key, attempt); err != nil {
		t.Fatal(err)
	}
}

func TestReadRecoveryAdmissionCancellationDoesNotInstallGate(t *testing.T) {
	store, key, receipts := witnessedHistory(t)
	ctx, cancel := context.WithCancel(t.Context())
	request := ReadOrderRecoveryRequest{
		AttemptID: uuid.NewString(),
		Admit:     func(context.Context) error { cancel(); return nil },
	}
	verificationErr := store.WithReadOrderRecovery(
		ctx,
		key,
		request,
		func(ReadOrderRecovery) error { t.Error("canceled admission reached apply"); return nil },
	)
	if !errors.Is(verificationErr, context.Canceled) {
		t.Fatal(verificationErr)
	}
	if active, err := store.ReadOrderRecoveryActive(t.Context(), key); err != nil || active {
		t.Fatal("canceled admission suspended content writers", active, err)
	}
	next, err := store.AppendHistoryReceipt(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "still writable"},
	)
	if err != nil || next.Order.Sequence != 4 || next.Order.StreamID != receipts[0].Order.StreamID {
		t.Fatal(next, err)
	}
	if err := store.WithReadOrderRecovery(
		t.Context(),
		key,
		admittedReadRecovery(request.AttemptID),
		func(ReadOrderRecovery) error { return nil },
	); err != nil {
		t.Fatal("canceled call retained its in-flight claim", err)
	}
}

func TestReadRecoveryCallbacksCanReadTheStoreReentrantly(t *testing.T) {
	store, key, _ := witnessedHistory(t)
	callback := func() error { _, err := store.ReadHistoryContent(t.Context(), key); return err }
	request := ReadOrderRecoveryRequest{
		AttemptID: uuid.NewString(),
		Admit:     func(context.Context) error { return callback() },
	}
	done := make(chan error, 1)
	go func() {
		done <- store.WithReadOrderRecovery(t.Context(), key, request, func(ReadOrderRecovery) error { return callback() })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback reentered a held session stripe")
	}
}

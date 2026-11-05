package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

type userPromptReceiptBackend struct {
	*session.JSONLBackend
	receiptCalls, regularCalls int
	beforeError                error
	afterError                 error
	alter                      func(*memory.CommittedHistoryMessage)
}

func (backend *userPromptReceiptBackend) AppendHistoryReceipt(
	ctx context.Context,
	key string,
	message providers.Message,
) (memory.CommittedHistoryMessage, error) {
	backend.receiptCalls++
	if backend.beforeError != nil {
		return memory.CommittedHistoryMessage{}, backend.beforeError
	}
	receipt, err := backend.JSONLBackend.AppendHistoryReceipt(ctx, key, message)
	if err != nil {
		return receipt, err
	}
	if backend.afterError != nil {
		return memory.CommittedHistoryMessage{}, backend.afterError
	}
	if backend.alter != nil {
		backend.alter(&receipt)
	}
	return receipt, nil
}

func (backend *userPromptReceiptBackend) AppendHistoryContext(
	ctx context.Context,
	key string,
	message providers.Message,
) error {
	backend.regularCalls++
	return backend.JSONLBackend.AppendHistoryContext(ctx, key, message)
}

type userPromptReceiptIngest struct {
	noopContextManager
	t        *testing.T
	observed *bool
	message  providers.Message
}

func (manager *userPromptReceiptIngest) Ingest(_ context.Context, request *IngestRequest) error {
	if !*manager.observed {
		manager.t.Error("user prompt ingestion preceded its committed receipt")
	}
	manager.message = request.Message
	return nil
}

func newUserPromptReceiptFixture(t *testing.T) (*AgentLoop, *AgentInstance, *userPromptReceiptBackend, *turnState) {
	t.Helper()
	loop, instance, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	t.Cleanup(cleanup)
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := &userPromptReceiptBackend{JSONLBackend: session.NewJSONLBackend(store)}
	instance.Sessions = backend
	state := newTurnState(
		instance,
		normalizeProcessOptions(makeTestProcessOpts("user-prompt-receipt")),
		turnEventScope{},
	)
	state.historyRunID = uuid.NewString()
	loop.contextManager = &noopContextManager{}
	return loop, instance, backend, state
}

func TestTurnUserPromptReceiptPrecedesIngestAndKeepsExactIdentity(t *testing.T) {
	loop, _, backend, state := newUserPromptReceiptFixture(t)
	observed := false
	manager := &userPromptReceiptIngest{t: t, observed: &observed}
	loop.contextManager = manager
	var actual []memory.CommittedHistoryMessage
	loop.SetSessionHistoryReceiptObserver(func(key string, committed []memory.CommittedHistoryMessage) {
		if key != state.sessionKey {
			t.Error("prompt receipt was reported to a different session")
		}
		actual = append(actual, committed...)
		observed = true
	})
	if _, err := NewPipeline(loop).SetupTurn(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	stored, err := backend.ReadHistoryWithReceipts(t.Context(), state.sessionKey)
	if err != nil || len(stored) != 1 || len(actual) != 1 || backend.receiptCalls != 1 || backend.regularCalls != 0 {
		t.Fatalf(
			"root prompt must be appended once and observed by its exact receipt: stored=%d observed=%d receipt_appends=%d legacy_appends=%d error=%t",
			len(stored),
			len(actual),
			backend.receiptCalls,
			backend.regularCalls,
			err != nil,
		)
	}
	if !actual[0].Order.Valid() || actual[0].Order != stored[0].Order ||
		actual[0].Message.HistoryID != stored[0].Message.HistoryID ||
		actual[0].Message.HistoryRunID != state.historyRunID {
		t.Fatal("prompt capture borrowed or lost the persisted identity")
	}
	persisted := state.persistedMessagesSnapshot()
	if len(persisted) != 1 || persisted[0].HistoryID != actual[0].Message.HistoryID ||
		persisted[0].HistoryRunID != state.historyRunID ||
		manager.message.HistoryID != actual[0].Message.HistoryID {
		t.Fatal("the prompt's restore/ingest input lost its exact committed message")
	}
}

func TestTurnUserPromptReceiptFailureNeverRepeatsAnUncertainCommit(t *testing.T) {
	for _, test := range []struct {
		name       string
		before     error
		after      error
		alter      func(*memory.CommittedHistoryMessage)
		wantLegacy int
	}{
		{name: "unsupported", before: memory.ErrHistoryUnsupported, wantLegacy: 1},
		{name: "ownership unavailable before write", before: memory.ErrHistoryOwnershipUnavailable, wantLegacy: 1},
		{name: "receipt unavailable before write", before: memory.ErrHistoryReceiptUnavailable, wantLegacy: 1},
		{name: "unknown outcome after write", after: errors.New("synthetic append outcome unknown")},
		{name: "source new with pending recovery", after: fmt.Errorf("%w: %w", memory.ErrHistoryInputCommitted, errors.Join(memory.ErrHistoryUnsupported, memory.ErrHistoryReceiptUnavailable))},
		{name: "invalid order after write", alter: func(receipt *memory.CommittedHistoryMessage) { receipt.Order.Version = 0 }},
		{name: "wrong run after write", alter: func(receipt *memory.CommittedHistoryMessage) { receipt.Message.HistoryRunID = uuid.NewString() }},
		{name: "invalid source after write", alter: func(receipt *memory.CommittedHistoryMessage) { receipt.Message.HistoryID = "invalid" }},
		{name: "different message after write", alter: func(receipt *memory.CommittedHistoryMessage) { receipt.Message.Content = "different synthetic message" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			loop, _, backend, state := newUserPromptReceiptFixture(t)
			backend.beforeError, backend.afterError, backend.alter = test.before, test.after, test.alter
			notifications := 0
			loop.SetSessionHistoryReceiptObserver(func(string, []memory.CommittedHistoryMessage) { notifications++ })
			if _, err := NewPipeline(loop).SetupTurn(t.Context(), state); err != nil {
				t.Fatal(err)
			}
			history := backend.GetHistory(state.sessionKey)
			if len(history) != 1 || backend.receiptCalls != 1 || backend.regularCalls != test.wantLegacy ||
				notifications != 0 {
				t.Fatalf(
					"failed receipt duplicated the prompt or fabricated proof: rows=%d receipt_appends=%d legacy_appends=%d notifications=%d",
					len(history),
					backend.receiptCalls,
					backend.regularCalls,
					notifications,
				)
			}
			persisted := state.persistedMessagesSnapshot()
			if len(persisted) != 1 || persisted[0].Content != state.userMessage || persisted[0].HistoryID != "" ||
				persisted[0].HistoryRunID != "" {
				t.Fatal("failed proof changed the pending prompt or granted it a source identity")
			}
		})
	}
}

func TestTurnUserPromptWithoutReceiptSubscriptionKeepsLegacyAppend(t *testing.T) {
	for _, item := range []string{"no run", "invalid run", "no observer"} {
		t.Run(item, func(t *testing.T) {
			loop, _, backend, state := newUserPromptReceiptFixture(t)
			notifications := 0
			if item != "no observer" {
				loop.SetSessionHistoryReceiptObserver(
					func(string, []memory.CommittedHistoryMessage) { notifications++ },
				)
				state.historyRunID = ""
				if item == "invalid run" {
					state.historyRunID = "invalid"
				}
			}
			if _, err := NewPipeline(loop).SetupTurn(t.Context(), state); err != nil {
				t.Fatal(err)
			}
			if len(backend.GetHistory(state.sessionKey)) != 1 || backend.receiptCalls != 0 ||
				backend.regularCalls != 1 ||
				notifications != 0 {
				t.Fatal("unsubscribed turn changed its original append path")
			}
		})
	}
}

type userPromptLegacyBackend struct {
	session.SessionStore
	appends int
}

func (backend *userPromptLegacyBackend) AddFullMessage(key string, message providers.Message) {
	backend.appends++
	backend.SessionStore.AddFullMessage(key, message)
}

func TestTurnUserPromptBackendWithoutReceiptsKeepsLegacyAppend(t *testing.T) {
	loop, instance, backend, state := newUserPromptReceiptFixture(t)
	legacy := &userPromptLegacyBackend{SessionStore: backend.JSONLBackend}
	instance.Sessions = legacy
	notifications := 0
	loop.SetSessionHistoryReceiptObserver(func(string, []memory.CommittedHistoryMessage) { notifications++ })
	if _, err := NewPipeline(loop).SetupTurn(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	if len(legacy.GetHistory(state.sessionKey)) != 1 || legacy.appends != 1 || notifications != 0 {
		t.Fatal("unsupported receipt backend changed its original append behavior")
	}
}

func TestTurnUserPromptReceiptIdentityStaysOutOfProviderMessages(t *testing.T) {
	provider := &recordingProvider{}
	loop, instance, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := &userPromptReceiptBackend{JSONLBackend: session.NewJSONLBackend(store)}
	instance.Sessions = backend
	var actual []memory.CommittedHistoryMessage
	loop.SetSessionHistoryReceiptObserver(
		func(_ string, committed []memory.CommittedHistoryMessage) { actual = append(actual, committed...) },
	)
	run := uuid.NewString()
	if _, err := loop.ProcessDirectWithMedia(
		providers.WithHistoryRun(t.Context(), run),
		"synthetic prompt",
		"prompt-provider-boundary",
		"grpc",
		run,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(actual) != 1 || actual[0].Message.HistoryRunID != run || backend.receiptCalls != 1 {
		t.Fatal("full turn did not use the root prompt receipt lane exactly once")
	}
	for _, message := range provider.lastMessages {
		if message.HistoryID != "" || message.HistoryRunID != "" || message.HistoryOrderVersion != 0 ||
			message.HistoryStreamID != "" ||
			message.HistorySequence != 0 {
			t.Fatal("private persistence identity reached the provider")
		}
	}
}

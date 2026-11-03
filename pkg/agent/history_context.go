package agent

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

type contextHistoryAppender interface {
	AppendHistoryContext(ctx context.Context, key string, message providers.Message) error
}
type contextHistoryReplacer interface {
	SetHistoryContext(ctx context.Context, key string, messages []providers.Message) error
}

func (ts *turnState) appendUserPrompt(loop *AgentLoop, message providers.Message) providers.Message {
	run, err := uuid.Parse(ts.historyRunID)
	if loop == nil || err != nil || run == uuid.Nil {
		ts.appendHistoryMessage(message)
		return message
	}
	loop.sessionHistoryObserverMu.RLock()
	observed := loop.sessionHistoryReceiptObserver != nil
	loop.sessionHistoryObserverMu.RUnlock()
	writer, supported := ts.agent.Sessions.(session.HistoryReceiptStore)
	if !observed || !supported {
		ts.appendHistoryMessage(message)
		return message
	}
	receipt, err := writer.AppendHistoryReceipt(
		providers.WithHistoryRun(context.Background(), ts.historyRunID),
		ts.sessionKey,
		message,
	)
	switch {
	case errors.Is(err, memory.ErrHistoryInputCommitted):
		warnCommittedHistoryRecovery(ts.sessionKey, err)
		return message
	case errors.Is(err, memory.ErrHistoryUnsupported),
		errors.Is(err, memory.ErrHistoryOwnershipUnavailable),
		errors.Is(err, memory.ErrHistoryReceiptUnavailable):
		// These outcomes reject the receipt before writing; uncertain commits
		// must never reach the legacy append and duplicate the prompt.
		ts.appendHistoryMessage(message)
		return message
	case err != nil:
		logger.WarnCF(
			"agent",
			"Turn history append failed",
			map[string]any{"operation": "append_user_prompt", "error": err.Error()},
		)
		return message
	}
	if !validUserPromptReceipt(receipt, message, ts.historyRunID) {
		logger.WarnCF(
			"agent",
			"User prompt receipt failed validation",
			map[string]any{"operation": "capture_user_prompt"},
		)
		return message
	}
	loop.notifySessionHistoryReceipts(ts.sessionKey, []memory.CommittedHistoryMessage{receipt})
	return receipt.Message
}

func validUserPromptReceipt(receipt memory.CommittedHistoryMessage, input providers.Message, runID string) bool {
	source, err := uuid.Parse(receipt.Message.HistoryID)
	if err != nil || source == uuid.Nil || !receipt.Order.Valid() || receipt.Message.HistoryRunID != runID ||
		receipt.Message.Role != "user" ||
		receipt.Message.HistoryOrderVersion != receipt.Order.Version ||
		receipt.Message.HistoryStreamID != receipt.Order.StreamID ||
		receipt.Message.HistorySequence != receipt.Order.Sequence {
		return false
	}
	input.HistoryID, input.HistoryRunID = receipt.Message.HistoryID, receipt.Message.HistoryRunID
	input.HistoryOrderVersion, input.HistoryStreamID, input.HistorySequence = receipt.Order.Version, receipt.Order.StreamID, receipt.Order.Sequence
	return messagesEquivalent(input, receipt.Message)
}

func (ts *turnState) appendHistoryMessage(message providers.Message) {
	if writer, ok := ts.agent.Sessions.(contextHistoryAppender); ok {
		if err := writer.AppendHistoryContext(
			providers.WithHistoryRun(context.Background(), ts.historyRunID),
			ts.sessionKey,
			message,
		); err != nil {
			logger.WarnCF(
				"agent",
				"Turn history append failed",
				map[string]any{"operation": "append_turn_history", "error": err.Error()},
			)
		}
		return
	}
	ts.agent.Sessions.AddFullMessage(ts.sessionKey, message)
}

func (ts *turnState) replaceHistory(store session.SessionStore, messages []providers.Message) error {
	if writer, ok := store.(contextHistoryReplacer); ok {
		err := writer.SetHistoryContext(
			providers.WithHistoryRun(context.Background(), ts.historyRunID),
			ts.sessionKey,
			messages,
		)
		if err != nil {
			logger.WarnCF(
				"agent",
				"Turn history replacement failed",
				map[string]any{"operation": "replace_turn_history", "error": err.Error()},
			)
		}
		return err
	}
	store.SetHistory(ts.sessionKey, messages)
	return nil
}

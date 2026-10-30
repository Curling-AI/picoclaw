package agent

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

type contextHistoryAppender interface {
	AppendHistoryContext(ctx context.Context, key string, message providers.Message) error
}
type contextHistoryReplacer interface {
	SetHistoryContext(ctx context.Context, key string, messages []providers.Message) error
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

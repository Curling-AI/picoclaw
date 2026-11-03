package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"log/slog"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type PublicHistoryProjectionResult struct {
	Message                providers.Message
	HasVersionedReferences bool
}

type PublicHistoryProjector interface {
	Commit(message providers.Message) (PublicHistoryProjectionResult, error)
	Legacy(message providers.Message) (PublicHistoryProjectionResult, error)
	Verify(ctx context.Context, message providers.Message) error
}

func (s *JSONLStore) SetPublicHistoryProjector(projector PublicHistoryProjector) {
	s.publicProjectionMu.Lock()
	s.publicProjector = projector
	s.publicProjectionMu.Unlock()
	// Constructor recovery can defer an owned cleanup until this capability exists.
	// Release the projector lock before recovery takes individual writer locks.
	if projector != nil {
		if err := s.recoverPendingHistoryWrites(); err != nil {
			log.Printf("memory: pending history cleanup discovery failed: %v", err)
		}
	}
}

func (s *JSONLStore) historyPublicProjector() PublicHistoryProjector {
	s.publicProjectionMu.RLock()
	defer s.publicProjectionMu.RUnlock()
	return s.publicProjector
}

func (s *JSONLStore) projectPublicCommits(messages []providers.Message, meta *SessionMeta) []providers.Message {
	projector := s.historyPublicProjector()
	if projector == nil || meta.publicProjectionUnavailable {
		return messages
	}
	projected := append([]providers.Message(nil), messages...)
	for index, message := range projected {
		if message.HistoryPublicMessage != nil {
			if !validPublicProjection(message, *message.HistoryPublicMessage) {
				meta.publicProjectionUnavailable = true
				return projected
			}
			continue
		}
		public, visible := publicHistoryMessage(message)
		if !visible {
			continue
		}
		var result PublicHistoryProjectionResult
		var resultErr error
		if scoped, ok := projector.(interface {
			CommitSession(key string, message providers.Message) (PublicHistoryProjectionResult, error)
		}); ok {
			result, resultErr = scoped.CommitSession(meta.Key, public)
		} else {
			result, resultErr = projector.Commit(public)
		}
		if resultErr != nil || !validPublicProjection(message, result.Message) {
			meta.publicProjectionUnavailable = true
			slog.Warn("public history projection unavailable", "operation", "commit_public_history")
			return projected
		}
		if result.HasVersionedReferences {
			publishedMessage := result.Message
			projected[index].HistoryPublicMessage = &publishedMessage
		}
	}
	return projected
}

func decodedPublicMessage(data []byte) *providers.Message {
	var raw struct {
		Message *providers.Message `json:"history_public_message"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	return raw.Message
}

func (s *JSONLStore) projectLegacyPublic(message providers.Message) (providers.Message, error) {
	if message.HistoryPublicMessage != nil {
		if !validPublicProjection(message, *message.HistoryPublicMessage) {
			return providers.Message{}, ErrPublicHistoryCoverage
		}
		return *message.HistoryPublicMessage, nil
	}
	projector := s.historyPublicProjector()
	if projector == nil {
		return message, nil
	}
	result, resultErr1 := projector.Legacy(message)
	return result.Message, resultErr1
}

func validPublicProjection(original, projected providers.Message) bool {
	safe, ok := publicHistoryMessage(original)
	if !ok {
		return false
	}
	safe.Content, safe.Media, safe.Attachments = projected.Content, projected.Media, projected.Attachments
	expected, _ := json.Marshal(safe)
	actual, _ := json.Marshal(projected)
	return bytes.Equal(expected, actual)
}

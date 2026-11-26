package agent

import (
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/session"
)

// refreshTurnSession rereads the turn's session from storage once, before the
// turn first reads it. Another process sharing sessions/ (the other pod of a
// rollout) may have written the session since this one cached its meta, and
// the store's plain reads serve that cache. A failure is logged: the turn's
// reads report their own errors.
func refreshTurnSession(store session.SessionStore, key string) {
	refresher, ok := store.(session.SessionRefresher)
	if !ok {
		return
	}
	if err := refresher.RefreshSession(key); err != nil {
		logger.WarnCF("agent", "Session refresh failed", map[string]any{
			"session_key": key,
			"error":       err.Error(),
		})
	}
}

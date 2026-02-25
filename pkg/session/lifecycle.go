package session

import (
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

// LifecycleManager handles session reset policies (daily reset, idle expiry).
type LifecycleManager struct {
	sessions *SessionManager
	policy   *config.ResetPolicyConfig
	tz       *time.Location
}

// NewLifecycleManager creates a lifecycle manager. If policy is nil, no resets occur.
func NewLifecycleManager(sm *SessionManager, policy *config.ResetPolicyConfig) *LifecycleManager {
	lm := &LifecycleManager{
		sessions: sm,
		policy:   policy,
		tz:       time.UTC,
	}
	if policy != nil && policy.Timezone != "" {
		if loc, err := time.LoadLocation(policy.Timezone); err == nil {
			lm.tz = loc
		}
	}
	return lm
}

// ShouldReset checks whether the session should be reset based on the configured policy.
// Returns (shouldReset, reason). Evaluated lazily on next inbound message.
func (lm *LifecycleManager) ShouldReset(sessionKey string) (bool, string) {
	if lm.policy == nil {
		return false, ""
	}

	session := lm.sessions.GetSession(sessionKey)
	if session == nil {
		return false, ""
	}

	now := time.Now().In(lm.tz)

	// Check daily reset hour
	if lm.policy.DailyResetHour != nil {
		resetHour := *lm.policy.DailyResetHour
		if resetHour >= 0 && resetHour <= 23 {
			lastUpdate := session.Updated.In(lm.tz)
			// Build today's reset time
			todayReset := time.Date(now.Year(), now.Month(), now.Day(), resetHour, 0, 0, 0, lm.tz)
			// If current time is past today's reset and last update was before it
			if now.After(todayReset) && lastUpdate.Before(todayReset) {
				return true, "daily reset"
			}
		}
	}

	// Check idle expiry
	if lm.policy.IdleExpiryMins > 0 {
		idleDuration := time.Duration(lm.policy.IdleExpiryMins) * time.Minute
		if time.Since(session.Updated) > idleDuration {
			return true, "idle expiry"
		}
	}

	return false, ""
}

// Reset archives the current session and clears it for a fresh start.
func (lm *LifecycleManager) Reset(sessionKey string, reason string) error {
	// Append meta event to transcript
	tw := lm.sessions.GetTranscriptWriter(sessionKey)
	if tw != nil {
		tw.Append(TranscriptEntry{
			Kind: "meta",
			Meta: map[string]any{
				"action": "reset",
				"reason": reason,
			},
		})
	}

	// Clear in-memory messages and summary
	lm.sessions.ClearSession(sessionKey)

	// Update record store
	if rs := lm.sessions.GetRecordStore(); rs != nil {
		rs.Update(sessionKey, func(r *SessionRecord) {
			r.MessageCount = 0
			r.HasSummary = false
		})
		rs.Save()
	}

	return nil
}

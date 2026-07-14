package evolution

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// negativeFeedbackStatus marks a task record dropped by user feedback. It is
// any non-"new" status, so coldPathEvidenceRejectReason excludes it from
// judging/clustering (a downvoted turn must never become a learned skill).
const negativeFeedbackStatus RecordStatus = "rejected"

// decayRetentionScore is the inverse of nextRetentionScore: a downvote pushes a
// skill toward the lifecycle's cold/archive/delete thresholds. Floors at 0 so
// the score never goes negative.
func decayRetentionScore(current float64) float64 {
	next := current - 0.2
	if next < 0 {
		return 0
	}
	return next
}

// MarkTaskRecordUnsuccessful flags the task record(s) of a downvoted turn as a
// failure so they are excluded from clustering and count against the cluster's
// success ratio. Matching key: SessionKey, narrowed by excerpt (a snippet of
// the downvoted response) against FinalOutput when provided. Returns the skill
// names used/active in the matched turns, deduped, so the caller can decay
// their retention. Mirrors MarkTaskRecordsClustered's load/lock/save flow.
func (s *Store) MarkTaskRecordUnsuccessful(sessionKey, excerpt string) ([]string, error) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return nil, nil
	}
	needle := normalizeForMatch(excerpt)

	unlock := lockStoreFile(s.paths.TaskRecords)
	defer unlock()

	current, err := s.loadRecordsFromPath(s.paths.TaskRecords)
	if err != nil {
		return nil, err
	}
	legacy, err := s.loadLegacyTaskRecords()
	if err != nil {
		return nil, err
	}
	records := mergeLearningRecordsByID(legacy, current)

	skillSet := map[string]struct{}{}
	changed := false
	for i := range records {
		if records[i].SessionKey != sessionKey {
			continue
		}
		if needle != "" && !strings.Contains(normalizeForMatch(records[i].FinalOutput), needle) {
			continue
		}
		no := false
		records[i].Success = &no
		records[i].Status = negativeFeedbackStatus
		for _, n := range records[i].UsedSkillNames {
			skillSet[n] = struct{}{}
		}
		for _, n := range records[i].ActiveSkillNames {
			skillSet[n] = struct{}{}
		}
		changed = true
	}
	if !changed {
		return nil, nil
	}
	if err := s.saveJSONLRecordsLocked(s.paths.TaskRecords, records); err != nil {
		return nil, err
	}

	skills := make([]string, 0, len(skillSet))
	for n := range skillSet {
		if n = strings.TrimSpace(n); n != "" {
			skills = append(skills, n)
		}
	}
	return skills, nil
}

// normalizeForMatch collapses whitespace and lowercases so an excerpt matches
// its FinalOutput despite minor formatting differences.
func normalizeForMatch(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// ApplyNegativeFeedback records a user's 👎 for a turn: it marks the turn's
// task record(s) as unsuccessful (so evolution never learns a skill from a
// downvoted answer) and decays the retention of every skill that was active in
// that turn (so a bad skill drifts toward retirement). workspace is the agent
// workspace path — the same key touchSkillProfile stores profiles under.
// Best-effort and silent when nothing matches; feedback must never break a turn.
//
// Defined on Store (not Runtime) so the gRPC handler — which runs in the pod
// process alongside the live Runtime and shares the package-level store file
// locks — can construct a Store over the same state dir and call this directly.
func (s *Store) ApplyNegativeFeedback(workspace, sessionKey, excerpt string) error {
	if s == nil {
		return nil
	}
	skills, err := s.MarkTaskRecordUnsuccessful(sessionKey, excerpt)
	if err != nil {
		return err
	}
	if len(skills) == 0 {
		logger.DebugCF("evolution", "negative feedback: no matching task record",
			map[string]any{"session_key": sessionKey})
		return nil
	}
	for _, name := range skills {
		if uerr := s.UpdateProfile(workspace, name, func(p *SkillProfile, exists bool) error {
			if !exists {
				return nil // no profile yet → nothing to decay
			}
			p.RetentionScore = decayRetentionScore(p.RetentionScore)
			return nil
		}); uerr != nil {
			logger.WarnCF("evolution", "negative feedback: failed to decay skill retention",
				map[string]any{"skill": name, "error": uerr.Error()})
		}
	}
	logger.InfoCF("evolution", "applied negative feedback",
		map[string]any{"session_key": sessionKey, "skills_decayed": len(skills)})
	return nil
}

package memory

import "github.com/sipeed/picoclaw/pkg/providers"

// HistoryIDs preserve display association across an unqualified rewrite; they
// do not restore independent ContentRef authority lost by that rewrite.
func (s *JSONLStore) rewritePublicDisplay(
	key string,
	original SessionMeta,
	next *SessionMeta,
	messages []providers.Message,
	entries []PublicHistoryEntry,
) (publicHistoryChange, error) {
	old, err := s.readPublicSource(key, original, original.Skip)
	if err != nil {
		return publicHistoryChange{}, err
	}
	active := make(map[string]providers.Message, len(old))
	for _, message := range old {
		active[message.HistoryID] = message
	}
	changed := make(map[string]providers.Message, len(messages))
	for _, message := range messages {
		changed[message.HistoryID] = message
	}
	replacements := publicHistoryEntries(messages, *next)
	for i := range replacements {
		replacements[i].CallID = ""
	}
	updated := make(map[string]PublicHistoryEntry, len(replacements))
	for _, entry := range replacements {
		updated[entry.ID] = entry
	}
	seen := make(map[string]bool, len(entries))
	kept := make([]PublicHistoryEntry, 0, len(entries)+len(replacements))
	for _, entry := range entries {
		if entry.ImportID != "" {
			if _, wasActive := active[entry.BoundaryID]; wasActive {
				if _, exists := changed[entry.BoundaryID]; !exists {
					continue
				}
			}
			if anchor, wasActive := active[entry.AnchorID]; wasActive {
				replacement, exists := changed[entry.AnchorID]
				if !exists || !sameHistoryCore(anchor, replacement) {
					continue
				}
			}
		} else if originalMessage, wasActive := active[entry.ID]; wasActive {
			replacement, exists := updated[entry.ID]
			if !exists {
				continue
			}
			if original.HistoryGeneration != "" && next.HistoryGeneration == original.HistoryGeneration &&
				sameHistoryCore(originalMessage, changed[entry.ID]) {
				replacement.CallID = entry.CallID
			}
			entry = replacement
		}
		kept = append(kept, entry)
		seen[entry.ID] = true
	}
	for _, entry := range replacements {
		if !seen[entry.ID] {
			kept = append(kept, entry)
		}
	}
	if next.ContentIdentity == nil || next.ContentIdentity.Unavailable {
		for i := range kept {
			kept[i].Ref = nil
			kept[i].AnchorRef = nil
		}
	}
	if next.HistoryGeneration != original.HistoryGeneration {
		next.PublicHistoryGeneration = newHistoryID()
	}
	return s.replacePublicEntries(key, kept, next)
}

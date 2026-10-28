package agent

import "github.com/sipeed/picoclaw/pkg/memory"

// SetSessionHistoryContentObserver opts into observing exact out-of-turn content
// commits, independently of receipt eligibility. Entries with no Ref remain
// unproved content; they must not be matched to an old delivery by similarity.
// The callback runs before ingestion and must retain its immutable batch before
// scheduling network work. This setter never initializes a source's identity.
func (al *AgentLoop) SetSessionHistoryContentObserver(observer func(string, []memory.ContentEntry)) {
	al.sessionHistoryObserverMu.Lock()
	defer al.sessionHistoryObserverMu.Unlock()
	al.sessionHistoryContentObserver = observer
}

func (al *AgentLoop) observesSessionHistoryContent() bool {
	al.sessionHistoryObserverMu.RLock()
	defer al.sessionHistoryObserverMu.RUnlock()
	return al.sessionHistoryContentObserver != nil
}

func (al *AgentLoop) notifySessionHistoryContent(key string, entries []memory.ContentEntry) {
	al.sessionHistoryObserverMu.RLock()
	observer := al.sessionHistoryContentObserver
	al.sessionHistoryObserverMu.RUnlock()
	if observer != nil {
		observer(key, entries)
	}
}

// SetSessionHistoryReceiptObserver observes exact committed out-of-turn writes.
// It runs synchronously; enqueue network work only after retaining these receipts.
// A nil batch is a durable legacy-history notification with no receipt or order
// evidence. It is emitted only after the fallback's existing Save/checked gate.
func (al *AgentLoop) SetSessionHistoryReceiptObserver(
	observer func(sessionKey string, committed []memory.CommittedHistoryMessage),
) {
	al.sessionHistoryObserverMu.Lock()
	defer al.sessionHistoryObserverMu.Unlock()
	al.sessionHistoryReceiptObserver = observer
}

func (al *AgentLoop) notifySessionHistoryReceipts(sessionKey string, committed []memory.CommittedHistoryMessage) {
	al.sessionHistoryObserverMu.RLock()
	observer := al.sessionHistoryReceiptObserver
	al.sessionHistoryObserverMu.RUnlock()
	if observer != nil {
		observer(sessionKey, committed)
	}
}

// SetSessionHistoryObserver observes persisted out-of-turn history writes.
// The callback runs synchronously and must enqueue any network work.
func (al *AgentLoop) SetSessionHistoryObserver(observer func(sessionKey string)) {
	al.sessionHistoryObserverMu.Lock()
	defer al.sessionHistoryObserverMu.Unlock()
	al.sessionHistoryObserver = observer
}

func (al *AgentLoop) notifySessionHistoryChanged(sessionKey string) {
	al.sessionHistoryObserverMu.RLock()
	observer := al.sessionHistoryObserver
	al.sessionHistoryObserverMu.RUnlock()
	if observer != nil {
		observer(sessionKey)
	}
}

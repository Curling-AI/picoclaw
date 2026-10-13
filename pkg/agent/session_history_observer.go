package agent

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

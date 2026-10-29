package agent

import (
	"encoding/json"

	"github.com/sipeed/picoclaw/pkg/memory"
)

// SetSessionHistoryContentInputObserver observes accepted content before
// ingestion, independently of order or prefix qualification. Entries contain no
// Ref or receipt binding. Consumers use the key/nonempty batch as a wake-up and
// obtain a strict current frame before publishing any identity or read state.
// Register this instead of a qualified observer when only a wake-up is needed;
// registration never initializes a source.
func (al *AgentLoop) SetSessionHistoryContentInputObserver(observer func(string, []memory.ContentEntry)) {
	al.sessionHistoryObserverMu.Lock()
	defer al.sessionHistoryObserverMu.Unlock()
	al.sessionHistoryContentInputObserver = observer
}

func (al *AgentLoop) observesSessionHistoryContentInput() bool {
	al.sessionHistoryObserverMu.RLock()
	defer al.sessionHistoryObserverMu.RUnlock()
	return al.sessionHistoryContentInputObserver != nil
}

func (al *AgentLoop) notifySessionHistoryContentInputs(key string, entries []memory.ContentEntry) error {
	al.sessionHistoryObserverMu.RLock()
	observer := al.sessionHistoryContentInputObserver
	al.sessionHistoryObserverMu.RUnlock()
	if observer == nil {
		return nil
	}
	stripped := make([]memory.ContentEntry, len(entries))
	for i, entry := range entries {
		stripped[i] = memory.ContentEntry{Message: entry.Message}
	}
	data, err := json.Marshal(stripped)
	if err != nil {
		return err
	}
	var detached []memory.ContentEntry
	if err = json.Unmarshal(data, &detached); err != nil {
		return err
	}
	observer(key, detached)
	return nil
}

// SetSessionHistoryContentCommitObserver observes each append's exact entry and
// state interval before ingestion or asynchronous work. Intervening appends
// remain gaps between results; the callback must not merge their state tokens.
// The v5 entry-only setter and receipt observers keep their existing contracts.
func (al *AgentLoop) SetSessionHistoryContentCommitObserver(observer func(string, []memory.ContentAppendResult)) {
	al.sessionHistoryObserverMu.Lock()
	defer al.sessionHistoryObserverMu.Unlock()
	al.sessionHistoryContentCommitObserver = observer
}

func (al *AgentLoop) notifySessionHistoryContentCommits(key string, committed []memory.ContentAppendResult) error {
	al.sessionHistoryObserverMu.RLock()
	observer := al.sessionHistoryContentCommitObserver
	al.sessionHistoryObserverMu.RUnlock()
	if observer == nil {
		return nil
	}
	// Each callback owns its detached payload. An entry-only observer or ingest
	// must not mutate a retained commit snapshot, or receive its mutations.
	type snapshot struct {
		Result  memory.ContentAppendResult      `json:"result"`
		Receipt *memory.CommittedHistoryMessage `json:"receipt,omitempty"`
	}
	snapshots := make([]snapshot, len(committed))
	for i, result := range committed {
		snapshots[i] = snapshot{Result: result, Receipt: result.Receipt}
	}
	data, err := json.Marshal(snapshots)
	if err != nil {
		return err
	}
	var detached []snapshot
	if err = json.Unmarshal(data, &detached); err != nil {
		return err
	}
	results := make([]memory.ContentAppendResult, len(detached))
	for i, item := range detached {
		if receipt := item.Receipt; receipt != nil {
			// Provider JSON intentionally omits these existing private fields.
			original := committed[i].Receipt
			receipt.Message.HistoryID = original.Message.HistoryID
			receipt.Message.HistoryRunID = original.Message.HistoryRunID
			receipt.Message.HistoryOrderVersion = original.Message.HistoryOrderVersion
			receipt.Message.HistoryStreamID = original.Message.HistoryStreamID
			receipt.Message.HistorySequence = original.Message.HistorySequence
			receipt.LegacyBeforeStreamID = original.LegacyBeforeStreamID
		}
		item.Result.Receipt = item.Receipt
		results[i] = item.Result
	}
	observer(key, results)
	return nil
}

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
	return al.sessionHistoryContentObserver != nil || al.sessionHistoryContentCommitObserver != nil
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

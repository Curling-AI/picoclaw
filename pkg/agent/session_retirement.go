package agent

import "context"

// RetireSession applies deletion under the same boundary that discards late
// results after /clear. The caller first drains the conversation's active turn.
func (al *AgentLoop) RetireSession(ctx context.Context, key string, remove func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stripe := al.mirror.stripe(key)
	stripe.Lock()
	defer stripe.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	al.dropMirroredDeliveriesLocked(key)
	if err := remove(); err != nil {
		return err
	}
	// Envelopes can resolve/stamp while remove is running, before they take
	// this stripe. Retire those too before releasing the completed deletion.
	al.dropMirroredDeliveriesLocked(key)
	return nil
}

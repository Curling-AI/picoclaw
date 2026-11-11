package agent

import "context"

// PrepareExternalTools uses the same lazy initialization as a normal turn.
func (al *AgentLoop) PrepareExternalTools(ctx context.Context) error {
	if err := al.ensureHooksInitialized(ctx); err != nil {
		return err
	}
	return al.ensureMCPInitialized(ctx)
}

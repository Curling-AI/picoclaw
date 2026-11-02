package agent

import "github.com/sipeed/picoclaw/pkg/memory"

// SetPublicHistoryProjector installs the display-only producer boundary on
// current and reloaded registries before either one accepts a new turn.
func (al *AgentLoop) SetPublicHistoryProjector(factory func(workspace string) memory.PublicHistoryProjector) {
	al.publicHistoryMu.Lock()
	al.publicHistoryFactory = factory
	al.publicHistoryMu.Unlock()
	al.projectPublicHistoryRegistry(al.GetRegistry())
}

func (al *AgentLoop) projectPublicHistoryRegistry(registry *AgentRegistry) {
	if registry == nil {
		return
	}
	al.publicHistoryMu.RLock()
	factory := al.publicHistoryFactory
	al.publicHistoryMu.RUnlock()
	if factory == nil {
		return
	}
	for _, id := range registry.ListAgentIDs() {
		instance, ok := registry.GetAgent(id)
		if !ok {
			continue
		}
		if store, ok := instance.Sessions.(interface {
			SetPublicHistoryProjector(projector memory.PublicHistoryProjector)
		}); ok {
			store.SetPublicHistoryProjector(factory(instance.Workspace))
		}
	}
}

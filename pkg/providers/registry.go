// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package providers

import "sync"

// ProviderRegistry maps (provider, model) pairs to LLMProvider instances.
// It is used by the fallback chain to dispatch each candidate to the correct provider.
type ProviderRegistry struct {
	mu       sync.RWMutex
	entries  map[string]LLMProvider
	fallback LLMProvider
}

// NewProviderRegistry creates a registry that returns fallback for unknown keys.
func NewProviderRegistry(fallback LLMProvider) *ProviderRegistry {
	return &ProviderRegistry{
		entries:  make(map[string]LLMProvider),
		fallback: fallback,
	}
}

// Register associates a provider/model pair with an LLMProvider.
func (r *ProviderRegistry) Register(provider, model string, p LLMProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[ModelKey(provider, model)] = p
}

// Get returns the LLMProvider for the given provider/model, or the fallback if not found.
func (r *ProviderRegistry) Get(provider, model string) LLMProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.entries[ModelKey(provider, model)]; ok {
		return p
	}
	return r.fallback
}

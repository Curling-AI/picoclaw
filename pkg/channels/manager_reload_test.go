package channels

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

// Reload must STOP the channel that left the config, not merely forget it.
//
// The case that motivated this test is the Slack migration from Socket Mode to
// webhook: the channel changes shape, its hash changes, and the old one has to
// drop the websocket. If it stays up, the pod keeps holding the very connection
// the migration exists to remove — and it does so quietly, because the new
// channel works too.
//
// Reload had its helpers covered (toChannelHashes, compareChannels) but not the
// added/removed path, and the control plane now depends on it.
func TestReloadStopsChannelDroppedFromConfig(t *testing.T) {
	old := config.DefaultConfig()
	old.Channels["slack"] = &config.Channel{
		Enabled:  true,
		Settings: config.RawNode(`{"app_token":"xapp-socket"}`),
	}

	stopped := false
	ch := &mockChannel{stopFn: func(context.Context) error {
		stopped = true
		return nil
	}}

	m := &Manager{
		channels:      map[string]Channel{"slack": ch},
		workers:       make(map[string]*channelWorker),
		bus:           bus.NewMessageBus(),
		config:        old,
		channelHashes: toChannelHashes(old),
	}

	// New config without the channel: this is what `removed` must catch.
	if err := m.Reload(context.Background(), config.DefaultConfig()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if !stopped {
		t.Error("the removed channel never got Stop — the old websocket is still up")
	}
	if _, still := m.channels["slack"]; still {
		t.Error("the removed channel is still registered in the manager")
	}
}

// A channel that did NOT change must not be restarted.
//
// This is what separates "reconcile" from "restart everything": tearing down an
// untouched channel over a change that is not its own costs a window of
// unavailability for nothing.
func TestReloadLeavesUnchangedChannelAlone(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Channels["slack"] = &config.Channel{
		Enabled:  true,
		Settings: config.RawNode(`{"bot_token":"xoxb-same"}`),
	}

	stopped := false
	ch := &mockChannel{stopFn: func(context.Context) error {
		stopped = true
		return nil
	}}

	m := &Manager{
		channels:      map[string]Channel{"slack": ch},
		workers:       make(map[string]*channelWorker),
		bus:           bus.NewMessageBus(),
		config:        cfg,
		channelHashes: toChannelHashes(cfg),
	}

	// Same channel, config reloaded: the hash does not move.
	same := config.DefaultConfig()
	same.Channels["slack"] = &config.Channel{
		Enabled:  true,
		Settings: config.RawNode(`{"bot_token":"xoxb-same"}`),
	}
	if err := m.Reload(context.Background(), same); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if stopped {
		t.Error("an unchanged channel was stopped — Reload is tearing down what did not change")
	}
	if _, ok := m.channels["slack"]; !ok {
		t.Error("the unchanged channel disappeared from the manager")
	}
}

// The `grpc` channel is registered by main, outside the config, and must not
// vanish on a reload: it is how the web conversation gets its replies.
func TestReloadKeepsChannelRegisteredOutsideConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	m := &Manager{
		channels:      map[string]Channel{"grpc": &mockChannel{}},
		workers:       make(map[string]*channelWorker),
		bus:           bus.NewMessageBus(),
		config:        cfg,
		channelHashes: toChannelHashes(cfg),
	}

	if err := m.Reload(context.Background(), config.DefaultConfig()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := m.channels["grpc"]; !ok {
		t.Error("the grpc channel vanished on reload — the web stops receiving replies")
	}
}

// A channel can be enabled in the config and still never come up: the factory
// is not registered, or it failed, or the settings were not ready (Slack
// without a token, say). channelHashes is built from the config alone, so such
// a channel is tracked as if it were running. When the next ApplyConfig drops
// it, Reload finds it in `removed`, looks it up, gets a nil interface and
// calls Stop on it — SIGSEGV at addr=0x50, the itab slot of Stop.
//
// This is the crash from the Sep 13 22:00 CEST incident, reached through
// ApplyConfig → main.go callback → channelManager.Reload.
func TestReloadSurvivesRemovingChannelThatNeverCameUp(t *testing.T) {
	old := config.DefaultConfig()
	old.Channels["slack"] = &config.Channel{
		Enabled:  true,
		Settings: config.RawNode(`{"app_token":"xapp-socket"}`),
	}

	m := &Manager{
		channels:      map[string]Channel{}, // enabled in config, never registered
		workers:       make(map[string]*channelWorker),
		bus:           bus.NewMessageBus(),
		config:        old,
		channelHashes: toChannelHashes(old),
	}

	if err := m.Reload(context.Background(), config.DefaultConfig()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := m.channelHashes["slack"]; ok {
		t.Error("the dropped channel is still tracked in channelHashes")
	}
}

// Same hole on the `added` side: a channel that fails to initialize again on
// reload (no factory for its type) is not in m.channels, and Start on the nil
// interface panics just like Stop did.
func TestReloadSurvivesChangingChannelThatCannotInit(t *testing.T) {
	old := config.DefaultConfig()
	old.Channels["ghost"] = &config.Channel{
		Enabled:  true,
		Type:     "no-such-channel-type",
		Settings: config.RawNode(`{"v":1}`),
	}

	m := &Manager{
		channels:      map[string]Channel{},
		workers:       make(map[string]*channelWorker),
		bus:           bus.NewMessageBus(),
		config:        old,
		channelHashes: toChannelHashes(old),
	}

	// Same channel, changed settings: it lands in both `removed` and `added`.
	next := config.DefaultConfig()
	next.Channels["ghost"] = &config.Channel{
		Enabled:  true,
		Type:     "no-such-channel-type",
		Settings: config.RawNode(`{"v":2}`),
	}
	if err := m.Reload(context.Background(), next); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := m.channels["ghost"]; ok {
		t.Error("a channel that never initialized got registered as nil")
	}
	if _, ok := m.workers["ghost"]; ok {
		t.Error("a worker was created for a channel that never started")
	}
}

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

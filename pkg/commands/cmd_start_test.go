package commands

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

func runStart(rt *Runtime) string {
	var got string
	_ = startCommand().Handler(context.Background(), Request{
		Reply: func(text string) error { got = text; return nil },
	}, rt)
	return got
}

func TestStartCommand_DynamicBotName(t *testing.T) {
	// Falls back to "PicoClaw" with no config.
	if got := runStart(nil); got != "Hello! I am PicoClaw 🦞" {
		t.Errorf("nil runtime: got %q", got)
	}
	// Uses the configured display name when set.
	rt := &Runtime{Config: &config.Config{}}
	rt.Config.Agents.Defaults.Name = "Seu Caranguejo"
	if got := runStart(rt); got != "Hello! I am Seu Caranguejo 🦞" {
		t.Errorf("named runtime: got %q", got)
	}
	// Empty name falls back to default.
	rt.Config.Agents.Defaults.Name = ""
	if got := runStart(rt); got != "Hello! I am PicoClaw 🦞" {
		t.Errorf("empty name: got %q", got)
	}
}

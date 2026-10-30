package cliprovider_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	cliprovider "github.com/sipeed/picoclaw/pkg/providers/cli"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func TestInternalToolProvidersRefuseRestrictedExecutionBeforeEffects(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "subprocess-effect")
	t.Setenv("MST394_PROVIDER_MARKER", marker)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for name, output := range map[string]string{
		"codex":  `{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`,
		"claude": `{"type":"result","result":"done","is_error":false}`,
	} {
		script := "#!/bin/sh\ncat >/dev/null\nprintf effected > \"$MST394_PROVIDER_MARKER\"\nprintf '%s\\n' '" + output + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	restricted := tools.WithExecutionPolicy(t.Context(), tools.ExecutionPolicy{Skills: []string{}})
	for _, item := range []struct {
		name     string
		provider providers.LLMProvider
	}{
		{"codex", cliprovider.NewCodexCliProvider(dir)},
		{"claude", cliprovider.NewClaudeCliProvider(dir)},
		{"copilot", &cliprovider.GitHubCopilotProvider{}},
	} {
		t.Run(item.name, func(t *testing.T) {
			for name, ctx := range map[string]context.Context{"direct": restricted, "child": context.WithoutCancel(restricted)} {
				t.Run(name, func(t *testing.T) {
					_, err := item.provider.Chat(ctx, []providers.Message{{Role: "user", Content: "do the task"}}, nil, "", nil)
					if err == nil || !strings.Contains(err.Error(), "restricted execution") {
						t.Fatalf("internal provider did not refuse the execution policy: %v", err)
					}
					if _, err := os.Stat(marker); !os.IsNotExist(err) {
						t.Fatalf("restricted provider started a subprocess: %v", err)
					}
				})
			}
			t.Run("fallback", func(t *testing.T) {
				chain := providers.NewFallbackChain(providers.NewCooldownTracker(), nil)
				reached := false
				_, err := chain.Execute(restricted, []providers.FallbackCandidate{{Provider: "primary", Model: "primary"}, {Provider: item.name, Model: "internal"}}, func(ctx context.Context, provider, _ string) (*providers.LLMResponse, error) {
					if provider == "primary" {
						return nil, fmt.Errorf("HTTP 429 rate limit exceeded")
					}
					reached = true
					return item.provider.Chat(ctx, []providers.Message{{Role: "user", Content: "do the task"}}, nil, "", nil)
				})
				if !reached || err == nil || !strings.Contains(err.Error(), "restricted execution") {
					t.Fatalf("fallback lost the restricted execution policy: reached=%v err=%v", reached, err)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("fallback started an internal subprocess: %v", err)
				}
			})
			if item.name == "copilot" {
				return
			}
			if _, err := item.provider.Chat(t.Context(), []providers.Message{{Role: "user", Content: "do the task"}}, nil, "", nil); err != nil {
				t.Fatalf("unrestricted provider changed: %v", err)
			}
			if err := os.Remove(marker); err != nil {
				t.Fatalf("unrestricted provider never ran the synthetic subprocess: %v", err)
			}
		})
	}
}

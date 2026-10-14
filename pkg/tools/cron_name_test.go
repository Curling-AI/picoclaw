package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
)

func TestCronAddUsesTaskNameWithoutChangingPayload(t *testing.T) {
	for _, tc := range []struct{ name, message, want string }{
		{"Resumo diário de notícias", "Rodada da automação: pesquisar notícias e entregar o resumo", "Resumo diário de notícias"},
		{"Rodada da automação — Resumo diário de notícias", "Pesquisar notícias", "Resumo diário de notícias"},
		{"", "Rodada da automação\nResumo diário de notícias", "Resumo diário de notícias"},
	} {
		t.Run(tc.name+tc.message, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			jobs := cron.NewCronService(path, nil)
			tool, err := NewCronTool(jobs, nil, nil, t.TempDir(), true, 0, &config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"action": "add", "message": tc.message, "every_seconds": float64(3600)}
			if tc.name != "" {
				args["name"] = tc.name
			}
			result := tool.Execute(WithToolContext(context.Background(), "grpc", "run"), args)
			if result.IsError {
				t.Fatal(result.ForLLM)
			}
			saved := cron.NewCronService(path, nil).ListJobs(true)
			if len(saved) != 1 || saved[0].Name != tc.want || saved[0].Payload.Message != tc.message || saved[0].Payload.To != "run" {
				t.Fatalf("incorrect persisted job: %+v", saved)
			}
		})
	}
}

func TestCronAddRejectsInvalidNameBeforeCreation(t *testing.T) {
	jobs := cron.NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	tool, err := NewCronTool(jobs, nil, nil, t.TempDir(), true, 0, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []any{42, "   "} {
		result := tool.Execute(WithToolContext(context.Background(), "grpc", "run"), map[string]any{
			"action": "add", "name": name, "message": "Pesquisar notícias", "every_seconds": float64(3600),
		})
		if !result.IsError || len(jobs.ListJobs(true)) != 0 {
			t.Fatalf("invalid name created a job: %+v", result)
		}
	}
}

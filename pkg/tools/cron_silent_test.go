package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
	"github.com/sipeed/picoclaw/pkg/turnresult"
)

// Job silencioso: o executor recebe Request.Silent, o prompt troca "rode em
// silêncio" por uma regra de entrega, e nada do texto final é publicado —
// nem a narração do silêncio que o modelo costuma devolver. (seucaranguejo fork)
func TestCronTool_SilentJobPublishesNothing(t *testing.T) {
	var executor typedJobExecutor
	executor.status = turnresult.Completed
	executor.response = "(vazio)"
	tool := newTestCronToolWithExecutorAndConfig(t, &executor, config.DefaultConfig())

	job := &cron.CronJob{ID: "job-silent"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "check the inbox"
	job.Payload.Silent = true

	if got := tool.ExecuteJob(context.Background(), job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}
	if !executor.request.Silent {
		t.Fatal("executor request is not silent")
	}
	if !strings.Contains(executor.request.Content, "check the inbox") ||
		!strings.Contains(executor.request.Content, "this job is SILENT") ||
		!strings.Contains(executor.request.Content, "message tool") {
		t.Fatalf("prompt = %q, want the silent delivery rule", executor.request.Content)
	}
	if executor.publishedResp != "" {
		t.Fatalf("silent job published %q, want nothing", executor.publishedResp)
	}
}

func TestCronTool_NonSilentJobPromptAndPublishUnchanged(t *testing.T) {
	var executor typedJobExecutor
	executor.status = turnresult.Completed
	executor.response = "generated reply"
	tool := newTestCronToolWithExecutorAndConfig(t, &executor, config.DefaultConfig())

	job := &cron.CronJob{ID: "job-loud"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "send me a poem"

	if got := tool.ExecuteJob(context.Background(), job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}
	if executor.request.Silent {
		t.Fatal("non-silent job sent a silent request")
	}
	if strings.Contains(executor.request.Content, "SILENT") ||
		!strings.HasSuffix(executor.request.Content, "never use the daily notes as a state store.)") {
		t.Fatalf("prompt = %q, want the regular note unchanged", executor.request.Content)
	}
	if executor.publishedResp != "generated reply" {
		t.Fatalf("published = %q, want generated reply", executor.publishedResp)
	}
}

func TestCronSilentCommandJobPublishesOnlyFailures(t *testing.T) {
	for _, tc := range []struct {
		command       string
		wantPublished bool
	}{
		{"echo nada-novo", false},
		{"exit 3", true},
	} {
		t.Run(tc.command, func(t *testing.T) {
			var executor mirroringJobExecutor
			tool := newTestCronToolWithExecutorAndConfig(t, &executor, config.DefaultConfig())
			var job cron.CronJob
			job.ID = "cmd-silent"
			job.Payload.Command = tc.command
			job.Payload.Channel = "telegram"
			job.Payload.To = "123"
			job.Payload.Silent = true

			_, _ = tool.ExecuteJobWithResult(
				context.Background(),
				cron.Execution{Progress: nil, RunID: "11111111-1111-4111-8111-111111111111", Trigger: cron.Scheduled, Job: &job},
			)

			var published bool
			select {
			case <-tool.msgBus.OutboundChan():
				published = true
			default:
			}
			if published != tc.wantPublished {
				t.Fatalf("published = %v, want %v", published, tc.wantPublished)
			}
			if len(executor.mirrored) != 0 {
				t.Fatalf("mirrored %+v, want nothing", executor.mirrored)
			}
		})
	}
}

func TestCronTool_AddAndUpdateSilent(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")

	added := tool.Execute(ctx, map[string]any{
		"action": "add", "message": "watch prices", "every_seconds": float64(600), "silent": true,
	})
	if added.IsError {
		t.Fatalf("add: %s", added.ForLLM)
	}
	jobs := tool.cronService.ListJobs(true)
	if len(jobs) != 1 || !jobs[0].Payload.Silent {
		t.Fatalf("jobs = %+v, want one silent job", jobs)
	}
	id := jobs[0].ID

	updated := tool.Execute(ctx, map[string]any{"action": "update", "job_id": id, "silent": false})
	if updated.IsError || parseCronJobResult(t, updated).Payload.Silent {
		t.Fatalf("update silent=false: %s", updated.ForLLM)
	}
	if bad := tool.Execute(ctx, map[string]any{"action": "update", "job_id": id, "silent": "yes"}); !bad.IsError {
		t.Fatal("update accepted a non-boolean silent")
	}
}

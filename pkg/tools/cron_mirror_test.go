package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
)

type mirroredDelivery struct {
	origin, channel, chatID, text string
}

type mirroringJobExecutor struct {
	stubJobExecutor
	mirrored []mirroredDelivery
}

func (m *mirroringJobExecutor) MirrorDelivery(d ExternalDelivery) {
	m.mirrored = append(m.mirrored, mirroredDelivery{d.Origin, d.Channel, d.ChatID, d.Text})
}

func runCommandJob(t *testing.T, executor JobExecutor, command string) string {
	t.Helper()
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())
	var job cron.CronJob
	job.ID = "cmd-job"
	job.Payload.Command = command
	job.Payload.Channel = "telegram"
	job.Payload.To = "123"
	runID := uuid.NewString()
	_, _ = tool.ExecuteJobWithResult(
		context.Background(),
		cron.Execution{Progress: nil, RunID: runID, Trigger: cron.Scheduled, Job: &job},
	)
	return "agent:cron-cmd-job-" + runID
}

// A command job's output goes to the chat without a turn; the chat's
// conversation keeps it, as it keeps a turn's final response.
func TestCronCommandOutputIsMirroredToTheChat(t *testing.T) {
	var executor mirroringJobExecutor

	origin := runCommandJob(t, &executor, "echo convite-expira-hoje")

	if len(executor.mirrored) != 1 {
		t.Fatalf("mirrored %d deliveries, want 1", len(executor.mirrored))
	}
	got := executor.mirrored[0]
	if got.origin != origin || got.channel != "telegram" || got.chatID != "123" {
		t.Fatalf("mirrored = %+v, want origin %s on telegram/123", got, origin)
	}
	if !strings.HasPrefix(got.text, "Scheduled command 'echo convite-expira-hoje' executed:") ||
		!strings.Contains(got.text, "convite-expira-hoje") {
		t.Fatalf("mirrored text = %q, want the published output", got.text)
	}
}

func TestCronCommandMirrorIsBounded(t *testing.T) {
	var executor mirroringJobExecutor

	runCommandJob(t, &executor, "head -c 9000 /dev/zero | tr '\\0' 'x'")

	if len(executor.mirrored) != 1 {
		t.Fatalf("mirrored %d deliveries, want 1", len(executor.mirrored))
	}
	got := executor.mirrored[0].text
	if !strings.HasSuffix(got, "[output truncated in this conversation]") ||
		len([]rune(got)) > maxMirroredCommandOutput+len("\n[output truncated in this conversation]") {
		t.Fatalf(
			"mirrored %d runes ending %q, want the output cut at %d",
			len([]rune(got)),
			got[len(got)-50:],
			maxMirroredCommandOutput,
		)
	}
}

func TestCronFailedCommandIsNotMirrored(t *testing.T) {
	var executor mirroringJobExecutor

	runCommandJob(t, &executor, "exit 3")

	if len(executor.mirrored) != 0 {
		t.Fatalf("mirrored %+v, want the error notice kept out of the conversation", executor.mirrored)
	}
}

// What the command does on its own (a script posting to the chat's API) is
// traced to the run through the session key its exec call carries.
func TestCronCommandRunsWithTheRunSession(t *testing.T) {
	var seen []string
	SetExecEnvProvider(func(ctx context.Context) []string {
		seen = append(seen, ToolSessionKey(ctx))
		return nil
	})
	t.Cleanup(func() { SetExecEnvProvider(nil) })

	origin := runCommandJob(t, &mirroringJobExecutor{}, "true")

	if len(seen) != 1 || seen[0] != origin {
		t.Fatalf("exec saw sessions %v, want [%s]", seen, origin)
	}
}

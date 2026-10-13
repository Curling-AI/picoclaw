package tools

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
	"github.com/sipeed/picoclaw/pkg/turnresult"
)

type typedJobExecutor struct {
	stubJobExecutor
	status  turnresult.Status
	request turnresult.Request
}

func (s *typedJobExecutor) ProcessDirectWithResult(
	_ context.Context,
	request turnresult.Request,
) (turnresult.Result, error) {
	s.request = request
	return turnresult.Result{Status: s.status, Content: s.response}, s.err
}

func TestCronTypedResultRetainsRunIdentityAndOutcome(t *testing.T) {
	for _, status := range []turnresult.Status{turnresult.Completed, turnresult.Failed, turnresult.Canceled} {
		t.Run(string(status), func(t *testing.T) {
			var executor typedJobExecutor
			executor.status = status
			tool := newTestCronToolWithExecutorAndConfig(t, &executor, config.DefaultConfig())
			var job cron.CronJob
			job.ID = "opaque-job"
			job.Payload.Message = "run"
			job.Payload.Model = "fixed"
			run := cron.Execution{Progress: nil, RunID: uuid.NewString(), Trigger: cron.Manual, Job: &job}
			got, err := tool.ExecuteJobWithResult(context.Background(), run)
			if err != nil || got.Status != status {
				t.Fatal("typed outcome lost", got, err)
			}
			if executor.request.SessionKey != "agent:cronmodel-opaque-job-"+run.RunID {
				t.Fatal("executor generated another identity", executor.request)
			}
		})
	}
}

func TestCronDisabledCommandFailsButLegacyResponseIsPreserved(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Exec.Enabled = false
	tool := newTestCronToolWithExecutorAndConfig(t, nil, cfg)
	var job cron.CronJob
	job.ID = "command"
	job.Payload.Command = "echo hi"
	result, err := tool.ExecuteJobWithResult(
		context.Background(),
		cron.Execution{Progress: nil, RunID: uuid.NewString(), Trigger: cron.Scheduled, Job: &job},
	)
	if err == nil || result.Status != turnresult.Failed {
		t.Fatal("disabled command succeeded", result, err)
	}
	if tool.ExecuteJob(context.Background(), &job) != "ok" {
		t.Fatal("legacy command return changed")
	}
}

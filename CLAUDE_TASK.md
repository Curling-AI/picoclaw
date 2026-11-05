# Task: design + implement a per-job "silent" flag for cron jobs in picoclaw (seucaranguejo fork)

You are working in /home/ethos/.assistant/workspace/work/picoclaw-analysis/fork — the Curling-AI/picoclaw vendored fork (Go, HEAD 06acb08, branch feat/silent-cron-jobs already checked out). Do NOT push or commit to remotes; work locally on this branch.

## Problem
Cron jobs that have nothing to report still send a message to the user's chat. Root cause analysis (verified line-by-line):

1. The system note prepended to every scheduled run (pkg/tools/cron.go ~line 812-831) tells the model to "run silently when there is nothing new" — the model OBEYS by narrating its compliance ("Silent turn — no text.", "(empty)"), and that narration becomes finalContent.
2. pkg/agent/turn_coord.go:294-296 and :363-366: an empty finalContent is replaced by ts.opts.DefaultResponse ("The model returned an empty response. This may indicate a provider error or token limit."). Fork improvement: finalIsFallback keeps it out of session history, but it is STILL shown to the user.
3. pkg/tools/cron.go ExecuteJobWithResult (~line 845-848): `if result.Content != "" { t.executor.PublishResponseIfNeeded(...) }` — publishes whatever survives.
4. Cron turns also publish tool feedback to the chat (shouldPublishToolFeedback, agent_utils.go:220; SuppressToolFeedback exists in processOptions but cron turns don't set it).

## Goal
A per-job flag so a cron job can end its turn with NO message delivered to the user, while still allowing the agent to deliver explicitly via the message tool when it DOES have something.

## Prior analysis suggested (you may improve on this — use your judgment)
1. CronPayload.Silent bool in pkg/cron/service.go (json omitempty).
2. ExecuteJobWithResult: skip publish when job.Payload.Silent.
3. Propagate Silent through turnresult.Request -> processMessageResult: set DefaultResponse: "" and SuppressToolFeedback: true for silent jobs (with DefaultResponse "" the empty finalContent stays empty, so publish is naturally skipped).
4. When Silent, adjust the system note: "This run is SILENT: end the turn with an empty response; nothing is delivered unless you call the message tool."
5. Tests mirroring TestCronTool_ExecuteJobPublishesAgentResponse (silent publishes nothing; message-tool sends still work).

## Constraints
- Follow the fork's existing code style (fork-specific comments are in Portuguese; match surrounding style).
- Do not change behavior of non-silent jobs.
- The turnresult.Result has Status/Incomplete/finalIsFallback machinery — consider using it instead of inventing new plumbing.
- Keep the change minimal and reviewable. Run the relevant tests (go test ./pkg/tools/... ./pkg/cron/... ./pkg/agent/... — note: go.mod may have a replace directive pointing to ../plz-out; if the build fails on that, fix go.mod temporarily for the test run and revert it after).
- Deliverables: (a) the implementation + tests on the branch, (b) a short summary of the design decisions you made and why, written to /home/ethos/.assistant/workspace/work/picoclaw-analysis/claude-summary.md, (c) test results in the same summary.

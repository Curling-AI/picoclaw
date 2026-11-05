# Silent cron jobs — design summary

> Should live at `../claude-summary.md`, but this session can only write inside `fork/`. Move it
> there (`mv claude-summary.md ..`); it's untracked and shouldn't be committed.

Branch `feat/silent-cron-jobs` (fork at 06acb08). Changes are in the working tree, **not committed**.
8 files changed (+94/−17), plus 2 new test files.

## ⚠️ Test status: NOT RUN

Every Go command (`go build`, `go test`, `gofmt`, even `go version`) was blocked by the session's
permission system: each needed interactive approval and none was granted. So nothing below has been
compiled or tested. I checked by hand that every symbol, helper and signature used exists
(`typedJobExecutor`, `mirroringJobExecutor`, `messageToolProvider`, `alwaysEmptyProvider`,
`WithToolContext`, `parseCronJobResult`, `OutboundChan`, `exec.toolRanThisTurn`, …), and also checked
struct alignment for gofmt. That is review, not verification. Run this before trusting it:

    go test ./pkg/tools/... ./pkg/cron/... ./pkg/agent/... ./pkg/turnresult/...
    gofmt -l pkg/

`go.mod` was not touched (its only `replace` is the discordgo fork; there's no `../plz-out` replace).

## What the flag does

`CronPayload.Silent bool` (`json:"silent,omitempty"`). In a silent job's run, nothing reaches the chat
on its own. The only way the run talks to the user is the `message` tool.

## Design decisions

1. **The prior plan alone would not silence web chats.** Cron turns run with
   `AllowInterimPicoPublish: true`. On `pico`/`grpc` the model's final text is streamed (or
   interim-published in `Finalize`) *during* the turn, before `ExecuteJobWithResult` ever decides
   whether to publish. Skipping the publish there only silences Telegram/WhatsApp/etc. Silent turns
   therefore also set `AllowInterimPicoPublish = false`, which turns off streaming, interim narration,
   tool-result frames and the Finalize interim publish. Pico/grpc reasoning ("thought" bubbles) is
   published *before* that gate (`pipeline_llm.go`, `structuredInterimChannel`), so silent turns are
   excluded from it explicitly. The operator's separate reasoning channel (`handleReasoning`) is left
   alone because it isn't the user's chat.

2. **Two layers of protection.** The agent side (`processMessageResult`) gets `Silent`,
   `DefaultResponse = ""`, `SuppressToolFeedback = true`, `AllowInterimPicoPublish = false`. On top of
   that, `ExecuteJobWithResult` never publishes `result.Content` for a silent job. The model may still
   narrate its silence ("(vazio)", "Sem novidades"), and an executor without `ProcessDirectWithResult`
   doesn't see `Request.Silent`; the cron-side gate covers both cases.

3. **Plumbing reuses what exists.** `turnresult.Request.Silent` → `processMessageResult(…, silent)` →
   `processOptions.Silent`. No new channel or event machinery.

4. **An empty ending is success, not failure.** Today an empty final sets `finalIsFallback`, so
   `incomplete` is true and the run reports `Status: Failed`. For a silent run, empty is the requested
   outcome. So in the `ControlBreak` branch of `turn_coord.go`, a silent turn skips the fallback and
   ends `Completed` with `Content: ""`. Turns that hit max iterations or have no final at all still go
   through the existing synthesized path and stay `Failed`. `Finalize` doesn't write an empty assistant
   message to the (per-run) session.

5. **Empty-reply retry and reasoning salvage.** Normally an empty reply is retried once, and then the
   model's reasoning is "salvaged" as the reply. In a silent turn:
   - The reasoning salvage is off, so private reasoning never becomes the reply.
   - The retry is skipped **only after a tool has run** (`exec.toolRanThisTurn`). The cron note makes
     the first action `bash scripts/<id>/run.sh`, so a legitimate silent ending always follows a tool.
     An empty reply before any tool is more likely the reasoning-only glitch, and one retry is cheap.

6. **Prompt.** The non-silent system note is byte-identical to before (there's a test for that).
   Silent jobs get one more bullet stated as a *delivery rule*: "your final reply is NOT delivered…
   send it with the message tool… otherwise end with an empty reply, do not say there is nothing new".
   The old wording ("run silently") is what made the model narrate its compliance.

7. **Command jobs.** A silent command job skips publishing (and mirroring) its output on success. On
   **failure** the error notice is still published, because a broken job is news and would otherwise
   fail unnoticed. This is a judgment call; if you want total silence, drop `|| result.IsError` in
   `cron.go`.

8. **Agent-facing tool.** New `silent` boolean in the cron tool schema. `add` sets it; `update`
   toggles it (and rejects non-booleans); if omitted, the current value is kept. The platform or
   control plane can also set it straight in `jobs.json`.

## Not changed / known limits

- `ContinueDirect` (continuing a session that still has queued messages) builds its own options and
  doesn't carry `Silent`. It only applies when messages get queued into a cron run's own per-run
  session, which is an edge case.
- In silent turns, guards that drop bad text (truncated tool-call markup, an announcement with no
  call, degenerate output) now end as `Completed`/empty rather than `Failed`. Nothing is shown in
  either case; only the reported status differs.
- Non-silent jobs: no behavior change on any path (every new branch is gated on `Silent`).

## Tests added (not run — see above)

`pkg/tools/cron_silent_test.go`
- `TestCronTool_SilentJobPublishesNothing`: executor gets `Request.Silent`, prompt has the delivery
  rule, and the narrated "(vazio)" is not published.
- `TestCronTool_NonSilentJobPromptAndPublishUnchanged`: regular note unchanged, reply published.
- `TestCronSilentCommandJobPublishesOnlyFailures`: `echo` → nothing published or mirrored; `exit 3`
  → error notice published.
- `TestCronTool_AddAndUpdateSilent`: add with `silent`, update to false, non-boolean rejected.

`pkg/agent/silent_turn_test.go`
- `TestSilentTurnEndsEmptyWithoutFallback`: empty → `Completed`, `""`, nothing on the bus (one
  retry, since no tool ran).
- `TestSilentTurnStillDeliversThroughMessageTool`: message-tool send is the only outbound, 2 LLM
  calls (no retry after the tool), `Completed`/`""`.
- `TestSilentTurnPublishesNothingOnPico`: text plus reasoning on `pico` → nothing on the bus, text
  returned only to the caller.

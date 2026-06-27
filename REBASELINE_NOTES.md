# Re-baseline onto upstream sipeed/picoclaw

Working branch: `rebaseline/upstream-sync` (based on upstream/main `52320f48`).
Goal: fork = upstream HEAD + preserved Curling-AI customizations, validated with evidence.

## Baseline facts (verified)
- Upstream is ~2091 commits / ~4 months ahead of the fork's old base (`aaf42b2`).
- Upstream restructured `pkg/agent` (monolithic `loop.go` → pipeline: `pipeline_*.go`, `agent_*.go`), `pkg/channels/telegram` (→ subdir + `pkg/commands`), `pkg/tools` (→ `pkg/tools/fs`), `pkg/session`, and added `pkg/fileutil.WriteFileAtomic`.
- **Matrix/CGO is NOT a blocker for the product.** Upstream added `pkg/channels/matrix` (needs `mautrix/crypto/libolm`, CGO + `olm/olm.h`). It is wired only via `pkg/gateway/channel_matrix.go` (side-effect import), which the **product does not use** (the product has its own gateway and imports `pkg/channels` directly; `manager.go` references "matrix" only as a string). The product's package subset builds green with `CGO_ENABLED=0`. Do NOT `go build ./...` (that pulls in pkg/gateway → matrix → libolm); build the product subset instead.

## The product imports only:
`pkg/{agent,bus,channels,config,cron,heartbeat,logger,providers,routing,session,tools}` — all with `CGO_ENABLED=0`.

## Verdict catalog (evidence-based)

### 🔴 PRODUCT-CRITICAL — re-port or product breaks
| Commit | What | Product dependency | Upstream | Difficulty |
|---|---|---|---|---|
| 1c2935be | session RecordStore/SessionRecord/TranscriptFile, lifecycle, pruning, compaction | server.go:244,253,317,441 | absent | hard |
| 8196e467 | `state_dir` / `StateDirPath()` | main.go:73,101,129,350; server.go:327; k8s/config.go:45 | absent (only unrelated EvolutionConfig.StateDir) | hard |
| dd7d1b20 + 38916498 | `loop_detection` + `messages.suppress_tool_errors` config + logic | gateway_ops.go:690-710; k8s/config.go:129-146; i18n UI | absent | hard |
| 0c650e5c | Gemini `Name` on tool-result messages | Gemini via openai_compat rejects empty function_response.name | absent (SerializeMessages has no name) | moderate |
| 5a158dc6 + 1a91d603 | Ollama/qwen tool-calls embedded in text | Ollama via openai_compat | absent in openai_compat | moderate |
| c5b80cdd + 24467e7e | S3 Mountpoint-safe writes (no temp+rename; 0644) | prod runs S3 Mountpoint; upstream WriteFileAtomic breaks it | absent (upstream does the opposite) | moderate |
| 1c228bd8 (split) | skill loader follows symlinks + name fallback; summarization config | Dockerfile installs skills by symlink; ListSkills RPC; k8s/config.go:55-57 | absent | moderate |
| aaf42b2d (core) | subagent survives turn (`context.WithoutCancel` + SessionWriteCallback) | server.go:98-100,608-663; product commit 90f4adc | absent | moderate |
| 2d880c71 | `NewAgentLoopWithRegistry` (shared registry) | main.go:89-90,164 | ✅ RESOLVED — upstream `agent_inject.go:26 GetRegistry()` already covers it. NO fork change; product-side: use `NewAgentLoop(...)` + `agentLoop.GetRegistry()` at re-import | done |
| d03b1c98 + 971ac51c + 282bd4e9 | empty-response → tool summary | server.go:545 returns string as final ChatEvent | absent (buildToolSummary fork-only) | moderate-hard |

### 🟢 MIGRATE-TO-UPSTREAM — upstream already does it; validate & drop fork version
- 1cedb667 context_window agent-default (upstream config.go:433) — product uses it directly; validate default.
- 455b05e1 ProviderRegistry → upstream instance.go per-candidate provider config.
- 3526e655 per-request timeout → upstream WithRequestTimeout.
- 54fab2e0 per-model cooldown → upstream StableKey/ModelKey.
- fa4f46f3 user-stop → upstream agent_stop.go (`/stop`); add free-text intent shim if needed.
- ec104c89 exec-guard path regex → upstream token-boundary/domain heuristics (superset).
- 4794367a sandbox home-dir → upstream AllowReadOutsideWorkspace config (hard: redesign-to-config).

### 🟡 RE-PORT simple / RECONCILE
- a3d95a63 (remove rm -rf deny) + e3e8d03a (remove sudo deny) — trivial line deletes; allowlist-bypass already upstream.
- 8cb6a41e + d96a1048 + d0d1d81d — retry/backoff + cooldown-skip; collides with upstream RateLimiterRegistry (hard). Keep the standalone `ctx.Err()!=nil` deadline fix + `isContextError` narrowing.
- d22ea9ce Telegram allowlist — mostly obsolete (upstream IsAllowedSender gates; product has own block_strangers); only the reject-reply UX is fork-unique (optional).
- 6b0f535e dynamic bot name — re-port (upstream hardcodes "PicoClaw").
- e94a79a1 — take upstream's multi-tool fix; re-port only Name-backfill (needs 0c650e5c).

### ⚪ OBSOLETE for product / low value
- c34a21d9 Dockerfile symlinks — product has its own docker/picoclaw-gateway/Dockerfile.
- cc8cbae9 MaxToolIterations 200 — product overrides to 20; accept upstream's 50.
- b91d44e3 per-model context_window — fork-only, unused by product.
- 0be46750 + antigravity half of 0c650e5c — product uses openai_compat, not antigravity OAuth.

### 🦞 TUI (657d06cd + TUI half of aaf42b2d)
Product doesn't use it, but kept in the fork. Needs re-wiring onto upstream's event system (fork events.go collides with upstream events.go). Lowest priority.

## Hardest conflict points
1. Session layer (1c2935be) — product hard-wired to fork session API; upstream pkg/session diverged.
2. state_dir (8196e467) — spans config+cmd; no upstream equivalent.
3. guardrails/loop_detection — product config/UI contract; re-wire onto pipeline.
4. S3 Mountpoint vs upstream `fileutil.WriteFileAtomic` (temp+rename+0600).

## Progress (branch rebaseline/upstream-sync)
DONE (each builds + tests green, CGO off):
- ✅ 2d880c71 constructor — resolved by upstream GetRegistry(); no fork change (product-side at re-import).
- ✅ a8e4f5db — 0c650e5c Gemini tool-result `name` via SerializeMessages.
- ✅ ee8521e6 — 5a158dc6+1a91d603 Ollama/qwen text tool-call extraction.
- ✅ edc655a7 — c5b80cdd+24467e7e S3 Mountpoint write fallback in fileutil.WriteFileAtomic.
- ✅ 6292d594 — 1c228bd8 (loader part) skill loader symlinks + name fallback.

TODO product-critical (hard): 8196e467 state_dir; 1c2935be session RecordStore/TranscriptFile;
dd7d1b20+38916498 guardrails/loop_detection; aaf42b2d (core) subagent survival;
d03b1c98+971ac51c+282bd4e9 empty-response tool summary; 1c228bd8 (summarization config part).
TODO migrate-to-upstream (validate & adopt): 1cedb667, 455b05e1, 3526e655, 54fab2e0, fa4f46f3, ec104c89, 4794367a.
TODO re-port simple: a3d95a63, e3e8d03a (deny removals); 6b0f535e (dynamic bot name).
TODO last/optional: TUI (657d06cd + aaf42b2d TUI half).

## Execution order
1. ✅ Branch from upstream HEAD; green baseline (product subset, CGO off).
2. Port PRODUCT-CRITICAL (validate each against the product).
3. MIGRATE-TO-UPSTREAM (validate equivalence, drop fork version).
4. RE-PORT simple + RECONCILE.
5. TUI last.
6. Build + picoclaw tests → re-import into seucaranguejo → make test → deploy → e2e.

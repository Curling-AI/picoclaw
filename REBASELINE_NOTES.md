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

### 🟢 MIGRATE-TO-UPSTREAM — VALIDATED (Explore evidence pass). Drop fork versions; no fork change.
All confirmed present in upstream with file:line evidence:
- ✅ 1cedb667 context_window — EQUIVALENT. config.go:434 field → instance.go:34/~168/283 wired →
  consumed by context budget + summarization. Product's context_window key flows through.
- ✅ 455b05e1 ProviderRegistry — SUPERSET. model_list has per-entry provider/api_base/api_key/
  request_timeout/rpm (config.go:772-810); instance.go candidateProviders map +
  populateCandidateProvidersFromNames resolve a distinct provider per fallback candidate.
- ✅ 3526e655 per-request timeout — EQUIVALENT. ModelConfig.RequestTimeout (config.go:791) →
  factory_provider.go NewHTTPProviderWith...RequestTimeout / bedrock.WithRequestTimeout.
- ✅ 54fab2e0 per-model cooldown — SUPERSET. providers/cooldown.go CooldownTracker +
  ratelimiter.go RateLimiterRegistry + ModelConfig.RPM → FallbackCandidate.RPM.
- ✅ ec104c89 exec-guard path heuristics — SUPERSET. shell.go has 40+ deny patterns, path-traversal
  regex (~1169), URL-scheme exemptions, isShellTokenBoundary/looksLikeDomain/localPathExists
  (~1228-1238), EvalSymlinks. Far beyond the fork's regexes.
- ✅ 4794367a read-outside-workspace — EQUIVALENT. AgentDefaults.AllowReadOutsideWorkspace
  (config.go:429) enforced at instance.go:87 + tools/fs/filesystem.go validatePathWithAllowPaths.
- ⚠️ fa4f46f3 user-stop — PARTIAL. /stop slash command EQUIVALENT (commands/cmd_stop.go +
  agent/agent_stop.go StopActiveTurn). GAP: no FREE-TEXT stop-intent detection (upstream only
  matches the slash command). The fork's free-text patterns are ENGLISH-only ("stop"/"cancel"/
  "abort") — low value for a PT-BR product (users type "pare"/"cancela"). DECISION: do NOT port the
  English patterns; /stop covers explicit stop. If desired later, add PT-BR free-text intent as a
  product-side enhancement. Not product-critical.

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

## Preserved history capture overrides (MAPP-289)

### Content lineage and checked mutations (MAPP-249)

These are fork overrides of six upstream files, not changes to accept from
upstream during the next rebaseline:

| Upstream file | Behavior to preserve |
| --- | --- |
| `pkg/agent/context_legacy.go` | Read the explicitly activated content view before summary work. Pending compaction or a typed mutation blocks dependent summarization **before** any provider call; qualified compaction uses the exact observed state and refs. Never infer edit direction from a current getter. |
| `pkg/memory/jsonl.go` | Retain `SessionMeta.ContentIdentity`, source-owned occurrence/revision refs, the guards on `SetSummary`/`TruncateHistory`, and independent order recovery. Active `TruncateHistory` validates the full frame at the cut, even without an intervening append. `Compact` remains strict when proof is unavailable. `CompactForSessionSave` only skips optional physical GC for pure proof loss; both preserve all bytes while Pending awaits ACK, even after its deadline. Real I/O, ownership and unresolved journal errors remain explicit. An opted-in metadata checkpoint requires file and directory sync rather than relying on the shared utility's direct-write fallback. |
| `pkg/agent/event_payloads.go` | Keep the independent, detached content and content-commit observer payloads, including each append's exact domain/before/after interval. Frozen mutation frames carry no receipt binding; missing binding is not itself an order-recovery decision. |
| `pkg/agent/agent.go` | Preserve the public observer registration methods (`SetSessionHistoryContentObserver` and `SetSessionHistoryContentCommitObserver`) and the separate receipt observer. Capture exact commits before callbacks/ingestion can enqueue other writes; an old ref or application ACK never becomes read proof. |
| `pkg/session/jsonl_backend.go` | `Save` prefers `CompactForSessionSave` while leaving the fallback capability optional. `DeleteSession` returns the source owner's complete deletion result and cannot fall back to partial `SetHistory`/`SetSummary` clear. Keep the optional lineage/mutation delegation methods alongside this backend; interface presence never initializes a legacy source. |
| `web/backend/api/session.go` | Owner HTTP DELETE reuses `memory.DeleteJSONLSession` for the complete source lifecycle, including exact owned preimages, journals, witnesses and cache retirement. A refusal returns an error before clearing the summary/body; an old fixed unlink list would leak mutation payloads. |

Also preserve the fork helpers used by those overrides: checked `edit` and
`retain_prefix` keep a source-owned frozen Before/After pair and stable command
ID/intent/plan digest; source NEW retries replay that operation rather than
cutting a second time. Application ACK is distinct from archive, coverage and
user-read ACKs. Last is bounded and closed; its absence is not evidence that an
old command never executed. The external current checkpoint binds logical
summary/Pending/ACK changes even when transcript bytes stay equal. Existing V7
writers do not understand this semantic baseline and are not a supported
rollback target.

In `pkg/agent/delivery_mirror.go`, keep the confirmed-input outcome separate
from proof and recovery: `ErrHistoryInputCommitted` exposes the operational cause
without inviting receipt fallback or a second append. An accepted unproved body
is retained/notified once; no new receipt is invented. Normal turn appends log
their source errors rather than reconstructing order. Owner deletion of a
never-initialized legacy source may use its exact own Key, absent content
witness and settled journal as a negative activation probe; strict append,
initialization and active-source ownership still validate aliases and reject
ambiguous or unreadable ownership metadata. Never turn that negative probe into
a general directory-scan bypass.

Physical content-witness presence uses `Lstat`: a dangling symlink remains
activation evidence and cannot silently select the never-initialized path. That
presence grants no known identity/order proof; strict reads still reject unsafe
artifacts. A final journal-unlink failure after the durable NEW decision and all
required syncs is narrowly classified as cleanup: retain/log the fence and keep
the already qualified receipt. Do not extend that disposition to failed syncs,
uncertain decisions or joined operational causes.

Preserve private disk format 2's small metadata/cache/checkpoint headers,
appendable record-chain locator index and immutable Pending/Last blobs. The
ordinary accepted-input capability withholds content proof and extends only new
index bytes; qualified readers and destructive operations still verify the full
physical prefix. Metadata, append journals and the warm cache must not expand
those vectors again. Pure getters do not migrate. Explicit inline downconversion
targets semantic writer `dfd1c2d`, preserving operation/ACK/ref/order evidence;
older V7 binaries remain unsupported. Keep the optional input observer's early
durable wake-up separate from qualified observers and genuine receipt delivery.

Deletion's bounded variant of the existing history journal retains exact typed
artifact roots, file fingerprints and expected absences until every payload and
source unlink is durable. The authorization journal retires last. Warm cron and
empty sweeps discover that prior intent even after metadata disappeared; they do
not reuse a normal append journal as authorization or repeat an already passed
empty hook. Preserve unsafe/unowned evidence and reject replacement inodes or
newly appeared files. The canonical metadata retains its alias list until every
owned legacy physical alias finishes deletion, including a retry with only its
own witness left. This does not make unresolved public aliases physical owners
or create shared directory locks/cache absent from this fork's current source.

The producer's `messageutil.ProjectLegacyPublic` predicate is still a separate
implementation from the currently published consumer's copy. The consumer pin,
error propagation and projection alignment are a separately validated follow-up;
an unpublished composition is not delivery evidence. Strict content readers can
still scan complete prefixes, including asynchronous consumer work. Measure all
data-API reads/writes and observer drain separately from the accepted append's
latency; local figures do not establish EFS or end-to-end constant-time behavior.

### Earlier capture overrides

- `pkg/memory/jsonl.go`, `history_*.go`, `migration.go`, and `prune.go`: private occurrence/run IDs, generation lineage, bounded cursor pages, and the committed `HistoryBytes` boundary. Append journals retain OLD metadata/offset; rewrites also retain the previous raw JSONL because `WriteFileAtomic` can partially fail through its direct-write fallback. Recovery records an OLD/NEW decision before rollback; unknown metadata and cleanup failures fence only the affected key. Startup, metadata caching, alias promotion, listing and pruning must preserve that isolation and use the same sanitized-key lock. Deletion removes the journal, decision, recovery marker and `.jsonl.order-witness` and evicts owned caches; this is not a cross-process deletion transaction.
- `pkg/providers/protocoltypes/types.go`, `pkg/session/jsonl_backend.go`, and agent history-write call sites: preserve private history provenance through internal reads/writes without adding it to the public provider DTO. Epoch and byte-count fields remain part of the existing capture consumer contract.
- `pkg/agent/context_legacy.go`: forced compression uses `TruncateHistory` only at a safe turn boundary. With a single protected turn and no safe boundary it returns without mutating persisted history; prompt fitting can trim model context in memory. Preserve this override when reconciling upstream compression changes.
- MAPP-249 producer order: preserve `AppendHistoryReceipt`/`ReadHistoryWithReceipts`, the exact synchronous receipt observer, persistent stream/highwater and legacy-origin evidence, and strict receipt validation. Recovery-aware content mutations retain the fence: unchanged message cores keep their genuine order, changed cores receive a genuinely allocated new source sequence only while proof is healthy, and proof loss never assigns or reconstructs order. Detect missing continuity before truncation or clear can hide it. Keep provider content reads tolerant of private proof metadata without inventing order. See [the private history-order contract](docs/architecture/history-order.md), including explicit recovery and startup/reconnect reconciliation when persistence has no live receipt.
- `web/backend/api/session.go` and the English/Chinese session architecture docs describe the same transcript, metadata, journal and decision lifecycle. Preserve the published `SetDeliveryConversationKept` API and main ancestry when updating this fork's consumer pin.
- An unowned complete tail must survive append and its rollback. `HistoryRecoveryRequired` and the durable `.jsonl.recovery-required` marker keep pages/cursors fenced without stopping ordinary content lifecycle: `SetHistory`, `TruncateHistory`, `Compact`/`Save`, explicit clear and alias promotion retain the marker. If either side of alias promotion requires recovery, both sources retain the latch before content is copied; the original transcript is preserved. Automatic pruning remains blocked. An explicit owner-requested launcher DELETE resolves its own pending journal before removing the source, metadata and recovery files; the owning writer retires its deleted caches on the next mutation. A deliberate truncation/clear is a content operation, not an implicit recovery or order attestation; only an own OLD/NEW journal authorizes rollback of complete rows.
- Upstream touchpoints include `readMeta`, `writeMeta`, the content mutators and alias promotion. Preserve the fork's warm metadata cache (`e84a00f7`): latch state is write-through, with filesystem checks on cold metadata reads and proof/deletion boundaries. Cold full `readMeta` and proof boundaries also validate `.jsonl.order-witness`; identity-only Key/Aliases selection does not inspect a non-owner's witness or cache partial metadata. A transient witness READ error remains explicit and retryable; confirmed proof loss and a failed first witness WRITE/SYNC retain the durable recovery fence. The owner-requested launcher DELETE removes the witness last, and the next metadata upsert/append retires the corresponding warm witness cache only after confirming both transcript and metadata are absent. A surviving witness or recovery marker still fences proof. Ordinary warm reads and listing keep cache hits; metadata upsert checks source presence before reusing a warm checkpoint so confirmed paired deletion cannot republish retired offsets. This write-boundary I/O preserves the ambiguous-source guard rather than inferring paired deletion from append's exclusive create. The upstream `TestTruncateHistory_StaleMetaCount` still reconciles the physical row count before retaining the requested suffix; do not invert that correction to block agent turns. Keep the marker across rewrites and rollback; legacy binaries do not enforce it while running, and deployment must prevent overlapping directory writers.

## Progress (branch rebaseline/upstream-sync)
DONE (each builds + tests green, CGO off):
- ✅ 2d880c71 constructor — resolved by upstream GetRegistry(); no fork change (product-side at re-import).
- ✅ a8e4f5db — 0c650e5c Gemini tool-result `name` via SerializeMessages.
- ✅ ee8521e6 — 5a158dc6+1a91d603 Ollama/qwen text tool-call extraction.
- ✅ edc655a7 — c5b80cdd+24467e7e S3 Mountpoint write fallback in fileutil.WriteFileAtomic.
- ✅ 6292d594 — 1c228bd8 (loader part) skill loader symlinks + name fallback.

- ✅ aeb852ad — 8196e467 state_dir (AgentDefaults.StateDir + Config.StateDirPath()).
- ✅ 6c339ede — 1c2935be session: thin ListSessionRecords()+DeleteSession() over upstream's
  Session (which already has Key/Created/Updated); NO heavy transcript/lifecycle re-port needed.
  KEY FINDING: upstream's session model carries the metadata the product needs.

- ✅ a837f31c — dd7d1b20+38916498 guardrails CONFIG: LoopDetectionConfig (+Validate) /
  LoopDetectorsConfig (5 toggles) / MessagesConfig / AgentDefaults.TimeoutSeconds+VerboseDefault.
  Upstream has no DisallowUnknownFields, so the product's loop_detection/messages keys were being
  silently dropped — structs added so they deserialize. LoadConfig validates thresholds.
- ✅ b25eaf07 — dd7d1b20+38916498 guardrails WIRING: LoopDetector (5 detectors, Warning→Block→Abort)
  on turnState, Record() at ExecuteTools per-tool-call site. Block=inject nudge into working
  messages; Abort=publish buildLoopAbortSummary (reads ts.toolExecutions []ToolExecutionRecord) +
  allResponsesHandled+ToolControlBreak. suppress_tool_errors hides non-mutating-tool errors at the
  ForUser publish path. Full pkg/agent + pkg/config suites green.
  DEFERRED (low-risk, documented): wall-clock run-timeout ENFORCEMENT (TimeoutSeconds/VerboseDefault)
  NOT re-wired into the new turn coordinator. Evidence it's safe to defer: the product does NOT
  serialize timeout_seconds/verbose_default (gateway_ops.go writes only loop_detection + messages),
  and upstream MaxIterations already bounds runaway loops. Config fields kept for schema fidelity.
  The fork's buildTimeoutSummary/buildStopSummary/isStopMessage/buildFallbackErrorReply were NOT
  ported here (timeout summary belongs to the deferred enforcement; stop-intent belongs to the
  separate fa4f46f3 user-stop MIGRATE item — upstream already has agent_stop.go).

TODO product-critical (hard) — REMAINING, with evidence gathered this session:

1. ✅ 403ef204 — 1c228bd8 summarization config DONE. Added max_history_messages /
   summarization_threshold_percent / keep_last_messages to AgentDefaults (exact product json
   tags); NewAgentInstance resolves them over the legacy summarize_* fields (legacy honored only
   when new key unset). max_history_messages==0 disables the message-count trigger (token threshold
   at 90% still guards). Wired into legacy context manager: maybeSummarize skips the count trigger
   when 0; summarizeSession keeps configurable KeepLastMessages (was hardcoded 4). Updated the
   upstream default-threshold test for the new contract. pkg/config + pkg/agent green.
   (original gap analysis kept below for reference)

   1c228bd8 summarization config — CONFIRMED REAL CONTRACT GAP (now resolved above).
   Product serializes agents.defaults.{max_history_messages, summarization_threshold_percent,
   keep_last_messages} (gateway_ops.go:636-638, 839-846; k8s/config.go:55-57). Upstream
   AgentDefaults instead has summarize_message_threshold (default 20) + summarize_token_percent
   (default 75) — DIFFERENT json keys, so the product's keys are silently dropped (no
   DisallowUnknownFields), same failure mode as loop_detection. NEEDS: add the 3 fork fields with
   exact json tags AND wire them into upstream's summarization path (ContextManager.Compact /
   wherever summarize_message_threshold + summarize_token_percent are consumed). RISK: this
   remaps EXISTING, working upstream summarization (different defaults) — must preserve upstream
   behavior when the new keys are unset (fall back to summarize_message_threshold/percent). Do with
   care + a test asserting both old and new keys resolve correctly. Moderate.

2. aaf42b2d (core) subagent survival — RE-SCOPED by evidence. The PRODUCT's survival fix (90f4adc)
   is ENTIRELY product-side (internal/picoclaw/server.go postLifecycleIdleTTL keeps the gRPC stream
   open 5min after lifecycle end; wsbridge.go; static/js/picoclaw.js) — it consumes whatever events
   picoclaw emits; it does NOT call picoclaw-side WithoutCancel/SessionWriteCallback APIs. Upstream
   picoclaw HAS the building blocks (subturn.go:336 context.WithTimeout(context.Background()) detaches
   child ctx; turnState.critical/parentEnded; KindAgentSubTurnOrphan event for late results) but per
   investigation is an incomplete WIP: parentEnded is never set on GRACEFUL parent exit (only on hard
   abort via Finish()), and detached results become orphan events rather than persisted. WHETHER the
   product needs the fork mechanism depends on event-flow behavior that is only verifiable END-TO-END
   (spawn sub-agent → end parent → does result reach the gateway stream as the product expects?).
   DECISION: defer to task 5 (re-import + e2e). Do NOT blind-port hundreds of lines of lifecycle code
   onto a different turn architecture without an e2e signal. High risk.

3. d03b1c98+971ac51c+282bd4e9 empty-response → tool summary — moderate-hard, touches the response
   path (server.go:545 returns string as final ChatEvent). Best validated with the product in the
   loop. Defer alongside (2) or do with fresh budget + careful response-path study.
NOTE: upstream session Save uses its own temp+rename (not fileutil.WriteFileAtomic), so sessions
on S3 still need either WriteFileAtomic routing or state_dir kept off-S3.
- ✅ MIGRATE-TO-UPSTREAM all VALIDATED (Explore evidence pass, see green section above):
  1cedb667/455b05e1/3526e655/54fab2e0/ec104c89/4794367a are EQUIVALENT or SUPERSET in upstream —
  no fork change needed. fa4f46f3 user-stop: /stop covered; free-text intent is an optional PT-BR
  product-side enhancement (English fork patterns not ported — low value for PT-BR).
- ✅ 1ef3d927 — a3d95a63+e3e8d03a exec-guard: removed rm -rf and sudo deny patterns
  (mkfs etc. still denied; allowlist-bypass already upstream).
- ✅ 48abf385 — 6b0f535e dynamic bot name: AgentDefaults.Name + /start welcome uses it (fallback
  "PicoClaw"). Telegram-specific GetMe() override dropped (handler now channel-agnostic).
NOTE: upstream session Save uses its own temp+rename (not fileutil.WriteFileAtomic), so sessions
on S3 still need either WriteFileAtomic routing or state_dir kept off-S3.

REMAINING (all deferred to task 5 / fresh budget — e2e-dependent or optional):
- aaf42b2d subagent survival (e2e), d03b1c98+ empty-response summary (e2e) — see items 2/3 above.
- TUI (657d06cd + aaf42b2d TUI half) — last/optional; product doesn't use it.
- fa4f46f3 free-text stop — optional PT-BR enhancement.

## Known PRE-EXISTING test failures on clean upstream HEAD (NOT regressions)
On macOS, these fail on the clean upstream baseline (temp dirs under /var/folders,
a symlink the path guard does not resolve) — verified by stashing local changes:
- pkg/tools TestShellTool_RelativePathWithSlashAllowed
- pkg/tools TestShellTool_DevNullAllowed
- pkg/tools TestShellTool_FileURISandboxing
When running task 4 (picoclaw test suite), exclude/expect these.

## Execution order
1. ✅ Branch from upstream HEAD; green baseline (product subset, CGO off).
2. Port PRODUCT-CRITICAL (validate each against the product).
3. MIGRATE-TO-UPSTREAM (validate equivalence, drop fork version).
4. RE-PORT simple + RECONCILE.
5. TUI last.
6. Build + picoclaw tests → re-import into seucaranguejo → make test → deploy → e2e.

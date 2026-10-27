# Session System

> Back to [README](../README.md)

This document describes the runtime session system used by PicoClaw to:

- map inbound messages onto stable conversation scopes
- persist message history and summaries
- preserve compatibility with legacy `agent:...` session keys while the runtime uses opaque canonical keys

This document covers the core runtime path in `pkg/session`, `pkg/memory`, and `pkg/agent`.
It does not describe launcher login cookies or dashboard authentication sessions in `web/backend/middleware`.

## Responsibilities

The session system has four jobs:

1. Decide which messages should share the same conversation context.
2. Persist that context durably across turns and restarts.
3. Expose a small `SessionStore` interface to the agent loop.
4. Keep older session-key formats working during storage and routing migrations.

## Main Components

| Layer | Files | Responsibility |
| --- | --- | --- |
| Session contract | `pkg/session/session_store.go` | Defines the `SessionStore` interface used by the agent loop. |
| Legacy backend | `pkg/session/manager.go` | Stores one JSON file per session. Still used as a fallback. |
| Session adapter | `pkg/session/jsonl_backend.go` | Adapts `pkg/memory.Store` to `SessionStore`, including alias and scope metadata support. |
| Durable storage | `pkg/memory/jsonl.go` | Append-only JSONL storage plus `.meta.json` sidecar metadata. |
| Scope and key building | `pkg/session/scope.go`, `pkg/session/key.go`, `pkg/session/allocator.go` | Builds structured scopes, opaque canonical keys, and legacy aliases from routing results. |
| Runtime integration | `pkg/agent/instance.go`, `pkg/agent/agent.go`, `pkg/agent/agent_message.go` | Initializes the store, allocates session scope, and persists metadata before turns run. |

## Session Data Model

The structured session identity is represented by `session.SessionScope`:

| Field | Meaning |
| --- | --- |
| `Version` | Schema version. Current value is `ScopeVersionV1`. |
| `AgentID` | Routed agent handling the turn. |
| `Channel` | Normalized inbound channel name. |
| `Account` | Normalized account or bot identifier. |
| `Dimensions` | Ordered list of active partition dimensions such as `chat` or `sender`. |
| `Values` | Concrete normalized values for each selected dimension. |

Only four dimensions are currently recognized by the allocator:

- `space`
- `chat`
- `topic`
- `sender`

The default config uses:

```json
{
  "session": {
    "dimensions": ["chat"]
  }
}
```

That means one shared conversation per chat unless a dispatch rule overrides it.

## Canonical Keys And Legacy Aliases

The runtime now prefers opaque canonical keys:

```text
sk_v1_<sha256>
```

These keys are built from a canonical scope signature in `pkg/session/key.go`.
The goal is to make storage keys stable while decoupling them from any specific legacy text format.

For compatibility, the allocator also emits legacy aliases such as:

```text
agent:main:direct:user123
agent:main:slack:channel:c001
agent:main:pico:direct:pico:session-123
```

These aliases matter because older sessions, tests, and some tools still refer to the legacy shape.
The JSONL backend resolves aliases back to the canonical key before reads and writes.

The agent loop also preserves explicit incoming session keys when the caller already supplied one of the recognized explicit formats:

- opaque canonical key
- legacy `agent:...` key

That behavior lives in `pkg/agent/agent_utils.go:resolveScopeKey`.

## Allocation Flow

The end-to-end flow for a normal inbound message is:

```text
InboundMessage
  -> RouteResolver.ResolveRoute(...)
  -> session.AllocateRouteSession(...)
  -> resolveScopeKey(...)
  -> ensureSessionMetadata(...)
  -> AgentLoop turn execution
  -> SessionStore read/write operations
```

More concretely:

1. `pkg/agent/agent_message.go` resolves the agent route from normalized inbound context.
2. `session.AllocateRouteSession` converts the route's `SessionPolicy` plus inbound context into a structured `SessionScope`.
3. The allocator builds:
   - `SessionKey`: canonical routed session key
   - `SessionAliases`: compatibility aliases for that routed scope
   - `MainSessionKey`: agent-level main session key
   - `MainAliases`: legacy alias for the main session
4. `runAgentLoop` persists scope metadata and aliases through `ensureSessionMetadata`.
5. During later reads or writes, `JSONLBackend.ResolveSessionKey` maps aliases back onto the canonical key.

The main session key is separate from routed chat sessions.
It is mainly used for agent-level or system-style flows that need one stable per-agent conversation, for example `processSystemMessage`.

## Scope Construction Rules

`pkg/session/allocator.go` builds scope values from normalized inbound context.
Important rules:

- `space` becomes `<space_type>:<space_id>`
- `chat` becomes `<chat_type>:<chat_id>`
- `topic` becomes `topic:<topic_id>`
- `sender` is canonicalized through `session.identity_links` before being stored

There are two special cases worth calling out.

### Telegram forum isolation

Telegram forum topics must stay isolated even when the configured dimensions only mention `chat`.
To preserve that behavior, the allocator appends `/<topic_id>` to the `chat` value for Telegram forum messages unless `topic` is already an explicit dimension.

Example:

```text
group:-1001234567890/42
group:-1001234567890/99
```

Those produce different session keys.

### Identity links

`session.identity_links` lets multiple sender identifiers collapse into one canonical identity.
Both dispatch matching and session allocation use that mapping so that the same person can keep one conversation even if their raw sender IDs differ across channels or accounts.

## Storage Format

The default runtime backend is `pkg/memory.JSONLStore`, wrapped by `session.JSONLBackend`.

Each session has a transcript, metadata, temporary write recovery files, and an optional persistent recovery latch:

```text
{sanitized_key}.jsonl
{sanitized_key}.meta.json
{sanitized_key}.jsonl.commit.json      # pending write journal
{sanitized_key}.jsonl.commit.decision  # recovery decision, when needed
{sanitized_key}.jsonl.recovery-required # persistent latch for an unproven source
```

The files store:

- `.jsonl`: one message per committed newline, with private occurrence/run IDs and generation metadata; ordinary writes append
- `.meta.json`: summary, timestamps, line counts, logical truncation offset, scope, aliases

`SessionMeta` currently includes:

- `Key`
- `Summary`
- `Skip`
- `Count`
- `CreatedAt`
- `UpdatedAt`
- `Scope`
- `Aliases`
- `HistoryGeneration`, `HistoryFileGeneration`, lineage, digest and tip fields
- `HistoryBytes`: the committed JSONL byte boundary
- `HistoryRecoveryRequired`: history remains readable and appendable, but cannot provide capture proofs

## Write And Crash Semantics

`AddMessage` and `AddFullMessage` write an immutable journal of the previous metadata and append offset, append and sync the JSONL, then publish metadata with `HistoryBytes`. A pending journal resolves to OLD or NEW before interpreting a tail. Only that journal authorizes rolling back complete rows. Without a journal, complete rows beyond the metadata checkpoint are preserved, and new messages append at their complete EOF. A failed new append rolls back only to that preserved offset. An unterminated suffix is never promoted. `SetHistory` can use the same path for a proven suffix; replacements and `Compact` journal the previous raw transcript, write JSONL first, and publish metadata last. The journal remains necessary because the shared file writer can fall back to a direct, partially failing metadata write.

An unowned complete tail latches `HistoryRecoveryRequired` and creates `.jsonl.recovery-required` with mode `0600`, syncing both the marker and directory before modifying content. New rows also carry `history_recovery_required`. The owning process keeps the latch write-through beside its warm metadata cache; cold metadata reads and proof boundaries check the sidecar, whose existence fences proofs even if its contents are torn. Observing a marker is distinct from syncing it: content writes ensure its durability before proceeding.

`GetHistory`, ordinary append and normal content lifecycle remain available. `SetHistory`, logical truncation, `Compact`/`Save` and explicit clear reconcile actual rows and preserve the latch rather than failing a completed agent turn. Deliberate truncation or replacement keeps its ordinary content semantics; it does not repair source proofs. If either side of alias promotion requires recovery, both sources retain the latch before copying and the original transcript is preserved. Even an empty cleared session remains fenced for future appends. `ReadHistoryPage`, including existing cursors, returns `ErrHistoryRecoveryRequired`; automatic pruning preserves the session. Explicit launcher DELETE resolves any pending owned write, then removes the source and its recovery marker without requiring capture proof. Summary and scope updates retain the latch. Counting complete physical rows for writer bookkeeping does not attest origin or ordering. No automatic import, new stream or explicit recovery procedure is implemented here.

Older binaries such as `6c89ca2a` and `b523ddb0` do not understand this fence. They can drop unknown JSON fields, and their own capture or destructive operations do not enforce it. The sidecar survives their ordinary appends and transcript/metadata rewrites so that a later compatible binary still refuses proofs, including cursors positioned after a marked row. It cannot prevent data deletion performed by an unaware binary or an external filesystem operation. Keep the marker with the transcript during backups and rollback; removing it is not a supported recovery procedure.

Recovery compares the persisted metadata with the journal's next checkpoint. It records a small OLD/NEW decision bound to the exact journal before restoring OLD data. A failed metadata read leaves the decision unknown and the session fenced; it never authorizes destructive rollback. A failed journal cleanup also keeps the session fenced until cleanup succeeds. This preserves confirmed NEW data when a write reports an error after publishing metadata, and prevents a retried recovery from relabeling already restored OLD data as NEW.

Startup scans recovery files once. A broken journal disables reads/writes only for that session and is logged; healthy sessions retain the JSONL backend. Cold metadata reads do not probe a journal unless this process marked the key pending. Migration excludes metadata and journals from legacy JSON imports.

`TruncateHistory` advances `meta.Skip` against complete physical rows and clears the byte checkpoint for a one-time legacy rescan. An unterminated last row is uncommitted in all transcript scanners. Corrupt and transient thought rows are filtered without renumbering the remaining physical positions; a rewrite that moves positions retires the old generation while retaining occurrence IDs. Bounded capture continues to return an explicit capacity error for a message exceeding 256 KiB.

Emergency compression uses logical truncation only at a safe turn boundary. With a single protected turn and no safe boundary, it leaves persisted history intact; prompt fitting can still trim the in-memory model context.

`JSONLBackend.Save` maps onto `store.Compact(...)`.
In other words, `Save` is no longer "flush dirty memory to disk"; it is now "reclaim dead lines after logical truncation".

## Concurrency Model

`pkg/memory.JSONLStore` uses a fixed 64-shard mutex array keyed by session hash.
That gives per-session serialization without keeping an unbounded mutex map in memory.
Listings and cron pruning use the same sanitized-key lock as writers. Pruning rechecks file ages and recovery state under that lock. For eligible healthy sessions it removes transcript, metadata, journal and decision together, then evicts the cached state; a recovery-required session and its persistent latch remain intact.

This is a single-writer process model, not an interprocess deletion transaction. Deployment and launcher coordination must prevent overlapping writers to the session directory during upgrades and rollback; the metadata cache is not a cross-process consistency mechanism. Launcher DELETE resolves pending writes before removing the explicitly selected source, metadata and sidecars; an unresolved journal still prevents deletion. When both transcript and metadata are absent, the next scoped turn's `UpsertSessionMeta` retires the cached checkpoint and starts a fresh generation before append. A direct append also starts a fresh generation when it exclusively recreates a missing transcript after confirmed metadata deletion. Both paths retire a cached recovery latch only when the durable marker was also deleted; a surviving marker still prevents proofs. Ambiguous recreation fails closed. Callers must separately prevent late events from recreating a globally deleted conversation; the file protocol does not provide that product-level guarantee.

The legacy `SessionManager` uses a single in-memory map guarded by an RW mutex.

Both backends satisfy the same `SessionStore` interface, which is why the agent loop does not need storage-specific code.

## Compatibility And Migration

`pkg/agent/instance.go:initSessionStore` prefers the JSONL backend.

Startup sequence:

1. Create `memory.NewJSONLStore(dir)`.
2. Run `memory.MigrateFromJSON(...)` to import legacy `.json` sessions.
3. Wrap the store with `session.NewJSONLBackend(store)`.
4. If JSONL initialization or migration fails, fall back to `session.NewSessionManager(dir)`.

An individual recovery failure is isolated inside the JSONL store and does not trigger this fallback.

This fallback is intentional: a partial migration would be worse than staying on the legacy store for one run.

### Alias promotion

When canonical metadata is first created, `EnsureSessionMetadata` may promote history from a non-empty legacy alias into the canonical session.
That promotion only happens when the canonical session is still empty, so active canonical history is not overwritten.

This is how the system preserves old histories such as:

- legacy direct-message keys
- older Pico direct-session keys

while moving the runtime onto opaque canonical keys.

## Other SessionStore Implementations

`pkg/agent/subturn.go` defines an `ephemeralSessionStore`.
It satisfies the same `SessionStore` interface, but keeps data in memory only and is destroyed when the sub-turn ends.

That lets SubTurn reuse the same session-facing APIs without writing child-session history into the parent's durable storage.

## Operational Consumers

The session system is consumed by more than the agent loop:

- `web/backend/api/session.go` reads JSONL metadata and legacy JSON sessions to expose session history in the launcher UI.
- `pkg/agent/steering.go` can recover scope metadata for active steering flows.
- tooling and tests can still refer to legacy aliases because alias resolution is handled below the agent loop.

## Related Files

- `pkg/session/session_store.go`
- `pkg/session/manager.go`
- `pkg/session/jsonl_backend.go`
- `pkg/session/scope.go`
- `pkg/session/key.go`
- `pkg/session/allocator.go`
- `pkg/memory/jsonl.go`
- `pkg/agent/instance.go`
- `pkg/agent/agent.go`
- `pkg/agent/agent_message.go`

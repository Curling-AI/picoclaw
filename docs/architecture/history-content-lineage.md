# Content identity and compaction

`JSONLStore` and `JSONLBackend` expose optional `HistoryContentLineage` and
`HistoryContentAppender` capabilities. They identify ordinary persisted content
independently of the ordering evidence described in [Committed history order](history-order.md).
An authenticated internal adapter must negotiate this capability and initialize
each source explicitly. Implementing the interface or reading a frame does not
activate retention for an older caller.

The optional `HistoryContentActivation` query reads only identity metadata and
witness presence. An unsettled journal or decision returns
`ErrHistoryRecoveryRequired` without recovering it. The query does not scan JSONL
content or validate the old physical message count. The context manager checks
that status before using a strict frame: never-initialized callers retain their
legacy compaction
behavior, including incomplete-tail and lost-count recovery. Evidence that a
source was initialized stays active even after identity loss, so it cannot escape
to an unproved destructive cut.

## References and initialization

`ContentFrame` contains the canonical owner, ordinary messages, summary, an
identity status and an opaque state token. Each known `ContentRef` contains a
random domain UUID, occurrence UUID and revision UUID. Identical messages have
different occurrences. Explicit edits retain the selected occurrence and create
a new revision; changing its content back does not restore an earlier revision.
Tokens, refs and mutation UUIDs support equality, strict CAS and idempotency.
They do not establish a receipt sequence or prove that a user viewed anything.

`ReadHistoryContentFrame` is read-only. It does not recover a pending journal,
initialize identity, create a marker or warm the metadata cache. Ownership,
cancellation and I/O failures remain explicit. A pending write returns
`ErrHistoryRecoveryRequired`; the owning writer or startup recovery settles it.
Aliases are rejected by the checked capability. The agent's existing alias
resolution occurs before its compaction view, and a rebound alias fails CAS.

`InitializeHistoryContentIdentity` takes the observation from a prior frame.
It establishes a forward baseline only if that exact physical checkpoint still
exists. It preserves the original JSONL bytes and private order fields. The
baseline has no historical bridge; it never remaps old deliveries to one of
two indistinguishable duplicates. Existing valid receipts may remain separate
bindings. If a nonempty source has no valid existing order checkpoint, baseline
initialization retains the recovery fence instead of making its observed file
size authorize future receipts. Empty new sources remain writable.

A private `.jsonl.content-domain` witness detects loss of established identity.
Missing or inconsistent identity becomes `unavailable`, with ordinary content
still readable and appendable. A getter does not create a replacement domain.
A persisted null identity remains an unavailable tombstone across appends and
restarts. Ordinary initialization cannot replace it, and automatic physical GC
and pruning preserve the source while its archive state is unknown.
Only the owning OLD/NEW journal can finish its own interrupted witness write.
An older writer that discards identity fields cannot preserve continuity merely
by returning the same text, timestamp or array layout.

## Atomic compaction and replay

The legacy `ContextManager` uses the initialized frame before computing a cut or
summary. `CommitHistoryCompaction` receives its exact token, prefix refs and
unchanged suffix refs. Every concurrent append, edit or summary change rejects
the whole plan with `ErrContentStateChanged`. It applies no stale summary or
cut; the next attempt reads and recalculates a fresh view. Uninitialized sources
and unsupported backends retain the established legacy compaction behavior.

Summary, active window, refs and one pending transition are committed together
through the existing OLD/NEW journal. A repeated mutation UUID with the same
plan returns the same result. The compression/summarization event carries that
exact immutable transition. It never rereads a later tail and calls it the
committed postimage. The two immutable sides are obtained separately through
`ReadHistoryCompactionFrame`, avoiding duplicated large payloads in one response.

The active window may accept further appends while the transition is pending.
Those appends advance its token and receive their own refs; they do not alter
the retained transition. Another dependent compaction is rejected before a
write. The pending operation's refs, locators and tokens have a 1 MiB metadata
budget, separate from its summary/content payload. The existing ordinary decoder
and bounded-page limits remain unchanged. The adapter must preserve its existing
1000-message/8 MiB window, per-message budget and production transport envelope.

The archive deadline is 45 seconds. Expiration returns observable backpressure
and preserves the original physical rows. It does not authorize GC or block
ordinary appends. A qualified recovery may replay and confirm the retained
operation after the deadline; no loop silently waits for an ACK while deleting
its only durable source.

## Qualified archive confirmation

`AcknowledgeHistoryCompaction` requires the same domain, mutation UUID, before
and after tokens, exact dropped refs, and a durable archive-commit identifier.
The authenticated consumer supplies it only after the existing archive transaction
has committed those exact drops and projection metadata. A generic HTTP success,
receipt callback or notification is insufficient. A response lost after that
commit can be replayed idempotently. Confirmation changes retention only; it
never clears the independent order fence or grants a read receipt.

While a transition is pending, automatic physical compaction retains its rows,
and destructive replacement/pruning fail explicitly. After qualified confirmation,
physical GC preserves the retained refs and later appends. The existing explicit
owner-requested DELETE still resolves its journal and deletes the source, retiring
its domain. Old ACKs/plans cannot recreate it or bind a newly created source.
The deployment's existing single-writer requirement still applies to deletion.

## Commit observer and private boundary

`SetSessionHistoryContentObserver(func(string, []memory.ContentEntry))` observes
exact durable out-of-turn commits before ingestion. It is independent of
`SetSessionHistoryReceiptObserver`; an absent receipt stays absent. Retain the
batch before scheduling network work. An entry without a ref is unproved content,
not permission to select an old occurrence by similarity.

The message inside the wrapper clears `HistoryID`, `HistoryRunID` and all three
order fields. Its complete persisted delivery origin, creation time, delivery
time, media and tool metadata remain available to the internal adapter. New refs
and transition metadata never become provider messages or public session DTOs.
This capability supplies no public history/recovery endpoint or new SQL content
store; transport negotiation and archive confirmation belong to its consumer.

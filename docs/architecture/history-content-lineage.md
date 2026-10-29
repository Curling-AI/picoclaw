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

A never-initialized physical source with its own exact-key metadata and no
identity, witness or unsettled journal can return a negative activation result
without decoding every other session's metadata. This preserves legacy
compaction even if another session has a broken checkpoint. That negative
probe establishes no alias ownership: qualified reads, initialization and typed
appends retain their separate strict ownership check. A possibly owning metadata
file that is truncated, unreadable or replaced by a directory still fails closed.

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
through `ReadPendingHistoryCompaction` and preserves the original physical rows.
Automatic `Compact`/session `Save` remain successful no-ops while any transition
is pending, including after expiration, so a durable assistant reply still
reaches its caller. Summary and forced-compression preflight reject that pending
transition before provider work. The deadline does not authorize GC or block
ordinary appends. A qualified recovery may replay and confirm the retained
operation after the deadline; no loop silently waits for an ACK while deleting
its only durable source.

If independent content proof is unavailable, `JSONLBackend.Save` uses the
optional `CompactForSessionSave` primitive to retain the physical rows without
invalidating the durable reply, domain, ACK or order evidence. Explicit
`Compact` still reports its proof refusal. The frame and pending APIs continue
to expose unavailable proof and refuse replacement initialization. Only a
proof-only refusal permits this session-save maintenance no-op; filesystem,
unresolved journal, ownership, cancellation and mixed operational errors remain
errors. This availability rule does not make an older writer compatible with
a future storage format or preserve unknown format fields.

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
physical GC preserves the retained refs and later appends. It also retains the
confirmed domain and ACK digest when its active
image is empty, so a lost response can still replay after GC and restart.
The existing explicit
owner-requested DELETE still resolves its journal and deletes the source, retiring
its domain. Old ACKs/plans cannot recreate it or bind a newly created source.
`JSONLBackend.DeleteSession` exposes this intent to the gateway's optional hard
delete interface instead of replacing history and summary independently. The
source's writer lock covers journal recovery and deletion. Preflight ownership
or unresolved-write failures are returned before removal. Once exact private
artifacts are verified, a bounded deletion variant of the existing
`.jsonl.commit.json` journal records their descriptors, original file identities,
digests and expected absences without copying message bodies. That record is
file- and directory-synced before the first unlink. Remaining files are checked
against it before a retry removes anything; a replacement inode, changed bytes,
unsafe link or newly appeared file fails closed. Private payloads retire before
the listing/transcript, and the authorization journal retires last. An I/O
failure can interrupt this authorized cleanup; callers receive the failure and
the journal remains discoverable after metadata removal and across writer
restart. The cron and empty-session sweeps can finish their prior authorization
without repeating the first GC decision or an already successful empty-session
hook. A normal append journal is never deletion authority. Automatic pruning
retains its separate stricter guards before creating a cleanup authorization.
The deployment's existing single-writer requirement still applies to deletion.
File-identity checks do not promise automatic replay after a filesystem identity
changes, such as a restore to another volume; ambiguous evidence is preserved.

The optional `HistoryContentArchiveCoverage` capability negotiates coverage
version 1 and a locally configured projection policy. It keeps `Archived` refs
separate from `Excluded` ref/reason metadata. Their union must cover every
dropped ref exactly once. The producer checks each disposition against its
immutable raw preimage through a trusted local classifier, never one supplied
by RPC. The coverage digest also binds the exact archive transaction. The v5
all-archived ACK remains strict and cannot confirm an excluded subset.

## External storage and append qualification

Private disk format 2 keeps small headers in metadata, the current checkpoint
and the ordinary cache. Locators live in a source-owned appendable `.idx` file;
Pending/Last are immutable `.blob` records. Their names do not match legacy
session JSON discovery. The public lineage and mutation wire versions stay 1,
and the original semantic discriminator remains separate from the disk format.
Unsupported formats retain their opaque evidence and cannot initialize a new
domain or recover a counter by scanning retained rows.

`AppendHistoryContentInput` returns a detached accepted body and, only when
independently known, its genuine producer receipt. It returns no content ref,
binding, domain or before/after interval. Its index append extends an expected
`sha256-record-chain-v1` commitment from new locator bytes; extent checks are a
write boundary, not proof that the previous prefix was reread. Metadata and the
OLD/NEW journal retain descriptors and the exact index delta rather than expanded
vectors or rewritten Pending/Last payloads. Recovery can truncate only its own
authorized suffix, preserving unowned complete rows.

Explicit `AppendHistoryContent`, frame readers and destructive proof boundaries
still validate the complete committed JSONL/index prefixes and exact locators.
A same-size edit to an earlier row therefore cannot receive known content proof
or authorize a cut. Unverified record counts cannot size an allocation; readers
validate count/extent coherence and grow only from actual decoded records. The
public adapter budgets do not become an artificial physical-prefix/ref cap.

The append lane's smaller storage work does not establish constant-time
end-to-end operation. A mounted consumer can still schedule full strict reads
and contend on the same source lock; measurements must include that asynchronous
work and all metadata, journal, index, blob and checkpoint bytes. Local APFS
measurements do not establish EFS cost or deployment performance.

Within the same owning store, content readers and checked mutation/ACK entry
points acquire the source lock before inspecting its journal or ownership
checkpoint. They wait for an active append's settled result instead of treating
its temporary journal as an unresolved recovery. A journal left unresolved
after the writer returns still fences pure reads without automatic recovery.

Pure getters neither migrate nor rewrite a format. The first owning mutation
can migrate a fully qualified inline source without changing its domain, refs,
state, frozen operation or independent order. Explicit
`PrepareHistoryContentInlineRollback` performs the reverse conversion for the
mutation-aware inline baseline `dfd1c2d`; it does not support older V7 writers.
Both directions preserve complete Pending/Last and exact application ACK replay.
Missing, corrupt or opaque required provenance refuses conversion and preserves
the files.

`messageutil.ProjectLegacyPublic` implements the producer's public-session
predicate. The currently published consumer still has its own copy; the
consumer follow-up must qualify their alignment before claiming shared use.
Sender provenance is classified before stripping the sender
prefix: a tagged person's literal `[System]` message remains public. Internal
steering and unsupported roles receive separate exclusion reasons without
copying their bodies to coverage metadata or a new content store. The 1 MiB
metadata budget applies to physical ref coverage; the consumer's 1000-message,
8 MiB and per-message budgets still govern its complete public payload. Private
refs do not impose an artificial 1000-ref limit or justify partial confirmation.

## Commit observer and private boundary

`SetSessionHistoryContentObserver(func(string, []memory.ContentEntry))` observes
exact durable out-of-turn commits before ingestion. It is independent of
`SetSessionHistoryReceiptObserver`; an absent receipt stays absent. Retain the
batch before scheduling network work. An entry without a ref is unproved content,
not permission to select an old occurrence by similarity.

`SetSessionHistoryContentCommitObserver` additionally reports each
`ContentAppendResult` with its exact `DomainID`, `BeforeState` and `AfterState`,
captured under the append lock. Interleaving writes remain gaps between commit
intervals. The callback receives its own detached message/ref/receipt snapshot
before the v5 observer and ingestion can schedule asynchronous work. The three
state fields stay empty together when content identity is unavailable, while
independently valid producer order keeps its existing receipt contract.

The optional `SetSessionHistoryContentInputObserver` selects the accepted-input
wake-up lane. Its payload never qualifies the old prefix, and a consumer must
read a strict frame when it needs refs. A qualified subscriber deliberately
keeps the qualified append lane; mixed registrations observe one append rather
than persisting the input twice. Non-checked fallback storage confirms its Save
before publishing a hint, and failed Save excludes only the unconfirmed members
of a mixed batch. Capability registration does not initialize content identity.

The message inside the wrapper clears `HistoryID`, `HistoryRunID` and all three
order fields. Its complete persisted delivery origin, creation time, delivery
time, media and tool metadata remain available to the internal adapter. New refs
and transition metadata never become provider messages or public session DTOs.
This capability supplies no public history/recovery endpoint or new SQL content
store; transport negotiation and archive confirmation belong to its consumer.

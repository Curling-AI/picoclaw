# Checked history mutations

`memory.HistoryContentMutations` is an optional internal capability for sources
that explicitly initialized content identity. Implementing the interface does
not initialize a source. Ordinary history content remains a separate concern
from producer order and user read receipts.

The capability supports two intents:

* `edit` keeps the exact occurrence layout and checks every selected revision.
  Changed ordinary content receives a new revision. The existing order helper
  allocates a genuinely new sequence for changed message cores on healthy
  sources; metadata-only revisions keep the genuine order of their unchanged
  core. Lost order proof is never reconstructed.
* `retain_prefix` keeps the exact selected prefix, including hidden entries,
  preserves its messages and the current summary, and discards the rest. An
  empty prefix preserves the domain and known highwater. Resend appends its
  replacement prompt afterwards as a new occurrence through the normal append
  pipeline. This intent is distinct from head-drop/suffix compaction and owner
  deletion.

Each new plan supplies a UUID `MutationID`, an opaque request `IntentDigest`
(SHA-256), the exact domain and `BeforeState`, and one kind-specific selection.
Any intervening append, edit, summary change or truncation rejects the whole
plan. Private history fields and incoming receipt bindings cannot mint proof.

An admission fixes its ID, intent and complete plan before the first commit.
`ReadHistoryContentMutation` can find its exact pending or applied result;
`ReadLatestHistoryContentMutation` exposes only the existing Pending/Last slots.
An absent Last does not prove an old command was never executed. Callers must
never replan an old ID against a fresh state. A gateway needs a real external
command/idempotency identity to recover an admission after restart; equal text
or timestamps do not identify a retry. This protocol does not extend model-run
deduplication beyond the gateway's existing retention horizon.

One source-wide Pending slot holds either compaction or a typed mutation.
Ordinary appends and session Save remain available while it awaits application,
including after the confirmation deadline. A dependent rewrite is blocked.
Exact retries of Pending replay its immutable transition; another Pending takes
priority over Last replay. With no Pending, exact Last returns `applied` and its
retained `AppliedAck` without opening old payload files.

An edit or physical prefix cut first retains a source-owned immutable raw
preimage. Its exact UUID, key, domain, mutation, byte length and SHA-256 are
rooted by the source OLD/NEW journal. Files use exclusive creation,
non-following opens, regular-file/hardlink checks and file/directory fsync.
Before locators select those old bytes. After locators select the exact new
image; later ordinary appends do not replace that frozen view. A pre-journal
orphan without committed provenance is preserved rather than adopted or
deleted by filename.

`ReadHistoryContentMutationFrame` fetches each side separately. Frozen frames
contain content and refs, and **never contain `ReceiptBinding`**. Absence of a
binding in that transport frame does not itself prove order loss. Receipt
eligibility requires separate currently valid producer evidence; neither refs,
application ACKs nor a previously stored SQL binding reattest that proof.

`AcknowledgeHistoryContentMutation` accepts only the exact kind, domain, command,
intent, states, both ref vectors and the authenticated consumer's durable
application commit ID. It confirms application to the existing projection and
identity transaction. It is not a claim that a discarded tail was archived or
that a user viewed it. Legacy archive/coverage ACKs cannot release typed Pending,
and application ACKs cannot release compaction. Qualified application closes
Last; preimages may then be retired. Failed optional retirement preserves its
descriptor/evidence and does not undo an already committed application.

The checked-edit signature `CommitHistoryContentEdit` delegates to this typed
operation and returns the frozen After frame. Its locally generated ID does not
give a caller without a command ID an exactly-once promise. A second edit waits
for ApplicationACK; the controller discovers the real pending pair instead of
inferring it from a later getter and SQL.

The semantic writer stores a current checkpoint digest in a versioned
external content-domain witness. Summary, Pending, Last and ACK changes update
the metadata and witness in the same OLD/NEW journal, even when JSONL bytes stay
equal. Metadata-only rollback therefore withholds identity rather than forgetting
an operation. Scope, aliases and UpdatedAt alone do not change this content
projection. Explicit owner deletion validates exact rooted artifacts before
clearing the body; it never removes unowned files by glob.

The witness also keeps a separate constant-size commitment to the actual order
metadata. It detects a lost counter even after larger sequences left the retained
window. A mismatch closes receipts and preserves content without reconstructing
the counter from retained messages. Proof loss retains the last known commitment
or an unavailable state; it never reattests the current unknown number. Content
frames and replay metadata remain independent. Checkpoint files reject symbolic
links and hard links and use synced atomic replacement without a direct-write
fallback. Persistent I/O/recovery failures remain explicit, including after
source NEW; a retained command ID and original plan resolve that outcome safely.

Ordinary append also distinguishes an accepted input from pending operational
recovery. `ErrHistoryInputCommitted` retains the I/O cause; callers must handle
this outcome before any receipt fallback and must not append the same input
again. `AppendHistoryContent` returns its detached accepted body alongside that
error, with no new ref, binding, receipt or state interval. The mirror caller
reports the recovery failure separately and retains/notifies that body once.
This is not a new deduplication ledger or proof of an uncompleted durability
phase. A lost independent order witness still withholds receipts; a real sync
failure keeps its journal until a writer completes the actual file/directory
barriers. Never-initialized legacy sources retain their existing storage path.

A journal-unlink failure after the NEW decision and all required durability
phases is cleanup work. It retains and logs its recovery fence without revoking
an already qualified receipt or inviting a second append. This narrow phase is
not a sync failure and cannot absorb a mixed operational cause. Active sources
also validate the full current frame at legacy `TruncateHistory`, even without
an intervening append, before changing their retained suffix. A physical dangling
content witness remains activation evidence; it is never a never-initialized
source or permission to invent known proof.

Private disk format 2 retains small headers and exact index deltas in ordinary
metadata/journals, with appendable locator indexes and immutable Pending/Last
capsules. Public mutation/lineage versions remain 1. An accepted-input wake-up
never attests a prefix: strict mutation, frame and destructive boundaries still
validate complete physical evidence. Independent Order can remain genuine after
content-only checkpoint loss, but no content ref, binding or state interval is
issued for that unavailable domain. Lost Order itself still withholds receipts.

`PrepareHistoryContentInlineRollback` explicitly converts a fully qualified
source to the mutation-aware inline writer `dfd1c2d`, preserving exact pending
payloads and closed ACK replay before changing writers. Missing or opaque
provenance refuses conversion. Older V7 writers do not understand these semantic
records and remain unsupported. Neither the external index nor a smaller
ordinary append cost establishes constant-time qualified reads or end-to-end
consumer work. See [content lineage](history-content-lineage.md) for storage,
owner deletion and observer boundaries.

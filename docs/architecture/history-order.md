# Committed history order

The JSONL producer exposes durable ordering evidence through optional APIs on `memory.JSONLStore` and `session.JSONLBackend`:

```go
AppendHistoryReceipt(ctx, key, message) (memory.CommittedHistoryMessage, error)
ReadHistoryWithReceipts(ctx, key) ([]memory.CommittedHistoryMessage, error)
```

`CommittedHistoryMessage` contains the exact prepared message and a `HistoryOrderProof` with `Version`, `StreamID`, and `Sequence`. Version 1 uses a persistent canonical-session UUID and a dedicated counter from 1 through `2^53-1`. Reaching the limit returns an explicit error; it does not rotate the stream.

The receipt is constructed from the serialized write buffer and returned only after the durable commit decision, while the same physical-session lock still protects the operation. Its message owns its nested data and does not alias the caller's argument. A write or recovery that cannot establish a commit returns no live receipt. Recovery of a confirmed NEW commit preserves its receipt and counter; cleanup failure must never turn a confirmed commit into an OLD rollback.

Persistence can succeed without a live receipt, for example when the process stops after commit but before returning it. Consumers reconcile on startup and reconnect through `ReadHistoryWithReceipts`; they must not infer a failed write from a missing callback or retry an uncertain append as if it had not committed.

Recovery confirmation requires a durable NEW decision bound to that journal and metadata matching the complete order and legacy-origin checkpoint. Readable NEW metadata alone does not acknowledge a receipt. Once those guarantees hold, an unlink failure returns the confirmed receipt without an error that would request a duplicate append; recovery stays fenced and the cleanup error is logged.

This follows the JSONL store's existing single-writer-per-directory contract. Ordering is allocated by the producer's commit, without timestamps, observer arrival order, generation UUID comparison, transcript indices, or a later tail read.

## Identity and mutations

`AppendHistoryReceipt` creates a new occurrence ID even if its argument carries an older `HistoryID`. It does not deduplicate repeated input. Two real appends with identical text and timestamps have distinct occurrence IDs and sequences.

On a proven source, `SetHistory` preserves an existing occurrence's order only when its ID and content still identify the stored occurrence. Changed content receives a new sequence even when the caller explicitly preserves the same ID. Logical rollback, truncation, and compaction never lower the committed counter. Unchanged retained occurrences keep their proof across restart and compaction. Recovery-required sources instead permit content changes without assigning new proofs, as described below.

The returned array follows transcript order. Editing the middle of a transcript can produce sequences `[1, 5, 3, 4]`: sequence 5 identifies the new content event; the unchanged later transcript entries retain their earlier proofs. Consumers must not use the last array index as the producer highwater or infer ordering from the array's physical positions.

For a source without a recovery-required fence, confirmed physical deletion and recreation starts a new stream. There is no implicit predecessor relationship between two stream UUIDs. Alias resolution verifies current canonical ownership; healthy promotion allocates fresh proofs in the canonical stream while retaining the canonical highwater. Promotion involving a recovery-required source or destination transfers only content, marks the destination too, and retains the original source. It does not copy an alias stream's proof into the canonical stream.

## Legacy origin

A missing proof is represented by a completely zero `Order`. It is never converted into a version 1 sequence.

The first version 1 commit records a fence for the existing unproven prefix, using its file generation and committed byte boundary under the same lock and journal. Rewrites of unchanged legacy occurrences preserve an explicit private origin marker in their records. A zero-proof record is attested as preceding the stream's first version 1 commit only when that fence or marker proves its origin.

Corrupt rows proven to belong to the original legacy prefix remain filtered. An implicit legacy offset upgrade cannot attest an uncommitted tail. A physical row count that exceeds its metadata checkpoint requires recovery before order is available. Proven replacements reuse origin evidence only from the committed checkpoint. Content truncation, replacement and compaction can proceed with the recovery fence retained; they do not turn an external complete tail into attested legacy content or allocate new proofs.

`CommittedHistoryMessage.LegacyBeforeStreamID` carries this atomic origin attestation. It is private, is not a sequence or a version 1 proof, and stays available even if the active transcript contains only legacy entries after all active version 1 messages have been removed. An edited version 1 entry physically before a retained legacy suffix does not change the suffix's origin.

An absent frame, unavailable RPC, missing metadata, malformed version 1 proof, foreign stream, or missing modern proof is not an attested legacy entry. Consumers keep unproven order unavailable. If no prior stream evidence exists, legacy entries remain unbound; consumers must not guess a stream UUID or a predecessor relationship.

JSONL files that predate metadata remain readable with zero proofs and no origin attestation. This compatibility path rejects any residual version 1 proof or persisted legacy-origin marker; it does not silently downgrade modern evidence when metadata is missing.

## Recovery-required content

An older writer can leave a complete, durable row beyond its metadata checkpoint or discard a modern stream's counter. If the producer cannot prove the prefix or stream highwater, ordinary runtime appends preserve the complete content and continue without assigning proofs. They never infer the counter from the largest retained sequence, renumber existing entries, or attest the extra rows as legacy origin. `AppendHistoryReceipt` records the recovery requirement and rejects that state before writing its input; `ReadHistoryWithReceipts` remains unavailable.

On an append, `ErrHistoryReceiptUnavailable` and `ErrHistoryOwnershipUnavailable` identify rejection before that input was written. They allow a checked content-only fallback. Other write errors can describe an uncertain commit and do not authorize that retry.

The private `history_recovery_required` flag in metadata and new records marks this state. A durable per-session `.jsonl.recovery-required` sidecar preserves it even if an older writer later removes those JSON fields. Warm content operations retain the observed fence in the write-through cache; cold reads and proof boundaries check the durable marker. Ordinary lifecycle operations do not clear it. Provider reads, normal turns, Save/compaction, requested content edits, logical truncation, rollback and clear remain available. Bounded cursor pages and order receipts stay unavailable; deletion and pruning retain their recovery protections. A valid legacy session without order metadata does not require recovery merely because its proof is absent.

Before a destructive content mutation can hide the last stored modern proof, the producer checks the existing private evidence and latches any missing continuity. On a marked source, unchanged occurrences may retain their already stored proof only as private evidence. Edited or new occurrences receive no proof, and caller-supplied proofs cannot substitute for stored evidence. These operations neither reconstruct the highwater nor recover receipt eligibility. In particular, clearing a conversation and then appending does not create a replacement stream or clear its recovery fence.

Recovery is an explicit owner decision. One supported action is to retain the original source and deliberately import its trusted content with `SetHistory` into a fresh canonical session. This creates a new stream with its own proofs, without implying continuity with the old one; it does not clear the original source's fence. There is no automatic in-place repair or recovery endpoint in this change.

Older binaries do not understand this fence. In particular, the legacy producer and the earlier bounded-history producer can ignore its fields and sidecar while running. Returning to an aware binary preserves the recovery requirement, but the sidecar cannot prevent an older binary from editing or deleting data. Keep the source and its private files together when preserving recovery evidence.

## Out-of-turn observer

`AgentLoop.SetSessionHistoryReceiptObserver(func(sessionKey string, committed []memory.CommittedHistoryMessage))` observes durable out-of-turn writes. The callback runs synchronously before ingestion can enqueue network work. Consumers retain these exact receipts before starting goroutines or network requests.

A batch preserves the existing write policy: when `b` fails while `a` and `c` are committed, the callback contains the real receipts for `[a, c]`. The failed input has no receipt. A later write reaching storage before the callback does not replace an earlier message's receipt.

For a legacy backend without receipt support, a **nil batch** means only that legacy history was confirmed through its existing Save/checked-write gate. The same notification applies when canonical ownership lookup is unavailable before the write or a recovery-required JSONL source confirms a content-only write through that gate. It carries no committed-message claim and no order proof, and is useful only for legacy source amendment. A known ownership conflict and failed persistence emit no such notification. An uncertain receipt commit is never retried through this fallback. The old session-history observer remains compatible.

## Private boundaries

Order metadata is persisted only in private JSONL records and session metadata. `providers.Message` carries private storage fields with `json:"-"`; `JSONLBackend.GetHistory` clears them for provider use. The legacy origin attestation belongs to the receipt wrapper and also has `json:"-"`.

The provider content reader ignores invalid or future private order metadata on otherwise valid message records. Recovery-aware content mutations keep the fence and preserve useful private evidence for unchanged content. Receipt readers validate private metadata strictly; content compatibility does not turn malformed proof into legacy evidence.

The internal RPC adapter must explicitly carry the proof and origin fields. Public session DTOs and provider requests do not expose stream IDs, sequences, or origin attestations. The public session-detail regression exercises this boundary through the real HTTP handler.

The bounded-history capacity policy is unchanged: no message content is silently cut, and the existing explicit record/page capacity errors remain in force.

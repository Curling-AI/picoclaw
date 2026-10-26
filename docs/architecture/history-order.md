# Committed history order

The JSONL producer exposes durable ordering evidence through optional APIs on `memory.JSONLStore` and `session.JSONLBackend`:

```go
AppendHistoryReceipt(ctx, key, message) (memory.CommittedHistoryMessage, error)
ReadHistoryWithReceipts(ctx, key) ([]memory.CommittedHistoryMessage, error)
```

`CommittedHistoryMessage` contains the exact prepared message and a `HistoryOrderProof` with `Version`, `StreamID`, and `Sequence`. Version 1 uses a persistent canonical-session UUID and a dedicated counter from 1 through `2^53-1`. Reaching the limit returns an explicit error; it does not rotate the stream.

The receipt is constructed from the serialized write buffer and returned only after the durable commit decision, while the same physical-session lock still protects the operation. Its message owns its nested data and does not alias the caller's argument. A write or recovery that cannot establish a commit returns no live receipt. Recovery of a confirmed NEW commit preserves its receipt and counter; cleanup failure must never turn a confirmed commit into an OLD rollback.

Recovery confirmation requires a durable NEW decision bound to that journal and metadata matching the complete order and legacy-origin checkpoint. Readable NEW metadata alone does not acknowledge a receipt. Once those guarantees hold, an unlink failure returns the confirmed receipt without an error that would request a duplicate append; recovery stays fenced and the cleanup error is logged.

This follows the JSONL store's existing single-writer-per-directory contract. Ordering is allocated by the producer's commit, without timestamps, observer arrival order, generation UUID comparison, transcript indices, or a later tail read.

## Identity and mutations

`AppendHistoryReceipt` creates a new occurrence ID even if its argument carries an older `HistoryID`. It does not deduplicate repeated input. Two real appends with identical text and timestamps have distinct occurrence IDs and sequences.

`SetHistory` preserves an existing occurrence's order only when its ID and content still identify the stored occurrence. Changed content receives a new sequence even when the caller explicitly preserves the same ID. Logical rollback, truncation, and compaction never lower the committed counter. Unchanged retained occurrences keep their proof across restart and compaction.

The returned array follows transcript order. Editing the middle of a transcript can produce sequences `[1, 5, 3, 4]`: sequence 5 identifies the new content event; the unchanged later transcript entries retain their earlier proofs. Consumers must not use the last array index as the producer highwater or infer ordering from the array's physical positions.

Confirmed physical deletion and recreation starts a new stream. There is no implicit predecessor relationship between two stream UUIDs. Alias resolution verifies current canonical ownership; promotion allocates fresh proofs in the canonical stream while retaining the canonical highwater. It does not copy an alias stream's proof into the canonical stream.

## Legacy origin

A missing proof is represented by a completely zero `Order`. It is never converted into a version 1 sequence.

The first version 1 commit records a fence for the existing unproven prefix, using its file generation and committed byte boundary under the same lock and journal. Rewrites of unchanged legacy occurrences preserve an explicit private origin marker in their records. A zero-proof record is attested as preceding the stream's first version 1 commit only when that fence or marker proves its origin.

Corrupt rows proven to belong to the original legacy prefix remain filtered. An implicit legacy offset upgrade fails if its physical row count disagrees with the committed metadata; it cannot attest an uncommitted tail. Replacements reuse origin evidence only from the committed checkpoint; explicitly importing content from an external tail allocates a new proof. Compaction rejects active rows outside that checkpoint without changing their bytes.

`CommittedHistoryMessage.LegacyBeforeStreamID` carries this atomic origin attestation. It is private, is not a sequence or a version 1 proof, and stays available even if the active transcript contains only legacy entries after all active version 1 messages have been removed. An edited version 1 entry physically before a retained legacy suffix does not change the suffix's origin.

An absent frame, unavailable RPC, missing metadata, malformed version 1 proof, foreign stream, or missing modern proof is not an attested legacy entry. Consumers keep unproven order unavailable. If no prior stream evidence exists, legacy entries remain unbound; consumers must not guess a stream UUID or a predecessor relationship.

JSONL files that predate metadata remain readable with zero proofs and no origin attestation. This compatibility path rejects any residual version 1 proof or persisted legacy-origin marker; it does not silently downgrade modern evidence when metadata is missing.

## Out-of-turn observer

`AgentLoop.SetSessionHistoryReceiptObserver(func(sessionKey string, committed []memory.CommittedHistoryMessage))` observes durable out-of-turn writes. The callback runs synchronously before ingestion can enqueue network work. Consumers retain these exact receipts before starting goroutines or network requests.

A batch preserves the existing write policy: when `b` fails while `a` and `c` are committed, the callback contains the real receipts for `[a, c]`. The failed input has no receipt. A later write reaching storage before the callback does not replace an earlier message's receipt.

For a legacy backend without receipt support, a **nil batch** means only that legacy history was confirmed through its existing Save/checked-write gate. It carries no committed-message claim and no order proof, and is useful only for legacy source amendment. Failed JSON Save emits no such notification. The old session-history observer remains compatible.

## Private boundaries

Order metadata is persisted only in private JSONL records and session metadata. `providers.Message` carries private storage fields with `json:"-"`; `JSONLBackend.GetHistory` clears them for provider use. The legacy origin attestation belongs to the receipt wrapper and also has `json:"-"`.

The internal RPC adapter must explicitly carry the proof and origin fields. Public session DTOs and provider requests do not expose stream IDs, sequences, or origin attestations. The public session-detail regression exercises this boundary through the real HTTP handler.

The bounded-history capacity policy is unchanged: no message content is silently cut, and the existing explicit record/page capacity errors remain in force.

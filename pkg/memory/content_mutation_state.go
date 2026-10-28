package memory

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// This semantic baseline understands every Pending/Last kind. It precedes the
// external-index format; older V7 writers are not a rollback target for it.
const contentMutationVersion = uint8(1)

type contentMutationPending struct {
	Transition ContentMutationTransition `json:"transition"`
	PlanDigest string                    `json:"planDigest"`
	Before     contentSnapshot           `json:"before"`
	After      contentSnapshot           `json:"after"`
}

type contentMutationAcknowledged struct {
	Transition ContentMutationTransition `json:"transition"`
	PlanDigest string                    `json:"planDigest"`
	Ack        ContentMutationAppliedAck `json:"ack"`
	AckDigest  string                    `json:"ackDigest"`
	Retired    *contentPreimage          `json:"retired,omitempty"`
}

func validContentMutationKind(kind ContentMutationKind) bool {
	return kind == ContentMutationEdit || kind == ContentMutationRetainPrefix
}

func validContentMutationTransition(transition ContentMutationTransition, key, domain string) bool {
	if transition.Version != ContentLineageVersion || transition.CanonicalKey != key || transition.DomainID != domain ||
		uuid.Validate(transition.MutationID) != nil || !validHistoryDigest(transition.IntentDigest) ||
		!validContentMutationKind(transition.Kind) || !validHistoryStreamID(transition.BeforeState) ||
		!validHistoryStreamID(transition.AfterState) || transition.BeforeState == transition.AfterState ||
		transition.ExpiresAt.IsZero() {
		return false
	}
	for _, refs := range [][]ContentRef{transition.Before, transition.After} {
		seen := make(map[string]bool, len(refs))
		for _, ref := range refs {
			if !ref.Valid() || ref.DomainID != domain || seen[ref.OccurrenceID] {
				return false
			}
			seen[ref.OccurrenceID] = true
		}
	}
	return true
}

func validContentMutationState(state *contentIdentityState, key string) bool {
	if state.MutationVersion > contentMutationVersion {
		return false
	}
	if state.storage != nil && !state.storage.SlotsLoaded {
		// Capsules are validated wire metadata, not hydrated transition proof.
		// Strict/replay callers hydrate before exposing vectors or an ACK.
		return validContentStorageHeader(state, key)
	}
	if pending := state.Pending; pending != nil && pending.Mutation != nil {
		mutation := pending.Mutation
		if state.MutationVersion != contentMutationVersion || pending.Transition.Version != 0 ||
			pending.PlanDigest != "" || pending.Before.StateToken != "" || pending.After.StateToken != "" ||
			!validContentMutationTransition(mutation.Transition, key, state.DomainID) ||
			!validHistoryDigest(mutation.PlanDigest) || mutation.Before.StateToken != mutation.Transition.BeforeState ||
			mutation.After.StateToken != mutation.Transition.AfterState || mutation.Before.Image == nil ||
			!mutation.Before.Image.valid(
				key,
				state.DomainID,
				mutation.Transition.MutationID,
			) || mutation.After.Image != nil ||
			!sameContentRefs(refsForLocators(mutation.Before.Rows), mutation.Transition.Before) ||
			!sameContentRefs(refsForLocators(mutation.After.Rows), mutation.Transition.After) {
			return false
		}
	}
	if last := state.Last; last != nil && last.Mutation != nil {
		mutation := last.Mutation
		if state.MutationVersion != contentMutationVersion || last.Transition.Version != 0 || last.PlanDigest != "" ||
			last.AckDigest != "" || !validContentMutationTransition(mutation.Transition, key, state.DomainID) ||
			!validHistoryDigest(
				mutation.PlanDigest,
			) || !matchingContentMutationAck(mutation.Ack, mutation.Transition) ||
			mutation.AckDigest != contentJSONDigest(mutation.Ack) ||
			(mutation.Retired != nil && !mutation.Retired.valid(key, state.DomainID, mutation.Transition.MutationID)) {
			return false
		}
	}
	return true
}

func pendingContentDeadline(pending *contentPending) time.Time {
	if pending.Mutation != nil {
		return pending.Mutation.Transition.ExpiresAt
	}
	return pending.Transition.ExpiresAt
}

func cloneContentMutationResult(result ContentMutationResult) ContentMutationResult {
	data, _ := json.Marshal(result)
	var cloned ContentMutationResult
	_ = json.Unmarshal(data, &cloned)
	return cloned
}

func pendingContentMutationResult(pending *contentMutationPending) ContentMutationResult {
	return cloneContentMutationResult(
		ContentMutationResult{Transition: pending.Transition, Status: ContentMutationPending},
	)
}

func appliedContentMutationResult(last *contentMutationAcknowledged) ContentMutationResult {
	return cloneContentMutationResult(ContentMutationResult{
		Transition: last.Transition, Status: ContentMutationApplied, AppliedAck: &last.Ack,
	})
}

func matchingContentMutationAck(ack ContentMutationAppliedAck, transition ContentMutationTransition) bool {
	return ack.AppliedCommitID != "" && len(ack.AppliedCommitID) <= 256 && ack.DomainID == transition.DomainID &&
		ack.MutationID == transition.MutationID && ack.IntentDigest == transition.IntentDigest &&
		ack.Kind == transition.Kind && ack.BeforeState == transition.BeforeState && ack.AfterState == transition.AfterState &&
		sameContentRefs(ack.Before, transition.Before) && sameContentRefs(ack.After, transition.After)
}

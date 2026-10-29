package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

const (
	contentStorageVersion = uint8(2)
	contentIndexAlgorithm = "sha256-record-chain-v1"
)

// These descriptors name exact private files, not discovery/glob candidates.
// Blob Digest is SHA256(bytes); the index uses the explicit record-chain
// algorithm and a separate physical extent. Neither is proof without hydration.
type contentArtifactDescriptor struct {
	Version    uint8  `json:"version"`
	Key        string `json:"key"`
	DomainID   string `json:"domain"`
	Generation string `json:"generation"`
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Bytes      int64  `json:"bytes"`
	Digest     string `json:"digest"`
}

type contentIndexDescriptor struct {
	File      contentArtifactDescriptor `json:"file"`
	Algorithm string                    `json:"algorithm"`
	Records   int                       `json:"records"`
	Start     int                       `json:"start"`
	Count     int                       `json:"count"`
}

// A slot capsule keeps guards/ownership bounded without decoding its vectors.
// Full transitions/ACKs are returned only after reading the immutable blob.
type contentSlotDescriptor struct {
	File        contentArtifactDescriptor `json:"file"`
	Typed       bool                      `json:"typed,omitempty"`
	Kind        ContentMutationKind       `json:"kind,omitempty"`
	MutationID  string                    `json:"mutationId"`
	Intent      string                    `json:"intent,omitempty"`
	PlanDigest  string                    `json:"planDigest"`
	BeforeState string                    `json:"before"`
	AfterState  string                    `json:"after"`
	ExpiresAt   time.Time                 `json:"expiresAt"`
	AckDigest   string                    `json:"ackDigest,omitempty"`
	CommitID    string                    `json:"commitId,omitempty"`
}

type contentStorageHeader struct {
	Version      uint8                  `json:"version"`
	Key          string                 `json:"key"`
	Generation   string                 `json:"generation"`
	Index        contentIndexDescriptor `json:"index"`
	Pending      *contentSlotDescriptor `json:"pending,omitempty"`
	Last         *contentSlotDescriptor `json:"last,omitempty"`
	BeforeImage  *contentPreimage       `json:"beforeImage,omitempty"`
	RetiredImage *contentPreimage       `json:"retiredImage,omitempty"`
}

type contentStorageState struct {
	Header       contentStorageHeader
	RowsLoaded   bool
	SlotsLoaded  bool
	Appended     []contentLocator
	InlineTarget bool
}

type contentWireIdentity struct {
	Version         uint8                `json:"version"`
	DomainID        string               `json:"domain"`
	StateToken      string               `json:"state"`
	SummaryDigest   string               `json:"summaryDigest"`
	Skip            int                  `json:"skip"`
	Count           int                  `json:"count"`
	Unavailable     bool                 `json:"unavailable,omitempty"`
	MutationVersion uint8                `json:"mutationVersion,omitempty"`
	DiskVersion     uint8                `json:"diskVersion"`
	Storage         contentStorageHeader `json:"storage"`
}

func contentIdentityWire(state *contentIdentityState) any {
	if state == nil {
		return nil
	}
	if len(state.opaque) != 0 {
		return state.opaque
	}
	if state.storage == nil || state.storage.InlineTarget {
		return state
	}
	return contentWireIdentity{
		Version:         state.Version,
		DomainID:        state.DomainID,
		StateToken:      state.StateToken,
		SummaryDigest:   state.SummaryDigest,
		Skip:            state.Skip,
		Count:           state.Count,
		Unavailable:     state.Unavailable,
		MutationVersion: state.MutationVersion,
		DiskVersion:     contentStorageVersion,
		Storage:         state.storage.Header,
	}
}

func decodeContentIdentity(data []byte) *contentIdentityState {
	var version struct {
		Version         uint8 `json:"version"`
		DiskVersion     uint8 `json:"diskVersion"`
		MutationVersion uint8 `json:"mutationVersion"`
		Unavailable     bool  `json:"unavailable"`
	}
	if json.Unmarshal(data, &version) != nil {
		return opaqueContentIdentity(data)
	}
	knownUnavailable := version.Version == 0 && version.DiskVersion == 0 && version.MutationVersion == 0 &&
		version.Unavailable
	if (!knownUnavailable && version.Version != ContentLineageVersion) ||
		version.MutationVersion > contentMutationVersion ||
		(version.DiskVersion != 0 && version.DiskVersion != contentStorageVersion) {
		return opaqueContentIdentity(data)
	}
	if version.DiskVersion == 0 {
		var state contentIdentityState
		if decodeContentWire(data, &state) != nil {
			return opaqueContentIdentity(data)
		}
		if !validContentMutationState(&state, contentStateKey(&state)) &&
			((state.Pending != nil && state.Pending.Mutation != nil) || (state.Last != nil && state.Last.Mutation != nil)) {
			return opaqueContentIdentity(data)
		}
		return &state
	}
	var wire contentWireIdentity
	if decodeContentWire(data, &wire) != nil {
		return opaqueContentIdentity(data)
	}
	state := &contentIdentityState{
		Version: wire.Version, DomainID: wire.DomainID, StateToken: wire.StateToken,
		SummaryDigest: wire.SummaryDigest, Skip: wire.Skip, Count: wire.Count, Unavailable: wire.Unavailable,
		MutationVersion: wire.MutationVersion, storage: &contentStorageState{Header: wire.Storage},
	}
	if !validContentStorageHeader(state, wire.Storage.Key) {
		return opaqueContentIdentity(data)
	}
	state.restoreContentCapsules()
	return state
}

func opaqueContentIdentity(data []byte) *contentIdentityState {
	var state contentIdentityState
	_ = json.Unmarshal(data, &state)
	var wire contentWireIdentity
	if json.Unmarshal(data, &wire) == nil && wire.DiskVersion != 0 {
		state.storage = &contentStorageState{Header: wire.Storage}
		if validContentStorageHeader(&state, wire.Storage.Key) {
			state.restoreContentCapsules()
		} else {
			state.storage = nil
		}
	}
	// Known diagnostic scalars may remain visible, but every proof/mutation
	// boundary rejects opaque state and the original wire bytes stay intact.
	state.Unavailable, state.opaque = true, cloneRawJSON(data)
	return &state
}

// Strict decode keeps an unsupported extension opaque instead of erasing its
// descriptors on a later ordinary write. No decoder performs I/O or migration.
func decodeContentWire(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ErrContentIdentityUnavailable
	}
	return nil
}

func contentStateKey(state *contentIdentityState) string {
	if state.Pending != nil && state.Pending.Mutation != nil {
		return state.Pending.Mutation.Transition.CanonicalKey
	}
	if state.Last != nil && state.Last.Mutation != nil {
		return state.Last.Mutation.Transition.CanonicalKey
	}
	return ""
}

func validContentArtifact(file contentArtifactDescriptor, key, domain, generation, kind string) bool {
	return file.Version == 1 && file.Key == key && file.DomainID == domain && file.Generation == generation &&
		file.Kind == kind && validHistoryStreamID(file.ID) && file.Bytes >= 0 && file.Bytes < 1<<62 && validHistoryDigest(file.Digest)
}

func validContentStorageHeader(state *contentIdentityState, key string) bool {
	if state == nil || state.storage == nil {
		return false
	}
	h := state.storage.Header
	if h.Version != contentStorageVersion || h.Key != key || !validHistoryStreamID(h.Generation) ||
		!validHistoryStreamID(state.DomainID) || !validHistoryStreamID(state.StateToken) ||
		!validContentArtifact(
			h.Index.File,
			key,
			state.DomainID,
			h.Generation,
			"rows",
		) || h.Index.Algorithm != contentIndexAlgorithm ||
		h.Index.Records < 0 || int64(h.Index.Records) > h.Index.File.Bytes ||
		h.Index.Start < 0 || h.Index.Count < 0 || h.Index.Start > h.Index.Records || h.Index.Count > h.Index.Records-h.Index.Start {
		return false
	}
	for _, entry := range []struct {
		slot *contentSlotDescriptor
		kind string
	}{{h.Pending, "pending"}, {h.Last, "last"}} {
		if entry.slot == nil {
			continue
		}
		slot := entry.slot
		if !validContentArtifact(slot.File, key, state.DomainID, h.Generation, entry.kind) ||
			!validHistoryDigest(
				slot.PlanDigest,
			) || !validHistoryStreamID(slot.BeforeState) || !validHistoryStreamID(slot.AfterState) ||
			slot.BeforeState == slot.AfterState || uuid.Validate(slot.MutationID) != nil || slot.ExpiresAt.IsZero() {
			return false
		}
		if slot.Typed && (!validContentMutationKind(slot.Kind) || !validHistoryDigest(slot.Intent)) {
			return false
		}
		if slot.Typed && state.MutationVersion != contentMutationVersion {
			return false
		}
	}
	if h.Pending != nil && h.Pending.Typed && h.BeforeImage == nil {
		return false
	}
	if h.BeforeImage != nil &&
		(h.Pending == nil || !h.Pending.Typed || !h.BeforeImage.valid(key, state.DomainID, h.Pending.MutationID)) {
		return false
	}
	if h.RetiredImage != nil &&
		(h.Last == nil || !h.Last.Typed || !h.RetiredImage.valid(key, state.DomainID, h.Last.MutationID)) {
		return false
	}
	return true
}

func (state *contentIdentityState) restoreContentCapsules() {
	h := state.storage.Header
	state.Pending, state.Last = nil, nil
	if d := h.Pending; d != nil {
		if d.Typed {
			t := contentCapsuleTransition(state, d, h.Key)
			state.Pending = &contentPending{Mutation: &contentMutationPending{
				Transition: t,
				PlanDigest: d.PlanDigest,
				Before: contentSnapshot{
					StateToken: d.BeforeState,
					Image:      h.BeforeImage,
				},
				After: contentSnapshot{StateToken: d.AfterState},
			}}
		} else {
			state.Pending = &contentPending{
				Transition: contentCapsuleCompaction(state, d, h.Key),
				PlanDigest: d.PlanDigest,
			}
		}
	}
	if d := h.Last; d != nil {
		if d.Typed {
			t := contentCapsuleTransition(state, d, h.Key)
			state.Last = &contentAcknowledged{Mutation: &contentMutationAcknowledged{
				Transition: t, PlanDigest: d.PlanDigest,
				Ack: ContentMutationAppliedAck{
					DomainID: state.DomainID, MutationID: d.MutationID, IntentDigest: d.Intent, Kind: d.Kind,
					BeforeState: d.BeforeState, AfterState: d.AfterState, AppliedCommitID: d.CommitID,
				}, AckDigest: d.AckDigest, Retired: h.RetiredImage,
			}}
		} else {
			state.Last = &contentAcknowledged{
				Transition: contentCapsuleCompaction(state, d, h.Key),
				PlanDigest: d.PlanDigest,
				AckDigest:  d.AckDigest,
			}
		}
	}
}

func contentCapsuleTransition(
	state *contentIdentityState,
	d *contentSlotDescriptor,
	key string,
) ContentMutationTransition {
	return ContentMutationTransition{
		Version:      ContentLineageVersion,
		CanonicalKey: key,
		DomainID:     state.DomainID,
		MutationID:   d.MutationID,
		IntentDigest: d.Intent,
		Kind:         d.Kind,
		BeforeState:  d.BeforeState,
		AfterState:   d.AfterState,
		ExpiresAt:    d.ExpiresAt,
	}
}

func contentCapsuleCompaction(state *contentIdentityState, d *contentSlotDescriptor, key string) ContentTransition {
	return ContentTransition{
		Version: ContentLineageVersion, CanonicalKey: key, DomainID: state.DomainID,
		MutationID: d.MutationID, BeforeState: d.BeforeState, AfterState: d.AfterState, ExpiresAt: d.ExpiresAt,
	}
}

func cloneContentStorage(storage *contentStorageState) *contentStorageState {
	if storage == nil {
		return nil
	}
	clone := *storage
	data, _ := json.Marshal(storage.Header)
	_ = json.Unmarshal(data, &clone.Header)
	clone.Appended = append([]contentLocator(nil), storage.Appended...)
	return &clone
}

func contentMetaForCache(meta SessionMeta) SessionMeta {
	state := meta.ContentIdentity
	if state == nil || state.storage == nil || len(state.opaque) != 0 {
		return copyMeta(meta)
	}
	meta.ContentIdentity = &contentIdentityState{
		Version:         state.Version,
		DomainID:        state.DomainID,
		StateToken:      state.StateToken,
		SummaryDigest:   state.SummaryDigest,
		Skip:            state.Skip,
		Count:           state.Count,
		Unavailable:     state.Unavailable,
		MutationVersion: state.MutationVersion,
		storage:         &contentStorageState{Header: cloneContentStorage(state.storage).Header},
	}
	meta.ContentIdentity.restoreContentCapsules()
	meta.Scope = cloneRawJSON(meta.Scope)
	meta.Aliases = append([]string(nil), meta.Aliases...)
	if meta.HistoryBytes != nil {
		size := *meta.HistoryBytes
		meta.HistoryBytes = &size
	}
	return meta
}

func (file contentArtifactDescriptor) name() string {
	suffix := ".blob"
	if file.Kind == "rows" {
		suffix = ".idx"
	}
	return fmt.Sprintf(".pc-history-%s-%s-%s%s", contentDigest([]byte(file.Key)), file.Kind, file.ID, suffix)
}

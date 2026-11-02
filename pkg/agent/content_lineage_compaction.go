package agent

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// A contentCompactionView is captured before any summary work. Only a source
// explicitly initialized by the authenticated consumer opts into retention.
// Implementing the optional interface alone never enables that protocol.
type contentCompactionView struct {
	history []providers.Message
	summary string
	frame   *memory.ContentFrame
	store   memory.HistoryContentLineage
	resolve func(string) string
}

func readContentCompactionView(ctx context.Context, agent *AgentInstance, key string) (contentCompactionView, error) {
	if err := ctx.Err(); err != nil {
		return contentCompactionView{}, err
	}
	api, ok := agent.Sessions.(memory.HistoryContentLineage)
	if !ok {
		return contentCompactionView{
			history: agent.Sessions.GetHistory(key),
			summary: agent.Sessions.GetSummary(key),
		}, nil
	}
	activation, available := agent.Sessions.(memory.HistoryContentActivation)
	if !available {
		return contentCompactionView{
			history: agent.Sessions.GetHistory(key),
			summary: agent.Sessions.GetSummary(key),
		}, nil
	}
	canonical := key
	var resolve func(string) string
	if resolver, supported := agent.Sessions.(interface{ ResolveSessionKey(key string) string }); supported {
		resolve = resolver.ResolveSessionKey
		canonical = resolve(key)
	}
	active, err := activation.HistoryContentIdentityActive(ctx, canonical)
	if errors.Is(err, memory.ErrHistoryUnsupported) || (err == nil && !active) {
		return contentCompactionView{
			history: agent.Sessions.GetHistory(key),
			summary: agent.Sessions.GetSummary(key),
		}, nil
	}
	if err != nil {
		return contentCompactionView{}, err
	}
	pending, err := api.ReadPendingHistoryCompaction(ctx, canonical)
	if err != nil {
		return contentCompactionView{}, err
	}
	if pending.MutationID != "" {
		// Capture cannot accept another compaction until this exact preimage
		// is archived. Do not spend provider calls on a plan that is already
		// known to be uncommittable. Strict commit CAS still handles races.
		return contentCompactionView{}, memory.ErrContentLineagePending
	}
	frame, err := api.ReadHistoryContentFrame(ctx, canonical)
	if err != nil {
		return contentCompactionView{}, err
	}
	if frame.IdentityStatus != memory.ContentIdentityKnown {
		return contentCompactionView{}, memory.ErrContentIdentityUnavailable
	}
	view := contentCompactionView{summary: frame.Summary, history: make([]providers.Message, len(frame.Entries))}
	for i, entry := range frame.Entries {
		view.history[i] = entry.Message
	}
	if frame.IdentityStatus == memory.ContentIdentityKnown {
		view.frame, view.store, view.resolve = &frame, api, resolve
	}
	return view, nil
}

func (view contentCompactionView) commit(
	ctx context.Context,
	key, summary string,
	cut int,
) (*memory.ContentTransition, error) {
	if view.frame == nil || cut < 0 || cut > len(view.frame.Entries) {
		return nil, memory.ErrContentStateChanged
	}
	if view.resolve != nil && view.resolve(key) != view.frame.CanonicalKey {
		return nil, memory.ErrContentStateChanged
	}
	refs := make([]memory.ContentRef, len(view.frame.Entries))
	for i, entry := range view.frame.Entries {
		if entry.Ref == nil {
			return nil, memory.ErrContentIdentityUnavailable
		}
		refs[i] = *entry.Ref
	}
	transition, err := view.store.CommitHistoryCompaction(ctx, view.frame.CanonicalKey, memory.ContentCompactionPlan{
		MutationID: uuid.NewString(), DomainID: view.frame.DomainID,
		BeforeState: view.frame.StateToken, Summary: summary,
		SummaryInputs: refs[:cut], Retained: refs[cut:],
	})
	if err != nil {
		return nil, err
	}
	return &transition, nil
}

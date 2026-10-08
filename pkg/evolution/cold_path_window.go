package evolution

import (
	"errors"
	"fmt"
	"sort"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// ColdPathTaskWindow is how many task records a cold-path run considers: the
// most recent eligible ones. Older unclustered records expire.
//
// Without it the run took every record still "new", and a record only stopped
// being new when it landed in a pattern. One-off tasks never do, so the input
// grew with the assistant's whole history. Production stores reached thousands
// of records and 560k-token clustering calls; past ~80k input tokens the model
// spends its whole output budget reasoning and answers empty 82–97% of the
// time, so nothing clusters and the next call is larger still. Thirty records
// with the 400-rune excerpt keep the call under ~10k tokens, where that happens
// 0.4% of the time. A pattern still forms from tasks that recur within the
// window.
const ColdPathTaskWindow = 30

const recordStatusExpired = RecordStatus("expired")

// ErrNoCredit means the gateway refused a cold-path call because the account
// has no credit left. The cold path stops instead of falling back to its
// heuristics: a verdict or a cluster made only because the model was out of
// reach would be persisted as if the model had decided it, and retrying every
// turn just repeats the refusal.
var ErrNoCredit = errors.New("evolution: account has no credit")

func noCreditError(err error) error {
	return fmt.Errorf("%w: %w", ErrNoCredit, err)
}

func isNoCreditError(err error) bool {
	return errors.Is(err, ErrNoCredit) || providers.IsInsufficientCreditError(err)
}

// splitColdPathWindow returns the workspace's eligible task records that fall
// in the window, in their stored order, and the ids of the unprocessed records
// older than the window, which are to expire.
func splitColdPathWindow(workspace string, records []LearningRecord) ([]LearningRecord, []string) {
	eligible := make([]int, 0)
	for i, record := range records {
		if !isTaskRecordKind(record.Kind) || record.WorkspaceID != workspace {
			continue
		}
		if coldPathEvidenceRejectReason(record) != "" {
			continue
		}
		eligible = append(eligible, i)
	}
	if len(eligible) <= ColdPathTaskWindow {
		window := make([]LearningRecord, 0, len(eligible))
		for _, i := range eligible {
			window = append(window, records[i])
		}
		return window, nil
	}

	// Newest first; on equal timestamps the later line in the file is newer.
	byRecency := append([]int(nil), eligible...)
	sort.SliceStable(byRecency, func(a, b int) bool {
		left, right := records[byRecency[a]], records[byRecency[b]]
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.After(right.CreatedAt)
		}
		return byRecency[a] > byRecency[b]
	})
	inWindow := make(map[int]struct{}, ColdPathTaskWindow)
	for _, i := range byRecency[:ColdPathTaskWindow] {
		inWindow[i] = struct{}{}
	}
	cutoff := records[byRecency[ColdPathTaskWindow-1]].CreatedAt

	window := make([]LearningRecord, 0, ColdPathTaskWindow)
	expired := make([]string, 0)
	for i, record := range records {
		if _, ok := inWindow[i]; ok {
			window = append(window, record)
			continue
		}
		if !isTaskRecordKind(record.Kind) || record.WorkspaceID != workspace {
			continue
		}
		if record.Status != "" && record.Status != RecordStatus("new") {
			continue
		}
		if record.CreatedAt.After(cutoff) {
			continue
		}
		expired = append(expired, record.ID)
	}
	return window, expired
}

package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/google/uuid"
)

type claimedContentProofError struct{}

func (claimedContentProofError) Error() string { return "opaque operational cause claiming proof" }
func (claimedContentProofError) Is(target error) bool {
	return target == ErrContentIdentityUnavailable
}

func TestContentLineageGCNoopAcceptsOnlyIdentityProofLeaves(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"exact", ErrContentIdentityUnavailable, true},
		{"wrapped", fmt.Errorf("identity: %w", ErrContentIdentityUnavailable), true},
		{"pure-join", errors.Join(ErrContentIdentityUnavailable, ErrContentIdentityUnavailable), true},
		{"io", io.ErrClosedPipe, false},
		{"mixed-io", errors.Join(ErrContentIdentityUnavailable, io.ErrClosedPipe), false},
		{"nested-io", fmt.Errorf("save: %w", errors.Join(ErrContentIdentityUnavailable, os.ErrPermission)), false},
		{"joined-wrapper", fmt.Errorf("%w: %w", ErrContentIdentityUnavailable, io.ErrUnexpectedEOF), false},
		{"journal", errors.Join(ErrContentIdentityUnavailable, ErrHistoryRecoveryRequired), false},
		{"owner", errors.Join(ErrContentIdentityUnavailable, ErrHistoryOwnershipUnavailable), false},
		{"canceled", errors.Join(ErrContentIdentityUnavailable, context.Canceled), false},
		{"deadline", errors.Join(ErrContentIdentityUnavailable, context.DeadlineExceeded), false},
		{"path-wrapper", &os.PathError{Op: "read", Path: "metadata", Err: ErrContentIdentityUnavailable}, false},
		{"opaque-Is", claimedContentProofError{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := onlyContentIdentityUnavailable(test.err); got != test.want {
				t.Fatal("GC no-op misclassified an operational or proof cause", got, test.want, test.err)
			}
		})
	}
}

func TestContentLineageGCNoopPreservesOperationalAndOwnershipErrors(t *testing.T) {
	for _, failure := range []string{"source-io", "own-meta-io", "journal", "alias-owner", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "agent:gc-noop-negative"
			_, _, pending := pendingContent(t, s, key)
			if err = s.AcknowledgeHistoryCompaction(t.Context(), key, ContentArchiveAck{
				DomainID: pending.DomainID, MutationID: pending.MutationID,
				BeforeState: pending.BeforeState, AfterState: pending.AfterState,
				Archived: pending.Dropped, ArchiveCommitID: uuid.NewString(),
			}); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			checkPath := s.jsonlPath(key)
			switch failure {
			case "source-io":
				checkPath += ".private-test-backup"
				if err = os.Rename(s.jsonlPath(key), checkPath); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(s.jsonlPath(key), 0o700); err != nil {
					t.Fatal(err)
				}
			case "own-meta-io":
				if err = os.Remove(s.metaPath(key)); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(s.metaPath(key), 0o700); err != nil {
					t.Fatal(err)
				}
			case "journal":
				if err = os.WriteFile(s.historyRecoveryPath(key), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "alias-owner":
				meta, readErr := s.GetSessionMeta(t.Context(), key)
				if readErr != nil {
					t.Fatal(readErr)
				}
				meta.ContentIdentity.Unavailable = true
				data, encodeErr := json.Marshal(meta)
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				if err = os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(s.metaPath("unknown-owner"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.CompactForSessionSave(ctx, key); err == nil {
				t.Fatal("optional GC swallowed a real operational/ownership failure", failure)
			}
			after, readErr := os.ReadFile(checkPath)
			if readErr != nil || !bytes.Equal(original, after) {
				t.Fatal("refused GC changed durable content", failure, readErr)
			}
		})
	}
}

func TestContentLineageExplicitCompactStillReportsUnavailableProof(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_explicit_vs_session_save"
	_, _, pending := pendingContent(t, s, key)
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, ContentArchiveAck{
		DomainID: pending.DomainID, MutationID: pending.MutationID,
		BeforeState: pending.BeforeState, AfterState: pending.AfterState,
		Archived: pending.Dropped, ArchiveCommitID: uuid.NewString(),
	}); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(s.contentDomainPath(key)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Compact(t.Context(), key); !errors.Is(err, ErrContentIdentityUnavailable) {
		t.Fatal("explicit compaction hid its proof refusal", err)
	}
	if err = s.CompactForSessionSave(t.Context(), key); err != nil {
		t.Fatal("optional session-save maintenance invalidated durable content", err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("proof-unavailable maintenance changed physical source", err)
	}
}

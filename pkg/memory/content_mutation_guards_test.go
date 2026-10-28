package memory

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentMutationAcceptedIDAlwaysSupportsRecoveryLookup(t *testing.T) {
	canonical := uuid.NewString()
	for _, id := range []string{
		canonical, strings.ToUpper(canonical), strings.ReplaceAll(canonical, "-", ""),
		"urn:uuid:" + canonical, "{" + canonical + "}", uuid.Nil.String(),
	} {
		t.Run(id, func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			plan := typedPrefixPlan(frame, 2)
			plan.MutationID = id
			result, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
			if err != nil {
				t.Fatal("public mutation ID acceptance changed", err)
			}
			found, err := s.ReadHistoryContentMutation(t.Context(), key, id, plan.IntentDigest)
			if err != nil || !reflect.DeepEqual(found, result) {
				t.Fatal("accepted NEW could not be recovered by its exact command ID", found, err)
			}
			if err = s.AcknowledgeHistoryContentMutation(
				t.Context(),
				key,
				typedAppliedAck(result.Transition),
			); err != nil {
				t.Fatal(err)
			}
			found, err = s.ReadHistoryContentMutation(t.Context(), key, id, plan.IntentDigest)
			if err != nil || found.Status != ContentMutationApplied {
				t.Fatal("accepted ID lost closed application replay", found, err)
			}
		})
	}
}

func TestContentMutationPendingPriorityAndBoundedLast(t *testing.T) {
	s, key, frame := typedMutationSource(t)
	planA := typedPrefixPlan(frame, 4)
	a, err := s.CommitHistoryContentMutation(t.Context(), key, planA)
	if err != nil {
		t.Fatal(err)
	}
	ackA := typedAppliedAck(a.Transition)
	if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, ackA); err != nil {
		t.Fatal(err)
	}
	current, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(current, 2))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadHistoryContentMutation(
		t.Context(),
		key,
		planA.MutationID,
		planA.IntentDigest,
	); !errors.Is(
		err,
		ErrContentLineagePending,
	) {
		t.Fatal("Last replay bypassed another Pending", err)
	}
	if _, err = s.CommitHistoryContentMutation(t.Context(), key, planA); !errors.Is(err, ErrContentLineagePending) {
		t.Fatal("old Last ID was reexecuted across another Pending", err)
	}
	if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, ackA); !errors.Is(err, ErrContentMutationAck) {
		t.Fatal("old application ACK released another operation", err)
	}
	if _, err = s.ReadPendingHistoryCompaction(t.Context(), key); !errors.Is(err, ErrContentMutationPendingKind) {
		t.Fatal("typed Pending looked like no legacy compaction", err)
	}
	if _, err = s.CommitHistoryCompaction(t.Context(), key, ContentCompactionPlan{
		DomainID: b.Transition.DomainID, MutationID: uuid.NewString(), BeforeState: b.Transition.AfterState,
	}); !errors.Is(err, ErrContentMutationPendingKind) {
		t.Fatal("legacy compaction bypassed a prefix mutation", err)
	}
	after, err := os.ReadFile(s.metaPath(key))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected replay/ACK mutated Pending", err)
	}
	if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, typedAppliedAck(b.Transition)); err != nil {
		t.Fatal(err)
	}
	missing, err := s.ReadHistoryContentMutation(t.Context(), key, planA.MutationID, planA.IntentDigest)
	if err != nil || missing.Transition.MutationID != "" {
		t.Fatal("bounded Last became an operation ledger", missing, err)
	}
	if _, err = s.CommitHistoryContentMutation(t.Context(), key, planA); !errors.Is(err, ErrContentStateChanged) {
		t.Fatal("original stale plan was reapplied after leaving Last", err)
	}
	latest, err := s.ReadLatestHistoryContentMutation(t.Context(), key)
	if err != nil || latest.Transition.MutationID != b.Transition.MutationID ||
		latest.Status != ContentMutationApplied {
		t.Fatal("wrong bounded latest operation", latest, err)
	}
}

func TestContentMutationExactApplicationEvidenceAndIDCollision(t *testing.T) {
	for _, fault := range []string{"kind", "intent", "beforeState", "afterState", "omittedBefore", "reorderedBefore", "revisionAfter", "emptyCommit"} {
		t.Run(fault, func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			plan := typedPrefixPlan(frame, 2)
			result, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.metaPath(key))
			if err != nil {
				t.Fatal(err)
			}
			ack := typedAppliedAck(result.Transition)
			switch fault {
			case "kind":
				ack.Kind = ContentMutationEdit
			case "intent":
				ack.IntentDigest = contentDigest([]byte("foreign command"))
			case "beforeState":
				ack.BeforeState = uuid.NewString()
			case "afterState":
				ack.AfterState = uuid.NewString()
			case "omittedBefore":
				ack.Before = ack.Before[:len(ack.Before)-1]
			case "reorderedBefore":
				ack.Before[0], ack.Before[1] = ack.Before[1], ack.Before[0]
			case "revisionAfter":
				ack.After[0].RevisionID = uuid.NewString()
			case "emptyCommit":
				ack.AppliedCommitID = ""
			}
			if err = s.AcknowledgeHistoryContentMutation(
				t.Context(),
				key,
				ack,
			); !errors.Is(
				err,
				ErrContentMutationAck,
			) {
				t.Fatal("foreign application evidence released a preimage", err)
			}
			after, err := os.ReadFile(s.metaPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected ACK partially changed the operation", err)
			}
			changed := plan
			changed.IntentDigest = contentDigest([]byte("different admitted request"))
			if _, err = s.CommitHistoryContentMutation(
				t.Context(),
				key,
				changed,
			); !errors.Is(
				err,
				ErrContentStateChanged,
			) {
				t.Fatal("same ID accepted another intent", err)
			}
			changed = plan
			changed.RetainPrefix = &ContentMutationRetainPrefixIntent{Retained: frameRefs(frame)[:1]}
			if _, err = s.CommitHistoryContentMutation(
				t.Context(),
				key,
				changed,
			); !errors.Is(
				err,
				ErrContentStateChanged,
			) {
				t.Fatal("same ID accepted another plan", err)
			}
		})
	}
}

func TestContentMutationCheckpointRejectsMetadataOnlyRollback(t *testing.T) {
	for _, operation := range []string{"application_ack", "summary", "logical_compaction"} {
		t.Run(operation, func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			result, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 4))
			if err != nil {
				t.Fatal(err)
			}
			if operation != "application_ack" {
				if err = s.AcknowledgeHistoryContentMutation(
					t.Context(),
					key,
					typedAppliedAck(result.Transition),
				); err != nil {
					t.Fatal(err)
				}
			}
			prior, err := os.ReadFile(s.metaPath(key))
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "application_ack":
				err = s.AcknowledgeHistoryContentMutation(t.Context(), key, typedAppliedAck(result.Transition))
			case "summary":
				err = s.SetSummary(t.Context(), key, "later independent summary")
			case "logical_compaction":
				current, readErr := s.ReadHistoryContentFrame(t.Context(), key)
				if readErr != nil {
					t.Fatal(readErr)
				}
				refs := frameRefs(current)
				_, err = s.CommitHistoryCompaction(t.Context(), key, ContentCompactionPlan{
					MutationID: uuid.NewString(), DomainID: current.DomainID, BeforeState: current.StateToken,
					SummaryInputs: refs[:1], Retained: refs[1:], Summary: "logical cut",
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			bodyAfter, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(body, bodyAfter) {
				t.Fatal("fixture did not isolate metadata-only mutation", err)
			}
			witness, err := os.ReadFile(s.contentDomainPath(key))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(s.metaPath(key), prior, 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || current.IdentityStatus != ContentIdentityUnavailable || len(current.Entries) != 4 {
				t.Fatal("rolled-back meta forgot a mutation while JSONL stayed equal", current, err)
			}
			if _, err = s.ReadLatestHistoryContentMutation(
				t.Context(),
				key,
			); !errors.Is(
				err,
				ErrContentIdentityUnavailable,
			) {
				t.Fatal("replay adopted a stale checkpoint", err)
			}
			metaAfter, err := os.ReadFile(s.metaPath(key))
			witnessAfter, witnessErr := os.ReadFile(s.contentDomainPath(key))
			if err != nil || witnessErr != nil || !bytes.Equal(prior, metaAfter) ||
				!bytes.Equal(witness, witnessAfter) {
				t.Fatal("getter repaired a rollback", err, witnessErr)
			}
			appended, err := s.AppendHistoryContent(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "durable without reattestation"},
			)
			if err != nil || appended.Entry.Ref != nil || appended.Receipt != nil ||
				appended.Entry.ReceiptBinding != nil || appended.Entry.Message.Content == "" {
				t.Fatal("rollback blocked ordinary content or invented proof", appended, err)
			}
			messages, err := s.GetHistory(t.Context(), key)
			if err != nil || len(messages) != 5 || messages[4].Content != "durable without reattestation" {
				t.Fatal("durable content was lost after proof closed", messages, err)
			}
		})
	}
}

func TestContentMutationOwnerDeleteRetiresTypedRootsAndPreservesUnownedFiles(t *testing.T) {
	s, key, frame := typedMutationSource(t)
	result, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 2))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	preimage := meta.ContentIdentity.Pending.Mutation.Before.Image
	// Filename similarity and a valid-looking UUID do not establish ownership.
	orphan := filepath.Join(s.dir, contentPreimageName(key, uuid.NewString()))
	unique := []byte("unowned evidence must survive")
	if err = os.WriteFile(orphan, unique, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteSession(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.metaPath(key), s.jsonlPath(key), s.contentDomainPath(key), filepath.Join(s.dir, contentPreimageName(key, preimage.ID))} {
		if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("owner DELETE left a rooted typed artifact", path, err)
		}
	}
	kept, err := os.ReadFile(orphan)
	if err != nil || !bytes.Equal(kept, unique) {
		t.Fatal("owner DELETE globbed unrelated evidence", err)
	}
	if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, typedAppliedAck(result.Transition)); err == nil {
		t.Fatal("application ACK resurrected an owner-deleted domain")
	}
}

func TestContentMutationOwnerDeleteRefusesUnsafePreimageBeforeClearingBody(t *testing.T) {
	for _, fault := range []string{"changed", "symlink", "directory", "hardlink"} {
		t.Run(fault, func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			if _, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 2)); err != nil {
				t.Fatal(err)
			}
			meta, err := s.readRecoveryMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			image := meta.ContentIdentity.Pending.Mutation.Before.Image
			path := filepath.Join(s.dir, contentPreimageName(key, image.ID))
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "changed":
				err = os.WriteFile(path, []byte("foreign bytes"), 0o600)
			case "symlink", "hardlink":
				other := filepath.Join(t.TempDir(), "unique")
				if err = os.WriteFile(other, saved, 0o600); err != nil {
					t.Fatal(err)
				}
				if fault == "symlink" {
					err = os.Symlink(other, path)
				} else {
					err = os.Link(other, path)
				}
			case "directory":
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := make(map[string][]byte)
			for _, source := range []string{s.jsonlPath(key), s.metaPath(key), s.contentDomainPath(key)} {
				before[source], err = os.ReadFile(source)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = s.DeleteSession(t.Context(), key); err == nil {
				t.Fatal("unsafe/unowned artifact was followed or deleted")
			}
			for source, want := range before {
				got, readErr := os.ReadFile(source)
				if readErr != nil || !reflect.DeepEqual(got, want) {
					t.Fatal("refused Delete partially cleared content/summary/witness", source, readErr)
				}
			}
		})
	}
}

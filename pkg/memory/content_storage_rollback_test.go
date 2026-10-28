package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

type contentRollbackExpectation struct {
	Key              string                    `json:"key"`
	Frame            ContentFrame              `json:"frame"`
	Plan             ContentMutationPlan       `json:"plan"`
	Result           ContentMutationResult     `json:"result"`
	Ack              ContentMutationAppliedAck `json:"ack"`
	Sequence         uint64                    `json:"sequence"`
	Stream           string                    `json:"stream"`
	RecoveryRequired bool                      `json:"recoveryRequired"`
}

func sameContentFrameAcrossFormat(before, after ContentFrame) bool {
	// Observation fingerprints the physical metadata representation. A format
	// conversion changes it while preserving the logical CAS token and refs.
	before.Observation, after.Observation = "", ""
	return reflect.DeepEqual(before, after)
}

func TestContentStorageInlineRollbackPreservesAllSemanticState(t *testing.T) {
	for _, applied := range []bool{false, true} {
		for _, lostOrder := range []bool{false, true} {
			name := map[bool]string{false: "pending", true: "applied"}[applied] + map[bool]string{false: "-healthy", true: "-order-lost"}[lostOrder]
			t.Run(name, func(t *testing.T) {
				s, key, before := typedMutationSource(t)
				plan := typedPrefixPlan(before, 2)
				result, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
				if err != nil {
					t.Fatal(err)
				}
				ack := typedAppliedAck(result.Transition)
				if applied {
					if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, ack); err != nil {
						t.Fatal(err)
					}
					result, err = s.ReadLatestHistoryContentMutation(t.Context(), key)
					if err != nil {
						t.Fatal(err)
					}
				}
				if lostOrder {
					if err = os.Remove(s.historyOrderWitnessPath(key)); err != nil {
						t.Fatal(err)
					}
					s, err = NewJSONLStore(s.dir)
					if err != nil {
						t.Fatal(err)
					}
				}
				frame, err := s.ReadHistoryContentFrame(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				meta, err := s.readMeta(key)
				if err != nil {
					t.Fatal(err)
				}
				files := contentStorageRoots(meta.ContentIdentity)
				body, err := os.ReadFile(s.jsonlPath(key))
				if err != nil {
					t.Fatal(err)
				}
				if err = s.PrepareHistoryContentInlineRollback(t.Context(), key); err != nil {
					t.Fatal(err)
				}
				after, err := s.ReadHistoryContentFrame(t.Context(), key)
				if err != nil || !sameContentFrameAcrossFormat(frame, after) {
					t.Fatal("conversion changed logical content or live bindings", frame, after, err)
				}
				afterMeta, err := s.readRecoveryMeta(key)
				if err != nil || afterMeta.ContentIdentity.storage != nil || afterMeta.ContentIdentity.MutationVersion != contentMutationVersion ||
					afterMeta.HistorySequence != meta.HistorySequence || afterMeta.HistoryStreamID != meta.HistoryStreamID ||
					afterMeta.Summary != meta.Summary || afterMeta.Count != meta.Count || afterMeta.Skip != meta.Skip {
					t.Fatal("conversion reset semantic/Order state", afterMeta, err)
				}
				bodyAfter, err := os.ReadFile(s.jsonlPath(key))
				if err != nil || !bytes.Equal(body, bodyAfter) {
					t.Fatal("conversion rewrote raw source", err)
				}
				for _, file := range files {
					if _, err = os.Lstat(filepath.Join(s.dir, file.name())); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("converted representation was not safely retired", file.Kind, err)
					}
				}
				got, err := s.ReadLatestHistoryContentMutation(t.Context(), key)
				if err != nil || !reflect.DeepEqual(result, got) {
					t.Fatal("conversion lost Pending/Last replay", result, got, err)
				}
				if root := os.Getenv("CONTENT_STORAGE_ROLLBACK_EXPORT_DIR"); root != "" {
					if !filepath.IsAbs(root) {
						t.Fatal("rollback fixture root must be absolute")
					}
					dst := filepath.Join(root, name)
					if err = os.Mkdir(dst, 0o700); err != nil {
						t.Fatal(err)
					}
					entries, readErr := os.ReadDir(s.dir)
					if readErr != nil {
						t.Fatal(readErr)
					}
					for _, entry := range entries {
						if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
							t.Fatal("unexpected fixture entry")
						}
						data, readErr := os.ReadFile(filepath.Join(s.dir, entry.Name()))
						if readErr != nil {
							t.Fatal(readErr)
						}
						if err = os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					expected := contentRollbackExpectation{key, after, plan, result, ack, afterMeta.HistorySequence, afterMeta.HistoryStreamID, afterMeta.HistoryRecoveryRequired}
					data, _ := json.MarshalIndent(expected, "", "  ")
					if err = os.WriteFile(filepath.Join(dst, "expected.fixture"), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestContentStorageInlineRollbackOLDNEWKeepsOneAuthoritativeFormat(t *testing.T) {
	for _, newDecision := range []bool{false, true} {
		t.Run(map[bool]string{false: "OLD", true: "NEW"}[newDecision], func(t *testing.T) {
			s, key, before := typedMutationSource(t)
			if _, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(before, 2)); err != nil {
				t.Fatal(err)
			}
			frame, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path == s.metaPath(key) {
					if newDecision {
						if err := fileutil.WriteFileAtomic(path, data, mode); err != nil {
							return err
						}
					}
					return errors.New("lost conversion metadata reply")
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			err = s.PrepareHistoryContentInlineRollback(t.Context(), key)
			if newDecision && err != nil {
				t.Fatal("NEW conversion was retried as another operation", err)
			}
			if !newDecision && err == nil {
				t.Fatal("OLD conversion reported success")
			}
			s.writeFile = nil
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := s.readRecoveryMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			if (meta.ContentIdentity.storage == nil) != newDecision {
				t.Fatal("source mixed format decisions")
			}
			after, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || !sameContentFrameAcrossFormat(frame, after) {
				t.Fatal("format recovery changed the content state", frame, after, err)
			}
		})
	}
}

func TestContentStorageInlineRollbackRefusesMissingIdentityArtifact(t *testing.T) {
	s, key, _ := typedMutationSource(t)
	meta, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(s.dir, meta.ContentIdentity.storage.Header.Index.File.name())); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	floor, err := os.ReadFile(s.contentDomainPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PrepareHistoryContentInlineRollback(t.Context(), key); !errors.Is(err, ErrContentIdentityUnavailable) {
		t.Fatal("downconversion invented missing proof", err)
	}
	bodyAfter, _ := os.ReadFile(s.jsonlPath(key))
	floorAfter, _ := os.ReadFile(s.contentDomainPath(key))
	if !bytes.Equal(body, bodyAfter) || !bytes.Equal(floor, floorAfter) {
		t.Fatal("refused conversion changed source/floor")
	}
}

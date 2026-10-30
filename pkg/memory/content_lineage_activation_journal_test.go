package memory

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

func TestContentLineageActivationRejectsUnsettledNEWWithoutRecovery(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(map[bool]string{false: "warm", true: "cold"}[reopen], func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_pending_activation"
			for range 5 {
				if err = s.AddMessage(t.Context(), key, "user", "durable"); err != nil {
					t.Fatal(err)
				}
			}
			oldMeta, err := os.ReadFile(s.metaPath(key))
			if err != nil {
				t.Fatal(err)
			}
			frame, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			var confirmedNew []byte
			sentinel := errors.New("metadata unavailable after owning durable NEW decision")
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path == s.metaPath(key) && confirmedNew == nil {
					confirmedNew = bytes.Clone(data)
					if e := fileutil.WriteFileAtomic(path, data, mode); e != nil {
						return e
					}
					journal, e := os.ReadFile(s.historyRecoveryPath(key))
					if e != nil {
						return e
					}
					if e = s.recordHistoryDecision(key, journal, historyWriteNew); e != nil {
						return e
					}
					if e = fileutil.WriteFileAtomic(path, oldMeta, mode); e != nil {
						return e
					}
					return sentinel
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			if _, err = s.InitializeHistoryContentIdentity(t.Context(), key, frame.Observation); err == nil {
				t.Fatal("fixture did not leave its own NEW journal unresolved")
			}
			s.writeFile = nil
			if reopen {
				s, err = NewJSONLStore(s.dir)
				if err != nil {
					t.Fatal(err)
				}
			}
			paths := []string{s.jsonlPath(key), s.metaPath(key), s.historyRecoveryPath(key), s.historyDecisionPath(key)}
			before := map[string][]byte{}
			for _, path := range paths {
				data, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				before[path] = data
			}
			active, probeErr := s.HistoryContentIdentityActive(t.Context(), key)
			if !errors.Is(probeErr, ErrHistoryRecoveryRequired) || active {
				t.Error("probe claimed absence despite an unresolved owning NEW journal", active, probeErr)
			}
			for _, path := range paths {
				after, e := os.ReadFile(path)
				if e != nil || !bytes.Equal(before[path], after) {
					t.Fatal("activation probe recovered or changed its journal/source", path, e)
				}
			}
			if err = fileutil.WriteFileAtomic(s.metaPath(key), confirmedNew, 0o600); err != nil {
				t.Fatal(err)
			}
			if err = s.AddMessage(t.Context(), key, "assistant", "writer recovery remains available"); err != nil {
				t.Fatal("ordinary writer could not finish its proven journal", err)
			}
			active, err = s.HistoryContentIdentityActive(t.Context(), key)
			if err != nil || !active {
				t.Fatal("writer recovery failed to finish its initialized source", active, err)
			}
		})
	}
}

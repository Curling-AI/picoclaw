package memory

import (
	"errors"
	"log"
	"os"
)

// A typed transition carries content only. Pure order loss closes receipts
// durably without undoing a decided content write. Operational causes, ownership
// and mixed errors are not optional proof loss and stay explicit.
func onlyContentMutationOrderLoss(err error) bool {
	if err == ErrHistoryReceiptUnavailable || err == ErrHistoryOrderUnavailable || err == ErrHistoryRecoveryRequired ||
		err == ErrHistoryCursor {
		return true
	}
	if err == nil || err == ErrHistoryOwnershipUnavailable {
		return false
	}
	switch err.(type) {
	case *os.PathError, *os.LinkError, *os.SyscallError:
		return false
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		causes := wrapped.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !onlyContentMutationOrderLoss(cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return onlyContentMutationOrderLoss(wrapped.Unwrap())
	default:
		return false
	}
}

func (s *JSONLStore) prepareContentMutationOrderFence(key string, meta *SessionMeta) error {
	marked, err := s.hasHistoryRecoveryMarker(key)
	if err != nil {
		return err
	}
	if marked {
		s.rememberHistoryRecoveryMarker(key, true)
		meta.HistoryRecoveryRequired = true
	}
	if meta.HistoryRecoveryRequired || meta.HistoryOrderVersion == 0 {
		return nil
	}
	if proofErr := s.probeHistoryOrderWitness(key, meta); proofErr != nil && !onlyContentMutationOrderLoss(proofErr) {
		return proofErr
	}
	return nil
}

// This uses the same authoritative journal and exact NEW fingerprint as the
// receipt helper, but a missing independent Order witness cannot negate a
// content-only transition. It never treats operational recovery errors as a
// successful publication or emits any receipt binding.
func (s *JSONLStore) recoverExpectedHistoryContentCommit(key string, next SessionMeta) (bool, error) {
	decision, recoveryErr := s.recoverHistoryWriteFor(key, historyFailedWrite)
	if decision != historyWriteNew {
		return false, recoveryErr
	}
	current, readErr := s.readRecoveryMeta(key)
	if readErr != nil {
		return false, errors.Join(recoveryErr, readErr)
	}
	if !historyNewCheckpointMatches(current, next) {
		return false, errors.Join(recoveryErr, ErrContentStateChanged)
	}
	if recoveryErr != nil {
		log.Printf("memory: decided content mutation has pending operational recovery: %v", recoveryErr)
		return true, recoveryErr
	}
	if err := s.prepareContentMutationOrderFence(key, &current); err != nil {
		return true, err
	}
	return true, recoveryErr
}

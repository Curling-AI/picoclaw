package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
)

// ReadPublicHistoryFS reads one confirmed view without initializing metadata,
// settling a journal, issuing receipts or modifying a sleeping pod's volume.
func ReadPublicHistoryFS(ctx context.Context, files fs.FS, key string) (PublicHistory, error) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key {
		return PublicHistory{}, ErrHistoryOwnershipUnavailable
	}
	name := sanitizeKey(key)
	metadata, err := fs.ReadFile(files, name+".meta.json")
	if err != nil {
		return PublicHistory{}, err
	}
	var meta SessionMeta
	if json.Unmarshal(metadata, &meta) != nil || meta.Key != key || meta.PublicHistoryVersion != PublicHistoryVersion ||
		meta.PublicHistoryPreparation != nil || meta.PublicHistoryBytes < 0 || meta.PublicHistoryCount < 0 {
		return PublicHistory{}, ErrPublicHistoryCoverage
	}
	if err = checkPublicReadFiles(ctx, files, name, &meta); err != nil {
		return PublicHistory{}, err
	}
	data, err := fs.ReadFile(files, name+".jsonl.public")
	if err != nil {
		return PublicHistory{}, err
	}
	entries, err := decodePublicEntries(data, meta)
	if err != nil {
		return PublicHistory{}, err
	}
	current, err := fs.ReadFile(files, name+".meta.json")
	if err != nil {
		return PublicHistory{}, err
	}
	if !bytes.Equal(metadata, current) {
		return PublicHistory{}, ErrPublicHistoryCoverage
	}
	if err = checkPublicReadFiles(ctx, files, name, &meta); err != nil {
		return PublicHistory{}, err
	}
	return publicHistoryResult(meta, entries), nil
}

func checkPublicReadFiles(ctx context.Context, files fs.FS, name string, meta *SessionMeta) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	meta.publicRefUnavailable = meta.ContentIdentity == nil || meta.ContentIdentity.Unavailable
	meta.publicOrderUnavailable = meta.HistoryRecoveryRequired || meta.HistoryOrderVersion != HistoryOrderVersion
	for _, suffix := range []string{".jsonl.commit.json", ".jsonl.commit.decision"} {
		if _, err := fs.Stat(files, name+suffix); err == nil {
			return ErrHistoryRecoveryRequired
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if _, err := fs.Stat(files, name+".jsonl.recovery-required"); err == nil {
		meta.publicOrderUnavailable = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if meta.ContentIdentity != nil {
		data, err := fs.ReadFile(files, name+".jsonl.content-domain")
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		var witness contentDomainWitness
		validDomain := err == nil && len(data) <= maxContentTransitionMetadata &&
			json.Unmarshal(data, &witness) == nil && witness.Key == meta.Key
		if !validDomain || !contentWitnessMatches(witness, *meta) {
			meta.publicRefUnavailable = true
		}
		// The independent domain also fences rollback of an otherwise valid
		// order witness. Display availability never weakens that receipt guard.
		protected := meta.ContentIdentity.MutationVersion != 0 || meta.ContentIdentity.storage != nil
		if !validDomain || (protected && witness.OrderCheckpoint == "") ||
			(witness.OrderCheckpoint != "" && witness.OrderCheckpoint != contentOrderCheckpointDigest(*meta)) {
			meta.publicOrderUnavailable = true
		}
	}
	if meta.HistoryOrderVersion != 0 {
		witness, err := fs.ReadFile(files, name+".jsonl.order-witness")
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err != nil || !validContentReceiptWitness(witness, *meta) {
			meta.publicOrderUnavailable = true
		}
	}
	return nil
}

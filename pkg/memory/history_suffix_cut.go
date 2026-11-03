package memory

import (
	"bytes"
	"path/filepath"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// A model rollback removes a suffix of the active window, not its skipped
// display source. Keep the exact bytes/locators of every retained occurrence;
// neither an artifact lookup nor a successful preparation is needed for that.
type historyRawRewrite struct {
	data           []byte
	removed        map[string]bool
	retainedPrefix bool
	publicChange   *publicHistoryChange
}

func (s *JSONLStore) commitHistorySuffixCut(key string, old, retained []providers.Message, meta SessionMeta) error {
	raw, _, err := s.readRawJSONL(key)
	if err != nil {
		return err
	}
	removed := make(map[string]bool, len(old)-len(retained))
	for _, message := range old[len(retained):] {
		removed[message.HistoryID] = true
	}
	first := old[len(retained)].HistoryID
	offset, count := 0, 0
	found := false
	for _, row := range bytes.SplitAfter(raw, []byte{'\n'}) {
		if len(row) == 0 {
			continue
		}
		if row[len(row)-1] != '\n' {
			break
		}
		if len(bytes.TrimRight(row, "\r\n")) == 0 {
			offset += len(row)
			continue
		}
		count++
		if count > meta.Skip && decodedHistoryID(row, filepath.Base(s.jsonlPath(key)), int64(count-1)) == first {
			count--
			found = true
			break
		}
		offset += len(row)
	}
	if !found || count < meta.Skip {
		return ErrContentStateChanged
	}
	// Physical positions survive; retire opaque cursors even after cut/regrowth.
	meta.HistoryFileEpoch++
	meta.Count = count
	meta.HistoryTip = ""
	if len(retained) > 0 {
		meta.HistoryTip = retained[len(retained)-1].HistoryID
	}
	meta.HistoryDigest, err = historyDigest(retained)
	if err != nil {
		return err
	}
	// Removing the legacy part of an initialized stream cannot reissue its
	// independent order witness. Preserve content availability and fail closed.
	if meta.HistoryFileGeneration == meta.HistoryLegacyFileGeneration && int64(offset) < meta.HistoryLegacyBytes {
		if err = s.markHistoryRecoveryRequired(key, &meta); err != nil {
			return err
		}
	}
	return s.commitHistoryRewriteData(
		key,
		retained,
		meta,
		false,
		&historyRawRewrite{data: raw[:offset], removed: removed},
	)
}

func (s *JSONLStore) preparePublicSuffixCut(
	key string,
	original SessionMeta,
	next *SessionMeta,
	removed map[string]bool,
	sourceBytes int64,
) (publicHistoryChange, error) {
	// Unknown display state stays unknown; a model rollback cannot adopt an
	// unrelated sidecar or manufacture migration coverage.
	if original.PublicHistoryVersion != PublicHistoryVersion {
		return publicHistoryChange{}, nil
	}
	readMeta := original
	readMeta.PublicHistoryPreparation = nil
	entries, err := s.readPublicEntries(key, readMeta)
	if err != nil {
		return publicHistoryChange{}, err
	}
	// Imported descendants share their anchor/boundary's lifecycle, including
	// transitive imports. Unrelated supplements keep their original identity.
	for changed := true; changed; {
		changed = false
		for _, entry := range entries {
			if !removed[entry.ID] && (removed[entry.AnchorID] || removed[entry.BoundaryID]) {
				removed[entry.ID] = true
				changed = true
			}
		}
	}
	kept := make([]PublicHistoryEntry, 0, len(entries))
	for _, entry := range entries {
		if removed[entry.ID] {
			continue
		}
		if next.ContentIdentity == nil || next.ContentIdentity.Unavailable {
			entry.Ref = nil
			entry.AnchorRef = nil
		}
		kept = append(kept, entry)
	}
	next.PublicHistoryGeneration = newHistoryID()
	if original.PublicHistoryPreparation != nil {
		preparation := *original.PublicHistoryPreparation
		if preparation.Generation != original.HistoryGeneration ||
			preparation.FileGeneration != original.HistoryFileGeneration ||
			preparation.Prefix != original.HistoryPrefix || preparation.Line < 0 ||
			preparation.Line > preparation.Count || preparation.Count > int64(original.Count) ||
			preparation.Offset < 0 || preparation.Offset > preparation.Ceiling || preparation.Ceiling > sourceBytes {
			return publicHistoryChange{}, ErrPublicHistoryCoverage
		}
		// Retained bytes have not moved. Checkpoints before the cut remain exact;
		// checkpoints at/after it have already covered every remaining source row.
		preparation.Ceiling = *next.HistoryBytes
		preparation.Count = int64(next.Count)
		if err := rebasePublicMediaTail(original, entries, kept, &preparation); err != nil {
			return publicHistoryChange{}, err
		}
		if preparation.Offset >= preparation.Ceiling && preparation.Line >= preparation.Count {
			next.PublicHistoryPreparation = nil
			next.PublicHistoryComplete = original.PublicHistoryComplete || preparation.Prefix == 0
		} else {
			next.PublicHistoryPreparation = &preparation
		}
	}
	return s.replacePublicEntries(key, kept, next)
}

// A replacement of the model context is not permission to collect an
// unavailable display prefix. Existing rewrite rules still govern the active
// window's new generation and independent occurrence order.
func (s *JSONLStore) commitHistoryRetainedPrefix(
	key string,
	messages []providers.Message,
	original, next SessionMeta,
) error {
	raw, _, err := s.readRawJSONL(key)
	if err != nil {
		return err
	}
	// Retain every complete original row, including confirmed active rows a
	// generic replacement would otherwise discard before their display is
	// covered. An unterminated tail is not a committed source row and must not
	// be concatenated onto the first new model record. OLD still backs it up.
	offset := bytes.LastIndexByte(raw, '\n') + 1
	count := 0
	for _, row := range bytes.SplitAfter(raw[:offset], []byte{'\n'}) {
		if len(bytes.TrimRight(row, "\r\n")) != 0 {
			count++
		}
	}
	if count != original.Count || (original.HistoryBytes != nil && *original.HistoryBytes != int64(offset)) {
		// Complete unowned rows remain data, never proof of a committed
		// continuation. Use the existing durable recovery fence.
		if err = s.markHistoryRecoveryRequired(key, &next); err != nil {
			return err
		}
	}
	next.HistoryPrefix = original.HistoryPrefix
	next.Skip = count
	next.Count = len(messages) + count
	active, err := marshalHistoryJSONL(messages, next)
	if err != nil {
		return err
	}
	data := append(append([]byte(nil), raw[:offset]...), active...)
	return s.commitHistoryRewriteData(key, messages, next, false, &historyRawRewrite{data: data, retainedPrefix: true})
}

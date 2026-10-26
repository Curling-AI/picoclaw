package memory

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// appendHistoryMessages commits the byte checkpoint only after the appended
// data is synced. Only our own journal authorizes rollback of complete rows;
// an external tail is preserved without claiming capture provenance for it.
func (s *JSONLStore) appendHistoryMessages(key string, messages []providers.Message, meta SessionMeta) error {
	original, err := s.readMeta(key)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(s.jsonlPath(key), os.O_RDWR, 0o644)
	if errors.Is(err, os.ErrNotExist) {
		// Only this successful exclusive create proves the old transcript is
		// gone. A concurrent creator is ambiguous and must retry from disk.
		file, err = os.OpenFile(s.jsonlPath(key), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
		if err == nil {
			if _, statErr := os.Stat(s.metaPath(key)); errors.Is(statErr, os.ErrNotExist) {
				now := time.Now()
				original = SessionMeta{Key: key, HistoryRecoveryRequired: meta.HistoryRecoveryRequired}
				meta = SessionMeta{
					Key: key, CreatedAt: now, UpdatedAt: now, HistoryRecoveryRequired: meta.HistoryRecoveryRequired,
				}
				resetHistoryGeneration(&meta)
				s.metaMu.Lock()
				delete(s.metaCache, sanitizeKey(key))
				s.metaMu.Unlock()
			} else if statErr != nil || original.Count != 0 || original.HistoryBytes != nil {
				file.Close()
				return errors.Join(ErrHistoryCursor, statErr)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("memory: open jsonl for append: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	offset, err := historyAppendOffset(file, &meta, info.Size())
	if err != nil {
		return err
	}
	if meta.HistoryRecoveryRequired {
		if operationErr := s.markHistoryRecoveryRequired(key, &meta); operationErr != nil {
			return operationErr
		}
		// The durable marker overlays readMeta, but OLD/NEW fingerprints must
		// describe the actual metadata that preceded this append.
		original, err = s.readRecoveryMeta(key)
		if errors.Is(err, os.ErrNotExist) {
			original, err = SessionMeta{Key: key}, nil
		}
		if err != nil {
			return err
		}
		if original.Key == "" {
			original.Key = key
		}
		meta.HistoryDigest, meta.HistoryTip = "", ""
	}
	data, err := marshalHistoryJSONL(messages, meta)
	if err != nil {
		return err
	}
	if meta.Count == 0 && offset == 0 && !meta.HistoryRecoveryRequired {
		meta.HistoryDigest = emptyHistoryDigest
	}
	for _, message := range messages {
		if validHistoryDigest(meta.HistoryDigest) {
			meta.HistoryDigest, err = appendHistoryDigest(meta.HistoryDigest, message)
			if err != nil {
				return err
			}
		}
		meta.Count++
		if !meta.HistoryRecoveryRequired {
			meta.HistoryTip = message.HistoryID
		}
	}
	end := offset + int64(len(data))
	meta.HistoryBytes = &end
	if commitErr := s.beginHistoryWrite(key, historyWriteRecovery{
		Meta: original, Next: meta, AppendOffset: &offset,
	}); commitErr != nil {
		return commitErr
	}
	// Discard only an incomplete suffix. Complete external rows are before
	// offset, so even a failure of this append rolls back to their preserved EOF.
	if truncateErr := file.Truncate(offset); truncateErr != nil {
		return errors.Join(truncateErr, s.recoverFailedHistoryWrite(key))
	}
	if _, seekErr := file.Seek(offset, io.SeekStart); seekErr != nil {
		return errors.Join(seekErr, s.recoverFailedHistoryWrite(key))
	}
	written, err := file.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = s.writeMeta(key, meta)
	}
	if err != nil {
		return errors.Join(err, s.recoverFailedHistoryWrite(key))
	}
	return s.finishHistoryWrite(key)
}

func (s *JSONLStore) rollbackHistoryAppend(key string, size int64) error {
	file, err := os.OpenFile(s.jsonlPath(key), os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("memory: open append rollback: %w", err)
	}
	defer file.Close()
	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("memory: truncate append rollback: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("memory: sync append rollback: %w", err)
	}
	return file.Close()
}

func historyAppendOffset(file *os.File, meta *SessionMeta, physicalBytes int64) (int64, error) {
	if meta.HistoryBytes != nil {
		if *meta.HistoryBytes < 0 || *meta.HistoryBytes > physicalBytes {
			return 0, ErrHistoryCursor
		}
		complete, err := hasCompleteHistoryTail(file, *meta.HistoryBytes, physicalBytes)
		if err != nil && !errors.Is(err, ErrHistoryCapacity) {
			return 0, err
		}
		if err == nil && !complete {
			return *meta.HistoryBytes, nil
		}
	}
	// An unowned complete tail cannot be rolled back to an older checkpoint.
	// Count physical rows for writer bookkeeping, without attesting origin or
	// order. Ordinary appends at an unchanged checkpoint do not rescan prefixes.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	var offset int64
	count := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		offset += int64(len(line))
		if len(bytes.TrimRight(line, "\r\n")) > 0 {
			count++
		}
		if errors.Is(historyRecordRecoveryRequired(line), ErrHistoryRecoveryRequired) {
			meta.HistoryRecoveryRequired = true
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if count != meta.Count {
		meta.HistoryDigest, meta.HistoryTip = "", ""
	}
	if (meta.HistoryBytes != nil && offset > *meta.HistoryBytes) || count > meta.Count {
		meta.HistoryRecoveryRequired = true
	}
	meta.Count = count
	if meta.Skip > count {
		meta.Skip = count
	}
	return offset, nil
}

// Page/guard probes have a bounded budget. Append can fall back to the normal
// full committed-row scan when a large partial tail makes this inconclusive.
func hasCompleteHistoryTail(file *os.File, offset, physicalBytes int64) (bool, error) {
	if offset < 0 || offset > physicalBytes {
		return false, ErrHistoryCursor
	}
	if offset == physicalBytes {
		return false, nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], physicalBytes-1); err != nil {
		return false, err
	}
	if last[0] == '\n' {
		return true, nil
	}
	limit := min(physicalBytes-offset, int64(HistoryPageBytes))
	reader := io.NewSectionReader(file, offset, limit)
	buffer := make([]byte, historyReadBufferBytes)
	for {
		n, err := reader.Read(buffer)
		if bytes.IndexByte(buffer[:n], '\n') >= 0 {
			return true, nil
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, err
		}
	}
	if physicalBytes-offset > limit {
		return false, ErrHistoryCapacity
	}
	return false, nil
}

// Content lifecycle remains available without capture proofs. Reconcile the
// physical count and durably propagate the latch before a legitimate rewrite.
// The caller holds the session lock; our own journal is already resolved.
func (s *JSONLStore) prepareHistoryMutation(key string, meta *SessionMeta) error {
	file, err := os.Open(s.jsonlPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if _, err := historyAppendOffset(file, meta, info.Size()); err != nil {
		return err
	}
	if meta.HistoryRecoveryRequired {
		return s.markHistoryRecoveryRequired(key, meta)
	}
	return nil
}

// Automatic pruning and file deletion must not discard an unproven source.
func (s *JSONLStore) guardHistoryMutation(key string, meta SessionMeta) error {
	if err := s.prepareHistoryMutation(key, &meta); err != nil {
		return err
	}
	if meta.HistoryRecoveryRequired {
		return ErrHistoryRecoveryRequired
	}
	return nil
}

// Only a newline commits a physical row, including for legacy truncation scans.
func scanCommittedHistoryLine(data []byte, atEOF bool) (int, []byte, error) {
	if index := bytes.IndexByte(data, '\n'); index >= 0 {
		return index + 1, data[:index+1], nil
	}
	if atEOF {
		return len(data), nil, nil
	}
	return 0, nil, nil
}

func (s *JSONLStore) commitHistoryRewrite(key string, messages []providers.Message, meta SessionMeta) error {
	original, err := s.readMeta(key)
	if err != nil {
		return err
	}
	if operationErr := s.prepareHistoryMutation(key, &original); operationErr != nil {
		return operationErr
	}
	if original.HistoryRecoveryRequired {
		meta.HistoryRecoveryRequired = true
		// The cache overlays the proof fence and reconciled row count. OLD
		// must instead fingerprint the metadata actually present on disk.
		original, err = s.readRecoveryMeta(key)
		if errors.Is(err, os.ErrNotExist) {
			original, err = SessionMeta{Key: key}, nil
		}
		if err != nil {
			return err
		}
		if original.Key == "" {
			original.Key = key
		}
	}
	previous, existed, err := s.readRawJSONL(key)
	if err != nil {
		return err
	}
	data, err := marshalHistoryJSONL(messages, meta)
	if err != nil {
		return err
	}
	size := int64(len(data))
	meta.HistoryBytes = &size
	if err := s.beginHistoryWrite(
		key,
		historyWriteRecovery{Meta: original, Next: meta, Data: previous, Existed: existed},
	); err != nil {
		return err
	}
	if err := s.writeSessionFile(s.jsonlPath(key), data, 0o644); err != nil {
		return errors.Join(err, s.recoverFailedHistoryWrite(key))
	}
	if err := s.writeMeta(key, meta); err != nil {
		return errors.Join(err, s.recoverFailedHistoryWrite(key))
	}
	// If cleanup is interrupted, recovery recognizes the committed generation.
	return s.finishHistoryWrite(key)
}

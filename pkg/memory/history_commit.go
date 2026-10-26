package memory

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// appendHistoryMessages commits the byte checkpoint only after the appended
// data is synced. Failed writes roll back; after a process interruption the
// persisted checkpoint still excludes any abandoned tail from capture.
func (s *JSONLStore) appendHistoryMessages(key string, messages []providers.Message, meta SessionMeta) error {
	_, err := s.appendHistoryMessagesWithReceipts(key, messages, meta)
	return err
}

func (s *JSONLStore) appendHistoryMessagesWithReceipts(
	key string,
	messages []providers.Message,
	meta SessionMeta,
) ([]CommittedHistoryMessage, error) {
	original, err := s.readMeta(key)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(s.jsonlPath(key), os.O_RDWR, 0o644)
	if errors.Is(err, os.ErrNotExist) {
		// Only this successful exclusive create proves the old transcript is
		// gone. A concurrent creator is ambiguous and must retry from disk.
		file, err = os.OpenFile(s.jsonlPath(key), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
		if err == nil {
			if _, statErr := os.Stat(s.metaPath(key)); errors.Is(statErr, os.ErrNotExist) {
				now := time.Now()
				original = SessionMeta{Key: key}
				meta = SessionMeta{Key: key, CreatedAt: now, UpdatedAt: now}
				resetHistoryGeneration(&meta)
				s.metaMu.Lock()
				delete(s.metaCache, sanitizeKey(key))
				s.metaMu.Unlock()
			} else if statErr != nil || original.Count != 0 || original.HistoryBytes != nil {
				file.Close()
				return nil, errors.Join(ErrHistoryCursor, statErr)
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("memory: open jsonl for append: %w", err)
	}
	defer file.Close()
	legacyCount := meta.Count
	offset, err := historyAppendOffset(file, &meta)
	if err != nil {
		return nil, err
	}
	if meta.HistoryOrderVersion == 0 && meta.Count != legacyCount {
		// A legacy offset upgrade may discover complete rows beyond Count.
		// They have no committed-origin evidence and cannot be silently
		// included in the first stream's legacy fence. SetHistory can import
		// them explicitly and allocate fresh content proofs instead.
		return nil, ErrHistoryOrderUnavailable
	}
	ordered := make([]providers.Message, len(messages))
	for index, message := range messages {
		ordered[index], err = s.allocateHistoryOrder(key, message, &meta)
		if err != nil {
			return nil, err
		}
	}
	messages = ordered
	data, err := marshalHistoryJSONL(messages, meta)
	if err != nil {
		return nil, err
	}
	receipts, err := committedHistoryMessages(data)
	if err != nil {
		return nil, err
	}
	if meta.Count == 0 && offset == 0 {
		meta.HistoryDigest = emptyHistoryDigest
	}
	for _, message := range messages {
		if validHistoryDigest(meta.HistoryDigest) {
			meta.HistoryDigest, err = appendHistoryDigest(meta.HistoryDigest, message)
			if err != nil {
				return nil, err
			}
		}
		meta.Count++
		meta.HistoryTip = message.HistoryID
	}
	end := offset + int64(len(data))
	meta.HistoryBytes = &end
	if commitErr := s.beginHistoryWrite(key, historyWriteRecovery{
		Meta: original, Next: meta, AppendOffset: &offset,
	}); commitErr != nil {
		return nil, commitErr
	}
	// This also removes bytes left by a prior append that never committed meta.
	if truncateErr := file.Truncate(offset); truncateErr != nil {
		return nil, errors.Join(truncateErr, s.recoverFailedHistoryWrite(key))
	}
	if _, seekErr := file.Seek(offset, io.SeekStart); seekErr != nil {
		return nil, errors.Join(seekErr, s.recoverFailedHistoryWrite(key))
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
	if err == nil {
		err = s.finishHistoryWrite(key)
	}
	if err != nil {
		committed, recoveryErr := s.recoverExpectedHistoryCommit(key, meta)
		if !committed {
			return nil, errors.Join(err, recoveryErr)
		}
		return receipts, nil
	}
	return receipts, nil
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

func historyAppendOffset(file *os.File, meta *SessionMeta) (int64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if meta.HistoryBytes != nil {
		if *meta.HistoryBytes < 0 || *meta.HistoryBytes > info.Size() {
			return 0, ErrHistoryCursor
		}
		return *meta.HistoryBytes, nil
	}
	// One-time legacy upgrade: preserve complete raw rows and their positions.
	// Subsequent ordinary writes use the byte checkpoint without rescanning.
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
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if count != meta.Count {
		meta.HistoryDigest, meta.HistoryTip = "", ""
	}
	meta.Count = count
	return offset, nil
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
	if err := validateHistoryMessageOrders(messages, meta); err != nil {
		return err
	}
	original, err := s.readMeta(key)
	if err != nil {
		return err
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
	if commitErr := s.beginHistoryWrite(
		key,
		historyWriteRecovery{Meta: original, Next: meta, Data: previous, Existed: existed},
	); commitErr != nil {
		return commitErr
	}
	if writeErr := s.writeSessionFile(s.jsonlPath(key), data, 0o644); writeErr != nil {
		return errors.Join(writeErr, s.recoverFailedHistoryWrite(key))
	}
	err = s.writeMeta(key, meta)
	if err == nil {
		err = s.finishHistoryWrite(key)
	}
	if err != nil {
		committed, recoveryErr := s.recoverExpectedHistoryCommit(key, meta)
		if committed {
			return nil
		}
		return errors.Join(err, recoveryErr)
	}
	return nil
}

func (s *JSONLStore) recoverExpectedHistoryCommit(key string, next SessionMeta) (bool, error) {
	decision, recoveryErr := s.recoverHistoryWriteFor(key, historyFailedWrite)
	if decision != historyWriteNew {
		return false, recoveryErr
	}
	// A durable NEW decision can also accompany unreadable/mismatched metadata.
	// Confirm this operation's exact checkpoint before acknowledging a receipt;
	// cleanup errors retain their fence and do not turn a committed write into
	// a caller retry and a second occurrence.
	current, readErr := s.readRecoveryMeta(key)
	if readErr != nil {
		return false, errors.Join(recoveryErr, readErr)
	}
	if !historyNewCheckpointMatches(current, next) {
		return false, errors.Join(recoveryErr, ErrHistoryOrderUnavailable)
	}
	if recoveryErr != nil {
		log.Printf("memory: confirmed history commit has pending recovery cleanup: %v", recoveryErr)
	}
	return true, recoveryErr
}

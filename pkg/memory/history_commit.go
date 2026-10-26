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
// data is synced. Failed writes roll back; after a process interruption the
// persisted checkpoint still excludes any abandoned tail from capture.
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
				original = SessionMeta{Key: key}
				meta = SessionMeta{Key: key, CreatedAt: now, UpdatedAt: now}
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
	offset, err := historyAppendOffset(file, &meta)
	if err != nil {
		return err
	}
	data, err := marshalHistoryJSONL(messages, meta)
	if err != nil {
		return err
	}
	if meta.Count == 0 && offset == 0 {
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
		meta.HistoryTip = message.HistoryID
	}
	end := offset + int64(len(data))
	meta.HistoryBytes = &end
	if commitErr := s.beginHistoryWrite(key, historyWriteRecovery{
		Meta: original, Next: meta, AppendOffset: &offset,
	}); commitErr != nil {
		return commitErr
	}
	// This also removes bytes left by a prior append that never committed meta.
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

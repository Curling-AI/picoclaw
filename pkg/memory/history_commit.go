package memory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	file, err := os.OpenFile(s.jsonlPath(key), os.O_CREATE|os.O_RDWR, 0o644)
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
		return errors.Join(truncateErr, s.recoverHistoryWrite(key))
	}
	if _, seekErr := file.Seek(offset, io.SeekStart); seekErr != nil {
		return errors.Join(seekErr, s.recoverHistoryWrite(key))
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
		return errors.Join(err, s.recoverHistoryWrite(key))
	}
	s.finishHistoryWrite(key)
	return nil
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
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			return index + 1, data[:index+1], nil
		}
		if atEOF {
			return len(data), nil, nil
		}
		return 0, nil, nil
	})
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

// Both write paths persist recovery metadata before changing the transcript.
// Appends need only the old byte offset; rewrites also retain the original
// bytes. The new metadata checkpoint determines whether a write committed.
type historyWriteRecovery struct {
	Meta         SessionMeta `json:"meta"`
	Next         SessionMeta `json:"next"`
	AppendOffset *int64      `json:"appendOffset,omitempty"`
	Data         []byte      `json:"data,omitempty"`
	Existed      bool        `json:"existed,omitempty"`
}

func (s *JSONLStore) historyRecoveryPath(key string) string {
	return s.jsonlPath(key) + ".commit.json"
}

// Recover before session listings can populate the metadata cache. After
// startup, the session lock protects each transaction and any failure recovery.
func (s *JSONLStore) recoverPendingHistoryWrites() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl.commit.json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if readErr != nil {
			return readErr
		}
		var pending struct {
			Meta struct {
				Key string `json:"key"`
			} `json:"meta"`
		}
		if decodeErr := json.Unmarshal(data, &pending); decodeErr != nil {
			return decodeErr
		}
		if filepath.Base(s.historyRecoveryPath(pending.Meta.Key)) != entry.Name() {
			return errors.New("invalid history recovery key")
		}
		if recoveryErr := s.recoverHistoryWrite(pending.Meta.Key); recoveryErr != nil {
			return recoveryErr
		}
	}
	return nil
}

func (s *JSONLStore) beginHistoryWrite(key string, recovery historyWriteRecovery) error {
	data, err := json.Marshal(recovery)
	if err != nil {
		return err
	}
	if err := s.writeSessionFile(s.historyRecoveryPath(key), data, 0o600); err != nil {
		// No transcript write has begun, so a partial recovery file is unused.
		_ = os.Remove(s.historyRecoveryPath(key))
		return err
	}
	s.markHistoryPending(key)
	return nil
}

func (s *JSONLStore) markHistoryPending(key string) {
	sanitized := sanitizeKey(key)
	s.metaMu.Lock()
	s.historyPending[sanitized] = true
	delete(s.metaCache, sanitized)
	s.metaMu.Unlock()
}

func (s *JSONLStore) finishHistoryWrite(key string) {
	_ = os.Remove(s.historyRecoveryPath(key))
	s.metaMu.Lock()
	delete(s.historyPending, sanitizeKey(key))
	s.metaMu.Unlock()
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
		return errors.Join(err, s.recoverHistoryWrite(key))
	}
	if err := s.writeMeta(key, meta); err != nil {
		return errors.Join(err, s.recoverHistoryWrite(key))
	}
	// If cleanup is interrupted, recovery recognizes the committed generation.
	s.finishHistoryWrite(key)
	return nil
}

func (s *JSONLStore) recoverHistoryWrite(key string) error {
	data, err := os.ReadFile(s.historyRecoveryPath(key))
	if errors.Is(err, os.ErrNotExist) {
		sanitized := sanitizeKey(key)
		s.metaMu.Lock()
		if s.historyPending[sanitized] {
			delete(s.metaCache, sanitized)
			delete(s.historyPending, sanitized)
		}
		s.metaMu.Unlock()
		return nil
	}
	s.markHistoryPending(key)
	if err != nil {
		return fmt.Errorf("memory: read history recovery: %w", err)
	}
	var recovery historyWriteRecovery
	if err := json.Unmarshal(data, &recovery); err != nil {
		return fmt.Errorf("memory: decode history recovery: %w", err)
	}
	if recovery.Next.HistoryBytes == nil || *recovery.Next.HistoryBytes < 0 ||
		recovery.Next.HistoryFileGeneration == "" || recovery.Next.Count < 0 {
		return errors.New("invalid history recovery checkpoint")
	}
	committedMeta, readErr := os.ReadFile(s.metaPath(key))
	var current SessionMeta
	if readErr == nil && json.Unmarshal(committedMeta, &current) == nil &&
		current.HistoryFileGeneration == recovery.Next.HistoryFileGeneration &&
		current.HistoryTip == recovery.Next.HistoryTip && current.Count == recovery.Next.Count &&
		current.HistoryBytes != nil && recovery.Next.HistoryBytes != nil &&
		*current.HistoryBytes == *recovery.Next.HistoryBytes {
		s.storeCachedMeta(sanitizeKey(key), current)
	} else {
		var err error
		if recovery.AppendOffset != nil {
			err = s.rollbackHistoryAppend(key, *recovery.AppendOffset)
		} else {
			err = s.restoreRawJSONL(key, recovery.Data, recovery.Existed)
		}
		if err != nil {
			return err
		}
		if err := s.writeMeta(key, recovery.Meta); err != nil {
			return err
		}
	}
	s.finishHistoryWrite(key)
	return nil
}

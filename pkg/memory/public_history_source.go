package memory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// readPublicSource reads the committed display source without treating its
// independent Order or ContentRef witnesses as a prerequisite for its body.
// IDs come only from source locators; this function does not issue receipts.
func (s *JSONLStore) readPublicSource(key string, meta SessionMeta, skip int) ([]providers.Message, error) {
	messages, _, err := s.readPublicSourceLines(key, meta, skip)
	return messages, err
}

func (s *JSONLStore) readPublicSourceLines(
	key string,
	meta SessionMeta,
	skip int,
) (messages []providers.Message, lines map[string]int, resultErr error) {
	file, err := os.Open(s.jsonlPath(key))
	if errors.Is(err, os.ErrNotExist) && meta.Count == 0 {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	var reader io.Reader = file
	if meta.HistoryBytes != nil {
		info, statErr := file.Stat()
		if statErr != nil {
			return nil, nil, statErr
		}
		if *meta.HistoryBytes < 0 || *meta.HistoryBytes > info.Size() {
			return nil, nil, ErrPublicHistoryCoverage
		}
		reader = io.LimitReader(file, *meta.HistoryBytes)
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, historyReadBufferBytes), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	line := 0
	lines = make(map[string]int)
	for line < meta.Count && scanner.Scan() {
		raw := bytes.TrimRight(scanner.Bytes(), "\r\n")
		if len(raw) == 0 {
			continue
		}
		line++
		if line <= skip {
			continue
		}
		var message providers.Message
		if json.Unmarshal(raw, &message) != nil {
			continue
		}
		if _, visible := publicHistoryMessage(message); !visible {
			continue
		}
		message.HistoryID = decodedHistoryID(raw, filepath.Base(file.Name()), int64(line-1))
		lines[message.HistoryID] = line
		message.HistoryRunID = decodedHistoryRunID(raw)
		message.HistoryPublicMessage = decodedPublicMessage(raw)
		if !meta.HistoryRecoveryRequired {
			message = messageWithHistoryOrder(message, decodedStoredContentOrder(raw))
		}
		messages = append(messages, message)
	}
	if scanner.Err() != nil {
		return nil, nil, scanner.Err()
	}
	if line != meta.Count {
		return nil, nil, ErrPublicHistoryCoverage
	}
	return messages, lines, nil
}

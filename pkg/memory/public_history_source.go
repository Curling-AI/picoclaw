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
	messages, _, _, err := s.readPublicSourceLines(key, meta, skip, true)
	return messages, err
}

func (s *JSONLStore) readPublicSourceLines(
	key string,
	meta SessionMeta,
	skip int,
	allowFilteredActive bool,
) (messages []providers.Message, lines map[string]int, filtered bool, resultErr error) {
	ensureHistoryGeneration(&meta, key)
	file, err := os.Open(s.jsonlPath(key))
	if errors.Is(err, os.ErrNotExist) && meta.Count == 0 {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	var reader io.Reader = file
	if meta.HistoryBytes != nil {
		info, statErr := file.Stat()
		if statErr != nil {
			return nil, nil, false, statErr
		}
		if *meta.HistoryBytes < 0 || *meta.HistoryBytes > info.Size() {
			return nil, nil, false, ErrPublicHistoryCoverage
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
			// An explicit model rewrite keeps its established filtering of
			// invalid active rows. A skipped row has no such removal intent:
			// losing it would silently collect unproved historical display.
			if !allowFilteredActive || line <= meta.Skip {
				return nil, nil, false, ErrPublicHistoryCoverage
			}
			filtered = true
			continue
		}
		if !allowFilteredActive && line <= meta.Skip && !historyLineageMatches(raw, meta, key) {
			return nil, nil, false, ErrPublicHistoryCoverage
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
		return nil, nil, false, scanner.Err()
	}
	if line != meta.Count {
		return nil, nil, false, ErrPublicHistoryCoverage
	}
	return messages, lines, filtered, nil
}

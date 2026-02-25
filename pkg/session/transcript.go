package session

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// TranscriptEntry is a single line in a JSONL transcript file.
type TranscriptEntry struct {
	Seq       int64              `json:"seq"`
	Timestamp time.Time          `json:"ts"`
	Kind      string             `json:"kind"`              // "message", "summary", "meta"
	Message   *providers.Message `json:"message,omitempty"` // present when Kind == "message"
	Summary   string             `json:"summary,omitempty"` // present when Kind == "summary"
	Meta      map[string]any     `json:"meta,omitempty"`    // present when Kind == "meta"
}

// TranscriptWriter appends entries to a JSONL transcript file.
type TranscriptWriter struct {
	path string
	mu   sync.Mutex
	seq  int64
}

// NewTranscriptWriter creates a writer for the given JSONL file path.
func NewTranscriptWriter(path string) *TranscriptWriter {
	return &TranscriptWriter{path: path}
}

// SetSeq sets the next sequence number (used after loading existing transcripts).
func (tw *TranscriptWriter) SetSeq(seq int64) {
	tw.mu.Lock()
	tw.seq = seq
	tw.mu.Unlock()
}

// Path returns the transcript file path.
func (tw *TranscriptWriter) Path() string {
	return tw.path
}

// Append writes a single entry to the transcript file.
func (tw *TranscriptWriter) Append(entry TranscriptEntry) error {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	tw.seq++
	entry.Seq = tw.seq
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	f, err := os.OpenFile(tw.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(data)
	return err
}

// TranscriptReader reads entries from a JSONL transcript file.
type TranscriptReader struct {
	path string
}

// NewTranscriptReader creates a reader for the given JSONL file path.
func NewTranscriptReader(path string) *TranscriptReader {
	return &TranscriptReader{path: path}
}

// ReadAll reads all valid entries from the transcript, skipping corrupt lines.
func (tr *TranscriptReader) ReadAll() ([]TranscriptEntry, error) {
	f, err := os.Open(tr.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []TranscriptEntry
	scanner := bufio.NewScanner(f)
	// Allow lines up to 10MB
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry TranscriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			// Skip corrupt lines
			continue
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return entries, err
	}
	return entries, nil
}

// ReadMessages extracts messages and the latest summary from the transcript.
func (tr *TranscriptReader) ReadMessages() ([]providers.Message, string, error) {
	entries, err := tr.ReadAll()
	if err != nil {
		return nil, "", err
	}

	var messages []providers.Message
	var latestSummary string

	for _, e := range entries {
		switch e.Kind {
		case "message":
			if e.Message != nil {
				messages = append(messages, *e.Message)
			}
		case "summary":
			latestSummary = e.Summary
		}
	}

	return messages, latestSummary, nil
}

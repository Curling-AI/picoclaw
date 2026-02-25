package session

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestTranscript_AppendAndReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	tw := NewTranscriptWriter(path)

	msg1 := providers.Message{Role: "user", Content: "hello"}
	msg2 := providers.Message{Role: "assistant", Content: "hi there"}

	if err := tw.Append(TranscriptEntry{Kind: "message", Message: &msg1}); err != nil {
		t.Fatalf("Append msg1: %v", err)
	}
	if err := tw.Append(TranscriptEntry{Kind: "message", Message: &msg2}); err != nil {
		t.Fatalf("Append msg2: %v", err)
	}
	if err := tw.Append(TranscriptEntry{Kind: "summary", Summary: "User greeted."}); err != nil {
		t.Fatalf("Append summary: %v", err)
	}

	tr := NewTranscriptReader(path)
	entries, err := tr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Seq != 1 || entries[1].Seq != 2 || entries[2].Seq != 3 {
		t.Errorf("unexpected sequence numbers: %d, %d, %d", entries[0].Seq, entries[1].Seq, entries[2].Seq)
	}
	if entries[0].Kind != "message" || entries[0].Message.Content != "hello" {
		t.Errorf("entry 0: unexpected kind/content: %s/%s", entries[0].Kind, entries[0].Message.Content)
	}
	if entries[2].Kind != "summary" || entries[2].Summary != "User greeted." {
		t.Errorf("entry 2: unexpected kind/summary: %s/%s", entries[2].Kind, entries[2].Summary)
	}
}

func TestTranscript_CorruptLineHandling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.jsonl")

	tw := NewTranscriptWriter(path)
	msg := providers.Message{Role: "user", Content: "valid"}
	if err := tw.Append(TranscriptEntry{Kind: "message", Message: &msg}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Inject garbage line
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("this is not valid json\n")
	f.Close()

	msg2 := providers.Message{Role: "assistant", Content: "also valid"}
	if err := tw.Append(TranscriptEntry{Kind: "message", Message: &msg2}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	tr := NewTranscriptReader(path)
	entries, err := tr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 valid entries (skipping corrupt), got %d", len(entries))
	}
	if entries[0].Message.Content != "valid" {
		t.Errorf("entry 0 content: %s", entries[0].Message.Content)
	}
	if entries[1].Message.Content != "also valid" {
		t.Errorf("entry 1 content: %s", entries[1].Message.Content)
	}
}

func TestTranscript_ConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.jsonl")

	tw := NewTranscriptWriter(path)
	const n = 50

	var wg sync.WaitGroup
	wg.Add(2)

	appendN := func(start int) {
		defer wg.Done()
		for i := start; i < start+n; i++ {
			msg := providers.Message{Role: "user", Content: "msg"}
			tw.Append(TranscriptEntry{Kind: "message", Message: &msg})
		}
	}
	go appendN(0)
	go appendN(n)
	wg.Wait()

	tr := NewTranscriptReader(path)
	entries, err := tr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 2*n {
		t.Fatalf("expected %d entries, got %d", 2*n, len(entries))
	}

	// Check all sequence numbers are unique
	seqs := make(map[int64]bool)
	for _, e := range entries {
		if seqs[e.Seq] {
			t.Errorf("duplicate seq %d", e.Seq)
		}
		seqs[e.Seq] = true
	}
}

func TestTranscript_ReadMessages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.jsonl")

	tw := NewTranscriptWriter(path)

	msg1 := providers.Message{Role: "user", Content: "hello"}
	msg2 := providers.Message{Role: "assistant", Content: "hi"}
	tw.Append(TranscriptEntry{Kind: "message", Message: &msg1})
	tw.Append(TranscriptEntry{Kind: "summary", Summary: "old summary"})
	tw.Append(TranscriptEntry{Kind: "message", Message: &msg2})
	tw.Append(TranscriptEntry{Kind: "summary", Summary: "latest summary"})
	tw.Append(TranscriptEntry{Kind: "meta", Meta: map[string]any{"action": "reset"}})

	tr := NewTranscriptReader(path)
	messages, summary, err := tr.ReadMessages()
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}

	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0].Content != "hello" || messages[1].Content != "hi" {
		t.Errorf("unexpected message contents")
	}
	if summary != "latest summary" {
		t.Errorf("expected latest summary, got %q", summary)
	}
}

func TestTranscript_ReadAll_FileNotFound(t *testing.T) {
	tr := NewTranscriptReader("/nonexistent/path.jsonl")
	entries, err := tr.ReadAll()
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if entries != nil {
		t.Errorf("expected nil entries, got %d", len(entries))
	}
}

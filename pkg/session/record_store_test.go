package session

import (
	"testing"
)

func TestRecordStore_CreateUpdateList(t *testing.T) {
	dir := t.TempDir()
	rs := NewRecordStore(dir)

	rec := rs.GetOrCreate("session-1", "agent-main")
	if rec.SessionKey != "session-1" {
		t.Errorf("SessionKey = %q, want %q", rec.SessionKey, "session-1")
	}
	if rec.AgentID != "agent-main" {
		t.Errorf("AgentID = %q, want %q", rec.AgentID, "agent-main")
	}

	rs.Update("session-1", func(r *SessionRecord) {
		r.MessageCount = 5
		r.HasSummary = true
	})

	rec2 := rs.Get("session-1")
	if rec2 == nil {
		t.Fatal("expected non-nil record")
	}
	if rec2.MessageCount != 5 {
		t.Errorf("MessageCount = %d, want 5", rec2.MessageCount)
	}
	if !rec2.HasSummary {
		t.Error("HasSummary should be true")
	}

	rs.GetOrCreate("session-2", "agent-sales")
	list := rs.List()
	if len(list) != 2 {
		t.Errorf("List() length = %d, want 2", len(list))
	}
}

func TestRecordStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	rs := NewRecordStore(dir)

	rs.GetOrCreate("key1", "agent1")
	rs.Update("key1", func(r *SessionRecord) {
		r.MessageCount = 10
		r.TranscriptFile = "key1.jsonl"
	})

	if err := rs.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload
	rs2 := NewRecordStore(dir)
	rec := rs2.Get("key1")
	if rec == nil {
		t.Fatal("expected record after reload")
	}
	if rec.MessageCount != 10 {
		t.Errorf("MessageCount = %d, want 10", rec.MessageCount)
	}
	if rec.TranscriptFile != "key1.jsonl" {
		t.Errorf("TranscriptFile = %q, want %q", rec.TranscriptFile, "key1.jsonl")
	}
}

func TestRecordStore_GetNonexistent(t *testing.T) {
	dir := t.TempDir()
	rs := NewRecordStore(dir)

	if rec := rs.Get("missing"); rec != nil {
		t.Errorf("expected nil for missing key, got %+v", rec)
	}
}

func TestRecordStore_Delete(t *testing.T) {
	dir := t.TempDir()
	rs := NewRecordStore(dir)

	rs.GetOrCreate("del-me", "agent1")
	rs.Delete("del-me")

	if rec := rs.Get("del-me"); rec != nil {
		t.Errorf("expected nil after delete, got %+v", rec)
	}
}

func TestRecordStore_UpdateNonexistent(t *testing.T) {
	dir := t.TempDir()
	rs := NewRecordStore(dir)

	// Should not panic
	rs.Update("missing", func(r *SessionRecord) {
		r.MessageCount = 99
	})
}

func TestRecordStore_GetOrCreateIdempotent(t *testing.T) {
	dir := t.TempDir()
	rs := NewRecordStore(dir)

	rec1 := rs.GetOrCreate("k", "a1")
	rec1Created := rec1.Created

	rec2 := rs.GetOrCreate("k", "a2") // second call should return existing
	if rec2.AgentID != "a1" {
		t.Errorf("AgentID = %q, want %q (should not change on second call)", rec2.AgentID, "a1")
	}
	if !rec2.Created.Equal(rec1Created) {
		t.Errorf("Created time changed on second GetOrCreate")
	}
}

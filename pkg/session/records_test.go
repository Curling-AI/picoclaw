package session

import "testing"

func TestSessionRecordsAndDelete(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	sm.GetOrCreate("k1")
	sm.AddMessage("k1", "user", "hi")
	sm.GetOrCreate("k2")

	recs := sm.ListSessionRecords()
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	var k1 *SessionRecord
	for i := range recs {
		if recs[i].SessionKey == "k1" {
			k1 = &recs[i]
		}
	}
	if k1 == nil {
		t.Fatal("k1 record missing")
	}
	if k1.MessageCount < 1 {
		t.Errorf("k1 MessageCount = %d, want >= 1", k1.MessageCount)
	}
	if k1.Created.IsZero() || k1.Updated.IsZero() {
		t.Errorf("k1 timestamps not set: created=%v updated=%v", k1.Created, k1.Updated)
	}

	if err := sm.DeleteSession("k1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if len(sm.ListSessionRecords()) != 1 {
		t.Errorf("k1 not deleted")
	}
	// Deleting a missing session is a no-op.
	if err := sm.DeleteSession("does-not-exist"); err != nil {
		t.Errorf("delete missing: %v", err)
	}
}

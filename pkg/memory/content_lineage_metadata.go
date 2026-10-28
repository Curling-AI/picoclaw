package memory

import (
	"bytes"
	"encoding/json"
)

// The disk projection is separate from the in-memory state clone. Expanded
// vectors stay intact for qualified operations and never leak into disk2 WAL.
func (meta SessionMeta) MarshalJSON() ([]byte, error) {
	type metadata SessionMeta
	return json.Marshal(struct {
		*metadata
		Identity any `json:"content_identity,omitempty"`
	}{
		metadata: (*metadata)(&meta), Identity: contentIdentityWire(meta.ContentIdentity),
	})
}

// UnmarshalJSON preserves a null identity as evidence of lost continuity. A
// later ordinary write must not erase that evidence through pointer omitempty.
func (meta *SessionMeta) UnmarshalJSON(data []byte) error {
	meta.ContentIdentity = nil
	meta.summaryCheckpoint = ""
	type metadata SessionMeta
	decoded := struct {
		*metadata
		Identity json.RawMessage `json:"content_identity"`
	}{metadata: (*metadata)(meta)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if len(decoded.Identity) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(decoded.Identity), []byte("null")) {
		meta.ContentIdentity = &contentIdentityState{Unavailable: true}
		return nil
	}
	meta.ContentIdentity = decodeContentIdentity(decoded.Identity)
	return nil
}

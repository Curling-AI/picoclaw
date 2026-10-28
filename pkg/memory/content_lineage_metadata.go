package memory

import (
	"bytes"
	"encoding/json"
)

// UnmarshalJSON preserves a null identity as evidence of lost continuity. A
// later ordinary write must not erase that evidence through pointer omitempty.
func (meta *SessionMeta) UnmarshalJSON(data []byte) error {
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
	return json.Unmarshal(decoded.Identity, &meta.ContentIdentity)
}

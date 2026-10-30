// Package lineagefixture constructs disposable pre-typed inline test sources.
// It is used only by tests, never by runtime recovery or format conversion.
package lineagefixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type fileDescriptor struct {
	Key  string `json:"key"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

func (file fileDescriptor) name() string {
	suffix := ".blob"
	if file.Kind == "rows" {
		suffix = ".idx"
	}
	return fmt.Sprintf(".pc-history-%x-%s-%s%s", sha256.Sum256([]byte(file.Key)), file.Kind, file.ID, suffix)
}

// LegacyArchiveDeadline preserves the original expiration assertions while
// making their fixture format explicit. Editing only an expanded Go field no
// longer edits an immutable disk2 blob. This creates the actual old inline
// fixture instead, with byte hashes available to its test log. It is NOT a
// production downgrade; typed operations and opaque states are rejected.
func LegacyArchiveDeadline(dir, key string, deadline time.Time) (map[string]string, error) {
	if filepath.Base(key) != key {
		return nil, fmt.Errorf("fixture key must be a single filename")
	}
	path := filepath.Join(dir, key+".meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	manifest := map[string]string{"originMetaSHA256": fmt.Sprintf("%x", sha256.Sum256(data))}
	var meta map[string]json.RawMessage
	if err = json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	var identity map[string]json.RawMessage
	if err = json.Unmarshal(meta["content_identity"], &identity); err != nil {
		return nil, err
	}
	var storage struct {
		Version uint8 `json:"version"`
		Index   struct {
			File  fileDescriptor `json:"file"`
			Start int            `json:"start"`
			Count int            `json:"count"`
		} `json:"index"`
		Pending *struct {
			File  fileDescriptor `json:"file"`
			Typed bool           `json:"typed"`
		} `json:"pending"`
		Last *struct {
			File  fileDescriptor `json:"file"`
			Typed bool           `json:"typed"`
		} `json:"last"`
	}
	if err = json.Unmarshal(identity["storage"], &storage); err != nil || storage.Version != 2 || storage.Pending == nil ||
		storage.Pending.Typed || (storage.Last != nil && storage.Last.Typed) {
		return nil, fmt.Errorf("fixture requires a known pre-typed disk2 Pending: %v", err)
	}
	index, err := os.ReadFile(filepath.Join(dir, storage.Index.File.name()))
	if err != nil {
		return nil, err
	}
	manifest["originIndexSHA256"] = fmt.Sprintf("%x", sha256.Sum256(index))
	lines := bytes.Split(bytes.TrimSuffix(index, []byte{'\n'}), []byte{'\n'})
	if storage.Index.Start < 0 || storage.Index.Count < 0 || storage.Index.Start > len(lines) ||
		storage.Index.Count > len(lines)-storage.Index.Start {
		return nil, fmt.Errorf("invalid fixture index extent")
	}
	rows := make([]json.RawMessage, storage.Index.Count)
	for i := range rows {
		rows[i] = lines[storage.Index.Start+i]
	}
	identity["rows"], _ = json.Marshal(rows)
	pending, err := os.ReadFile(filepath.Join(dir, storage.Pending.File.name()))
	if err != nil {
		return nil, err
	}
	manifest["originPendingSHA256"] = fmt.Sprintf("%x", sha256.Sum256(pending))
	var record map[string]json.RawMessage
	if err = json.Unmarshal(pending, &record); err != nil {
		return nil, err
	}
	var transition map[string]json.RawMessage
	if err = json.Unmarshal(record["transition"], &transition); err != nil {
		return nil, err
	}
	transition["expiresAt"], _ = json.Marshal(deadline)
	record["transition"], _ = json.Marshal(transition)
	identity["pending"], _ = json.Marshal(record)
	if storage.Last != nil {
		identity["last"], err = os.ReadFile(filepath.Join(dir, storage.Last.File.name()))
		if err != nil {
			return nil, err
		}
	} else {
		delete(identity, "last")
	}
	delete(identity, "diskVersion")
	delete(identity, "storage")
	delete(identity, "mutationVersion")
	meta["content_identity"], _ = json.Marshal(identity)
	data, err = json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	witness, _ := json.Marshal(struct {
		Version uint8           `json:"version"`
		Key     string          `json:"key"`
		Domain  json.RawMessage `json:"domain"`
	}{1, key, identity["domain"]})
	if err = os.WriteFile(filepath.Join(dir, key+".jsonl.content-domain"), witness, 0o600); err != nil {
		return nil, err
	}
	manifest["inlineMetaSHA256"] = fmt.Sprintf("%x", sha256.Sum256(data))
	manifest["inlineFloorSHA256"] = fmt.Sprintf("%x", sha256.Sum256(witness))
	return manifest, nil
}

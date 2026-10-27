package memory

// CheckHistoryDeletion resolves this key's pending write before owner-requested
// deletion. Missing capture proof does not override an explicit deletion intent.
// The caller must still exclude concurrent writers; this is not a transaction
// across processes.
func CheckHistoryDeletion(dir, key string) error {
	s := &JSONLStore{
		dir: dir, metaCache: make(map[string]SessionMeta), historyPending: make(map[string]bool),
	}
	return s.recoverHistoryWrite(key)
}

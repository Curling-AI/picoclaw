package memory

import "context"

type publicHistoryClearIntent struct{}

// ClearHistory is explicit owner intent, distinct from rollback's empty context.
func (s *JSONLStore) ClearHistory(ctx context.Context, key string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.SetHistory(context.WithValue(ctx, publicHistoryClearIntent{}, true), key, nil)
}

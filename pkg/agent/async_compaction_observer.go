package agent

import "context"

type asyncCompactionObserverKey struct{}

// WithAsyncCompactionObserver observes an admitted asynchronous compaction in
// this context, synchronously before its goroutine starts. The callback must
// return promptly and only observe the launch; it does not change the
// background compaction's context, cancellation or usage attribution.
func WithAsyncCompactionObserver(ctx context.Context, observer func(sessionKey string)) context.Context {
	return context.WithValue(ctx, asyncCompactionObserverKey{}, observer)
}

func observeAsyncCompaction(ctx context.Context, sessionKey string) {
	if observer, _ := ctx.Value(asyncCompactionObserverKey{}).(func(string)); observer != nil {
		observer(sessionKey)
	}
}

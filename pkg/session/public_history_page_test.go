package session_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestJSONLBackendPublicHistoryPageResolvesAliasesToThePreparedSource(t *testing.T) {
	store, openErr := memory.NewJSONLStore(t.TempDir())
	if openErr != nil {
		t.Fatal(openErr)
	}
	backend := session.NewJSONLBackend(store)
	key, alias := "sk_v1_paged_alias", "agent:main:paged-alias"
	if _, err := backend.AppendHistoryReceipt(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "canonical occurrence"},
	); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSessionMeta(t.Context(), key, nil, []string{alias}); err != nil {
		t.Fatal(err)
	}
	page, err := backend.ReadPublicHistoryPage(t.Context(), memory.PublicHistoryPageRequest{Key: alias})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Message.Content != "canonical occurrence" {
		t.Fatal("alias selected a different public source", page, err)
	}
	canonical, err := backend.ReadPublicHistoryPage(
		t.Context(),
		memory.PublicHistoryPageRequest{Key: key, Cursor: "", MessageBytes: nil},
	)
	if err != nil || !reflect.DeepEqual(page, canonical) {
		t.Fatal("alias and canonical key selected different public pages", page, canonical, err)
	}
}

type resolvingPublicHistoryStore struct {
	*memory.JSONLStore
	resolve  func(context.Context, string) (string, bool, error)
	readKeys []string
}

func (s *resolvingPublicHistoryStore) ResolveSessionKey(ctx context.Context, key string) (string, bool, error) {
	return s.resolve(ctx, key)
}

func (s *resolvingPublicHistoryStore) ReadPublicHistoryPage(
	_ context.Context,
	request memory.PublicHistoryPageRequest,
) (memory.PublicHistoryPage, error) {
	s.readKeys = append(s.readKeys, request.Key)
	var page memory.PublicHistoryPage
	return page, nil
}

func TestJSONLBackendPublicHistoryPageStopsWhenAliasResolutionIsCanceled(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			wantErr := context.Canceled
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				wantErr = context.DeadlineExceeded
			}
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			store := &resolvingPublicHistoryStore{
				JSONLStore: nil,
				readKeys:   nil,
				resolve: func(resolveCtx context.Context, _ string) (string, bool, error) {
					close(entered)
					select {
					case <-resolveCtx.Done():
						return "", false, resolveCtx.Err()
					case <-release:
						return "", false, nil
					}
				},
			}
			backend := session.NewJSONLBackend(store)
			result := make(chan error, 1)
			go func() {
				_, err := backend.ReadPublicHistoryPage(
					ctx,
					memory.PublicHistoryPageRequest{Key: "alias", Cursor: "", MessageBytes: nil},
				)
				result <- err
			}()
			select {
			case <-entered:
			case err := <-result:
				close(release)
				t.Fatalf("reading ended before entering the alias resolution barrier: %v", err)
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-result:
				close(release)
				if !errors.Is(err, wantErr) || len(store.readKeys) != 0 {
					t.Fatalf(
						"retired alias resolution returned %v and read keys %v, want %v without reading",
						err,
						store.readKeys,
						wantErr,
					)
				}
			case <-time.After(3 * time.Second):
				close(release)
				err := <-result
				t.Fatalf(
					"caller cancellation did not reach alias resolution: error %v, read keys %v",
					err,
					store.readKeys,
				)
			}
		})
	}
}

func TestJSONLBackendPublicHistoryPagePreservesAliasOutcomes(t *testing.T) {
	resolverErr := errors.New("resolver unavailable")
	for _, test := range []struct {
		name     string
		resolved string
		found    bool
		err      error
		wantKey  string
	}{
		{name: "known alias", resolved: "canonical", found: true, err: nil, wantKey: "canonical"},
		{name: "unknown alias", resolved: "", found: false, err: nil, wantKey: "input"},
		{name: "empty resolved alias", resolved: "", found: true, err: nil, wantKey: "input"},
		{name: "resolver failure", resolved: "", found: false, err: resolverErr, wantKey: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &resolvingPublicHistoryStore{
				JSONLStore: nil,
				readKeys:   nil,
				resolve: func(context.Context, string) (string, bool, error) {
					return test.resolved, test.found, test.err
				},
			}
			backend := session.NewJSONLBackend(store)
			_, err := backend.ReadPublicHistoryPage(
				t.Context(),
				memory.PublicHistoryPageRequest{Key: "input", Cursor: "", MessageBytes: nil},
			)
			if !errors.Is(err, test.err) {
				t.Fatalf("ReadPublicHistoryPage error = %v, want %v", err, test.err)
			}
			if test.err != nil {
				if len(store.readKeys) != 0 {
					t.Fatalf("resolver failure continued reading %v", store.readKeys)
				}
				if key := backend.ResolveSessionKey("input"); key != "input" {
					t.Fatalf("legacy resolution returned %q, want original key", key)
				}
				return
			}
			if len(store.readKeys) != 1 || store.readKeys[0] != test.wantKey {
				t.Fatalf("read keys = %v, want [%s]", store.readKeys, test.wantKey)
			}
		})
	}
}

func TestJSONLBackendPublicHistoryPageRejectsRetiredCallerBeforeReading(t *testing.T) {
	for _, mode := range []string{"already canceled", "canceled after resolution"} {
		t.Run(mode, func(t *testing.T) {
			cancelDuringResolution := mode == "canceled after resolution"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			resolved := false
			store := &resolvingPublicHistoryStore{
				JSONLStore: nil,
				readKeys:   nil,
				resolve: func(context.Context, string) (string, bool, error) {
					resolved = true
					cancel()
					return "canonical", true, nil
				},
			}
			if !cancelDuringResolution {
				cancel()
			}
			_, err := session.NewJSONLBackend(store).
				ReadPublicHistoryPage(ctx, memory.PublicHistoryPageRequest{Key: "alias", Cursor: "", MessageBytes: nil})
			if !errors.Is(err, context.Canceled) || len(store.readKeys) != 0 || resolved != cancelDuringResolution {
				t.Fatalf("retired caller resolved=%v, read=%v, error=%v", resolved, store.readKeys, err)
			}
		})
	}
}

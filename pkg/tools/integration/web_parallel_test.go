package integrationtools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebTool_AutoProviderPrefersParallel(t *testing.T) {
	tool, err := NewWebSearchTool(WebSearchToolOptions{
		DuckDuckGoEnabled:    true,
		DuckDuckGoMaxResults: 5,
		BraveEnabled:         true,
		BraveAPIKeys:         []string{"brave-key"},
		ParallelEnabled:      true,
		ParallelAPIKeys:      []string{"parallel-key"},
	})
	if err != nil {
		t.Fatalf("NewWebSearchTool() error: %v", err)
	}
	if _, ok := tool.provider.(*ParallelSearchProvider); !ok {
		t.Fatalf("expected ParallelSearchProvider, got %T", tool.provider)
	}
}

func TestWebTool_ParallelNotReadyWithoutKeys(t *testing.T) {
	opts := WebSearchToolOptions{ParallelEnabled: true}
	if opts.providerReady("parallel") {
		t.Fatal("parallel should not be ready without API keys")
	}
	opts.ParallelAPIKeys = []string{"key"}
	if !opts.providerReady("parallel") {
		t.Fatal("parallel should be ready with key + enabled")
	}
}

func TestParallelSearchProvider_SearchSuccess(t *testing.T) {
	provider := &ParallelSearchProvider{
		keyPool: NewAPIKeyPool([]string{"parallel-key"}),
		mode:    "turbo",
		client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost {
					t.Fatalf("method = %s, want POST", req.Method)
				}
				if got := req.Header.Get("x-api-key"); got != "parallel-key" {
					t.Fatalf("x-api-key = %q, want parallel-key", got)
				}
				if !strings.Contains(req.URL.String(), "api.parallel.ai/v1/search") {
					t.Fatalf("unexpected URL: %s", req.URL.String())
				}
				body, _ := io.ReadAll(req.Body)
				var payload struct {
					Objective        string   `json:"objective"`
					SearchQueries    []string `json:"search_queries"`
					Mode             string   `json:"mode"`
					AdvancedSettings struct {
						MaxResults   int `json:"max_results"`
						SourcePolicy struct {
							AfterDate string `json:"after_date"`
						} `json:"source_policy"`
					} `json:"advanced_settings"`
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("payload unmarshal: %v", err)
				}
				if payload.Objective != "golang generics" {
					t.Fatalf("objective = %q", payload.Objective)
				}
				if len(payload.SearchQueries) != 1 || payload.SearchQueries[0] != "golang generics" {
					t.Fatalf("search_queries = %v", payload.SearchQueries)
				}
				if payload.Mode != "turbo" {
					t.Fatalf("mode = %q, want turbo", payload.Mode)
				}
				if payload.AdvancedSettings.MaxResults != 5 {
					t.Fatalf("max_results = %d, want 5", payload.AdvancedSettings.MaxResults)
				}
				if payload.AdvancedSettings.SourcePolicy.AfterDate == "" {
					t.Fatal("expected after_date for range code w")
				}
				rec := httptest.NewRecorder()
				rec.WriteHeader(http.StatusOK)
				fmt.Fprint(rec, `{
  "search_id": "search_abc",
  "results": [
    {
      "url": "https://go.dev/blog/intro-generics",
      "title": "An Introduction To Generics",
      "publish_date": "2022-03-22",
      "excerpts": ["Generics add type parameters to Go."]
    },
    {
      "url": "https://example.com/untitled",
      "title": null,
      "publish_date": null,
      "excerpts": []
    }
  ]
}`)
				return rec.Result(), nil
			}),
		},
	}

	out, err := provider.Search(context.Background(), "golang generics", 5, "w")
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	for _, want := range []string{
		"(via Parallel)",
		"An Introduction To Generics (2022-03-22)",
		"https://go.dev/blog/intro-generics",
		"Generics add type parameters to Go.",
		"2. https://example.com/untitled",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestParallelSearchProvider_KeyRotationOnAuthError(t *testing.T) {
	var keysSeen []string
	provider := &ParallelSearchProvider{
		keyPool: NewAPIKeyPool([]string{"bad-key", "good-key"}),
		client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				key := req.Header.Get("x-api-key")
				keysSeen = append(keysSeen, key)
				rec := httptest.NewRecorder()
				if key == "bad-key" {
					rec.WriteHeader(http.StatusUnauthorized)
					fmt.Fprint(rec, `{"error": "invalid key"}`)
				} else {
					rec.WriteHeader(http.StatusOK)
					fmt.Fprint(
						rec,
						`{"results": [{"url": "https://example.com", "title": "OK", "excerpts": ["text"]}]}`,
					)
				}
				return rec.Result(), nil
			}),
		},
	}

	out, err := provider.Search(context.Background(), "query", 3, "")
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(keysSeen) != 2 {
		t.Fatalf("expected 2 attempts, got %d (%v)", len(keysSeen), keysSeen)
	}
	if !strings.Contains(out, "https://example.com") {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestParallelSearchProvider_EmptyResults(t *testing.T) {
	provider := &ParallelSearchProvider{
		keyPool: NewAPIKeyPool([]string{"key"}),
		client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				rec.WriteHeader(http.StatusOK)
				fmt.Fprint(rec, `{"results": []}`)
				return rec.Result(), nil
			}),
		},
	}
	out, err := provider.Search(context.Background(), "nothing here", 3, "")
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if !strings.Contains(out, "No results for: nothing here") {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestMapParallelAfterDate(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"d": "2026-07-20",
		"w": "2026-07-14",
		"m": "2026-06-21",
		"y": "2025-07-21",
		"":  "",
		"x": "",
	}
	for code, want := range cases {
		if got := mapParallelAfterDate(code, now); got != want {
			t.Errorf("mapParallelAfterDate(%q) = %q, want %q", code, got, want)
		}
	}
}

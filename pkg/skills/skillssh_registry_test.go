package skills

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

func newSkillsShTestRegistry(t *testing.T, baseURL string) *SkillsShRegistry {
	t.Helper()
	reg := SkillsShRegistryConfig{Enabled: true, BaseURL: baseURL}.BuildRegistry()
	skillssh, ok := reg.(*SkillsShRegistry)
	if !ok {
		t.Fatalf("expected *SkillsShRegistry, got %T", reg)
	}
	return skillssh
}

func TestSkillsShRegistry_Search(t *testing.T) {
	var gotQuery, gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != skillsShSearchPath {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gotQuery = r.URL.Query().Get("q")
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		// Second entry has no source -> slug falls back to id.
		_, _ = w.Write([]byte(`{"skills":[
			{"id":"acme/db","name":"Database","installs":1000,"source":"acme/db"},
			{"id":"loose-skill","name":"Loose","installs":0,"source":""}
		]}`))
	}))
	defer srv.Close()

	reg := newSkillsShTestRegistry(t, srv.URL)
	results, err := reg.Search(context.Background(), "database", 5)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if gotQuery != "database" {
		t.Errorf("query = %q, want %q", gotQuery, "database")
	}
	if gotLimit != "5" {
		t.Errorf("limit = %q, want %q", gotLimit, "5")
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	first := results[0]
	if first.Slug != "acme/db" {
		t.Errorf("slug = %q, want acme/db", first.Slug)
	}
	if first.DisplayName != "Database" {
		t.Errorf("display = %q, want Database", first.DisplayName)
	}
	if first.RegistryName != "skillssh" {
		t.Errorf("registry = %q, want skillssh", first.RegistryName)
	}
	if want := math.Log10(1001); first.Score != want {
		t.Errorf("score = %v, want %v (log10 of installs+1)", first.Score, want)
	}

	// Source empty -> slug falls back to the id.
	if results[1].Slug != "loose-skill" {
		t.Errorf("fallback slug = %q, want loose-skill", results[1].Slug)
	}
}

func TestSkillsShRegistry_SearchEmptyQuery(t *testing.T) {
	reg := newSkillsShTestRegistry(t, "https://skills.sh")
	results, err := reg.Search(context.Background(), "   ", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results for empty query, got %v", results)
	}
}

func TestSkillsShRegistry_Name(t *testing.T) {
	reg := newSkillsShTestRegistry(t, "")
	if reg.Name() != "skillssh" {
		t.Errorf("Name() = %q, want skillssh", reg.Name())
	}
	if reg.baseURL != defaultSkillsShBaseURL {
		t.Errorf("empty BaseURL should default to %q, got %q", defaultSkillsShBaseURL, reg.baseURL)
	}
}

func TestSkillsShRegistry_BuilderRegistered(t *testing.T) {
	provider := buildRegistryProvider("skillssh", config.SkillRegistryConfig{
		Name:    "skillssh",
		Enabled: true,
		BaseURL: "https://skills.sh",
	})
	if provider == nil {
		t.Fatal("skillssh provider builder not registered")
	}
	if !provider.IsEnabled() {
		t.Error("provider should be enabled")
	}
	if reg := provider.BuildRegistry(); reg == nil || reg.Name() != "skillssh" {
		t.Errorf("BuildRegistry produced %v", reg)
	}
}

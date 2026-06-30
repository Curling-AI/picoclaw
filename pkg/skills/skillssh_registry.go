package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

// The skills.sh ("Vercel Labs skills") directory is a popularity-ranked search
// index over agent skills that are themselves hosted as GitHub repos. We use its
// HTTP search API for discovery and delegate the actual download/install to the
// GitHub registry, since every skills.sh result resolves to an owner/repo target.
const (
	defaultSkillsShBaseURL       = "https://skills.sh"
	defaultSkillsShGitHubBaseURL = "https://github.com"
	skillsShSearchPath           = "/api/search"
	skillsShRequestTimeout       = 30 * time.Second
	skillsShMaxResponseBytes     = 2 << 20 // 2MB
)

func init() {
	RegisterRegistryProviderBuilder("skillssh", func(_ string, cfg config.SkillRegistryConfig) RegistryProvider {
		return SkillsShRegistryConfig{
			Enabled: cfg.Enabled,
			BaseURL: cfg.BaseURL,
			// Optional GitHub token reused for the install/meta delegation so
			// downloads aren't subject to unauthenticated GitHub rate limits.
			GitHubAuthToken: cfg.AuthToken.String(),
		}
	})
}

// SkillsShRegistryConfig configures the skills.sh registry.
type SkillsShRegistryConfig struct {
	Enabled         bool
	BaseURL         string // skills.sh API base; defaults to https://skills.sh
	GitHubAuthToken string // optional token for the GitHub install/meta delegate
}

func (c SkillsShRegistryConfig) IsEnabled() bool { return c.Enabled }

func (c SkillsShRegistryConfig) BuildRegistry() SkillRegistry {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = defaultSkillsShBaseURL
	}

	// skills.sh skills are GitHub repos: delegate install/metadata to a GitHub
	// registry so installs get full extraction, atomic placement, and the same
	// workspace/skills/<dir> layout as a direct GitHub install.
	delegate := GitHubRegistryConfig{
		Enabled:   true,
		BaseURL:   defaultSkillsShGitHubBaseURL,
		AuthToken: c.GitHubAuthToken,
	}.BuildRegistry()

	github, _ := delegate.(*GitHubRegistry)
	if github == nil {
		slog.Warn("skillssh registry: failed to build github delegate; installs will be unavailable")
	}

	return &SkillsShRegistry{
		baseURL: base,
		github:  github,
		client:  &http.Client{Timeout: skillsShRequestTimeout},
	}
}

// SkillsShRegistry searches the skills.sh index and installs via GitHub.
type SkillsShRegistry struct {
	baseURL string
	github  *GitHubRegistry
	client  *http.Client
}

func (r *SkillsShRegistry) Name() string { return "skillssh" }

// --- Search (skills.sh HTTP index) ---

type skillsShSearchResponse struct {
	Skills []skillsShSkill `json:"skills"`
}

type skillsShSkill struct {
	ID       string `json:"id"`       // skill identifier / slug
	Name     string `json:"name"`     // display name
	Installs int64  `json:"installs"` // popularity signal
	Source   string `json:"source"`   // GitHub owner/repo install target
}

func (r *SkillsShRegistry) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	u, err := url.Parse(r.baseURL + skillsShSearchPath)
	if err != nil {
		return nil, fmt.Errorf("invalid skills.sh base url: %w", err)
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("limit", fmt.Sprintf("%d", limit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, skillsShMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read skills.sh search response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("skills.sh search failed: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var parsed skillsShSearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse skills.sh search response: %w", err)
	}

	results := make([]SearchResult, 0, len(parsed.Skills))
	for _, s := range parsed.Skills {
		// Prefer the GitHub source as the install slug; fall back to the id.
		slug := strings.TrimSpace(s.Source)
		if slug == "" {
			slug = strings.TrimSpace(s.ID)
		}
		if slug == "" {
			continue
		}
		display := strings.TrimSpace(s.Name)
		if display == "" {
			display = slug
		}
		results = append(results, SearchResult{
			// Rank by install popularity, log-scaled so very large install
			// counts don't completely swamp the other registries when the
			// RegistryManager merges and sorts results by score.
			Score:        math.Log10(float64(s.Installs) + 1),
			Slug:         slug,
			DisplayName:  display,
			Summary:      fmt.Sprintf("%d installs (skills.sh)", s.Installs),
			RegistryName: r.Name(),
		})
	}
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// --- Install / metadata: delegated to GitHub (skills.sh skills are GitHub repos) ---

func (r *SkillsShRegistry) ResolveInstallDirName(target string) (string, error) {
	if r.github == nil {
		return "", fmt.Errorf("skillssh registry: github delegate unavailable")
	}
	return r.github.ResolveInstallDirName(target)
}

func (r *SkillsShRegistry) SkillURL(slug, version string) string {
	if r.github == nil {
		return ""
	}
	return r.github.SkillURL(slug, version)
}

func (r *SkillsShRegistry) GetSkillMeta(ctx context.Context, slug string) (*SkillMeta, error) {
	if r.github == nil {
		return nil, fmt.Errorf("skillssh registry: github delegate unavailable")
	}
	meta, err := r.github.GetSkillMeta(ctx, slug)
	if err != nil {
		return nil, err
	}
	if meta != nil {
		meta.RegistryName = r.Name()
	}
	return meta, nil
}

func (r *SkillsShRegistry) DownloadAndInstall(
	ctx context.Context,
	slug, version, targetDir string,
) (*InstallResult, error) {
	if r.github == nil {
		return nil, fmt.Errorf("skillssh registry: github delegate unavailable")
	}
	return r.github.DownloadAndInstall(ctx, slug, version, targetDir)
}

// NormalizeInstallTarget canonicalizes a skills.sh slug the same way the GitHub
// registry does (owner/repo[/subpath]) so install origin metadata is stable.
func (r *SkillsShRegistry) NormalizeInstallTarget(target string) string {
	if r.github == nil {
		return target
	}
	return r.github.NormalizeInstallTarget(target)
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/skills"
	"github.com/sipeed/picoclaw/pkg/utils"
)

// SkillSearchToolName is the on-demand local-skill discovery tool. It exists so
// the full skill catalog can be deferred out of the system prompt (kept lean),
// mirroring the MCP tool_search deferral: the agent describes what it needs and
// gets back the relevant installed skills instead of every skill's summary
// riding in every prompt.
const SkillSearchToolName = "skill_search"

// skillLister is the subset of *skills.SkillsLoader the tool needs (eases tests).
type skillLister interface {
	ListSkills() []skills.SkillInfo
}

// SkillSearchTool ranks the assistant's installed skills against a natural
// language query using BM25 over each skill's name+description.
type SkillSearchTool struct {
	loader     skillLister
	maxResults int
}

// NewSkillSearchTool builds the tool over a skills loader. maxResults <= 0
// defaults to 5.
func NewSkillSearchTool(loader skillLister, maxResults int) *SkillSearchTool {
	if maxResults <= 0 {
		maxResults = 5
	}
	return &SkillSearchTool{loader: loader, maxResults: maxResults}
}

func (t *SkillSearchTool) Name() string { return SkillSearchToolName }

func (t *SkillSearchTool) Description() string {
	return "Search your installed skills on-demand with a natural-language description of the task. " +
		"Returns the matching skills' name, description, and SKILL.md location — read that file with " +
		"read_file to use the skill. Use this instead of expecting every skill to be listed upfront."
}

func (t *SkillSearchTool) PromptMetadata() PromptMetadata {
	return PromptMetadata{
		Layer:  ToolPromptLayerCapability,
		Slot:   ToolPromptSlotTooling,
		Source: ToolPromptSourceDiscovery,
	}
}

func (t *SkillSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Natural-language description of the task or capability you need a skill for.",
			},
		},
		"required": []string{"query"},
	}
}

// skillSearchDoc is the BM25 corpus document for one skill.
type skillSearchDoc struct {
	Name        string
	Description string
	Location    string
}

// skillSearchResult is the per-skill payload returned to the model.
type skillSearchResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Location    string `json:"location"`
}

func (t *SkillSearchTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return ErrorResult("Missing or invalid 'query' argument. Must be a non-empty string.")
	}

	docs := make([]skillSearchDoc, 0)
	for _, s := range t.loader.ListSkills() {
		if s.Disabled {
			continue
		}
		docs = append(docs, skillSearchDoc{Name: s.Name, Description: s.Description, Location: s.Path})
	}
	if len(docs) == 0 {
		return SilentResult("No skills are installed.")
	}

	// Name weighed above description (same field boost as the tool search): a hit
	// on the skill's own name should outrank a hit on a lookalike description.
	engine := utils.NewBM25Engine(docs, func(d skillSearchDoc) string {
		return d.Name + " " + d.Name + " " + d.Description
	})
	ranked := engine.Search(query, t.maxResults)
	if len(ranked) == 0 {
		return SilentResult("No skills found matching the query.")
	}

	results := make([]skillSearchResult, len(ranked))
	for i, r := range ranked {
		results[i] = skillSearchResult{Name: r.Document.Name, Description: r.Document.Description, Location: r.Document.Location}
	}
	logger.InfoCF("discovery", "skill_search completed", map[string]any{"query": query, "results": len(results)})

	body, err := json.Marshal(results)
	if err != nil {
		return ErrorResult("Failed to format skill search results: " + err.Error())
	}
	msg := fmt.Sprintf(
		"Found %d skill(s):\n%s\n\nRead a skill's location with read_file to use it.",
		len(results), string(body),
	)
	return SilentResult(msg)
}

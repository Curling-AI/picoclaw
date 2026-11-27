package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/skills"
)

type LLMDraftGenerator struct {
	provider providers.LLMProvider
	model    string
	fallback DraftGenerator
}

// ErrDraftUnavailable says the model gave no usable draft: the pattern stays
// ready for a later run. The heuristic draft is not a stand-in for the model's,
// because it only points at the skills the tasks used.
var ErrDraftUnavailable = errors.New("evolution: the model gave no usable skill draft")

type llmDraftResponse struct {
	TargetSkillName    string     `json:"target_skill_name"`
	DraftType          string     `json:"draft_type"`
	ChangeKind         string     `json:"change_kind"`
	HumanSummary       string     `json:"human_summary"`
	IntendedUseCases   stringList `json:"intended_use_cases"`
	PreferredEntryPath stringList `json:"preferred_entry_path"`
	AvoidPatterns      stringList `json:"avoid_patterns"`
	BodyOrPatch        string     `json:"body_or_patch"`
}

// stringList takes a JSON array of strings or, as models sometimes answer for
// a one-item list, a single string.
type stringList []string

func (l *stringList) UnmarshalJSON(data []byte) error {
	var many []string
	if err := json.Unmarshal(data, &many); err == nil {
		*l = many
		return nil
	}
	var one string
	if err := json.Unmarshal(data, &one); err != nil {
		return err
	}
	*l = nil
	if strings.TrimSpace(one) != "" {
		*l = []string{one}
	}
	return nil
}

func NewLLMDraftGenerator(provider providers.LLMProvider, model string, fallback DraftGenerator) *LLMDraftGenerator {
	return &LLMDraftGenerator{
		provider: provider,
		model:    strings.TrimSpace(model),
		fallback: fallback,
	}
}

func (g *LLMDraftGenerator) GenerateDraft(
	ctx context.Context,
	rule LearningRecord,
	matches []skills.SkillInfo,
) (SkillDraft, error) {
	return g.GenerateDraftWithEvidence(ctx, rule, matches, DraftEvidence{})
}

func (g *LLMDraftGenerator) GenerateDraftWithEvidence(
	ctx context.Context,
	rule LearningRecord,
	matches []skills.SkillInfo,
	evidence DraftEvidence,
) (SkillDraft, error) {
	rule = enrichRuleWithDraftEvidence(rule, evidence)
	if g == nil || g.provider == nil {
		return g.generateFallback(ctx, rule, matches, evidence)
	}

	model := g.model
	if model == "" {
		model = strings.TrimSpace(g.provider.GetDefaultModel())
	}
	if model == "" {
		return g.generateFallback(ctx, rule, matches, evidence)
	}

	callCtx, cancel := withLLMCallTimeout(ctx, llmDraftGenerationTimeout())
	defer cancel()
	resp, err := g.provider.Chat(callCtx, []providers.Message{
		{
			Role:    "system",
			Content: "Return exactly one JSON object for a skill draft. Do not use markdown fences.",
		},
		{
			Role:    "user",
			Content: g.buildPrompt(rule, matches, evidence),
		},
	}, nil, model, map[string]any{"temperature": 0.2})
	if isNoCreditError(err) {
		return SkillDraft{}, noCreditError(err)
	}
	// A canceled run (Close on every sleep and config reload) is not this step
	// failing: falling back would persist a heuristic answer as the model's.
	// The run's ctx, not callCtx, so a per-call timeout keeps its fallback.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return SkillDraft{}, ctxErr
	}
	if err != nil {
		return SkillDraft{}, fmt.Errorf("%w: %w", ErrDraftUnavailable, err)
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return SkillDraft{}, fmt.Errorf("%w: empty answer", ErrDraftUnavailable)
	}

	draft, ok := parseLLMDraft(strings.TrimSpace(resp.Content))
	if !ok {
		return SkillDraft{}, fmt.Errorf("%w: the answer is not a draft object", ErrDraftUnavailable)
	}
	if findings := ValidateDraft(draft); len(findings) > 0 {
		return SkillDraft{}, fmt.Errorf("%w: %s", ErrDraftUnavailable, strings.Join(findings, "; "))
	}

	return draft, nil
}

func (g *LLMDraftGenerator) generateFallback(
	ctx context.Context,
	rule LearningRecord,
	matches []skills.SkillInfo,
	evidence DraftEvidence,
) (SkillDraft, error) {
	if g == nil || g.fallback == nil {
		return SkillDraft{}, nil
	}
	if generator, ok := g.fallback.(EvidenceAwareDraftGenerator); ok {
		return generator.GenerateDraftWithEvidence(ctx, rule, matches, evidence)
	}
	return g.fallback.GenerateDraft(ctx, rule, matches)
}

func (g *LLMDraftGenerator) buildPrompt(
	rule LearningRecord,
	matches []skills.SkillInfo,
	evidence DraftEvidence,
) string {
	return strings.Join([]string{
		"Generate a skill draft JSON object with these required string fields:",
		`target_skill_name, draft_type, change_kind, human_summary, body_or_patch.`,
		"Optional array fields: intended_use_cases, preferred_entry_path, avoid_patterns.",
		"",
		"Allowed values:",
		"- draft_type: workflow | shortcut",
		"- change_kind: create | append | replace | merge",
		"- target_skill_name: lowercase hyphenated skill name that describes the functional purpose; it must not be numeric-only",
		"",
		"Rule summary: " + strings.TrimSpace(rule.Summary),
		"Winning path: " + joinOrFallback(rule.WinningPath, "none"),
		"Late-added successful skills: " + joinOrFallback(rule.LateAddedSkills, "none"),
		"Final snapshot trigger: " + fallbackString(rule.FinalSnapshotTrigger, "none"),
		fmt.Sprintf("Event count: %d", rule.EventCount),
		fmt.Sprintf("Success rate: %.2f", rule.SuccessRate),
		"Matched skill refs: " + summarizeSkillMatches(matches),
		"Matched skill names: " + joinOrFallback(rule.MatchedSkillNames, "none"),
		"Source task evidence:",
		summarizeDraftTaskEvidence(evidence),
		"Matched skill content excerpts:",
		summarizeMatchedSkillExcerpts(matches),
		"",
		combinedSkillGuidance(rule),
		skillDraftPromptText(),
	}, "\n")
}

func summarizeDraftTaskEvidence(evidence DraftEvidence) string {
	if len(evidence.TaskRecords) == 0 {
		return "none"
	}
	lines := make([]string, 0, minInt(len(evidence.TaskRecords), 5))
	for i, task := range evidence.TaskRecords {
		if i >= 5 {
			break
		}
		parts := []string{
			"- id: " + fallbackString(task.ID, "unknown"),
			"  summary: " + fallbackString(task.Summary, "none"),
			"  final_output_excerpt: " + fallbackString(summarizeText(task.FinalOutput, 700), "none"),
			"  used_skill_names: " + joinOrFallback(task.UsedSkillNames, "none"),
			"  tool_steps:" + summarizeTaskSteps(task.ToolExecutions),
		}
		lines = append(lines, strings.Join(parts, "\n"))
	}
	return strings.Join(lines, "\n")
}

// draftStepsPerTask bounds the tool steps the draft prompt shows per task.
const draftStepsPerTask = 16

// summarizeTaskSteps lists a task's tool steps in order, each with what it
// did and whether it worked: the way the task got done, which the skill's
// procedure follows.
func summarizeTaskSteps(steps []ToolExecutionRecord) string {
	if len(steps) == 0 {
		return " none"
	}
	steps = steps[max(0, len(steps)-draftStepsPerTask):]
	lines := make([]string, 0, len(steps))
	for i, step := range steps {
		line := fmt.Sprintf("    %d. %s", i+1, step.Name)
		if step.Argument != "" {
			line += ": " + step.Argument
		}
		if step.Success {
			line += " -> ok"
		} else {
			line += " -> failed"
			if step.ErrorSummary != "" {
				line += ": " + summarizeText(step.ErrorSummary, 160)
			}
		}
		lines = append(lines, line)
	}
	return "\n" + strings.Join(lines, "\n")
}

func combinedSkillGuidance(rule LearningRecord) string {
	if target := inferCombinedSkillName(rule); target != "" {
		return strings.Join([]string{
			"This rule represents a stable multi-step successful path.",
			"Prefer creating a new combined shortcut skill instead of modifying one component skill.",
			"Suggested target skill name: " + target,
		}, "\n")
	}
	return "Prefer updating an existing skill only when the learned pattern clearly belongs inside that single skill."
}

func parseLLMDraft(content string) (SkillDraft, bool) {
	normalized := strings.TrimSpace(content)
	normalized = strings.TrimPrefix(normalized, "```json")
	normalized = strings.TrimPrefix(normalized, "```")
	normalized = strings.TrimSuffix(normalized, "```")
	normalized = strings.TrimSpace(normalized)

	var payload llmDraftResponse
	if err := json.Unmarshal([]byte(normalized), &payload); err != nil {
		return SkillDraft{}, false
	}

	draft := SkillDraft{
		TargetSkillName:    strings.TrimSpace(payload.TargetSkillName),
		DraftType:          DraftType(strings.TrimSpace(payload.DraftType)),
		ChangeKind:         ChangeKind(strings.TrimSpace(payload.ChangeKind)),
		HumanSummary:       strings.TrimSpace(payload.HumanSummary),
		IntendedUseCases:   append([]string(nil), payload.IntendedUseCases...),
		PreferredEntryPath: append([]string(nil), payload.PreferredEntryPath...),
		AvoidPatterns:      append([]string(nil), payload.AvoidPatterns...),
		BodyOrPatch:        strings.TrimSpace(payload.BodyOrPatch),
	}
	return draft, true
}

func summarizeSkillMatches(matches []skills.SkillInfo) string {
	if len(matches) == 0 {
		return "none"
	}

	parts := make([]string, 0, len(matches))
	for _, match := range matches {
		part := strings.TrimSpace(match.Name)
		if desc := strings.TrimSpace(match.Description); desc != "" {
			part += ": " + desc
		}
		if path := strings.TrimSpace(match.Path); path != "" {
			part += " (" + path + ")"
		}
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

func joinOrFallback(parts []string, fallback string) string {
	if len(parts) == 0 {
		return fallback
	}
	return strings.Join(parts, " -> ")
}

func fallbackString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

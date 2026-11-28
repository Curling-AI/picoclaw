package evolution_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/evolution"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/skills"
)

type recordingDraftGenerator struct {
	draft evolution.SkillDraft
	err   error
	calls int
}

func (g *recordingDraftGenerator) GenerateDraft(
	_ context.Context,
	_ evolution.LearningRecord,
	_ []skills.SkillInfo,
) (evolution.SkillDraft, error) {
	g.calls++
	return g.draft, g.err
}

type llmDraftTestProvider struct {
	response      *providers.LLMResponse
	err           error
	defaultModel  string
	lastModel     string
	lastMessages  []providers.Message
	chatCallCount int
}

func (p *llmDraftTestProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	model string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.chatCallCount++
	p.lastModel = model
	p.lastMessages = append([]providers.Message(nil), messages...)
	return p.response, p.err
}

func (p *llmDraftTestProvider) GetDefaultModel() string {
	return p.defaultModel
}

func testLearningRule() evolution.LearningRecord {
	return evolution.LearningRecord{
		ID:                "rule-1",
		Summary:           "weather native-name path",
		EventCount:        7,
		SuccessRate:       0.86,
		WinningPath:       []string{"weather", "native-name"},
		MatchedSkillNames: []string{"weather"},
	}
}

func testSkillMatches() []skills.SkillInfo {
	return []skills.SkillInfo{
		{
			Name:        "weather",
			Path:        "/tmp/weather/SKILL.md",
			Source:      "workspace",
			Description: "Find weather details.",
		},
	}
}

func TestLLMDraftGenerator_GenerateDraft_ParsesJSONResponse(t *testing.T) {
	provider := &llmDraftTestProvider{
		defaultModel: "test-model",
		response: &providers.LLMResponse{
			Content: `{"target_skill_name":"weather","draft_type":"shortcut","change_kind":"append","human_summary":"Prefer native-name lookup first","body_or_patch":"## Start Here\nUse native-name first."}`,
		},
	}
	fallback := &recordingDraftGenerator{
		draft: evolution.SkillDraft{TargetSkillName: "fallback"},
	}
	generator := evolution.NewLLMDraftGenerator(provider, "", fallback)

	draft, err := generator.GenerateDraft(context.Background(), testLearningRule(), testSkillMatches())
	if err != nil {
		t.Fatalf("GenerateDraft: %v", err)
	}

	if provider.chatCallCount != 1 {
		t.Fatalf("chatCallCount = %d, want 1", provider.chatCallCount)
	}
	if provider.lastModel != "test-model" {
		t.Fatalf("lastModel = %q, want test-model", provider.lastModel)
	}
	if len(provider.lastMessages) == 0 {
		t.Fatal("expected prompt messages")
	}
	if fallback.calls != 0 {
		t.Fatalf("fallback.calls = %d, want 0", fallback.calls)
	}
	if draft.TargetSkillName != "weather" {
		t.Fatalf("TargetSkillName = %q, want weather", draft.TargetSkillName)
	}
	if draft.DraftType != evolution.DraftTypeShortcut {
		t.Fatalf("DraftType = %q, want %q", draft.DraftType, evolution.DraftTypeShortcut)
	}
	if draft.ChangeKind != evolution.ChangeKindAppend {
		t.Fatalf("ChangeKind = %q, want %q", draft.ChangeKind, evolution.ChangeKindAppend)
	}
	if draft.HumanSummary == "" || draft.BodyOrPatch == "" {
		t.Fatal("expected non-empty draft content")
	}
}

func TestLLMDraftGenerator_BuildPromptIncludesMatchedSkillContent(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, "skills", "three-one-theorem", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(
		skillPath,
		[]byte(
			"---\nname: three-one-theorem\ndescription: Add 31 then delegate\n---\n# Three One\nAdd 31 to the input, then continue with the next theorem.\n",
		),
		0o644,
	); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	provider := &llmDraftTestProvider{
		defaultModel: "test-model",
		response: &providers.LLMResponse{
			Content: `{"target_skill_name":"calculate-100-via-theorems","draft_type":"shortcut","change_kind":"create","human_summary":"Combine theorem chain","body_or_patch":"## Start Here\nAdd 31, then continue."}`,
		},
	}
	generator := evolution.NewLLMDraftGenerator(provider, "", &recordingDraftGenerator{})

	_, err := generator.GenerateDraft(context.Background(), evolution.LearningRecord{
		ID:          "rule-1",
		Summary:     "calculate 100",
		WinningPath: []string{"three-one-theorem", "four-two-theorem"},
		EventCount:  2,
		SuccessRate: 1,
	}, []skills.SkillInfo{{
		Name:        "three-one-theorem",
		Path:        skillPath,
		Source:      "workspace",
		Description: "Add 31 then delegate",
	}})
	if err != nil {
		t.Fatalf("GenerateDraft: %v", err)
	}
	if len(provider.lastMessages) < 2 {
		t.Fatal("expected user prompt")
	}
	prompt := provider.lastMessages[1].Content
	if !strings.Contains(prompt, "Matched skill content excerpts") {
		t.Fatalf("prompt missing content section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Add 31 to the input") {
		t.Fatalf("prompt missing matched skill body:\n%s", prompt)
	}
	if !strings.Contains(prompt, "summarize the functional purpose and result") {
		t.Fatalf("prompt missing synthesis instruction:\n%s", prompt)
	}
	if !strings.Contains(prompt, "complete SKILL.md file with exactly two parts") {
		t.Fatalf("prompt missing complete skill instruction:\n%s", prompt)
	}
	if !strings.Contains(prompt, "The YAML frontmatter must contain only name and description fields") {
		t.Fatalf("prompt missing frontmatter instruction:\n%s", prompt)
	}
	if !strings.Contains(
		prompt,
		"The description field must and only describe what this skill can do and when to use it",
	) {
		t.Fatalf("prompt missing description field instruction:\n%s", prompt)
	}
	if !strings.Contains(
		prompt,
		"The deployable Markdown body should only contain what the skill is useful for and how to use it",
	) {
		t.Fatalf("prompt missing deployable body scope instruction:\n%s", prompt)
	}
	// The body is the skill as the assistant will read it, the shortest path the
	// tasks proved: review notes and past results leaked into the applied skill,
	// and steps to find or inspect the inputs again made the next task slower.
	for _, want := range []string{
		"body_or_patch is the skill the assistant reads when it uses it",
		"No notes for reviewers, no provenance, no results of past runs",
		"Give the shortest path the tasks proved to the final answer",
		"reuse a script or file the tasks created",
		"so it needs no step to list the files or check their headers",
		"Keep every rule the person set or corrected in the tasks",
		"do not send the assistant to read another skill for it",
		"in one short clause, what it is not for when a request about the same inputs could look alike",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, gone := range []string{"internal draft and review artifact", "detailed step-by-step instructions"} {
		if strings.Contains(prompt, gone) {
			t.Fatalf("prompt still asks for %q:\n%s", gone, prompt)
		}
	}
	if !strings.Contains(prompt, "do not copy or directly include other skills' instructions") {
		t.Fatalf("prompt missing no-copy instruction:\n%s", prompt)
	}
}

func TestLLMDraftGenerator_BuildPromptIncludesTaskEvidence(t *testing.T) {
	provider := &llmDraftTestProvider{
		defaultModel: "test-model",
		response: &providers.LLMResponse{
			Content: `{"target_skill_name":"calculate-with-three-one-theorem","draft_type":"shortcut","change_kind":"create","human_summary":"Calculate using theorem chain","body_or_patch":"---\nname: calculate-with-three-one-theorem\ndescription: Calculate with theorem chain.\n---\n# Calculate With Three One Theorem\n\n## Procedure\nAdd 31, add 42, then subtract 53."}`,
		},
	}
	generator := evolution.NewLLMDraftGenerator(provider, "", &recordingDraftGenerator{})

	_, err := generator.GenerateDraftWithEvidence(context.Background(), evolution.LearningRecord{
		ID:      "rule-1",
		Label:   "calculate-with-three-one-theorem",
		Summary: "调用三一定理计算",
	}, nil, evolution.DraftEvidence{
		TaskRecords: []evolution.LearningRecord{
			{
				ID:             "main-turn-6",
				Summary:        "调用三一定理计算100",
				FinalOutput:    "100 + 31 = 131; 131 + 42 = 173; 173 - 53 = 120",
				UsedSkillNames: []string{"three-one-theorem", "four-two-theorem", "five-three-theorem"},
			},
		},
	})
	if err != nil {
		t.Fatalf("GenerateDraftWithEvidence: %v", err)
	}
	if len(provider.lastMessages) < 2 {
		t.Fatal("expected user prompt")
	}
	prompt := provider.lastMessages[1].Content
	for _, want := range []string{
		"Source task evidence",
		"main-turn-6",
		"调用三一定理计算100",
		"100 + 31 = 131",
		"three-one-theorem -> four-two-theorem -> five-three-theorem",
		"directly usable by a future agent",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestLLMDraftGenerator_GenerateDraft_PrefersExplicitModelIDOverProviderDefault(t *testing.T) {
	provider := &llmDraftTestProvider{
		defaultModel: "provider-default-model",
		response: &providers.LLMResponse{
			Content: `{"target_skill_name":"weather","draft_type":"shortcut","change_kind":"append","human_summary":"Prefer native-name lookup first","body_or_patch":"## Start Here\nUse native-name first."}`,
		},
	}
	generator := evolution.NewLLMDraftGenerator(provider, "explicit-model-id", &recordingDraftGenerator{})

	_, err := generator.GenerateDraft(context.Background(), testLearningRule(), testSkillMatches())
	if err != nil {
		t.Fatalf("GenerateDraft: %v", err)
	}
	if provider.lastModel != "explicit-model-id" {
		t.Fatalf("lastModel = %q, want explicit-model-id", provider.lastModel)
	}
}

// When the model gives no usable draft (it fails, answers nothing, or answers
// something that is not a valid draft) there is no skill this run: the
// heuristic draft only points at the skills the tasks used, and applying it
// made the next task slower.
func TestLLMDraftGenerator_GenerateDraft_GivesNoDraftWhenTheModelGivesNone(t *testing.T) {
	cases := map[string]*llmDraftTestProvider{
		"provider error": {defaultModel: "test-model", err: errors.New("provider unavailable")},
		"call timeout":   {defaultModel: "test-model", err: context.DeadlineExceeded},
		"invalid json":   {defaultModel: "test-model", response: &providers.LLMResponse{Content: `not-json`}},
		"empty answer":   {defaultModel: "test-model", response: &providers.LLMResponse{Content: ``}},
		"numeric-only name": {defaultModel: "test-model", response: &providers.LLMResponse{
			Content: `{"target_skill_name":"100","draft_type":"shortcut","change_kind":"create","human_summary":"Calculate 100","body_or_patch":"## Start Here\nCalculate 100."}`,
		}},
	}
	for name, provider := range cases {
		t.Run(name, func(t *testing.T) {
			fallback := &recordingDraftGenerator{draft: evolution.SkillDraft{
				TargetSkillName: "weather-fallback",
				DraftType:       evolution.DraftTypeWorkflow,
				ChangeKind:      evolution.ChangeKindCreate,
				HumanSummary:    "fallback summary",
				BodyOrPatch:     "fallback body",
			}}
			generator := evolution.NewLLMDraftGenerator(provider, "", fallback)

			draft, err := generator.GenerateDraft(context.Background(), testLearningRule(), testSkillMatches())
			if !errors.Is(err, evolution.ErrDraftUnavailable) {
				t.Fatalf("err = %v, want ErrDraftUnavailable", err)
			}
			if fallback.calls != 0 || draft.TargetSkillName != "" {
				t.Fatalf("fallback.calls = %d, draft = %q: want no draft", fallback.calls, draft.TargetSkillName)
			}
		})
	}
}

// A model sometimes answers a one-item list as plain text; the draft is still
// the model's, not lost to a parse error.
func TestLLMDraftGenerator_GenerateDraft_TakesAListGivenAsText(t *testing.T) {
	generator := evolution.NewLLMDraftGenerator(&llmDraftTestProvider{
		defaultModel: "test-model",
		response: &providers.LLMResponse{Content: `{"target_skill_name":"ranking-vendas","draft_type":"workflow",` +
			`"change_kind":"create","human_summary":"ranking","body_or_patch":"## Passos\nSomar por categoria.",` +
			`"intended_use_cases":["ranking mensal"],"preferred_entry_path":"exec","avoid_patterns":null}`},
	}, "", &recordingDraftGenerator{})

	draft, err := generator.GenerateDraft(context.Background(), testLearningRule(), testSkillMatches())
	if err != nil {
		t.Fatalf("GenerateDraft: %v", err)
	}
	if draft.TargetSkillName != "ranking-vendas" || len(draft.PreferredEntryPath) != 1 ||
		draft.PreferredEntryPath[0] != "exec" {
		t.Fatalf("draft = %+v, want the model's draft with preferred_entry_path [exec]", draft)
	}
}

// The draft sees how each task got done: its tool steps in order, what each
// did, the failure before the step that worked, and the instruction to write
// the procedure from them, in the person's language, without extras.
func TestLLMDraftGenerator_BuildPromptShowsTheTaskSteps(t *testing.T) {
	provider := &llmDraftTestProvider{
		defaultModel: "test-model",
		response: &providers.LLMResponse{
			Content: `{"target_skill_name":"ranking-vendas-categoria","draft_type":"workflow","change_kind":"create","human_summary":"Ranking","body_or_patch":"---\nname: ranking-vendas-categoria\ndescription: Ranking.\n---\n# Ranking\n\n## Passos\n1. Ler o CSV."}`,
		},
	}
	generator := evolution.NewLLMDraftGenerator(provider, "", &recordingDraftGenerator{})
	_, err := generator.GenerateDraftWithEvidence(context.Background(), evolution.LearningRecord{
		ID: "rule-1", Label: "ranking-vendas-categoria", Summary: "ranking de vendas por categoria",
	}, nil, evolution.DraftEvidence{
		TaskRecords: []evolution.LearningRecord{{
			ID:      "main-turn-3",
			Summary: "faz o ranking de vendas por categoria de agosto",
			ToolExecutions: []evolution.ToolExecutionRecord{
				{Name: "read_file", Argument: "artifacts/vendas-agosto.csv", Success: true},
				{
					Name:         "exec",
					Argument:     "python3 -c 'import pandas'",
					Success:      false,
					ErrorSummary: "No module named pandas",
				},
				{Name: "exec", Argument: "python3 ranking.py artifacts/vendas-agosto.csv", Success: true},
			},
		}},
	})
	if err != nil {
		t.Fatalf("GenerateDraftWithEvidence: %v", err)
	}
	prompt := provider.lastMessages[1].Content
	for _, want := range []string{
		"tool_steps:",
		"1. read_file: artifacts/vendas-agosto.csv -> ok",
		"2. exec: python3 -c 'import pandas' -> failed: No module named pandas",
		"3. exec: python3 ranking.py artifacts/vendas-agosto.csv -> ok",
		"Write the procedure from the tool_steps that worked",
		"no output format, chart, card, extra column or extra step",
		"in the language of the person's requests in the source tasks",
		"say what result the skill produces",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

// The prompt shows the most recent source tasks, the ones with their steps
// recorded, and says a background step's success is not the command's.
func TestLLMDraftGenerator_BuildPromptShowsRecentTasksAndBackgroundSteps(t *testing.T) {
	tasks := make([]evolution.LearningRecord, 0, 7)
	for i := 1; i <= 7; i++ {
		task := evolution.LearningRecord{ID: fmt.Sprintf("task-%d", i), Summary: "ranking de vendas"}
		if i > 2 {
			task.ToolExecutions = []evolution.ToolExecutionRecord{
				{Name: "exec", Success: true, Background: true, Argument: "python3 import.py"},
				{Name: "exec", Success: true, Argument: "python3 ranking.py"},
			}
		}
		tasks = append(tasks, task)
	}
	provider := &llmDraftTestProvider{defaultModel: "test-model", response: &providers.LLMResponse{Content: `not-json`}}
	generator := evolution.NewLLMDraftGenerator(provider, "", &recordingDraftGenerator{})
	_, _ = generator.GenerateDraftWithEvidence(
		context.Background(),
		testLearningRule(),
		nil,
		evolution.DraftEvidence{TaskRecords: tasks},
	)

	prompt := provider.lastMessages[len(provider.lastMessages)-1].Content
	for _, want := range []string{"task-3", "task-7", "python3 import.py -> ran in the background, its result is not in the record", "python3 ranking.py -> ok"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, old := range []string{"id: task-1\n", "id: task-2\n"} {
		if strings.Contains(prompt, old) {
			t.Fatalf("prompt kept an old task %q over a recent one:\n%s", old, prompt)
		}
	}
}

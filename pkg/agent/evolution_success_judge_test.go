package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/evolution"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// The cold path asks the decision model when the deployment has one and its
// gateway answers decisions, and the cheap chat model otherwise.
func TestEvolutionSuccessJudgeFollowsTheDecisionModel(t *testing.T) {
	withModel := config.DefaultConfig()
	withModel.Agents.Defaults.DecisionModel = testDecisionModel
	decider := &judgedProvider{scriptedProvider: &scriptedProvider{}}

	if _, ok := evolutionSuccessJudge(withModel, decider, "cheap").(*evolution.DecisionTaskSuccessJudge); !ok {
		t.Error("with a decision model and a deciding gateway, the success judge is not the decision judge")
	}
	// NewAgentLoop and ReloadProviderAndConfig hand the bridge the metering wrapper.
	if _, ok := evolutionSuccessJudge(withModel, newObservedProvider(decider, "evolution"), "cheap").(*evolution.DecisionTaskSuccessJudge); !ok {
		t.Error("behind the metering wrapper, the success judge is not the decision judge")
	}
	if _, ok := evolutionSuccessJudge(withModel, newObservedProvider(&scriptedProvider{}, "evolution"), "cheap").(*evolution.LLMTaskSuccessJudge); !ok {
		t.Error("behind the metering wrapper, a gateway that cannot decide left the chat model")
	}
	if _, ok := evolutionSuccessJudge(config.DefaultConfig(), decider, "cheap").(*evolution.LLMTaskSuccessJudge); !ok {
		t.Error("without a decision model, the success judge left the chat model")
	}
	if _, ok := evolutionSuccessJudge(withModel, &scriptedProvider{}, "cheap").(*evolution.LLMTaskSuccessJudge); !ok {
		t.Error("with a gateway that cannot decide, the success judge left the chat model")
	}
}

// A model that needs the simple tool schemas gets its provider behind a schema
// transform, and the bridge gets that behind the metering wrapper: the judge
// follows what the provider inside can do. A chat-only one behind both used to
// get the decision judge, whose every call failed into the heuristic that
// accepts any completed turn.
func TestEvolutionSuccessJudgeSeesThroughTheSchemaTransform(t *testing.T) {
	withModel := config.DefaultConfig()
	withModel.Agents.Defaults.DecisionModel = testDecisionModel
	built := func(model *config.ModelConfig) providers.LLMProvider {
		model.ToolSchemaTransform = "simple"
		provider, _, err := providers.CreateProviderFromConfig(model)
		if err != nil {
			t.Fatalf("CreateProviderFromConfig(%s) error = %v", model.ModelName, err)
		}
		return newObservedProvider(provider, "evolution")
	}

	gateway := &config.ModelConfig{ModelName: "gateway", Model: "openai/gpt-4o", APIBase: "http://127.0.0.1:9/v1"}
	gateway.SetAPIKey("test-key")
	if _, ok := evolutionSuccessJudge(withModel, built(gateway), "cheap").(*evolution.DecisionTaskSuccessJudge); !ok {
		t.Error("a deciding gateway behind the schema transform is not judged by the decision model")
	}
	chatOnly := &config.ModelConfig{
		ModelName: "cli", Provider: "claude-cli", Model: "claude-sonnet-4.6", Workspace: t.TempDir(),
	}
	if _, ok := evolutionSuccessJudge(withModel, built(chatOnly), "cheap").(*evolution.LLMTaskSuccessJudge); !ok {
		t.Error("a chat-only provider behind the schema transform left the chat judge")
	}
}

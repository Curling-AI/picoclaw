package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/evolution"
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

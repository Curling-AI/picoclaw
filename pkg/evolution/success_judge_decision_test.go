package evolution_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/evolution"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// scriptedDecider is the gateway's Decisions API: one answer per ask, in order.
type scriptedDecider struct {
	answers []decision
	asked   []string
	models  []string
	names   []string
}

type decision struct {
	p       float64
	refused bool
	err     error
}

func (d *scriptedDecider) Decide(
	_ context.Context,
	model, input string,
	questions []providers.DecisionQuestion,
) (map[string]float64, error) {
	d.asked = append(d.asked, input)
	d.models = append(d.models, model)
	d.names = append(d.names, questions[0].Name)
	a := d.answers[len(d.asked)-1]
	switch {
	case a.err != nil:
		return nil, a.err
	case a.refused:
		return map[string]float64{}, nil
	}
	return map[string]float64{questions[0].Name: a.p}, nil
}

func completedRecord() evolution.LearningRecord {
	ok := true
	return evolution.LearningRecord{
		ID:          "task-1",
		UserGoal:    "monte o relatório de vendas de outubro",
		Summary:     "relatório de vendas",
		FinalOutput: "Relatório pronto: outubro fechou em R$ 42 mil, 12% acima de setembro.",
		Success:     &ok,
	}
}

func TestDecisionTaskSuccessJudge_DecidesFromTheRecord(t *testing.T) {
	decider := &scriptedDecider{answers: []decision{{p: 0.93}, {p: 0.05}}}
	judge := evolution.NewDecisionTaskSuccessJudge(
		decider,
		"typesafe-ai/jev-1.14.0",
		&evolution.HeuristicSuccessJudge{},
	)

	got, err := judge.JudgeTaskRecord(context.Background(), completedRecord())
	if err != nil || !got.Success || got.Reason != "decision p=0.93" {
		t.Fatalf("decision = %+v, %v; want success with the decision model's probability", got, err)
	}
	if decider.models[0] != "typesafe-ai/jev-1.14.0" || decider.names[0] != "task_succeeded" {
		t.Errorf("asked %q about %q, want the decision model and the success question",
			decider.models[0], decider.names[0])
	}
	for _, want := range []string{"monte o relatório de vendas de outubro", "outubro fechou em R$ 42 mil"} {
		if !strings.Contains(decider.asked[0], want) {
			t.Errorf("the judge did not read %q", want)
		}
	}

	got, err = judge.JudgeTaskRecord(context.Background(), completedRecord())
	if err != nil || got.Success || got.Reason != "decision p=0.05" {
		t.Fatalf("decision = %+v, %v; want a rejection, even where the heuristic would accept", got, err)
	}
}

// Doubt, a refusal, an error or no decider fall back to the heuristic judge,
// which turns down a record that did not complete.
func TestDecisionTaskSuccessJudge_FallsBackWithoutAClearAnswer(t *testing.T) {
	record := completedRecord()
	record.Success = nil
	for name, judge := range map[string]*evolution.DecisionTaskSuccessJudge{
		"doubt":      evolution.NewDecisionTaskSuccessJudge(&scriptedDecider{answers: []decision{{p: 0.5}}}, "m", nil),
		"refused":    evolution.NewDecisionTaskSuccessJudge(&scriptedDecider{answers: []decision{{refused: true}}}, "m", nil),
		"error":      evolution.NewDecisionTaskSuccessJudge(&scriptedDecider{answers: []decision{{err: errGatewayDown}}}, "m", nil),
		"no decider": evolution.NewDecisionTaskSuccessJudge(nil, "m", nil),
		"no model":   evolution.NewDecisionTaskSuccessJudge(&scriptedDecider{answers: []decision{{p: 0.93}}}, " ", nil),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := judge.JudgeTaskRecord(context.Background(), record)
			if err != nil || got.Success || got.Reason != "task not completed" {
				t.Fatalf("decision = %+v, %v; want the heuristic verdict", got, err)
			}
		})
	}
}

// Out of credit, the cold path stops instead of judging heuristically.
func TestDecisionTaskSuccessJudge_NoCreditIsReported(t *testing.T) {
	judge := evolution.NewDecisionTaskSuccessJudge(
		&scriptedDecider{answers: []decision{{err: errGatewayNoCredit}}}, "m", &evolution.HeuristicSuccessJudge{})
	if _, err := judge.JudgeTaskRecord(
		context.Background(),
		completedRecord(),
	); !errors.Is(
		err,
		evolution.ErrNoCredit,
	) {
		t.Fatalf("err = %v, want ErrNoCredit", err)
	}
}

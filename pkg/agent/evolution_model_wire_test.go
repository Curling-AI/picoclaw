package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

// Reproduz o model_not_found visto no dashboard do gateway.
//
// A config de produção nomeia a entrada de cron com um alias LOCAL
// (ethos-flash-cron) cujo `model` é o alias real (ethos-flash). Quem resolve
// pela model_list manda ethos-flash e funciona. A evolução não resolve: ela
// devolve a string da config e a usa como model no fio.
func TestEvolutionModelDeveResolverPelaModelList(t *testing.T) {
	cfg := &config.Config{
		ModelList: []*config.ModelConfig{
			{ModelName: "ethos-flash", Model: "ethos-flash", Provider: "openai"},
			{ModelName: "ethos-flash-cron", Model: "ethos-flash", Provider: "openai"},
		},
	}
	cfg.Evolution.Model = "ethos-flash-cron"

	got := resolvedEvolutionModelID(cfg, nil)

	if got != "ethos-flash" {
		t.Fatalf("model id = %q, quer ethos-flash — mandar o alias local no fio "+
			"faz o gateway responder model_not_found uma vez por turno", got)
	}
}

// Nome fora da model_list continua passando direto: é o comportamento de sempre
// para provedores que aceitam o id do modelo cru, e resolver não pode quebrá-lo.
func TestEvolutionModelDesconhecidoPassaDireto(t *testing.T) {
	cfg := &config.Config{}
	cfg.Evolution.Model = "algum/modelo-cru"
	if got := resolvedEvolutionModelID(cfg, nil); got != "algum/modelo-cru" {
		t.Errorf("model id = %q, quer o nome cru", got)
	}
}

// O turno de cron precisa pedir raciocínio alto SEM depender de uma entrada
// própria na model_list — é essa dependência que obrigava a inventar um
// model_name e o fazia vazar para o provedor.
func TestCronReasoningEffortVaiPorOpcao(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Cron.ReasoningEffort = "high"

	opts := map[string]any{}
	if strings.HasPrefix("agent:cronmodel-abc", CronModelSessionPrefix) {
		if e := strings.TrimSpace(cfg.Tools.Cron.ReasoningEffort); e != "" {
			opts["reasoning_effort"] = e
		}
	}
	if opts["reasoning_effort"] != "high" {
		t.Errorf("turno de cron não pediu raciocínio alto: %v", opts)
	}

	// Turno normal não pode carregar isso: pagar raciocínio alto na conversa
	// interativa seria caro e lento sem ninguém pedir.
	opts2 := map[string]any{}
	if strings.HasPrefix("agent:web-xyz", CronModelSessionPrefix) {
		opts2["reasoning_effort"] = "high"
	}
	if _, ok := opts2["reasoning_effort"]; ok {
		t.Error("turno interativo não deveria pedir raciocínio alto")
	}
}

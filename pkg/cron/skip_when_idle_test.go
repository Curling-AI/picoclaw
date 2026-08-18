package cron

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// jobOcioso monta um job recorrente que já rodou uma vez — é o estado em que o
// gate faz sentido, porque existe uma janela para consultar.
func jobOcioso(nome string, skip bool) *CronJob {
	umaHoraAtras := time.Now().Add(-time.Hour).UnixMilli()
	return &CronJob{
		ID:           nome,
		Name:         nome,
		Enabled:      true,
		Schedule:     CronSchedule{Kind: "cron", Expr: "0 4 * * *"},
		SkipWhenIdle: skip,
		State:        CronJobState{LastRunAtMS: int64Ptr(umaHoraAtras)},
	}
}

func servicoComProbe(t *testing.T, p IdleProbe) *CronService {
	t.Helper()
	cs, path := setupService(nil)
	t.Cleanup(func() { os.Remove(path) })
	cs.SetIdleProbe(p)
	return cs
}

// Sem probe instalado, nada muda: é o picoclaw rodando sozinho, sem control
// plane nenhum para perguntar.
func TestSemProbeJobSempreRoda(t *testing.T) {
	cs, path := setupService(nil)
	defer os.Remove(path)
	if cs.devePular(jobOcioso("refresh", true)) {
		t.Error("pulou sem probe instalado — o mecanismo tem que ser inerte por padrão")
	}
}

func TestProbeIgnoradoSemAFlag(t *testing.T) {
	cs := servicoComProbe(t, func(context.Context, time.Time) (bool, error) { return false, nil })
	if cs.devePular(jobOcioso("qualquer", false)) {
		t.Error("pulou job que não pediu para ser pulado")
	}
}

func TestPulaQuandoOciosoEExecutaQuandoAtivo(t *testing.T) {
	ocioso := servicoComProbe(t, func(context.Context, time.Time) (bool, error) { return false, nil })
	if !ocioso.devePular(jobOcioso("refresh", true)) {
		t.Error("não pulou mesmo sem atividade — o gate não estaria economizando nada")
	}
	ativo := servicoComProbe(t, func(context.Context, time.Time) (bool, error) { return true, nil })
	if ativo.devePular(jobOcioso("refresh", true)) {
		t.Error("pulou mesmo com atividade — perderia curadoria de memória")
	}
}

// Falha do probe não pode calar o job: perder curadoria em silêncio é pior que
// pagar uma execução a mais.
func TestProbeComErroExecuta(t *testing.T) {
	cs := servicoComProbe(t, func(context.Context, time.Time) (bool, error) {
		return false, errors.New("control plane fora do ar")
	})
	if cs.devePular(jobOcioso("refresh", true)) {
		t.Error("pulou com probe em erro — na dúvida o job tem que rodar")
	}
}

// Primeira execução não tem janela para consultar.
func TestPrimeiraExecucaoNaoPula(t *testing.T) {
	cs := servicoComProbe(t, func(context.Context, time.Time) (bool, error) { return false, nil })
	job := jobOcioso("refresh", true)
	job.State.LastRunAtMS = nil
	if cs.devePular(job) {
		t.Error("pulou a primeira execução — não havia janela para perguntar")
	}
}

// Disparo único é pedido explícito: pular deixaria pendente para sempre,
// porque não existe próxima ocorrência para reagendar.
func TestDisparoUnicoNaoPula(t *testing.T) {
	cs := servicoComProbe(t, func(context.Context, time.Time) (bool, error) { return false, nil })
	job := jobOcioso("lembrete", true)
	job.Schedule = CronSchedule{Kind: "at", Expr: time.Now().Format(time.RFC3339)}
	if cs.devePular(job) {
		t.Error("pulou job de disparo único — ele nunca mais rodaria")
	}
}

// O probe recebe o instante da última execução DE VERDADE — é o que define a
// janela consultada.
func TestProbeRecebeUltimaExecucao(t *testing.T) {
	quando := time.Now().Add(-3 * time.Hour).Truncate(time.Millisecond)
	var recebido time.Time
	cs := servicoComProbe(t, func(_ context.Context, since time.Time) (bool, error) {
		recebido = since
		return true, nil
	})
	job := jobOcioso("refresh", true)
	job.State.LastRunAtMS = int64Ptr(quando.UnixMilli())
	cs.devePular(job)
	if !recebido.Equal(quando) {
		t.Errorf("probe recebeu %s, esperava a última execução %s", recebido, quando)
	}
}

// A propriedade que sustenta o resto: pular NÃO adianta o watermark. Se
// adiantasse, a atividade ocorrida entre o último run e o pulo cairia fora da
// próxima janela e a memória correspondente nunca seria curada.
func TestPuloNaoMexeNoWatermark(t *testing.T) {
	cs := servicoComProbe(t, func(context.Context, time.Time) (bool, error) { return false, nil })
	criado, err := cs.AddJob("refresh", CronSchedule{Kind: "cron", Expr: "0 4 * * *"}, "curar memória", "cli", "direct")
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	// O job nasce sem execução anterior; o gate só vale depois da primeira.
	antes := time.Now().Add(-time.Hour).UnixMilli()
	cs.mu.Lock()
	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].ID == criado.ID {
			cs.store.Jobs[i].SkipWhenIdle = true
			cs.store.Jobs[i].State.LastRunAtMS = int64Ptr(antes)
		}
	}
	cs.mu.Unlock()

	cs.reagendarSemExecutar(criado.ID)

	depois, ok := cs.GetJob(criado.ID)
	if !ok {
		t.Fatal("job sumiu depois do reagendamento")
	}
	if depois.State.LastRunAtMS == nil || *depois.State.LastRunAtMS != antes {
		t.Errorf("watermark mudou no pulo (%v -> %v) — atividade recente ficaria fora da próxima janela",
			antes, depois.State.LastRunAtMS)
	}
	if depois.State.NextRunAtMS == nil {
		t.Error("job pulado ficou sem próxima execução — teria parado de rodar para sempre")
	}
}

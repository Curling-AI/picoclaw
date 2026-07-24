package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSession drops a session pair on disk with the given age.
func writeSession(t *testing.T, dir, sanitized string, age time.Duration) {
	t.Helper()
	for _, suffix := range []string{".jsonl", ".meta.json"} {
		path := filepath.Join(dir, sanitized+suffix)
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// A poda remove husks de cron vencidos e NUNCA toca em conversas — foi o
// acúmulo desses husks (7,7k arquivos num pod) que estourou o timeout da
// listagem e deixou a sidebar vazia.
func TestPruneCronRuns_RemoveVencidosPreservaConversas(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}

	writeSession(t, dir, "agent_cron-job1-antigo", 30*24*time.Hour)
	writeSession(t, dir, "agent_cronmodel-job2-antigo", 30*24*time.Hour)
	writeSession(t, dir, "agent_cron-job1-recente", time.Hour)
	writeSession(t, dir, "sk_v1_conversa", 365*24*time.Hour)
	writeSession(t, dir, "agent_main_telegram_group_x", 365*24*time.Hour)

	removed, err := store.PruneCronRuns(7*24*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("PruneCronRuns: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}

	gone := []string{"agent_cron-job1-antigo", "agent_cronmodel-job2-antigo"}
	for _, name := range gone {
		for _, suffix := range []string{".jsonl", ".meta.json"} {
			if exists(t, filepath.Join(dir, name+suffix)) {
				t.Errorf("%s%s deveria ter sido removido", name, suffix)
			}
		}
	}

	// Conversas antigas ficam: elas não são runs e não têm retenção.
	kept := []string{"agent_cron-job1-recente", "sk_v1_conversa", "agent_main_telegram_group_x"}
	for _, name := range kept {
		for _, suffix := range []string{".jsonl", ".meta.json"} {
			if !exists(t, filepath.Join(dir, name+suffix)) {
				t.Errorf("%s%s NÃO deveria ter sido removido", name, suffix)
			}
		}
	}
}

// Retenção zero/negativa desliga a poda — nada é apagado por engano.
func TestPruneCronRuns_RetencaoZeroDesliga(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	writeSession(t, dir, "agent_cron-job1-antigo", 365*24*time.Hour)

	for _, retention := range []time.Duration{0, -time.Hour} {
		removed, err := store.PruneCronRuns(retention, time.Now())
		if err != nil {
			t.Fatalf("PruneCronRuns(%v): %v", retention, err)
		}
		if removed != 0 {
			t.Errorf("retention=%v: removed = %d, want 0", retention, removed)
		}
	}
	if !exists(t, filepath.Join(dir, "agent_cron-job1-antigo.jsonl")) {
		t.Error("husk foi removido com a poda desligada")
	}
}

// Depois da poda a sessão não pode ressuscitar pelo cache de metas em memória.
func TestPruneCronRuns_EvictaCacheDeMeta(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	writeSession(t, dir, "agent_cron-job1-antigo", 30*24*time.Hour)
	store.storeCachedMeta("agent_cron-job1-antigo", SessionMeta{Key: "agent:cron-job1-antigo", Count: 3})

	if _, err := store.PruneCronRuns(7*24*time.Hour, time.Now()); err != nil {
		t.Fatalf("PruneCronRuns: %v", err)
	}

	if _, ok := store.cachedMeta("agent_cron-job1-antigo"); ok {
		t.Error("meta continuou no cache — a sessão podada reapareceria na listagem")
	}
	for _, meta := range store.ListSessionMetas() {
		if meta.Key == "agent:cron-job1-antigo" {
			t.Error("sessão podada ainda aparece em ListSessionMetas")
		}
	}
}

func TestIsCronRunFile(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"agent_cron-job-uuid.meta.json", true},
		{"agent_cronmodel-job-uuid.meta.json", true},
		{"sk_v1_abc.meta.json", false},
		{"agent_main_telegram_group_x.meta.json", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isCronRunFile(c.name); got != c.want {
			t.Errorf("isCronRunFile(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

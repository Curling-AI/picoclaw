package memory

import (
	"encoding/json"
	"fmt"
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
		data := []byte("{}\n")
		if suffix == ".meta.json" {
			var err error
			data, err = json.Marshal(SessionMeta{Key: sanitized, Count: 1})
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
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

// Each automation keeps its newest maxCronRunsPerJob runs, counting both key
// variants and a job ID with '-'. The age rule still removes an old run of a
// small job, and a conversation is never touched.
func TestPruneCronRunsCapsRunsPerJob(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := func(prefix, job string, n int) string {
		return fmt.Sprintf("%s-%s-%08x-0000-4000-8000-000000000000", prefix, job, n)
	}
	busy := make([]string, 0, maxCronRunsPerJob+10)
	for i := range cap(busy) {
		prefix := "agent_cron"
		if i%2 == 1 {
			prefix = "agent_cronmodel"
		}
		busy = append(busy, run(prefix, "busy-job", i))
		writeSession(t, dir, busy[i], time.Duration(i)*time.Minute)
	}
	small := []string{run("agent_cron", "small", 0), run("agent_cron", "small", 1), run("agent_cron", "small", 2)}
	for i, name := range small {
		writeSession(t, dir, name, time.Duration(i)*time.Hour)
	}
	expired := run("agent_cron", "small", 3)
	writeSession(t, dir, expired, 15*24*time.Hour)
	writeSession(t, dir, "sk_v1_conversation", 365*24*time.Hour)

	removed, err := store.PruneCronRuns(14*24*time.Hour, time.Now())
	if err != nil || removed != 11 {
		t.Fatalf("PruneCronRuns = %d, %v; want 11 removed", removed, err)
	}
	kept := append(append(busy[:maxCronRunsPerJob:maxCronRunsPerJob], small...), "sk_v1_conversation")
	for _, name := range kept {
		if !exists(t, filepath.Join(dir, name+".meta.json")) {
			t.Errorf("%s was removed", name)
		}
	}
	for _, name := range append(busy[maxCronRunsPerJob:], expired) {
		if exists(t, filepath.Join(dir, name+".meta.json")) || exists(t, filepath.Join(dir, name+".jsonl")) {
			t.Errorf("%s survived the prune", name)
		}
	}
}

func TestCronRunJob(t *testing.T) {
	id := "0b89ab77-2375-42d6-ac89-83d2ebc233b6"
	cases := []struct {
		name, job string
		ok        bool
	}{
		{"agent_cron-a1b2-" + id, "a1b2", true},
		{"agent_cronmodel-a1b2-" + id, "a1b2", true},
		{"agent_cron-job-with-dashes-" + id, "job-with-dashes", true},
		{"agent_cron-" + id, "", false},
		{"agent_cron-a1b2-not-a-uuid", "", false},
		{"agent_task-" + id, "", false},
		{"sk_v1_conversation", "", false},
	}
	for _, tc := range cases {
		if job, ok := cronRunJob(tc.name); job != tc.job || ok != tc.ok {
			t.Errorf("cronRunJob(%q) = %q, %v; want %q, %v", tc.name, job, ok, tc.job, tc.ok)
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

func TestIsCronRunSessionKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"agent:cron-job-uuid", true},
		{"agent:cronmodel-job-uuid", true},
		{"AGENT:CRON-job-uuid", true},
		{"  agent:cron-job-uuid", true},
		{"sk_v1_abc", false},
		{"agent:main:telegram:group:x", false},
		{"heartbeat", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsCronRunSessionKey(c.key); got != c.want {
			t.Errorf("IsCronRunSessionKey(%q) = %v, want %v", c.key, got, c.want)
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

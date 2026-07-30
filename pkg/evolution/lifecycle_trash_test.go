package evolution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSkillFile(t *testing.T, workspace, name, body string) string {
	t.Helper()
	dir := filepath.Join(workspace, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// A decisão de aposentar vem de um retention score alimentado por um juiz
// heurístico/LLM — sinal ruidoso. Apagar sem rede transforma erro de pontuação
// em perda definitiva de trabalho que o próprio sistema gerou.
func TestApplyLifecycleState_MoveParaALixeiraEmVezDeApagar(t *testing.T) {
	workspace := t.TempDir()
	paths := NewPaths(workspace, "")
	path := writeSkillFile(t, workspace, "obsoleta", "conteúdo que não pode sumir")

	err := ApplyLifecycleState(paths, SkillProfile{
		SkillName:   "obsoleta",
		WorkspaceID: workspace,
	}, SkillStatusDeleted)
	if err != nil {
		t.Fatalf("ApplyLifecycleState: %v", err)
	}

	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("SKILL.md continua no lugar (%v)", statErr)
	}

	entries, err := os.ReadDir(filepath.Join(paths.RootDir, "trash"))
	if err != nil {
		t.Fatalf("lixeira não existe: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("lixeira com %d arquivos, want 1", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), "obsoleta.") {
		t.Fatalf("nome na lixeira = %q, want prefixo obsoleta.", entries[0].Name())
	}
	data, err := os.ReadFile(filepath.Join(paths.RootDir, "trash", entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile lixeira: %v", err)
	}
	if string(data) != "conteúdo que não pode sumir" {
		t.Fatalf("cópia na lixeira = %q", data)
	}
}

// Transição só dispara com 365 dias de ociosidade: muitas de uma vez não é o
// ciclo de vida funcionando, é relógio errado ou restore com mtime zerado.
func TestRunLifecycleOnce_LimitaDelecoesPorRun(t *testing.T) {
	workspace := t.TempDir()
	paths := NewPaths(workspace, "")
	store := NewStore(paths)

	ancient := time.Now().Add(-400 * 24 * time.Hour)
	total := maxLifecycleDeletesPerRun + 3
	for i := range total {
		name := "velha-" + string(rune('a'+i))
		writeSkillFile(t, workspace, name, "corpo")
		if err := store.SaveProfile(SkillProfile{
			SkillName:      name,
			WorkspaceID:    workspace,
			Status:         SkillStatusArchived,
			Origin:         "evolution",
			LastUsedAt:     ancient,
			RetentionScore: 0.0,
		}); err != nil {
			t.Fatalf("SaveProfile: %v", err)
		}
	}

	summary, err := RunLifecycleOnce(store, paths, workspace, time.Now())
	if err != nil {
		t.Fatalf("RunLifecycleOnce: %v", err)
	}
	if summary.DeletedSkills != maxLifecycleDeletesPerRun {
		t.Fatalf("DeletedSkills = %d, want %d", summary.DeletedSkills, maxLifecycleDeletesPerRun)
	}

	// As que sobraram continuam no disco — e voltam a ser avaliadas no run
	// seguinte, então nada fica preso, só desacelerado.
	survivors := 0
	for i := range total {
		name := "velha-" + string(rune('a'+i))
		if _, err := os.Stat(filepath.Join(workspace, "skills", name, "SKILL.md")); err == nil {
			survivors++
		}
	}
	if survivors != total-maxLifecycleDeletesPerRun {
		t.Fatalf("%d skills sobreviveram, want %d", survivors, total-maxLifecycleDeletesPerRun)
	}
}

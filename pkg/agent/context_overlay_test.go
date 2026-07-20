package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Overlays graváveis (memory/USER.md, memory/SOUL.md) aparecem no bootstrap —
// é onde o agente grava o que APRENDE, já que os arquivos base podem ser
// montados read-only pelo deployment (roteamento de memória).
func TestLoadBootstrapFiles_MemoryOverlays(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("USER.md", "# User\nbase user prefs")
	writeFile("SOUL.md", "# Soul\nbase persona")
	writeFile(filepath.Join("memory", "USER.md"), "- gosta de respostas curtas")
	writeFile(filepath.Join("memory", "SOUL.md"), "- evitar jargão de marketing")

	out := NewContextBuilder(ws).LoadBootstrapFiles()

	for _, want := range []string{
		"USER.md (learned)",
		"gosta de respostas curtas",
		"SOUL.md (learned)",
		"evitar jargão de marketing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bootstrap must contain %q; got:\n%s", want, out)
		}
	}
	// A seção learned do USER vem DEPOIS da base (complemento, não substitution).
	if strings.Index(out, "base user prefs") > strings.Index(out, "gosta de respostas curtas") {
		t.Error("learned USER overlay must come after the base USER.md section")
	}
}

// Overlays vazios/inexistentes não geram seção (sem ruído no prompt).
func TestLoadBootstrapFiles_MemoryOverlaysAbsent(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "USER.md"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := NewContextBuilder(ws).LoadBootstrapFiles()
	if strings.Contains(out, "(learned)") {
		t.Errorf("no overlay sections expected without overlay files; got:\n%s", out)
	}
}

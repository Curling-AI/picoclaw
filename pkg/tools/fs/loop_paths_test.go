package fstools

import (
	"context"
	"path/filepath"
	"testing"
)

// Isto começou como instrução no prompt e falhou em produção: num turno de
// automação o agente gravou em artifacts/ global mesmo com a instrução
// presente. "artifacts/" aparece em outros pontos do prompt, e instrução perde
// para instrução — por isso virou código.
func TestResolveLoopPath(t *testing.T) {
	root := "/ws/loops/vendas"
	SetLoopRootResolver(func(context.Context) string { return root })
	t.Cleanup(func() { SetLoopRootResolver(nil) })
	ctx := context.Background()

	casos := map[string]string{
		"artifacts/relatorio.md":  filepath.Join(root, "artifacts/relatorio.md"),
		"artifacts":               filepath.Join(root, "artifacts"),
		"artifacts/sub/dir/a.png": filepath.Join(root, "artifacts/sub/dir/a.png"),
		"./artifacts/x.md":        filepath.Join(root, "artifacts/x.md"),
		// Fora de artifacts/: intocado. Memória tem ferramenta própria com
		// guard-rail, e skills são escritas pelo evolution, que já recebe a raiz.
		"memory/MEMORY.md":  "memory/MEMORY.md",
		"skills/x/SKILL.md": "skills/x/SKILL.md",
		"uploads/a.pdf":     "uploads/a.pdf",
		// Prefixo parecido não conta.
		"artifacts-antigos/a.md": "artifacts-antigos/a.md",
		// Absoluto é escolha explícita de quem chamou.
		"/ws/artifacts/a.md": "/ws/artifacts/a.md",
	}
	for in, want := range casos {
		if got := resolveLoopPath(ctx, in); got != want {
			t.Fatalf("resolveLoopPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Fora de um loop, e sem resolver instalado, nada muda — o comportamento
// anterior byte a byte.
func TestResolveLoopPath_SemLoopNaoMexe(t *testing.T) {
	SetLoopRootResolver(nil)
	if got := resolveLoopPath(context.Background(), "artifacts/a.md"); got != "artifacts/a.md" {
		t.Fatalf("sem resolver: %q", got)
	}
	SetLoopRootResolver(func(context.Context) string { return "" })
	t.Cleanup(func() { SetLoopRootResolver(nil) })
	if got := resolveLoopPath(context.Background(), "artifacts/a.md"); got != "artifacts/a.md" {
		t.Fatalf("turno sem loop: %q", got)
	}
}

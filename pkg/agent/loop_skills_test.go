package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill grava um SKILL.md com frontmatter, como o gerador de drafts faz.
func writeSkill(t *testing.T, root, name, description, body string) {
	t.Helper()
	dir := filepath.Join(root, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// O evolution do loop APLICA skills em loops/<slug>/skills/. Sem esta raiz no
// loader, elas seriam geradas, versionadas, com backup e ciclo de vida — e
// invisíveis para o modelo.
func TestLoaderForLoop_CarregaSkillDoLoop(t *testing.T) {
	workspace := t.TempDir()
	loopRoot := filepath.Join(workspace, "loops", "vendas")
	writeSkill(t, loopRoot, "fechar-negocio", "Fecha negócio no CRM", "Passo 1 do loop.")

	cb := NewContextBuilder(workspace)
	scope := LoopScope{Slug: "vendas", Root: loopRoot}

	if _, ok := cb.skillsLoader.LoadSkill("fechar-negocio"); ok {
		t.Fatal("loader do agente enxergou a skill do loop — deveria ser invisível fora dele")
	}
	body, ok := cb.loaderForLoop(scope).LoadSkill("fechar-negocio")
	if !ok {
		t.Fatal("loader do loop não carregou a skill do loop")
	}
	if !strings.Contains(body, "Passo 1 do loop.") {
		t.Fatalf("conteúdo = %q", body)
	}
}

// A skill do loop vence a global de mesmo nome DENTRO do loop — é o que "o loop
// aprendeu um jeito próprio de fazer isso" significa. E fora dele, nada muda.
func TestLoaderForLoop_SkillDoLoopVenceAGlobal(t *testing.T) {
	workspace := t.TempDir()
	loopRoot := filepath.Join(workspace, "loops", "vendas")
	writeSkill(t, workspace, "relatorio", "Relatório padrão", "VERSAO GLOBAL")
	writeSkill(t, loopRoot, "relatorio", "Relatório do loop", "VERSAO DO LOOP")

	cb := NewContextBuilder(workspace)

	global, _ := cb.skillsLoader.LoadSkill("relatorio")
	if !strings.Contains(global, "VERSAO GLOBAL") {
		t.Fatalf("fora do loop deveria carregar a global, veio %q", global)
	}
	inLoop, _ := cb.loaderForLoop(LoopScope{Slug: "vendas", Root: loopRoot}).LoadSkill("relatorio")
	if !strings.Contains(inLoop, "VERSAO DO LOOP") {
		t.Fatalf("dentro do loop deveria carregar a do loop, veio %q", inLoop)
	}
}

// WithLoopSkills não pode mutar o loader do agente: ele é compartilhado entre
// turnos concorrentes, e o turno ao lado pode ser de outro loop.
func TestWithLoopSkills_NaoMutaOLoaderCompartilhado(t *testing.T) {
	workspace := t.TempDir()
	cb := NewContextBuilder(workspace)

	rootA := filepath.Join(workspace, "loops", "a")
	rootB := filepath.Join(workspace, "loops", "b")
	writeSkill(t, rootA, "so-do-a", "A", "corpo A")
	writeSkill(t, rootB, "so-do-b", "B", "corpo B")

	loaderA := cb.loaderForLoop(LoopScope{Slug: "a", Root: rootA})
	loaderB := cb.loaderForLoop(LoopScope{Slug: "b", Root: rootB})

	if _, ok := loaderA.LoadSkill("so-do-b"); ok {
		t.Fatal("loader do loop A enxergou skill do loop B")
	}
	if _, ok := loaderB.LoadSkill("so-do-a"); ok {
		t.Fatal("loader do loop B enxergou skill do loop A")
	}
	if _, ok := cb.skillsLoader.LoadSkill("so-do-a"); ok {
		t.Fatal("o loader do agente foi mutado por WithLoopSkills")
	}
}

// A resolução por nome é o que permite ATIVAR a skill: sem ela o modelo vê a
// skill no catálogo do loop e não consegue usá-la.
func TestResolveSkillNameForLoop(t *testing.T) {
	workspace := t.TempDir()
	loopRoot := filepath.Join(workspace, "loops", "vendas")
	writeSkill(t, loopRoot, "fechar-negocio", "Fecha negócio", "corpo")

	cb := NewContextBuilder(workspace)
	if _, ok := cb.ResolveSkillName("fechar-negocio"); ok {
		t.Fatal("resolveu skill do loop fora do loop")
	}
	name, ok := cb.ResolveSkillNameForLoop("FECHAR-NEGOCIO", LoopScope{Slug: "vendas", Root: loopRoot})
	if !ok || name != "fechar-negocio" {
		t.Fatalf("ResolveSkillNameForLoop = (%q, %v), want (fechar-negocio, true)", name, ok)
	}
}

// O catálogo é o único jeito de o modelo saber que a skill do loop existe: o
// find_installed_skills enumera só as três raízes fixas.
func TestLoopSkillCatalog(t *testing.T) {
	loopRoot := filepath.Join(t.TempDir(), "loops", "vendas")
	writeSkill(t, loopRoot, "fechar-negocio", "Fecha negócio no HubSpot", "corpo")
	writeSkill(t, loopRoot, "sem-descricao", "", "corpo")

	catalog := loopSkillCatalog(LoopScope{Slug: "vendas", Root: loopRoot})
	if !strings.Contains(catalog, "`fechar-negocio` — Fecha negócio no HubSpot") {
		t.Fatalf("catálogo sem a skill com descrição:\n%s", catalog)
	}
	if !strings.Contains(catalog, "`sem-descricao`") {
		t.Fatalf("catálogo omitiu skill sem descrição:\n%s", catalog)
	}

	if got := loopSkillCatalog(LoopScope{}); got != "" {
		t.Fatalf("sem loop, catálogo = %q, want vazio", got)
	}
}

// O bloco do loop é pago em input a cada mensagem, e o evolution não tem teto de
// quantas skills cria.
func TestLoopSkillCatalog_LimitaOTamanho(t *testing.T) {
	loopRoot := filepath.Join(t.TempDir(), "loops", "vendas")
	for i := range maxLoopCatalogEntries + 10 {
		writeSkill(t, loopRoot, "skill-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "d", "corpo")
	}
	lines := strings.Count(strings.TrimSpace(loopSkillCatalog(LoopScope{Slug: "vendas", Root: loopRoot})), "\n") + 1
	if lines > maxLoopCatalogEntries {
		t.Fatalf("catálogo com %d entradas, teto é %d", lines, maxLoopCatalogEntries)
	}
}

// A atribuição de uso precisa do loop: sem ela, ler uma skill do loop não conta
// como uso, e a skill que o próprio loop gerou apodrece por falta de sinal.
func TestSkillRootsForLoop_IncluiARaizDoLoop(t *testing.T) {
	workspace := t.TempDir()
	loopRoot := filepath.Join(workspace, "loops", "vendas")
	cb := NewContextBuilder(workspace)

	want := filepath.Join(loopRoot, "skills")
	if slicesContains(cb.skillRoots(), want) {
		t.Fatal("raiz do loop apareceu sem loop no turno")
	}
	if !slicesContains(cb.skillRootsForLoop(LoopScope{Slug: "vendas", Root: loopRoot}), want) {
		t.Fatalf("skillRootsForLoop não inclui %q", want)
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

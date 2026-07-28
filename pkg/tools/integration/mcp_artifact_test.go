package integrationtools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// O despejo de resultado grande de MCP vive DENTRO de artifacts/, não num
// ".artifacts" paralelo. O diretório oculto era o único não persistido do
// workspace: os arquivos morriam com o pod enquanto o histórico seguia citando
// o caminho (em produção, 83 de 85 referências já apontavam para o vazio).
func TestPersistLargeTextArtifact_LivesUnderArtifacts(t *testing.T) {
	ws := t.TempDir()
	tool := &MCPTool{
		workspace:          ws,
		serverName:         "zendesk",
		tool:               &mcp.Tool{Name: "search"},
		maxInlineTextRunes: 16,
	}

	res := tool.persistLargeTextArtifact(strings.Repeat("a", 100))
	if res == nil {
		t.Fatal("esperava um artifact para texto acima do limite")
	}
	if len(res.ArtifactTags) != 1 {
		t.Fatalf("esperava 1 artifact tag, veio %v", res.ArtifactTags)
	}

	path := strings.TrimSuffix(strings.TrimPrefix(res.ArtifactTags[0], "[file:"), "]")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("arquivo do artifact não existe: %v", err)
	}

	rel, err := filepath.Rel(ws, path)
	if err != nil {
		t.Fatalf("caminho fora do workspace: %v", err)
	}
	if !strings.HasPrefix(rel, filepath.Join("artifacts", "mcp")+string(filepath.Separator)) {
		t.Errorf("esperava o despejo em artifacts/mcp/, veio %q", rel)
	}
	// O diretório oculto some: era ele que não tinha mount.
	if strings.HasPrefix(rel, ".artifacts") {
		t.Errorf("despejo voltou para o diretório oculto: %q", rel)
	}
}

// Abaixo do limite não gera arquivo nenhum — o texto vai inteiro para o modelo.
func TestPersistLargeTextArtifact_SkipsSmallText(t *testing.T) {
	ws := t.TempDir()
	tool := &MCPTool{
		workspace:          ws,
		serverName:         "zendesk",
		tool:               &mcp.Tool{Name: "search"},
		maxInlineTextRunes: 1000,
	}

	if res := tool.persistLargeTextArtifact("curto"); res != nil {
		t.Fatalf("texto pequeno não deve virar artifact, veio %#v", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "artifacts")); !os.IsNotExist(err) {
		t.Error("não deveria ter criado artifacts/ para texto pequeno")
	}
}

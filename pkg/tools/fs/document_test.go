package fstools

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// O conversor é um binário externo. Os testes o substituem por um stub de
// shell: o que importa aqui é o CONTRATO (entra por stdin, sai markdown, falha
// vira motivo), não o parser do anydoc — que tem os testes dele.
func stubAnydoc(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub de shell não roda no Windows")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "anydoc-stub")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	anterior := lookupAnydoc
	lookupAnydoc = func() (string, error) { return bin, nil }
	t.Cleanup(func() { lookupAnydoc = anterior })
	return bin
}

// semConversor simula a máquina sem anydoc — o estado em que o comportamento
// antigo tem que valer inteiro.
func semConversor(t *testing.T) {
	t.Helper()
	anterior := lookupAnydoc
	lookupAnydoc = func() (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookupAnydoc = anterior })
}

// docBinario escreve um arquivo que dispara a guarda de binário (magic de zip
// + NULs). Não é um docx válido — nenhum destes testes depende disso.
func docBinario(t *testing.T, dir, nome string) string {
	t.Helper()
	p := filepath.Join(dir, nome)
	if err := os.WriteFile(p, append([]byte("PK\x03\x04"), make([]byte, 600)...), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const mdConvertido = "# Relatorio\n\n| Regiao | Receita |\n| --- | --- |\n| Sul | 120 |\n"

// O caso central: o que era recusa vira conteúdo legível.
func TestReadFile_DocumentoViraMarkdown(t *testing.T) {
	stubAnydoc(t, "cat >/dev/null; printf '%s' "+shellQuote(mdConvertido))
	dir := t.TempDir()
	doc := docBinario(t, dir, "relatorio.docx")

	res := NewReadFileTool(dir, false, 0).Execute(t.Context(), map[string]any{"path": doc})

	if res.IsError {
		t.Fatalf("documento deveria ser lido, veio erro: %s", res.ContentForLLM())
	}
	out := res.ContentForLLM()
	if !strings.Contains(out, "| Regiao | Receita |") {
		t.Errorf("markdown convertido ausente: %s", out)
	}
	// O cabeçalho tem que avisar que houve troca de formato: os offsets passam
	// a ser do markdown, não do arquivo em disco, e quem pagina precisa saber.
	if !strings.Contains(out, "converted to markdown") || !strings.Contains(out, "relatorio.docx") {
		t.Errorf("cabeçalho não identifica a troca de formato: %s", out)
	}
}

// A propriedade de segurança: o sandbox do read_file já autorizou o handle.
// Passar o CAMINHO para o subprocesso o resolveria de novo, fora daquela
// autorização — então o conversor não pode receber caminho nenhum.
func TestReadFile_ConversorRecebePorStdinNaoPorCaminho(t *testing.T) {
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv.txt")
	entrada := filepath.Join(dir, "stdin.bin")
	stubAnydoc(t, "printf '%s' \"$*\" > "+argv+"; cat > "+entrada+"; printf '%s' "+shellQuote(mdConvertido))

	doc := docBinario(t, dir, "relatorio.docx")
	res := NewReadFileTool(dir, false, 0).Execute(t.Context(), map[string]any{"path": doc})
	if res.IsError {
		t.Fatalf("erro inesperado: %s", res.ContentForLLM())
	}

	args, _ := os.ReadFile(argv)
	if strings.TrimSpace(string(args)) != "-" {
		t.Errorf("conversor deveria receber apenas \"-\" (stdin), recebeu %q", args)
	}
	if strings.Contains(string(args), dir) || strings.Contains(string(args), "relatorio") {
		t.Errorf("o caminho vazou para o subprocesso: %q", args)
	}

	// E os bytes têm que chegar inteiros, inclusive os 512 já consumidos pela
	// farejada de binário — senão o conversor recebe um arquivo decapitado.
	lido, err := os.ReadFile(entrada)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(doc)
	if len(lido) != len(original) {
		t.Errorf("stdin recebeu %d bytes, arquivo tem %d — a farejada não foi reposta", len(lido), len(original))
	}
}

// Paginação: um PDF grande vira markdown grande, e tem que ser percorrível
// com as mesmas chamadas de um arquivo de texto.
func TestReadFile_DocumentoPaginado(t *testing.T) {
	stubAnydoc(t, "cat >/dev/null; printf '%s' "+shellQuote(mdConvertido))
	dir := t.TempDir()
	doc := docBinario(t, dir, "relatorio.docx")
	tool := NewReadFileTool(dir, false, 0)

	primeira := tool.Execute(t.Context(), map[string]any{"path": doc, "length": 10})
	out := primeira.ContentForLLM()
	if !strings.Contains(out, "TRUNCATED") || !strings.Contains(out, "offset=10") {
		t.Fatalf("primeira página deveria pedir offset=10: %s", out)
	}

	segunda := tool.Execute(t.Context(), map[string]any{"path": doc, "offset": 10})
	if !strings.Contains(segunda.ContentForLLM(), "END OF DOCUMENT") {
		t.Errorf("segunda página deveria terminar o documento: %s", segunda.ContentForLLM())
	}

	// Offset além do fim não pode estourar.
	fim := tool.Execute(t.Context(), map[string]any{"path": doc, "offset": 99999})
	if !strings.Contains(fim.ContentForLLM(), "no content at this offset") {
		t.Errorf("offset além do fim: %s", fim.ContentForLLM())
	}
}

// Sem anydoc instalado nada muda — é o que torna a mudança segura de subir
// antes da imagem que instala o binário.
func TestReadFile_SemConversorMantemComportamentoAntigo(t *testing.T) {
	semConversor(t)
	dir := t.TempDir()
	doc := docBinario(t, dir, "relatorio.docx")

	res := NewReadFileTool(dir, false, 0).Execute(t.Context(), map[string]any{"path": doc})

	if !res.IsError {
		t.Fatalf("sem conversor a leitura binária ainda tem que recusar: %s", res.ContentForLLM())
	}
	if !strings.Contains(res.ContentForLLM(), "relatorio.docx") {
		t.Errorf("recusa deveria nomear o arquivo: %s", res.ContentForLLM())
	}
	if strings.Contains(res.ContentForLLM(), "PK\x03") {
		t.Error("recusa não pode conter bytes crus")
	}
}

// Conversor que falha (PDF escaneado, arquivo cifrado) devolve o motivo
// dele — é o que manda para o OCR em vez de para outra tentativa.
func TestReadFile_FalhaDeConversaoExplicaOMotivo(t *testing.T) {
	stubAnydoc(t, "cat >/dev/null; echo 'PDF has no extractable text (Scanned, 1 pages): OCR is required' >&2; exit 1")
	dir := t.TempDir()
	doc := docBinario(t, dir, "escaneado.pdf")

	res := NewReadFileTool(dir, false, 0).Execute(t.Context(), map[string]any{"path": doc})

	if !res.IsError {
		t.Fatalf("falha do conversor tem que virar erro: %s", res.ContentForLLM())
	}
	if !strings.Contains(res.ContentForLLM(), "OCR is required") {
		t.Errorf("motivo do conversor perdido: %s", res.ContentForLLM())
	}
}

// Saída vazia é falha, não sucesso: devolver um resultado em branco faria o
// modelo concluir que o documento não tem conteúdo.
func TestReadFile_ConversaoVaziaEFalha(t *testing.T) {
	stubAnydoc(t, "cat >/dev/null; printf '   \\n'")
	dir := t.TempDir()
	doc := docBinario(t, dir, "vazio.docx")

	res := NewReadFileTool(dir, false, 0).Execute(t.Context(), map[string]any{"path": doc})
	if !res.IsError {
		t.Errorf("markdown vazio deveria falhar, veio: %q", res.ContentForLLM())
	}
}

// Binário que NÃO é documento nem chega ao conversor — um zip continua com a
// dica de unzip, uma imagem com load_image.
func TestReadFile_BinarioNaoDocumentoNaoChamaConversor(t *testing.T) {
	dir := t.TempDir()
	marcador := filepath.Join(dir, "chamou.txt")
	stubAnydoc(t, "touch "+marcador+"; cat >/dev/null; printf 'nao deveria'")

	for nome, dica := range map[string]string{"pacote.zip": "unzip -l", "foto.png": "load_image"} {
		res := NewReadFileTool(dir, false, 0).Execute(t.Context(), map[string]any{
			"path": docBinario(t, dir, nome),
		})
		if !res.IsError || !strings.Contains(res.ContentForLLM(), dica) {
			t.Errorf("%s deveria manter a dica %q, veio: %s", nome, dica, res.ContentForLLM())
		}
	}
	if _, err := os.Stat(marcador); err == nil {
		t.Error("o conversor foi chamado para um binário que não é documento")
	}
}

// Modo linhas serve o mesmo markdown, numerado — antes ele respondia "switch
// read_file mode to 'bytes'", que nunca foi resposta útil para um docx.
func TestReadFileLines_DocumentoNumerado(t *testing.T) {
	stubAnydoc(t, "cat >/dev/null; printf '%s' "+shellQuote(mdConvertido))
	dir := t.TempDir()
	doc := docBinario(t, dir, "relatorio.docx")

	res := NewReadFileLinesTool(dir, false, 0).Execute(t.Context(), map[string]any{"path": doc})

	if res.IsError {
		t.Fatalf("modo linhas deveria converter: %s", res.ContentForLLM())
	}
	out := res.ContentForLLM()
	if !strings.Contains(out, "1|# Relatorio") {
		t.Errorf("markdown não foi numerado: %s", out)
	}
}

func TestReadFileLines_SemConversorDaGuidancePorFormato(t *testing.T) {
	semConversor(t)
	dir := t.TempDir()
	doc := docBinario(t, dir, "relatorio.docx")

	res := NewReadFileLinesTool(dir, false, 0).Execute(t.Context(), map[string]any{"path": doc})
	if !res.IsError {
		t.Fatal("deveria recusar")
	}
	if !strings.Contains(res.ContentForLLM(), "relatorio.docx") {
		t.Errorf("a recusa antiga (\"switch to bytes\") não ajudava num docx: %s", res.ContentForLLM())
	}
}

func TestIsConvertibleDocument(t *testing.T) {
	sim := []string{"a.docx", "a.DOCX", "a.pdf", "a.xlsx", "a.pptx", "a.odt", "a.epub", "a.rtf", "a.xlsb"}
	nao := []string{"a.zip", "a.png", "a.txt", "a.json", "a.tar.gz", "a", "a.md"}
	for _, p := range sim {
		if !isConvertibleDocument(p) {
			t.Errorf("%s deveria ser aceito pelo conversor", p)
		}
	}
	for _, p := range nao {
		if isConvertibleDocument(p) {
			t.Errorf("%s NÃO deveria ser aceito pelo conversor", p)
		}
	}
}

// shellQuote embrulha em aspas simples para o stub, escapando as internas.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

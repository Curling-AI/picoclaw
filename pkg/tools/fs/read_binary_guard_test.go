package fstools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// read_file (byte mode) must refuse binary dumps with actionable guidance —
// prod sessions carried 20-60KB of raw docx/pdf zip bytes as tool results.
func TestReadFileTool_RefusesBinaryWithGuidance(t *testing.T) {
	dir := t.TempDir()

	// A fake docx: zip magic + NUL bytes → binary by every heuristic.
	docx := filepath.Join(dir, "relatorio.docx")
	if err := os.WriteFile(docx, append([]byte("PK\x03\x04"), make([]byte, 600)...), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadFileTool(dir, false, 0)
	res := tool.Execute(t.Context(), map[string]any{"path": docx})
	if !res.IsError {
		t.Fatalf("binary read should error, got: %.120s", res.ContentForLLM())
	}
	out := res.ContentForLLM()
	if !strings.Contains(out, "relatorio.docx") || !strings.Contains(out, "docx skill") {
		t.Errorf("guidance should name the file and the docx extraction path, got: %s", out)
	}
	if strings.Contains(out, "PK\x03") {
		t.Errorf("guidance must not include raw bytes")
	}

	// Text files keep working untouched.
	txt := filepath.Join(dir, "notas.txt")
	if err := os.WriteFile(txt, []byte("linha um\nlinha dois\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = tool.Execute(t.Context(), map[string]any{"path": txt})
	if res.IsError {
		t.Fatalf("text read should succeed: %s", res.ContentForLLM())
	}
	if !strings.Contains(res.ContentForLLM(), "linha um") {
		t.Errorf("text content missing: %s", res.ContentForLLM())
	}
}

func TestBinaryReadFileGuidance_PerFormatHints(t *testing.T) {
	cases := map[string]string{
		"a.xlsx": "anydoc",
		"b.pdf":  "anydoc",
		"c.pptx": "anydoc",
		"d.zip":  "unzip -l",
		"e.png":  "load_image",
		"f.bin":  "suitable tool",
	}
	for file, want := range cases {
		got := binaryReadFileGuidance("/x/"+file, 123, nil)
		if !strings.Contains(got, want) {
			t.Errorf("%s: guidance %q should mention %q", file, got, want)
		}
	}

	// Um PDF escaneado é o caso em que a dica genérica não serve: a saída tem
	// de mandar para o OCR, não para outra tentativa do conversor.
	if got := binaryReadFileGuidance("/x/scan.pdf", 1, nil); !strings.Contains(got, "OCR") {
		t.Errorf("PDF hint deveria citar OCR: %s", got)
	}
}

// Quando o arquivo É um documento mas o conversor falhou, o motivo dele
// tem de chegar ao modelo: é ele que diz "OCR is required" e evita
// que o agente tente de novo a mesma coisa.
func TestBinaryReadFileGuidance_IncluiMotivoDaFalha(t *testing.T) {
	got := binaryReadFileGuidance(
		"/x/scan.pdf", 42,
		errors.New("PDF has no extractable text (Scanned, 1 pages): OCR is required"),
	)
	if !strings.Contains(got, "OCR is required") {
		t.Errorf("motivo do conversor perdido: %s", got)
	}
	if !strings.Contains(got, "scan.pdf") {
		t.Errorf("guidance deveria nomear o arquivo: %s", got)
	}
}

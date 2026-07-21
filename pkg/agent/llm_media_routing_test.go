package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// Document-only turns must NOT count as media turns: routing them swapped the
// whole agentic turn to the vision model (prod: xlsx upload → gemini driving
// a 40-tool turn → empty responses right after the upload).
func TestMessagesContainCurrentTurnMediaTurn_DocumentsDoNotRoute(t *testing.T) {
	cases := []struct {
		name string
		msgs []providers.Message
		want bool
	}{
		{"docx upload", []providers.Message{
			{Role: "user", Content: "analisa", Media: []string{"/ws/uploads/abc-Documento.docx"}},
		}, false},
		{"xlsx upload", []providers.Message{
			{Role: "user", Media: []string{"uploads/planilha.xlsx"}},
		}, false},
		{"pdf upload", []providers.Message{
			{Role: "user", Media: []string{"/ws/uploads/relatorio.pdf"}},
		}, false},
		{"image path ref", []providers.Message{
			{Role: "user", Media: []string{"/ws/uploads/foto.png"}},
		}, true},
		{"resolved image data url", []providers.Message{
			{Role: "user", Media: []string{"data:image/png;base64,AAAA"}},
		}, true},
		{"audio ref", []providers.Message{
			{Role: "user", Media: []string{"/ws/uploads/nota-de-voz.ogg"}},
		}, true},
		{"resolved image path tag in content", []providers.Message{
			{Role: "user", Content: "veja [image:/ws/uploads/print.png]"},
		}, true},
		{"file path tag only", []providers.Message{
			{Role: "user", Content: "veja [file:/ws/uploads/doc.docx]"},
		}, false},
		{"no media", []providers.Message{
			{Role: "user", Content: "oi"},
		}, false},
		{"doc plus image routes", []providers.Message{
			{Role: "user", Media: []string{"uploads/doc.docx", "uploads/foto.jpg"}},
		}, true},
	}
	for _, tc := range cases {
		if got := messagesContainCurrentTurnMediaTurn(tc.msgs); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

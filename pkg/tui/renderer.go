// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT

package tui

import (
	"strings"

	"github.com/charmbracelet/glamour"
)

// Renderer wraps glamour for markdown-to-ANSI conversion.
type Renderer struct {
	tr *glamour.TermRenderer
}

// NewRenderer creates a markdown renderer sized to the given terminal width.
// Falls back to plain text on init error.
func NewRenderer(width int) *Renderer {
	if width <= 0 {
		width = 80
	}
	tr, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return &Renderer{} // fallback: no glamour
	}
	return &Renderer{tr: tr}
}

// Render converts markdown to styled terminal output.
// Returns the input unchanged if glamour is unavailable.
func (r *Renderer) Render(markdown string) string {
	if r.tr == nil {
		return markdown
	}
	out, err := r.tr.Render(markdown)
	if err != nil {
		return markdown
	}
	return strings.TrimRight(out, "\n")
}

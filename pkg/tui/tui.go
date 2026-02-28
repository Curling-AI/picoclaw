// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT

package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/sipeed/picoclaw/pkg/agent"
)

const eventChanSize = 64

// TUI provides a rich terminal interface for the agent loop.
type TUI struct {
	agentLoop  *agent.AgentLoop
	sessionKey string
	renderer   *Renderer
	spinner    *Spinner
	input      *Input
	events     chan agent.AgentEvent
	logo       string
}

// New creates a TUI wired to the given agent loop.
func New(agentLoop *agent.AgentLoop, sessionKey, logo string) *TUI {
	width, _, _ := term.GetSize(int(os.Stdout.Fd()))
	if width <= 0 {
		width = 80
	}

	histFile := ""
	if home, err := os.UserHomeDir(); err == nil {
		histFile = filepath.Join(home, ".picoclaw_history")
	}

	t := &TUI{
		agentLoop:  agentLoop,
		sessionKey: sessionKey,
		renderer:   NewRenderer(width),
		spinner:    NewSpinner(),
		input:      NewInput(histFile),
		events:     make(chan agent.AgentEvent, eventChanSize),
		logo:       logo,
	}

	// Register event handler
	agentLoop.SetEventHandler(func(event agent.AgentEvent) {
		// Non-blocking send; drop events if channel is full
		select {
		case t.events <- event:
		default:
		}
	})

	return t
}

// Run starts the interactive TUI loop. Blocks until the user exits.
func (t *TUI) Run() {
	fmt.Printf("%s Interactive mode (Ctrl+C to exit, \\ at end of line for multi-line)\n\n", t.logo)

	for {
		input, err := t.input.ReadLine()
		if err != nil {
			errMsg := err.Error()
			if errMsg == "interrupt" || errMsg == "EOF" {
				fmt.Println("Goodbye!")
				return
			}
			fmt.Printf("Input error: %v\n", err)
			continue
		}

		if input == "" {
			continue
		}

		if input == "exit" || input == "quit" {
			fmt.Println("Goodbye!")
			return
		}

		t.processInput(input)
	}
}

// processInput sends input to the agent and renders events + response.
func (t *TUI) processInput(input string) {
	// Start event renderer goroutine
	renderDone := make(chan struct{})
	go t.renderEvents(renderDone)

	// Block on agent processing
	ctx := context.Background()
	response, err := t.agentLoop.ProcessDirect(ctx, input, t.sessionKey)

	// Drain any remaining events and stop renderer
	close(t.events)
	<-renderDone

	// Re-create channel for next round
	t.events = make(chan agent.AgentEvent, eventChanSize)
	t.agentLoop.SetEventHandler(func(event agent.AgentEvent) {
		select {
		case t.events <- event:
		default:
		}
	})

	// Stop spinner if still running
	t.spinner.Stop()

	if err != nil {
		fmt.Printf("  Error: %v\n\n", err)
		return
	}

	// Render final response with markdown
	fmt.Println()
	rendered := t.renderer.Render(response)
	fmt.Println(rendered)
	fmt.Println()
}

// renderEvents consumes events from the channel and updates the terminal.
func (t *TUI) renderEvents(done chan struct{}) {
	defer close(done)

	for event := range t.events {
		switch event.Type {
		case agent.EventThinking:
			t.spinner.Start("Thinking...")

		case agent.EventToolStart:
			t.spinner.Stop()
			argsPreview := formatToolArgs(event.ToolArgs)
			fmt.Printf("  \033[36m[tool]\033[0m %s(%s)\n", event.ToolName, argsPreview)
			t.spinner.Start(fmt.Sprintf("Running %s...", event.ToolName))

		case agent.EventToolComplete:
			t.spinner.Stop()
			fmt.Printf("  \033[32m[done]\033[0m %s (%s)\n", event.ToolName, event.Duration.Round(1e6))

		case agent.EventToolError:
			t.spinner.Stop()
			fmt.Printf("  \033[31m[fail]\033[0m %s (%s)\n", event.ToolName, event.Duration.Round(1e6))

		case agent.EventCompacting:
			t.spinner.Stop()
			t.spinner.Start("Compressing context...")

		case agent.EventStopped:
			t.spinner.Stop()
			fmt.Println("  Stopped.")

		case agent.EventResponse:
			t.spinner.Stop()
		}
	}
}

// formatToolArgs produces a compact preview of tool arguments.
func formatToolArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	data, err := json.Marshal(args)
	if err != nil {
		return "..."
	}
	s := string(data)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	// Strip outer braces for readability
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	return s
}

// IsTerminal reports whether stdin is a terminal and TUI mode is appropriate.
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && os.Getenv("TERM") != ""
}

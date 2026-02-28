// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT

package tui

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	maxHistorySize = 100
	primaryPrompt  = "> "
	contPrompt     = "... "
)

// Input handles multi-line terminal input with history.
type Input struct {
	history  []string
	histFile string
	fd       int
}

// NewInput creates an input handler. histFile is the path for persistent history
// (empty string disables persistence).
func NewInput(histFile string) *Input {
	inp := &Input{
		histFile: histFile,
		fd:       int(os.Stdin.Fd()),
	}
	inp.loadHistory()
	return inp
}

// ReadLine reads a potentially multi-line input from the user.
// Enter submits; for multi-line input, end a line with '\' to continue.
// Returns the input string and any error (io.EOF on Ctrl+D).
func (inp *Input) ReadLine() (string, error) {
	if !term.IsTerminal(inp.fd) {
		return inp.readSimple()
	}
	return inp.readRaw()
}

// readSimple is the non-terminal fallback using bufio.
func (inp *Input) readSimple() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print(primaryPrompt)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// readRaw uses raw terminal mode for key-by-key processing.
func (inp *Input) readRaw() (string, error) {
	oldState, err := term.MakeRaw(inp.fd)
	if err != nil {
		// Fall back to simple mode
		return inp.readSimple()
	}
	defer term.Restore(inp.fd, oldState)

	var lines []string
	currentLine := ""
	histIdx := len(inp.history) // points past end = "new input"
	savedInput := ""            // saves current input while browsing history
	cursorPos := 0

	writePrompt := func() {
		if len(lines) == 0 {
			fmt.Print("\r\033[K" + primaryPrompt)
		} else {
			fmt.Print("\r\033[K" + contPrompt)
		}
	}

	redrawLine := func() {
		writePrompt()
		fmt.Print(currentLine)
		// Move cursor to correct position
		if cursorPos < len(currentLine) {
			fmt.Printf("\033[%dD", len(currentLine)-cursorPos)
		}
	}

	writePrompt()

	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return "", err
		}
		for i := 0; i < n; i++ {
			b := buf[i]

			switch {
			case b == 3: // Ctrl+C
				fmt.Print("\r\n")
				if currentLine != "" || len(lines) > 0 {
					// Cancel current input
					return "", nil
				}
				// Exit signal
				return "", fmt.Errorf("interrupt")

			case b == 4: // Ctrl+D
				fmt.Print("\r\n")
				if currentLine == "" && len(lines) == 0 {
					return "", fmt.Errorf("EOF")
				}

			case b == 13 || b == 10: // Enter
				fmt.Print("\r\n")
				// If line ends with \, continue on next line
				if strings.HasSuffix(currentLine, "\\") {
					lines = append(lines, strings.TrimSuffix(currentLine, "\\"))
					currentLine = ""
					cursorPos = 0
					writePrompt()
					continue
				}
				lines = append(lines, currentLine)
				result := strings.Join(lines, "\n")
				result = strings.TrimSpace(result)
				if result != "" {
					inp.addHistory(result)
				}
				return result, nil

			case b == 127 || b == 8: // Backspace
				if cursorPos > 0 {
					currentLine = currentLine[:cursorPos-1] + currentLine[cursorPos:]
					cursorPos--
					redrawLine()
				}

			case b == 1: // Ctrl+A - beginning of line
				cursorPos = 0
				redrawLine()

			case b == 5: // Ctrl+E - end of line
				cursorPos = len(currentLine)
				redrawLine()

			case b == 21: // Ctrl+U - clear line
				currentLine = ""
				cursorPos = 0
				redrawLine()

			case b == 23: // Ctrl+W - delete word backward
				if cursorPos > 0 {
					// Find start of previous word
					pos := cursorPos - 1
					for pos > 0 && currentLine[pos-1] == ' ' {
						pos--
					}
					for pos > 0 && currentLine[pos-1] != ' ' {
						pos--
					}
					currentLine = currentLine[:pos] + currentLine[cursorPos:]
					cursorPos = pos
					redrawLine()
				}

			case b == 27: // Escape sequence
				if i+1 < n {
					i++
					switch buf[i] {
					case '[': // CSI sequence
						if i+1 < n {
							i++
							switch buf[i] {
							case 'A': // Up arrow - history prev
								if histIdx > 0 {
									if histIdx == len(inp.history) {
										savedInput = currentLine
									}
									histIdx--
									currentLine = inp.history[histIdx]
									cursorPos = len(currentLine)
									redrawLine()
								}
							case 'B': // Down arrow - history next
								if histIdx < len(inp.history) {
									histIdx++
									if histIdx == len(inp.history) {
										currentLine = savedInput
									} else {
										currentLine = inp.history[histIdx]
									}
									cursorPos = len(currentLine)
									redrawLine()
								}
							case 'C': // Right arrow
								if cursorPos < len(currentLine) {
									cursorPos++
									fmt.Print("\033[C")
								}
							case 'D': // Left arrow
								if cursorPos > 0 {
									cursorPos--
									fmt.Print("\033[D")
								}
							case '3': // Delete key (ESC [ 3 ~)
								if i+1 < n && buf[i+1] == '~' {
									i++
									if cursorPos < len(currentLine) {
										currentLine = currentLine[:cursorPos] + currentLine[cursorPos+1:]
										redrawLine()
									}
								}
							}
						}
					case 10, 13: // Alt+Enter - insert newline
						lines = append(lines, currentLine)
						currentLine = ""
						cursorPos = 0
						fmt.Print("\r\n")
						writePrompt()
					}
				}

			default:
				if b >= 32 && b < 127 { // Printable ASCII
					currentLine = currentLine[:cursorPos] + string(b) + currentLine[cursorPos:]
					cursorPos++
					redrawLine()
				}
			}
		}
	}
}

func (inp *Input) addHistory(entry string) {
	// Deduplicate: don't add if same as last entry
	if len(inp.history) > 0 && inp.history[len(inp.history)-1] == entry {
		return
	}
	inp.history = append(inp.history, entry)
	if len(inp.history) > maxHistorySize {
		inp.history = inp.history[len(inp.history)-maxHistorySize:]
	}
	inp.saveHistory()
}

func (inp *Input) loadHistory() {
	if inp.histFile == "" {
		return
	}
	data, err := os.ReadFile(inp.histFile)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			inp.history = append(inp.history, line)
		}
	}
	if len(inp.history) > maxHistorySize {
		inp.history = inp.history[len(inp.history)-maxHistorySize:]
	}
}

func (inp *Input) saveHistory() {
	if inp.histFile == "" {
		return
	}
	f, err := os.Create(inp.histFile)
	if err != nil {
		return
	}
	defer f.Close()
	for _, line := range inp.history {
		fmt.Fprintln(f, line)
	}
}

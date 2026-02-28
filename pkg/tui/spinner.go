// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT

package tui

import (
	"fmt"
	"sync"
	"time"
)

var brailleFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner renders a braille-dot spinner with a message, overwriting the current line.
type Spinner struct {
	mu      sync.Mutex
	message string
	stopCh  chan struct{}
	done    chan struct{}
	running bool
}

// NewSpinner creates a new spinner (not started).
func NewSpinner() *Spinner {
	return &Spinner{}
}

// Start begins the spinner animation with the given message.
// If already running, it updates the message.
func (s *Spinner) Start(message string) {
	s.mu.Lock()
	s.message = message
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.stopCh = make(chan struct{})
	s.done = make(chan struct{})
	s.mu.Unlock()

	go s.run()
}

// Stop halts the spinner and clears its line.
func (s *Spinner) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stopCh)
	s.mu.Unlock()
	<-s.done
}

func (s *Spinner) run() {
	defer close(s.done)
	frame := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			// Clear the spinner line
			fmt.Print("\r\033[K")
			return
		case <-ticker.C:
			s.mu.Lock()
			msg := s.message
			s.mu.Unlock()
			fmt.Printf("\r\033[K  %s %s", brailleFrames[frame%len(brailleFrames)], msg)
			frame++
		}
	}
}

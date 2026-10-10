package tools

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

const maxOutputBufferSize = 1 * 1024 * 1024 // 1MB

const outputTruncateMarker = "\n... [output truncated, exceeded 1MB]\n"

// PtyKeyMode represents arrow key encoding mode for PTY sessions.
// Programs send smkx/rmkx sequences to switch between CSI and SS3 modes.
type PtyKeyMode uint8

const (
	PtyKeyModeCSI PtyKeyMode = iota // triggered by rmkx (\x1b[?1l)
	PtyKeyModeSS3                   // triggered by smkx (\x1b[?1h)
)

const PtyKeyModeNotFound PtyKeyMode = 255

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionDone     = errors.New("session already completed")
	ErrPTYNotSupported = errors.New("PTY is not supported on this platform")
	ErrNoStdin         = errors.New("no stdin available")

	// errProcessGone: the process had already exited, nothing was killed.
	errProcessGone = errors.New("process already gone")
)

type ProcessSession struct {
	mu         sync.Mutex
	ID         string
	PID        int
	Command    string
	PTY        bool
	Background bool
	// Owner is the agent session that started the process (empty outside a
	// turn), so the process can be stopped when that session dies.
	Owner           string
	StartTime       int64
	ExitCode        int
	Status          string
	stdinWriter     io.Writer
	stdoutPipe      io.Reader
	outputBuffer    *bytes.Buffer
	outputTruncated bool
	ptyMaster       *os.File

	// ptyKeyMode tracks arrow key encoding mode (CSI vs SS3)
	ptyKeyMode PtyKeyMode
}

func (s *ProcessSession) IsDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status == "done" || s.Status == "exited"
}

func (s *ProcessSession) GetPtyKeyMode() PtyKeyMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ptyKeyMode
}

func (s *ProcessSession) SetPtyKeyMode(mode PtyKeyMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ptyKeyMode = mode
}

func (s *ProcessSession) GetStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status
}

func (s *ProcessSession) SetStatus(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = status
}

func (s *ProcessSession) GetExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ExitCode
}

func (s *ProcessSession) SetExitCode(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ExitCode = code
}

func (s *ProcessSession) killProcess() error {
	if err := s.terminate(); err != nil && !errors.Is(err, errProcessGone) {
		return err
	}
	return nil
}

// terminate kills the session's process group and marks the session done;
// errProcessGone when the process had already exited.
func (s *ProcessSession) terminate() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Status != "running" {
		return ErrSessionDone
	}

	pid := s.PID
	if pid <= 0 {
		return ErrSessionNotFound
	}

	err := killProcessGroup(pid)
	if err != nil && !errors.Is(err, errProcessGone) {
		return err
	}

	s.Status = "done"
	s.ExitCode = -1
	return err
}

func (s *ProcessSession) owner() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Owner
}

func (s *ProcessSession) pid() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.PID
}

func (s *ProcessSession) Kill() error {
	return s.killProcess()
}

func (s *ProcessSession) Write(data string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Status != "running" {
		return ErrSessionDone
	}

	var writer io.Writer
	if s.PTY && s.ptyMaster != nil {
		writer = s.ptyMaster
	} else if s.stdinWriter != nil {
		writer = s.stdinWriter
	} else {
		return ErrNoStdin
	}

	_, err := writer.Write([]byte(data))
	return err
}

func (s *ProcessSession) Read() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.outputBuffer.Len() == 0 {
		return ""
	}

	data := s.outputBuffer.String()
	s.outputBuffer.Reset()
	return data
}

func (s *ProcessSession) ToSessionInfo() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	return SessionInfo{
		ID:        s.ID,
		Command:   s.Command,
		Status:    s.Status,
		PID:       s.PID,
		StartedAt: s.StartTime,
	}
}

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*ProcessSession
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewSessionManager() *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*ProcessSession),
		stopCh:   make(chan struct{}),
	}

	// Start cleaner goroutine - runs every 5 minutes, cleans up sessions done for >30 minutes
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-sm.stopCh:
				return
			case <-ticker.C:
				sm.cleanupOldSessions()
			}
		}
	}()

	return sm
}

// Stop shuts down the background cleanup goroutine. Safe to call multiple
// times from concurrent goroutines. After Stop returns, the SessionManager
// is still usable — only the cleanup goroutine is terminated.
func (sm *SessionManager) Stop() {
	sm.stopOnce.Do(func() {
		close(sm.stopCh)
	})
}

// cleanupOldSessions removes sessions that are done and older than 30 minutes
func (sm *SessionManager) cleanupOldSessions() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	cutoff := time.Now().Add(-30 * time.Minute)
	for id, session := range sm.sessions {
		if session.IsDone() && session.StartTime < cutoff.Unix() {
			delete(sm.sessions, id)
		}
	}
}

func (sm *SessionManager) Add(session *ProcessSession) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sessions[session.ID] = session
}

func (sm *SessionManager) Get(sessionID string) (*ProcessSession, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}

	return session, nil
}

func (sm *SessionManager) Remove(sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, sessionID)
}

func (sm *SessionManager) List() []SessionInfo {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make([]SessionInfo, 0, len(sm.sessions))
	for _, session := range sm.sessions {
		result = append(result, session.ToSessionInfo())
	}

	return result
}

// KillOwnedBy stops the processes started by owner and returns the session IDs
// of those it actually stopped. A session whose shell already exited still
// counts: what the shell left in the background (nohup script &) keeps running
// in its process group.
func (sm *SessionManager) KillOwnedBy(owner string) []string {
	if owner == "" {
		return nil
	}
	sm.mu.RLock()
	var owned []*ProcessSession
	live := make(map[int]string) // pid of a running session -> its id
	for _, session := range sm.sessions {
		if !session.IsDone() {
			live[session.pid()] = session.ID
		}
		if session.owner() == owner {
			owned = append(owned, session)
		}
	}
	sm.mu.RUnlock()

	killed := make([]string, 0, len(owned))
	for _, session := range owned {
		if stopSession(session, live) {
			killed = append(killed, session.ID)
		}
	}
	return killed
}

// stopSession kills a running session's group, or what is left of a finished
// one's, and reports whether something was killed. A finished session's pid
// that now leads another running session's group is left alone.
func stopSession(session *ProcessSession, live map[int]string) bool {
	err := session.terminate()
	if errors.Is(err, ErrSessionDone) {
		pid := session.pid()
		if id, ok := live[pid]; ok && id != session.ID {
			return false
		}
		err = killLeftoverGroup(pid)
	}
	return err == nil
}

// HandOver gives the processes started by from to owner: a sub-turn that
// finished hands what it left running to the turn that launched it, whose
// failure must then stop them.
func (sm *SessionManager) HandOver(from, owner string) {
	if from == "" || owner == "" {
		return
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, session := range sm.sessions {
		session.mu.Lock()
		if session.Owner == from {
			session.Owner = owner
		}
		session.mu.Unlock()
	}
}

// KillBackgroundSessions stops the background processes that the agent session
// owner started with the exec tool, including what their shells left running.
func KillBackgroundSessions(owner string) []string {
	if sm := getSessionManager(); sm != nil {
		return sm.KillOwnedBy(owner)
	}
	return nil
}

// HandOverBackgroundSessions gives the background processes of the agent
// session from to owner (see SessionManager.HandOver).
func HandOverBackgroundSessions(from, owner string) {
	if sm := getSessionManager(); sm != nil {
		sm.HandOver(from, owner)
	}
}

func generateSessionID() string {
	return uuid.New().String()[:8]
}

package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

// Skill enable/disable state, persisted as a single JSON file inside the
// workspace skills root. Living under the skills root means the agent's
// prompt cache (which mtime-tracks that tree recursively) invalidates for
// free on every toggle, and the state survives restarts on persistent
// workspaces. Ids are directory names — the stable identifier used by
// LoadSkill, uninstall and the gateway API.
const stateFileName = ".skills-state.json"

type stateFile struct {
	Version  int      `json:"version"`
	Disabled []string `json:"disabled"`
}

var stateMu sync.Mutex

// LoadDisabled returns the set of disabled skill ids (directory names) for a
// workspace skills dir. Missing or unreadable state means nothing disabled.
func LoadDisabled(workspaceSkillsDir string) map[string]bool {
	out := make(map[string]bool)
	if workspaceSkillsDir == "" {
		return out
	}
	data, err := os.ReadFile(filepath.Join(workspaceSkillsDir, stateFileName))
	if err != nil {
		return out
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return out
	}
	for _, id := range st.Disabled {
		out[id] = true
	}
	return out
}

// SetDisabled marks a skill id as disabled (or re-enabled) and persists the
// state atomically. Safe for concurrent gateway calls.
func SetDisabled(workspaceSkillsDir, id string, disabled bool) error {
	stateMu.Lock()
	defer stateMu.Unlock()

	path := filepath.Join(workspaceSkillsDir, stateFileName)
	st := stateFile{Version: 1}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &st)
		st.Version = 1
	}

	has := slices.Contains(st.Disabled, id)
	if disabled && !has {
		st.Disabled = append(st.Disabled, id)
	} else if !disabled && has {
		st.Disabled = slices.DeleteFunc(st.Disabled, func(s string) bool { return s == id })
	} else {
		return nil // sem mudança — não invalida o cache do prompt à toa
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workspaceSkillsDir, 0o755); err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

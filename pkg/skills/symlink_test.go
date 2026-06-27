package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// `npx skills add` installs skills as symlinks. ListSkills must follow them
// (os.DirEntry.IsDir() is false for a symlink to a directory).
func TestListSkills_FollowsSymlinks(t *testing.T) {
	base := t.TempDir()

	realSkill := filepath.Join(base, "real", "my-skill")
	if err := os.MkdirAll(realSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realSkill, "SKILL.md"), []byte("# my-skill\n\nA test skill.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	globalDir := filepath.Join(base, "global")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realSkill, filepath.Join(globalDir, "my-skill")); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoader("", globalDir, "")
	skills := loader.ListSkills()

	for _, s := range skills {
		if s.Name == "my-skill" {
			return // found
		}
	}
	t.Errorf("symlinked skill not found; got %d skills", len(skills))
}

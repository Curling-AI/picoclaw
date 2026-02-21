package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkillsInfoValidate(t *testing.T) {
	testcases := []struct {
		name        string
		skillName   string
		description string
		wantErr     bool
		errContains []string
	}{
		{
			name:        "valid-skill",
			skillName:   "valid-skill",
			description: "a valid skill description",
			wantErr:     false,
		},
		{
			name:        "empty-name",
			skillName:   "",
			description: "description without name",
			wantErr:     true,
			errContains: []string{"name is required"},
		},
		{
			name:        "empty-description",
			skillName:   "skill-without-description",
			description: "",
			wantErr:     true,
			errContains: []string{"description is required"},
		},
		{
			name:        "empty-both",
			skillName:   "",
			description: "",
			wantErr:     true,
			errContains: []string{"name is required", "description is required"},
		},
		{
			name:        "name-with-spaces",
			skillName:   "skill with spaces",
			description: "invalid name with spaces",
			wantErr:     true,
			errContains: []string{"name must be alphanumeric with hyphens"},
		},
		{
			name:        "name-with-underscore",
			skillName:   "skill_underscore",
			description: "invalid name with underscore",
			wantErr:     true,
			errContains: []string{"name must be alphanumeric with hyphens"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			info := SkillInfo{
				Name:        tc.skillName,
				Description: tc.description,
			}
			err := info.validate()
			if tc.wantErr {
				assert.Error(t, err)
				for _, msg := range tc.errContains {
					assert.ErrorContains(t, err, msg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestExtractFrontmatter(t *testing.T) {
	sl := &SkillsLoader{}

	testcases := []struct {
		name           string
		content        string
		expectedName   string
		expectedDesc   string
		lineEndingType string
	}{
		{
			name:           "unix-line-endings",
			lineEndingType: "Unix (\\n)",
			content:        "---\nname: test-skill\ndescription: A test skill\n---\n\n# Skill Content",
			expectedName:   "test-skill",
			expectedDesc:   "A test skill",
		},
		{
			name:           "windows-line-endings",
			lineEndingType: "Windows (\\r\\n)",
			content:        "---\r\nname: test-skill\r\ndescription: A test skill\r\n---\r\n\r\n# Skill Content",
			expectedName:   "test-skill",
			expectedDesc:   "A test skill",
		},
		{
			name:           "classic-mac-line-endings",
			lineEndingType: "Classic Mac (\\r)",
			content:        "---\rname: test-skill\rdescription: A test skill\r---\r\r# Skill Content",
			expectedName:   "test-skill",
			expectedDesc:   "A test skill",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// Extract frontmatter
			frontmatter := sl.extractFrontmatter(tc.content)
			assert.NotEmpty(t, frontmatter, "Frontmatter should be extracted for %s line endings", tc.lineEndingType)

			// Parse YAML to get name and description (parseSimpleYAML now handles all line ending types)
			yamlMeta := sl.parseSimpleYAML(frontmatter)
			assert.Equal(
				t,
				tc.expectedName,
				yamlMeta["name"],
				"Name should be correctly parsed from frontmatter with %s line endings",
				tc.lineEndingType,
			)
			assert.Equal(
				t,
				tc.expectedDesc,
				yamlMeta["description"],
				"Description should be correctly parsed from frontmatter with %s line endings",
				tc.lineEndingType,
			)
		})
	}
}

func TestListSkills_FollowsSymlinks(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a real skill directory (the symlink target)
	realSkillDir := filepath.Join(tmpDir, "real-skills", "my-skill")
	require.NoError(t, os.MkdirAll(realSkillDir, 0755))
	require.NoError(t, os.WriteFile(
		filepath.Join(realSkillDir, "SKILL.md"),
		[]byte("---\nname: my-skill\ndescription: A test skill via symlink\n---\n# Content"),
		0644,
	))

	// Create the global skills dir with a symlink to the real skill
	globalSkillsDir := filepath.Join(tmpDir, "global-skills")
	require.NoError(t, os.MkdirAll(globalSkillsDir, 0755))
	require.NoError(t, os.Symlink(realSkillDir, filepath.Join(globalSkillsDir, "my-skill")))

	loader := NewSkillsLoader(filepath.Join(tmpDir, "workspace"), globalSkillsDir, "")
	skills := loader.ListSkills()

	require.Len(t, skills, 1, "should discover skill via symlink")
	assert.Equal(t, "my-skill", skills[0].Name)
	assert.Equal(t, "global", skills[0].Source)
	assert.Equal(t, "A test skill via symlink", skills[0].Description)
}

func TestListSkills_FallsBackToDirNameForInvalidMetadataName(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a skill with a name containing spaces in the metadata
	skillDir := filepath.Join(tmpDir, "skills", "skill-development")
	require.NoError(t, os.MkdirAll(skillDir, 0755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: Skill Development\ndescription: A skill with spaces in name\n---\n# Content"),
		0644,
	))

	loader := NewSkillsLoader(filepath.Join(tmpDir, "workspace"), filepath.Join(tmpDir, "skills"), "")
	skills := loader.ListSkills()

	require.Len(t, skills, 1, "should discover skill with invalid metadata name")
	assert.Equal(t, "skill-development", skills[0].Name, "should fall back to directory name")
	assert.Equal(t, "A skill with spaces in name", skills[0].Description)
}

func TestStripFrontmatter(t *testing.T) {
	sl := &SkillsLoader{}

	testcases := []struct {
		name            string
		content         string
		expectedContent string
		lineEndingType  string
	}{
		{
			name:            "unix-line-endings",
			lineEndingType:  "Unix (\\n)",
			content:         "---\nname: test-skill\ndescription: A test skill\n---\n\n# Skill Content",
			expectedContent: "# Skill Content",
		},
		{
			name:            "windows-line-endings",
			lineEndingType:  "Windows (\\r\\n)",
			content:         "---\r\nname: test-skill\r\ndescription: A test skill\r\n---\r\n\r\n# Skill Content",
			expectedContent: "# Skill Content",
		},
		{
			name:            "classic-mac-line-endings",
			lineEndingType:  "Classic Mac (\\r)",
			content:         "---\rname: test-skill\rdescription: A test skill\r---\r\r# Skill Content",
			expectedContent: "# Skill Content",
		},
		{
			name:            "unix-line-endings-without-trailing-newline",
			lineEndingType:  "Unix (\\n) without trailing newline",
			content:         "---\nname: test-skill\ndescription: A test skill\n---\n# Skill Content",
			expectedContent: "# Skill Content",
		},
		{
			name:            "windows-line-endings-without-trailing-newline",
			lineEndingType:  "Windows (\\r\\n) without trailing newline",
			content:         "---\r\nname: test-skill\r\ndescription: A test skill\r\n---\r\n# Skill Content",
			expectedContent: "# Skill Content",
		},
		{
			name:            "no-frontmatter",
			lineEndingType:  "No frontmatter",
			content:         "# Skill Content\n\nSome content here.",
			expectedContent: "# Skill Content\n\nSome content here.",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			result := sl.stripFrontmatter(tc.content)
			assert.Equal(
				t,
				tc.expectedContent,
				result,
				"Frontmatter should be stripped correctly for %s",
				tc.lineEndingType,
			)
		})
	}
}

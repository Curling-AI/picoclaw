package fstools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteFile_ReplacesScratchAndOwnFilesWithoutFlag(t *testing.T) {
	workspace := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(workspace, rel)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(workspace, rel), []byte(content), 0o644))
	}
	tool := NewWriteFileTool(workspace, true)
	run := func(path string) *ToolResult {
		return tool.Execute(context.Background(), map[string]any{"path": path, "content": "new"})
	}
	read := func(rel string) string {
		data, err := os.ReadFile(filepath.Join(workspace, rel))
		require.NoError(t, err)
		return string(data)
	}

	for _, rel := range []string{"work/a.py", "tmp/b/c.txt", "scripts/d.sh"} {
		write(rel, "old")
		result := run(rel)
		assert.False(t, result.IsError, rel)
		assert.Contains(t, result.ForLLM, "previous contents are gone")
		assert.Equal(t, "new", read(rel))
	}

	write("work/abs.py", "old")
	assert.False(t, run(filepath.Join(workspace, "work/abs.py")).IsError, "an absolute path counts too")

	for _, rel := range []string{"memory/MEMORY.md", "artifacts/report.md", "todo.md", "working/x.md"} {
		write(rel, "old")
		result := run(rel)
		assert.True(t, result.IsError, rel)
		assert.Contains(t, result.ForLLM, "already exists")
		assert.Equal(t, "old", read(rel), "a refused file stays intact")
	}

	assert.True(t, run("work/../todo.md").IsError, "climbing out of a scratch dir is not scratch")

	assert.False(t, run("artifacts/mine.md").IsError)
	result := run("artifacts/mine.md")
	assert.False(t, result.IsError, "what the agent wrote itself can be rewritten")
	assert.Contains(t, result.ForLLM, "previous contents are gone")

	flagged := tool.Execute(
		context.Background(),
		map[string]any{"path": "todo.md", "content": "flag", "overwrite": true},
	)
	assert.False(t, flagged.IsError)
	assert.Equal(t, "flag", read("todo.md"))
}

func TestSimilarFileNames(t *testing.T) {
	names := []string{"s-01.png", "s-02.png", "S-03.PNG", "other.png", "notes.txt"}
	assert.Equal(t, []string{"s-01.png", "s-02.png", "S-03.PNG"}, similarFileNames("s-1.png", names))
	assert.Empty(t, similarFileNames("zzz.png", names))
	assert.Empty(t, similarFileNames("123.png", names), "a name that is only a number says nothing")
}

func TestReadFile_MissingNamesSimilarFiles(t *testing.T) {
	workspace := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "tmp"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "tmp", "s-01.png"), []byte("x"), 0o644))
	tool := NewReadFileTool(workspace, true, 1024)

	result := tool.Execute(context.Background(), map[string]any{"path": "tmp/s-1.png"})

	assert.True(t, result.IsError)
	assert.Contains(t, result.ForLLM, "s-01.png")
}

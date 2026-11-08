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

func TestWriteFile_OverwriteFlagDoesNotUnprotectAFile(t *testing.T) {
	workspace := t.TempDir()
	tool := NewWriteFileTool(workspace, true)
	path := filepath.Join(workspace, "report.md")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	assert.False(t, tool.Execute(context.Background(), map[string]any{
		"path": "report.md", "content": "new", "overwrite": true,
	}).IsError)
	result := tool.Execute(context.Background(), map[string]any{"path": "report.md", "content": "again"})

	assert.True(t, result.IsError, "the agent replaced this file, it did not create it")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
}

func TestWriteFile_FileChangedSinceTheAgentCreatedItKeepsTheRefusal(t *testing.T) {
	workspace := t.TempDir()
	tool := NewWriteFileTool(workspace, true)
	args := map[string]any{"path": "report.md", "content": "agent"}
	assert.False(t, tool.Execute(context.Background(), args).IsError)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "report.md"), []byte("the user's edit"), 0o644))

	assert.True(t, tool.Execute(context.Background(), args).IsError)

	got, err := os.ReadFile(filepath.Join(workspace, "report.md"))
	require.NoError(t, err)
	assert.Equal(t, "the user's edit", string(got))
}

func TestWriteFile_RepeatedRewriteKeepsWorking(t *testing.T) {
	tool := NewWriteFileTool(t.TempDir(), true)
	for _, content := range []string{"one", "two", "three"} {
		result := tool.Execute(context.Background(), map[string]any{"path": "report.md", "content": content})
		assert.False(t, result.IsError, result.ForLLM)
	}
}

// Only a file known to be absent counts as free.
func TestWriteFile_UnreadableFileIsNotTreatedAsNew(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	workspace := t.TempDir()
	tool := NewWriteFileTool(workspace, true)
	path := filepath.Join(workspace, "secret.md")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o200))

	result := tool.Execute(context.Background(), map[string]any{"path": "secret.md", "content": "new"})

	assert.True(t, result.IsError, result.ForLLM)
	require.NoError(t, os.Chmod(path, 0o600))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
}

func TestWriteFile_MemoryKeepsTheRefusalEvenWhenCreatedHere(t *testing.T) {
	tool := NewWriteFileTool(t.TempDir(), true)
	args := map[string]any{"path": "memory/MEMORY.md", "content": "first"}
	assert.False(t, tool.Execute(context.Background(), args).IsError)
	assert.True(t, tool.Execute(context.Background(), args).IsError)
}

func TestWriteFile_LinkInAScratchDirIsJudgedByWhereItLands(t *testing.T) {
	workspace := t.TempDir()
	tool := NewWriteFileTool(workspace, true)
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "artifacts"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "work"), 0o755))
	report := filepath.Join(workspace, "artifacts", "report.md")
	require.NoError(t, os.WriteFile(report, []byte("old"), 0o644))
	require.NoError(t, os.Symlink("../artifacts", filepath.Join(workspace, "work", "m")))

	result := tool.Execute(context.Background(), map[string]any{"path": "work/m/report.md", "content": "new"})

	assert.True(t, result.IsError)
	got, err := os.ReadFile(report)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
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

// writeRacingFs lets another writer change the file right after the lock is released.
type writeRacingFs struct {
	fileSystem
	afterUnlock func()
}

func (r writeRacingFs) withWriteLock(path string, write func() error) error {
	err := r.fileSystem.withWriteLock(path, write)
	r.afterUnlock()
	return err
}

func TestWriteFile_ConcurrentWriteAfterUnlockIsNotRecordedAsOwn(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "artifacts", "report.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	tool := NewWriteFileTool(workspace, true)
	tool.fs = writeRacingFs{fileSystem: tool.fs, afterUnlock: func() {
		require.NoError(t, os.WriteFile(path, []byte("another writer's version"), 0o644))
	}}

	assert.False(
		t,
		tool.Execute(context.Background(), map[string]any{"path": "artifacts/report.md", "content": "mine"}).IsError,
	)
	result := tool.Execute(context.Background(), map[string]any{"path": "artifacts/report.md", "content": "again"})

	assert.True(t, result.IsError, "the version on disk is not the one this tool wrote")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "another writer's version", string(got))
}

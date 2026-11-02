package fstools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

func holdMemoryFile(t *testing.T, path string) func() {
	t.Helper()
	locked, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- fileutil.WithFileLock(path, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	return func() {
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func executeMemoryWrite(workspace string, restricted bool, operation, path string) *ToolResult {
	switch operation {
	case "write":
		return NewWriteFileTool(workspace, restricted).Execute(context.Background(), map[string]any{"path": path, "content": "changed", "overwrite": true})
	case "edit":
		return NewEditFileTool(workspace, restricted).Execute(context.Background(), map[string]any{"path": path, "old_text": "original", "new_text": "changed"})
	case "append":
		return NewAppendFileTool(workspace, restricted).Execute(context.Background(), map[string]any{"path": path, "content": " changed"})
	default:
		return ErrorResult("unknown test operation")
	}
}

func TestMemoryWriteToolsShareFileLock(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		for _, name := range []string{"MEMORY.md", "USER.md", "SOUL.md"} {
			for _, operation := range []string{"write", "edit", "append"} {
				t.Run(operation+"/"+name+map[bool]string{false: "/host", true: "/sandbox"}[restricted], func(t *testing.T) {
					workspace := t.TempDir()
					path := filepath.Join(workspace, "memory", name)
					if err := fileutil.WriteFileAtomic(path, []byte("original"), 0o600); err != nil {
						t.Fatal(err)
					}
					argument := path
					if restricted {
						argument = filepath.Join("memory", name)
					}
					release := holdMemoryFile(t, path)
					done := make(chan *ToolResult, 1)
					go func() {
						done <- executeMemoryWrite(workspace, restricted, operation, argument)
					}()
					select {
					case result := <-done:
						release()
						t.Fatalf("memory writer bypassed the shared lock: %v", result)
					case <-time.After(25 * time.Millisecond):
					}
					release()
					if result := <-done; result.IsError {
						t.Fatal(result.ForLLM)
					}
					want := "changed"
					if operation == "append" {
						want = "original changed"
					}
					if got, err := os.ReadFile(path); err != nil || string(got) != want {
						t.Fatalf("memory writer bytes = %q, error = %v, want %q", got, err, want)
					}
				})
			}
		}
	}
}

type memoryReadBarrier struct {
	fileSystem
	read    chan struct{}
	release chan struct{}
}

func (f *memoryReadBarrier) ReadFile(path string) ([]byte, error) {
	content, err := f.fileSystem.ReadFile(path)
	close(f.read)
	<-f.release
	return content, err
}

func TestMemoryEditAndAppendHoldLockAcrossReadModifyWrite(t *testing.T) {
	for _, operation := range []string{"edit", "append"} {
		t.Run(operation, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, "memory", "MEMORY.md")
			if err := fileutil.WriteFileAtomic(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			barrier := &memoryReadBarrier{fileSystem: buildFs(workspace, true, nil), read: make(chan struct{}), release: make(chan struct{})}
			written := make(chan *ToolResult, 1)
			go func() {
				if operation == "edit" {
					written <- (&EditFileTool{fs: barrier}).Execute(context.Background(), map[string]any{"path": "memory/MEMORY.md", "old_text": "original", "new_text": "changed"})
				} else {
					written <- (&AppendFileTool{fs: barrier}).Execute(context.Background(), map[string]any{"path": "memory/MEMORY.md", "content": " changed"})
				}
			}()
			<-barrier.read
			compared := make(chan error, 1)
			go func() {
				compared <- fileutil.WriteFileIfContent(path, []byte("ui"), fileutil.FileContent{Content: []byte("original")}, 0o600)
			}()
			select {
			case err := <-compared:
				close(barrier.release)
				t.Fatalf("CAS passed while the tool held its captured read: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			close(barrier.release)
			if result := <-written; result.IsError {
				t.Fatal(result.ForLLM)
			}
			if err := <-compared; !errors.Is(err, fileutil.ErrFileChanged) {
				t.Fatalf("CAS overwrote the tool's completed mutation: %v", err)
			}
		})
	}
}

func TestNonMemoryWritesKeepIndependentBehavior(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "artifacts", "MEMORY.md")
	release := holdMemoryFile(t, filepath.Join(workspace, "memory", "MEMORY.md"))
	defer release()
	done := make(chan *ToolResult, 1)
	go func() {
		done <- NewWriteFileTool(workspace, true).Execute(context.Background(), map[string]any{"path": "artifacts/MEMORY.md", "content": "artifact"})
	}()
	select {
	case result := <-done:
		if result.IsError {
			t.Fatal(result.ForLLM)
		}
	case <-time.After(time.Second):
		t.Fatal("an unrelated artifact write waited for a memory lock")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "artifact" {
		t.Fatalf("independent artifact bytes = %q, error = %v", got, err)
	}
}

func TestMemoryToolsShareLockThroughArbitraryDirectoryAlias(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		for _, operation := range []string{"write", "edit", "append"} {
			t.Run(operation+map[bool]string{false: "/host", true: "/sandbox"}[restricted], func(t *testing.T) {
				workspace := t.TempDir()
				physical := filepath.Join(workspace, "data", "MEMORY.md")
				if err := fileutil.WriteFileAtomic(physical, []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
				for _, alias := range []string{"memory", "other-alias"} {
					if err := os.Symlink("data", filepath.Join(workspace, alias)); err != nil {
						t.Skipf("directory symlinks unavailable: %v", err)
					}
				}
				release := holdMemoryFile(t, filepath.Join(workspace, "memory", "MEMORY.md"))
				argument := filepath.Join(workspace, "other-alias", "MEMORY.md")
				if restricted {
					argument = filepath.Join("other-alias", "MEMORY.md")
				}
				done := make(chan *ToolResult, 1)
				go func() { done <- executeMemoryWrite(workspace, restricted, operation, argument) }()
				select {
				case result := <-done:
					release()
					t.Fatalf("directory alias bypassed the memory lock: %v", result)
				case <-time.After(25 * time.Millisecond):
				}
				release()
				if result := <-done; result.IsError {
					t.Fatal(result.ForLLM)
				}
			})
		}
	}
}

func TestMemoryToolsReplaceFinalSymlinkWithoutChangingTarget(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		for _, operation := range []string{"write", "edit", "append"} {
			t.Run(operation+map[bool]string{false: "/host", true: "/sandbox"}[restricted], func(t *testing.T) {
				workspace := t.TempDir()
				path, alias := filepath.Join(workspace, "memory", "MEMORY.md"), filepath.Join(workspace, "alias.md")
				if err := fileutil.WriteFileAtomic(path, []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join("memory", "MEMORY.md"), alias); err != nil {
					t.Skipf("file symlinks unavailable: %v", err)
				}
				argument := alias
				if restricted {
					argument = "alias.md"
				}
				if result := executeMemoryWrite(workspace, restricted, operation, argument); result.IsError {
					t.Fatal(result.ForLLM)
				}
				if got, err := os.ReadFile(path); err != nil || string(got) != "original" {
					t.Fatalf("final alias changed the memory target: %q, %v", got, err)
				}
				if info, err := os.Lstat(alias); err != nil || !info.Mode().IsRegular() {
					t.Fatalf("atomic write did not replace the final symlink entry: %v", err)
				}
			})
		}
	}
}

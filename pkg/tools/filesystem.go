package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// validatePath ensures the given path is within one of the allowed directories if restrict is true.
// The first element of allowedDirs is used for resolving relative paths.
func validatePath(path string, allowedDirs []string, restrict bool) (string, error) {
	if len(allowedDirs) == 0 || allowedDirs[0] == "" {
		return path, nil
	}

	workspace := allowedDirs[0]
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("failed to resolve workspace path: %w", err)
	}

	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		absPath, err = filepath.Abs(filepath.Join(absWorkspace, path))
		if err != nil {
			return "", fmt.Errorf("failed to resolve file path: %w", err)
		}
	}

	if restrict {
		if !isWithinAnyAllowedDir(absPath, allowedDirs) {
			return "", fmt.Errorf("access denied: path is outside allowed directories")
		}

		if err := checkSymlinksAgainstDirs(absPath, allowedDirs); err != nil {
			return "", err
		}
	}

	return absPath, nil
}

// isWithinAnyAllowedDir checks whether absPath falls within any of the allowed directories.
func isWithinAnyAllowedDir(absPath string, allowedDirs []string) bool {
	for _, dir := range allowedDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if isWithinWorkspace(absPath, absDir) {
			return true
		}
	}
	return false
}

// checkSymlinksAgainstDirs resolves symlinks and verifies the target is within any allowed directory.
func checkSymlinksAgainstDirs(absPath string, allowedDirs []string) error {
	// Resolve each allowed dir through symlinks
	resolvedDirs := make([]string, 0, len(allowedDirs))
	for _, dir := range allowedDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(absDir); err == nil {
			resolvedDirs = append(resolvedDirs, resolved)
		} else {
			resolvedDirs = append(resolvedDirs, absDir)
		}
	}

	resolved, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		if !isWithinAnyAllowedDir(resolved, resolvedDirs) {
			return fmt.Errorf("access denied: symlink resolves outside allowed directories")
		}
	} else if os.IsNotExist(err) {
		parentResolved, err := resolveExistingAncestor(filepath.Dir(absPath))
		if err == nil {
			if !isWithinAnyAllowedDir(parentResolved, resolvedDirs) {
				return fmt.Errorf("access denied: symlink resolves outside allowed directories")
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to resolve path: %w", err)
		}
	} else {
		return fmt.Errorf("failed to resolve path: %w", err)
	}

	return nil
}

func resolveExistingAncestor(path string) (string, error) {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if filepath.Dir(current) == current {
			return "", os.ErrNotExist
		}
	}
}

func isWithinWorkspace(candidate, workspace string) bool {
	rel, err := filepath.Rel(filepath.Clean(workspace), filepath.Clean(candidate))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

type ReadFileTool struct {
	allowedDirs []string
	restrict    bool
}

func NewReadFileTool(workspace string, restrict bool) *ReadFileTool {
	return &ReadFileTool{allowedDirs: []string{workspace}, restrict: restrict}
}

func NewReadFileToolWithDirs(allowedDirs []string, restrict bool) *ReadFileTool {
	return &ReadFileTool{allowedDirs: allowedDirs, restrict: restrict}
}

func (t *ReadFileTool) Name() string {
	return "read_file"
}

func (t *ReadFileTool) Description() string {
	return "Read the contents of a file"
}

func (t *ReadFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to read",
			},
		},
		"required": []string{"path"},
	}
}

func (t *ReadFileTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, ok := args["path"].(string)
	if !ok {
		return ErrorResult("path is required")
	}

	resolvedPath, err := validatePath(path, t.allowedDirs, t.restrict)
	if err != nil {
		return ErrorResult(err.Error())
	}

	content, err := os.ReadFile(resolvedPath)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to read file: %v", err))
	}

	return NewToolResult(string(content))
}

type WriteFileTool struct {
	allowedDirs []string
	restrict    bool
}

func NewWriteFileTool(workspace string, restrict bool) *WriteFileTool {
	return &WriteFileTool{allowedDirs: []string{workspace}, restrict: restrict}
}

func NewWriteFileToolWithDirs(allowedDirs []string, restrict bool) *WriteFileTool {
	return &WriteFileTool{allowedDirs: allowedDirs, restrict: restrict}
}

func (t *WriteFileTool) Name() string {
	return "write_file"
}

func (t *WriteFileTool) Description() string {
	return "Write content to a file"
}

func (t *WriteFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to write",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Content to write to the file",
			},
		},
		"required": []string{"path", "content"},
	}
}

func (t *WriteFileTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, ok := args["path"].(string)
	if !ok {
		return ErrorResult("path is required")
	}

	content, ok := args["content"].(string)
	if !ok {
		return ErrorResult("content is required")
	}

	resolvedPath, err := validatePath(path, t.allowedDirs, t.restrict)
	if err != nil {
		return ErrorResult(err.Error())
	}

	dir := filepath.Dir(resolvedPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ErrorResult(fmt.Sprintf("failed to create directory: %v", err))
	}

	if err := os.WriteFile(resolvedPath, []byte(content), 0o644); err != nil {
		return ErrorResult(fmt.Sprintf("failed to write file: %v", err))
	}

	return SilentResult(fmt.Sprintf("File written: %s", path))
}

type ListDirTool struct {
	allowedDirs []string
	restrict    bool
}

func NewListDirTool(workspace string, restrict bool) *ListDirTool {
	return &ListDirTool{allowedDirs: []string{workspace}, restrict: restrict}
}

func NewListDirToolWithDirs(allowedDirs []string, restrict bool) *ListDirTool {
	return &ListDirTool{allowedDirs: allowedDirs, restrict: restrict}
}

func (t *ListDirTool) Name() string {
	return "list_dir"
}

func (t *ListDirTool) Description() string {
	return "List files and directories in a path"
}

func (t *ListDirTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to list",
			},
		},
		"required": []string{"path"},
	}
}

func (t *ListDirTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, ok := args["path"].(string)
	if !ok {
		path = "."
	}

	resolvedPath, err := validatePath(path, t.allowedDirs, t.restrict)
	if err != nil {
		return ErrorResult(err.Error())
	}

	entries, err := os.ReadDir(resolvedPath)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to read directory: %v", err))
	}

	result := ""
	for _, entry := range entries {
		if entry.IsDir() {
			result += "DIR:  " + entry.Name() + "\n"
		} else {
			result += "FILE: " + entry.Name() + "\n"
		}
	}

	return NewToolResult(result)
}

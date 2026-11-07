package fstools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeModeFile creates a file with the given mode under dir and returns its path.
func writeModeFile(t *testing.T, dir, name string, mode os.FileMode, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("setup write failed: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("setup chmod failed: %v", err)
	}
	return path
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s) failed: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s: permissions = %o, want %o (mode must survive an edit)",
			filepath.Base(path), got, want)
	}
}

// TestEditFileTool_PreservesMode_Sandboxed reproduces the agent-visible bug:
// editing a script inside the workspace used to reset it to 0600 because the
// atomic write created a temp file with a fixed perm and renamed it over the
// target without consulting the existing mode.
func TestEditFileTool_PreservesMode_Sandboxed(t *testing.T) {
	workspace := t.TempDir()
	const name = "deploy.sh"
	path := writeModeFile(t, workspace, name, 0o755, "#!/bin/sh\necho original\n")

	tool := NewEditFileTool(workspace, true)
	result := tool.Execute(context.Background(), map[string]any{
		"path":     name,
		"old_text": "echo original",
		"new_text": "echo edited",
	})
	if result == nil || result.IsError {
		t.Fatalf("edit failed: %#v", result)
	}

	requireMode(t, path, 0o755)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if want := "#!/bin/sh\necho edited\n"; string(data) != want {
		t.Errorf("content = %q, want %q", string(data), want)
	}
}

// TestEditFileTool_PreservesMode_Unrestricted covers the hostFs path, which also
// hardcoded 0o600.
func TestEditFileTool_PreservesMode_Unrestricted(t *testing.T) {
	dir := t.TempDir()
	path := writeModeFile(t, dir, "deploy.sh", 0o755, "#!/bin/sh\necho original\n")

	tool := NewEditFileTool("", false)
	result := tool.Execute(context.Background(), map[string]any{
		"path":     path,
		"old_text": "echo original",
		"new_text": "echo edited",
	})
	if result == nil || result.IsError {
		t.Fatalf("edit failed: %#v", result)
	}

	requireMode(t, path, 0o755)
}

// TestAppendFileTool_PreservesMode verifies the sibling tool has the same guarantee.
func TestAppendFileTool_PreservesMode(t *testing.T) {
	workspace := t.TempDir()
	const name = "run.sh"
	path := writeModeFile(t, workspace, name, 0o750, "#!/bin/sh\n")

	tool := NewAppendFileTool(workspace, true)
	result := tool.Execute(context.Background(), map[string]any{
		"path":    name,
		"content": "echo appended\n",
	})
	if result == nil || result.IsError {
		t.Fatalf("append failed: %#v", result)
	}

	requireMode(t, path, 0o750)
}

// TestWriteFileTool_PreservesMode verifies that overwrite=true keeps the mode.
func TestWriteFileTool_PreservesMode(t *testing.T) {
	workspace := t.TempDir()
	const name = "script.sh"
	path := writeModeFile(t, workspace, name, 0o755, "#!/bin/sh\necho old\n")

	tool := NewWriteFileTool(workspace, true)
	result := tool.Execute(context.Background(), map[string]any{
		"path":      name,
		"content":   "#!/bin/sh\necho new\n",
		"overwrite": true,
	})
	if result == nil || result.IsError {
		t.Fatalf("write failed: %#v", result)
	}

	requireMode(t, path, 0o755)
}

// TestEditFileTool_PreservesModeThroughUmask verifies the mode is restored
// exactly, not merely "as masked by the process umask". A 0777 target would
// otherwise come back as 0755 under the common 022 umask because the temp file
// is created with OpenFile, which applies the umask.
func TestEditFileTool_PreservesModeThroughUmask(t *testing.T) {
	workspace := t.TempDir()
	const name = "wide.sh"
	path := writeModeFile(t, workspace, name, 0o777, "#!/bin/sh\necho original\n")

	tool := NewEditFileTool(workspace, true)
	result := tool.Execute(context.Background(), map[string]any{
		"path":     name,
		"old_text": "echo original",
		"new_text": "echo edited",
	})
	if result == nil || result.IsError {
		t.Fatalf("edit failed: %#v", result)
	}

	requireMode(t, path, 0o777)
}

// TestWriteFileTool_NewFileStillPrivate guards the security default: a brand new
// file created by the agent must not become world-readable.
func TestWriteFileTool_NewFileStillPrivate(t *testing.T) {
	workspace := t.TempDir()
	const name = "fresh.txt"
	path := filepath.Join(workspace, name)

	tool := NewWriteFileTool(workspace, true)
	result := tool.Execute(context.Background(), map[string]any{
		"path":    name,
		"content": "hello",
	})
	if result == nil || result.IsError {
		t.Fatalf("write failed: %#v", result)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("new file permissions = %o, want 0o600 (secure default)", got)
	}
}

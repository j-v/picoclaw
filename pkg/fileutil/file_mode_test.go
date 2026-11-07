package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileAtomic_PreservesExistingMode verifies that overwriting an existing
// file keeps its original permission bits instead of resetting them to the perm
// argument. Without this, editing a script through the agent file tools strips the
// execute bit and turns e.g. a deploy script into an unexecutable file.
func TestWriteFileAtomic_PreservesExistingMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "script.sh")

	if err := WriteFileAtomic(path, []byte("#!/bin/sh\necho one\n"), 0o644); err != nil {
		t.Fatalf("initial write failed: %v", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}

	// Overwrite with a perm argument that differs from the on-disk mode.
	if err := WriteFileAtomic(path, []byte("#!/bin/sh\necho two\n"), 0o600); err != nil {
		t.Fatalf("overwrite failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("permissions after overwrite = %o, want %o (existing mode must be preserved)", got, 0o755)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(data) != "#!/bin/sh\necho two\n" {
		t.Errorf("content after overwrite = %q, want updated content", string(data))
	}
}

// TestWriteFileAtomic_NewFileUsesPerm verifies that the perm argument still
// applies when the target does not exist yet (no regression for fresh files).
func TestWriteFileAtomic_NewFileUsesPerm(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name string
		perm os.FileMode
	}{
		{"0600", 0o600},
		{"0644", 0o644},
		{"0755", 0o755},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "new-"+tc.name+".txt")
			if err := WriteFileAtomic(path, []byte("data"), tc.perm); err != nil {
				t.Fatalf("write failed: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("Stat failed: %v", err)
			}
			if got := info.Mode().Perm(); got != tc.perm {
				t.Errorf("permissions for new file = %o, want %o", got, tc.perm)
			}
		})
	}
}

// TestWriteFileAtomic_PreservesReadOnlyMode verifies that a read-only file stays
// read-only after an overwrite, so a 0444 config cannot silently become 0600.
func TestWriteFileAtomic_PreservesReadOnlyMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "readonly.txt")

	if err := WriteFileAtomic(path, []byte("v1"), 0o644); err != nil {
		t.Fatalf("initial write failed: %v", err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	// Ensure the mode is restored even if the test fails midway.
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	if err := WriteFileAtomic(path, []byte("v2"), 0o600); err != nil {
		t.Fatalf("overwrite failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o444 {
		t.Errorf("permissions after overwrite = %o, want %o", got, 0o444)
	}
}

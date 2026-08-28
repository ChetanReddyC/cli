package paths

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// symlinkOrSkip creates a symlink, skipping the test where the platform or the
// account cannot make one (Windows without developer mode).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
}

func TestValidateEntireDirAt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, root string)
		wantErr bool
	}{
		{
			name:  "absent is fine",
			setup: func(*testing.T, string) {},
		},
		{
			name: "real directory is fine",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(root, EntireDir), 0o750); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "regular file is rejected",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, EntireDir), []byte("nope"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
		{
			name: "symlink to a directory is rejected",
			setup: func(t *testing.T, root string) {
				t.Helper()
				target := filepath.Join(root, "elsewhere")
				if err := os.Mkdir(target, 0o750); err != nil {
					t.Fatal(err)
				}
				symlinkOrSkip(t, target, filepath.Join(root, EntireDir))
			},
			wantErr: true,
		},
		{
			name: "symlink to a file is rejected",
			setup: func(t *testing.T, root string) {
				t.Helper()
				target := filepath.Join(root, "elsewhere")
				if err := os.WriteFile(target, []byte("nope"), 0o600); err != nil {
					t.Fatal(err)
				}
				symlinkOrSkip(t, target, filepath.Join(root, EntireDir))
			},
			wantErr: true,
		},
		{
			name: "dangling symlink is rejected",
			setup: func(t *testing.T, root string) {
				t.Helper()
				symlinkOrSkip(t, filepath.Join(root, "missing"), filepath.Join(root, EntireDir))
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			tt.setup(t, root)

			err := ValidateEntireDirAt(root)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ValidateEntireDirAt returned nil, want error")
				}
				if !errors.Is(err, ErrEntireDirNotDirectory) {
					t.Errorf("error does not wrap ErrEntireDirNotDirectory: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateEntireDirAt: %v", err)
			}
		})
	}
}

// A directory we cannot stat through is not proof the invariant holds, so it
// must fail rather than pass. Root bypasses permission bits, and Windows does
// not honour them here at all.
func TestValidateEntireDirAt_UnreadableParentFails(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("directory permission bits do not gate stat on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits")
	}

	root := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, EntireDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(root, 0o750); err != nil {
			t.Logf("restore permissions on %s: %v", root, err)
		}
	})

	err := ValidateEntireDirAt(root)
	if err == nil {
		t.Fatal("ValidateEntireDirAt returned nil for an unreadable parent, want error")
	}
	if errors.Is(err, ErrEntireDirNotDirectory) {
		t.Errorf("a stat failure must not be reported as a wrong file type: %v", err)
	}
}

// The message is what a user acts on, so it must name the path and what was
// found there.
func TestValidateEntireDirAt_MessageNamesPathAndType(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	entire := filepath.Join(root, EntireDir)
	symlinkOrSkip(t, root, entire)

	err := ValidateEntireDirAt(root)
	if err == nil {
		t.Fatal("ValidateEntireDirAt returned nil, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, entire) {
		t.Errorf("message %q does not name the path %q", msg, entire)
	}
	if !strings.Contains(msg, "symbolic link") {
		t.Errorf("message %q does not say what was found", msg)
	}
}

// Outside a git repository there is no worktree root, so there is no `.entire`
// to validate and the check is skipped. Commands that need a repository report
// its absence themselves, with a message about the repository rather than one
// about `.entire`.
//
// The stray `.entire` file proves the skip is real: were the check resolving a
// root some other way, this would trip it.
func TestRequireEntireDir_OutsideRepositoryIsSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, EntireDir), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(dir)
	ClearWorktreeRootCache()
	t.Cleanup(ClearWorktreeRootCache)

	if err := RequireEntireDir(context.Background()); err != nil {
		t.Fatalf("RequireEntireDir outside a repository: %v", err)
	}
}

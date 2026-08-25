package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteHooksDocument_RejectsInvalidProjectDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == testWindowsOS {
		t.Skip("directory symlinks require privileges on Windows")
	}

	tests := map[string]func(t *testing.T, root string){
		"git metadata symlink": func(t *testing.T, root string) {
			require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o750))
			require.NoError(t, os.Symlink(".git", filepath.Join(root, ".codex")))
		},
		"redirected directory": func(t *testing.T, root string) {
			require.NoError(t, os.Mkdir(filepath.Join(root, "redirected"), 0o750))
			require.NoError(t, os.Symlink("redirected", filepath.Join(root, ".codex")))
		},
		"regular file": func(t *testing.T, root string) {
			require.NoError(t, os.WriteFile(filepath.Join(root, ".codex"), []byte("not a directory"), 0o600))
		},
	}

	for name, arrange := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			arrange(t, root)
			worktreeHooks, err := resolveWorktreeHooksPath(root)
			require.NoError(t, err)

			err = writeHooksDocument(worktreeHooks, testUserHooksDocument())
			require.Error(t, err)
			require.NoFileExists(t, worktreeHooks.Path())
		})
	}
}

func TestWriteHooksDocument_RejectsHooksFileSymlinks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == testWindowsOS {
		t.Skip("file symlinks require privileges on Windows")
	}

	tests := map[string]func(t *testing.T, root string) string{
		"unrelated file in checkout": func(t *testing.T, root string) string {
			target := filepath.Join(root, "package.json")
			require.NoError(t, os.WriteFile(target, []byte(`{"keep":true}`), 0o640))
			return target
		},
		"file outside checkout": func(t *testing.T, _ string) string {
			target := filepath.Join(t.TempDir(), "unrelated.json")
			require.NoError(t, os.WriteFile(target, []byte(`{"keep":true}`), 0o640))
			return target
		},
	}

	for name, targetPath := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			worktreeHooks, err := resolveWorktreeHooksPath(root)
			require.NoError(t, err)
			require.NoError(t, os.Mkdir(filepath.Dir(worktreeHooks.Path()), 0o750))
			target := targetPath(t, root)
			before, err := os.ReadFile(target)
			require.NoError(t, err)
			require.NoError(t, os.Symlink(target, worktreeHooks.Path()))

			err = writeHooksDocument(worktreeHooks, testUserHooksDocument())
			require.ErrorContains(t, err, "symbolic link")
			after, err := os.ReadFile(target)
			require.NoError(t, err)
			require.Equal(t, before, after)
			info, err := os.Lstat(worktreeHooks.Path())
			require.NoError(t, err)
			require.NotZero(t, info.Mode()&os.ModeSymlink)
		})
	}
}

func TestWriteHooksDocument_RejectsNonRegularDestination(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	worktreeHooks, err := resolveWorktreeHooksPath(root)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(worktreeHooks.Path(), 0o750))

	err = writeHooksDocument(worktreeHooks, testUserHooksDocument())
	require.ErrorContains(t, err, "not a regular file")
	require.DirExists(t, worktreeHooks.Path())
}

func TestWriteHooksDocument_PreservesExistingMode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	worktreeHooks, err := resolveWorktreeHooksPath(root)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Dir(worktreeHooks.Path()), 0o750))
	require.NoError(t, os.WriteFile(worktreeHooks.Path(), []byte(`{}`), 0o640))
	require.NoError(t, os.Chmod(worktreeHooks.Path(), 0o640))

	require.NoError(t, writeHooksDocument(worktreeHooks, testUserHooksDocument()))
	info, err := os.Stat(worktreeHooks.Path())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

func TestWriteHooksDocument_NewFileUsesRestrictedMode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	worktreeHooks, err := resolveWorktreeHooksPath(root)
	require.NoError(t, err)

	require.NoError(t, writeHooksDocument(worktreeHooks, testUserHooksDocument()))
	info, err := os.Stat(worktreeHooks.Path())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func testUserHooksDocument() *hooksDocument {
	return &hooksDocument{
		topLevel: map[string]json.RawMessage{
			"user_setting": json.RawMessage(`{"keep":true}`),
		},
		rawHooks: make(map[string]json.RawMessage),
	}
}

package paths

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrEntireDirNotDirectory reports that `.entire` exists but is not a real
// directory. Callers match it with errors.Is to distinguish a broken repo from
// a stat that simply failed.
var ErrEntireDirNotDirectory = errors.New("not a directory")

// ValidateEntireDirAt reports whether worktreeRoot's `.entire` is safe to read
// and write through. It is safe when the path is absent (Entire is not enabled
// here yet, or `enable` is about to create it) or is a real directory. Anything
// else is a broken repo and the caller must not touch the path.
//
// The stat is Lstat, not Stat, so a symlink is rejected even when it points at
// a perfectly good directory. `.entire` holds session metadata, transcripts,
// and the settings that decide what gets redacted before it is committed, so a
// path someone else controls the far end of is not a path we write through.
//
// A stat error other than "not exist" is also a failure. It is not evidence
// that the invariant is violated, but neither is it evidence that it holds, and
// the caller's next move is to write there.
func ValidateEntireDirAt(worktreeRoot string) error {
	path := filepath.Join(worktreeRoot, EntireDir)

	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("cannot determine whether %s is a directory: %w", path, err)
	case info.Mode().IsDir():
		return nil
	}

	return fmt.Errorf("%s is %s, %w", path, describeMode(info.Mode()), ErrEntireDirNotDirectory)
}

// RequireEntireDir validates the current worktree's `.entire`. Outside a git
// repository there is no worktree root and so nothing to validate, which is not
// an error: commands that need a repository report its absence themselves, with
// a message about the repository rather than about `.entire`.
//
// Deliberately not memoized. The Lstat is free next to the `git rev-parse` that
// WorktreeRoot runs, and a cached "it was fine" is a stale answer in a
// long-lived process such as `entire mcp`.
func RequireEntireDir(ctx context.Context) error {
	root, err := WorktreeRoot(ctx)
	if err != nil {
		//nolint:nilerr // No repository means no `.entire` to validate. Reporting
		// the rev-parse failure here would make every command outside a repo fail
		// with a message about `.entire` instead of about the missing repository.
		return nil
	}
	return ValidateEntireDirAt(root)
}

// describeMode names what was found. The sentinel supplies the "not a
// directory" half of the sentence, so these read as the first half of "X is a
// symbolic link, not a directory".
func describeMode(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "a symbolic link"
	case mode.IsRegular():
		return "a regular file"
	case mode&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeDevice != 0:
		return "a device"
	default:
		return "of an unsupported type"
	}
}

package paths

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The four ways validating `.entire` can fail. Each is identified positively
// and carries a different remedy, which is why they are separate sentinels
// rather than one error plus an else branch: callers print the fix, and telling
// someone to reinstall git because their filesystem returned EACCES sends them
// after the wrong thing. A caller matching none of these must offer no remedy
// rather than guess at one.
var (
	// ErrEntireDirNotDirectory reports that `.entire` exists and is not a real
	// directory. The remedy is to inspect and replace the path.
	ErrEntireDirNotDirectory = errors.New("not a directory")

	// ErrEntireDirSymlinkedEntry reports that an entry directly under `.entire`
	// is a symbolic link. The remedy is to inspect that entry and replace it,
	// which is the same shape as ErrEntireDirNotDirectory's but not the same
	// sentence: `.entire/settings.json` is not required to be a directory, so
	// telling someone it is not one names the wrong problem.
	ErrEntireDirSymlinkedEntry = errors.New("is a symbolic link")

	// ErrEntireDirUnreadable reports that `.entire` could not be inspected at
	// all — a permission failure, an I/O error, a dead mount. Nothing is known
	// about what is at the path. The remedy is ownership, permissions, or the
	// filesystem itself.
	ErrEntireDirUnreadable = errors.New("cannot be inspected")

	// ErrRepositoryUnresolved reports that the worktree root could not be
	// determined for a reason other than there being no repository, so there is
	// no `.entire` path to inspect yet. The remedy is git.
	ErrRepositoryUnresolved = errors.New("cannot determine which repository this directory belongs to")
)

// ValidateEntireDirAt reports whether worktreeRoot's `.entire` is safe to read
// and write through. It is safe when the path is absent (Entire is not enabled
// here yet, or `enable` is about to create it), or is a real directory whose own
// entries are real files and directories. Anything else is a broken repo and
// the caller must not touch the path.
//
// The stat is Lstat, not Stat, so a symlink is rejected even when it points at
// a perfectly good directory. `.entire` holds session metadata, transcripts,
// and the settings that decide what gets redacted before it is committed, so a
// path someone else controls the far end of is not a path we write through.
//
// The same reasoning covers one level down, so the entries directly inside are
// checked too: a symlinked `.entire/metadata` redirects transcripts, and a
// symlinked `.entire/settings.local.json` redirects the file that names the
// command Entire executes at pre-push. See validateEntireDirEntries for why the
// scan stops there.
//
// The settings package refuses a symlinked settings file at the read itself as
// well (readConfined). Neither check subsumes the other: this one stops a
// command before it does anything, and covers the subdirectories no settings
// read touches, while that one covers the many callers that reach settings.Load
// without passing through a command's pre-run.
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
		return fmt.Errorf("%s %w: %w", path, ErrEntireDirUnreadable, err)
	case !info.Mode().IsDir():
		return fmt.Errorf("%s is %s, %w", path, describeMode(info.Mode()), ErrEntireDirNotDirectory)
	}

	return validateEntireDirEntries(path)
}

// validateEntireDirEntries rejects a symbolic link sitting directly inside a
// `.entire` already established to be a real directory.
//
// The entries one level down are Entire's own — `logs`, `metadata`, `tmp`, and
// the two settings files — so the reasoning that rejects a symlinked `.entire`
// applies to them unchanged: the far end is outside Entire's control, and for
// the settings files it decides what may be committed.
//
// Deliberately not recursive, and not a stricter type check. Walking deeper
// would mean traversing every session's transcripts on every command, and the
// checkpoint writer already skips symlinks as it walks the metadata directory.
// Entries of some other wrong type are also left alone: a FIFO at
// `.entire/settings.json` is a hang rather than a redirection, which is a
// different failure with a different fix.
func validateEntireDirEntries(dir string) error {
	entries, err := os.ReadDir(dir)

	// Checked before err, not after. os.ReadDir returns what it managed to read
	// alongside a partial-read error, and a symlink among those entries is a
	// positive finding — a stronger statement than "the listing failed", and
	// one with an actionable remedy.
	if symlinkErr := firstSymlinkedEntry(dir, entries); symlinkErr != nil {
		return symlinkErr
	}
	if err != nil {
		return fmt.Errorf("%s %w: %w", dir, ErrEntireDirUnreadable, err)
	}
	return nil
}

// firstSymlinkedEntry names the first symlink among entries and counts the
// rest, or returns nil when there is none.
//
// One error naming one entry, rather than one per entry: the remedy is the same
// for all of them, and a user who has to rerun the command once per planted
// link pays for our formatting choice. The named entry is the first in
// os.ReadDir's sorted order, so the message is deterministic.
//
// No Lstat of our own. DirEntry.Type() comes from the directory read itself on
// the platforms that report a type there, and where the filesystem does not,
// os.ReadDir does the Lstat internally — skipping an entry that vanished
// between the read and the stat, and surfacing any other failure as the
// partial-read error validateEntireDirEntries reports. Adding an Lstat here
// would reintroduce the vanished-entry race that os.ReadDir already handles.
func firstSymlinkedEntry(dir string, entries []os.DirEntry) error {
	var first string
	others := 0
	for _, entry := range entries {
		if entry.Type()&fs.ModeSymlink == 0 {
			continue
		}
		if first != "" {
			others++
			continue
		}
		first = filepath.Join(dir, entry.Name())
	}
	if first == "" {
		return nil
	}

	err := SymlinkedEntryError(first)
	if others > 0 {
		err = fmt.Errorf("%w%s", err, otherSymlinksClause(others, dir))
	}
	return err
}

// SymlinkedEntryError reports that path is a symbolic link, naming the target
// when it can be read. A Readlink that fails leaves the entry named, which is
// still enough to act on.
//
// Exported because this sentence has two producers — the `.entire` entry scan
// here and the settings reader, which refuses a symlinked settings file at the
// read itself (see readConfined in the settings package). One function so the
// two cannot drift into describing the same condition differently.
func SymlinkedEntryError(path string) error {
	if target, err := os.Readlink(path); err == nil {
		return fmt.Errorf("%s %w to %s", path, ErrEntireDirSymlinkedEntry, target)
	}
	return fmt.Errorf("%s %w", path, ErrEntireDirSymlinkedEntry)
}

// otherSymlinksClause accounts for symlinked entries beyond the one named.
// Number and verb are built together so they cannot disagree.
func otherSymlinksClause(n int, dir string) string {
	if n == 1 {
		return fmt.Sprintf(" (and 1 other entry under %s is too)", dir)
	}
	return fmt.Sprintf(" (and %d other entries under %s are too)", n, dir)
}

// RequireEntireDir validates the current worktree's `.entire`.
//
// Outside a git repository there is no worktree root and so nothing to
// validate, which is not an error: commands that need a repository report its
// absence themselves, with a message about the repository rather than about
// `.entire`. That skip requires git's positive ErrNotARepository verdict.
//
// Every other discovery failure — git missing from PATH, a cancelled context, a
// permission failure, dubious ownership, malformed output — fails closed. Those
// mean "we could not find out", and the consequence of guessing "no repository"
// is not merely a skipped check: settings resolution falls back to a path
// relative to the current directory when the root will not resolve
// (settingsAbsPaths in the settings package), so a guess would read
// ./.entire/settings.json — through the very symlink this exists to reject.
// Refusing to run on a machine whose git is broken is the cheaper mistake.
//
// Deliberately not memoized. The Lstat and the one-level listing are free next
// to the `git rev-parse` that WorktreeRoot runs — measured 8.2µs against a
// ~millisecond subprocess — and a cached "it was fine" is a stale answer in a
// long-lived process such as `entire mcp`.
func RequireEntireDir(ctx context.Context) error {
	root, err := WorktreeRoot(ctx)
	switch {
	case err == nil:
		return ValidateEntireDirAt(root)
	case errors.Is(err, ErrNotARepository):
		return nil
	default:
		return fmt.Errorf("%w, so %s cannot be verified: %w", ErrRepositoryUnresolved, EntireDir, err)
	}
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

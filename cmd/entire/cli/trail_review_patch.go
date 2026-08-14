package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6/plumbing"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
)

// trailReviewPatchAnchor is the pre-image a machine-applicable suggested change
// is pinned to: which file the patch rewrites, what that file hashed to when the
// finding was written, and the exact lines the patch expects to find there. The
// API requires all of it for a unified_diff ("unified_diff change requires
// expected_file_path" and four sibling checks), so these fields are mandatory
// rather than decorative — a patch sent without them is rejected outright.
type trailReviewPatchAnchor struct {
	FilePath  string
	FileHash  string
	StartLine int
	EndLine   int
	Lines     string
}

// resolveTrailReviewPatchAnchor derives the anchor from the patch plus the
// working tree: the diff headers name the file and the old-side line span, and
// the file on disk supplies the hash and the expected lines.
func resolveTrailReviewPatchAnchor(ctx context.Context, patch string) (*trailReviewPatchAnchor, error) {
	target, err := parseTrailReviewPatchTarget(patch)
	if err != nil {
		return nil, err
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	fullPath, ok := safeWorktreeFilePath(root, target.Path)
	if !ok {
		return nil, fmt.Errorf("patch path %s does not resolve inside the worktree", target.Path)
	}
	content, err := os.ReadFile(fullPath) //nolint:gosec // path is constrained to the current worktree root.
	if err != nil {
		return nil, fmt.Errorf("read %s to anchor the suggested change: %w", target.Path, err)
	}
	lines, err := patchAnchorLines(content, target.StartLine, target.EndLine)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target.Path, err)
	}
	// Resolved only once the patch and the file have checked out, so a rejected
	// patch does not pay for opening the repository.
	hash, err := gitBlobHash(content, trailReviewObjectFormat(ctx))
	if err != nil {
		return nil, fmt.Errorf("hash %s: %w", target.Path, err)
	}
	return &trailReviewPatchAnchor{
		FilePath:  target.Path,
		FileHash:  hash,
		StartLine: target.StartLine,
		EndLine:   target.EndLine,
		Lines:     lines,
	}, nil
}

// trailReviewObjectFormat reports the repository's git object format, so that
// expected_file_hash is the blob OID `git hash-object` prints in this repo
// rather than a sha1 value that a sha256 repository would never reproduce.
func trailReviewObjectFormat(ctx context.Context) format.ObjectFormat {
	repo, err := gitrepo.OpenCurrent(ctx)
	if err != nil {
		return format.SHA1
	}
	defer repo.Close()
	cfg, err := repo.Config()
	if err != nil || cfg.Extensions.ObjectFormat == format.UnsetObjectFormat {
		return format.SHA1
	}
	return cfg.Extensions.ObjectFormat
}

// trailReviewPatchTarget is the single file a suggested-change patch rewrites
// and the old-side line span it covers.
type trailReviewPatchTarget struct {
	Path      string
	StartLine int
	EndLine   int
}

// trailReviewHunkRangePattern captures the old-side range of a unified-diff hunk
// header: "@@ -12,7 +12,8 @@" yields start 12 and count 7, and an omitted count
// means a single line.
var trailReviewHunkRangePattern = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+`)

// parseTrailReviewPatchTarget reads the file and line span a unified diff
// targets. A suggestion anchors to exactly one file, so a multi-file patch is
// rejected here with an actionable message instead of as a server 400.
func parseTrailReviewPatchTarget(patch string) (trailReviewPatchTarget, error) {
	var (
		target  trailReviewPatchTarget
		oldPath string
		newPath string
		sawFile bool
		sawHunk bool
	)
	scanner := bufio.NewScanner(strings.NewReader(patch))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "--- "):
			if sawFile {
				return target, errors.New("patch targets multiple files; attach one suggested change per file")
			}
			sawFile = true
			oldPath = patchHeaderPath(line[4:])
		case strings.HasPrefix(line, "+++ "):
			newPath = patchHeaderPath(line[4:])
		case strings.HasPrefix(line, "@@ "):
			start, count, ok := parseUnifiedDiffHunkRange(line)
			if !ok {
				continue
			}
			end := start + count - 1
			if count == 0 {
				// A zero-length old range is a pure insertion (git -U0 output);
				// it anchors on the line the new text is inserted after.
				end = start
			}
			if !sawHunk || start < target.StartLine {
				target.StartLine = start
			}
			if end > target.EndLine {
				target.EndLine = end
			}
			sawHunk = true
		}
	}
	if err := scanner.Err(); err != nil {
		return target, fmt.Errorf("scan patch: %w", err)
	}
	if !sawFile {
		return target, errors.New("patch has no '--- <file>' header naming the file it changes")
	}
	if !sawHunk {
		return target, errors.New("patch has no '@@' hunk header")
	}
	if strings.TrimSpace(oldPath) == "/dev/null" || target.StartLine < 1 {
		// A suggestion is anchored to lines that already exist, so a patch that
		// creates a file has nothing to pin to. Say so rather than letting the
		// API reject expected_start_line 0.
		return target, fmt.Errorf("patch creates %s; a suggested change must modify an existing file (use --instruction instead)",
			strings.TrimSpace(cleanPatchPath(newPath)))
	}
	// The same whole-patch check the apply path runs, so a patch is held to one
	// path-safety standard whether it is being written or applied: this covers
	// the +++ side and any rename/copy headers, not just the --- path we anchor
	// to.
	if err := validateUnifiedDiffPatchPaths(patch); err != nil {
		return target, fmt.Errorf("unsafe patch path: %w", err)
	}
	target.Path = path.Clean(cleanPatchPath(oldPath))
	return target, nil
}

// parseUnifiedDiffHunkRange extracts the old-side start line and line count from
// a hunk header.
func parseUnifiedDiffHunkRange(line string) (start, count int, ok bool) {
	match := trailReviewHunkRangePattern.FindStringSubmatch(line)
	if match == nil {
		return 0, 0, false
	}
	start, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, false
	}
	count = 1
	if match[2] != "" {
		if count, err = strconv.Atoi(match[2]); err != nil {
			return 0, 0, false
		}
	}
	return start, count, true
}

// patchAnchorLines returns the file's bytes for lines [start, end] with their
// line endings intact, which is what expected_lines records: the exact slice a
// later apply compares against to tell whether the file has moved on. Unlike
// trailReviewSelectedTextFromWorktree, it must not normalize CRLF — the value is
// a byte-for-byte pre-image, not display text.
func patchAnchorLines(content []byte, start, end int) (string, error) {
	lines := strings.SplitAfter(string(content), "\n")
	// A trailing newline leaves a final empty element that is not a line.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	// end >= start >= 1 holds for every target the parser returns, so this one
	// bound covers both.
	if end > len(lines) {
		return "", fmt.Errorf("patch expects lines %d-%d but the file has %d", start, end, len(lines))
	}
	return strings.Join(lines[start-1:end], ""), nil
}

// gitBlobHash computes the git blob object ID of the given content, so the value
// stored as expected_file_hash matches `git hash-object <file>`.
func gitBlobHash(content []byte, objectFormat format.ObjectFormat) (string, error) {
	hasher := plumbing.NewHasher(objectFormat, plumbing.BlobObject, int64(len(content)))
	if _, err := hasher.Write(content); err != nil {
		return "", fmt.Errorf("write blob content to hasher: %w", err)
	}
	return hasher.Sum().String(), nil
}

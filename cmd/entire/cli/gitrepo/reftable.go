package gitrepo

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/storer"
	gogitstorage "github.com/go-git/go-git/v6/storage"
	gitfilesystem "github.com/go-git/go-git/v6/storage/filesystem"
)

// reftableGitTimeout bounds each git plumbing invocation the reftable storer
// makes. Ref reads/writes against the local reftable stack are fast; the
// timeout only guards against a wedged git process.
const reftableGitTimeout = 30 * time.Second

// repoUsesReftable reports whether the repository at the given git directories
// stores its references using the reftable backend rather than the classic
// loose-files + packed-refs layout.
//
// go-git (through the vendored v6 alpha) has no reftable reader: its filesystem
// storer reads refs from .git/refs, .git/packed-refs and .git/HEAD, none of
// which are authoritative in a reftable repository. Detection lets us route ref
// operations through the git CLI instead (see reftableStorer).
//
// A reftable repository is identified by the presence of a "reftable/"
// directory under either the worktree git dir or the common git dir. Git 2.45+
// creates this directory for `git init --ref-format=reftable` and for
// `git refs migrate --ref-format=reftable`. Checking the directory avoids
// parsing config and works for linked worktrees, where the shared stack lives
// under the common git dir.
func repoUsesReftable(dotGitPath, commonGitPath string) bool {
	candidates := []string{filepath.Join(dotGitPath, "reftable")}
	if commonGitPath != "" && commonGitPath != dotGitPath {
		candidates = append(candidates, filepath.Join(commonGitPath, "reftable"))
	}
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// reftableStorer adapts a filesystem-backed go-git storage so it can open and
// operate on repositories that use the reftable ref backend. Object, config,
// index, shallow and module storage keep flowing through the embedded
// filesystem storage (reftable only changes ref storage, not object storage);
// the reference-storer methods are overridden to shell out to the git CLI,
// which is the only reftable reader/writer available to us.
//
// It also advertises reftable support via the ExtensionChecker interface so
// go-git's extension verification does not reject the repository on open.
type reftableStorer struct {
	*gitfilesystem.Storage

	gitDir string
}

var (
	_ gogitstorage.Storer = (*reftableStorer)(nil)
	// The concrete methods below satisfy storer.ReferenceStorer, overriding the
	// embedded filesystem implementation.
	_ storer.ReferenceStorer = (*reftableStorer)(nil)
)

// newReftableStorer wraps a filesystem storage with reftable-aware reference
// handling. gitDir must be the repository's git directory (for a linked
// worktree, the worktree's git dir); git resolves the shared reftable stack
// from its commondir automatically.
func newReftableStorer(fs *gitfilesystem.Storage, gitDir string) *reftableStorer {
	return &reftableStorer{Storage: fs, gitDir: gitDir}
}

// SupportsExtension lets go-git open a repository that declares the
// extensions.refstorage=reftable extension. Object-format and other extensions
// remain the responsibility of the embedded filesystem storage / go-git core.
func (s *reftableStorer) SupportsExtension(name, _ string) bool {
	return strings.EqualFold(name, "refstorage")
}

// runGit runs a git plumbing command scoped to this repository's git dir and
// returns trimmed stdout. Only ref plumbing (show-ref, for-each-ref,
// symbolic-ref, update-ref, rev-parse) is used, none of which trigger git
// hooks, so this cannot recurse back into entire.
func (s *reftableStorer) runGit(args ...string) (string, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), reftableGitTimeout)
	defer cancel()

	full := append([]string{"--git-dir", s.gitDir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimRight(stdout.String(), "\n"), stderr.Bytes(), err
}

// SetReference stores a reference, dispatching to symbolic-ref for symbolic
// references and update-ref for hash references.
func (s *reftableStorer) SetReference(ref *plumbing.Reference) error {
	if ref == nil {
		return nil
	}
	if ref.Type() == plumbing.SymbolicReference {
		if _, stderr, err := s.runGit("symbolic-ref", ref.Name().String(), ref.Target().String()); err != nil {
			return fmt.Errorf("reftable set symbolic ref %s: %s: %w", ref.Name(), strings.TrimSpace(string(stderr)), err)
		}
		return nil
	}
	if _, stderr, err := s.runGit("update-ref", ref.Name().String(), ref.Hash().String()); err != nil {
		return fmt.Errorf("reftable set ref %s: %s: %w", ref.Name(), strings.TrimSpace(string(stderr)), err)
	}
	return nil
}

// CheckAndSetReference performs a compare-and-swap update. When old is non-nil
// the update is conditioned on the current value, mirroring go-git's atomic
// semantics; a mismatch is reported as storage.ErrReferenceHasChanged.
func (s *reftableStorer) CheckAndSetReference(newRef, old *plumbing.Reference) error {
	if newRef == nil {
		return nil
	}
	if newRef.Type() == plumbing.SymbolicReference {
		// Symbolic refs (e.g. HEAD) have no CAS form in update-ref; set directly.
		return s.SetReference(newRef)
	}
	if old == nil {
		return s.SetReference(newRef)
	}
	if _, _, err := s.runGit("update-ref", newRef.Name().String(), newRef.Hash().String(), old.Hash().String()); err != nil {
		// git update-ref fails when the stored value differs from the expected
		// old value; surface the sentinel go-git callers check for.
		return gogitstorage.ErrReferenceHasChanged
	}
	return nil
}

// Reference returns the reference with the given name, preserving symbolic refs
// (such as HEAD) so go-git can resolve them itself.
func (s *reftableStorer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	// Symbolic refs: symbolic-ref exits 0 and prints the target only for a
	// genuine symbolic ref; -q suppresses the error for non-symbolic names.
	if target, _, err := s.runGit("symbolic-ref", "-q", name.String()); err == nil && target != "" {
		return plumbing.NewSymbolicReference(name, plumbing.ReferenceName(target)), nil
	}

	// Hash refs: rev-parse --verify resolves the ref to the object it points at.
	// "^0" would peel tags; we want the ref's direct target, so verify the name
	// as-is. A non-existent ref exits non-zero.
	out, _, err := s.runGit("rev-parse", "--verify", "--quiet", name.String())
	if err != nil || out == "" {
		return nil, plumbing.ErrReferenceNotFound
	}
	h := plumbing.NewHash(out)
	if h.IsZero() {
		return nil, plumbing.ErrReferenceNotFound
	}
	return plumbing.NewHashReference(name, h), nil
}

// IterReferences returns an iterator over every reference in the repository,
// including HEAD, matching the behaviour of the filesystem storer.
func (s *reftableStorer) IterReferences() (storer.ReferenceIter, error) {
	refs := make([]*plumbing.Reference, 0, 16)

	// HEAD (symbolic on a branch, detached hash otherwise) is not emitted by
	// for-each-ref, so resolve it explicitly first.
	if target, _, err := s.runGit("symbolic-ref", "-q", "HEAD"); err == nil && target != "" {
		refs = append(refs, plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.ReferenceName(target)))
	} else if out, _, err := s.runGit("rev-parse", "--verify", "--quiet", "HEAD"); err == nil && out != "" {
		if h := plumbing.NewHash(out); !h.IsZero() {
			refs = append(refs, plumbing.NewHashReference(plumbing.HEAD, h))
		}
	}

	// All refs under refs/. %(symref) is non-empty only for symbolic refs so we
	// preserve their symbolic nature (e.g. refs/remotes/origin/HEAD).
	out, stderr, err := s.runGit("for-each-ref", "--format=%(objectname) %(refname) %(symref)")
	if err != nil {
		return nil, fmt.Errorf("reftable iterate refs: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) < 2 {
			continue
		}
		objectName, refName := fields[0], fields[1]
		symref := ""
		if len(fields) == 3 {
			symref = strings.TrimSpace(fields[2])
		}
		if symref != "" {
			refs = append(refs, plumbing.NewSymbolicReference(plumbing.ReferenceName(refName), plumbing.ReferenceName(symref)))
			continue
		}
		h := plumbing.NewHash(objectName)
		if h.IsZero() {
			continue
		}
		refs = append(refs, plumbing.NewHashReference(plumbing.ReferenceName(refName), h))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reftable scan refs: %w", err)
	}

	return storer.NewReferenceSliceIter(refs), nil
}

// RemoveReference deletes a reference. Deleting a missing ref is treated as a
// success so callers can remove idempotently, matching go-git's semantics.
func (s *reftableStorer) RemoveReference(name plumbing.ReferenceName) error {
	// A symbolic ref must be deleted with symbolic-ref -d; update-ref -d on a
	// symbolic ref would delete the ref it points at.
	if target, _, err := s.runGit("symbolic-ref", "-q", name.String()); err == nil && target != "" {
		if _, stderr, delErr := s.runGit("symbolic-ref", "-d", name.String()); delErr != nil {
			return fmt.Errorf("reftable remove symbolic ref %s: %s: %w", name, strings.TrimSpace(string(stderr)), delErr)
		}
		return nil
	}
	_, stderr, err := s.runGit("update-ref", "-d", name.String())
	if err != nil {
		msg := strings.ToLower(strings.TrimSpace(string(stderr)))
		// Deleting an already-absent ref is not an error for our callers.
		if msg == "" || strings.Contains(msg, "does not exist") || strings.Contains(msg, "not exist") {
			return nil
		}
		return fmt.Errorf("reftable remove ref %s: %s: %w", name, strings.TrimSpace(string(stderr)), err)
	}
	return nil
}

// CountLooseRefs returns 0: reftable has no loose refs, and go-git only uses
// this count to decide whether to pack loose refs, which is a no-op here.
func (s *reftableStorer) CountLooseRefs() (int, error) {
	return 0, nil
}

// PackRefs is a no-op: the reftable backend maintains its own compaction, so
// there is nothing for go-git to pack.
func (s *reftableStorer) PackRefs() error {
	return nil
}

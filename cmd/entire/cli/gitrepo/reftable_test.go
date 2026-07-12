package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	gitstorage "github.com/go-git/go-git/v6/storage"
	"github.com/stretchr/testify/require"
)

// initReftableRepo creates a repository using the reftable ref backend via the
// git CLI, or skips the test when the installed git is too old to support it.
// It returns the repo dir and the initial commit hash.
func initReftableRepo(t *testing.T, name, content string) (string, string) {
	t.Helper()
	repoDir := t.TempDir()

	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)

	initCmd := exec.Command("git", "init", "-b", "main", "--ref-format=reftable", repoDir) //nolint:noctx // test capability probe
	initCmd.Env = env
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Skipf("git does not support reftable repositories: %v\n%s", err, out)
	}

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...) //nolint:noctx // test helper
		cmd.Dir = repoDir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
		return strings.TrimSpace(string(out))
	}

	if got := git("rev-parse", "--show-ref-format"); got != "reftable" {
		t.Skipf("git initialized ref format %q, not reftable", got)
	}
	git("config", "user.name", "Test User")
	git("config", "user.email", "test@example.com")
	git("config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644))
	git("add", name)
	git("commit", "-m", "initial")

	return repoDir, git("rev-parse", "HEAD")
}

// reftableCommit adds a file and commits it in an existing reftable repo,
// returning the new HEAD hash. The repo's user identity is already configured
// by initReftableRepo, so only an isolated global/system config is supplied.
func reftableCommit(t *testing.T, repoDir, name, content string) string {
	t.Helper()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...) //nolint:noctx // test helper
		cmd.Dir = repoDir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
		return strings.TrimSpace(string(out))
	}
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644))
	run("add", name)
	run("commit", "-m", content)
	return run("rev-parse", "HEAD")
}

// TestCheckAndSetReference_ConflictVsError verifies that CheckAndSetReference
// maps only a genuine compare-and-swap conflict to storage.ErrReferenceHasChanged
// and surfaces unrelated failures (a new value pointing at a nonexistent object)
// as themselves. Callers such as strategy.atomicSetV1Ref branch on that sentinel
// to decide whether a privacy-critical push aborts because of concurrency or a
// real storage error, so misclassifying an I/O/object failure as a conflict is a
// correctness bug. Regression for the coarse error mapping in #547.
func TestCheckAndSetReference_ConflictVsError(t *testing.T) {
	t.Parallel()
	repoDir, headHash := initReftableRepo(t, "file.txt", "hello\n")

	repo, err := OpenPath(repoDir)
	require.NoError(t, err)
	defer repo.Close()

	refName := plumbing.ReferenceName("refs/entire/cas")
	head := plumbing.NewHash(headHash)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, head)))

	// A distinct, real object to swap the ref to.
	secondHash := plumbing.NewHash(reftableCommit(t, repoDir, "second.txt", "second\n"))
	require.NotEqual(t, head, secondHash)

	// Correct compare-and-swap succeeds: ref is at head, swap head -> second.
	require.NoError(t, repo.Storer.CheckAndSetReference(
		plumbing.NewHashReference(refName, secondHash),
		plumbing.NewHashReference(refName, head),
	))

	// Genuine conflict: ref is now at second, but we claim it is still at head.
	err = repo.Storer.CheckAndSetReference(
		plumbing.NewHashReference(refName, head),
		plumbing.NewHashReference(refName, head),
	)
	require.ErrorIs(t, err, gitstorage.ErrReferenceHasChanged,
		"a stale expected-old value must be reported as a CAS conflict")

	// Non-conflict failure: the expected-old value is correct (ref is at second),
	// but the new value points at a nonexistent object. git rejects the object,
	// not the CAS, so this must NOT be reported as a concurrency conflict.
	bogus := plumbing.NewHash("1111111111111111111111111111111111111111")
	err = repo.Storer.CheckAndSetReference(
		plumbing.NewHashReference(refName, bogus),
		plumbing.NewHashReference(refName, secondHash),
	)
	require.Error(t, err)
	require.NotErrorIs(t, err, gitstorage.ErrReferenceHasChanged,
		"a nonexistent-object write must not be misreported as a CAS conflict")

	// The failed CAS must not have advanced the ref.
	cur, err := repo.Storer.Reference(refName)
	require.NoError(t, err)
	require.Equal(t, secondHash, cur.Hash())
}

// TestReftableStorer_RefNamesAreArgvNotShell verifies that ref names are passed
// to git as argv, never interpolated into a shell command line. The injected
// name embeds a backtick command substitution with output redirection; if any
// method shelled out, the shell would create the marker file. Every method must
// instead treat the whole string as a literal ref name that round-trips.
func TestReftableStorer_RefNamesAreArgvNotShell(t *testing.T) {
	t.Parallel()
	repoDir, headHash := initReftableRepo(t, "file.txt", "hello\n")

	repo, err := OpenPath(repoDir)
	require.NoError(t, err)
	defer repo.Close()

	marker := filepath.Join(t.TempDir(), "PWNED")
	require.NotContains(t, marker, " ", "test temp path must be space-free for a valid ref name")
	// `>marker` is a shell redirection inside a backtick command substitution:
	// it creates marker if (and only if) the name is evaluated by a shell.
	injected := plumbing.ReferenceName("refs/entire/inj`>" + marker + "`tail")
	head := plumbing.NewHash(headHash)

	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(injected, head)))
	require.NoFileExists(t, marker, "ref name must not be evaluated by a shell on write")

	got, err := repo.Storer.Reference(injected)
	require.NoError(t, err)
	require.Equal(t, head, got.Hash())
	require.NoFileExists(t, marker, "ref name must not be evaluated by a shell on read")

	iter, err := repo.Storer.IterReferences()
	require.NoError(t, err)
	found := false
	require.NoError(t, iter.ForEach(func(r *plumbing.Reference) error {
		if r.Name() == injected {
			found = true
		}
		return nil
	}))
	iter.Close()
	require.True(t, found, "injected ref name must appear verbatim in iteration")

	require.NoError(t, repo.Storer.RemoveReference(injected))
	require.NoFileExists(t, marker, "ref name must not be evaluated by a shell on remove")
	_, err = repo.Storer.Reference(injected)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}

// TestOpenPath_ReftableRepository verifies that a reftable repository, which
// go-git's filesystem storer cannot open, is opened successfully and that ref
// read/write/list/remove all round-trip through the git-CLI-backed storer.
func TestOpenPath_ReftableRepository(t *testing.T) {
	t.Parallel()
	repoDir, headHash := initReftableRepo(t, "file.txt", "hello\n")

	repo, err := OpenPath(repoDir)
	require.NoError(t, err, "reftable repository should open")
	defer repo.Close()

	// HEAD resolves to the real branch, not the reftable .invalid stub.
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, "refs/heads/main", head.Name().String())
	require.Equal(t, headHash, head.Hash().String())

	// Write a new ref via go-git (routed through git update-ref) and read it back.
	newRef := plumbing.NewHashReference(plumbing.ReferenceName("refs/entire/test/one"), head.Hash())
	require.NoError(t, repo.Storer.SetReference(newRef))

	got, err := repo.Storer.Reference(newRef.Name())
	require.NoError(t, err)
	require.Equal(t, head.Hash(), got.Hash())

	// The new ref appears in iteration.
	iter, err := repo.Storer.IterReferences()
	require.NoError(t, err)
	found := false
	require.NoError(t, iter.ForEach(func(r *plumbing.Reference) error {
		if r.Name() == newRef.Name() {
			found = true
		}
		return nil
	}))
	iter.Close()
	require.True(t, found, "written ref should appear in IterReferences")

	// Removal round-trips, and removing again is a no-op.
	require.NoError(t, repo.Storer.RemoveReference(newRef.Name()))
	_, err = repo.Storer.Reference(newRef.Name())
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
	require.NoError(t, repo.Storer.RemoveReference(newRef.Name()))
}

// TestRepoUsesReftable_Detection checks that reftable detection distinguishes
// reftable repositories from classic files-backend repositories.
func TestRepoUsesReftable_Detection(t *testing.T) {
	t.Parallel()

	reftableRepo, _ := initReftableRepo(t, "a.txt", "a\n")
	require.True(t, repoUsesReftable(filepath.Join(reftableRepo, ".git"), filepath.Join(reftableRepo, ".git")))

	filesRepo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(filesRepo, ".git", "refs"), 0o755))
	require.False(t, repoUsesReftable(filepath.Join(filesRepo, ".git"), filepath.Join(filesRepo, ".git")))
}

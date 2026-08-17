package proctree

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain doubles as the helper process: when re-exec'd with
// PROCTREE_HELPER=1 it prints its own ancestors as JSON and exits, so tests
// can assert on the ancestry a real child process observes.
func TestMain(m *testing.M) {
	if os.Getenv("PROCTREE_HELPER") == "1" {
		refs := Ancestors(10)
		if err := json.NewEncoder(os.Stdout).Encode(refs); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRef_Self(t *testing.T) {
	t.Parallel()
	ref, err := Ref(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, os.Getpid(), ref.PID)
	assert.NotZero(t, ref.StartTime, "a live process must have a resolvable start time")
}

func TestAncestors_FirstIsParent(t *testing.T) {
	t.Parallel()
	refs := Ancestors(10)
	require.NotEmpty(t, refs)
	assert.Equal(t, os.Getppid(), refs[0].PID)
	for _, r := range refs {
		assert.Positive(t, r.PID)
	}
}

// The identity contract this package exists for: a process spawned by this
// test sees this test process in its ancestry, with a start time that matches
// what this process resolves for itself — so pid+starttime is a stable
// cross-process identity for "the same live process".
func TestAncestors_ChildSeesParentIdentity(t *testing.T) {
	t.Parallel()
	self, err := Ref(os.Getpid())
	require.NoError(t, err)

	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), exe)
	cmd.Env = append(os.Environ(), "PROCTREE_HELPER=1")
	out, err := cmd.Output()
	require.NoError(t, err, "helper process failed")

	var refs []ProcessRef
	require.NoError(t, json.Unmarshal(out, &refs))
	require.NotEmpty(t, refs)

	found := false
	for _, r := range refs {
		if r.SameProcess(self) {
			found = true
		}
	}
	assert.True(t, found, "child's ancestry must contain the parent's pid+starttime identity, got %+v (want %+v)", refs, self)
}

func TestSameProcess_RejectsPIDReuse(t *testing.T) {
	t.Parallel()
	a := ProcessRef{PID: 42, StartTime: 1000}
	b := ProcessRef{PID: 42, StartTime: 2000}
	assert.False(t, a.SameProcess(b), "same PID with a different start time is a recycled PID, not the same process")
	assert.True(t, a.SameProcess(ProcessRef{PID: 42, StartTime: 1000}))
}

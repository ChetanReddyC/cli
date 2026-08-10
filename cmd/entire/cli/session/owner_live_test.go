//go:build linux || darwin

package session

import (
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
)

func TestOwnerExited_DeadOwnerActiveIsTrue(t *testing.T) {
	t.Parallel()
	// Our own PID is alive, but a mismatched start fingerprint makes proclive
	// treat it as a reused (dead) PID — a deterministic "owner gone" signal.
	exitedOwner := &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"}
	s := &State{Phase: PhaseActive, Owner: exitedOwner}
	if s.OwnerLiveness() != proclive.LivenessDead {
		t.Fatalf("OwnerLiveness = %v, want dead", s.OwnerLiveness())
	}
	if !s.OwnerExited() {
		t.Error("OwnerExited(active, dead owner) = false, want true")
	}
}

func TestOwnerExited_DeadOwnerIdleIsTrue(t *testing.T) {
	t.Parallel()
	// The Codex shape: the agent finished its last turn (ACTIVE -> IDLE) and
	// then quit without firing a session-end hook. Such a session must still be
	// reclaimable, or it lingers as "active" in `entire status` until
	// StaleSessionThreshold hard-deletes it.
	exitedOwner := &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"}
	s := &State{Phase: PhaseIdle, Owner: exitedOwner}
	if !s.OwnerExited() {
		t.Error("OwnerExited(idle, dead owner) = false, want true")
	}
}

func TestOwnerExited_LiveOwnerIdleIsFalse(t *testing.T) {
	t.Parallel()
	// An idle session whose agent is still running (agent sitting at its prompt
	// between turns) must never be swept — that is the common case.
	id, ok := proclive.ResolveOwner()
	if !ok {
		t.Skip("no stable owner resolved in this environment")
	}
	s := &State{Phase: PhaseIdle, Owner: &id}
	if s.OwnerExited() {
		t.Error("OwnerExited(idle, live owner) = true, want false")
	}
}

func TestOwnerExited_LiveOwnerActiveIsFalse(t *testing.T) {
	t.Parallel()
	// A faithfully-captured identity of a live process must NOT read as exited.
	id, ok := proclive.ResolveOwner()
	if !ok {
		t.Skip("no stable owner resolved in this environment")
	}
	s := &State{Phase: PhaseActive, Owner: &id}
	if s.OwnerExited() {
		t.Error("OwnerExited(active, live owner) = true, want false")
	}
}

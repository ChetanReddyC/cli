package agentimport

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// progressSessionEvent and progressTurnEvent capture one Progress callback
// invocation each, in call order, for assertion.
type progressSessionEvent struct {
	sessionIndex, sessionTotal int
	agentName, sessionID       string
	turnCount                  int
}

type progressTurnEvent struct {
	sessionIndex, turnIndex, turnCount int
}

// TestRun_ReportsProgress proves SessionStart fires exactly once per session
// with correct totals, and TurnWritten fires exactly turnCount times per
// session, in order, on a fixture with 2 sessions x 2 turns each.
func TestRun_ReportsProgress(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")
	writeFixtureSession(t, claudeDir, "sess2.jsonl")

	var sessionEvents []progressSessionEvent
	var turnEvents []progressTurnEvent
	progress := &Progress{
		SessionStart: func(sessionIndex, sessionTotal int, agentName, sessionID string, turnCount int) {
			sessionEvents = append(sessionEvents, progressSessionEvent{sessionIndex, sessionTotal, agentName, sessionID, turnCount})
		},
		TurnWritten: func(sessionIndex, turnIndex, turnCount int) {
			turnEvents = append(turnEvents, progressTurnEvent{sessionIndex, turnIndex, turnCount})
		},
	}

	imp := claudeImporter{}
	res, err := Run(context.Background(), repo, imp, Options{
		RepoRoot: repoDir, OverridePath: claudeDir,
		Now:      time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		Progress: progress,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 4 {
		t.Fatalf("want 4 imported, got %+v", res)
	}

	wantAgentName := string(imp.AgentType())
	wantSessions := []progressSessionEvent{
		{sessionIndex: 0, sessionTotal: 2, agentName: wantAgentName, sessionID: "sess1", turnCount: 2},
		{sessionIndex: 1, sessionTotal: 2, agentName: wantAgentName, sessionID: "sess2", turnCount: 2},
	}
	if !reflect.DeepEqual(sessionEvents, wantSessions) {
		t.Fatalf("session events = %+v, want %+v", sessionEvents, wantSessions)
	}

	wantTurns := []progressTurnEvent{
		{sessionIndex: 0, turnIndex: 0, turnCount: 2},
		{sessionIndex: 0, turnIndex: 1, turnCount: 2},
		{sessionIndex: 1, turnIndex: 0, turnCount: 2},
		{sessionIndex: 1, turnIndex: 1, turnCount: 2},
	}
	if !reflect.DeepEqual(turnEvents, wantTurns) {
		t.Fatalf("turn events = %+v, want %+v", turnEvents, wantTurns)
	}
}

// TestRun_NilProgressDoesNotPanic proves a nil Progress (the zero value of
// Options.Progress) behaves identically to a reporter-enabled run: no panic,
// same turn count imported.
func TestRun_NilProgressDoesNotPanic(t *testing.T) {
	t.Parallel()
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")
	writeFixtureSession(t, claudeDir, "sess2.jsonl")
	now := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)

	repoNil, repoNilDir := initRepoWithCommit(t)
	resNil, err := Run(context.Background(), repoNil, claudeImporter{}, Options{
		RepoRoot: repoNilDir, OverridePath: claudeDir, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	repoWith, repoWithDir := initRepoWithCommit(t)
	resWith, err := Run(context.Background(), repoWith, claudeImporter{}, Options{
		RepoRoot: repoWithDir, OverridePath: claudeDir, Now: now,
		Progress: &Progress{
			SessionStart: func(int, int, string, string, int) {},
			TurnWritten:  func(int, int, int) {},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if resNil.TurnsImported != resWith.TurnsImported {
		t.Fatalf("nil progress imported %d turns, reporter-enabled imported %d", resNil.TurnsImported, resWith.TurnsImported)
	}
	if resNil.TurnsImported != 4 {
		t.Fatalf("want 4 imported, got %+v", resNil)
	}
}

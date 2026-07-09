package agentimport

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// fakeImporter is the minimal Importer needed to exercise writeSessionState.
type fakeImporter struct{}

func (fakeImporter) Name() string               { return "claude-code" }
func (fakeImporter) AgentType() types.AgentType { return agent.AgentTypeClaudeCode }
func (fakeImporter) Discover(_, _ string, _ time.Time, _ []string) ([]SessionFile, error) {
	return nil, nil
}
func (fakeImporter) SplitTurns(_ SessionFile, _ []byte) ([]Turn, error) { return nil, nil }

func totalImported(s *session.State) int {
	if s.TokenUsage == nil {
		return 0
	}
	return s.TokenUsage.InputTokens + s.TokenUsage.OutputTokens +
		s.TokenUsage.CacheCreationTokens + s.TokenUsage.CacheReadTokens
}

func TestWriteSessionState_CreatesListableImportedState(t *testing.T) {
	// Not parallel: t.Chdir + git-cwd resolution.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	ctx := context.Background()
	t0 := time.Now().Add(-48 * time.Hour)
	t1 := t0.Add(30 * time.Minute)
	sf := SessionFile{Path: "/does/not/matter.jsonl", SessionID: "11111111-1111-1111-1111-111111111111"}
	turns := []Turn{
		{UUID: "a", Prompt: "first prompt", Model: "claude-x", CreatedAt: t0, Tokens: &types.TokenUsage{InputTokens: 10, OutputTokens: 5}},
		{UUID: "b", Prompt: "second prompt", Model: "claude-x", CreatedAt: t1, Tokens: &types.TokenUsage{InputTokens: 3, OutputTokens: 2}},
	}

	if err := writeSessionState(ctx, fakeImporter{}, sf, turns); err != nil {
		t.Fatalf("writeSessionState: %v", err)
	}

	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}
	st, err := store.Load(ctx, sf.SessionID)
	if err != nil || st == nil {
		t.Fatalf("Load returned (%v, %v); want a state", st, err)
	}
	if st.Kind != session.KindImported {
		t.Errorf("Kind = %q, want %q", st.Kind, session.KindImported)
	}
	if st.BaseCommit != "" {
		t.Errorf("BaseCommit = %q, want empty (never HEAD-pinned)", st.BaseCommit)
	}
	if st.AgentType != agent.AgentTypeClaudeCode {
		t.Errorf("AgentType = %q, want %q", st.AgentType, agent.AgentTypeClaudeCode)
	}
	if !st.StartedAt.Equal(t0) {
		t.Errorf("StartedAt = %v, want %v (earliest turn)", st.StartedAt, t0)
	}
	if st.EndedAt == nil || !st.EndedAt.Equal(t1) {
		t.Errorf("EndedAt = %v, want %v (latest turn)", st.EndedAt, t1)
	}
	if st.StepCount != 2 {
		t.Errorf("StepCount = %d, want 2", st.StepCount)
	}
	if got := totalImported(st); got != 20 {
		t.Errorf("token total = %d, want 20", got)
	}
	if st.LastPrompt != "first prompt" {
		t.Errorf("LastPrompt = %q, want the opening prompt", st.LastPrompt)
	}
}

func TestWriteSessionState_DoesNotClobberLiveSession(t *testing.T) {
	// Not parallel: t.Chdir.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	ctx := context.Background()
	sid := "22222222-2222-2222-2222-222222222222"
	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}
	live := &session.State{SessionID: sid, Phase: session.PhaseActive, StartedAt: time.Now()}
	if err := store.Save(ctx, live); err != nil {
		t.Fatalf("seed live state: %v", err)
	}

	sf := SessionFile{Path: "/x.jsonl", SessionID: sid}
	turns := []Turn{{UUID: "a", Prompt: "p", CreatedAt: time.Now()}}
	if err := writeSessionState(ctx, fakeImporter{}, sf, turns); err != nil {
		t.Fatalf("writeSessionState: %v", err)
	}

	got, _ := store.Load(ctx, sid)
	if got == nil || got.Kind == session.KindImported || got.Phase != session.PhaseActive {
		t.Fatalf("import clobbered a live session: %+v", got)
	}
}

func TestImportedSessionSurvivesListing(t *testing.T) {
	// Not parallel: t.Chdir.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	ctx := context.Background()
	old := time.Now().Add(-30 * 24 * time.Hour) // 30 days > 7-day stale threshold
	sf := SessionFile{Path: "/x.jsonl", SessionID: "33333333-3333-3333-3333-333333333333"}
	turns := []Turn{{UUID: "a", Prompt: "p", CreatedAt: old}}
	if err := writeSessionState(ctx, fakeImporter{}, sf, turns); err != nil {
		t.Fatalf("writeSessionState: %v", err)
	}

	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		t.Fatalf("ListSessionStates: %v", err)
	}
	found := false
	for _, s := range states {
		if s.SessionID == sf.SessionID {
			found = true
		}
	}
	if !found {
		t.Fatal("imported session (30 days old) was not returned by ListSessionStates")
	}
}

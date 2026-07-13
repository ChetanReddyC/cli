//go:build integration

package integration

import (
	"testing"
)

// TestIssue1423_IDEContextTagStrippedFromSessionPrompt is a full-flow
// reproduction of #1423: Claude Code inside the VS Code extension prepends an
// <ide_opened_file> context block to the prompt, which leaked verbatim into the
// session/checkpoint title and prompt display.
//
// It drives the real user-prompt-submit hook binary with such a prompt and then
// reads the stored session state's LastPrompt (the value used for the session
// title / prompt display) — it must contain only what the user typed.
func TestIssue1423_IDEContextTagStrippedFromSessionPrompt(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/ide-tag")
	env.InitEntire()

	session := env.NewSession()
	prompt := "<ide_opened_file>The user opened /a/b.md in the IDE. This may or may not be related.</ide_opened_file>\n\nrewrite these docs as one plan"
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, prompt); err != nil {
		t.Fatalf("user-prompt-submit: %v", err)
	}

	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("get session state: %v", err)
	}
	const want = "rewrite these docs as one plan"
	if state.LastPrompt != want {
		t.Fatalf("session prompt/title not fully sanitized: got %q, want %q (#1423)", state.LastPrompt, want)
	}
}

package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// capturedSyncRemotesFileName is the per-clone captured-election state, stored
// in the git common dir (worktree-shared, like the push queue). List-shaped
// from day one: phase 1 caps membership at one remote, and lifting the cap
// (per-remote push-queue tracking) must not need a state migration.
const capturedSyncRemotesFileName = "entire-checkpoint-sync-remotes.json"

type capturedSyncRemotesFile struct {
	Remotes []string `json:"remotes"`
}

func capturedSyncRemotesPath(ctx context.Context) (string, error) {
	commonDir, err := GetGitCommonDir(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, capturedSyncRemotesFileName), nil
}

// loadCapturedSyncRemotes reads the captured election. Fail-soft: a missing,
// unreadable, or corrupt file reads as "nothing captured" — capture is
// automatic state, so unlike the explicit checkpoint_push_remote setting it
// must never fail sync closed.
func loadCapturedSyncRemotes(ctx context.Context) []string {
	path, err := capturedSyncRemotesPath(ctx)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the git common dir resolved from the repo itself, not user input.
	if err != nil {
		return nil
	}
	var f capturedSyncRemotesFile
	if err := json.Unmarshal(data, &f); err != nil {
		logging.Debug(ctx, "captured sync remotes file unreadable; ignoring",
			slog.String("error", err.Error()))
		return nil
	}
	return f.Remotes
}

// saveCapturedSyncRemotes writes the captured election atomically
// (temp+rename), so a concurrent reader never sees a partial file.
func saveCapturedSyncRemotes(ctx context.Context, remotes []string) error {
	path, err := capturedSyncRemotesPath(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(capturedSyncRemotesFile{Remotes: remotes})
	if err != nil {
		return fmt.Errorf("encode captured sync remotes: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write captured sync remotes: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit captured sync remotes: %w", err)
	}
	return nil
}

// maybeCaptureCheckpointSyncRemote elects pushRemote as the checkpoint sync
// remote when this push is consent-grade evidence that it is the user's own
// remote: the push target agrees with the branch's declared push destination
// (the config a bare `git push` resolves through). Declaration alone was the
// bug that got the tracking tier dropped from the election (74e239a9 — it
// elected remotes that never receive pushes); behavior alone is the pre-
// single-remote transcript leak. Capture acts only on their intersection,
// and announces itself — an election change must never be silent.
//
// Phase-1 rules: at most one captured remote, and the first capture sticks
// (a mixed-habit repo whose branches push two remotes must not flip the
// election on every push). The default-elected seed is displaceable once;
// after that, re-routing takes the explicit setting until the multi-remote
// set ships.
func maybeCaptureCheckpointSyncRemote(ctx context.Context, pushRemote string) {
	if !isConfiguredRemote(ctx, pushRemote) {
		return
	}
	if len(loadCapturedSyncRemotes(ctx)) > 0 {
		return
	}
	// Fail-closed on unreadable settings, same as the election itself: a
	// checkpoint_push_remote we could not read must not be overridden by a
	// capture.
	s, err := settings.Load(ctx)
	if err != nil || s.GetCheckpointPushRemote() != "" {
		return
	}
	elected, err := ResolveCheckpointSyncRemote(ctx)
	if err != nil || elected.Name == pushRemote {
		// The already-elected remote needs no capture — and persisting it
		// would block the one real capture this clone gets in phase 1.
		return
	}
	if declaredPushDestination(ctx) != pushRemote {
		return
	}
	if saveErr := saveCapturedSyncRemotes(ctx, []string{pushRemote}); saveErr != nil {
		logging.Warn(ctx, "failed to persist captured checkpoint sync remote",
			slog.String("remote", pushRemote),
			slog.String("error", saveErr.Error()))
		return
	}
	fmt.Fprintf(stderrWriter,
		"[entire] Checkpoints now sync to %q — the remote your branch pushes to. Override with strategy_options.checkpoint_push_remote in .entire/settings.local.json.\n",
		pushRemote)
	logging.Info(ctx, "checkpoint sync remote captured",
		slog.String("remote", pushRemote),
		slog.String("previously_elected", elected.Name))
}

// declaredPushDestination resolves where a bare `git push` on the current
// branch would go, through git's own precedence: branch.<name>.pushRemote,
// then remote.pushDefault, then branch.<name>.remote. Empty when HEAD is
// detached or nothing is declared.
//
// Phase-1 simplification: the pre-push hook receives only the remote name
// (refspecs are not plumbed through), so the declaration is read from HEAD's
// branch rather than the branches actually being pushed. A miss is
// conservative — no capture happens and the gate behaves as before.
func declaredPushDestination(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return ""
	}
	if v := gitConfigValue(ctx, "branch."+branch+".pushRemote"); v != "" {
		return v
	}
	if v := gitConfigValue(ctx, "remote.pushDefault"); v != "" {
		return v
	}
	return gitConfigValue(ctx, "branch."+branch+".remote")
}

// gitConfigValue returns a single git config value, or "" when unset or on
// any error.
func gitConfigValue(ctx context.Context, key string) string {
	out, err := exec.CommandContext(ctx, "git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

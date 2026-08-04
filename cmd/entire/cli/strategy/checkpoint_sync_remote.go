package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// CheckpointSyncRemoteSource identifies which precedence rule elected the
// checkpoint sync remote.
type CheckpointSyncRemoteSource string

const (
	// SyncRemoteSourceConfig: strategy_options.checkpoint_push_remote.
	SyncRemoteSourceConfig CheckpointSyncRemoteSource = "config"
	// SyncRemoteSourceTracking: the current branch's push destination —
	// branch.<name>.pushRemote, remote.pushDefault, or branch.<name>.remote.
	SyncRemoteSourceTracking CheckpointSyncRemoteSource = "tracking"
	// SyncRemoteSourceDefault: "origin" exists.
	SyncRemoteSourceDefault CheckpointSyncRemoteSource = "default"
	// SyncRemoteSourceSole: exactly one remote configured.
	SyncRemoteSourceSole CheckpointSyncRemoteSource = "sole"
	// SyncRemoteSourceFirst: first remote in .git/config order.
	SyncRemoteSourceFirst CheckpointSyncRemoteSource = "first"
)

// CheckpointSyncRemote is the single git remote elected to carry checkpoint
// data. Name is empty when no remotes are configured.
type CheckpointSyncRemote struct {
	Name   string
	Source CheckpointSyncRemoteSource
}

// ResolveCheckpointSyncRemote elects the one configured git remote that
// checkpoint data syncs to. Pure local lookup — no network. Precedence:
// checkpoint_push_remote setting (fail-closed if the named remote does not
// exist), then the current branch's own push destination (mirroring git's
// push resolution: branch.<name>.pushRemote, remote.pushDefault,
// branch.<name>.remote), then "origin", then the sole remote, then the first
// remote in .git/config order. It knows nothing about the checkpoint_remote
// URL feature; callers exempt that case themselves.
func ResolveCheckpointSyncRemote(ctx context.Context) (CheckpointSyncRemote, error) {
	// Fail closed on an unreadable settings file: election must never
	// override a checkpoint_push_remote the file may contain but we could
	// not read, or checkpoints would silently re-route away from the remote
	// the user configured for isolation.
	s, err := settings.Load(ctx)
	if err != nil {
		return CheckpointSyncRemote{}, fmt.Errorf("cannot read settings to resolve the checkpoint sync remote: %w", err)
	}
	if name := s.GetCheckpointPushRemote(); name != "" {
		if !isConfiguredRemote(ctx, name) {
			return CheckpointSyncRemote{}, fmt.Errorf(
				"checkpoint_push_remote %q is not a configured git remote; checkpoint sync disabled until fixed", name)
		}
		return CheckpointSyncRemote{Name: name, Source: SyncRemoteSourceConfig}, nil
	}

	if name := resolveBranchPushRemote(ctx); name != "" {
		return CheckpointSyncRemote{Name: name, Source: SyncRemoteSourceTracking}, nil
	}

	remotes := configuredRemotesInConfigOrder(ctx)
	switch {
	case len(remotes) == 0:
		return CheckpointSyncRemote{}, nil
	case slices.Contains(remotes, "origin"):
		return CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, nil
	case len(remotes) == 1:
		return CheckpointSyncRemote{Name: remotes[0], Source: SyncRemoteSourceSole}, nil
	default:
		return CheckpointSyncRemote{Name: remotes[0], Source: SyncRemoteSourceFirst}, nil
	}
}

// resolveBranchPushRemote mirrors git's own resolution of where `git push`
// would send the current branch: branch.<name>.pushRemote, then
// remote.pushDefault, then branch.<name>.remote — the first non-empty value
// wins (git's own precedence, not a cascade through all three). This is the
// fix for the common fork setup where origin is a base repo the user cannot
// push (cloned base, added a fork as "upstream", branched with `-u upstream`):
// origin would otherwise win the election and every checkpoint would strand
// locally.
//
// Returns "" on detached HEAD, when no tracking config applies, or when the
// resolved name is not a configured remote (dangling tracking left over from
// a removed remote) — that last case is git state, not user intent, so it
// does not fail closed; the caller simply falls through to the next
// precedence tier instead.
func resolveBranchPushRemote(ctx context.Context) string {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return ""
	}
	defer repo.Close()

	branch := GetCurrentBranchName(repo)
	if branch == "" {
		return ""
	}

	name := gitConfigValue(ctx, "branch."+branch+".pushRemote")
	if name == "" {
		name = gitConfigValue(ctx, "remote.pushDefault")
	}
	if name == "" {
		name = gitConfigValue(ctx, "branch."+branch+".remote")
	}
	if name == "" || !isConfiguredRemote(ctx, name) {
		return ""
	}
	return name
}

// gitConfigValue returns the effective value of a git config key (local
// overriding global, matching git's own resolution), or "" if unset or on
// error.
func gitConfigValue(ctx context.Context, key string) string {
	out, err := exec.CommandContext(ctx, "git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// checkpointSyncAllowedForRemote reports whether a push to pushRemote may
// carry checkpoint data. False for every remote except the elected
// checkpoint sync remote — including raw-URL pushes (git passes the URL as
// the hook arg) and the fail-closed misconfigured case. Callers exempt the
// dedicated checkpoint_remote URL mode before calling.
func checkpointSyncAllowedForRemote(ctx context.Context, pushRemote string) bool {
	syncRemote, err := ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		// Neutral wording: err covers both a misconfigured checkpoint_push_remote
		// and an unreadable settings file, and the wrapped error already carries
		// the specifics.
		logging.Warn(ctx, "checkpoint sync skipped: cannot resolve checkpoint sync remote",
			slog.String("error", err.Error()))
		return false
	}
	if syncRemote.Name == "" || syncRemote.Name != pushRemote {
		logging.Debug(ctx, "checkpoint sync skipped: push remote is not the checkpoint sync remote",
			slog.String("push_remote", pushRemote),
			slog.String("checkpoint_sync_remote", syncRemote.Name))
		return false
	}
	return true
}

// configuredRemotesInConfigOrder lists remote names in .git/config section
// order (approximates "first remote added"; `git remote` output is
// alphabetical and unsuitable). Remotes configured with only pushurl are
// deliberately invisible (spec Unit 1). Errors yield an empty list.
func configuredRemotesInConfigOrder(ctx context.Context) []string {
	out, err := exec.CommandContext(ctx, "git", "config", "--local", "--get-regexp", `^remote\..*\.url$`).Output()
	if err != nil {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// line: "remote.<name>.url <url>"; <name> may contain dots, so trim
		// the fixed prefix and the ".url <value>" suffix instead of splitting.
		key, _, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".url")
		if name == "" || name == key || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

package strategy

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// CheckpointReadRemotes returns the ordered, deduped remotes that checkpoint
// READS consult: the elected sync remote first, then "origin" as the legacy
// tier (pre-single-remote-sync checkpoints live there, and a fresh clone
// may lack the local settings that elected a non-origin remote).
//
// Unlike the write-side election, this fails OPEN: on an election error
// (misconfigured checkpoint_push_remote, unreadable settings) the chain is
// ["origin"] when configured. Writes fail closed to prevent leaking data;
// failing reads closed would only prevent FINDING data, with no privacy
// benefit. Callers must uphold the read-only rule for every candidate after
// the first: only the elected remote may seed or advance local refs.
//
// An empty result means no candidates — callers needing the "checkpoint
// absent" classification must gather their own positive evidence (successful
// remote listing, readable settings); an empty chain alone is not proof.
func CheckpointReadRemotes(ctx context.Context) []string {
	var candidates []string
	elected, err := ResolveCheckpointSyncRemote(ctx)
	switch {
	case err != nil:
		logging.Debug(ctx, "checkpoint reads: election failed, falling back to origin only",
			slog.String("error", err.Error()))
	case elected.Name != "":
		candidates = append(candidates, elected.Name)
	}
	if isConfiguredRemote(ctx, "origin") && (len(candidates) == 0 || candidates[0] != "origin") {
		candidates = append(candidates, "origin")
	}
	return candidates
}

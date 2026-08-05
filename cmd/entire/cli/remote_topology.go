package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Checkpoint destinations are unambiguous in the ordinary single-remote,
// single-URL repo and stop being unambiguous in two topologies users set up
// deliberately. Neither is broken, but in both the destination is decided by
// something other than "the repo I work in", so it is worth saying out loud once
// at `entire enable` and on demand from `entire doctor` rather than letting
// someone discover it when a resume comes up empty.

// remoteTopology summarizes the checkpoint-destination ambiguity in this repo.
type remoteTopology struct {
	// Remotes is every configured remote name, sorted.
	Remotes []string
	// MultiURLRemote names a remote that pushes to more than one URL ("" if
	// none), and PushURLs are its push URLs in the order git uses them.
	MultiURLRemote string
	PushURLs       []string
	// PrimaryIsRefs reports whether the git-refs backend is active, which
	// decides what a multi-URL remote means for checkpoints.
	PrimaryIsRefs bool
	// CheckpointRemote is the configured checkpoint_remote repo ("" if none).
	// When set, it already pins one explicit destination and there is nothing
	// ambiguous left to report.
	CheckpointRemote string
}

// inspectRemoteTopology reads the repo's remotes and checkpoint configuration.
// Best-effort and local-only (no network): every failure yields an empty
// topology, which reports nothing, because this is advisory output and must
// never obstruct enable or doctor.
func inspectRemoteTopology(ctx context.Context) remoteTopology {
	var t remoteTopology

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return t
	}

	remotes, err := listGitRemotes(ctx, repoRoot)
	if err != nil {
		logging.Debug(ctx, "remote topology: could not list remotes", slog.String("error", err.Error()))
		return t
	}
	for name := range remotes {
		t.Remotes = append(t.Remotes, name)
	}
	sort.Strings(t.Remotes)

	if s, err := settings.Load(ctx); err == nil {
		if cfg := s.GetCheckpointRemote(); cfg != nil {
			t.CheckpointRemote = cfg.Repo
		}
	}

	if cpCfg, err := settings.LoadCheckpointsConfig(ctx); err == nil {
		t.PrimaryIsRefs = checkpoint.PrimaryIsRefs(cpCfg)
	}

	// Report the first remote (in sorted order) that fans out, so the message is
	// stable rather than dependent on map iteration.
	for _, name := range t.Remotes {
		urls, err := gitremote.GetPushURLs(ctx, name)
		if err != nil || len(urls) < 2 {
			continue
		}
		t.MultiURLRemote = name
		t.PushURLs = urls
		break
	}

	return t
}

// hasAmbiguousDestination reports whether anything is worth telling the user.
func (t remoteTopology) hasAmbiguousDestination() bool {
	if t.CheckpointRemote != "" {
		return false
	}
	return len(t.Remotes) > 1 || t.MultiURLRemote != ""
}

// describeCheckpointDestination writes an explanation of where checkpoints will
// go. Writes nothing when the destination is unambiguous. indent prefixes every
// line so callers can nest it under a heading.
func (t remoteTopology) describeCheckpointDestination(w io.Writer, indent string) {
	if !t.hasAmbiguousDestination() {
		return
	}

	if t.MultiURLRemote != "" {
		fmt.Fprintf(w, "%sRemote %q pushes to %d URLs:\n", indent, t.MultiURLRemote, len(t.PushURLs))
		for i, u := range t.PushURLs {
			marker := "  "
			if i == 0 && t.PrimaryIsRefs {
				marker = "→ "
			}
			fmt.Fprintf(w, "%s  %s%s\n", indent, marker, redactForDisplay(u))
		}
		if t.PrimaryIsRefs {
			fmt.Fprintf(w, "%s  Checkpoints go to the first URL only; the others receive your code but no\n", indent)
			fmt.Fprintf(w, "%s  session history. Clone that first repository to resume sessions elsewhere.\n", indent)
		} else {
			fmt.Fprintf(w, "%s  Checkpoints are pushed to every URL. If one of them rejects or is\n", indent)
			fmt.Fprintf(w, "%s  unreachable, checkpoint sync reports a warning and retries on the next push.\n", indent)
		}
	}

	if len(t.Remotes) > 1 {
		fmt.Fprintf(w, "%sThis repo has %d remotes (%s).\n", indent, len(t.Remotes), strings.Join(t.Remotes, ", "))
		fmt.Fprintf(w, "%s  Checkpoints follow whichever remote you push to, while reading them back\n", indent)
		fmt.Fprintf(w, "%s  (resume, explain) always looks at origin — so checkpoints pushed elsewhere\n", indent)
		fmt.Fprintf(w, "%s  are not found again from this clone.\n", indent)
	}

	fmt.Fprintf(w, "%sTo pin one repository for checkpoints, set checkpoint_remote in .entire/settings.json\n", indent)
	fmt.Fprintf(w, "%s(or .entire/settings.local.json to keep it to this clone).\n", indent)
}

// printCheckpointDestinationNote is the `entire enable` half of the explanation
// `entire doctor` prints (checkCheckpointDestination): enable is where a user is
// most likely to be looking, and the least surprising moment to learn that this
// repo's remotes make the checkpoint destination a choice. Silent on the
// ordinary repo, so it adds nothing to the common enable output.
func printCheckpointDestinationNote(ctx context.Context, w io.Writer) {
	topology := inspectRemoteTopology(ctx)
	if !topology.hasAmbiguousDestination() {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Note: this repo's remotes make the checkpoint destination ambiguous.")
	topology.describeCheckpointDestination(w, "  ")
}

// redactForDisplay strips credentials from URL-shaped values and passes
// filesystem paths through unchanged (RedactURL renders a plain path as
// ":///path").
func redactForDisplay(u string) string {
	if strings.Contains(u, "://") || strings.Contains(u, "@") {
		return gitremote.RedactURL(u)
	}
	return u
}

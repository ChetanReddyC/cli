package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// migrateCheckpointsPushRemote is the remote the opt-in "push now" targets; the
// actual push URL is resolved from checkpoint-remote settings when configured.
const migrateCheckpointsPushRemote = "origin"

func newDoctorMigrateCheckpointsCmd() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "migrate-checkpoints",
		Short: "Convert git-branch checkpoints into per-checkpoint git refs (git-refs store)",
		Long: `Convert the checkpoints stored on the entire/checkpoints/v1 branch into
per-checkpoint refs under refs/entire/checkpoints/<shard>/<id>, the layout the
git-refs checkpoint store uses.

Each checkpoint's current tree is wrapped in a fresh commit and its ref is
pointed at it — existing branch commits are not remapped. The command is
idempotent: checkpoints already converted (their ref carries the same tree) are
skipped, so it is safe to re-run after more branch activity.

New refs are queued for push. Run interactively, it asks whether to push them
now; non-interactively it never pushes — the refs stay queued and flush on the
next push once the git-refs store is the configured primary.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			repo, err := strategy.OpenRepository(ctx)
			if err != nil {
				return fmt.Errorf("open repository: %w", err)
			}
			defer repo.Close()

			result, err := checkpoint.MigrateBranchToRefs(ctx, repo, dryRun)
			if err != nil {
				return fmt.Errorf("migrate checkpoints: %w", err)
			}

			if result.Total == 0 {
				fmt.Fprintln(out, "No checkpoints found on the v1 branch — nothing to migrate.")
				return nil
			}

			verb := "Migrated"
			if dryRun {
				verb = "Would migrate"
			}
			fmt.Fprintf(out, "%s %d checkpoint(s) to refs (%d already up to date, %d total).\n",
				verb, len(result.Migrated), result.Skipped, result.Total)

			if dryRun || len(result.Migrated) == 0 {
				return nil
			}

			if !interactive.CanPromptInteractively() {
				fmt.Fprintln(out, "Refs are queued; they push on the next `git push` once git-refs is the primary store.")
				return nil
			}

			title := fmt.Sprintf("Push %d migrated checkpoint ref(s) now?", len(result.Migrated))
			confirmed, err := confirmDoctorFix(ctx, out, title)
			if err != nil {
				return err
			}
			if !confirmed {
				fmt.Fprintln(out, "Refs stay queued for the next push.")
				return nil
			}

			pushed, err := strategy.PushMigratedCheckpointRefs(ctx, repo, migrateCheckpointsPushRemote)
			if err != nil {
				return fmt.Errorf("push migrated refs: %w", err)
			}
			fmt.Fprintf(out, "Pushed %d checkpoint ref(s).\n", pushed)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be migrated without writing refs")
	return cmd
}

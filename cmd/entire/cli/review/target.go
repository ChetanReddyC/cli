package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

type reviewWorktreeRunner func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error

func runTargetReview(ctx context.Context, cmd *cobra.Command, target string, modeSelected bool, deps Deps) error {
	if modeSelected {
		return errors.New("--target can only be used when running a review")
	}
	if deps.PrepareTarget == nil {
		return errors.New("review target checkout is unavailable")
	}
	worktreeRoot, err := deps.PrepareTarget(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), target)
	if err != nil {
		return err
	}
	return runReviewInWorktree(ctx, deps.RunInWorktree, worktreeRoot, stripReviewTargetArgs(os.Args[1:]), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func runReviewInWorktree(ctx context.Context, runner reviewWorktreeRunner, worktreeRoot string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if strings.TrimSpace(worktreeRoot) == "" {
		return errors.New("review target checkout returned an empty worktree path")
	}
	if runner != nil {
		return runner(ctx, worktreeRoot, args, stdin, stdout, stderr)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve entire executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = worktreeRoot
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run review in target worktree: %w", err)
	}
	return nil
}

func stripReviewTargetArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--target" {
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "--target=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

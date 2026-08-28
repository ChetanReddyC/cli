package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/spf13/cobra"
)

// skipEntireDirCheckAnnotation exempts a command from the `.entire` directory
// check. Guarded is the default, so a new command inherits the check without
// its author thinking about it, and the cost of forgetting the annotation is a
// false failure in a repo that is already broken rather than a write through a
// path someone else controls.
//
// Set it on a group root to cover the whole group: cobra does not propagate
// Annotations to children, so skipsEntireDirCheck walks the parent chain.
//
// Exempt the command only when it neither reads nor writes anything under
// `.entire` — control-plane and account commands, and `doctor`, which has to be
// able to run on a broken repo in order to report it. Every exemption needs an
// entry in the allowlist in entiredir_guard_test.go.
const (
	skipEntireDirCheckAnnotation = "entire_skip_entire_dir_check"
	skipEntireDirCheckEnabled    = "true"
)

// skipsEntireDirCheck reports whether cmd or any ancestor is exempt.
func skipsEntireDirCheck(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations[skipEntireDirCheckAnnotation] == skipEntireDirCheckEnabled {
			return true
		}
	}
	return false
}

// exemptFromEntireDirCheck marks cmd (and everything under it) exempt,
// preserving any annotations it already carries.
func exemptFromEntireDirCheck(cmd *cobra.Command) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[skipEntireDirCheckAnnotation] = skipEntireDirCheckEnabled
	return cmd
}

// checkEntireDirBeforeRun enforces the invariant that `.entire` is either
// absent or a real directory. It reports whether the caller may go on to touch
// the path, and returns the error a guarded command should fail with.
//
// Both return values matter, and they are not redundant. An exempt command gets
// (false, nil): it runs, but it must not build a logger or otherwise write
// under `.entire` — `ensureLogger` opens `.entire/logs/entire.log`, which on a
// symlinked `.entire` would create the very file we are refusing to write.
// Exemption means "this command's work does not depend on `.entire`", not
// "write through it anyway".
//
// The exempt path stays silent rather than warning. An exempt command has
// nothing to do with the repo's session data, and `doctor` reports the problem
// properly.
func checkEntireDirBeforeRun(cmd *cobra.Command) (safe bool, err error) {
	if verr := paths.RequireEntireDir(cmd.Context()); verr != nil {
		if skipsEntireDirCheck(cmd) {
			return false, nil
		}
		writeEntireDirRemedy(cmd.ErrOrStderr(), verr)
		return false, NewSilentError(verr)
	}
	return true, nil
}

// writeEntireDirRemedy prints what is wrong and what to do about it. The error
// alone names the problem but not the way out, and the way out is not obvious:
// the natural guess is that Entire is misconfigured, when in fact something has
// replaced a directory Entire owns, or git cannot say which repository this is.
//
// Those two are separate remedies, and printing the wrong one is worse than
// printing none. A discovery failure has established nothing about `.entire`,
// so telling the user to go replace it sends them after a file that may be
// perfectly fine.
func writeEntireDirRemedy(w io.Writer, err error) {
	fmt.Fprintf(w, "%v\n", err)
	fmt.Fprintf(w, "\n")

	if !errors.Is(err, paths.ErrEntireDirNotDirectory) {
		fmt.Fprintf(w, "Entire cannot tell which repository this directory belongs to, so it cannot\n")
		fmt.Fprintf(w, "verify that %s is a directory it owns, and will not read or write through\n", paths.EntireDir)
		fmt.Fprintf(w, "it on the strength of a guess.\n")
		fmt.Fprintf(w, "\n")
		fmt.Fprintf(w, "Fix: resolve the git error above. Common causes are git missing from PATH\n")
		fmt.Fprintf(w, "and git's ownership check on a mounted or shared repository, which names\n")
		fmt.Fprintf(w, "the `git config --global --add safe.directory` line to add.\n")
		return
	}

	fmt.Fprintf(w, "Entire keeps session metadata, transcripts, and the redaction settings that\n")
	fmt.Fprintf(w, "decide what may be committed under %s, and will not read or write through a\n", paths.EntireDir)
	fmt.Fprintf(w, "path that is not a real directory it owns.\n")
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "Fix: inspect the path, then either replace it with a real directory or remove\n")
	fmt.Fprintf(w, "it and run `entire enable` again. `entire doctor` still works here, and\n")
	fmt.Fprintf(w, "`entire <command> --help` is unaffected.\n")
}

// reportBrokenEntireDir is doctor's half of the guard. doctor is exempt from the
// root check so that it can run on a broken repo, which is only worth doing if
// it says what is wrong — so it diagnoses the problem on its own report channel
// and stops.
//
// It stops rather than continuing with the remaining checks because those read
// through `.entire`: doctor's PreRunE loads redaction settings from
// `.entire/settings.json`, and `doctor logs` / `doctor bundle` read
// `.entire/logs`. Conclusions drawn from config we cannot read are worse than
// no conclusions, and this problem is a prerequisite for every other fix
// anyway.
func reportBrokenEntireDir(cmd *cobra.Command) error {
	err := paths.RequireEntireDir(cmd.Context())
	if err == nil {
		return nil
	}

	w := cmd.OutOrStdout()

	if !errors.Is(err, paths.ErrEntireDirNotDirectory) {
		fmt.Fprintf(w, "%s: UNVERIFIED\n", paths.EntireDir)
		fmt.Fprintf(w, "  %v\n", err)
		fmt.Fprintf(w, "  Entire cannot tell which repository this directory belongs to, so it\n")
		fmt.Fprintf(w, "  cannot verify %s and every other command here is stopped.\n", paths.EntireDir)
		fmt.Fprintf(w, "  Fix: resolve the git error above. Common causes are git missing from\n")
		fmt.Fprintf(w, "  PATH and git's ownership check on a mounted or shared repository.\n")
		return NewSilentError(err)
	}

	fmt.Fprintf(w, "%s: BROKEN\n", paths.EntireDir)
	fmt.Fprintf(w, "  %v\n", err)
	fmt.Fprintf(w, "  Entire has refused to read or write through it, so every other command\n")
	fmt.Fprintf(w, "  in this repository is stopped and no session data is being captured.\n")
	fmt.Fprintf(w, "  Fix: inspect the path, then either replace it with a real directory or\n")
	fmt.Fprintf(w, "  remove it and run `entire enable` again.\n")
	fmt.Fprintf(w, "  Not auto-fixed: what is there may be someone's data, and deleting it is\n")
	fmt.Fprintf(w, "  not a call doctor should make on your behalf.\n")

	return NewSilentError(err)
}

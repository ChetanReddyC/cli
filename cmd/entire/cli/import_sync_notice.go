package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// importLoggedIn reports whether there is an active login the imported history
// could eventually sync under: an ENTIRE_TOKEN env token, or a current stored
// login context. It is local-only (reads env + contexts.json) and never makes a
// network call, so it is safe on the import path. It is a package var so tests
// can force either state.
var importLoggedIn = func() bool {
	if os.Getenv(auth.EnvTokenVar) != "" {
		return true
	}
	ctxs, current, err := auth.Contexts()
	return err == nil && current != "" && len(ctxs) > 0
}

// warnIfImportNotSynced prints a one-time notice, when the user is not logged
// in, that imported agent history is stored locally only and will not appear in
// the Entire dashboard. It is a no-op when logged in or when nothing local was
// imported.
//
// Import writes read-only checkpoints to the local entire/checkpoints/v1 store
// and never syncs on its own; sync happens later via the git pre-push hook once
// logged in. Importing while logged out therefore succeeds locally but silently
// never reaches the dashboard — this notice surfaces that instead of leaving the
// user to discover an empty dashboard (see issue #1773).
func warnIfImportNotSynced(w io.Writer, importedLocalHistory bool) {
	if !importedLocalHistory || importLoggedIn() {
		return
	}
	fmt.Fprintln(w, "Note: you're not logged in, so this history was imported locally only and won't appear in your Entire dashboard.")
	fmt.Fprintln(w, "Log in with 'entire login' before importing to have your history synced.")
}

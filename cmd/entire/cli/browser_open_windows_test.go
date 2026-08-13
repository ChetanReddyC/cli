package cli

import (
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// authorizeURLWithSeparators is shaped like a real authorization URL: url.Values
// sorts its keys, so client_id comes first and redirect_uri sits behind three
// `&` separators, and the redirect_uri itself is percent-encoded.
const authorizeURLWithSeparators = "https://us.auth.entire.io/authorize?" +
	"client_id=entire-cli&code_challenge=Zt1c&code_challenge_method=S256&" +
	"redirect_uri=http%3A%2F%2F127.0.0.1%3A54123%2Fcallback&response_type=code&" +
	"scope=cli+offline_access&state=Yg8q"

// TestOpenBrowserWindows_PassesWholeURLToShellAssociation is the regression
// test for a Windows `entire login` that opened
// https://us.auth.entire.io/authorize?client_id=entire-cli and was rejected
// with "auth request is missing redirect uri". The whole URL — every `&`
// separator and the percent-encoded redirect_uri — must reach the browser.
func TestOpenBrowserWindows_PassesWholeURLToShellAssociation(t *testing.T) {
	// No t.Parallel(): this swaps the package-level shellExecute seam.
	var got string
	original := shellExecute
	shellExecute = func(_ windows.Handle, _, file, _, _ *uint16, _ int32) error {
		got = windows.UTF16PtrToString(file)
		return nil
	}
	t.Cleanup(func() { shellExecute = original })

	if err := openBrowserPlatform(t.Context(), authorizeURLWithSeparators); err != nil {
		t.Fatalf("openBrowserPlatform: %v", err)
	}

	if got != authorizeURLWithSeparators {
		t.Errorf("URL handed to the shell association was mangled:\n got: %s\nwant: %s", got, authorizeURLWithSeparators)
	}
}

// TestOpenBrowserWindows_CmdTruncatesURLAtFirstAmpersand pins the root cause,
// so nobody reintroduces `cmd /c start "" <url>` as the launcher. cmd.exe
// treats an unquoted `&` as a command separator, and Go cannot help here:
// syscall.EscapeArg only quotes an argument containing a space, tab, quote, or
// backslash, and a URL has none of those, so it reaches cmd.exe bare. `echo`
// stands in for `start` so the test observes the truncation without launching
// anything.
func TestOpenBrowserWindows_CmdTruncatesURLAtFirstAmpersand(t *testing.T) {
	t.Parallel()

	// Stdout only: cmd also tries to run each `&`-separated remainder as a
	// command, and those failures land on stderr.
	out, _ := exec.CommandContext(t.Context(), "cmd", "/c", "echo", authorizeURLWithSeparators).Output() //nolint:errcheck // nonzero exit is expected; the truncated stdout is the observation
	echoed := strings.TrimSpace(string(out))

	// Exactly the URL the bug report showed: everything from code_challenge
	// onwards, redirect_uri included, was lost.
	const truncated = "https://us.auth.entire.io/authorize?client_id=entire-cli"
	if echoed != truncated {
		t.Fatalf("expected cmd.exe to truncate at the first ampersand\n got: %s\nwant: %s", echoed, truncated)
	}
}

package cli

import (
	"context"
	"strings"
	"testing"
)

func TestOpenBrowser_RefusesNonHTTPURL(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"file:///etc/passwd",
		"javascript:alert(1)",
		"ftp://example.test/x",
		"not a url at all",
		"",
	} {
		if err := openBrowser(context.Background(), raw); err == nil ||
			!strings.Contains(err.Error(), "refusing to open non-HTTP URL") {
			t.Errorf("openBrowser(%q) error = %v, want refusal", raw, err)
		}
	}
}

// TestOpenBrowser_AcceptsHTTPURLsWithoutLaunching checks that an authorization
// URL clears validation and reaches the platform launcher, which is stubbed out
// under test so no browser is spawned on a dev or CI host.
func TestOpenBrowser_AcceptsHTTPURLsWithoutLaunching(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"https://us.auth.entire.io/authorize?client_id=entire-cli&redirect_uri=http%3A%2F%2F127.0.0.1%3A1%2Fcb&state=x",
		"http://127.0.0.1:8080/callback?code=abc&state=x",
	} {
		err := openBrowser(context.Background(), raw)
		if err == nil || !strings.Contains(err.Error(), "browser unavailable under test") {
			t.Errorf("openBrowser(%q) error = %v, want the under-test sentinel (URL should pass validation)", raw, err)
		}
	}
}

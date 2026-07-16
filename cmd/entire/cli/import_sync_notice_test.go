package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestWarnIfImportNotSynced(t *testing.T) {
	// Mutates the package-level importLoggedIn seam, so it cannot run in
	// parallel with other tests that read it.
	orig := importLoggedIn
	t.Cleanup(func() { importLoggedIn = orig })

	cases := []struct {
		name       string
		loggedIn   bool
		imported   bool
		wantNotice bool
	}{
		{name: "logged out with imported history warns", loggedIn: false, imported: true, wantNotice: true},
		{name: "logged in does not warn", loggedIn: true, imported: true, wantNotice: false},
		{name: "nothing imported does not warn", loggedIn: false, imported: false, wantNotice: false},
		{name: "logged in and nothing imported does not warn", loggedIn: true, imported: false, wantNotice: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			importLoggedIn = func() bool { return tc.loggedIn }
			var buf bytes.Buffer
			warnIfImportNotSynced(&buf, tc.imported)
			got := buf.String()
			hasNotice := strings.Contains(got, "not logged in") && strings.Contains(got, "entire login")
			if hasNotice != tc.wantNotice {
				t.Fatalf("warnIfImportNotSynced(logged_in=%v, imported=%v): notice=%v, want %v; output=%q",
					tc.loggedIn, tc.imported, hasNotice, tc.wantNotice, got)
			}
		})
	}
}

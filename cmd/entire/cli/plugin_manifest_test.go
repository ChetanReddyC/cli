package cli

import (
	"strings"
	"testing"
	"time"
)

func TestPluginManifest_Roundtrip(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	in := &PluginManifest{
		Name:        "run",
		RepoURL:     "https://github.com/entireio/entire-run",
		Tag:         "v1.2.3",
		Asset:       "entire-run_1.2.3_darwin_arm64.tar.gz",
		SHA256:      "abc",
		Pinned:      true,
		InstalledAt: time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC),
		Requires:    []PluginRequirement{{Name: "sem", RepoURL: "https://github.com/entireio/entire-sem", MinVersion: "v0.2.0"}},
	}
	if err := SavePluginManifest(in); err != nil {
		t.Fatalf("SavePluginManifest: %v", err)
	}
	out, err := LoadPluginManifest("run")
	if err != nil {
		t.Fatalf("LoadPluginManifest: %v", err)
	}
	if out == nil || out.Tag != in.Tag || out.RepoURL != in.RepoURL || !out.Pinned || len(out.Requires) != 1 || out.Requires[0].MinVersion != "v0.2.0" {
		t.Errorf("roundtrip mismatch: %+v", out)
	}
}

func TestLoadPluginManifest_AbsentIsNilNil(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	m, err := LoadPluginManifest("ghost")
	if err != nil || m != nil {
		t.Errorf("LoadPluginManifest(ghost) = %v, %v; want nil, nil", m, err)
	}
}

func TestListPluginManifests_SortedAndTolerant(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	for _, name := range []string{"zeta", "alpha"} {
		if err := SavePluginManifest(&PluginManifest{Name: name, RepoURL: "https://x.example/" + name, Tag: "v1.0.0"}); err != nil {
			t.Fatal(err)
		}
	}
	// A pkg dir without a manifest (half-removed plugin) must not break listing.
	if _, err := EnsurePluginPkgDir("broken"); err != nil {
		t.Fatal(err)
	}
	got, err := ListPluginManifests()
	if err != nil {
		t.Fatalf("ListPluginManifests: %v", err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Errorf("ListPluginManifests = %+v, want [alpha zeta]", got)
	}
}

func TestRemovePluginPkg(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	if err := SavePluginManifest(&PluginManifest{Name: "run", RepoURL: "https://x.example/run", Tag: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := RemovePluginPkg("run"); err != nil {
		t.Fatalf("RemovePluginPkg: %v", err)
	}
	if m, err := LoadPluginManifest("run"); err != nil || m != nil {
		t.Error("manifest survived RemovePluginPkg")
	}
	// Removing a never-installed pkg is not an error.
	if err := RemovePluginPkg("ghost"); err != nil {
		t.Errorf("RemovePluginPkg(ghost) = %v, want nil", err)
	}
}

func TestParsePluginMetadata(t *testing.T) {
	t.Parallel()
	meta, err := ParsePluginMetadata([]byte(`
name: brain
description: Repository memory
download_url: "https://dl.example.com/{tag}/{asset}"
requires:
  - name: sem
    repo_url: https://github.com/entireio/entire-sem
    min_version: v0.2.0
`))
	if err != nil {
		t.Fatalf("ParsePluginMetadata: %v", err)
	}
	if meta.Name != "brain" || len(meta.Requires) != 1 || meta.Requires[0].Name != "sem" {
		t.Errorf("parsed %+v", meta)
	}

	// Unknown keys are author typos; strict decoding surfaces them. The
	// key must NOT be a near-miss spelling of a real field: a spell-fixing
	// formatter pass once rewrote such a key into the correctly-spelled
	// field name, which made the input valid and the test vacuous.
	if _, err := ParsePluginMetadata([]byte("name: x\nnot_a_real_field:\n  - name: y\n")); err == nil {
		t.Error("ParsePluginMetadata accepted unknown key")
	}
	// Reserved names rejected.
	if _, err := ParsePluginMetadata([]byte("name: agent-evil\n")); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("ParsePluginMetadata(agent-evil) = %v, want reserved-name error", err)
	}
	if _, err := ParsePluginMetadata([]byte("name: ok\nrequires:\n  - name: agent-evil\n")); err == nil {
		t.Error("ParsePluginMetadata accepted reserved requirement name")
	}
	// A requirement's repo_url is author-controlled and reaches the git CLI
	// during dependency planning — which runs before the confirmation prompt.
	for _, bad := range []string{
		"--upload-pack=touch /tmp/pwned; git-upload-pack",
		"ext::sh -c whoami",
		"not-a-url",
	} {
		yml := "name: ok\nrequires:\n  - name: dep\n    repo_url: \"" + bad + "\"\n"
		if _, err := ParsePluginMetadata([]byte(yml)); err == nil {
			t.Errorf("ParsePluginMetadata accepted requirement repo_url %q", bad)
		}
	}
}

// entire-plugin.yml is documented as optional and a missing file is handled, so
// a committed placeholder must not be fatal at every tag. An empty stream
// decodes to io.EOF, which used to abort the whole install.
func TestParsePluginMetadata_EmptyIsNotAnError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ label, body string }{
		{"empty", ""},
		{"comment only", "# nothing yet\n"},
		{"whitespace", "\n\n  \n"},
		{"explicit empty document", "---\n"},
	} {
		meta, err := ParsePluginMetadata([]byte(tc.body))
		if err != nil {
			t.Errorf("%s: ParsePluginMetadata = %v, want no error", tc.label, err)
			continue
		}
		if meta == nil {
			t.Errorf("%s: meta is nil, want an empty metadata value", tc.label)
			continue
		}
		if meta.Name != "" || len(meta.Requires) != 0 {
			t.Errorf("%s: meta = %+v, want zero value", tc.label, meta)
		}
	}
	// Genuine syntax errors must still fail.
	if _, err := ParsePluginMetadata([]byte("name: [unclosed\n")); err == nil {
		t.Error("ParsePluginMetadata accepted malformed YAML")
	}
}

// Credentials embedded in a remote must not be persisted: manifest.yml is mode
// 0644 and upgrades re-resolve auth through git's credential helpers.
func TestSavePluginManifest_StripsCredentials(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	if err := SavePluginManifest(&PluginManifest{
		Name:    "demo",
		RepoURL: "https://bob:hunter2@git.example.com/o/entire-demo",
		Tag:     "v1.0.0",
		Requires: []PluginRequirement{
			{Name: "dep", RepoURL: "https://tok@git.example.com/o/entire-dep"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := LoadPluginManifest("demo")
	if err != nil || m == nil {
		t.Fatalf("LoadPluginManifest = %v, %v", m, err)
	}
	if strings.Contains(m.RepoURL, "hunter2") || strings.Contains(m.RepoURL, "bob") {
		t.Errorf("repo_url kept credentials: %q", m.RepoURL)
	}
	if len(m.Requires) != 1 || strings.Contains(m.Requires[0].RepoURL, "tok@") {
		t.Errorf("requirement repo_url kept credentials: %+v", m.Requires)
	}
}

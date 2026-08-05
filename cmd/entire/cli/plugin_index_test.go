package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// newIndexRepo creates a git repo holding index.json and returns its
// file:// URL.
func newIndexRepo(t *testing.T, indexJSON string) (url, dir string) {
	t.Helper()
	dir = t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, pluginIndexFileName, indexJSON)
	testutil.GitAdd(t, dir, pluginIndexFileName)
	testutil.GitCommit(t, dir, "index")
	return "file://" + filepath.ToSlash(dir), dir
}

const testIndexJSON = `{
  "version": 1,
  "plugins": [
    {"name": "run", "repo_url": "https://github.com/entireio/entire-run", "description": "Run apps", "official": true},
    {"name": "sem", "repo_url": "https://github.com/entireio/entire-sem", "description": "Semantic search"},
    {"name": "agent-bad", "repo_url": "https://example.com/x", "description": "invalid, filtered"},
    {"name": "noend", "repo_url": "", "description": "missing repo, filtered"}
  ]
}`

func TestSyncPluginIndex_CloneSearchFind(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	url, _ := newIndexRepo(t, testIndexJSON)
	idx, err := SyncPluginIndex(context.Background(), url, false)
	if err != nil {
		t.Fatalf("SyncPluginIndex: %v", err)
	}
	// Invalid entries (reserved name, empty repo) are filtered, not fatal.
	if len(idx.Plugins) != 2 {
		t.Fatalf("plugins = %+v, want 2 valid entries", idx.Plugins)
	}
	if e := idx.Find("run"); e == nil || !e.Official {
		t.Errorf("Find(run) = %+v", e)
	}
	if got := idx.Search("semantic"); len(got) != 1 || got[0].Name != "sem" {
		t.Errorf("Search(semantic) = %+v", got)
	}
	if !idx.HasRepoURL("https://github.com/entireio/entire-run.git") {
		t.Error("HasRepoURL should normalize .git suffix")
	}
	if idx.HasRepoURL("https://github.com/entireio/entire-other") {
		t.Error("HasRepoURL matched an unlisted repo")
	}
}

func TestSyncPluginIndex_RefreshPicksUpNewEntries(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	url, dir := newIndexRepo(t, `{"version":1,"plugins":[{"name":"run","repo_url":"https://x.example/entire-run"}]}`)
	ctx := context.Background()
	idx, err := SyncPluginIndex(ctx, url, false)
	if err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Fatalf("initial plugins = %d, want 1", len(idx.Plugins))
	}

	testutil.WriteFile(t, dir, pluginIndexFileName, `{"version":1,"plugins":[{"name":"run","repo_url":"https://x.example/entire-run"},{"name":"sem","repo_url":"https://x.example/entire-sem"}]}`)
	testutil.GitAdd(t, dir, pluginIndexFileName)
	testutil.GitCommit(t, dir, "add sem")

	// Within TTL the cached copy is served...
	idx, err = SyncPluginIndex(ctx, url, false)
	if err != nil {
		t.Fatalf("cached sync: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Errorf("TTL-fresh sync re-fetched: got %d plugins", len(idx.Plugins))
	}
	// ...force bypasses the TTL.
	idx, err = SyncPluginIndex(ctx, url, true)
	if err != nil {
		t.Fatalf("forced sync: %v", err)
	}
	if len(idx.Plugins) != 2 {
		t.Errorf("forced sync plugins = %d, want 2", len(idx.Plugins))
	}
}

func TestSyncPluginIndex_OfflineUsesStaleCopy(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	url, dir := newIndexRepo(t, `{"version":1,"plugins":[{"name":"run","repo_url":"https://x.example/entire-run"}]}`)
	ctx := context.Background()
	if _, err := SyncPluginIndex(ctx, url, false); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	// Simulate the remote disappearing (laptop offline / index moved).
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove remote dir: %v", err)
	}
	idx, err := SyncPluginIndex(ctx, url, true) // force → refresh fails → stale copy
	if err != nil {
		t.Fatalf("offline sync should fall back to cache: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Errorf("stale copy plugins = %d, want 1", len(idx.Plugins))
	}
}

// version is advisory, not a gate. Enforcing it would guard a migration that
// can never happen — the index is one shared resource read by every shipped
// CLI, so a bump breaks discovery fleet-wide — while punishing the case that
// does happen: an index (often hand-written, as an internal catalog set via
// ENTIRE_PLUGIN_INDEX_URL is) that omits the field, or one that grows a field
// this CLI ignores.
func TestSyncPluginIndex_VersionIsAdvisory(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	entry := `{"name":"run","repo_url":"https://x.example/entire-run"}`
	for _, tt := range []struct{ label, json string }{
		{label: "omitted", json: `{"plugins":[` + entry + `]}`},
		{label: "current", json: `{"version":1,"plugins":[` + entry + `]}`},
		{label: "future", json: `{"version":99,"plugins":[` + entry + `]}`},
		{label: "future with unknown fields", json: `{"version":99,"generated_at":"x","plugins":[` + entry + `]}`},
	} {
		url, _ := newIndexRepo(t, tt.json)
		idx, err := SyncPluginIndex(context.Background(), url, false)
		if err != nil {
			t.Errorf("%s: SyncPluginIndex = %v, want the catalog to load", tt.label, err)
			continue
		}
		if idx.Find("run") == nil {
			t.Errorf("%s: entry dropped; got %+v", tt.label, idx.Plugins)
		}
	}
}

// An index entry whose repo_url is option-shaped would reach the git CLI on
// an index-resolved install, which is treated as trusted and never prompts.
// Bad entries are dropped the same way an invalid name is, so one hostile
// row can't take out the whole catalog.
func TestSyncPluginIndex_DropsEntriesWithUnusableRepoURL(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	url, _ := newIndexRepo(t, `{"version":1,"plugins":[
		{"name":"evil","repo_url":"--upload-pack=touch /tmp/pwned; git-upload-pack"},
		{"name":"alsoevil","repo_url":"ext::sh -c whoami"},
		{"name":"good","repo_url":"https://github.com/entireio/entire-good"}
	]}`)
	idx, err := SyncPluginIndex(context.Background(), url, false)
	if err != nil {
		t.Fatalf("SyncPluginIndex: %v", err)
	}
	if idx.Find("evil") != nil || idx.Find("alsoevil") != nil {
		t.Error("index kept an entry whose repo_url is not a usable git URL")
	}
	if idx.Find("good") == nil {
		t.Error("index dropped the valid entry alongside the bad ones")
	}
}

// The index URL itself reaches `git clone` as a positional, and --index /
// ENTIRE_PLUGIN_INDEX_URL bypass the settings validator entirely.
func TestSyncPluginIndex_RejectsUnusableIndexURL(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if _, err := SyncPluginIndex(context.Background(), "--upload-pack=touch /tmp/pwned; git-upload-pack", false); err == nil {
		t.Error("SyncPluginIndex accepted an option-shaped index URL")
	}
}

// Precedence is --index > ENTIRE_PLUGIN_INDEX_URL > built-in default, and
// repo-level settings are deliberately not a source: .entire/settings.json is
// committed and resolved from the working directory, so honoring it let a
// cloned repository redirect the catalog — and an index-listed repo installs
// with no confirmation prompt.
func TestResolvePluginIndexURL_Precedence(t *testing.T) { //nolint:paralleltest // mutates env
	t.Setenv(pluginIndexEnvVar, "")
	if got := resolvePluginIndexURL("https://flag.example/idx"); got != "https://flag.example/idx" {
		t.Errorf("flag should win: %q", got)
	}
	if got := resolvePluginIndexURL(""); got != defaultPluginIndexURL {
		t.Errorf("bare call should fall back to the built-in default: %q", got)
	}
	t.Setenv(pluginIndexEnvVar, "https://env.example/idx")
	if got := resolvePluginIndexURL(""); got != "https://env.example/idx" {
		t.Errorf("env should win over the default: %q", got)
	}
	if got := resolvePluginIndexURL("https://flag.example/idx"); got != "https://flag.example/idx" {
		t.Errorf("flag should win over env: %q", got)
	}
}

// A committed .entire/settings.json must not be able to steer the catalog. The
// settings schema no longer has the key at all, so the strict loader rejects it
// outright rather than honoring it — this pins that the door stays shut.
func TestPluginIndexURL_NotConfigurableViaRepoSettings(t *testing.T) { //nolint:paralleltest // mutates env
	t.Setenv(pluginIndexEnvVar, "")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".entire"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".entire", "settings.json"),
		[]byte(`{"plugins":{"index_url":"https://evil.example/idx"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if got := resolvePluginIndexURL(""); got != defaultPluginIndexURL {
		t.Errorf("repo settings steered the index to %q", got)
	}
}

func TestClassifyInstallArg(t *testing.T) {
	t.Parallel()
	for arg, want := range map[string]installArgKind{
		"https://github.com/entireio/entire-run": installFromURL,
		"git@github.com:entireio/entire-run.git": installFromURL,
		"file:///tmp/repo":                       installFromURL,
		"./dist/entire-run":                      installFromPath,
		"dist/entire-run":                        installFromPath,
		"../entire-run":                          installFromPath,
		"run":                                    installFromIndex,
		"brain":                                  installFromIndex,
		// Bare names are index lookups even when a same-named file exists
		// in the CWD — classification is pure string logic, never stat,
		// so a stray local file can't shadow an index name. Local files
		// need an explicit ./ prefix.
		"entire-run": installFromIndex,
	} {
		if got := classifyInstallArg(arg); got != want {
			t.Errorf("classifyInstallArg(%q) = %d, want %d", arg, got, want)
		}
	}
}

func TestSyncPluginIndex_RecoversFromPartialClone(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	url, _ := newIndexRepo(t, `{"version":1,"plugins":[{"name":"run","repo_url":"https://x.example/entire-run"}]}`)
	// Simulate an interrupted first clone: cache dir exists, is non-empty,
	// but has no .git. git clone refuses non-empty targets, so sync must
	// sweep the partial dir instead of staying wedged.
	dir, err := pluginIndexCacheDir(url)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "leftover"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	idx, err := SyncPluginIndex(context.Background(), url, false)
	if err != nil {
		t.Fatalf("SyncPluginIndex after partial clone: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Errorf("plugins = %d, want 1", len(idx.Plugins))
	}
}

// The per-URL cache dir is shared by every concurrent `entire plugin`
// invocation, and syncing it is destructive (RemoveAll + clone, or
// fetch + reset --hard). Concurrent syncs of the same index must all return a
// usable catalog rather than racing on partial state — including the cold-start
// case, where every caller wants to create the same clone at once.
func TestSyncPluginIndex_ConcurrentSyncsAreSerialized(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	url, _ := newIndexRepo(t, `{"version":1,"plugins":[{"name":"run","repo_url":"https://x.example/entire-run"}]}`)

	const workers = 6
	errs := make(chan error, workers)
	var start sync.WaitGroup
	start.Add(1)
	for range workers {
		go func() {
			start.Wait() // maximize overlap on the cold clone
			idx, err := SyncPluginIndex(context.Background(), url, true)
			switch {
			case err != nil:
				errs <- err
			case idx.Find("run") == nil:
				errs <- errors.New("catalog loaded without the expected entry")
			default:
				errs <- nil
			}
		}()
	}
	start.Done()
	for range workers {
		if err := <-errs; err != nil {
			t.Errorf("concurrent SyncPluginIndex: %v", err)
		}
	}
}

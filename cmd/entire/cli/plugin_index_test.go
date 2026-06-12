package cli

import (
	"context"
	"os"
	"path/filepath"
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

func TestSyncPluginIndex_UnsupportedVersion(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	url, _ := newIndexRepo(t, `{"version":99,"plugins":[]}`)
	if _, err := SyncPluginIndex(context.Background(), url, false); err == nil {
		t.Error("SyncPluginIndex accepted unsupported index version")
	}
}

func TestResolvePluginIndexURL_Precedence(t *testing.T) { //nolint:paralleltest // mutates env
	ctx := context.Background()
	t.Setenv(pluginIndexEnvVar, "")
	if got := resolvePluginIndexURL(ctx, "https://flag.example/idx"); got != "https://flag.example/idx" {
		t.Errorf("flag should win: %q", got)
	}
	t.Setenv(pluginIndexEnvVar, "https://env.example/idx")
	if got := resolvePluginIndexURL(ctx, ""); got != "https://env.example/idx" {
		t.Errorf("env should win over settings/default: %q", got)
	}
	if got := resolvePluginIndexURL(ctx, "https://flag.example/idx"); got != "https://flag.example/idx" {
		t.Errorf("flag should win over env: %q", got)
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

package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Tags used across the remote-install lifecycle tests.
const (
	remoteTestTagOld = "v0.1.0"
	remoteTestTagMid = "v0.2.0"
)

// pluginReleaseServer serves goreleaser-shaped release assets for the "demo"
// plugin at the given versions, alongside the per-tag checksums.txt goreleaser
// publishes. Assets are tar.gz archives whose entire-<name> entry content is
// "payload-<version>", letting tests assert which version got installed.
//
// Serving checksums matters: installs require an authenticated download by
// default, so a server without them would exercise only the refusal path.
func pluginReleaseServer(t *testing.T, versions ...string) *httptest.Server {
	t.Helper()
	binName := pluginBinaryPrefix + "demo"
	assets := map[string][]byte{}
	// checksums.txt is per-tag, and the URL template routes by tag, so index
	// the manifests by the version segment of the request path.
	sumsByVersion := map[string][]byte{}
	for _, v := range versions {
		archive := makeTarGz(t, map[string][]byte{binName: []byte("payload-" + v)})
		asset := fmt.Sprintf("%s_%s_%s_%s.tar.gz", binName, v, runtime.GOOS, runtime.GOARCH)
		assets[asset] = archive
		sumsByVersion[v] = []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), asset))
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if base == checksumsFileName {
			for v, sums := range sumsByVersion {
				if strings.Contains(r.URL.Path, "/v"+v+"/") || strings.Contains(r.URL.Path, "/"+v+"/") {
					_, _ = w.Write(sums) //nolint:errcheck // test server write; failure surfaces as a client error
					return
				}
			}
			http.NotFound(w, r)
			return
		}
		if data, ok := assets[base]; ok {
			_, _ = w.Write(data) //nolint:errcheck // test server write; failure surfaces as a client error
			return
		}
		http.NotFound(w, r)
	}))
}

func gitTag(t *testing.T, repoURL, tag string) {
	t.Helper()
	dir := strings.TrimPrefix(repoURL, "file://")
	if out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "tag", tag).CombinedOutput(); err != nil {
		t.Fatalf("git tag: %v: %s", err, out)
	}
}

// installedPayload reads back the payload of the "demo" fixture plugin, which
// is the only plugin these lifecycle tests install.
func installedPayload(t *testing.T) string {
	t.Helper()
	const name = "demo"
	p, err := FindInstalledPlugin(name)
	if err != nil || p == nil {
		t.Fatalf("FindInstalledPlugin(%s) = %v, %v", name, p, err)
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	return string(data)
}

func TestInstallPluginFromRepo_EndToEnd(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	ctx := context.Background()

	srv := pluginReleaseServer(t, "0.1.0", "0.2.0")
	defer srv.Close()
	meta := fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
	repoURL := newTaggedPluginRepo(t, meta, remoteTestTagOld, remoteTestTagMid)

	res, err := InstallPluginFromRepo(ctx, RemoteInstallOptions{RepoURL: repoURL})
	if err != nil {
		t.Fatalf("InstallPluginFromRepo: %v", err)
	}
	if res.Manifest.Tag != remoteTestTagMid {
		t.Errorf("installed tag = %s, want newest v0.2.0", res.Manifest.Tag)
	}
	if res.Manifest.SHA256 == "" || res.Manifest.Asset == "" {
		t.Errorf("manifest missing provenance: %+v", res.Manifest)
	}
	if got := installedPayload(t); got != "payload-0.2.0" {
		t.Errorf("installed payload = %q, want payload-0.2.0", got)
	}

	// Second install without --force refuses.
	if _, err := InstallPluginFromRepo(ctx, RemoteInstallOptions{RepoURL: repoURL}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("reinstall without force = %v, want already-installed error", err)
	}

	// Upgrade with no new tag reports up-to-date.
	o, err := UpgradeInstalledPlugin(ctx, "demo")
	if err != nil || !o.UpToDate {
		t.Errorf("upgrade with no new tag = %+v, %v; want UpToDate", o, err)
	}

	// New tag + new asset → upgrade replaces the binary.
	srv2 := pluginReleaseServer(t, "0.1.0", "0.2.0", "0.3.0")
	defer srv2.Close()
	updateRepoMetadata(t, repoURL, fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv2.URL))
	gitTag(t, repoURL, "v0.3.0")
	o, err = UpgradeInstalledPlugin(ctx, "demo")
	if err != nil {
		t.Fatalf("UpgradeInstalledPlugin: %v", err)
	}
	if o.FromTag != remoteTestTagMid || o.ToTag != "v0.3.0" {
		t.Errorf("upgrade outcome = %+v, want v0.2.0 → v0.3.0", o)
	}
	if got := installedPayload(t); got != "payload-0.3.0" {
		t.Errorf("post-upgrade payload = %q, want payload-0.3.0", got)
	}
}

// updateRepoMetadata commits a new entire-plugin.yml to the test repo.
func updateRepoMetadata(t *testing.T, repoURL, metadata string) {
	t.Helper()
	dir := strings.TrimPrefix(repoURL, "file://")
	if err := os.WriteFile(dir+"/"+pluginMetadataFileName, []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", dir, "add", pluginMetadataFileName},
		{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--no-gpg-sign", "-m", "update metadata"},
	} {
		if out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestInstallPluginFromRepo_PinnedSkipsUpgrade(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	ctx := context.Background()

	srv := pluginReleaseServer(t, "0.1.0", "0.2.0")
	defer srv.Close()
	meta := fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
	repoURL := newTaggedPluginRepo(t, meta, remoteTestTagOld, remoteTestTagMid)

	res, err := InstallPluginFromRepo(ctx, RemoteInstallOptions{RepoURL: repoURL, Pin: remoteTestTagOld})
	if err != nil {
		t.Fatalf("pinned install: %v", err)
	}
	if res.Manifest.Tag != remoteTestTagOld || !res.Manifest.Pinned {
		t.Errorf("manifest = %+v, want pinned v0.1.0", res.Manifest)
	}
	if got := installedPayload(t); got != "payload-0.1.0" {
		t.Errorf("payload = %q, want payload-0.1.0", got)
	}
	o, err := UpgradeInstalledPlugin(ctx, "demo")
	if err != nil || !o.Pinned {
		t.Errorf("upgrade of pinned = %+v, %v; want Pinned skip", o, err)
	}
}

func TestInstallPluginFromRepo_FallsBackPastAssetlessTag(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	ctx := context.Background()

	// Assets exist for 0.1.0 only; v0.2.0 is a pushed tag with no
	// published release. Install must fall back with the skipped tag
	// reported.
	srv := pluginReleaseServer(t, "0.1.0")
	defer srv.Close()
	meta := fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
	repoURL := newTaggedPluginRepo(t, meta, remoteTestTagOld, remoteTestTagMid)

	res, err := InstallPluginFromRepo(ctx, RemoteInstallOptions{RepoURL: repoURL})
	if err != nil {
		t.Fatalf("InstallPluginFromRepo: %v", err)
	}
	if res.Manifest.Tag != remoteTestTagOld {
		t.Errorf("tag = %s, want fallback to v0.1.0", res.Manifest.Tag)
	}
	if len(res.SkippedTags) != 1 || res.SkippedTags[0] != remoteTestTagMid {
		t.Errorf("SkippedTags = %v, want [v0.2.0]", res.SkippedTags)
	}
}

func TestUpgradeInstalledPlugin_NoManifest(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	if _, err := UpgradeInstalledPlugin(context.Background(), "localdev"); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("err = %v, want no-manifest explanation", err)
	}
}

func TestRemoveManagedPlugin_CleansBinAndPkg(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	ctx := context.Background()

	srv := pluginReleaseServer(t, "0.1.0")
	defer srv.Close()
	meta := fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
	repoURL := newTaggedPluginRepo(t, meta, remoteTestTagOld)
	if _, err := InstallPluginFromRepo(ctx, RemoteInstallOptions{RepoURL: repoURL}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := RemoveManagedPlugin("demo"); err != nil {
		t.Fatalf("RemoveManagedPlugin: %v", err)
	}
	if p, err := FindInstalledPlugin("demo"); err != nil || p != nil {
		t.Error("bin entry survived removal")
	}
	if m, err := LoadPluginManifest("demo"); err != nil || m != nil {
		t.Error("manifest survived removal")
	}
}

func TestUpgradeInstalledPlugin_EquivalentTagSpellingIsUpToDate(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	// Manifest recorded the bare spelling; the remote tag carries the v
	// prefix. Equivalent semver must not trigger a reinstall — the repo
	// has no release server at all, so any download attempt would fail.
	repoURL := newTaggedPluginRepo(t, "", "v0.2.0")
	if err := SavePluginManifest(&PluginManifest{Name: "demo", RepoURL: repoURL, Tag: "0.2.0"}); err != nil {
		t.Fatal(err)
	}
	o, err := UpgradeInstalledPlugin(context.Background(), "demo")
	if err != nil {
		t.Fatalf("UpgradeInstalledPlugin: %v", err)
	}
	if !o.UpToDate {
		t.Errorf("outcome = %+v, want UpToDate for equivalent tag spellings", o)
	}
}

func TestUpgradeInstalledPlugin_AssetlessNewestTagReportsUpToDate(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	ctx := context.Background()
	// Assets exist only for 0.1.0; v0.2.0 is tagged but unpublished.
	// Upgrade falls back to the installed version and must report
	// up-to-date, not a misleading "v0.1.0 → v0.1.0" upgrade line.
	srv := pluginReleaseServer(t, "0.1.0")
	defer srv.Close()
	meta := fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
	repoURL := newTaggedPluginRepo(t, meta, remoteTestTagOld)
	if _, err := InstallPluginFromRepo(ctx, RemoteInstallOptions{RepoURL: repoURL}); err != nil {
		t.Fatalf("install: %v", err)
	}
	gitTag(t, repoURL, remoteTestTagMid) // newer tag, no assets
	o, err := UpgradeInstalledPlugin(ctx, "demo")
	if err != nil {
		t.Fatalf("UpgradeInstalledPlugin: %v", err)
	}
	if !o.UpToDate {
		t.Errorf("outcome = %+v, want UpToDate when fallback lands on the installed tag", o)
	}
}

// The installed name comes from the remote (entire-plugin.yml's name:, else the
// repo basename). When the caller already committed to a name — an index entry
// the user typed, a requirement being satisfied, a plugin being upgraded — the
// remote must not substitute a different one. Unchecked this let an index entry
// named "safe" install entire-hijack with no prompt, and let --force on plugin A
// replace an unrelated installed plugin B.
func TestInstallPluginFromRepo_RejectsNameMismatch(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	srv := pluginReleaseServer(t, "0.1.0")
	defer srv.Close()
	meta := fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
	repoURL := newTaggedPluginRepo(t, meta, remoteTestTagOld)

	_, err := InstallPluginFromRepo(context.Background(), RemoteInstallOptions{
		RepoURL: repoURL, ExpectedName: "safe",
	})
	if err == nil {
		t.Fatal("install proceeded under a name the caller did not request")
	}
	for _, want := range []string{`"demo"`, `"safe"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %s", err, want)
		}
	}
	// Nothing may land on PATH under either name.
	for _, name := range []string{"demo", "safe"} {
		p, findErr := FindInstalledPlugin(name)
		if findErr != nil {
			t.Fatalf("FindInstalledPlugin(%s): %v", name, findErr)
		}
		if p != nil {
			t.Errorf("plugin %q was installed despite the mismatch", name)
		}
	}

	// The same repo installs fine when the expectation matches, and when the
	// caller has no expectation at all (a bare `install <url>`, where the
	// repository legitimately names itself).
	if _, err := InstallPluginFromRepo(context.Background(), RemoteInstallOptions{
		RepoURL: repoURL, ExpectedName: "demo",
	}); err != nil {
		t.Fatalf("matching expectation should install: %v", err)
	}
	if _, err := InstallPluginFromRepo(context.Background(), RemoteInstallOptions{
		RepoURL: repoURL, Force: true,
	}); err != nil {
		t.Errorf("no expectation should install: %v", err)
	}
}

// A dependency installed under a different name would never satisfy the
// requirement, so dependencySatisfied and doctor would report it missing
// forever and every future parent install would re-attempt it.
func TestExecuteDepPlan_RejectsNameMismatch(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	srv := pluginReleaseServer(t, "0.1.0")
	defer srv.Close()
	meta := fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
	repoURL := newTaggedPluginRepo(t, meta, remoteTestTagOld)

	// The plan says "sem"; the repo declares "demo".
	plan := &DepPlan{Actions: []DepAction{{Name: "sem", RepoURL: repoURL}}}
	err := ExecuteDepPlan(context.Background(), plan, false)
	if err == nil {
		t.Fatal("ExecuteDepPlan installed a dependency under the wrong name")
	}
	if !strings.Contains(err.Error(), "sem") {
		t.Errorf("err = %v, want it to name the unsatisfied requirement", err)
	}
	p, findErr := FindInstalledPlugin("demo")
	if findErr != nil {
		t.Fatalf("FindInstalledPlugin: %v", findErr)
	}
	if p != nil {
		t.Error("the mis-named dependency was installed anyway")
	}
}

// Upgrade knows the name of the plugin it is upgrading, so a repo that renamed
// itself must fail loudly rather than quietly installing a second plugin and
// leaving the original behind.
func TestUpgradeInstalledPlugin_RejectsRename(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	srv := pluginReleaseServer(t, "0.1.0", "0.2.0")
	defer srv.Close()
	meta := fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
	repoURL := newTaggedPluginRepo(t, meta, remoteTestTagOld)
	if _, err := InstallPluginFromRepo(context.Background(), RemoteInstallOptions{RepoURL: repoURL}); err != nil {
		t.Fatalf("initial install: %v", err)
	}

	// The upstream renames itself and tags a newer release.
	dir := strings.TrimPrefix(repoURL, "file://")
	testutil.WriteFile(t, dir, pluginMetadataFileName,
		fmt.Sprintf("name: renamed\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL))
	testutil.GitAdd(t, dir, pluginMetadataFileName)
	testutil.GitCommit(t, dir, "rename")
	gitTag(t, repoURL, remoteTestTagMid)

	if _, err := UpgradeInstalledPlugin(context.Background(), "demo"); err == nil {
		t.Error("upgrade accepted a renamed plugin")
	}
	p, findErr := FindInstalledPlugin("renamed")
	if findErr != nil {
		t.Fatalf("FindInstalledPlugin: %v", findErr)
	}
	if p != nil {
		t.Error("upgrade installed the plugin under its new name")
	}
	if installedPayload(t) != "payload-0.1.0" {
		t.Error("the original install was disturbed by the failed upgrade")
	}
}

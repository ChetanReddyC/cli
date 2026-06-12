package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// Tags used across the remote-install lifecycle tests.
const (
	remoteTestTagOld = "v0.1.0"
	remoteTestTagMid = "v0.2.0"
)

// pluginReleaseServer serves goreleaser-shaped release assets for the "demo"
// plugin at the given versions. Assets are tar.gz archives whose entire-<name> entry content is
// "payload-<version>", letting tests assert which version got installed.
func pluginReleaseServer(t *testing.T, versions ...string) *httptest.Server {
	t.Helper()
	binName := pluginBinaryPrefix + "demo"
	assets := map[string][]byte{}
	for _, v := range versions {
		archive := makeTarGz(t, map[string][]byte{binName: []byte("payload-" + v)})
		assets[fmt.Sprintf("%s_%s_%s_%s.tar.gz", binName, v, runtime.GOOS, runtime.GOARCH)] = archive
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
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

func installedPayload(t *testing.T, name string) string {
	t.Helper()
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
	if got := installedPayload(t, "demo"); got != "payload-0.2.0" {
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
	if got := installedPayload(t, "demo"); got != "payload-0.3.0" {
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
	if got := installedPayload(t, "demo"); got != "payload-0.1.0" {
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

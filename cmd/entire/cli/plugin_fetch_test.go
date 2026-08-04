package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseAssetBaseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		repo, tag, want string
		wantErr         bool
	}{
		{repo: "https://github.com/entireio/entire-run", tag: "v1.0.0", want: "https://github.com/entireio/entire-run/releases/download/v1.0.0/"},
		{repo: "https://github.com/entireio/entire-run.git", tag: "v1.0.0", want: "https://github.com/entireio/entire-run/releases/download/v1.0.0/"},
		{repo: "https://gitlab.com/group/entire-foo", tag: "v2.1.0", want: "https://gitlab.com/group/entire-foo/-/releases/v2.1.0/downloads/"},
		{repo: "https://gitlab.example.com/group/entire-foo", tag: "v2.1.0", want: "https://gitlab.example.com/group/entire-foo/-/releases/v2.1.0/downloads/"},
		{repo: "https://codeberg.org/me/entire-bar", tag: "v0.1.0", want: "https://codeberg.org/me/entire-bar/releases/download/v0.1.0/"},
		// Unknown hosts default to the GitHub-style convention.
		{repo: "https://git.example.com/me/entire-bar", tag: "v0.1.0", want: "https://git.example.com/me/entire-bar/releases/download/v0.1.0/"},
		// Non-HTTP remotes can't derive a download URL.
		{repo: "git@github.com:entireio/entire-run.git", tag: "v1.0.0", wantErr: true},
		{repo: "ssh://git@example.com/entire-run", tag: "v1.0.0", wantErr: true},
	}
	for _, tt := range tests {
		got, err := releaseAssetBaseURL(tt.repo, tt.tag)
		if tt.wantErr {
			if err == nil {
				t.Errorf("releaseAssetBaseURL(%q) = %q, want error", tt.repo, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("releaseAssetBaseURL(%q): %v", tt.repo, err)
			continue
		}
		if got != tt.want {
			t.Errorf("releaseAssetBaseURL(%q) = %q, want %q", tt.repo, got, tt.want)
		}
	}
}

func TestExpandDownloadTemplate(t *testing.T) {
	t.Parallel()
	got := expandDownloadTemplate("https://dl.example.com/{name}/{tag}/{version}/{os}_{arch}/{asset}", "run", "v1.2.3", "a.tar.gz")
	want := fmt.Sprintf("https://dl.example.com/run/v1.2.3/1.2.3/%s_%s/a.tar.gz", runtime.GOOS, runtime.GOARCH)
	if got != want {
		t.Errorf("expandDownloadTemplate = %q, want %q", got, want)
	}
}

// A fully-specified download_url may carry a query string (signed URLs,
// artifact proxies). path.Base on the whole URL folds it into the filename,
// which then misses the archive-extension sniff in extractPluginBinary and
// gets written out as a raw binary — a silently broken install.
func TestAssetNameFromURL(t *testing.T) {
	t.Parallel()
	tests := []struct{ url, want string }{
		{url: "https://ex.com/rel/v1/entire-foo.tar.gz", want: "entire-foo.tar.gz"},
		{url: "https://ex.com/rel/entire-foo.tar.gz?token=abc", want: "entire-foo.tar.gz"},
		{url: "https://ex.com/rel/entire-foo.zip#frag", want: "entire-foo.zip"},
		{url: "https://ex.com/rel/entire-foo", want: "entire-foo"},
	}
	for _, tt := range tests {
		if got := assetNameFromURL(tt.url); got != tt.want {
			t.Errorf("assetNameFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestAssetCandidates_CoverConventions(t *testing.T) {
	t.Parallel()
	cands := assetCandidates("run", "v1.2.3")
	mustContain := []string{
		fmt.Sprintf("entire-run_1.2.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH),
		fmt.Sprintf("entire-run_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH),
		fmt.Sprintf("entire-run_1.2.3_%s_%s.zip", runtime.GOOS, runtime.GOARCH),
	}
	for _, want := range mustContain {
		found := false
		for _, c := range cands {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("assetCandidates missing %q in %v", want, cands)
		}
	}
	// Arch aliases: amd64 hosts must also try x86_64 spellings.
	if runtime.GOARCH == "amd64" {
		found := false
		for _, c := range cands {
			if strings.Contains(c, "x86_64") {
				found = true
				break
			}
		}
		if !found {
			t.Error("assetCandidates lacks x86_64 alias on amd64")
		}
	}
}

func TestParseChecksums_AndSelect(t *testing.T) {
	t.Parallel()
	osName, arch := runtime.GOOS, runtime.GOARCH
	manifest := fmt.Sprintf(`
abc123  entire-run_1.0.0_%s_%s.tar.gz
def456  *entire-run_1.0.0_other_other.zip

malformed line without two fields maybe three
`, osName, arch)
	sums := parseChecksums([]byte(manifest))
	if len(sums) != 2 {
		t.Fatalf("parseChecksums = %d entries, want 2: %v", len(sums), sums)
	}
	asset, digest, ok := selectAssetFromChecksums(sums, "run", "v1.0.0")
	if !ok {
		t.Fatal("selectAssetFromChecksums found nothing")
	}
	if digest != "abc123" || !strings.Contains(asset, osName) {
		t.Errorf("selected %q/%q, want platform asset with digest abc123", asset, digest)
	}
	// A manifest with no matching platform asset must report not-ok.
	if _, _, ok := selectAssetFromChecksums(map[string]string{"entire-run_1.0.0_plan9_mips.tar.gz": "x"}, "run", "v1.0.0"); ok {
		t.Error("selectAssetFromChecksums matched a foreign platform")
	}
}

// makeTarGz builds an in-memory tar.gz with the given entries.
func makeTarGz(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractPluginBinary_TarGz(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	payload := []byte("#!/bin/sh\necho run\n")
	if err := os.WriteFile(archive, makeTarGz(t, map[string][]byte{
		"README.md":               []byte("docs"),
		"subdir/entire-run":       payload,
		"../escape-attempt":       []byte("nope"),
		"unrelated/entire-runner": []byte("close but no"),
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")
	if err := extractPluginBinary(archive, "run", dest); err != nil {
		t.Fatalf("extractPluginBinary: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("extracted content mismatch")
	}
	if runtime.GOOS != windowsGOOS {
		info, err := os.Stat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Error("extracted binary is not executable")
		}
	}
}

func TestExtractPluginBinary_Zip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.zip")
	payload := []byte("zip payload")
	if err := os.WriteFile(archive, makeZip(t, map[string][]byte{"entire-run": payload}), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")
	if err := extractPluginBinary(archive, "run", dest); err != nil {
		t.Fatalf("extractPluginBinary: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("extracted content mismatch")
	}
}

func TestExtractPluginBinary_MissingEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archive, makeTarGz(t, map[string][]byte{"other": []byte("x")}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractPluginBinary(archive, "run", filepath.Join(dir, "out")); err == nil {
		t.Error("extractPluginBinary succeeded on archive without the binary")
	}
}

func TestExtractPluginBinary_RawBinary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	raw := filepath.Join(dir, "entire-run_1.0.0_x_y")
	payload := []byte("raw binary bytes")
	if err := os.WriteFile(raw, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")
	if err := extractPluginBinary(raw, "run", dest); err != nil {
		t.Fatalf("extractPluginBinary: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("raw copy mismatch")
	}
}

func TestSafeArchiveEntry(t *testing.T) {
	t.Parallel()
	for entry, want := range map[string]bool{
		"entire-run":        true,
		"dist/entire-run":   true,
		"../evil":           false,
		"a/../../evil":      false,
		"/abs/path":         false,
		"with\x00null":      false,
		"./fine/entire-run": true,
	} {
		if got := safeArchiveEntry(entry); got != want {
			t.Errorf("safeArchiveEntry(%q) = %t, want %t", entry, got, want)
		}
	}
}

func TestFetchAndVerify_ChecksumEnforced(t *testing.T) {
	t.Parallel()
	payload := []byte("plugin bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload) //nolint:errcheck // test server write; failure surfaces as a client error
	}))
	defer srv.Close()

	dir := t.TempDir()
	sum := sha256.Sum256(payload)
	good := hex.EncodeToString(sum[:])

	fa, err := fetchAndVerify(context.Background(), srv.URL+"/asset", "asset", good, dir)
	if err != nil {
		t.Fatalf("fetchAndVerify with good digest: %v", err)
	}
	if fa.SHA256 != good {
		t.Errorf("SHA256 = %s, want %s", fa.SHA256, good)
	}

	if _, err := fetchAndVerify(context.Background(), srv.URL+"/asset", "asset2", strings.Repeat("0", 64), dir); err == nil {
		t.Error("fetchAndVerify accepted a wrong digest")
	}
}

func TestFetchAndVerify_404IsAssetNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := fetchAndVerify(context.Background(), srv.URL+"/missing", "missing", "", t.TempDir())
	if !errors.Is(err, errAssetNotFound) {
		t.Errorf("404 error = %v, want errAssetNotFound", err)
	}
}

func TestDownloadPluginAsset_ViaChecksumManifest(t *testing.T) {
	t.Parallel()
	payload := makeTarGz(t, map[string][]byte{"entire-run": []byte("bin")})
	asset := fmt.Sprintf("entire-run_1.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(payload)
	checksums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			_, _ = w.Write([]byte(checksums)) //nolint:errcheck // test server write; failure surfaces as a client error
		case strings.HasSuffix(r.URL.Path, asset):
			_, _ = w.Write(payload) //nolint:errcheck // test server write; failure surfaces as a client error
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	meta := &PluginMetadata{DownloadURL: srv.URL + "/dl/{tag}/{asset}"}
	fa, err := downloadPluginAsset(context.Background(), meta, "https://example.invalid/entire-run", "run", "v1.0.0", t.TempDir())
	if err != nil {
		t.Fatalf("downloadPluginAsset: %v", err)
	}
	if fa.Asset != asset {
		t.Errorf("Asset = %q, want %q", fa.Asset, asset)
	}
}

func TestDownloadPluginAsset_ProbeFallbackWithoutChecksums(t *testing.T) {
	t.Parallel()
	payload := makeTarGz(t, map[string][]byte{"entire-run": []byte("bin")})
	asset := fmt.Sprintf("entire-run_1.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, asset) {
			_, _ = w.Write(payload) //nolint:errcheck // test server write; failure surfaces as a client error
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	meta := &PluginMetadata{DownloadURL: srv.URL + "/dl/{asset}"}
	fa, err := downloadPluginAsset(context.Background(), meta, "https://example.invalid/entire-run", "run", "v1.0.0", t.TempDir())
	if err != nil {
		t.Fatalf("downloadPluginAsset: %v", err)
	}
	if fa.Asset != asset {
		t.Errorf("Asset = %q, want %q", fa.Asset, asset)
	}
}

func TestDownloadPluginAsset_NoAssetForPlatform(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	meta := &PluginMetadata{DownloadURL: srv.URL + "/dl/{asset}"}
	_, err := downloadPluginAsset(context.Background(), meta, "https://example.invalid/entire-run", "run", "v1.0.0", t.TempDir())
	if !errors.Is(err, errAssetNotFound) {
		t.Errorf("err = %v, want errAssetNotFound", err)
	}
}

func TestFetchAndVerify_RejectsUnsafeAssetNames(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x")) //nolint:errcheck // test server write; failure surfaces as a client error
	}))
	defer srv.Close()
	for _, asset := range []string{"", ".", "..", "../escape", "a/b", `a\b`} {
		if _, err := fetchAndVerify(context.Background(), srv.URL, asset, "", t.TempDir()); err == nil {
			t.Errorf("fetchAndVerify accepted unsafe asset name %q", asset)
		}
	}
}

func TestFetchAndVerify_RemovesPartialFileOnMismatch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload")) //nolint:errcheck // test server write; failure surfaces as a client error
	}))
	defer srv.Close()
	dir := t.TempDir()
	if _, err := fetchAndVerify(context.Background(), srv.URL+"/a", "a", strings.Repeat("0", 64), dir); err == nil {
		t.Fatal("fetchAndVerify accepted a wrong digest")
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial download left behind after checksum mismatch: stat err = %v", err)
	}
}

func TestDownloadPluginAsset_StaleManifestFallsThroughToProbe(t *testing.T) {
	t.Parallel()
	// The root checksums.txt lists only a foreign platform, but the real
	// asset is published under its conventional name. A stale manifest
	// must not block the install: selection falls through to the probe.
	payload := makeTarGz(t, map[string][]byte{"entire-run": []byte("bin")})
	asset := fmt.Sprintf("entire-run_1.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			_, _ = w.Write([]byte("abc  entire-run_1.0.0_plan9_mips.tar.gz\n")) //nolint:errcheck // test server write
		case strings.HasSuffix(r.URL.Path, asset):
			_, _ = w.Write(payload) //nolint:errcheck // test server write
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	meta := &PluginMetadata{DownloadURL: srv.URL + "/dl/{asset}"}
	fa, err := downloadPluginAsset(context.Background(), meta, "https://example.invalid/entire-run", "run", "v1.0.0", t.TempDir())
	if err != nil {
		t.Fatalf("downloadPluginAsset with stale manifest: %v", err)
	}
	if fa.Asset != asset {
		t.Errorf("Asset = %q, want %q via probe fallback", fa.Asset, asset)
	}
}

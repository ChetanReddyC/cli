package cli

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Release-asset download. The one irreducibly forge-specific piece of the
// plugin system is where release binaries live; it's contained here as a
// small per-host URL convention table with a declarative escape hatch
// (download_url in entire-plugin.yml). Everything else — version listing,
// metadata, the index — runs on the git protocol.

// errAssetNotFound distinguishes "this tag has no matching asset" (worth
// trying the next-highest tag — a pushed tag whose release isn't published
// yet) from transport errors (not worth retrying on another tag).
var errAssetNotFound = errors.New("no matching release asset")

// pluginHTTPClient bounds the total time for a single asset download.
// Plugins can be tens of MB; 5 minutes is generous even on slow links.
var pluginHTTPClient = &http.Client{Timeout: 5 * time.Minute}

// maxPluginAssetSize caps a downloaded asset. Real plugin binaries are tens
// of MB; the cap exists so a misconfigured URL serving an endless stream
// can't fill the disk.
const maxPluginAssetSize = 512 << 20 // 512 MiB

// releaseAssetBaseURL returns the URL prefix release assets live under for
// the repo's host. GitHub and the Gitea family share a convention; GitLab
// has its own. Unknown hosts default to the GitHub-style convention (most
// self-hosted forges mirror it) — authors on hosts that don't can declare
// download_url in entire-plugin.yml.
func releaseAssetBaseURL(repoURL, tag string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(repoURL, ".git"))
	if err != nil {
		return "", fmt.Errorf("parse repo URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("release downloads need an http(s) repo URL, got %q (declare download_url in %s for non-HTTP remotes)", repoURL, pluginMetadataFileName)
	}
	base := strings.TrimSuffix(u.String(), "/")
	host := strings.ToLower(u.Hostname())
	if host == "gitlab.com" || strings.HasPrefix(host, "gitlab.") {
		return base + "/-/releases/" + url.PathEscape(tag) + "/downloads/", nil
	}
	return base + "/releases/download/" + url.PathEscape(tag) + "/", nil
}

// expandDownloadTemplate expands the author-declared download_url template.
// Placeholders: {name} {tag} {version} {os} {arch} {asset}.
func expandDownloadTemplate(tmpl, name, tag, asset string) string {
	r := strings.NewReplacer(
		"{name}", name,
		"{tag}", tag,
		"{version}", strings.TrimPrefix(tag, "v"),
		"{os}", runtime.GOOS,
		"{arch}", runtime.GOARCH,
		"{asset}", asset,
	)
	return r.Replace(tmpl)
}

// archAliases maps a GOARCH to the spellings seen in release asset names.
func archAliases(goarch string) []string {
	switch goarch {
	case "amd64":
		return []string{"amd64", "x86_64"}
	case "arm64":
		return []string{"arm64", "aarch64"}
	default:
		return []string{goarch}
	}
}

// osAliases maps a GOOS to asset-name spellings, most specific first.
// "all" covers macOS universal binaries.
func osAliases(goos string) []string {
	if goos == darwinGOOS {
		return []string{"darwin", "macos", "all"}
	}
	return []string{goos}
}

// assetCandidates returns release asset filenames to try for this platform,
// in preference order: archives before raw binaries, version-in-name
// (goreleaser's default template) before version-less.
func assetCandidates(name, tag string) []string {
	binName := pluginBinaryPrefix + name
	version := strings.TrimPrefix(tag, "v")
	stems := make([]string, 0, 16)
	for _, osName := range osAliases(runtime.GOOS) {
		for _, arch := range archAliases(runtime.GOARCH) {
			stems = append(stems,
				fmt.Sprintf("%s_%s_%s_%s", binName, version, osName, arch),
				fmt.Sprintf("%s_%s_%s", binName, osName, arch),
			)
		}
	}
	exts := []string{".tar.gz", ".zip", ""}
	if runtime.GOOS == windowsGOOS {
		exts = []string{".zip", ".tar.gz", ".exe"}
	}
	var out []string
	for _, ext := range exts {
		for _, stem := range stems {
			out = append(out, stem+ext)
		}
	}
	return out
}

// checksumCandidates returns filenames the checksum manifest may go by.
func checksumCandidates(name, tag string) []string {
	binName := pluginBinaryPrefix + name
	version := strings.TrimPrefix(tag, "v")
	return []string{
		"checksums.txt",
		fmt.Sprintf("%s_%s_checksums.txt", binName, version),
		binName + "_checksums.txt",
	}
}

// parseChecksums parses "hex  filename" lines (sha256sum format) into a
// filename → digest map.
func parseChecksums(data []byte) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// sha256sum marks binary-mode files with a leading *.
		out[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	return out
}

// selectAssetFromChecksums picks the preferred candidate present in the
// checksum manifest. The manifest lists what was actually published, so
// this avoids probing candidate URLs one by one.
func selectAssetFromChecksums(sums map[string]string, name, tag string) (asset, digest string, ok bool) {
	for _, c := range assetCandidates(name, tag) {
		if d, present := sums[c]; present {
			return c, d, true
		}
	}
	return "", "", false
}

// fetchedAsset is the result of downloadPluginAsset: a verified asset on
// disk in a caller-owned staging dir.
type fetchedAsset struct {
	// Path is the downloaded asset file inside the staging dir.
	Path string
	// Asset is the asset filename that matched.
	Asset string
	// SHA256 is the hex digest of the downloaded bytes.
	SHA256 string
}

// downloadPluginAsset locates and downloads the release asset for name@tag
// into stagingDir, verifying against the release's checksum manifest when
// one is published. Returns errAssetNotFound (possibly wrapped) when the
// tag has no asset for this platform.
func downloadPluginAsset(ctx context.Context, meta *PluginMetadata, repoURL, name, tag, stagingDir string) (*fetchedAsset, error) {
	assetURL := func(asset string) (string, error) {
		if meta != nil && meta.DownloadURL != "" {
			return expandDownloadTemplate(meta.DownloadURL, name, tag, asset), nil
		}
		base, err := releaseAssetBaseURL(repoURL, tag)
		if err != nil {
			return "", err
		}
		return base + asset, nil
	}

	// A download_url template without {asset} is a single fully-specified
	// URL; there is nothing to select.
	if meta != nil && meta.DownloadURL != "" && !strings.Contains(meta.DownloadURL, "{asset}") {
		u := expandDownloadTemplate(meta.DownloadURL, name, tag, "")
		return fetchAndVerify(ctx, u, path.Base(u), "", stagingDir)
	}

	// Preferred path: fetch the checksum manifest and pick from what was
	// actually published.
	for _, cs := range checksumCandidates(name, tag) {
		u, err := assetURL(cs)
		if err != nil {
			return nil, err
		}
		data, err := httpGetSmall(ctx, u)
		if err != nil {
			continue
		}
		asset, digest, ok := selectAssetFromChecksums(parseChecksums(data), name, tag)
		if !ok {
			return nil, fmt.Errorf("%w: %s lists no asset for %s/%s", errAssetNotFound, cs, runtime.GOOS, runtime.GOARCH)
		}
		au, err := assetURL(asset)
		if err != nil {
			return nil, err
		}
		return fetchAndVerify(ctx, au, asset, digest, stagingDir)
	}

	// Fallback: probe candidates directly (release without a checksum
	// manifest). No digest to verify against; the manifest records the
	// digest we computed so upgrades can at least detect drift.
	for _, asset := range assetCandidates(name, tag) {
		u, err := assetURL(asset)
		if err != nil {
			return nil, err
		}
		fa, err := fetchAndVerify(ctx, u, asset, "", stagingDir)
		if err == nil {
			return fa, nil
		}
		if !errors.Is(err, errAssetNotFound) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w for %s/%s at %s %s", errAssetNotFound, runtime.GOOS, runtime.GOARCH, repoURL, tag)
}

// httpGetSmall fetches a small text resource (checksum manifests).
func httpGetSmall(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", rawURL, err)
	}
	resp, err := pluginHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rawURL, err)
	}
	return data, nil
}

// fetchAndVerify downloads rawURL into stagingDir, streaming the SHA-256 as
// it writes. A non-empty wantDigest is enforced; an empty one records the
// computed digest. 404/410 map to errAssetNotFound.
func fetchAndVerify(ctx context.Context, rawURL, asset, wantDigest, stagingDir string) (*fetchedAsset, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", rawURL, err)
	}
	resp, err := pluginHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		return nil, fmt.Errorf("%w: GET %s: %s", errAssetNotFound, rawURL, resp.Status)
	default:
		return nil, fmt.Errorf("download %s: %s", rawURL, resp.Status)
	}

	dest := filepath.Join(stagingDir, asset)
	out, err := os.Create(dest) //nolint:gosec // dest is inside the caller-owned staging dir; asset name came from our candidate list or checksum manifest
	if err != nil {
		return nil, fmt.Errorf("create staging file: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, maxPluginAssetSize+1))
	closeErr := out.Close()
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", rawURL, err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("write staging file: %w", closeErr)
	}
	if n > maxPluginAssetSize {
		return nil, fmt.Errorf("download %s: exceeds %d byte limit", rawURL, int64(maxPluginAssetSize))
	}
	got := hex.EncodeToString(h.Sum(nil))
	if wantDigest != "" && !strings.EqualFold(got, wantDigest) {
		return nil, fmt.Errorf("checksum mismatch for %s: got %s, want %s", asset, got, wantDigest)
	}
	return &fetchedAsset{Path: dest, Asset: asset, SHA256: got}, nil
}

// extractPluginBinary locates the plugin executable inside the downloaded
// asset and writes it to destPath (mode 0755 on Unix). Raw binaries are
// copied; .tar.gz/.tgz and .zip archives are searched for an entry whose
// basename matches entire-<name>[.exe], wherever it sits in the archive.
func extractPluginBinary(assetPath, name, destPath string) error {
	binName := pluginBinaryPrefix + name
	lower := strings.ToLower(assetPath)
	switch {
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return extractFromTarGz(assetPath, binName, destPath)
	case strings.HasSuffix(lower, ".zip"):
		return extractFromZip(assetPath, binName, destPath)
	default:
		src, err := os.Open(assetPath) //nolint:gosec // staging-dir file we just wrote
		if err != nil {
			return fmt.Errorf("open asset: %w", err)
		}
		defer src.Close()
		return writeExecutable(src, destPath)
	}
}

// matchesPluginBinary reports whether an archive entry basename is the
// plugin binary, tolerating a Windows extension.
func matchesPluginBinary(entryName, binName string) bool {
	base := path.Base(filepath.ToSlash(entryName))
	if base == binName {
		return true
	}
	return runtime.GOOS == windowsGOOS && strings.EqualFold(base, binName+".exe")
}

// safeArchiveEntry rejects entry names that could escape an extraction
// root. We only ever extract a single matched file to a fixed dest, so this
// is defense in depth against a hostile archive masking as a plugin.
func safeArchiveEntry(entryName string) bool {
	clean := path.Clean(filepath.ToSlash(entryName))
	return !strings.HasPrefix(clean, "../") && !path.IsAbs(clean) && !strings.Contains(entryName, "\x00")
}

func extractFromTarGz(archivePath, binName, destPath string) error {
	f, err := os.Open(archivePath) //nolint:gosec // staging-dir file we just wrote
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || !safeArchiveEntry(hdr.Name) || !matchesPluginBinary(hdr.Name, binName) {
			continue
		}
		return writeExecutable(io.LimitReader(tr, maxPluginAssetSize), destPath)
	}
	return fmt.Errorf("archive %s contains no %s entry", filepath.Base(archivePath), binName)
}

func extractFromZip(archivePath, binName, destPath string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !safeArchiveEntry(f.Name) || !matchesPluginBinary(f.Name, binName) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open zip entry: %w", err)
		}
		defer rc.Close()
		return writeExecutable(io.LimitReader(rc, maxPluginAssetSize), destPath)
	}
	return fmt.Errorf("archive %s contains no %s entry", filepath.Base(archivePath), binName)
}

// writeExecutable writes r to destPath with the executable bit set.
// Explicit chmod (not inherited archive mode) because zip-built and
// raw-binary releases routinely lose the bit — the #1 "downloaded plugin
// doesn't run" failure.
func writeExecutable(r io.Reader, destPath string) error {
	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // dest is inside the managed pkg tree; the binary must be executable
	if err != nil {
		return fmt.Errorf("create binary: %w", err)
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		_ = os.Remove(destPath)
		return fmt.Errorf("write binary: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(destPath)
		return fmt.Errorf("close binary: %w", err)
	}
	return nil
}

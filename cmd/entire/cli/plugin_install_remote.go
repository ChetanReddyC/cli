package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Remote install orchestration: resolve tag → fetch metadata → download +
// verify + extract → place under pkg/<name>/ → link into bin/ → write
// manifest. The bin/ link goes through the same InstallPluginFromPath used
// by local-dev installs, so conflict handling, atomic replace, and the
// Windows symlink fallbacks are shared, not duplicated.

// maxTagFallbacks bounds how many tags we walk down when the newest tag has
// no published assets (tag pushed, release not finished).
const maxTagFallbacks = 3

// RemoteInstallOptions configures InstallPluginFromRepo.
type RemoteInstallOptions struct {
	// RepoURL is the full git URL of the plugin repository.
	RepoURL string
	// Pin, when non-empty, installs exactly this tag and marks the
	// manifest pinned so upgrade skips it.
	Pin string
	// Force replaces an existing managed entry with the same name.
	Force bool
}

// RemoteInstallResult is what a successful remote install produced.
type RemoteInstallResult struct {
	Installed *InstalledPlugin
	Manifest  *PluginManifest
	Metadata  *PluginMetadata
	// SkippedTags lists newer tags that were passed over for missing
	// assets, newest first. Callers surface these as warnings.
	SkippedTags []string
}

// InstallPluginFromRepo installs a plugin from a git repository URL.
// Dependency resolution deliberately does not happen here — callers
// (the install command) plan and confirm dependency installs first.
func InstallPluginFromRepo(ctx context.Context, opts RemoteInstallOptions) (*RemoteInstallResult, error) {
	repoURL := strings.TrimRight(opts.RepoURL, "/")

	var tags []string
	if opts.Pin != "" {
		tags = []string{opts.Pin}
	} else {
		var err error
		tags, err = listRemoteSemverTags(ctx, repoURL)
		if err != nil {
			return nil, err
		}
		if len(tags) == 0 {
			return nil, fmt.Errorf("%s has no semver tags; pass --pin <tag> to install a non-semver tag", repoURL)
		}
		if len(tags) > maxTagFallbacks {
			tags = tags[:maxTagFallbacks]
		}
	}

	var lastErr error
	for i, tag := range tags {
		res, err := installRepoAtTag(ctx, repoURL, tag, opts)
		if err == nil {
			res.SkippedTags = tags[:i]
			return res, nil
		}
		lastErr = err
		if !errors.Is(err, errAssetNotFound) {
			return nil, err
		}
	}
	return nil, lastErr
}

func installRepoAtTag(ctx context.Context, repoURL, tag string, opts RemoteInstallOptions) (*RemoteInstallResult, error) {
	meta, err := fetchPluginMetadataAtTag(ctx, repoURL, tag)
	if err != nil {
		return nil, err
	}
	var name string
	if meta != nil && meta.Name != "" {
		name = meta.Name
	} else if name, err = pluginNameFromRepoURL(repoURL); err != nil {
		return nil, err
	}

	if existing, err := FindInstalledPlugin(name); err != nil {
		return nil, err
	} else if existing != nil && !opts.Force {
		return nil, fmt.Errorf("plugin %q already installed at %s; use --force to replace", name, existing.Path)
	}

	staging, err := os.MkdirTemp("", "entire-plugin-fetch-")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	asset, err := downloadPluginAsset(ctx, meta, repoURL, name, tag, staging)
	if err != nil {
		return nil, err
	}

	binBase := pluginBinaryPrefix + name
	if runtime.GOOS == windowsGOOS {
		binBase += ".exe"
	}
	stagedBin := filepath.Join(staging, "extracted-"+binBase)
	if err := extractPluginBinary(asset.Path, name, stagedBin); err != nil {
		return nil, err
	}

	pkgDir, err := EnsurePluginPkgDir(name)
	if err != nil {
		return nil, err
	}
	sweepOldBinaries(pkgDir)
	pkgBin := filepath.Join(pkgDir, binBase)
	if err := replaceBinary(stagedBin, pkgBin); err != nil {
		return nil, fmt.Errorf("place plugin binary: %w", err)
	}

	installed, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: pkgBin, Force: true})
	if err != nil {
		return nil, err
	}

	manifest := &PluginManifest{
		Name:        name,
		RepoURL:     repoURL,
		Tag:         tag,
		Asset:       asset.Asset,
		SHA256:      asset.SHA256,
		Pinned:      opts.Pin != "",
		InstalledAt: time.Now().UTC(),
	}
	if meta != nil {
		manifest.Requires = meta.Requires
	}
	if err := SavePluginManifest(manifest); err != nil {
		return nil, err
	}
	return &RemoteInstallResult{Installed: installed, Manifest: manifest, Metadata: meta}, nil
}

// UpgradeOutcome describes what UpgradeInstalledPlugin did.
type UpgradeOutcome struct {
	Name string
	// Pinned: skipped because the manifest is pinned.
	Pinned bool
	// UpToDate: already at the newest tag.
	UpToDate bool
	// FromTag/ToTag are set when an upgrade actually happened.
	FromTag, ToTag string
}

// UpgradeInstalledPlugin re-resolves the newest tag for a remote-installed
// plugin and reinstalls when it differs from the manifest's tag. Plugins
// without a manifest (local-dev symlinks) are not upgradable.
func UpgradeInstalledPlugin(ctx context.Context, name string) (*UpgradeOutcome, error) {
	m, err := LoadPluginManifest(name)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("plugin %q has no install manifest (local-dev install?); reinstall it from its repository URL to make it upgradable", name)
	}
	if m.Pinned {
		return &UpgradeOutcome{Name: name, Pinned: true}, nil
	}
	tags, err := listRemoteSemverTags(ctx, m.RepoURL)
	if err != nil {
		return nil, err
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("%s has no semver tags", m.RepoURL)
	}
	if tags[0] == m.Tag {
		return &UpgradeOutcome{Name: name, UpToDate: true}, nil
	}
	res, err := InstallPluginFromRepo(ctx, RemoteInstallOptions{RepoURL: m.RepoURL, Force: true})
	if err != nil {
		return nil, err
	}
	if res.Manifest.Tag == m.Tag {
		return &UpgradeOutcome{Name: name, UpToDate: true}, nil
	}
	return &UpgradeOutcome{Name: name, FromTag: m.Tag, ToTag: res.Manifest.Tag}, nil
}

// replaceBinary moves src over dest atomically. On Windows, a running
// executable can't be replaced (sharing violation) but it *can* be renamed:
// move the old binary aside to a .old-<rand> file and retry; leftovers are
// swept on the next install. os.Rename fails across filesystems (staging is
// in the system temp dir), so a copy fallback covers that case.
func replaceBinary(src, dest string) error {
	err := os.Rename(src, dest)
	if err == nil {
		return nil
	}
	if runtime.GOOS == windowsGOOS {
		if asideErr := os.Rename(dest, oldBinaryAsidePath(dest)); asideErr == nil {
			if err = os.Rename(src, dest); err == nil {
				return nil
			}
		}
	}
	// Cross-device rename: copy + fsync-free write, then remove src.
	in, openErr := os.Open(src) //nolint:gosec // staging file we created
	if openErr != nil {
		return errors.Join(err, openErr)
	}
	defer in.Close()
	if writeErr := writeExecutable(in, dest); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	_ = os.Remove(src)
	return nil
}

// oldBinaryAsidePath returns a unique .old- sibling path for the
// rename-aside trick. Random suffix so concurrent upgrades can't collide.
func oldBinaryAsidePath(dest string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return dest + ".old-fallback"
	}
	return dest + ".old-" + hex.EncodeToString(b[:])
}

// sweepOldBinaries best-effort removes .old-* leftovers from the Windows
// rename-aside fallback. Failures are ignored — the files are inert.
func sweepOldBinaries(pkgDir string) {
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".old-") {
			_ = os.Remove(filepath.Join(pkgDir, e.Name()))
		}
	}
}

// RemoveManagedPlugin removes a plugin's bin entries and its pkg dir.
// The dependency guard lives in the command layer so --force can bypass it.
func RemoveManagedPlugin(name string) error {
	if err := RemoveInstalledPlugin(name); err != nil {
		return err
	}
	return RemovePluginPkg(name)
}

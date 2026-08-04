package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Managed-install manifests. A plugin installed from a remote repository
// gets a per-plugin home under the managed dir:
//
//	<plugin-root>/pkg/<name>/entire-<name>[.exe]   the binary
//	<plugin-root>/pkg/<name>/manifest.yml          install provenance
//
// The bin/ dir remains the only dispatch surface — pkg/ binaries are
// linked into bin/ via the same symlink→hardlink→copy fallback as local
// installs, so the kubectl-style resolver in plugin.go never changes.
//
// Local-dev installs (`entire plugin install ./path`) bypass pkg/ entirely
// and have no manifest; ListPluginManifests simply doesn't see them.
const (
	pluginManagedPkgSubdir = "pkg"
	pluginManifestFileName = "manifest.yml"
)

// PluginManifest records how a managed plugin was installed. Settings
// configure behavior; manifests record facts. The dependency list is
// copied from the plugin's metadata at install time so reverse-dependency
// checks (remove guard, doctor) work offline.
type PluginManifest struct {
	// Name is the bare plugin name ("run" for entire-run).
	Name string `yaml:"name"`
	// RepoURL is the full git URL the plugin was installed from.
	RepoURL string `yaml:"repo_url"`
	// Tag is the git tag that was installed (e.g. "v0.2.1").
	Tag string `yaml:"tag"`
	// Asset is the release asset filename the binary came from. Empty for
	// raw-binary downloads where the asset name equals the binary name.
	Asset string `yaml:"asset,omitempty"`
	// SHA256 is the hex digest of the downloaded asset.
	SHA256 string `yaml:"sha256,omitempty"`
	// Pinned marks installs done with --pin; upgrade skips them.
	Pinned bool `yaml:"pinned,omitempty"`
	// InstalledAt is when the install (or last upgrade) completed.
	InstalledAt time.Time `yaml:"installed_at,omitempty"`
	// Requires is the dependency list from the plugin's entire-plugin.yml
	// at the installed tag.
	Requires []PluginRequirement `yaml:"requires,omitempty"`
}

// PluginRequirement declares a dependency on another plugin. Shared between
// the author-side metadata file (entire-plugin.yml) and the install-side
// manifest so the two can never drift.
type PluginRequirement struct {
	// Name is the bare plugin name of the dependency.
	Name string `yaml:"name"`
	// RepoURL is where to install the dependency from when it's missing.
	// Optional when the dependency is resolvable through the index.
	RepoURL string `yaml:"repo_url,omitempty"`
	// MinVersion is the minimum acceptable tag (e.g. "v0.2.0"). Minimum
	// only — there is deliberately no range syntax.
	MinVersion string `yaml:"min_version,omitempty"`
}

// PluginMetadata is the author-side declaration committed at the root of a
// plugin repository as entire-plugin.yml. Everything is optional — a repo
// without the file installs fine; the name then derives from the repo URL.
type PluginMetadata struct {
	// Name is the bare plugin name. When empty, derived from the repo URL
	// basename (entire-run → run).
	Name string `yaml:"name,omitempty"`
	// Description is a one-line summary shown by info/search.
	Description string `yaml:"description,omitempty"`
	// DownloadURL overrides the per-forge release-asset URL convention.
	// Template placeholders: {name} {tag} {version} {os} {arch} {asset}.
	// When {asset} is present, candidate asset filenames are substituted;
	// otherwise the expanded template is fetched as-is.
	DownloadURL string `yaml:"download_url,omitempty"`
	// Requires lists plugins this plugin needs at runtime.
	Requires []PluginRequirement `yaml:"requires,omitempty"`
}

// pluginMetadataFileName is the well-known path of the author-side metadata
// file at the root of a plugin repository.
const pluginMetadataFileName = "entire-plugin.yml"

// ParsePluginMetadata decodes entire-plugin.yml content. Strict decoding:
// unknown keys are an error, surfacing author typos at install time rather
// than silently ignoring a misspelled "requires".
func ParsePluginMetadata(data []byte) (*PluginMetadata, error) {
	var meta PluginMetadata
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&meta); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", pluginMetadataFileName, err)
	}
	if meta.Name != "" {
		if err := validatePluginName(meta.Name); err != nil {
			return nil, fmt.Errorf("%s declares invalid name: %w", pluginMetadataFileName, err)
		}
	}
	for _, req := range meta.Requires {
		if err := validatePluginName(req.Name); err != nil {
			return nil, fmt.Errorf("%s declares invalid requirement: %w", pluginMetadataFileName, err)
		}
		// A requirement's repo_url is author-controlled and reaches the git
		// CLI during dependency planning — which runs before the install
		// confirmation prompt. Reject non-URLs at the parse boundary.
		if req.RepoURL != "" {
			if err := validatePluginRepoURL(req.RepoURL); err != nil {
				return nil, fmt.Errorf("%s declares invalid repo_url for requirement %q: %w", pluginMetadataFileName, req.Name, err)
			}
		}
	}
	return &meta, nil
}

// PluginPkgDir returns the per-plugin package directory for the given bare
// name. Not created — callers use EnsurePluginPkgDir when writing.
func PluginPkgDir(name string) (string, error) {
	if err := validatePluginName(name); err != nil {
		return "", err
	}
	parent, err := pluginParentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, pluginManagedPkgSubdir, name), nil
}

// EnsurePluginPkgDir creates the package directory for name.
func EnsurePluginPkgDir(name string) (string, error) {
	dir, err := PluginPkgDir(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create plugin pkg dir: %w", err)
	}
	return dir, nil
}

// LoadPluginManifest reads the manifest for name. Returns (nil, nil) when
// the plugin has no manifest — local-dev installs and raw-PATH plugins.
func LoadPluginManifest(name string) (*PluginManifest, error) {
	dir, err := PluginPkgDir(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, pluginManifestFileName)) //nolint:gosec // path is inside the managed pkg tree
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil //nolint:nilnil // no-manifest signal
		}
		return nil, fmt.Errorf("read plugin manifest: %w", err)
	}
	var m PluginManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse plugin manifest for %q: %w", name, err)
	}
	return &m, nil
}

// SavePluginManifest writes the manifest into the plugin's pkg dir.
func SavePluginManifest(m *PluginManifest) error {
	dir, err := EnsurePluginPkgDir(m.Name)
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal plugin manifest: %w", err)
	}
	path := filepath.Join(dir, pluginManifestFileName)
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // manifest is non-secret provenance metadata
		return fmt.Errorf("write plugin manifest: %w", err)
	}
	return nil
}

// ListPluginManifests returns the manifests of every remote-installed
// plugin, sorted by name. Pkg entries without a readable manifest are
// skipped — a half-removed plugin shouldn't break listing.
func ListPluginManifests() ([]*PluginManifest, error) {
	parent, err := pluginParentDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(parent, pluginManagedPkgSubdir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read plugin pkg dir: %w", err)
	}
	var out []*PluginManifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := LoadPluginManifest(e.Name())
		if err != nil || m == nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RemovePluginPkg deletes the plugin's pkg dir (binary + manifest).
// Missing dir is not an error — local-dev installs never had one.
func RemovePluginPkg(name string) error {
	dir, err := PluginPkgDir(name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove plugin pkg dir: %w", err)
	}
	return nil
}

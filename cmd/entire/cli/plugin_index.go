package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Plugin discovery rides on a git-synced index, krew-style: the index is
// itself a git repository containing index.json, shallow-cloned into the
// user cache and re-pulled on a TTL. No hosted service, no forge REST API —
// any git server can host an index, and a repo can point contributors at an
// internal catalog via plugins.index_url in .entire/settings.json.
const (
	// defaultPluginIndexURL is the built-in curated index.
	defaultPluginIndexURL = "https://github.com/entireio/plugin-index"
	// pluginIndexEnvVar overrides the index URL (forwarded to plugin
	// children automatically via the ENTIRE_ allowlist prefix).
	pluginIndexEnvVar = "ENTIRE_PLUGIN_INDEX_URL"
	// pluginIndexFileName is the catalog file at the index repo root.
	pluginIndexFileName = "index.json"
	// pluginIndexSchemaVersion is the index schema this CLI reads. Advisory:
	// a higher declared version logs a warning and is still read (see
	// loadPluginIndexFromDir).
	pluginIndexSchemaVersion = 1
	// pluginIndexSyncMarker records the last successful sync time as the
	// marker file's mtime. Untracked, so fetch/reset never touches it.
	pluginIndexSyncMarker = ".entire-last-sync"
)

// PluginIndex is the parsed catalog. Decoding is deliberately lenient
// (unknown fields ignored) so an index that grows new fields doesn't break
// older CLI versions fleet-wide.
type PluginIndex struct {
	// Version is the declared schema version. Advisory only — see
	// loadPluginIndexFromDir for why it isn't enforced.
	Version int                `json:"version"`
	Plugins []PluginIndexEntry `json:"plugins"`
}

// PluginIndexEntry describes one plugin in the catalog.
type PluginIndexEntry struct {
	Name        string `json:"name"`
	RepoURL     string `json:"repo_url"`
	Description string `json:"description,omitempty"`
	Official    bool   `json:"official,omitempty"`
	// Platforms lists supported GOOS values when the plugin doesn't ship
	// the full matrix; empty means all.
	Platforms []string `json:"platforms,omitempty"`
}

// Find returns the entry with the given bare name, or nil.
func (idx *PluginIndex) Find(name string) *PluginIndexEntry {
	if idx == nil {
		return nil
	}
	for i := range idx.Plugins {
		if idx.Plugins[i].Name == name {
			return &idx.Plugins[i]
		}
	}
	return nil
}

// Search returns entries whose name or description contains term
// (case-insensitive). An empty term returns everything.
func (idx *PluginIndex) Search(term string) []PluginIndexEntry {
	if idx == nil {
		return nil
	}
	if term == "" {
		return idx.Plugins
	}
	t := strings.ToLower(term)
	var out []PluginIndexEntry
	for _, e := range idx.Plugins {
		if strings.Contains(strings.ToLower(e.Name), t) || strings.Contains(strings.ToLower(e.Description), t) {
			out = append(out, e)
		}
	}
	return out
}

// HasRepoURL reports whether a repo URL is listed in the index — the trust
// check for install confirmations. Compared with the .git suffix and
// trailing slashes normalized.
func (idx *PluginIndex) HasRepoURL(repoURL string) bool {
	if idx == nil {
		return false
	}
	want := normalizeRepoURL(repoURL)
	for _, e := range idx.Plugins {
		if normalizeRepoURL(e.RepoURL) == want {
			return true
		}
	}
	return false
}

func normalizeRepoURL(u string) string {
	return strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(u), "/"), ".git")
}

// resolvePluginIndexURL applies the documented precedence: --index flag >
// ENTIRE_PLUGIN_INDEX_URL > settings (local over project) > built-in
// default. Settings load failures fall through to the default rather than
// blocking discovery; the failure is logged at debug.
func resolvePluginIndexURL(ctx context.Context, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv(pluginIndexEnvVar); v != "" {
		return v
	}
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		logging.Debug(ctx, "plugin index: settings load failed, using default index", slog.String("error", err.Error()))
		return defaultPluginIndexURL
	}
	if u := s.PluginIndexURL(); u != "" {
		return u
	}
	return defaultPluginIndexURL
}

// pluginIndexTTL returns the freshness window from settings (default 24h).
func pluginIndexTTL(ctx context.Context) time.Duration {
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return 24 * time.Hour
	}
	return s.PluginIndexTTL()
}

// pluginIndexCacheDir is the per-URL local copy. Keyed by a hash of the
// URL so switching indexes (or per-repo overrides) never serves one
// catalog's cache for another.
func pluginIndexCacheDir(indexURL string) (string, error) {
	cache := userdirs.Cache()
	if cache == "" {
		return "", errors.New("cannot resolve user cache directory")
	}
	sum := sha256.Sum256([]byte(normalizeRepoURL(indexURL)))
	return filepath.Join(cache, "plugin-index", hex.EncodeToString(sum[:6])), nil
}

// SyncPluginIndex returns the catalog for indexURL, cloning or refreshing
// the local copy as needed. force bypasses the TTL. When a refresh fails
// but a previous copy exists, the stale copy is used with a warning logged
// — discovery shouldn't hard-fail because a laptop is offline.
func SyncPluginIndex(ctx context.Context, indexURL string, force bool) (*PluginIndex, error) {
	// The index URL reaches `git clone` as a positional; it can come from
	// --index or ENTIRE_PLUGIN_INDEX_URL, neither of which the settings
	// validator sees. Reject non-URLs before git can read one as an option.
	if err := validatePluginRepoURL(indexURL); err != nil {
		return nil, fmt.Errorf("plugin index URL: %w", err)
	}
	dir, err := pluginIndexCacheDir(indexURL)
	if err != nil {
		return nil, err
	}

	// The cache dir is shared by every concurrent `entire plugin` invocation
	// and syncing it is destructive: RemoveAll + clone on the cold path,
	// fetch + `reset --hard FETCH_HEAD` on the warm one. Serialize the whole
	// sync-and-read under an advisory lock, the same way discovery/cache.go
	// guards its shared cache files. Without it, one process cloning while
	// another reads index.json surfaces a spurious "no readable index.json",
	// and two cold syncs racing can leave exactly the half-created directory
	// the sweep below exists to recover from.
	//
	// The lock file is a *sibling* of the cache dir, not inside it: RemoveAll
	// on the dir would otherwise delete the lock file while we hold it.
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return nil, fmt.Errorf("create index cache dir: %w", err)
	}
	release, err := flock.AcquireContext(ctx, dir+".lock")
	if err != nil {
		return nil, fmt.Errorf("lock plugin index cache: %w", err)
	}
	defer release()

	marker := filepath.Join(dir, pluginIndexSyncMarker)
	_, statErr := os.Stat(filepath.Join(dir, ".git"))
	cloned := statErr == nil

	fresh := false
	if cloned && !force {
		if info, err := os.Stat(marker); err == nil {
			fresh = time.Since(info.ModTime()) < pluginIndexTTL(ctx)
		}
	}

	switch {
	case !cloned:
		// A previously interrupted clone can leave a partial directory
		// without .git. git refuses to clone into a non-empty target, so
		// without this sweep, discovery would stay wedged until the user
		// cleared the cache by hand.
		if err := os.RemoveAll(dir); err != nil {
			return nil, fmt.Errorf("clear stale index cache: %w", err)
		}
		if _, err := gitQuiet(ctx, "clone", "--depth", "1", "--quiet", "--", indexURL, dir).Output(); err != nil {
			return nil, fmt.Errorf("clone plugin index %s: %w%s", indexURL, err, stderrSuffix(err))
		}
		touchFile(marker)
	case !fresh:
		if err := refreshPluginIndexClone(ctx, dir); err != nil {
			logging.Warn(ctx, "plugin index refresh failed; using cached copy",
				slog.String("index", indexURL), slog.String("error", err.Error()))
		} else {
			touchFile(marker)
		}
	}

	return loadPluginIndexFromDir(ctx, dir, indexURL)
}

// refreshPluginIndexClone updates an existing shallow clone to the remote
// tip regardless of the remote's default branch name.
func refreshPluginIndexClone(ctx context.Context, dir string) error {
	if _, err := gitQuiet(ctx, "-C", dir, "fetch", "--depth", "1", "--quiet", "origin", "HEAD").Output(); err != nil {
		return fmt.Errorf("fetch: %w%s", err, stderrSuffix(err))
	}
	if _, err := gitQuiet(ctx, "-C", dir, "reset", "--hard", "--quiet", "FETCH_HEAD").Output(); err != nil {
		return fmt.Errorf("reset: %w%s", err, stderrSuffix(err))
	}
	return nil
}

func touchFile(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return
	}
	if f, err := os.Create(path); err == nil { //nolint:gosec // marker file inside our cache dir
		_ = f.Close()
	}
}

func loadPluginIndexFromDir(ctx context.Context, dir, indexURL string) (*PluginIndex, error) {
	data, err := os.ReadFile(filepath.Join(dir, pluginIndexFileName)) //nolint:gosec // file inside our cache dir
	if err != nil {
		return nil, fmt.Errorf("plugin index %s has no readable %s: %w", indexURL, pluginIndexFileName, err)
	}
	var idx PluginIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse %s from %s: %w", pluginIndexFileName, indexURL, err)
	}
	// version is recorded, not enforced. Refusing an unrecognized value would
	// protect against a migration that can never happen: the index is one
	// shared resource read by every CLI version ever shipped, so bumping it
	// would break discovery fleet-wide with no gradual rollout and no undo for
	// installed binaries. An incompatible schema therefore ships at a new path
	// (index-v2.json, another branch, another repo), which needs no gate here.
	// Enforcing it only ever fired by accident — most painfully on an index
	// that simply omits the field (version 0), which told the author to
	// upgrade the CLI when the fix was in their own file. Hand-written
	// internal catalogs are exactly what plugins.index_url exists for.
	//
	// Additive changes, the ones that actually happen, are already absorbed:
	// decoding ignores unknown fields and unreadable entries are dropped
	// below. Degrading per entry beats refusing the catalog.
	if idx.Version > pluginIndexSchemaVersion {
		logging.Warn(ctx, "plugin index declares a newer schema version; reading what this CLI understands",
			slog.String("index", indexURL),
			slog.Int("declared_version", idx.Version),
			slog.Int("understood_version", pluginIndexSchemaVersion))
	}
	var valid []PluginIndexEntry
	for _, e := range idx.Plugins {
		// repo_url is validated here, not just at the git boundary: an
		// index-resolved install is treated as trusted and never prompts, so
		// a hostile catalog entry must not reach the git CLI at all.
		if validatePluginName(e.Name) != nil || validatePluginRepoURL(e.RepoURL) != nil {
			continue // tolerate bad entries rather than failing the catalog
		}
		valid = append(valid, e)
	}
	idx.Plugins = valid
	return &idx, nil
}

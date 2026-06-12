package cli

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/mod/semver"
)

// Dependency handling is install-time only. Dispatch stays zero-cost: a
// plugin invokes its deps as entire-<dep> via PATH at runtime (the managed
// bin dir is prepended for plugin children), and `entire plugin doctor`
// reports drift after the fact. There is deliberately no version-range
// solver — requirements carry at most a minimum version.

// maxDepDepth bounds transitive resolution. Plugin graphs are shallow in
// practice; the bound turns a metadata cycle that slips past the visited
// set (e.g. two names aliasing one repo) into an error instead of a hang.
const maxDepDepth = 10

// DepAction is one planned dependency install or upgrade.
type DepAction struct {
	Name       string
	RepoURL    string
	MinVersion string
	// Upgrade is true when the dependency is installed but below
	// MinVersion; the action reinstalls at the latest tag.
	Upgrade bool
	// CurrentTag is the installed tag for upgrades.
	CurrentTag string
	// FromIndex is true when the repo URL came from the index (trusted)
	// rather than from another plugin's metadata.
	FromIndex bool
}

// DepPlan is the result of resolving a plugin's transitive requirements.
type DepPlan struct {
	Actions []DepAction
	// Warnings are non-blocking observations, e.g. a dependency satisfied
	// from raw $PATH whose version cannot be verified.
	Warnings []string
}

// PlanDependencyInstalls resolves the transitive requirements of rootReqs
// into ordered install/upgrade actions. Missing dependencies must be
// resolvable to a repo URL via the requirement itself or the index. The
// visited set plus a depth bound make cycles an error path, not a hang.
func PlanDependencyInstalls(ctx context.Context, rootReqs []PluginRequirement, idx *PluginIndex) (*DepPlan, error) {
	plan := &DepPlan{}
	visited := map[string]bool{}
	if err := planDeps(ctx, rootReqs, idx, plan, visited, 0); err != nil {
		return nil, err
	}
	return plan, nil
}

func planDeps(ctx context.Context, reqs []PluginRequirement, idx *PluginIndex, plan *DepPlan, visited map[string]bool, depth int) error {
	if depth > maxDepDepth {
		return fmt.Errorf("dependency chain deeper than %d; cycle in plugin metadata?", maxDepDepth)
	}
	for _, req := range reqs {
		if visited[req.Name] {
			continue
		}
		visited[req.Name] = true

		satisfied, warning, err := dependencySatisfied(req)
		if err != nil {
			return err
		}
		if warning != "" {
			plan.Warnings = append(plan.Warnings, warning)
		}
		if satisfied {
			// An already-satisfied managed dependency can itself have
			// gaps (e.g. its own dep was removed with --force since
			// install). Walk its recorded requirements — offline, from
			// the manifest — so installing a parent repairs the whole
			// chain instead of stopping at the first satisfied node.
			// PATH/local-dev installs have no manifest; doctor covers
			// those.
			m, err := LoadPluginManifest(req.Name)
			if err != nil {
				return err
			}
			if m != nil {
				if err := planDeps(ctx, m.Requires, idx, plan, visited, depth+1); err != nil {
					return err
				}
			}
			continue
		}

		m, err := LoadPluginManifest(req.Name)
		if err != nil {
			return err
		}
		action := DepAction{Name: req.Name, MinVersion: req.MinVersion}
		if m != nil {
			// Installed but below min_version: upgrade in place from its
			// recorded repo.
			action.Upgrade = true
			action.CurrentTag = m.Tag
			action.RepoURL = m.RepoURL
		} else {
			action.RepoURL = req.RepoURL
			if action.RepoURL == "" {
				if e := idx.Find(req.Name); e != nil {
					action.RepoURL = e.RepoURL
					action.FromIndex = true
				}
			} else if idx.HasRepoURL(action.RepoURL) {
				action.FromIndex = true
			}
			if action.RepoURL == "" {
				return fmt.Errorf("dependency %q is not installed and has no repo URL (not in the plugin index either); add repo_url to the requirement or install it manually", req.Name)
			}
		}
		plan.Actions = append(plan.Actions, action)

		// Recurse into what the dependency itself requires, using metadata
		// only — nothing is downloaded during planning. Inspection
		// failures don't fail the plan (the install will surface hard
		// errors itself), but they must not be silent either: a confirmed
		// plan that quietly omitted nested dependencies would look
		// complete while leaving gaps for doctor to find.
		tag := ""
		if tags, err := listRemoteSemverTags(ctx, action.RepoURL); err == nil && len(tags) > 0 {
			tag = tags[0]
		}
		if tag == "" {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("could not list tags for %q (%s); its own dependencies were not inspected — 'entire plugin doctor' will report any gaps", req.Name, action.RepoURL))
			continue
		}
		meta, err := fetchPluginMetadataAtTag(ctx, action.RepoURL, tag)
		if err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("could not read %s for %q at %s; its own dependencies were not inspected — 'entire plugin doctor' will report any gaps", pluginMetadataFileName, req.Name, tag))
			continue
		}
		if meta == nil {
			continue // no metadata file: no declared dependencies
		}
		if err := planDeps(ctx, meta.Requires, idx, plan, visited, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// dependencySatisfied checks whether a requirement is already met, in
// order: managed install with manifest (version-checkable) → managed entry
// without manifest (local-dev, version unknown) → raw $PATH (version
// unknown). Note `entire plugin <verb>` runs with the user's original PATH
// (main.go restores it for built-ins), so LookPath here sees only
// raw-PATH plugins — managed entries are checked explicitly first.
func dependencySatisfied(req PluginRequirement) (satisfied bool, warning string, err error) {
	m, err := LoadPluginManifest(req.Name)
	if err != nil {
		return false, "", err
	}
	if m != nil {
		if req.MinVersion == "" || semver.Compare(canonicalSemver(m.Tag), canonicalSemver(req.MinVersion)) >= 0 {
			return true, "", nil
		}
		return false, "", nil // triggers the upgrade path in planDeps
	}
	installed, err := FindInstalledPlugin(req.Name)
	if err != nil {
		return false, "", err
	}
	if installed != nil {
		return true, unverifiableVersionWarning(req, "a local-dev install"), nil
	}
	if _, err := exec.LookPath(pluginBinaryPrefix + req.Name); err == nil {
		return true, unverifiableVersionWarning(req, "$PATH"), nil
	}
	return false, "", nil
}

func unverifiableVersionWarning(req PluginRequirement, source string) string {
	if req.MinVersion == "" {
		return ""
	}
	return fmt.Sprintf("dependency %q is satisfied from %s; cannot verify min_version %s", req.Name, source, req.MinVersion)
}

// ExecuteDepPlan runs the planned installs. Upgrades pass Force.
func ExecuteDepPlan(ctx context.Context, plan *DepPlan) error {
	for _, a := range plan.Actions {
		if _, err := InstallPluginFromRepo(ctx, RemoteInstallOptions{RepoURL: a.RepoURL, Force: a.Upgrade}); err != nil {
			return fmt.Errorf("install dependency %q: %w", a.Name, err)
		}
	}
	return nil
}

// DependentsOf returns the names of managed plugins whose manifests list
// name as a requirement — the remove guard. Computed by scanning
// pkg/*/manifest.yml; no extra state to maintain.
func DependentsOf(name string) ([]string, error) {
	manifests, err := ListPluginManifests()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range manifests {
		for _, req := range m.Requires {
			if req.Name == name {
				out = append(out, m.Name)
				break
			}
		}
	}
	return out, nil
}

// PluginDoctorIssue is one problem found by RunPluginDoctor.
type PluginDoctorIssue struct {
	Plugin  string
	Problem string
	Fix     string
}

// RunPluginDoctor checks every managed plugin: bin entry present, dangling
// local-dev symlinks, dependency presence and min_versions, and (macOS) a
// quarantine attribute that would block execution.
func RunPluginDoctor(ctx context.Context) ([]PluginDoctorIssue, error) {
	var issues []PluginDoctorIssue

	installed, err := ListInstalledPlugins()
	if err != nil {
		return nil, err
	}
	installedByName := map[string]*InstalledPlugin{}
	for _, p := range installed {
		installedByName[p.Name] = p
		if p.Symlink {
			if _, err := exec.LookPath(p.Path); err != nil {
				issues = append(issues, PluginDoctorIssue{
					Plugin:  p.Name,
					Problem: "managed entry is a dangling or non-executable link to " + p.LinkTarget,
					Fix:     "rebuild the target or run: entire plugin remove " + p.Name,
				})
			}
		}
		if q := quarantinedPath(ctx, p.Path); q != "" {
			issues = append(issues, PluginDoctorIssue{
				Plugin:  p.Name,
				Problem: "binary carries the macOS quarantine attribute; Gatekeeper will block it",
				Fix:     "run: xattr -d com.apple.quarantine " + q,
			})
		}
	}

	manifests, err := ListPluginManifests()
	if err != nil {
		return nil, err
	}
	for _, m := range manifests {
		if installedByName[m.Name] == nil {
			issues = append(issues, PluginDoctorIssue{
				Plugin:  m.Name,
				Problem: "has an install manifest but no entry in the managed bin dir",
				Fix:     fmt.Sprintf("reinstall: entire plugin install %s --force", m.RepoURL),
			})
		}
		for _, req := range m.Requires {
			satisfied, warning, err := dependencySatisfied(req)
			if err != nil {
				return nil, err
			}
			switch {
			case satisfied && warning != "":
				issues = append(issues, PluginDoctorIssue{Plugin: m.Name, Problem: warning, Fix: ""})
			case !satisfied:
				dep, depErr := LoadPluginManifest(req.Name)
				if depErr != nil {
					return nil, depErr
				}
				if dep != nil {
					issues = append(issues, PluginDoctorIssue{
						Plugin:  m.Name,
						Problem: fmt.Sprintf("requires %s >= %s but %s is installed", req.Name, req.MinVersion, dep.Tag),
						Fix:     "run: entire plugin upgrade " + req.Name,
					})
				} else {
					issues = append(issues, PluginDoctorIssue{
						Plugin:  m.Name,
						Problem: fmt.Sprintf("requires %q, which is not installed", req.Name),
						Fix:     "run: entire plugin install " + req.Name,
					})
				}
			}
		}
	}
	return issues, nil
}

// quarantinedPath returns the path to flag when the file (or its symlink
// target) carries com.apple.quarantine. Best-effort, macOS only; any error
// means "not quarantined". The attribute appears when a user manually
// drops a browser-downloaded binary into the managed dir — CLI downloads
// don't set it.
//
// Checking the bin entry is sufficient even though remote installs link
// bin/entire-<name> to the real binary under pkg/: macOS xattr follows
// symlinks by default (-s is the flag to act on the link itself), so both
// the probe here and the suggested `xattr -d` fix operate on the target.
func quarantinedPath(ctx context.Context, path string) string {
	if runtime.GOOS != darwinGOOS {
		return ""
	}
	out, err := exec.CommandContext(ctx, "xattr", "-p", "com.apple.quarantine", path).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return ""
	}
	return path
}

package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// depTestOldTag is the below-min_version tag used across dependency tests.
const depTestOldTag = "v0.1.0"

// withIsolatedPath shrinks $PATH to git's dir plus the system basics, so a
// developer whose shell PATH includes a real managed plugin dir (the normal
// case for anyone using entire plugins) can't leak entire-* binaries into
// dependencySatisfied's LookPath fallback.
func withIsolatedPath(t *testing.T) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(gitPath), "/usr/bin", "/bin"}, string(filepath.ListSeparator)))
}

func TestDependentsOf(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	mustSave := func(m *PluginManifest) {
		t.Helper()
		if err := SavePluginManifest(m); err != nil {
			t.Fatal(err)
		}
	}
	mustSave(&PluginManifest{Name: "brain", RepoURL: "https://x.example/entire-brain", Tag: "v1.0.0",
		Requires: []PluginRequirement{{Name: "sem"}}})
	mustSave(&PluginManifest{Name: "viz", RepoURL: "https://x.example/entire-viz", Tag: "v1.0.0",
		Requires: []PluginRequirement{{Name: "sem"}, {Name: "run"}}})
	mustSave(&PluginManifest{Name: "sem", RepoURL: "https://x.example/entire-sem", Tag: "v1.0.0"})

	deps, err := DependentsOf("sem")
	if err != nil {
		t.Fatalf("DependentsOf: %v", err)
	}
	if len(deps) != 2 || deps[0] != "brain" || deps[1] != "viz" {
		t.Errorf("DependentsOf(sem) = %v, want [brain viz]", deps)
	}
	if deps, err := DependentsOf("brain"); err != nil || len(deps) != 0 {
		t.Errorf("DependentsOf(brain) = %v, want none", deps)
	}
}

func TestDependencySatisfied_ManagedManifest(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	if err := SavePluginManifest(&PluginManifest{Name: "sem", RepoURL: "https://x.example/entire-sem", Tag: "v0.3.0"}); err != nil {
		t.Fatal(err)
	}
	ok, warn, err := dependencySatisfied(PluginRequirement{Name: "sem", MinVersion: "v0.2.0"})
	if err != nil || !ok || warn != "" {
		t.Errorf("satisfied above min: ok=%t warn=%q err=%v", ok, warn, err)
	}
	ok, _, err = dependencySatisfied(PluginRequirement{Name: "sem", MinVersion: "v0.4.0"})
	if err != nil || ok {
		t.Errorf("below min must be unsatisfied: ok=%t err=%v", ok, err)
	}
	ok, _, err = dependencySatisfied(PluginRequirement{Name: "ghost"})
	if err != nil || ok {
		t.Errorf("missing dep must be unsatisfied: ok=%t err=%v", ok, err)
	}
}

func TestPlanDependencyInstalls(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	ctx := context.Background()

	// sem installed but old; run missing and resolvable via index;
	// path-only is satisfied via local-dev managed entry (no manifest).
	if err := SavePluginManifest(&PluginManifest{Name: "sem", RepoURL: "https://x.example/entire-sem", Tag: depTestOldTag}); err != nil {
		t.Fatal(err)
	}
	idx := &PluginIndex{Version: 1, Plugins: []PluginIndexEntry{
		{Name: "run", RepoURL: newTaggedPluginRepo(t, "", "v1.0.0")},
	}}

	plan, err := PlanDependencyInstalls(ctx, []PluginRequirement{
		{Name: "sem", MinVersion: "v0.2.0"},
		{Name: "run"},
	}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls: %v", err)
	}
	if len(plan.Actions) != 2 {
		t.Fatalf("actions = %+v, want 2", plan.Actions)
	}
	bySemName := map[string]DepAction{}
	for _, a := range plan.Actions {
		bySemName[a.Name] = a
	}
	if a := bySemName["sem"]; !a.Upgrade || a.CurrentTag != depTestOldTag || a.RepoURL != "https://x.example/entire-sem" {
		t.Errorf("sem action = %+v, want upgrade from its recorded repo", a)
	}
	if a := bySemName["run"]; a.Upgrade || !a.FromIndex {
		t.Errorf("run action = %+v, want fresh install from index", a)
	}
}

func TestPlanDependencyInstalls_UnresolvableDep(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	_, err := PlanDependencyInstalls(context.Background(), []PluginRequirement{{Name: "mystery"}}, &PluginIndex{Version: 1})
	if err == nil || !strings.Contains(err.Error(), "mystery") {
		t.Errorf("err = %v, want unresolvable-dependency error naming the plugin", err)
	}
}

func TestPlanDependencyInstalls_VisitedSetBreaksCycles(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	// a requires b; b requires a. Both missing, both resolvable. The
	// visited set must terminate planning with each appearing once.
	repoA := newTaggedPluginRepo(t, "name: cyca\nrequires:\n  - name: cycb\n", "v1.0.0")
	repoB := newTaggedPluginRepo(t, "name: cycb\nrequires:\n  - name: cyca\n", "v1.0.0")
	idx := &PluginIndex{Version: 1, Plugins: []PluginIndexEntry{
		{Name: "cyca", RepoURL: repoA},
		{Name: "cycb", RepoURL: repoB},
	}}
	plan, err := PlanDependencyInstalls(context.Background(), []PluginRequirement{{Name: "cyca"}}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls: %v", err)
	}
	if len(plan.Actions) != 2 {
		t.Errorf("actions = %+v, want exactly [cyca cycb]", plan.Actions)
	}
}

func TestRunPluginDoctor_FlagsMissingAndOutdatedDeps(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	if err := SavePluginManifest(&PluginManifest{Name: "brain", RepoURL: "https://x.example/entire-brain", Tag: "v1.0.0",
		Requires: []PluginRequirement{{Name: "sem", MinVersion: "v0.2.0"}, {Name: "ghost"}}}); err != nil {
		t.Fatal(err)
	}
	if err := SavePluginManifest(&PluginManifest{Name: "sem", RepoURL: "https://x.example/entire-sem", Tag: depTestOldTag}); err != nil {
		t.Fatal(err)
	}
	issues, err := RunPluginDoctor(context.Background())
	if err != nil {
		t.Fatalf("RunPluginDoctor: %v", err)
	}
	var problems []string
	for _, i := range issues {
		problems = append(problems, i.Problem)
	}
	joined := strings.Join(problems, " | ")
	if !strings.Contains(joined, "requires sem >= v0.2.0") {
		t.Errorf("doctor missed outdated dep: %s", joined)
	}
	if !strings.Contains(joined, `requires "ghost"`) {
		t.Errorf("doctor missed missing dep: %s", joined)
	}
	// brain and sem have manifests but no bin entries in this synthetic
	// setup — doctor flags that too.
	if !strings.Contains(joined, "no entry in the managed bin dir") {
		t.Errorf("doctor missed manifest-without-bin: %s", joined)
	}
}

func TestPlanDependencyInstalls_WalksSatisfiedTransitives(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	// sem is installed and satisfied, but its own recorded requirement
	// ("leaf") is missing — e.g. removed with --force after install.
	// Planning a parent that requires sem must surface leaf.
	if err := SavePluginManifest(&PluginManifest{
		Name: "sem", RepoURL: "https://x.example/entire-sem", Tag: "v1.0.0",
		Requires: []PluginRequirement{{Name: "leaf"}},
	}); err != nil {
		t.Fatal(err)
	}
	idx := &PluginIndex{Version: 1, Plugins: []PluginIndexEntry{
		{Name: "leaf", RepoURL: newTaggedPluginRepo(t, "", "v1.0.0")},
	}}
	plan, err := PlanDependencyInstalls(context.Background(), []PluginRequirement{{Name: "sem"}}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Name != "leaf" {
		t.Errorf("actions = %+v, want [leaf] via satisfied sem's manifest", plan.Actions)
	}
}

func TestPlanDependencyInstalls_WarnsOnUninspectableDep(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	withIsolatedPath(t)
	// The dependency's repo has no tags, so its own requirements can't be
	// inspected during planning. The action must still be planned, with a
	// warning instead of silence.
	untaggedRepo := newTaggedPluginRepo(t, "") // commit, no tags
	idx := &PluginIndex{Version: 1, Plugins: []PluginIndexEntry{
		{Name: "leaf", RepoURL: untaggedRepo},
	}}
	plan, err := PlanDependencyInstalls(context.Background(), []PluginRequirement{{Name: "leaf"}}, idx)
	if err != nil {
		t.Fatalf("PlanDependencyInstalls: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Name != "leaf" {
		t.Fatalf("actions = %+v, want [leaf]", plan.Actions)
	}
	found := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "leaf") && strings.Contains(w, "not inspected") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want uninspectable-dep warning naming leaf", plan.Warnings)
	}
}

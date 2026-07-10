# Experimental Command Gating — Design

Date: 2026-07-10
Branch: `experimental-command-gating`

## Problem

A curated set of maturing/hidden commands should be **visible to developers**
(any non-release build: `go build`, `go run`, `mise`) and **hidden in shipped
binaries** (any GoReleaser build, prod and nonprod). They should also be grouped
under an "Experimental commands:" section in `entire help` when visible, instead
of being scattered as unrelated hidden commands.

This replaces the current hardcoded `Hidden: true` on these commands with a
single build-time gate.

## Scope

### Gated as experimental (10 commands)

Root-level (join root's "Experimental commands:" help group):

- `tokens` (`tokens_profile.go`, `newTokensGroupCmd`) — labs token diagnostics
- `import` (`import_cmd.go`)
- `review` (`review/cmd.go`, separate package)
- `investigate` (`investigate/cmd.go`, separate package)
- `blame` (`attribution.go`)
- `why` (`attribution.go`)
- `search` (`search_cmd.go`) — the top-level `entire search` shortcut
- `experts` (`experts_cmd.go`)
- `runner` (`runner_group.go`)

Sub-command (joins an experimental group under `checkpoint`):

- `checkpoint policy` (`checkpoint_policy.go`, registered in `checkpoint_group.go`)

### Explicitly out of scope (untouched, stay always-hidden)

These are hidden for reasons other than "experimental" and must keep working in
release binaries:

- Infra/plumbing invoked by git hooks or agents: `hooks`, `hooks git ...`,
  `mcp`, `__send_analytics`, `curl-bash-post-install`, `trail`, agent hook
  registry commands.
- Deprecated shortcuts (functional, emit hint): `resume`, `attach`, `explain`,
  `trace`, `reset`, `rewind`.
- Cobra-native aliases: `sessions`, `cp`, `checkpoints`.

Note: `entire search` (root shortcut) is gated; the canonical `checkpoint
search` is **not** hidden and is left untouched. The canonical `checkpoint
search` in `checkpoint_group.go` stays a normal `AddCommand`.

## Design

### Gate mechanism

New package `cmd/entire/cli/experimental/experimental.go`, mirroring the
`versioninfo` ldflags precedent (default value is dev-friendly; only GoReleaser
stamps a non-default):

```go
package experimental

import "github.com/spf13/cobra"

// State is stamped by GoReleaser via ldflags
// (-X github.com/entireio/cli/cmd/entire/cli/experimental.State=false)
// to hide experimental commands in shipped binaries. It defaults to "true",
// so every non-release build (go build, go run, mise) shows them.
var State = "true"

// Enabled reports whether experimental commands are visible.
func Enabled() bool { return State != "false" }

// GroupID is the cobra group experimental commands are filed under.
const GroupID = "experimental"

const groupTitle = "Experimental commands:"

// Register adds child under parent, gated and grouped as experimental.
// It overrides any Hidden value the child's constructor set, so callers do not
// need to touch the constructors (including ones in other packages).
func Register(parent, child *cobra.Command) {
	if !parent.ContainsGroup(GroupID) {
		parent.AddGroup(&cobra.Group{ID: GroupID, Title: groupTitle})
	}
	child.Hidden = !Enabled()
	child.GroupID = GroupID
	parent.AddCommand(child)
}
```

Rationale for `Register(parent, child)` overriding `Hidden`: the two experimental
commands in other packages (`review`, `investigate`) set `Hidden: true`
internally. Overriding at the registration site means we do not edit those
packages and there is a single source of truth for the gate.

The cobra group is always registered on the parent (even in release builds where
every child is hidden) so cobra never panics on an unregistered `GroupID`. When
all children are hidden, cobra omits the empty group header from help output.

### Wiring

Swap `AddCommand` → `experimental.Register` at exactly these sites:

- `cmd/entire/cli/root.go`: the 9 root-level commands listed above.
- `cmd/entire/cli/checkpoint_group.go`: `newCheckpointPolicyCmd()` (adds an
  experimental group under `checkpoint`). The `checkpoint search` line stays
  unchanged.

No changes to the command constructors themselves. Inline `Hidden: true` in the
constructors is harmless (overridden by `Register`) but may be left as-is to
keep the diff focused on registration sites.

### Build gating

- Dev — `go build ./cmd/entire`, `go run`, mise: no ldflags → `State="true"` →
  experimental visible and grouped.
- Release — both GoReleaser configs, `entire` binary only (not
  `git-remote-entire`), add to the `ldflags` list:
  ```
  -X github.com/entireio/cli/cmd/entire/cli/experimental.State=false
  ```
  - `.goreleaser.yaml` (prod `entire` build)
  - `.goreleaser.nonprod.yaml` (nightly/internal — also hidden, per decision)

### mise convenience task

Add a plain build task (no experimental ldflags, so experimental stays visible):

```toml
[tasks.build]
run = "CGO_ENABLED=0 go build -o entire ./cmd/entire/"
```

This is ergonomic only; any non-GoReleaser build already defaults to visible.

## Testing

- `cmd/entire/cli/experimental/experimental_test.go`: truth table for
  `Enabled()` (`State` = "true" / "false" / "" / arbitrary). Save/restore
  `State`; cannot `t.Parallel()` (mutates package global).
- cli-level test (in the `cli` package): flip `experimental.State`, build the
  root command, and assert:
  - release (`State="false"`): each of the 10 commands has `Hidden == true`.
  - dev (`State="true"`): each has `Hidden == false`, `GroupID == "experimental"`,
    and the parent reports `ContainsGroup("experimental")`.
  - Cannot `t.Parallel()` — mutates the global `State`; save/restore in the test.

## Non-goals

- No change to which commands exist or their behavior — only visibility/grouping.
- No change to infra, deprecated, or alias commands.
- No runtime flag/env var to toggle experimental (build-time only, per request).

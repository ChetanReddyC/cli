# Codex Hooks in Linked Worktrees

Status: implemented design for [PR #2052](https://github.com/entireio/cli/pull/2052),
with full-suite verification intentionally deferred to implementation-order
item 9.

This document defines how Entire resolves, inspects, installs, and removes Codex
hooks when the current checkout is a linked Git worktree. It incorporates the
scope and safety feedback in
[Soph's review](https://github.com/entireio/cli/pull/2052#issuecomment-5400538888).

## Decision

PR #2052 contains two related changes:

1. A read-only discovery and diagnostic path that reports the hook file Codex
   actually loads.
2. A hardened mutation path that installs and removes hooks only in the current
   checkout.

Entire will not write to another checkout. It will not migrate hook files from
one worktree to another, clean another checkout's file, create another
checkout's `.codex` directory, or coordinate repository-wide writes with a
shared lock.

The ownership rule is:

> A command may mutate the checkout in which it was invoked. It may inspect
> another checkout to explain Codex's effective configuration, but it may not
> modify that checkout.

## Problem

Linked worktrees share Git metadata and objects but have separate working
directories and usually separate branches:

```text
/repo-main/                         primary checkout
  .git/                             common Git directory
  .codex/hooks.json                 root-checkout Codex hooks

/repo-feature/                      linked checkout
  .git                              gitdir pointer into /repo-main/.git
  .codex/hooks.json                 feature-branch Codex hooks
```

Codex's app-server documentation states that, for linked worktrees, project
hook declarations come from the matching `.codex` directory in the root
checkout rather than from a divergent hook file stored only in the linked
worktree. The behavior is observable through the app-server's `hooks/list`
method.

Entire historically wrote `.codex/hooks.json` relative to the current
worktree. That is the correct mutation boundary, but it can produce a confusing
intermediate state: the linked worktree contains the desired configuration
while the currently installed Codex version still reports hooks from the root
checkout.

The previous version of PR #2052 tried to remove that intermediate state by
writing directly to the root checkout. That creates action at a distance:

- `entire enable` in a scratch worktree modifies the primary checkout.
- The user may never see the primary checkout's dirty file.
- `entire agent remove codex` or `entire clean` can remove hooks used by every
  linked worktree.
- Rewriting the file can change its mode even when its content change is
  otherwise valid.

The revised design surfaces the mismatch without repairing it automatically.

## Goals

- Resolve the hook configuration Codex actually discovers from the current
  checkout.
- Reuse the repository's canonical Git-layout primitives instead of adding a
  Codex-specific `.git` and `commondir` parser.
- Inspect local and discovered configurations without modifying either.
- Give the user an actionable warning when a linked-worktree hook file is not
  the file Codex currently loads.
- Keep installation and removal scoped to the current checkout.
- Harden current-checkout writes against symlink redirection, Git-metadata
  writes, unrelated-file overwrites, unbounded input, and mode changes.
- Anchor resolver behavior to a real `codex app-server` integration test.

## Non-goals

- Automatically migrating hooks into the root checkout.
- Removing a stale or misplaced file from another checkout.
- Adding a flag for repository-wide mutation in this PR.
- Synchronizing concurrent writes from multiple worktrees.
- Copying or synthesizing Codex trust hashes.
- Claiming support for bare or linked-submodule layouts without app-server
  evidence for the pinned Codex version.

## Two path roles

The implementation must keep two paths distinct:

| Path | Meaning | May Entire mutate it? |
| --- | --- | --- |
| Worktree hooks path | `<current-worktree>/.codex/hooks.json` | Yes |
| Discovered hooks path | The project hook file Codex reports for the current worktree | No, unless it is the same file as the worktree path |

The separation is visible in the API:

```go
type HookDiscovery struct {
	State           HookDiscoveryState
	DiscoveredHooks DiscoveredHooksPath
	RepositoryWide  bool
	Diagnostic      error
}

func ResolveWorktreeHooksPath(context.Context) (WorktreeHooksPath, error)
func ResolveHookDiscovery(context.Context) HookDiscovery
```

`RepositoryWide` describes the reach of the configuration Codex discovers. It
does not authorize a repository-wide write. The important constraints are:

- A discovery result contains no lock path.
- A worktree-local file is not called "legacy" merely because Codex currently
  discovers another file.
- Installation and removal do not accept `DiscoveredPath` as their target.
- Cross-checkout mutation cannot be enabled accidentally by passing the
  resolver result into a generic writer.

The intended data flow is:

```text
                         +----------------------+
Git layout primitives ->| ResolveHookDiscovery |
                         +----------+-----------+
                                    |
                                    v
                         +----------------------+
                         | Inspect + warn only  |
                         +----------------------+

Current worktree root -> worktreeHooksPath -> install/remove
```

## Git layout resolution

Codex must not implement another Git worktree parser. The repository already
has overlapping primitives:

- `gitrepo.resolveDotGitPath` resolves a checkout's `.git` directory or pointer.
- `gitrepo.resolveCommonGitPath` resolves `commondir`.
- `paths.parseWorktreeID` recognizes conventional, `.bare`, and submodule
  worktree layouts.
- `session.GetGitCommonDir` memoizes common-directory resolution for the current
  working directory.

The implementation should expose or extract one high-level Git-layout result
from these primitives. The Codex resolver consumes that result and applies only
Codex-specific discovery rules.

The shared result should provide enough information to distinguish:

- A normal checkout.
- A conventional linked worktree.
- A `.bare/worktrees` layout.
- An ordinary submodule.
- A linked submodule.
- A separate Git directory that is not a linked worktree.

Memoization must live at, or be reused by, this shared layer because hook
discovery is reachable from SessionStart. Codex should not add a second cache
around a second parser.

For an unfamiliar or contradictory layout, discovery should return an
unresolved diagnostic state. It is safer to say that Entire cannot determine
what Codex loads than to guess and produce a misleading warning.

## Read-only discovery and inspection

Discovery answers where Codex is expected to load hooks. Inspection answers
what is present at the worktree and discovered paths.

The inspection result should distinguish at least:

| State | Meaning |
| --- | --- |
| Absent | The file does not exist. |
| User-only | The file exists but contains no Entire-managed hooks. |
| Entire-managed | At least one Entire-managed hook exists. |
| Invalid | The file cannot be safely read or parsed. |
| Unresolved | The Codex discovery location cannot be determined. |

When inspecting an Entire-managed file, the result should also report:

- Whether the core Entire events exist.
- Which current managed events are missing.
- Whether managed commands and timeouts match the current CLI.
- Whether the current checkout has the project layer Codex needs.
- Whether local trust records can be inspected.
- Which declared hooks lack an approval record.

Inspection is read-only. It must not create directories, normalize a file by
rewriting it, remove entries, or acquire a mutation lock.

### Bounded reads

Repository-controlled `.git` metadata and `.codex/hooks.json` must be read with
explicit size limits. SessionStart reaches hook-trust inspection, so an
unbounded `os.ReadFile` lets a repository-controlled file allocate arbitrary
memory on a latency-sensitive path.

The reader should:

1. Inspect the path without following a final symlink.
2. Require the expected file type.
3. Reject a file larger than the configured maximum.
4. Read through an `io.LimitReader` with one extra byte to detect overflow.
5. Return an invalid inspection result rather than partially parsing data.

The size limit should be a named Codex-hook configuration limit. It may share a
generic bounded-file helper with Git metadata, but it should not silently reuse
a Git-specific constant if the product wants a different hooks-file budget.

### Read containment

Even a read-only diagnostic should not follow a repository-controlled symlink
to an arbitrary file. The worktree and discovered project directories must be
the exact canonical `<checkout-root>/.codex` directories expected by the
resolver. A resolved hooks path inside Git metadata or outside the intended
`.codex` directory is invalid.

## Checkout-local installation and removal

`InstallHooks`, `UninstallHooks`, and `AreHooksInstalled` retain
current-checkout semantics:

```text
target = <current-worktree>/.codex/hooks.json
```

They do not call the discovery resolver to select a mutation target.

Installation may preserve existing user hooks and unrelated top-level fields,
but it must not read from one checkout and write a merged document into
another. Removal deletes only Entire-managed entries from the current
checkout's file.

Install and removal serialize only against
`<current-worktree>/.codex/hooks.json.lock`. That exact-checkout lock protects
concurrent updates to the owned file without coordinating any other worktree.

The following behavior is removed from PR #2052:

- `LegacyHooksPath` migration and cleanup.
- Root-checkout installation from a linked worktree.
- Root-checkout removal from a linked worktree.
- Creation of another checkout's `.codex` project layer.
- A lock in the Git common directory.
- `RepositorySharedHooks` and setup/removal messages describing an impending
  repository-wide mutation.
- Deselection or clean logic that treats a discovered file in another checkout
  as locally removable.

## Hardened local writes

Keeping writes local does not by itself make them safe. A path can remain under
the checkout while resolving to the wrong destination:

```text
.codex -> .git
.codex/hooks.json -> ../package.json
```

Both examples stay within the checkout, but neither is a valid Codex hooks
destination.

Before any current-checkout write, the implementation must enforce:

1. The logical target is exactly `<worktree-root>/.codex/hooks.json`.
2. The resolved project directory is exactly
   `<canonical-worktree-root>/.codex`.
3. The resolved project directory is not inside Git metadata.
4. The final file resolves to a regular file at the expected hooks location,
   not an unrelated file elsewhere in the checkout.
5. A missing `.codex` directory may be created only at the exact expected path.
6. An existing non-directory `.codex` path is rejected.
7. An existing hooks file's mode is preserved.
8. A newly created hooks file uses the intended restrictive default mode.

The check must happen against the final resolved destination immediately before
the write. Validating only the initially constructed path leaves a time-of-check
to time-of-use gap and misses nested symlink redirection.

### Atomic-write mode behavior

An atomic replacement creates a new inode, so passing `0600` unconditionally
changes an existing `0644` file to `0600`. Before replacement:

- If the destination exists, capture its permission bits and use them for the
  replacement.
- If the destination does not exist, use `0600`.
- Do not copy ownership or special bits without an explicit requirement.

The write helper should have tests for both existing and new files so a future
refactor cannot reintroduce the mode change.

## Diagnostics and warnings

Warnings describe the observed state and a user-owned remedy. They must not
promise that `entire enable` will migrate or clean another checkout.

### Misplaced linked-worktree configuration

When Entire-managed hooks exist only at the worktree path:

```text
Codex hooks: NOT ACTIVE IN THIS WORKTREE
  Entire hooks are configured at:
    /repo-feature/.codex/hooks.json
  Codex currently discovers:
    /repo-main/.codex/hooks.json
  Commit the hooks file and apply that commit to the primary checkout,
  or run `entire enable` from the primary checkout.
```

### Discovered configuration is invalid

Report the path and the bounded-read, containment, or JSON error. Do not treat a
malformed file as installed merely because it exists.

### Project layer missing

If the pinned Codex version requires a local `.codex` project layer before it
discovers root-checkout hooks, report that separately from installation and
trust. This behavior must be backed by the app-server integration test rather
than only a unit-test assumption.

### Trust gaps

Trust inspection remains structural. Entire may report missing state entries
and direct the user to `/hooks`, but it does not compute or copy trusted hashes.

### Surfaces

- `entire doctor` provides the detailed paths, state, and remedy.
- `entire status` provides a concise warning suitable for humans and JSON.
- SessionStart may append a short trust or discovery warning when the required
  information is already available within the hook's latency budget.

## Propagating a hook commit between worktrees

Linked worktrees share one Git object database. A commit created in one
worktree is immediately known to the other worktrees in the same repository;
there is no need to push, fetch, copy the commit, or address it through a
remote.

The commit still belongs to the branch on which it was created. The user must
apply that commit to the branch checked out in the primary worktree.

### Recommended: a dedicated hooks commit and cherry-pick

In the linked worktree:

```bash
git add .codex/hooks.json
git commit -m "chore: update Codex hooks"
git rev-parse HEAD
```

Suppose the resulting commit is `abc1234`. In the primary worktree:

```bash
git status --short
git cherry-pick abc1234
```

Because both worktrees share Git objects, `abc1234` is already available. The
cherry-pick creates a new commit on the primary worktree's branch with the same
patch.

A dedicated hooks commit is preferable when the feature branch also contains
unrelated application changes. It lets the user activate the Codex
configuration without bringing those unrelated changes into the primary
branch.

### Alternative: merge the linked worktree's branch

If the entire feature branch is ready, the user can merge it from the primary
worktree:

```bash
git status --short
git merge feature-branch
```

This brings every commit from `feature-branch`, including the hooks change. It
is appropriate only when the branch as a whole is ready to enter the primary
branch.

### Apply only the file without preserving the original commit

If the hooks change was committed together with unrelated files and cannot be
split easily, the user can restore only the hooks file from that commit and
make a new commit in the primary worktree:

```bash
git restore --source=abc1234 -- .codex/hooks.json
git add .codex/hooks.json
git commit -m "chore: update Codex hooks"
```

This copies the file content, not the commit's other changes.

### Conflicts and dirty worktrees

Before cherry-picking or merging, the primary worktree should be clean. If both
branches changed `.codex/hooks.json`, Git may report a conflict. The user should
resolve the file, stage it, and continue:

```bash
git add .codex/hooks.json
git cherry-pick --continue
```

For a merge conflict, use `git commit` after staging the resolution instead.
Entire must not attempt to resolve this conflict automatically because the file
may contain user-owned hooks and unrelated top-level fields.

### Same branch restriction

Git normally prevents the same branch from being checked out in two worktrees.
The linked worktree therefore usually has a feature branch while the primary
worktree has `main` or another integration branch. Cherry-pick and merge are
the mechanisms that intentionally move the change between those branch
histories.

## Mapping the review findings to the design

| Review finding | Resolution in PR #2052 |
| --- | --- |
| Fourth worktree traversal implementation | Replace Codex parsing with shared Git-layout primitives and shared memoization. |
| Cross-checkout write needs a gate | Remove cross-checkout writes entirely; no gate is needed for current-checkout mutation. |
| Containment stops at checkout | Harden current-checkout writes with exact `.codex/hooks.json` identity, Git-metadata rejection, final-destination validation, and mode preservation. |
| CI does not anchor Codex behavior | Add a no-model `codex app-server` `hooks/list` integration test in a real linked worktree. |

The review's additional cleanup is included: the stale
`ErrLinkedSubmoduleHooksUnsupported` and bare-layout support claims are gone;
`.bare/worktrees` and linked-submodule discovery are unresolved until pinned by
app-server evidence, and reads of repository-controlled hook configuration are
bounded.

## Integration contract with Codex

The integration test starts `codex app-server` from exactly
`@openai/codex@0.149.0` and uses newline-delimited JSON over stdio. It performs
no model request and needs no OpenAI credentials.

The test:

1. Create a normal repository and a conventional linked worktree.
2. Put distinct hook commands in the primary and linked hook files.
3. Start `codex app-server` with an isolated `CODEX_HOME` and the linked
   worktree as its working directory.
4. Send `initialize` with test client metadata.
5. Send the `initialized` notification.
6. Send:

   ```json
   {
     "method": "hooks/list",
     "id": 2,
     "params": {"cwds": ["/absolute/path/to/linked-worktree"]}
   }
   ```

7. Read JSONL messages until response ID `2` arrives.
8. Assert that the primary hook marker and primary `sourcePath` are present.
9. Assert that the linked-only marker is absent.
10. Assert that `warnings` and `errors` are empty.

The test must use a context timeout, isolate all configuration, and avoid
`os.Chdir` so it can call `t.Parallel()`.

Local integration runs skip when `codex` is absent or is not version 0.149.0.
CI installs the pinned version and sets
`ENTIRE_TEST_REQUIRE_CODEX_APP_SERVER=1`, so a missing binary or version drift
fails rather than producing a false-green skip.

The protocol and linked-worktree behavior are documented in the
[Codex app-server README](https://github.com/openai/codex/blob/4347f94d5539880e8583028a50a19df5b202d9fa/codex-rs/app-server/README.md#L2006-L2055).

## File-level implementation outline

### Shared Git layout

- `cmd/entire/cli/gitrepo/repository.go`
  - Expose or extract a high-level Git-layout resolver from the existing `.git`
    and `commondir` primitives.
  - Centralize memoization or make the existing session cache delegate to the
    shared result.
- `cmd/entire/cli/paths/worktree.go`
  - Reuse its worktree and submodule classification rather than duplicating
    marker parsing.

### Codex discovery and inspection

- `cmd/entire/cli/agent/codex/hook_root.go`
  - Reduce it to Codex-specific discovery over the shared Git layout.
  - Remove mutation, legacy, and lock concepts.
- `cmd/entire/cli/agent/codex/hooks.go`
  - Keep structured read-only inspection.
  - Restore worktree-local mutation semantics.
  - Bound hook-document reads.
- `cmd/entire/cli/agent/codex/trust.go`
  - Inspect the discovered path read-only.
  - Use the bounded, contained reader.

### Local write safety

- `cmd/entire/cli/agent/codex/hook_path.go`
  - Validate the exact local project directory and final hooks destination.
  - Reject Git metadata and unrelated in-checkout targets.
- `cmd/entire/cli/agent/codex/hooks.go`
  - Preserve an existing file's permission bits during atomic replacement.
  - Use `0600` only for a newly created file.

### User-facing diagnostics

- `cmd/entire/cli/doctor.go`
  - Report local and discovered paths and user-owned remedies.
- `cmd/entire/cli/config.go` and status plumbing
  - Keep local installed/freshness semantics separate from effective Codex
    discovery diagnostics.
- `cmd/entire/cli/lifecycle.go`
  - Keep SessionStart diagnostics bounded and read-only.
- `cmd/entire/cli/setup.go`
  - Remove repository-wide mutation notices and cross-checkout removal logic.

### Tests

- Unit tests for shared Git layout classification.
- Unit tests for discovery and diagnostic state transitions.
- Unit tests proving install, remove, deselection, and clean never modify the
  primary checkout when invoked from a linked worktree.
- Unit tests for `.codex -> .git`, file symlinks to unrelated checkout files,
  outside-checkout symlinks, oversized files, existing-mode preservation, and
  new-file mode.
- Integration coverage for real `codex app-server` discovery.

## Implementation order

1. Add the real app-server integration test and pin the observed conventional
   linked-worktree behavior.
2. Consolidate Git-layout resolution and make the Codex resolver read-only.
3. Split discovery and worktree-local paths in the Codex API.
4. Restore install, remove, presence, deselection, and clean to
   current-checkout ownership.
5. Harden the local destination and preserve existing file modes.
6. Update doctor, status, and SessionStart warning text.
7. Remove shared locks, migration helpers, cross-checkout interfaces, and their
   tests.
8. Correct the PR description and Codex integration documentation.
9. Run formatting, lint, unit, integration, canary, duplication, and full check
   verification.

## Acceptance criteria

- `entire enable --agent codex` in a linked worktree modifies only that
  worktree.
- `entire agent remove codex` and `entire clean` in a linked worktree modify
  only that worktree.
- No Git-common-directory Codex lock is created.
- No hook file is migrated, cleaned, or removed from another checkout.
- Doctor reports both the worktree-local and Codex-discovered paths when they
  differ.
- Warning text describes how the user can propagate a commit; it does not claim
  that Entire will perform the propagation.
- Codex contains no independent parser for `.git`, `commondir`, or worktree
  markers.
- `.codex -> .git` and `hooks.json -> ../package.json` are rejected.
- An existing hooks file keeps its permission bits after an update.
- A new hooks file uses the intended restrictive mode.
- Oversized or redirected hook files produce a bounded invalid diagnostic.
- A pinned Codex app-server test confirms the conventional linked-worktree
  discovery result.
- Bare and submodule behavior is either empirically pinned or reported as
  unresolved without unsupported claims.

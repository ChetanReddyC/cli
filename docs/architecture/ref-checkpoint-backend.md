# Ref-Based Checkpoint Backend (git-refs)

This document explains the **git-refs** checkpoint backend as a system: how it stores checkpoints, how it pushes and fetches them, how it coexists with the legacy **git-branch** backend, and how it is selected through configuration.

It is the companion to [Sessions and Checkpoints](sessions-and-checkpoints.md), which covers the domain model (sessions, checkpoints, IDs) shared by both backends. Read that first for the checkpoint tree layout, checkpoint-ID linking, and the compact-transcript format; this doc focuses on what is specific to the ref-based store.

## Why a second backend

The original backend stores every committed checkpoint as a subtree of a single long-lived branch, `entire/checkpoints/v1` (the **git-branch** backend). That branch is a serialization point: every condensation rewrites its tip, every push races on one ref, and the whole history travels together.

The **git-refs** backend instead keeps **one git ref per checkpoint**:

```
refs/entire/checkpoints/<shard>/<id>
```

Each ref points at a commit whose **tree root is that checkpoint's contents** (`metadata.json`, `0/`, `1/`, `tasks/…`) — the same subtree the git-branch backend splices *under* `<id[:2]>/<id[2:]>/` in the v1 tree. Independent refs mean checkpoints are written, pushed, and fetched independently: no shared tip to contend on, and a reader can fetch exactly the one checkpoint it needs instead of the whole branch.

Both backends are **git-backed** — they store the committed record in the repo's own object store — and never touch the working branch's history.

## Backend taxonomy: primary and mirrors

Checkpoint storage is pluggable. The topology is a single **primary** plus zero or more **mirrors**:

- **Primary** — the source of truth. It serves all reads and writes, and the full checkpoint lifecycle (resume bootstrap, `doctor` reconcile, `explain` tree reads, push, cleanup, pre-push OPF) drives *its* record.
- **Mirror** — an independent backend that receives best-effort **write fan-out** only. Reads never come from a mirror.

Backends register in `checkpoint/registry.go`. Each carries a `gitBacked` capability:

| Capability | Meaning | Can be primary? | Can be mirror? |
|------------|---------|-----------------|----------------|
| `gitBacked: true` | Stores the committed record in this repo's git object store | Yes | Yes |
| `gitBacked: false` | Stores elsewhere (e.g. a filesystem store) | No — mirror-only | Yes |

Only a git-backed backend can be the primary, because the lifecycle paths above operate through the repo and its refs; a non-git-backed backend has no such ref to drive them. The two built-in backends — `git-branch` and `git-refs` — are **both** git-backed and are registered directly in the built-in registry map. The `Register()` entry point is for non-git-backed (mirror-only) backends and is used in practice only by test-only backends, so a production binary can never select an unregistered one.

A **one-of-each-type** rule lets two distinct git-backed backends run in the same topology — specifically `git-refs` as primary with `git-branch` as a mirror, which is the safe rollout configuration (see [Rollout](#configuration-and-rollout)).

## Ref layout and sharding

```
refs/entire/checkpoints/<shard>/<id>
```

- `<id>` is the full checkpoint ID (12-hex or ULID) and is always the leaf, so the ref round-trips: `RefName(id)` builds it and `ParseRef(name)` recovers the ID (`checkpoint/refs_naming.go`).
- `<shard>` is `id.ShardFor()` — the **last two characters** of the ID, for **both** formats.

A single positional rule (independent of ID kind) keeps ref naming impossible to compute inconsistently between callers, and the suffix distributes checkpoints evenly for either format:

- A **legacy hex** ID is random throughout, so its last two chars are as good as any.
- A **ULID**'s leading chars encode a millisecond timestamp (barely varying between nearby checkpoints) while its trailing chars are random — so sharding on the suffix keeps buckets even *and* keeps the ID itself lexicographically time-sortable.

`ParseRef` validates that the shard in a ref name matches the ID's own `ShardFor()` and that the tail is exactly `<shard>/<id>` (no extra path segments), so a malformed or foreign ref is rejected rather than resolved to the wrong bucket. `RefName` errors on an empty or unrecognized ID rather than emitting `refs/entire/checkpoints//`.

> **Note:** this is the git-refs namespace only. The git-branch backend keeps its own independent **first-two-chars** tree layout (`<id[:2]>/<id[2:]>/`) inside the v1 branch. The two sharding schemes are deliberately different and do not interact.

## ID formats

Checkpoint IDs come in two shapes; the store determines which is minted:

- **git-branch primary** → 12-char hex IDs.
- **git-refs primary** → 26-char ULIDs (Crockford base32, lexicographically time-sortable).

IDs are minted by `checkpoint.GenerateCheckpointID`, which picks the format from the configured primary. Never call `id.Generate()` / `id.GenerateULID()` directly from a write path. The full rationale for the two formats, `DisplayShort`, and `id.MaxIDLength` lives in [Sessions and Checkpoints → Checkpoint ID Linking](sessions-and-checkpoints.md#checkpoint-id-linking).

## Write path

The git-refs store (`gitRefsStore`, `checkpoint/refs_store.go`) shares the checkpoint-subtree machinery with the git-branch store via an embedded `*treeWriter` anchored at base path `""`. The two differ only in **where the subtree is committed**: a per-checkpoint ref instead of a subtree of the v1 branch.

Every persistent write (`WriteSession`, and the `Backfill*` operations for transcript / summary / attribution) follows the same shape:

1. **Resolve the ref's current tip** (`refBase`). A missing ref → `(ZeroHash, nil)`, so the first write to a checkpoint becomes an **orphan commit**. A real lookup failure (IO/corruption) is surfaced, never silently treated as "new checkpoint".
2. **Build the updated checkpoint subtree** from the existing tree plus the new content (shared `treeWriter` logic).
3. **Create a commit** with the current tip as parent (orphan on first write, parented thereafter), so each checkpoint accretes its **own per-checkpoint history**.
4. **Point the ref at the new commit** (`setRef`) and **enqueue it for push**.

Enqueue is best-effort: a write that lands locally but fails to enqueue must not fail condensation. The ref is still local and correct; only its remote sync is deferred until the next write to the same checkpoint re-enqueues it (see the push queue below).

## Push and fetch

Because reads can *fetch* refs on demand and there is no single branch tip to push, the git-refs backend cannot simply "push everything" at pre-push time. Deleting local refs after pushing would also hurt local workflows. Instead it tracks exactly which checkpoints changed, in a **push-discovery queue**, and pushes those.

### Push-discovery queue

`checkpoint/pushqueue.go` — a flock-protected JSONL file in the git **common dir** (so every worktree sharing the object store enqueues into one queue):

- `entire-checkpoint-push-queue.jsonl` — one `{"ref": …}` record per queued ref.
- `entire-checkpoint-push-queue.lock` — the flock.

Semantics:

- **Enqueue** appends under the lock; enqueuing an already-present or already-pushed ref is safe (drain de-dups, the push is idempotent).
- **Drain** returns the de-duplicated queued refs *without* removing them, and compacts the file in place when it held redundant lines (so a long-lived session that keeps re-enqueuing the same ref cannot grow the file unboundedly).
- **Remove** deletes refs only **after a confirmed push**, preserving any entry appended during the push. An interrupted or failed push therefore leaves its refs queued for the next pre-push — the queue degrades toward "will retry", never toward silent loss.

Rewrites are atomic (temp file + rename under the lock) so a concurrent reader never sees a half-written queue.

### Pre-push flow

`ManualCommitStrategy.PrePush` (`strategy/manual_commit_push.go`) branches on `checkpoint.PrimaryIsRefs(cfg)`. When the primary is git-refs it:

1. **Drains** the queue.
2. **Partitions** the drained refs into those that still exist locally and stale ones (dropped from the queue).
3. **Batch-pushes** the existing refs in one network round-trip (`batchPushRefs`, `strategy/push_common.go`).
4. On success, **removes** the pushed refs from the queue and runs shadow-branch cleanup.
5. On a batch failure (typically a non-fast-forward rejection), **falls back to per-ref recovery** (`pushCheckpointRefWithRecovery`) and removes from the queue only the refs that land.

### Non-force, fast-forward-only

All checkpoint-ref pushes are **fast-forward-only — never a force push.** There is no server-side ref protection, so a force push risks silently clobbering a checkpoint written elsewhere. Per-checkpoint refs normally advance by fast-forward (append-only per-checkpoint history), so this is the common case.

When a push *is* rejected as non-fast-forward — genuine divergence, e.g. the same checkpoint was written on two machines — recovery **fetches the remote ref and replays the local-only commits on top** (`fetchAndRebaseRefCommon`), then retries. After the replay the local ref is a fast-forward over the remote, so the retry is *still* non-force and the remote commit is preserved as an ancestor rather than overwritten. A genuine cherry-pick conflict (both sides rewrote the same file, e.g. root `metadata.json`) leaves the ref queued — degrading to the safe state, never forcing.

### On-demand fetch for reads

A checkpoint written on another machine has no local ref. When a read misses locally and a **ref fetcher** is configured, `resolveRefMaybeFetch` fetches that one ref from the remote and retries once. It carefully distinguishes:

- **genuinely absent** (remote has no such checkpoint) → maps to `ErrCheckpointNotFound`;
- **a real failure** (IO, network, context cancellation) → returned as-is, never swallowed as "not found".

`List` at the storage level is **local-refs-only** — it enumerates local refs and reads each root summary. There is no remote enumeration.

## Read routing and coexistence

`checkpoint.Open` returns a `kindRoutingStore` (`checkpoint/routing_store.go`) that resolves id-keyed reads across **both** git backends by the checkpoint's ID kind, so a repo running git-refs and git-branch side by side (or mid-migration) reads either format without reconfiguring:

| ID kind | Read from | Rationale |
|---------|-----------|-----------|
| **ULID** | git-refs only, never the branch | ULIDs are only ever minted under git-refs |
| **hex**, git-branch primary | branch only | branch is authoritative for hex |
| **hex**, git-refs primary | refs first, then git-branch fallback | a hex checkpoint may still sit on the pre-migration v1 branch, or have been migrated into refs |

- `List` **unions both** backends and de-dups by ID (the same checkpoint can appear in both during coexistence), keeping the most recent.
- The `firstResolved` helper tries stores in priority order; a non-final store that reports absent *or* errors falls through to the next, so a transient git-refs fetch error cannot hide a checkpoint that resolves on the branch. The final store's result (hit, absent, or error) is returned verbatim.
- The optional `AuthorReader` capability (`explain` relies on it) is preserved and routed by the same rules when both read stores provide it.
- **Writes are not kind-routed.** They target the configured primary (+ mirrors); the minted ID already matches the primary's format.

All general read paths — resume, explain, attribution, blame, tokens, attach — inherit this routing for free through `checkpoint.Open`; there is no per-command config knob.

## Configuration and rollout

Backend selection lives in the `checkpoints` block of settings (`settings/checkpoints.go`):

```json
{
  "checkpoints": {
    "primary": { "type": "git-refs" },
    "mirrors": [ { "type": "git-branch" } ]
  }
}
```

- `primary.type` is required. When the whole block is absent, the layer defaults to the **git-branch** backend with no mirrors — so existing repos are unchanged.
- `settings.local.json`'s `checkpoints` block **replaces** the one in `settings.json` wholesale (this is a selection config, not a deep-merged document).
- Config loading is **fail-soft**: a missing file, a whole-file JSON syntax error, or unrelated invalid fields all resolve to "no config" → default git-branch. It errors *only* when a present `checkpoints` block is itself invalid.
- Unknown fields are rejected (`DisallowUnknownFields`) to surface typos. The trade-off: adding a `checkpoints` field is a coordinated rollout — ship the reader before any writer emits the field.

### Environment override

`ENTIRE_CHECKPOINTS_PRIMARY` (and the optional comma-separated `ENTIRE_CHECKPOINTS_MIRRORS`) **fully replace** any settings block — env wins over file, matching other `ENTIRE_*` overrides. This is how e2e/CI and rollout drive a specific backend without editing settings; the CI test-canary job runs a matrix over `[git-branch, git-refs]` via this variable. The env override is selection-only (no per-backend config blocks).

### Rollout states

| State | `primary` | `mirrors` | Behavior |
|-------|-----------|-----------|----------|
| **Default** | `git-branch` | — | Legacy behavior; v1 branch only |
| **Parallel** | `git-refs` | `[git-branch]` | Reads are authoritative from refs (bugs surface immediately), while v1 is still written for downgrade safety |
| **Refs-only** | `git-refs` | — | Refs are the sole store |

## Checkpoint version and policy

Checkpoint formats are named `<family>-v<major>` and validated in `checkpointpolicy/format.go`:

| Format | Family | Written by |
|--------|--------|------------|
| `branch-v1` | `branch` | git-branch backend |
| `refs-v1` | `refs` | git-refs backend |

Both are in the CLI's read **and** write sets. The repo-wide checkpoint policy (`refs/entire/policies/checkpoint`, `checkpoint_version` / `checkpoint_min_version`) gates which formats a client may write and nudges upgrades; see [Sessions and Checkpoints → Checkpoint Policy](sessions-and-checkpoints.md#checkpoint-policy).

## Migration and coexistence

The read-routing rules above are what make a hex-on-branch repo and a ULID-in-refs repo the same repo: nothing needs to move for both formats to be readable. When checkpoints *are* migrated from the branch into refs, they are written under `RefName(hexID)` — i.e. **hex-named refs** — which is why a hex ID under a git-refs primary is looked up in refs first and only then falls back to the branch.

## Key files

| File | Responsibility |
|------|----------------|
| `checkpoint/registry.go` | Backend registry, `gitBacked` capability, built-in `git-branch`/`git-refs` |
| `checkpoint/open.go` | `Open` topology resolution, `PrimaryIsRefs`, `kindRoutingStore` wiring |
| `checkpoint/refs_naming.go` | `RefName` / `ParseRef`, `CheckpointRefPrefix` |
| `checkpoint/refs_store.go` | `gitRefsStore` — per-checkpoint write/read, on-demand fetch |
| `checkpoint/pushqueue.go` | Flock JSONL push-discovery queue |
| `checkpoint/routing_store.go` | `kindRoutingStore` — id-kind read routing across both backends |
| `checkpoint/id/id.go` | `ShardFor`, `Kind`/`KindOf`, ID generation |
| `checkpointpolicy/format.go` | `branch-v1` / `refs-v1` format families and read/write sets |
| `settings/checkpoints.go` | `checkpoints` block parsing + env override |
| `strategy/manual_commit_push.go` | Pre-push: drain queue, batch push, per-ref recovery |
| `strategy/push_common.go` | `batchPushRefs`, `pushCheckpointRefWithRecovery`, fetch+replay |

## Known limitations and deferred work

- **Storage-level `List` is local-only** — no remote enumeration of checkpoint refs. `List` at the routing layer still unions the two local stores.
- **OPF (OpenAI Privacy Filter) at pre-push is git-branch-only for now.** The per-ref push does not run OPF re-redaction; that is deferred until after the store lands. See `strategy/manual_commit_opf_rewrite.go` and [security-and-privacy.md](../security-and-privacy.md).
- **v1 mirror push for full downgrade safety** is a rollout concern handled by running git-branch as a mirror (the Parallel state), not an automatic behavior of the refs store itself.
- **No write-time ULID⇒refs boundary guard.** Nothing at write time enforces that a ULID checkpoint only condenses onto refs. A config flip or a missing `ENTIRE_CHECKPOINTS_PRIMARY` in an amending environment could, in principle, land a ULID on the v1 branch, which readers (routing ULIDs to refs) would then fail to find. The guard must be topology-role aware (git-branch legitimately receives ULIDs when it is a *mirror* of a git-refs primary), so it belongs with the routing layer, not in the branch store's write path.

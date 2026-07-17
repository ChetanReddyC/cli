# ENT-1055 — `entire search` v4 query path (code-level findings)

## What changed

`entire search` (semantic) now has a flag-gated v4 backend. Flag OFF (default) →
byte-identical v3 (Cloudflare worker). Flag ON + single-repo scope → v4
query-serve via the entire-api cell gateway. `--json` shape unchanged.

## Flag mechanism (matches existing CLI conventions)

The CLI has **no** PostHog flag evaluation and **no** product-settings fetch —
the only server gate is the per-repo trails probe, which isn't a general
named-flag system. The established local-gate patterns are (a) an `ENTIRE_*`
env var (e.g. `ENTIRE_CODE_SEARCH`) and (b) a `.entire/settings.json` bool
(e.g. `external_agents`). This change uses both, mirroring
`settings.IsImageExternalizationEnabled` exactly:

- `settings.IsSemanticSearchV4Enabled(ctx)` — `settings/settings.go`. Env
  `ENTIRE_SEMANTIC_SEARCH_V4=1|true` forces on; else the `semantic_search_v4`
  bool from `.entire/settings.json` / `.local.json` (merged via
  `mergeScalarFields`). Default false.

## Backend selection

- Command entry `search_cmd.go`: after building `search.Config`, if the flag is
  on **and** `searchScopedToCurrentRepo(repos, owner, repoName, allRepos)` is
  true, it calls `resolveSemanticSearchV4(...)` and populates the v4 fields on
  the config. The v3 token/URL stay populated regardless, so broader searches
  (and TUI re-searches that broaden scope) still work on v3.
- `searchScopedToCurrentRepo` → true only for: no repo filter (defaults to
  current), or a single explicit filter equal to the current repo
  (case-insensitive). `repo:*`, `--all-repos`, a different repo, or multiple
  repos → false (stay v3; cross-repo v4 is ENT-1054). A debug line records the
  fall-through.
- `resolveSemanticSearchV4` → reuses `currentRepoRef` for the repo ULID and
  `NewAuthenticatedEntireAPICellClient(ctx, insecure, fullName, ulid)` for a
  client aimed at the repo's owning cell (jurisdictional identity token; home
  fallback on unresolved placement — same helper the experts commands use). Any
  failure returns a clear error (opt-in flag → no silent v3 fallback).

## Request/response (search package, `search/search.go`)

New `Config` fields: `UseV4 bool`, `CellClient *api.Client`, `RepoID string`,
`V4RepoSlug string`. Two new predicates:

- `Config.effectiveSingleRepoSlug()` — the owner/name the request is scoped to
  (`""` for unscoped/multi), mirroring the v3 repo-param selection.
- `Config.useV4()` — v4 only when the flag/client/ULID are set **and** the
  request's effective scope still equals `V4RepoSlug` (case-insensitive). This
  is the safety belt for the TUI: a re-search that changes repo scope
  transparently falls back to v3 instead of sending the wrong repo's ULID to
  the cell.

`Search` now dispatches: `useV4()` → `searchV4`, else `searchV3` (the original
body, unchanged). Shared helpers keep the two in lockstep and avoid dupl:

- `addCommonSearchParams` — `q`/`limit`/`author`/`date`/`branch`/`page`
  (never `types`; the v4 route rejects it and v3 never sent it).
- `parseSearchResponse` — status/error/JSON decode with the exact v3 error
  wording.

`searchV4` GETs `/api/v1/semantic-search/search/v1/search?repo=<ULID>&...` via
`CellClient.Get` (secure transport, cross-host-redirect protection). Response
decodes into the same `Response` struct; the v4-only top-level fields
(`accessible_repos`, `fanout`) are dropped (no `DisallowUnknownFields`), so the
`--json` shape is identical.

## TUI

Zero TUI code changes. `search_tui.go` threads one `search.Config` through
`performSearch`/`fetchMoreResults` → `search.Search`, so pagination and
re-search inherit the v4 branch (and the per-request `useV4()` scope check).

## Files touched

- `cmd/entire/cli/search/search.go` — v4 Config fields, `effectiveSingleRepoSlug`,
  `useV4`, `searchV3`/`searchV4` split, `addCommonSearchParams`,
  `parseSearchResponse`, path consts.
- `cmd/entire/cli/search_cmd.go` — flag gate, `searchScopedToCurrentRepo`,
  `resolveSemanticSearchV4`, debug logging.
- `cmd/entire/cli/settings/settings.go` — `SemanticSearchV4` field, merge
  wiring, `IsSemanticSearchV4Enabled`.
- Tests: `search/search_v4_test.go`, `settings/settings_semantic_search_v4_test.go`,
  `search_cmd_test.go` (`TestSearchScopedToCurrentRepo`).

## Verification (production, logged in as us jurisdiction, repo entireio/cli)

Binary built from this worktree (`go build -o /tmp/entire-1055 ./cmd/entire`).

- Flag OFF `search "feature flag" --json`: v3, total 48, mixed
  pr/checkpoint/commit/session.
- Flag ON (`ENTIRE_SEMANTIC_SEARCH_V4=1`): v4, total 15. Result IDs are
  **byte-identical** to a direct `entire api --to cell
  /api/v1/semantic-search/search/v1/search?repo={repo_id}&q=...` probe (which
  bypasses the CLI code), and differ from the v3 set. Same top-level JSON keys
  (`results,total,page,total_pages,limit,counts`), same result keys
  (`type,data,searchMeta`), same `searchMeta` keys.
- Flag ON via `.entire/settings.local.json` `semantic_search_v4:true` (no env):
  total 15 (v4). 
- Flag ON + `--all-repos`, `repo:*`, or `--repo entireio/entire.io`: stays v3.
- Flag ON + `--repo entireio/cli` (explicit current): v4 (total 15).
- `mise run fmt && mise run lint`: 0 issues. Unit tests pass for all touched
  packages (the lone `tokenstore` failure is a stderr-redirect artifact of the
  parallel runner, unrelated — passes in isolation, package untouched).

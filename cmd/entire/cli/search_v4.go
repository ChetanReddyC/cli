package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/internal/coreapi"
)

// semanticSearchV4CellTimeout bounds each per-cell v4 query (token exchange +
// the query-serve call), mirroring codeSearchCellTimeout.
const semanticSearchV4CellTimeout = 30 * time.Second

// runSemanticSearch dispatches a semantic search to the v4 query-serve path
// (cross-cell fan-out) when useV4 is set, else the v3 Cloudflare worker. It is
// the single entry the command and the TUI share, so both honor the flag
// identically across the initial fetch, re-search, and pagination.
func runSemanticSearch(ctx context.Context, cfg search.Config, useV4, insecureHTTP bool) (*search.Response, error) {
	if useV4 {
		return runSemanticSearchV4(ctx, cfg, insecureHTTP)
	}
	return search.Search(ctx, cfg) //nolint:wrapcheck // v3 errors are already user-facing ("search service ...")
}

// mergedTier2Max mirrors query-serve's maxTier2 and the BFF's MERGED_TIER2_MAX:
// the ANN-only fallback tail is capped when it's all there is.
const mergedTier2Max = 15

// runSemanticSearchV4 performs a v4 query-serve search across every cell that
// hosts the caller's in-scope repos, then merges the per-cell responses into one
// v3-shaped response. It is the cli-layer counterpart to search.Search (v3) and
// the semantic sibling of searchAllCells (code): list the repo index → resolve
// scope → group by hosting cell → one query-serve call per cell → tiered merge.
//
// Scope follows the same rules as the v3 repo param:
//   - AllRepos / repo:* → unfiltered: every cell is queried with NO repo param,
//     so query-serve returns everything the caller's token authorizes there
//     (keeping the query small for users with many repos, matching the BFF).
//   - explicit repo filter(s) or the current-repo default → those slugs are
//     resolved to ULIDs and each cell is scoped to the ULIDs it hosts.
//
// Any resolution failure is a clear error rather than a silent v3 fallback: the
// flag is opt-in, so v4 breakage must stay visible during rollout.
func runSemanticSearchV4(ctx context.Context, cfg search.Config, insecureHTTP bool) (*search.Response, error) {
	coreClient, err := coreapi.New()
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return nil, errors.New("not authenticated. Run 'entire login' to authenticate")
		}
		return nil, fmt.Errorf("semantic-search-v4: resolving control-plane client: %w", err)
	}

	reposCtx, reposCancel := context.WithTimeout(ctx, 10*time.Second)
	defer reposCancel()
	repoIndex, err := coreClient.ListRepos(reposCtx, coreapi.ListReposParams{})
	if err != nil {
		return nil, fmt.Errorf("semantic-search-v4: listing repos for cell discovery: %w", err)
	}

	unfiltered := semanticSearchUnfiltered(cfg)
	indexRepos := repoIndex.Repos
	var filtered bool
	if !unfiltered {
		slugs := semanticSearchSlugs(cfg)
		if len(slugs) == 0 {
			return nil, errors.New("semantic-search-v4: could not determine the repository to search")
		}
		resolved, matched := resolveRepoFilters(slugs, repoIndex.Repos)
		if len(resolved) == 0 {
			hint := ""
			if repoIndex.Truncated {
				hint = " (repo index was truncated — the repo may exist but was not included)"
			}
			return nil, fmt.Errorf("semantic-search-v4: no matching repositories found for %v%s; unset the semantic-search-v4 flag to use v3", slugs, hint)
		}
		indexRepos = matched
		filtered = true
	} else if repoIndex.Truncated {
		logging.Warn(ctx, "semantic-search-v4: repo index truncated; cross-repo results may be incomplete")
	}

	cells := groupReposByCell(indexRepos)
	if len(cells) == 0 {
		return &search.Response{Results: []search.Result{}}, nil
	}
	resolveCellBaseURLs(ctx, coreClient, cells)

	results, err := fanOutCells(ctx, insecureHTTP, semanticSearchV4CellTimeout, cells, func(ctx context.Context, group cellGroup, client *api.Client) (*search.Response, error) {
		// Filtered: scope the cell to the ULIDs it hosts. Unfiltered: send no
		// repo param and let query-serve search everything the token authorizes
		// in that cell (avoids the 100-repo filter cap for large accounts).
		var repoIDs []string
		if filtered {
			repoIDs = group.repoIDs
		}
		return search.CellV4(ctx, client, cfg, repoIDs)
	})
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return nil, errors.New("not authenticated. Run 'entire login' to authenticate")
		}
		return nil, fmt.Errorf("semantic-search-v4: %w", err)
	}

	return mergeSemanticV4Responses(ctx, cfg.Limit, results)
}

// semanticSearchUnfiltered reports whether the search covers every accessible
// repo (repo:* or --all-repos), mirroring search.Search's v3 allRepos logic.
func semanticSearchUnfiltered(cfg search.Config) bool {
	if cfg.AllRepos {
		return true
	}
	return len(cfg.Repos) == 1 && cfg.Repos[0] == search.AllReposFilter
}

// semanticSearchSlugs returns the owner/name slugs to resolve for a scoped
// search: the explicit repo filters, or the current-repo default.
func semanticSearchSlugs(cfg search.Config) []string {
	if len(cfg.Repos) > 0 {
		return cfg.Repos
	}
	if cfg.Owner != "" && cfg.Repo != "" {
		return []string{cfg.Owner + "/" + cfg.Repo}
	}
	return nil
}

// tierOf returns a result's tier, or -1 when unset (treated as the ANN-only
// fallback tier).
func tierOf(r search.Result) int {
	if r.Meta.Tier == nil {
		return -1
	}
	return *r.Meta.Tier
}

func bm25Of(r search.Result) float64 {
	if r.Meta.BM25Score != nil {
		return *r.Meta.BM25Score
	}
	return 0
}

// annOrScoreOf prefers the raw ANN score, falling back to the overall score —
// the ordering key query-serve/BFF use for the tier-2 ANN fallback.
func annOrScoreOf(r search.Result) float64 {
	if r.Meta.ANNScore != nil {
		return *r.Meta.ANNScore
	}
	return r.Meta.Score
}

// mergeSemanticV4Responses interleaves per-cell query-serve responses into one,
// applying the SAME ordering query-serve uses within a cell (and the BFF uses
// across cells): repos first, then tier-0 by BM25 desc, then tier-1 by rerank
// score desc, then promoted tier-2 by ANN asc; when no cell produced tier 0/1
// the ANN-only fallback tail is shown (capped). Results are deduped by
// type+id and capped to limit. Rerank scores share a space across cells (same
// Cohere model), so interleaving is meaningful. All-cells-failed is an error;
// a partial failure is logged and the surviving cells are merged.
func mergeSemanticV4Responses(ctx context.Context, limit int, results []cellCallResult[*search.Response]) (*search.Response, error) {
	var bodies []*search.Response
	var failed []string
	var lastErr error
	for _, r := range results {
		switch {
		case r.err != nil:
			lastErr = r.err
			failed = append(failed, r.group.label())
		case r.value != nil:
			bodies = append(bodies, r.value)
		}
	}
	if len(bodies) == 0 {
		if lastErr != nil {
			return nil, fmt.Errorf("semantic-search-v4: %w", lastErr)
		}
		return &search.Response{Results: []search.Result{}}, nil
	}
	if len(failed) > 0 {
		logging.Warn(ctx, "semantic-search-v4: partial failure; results may be incomplete",
			"succeeded", len(bodies), "total", len(results), "failed_cells", failed)
	}

	var repos, tier0, tier1, promotedTier2, fallbackTier2 []search.Result
	for _, b := range bodies {
		// Tier-2 results arriving alongside tier 0/1 from the same cell were
		// deliberately promoted by query-serve (they keep the tier:2 tag). A
		// cell whose page is entirely tier 2 had nothing better — its ANN
		// fallback, only shown globally when tiers 0/1 are empty everywhere.
		cellHasUpperTiers := false
		for _, r := range b.Results {
			if r.Type != search.TypeRepo && (tierOf(r) == 0 || tierOf(r) == 1) {
				cellHasUpperTiers = true
				break
			}
		}
		for _, r := range b.Results {
			switch {
			case r.Type == search.TypeRepo:
				repos = append(repos, r)
			case tierOf(r) == 0:
				tier0 = append(tier0, r)
			case tierOf(r) == 1:
				tier1 = append(tier1, r)
			case cellHasUpperTiers:
				promotedTier2 = append(promotedTier2, r)
			default:
				fallbackTier2 = append(fallbackTier2, r)
			}
		}
	}
	sort.SliceStable(repos, func(i, j int) bool { return repos[i].Meta.Score > repos[j].Meta.Score })
	sort.SliceStable(tier0, func(i, j int) bool { return bm25Of(tier0[i]) > bm25Of(tier0[j]) })
	sort.SliceStable(tier1, func(i, j int) bool { return tier1[i].Meta.Score > tier1[j].Meta.Score })
	sort.SliceStable(promotedTier2, func(i, j int) bool { return annOrScoreOf(promotedTier2[i]) < annOrScoreOf(promotedTier2[j]) })

	var merged []search.Result
	if len(tier0) == 0 && len(tier1) == 0 {
		sort.SliceStable(fallbackTier2, func(i, j int) bool { return annOrScoreOf(fallbackTier2[i]) < annOrScoreOf(fallbackTier2[j]) })
		if len(fallbackTier2) > mergedTier2Max {
			fallbackTier2 = fallbackTier2[:mergedTier2Max]
		}
		merged = append(merged, repos...)
		merged = append(merged, fallbackTier2...)
	} else {
		merged = append(merged, repos...)
		merged = append(merged, tier0...)
		merged = append(merged, tier1...)
		merged = append(merged, promotedTier2...)
	}

	// Dedup by type+id (a repo mirrored across cells can return the same logical
	// result); keep the first (higher-ranked). Results without an id (e.g. repo
	// rows) are always kept.
	seen := make(map[string]bool, len(merged))
	dupesByType := make(map[string]int)
	deduped := merged[:0]
	dupes := 0
	for _, r := range merged {
		id := r.ResultID()
		if id == "" {
			deduped = append(deduped, r)
			continue
		}
		key := r.Type + "\x00" + id
		if seen[key] {
			dupes++
			dupesByType[r.Type]++
			continue
		}
		seen[key] = true
		deduped = append(deduped, r)
	}
	merged = deduped

	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	if merged == nil {
		merged = []search.Result{}
	}

	total := -dupes
	counts := &search.TypeCounts{}
	for _, b := range bodies {
		total += b.Total
		if b.Counts != nil {
			counts.Repos += b.Counts.Repos
			counts.Checkpoints += b.Counts.Checkpoints
			counts.Commits += b.Counts.Commits
			counts.PRs += b.Counts.PRs
			counts.Sessions += b.Counts.Sessions
		}
	}
	subtractDupeCounts(counts, dupesByType)
	if total < 0 {
		total = 0
	}

	return &search.Response{
		Results: merged,
		Total:   total,
		Page:    1,
		Counts:  counts,
	}, nil
}

// subtractDupeCounts removes deduplicated rows from the aggregate per-type
// counts so `counts` reflects distinct results, matching the corrected total.
func subtractDupeCounts(counts *search.TypeCounts, dupesByType map[string]int) {
	for typ, n := range dupesByType {
		switch typ {
		case search.TypeCheckpoint:
			counts.Checkpoints = max(0, counts.Checkpoints-n)
		case search.TypeCommit:
			counts.Commits = max(0, counts.Commits-n)
		case search.TypeSession:
			counts.Sessions = max(0, counts.Sessions-n)
		case search.TypeRepo:
			counts.Repos = max(0, counts.Repos-n)
		case search.TypePR:
			counts.PRs = max(0, counts.PRs-n)
		}
	}
}

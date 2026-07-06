package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/codesearch"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/spf13/cobra"
)

func newSearchCmd() *cobra.Command { //nolint:maintidx // command wiring is inherently complex
	var (
		jsonOutput       bool
		codeFlag         bool
		caseSensitive    bool
		limitFlag        int
		pageFlag         int
		authorFlag       string
		dateFlag         string
		branchFlag       string
		repoFlag         string
		allReposFlag     bool
		insecureHTTPAuth bool
	)

	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search checkpoints, commits, and sessions using semantic and keyword matching",
		Long: `Search checkpoints, commits, and sessions using hybrid search (semantic + keyword),
powered by the Entire search service.

Requires authentication via 'entire login' (GitHub device flow).

By default, results are scoped to the current repository. Use --all-repos to
search across all accessible repos.

Run without arguments to open an interactive search. Results are
displayed in an interactive table. Use --json for machine-readable output.

CLI queries also support inline filters like author:<name>, date:<week|month>,
branch:<name>, repo:<owner/name>, and repo:* to search all accessible repos.`,
		Args:   cobra.ArbitraryArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			query := strings.Join(args, " ")

			if caseSensitive && !codeFlag {
				return errors.New("--case-sensitive can only be used with --code")
			}

			if codeFlag {
				// Reject flags that only apply to checkpoint search.
				for _, pair := range []struct{ flag, name string }{
					{authorFlag, "--author"},
					{dateFlag, "--date"},
					{branchFlag, "--branch"},
				} {
					if pair.flag != "" {
						return fmt.Errorf("%s cannot be used with --code", pair.name)
					}
				}
				if cmd.Flags().Changed("page") {
					return errors.New("--page cannot be used with --code")
				}

				// For code search, only extract repo: inline filters from
				// the query. Other checkpoint filters (author:, date:,
				// branch:) are not supported and must be preserved as
				// literal search text so "author:foo" searches for that
				// string in code rather than being silently consumed.
				codeQuery, inlineRepos := extractInlineRepoFilters(query)
				var codeRepos []string
				if repoFlag != "" {
					codeRepos = []string{repoFlag}
				}
				codeRepos = append(codeRepos, inlineRepos...)
				// repo:* or --all-repos means "all repos" — no filter.
				// Otherwise, if no explicit filter was given, scope to the
				// current repo (matching the checkpoint-search default).
				hasAllRepos := allReposFlag
				for _, r := range codeRepos {
					if r == search.AllReposFilter {
						hasAllRepos = true
					}
				}
				if hasAllRepos {
					codeRepos = nil
				} else {
					// Remove any stray "*" entries.
					filtered := codeRepos[:0]
					for _, r := range codeRepos {
						if r != search.AllReposFilter {
							filtered = append(filtered, r)
						}
					}
					codeRepos = filtered

					// No explicit repo filter → derive from git origin remote.
					if len(codeRepos) == 0 {
						slug := currentRepoSlug(ctx)
						if slug == "" {
							return errors.New("could not determine current repository for code search (use --repo or --all-repos)")
						}
						codeRepos = []string{slug}
					}
				}
				return runCodeSearch(ctx, cmd, codeSearchOpts{
					query:         codeQuery,
					repoFilters:   codeRepos,
					limit:         limitFlag,
					caseSensitive: caseSensitive,
					jsonOutput:    jsonOutput,
					insecureHTTP:  insecureHTTPAuth,
				})
			}

			// Extract inline filters (author:, date:, branch:, repo:) from query args
			parsed := search.ParseSearchInput(query)
			query = parsed.Query
			if authorFlag == "" {
				authorFlag = parsed.Author
			}
			if dateFlag == "" {
				dateFlag = parsed.Date
			}
			if branchFlag == "" {
				branchFlag = parsed.Branch
			}
			repos := parsed.Repos
			if repoFlag != "" {
				repos = []string{repoFlag}
			}
			if err := search.ValidateRepoFilters(repos); err != nil {
				return fmt.Errorf("validating repo filter: %w", err)
			}

			// Check for repo:* in inline filters
			allRepos := allReposFlag
			if len(repos) == 1 && repos[0] == search.AllReposFilter {
				allRepos = true
			}

			w := cmd.OutOrStdout()
			isTerminal := interactive.IsTerminalWriter(w)
			// Mirror search.Config.HasFilters (incl. --all-repos) so an empty
			// query with only filters isn't rejected here. This guard runs
			// before git/auth, so it can't call searchCfg.HasFilters() directly.
			hasFilters := authorFlag != "" || dateFlag != "" || branchFlag != "" || len(repos) > 0 || allRepos

			// Fast-fail: no query + non-interactive mode = error (before auth/git checks)
			if query == "" && !hasFilters && (jsonOutput || !isTerminal || IsAccessibleMode()) {
				return errors.New("query required when using --json, accessible mode, or piped output. Usage: entire search <query>")
			}

			// Get the repo's GitHub remote URL
			repo, err := strategy.OpenRepository(ctx)
			if err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run this command from within a git repository.")
				return NewSilentError(err)
			}
			defer repo.Close()

			remote, err := repo.Remote("origin")
			if err != nil {
				return fmt.Errorf("could not find 'origin' remote: %w", err)
			}
			urls := remote.Config().URLs
			if len(urls) == 0 {
				return errors.New("origin remote has no URLs configured")
			}

			owner, repoName, err := search.ParseGitHubRemote(urls[0])
			if err != nil {
				return fmt.Errorf("parsing remote URL: %w", err)
			}

			serviceURL := os.Getenv("ENTIRE_SEARCH_URL")
			if serviceURL == "" {
				// Search lives on the data API host. Fall back to
				// api.BaseURL() so ENTIRE_API_BASE_URL applies; the search
				// package's DefaultServiceURL is only consulted by callers
				// that bypass this entry point.
				serviceURL = api.BaseURL()
			}

			ghToken, err := resolveSearchToken(ctx, serviceURL, insecureHTTPAuth)
			if err != nil {
				return err
			}

			searchCfg := search.Config{
				ServiceURL:  serviceURL,
				GitHubToken: ghToken,
				Owner:       owner,
				Repo:        repoName,
				Repos:       repos,
				AllRepos:    allRepos,
				Query:       query,
				Limit:       limitFlag,
				Page:        pageFlag,
				Author:      authorFlag,
				Date:        dateFlag,
				Branch:      branchFlag,
			}

			// Use wildcard query when only filters are provided
			if query == "" && searchCfg.HasFilters() {
				searchCfg.Query = search.WildcardQuery
			}

			// No query provided + interactive = open TUI with search bar focused
			if query == "" && !searchCfg.HasFilters() {
				searchCfg.Limit = search.DefaultLimit
				styles := newStatusStyles(w)
				model := newSearchModel(nil, "", 0, searchCfg, styles)
				model.mode = modeSearch
				model.input.Focus()
				p := tea.NewProgram(model)
				if _, err := p.Run(); err != nil {
					return fmt.Errorf("TUI error: %w", err)
				}
				return nil
			}

			// Fetch a full page (DefaultLimit, matching the web UI) up front and
			// paginate client-side for all output modes; the requested --limit
			// only controls the client-side page size.
			requestedLimit := searchCfg.Limit
			requestedPage := searchCfg.Page
			searchCfg.Limit = search.DefaultLimit
			searchCfg.Page = 0 // let API default to page 1

			resp, err := search.Search(ctx, searchCfg)
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}

			// JSON output: explicit flag or piped/redirected stdout
			if jsonOutput || !isTerminal {
				return writeSearchJSON(w, resp, requestedLimit, requestedPage)
			}

			styles := newStatusStyles(w)

			// Accessible mode: static table
			if IsAccessibleMode() {
				if len(resp.Results) == 0 {
					fmt.Fprintln(w, "No results found.")
					return nil
				}
				renderSearchStatic(w, resp.Results, query, resp.Total, styles)
				return nil
			}

			// Interactive TUI
			model := newSearchModel(resp.Results, query, resp.Total, searchCfg, styles)
			p := tea.NewProgram(model)
			if _, err := p.Run(); err != nil {
				return fmt.Errorf("TUI error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&codeFlag, "code", false, "Search code content across repositories")
	cmd.Flags().BoolVar(&caseSensitive, "case-sensitive", false, "Case-sensitive code search (only with --code)")
	cmd.Flags().IntVar(&limitFlag, "limit", resultsPerPage, "Maximum number of results (per page for checkpoint search, total for --code)")
	cmd.Flags().IntVar(&pageFlag, "page", 1, "Page number (1-based)")
	cmd.Flags().StringVar(&authorFlag, "author", "", "Filter by author name")
	cmd.Flags().StringVar(&dateFlag, "date", "", "Filter by time period (week or month)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "Filter by branch name")
	cmd.Flags().StringVar(&repoFlag, "repo", "", "Filter by repository (gh/owner/repo, et/proj/repo, owner/repo, ULID, or *)")
	cmd.Flags().BoolVar(&allReposFlag, "all-repos", false, "Search all accessible repos instead of just the current one")
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)

	cmd.RegisterFlagCompletionFunc("date", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck,gosec // only fails if the flag isn't defined; defined directly above
		return []string{"week", "month"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.RegisterFlagCompletionFunc("repo", completeRepoFlag) //nolint:errcheck,gosec // only fails if the flag isn't defined; defined directly above

	return cmd
}

// resolveSearchToken returns a bearer scoped to the search service host.
// In split-host deployments this triggers an RFC 8693 exchange so the bearer
// carries the data-API audience rather than the auth-host one; single-host
// setups hit the same-host shortcut and return the core token unchanged.
// insecureHTTPAuth opts into non-loopback http:// resources at the
// tokenmanager layer, matching the per-command --insecure-http-auth pattern
// used by NewAuthenticatedAPIClient and newRecapClient.
func resolveSearchToken(ctx context.Context, serviceURL string, insecureHTTPAuth bool) (string, error) {
	if insecureHTTPAuth {
		auth.EnableInsecureHTTP()
	}
	token, err := auth.ResolveDataAPIToken(ctx, serviceURL)
	if errors.Is(err, auth.ErrNotLoggedIn) {
		return "", errors.New("not authenticated. Run 'entire login' to authenticate")
	}
	if err != nil {
		return "", fmt.Errorf("reading credentials: %w", err)
	}
	return token, nil
}

// completeRepoFlag returns shell-completion suggestions for the search
// command's --repo flag. "*" is always offered so the wildcard works
// regardless of auth state. Errors are swallowed (rather than surfaced via
// ShellCompDirectiveError) because completion runs on every TAB press and
// must never pollute the user's prompt with error output.
func completeRepoFlag(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	suggestions := []string{"*"}
	client, err := NewAuthenticatedAPIClient(cmd.Context(), false)
	if err != nil {
		return suggestions, cobra.ShellCompDirectiveNoFileComp
	}
	repos, err := client.ListRepositories(cmd.Context(), api.RepositorySortRecent)
	if err != nil {
		return suggestions, cobra.ShellCompDirectiveNoFileComp
	}
	for _, r := range repos {
		if r.CheckpointCount == 0 {
			continue // searching a repo with no checkpoints would always be empty
		}
		suggestions = append(suggestions, r.FullName)
	}
	return suggestions, cobra.ShellCompDirectiveNoFileComp
}

// codeSearchEnabled reports whether the code search feature is gated on.
func codeSearchEnabled() bool {
	return os.Getenv("ENTIRE_CODE_SEARCH") == "1"
}

type codeSearchOpts struct {
	query           string
	repoFilters     []string
	resolvedRepoIDs []string // ULIDs resolved from repoFilters via repo index
	limit           int
	caseSensitive   bool
	jsonOutput      bool
	insecureHTTP    bool

	// Test seams — nil in production.
	searchCellFn func(ctx context.Context, opts codeSearchOpts, cg cellGroup) (*codesearch.SearchResponse, error)
}

// codeSearchCoreClient is the control-plane surface searchAllCells needs.
// An interface so the fan-out is unit-testable against a fake control plane.
type codeSearchCoreClient interface {
	ListRepos(ctx context.Context) (*coreapi.ListReposOutputBody, error)
	ListClusters(ctx context.Context) (*coreapi.ListClustersOutputBody, error)
}

// newCodeSearchCoreClient builds the control-plane client. Swapped in tests.
var newCodeSearchCoreClient = func() (codeSearchCoreClient, error) { return coreapi.New() }

// extractInlineRepoFilters extracts only repo: prefixed filters from a query
// string, returning the remaining query text and the list of repo values.
// Unlike search.ParseSearchInput, this does NOT consume author:, date:, or
// branch: tokens — those are checkpoint-search-only and should be treated as
// literal text in code search queries.
func extractInlineRepoFilters(query string) (remaining string, repos []string) {
	var kept []string
	for _, part := range strings.Fields(query) {
		if strings.HasPrefix(part, "repo:") {
			if v := part[5:]; v != "" {
				repos = append(repos, v)
			}
		} else {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " "), repos
}

// codeSearchCellTimeout bounds each per-cell search call (token exchange + API).
const codeSearchCellTimeout = 30 * time.Second

// runCodeSearch handles the --code flag path: search code content via peregrine.
//
// When a repo filter is specified, it routes to that repo's owning cell.
// Without a filter, it fans out across all cells that host the user's repos
// (mirroring the BFF's /api/v1/stream endpoint): list repos from the control
// plane, group by cell/jurisdiction, search each cell in parallel, merge.
func runCodeSearch(ctx context.Context, cmd *cobra.Command, opts codeSearchOpts) error {
	if !codeSearchEnabled() {
		return errors.New("code search is not yet available")
	}

	if opts.query == "" {
		return errors.New("query required for code search. Usage: entire search --code <query>")
	}

	w := cmd.OutOrStdout()

	// Always fan out via searchAllCells — it fetches the repo index,
	// resolves slugs to ULIDs, and handles single- vs multi-jurisdiction.
	resp, err := searchAllCells(ctx, opts)
	if err != nil {
		return err
	}

	isTerminal := interactive.IsTerminalWriter(w)
	if opts.jsonOutput || !isTerminal {
		return writeCodeSearchJSON(w, resp)
	}

	writeCodeSearchText(w, resp)
	return nil
}

// cellGroup groups repos by cell for fan-out, matching the BFF's per-cell
// search pattern. Each cell has its own peregrine instance, so we search
// each cell independently. The jurisdiction is kept for token minting.
type cellGroup struct {
	cell         string   // cell identifier from RepoIndexEntry.Cell (grouping key, matches BFF)
	jurisdiction string   // used for jurisdictional token exchange
	baseURL      string   // cell's apiUrl from the cluster catalog (empty → home-cell fallback)
	repoIDs      []string // repo ULIDs that belong to this cell (set when filtering)
}

// searchAllCells fans out code search across all cells that host the user's
// repos, mirroring the BFF's per-cell search pattern:
//  1. List repos from the control plane (entire-core) to discover cells
//  2. Group by cell (one search per cell, matching the BFF)
//  3. Resolve each cell's apiUrl from the cluster catalog
//  4. Search each cell in parallel with per-cell timeouts
//  5. Merge results (sorted by score, capped to limit)
func searchAllCells(ctx context.Context, opts codeSearchOpts) (*codesearch.SearchResponse, error) {
	// Step 1: Get repos index from the control plane.
	coreClient, err := newCodeSearchCoreClient()
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return nil, errors.New("not authenticated. Run 'entire login' to authenticate")
		}
		return nil, fmt.Errorf("resolving control-plane client: %w", err)
	}

	reposCtx, reposCancel := context.WithTimeout(ctx, 10*time.Second)
	defer reposCancel()

	repoIndex, err := coreClient.ListRepos(reposCtx)
	if err != nil {
		return nil, fmt.Errorf("listing repos for cell discovery: %w", err)
	}

	if repoIndex.Truncated {
		logging.Warn(ctx, "repo index truncated; code search results may be incomplete")
	}

	// Step 2: Resolve repo slug filters to ULIDs and narrow to matching cells.
	indexRepos := repoIndex.Repos
	if len(opts.repoFilters) > 0 {
		resolved, filtered := resolveRepoFilters(opts.repoFilters, repoIndex.Repos)
		if len(resolved) == 0 {
			hint := ""
			if repoIndex.Truncated {
				hint = " (repo index was truncated — the repo may exist but was not included)"
			}
			return nil, fmt.Errorf("no matching repositories found for filter %q%s", opts.repoFilters, hint)
		}
		opts.resolvedRepoIDs = resolved
		indexRepos = filtered
	}

	// Step 3: Group repos by cell (one search per cell, matching the BFF).
	cells := groupReposByCell(indexRepos)
	if len(cells) == 0 {
		return &codesearch.SearchResponse{}, nil
	}

	// Step 3b: Resolve each cell's apiUrl from the cluster catalog so we
	// can route directly to the cell rather than relying on jurisdiction
	// fallback. Best-effort: if the listing fails, cells without a baseURL
	// will fall back to jurisdiction-based routing in searchCell.
	clusterCtx, clusterCancel := context.WithTimeout(ctx, 10*time.Second)
	defer clusterCancel()
	clusters, clusterErr := coreClient.ListClusters(clusterCtx)
	if clusterErr != nil {
		logging.Warn(ctx, "could not list clusters for cell URL resolution; falling back to jurisdiction routing",
			"error", clusterErr.Error())
	} else {
		slugToCluster := make(map[string]coreapi.Cluster, len(clusters.Clusters))
		for _, cl := range clusters.Clusters {
			slugToCluster[strings.ToLower(cl.Slug)] = cl
		}
		for i := range cells {
			if cl, ok := slugToCluster[cells[i].cell]; ok {
				cells[i].baseURL = strings.TrimRight(strings.TrimSpace(cl.ApiUrl.Or("")), "/")
			}
		}
	}

	// Step 4: Search each cell in parallel (or serially for a single cell).
	// Every path goes through mergeSearchResults for uniform sort/dedup/truncate.
	doSearch := searchCell
	if opts.searchCellFn != nil {
		doSearch = opts.searchCellFn
	}
	results := make([]codeSearchCellResult, len(cells))
	if len(cells) == 1 {
		resp, err := doSearch(ctx, opts, cells[0])
		results[0] = codeSearchCellResult{resp: resp, err: err}
	} else {
		var wg sync.WaitGroup
		for i, cg := range cells {
			wg.Add(1)
			go func(idx int, cg cellGroup) {
				defer wg.Done()
				resp, err := doSearch(ctx, opts, cg)
				results[idx] = codeSearchCellResult{resp: resp, err: err}
			}(i, cg)
		}
		wg.Wait()
	}

	return mergeSearchResults(ctx, opts.limit, cells, results)
}

// groupReposByCell groups repos by cell, returning one entry per distinct cell.
// This matches the BFF pattern where peregrine runs per-cell.
func groupReposByCell(repos []coreapi.RepoIndexEntry) []cellGroup {
	idx := make(map[string]int) // cell → index in groups
	var groups []cellGroup
	for _, r := range repos {
		cell := strings.ToLower(strings.TrimSpace(r.Cell))
		if i, ok := idx[cell]; ok {
			groups[i].repoIDs = append(groups[i].repoIDs, r.ID)
			continue
		}
		idx[cell] = len(groups)
		j := strings.ToLower(strings.TrimSpace(r.Jurisdiction))
		groups = append(groups, cellGroup{
			cell:         cell,
			jurisdiction: j,
			repoIDs:      []string{r.ID},
		})
	}
	return groups
}

// resolveRepoFilters matches user-provided filters against the repo index,
// returning the ULID list for peregrine and the subset of index entries whose
// repos matched (for cell grouping).
//
// Matching mirrors the BFF (code-search.ts lines 315-319):
//
//	slug = filter starts with "gh/" ? strip prefix : filter unchanged
//	match = id === filter || full_name === slug || full_name === filter
//
// Accepted filter formats:
//   - ULID            — matched directly on repo ID (raw filter)
//   - gh/owner/repo   — GitHub repo, stripped to owner/repo for FullName match
//   - owner/repo      — bare slug, matched on FullName directly
func resolveRepoFilters(filters []string, repos []coreapi.RepoIndexEntry) (repoIDs []string, matched []coreapi.RepoIndexEntry) {
	byName := make(map[string]coreapi.RepoIndexEntry, len(repos))
	byID := make(map[string]coreapi.RepoIndexEntry, len(repos))
	for _, r := range repos {
		byName[r.FullName] = r
		byID[r.ID] = r
	}
	seen := make(map[string]bool) // dedup by ID
	for _, f := range filters {
		// BFF only strips gh/ prefix; other prefixes are left as-is.
		slug := f
		if strings.HasPrefix(f, "gh/") {
			slug = f[3:]
		}

		// Match order mirrors the BFF: id === filter || full_name === slug || full_name === filter
		var r coreapi.RepoIndexEntry
		var ok bool
		if r, ok = byID[f]; !ok {
			if r, ok = byName[slug]; !ok {
				r, ok = byName[f]
			}
		}
		if ok && !seen[r.ID] {
			repoIDs = append(repoIDs, r.ID)
			matched = append(matched, r)
			seen[r.ID] = true
		}
	}
	return repoIDs, matched
}

// searchCell searches a single cell, using auth.NewEntireAPICellClient with
// an explicit CellTarget. When baseURL is available (resolved from the cluster
// catalog), it routes directly to the cell; otherwise falls back to
// jurisdiction-based routing.
func searchCell(ctx context.Context, opts codeSearchOpts, cg cellGroup) (*codesearch.SearchResponse, error) {
	cellCtx, cancel := context.WithTimeout(ctx, codeSearchCellTimeout)
	defer cancel()

	var target *auth.CellTarget
	label := cg.cell
	if label == "" {
		label = cg.jurisdiction
	}
	if label == "" {
		label = "home"
	}
	switch {
	case cg.baseURL != "":
		target = &auth.CellTarget{BaseURL: cg.baseURL, Jurisdiction: cg.jurisdiction}
	case cg.jurisdiction != "":
		target = &auth.CellTarget{Jurisdiction: cg.jurisdiction}
	}
	client, err := auth.NewEntireAPICellClient(cellCtx, opts.insecureHTTP, target)
	if err != nil {
		return nil, fmt.Errorf("resolving cell client for %s: %w", label, err)
	}

	// Use per-cell repo IDs when filtering, so each cell only searches
	// repos that belong to it.
	var repoIDs []string
	if len(opts.resolvedRepoIDs) > 0 {
		repoIDs = cg.repoIDs
	}
	req := codesearch.SearchRequest{
		Query:         opts.query,
		Repos:         repoIDs,
		CaseSensitive: opts.caseSensitive,
	}
	if opts.limit > 0 {
		req.MaxResults = opts.limit
	}

	resp, err := codesearch.Search(cellCtx, client, req)
	if err != nil {
		return nil, fmt.Errorf("code search on %s: %w", label, err)
	}
	return resp, nil
}

// codeSearchCellResult holds the outcome of a single-cell search.
type codeSearchCellResult struct {
	resp *codesearch.SearchResponse
	err  error
}

// mergeSearchResults merges responses from multiple cells into one, combining
// results, stats, and repo_stats. Results are sorted by Score (descending) for
// global relevance ranking and truncated to limit. Individual cell errors are
// logged and skipped, but if ALL cells fail the error is surfaced.
func mergeSearchResults(ctx context.Context, limit int, cells []cellGroup, results []codeSearchCellResult) (*codesearch.SearchResponse, error) {
	merged := &codesearch.SearchResponse{}
	var lastErr error
	successCount := 0
	for _, r := range results {
		if r.err != nil {
			lastErr = r.err
			continue
		}
		if r.resp == nil {
			continue
		}
		successCount++
		merged.Results = append(merged.Results, r.resp.Results...)
		merged.RepoStats = append(merged.RepoStats, r.resp.RepoStats...)
		merged.Stats.TotalMatches += r.resp.Stats.TotalMatches
		merged.Stats.TotalFiles += r.resp.Stats.TotalFiles
		merged.Stats.ReposSearched += r.resp.Stats.ReposSearched
		if r.resp.Stats.DurationMs > merged.Stats.DurationMs {
			merged.Stats.DurationMs = r.resp.Stats.DurationMs // wall-clock = slowest cell
		}
		if merged.Query == "" {
			merged.Query = r.resp.Query
		}
	}

	if successCount == 0 && lastErr != nil {
		return nil, fmt.Errorf("code search failed: %w", lastErr)
	}

	// Track partial failures so consumers (especially --json) can see them.
	// Use cell name for labeling when available, fall back to jurisdiction.
	var failedJurisdictions []string
	for i, r := range results {
		if r.err == nil {
			continue
		}
		label := "home"
		if i < len(cells) {
			if cells[i].cell != "" {
				label = cells[i].cell
			} else if cells[i].jurisdiction != "" {
				label = cells[i].jurisdiction
			}
		}
		failedJurisdictions = append(failedJurisdictions, label)
	}
	if len(failedJurisdictions) > 0 {
		logging.Warn(ctx, "code search partial failure; results may be incomplete",
			"succeeded", successCount,
			"total", len(cells),
			"failed_cells", failedJurisdictions)
	}

	// Sort by score descending so results are globally ranked by relevance,
	// not grouped by whichever cell returned first. Stable sort with a
	// tiebreaker keeps --json output deterministic across runs.
	sort.SliceStable(merged.Results, func(i, j int) bool {
		a, b := merged.Results[i], merged.Results[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})

	// Deduplicate results that may appear from overlapping cells (e.g. a repo
	// with empty jurisdiction searched via both home and explicit cell).
	seen := make(map[string]bool, len(merged.Results))
	deduped := merged.Results[:0]
	for _, r := range merged.Results {
		key := r.Repo + "\x00" + r.Path + "\x00" + fmt.Sprintf("%d:%d", r.Line, r.Column)
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, r)
	}
	merged.Results = deduped

	// Deduplicate RepoStats by repo name, summing match/file counts.
	repoStatsMap := make(map[string]*codesearch.RepoStats, len(merged.RepoStats))
	var dedupedStats []codesearch.RepoStats
	for _, rs := range merged.RepoStats {
		if existing, ok := repoStatsMap[rs.Repo]; ok {
			existing.MatchCount += rs.MatchCount
			existing.FileCount += rs.FileCount
		} else {
			entry := rs // copy
			repoStatsMap[rs.Repo] = &entry
			dedupedStats = append(dedupedStats, entry)
		}
	}
	// Write back merged values.
	for i := range dedupedStats {
		if m, ok := repoStatsMap[dedupedStats[i].Repo]; ok {
			dedupedStats[i] = *m
		}
	}
	merged.RepoStats = dedupedStats

	// Stats are preserved as the sum of per-cell peregrine stats — they
	// reflect the true totals (including zero-match repos and per-cell
	// truncation), not just the deduped result slice.

	// Cap to the caller's requested limit.
	if limit > 0 && len(merged.Results) > limit {
		merged.Results = merged.Results[:limit]
	}

	// Surface partial failures in the response so JSON consumers can detect them.
	merged.FailedJurisdictions = failedJurisdictions

	return merged, nil
}

// writeCodeSearchJSON writes code search results as JSON.
func writeCodeSearchJSON(w io.Writer, resp *codesearch.SearchResponse) error {
	out := struct {
		Query               string                 `json:"query"`
		Results             []codesearch.Result    `json:"results"`
		Total               int                    `json:"total"`
		Stats               codesearch.Stats       `json:"stats"`
		RepoStats           []codesearch.RepoStats `json:"repo_stats,omitempty"`
		FailedJurisdictions []string               `json:"failed_jurisdictions,omitempty"`
	}{
		Query:               resp.Query,
		Results:             resp.Results,
		Total:               resp.Stats.TotalMatches,
		Stats:               resp.Stats,
		RepoStats:           resp.RepoStats,
		FailedJurisdictions: resp.FailedJurisdictions,
	}
	if out.Results == nil {
		out.Results = []codesearch.Result{}
	}
	data, err := jsonutil.MarshalIndentWithNewline(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling code search results: %w", err)
	}
	fmt.Fprint(w, string(data))
	return nil
}

// maxContextLineLen is the maximum number of characters to display for a
// context_line in grep-style text output. Lines longer than this are truncated
// with an ellipsis so that JSONL/minified files don't blow up the terminal.
const maxContextLineLen = 200

// writeCodeSearchText renders code search results in grep-style format.
func writeCodeSearchText(w io.Writer, resp *codesearch.SearchResponse) {
	if len(resp.Results) == 0 {
		if len(resp.FailedJurisdictions) > 0 {
			fmt.Fprintf(w, "No code search results found (some regions failed: %s)\n",
				strings.Join(resp.FailedJurisdictions, ", "))
		} else {
			fmt.Fprintln(w, "No code search results found.")
		}
		return
	}
	for _, r := range resp.Results {
		line := r.ContextLine
		runes := []rune(line)
		if len(runes) > maxContextLineLen {
			line = string(runes[:maxContextLineLen]) + "…"
		}
		fmt.Fprintf(w, "%s:%s:%d: %s\n", r.Repo, r.Path, r.Line, line)
	}
	fmt.Fprintf(w, "\n%d matches across %d files in %d repos (%.0fms)\n",
		resp.Stats.TotalMatches, resp.Stats.TotalFiles, resp.Stats.ReposSearched, resp.Stats.DurationMs)
	if len(resp.FailedJurisdictions) > 0 {
		fmt.Fprintf(w, "Warning: results may be incomplete (failed jurisdictions: %s)\n",
			strings.Join(resp.FailedJurisdictions, ", "))
	}
}

// writeSearchJSON writes client-side paginated search results as JSON.
func writeSearchJSON(w io.Writer, resp *search.Response, limit, page int) error {
	if limit <= 0 {
		limit = resultsPerPage
	}

	total := len(resp.Results)
	totalPages := (total + limit - 1) / limit
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 1 {
		page = 1
	}

	// Slice results for the requested page.
	start := (page - 1) * limit
	end := start + limit
	var pageResults []search.Result
	if start < total {
		if end > total {
			end = total
		}
		pageResults = resp.Results[start:end]
	}
	if pageResults == nil {
		pageResults = []search.Result{}
	}

	out := struct {
		Results    []search.Result    `json:"results"`
		Total      int                `json:"total"`
		Page       int                `json:"page"`
		TotalPages int                `json:"total_pages"`
		Limit      int                `json:"limit"`
		Counts     *search.TypeCounts `json:"counts,omitempty"`
	}{
		Results:    pageResults,
		Total:      total,
		Page:       page,
		TotalPages: totalPages,
		Limit:      limit,
		Counts:     resp.Counts,
	}
	data, err := jsonutil.MarshalIndentWithNewline(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling results: %w", err)
	}
	fmt.Fprint(w, string(data))
	return nil
}

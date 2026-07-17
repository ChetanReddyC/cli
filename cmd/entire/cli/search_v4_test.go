package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/search"
)

// --- helpers -----------------------------------------------------------------

func fptr(f float64) *float64 { return &f }
func iptr(i int) *int         { return &i }

// v4Ckpt builds a checkpoint result with the given id and ranking metadata.
// tier < 0 means "no tier field" (the ANN-only fallback shape).
func v4Ckpt(id string, tier int, meta search.Meta) search.Result {
	if tier >= 0 {
		meta.Tier = iptr(tier)
	}
	return search.Result{
		Type:       search.TypeCheckpoint,
		Meta:       meta,
		Checkpoint: &search.CheckpointResult{ID: id},
	}
}

func v4Commit(sha string, tier int, meta search.Meta) search.Result {
	if tier >= 0 {
		meta.Tier = iptr(tier)
	}
	return search.Result{
		Type:   search.TypeCommit,
		Meta:   meta,
		Commit: &search.CommitResult{CommitSHA: sha},
	}
}

func v4Repo(score float64) search.Result {
	return search.Result{Type: search.TypeRepo, Meta: search.Meta{Score: score}}
}

func v4CellOK(resp *search.Response) cellCallResult[*search.Response] {
	return cellCallResult[*search.Response]{value: resp}
}

func v4CellErr(err error) cellCallResult[*search.Response] {
	return cellCallResult[*search.Response]{err: err}
}

func v4ResultIDs(t *testing.T, results []search.Result) []string {
	t.Helper()
	ids := make([]string, len(results))
	for i := range results {
		if results[i].Type == search.TypeRepo {
			ids[i] = search.TypeRepo
			continue
		}
		ids[i] = results[i].ResultID()
	}
	return ids
}

// --- scope helpers -----------------------------------------------------------

func TestSemanticSearchUnfiltered(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  search.Config
		want bool
	}{
		{"all-repos flag", search.Config{AllRepos: true}, true},
		{"repo:* filter", search.Config{Repos: []string{search.AllReposFilter}}, true},
		{"explicit repo", search.Config{Repos: []string{"o/r"}}, false},
		{"current-repo default", search.Config{Owner: "o", Repo: "r"}, false},
		{"repo:* among others is not unfiltered", search.Config{Repos: []string{"o/r", search.AllReposFilter}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := semanticSearchUnfiltered(tt.cfg); got != tt.want {
				t.Errorf("semanticSearchUnfiltered(%+v) = %v, want %v", tt.cfg, got, tt.want)
			}
		})
	}
}

func TestSemanticSearchSlugs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  search.Config
		want []string
	}{
		{"explicit filters win over current repo", search.Config{Repos: []string{"a/b", "c/d"}, Owner: "o", Repo: "r"}, []string{"a/b", "c/d"}},
		{"current-repo default", search.Config{Owner: "o", Repo: "r"}, []string{"o/r"}},
		{"no scope", search.Config{}, nil},
		{"owner without repo is no scope", search.Config{Owner: "o"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := semanticSearchSlugs(tt.cfg)
			if len(got) != len(tt.want) {
				t.Fatalf("semanticSearchSlugs(%+v) = %v, want %v", tt.cfg, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("slug[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// --- merge: tier ordering ------------------------------------------------------

// TestMergeSemanticV4Responses_TierOrdering verifies the cross-cell interleave
// applies query-serve's own ordering: repos first, then tier-0 by BM25 desc,
// tier-1 by rerank score desc, then promoted tier-2 by ANN asc — regardless of
// which cell each result came from.
func TestMergeSemanticV4Responses_TierOrdering(t *testing.T) {
	t.Parallel()

	cellA := &search.Response{Results: []search.Result{
		v4Ckpt("a-t1-low", 1, search.Meta{Score: 0.5}),
		v4Ckpt("a-t0-high", 0, search.Meta{BM25Score: fptr(9.0)}),
		// tier-2 alongside upper tiers in the same cell → promoted.
		v4Ckpt("a-t2", 2, search.Meta{ANNScore: fptr(0.30)}),
	}, Total: 3}
	cellB := &search.Response{Results: []search.Result{
		v4Repo(0.9),
		v4Ckpt("b-t0-low", 0, search.Meta{BM25Score: fptr(3.0)}),
		v4Ckpt("b-t1-high", 1, search.Meta{Score: 0.8}),
		v4Ckpt("b-t2", 2, search.Meta{ANNScore: fptr(0.10)}),
	}, Total: 4}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, []cellCallResult[*search.Response]{
		v4CellOK(cellA), v4CellOK(cellB),
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{search.TypeRepo, "a-t0-high", "b-t0-low", "b-t1-high", "a-t1-low", "b-t2", "a-t2"}
	got := v4ResultIDs(t, resp.Results)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("merged order = %v, want %v", got, want)
	}
	if resp.Total != 7 {
		t.Errorf("total = %d, want 7", resp.Total)
	}
	if resp.Page != 1 {
		t.Errorf("page = %d, want 1", resp.Page)
	}
}

// TestMergeSemanticV4Responses_ANNFallback verifies that when no cell produced
// tier 0/1 results, the ANN-only tail is shown, ordered ANN asc and capped at
// mergedTier2Max. Results without a tier field count as the fallback tier.
func TestMergeSemanticV4Responses_ANNFallback(t *testing.T) {
	t.Parallel()

	var results []search.Result
	for i := range mergedTier2Max + 5 {
		results = append(results, v4Ckpt(
			"c-"+string(rune('a'+i)), -1,
			search.Meta{ANNScore: fptr(float64(i) / 100)},
		))
	}
	resp, err := mergeSemanticV4Responses(context.Background(), 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: results, Total: len(results)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != mergedTier2Max {
		t.Errorf("fallback results = %d, want capped at %d", len(resp.Results), mergedTier2Max)
	}
	// ANN asc: the lowest scores survive the cap, in ascending order.
	for i := 1; i < len(resp.Results); i++ {
		if *resp.Results[i-1].Meta.ANNScore > *resp.Results[i].Meta.ANNScore {
			t.Errorf("fallback not sorted ANN asc at %d", i)
		}
	}
}

// TestMergeSemanticV4Responses_FallbackDroppedWhenUpperTiersExist documents
// that a cell whose page is entirely tier-2 (its ANN fallback) contributes
// nothing when another cell produced tier 0/1 — matching how query-serve only
// shows the fallback tail when there is nothing better.
func TestMergeSemanticV4Responses_FallbackDroppedWhenUpperTiersExist(t *testing.T) {
	t.Parallel()

	upper := &search.Response{Results: []search.Result{
		v4Ckpt("good", 1, search.Meta{Score: 0.7}),
	}, Total: 1}
	fallbackOnly := &search.Response{Results: []search.Result{
		v4Ckpt("ann-only", 2, search.Meta{ANNScore: fptr(0.2)}),
	}, Total: 1}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, []cellCallResult[*search.Response]{
		v4CellOK(upper), v4CellOK(fallbackOnly),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := v4ResultIDs(t, resp.Results)
	if len(got) != 1 || got[0] != "good" {
		t.Errorf("results = %v, want only [good] (all-tier-2 cell's fallback dropped)", got)
	}
}

// --- merge: dedup ---------------------------------------------------------------

// TestMergeSemanticV4Responses_DedupAdjustsTotalsAndCounts verifies a result
// mirrored across cells is kept once (first/higher-ranked wins) and that both
// the total and the per-type counts are reduced accordingly.
func TestMergeSemanticV4Responses_DedupAdjustsTotalsAndCounts(t *testing.T) {
	t.Parallel()

	cellA := &search.Response{
		Results: []search.Result{
			v4Ckpt("dup", 1, search.Meta{Score: 0.9}),
			v4Commit("sha1", 1, search.Meta{Score: 0.6}),
		},
		Total:  2,
		Counts: &search.TypeCounts{Checkpoints: 1, Commits: 1},
	}
	cellB := &search.Response{
		Results: []search.Result{
			v4Ckpt("dup", 1, search.Meta{Score: 0.4}),
		},
		Total:  1,
		Counts: &search.TypeCounts{Checkpoints: 1},
	}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, []cellCallResult[*search.Response]{
		v4CellOK(cellA), v4CellOK(cellB),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := v4ResultIDs(t, resp.Results)
	want := []string{"dup", "sha1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("results = %v, want %v", got, want)
	}
	// The kept "dup" is cell A's higher-ranked copy.
	if resp.Results[0].Meta.Score != 0.9 {
		t.Errorf("kept dup score = %v, want the first/higher-ranked 0.9", resp.Results[0].Meta.Score)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2 (3 rows - 1 dupe)", resp.Total)
	}
	if resp.Counts.Checkpoints != 1 || resp.Counts.Commits != 1 {
		t.Errorf("counts = %+v, want checkpoints=1 commits=1", resp.Counts)
	}
}

// TestMergeSemanticV4Responses_SameIDDifferentTypeNotDeduped guards the dedup
// key: a commit and a checkpoint sharing an id string are distinct results.
func TestMergeSemanticV4Responses_SameIDDifferentTypeNotDeduped(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("x", 1, search.Meta{Score: 0.9}),
			v4Commit("x", 1, search.Meta{Score: 0.8}),
		}, Total: 2}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Errorf("results = %d, want 2 (same id, different types)", len(resp.Results))
	}
}

// --- merge: failures, limits, empties -------------------------------------------

func TestMergeSemanticV4Responses_PartialFailureMergesSurvivors(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, []cellCallResult[*search.Response]{
		v4CellErr(errors.New("cell down")),
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("ok", 1, search.Meta{Score: 0.5}),
		}, Total: 1}),
	})
	if err != nil {
		t.Fatalf("partial failure should merge survivors, got error: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].ResultID() != "ok" {
		t.Errorf("results = %v, want [ok]", v4ResultIDs(t, resp.Results))
	}
}

func TestMergeSemanticV4Responses_AllCellsFail(t *testing.T) {
	t.Parallel()

	_, err := mergeSemanticV4Responses(context.Background(), 0, []cellCallResult[*search.Response]{
		v4CellErr(errors.New("cell a down")),
		v4CellErr(errors.New("cell b down")),
	})
	if err == nil {
		t.Fatal("expected an error when every cell failed")
	}
	if !strings.Contains(err.Error(), "semantic-search-v4") {
		t.Errorf("error = %q, want it labeled semantic-search-v4", err.Error())
	}
}

func TestMergeSemanticV4Responses_NoCells(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Results == nil || len(resp.Results) != 0 {
		t.Errorf("results = %v, want non-nil empty slice", resp.Results)
	}
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
}

func TestMergeSemanticV4Responses_LimitCapsResults(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 2, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("a", 1, search.Meta{Score: 0.9}),
			v4Ckpt("b", 1, search.Meta{Score: 0.8}),
			v4Ckpt("c", 1, search.Meta{Score: 0.7}),
		}, Total: 3}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Errorf("results = %d, want capped at limit 2", len(resp.Results))
	}
	if resp.Total != 3 {
		t.Errorf("total = %d, want 3 (limit caps the page, not the total)", resp.Total)
	}
}

func TestMergeSemanticV4Responses_NilCountsBodiesTolerated(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("a", 1, search.Meta{Score: 0.9}),
		}, Total: 1}), // no Counts
		v4CellOK(&search.Response{Results: []search.Result{
			v4Commit("sha", 1, search.Meta{Score: 0.5}),
		}, Total: 1, Counts: &search.TypeCounts{Commits: 1}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Counts == nil || resp.Counts.Commits != 1 || resp.Counts.Checkpoints != 0 {
		t.Errorf("counts = %+v, want commits=1 from the one counted body", resp.Counts)
	}
}

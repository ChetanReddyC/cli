package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// TestSearch_V4URLConstruction verifies the v4 branch hits the query-serve
// route on the cell gateway, sends the repo ULID (not the owner/name slug),
// carries the identity token, forwards every filter param, and — like v3 —
// never sends types.
func TestSearch_V4URLConstruction(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("id-token-123", srv.URL)
	_, err := Search(context.Background(), Config{
		UseV4:      true,
		CellClient: client,
		RepoID:     "01JREPOULID",
		V4RepoSlug: "myowner/myrepo",
		Owner:      "myowner",
		Repo:       "myrepo",
		Query:      "find bugs",
		Limit:      10,
		Author:     "alice",
		Date:       "week",
		Branch:     "main",
		Page:       2,
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Path != v4ServicePath {
		t.Errorf("path = %s, want %s", capturedReq.URL.Path, v4ServicePath)
	}
	q := capturedReq.URL.Query()
	if q.Get("repo") != "01JREPOULID" {
		t.Errorf("repo = %s, want the ULID '01JREPOULID' (not the slug)", q.Get("repo"))
	}
	if q.Get("q") != "find bugs" {
		t.Errorf("q = %s, want 'find bugs'", q.Get("q"))
	}
	if q.Get("limit") != "10" {
		t.Errorf("limit = %s, want '10'", q.Get("limit"))
	}
	if q.Get("author") != "alice" {
		t.Errorf("author = %s, want 'alice'", q.Get("author"))
	}
	if q.Get("date") != "week" {
		t.Errorf("date = %s, want 'week'", q.Get("date"))
	}
	if q.Get("branch") != "main" {
		t.Errorf("branch = %s, want 'main'", q.Get("branch"))
	}
	if q.Get("page") != "2" {
		t.Errorf("page = %s, want '2'", q.Get("page"))
	}
	// types must NOT be set — the v4 route rejects it, and both backends
	// return all types.
	if q.Has("types") {
		t.Errorf("types param should not be set, got %q", q.Get("types"))
	}
	if capturedReq.Header.Get("Authorization") != "Bearer id-token-123" {
		t.Errorf("auth header = %s, want 'Bearer id-token-123'", capturedReq.Header.Get("Authorization"))
	}
}

// TestSearch_V4ResponseDecodesLikeV3 confirms the v4 response — which carries
// extra top-level fields (accessible_repos, fanout) the v3 worker doesn't —
// decodes into the same Response the JSON output shape depends on, dropping the
// unknown fields.
func TestSearch_V4ResponseDecodesLikeV3(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, `{
			"results": [
				{"type": "commit", "data": {"commitSha": "abc123", "org": "o", "repo": "r"}, "searchMeta": {"matchType": "both", "score": 1.5}}
			],
			"total": 1,
			"page": 1,
			"counts": {"repos": 0, "checkpoints": 0, "commits": 1, "prs": 0, "sessions": 0},
			"accessible_repos": ["01JREPOULID"],
			"fanout": {"namespaces": 1}
		}`)
	}))
	defer srv.Close()

	resp, err := Search(context.Background(), Config{
		UseV4:      true,
		CellClient: api.NewClientWithBaseURL("tok", srv.URL),
		RepoID:     "01JREPOULID",
		V4RepoSlug: "o/r",
		Owner:      "o",
		Repo:       "r",
		Query:      "q",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 || len(resp.Results) != 1 {
		t.Fatalf("total=%d results=%d, want 1/1", resp.Total, len(resp.Results))
	}
	if resp.Results[0].Type != TypeCommit || resp.Results[0].Commit == nil || resp.Results[0].Commit.CommitSHA != "abc123" {
		t.Errorf("commit result did not decode; got %+v", resp.Results[0])
	}
	if resp.Counts == nil || resp.Counts.Commits != 1 {
		t.Errorf("counts did not decode; got %+v", resp.Counts)
	}
}

// TestSearch_V4Routing exercises the dispatch: with a v4 backend wired, a
// current-repo search hits the cell, but a broadened scope (repo:* / all-repos
// / a different repo) falls back to the v3 host — proving useV4's per-request
// scope check keeps the wrong repo's ULID from reaching the cell.
func TestSearch_V4Routing(t *testing.T) {
	t.Parallel()

	newServer := func(hit *bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			*hit = true
			resp := Response{Results: []Result{}, Total: 0, Page: 1}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
		}))
	}

	tests := []struct {
		name      string
		mutate    func(c *Config)
		wantV4Hit bool
	}{
		{"default scope routes to v4", func(*Config) {}, true},
		{"explicit current repo routes to v4", func(c *Config) { c.Repos = []string{"o/r"} }, true},
		{"explicit current repo different case routes to v4", func(c *Config) { c.Repos = []string{"O/R"} }, true},
		{"all-repos falls back to v3", func(c *Config) { c.AllRepos = true }, false},
		{"repo:* falls back to v3", func(c *Config) { c.Repos = []string{AllReposFilter} }, false},
		{"different repo falls back to v3", func(c *Config) { c.Repos = []string{"other/repo"} }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var v3Hit, v4Hit bool
			v3 := newServer(&v3Hit)
			defer v3.Close()
			v4 := newServer(&v4Hit)
			defer v4.Close()

			cfg := Config{
				ServiceURL:  v3.URL,
				GitHubToken: "v3tok",
				Owner:       "o",
				Repo:        "r",
				Query:       "q",
				UseV4:       true,
				CellClient:  api.NewClientWithBaseURL("v4tok", v4.URL),
				RepoID:      "01JREPOULID",
				V4RepoSlug:  "o/r",
			}
			tt.mutate(&cfg)
			if _, err := Search(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			if v4Hit != tt.wantV4Hit {
				t.Errorf("v4 hit = %v, want %v", v4Hit, tt.wantV4Hit)
			}
			if v3Hit == tt.wantV4Hit {
				t.Errorf("v3 hit = %v, want %v", v3Hit, !tt.wantV4Hit)
			}
		})
	}
}

// TestConfig_useV4 unit-tests the gating predicate directly.
func TestConfig_useV4(t *testing.T) {
	t.Parallel()

	client := &api.Client{} // zero client is enough; useV4 only checks non-nil
	valid := Config{UseV4: true, CellClient: client, RepoID: "ULID", V4RepoSlug: "o/r", Owner: "o", Repo: "r"}

	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"fully wired, default scope", valid, true},
		{"flag off", func() Config { c := valid; c.UseV4 = false; return c }(), false},
		{"nil client", func() Config { c := valid; c.CellClient = nil; return c }(), false},
		{"no repo id", func() Config { c := valid; c.RepoID = ""; return c }(), false},
		{"all repos", func() Config { c := valid; c.AllRepos = true; return c }(), false},
		{"repo:* filter", func() Config { c := valid; c.Repos = []string{AllReposFilter}; return c }(), false},
		{"matching explicit repo", func() Config { c := valid; c.Repos = []string{"o/r"}; return c }(), true},
		{"mismatched explicit repo", func() Config { c := valid; c.Repos = []string{"x/y"}; return c }(), false},
		{"multiple repos", func() Config { c := valid; c.Repos = []string{"o/r", "x/y"}; return c }(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cfg.useV4(); got != tt.want {
				t.Errorf("useV4() = %v, want %v", got, tt.want)
			}
		})
	}
}

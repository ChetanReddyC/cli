package checkpoint

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/redact"

	"github.com/stretchr/testify/require"
)

// jsonlRedactor is the production pipeline the in-memory paths use.
func jsonlRedactor(_ context.Context, b []byte) (redact.RedactedBytes, error) {
	return redact.JSONLBytes(b)
}

// TestRedactTranscriptIncremental_MatchesFullRedaction is the correctness core
// for the in-memory paths (condensation and the Stop finalize rewrite): growing a
// transcript across checkpoints must produce exactly what one whole-transcript
// pass produces.
func TestRedactTranscriptIncremental_MatchesFullRedaction(t *testing.T) {
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	const session = "sess-abc"

	content := padPastCacheThreshold(t, transcriptLines(0, 100))

	for round := range 5 {
		if round > 0 {
			content += transcriptLines(round*10_000, 50)
		}
		got, err := RedactTranscriptIncremental(
			ctx, repo, CommittedScope, session, []byte(content), jsonlRedactor)
		require.NoError(t, err)

		want, wantErr := redact.JSONLBytes([]byte(content))
		require.NoError(t, wantErr)
		require.Equal(t, string(want.Bytes()), string(got.Bytes()),
			"round %d: incremental output must equal a whole-transcript redaction", round)
	}
}

// TestRedactTranscriptIncremental_ActuallyReusesPrefix proves the fast path is
// engaged rather than silently falling back — otherwise the test above would
// pass just as happily with the cache doing nothing.
func TestRedactTranscriptIncremental_ActuallyReusesPrefix(t *testing.T) {
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	const session = "sess-reuse"

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	_, err := RedactTranscriptIncremental(ctx, repo, CommittedScope, session, []byte(content), jsonlRedactor)
	require.NoError(t, err)

	// Second round: count the bytes the redactor is handed. With reuse it sees
	// only the appended suffix, never the whole transcript.
	appended := transcriptLines(50_000, 40)
	grown := content + appended

	var sawBytes int
	counting := func(_ context.Context, b []byte) (redact.RedactedBytes, error) {
		sawBytes += len(b)
		return redact.JSONLBytes(b)
	}
	got, err := RedactTranscriptIncremental(ctx, repo, CommittedScope, session, []byte(grown), counting)
	require.NoError(t, err)

	require.Equal(t, len(appended), sawBytes,
		"only the appended suffix should be redacted, not the whole %d-byte transcript", len(grown))

	want, wantErr := redact.JSONLBytes([]byte(grown))
	require.NoError(t, wantErr)
	require.Equal(t, string(want.Bytes()), string(got.Bytes()))
}

// TestRedactTranscriptIncremental_ScopesAreIsolated pins the namespacing. The
// shadow write stores sanitized bytes while condensation stores sanitized and
// image-externalized ones, so a prefix cached under one scope must never be
// offered to the other.
func TestRedactTranscriptIncremental_ScopesAreIsolated(t *testing.T) {
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	const session = "sess-scope"

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	_, err := RedactTranscriptIncremental(ctx, repo, CommittedScope, session, []byte(content), jsonlRedactor)
	require.NoError(t, err)

	// A different scope, same session and content: must redact in full.
	grown := content + transcriptLines(70_000, 30)
	var sawBytes int
	counting := func(_ context.Context, b []byte) (redact.RedactedBytes, error) {
		sawBytes += len(b)
		return redact.JSONLBytes(b)
	}
	_, err = RedactTranscriptIncremental(ctx, repo, ShadowScope, session, []byte(grown), counting)
	require.NoError(t, err)
	require.Equal(t, len(grown), sawBytes,
		"a prefix cached under CommittedScope must not be reused for ShadowScope")

	require.NotEqual(t, transcriptCacheKey(CommittedScope, session), transcriptCacheKey(ShadowScope, session))
}

// TestRedactTranscriptIncremental_SessionsAreIsolated: two concurrent sessions in
// one worktree must not share a prefix.
func TestRedactTranscriptIncremental_SessionsAreIsolated(t *testing.T) {
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()

	contentA := padPastCacheThreshold(t, transcriptLines(0, 100))
	_, err := RedactTranscriptIncremental(ctx, repo, CommittedScope, "sess-A", []byte(contentA), jsonlRedactor)
	require.NoError(t, err)

	contentB := padPastCacheThreshold(t, transcriptLines(500_000, 100))
	var sawBytes int
	counting := func(_ context.Context, b []byte) (redact.RedactedBytes, error) {
		sawBytes += len(b)
		return redact.JSONLBytes(b)
	}
	got, err := RedactTranscriptIncremental(ctx, repo, CommittedScope, "sess-B", []byte(contentB), counting)
	require.NoError(t, err)
	require.Equal(t, len(contentB), sawBytes, "session B must not reuse session A's prefix")

	want, wantErr := redact.JSONLBytes([]byte(contentB))
	require.NoError(t, wantErr)
	require.Equal(t, string(want.Bytes()), string(got.Bytes()))
}

// TestRedactTranscriptIncremental_OptsOutWithoutRepoOrSession covers the callers
// that deliberately decline caching (the per-subagent transcript passes a nil
// repo). Output must still be correct.
func TestRedactTranscriptIncremental_OptsOutWithoutRepoOrSession(t *testing.T) {
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	want, wantErr := redact.JSONLBytes([]byte(content))
	require.NoError(t, wantErr)

	t.Run("nil repo", func(t *testing.T) {
		got, err := RedactTranscriptIncremental(ctx, nil, CommittedScope, "s1", []byte(content), jsonlRedactor)
		require.NoError(t, err)
		require.Equal(t, string(want.Bytes()), string(got.Bytes()))
	})

	t.Run("empty session", func(t *testing.T) {
		got, err := RedactTranscriptIncremental(ctx, repo, CommittedScope, "", []byte(content), jsonlRedactor)
		require.NoError(t, err)
		require.Equal(t, string(want.Bytes()), string(got.Bytes()))
	})
}

// TestRedactTranscriptIncremental_RedactorErrorPropagates: a degraded scanner
// must fail the write rather than store under-scanned content.
func TestRedactTranscriptIncremental_RedactorErrorPropagates(t *testing.T) {
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	content := padPastCacheThreshold(t, transcriptLines(0, 100))

	failing := func(context.Context, []byte) (redact.RedactedBytes, error) {
		return redact.RedactedBytes{}, redact.ErrScannerDegraded
	}
	_, err := RedactTranscriptIncremental(ctx, repo, CommittedScope, "sess-err", []byte(content), failing)
	require.ErrorIs(t, err, redact.ErrScannerDegraded)
}

// TestRedactTranscriptIncremental_SuffixErrorPropagates is the same guarantee on
// the reuse path: a failure redacting the appended lines must not silently ship
// the reused prefix alone.
func TestRedactTranscriptIncremental_SuffixErrorPropagates(t *testing.T) {
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	const session = "sess-suffix"

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	_, err := RedactTranscriptIncremental(ctx, repo, CommittedScope, session, []byte(content), jsonlRedactor)
	require.NoError(t, err)

	grown := content + transcriptLines(80_000, 10)
	failing := func(context.Context, []byte) (redact.RedactedBytes, error) {
		return redact.RedactedBytes{}, redact.ErrScannerDegraded
	}
	_, err = RedactTranscriptIncremental(ctx, repo, CommittedScope, session, []byte(grown), failing)
	require.ErrorIs(t, err, redact.ErrScannerDegraded)
}

// TestRedactTranscriptIncremental_SingleJSONValueNotSpliced guards the OpenCode
// shape: a single JSON object has no line structure, so splicing it would drop
// out of field-aware redaction into raw entropy detection over a fragment.
func TestRedactTranscriptIncremental_SingleJSONValueNotSpliced(t *testing.T) {
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()

	var b strings.Builder
	b.WriteString(`{"info":{"id":"x"},"messages":[`)
	for i := 0; b.Len() < redactCacheMinBytes+1024; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"text":"token sk-live-abcdefghijklmnopqrstuv"}`)
	}
	b.WriteString("]}\n")
	content := b.String()

	_, err := RedactTranscriptIncremental(ctx, repo, CommittedScope, "sess-oc", []byte(content), jsonlRedactor)
	require.NoError(t, err)

	cache := repoRedactCache(ctx, repo)
	require.NotNil(t, cache)
	require.Nil(t, cache.load(transcriptCacheKey(CommittedScope, "sess-oc")),
		"a single JSON value must never be cached for splicing")
}

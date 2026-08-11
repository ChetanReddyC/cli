package cli

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetMetadataTree_ElectedRemoteReadBack: the configured read-back scenario
// — checkpoint_push_remote elects upstream, the metadata branch lives on
// upstream only, origin is configured but empty. Resume's metadata resolution
// must find the tree (and, since upstream is the elected remote, the local
// primary advances to its tip).
//
// Uses t.Chdir — not parallel.
func TestGetMetadataTree_ElectedRemoteReadBack(t *testing.T) {
	localDir, _, upstreamHash := metadataCandidatesFixture(t, false, true)

	tree, repo, err := getMetadataTree(context.Background())
	require.NoError(t, err, "resume must find metadata that lives only on the elected remote")
	defer repo.Close()

	_, err = tree.File("README.md")
	require.NoError(t, err, "the tree must be the metadata branch's tree")
	require.True(t, refExists(t, localDir, "refs/heads/"+paths.MetadataBranchName),
		"the elected remote's fetch advances the local primary")
	assert.Equal(t, upstreamHash, revParse(t, localDir, "refs/heads/"+paths.MetadataBranchName))
}

// TestGetMetadataTree_LegacyOriginServedWithoutLocalAdvance: the legacy
// scenario — data on origin only while the election picks upstream. Resume
// must fall back to origin and find the tree via the remote-tracking tier
// (the final tier of the chain, which must survive any restructure), and the
// local primary must NOT be advanced from origin (#1374-class confinement
// holds through the resume path).
//
// Uses t.Chdir — not parallel.
func TestGetMetadataTree_LegacyOriginServedWithoutLocalAdvance(t *testing.T) {
	localDir, _, _ := metadataCandidatesFixture(t, true, false)

	tree, repo, err := getMetadataTree(context.Background())
	require.NoError(t, err, "resume must find legacy metadata that lives only on origin")
	defer repo.Close()

	_, err = tree.File("README.md")
	require.NoError(t, err, "the tree must be the metadata branch's tree")
	assert.False(t, refExists(t, localDir, "refs/heads/"+paths.MetadataBranchName),
		"the legacy origin tier must never advance the local primary")
	require.True(t, refExists(t, localDir, "refs/remotes/origin/"+paths.MetadataBranchName),
		"the legacy data is served through origin's tracking ref")
}

// TestCheckRemoteMetadata_HintNamesElectedRemote: when nothing can be fetched,
// the pasteable hint must name the first read candidate (the elected remote),
// not the literal "origin".
//
// Uses t.Chdir — not parallel.
func TestCheckRemoteMetadata_HintNamesElectedRemote(t *testing.T) {
	_, _, _ = metadataCandidatesFixture(t, false, false)

	var errBuf bytes.Buffer
	ctx := context.Background()
	sessions, err := checkRemoteMetadata(ctx, io.Discard, &errBuf,
		id.MustCheckpointID("aabb11223344"), checkpoint.ResolveRefs(ctx))
	require.NoError(t, err)
	require.Nil(t, sessions)

	assert.Contains(t, errBuf.String(),
		"git fetch upstream "+paths.MetadataBranchName+":"+paths.MetadataBranchName,
		"the hint must name the elected remote, not literal origin")
	assert.NotContains(t, errBuf.String(), "git fetch origin")
}

// TestPromoteRemoteTrackingPrimary_LegacyOriginNeverPromotes: the promotion
// feeds SafelyAdvanceLocalRef, so it is confined to the elected remote — a
// stale origin tracking ref must not create/advance the local primary when
// the election picks upstream.
//
// Uses t.Chdir — not parallel.
func TestPromoteRemoteTrackingPrimary_LegacyOriginNeverPromotes(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, dir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, dir, "upstream")

	staleHash := revParse(t, dir, "HEAD")
	testutil.GitUpdateRef(t, dir, "refs/remotes/origin/"+paths.MetadataBranchName, staleHash)

	t.Chdir(dir)
	ctx := context.Background()
	repo, err := openRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	promoteRemoteTrackingPrimary(ctx, repo, checkpoint.ResolveRefs(ctx))

	assert.False(t, refExists(t, dir, "refs/heads/"+paths.MetadataBranchName),
		"origin's stale tracking ref must never promote into the local primary when the election picks upstream")
}

// TestPromoteRemoteTrackingPrimary_ElectedRemotePromotes: the control — the
// elected remote's tracking ref does promote into the local primary.
//
// Uses t.Chdir — not parallel.
func TestPromoteRemoteTrackingPrimary_ElectedRemotePromotes(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, dir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, dir, "upstream")

	headHash := revParse(t, dir, "HEAD")
	testutil.GitUpdateRef(t, dir, "refs/remotes/upstream/"+paths.MetadataBranchName, headHash)

	t.Chdir(dir)
	ctx := context.Background()
	repo, err := openRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	promoteRemoteTrackingPrimary(ctx, repo, checkpoint.ResolveRefs(ctx))

	require.True(t, refExists(t, dir, "refs/heads/"+paths.MetadataBranchName),
		"the elected remote's tracking ref promotes into the local primary")
	assert.Equal(t, headHash, revParse(t, dir, "refs/heads/"+paths.MetadataBranchName))
}

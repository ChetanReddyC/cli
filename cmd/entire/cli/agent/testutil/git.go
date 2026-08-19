package testutil

import (
	"testing"

	clitestutil "github.com/entireio/cli/cmd/entire/cli/testutil"
)

// InitRepo initializes an isolated repository for an agent-package test.
func InitRepo(t *testing.T, repoDir string) {
	t.Helper()
	clitestutil.InitRepo(t, repoDir)
}

// WriteFile writes a repository-relative test fixture.
func WriteFile(t *testing.T, repoDir, relativePath, content string) {
	t.Helper()
	clitestutil.WriteFile(t, repoDir, relativePath, content)
}

// GitAdd stages repository-relative fixture paths.
func GitAdd(t *testing.T, repoDir string, paths ...string) {
	t.Helper()
	clitestutil.GitAdd(t, repoDir, paths...)
}

// GitCommit creates a test commit with repository-local identity settings.
func GitCommit(t *testing.T, repoDir, message string) {
	t.Helper()
	clitestutil.GitCommit(t, repoDir, message)
}

// GitIsolatedEnv returns an environment isolated from the developer's Git config.
func GitIsolatedEnv() []string {
	return clitestutil.GitIsolatedEnv()
}

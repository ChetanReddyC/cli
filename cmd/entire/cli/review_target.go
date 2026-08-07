package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func prepareReviewTarget(ctx context.Context, out, errOut io.Writer, selector string) (string, error) {
	forge, owner, repo, err := resolveTrailRemote(ctx)
	if err != nil {
		return "", err
	}

	normalized, isURL, err := normalizeReviewTargetSelector(selector, forge, owner, repo)
	if err != nil {
		return "", err
	}
	branch := ""
	if !isURL && reviewTargetBranchExists(ctx, normalized) {
		branch = normalized
	} else {
		err = runAuthenticatedTrailAPI(ctx, errOut, false, "", func(ctx context.Context, client *api.Client) error {
			found, findErr := resolveTrailBySelector(ctx, client, forge, owner, repo, normalized, "")
			if findErr != nil {
				return findErr
			}
			branch = strings.TrimSpace(found.Branch)
			if branch == "" {
				return fmt.Errorf("%s has no branch to review", describeTrailRef(found))
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}

	worktreeRoot, err := checkoutReviewWorktree(ctx, io.Discard, errOut, branch)
	if err != nil {
		return "", err
	}
	if worktreeRoot == "" {
		return "", errors.New("review target checkout was cancelled")
	}
	fmt.Fprintf(out, "Reviewing branch %s in %s.\n", branch, worktreeRoot)
	return worktreeRoot, nil
}

func reviewTargetBranchExists(ctx context.Context, branch string) bool {
	if strings.TrimSpace(branch) == "" || ValidateBranchName(ctx, branch) != nil {
		return false
	}
	if exists, err := BranchExistsLocally(ctx, branch); err == nil && exists {
		return true
	}
	exists, err := BranchExistsOnRemote(ctx, branch)
	return err == nil && exists
}

func normalizeReviewTargetSelector(raw, forge, owner, repo string) (string, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, errors.New("review target cannot be empty")
	}
	if !strings.Contains(raw, "://") {
		return raw, false, nil
	}
	u, parseErr := url.Parse(raw)
	if parseErr != nil || u.Scheme == "" || u.Host == "" {
		return "", true, fmt.Errorf("invalid review target URL %q", raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", true, fmt.Errorf("unsupported review target URL scheme %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host != "entire.io" && !strings.HasSuffix(host, ".entire.io") {
		return "", true, errors.New("review target URL must be an Entire trail URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 5 || parts[3] != "trails" || strings.TrimSpace(parts[4]) == "" {
		return "", true, fmt.Errorf("invalid Entire trail URL %q", raw)
	}
	if parts[0] != forge || parts[1] != owner || parts[2] != repo {
		return "", true, fmt.Errorf("trail URL targets %s/%s/%s, but this clone is %s/%s/%s", parts[0], parts[1], parts[2], forge, owner, repo)
	}
	return parts[4], true, nil
}

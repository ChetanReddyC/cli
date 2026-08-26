package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

// classifySearchError maps a search failure to a coarse telemetry error class
// (ENT-1938). Classification uses typed errors only — never error message
// text. Sentinel checks run before status-code checks so a mapped failure
// (e.g. the gateway 404 behind search.ErrCellUnavailable) keeps its specific
// class.
func classifySearchError(err error) string {
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		return telemetry.SearchErrClassAuth
	case errors.Is(err, auth.ErrNoCellForJurisdiction):
		return telemetry.SearchErrClassCellSkip
	case errors.Is(err, search.ErrCellUnavailable), errors.Is(err, errNoRegionAvailable):
		return telemetry.SearchErrClassRegionUnavailable
	case errors.Is(err, search.ErrRepoFilterUnmatched), errors.Is(err, errNoRepoAvailable):
		return telemetry.SearchErrClassRepoUnavailable
	}

	var statusErr *search.HTTPStatusError
	if errors.As(err, &statusErr) {
		return classForHTTPStatus(statusErr.StatusCode)
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) {
		return classForHTTPStatus(httpErr.StatusCode)
	}

	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) {
		return telemetry.SearchErrClassNetwork
	}

	return telemetry.SearchErrClassOther
}

func classForHTTPStatus(status int) string {
	switch {
	case status >= http.StatusInternalServerError:
		return telemetry.SearchErrClassServer
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return telemetry.SearchErrClassAuth
	default:
		return telemetry.SearchErrClassHTTPOther
	}
}

// emitSearchOutcome reports one search request's outcome when telemetry is
// opted in (settings.Telemetry == true). Content-free: booleans, enums,
// counts, and durations only — never query text, results, or repo names.
// Best-effort and non-blocking; failures to load settings suppress the event.
func emitSearchOutcome(ctx context.Context, cmd *cobra.Command, mode string, resultCount int, duration time.Duration, err error) {
	s, loadErr := LoadEntireSettings(ctx)
	if loadErr != nil || !s.IsTelemetryEnabled() {
		return
	}

	outcome := telemetry.SearchOutcome{
		Command:    cmd.CommandPath(),
		Mode:       mode,
		Success:    err == nil,
		DurationMS: duration.Milliseconds(),
	}
	if err != nil {
		outcome.ErrorClass = classifySearchError(err)
	} else {
		outcome.ResultCount = resultCount
	}
	telemetry.TrackSearchOutcomeDetached(outcome, s.Enabled, versioninfo.Version)
}

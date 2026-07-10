package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/spf13/cobra"
)

// trailWatchersPath returns the watchers path, keyed by the trail's UUID (not
// its number, unlike the other trail subresources).
func trailWatchersPath(trailID string) string {
	return "/api/v1/trails/" + trailID + "/watchers"
}

func newTrailWatchersCmd() *cobra.Command {
	var branch string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "watchers [<trail>]",
		Short: "List live watchers of a trail",
		Long: `List the users currently watching a trail's live event stream.

Watchers are presence-based: they reflect who is connected right now, not a
persisted subscription list, and are reported as user IDs. Requires the
deployment to have trail-watch configured (otherwise the server returns 503).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := selectorFromArgs(args)
			if selector != "" && strings.TrimSpace(branch) != "" {
				return errors.New("pass a trail selector or --branch, not both")
			}
			repoOverride := trailRepoFlag(cmd)
			// Auth/not-logged-in messages go to stderr; stdout carries output only.
			return runAuthenticatedTrailAPI(cmd.Context(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), repoOverride, func(ctx context.Context, client *api.Client) error {
				forge, owner, repoName, err := resolveTrailRepoOrRemote(ctx, repoOverride)
				if err != nil {
					return err
				}
				found, err := resolveTrailBySelector(ctx, client, forge, owner, repoName, selector, branch)
				if err != nil {
					return err
				}
				if strings.TrimSpace(found.ID) == "" {
					return errors.New("trail has no id; cannot list watchers")
				}
				resp, err := client.Get(ctx, trailWatchersPath(found.ID))
				if err != nil {
					return fmt.Errorf("failed to list watchers: %w", err)
				}
				defer resp.Body.Close()
				if err := checkTrailResponse(resp); err != nil {
					return err
				}
				var out api.TrailWatchersResponse
				if err := api.DecodeJSON(resp, &out); err != nil {
					return fmt.Errorf("failed to decode watchers response: %w", err)
				}
				if jsonOut {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(out)
				}
				if len(out.Watchers) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "No one is currently watching this trail")
					return nil
				}
				for _, w := range out.Watchers {
					fmt.Fprintln(cmd.OutOrStdout(), w)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch of the trail (defaults to current); cannot be combined with a trail selector")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

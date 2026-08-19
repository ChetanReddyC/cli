package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// trailsCellClient exists so its three callers do not each re-derive which
// client-build failure is a definitive negative. Its contract is that err
// ALWAYS describes the client build — never a cache save — so a caller can log
// it without knowing which of the two it got; notOnboarded is the flag callers
// switch on.
func TestTrailsCellClient_Contract(t *testing.T) {
	sentinel := fmt.Errorf("resolve processing placement for acme/widget: %w", errRepoNotOnboarded)
	transient := errors.New("control plane unavailable")

	for _, tc := range []struct {
		name           string
		clientErr      error
		wantNotOnboard bool
		wantClient     bool
	}{
		{name: "client builds", wantClient: true},
		{name: "not onboarded is flagged", clientErr: sentinel, wantNotOnboard: true},
		{name: "any other failure is not flagged", clientErr: transient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := trailRefreshAPIClient
			trailRefreshAPIClient = func(context.Context, bool, string) (*api.Client, error) {
				if tc.clientErr != nil {
					return nil, tc.clientErr
				}
				return &api.Client{}, nil
			}
			t.Cleanup(func() { trailRefreshAPIClient = previous })

			client, notOnboarded, err := trailsCellClient(context.Background(), false, "acme/widget")

			if notOnboarded != tc.wantNotOnboard {
				t.Errorf("notOnboarded = %v, want %v", notOnboarded, tc.wantNotOnboard)
			}
			if (client != nil) != tc.wantClient {
				t.Errorf("client != nil = %v, want %v", client != nil, tc.wantClient)
			}
			// err mirrors the client build in every branch, including the
			// not-onboarded one where the caller ignores it in favour of the flag.
			if tc.clientErr == nil {
				if err != nil {
					t.Errorf("err = %v, want nil when the client builds", err)
				}
				return
			}
			if !errors.Is(err, tc.clientErr) {
				t.Errorf("err = %v, want it to wrap the client-build failure %v", err, tc.clientErr)
			}
		})
	}
}

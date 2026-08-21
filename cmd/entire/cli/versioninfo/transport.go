package versioninfo

import (
	"net/http"

	"github.com/entireio/cli/internal/entireclient/httpclient"
)

// WrapTransport returns next wrapped so every request sent through it carries
// UserAgent(). A nil next means Go's default transport, matching how
// http.Client treats a nil Transport.
//
// Prefer this over setting the header at a call site whenever the *http.Client
// is handed to a helper that builds its own requests (clusterdiscovery's
// well-known fetch, coreapi's token exchange): those requests never pass
// through the caller's code, so only the transport can stamp them.
//
// Wrap innermost. When a client layers transports, this belongs at the base of
// the chain rather than on top: a transport above it may synthesize requests of
// its own and send them straight to its base, bypassing anything wrapped
// outside it. Stamping at the base catches every request that actually leaves
// the process. (httpclient.UserAgentTransport clones before mutating, so
// wrapping never disturbs the caller's request.)
func WrapTransport(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &httpclient.UserAgentTransport{Next: next, UA: UserAgent()}
}

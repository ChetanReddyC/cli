package versioninfo

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/entireio/cli/internal/entireclient/httpclient"
)

// uaRecorder is the terminal RoundTripper in a wrapped chain: it records the
// User-Agent each request carried by the time it would have hit the network.
// Mutex-guarded because a transport may be driven from more than one goroutine.
type uaRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *uaRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.seen = append(r.seen, req.Header.Get("User-Agent"))
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

func (r *uaRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func TestWrapTransport_StampsUserAgent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		callerU string // User-Agent the caller set, if any
	}{
		{name: "no user-agent set"},
		{name: "overwrites go default", callerU: "Go-http-client/2.0"},
		{name: "overwrites caller value", callerU: "something-else/1.2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &uaRecorder{}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://entire.io/api/v1/x", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.callerU != "" {
				req.Header.Set("User-Agent", tt.callerU)
			}

			resp, err := WrapTransport(rec).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			want := UserAgent()
			if got := rec.snapshot(); len(got) != 1 || got[0] != want {
				t.Fatalf("User-Agent sent = %v, want [%s]", got, want)
			}
			// The caller's own request must be left as it was: callers reuse
			// requests across retries.
			if got := req.Header.Get("User-Agent"); got != tt.callerU {
				t.Fatalf("caller request mutated: User-Agent = %q, want %q", got, tt.callerU)
			}
		})
	}
}

// A nil next means "Go's default transport", so callers can wrap a client that
// has no Transport of its own without reaching for http.DefaultTransport.
func TestWrapTransport_NilNextUsesDefaultTransport(t *testing.T) {
	t.Parallel()

	wrapped, ok := WrapTransport(nil).(*httpclient.UserAgentTransport)
	if !ok {
		t.Fatalf("WrapTransport(nil) = %T, want *httpclient.UserAgentTransport", WrapTransport(nil))
	}
	if wrapped.Next != http.DefaultTransport {
		t.Fatalf("Next = %v, want http.DefaultTransport", wrapped.Next)
	}
	if wrapped.UA != UserAgent() {
		t.Fatalf("UA = %q, want %q", wrapped.UA, UserAgent())
	}
}

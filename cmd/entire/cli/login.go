package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

const fallbackDeviceAuthPollInterval = time.Second
const defaultSlowDownBackoff = 5 * time.Second
const maxPollInterval = 30 * time.Second
const maxExpiresIn = 15 * time.Minute
const maxTransientErrors = 5

// browserLoginTimeout bounds how long the browser flow waits for the
// loopback redirect. The device flow is bounded by the AS's expires_in
// (capped at maxExpiresIn); without a bound here a closed browser tab
// would hang `entire login` forever.
const browserLoginTimeout = 5 * time.Minute

// browserOpenFunc is the signature for opening a URL in the user's browser.
type browserOpenFunc func(ctx context.Context, url string) error

// clipboardWriteFunc is the signature for copying a URL to the system
// clipboard. Keeping it injectable lets login tests remain parallel and avoids
// touching the developer's real clipboard.
type clipboardWriteFunc func(text string) error

type loginURLAction byte

const (
	loginURLContinue loginURLAction = iota
	loginURLCopy
	loginURLOpen
)

type loginURLActionReadFunc func(ctx context.Context) (loginURLAction, error)

// loginURLInteractor owns the three side effects behind the interactive URL
// prompt. Production uses the controlling TTY, system clipboard, and default
// browser; tests provide deterministic functions instead.
type loginURLInteractor struct {
	readAction loginURLActionReadFunc
	copyURL    clipboardWriteFunc
	openURL    browserOpenFunc
}

func defaultLoginURLInteractor() loginURLInteractor {
	return loginURLInteractor{
		readAction: readLoginURLAction,
		copyURL:    clipboard.WriteAll,
		openURL:    openBrowser,
	}
}

// chooseApprovalURL prefers verification_uri_complete (RFC 8628 §3.3.1) so the
// browser lands on a URL with the user_code already in the query string —
// most verification pages prefill the input from that param, sparing the
// user from typing. Falls back to the bare verification_uri when the AS
// didn't supply a complete form.
func chooseApprovalURL(start *auth.DeviceAuthStart) string {
	if start.VerificationURIComplete != "" {
		return start.VerificationURIComplete
	}
	return start.VerificationURI
}

// deviceAuthClient abstracts the auth client so runLogin and waitForApproval can be unit-tested.
type deviceAuthClient interface {
	StartDeviceAuth(ctx context.Context) (*auth.DeviceAuthStart, error)
	PollDeviceAuth(ctx context.Context, deviceCode string) (*auth.DeviceAuthPoll, error)
	BaseURL() string
}

// browserAuthFlow abstracts an in-progress loopback authorization-code
// login so runBrowserLogin can be unit-tested with a fake instead of a real
// listener. *auth.BrowserAuthFlow satisfies it.
type browserAuthFlow interface {
	AuthorizationURL() string
	Wait(ctx context.Context) (code string, err error)
	Exchange(ctx context.Context, code string) (accessToken, refreshToken string, err error)
	Close() error
}

func newLoginCmd() *cobra.Command {
	var (
		insecureHTTPAuth bool
		useDevice        bool
		server           string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to Entire",
		RunE: func(cmd *cobra.Command, _ []string) error {
			loginServer, err := parseLoginServer(server)
			if err != nil {
				return fmt.Errorf("invalid --server: %w", err)
			}
			if err := requireSecureLoginServer(loginServer, insecureHTTPAuth); err != nil {
				return err
			}
			client := auth.NewClient(loginServer, nil, insecureHTTPAuth)
			// Closure adapts the concrete *auth.BrowserAuthFlow result to the
			// browserAuthFlow interface (func types are invariant, so the
			// method value alone won't do). On error the flow is a typed nil,
			// which is fine — runLoginAuto checks err before touching it.
			startBrowser := func(ctx context.Context) (browserAuthFlow, error) {
				return client.StartBrowserAuth(ctx)
			}
			return runLoginAuto(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				client, startBrowser, defaultLoginURLInteractor(), loginFlowFacts{
					useDevice:  useDevice,
					canPrompt:  interactive.CanPromptInteractively(),
					sshSession: isSSHSession(),
				})
		},
	}
	cmd.Flags().StringVar(&server, "server", api.DefaultAuthBaseURL,
		"login server to authenticate against (rarely needed; the default serves all standard accounts)")
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)
	cmd.Flags().BoolVar(&useDevice, "device", false, "Use the device-code flow (enter a code in your browser) instead of the default browser redirect")
	return cmd
}

// parseLoginServer validates and canonicalises the --server value: an
// http(s) origin with nothing but scheme and host. Userinfo, path, query,
// and fragment are rejected rather than silently dropped — the value
// becomes the OAuth issuer, the token-exchange target, and the keyring
// key, so surprising rewrites would surface as confusing auth failures
// much later. A lone trailing slash is tolerated (normalised away).
func parseLoginServer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty server URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse server URL: %w", err)
	}
	// Error messages echo u.Redacted(), not raw: the URL may carry
	// userinfo (that's one of the rejection cases), and stderr often ends
	// up in CI logs where a password must not appear.
	switch {
	case u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP:
		return "", fmt.Errorf("scheme must be http or https, got %q", u.Redacted())
	case u.Host == "":
		return "", fmt.Errorf("missing host in %q", u.Redacted())
	case u.User != nil:
		return "", fmt.Errorf("userinfo not allowed in %q", u.Redacted())
	case u.Path != "" && u.Path != "/":
		return "", fmt.Errorf("path not allowed in %q (use the bare origin)", u.Redacted())
	case u.RawQuery != "" || u.Fragment != "":
		return "", fmt.Errorf("query/fragment not allowed in %q", u.Redacted())
	}
	return api.NormalizeOriginURL(raw), nil
}

// requireSecureLoginServer enforces TLS for the chosen login server — the
// only host login dials. --insecure-http-auth opts in to http:// (and
// enables it process-wide for the token save path).
func requireSecureLoginServer(server string, insecureHTTPAuth bool) error {
	if insecureHTTPAuth {
		auth.EnableInsecureHTTP()
		return nil
	}
	if err := api.RequireSecureURL(server); err != nil {
		return fmt.Errorf("login server check: %w", err)
	}
	return nil
}

func runLogin(ctx context.Context, outW, errW io.Writer, client deviceAuthClient, urlInteractor loginURLInteractor, canPrompt bool) error {
	start, err := client.StartDeviceAuth(ctx)
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}

	fmt.Fprintf(outW, "Device code: %s\n", start.UserCode)

	approvalURL := chooseApprovalURL(start)

	if canPrompt {
		if err := promptLoginURL(ctx, outW, errW, approvalURL, urlInteractor); err != nil {
			return fmt.Errorf("wait for input: %w", err)
		}
	} else {
		fmt.Fprintf(outW, "Login URL:   %s\n\n", approvalURL)
	}

	fmt.Fprint(outW, "Waiting for approval... ")

	token, refreshToken, err := waitForApproval(ctx, client, start.DeviceCode, start.ExpiresIn, time.Duration(start.Interval)*time.Second, defaultSlowDownBackoff)
	if err != nil {
		return fmt.Errorf("complete login: %w", err)
	}

	return persistLogin(outW, client.BaseURL(), token, refreshToken)
}

// loginFlowFacts carries the environment facts that pick between the
// browser and device-code flows. Detection happens once at the command
// entry point; the decision logic below only consumes these values.
type loginFlowFacts struct {
	useDevice  bool // --device flag
	canPrompt  bool // interactive terminal present
	sshSession bool // running inside an SSH session
}

// runLoginAuto picks between the browser (loopback authorization-code) and
// device-code flows and runs the chosen one. The browser flow is the
// default — no code to type, no poll latency — but it needs a browser that
// can reach this machine's 127.0.0.1, so headless terminals (CI, piped
// stdin), SSH sessions, and a loopback listener that fails to start all
// fall back to the device flow with a one-line explanation; the same
// both-flows-with-fallback shape gh / gcloud / aws sso ship. --device
// forces the device flow without commentary.
func runLoginAuto(ctx context.Context, outW, errW io.Writer, deviceClient deviceAuthClient, startBrowser func(context.Context) (browserAuthFlow, error), urlInteractor loginURLInteractor, facts loginFlowFacts) error {
	if shouldUseBrowserLogin(facts) {
		flow, err := startBrowser(ctx)
		if err != nil {
			// Binding the loopback listener can fail (sandboxing, firewall,
			// exhausted ports); that shouldn't strand the user — warn and
			// use the device flow instead.
			fmt.Fprintf(errW, "Warning: could not start browser sign-in (%v); falling back to the device-code flow.\n", err)
			return runLogin(ctx, outW, errW, deviceClient, urlInteractor, facts.canPrompt)
		}
		return runBrowserLogin(ctx, outW, errW, flow, deviceClient.BaseURL(), urlInteractor, browserLoginTimeout)
	}
	switch {
	case facts.useDevice:
		// Explicitly requested; no explanation needed.
	case !facts.canPrompt:
		fmt.Fprintln(errW, "No interactive terminal detected; using device-code flow.")
	case facts.sshSession:
		fmt.Fprintln(errW, "SSH session detected; using device-code flow (a browser opened here couldn't reach this machine).")
	}
	return runLogin(ctx, outW, errW, deviceClient, urlInteractor, facts.canPrompt)
}

// shouldUseBrowserLogin reports whether `entire login` should use the
// loopback authorization-code (browser) flow. The browser flow is the
// default but needs a local browser + reachable 127.0.0.1, so it's only
// chosen when --device wasn't passed, an interactive terminal is present,
// and we're not inside an SSH session (where the loopback listener binds
// on the remote host, out of the user's browser's reach); otherwise the
// caller falls back to the device flow.
func shouldUseBrowserLogin(f loginFlowFacts) bool {
	return !f.useDevice && f.canPrompt && !f.sshSession
}

// isSSHSession reports whether this process is running inside an SSH
// session: sshd sets SSH_CONNECTION/SSH_CLIENT for every session and
// SSH_TTY for interactive ones.
func isSSHSession() bool {
	return os.Getenv("SSH_CONNECTION") != "" ||
		os.Getenv("SSH_CLIENT") != "" ||
		os.Getenv("SSH_TTY") != ""
}

// runBrowserLogin runs the loopback authorization-code flow on an
// already-started flow: show the authorization URL and explicit actions, wait
// up to waitTimeout for the redirect back to the local listener, then exchange
// the code for tokens. Shares the token validation + persistence tail with
// runLogin via persistLogin.
func runBrowserLogin(ctx context.Context, outW, errW io.Writer, flow browserAuthFlow, baseURL string, urlInteractor loginURLInteractor, waitTimeout time.Duration) error {
	// Wait tears the listener down on return, but Close is idempotent and
	// covers the error paths before Wait runs.
	defer func() { _ = flow.Close() }()

	authURL := flow.AuthorizationURL()
	fmt.Fprintf(outW, "Logging in to: %s\n\n", baseURL)
	if err := promptLoginURL(ctx, outW, errW, authURL, urlInteractor); err != nil {
		return fmt.Errorf("wait for input: %w", err)
	}

	fmt.Fprint(outW, "Waiting for sign-in... ")

	// The clock starts after the user explicitly continues or successfully
	// opens the browser, so time spent choosing or copying isn't counted.
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()

	code, err := flow.Wait(waitCtx)
	if err != nil {
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out waiting for sign-in after %v; run `entire login` again, or use `entire login --device`", waitTimeout)
		}
		return fmt.Errorf("complete login: %w", err)
	}

	token, refreshToken, err := flow.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("complete login: %w", err)
	}

	return persistLogin(outW, baseURL, token, refreshToken)
}

// persistLogin validates the freshly-issued access token and records it in
// the shared contexts.json credential model. Shared by the device-code and
// browser flows.
func persistLogin(outW io.Writer, baseURL, token, refreshToken string) error {
	if err := validateReceivedToken(token, baseURL, time.Now()); err != nil {
		return fmt.Errorf("reject login token: %w", err)
	}

	// Record the login in the shared contexts.json credential model — the
	// single store every consumer (control plane, data API, git remote
	// helper, entiredb's CLIs) resolves against.
	if _, err := auth.RecordLoginContext(token, refreshToken, true); err != nil {
		return fmt.Errorf("save login: %w", withHeadlessStoreHint(err))
	}

	fmt.Fprintln(outW, "✓ Login complete.")
	return nil
}

// withHeadlessStoreHint appends file-token-store guidance to a credential
// store write failure. The default backend is the OS keyring, which locked
// or keyring-less machines (CI, containers, minimal server VMs) can't use —
// the raw store error gives those users no way forward (#1036). The hint is
// skipped when ENTIRE_TOKEN_STORE=file is already set (suggesting it again
// would be nonsense) and for failures the file store wouldn't help with.
func withHeadlessStoreHint(err error) error {
	if !errors.Is(err, auth.ErrCredentialStoreWrite) || tokenstore.FileBackendSelected() {
		return err
	}

	return fmt.Errorf("%w\n\nIf this machine has no usable OS keyring (headless server, container, CI), store tokens in a file instead:\n\n  %s=file entire login\n\nTokens are then written with 0600 permissions to %s (override the location with %s)",
		err, tokenstore.BackendEnvVar, tokenstore.FileBackendPath(), tokenstore.PathEnvVar)
}

// validateReceivedToken runs minimum-trust checks on the access token
// the AS handed us before we persist it. The server is the authority
// on signature/exp; this is defense in depth aimed at catching gross
// misbehaviour by a compromised or misconfigured AS (e.g. handing back
// a token from a different issuer than the one we asked, or one whose
// claims are already-expired).
//
// It also enforces what the contexts model needs up front:
// RecordLoginContext — the sole persistence path — keys the context and
// keychain slot on the token's iss and handle/sub claims, so a token
// without parseable claims can never complete a login. Rejecting it here
// names the requirement instead of surfacing a parse error from the save
// step. Entire-core always issues claim-bearing JWTs; opaque-token-only
// servers are not supported.
func validateReceivedToken(rawToken, issuerURL string, now time.Time) error {
	claims, err := tokens.ParseClaims(rawToken)
	if errors.Is(err, tokens.ErrUnsignedJWT) {
		return err //nolint:wrapcheck // sentinel surfaces verbatim for caller's errors.Is
	}
	if err != nil {
		return fmt.Errorf("login server issued a token without parseable JWT claims (claim-bearing JWTs are required): %w", err)
	}
	if claims.Issuer == "" {
		return errors.New("token has no iss claim; cannot record a login context")
	}
	if claims.Handle == "" && claims.Subject == "" {
		return errors.New("token has no handle or sub claim; cannot record a login context")
	}

	// iss check: the token must claim to come from the issuer we sent
	// the device-code request to. A mismatch means either the AS is
	// misconfigured or someone's playing games.
	if issErr := issMatches(claims.Issuer, issuerURL); issErr != nil {
		return issErr
	}

	// exp sanity: a token that's already expired before we even store
	// it is a smell. Don't reject if exp is unset (some servers omit).
	if !claims.ExpiresAt.IsZero() && !now.Before(claims.ExpiresAt) {
		return fmt.Errorf("token already expired (exp=%s, now=%s)",
			claims.ExpiresAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}

	return nil
}

// issMatches reports whether claimed equals expected after stripping path/
// query/fragment via api.OriginOnly, so "https://issuer/" and "https://issuer"
// match. The caller has already rejected an empty iss claim.
func issMatches(claimed, expected string) error {
	normClaimed := api.OriginOnly(claimed)
	normExpected := api.OriginOnly(expected)
	if normClaimed != normExpected {
		return fmt.Errorf("iss mismatch: token claims %q, expected %q", normClaimed, normExpected)
	}
	return nil
}

func waitForApproval(ctx context.Context, poller deviceAuthClient, deviceCode string, expiresIn int, interval, slowDownBackoff time.Duration) (accessToken, refreshToken string, err error) {
	expiry := time.Duration(expiresIn) * time.Second
	if expiry <= 0 || expiry > maxExpiresIn {
		expiry = maxExpiresIn
	}
	deadline := time.Now().Add(expiry)
	pollInterval := interval
	if pollInterval <= 0 {
		pollInterval = fallbackDeviceAuthPollInterval
	}

	consecutiveErrors := 0

	for {
		if time.Now().After(deadline) {
			return "", "", errors.New("device authorization expired")
		}

		result, err := poller.PollDeviceAuth(ctx, deviceCode)
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= maxTransientErrors {
				return "", "", fmt.Errorf("poll approval status (after %d consecutive failures): %w", consecutiveErrors, err)
			}
			// Transient error — wait and retry.
			select {
			case <-ctx.Done():
				return "", "", fmt.Errorf("wait for approval: %w", ctx.Err())
			case <-time.After(pollInterval):
			}
			continue
		}
		consecutiveErrors = 0

		switch result.Error {
		case "":
			if result.AccessToken == "" {
				return "", "", errors.New("device authorization completed without a token")
			}
			return result.AccessToken, result.RefreshToken, nil
		case "authorization_pending":
			// no-op, will sleep and retry below
		case "slow_down":
			pollInterval += slowDownBackoff
			if pollInterval > maxPollInterval {
				pollInterval = maxPollInterval
			}
		case "access_denied":
			return "", "", errors.New("device authorization denied")
		case "expired_token":
			return "", "", errors.New("device authorization expired")
		default:
			if result.ErrorDescription != "" {
				return "", "", fmt.Errorf("device authorization failed: %s: %s", result.Error, result.ErrorDescription)
			}
			return "", "", fmt.Errorf("device authorization failed: %s", result.Error)
		}

		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("wait for approval: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

const loginURLPrompt = "[Enter] Continue  [c] Copy URL  [o] Open in default browser"

// promptLoginURL displays the exact URL and waits for an explicit user choice.
// Copy keeps the prompt active so the user can paste into a chosen browser
// before deciding when the login timeout should begin.
func promptLoginURL(ctx context.Context, outW, errW io.Writer, loginURL string, interactor loginURLInteractor) error {
	fmt.Fprintf(outW, "Login URL:   %s\n\n", loginURL)

	for {
		fmt.Fprint(outW, loginURLPrompt)
		action, err := interactor.readAction(ctx)
		fmt.Fprintln(outW)
		if err != nil {
			return err
		}

		switch action {
		case loginURLContinue:
			return nil
		case loginURLCopy:
			if err := interactor.copyURL(loginURL); err != nil {
				fmt.Fprintf(errW, "Warning: failed to copy login URL: %v\n", err)
			} else {
				fmt.Fprintln(outW, "Copied login URL to clipboard.")
			}
		case loginURLOpen:
			if err := interactor.openURL(ctx, loginURL); err != nil {
				fmt.Fprintf(errW, "Warning: failed to open default browser: %v\n", err)
				continue
			}
			return nil
		}
	}
}

type loginURLActionResult struct {
	action loginURLAction
	err    error
}

// readLoginURLAction reads a single key from the controlling terminal without
// consuming piped stdin. Missing TTYs continue without opening anything,
// preserving the non-blocking fallback of the old Enter prompt.
func readLoginURLAction(ctx context.Context) (loginURLAction, error) {
	// In-process and forced-interactive subprocess tests must never touch a
	// developer's real terminal. Enter/continue is the side-effect-free default.
	if interactive.UnderTest() {
		return loginURLContinue, nil
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return loginURLContinue, nil //nolint:nilerr // no controlling TTY; continue without side effects
	}

	return readLoginURLActionFromTTY(ctx, tty)
}

// readLoginURLActionFromTTY takes ownership of tty, temporarily switches it to
// raw mode so c/o act immediately, and restores the original state before
// closing it on every success, error, and cancellation path.
func readLoginURLActionFromTTY(ctx context.Context, tty *os.File) (loginURLAction, error) {
	fd := int(tty.Fd()) //nolint:gosec // G115: uintptr->int is safe for a file descriptor
	state, err := term.MakeRaw(fd)
	if err != nil {
		_ = tty.Close()
		return loginURLContinue, fmt.Errorf("enable single-key input: %w", err)
	}

	resultCh := make(chan loginURLActionResult, 1)
	go func() {
		resultCh <- readLoginURLActionBytes(tty)
	}()

	finish := func() error {
		restoreErr := term.Restore(fd, state)
		closeErr := tty.Close()
		return errors.Join(restoreErr, closeErr)
	}

	select {
	case <-ctx.Done():
		finishErr := finish()
		return loginURLContinue, errors.Join(fmt.Errorf("interrupted: %w", ctx.Err()), finishErr)
	case result := <-resultCh:
		finishErr := finish()
		if ctx.Err() != nil {
			return loginURLContinue, errors.Join(fmt.Errorf("interrupted: %w", ctx.Err()), finishErr)
		}
		if result.err != nil {
			return loginURLContinue, errors.Join(result.err, finishErr)
		}
		if finishErr != nil {
			return loginURLContinue, fmt.Errorf("restore terminal input: %w", finishErr)
		}
		return result.action, nil
	}
}

// readLoginURLActionBytes ignores unsupported keys until it sees one of the
// advertised choices. Ctrl-C is handled explicitly because raw mode prevents
// the terminal driver from turning it into SIGINT.
func readLoginURLActionBytes(r io.Reader) loginURLActionResult {
	var key [1]byte
	for {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return loginURLActionResult{err: fmt.Errorf("read login action: %w", err)}
		}

		switch key[0] {
		case '\r', '\n':
			return loginURLActionResult{action: loginURLContinue}
		case 'c', 'C':
			return loginURLActionResult{action: loginURLCopy}
		case 'o', 'O':
			return loginURLActionResult{action: loginURLOpen}
		case 0x03:
			return loginURLActionResult{err: context.Canceled}
		}
	}
}

func openBrowser(ctx context.Context, browserURL string) error {
	u, err := url.Parse(browserURL)
	if err != nil || (u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP) {
		return fmt.Errorf("refusing to open non-HTTP URL: %s", browserURL)
	}

	// Under test there's no usable browser, and we must not spawn a real one
	// on a dev/CI host. URL validation above still applies.
	if interactive.UnderTest() {
		return errors.New("browser unavailable under test")
	}

	var command string
	var args []string

	switch runtime.GOOS {
	case darwinGOOS:
		command = "open"
		args = []string{browserURL}
	case "linux":
		command = "xdg-open"
		args = []string{browserURL}
	case "windows":
		command = "cmd"
		args = []string{"/c", "start", "", browserURL}
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}

	cmd := exec.CommandContext(ctx, command, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser command %q: %w", command, err)
	}

	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release browser process: %w", err)
	}

	return nil
}

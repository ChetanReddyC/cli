package redact

import "regexp"

// Provider-specific deterministic secret patterns.
//
// These catch credential formats whose Shannon entropy can fall below the
// entropyThreshold (4.5) and whose surrounding variable name is not
// password-shaped, so none of the entropy, credentialed-URI,
// connection-string, or credential-key/value layers reliably flag them.
// The betterleaks layer also misses them in isolation: its Supabase
// secret-key rule is a *composite* rule (RequiredRules:
// supabase-project-url) that only fires when a matching "*.supabase.co"
// URL is present in the same content, plus an entropy filter. A secret
// captured on its own therefore passes straight through.
//
// Detection here is purely prefix + length based: it never depends on
// entropy or the surrounding key name, matching the deterministic
// behaviour requested in issue #1716.
//
// Supabase (https://supabase.com/docs/guides/getting-started/api-keys):
//   - sb_secret_...      secret API key (replaces the legacy service_role
//     key; bypasses row-level security, server-side
//     only) — SENSITIVE, always redacted.
//   - sbp_...            personal access token used by the Supabase CLI and
//     Management API — SENSITIVE, always redacted.
//   - sb_publishable_... publishable key (replaces the legacy anon key). It
//     is designed to be embedded in client-side code and
//     is protected by row-level security, so it is NOT a
//     secret and is intentionally NOT redacted here.
//     Redacting it would be false-positive noise; this
//     matches betterleaks, which also ships no
//     publishable-key rule.
//
// The current real key bodies are 31 chars (sb_secret_) and 40 chars
// (sbp_); charsets mirror betterleaks (sb_secret_ keys are mixed-case
// base64url, sbp_ tokens are lowercase). The {20,} floor comfortably
// catches the current and plausibly-longer future formats while rejecting
// short identifier-like collisions such as "sb_secret_short".
var providerTokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsb_secret_[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bsbp_[a-z0-9_-]{20,}`),
}

// detectProviderTokens returns tagged regions for every occurrence of a
// known provider secret-token prefix in s. Regions use the empty label so
// they render as the bare "REDACTED" token, consistent with the other
// always-on secret layers.
func detectProviderTokens(s string) []taggedRegion {
	var regions []taggedRegion
	for _, pat := range providerTokenPatterns {
		for _, loc := range pat.FindAllStringIndex(s, -1) {
			regions = append(regions, taggedRegion{region: region{loc[0], loc[1]}})
		}
	}
	return regions
}

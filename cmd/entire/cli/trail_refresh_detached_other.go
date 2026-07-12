//go:build !unix && !windows

package cli

// spawnDetachedTrailRefreshProcess is a no-op on unsupported platforms.
// The trails-enablement cache simply stays unknown; this only affects a
// best-effort feature-enablement hint, never checkpoint capture.
func spawnDetachedTrailRefreshProcess(string) {
	// No-op: detached subprocess spawning not implemented for this platform.
}

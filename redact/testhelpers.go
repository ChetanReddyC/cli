package redact

// Cross-package test helpers. Lives in a regular .go file (not
// export_test.go) so tests in cmd/entire/cli/strategy can call it.
// The "ForTest" suffix is the production-code-must-not-call signal.

// ResetOPFConfigForTest clears OPF configuration and the circuit
// breaker. Test-only.
func ResetOPFConfigForTest() {
	resetOPFConfig()
}

// SetScannerDegradedForTest flips the scanner degradation flag. Runtime
// scan errors are engineered out (see detectGoredact), so tests cannot
// reach this state organically. Exported for redact's own sentinel tests
// and for dependent packages' tests of the fail-the-write paths that
// consume ErrScannerDegraded (a later task adds those call sites).
// Test-only.
func SetScannerDegradedForTest(v bool) {
	scannerDegraded.Store(v)
}

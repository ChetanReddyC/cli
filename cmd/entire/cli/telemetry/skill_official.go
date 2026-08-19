package telemetry

// Skill telemetry is recorded verbatim only for skill names listed here.
// Skill names are arbitrary user tokens — a custom slash command or
// third-party skill name can carry sensitive identifiers (project, vendor,
// customer), exactly like third-party plugin names (see the cli package's
// IsOfficialPlugin). Unlike plugins, the event itself is still worth counting
// — the metric this feeds compares skill adoption volume against agent-help —
// so unknown names are reported under the fixed "custom" category instead of
// being dropped. Match is case-sensitive and exact.
//
//nolint:gochecknoglobals // package-level allowlist, mirroring officialPlugins.
var officialSkillNames = map[string]struct{}{
	// Entire-shipped skills. The agent-help skill is scaffolded as "entire"
	// across claude/codex/gemini (see agentHelpSkillTemplate).
	"entire": {},
}

// customSkillCategory is the fixed value reported in place of any skill name
// not on the allowlist.
const customSkillCategory = "custom"

// skillNameForTelemetry maps a raw skill name to the value safe to send:
// allowlisted names pass through, everything else collapses to "custom".
func skillNameForTelemetry(name string) string {
	if _, ok := officialSkillNames[name]; ok {
		return name
	}
	return customSkillCategory
}

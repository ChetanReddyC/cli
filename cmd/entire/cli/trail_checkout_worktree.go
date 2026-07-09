package cli

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	trailWorktreesRelDir      = ".entire/worktrees"
	trailWorktreeFallbackName = "branch"
)

func defaultTrailWorktreePath(repoRoot, branch string, trailNumber int) string {
	name := sanitizeTrailWorktreeName(branch)
	if trailNumber > 0 {
		name = fmt.Sprintf("trail-%d-%s", trailNumber, name)
	}
	return filepath.Join(repoRoot, filepath.FromSlash(trailWorktreesRelDir), name)
}

func sanitizeTrailWorktreeName(branch string) string {
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, strings.TrimSpace(branch))
	name = strings.Trim(name, "-.")
	if name == "" {
		return trailWorktreeFallbackName
	}
	return name
}

func shellQuotePath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

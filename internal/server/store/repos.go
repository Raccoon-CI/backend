package store

import (
	"regexp"
	"strings"
)

// repoPattern is owner/name or owner/*: the forms a token's repository list
// takes.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/([A-Za-z0-9._-]+|\*)$`)

// NormalizeRepoPatterns checks and lower-cases a list of repository patterns
// (GitHub names are not case sensitive), dropping repeats. ok is false when one
// is not owner/name or owner/*.
func NormalizeRepoPatterns(in []string) (out []string, ok bool) {
	seen := map[string]bool{}
	for _, p := range in {
		p = strings.ToLower(strings.TrimSpace(p))
		if !repoPattern.MatchString(p) {
			return nil, false
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, true
}

// RepoAllowed says whether repo ("owner/name") is covered by the patterns. No
// patterns at all means every repository.
func RepoAllowed(patterns []string, repo string) bool {
	if len(patterns) == 0 {
		return true
	}
	repo = strings.ToLower(repo)
	owner, _, _ := strings.Cut(repo, "/")
	for _, p := range patterns {
		if p == repo || p == owner+"/*" {
			return true
		}
	}
	return false
}

// likeEscape escapes what LIKE treats as wildcards (names have underscores).
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

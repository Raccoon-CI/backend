package caches

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Space is where the caches a run may use live. Each run has one space it
// writes to (Own) and a list of spaces it may restore from (Read, Own first).
//
// The rules follow GitHub's, so that what a workflow does there it does here,
// and so that one run's code cannot plant a cache another run will trust:
//
//   - the default branch has a space of its own, which only runs on the
//     default branch write;
//   - every other ref (a feature branch, a tag, a pull request's merge ref,
//     fork or not) has a space of its own, and may also restore from the
//     default branch's, but never from another ref's and never write to it.
//
// GitHub also lets a pull request restore from its base branch when that is
// not the default branch; here it cannot (fewer hits, nothing less safe).
type Space struct {
	Own  string
	Read []string
	// Org is the owner of the repository, in lower case: whose bucket the
	// objects go to.
	Org string
}

// hash names a repository or a ref in a path without any of its characters.
func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// RepoID is the directory of a repository's spaces: repository names are not
// case-sensitive on GitHub.
func RepoID(repo string) string { return hash(strings.ToLower(repo)) }

// SpaceFor decides the spaces of a run on repo at ref. defaultBranch may be
// empty when it is not known: the run is then taken for a non-default one,
// which can only cost it hits.
func SpaceFor(repo, ref, defaultBranch string) Space {
	r := RepoID(repo)
	def := r + "/default"
	org, _, _ := strings.Cut(strings.ToLower(repo), "/")
	if defaultBranch != "" && ref == "refs/heads/"+defaultBranch {
		return Space{Own: def, Read: []string{def}, Org: org}
	}
	own := r + "/r-" + hash(ref)
	return Space{Own: own, Read: []string{own, def}, Org: org}
}

// validSpace says whether id has the shape SpaceFor makes, which is all the
// paths and keys built from it need.
func validSpace(id string) bool {
	repo, space, ok := strings.Cut(id, "/")
	if !ok || !isHex(repo, 16) {
		return false
	}
	if space == "default" {
		return true
	}
	rest, ok := strings.CutPrefix(space, "r-")
	return ok && isHex(rest, 16)
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

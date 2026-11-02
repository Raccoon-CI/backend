// Package secrets keeps the secrets and variables of organizations and
// repositories, and says which of them a run gets.
//
// A secret is sealed (internal/server/vault) before it reaches the database,
// can be written and deleted but never read back, and is opened only when a
// run is about to be sent to a machine. A run gets only the secrets its
// workflow names (secrets.NAME, secrets['NAME']), the repository's over the
// organization's of the same name, and a pull request from another repository
// gets none, as on GitHub. Variables are not secret: they can be read back and
// every run gets them.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/server/store"
	"github.com/Raccoon-CI/raccoonci/internal/server/vault"
)

// Limits, GitHub's own.
const (
	MaxValue = 48 << 10
	MaxName  = 100
)

var (
	namePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	scopePattern = regexp.MustCompile(`^(org:[a-z0-9][a-z0-9._-]*|repo:[a-z0-9][a-z0-9._-]*/[a-z0-9._-]+)$`)
)

// ErrInvalid is wrapped by what is wrong with a scope, a name or a value.
var ErrInvalid = errors.New("invalid")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Org is the scope of an organization (or user) account; names are not
// case-sensitive on GitHub.
func Org(name string) string { return "org:" + strings.ToLower(name) }

// Repo is the scope of a repository, "owner/name".
func Repo(full string) string { return "repo:" + strings.ToLower(full) }

// Service keeps and hands out secrets.
type Service struct {
	Store store.Store
	Vault *vault.Vault
}

// ValidScope says whether s is a scope this service keeps things under.
func ValidScope(s string) bool { return scopePattern.MatchString(s) }

func checkName(name string) (string, error) {
	if len(name) > MaxName || !namePattern.MatchString(name) {
		return "", invalid("a name is letters, digits and underscores, not starting with a digit, at most %d long", MaxName)
	}
	if strings.HasPrefix(strings.ToUpper(name), "GITHUB_") {
		return "", invalid("names starting with GITHUB_ are reserved")
	}
	// Names are not case-sensitive and are kept in capitals, as on GitHub.
	return strings.ToUpper(name), nil
}

func checkScope(scope string) error {
	if !ValidScope(scope) {
		return invalid("a scope is org:<name> or repo:<owner>/<name>, in lower case")
	}
	return nil
}

// SetSecret seals and stores a secret.
func (s *Service) SetSecret(ctx context.Context, scope, name, value string) error {
	if err := checkScope(scope); err != nil {
		return err
	}
	name, err := checkName(name)
	if err != nil {
		return err
	}
	if value == "" || len(value) > MaxValue {
		return invalid("a secret's value is 1 to %d bytes", MaxValue)
	}
	keyID, sealed, err := s.Vault.Seal(scope, name, []byte(value))
	if err != nil {
		return err
	}
	return s.Store.PutSecret(ctx, store.Secret{Scope: scope, Name: name, Kind: store.KindSecret, KeyID: keyID, Value: sealed})
}

// SetVar stores a variable.
func (s *Service) SetVar(ctx context.Context, scope, name, value string) error {
	if err := checkScope(scope); err != nil {
		return err
	}
	name, err := checkName(name)
	if err != nil {
		return err
	}
	if len(value) > MaxValue {
		return invalid("a variable's value is at most %d bytes", MaxValue)
	}
	return s.Store.PutSecret(ctx, store.Secret{Scope: scope, Name: name, Kind: store.KindVar, Value: []byte(value)})
}

// Delete removes a secret or variable of the kind given.
func (s *Service) Delete(ctx context.Context, scope, kind, name string) (bool, error) {
	if err := checkScope(scope); err != nil {
		return false, err
	}
	name, err := checkName(name)
	if err != nil {
		return false, err
	}
	cur, err := s.Store.GetSecret(ctx, scope, name)
	if errors.Is(err, store.ErrNotFound) || (err == nil && cur.Kind != kind) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return s.Store.DeleteSecret(ctx, scope, name)
}

// Entry is what listing shows: a secret's name and when it changed, never its
// value; a variable's value too.
type Entry struct {
	Name      string
	UpdatedAt time.Time
	Value     string // variables only
}

// List lists the secrets (kind secret) or variables (kind var) of a scope.
func (s *Service) List(ctx context.Context, scope, kind string) ([]Entry, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	all, err := s.Store.ListSecrets(ctx, scope)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, sec := range all {
		if sec.Kind != kind {
			continue
		}
		e := Entry{Name: sec.Name, UpdatedAt: sec.UpdatedAt}
		if kind == store.KindVar {
			e.Value = string(sec.Value)
		}
		out = append(out, e)
	}
	return out, nil
}

var (
	dotted  = regexp.MustCompile(`(?i)\bsecrets\.([A-Za-z_][A-Za-z0-9_]*)`)
	indexed = regexp.MustCompile(`(?i)\bsecrets\[\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]\s*\]`)
)

// Referenced returns the secret names a workflow's text names, in capitals,
// sorted. Only literal names count: a secret reached by a computed name, or
// by passing all of `secrets` on, is not sent to the machine.
func Referenced(workflowText string) []string {
	seen := map[string]bool{}
	for _, re := range []*regexp.Regexp{dotted, indexed} {
		for _, m := range re.FindAllStringSubmatch(workflowText, -1) {
			seen[strings.ToUpper(m[1])] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Run is what a run gets.
type Run struct {
	Secrets  map[string]string
	Vars     map[string]string
	Warnings []string // what the run's log should say: a secret that could not be opened
}

// ForRun resolves the secrets and variables of a run of repo (owner/name) with
// the given workflow text. fork is set for a pull request from another
// repository: it gets no secrets.
func (s *Service) ForRun(ctx context.Context, repo, workflowText string, fork bool) (Run, error) {
	owner, _, _ := strings.Cut(repo, "/")
	scopes := []string{Org(owner), Repo(repo)} // later ones win
	out := Run{Secrets: map[string]string{}, Vars: map[string]string{}}
	bySecret := map[string]store.Secret{}
	for _, scope := range scopes {
		list, err := s.Store.ListSecrets(ctx, scope)
		if err != nil {
			return Run{}, err
		}
		for _, sec := range list {
			// Only these two kinds are a workflow's to use: what the server keeps
			// under the same scope for itself (an organization's bucket) never goes.
			switch sec.Kind {
			case store.KindVar:
				out.Vars[sec.Name] = string(sec.Value)
			case store.KindSecret:
				bySecret[sec.Name] = sec
			}
		}
	}
	if fork {
		return out, nil
	}
	for _, name := range Referenced(workflowText) {
		sec, ok := bySecret[name]
		if !ok {
			continue
		}
		pt, err := s.Vault.Open(sec.Scope, sec.Name, sec.KeyID, sec.Value)
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("the secret %s could not be opened (%v) and was not passed", name, err))
			continue
		}
		out.Secrets[name] = string(pt)
	}
	return out, nil
}

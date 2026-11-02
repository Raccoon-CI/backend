package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"raccoonci/internal/server/secrets"
	"raccoonci/internal/server/store"
)

// secretBody is what PUT /v1/secrets and /v1/vars take.
type secretBody struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (a *API) secretsOff(w http.ResponseWriter) bool {
	if a.Secrets == nil {
		fail(w, http.StatusServiceUnavailable, "secrets are not available: the server has no master key")
		return true
	}
	return false
}

// answerSecrets turns what the secrets service said into an answer.
func answerSecrets(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, secrets.ErrInvalid):
		fail(w, http.StatusBadRequest, err.Error())
	default:
		fail(w, http.StatusInternalServerError, "could not keep the secret")
	}
	return true
}

func (a *API) putSecret(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.secretsOff(w) {
			return
		}
		var b secretBody
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil {
			fail(w, http.StatusBadRequest, "the body must be a JSON object with scope, name and value")
			return
		}
		scope := strings.ToLower(b.Scope)
		var err error
		if kind == store.KindSecret {
			err = a.Secrets.SetSecret(r.Context(), scope, b.Name, b.Value)
		} else {
			err = a.Secrets.SetVar(r.Context(), scope, b.Name, b.Value)
		}
		if answerSecrets(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *API) deleteSecret(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.secretsOff(w) {
			return
		}
		q := r.URL.Query()
		ok, err := a.Secrets.Delete(r.Context(), strings.ToLower(q.Get("scope")), kind, q.Get("name"))
		if answerSecrets(w, err) {
			return
		}
		if !ok {
			fail(w, http.StatusNotFound, "there is none with that scope and name")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *API) listSecrets(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.secretsOff(w) {
			return
		}
		list, err := a.Secrets.List(r.Context(), strings.ToLower(r.URL.Query().Get("scope")), kind)
		if answerSecrets(w, err) {
			return
		}
		type entry struct {
			Name      string    `json:"name"`
			Value     *string   `json:"value,omitempty"` // variables only
			UpdatedAt time.Time `json:"updated_at"`
		}
		out := make([]entry, 0, len(list))
		for _, e := range list {
			item := entry{Name: e.Name, UpdatedAt: e.UpdatedAt}
			if kind == store.KindVar {
				v := e.Value
				item.Value = &v
			}
			out = append(out, item)
		}
		key := "secrets"
		if kind == store.KindVar {
			key = "vars"
		}
		writeJSON(w, http.StatusOK, map[string]any{key: out})
	}
}

type tokenJSON struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	// Repos are the repositories a read token is limited to; absent for all.
	Repos []string `json:"repos,omitempty"`
}

func toToken(t store.APIToken) tokenJSON {
	return tokenJSON{ID: t.ID, Name: t.Name, Role: t.Role, CreatedAt: t.CreatedAt, LastUsedAt: timePtr(t.LastUsedAt), RevokedAt: timePtr(t.RevokedAt), Repos: t.Repos}
}

func (a *API) listTokens(w http.ResponseWriter, r *http.Request) {
	list, err := a.Store.ListTokens(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not read the tokens")
		return
	}
	out := make([]tokenJSON, 0, len(list))
	for _, t := range list {
		out = append(out, toToken(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// createToken makes a token and shows it once: only its hash is kept.
func (a *API) createToken(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name  string   `json:"name"`
		Role  string   `json:"role"`
		Repos []string `json:"repos"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil || strings.TrimSpace(b.Name) == "" || len(b.Name) > 100 {
		fail(w, http.StatusBadRequest, "the body must be a JSON object with a name (up to 100 characters) and a role")
		return
	}
	if b.Role != store.RoleRead && b.Role != store.RoleAdmin {
		fail(w, http.StatusBadRequest, "the role is read or admin")
		return
	}
	var repos []string
	if len(b.Repos) > 0 {
		if b.Role != store.RoleRead {
			// An admin changes things everywhere (secrets, tokens), which a
			// repository list could not honestly limit.
			fail(w, http.StatusBadRequest, "only a token of the read role can be limited to repositories")
			return
		}
		var ok bool
		if repos, ok = store.NormalizeRepoPatterns(b.Repos); !ok || len(repos) > 100 {
			fail(w, http.StatusBadRequest, "repos are owner/name or owner/* (at most 100)")
			return
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		fail(w, http.StatusInternalServerError, "could not make a token")
		return
	}
	token := "rct_" + base64.RawURLEncoding.EncodeToString(raw)
	t, err := a.Store.CreateToken(r.Context(), strings.TrimSpace(b.Name), b.Role, HashToken(token), repos)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not keep the token")
		return
	}
	out := struct {
		tokenJSON
		// Token is shown here and nowhere else.
		Token string `json:"token"`
	}{toToken(t), token}
	writeJSON(w, http.StatusCreated, out)
}

func (a *API) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		fail(w, http.StatusBadRequest, "the id must be a number")
		return
	}
	ok, err := a.Store.RevokeToken(r.Context(), id)
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, "could not revoke the token")
	case !ok:
		fail(w, http.StatusNotFound, "no working token with that id")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

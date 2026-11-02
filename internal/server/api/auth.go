package api

import (
	"net/http"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/server/store"
)

// SignIn is auth.Service: the two steps of signing in with GitHub.
type SignIn interface {
	Login(w http.ResponseWriter, r *http.Request)
	Callback(w http.ResponseWriter, r *http.Request)
}

func (a *API) authLogin(w http.ResponseWriter, r *http.Request) {
	if a.Auth == nil {
		fail(w, http.StatusServiceUnavailable, "signing in with GitHub is not set up on this server")
		return
	}
	a.Auth.Login(w, r)
}

func (a *API) authCallback(w http.ResponseWriter, r *http.Request) {
	if a.Auth == nil {
		fail(w, http.StatusServiceUnavailable, "signing in with GitHub is not set up on this server")
		return
	}
	a.Auth.Callback(w, r)
}

// authMe says who a token is: what a frontend asks after sign-in, and to know
// when to sign in again.
func (a *API) authMe(w http.ResponseWriter, r *http.Request) {
	t := whoOf(r.Context())
	out := map[string]any{"kind": "api", "role": t.Role, "repos": []string{}}
	switch {
	case t.ID == 0: // the bootstrap token
		out["kind"], out["role"] = "bootstrap", store.RoleAdmin
	case t.User != "":
		out["kind"], out["user"] = "session", t.User
	default:
		out["name"] = t.Name
	}
	if len(t.Repos) > 0 {
		out["repos"] = t.Repos
	}
	if !t.ExpiresAt.IsZero() {
		out["expires_at"] = t.ExpiresAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// authLogout ends the token it is called with. The bootstrap token has no row
// to end; a token of the API ends too, which is what its holder asked.
func (a *API) authLogout(w http.ResponseWriter, r *http.Request) {
	t := whoOf(r.Context())
	if t.ID == 0 {
		fail(w, http.StatusBadRequest, "the bootstrap token cannot be signed out; it is set in the server's configuration")
		return
	}
	if _, err := a.Store.RevokeToken(r.Context(), t.ID); err != nil {
		fail(w, http.StatusInternalServerError, "could not sign out")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

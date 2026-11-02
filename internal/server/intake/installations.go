package intake

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Raccoon-CI/raccoonci/internal/server/store"
)

// installationPayload is the part of an `installation` or
// `installation_repositories` delivery the server keeps.
type installationPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
		RepositorySelection string `json:"repository_selection"`
		SuspendedAt         any    `json:"suspended_at"`
	} `json:"installation"`
	Repositories        []repoRef `json:"repositories"`
	RepositoriesAdded   []repoRef `json:"repositories_added"`
	RepositoriesRemoved []repoRef `json:"repositories_removed"`
}

type repoRef struct {
	FullName string `json:"full_name"`
}

func names(in []repoRef) []string {
	var out []string
	for _, r := range in {
		if r.FullName != "" {
			out = append(out, r.FullName)
		}
	}
	return out
}

// handleInstallation keeps where the App is installed. These deliveries name
// no repository to work on, so they are not events or runs; they are only
// recorded (an installation is a fact, so repeating one is harmless). It
// answers like the other handlers: 202 when kept.
func (h *Handler) handleInstallation(w http.ResponseWriter, r *http.Request, kind string, body []byte) {
	var p installationPayload
	if err := json.Unmarshal(body, &p); err != nil || p.Installation.ID == 0 {
		http.Error(w, "the payload names no installation", http.StatusBadRequest)
		return
	}
	in := store.Installation{
		ID: p.Installation.ID, Account: p.Installation.Account.Login, AccountType: p.Installation.Account.Type,
		Selection: p.Installation.RepositorySelection, Suspended: p.Installation.SuspendedAt != nil,
	}
	ctx := r.Context()
	var err error
	switch {
	case kind == "installation" && p.Action == "deleted":
		err = h.Store.DeleteInstallation(ctx, in.ID)
	case kind == "installation" && (p.Action == "suspend" || p.Action == "unsuspend"):
		in.Suspended = p.Action == "suspend"
		// A suspend for an installation the server never heard of is made known.
		if err = h.Store.SetInstallationSuspended(ctx, in.ID, in.Suspended); err == store.ErrNotFound {
			err = h.Store.PutInstallation(ctx, in, nil, false)
		}
	case kind == "installation":
		// created, new_permissions_accepted: the repositories in the payload are
		// the full list for a "selected" installation (none are listed for "all"
		// on some actions, which then leaves the list as it was).
		err = h.Store.PutInstallation(ctx, in, names(p.Repositories), len(p.Repositories) > 0)
	case kind == "installation_repositories":
		if err = h.Store.ChangeInstallationRepos(ctx, in.ID, in.Selection, names(p.RepositoriesAdded), names(p.RepositoriesRemoved)); err == store.ErrNotFound {
			err = h.Store.PutInstallation(ctx, in, names(p.RepositoriesAdded), false)
		}
	}
	if err != nil {
		h.log().Error("keeping an installation failed", "installation", in.ID, "event", kind, "err", err)
		http.Error(w, "could not keep the installation", http.StatusInternalServerError)
		return
	}
	h.log().Info("installation recorded", "installation", in.ID, "account", strings.ToLower(in.Account), "event", kind, "action", p.Action)
	reply(w, http.StatusAccepted, map[string]string{"status": "recorded"})
}

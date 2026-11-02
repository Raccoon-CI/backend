package intake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"

	"raccoonci/internal/server/store"
)

// Rerun queues a new run like a finished one: the same event, commit, workflow
// file (as it was then) and inputs. The new run remembers the one it repeats.
func (h *Handler) Rerun(ctx context.Context, runID int64) (store.Run, error) {
	orig, err := h.Store.GetRun(ctx, runID)
	if err != nil {
		if err == store.ErrNotFound {
			return store.Run{}, refuse(http.StatusNotFound, "no such run")
		}
		return store.Run{}, err
	}
	if orig.Status != store.RunCompleted {
		return store.Run{}, refuse(http.StatusConflict, "run %d is %s; only a finished run can be run again", runID, orig.Status)
	}
	ev, err := h.Store.GetEvent(ctx, orig.EventID)
	if err != nil {
		return store.Run{}, fmt.Errorf("reading the event of run %d: %w", runID, err)
	}
	// A run started by hand before the App was known has no installation.
	if ev.InstallationID == 0 {
		inst, err := h.installationFor(ctx, ev.Repo)
		if err != nil {
			return store.Run{}, fmt.Errorf("finding the installation for %s: %w", ev.Repo, err)
		}
		ev.InstallationID = inst
	}
	// A rerun is a delivery of its own as far as the store goes: a run belongs
	// to one event and one workflow file, and the same pair cannot start twice.
	var id [12]byte
	rand.Read(id[:])
	again, _, err := h.Store.RecordEvent(ctx, store.Event{
		DeliveryID: fmt.Sprintf("rerun-%d-%s", runID, hex.EncodeToString(id[:])), Kind: ev.Kind, Action: ev.Action, Repo: ev.Repo,
		InstallationID: ev.InstallationID, Payload: ev.Payload, Status: store.EventHandled, Note: fmt.Sprintf("a rerun of run %d", runID),
	})
	if err != nil {
		return store.Run{}, err
	}
	of := orig.ID
	if orig.RerunOf != 0 {
		of = orig.RerunOf // the chain points at the run that started it
	}
	run, _, err := h.Store.CreateRun(ctx, store.Run{
		EventID: again.ID, Repo: orig.Repo, WorkflowPath: orig.WorkflowPath, WorkflowName: orig.WorkflowName, Event: orig.Event,
		Ref: orig.Ref, SHA: orig.SHA, BaseSHA: orig.BaseSHA, PullRequest: orig.PullRequest, Fork: orig.Fork, Actor: orig.Actor,
		Inputs: orig.Inputs, RerunOf: of, WorkflowText: orig.WorkflowText,
	})
	if err != nil {
		return store.Run{}, err
	}
	h.log().Info("run repeated", "run", run.ID, "of", runID)
	if h.Notify != nil {
		h.Notify()
	}
	return run, nil
}

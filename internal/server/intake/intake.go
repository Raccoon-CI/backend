// Package intake receives GitHub's webhook deliveries and turns them into
// runs: it checks the signature, stores the delivery as received, answers
// GitHub at once, and then (still in the background) reads the repository's
// workflows at the event's commit, asks trigger.Match which of them the event
// starts, and creates a run for each.
//
// A delivery is stored before anything is done with it, under GitHub's
// delivery id, so a redelivery starts nothing twice and one interrupted by a
// restart is picked up again (Recover). The raw payload is kept so a delivery
// can be replayed.
package intake

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"raccoonci/internal/server/github"
	"raccoonci/internal/server/store"
	"raccoonci/internal/trigger"
	"raccoonci/internal/workflow"
)

// maxBody is GitHub's own limit for a delivery (25 MB), a little over.
const maxBody = 26 << 20

// handleTimeout bounds the background handling of one delivery.
const handleTimeout = 2 * time.Minute

// Source is what the handler needs from GitHub; internal/server/github.Client
// is the real one.
type Source interface {
	WorkflowFiles(ctx context.Context, installationID int64, repo, ref string) ([]github.File, error)
	ChangedFiles(ctx context.Context, installationID int64, repo, base, head string) ([]string, error)
	// ResolveRef and FileAt serve Dispatch.
	ResolveRef(ctx context.Context, installationID int64, repo, ref string) (string, error)
	FileAt(ctx context.Context, installationID int64, repo, filePath, ref string) ([]byte, error)
}

// Handler is the webhook endpoint.
type Handler struct {
	// Secret signs the deliveries (the App's or the repository webhook's
	// secret). An empty secret refuses every delivery.
	Secret []byte
	Store  store.Store
	Source Source
	// Allow lists the repositories the server works for: "owner/name" or
	// "owner/*". A delivery for any other repository is answered and
	// forgotten, not stored. Empty allows none.
	Allow []string
	Log   *slog.Logger
	// Inline handles a delivery before answering instead of in the
	// background; tests use it.
	Inline bool
	// Notify, if set, is called after runs have been queued, so the executor
	// need not wait for its next look at the queue.
	Notify func()

	wg   sync.WaitGroup
	sem  chan struct{}
	once sync.Once
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (h *Handler) init() {
	h.once.Do(func() { h.sem = make(chan struct{}, 8) })
}

// Wait blocks until the deliveries being handled in the background are done.
func (h *Handler) Wait() { h.wg.Wait() }

// Allows reports whether the server works for the repository (see Allow).
func (h *Handler) Allows(repo string) bool { return h.allowed(repo) }

func (h *Handler) allowed(repo string) bool {
	for _, a := range h.Allow {
		if a == repo {
			return true
		}
		if owner, ok := strings.CutSuffix(a, "/*"); ok && strings.HasPrefix(repo, owner+"/") {
			return true
		}
	}
	return false
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// Sign returns the X-Hub-Signature-256 value for body: what GitHub sends, and
// what `raccoonci-server replay` sends to test a server.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Signed reports whether sig (the X-Hub-Signature-256 header) is the
// HMAC-SHA256 of body under secret, compared in constant time.
func Signed(secret, body []byte, sig string) bool {
	hexSum, ok := strings.CutPrefix(sig, "sha256=")
	if !ok || len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// envelope is what every delivery has in common.
type envelope struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.init()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "the body could not be read", http.StatusRequestEntityTooLarge)
		return
	}
	// Nothing in the body is looked at before this: an unsigned request gets
	// the same answer whatever it says.
	if !Signed(h.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		h.log().Warn("delivery refused: bad signature", "remote", r.RemoteAddr)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	kind, delivery := r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
	if kind == "ping" {
		reply(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}
	if kind != "push" && kind != "pull_request" {
		reply(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": "not an event this server handles"})
		return
	}
	if delivery == "" {
		http.Error(w, "missing X-GitHub-Delivery", http.StatusBadRequest)
		return
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || env.Repository.FullName == "" {
		http.Error(w, "the payload names no repository", http.StatusBadRequest)
		return
	}
	repo := env.Repository.FullName
	if !h.allowed(repo) {
		h.log().Info("delivery for a repository that is not enabled", "repo", repo, "event", kind)
		reply(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": "repository not enabled"})
		return
	}
	ev, created, err := h.Store.RecordEvent(r.Context(), store.Event{
		DeliveryID: delivery, Kind: kind, Action: env.Action, Repo: repo,
		InstallationID: env.Installation.ID, Payload: body,
	})
	if err != nil {
		h.log().Error("storing a delivery failed", "delivery", delivery, "err", err)
		http.Error(w, "could not store the delivery", http.StatusInternalServerError)
		return
	}
	if !created && ev.Status != store.EventReceived {
		reply(w, http.StatusOK, map[string]any{"status": "duplicate", "event": ev.ID})
		return
	}
	h.dispatch(ev)
	reply(w, http.StatusAccepted, map[string]any{"status": "accepted", "event": ev.ID})
}

func (h *Handler) dispatch(ev store.Event) {
	h.init()
	if h.Inline {
		h.process(ev)
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.sem <- struct{}{}
		defer func() { <-h.sem }()
		h.process(ev)
	}()
}

// Recover handles the deliveries a restart interrupted.
func (h *Handler) Recover(ctx context.Context) error {
	pending, err := h.Store.PendingEvents(ctx)
	if err != nil {
		return err
	}
	for _, ev := range pending {
		h.log().Info("handling a delivery left from before", "event", ev.ID, "kind", ev.Kind, "repo", ev.Repo)
		h.dispatch(ev)
	}
	return nil
}

// process decides what a stored delivery starts and records the outcome.
func (h *Handler) process(ev store.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), handleTimeout)
	defer cancel()
	status, note, err := h.decide(ctx, ev)
	if err != nil {
		status, note = store.EventFailed, err.Error()
		h.log().Error("handling a delivery failed", "event", ev.ID, "repo", ev.Repo, "err", err)
	}
	if err := h.Store.FinishEvent(ctx, ev.ID, status, note); err != nil {
		h.log().Error("recording the outcome of a delivery failed", "event", ev.ID, "err", err)
	}
}

// what a delivery is about, reduced to what the matching needs.
type subject struct {
	trig    trigger.Event
	sha     string // the commit to read workflows at, and to run
	baseSHA string // for the changed files; empty when there is none
	ref     string // the ref shown on the run
	pr      int
	fork    bool
	actor   string
}

const zeroSHA = "0000000000000000000000000000000000000000"

func subjectOf(ev store.Event) (s subject, ignore string, err error) {
	switch ev.Kind {
	case "push":
		var p struct {
			Ref     string `json:"ref"`
			Before  string `json:"before"`
			After   string `json:"after"`
			Deleted bool   `json:"deleted"`
			Sender  struct {
				Login string `json:"login"`
			} `json:"sender"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return s, "", fmt.Errorf("the push payload could not be read: %w", err)
		}
		if p.Deleted || p.After == zeroSHA || p.After == "" {
			return s, "the push deleted the ref", nil
		}
		s = subject{trig: trigger.Event{Name: "push", Ref: p.Ref}, sha: p.After, ref: p.Ref, actor: p.Sender.Login}
		if p.Before != zeroSHA {
			s.baseSHA = p.Before
		}
		return s, "", nil
	case "pull_request":
		var p struct {
			Action      string `json:"action"`
			Number      int    `json:"number"`
			PullRequest struct {
				Head struct {
					SHA  string `json:"sha"`
					Repo *struct {
						FullName string `json:"full_name"`
					} `json:"repo"`
				} `json:"head"`
				Base struct {
					Ref string `json:"ref"`
					SHA string `json:"sha"`
				} `json:"base"`
			} `json:"pull_request"`
			Sender struct {
				Login string `json:"login"`
			} `json:"sender"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return s, "", fmt.Errorf("the pull request payload could not be read: %w", err)
		}
		pr := p.PullRequest
		if pr.Head.SHA == "" || pr.Base.Ref == "" {
			return s, "", fmt.Errorf("the pull request payload has no head or base")
		}
		s = subject{
			trig: trigger.Event{Name: "pull_request", Action: p.Action, BaseRef: pr.Base.Ref},
			sha:  pr.Head.SHA, baseSHA: pr.Base.SHA, ref: fmt.Sprintf("refs/pull/%d/merge", p.Number), pr: p.Number,
			// A fork that was deleted has no head repository: treat it as a fork.
			fork:  pr.Head.Repo == nil || pr.Head.Repo.FullName != ev.Repo,
			actor: p.Sender.Login,
		}
		return s, "", nil
	}
	return s, "not an event this server handles", nil
}

// outcome of one workflow file for the event, kept in the delivery's note.
type outcome struct {
	Path   string `json:"path"`
	Result string `json:"result"` // run, skipped, invalid
	Reason string `json:"reason,omitempty"`
	Run    int64  `json:"run,omitempty"`
}

func (h *Handler) decide(ctx context.Context, ev store.Event) (status, note string, err error) {
	s, ignore, err := subjectOf(ev)
	if err != nil {
		return "", "", err
	}
	if ignore != "" {
		return store.EventIgnored, ignore, nil
	}
	files, err := h.Source.WorkflowFiles(ctx, ev.InstallationID, ev.Repo, s.sha)
	if err != nil {
		return "", "", fmt.Errorf("reading the workflows at %s: %w", s.sha, err)
	}
	if len(files) == 0 {
		return store.EventIgnored, "the repository has no workflows at that commit", nil
	}
	// The changed files are fetched once, for every workflow that has a path filter.
	if s.baseSHA != "" {
		changed, err := h.Source.ChangedFiles(ctx, ev.InstallationID, ev.Repo, s.baseSHA, s.sha)
		if err != nil {
			h.log().Warn("changed files unknown; path filters will not apply", "event", ev.ID, "err", err)
		} else {
			s.trig.Files = changed
		}
	}
	var outcomes []outcome
	started := 0
	for _, f := range files {
		o := outcome{Path: f.Path}
		w, perr := workflow.Parse(f.Content)
		if perr != nil {
			o.Result, o.Reason = "invalid", perr.Error()
			outcomes = append(outcomes, o)
			continue
		}
		res, merr := trigger.Match(w.On, s.trig)
		switch {
		case merr != nil:
			o.Result, o.Reason = "invalid", merr.Error()
		case !res.Triggered:
			o.Result, o.Reason = "skipped", res.Reason
		default:
			run, _, cerr := h.Store.CreateRun(ctx, store.Run{
				EventID: ev.ID, Repo: ev.Repo, WorkflowPath: f.Path, WorkflowName: nameOf(w, f.Path),
				Event: s.trig.Name, Ref: s.ref, SHA: s.sha, BaseSHA: s.baseSHA, PullRequest: s.pr, Fork: s.fork,
				Actor: s.actor, WorkflowText: string(f.Content),
			})
			if cerr != nil {
				return "", "", fmt.Errorf("creating the run for %s: %w", f.Path, cerr)
			}
			o.Result, o.Run = "run", run.ID
			started++
		}
		outcomes = append(outcomes, o)
	}
	b, _ := json.Marshal(outcomes)
	if started == 0 {
		return store.EventIgnored, string(b), nil
	}
	h.log().Info("runs created", "event", ev.ID, "repo", ev.Repo, "kind", ev.Kind, "runs", started)
	if h.Notify != nil {
		h.Notify()
	}
	return store.EventHandled, string(b), nil
}

func nameOf(w *workflow.Workflow, filePath string) string {
	if w.Name != "" {
		return w.Name
	}
	return path.Base(filePath)
}

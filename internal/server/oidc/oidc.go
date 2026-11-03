// Package oidc is the server as an OpenID Connect issuer for its jobs: a job
// with `permissions: id-token: write` can ask for a signed ID token (what
// actions/core's getIDToken and the cloud providers' login actions do) and
// trade it with AWS, GCP, Azure, Vault... for short-lived credentials, with no
// stored secret. It is GitHub's mechanism with another issuer.
//
// Three routes, all under /oidc: the discovery document, the key set (JWKS),
// and the token endpoint. The token endpoint is reached with a request token
// that only jobs with the permission are given (ACTIONS_ID_TOKEN_REQUEST_URL
// and ACTIONS_ID_TOKEN_REQUEST_TOKEN); it is a short HMAC token bound to one
// run and job, made with a key that lives in this process only, since it is
// needed only while the job runs (and a restart queues the run again).
//
// The ID token is signed RS256 with a key kept sealed in the database (a key
// per process when there is no master key, which works, since verifiers fetch
// the key set when they see an unknown key id). It carries the claims GitHub's
// carries that this server knows, so trust conditions written for GitHub
// (`sub`, `repository`, `ref`, `job_workflow_ref`...) keep their meaning; the
// issuer is this server's, so the trust is set up once for it.
package oidc

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/backend/internal/server/vault"
)

const (
	documentName = "oidc-key"
	// vaultScope and vaultName bind the sealed key to this use.
	vaultScope = "server"
	vaultName  = "$OIDC_KEY"

	requestTTL = 6 * time.Hour // a job's longest sensible life; the ID token itself lives for TTL
	maxAud     = 256
)

// Runs is what the issuer reads of a run: store.Store.
type Runs interface {
	GetRun(ctx context.Context, id int64) (store.Run, error)
	RunNumbers(ctx context.Context, run store.Run) (number, attempt int, err error)
}

// Documents keeps the sealed signing key (store.Store).
type Documents interface {
	PutDocument(ctx context.Context, name string, data []byte) error
	GetDocument(ctx context.Context, name string) (store.Document, error)
}

// Config for New.
type Config struct {
	Runs Runs
	Docs Documents
	// Vault seals the signing key in the database; nil keeps it in memory only.
	Vault *vault.Vault
	// PublicURL is the https address jobs and verifiers reach the server at,
	// without a path. The issuer is PublicURL + "/oidc".
	PublicURL string
	// TTL is how long an ID token is valid (default 10 minutes).
	TTL time.Duration
	Now func() time.Time
	Log *slog.Logger
}

// Service is the issuer.
type Service struct {
	cfg    Config
	key    *rsa.PrivateKey
	kid    string
	secret []byte // MAC key of request tokens, this process only
}

// New loads the signing key, or makes one (and keeps it, sealed, when there is
// a vault and a document store).
func New(ctx context.Context, c Config) (*Service, error) {
	if c.Runs == nil || !strings.HasPrefix(c.PublicURL, "https://") && !strings.HasPrefix(c.PublicURL, "http://127.0.0.1") && !strings.HasPrefix(c.PublicURL, "http://localhost") {
		return nil, errors.New("oidc: Runs and an https PublicURL are required (verifiers fetch the key set from it)")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.TTL <= 0 {
		c.TTL = 10 * time.Minute
	}
	s := &Service{cfg: c, secret: make([]byte, 32)}
	if _, err := rand.Read(s.secret); err != nil {
		return nil, err
	}
	key, err := s.loadKey(ctx)
	if err != nil {
		return nil, err
	}
	s.key = key
	der := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	sum := sha256.Sum256(der)
	s.kid = base64.RawURLEncoding.EncodeToString(sum[:12])
	return s, nil
}

func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.cfg.Log != nil {
		return s.cfg.Log
	}
	return slog.New(slog.DiscardHandler)
}

// stored is the document: the key id of the vault and the sealed PKCS#1 key.
type stored struct {
	KeyID  string `json:"key_id"`
	Sealed []byte `json:"sealed"`
}

func (s *Service) loadKey(ctx context.Context) (*rsa.PrivateKey, error) {
	if s.cfg.Vault != nil && s.cfg.Docs != nil {
		doc, err := s.cfg.Docs.GetDocument(ctx, documentName)
		switch {
		case err == nil:
			var st stored
			if err := json.Unmarshal(doc.Data, &st); err == nil {
				if der, err := s.cfg.Vault.Open(vaultScope, vaultName, st.KeyID, st.Sealed); err == nil {
					if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
						return k, nil
					}
				}
			}
			// Unreadable (the master key changed, or damage): a new key replaces it,
			// which only makes verifiers fetch the key set again.
			s.log().Warn("the stored OIDC signing key cannot be opened; making a new one")
		case errors.Is(err, store.ErrNotFound):
		default:
			return nil, err
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	if s.cfg.Vault != nil && s.cfg.Docs != nil {
		id, sealed, err := s.cfg.Vault.Seal(vaultScope, vaultName, x509.MarshalPKCS1PrivateKey(key))
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(stored{KeyID: id, Sealed: sealed})
		if err := s.cfg.Docs.PutDocument(ctx, documentName, b); err != nil {
			return nil, err
		}
	} else {
		s.log().Warn("no master key: the OIDC signing key lives in memory only and changes at every start")
	}
	return key, nil
}

// Issuer is the value of the `iss` claim.
func (s *Service) Issuer() string { return s.cfg.PublicURL + "/oidc" }

// Env is what a job with the permission needs to ask for ID tokens, as the
// toolkit reads it.
func (s *Service) Env(run int64, job string) map[string]string {
	return map[string]string{
		"ACTIONS_ID_TOKEN_REQUEST_URL":   s.Issuer() + "/token?api-version=2.0",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": s.requestToken(run, job),
	}
}

var b64 = base64.RawURLEncoding

type requestClaims struct {
	Run int64  `json:"r"`
	Job string `json:"j"`
	Exp int64  `json:"e"`
}

func (s *Service) mac(payload string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte("oidc-request\n" + payload))
	return b64.EncodeToString(m.Sum(nil))
}

func (s *Service) requestToken(run int64, job string) string {
	p, _ := json.Marshal(requestClaims{Run: run, Job: job, Exp: s.now().Add(requestTTL).Unix()})
	payload := b64.EncodeToString(p)
	return payload + "." + s.mac(payload)
}

var errRequestToken = errors.New("invalid or expired request token")

func (s *Service) verifyRequestToken(tok string) (requestClaims, error) {
	payload, sig, ok := strings.Cut(tok, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.mac(payload))) {
		return requestClaims{}, errRequestToken
	}
	raw, err := b64.DecodeString(payload)
	if err != nil {
		return requestClaims{}, errRequestToken
	}
	var c requestClaims
	if json.Unmarshal(raw, &c) != nil || c.Run == 0 || s.now().Unix() >= c.Exp {
		return requestClaims{}, errRequestToken
	}
	return c, nil
}

// Mount adds the routes to mux.
func (s *Service) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /oidc/.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("GET /oidc/jwks", s.jwks)
	mux.HandleFunc("GET /oidc/token", s.token)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Service) discovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, 200, map[string]any{
		"issuer":                                s.Issuer(),
		"jwks_uri":                              s.Issuer() + "/jwks",
		"subject_types_supported":               []string{"public"},
		"response_types_supported":              []string{"id_token"},
		"claims_supported":                      []string{"sub", "aud", "exp", "iat", "iss", "jti", "nbf", "ref", "sha", "repository", "repository_owner", "actor", "workflow", "event_name", "ref_type", "run_id", "run_number", "run_attempt", "job_workflow_ref", "runner_environment"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid"},
	})
}

func (s *Service) jwks(w http.ResponseWriter, r *http.Request) {
	pub := s.key.PublicKey
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, 200, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": s.kid,
		"n": b64.EncodeToString(pub.N.Bytes()),
		"e": b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (s *Service) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		writeJSON(w, 401, map[string]string{"message": "missing bearer token"})
		return
	}
	rc, err := s.verifyRequestToken(tok)
	if err != nil {
		writeJSON(w, 401, map[string]string{"message": err.Error()})
		return
	}
	run, err := s.cfg.Runs.GetRun(r.Context(), rc.Run)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, 404, map[string]string{"message": "no such run"})
		return
	}
	if err != nil {
		s.log().Error("reading a run for an ID token failed", "run", rc.Run, "err", err)
		writeJSON(w, 500, map[string]string{"message": "internal error"})
		return
	}
	// A run that is over has nothing to prove any more.
	if run.Status == store.RunCompleted {
		writeJSON(w, 403, map[string]string{"message": "the run is over"})
		return
	}
	aud := r.URL.Query().Get("audience")
	if aud == "" {
		owner, _, _ := strings.Cut(run.Repo, "/")
		aud = "https://github.com/" + owner // GitHub's default
	}
	if len(aud) > maxAud || strings.ContainsAny(aud, "\r\n\x00") {
		writeJSON(w, 400, map[string]string{"message": "the audience is too long or malformed"})
		return
	}
	number, attempt, err := s.cfg.Runs.RunNumbers(r.Context(), run)
	if err != nil {
		writeJSON(w, 500, map[string]string{"message": "internal error"})
		return
	}
	jwt, err := s.sign(s.claimsFor(run, aud, number, attempt))
	if err != nil {
		s.log().Error("signing an ID token failed", "run", rc.Run, "err", err)
		writeJSON(w, 500, map[string]string{"message": "internal error"})
		return
	}
	writeJSON(w, 200, map[string]string{"value": jwt})
}

// Subject is the `sub` claim, in GitHub's forms: a pull request is
// repo:owner/name:pull_request, anything else repo:owner/name:ref:<ref>.
func Subject(run store.Run) string {
	if run.Event == "pull_request" || run.Event == "pull_request_target" {
		return "repo:" + run.Repo + ":pull_request"
	}
	return "repo:" + run.Repo + ":ref:" + run.Ref
}

func (s *Service) claimsFor(run store.Run, aud string, number, attempt int) map[string]any {
	now := s.now()
	owner, _, _ := strings.Cut(run.Repo, "/")
	refType := "branch"
	if strings.HasPrefix(run.Ref, "refs/tags/") {
		refType = "tag"
	}
	id := make([]byte, 16)
	rand.Read(id)
	c := map[string]any{
		"iss": s.Issuer(), "aud": aud, "sub": Subject(run),
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(s.cfg.TTL).Unix(),
		"jti": fmt.Sprintf("%x", id),
		"ref": run.Ref, "sha": run.SHA, "ref_type": refType,
		"repository": run.Repo, "repository_owner": owner,
		"actor": run.Actor, "workflow": run.WorkflowName, "event_name": run.Event,
		"run_id": fmt.Sprint(run.ID), "run_number": fmt.Sprint(number), "run_attempt": fmt.Sprint(attempt),
		"job_workflow_ref":   run.Repo + "/" + run.WorkflowPath + "@" + run.Ref,
		"runner_environment": "self-hosted",
	}
	// Not issued: head_ref and base_ref (a pull request's branches are in the
	// event's payload, which the issuer does not read), environment, and the
	// numeric repository_id.
	return c
}

func (s *Service) sign(claims map[string]any) (string, error) {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.kid})
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	in := b64.EncodeToString(hdr) + "." + b64.EncodeToString(body)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return in + "." + b64.EncodeToString(sig), nil
}

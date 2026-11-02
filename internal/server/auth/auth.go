// Package auth signs people in with GitHub (the GitHub App's "Request user
// authorization (OAuth)" flow) and gives them a session token for the API.
//
// The flow: /v1/auth/github/login sends the browser to GitHub; GitHub sends it
// back to /v1/auth/github/callback with a code; the server trades the code for
// the user's own token, asks GitHub who the user is and which repositories the
// App is installed on that the user can reach, throws the user's token away
// (revoking it), and makes a session: a read token limited to exactly those
// repositories, expiring after a while. The GitHub token is never stored and
// never leaves this package. Only the logins listed as administrators get the
// admin role; being an administrator of a GitHub organization gives nothing
// here.
//
// A state value signed by the server and bound to the browser by a cookie keeps
// someone else's authorization code from being put into a victim's session.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"raccoonci/internal/server/store"
)

const (
	cookieName  = "rc_oauth_state"
	stateTTL    = 10 * time.Minute
	maxRepos    = 2000
	defaultTTL  = 12 * time.Hour
	callbackURI = "/v1/auth/github/callback"
)

// Sessions is where sessions are kept (store.Store).
type Sessions interface {
	CreateSession(ctx context.Context, name, user, role, hash string, repos []string, expires time.Time) (store.APIToken, error)
}

// Config for New.
type Config struct {
	ClientID     string
	ClientSecret string
	// PublicURL is the address GitHub sends the browser back to (without a
	// path); the App's callback URL must be PublicURL + "/v1/auth/github/callback".
	PublicURL string
	// Admins are the GitHub logins that get the admin role.
	Admins []string
	// Redirects are the URL prefixes a sign-in may send the browser to when it is
	// done, with the session in the fragment (the frontend's address). Without
	// one the session is the answer, as JSON.
	Redirects []string
	// TTL is how long a session lasts (default 12 hours).
	TTL      time.Duration
	Sessions Sessions
	HTTP     *http.Client
	// WebBase and APIBase are GitHub's (default https://github.com and
	// https://api.github.com); tests replace them.
	WebBase, APIBase string
	Now              func() time.Time
	Log              *slog.Logger
}

// Service is the sign-in.
type Service struct {
	cfg Config
	key []byte // signs the state; this process only, since a state lives minutes
}

// New checks the configuration.
func New(c Config) (*Service, error) {
	if c.ClientID == "" || c.ClientSecret == "" || c.Sessions == nil {
		return nil, errors.New("auth: ClientID, ClientSecret and Sessions are required")
	}
	if !strings.HasPrefix(c.PublicURL, "https://") && !strings.HasPrefix(c.PublicURL, "http://127.0.0.1") && !strings.HasPrefix(c.PublicURL, "http://localhost") {
		return nil, errors.New("auth: PublicURL must be https (GitHub sends the browser back to it)")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.TTL <= 0 {
		c.TTL = defaultTTL
	}
	if c.WebBase == "" {
		c.WebBase = "https://github.com"
	}
	if c.APIBase == "" {
		c.APIBase = "https://api.github.com"
	}
	for i, a := range c.Admins {
		c.Admins[i] = strings.ToLower(strings.TrimSpace(a))
	}
	for _, r := range c.Redirects {
		if !allowedRedirectBase(r) {
			return nil, fmt.Errorf("auth: the redirect %q must be https, or http on localhost", r)
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &Service{cfg: c, key: key}, nil
}

func allowedRedirectBase(r string) bool {
	return strings.HasPrefix(r, "https://") || strings.HasPrefix(r, "http://localhost") || strings.HasPrefix(r, "http://127.0.0.1")
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

// CallbackURL is the address to register as the App's callback URL.
func (s *Service) CallbackURL() string { return s.cfg.PublicURL + callbackURI }

var b64 = base64.RawURLEncoding

type state struct {
	Nonce    string `json:"n"`
	Redirect string `json:"r,omitempty"`
	Expires  int64  `json:"e"`
}

func (s *Service) mac(payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("oauth-state\n" + payload))
	return b64.EncodeToString(m.Sum(nil))
}

func (s *Service) sealState(st state) string {
	p, _ := json.Marshal(st)
	payload := b64.EncodeToString(p)
	return payload + "." + s.mac(payload)
}

var errState = errors.New("the sign-in link is invalid or has expired; start again")

func (s *Service) openState(v string) (state, error) {
	payload, sig, ok := strings.Cut(v, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.mac(payload))) {
		return state{}, errState
	}
	raw, err := b64.DecodeString(payload)
	if err != nil {
		return state{}, errState
	}
	var st state
	if json.Unmarshal(raw, &st) != nil || st.Nonce == "" || s.now().Unix() >= st.Expires {
		return state{}, errState
	}
	return st, nil
}

func (s *Service) redirectAllowed(r string) bool {
	u, err := url.Parse(r)
	if err != nil || u.Fragment != "" || u.User != nil {
		return false
	}
	return slices.ContainsFunc(s.cfg.Redirects, func(prefix string) bool { return strings.HasPrefix(r, prefix) })
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Login sends the browser to GitHub. ?redirect=URL says where to go when it is
// done (a prefix the server was given); without it the session is shown as JSON.
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	redirect := r.URL.Query().Get("redirect")
	if redirect != "" && !s.redirectAllowed(redirect) {
		fail(w, http.StatusBadRequest, "that redirect is not one this server was given")
		return
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		fail(w, http.StatusInternalServerError, "could not start the sign-in")
		return
	}
	n := hex.EncodeToString(nonce)
	st := s.sealState(state{Nonce: n, Redirect: redirect, Expires: s.now().Add(stateTTL).Unix()})
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: n, Path: "/v1/auth/github", MaxAge: int(stateTTL.Seconds()),
		HttpOnly: true, Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"), SameSite: http.SameSiteLaxMode})
	q := url.Values{"client_id": {s.cfg.ClientID}, "redirect_uri": {s.CallbackURL()}, "state": {st}, "allow_signup": {"false"}}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, s.cfg.WebBase+"/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (s *Service) client() *http.Client {
	if s.cfg.HTTP != nil {
		return s.cfg.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// call does one request to GitHub and reads a JSON answer into out (if any).
func (s *Service) call(ctx context.Context, method, rawURL, bearer string, form url.Values, out any) (int, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, errors.New("GitHub's answer could not be read")
		}
	}
	return resp.StatusCode, nil
}

// Session is what a successful sign-in gives back.
type Session struct {
	Token     string    `json:"token"`
	User      string    `json:"user"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
	// Repos are the repositories the session is limited to (none for an
	// administrator).
	Repos []string `json:"repos"`
}

// Callback finishes a sign-in.
func (s *Service) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail(w, http.StatusForbidden, "GitHub did not authorize the sign-in ("+e+")")
		return
	}
	st, err := s.openState(q.Get("state"))
	c, cerr := r.Cookie(cookieName)
	if err != nil || cerr != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(st.Nonce)) != 1 {
		// Without the cookie this browser did not start the sign-in.
		fail(w, http.StatusBadRequest, errState.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/v1/auth/github", MaxAge: -1, HttpOnly: true})
	code := q.Get("code")
	if code == "" {
		fail(w, http.StatusBadRequest, "GitHub sent no code")
		return
	}
	sess, status, err := s.signIn(r.Context(), code)
	if err != nil {
		s.log().Warn("signing in failed", "err", err)
		if status == 0 {
			status = http.StatusBadGateway
		}
		fail(w, status, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if st.Redirect != "" && s.redirectAllowed(st.Redirect) {
		frag := url.Values{"token": {sess.Token}, "expires_at": {sess.ExpiresAt.Format(time.RFC3339)}, "user": {sess.User}, "role": {sess.Role}}
		http.Redirect(w, r, st.Redirect+"#"+frag.Encode(), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sess)
}

// signIn trades the code for who the user is and what they may see.
func (s *Service) signIn(ctx context.Context, code string) (Session, int, error) {
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	form := url.Values{"client_id": {s.cfg.ClientID}, "client_secret": {s.cfg.ClientSecret}, "code": {code}, "redirect_uri": {s.CallbackURL()}}
	if _, err := s.call(ctx, "POST", s.cfg.WebBase+"/login/oauth/access_token", "", form, &tok); err != nil {
		return Session{}, 0, fmt.Errorf("could not reach GitHub to finish the sign-in: %w", err)
	}
	if tok.AccessToken == "" {
		return Session{}, http.StatusBadRequest, fmt.Errorf("GitHub refused the code (%s); start the sign-in again", tok.Error)
	}
	// The user's token is only for the next few requests and is revoked after.
	defer s.revoke(tok.AccessToken)

	var user struct {
		Login string `json:"login"`
	}
	if code, err := s.get(ctx, s.cfg.APIBase+"/user", tok.AccessToken, &user); err != nil || code != 200 || user.Login == "" {
		return Session{}, 0, errors.New("GitHub did not say who you are")
	}
	login := strings.ToLower(user.Login)
	repos, err := s.reachableRepos(ctx, tok.AccessToken)
	if err != nil {
		return Session{}, 0, err
	}
	role := store.RoleRead
	if slices.Contains(s.cfg.Admins, login) {
		role, repos = store.RoleAdmin, nil
	} else if len(repos) == 0 {
		return Session{}, http.StatusForbidden, errors.New("you cannot reach any repository this App is installed on")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, 0, errors.New("could not make a session")
	}
	token := "rcs_" + b64.EncodeToString(raw)
	expires := s.now().Add(s.cfg.TTL)
	sum := sha256.Sum256([]byte(token))
	if _, err := s.cfg.Sessions.CreateSession(ctx, "github:"+login, login, role, hex.EncodeToString(sum[:]), repos, expires); err != nil {
		return Session{}, 0, errors.New("could not keep the session")
	}
	if repos == nil {
		repos = []string{}
	}
	s.log().Info("signed in", "user", login, "role", role, "repos", len(repos))
	return Session{Token: token, User: login, Role: role, ExpiresAt: expires, Repos: repos}, 0, nil
}

// revoke ends the user's GitHub token (DELETE /applications/{client_id}/token,
// with the client's own credentials and the token in a JSON body). A failure is
// only logged: the token is the user's, short-lived, and never stored here.
func (s *Service) revoke(userToken string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"access_token": userToken})
	req, err := http.NewRequestWithContext(ctx, "DELETE", s.cfg.APIBase+"/applications/"+url.PathEscape(s.cfg.ClientID)+"/token", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.SetBasicAuth(s.cfg.ClientID, s.cfg.ClientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := s.client().Do(req)
	if err != nil {
		s.log().Warn("revoking a user's GitHub token failed (it expires by itself)", "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		s.log().Warn("revoking a user's GitHub token failed (it expires by itself)", "status", resp.Status)
	}
}

// get is a GET with the user's token.
func (s *Service) get(ctx context.Context, rawURL, bearer string, out any) (int, error) {
	return s.call(ctx, "GET", rawURL, bearer, nil, out)
}

// reachableRepos lists, lower-cased, the repositories of the App's
// installations that the user can reach.
func (s *Service) reachableRepos(ctx context.Context, userToken string) ([]string, error) {
	var out []string
	for page := 1; page <= 20; page++ {
		var res struct {
			Installations []struct {
				ID int64 `json:"id"`
			} `json:"installations"`
		}
		code, err := s.get(ctx, fmt.Sprintf("%s/user/installations?per_page=100&page=%d", s.cfg.APIBase, page), userToken, &res)
		if err != nil || code != 200 {
			return nil, errors.New("GitHub did not list the installations you can reach")
		}
		for _, in := range res.Installations {
			for rp := 1; rp <= 20; rp++ {
				var rr struct {
					Repositories []struct {
						FullName string `json:"full_name"`
					} `json:"repositories"`
				}
				code, err := s.get(ctx, fmt.Sprintf("%s/user/installations/%d/repositories?per_page=100&page=%d", s.cfg.APIBase, in.ID, rp), userToken, &rr)
				if err != nil || code != 200 {
					return nil, errors.New("GitHub did not list the repositories you can reach")
				}
				for _, r := range rr.Repositories {
					out = append(out, strings.ToLower(r.FullName))
				}
				if len(rr.Repositories) < 100 {
					break
				}
			}
		}
		if len(res.Installations) < 100 {
			break
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	// Only well-formed names go into a token's list (and no pattern: a
	// repository's name has no wildcard).
	clean := out[:0]
	for _, r := range out {
		if !strings.Contains(r, "*") {
			if one, ok := store.NormalizeRepoPatterns([]string{r}); ok {
				clean = append(clean, one...)
			}
		}
	}
	if len(clean) > maxRepos {
		clean = clean[:maxRepos]
	}
	return clean, nil
}

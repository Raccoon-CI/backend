// Command raccoonci-server is the Raccoon CI server: it receives GitHub's
// webhooks, decides which workflows they start, keeps the runs, and serves
// them through an HTTP API. See docs/server-design.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/blob"
	"github.com/Raccoon-CI/raccoonci/internal/dispatch"
	"github.com/Raccoon-CI/raccoonci/internal/instabox"
	"github.com/Raccoon-CI/raccoonci/internal/runnerimages"
	"github.com/Raccoon-CI/raccoonci/internal/server/api"
	"github.com/Raccoon-CI/raccoonci/internal/server/artifacts"
	"github.com/Raccoon-CI/raccoonci/internal/server/auth"
	"github.com/Raccoon-CI/raccoonci/internal/server/caches"
	"github.com/Raccoon-CI/raccoonci/internal/server/config"
	"github.com/Raccoon-CI/raccoonci/internal/server/execute"
	"github.com/Raccoon-CI/raccoonci/internal/server/github"
	"github.com/Raccoon-CI/raccoonci/internal/server/imagesync"
	"github.com/Raccoon-CI/raccoonci/internal/server/instsync"
	"github.com/Raccoon-CI/raccoonci/internal/server/intake"
	"github.com/Raccoon-CI/raccoonci/internal/server/oidc"
	"github.com/Raccoon-CI/raccoonci/internal/server/retention"
	"github.com/Raccoon-CI/raccoonci/internal/server/schedule"
	"github.com/Raccoon-CI/raccoonci/internal/server/secrets"
	"github.com/Raccoon-CI/raccoonci/internal/server/storage"
	"github.com/Raccoon-CI/raccoonci/internal/server/store"
	"github.com/Raccoon-CI/raccoonci/internal/server/vault"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

type listFlag []string

func (l *listFlag) String() string     { return fmt.Sprint(*l) }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "raccoonci-server:", err)
		os.Exit(1)
	}
}

// appInstallations is what instsync asks of GitHub: the App lists its
// installations, the client reads one installation's repositories.
type appInstallations struct {
	app *github.App
	gh  *github.Client
}

func (a appInstallations) Installations(ctx context.Context) ([]github.AppInstallation, error) {
	return a.app.Installations(ctx)
}

func (a appInstallations) InstallationRepos(ctx context.Context, id int64) ([]string, error) {
	return a.gh.InstallationRepos(ctx, id)
}

func defaultDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "raccoonci-server")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "raccoonci-server-data"
	}
	return filepath.Join(home, ".local", "share", "raccoonci-server")
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "replay" {
		return replay(args[1:])
	}
	if len(args) == 2 && args[0] == "-check" {
		return healthCheck(args[1])
	}
	fs := flag.NewFlagSet("raccoonci-server", flag.ExitOnError)
	showVersion := fs.Bool("version", false, "print the version and exit")
	listen := fs.String("listen", "127.0.0.1:18080", "address to listen on (put a TLS-terminating proxy or tunnel in front of it)")
	dataDir := fs.String("data-dir", defaultDataDir(), "where the database lives")
	publicURL := fs.String("public-url", "", "the https address the machines reach this server at (their job assignments, artifacts); needed to run jobs")
	snapshot := fs.String("snapshot", os.Getenv("RACCOONCI_INSTABOX_SNAPSHOT"), "the InstaBox runner snapshot machines start from (env RACCOONCI_INSTABOX_SNAPSHOT); needed to run jobs")
	status := fs.Bool("status", true, "report each run to GitHub as a commit status (raccoonci / <workflow>) on the commit it ran; the token or App needs to write commit statuses")
	imagesEvery := fs.Duration("runner-images-interval", 6*time.Hour, "how often to read what GitHub's hosted runner images preinstall (no token needed); runs are planned with the last good data, else the snapshot built into the binary (0 = never poll)")
	quotaRepo := fs.Int64("quota-repo-minutes", 0, "an allowance of machine time per UTC month for each repository, in minutes (0: none); a run of a repository that used it up fails at once with the reason, and runs start again on the 1st")
	quotaOwner := fs.Int64("quota-owner-minutes", 0, "the same for each owner (user or organization) over all its repositories (0: none)")
	minFreeMB := fs.Int("min-free-mb", 1024, "keep queued runs waiting while the data directory's disk has less than this many MiB free (0 = no check)")
	maxSourceMB := fs.Int("max-source-mb", 2048, "fail a run whose repository source package is bigger than this many MiB")
	oidcOn := fs.Bool("oidc", true, "be an OpenID Connect issuer for jobs with permissions: id-token: write (needs an https -public-url; the issuer is <public-url>/oidc); the signing key is sealed with the master key, or lives in memory without one")
	jobToken := fs.Bool("job-token", true, "with a GitHub App, give each job its own GITHUB_TOKEN: an installation token for the run's repository only, read-only unless the workflow's permissions: say more (never for a pull request from a fork); a plain token is never passed on to jobs")
	checks := fs.Bool("checks", true, "with a GitHub App, also make a check run per job (raccoonci / <workflow> / <job>); needs the App's Checks write permission; a plain token cannot, and gets none")
	localRunner := fs.String("local-runner", "", "run jobs on THIS machine with this raccoonci binary, as this user and without isolation: for development and trusted code only")
	s3Bucket := fs.String("storage-s3-bucket", "", "keep artifacts and caches in this S3-compatible bucket (R2, S3, ...) instead of on this disk; the bucket must be private (it is checked at start)")
	s3Endpoint := fs.String("storage-s3-endpoint", "", "the bucket's endpoint, for an S3-compatible store (R2: https://<account>.r2.cloudflarestorage.com); empty for AWS")
	s3Region := fs.String("storage-s3-region", "", "the bucket's region (R2: auto; default us-east-1)")
	s3Prefix := fs.String("storage-s3-prefix", "", "a prefix for every key in the bucket, to share it with something else")
	s3PathStyle := fs.Bool("storage-s3-path-style", false, "address the bucket as host/bucket/key (most self-hosted stores need it)")
	masterKeyID := fs.String("master-key-id", "1", "the id of RACCOONCI_MASTER_KEY, written beside every secret it seals")
	masterKeyOldID := fs.String("master-key-old-id", "", "the id of RACCOONCI_MASTER_KEY_OLD, the key that sealed secrets before a rotation (they are opened with it, new ones sealed with the current key)")
	artifactDays := fs.Int("artifact-retention-days", 90, "how long an artifact is kept: the default, and the most a workflow may ask for (retention-days); then it is deleted")
	logDays := fs.Float64("log-retention-days", 90, "how long the log of a finished run is kept, in days (0: for ever; fractions work); the run and its result stay, the log answers 410 after that")
	cacheRepoGB := fs.Int("cache-repo-limit-gb", 10, "how much all the cache spaces of one repository may hold together, in GB (0: no cap); checked every 15 minutes, the least recently used go first")
	cacheDays := fs.Int("cache-max-age-days", 7, "a cache entry nobody has restored for this many days is deleted")
	cacheLimitGB := fs.Int("cache-limit-gb", 5, "how much one cache space (the default branch, or one other ref) may hold, in GB; the least recently used go first")
	startsPerMinute := fs.Int("starts-per-minute", 10, "how many InstaBox machines may be started a minute (the organization's limit; 10 on the Pro plan): more wait their turn; 0 = no limit of our own (a refusal from InstaBox is still waited out)")
	schedules := fs.Bool("schedule", true, "fire the runs of `on: schedule:` workflows (on the default branch) for the repositories the server knows; -schedule=false leaves it to GitHub")
	clientID := fs.String("github-client-id", os.Getenv("RACCOONCI_GITHUB_CLIENT_ID"), "the GitHub App's client id, to let people sign in with GitHub (env RACCOONCI_GITHUB_CLIENT_ID; its secret is RACCOONCI_GITHUB_CLIENT_SECRET or _FILE); the App's callback URL must be <public-url>/v1/auth/github/callback")
	authAdmins := fs.String("auth-admins", "", "GitHub logins (comma-separated) that get the admin role when they sign in; everyone else gets a read session limited to the repositories they can reach")
	authHours := fs.Float64("auth-session-hours", 12, "how long a session made by signing in lasts, in hours")
	var authRedirects listFlag
	fs.Var(&authRedirects, "auth-redirect", "an address (prefix) a sign-in may send the browser to afterwards, with the session in the URL's fragment: the frontend's (repeatable; https, or http on localhost)")
	var allow listFlag
	fs.Var(&allow, "allow", "a repository the server works for, owner/name or owner/* (repeatable; at least one)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `usage: raccoonci-server -allow owner/name [flags]

Secrets are read from the environment, or from the file named by NAME_FILE (mode 600), never from flags:
  RACCOONCI_WEBHOOK_SECRET       signs GitHub's deliveries (required)
  RACCOONCI_API_TOKEN            bearer token for /v1 (without it the API answers 503)
  RACCOONCI_GITHUB_APP_ID        with RACCOONCI_GITHUB_APP_KEY_FILE (the App's private key, mode 600): act as a GitHub App
  RACCOONCI_GITHUB_TOKEN         instead of an App, one token (development, with a repository webhook)
  INSTABOX_API_KEY (or IBX_API_KEY)  the InstaBox key machines are made with (with -public-url and -snapshot: run jobs)
  RACCOONCI_STORAGE_S3_KEY, RACCOONCI_STORAGE_S3_SECRET  the bucket's access key (with -storage-s3-bucket)
  RACCOONCI_MASTER_KEY           32 bytes (64 hex digits or base64; openssl rand -hex 32) that seal the secrets in the database; without it secrets are off
  RACCOONCI_MASTER_KEY_OLD       the key from before a rotation (with -master-key-old-id)`)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if len(allow) == 0 {
		fs.Usage()
		return errors.New("no repository is enabled: give -allow owner/name or owner/*")
	}

	webhookSecret, err := config.Required("RACCOONCI_WEBHOOK_SECRET")
	if err != nil {
		return err
	}
	apiToken, err := config.Secret("RACCOONCI_API_TOKEN")
	if err != nil {
		return err
	}
	tokens, authMode, err := githubAuth()
	if err != nil {
		return err
	}
	instaboxKey, err := config.Secret("INSTABOX_API_KEY")
	if err != nil {
		return err
	}
	if instaboxKey == "" { // INSTABOX_API_KEY is reserved on InstaBox machines; a Secret there needs another name
		if instaboxKey, err = config.Secret("IBX_API_KEY"); err != nil {
			return err
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	dbPath := filepath.Join(*dataDir, "server.db")
	warnOpenDirectory(*dataDir, log)
	if err := privateDatabase(dbPath, log); err != nil {
		return err
	}
	st, err := store.OpenSQLite(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	gh := &github.Client{Tokens: tokens}
	hook := &intake.Handler{Secret: []byte(webhookSecret), Store: st, Allow: allow, Log: log, Source: gh}
	if app, ok := tokens.(*github.App); ok {
		hook.Installation = app.InstallationOf // a run started by hand names no installation itself
	}
	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", hook)
	apiSvc := &api.API{Store: st, Dispatcher: hook, Token: apiToken, Version: version, Log: log}
	apiSvc.Checks = []api.Check{{Name: "database", Run: st.Ping}}
	if *minFreeMB > 0 {
		free, need := execute.DiskFree(*dataDir), int64(*minFreeMB)<<20
		apiSvc.Checks = append(apiSvc.Checks, api.Check{Name: "disk", Run: func(context.Context) error {
			if n, err := free(); err != nil {
				return err
			} else if n < need {
				return fmt.Errorf("%d MiB free, %d MiB needed", n>>20, need>>20)
			}
			return nil
		}})
	}
	if *clientID != "" {
		clientSecret, err := config.Secret("RACCOONCI_GITHUB_CLIENT_SECRET")
		if err != nil {
			return err
		}
		if clientSecret == "" {
			return errors.New("-github-client-id is given but RACCOONCI_GITHUB_CLIENT_SECRET (or _FILE) is not")
		}
		var admins []string
		for _, a := range strings.Split(*authAdmins, ",") {
			if a = strings.TrimSpace(a); a != "" {
				admins = append(admins, a)
			}
		}
		signIn, err := auth.New(auth.Config{ClientID: *clientID, ClientSecret: clientSecret, PublicURL: *publicURL, Admins: admins,
			Redirects: authRedirects, TTL: time.Duration(*authHours * float64(time.Hour)), Sessions: st, Log: log})
		if err != nil {
			return fmt.Errorf("signing in with GitHub: %w (it needs an https -public-url)", err)
		}
		apiSvc.Auth = signIn
		log.Info("signing in with GitHub is on", "callback", signIn.CallbackURL(), "admins", len(admins))
	}
	mux.Handle("/", apiSvc.Handler())
	// Secrets need a master key; without one they are off (the routes answer
	// 503 and no run gets a secret), and nothing is ever stored in the clear.
	var secretsSvc *secrets.Service
	if v, err := openVault(*masterKeyID, *masterKeyOldID); err != nil {
		return err
	} else if v != nil {
		secretsSvc = &secrets.Service{Store: st, Vault: v}
		apiSvc.Secrets = secretsSvc
	} else {
		log.Warn("no master key (RACCOONCI_MASTER_KEY): secrets, variables and organizations' own buckets are off")
	}

	// The server's own bucket, if one was given, is tried before it is used.
	// An organization may have one of its own (sealed under the master key);
	// without a master key everything goes to the server's.
	var bucket *blob.S3Config
	if *s3Bucket != "" {
		var err error
		if bucket, err = storageConfig(*s3Bucket, *s3Endpoint, *s3Region, *s3Prefix, *s3PathStyle); err != nil {
			return err
		}
		if err := checkStorage(context.Background(), bucket, log); err != nil {
			return err
		}
	}
	s3For := func(context.Context, string) (*blob.S3Config, string, error) { return bucket, "default", nil }
	if secretsSvc != nil {
		storageSvc := &storage.Service{Store: st, Vault: secretsSvc.Vault, Default: bucket}
		apiSvc.Storage = storageSvc
		s3For = storageSvc.For
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Execution needs three things together: where machines reach the
	// server, the runner snapshot to start them from, and the InstaBox key.
	var executed, janitor, cacheJanitor, imagesDone chan struct{}
	keep := time.Duration(*artifactDays) * 24 * time.Hour
	instaboxMode := *publicURL != "" || *snapshot != "" || instaboxKey != ""
	execution := instaboxMode || *localRunner != ""
	if instaboxMode && *localRunner != "" {
		return errors.New("-local-runner and InstaBox (-public-url, -snapshot, the key) are two ways to run jobs; use one")
	}
	if execution {
		var machine execute.Machine
		var base string
		if *localRunner != "" {
			// Jobs run on this machine, as this user, with no isolation:
			// development, or a machine that exists for this, trusted code only.
			host, _, err := net.SplitHostPort(*listen)
			if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1" && *publicURL == "") {
				return errors.New("-local-runner serves its jobs at the listen address, which must then be a loopback one (127.0.0.1:PORT), or give -public-url")
			}
			base = "http://" + *listen
			if *publicURL != "" {
				base = strings.TrimRight(*publicURL, "/")
			}
			machine = execute.Local{Binary: *localRunner, Dir: filepath.Join(*dataDir, "jobs")}
			log.Warn("jobs run on THIS machine, as this user, without isolation (-local-runner): use it only for code you trust")
		} else {
			if *publicURL == "" || *snapshot == "" || instaboxKey == "" {
				return errors.New("to run jobs on InstaBox give all three: -public-url, -snapshot (or RACCOONCI_INSTABOX_SNAPSHOT) and INSTABOX_API_KEY (or IBX_API_KEY)")
			}
			if !strings.HasPrefix(*publicURL, "https://") {
				return errors.New("-public-url must be https: the machines' tokens travel on it")
			}
			base = strings.TrimRight(*publicURL, "/")
			ctl := &dispatch.Controller{}
			prov := &instabox.Provider{
				Client:        &instabox.Client{Key: instaboxKey, URL: os.Getenv("INSTABOX_API_URL")},
				Controller:    ctl,
				ControllerURL: base + "/agent",
				Snapshot:      *snapshot,
				Limit:         &instabox.StartLimit{PerMinute: *startsPerMinute},
				Logf:          func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...), "from", "instabox") },
			}
			defer prov.Wait() // machines being deleted
			mux.Handle("/agent/", http.StripPrefix("/agent", ctl.Handler()))
			machine = execute.Instabox{P: prov}
		}
		arts, err := artifacts.New(artifacts.Config{Dir: filepath.Join(*dataDir, "artifacts"), PublicURL: base,
			Records: st, Keep: keep, MaxKeep: keep, Log: log,
			Free: execute.DiskFree(*dataDir), MinFree: int64(*minFreeMB) << 20,
			S3For: s3For, RunOrg: func(ctx context.Context, runID int64) (string, error) {
				run, err := st.GetRun(ctx, runID)
				if err != nil {
					return "", err
				}
				owner, _, _ := strings.Cut(strings.ToLower(run.Repo), "/")
				return owner, nil
			}})
		if err != nil {
			return err
		}
		defer arts.Close()
		arts.Mount(mux)
		apiSvc.Artifacts = arts
		janitor = make(chan struct{})
		go func() {
			defer close(janitor)
			arts.Janitor(ctx, time.Hour)
		}()
		cacheSvc, err := caches.New(caches.Config{
			Dir: filepath.Join(*dataDir, "caches"), PublicURL: base, Signer: arts.Signer(), S3For: s3For,
			MaxBytes: int64(*cacheLimitGB) << 30, MaxRepoBytes: int64(*cacheRepoGB) << 30,
			MaxAge: time.Duration(*cacheDays) * 24 * time.Hour, Log: log,
			Spaces: (&caches.Resolver{Runs: st, Branches: gh}).Spaces,
		})
		if err != nil {
			return err
		}
		defer cacheSvc.Close()
		cacheSvc.Mount(mux)
		cacheJanitor = make(chan struct{})
		go func() {
			defer close(cacheJanitor)
			cacheSvc.Janitor(ctx, 15*time.Minute)
		}()
		ex := &execute.Executor{Store: st, Source: gh, Machine: machine, Services: cacheSvc.Extend(arts),
			WorkDir: filepath.Join(*dataDir, "work"), Log: log}
		if *status {
			ex.Reporter = execute.Statuses{GH: gh}
		}
		if *imagesEvery > 0 {
			syncer := &imagesync.Syncer{Store: st, Source: runnerimages.NewFetcher(""), Log: log}
			ex.Images = syncer.Snapshot
			imagesDone = make(chan struct{})
			go func() {
				defer close(imagesDone)
				syncer.Run(ctx, *imagesEvery)
			}()
		}
		ex.Quota = execute.Quota{RepoMinutes: *quotaRepo, OwnerMinutes: *quotaOwner}
		apiSvc.Quota = api.Quota{RepoMinutes: *quotaRepo, OwnerMinutes: *quotaOwner}
		if *minFreeMB > 0 {
			ex.MinFreeBytes, ex.Free = int64(*minFreeMB)<<20, execute.DiskFree(*dataDir)
		}
		ex.MaxSourceBytes = int64(*maxSourceMB) << 20
		if *checks {
			ex.Checks = gh
		}
		if *oidcOn && strings.HasPrefix(*publicURL, "https://") {
			var vlt *vault.Vault
			if secretsSvc != nil {
				vlt = secretsSvc.Vault
			}
			oidcSvc, err := oidc.New(ctx, oidc.Config{Runs: st, Docs: st, Vault: vlt, PublicURL: *publicURL, Log: log})
			if err != nil {
				return err
			}
			oidcSvc.Mount(mux)
			ex.IDTokens = oidcSvc
			log.Info("OIDC issuer on", "issuer", oidcSvc.Issuer())
		}
		if app, ok := tokens.(*github.App); ok && *jobToken {
			ex.Tokens = app
		}
		if secretsSvc != nil {
			ex.Secrets = secretsSvc
		}
		hook.Notify = ex.Wake
		apiSvc.Canceller = ex
		executed = make(chan struct{})
		go func() {
			defer close(executed)
			if err := ex.Run(ctx); err != nil {
				log.Error("the executor stopped", "err", err)
			}
		}()
	}

	srv := &http.Server{
		Addr: *listen, Handler: mux,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 0, IdleTimeout: 2 * time.Minute,
	}
	if err := hook.Recover(ctx); err != nil {
		return fmt.Errorf("looking for deliveries left from before: %w", err)
	}
	// With an App, the table of its installations is read from GitHub at start
	// and then hourly (the installation events keep it current in between).
	var instDone chan struct{}
	if app, ok := tokens.(*github.App); ok {
		instDone = make(chan struct{})
		syncer := &instsync.Syncer{Store: st, GH: appInstallations{app, gh}, Log: log}
		go func() {
			defer close(instDone)
			syncer.Run(ctx, time.Hour)
		}()
	}
	logsDone := make(chan struct{})
	go func() {
		defer close(logsDone)
		(&retention.Janitor{Store: st, LogKeep: time.Duration(*logDays * float64(24*time.Hour)), Log: log}).Run(ctx, time.Hour)
	}()
	var scheduled chan struct{}
	if *schedules {
		// Repositories are known from their events; those named exactly in
		// -allow are watched from the start.
		var extra []string
		for _, a := range allow {
			if !strings.HasSuffix(a, "/*") {
				extra = append(extra, a)
			}
		}
		sch := &schedule.Scheduler{Store: st, Source: gh, Allowed: hook.Allows, Extra: extra, Log: log,
			Notify: func() {
				if hook.Notify != nil {
					hook.Notify()
				}
			}}
		scheduled = make(chan struct{})
		go func() {
			defer close(scheduled)
			sch.Run(ctx)
		}()
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", *listen, "db", dbPath, "allow", []string(allow), "github_auth", authMode, "api", apiToken != "",
		"runs", map[bool]string{true: "executed on InstaBox", false: "queued only (no -public-url, -snapshot and InstaBox key)"}[execution], "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdown)
	hook.Wait()
	if scheduled != nil {
		<-scheduled
	}
	<-logsDone
	if instDone != nil {
		<-instDone
	}
	if janitor != nil {
		<-janitor
	}
	if cacheJanitor != nil {
		<-cacheJanitor
	}
	if imagesDone != nil {
		<-imagesDone
	}
	if executed != nil {
		<-executed // runs in progress are cancelled and recorded
	}
	return err
}

// githubAuth picks how the server calls GitHub, and says which in words (never the credential).
func githubAuth() (github.TokenSource, string, error) {
	appID, err := config.Secret("RACCOONCI_GITHUB_APP_ID")
	if err != nil {
		return nil, "", err
	}
	token, err := config.Secret("RACCOONCI_GITHUB_TOKEN")
	if err != nil {
		return nil, "", err
	}
	switch {
	case appID != "" && token != "":
		return nil, "", errors.New("both a GitHub App and a token are configured; use one")
	case appID != "":
		id, err := strconv.ParseInt(appID, 10, 64)
		if err != nil {
			return nil, "", errors.New("RACCOONCI_GITHUB_APP_ID is not a number")
		}
		keyPath := os.Getenv("RACCOONCI_GITHUB_APP_KEY_FILE")
		if keyPath == "" {
			return nil, "", errors.New("RACCOONCI_GITHUB_APP_KEY_FILE is not set")
		}
		info, err := os.Stat(keyPath)
		if err != nil {
			return nil, "", err
		}
		if config.OthersCanRead(keyPath, info) {
			return nil, "", fmt.Errorf("%s is readable by others; make it 600", keyPath)
		}
		pem, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, "", err
		}
		key, err := github.ParsePrivateKey(pem)
		if err != nil {
			return nil, "", err
		}
		return &github.App{ID: id, Key: key}, "app", nil
	case token != "":
		return github.StaticToken(token), "token", nil
	}
	return nil, "", errors.New("no way to call GitHub: set RACCOONCI_GITHUB_APP_ID and RACCOONCI_GITHUB_APP_KEY_FILE, or RACCOONCI_GITHUB_TOKEN")
}

# Server design (draft)

[中文版](server-design.zh.md) — keep the two in step.

Status: 2026-11-01. Steps 1 and 2 of "Order of work" are built (intake, run records, source, execution on machines, logs; see `docs/server-dev.md` for what runs and what was checked); the rest is not. It is the plan for TODO section C.1, written before the split into separate repositories (raccoonci-frontend, -backend, -runner, -cli, plus a shared library repository). The server is written first, inside this repository, under rules that make the later split a move and not a rewrite (see "Boundaries").

## What the server is for

The own-engine path (`raccoonci run`) works on one machine and in one process. The server makes it a platform: it receives events from GitHub, decides which workflows run, runs them on machines of a compute provider, keeps the results, and shows and reports them. The GitHub-triggered path (`raccoonci serve`, `internal/ghrunner`) does not need it, because GitHub is the server there. It stays as it is and may later run as a module inside the backend; neither path is removed and neither is "the" architecture.

## Boundaries (what makes the split cheap)

- Server code lives under `internal/server/…`. It never imports `cmd/…` and never imports another consumer's code (`ghrunner` is separate).
- It may import only packages that will go to the shared library repository: `value`, `workflow`, `expr`, `trigger`, `plan`, `catalog`, `scan`, `affected`, and the wire types of `dispatch`. Anything else it needs that is not on this list is a signal to decide where it lives before using it.
- Everything that talks to the outside is behind a small interface owned by the server: database (`store`), blob storage (`blob.Store`, exists), compute (`Provider`, exists in `instabox`), source (`Source`), GitHub (`GitHub`). Fakes for tests live next to the interface.
- The HTTP API is the only way the frontend and the CLI talk to the server: JSON over HTTP, one OpenAPI file in the repository, server-sent events for logs. No shared Go types with the frontend.
- The controller↔agent protocol (`internal/dispatch`) is a published protocol with a version number. Today the agent runs `raccoonci run-instance`; after the split the runner repository implements the agent side and the backend the controller side, and they meet only through this protocol.

## Pieces

1. **Event intake.** A GitHub App (webhook events `push`, `pull_request`, `workflow_dispatch` via API, `create`, `delete`; Checks and Contents permissions). Every delivery is verified (HMAC-SHA256, constant time) before it is read, stored as raw payload with its delivery id (so a redelivery is a no-op), then handled. `schedule:` is fired by the server's own cron (the parser is `internal/cron`, a package for the library; `internal/server/schedule` fires the entries).
2. **Trigger matching.** `trigger.Match` per workflow file at the event's commit; changed files from the compare API (unknown means "do not filter"). A matching workflow creates a run. `workflow_dispatch` comes from the API with typed inputs (`trigger.DispatchInputs`).
3. **Runs and jobs.** A run holds the event, the commit, the workflow file at that commit (stored, so a rerun uses the same text) and its job instances. State: queued, in progress, completed, with a conclusion. The first version keeps the engine in process (see "Engine and restarts").
4. **Source.** The commit's tarball through the App installation token, repackaged with `dispatch.TarDir`'s layout so the agent receives what it receives today. For a pull request it uses the merge ref (`refs/pull/N/merge`), which GitHub makes a moment after the event; the server retries for a short time and then falls back to the head commit and says so in the run. Merging the base into the head itself is out of scope.
5. **Execution.** `engine.Run` with `Options.Remote` set, exactly like `run -on instabox` today, but the controller, artifact/cache services and provider belong to the server, not to a command. The agent protocol and `instabox.Provider`/`Pool` are reused.
6. **Logs and results.** Every `runner.Event` of an instance is appended to storage (chunked, ordered, masked already by the runner) and published to subscribers. Live view is server-sent events from a chunk offset; finished logs are plain reads. Job outputs and the result go to the database.
7. **Artifacts and caches.** `internal/artifact` and `internal/cache` get a persistent metadata store (database instead of memory and `index.json`) and an S3-compatible blob store, which **the user provides**, per organization: Cloudflare R2, AWS S3 or anything S3-compatible (`blob.S3` with `Endpoint` and `PathStyle`). The project does not host anyone's data and has no default production bucket. Without a configured bucket the server falls back to a local directory (`blob.Dir`), which is enough for one machine and for trying it out. Settings are stored as secrets (item 9), and saving them runs a check: put, presign, get and delete a small object, with a clear error if one step fails. Our own development setup uses the maintainer's R2 bucket through environment variables; that is a test fixture, not a default. Downloads are presigned links, so they bypass the server (and R2 charges no egress). Retention is done by the server's own deletes; it must not depend on lifecycle rules the user may not have set. Uploads still pass through the server, because the toolkit speaks Azure Blob's block protocol and cannot be handed an R2 URL; the host that receives them must not sit behind Cloudflare's proxy (100 MB request limit on the free plan). This is the "shelved until a server exists" list in TODO B.3: persistence, retention, per-branch cache scope, one writer per index.
8. **Reporting to GitHub.** One check run per job instance through the Checks API (status, conclusion, title, summary from `$GITHUB_STEP_SUMMARY`, annotations when problem matchers exist). Check runs are updated at most about once a few seconds.
9. **Secrets and variables.** Encrypted at rest with an envelope key from the server's configuration, per organization and repository; decrypted only when an instance is dispatched; never in logs or API responses; the machine never gets bucket or provider keys. Fork pull requests get no secrets.
10. **Auth.** Users sign in with GitHub (OAuth on the App); the API checks that the user may see the repository through GitHub. The CLI uses a personal API token. Machines use the per-assignment token of `dispatch`.
11. **API.** Runs, jobs, logs, artifacts, rerun, cancel, workflow dispatch, secrets (write-only), repositories enabled. The frontend and the CLI are clients of this and nothing else.

## Object storage safety

- The bucket is private. Never public read, no `r2.dev` public URL, no custom domain on it. The configuration check (item 7) tries to detect a publicly readable bucket and warns.
- Machines never hold bucket credentials. They get a presigned link only after the server has checked their token, with a lifetime of minutes (today's code uses one hour in `downloadTTL`; the server lowers it), and links are never logged.
- Keys are prefixed by repository, so one repository cannot read another's artifacts or caches.
- Caches are scoped by branch and by trust: a pull request from a fork can neither write a cache that a trusted ref later restores nor read the caches of protected branches it should not see (the "per-branch scope" item of TODO B.3 is mandatory here, not shelved).
- Upload links are bound to one job's token and to their own realm (`~cache` for caches), so an upload link cannot write anywhere else.

## Development setup

While the server is developed on a sandbox without an inbound address, the webhook and agent endpoints are reached through a Cloudflare Tunnel (`cloudflared` on the sandbox, one hostname on a zone the maintainer owns). The tunnel, its DNS record and its token are created for debugging only and deleted when it ends. A tunnel goes through Cloudflare's proxy, so requests above 100 MB (large cache uploads) fail there; that limit does not exist on the production setup (a VPS with a DNS-only host name for uploads).

## Engine and restarts

`engine.Run` is a goroutine graph in memory. The first server version keeps it: one goroutine per run, instances sent out through `Options.Remote`, events stored as they come. If the server restarts, runs in progress are marked failed with the reason "server restarted" and their machines are deleted (machines have `deleteAfterSeconds`, so none leak). A database-driven scheduler that can resume a run is the second step and is only worth it when restarts during runs hurt. This also means the first version runs as a single instance.

## Storage

One `store` interface with the operations the server needs (events, runs, instances, log chunks, artifacts and cache metadata, secrets, installations), implemented first on SQLite (pure Go driver, so the binary stays static and a self-hoster needs nothing else), with PostgreSQL as the second implementation once the server needs more than one instance. Migrations are numbered SQL files embedded in the binary. Both implementations run the same test suite.

## Order of work

1. **Skeleton and intake.** Config, database and migrations, webhook endpoint with verification, event and run records, `GET` runs through the API. No execution. Test: replay recorded deliveries.
2. **Execution.** Source fetch, engine with the dispatch controller and InstaBox provider, logs stored and streamed. First end-to-end: push to a test repository, run appears, job runs on a machine, log is readable. Test: this repository's own `ci.yml`.
3. **Reporting.** Check runs for jobs, rerun and cancel.
4. **Services made persistent.** Artifacts and caches in the database and S3.
5. **Secrets and auth.** Needed before anyone else uses it.
6. **Schedules and dispatch.** Cron (`internal/server/schedule`), `workflow_dispatch` with inputs (`POST /v1/runs`). Done.
7. **Hardening.** Restart behaviour, quotas, retention, workdir clean-up (TODO C.1).

The frontend starts after step 2, when the API has runs and logs to show. Until then it is not worth writing against an API that does not exist.

## Decisions taken here, easy to change before step 1 is built

- Go, one binary, SQLite first and PostgreSQL second.
- Engine in process for the first version; runs interrupted by a restart are failed.
- GitHub App as the only way in (no polling in this path).
- Documentation in two versions, English and Chinese, and the API described in English, because the repository will be open source.

## Needs the user

- A GitHub App has to be created and installed (name, webhook URL and secret, permissions, private key). The key and the webhook secret are given to the server through environment variables or files that are never printed or committed; I will not ask for them in chat.
- A public address for the webhook, as with `serve -webhook-url`: a VPS (Singapore) with a domain and HTTPS, two host names (one behind Cloudflare's proxy for the webhook and the API, one DNS-only for the agent and upload endpoints).
- For development only: the maintainer's R2 bucket and an S3 API token for it, given to the server through environment variables or files. Users of the finished server bring their own.
- Where the server will really run (it needs outbound access to GitHub and InstaBox and an inbound port).

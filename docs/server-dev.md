# Running the server while developing

[中文版](server-dev.zh.md) — keep the two in step.

State: steps 1 and 2 of `docs/server-design.md`. The server receives GitHub's `push` and `pull_request` deliveries (or a request to start a workflow by hand), decides which workflows they start, stores a queued run for each, fetches the repository at the commit, runs every job instance on a machine, stores the log, and shows runs and logs through the API. It reports each run back to GitHub as a commit status, and can cancel and repeat runs. It keeps secrets and variables (sealed in the database) and gives runs the ones they name, and has API tokens with roles. It does not yet send check runs (those need a GitHub App) or log users in with GitHub.

## Build and start

```sh
make build                       # dist/raccoonci and dist/raccoonci-server
raccoonci-server -allow owner/name [-allow owner/*] [-listen 127.0.0.1:18080] [-data-dir DIR] [how to run jobs]
```

At least one `-allow` is needed: a delivery or request for any other repository is refused or forgotten. The database is `DIR/server.db` (SQLite, migrated at start). Deliveries a restart interrupted are handled again at start; runs that were in progress are queued again (see Restarts).

**How to run jobs** (without one of these, runs stay queued):

- InstaBox: `-public-url https://…` (where machines reach the server), `-snapshot ID` (the runner snapshot; env `RACCOONCI_INSTABOX_SNAPSHOT`) and the InstaBox key in `INSTABOX_API_KEY` or `IBX_API_KEY`. Each job instance gets a machine of its own; the machine's agent calls back at `<public-url>/agent`.
- `-local-runner PATH-TO-raccoonci`: jobs run **on the machine the server runs on, as the same user, with no isolation**. For development, or a machine that exists for this, with code you trust. Jobs reach the server at the listen address, so it must be a loopback one unless `-public-url` is given.

## Secrets

Each is an environment variable `NAME`, or `NAME_FILE` naming a file that only you can read (mode 600; a looser file is refused). They are never flags.

| Name | Meaning |
|------|---------|
| `RACCOONCI_WEBHOOK_SECRET` | signs GitHub's deliveries (required) |
| `RACCOONCI_API_TOKEN` | the bootstrap bearer token for `/v1`, with the admin role; others are made through the API |
| `RACCOONCI_MASTER_KEY` | 32 bytes (`openssl rand -hex 32`) that seal the secrets in the database; without it secrets and variables are off. Losing it loses the secrets |
| `RACCOONCI_MASTER_KEY_OLD` | with `-master-key-old-id`: the key from before a rotation, so older secrets still open (a new one is sealed with the current key, `-master-key-id`) |
| `RACCOONCI_GITHUB_APP_ID`, `RACCOONCI_GITHUB_APP_KEY_FILE` | act as a GitHub App (installation tokens from the App's private key) |
| `RACCOONCI_GITHUB_TOKEN` | instead of an App: one token, for development with a repository webhook |
| `INSTABOX_API_KEY` / `IBX_API_KEY` | the InstaBox key (the first name is reserved on InstaBox machines) |

## Telling GitHub

A repository webhook (development) or the GitHub App (the real thing) points at `https://<your host>/webhooks/github`, content type `application/json`, secret = `RACCOONCI_WEBHOOK_SECRET`, events `push` and `pull_request`. How to register the App: [github-app.md](github-app.md). While developing on a sandbox with no public address, the address is a Cloudflare Tunnel to `127.0.0.1:18080`.

## Starting a run

Without a webhook, ask the API (the workflow needs a `workflow_dispatch` trigger):

```sh
curl -H "Authorization: Bearer $TOKEN" -X POST https://HOST/v1/runs \
  -d '{"repo":"owner/name","ref":"main","workflow":".github/workflows/ci.yml","inputs":{}}'
```

or replay a recorded delivery, signed as GitHub does (`RACCOONCI_WEBHOOK_SECRET_FILE=… raccoonci-server replay -url … -delivery d1 payload.json`; the same `-delivery` again must answer `duplicate`).

## Reading the result

```sh
GET /v1/runs                       # newest first
GET /v1/runs/{id}                  # with the workflow file it started from
GET /v1/runs/{id}/jobs             # the job instances and how each ended
GET /v1/runs/{id}/log?after=N      # the log, a page at a time ("done" says when it is over)
GET /v1/runs/{id}/log              # with Accept: text/event-stream: the log as it grows, then an "end" event
GET /v1/events/{id}                # a delivery and what was decided for each workflow file
POST /v1/runs/{id}/cancel          # a queued run ends at once; a running one is told to stop (and its machine is deleted)
POST /v1/runs/{id}/rerun           # a new run like a finished one: same commit, same workflow file as it was, same inputs
```

## Secrets, variables and tokens

All of this needs the admin role. A scope is `org:<name>` or `repo:<owner>/<name>`, in lower case.

```sh
PUT    /v1/secrets   {"scope":"repo:acme/web","name":"DEPLOY_TOKEN","value":"…"}   # sealed; cannot be read back
GET    /v1/secrets?scope=repo:acme/web                                              # names only
DELETE /v1/secrets?scope=repo:acme/web&name=DEPLOY_TOKEN
PUT/GET/DELETE /v1/vars                                                             # the same, for variables, which can be read back
POST   /v1/tokens    {"name":"dashboard","role":"read"}                              # the token is in the answer once
GET    /v1/tokens    DELETE /v1/tokens/{id}                                          # list; revoke
```

- A secret is sealed (AES-256-GCM, under the master key, bound to its scope and name) before it reaches the database, and opened only when a run is about to be sent to a machine. It travels in the job's assignment, over https, and the runner masks it in what steps print. It is never in the log, the API's answers or an error.
- A run gets the secrets its workflow names literally (`secrets.NAME`, `secrets['NAME']`), the repository's over the organization's of the same name. A secret reached by a computed name, or by passing all of `secrets` on, is not sent. Names are kept in capitals; names starting with `GITHUB_` are not allowed.
- A pull request from another repository (or one whose fork was deleted) gets **no secrets**, and its log says so. It still gets variables.
- A `read` token reads runs, jobs, logs and events; an `admin` token also starts, cancels and repeats runs and manages secrets, variables and tokens. Only a hash of a token is kept.
- `GITHUB_TOKEN` (`secrets.GITHUB_TOKEN`, `github.token`) is made by the server **only with a GitHub App** (`-job-token`, default on): each job instance gets an installation token of its own for the run's repository alone, revoked as soon as the job ends (and expiring in an hour at the latest). Its permissions are the job's `permissions:`, else the workflow's, else the server's default, **read-only contents** (GitHub's own default may be read and write); a mapping names what it wants and the rest is none, `read-all`/`write-all` work, and names an App token cannot have (`id-token`...) are dropped. If the App lacks a permission the job asks for, the job gets the default token and a warning in its log; if no token can be made it runs without one, also with a warning. A pull request from a fork gets none, like it gets no secrets. A plain token (`RACCOONCI_GITHUB_TOKEN`) is never passed on to jobs. A secret of that name cannot be stored. Not tried against real GitHub: no App is registered yet.

## Status on GitHub

A run sets the commit status `raccoonci / <workflow name>` on the commit it ran: `pending` ("Running") when it starts, then `success`, `failure` or `error` ("Cancelled") with how long it took. It shows on the commit and, for a pull request, in its checks. It works with a plain token (needs the `repo:status` ability) and with a GitHub App (needs the "Commit statuses" permission); `-status=false` turns it off. A failed report is logged and never fails the run. With a GitHub App each job instance also gets a check run `raccoonci / <workflow name> / <job>` (`in_progress` when it starts, then `success`, `failure` or `cancelled`); it needs the App's Checks write permission, a plain token cannot write check runs and gets none, and `-checks=false` turns it off. Not done: annotations and the step summary in the check run. Not tried against real GitHub, because no App is registered yet.

`/v1/openapi.yaml` describes all of it; a test keeps it in step with the routes.

## Schedules

`on: schedule:` workflows are fired by the server (`-schedule=false` leaves them to GitHub). As on GitHub, only the workflow files on the repository's **default branch** count, and a run is for the newest commit of that branch when the minute comes (`github.event_name` is `schedule`, `github.event.schedule` the cron entry that fired, the actor `schedule`). Cron is UTC, five fields, with lists, ranges and steps; a workflow with two entries gets a run per entry that fires.

- Which repositories are looked at: those the server has had an event for (a delivery or a dispatch) and those named exactly in `-allow` (not `owner/*`: a repository of a wildcard is known from its first event). What is known about a repository's schedules is kept 5 minutes; when a minute comes the workflow is read again at the branch's newest commit and the entry must still be in it.
- A fire is recorded as an event with an id made of the repository, the file, the entry and the minute, so a restart (or two servers on one database) cannot start it twice. A fire that passes while the server is down is not made up for, and after a stall of several minutes one run is made, not a burst. GitHub itself may run schedules late or drop them under load; here they are on time to within the 15-second look.
- Not done: GitHub's disabling of the schedules of a repository with no activity for 60 days; a repository nobody has sent an event for is not found unless it is in `-allow`.

## Concurrency groups

A workflow's `concurrency:` (a group name, or `group` with `cancel-in-progress`; expressions may use `github`, `inputs` and `vars`) is honoured per repository, the group name compared without regard to case. In a group one run executes and one waits: a run that arrives while the group is taken waits (its log says `waiting for run N`), and when another run arrives behind it, the one waiting is cancelled (`a newer run of the same concurrency group replaced this one`), as on GitHub. With `cancel-in-progress` true the run in the group is cancelled at once. A cancelled waiting run gives its place up. The source is fetched after the wait. Limits: only the workflow-level key (a job-level `concurrency:` is parsed but ignored), the state lives in the server process (single instance; after a restart every run enters its group again), a waiting run keeps one of the `MaxRuns` slots, and it was tested against a fake machine only.

## Start limit

InstaBox limits how many machines an organization starts a minute (10 on the Pro plan) and refuses more with 429. `-starts-per-minute` (default 10, 0 = no count of our own) makes the server count its starts in a sliding 62-second window and make the next one wait its turn, so a burst of runs is spread out instead of failing; after a 429 all starts wait 20 seconds and the refused one is asked again (up to about 7 minutes). The log says `waited … for the machine start limit`. The count is the server's own: starts by other users of the same InstaBox organization are only noticed through a 429.

## Log retention

The log of a finished run is kept 90 days (`-log-retention-days`, fractions work, 0 keeps it for ever); a sweep at start and every hour deletes the lines of runs that ended earlier. The run, its jobs and its result stay. Its `log_purged` is then true, and `GET /v1/runs/{id}/log` (and the event stream) answer **410 Gone** instead of an empty log. Not done: the raw payloads of old webhook deliveries and the workflow texts of old runs are kept (a rerun reads them), and SQLite keeps the freed pages for reuse rather than shrinking the file.

Disk protection: while the disk holding `-data-dir` has less than `-min-free-mb` (default 1024, 0 = no check) available, the server takes no new run from the queue; it logs one warning and goes on by itself when space is back. A run whose source package is bigger than `-max-source-mb` (default 2048) fails with a message saying so; the download is cut while it is written, and nothing is left in the work directory. The artifact service uses the same limit: with the disk this low it answers a new artifact with 429 (`resource_exhausted`), and accepts them again when space is back; uploads already started are not cut. Not done: quotas per repository or organization.

Runner images: at start and every `-runner-images-interval` (default 6 h, 0 = never) the server reads what GitHub's hosted images preinstall (the same sources as `raccoonci runner-images update`, no token needed) into the database. Runs are planned with the built-in snapshot with that data laid over it; an image that cannot be read keeps its last good data. Only the server's own planning uses it today: the machines run with the snapshot built into their binary.

## Artifacts

`actions/upload-artifact` and `download-artifact` (v4) work, and a finished artifact is kept in the database (its name, size, job, the time it expires) beside its data in the store, so a restart does not forget it. What a user does with them:

```sh
GET /v1/runs/{id}/artifacts                       # [{id, name, job, size, digest, created_at, expires_at}]
GET /v1/runs/{id}/artifacts/{artifact}/download   # 302 to a link that works for an hour, straight to the storage
```

Both need a token with the `read` role. An artifact is kept 90 days, or less if the step says `retention-days`; `-artifact-retention-days N` changes the default and caps what a step may ask for. A sweep every hour (and at start) deletes what has expired, data and record. Notes:

- Each artifact service (the server's disk, and every organization's bucket) has its own records. If an organization changes its bucket, the artifacts in the old one are no longer listed, and when their time is up the records are forgotten but the objects stay in the old bucket: delete them there if that matters.
- An upload that was never finished is not remembered; if the server stops in the middle of one, the pieces already in the bucket stay there (a bucket lifecycle rule for incomplete multipart uploads cleans them; the server does not).
- The client's sha256 is stored and shown, not verified. No artifact v3 (the older REST protocol).

## Restarts

A run that was in progress when the server stopped is started again **from the beginning**, not resumed: on SIGTERM each run in progress is cancelled (its machines are deleted, which takes a few seconds) and goes back to the queue, and after a crash the runs left in progress are queued at the next start. The run's log gets a warning ("the server stopped while this ran; it is started again … attempt N"), the earlier attempt's lines stay in it, and GitHub's status stays `pending` meanwhile. At every start, before any run is taken, the server also deletes what a run that died left on its disk: the work directories (`work/`), the local runner's job directories (`jobs/`) and the half-received blocks of uploads (`artifacts/spool`, `caches/cache-spool`). This assumes one server per data directory. A run is taken at most 3 times; after that it ends as failed. A run someone cancelled stays cancelled. After a crash the machines of the old attempt are not known any more; they are deleted by InstaBox at their 2-hour deadline.

## Caches and where objects live

`actions/cache` and the `cache:` option of the `setup-*` actions work (protocol v1). A cache is not shared by everything: each run saves into the **space of its own ref** and restores from that and from the **default branch's** space.

- The default branch has a space only its own runs write.
- Every other ref (a feature branch, a tag, a pull request's merge ref, from a fork or not) has a space of its own, restores from the default branch's too, and can neither read another ref's nor write to the default branch's. A pull request that saves a cache under the key the default branch uses lands in its own space; the default branch keeps its entry.
- Repositories never share. Unlike GitHub, a pull request cannot restore from its base branch unless that is the default branch.
- Three limits. Each space is limited to `-cache-limit-gb` (default 5), checked at every upload: the least recently used entries go first. All the spaces of one repository together are limited to `-cache-repo-limit-gb` (default 10, 0 = none), and an entry nobody has restored for `-cache-max-age-days` (default 7, as on GitHub) is deleted; both are done by a sweep at start and every 15 minutes, so a repository can be over its cap for up to that long. The sweep deletes the least recently used entries of a repository across its spaces, and removes a space left with nothing in it (never one with an upload in progress). The time of a restore is saved to the disk at most every 5 minutes and when the service closes, so a restart does not make a busy entry look unused.
- A space made before the sweep existed is known by its directory only; the sweep treats it as on this disk if it has an `objects` directory and leaves it alone otherwise (the organization whose bucket it is in is not written down). Newer spaces have an `org` file.

Objects (artifacts and caches) live in a directory under `-data-dir`, or in a bucket the user provides: `-storage-s3-bucket B [-storage-s3-endpoint URL] [-storage-s3-region R] [-storage-s3-prefix P] [-storage-s3-path-style]`, with `RACCOONCI_STORAGE_S3_KEY` and `RACCOONCI_STORAGE_S3_SECRET`. For Cloudflare R2: endpoint `https://<account id>.r2.cloudflarestorage.com`, region `auto`. At start the bucket is tried (write, read back through a presigned link, delete) and the server refuses to start with one that does not work; a bucket that gives its objects to anyone without credentials is a warning in the log. Keep the bucket private. Downloads are presigned links straight to the bucket; uploads still pass through the server. That bucket is the server's own, for organizations that have none of their own.

**A bucket per organization** (what the users of the finished server do): an admin gives it through the API, and the artifacts and caches of that organization's repositories then go there, not to the server's.

```sh
PUT    /v1/storage  {"org":"acme","bucket":"acme-ci","endpoint":"https://<account>.r2.cloudflarestorage.com","region":"auto","prefix":"raccoon","access_key":"…","secret_key":"…"}
GET    /v1/storage?org=acme        # where it is, never the keys
DELETE /v1/storage?org=acme        # back to the server's own storage
```

The bucket is tried before its settings are kept (write, read back through a presigned link, delete): one that does not work is refused with the step that failed, one that anyone can read is kept with a warning. The settings and keys are sealed under the master key, in a row only the server reads: a workflow cannot name them, and the secrets API can neither list, overwrite nor delete them. Changing the bucket makes new artifacts and caches go to the new one; what is in the old one stays there, and a cache entry whose object is not in the new bucket is dropped when it is looked up. Needs the master key. Artifacts and caches of an organization with its own bucket go to separate services made on demand.

## What a run is, for now

- The commit is the one the event is about. For a `pull_request` run the source is GitHub's merge of the head into the base (`refs/pull/N/merge`, as GitHub's own runners see it); the ref appears a moment after the event, so it is asked for up to 4 times, 2 s apart, and when it never comes (the two conflict) the **head** is run instead and the log says so. The `github.sha` value is still the head commit, not the merge commit's id.
- The source comes from GitHub's archive of the commit: no `.git` directory, no history.
- `GITHUB_TOKEN` exists only with a GitHub App (see Secrets). Artifacts and caches work (see the next sections).
- `github.run_number` counts the workflow's runs started by an event, as on GitHub (per repository and workflow file); a rerun keeps the number of the run it repeats and raises `github.run_attempt` (1, 2, ...). `github.run_id` is the server's run id (a rerun has a new one, unlike GitHub).

## Checked so far

- Unit tests (`-race`): signature, redelivery, allow list, activity types, forks, filters, recovery after a restart; the store; the executor with a fake machine (outputs flow between jobs, a failing job stops what needs it, machine errors, cancellation on shutdown, the run limit); the source archive repack through the real extractor; the API, including the SSE stream and a test that the spec and the routes agree.
- A signed replay of a real push of `cli/cli` through a Cloudflare Tunnel with the real GitHub API: 15 workflow files read, 3 runs created, the same three GitHub ran for that commit.
- **A whole run, end to end** (`-local-runner`): `POST /v1/runs` through the tunnel for `bench-lint-docs`' workflow; the server fetched the real archive from GitHub, resolved the job, ran it with the real `raccoonci run-instance` (checkout, `setup-node`, three `run` steps), stored 62 log lines and a successful job, and the API served them; 4 s in all. The same log streamed live over the tunnel as server-sent events.
- **The same run on an InstaBox machine** (runner snapshot `raccoonci-runner-9c482bc`, reached through the tunnel at `/agent`): success, the machine up in 9.8 s, 16 s from the request to the end of the run, the same steps in the log, and the machine was gone afterwards.
- **Deliveries sent by GitHub itself** (a repository webhook on `bench-lint-docs` through the tunnel): the ping got 200; two pushes and a pull request being opened and closed arrived signed, were accepted with 202, read the repository's workflow at the commit with the real API, and were recorded as ignored (the repository's only workflow listens to `workflow_dispatch`). The token lacked the `workflow` scope, so no push-triggered workflow could be added to see a run created from a real delivery.
- **Status, cancel, rerun on InstaBox:** a run's commit showed `pending` then `success` ("Succeeded in 27s") on GitHub; a run cancelled 5 s in ended as cancelled with status `error`; a rerun repeated the first (`rerun_of`) and succeeded. The cancelled run exposed a leak: the machine was still being created, the delete was refused (409 `operation_conflict`) and given up on, so it ran until its 2 h deadline. `instabox.Client.DeleteMachine` now waits and asks again; checked live (no machine left after a cancel during creation).
- **Caches in real jobs on InstaBox machines** (`bench-go-integration`, `setup-go` with `cache: true`, a postgres service container): the first run found no cache and saved 44 MB through the server; a rerun restored it ("Cache restored from key …", "Cache hit occurred on the primary key"); a run on a new branch restored it too, through the default branch's space; only the default branch's space exists on disk; no machine was left afterwards. The isolation rules are checked by tests with several runs (feature branches, a pull-request ref, another repository, a restart), which fail when the isolation is removed.
- **Secrets:** through the tunnel, a secret and a variable were stored and listed (the secret by name only), a `read` token got 200 on runs and 403 on secrets, a revoked token stopped working. With the real runner (`RACCOONCI_TEST_BIN=dist/raccoonci go test ./internal/server/execute -run Real`): a step printed `token=[***]` for the secret and `region=[eu]` for the variable, the secret's value appeared nowhere in the log, and a fork's run printed an empty token. Not run on a real GitHub-triggered workflow that uses a secret (no workflow file can be added with the token at hand).
- **Start limit on InstaBox** (`-starts-per-minute 2`, four runs of `bench-lint-docs` started at once): two machines were up in 9–11 s, the other two waited 1 m 0.5 s and 1 m 1.5 s for the limit and then started; all four runs succeeded and no machine was left. A real 429 from InstaBox was not seen (the retry after one is tested against a fake API only).
- **Restarts on InstaBox:** a run killed with the server mid-run (the server was stopped while its machine was being created) was queued again at the next start and succeeded on attempt 2; a SIGTERM during a run took about 15 s to finish (the machine is deleted first), logged `queued again`, and the run succeeded on attempt 2 after the restart. Both were `bench-lint-docs`, a few seconds into the run; a restart late in a long run was not tried.
- **Log retention** (`-log-retention-days 0.01`, on the dev database): the 20 runs older than the 14 minutes lost their 766 log lines at start, their log routes answered 410 and the run showed `log_purged: true` with its conclusion; a run 12 minutes old kept its log. Back on the default 90 days afterwards.
- **Artifact records:** tested against SQLite with the real upload protocol (upload, restart, list, download, expiry sweep); live only for the migration on the existing database and the three routes (200 with an empty list, 404, 404). No job that uploads an artifact was run on the server: none of the benchmark workflows does, and the token cannot add one.
- **Schedules:** unit-tested only (the clock is passed in). No repository at hand has a `schedule:` workflow and the token cannot add one, so a real fire was not seen.
- **Not tried:** a bucket (no access key for the dev R2 bucket was given, so the S3 path of artifacts and caches and the bucket check are tested only against fakes), artifacts uploaded by a real job, a run created from a delivery GitHub sent, a real pull request, App authentication against real GitHub, artifacts from a real job, a run with a matrix.

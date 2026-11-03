# backend

[中文](README.zh.md)

The server of [Raccoon CI](https://github.com/Raccoon-CI), a self-hosted CI/CD platform that moves GitHub Actions workflows onto compute you choose. `raccoonci-server` receives GitHub's webhooks (or a request to start a workflow by hand), decides which workflows run, runs every job instance on a machine of a compute provider (InstaBox first), and keeps the runs, logs, artifacts and caches. It reports back to GitHub as commit statuses and check runs, and serves everything through a JSON API.

This repository holds `cmd/raccoonci-server`, `internal/server/*`, the deployment files in `deploy/` and the server docs in `docs/`. Module: `github.com/Raccoon-CI/backend`. Licence: Apache-2.0 (see `LICENSE`; the licences of what is linked in are in `THIRD_PARTY_NOTICES`).

## The API contract (for the frontend)

The frontend is written against **[`internal/server/api/openapi.yaml`](internal/server/api/openapi.yaml)**. The server serves the same file at `GET /v1/openapi.yaml`, and a test (`internal/server/api`) fails when a route is missing from it, so the file is the contract. Changing a route means changing that file in the same commit.

## Build, test, run

```sh
make build          # dist/raccoonci-server (VERSION=1.2.3 sets the version)
make check          # go vet + go test -race ./...
raccoonci-server -allow owner/name [-allow owner/*] [-listen 127.0.0.1:18080] [-data-dir DIR] [how to run jobs]
```

At least one `-allow` is needed (a delivery for any other repository is refused). The database is `DIR/server.db` (SQLite, migrated at start). Without a way to run jobs, runs stay queued:

- InstaBox: `-public-url https://…` (where machines reach the server), `-snapshot ID` (the runner snapshot; or `RACCOONCI_INSTABOX_SNAPSHOT`) and the InstaBox key in `INSTABOX_API_KEY` or `IBX_API_KEY`.
- `-local-runner PATH`: jobs run on the server's own machine, as its user, with **no isolation** — development and trusted code only. `PATH` is a `raccoonci` binary built from the cli repository.

Environment variables (secrets: each as `NAME` or `NAME_FILE` naming a file only you can read, mode 600; never flags):

| Name | Meaning |
|------|---------|
| `RACCOONCI_WEBHOOK_SECRET` | signs GitHub's deliveries (required) |
| `RACCOONCI_API_TOKEN` | bootstrap bearer token for `/v1` (admin); others are made through the API |
| `RACCOONCI_MASTER_KEY` | 32 bytes (`openssl rand -hex 32`) sealing secrets in the database; without it secrets and variables are off. `RACCOONCI_MASTER_KEY_OLD` for a rotation |
| `RACCOONCI_GITHUB_APP_ID`, `RACCOONCI_GITHUB_APP_KEY_FILE` | act as a GitHub App (check runs, per-job `GITHUB_TOKEN`) |
| `RACCOONCI_GITHUB_TOKEN` | instead of an App: one token, for development |
| `RACCOONCI_GITHUB_CLIENT_ID`, `RACCOONCI_GITHUB_CLIENT_SECRET` | sign in with GitHub (OAuth) |
| `INSTABOX_API_KEY` / `IBX_API_KEY` | the InstaBox key |
| `RACCOONCI_INSTABOX_SNAPSHOT` | the runner snapshot (same as `-snapshot`) |
| `RACCOONCI_STORAGE_S3_*` | the server's own S3 bucket for artifacts and caches |
| `RACCOONCI_CATALOG_URL`, `RACCOONCI_CATALOG_KEY` | the catalog feed and the public key(s) that must have signed it (defaults built in) |

`raccoonci-server -h` lists every flag. Details: [docs/server-dev.md](docs/server-dev.md) (running it while developing), [docs/deploy.md](docs/deploy.md) (systemd or a container, reverse proxy, backups), [docs/github-app.md](docs/github-app.md) (registering the GitHub App), [docs/server-design.md](docs/server-design.md) (the design). `deploy/` has `Dockerfile`, `raccoonci-server.service` and a `Caddyfile`.

## Other repositories

| Repository | Holds |
|---|---|
| lib | the shared library (`pkg/*`: workflow, expr, trigger, catalog, dispatch, …) — **this repository depends on it** |
| raccoonci-runner | the machine side: the engine and `raccoonci-runner` |
| cli | the `raccoonci` command line |
| frontend | talks to this server through `openapi.yaml` |

Dependency direction: lib ← runner ← cli, and lib ← backend. The backend never imports the runner or the cli (a test checks it); it meets the machine's agent only through the `dispatch` protocol.

## Dependencies

`go.mod` requires `lib v0.1.0`, which is published, so `go build ./...` and the CI (`.github/workflows/ci.yml`) work on their own. `docker build -f deploy/Dockerfile .` fetches lib with `go mod download` inside the container; that is expected to work now that lib is public, but it has not been run yet.

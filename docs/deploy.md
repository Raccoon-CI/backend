# Deploying the server

[中文版](deploy.zh.md) — keep the two in step.

How to run `raccoonci-server` on a machine of its own: as a systemd service, or as a container, behind a reverse proxy with HTTPS. The files are in `deploy/`. For running it while developing, see [server-dev.md](server-dev.md); for the GitHub App it needs, [github-app.md](github-app.md).

## What it needs

- A Linux machine with outbound HTTPS to `api.github.com` and to the compute provider (InstaBox), and one host name with a DNS record pointing at it. Inbound, only ports 80 and 443 of the reverse proxy; the server itself listens on `127.0.0.1:18080` and should not be reachable from outside.
- A GitHub App installed on the repositories ([github-app.md](github-app.md)), whose webhook URL is `https://<host>/webhooks/github`.
- The same host name is what the machines' agents call back (`-public-url`) and what the API is served at.
- Not measured: how much CPU, memory and disk it needs. The binary is 17 MB and the container image 30 MB; the database of the development server, after about ten runs, is 200 KB. The disk grows with the artifacts and caches kept on it (`-storage-s3-*` or a bucket per organization moves them to object storage) and with the logs (`-log-retention-days`).

## Secrets

Every secret is an environment variable `NAME` or a file named by `NAME_FILE` that only its owner can read (mode 600 or 400; the server refuses a looser file and says so; the one exception is systemd's own credentials, which the unit uses: root-owned mode 440 files in `$CREDENTIALS_DIRECTORY`, checked on a real systemd 257). Never flags. Make them as root, once:

```sh
sudo install -d -m 0700 /etc/raccoonci
cd /etc/raccoonci
openssl rand -hex 32 | sudo tee webhook.secret >/dev/null   # paste the same string into the App's "Webhook secret"
openssl rand -hex 32 | sudo tee api.token >/dev/null        # the bootstrap admin token of the API
openssl rand -hex 32 | sudo tee master.key >/dev/null       # seals the secrets in the database
sudo cp /path/to/downloaded-app-key.pem app.pem             # the App's private key
sudo chmod 600 *
```

**Back up `master.key` somewhere else.** Without it the secrets, variables and buckets stored in the database cannot be opened, and nothing can bring them back.

`/etc/raccoonci/server.env` holds what is not secret:

```sh
RACCOONCI_GITHUB_APP_ID=1234567
# The variable part of the command line (see raccoonci-server -h). -allow says which repositories the
# server works for; -public-url and -snapshot let it run jobs on InstaBox (with INSTABOX_API_KEY_FILE).
RACCOONCI_ARGS=-allow my-org/* -public-url https://ci.example.com -snapshot sn_xxxxxxxxxxxx
```

## As a systemd service

```sh
make build VERSION=1.2.3          # dist/raccoonci-server
sudo install -m 0755 dist/raccoonci-server /usr/local/bin/
sudo install -m 0644 deploy/raccoonci-server.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now raccoonci-server
journalctl -u raccoonci-server -f
```

What `deploy/raccoonci-server.service` does: runs as a dynamic user with its state in `/var/lib/raccoonci-server` (mode 700); hands the secrets over as systemd credentials (`LoadCredential`, read from `/etc/raccoonci/` by root, given to the service as files in a private directory, so they are not in the environment, in `systemctl show` or on the command line); and restricts the process (no new privileges, read-only system, no home, no capabilities, only IP and Unix sockets, a system-call filter, no writable-and-executable memory). `systemd-analyze security` rates it 1.2 OK. It gives a stop 90 seconds, because on SIGTERM the server cancels the runs in progress, deletes their machines and queues them again.

What was checked: the file passes `systemd-analyze verify`; the real binary runs and answers under the same restrictions (the system-call filter, no writable-and-executable memory, the address families, no new privileges), and reads its secrets from `LoadCredential` files through `%d` as a real unit file does. What was not: the directives that need the system manager (`DynamicUser`, `StateDirectory`, `ProtectSystem`, `ProtectHome`, `CapabilityBoundingSet`) have not been run, because installing a system service on the development machine was not allowed. Look at `journalctl` after the first start, and report what it says.

Do not use `-local-runner` under this unit: it runs jobs on the server's own machine as the same user with no isolation, which is for development.

## As a container

```sh
docker build -f deploy/Dockerfile --build-arg VERSION=1.2.3 -t raccoonci-server .
# The App's private key is a file, and the server (user 65532 in the container)
# must be able to read it and nobody else: a copy owned by 65532, mode 400.
sudo install -o 65532 -g 65532 -m 0400 /etc/raccoonci/app.pem /etc/raccoonci/app.container.pem
docker run -d --name raccoonci --restart unless-stopped \
  -p 127.0.0.1:18080:18080 -v raccoonci-data:/data \
  -e RACCOONCI_WEBHOOK_SECRET=... -e RACCOONCI_API_TOKEN=... -e RACCOONCI_MASTER_KEY=... \
  -e RACCOONCI_GITHUB_APP_ID=1234567 \
  -v /etc/raccoonci/app.container.pem:/run/secrets/app.pem:ro -e RACCOONCI_GITHUB_APP_KEY_FILE=/run/secrets/app.pem \
  raccoonci-server -allow 'my-org/*' -public-url https://ci.example.com
```

The image holds the static binary and the CA certificates, runs as the non-root user 65532, and keeps its data in the `/data` volume, which a new volume takes the owner of (65532). Secrets in the environment are visible to anyone who can run `docker inspect`; secrets as files must be **readable by the server only** (mode 400 or 600, owned by 65532). A file owned by root, as the key is on the host (`permission denied`), and Docker's and Kubernetes' usual secret files (root, mode 444, refused with "readable by others") do not work. A directory of the host mounted as `/data` must be `chown 65532` first.

What was checked: the image's runtime stage, built from a binary made outside, starts, writes its database in a new volume, answers `/healthz` and the API, keeps the data across a restart, refuses a secret file readable by others, cannot read a private key owned by root, and reads one owned by 65532 with mode 400 (from a volume; a bind-mounted file with the same owner and mode is the same to the container, but was not tried). **The build stage was not run here**: the development machine's TLS proxy makes `go mod download` fail inside a container ("certificate signed by unknown authority"), which has nothing to do with the file. The build command itself is the one used on the host.

## The reverse proxy

`deploy/Caddyfile` is the whole configuration with Caddy (automatic HTTPS from Let's Encrypt): one host name, a large request body limit and no response buffering. It passes `caddy validate`. With another proxy, set the same three things — pass `/` to `127.0.0.1:18080`, allow request bodies of some hundred MB or more (artifacts and caches are uploaded through the server, in blocks), and do not buffer responses (the log of a run is a stream of server-sent events). Not tested with nginx.

If the host name is behind Cloudflare's proxy, uploads above 100 MB (the free plan's limit) fail. Give the machines' address its own host name that is not proxied (DNS only) and use that as `-public-url`; the webhook and the API can stay behind the proxy.

## First start

1. `curl https://ci.example.com/healthz` answers `{"status":"ok","version":"…"}`.
2. In the GitHub App → Advanced → Recent Deliveries, the `ping` shows 200.
3. `curl -H "Authorization: Bearer $(sudo cat /etc/raccoonci/api.token)" https://ci.example.com/v1/installations` lists where the App is installed (read from GitHub at start).
4. Push to an installed repository, or `POST /v1/runs` (see server-dev.md): the run appears in `/v1/runs`.

## Upgrading

Install the new binary (or image) and restart. The migrations of the database run at start; a database written by a newer server is refused, so there is no going back to an older binary after a start of a newer one — restore a backup for that. Runs in progress at the restart are queued again and start from the beginning (up to 3 attempts). Machines of the old server that were not deleted expire by themselves at their deadline.

## Backup and restore

What to keep: `/var/lib/raccoonci-server` (the `/data` volume), `/etc/raccoonci/master.key` and the other secrets (separately from the data), and the server's flags.

A consistent copy of the database while the server runs, with any SQLite client:

```sh
sqlite3 /var/lib/raccoonci-server/server.db "VACUUM INTO '/backup/server-$(date +%F).db'"
chmod 600 /backup/server-*.db
```

This was tried on the development database while the server ran: `PRAGMA integrity_check` says ok, the copy has the runs, events, installations, usage and tokens. Artifacts and caches kept on the disk are under `artifacts/` and `caches/` of the data directory (nothing to copy if they are in a bucket); logs, runs and results are in the database.

To restore, stop the server, put the copy in the data directory as `server.db` (delete any `server.db-wal` and `server.db-shm` there) and start it: the server makes the files private to its user at start. The restore was tried on a copy: the runs and the installations were there. Without the same `master.key` the secrets in the copy cannot be opened.

## Things to know

- The database and its files are made readable by the server's user alone (mode 600) at every start, and the server warns when the data directory can be entered by other users: it holds hashes of tokens, the payloads of GitHub's deliveries, the logs and the sealed secrets.
- There is one server per data directory. Two servers on the same directory are not supported.
- Nothing here sets up a firewall, log rotation beyond journald, monitoring or alerts: the server has `/healthz` (the process is up) and `/readyz` (it can do its work: the database accepts a write, and the data disk has more than `-min-free-mb` free; 503 with the name of the failing check, the reason only in the log; no token needed) and logs to standard error, and that is all there is to watch for now. The Docker image's `HEALTHCHECK` runs `raccoonci-server -check http://127.0.0.1:18080/readyz`; point a proxy's or monitor's probe at `/readyz`. For a Prometheus scrape, `GET /metrics` (admin bearer token; `authorization: {credentials_file: …}` in the scrape config) gives `raccoonci_check{check}` (1/0), `raccoonci_runs{status,conclusion}` and `raccoonci_oldest_queued_run_age_seconds`; alert on the last one staying high (runs are not being taken: the disk check failing, a quota, or no machine start available).

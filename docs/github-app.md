# Registering the GitHub App

Chinese version: [github-app.zh.md](github-app.zh.md)

The server can call GitHub with one personal token (development) or as a GitHub App (the real way). An App gets webhooks for every repository it is installed on, writes commit statuses, reads source, and needs no person's token. This page is for registering one for development; the production App is the same with other names and URLs.

You need: an account that owns the App (your own, or the organization the repositories are in), the public address of the server (`https://ci.example.com` in development), and the server's webhook secret.

## 1. Create the App

GitHub → your avatar → **Settings** → **Developer settings** → **GitHub Apps** → **New GitHub App**. (For an organization: the organization's **Settings** → **Developer settings** → **GitHub Apps**.)

| Field | Value |
|---|---|
| GitHub App name | anything unique on GitHub, e.g. `Raccoon CI (example App)` (the name is public) |
| Homepage URL | required; any address, e.g. the organization's page |
| Callback URL, "Request user authorization (OAuth) during installation" | leave empty / off. Signing in with GitHub is not built yet |
| Webhook → **Active** | on |
| Webhook URL | `https://<the server's public address>/webhooks/github` |
| Webhook secret | the same string as the server's `RACCOONCI_WEBHOOK_SECRET`, see below |
| SSL verification | enabled |

### Repository permissions

| Permission | Access | Why |
|---|---|---|
| Contents | Read-only | the workflow files and the source of a commit |
| Commit statuses | Read and write | the `raccoonci / <workflow>` status on a commit |
| Pull requests | Read-only | needed to receive pull request events |
| Metadata | Read-only | GitHub adds it itself |

Nothing under Organization or Account permissions. Do **not** give Actions, Workflows or Administration. **Checks** (read and write) is not needed yet: check runs are not built. When they are, the permission is added to the App and each installation approves the change once.

### Subscribe to events

**Push** and **Pull request**. (The Pull request box appears only after the Pull requests permission is set. GitHub always sends `ping` when a webhook is made.)

### Where can this GitHub App be installed?

**Only on this account** is enough for development. Choose **Any account** only for the App that other people will install.

Press **Create GitHub App**.

## 2. The webhook secret

The secret is typed into GitHub once and read from a file by the server, so the two must be the same string. The development server keeps it in `/data/home/.config/raccoonci/webhook.secret` (mode 600). Open it **yourself** (in your own terminal on that machine) and paste its content into the App's "Webhook secret" field. Do not paste it into a chat or a ticket. To use a new one instead: `openssl rand -hex 32` into the file (keep mode 600), and paste that.

## 3. The App ID and the private key

On the App's page (**Settings → Developer settings → GitHub Apps → your App → General**):

- **App ID** is a number near the top. It is not a secret.
- Scroll to **Private keys** → **Generate a private key**. A `.pem` file downloads. It *is* a secret: whoever has it can act as the App on every installation.

Put the file on the machine the server runs on, readable only by its user, for example:

```sh
umask 077
cat > /data/home/.config/raccoonci/app.pem     # paste the file's content, then Ctrl-D
ls -l /data/home/.config/raccoonci/app.pem     # -rw-------
```

The server refuses a key file that others can read. Delete the downloaded copy afterwards. Never commit it, never paste it into a chat.

## 4. Install the App

On the App's page: **Install App** → choose the account (the organization) → **Only select repositories** and pick them (or all). The server also needs the repositories in its `-allow` list (`-allow 'Raccoon-CI/*'`): the App says what GitHub may send, `-allow` says what the server works for.

## 5. Point the server at the App

The server takes **either** an App **or** a token, not both:

```sh
RACCOONCI_GITHUB_APP_ID=<the App ID>
RACCOONCI_GITHUB_APP_KEY_FILE=/data/home/.config/raccoonci/app.pem
RACCOONCI_WEBHOOK_SECRET_FILE=/data/home/.config/raccoonci/webhook.secret
# and no RACCOONCI_GITHUB_TOKEN
```

Start it **without** `-status=false` so that it reports commit statuses.

**If a repository webhook exists for the same server** (the development one on `bench-lint-docs`), delete it first. Both would deliver every push, and the server cannot tell the two deliveries are the same event: you would get two runs.

## 6. Check it works

1. App page → **Advanced** → **Recent Deliveries**: the `ping` should show a green 200 (`{"status":"pong"}`). Red means the address or the secret is wrong; the delivery's **Response** tab says which (401 is the secret). **Redeliver** repeats one.
2. Push a commit (or open a pull request) in an installed repository that has a workflow with `on: push` / `on: pull_request` and a `runs-on` the server can serve. The delivery should be 202, the server log says `runs created`, and the run shows in `GET /v1/runs`.
3. When the run starts, the commit gets a pending `raccoonci / <workflow>` status, and a success or failure at the end.
4. `POST /v1/runs` (start by hand) and a rerun look the installation up by themselves.

What this checks that a token could not: installation tokens, the App's own permissions, deliveries that carry an installation id. What it does not cover: check runs, signing in with GitHub (both not built).

## If something fails

- **Delivery 401**: the secret in GitHub differs from the server's file. Paste it again.
- **Delivery 404 / connection errors**: the public address is wrong or the tunnel is down; the URL must end in `/webhooks/github`.
- **Delivery 202 but no run**: the repository is not in `-allow`, or no workflow at that commit matches the event (`GET /v1/events/{id}` has a note per file).
- **A run fails at once with "installation token … 401/403/404"**: the key file is not that App's, the App ID is wrong, or the App is not installed on that repository.
- **No status on the commit**: the Commit statuses permission is missing (changing permissions asks the installer to approve again, under the installation's settings), or `-status=false` is still on.

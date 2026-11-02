# 注册 GitHub App

English version: [github-app.md](github-app.md)

服务端调用 GitHub 有两种方式：一个个人令牌（开发用），或者以 GitHub App 的身份（正式的做法）。App 装在哪个仓库上，就能收到哪个仓库的 webhook、写提交状态、读源码，不需要任何人的个人令牌。这一页讲怎么为开发环境注册一个；正式环境的 App 做法一样，只是名字和地址不同。

你需要：一个拥有这个 App 的账号（你自己的，或者仓库所在的组织）、服务端的公网地址（开发时是 `https://ci.example.com`）、服务端的 webhook 密钥。

## 1. 创建 App

GitHub → 右上角头像 → **Settings** → **Developer settings** → **GitHub Apps** → **New GitHub App**。（用组织的话：组织的 **Settings** → **Developer settings** → **GitHub Apps**。）

| 字段 | 填什么 |
|---|---|
| GitHub App name | GitHub 上唯一的名字，例如 `Raccoon CI (example App)`（名字是公开的） |
| Homepage URL | 必填，随便一个地址，例如组织主页 |
| Callback URL、"Request user authorization (OAuth) during installation" | 留空 / 不勾。还没做 GitHub 登录 |
| Webhook → **Active** | 勾上 |
| Webhook URL | `https://<服务端的公网地址>/webhooks/github` |
| Webhook secret | 和服务端的 `RACCOONCI_WEBHOOK_SECRET` 是同一串，见下面 |
| SSL verification | 保持启用 |

### Repository permissions（仓库权限）

| 权限 | 级别 | 用来做什么 |
|---|---|---|
| Contents | Read and write | 读 workflow 文件和某个提交的源码；也是 job 的 `permissions: contents: write` 的上限 |
| Commit statuses | Read and write | 在提交上写 `raccoonci / <workflow>` 状态 |
| Checks | Read and write | 每个 job 一个 check run，`raccoonci / <workflow> / <job>` |
| Pull requests | Read and write | 接收 pull request 事件；也是在 PR 上评论、打标签的 job 的上限 |
| Issues | Read and write | 用到 issue 的 job 的上限（PR 的标签和评论算 issue） |
| Deployments | Read and write | 创建 deployment 的 job 的上限 |
| Actions | Read-only | 读 GitHub 自己的 Actions 的 run 或 artifact 的 job 的上限 |
| Metadata | Read-only | GitHub 自动加上 |

每个 job 的 `GITHUB_TOKEN` 是按 job（或其 workflow）要求的权限裁剪出来的 installation token，而 **GitHub 不允许要 App 自己没有的权限**：App 的权限就是上限，job 要得更多时服务端退回只读 token，并在 job 日志里警告。所以只给 App 你仓库里的 job 可能用到的权限，不要更多。开发用的 App 正好是上面这张表。表里没有 Packages、Pages、Discussions、Security events，要它们的 job 会得到退回的 token。Organization 和 Account 权限都不要。**不要**给 Workflows、Administration。以后新增或放宽权限，每个安装方都要确认一次。

### Subscribe to events（订阅事件）

**Push** 和 **Pull request**。（Pull request 这一项要先设了 Pull requests 权限才会出现。GitHub 建 webhook 时总会发一个 `ping`。）

### Where can this GitHub App be installed?

开发时选 **Only on this account** 就够了。只有给别人装的那个正式 App 才选 **Any account**。

点 **Create GitHub App**。

## 2. webhook 密钥

密钥在 GitHub 上填一次，服务端从文件里读，所以两边必须是同一串。开发用的服务端把它放在 `/data/home/.config/raccoonci/webhook.secret`（权限 600）。请**你自己**在那台机器的终端里打开这个文件，把内容粘到 App 的 "Webhook secret" 里。不要把它贴到聊天或工单里。想换一个新的：用 `openssl rand -hex 32` 写进这个文件（保持权限 600），再把新的粘到 GitHub。

## 3. App ID 和私钥

在 App 的页面（**Settings → Developer settings → GitHub Apps → 你的 App → General**）：

- **App ID** 是页面上方的一个数字，不是秘密。
- 往下拉到 **Private keys** → **Generate a private key**，会下载一个 `.pem` 文件。它**是**秘密：谁拿到它，谁就能以这个 App 的身份在所有安装上行事。

把这个文件放到服务端运行的那台机器上，只让运行服务端的用户能读，例如：

```sh
umask 077
cat > /data/home/.config/raccoonci/app.pem     # 粘贴文件内容，然后 Ctrl-D
ls -l /data/home/.config/raccoonci/app.pem     # -rw-------
```

别人能读的私钥文件，服务端会拒绝使用。之后把下载下来的那份删掉。不要提交进 git，不要贴到聊天里。

## 4. 安装 App

在 App 的页面：**Install App** → 选账号（组织）→ **Only select repositories**，勾上要用的仓库（或者全部）。服务端的 `-allow` 里还要有这些仓库（`-allow 'Raccoon-CI/*'`）：App 决定 GitHub 能发什么，`-allow` 决定服务端为谁工作。

## 5. 让服务端使用 App

服务端要么用 App，要么用令牌，**不能同时**：

```sh
RACCOONCI_GITHUB_APP_ID=<App ID>
RACCOONCI_GITHUB_APP_KEY_FILE=/data/home/.config/raccoonci/app.pem
RACCOONCI_WEBHOOK_SECRET_FILE=/data/home/.config/raccoonci/webhook.secret
# 并且不设 RACCOONCI_GITHUB_TOKEN
```

启动时**不要**带 `-status=false`，这样它才会报提交状态。

**如果同一个服务端已经有一个仓库 webhook**（开发时 `bench-lint-docs` 上那个），先把它删掉。两条路都会投递每一次 push，服务端分不出这是同一个事件：你会得到两个 run。

## 6. 检查是否正常

1. App 页面 → **Advanced** → **Recent Deliveries**：`ping` 应该是绿色的 200（`{"status":"pong"}`）。红色说明地址或密钥不对，点开那次投递的 **Response** 标签能看原因（401 是密钥）。**Redeliver** 可以重发。
2. 在已安装的仓库里推一个提交（或开一个 pull request），该仓库要有 `on: push` / `on: pull_request` 的 workflow，且 `runs-on` 是服务端能提供的。投递应是 202，服务端日志有 `runs created`，`GET /v1/runs` 里能看到这个 run。
3. run 开始时，提交上出现 pending 的 `raccoonci / <workflow>` 状态，结束时变成 success 或 failure。
4. `POST /v1/runs`（手动启动）和重跑会自己去查 installation。

这一步能验证令牌验证不了的东西：installation 令牌、App 自己的权限、带 installation id 的投递。它也覆盖每个 job 一个的 check run（run 所在的提交上出现 App 建的 `raccoonci / <workflow> / <job>`）和每个 job 的 `GITHUB_TOKEN`。它不覆盖的：GitHub 登录（还没做）。

## 出问题时

- **投递 401**：GitHub 上的密钥和服务端文件里的不一样，重新粘一次。
- **投递 404 / 连接错误**：公网地址不对或隧道没通；地址必须以 `/webhooks/github` 结尾。
- **投递 202 但没有 run**：仓库不在 `-allow` 里，或者那个提交上没有 workflow 匹配这个事件（`GET /v1/events/{id}` 的 note 里每个文件各有说明）。
- **run 一开始就失败，写着 "installation token … 401/403/404"**：私钥文件不是这个 App 的、App ID 错了，或者 App 没有装在这个仓库上。
- **提交上没有状态**：缺 Commit statuses 权限（改权限后，安装方要在安装设置里再确认一次），或者还带着 `-status=false`。

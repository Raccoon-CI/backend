# 开发时怎么运行服务端

[English](server-dev.md) — 两个版本要同步修改。

状态：`docs/server-design.md` 的第 1、2 步。服务端接收 GitHub 的 `push` 和 `pull_request` 投递（或者一个手动启动 workflow 的请求），判断它们会触发哪些 workflow，为每个存一条排队中的 run，按提交取回仓库，把每个 job 实例放到一台机器上跑，存下日志，并通过 API 展示 run 和日志。会把每个 run 作为 commit status 回报给 GitHub，也能取消和重跑 run。保存密钥和变量（在数据库里加密），只把 run 用到的那几个交给它，API token 有角色。用 GitHub App 时还会给每个 job 发一个 check run，并给每个 job 一个 `GITHUB_TOKEN`。还不做用 GitHub 登录。

## 构建和启动

（要把它跑在一台自己的机器上——systemd、容器、反向代理、备份——见 [deploy.zh.md](deploy.zh.md)。）

```sh
make build                       # dist/raccoonci 和 dist/raccoonci-server
raccoonci-server -allow owner/name [-allow owner/*] [-listen 127.0.0.1:18080] [-data-dir DIR] [怎么跑 job]
```

至少要一个 `-allow`：别的仓库的投递或请求会被拒绝或忘掉。数据库是 `DIR/server.db`（SQLite，启动时迁移）。被重启打断的投递在启动时会重新处理；正在进行中的 run 会重新排队（见"重启"一节）。

**怎么跑 job**（下面两种都没配时，run 一直排队）：

- InstaBox：`-public-url https://…`（机器访问服务端的地址）、`-snapshot ID`（runner 快照；环境变量 `RACCOONCI_INSTABOX_SNAPSHOT`）和 `INSTABOX_API_KEY` 或 `IBX_API_KEY` 里的 InstaBox 密钥。每个 job 实例一台机器；机器上的 agent 回连 `<public-url>/agent`。
- `-local-runner raccoonci 二进制的路径`：job **跑在服务端所在的这台机器上，用同一个用户，没有任何隔离**。只用于开发，或者专门为此准备的机器，并且只跑你信任的代码。job 通过监听地址访问服务端，所以监听地址必须是回环地址，除非给了 `-public-url`。

## 密钥

每个都是环境变量 `NAME`，或者用 `NAME_FILE` 指向一个只有你能读的文件（权限 600，更宽松的文件会被拒绝）。它们永远不是命令行参数。

| 名字 | 含义 |
|------|------|
| `RACCOONCI_WEBHOOK_SECRET` | GitHub 投递的签名密钥（必需） |
| `RACCOONCI_API_TOKEN` | `/v1` 的引导 bearer token，admin 角色；别的 token 通过 API 创建 |
| `RACCOONCI_MASTER_KEY` | 32 字节（`openssl rand -hex 32`），用来加密数据库里的密钥；不设的话密钥和变量功能关闭。丢了它就丢了所有密钥 |
| `RACCOONCI_MASTER_KEY_OLD` | 配合 `-master-key-old-id`：轮换之前的旧主密钥，旧的密钥还能打开（新的用当前的 `-master-key-id` 加密） |
| `RACCOONCI_GITHUB_APP_ID`、`RACCOONCI_GITHUB_APP_KEY_FILE` | 以 GitHub App 身份行事（用 App 私钥换 installation token） |
| `RACCOONCI_GITHUB_TOKEN` | 不用 App，用一个 token，开发时配合仓库 webhook |
| `INSTABOX_API_KEY` / `IBX_API_KEY` | InstaBox 密钥（第一个名字在 InstaBox 机器上是保留的） |

## 告诉 GitHub

仓库 webhook（开发用）或 GitHub App（正式的）指向 `https://<你的主机>/webhooks/github`，内容类型 `application/json`，secret 就是 `RACCOONCI_WEBHOOK_SECRET`，事件选 `push` 和 `pull_request`。怎么注册 App：[github-app.zh.md](github-app.zh.md)。在没有公网地址的沙箱上开发时，这个地址是指向 `127.0.0.1:18080` 的 Cloudflare Tunnel。

## 启动一个 run

没有 webhook 时，直接请求 API（workflow 需要有 `workflow_dispatch` 触发器）：

```sh
curl -H "Authorization: Bearer $TOKEN" -X POST https://HOST/v1/runs \
  -d '{"repo":"owner/name","ref":"main","workflow":".github/workflows/ci.yml","inputs":{}}'
```

或者回放一份录下来的投递，签名和 GitHub 一样（`RACCOONCI_WEBHOOK_SECRET_FILE=… raccoonci-server replay -url … -delivery d1 payload.json`；再发同样的 `-delivery` 必须回答 `duplicate`）。

## 读结果

```sh
GET /v1/runs                       # 最新的在前
GET /v1/runs/{id}                  # 带上它启动时的 workflow 文件
GET /v1/runs/{id}/jobs             # job 实例和各自的结果
GET /v1/runs/{id}/log?after=N      # 日志，一页一页读（"done" 说明已经结束）
GET /v1/runs/{id}/log              # 带 Accept: text/event-stream：日志边长边推，最后一个 "end" 事件
GET /v1/events/{id}                # 一次投递，以及对每个 workflow 文件做了什么决定
POST /v1/runs/{id}/cancel          # 排队中的 run 立刻结束；运行中的被通知停止（它的机器也会被删掉）
POST /v1/runs/{id}/rerun           # 新建一个和已结束的 run 一样的 run：同一提交、当时的 workflow 文件、同样的输入
```

## 密钥、变量和 token

下面都需要 admin 角色。scope 写成 `org:<名字>` 或 `repo:<owner>/<名字>`，全小写。

```sh
PUT    /v1/secrets   {"scope":"repo:acme/web","name":"DEPLOY_TOKEN","value":"…"}   # 加密保存；读不回来
GET    /v1/secrets?scope=repo:acme/web                                              # 只有名字
DELETE /v1/secrets?scope=repo:acme/web&name=DEPLOY_TOKEN
PUT/GET/DELETE /v1/vars                                                             # 变量同理，但能读回来
POST   /v1/tokens    {"name":"dashboard","role":"read"}                              # token 只在返回里出现一次
GET    /v1/tokens    DELETE /v1/tokens/{id}                                          # 列出；吊销
```

- 密钥进数据库之前先加密（AES-256-GCM，主密钥加密，并绑定它的 scope 和名字），只在 run 要发给机器的那一刻才解开。它放在 job 的任务描述里经 https 发过去，runner 会把 step 打印的内容里的它打码。它不会出现在日志、API 的返回或错误信息里。
- run 只拿到 workflow 里按字面写出的密钥（`secrets.NAME`、`secrets['NAME']`），同名时仓库的优先于组织的。通过拼出来的名字、或把整个 `secrets` 传下去才能读到的密钥不会发送。名字统一存成大写；不允许以 `GITHUB_` 开头。
- 来自别的仓库（或 fork 已被删除）的 pull request 的 run **拿不到任何密钥**，日志里会写明。变量还是能拿到。
- `read` token 能读 run、job、日志和事件；`admin` token 还能启动、取消、重跑 run，管理密钥、变量和 token。token 只保存哈希。
- `read` token 可以限定到某些仓库：`POST /v1/tokens {"name":"acme-dashboard","role":"read","repos":["acme/*","other/web"]}`（`owner/name` 或 `owner/*`，不分大小写）。这样的 token 只能看到这些仓库的 run 和事件：列表里没有别的仓库的，别的仓库的 run 或事件回 404，和不存在的一模一样。它不能用其他任何路由（403），以后新加的路由在决定之前对它是关闭的。`admin` token 不能限定（400），因为它到处都能改东西。引导 token 和没写 `repos` 的 token 看得到全部。
- `GITHUB_TOKEN`（`secrets.GITHUB_TOKEN`、`github.token`）**只有用 GitHub App 时**由服务端生成（`-job-token`，默认开）：每个 job 实例拿到一个自己的 installation token，只对这个 run 的仓库有效，job 一结束就吊销（最迟一小时过期）。权限取 job 的 `permissions:`，没有就取 workflow 的，再没有就用服务端的默认——**只读 contents**（GitHub 自己的默认可能是读写）；写成映射的只给点名的，其余为 none，`read-all`/`write-all` 可用，App token 不可能有的名字（`id-token` 等）会被丢掉。App 没有 job 要的权限时，job 拿默认权限的 token，日志里有警告；一个 token 都做不出来时 job 不带 token 运行，同样有警告。fork 的 pull request 拿不到，和它拿不到密钥一致。普通 token（`RACCOONCI_GITHUB_TOKEN`）绝不会传给 job。这个名字的密钥不能存。已用开发 App 对真实 GitHub 验证过（见"目前验证过的"）；机器上的 step 真的用这个 token 还没试过。

## GitHub 上的状态

run 会在它所跑的提交上设置 commit status `raccoonci / <workflow 名>`：开始时 `pending`（"Running"），之后是 `success`、`failure` 或 `error`（"Cancelled"），带上耗时。它显示在提交上，pull request 的 checks 里也能看到。用普通 token（需要 `repo:status` 能力）或 GitHub App（需要 "Commit statuses" 权限）都行；`-status=false` 关闭。报告失败只记日志，不会让 run 失败。用 GitHub App 时，每个 job 实例还会有一个 check run `raccoonci / <workflow 名> / <job>`（开始时 `in_progress`，之后是 `success`、`failure` 或 `cancelled`）；需要 App 有 Checks 写权限，普通 token 写不了 check run，所以不会建；`-checks=false` 关闭。check run 里带着 job summary（step 写进 `$GITHUB_STEP_SUMMARY` 的内容，markdown，密钥已遮蔽）和 step 打印的注解（`::error file=…,line=…,title=…::消息`、`::warning`、`::notice`；每个 job 最多 50 条，其余在 summary 里计数；没有可用文件或路径跑出了仓库的，挂在 `.github` 第 1 行，和 GitHub 一样）。没做：每个 step 一行、problem matcher 产生的注解（runner 还没有 problem matcher）、在报告之前就失败的 job 的 summary。

`/v1/openapi.yaml` 描述这一切；有一个测试保证它和路由一致。

## 定时触发

`on: schedule:` 的 workflow 由服务端触发（`-schedule=false` 则交给 GitHub）。和 GitHub 一样，只认仓库**默认分支**上的 workflow 文件，到点时 run 用的是该分支当时最新的提交（`github.event_name` 为 `schedule`，`github.event.schedule` 是触发的那条 cron，actor 是 `schedule`）。cron 按 UTC、五段，支持列表、范围和步长；一个 workflow 写了两条就是各自到点各出一个 run。

- 看哪些仓库：服务端收到过事件（投递或 dispatch）的仓库，加上 `-allow` 里写明全名的（`owner/*` 不算：通配里的仓库要等它的第一个事件才认得）。对一个仓库的定时信息缓存 5 分钟；到点时会在分支最新提交上重新读一次 workflow，那条 cron 必须还在。
- 一次触发会记成一个事件，id 由仓库、文件、cron 条目和分钟组成，所以重启（或两个服务端共用一个库）不会触发两次。服务端停着的时候错过的触发不补；卡顿了几分钟只出一个 run，不会一下子出一串。GitHub 自己在高峰时可能晚点或丢掉定时 run；这里误差在 15 秒的检查间隔以内。
- 没做：GitHub 对 60 天没有活动的仓库停掉定时任务的规则；没人给它发过事件、又不在 `-allow` 里的仓库发现不了。

## 并发组

workflow 的 `concurrency:`（一个组名，或带 `cancel-in-progress` 的 `group`；表达式可用 `github`、`inputs`、`vars`）按仓库生效，组名不分大小写。一个组里同时只有一个 run 在跑、一个在等：组被占用时到来的 run 要等（日志写 `waiting for run N`），后面又来一个时，在等的那个被取消（`a newer run of the same concurrency group replaced this one`），和 GitHub 一样。`cancel-in-progress` 为真时，组里正在跑的 run 立刻被取消。被取消的等待者会让出位置。源码在等待结束之后才取。job 自己的 `concurrency:` 对这个 job 同样生效（表达式里还能用 `needs`、`strategy`、`matrix`）：进组发生在 job 开始之前，所以在等的 job 还没有 job 记录、check run 和机器（它所在 run 的日志写 `waiting for job deploy of run N`）；新来的 job 顶掉在等的那个；`cancel-in-progress` 取消的是组里的那个 job，不是它的整个 run，那个 job 以 cancelled 结束。同一个 matrix 的 job 在同一个组里轮流跑，每个 matrix 值一个组时一起跑。workflow 级和 job 级同名的组是两个组。限制：状态在服务进程内存里（单实例，重启后所有 run 重新进组）；只用假机器测过。等待并发组的 run 不占执行器的名额（`MaxRuns`），所以同一个组里一串 push 不会挡住别的 run。

## 用 GitHub 登录

给不该持有 API token 的人用（前端、命令行）。用的是 App 自己的用户授权（OAuth），所以 App 的**回调地址**要设成 `https://<public-url>/v1/auth/github/callback`（见 [github-app.zh.md](github-app.zh.md)），启动时要有：`-github-client-id`（App 的 client id，环境变量 `RACCOONCI_GITHUB_CLIENT_ID`）、`RACCOONCI_GITHUB_CLIENT_SECRET` 或 `_FILE`，以及 https 的 `-public-url`。可选：`-auth-admins 登录名,登录名`、`-auth-redirect https://app.example.com/`（可重复）、`-auth-session-hours`（默认 12）。

- `GET /v1/auth/github/login[?redirect=URL]` 把浏览器送去 GitHub；state 带签名，并用一个短命的 cookie（HttpOnly、Secure、SameSite=Lax）绑定到这个浏览器，所以别人发起的链接没法在你的浏览器里完成。`redirect` 必须以某个 `-auth-redirect` 地址开头，否则 400。
- `GET /v1/auth/github/callback`（GitHub 把浏览器送回这里）用 code 换用户自己的 GitHub token，问出他是谁、能访问 App 安装的哪些仓库，然后**吊销这个 token**，再做一个**会话**：一个正好限定到这些仓库的 `read` token（限制和被限定的 API token 一样，见"密钥"一节），到会话时长就过期。回答是 `{token, user, role, expires_at, repos}`；带了 `redirect` 就把浏览器送过去，同样的内容放在 URL 的**片段**里（绝不放查询串，那会进日志）。一个仓库也访问不到的人得 403，不产生会话。
- 只有在 `-auth-admins` 里的登录名才有 `admin`（不受限）。是 GitHub 组织的 owner 在这里不代表任何东西。
- `GET /v1/auth/me` 说明一个 token 是谁（`session`、`api` 或 `bootstrap`，它的角色、仓库、过期时间）；`POST /v1/auth/logout` 结束调用它的那个 token。两个对被限定的 token 也可用。过期超过一天的会话会被删掉。
- 会话能看到什么在创建时就定了：之后才被授权的仓库需要重新登录。
- 已测：用假 GitHub 走完整个流程、各种拒绝情形和吊销；经隧道测了第一步（跳转到 GitHub、cookie、两个 400）。**还没有人在浏览器里真走过一遍**：那需要先在 App 上登记回调地址。

## App 装在哪里

用 GitHub App 时，服务端保存一张它的安装表（账号、全部仓库还是选定仓库、是否挂起）：启动时和之后每小时从 GitHub 读（`GET /app/installations` 和每个安装的仓库），并由 App 的 `installation` 和 `installation_repositories` 投递随时更新——GitHub 对每个 App 都会发这两种，不用订阅，也不要求仓库在 `-allow` 里（只记录，不跑）。手动启动或重跑的 run 找 installation 时先查这张表，再看存下的事件，最后才问 GitHub。`GET /v1/installations` 显示这张表（限定了仓库的 token 看不了）。限制：重新投递一个旧的 `installation` 事件会把仓库列表退回当时的样子，直到下一个事件或每小时的读取；这两种事件还没见过 GitHub 自己发的（启动时的读取见过：开发 App 在 8 个仓库上）。

## OpenID Connect（id-token）

有 `permissions: id-token: write` 的 job（取它自己的，没有就取 workflow 的；`write-all` 也算）可以像向 GitHub 要一样向服务端要一个签名的 ID token，拿它去换 AWS、GCP、Azure、Vault 等的短期凭据，不用存任何密钥。签发者是服务端自己：`https://<public-url>/oidc`（`-oidc`，默认开，需要 https 的 `-public-url`）。job 会拿到 `ACTIONS_ID_TOKEN_REQUEST_URL` 和 `ACTIONS_ID_TOKEN_REQUEST_TOKEN`，这正是 `actions/core` 的 `getIDToken(audience)` 和各云的登录 action（`aws-actions/configure-aws-credentials`、`google-github-actions/auth`、`azure/login`）读的；没有这个权限的 job 两个都拿不到，fork 的 pull request 永远拿不到。

- `GET /oidc/.well-known/openid-configuration` 和 `GET /oidc/jwks` 是公开的，供云厂商取来验证。`GET /oidc/token?audience=…` 带 job 的请求 token（绑定一个 run 和 job，job 运行期间且 run 未结束时有效），回 `{"value": "<jwt>"}`。不给 `audience` 时是 `https://github.com/<owner>`，和 GitHub 一样。
- RS256，有效期 10 分钟。密钥第一次用时生成，用主密钥封存后存库（主密钥换了就换一把新的，验证方只是重新取一次公钥集；没有主密钥时只在内存里，每次启动都变）。
- claim 用 GitHub 的名字：`iss`、`aud`、`sub`、`jti`、`iat`、`nbf`、`exp`、`ref`、`sha`、`ref_type`、`repository`、`repository_owner`、`actor`、`workflow`、`event_name`、`run_id`、`run_number`、`run_attempt`、`job_workflow_ref`、`runner_environment`（`self-hosted`）。`sub` 是 `repo:<owner>/<name>:ref:<ref>`，pull request 是 `repo:<owner>/<name>:pull_request`。所以为 GitHub 写的信任条件含义不变，但**签发者是这个服务端**，每个云账号里要为它设置一次。没有签发的：`environment`（以及带它的 `sub` 形式）、`head_ref`、`base_ref`、`repository_id`、`job_workflow_sha`、自定义 `sub` 模板。
- 已测：真 runner（step 取到自己的 token，claim 正确）、用公布的公钥集独立验签、经隧道取发现文档和公钥集。**还没有任何云厂商接受过这样的 token。**

## 起机限额

InstaBox 限制一个组织每分钟起机的数量（Pro 套餐 10 台），超了回 429。`-starts-per-minute`（默认 10，0 表示自己不计数）让服务端用 62 秒的滑动窗口数自己的起机次数，超了的排队等，所以一阵 run 会被摊开而不是失败；收到 429 后所有起机停 20 秒，被拒的那一次再问（最多约 7 分钟）。日志里会写 `waited … for the machine start limit`。计数只是服务端自己的：同一个 InstaBox 组织里别人起的机器，只能靠 429 才知道。

## 日志保留

已结束的 run 的日志保留 90 天（`-log-retention-days`，可以写小数，0 表示永久保留）；启动时和之后每小时清理一次，删掉更早结束的 run 的日志行。run、它的 job 和结果都保留，`log_purged` 变为 true，这时 `GET /v1/runs/{id}/log`（以及事件流）回 **410 Gone**，而不是一份空日志。没做：旧 webhook 投递的原始载荷和旧 run 的 workflow 原文还留着（重跑要读它们）；SQLite 把释放的页留着复用，文件本身不会缩小。

磁盘保护：`-data-dir` 所在的盘剩余空间低于 `-min-free-mb`（默认 1024，0 表示不检查）时，服务端不再从队列里领新的 run，只记一条警告，空间回来后自己继续。一个 run 的源码包超过 `-max-source-mb`（默认 2048）就让这个 run 失败并写明原因；下载在写入时就被截断，工作目录里不留东西。artifact 服务用同一个限额：盘这么满时，新建 artifact 回 429（`resource_exhausted`），空间回来后又接受；已经开始的上传不会被中断。没做：按仓库或组织的配额。

用量与额度：每个 run 把用掉的机器时间按 UTC 日期和仓库记进一张表（`GET /v1/usage?month=2026-11`；被限定了仓库的 token 只能看自己的）。机器时间是 job 实例占着机器的时间，含起机，按 run 合计后向上取整到秒；它是机器要花的钱对应的时间，不是 step 跑的时间。`-quota-repo-minutes N` 和 `-quota-owner-minutes N`（默认 0，不限）给每个仓库、以及每个所有者（名下所有仓库合计）定一个每 UTC 月的额度：用完了的仓库/所有者的新 run 立刻失败，信息里写明已用多少、几号起恢复（下月 1 日），并且不占用任何机器。计数的是已经结束的 run，所以额度用完时还在跑的 run 会跑完（同时在跑的会让它略微超出），重跑和别的 run 一样计。没做：按仓库或组织各自不同的额度、额度快用完时的提醒、换算成钱（表里是秒，不是美元）。

公平：执行器有空位（`MaxRuns`，默认 4）时，下一个领的是排队的 run 里"所属仓库正在跑的 run 最少"的那个，一样多就先到先得。一个仓库排了五十个 run 也饿不死别的仓库，只有一个仓库有活时它仍然独占所有空位。没有按仓库或组织配置的额度（比如机器时间的配额）：那需要用量统计，还没做。

runner 镜像：启动时和之后每 `-runner-images-interval`（默认 6 小时，0 表示不轮询）把 GitHub 托管镜像预装了什么读进数据库（来源和 `raccoonci runner-images update` 一样，不需要令牌）。run 用内置快照盖上这份数据来规划；读不到的镜像保留上次的好数据。一个 run 规划时用的那份数据（约 10 KB）会随它的每个 job 一起发给机器，`run-instance` 用它而不是二进制里内置的快照，所以服务端和机器对"预装了什么"的看法一致。机器用不了的数据（不是真的快照）只在 job 日志里留一行，改用内置的；旧版本的机器会忽略这个字段。

## Artifact

`actions/upload-artifact` 和 `download-artifact`（v4）可用，一个完成了的 artifact 会和数据一起记进数据库（名字、大小、哪个 job、过期时间），所以重启不会忘。用户这样用：

```sh
GET /v1/runs/{id}/artifacts                       # [{id, name, job, size, digest, created_at, expires_at}]
GET /v1/runs/{id}/artifacts/{artifact}/download   # 302 到一个一小时内有效的链接，直接到存储
```

两个都要 `read` 角色的 token。artifact 保留 90 天，步骤里写了 `retention-days` 就更短；`-artifact-retention-days N` 改默认值，也限制步骤最多能要多久。每小时（和启动时）清理一次过期的，数据和记录一起删。说明：

- 每个 artifact 服务（服务端的磁盘、每个组织自己的桶）各有自己的记录。组织换了桶之后，旧桶里的 artifact 不再列出；到期时只是忘掉记录，对象还留在旧桶里，需要的话请在那边删。
- 没上传完的不记录；服务在上传中途停掉的话，已经放进桶里的分片会留下（桶上给"未完成的分段上传"设一条生命周期规则就能清；服务端不做）。
- 客户端给的 sha256 只存和显示，不校验。没有 artifact v3（旧的 REST 协议）。

## 重启

服务停止时正在进行的 run 会**从头重新开始**，不是接着跑：收到 SIGTERM 时每个进行中的 run 被取消（机器删掉，要几秒），然后回到队列；崩溃的话，下次启动时把还在进行中的 run 放回队列。run 的日志里会有一条警告（"the server stopped while this ran; it is started again … attempt N"），前一次尝试的日志行保留，这期间 GitHub 上的状态一直是 `pending`。每次启动时，在取任何 run 之前，服务还会删掉死掉的 run 留在磁盘上的东西：工作目录（`work/`）、本机 runner 的 job 目录（`jobs/`），以及没收完的上传分块（`artifacts/spool`、`caches/cache-spool`）。这假定一个数据目录只有一个服务在用。一个 run 最多被取走 3 次，之后记为失败。被人取消的 run 仍是已取消。崩溃之后旧一次尝试的机器已经不知道是哪些，由 InstaBox 在 2 小时期限到时删掉。

## 缓存，以及对象放在哪里

`actions/cache` 和 `setup-*` 的 `cache:` 选项都能用（v1 协议）。缓存不是所有人共用一份：每个 run 把缓存存进**它自己那个 ref 的空间**，恢复时从这个空间和**默认分支的空间**找。

- 默认分支有一个空间，只有默认分支的 run 能写。
- 其余的 ref（功能分支、tag、pull request 的合并 ref，不管是不是来自 fork）各有自己的空间，也能从默认分支的空间恢复，但读不到别的 ref 的空间，也写不进默认分支的空间。pull request 用默认分支用的那个 key 保存缓存，会落进它自己的空间，默认分支的那一条不变。
- 不同仓库之间永远不共用。和 GitHub 不同的是，pull request 不能从它的目标分支恢复，除非目标分支就是默认分支。
- 三个限制。每个空间的上限是 `-cache-limit-gb`（默认 5），每次上传都检查，超了就淘汰最久没用的条目。一个仓库所有空间加起来的上限是 `-cache-repo-limit-gb`（默认 10，0 表示不限），一个条目 `-cache-max-age-days`（默认 7 天，和 GitHub 一样）没人恢复过就删除；这两个由清理任务做，启动时和之后每 15 分钟一次，所以仓库可能在这段时间内超过上限。清理时按仓库跨空间删最久没用的条目，并删掉变空的空间（有上传进行中的不删）。恢复的时间最多每 5 分钟和服务关闭时写一次盘，所以重启不会让一个常用条目看起来像没人用。
- 清理功能出现之前建的空间只靠目录名认：有 `objects` 目录就当作在本机磁盘上，否则不动它（它在哪个组织的桶里没有记下来）。新建的空间有一个 `org` 文件。

对象（artifact 和缓存）放在 `-data-dir` 下的目录里，或者放在用户自己提供的桶里：`-storage-s3-bucket B [-storage-s3-endpoint URL] [-storage-s3-region R] [-storage-s3-prefix P] [-storage-s3-path-style]`，加上 `RACCOONCI_STORAGE_S3_KEY` 和 `RACCOONCI_STORAGE_S3_SECRET`。用 Cloudflare R2 的话：endpoint 是 `https://<账号 id>.r2.cloudflarestorage.com`，region 填 `auto`。启动时会试一遍这个桶（写入、通过预签名链接读回、删除），桶不能用就拒绝启动；如果桶不用凭证就把对象给任何人，会在日志里警告。桶要保持私有。下载是直接指向桶的预签名链接，上传仍然经过服务端。这个桶是服务端自己的，给没有自己的桶的组织用。

**每个组织一个桶**（正式使用时用户这么做）：admin 通过 API 给出，之后这个组织各仓库的 artifact 和缓存就放进它的桶，不再进服务端的。

```sh
PUT    /v1/storage  {"org":"acme","bucket":"acme-ci","endpoint":"https://<账号>.r2.cloudflarestorage.com","region":"auto","prefix":"raccoon","access_key":"…","secret_key":"…"}
GET    /v1/storage?org=acme        # 在哪里，但不返回密钥
DELETE /v1/storage?org=acme        # 回到服务端自己的存储
PUT    /v1/storage  {"org":"*", ...}   # 服务端自己的桶，给没有自己桶的组织用：请求体一样，`org` 写 `*`
```

服务端自己的桶不必再用启动参数和环境变量给了：用 `org: "*"` 保存后，它和组织的桶一样先试再封存、立即生效（不用重启），并且优先于 `-storage-s3-*`；删掉后，启动时给的桶（或本机磁盘）又成了默认。组织自己的桶仍然优先于两者。

保存设置之前会先试这个桶（写入、通过预签名链接读回、删除）：不能用的桶会被拒绝，并说明是哪一步失败；任何人都能读的桶会保存下来并给出警告。设置和密钥用主密钥加密，放在只有服务端读的一行里：workflow 引用不到它，密钥 API 也不能列出、覆盖或删除它。换桶之后，新的 artifact 和缓存进新桶；旧桶里的留在原处，对象不在新桶里的缓存条目在被查找时会被丢弃。需要主密钥。有自己桶的组织，它的 artifact 和缓存由按需创建的单独服务处理。

## 现在的 run 是什么样

- 提交是事件所指的那个。`pull_request` 的 run 用的源码是 GitHub 把头合并进基分支的结果（`refs/pull/N/merge`，和 GitHub 自己的 runner 看到的一样）；这个 ref 在事件之后过一会儿才出现，所以最多问 4 次、每次隔 2 秒，一直没有（两者冲突）就改跑**头提交**，日志里会写明。`github.sha` 仍是头提交，不是合并提交的 id。
- 源码来自 GitHub 对该提交的归档：没有 `.git` 目录，没有历史。
- `GITHUB_TOKEN` 只有用 GitHub App 时才有（见"密钥"一节）。artifact 和 cache 可用（见后面几节）。
- `github.run_number` 和 GitHub 一样按 workflow 数事件启动的 run（每个仓库、每个 workflow 文件各数各的）；重跑沿用被重跑那个 run 的编号，`github.run_attempt` 加一（1、2……）。`github.run_id` 是服务端的 run id（和 GitHub 不同，重跑会有新的 id）。

## 目前验证过的

- 单元测试（`-race`）：签名、重复投递、允许列表、活动类型、fork、过滤器、重启后的恢复；存储；用假机器测的执行器（输出在 job 之间传递、失败的 job 挡住依赖它的 job、机器出错、关机时取消、同时运行数上限）；源码归档重新打包后用真正的解包器验证；API，包括 SSE 流，以及规格和路由一致的测试。
- 把 `cli/cli` 一次真实 push 的签名回放经 Cloudflare Tunnel 发给服务端，用真实 GitHub API：读到 15 个 workflow 文件，建了 3 个 run，和 GitHub 自己在那个提交上跑的完全一致。
- **一个完整的 run，端到端**（`-local-runner`）：经隧道 `POST /v1/runs` 启动 `bench-lint-docs` 的 workflow；服务端从 GitHub 取回真实归档、解析出 job，用真正的 `raccoonci run-instance` 跑（checkout、`setup-node`、三个 `run` 步骤），存下 62 行日志和一个成功的 job，API 能读到；总共 4 秒。同一份日志经隧道以 server-sent events 实时推送也验证了。
- **同一个 run 放到 InstaBox 机器上跑**（runner 快照 `raccoonci-runner-9c482bc`，机器经隧道的 `/agent` 回连服务端）：成功，机器 9.8 秒就绪，从请求到 run 结束 16 秒，日志里是同样的步骤，结束后机器已被删除。
- **GitHub 自己发出的投递**（`bench-lint-docs` 上的仓库 webhook，经隧道）：ping 回 200；两次 push，以及一个 pull request 的打开和关闭，都带着签名到达，回 202，用真实 API 读了该提交上的 workflow，并记为已忽略（仓库里唯一的 workflow 只监听 `workflow_dispatch`）。token 没有 `workflow` 权限，没法加一个由 push 触发的 workflow，所以还没看到由真实投递建出来的 run。
- **InstaBox 上的状态、取消、重跑：** run 的提交在 GitHub 上先 `pending` 后 `success`（"Succeeded in 27s"）；5 秒时取消的 run 记为 cancelled，状态是 `error`；重跑重复了第一次（`rerun_of`）并成功。取消的那次暴露了一个泄漏：机器还在创建，删除被拒绝（409 `operation_conflict`）后就放弃了，机器一直跑到 2 小时的期限。现在 `instabox.Client.DeleteMachine` 会等待后重试；真机验证过（创建中取消后没有残留机器）。
- **真实 job 里的缓存，在 InstaBox 机器上**（`bench-go-integration`，`setup-go` 带 `cache: true`，还有一个 postgres 服务容器）：第一次没找到缓存，经服务端保存了 44 MB；重跑时恢复成功（"Cache restored from key …"、"Cache hit occurred on the primary key"）；在新分支上的 run 也恢复成功，走的是默认分支的空间；磁盘上只有默认分支的那一个空间；结束后没有残留机器。隔离规则有用多个 run 做的测试（功能分支、pull request 的 ref、另一个仓库、重启），去掉隔离后这些测试会失败。
- **密钥：** 经隧道存了一个密钥和一个变量并列出（密钥只列名字），`read` token 读 run 得到 200、读密钥得到 403，被吊销的 token 立刻失效。用真正的 runner（`RACCOONCI_TEST_BIN=dist/raccoonci go test ./internal/server/execute -run Real`）：step 对密钥打印出 `token=[***]`，对变量打印出 `region=[eu]`，密钥的值在日志里任何地方都没有出现，fork 的 run 打印的 token 是空的。没在真实的、由 GitHub 触发且用到密钥的 workflow 上跑过（手头的 token 加不了 workflow 文件）。
- **InstaBox 上的起机限额**（`-starts-per-minute 2`，同时启动 `bench-lint-docs` 的四个 run）：两台机器 9–11 秒就绪，另外两个分别等了 1 分 0.5 秒和 1 分 1.5 秒的限额才起机；四个 run 都成功，没有机器遗留。没有真的收到过 InstaBox 的 429（收到后重试只在假 API 上测过）。
- **InstaBox 上的重启：**服务在 run 中途被杀（停的时候机器还在创建）后，下次启动把 run 重新排队，第 2 次尝试成功；run 进行中发 SIGTERM，服务约 15 秒后才退出（先删机器），日志写了 `queued again`，重启后 run 第 2 次尝试成功。两次都是 `bench-lint-docs`、run 开始后几秒就停；在长 run 的后段重启没试过。
- **日志保留**（`-log-retention-days 0.01`，在开发库上）：早于 14 分钟的 20 个 run 在启动时被删掉 766 行日志，日志路由回 410，run 显示 `log_purged: true` 且结论还在；12 分钟前的 run 日志还在。之后改回默认的 90 天。
- **artifact 记录：**用真实的上传协议和 SQLite 测过（上传、重启、列出、下载、过期清理）；真机只验证了现有数据库上的迁移和三个路由（空列表 200、404、404）。服务端上没有跑过上传 artifact 的 job：基准 workflow 没有一个用它，令牌也加不了 workflow。
- **定时触发：**只有单测（时钟是传进去的）。手边没有带 `schedule:` 的仓库，令牌也加不了，所以没看到过真实触发。
- **以 GitHub App 身份**（2026-11-02，开发用 App "Raccoon CI (example App)" 装在组织的 8 个仓库上，旧的仓库 webhook 已删）：用 `POST /v1/runs` 手动启动的 run 自己找到了 installation、读到 workflow 和提交，在 InstaBox 机器上跑成功（机器 9.8 秒就绪，总共 19 秒）。GitHub 上这个提交显示 `pending` 然后 `success` 的 status，以及 check run `raccoonci / <workflow> / <job>`（已完成，"Success in 18s"），都是 `example-app[bot]` 建的。一个 job 要 App 没有的权限（`packages: write`），拿到了 GitHub 真实的 422、退回默认 token，日志里有警告。对真实 GitHub（`TestRealAppTokens`，需要 `RACCOONCI_TEST_APP_ID`、`_APP_KEY_FILE`、`_REPO`、`_OTHER_REPO`、`_SHA`）：只对一个仓库的 token 能读它（200）、读同一安装下另一个私有仓库是 404；只读 token 写不了 status（403）；有 `statuses: write` 的能写（201）但读不了 contents；吊销后的 token 失效（GitHub 不是立刻生效，要等一小会儿）。runner-images 轮询也第一次在真实网络上跑，读到了三个镜像。GitHub 自己发的 push（用 API 建了再删一个分支）经隧道带签名和 installation id 到达，因为仓库唯一的 workflow 只监听 `workflow_dispatch`，被记为忽略。
- **没试过：** 桶（没有给开发用 R2 桶的访问密钥，所以 artifact 和缓存走 S3 的路径以及桶的检查只用假的测过）、真实 job 上传 artifact、由 GitHub 发出的投递建出的 run（这些仓库没有监听 push 的 workflow，也加不了）、真实的 pull request（它的合并 ref、fork）、机器上的 step 真的用 `GITHUB_TOKEN`（加 workflow 文件需要 `workflow` 权限，手边的 token 没有）、带 matrix 的 run。

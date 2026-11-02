# 开发时怎么运行服务端

[English](server-dev.md) — 两个版本要同步修改。

状态：`docs/server-design.md` 的第 1、2 步。服务端接收 GitHub 的 `push` 和 `pull_request` 投递（或者一个手动启动 workflow 的请求），判断它们会触发哪些 workflow，为每个存一条排队中的 run，按提交取回仓库，把每个 job 实例放到一台机器上跑，存下日志，并通过 API 展示 run 和日志。会把每个 run 作为 commit status 回报给 GitHub，也能取消和重跑 run。保存密钥和变量（在数据库里加密），只把 run 用到的那几个交给它，API token 有角色。还不发 check run（那需要 GitHub App），也不做用 GitHub 登录。

## 构建和启动

```sh
make build                       # dist/raccoonci 和 dist/raccoonci-server
raccoonci-server -allow owner/name [-allow owner/*] [-listen 127.0.0.1:18080] [-data-dir DIR] [怎么跑 job]
```

至少要一个 `-allow`：别的仓库的投递或请求会被拒绝或忘掉。数据库是 `DIR/server.db`（SQLite，启动时迁移）。被重启打断的投递在启动时会重新处理；正在进行中的 run 会被记为失败。

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

仓库 webhook（开发用）或 GitHub App（正式的）指向 `https://<你的主机>/webhooks/github`，内容类型 `application/json`，secret 就是 `RACCOONCI_WEBHOOK_SECRET`，事件选 `push` 和 `pull_request`。在没有公网地址的沙箱上开发时，这个地址是指向 `127.0.0.1:18080` 的 Cloudflare Tunnel。

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
- `secrets.GITHUB_TOKEN` 不是服务端生成的：这个名字的密钥是不允许的，所以它是空的。

## GitHub 上的状态

run 会在它所跑的提交上设置 commit status `raccoonci / <workflow 名>`：开始时 `pending`（"Running"），之后是 `success`、`failure` 或 `error`（"Cancelled"），带上耗时。它显示在提交上，pull request 的 checks 里也能看到。用普通 token（需要 `repo:status` 能力）或 GitHub App（需要 "Commit statuses" 权限）都行；`-status=false` 关闭。报告失败只记日志，不会让 run 失败。check run（每个 job 一行、带注解和摘要）只有 GitHub App 能写，还没做。

`/v1/openapi.yaml` 描述这一切；有一个测试保证它和路由一致。

## 定时触发

`on: schedule:` 的 workflow 由服务端触发（`-schedule=false` 则交给 GitHub）。和 GitHub 一样，只认仓库**默认分支**上的 workflow 文件，到点时 run 用的是该分支当时最新的提交（`github.event_name` 为 `schedule`，`github.event.schedule` 是触发的那条 cron，actor 是 `schedule`）。cron 按 UTC、五段，支持列表、范围和步长；一个 workflow 写了两条就是各自到点各出一个 run。

- 看哪些仓库：服务端收到过事件（投递或 dispatch）的仓库，加上 `-allow` 里写明全名的（`owner/*` 不算：通配里的仓库要等它的第一个事件才认得）。对一个仓库的定时信息缓存 5 分钟；到点时会在分支最新提交上重新读一次 workflow，那条 cron 必须还在。
- 一次触发会记成一个事件，id 由仓库、文件、cron 条目和分钟组成，所以重启（或两个服务端共用一个库）不会触发两次。服务端停着的时候错过的触发不补；卡顿了几分钟只出一个 run，不会一下子出一串。GitHub 自己在高峰时可能晚点或丢掉定时 run；这里误差在 15 秒的检查间隔以内。
- 没做：GitHub 对 60 天没有活动的仓库停掉定时任务的规则；没人给它发过事件、又不在 `-allow` 里的仓库发现不了。

## 起机限额

InstaBox 限制一个组织每分钟起机的数量（Pro 套餐 10 台），超了回 429。`-starts-per-minute`（默认 10，0 表示自己不计数）让服务端用 62 秒的滑动窗口数自己的起机次数，超了的排队等，所以一阵 run 会被摊开而不是失败；收到 429 后所有起机停 20 秒，被拒的那一次再问（最多约 7 分钟）。日志里会写 `waited … for the machine start limit`。计数只是服务端自己的：同一个 InstaBox 组织里别人起的机器，只能靠 429 才知道。

## 重启

服务停止时正在进行的 run 会**从头重新开始**，不是接着跑：收到 SIGTERM 时每个进行中的 run 被取消（机器删掉，要几秒），然后回到队列；崩溃的话，下次启动时把还在进行中的 run 放回队列。run 的日志里会有一条警告（"the server stopped while this ran; it is started again … attempt N"），前一次尝试的日志行保留，这期间 GitHub 上的状态一直是 `pending`。一个 run 最多被取走 3 次，之后记为失败。被人取消的 run 仍是已取消。崩溃之后旧一次尝试的机器已经不知道是哪些，由 InstaBox 在 2 小时期限到时删掉。

## 缓存，以及对象放在哪里

`actions/cache` 和 `setup-*` 的 `cache:` 选项都能用（v1 协议）。缓存不是所有人共用一份：每个 run 把缓存存进**它自己那个 ref 的空间**，恢复时从这个空间和**默认分支的空间**找。

- 默认分支有一个空间，只有默认分支的 run 能写。
- 其余的 ref（功能分支、tag、pull request 的合并 ref，不管是不是来自 fork）各有自己的空间，也能从默认分支的空间恢复，但读不到别的 ref 的空间，也写不进默认分支的空间。pull request 用默认分支用的那个 key 保存缓存，会落进它自己的空间，默认分支的那一条不变。
- 不同仓库之间永远不共用。和 GitHub 不同的是，pull request 不能从它的目标分支恢复，除非目标分支就是默认分支。
- 每个空间的上限是 `-cache-limit-gb`（默认 5），超了就淘汰最久没用的条目。没人再用的空间还不会被删除，上限是按空间算的，不是按仓库。

对象（artifact 和缓存）放在 `-data-dir` 下的目录里，或者放在用户自己提供的桶里：`-storage-s3-bucket B [-storage-s3-endpoint URL] [-storage-s3-region R] [-storage-s3-prefix P] [-storage-s3-path-style]`，加上 `RACCOONCI_STORAGE_S3_KEY` 和 `RACCOONCI_STORAGE_S3_SECRET`。用 Cloudflare R2 的话：endpoint 是 `https://<账号 id>.r2.cloudflarestorage.com`，region 填 `auto`。启动时会试一遍这个桶（写入、通过预签名链接读回、删除），桶不能用就拒绝启动；如果桶不用凭证就把对象给任何人，会在日志里警告。桶要保持私有。下载是直接指向桶的预签名链接，上传仍然经过服务端。这个桶是服务端自己的，给没有自己的桶的组织用。

**每个组织一个桶**（正式使用时用户这么做）：admin 通过 API 给出，之后这个组织各仓库的 artifact 和缓存就放进它的桶，不再进服务端的。

```sh
PUT    /v1/storage  {"org":"acme","bucket":"acme-ci","endpoint":"https://<账号>.r2.cloudflarestorage.com","region":"auto","prefix":"raccoon","access_key":"…","secret_key":"…"}
GET    /v1/storage?org=acme        # 在哪里，但不返回密钥
DELETE /v1/storage?org=acme        # 回到服务端自己的存储
```

保存设置之前会先试这个桶（写入、通过预签名链接读回、删除）：不能用的桶会被拒绝，并说明是哪一步失败；任何人都能读的桶会保存下来并给出警告。设置和密钥用主密钥加密，放在只有服务端读的一行里：workflow 引用不到它，密钥 API 也不能列出、覆盖或删除它。换桶之后，新的 artifact 和缓存进新桶；旧桶里的留在原处，对象不在新桶里的缓存条目在被查找时会被丢弃。需要主密钥。有自己桶的组织，它的 artifact 和缓存由按需创建的单独服务处理。

## 现在的 run 是什么样

- 提交是事件所指的那个；pull request 用的是它的**头提交**，不是 GitHub 会用的合并提交。
- 源码来自 GitHub 对该提交的归档：没有 `.git` 目录，没有历史。
- `secrets.GITHUB_TOKEN` 是空的。artifact 可用，但元数据在内存里，重启就忘（数据还在存储里）。cache 可用（见下一节）。
- `github.run_number` 就是 run 的 id。

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
- **定时触发：**只有单测（时钟是传进去的）。手边没有带 `schedule:` 的仓库，令牌也加不了，所以没看到过真实触发。
- **没试过：** 桶（没有给开发用 R2 桶的访问密钥，所以 artifact 和缓存走 S3 的路径以及桶的检查只用假的测过）、真实 job 上传 artifact、由 GitHub 发出的投递建出的 run、真实的 pull request、App 对真实 GitHub 的认证、真实 job 里的 artifact、带 matrix 的 run。

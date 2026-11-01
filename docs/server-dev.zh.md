# 开发时怎么运行服务端

[English](server-dev.md) — 两个版本要同步修改。

状态：`docs/server-design.md` 的第 1、2 步。服务端接收 GitHub 的 `push` 和 `pull_request` 投递（或者一个手动启动 workflow 的请求），判断它们会触发哪些 workflow，为每个存一条排队中的 run，按提交取回仓库，把每个 job 实例放到一台机器上跑，存下日志，并通过 API 展示 run 和日志。会把每个 run 作为 commit status 回报给 GitHub，也能取消和重跑 run。还不保存密钥、不发 check run（那需要 GitHub App）、不做用户登录。

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
| `RACCOONCI_API_TOKEN` | `/v1` 的 bearer token（没设的话 API 返回 503） |
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

## GitHub 上的状态

run 会在它所跑的提交上设置 commit status `raccoonci / <workflow 名>`：开始时 `pending`（"Running"），之后是 `success`、`failure` 或 `error`（"Cancelled"），带上耗时。它显示在提交上，pull request 的 checks 里也能看到。用普通 token（需要 `repo:status` 能力）或 GitHub App（需要 "Commit statuses" 权限）都行；`-status=false` 关闭。报告失败只记日志，不会让 run 失败。check run（每个 job 一行、带注解和摘要）只有 GitHub App 能写，还没做。

`/v1/openapi.yaml` 描述这一切；有一个测试保证它和路由一致。

## 现在的 run 是什么样

- 提交是事件所指的那个；pull request 用的是它的**头提交**，不是 GitHub 会用的合并提交。
- 源码来自 GitHub 对该提交的归档：没有 `.git` 目录，没有历史。
- 没有密钥，所以 `secrets.GITHUB_TOKEN` 是空的。没有 cache 服务（job 的表现和服务不存在时一样）；artifact 可用，但元数据在内存里，重启就忘。
- `github.run_number` 就是 run 的 id。

## 目前验证过的

- 单元测试（`-race`）：签名、重复投递、允许列表、活动类型、fork、过滤器、重启后的恢复；存储；用假机器测的执行器（输出在 job 之间传递、失败的 job 挡住依赖它的 job、机器出错、关机时取消、同时运行数上限）；源码归档重新打包后用真正的解包器验证；API，包括 SSE 流，以及规格和路由一致的测试。
- 把 `cli/cli` 一次真实 push 的签名回放经 Cloudflare Tunnel 发给服务端，用真实 GitHub API：读到 15 个 workflow 文件，建了 3 个 run，和 GitHub 自己在那个提交上跑的完全一致。
- **一个完整的 run，端到端**（`-local-runner`）：经隧道 `POST /v1/runs` 启动 `bench-lint-docs` 的 workflow；服务端从 GitHub 取回真实归档、解析出 job，用真正的 `raccoonci run-instance` 跑（checkout、`setup-node`、三个 `run` 步骤），存下 62 行日志和一个成功的 job，API 能读到；总共 4 秒。同一份日志经隧道以 server-sent events 实时推送也验证了。
- **同一个 run 放到 InstaBox 机器上跑**（runner 快照 `raccoonci-runner-9c482bc`，机器经隧道的 `/agent` 回连服务端）：成功，机器 9.8 秒就绪，从请求到 run 结束 16 秒，日志里是同样的步骤，结束后机器已被删除。
- **GitHub 自己发出的投递**（`bench-lint-docs` 上的仓库 webhook，经隧道）：ping 回 200；两次 push，以及一个 pull request 的打开和关闭，都带着签名到达，回 202，用真实 API 读了该提交上的 workflow，并记为已忽略（仓库里唯一的 workflow 只监听 `workflow_dispatch`）。token 没有 `workflow` 权限，没法加一个由 push 触发的 workflow，所以还没看到由真实投递建出来的 run。
- **InstaBox 上的状态、取消、重跑：** run 的提交在 GitHub 上先 `pending` 后 `success`（"Succeeded in 27s"）；5 秒时取消的 run 记为 cancelled，状态是 `error`；重跑重复了第一次（`rerun_of`）并成功。取消的那次暴露了一个泄漏：机器还在创建，删除被拒绝（409 `operation_conflict`）后就放弃了，机器一直跑到 2 小时的期限。现在 `instabox.Client.DeleteMachine` 会等待后重试；真机验证过（创建中取消后没有残留机器）。
- **没试过：** 由 GitHub 发出的投递建出的 run、真实的 pull request、App 对真实 GitHub 的认证、真实 job 里的 artifact、带 matrix 的 run。

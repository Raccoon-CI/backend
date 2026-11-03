# backend

[English](README.md)

[Raccoon CI](https://github.com/Raccoon-CI) 的服务端。Raccoon CI 是自托管的 CI/CD 平台，把 GitHub Actions 的 workflow 搬到你自己选的算力上跑。`raccoonci-server` 接收 GitHub 的 webhook（或手动启动 workflow 的请求），决定哪些 workflow 要跑，把每个 job 实例放到算力提供方的机器上执行（先支持 InstaBox），并保存 run、日志、artifact 和缓存。结果以 commit status 和 check run 回报给 GitHub，也通过 JSON API 提供。

本仓库包含 `cmd/raccoonci-server`、`internal/server/*`、`deploy/` 里的部署文件和 `docs/` 里的服务端文档。模块：`github.com/Raccoon-CI/backend`。许可证：Apache-2.0（见 `LICENSE`；链接进来的依赖的许可证在 `THIRD_PARTY_NOTICES`）。

## API 契约（给前端）

前端以 **[`internal/server/api/openapi.yaml`](internal/server/api/openapi.yaml)** 为准。服务端在 `GET /v1/openapi.yaml` 提供同一个文件，并且 `internal/server/api` 里有测试：只要有路由没写进去就失败，所以这个文件就是契约。改路由要在同一个提交里改这个文件。

## 构建、测试、运行

```sh
make build          # dist/raccoonci-server（VERSION=1.2.3 设置版本）
make check          # go vet + go test -race ./...
raccoonci-server -allow owner/name [-allow owner/*] [-listen 127.0.0.1:18080] [-data-dir DIR] [怎么跑 job]
```

至少要一个 `-allow`（别的仓库的投递会被拒绝）。数据库是 `DIR/server.db`（SQLite，启动时迁移）。没有指定怎么跑 job 时，run 一直排队：

- InstaBox：`-public-url https://…`（机器回连服务端的地址）、`-snapshot ID`（runner 快照；也可用 `RACCOONCI_INSTABOX_SNAPSHOT`），以及放在 `INSTABOX_API_KEY` 或 `IBX_API_KEY` 里的 InstaBox 密钥。
- `-local-runner 路径`：job 跑在服务端这台机器上，用同一个用户，**没有任何隔离**——只用于开发和可信代码。路径是由 cli 仓库构建的 `raccoonci` 二进制。

环境变量（密钥类：写成 `NAME`，或 `NAME_FILE` 指向只有你能读的文件，模式 600；从不用命令行参数）：

| 名称 | 含义 |
|------|---------|
| `RACCOONCI_WEBHOOK_SECRET` | 校验 GitHub 投递的签名（必需） |
| `RACCOONCI_API_TOKEN` | `/v1` 的引导 bearer token（admin 角色），其他 token 通过 API 创建 |
| `RACCOONCI_MASTER_KEY` | 32 字节（`openssl rand -hex 32`），用来加密数据库里的密钥；没有它，secrets 和 variables 功能关闭。轮换时用 `RACCOONCI_MASTER_KEY_OLD` |
| `RACCOONCI_GITHUB_APP_ID`、`RACCOONCI_GITHUB_APP_KEY_FILE` | 以 GitHub App 身份工作（check run、每个 job 的 `GITHUB_TOKEN`） |
| `RACCOONCI_GITHUB_TOKEN` | 不用 App 时：一个 token，仅用于开发 |
| `RACCOONCI_GITHUB_CLIENT_ID`、`RACCOONCI_GITHUB_CLIENT_SECRET` | 用 GitHub 登录（OAuth） |
| `INSTABOX_API_KEY` / `IBX_API_KEY` | InstaBox 密钥 |
| `RACCOONCI_INSTABOX_SNAPSHOT` | runner 快照（等同 `-snapshot`） |
| `RACCOONCI_STORAGE_S3_*` | 服务端自己的 S3 桶，存 artifact 和缓存 |
| `RACCOONCI_CATALOG_URL`、`RACCOONCI_CATALOG_KEY` | catalog feed 的地址，以及必须由它签名的公钥（有内置默认值） |

`raccoonci-server -h` 列出全部参数。详情见：[docs/server-dev.zh.md](docs/server-dev.zh.md)（开发时怎么跑）、[docs/deploy.zh.md](docs/deploy.zh.md)（systemd 或容器、反向代理、备份）、[docs/github-app.zh.md](docs/github-app.zh.md)（注册 GitHub App）、[docs/server-design.zh.md](docs/server-design.zh.md)（设计）。`deploy/` 里有 `Dockerfile`、`raccoonci-server.service` 和 `Caddyfile`。

## 其他仓库

| 仓库 | 内容 |
|---|---|
| lib | 共用库（`pkg/*`：workflow、expr、trigger、catalog、dispatch 等）——**本仓库依赖它** |
| raccoonci-runner | 机器一侧：引擎和 `raccoonci-runner` |
| cli | `raccoonci` 命令行 |
| 前端 | 通过 `openapi.yaml` 与本服务端通信 |

依赖方向：lib ← runner ← cli，以及 lib ← backend。backend 不会引用 runner 或 cli（有测试守着）；它与机器上的 agent 只通过 `dispatch` 协议相遇。

## 尚未发布

`go.mod` 里写的是 `lib v0.1.0`，这个标签还不存在。在它出现之前：

- 用四个仓库目录旁边的工作区 `go.work` 构建和测试（它把未发布的版本替换成本地目录）。现在不要在这里跑 `go mod tidy`。
- 本仓库的 CI（`.github/workflows/ci.yml`）会失败，`docker build -f deploy/Dockerfile .` 也会失败（容器里 `go mod download` 取不到 lib）。代码没有为此做任何改动；lib 打上标签后，跑一次 `go mod tidy`，两者就都能用了。

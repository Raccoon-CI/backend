# 服务端设计（草案）

[English](server-design.md) — 两个版本要同步修改。

状态：2026-11-01。"工作顺序"的第 1、2 步已经做了（事件入口、run 记录、源码、在机器上执行、日志；跑了什么、验证了什么见 `docs/server-dev.zh.md`），其余还没有。这是 TODO 里 C.1 的计划，写在拆分成独立仓库（raccoonci-frontend、-backend、-runner、-cli，外加一个共用库仓库）之前。服务端先在本仓库里写，遵守下面的"边界"，这样以后拆分只是搬代码，不是重写。

## 服务端是做什么的

自有引擎（`raccoonci run`）在一台机器、一个进程里工作。服务端把它变成平台：接收 GitHub 的事件，决定哪些 workflow 要跑，在算力提供方的机器上执行，保存结果，展示并回报。GitHub 触发的路径（`raccoonci serve`、`internal/ghrunner`）不需要服务端，因为那条路径上 GitHub 就是服务端。它保持原样，以后可以作为 backend 里的一个模块运行；两条路径都不删，也都不叫"那个"架构。

## 边界（让拆分变便宜的规则）

- 服务端代码放在 `internal/server/…`。不引用 `cmd/…`，也不引用别的使用方的代码（`ghrunner` 是独立的）。
- 只能引用将来会进共用库仓库的包：`value`、`workflow`、`expr`、`trigger`、`plan`、`catalog`、`scan`、`affected`，以及 `dispatch` 的协议类型。需要这个清单之外的东西时，先决定它放在哪里再用。
- 对外的一切都放在服务端自己定义的小接口后面：数据库（`store`）、对象存储（`blob.Store`，已有）、算力（`Provider`，`instabox` 里已有）、源码（`Source`）、GitHub（`GitHub`）。测试用的假实现放在接口旁边。
- 前端和 CLI 只通过 HTTP API 和服务端通信：JSON over HTTP，仓库里放一份 OpenAPI 文件，日志用 server-sent events。和前端不共用 Go 类型。
- 控制器与 agent 之间的协议（`internal/dispatch`）是带版本号的公开协议。现在 agent 跑的是 `raccoonci run-instance`；拆分后 runner 仓库实现 agent 一侧，backend 实现控制器一侧，两边只通过这份协议接触。

## 组成部分

1. **事件入口。** 一个 GitHub App（webhook 事件 `push`、`pull_request`、`create`、`delete`，`workflow_dispatch` 通过 API；需要 Checks 和 Contents 权限）。每次投递先验签（HMAC-SHA256，常量时间比较）再读取，原始载荷连同投递 id 一起存下来（重复投递不会重复处理），然后处理。`schedule:` 由服务端自己的 cron 触发（解析器是 `internal/cron`，将来进库；`internal/server/schedule` 负责到点触发）。
2. **触发匹配。** 对事件所在提交上的每个 workflow 文件调用 `trigger.Match`；变更文件来自 compare 接口（未知就表示"不过滤"）。匹配上就创建一个 run。`workflow_dispatch` 来自 API，输入有类型（`trigger.DispatchInputs`）。
3. **run 和 job。** run 记录事件、提交、该提交上的 workflow 文件原文（存下来，重跑用的是同一份文本）和它的 job 实例。状态：queued、in progress、completed，加一个结论。第一版把 engine 放在进程内（见"engine 与重启"）。
4. **源码。** 通过 App 的 installation token 取该提交的 tarball，按 `dispatch.TarDir` 的布局重新打包，agent 收到的和今天收到的一样。pull request 用合并 ref（`refs/pull/N/merge`），它在事件之后过一会儿 GitHub 才生成；服务端短时间重试，之后退回头提交，并在 run 里说明。自己把 base 合并进 head 不在范围内。
5. **执行。** `engine.Run` 配上 `Options.Remote`，和今天的 `run -on instabox` 完全一样，区别是控制器、artifact/cache 服务和算力提供方属于服务端，而不是某个命令。agent 协议和 `instabox.Provider`/`Pool` 直接复用。
6. **日志与结果。** 每个实例的每个 `runner.Event` 按块、按顺序追加存储（runner 已经打过码），并推给订阅者。实时查看用 server-sent events，从某个块偏移量开始；已结束的日志是普通读取。job 输出和结果进数据库。
7. **artifact 与 cache。** `internal/artifact` 和 `internal/cache` 换成持久的元数据存储（用数据库替代内存和 `index.json`）和 S3 兼容的对象存储，由**用户自己提供**，按组织配置：Cloudflare R2、AWS S3 或任何 S3 兼容存储（`blob.S3` 配 `Endpoint` 和 `PathStyle`）。项目不替任何人托管数据，也没有生产环境的默认桶。没配桶时服务端退回本地目录（`blob.Dir`），够单机和试用。配置按密钥保存（第 9 项），保存时做一次检查：写入、预签名、读取、删除一个小对象，哪一步失败就给出明确的错误。我们自己的开发环境通过环境变量用维护者的 R2 桶，那只是测试用的夹具，不是默认值。下载是预签名链接，不经过服务端（R2 也不收出站流量费）。保留期由服务端自己删除来实现，不能依赖用户可能没设的生命周期规则。上传仍然要经过服务端，因为 toolkit 说的是 Azure Blob 的分块协议，没法直接给它一个 R2 地址；接收上传的主机不能放在 Cloudflare 代理后面（免费套餐单个请求限 100 MB）。这就是 TODO B.3 里"等有服务端再做"的那份清单：持久化、保留期、按分支隔离的缓存、每个 index 只有一个写入者。
8. **回报给 GitHub。** 每个 job 实例一个 check run，走 Checks API（状态、结论、标题、来自 `$GITHUB_STEP_SUMMARY` 的摘要，有 problem matcher 之后再加 annotations）。check run 的更新最多每隔几秒一次。
9. **密钥与变量。** 静态加密，信封密钥来自服务端配置，按组织和仓库区分；只在派发实例时解密；不进日志也不进 API 响应；机器拿不到桶或算力提供方的密钥。来自 fork 的 pull request 拿不到密钥。
10. **认证。** 用户通过 GitHub 登录（App 的 OAuth）；API 通过 GitHub 确认用户能否看到这个仓库。CLI 用个人 API token。机器用 `dispatch` 的每任务 token。
11. **API。** run、job、日志、artifact、重跑、取消、workflow dispatch、密钥（只写不读）、启用的仓库。前端和 CLI 是它的客户端，别无他途。

## 对象存储的安全

- 桶是私有的。绝不开公共读，不用 `r2.dev` 公共地址，也不给它绑自定义域名。配置检查（第 7 项）会尝试发现可公开读取的桶并给出警告。
- 机器永远不持有桶的凭证。服务端验过它的 token 之后才发预签名链接，有效期按分钟算（现在代码里 `downloadTTL` 是一小时，服务端版要调低），链接不进日志。
- 对象的键带仓库前缀，一个仓库读不到另一个仓库的 artifact 和缓存。
- 缓存按分支和信任级别隔离：来自 fork 的 pull request 既不能写入之后受信任的 ref 会恢复的缓存，也不能读它不该看到的受保护分支的缓存（TODO B.3 里"按分支隔离"那一项在这里是必做，不再搁置）。
- 上传链接绑定到单个 job 的 token 和自己的域（缓存用 `~cache`），所以一条上传链接不能写到别的地方。

## 开发环境

服务端在没有入站地址的沙箱上开发期间，webhook 和 agent 的接口通过 Cloudflare Tunnel 对外（沙箱上跑 `cloudflared`，用维护者自己域名下的一个主机名）。这个隧道、它的 DNS 记录和 token 只为调试而建，调试结束就删除。隧道经过 Cloudflare 的代理，所以超过 100 MB 的请求（大的缓存上传）会失败；正式环境没有这个限制（VPS，上传用只做 DNS 的主机名）。

## engine 与重启

`engine.Run` 是内存里的 goroutine 图。服务端第一版保留这个设计：每个 run 一个 goroutine，实例通过 `Options.Remote` 发出去，事件边来边存。服务被停（SIGTERM）时，正在跑的 run 会被取消、机器删掉，然后 run 放回队列；如果服务是崩溃或被杀，下次启动时把还在"进行中"的 run 放回队列。两种情况下 run 都**从头重来**，日志里有一条警告，`runs.attempts` 记执行器取走它几次：到 3 次（`Executor.MaxAttempts`）就记为失败，所以一个会弄垮服务的 run 不会无限循环。从中间接着跑（保留已完成的 job）需要由数据库驱动的调度器，只有整个 run 重来真的造成困扰时才值得做。崩溃之后上一次的机器已经不知道是哪些，只能等它们的 `deleteAfterSeconds` 到期。这也意味着第一版只以单实例运行。

## 存储

一个 `store` 接口，包含服务端需要的操作（事件、run、实例、日志块、artifact 和 cache 元数据、密钥、installation）。第一个实现用 SQLite（纯 Go 驱动，二进制保持静态，自托管不用装别的），等服务端需要多实例时再做第二个实现 PostgreSQL。迁移是带编号的 SQL 文件，嵌进二进制。两个实现跑同一套测试。

## 工作顺序

1. **骨架与入口。** 配置、数据库和迁移、带验签的 webhook 端点、事件和 run 记录、通过 API `GET` run。先不执行。测试：回放录下来的投递。
2. **执行。** 拉源码，engine 配上 dispatch 控制器和 InstaBox provider，日志存储并推流。第一个端到端：往测试仓库 push，run 出现，job 在机器上跑，日志可读。测试对象：本仓库自己的 `ci.yml`。
3. **回报。** job 的 check run，重跑和取消。
4. **服务持久化。** artifact 和 cache 进数据库和 S3。
5. **密钥与认证。** 别人用之前必须有。
6. **定时与 dispatch。** cron（`internal/server/schedule`）、带输入的 `workflow_dispatch`（`POST /v1/runs`）。已做。
7. **加固。** 重启行为、配额、保留期、工作目录清理（TODO C.1）。

前端在第 2 步之后开始，那时 API 里有 run 和日志可展示。在此之前，对着一个不存在的 API 写前端不值得。

## 这里先定下的决定，第 1 步写出来之前都容易改

- Go，单个二进制，先 SQLite，再 PostgreSQL。
- 第一版 engine 在进程内；被重启打断的 run 重新排队（最多 3 次）。
- GitHub App 是唯一入口（这条路径不轮询）。
- 文档和 API 用中英两个版本，因为仓库会开源。

## 需要用户做的

- 要创建并安装一个 GitHub App（名字、webhook URL 和 secret、权限、私钥）。私钥和 webhook secret 通过环境变量或文件交给服务端，不打印也不提交；我不会在对话里要它们。
- webhook 需要一个公网地址，和 `serve -webhook-url` 一样：一台（新加坡的）VPS，加域名和 HTTPS，两个主机名（一个走 Cloudflare 代理，给 webhook 和 API；一个只做 DNS，给 agent 和上传接口）。
- 只在开发阶段：维护者自己的 R2 存储桶和对应的 S3 API token，通过环境变量或文件交给服务端。做好之后，使用者自带存储桶。
- 服务端最终跑在哪里（需要能出站访问 GitHub 和 InstaBox，并开放一个入站端口）。

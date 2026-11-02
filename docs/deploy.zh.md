# 部署服务端

[English](deploy.md) — 两个版本要保持一致。

怎么把 `raccoonci-server` 跑在一台自己的机器上：做成 systemd 服务，或者做成容器，前面放一个带 HTTPS 的反向代理。文件在 `deploy/`。开发时怎么跑见 [server-dev.zh.md](server-dev.zh.md)；它需要的 GitHub App 见 [github-app.zh.md](github-app.zh.md)。

## 需要什么

- 一台 Linux 机器，能出站 HTTPS 到 `api.github.com` 和算力提供方（InstaBox），再有一个指向它的域名。入站只开反向代理的 80 和 443；服务端自己监听 `127.0.0.1:18080`，不应该能从外面直接访问。
- 一个装在那些仓库上的 GitHub App（[github-app.zh.md](github-app.zh.md)），webhook 地址是 `https://<域名>/webhooks/github`。
- 同一个域名也是机器上的 agent 回连的地址（`-public-url`），和 API 的地址。
- 没测过：它需要多少 CPU、内存和磁盘。二进制 17 MB，容器镜像 30 MB；开发用的服务端跑了大约十个 run 之后，数据库是 200 KB。磁盘会随放在本机的 artifact 和缓存增长（`-storage-s3-*` 或每个组织一个桶可以把它们移到对象存储），也随日志增长（`-log-retention-days`）。

## 密钥

每个密钥是一个环境变量 `NAME`，或者一个由 `NAME_FILE` 指向、只有所有者能读的文件（模式 600 或 400；服务端拒绝权限更松的文件并说明）。绝不用命令行参数。用 root 一次性做好：

```sh
sudo install -d -m 0700 /etc/raccoonci
cd /etc/raccoonci
openssl rand -hex 32 | sudo tee webhook.secret >/dev/null   # 同一串填进 App 的 "Webhook secret"
openssl rand -hex 32 | sudo tee api.token >/dev/null        # API 的引导管理员 token
openssl rand -hex 32 | sudo tee master.key >/dev/null       # 用来封存数据库里的密钥
sudo cp /path/to/downloaded-app-key.pem app.pem             # App 的私钥
sudo chmod 600 *
```

**把 `master.key` 另外备份一份。** 没有它，数据库里的密钥、变量和桶设置就打不开，也没有任何办法找回。

`/etc/raccoonci/server.env` 放不是密钥的东西：

```sh
RACCOONCI_GITHUB_APP_ID=1234567
# 命令行里可变的部分（见 raccoonci-server -h）。-allow 说明服务端为哪些仓库工作；
# -public-url 和 -snapshot 让它能在 InstaBox 上跑 job（还需要 INSTABOX_API_KEY_FILE）。
RACCOONCI_ARGS=-allow my-org/* -public-url https://ci.example.com -snapshot sn_xxxxxxxxxxxx
```

## 做成 systemd 服务

```sh
make build VERSION=1.2.3          # dist/raccoonci-server
sudo install -m 0755 dist/raccoonci-server /usr/local/bin/
sudo install -m 0644 deploy/raccoonci-server.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now raccoonci-server
journalctl -u raccoonci-server -f
```

`deploy/raccoonci-server.service` 做了什么：以动态用户运行，状态放在 `/var/lib/raccoonci-server`（模式 700）；密钥用 systemd 凭据交给它（`LoadCredential`：由 root 从 `/etc/raccoonci/` 读，以文件的形式放在一个私有目录里给服务用，所以不在环境变量里、不在 `systemctl show` 里、也不在命令行里）；并限制进程（不能提权、系统只读、没有 home、没有 capability、只能用 IP 和 Unix socket、系统调用过滤、不允许既可写又可执行的内存）。`systemd-analyze security` 给它的评级是 1.2 OK。停止给 90 秒，因为收到 SIGTERM 时服务端要取消在跑的 run、删掉它们的机器、再把 run 放回队列。

检查过的：这个文件通过 `systemd-analyze verify`；真正的二进制在同样的限制下（系统调用过滤、不允许可写可执行内存、地址族、不能提权）能运行并响应，并且像真正的单元文件那样通过 `%d` 从 `LoadCredential` 的文件里读到密钥。没检查的：需要系统管理器的那些指令（`DynamicUser`、`StateDirectory`、`ProtectSystem`、`ProtectHome`、`CapabilityBoundingSet`）没有跑过，因为在开发机上装系统服务没有被允许。第一次启动后请看 `journalctl`，并把看到的告诉我们。

不要在这个单元下用 `-local-runner`：它在服务端自己的机器上以同一个用户、不隔离地跑 job，那是给开发用的。

## 做成容器

```sh
docker build -f deploy/Dockerfile --build-arg VERSION=1.2.3 -t raccoonci-server .
# App 的私钥是个文件，服务端（容器里的用户 65532）必须能读它，而且只有它能读：
# 一份属主为 65532、模式 400 的拷贝。
sudo install -o 65532 -g 65532 -m 0400 /etc/raccoonci/app.pem /etc/raccoonci/app.container.pem
docker run -d --name raccoonci --restart unless-stopped \
  -p 127.0.0.1:18080:18080 -v raccoonci-data:/data \
  -e RACCOONCI_WEBHOOK_SECRET=... -e RACCOONCI_API_TOKEN=... -e RACCOONCI_MASTER_KEY=... \
  -e RACCOONCI_GITHUB_APP_ID=1234567 \
  -v /etc/raccoonci/app.container.pem:/run/secrets/app.pem:ro -e RACCOONCI_GITHUB_APP_KEY_FILE=/run/secrets/app.pem \
  raccoonci-server -allow 'my-org/*' -public-url https://ci.example.com
```

镜像里只有静态二进制和 CA 证书，以非 root 用户 65532 运行，数据放在 `/data` 卷里（新卷会沿用 `/data` 的属主 65532）。放在环境变量里的密钥，谁能运行 `docker inspect` 谁就看得到；用文件的话必须**只有服务端能读**（模式 400 或 600，属主 65532）。属主是 root 的文件（宿主机上的私钥就是，会 `permission denied`），以及 Docker 和 Kubernetes 通常的密钥文件（root、模式 444，会被拒绝并提示 "readable by others"）都不行。把宿主机的目录挂成 `/data` 时，要先 `chown 65532`。

检查过的：镜像的运行阶段（用在外面编好的二进制构建）能启动，在新卷里写数据库，响应 `/healthz` 和 API，重启后数据还在，拒绝别人可读的密钥文件，读不了属主为 root 的私钥，能读属主 65532、模式 400 的私钥（用的是卷里的文件；绑定挂载的文件，属主和模式一样，对容器来说是同一回事，但没试过）。**构建阶段在这里没跑**：开发机的 TLS 代理让容器里的 `go mod download` 失败（"certificate signed by unknown authority"），和这个文件无关；构建命令本身就是在宿主机上用的那条。

## 反向代理

`deploy/Caddyfile` 就是用 Caddy 的完整配置（Let's Encrypt 自动 HTTPS）：一个域名、很大的请求体上限、不缓冲响应。它通过了 `caddy validate`。换别的代理，要设同样三件事——把 `/` 转给 `127.0.0.1:18080`；允许几百 MB 甚至更大的请求体（artifact 和缓存是经服务端分块上传的）；不缓冲响应（run 的日志是一路 server-sent events 的流）。没用 nginx 试过。

如果域名放在 Cloudflare 的代理后面，超过 100 MB（免费套餐的上限）的上传会失败。给机器用的地址单独一个不走代理的域名（只做 DNS），把它作为 `-public-url`；webhook 和 API 可以留在代理后面。

## 第一次启动

1. `curl https://ci.example.com/healthz` 回 `{"status":"ok","version":"…"}`。
2. 在 GitHub App → Advanced → Recent Deliveries 里，`ping` 显示 200。
3. `curl -H "Authorization: Bearer $(sudo cat /etc/raccoonci/api.token)" https://ci.example.com/v1/installations` 列出 App 装在哪里（启动时从 GitHub 读的）。
4. 往一个装了 App 的仓库 push，或者 `POST /v1/runs`（见 server-dev.zh.md）：run 会出现在 `/v1/runs` 里。

## 升级

装上新的二进制（或镜像）再重启。数据库的迁移在启动时跑；新版服务端写过的数据库，旧版会拒绝打开，所以新版启动过之后不能回到旧二进制——要回去只能恢复备份。重启时在跑的 run 会重新排队，从头开始（最多 3 次）。旧服务端没来得及删的机器到期会自己消失。

## 备份与恢复

要保存的：`/var/lib/raccoonci-server`（也就是 `/data` 卷）、`/etc/raccoonci/master.key` 和别的密钥（和数据分开放）、服务端的命令行参数。

服务端运行时，用任何 SQLite 客户端做一份一致的数据库拷贝：

```sh
sqlite3 /var/lib/raccoonci-server/server.db "VACUUM INTO '/backup/server-$(date +%F).db'"
chmod 600 /backup/server-*.db
```

这条在服务端运行时对开发库试过：`PRAGMA integrity_check` 是 ok，拷贝里有 run、事件、安装、用量和 token。放在本机磁盘上的 artifact 和缓存在数据目录的 `artifacts/` 和 `caches/` 下（放在桶里就没有要拷的）；日志、run 和结果都在数据库里。

恢复时，停掉服务端，把拷贝作为 `server.db` 放进数据目录（删掉那里的 `server.db-wal` 和 `server.db-shm`），再启动：服务端启动时会把这些文件改成只有它的用户能读。这个恢复在一份拷贝上试过：run 和安装都在。没有同一份 `master.key`，拷贝里的密钥打不开。

## 要知道的

- 数据库和它的文件每次启动都被改成只有服务端的用户可读（模式 600），数据目录能被其他用户进入时服务端会警告：里面有 token 的哈希、GitHub 投递的载荷、日志和封存的密钥。
- 一个数据目录只能有一个服务端。两个服务端用同一个目录不受支持。
- 这里没有设置防火墙、journald 之外的日志轮转、监控或告警：服务端有 `/healthz`（进程在）和 `/readyz`（能干活：数据库能写入，数据盘剩余空间大于 `-min-free-mb`；不行时回 503 并写出哪项检查失败，原因只进日志；不需要 token），日志写到标准错误，目前能盯的就这些。Docker 镜像的 `HEALTHCHECK` 运行 `raccoonci-server -check http://127.0.0.1:18080/readyz`；代理或监控的探测请指向 `/readyz`。

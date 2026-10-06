# InferGate 观测栈配置（`deploy/`）

这个目录只放**配置**，不放任何 Go 代码，也不放二进制：M5（全链路可观测）的采集侧与展示侧配置都在这里。
Prometheus 与 Grafana 都是**外部程序**——仓库里没有 vendored、也没有联网下载，你需要自己准备好这两份二进制（或镜像），
再把这里的配置文件喂给它们。配置本身与网关的 `/metrics` 契约（见根 `README.md` 与 `internal/metrics`）是一一对应的。

> 一句话：网关只负责在 `GET /metrics` 上吐手写的 Prometheus 文本；**抓取、存储、查询、画图全是外部的事**。

## 1. 目录结构

```
deploy/
├─ README.md                               ← 本文件
├─ prometheus/
│  ├─ prometheus.yml                       ← scrape 配置（本机形态）：两个 job（single / fleet）
│  └─ docker.yml                           ← scrape 配置（容器形态）：target 是服务名 gateway:8080
└─ grafana/
   ├─ provisioning/
   │  ├─ datasources/prometheus.yml        ← 自动注册数据源（uid=PROMETHEUS，url 127.0.0.1:9090）
   │  └─ dashboards/infergate.yml          ← 自动注册看板目录（provider）
   ├─ datasources-docker/
   │  └─ prometheus.yml                    ← 容器形态的数据源（同一个 uid，url 换成 prometheus:9090）
   └─ dashboards/
      └─ infergate.json                    ← 看板本体（uid=infergate，33 个 panel）
```

两份 scrape 配置与两份 datasource 的区别只有一个词，但换错了就是"target DOWN / 所有 panel 空白"：
本机形态里 Prometheus 自己就是 `127.0.0.1`，容器形态里 `127.0.0.1` 指的是 Prometheus 容器**自己**。
`docker-compose.yml` 挂的是 `docker.yml` + `datasources-docker/prometheus.yml`；第 3、4 节讲的本机直跑挂的是另外两份。
两者的 `external_labels.host` 不同（`windows-dev` / `docker-desktop`），所以两套数据进同一个 Prometheus 也能分开看。

三份 YAML 都是 2 空格缩进、无 Tab（YAML 里 Tab 是非法的）；看板 JSON 是纯 ASCII（中文全部写成 `\uXXXX`），
原因见第 8 节——这是为了让 Windows PowerShell 5.1 的 `Get-Content`（默认按 gb2312 解码无 BOM 文件）也能直接解析它。

## 2. 抓取目标：为什么有 single 和 fleet 两个 job

`deploy/prometheus/prometheus.yml` 里有两个 job：

| job | 目标 | 什么时候用 |
| --- | --- | --- |
| `infergate-single` | `127.0.0.1:18999` | 只起**一个**网关时（M5 的大多数单实例测量） |
| `infergate-fleet` | `127.0.0.1:18999`、`19000`、`19001`、`19002` | 四实例水平扩展那种测量：一个 mock 上游 + 四个网关进程 |

两个 job 都抓 `metrics_path: /metrics`，`scrape_interval: 5s`、`scrape_timeout: 4s`（超时必须小于间隔，否则一次抓取会跨到下一次），
`evaluation_interval: 15s`。`external_labels` 给所有时间序列打上 `cluster=infergate-local`、`host=windows-dev`、`milestone=m5`，
将来如果真的要异地/多机汇总，靠这几个标签区分来源。

**注意 18999 同时出现在两个 job 里**：如果你同时把 single 和 fleet 两个形态都跑起来（或四个实例里恰好有一个监听 18999），
那么 `sum(rate(infergate_requests_total[1m]))` 会把同一个进程数两遍。看集群聚合 QPS 时请带 `job` 过滤，例如
`sum(rate(infergate_requests_total{job="infergate-fleet"}[1m]))`，或者干脆只跑一种形态。

配置里还有一段**注释掉的** mock 上游 job：`cmd/mockupstream` 今天**不暴露** `/metrics`，所以那段只是留个位置，
哪天真加了指标，把注释去掉并把端口改成 mock 实际监听的端口即可。

## 3. Prometheus：怎么跑（外部二进制，本仓库不含）

### 3.1 Windows 本机

假设你已经把 `prometheus-2.53.0.windows-amd64.zip` 解压到 `C:\tools\prometheus-2.53.0.windows-amd64`（这一步得你自己做，
本机没有联网下载过它）：

```powershell
# 建议先只做语法检查（promtool 与 prometheus.exe 同目录）
cd C:\tools\prometheus-2.53.0.windows-amd64
.\promtool.exe check config "C:\Users\20106\Desktop\infergate\deploy\prometheus\prometheus.yml"

# 真正起来：--config.file 指过来，数据目录与监听地址都显式给死
.\prometheus.exe `
  --config.file="C:\Users\20106\Desktop\infergate\deploy\prometheus\prometheus.yml" `
  --storage.tsdb.path="C:\Users\20106\Desktop\infergate\tmp\prom-data" `
  --web.listen-address="127.0.0.1:9090" `
  --log.level=warn
```

起来之后：

* `http://127.0.0.1:9090/targets` —— 看每个 target 是不是 UP（这一步才算真的验证了抓取链路）。
* `http://127.0.0.1:9090/graph` —— 手敲一条 PromQL，例如 `infergate_build_info`。
* 单条查询走 HTTP API：`curl.exe -s "http://127.0.0.1:9090/api/v1/query?query=up"`。

### 3.2 Docker

```powershell
# 注意 -v 的两边：本机路径用 %CD%（或绝对路径），容器内路径固定是 /etc/prometheus/prometheus.yml
docker run -d --name infergate-prometheus -p 9090:9090 `
  -v "%CD%\deploy\prometheus\prometheus.yml:/etc/prometheus/prometheus.yml:ro" `
  --add-host=host.docker.internal:host-gateway `
  prom/prometheus:v2.53.0 `
  --config.file=/etc/prometheus/prometheus.yml `
  --web.enable-lifecycle
```

**用容器抓宿主机上的网关时，`127.0.0.1` 指的是容器自己**，抓不到宿主机上的 18999 —— 得把目标换成 `host.docker.internal:18999`。
两种做法：

```powershell
# 做法 A：改一份副本，把 127.0.0.1 全换成 host.docker.internal（最简单，推荐）
#   （改的是你本机的一份 copy，不要改仓库里这份，免得本机直跑时又反了）

# 做法 B：让 Prometheus 自己做环境变量替换。把 yaml 里的地址写成 ${GATEWAY_HOST}:18999，
#         然后加 --config.expand-env=true。注意：开启替换后，配置里原本的 $ 必须写成 $$。
docker run -d --name infergate-prometheus -p 9090:9090 `
  -e GATEWAY_HOST=host.docker.internal `
  -v "%CD%\deploy\prometheus\prometheus.yml:/etc/prometheus/prometheus.yml:ro" `
  --add-host=host.docker.internal:host-gateway `
  prom/prometheus:v2.53.0 `
  --config.file=/etc/prometheus/prometheus.yml --config.expand-env=true
```

`--add-host=host.docker.internal:host-gateway` 是 Linux 上跑 Docker 时需要的手工映射；Docker Desktop（Windows/macOS）
自带这个名字，加不加都能用。Docker Desktop 上**不能用** `--network host`（那是 Linux 才有的语义），所以别指望靠 host 网络绕过这件事。

另外，容器里改配置后不用重启：`--web.enable-lifecycle` 打开后
`curl.exe -X POST http://127.0.0.1:9090/-/reload` 就能重载（本机直跑时同样加这个 flag 才有效）。

## 4. Grafana：怎么跑（外部二进制，本仓库不含）

### 4.1 Windows 本机

Grafana 本机版是「homepath + 若干 cfg: 覆盖项」的启动方式。provisioning 目录必须**指到本仓库这一份**，
否则 Grafana 只会去读它自带的空目录，什么都不会出现：

```powershell
# 先改一行配置：deploy\grafana\provisioning\dashboards\infergate.yml 里的 options.path
#   /etc/grafana/provisioning/dashboards            ← 容器里才成立
#   C:\Users\20106\Desktop\infergate\deploy\grafana\dashboards   ← Windows 本机改成这个（文件里已有一行注释好的样例）

cd C:\tools\grafana-11.1.0
.\bin\grafana-server.exe `
  --homepath="C:\tools\grafana-11.1.0" `
  --config="C:\tools\grafana-11.1.0\conf\defaults.ini" `
  cfg:paths.provisioning="C:\Users\20106\Desktop\infergate\deploy\grafana\provisioning" `
  cfg:paths.data="C:\Users\20106\Desktop\infergate\tmp\grafana-data" `
  cfg:paths.logs="C:\Users\20106\Desktop\infergate\tmp\grafana-logs"
```

> 忘了改 `options.path` 的话，Grafana 会安静地什么都不加载（日志里只有一行 provisioning dashboards 的提示），
> 看板不会出现在列表里——这是本配置最容易踩的一脚。

### 4.2 Docker

容器里路径就是配置里写的 `/etc/grafana/provisioning`，所以**要挂两个卷**：一个挂整个 provisioning 目录，
一个把看板目录挂到 `/etc/grafana/provisioning/dashboards`（就是 YAML 里 `options.path` 指的那个位置）：

```powershell
docker run -d --name infergate-grafana -p 3000:3000 `
  -v "%CD%\deploy\grafana\provisioning:/etc/grafana/provisioning:ro" `
  -v "%CD%\deploy\grafana\dashboards:/etc/grafana/provisioning/dashboards:ro" `
  -e GF_AUTH_ANONYMOUS_ENABLED=true `
  -e GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer `
  grafana/grafana-oss:11.1.0
```

（不要设 `GF_AUTH_ANONYMOUS_ORG_ROLE=Admin` 除非你只是本地随便看；默认管理员仍是 `admin`/`admin`，首次登录会让你改密码。）

### 4.3 数据源与看板为什么会自己出现

* 数据源：`provisioning/datasources/prometheus.yml` 注册了一个 `uid: PROMETHEUS` 的 Prometheus 数据源，
  `isDefault: true`、`editable: false`、地址 `http://127.0.0.1:9090`。**uid 固定**是为了让看板能长期引用它。
  ⚠️ Grafana 跑在容器里、Prometheus 跑在宿主机时，这个 `http://127.0.0.1:9090` 同样要改成 `http://host.docker.internal:9090`。
* 看板：`provisioning/dashboards/infergate.yml` 是一个 file provider，`options.path` 下的每个 `.json` 都会被注册；
  `foldersFromFilesStructure: false` 表示不看目录结构、统一放进 `folder: InferGate` 这个文件夹。
  看板里用的是 `DS_PROMETHEUS` 这个 templating 变量（`type: datasource`），所以即使 uid 变了也能在 UI 里重选数据源。
* 改完 JSON/YAML 后，provider 每 `updateIntervalSeconds: 30` 秒扫一次，通常半分钟内自动生效，不用重启 Grafana。

## 5. 不装 Prometheus/Grafana 也能手工看的三个端点

网关自己暴露的面（端口取自它的配置；M5 测量用 18999）：

```powershell
# 1) 指标原文：手写 Prometheus 文本，人也能读。只看 infergate_ 开头的行
curl.exe -s http://127.0.0.1:18999/metrics | Select-String "infergate_"

# 2) 网关自己的 JSON 视图（计数器/直方图/熔断/缓存/配额的结构化快照）
curl.exe -s http://127.0.0.1:18999/stats

# 3) 就绪探针：能不能对外服务（具体判定规则见 internal/ 里的实现与根 README 3.3）
curl.exe -s -o NUL -w "%{http_code}`n" http://127.0.0.1:18999/readyz
```

> PS 5.1 里 `curl` 是 `Invoke-WebRequest` 的别名，参数完全不同，所以上面一律写 `curl.exe`。
> 顺手可用的还有 `/healthz` 以及 `/admin/{upstreams,breakers,cache,quota...}` 这些排障接口。

看板用到的指标家族（M5 契约）分四类：请求/延迟（`infergate_requests_total`、`infergate_request_duration_seconds_*`、
`infergate_upstream_attempt_duration_seconds_*`、`infergate_first_token_seconds_*`、`infergate_completion_tokens_per_request_*`、
`infergate_tokens_total`、`infergate_stream_{frames,bytes}_total`、`infergate_failovers_total`）、
熔断（`infergate_breaker_{state,trips_total,rejections_total,failure_ratio}`）、
缓存与配额（`infergate_cache_*`、`infergate_quota_*`）、运行时（`infergate_runtime_*`、`infergate_build_info`）。
**不要在 PromQL 里碰这份清单以外的名字**——尤其注意 `infergate_request_duration_seconds` 是直方图（有 `_bucket`/`_sum`/`_count`），
它**替换掉了**更早的裸 `_sum` counter，别再按普通 counter 那么写。

## 6. measured vs not measured（这一轮到底验了什么）

**已验证（可以在仓库里复现）**

* `deploy/grafana/dashboards/infergate.json` 能被解析：PowerShell 5.1 的 `Get-Content -Raw ... | ConvertFrom-Json` 与
  Node 的 `JSON.parse` 都通过；`uid=infergate`、`schemaVersion=39`、`refresh=5s`、`time: now-15m → now`、33 个 panel 都在。
* 三份 YAML 与看板 JSON 里的**每一个** `infergate_*` 指标名，都用脚本对着 M5 契约清单逐个比对过，没有清单外的名字。
* YAML 是**按检查单看过的**：2 空格缩进、无 Tab、无重复键（本机没有 YAML 解析器，见下条）。

**未验证（本机做不到，别把这份配置当成"跑通了的观测栈"）**

* 本机**没有** Prometheus 也没有 Grafana 二进制，网络也不通，所以：
  * 没跑过 `promtool check config`，没起过 scrape，没查过 `up{}`；
  * 没做过一次真实查询，看板上任何一个 panel 的图形**没有被渲染过**；
  * Grafana 的 provisioning 没有被真正加载过，数据源健康检查也没跑过。
* 也就是说：**配置正确性只到"能解析 + 名字对得上契约 + 逐行人工检查"为止**。想真正确认，请按第 3、4 节起来之后自查：
  1. `promtool check config deploy\prometheus\prometheus.yml`（语法）；
  2. `http://127.0.0.1:9090/targets` 里两个 job 都是 UP（抓取链路）；
  3. `curl.exe -s "http://127.0.0.1:9090/api/v1/query?query=infergate_build_info"` 有序列（标签正确）；
  4. `http://127.0.0.1:3000/api/datasources` 能看到 uid=`PROMETHEUS`，`http://127.0.0.1:3000/api/dashboards/uid/infergate` 能取到看板；
  5. 打开看板，逐个 panel 确认有数据——**空 panel 与真 0 是两件事**，延迟类 panel 尤其如此。

## 7. 一条命令起整套（`docker-compose.yml`）

第 3、4 节是"自己准备两份二进制、两个终端、注意 datasource 的 url"的路线。如果你只是想把面板看亮，
仓库根目录的 `docker-compose.yml` 把四件事都接好了：网关、mock 上游、仓库内的 RESP2 服务器（`cmd/miniredis`）、
以及（可选）Prometheus + Grafana。

```
docker compose up -d --build                 # 网关 + mock 上游 + RESP2（三容器，几秒）
docker compose --profile obs up -d --build   # 再加 Prometheus + Grafana
docker compose down                          # 收工（无 named volume，状态随容器一起没）
```

起来之后：

| 地址 | 是什么 |
| --- | --- |
| `http://127.0.0.1:18080/healthz` `/readyz` `/metrics` | 网关自身（`/readyz` 是强探针：路由表 + 存储都在才算 ready） |
| `http://127.0.0.1:18080/admin/upstreams` 等 `/admin/*` | 管理面（**无认证**，见根 README §9 的安全边界） |
| `http://127.0.0.1:19000/v1/chat/completions` | 直连 mock 上游，用来回答"瓶颈在网关还是在后端" |
| `http://127.0.0.1:19090/targets` | Prometheus 抓取状态（`--profile obs`） |
| `http://127.0.0.1:13000` | Grafana，匿名 Viewer，看板 `InferGate` 已自动加载（`--profile obs`） |

**端口故意都不是默认值**（18080/19000/16399/19090/13000）：这台机器平时是"本机进程"形态在跑，
验收脚本也用 18080/18999（根 README §3.2）。想改就用环境变量，例如 `INFERGATE_GATEWAY_PORT=8080 docker compose up -d`。

两个形态的差别只有地址，配置是分开的两份：

* `configs/docker.yaml` —— 容器画像：`base_url` 写 `http://mockupstream:9000`，cache 与 quota 的 `redis.addr` 都写 `miniredis:6399`。
  在容器里写 `127.0.0.1` 会指到网关自己，于是"缓存永远 miss、配额永远连不上"。
* `configs/mock.yaml` / `cache-redis.yaml` / `quota-redis.yaml` —— 本机画像，保持 loopback 不变。

镜像只有一个 `Dockerfile`，用 `--build-arg CMD=` 选择编译哪个 `cmd/`（网关 / mock 上游 / miniredis 三个都行），
compose 的三个服务就是这么来的。构建细节写在 `Dockerfile` 顶部注释里。

### 7.0 三个健康检查，以及为什么镜像里装了 netcat

`depends_on` 用的是 `condition: service_healthy`，不是 `service_started`，这是一个真实的启动竞争：

* 配额默认 **fail-closed**。网关读不到自己的存储时会**拒绝启动**，不是一个能慢慢重试的软失败。
  容器"存在"不等于"能连"——miniredis 起来到监听 6399 之间有一个窗口，网关在这个窗口里启动就是起不来。
* 所以三个服务各有一个探针，且都是"真的用那个协议敲一下"：

| 服务 | 探针 | 为什么是它 |
| --- | --- | --- |
| `gateway` | `wget -qO- http://127.0.0.1:8080/readyz` | 强探针：路由表与两个 store 都就绪才算 healthy，不只是端口在听 |
| `mockupstream` | `wget -qO- http://127.0.0.1:9000/v1/models` | mock 的 HTTP 面，与网关同一个套路 |
| `miniredis` | `nc -z 127.0.0.1 6399` | **RESP2 协议不能用 wget 探**；打开 socket 再关掉是唯一简单的"在听"证据 |

`netcat-openbsd` 就是为最后一行装的（`Dockerfile` 的 runtime 阶段）。替代方案是再写一个 `cmd/healthcheck`
之类的探针程序，但那是"为了一个布尔值多一个命令、多一份文档、多一个 `configs/` 条目"，
而 alpine 镜像里装 netcat 属于常规操作。**这一条没有被执行验证过**（守护进程没运行），
判据很简单：`docker compose up -d` 之后 `docker compose ps` 里三个服务都应该是 `healthy`；
只要 miniredis 卡在 `starting`，就说明 `nc -z` 在这个镜像里不可用。

### 7.1 这一节里被验证过、与没被验证过的

口径与第 6 节一致，不把"写好了"说成"跑通了"。

**已验证**

* `docker compose -f docker-compose.yml config --quiet` 通过（YAML 与 compose 语法，不需要 daemon）。
* `configs/docker.yaml` 用真实加载器校验通过：`go run ./cmd/infergate -config configs/docker.yaml -check` → `configuration OK`。
* **`scripts/verify-docker-profile.ps1` 20 项全过**（本机 25 秒）：把 `configs/docker.yaml` 里的服务名换成本地地址，
  用仓库自己的三个二进制真起一遍（网关 + mock 上游 + miniredis），证明这些键不只是"能被解析"，而是**真的生效**：
  `capacity`（幂等 512 / 会话 512）、`max_response_bytes`、`recent_per_session`、`tracing.jsonl_path: ""`，
  以及缓存/幂等/会话/配额/追踪五个面在 RESP2 存储上的往返。这道门量的是**值有没有起作用**；
  至于**键名有没有写对**，现在是加载器的性质：严格解码（`miniyaml.UnmarshalStrict` →
  `json.Decoder.DisallowUnknownFields`）会在加载期点名拒绝未知键（把 `capacity` 写成 `max_entries`
  → `unknown field "max_entries"`，退出码 1）。`configs/docker.yaml` 里 `idempotency:` 那段注释曾经是这里唯一的防线，
  因为它当时只能提醒读者去真跑一次。
  这道门在 2026-10-06 加了一层前置检查（端口空闲、`-check` 通过、两个 store 可达）后才敢下结论：
  在此之前它曾在 **miniredis 根本没起来**的运行里报出 `first request is a miss got 'skip'`，
  把"环境没准备好"显示成"缓存错了"。判读这类失败前先看门开头的三条前置断言。

**未验证**

* **镜像本身没有被构建过**，compose 也没有被起来过：写这份文档的机器上 Docker Desktop 装着但**守护进程没运行**
  （`failed to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine`）。
* 所以 compose 里的东西——`golang:1.26-alpine` / `alpine:3.20` / `prom/prometheus:v2.53.0` / `grafana/grafana-oss:11.1.0`
  这四个 tag 是否都存在、多阶段构建是否通过、非 root 用户能否写 OTLP 之类——**一概没验过**。
  `GO_IMAGE` 因此做成 build arg，tag 换了不用改文件。
* `docker.yml` 与 `datasources-docker/prometheus.yml` 里的服务名解析（`gateway:8080`、`prometheus:9090`）没验过，
  这是 Docker 内嵌 DNS 的标准行为，不是本仓库的代码。
* 结论：**容器形态的正确性到"配置解析通过 + 同一份配置在本地真实进程上跑通 + compose 语法合法"为止**。
  第一次 `docker compose up -d --build` 仍然可能撞上镜像 tag 或构建问题。

## 8. 为什么有一批 `\uXXXX`

（见第 1 节末段。）看板 JSON 里所有中文都写成了 `\uXXXX` 转义，是刻意的：PowerShell 5.1 的
`Get-Content` 对无 BOM 文件按本地代码页解码，直接写中文会在"读-改-写"一次之后变成乱码。
同样原因，`scripts/verify-docker-profile.ps1` 通篇只用 ASCII、且所有写文件都走 `[System.IO.File]::WriteAllText`
（`Set-Content -Encoding utf8` 会写 BOM，网关会把带 BOM 的请求体判成"不是 JSON"）。

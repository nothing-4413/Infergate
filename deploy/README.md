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
│  └─ prometheus.yml                       ← scrape 配置：两个 job（single / fleet）
└─ grafana/
   ├─ provisioning/
   │  ├─ datasources/prometheus.yml        ← 自动注册数据源（uid=PROMETHEUS）
   │  └─ dashboards/infergate.yml          ← 自动注册看板目录（provider）
   └─ dashboards/
      └─ infergate.json                    ← 看板本体（uid=infergate，33 个 panel）
```

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

# InferGate 使用手册

README 只保留最短路径；本文是它原本的完整版：每个里程碑的启动方式、可复制的 curl 命令、
故障注入手法，以及各自的“验收与实测”段落。章节号沿用原来的 `3.x`，这样代码注释、
其它文档里“见 3.x”的指认依然有效。

文档分工：[../README.md](../README.md) 是入口，本文是**怎么跑**，
[ACCEPTANCE.md](ACCEPTANCE.md) 是**每一条能力的验收口径**，
[DESIGN.md](DESIGN.md) 是**为什么这么设计**，[RESUME.md](RESUME.md) 是量化结论，
`baseline/` 是原始数据。

---

## 3. 快速开始（完整版）

### 3.1 编译与测试

```powershell
# Windows PowerShell（本机已验证；cmd 下把 .\tools\go.cmd 换成 tools\go.cmd 亦可）
.\tools\go.cmd build ./...
.\tools\go.cmd vet ./...
.\tools\go.cmd test ./...

# 生成两个可执行文件
.\tools\go.cmd build -o .\bin\infergate.exe    .\cmd\infergate
.\tools\go.cmd build -o .\bin\mockupstream.exe .\cmd\mockupstream
```

### 3.2 起一个可用的本地栈

```powershell
# 终端 1：假上游（9000 端口，首帧前故意停顿 250ms，用来验证首字延迟）
.\bin\mockupstream.exe -listen :9000 -ttfb 250ms

# 终端 2：网关（8080 端口，配置见 configs/mock.yaml）
.\bin\infergate.exe -config .\configs\mock.yaml
```

mock 的常用开关：`-token-delay 0` 关掉每 token 的 15ms 间隔（**压测时必须关**，否则测到的是 mock
的速度而不是网关的），`-ttfb` 控制首帧前的停顿。另外 mock 认这些请求头做故障注入：
`X-Mock-Status`、`X-Mock-Delay`、`X-Mock-TTFB`、`X-Mock-Omit-Done`，并在响应上回写 `X-Mock-Upstream`。

> **端口提示**：本机 `127.0.0.1:8080` 已被其它进程占用时，网关只能绑到 `[::]:8080`；
> Windows 会优先把 `127.0.0.1` 的请求交给那个更具体的绑定，于是 `/healthz` 会莫名其妙返回别人的 404。
> 验收脚本默认用 `18080` 就是为了绕开这一点；手动实验时请用 `localhost` 而不是 `127.0.0.1`。

### 3.3 curl 验证（对应"可 curl 验证"的交付要求）

```powershell
$base = 'http://127.0.0.1:18080'

# 1) 健康与就绪、路由表（不会泄露 api_key，只报 has_api_key）
curl.exe -s "$base/healthz"
curl.exe -s "$base/readyz"
curl.exe -s "$base/admin/upstreams"

# 2) 非流式透传。
#    注意：Windows PowerShell 5.1 会把传给原生 exe 的参数里的引号吃掉，
#    所以请求体一律写文件、用 --data-binary @file 发送（脚本里也是这么做的）。
'{"model":"mock-gpt","messages":[{"role":"user","content":"hello"}]}' |
  Set-Content -NoNewline -Encoding ascii .\tmp\chat.json
curl.exe -s -i -X POST "$base/v1/chat/completions" `
  -H 'Content-Type: application/json' `
  --data-binary "@.\tmp\chat.json"

# 3) SSE 流式：-N 关闭 curl 缓冲，否则看不到"逐帧到达"
'{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}' |
  Set-Content -NoNewline -Encoding ascii .\tmp\stream.json
curl.exe -N -s -X POST "$base/v1/chat/completions" `
  -H 'Content-Type: application/json' `
  --data-binary "@.\tmp\stream.json"

# 4) 首帧到达时间：注入 250ms 停顿，首帧应 >= 200ms，说明中间没有缓冲
curl.exe -N -s -o NUL -w "ttfb=%{time_starttransfer}s total=%{time_total}s`n" `
  -X POST "$base/v1/chat/completions" -H 'Content-Type: application/json' `
  --data-binary "@.\tmp\stream.json"

# 5) 故障注入（mock 的控制头）
curl.exe -s -i -X POST "$base/v1/chat/completions" `
  -H 'Content-Type: application/json' -H 'X-Mock-Status: 429' `
  --data-binary "@.\tmp\chat.json"
curl.exe -N -s -X POST "$base/v1/chat/completions" `
  -H 'Content-Type: application/json' -H 'X-Mock-Omit-Done: 1' `
  --data-binary "@.\tmp\stream.json"     # 网关应补上 data: [DONE]

# 6) 计量与可观测
curl.exe -s "$base/stats"     # JSON：请求序列、p50/p90/p95/p99、token、首字均值
curl.exe -s "$base/metrics"   # Prometheus 文本
```

### 3.4 一键验收（推荐）

```powershell
# 每个里程碑两条路径：Go 端到端 + 真实进程 curl
.\tools\go.cmd run .\cmd\verify                                        # M0，38 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m0.ps1   # M0 curl，47 条
.\tools\go.cmd run .\cmd\verify-m1                                     # M1，64 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m1.ps1   # M1 curl，56 条
.\tools\go.cmd run .\cmd\verify-m2                                     # M2，103 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m2.ps1   # M2 curl，157 条（内存 + Redis）
.\tools\go.cmd run .\cmd\verify-m3                                     # M3，470 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m3.ps1   # M3 curl，323 条（内存 + 真 Redis 协议）
.\tools\go.cmd run .\cmd\verify-m4                                     # M4，877 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m4.ps1   # M4 curl，125 条（两层假上游，不需要 GPU）
.\tools\go.cmd run .\cmd\verify-m5                                     # M5，420 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m5.ps1   # M5 curl，211 条
.\tools\go.cmd run .\cmd\verify-m6                                     # M6，381 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m6.ps1   # M6 curl，149 条（幂等 / 账本 / 能力发现）
```

`scripts/verify-m0.ps1` 会自行编译两个二进制、拉起两个真实进程、跑完 47 条断言，并在 `finally` 中
必定回收进程；参数为 `-GatewayPort 18080 -MockPort 19000 -TtfbMillis 250`。
注意本机没有 `pwsh`，必须用 `powershell -NoProfile -ExecutionPolicy Bypass`（脚本未签名）。

### 3.5 压测（M0 基线）

```powershell
# 全场景基线：直连 vs 经网关 × 非流式/流式 × 并发 8/32，取 3 轮中位数
.\tools\go.cmd run .\cmd\loadtest -all -c 8,32 -n 1500 -warmup 300 -rounds 3 -out docs\baseline\m0-baseline.json

# 逐段定位开销：直连 / 朴素反代(pool=2) / 调优反代(pool=256) / 最小透传 / InferGate
.\tools\go.cmd run .\cmd\loadtest -diag -c 8,32 -n 1500 -warmup 300 -rounds 3

# 也可以只压已有部署
.\tools\go.cmd run .\cmd\loadtest -url http://127.0.0.1:18080 -c 32 -n 3000
```

`cmd/loadtest` 会自己起一个真实 mock 上游子进程（`-token-delay 0`，即不注入每 token 延迟，
否则测到的是 mock 的速度），并同时报告**客户端观测延迟**和**网关自报的 /metrics 处理耗时**。
本机实测结论（原始数据见 `docs/baseline/m0-baseline.json`，明细与解读见 `docs/RESUME.md` 的 M0 一节）：

| 场景 | 并发 | QPS（中位数[轮间区间]） | P95 | TTFT P95 |
| --- | --- | --- | --- | --- |
| 直连上游（非流式） | 8 | 8393 [6678..10531] | 2.00ms | - |
| 经网关（非流式） | 8 | 5026 [2837..7359] | 3.50ms | - |
| 直连上游（流式） | 8 | 1728 [1655..2297] | 7.00ms | 2.00ms |
| 经网关（流式） | 8 | 1088 [820..1144] | 11.09ms | 2.74ms |
| 经网关（非流式） | 32 | 5497 [5140..7477] | 11.00ms | - |
| 经网关（流式） | 32 | 1324 [1096..2125] | 41.55ms | 10.81ms |

> 读数警告：这是**本机回环**压测（同一台笔记本，机器上还有别的负载），直连基线自身就有 40%+
> 的轮间抖动，所以只有毫秒级以上的差异才可信；`loadtest` 在抖动超过 5% 时会主动打印告警。
> `/metrics` 显示网关自身处理耗时只有 1–5us/请求，远小于客户端观测的毫秒级延迟——
> 也就是说这台机器上**先成为瓶颈的是压测客户端**，引用数字时不要把客户端等待记成网关开销。

### 3.6 配置说明（`configs/infergate.yaml`）

```yaml
server:
  listen: ":8080"
  read_header_timeout: "10s"     # 时长必须写成带引号的字符串
  idle_timeout: "90s"
  upstream_timeout: "10m"        # 单次上游交换上限（含流式体）；0 表示不设限
  shutdown_timeout: "15s"
  max_body_bytes: 8388608        # 8MiB，超出直接 413，不读进内存
  max_idle_conns_per_host: 64    # 最关键的吞吐旋钮

upstreams:
  - name: "openai"
    kind: "openai"
    base_url: "https://api.openai.com"      # 入站 /v1/... 会原样拼接在后
    api_key: "${OPENAI_API_KEY}"            # ${VAR} 未定义会直接启动失败，而不是静默为空
    models: ["gpt-4o", "gpt-4o-mini"]
  - name: "deepseek"
    base_url: "https://api.deepseek.com"
    api_key: "${DEEPSEEK_API_KEY}"
    models: ["deepseek-chat"]
  - name: "local"
    base_url: "http://127.0.0.1:8000"       # vLLM / Ollama
    models: ["/"]                           # "/" = 兜底：任何未匹配模型都走这里

pricing:                                     # 单位：USD / 1M tokens
  default: { in: 0.15, out: 0.60 }
  models:
    "gpt-4o": { in: 2.50, out: 10.00 }

access:                                      # 运营者令牌：默认关闭，见下方
  enabled: true
  tokens: ["${INFERGATE_ADMIN_TOKEN}"]
  protect: []                                # 空 = ["/admin", "/stats"]
```

环境变量覆盖：`INFERGATE_LISTEN`、`INFERGATE_LOG_LEVEL`、`INFERGATE_MAX_BODY_BYTES`。
请求级控制头：`X-InferGate-Upstream: <name>` 强制指定上游（用于灰度与排障）。
路由顺序：显式头 → 模型精确匹配（大小写不敏感）→ 兜底 `"/"` → 只剩一个后端时吸收任意模型名。

**`access` 这一节**：`/admin/*`（刷配额、清缓存、使重放失效、读每个租户的花费）和 `/stats`
默认**不鉴权**——这是 M0–M6 的既有行为，本节所有 curl 示例也都以无凭证访问为前提。
绑到可路由地址时打开它：

```powershell
$env:INFERGATE_ADMIN_TOKEN = "pick-something-long"
.\bin\infergate.exe -config .\configs\agent.yaml
# 无令牌 -> 401；令牌对 -> 和以前一样
curl.exe -s -o NUL -w "%{http_code}`n" http://127.0.0.1:8080/admin/upstreams
curl.exe -s -o NUL -w "%{http_code}`n" -H "Authorization: Bearer $env:INFERGATE_ADMIN_TOKEN" http://127.0.0.1:8080/admin/upstreams
```

`protect: []` 表示用默认集 `["/admin", "/stats"]`；给出显式列表是**替换**默认集，不是追加。
`/healthz`、`/readyz`、`/metrics` 不在默认集里（探针与抓取器不带凭证），要关就显式列上。
少带令牌和带错令牌都是 401、响应体相同；`enabled: true` 却没有令牌、或 `protect: ["/"]` 会在
加载期报错。完整取舍见 [README.md](../README.md) 的 8.1。

### 3.7 多 Provider 路由与故障转移（M1）

```powershell
# 终端 1~3：三个假上游；-name 决定 /healthz 与 X-InferGate-Upstream-Name 里显示谁答的
.\bin\mockupstream.exe -listen :19100 -name primary
.\bin\mockupstream.exe -listen :19101 -name secondary
.\bin\mockupstream.exe -listen :19102 -name tools     # 唯一声明了 tools 能力的后端

# 终端 4：网关（configs/routing-local.yaml：strategy=priority，primary > secondary > tools）
.\tools\go.cmd run .\cmd\infergate -config .\configs\routing-local.yaml
```

三个副本故意声明同一个模型 `["/"]`：这样"选谁"只由优先级与健康度决定，故障转移在响应头里
一眼可见（换个上游就是换个 `X-InferGate-Upstream-Name`），不会被模型名改写遮住。

```powershell
$base = 'http://127.0.0.1:8080'

# 路由表（模型索引 / 策略 / 权重 / 能力 / 当前熔断状态）与每上游健康窗口
curl.exe -s "$base/admin/upstreams"
curl.exe -s "$base/admin/breakers"

'{"model":"mock-gpt","messages":[{"role":"user","content":"hi"}]}' |
  Set-Content -NoNewline -Encoding ascii .\tmp\chat.json

# 1) 普通请求：打到 priority=1 的 primary
curl.exe -s -i -X POST "$base/v1/chat/completions" `
  -H 'Content-Type: application/json' --data-binary "@.\tmp\chat.json" |
  Select-String 'X-InferGate-'

# 2) 能力路由：只送给声明了该能力的后端；一个都匹配不上时是 400，不静默降级
curl.exe -s -i -X POST "$base/v1/chat/completions" `
  -H 'Content-Type: application/json' -H 'X-InferGate-Capabilities: tools' `
  --data-binary "@.\tmp\chat.json" | Select-String 'X-InferGate-'

# 3) 显式指定后端（灰度 / 排障），跳过一切打分
curl.exe -s -i -X POST "$base/v1/chat/completions" `
  -H 'Content-Type: application/json' -H 'X-InferGate-Upstream: tools' `
  --data-binary "@.\tmp\chat.json" | Select-String 'X-InferGate-'
```

响应头就是这套机制的自证（`internal/gateway/headers.go`）：

| 响应头 | 含义 |
| --- | --- |
| `X-InferGate-Upstream-Name` | 最终服务这次请求的后端 |
| `X-InferGate-Attempt` | 这是第几次尝试（`1` 表示第一次就成功） |
| `X-InferGate-Tried` | **只有发生过重试才出现**：逗号分隔的尝试顺序，如 `primary, secondary` |
| `X-InferGate-Request-Id` | 请求关联 id，客户端给了就沿用，没给就生成 |

「为什么选了它」不进响应头，而是写进网关的结构化日志：每条 `msg=request` 都带 `reason=`，
内容来自 `internal/router` 的 `reasonFor`（如 `priority (priority 1)`、`cost`，
熔断中的后端则是 `priority: circuit open, only used as a last resort`）。响应头只回答
"换没换、换了谁"，日志回答"凭什么是它"。

故意杀一个后端，就能看到"熔断把一个死后端从第一次尝试里摘掉"：

```powershell
# 杀掉 primary（关掉那个终端即可），再发请求：客户端仍然是 200，只是换了后端
curl.exe -s -i -X POST "$base/v1/chat/completions" `
  -H 'Content-Type: application/json' --data-binary "@.\tmp\chat.json" |
  Select-String 'X-InferGate-'                     # Tried: primary, secondary

# 连续发，等窗口内失败率越过 failure_ratio：primary 被排到最后，第一次尝试不再碰它，
# 于是 Tried 头直接消失（只剩一次尝试），/admin/breakers 里 primary 变成 open
curl.exe -s "$base/admin/breakers"

# 修好后不必等冷却：手动重置熔断器（全部，或只重置一个上游）
curl.exe -s -X POST "$base/admin/breakers/reset"
curl.exe -s -X POST "$base/admin/breakers/reset?upstream=primary"
```

配置里与 M1 相关的三个块（完整注释与"为什么这么设"见 `configs/routing.yaml`）：

```yaml
routing:
  strategy: "priority"          # priority | cost | latency | weighted | score
  weights:                      # 只有 score 策略读；是比例，不要求和为 1
    cost: 3
    latency: 1
    reliability: 2
    priority: 1
  default_capabilities: ["chat"]  # 没声明能力的后端按此补齐，否则会被能力请求排除
  fallback_model: ""              # 空 = 绝不替换模型：可预测优先

health:                         # 滑动窗口熔断（不是计数器：一小时前的失败不该压着现在）
  window: "30s"
  buckets: 6
  min_requests: 5               # 样本不足不熔断：未知 ≠ 坏了
  failure_ratio: 0.5            # 窗口内失败率阈值（超时也算失败）
  open_duration: "5s"           # open 持续多久后放半开探针
  half_open_probes: 2           # 连续几个探针成功才回到 closed
  max_failures_per_request: 2   # 单次请求允许失败几次（= 重试预算的上界）
  retry_backoff: "50ms"         # 线性增长 + 全抖动，避免所有客户端同拍重试

pricing:                        # USD / 1M tokens；cost 策略与日志里的 cost_usd 都用它
  default: { in: 1.0, out: 3.0 }   # 未标价的模型也有成本数字，而不是看起来免费
  models:
    "gpt-4o": { in: 2.5, out: 10.0 }
```

两条验收路径（都是真实进程 + 真实 curl）：

```powershell
.\tools\go.cmd run .\cmd\verify-m1                                            # 64/64 断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m1.ps1    # 56/56 断言
```

`scripts/verify-m1.ps1` 自己编译二进制、拉起三个副本和一个网关，依次验证：优先级路由 → 杀掉一个
后端后的非流式与流式故障转移 → 熔断把死后端摘出轮转 → 能力路由 → 显式指定 → 四条可观测面 →
恢复（重启副本 + 重置熔断器），并在 `finally` 中回收全部进程；参数为
`-GatewayPort 18180 -PrimaryPort 19100 -SecondaryPort 19101 -ToolsPort 19102`。

### 3.8 M1 实测（路由开销 / 故障吸收 / 熔断收益）

```powershell
# 约 3 分钟：单上游网关 vs 三副本路由网关（交错 3 轮），再中途杀掉一个副本压同样的负载，
# 最后用"停顿 1500ms 的后端 + 400ms 单次超时预算"逐请求量出熔断省下的延迟
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m1.ps1
```

原始数据：`docs/baseline/m1-summary.json` 与 `docs/baseline/m1-load-{single,router,faulted}-r{1,2,3}.json`
（每阶段 1500 请求 + 300 预热，3 轮取中位数，本机回环）。

| 场景 | 负载 | 并发 | QPS（中位数[轮间区间]） | P95 | P99 | 错误 |
| --- | --- | --- | --- | --- | --- | --- |
| M0 单上游 | 非流式 | 8 | 1772 [1228..1975] | 6.35ms | 12.49ms | 0 |
| M0 单上游 | 非流式 | 32 | 1825 [1402..1948] | 22.08ms | 31.00ms | 0 |
| M0 单上游 | 流式 | 8 | 977 [892..1266] | 12.50ms | 15.01ms | 0 |
| M0 单上游 | 流式 | 32 | 1425 [1050..1434] | 27.53ms | 31.75ms | 0 |
| M1 路由（三副本健康） | 非流式 | 8 | 1749 [1287..1777] | 7.08ms | 8.99ms | 0 |
| M1 路由（三副本健康） | 非流式 | 32 | 1701 [1558..1724] | 22.25ms | 27.92ms | 0 |
| M1 路由（三副本健康） | 流式 | 8 | 935 [871..940] | 13.00ms | 15.58ms | 0 |
| M1 路由（三副本健康） | 流式 | 32 | 1334 [1152..1647] | 31.62ms | 35.62ms | 0 |
| M1 路由（priority=1 副本被杀） | 非流式 | 8 | 431 [411..435] | 23.10ms | 24.71ms | 0 |
| M1 路由（priority=1 副本被杀） | 非流式 | 32 | 1652 [1317..1669] | 24.58ms | 29.79ms | 0 |
| M1 路由（priority=1 副本被杀） | 流式 | 8 | 803 [784..1146] | 14.01ms | 16.77ms | 0 |
| M1 路由（priority=1 副本被杀） | 流式 | 32 | 1033 [629..1691] | 40.34ms | 43.74ms | 0 |

怎么读这张表：

- **路由本身不加延迟**：M1（三副本健康）与 M0（单上游）的 QPS/P95 差异全部落在 40%+ 的轮间抖动以内
  （非流式 c=8：1749 vs 1772 QPS，P95 7.08ms vs 6.35ms）。候选筛选 + 打分的成本在这台机器上量不出来。
- **故障对客户端是透明的**：每轮压测中途 `Stop-Process` 杀掉 priority=1 的副本，该场景 18000 个请求
  **0 错误**（全部 200，改由 secondary 接管）；整个实测 54000 个请求同样 0 错误。
- **代价集中在"熔断窗口还没关掉死副本"的那一段**：此时每个请求都先白跑一次死上游，非流式 c=8
  掉到 431 QPS、P50 19.5ms；等窗口内失败率越过阈值、primary 被排到最后，同一轮的后半段就回到
  健康水平（非流式 c=32：1652 vs 健康 1701 QPS）。这是 `failure_ratio` / `min_requests` 的取舍，
  不是路由失效。
- **熔断省下的延迟是逐请求量出来的**（停顿 1500ms 的后端 + 400ms 单次超时预算，共 10 个请求）：
  第 1~4 个请求各付约 418–423ms（第一次尝试超时 → 换到快后端，`tried='stalled, fast'`）；
  第 5 个请求起熔断已 open（attempts=4 failures=4 timeouts=4 failure_ratio=1.0），
  **中位延迟 420.42ms → 17.81ms（23.6×）**，且 `attempts=1`、不再出现 `X-InferGate-Tried`。
- 与 M0 相同的读数警告：本机回环、机器上还有别的负载，只有毫秒级以上的差异才可信；
  `/metrics` 显示这台机器上先成为瓶颈的是压测客户端本身。

---

### 3.9 语义缓存（M2）

缓存插在**解析之后、路由之前**：命中时不需要知道有哪些后端，也不会碰熔断器的窗口。
默认**关闭**（打开缓存会改变调用方能观察到的事实：本该被调用的上游没被调用，答案可能是
几分钟前的），要显式打开。

```powershell
# 起一个小栈：一个 mock 上游 + 一个网关（配置见 configs\cache-local.yaml）
.\tools\go.cmd run .\cmd\mockupstream -listen :9200 -name local
.\tools\go.cmd run .\cmd\infergate   -config configs\cache-local.yaml
```

```powershell
# 同一个问题问两次：第二次的 X-InferGate-Cache 是 hit-exact，上游没有被调用
curl.exe -s -D - -o NUL http://127.0.0.1:8082/v1/chat/completions -H "content-type: application/json" -d "{\"model\":\"mock-gpt\",\"messages\":[{\"role\":\"user\",\"content\":\"summarise the design document\"}]}"

# 换一种说法问（"please summarise ..."）：hit-semantic，仍然没有调用上游
curl.exe -s -D - -o NUL http://127.0.0.1:8082/v1/chat/completions -H "content-type: application/json" -d "{\"model\":\"mock-gpt\",\"messages\":[{\"role\":\"user\",\"content\":\"please summarise the design document\"}]}"
```

响应头与开关：

| 头 | 取值 | 含义 |
| --- | --- | --- |
| `X-InferGate-Cache`（响应） | `hit-exact` / `hit-semantic` | 命中，且说明是逐字节命中还是语义命中 |
| | `miss` / `skip` | 未命中 / 这条请求按策略不缓存 |
| | `bypass` / `refresh` | 调用方要求跳过查询 / 重新生成 |
| | `disabled` / `error` | 缓存没开 / store 出错（**降级为 miss，不是报错**） |
| `X-InferGate-Cache-Age` | 毫秒 | 被重放的答案有多旧 |
| `X-InferGate-Cache`（请求） | `bypass` / `refresh` | 二者都**仍然会**把新答案写进缓存 |
| `X-InferGate-Tenant` | 任意字符串 | 多租户隔离；不设则按 `Authorization` 的哈希前缀分租户 |

三个观测面（语义缓存出问题时，"没命中"和"命中了错的条目"在响应体里长得一样，所以必须能只看缓存）：

```powershell
curl.exe -s http://127.0.0.1:8082/admin/cache                       # 配置 + 命中率 + saved_tokens + 各 scope 条目数
curl.exe -s "http://127.0.0.1:8082/admin/cache/lookup?prompt=summarise+the+design+document"   # 最近邻与相似度
curl.exe -s -X POST http://127.0.0.1:8082/admin/cache/flush          # 清空（POST：GET 会被预取/爬虫清库）
```

`/metrics` 上的 `infergate_cache_*`（`lookups` / `hits{kind}` / `misses` / `stores` / `errors{kind}` /
`entries` / `evictions` / `hit_ratio` / `saved_tokens_total{kind}`）与 `/stats` 的 `cache` 块同源。

配置项（`configs/cache-local.yaml` 是逐行注释的版本，`configs/cache-redis.yaml` 是共享缓存版本）：

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `cache.enabled` | `false` | 显式开关 |
| `cache.store` | `memory` | `memory`（进程内 LRU）或 `redis`（跨副本共享） |
| `cache.threshold` | `0.86` | 语义门控；**实测值**，换 embedder 必须重跑 `cmd/measure-m2` |
| `cache.ttl` | `15m` | 从写入起算，命中不续期（否则一个热问题能让答案永生） |
| `cache.max_entries_per_scope` | `256` | 每 scope 上限；memory 按 LRU 淘汰，Redis 按创建时间 FIFO |
| `cache.min_prompt_chars` | `12` | 太短的提示不值得 embedding，"hi" 命中别人的 "hi" 不算本事 |
| `cache.allow_nondeterministic` | `false` | `temperature > 0` / `top_p < 1` 只做精确命中 |
| `cache.allow_tools` | `false` | 带 `tools` 的请求只做精确命中（两种说法可以合法地选不同工具） |
| `cache.embedding.provider` | `hashing` | `hashing`（离线词法，零依赖）/ `http`（OpenAI 兼容 `/embeddings`）/ `none`（只做精确命中） |
| `cache.redis.*` | `127.0.0.1:6379` / `ig:cache` | 共享缓存；本机没有 Redis 时可用 `cmd/miniredis`（默认 :6399） |

共享缓存的最小本地栈（无需安装 Redis）：

```powershell
.\tools\go.cmd run .\cmd\miniredis    -listen :6399
.\tools\go.cmd run .\cmd\mockupstream -listen :9201 -name local
.\tools\go.cmd run .\cmd\infergate    -config configs\cache-redis.yaml
```

### 3.10 M2 验收与实测

```powershell
.\tools\go.cmd run .\cmd\verify-m2                                   # Go 门禁，103 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m2.ps1     # curl 门禁，157 条（内存 + 真 Redis 协议，约 20s）
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m2.ps1    # 命中率 / 延迟 / 成本实测
.\tools\go.cmd run .\cmd\measure-m2                                  # 阈值扫描（语料 26 对）
```

阈值扫描（离线 hashing embedder，`internal/evalset` 的 26 对中英标注语料）：

| 阈值 | 真命中率 | 误命中率 | 误命中 |
| --- | --- | --- | --- |
| 0.84 | 77% (10/13) | 23% (3/13) | nm-en-2、nm-en-7、nm-en-8 |
| **0.86** | **62% (8/13)** | **0% (0/13)** | — |
| 0.88 | 62% (8/13) | 0% (0/13) | — |
| 0.90 | 46% (6/13) | 0% (0/13) | — |
| 0.94（旧默认） | 31% (4/13) | 0% (0/13) | — |

怎么读：

- **0.86–0.88 是"零误命中"平台**。0.84 多出的两次命中是拿"删掉 tenant alpha 的缓存条目"命中
  "tenant beta"、拿"给 router 写单测"命中"给 breaker 写单测"换来的——命中率 +15%，代价是
  错租户、错包。
- **离线 embedder 的天花板是 embedder 的属性，不是阈值的属性**：语料里有 5 对真同义改写的
  相似度低于某个近似对，任何阈值都分不开它们。要更高的命中率，就把 `cache.embedding.provider`
  指向真实模型（`http`）并重跑扫描。
- 换 embedder 后平台会整体平移，所以阈值是配置项而不是常量。

端到端实测（`scripts/measure-m2.ps1` → `docs/baseline/m2-summary.json`，本机回环，上游是仓库内的
`cmd/mockupstream`，store=memory，embedder=hashing-512，阈值 0.86）：

| 指标 | 结果 | 口径 |
| --- | --- | --- |
| 命中率 | **58.97%**（23/39：18 精确 + 5 语义） | 13 对应命中语料各发 3 次（A 未命中 → A 精确命中 → B 语义命中） |
| 覆盖到的语料对 | **13/13** | 每对至少命中过一次 |
| 误命中 | **0/26** | 13 对近似但不同的语料各发 2 次，一次都没命中 |
| 端到端 P95 | **47.31ms → 5.50ms** | `/stats`：纯未命中 3626 次 P95 47.3147ms vs 纯命中 3600 次 P95 5.5028ms（p50 5.5327 → 1.0132ms） |
| 命中不产生上游请求 | 上游日志 31 次调用 == 31 次未命中 | 压测每轮 3600 请求 = 3598 命中 + 2 未命中 + 2 次上游调用 |
| 上游 token | 节省 **604**（prompt 319 + completion 285） | `/admin/cache` 全语料 delta；命中时计在 cache 账上，不计入上游 |
| 成本 | 每 1000 请求 **$0.036692 → $0.016**（−**56.39%**） | 按配置价格表（`mock-gpt` in 1 / out 3 USD/1M）外推 |
| 压测错误数 | **0** | 800 请求 + 100 预热 × 3 轮 × 并发 8/32，缓存开/关交错执行 |

怎么读：

- **命中率必须带口径**。62% 是"语料对"口径（阈值扫描，13 对里命中 8 对），58.97% 是"请求"口径
  （每个应命中的对发 3 次，其中 B 只有部分触发）。同一个系统两个数都对，混着说就是错。
- **换一个长度门就会换一个命中率**：出厂配置 `min_prompt_chars: 12` 会跳过 26 对里 6 对
  （中文提示只有 7–8 个字），命中率降到 41.03%（10/13 对）、9 次 `skip`。所以实测跑的是
  `min_prompt_chars: 1` 的临时配置（写进 `tmp/`，不动 `configs/`），两个数都报出来。
- **上游是本机 mock，所以延迟差只说明省掉了网关自己的工作**（一次 HTTP 往返 + provider JSON 编解码），
  不代表省掉了真实 provider 的生成时间；真正硬的结论是"命中不产生上游请求"（mock 自己记的调用数
  恰好等于未命中数）和命中路径的 P95。
- **两个数的口径不同，必须一起说**：节省的 token（319 + 285）是 `/admin/cache` 在**全部 65 次**
  语料请求（含"不该命中"的探针）上的 delta，而 −56.39% 是按 **39 次**应命中请求的工作量模型外推的
  （模型口径下节省 $0.000807）。差的 $0.000367 记为 `cost_cross_check_delta_usd`，不藏。
- 三轮交错只给区间不给置信区间：本机轮间抖动最坏 47.1%（两次跑出来的 P95 差了一倍，
  所以文档里只引用同一次运行内部"命中 vs 未命中"的对比，不跨次引用绝对值）。

### 3.11 Token 配额与成本治理（M3）

配额判定（准入）插在**缓存查找之前、路由之前**，且只治理生成类路径。顺序是承重的：开在缓存
之前，命中虽然不花 token、但仍占每分钟请求配额、仍进租户报表；只治理 `/chat/completions`、
`/completions`、`/embeddings`，是因为把 `/v1/models` 也计入会因为一个零成本请求拒绝调用方，
并让 token 维度在没有任何 prompt 的请求上变成虚构。默认**关闭**。

```powershell
# 起一个小栈：一个 mock 上游 + 一个网关（配置见 configs\quota-local.yaml，内存计数）
.\tools\go.cmd run .\cmd\mockupstream -listen :9300 -name local
.\tools\go.cmd run .\cmd\infergate   -config configs\quota-local.yaml
```

```powershell
# 请求体一律写文件、用 --data-binary @file 发送（PS 5.1 会吃掉原生参数里的引号，见 3.3）
# 1) 正常请求：X-InferGate-Quota 为 allow，并带 X-InferGate-Quota-Reason: within-budget
#    （放行不带限额/已用头，见下表最后一行：只有越界那次才知道限额是多少）
curl.exe -s -D - -o NUL http://127.0.0.1:8084/v1/chat/completions -H "content-type: application/json" -H "X-InferGate-Tenant: acme" --data-binary "@tmp\body.json"

# 2) 额度用尽：429 + infergate_quota_exceeded + Retry-After；此时上游一次都没有被调用
# 3) 每分钟限流：bursty 租户（requests_per_minute: 5）第 6 次开始 429
# 4) 降级：growth 租户超预算后**仍然是 200**，响应头说明降级到哪个模型、上限压到多少；
#    真正的证据在后端收到的请求体里（模型名与 max_tokens 被改写了），不在响应头里
```

响应头与请求头：

| 头 | 取值 | 含义 |
| --- | --- | --- |
| `X-InferGate-Quota`（响应） | `allow` / `degrade` / `reject` | 这次判定是放行、降级放行还是拒绝；**每个受治理请求都有** |
| `X-InferGate-Quota-Reason` | `within-budget` / `tokens_per_day` / `cost_per_day_usd` / `tokens_per_session` / `requests_per_minute` / `store-error` … | 为什么这样判定（机器可读；降级时同时说明是哪个维度越界） |
| `X-InferGate-Quota-Limit` / `-Used` | 数字 | 越界那一维度的限额与已用量；**只有越界（拒绝或降级）时才出现**——放行时网关手上没有该维度的实时余量（那需要一次额外读），所以"还剩多少额度"只能看 `/admin/quota` |
| `X-InferGate-Quota-Model` / `-Max-Tokens` | 模型名 / 数字 | 降级实际改成了什么（`-Max-Tokens` 是压完之后的完成上限） |
| `Retry-After` | 秒 | 拒绝时**总是**给出；没有窗口信息时给 1，因为不带退避提示的 429 会被 SDK 立刻重试 |
| `X-InferGate-Tenant`（请求） | 任意字符串 | 配额与缓存共用的租户身份；不设则按 `Authorization` 的哈希前缀分租户 |
| `X-InferGate-Session`（请求） | 任意字符串 | 会话身份，用来支持"每个会话多少钱"的预算；日预算按 **UTC 当天**计，UTC+8 的机器上本地 08:00 重置 |

三个观测面（"被拒绝"和"被降级"在客户端看来都是少花了一次上游调用，所以必须能只看配额）：

```powershell
curl.exe -s http://127.0.0.1:8084/admin/quota                       # 配置 + 各维度决策计数 + 预扣/结算/超支
curl.exe -s "http://127.0.0.1:8084/admin/quota?tenant=acme&session=sess-1"   # 该租户当天/当分钟/当会话用量与策略
```

`/metrics` 上的 `infergate_quota_*`（`decisions_total{action}` / `store_errors_total` / `alerts_total` /
`tokens_total{kind="reserved"|"settled"|"released"}` / `overshoot_tokens_total` /
`overshoot_cost_micros_total` / `released_cost_micros_total`）与 `/stats` 的 `quota` 块同源。

配置项（`configs/quota-local.yaml` 是逐行注释的内存版，`configs/quota-redis.yaml` 是共享计数版）：

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `quota.enabled` | `false` | 显式开关；关着时配置仍会被校验，但不要求配全 |
| `quota.store` | `memory` | `memory`（进程内）或 `redis`（跨副本共享）。多副本用 `memory` 等于每个副本各发一整份预算 |
| `quota.fail_open` | `false` | 计数器读不到时默认**拒绝**（503 + `infergate_quota_unavailable`）；读不到的预算不是预算 |
| `quota.estimate_chars_per_token` | `4` | 只用于**预扣**的 prompt 估算；provider 的真实 `usage` 一到就以它为准 |
| `quota.estimate_completion_tokens` | `256` | 调用方没声明完成上限时按这个预扣，结算时释放没用完的部分 |
| `quota.anomaly_ratio` | `3` | 当日用量超过此前有流量几日均值的倍数就告警；**只告警，不拦流量**，0 关闭 |
| `quota.default_policy` | 全 0 | 没单独配的租户用它；`tokens_per_day` / `cost_per_day_usd` / `requests_per_minute` / `tokens_per_session` 为 0 表示该维度不限 |
| `quota.tenants[]` | — | 每租户一份策略：四个维度 + `on_exceed`（`reject` / `degrade`）+ `downgrade_model` + `max_tokens_cap`；租户名重复是配置错误而不是后者覆盖前者 |
| `quota.redis.*` | `127.0.0.1:6379` / `ig:quota` | 共享计数；本机没有 Redis 时用 `cmd/miniredis`（默认 :6399） |

共享计数的最小本地栈（无需安装 Redis）：

```powershell
.\tools\go.cmd run .\cmd\miniredis    -listen :6398
.\tools\go.cmd run .\cmd\mockupstream -listen :9301 -name local
.\tools\go.cmd run .\cmd\infergate    -config configs\quota-redis.yaml
```

计数键的布局（`GET` 就能读当天用量，跨天不需要迁移）：

```
ig:quota:<tenant>:day:<YYYYMMDD>:tokens        预扣与结算都打在这个键上
ig:quota:<tenant>:day:<YYYYMMDD>:cost_micros   成本按微美元（1e-6 USD）整数记账，避免浮点累加漂移
ig:quota:<tenant>:minute:<YYYYMMDDHHmm>:requests
ig:quota:<tenant>:session:<id>:tokens
```

上面的 `<tenant>` 是**转义后**的租户名（`X-InferGate-Tenant` 是调用方控制的，而它是 key 的一部分）：
`_` 写成 `__`，其它非 `[A-Za-z0-9.-]` 字符写成 `_x` + 固定六位十六进制，所以 `acme:inc` →
`acme_x00003ainc`、`acme_inc` → `acme__inc`：两个名字不可能共用一本账。转义是**单射**的（固定宽度），
变宽编码会让 `U+10FFF`+`"ff"` 和 `U+10FFFF`+`"f"` 撞成同一个 key。

`on_exceed: degrade` 的两个杠杆都可以单独用；两个都没配时**配置校验期**就报错，而不是在运行时
"降级"成一个什么都没改的请求。降级请求**保留预扣**（它仍然是一次请求、仍然要结算），
便宜模型没用完的额度在结算时还回去。

### 3.12 M3 验收与实测

两条验收路径（都是真进程）：

```powershell
.\tools\go.cmd run .\cmd\verify-m3                                             # Go 门禁，470/470 断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m3.ps1     # curl 门禁，323/323 断言（约 40s）
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m3.ps1    # 实测，57/57 断言（约 70s）→ docs\baseline\m3-summary.json
```

curl 门禁自己编译三个二进制、拉起内存与 Redis 两条栈、并**杀掉 miniredis** 来验 fail-closed：
`X-InferGate-Quota-Reason` 从 `within-budget` 变成 `store-error`、客户端拿到 503、上游一次都没被
调用，重启 store 后同一个网关进程（PID 不变，无需重启）恢复 200。

治理到底花了多少、挡住了多少（`docs/baseline/m3-summary.json`，3 轮交错，n=800/warmup=100）：

| 测点 | 结果 |
| --- | --- |
| 准入开销（治理 vs 不治理，非流式） | 并发 8：2533 QPS / P95 5.42ms vs 2439 / 4.83ms；并发 32：2589 / 14.49ms vs 2632 / 15.05ms，**错误数 0** |
| `/stats` 自视（同一批请求） | 治理 p50 4.41 / p95 15.48ms，不治理 p50 4.74 / p95 15.49ms |
| memory vs Redis 计数 | 并发 8：2960 QPS / P95 3.97ms vs 2257 / 5.49ms；并发 32：3346 / 13.54ms vs 2743 / 13.84ms（每次预扣/结算多一次回环 RESP2 往返） |
| 预扣 vs 真实用量（12 请求） | reserved 2872（= 逐请求公式之和）、settled 320（= 真实 usage 之和）、released 2608、overshoot 56 tokens / 171 微美元；`reserved − released + overshoot == settled` 成立 |
| 估算偏差 | 平均绝对偏差 222 tokens，平均有符号 **+212.7**（系统性高估：completion 预扣 256，mock 只产 4 个词） |
| 预算挡下的上游调用 | 同 40 个请求：无预算 40/40 到上游（40×200）；`tokens_per_day: 280` 时 1×200 + 39×429（97.5% 被拒），**上游调用从 40 降到 1** |
| fail-closed | 停掉 miniredis：20/20 得到 503 `store-error`，上游调用 **0**，`store_errors` 20；重启后 10/10 200（网关未重启） |
| fail-open | 同一故障只把 `fail_open` 翻成 true：5/5 得到 200，5 次上游调用 |
| 降级梯子 | `growth` 超预算：200 + `degrade` + `mock-gpt-mini` + 上限头 64，后端日志确认收到的就是 `mock-gpt-mini` |
| Redis 里的真实计数 | 裸 RESP2 `GET`：日 token 32238 → 64305 → 96327（每轮 Δ 约 32064），分钟请求键 7200 |

怎么读这几行：

- **治理的开销落在这台机器的轮间抖动之内**。治理臂并发 8 的三轮是 5230 / 2262 / 2533 QPS，跨度
  2.3×；同一次运行内部对比是有效的，跨次引用绝对值没有意义。
- 上游是**仓库内的 mock**（本地、免费、没有 provider 账单），所以这里量到的是"网关自身多做的工作"
  （扫 body + 估算 + 一次 store 往返），不是 provider 延迟的节省；成本一律按配置价目表外推。
- mock 的 token 口径是"空格分词"（prompt = 词数 + 4），不是真 tokenizer，所以上面那条 +212.7 的
  系统性高估是这个配对的属性，不是估算公式的普适结论。
- 降级把 `max_tokens` 改写进了转发体，但 `cmd/mockupstream` 完全忽略 `max_tokens`，所以
  "降级省了多少 token"这一项测不出来（只能证明模型名与上限被改写、预扣照旧）。
- Redis 臂走的是仓库内 `cmd/miniredis`（回环 RESP2）：验的是代码路径与键布局，不是生产 Redis。
- fail-open 的计数天然不完整（store 没记上那几笔），所以它的可信度低于 fail-closed 臂。
- 日成本键只在"配了钱"的租户上打开；压测租户用默认策略（不限钱），所以 RESP2 里读不到 cost 键，
  钱的部分从 `/admin/quota` 的 `released_cost_micros` 读。

---

### 3.13 分层路由与本地推理（M4）

M4 的产品能力是**分层路由**：给每个上游标一层（`tier: local` / `tier: cloud`），再在网关上定义
"什么算简单请求"。简单请求本地优先、难请求云端优先，于是"多少比例的请求被本地接住、省下多少云侧
花费"变成一个可测的数字，而不是一句愿景。分层是**排序**，不是过滤——所以 M1 的能力筛选与跨层故障
转移都还在（见 `docs/DESIGN.md` 11.1–11.3）。

`configs/tiered-local.yaml` 是自洽的形态：本地层是**真的 vLLM**（WSL2 + GPU 直通），云端层用仓库内
的 `mockupstream` 顶替付费 provider（不花钱、离线可跑，但单价表是真的）。

```powershell
# 0) 本地层：WSL 里起一个真的 vLLM（M4 的所有数字都来自它，不是 mock）
#    环境搭建（驱动/CUDA 大版本、transformers 上界、gcc、镜像源）见 docs\DESIGN.md 11.4
#    下面这行是 fp16；分层实测那一档用的是 AWQ 权重，把路径换成 /opt/models/awq 即可复现
wsl -d Ubuntu24 -u root -- /opt/vllm/bin/vllm serve /opt/models/fp16 `
    --served-model-name local-chat --port 8000 --gpu-memory-utilization 0.85 --max-model-len 4096

# 1) 云端层：假上游顶替付费 provider
.\tools\go.cmd run .\cmd\mockupstream -listen :9100 -name cloud-mock

# 2) 网关：strategy=tiered，本地层 = vLLM，云端层 = mockupstream
.\tools\go.cmd run .\cmd\infergate -config configs\tiered-local.yaml
```

```powershell
# 3) 短 prompt → 本地层；把 prompt 写长（估算超过 local_max_prompt_tokens: 400）→ 云端层。
#    请求体一律写文件用 --data-binary @file 发送（PS 5.1 会吃掉原生参数里的引号，见 3.3）
curl.exe -s -D headers.txt -o body.txt -H "Content-Type: application/json" `
    --data-binary "@body.json" http://127.0.0.1:8080/v1/chat/completions
#    看 X-InferGate-Upstream-Name / -Model，以及网关日志里的那一行：
#      strategy=tiered tier=local reason=simple request prefers this tier
#    两个请求只差请求体长度，层就换了——判据是"序列化后的消息长度 / 4"，与 M3 的预扣估算同一个常数
```

三个可复现的门（前两条不需要显卡，第三条需要）：

```powershell
.\tools\go.cmd run .\cmd\verify-m4                                          # Go 门禁，877/877 断言（16 组）
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m4.ps1   # curl 门禁，125/125（两层假上游，不需要 GPU）
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m4.ps1  # 实测：三变体量化对比 + 分层分流 → docs\baseline\m4-summary.json
```

量化对比（同一份 12 条固定 prompt、同一套服务参数、单并发、单轮；fp16 为参照）：

| 变体 | 权重文件 | 加载 | 首字 P50 | 端到端 P50 | 吞吐（tok/s，请求墙钟） | 显存增量 | 与 fp16 完全一致 | 平均 token 重合 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| FP16（参照） | 3.09 GB | 58s | 35.2ms | 1758.5ms | 41.5 | 6169 MiB | — | — |
| AWQ（4bit, group 128） | 1.61 GB | 60s | 28.1ms | 699.5ms | **100.7** | 6921 MiB | 0/12 | 0.444 |
| GPTQ-Int4 | 1.15 GB | 55s | 28.7ms | 723.6ms | 95.3 | 7353 MiB | 1/12 | 0.460 |

同一批请求里量化模型的真实差异（12 条里最容易看的两条）：`4873+6259` 那条，fp16 与 GPTQ 都答
`11132` 且差值 `1386`，AWQ 把差值答成 `-1386`——这就是它 0/12 完全一致的原因；问"澳大利亚人口
（百万）"那条，fp16 给 4.1，AWQ 给 40，GPTQ 给 2.8。也就是说 **token 重合率 0.44 这个量级不是
"输出没崩"的证明，而是"语义框架还在、数字与措辞会漂"的证明**。

相对 fp16（同一次运行内部对比）：AWQ 吞吐 **2.42×**、端到端 P50 **−60.2%**、首字 P50 −20.0%、
权重文件 **−47.7%**；GPTQ 吞吐 **2.30×**、端到端 P50 −58.8%、首字 P50 −18.5%、权重文件 −62.8%。
两者吞吐只差 5.6%（12 条、单轮，低于本机可分辨的门槛），所以这张表能支持"量化把这张 8 GB 卡上的
本地推理变成可用"，不能支持"AWQ 与 GPTQ 谁更好"。

分层分流（本地 vLLM + 云端 mock，同一批请求走网关，产物 `docs/baseline/m4-summary.json`）：

一次运行 16 个请求——8 条"短 prompt"（约 42 字符 user 文本）+ 8 条"长 prompt"（2444 字符），两者
`max_tokens` 都是 96；归属不看网关日志的说法，而看每个响应上的 `X-InferGate-Upstream-Name`：

| 层 | 接到哪些请求 | 客户端墙钟 P50 / P95 | 层内 token（prompt / completion） | 按单价折算 |
| --- | --- | --- | --- | --- |
| `local-vllm`（真 vLLM，AWQ 4bit） | 8/8 短请求 | 560.4ms / 946.2ms | 312 / 421 | 0.00054734 USD |
| `cloud-mock`（仓库内 mock 顶替付费 provider） | 8/8 长请求 | 71.8ms / 135.2ms | 3096 / 184 | 0.00103832 USD |

- **分层的判据是可复现的**：短请求 8/8 被本地接住、长请求 8/8 去了云端，`/stats` 的 per-upstream
  增量同样是 local +8 / cloud +8（网关自己数的）；同一批请求把 prompt 写到 2400 字符以上就是换层，
  边界仍然是"序列化消息长度 / 4"这个估算值，不是模型名。
- **"省下的云侧花费" = 本地接住的那 8 条按云侧单价折算出来的钱**：0.00054734 USD；同一批 16 个请求
  全走云侧要 0.00158566 USD，所以这次分流打掉了云侧账单的 **34.52%**。本地层的现金边际成本是 0，
  但网关按模型名定价、两层都答 `local-chat`，所以这 0.00054734 是**按云侧单价记账**的数字——
  它是"账面替代"，不是"少付的账单"；云端层自己那 8 条仍然记 0.00103832 USD。
- **层间延迟差不是"本地比云端慢"的结论**：云端层是仓库内 mock，秒回；本地层是真模型在笔记本 GPU 上
  逐字生成，所以 560.4ms vs 71.8ms 只证明"真模型有生成时间、mock 没有"（网关 `/stats` 看整批 16 个
  请求是 P50 342.6ms / P95 851.8ms）。同一天三次运行本地 P50 分别是 560.4 / 627.2 / 784.5ms
  （同机同权重，最大摆动 40%），所以这里只引用同一次运行内部的对比。

怎么读这几行：

- **量化对比不是精度评测**：12 条 prompt、单次采样，`完全一致 / token 重合` 只能说明"输出没崩"，
  不能当准确率用；产物里也这么写。
- **显存要读增量**：`nvidia-smi` 的 `memory.used` 包含 Windows 桌面占用的 1.7–1.9 GiB，所以只报
  加载前后的差值，且 ±50 MiB 以内不算差异；GPTQ 的增量比 FP16 大是因为 vLLM 按
  `--gpu-memory-utilization 0.85` 预分配 KV cache，权重小了反而留出更多缓存——这是口径问题，
  不是"量化更费显存"。
- **不读小于 5% 的差异**：单并发、单轮、共享散热受限的笔记本 GPU，吞吐差异在几个百分点内没有
  区分度；跨次引用绝对值同理（M2/M3 的 47% 轮间抖动就是前车之鉴）。
- 本地层的边际成本按 0 计，所以"被省下的云侧花费"= 本地服务的那些请求按**云侧单价**折算出来的钱，
  单价取自 `pricing`（`internal/gateway/pricing.go`），不是账单。
- 分层实测只覆盖两种固定形态（16 个请求、单并发、非流式、每层各 8 条），它证明的是这个 mix 下的
  分类与分流、以及本地层确实在被调用（本地答案是真模型的输出且带 `usage`），不代表负载下、
  流式下或估算边界上的行为。

### 3.14 M5 验收与实测：全链路可观测与压测基线

**做了什么**

- `/metrics` 从"只有计数器"补成真直方图：同名替换 `infergate_request_duration_seconds`（原来是带
  `_sum` 后缀的 TYPE counter，任何面板都算不出分位数），另加 `infergate_upstream_attempt_duration_seconds`、
  `infergate_first_token_seconds`、`infergate_completion_tokens_per_request`，固定分桶 + `+Inf` 累积桶，
  仍然零依赖；延迟样本窗口改成 65536 环形缓冲，并把 `latency.window` / `latency.dropped` 报出来
  （丢样本必须可见，否则"P99 变好了"可能只是样本被丢了）。M2 的缓存族
  （`infergate_cache_hit_ratio`、`hits_total{kind="exact"|"semantic"}`、`saved_tokens_total`）由同一个
  `/metrics` 继续暴露，dashboard 里的命中率面板用的就是它。
- Trace 复用 W3C `traceparent`：入站有就跟随、没有才新建 root（中途换 trace id 会让"客户端 → 网关 →
  上游"不再是一条 trace）；一条请求 = 一个 `gateway.request` server span + 每次上游尝试一个
  `upstream.<name>` client span，流式也是一个 span、帧数/字节/首字延迟挂属性；`/admin/traces` 可以按
  request id 回放（日志里能看到的就是 request id）。导出两个 sink：手写 OTLP/HTTP(JSON) 与 JSONL，
  都走后台 worker，`Export()` 只入队。
- 压测由 `cmd/loadtest`（无第三方依赖，自带 SSE 解析与首字计时）驱动，`scripts/measure-m5.ps1` 产出
  `docs/baseline/m5-summary.json`；`deploy/` 给出 Prometheus 抓取配置（`infergate-single` /
  `infergate-fleet`）与 33 面板的 Grafana dashboard——**但本机没有 Prometheus / Grafana 二进制，
  deploy 只验证到"能解析 + 每个指标名与契约逐条对齐"，没有真的抓取、查询或渲染面板**
  （写在 `deploy/README.md` §6）。

**实测（i9-14900HX / 32 逻辑核，单机回环，后端是仓库内 mock，`log.level: error`，8 臂交错 × 3 轮取中位，
每相位 3000 请求 + 300 预热）**

| 臂 | 非流式 c=8 / 32 / 128 QPS（P95 ms） | 流式 c=8 / 32 / 128 QPS（P95 ms，TTFT P95 ms） |
| --- | --- | --- |
| `direct`（证人臂） | 5834（2.10）/ 5688（7.42）/ 5623（26.00） | 1500（6.41，1.51）/ 3448（11.78，1.55）/ 3437（39.31，1.23） |
| `gateway` | 4462（2.39）/ 4560（7.88）/ 4382（32.82） | 1366（6.93，1.55）/ 2731（14.15，1.63）/ 2750（56.70，1.59） |
| `gateway-scraped` | 4110 / 4255 / 3959 | 1320 / 2656 / 2728 |
| `traced-otlp` | 3922 / 3666 / 3822 | 1338 / 2690 / 2701 |
| `traced-full`（+JSONL） | 3847 / 3872 / 3720 | 1327 / 2647 / 2731 |
| `horizontal-2` | 4347 / 4286 / 4415 | 1332 / 2749 / 2829 |
| `horizontal-4` | 4387 / 4044 / 4340 | 1317 / 2637 / 2749 |

怎么读这几行：

- **网关是限速的那一半（这一次）**：非流式 c=128 少 22.07% QPS（4382 vs 5623，P95 +6.82ms / P99 +4.59ms），
  流式 c=128 少 20.00%（2750 vs 3437，P95 +17.39ms / P99 +14.96ms）。但这些差值必须配对同轮噪声带读：
  非流式三个档位都超带，流式 c=8 / c=128 超带、c=32 的 20.79% 落在 26.04% 的带宽内。
- **绝对值不可跨运行引用，"谁比谁快"也不可单独引用**：同一条命令、同一台机器、相邻两次全量运行，
  `direct` 非流式 c=8 从 1909 到 5834 QPS（**3.06×**），而"网关 vs 直连"的符号直接翻转
  （前一次 +15.6 / +16.2 / +16.3%，这一次 −23.5 / −19.8 / −22.1%）。只有同一次运行内、与同轮带宽
  比较的差值可读；§12.5 的证人臂教训（31.3k → 8.7k QPS）在这里是必答题。
- **c=128 的流式臂其实测的是客户端**：客户端观测均值 45.60ms 而网关自报 15.05ms，墙钟大半花在压测
  客户端自己身上（脚本把 8 个这类相位单列进 `limitations`）。
- **tracing 的成本取决于负载形态**：非流式在 `sample_ratio 1.0` 下 −12.09% / −19.60% / −12.79% QPS
  （P95 +0.31 / +2.36 / +4.71ms，全部超带）；流式 −2.02% / −1.50% / −1.77%（基本在带内）。
  再加一个 JSONL sink 在六对比较里都不产生可测开销（−1.93% … +5.63%）——因为导出只入队，
  采集端慢只会涨 drop 计数。
- **被抓取是有代价的，但 100ms 常轮询是上界**：6 对里 3 对超带，最大 −9.65% QPS（非流式 c=128，
  P95 +5.75ms）；抓取器自己实测 9.702 端点 GET/s（请求 10，三轮离散 3.25%）。真实 Prometheus 是
  5–15s 一次，且差值里有一部分是抓取进程抢 CPU。
- **水平扩展在单一后端下不涨吞吐**：2 实例加速比 0.94–1.03×、4 实例 0.89–1.00×（理想线性度分别是
  100%，实测 47–51.5% / 22.2–25.0%），因为所有实例共享同一个 mock 后端；同配置的两个实例本身就差
  −3.11% … +4.06%，这就是这台机器能分辨的下限。
- **一致性证据**：144 个相位 `errors=0`；一轮 `gateway` 臂期间 `infergate_requests_total` 恰好
  +19800 = 18000 派发 + 1800 预热；6 个实例都报 `window=65536`、P50 ≤ P95 ≤ P99、无 open 熔断、
  都声明 `# TYPE infergate_stream_bytes_total`；OTLP `exported=59400` / `failed=0`，采集端 118714 次
  POST 全落在配置端点 + `/v1/traces`；JSONL 59400 行与 `export_stats` 对齐。
- 这份基线**不是**容量承诺：后端是仓库内 mock、单机回环、`log.level: error`（不含 §12.5 那 18% 的
  日志成本）、导出器异步（量的是建 span + 入队）、`sample_ratio 1.0` 是最坏情况、抓取是上界，
  固定请求数还意味着机器越快相位越短（本次 c=8 非流式相位约 0.5s）。

验收：Go `cmd/verify-m5` **420** 条断言 + 真实进程 curl `scripts/verify-m5.ps1` **211** 条断言
（两个真 mock + 仓库自带的 `cmd/mockcollector`）+ `scripts/measure-m5.ps1` **10** 项检查全部通过、
**0** 条失败断言（72 条在写产物前记录，12 条收尾断言在其后执行，标准输出共 84 条）。设计取舍与全部
口径见 `docs/DESIGN.md` §12。

### 3.15 M6 验收与实测：幂等重放、会话账本与能力发现

**做了什么**

- **幂等重放**：调用方给 `Idempotency-Key`，网关用"租户作用域 + 请求指纹"（`method \0 path \0 body`
  的 SHA-256 前 16 字节，method 与 path 必须进哈希——同一段 body 发到 `/v1/embeddings` 与
  `/v1/chat/completions` 不是同一个操作）认领一个逻辑操作，每个带 key 的请求只有四种归宿：
  Proceed / Replay / 409 `infergate_idempotency_conflict`（同 key 换 body）/ 409
  `infergate_idempotency_in_flight` + `Retry-After: 1`（同 key 还在飞）。**只有 2xx 与 4xx 才可重放**，
  5xx / 超时 / 被取消的流一律释放占用——把一次瞬时失败钉死在一个 key 上，等于把这个操作永久变成失败。
  写入发生在响应**写出之后**（`serve()` 的 deferred recorder），所以再慢的 store 也不会拖慢一个答案；
  响应副本是 tee 不是缓冲（字节立刻到客户端、`Flush` 转发、`Unwrap` 让 `ResponseController` 还能拿到
  Hijacker），上限 `max_response_bytes + 1`，超限的答案**照常送达、只是不被记住**。重放把记录的字节
  原样送出，记录下来的流式响应作为一整段 body 交付（转录就是答案，重编帧时序等于凭空发明一个上游从未
  有过的生成速度）。
- **会话账本**：`X-InferGate-Session` 一次归属、两处受益——M3 的配额可以按会话记，`/admin/sessions`
  又能回答"这段对话花了多少"（requests/ok/failed、prompt/completion/cached token、按**实际服务的模型**
  定价的成本、模型与上游 rollup、最近 N 条请求）。空 id 只加 `no_session_id` 并且**不建会话**；
  TTL 由 janitor 按 `SweepInterval`（默认 1 分钟）清扫，管理面只报事实（`ttl` 与每条的 `expires_at`），
  "真的会被清掉"由单测证明，而不是让验收器睡一觉去赌一次清扫。会话的四个汇总在 `/metrics` 里是
  **gauge 不是 counter**（容量淘汰会让它下降，Prometheus 的 counter 不允许下降），并且刻意**不带租户
  标签**——租户来自调用方可控的请求头，做成标签就是让人往指标基数里注入任意维度。
- **能力发现**：`GET /v1/capabilities` 纯计算、无 I/O，回答"哪些模型存在、上下文窗口与最大输出多少、
  声明了哪些能力、哪些后端在服务它、现在是否可用"——协议本身不会告诉 Agent 一个模型能装多少上下文，
  而 Agent 决定要不要压缩历史时必须知道。`POST /v1/capabilities/probe` 反过来**故意绕过缓存、配额、
  熔断与重试**：探测是诊断流量而不是客户流量，让它去消耗租户预算、或被一个 open 的熔断器拦掉，就把
  "这个后端到底行不行"偷换成了"现在允许我问吗"。回答的 `accepted` 只声明"后端没有拒绝这个请求形状"，
  不是对答案质量的说法，响应里的 `note` 原文写明了这一点；一个只声明 `models: ["/"]` 的 catch-all
  后端在没给 `model` 参数时报 `skipped` 与理由，而不是替它编一个模型名。
- **顺手修掉的缺陷**：第一版验收器发现上游**真的收到了** `Idempotency-Key`（`copyHeaders` 只丢
  hop-by-hop 与 `x-infergate-` 前缀）。这不是卫生问题而是跨租户串答案：上游若用自己的幂等实现按这个头
  去重，两个租户凑巧撞上同一个 key 字符串就会拿到对方的答案。现在 `isGatewayHeader` 把它一并拦下，
  并由 `TestGatewayHeadersDoNotReachProviders` 双向钉死（出站丢 key / `X-InferGate-*` / Connection
  列出的头 / hop-by-hop，保留 Authorization、Content-Type 与普通自定义头；入站仍保留
  `X-Ratelimit-Remaining` 这类上游回传头）。
- **Warden（Python Agent 平台）侧接入**：客户端把 Warden 自己的会话/线程 id 作为
  `X-InferGate-Session`、租户作为 `X-InferGate-Tenant`，并为每个**逻辑轮次**生成可复现的
  `Idempotency-Key`（重试同一个 key）；`409 ..._in_flight` 按 `Retry-After` 重试、`409 ..._conflict`
  当作 key 派生 bug 直接失败、看到 `X-InferGate-Idempotent-Replay: true` 视为成功并记录；
  遇到不支持这些头的网关（或无这些头）自动退化成普通请求；启动时惰性拉一次 `/v1/capabilities`
  缓存上下文窗口。

**实测（i9-14900HX / 32 逻辑核，单机回环，两个真网关进程 + 仓库内 mock，`log.level: error`，
3 轮取中位，每相位 1500 请求 + 300 预热；原始数据 `docs/baseline/m6-summary.json`，
`scripts/measure-m6.ps1` 9 项检查 / 0 条失败断言）**

| 负载 | c | m6-off QPS | m6-on QPS | ΔQPS | off / on P95 ms | ΔP95 |
| --- | --- | --- | --- | --- | --- | --- |
| 非流式 | 8 | 1915.1 | 2051.8 | +7.14% | 6.562 / 5.748 | −12.41% |
| 非流式 | 32 | 2411.4 | 2249.6 | −6.71% | 16.211 / 18.080 | +11.53% |
| 流式 | 8 | 119.5 | 119.8 | +0.24% | 70.226 / 70.128 | −0.14% |
| 流式 | 32 | 483.3 | 477.5 | −1.20% | 69.376 / 72.837 | +4.99% |

收益与容量（同一份产物）：**重放 20 对**里重放没有一次打到上游（mock `/calls` 不动），单轮省
6 + 5 token ≈ **$0.000021**（20 轮共 120 + 100 token = $0.00042，按 `pricing` 折算），重放 P50
**4.5ms** vs 真实生成 P50 **5.7ms**（1.27×）；**300 个不同 key 泵进容量 256** → `stored` 封顶 256、
`evicted` 44（`stats.stored` 320），256 条答案的载荷下界 **71.3 KiB**（256 × 285 B），RSS 增量
**−4.05 MiB**；**16 个同时到达的同 key 请求** → 1 个 200 新答案 + 15 个 409 in-flight、0 个重放，
上游恰好被问一次。

怎么读这几行：

- **c=8 的符号不可引用**：同一条命令的另一次全量运行给的是 **−5.96%**（这次 +7.14%），且对照臂自己
  三轮就跨 **1888–4194 QPS**——中位数旁边那两次是快轮，把对照臂的中位压低了。这台机器在 c=8 上分辨
  不出 M6 的成本。
- **c=32 非流式是唯一跨运行复现的信号**：两次运行都给 P95 **+11.5%**（+11.53% / +11.78%），QPS
  −6.71% / −15.13%（同号、量级不稳）。
- **流式看不出来**：mock 每词睡 15ms 把相位填满（P50 66.5 → 66.3ms），网关侧成本被淹没。
- **内存 +4.9~5.1 MiB**（31.1 → 36.0 / 30.4 → 35.5 MiB）：A 臂不带 key，所以这 5 MiB 里没有一条被
  记住的答案，是 trace 环与账本的成本；重放只快 1.27× 也不该被引用成"重放很快"——那一列被 mock 的
  限速支配，这一臂要证明的是**上游没被调用**。
- 这份基线**不是**容量承诺：后端是仓库内 mock、单机回环、`log.level: error`；A 臂（`cmd\loadtest`）
  根本没有自定义头参数，所以它量的是 M6 接线的每请求固定成本，不是"存下一条答案"的成本。


验收：Go `cmd/verify-m6` **381** 条断言 + 真实进程 curl `scripts/verify-m6.ps1` **149** 条断言
（上游是仓库内可脚本化的 `cmd/mockupstream`，并用它自己的 `GET /calls` 作外部证人——"两次客户端尝试、
上游只被调用一次"这句话由上游数出来，不是由网关自己声称）+ `scripts/measure-m6.ps1` 的检查全部通过。
设计取舍与全部口径见 `docs/DESIGN.md` §13。

---


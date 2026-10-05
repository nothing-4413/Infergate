# InferGate —— Agent 推理网关（多模型路由 / 语义缓存 / 成本治理）

面向 Agent / LLM 应用的**推理网关**：统一多 Provider 接入、智能路由、语义缓存、Token 成本治理、
稳定性治理、本地推理服务化与全链路可观测。它同时是另一个 Agent 项目 Warden 的底层模型接入层。

当前进度：**M0 / M1 已完成，双路验证全绿**。M0：Go 端到端 38/38、curl 端到端 47/47；
M1：Go 端到端 64/64、curl 端到端 56/56，多 Provider 路由 + 故障转移实测 54000 请求 0 错误。

---

## 1. M0 做了什么

M0 只做一件事：**把 OpenAI 兼容协议透明地代理出去，并把 SSE 流式做到"一个字节都不改写"**，
同时记录每次请求的 token、成本、首字延迟、分片数。后续里程碑（路由、缓存、配额）都建立在
"M0 的转发与观测是对的"这一前提上，所以 M0 的重点不是功能多，而是**可验证**。

| 能力 | 实现要点 | 验证方式 |
| --- | --- | --- |
| OpenAI 兼容透传 | 反向代理语义：入站 path 原样拼到 `base_url` 后；请求体先解析成 `map[string]json.RawMessage`，避免大整数/浮点被重新格式化 | `cmd/verify` 逐字节比对；curl 断言 `chat.completion` |
| SSE 流式 | 自研 SSE 帧解析器（`internal/sse`），保留上游原始字节并逐帧 `Flush` | curl `-N` + `--trace-time` 证明首帧到达时间 |
| `tool_call` 增量 | 按 `choices[].delta.tool_calls[].index` 累积 `arguments` 分片 | curl 断言 3 个分片拼成合法 JSON |
| 首字延迟 | 只在**首个非空 content / tool_calls 帧**打点，跳过 OpenAI 的空 role 帧 | `/stats.first_token_mean`、curl trace 时间戳 |
| Token 与成本 | 只采信 provider 返回的 `usage`（含 `prompt_tokens_details.cached_tokens`），按配置单价折算 USD | `/metrics infergate_tokens_total`、日志 `cost_usd=` |
| 稳定性 | `ReadHeaderTimeout` / `IdleTimeout` / 单次 `upstream_timeout`；模型不可解析返回 **400**（调用方问题）而非 502（后端问题） | 故障注入 429 / 500 / 超时 / 404 |
| 可观测 | 结构化日志（slog）+ `/stats` JSON + `/metrics` Prometheus 文本 | 脚本断言日志字段与指标名 |
| 流式收尾 | 上游未发 `[DONE]` 时由网关补齐，避免 OpenAI SDK 客户端挂死 | `X-Mock-Omit-Done: 1` 故障注入 |

**M0 的关键设计取舍（可逐行讲解的部分）**

1. **零第三方依赖**：只用标准库。`internal/miniyaml` 是手写的 YAML 子集解析器，它把 YAML
   规范成 JSON 再交给 `encoding/json`，因此 `json` tag 是配置 schema 的唯一真相来源。
   理由见第 4 节（本机模块下载不可靠），且 M0 配置面很小。
2. **不缓冲上游响应**：`http.Server` 故意不设 `ReadTimeout` / `WriteTimeout`，否则长回答会被静默截断。
   真正的边界是 `ReadHeaderTimeout`（防 Slowloris）、单次 `upstream_timeout`、`IdleTimeout`（keep-alive 空转）。
3. **出站 context 不继承 `r.Context()`**：`http.Server` 会在 handler 返回或读体失败时取消入站 context，
   长流会被连带中断。`buildRequest` 从 `context.Background()` 派生，另起一个 goroutine 监听客户端断开。
4. **一个 writer**：`relayStream` 中只有一个 reader goroutine 拥有 `resp.Body`，通过带缓冲 channel 投递帧，
   主循环是唯一向客户端写入的 goroutine，因此不需要写锁，也不会交叉写坏帧。
5. **`max_idle_conns_per_host` 默认 64**：Go 默认值 2 会让并发请求退化成"每请求新建 TCP+TLS"，
   这是网关吞吐最容易踩的坑，也是单点最重要的旋钮。
6. **错误分类即责任划分**：模型无法解析 = 400（调用方/配置问题）、上游不可达 = 502、
   上游超时 = 504、客户端断开 = 499（nginx 约定，不写响应体）。

---

## 2. 目录结构

```
infergate/
├── cmd/
│   ├── infergate/          # 主程序入口：加载配置、起服务、优雅退出
│   ├── mockupstream/       # OpenAI 兼容假上游，支持故障注入（延迟 / 异常码 / 省略 DONE）
│   ├── loadtest/           # 压测与基线：直连 vs 经网关 × 流式/非流式 × 多并发
│   ├── verify/             # M0 Go 端到端验收（38 条断言，CI 门禁）
│   └── verify-m1/          # M1 Go 端到端验收：路由 / 故障转移 / 熔断（64 条断言）
├── configs/
│   ├── infergate.yaml      # 生产形态示例（OpenAI / DeepSeek / 本地兜底）
│   ├── mock.yaml           # 本地形态示例（指向 mockupstream，单上游 = M0 路径）
│   ├── routing.yaml        # 生产形态多 Provider 路由（成本/延迟/可靠性权重 + 健康度窗口）
│   └── routing-local.yaml  # 本地三副本路由（primary/secondary/tools，供 curl 验收与压测）
├── internal/
│   ├── config/             # 配置加载：YAML -> JSON -> struct，环境变量覆盖，启动即校验
│   ├── miniyaml/           # 手写 YAML 子集解析器（代价与收益见 docs/DESIGN.md）
│   ├── sse/                # SSE 帧解析 / 写出 / 增量观测（usage、tool_call、首字）
│   ├── upstream/           # Provider 注册表：模型 -> 目标，per-target 连接池
│   ├── router/             # 路由决策：候选筛选 + 能力/健康/成本/延迟打分排序
│   ├── breaker/            # 滑动窗口熔断：closed / open / half-open + 半开探针
│   ├── stats/              # 每上游滑动窗口统计（attempts/failures/timeouts/延迟/首字）
│   ├── gateway/            # 代理核心：路由、多次尝试转发、流式中继、错误映射、计费
│   ├── metrics/            # 内存指标聚合（请求 / 尝试 / token / 首字 / 流分片 / 熔断）
│   ├── mockbackend/        # 进程内假上游（验收程序用，可按后端注入故障与停顿）
│   ├── logging/            # slog 初始化
│   └── server/             # HTTP 服务与运维端点（/healthz /readyz /stats /metrics /admin）
├── scripts/
│   ├── verify-m0.ps1       # M0 curl 端到端验收（47 条断言，真实进程 + 真实 curl）
│   ├── verify-m1.ps1       # M1 curl 端到端验收：优先级/能力/指定/熔断/恢复（56 条断言）
│   └── measure-m1.ps1      # M1 实测：路由开销、故障吸收、熔断省下的延迟
├── tools/go.cmd            # 本机工具链 shim（GOROOT / GOCACHE 重定向，见第 4 节）
└── docs/
    ├── DESIGN.md           # 模块划分、请求生命周期、关键决策与踩坑记录
    ├── RESUME.md           # 每个里程碑对应的简历项目描述（含量化指标占位）
    └── baseline/           # 压测原始数据（m0-baseline.json、m1-*.json）
```

---

## 3. 快速开始

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
# 两条路径：Go 端到端 + 真实进程 curl
.\tools\go.cmd run .\cmd\verify
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m0.ps1
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
```

环境变量覆盖：`INFERGATE_LISTEN`、`INFERGATE_LOG_LEVEL`、`INFERGATE_MAX_BODY_BYTES`。
请求级控制头：`X-InferGate-Upstream: <name>` 强制指定上游（用于灰度与排障）。
路由顺序：显式头 → 模型精确匹配（大小写不敏感）→ 兜底 `"/"` → 只剩一个后端时吸收任意模型名。

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

## 4. 本机工具链说明（为什么有 `tools/go.cmd`）

这台机器上 `go` 不在 PATH，且有两处硬限制，M0 的构建方式是被它们逼出来的：

1. **模块缓存里的 toolchain 是残缺的**：`D:\goproject\pkg\mod\...\toolchain@v0.0.1-go1.26.8`
   缺少 `src\unsafe` 与 `src\runtime`，连 `package unsafe is not in std` 都会报。
   可用的完整发行版来自模块缓存里的 zip，已解压到仓库内 `.gotoolchain\`。
2. **默认缓存目录被沙箱拒绝写入**：`GOCACHE` / `GOMODCACHE` / `GOTMPDIR` 全部重定向到仓库内。

`tools/go.cmd` 把这两件事封在一处，其余命令统一通过它调用：

```cmd
tools\go.cmd build ./...
```

其它已知限制：`go test -race` 不可用（本机无 gcc，race 需要 cgo），因此并发正确性依靠
"单 writer 结构 + 非 race 测试"论证；HTTPS 只有 Node / Go 的 TLS 栈可用，PowerShell / curl 的
schannel 取不到凭证，所以验收脚本只打本机回环地址。

---

## 5. 里程碑与计划

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M0 | 最小网关：OpenAI 兼容透传 + SSE 流式 + 基础计量与可观测 | **完成** |
| M1 | 多 Provider 路由（成本 / 延迟 / 能力 / 健康度）+ 故障转移 | **完成** |
| M2 | 语义缓存：Embedding + 阈值门控 + Redis | 计划中 |
| M3 | Token 配额与成本治理：预算、超限降级、计量对账 | 计划中 |
| M4 | vLLM 本地推理服务化 + 量化对比（FP16 / AWQ / GPTQ） | 计划中 |
| M5 | 可观测完善 + 压测基线（QPS / P95 / 首字延迟 / 缓存命中率） | 计划中 |
| M6 | 与 Warden 打通：工具调用、会话、成本归因 | 计划中 |

非目标：不做前端控制台、不做计费系统、不做模型训练。

---

## 6. 已验证结论（M0 验收口径）

| 验收项 | 结论 | 证据 |
| --- | --- | --- |
| 单元 / 集成测试 | 全绿 | `go test ./...` exit 0（gateway / sse / miniyaml） |
| 静态检查 | 全绿 | `go vet ./...` exit 0 |
| Go 端到端 | 38/38 断言通过 | `go run ./cmd/verify` |
| curl 端到端 | 47/47 断言通过 | `scripts/verify-m0.ps1` |
| 字节透明 | 响应体与上游逐字节一致 | `TestPassthroughNonStreaming`、verify 断言 |
| 首字延迟可测 | 注入 250ms 停顿，实测首帧 268.0ms | curl `--trace-time` |
| `[DONE]` 补齐 | 上游省略时网关补齐 | `X-Mock-Omit-Done: 1` 断言通过 |
| 成本计量 | 采信 provider usage 并按单价折算 | 日志 `cost_usd=`、`infergate_tokens_total` |
| 字节账目一致 | 日志 `resp_bytes=1718` 等于 curl 落盘 1718 字节 | `tmp\stream.out` 交叉核对 |

---

## 7. 已验证结论（M1 验收口径）

| 验收项 | 结论 | 证据 |
| --- | --- | --- |
| 单元 / 集成测试 | 全绿 | `go test ./...` exit 0（gateway / router / breaker / stats / sse / miniyaml） |
| 静态检查 | 全绿 | `go vet ./...` exit 0 |
| Go 端到端 | 64/64 断言通过 | `go run ./cmd/verify-m1` |
| curl 端到端 | 56/56 断言通过 | `scripts/verify-m1.ps1` |
| 优先级路由 | 无异常时第一名恒为 priority=1 | `TestPriorityIsTheDefaultOrder`、verify-m1 段 3 |
| 成本排序 | 更便宜的后端胜过更优先的贵后端 | `TestCostOrderingBeatsPriority`、`TestFreeBackendWinsOnCost` |
| 延迟排序 | 按窗口实测均值排，没测过的排最后 | `TestLatencyOrderingUsesTheMeasuredWindow`、`TestUnmeasuredBackendSortsLast` |
| 加权策略 | 按 weight 抽样，两个后端都能分到流量 | `TestWeightedStrategyHandsOutBothBackends` |
| 健康度参与排序 | 不健康的后端排到最后但仍然保留（不是直接剔除） | `TestUnhealthyBackendSortsLastButSurvives`、`TestScoreBlendsReliability` |
| 非流式故障转移 | 上游 5xx / 429 换下一个后端，客户端无感 | `TestFailoverOn5xx`、`TestFailoverOn429`、verify-m1 段 4 |
| 流式故障转移 | 首帧之前可以重试，已出帧不重试（不会拼出两份答案） | `TestStreamingFailoverRelaysTheSecondBackend`、verify-m1 段 4 |
| 4xx 不换后端也不透支健康度 | 上游 4xx 原样回传且记为成功 | `TestNoFailoverOn4xx` |
| 超时也走故障转移 | 单次上游超时只算这一次尝试失败 | `TestTimeoutFailsOverWithoutAnswering504ForTheClient` |
| 全部候选失败 | 回传最后一个 Provider 的状态与响应体 | `TestAllCandidatesFailForwardsTheProviderError`、`TestLastCandidateTimeoutAnswers504` |
| 重试预算有上界 | `max_failures_per_request` 封顶尝试次数 | `TestMaxAttemptsCapsTheFailoverBudget`、verify-m1 `checkMaxAttemptsBudget` |
| 熔断状态机 | closed → open → half-open → closed，半开只放 1 个探针 | `TestHalfOpenAdmitsExactlyOneProbe`、`TestFailedProbeReopensImmediately`、`TestHalfOpenProbesCloseTheBreaker` |
| 样本不足不熔断 | 低于 `min_requests` 永不跳闸：未知 ≠ 坏了 | `TestBelowMinRequestsNeverTrips` |
| 熔断不吞请求 | open 的后端排最后而非剔除；全熔断时给出 502 而不是挂住 | `TestBreakerStopsRoutingToADeadBackend`、verify-m1 段 5 |
| 能力路由 | 只送给声明了该能力的后端；无匹配返回 400 而不降级 | `TestCapabilityHeaderExcludesABackend`、`TestUnmatchedCapabilityIs400NotADowngrade` |
| 显式指定 | `X-InferGate-Upstream` 跳过打分，且是唯一候选 | `TestExplicitPinSkipsThePreferredBackend`、`TestExplicitPinWinsAndIsAlone` |
| 模型名改写 | 后端声明了具体模型就送它自己的名字 | `TestBackendReceivesItsOwnModelName`、`TestCatchAllWithConcreteModelPrefersTheConcreteName` |
| 四条可观测面 | 路由表 / 熔断 / 统计 / 指标都可读且带标签 | `/admin/upstreams`、`/admin/breakers`、`/stats`、`/metrics`，verify-m1 段 8 |
| 故障吸收实测 | 杀副本期间 18000 请求 0 错误 | `docs/baseline/m1-load-faulted-r{1,2,3}.json` |
| 熔断收益实测 | 中位延迟 420.42ms → 17.81ms（23.6×） | `docs/baseline/m1-summary.json` 的 `failover` 块 |

M1 的取舍与已知边界（都写在代码注释里，不是事后找补）：

1. **`upstream_timeout` 是"每次尝试"的预算，不是整条请求的共享预算**。共享预算会让慢的第一个候选
   用光整段时间，后面的健康候选拿到一个已经过期的 deadline，于是客户端拿到 504 而网关其实从未
   问过那个快的后端——那正是故障转移要解决的场景。代价是最坏情况 `上游超时 × 尝试次数`，
   两个旋钮都暴露在配置里，由使用方按自己的超时预算权衡。
2. **熔断失败率的分母是整个窗口，成功也计入**。所以短促故障在繁忙窗口里需要更多失败才会跳闸；
   这是"宁可晚跳闸也不要抖动"的选择，想更激进就调小 `window` 或 `min_requests`。
3. **流式重试只在首帧之前**。一旦有字节发给客户端就不能再换后端，否则会把两个后端的输出拼成
   一份看起来正常、实际自相矛盾的答案。
4. **打分与排序不碰 IO**：`internal/router` 只吃传入的候选与窗口快照，不读网络也不改输入，
   所以策略可以单测、决策可以解释（`reason=` 直接进日志）。

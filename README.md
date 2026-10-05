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
│   ├── miniredis/          # 进程内 RESP2 服务（默认 :6399），给共享缓存做本机后端
│   ├── measure-m2/         # 阈值扫描：语料 26 对，输出各阈值下的真命中率 / 误命中率
│   ├── verify/             # M0 Go 端到端验收（38 条断言，CI 门禁）
│   ├── verify-m1/          # M1 Go 端到端验收：路由 / 故障转移 / 熔断（64 条断言）
│   ├── verify-m2/          # M2 Go 端到端验收：语义缓存 / 存储降级 / 管理面（103 条断言）
│   └── verify-m3/          # M3 Go 端到端验收：配额准入 / 降级 / 账目 / fail-open（470 条断言）
├── configs/
│   ├── infergate.yaml      # 生产形态示例（OpenAI / DeepSeek / 本地兜底）
│   ├── mock.yaml           # 本地形态示例（指向 mockupstream，单上游 = M0 路径）
│   ├── routing.yaml        # 生产形态多 Provider 路由（成本/延迟/可靠性权重 + 健康度窗口）
│   ├── routing-local.yaml  # 本地三副本路由（primary/secondary/tools，供 curl 验收与压测）
│   ├── cache-local.yaml    # M2 本地形态：单上游 + 内存缓存（逐行注释的配置说明）
│   ├── cache-redis.yaml    # M2 共享形态：同一套缓存策略换 Redis store
│   ├── quota-local.yaml    # M3 本地形态：四租户四维度预算，计数在进程内存
│   └── quota-redis.yaml    # M3 共享形态：同一套预算换 Redis 计数（多副本唯一正确选择）
├── internal/
│   ├── config/             # 配置加载：YAML -> JSON -> struct，环境变量覆盖，启动即校验
│   ├── miniyaml/           # 手写 YAML 子集解析器（代价与收益见 docs/DESIGN.md）
│   ├── sse/                # SSE 帧解析 / 写出 / 增量观测（usage、tool_call、首字）
│   ├── upstream/           # Provider 注册表：模型 -> 目标，per-target 连接池
│   ├── router/             # 路由决策：候选筛选 + 能力/健康/成本/延迟打分排序
│   ├── breaker/            # 滑动窗口熔断：closed / open / half-open + 半开探针
│   ├── stats/              # 每上游滑动窗口统计（attempts/failures/timeouts/延迟/首字）
│   ├── gateway/            # 代理核心：路由、多次尝试转发、流式中继、错误映射、计费
│   ├── cache/              # 语义缓存：身份/策略/阈值门控 + memory(LRU) 与 Redis 两个 store
│   ├── embed/              # Embedder：离线 hashing（词法）与 HTTP（OpenAI 兼容 /embeddings）
│   ├── redis/              # 手写 RESP2 客户端（连接池、pipeline、超时、统计）
│   ├── mockredis/          # 进程内 RESP2 服务端（测试用，支持 hash / zset）
│   ├── evalset/            # 26 对中英标注语料（13 对同义改写 + 13 对近似但不同）
│   ├── quota/              # M3 配额治理：预扣 + 结算账本、四维度策略、memory / Redis 两个 store
│   ├── metrics/            # 内存指标聚合（请求 / 尝试 / token / 首字 / 流分片 / 缓存 / 熔断 / 配额）
│   ├── mockbackend/        # 进程内假上游（验收程序用，可按后端注入故障与停顿）
│   ├── logging/            # slog 初始化
│   └── server/             # HTTP 服务与运维端点（/healthz /readyz /stats /metrics /admin）
├── scripts/
│   ├── verify-m0.ps1       # M0 curl 端到端验收（47 条断言，真实进程 + 真实 curl）
│   ├── verify-m1.ps1       # M1 curl 端到端验收：优先级/能力/指定/熔断/恢复（56 条断言）
│   ├── verify-m2.ps1       # M2 curl 端到端验收：内存 store 与 Redis store 两条路径（157 条）
│   ├── verify-m3.ps1       # M3 curl 端到端验收：内存与 Redis 计数、降级、fail-closed（323 条）
│   ├── measure-m1.ps1      # M1 实测：路由开销、故障吸收、熔断省下的延迟
│   ├── measure-m2.ps1      # M2 实测：命中率、token/成本节省、命中 vs 未命中延迟
│   └── measure-m3.ps1      # M3 实测：准入开销、预扣准确度、预算挡下的上游调用（57 条断言）
├── tools/go.cmd            # 本机工具链 shim（GOROOT / GOCACHE 重定向，见第 4 节）
└── docs/
    ├── DESIGN.md           # 模块划分、请求生命周期、关键决策与踩坑记录
    ├── RESUME.md           # 每个里程碑对应的简历项目描述（含量化指标占位）
    └── baseline/           # 压测原始数据（m0-baseline.json、m1-*.json、m2-summary.json、m3-summary.json）
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
# 每个里程碑两条路径：Go 端到端 + 真实进程 curl
.\tools\go.cmd run .\cmd\verify                                        # M0，38 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m0.ps1   # M0 curl，47 条
.\tools\go.cmd run .\cmd\verify-m1                                     # M1，64 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m1.ps1   # M1 curl，56 条
.\tools\go.cmd run .\cmd\verify-m2                                     # M2，103 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m2.ps1   # M2 curl，157 条（内存 + Redis）
.\tools\go.cmd run .\cmd\verify-m3                                     # M3，470 条断言
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m3.ps1   # M3 curl，323 条（内存 + 真 Redis 协议）
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
| M2 | 语义缓存：Embedding + 阈值门控 + Redis | **完成** |
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

---

## 8. 已验证结论（M2 验收口径）

| 验收项 | 结论 | 证据 |
| --- | --- | --- |
| 单元 / 集成测试 | 全绿 | `go test ./...` exit 0（cache / gateway / embed / redis / evalset / config 全包含在内） |
| 静态检查 | 全绿 | `go vet ./...` exit 0 |
| Go 端到端 | 103/103 断言通过 | `go run ./cmd/verify-m2` |
| 精确命中 | 同字节重试不再调用上游 | `TestCacheMissThenExactHit`、verify-m2 段 1 |
| 语义命中 | 换一种说法命中（语料实测 0.8819 的真同义对） | `TestCacheSemanticHitAcrossWording`、verify-m2 段 2 |
| 语义不误命中 | 语料里 13 对不该命中的全部不命中 | verify-m2 段 2 逐对检查 `evalset.Cases()` |
| 阈值有据 | 0.86：真命中 8/13、误命中 0/13（0.84 是 3 条误命中） | `go run ./cmd/measure-m2`，见 3.10 |
| 租户与模型隔离 | 不同 tenant / 不同 model 永不互借 | `TestCacheIsScopedToTenantAndModel`、verify-m2 段 3 |
| 采样请求只精确命中 | `temperature > 0` 的重试命中、改写不命中 | `TestCacheSamplingRequestsMatchExactlyOnly`、`internal/cache` `TestNondeterministicRequestsAreNeverSemanticallyMatched` |
| 工具请求只精确命中 | 带 `tools` 的请求只做精确匹配 | `TestCacheToolsAreExactMatchOnly` |
| 答案形状参与身份 | `max_tokens` / 对话前缀不同即不命中 | `TestCacheSignatureBlocksADifferentAnswerShape`、`TestCacheSignatureSeparatesConversationPrefixes` |
| 短提示不缓存 | 低于 `min_prompt_chars` 直接 `skip` | `TestCacheSkipsTrivialPrompts` |
| bypass / refresh | 跳过查询，且**仍然写入**新答案 | `TestCacheBypassSkipsTheLookupButStillStores`、`TestCacheRefreshReplacesTheStoredAnswer` |
| 流式重放 | 逐帧与原流一致（帧数、payload、`[DONE]` 都对） | `TestCacheReplaysStreamingAnswers`、verify-m2 段 6「byte-identical」 |
| 命中不算上游消耗 | 上游 token 计数不变，节省记在 `saved_tokens` | `TestCacheHitIsNotAttributedToAnUpstream`、verify-m2 段 9 |
| 两个 Store 同一套要求 | memory 与 Redis 跑同一个一致性套件 | `TestStoreConformance`（含过期、淘汰、flush、并发） |
| 存储降级 | store 死掉只是不命中，不是报错 | `TestRedisStoreDegradesWhenTheServerGoesAway`、verify-m2 段 8、`/metrics` 仍 200 |
| 只缓存成功的答案 | 上游 4xx 原样回传且不入缓存 | `TestCacheStoresOnlySuccessfulAnswers` |
| 关掉即 M1 | 不命中、不加头、行为与 M1 完全一致 | `TestCacheDisabledLeavesM1Behaviour`、`TestCacheDoesNotAnswerNonCompletionRoutes` |
| 四个管理面 | `/admin/cache`、`/admin/cache/lookup`、`POST /admin/cache/flush`、`/stats` 的 cache 块 | verify-m2 段 9；GET flush 返回 404 |
| 命中率实测 | 请求口径 58.97%（23/39），13/13 对全部覆盖 | `docs/baseline/m2-summary.json` 的 `hit_rate` |
| 零误命中实测 | 13 对近似但不同的语料发 26 次请求，0 次命中 | `docs/baseline/m2-summary.json` `false_hits: 0` |
| 命中延迟实测 | 纯命中 P95 5.5028ms（p50 1.0132） vs 纯未命中 P95 47.3147ms（p50 5.5327） | 同上 `latency.gateway_stats` |
| 命中不产生上游请求 | 上游日志 31 次调用 == 31 次未命中；每轮 3600 请求 = 3598 命中 + 2 未命中 | 同上 `hit_rate.upstream_calls_seen_by_mock`、`latency.rows` |
| 上游挂了仍能服务 | 杀掉上游进程后 26 次请求全部 200（21 精确 + 5 语义），5xx 0；未预热对照请求 502 | 同上 `shield` 块（含 `shield_assertion_passed`） |
| 成本实测 | 每 1000 请求 $0.036692 → $0.016（−56.39%） | 同上 `cost`（按配置价格表外推） |
| 压测无错误 | 缓存开/关各 3 轮 × 8/32 并发，错误数 0 | 同上 `latency.rows[].errors` |

M2 的取舍与已知边界（同样写在代码注释里）：

1. **命中判定必须能解释**：`X-InferGate-Cache` 区分 `hit-exact` 与 `hit-semantic`，
   `/admin/cache/lookup` 给出最近邻与相似度。语义缓存最难的运维问题不是"没命中"，而是
   "命中了错的东西"——这两种情况在响应体里长得一样。
2. **离线 `hashing` embedder 是词法的，天花板偏低**：语料里有 5 对真同义改写的相似度低于
   某个近似对，任何阈值都分不开。要更高的命中率就换成 `provider: http` 的真实 embedding
   模型，并重跑 `cmd/measure-m2`——阈值是配置项，因为它是 embedder 的属性。
3. **`memory` store 是每进程的**：多副本必须用 `redis`，否则两个副本会对同一个问题给出不同
   答案（缓存反而是不一致的来源）。所以 `buildCache` 在 Redis 连不上时直接启动失败，而不是
   静默退回进程内存。
4. **Redis 的淘汰是"按创建时间 FIFO"，不是 LRU**（ZSET 报不出读取顺序）：
   对近期性撒谎的缓存比承认自己按年龄淘汰的缓存更糟。memory store 则是真 LRU。
5. **省略 `temperature` 视为确定性**：这是最常见的情况，不这么判缓存几乎永远不命中；
   想更保守就设 `allow_nondeterministic: false` 并让调用方显式传参。
6. **语义检索是 scope 内的全量扫描**（几百条 512 维向量，几十微秒），不是 ANN 索引；
   `max_entries_per_scope` 就是让这个假设成立的上界。
7. **重放不算生成**：命中时不写 `first_token` 直方图——把接近 0 的重放混进去，会让这个指标
   在缓存用得最狠的时候"变好"。

---

## 9. 已验证结论（M3 验收口径）

| 验收项 | 结论 | 证据 |
| --- | --- | --- |
| 单元 / 集成测试 | 全绿 | `go test ./internal/... -count=1` exit 0（含 `internal/quota`） |
| 静态检查 | 全绿 | `go vet ./...` exit 0 |
| Go 端到端 | 470/470 断言通过 | `go run ./cmd/verify-m3` |
| curl 端到端 | 323/323 断言通过 | `scripts/verify-m3.ps1`（内存 + 真 Redis 协议服务） |
| 日 token 预算 | 第三次 40-token 请求被拒（`tokens_per_day`），计数器停在已准入的量 | `checkDailyTokenBudget`、`TestDailyTokenBudgetRejects` |
| 日成本预算 | 同一套逻辑走微美元账目，拒绝原因为 `cost_per_day_usd` | `checkCostBudget` |
| 每分钟限流 | 第 N+1 次拒绝（`requests_per_minute`），分钟计数不涨 | `checkMinuteRateLimit` |
| 每会话预算 | 会话独立计量，会话缺失时不预扣 | `checkSessionBudget` |
| 降级仍是一次请求 | 200 + `degrade` + 改写模型/上限，预扣保留、结算返还差额 | `checkDegrade`、`TestDegradeKeepsTheReservation` |
| 账本按条目配平 | 混合流量（日 + 会话两个 token 维度、拒绝、缓存命中）下 `reserved − released + overshoot == settled` | verify-m3 `identityOK`、verify-m3-curl 段 11 |
| 拒绝不产生上游调用 | 拒绝时上游调用计数不变（预算挡下的正是 provider 账单） | `checkUpstreamNotCalled`、实测 `provider_calls_prevented: 39` |
| 缓存命中仍占配额 | 命中按 `Usage{Requests: 1}` 结算，token 记 0 | `checkCacheHitSettlesRequestsOnly` |
| fail-closed 是默认 | store 不可读 → 503 `infergate_quota_unavailable`，且 **Redis 连不上时启动失败** | `checkFailClosedOpen`、`TestNewServerFailsWhenQuotaRedisIsDown` |
| fail-open 可显式打开 | 同一次故障下 5/5 放行，`store_errors` 照记，且决策计数记 `allow` | `checkFailClosedOpen`、`docs/baseline/m3-summary.json` 的 `fail_open` |
| 键隔离与单射转义 | 配置里 `acme:inc` 与 `acme_inc` 各记各的账 | `checkKeyIsolation`、`TestKeyLayoutAndBucketFormats` |
| 三个观测面 | `/admin/quota`、`/stats` 的 `quota` 块、`/metrics` 七个 `infergate_quota_*` 族三者同源 | `checkSurfaces` |
| 非生成路径不受治理 | `/v1/models` 不带任何配额头 | `checkUngovernedRoutes` |
| 配置校验 | 负值、重复租户、`degrade` 却没有任何杠杆、`anomaly_ratio < 1` 都在**加载期**报错 | `internal/config` 的配额用例 |
| 预算挡下多少实测 | 40 个请求：无预算 40 次上游调用；`tokens_per_day: 280` 时 1 次（97.5% 被拒） | `docs/baseline/m3-summary.json` 的 `budget` |
| 预扣准确度实测 | 12 请求：reserved 2872 / settled 320 / released 2608 / overshoot 56 tokens，恒等式成立 | 同上 `accuracy` |
| 故障实测 | fail-closed 20/20 503 且上游 0 调用；重启 store 后同进程 10/10 恢复 | 同上 `fail_closed`、`fail_open` |

M3 的取舍与已知边界（同样写在代码注释里）：

1. **日预算是 UTC 天**：`untilDayEnd` 先转 UTC，所以 UTC+8 的机器上本地 08:00 重置。跨时区团队
   要么接受这一点，要么以后加 `timezone` 配置项——沉默地按本地时间算只会让对账更难受。
2. **放行不带 `-Limit`/`-Used`**：网关只在越界那次知道限额（放行要报余量就得额外读一次 store，
   那是给每个请求加一次开销）。要实时余量请查 `/admin/quota`。
3. **预扣是乐观记账，软限额是软的**：估算高估会让额度被"临时占用"，估算低估（长 CJK、短
   `max_tokens`）会穿透到 `overshoot_tokens`。这个计数器就是用来量"限额到底有多软"的。
4. **预扣不是跨维度事务**：`internal/redis` 没有 `MULTI`/`EVAL`（也没有会话概念），所以保证的是
   "每个维度一次原子 `INCRBY`"，不是"四个维度一起成功"；某维度失败时已扣的维度会被释放回滚。
5. **`memory` store 是每进程的**：多副本共用一份预算必须用 `redis`，否则 N 个副本各发一整份；
   所以 Redis 连不上时**启动失败**，而不是静默退化成进程内存。
6. **异常指纹只告警不拦流量**：花钱突然变多通常是真实业务，拦流量的是预算，不是这个比值。
7. **降级的 token 收益未测**：`cmd/mockupstream` 忽略 `max_tokens`，所以只证明了改写发生了
   （后端收到的模型名与上限），没证明省钱——换真 provider 才能量到。

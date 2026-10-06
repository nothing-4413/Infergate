# InferGate —— Agent 推理网关（多模型路由 / 语义缓存 / 成本治理）

[![ci](https://github.com/nothing-4413/Infergate/actions/workflows/ci.yml/badge.svg)](https://github.com/nothing-4413/Infergate/actions/workflows/ci.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

面向 Agent / LLM 应用的**推理网关**：统一多 Provider 接入、智能路由、语义缓存、Token 成本治理、
稳定性治理、本地推理服务化与全链路可观测。它同时是另一个 Agent 项目 Warden 的底层模型接入层。

**M0–M6 七个里程碑全部完成**，每个里程碑都有两条互相独立的验收路径和一份可复现的实测数据：
Go 进程内端到端 **2353** 条断言、真实进程 + 真实 `curl.exe` **1068** 条断言，两条都绿；
`go test ./...` 与 `go vet ./...` 均 exit 0。原始数据见 `docs/baseline/`。

- **透传正确性**：OpenAI 兼容协议 + SSE 逐帧透传，`tool_call` 增量按 `index` 拼回合法 JSON；
  注入 250ms 停顿后实测首帧 **268.0ms**，证明中间没有缓冲（M0）
- **路由与自愈**：五种策略 + 滑动窗口熔断；压测中途杀掉 priority=1 的副本，**18000 个请求 0 错误**，
  熔断生效后 **P50 420.42ms → 17.81ms（23.6×）**（M1）
- **省钱**：语义缓存真同义命中 **58.97%**（23/39），13 对近似语料 **0 误命中**，
  每 1000 请求成本 **−56.39%**（M2）
- **管住成本**：四维度预算（日 token / 日成本 / RPM / 会话）+ 预扣-结算账本，
  `reserved − released + overshoot == settled` 恒等式成立；fail-closed 时 20/20 返回 503 且上游 0 调用（M3）
- **本地推理**：真实 vLLM（RTX 4070 Laptop）上跑 FP16 / AWQ / GPTQ 三方对比 —— AWQ 输出 **100.7 tok/s**，
  是 FP16 的 **2.42×**；tiered 路由把 16 个请求按 **8:8** 分流，省 **34.52%** 标价成本（M4）
- **可观测**：Prometheus + `/stats` + 有界 trace store（OTLP / JSONL），**59400 条 span 0 失败**；
  并量清了代价（c=128 非流式下网关是瓶颈，比直连少 22.07% QPS）（M5）
- **Agent 友好**：幂等重放不重复计费（重放期间 provider 调用 **0** 次）、会话成本账本、能力发现；
  Warden 端到端 **26/26** 检查通过（M6）

## 30 秒：它是什么、怎么自己看一遍

没有依赖、没有 `go.sum`、没有外部服务——**一个 Go 进程 + 一个假上游**就是一套完整实验台。
下面的命令可以原样粘贴（Windows PowerShell；`go` 走仓库里的 shim，原因见第 4 节）：

```powershell
.\tools\go.cmd test ./...                       # 全绿：约 90 个 Go 文件、无第三方依赖
.\tools\go.cmd run .\cmd\verify-m6              # 381 条进程内端到端断言，退出码 0 就是过
.\tools\go.cmd run .\cmd\infergate -config .\configs\mock.yaml -check   # 只校验配置
```

想要"真进程 + 真 socket"那一侧，是第 3.2 节的两条启动命令加第 3.3 节的一条验收命令（149 条 `curl.exe` 断言）；
不想开三个终端就 `docker compose up -d --build`，见第 3.5 节。

三条路径的细节在[第 3 节](#3-快速开始)，量化结论在第 5 节，**没做到的事**写在第 10 节。

用法见 [docs/USAGE.md](docs/USAGE.md)，验收口径见 [docs/ACCEPTANCE.md](docs/ACCEPTANCE.md)，
设计决策见 [docs/DESIGN.md](docs/DESIGN.md)，量化结论见 [docs/RESUME.md](docs/RESUME.md)。

---

## 1. 能力总览

| 能力 | 实现要点 | 验收证据 |
| --- | --- | --- |
| OpenAI 兼容透传 | 反向代理语义：入站 path 原样拼到 `base_url` 后；请求体先解析成 `map[string]json.RawMessage`，避免大整数/浮点被重新格式化 | `cmd/verify` 逐字节比对 |
| SSE 流式 | 自研 SSE 帧解析器（`internal/sse`），保留上游原始字节并逐帧 `Flush`；上游漏发 `[DONE]` 由网关补齐 | `X-Mock-Omit-Done` 故障注入 |
| `tool_call` 增量 | 按 `choices[].delta.tool_calls[].index` 累积 `arguments` 分片 | 3 个分片拼成合法 JSON |
| 计量与成本 | 只采信上游返回的 `usage`（含 `cached_tokens`），按配置单价折算 USD | `/metrics`、日志 `cost_usd=` |
| 多 Provider 路由 | priority / cost / latency / weighted / tiered 五种策略，候选筛选后按权重打分排序 | `cmd/verify-m1`、`cmd/verify-m4` |
| 故障转移 | 单请求最多 N 次尝试，可重试状态码（429/5xx）换下一个候选，4xx 原样透传 | 杀副本压测 18000 请求 0 错误 |
| 熔断与健康度 | 滑动窗口（`window` / `buckets` / `min_requests` / `failure_ratio`）+ half-open 探针 + 管理面重置 | 中位延迟 420.42 → 17.81ms |
| 语义缓存 | 长度门 + 相似度阈值门控；memory(LRU) 与 Redis 两个 store；命中直接回放存储的字节 | `scripts/verify-m2.ps1` 两条路径 |
| 配额与成本治理 | 四维度预算，预扣-结算账本，超限 503 / 可配置降级；memory 与 Redis 两种计数 | 账目恒等式 + fail-closed 20/20 |
| 本地推理分层 | 真实 vLLM 作为本地层，云层并存；按 prompt 长度与能力分流 | `scripts/measure-m4.ps1` |
| 可观测 | 结构化日志 + `/stats` + Prometheus 指标 + 有界 trace store（OTLP / JSONL 两个 exporter） | `cmd/verify-m5`、`measure-m5` |
| Agent 友好面 | `Idempotency-Key` 指纹（409 in-flight / conflict）、`X-InferGate-Session` 会话账本、`/v1/capabilities` | `cmd/verify-m6` + Warden E2E 26/26 |

---

## 2. 架构与请求生命周期

```
客户端 ─▶ 准入(配额) ─▶ 幂等/会话账本 ─▶ 语义缓存 ─▶ 路由(能力/健康/成本/延迟) ─▶ 上游尝试 ×N ─▶ SSE 中继
                                     └──────── 观测：日志 / metrics / stats / trace ────────┘
```

1. **入站校验**：path 与 body 上限（`max_body_bytes`），模型不可解析直接 400。
2. **准入**：配额与预算预扣；超限按配置 503（fail-closed）或降级到备用模型。
3. **幂等与会话**：带 `Idempotency-Key` 的完成类请求命中已存结果就回放（完全不碰上游），
   否则登记 claim、响应落库；带 `X-InferGate-Session` 的请求同时记进会话账本。
4. **缓存**：先判"这个请求是否可缓存"，命中直接回放，未命中才走上游。
5. **路由**：候选筛选（模型 / 能力 / 健康）后按策略打分，逐个尝试，失败换下一个。
6. **转发与中继**：单 writer 结构逐帧 `Flush`，同时观测 token / 首字 / 分片 / 成本。

**四条关键取舍**（完整清单与踩坑记录见 [docs/DESIGN.md](docs/DESIGN.md)）：

- **零第三方依赖**：只用标准库。`internal/miniyaml` 是手写的 YAML 子集解析器，把 JSON tag
  当成配置 schema 的唯一真相来源（理由见第 4 节）。
- **不缓冲上游响应**：`http.Server` 故意不设 `WriteTimeout`，否则长回答会被静默截断；
  真正的边界是 `read_header_timeout` + 单次 `upstream_timeout` + `idle_timeout`。
- **出站 context 不继承 `r.Context()`**：handler 返回会取消入站 context，长流会被连带掐断；
  出站从 `context.Background()` 派生，另起 goroutine 监听客户端断开。
- **错误分类即责任划分**：模型不可解析 = 400（调用方）、上游不可达 = 502、
  上游超时 = 504、客户端断开 = 499（nginx 约定，不写响应体）。

---

## 3. 快速开始

### 3.1 编译与测试

```powershell
# 本机 go 不在 PATH 上，Go 命令统一通过 .\tools\go.cmd（原因见第 4 节）
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

# 终端 3：打一发（PowerShell 5.1 会吃掉原生 exe 参数里的引号，请求体一律写文件）
'{"model":"mock-gpt","messages":[{"role":"user","content":"hello"}]}' |
  Set-Content -NoNewline -Encoding ascii .\tmp\chat.json
curl.exe -s -X POST "http://127.0.0.1:8080/v1/chat/completions" `
  -H 'Content-Type: application/json' --data-binary "@.\tmp\chat.json"
```

mock 的常用开关：`-token-delay 0` 关掉每 token 的 15ms 间隔（**压测时必须关**，否则测到的是 mock
而不是网关），`-ttfb` 控制首帧前的停顿；故障注入用请求头 `X-Mock-Status` / `X-Mock-Delay` /
`X-Mock-TTFB` / `X-Mock-Omit-Done`。这几个头的值**读不懂就是 400**（点名是哪个头，例如
`X-Mock-Delay must be a duration such as "250ms": "300"`），不会被当成"没有延迟"——一个被悄悄忽略的
注入头会让门禁对着一场从未发生的场景报绿。

> **端口提示**：本机 `127.0.0.1:8080` 被其它进程占用时，网关只能绑到 `[::]:8080`，
> Windows 会把 `127.0.0.1` 的请求交给那个更具体的绑定，于是 `/healthz` 莫名其妙返回别人的 404。
> 验收脚本默认用 `18080` 就是为了绕开这一点；手动实验请用 `localhost` 而不是 `127.0.0.1`。

### 3.3 一键验收

两条路径相互独立：Go 门用进程内假上游，跑得快、断言密度高；curl 门编译真二进制、拉真进程、
用真 `curl.exe` 打真 socket，证的是"部署起来就是这样"。7 个里程碑两条都绿：

| 里程碑 | Go 门（`.\tools\go.cmd run .\cmd\verify-mN`） | curl 门（`.\scripts\verify-mN.ps1`） |
| --- | --- | --- |
| M0 透传与 SSE | `cmd/verify` — 38 条 | 47 条 |
| M1 路由与熔断 | `cmd/verify-m1` — 64 条 | 56 条 |
| M2 语义缓存 | `cmd/verify-m2` — 103 条 | 157 条 |
| M3 配额治理 | `cmd/verify-m3` — 470 条 | 323 条 |
| M4 分层与量化 | `cmd/verify-m4` — 877 条 | 125 条 |
| M5 可观测与压测 | `cmd/verify-m5` — 420 条 | 211 条 |
| M6 幂等/账本/能力 | `cmd/verify-m6` — 381 条 | 149 条 |
| **合计** | **2353 条** | **1068 条** |

`合计` 行不是手抄的：`internal/repofmt` 的 `TestDocumentedGateTotalsAreTheSumOfTheirRows` 会把这张表
与 [docs/ACCEPTANCE.md](docs/ACCEPTANCE.md) 的门禁总表逐行对照、并核对每一句重述总数的句子——M5 那
8 条解析器自检一度只加到了行上、没加到合计里，就是它抓出来的。

```powershell
# Go 门（任意一个）
.\tools\go.cmd run .\cmd\verify-m6

# curl 门（本机没有 pwsh，必须用 Windows PowerShell；脚本自己编译、起栈、收尾）
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-m6.ps1
```

Makefile 里有等价封装（`make verify-m6` / `make verify-m6-curl` / `make measure-m6` /
`make run-agent` 等，`.PHONY` 与 `make help` 同步维护）。逐里程碑的完整流程、故障注入手法与
"验收 + 实测"段落见 [docs/USAGE.md](docs/USAGE.md)。

### 3.4 压测与实测脚本

```powershell
# 自带的压测工具：直连 vs 经网关 × 流式/非流式 × 多并发
.\tools\go.cmd run .\cmd\loadtest -url http://127.0.0.1:9000/v1/chat/completions -c 32 -n 3000 -warmup 300

# 每个里程碑的实测脚本（产出 docs/baseline/*.json）
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\measure-m5.ps1 -Concurrency 8,32,128 -Requests 3000 -Warmup 300 -Rounds 3 -Timeout 30s
```

### 3.5 容器形态（一条命令起整套）

不想开三个终端就用 compose：网关 + mock 上游 + `cmd/miniredis`（共享缓存/配额的后端）三容器，
`--profile obs` 再加 Prometheus + Grafana。

```powershell
docker compose up -d --build                 # 网关 :18080、mock :19000、RESP2 :16399
docker compose --profile obs up -d --build   # 再加 Prometheus :19090、Grafana :13000（匿名 Viewer）
docker compose down
```

容器里用的是 `configs/docker.yaml`：上游写服务名 `http://mockupstream:9000`、存储写 `miniredis:6399`
（写 `127.0.0.1` 会指到网关自己）。端口特意都不是默认值，可用 `INFERGATE_GATEWAY_PORT` 等环境变量覆盖。
**这一节只做到了"配置正确"这一层**：compose 语法与 `configs/docker.yaml` 都用真实加载器验过，
`scripts/verify-docker-profile.ps1` 把同一份配置换成本地地址、用真进程跑了 20 项检查全过；
但**镜像从未被构建过**（写这份文档的机器上 Docker 守护进程没运行），明细见
[deploy/README.md](deploy/README.md) §7.1。

---

## 4. 本机工具链说明（为什么有 `tools/go.cmd`）

这台机器上 `go` 不在 PATH，且有两处硬限制，构建方式是被它们逼出来的：

1. **模块缓存里的 toolchain 是残缺的**：`toolchain@v0.0.1-go1.26.8` 缺 `src\unsafe` 与 `src\runtime`，
   连 `package unsafe is not in std` 都会报。可用的完整发行版来自模块缓存里的 zip，已解压到仓库内 `.gotoolchain\`。
2. **默认缓存目录被沙箱拒绝写入**：`GOCACHE` / `GOMODCACHE` / `GOTMPDIR` 全部重定向到仓库内。

`tools/go.cmd` 把这两件事封在一处，其余命令统一通过它调用（例如 `tools\go.cmd build ./...`）。

其它已知限制：HTTPS 只有 Node / Go 的 TLS 栈可用，PowerShell / curl 的 schannel 取不到凭证，
所以验收脚本只打本机回环地址。

`go test -race` 需要 cgo，而 `gcc` 不在 PATH 上，所以早期版本的这里写着"本机跑不了"。实际上这台
机器有编译器——`C:\msys64\ucrt64\bin\gcc.exe`（15.2.0）与 Visual Studio 2022 的 MSVC 14.44——
只要显式指路就能跑。配方已经写进脚本，逐包跑（与 CI 同形，一次报告全部被拒的包）：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\run-race.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\run-race.ps1 -Package ./internal/cache
```

它做的三件事就是原来那段手抄命令的三行：把 `C:\msys64\ucrt64\bin` 加进 `PATH`、设 `CC=gcc` 与
`CGO_ENABLED=1`、把 `TMP`/`TEMP` 指向仓库内的 `.gotmp`（**这一条是关键**：否则 cgo 把中间文件写进
`%LOCALAPPDATA%\Temp`，报 `cgo: open ...cgo-gcc-input-...: Access is denied.`）。没有编译器时以
退出码 2 明确拒绝，而不是留下一个看不懂的链接错误。

这一步立刻就抓到了一条 CI 一直红着的真实竞态（`internal/cache/redis.go` 的 `RedisStore.stats`，
详见 [docs/ACCEPTANCE.md](docs/ACCEPTANCE.md)）。修完逐包跑完全树是 **36 个包全绿、0 条 `DATA RACE`
行、218 秒**。

---

## 5. 实测数据

一台笔记本（i9-14900HX / RTX 4070 Laptop 8GiB / Windows 11）单机跑出来的数字，
每个里程碑一行，完整表格、区间与口径见 [docs/USAGE.md](docs/USAGE.md)：

| 里程碑 | 关键数字 | 原始数据 |
| --- | --- | --- |
| M0 | 直连 c=8 8393 QPS → 经网关 5026 QPS；注入 250ms 停顿实测首帧 268.0ms；`resp_bytes=1718` 与 curl 落盘逐字节一致 | `baseline/m0-baseline.json` |
| M1 | 杀副本期间 18000 请求 0 错误；熔断生效后 P50 420.42 → 17.81ms（23.6×）；路由开销落在 40% 轮间抖动内 | `baseline/m1-summary.json` |
| M2 | 真同义命中 58.97%（23/39）、0 误命中；命中 P95 5.50ms vs 未命中 47.31ms；每 1000 请求 $0.036692 → $0.016（−56.39%） | `baseline/m2-summary.json` |
| M3 | 预扣 2872 / 结算 320 / 释放 2608 / 超发 56，恒等式成立；fail-closed 20/20 503 且上游 0 调用 | `baseline/m3-summary.json` |
| M4 | 真实 vLLM 三量化：AWQ 28.1ms TTFT / 100.7 tok/s（FP16 的 2.42×），GPTQ 28.7ms / 95.3 tok/s；分层 16 请求 8:8，省 34.52% 标价成本 | `baseline/m4-summary.json` |
| M5 | 直连 c=128 5622 QPS vs 经网关 4382（−22.07%，网关是瓶颈）；tracing −12.09%~−19.60%；59400 条 span 0 失败 | `baseline/m5-summary.json`（+ 24 条逐轮文件） |
| M6 | 同 key 重放 0 次 provider 调用、省 11 token/轮（端到端）；16 并发同 key → 1 次生成 + 15 次 409；c=32 非流式 P95 +11.5% | `baseline/m6-summary.json` |

**这些是测量，不是容量承诺**：绝对 QPS 是这台 mock、这台主机和这个客户端的属性，
所有结论都以"本机实测 + 轮间噪声带"为前提写明。M4/M5/M6 的边界条件（stand-in 云层、
单次运行、共享 GPU、噪声带）逐条列在 `baseline/*.json` 的 `limitations` 里。

---

## 6. 里程碑与计划

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M0 | 最小网关：OpenAI 兼容透传 + SSE 流式 + 基础计量与可观测 | **完成** |
| M1 | 多 Provider 路由（成本 / 延迟 / 能力 / 健康度）+ 故障转移 | **完成** |
| M2 | 语义缓存：Embedding + 阈值门控 + Redis | **完成** |
| M3 | Token 配额与成本治理：预算、超限降级、计量对账 | **完成** |
| M4 | vLLM 本地推理服务化 + 量化对比（FP16 / AWQ / GPTQ）+ 分层路由 | **完成** |
| M5 | 可观测完善 + 压测基线（QPS / P95 / 首字延迟 / 缓存命中率 / trace 回放） | **完成** |
| M6 | 与 Warden 打通：幂等重放（不重复计费）+ 会话账本（成本归因）+ 能力发现 | **完成** |

非目标：不做前端控制台、不做计费系统、不做模型训练。

---

## 7. 目录结构

```
infergate/
├── cmd/
│   ├── infergate/          # 主程序：加载配置、起服务、优雅退出
│   ├── mockupstream/       # OpenAI 兼容假上游（故障注入 + M6 脚本化应答）
│   ├── loadtest/           # 压测与基线：直连 vs 经网关 × 流式/非流式 × 多并发
│   ├── miniredis/          # 进程内 RESP2 服务（默认 :6399），共享缓存/配额的本机后端
│   ├── mockcollector/      # 假 OTLP collector（M5 验证导出路径）
│   ├── measure-m2/         # 缓存阈值扫描：26 对语料 → 各阈值下的真/误命中率
│   └── verify, verify-m1..m6/   # 7 个 Go 端到端验收程序（38 ~ 877 条断言）
├── configs/                # 14 份逐行注释的示例配置（mock / routing / cache / quota / tiered / agent / docker …）
├── internal/
│   ├── config/ miniyaml/ logging/     # 配置加载（YAML→JSON→struct）、手写 YAML 子集、slog
│   ├── sse/                            # SSE 帧解析 / 写出 / 增量观测（usage、tool_call、首字）
│   ├── upstream/ router/ breaker/ stats/   # Provider 注册表、候选打分、滑动窗口熔断与统计
│   ├── gateway/                        # 代理核心：路由、多次尝试、流式中继、错误映射、计费
│   ├── cache/ embed/ evalset/          # 语义缓存（阈值门控 + LRU/Redis）、Embedder、标注语料
│   ├── quota/                          # 预扣-结算账本与四维度策略（memory / Redis 两个 store）
│   ├── redis/ mockredis/               # 手写 RESP2 客户端 / 进程内 RESP2 服务端
│   ├── idempotency/ sessions/          # 幂等存储（LRU + claim）与会话成本账本
│   ├── tracing/ traceexport/           # 有界 trace store + OTLP / JSONL 两个 exporter
│   ├── metrics/ mockbackend/           # 内存指标聚合、进程内假上游（验收用）
│   └── server/                         # HTTP 服务与运维端点（/healthz /readyz /stats /metrics /admin）
├── scripts/                # 7 个 curl 端到端验收（verify-m0..m6.ps1）+ 6 个实测脚本 + vLLM 量化对比
├── docs/                   # DESIGN / USAGE / ACCEPTANCE / RESUME + baseline/（原始测量数据）
├── deploy/                 # Prometheus + Grafana 配置（本机形态与容器形态各一份，见 deploy/README.md）
├── .github/workflows/      # ci.yml：Linux 门（build / vet / test / race）
├── Dockerfile              # 多阶段构建，--build-arg CMD= 选编译哪个 cmd/
├── docker-compose.yml      # 网关 + mock 上游 + miniredis（+ obs profile）
└── tools/go.cmd            # 本机工具链 shim（GOROOT / GOCACHE 重定向，见第 4 节）
```

---

## 8. 配置说明

一份最小配置（`configs/mock.yaml` 的节选）：入站 path 会原样拼到 `base_url` 之后，
所以 `base_url: http://127.0.0.1:9000` + 调用方 `/v1/chat/completions` 就是完整的上游 URL。

```yaml
server:
  listen: ":8080"
  upstream_timeout: "10m"        # 单次上游交换（含流式 body）的上限；时间是带引号的字符串
  max_idle_conns_per_host: 256   # 吞吐第一旋钮：Go 默认 2 会让并发退化成每请求新建连接
upstreams:
  - name: "mock"
    kind: "openai"
    base_url: "http://127.0.0.1:9000"
    api_key: ""                  # 空 = 转发调用方的 Authorization，本地后端无需改配置
    models: ["/"]                # "/" 是 catch-all
pricing:
  default: { in: 1.0, out: 3.0 } # USD / 1M tokens，只用于给每个请求挂上成本数字
```

几个值得单独说的旋钮：

| 旋钮 | 作用 | 注意 |
| --- | --- | --- |
| `server.upstream_timeout` | 单次**尝试**的预算 | 最坏情况是 `upstream_timeout × health.max_failures_per_request` |
| `health.max_failures_per_request` | 一次请求最多尝试几个上游 | 直接决定故障转移的延迟上界 |
| `routing.strategy` | `priority` / `cost` / `latency` / `weighted` / `tiered` | 权重配置在 `routing.weights` |
| `cache.embedding.*` / `cache.threshold` | 缓存的身份与命中阈值 | 阈值扫描结果见 `docs/USAGE.md` 3.10 |
| `quota.*` | 四维度预算与降级策略 | 计数在内存还是 Redis 决定多副本正确性 |
| `access.*` | 运营者令牌：谁可以读 / 改管理面 | 默认关闭；绑到可路由地址就该打开，见 8.1 |

每个配置块的理由都写在示例文件的注释里；逐块讲解见 [docs/USAGE.md](docs/USAGE.md) 的 3.6。

### 8.1 管理面鉴权（`access`）

`/admin/*` 能刷掉配额计数、清空缓存、让已重放的答案失效、读到每个租户的花费，`/stats`
是同一份账目的汇总。它们**不在**任何 provider 的 API key 后面：那些 key 是网关**发出去**的
凭证，这些端点是网关**回答**的。默认不鉴权，这是 M0–M6 的既有行为（所有验收脚本都以无凭证
访问 `/admin/*` 为前提），在 loopback 上没问题；一旦 `server.listen` 绑到别人能连上的地址，
不加这一节就等于把每个租户的花费和一个清缓存的按钮开放给任何能建立 TCP 连接的人。启动时
日志会明确说一次是哪一种情况（有令牌则 Info 报出受保护前缀与令牌数量，没有则 Warn 明说
管理面是开放的）。

```yaml
access:
  enabled: true
  tokens: ["${INFERGATE_ADMIN_TOKEN}"]  # 任一命中即可；多个令牌 = 轮换而不是硬切
  header: ""            # 空 = Authorization，按 RFC 7235 的 Bearer 方案（scheme 大小写不敏感）
  protect: []           # 空 = ["/admin", "/stats"]；显式给出则替换默认，不是追加
  allow_query_token: false   # 也可用 ?access_token=…，默认关：URL 会进访问日志和浏览器历史
```

几条刻意的取舍：

- **`/healthz`、`/readyz`、`/metrics` 不在默认保护集里。** 探针和抓取器不带凭证；给它们加
  令牌就是把一个正常的部署变成永远不健康。要关就显式写进 `protect`（`/metrics` 常这么干）。
- **少带令牌和带错令牌都是 401，响应体也一样。** 区分 403 会告诉探测者"这个端点存在、只是
  格式不对"。`WWW-Authenticate: Bearer realm="infergate"` 只在凭证本身是 Bearer 形态时才发。
- **写错的配置会让启动失败，而不是静默失效。** 两层兜：① 键名写错（`quota:` 写成 `quotas:`）由加载器的
  严格解码在加载期点名拒绝（`miniyaml: decode: json: unknown field "quotas"`，退出码 1）；
  ② `enabled: true` 却没有令牌、或 `protect: ["/"]`（会把 OpenAI 兼容面一起挡住）由 `Validate` 拒绝。
  第 ① 层是后来才有的——在它之前，键名拼错是这个项目里唯一没有任何断言的失败形态。
- **令牌只从 `${ENV}` 读也行，未定义的变量会让启动中止**，和 upstream 的 `api_key` 同一条规则：
  静默变成空串的令牌会让"配置看起来受保护"而实际开放。注意这条检查在 `enabled: false` 时
  也会跑，所以 `configs/agent.yaml` 里那段 `tokens` 是注释掉的。
- **`access.protect` 按路径段匹配**：`/admin` 覆盖 `/admin/cache`，但不覆盖 `/administrator`。

---

## 9. 文档索引

| 文档 | 内容 |
| --- | --- |
| [README.md](README.md) | 你正在看的：定位、能力、快速开始、实测摘要 |
| [docs/USAGE.md](docs/USAGE.md) | 完整使用手册：逐里程碑启动方式、可复制的 curl 命令、故障注入、验收与实测段落 |
| [docs/ACCEPTANCE.md](docs/ACCEPTANCE.md) | 验收口径：门禁总表 + M0–M3 逐项证据 |
| [docs/DESIGN.md](docs/DESIGN.md) | 设计：模块划分、请求生命周期、关键决策与踩坑记录（§1–§13） |
| [docs/RESUME.md](docs/RESUME.md) | 每个里程碑对应的简历描述与量化结论 |
| [docs/baseline/](docs/baseline/) | 压测与实测的原始 JSON（含每条限制条件的 `limitations`） |

---

## 10. 已知限制与取舍

- **CI 的 race 步曾经一直红，根因已在本机复现并修掉**：`internal/cache/redis.go` 的 `RedisStore.stats`
  是裸字段，29 处写入点分布在每个请求 goroutine 上，而 `/admin/cache` 会并发读它（§4 有本机跑
  `-race` 的配方）。修复提交是 `ec83c07`，而它**已被 runner 判定为绿**：修好 `ci.yml` 之后第一个真正
  执行的 run（37408647484，head `29ed38c`）两个 job 全 `success`，其中 `go test -race` 那一步也是。
  设计里原本给 race 失败准备了两条读法，**两条都被实测关掉了**：`$GITHUB_STEP_SUMMARY` 不进
  check-runs 的 `output.summary`；`PATCH` 本 job 自己的 check run 返回 2xx 但 job 一结束字段即被清空
  （run 37377940124 测定）。唯一活下来的是 `scripts/ci-publish-failure-check.sh` 用
  `POST /check-runs` 建一个**自己的** check run——"创建"与"修改"是两回事，自己创建的不会被回收
  （run 37379724217 的探测证实）；它只在失败时执行，**而它已经红过一次真的**：run 37414903323（head `97dade6`）
  的 `build / vet / test / race` job 在 `go test` 步失败（`TestDropClosesWithoutAResponse`），
  匿名 `GET /commits/97dade6/check-runs` 里就多出一条名为 `ci failure: build / vet / test / race` 的
  check run，`output.summary` 是那份失败摘要。同一批还修了它读不出的两件事：摘要当时只保留
  `FAIL` 行、把 go test 缩进打印的原因行过滤掉了（现在留 `file.go:NNN:` 形状的行），以及那个测试
  读 `/calls` 计数器是无序读（计数器在 `defer` 里、hijack 的 socket 关掉之后才更新，现在改成 2 秒内轮询）。
  修完后的 run 37416449896（head `3716f18`）与 37416672028（head `b45e601`）两个 job 全 `success`，
  含 Linux 上 `go test` / `go test -race` 两步与 `curl gates` 的十四个步骤。
- **从 `760c303` 起的六个提交其实什么都没跑，原因不是 GitHub，是 `ci.yml` 本身不是合法 YAML**：
  那个提交把一个含"冒号加空格"的标题直接写在了 `run:` 的裸标量里
  （`run: bash ... "ci failure: build / vet / test / race" ...`），YAML 裸标量不允许这样写，
  于是整份文件解析失败（`go-yaml load error in scanner at L185.C66: mapping values are not allowed
  in this context`，用 `docker compose -f .github/workflows/ci.yml config` 复现）。
  **GitHub 从解析不了的文件里起不了 job**：run 是零 job 的 `completed/failure`，显示名退回文件路径
  （`.github/workflows/ci.yml` 而不是 `ci`），`updated_at` 等于 `created_at`，`jobs` 与 `check-runs`
  自然 `total_count: 0`——`760c303` 到 `03655b2` 之间的每一次 push 都是如此，Linux 门、八个 Windows
  curl 门、check-run relay 一个都没执行过。此前这里写的是"GitHub 侧的一次异常"：那些观察（`total_count: 0`、
  `name` 变路径、run 页面被重定向到 commit 页）都是真的，解释是错的。修法是把标题挪进 `env:` 变量，
  并加了 `internal/repofmt/workflow_yaml_test.go` 把这个规则本身钉住。修好之后的第一个 run
  （37408647484，head `29ed38c`）`name` 变回 `ci`、`/jobs` 返回 2 个 job、两个都绿，`docs/ACCEPTANCE.md`
  的「自动化的那一层」一节有完整取证与判据。
  - job 日志（`GET /actions/jobs/{id}/logs`）与 artifact 下载对匿名读者都是 **403**
    （`{"message":"Must have admin rights to Repository.", "status":403}`）。
- **九个门步骤都带 `continue-on-error: true`，所以它们自己从来不能让 job 变红**：job 里全是允许失败的
  步骤，等于整条 Windows 验收链（1068 条 curl 断言 + 2353 条进程内 Go 断言）即使全红，job、run、徽章、
  匿名 jobs API 也都会报 `success`。GitHub 的定义就是针对这种情况写的（contexts 参考，
  `steps.<step_id>.conclusion`）：
  *"When a `continue-on-error` step fails, the `outcome` is `failure`, but the final `conclusion` is
  `success`."* 这个 flag 故意留着，因为它换来"一次红 run 报出全部九个门"；代价由新增的
  `every gate must have passed` 步骤付掉——它是 job 里**唯一**不允许失败的一步，跑
  `scripts/lib/summarize-gates.ps1 -Gates m0,…,hardening,go-verify`，读 `run-gate.ps1` 与
  `run-go-verify.ps1` 为每个门写的 `tmp\gate-<name>.exit` marker，
  把"marker 缺失 / 内容不可解析 / 非 0"三种都判成红，并把失败门与日志尾部写进
  `tmp\gate-failures.md`（随 artifact 上传）再 `exit 1`。钉住它的是 `internal/repofmt/curl_gates_test.go`：
  `TestEveryCurlGateCanFailTheJob` 断言"每个门都带 flag、恰好一个 checker 不带、两份门名单完全一致、
  Go 门列全七个包"，`TestSummarizeGatesFailsClosed` 真跑脚本核对五种情形下的退出码与摘要内容。这条步骤的
  绿路径已在 runner 上走过两次：run 37410701867（head `e248c7a`，当时还是八个门）第 12 步
  `every gate must have passed` = success；run 37413713212（head `6ec3554`）第 13 步同样的结论，
  而它前面**九个**门步骤全部 success——那是第九个门
  （`go verify gates (M0-M6)`，把 `.\tools\go.cmd run .\cmd\verify*` 的 2353 条断言接进同一条链）
  第一次在 runner 上跑，所以它现在**有** runner 证据，而且是它把 marker 凑齐到九个的那一次。
  红路径只有本机证据——在 runner 上制造一次红门会让那次 run 的其余结论一起作废（本机：七包 21.3 秒全过、
  marker 写 0，两个失败路径"包不存在 / 包名为空"都验过是红）。这一批同时修掉了这个门里长期未解释的 M5
  偶发：`6.29`（根 span 的派生时长必须为正）在本机 20 次连跑里红 9 次（它在 runner 上没有先例，因为进程内
  验证器是这一批才第一次进 CI；run 37371614002 红过的那一步是 **curl** 的 M5 门，那门对时长只要求
  `-ge 0`），根因是本机 `time.Now()` 只有约 0.5–1.6ms 的步进，mock 在一个 tick 内答完则跨度只能量成 0；
  修法是给 mock 加 20ms think time（`cmd/verify-m5/harness.go` 的 `mockThinkTime`，断言一条没改），
  改后 20/20 绿。
- **测量不是容量承诺**：绝对 QPS 依赖这台主机、这个 mock 和这个客户端；带轮间噪声带的结论才算结论。
- **云层在 M4 里是 stand-in**：分层路由的跨层延迟差是"本地真模型 + 本仓 mock"的差，不是与真实云 API 的对比。
- **量化对比是 drift 不是精度**：AWQ/GPTQ 与 FP16 的输出差异以文本漂移度衡量，没有人工或自动评分。
- **成本是折算不是账单**：没有 cost 指标，所有 USD 都由配置价格表 + 响应 `usage` 推导。
- **prompt token 是估算**（长度 / 4），因此 M4 的本地/云分界是网关的估算，不是真分词结果。
- **缓存语义检索是线性扫描**：命中判断的代价随条目数增长，规模化需要换成向量索引（当前规模下没量出问题）。
- **配额计数在内存时只对单副本正确**：多副本必须换 Redis store。
- **M3 的"日"预算是 UTC 日**：对本地运维的日历而言会在早上 8 点（UTC+8）重置。
- **M6 的幂等存储有容量上限**：LRU 淘汰后同 key 重放会重新打到上游（安全但不再省调用）。
- **Warden 仓库的 embeddings 调用不带网关头**（该仓库的 `app/memory/embeddings.py` 未接入），这条路径没有成本归因。
- **M6 的 tracing 属性只记标量**：不采样请求/响应体，所以回放看到的是决策链而不是内容。
- **`/admin/*` 与 `/stats` 默认不鉴权**：`access` 这一节提供了运营者令牌，但默认关闭（打开它会让本仓库
  全部 curl 验收脚本的凭证假设失效）。这意味着把网关绑到 0.0.0.0 而不加 `access.tokens`，等于把
  每个租户的花费和一个清缓存的按钮开放给任何能连上这个端口的人；启动日志会 Warn 提醒一次。
  鉴权本身有两条链：Go 进程内 50 个检查点（`internal/server/access_test.go` 40 个 +
  `internal/config/config_test.go` 的两个 access 用例 10 个），加 `scripts/verify-hardening.ps1`
  （真进程 + 真 curl，44 条，已接进 CI 的 curl 门并跑绿）。详见 `docs/ACCEPTANCE.md` 的
  「管理面鉴权的证据边界」。

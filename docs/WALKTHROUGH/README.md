# InferGate 源码走读分册（WALKTHROUGH）

本目录是对 `github.com/infergate/infergate` **全部 36 个构建单元**（24 个 `internal/` 包 + 12 个 `cmd/` 工具）的逐文件、逐符号中文走读。它不是教程，也不是设计文档：每个函数、方法、类型、常量都单独成条，记它的作用、读写哪些状态、错误路径、并发保护和被谁调用；源码注释里解释"为什么"的句子照译保留。

## 与仓库既有文档的分工

| 文档 | 回答什么问题 | 读法 |
| --- | --- | --- |
| `docs/DESIGN.md` | 为什么这样设计？有哪些关键决策与踩过的坑 | 先读结论，再回源码 |
| `docs/USAGE.md` | 怎么把它跑起来？每个里程碑的启动与验证命令 | 动手时查 |
| `docs/ACCEPTANCE.md` | 门槛怎么算过关？每条断言对应哪段代码 | 验收时查 |
| `docs/RESUME.md` | 每个里程碑做完后留下了什么结论与数字 | 回顾时查 |
| `docs/WALKTHROUGH/`（本目录） | 代码到底怎么运转？每一行属于哪个决策 | 逐册读 |

本目录不重复上表里已有的"结论与数字"；它负责把结论**落回到符号**。

## 分册索引

| 册 | 文件 | 主题 | 覆盖单元 |
| --- | --- | --- | --- |
| 00 | `00-config-and-startup.md` | 启动与配置 | `cmd/infergate`、`internal/config`、`internal/miniyaml`、`internal/logging`、`configs/*.yaml` |
| 01 | `01-http-server.md` | HTTP 服务层 | `internal/server`（路由、鉴权、`/metrics`、`/stats`、`/admin/*`） |
| 02A | `02a-gateway-core.md` | 网关核心管线 | `internal/gateway`（`proxy.go`、`request.go`、`errors.go`、`headers.go`） |
| 02B | `02b-stream-and-upstream.md` | 流式中继与上游 | `internal/gateway/stream*.go`、`internal/sse`、`internal/upstream` |
| 02C | `02c-governance-paths.md` | 治理路径 | 缓存、配额、幂等、能力探测、请求级追踪（`internal/gateway` 其余文件） |
| 03 | `03-router-breaker-ledger.md` | 路由与熔断 | `internal/router`、`internal/breaker` |
| 04 | `04-cache-quota-memory.md` | 缓存 · 配额 · 向量 · Redis · 账本 | `internal/cache`、`internal/embed`、`internal/redis`、`internal/quota`、`internal/idempotency`、`internal/sessions`、`internal/stats`、`internal/mockredis`、`internal/mockbackend` |
| 05 | `05-observability.md` | 可观测性 | `internal/tracing`、`internal/traceexport`、`internal/metrics`、`internal/server` 的 `/metrics` 文本 exposition、`cmd/mockupstream`、`cmd/mockcollector` |
| 06 | `06-repo-selfchecks-ci.md` | 命令 · 部署 · 工具链 | `cmd/` 目录形状、六个可执行命令、`cmd/verify` 家族的定位、`deploy/`、`Dockerfile`、`docker-compose.yml`、`configs/docker.yaml` |
| 07 | `07-loadtest-measure-deploy.md` | 构建 · 脚本 · CI 自检 | `tools/go.cmd`、`Makefile`、`scripts/**` 全清单、`.github/workflows/ci.yml`（九步 `continue-on-error` 与总闸）、`.gitignore`/`.dockerignore`/`.gitattributes`、`internal/repofmt` |
| 07B | `07b-verify-and-measure.md` | 验收程序与测量脚本 | `cmd/verify`、`cmd/verify-m1`…`cmd/verify-m6` 的逐断言主线、`cmd/verify-m6` 的 `scriptedUpstream`、`scripts/measure-m2.ps1`…`measure-m5.ps1` |
| 08 | `08-supplement-data-plane.md` | 数据面补遗 | `internal/embed`、`internal/evalset`、`internal/mockbackend` 的逐符号走读 |
| 09 | `09-loadtest-and-measure.md` | 测量工具 | `cmd/loadtest`（38 个函数、8 个类型、`result` 字段逐个）、`cmd/measure-m2`、`scripts/measure-m2.ps1`…`measure-m5.ps1` |
| 10 | `10-repofmt-helpers.md` | 自检的辅助符号 | `internal/repofmt` 十四个自查文件里被测试调用的辅助函数与辅助类型逐个展开 |
| 11 | `11-verify-harness-fixtures.md` | 验收程序的夹具 | `cmd/verify`、`cmd/verify-m1`…`cmd/verify-m6` 的起栈、造体、断言、轮询等待与逐 `check*` 定位表 |
| 12 | `12-mockredis-and-quota-fields.md` | 模拟服务与额度字段 | `internal/mockredis` 的十六条命令处理器与 `infoText`、`internal/quota` 的策略字段、计数器、`Reason*` 取值与桶函数 |

建议路线：想做网关的人按 00 → 01 → 02A → 02B → 02C 读；想做数据面治理的人按 02C → 03 → 04 读；只想复现数字的人按 07 → 05 读。

## 三分钟看懂：一次请求的数据流与状态落点

这一节是索引页上的速览，详解在各册；它只回答三个问题：请求怎么走、状态存在哪、为什么这么切分。

**一次补全请求的完整路径**（`internal/gateway/proxy.go` 的 `serve` 就是这条流水线，逐闸门见第 02A 册第 11 节）：

1. **准入与校验**：路径可代理吗（不是则 404）→ 请求体读取受 `max_body_bytes` 限制（超限 413）→ 解析 `model` / `stream` / `max_tokens`（`model` 不可解析则 400，不是 502）。
2. **配额准入**：`internal/quota` 先按维度（租户 / 会话 / 模型 / 日 / 分钟）**预留**额度，预留失败按配置**失败关闭**（503，且零上游调用）或降级到备用模型继续。
3. **幂等**：带 `Idempotency-Key` 的重复请求**完全重放**上一次的响应，不触上游、不再计费。
4. **语义缓存**：算缓存身份（模型 + 温度 + prompt 摘要 + …）→ 精确命中或向量近邻命中则直接重放；未命中走上游，响应在**写回客户端之后**再落缓存。
5. **路由**：按模型与能力过滤掉不合格候选，再按策略打分（优先级 / 成本 / 延迟 / 分数 / 分层 / 加权），逐个尝试；每次失败换下一个候选，尝试次数由 `maxAttempts` 封顶。
6. **转发与中继**：非流式整段拷贝；流式走 `internal/sse` 的解析 + 单 writer 逐帧 `Flush`，顺带采 token、首字延迟、帧数、字节数与成本。
7. **收尾（deferred finalizer）**：无论成功、失败还是客户端断开，都在同一处结算配额、落缓存、写幂等记录、记会话账本、写日志与指标——**一次请求恰好一条日志、一次指标观测**。

**状态存在哪里**（第 02A 册第 3 节与第 24 至 31 节）：

- `gateway.Proxy` 本身**每请求无状态**：字段构造后只读，所以可以被所有 handler goroutine 共享而不加锁。
- 可变状态收在一个每请求的 `record` 累加器里（地址、模型、状态、字节数、帧数、用量、配额三态、缓存身份、幂等结果、追踪 span…），只在本次请求内传递。
- 真正共享可变的是**注入的句柄**：上游注册表、路由器、配额管理器、缓存与价目表、会话账本、幂等存储、限流/熔断器，各自带自己的锁；灰度统计用 `atomic` 值。

**为什么这么切分**（设计取舍，理由分散在各册，这里只给一句话版）：

- **零第三方依赖**：配置解析器（`internal/miniyaml`）、Redis 客户端（`internal/redis`）、RESP2 模拟服务（`internal/mockredis`）都是自研的；代价是要自己维护解析器，收益是 `go build` 不依赖网络。
- **不缓冲上游响应**：HTTP 服务器**故意不设 `WriteTimeout`**（否则长回答会在句子中间被静默截断），代价是流式请求的唯一上限落在 `upstream_timeout` 上。
- **出站 context 不继承入站**：handler 一返回入站 context 就被取消，长流会被连带中断，所以出站请求改从 `context.Background()` 派生。
- **错误分类即责任划分**：模型不可解析是调用方的错（400），上游不可达是上游的错（502），上游超时是超时（504），客户端自己断开是 499——把调用方的错记成上游的错会污染健康度与熔断判定。
- **缓存只做"无害的漏"，不做"看不见的错"**：未命中只是慢一点，误命中是调用方看不见的错误，所以阈值取语料里最紧真释义对之上的 0.86。

## 阅读约定

1. **锚点格式**：`符号名 (internal/config/config.go)`。历史上本目录写过 `文件:行号` 形式的锚点，现已改为只写符号名与文件——行号会腐坏，而符号名由 `internal/repofmt` 的自检测试保证真实存在。
2. **行号只出现在代码块里**。散文里出现 `某个文件:数字` 会让 `internal/repofmt/citedpaths_test.go` 报错，这是仓库的硬规则：散文负责指认符号，不负责指认位置。
3. **"'为什么'是源码注释的观点"** 这类标记表示那句话来自源码注释或设计文档，不是分册作者的推断。
4. 分册里出现的每一个 `pkg.Symbol` 都由测试对照 `go/ast` 索引校验过；出现"未能确认"字样表示作者没有在源码里找到确证。

## 写文档的注意事项（教训）

本目录曾经被一次编码事故毁掉过：用 PowerShell 的 `Get-Content -Raw` 读 UTF-8 文件时，Windows PowerShell 5.1 会按系统 ANSI 代码页（本机是 936）解码，再用 `Set-Content -Encoding UTF8` 写回，中文就永久损坏（大量字符变成 `\uFFFD`）。所以：

- 写这些文件**只能用具备 UTF-8 能力的编辑工具**，不要用 `Set-Content` / `Out-File` / `>` / `>>`。
- 校验时读文件也要显式指定 UTF-8，例如 `Get-Content -Encoding UTF8`，或用 `[System.IO.File]::ReadAllText($p, [System.Text.UTF8Encoding]::new($false))`。

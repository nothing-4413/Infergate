# InferGate 设计文档（M0）

本文档回答两个问题：**M0 的代码为什么这么组织**，以及**每个关键位置为什么这样写**。
目标读者是"要能逐行讲清自己代码"的作者本人，所以每条都写到可以口头复述的程度。

---

## 1. 分层与依赖方向

```
cmd/infergate ──► internal/server ──► internal/gateway ──► internal/upstream ──► internal/config
                        │                    │                    │
                        │                    ├──► internal/sse    │
                        │                    ├──► internal/metrics│
                        │                    └──► internal/logging│
                        └──► internal/metrics                      └──► internal/miniyaml
```

约束：**依赖只能向下，不能成环**，而且 `internal/gateway` 不认识 `http.Server`、不认识 CLI、
不认识配置文件如何被解析。这样做的收益在 M1–M3 会兑现：路由策略、语义缓存、配额判定都要能被
单独单测，不需要起一个真服务。

`internal/gateway` 对外的唯一缝是 `Options`：

```go
type Options struct {
    Upstreams      *upstream.Registry
    Pricing        *PriceBook
    Metrics        metrics.Sink   // 接口，nil 时退化为 metrics.Nop
    Logger         *slog.Logger
    MaxBodyBytes   int64
    UpstreamTimeout time.Duration
    TransportFor   func(*upstream.Target) http.RoundTripper
}
```

`Metrics` 是接口而不是结构体：单测里可以塞一个只记录数组的假实现，不必清空全局状态；
`TransportFor` 是函数而不是字段：单测可以注入 `RoundTripper` 打桩，而生产始终走 per-target 连接池。
**`TransportFor` 为 nil 时 `New` 会补一个返回 `t.Transport` 的默认实现**——这一条是踩坑换来的：
早先 nil 被原样存下来，导致每个请求都在 `buildRequest` 里空指针 panic，然后被 recover 成 500，
表面上像"上游问题"，实际是构造问题（详见第 6 节）。

---

## 2. 配置：YAML → JSON → struct

### 为什么手写 `internal/miniyaml`

M0 只需要映射、序列、标量三种结构（环境里也没有可用的模块下载，见 README 第 4 节）。
手写解析器的真正价值不在"省一个依赖"，而在**把配置的类型判定收敛到一处**：

1. `Parse` 把 YAML 子集规范化成 JSON（`Node` 树 → `encoding/json`），
   于是 `json` tag 成为 schema 的唯一真相来源，不需要为 YAML 再维护一套 tag 或反射规则。
2. 标量按 YAML 规则定型（`scalarJSONValue`）：**带引号的一律是字符串**；
   `""`/`~`/`null` → nil；`true/yes/on`、`false/no/off` → bool；能被 `strconv.ParseFloat`
   接受的 token → `json.Number`（而不是 `float64`，因为 `encoding/json` 会原样输出 `json.Number`，
   大整数如 `max_body_bytes` 和价格精度都不会被浮点化）。`v2`、`1.2.3` 这类不是数字，保持字符串。
3. 明确拒绝不支持的东西并给出可定位的错误：Tab 缩进、锚点/别名、块标量、重复键。

代价也很清楚：**不是完整 YAML**（不支持多文档、块标量、锚点），因此配置里不能写多行字符串。
这是一条要写进贡献指南的硬约束。

### 三个被真实配置暴露出来的解析器缺陷（都值得记住）

| 缺陷 | 症状 | 根因 | 修法 |
| --- | --- | --- | --- |
| 序列项缩进过严 | `line 55: unexpected indentation 4 in sequence, expected 2`，最常见的 `- name: x` + `  kind: y` 写法直接加载失败 | `parseSeq` 要求每一行都在短横线的列上 | 只有当更深的行**本身不是** `- ` 序列项时才视为越界，其余交给该项目的映射继续消费 |
| 映射基准列算错 | 修好上一条后，第二个键仍被当成"跑偏的缩进行" | `strings.Index(ln.text, rest)` 在**已 trim** 的文本上取值，得到的是 key 在行内的列（2），而不是它在文件里的列（4） | 改成 `indent + strings.Index(...)`，并补一个测试把三种等价写法钉死 |
| 标量永远是字符串 | `cannot unmarshal string into Go struct field price.pricing.default.in of type float64`；引号还留在值里（`"deepseek"`） | `toJSONValue` 直接返回 `n.Str` | 引入 `scalarJSONValue`，按 YAML 规则定型为 bool / `json.Number` / nil / string |

### 时长为什么需要自定义类型

`encoding/json` 无法把 `"90s"` 解进 `time.Duration`（底层是 int64，只接受数字），而配置里写
`90000000000` 是不可用的。所以 `internal/config` 定义了：

```go
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error   // 接受 "90s" 或裸纳秒整数
func (d Duration) MarshalJSON() ([]byte, error)     // 始终输出字符串形式
func (d Duration) Duration() time.Duration
func (d Duration) String() string
```

放在 `UnmarshalJSON` 而不是后处理，好处是：schema 仍是普通 struct，转换逻辑可单独测试，
且任何加载路径（含测试里直接构造 `config.Config`）都不会绕过它。
注意**替换了 4 个字段的类型后，所有调用点都要显式 `.Duration()`**——编译器会全部指出来，
这正是选"新类型"而不是"松散的字符串字段"的原因。

### 其它加载期决策

- `Load()` 首先 `bytes.TrimPrefix(raw, BOM)`：UTF-8 BOM 是编码产物而不是内容，
  而第 1 行通常是注释，若不剥离会报出"expected key: value, got \ufeff# ..."这种极具误导性的错误。
- `${VAR}` 未定义时**直接启动失败**，不回退空串：把配置错误暴露在启动期，而不是让它变成
  "运行时所有请求 401"。
- 启动即 `Validate()`：拒绝重名/空名上游、非 http(s) scheme、既无 `models` 也无 `"/"` 的上游、
  负价格。上游 `base_url` 在**构造时**就去掉尾部 `/`，否则会拼出 `//v1/chat/completions` 而全量 404。

---

## 3. 请求生命周期（非流式）

```
ServeHTTP
 ├─ 建立 record（request_id / route / 起始时间）
 ├─ defer: recover → 500 | 一次性指标上报 | 一条结构化日志
 └─ serve
     ├─ isProxiablePath?  否 → 404（OpenAI 形状的错误体）
     ├─ readBody            → 超过 max_body_bytes → 413（用 LimitReader 多读 1 字节判断越界）
     ├─ inspectRequest      → model / stream
     ├─ upstreams.Resolve(model, X-InferGate-Upstream)
     │      显式头 → 模型精确匹配 → 兜底 "/" → 只剩一个后端时吸收任意模型名
     ├─ resolveModelName + rewriteModel
     │      "/" 兜底后端 → 原样透传模型名（vLLM/Ollama 需要用户自己的别名）
     │      精确命中 → 不改；否则改写成该后端唯一的具体模型名
     ├─ buildRequest
     │      ctx 从 context.Background() 派生 + upstream_timeout
     │      拷贝头部（去 hop-by-hop、去 x-infergate-*）
     │      target.APIKey 存在则覆盖 Authorization，否则透传调用方的
     │      URL = target.BaseURL + r.URL.Path (+ raw query)
     └─ client.Do
         ├─ 传输错误 → 504 超时 / 499 客户端断开（不写体）/ 502 不可达
         ├─ 4xx/5xx  → 原样转发上游错误体（不替换成网关自己的文案）
         └─ 2xx      → copyWhole：JSON 走 accountJSON（取 usage 与 provider 报告的 model），
                        其它类型走 io.Copy
```

### 为什么这些细节必须这样

- **请求体解析成 `map[string]json.RawMessage` 再序列化**：普通 `map[string]any` 会把
  `1e9` 变成 `1e+09`、把大整数变成浮点。用 `RawMessage` 保留每个值的原始字节，
  保证"只改了 model 字段"是真的只改 model 字段。
- **不替换上游错误体**：调用方（Agent 框架）依赖 provider 的 `error.type` / `code` 做重试决策，
  网关改写文案会把可重试错误变成不可重试错误。
- **客户端断开 → 499 且不写响应体**：连接已经不在了，写任何东西都是浪费；499 是 nginx 约定，
  让日志与指标能区分"我们错了"和"客户走了"。
- **模型无法解析 → 400 而不是 502**：这是调用方传错模型名或运维配错路由表，属于 4xx。
  返回 502 会污染上游健康度统计，还可能让熔断器误判后端故障（M1 起熔断要吃这些数据）。
- **`copyHeaders` 必须同时剔除 `Connection` 里点名的头部**：RFC 7230 §6.1 规定
  `Connection` 字段声明的是**本条消息**的逐跳头。只删静态黑名单的话，客户端只要发
  `Connection: Authorization` 就能影响下一跳对 Authorization 的处理。

---

## 4. 流式中继（M0 的核心）

```
relayStream
 ├─ sse.Writer（要求 ResponseWriter 支持 Flusher，否则 500）
 ├─ reader goroutine：独占 resp.Body ──帧──► 带缓冲 channel(8)
 └─ 主循环 select：
      ├─ r.Context().Done()  → outcome=canceled，reason="client disconnected mid-stream"
      ├─ 15s ticker          → 注释帧 `: keep-alive`（防中间设备掐空闲连接）
      └─ 帧
           ├─ inspector.Observe(frame)      → usage / model / finish_reason / tool_calls / 流内 error
           ├─ 首个"可见 token"帧 → rec.firstToken 打点
           ├─ obs.Err != nil → 原样转发后结束，outcome=upstream_error
           └─ writer.WriteUpstream(frame)   → 回放上游原始字节
 收尾：若上游没发 [DONE] 则补一个；落 metrics/usage/model
```

### 逐条理由

- **回放 `Raw` 而不是重新序列化**：帧的 `Raw` 保存的是上游原字节（含空行终止符），
  客户端与上游看到的流是完全一致的。这也是 `sse.Reader` 里"终止符必须恰好剥一次"的由来：
  既要能判断空行（结束一帧），又要让字段值干净，还要让 `Raw` 保留原始终止符。
- **首字打点只在"可见 token"**：OpenAI 会先发一个 `{"delta":{"role":"assistant","content":""}}`
  的开场帧。如果按"第一个非空 data 帧"计算，TTFT 会虚低到几微秒，指标就没有意义了。
  所以 `firstVisibleToken` 要求 `delta.content` / `delta.reasoning_content` 非空、
  或 `delta.tool_calls` 非空、或 `message.content` 非空；**JSON 解析失败时返回 true**——
  延迟指标可以偏大，但不能把一个真实的卡顿藏起来。
- **`firstTokenSet` 布尔标志而不是 `> 0` 判断**：本机回环上首个 token 可能真的落在 0 秒
  （实测 `first_token=0s`），用 `> 0` 会把合法样本丢掉，而且 0 与"没记录"无法区分。
- **单 writer**：只有一个 goroutine 向客户端写，因此既不需要给 writer 加锁竞争，
  也不会出现两个 goroutine 交错写出半帧的情况。channel 容量 8 是在"上游突发"与"内存"之间取的折中。
- **补 `[DONE]`**：OpenAI SDK 以 `[DONE]` 作为流结束信号，上游省略时客户端会一直挂着。
  网关补齐是"兼容性兜底"，同时让 M5 的压测口径统一。
- **`upstream_timeout` 覆盖整条流**：它是从转发开始计时的总上限，防止"上游一直滴答但永不结束"
  的连接把 goroutine 与连接池拖死。

---

## 5. 观测：日志、`/stats`、`/metrics` 三者的分工

- **结构化日志（slog）**：一次请求一条，带 `request_id`、`route`、`upstream`、`model`、`stream`、
  `status`、`outcome`、`req_bytes`、`resp_bytes`、`elapsed`、`first_token`、`frames`、
  `prompt/completion/cached_tokens`、`cost_usd`、`reason`。成功是 info，客户端断开是 debug，
  其余是 warn——这样默认级别下日志里出现的每一条都值得看。
- **`/stats`（JSON）**：给人和脚本看的聚合，含 p50/p90/p95/p99/max（最近秩法）、token 汇总、
  首字均值、流分片数。
- **`/metrics`（Prometheus 文本）**：给抓取器看，手写文本以避免依赖客户端库。
  M5 会补直方图（现在是 `_sum` + 计数），因为平均延迟无法表达长尾。

三者刻意重叠是有意的：日志回答"这一次请求发生了什么"，`/stats` 回答"刚才这批请求怎么样"，
`/metrics` 回答"长期趋势如何"。排障时先看日志定位单条，再用指标确认是否系统性。

`internal/metrics` 的内部 key 类型（`requestKey`/`attemptKey`/`tokenKey`）不导出，
因此对外只提供 `*Rows`/`*Snapshot` 形式的切片访问器，排序在 `sort.go` 里做——
保证 `/stats` 输出顺序稳定，便于 diff 与脚本断言。

---

## 6. 踩坑记录（每条都改变了代码）

1. **`TransportFor` 为 nil → 每请求 panic 成 500**。panic 被 recover 后表现为"网关内部错误"，
   看起来像上游问题。修法是 `New()` 里补默认实现；同时给 recover 加了 `debug.Stack()`，
   因为"panic recovered"这一行不打印栈就等于没有信息。
2. **SSE 解析器 `readLine` 返回 bufio 内部缓冲的切片**，后一次读会静默改写前一行；
   修法是读出来就 copy。凡是"借出缓冲区视图"的 API 都要问一句：调用方保存它多久？
3. **行终止符没剥干净（只剥 `\r` 不剥 `\n`）**，导致空行判断永远为假，
   整条流被解析成**一帧**，帧名甚至是 `"message\n"`。修法是统一 `TrimRight("\r\n")`，
   并写明"终止符只在一处剥一次"，因为空行判定与字段取值都依赖它。
4. **`Raw` 里没写空行**，于是回放给客户端的帧是首尾相接的，
   `data: {..}data: {..}` 已经不是合法 SSE（客户端重新解析会得到一帧）。
   修法是取 `Raw` 之前先把该行写进累积缓冲——**"字节透明"必须用真客户端重新解析来验证**，
   只数帧数会漏掉这一类错误。
5. **测试假上游持有锁跨越 handler**，`httptest.Server.Close()` 等 handler 返回、handler 等锁释放，
   测试挂死。修法是 `sync.Mutex` + 先记录后回调、并立刻释放锁。
   教训：清理函数可能等待在飞请求，不要在 handler 内长时间持锁。
6. **`Connection` 点名的头没被剔除**（见第 3 节第 5 条）。
7. **单后端时模型名无法路由**：本地 vLLM/Ollama 只服务一个量化模型，Agent 框架发的别名网关从没见过。
   于是加了"只剩一个后端时吸收任意模型名"的规则，并**刻意放在最后**：
   有多个后端时没有唯一答案，猜测会静默路由到错误模型，那是任何指标都发现不了的错误。
   这条规则也改了测试——`TestUnknownModelWithoutCatchAllReturns400` 现在必须配两个后端，
   因为 400 只存在于"真的需要选择"的场景。
8. **`127.0.0.1:8080` 上的 404 不是网关的错**：本机已有别的进程绑在 `127.0.0.1:8080`，
   Windows 优先更具体的绑定，所以 `127.0.0.1` 的请求根本没到网关。用 `localhost` 或换端口。
   `Get-NetTCPConnection` 在这里查不到东西，要用 `netstat -ano`。
9. **PowerShell 5.1 会吃掉传给原生 exe 的引号**，请求体变成非法 JSON（脚本用 `--data-binary @file` 规避）；
   `-like` 把 `[DONE]` 当字符类，导致假失败（改用 `.Contains`）；
   `2>&1` 配合 `$ErrorActionPreference='Stop'` 会让脚本中途崩掉（改用 `2>$null`）；
   `Start-Process -RedirectStandardOutput` 是追加模式，会让日志断言读到上一轮的结果（改用文件偏移）。
   这四条都属于"验证工具本身骗人"，比产品 bug 更危险，所以每条都在脚本里写了注释。

---

## 7. 已知限制与 M1 的接口预留

| 限制 | 现状 | M1 及以后 |
| --- | --- | --- |
| 无重试 | 上游错误原样转发 | 幂等判定 + 退避重试（流式只在首帧前可重试） |
| 无熔断 | 只有 per-request 超时 | 按 upstream 维度的滑动窗口 + 半开探测 |
| 路由是静态表 | 显式头 / 模型匹配 / 兜底 | 成本·延迟·能力·健康度加权打分 |
| 指标是内存态 | 进程重启即清零 | 接 OTel/Prometheus，加直方图与标签基数控制 |
| 无多模态/embedding 特化 | 统一转发 | 按 route 区分 `/embeddings` 的 usage 语义 |
| 缓存无 | 每请求都打上游 | M2 语义缓存：embedding + 阈值门控 + Redis |

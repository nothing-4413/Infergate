# InferGate 设计文档（M0 + M1 + M2 + M3 + M4）

本文档回答两个问题：**M0 / M1 的代码为什么这么组织**，以及**每个关键位置为什么这样写**。
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
  代价是 reader 侧两次入 channel（帧、以及说明流为何结束的终局错误）都可能在缓冲满且 writer 停住时
  阻塞；两处都带 `r.Context().Done()` 守卫，客户端中途断流时 reader 就随请求上下文退出，
  不会把 goroutine 连同它独占的上游连接一起押在发送上。守卫只有一处漏掉就足够泄漏，
  所以 `internal/gateway/stream_test.go` 的 `TestClientHangupMidStreamDoesNotStrandTheReaderGoroutine`
  专门把缓冲填满、逼 reader 停在终局发送上，再取消请求来盯住这一点。
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

## 7. M0 遗留限制的 M1 结论

| M0 的限制 | M1 的结论 |
| --- | --- |
| 无重试 | **已实现**：错误分类后换后端重试（连接错误 / 超时 / 429 / 5xx），流式只在首帧前；4xx 原样转发且记为成功 |
| 无熔断 | **已实现**：按 upstream 的滑动窗口（时间分桶）+ `min_requests` 门槛 + 半开探针 |
| 路由是静态表 | **已实现**：五种策略排序（priority / cost / latency / weighted / score），候选先按模型与能力筛 |
| 指标是内存态 | 仍是内存态（M1 只加了熔断与故障转移指标）；OTel + Prometheus 在 M5 |
| 无多模态 / embedding 特化 | 仍未做：`/embeddings` 的 usage 语义按 route 区分，留到 M5 |
| 无缓存 | 仍无：每请求都打上游，M2 语义缓存（embedding + 阈值门控 + Redis） |

---

## 8. M1：路由、故障转移与熔断

### 8.1 分层与新的依赖方向

```
internal/gateway ──► internal/router ──► internal/upstream
      │                   └──────────► internal/stats ◄── internal/breaker
      ├──► internal/breaker
      └──► internal/metrics / internal/logging / internal/sse
```

- `internal/router` **不做 IO、不改输入**。`Plan(Request) ([]Candidate, error)` 只吃一个请求描述
  和"某个后端现在健不健康、均延迟多少"的快照，输出排好序的候选，每个候选带上**该用的模型名**
  与一句 `Reason`。这样路由策略可以纯单测（`internal/router/router_test.go`，18 个用例），
  也让"为什么选它"只有一个出处：`reasonFor()`。
- `internal/stats` 是每个上游的滑动窗口（attempts / failures / timeouts / 延迟 / 首字），
  `internal/breaker` 在它之上做状态机。**窗口按时间分桶，不是累计计数器**：一小时前的一串失败
  不应该压着现在的判定，而"清零"又会让熔断器永远攒不满样本。
- `internal/gateway` 只认识 `Plan` 的返回值，不关心候选是怎么排出来的。这是 M1 能插进 M0
  代理循环、而不是重写它的原因：`serveAttempts` 的循环体就是 M0 的单次转发，
  多出来的只有"换一个候选再来一次"。

### 8.2 候选集：先按模型与能力筛，再排序

`Plan` 的第一刀是**模型与能力**，不是健康度。`eligible()` 只保留"模型能对得上"且"声明了请求
要求的全部能力"的后端。理由很直接：把请求交给一个模型名对不上的后端，得到的是一个 404
或者一个张冠李戴的答案，健康度再好也没用。一个能力都匹配不上时返回错误（网关映射为 400），
**不静默降级**——把需要 function calling 的请求降级给不会调用工具的模型，产生的错误答案
看起来完全正常，这比直接报错危险得多。

第二刀才是排序（`priority` / `cost` / `latency` / `weighted` / `score`）：

- `cost` 用价格表（USD / 1M tokens）算混合单价。**没有标价的后端排最后，而不是按 0 算**，
  否则"漏配一行价格"会静默变成"免费，永远优先"。价格行按**具体模型名**索引，
  所以只有声明了具体模型名的后端才带自己的价格；请求的模型名没标价时回退到该请求的模型价。
  **计费侧没有同一把锁**：`CostUSD` 只查价、查不到就按 `pricing.default` 折算，而 default 没配
  就是 0——那些请求会被累加成"不花钱"，成本预算也就永远看不见它们。所以启动时把"上游声明了、
  价格表却没有"的模型报一次（`internal/server/server.go` 的 `reportUnpricedModels`）：default 为 0
  是 WARN（说的是"记成 $0"这个方向），非 0 是 INFO（是猜测，不是 0）；全都标了价就一行都不打。
  放在启动而不是请求路径上，是因为这是配置缺口，不是每请求事件。
- `latency` 用窗口实测均值；**没测过的后端排最后**（未知 ≠ 最快）。
- `weighted` 按 weight 无放回抽样，用于灰度分流；无放回保证一次请求内不会重复抽到同一个后端。
- `score` 把成本、延迟、可靠性、优先级四个因子各自在**候选集内**归一化到 0..1 再加权求和。
  可靠性是唯一能单独把后端踢出轮转的因子，因为它不是偏好而是事实。
- `priority` 相同时按名字排，保证同一份输入的排序可复现（否则并发请求会看到随机顺序）。

### 8.3 熔断：open 的后端排最后，而不是被删掉

`breaker.Allow()` 回答"这次放不放行"，`router.healthy()` 只是**读状态**。不健康的后端保留在
候选集里、排到最后——两个后果都是故意的：全挂时还有最后一个能试的后端（删掉就只剩 502），
而 `Reason` 会写成 `priority: circuit open, only used as a last resort`，日志和
`/admin/breakers` 都能解释这次为什么打到了"坏"后端。

跳闸门槛是 `min_requests` + `failure_ratio`：样本不足时**未知按健康处理**。反过来（默认不健康）
会让一个刚启动、还没跑过请求的后端永远拿不到流量，冷启动直接死锁。分母是整个窗口的尝试数，
**成功也计入**——这是"宁可晚跳闸，也不要抖动"的取舍；代价是短促故障发生在繁忙窗口里时，
需要更多失败才凑够比例。

半开状态只放行 `half_open_probes` 次探测，探测成功才闭合；探测失败立即重新跳闸（不用再攒样本），
因为"刚试过，坏了"本身就是最新的事实。

还有第三条出路，是"这次探测什么都没说"：客户端在生成过程中断了、或者请求根本没能构造出来。
这两种结局都**不构成对后端的判断**（把客户端的行为记成后端失败，会让一个健康后端被踢出去），
但也不能什么都不做——半开只放行一次探测并等结果，一次放行而永远不回话，那个后端就会被
`half-open probe already in flight` 拒绝到进程结束。所以 `internal/breaker` 的
`ReleaseProbe()` 只归还这个名额、不写任何样本；`internal/gateway/proxy.go` 在
`attemptUpstream` 里用一个 `defer` 兜底：八个报结论的出口（`reportSuccess` / `reportFailure`）
会把它标成已报，其余所有返回路径都走归还。

### 8.4 故障转移：每次尝试一个预算

`upstream_timeout` 是**每次尝试**的预算，不是整条请求的共享预算。这一条是实测改出来的：
先写成共享 deadline（`remainingBudget`）看起来更"正确"，实际让第一个慢候选吃光了窗口，
后续健康候选拿到的都是已经过期的 deadline，客户端拿到 504 而网关**从未问过那个快的后端**——
日志是 `slow attempt abandoned after 600000000ms, client total 1210ms`。故障转移沦为装饰。
改成每次尝试独立计时后，最坏情况是 `upstream_timeout × 尝试次数`，两个旋钮都在配置里。

其余几条都写在 `serveAttempts` 的注释里：

- 可重试的错误类别是**连接错误 / 超时 / 429 / 5xx**。4xx 原样回传给调用方，并且
  `RecordSuccess()`：调用方发来的坏 payload 不该让一个健康上游的熔断器跳闸。
- 重试之间有 `retry_backoff`（线性增长 + 全抖动）。固定退避会让所有客户端同拍重试，
  等于自己制造雪崩。
- 流式请求**只在首帧之前**可以重试。已经写出去的字节收不回来，重试会把两个后端的输出拼成
  一份"看起来正常"的答案，而且重复计费。
- 全部候选都失败时，回传**最后一个** Provider 的状态码与响应体（而不是网关自己编一个），
  调用方仍然可以按 provider 的 `error.type` 决策。
- `X-InferGate-Tried` 只在尝试次数 > 1 时出现。一次就成功时带上它，只会让客户端多解析一个
  字段而没有任何信息量。

### 8.5 验证策略：为什么要有两个验收器

M0 的经验是"验证工具本身会骗人"，M1 把它变成了两条互补的路径：

- `cmd/verify-m1`（Go，64 条断言）：用 `internal/mockbackend` 在**进程内**起后端，可以按后端
  注入故障（`SetFailStatus` / `SetStall`），从而确定性地构造"只有某一个后端坏"的场景。
  它不依赖端口、不依赖外部进程，所以是 CI 门禁。
- `scripts/verify-m1.ps1`（curl，56 条断言）：真进程 + 真 curl，覆盖 operator 视角的路径
  （编译、起三副本、杀进程、`/admin/*`、日志）。它测的是 Go 验收器测不到的东西：
  真实的 socket、真实的进程生命周期、真实的管理端点。

M1 期间最大的一次"验证工具骗人"：`cmd/mockupstream` 的故障注入头（`X-Mock-Status` 等）是
**按请求**生效的，网关转发时会把它带给**每一个**候选——于是"让第一个后端失败"实际变成了
"让所有后端都失败"，测试看起来在验证故障转移，其实在验证全挂路径。所以按后端注入故障的能力
（`internal/mockbackend`）不是方便，而是必需；curl 脚本里则改成"真的杀掉一个进程"。

## 9. M2：语义缓存

### 9.1 位置与依赖方向

```
请求 → 解析（inspectRequest + cacheIdentityFor）→ 缓存判定/命中 → 路由 → 尝试上游
```

缓存插在**解析之后、路由之前**（`internal/gateway/proxy.go` 的 `serve` 里
`prepareCache` / `cacheAttempt` 先于 `serveAttempts`）：命中时不需要知道有哪些后端，
更不应该去碰熔断器的窗口。这条"依赖方向"也解释了为什么 `gateway.Proxy` 里持有的是
**具体类型** `*cache.Cache` 而不是接口——缓存的正确性论证（什么算同一个问题）属于网关这一层，
存储层（`cache.Store`）才是可替换的。

新增包：`internal/cache`（策略 + 两个 Store）、`internal/evalset`（带标签的语料）。
`internal/embed`（离线 hashing + HTTP 两种 embedder）与 `internal/redis`（手写 RESP2 客户端）
在 M2 之前就已存在并有测试。

### 9.2 身份：什么算"同一个问题"

一次缓存判定需要四个东西，全部在 `internal/gateway/cachereq.go` 里由**纯函数**算出：

| 名称 | 内容 | 为什么 |
| --- | --- | --- |
| `Scope` | `tenant + model + capabilities` 的 sha256 前 16 位，前缀是清洗过的模型名 → `<model>/<hash16>` | 多租户隔离；不同能力约束可能路由到不同后端，回答不能互借 |
| `ExactKey` | 客户端**原始** body + path + tenant 的 sha256 | 字节级重试必须命中，包括 temperature/seed/max_tokens 全一致 |
| `Signature` | model、stream、temperature、top_p、max_tokens、n、seed、stop、tools、tool_choice、response_format + **对话前缀**的 sha256 | "总结设计文档"在 temperature 0 和 1 下是两个请求；同一句话在不同工具结果之后也是两个问题 |
| `Prompt` | 最后一条 `user`/`tool` 消息的文本 | 语义检索的对象 |

两个容易踩的细节：

- **`ExactKey` 必须在 `rewriteModel` 之前算**。`rewriteModel` 会把 body 重新 marshal，
  键序变成字典序；用改写后的 body 算键，两个客户端用不同别名指同一个模型时会得到不同的键。
- **前缀整体参与 hash**（`messagesFingerprint`），而不是只看最后一句话。Agent 循环里
  "同一个问题"往往是不同的上下文，只比最后一句话会把别的问题的答案发出去。

`X-InferGate-Cache` 是**双向**头：请求侧取值 `bypass` / `refresh`，响应侧取值
`hit-exact` / `hit-semantic` / `miss` / `skip` / `disabled` / `error`。它属于网关自己的头，
上游拿不到（`isGatewayHeader` 会剥掉）。

### 9.3 策略：只做减法

`cache.Cacheable` 返回 `Decision{Lookup, Store, Semantic}`，初值全是 `true`，之后**只会被拿掉**：

- `tools` 非空且未开 `allow_tools` → `Semantic=false`，理由 "tools present: exact match only"。
- `temperature > 0`（或 `top_p < 1`）且未开 `allow_nondeterministic` → `Semantic=false`。
- `bypass` / `refresh` → `Lookup=false`（但**仍然 Store**，所以 bypass 请求拿到的新答案会进缓存）。
- 提示词短于 `min_prompt_chars` → 完全不缓存（`skip`）。

这套规则来自一个不对称性：**miss 的代价是本来就要发生的一次上游调用，而错误的命中是调用方
看不见的缺陷**。所以只要身份不确定，就选择重新问模型。注意 `temperature > 0` 仍然可以做
**精确**命中——逐字节重试同一个采样请求是同一件事，而**换一种说法**永远不会被当成同一件事
（网关测试 `TestCacheSamplingRequestsMatchExactlyOnly` 钉住这条）。

省略 `temperature` 视为确定性（指针为 `nil`），因为省略参数是最常见的情况，而拒绝缓存它
等于让缓存永远不命中。这条判断是有风险的，所以 `allow_nondeterministic` 与 `allow_tools`
两个开关是给运维的，而不是给代码的。

### 9.4 阈值：由测量决定，不由直觉决定

离线 `HashingEmbedder` 是**词法**的，所以阈值必须对着语料量。`internal/evalset` 有 26 对
带标签的中英样本（13 对应命中：同义改写、跨语言；13 对不该命中：近似词、只换一个实体），
`cmd/measure-m2` 扫一遍阈值：

```
thresh    hit rate       false-hit rate
0.84      77% (10/13)    23% (3/13)   nm-en-7,nm-en-8,nm-en-2
0.86      62% (8/13)     0%  (0/13)
0.88      62% (8/13)     0%  (0/13)
0.90      46% (6/13)     0%  (0/13)
0.94      31% (4/13)     0%  (0/13)
```

结论与取舍：

- 0.86–0.88 是"零误命中"的安全平台；0.84 仍有 3 条误命中（`nm-en-2` 是
  "reset my password" vs "reset my username" 这类，正是最不该命中的那种），0.90 起真命中
  开始崩塌。
- 工具推荐 0.88，代码默认 **0.86**：语料里最紧的一对真同义改写实测 0.8819，只比 0.88 高
  0.0019——那是刀刃上的余量，不是余量。取平台的下沿保留约 0.02 的余量，而误命中率在语料上
  仍为 0。
- 阈值随 embedder 变：换成真实 embedding 模型后平台会整体移动，所以它是配置项
  （`cache.threshold`），不是常量。`DefaultThreshold` 只是"离线 embedder + 这份语料"下的
  实测值，替换 embedder 必须重跑 `cmd/measure-m2`。

### 9.5 两个 Store、一套一致性测试

`cache.Store` 有 memory（默认，进程内 LRU）与 Redis（跨副本共享）两种实现，同一个
`TestStoreConformance` 对两者都跑。三处刻意的设计：

- **向量与条目分开存**：`ig:cache:<scope>:vec` 是紧凑的 base64 float32 hash，
  `...:meta` 才是 JSON。语义检索只读 vec hash，只对胜者取 JSON。
- **过期按条目**（`...:exp` ZSET 的 score 是 expiresAt），不是 key 级 `EXPIRE`：
  否则一个繁忙提示词会顺带延长同一 scope 里另一条陈旧答案的寿命。key 级 EXPIRE 只作为
  2× TTL 的兜底。
- **淘汰语义不同，且写在类型注释里**：memory 是真 LRU（`container/list`，因为
  Agent 的会话历史正是"近期性有意义"的访问模式）；Redis 只能按创建时间 FIFO
  （ZSET 报不出读取顺序）——"对近期性撒谎的缓存比承认自己按年龄淘汰的缓存更糟"。
  这一条差异是唯一被允许的差异，也由 `TestRedisStoreIsFIFOAndDocumentedSo` 钉住。

降级是硬要求：`NewRedisStore` 启动时 ping 一次（配错了要在启动时失败，而不是让每个请求
静默 miss）；运行中 store 出错则退化为 miss，`/metrics` 仍然 200（`cacheEntries` 在
store 报错时返回 -1，而不是把整个 `/metrics` 变成 500）。`internal/embed.Fallback`
让 embedding 服务挂掉时退化成词法匹配，而不是让缓存失效。

### 9.6 命中之后的账目：为什么不算上游消耗

`ObserveTokens` 只在**真的发起过一次上游尝试**时调用，所以重放不会虚增 provider 消耗；
命中时 `X-InferGate-Upstream-Name: cache`，`rec.usage` 仍然填上条目里的 token 数（日志里
看得见答案包含多少 token），但这笔 token 记在缓存自己的 `saved_tokens` 上。同理刻意不动
`rec.firstTokenSet`：首 token 直方图度量的是生成，把接近 0 的重放混进去，会让这个指标在
缓存用得最狠的时候"变好"。上游的响应头也不会被重放——那等于声称一个从未被调用的后端刚刚作答。

### 9.7 流式命中的重放

流式答案存的是**客户端实际收到的帧字节**（`sse.Frame.Raw`，保留 id、model 和 provider 自己
的分帧方式），上限 `max_body_bytes`，超限则放弃存储并把原因记进 `cache_reason`。重放时用
`sse.NewReader` 重新解析这些字节并逐帧 `WriteUpstream`，每帧 flush，所以重放是增量的而不是
一次性吐出。一个真实缺陷：`MirrorSSEHeader()` 在没有活的上游响应时什么都镜像不到，重放的
响应因此**没有 Content-Type**——所以重放路径显式设置 `sse.ContentType`。

### 9.8 验证：两个门 + 一份测量

- `cmd/verify-m2`（Go，103 条断言，CI 门禁）：在进程内起**真实 server**（路由器、熔断器、
  账目全在），覆盖 miss→exact→semantic、语料里所有不该命中的对、租户/模型隔离、
  bypass/refresh、策略上限、流式逐帧重放、过期与 scope 上限、Redis store 与 store 死掉后的
  降级、以及 `/admin/cache*`、`/stats`、`/metrics` 三个观测面。
- `scripts/verify-m2.ps1`（curl，真进程）：内存 store 与真 Redis 协议服务
  （`cmd/miniredis`，默认 :6399——刻意不是 6379，免得遮住本机真的 Redis）两条路径。
- `scripts/measure-m2.ps1`：命中率/误命中率、命中率节流后的 token 与成本节省、命中与未命中
  的端到端延迟对比，原始数据落在 `docs/baseline/m2-summary.json`。

实测口径下的结果（本机回环，上游是仓库内 mock，store=memory，阈值 0.86）：请求口径命中率
58.97%（23/39，13/13 对全部覆盖），13 对"近似但不同"的语料发 26 次请求**零命中**；
纯命中路径 P95 5.5028ms（p50 1.0132ms）vs 纯未命中 47.3147ms（p50 5.5327ms）；
节省上游 token 604；每 1000 请求成本 $0.036692 → $0.016（−56.39%）；杀掉上游进程后
26 次请求仍全部 200（21 精确 + 5 语义，5xx 为 0），而未预热的对照请求返回 502。
出厂配置 `min_prompt_chars: 12` 会跳过 26 对里 6 对（中文提示 7–8 个字），同一脚本在该配置下
测得 41.03%（10/13 对），两个数一起报——否则测的是长度门而不是匹配器。

这里也有一次"验证工具骗人"：第一版 `metricSum` 按**前缀**匹配 series，而暴露的 label 顺序
每个 family 不同（`infergate_tokens_total` 以 upstream 开头，`infergate_cache_hits_total`
以 kind 开头），于是它静默地读到 0——一个计数为 0 的指标和一个什么都没做的功能长得一模一样。
现在用 `metricValue(text, series, labels)` 做**顺序无关**的 label 匹配。同一个验收器第一轮的
7 条失败全部是期望写错或测试脚手架的 bug，没有一条是产品缺陷；这也是为什么验收器要先证明
自己会失败。

## 10. M3：token 与成本治理

目标：让"谁用了多少、还能用多少"成为一个**可以拒绝请求**的判断，而不是一份事后账单。

### 10.1 位置：在缓存查找之前，且只治理生成类路径

准入（`admitQuota`）发生在 `serve()` 里、缓存之前，这个顺序两个方向都是承重的：

- 开在缓存**之前**：命中是免费的，但不是**不可见**的——它照样占用每分钟请求配额，照样应该
  出现在租户报表里。所以先开租约，命中后用 `Usage{Requests: 1}` 结算（token 为 0）。反过来说，
  如果反了顺序，一个超预算的租户会继续从缓存里拿到答案，预算形同虚设。
- 只治理 `/chat/completions`、`/completions`、`/embeddings`（`isCompletionPath`）：把
  `/v1/models` 也计入配额，会因为一个零成本请求拒绝调用方，并且让 token 这个维度在没有任何
  prompt 的请求上变成纯粹的虚构。

请求路径只依赖一个 3 方法的接口（`gateway.QuotaGate`：`Enabled`/`Admit`/`Settle`/`Release`），
并有一行 `var _ QuotaGate = (*quota.Manager)(nil)`：接口漂移会**编译失败**，而不是在运行时
被一个类型断言悄悄吞掉。

### 10.2 预扣 + 结算：为什么不是"先用后记"

先用后记（先转发、拿到 `usage` 再累加）在串行世界里是对的，在并发世界里是错的：N 个并发请求
都会看到"今天还剩 100 token"，然后一起花掉。反过来先读后写（GET 判断、再 INCR）同样错，因为
读和写之间有一个窗口，两个副本会同时看到"还有余量"。

所以这里用**预扣**（reserve）：准入时按估算把额度先扣掉，请求结束后用真实用量**结算**
（settle）差额。预扣是"乐观记账"：宁可先多扣、后返还，也不允许先花、后发现超支。代价是估算
不准时要如实报告——见 10.5。

### 10.3 原子性：为什么没有 Lua，也没有 MULTI/EXEC

M3 计划里最初写的是"Redis Lua 原子扣减"。落地时改成了**单条命令原子**，原因是读代码读出来的：
`internal/redis` 的 `Do` 每次从池里取一条连接（`acquire`/`release` 成对出现，调用之间不保留会话
状态），所以 `MULTI`/`EXEC`/`WATCH` 序列会跨连接，**根本不是事务**；一个 check-then-write 序列
在那个客户端上就是竞态，而且看起来像正确。要在 mock 里支持 Lua，还得写一个解释器。

现在每个维度的预扣就是**一条** `INCRBY`：读改写发生在 Redis 内部，两个副本不可能同时看到
"100 里已用 40" 然后都放行。窗口不是固定 key 上的 TTL，而是**key 的一部分**
（`ig:quota:<tenant>:day:<YYYYMMDD>:tokens`），因为"按 TTL 过期的日桶"会被一个迟到的请求
重新创建，从而静默重置当天预算；窗口进 key 之后，读"今天用量"就是一个普通 `GET`，跨天也不可能
竞争。只有三种情况才发 `EXPIRE`，且只在**首次自增**时（`delta > 0 && total == delta`，见
`internal/quota/redis.go`）：无条件刷新会让一个已经结算回零的计数器重新获得整窗寿命。

租户名是**调用方控制**的（`X-InferGate-Tenant`），而它是 key 的一部分，所以进 key 之前必须转义，
且转义必须是**单射**：`sanitise` 把 `_` 写成 `__`、其它非 `[A-Za-z0-9.-]` 字符写成 `_x` + 固定
六位十六进制（`acme:inc` → `acme_x00003ainc`，`acme_inc` → `acme__inc`，于是两者不共用账本）。
固定宽度是承重的：变宽编码下 `U+10FFF` 接 `"ff"` 与 `U+10FFFF` 接 `"f"` 会编成同一个 key。第一版
把每个非法字符都映射成 `_`，于是配置里一个叫 `acme:inc` 的租户会**用别人的计数器**判断自己的
预算（策略按原名查、计数按转义名记）——这不是理论问题，两个门禁都复现过。

"当天"是 **UTC 当天**（`untilDayEnd` 自己先转 UTC），所以在这台 UTC+8 的机器上，日预算在本地
08:00 重置；这个副作用写在 docs/ACCEPTANCE.md 的 M3 验收口径里，因为它会让"今天的用量"和运维的日历对不上。

`SETNX`/`SETEX`/`INCR`/`INCRBY`/`DECR`/`DECRBY`/`MGET`/`HINCRBY`/`ZINCRBY` 是这一轮给
`internal/mockredis` 补上的，走的都是真实 RESP2 派发路径：验收器因此在协议层被测过，而不是测一个
Go 方法。

### 10.4 降级梯子：超预算不等于失败

`on_exceed: degrade` 且配了 `downgrade_model` 或 `max_tokens_cap` 时，超预算的请求**仍然放行**，
只是被改写：

- `downgrade_model`：改写请求体里的 `model`（大小写不敏感比较，已经是目标模型就原样发）。
- `max_tokens_cap`：把完成上限**压到** cap，绝不抬高——把调用方没要的长度加上去，等于网关替
  调用方花钱。body 用的是哪个拼写就改哪个（`max_completion_tokens` 优先）：两个字段同时出现
  会让不同 provider 作出不同解释。

关键细节：降级请求**保留预扣**（降级后的请求仍然是一次请求、仍然要结算），结算会把便宜模型没用完
的额度还回去。若两个杠杆都没配，配置校验期就报错
（`on_exceed is degrade but neither downgrade_model nor max_tokens_cap is set`），而不是在运行时
降级成一个什么都没改的请求。

### 10.5 估算与超支：把不准的地方说出来

预扣用的是估算：prompt 按 `len(body)/chars_per_token`（默认 4，CJK 与长标识符代码会**低估**），
completion 用调用方自己声明的上限，没声明就用 `estimate_completion_tokens`（默认 256）；成本由
**配置里的价目表**算出（网关看不见发票，只能乘）。

估算必然不准，所以差额被显式记账而不是藏起来：结算时 `actual - reserved > 0` 记进
`overshoot_tokens`/`overshoot_cost_micros`，`< 0` 记进 `released_tokens`（成本维度的返还进
`released_cost_micros`，不混进 token 计数）。`overshoot` 就是"这个软限额到底有多软"的度量——
一个持续增长的 overshoot 意味着估算系统性偏低，而这个结论只能从这两个计数器读出来。三个审计
计数是同一条账本的三个视角而不是三个独立计数器，所以 `reserved − released + overshoot == settled`
对每一笔预扣都成立：`settled` 是**真实扣掉**的量，不是 `overshoot` 的别名。这条不变量是实跑
发现的机制性 bug 的产物——第一版把成本条目的返还加进了 token 计数器，屏幕上出现"释放了 1014 个
token"而这次请求只预扣了 274 个（1014 = 260 个真实 token + 754 微美元）。

记账的单位是**条目**（一次预扣按维度展开成若干条），不是请求：同时配了日 token 与每会话 token 的
租户，一次带 session 的请求会预扣两个 token 维度，`reserved`/`released`/`settled` 就各记两条；被
拒绝的请求也同时出现在两侧（它预扣了条目，又在同一次准入里全部释放）。第一版按请求记 `reserved`
（`Admit` 每请求加一次 `est.Tokens()`）而按条目记 `settled`，于是恒等式只在"每租户只有一个 token
维度"的网关上成立；两个验收门禁都复现过这个失衡（reserved 20741 / released 26665 / settled 7670），
现在两个门禁都在混合流量上直接断言恒等式。

两处刻意的账目选择：

- 缓存命中按 `Usage{Requests: 1}` 结算，token 为 0。按条目里存的 token 收费，等于向租户收取
  provider 从未生成的 token，正好和缓存的用途相反。
- 流被中途截断时结算**实际发出**的 token，未用完的预扣释放（体现在 `released_tokens`）：宁可
  少收，不可多收。

### 10.6 存储失败时：fail-closed 是默认

计数器读不到时，默认**拒绝**（503 + `infergate_quota_unavailable`）：读不到的预算不是预算，一个
在 Redis 抖动时静默停止限额的网关，恰好会在没人看着的时候无限花钱。`fail_open: true` 是给
"可用性优先于成本控制"的部署准备的，而且无论哪种模式，响应头都会如实说明是哪种模式答的。

两种模式都要出现在**决策计数器**里：fail-closed 的 503 记 `rejected`，fail-open 的放行记
`allowed`。第一版只记 `store_errors`，于是在计数器坏掉的那一刻，`allowed + degraded + rejected`
恰好少算了网关真正服务掉的请求——而那时这几个数字是唯一的证据。

启动期与运行期分开：Redis 连不上时**启动失败**（`server: quota: redis at ...: ...`），而不是退化成
进程内存——那会让 N 个副本各自允许一整套日预算，超额要到发票到了才被发现。运行期的降级属于请求
路径（估算失败、结算失败只记日志，不改变已经作出的判断）。这条规则和 M2 的共享缓存完全一致。

### 10.7 异常告警：只报警，不拦流量

`anomaly_ratio`（默认 3）比较的是"今天"与"此前有流量那几天的均值"。触发时只做两件事：记一条
日志（进入/退出告警态各一次，每租户每分钟最多一次）并把 `Stats.Alerts` 加一。**它不会拒绝任何
请求**：花钱突然变多通常是真实业务（上线、爬虫、新客户），一个因为"看起来不寻常"就拒绝流量的
网关会把一次发布变成一次故障。要拦流量的是 10.2 的预算，不是这个比值。

### 10.8 验证：两个门 + 一份测量

- `cmd/verify-m3`（Go，进程内真 server，470 条断言）：内存与 Redis 两个 store、日 token / 日成本 /
  每分钟 / 每会话四个维度、降级与限长、结算/超支/释放、键隔离（含 `:` 的租户名不得记到别人账上）、
  fail-closed 与 fail-open、非生成路径不被治理、以及 `/admin/quota`、`/stats`、`/metrics` 三个
  观测面。
- `scripts/verify-m3.ps1`（curl，真进程 + 真 Redis 协议服务，323 条断言）：同样的主张在真二进制上
  再走一遍。
- `scripts/measure-m3.ps1`：治理路径的开销、预扣与真实用量的偏差、以及预算能挡下多少失控消耗，
  原始数据落在 `docs/baseline/m3-summary.json`（脚本自身 57 条断言）。

证据口径和 M2 一样：拒绝由**上游再也不被调用**证明，降级由**后端实际收到的请求体**证明，账目由
计数器读回的值证明，而不是由网关自己的说法证明。

---

## 11. M4：分层路由与本地推理

### 11.1 位置：给已有的排序再加一个维度，而不是加一层

M4 没有引入新的包，也没有新的调用链。路由决策仍然是 `internal/router` 里那一次排序：候选先按
模型与能力筛（8.2），再按健康度、策略权重排（8.3）。分层只做了两件事：

1. `UpstreamConfig.Tier`（`local` / `cloud`，空等于 `cloud`）挂在**上游**上——一个后端属于哪一层
   是部署事实，不是请求属性；
2. `routing.tier_policy` 挂在**网关**上——什么算"简单请求"是业务口径，需要能改而不用改后端配置。

排序规则是"健康的优先，然后优先层在前，然后 priority，然后配置顺序"。这一条看似平淡，却决定了
M1 的能力没有被削弱：分层是**排序**，不是**过滤**，所以跨层故障转移天然还在（11.3）。

### 11.2 分类：为什么用长度，而不是模型名或再训一个小模型

`local_max_prompt_tokens` / `local_max_completion_tokens` 是仅有的两个判据，比较用严格大于，`0`
表示该维度不设限。选它有三个具体理由：

- **零额外 I/O**：估算只做一次整数除法 `len(serialised messages)/4`；把它接到一次 embedding 或
  一次分类调用上，本地优先带来的成本节省会被分类本身吃掉。
- **口径与配额一致**：这个 4 就是 `config.EstimateCharsPerToken`，同一个常数也用在 M3 的预扣里。
  于是"长 prompt"在路由和配额两个地方是同一个意思，不会出现"路由认定简单、配额认定昂贵"的分裂。
- **确定性可测**：同样的请求体永远得到同样的层，验收脚本才能用一个边界值（1600 / 1604 字节）
  把 `<=400` 与 `>400` 两侧钉死。

模型名做不了这件事：本地模型和云端模型从来不同名（`local-chat` vs `deepseek-chat`），而 M1 的
候选筛选是**先按模型名筛的**（8.2）。所以"分层"必须发生在模型索引之后，而想要两条路径真的可比，
配置里就得让两层都能接住同一个模型名：示例配置给两层都写了 `models: ["/"]`（catch-all），
真实部署里则应该给 vLLM 起 `--served-model-name` 与云端同名。

### 11.3 排序与故障转移：难请求也要把本地留在后面

分类只在**健康**候选之间决定谁排第一，于是：

- 简单请求：本地在前、云端在后 —— 云端仍然是被压着的那一层；
- 难请求：云端在前、本地在后 —— 顺序反过来，但本地仍在候选里，所以云侧 5xx 时还能落到本地；
- 熔断打开的那一层**排到最后而不是被删掉**（8.3 的规则照旧）——"上次挂了"不等于"这次不能用"；
- 声明了能力的请求仍走 8.2 的能力筛选：`tools` / `vision` 在 `cloud_capabilities` 里，一个只有
  `chat` 的本地副本会因此被排除在候选之外，而不是"被排到后面然后 400"。

决策面仍然是那一个 `reason` 字符串（`strategy=tiered tier=local reason=simple request prefers this
tier` 等四种形态），没有新增指标标签、没有新增决策字段。理由是：多一个字段就要多一张表、多一处
派生逻辑，而排障时真正要回答的问题只有一个——"这个请求为什么去了这一层"——一行字符串答完了。

### 11.4 真实本地推理的环境：三次依赖事故

本地层不是 mock，而是在这台 8 GB 笔记本显卡上真的跑 vLLM。三件事必须写下来，因为它们都是
"照文档做就会踩"的坑：

1. **CUDA 大版本不向前兼容**。`pip install vllm` 解析到的是 CUDA 13 轮子（vllm 0.31.0 → torch
   2.13.0 → `nvidia-*-cu13`），而本机驱动 560.94 对应 CUDA 12.6。CUDA 的大版本要求驱动 ≥ 对应版本，
   小版本才向后兼容。定版到 `vllm==0.11.0`（→ `torch 2.8.0+cu128`）之后 `torch.cuda.is_available()`
   立刻为真。**结论**：在"驱动版本比框架新"的机器上，装之前先确认 `torch.version.cuda` 与驱动上限，
   否则会得到一堆看起来像显卡故障的运行时错误。
2. **vLLM 对 `transformers` 只有下界**。它允许装到 5.x，而 5.x 删掉了
   `all_special_tokens_extended`，于是启动时抛 `AttributeError`。定版 `transformers>=4.55.2,<5`
   （4.57.6）后正常。这个错误发生在 tokenizer 初始化，日志里离"显卡"很远，很容易误判。
3. **Inductor 需要 C 编译器**。默认配置下 vLLM 会走 torch.compile，缺 gcc 时报
   `Failed to find C compiler`。镜像里装上 `build-essential python3-dev` 即可；这条是纯环境问题，
   不该用 `--enforce-eager` 绕过——那会改变测量结果本身。

镜像源同样是环境的一部分：本机**无法访问 pypi.org 与 huggingface.co**（超时），PyPI 走清华源、
HF 走 `hf-mirror.com`；而 `hf download` 在本机反复卡死（一次卡在 732 MB、20 秒零增长），
ModelScope 同一条 20 MB 区间的实测速度是 hf-mirror 的 6–12 倍（1.86 MB/s vs 11.91 / 22.16 MB/s），
换源后 1.54 GB 权重 2.3 分钟下完。**教训**：下载器不是"能用就行"的细节，它会决定这个里程碑是
半小时还是三小时。

### 11.5 量化对比的口径：把不可比的地方先说出来

三个变体（fp16 / AWQ / GPTQ-Int4）跑**同一份 12 条固定 prompt**（sha256 记录在产物里）、同一套
服务参数（`--gpu-memory-utilization 0.85`、`max-model-len 4096`）、同一个流式客户端。几点刻意为之：

- **首字延迟用真流式**：客户端用 `read1()` 逐块读 SSE，TTFT 是"第一个内容帧到达"的时间，而不是
  "整段生成完"的时间；
- **一致性不是准确率**：与 fp16 输出比较的是 `exact_match_rate` 与 `mean_token_overlap`，产物里
  明确写着这两个数字**不能**当作精度指标——12 条 prompt、单次采样，只能说明"量化后输出没崩"；
- **显存是差值**：`memory.used` 通过 `nvidia-smi` 读，包含 Windows 桌面占用的 1.7–1.9 GiB，所以
  只报加载前后的 delta，且明确 ±50 MiB 噪声；
- **不读小于 5% 的差异**：单并发、单轮、共享散热受限的笔记本 GPU，吞吐差异在几个百分点内不具
  区分度。产物里把这条写成限制，而不是让读者自己猜。

实测（RTX 4070 Laptop 8 GB，WSL2，vLLM 0.11.0 + torch 2.8.0+cu128，12 条固定 prompt，单并发单轮）：
fp16 首字 P50 35.2ms / 端到端 P50 1758.5ms / 41.5 tok·s⁻¹ / 权重 3.09 GB；AWQ 28.1ms / 699.5ms /
100.7 tok·s⁻¹ / 1.61 GB；GPTQ-Int4 28.7ms / 723.6ms / 95.3 tok·s⁻¹ / 1.15 GB。两个量化的吞吐是
fp16 的 2.3–2.4 倍，而彼此只差 5.6%（在噪声门槛之内）。质量上，12 条里与 fp16 完全一致的分别是
AWQ 0 条、GPTQ 1 条，token 重合 0.444 / 0.460——但考点不在这个数字：`4873+6259` 那条 AWQ 把差值
答成 `-1386`（fp16 与 GPTQ 都是 `1386`），"澳大利亚人口"那条 fp16 答 4.1、AWQ 答 40、GPTQ 答 2.8。
结论是"量化让 8 GB 卡上的本地推理从不可用变成可用，并且会引入数字级的漂移"，而不是"AWQ 比 GPTQ 好"。
显存增量（6169 / 6921 / 7353 MiB）看起来反而更大，那是 `--gpu-memory-utilization 0.85` 预分配
KV cache 的结果：三个变体都填满同一张卡，所以这个数不能当"模型占用"读，权重文件大小才是。

### 11.6 顺手修掉的死配置：`routing.fallback_model`

写 M4 的配置注释时发现，`routing.fallback_model` 被解析（config.go）、被 `/admin/upstreams` 报出
（server.go）、被 README 和示例配置宣传，**却没有任何代码读过它**——一个"文档里有、行为上没有"
的开关，比没有这个开关更糟。它的语义被实现成：只有当目标既不服务请求的模型、又确实服务 fallback
模型时才改写模型名；catch-all 目标永不改写；改写后的候选按 fallback 模型的单价计费。这样它才不会
和 11.2 的分层口径打架——分层靠健康与层序决定"去哪一层"，fallback 只决定"到了那一层之后用哪个
模型名"。

### 11.7 验证：两个门 + 一份测量

- `cmd/verify-m4`（Go，进程内真 server，877 条断言，16 组）：分类与容量上限的边界、层序、能力
  筛选、优先级、跨层故障转移、`/admin/upstreams` 的层字段，以及一次真实的本地/云端分流与省钱核算。
- `scripts/verify-m4.ps1`（curl，两个假上游层，127 条断言）：同样的主张在真二进制上再走一遍，
  **不需要 GPU**——真实 vLLM 层由测量脚本负责，验收门禁不能依赖一张显卡。
- `scripts/measure-m4.ps1`：真实 vLLM 的三变体量化对比 + 一次真实的分层运行（本地 vLLM + 云端
  mock），产物落在 `docs/baseline/m4-summary.json`。

分层本身的价值主张是"简单请求留在本地"，所以证据必须是**分流比例**与**被省下的云侧花费**，而不是
一个自证的计数器：分流比例来自每个上游各自的 `/stats` 计数，花费由 `internal/gateway/pricing.go`
的单价折算——两样都是 M1/M3 就已经存在的观测量。

分层实测（`scripts/measure-m4.ps1 -SkipBench`，本地层是 WSL 里真 vLLM 的 AWQ 权重，云端层是仓库内
`cmd\mockupstream`；产物 `docs/baseline/m4-summary.json`，本次 63/63 断言）：

- 16 个请求、两种固定形态各 8 条：短请求（约 42 字符）**8/8 落本地**，长请求（2444 字符）**8/8 落
  云端**；归属逐请求按响应的 `X-InferGate-Upstream-Name` 读，`/stats` 的 per-upstream 增量同为
  local +8 / cloud +8——是网关数的，不是脚本自己记的。
- 客户端墙钟：本地 p50 560.4 / p95 946.2ms，云端 p50 71.8 / p95 135.2ms；网关 `/stats` 对整批 16 个
  请求是 p50 342.6 / p95 851.8ms。**层间差不是"本地比云端慢"**：云端是秒回的 mock，本地是真模型在
  笔记本 GPU 上逐字生成；而且本地臂在同一台机上两分钟内从 784.5ms 变到 560.4ms（−29%），
  所以只能引用同一次运行内部的两层对比。
- token 与花费：本地层 312 prompt / 421 completion，云端层 3096 / 184。单价取自 `pricing`（按模型名
  定价，两层都答 `local-chat`，in 0.27 / out 1.1）。被本地接住的 8 条按云侧单价折 **0.00054734 USD**，
  全批 16 条若全走云侧是 0.00158566 USD，所以这次分流打掉云侧账单的 **34.52%**；云端层自身仍是
  0.00103832 USD。本地层现金边际成本为 0 但记账按云侧单价，所以它是"账面替代"而不是账单
  （`tiering.cost.note` 写清楚了，避免把 0.00054734 读成"少付了这么多钱"）。
- 同一次运行也验证了"本地层真的在被调用"：每个本地答案非空且带 `usage`，说明走的是真模型的生成
  路径，而不是一个被 mock 掉的分支。

## 12. M5：全链路可观测与压测基线

### 12.1 位置：先让指标能被 Prometheus 抓走，再谈"可观测"

M0–M4 的 `/metrics` 只有计数器和 gauge，一个直方图都没有：`infergate_request_duration_seconds_sum`
是个 TYPE counter 却带着 `_sum` 后缀，既没有 `_count` 也没有 `_bucket`，所以任何面板都算不出分位数，
只能看平均值。M5 第一件事就是把这个"看起来像直方图"的家族换成真直方图（**同名**，避免面板与查询
两套名字），并补上三个本来就该有、却一直缺失的家族：`infergate_upstream_attempt_duration_seconds`
（每次上游尝试的耗时一直在记录，从来没有导出）、`infergate_first_token_seconds`（TTFT 直方图，
和已有的 `_mean` gauge 并存）与 `infergate_completion_tokens_per_request`。

为什么不引第三方 client 库：这个仓库从 M0 起 `go.mod` 就没有任何依赖，为了一个直方图破例不值得。
固定分桶 + `+Inf` 累积桶不到 200 行，而且"`count == Σ bucket`、`+Inf` 桶等于总数"这两条不变式可以
被单测直接钉死。分桶口径按真实量程选：延迟 1ms…10s（本机 mock 的 ~1ms 到真机 vLLM 的秒级都要覆盖），
TTFT 5ms…2.5s，每请求 completion token 1…4096。负值与 NaN 归零——直方图是观测器，不是校验层。

有界性同样是这一节的一部分：`Recorder.requestLatency` 原本是一个只增不减的 slice（唯一的清理点是
`Reset`），`/stats` 每次抓取还把它复制排序五遍。改成 65536 样本的环形缓冲，并在 `/stats` 里报出
`latency.window` / `latency.dropped`：**丢样本这件事必须可见**，否则"P99 变好了"可能只是样本被丢了。
同一次抓取只排序一次。

### 12.2 trace 模型：一条请求 = 一个 server span + 每个上游尝试一个 client span

- 复用 W3C `traceparent`，不自造 header：上游是 OpenAI 兼容服务，将来接任何带 trace 的客户端或
  网关都不需要翻译层，网关自己也不被迫成为 trace 的起点。
- 网关不重新生成 trace id：入站有 `traceparent` 就跟随（trace id 原样、sampled 位沿用），没有就新建
  root。**中途换 trace id 会让"客户端 → 网关 → 上游"不再是一条 trace**，这是 trace 模型里最容易
  犯又最难发现的错误。
- 流式不能每帧一个 span：一次 SSE 响应有几十到几百帧，帧级 span 会把 span 数放大两个数量级、先把
  存储打爆，再让真正的慢点淹没在噪声里。流式就是**一个** client span，帧数 / 字节数 / 首字延迟作为
  属性挂上去。
- 请求级证据面：`/admin/traces`（最新优先的摘要列表，带 limit）与 `/admin/traces/{id}`（取回完整
  trace）。按 **request id 也能取回**是刻意的：日志里能看到的就是 request id，能按它回放，trace 才
  有实用价值，否则只是一份好看的 JSON。
- 存储有界：环形缓冲（`tracing.capacity`，默认 512），被挤掉的条数必须在接口里报出来。trace 是诊断
  工具，不是账本。

### 12.3 导出：JSONL + 手写 OTLP/HTTP(JSON)

OTLP/HTTP 允许用 protobuf 的 JSON 映射直接 POST 到 `/v1/traces`，所以"接 OpenTelemetry"不必引入 SDK：
一个标准库的 `net/http` POST 就是合法的 OTLP exporter。文档里如实写成"手写 OTLP/HTTP(JSON) 导出，
与标准 collector 的 :4318 兼容"，而不是含糊成"接入 OTel SDK"——**能用**和**是官方 SDK**是两件事。

导出走后台 goroutine + 有缓冲 channel：请求路径绝不因为采集端慢而变慢，导出失败只记日志并丢弃，不影响
响应。JSONL 是离线复盘的兜底：采集端不在，也能把一次压测的 trace 原样存下来。

### 12.4 两个被修掉的观测缺陷（都会让面板说谎）

- **TTFT 与流式帧/字节被记了两遍**：`attemptUpstream` 尾部记一次（`internal/gateway/proxy.go`），
  `ServeHTTP` 的 defer 又记一次（`internal/gateway/proxy.go`）。保留 defer 那一处——它同时
  覆盖缓存命中路径，并且把样本归给真正服务这次请求的上游。
- **上游跳的 request id 与网关日志里的不是同一个**：`buildRequest` 又调了一次 `requestID(r)`
  （`internal/gateway/proxy.go` 的 `buildRequest`），客户端没带 id 时，上游看到的是一个新造的 id。改成把
  `rec.requestID` 传进去，一次请求只有一个 id。

### 12.5 瓶颈排查的方法：先证伪"网关是瓶颈"

`-diag` 让同一份客户端在同一台机器上跑五个对照：直连后端、默认连接池的 `httputil`、按网关参数调过的
`httputil`、手写最小透传、真网关。它回答的不是"网关有多快"，而是"**网关相对一个什么都不做的透传贵多少**"。

结论是反直觉的：真网关在进程内比手写最小透传还快（23.9k vs 22.2k QPS，p50 1.14ms / p95 2.71ms），
而直连基线自身的轮间抖动就有 27%——所以"网关逻辑是瓶颈"这个说法不成立，之前那个 ~6k QPS 的
"单实例上限"是测量方式造成的假象。真正有代价的是**每请求日志**：同一外部拓扑下 `log.level: error`
相对 `info` 多约 18% QPS、p95 少约 4ms。

记数的纪律写在这里，因为它比数字本身更值钱：所有臂必须在**同一次调用里交错跑**（轮次在外、配置在内），
并且每次都带一个"只连后端"的臂作为机器负载的证人。本次排查中这个证人臂就从 31.3k 掉到 8.7k（并发跑
构建/基准的子代理把机器吃满）——如果只记网关臂，就会把一个纯负载假象写成"网关退化"。

### 12.6 验证：两个门 + 一份测量

沿用 §8.5 的分工：**进程内门能断言任意不变式，但证明不了"网关真的把头发出去了"；进程门能证明线路上
真的发生了什么，但写不出桶的累积性**。所以 M5 的证据面有三个，各自只回答自己能回答的那部分。

- **进程内门 `cmd/verify-m5`（420 条断言）**：把 Prometheus 文本契约本身当断言对象——每个家族都有
  `# HELP` / `# TYPE`、桶是累积的、`+Inf` 桶等于 `_count`、恰好落在边界值上的观测进边界桶、负值与 NaN
  计数但不污染 `_sum`；`/stats` 的分位数与最近秩定义一致且 P50 ≤ P95 ≤ P99，`latency.window` / `dropped`
  与环形缓冲的容量、丢弃数逐个对齐；同一 request id 的采样决策幂等；导出载荷逐字段断言（OTLP 的
  `kind` 是枚举**数字**而不是字符串、`parentSpanId` 指向 root、`status.code`、`startTimeUnixNano` 是十进制
  字符串）。这些不变式只有进程内断言写得死：对着几个真实样本反推"桶是否累积"是做不到的。
- **真实进程门 `scripts/verify-m5.ps1`（211 条断言；203 是加入 8 条解析器自检之前的数字）**：两个真 mock 上游 + `cmd/mockcollector`（本仓库
  自带的 OTLP/HTTP 采集端）+ 真 `curl.exe`。它证明的是跨进程的那部分：网关发给上游的 `traceparent` 与
  调用方带的是同一个（从采集端**记录的真实请求头**读回，而不是从网关自述里读）、OTLP 载荷真的 POST 到了
  配置的端点、JSONL 行数与 `/admin/tracing` 的 `export_stats` 一致、有界存储淘汰后旧 trace 取回是 404、
  以及熔断/后端全挂时失败请求照样有 trace（span 状态为 error + reason）和 Warn 日志。
- **一份测量 `scripts/measure-m5.ps1` → `docs/baseline/m5-summary.json`**：见 §12.7。

### 12.7 压测基线：先量噪声，再量网关

`scripts/measure-m5.ps1` 在一次调用里交错跑 8 个臂（轮次在外、臂在内，每臂 3 轮取中位），每臂 6 个相位
（非流式 / 流式 × 并发 8 / 32 / 128，每相位 3000 请求 + 300 预热）：

| 臂 | 是什么 |
| --- | --- |
| `direct` | 客户端直连 mock 后端——**证人臂**：它自己不动，机器状态就写在它身上 |
| `gateway` | 单实例网关（`:18999`），本轮所有对照的基准 |
| `second` | 同一份配置的第二个实例（`:19000`），用来量"两个一样的实例之间能差多少" |
| `gateway-scraped` | 网关 + 每 100ms 轮询 `/stats` 与 `/metrics` 的共置抓取器 |
| `traced-otlp` | 只开 OTLP 导出（本地 `cmd/mockcollector`），`sample_ratio 1.0` |
| `traced-full` | OTLP + JSONL 两个 sink |
| `horizontal-2` / `horizontal-4` | 一个客户端轮流把请求发给 2 / 4 个实例（`loadtest -urls`），客户端开销只算一份 |

所有网关跑 `log.level: error`：§12.5 已经证明每请求日志值约 18% QPS，那是另一个变量，不能被混进
tracing 的成本里。

| 臂 | 非流式 c=8 / 32 / 128 QPS（P95 ms） | 流式 c=8 / 32 / 128 QPS（P95 ms，TTFT P95 ms） |
| --- | --- | --- |
| `direct` | 5834（2.10）/ 5688（7.42）/ 5623（26.00） | 1500（6.41，1.51）/ 3448（11.78，1.55）/ 3437（39.31，1.23） |
| `gateway` | 4462（2.39）/ 4560（7.88）/ 4382（32.82） | 1366（6.93，1.55）/ 2731（14.15，1.63）/ 2750（56.70，1.59） |
| `gateway-scraped` | 4110 / 4255 / 3959 | 1320 / 2656 / 2728 |
| `traced-otlp` | 3922 / 3666 / 3822 | 1338 / 2690 / 2701 |
| `traced-full` | 3847 / 3872 / 3720 | 1327 / 2647 / 2731 |
| `horizontal-2` | 4347 / 4286 / 4415 | 1332 / 2749 / 2829 |
| `horizontal-4` | 4387 / 4044 / 4340 | 1317 / 2637 / 2749 |

**这一次，网关是限速的那一半**：非流式 c=128 少 22.07%（4382 vs 5623 QPS，P95 +6.82ms / P99 +4.59ms），
流式 c=128 少 20.00%（2750 vs 3437，P95 +17.39ms / P99 +14.96ms）。

**然后必须说清楚这些数字不能怎么读。** 同一条命令、同一台机器、相邻两次全量运行（602s 与 385s）：

- `direct` 非流式 c=8：1909 → 5834 QPS（**3.06×**）；`gateway` 同点 2206 → 4462（2.02×）；
  流式 c=128 的 `direct` 1809 → 3437（1.90×）。
- 于是"网关比直连慢多少"的**符号都会翻转**：前一次非流式是 +15.6 / +16.2 / +16.3%（网关更快），
  这一次是 −23.5 / −19.8 / −22.1%。这不是舍入级的抖动，是同一份代码在两种机器状态下得出的两种结论。

所以在这台机器上，**绝对值不可跨运行引用，"谁比谁快"也不可单独引用**；能读的只有"同一次运行内、
与同轮噪声带比较"的差值，而带子是每轮跑出来的（`QPSNoise_pct`），不是估的。§12.5 的证人臂教训
（31.3k → 8.7k）在这里从脚注变成了必答题。另外 c=128 的流式臂其实**受限于压测客户端**：客户端观测均值
45.60ms 而网关自报 15.05ms，说明墙钟大半花在客户端自己身上（脚本把这类相位单独列进 `limitations`，
共 8 个）。

**三个在噪声之上、因此可以引用的结论**

1. **水平扩展在单一后端下不涨吞吐**：2 实例相对单实例的加速比是 0.94–1.03×，4 实例是 0.89–1.00×，
   即"加了实例，吞吐没动"——所有实例共享同一个 mock 后端。`second` 臂（同配置的另一个实例）之间
   就差 −3.11% … +4.06%，这就是这台机器能分辨的下限，所以线性度只能报"≤ 噪声"（2 实例 47–51.5%、
   4 实例 22.2–25.0% 的理想线性度），不能报成"4 实例接近 4×"。
2. **tracing 的成本取决于负载形态**：非流式在 `sample_ratio 1.0` 下三个档位都超出同轮噪声带，
   −12.09% / −19.60% / −12.79% QPS（P95 +0.31 / +2.36 / +4.71ms）；流式则基本落进带内
   （−2.02% / −1.50% / −1.77%，只有 OTLP+JSONL 在 c=8 的 −2.80% 略超 2.75% 的带宽）。
   再加第二个 sink（JSONL）在全部六对比较里都不产生可测开销（−1.93% … +5.63%，全部小于带宽），
   原因正是 §12.3 的设计：`Export()` 只入队，写盘与发网在后台 worker 上，采集端不可达只会涨 drop 计数。
3. **被持续抓取是有代价的，但 100ms 常轮询是上界**：6 对里 3 对超出噪声，最大 −9.65% QPS
   （非流式 c=128，P95 +5.75ms）。抓取器自己也被量了：请求 10 端点 GET/s，实际做到 9.702（0.9702×，
   三轮 9.911 / 9.599 / 9.601，离散 3.25%），因为共置抓取器是"每次迭代起一个 `curl.exe`"。
   真实 Prometheus 是 5–15s 一次，所以这张表是**上界**，且差值里还有一部分是抓取进程自己在抢 CPU。

一致性侧的证据面：144 个相位 `errors=0`；一轮 `gateway` 臂期间 `:18999` 的 `infergate_requests_total`
增长 19800 = 18000 派发 + 1800 预热（每个请求恰好 +1，没有重试或故障转移放大计数）；6 个实例的 `/stats`
都报 `window=65536` 且 P50 ≤ P95 ≤ P99、无 open 熔断、都声明了 `# TYPE infergate_stream_bytes_total`
（§12.4 那个被修掉的暴露缺陷的回归护栏）；OTLP `exported=59400` / `failed=0`，采集端收到 118714 次 POST
**全部**落在配置的 base + `/v1/traces`（重复路径 0 次）；JSONL 59400 行与 `export_stats.jsonl.written`
相等且每行都能解析出 `trace_id`。

**这份基线是什么、不是什么**：后端是仓库内的 `cmd/mockupstream`（不是任何 Provider 的基准），
全程单机回环（路径里没有网络），`log.level: error`（所以 §12.5 的 18% 日志成本不在这张表里），
导出器是异步的（量的是"建 span + 入队"而不是同步写），`sample_ratio 1.0`（最坏情况而非采样部署），
抓取负载是上界，固定请求数意味着机器越快相位越短（本次 c=8 的非流式相位约 0.5s）。它证明的是
**测量方法与相对关系**，不是容量承诺。

## 13. M6：幂等重放、会话账本与能力发现

目标：让"重试"不再等于"重新计费"，让"这段对话花了多少"成为一个能查的事实，并让 Agent 在发请求
之前知道自己能用什么。

### 13.1 位置：在准入之后、缓存之前

`idempotencyBegin`（`internal/gateway/idempotency.go`）夹在配额准入与缓存之间，两侧都是承重的：

- 放在准入**之后**：重放和缓存命中一样是调用方发起的请求，照样占用每分钟请求配额、但不消耗上游
  token，所以结算走 `Usage{Requests: 1}`（`internal/gateway/idempotency.go` 的 `serveReplay` 里那段注释）。反过来放，一个超预算的租户
  可以靠重放继续拿答案，配额就成了摆设。
- 放在缓存**之前**：重放比缓存命中是**更强**的断言——调用方问的是"我自己那次操作的结果"，不是
  "有没有人问过类似的问题"。顺序反了，一个带 key 的重试会先被语义缓存接走，"这次是重放"这件事
  在账目里就消失了。
- 非生成类路径带 key 时不报错，而是忽略并在响应上回 `X-InferGate-Idempotent-Store: skip`
  （`internal/gateway/idempotency.go` 的 `idempotencyBegin` 里那段 `!isCompletionPath` 分支）：
  一个给每个请求都盖 key 的客户端不该因此挂掉，但也不能让它以为自己
  受了保护——所以这个忽略是**可见**的。

### 13.2 身份：key 由调用方给，作用域是租户，指纹是请求本身

`KeyOf` 规范化 key，scope 取租户（没有租户头就是 `anonymous`），指纹是
`requestHash(method, path, body)` = `sha256(method \0 path \0 body)` 的前 16 字节
（`internal/gateway/idempotency.go` 的 `requestHash`）。method 与 path 进哈希是必要的：同一段 body POST 到 `/v1/embeddings`
和 `/v1/chat/completions` 不是同一个操作。

于是每个带 key 的请求有四种归宿：Proceed（我做这次活）、Replay（已经有人做完了）、ConflictBody
（同一个 key 换了 body）、ConflictInFlight（同一个 key 还在飞）。两种冲突都是立刻 409
（`infergate_idempotency_conflict` / `infergate_idempotency_in_flight`，后者带 `Retry-After: 1`），
**不排队也不并发第二次生成**——在途冲突回 409 而不是等待，正是这个 store 的意义：调用方稍后用同一个
key 重试就能拿到第一次的答案，而上游总共只被要了一次生成。

Proceed 时立刻回两个头（`internal/gateway/idempotency.go` 的 `idempotencyBegin` 里
`case idempotency.Proceed`）：回显 key，并把 `X-InferGate-Idempotent-Replay`
显式设成 `false`。让调用方能区分"这次是我干的活"与"你拿到的是上一次的答案"，而不是从"头不存在"
去推断——后者在一个只读了流的前几个字节的客户端上必然猜错。

### 13.3 记忆什么、不记忆什么

- **可重放性由 `replayableResponse` 判定**（`internal/gateway/idempotency.go`）：2xx 与 4xx 是确定性答案；
  5xx、超时、被取消的流一律 `Abort` 并释放占用——把一次瞬时失败钉死在一个 key 上，等于把这个操作
  永久变成失败，而调用方的重试恰恰是必须被允许再试一次的那个场景。
- **写入发生在响应写出之后**：`idempotencyComplete` 由 `serve()` 的 deferred recorder 调用
  （`internal/gateway/proxy.go` 的 `ServeHTTP` defer，那里调 `idempotencyComplete`），所以再慢的
  store 也不会拖慢一个答案。
- **超限的答案照常送达、只是不被记住**。`captureWriter` 是 tee 不是缓冲：每个字节立刻到客户端、
  `Flush` 转发、`Unwrap` 让 `http.ResponseController` 还能拿到 Hijacker；副本上限是
  `max_response_bytes + 1`，多出来的那一字节就是"答案没装下"的信号（`internal/gateway/idempotency.go`
  的 `idempotencyBegin` 构造 `newCaptureWriter` 的那两行、
  `captureWriter`）：为了一个可能永不到来的重试把一个无界答案留在内存里，是网关自己制造事故的方式。
- **重放把记录的字节原样写出**（`internal/gateway/idempotency.go` 的 `serveReplay`）。记录下来的
  **流式**响应作为一整段 body
  送出：转录就是答案，重新给它编一遍帧时序等于凭空发明一个上游从未有过的生成速度。`storedHeaders`
  会去掉 `Content-Length` / `Transfer-Encoding`（chunked 生成的框架在重放里并不存在）以及
  `X-InferGate-Idempotent-Store`，其余（Content-Type、上游自定义头）原样保留。

### 13.4 会话账本：同一个头承载会话与配额

`X-InferGate-Session` 一次归属、两处受益：M3 的配额可以按会话记，`/admin/sessions` 又能回答
"这段对话花了多少"。`Ledger.Record`（`internal/sessions/ledger.go`）在一个空 id 处分叉：空/全空白
只加 `no_session_id` 并且**不建会话**（"没带会话"与"有一个叫空字符串的会话"是两件事）；否则累计
requests / ok / failed、token 三态、按**实际服务的模型**定价的成本（`pricing.CostUSD(rec.model, …)`）、
模型与上游的 rollup，并把 `Recent` 裁到 `recent_per_session`。

- **会话的三个汇总（requests / cost / tokens）在 `/metrics` 里是 gauge 不是 counter**
  （`internal/server/m6.go` 的 `writeM6Metrics` 里 `infergate_sessions_*` 三个 gauge 起）：容量淘汰会
  让它们下降，而 Prometheus 的 counter 不允许下降——
  把它写成 counter 才是"面板说谎"的经典做法。
- **M6 的指标族刻意不带租户标签**：租户来自调用方可控的请求头，把它做成标签就是让任何人往指标基数里
  注入任意维度。
- **TTL 只报事实，不赌清扫**：janitor 按 `SweepInterval`（默认 1 分钟）清扫，`List`/`Get` 不做过期
  过滤。所以 `/admin/sessions` 报 `ttl` 与每条的 `expires_at`，而"真的会被清掉"由单测证明，而不是
  让验收器睡 150ms 去赌一次清扫恰好发生。

### 13.5 能力发现：探测故意绕过所有策略

`GET /v1/capabilities` 是纯计算、无 I/O（`internal/gateway/capabilities.go`）：模型集合是上游
`ModelPatterns()` 与配置 `models` 块的并集，能力是"服务该模型的后端声明的能力"与"模型自身声明的能力"
的并集，`available` 只在所有服务它的后端都熔断时才是 false。这条接口的存在理由很具体：**模型有多少
上下文，协议本身不会告诉 Agent**，而 Agent 决定要不要压缩历史时必须知道。

`POST /v1/capabilities/probe` 反过来**故意绕过缓存、配额、熔断与重试**
（`internal/gateway/capabilities.go` 的 `probeOne`）：探测是诊断流量，不是客户流量。让它去消耗
租户预算、或者被一个 open 的熔断器拦掉，就把
"这个后端到底行不行"偷换成了"现在允许我问吗"。分类口径（`classifyProbe`）：401/403 = unauthorized、
408/429 与 ≥500 = indeterminate、其余 ≥400 = rejected、2xx/3xx = accepted（`stream` 还额外要求
`text/event-stream`）。返回的 `accepted` 只声明"后端没有拒绝这个请求形状"，**不是**对答案质量的说法
（响应里的 `note` 原文写明了这一点）。一个只声明 `models: ["/"]` 的 catch-all 后端在没给 `model` 参数时
报告 `skipped` 与理由，而不是替它编一个模型名。

**多轮上下文压缩的边界**：网关不重写、不截断、也不摘要调用方的历史——它无法知道哪几轮是承重的，
而"猜错哪一轮可以丢"的代价是静默的错误答案，比 4xx 贵得多。网关只做两件事：把预算公布出来
（`context_window` / `max_output_tokens`），以及说明这个模型现在由哪些后端在服务、是否可用。压缩的
判断与执行留在 Agent 侧（Warden），因为它才知道这一轮的任务目标。

### 13.6 顺手修掉的缺陷：`Idempotency-Key` 会被透传给上游

第一版验收器发现上游**真的收到了** `Idempotency-Key`：`copyHeaders` 只丢 hop-by-hop 与
`x-infergate-` 前缀的头。这不是卫生问题，而是跨租户串答案：上游若用自己的幂等实现按这个头去重，
两个租户凑巧撞上同一个 key 字符串（共享模板里的 UUID、`"retry-1"`、一个日期），第二个租户会拿到
第一个租户的答案——key 在这里是按租户分作用域的，到了别处就不是了。现在
`isGatewayHeader`（`internal/gateway/headers.go`）把它一并拦下，并由
`TestGatewayHeadersDoNotReachProviders` 双向钉死：出站丢 key、`X-InferGate-*`、Connection 列出的头
与 hop-by-hop，保留 Authorization / Content-Type / 普通自定义头；入站方向仍保留
`X-Ratelimit-Remaining` 这类上游回传头。

### 13.7 验证：两个门 + 一份测量

- `cmd/verify-m6`（Go，**381** 条断言，CI 门禁）：在进程内起真实 server 与真实上游，六段——幂等重放、
  什么不会被记住、幂等管理面与指标、会话账本、声明式能力面与活体探测、一次完整的 Agent 工具调用对话
  （工具调用 → 工具结果 → 回答，以及重放与冲突各自如何入账）。
- `scripts/verify-m6.ps1`（curl，**149** 条断言）：真进程 + 真 curl，上游是仓库内**脚本化** mock
  （`cmd/mockupstream -script`），并用 mock 自己的 `GET /calls` 作外部证人——"两次客户端尝试、上游只被
  调用一次"这句话由上游数出来，而不是由网关自己声称。
- `scripts/measure-m6.ps1`：M6 的代价与收益（下节），原始数据落在
  `docs/baseline/m6-summary.json`。

### 13.8 测量：代价与收益

`scripts/measure-m6.ps1` 用四个臂回答四个不同的问题，且产物只有在**自身的检查全过**时才写进
`docs/baseline/m6-summary.json`（否则写到 `tmp\m6-summary.json` 并以非零码退出——一次坏运行永远
覆盖不了基线）：

- **A 代价**：两个真实网关跑同一个 mock，唯一差别是 M6 开关（幂等 store + 会话账本 + trace），
  `cmd/loadtest` 以 c=8/32、每相位 1500 请求 + 300 预热、3 轮取中位驱动。请求**不带**
  `Idempotency-Key`、也不带会话头：`cmd/loadtest` 没有自定义头参数，而这恰好是**无 key 路径**，
  不是退化路径——`idempotencyBegin` 直接返回且不做查表、不做插入，账本只把这个请求记成"未识别"。
  所以 A 臂量到的是 M6 接线**每请求固定成本**（recorder、trace 记录、账本计数），不是"存下一条答案"
  的成本；后者的成本是 C 臂的载荷字节与 B 臂的耗时。拿重放去美化延迟不是这一臂要做的事。
- **B 收益**：一轮真实生成 vs 一轮重放，用 curl 自己的 `%{time_total}` 打 20 对，并用 mock 的
  `GET /calls` 证明重放**没有**再打上游，同时报出省下的 token 与美元（按 `pricing` 折算，
  不是账单）。
- **C 存储**：容量 256 下逐个泵入 300 个不同的 key，`stored` 必须被封顶、淘汰数必须能解释差额。
  字节数分成两笔诚实的账：**载荷下界**是"重放一个已存 key、量出网关原样送回多少字节"，即 store
  确定持有的部分；**RSS 增量**是旁证，但它含分配器松量，在这台机器上 256 条的working set 甚至可能
  低到让增量为负（Go 会把内存还给操作系统），所以增量为负时该臂**不主张**任何"每条答案字节数"。
- **D 正确性**：16 个**同时**发出的同 key 请求只应产生一次生成（其余拿到 409 in-flight 或 200 重放），
  上游恰好被问一次。为了让 16 个客户端真的重叠，上游延迟用 mock 的 `X-Mock-Delay` 拉到 300ms——
  否则 1ms 的答案会在第 16 个客户端到达前就关闭窗口，那测的是到达顺序，不是缓存外的在途保护。

**实测（i9-14900HX / 32 逻辑核，单机回环，两个真网关进程 + 仓库内 mock，`log.level: error`，
3 轮取中位，每相位 1500 请求 + 300 预热；`scripts/measure-m6.ps1` 9 项检查、58 条断言（收尾后产物
共记 67 条）全过、0 条失败，产物 `docs/baseline/m6-summary.json`，本次运行 400.7s）**

| 负载 | c | m6-off QPS | m6-on QPS | ΔQPS | off p95 | on p95 | Δp95 | off p99 | on p99 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 非流式 | 8 | 1915.1 | 2051.8 | +7.14% | 6.562 ms | 5.748 ms | −12.41% | 7.588 ms | 7.757 ms |
| 非流式 | 32 | 2411.4 | 2249.6 | −6.71% | 16.211 ms | 18.080 ms | +11.53% | 18.530 ms | 21.896 ms |
| 流式 | 8 | 119.5 | 119.8 | +0.24% | 70.226 ms | 70.128 ms | −0.14% | 73.445 ms | 73.114 ms |
| 流式 | 32 | 483.3 | 477.5 | −1.20% | 69.376 ms | 72.837 ms | +4.99% | 82.078 ms | 90.555 ms |

- **c=8 的符号不可引用**：同一条命令的另一次全量运行给的是 **−5.96%**（这次 +7.14%），而且**对照臂
  自己**的三轮就跨 **1888–4194 QPS**——中位数旁边那两次是快轮，对照臂的中位因此被压低。这台机器在
  c=8 上分辨不出 M6 的成本（与 §12 的"绝对值不可跨运行引用"是同一个原因）。
- **c=32 非流式才是可复现的信号**：两次运行都给 p95 **+11.5%**（+11.53% / +11.78%），QPS −6.71% /
  −15.13%（同号，量级不稳）。M6 的每请求接线（trace 记录 + recorder + 账本计数）在 32 并发下开始
  可测，代价是十几毫秒 P95 里的一两毫秒。
- **流式 ≈ 0**：mock 每词睡 15ms，相位被上游填满（p50 66.5 → 66.3ms），网关侧成本被淹没。
- **内存**：RSS 31.1 → 36.0 MiB（另一次 30.4 → 35.5），即 **+4.9~5.1 MiB**；A 臂不带 key，所以这
  5 MiB 里没有一条被记住的答案，是 trace 环与账本的成本。
- **B 臂（收益）**：20 对真实生成 / 重放，重放**没有一次**打到上游（mock `/calls` 不动），单轮省
  6 prompt + 5 completion token ≈ **$0.000021**（20 轮共 220 token / $0.00042，按 `pricing` 折算）。
  墙上时间只快 **1.267×**（4.5 vs 5.7 ms）——这一臂要证明的不是"重放更快"，而是"上游没被调用"；
  fresh 那一列被 mock 的 15ms/词限速支配，所以延迟差不是网关的功劳（脚本把它写进了 limitations）。
- **C 臂（存储）**：300 个不同 key 泵进容量 256 → `stored` 封顶 256、`evicted` 44（`stats.stored` 320、
  `stats.lookups` 340、`evicted_total` 44），上游恰好被问了 300 次（每个 key 一次）；256 条答案的
  **载荷下界**是 256 × 285 B = **71.3 KiB**。RSS 增量是 **−4.05 MiB**（flush 后不回涨），所以这一臂
  **不主张**"每条答案多少字节"：把负数除以 256 条会印出"存一条答案省 14 KB"，而第一版脚本正是这么做的。
- **D 臂（正确性）**：16 个同时到达的同 key 请求（上游窗口 300ms）→ **1 个 200 新答案 + 15 个 409
  in-flight**、0 个重放，mock `/calls` 恰好 **+1**。重叠不是造的：窗口由 mock 的 `X-Mock-Delay`
  拉开，否则 1ms 的答案会在第 16 个客户端到达前就关窗。


# InferGate 设计文档（M0 + M1 + M2 + M3）

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
08:00 重置；这个副作用写在 README 里，因为它会让"今天的用量"和运维的日历对不上。

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

# 第 05 册 · 可观测性（追踪 · 指标 · 假上游 · 收集器）

本册走读 InferGate 里"让网关可被外部观察"的四条线：

1. **追踪** `internal/tracing`（`trace.go` / `context.go` / `store.go`）——W3C Trace Context 传播、内存 span 模型、有界 trace 存储。
2. **导出** `internal/traceexport`（`otlp.go` / `jsonl.go`）——把 trace 送出进程的两个实现。
3. **指标** `internal/metrics`（`metrics.go` / `histogram.go` / `sort.go`）加上 `internal/server/server.go` 里的 `/metrics` 文本 exposition。
4. **假上游** `internal/mockbackend`、`cmd/mockupstream`、`cmd/mockcollector`——验收脚本的测量仪器。

写作约定：锚点一律写成 `符号名 (infergate/internal/.../file.go)` 的形状，只有代码块里才会出现行号。凡是"为什么这么设计"的句子，如果来自源码注释，会显式标注为**源码注释观点**。

一个贯穿全册的总纲（`internal/traceexport/jsonl.go` 的包注释，**源码注释观点**）：两个导出器共享一条设计规则——`Export` 永不在 I/O 上阻塞；trace 是调试产物，调试产物永远不值一毫秒请求延迟，更不值一个卡住的请求。这条规则解释了后面几乎每一个 API 形状。

---

## 1. internal/tracing ：追踪数据模型

## 2. trace.go ：时间基准、Span、Trace、纳秒与毫秒的派生

## 3. 时间基准（包级约定）

`internal/tracing/trace.go` 的全部时间是"Unix 纪元起的纳秒 `int64`"，并且**源码注释观点**是：整数是权威表示，浮点毫秒字段只是因为 JSON 要给人读才存在。因此没有任何一处把 `time.Time` 当权威——`time.Time` 只在 `Trace.StartedAt` 这一处出现，作为给人看的锚。

- `nanosToMS (internal/tracing/trace.go)` —— `func nanosToMS(nanos int64) float64`。作用：把 int64 纳秒换算成保留 6 位小数的毫秒浮点。实现是 `math.Round(float64(nanos)/1e6*1e6)/1e6`。读写状态：无状态纯函数，无锁。错误路径：无（不做溢出检查，`int64` 纳秒的可表示范围远大于任何真实时长）。调用关系：被 `(*Trace).Add` 与 `(*Span)` 的时长派生使用。**源码注释观点**：1ns = 1e-6ms 在二进制里不可精确表示，四舍五入让打印与编码稳定；亚纳秒不存在，所以 6 位是无损的；用取整而不是截断，因为截断会让每次测量都偏低最多 1 微秒，也就是系统性地偏向"隐藏延迟"。

## 4. `Event`

- `Event (internal/tracing/trace.go)` —— `type Event struct { Name string \`json:"name"\`; TimeUnixNano int64 \`json:"time_unix_nano"\`; Attributes map[string]any \`json:"attributes,omitempty"\` }`。作用：一个没有时长的、发生在 span 内部的时间点。读写状态：值类型，被 `Span.Events` 持有；`(*Trace).Add` 与 `Clone` 会深拷贝其中的 `Attributes`。**源码注释观点**：形状镜像 OTLP 的 span event；用 `Event` 而不是子 span，因为"首 token"这种事件没有时长可供嵌套，用子 span 表示它就要伪造一个起止对。

## 5. `Span`

- `Span (internal/tracing/trace.go)` —— `type Span struct { TraceID string \`json:"trace_id"\`; SpanID string \`json:"span_id"\`; ParentSpanID string \`json:"parent_span_id,omitempty"\`; Name string \`json:"name"\`; Kind string \`json:"kind"\`; StartUnixNano int64 \`json:"start_unix_nano"\`; EndUnixNano int64 \`json:"end_unix_nano"\`; DurationMS float64 \`json:"duration_ms"\`; Status string \`json:"status"\`; Attributes map[string]any \`json:"attributes,omitempty"\`; Events []Event \`json:"events,omitempty"\` }`。作用：一个工作单元。读写状态：`DurationMS` 是派生冗余字段——`Add` 会在 `DurationMS == 0 && EndUnixNano > StartUnixNano` 时替调用方填上，`Clone` 逐字段深拷贝。并发：`Span` 本身没有锁，约定是"移交后不再被写"。调用关系：被 `Trace.Add` 收下，被 `Store.Add` 经 `Trace.Clone` 拷贝，被 `encodeOTLP` 转成 OTLP JSON 形状。**源码注释观点**：根 gateway span 与它所属的 trace 共享 `TraceID`，并且是唯一 `ParentSpanID` 为空的 span——这是"哪个 span 是根"的可判定定义，不需要额外字段。

## 6. Kind 常量

- `KindServer (internal/tracing/trace.go)` —— `const KindServer = "server"`。作用：标记"我们作为服务端处理入站请求"的 span。
- `KindClient (internal/tracing/trace.go)` —— `const KindClient = "client"`。作用：标记"我们向外部发起调用"的 span（例如对上游 provider 的那一跳）。
- `KindInternal (internal/tracing/trace.go)` —— `const KindInternal = "internal"`。作用：进程内的、不对应任何网络边界的 span。

三者都是**字符串常量而不是 `int` 枚举**。**源码注释观点**：字符串让 JSON 直接可读，人不用查表就能看懂一行 trace；同时让 collector 侧的类型映射是一个 `switch` 而不是一张查表。

## 7. Status 常量

- `StatusUnset (internal/tracing/trace.go)` —— `const StatusUnset = "unset"`。
- `StatusOK (internal/tracing/trace.go)` —— `const StatusOK = "ok"`。
- `StatusError (internal/tracing/trace.go)` —— `const StatusError = "error"`。

**源码注释观点**：`"unset"` 是一个真实状态而不是"空值"的同义词——它用来区分那些被遗弃的 span（开始了、但进程/请求在结束前就放弃了它），这类 span 与"成功"和"失败"都不同。`NewTrace` 造出的 trace 起始状态就是 `StatusUnset`。

## 8. 属性键常量

- `AttrRoute (internal/tracing/trace.go)` —— `const AttrRoute = "route"`。
- `AttrUpstream (internal/tracing/trace.go)` —— `const AttrUpstream = "upstream"`。
- `AttrModel (internal/tracing/trace.go)` —— `const AttrModel = "model"`。
- `AttrRequest (internal/tracing/trace.go)` —— `const AttrRequest = "request_id"`。

作用：这三个键是 `Summary()` 抽取 route/upstream/model 时用的键名。**源码注释观点**：把它们导出，是为了让调用方无法把 `"route"` 拼错成 `"rout"`——拼错不会报错，只会得到一个静默为空的摘要字段，而这种 bug 要等排障当天才发现。

## 9. `Summary`

- `Summary (internal/tracing/trace.go)` —— `type Summary struct { TraceID string \`json:"trace_id"\`; RequestID string \`json:"request_id"\`; StartedAt string \`json:"started_at"\`; DurationMS float64 \`json:"duration_ms"\`; Status string \`json:"status"\`; Route string \`json:"route,omitempty"\`; Upstream string \`json:"upstream,omitempty"\`; Model string \`json:"model,omitempty"\`; SpanCount int \`json:"span_count"\` }`。作用：列表端点要展示的那一小撮字段。读写状态：全部是派生值，由 `(*Trace).Summary` 计算，并在 `Store.addLocked` 时被预先算好存进 `entry`。**源码注释观点**：`StartedAt` 在这里是 RFC3339Nano 字符串而不是 `time.Time`，因为摘要的目的就是直接进 JSON。

## 10. `Trace`

- `Trace (internal/tracing/trace.go)` —— `type Trace struct { TraceID string \`json:"trace_id"\`; RequestID string \`json:"request_id"\`; StartedAt time.Time \`json:"started_at"\`; DurationMS float64 \`json:"duration_ms"\`; Status string \`json:"status"\`; Attributes map[string]any \`json:"attributes,omitempty"\`; Spans []Span \`json:"spans"\` }`。作用：一棵 span 树加上 trace 级属性。读写状态：`StartedAt` 与 `DurationMS` 是缓存字段，`Summary()` 只把 `DurationMS` 当退化情形下的兜底。并发：`Trace` 自身无锁，`Add` 非并发安全；**源码注释观点**：包级约定是" Trace 零值可用；`Store` 必须用 `NewStore` 构造"——trace 的零值能安全地读，store 的零值没有容量。

- `NewTrace (internal/tracing/trace.go)` —— `func NewTrace(traceID, requestID string, t0 time.Time) *Trace`。作用：造一个还没有 span 的 trace，`Status` 起始为 `StatusUnset`。读写状态：只写新分配对象。错误路径：无（不做 id 校验；id 的合法性是 `Parse` 的职责）。调用关系：由请求路径（`internal/server`、`internal/gateway`）在开根 span 前调用。

- `(*Trace).Add (internal/tracing/trace.go)` —— `func (t *Trace) Add(span Span)`。作用：把一个 span 收进 trace，并在收下的过程中补全派生字段。实现分三步，每一步都有理由：
  1. 先 `cloneSpan(span)` 深拷贝。**源码注释观点**：调用方在请求路径上还在继续写同一个 `Attributes` map，如果直接存别名，一个"已完成"的 trace 会被后续写入在并发读者眼皮底下改掉——那正是 `Store` 承诺要防的 data race。
  2. 若 `span.DurationMS == 0 && span.EndUnixNano > span.StartUnixNano`，则 `DurationMS = nanosToMS(EndUnixNano - StartUnixNano)`，替调用方算好，避免每个调用点重复这个算术。
  3. append 之后重新计算 `t.StartedAt = time.Unix(0, earliestStart(t.Spans)).UTC()`，并令 `t.DurationMS = t.Duration()`。
  读写状态：写 `t.Spans`、`t.StartedAt`、`t.DurationMS`。错误路径：无；nil 接收者会 panic（没有任何地方为 nil `*Trace` 调 `Add`）。并发：**非并发安全**，由 `Store` 的 mutex 在它之外串行化；**源码注释观点**：归一化到最早 span 的开始，是为了保证 `StartedAt.UnixNano()` 恒等于最早 span 的开始时间，即使 span 是乱序到达的。另外**源码注释观点**：`Spans[0]` 预期是根 span，但 `Add` 不强制这一点（它无法知道调用方的意图）。

- `(*Trace).Duration (internal/tracing/trace.go)` —— `func (t *Trace) Duration() float64`。作用：把 trace 的时长定义为"最早 span 的开始到最晚 span 的结束"。读写状态：只读 `t.Spans`，无写。错误路径：`t == nil`、无 span、`min == 0`、`max <= min` 都返回 `0`。并发：只读，但仍不能与并发 `Add` 并存。边界处理：`EndUnixNano < StartUnixNano` 的 span 被跳过其结束时间（未结束的 span 只贡献开始时间）。

- `(*Trace).Summary (internal/tracing/trace.go)` —— `func (t *Trace) Summary() Summary`。作用：派生列表要用的摘要。读写状态：只读 `t.Status`、`t.Attributes`、`t.Spans`、`t.DurationMS`、`t.StartedAt`。错误路径：`t == nil` 返回 `Summary{}`。派生规则逐条：
  - `TraceID`/`RequestID`/`SpanCount` 直接来自 trace 本身与 `len(t.Spans)`。
  - `Status`：先取 trace 级 `t.Status`，空则用 `StatusUnset`；若存在 `Spans[0]` 且它的 `Status != ""`，改用根 span 的 status。
  - `Route`/`Upstream`/`Model`：优先根 span 的属性，回退 trace 级属性。**源码注释观点**：根 span 上的值更具体——trace 级的 `model` 是网关的意图，根 span 上的是路由与故障转移之后实际解析出来的结果；两者不一致本身就是信息。
  - `StartedAt`：`startNanos` 取根 span 的 `StartUnixNano`，为 0 则用 `earliestStart(t.Spans)`；`> 0` 时格式化为 `time.Unix(0, startNanos).UTC().Format(time.RFC3339Nano)`；否则退回非零的 `t.StartedAt`；再否则 `time.Unix(0, 0)`。
  - `DurationMS`：用 `t.Duration()`；若为 0 而 `t.DurationMS > 0`，改用缓存值。**源码注释观点**：这是手工组装 trace 的退化情形，宁可报一个数也不报 0。
  - **源码注释观点（重要）**：所有字段都从已存 span 重新派生，不读 trace 自身那份浮点缓存——缓存是给 JSON 编码器看的，摘要是给列表页看的，两者共用一份缓存迟早会不一致。

- `(*Trace).Clone (internal/tracing/trace.go)` —— `func (t *Trace) Clone() *Trace`。作用：深拷贝整棵 trace，作为 `Store` 两侧隔离的实现。读写状态：只读源，产出新对象。错误路径：`t == nil` 返回 `nil`。并发：对源只读，因此必须与 `Add` 互斥（实际由 `Store` 的锁保证）。拷贝范围：`Spans` 切片、每个 span 的 `Attributes`、每个 span 的 `Events` 切片、每个 event 的 `Attributes`、trace 级 `Attributes`。

- `cloneSpan (internal/tracing/trace.go)` —— `func cloneSpan(s Span) Span`。作用：拷贝单个 span，包括 `Events` 切片及每个 event 的属性 map。错误路径：无。

- `cloneAnyMap (internal/tracing/trace.go)` —— `func cloneAnyMap(m map[string]any) map[string]any`。作用：拷贝一层 map 的值为深拷贝。错误路径：`m == nil` 返回 `nil`（不返回空 map，以免 `omitempty` 行为改变）。

- `cloneAny (internal/tracing/trace.go)` —— `func cloneAny(v any) any`。作用：对 `map[string]any`、`[]any`、`[]string` 递归克隆，标量原样返回。**源码注释观点**：未知类型按引用带走——包里可能出现 `*http.Request` 这类本包无法理解、也无法深拷贝的值，强行克隆比保留引用更危险。

- `strAttr (internal/tracing/trace.go)` —— `func strAttr(m map[string]any, key string) string`。作用：从属性 map 里取一个字符串值。分支：`case string` 返回值；`case fmt.Stringer` 返回 `v.String()`；`default` 返回 `""`；`m == nil` 返回 `""`。错误路径：无错误返回，取不到就是空串——这是 `Summary` 的 `omitempty` 期望的语义。

- `earliestStart (internal/tracing/trace.go)` —— `func earliestStart(spans []Span) int64`。作用：返回最小的**非零** `StartUnixNano`。语义细节：零值被忽略而不是当作"纪元开始"，否则一个未被赋值的 span 会把整棵 trace 的起始时间拉到 1970 年。错误路径：全部为零或切片为空时返回 0。

- `fallbackCounter (internal/tracing/trace.go)` —— `var fallbackCounter atomic.Uint64`。作用：`fallbackHex` 的进程级单调计数器，让熵源失效时的输出仍然唯一。并发：原子操作。

- `fallbackHex (internal/tracing/trace.go)` —— `func fallbackHex(n int) string`。作用：在 `crypto/rand` 失败时生成 `n` 字节的十六进制。实现是对混合了时间、`fallbackCounter` 的种子跑 splitmix64（`seed += 0x9e3779b97f4a7c15`，`z ^= z >> 30` 乘 `0xbf58476d1ce4e5b9`，`z ^= z >> 27` 乘 `0x94d049bb133111eb`，`z ^= z >> 31`），再用 `hexdigits = "0123456789abcdef"` 转小写。**源码注释观点**：这是非密码学材料，绝不可用于 token；`store` 与 parser 需要的是唯一性而不是不可预测性。

## 11. context.go ：W3C traceparent 的严格解析与传播

`internal/tracing/context.go` 的包注释给出了整个 M5 的定位（**源码注释观点**）：

- 不用 OpenTelemetry 的原因不是"想省事"，而是零第三方依赖是硬约束，而 Go OTel SDK 会拖进一棵比网关本身更大的依赖树；本模块真正需要的只有三件事——跨 `client → gateway → upstream` 存活的 trace id、能解释时间花在哪的 span 树、事后按 id 重放某一个请求。
- 传播是真正的 W3C Trace Context version 00（规范地址在注释里给出：https://www.w3.org/TR/trace-context/）。
- span/trace 的形状是"OTLP-LIKE"：snake_case JSON 字段、int64 纳秒、`Events` 映射 OTLP span events；但 JSON 不是 OTLP（没有 resource/scope 层、没有 protobuf、kind/status 是小写字符串而不是整数枚举）。
- 存储是有界环形缓冲，store 满时丢最老的并计数。
- **故意不做**：采样导出器、后台 flush、基于父的采样率、时钟注入。这些都是属于调用方（`internal/server`、`internal/gateway`）的策略决定，放在这个包里会让"数据模型"和"运营策略"纠缠在一起。

## 12. 头部常量

- `HeaderTraceparent (internal/tracing/context.go)` —— `const HeaderTraceparent = "traceparent"`。
- `HeaderTracestate (internal/tracing/context.go)` —— `const HeaderTracestate = "tracestate"`。

**源码注释观点**：常量写成小写，是因为 Go 的 `http.Header` 会把名字规范化成 `Traceparent`/`Tracestate`，而规范里的小写形式就是获取时的键；`tracestate` 本包不解析、不校验，逐字节转发——网关改写别家 vendor 的 state 就等于破坏它。

## 13. `Context`

- `Context (internal/tracing/context.go)` —— `type Context struct { TraceID string; SpanID string; Sampled bool; TraceState string; Remote bool }`。作用：一次传播解析的产物。每个字段的语义：
  - `TraceID`/`SpanID`：从入站 `traceparent` 读到的 id。
  - `Sampled`：flags octet 的最低位。
  - `TraceState`：原样携带的 `tracestate`。
  - `Remote`：这个上下文是否来自对端（`Parse` 成功即 `true`，`NewRootContext` 为 `false`）。
  读写状态：值类型，无锁。**源码注释观点（关键）**：`SpanID` 是**调用方（我们的父）的 span id**，不是我们自己的。网关开根 server span 时必须把它记成那个 span 的 `ParentSpanID`，再往下游发自己新铸的 span id；如果原样转发收到的 `SpanID`，每一跳都会自称同一个 span id，trace 就退化成一堆兄弟的扁平列表，父子关系全部丢失。

## 14. 长度与文法常量

- `traceIDLen (internal/tracing/context.go)` —— `const traceIDLen = 32`。作用：trace id 的十六进制字符数。
- `spanIDLen (internal/tracing/context.go)` —— `const spanIDLen = 16`。作用：span id 的十六进制字符数。
- `version00 (internal/tracing/context.go)` —— `const version00 = "00"`。**源码注释观点**：只接受 `00`。未来版本可能改变字段文法，静默接受一个未知版本，等于在用"自己声称实现的格式"去转发"自己并不理解的字段"。
- `sampledFlag (internal/tracing/context.go)` —— `const sampledFlag = 0x01`。作用：flags octet 的最低位掩码。

## 15. `Parse`

- `Parse (internal/tracing/context.go)` —— `func Parse(traceparent, tracestate string) (Context, bool)`。作用：严格解析一个入站 `traceparent`，并把 `tracestate` 原样带走。读写状态：纯函数，无状态、无锁。错误路径：`ok == false` 时返回**零值** `Context`，绝不返回部分填充的对象；**永不 panic**。调用关系：`internal/server` 的请求入口调用它；它的逆函数是 `(*Context).Header`，`context_test.go` 用往返测试把两者钉在一起。

  被拒绝的输入清单（来自函数注释，逐条保留）：
  - 空的输入，或带前后空白填充的输入；
  - 用 `-` 切分后分段数不等于 4；
  - `version` 不是 `"00"`；
  - trace id 不是恰好 32 个小写十六进制字符；
  - span id 不是恰好 16 个小写十六进制字符；
  - flags 不是恰好 2 个十六进制字符；
  - **大写十六进制**：文法是仅小写，两种都接受会让同一个 trace 在 store 里被两个 id 寻址；
  - 全零的 trace id 或 span id（规范里的 "unspecified"）；
  - 全 `ff` 的 trace id 或 span id（规范里的 "invalid"）。

  `tracestate` 不参与校验：它是自有规范的、opaque 的 vendor state；只要 `traceparent` 有效就逐字携带（包括空串）。

  实现顺序（对应的健壮性理由）：先 `traceparent == ""` 或 `traceparent != strings.TrimSpace(traceparent)` 直接 false（一行同时挡掉空值与所有空白填充）；再 `strings.Split(traceparent, "-")` 并检查 `len != 4`；对 trace id 与 span id 各做 `isID(..., n)` 与 `isAllZeroOrAllFF(...)`；`len(flags) != 2` 拒绝；`hex.DecodeString(flags)` 出错或 `!isLowerHex(flags)` 拒绝；成功后返回 `Sampled: fb[0]&sampledFlag != 0`、`Remote: true`、`TraceState: tracestate`。

- `(*Context).Header (internal/tracing/context.go)` —— `func (c Context) Header() string`。作用：把上下文渲染成 `version00 + "-" + TraceID + "-" + SpanID + "-" + ("01"|"00")`。读写状态：只读，无锁。错误路径：无——**它刻意不校验**。**源码注释观点**：唯一重要的调用方是出口路径，在那里"拒绝发 header"比"发一个会被下一跳拒绝的畸形 header"更糟，因为前者会让一个本来能工作的链路凭空断掉。副作用：flags 里的其他位被构造性地丢弃（只保留 sampled 位）。

- `NewRootContext (internal/tracing/context.go)` —— `func NewRootContext() Context`。作用：为一个本地新请求开一个新的 trace 根，返回 `TraceID: NewTraceID()`、`SpanID: NewSpanID()`、`Sampled: true`、`Remote: false`。错误路径：无。**源码注释观点（`Sampled` 默认 `true` 的理由）**：如果默认不采样，就意味着只有调用方主动 opt-in 才有 trace，实际效果等于"没有 trace"——而会 opt-in 的调用方恰恰是那些已经在跑 tracer 的；M5 要的是普通流量的 trace，所以默认开，把"丢弃"的决定留给调用方。

- `NewTraceID (internal/tracing/context.go)` —— `func NewTraceID() string { return randomHex(traceIDLen / 2) }`。作用：16 个随机字节 → 32 个小写十六进制字符。**源码注释观点**：用随机而不是计数器或时间戳，因为 trace id 会交给客户端、并会出现在日志与 header 里；可猜的 id 意味着能读、甚至能伪造别人租户的 trace。错误路径：熵源失败时降级到 `fallbackHex`（见 `randomHex`）。

- `NewSpanID (internal/tracing/context.go)` —— `func NewSpanID() string { return randomHex(spanIDLen / 2) }`。作用：8 个随机字节 → 16 个小写十六进制字符。

- `randomHex (internal/tracing/context.go)` —— `func randomHex(n int) string`。作用：唯一的随机 id 生成点。读写状态：无共享状态（除降级路径里的 `fallbackCounter`）。错误路径：`crypto/rand.Read` 失败时不 panic，改用 `fallbackHex`。**源码注释观点**：`crypto/rand.Read` 的文档保证在支持平台上不会失败，但函数签名逼出一个决定——请求路径上是 panic 还是降级？选降级：混合时间、进程级计数器与栈地址进哈希，结果虽非密码学随机，但实践中唯一，并且能让网关继续服务；store 与 parser 真正需要的是唯一性。

- `isID (internal/tracing/context.go)` —— `func isID(s string, n int) bool`。作用：`len(s) == n && isLowerHex(s)`。错误路径：无。

- `isLowerHex (internal/tracing/context.go)` —— `func isLowerHex(s string) bool`。作用：逐字节判断是否只含 `0-9a-f`。**源码注释观点**：手写而不是用 `hex.DecodeString`，有两个理由——后者也接受大写（与只接受小写的文法不符），而且这段代码在每个入站请求上都会跑，手写循环比分配型解码便宜。

- `isAllZeroOrAllFF (internal/tracing/context.go)` —— `func isAllZeroOrAllFF(s string) bool`。作用：判断保留值。实现同时跟踪"是否全 `0`"与"是否全 `f`"，两者都变假就提前返回。**源码注释观点**：规范里全零是 "unspecified"、全 `ff` 是 "invalid"，两者都要拒绝，而不是替换成一个新 id——静默替换 id 会让坏掉的生产者看起来是好的，而且那条 trace 会被归档到客户端从未发过的 id 下，事后根本对不上账。

## 16. store.go ：环形缓冲、FIFO 淘汰、生命周期计数

- `DefaultCapacity (internal/tracing/store.go)` —— `const DefaultCapacity = 1024`。作用：`Store` 的默认容量。**源码注释观点**：约 1MB，小到可以常驻、大到能回答"五分钟前那个请求怎么了"；它是内存上限而不是目标——需要更长的历史应该导出（也就是 `WriteJSONL`），而不是把 store 调大。

- `ErrNilTrace (internal/tracing/store.go)` —— `var ErrNilTrace = errors.New("tracing: nil trace")`。作用：`WriteJSONL` 收到 nil trace 时的错误值（错误字符串原文：`tracing: nil trace`），调用方可用 `errors.Is` 比对。

- `entry (internal/tracing/store.go)` —— `type entry struct { trace *Trace; summary Summary }`。作用：环里一个槽位的全部内容——指针加上预先算好的摘要。**源码注释观点**：进 `Add` 时克隆、出 `Get` 时克隆，因此 store 之外没有任何人持有这个指针，槽位替换可以安全地丢掉旧对象。

- `Store (internal/tracing/store.go)` —— `type Store struct { mu sync.RWMutex; capacity int; entries []entry; head int; count int; index map[string]int; dropped int64 }`。作用：有界的内存 trace 存储。字段语义：`entries` 是环形缓冲（固定长度 `capacity`），`head` 是最老槽位，`count` 是当前存活数，`index` 把 trace id / request id 映射到槽位下标，`dropped` 是生命周期淘汰计数。并发：一把 `sync.RWMutex`，写用 `Lock`、读用 `RLock`；**源码注释观点**：克隆在锁内完成，但 `WriteJSONL` 这类渲染刻意在锁外发生。关键设计（来自类型上方注释，**逐条为源码注释观点**）：
  - **淘汰是按插入顺序的 FIFO，不是 LRU**。存下来的 trace 最多被读几次（操作员重放着看），"最近被访问过"这件事几乎不携带信息；而 LRU 的簿记是多一个要在 mutex 下维护、并且会出错的东西。
  - **`entries` 是环形缓冲而不是普通切片**。如果用切片并在淘汰时删 `entries[0]`，每个元素都要移动，`index` 里每个 id 都会失效——这是本包最坏的一种失败：排障的人读到别人的请求。用环则槽位永不变，`index` 每次 `Add` 只做 O(1) 补丁，淘汰就是两次 map 删除。
  - **store 不跨 I/O 持锁**。`WriteJSONL` 显式接收 writer 与 trace、而且不是 `Store` 的方法，所以渲染总在 mutex 外发生；一个阻塞的 writer 不可能拖住请求路径上的 `Add`。
  - 约定：`Store` 必须用 `NewStore` 构造，零值没有容量。

- `NewStore (internal/tracing/store.go)` —— `func NewStore(capacity int) *Store`。作用：构造 store。错误路径：无错误返回；`capacity <= 0` 时改用 `DefaultCapacity`（**源码注释观点**：store 永远不会有 0 容量——"静默什么都不存"与"仪表坏了"无法区分，所以宁可用默认值）。实现：`entries: make([]entry, capacity)`、`index: make(map[string]int, capacity*2)`。

- `(*Store).Add (internal/tracing/store.go)` —— `func (s *Store) Add(t *Trace)`。作用：把一个 trace 拷进 store（或原地替换同 id 的旧版本）。读写状态：写 `entries`、`index`、`head`、`count` 与 `dropped`。错误路径：`t == nil` 直接返回（不 panic、不报错）。并发：`Lock` + `defer Unlock`，实际逻辑在 `addLocked`。**源码注释观点（几条要点）**：
  - `Dropped` 数的是生命周期里的淘汰总量，`Reset` 不会把它清零——"丢失了多少"是计数器（counter），不是瞬时值（gauge）。
  - 收下之后的这份拷贝就是 `Add` 这个方法存在的意义：调用方可以继续 append span、继续写自己那份属性 map，既不会通过 `Get` 可见，也不会与并发的 `Get` 竞争；两个方向都有测试覆盖（`TestStoreAddDoesNotAliasCaller` 与 `TestStoreGetByTraceAndRequestID`）。
  - **空 `TraceID` 的 trace 会被存下来并计入 `Len`**（`WriteJSONL` 用它 dump），但**故意不索引**。理由：空键会让所有这类 trace 冲突，`Get("")` 会返回其中任意一个。结果是"忘设 trace id 的仪表"得到的是可列举、可计数、但永远 fetch 不到的 trace——一个响亮又便宜的失败模式，而不是一个会返回错东西的 API。**注释明确要求不要把它"修"成索引空串。**
  - `RequestID` 是独立索引的第二个键。
  - 重复 trace id 的 `Add` 是**原地替换**：槽位不动，因此 `List` 的顺序稳定，`Get` 立刻返回新版本。

- `(*Store).addLocked (internal/tracing/store.go)` —— `func (s *Store) addLocked(t *Trace)`。作用：`Add` 的持锁主体。流程：先 `cp := t.Clone()`；若 `cp.TraceID != ""` 且 `index` 命中某个槽位，则原地替换 `entries[slot] = entry{trace: cp, summary: cp.Summary()}`，并且当 `cp.RequestID != ""` 时把 `index[cp.RequestID] = slot`，然后返回。否则若 `count == capacity`，淘汰 `s.entries[s.head]`：`delete(index, old.trace.TraceID)`、`delete(index, old.trace.RequestID)`、把旧槽清成 `nil` 与 `Summary{}`、`head = (head+1) % capacity`、`count--`、`dropped++`。接着 `slot := (head + count) % capacity` 写入新 entry、`count++`，最后把非空的 `TraceID`/`RequestID` 写进 `index`。错误路径：无（调用方已挡掉 nil）。并发：**调用者必须已持有 `s.mu` 写锁**。

- `(*Store).entryAt (internal/tracing/store.go)` —— `func (s *Store) entryAt(slot int) (entry, bool)`。作用：越界或空槽时返回 `false` 的安全取值。错误路径：`slot < 0 || slot >= capacity` 或 `e.trace == nil` 时 `(entry{}, false)`。

- `(*Store).Get (internal/tracing/store.go)` —— `func (s *Store) Get(id string) (*Trace, bool)`。作用：按 trace id 或 request id 取出一份拷贝。读写状态：只读，`RLock`。错误路径：`id == ""` 直接 `(nil, false)`；索引未命中 `(nil, false)`；槽位为空 `(nil, false)`。成功时返回 `e.trace.Clone(), true`。语义要点：
  - 查找是**精确且大小写敏感**的。**源码注释观点**：store 只持有被交进来的原字符串；大小写不敏感会让两个不同的 id 解析到同一个 trace。
  - 这个 `bool` 值得检查。**源码注释观点**：对不存在的 id，API 无法区分"从未记录"、"被淘汰"与"畸形 id"，只有调用方知道该把哪一种解释交给操作员；`Dropped()` 正是用来区分前两者的那个数。
  - **源码注释观点**：多一个索引键（request id）不是装饰，它让"知道 request id 但不知道 trace id"的排障路径也能取回整棵 trace。

- `(*Store).List (internal/tracing/store.go)` —— `func (s *Store) List(limit int) []Summary`。作用：按"最新在前"的顺序返回摘要。读写状态：只读，`RLock`。错误路径：无；`limit <= 0` 或 `limit > count` 时 `limit = count`。实现细节：`out = make([]Summary, 0, limit)`，槽位按下标 `slot := (head + count - 1 - i + capacity) % capacity` 取，也就是**反插入序**。语义要点：
  - 返回的是 `Add` 时就算好的摘要，不重建、不碰 spans。**源码注释观点**：列表端点必须便宜且少分配；为了渲染一张表格就去克隆整棵 trace，正是让"可观测性"变成"它本来要测量的那个请求路径上的延迟问题"的原因。
  - 返回的切片总是新分配的（调用方拥有它）。
  - 顺序是插入序：确定，但不一定是开始时间序——一个先开始、后结束的长请求会显示为"最新"。需要时间序的调用方自己排序（这些是值，不是共享状态）。

- `(*Store).Len (internal/tracing/store.go)` —— `func (s *Store) Len() int`。作用：当前存活 trace 数。读写状态：`RLock` 只读 `count`。**源码注释观点**：它是活 gauge——`Reset` 归 0，而 `Dropped` 继续计。

- `(*Store).Dropped (internal/tracing/store.go)` —— `func (s *Store) Dropped() int64`。作用：被淘汰的 trace 总数。读写状态：`RLock` 只读。**源码注释观点（为什么 `Reset` 不清零）**：它回答的是"我要找的那条 trace 是没记录，还是被挤掉了"；一个会被重置的计数器恰好会抹掉配置重载或手动 `Reset` 之后操作员最需要的证据。`Dropped` 稳步上升的正确反应是提高容量或开导出，而不是"仪表坏了"。

- `(*Store).Reset (internal/tracing/store.go)` —— `func (s *Store) Reset()`。作用：清空 store 但保留容量与 `Dropped`。实现：`Lock` 后把整个 `entries` 清成 `entry{}`（不是遍历删）、`index = make(map[string]int, capacity*2)`、`head` 与 `count` 归 0。**源码注释观点**：清整环与整个 index，而不是"逐条清"，因为半清空是唯一会产生"陈旧 index 条目比它指名的 trace 活得更久"这种状态的做法；一个空环是平凡一致的。`Reset` 之后重新 `Add` 一个曾经存在过的 trace id 就是一次普通插入。

- `(*Store).WriteJSONL (internal/tracing/store.go)` —— `func (s *Store) WriteJSONL(w io.Writer, t *Trace) error`。作用：把一个 trace 渲染成一行紧凑 JSON（加一个 `'\n'`）。错误路径：`t == nil` 返回 `ErrNilTrace`（`tracing: nil trace`）；否则 `json.NewEncoder(w).Encode(t)` 的错误原样返回。读写状态：**不碰 store 内部**——这是刻意的 API 形状。**源码注释观点（两条）**：
  - 它故意是"接收 writer 和 trace 的函数"而不是渲染整个 store 的方法：渲染是本包里唯一会阻塞在进程外东西上的操作（管道、慢控制台、满磁盘），如果它是 store 的方法，就会诱使调用方在那次写期间持有 store 的 mutex，从而拖住请求路径上的每一个 `Add`。预期用法是先 `Get`（拿到的是克隆），然后在**不持锁**的情况下调 `WriteJSONL`。
  - nil trace 是错误而不是空行：在期待一条 trace 的位置写一个空行，产生的流"解析起来什么都没有"，因而**不会报告任何损坏**——静默的坏数据比响亮的错误贵得多。

## 17. tracing 的测试：意图反推

- `internal/tracing/context_test.go` —— `TestParseRoundTrip` 用采样/非采样、带/不带 `tracestate`、随机与最小 id 六种形状证明 `Header()` 是 `Parse()` 的逆，并额外断言 `Remote` 为 `true`、`Header()` 对同一个上下文稳定（幂等）。`TestParseRejects` 是一张含 29 条的畸形表，**每一条都同时断言 `ok == false` 且 `Context` 是零值**——注释解释了为什么零值断言不能省：部分填充的 `Context` 正是这张表存在的意义（它会产出一个 id 对不上任何东西的 span）。表里包含空、纯空白、前导/尾随空格、换行后缀、分段数 3/5、版本 `ff`/`01`/`0`/`000`/`0A`、trace id 过短/过长/非 hex/大写/全零/全 ff、span id 同样六种、flags 空/1 字符/3 字符/非 hex/大写。`TestParseFlagsBit` 记录"只有 bit 0 有意义"：`02`（random-trace-id 位）必须解析成未采样、`03` 解析成已采样。`TestParseTraceStateOpaque` 用 `""`、`"a=b"`、`"==="`、`"weird value with spaces"` 证明 tracestate 逐字携带且从不校验。`TestNewRootContextShape` 钉住根上下文的形状（32/16 位小写 hex、`Sampled == true`、`Remote == false`、`TraceState` 为空、自己的 header 必须能被 `Parse` 接受）。`TestGeneratedIDsAreUnique` 生成 10000 个 trace id 与 10000 个 span id，既查文法也查碰撞。`TestIsAllZeroOrAllFF` 直接覆盖保留值谓词，特别包含"混合值不算保留"的反例，并记录 `""` 是 vacuously true、调用方总是先查长度。

- `internal/tracing/store_test.go` —— 测试名字本身就是契约清单：`TestStoreEvictionAndDroppedAccounting`（淘汰与两个计数的语义：`Len` 是活 gauge、"Dropped" 是生命周期计数器）、`TestNewStoreDefaultCapacity`（`capacity <= 0` 即默认容量）、`TestStoreReset`（`Reset` 清空、保容量、保 `Dropped`，并且 Reset 前存在的 id 重新 `Add` 后必须能再次查到）、`TestStoreGetByTraceAndRequestID`（两个键、未命中、以及 `Get` 返回的是克隆——注释提醒大写未命中用例必须用"含字母"的字面量，否则 `traceIDFor(7)` 全数字、大写化是空操作，这条断言会静默失效）、`TestStoreAddDoesNotAliasCaller`、`TestStoreAddReplacesSameTraceID`（替换而非追加，且被替换的条目保持原位置，所以 `List` 顺序稳定）、`TestStoreAddNilIsNoop`、`TestStoreListOrderAndLimit`、`TestStoreListEmpty`、`TestStoreReAddEvictedTraceID`（这是"切片版 store 会做错"的回归测试：条目被淘汰之后，它的 trace id 必须解析不到东西，而不是解析到滑进它槽位的另一条 trace）、`TestListDoesNotCloneTraces`（`List` 必须是 O(limit) 且不深拷贝 span）、`TestWriteJSONL`（一行一个紧凑对象 + 一个换行、可逐行解码、且在**任何 store 锁之外**渲染；nil trace 是错误且不写任何字节）、`TestStoreConcurrentAddGetList`（`-race` 下并发写读，并断言"存活集合与淘汰集合恰好划分所有被 `Add` 的 id"——这正是证明环与索引不会在并发下漂移的方式）、`TestStoreConcurrentResetAndWriteJSONL`（用故意慢的 writer 证明 `WriteJSONL` 期间 store 未被锁住；注释说明真正的断言是"无 race、无死锁"）。

- `internal/tracing/trace_test.go` —— `TestSummaryFieldMapping`、`TestSummaryDefaults`、`TestTraceDuration`、`TestAddEnrichesSpan`、`TestAddDoesNotAliasCallerSpan`、`TestCloneIsolation`、`TestJSONRoundTrip`、`TestJSONRoundTripEmptyAndOmitEmpty`、`TestNanosToMS`、`TestTimeBaseConsistency`、`TestStrAttrAndEarliestStart`、`TestNewTraceShape`。这组名字对应的意图是：摘要字段的来源优先级（根 span 属性 vs trace 属性）、退化情形下的默认值、时长定义（最早开始到最晚结束）、`Add` 对派生字段的补全、克隆隔离的两个方向、JSON 字段名与 `omitempty` 的稳定性、`nanosToMS` 的取整、以及"整数是权威"在往返之后仍然成立。

---

## 18. internal/traceexport ：把 trace 送出进程

`internal/traceexport/jsonl.go` 的包注释规定了两个导出器共用的设计规则（**源码注释观点**）：每个导出器各拥有一个 worker goroutine 与一个有界队列，`Export` 只是把句柄拷进队列就返回；队列满就丢弃并计数，因为丢一条 trace 不能拖慢请求——计数器就是操作员得知此事的方式。两者都不是 metrics hook，都不在请求路径上加锁。

## 19. otlp.go ：OTLP/HTTP JSON 导出器

## 20. 常量

- `defaultOTLPTimeout (internal/traceexport/otlp.go)` —— `const defaultOTLPTimeout = 5 * time.Second`。作用：`OTLPOptions.Timeout <= 0` 时的默认值；它同时也是 `Close` 等待 worker 的上界。
- `defaultServiceName (internal/traceexport/otlp.go)` —— `const defaultServiceName = "infergate"`。作用：`OTLPOptions.ServiceName` 为空时的 resource `service.name`。
- `scopeName (internal/traceexport/otlp.go)` —— `const scopeName = "infergate.gateway"`。作用：OTLP scope 的名字，写进每个 payload 的 `scope.name`。
- `errBodyBytes (internal/traceexport/otlp.go)` —— `const errBodyBytes = 200`。作用：失败响应体回显进 `LastError` 的字节上限，也是读取响应体的 `io.LimitReader` 上限。

## 21. `OTLPOptions`

- `OTLPOptions (internal/traceexport/otlp.go)` —— `type OTLPOptions struct { Endpoint string; Timeout time.Duration; ServiceName string; Headers map[string]string; Buffer int; Logger *slog.Logger; Client *http.Client }`。逐字段契约：
  - `Endpoint` 必填，且必须是绝对的 `http`/`https` URL。
  - `Timeout <= 0` 时取 5 秒；它同时是 `Close` 等待 worker 完成的界。
  - `ServiceName` 为空时取 `"infergate"`。
  - `Headers` 在构造时被复制（`TestOTLPHeadersCopied` 钉住这点），所以调用方之后改自己那份 map 不影响导出器。
  - `Buffer <= 0` 时队列容量取 256。
  - `Logger` 为 nil 时所有告警静默丢弃。
  - `Client` 为 nil 时按 `Timeout` 造一个。

## 22. `OTLPStats`

- `OTLPStats (internal/traceexport/otlp.go)` —— `type OTLPStats struct { Exported int64; Failed int64; Dropped int64; LastError string; LastStatus int }`。作用：导出器的可观测快照。**源码注释观点**：它是值而不是指针，并且在导出器自己的 mutex 下取，所以读到的是一组一致的数。`LastStatus` 是 worker 最后看到的 HTTP 状态码（包括失败），`0` 表示请求从未完成（传输错误、超时、连接被拒）。

## 23. `OTLP`

- `OTLP (internal/traceexport/otlp.go)` —— `type OTLP struct { client *http.Client; url string; timeout time.Duration; serviceName string; headers map[string]string; ch chan *tracing.Trace; log *slog.Logger; stop chan struct{}; done chan struct{}; closeOnce sync.Once; exported atomic.Int64; failed atomic.Int64; dropped atomic.Int64; mu sync.Mutex; lastError string; lastStatus int }`。作用：一个后台 worker + 有界队列的 OTLP/HTTP 导出器。并发：`Export` 只碰 channel 与原子计数器（无锁）；worker 是唯一的 HTTP 执行者；`lastError`/`lastStatus` 由 `mu` 保护；`closeOnce` 让 `Close` 幂等。

- `NewOTLP (internal/traceexport/otlp.go)` —— `func NewOTLP(opts OTLPOptions) (*OTLP, error)`。作用：校验选项、拼端点、起 worker。错误字符串原文：
  - `traceexport: OTLP Endpoint is required`（未提供端点）；
  - `fmt.Errorf("traceexport: OTLP Endpoint %q: %w", opts.Endpoint, err)`（端点不能被 `url.Parse` 解析）；
  - `traceexport: OTLP Endpoint %q is not an absolute http/https URL`（可解析但不是绝对 http/https）。
  端点在这里校验而不是等到第一次 POST。**源码注释观点**：一个无法寻址的导出器应该在调用方还能看见错误的地方失败，而不是把每一个请求都变成一个静默计数的丢弃；这里显式强制绝对 http/https，是因为 `url.Parse` 几乎接受一切（`"collector"` 会被解析成一个相对路径，看起来完全正常）。成功路径：`URL = strings.TrimSuffix(opts.Endpoint, "/") + "/v1/traces"`，然后 `go o.worker()`。

- `(*OTLP).Export (internal/traceexport/otlp.go)` —— `func (o *OTLP) Export(t *tracing.Trace)`。作用：把 trace 交给 worker。读写状态：只写 channel 与 `dropped`。错误路径（没有 error 返回值，失败表现为计数）：`o == nil` 或 `t == nil` 直接返回；否则 `select { case o.ch <- t: ; case <-o.done: dropped.Add(1); default: dropped.Add(1) }`。**源码注释观点**：这里永不阻塞在网络上——队列满、或导出器已关闭，都丢弃并计数；trace 按引用存储（不克隆），因为它已经越过了"完成即不可变并移交"的边界。

- `(*OTLP).Close (internal/traceexport/otlp.go)` —— `func (o *OTLP) Close() error`。作用：请求 worker 停止、drain 队列、返回最后一个错误。实现：`o == nil` 返回 nil；`closeOnce.Do(close(o.stop))`；然后 `select` 等 `o.done` 或 `time.After(o.timeout)`；超时则 `o.logWarn("otlp: worker did not stop within the timeout; giving up", "endpoint", o.url)` 并返回 `o.lastErr()`；否则返回 `o.lastErr()`。语义：幂等（第二次调用只等同一个 `done` 并报同一个错误）；有界（从不超过一次 POST 的超时）。**源码注释观点**：超时放弃时，仍然排队中的 trace 计为 `dropped`，而不是假装投递成功。

- `(*OTLP).Stats (internal/traceexport/otlp.go)` —— `func (o *OTLP) Stats() OTLPStats`。作用：读快照。错误路径：`o == nil` 返回 `OTLPStats{}`（零值可直接用，`TestStatsZeroValue` 钉住这点）。实现：在 `mu` 下读 `lastError`/`lastStatus`，再 `Load` 三个原子计数器。

- `(*OTLP).lastErr (internal/traceexport/otlp.go)` —— `func (o *OTLP) lastErr() error`。作用：把 `lastError` 字符串变成 error。错误路径：`lastError == ""` 返回 nil，否则 `errors.New(o.lastError)`。

- `(*OTLP).setFailure (internal/traceexport/otlp.go)` —— `func (o *OTLP) setFailure(status int, msg string)`。作用：在 `mu` 下记录最后一次失败的状态码与消息。并发：持锁。

- `(*OTLP).logWarn (internal/traceexport/otlp.go)` —— `func (o *OTLP) logWarn(msg string, args ...any)`。作用：`o.log` 为 nil 时丢弃，否则 `Warn`。

- `(*OTLP).worker (internal/traceexport/otlp.go)` —— `func (o *OTLP).worker()`。作用：唯一做 HTTP 的 goroutine，顺序地一次一条。实现：`defer close(o.done)`；循环 `select`：收到 `<-o.stop` 就 `o.drain()` 然后返回；收到 `t := <-o.ch` 就 `o.post(t)`。**源码注释观点（关键取舍）**：它不会在第一次失败时停下——collector 短暂重启恰恰是"下一条 trace 仍然重要"的场景；如果放弃，导出器会永久死亡，而调用方只有去读 `Stats` 才能知道这件事。所以每个失败都只计数并保留。

- `(*OTLP).drain (internal/traceexport/otlp.go)` —— `func (o *OTLP).drain()`。作用：关闭时把已经在队列里的 trace 尽量发完。实现是非阻塞收的 `for { select { case t := <-o.ch: o.post(t); default: return } }`。

- `(*OTLP).post (internal/traceexport/otlp.go)` —— `func (o *OTLP).post(t *tracing.Trace)`。作用：编码并 POST 一条 trace。完整分支与计数行为：
  - `t == nil` 返回。
  - `encodeOTLP` 出错：`failed.Add(1)` + `setFailure(0, "otlp: encode: " + err.Error())`。
  - `context.WithTimeout(context.Background(), o.timeout)` 后 `http.NewRequestWithContext` 出错：`failed.Add(1)` + `setFailure(0, "otlp: request: " + err.Error())`。
  - 先 `req.Header.Set("Content-Type", "application/json")`，**然后**才写额外 headers。**源码注释观点**：放在最后是为了让显式选项可以覆盖默认值，而不是被静默忽略。
  - `client.Do` 出错：`failed.Add(1)` + `setFailure(0, "otlp: " + err.Error())`（状态 0 = 请求从未完成）。
  - 读响应：`raw, readErr := io.ReadAll(io.LimitReader(resp.Body, errBodyBytes))`，接着 `io.Copy(io.Discard, resp.Body)`，再 `resp.Body.Close()`。**源码注释观点**：每一条返回路径都必须把 body 读完并关闭，否则连接无法回到连接池，每次 POST 都要付一次新的握手。
  - 状态码不在 200..299：`failed.Add(1)`，`msg := "otlp: " + resp.Status + " " + string(bytes.TrimSpace(raw))`，`setFailure(resp.StatusCode, truncate(msg, errBodyBytes))`。
  - `readErr != nil` 只 `logWarn("otlp: reading response body", ...)`。**源码注释观点**：2xx 但 body 没读完仍然是成功——collector 已经接受了 payload，丢掉的只是一条池化连接。
  - 其余情况 `exported.Add(1)`。

- `truncate (internal/traceexport/otlp.go)` —— `func truncate(s string, n int) string`。作用：`len(s) <= n` 时原样返回，否则 `s[:n] + "..."`。

## 24. OTLP JSON 信封的形状类型

`otlp.go` 后半段用一组 struct 展开地写出了标准 OTLP/HTTP JSON 信封。**源码注释观点**：之所以不用 `map[string]any`，是因为 map 的迭代顺序会决定线格式；这些 struct 加排序后的属性切片让 payload 逐字节确定，测试才能对着 golden 字符串断言。

- `otlpEnvelope (internal/traceexport/otlp.go)` —— `type otlpEnvelope struct { ResourceSpans []otlpResourceSpans \`json:"resourceSpans"\` }`。
- `otlpResourceSpans (internal/traceexport/otlp.go)` —— `type otlpResourceSpans struct { Resource otlpResource \`json:"resource"\`; ScopeSpans []otlpScopeSpans \`json:"scopeSpans"\` }`。
- `otlpResource (internal/traceexport/otlp.go)` —— `type otlpResource struct { Attributes []otlpKeyValue \`json:"attributes"\` }`。作用：只承载 `service.name` 一个属性。
- `otlpScopeSpans (internal/traceexport/otlp.go)` —— `type otlpScopeSpans struct { Scope otlpScope \`json:"scope"\`; Spans []otlpSpan \`json:"spans"\` }`。
- `otlpScope (internal/traceexport/otlp.go)` —— `type otlpScope struct { Name string \`json:"name"\`; Version string \`json:"version,omitempty"\` }`。
- `otlpSpan (internal/traceexport/otlp.go)` —— `type otlpSpan struct { TraceID string \`json:"traceId"\`; SpanID string \`json:"spanId"\`; ParentSpanID string \`json:"parentSpanId,omitempty"\`; Name string \`json:"name"\`; Kind int \`json:"kind"\`; StartTimeUnixNano string \`json:"startTimeUnixNano"\`; EndTimeUnixNano string \`json:"endTimeUnixNano"\`; Attributes []otlpKeyValue \`json:"attributes,omitempty"\`; Events []otlpEvent \`json:"events,omitempty"\`; Status *otlpStatus \`json:"status,omitempty"\` }`。注意 `StartTimeUnixNano`/`EndTimeUnixNano` 是**字符串**——OTLP JSON 对 64 位整数就用十进制字符串表示。
- `otlpEvent (internal/traceexport/otlp.go)` —— `type otlpEvent struct { Name string \`json:"name"\`; TimeUnixNano string \`json:"timeUnixNano"\`; Attributes []otlpKeyValue \`json:"attributes,omitempty"\` }`。
- `otlpStatus (internal/traceexport/otlp.go)` —— `type otlpStatus struct { Code int \`json:"code"\` }`。作用：只在 status 非 unset 时出现；**源码注释观点**：缺失 status 在 OTLP 里的默认含义本来就是 unset，所以 `nil` 与 `code: 0` 表达同一件事，索性整个对象省略。
- `otlpKeyValue (internal/traceexport/otlp.go)` —— `type otlpKeyValue struct { Key string \`json:"key"\`; Value otlpValue \`json:"value"\` }`。
- `otlpValue (internal/traceexport/otlp.go)` —— `type otlpValue struct { StringValue *string \`json:"stringValue,omitempty"\`; BoolValue *bool \`json:"boolValue,omitempty"\`; IntValue *string \`json:"intValue,omitempty"\`; DoubleValue *float64 \`json:"doubleValue,omitempty"\`; ArrayValue *otlpArray \`json:"arrayValue,omitempty"\` }`。全部用指针，因为"未设置"与"零值"在 OTLP 的 `AnyValue` 里是两件不同的事。
- `otlpArray (internal/traceexport/otlp.go)` —— `type otlpArray struct { Values []otlpValue \`json:"values"\` }`。

## 25. 编码函数

- `encodeOTLP (internal/traceexport/otlp.go)` —— `func encodeOTLP(t *tracing.Trace, serviceName string) ([]byte, error)`。作用：把一条 `*tracing.Trace` 编码成一个 OTLP payload。处理规则：
  - `Resource` 属性只有 `service.name`，值来自 `serviceName` 参数。
  - `Scope.Name` = `scopeName`（`"infergate.gateway"`，常量）。
  - 时间字段用 `strconv.FormatInt(..., 10)` 写成十进制字符串。
  - **跳过 `EndUnixNano == 0` 的 span**。**源码注释观点**：OTLP 的 schema 无法表示"已开始但未结束"，把一个声明结束时间在纪元的 span 导出去会破坏下游每一个时长计算；跳过的 span **不计为失败**——那是被遗弃的 span，已经由 `tracing.StatusUnset` 建模了（`TestOTLPSkipsUnendedSpan` 钉住这一点）。
  - 最后 `json.Marshal(env)`。

- `otlpKind (internal/traceexport/otlp.go)` —— `func otlpKind(kind string) int`。作用：字符串 kind → OTLP 整数枚举。映射：`internal = 1`、`server = 2`、`client = 3`、其余 `0`（`TestOTLPKindMapping` 覆盖）。

- `otlpSpanStatus (internal/traceexport/otlp.go)` —— `func otlpSpanStatus(status string) *otlpStatus`。作用：字符串 status → OTLP status 对象。映射：`ok = 1`、`error = 2`、其余返回 `nil`（即整个 status 对象省略）。

- `encodeEvents (internal/traceexport/otlp.go)` —— `func encodeEvents(events []tracing.Event) []otlpEvent`。作用：逐个 event 转形状；`len==0` 时返回 `nil`（让 `omitempty` 生效）。

- `encodeAttributes (internal/traceexport/otlp.go)` —— `func encodeAttributes(attrs map[string]any) []otlpKeyValue`。作用：把属性 map 变成排序好的键值切片。三条规则：
  - `len == 0` 返回 `nil`。
  - **跳过 nil 值**。**源码注释观点**：OTLP 没有 null `AnyValue`，而 `fmt.Sprint(nil)` 会造出文本 `"<nil>"`——dashboard 会兴高采烈地把它画成一条曲线。
  - `sort.Strings(keys)`。**源码注释观点（这不是装饰）**：Go 的 map 迭代顺序是随机的，不排序的话两个本来相同的 trace 会产生不同的 payload——diff 不可读、无法按哈希去重、测试无法对 golden 断言。

- `anyValue (internal/traceexport/otlp.go)` —— `func anyValue(v any) otlpValue`。作用：单个 Go 值 → OTLP `AnyValue`。分支与理由：
  - `nil` → `otlpValue{}`。
  - `string`/`bool` → 对应指针字段。
  - `int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr` → 全部经 `intValue(strconv.FormatInt/FormatUint(..., 10))` 变成十进制字符串。**源码注释观点**：整数性由 Go 类型而非值决定——`int64(1)` 与 `float64(1.0)` 对 collector 意义不同；而 OTLP JSON 对 64 位整数本来就规定成十进制字符串。
  - `float32`/`float64` → `DoubleValue`。
  - `[]string` → `ArrayValue`，元素是 `StringValue`（循环里做了 `s := s` 以避免取到同一个循环变量地址）。
  - `default` → `fmt.Sprint(v)` 作 `StringValue`。**源码注释观点**：未知类型退化成文本而不是被丢弃；只有 `[]string` 有专用分支，因为它正是网关自身仪表写入的形状——扩大这个 `switch` 会改变 collector 原本看到字符串的那些值的类型，而线上的类型变更在 dashboard 建好之后是不可撤销的。

- `intValue (internal/traceexport/otlp.go)` —— `func intValue(decimal string) otlpValue`。作用：`otlpValue{IntValue: &decimal}`，把十进制字符串装进 `AnyValue` 的整数字段。

## 26. jsonl.go ：追加写 JSONL 导出器

## 27. 常量

- `defaultBuffer (internal/traceexport/jsonl.go)` —— `const defaultBuffer = 256`。作用：`JSONLOptions.Buffer <= 0` 时的队列容量。**源码注释观点**：单位是 trace 条数而不是字节——大约几百 KB 的指针句柄，在正常请求率下足以扛住数秒的磁盘/网络停顿。
- `jsonlFileMode (internal/traceexport/jsonl.go)` —— `const jsonlFileMode = 0o640`。作用：文件权限。**源码注释观点**：trace 携带 request id、route、model 名，所以"网关组可读"是对的，"全世界可读"不是。
- `jsonlWriterBuf (internal/traceexport/jsonl.go)` —— `const jsonlWriterBuf = 64 << 10`。作用：`bufio.Writer` 的大小（64KiB）。**源码注释观点**：一次突发只付一次 write 系统调用，而空闲导出器的常驻缓冲无足轻重。

## 28. `JSONLOptions` 与 `JSONL`

- `JSONLOptions (internal/traceexport/jsonl.go)` —— `type JSONLOptions struct { Path string; Buffer int; Logger *slog.Logger }`。逐字段契约：
  - `Path` 必填；文件以 `O_CREATE|O_WRONLY|O_APPEND` 打开。**源码注释观点**：重启的网关继续写上次的文件而不是把它毁掉；`O_APPEND` 同时让每一次写成为向末尾的原子追加，所以共享同一路径的第二个进程无法覆盖第一个进程的行。
  - `Buffer` 为队列容量，`<= 0` 取 `defaultBuffer`。
  - `Logger` 只用于真正需要人看的告警。**源码注释观点**：worker 不会为每一次丢弃打日志——满队列是成批丢弃的，每丢一行日志本身就是一场洪水；丢弃量应该从 `Dropped()`/`Err()` 读。

- `JSONL (internal/traceexport/jsonl.go)` —— `type JSONL struct { store *tracing.Store; path string; ch chan *tracing.Trace; closed atomic.Bool; done chan struct{}; closeOnce sync.Once; closeErr error; written atomic.Int64; dropped atomic.Int64; mu sync.Mutex; err error; log *slog.Logger }`。作用：单 worker、有界队列的 JSONL 追加导出器。并发：文件只被 `NewJSONL` 创建、由 worker 循环独占的那一个 goroutine 触碰，其他 goroutine 从不持有 `*os.File` 或 `bufio.Writer`；`err` 由 `mu` 保护；`written`/`dropped` 是原子计数器。**源码注释观点**：那个 `store` 字段是"一条 trace 的 store"（`tracing.NewStore(1)`）——`WriteJSONL` 虽是 `tracing.Store` 的方法，却完全不碰 store 内部，所以一个 store 就够用，而且不花环形缓冲的内存。

- `NewJSONL (internal/traceexport/jsonl.go)` —— `func NewJSONL(opts JSONLOptions) (*JSONL, error)`。作用：开文件并起 worker。错误字符串原文：
  - `traceexport: JSONL Path is required`；
  - `fmt.Errorf("traceexport: open %s: %w", opts.Path, err)`。
  **源码注释观点**：文件在这里同步打开，而不是让 worker 懒打开——错配的路径（目录、只读挂载、父目录拼错）必须在构造时失败，此时调用方还能决定"干脆不启用这个功能"，而不是在之后表现为一条静默的丢弃流。成功路径：`store: tracing.NewStore(1)`，然后 `go j.worker(f)`（文件交给 worker，此后这里不再使用；worker 退出时负责关闭它）。

- `(*JSONL).Export (internal/traceexport/jsonl.go)` —— `func (j *JSONL) Export(t *tracing.Trace)`。作用：入队。读写状态：只写 channel 与 `dropped`。错误路径（表现为计数）：`j == nil` 或 `t == nil` 返回；`j.closed.Load()` 为真则 `dropped.Add(1)` 并返回；否则 `select { case j.ch <- t: ; default: j.dropped.Add(1) }`。**源码注释观点**：按引用存储、不克隆，这样做是安全的，因为 `tracing.Trace` 的契约是"已完成的 trace 不可变并已移交"（网关的 `tracing.Store` 已经在那条边界上克隆过一次）；在这里再克隆会把每个 span 的深拷贝放到请求路径上，而那正是本类型要避免的开销。

- `(*JSONL).Close (internal/traceexport/jsonl.go)` —— `func (j *JSONL) Close() error`。作用：关闭队列、等 worker drain 完、返回错误。实现：`j == nil` 返回 nil；`closeOnce.Do(func(){ j.closed.Store(true); close(j.ch) })`；`<-j.done`；返回 `j.closeErr`。语义要点：
  - 幂等：第二次及以后等同一次 drain 并报同一个错误。
  - **不设超时**。**源码注释观点**：队列有界、通道关闭后无法再添加，只要文件系统还能用，drain 就一定会结束；卡死的文件系统不是这里任何 deadline 能修的，而截断 drain 反而会静默丢掉最值得留下的那些 trace（关闭前最后一秒的）。
  - 返回值是首个写/flush 错误（与 `Err()` 相同），否则是文件关闭错误，都没有则 nil。

- `(*JSONL).Written (internal/traceexport/jsonl.go)` —— `func (j *JSONL) Written() int64`。作用：已完成的写入条数。错误路径：`j == nil` 返回 0。语义：**它只在 `Close` 之后才到终值**——worker 在 drain 时递增，不是在 `Export` 时。

- `(*JSONL).Dropped (internal/traceexport/jsonl.go)` —— `func (j *JSONL) Dropped() int64`。作用：`Export` 因为队列满或已关闭而拒绝的条数。错误路径：`j == nil` 返回 0。

- `(*JSONL).Err (internal/traceexport/jsonl.go)` —— `func (j *JSONL) Err() error`。作用：最近一次错误。错误路径：`j == nil` 返回 nil。实现：在 `mu` 下返回 `j.err`。**源码注释观点（一次失败不终止 worker）**：暂时的 `ENOSPC` 后来被释放、或者日志轮转短暂替换了路径，都不该结束"进程余生里的 tracing"，所以 worker 继续 drain 并保留最近的错误。被接受的后果是：`bufio.Writer` 里已经丢掉的字节不重试——从 tracing 路径去重试一块坏磁盘，正是让调试助手变成故障的方式。

- `(*JSONL).setErr (internal/traceexport/jsonl.go)` —— `func (j *JSONL) setErr(err error)`。作用：在 `mu` 下记录错误（只保留最近一次）。

- `(*JSONL).worker (internal/traceexport/jsonl.go)` —— `func (j *JSONL) worker(f *os.File)`。作用：文件的所有者与唯一写者。流程：`defer` 里若文件尚未关闭则 `closeErr = f.Close()`，然后 `j.closeErr = errors.Join(j.Err(), closeErr)`，最后 `close(j.done)`；主体 `bw := bufio.NewWriterSize(f, jsonlWriterBuf)`，`j.run(bw)`，随后**再**做一次 `bw.Flush()`，失败则 `setErr(fmt.Errorf("traceexport: flush %s: %w", j.path, err))`。**源码注释观点**：这最后一次 flush 是"`Close` 立刻之前入队的 trace"真正落盘的地方；因为 `run` 在每批末尾都 flush，这次调用几乎总是 no-op，但保留它可以让"`Close` 会 flush"这个承诺不依赖 `run` 的内部流程（`errors.Join` 把首个错误与关闭错误合并，两者都保留）。

- `(*JSONL).run (internal/traceexport/jsonl.go)` —— `func (j *JSONL) run(bw *bufio.Writer)`。作用：主循环，实现"批量写 + 队列瞬时为空时 flush"的规则。实现要点：
  - 手写 `select` 循环，而不是 `for t := range j.ch`。**源码注释观点**：批处理需要知道"是否已经有另一条在等"，只有非阻塞接收能回答这个问题，而不会让 drain 停滞。
  - 规则是"**队列瞬时为空时 flush**"，于是一批 trace 变成一次系统调用，同时保证 `Close` 之前刚入队的那条也能在循环结束前落盘。
  - `flush` 是一个闭包，失败时 `setErr(fmt.Errorf("traceexport: flush %s: %w", j.path, err))`。
  - 主循环：`for t := range j.ch` 里先 `j.writeOne(bw, t)`；若 `bw.Buffered() >= jsonlWriterBuf` 则立刻 `flush()` 并 `continue`（**源码注释观点**：超过缓冲的突发必须及时下到 OS，否则这些字节会在突发期间留在缓冲里，暴露给之后的 flush 失败）；然后进入内层 `batch:` 循环做非阻塞收：收到就 `writeOne`，通道关闭就 `break batch`，取不到就 `break batch`；退出内层后 `flush()`。

- `(*JSONL).writeOne (internal/traceexport/jsonl.go)` —— `func (j *JSONL) writeOne(bw *bufio.Writer, t *tracing.Trace)`。作用：渲染一条 trace 并计数。错误路径：`t == nil` 返回（`Export` 已经拒绝过 nil，但这里选择跳过而不是写一个空行——`WriteJSONL` 把 nil 当错误，而 JSONL 流里的空行比缺失更糟：解析起来什么都没有，因而报告不了任何损坏）；`j.store.WriteJSONL(bw, t)` 失败则 `setErr(fmt.Errorf("traceexport: encode %s: %w", j.path, err))` 并返回；成功则 `j.written.Add(1)`。

## 29. traceexport 的测试：意图反推

`internal/traceexport/export_test.go` 的用例名构成一张契约表：`TestJSONLExport`（一行一个对象、可逐行解码、文件权限、追加语义）、`TestJSONLNonBlocking`（`Export` 不得阻塞）、`TestOTLPPayload`（对 golden payload 断言逐字节形状）、`TestOTLPKindMapping`、`TestOTLPSkipsUnendedSpan`、`TestOTLPAttributeTypes`、`TestOTLPAttributeRules`（nil 跳过、键排序）、`TestOTLPFailure`（非 2xx → `Failed` 与 `LastStatus`）、`TestOTLPTransportError`（`LastStatus == 0`）、`TestOTLPNewValidation`（三条错误字符串）、`TestOTLPNonBlocking`、`TestOTLPConcurrency`（`-race`）、`TestOTLPHeadersCopied`、`TestStatsZeroValue`。
---

## 30. internal/metrics ：仪表聚合

`internal/metrics/metrics.go` 的包注释（**源码注释观点**）：包定义的是网关依赖的仪表面；M0 有两个实现——二进制暴露为 Prometheus 文本的记录实现，与测试用的 no-op。把仪表表达成接口是为了让依赖指向内部：`internal/gateway` 从不 import 任何指标库，所以 M5 想换导出器时不必碰代理代码。

## 31. 结果分类与 `Sink` 接口

- `Outcome (internal/metrics/metrics.go)` —— `type Outcome string`。作用：一次请求或一次上游尝试的最终归属。
- `OutcomeSuccess (internal/metrics/metrics.go)` —— `const OutcomeSuccess = "success"`。
- `OutcomeUpstreamErr (internal/metrics/metrics.go)` —— `const OutcomeUpstreamErr = "upstream_error"`。
- `OutcomeTimeout (internal/metrics/metrics.go)` —— `const OutcomeTimeout = "timeout"`。
- `OutcomeCanceled (internal/metrics/metrics.go)` —— `const OutcomeCanceled = "canceled"`。
- `OutcomeBadRequest (internal/metrics/metrics.go)` —— `const OutcomeBadRequest = "bad_request"`。
- `OutcomeInternal (internal/metrics/metrics.go)` —— `const OutcomeInternal = "internal_error"`。
- `OutcomeRateLimited (internal/metrics/metrics.go)` —— `const OutcomeRateLimited = "rate_limited"`。

七个结果的划分里，`rate_limited` 是**网关因为预算（M3 quota）自己拒绝**的请求，**源码注释观点**：它故意不并入 `bad_request`——一个超标的租户是有 owner 的运营事件，看错误率的操作员需要它有独立的序列，混进 4xx 会让"客户端发错了"和"我们把人挡在外面"变成同一个数。

- `Sink (internal/metrics/metrics.go)` —— `type Sink interface { ObserveRequest(route, upstream, model string, status int, outcome Outcome, elapsed time.Duration); ObserveTokens(upstream, model string, prompt, completion, cached int); ObserveUpstreamAttempt(upstream string, status int, outcome Outcome, elapsed time.Duration); ObserveFailover(upstream string, outcome Outcome, status int); ObserveFirstToken(upstream, model string, elapsed time.Duration); ObserveStreamFrames(upstream string, frames, bytes int64) }`。作用：网关唯一的指标出口。实现必须并发安全，且不得阻塞请求路径。**源码注释观点**：`ObserveFailover` 与 `ObserveUpstreamAttempt` 分开报告，因为它们回答不同的问题——attempt 说"这个后端被试了多少次"，failover 说"网关判定它不值得等、于是放弃了多少次"；failover 率上升而 attempt 率平稳，就是"整支舰队一起变慢"的签名，只看单个后端的错误率是看不出来的。

- `Nop (internal/metrics/metrics.go)` —— `type Nop struct{}`，六个空方法：`func (Nop) ObserveRequest(string, string, string, int, Outcome, time.Duration) {}`、`func (Nop) ObserveTokens(string, string, int, int, int) {}`、`func (Nop) ObserveUpstreamAttempt(string, int, Outcome, time.Duration) {}`、`func (Nop) ObserveFailover(string, Outcome, int) {}`、`func (Nop) ObserveFirstToken(string, string, time.Duration) {}`、`func (Nop) ObserveStreamFrames(string, int64, int64) {}`。作用：测试与"指标关闭"的零成本实现。

- `DefaultLatencyWindow (internal/metrics/metrics.go)` —— `const DefaultLatencyWindow = 65536`。**源码注释观点**：65536 个样本在 `time.Duration` 是 8 字节的前提下等于 512KiB 的有界内存，按每秒一个请求算约一天流量；分位数必须描述"操作员此刻正在看的流量"，而不是"进程启动以来的所有请求"——无界切片既泄漏，又让 `/stats` 每次 scrape 都要为它排序。短于窗口的运行不受影响，这正是 3600 请求的 M2/M3 基线逐字节一致的原因。

## 32. `Recorder` 及其内部类型

- `Recorder (internal/metrics/metrics.go)` —— `type Recorder struct { mu sync.Mutex; requests map[requestKey]*requestStat; attempts map[attemptKey]*attemptStat; failovers map[failoverKey]int64; tokens map[tokenKey]*TokenStat; firstToken map[tokenKey]*latencyStat; streams map[string]*streamStat; requestLatency latencyRing; requestDuration map[requestDurationKey]*Histogram; attemptDuration map[string]*Histogram; firstTokenHist map[tokenKey]*Histogram; completionTokens map[tokenKey]*Histogram }`。作用：`Sink` 的记录实现。并发：**所有字段由那一把 `mu` 保护**。**源码注释观点**：时长被故意记两次——一次作为 `/metrics` 的直方图桶（有界、可导出），一次作为固定大小环里的原始样本供 `/stats` 计算精确的最近秩分位数；环的上限是 `DefaultLatencyWindow`，所以 recorder 的内存是配置的常数，而不是 uptime 的函数。

- `latencyRing (internal/metrics/metrics.go)` —— `type latencyRing struct { buf []time.Duration; next int; n int; dropped int64 }`。作用：固定容量、可"从最老的活跃样本开始按插入序"读出的环形样本缓冲。**源码注释观点**：它取代了曾经随 uptime 增长的无界 append；用淘汰而不是截断，使分位数仍然描述一个连续的近期窗口，而不是"进程生命里的前 N 个请求"。

- `(*latencyRing).observe (internal/metrics/metrics.go)` —— `func (r *latencyRing) observe(d time.Duration)`。流程：`len(buf) == 0` 直接返回（**源码注释观点**：手工构造而非经 `NewRecorder` 的 `Recorder` 没有窗口，不能在请求路径上 panic，只是不记样本）；若 `n == len(buf)` 则 `dropped++`，否则 `n++`；写 `buf[next] = d`；`next++`，到末尾就绕回 0。错误路径：无。并发：由 `Recorder.mu` 串行化。

- `(*latencyRing).samples (internal/metrics/metrics.go)` —— `func (r *latencyRing) samples() []time.Duration`。作用：按插入序导出样本副本。实现：`start := r.next`，若 `n < len(buf)` 则 `start = 0`（首次绕回前样本占 `buf[0:n]` 且 `next == n`），然后 append `buf[(start+i)%len(buf)]`。**源码注释观点**：插入序（而不是物理环序）是刻意的——要分位数的调用方反正会排序，而测试调用方需要一个"环绕过之后依然不变"的值。

- `(*latencyRing).reset (internal/metrics/metrics.go)` —— `func (r *latencyRing) reset()`。作用：`next = 0`、`n = 0`、`dropped = 0`。**源码注释观点**：保留 `buf`——`Reset` 不该强制一次 512KiB 的重新分配和随之而来的一轮 GC。

- `failoverKey (internal/metrics/metrics.go)` —— `type failoverKey struct { Upstream string; Outcome Outcome; Status int }`。**源码注释观点**：保留 status，因为"从 429 failover"是配额问题、"从 503 failover"是故障，操作员的响应完全不同。

- `requestKey (internal/metrics/metrics.go)` —— `type requestKey struct { Route string; Upstream string; Model string; Status int; Outcome Outcome }`。作用：请求计数的聚合维度（完整五维）。
- `attemptKey (internal/metrics/metrics.go)` —— `type attemptKey struct { Upstream string; Status int; Outcome Outcome }`。作用：单次上游尝试的聚合维度。**源码注释观点**：这里没有 route/model——尝试属于后端，不属于调用方那条路径。
- `tokenKey (internal/metrics/metrics.go)` —— `type tokenKey struct { Upstream string; Model string }`。作用：token 与首 token 两类指标的聚合维度。
- `requestDurationKey (internal/metrics/metrics.go)` —— `type requestDurationKey struct { Route string; Upstream string; Model string }`。**源码注释观点**：它故意省略 status 与 outcome（而 `requestKey` 保留它们）——按 outcome 拆分的延迟面板回答的是"失败有多慢"，而 SLO 问的是"用户真正等待的请求有多慢"。
- `requestStat (internal/metrics/metrics.go)` —— `type requestStat struct { Count int64; Seconds float64 }`。
- `attemptStat (internal/metrics/metrics.go)` —— `type attemptStat struct { Count int64; Seconds float64 }`。
- `TokenStat (internal/metrics/metrics.go)` —— `type TokenStat struct { Prompt int64; Completion int64; Cached int64; Requests int64 }`。
- `latencyStat (internal/metrics/metrics.go)` —— `type latencyStat struct { Count int64; Seconds float64 }`（首 token 用）。
- `streamStat (internal/metrics/metrics.go)` —— `type streamStat struct { Frames int64; Bytes int64 }`。

- `NewRecorder (internal/metrics/metrics.go)` —— `func NewRecorder() *Recorder { return NewRecorderWithWindow(0) }`。
- `NewRecorderWithWindow (internal/metrics/metrics.go)` —— `func NewRecorderWithWindow(n int) *Recorder`。作用：构造 recorder 并指定延迟环容量。错误路径：无；`n <= 0` 时取 `DefaultLatencyWindow`（所以 `NewRecorder()` 与 `NewRecorderWithWindow(0)` 等价）。**源码注释观点**：小 `n` 的存在只为让测试能证明淘汰确实发生；网关总是取默认值。

## 33. 记录方法与快照方法

- `(*Recorder).ObserveRequest (internal/metrics/metrics.go)` —— `func (m *Recorder) ObserveRequest(route, upstream, model string, status int, outcome Outcome, elapsed time.Duration)`。作用：记一次完成的请求。流程：`secs := elapsed.Seconds()`；`Lock`/`defer Unlock`；`requests[requestKey]` 的 `Count++`、`Seconds += secs`；`m.requestLatency.observe(elapsed)`；`requestDuration[requestDurationKey]` 直方图（为 nil 时用 `defaultDurationBuckets` 新建）`h.Observe(secs)`。错误路径：无。并发：持 `mu`。

- `(*Recorder).ObserveTokens (internal/metrics/metrics.go)` —— `func (m *Recorder) ObserveTokens(upstream, model string, prompt, completion, cached int)`。作用：累加 provider 自报的三个 token 数与 `Requests++`，并把 `float64(completion)` 记进 `completionTokens` 直方图。**源码注释观点**：这里用补全 token 而不是 prompt+completion——prompt 长度是"客户端发了什么"的属性，已由 token 计数器完整描述；补全长度才是网关真正在等、流式客户端真正体验成时长的东西。

- `(*Recorder).ObserveUpstreamAttempt (internal/metrics/metrics.go)` —— `func (m *Recorder) ObserveUpstreamAttempt(upstream string, status int, outcome Outcome, elapsed time.Duration)`。作用：记一次出站尝试（含重试）；`attemptDuration[upstream]` 用 `defaultDurationBuckets`。

- `(*Recorder).ObserveFailover (internal/metrics/metrics.go)` —— `func (m *Recorder) ObserveFailover(upstream string, outcome Outcome, status int)`。作用：`failovers[failoverKey]++`。语义：`status` 是被放弃的那次尝试的 HTTP 状态码，若那次尝试从未产生响应（拨号失败、超时、流被重置）则为 `0`。

- `(*Recorder).ObserveFirstToken (internal/metrics/metrics.go)` —— `func (m *Recorder) ObserveFirstToken(upstream, model string, elapsed time.Duration)`。作用：`firstToken[tokenKey]` 的 `Count++` 与 `Seconds += elapsed.Seconds()`，并把秒数记进 `firstTokenHist`（`defaultFirstTokenBuckets`）。

- `(*Recorder).ObserveStreamFrames (internal/metrics/metrics.go)` —— `func (m *Recorder) ObserveStreamFrames(upstream string, frames, bytes int64)`。作用：`streams[upstream]` 的 `Frames`/`Bytes` 累加。

- `FailoverSample (internal/metrics/metrics.go)` —— `type FailoverSample struct { Upstream string; Outcome Outcome; Status int; Count int64 }`。
- `(*Recorder).FailoverSnapshot (internal/metrics/metrics.go)` —— `func (m *Recorder) FailoverSnapshot() []FailoverSample`。作用：按 `Upstream, Outcome, Status` 升序快照。并发：`Lock` 下拷贝。

- `RequestSnapshot (internal/metrics/metrics.go)` —— `type RequestSnapshot struct { Route string; Upstream string; Model string; Status int; Outcome Outcome; Count int64; TotalSeconds float64; MeanSeconds float64 }`。
- `(*Recorder).Snapshot (internal/metrics/metrics.go)` —— `func (m *Recorder) Snapshot() []RequestSnapshot`。作用：组装请求计数快照；`Count > 0` 时 `MeanSeconds = st.Seconds / float64(st.Count)`；最后 `sortRequests(out)`。

- `(*Recorder).TokenSnapshot (internal/metrics/metrics.go)` —— `func (m *Recorder) TokenSnapshot() map[tokenKey]TokenStat`。作用：值拷贝的 map，按未导出 key 组织（包内使用）。
- `TokenSnapshotRow (internal/metrics/metrics.go)` —— `type TokenSnapshotRow struct { Upstream string; Model string; Prompt int64; Completion int64; Cached int64; Requests int64 }`。**源码注释观点**：把未导出的 key 展平成字段，使包外调用方不必依赖未导出的 key 类型。
- `(*Recorder).TokenSnapshotRows (internal/metrics/metrics.go)` —— `func (m *Recorder) TokenSnapshotRows() []TokenSnapshotRow`。作用：按 `(Upstream, Model)` 升序的行列表。
- `(*Recorder).TokenTotals (internal/metrics/metrics.go)` —— `func (m *Recorder) TokenTotals() (prompt, completion, cached int64)`。作用：跨所有 (upstream, model) 求和。

- `FirstTokenSample (internal/metrics/metrics.go)` —— `type FirstTokenSample struct { Upstream string; Model string; Mean time.Duration; Count int64 }`。
- `(*Recorder).FirstTokenSnapshot (internal/metrics/metrics.go)` —— `func (m *Recorder) FirstTokenSnapshot() []FirstTokenSample`。作用：跳过 `Count == 0` 的条目；`Mean = time.Duration(v.Seconds / float64(v.Count) * float64(time.Second))`；按 `(Upstream, Model)` 升序。

- `StreamSample (internal/metrics/metrics.go)` —— `type StreamSample struct { Upstream string; Frames int64; Bytes int64 }`。
- `(*Recorder).StreamSnapshot (internal/metrics/metrics.go)` —— `func (m *Recorder) StreamSnapshot() []StreamSample`。作用：按 `Upstream` 升序。

- `(*Recorder).RequestLatencySamples (internal/metrics/metrics.go)` —— `func (m *Recorder) RequestLatencySamples() []time.Duration`。作用：返回环样本的拷贝。语义（**源码注释观点**）：调用方由此算出的分位数是"最近 `min(已记录请求数, 窗口)` 个请求"的；短于窗口的运行（本仓库里的每一个负载测试）看到的样本与从前完全一样，所以结论不变；更长的运行丢掉最老样本而不是永远保留，丢了多少由 `RequestLatencyDropped` 报告；顺序是插入序，所以无论环绕了多少次，值都是确定的。

- `(*Recorder).RequestLatencyWindow (internal/metrics/metrics.go)` —— `func (m *Recorder) RequestLatencyWindow() int`。作用：返回 `len(m.requestLatency.buf)`，让 `/stats` 能在不重复默认值的前提下公布"分位数所依据的样本总体有多大"。
- `(*Recorder).RequestLatencyDropped (internal/metrics/metrics.go)` —— `func (m *Recorder) RequestLatencyDropped() int64`。**源码注释观点**：之所以把它导出来，是因为"截断窗口上的分位数"和"全部数据上的分位数"是两个不同的主张——一个只显示 p99 而没有这个数的 dashboard，分不清自己展示的是"最近 65536 个请求"还是"自启动以来"。

- `RequestDurationHistogram (internal/metrics/metrics.go)` —— `type RequestDurationHistogram struct { Route string; Upstream string; Model string; Count int64; Sum float64; Counts []int64 }`，方法 `func (RequestDurationHistogram) Bounds() []float64 { return DefaultDurationBuckets() }` 与 `func (r RequestDurationHistogram) Buckets() []Bucket { return cumulativeBuckets(DefaultDurationBuckets(), r.Counts) }`。
- `(*Recorder).RequestDurationHistograms (internal/metrics/metrics.go)` —— `func (m *Recorder) RequestDurationHistograms() []RequestDurationHistogram`，按 `(Route, Upstream, Model)` 升序。
- `AttemptDurationHistogram (internal/metrics/metrics.go)` —— `type AttemptDurationHistogram struct { Upstream string; Count int64; Sum float64; Counts []int64 }`，`Bounds()` → `DefaultDurationBuckets()`，`Buckets()` → `cumulativeBuckets(DefaultDurationBuckets(), a.Counts)`。**源码注释观点**：它是尝试计时的唯一导出——`attemptStat` 带着 count 与 sum，但 `/metrics` 从不渲染它，所以此前"被网关重试掉的慢后端"在每一次 scrape 里都是不可见的。
- `(*Recorder).AttemptDurationHistograms (internal/metrics/metrics.go)` —— `func (m *Recorder) AttemptDurationHistograms() []AttemptDurationHistogram`，按 `Upstream` 升序。
- `FirstTokenHistogram (internal/metrics/metrics.go)` —— `type FirstTokenHistogram struct { Upstream string; Model string; Count int64; Sum float64; Counts []int64 }`，边界族是 `DefaultFirstTokenBuckets()`。
- `(*Recorder).FirstTokenHistograms (internal/metrics/metrics.go)` —— `func (m *Recorder) FirstTokenHistograms() []FirstTokenHistogram`，按 `(Upstream, Model)` 升序。
- `CompletionTokensHistogram (internal/metrics/metrics.go)` —— `type CompletionTokensHistogram struct { Upstream string; Model string; Count int64; Sum float64; Counts []int64 }`，边界族是 `DefaultTokensPerRequestBuckets()`。
- `(*Recorder).CompletionTokensHistograms (internal/metrics/metrics.go)` —— `func (m *Recorder) CompletionTokensHistograms() []CompletionTokensHistogram`，按 `(Upstream, Model)` 升序。

- `(*Recorder).Reset (internal/metrics/metrics.go)` —— `func (m *Recorder) Reset()`。作用：`Lock` 下把 6 个 map 全部换成新 map、`m.requestLatency.reset()`、4 个直方图 map 换成新 map。**源码注释观点**：它存在是为测试与"只测量一个阶段"的基准服务。

## 34. histogram.go ：累积桶与三类边界

- `Bucket (internal/metrics/histogram.go)` —— `type Bucket struct { UpperBound float64; Count int64 }`。作用：**累积桶**——`Count` 是 `<= UpperBound` 的观测数。**源码注释观点**：因为 Prometheus 就是这个形状——文本格式没有"每桶一条序列"，只有"每个边界一条 `le` 序列"，要区间就相减；把累积数交给调用方，可以让这次算术在这里做一次，而不是每次 scrape 在 server 里做一次。`UpperBound` 对 duration 是秒、对 token 是 token 数，最终 catch-all 是 `+Inf`。

- `(Bucket).IsInf (internal/metrics/histogram.go)` —— `func (b Bucket) IsInf() bool { return math.IsInf(b.UpperBound, 1) }`。作用：识别 catch-all 桶。
- `(Bucket).Le (internal/metrics/histogram.go)` —— `func (b Bucket) Le() string`。作用：渲染 `le` 标签值。`IsInf` 时返回 `"+Inf"`，否则 `strconv.FormatFloat(b.UpperBound, 'g', -1, 64)`。**源码注释观点**：用最短可往返形式，是为了让 `0.001` 保持成 `"0.001"` 而不是漂成 `"0.0010000000000000001"`——后者会破坏按标签匹配的 dashboard。

- `Histogram (internal/metrics/histogram.go)` —— `type Histogram struct { bounds []float64; counts []int64; count int64; sum float64 }`。作用：固定桶直方图。**源码注释观点（为什么是桶而不是原始样本）**：网关要跑好几周，为了算分位数而保留每个请求的时长，等于把"延迟功能"变成一个内存泄漏。并发：`Histogram` 自身**非并发安全**，由所有者持锁——`Recorder` 已经把全部状态串行在一把 mutex 后面，每个直方图再加一把锁会在请求路径上把临界区翻倍。语义：`counts[i]` 统计 `(bounds[i-1], bounds[i]]`，最后一个是 `+Inf` 桶；`len(counts) == len(bounds)+1`；`counts` **故意非累积**，一次观测只碰一个元素。

- `NewHistogram (internal/metrics/histogram.go)` —— `func NewHistogram(bounds []float64) *Histogram`。作用：构造并规范化边界。实现要点：复制传入的 bounds（**源码注释观点**：调用方可能传一个共享的包级切片）；丢弃非有限边界（`+Inf` 是隐式的最后桶，不能同时是一个有限边界；`NaN` 会让 `sort.SearchFloat64s` 静默地把观测放错位置）；`sort.Float64s`；原地折叠重复值（**源码注释观点**：两个相同边界会造出一个永远不可能收到观测的桶——输出里的一个静默空洞，操作员会把它读成"这个区间没有请求"，而不是"边界表有 bug"）；`counts: make([]int64, len(out)+1)`。错误路径：无错误返回，坏输入被规范化掉。

- `(*Histogram).Observe (internal/metrics/histogram.go)` —— `func (h *Histogram) Observe(v float64)`。作用：记一次观测。对坏值的处理：**NaN 或 `v < 0` 一律钳成 `0`**。**源码注释观点**：观测来自墙上时钟算术（请求路径上的 `time.Since`、上游自报时长），NTP 跳变或时钟倒退会产生小的负时长，诚实的读法是"快到测不出来"，而不是"请求在开始之前就结束了"；放过去还会破坏导出的 `sum`——每个 dashboard 都拿它除以 `count` 求均值——而且这个破坏会持续整个进程生命周期。正常路径：`h.count++`、`h.sum += v`、`h.counts[sort.SearchFloat64s(h.bounds, v)]++`。**源码注释观点**：`SearchFloat64s` 返回第一个 `>= v` 的边界下标，所以正好落在边界上的观测进入该边界的桶，这正是 `le` 所承诺的语义。

- `HistogramSnapshot (internal/metrics/histogram.go)` —— `type HistogramSnapshot struct { Bounds []float64; Counts []int64; Count int64; Sum float64 }`。语义：`Bounds` 是升序的有限上界，隐式的 `+Inf` 桶不列在这里；`Counts` 非累积且 `len(Counts) == len(Bounds)+1`，`sum(Counts) == Count`；`Count` 是总观测数，`Sum` 是总和，`Sum/Count` 就是寿命均值。
- `(*Histogram).Snapshot (internal/metrics/histogram.go)` —— `func (h *Histogram) Snapshot() HistogramSnapshot`。作用：拷贝式快照（`append([]float64(nil), h.bounds...)` 与 `append([]int64(nil), h.counts...)`）。
- `(HistogramSnapshot).Buckets (internal/metrics/histogram.go)` —— `func (s HistogramSnapshot) Buckets() []Bucket { return cumulativeBuckets(s.Bounds, s.Counts) }`。
- `cumulativeBuckets (internal/metrics/histogram.go)` —— `func cumulativeBuckets(bounds []float64, counts []int64) []Bucket`。作用：把非累积计数变成累积桶。流程：按序累加每个 bound，然后 `cum += counts[len(counts)-1]` 并 append `Bucket{UpperBound: math.Inf(1), Count: cum}`。**源码注释观点**：`+Inf` 桶只在这一处追加——`HistogramSnapshot`、以及 `Recorder` 那些展平的行类型共用同一个函数，所以"最后一定要有 +Inf"这个不变量只有一个实现点。

## 35. 三类桶边界的取值与理由

- `DefaultDurationBuckets (internal/metrics/histogram.go)` —— `func DefaultDurationBuckets() []float64`，返回 `defaultDurationBuckets` 的副本。
- `defaultDurationBuckets (internal/metrics/histogram.go)` —— `var defaultDurationBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}`。**源码注释观点**：集合的上半部分是 Prometheus 自己的默认时长桶（5ms 到 10s），向下扩展到 1ms 与 2.5ms——网关自身开销（上游健康时操作员真正要看的那个数）在 5ms 以下，否则它会藏在第一个桶里；10s 以上的请求已经失去用户，更细的切分回答不了任何问题。间距"大致几何"是故意的：从直方图读分位数要在桶内插值，等宽桶会把健康网关 90% 的流量塞进第一个桶、把事故时 90% 的流量塞进最后一个桶，两种读法都无用。
- `DefaultFirstTokenBuckets (internal/metrics/histogram.go)` —— `func DefaultFirstTokenBuckets() []float64`，返回 `defaultFirstTokenBuckets` 的副本。
- `defaultFirstTokenBuckets (internal/metrics/histogram.go)` —— `var defaultFirstTokenBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}`。**源码注释观点**：TTFT 由上游的 prefill 主导（几十到几百毫秒）；真实模型不可能低于 5ms，而 2.5s 以上用户已经认定产品坏了——所以这套边界比请求时长更紧、且整体上移。
- `DefaultTokensPerRequestBuckets (internal/metrics/histogram.go)` —— `func DefaultTokensPerRequestBuckets() []float64`，返回 `defaultTokensPerRequestBuckets` 的副本。
- `defaultTokensPerRequestBuckets (internal/metrics/histogram.go)` —— `var defaultTokensPerRequestBuckets = []float64{1, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096}`。**源码注释观点**：取 2 的幂，是因为补全长度重尾、并且是按量级读的——操作员问的是"这条路由返回的是约 100 还是约 2000 tokens"，不是"是否正好 96"；上界高于任何配置的 `max_tokens` 所允许的补全长度，所以 `+Inf` 桶就是"provider 忽略了上限"的可见信号。

## 36. sort.go ：确定性排序

- `sortRequests (internal/metrics/sort.go)` —— `func sortRequests(rs []RequestSnapshot)`。作用：用 `sort.Slice` 按 `Route`、`Upstream`、`Model`、`Status`、`Outcome` 依次升序排列。**源码注释观点**：确定性排序使 `/metrics` 输出与测试断言不依赖 Go 随机化的 map 迭代顺序——没有它，"同一次 scrape 两次输出不同"会变成一个永远修不干净的测试抖动源。

## 37. metrics 的测试：意图反推

`internal/metrics/metrics_test.go` 的用例名对应这些契约：`TestHistogramObservePlacement`（正好落在边界上的观测进入该桶）、`TestHistogramInvariants`（`len(Counts) == len(Bounds)+1`、`sum(Counts) == Count`）、`TestHistogramBoundsAreCopiedAndSanitised`（传入切片被复制、非有限与重复边界被处理）、`TestHistogramEmptyBuckets`（空边界表只有一个 `+Inf` 桶）、`TestRecorderLatencyWindow`、`TestRecorderDefaultWindow`（`0` 即默认窗口）、`TestRecorderWindowIsBounded`（淘汰真的发生且被计数）、`TestRecorderHistogramFamilies`、`TestRecorderHistogramOrderingIsDeterministic`、`TestRecorderConcurrentObserveAndSnapshot`（`-race`）。基准用例把这一点说得很清楚：`BenchmarkLegacyRecorderObserveRequest`、`BenchmarkRecorderObserveRequest`、`BenchmarkHistogramObserve`、`BenchmarkRequestDurationMapLookup`、`BenchmarkRequestLatencySamples`、`BenchmarkRequestLatencySamplesSmall`、`BenchmarkPercentilesUnsortedPerCall`、`BenchmarkPercentilesFromSorted`——也就是说，"记录一次请求"的开销、以及"每次调用都重新排序"与"利用已排序"的对比，是被当成会回归的性能契约来测的。

---

## 38. internal/server 的 /metrics 文本 exposition

`/metrics` 由 `internal/server/server.go` 里的 `handleMetrics` 提供（路由注册为 `GET /metrics`），`Content-Type` 是 `text/plain; version=0.0.4; charset=utf-8`。**源码注释观点**：exposition 是手写的而不是用 `client_golang`——M0 的零第三方依赖是硬约束，而 exposition 格式本身是一个稳定的小契约。

## 39. 渲染骨架

- `(*Server).handleMetrics (internal/server/server.go)` —— 作用：把 recorder 的快照渲染成 Prometheus 文本。读写状态：只读指标状态（各自在 recorder 的锁下取快照），收集完再一次写出。错误路径：单个数据源的失败不升级为 5xx（见 `cacheEntries`）。并发：请求路径上不加锁，只在读快照时短暂持锁。

- `writeHistogramFamily (internal/server/server.go)` —— `func writeHistogramFamily(b *strings.Builder, name, help string, labelNames []string, series []histogramSeries)`。作用：渲染**一个**直方图族。顺序：先 `# HELP %s %s`，再 `# TYPE %s histogram`；然后对每个 series，按 `labelNames` 生成 `name=%q` 的基础标签列表，通过 `withLe(le string)` 追加 `le=%q`，逐桶写 `%s_bucket{%s} %d`，最后写 `%s_count%s %d` 与 `%s_sum%s %g`。**源码注释观点（关键）**：exposition 格式在这里是严格的，而且"形状只写一次、不是每族一次"——每个边界一条带 `le` 的 `_bucket` 序列，随后是由同一条序列导出的 `_count` 与 `_sum`；**没有序列的族仍然写 HELP 与 TYPE**，这是 scraper 得知"这个指标存在、只是当前空闲"的方式——如果改发一条没有标签的裸 bucket 行，那会是一个畸形序列。

- `labelSuffix (internal/server/server.go)` —— `func labelSuffix(labels []string) string`。作用：空切片返回 `""`，否则 `"{" + strings.Join(labels, ",") + "}"`。存在理由是让 `_count`/`_sum` 这类"没有 `le`"的行与 bucket 行共用同一套标签拼接。

- `histogramSeries (internal/server/server.go)` —— `type histogramSeries struct { labels []string; count int64; sum float64; buckets []metrics.Bucket }`。作用：`writeHistogramFamily` 的输入形状——标签已经渲染成 `k="v"` 字符串，桶是累积桶。

- 四个转换函数：`requestDurationSeries (internal/server/server.go)`（标签 `route, upstream, model`）、`attemptDurationSeries (internal/server/server.go)`（标签 `upstream`）、`firstTokenSeries (internal/server/server.go)`（标签 `upstream, model`）、`completionTokensSeries (internal/server/server.go)`（标签 `upstream, model`）。作用：把 `metrics` 的行类型转成 `histogramSeries`，把 `Bucket` 的 `Le()` 交给 `withLe`。

- `writeProcessMetric (internal/server/server.go)` —— `func writeProcessMetric(b *strings.Builder, name, typ, help, value string)`。作用：渲染一条"单值指标"——`# HELP %s %s`、`# TYPE %s %s`、`%s %s`。名字来自它最初的用途（进程/运行时指标），但 build_info 这类单值指标也走它。

- `writeRuntimeMetrics (internal/server/server.go)` —— `func writeRuntimeMetrics(b *strings.Builder)`。作用：渲染整个运行时块。**源码注释观点**：它在开头一次性调 `runtime.ReadMemStats`，而不是每一行懒读——否则一次 scrape 会报出"某一瞬间的 goroutine 数"和"另一个瞬间的堆大小"，而这一整块的意义恰恰是让这些数能被相互关联。

- `cacheEntries (internal/server/server.go)` —— `func cacheEntries(ctx context.Context, c *cache.Cache) int`。作用：读缓存条目数。错误路径：`c.Store().Len(ctx, "")` 出错时返回 `-1`。**源码注释观点（为什么是 -1 而不是 500）**：这个模板刻意宽容——它喂的是一个 gauge，一个短暂不可用的 store（比如 Redis 正在重启）不应该在其余每个数都还有效的时候，把整个 `/metrics` 变成一个 500。

## 40. 指标族清单（HELP 文案为源码原文）

基础与请求路径：

| 指标 | 类型 | HELP |
| --- | --- | --- |
| `infergate_build_info` | gauge | Build information, always 1. |
| `infergate_requests_total` | counter | Completed requests by route, upstream, model, status and outcome. |
| `infergate_request_duration_seconds` | histogram | Request duration by route, upstream and model. |
| `infergate_upstream_attempt_duration_seconds` | histogram | Duration of one outbound upstream attempt, including retries. |
| `infergate_tokens_total` | counter | Provider-reported tokens by upstream and model. |
| `infergate_completion_tokens_per_request` | histogram | Completion tokens produced per request. |
| `infergate_stream_frames_total` | counter | SSE frames relayed downstream. |
| `infergate_stream_bytes_total` | counter | SSE bytes relayed downstream. |
| `infergate_first_token_seconds_mean` | gauge | Mean time to first streamed token. |
| `infergate_first_token_seconds` | histogram | Time to first streamed token by upstream and model. |
| `infergate_failovers_total` | counter | Attempts abandoned in favour of another backend. |

标签细节：`infergate_build_info` 带 `version="…"`，值恒为 1；`infergate_requests_total` 带 `route/upstream/model/status/outcome`，其中 status 先 `strconv.Itoa` 再按 `%q` 渲染；`infergate_tokens_total` 对每个 `(upstream, model)` 发 `kind="prompt"`、`kind="completion"`、`kind="cached"`、`kind="requests"` 四行；`infergate_first_token_seconds_mean` 的值是 `%g` 的 `row.Mean.Seconds()`；`infergate_failovers_total` 带 `upstream/outcome/status`。**源码注释观点**：`stream_frames_total` 与 `stream_bytes_total` 两个族的 HELP/TYPE 必须在**任何样本之前**发出——exposition 里这两族是按 upstream 交错的，而 Prometheus 读者以它看到的第一个同名行作为声明，夹在两条样本行之间的 `# TYPE` 会到得太晚。

熔断器族：`infergate_breaker_state`（gauge，"Current circuit breaker state, as a one-hot gauge."，带 `upstream/state`，对 `StateClosed`/`StateHalfOpen`/`StateOpen` 各发一行，命中为 1、否则 0）、`infergate_breaker_trips_total`（counter，"Times a breaker opened since process start."）、`infergate_breaker_rejections_total`（counter，"Attempts refused because a breaker was not closed."）、`infergate_breaker_failure_ratio`（gauge，"Windowed failure ratio the breaker is acting on."）。

cache 族（仅当 `s.cache != nil` 时渲染）：`infergate_cache_lookups_total`（"Cache lookups performed."）、`infergate_cache_hits_total`（"Cache hits, by kind."，`kind="exact"`/`kind="semantic"`）、`infergate_cache_misses_total`（"Lookups that fell through to an upstream."）、`infergate_cache_stores_total`（"Responses stored for later reuse."）、`infergate_cache_errors_total`（"Cache operations that failed and degraded to an upstream call."，`kind="lookup"`/`kind="store"`）、`infergate_cache_entries`（gauge，"Current entry count."，带 `store="…"`）、`infergate_cache_evictions_total`（"Entries dropped to stay within the per-scope bound."）、`infergate_cache_hit_ratio`（gauge，"Share of lookups answered from the cache."）、`infergate_cache_saved_tokens_total`（"Provider tokens a cache hit avoided regenerating."，`kind="prompt"`/`kind="completion"`）。

quota 族（仅当 `s.quota != nil && s.quota.Enabled()` 时渲染）：`infergate_quota_decisions_total`（"Admission decisions by outcome."，`action="allow"`/`"degrade"`/`"reject"`）、`infergate_quota_store_errors_total`（"Counter operations that failed."）、`infergate_quota_alerts_total`（"Tenant spend anomalies observed."）、`infergate_quota_tokens_total`（"Reserved, settled and released token counts."，`kind="reserved"`/`"settled"`/`"released"`）、`infergate_quota_overshoot_tokens_total`（"Tokens spent beyond the reservation."）、`infergate_quota_overshoot_cost_micros_total`（"Spend beyond the reservation, in micro-dollars."）、`infergate_quota_released_cost_micros_total`（"Reserved spend returned to the tenant, in micro-dollars."）。

## 41. idempotency 与 session 族

- `writeM6Metrics (internal/server/m6.go)` —— `func writeM6Metrics(b *strings.Builder, store *idempotency.Store, ledger *sessions.Ledger)`。作用：渲染 `idempotency` 与 `session` 两族。**源码注释观点（为什么没有租户标签）**：基数（cardinality）就是原因——在一个 per-process store 上加租户维度，等于把调用方发明的每一个 tenant id 都变成一条新的时间序列，而 tenant header 是调用方控制的；per-tenant 的细节住在 `/admin/idempotency` 与 `/admin/sessions` 里。两族都受 nil 保护：`store != nil` 与 `ledger != nil` 才渲染。

idempotency 族（HELP 原文）：`infergate_idempotency_lookups_total`（"Idempotency-Key lookups performed."）、`infergate_idempotency_hits_total`（"Keyed requests answered by replaying a stored answer."）、`infergate_idempotency_misses_total`（"Keyed requests that did the work themselves."）、`infergate_idempotency_conflicts_total`（"Reuses of a key with a different body."）、`infergate_idempotency_in_flight_rejects_total`（"Concurrent duplicates refused while the first was running."）、`infergate_idempotency_stored_total`（"Answers remembered against their key."）、`infergate_idempotency_oversize_total`（"Answers served but not remembered because they exceeded max_response_bytes."）、`infergate_idempotency_aborted_total`（"Claims released because the attempt failed and must be retried for real."）、`infergate_idempotency_evicted_total`（"Stored answers dropped to stay within capacity."）、`infergate_idempotency_expired_total`（"Stored answers dropped because their TTL passed."），再加三个 gauge：`infergate_idempotency_entries`（"Stored answers currently held."，值来自 `store.Len()`）、`infergate_idempotency_in_flight`（"Claims currently running."，来自 `store.InFlight()`）、`infergate_idempotency_scopes`（"Distinct tenant scopes holding at least one entry."，来自 `store.ScopeCount()`）。

session 族（HELP 原文）：`infergate_session_records_total`（"Requests recorded in a session ledger."）、`infergate_session_no_id_total`（"Requests that carried no session id and were therefore not attributed."）、`infergate_session_evictions_total`（"Sessions dropped to stay within capacity."）、`infergate_session_expirations_total`（"Sessions dropped because they went idle past the TTL."）、`infergate_sessions_tracked`（gauge，"Sessions currently held."）、`infergate_sessions_tenants`（gauge，"Distinct tenants with at least one tracked session."），以及 `infergate_sessions_requests`（gauge，"Requests attributed to currently tracked sessions."）、`infergate_sessions_cost_usd`（gauge，"Spend attributed to currently tracked sessions."）、`infergate_sessions_tokens`（gauge，"Tokens attributed to currently tracked sessions."，带 `kind="prompt"`/`"completion"`/`"cached"`）。后三个的值由遍历 `ledger.List(0)` 现场累加得出（`requests`、`cost`、`prompt`/`completion`/`cached`）。**源码注释观点（类型选择是承重的）**：这三个是 gauge 而不是 counter——一个在容量上限处被淘汰的 session 会把它自己的请求数与花费从总和里带走，所以如果写成 counter，这个数就会**下降**，而 Prometheus 的 counter 不允许下降。

## 42. 运行时与进程族

`writeRuntimeMetrics` 渲染：`infergate_runtime_goroutines`（gauge，"Live goroutines in this process."）、`infergate_runtime_num_cpu`（gauge，"Logical CPUs available to this process, as runtime.NumCPU sees them."）、`infergate_runtime_memstats_alloc_bytes`（gauge，"Bytes of allocated heap objects, including unreachable ones not yet collected."）、`infergate_runtime_memstats_heap_alloc_bytes`（gauge，"Bytes of allocated heap objects."）、`infergate_runtime_memstats_heap_inuse_bytes`（gauge，"Heap bytes in spans that have at least one live object."）、`infergate_runtime_memstats_heap_objects`（gauge，"Allocated heap objects."）、`infergate_runtime_memstats_stack_inuse_bytes`（gauge，"Bytes in stack spans."）、`infergate_runtime_memstats_sys_bytes`（gauge，"Bytes obtained from the OS for the runtime, including unused spans."）、`infergate_runtime_memstats_total_alloc_bytes`（counter，"Cumulative bytes allocated over the process lifetime, even if freed."）、`infergate_runtime_memstats_gc_cycles_total`（counter，"Completed GC cycles since process start."）、`infergate_runtime_gc_pause_seconds_total`（counter，"Cumulative stop-the-world GC pause time."）、`infergate_runtime_gc_last_pause_seconds`（gauge，"Most recent stop-the-world GC pause; 0 before the first collection."，取值 `ms.PauseNs[(ms.NumGC+255)%256]/1e9`，且仅在 `NumGC > 0` 时非零）、`infergate_runtime_go_version`（gauge，"Go runtime the binary was built against, always 1."，带 `version="…"`）。

---

## 43. internal/mockbackend ：进程内假上游

包注释（**源码注释观点**）：`mockbackend` 之所以是库而不只是 `cmd/mockupstream`，是为了让一个验收器能在同一个进程里起**多个行为不同**的后端（一个健康、一个坏掉、一个很慢），并精确检查每一个被问了什么。需要两种不同行为的验收器没法只用那个全局 mock 二进制，除非去起并配置一堆子进程；而 per-backend 的请求日志正是"路由与故障转移真的发生了"的证据——否则只能从响应里去推断。

## 44. 注入头常量

- `HeaderStatus (internal/mockbackend/mockbackend.go)` —— `const HeaderStatus = "X-Mock-Status"`，注释原文：reply with this HTTP status instead。
- `HeaderDelay (internal/mockbackend/mockbackend.go)` —— `const HeaderDelay = "X-Mock-Delay"`，sleep this long before answering。
- `HeaderTTFB (internal/mockbackend/mockbackend.go)` —— `const HeaderTTFB = "X-Mock-TTFB"`，sleep this long before the first stream frame。
- `HeaderOmitEnd (internal/mockbackend/mockbackend.go)` —— `const HeaderOmitEnd = "X-Mock-Omit-Done"`，stream without the `[DONE]` sentinel。
- `HeaderUpstreamName (internal/mockbackend/mockbackend.go)` —— `const HeaderUpstreamName = "X-Mock-Upstream"`。作用：每个后端在响应上回写自己的名字，让客户端不读 body 也能知道是哪个副本答的。

**源码注释观点（这一组头的存在理由）**：它们是 per-request 的，所以**同一个后端**可以对一次调用健康、对下一次坏掉——这正是让测试能在不重启任何东西的前提下演练熔断器状态迁移的原因。

## 45. 选项、请求日志、后端

- `Options (internal/mockbackend/mockbackend.go)` —— `type Options struct { Name string; TTFB time.Duration; TokenDelay time.Duration; FailStatus int; Methods bool }`。逐字段：`Name` 会出现在 `X-Mock-Upstream` 与 `/healthz` 里，让验收器能分辨是哪个副本答的；`TTFB` 是首帧的默认延迟，会被请求头覆盖；`TokenDelay` 给流式 content 帧之间定速，零表示尽快；`FailStatus` 非零时让每个 chat 请求都以该状态失败（除非请求显式带了 `X-Mock-Status`），这是"永久坏掉的后端"的建模方式；`Methods` 为真会在模型列表里追加一个 `methods` 能力（能力路由由网关配置，不由后端决定）。

- `Call (internal/mockbackend/mockbackend.go)` —— `type Call struct { Path string; Model string; Stream bool; Authorization string; Header http.Header }`。作用：一次被记录下来的请求。语义：`Authorization` 单独留一个字段，是为了让验收器能直接断言"凭证被透传了"而不用去 header map 里翻。

- `Backend (internal/mockbackend/mockbackend.go)` —— `type Backend struct { *httptest.Server; name string; ttfb time.Duration; delay time.Duration; failState atomic.Int32; stallFor atomic.Int64; mu sync.Mutex; calls []Call }`。作用：一个正在运行的后端加上它的请求日志。并发：`calls` 由 `mu` 保护；`failState`/`stallFor` 是原子值，所以可以在请求进行中改变行为。

- `New (internal/mockbackend/mockbackend.go)` —— `func New(opts Options) *Backend`。作用：在一个临时端口上起后端。错误路径：没有 error 返回值；`opts.Name == ""` 时取 `"mock"`。关闭是调用方的责任（通过内嵌的 `*httptest.Server`）。注册的路由：`/healthz`（返回 `{"status":"ok","upstream":name}`）、`/v1/models`（返回一个 `mock-gpt` 的模型列表）、`/calls`、以及 `/v1/chat/completions` 与 `/v1/embeddings`。
  - `/calls` 的语义：`DELETE` 会 `b.Reset()` 并返回 `{"reset":true}`，其余方法返回 `{"calls": …, "count": …}`。**源码注释观点**：请求日志通过 HTTP 暴露，是为了让子进程不可能成为唯一的证人——验收器直接读它，人在排障时也可以 curl。
  - **源码注释观点（路径形状）**：`/v1` 前缀在分发前被 `strings.TrimPrefix` 掉，因为网关逐字转发调用方的路径——客户端 POST `/v1/chat/completions` 到达这里时就是那个完整路径，而不是 mux 模式写的那条裸路径；早先的版本正是因为在完整路径上匹配，才让每个请求都变成 404。两种形状（带 `/v1` 与不带）都被注册，因为 base_url 本身可能已经以 `/v1` 结尾。

- `(*Backend).Calls (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) Calls() []Call`。作用：返回日志的副本（`make` + `copy`，调用方拥有它）。并发：`mu` 下拷贝。

- `(*Backend).Count (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) Count() int`。作用：这个后端见过的 chat 请求数。并发：`mu`。

- `(*Backend).Reset (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) Reset()`。作用：把 `calls` 置 nil。并发：`mu`。

- `(*Backend).SetFailStatus (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) SetFailStatus(status int)`。作用：运行时改变"永久注入的状态"。**源码注释观点**：可以在一个测试内先弄坏一个后端再修好它；替代方案（重启后端来"修好"它）同时会重置它的连接池与熔断窗口，而那两个正是被测状态。

- `(*Backend).SetStall (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) SetStall(d time.Duration)`。作用：运行时让这个后端变慢（传 0 恢复）。**源码注释观点**：同样是刻意的运行时控制——为了改变速度而重启后端，会顺带重置连接池与熔断窗口，而超时场景的全部意义就在于"网关对**已经存在**的状态做了什么"。

- `(*Backend).stall (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) stall(header time.Duration) time.Duration`。作用：决定这次要等多久。优先级：**header 值（已由 `chat` 解析并校验）> 运行时值（`stallFor`）> 配置值（`ttfb`）**。错误路径：无。

## 46. chat 路径：头的解析顺序与停滞的时机

- `(*Backend).chat (internal/mockbackend/mockbackend.go)` —— 作用：处理一次 chat 请求。完整顺序与理由：
  1. 非 `POST` → `405` + `errorBody("method not allowed", "invalid_request_error")`。
  2. 解 JSON body（只解 `model`、`stream`、`tools`）；失败 → `400` + `errorBody("invalid JSON body: "+err.Error(), "invalid_request_error")`。
  3. 在 `mu` 下把请求追加进 `calls`（记录 `Path`、`Model`、`Stream`、`Authorization`、`r.Header.Clone()`）。
  4. **在任何字节被写出之前**解析两个计时头：`durationHeader(r, HeaderTTFB)` 与 `durationHeader(r, HeaderDelay)`，任一失败 → `400` + `errorBody(err.Error(), "invalid_request_error")`。**源码注释观点**：计时头在写任何东西之前解析，所以一个不可读的值是一个"点名了那个头"的 400，而不是静默的"没有停滞"；`HeaderStatus` 的优先级在这个检查之后（一个注入了 status 的请求永远不会走到 sleep），而这个先后顺序比这次检查更早。
  5. 停滞：`if stall := b.stall(ttfb); stall > 0 { time.Sleep(stall) }`。**源码注释观点（两条）**：停滞作用于**每一个**回答、流式与非流式都一样，并且发生在注入的 status 或真实响应被写出来之前——只把它绑在流式路径上，会让一个非流式的"慢后端"在微秒内作答，于是上游超时场景什么也证明不了（网关根本没有理由放弃它）；"在注入 status 之前睡"同样重要——一个**慢慢地失败**的后端才能让故障转移的代价可测量，而一个快速失败的后端会把这个代价藏起来。
  6. 注入状态：`X-Mock-Status` 非空则 `strconv.Atoi`，失败 → `400` + `errorBody("X-Mock-Status must be an integer", "invalid_request_error")`；成功则 `writeJSON(w, code, errorBody(fmt.Sprintf("injected status %d", code), "mock_error"))` 并返回。**源码注释观点**：一个永久坏掉的后端仍然尊重 per-request 的显式状态，所以一个实例可以同时服务"永远 503"（熔断测试）与"只 503 一次"（故障转移测试）。
  7. 永久失败：`b.failState.Load() != 0` → 该状态 + `errorBody(fmt.Sprintf("%s is permanently failing with %d", b.name, code), "mock_error")`。
  8. `delay > 0` 则 `time.Sleep(delay)`。
  9. `req.Model == ""` → `400` + `errorBody("you must provide a model parameter", "invalid_request_error")`。
  10. `req.Stream` 为真走 `streamChat`，否则写一个完整的 `chat.completion`（`id: "chatcmpl-mock"`，`choices[0].message.content = "mock answer from " + b.name`，`finish_reason: "stop"`，`usage: {"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}`）。

- `(*Backend).streamChat (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) streamChat(w http.ResponseWriter, r *http.Request, model string)`。作用：写真正的 SSE。细节：
  - `w.(http.Flusher)` 不支持 → `http.Error(w, "streaming unsupported", http.StatusInternalServerError)`。
  - 设置 `Content-Type: text/event-stream`、`Cache-Control: no-cache`、`X-Mock-Upstream: <name>`，然后 `w.WriteHeader(200)` 并 `Flush()`。
  - **源码注释观点**：这里**不再停滞**——延迟只在 `chat` 里施加一次，在任何 header 或 status 被选中之前；如果两个地方都睡，配置的延迟会翻倍，而且"首 token 时间"的测量会取决于后端到底查了几次时钟。
  - `emit` 闭包：`json.Marshal` 后写 `data: %s\n\n`、`Flush()`，若 `b.delay > 0` 则 `time.Sleep(b.delay)`。
  - 帧序（每帧都是 `chat.completion.chunk`）：role 帧（`delta: {"role":"assistant","content":""}`）→ 两个 content 帧（`"mock "`、`"answer"`）→ finish 帧（`delta: {}`，`finish_reason: "stop"`）→ usage 帧（`choices: []`，带 `{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}`）→ 若 `X-Mock-Omit-Done` 为空则写 `data: [DONE]\n\n` 并 flush。

- `durationHeader (internal/mockbackend/mockbackend.go)` —— `func durationHeader(r *http.Request, name string) (time.Duration, error)`。作用：解析一个计时头。错误字符串原文：`fmt.Errorf("%s must be a duration such as \"250ms\": %q", name, v)`（不可解析）与 `fmt.Errorf("%s must not be negative: %q", name, v)`（负数）。空值返回 `(0, nil)`。**源码注释观点（为什么不可读的值是错误而不是 0）**："没有停滞"是对一个被忽略的头的**貌似合理**的读法——一个本想拖慢后端的验收器反而会得到瞬时回答，于是一个超时场景什么也证明不了。`mockbackend_test.go` 用 `TestUnreadableTimingHeadersAreRejected`（`"300"`、`"soon"`、`"-1s"`、ttfb `"300"`、`"-1ms"` 全部 400 且 body 里点名该头）、`TestReadableTimingHeadersAreHonoured`（`40ms` 必须真的让非流式回答也晚到）、`TestHeaderStallWinsOverTheRuntimeStall`（`20ms` 头必须压过 `300ms` 运行时停滞；不带头时 `300ms` 必须生效）把这三条钉死。

- `readAll (internal/mockbackend/mockbackend.go)` —— `func readAll(r *http.Request) ([]byte, error)`。作用：安全读完 body（`r.Body == nil` 返回 `nil, nil`，否则 `defer Close` + `io.ReadAll`）。
- `writeJSON (internal/mockbackend/mockbackend.go)` —— `func writeJSON(w http.ResponseWriter, status int, v any)`。作用：设 `Content-Type: application/json`、写状态、`json.NewEncoder(w).Encode(v)`（编码错误被显式忽略）。
- `errorBody (internal/mockbackend/mockbackend.go)` —— `func errorBody(message, typ string) map[string]any`。作用：构造 OpenAI 形状的错误信封 `{"error":{"message":…,"type":…,"param":nil,"code":nil}}`。

---

## 47. cmd/mockupstream ：独立假上游

`cmd/mockupstream/main.go` 的包注释（**源码注释观点**）：它存在的理由是 InferGate 的 M0 出口标准是"一条真实的 SSE 流到达真实客户端"，而拿付费 provider 去验证这件事会让测试变慢、变贵、不确定、且无法在 CI 里跑。这个 mock 是一个真实的 `net/http` 服务器，说真实的线上协议——chunked SSE 帧、provider 风格的 usage 块、分册的 `tool_call` delta——所以网关是被真正的 HTTP 演练的，而不是被一个 mock `RoundTripper` 演练的。它同时是 M1 的参照物：路由器必须挺过的每一种上游行为（慢首字节、429、500、没有 `[DONE]` 就截断的流）在这里都能通过请求头触达，这才让故障转移与熔断在没有 provider 账号的情况下可测。

## 48. flags 与进程装配

`main (cmd/mockupstream/main.go)` 的 flags：

- `-listen`（默认 `":9000"`）；
- `-name`（默认 `"mock"`，出现在 `/healthz` 里，用来识别是哪个副本答的）；
- `-ttfb`（默认 0，首帧前的人为延迟，用来演练 time-to-first-token）；
- `-token-delay`（默认 `15ms`，流式 content 帧之间的间隔）。**源码注释观点**：默认值让生成延迟对肉眼和对验收脚本都可观察；负载测试要把它设成 0，否则每个 token 睡 15ms 测的是 mock 而不是网关。
- `-script`（默认空 = 今天的行为）。

装配顺序：`slog` 文本 handler 输出到 stdout；`-script` 非空则 `loadScript`，失败时 `logger.Error("scripted mode unavailable", "err", err)` 后 `os.Exit(2)`（**源码注释观点**：拒绝启动才是重点——一个因为脚本读不出来而"照常应答"的 mock，会把 agent 运行的失败变成一个谜案，而不是一个启动错误）；然后构造 `server`；注册路由；`ReadHeaderTimeout: 10s` 且**显式没有 `WriteTimeout`**（**源码注释观点**：否则会切断一条故意很慢的流）；`signal.Notify` 捕获 `os.Interrupt` 与 `syscall.SIGTERM`，收到后 `httpSrv.Close()`；`ListenAndServe` 返回非 `http.ErrServerClosed` 的错误时 `os.Exit(1)`。

`server` 结构与路由：

- `server (cmd/mockupstream/main.go)` —— `type server struct { name string; ttfb time.Duration; tokenDelay time.Duration; log *slog.Logger; script *script; mux *http.ServeMux; callCount atomic.Int64; turn atomic.Int64; toolCallsEmitted atomic.Int64; dropped atomic.Int64; failed atomic.Int64; byPath countSink; byRule countSink }`。注释要点：`script` 只在给了 `-script` 时非空，此时 chat 路径在作答前会咨询它，文件里其余部分完全不知道"脚本模式"的存在；`mux` 是这个服务器挂载的路由表，测试自己造一个并直接驱动真实 handler，而不是起进程；那批计数器在每次 chat 请求的**响应写完之后**更新一次，所以它们的含义是"真的被服务过的请求数"。
- 路由表（`main` 里注册）：`/healthz`、`/models` 与 `/v1/models`、`/chat/completions` 与 `/v1/chat/completions`、`/embeddings` 与 `/v1/embeddings`、`/calls`、`/stats/calls`。**源码注释观点**：两种路径形状都注册是有意的——网关在前面时它转发调用方的路径（通常是 `/v1/...`），客户端直连时也是同一个路径，两者都注册就让这个 mock 在任一位置都能用、无需配置；而计数器端点**是**一个 gate 证明"幂等重放造成了零次额外上游调用"的唯一方式——没有任何别的办法见证一个"被回答之后又被丢掉"的请求。

- `(*server).healthz (cmd/mockupstream/main.go)` —— 返回 `{"status":"ok","upstream":s.name}`。
- `(*server).models (cmd/mockupstream/main.go)` —— 返回 `mock-gpt`、`mock-reasoner`、`mock-tool` 三个模型（`owned_by: "infergate-mock"`）。
- `(*server).embeddings (cmd/mockupstream/main.go)` —— 返回一个 3 维向量 `[0.01,-0.02,0.03]` 与 `usage: {"prompt_tokens":3,"total_tokens":3}`。
- `writeJSON (cmd/mockupstream/main.go)` —— `func writeJSON(w http.ResponseWriter, status int, v any)`。注意它比 `mockbackend` 的版本多一行：`enc.SetEscapeHTML(false)`——让 `<`、`>`、`&` 在 JSON 里原样出现，从而使 SSE 帧与工具调用参数逐字节可比。
- `errorBody (cmd/mockupstream/main.go)` —— 同形状的错误信封构造器。
- `truncate (cmd/mockupstream/main.go)` —— `func truncate(s string, max int) string`，超长时 `s[:max] + "..."`。
- `tokenCount (cmd/mockupstream/main.go)` —— `func tokenCount(req chatRequest) int`。作用：一个刻意粗糙的 tokenizer 替身（每条 message 的空白分词数 + 4，全零则 1）。**源码注释观点**：它只需要确定且非零，M3 会拿真实 provider 的 usage 块去对账。
- `completionFor (cmd/mockupstream/main.go)` —— `func completionFor(req chatRequest) string`。作用：产生一个**依赖问题本身**的确定性回答（`fmt.Sprintf("mock answer to %q", truncate(last, 120))`，无用户消息时用 `"nothing"`）。**源码注释观点**：这样测试才能断言因果，而不是只断言"回来了一些文本"。
- `chatRequest (cmd/mockupstream/main.go)` —— `type chatRequest struct { Model string; Stream bool; Messages []chatMessage; Tools json.RawMessage }`。
- `chatMessage (cmd/mockupstream/main.go)` —— `type chatMessage struct { Role string; Content string }`。

## 49. chat 路径与注入头

- `(*server).chat (cmd/mockupstream/main.go)` —— 处理一次 chat。顺序与理由：
  1. 非 `POST` → 405 + `method not allowed`。
  2. **先读计时头**：`durationHeader(r, "X-Mock-Delay")` 然后 `durationHeader(r, "X-Mock-TTFB")`，任一失败 → 400 并点名该头。**源码注释观点（重要）**：计时头必须在选择响应形状之前读，这样一个不可读的值才是"点名那个头的 400"，而不是静默的"没有延迟"；这个区别就是重点——注入的停滞是 gate 用来制造它随后要测量的那个窗口的手段，一个把不可读的 `"300"`（而不是 `"300ms"`）在微秒内答掉的 mock，会让 gate 去测量一个从未被拖慢的 provider。同时注释明确记录了**与 `internal/mockbackend` 的差异**：那边刻意让 `X-Mock-TTFB` 的停滞也发生在注入 status 之前（因为那里被测量的就是"慢慢地失败"），而这边一个畸形请求即使同时带着 `X-Mock-Status` 也仍然按名字回答；并且 `X-Mock-TTFB` **过去**是在 SSE header 已经提交之后才解析的，那时 400 已经不可能了。
  3. `X-Mock-Status` 非空 → `strconv.Atoi`，失败 → 400 + `X-Mock-Status must be an integer`；成功 → 该状态 + `injected status %d`（`type: "mock_error"`），立即返回。**源码注释观点**：注入的状态被立即回答、发生在下面的延迟之前——延迟是用来围绕一个真实答案扩宽窗口的，而一个 status **就是**答案。
  4. `delay > 0` 则 `time.Sleep(delay)`。
  5. 解码 body → 失败 400 + `invalid JSON body: …`；`req.Model == ""` → 400 + `you must provide a model parameter`。
  6. 打一行结构化日志（model / stream / messages 数 / tools 是否存在 / path）。
  7. 脚本选择：`s.script != nil` 时 `m := s.script.selectResponse(req, req.Stream, r.URL.Path, s.nextTurn())`。**源码注释观点**：选择发生在上面那行日志之后（让日志形状与非脚本模式一致），也发生在 counting writer 存在之后（让脚本化回答与被内置的回答被完全一样地计数）。
  8. 包裹计数：`opts := countOpts{path: r.URL.Path}`，脚本命中时 `opts.rule = match.RuleName`（有规则对象时用 `match.Rule.Name`），然后 `cw := &countingWriter{ResponseWriter: w, server: s, opts: opts}` 与 `defer cw.record()`。**源码注释观点**：每一个 chat 回答——脚本化的或内置的——都经由一个 counting writer 服务，所以 `/calls` 计数是**构造上**精确的：计数发生在响应真的被写出来之后，覆盖每一条返回路径；如果改成在每条响应路径里各自自增，就需要在六个地方各配一次，而漏掉的那一处会让一次幂等重放看起来像真的调用了上游。
  9. `match != nil && !match.Legacy` 时调 `s.answerScripted(cw, r, req, m)`；返回 true 就结束。**源码注释观点**：一个 legacy 匹配（有脚本但没有规则、也没有 default）留下 `match.Respond == nil`，于是掉到下面的内置回答，今天的响应因此逐字节不变；脚本化回答的成功路径也一样——"回答 how"是这个函数的职责，不是 `answerScripted` 的，后者只处理失败与 drop。
  10. `req.Stream` 为真走 `streamChat(cw, r, req, match, ttfb)`，否则 `wholeChat(cw, req, match)`。

- `durationHeader (cmd/mockupstream/main.go)` —— `func durationHeader(r *http.Request, name string) (time.Duration, error)`。行为与 `mockbackend` 的同名函数一致，错误字符串也是那两条；**源码注释观点**：一个不可读或负的值是错误而不是 0，因为"没有延迟"是对一个被忽略的头的貌似合理的读法——请求会成功、gate 会通过，而唯一从未发生的事情就是 gate 想注入的那个效果。

- `countOpts (cmd/mockupstream/main.go)` —— `type countOpts struct { path string; rule string }`。作用：在响应开始之前就被填好，让计数能归因到产生这个回答的脚本规则（或内置默认）。
- `countingWriter (cmd/mockupstream/main.go)` —— `type countingWriter struct { http.ResponseWriter; server *server; opts countOpts; code int; written bool; dropped bool; toolCall int }`。**源码注释观点**：把计数集中在一个地方而不是每条响应路径的末尾，才让这批计数器可信——gate 拿它们证明幂等重放造成了零次额外上游调用，因此一条忘了计数的路径就是一次假通过。
- `(*countingWriter).wantsDrop (cmd/mockupstream/main.go)` —— `func (w *countingWriter) wantsDrop()`，置 `dropped`。**源码注释观点**：没有这个标志，一个被故意掐断的连接与"一个什么都没写的 handler"就无法区分。
- `(*countingWriter).countToolCalls (cmd/mockupstream/main.go)` —— `func (w *countingWriter) countToolCalls(n int)`，累加 `toolCall`；gate 断言的就是这个数在一次幂等重放前后保持为 0。
- `(*countingWriter).WriteHeader (cmd/mockupstream/main.go)` —— 首次调用记录 `code`，然后转发。
- `(*countingWriter).Write (cmd/mockupstream/main.go)` —— 首次写且未显式写过 header 时把 `code` 记为 200，然后转发。
- `(*countingWriter).Flush (cmd/mockupstream/main.go)` —— 若底层实现了 `http.Flusher` 就转发。**源码注释观点**：没有它，被 flush 的帧会缓冲起来，流就不再是增量的。
- `(*countingWriter).Hijack (cmd/mockupstream/main.go)` —— 转发 `http.Hijacker`，底层不支持时返回 `http.ErrNotSupported`。作用：让 drop 路径能放弃连接而不是用一个状态码作答。
- `(*countingWriter).record (cmd/mockupstream/main.go)` —— `func (w *countingWriter) record()`。作用：把 writer 的最终状态记一次账。规则：既没写也没被 drop → 直接返回（**源码注释观点**：一个什么都没产生的 chat 请求不是"被服务过的请求"）；`failed := w.written && w.code >= http.StatusBadRequest`；最后 `w.server.record(w.opts.path, w.opts.rule, w.toolCall, w.dropped, failed)`。

## 50. 非流式与流式回答

- `(*server).wholeChat (cmd/mockupstream/main.go)` —— `func (s *server) wholeChat(w http.ResponseWriter, req chatRequest, match *scriptMatch)`。作用：用单个 JSON completion 作答（非流式形状）。行为：默认 `content = completionFor(req)`、`finishReason = "stop"`、`usage` 由 `tokenCount(req)` 与 `len(strings.Fields(completionFor(req)))` 算出；脚本命中的非 legacy 分支改用 `match.respond(req)` 与 `match.usageFor(req, content, len(calls))`；有 tool calls 时写进 `message["tool_calls"]` 并通过 `interface{ countToolCalls(int) }` 上报个数；最后 `writeJSON` 一个 `chat.completion`（`id: "chatcmpl-mock-1"`）。**源码注释观点**：非流式路径上的 usage 就是 M3 的全部要点——网关必须读 provider 的数字，而不是从文本里估算。

- `(*server).streamChat (cmd/mockupstream/main.go)` —— `func (s *server) streamChat(w http.ResponseWriter, r *http.Request, req chatRequest, match *scriptMatch, ttfb time.Duration)`。作用：写真正的 SSE，一个 flushed chunk 对应一个 token 粒度的 delta。细节与理由：
  - 不支持 `Flusher` → `http.Error(w, "streaming unsupported", http.StatusInternalServerError)`。
  - 先设 `Content-Type: text/event-stream`、`Cache-Control: no-cache`、`X-Mock-Upstream: <name>`。
  - **停滞发生在 header 被提交并 flush 之前**：若 `s.ttfb > 0`，`select { case <-time.After(s.ttfb): case <-r.Context().Done(): return }`；然后才 `w.WriteHeader(200)` 与 `flusher.Flush()`。**源码注释观点（这一段是本 mock 的全部要点）**：如果响应头先出去，网关会立刻转发它们，于是客户端对"首字节时间"的每一次测量都会读到约 2ms，尽管一个 token 都还没到——停滞变得不可观察，检查就静默地什么都没证明；把 header 扣到第一个 token 才发，是真实 provider 的行为，也是让 time-to-first-token 可测量的原因。
  - 调用方给的停滞（`ttfb`，已在 `chat` 里、在 header 出去之前解析）随后再 `select` 一次。**源码注释观点**：这是流式超时路径的测试手段；因为它是在 header 之后施加的，所以在这里回答"没有停滞"就正是这个 mock 绝不能有的静默空操作——不可读的值已经在 `chat` 里被 400 掉了。
  - `send := chunkSender(w, flusher, req.Model, "chatcmpl-mock-stream")`。
  - 工具调用模式：`match == nil && len(req.Tools) > 0` 时走 `s.streamToolCall` 并返回。**源码注释观点**：参数是分册到达的，这正是会打碎朴素代理的情形——客户端必须按索引累积 `choices[0].delta.tool_calls[0].function.arguments`；只有非脚本路径用分册形状，因为脚本声明的是完整的工具调用，它们在一个 opening 帧里整块发出（见 `streamScripted`）：脚本化的调用是要被 agent runtime 逐字读回的，不是用来给代理的缓冲施压的。
  - 脚本命中且非 legacy → `s.streamScripted(w, r, req, *match, send)` 并返回。
  - 非脚本路径的帧序：role 帧（**内容为空**）→ 逐词 content 帧（每个词后加一个空格，每个词前检查 `r.Context().Done()` 以在客户端消失时停止生成，每帧后按 `s.tokenDelay` 定速）→ finish 帧（`send(map[string]any{}, "stop")`）→ `writeUsageAndDone`。**源码注释观点**：opening role 帧的内容是空的，正是 OpenAI 的发射方式；网关**不能**把它当作首 token——它在模型产出任何东西之前就到了，把它计入会把排队时间报成生成延迟。
  - **源码注释观点（usage 帧的存在理由）**：usage 在真实 API 上只出现在最后一帧；忽略流尾部的记账会报出零成本，而这正是这一帧要抓的 bug。
  - 定速的可配置性：**源码注释观点**：固定 15ms 会把流式吞吐限制在 mock 自己的速度上而不是网关的，从而让负载测试测错进程。

- `(*server).streamScripted (cmd/mockupstream/main.go)` —— `func (s *server) streamScripted(w http.ResponseWriter, r *http.Request, req chatRequest, match scriptMatch, send func(map[string]any, any) bool)`。作用：写脚本化回答的 SSE 形式。帧序：opening 帧（`{"role":"assistant","content":""}`，有工具调用时把**完整的** `tool_calls` 放进同一个 delta 并上报个数）→ 逐词 content 帧（同样的 `Context().Done()` 检查与定速）→ finish 帧（`finishReason` 是 `"tool_calls"` 或 `"stop"`）→ `writeUsageAndDone(..., match.usageFor(req, content, len(calls)))`。**源码注释观点**：帧形状与非脚本路径刻意完全一致，因为网关的流处理不该为脚本化的上游多一条代码路径；脚本化的工具调用整块发出（`id`/`type`/`function.name`/`function.arguments` 一起来），而不是像非脚本路径那样分册。

- `writeUsageAndDone (cmd/mockupstream/main.go)` —— `func writeUsageAndDone(w http.ResponseWriter, r *http.Request, model, id string, usage map[string]any)`。作用：写每个 SSE 回答的尾部——先 usage 帧（`json.Marshal(chunkUsageFrame(...))` 后写 `data: %s\n\n` 并 flush），然后在 `X-Mock-Omit-Done` 为空时写 `data: [DONE]\n\n` 并 flush。**源码注释观点**：两个流路径共用它，所以它们不会各自漂移——网关能处理一个就必须能同样处理另一个，这正是脚本模式复用今天帧形状的全部原因；而被要求省略 `[DONE]` 的 mock 复现了一个真实集成隐患：有些 OpenAI 兼容服务器不带哨兵就关流，朴素的客户端会一直挂到自己的超时——网关必须自己合成它。

- `chunkSender (cmd/mockupstream/main.go)` —— `func chunkSender(w http.ResponseWriter, flusher http.Flusher, model, id string) func(map[string]any, any) bool`。作用：返回"写一个 `chat.completion.chunk` 帧"的闭包，字段为 `id`/`object`/`created`/`model`/`choices[{index:0, delta, finish_reason}]`。失败返回 false（`json.Marshal` 出错或写返回错误），调用方据此停止生成。

- `chunkUsageFrame (cmd/mockupstream/main.go)` —— `func chunkUsageFrame(model, id string, usage map[string]any) map[string]any`。作用：尾部 usage 帧，`choices` 是空列表——真实 API 就是把它放在那里。

- `(*server).streamToolCall (cmd/mockupstream/main.go)` —— `func (s *server) streamToolCall(w http.ResponseWriter, r *http.Request, req chatRequest, send func(map[string]any, any) bool)`。作用：发射一个**分册**的工具调用。id 是 `call_mock_1`，名字是 `get_weather`，参数字符串被故意从 JSON 中间切开：`{"city":"Bei`、`jing","units":"met`、`ric"}`。三帧的 delta 依次是：带 `index`/`id`/`type`/`function.name` 与第一段参数 → 带 `index` 与第二段参数 → 带 `index` 与第三段参数；帧间 `time.Sleep(10 * time.Millisecond)`；然后 finish 帧（`finish_reason: "tool_calls"`），最后（`X-Mock-Omit-Done` 为空时）`data: [DONE]`。**源码注释观点**：一个逐 delta 独立解析的客户端会看到非法 JSON，必须按索引缓冲。

## 51. 脚本规则：匹配与应答（script.go）

包注释（**源码注释观点**）：脚本模式存在的理由是 M6 把网关接进一个使用原生函数调用的 Python agent runtime（Warden）；在付费 provider 上证明"agent 循环走到了答案"既慢又不确定，在 CI 里还不可用，而**从网关这一侧根本无法证明幂等重放造成了零次额外上游调用**。一个脚本文件把 mock 变成一个确定性的工具调用模型（"第一回合：调 `get_weather`；任何带工具结果的回合：把它回显出来"），而 `/calls` 给了 gate 一个精确的、外部的证人，证明上游到底被问了多少次。`-script` 为空时整个特性休眠，所以每一个 curl 这个二进制的 M0–M5 gate 看到的都是逐字节不变的旧行为。

类型：

- `script (cmd/mockupstream/script.go)` —— `type script struct { Default *scriptRespond \`json:"default"\`; Rules []scriptRule \`json:"rules"\`; path string }`。注释要点：`Default` 回答没被任何规则匹配的 chat 请求；它缺席时保留内置回答（`mock answer to ...`），所以"只加规则"的脚本不会静默改变未匹配的流量；`Rules` 从上到下求值，**第一个命中者胜**；`path` 是这个脚本的来源文件，只为启动日志与错误服务。
- `scriptRule (cmd/mockupstream/script.go)` —— `type scriptRule struct { Name string; When scriptWhen; Respond scriptRespond; hits atomic.Int64 }`。注释要点：`Name` 在 `/calls` 与日志里标识规则，未命名的规则仍可用，但会以 `""` 上报；`hits` 统计命中过这条规则的请求数，它驱动 `fail_first`，并且是原子的，因为一个运行中的服务器的每个 chat 请求都可能碰到它。
- `scriptWhen (cmd/mockupstream/script.go)` —— `type scriptWhen struct { HasToolResult bool; ToolsPresent bool; Contains string; Turn int; Stream *bool; Path string }`。它是一个**合取**：设置了每个字段都必须匹配，因此零值（或整个省略 `"when"`）匹配任意 chat 请求。逐字段语义：`HasToolResult` 要求请求包含（true）或不包含（false）一条 `role:"tool"` 的消息——它是**永远被应用**的二值谓词，所以与 `Contains`/`Turn`/`Path` 不同，它没有"不关心"的取值：`{"has_tool_result": false}` 真的意味着"还没有工具结果"；`ToolsPresent` 要求 body 里有一个非空的 `tools` 数组；`Contains` 是对**最后一条 user 消息**做大小写不敏感的子串测试，空表示不关心；`Turn` 匹配这个进程**已经**处理过的 chat 请求数（1 起算，在计数器自增之前求值，所以 1 是第一个 chat 请求），0 表示任意回合；`Stream` 用指针是为了让 `"stream": false` 与省略该字段可区分；`Path` 与 `r.URL.Path` 精确相等（例如 `/v1/chat/completions`）。
- `scriptRespond (cmd/mockupstream/script.go)` —— `type scriptRespond struct { Content string; EchoToolResult bool; ToolCalls []scriptToolCall; Usage *scriptUsage; Status int; ErrorBody json.RawMessage; DelayMs int; Drop bool; FailFirst int }`。每个字段都有定义好的默认值，所以最小的 `"respond": {}` 仍然是一个普通 mock 回答。逐字段语义：`Content` 空表示"没有文本"，它**刻意不等于**"用 legacy 默认值"，因为一个工具调用回答通常带空内容；`EchoToolResult` 为真时用请求里按顺序拼接的所有 `role:"tool"` 消息内容替换 `Content`，这是让确定性的两回合 agent 循环成为可能的东西；`ToolCalls` 发射带 OpenAI 形状 `tool_calls` 的 assistant 消息；`Usage` 覆盖 token 数字，nil 则保留这个二进制今天对等价响应形状算出的数字，`TotalTokens` 在脚本省略时被填上；`Status` 是回答用的 HTTP 状态，0 与 200 都表示成功；`ErrorBody` 设置时被**逐字**用作非 2xx 回答的 JSON body，否则发 OpenAI 形状信封；`DelayMs` 在响应写出来之前睡，而发生在更早的 `-ttfb` 停滞仍然生效；`Drop` 不写任何东西就关闭连接，让网关看到一条坏掉的跳，而不是一个 HTTP 状态——`Drop` 优先于 `Status`；`FailFirst` 表示"匹配这条规则的前 N 个请求以 `Status`（默认 502）失败，从第 N+1 个请求起这条规则才正常行为"。
- `scriptToolCall (cmd/mockupstream/script.go)` —— `type scriptToolCall struct { ID string; Name string; Arguments json.RawMessage }`。注释要点：省略的 `id` 会按列表顺序变成 `call_1`、`call_2`……；显式的 id 必须在重试之间保持稳定——一次脚本化失败之后的重放必须产生同一个 id，否则 agent runtime 无法把工具结果与请求关联起来；`Arguments` 是函数参数对象，被 marshal 成 OpenAI 线上形式（一个 JSON **字符串**）。
- `scriptUsage (cmd/mockupstream/script.go)` —— `type scriptUsage struct { PromptTokens int; CompletionTokens int; TotalTokens int; CachedTokens int }`。注释要点：它镜像 OpenAI 的 usage 块；`CachedTokens` 存在是因为网关的记账区分缓存命中的 prompt token 与全新的。

加载与校验：

- `loadScript (cmd/mockupstream/script.go)` —— `func loadScript(path string) (*script, error)`。作用：读并解析脚本文件。错误字符串原文：`fmt.Errorf("read script %s: %w", path, err)` 与 `fmt.Errorf("script %s: %w", path, err)`。成功时把 `sc.path = path`。**源码注释观点**：每一种失败都点名问题（文件不存在、不可读、JSON 坏、未知字段），这样操作员可以修文件，而不是去猜为什么 mock 答错了。
- `parseScript (cmd/mockupstream/script.go)` —— `func parseScript(raw []byte) (*script, error)`。作用：解码脚本体并校验。**源码注释观点（未知字段被拒是故意的）**：`tools_present` 里的一个拼写错误会静默地把一条工具调用规则变成匹配一切的规则，而这个失败只会表现为"某次 agent 运行行为古怪"。校验项与错误字符串原文：
  - `dec.DisallowUnknownFields()`；
  - `fmt.Errorf("parse: %w", err)`（解码失败）；
  - `dec.More()` 为真 → `parse: trailing content after the script object`（**源码注释观点**：一个把两个 JSON 对象留在文件里的截断编辑是真实的操作员错误，解码会把它藏起来）；
  - `sc.Default == nil && len(sc.Rules) == 0` → `parse: script has neither rules nor a default response`；
  - `FailFirst < 0` → `fmt.Errorf("parse: rules[%d] (%s): fail_first must not be negative", i, name)`；
  - `When.Turn < 0` → `fmt.Errorf("parse: rules[%d] (%s): when.turn must not be negative", i, name)`；
  - `Status < 0 || Status > 599` → `fmt.Errorf("parse: rules[%d] (%s): status %d is not a valid HTTP status", i, name, status)`。

选择与匹配：

- `scriptMatch (cmd/mockupstream/script.go)` —— `type scriptMatch struct { Rule *scriptRule; Respond *scriptRespond; RuleName string; Turn int; Legacy bool }`。逐字段语义：`Rule` 是第一个命中的规则，采用默认响应时为 nil；`Respond` 是要构造的响应（命中的规则的，或脚本 default 的）；`RuleName` 在 `/calls` 里标记这次请求（没有规则命中时是 `"default"`）；`Turn` 是这次匹配所依据的 1 起算的 chat 请求计数；`Legacy` 表示"用今天的内置回答"——没有规则命中且脚本没有声明 default。
- `(*script).selectResponse (cmd/mockupstream/script.go)` —— `func (sc *script) selectResponse(req chatRequest, stream bool, path string, turn int) scriptMatch`。作用：为一次 chat 请求挑响应。流程：自上而下遍历 `sc.Rules`，`matches` 为真则取 `rule := &sc.Rules[i]`、`rule.hits.Add(1)`，返回 `scriptMatch{Rule: rule, Respond: &rule.Respond, RuleName: rule.Name, Turn: turn}`；一个都没命中时若 `sc.Default != nil` 返回 `{Respond: sc.Default, RuleName: "default", Turn: turn}`；否则返回 `{RuleName: "default", Turn: turn, Legacy: true}`。**源码注释观点**：它只在请求 body 已经校验之后才被调用，所以回合计数计的是"mock 真的回答过的请求"，而不是畸形请求。
- `(*scriptRule).matches (cmd/mockupstream/script.go)` —— `func (r *scriptRule) matches(req chatRequest, stream bool, path string, turn int) bool`。作用：合取求值，顺序为 `HasToolResult != hasToolResult(req.Messages)` → `ToolsPresent != (len(req.Tools) > 0)` → `Contains`（两边都 `strings.ToLower` 后做子串判断）→ `Turn != 0 && Turn != turn` → `Stream != nil && *Stream != stream` → `Path != "" && Path != path`；全部通过返回 true。
- `hasToolResult (cmd/mockupstream/script.go)` —— `func hasToolResult(msgs []chatMessage) bool`，是否存在 `Role == "tool"` 的消息。
- `lastUserContent (cmd/mockupstream/script.go)` —— `func lastUserContent(msgs []chatMessage) string`，返回最后一条 user 消息的内容（legacy 回答与 `contains` 谓词推理的是同一条消息）。
- `toolResultText (cmd/mockupstream/script.go)` —— `func toolResultText(msgs []chatMessage) string`，按请求顺序拼接所有 `role:"tool"` 消息的内容，无前缀无分隔符。**源码注释观点**：agent runtime 会拿这个回答与它自己工具返回的东西比较，所以在这里发明装饰会让断言依赖 mock 的排版。

应答构造：

- `effectiveStatus (cmd/mockupstream/script.go)` —— `func effectiveStatus(r *scriptRespond, hits int64) (status int, failing bool)`。作用：解析应该用哪个状态作答，同时应用 `fail_first` 与"0 即 200"。规则：`status = r.Status`，为 0 则 200；若 `r.FailFirst > 0 && hits <= int64(r.FailFirst)`，在脚本省略 `status` 时把状态改成 502 并返回 `(status, true)`（**源码注释观点**：`fail_first` 必须在规则省略 `status` 时也失败，502 是"注入的上游故障"的文档化默认值）；否则返回 `(status, status < 200 || status > 299)`。
- `(*server).answerScripted (cmd/mockupstream/script.go)` —— `func (s *server) answerScripted(w http.ResponseWriter, r *http.Request, req chatRequest, m scriptMatch) bool`。作用：在成功形状被构造**之前**应用脚本化响应的失败与 drop 那一侧（**源码注释观点**：一条失败或 drop 的规则绝不能写出成功信封——解析了 200 的 agent runtime 会把注入的失败当成真实回答，于是 gate 什么也证明不了）。返回值语义：它已经写过（或有意放弃）响应时返回 true；成功路径返回 false，因为"施加一次延迟"不等于"写出了一个答案"，调用方仍需构造成功形状。流程：`m.Respond == nil`（legacy）返回 false；算出 `rule` 与 `hits`（`m.Rule != nil` 时取 `m.Rule.hits.Load()`）；调 `effectiveStatus`；若 `m.Respond.Drop`，打 `script dropping connection` 日志、通过 `interface{ wantsDrop() }` 通知计数 writer、然后 `http.Hijacker` 可用就 `Hijack` 并 `conn.Close()`，**不可用则 `panic(http.ErrAbortHandler)`**（**源码注释观点**：没有 Hijacker 就无法从这里关闭这条跳，而 `http.ErrAbortHandler` 正是 `net/http` 文档为"我无法服务这个请求"提供的逃生舱——它对客户端中止连接，同时抑制 panic 日志），返回 true；若 `failing`，打 `script failing request` 日志、按 `DelayMs` 睡、`writeScriptedError(w, status, m.Respond.ErrorBody)`、返回 true；否则按 `DelayMs` 睡并返回 false。
- `writeScriptedError (cmd/mockupstream/script.go)` —— `func writeScriptedError(w http.ResponseWriter, status int, body json.RawMessage)`。作用：有自带 body 时**逐字**写它，否则写 OpenAI 错误信封（`{"error":{"message":"scripted failure","type":"mock_error"}}`）。细节：自带 body 时设 `Content-Type: application/json`、写状态、若末字节不是换行则补一个再写。**源码注释观点**：body 被逐字写出，所以 gate 可以断言 provider 风格失败携带的精确字节。
- `(scriptMatch).respond (cmd/mockupstream/script.go)` —— `func (m scriptMatch) respond(req chatRequest) (content string, calls []map[string]any, finishReason string)`。作用：构造这次匹配描述的 assistant 回合。`m.Respond == nil` 返回 `completionFor(req), nil, "stop"`；否则 `content = r.Content`，`EchoToolResult` 为真则 `content = toolResultText(req.Messages)`；`calls = wireToolCalls(r.ToolCalls)`；有调用时返回 `"tool_calls"`，否则 `"stop"`。
- `wireToolCalls (cmd/mockupstream/script.go)` —— `func wireToolCalls(specs []scriptToolCall) []map[string]any`。作用：把脚本的工具调用转成 OpenAI 线上形状。规则：空输入返回 nil；`id` 为空时 `fmt.Sprintf("call_%d", i+1)`；`args` 默认 `"{}"`，有 `Arguments` 时用它的字符串形式；每项是 `{"id":…, "type":"function", "function":{"name":…, "arguments": args}}`。**源码注释观点**：参数对象被重新编码成一个 JSON **字符串**，这是每个 OpenAI 兼容客户端期望的，也是让流式与非流式形状完全一致的原因。
- `(scriptMatch).usageFor (cmd/mockupstream/script.go)` —— `func (m scriptMatch) usageFor(req chatRequest, content string, toolCalls int) map[string]any`。作用：返回响应要带的 usage 块。默认值是"这个二进制对这种形状一贯算出来的数字"（`prompt = tokenCount(req)`，`completion = len(strings.Fields(content))`，有 tool calls 时 `completion += toolCalls`），所以加 `-script` 不会改变 M3 gate 对账的记账；当 `m.Respond.Usage != nil` 时改用脚本的数字，`TotalTokens` 为 0 时填成 `prompt + completion`，并在这一支里带上 `prompt_tokens_details.cached_tokens`。

## 52. /calls 见证端点

- `callsSnapshot (cmd/mockupstream/script.go)` —— `type callsSnapshot struct { Calls int64 \`json:"calls"\`; ByPath map[string]int64 \`json:"by_path"\`; ByRule map[string]int64 \`json:"by_rule"\`; ToolCallsEmitted int64 \`json:"tool_calls_emitted"\`; Dropped int64 \`json:"dropped"\`; Failed int64 \`json:"failed"\` }`。**源码注释观点**：它存在是为了让 gate 证明一次幂等重放造成了零次额外上游调用，也让人在调试 agent 运行时能看到是哪条规则回答了哪一次请求。
- `(*server).callsHandler (cmd/mockupstream/script.go)` —— `func (s *server) callsHandler(w http.ResponseWriter, r *http.Request)`，写 `writeJSON(w, 200, s.calls())`。**源码注释观点**：它本身刻意不计数——gate 会轮询它，而它绝不能扰动自己在测量的那个数。
- `(*server).statsCallsHandler (cmd/mockupstream/script.go)` —— `func (s *server) statsCallsHandler(w http.ResponseWriter, r *http.Request)`，直接转调 `callsHandler`；存在只是为了把这个快照放在与未来 `/stats/*` 端点成组的路由下。
- `(*server).calls (cmd/mockupstream/script.go)` —— `func (s *server) calls() callsSnapshot`。作用：快照计数器。**源码注释观点**：读它们每个计数器只是一次普通 load，不持任何锁，所以轮询 `/calls` 的 gate 不可能卡住一个在飞的请求。
- `countSink (cmd/mockupstream/script.go)` —— `type countSink struct { mu sync.Mutex; m map[string]int64 }`。作用：一个并发安全的计数 map；键是已知的路径与规则名，所以它就是"map 加互斥锁"而不是更花哨的东西。
- `(*countSink).add (cmd/mockupstream/script.go)` —— `func (c *countSink) add(key string)`，`mu` 下懒初始化 map 并自增。
- `(*countSink).snapshot (cmd/mockupstream/script.go)` —— `func (c *countSink) snapshot() map[string]int64`，`mu` 下复制一份。
- `(*server).record (cmd/mockupstream/script.go)` —— `func (s *server) record(path, rule string, toolCalls int, dropped, failed bool)`。作用：为一次被处理过的 chat 请求记账，在响应体写完之后被调用，所以 `calls` 的含义是"mock 真的回答过的上游请求"——恰恰是幂等重放不允许增加的那个数。规则：`rule == ""` 时改成 `"default"`；`callCount.Add(1)`；`byPath.add(path)`；`byRule.add(rule)`；`toolCalls > 0` 时 `toolCallsEmitted.Add(...)`；`dropped`/`failed` 各自自增。**源码注释观点（为什么空规则名必须改名）**：未命名的规则意味着"没有脚本回答这一次"（mock 没带 `-script`，或没有规则命中）；用空串计数会在 `/calls` 的 JSON 里放一个 `""` 键——合法，但 PowerShell 5.1 的 `ConvertFrom-Json` 会在空属性名上抛错（"the value of argument \"name\" is not valid"），于是每一个用 PowerShell 写的 `/calls` 读者都会丢掉整个快照、并静默跳过它的 provider 调用断言。改成给内置回答起个名字。
- `(*server).nextTurn (cmd/mockupstream/script.go)` —— `func (s *server) nextTurn() int { return int(s.turn.Add(1)) }`。**源码注释观点**：匹配是对**自增之前**的值求值的，所以 `when.turn == 1` 意味着"这个进程看到的第一个 chat 请求"，而不是第二个。

## 53. mockupstream 的测试：意图反推

`cmd/mockupstream/headers_test.go` 的包注释（**源码注释观点**）：这些头是 gate 在不重新编译 mock 的前提下注入状态或停滞的方式，所以**它们的失败模式比成功模式更重要**——一个被 mock 忽略的头，会让一个"从未发生过"的场景上方立着一个绿色的勾。测试钉的是两件事：不可读值的答案（点名该头的 400），以及当请求带多个头时决定胜负的两个顺序。

- `TestUnreadableTimingHeadersAreRejected`：`X-Mock-Delay` 取 `"300"`/`"soon"`/`"-1s"`、`X-Mock-TTFB` 取 `"300"`/`"-1ms"` 全部必须 400、body 里点名该头，**并且 `/calls` 的 `Calls` 必须是 0**——注释原文的意思是：一个被拒绝的请求不能看起来像一个回答。
- `TestReadableTimingHeadersAreHonoured`：`X-Mock-Delay: 40ms` 必须让非流式回答也晚到；`X-Mock-TTFB: 40ms` 必须让流式回答晚到且 body 以 `data: ` 开头（注释说明 `ResponseRecorder` 会缓冲，所以这里观察的是"停滞发生了"，而不是 flush 边界在哪）。
- `TestInjectedStatusIsAnsweredBeforeTheDelay`：同时带 `X-Mock-Status: 429` 与 `X-Mock-Delay: 400ms` 时必须立刻返回 429、不产生 content 帧。
- `TestAMalformedTimingHeaderBeatsAnInjectedStatus`：`X-Mock-Status: 429` + `X-Mock-Delay: "300"` 必须返回 400 且点名 `X-Mock-Delay`——畸形请求按名字回答，即使它同时要求了一个状态码。

`cmd/mockupstream/script_test.go` 的用例名构成脚本模式的契约表：`TestDefaultContentWithoutScript`、`TestUnscriptedCallsAreCountedUnderDefaultName`、`TestScriptedDefaultContentWithoutRuleApplies`、`TestLegacyDefaultWhenScriptHasNoDefault`、`TestToolsPresentEmitsToolCalls`、`TestToolCallIDIsStableAcrossRetries`、`TestEchoToolResult`、`TestTurnMatchesOnlyFirstChatRequest`、`TestContainsIsCaseInsensitive`、`TestStatusWithErrorBody`、`TestStatusWithoutErrorBodyUsesTheEnvelope`、`TestFailFirstFailsOnceThenSucceeds`、`TestDelayMs`、`TestDropClosesWithoutAResponse`、`TestCallsCountsChatsOnlyAndGroupsByPathAndRule`、`TestPathPredicate`、`TestStreamPredicate`、`TestStreamedScriptedContent`、`TestStreamedScriptedToolCalls`、`TestStreamedEchoToolResult`、`TestOmitDoneStillHonoured`、`TestFirstMatchingRuleWins`、`TestNoScriptKeepsStreamShape`、`TestParseScriptRejects`、`TestLoadScriptUnreadableErrors`、`TestToolCallArgumentsDefaultToEmptyObject`、`TestUsageDefaultsWhenScriptOmitsIt`、`TestScriptedUsageWithoutTotal`。它对应的意图很集中：脚本缺席时字节不变、计数归因、谓词的每一个方向、失败与 drop 的语义、以及 usage 的默认值。

---

## 54. cmd/mockcollector ：一次性 OTLP/HTTP 接收器

包注释（**源码注释观点**）：Go 验收 gate（`cmd/verify-m5`）已经在同进程内用一个 `httptest` collector 证明了导出器，而那是断言 payload 形状的正确位置；这个二进制存在的目的，是让 curl 级别的 gate 能跨**真实的进程边界**证明同一件事——由 `scripts/verify-m5.ps1` 启动的网关通过真实 socket POST 到这个监听器，脚本随后问这个 collector 它收到了什么。它接受**任意路径**（这样"路径写错"这个缺陷是可观察的，而不是脚本还得去猜的 404），记录每个请求路径、content type、字节数和 body 的 SHA-256，并回答：`GET /healthz` → 200 ok；`GET /requests` → `{"count":N,"requests":[{path,content_type,bytes,sha256}]}`；`GET /dump` → 记录下来的 body，用一行破折号分隔。body 保存在内存里（一次 trace 导出是 KB 级，gate 只跑几秒），而 `/dump` 就是断言失败时会打印出来、展示"实际到达了什么"的东西。

- `recorded (cmd/mockcollector/main.go)` —— `type recorded struct { Path string \`json:"path"\`; ContentType string \`json:"content_type"\`; Bytes int \`json:"bytes"\`; SHA256 string \`json:"sha256"\`; Header map[string][]string \`json:"header"\`; body string }`。作用：一次请求的记录；`body` 不导出（不在 JSON 里），只在 `/dump` 里出现。
- `collector (cmd/mockcollector/main.go)` —— `type collector struct { mu sync.Mutex; records []recorded }`。
- `(*collector).add (cmd/mockcollector/main.go)` —— `func (c *collector) add(path string, header http.Header, contentType, body string) recorded`。作用：算 SHA-256（`sha256.Sum256` + `hex.EncodeToString`）、克隆 header、在 `mu` 下 append 并返回记录。
- `(*collector).snapshot (cmd/mockcollector/main.go)` —— `func (c *collector) snapshot() []recorded`，`mu` 下复制一份。
- `main (cmd/mockcollector/main.go)` 的 flags：`-listen`（默认 `127.0.0.1:19999`）、`-name`（默认 `mockcollector`，用于日志前缀）、`-max-body-bytes`（默认 `8<<20`）、`-quiet`（默认 false，只记错误）。日志用 `log.New(os.Stderr, "collector "+name+" ", log.LstdFlags|log.Lmicroseconds)`。
- 兜底 handler（`/`）的行为：`io.ReadAll(io.LimitReader(r.Body, maxBody+1))`，读失败 → 400 `{"error":"read body"}`；超过上限 → 413 `{"error":"body too large"}`；否则记录并按 `-quiet` 决定是否打 `POST %s %d bytes sha256=%s`（SHA 只打前 12 个字符）；**响应固定为 200 `{"partialSuccess":{}}`**——**源码注释观点**：这是 OTLP/HTTP 的响应体；"200 加空 body"也合法，但 `{"partialSuccess":{}}` 是规范示例里展示的、也是真实 collector 返回的。
- HTTP 服务器：`ReadHeaderTimeout: 10 * time.Second`；用 `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)` 捕获信号，收到后打 `shutting down` 并 `srv.Shutdown` 一个 5 秒超时的 context；`ListenAndServe` 返回非 `http.ErrServerClosed` 时打印 `collector: %v` 并 `os.Exit(1)`。

---

## 55. 交叉引用：部署侧的消费方

- `deploy/prometheus/prometheus.yml` 与 `deploy/prometheus/docker.yml` —— scrape 配置的两份变体（本机与 docker 网络）。
- `deploy/grafana/dashboards/infergate.json` —— 面板定义；`deploy/grafana/provisioning/dashboards/infergate.yml` 与 `deploy/grafana/provisioning/datasources/prometheus.yml`（另有 docker 变体 `deploy/grafana/datasources-docker/prometheus.yml`）—— 自动装载用的 provisioning。
- `deploy/README.md` —— 部署与面板的说明入口。

这一层不改变上一节的任何契约，但它是"HELP 文案与标签必须稳定"这条约束的实际理由：一旦 dashboard 建在某个 `le` 值或某个标签名上，改类型或改名字的代价就不再是"改一行代码"，而是"改线上面板"。

---

## 56. 读完之后应该记住的十条

1. 时间是 `int64` 纳秒，浮点毫秒只是给人看的投影；`nanosToMS` 做 6 位小数的四舍五入。
2. `traceparent` 的解析是严格白名单：只接受版本 `00`、只接受小写十六进制、拒绝全零与全 `ff`；拒绝时返回零值而不是部分填充。
3. `Context.SpanID` 是**父**的 span id；根 span 把它记成 `ParentSpanID`，自己另铸新的。
4. `NewRootContext` 默认 `Sampled = true`，因为"默认不采样"在实践中等于"没有 trace"。
5. `Store` 是环 + FIFO：环形让槽位永不移动、索引每 `Add` 只做 O(1) 补丁；FIFO 而不是 LRU，因为访问新近性在这里几乎没有信息。
6. `Store.Dropped` 是生命周期计数器，`Reset` 不清零；它可以区分"从未记录"与"被挤掉"。
7. 两个导出器的共同规则是 `Export` 永不阻塞：各有 worker + 有界队列，满了就丢并计数；OTLP 无批量、无重试，JSONL 在"队列瞬时为空"时 flush。
8. 指标聚合有明确维度：请求五维、尝试三维、token/首 token 两维，而请求时长直方图刻意只有三维（不含 status/outcome）；所有快照都按固定键序排序。
9. 假上游的注入头**不可读即 400**，绝不静默当成"无延迟"；`X-Mock-TTFB` 的停滞发生在 SSE 响应头提交之前，否则首 token 时间不可测量，而 `X-Mock-Delay`/注入 status 的先后顺序在两个 mock 里是刻意相反的。
10. mockupstream 的 SSE 帧序固定为 role → 逐词 content → finish → usage → `[DONE]`；工具调用在非脚本路径上分三帧分册，在脚本路径上整块发出；`/calls` 是"上游真的被问了几次"的外部证人。


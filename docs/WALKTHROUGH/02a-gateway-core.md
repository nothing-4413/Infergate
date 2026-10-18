# 第 02A 册 · 网关核心管线

## 1. 本册范围与读法

- 本册是「网关核心管线」三部分中的第一部分，覆盖 [internal/gateway/proxy.go](../../internal/gateway/proxy.go) 的**前半**（包声明与 import 块、`Proxy`、`record`、`Usage`、`ServeHTTP` 及其唯一的 deferred finalizer）与 [internal/gateway/errors.go](../../internal/gateway/errors.go) 全文。
- 先给一个容易误判的事实：`internal/gateway/proxy.go` 顶部**没有**任何 `const` / `var` 声明。包声明之后第一个声明就是 `type Proxy struct`；全文件第一个函数是 `New`，第一个方法才是 `ServeHTTP`。本册范围内唯一存在的常量块位于 `internal/gateway/errors.go`（`Type*` 系列）。
- 因此"文件顶部常量"在本册只落地为：`errors.go` 的 import 块（`encoding/json`、`net/http`）与其后的 `Type*` 常量组，见第 7 节。
- 锚点统一写作 `符号名 (internal/gateway/proxy.go)`，**不含行号**；后半文件的 `serve`、`serveAttempts`、`attemptUpstream`、`buildRequest`、`copyWhole`、`readBody`、`plan`、`backoff`、`readBody` 等一律不在本册，见第 8 节。
- 并发总纲（贯穿全册）：`Proxy` 构造后不可变，可被所有 handler goroutine 共享而无需锁；可变状态只存在于每请求的 `record` 与该请求的局部变量中（源码注释的观点）。

## 2. 文件头：包声明与 import 块 (internal/gateway/proxy.go)

- `package gateway (internal/gateway/proxy.go)` — 包声明本身，其上方**没有**包级文档注释；"这个包是什么"的说明落在 `Proxy` 的类型注释里。本册不出现包级注释，故不虚构。
- 标准库 import 块 (internal/gateway/proxy.go) — `bytes`、`context`、`encoding/json`、`errors`、`io`、`log/slog`、`math/rand`、`mime`、`net/http`、`runtime/debug`、`strconv`、`strings`、`time`。本册范围内直接可见其用途的只有 `log/slog`（日志）、`net/http`（`ResponseWriter` / `Request` / 状态码）、`runtime/debug`（`debug.Stack()` 取 panic 栈）、`strings`（`strings.Join` 拼 `tried`）、`time`（`time.Now` / `time.Since`）；其余由后半的 body 读取、SSE 拷贝、退避抖动等使用。
- 内部包 import 块 (internal/gateway/proxy.go) — 模块路径均为 `github.com/infergate/infergate/internal/...`：`breaker`、`cache`、`idempotency`、`metrics`、`quota`、`router`、`sessions`、`tracing`、`upstream`。它们分别对应 `Proxy` 的断路器、语义缓存、幂等存储、指标 sink、准入控制、路由、会话账本、链路追踪、上游注册表字段。
- 注意 `context` 被 import 但在本册只以 `r.Context()` 的形式间接出现（`p.settleQuota(r.Context(), rec)` 与 `p.log.Log(r.Context(), ...)`）；`settleQuota` 的形参类型是 `context.Context`，声明在 `internal/gateway/quotapath.go`。

## 3. `Proxy` 结构体：字段逐条

- `type Proxy struct (internal/gateway/proxy.go)` — OpenAI 兼容反向代理，是 InferGate 的 M0 表面。**源码注释的观点**：它"刻意做到每请求无状态：每个字段在构造后只读，因此该值可被所有 handler goroutine 共享而无需加锁；可变的每请求状态放在 `record` 和局部变量里"。签名照抄：`type Proxy struct {`
- `Proxy.upstreams (internal/gateway/proxy.go)` — `*upstream.Registry`；把模型（外加可选 pin）解析成后端。只读，无锁。
- `Proxy.pricing (internal/gateway/proxy.go)` — `*PriceBook`；把 token 换算成美元。为 nil 表示"不计算成本"（源码注释观点，见 `Options.Pricing`）。只在日志收尾处按 `rec.usage` 调用 `p.pricing.CostUSD(rec.model, rec.usage.Prompt, rec.usage.Completion)`。
- `Proxy.metrics (internal/gateway/proxy.go)` — `metrics.Sink`；观测出口。`New` 会把 nil 归一为 `metrics.Nop{}`，所以请求路径上它永不为 nil。
- `Proxy.log (internal/gateway/proxy.go)` — `*slog.Logger`；结构化请求记录出口。`New` 会把 nil 归一为 `slog.Default()`。
- `Proxy.client (internal/gateway/proxy.go)` — `*http.Client`；`New` 里构造为 `&http.Client{}`，**源码注释的观点**："no Timeout: streaming bodies must not be cut off by the client"——超时不由 client 承担，否则流式体会被切断。
- `Proxy.maxBody (internal/gateway/proxy.go)` — `int64`；入站请求体上限。`New` 中 `maxBody <= 0` 时取 `8 << 20`。本册只读它被 `serve`→`readBody` 使用。
- `Proxy.upstreamTTL (internal/gateway/proxy.go)` — `time.Duration`；单次上游交换（含流式）的时限。零值表示关闭网关级上限，**源码注释的观点**：只留"客户端断开"作为唯一终止条件。
- `Proxy.router (internal/gateway/proxy.go)` — `*router.Router`；把模型 + 能力集转成有序 failover 链。**源码注释的观点**：nil 表示 M0 行为——只解析一个后端、从不重试。
- `Proxy.breakers (internal/gateway/proxy.go)` — `*breaker.Group`；持有各上游的时间窗与熔断状态。**源码注释的观点**：nil 表示所有后端永远健康。
- `Proxy.maxAttempts (internal/gateway/proxy.go)` — `int`；failover 链的上界。**源码注释的观点**：它存在的目的是掐断放大回路——三个坏后端各重试三次会把一个客户端请求变成九个 provider 请求，而 provider 的限流器为此买单；取 1 即 M0 行为。`New` 中 `< 1` 一律归一为 1。
- `Proxy.retryBackoff (internal/gateway/proxy.go)` — `time.Duration`；下一次尝试前的基准延迟。**源码注释的观点**：它线性增长并带抖动，因为固定延迟会让所有客户端重试步调一致、重新制造重试本要跨过的那次踩踏。
- `Proxy.transportFor (internal/gateway/proxy.go)` — `func(*upstream.Target) http.RoundTripper`；按 target 返回 RoundTripper。**源码注释的观点**：它存在是为了让测试能注入 transport（模拟死后端、慢后端、流中途断连）而不必真的起一个服务器。`New` 保证它非 nil：默认返回 `t.Transport`，注释理由是"nil 函数值在这条每请求路径上意味着每个请求都 panic 成 500"。
- `Proxy.cache (internal/gateway/proxy.go)` — `*cache.Cache`；M2 语义缓存。**源码注释的观点**：nil 表示每个请求都走上游（即 M0/M1 行为）；这里持有具体类型而非接口，是因为"哪些 prompt 可匹配、哪些参数必须一致"这套身份规则属于本包的正确性论证，而不是存储层的细节。
- `Proxy.quota (internal/gateway/proxy.go)` — `QuotaGate`（接口）；M3 准入控制器。**源码注释的观点**：nil 表示不强制预算（M0–M2 行为且是默认值）——"一个因为某个它从未有过的配置节现在为空就开始拒流的网关，比一个不设预算的网关更糟"。
- `Proxy.quotaCharsPerToken (internal/gateway/proxy.go)` — `int`；请求自身没给上限时，用它把入站 body 估成 prompt token。
- `Proxy.quotaCompletionTokens (internal/gateway/proxy.go)` — `int`；请求自身没声明 completion 上限时的预留量。**源码注释的观点**：这两个估算参数放在 Proxy 而不是 manager，是因为估算属于请求路径的关注点——只有网关手里有 body。
- `Proxy.tracer (internal/gateway/proxy.go)` — `*Tracer`；对采样中的请求记录一条 trace。**源码注释的观点**：nil 关闭追踪且这是默认值，因为 trace 存储会在内存里保留请求元数据，所以那是运营者的选择、不是每个部署都要付的成本。
- `Proxy.idempotency (internal/gateway/proxy.go)` — `*idempotency.Store`；按调用方的 `Idempotency-Key` 记住已完成的答案。**源码注释的观点**：nil 表示忽略该头（M0–M5 行为），这样"客户端超时后重试不会为第二次生成付费"。
- `Proxy.sessions (internal/gateway/proxy.go)` — `*sessions.Ledger`；M6 每会话账本。**源码注释的观点**：nil 表示不保留会话汇总；请求日志与 trace 不受影响。
- `Proxy.models (internal/gateway/proxy.go)` — `map[string]ModelInfo`；运营者给的每模型元数据。**源码注释的观点**：与其它字段一样构造后不可变，空 map 是常态——此时能力端点只报告上游自己声明的东西；它只被能力端点读，绝不在请求路径上读。并发：map 只读，无需锁。

## 4. `record` 结构体：字段逐条

- `type record struct (internal/gateway/proxy.go)` — 累积单个请求的全部事实，使"恰好一次指标观测 + 恰好一条日志记录"能从**单一位置**发出，无论 handler 有多少个 return（源码注释的观点）。它在 `ServeHTTP` 里按值指针创建，只被处理该请求的 goroutine 触碰，不加锁。
- `record.start (internal/gateway/proxy.go)` — `time.Time`；`ServeHTTP` 入口的 `time.Now()`，finalizer 用 `time.Since` 得 `elapsed`。
- `record.requestID (internal/gateway/proxy.go)` — `string`；由 `requestID(r)` 求得（定义在 `internal/gateway/proxy.go`，属后半范围），出现在 panic 日志、请求日志与 trace 属性中。
- `record.route (internal/gateway/proxy.go)` — `string`；由 `routeLabel(r.URL.Path)` 求得（定义在 `internal/gateway/request.go`），是指标维度。
- `record.path (internal/gateway/proxy.go)` — `string`；原始 `r.URL.Path`。
- `record.upstream (internal/gateway/proxy.go)` — `string`；最终服务的后端名，指标维度。
- `record.model (internal/gateway/proxy.go)` — `string`；最终用于该请求的模型名（可能已被配额降级改写）。
- `record.stream (internal/gateway/proxy.go)` — `bool`；由 `parsed.Stream && isStreamPath(r.URL.Path)` 决定（后半的 `serve` 设置），日志与 trace 都带它。
- `record.status (internal/gateway/proxy.go)` — `int`；`ServeHTTP` 初始化为 `http.StatusOK`。panic 分支只在它**仍等于 200** 时才改成 500，即已经写出别的状态时不被覆写。
- `record.outcome (internal/gateway/proxy.go)` — `metrics.Outcome`；初始 `metrics.OutcomeSuccess`（`internal/metrics/metrics.go` 中字面值 `"success"`、`"canceled"`、`"internal_error"`）。它同时是指标维度、日志 `level` 选择依据、以及 trace 里 OK/Error 的判据。
- `record.reason (internal/gateway/proxy.go)` — `string`；人类可读的失败原因（如 panic 分支写入的 `"panic"`）。非空才追加到日志。
- `record.reqBytes (internal/gateway/proxy.go)` — `int64`；入站 body 长度。
- `record.respBytes (internal/gateway/proxy.go)` — `int64`；写出字节数。
- `record.frames (internal/gateway/proxy.go)` — `int64`；SSE 帧数，`> 0` 才产生 `ObserveStreamFrames` 与 `stream.complete` 事件。
- `record.firstToken (internal/gateway/proxy.go)` — `time.Duration`；首 token 延迟。
- `record.firstTokenSet (internal/gateway/proxy.go)` — `bool`；**源码注释的观点**：它独立于测出的值记录"确实观测到了首 token"，因为粗粒度时钟下一个合法的首 token 可能正好测得 0，而用 `firstToken > 0` 做闸门会静默丢样本。
- `record.usage (internal/gateway/proxy.go)` — `Usage`；prompt/completion/cached token 三元组。日志里用 `!rec.usage.IsEmpty()` 做闸门。
- `record.explicitModel (internal/gateway/proxy.go)` — `string`；**源码注释的观点**：区分"客户端点名要这个模型"与"我们把后端的模型名替换上去了"，读日志时这个区别有意义。
- `record.attempts (internal/gateway/proxy.go)` — `int`；本请求做过的上游尝试数。**源码注释的观点**：大于 1 意味着发生过 failover，这是运营者问"某个后端被跳过得多频繁"时需要的数字。
- `record.tried (internal/gateway/proxy.go)` — `[]string`；按顺序记下每个被尝试过的后端。**源码注释的观点**：这样一条日志就讲完整个失败故事，而不是每次尝试一行。
- `record.failovers (internal/gateway/proxy.go)` — `int`；因为传输错误或上游 5xx 而放弃的尝试数。
- `record.cacheIdent (internal/gateway/proxy.go)` — `*cacheIdentity`；**源码注释的观点**：在 `serve` 里只算一次，好让查找、响应头与写入存储三者描述同一个问题；缓存关闭时为 nil。
- `record.cacheStatus (internal/gateway/proxy.go)` — `string`；写进 `X-InferGate-Cache` 与请求日志的值。**源码注释的观点**：即使什么都没缓存也会设置它，因为"这个请求被跳过了，原因是这个"正是运营者需要的事实。
- `record.cacheReason (internal/gateway/proxy.go)` — `string`；**源码注释的观点**：解释一次跳过或一次错误，给日志而不是给响应。
- `record.tenant (internal/gateway/proxy.go)` — `string`；预算与缓存隔离的命名空间。**源码注释的观点**：它来自客户端 header 或哈希后的凭据，**绝不来自 body**；记下来是为了让一行日志说清这是谁的请求。
- `record.session (internal/gateway/proxy.go)` — `string`；调用方声明的会话 id（有才记）。**源码注释的观点**：只有"按会话"的预算会读它。
- `record.quotaRes (internal/gateway/proxy.go)` — `*quota.Reservation`；本次准入开出的租约。**源码注释的观点**：它的存在与否，正是 deferred settle 用以判断"这个请求到底有没有被治理过"的依据。
- `record.quotaAction (internal/gateway/proxy.go)` — `string`；准入裁决。
- `record.quotaReason (internal/gateway/proxy.go)` — `string`；裁决理由；与 `quotaAction` 同行声明。
- `record.quotaLimit (internal/gateway/proxy.go)` — `int64`；决定裁决的维度上限。
- `record.quotaUsed (internal/gateway/proxy.go)` — `int64`；该维度已用量；与 `quotaLimit` 同行声明。
- `record.quotaRequested (internal/gateway/proxy.go)` — `int64`；本次请求量。三者只在 `quotaLimit > 0` 时才进日志。
- `record.quotaSettleError (internal/gateway/proxy.go)` — `string`；结算失败时的错误文本，非空才追加。
- `record.degradedModel (internal/gateway/proxy.go)` — `string`；**源码注释的观点**：被降级请求改写成的模型，让日志能说"这个回答来自便宜模型"，而不是留下一个无法解释的质量变化。
- `record.servedFromCache (internal/gateway/proxy.go)` — `bool`；**源码注释的观点**：表示答案是重放而非生成的；它的配额含义很精确——占了一个名额，但没花 provider token。
- `record.idemKey (internal/gateway/proxy.go)` — `string`；调用方的 `Idempotency-Key`，已归一化，无则该字段为空。
- `record.idemScope (internal/gateway/proxy.go)` — `string`；该 key 被认领时所在的命名空间。
- `record.idemHash (internal/gateway/proxy.go)` — `string`；请求体指纹。
- `record.idemClaimed (internal/gateway/proxy.go)` — `bool`；**源码注释的观点**：记录**本**请求持有该认领，因此它必须完成或中止它。
- `record.idemDecision (internal/gateway/proxy.go)` — `string`；给日志与 trace 的裁决，**源码注释列举的取值**：`"proceed"`、`"replay"`、`"stored"`、`"aborted"`、`"conflict_body"`、`"in_flight"`、`"skip"`。
- `record.idemReason (internal/gateway/proxy.go)` — `string`；解释一次冲突或一次被拒绝的写入。
- `record.idemStore (internal/gateway/proxy.go)` — `string`；写进 `X-InferGate-Idempotent-Store` 的值。
- `record.idemCapture (internal/gateway/proxy.go)` — `*captureWriter`；被捕获的响应（头与体），**源码注释的观点**：只在本请求持有认领时才保留，且只到存储的大小上限为止。
- `record.servedFromReplay (internal/gateway/proxy.go)` — `bool`；**源码注释的观点**：答案来自幂等存储；它的配额含义与 `servedFromCache` 相同——一个名额，零 provider token。
- `record.trace (internal/gateway/proxy.go)` — `*requestTrace`；本请求进行中的 trace。**源码注释的观点**：追踪被关闭或本请求被采样排除时为 nil；`requestTrace.addEvent` / `finish` 都容忍 nil 接收者（`internal/gateway/tracing.go`），所以 finalizer 可以无条件调用。
- `record.traceparent (internal/gateway/proxy.go)` — `string`；传给上游的 W3C `traceparent`。
- `record.tracestate (internal/gateway/proxy.go)` — `string`；同上，`tracestate`。**源码注释的观点**：这两个值即使请求不被记录也会算出来，好让一个正在追踪的调用方跨过本跳后仍然连着。

## 5. `Usage` 与 `IsEmpty`

- `type Usage struct (internal/gateway/proxy.go)` — 镜像 `sse.Usage` 而不在此处 import 它，**源码注释的观点**：让 `record` 类型不必依赖流式包。字段：`Prompt int`、`Completion int`、`Cached int`。
- `func (r *Usage) IsEmpty() bool (internal/gateway/proxy.go)` — 签名照抄；返回 `r.Prompt == 0 && r.Completion == 0 && r.Cached == 0`。无错误路径、无状态、无锁。被 deferred finalizer 的日志分支调用，用来决定是否输出 token 与成本属性。

## 6. `ServeHTTP`：入口与调用序

- `func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) (internal/gateway/proxy.go)` — 签名照抄，实现 `http.Handler`。被谁调用：生产路径上由 `internal/server/server.go` 用 `gateway.New(gateway.Options{...})` 构造出 `*Proxy` 后接入 handler 链（该链中的中间件以 `next.ServeHTTP(w, r)` 形式继续传递）；测试直接调用它（`internal/gateway/proxy_test.go`、`internal/gateway/failover_test.go`、`internal/server/m6_test.go`）。
- 第一步构造 `rec *record`，初始值只有五项：`start: time.Now()`、`route: routeLabel(r.URL.Path)`、`path: r.URL.Path`、`requestID: requestID(r)`、`status: http.StatusOK`、`outcome: metrics.OutcomeSuccess`。除此之外的字段全部留给下游填充，因此 finalizer 必须容忍零值。
- 第二步 `p.tracer.begin(r, rec)`，**源码注释的观点**：trace 先于一切打开，是为了让上游那一跳以本请求自己的 span 作为 parent；而入站 `traceparent` 在被信任之前先做校验。`Tracer.begin` 容忍 nil 接收者（未配置追踪时直接返回，注释观点：调用方自己的 traceparent 已由 header 拷贝原样转发，所以没追踪的部署既不破坏也不凭空发明 trace 链）。
- 第三步注册那**唯一一个** deferred finalizer（见第 6 节）。
- 第四步也是最后一步：`p.serve(w, r, rec)`。这是全函数唯一的主干调用；`serve` 定义在同一文件后半（属第二分册范围），其文档注释是"请求管线，`ServeHTTP` 里的延迟记录器包着它"。
- 控制流特征：`ServeHTTP` 内部对所有返回路径都没有提前 return，连 panic 也由 defer 兜住——这正是"恰好一次观测"能成立的结构前提。

## 7. deferred finalizer：收尾动作顺序与理由

以下步骤严格按代码顺序执行；panic 分支执行完**不会** return，因此剩下的收尾照样跑完。

1. panic 恢复 — `if rv := recover(); rv != nil`：置 `rec.outcome = metrics.OutcomeInternal`、`rec.reason = "panic"`，仅当 `rec.status == http.StatusOK` 时改为 `http.StatusInternalServerError`；然后 `p.log.Error("panic recovered", slog.String("request_id", rec.requestID), slog.Any("panic", rv), slog.String("path", rec.path), slog.String("stack", string(debug.Stack())))`，并 `writeError(w, http.StatusInternalServerError, TypeInternal, "internal gateway error")`。**源码注释的观点**：任意 handler goroutine 里的 panic 否则会杀掉进程；恢复、上报、答 500，让一个畸形请求无法拖倒共享网关。注意 `recover()` 只在直接 defer 的函数体内有效——这就是它必须和后面的收尾共处同一个闭包的原因。
2. `elapsed := time.Since(rec.start)` — 延迟只量一次，指标与日志共用同一个值。
3. `p.metrics.ObserveRequest(rec.route, rec.upstream, rec.model, rec.status, rec.outcome, elapsed)` — 每请求恰好一次。
4. `p.settleQuota(r.Context(), rec)`（定义在 `internal/gateway/quotapath.go`）— **源码注释的观点**：预算租约在 defer 里关闭而不是在 happy path 里，是为了让每个出口——一次拒绝、一次失败的 failover、一次 panic、一个流中途消失的客户端——都**恰好结算一次**。传 `r.Context()` 而非后台 context：结算发生在请求生命周期内。
5. `if rec.frames > 0 { p.metrics.ObserveStreamFrames(rec.upstream, rec.frames, rec.respBytes) }`。
6. `if rec.firstTokenSet { p.metrics.ObserveFirstToken(rec.upstream, rec.model, rec.firstToken) }`。第 5、6 步与第 3 步之间的**源码注释的观点**：尝试（attempt）样本在它们发生的地方（`attemptUpstream`）观测，**不在这里**；一个发生了 failover 的请求必须为每个后端贡献一个尝试样本，否则失败后端的错误率一直不可见，它的熔断器也就永远无法凭证据跳闸。
7. `p.idempotencyComplete(rec)`（定义在 `internal/gateway/idempotency.go`）— **源码注释的观点**：重放存储在这里、trace 关闭之前结算，且已经在响应上线之后——一个慢、满或不可用的 store 绝不能拖慢它正想记住的那个回答，而 trace 应该记下 store 的决定。顺序含义：它先于第 8 步，所以 `rec.idemDecision` / `rec.idemStore` / `rec.idemReason` 已定稿。
8. trace 事件（全部经 `requestTrace.addEvent`，nil 安全）：`if rec.idemKey != ""` → 事件名 `"idempotency."+rec.idemDecision`，属性 `key`、`reason`；`if rec.cacheStatus != ""` → 事件名 `"cache."+rec.cacheStatus`，属性 `reason`；`if rec.failovers > 0` → 事件名 `"failover"`，属性 `failovers`、`tried`；`if rec.frames > 0` → 事件名 `"stream.complete"`，属性 `frames`、`response_bytes`、`first_token_ms`（末者由 `firstTokenMS(rec)` 求得，定义在 `internal/gateway/tracing.go`，未观测到首 token 时返回 0）。
9. `rec.trace.finish(rec)`（`internal/gateway/tracing.go`）— **源码注释的观点**：M5 在这里关闭 trace，位置在响应写完之后、指标之后，这样一个慢导出器无法延迟回答；上面那些事件正是让 trace 脱离日志行也能读懂的东西——把解释结果的裁决记在它们被知晓的地方。`finish` 自身把 `outcome != metrics.OutcomeSuccess` 映射为错误状态，并容忍 nil 接收者。
10. `p.recordSession(r, rec)`（定义在 `internal/gateway/sessionpath.go`）— **源码注释的观点**：M6 的会话汇总最后更新，这样它拿到的是最终状态、实测延迟与尝试次数。它是唯一在日志行之前、用 `*http.Request` 取会话头的步骤。
11. 日志级别选择：`level := slog.LevelInfo`；`switch rec.outcome` 中 `metrics.OutcomeSuccess` 分支为空体（保持 Info），`metrics.OutcomeCanceled` 降为 `slog.LevelDebug`（客户端自己走了，不算网关的病），`default` 升为 `slog.LevelWarn`。
12. 组装 `attrs []any` 并 `p.log.Log(r.Context(), level, "request", attrs...)` — 基础属性恒有：`request_id`、`route`、`upstream`、`model`、`stream`、`status`、`outcome`、`req_bytes`、`resp_bytes`、`elapsed`。条件属性按序：`firstTokenSet` → `first_token`；`frames > 0` → `frames`；`!rec.usage.IsEmpty()` → `prompt_tokens`/`completion_tokens`/`cached_tokens`，且在 `p.pricing != nil` 时追加 `cost_usd = p.pricing.CostUSD(rec.model, rec.usage.Prompt, rec.usage.Completion)`；`reason != ""` → `reason`；`cacheStatus != ""` → `cache`（`cacheReason != ""` 时再追加 `cache_reason`）；`tenant != ""` → `tenant`；`session != ""` → `session`；`quotaAction != ""` → `quota`（`quotaReason` 再追加 `quota_reason`；`quotaLimit > 0` 时追加 `quota_limit`/`quota_used`/`quota_requested`；`degradedModel != ""` 追加 `degraded_to`；`quotaSettleError != ""` 追加 `quota_settle_error`）；`attempts > 1` → `attempts`；`len(rec.tried) > 1` → `tried = strings.Join(rec.tried, " -> ")`；`idemKey != ""` → `idempotency = rec.idemDecision`（`idemStore`/`idemReason` 非空再各追加一项）。
13. 收尾顺序的总体理由（源码注释的观点汇总）：指标 → 配额结算 → 幂等结算 → trace 关闭 → 会话汇总 → 日志。可观测性与外部存储排在响应之后，唯一一条日志排在最末，因此它是"所有裁决都已知"的快照；把它放在最后也意味着它拿得到配额、缓存、幂等、failover 的最终值。

## 8. `internal/gateway/errors.go` 全文

- 该文件的 import 块 — 只有 `encoding/json` 与 `net/http`。
- `type ErrorBody struct (internal/gateway/errors.go)` — OpenAI 兼容的错误信封，唯一字段 `Error ErrorDetail`，tag `json:"error"`。**源码注释的观点**：InferGate 为自己的失败复现这个形状而不是返回裸文本——已经会解析 provider 错误的客户端（每个 LLM SDK）不用改就能继续工作，而网关特有的错误可以用它的 `type` 前缀区分出来。
- `type ErrorDetail struct (internal/gateway/errors.go)` — 内层错误对象，签名照抄：`Message string \`json:"message"\``、`Type string \`json:"type"\``、`Param any \`json:"param"\``、`Code any \`json:"code"\``。后两者是 `any`，因此普通网关错误序列化成 JSON `null`。
- `TypeBadRequest = "infergate_bad_request" (internal/gateway/errors.go)` — 400 类，客户端请求本身不合法。
- `TypeNoUpstream = "infergate_no_upstream" (internal/gateway/errors.go)` — 没有任何上游能服务该模型。
- `TypeBadGateway = "infergate_upstream_unavailable" (internal/gateway/errors.go)` — 上游不可用（注意常量名与字面值不同名）。
- `TypeTimeout = "infergate_upstream_timeout" (internal/gateway/errors.go)` — 上游超时。
- `TypeInternal = "infergate_internal_error" (internal/gateway/errors.go)` — 网关内部错误；`ServeHTTP` 的 panic 分支用的就是它。
- `TypeBodyTooLarge = "infergate_request_too_large" (internal/gateway/errors.go)` — 请求体超限。
- `TypeQuotaExceeded = "infergate_quota_exceeded" (internal/gateway/errors.go)` — 预算拒绝（HTTP 429）。**源码注释的观点**：它与 provider 自己的限流是两回事，因为调用方的补救方式不同——provider 的 429 说"这个后端忙，重试或换一个"，而这个说"这个租户已花光额度，换任何后端都没用"。
- `TypeQuotaStore = "infergate_quota_unavailable" (internal/gateway/errors.go)` — 因为读不到预算而拒绝（HTTP 503）。**源码注释的观点**：这是网关故障而非调用方的错，单独上报是为了让运营者能区分"Redis 坏了"和"租户超额了"。
- `TypeIdempotencyConflict = "infergate_idempotency_conflict" (internal/gateway/errors.go)` — 复用了 `Idempotency-Key` 但请求体**不同**（HTTP 409）。**源码注释的观点**：这里重放已记录的答案会回答一个调用方没问的问题，所以必须把这个 key 当成一次失误而不是一次命中。
- `TypeIdempotencyInFlight = "infergate_idempotency_in_flight" (internal/gateway/errors.go)` — 同一个 key 的第二个请求在第一个还跑着时到达（HTTP 409）。**源码注释的观点**：它是独立类型，因为补救方式不同——稍后用同一个 key 重试会重放到第一次的答案，而请求体冲突则需要换一个新 key。
- 常量块的整体注释观点：`"infergate_"` 前缀让网关本地类别不会与 `"invalid_request_error"` 这类 provider 错误类型撞车。
- `func writeError(w http.ResponseWriter, status int, typ, msg string) (internal/gateway/errors.go)` — 签名字面照抄。行为：`w.Header().Set("Content-Type", "application/json; charset=utf-8")`、`w.Header().Set("X-Content-Type-Options", "nosniff")`、`w.WriteHeader(status)`，然后 `_ = json.NewEncoder(w).Encode(ErrorBody{Error: ErrorDetail{Message: msg, Type: typ}})`——`Param`/`Code` 未赋值保持零值（序列化成 `null`），编码错误被显式丢弃。无状态、无锁、无返回值；被 `ServeHTTP` 的 panic 分支调用（也在后半与其它文件里被广泛调用）。
- `func writeRawError(w http.ResponseWriter, status int, contentType string, body []byte) (internal/gateway/errors.go)` — 签名字面照抄。行为：`contentType == ""` 时回落为 `"application/json; charset=utf-8"`；设置同一个 `Content-Type` 与 `X-Content-Type-Options: nosniff`，`w.WriteHeader(status)`，然后 `_, _ = w.Write(body)`（写错误被丢弃）。**源码注释的观点**：原样透传上游错误体，因为重新编码 provider 的错误会丢掉客户端用于自动补救的厂商字段（重试提示、配额重置时间戳），所以字节逐字转发。
- `func errorBodyBytes(status int, typ, msg string) []byte (internal/gateway/errors.go)` — 签名字面照抄；把 `writeError` 会发的那份信封渲染成字节。**源码注释的观点**：它为 failover 路径而存在——一次调用方还看不见的失败尝试必须把自己的响应随身带着，好让后续候选仍能作答；在这里产出 body 意味着"所有候选都失败"与"立刻写出"两种情况发出的是**逐字节相同**的错误。实现细节：`_ = status`（作者显式忽略，注释说"状态随 header 单独传递"——所以 `status` 不参与 body 构造）；`json.Marshal(ErrorBody{...})` 出错时返回固定字面量 `{"error":{"message":"internal gateway error","type":"infergate_internal_error","param":null,"code":null}}`，**源码注释的观点**：字符串与 nil 组成的结构体不可能让 `json.Marshal` 失败，这个兜底只是让函数保持"总是有返回值"而不是返回空 body；成功路径 `return append(body, '\n')`，即补一个换行（与 `json.Encoder.Encode` 的输出形态一致）。
- 并发/锁：`errors.go` 三个函数都是无状态的纯写函数，不读 `Proxy` 任何字段，可被任意 goroutine 并发调用；它们之间的唯一契约是"信封形状一致"，靠共用 `ErrorBody` 保证。

## 9. 待续（交下一分册）

- `Options` 及其全部字段、`New(opts Options) *Proxy`：紧邻本册范围但未列入，故未展开（其中 nil 归一化规则已在本册字段条目里引用）。
- 后半同名文件内容全部未写：`serve`、`attemptError`/`Error`/`Unwrap`、`retryableStatus`、`serveAttempts`、`attemptUpstream`、`observeAttempt`、`backoff`、`plan`、`itoa`、`readBody`、`buildRequest`、`passThroughError`、`copyWhole`、`accountJSON`、`isJSON`、`isEventStream`、`usageFromJSON`、`resolveModelName`、`forwardResponseHeaders`、`requestID`、`isProxiablePath`。
- 被本册调用但定义在其它文件、本册只做引用解释的函数：`routeLabel`（`internal/gateway/request.go`）、`requestID`（`internal/gateway/proxy.go` 后半）、`firstTokenMS`、`Tracer.begin`、`requestTrace.addEvent`、`requestTrace.finish`（`internal/gateway/tracing.go`）、`settleQuota`（`internal/gateway/quotapath.go`）、`idempotencyComplete`（`internal/gateway/idempotency.go`）、`recordSession`（`internal/gateway/sessionpath.go`）。


## 10. 本册范围

本册覆盖网关核心管线的**中段**：闸门顺序、候选链构造、逐个候选尝试与失败转移、最终写出口，以及请求体的"只读式"解析。

- 闸门与编排：`serve`、`plan`、`serveAttempts`、`attemptUpstream`、`observeAttempt`、`retryableStatus`、`backoff`、`attemptError`（及其 `Error`/`Unwrap`），均在 `internal/gateway/proxy.go`。
- 请求体解析：`internal/gateway/request.go` 全文。

**不在本册**：`Proxy` 结构体、`record`、`Options`、`New`、`ServeHTTP`、`errors.go`、`buildRequest`、`readBody`、`copyWhole`、`passThroughError`、`relayStream`、响应头转发、定价与用量解析 —— 由前后两位走读员负责。文中出现这些符号时只作"被调用"标注。

---

## 11. serve 的闸门顺序

### serve (internal/gateway/proxy.go)

`func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, rec *record)`

- **作用**：请求管线主体，由 `ServeHTTP` 调用，并处在 `ServeHTTP` 的 deferred recorder 之内（源码注释：`// serve is the request pipeline. The deferred recorder in ServeHTTP wraps it.`）。它把一条请求依次送过六道闸门，并在任一闸门终结时直接返回。
- **六道闸门，顺序不可换**：
  1. **路径闸门**：`!isCompletionPath(r.URL.Path)` 时再查 `!isProxiablePath(r.URL.Path)`，不通过则 `rec.status = http.StatusNotFound`、`rec.outcome = metrics.OutcomeBadRequest`、`rec.reason = "unsupported path"`，`writeError(w, http.StatusNotFound, TypeBadRequest, "unsupported path "+r.URL.Path+"; InferGate proxies OpenAI-compatible endpoints only")`。源码注释的观点（为什么非补全路径也放行）：`// Non-completion endpoints (/v1/models, /health) are proxied too, so a client can point one base_url at the gateway and reach everything.`
  2. **读体闸门**：`p.readBody(r)` 返回 `(body, tooLarge, err)`。`err != nil` → 400 `TypeBadRequest` + 字符串 `"could not read request body: "+err.Error()`；`tooLarge` → `http.StatusRequestEntityTooLarge` + `TypeBodyTooLarge` + `"request body exceeds the configured limit"`。成功则 `rec.reqBytes = int64(len(body))`。
  3. **解析闸门**：`parsed := inspectRequest(body)`，随后写 `rec.model`、`rec.explicitModel = parsed.Model`、`rec.stream = parsed.Stream && isStreamPath(r.URL.Path)`。此闸门不拒绝任何请求。
  4. **身份闸门**：`rec.tenant` 为空时 `tenantFor(r)`，`rec.session` 为空时取 `strings.TrimSpace(r.Header.Get(HeaderSession))`。源码注释的观点（为什么一次解析而不是各子系统懒解析）：租户决定缓存与预算的隔离边界、session 是账本汇总单位，二者必须在第一个子系统运行前已知；且即便部署里没有缓存也没有预算，日志行也应当带上它们。
  5. **准入闸门**：条件 `p.quota != nil && p.quota.Enabled() && isCompletionPath(r.URL.Path)`，调用 `p.admitQuota(w, r, rec, body, parsed)` 返回 `(body, parsed, admitted)`；`!admitted` 立即 `return`；否则回写 `rec.model = parsed.Model`。
  6. **幂等闸门**：`w, done = p.idempotencyBegin(w, r, rec, body)`；`done` 为真直接 `return`（注意 `w` 可能被换成一个缓冲写口，后续必须用新的 `w`）。
  7. **缓存闸门**：`p.cache != nil` 时先 `p.prepareCache(w, r, rec, body, parsed)`，再 `p.cacheAttempt(w, r, rec)`；命中即 `return`。
  8. **路由与尝试**：`p.serveAttempts(w, r, rec, parsed, body)`。

- **三条排序理由（源码注释原文观点）**：
  - **准入先于缓存**：`// M3: admission runs BEFORE the cache lookup, and the order is load-bearing in both directions.` 两个方向是：其一，缓存命中虽然免费但并非"不可见"——它消耗一个每分钟名额、也应计入租户报表，所以先开租约、命中后再以零 token 用量结算；其二，即将被拒绝的请求绝不能被缓存先行应答，否则超预算的租户会持续拿到流量。
  - **只治理补全路由**：`// Only completion routes are governed.` 若把 `/v1/models` 列表计入预算，会因零成本流量而拒绝调用方；而在没有 prompt 的请求上谈 token 维度纯属虚构。
  - **幂等先于缓存**：`// M6: the idempotency claim is taken after admission and before the cache.` 在准入之后，因为重放确实消耗每分钟名额（与缓存命中同理）；在缓存之前，因为重放调用方**自己**那笔已完成操作，比返回"相似问题的答案"是更强的承诺。
  - **缓存先于路由**：`// M2: the cache answers before routing is even considered, because a hit makes the routing decision moot.` 缓存键基于**客户端原样发来的** body 派生，早于任何面向后端的模型改写，于是两个只在模型名拼写上不同的请求共享同一个 exact key。

- **读写状态**：写 `rec.status/outcome/reason/reqBytes/model/explicitModel/stream/tenant/session`，读 `r.URL.Path`、请求头、`p.quota`、`p.cache`。可能替换 `w`（幂等闸门返回的 `w`）。
- **并发/锁**：`serve` 自身不取锁。源码注释的观点：`Proxy` 的所有字段构造后只读，因此可被所有处理 goroutine 共享而无需加锁；可变状态只存在于 `record` 与局部变量中。
- **被谁调用**：`(*Proxy).ServeHTTP`。

### isCompletionPath (internal/gateway/request.go)

`func isCompletionPath(path string) bool`

- **作用**：去掉尾部一个 `/` 后，判断路径是否以 `/chat/completions`、`/completions` 或 `/embeddings` 结尾。源码注释的观点：这些是"body 里带 `model` 字段、网关可能需要改写"的模型生成调用。
- 注意 `TrimSuffix` 只去一个尾斜杠，`/v1/chat/completions//` 不算补全路径。
- **被谁调用**：`serve`（路径闸门、准入闸门条件）。

### isStreamPath (internal/gateway/request.go)

`func isStreamPath(path string) bool`

- **作用**：仅 `strings.HasSuffix(strings.TrimSuffix(path, "/"), "/chat/completions")` 为真。源码注释的观点：M0 里只有 chat completions 会产 SSE，`completions`/`embeddings` 一律返回整流 body。
- **被谁调用**：`serve` 计算 `rec.stream`。

---

## 12. 候选链与尝试循环

### plan (internal/gateway/proxy.go)

`func (p *Proxy) plan(parsed parsedRequest, r *http.Request) ([]router.Candidate, error)`

- **作用**：把一次请求解析成有序候选链。先 `explicit := strings.TrimSpace(r.Header.Get(HeaderUpstream))`；再把 `HeaderCapabilities` 按 `,` 切分、逐段 `TrimSpace`、丢弃空段得到 `caps`。
- **两条分支**：
  - 有路由器（`p.router != nil`）：交给 `p.router.Plan(router.Request{Model: parsed.Model, Capabilities: caps, Explicit: explicit, Messages: parsed.Messages, MaxTokens: parsed.MaxTokens})` 并原样返回其结果与错误。
  - **M0 回退**：无路由器时只解析一个后端。`target, err := p.upstreams.Resolve(parsed.Model, explicit)`；失败时 `return nil, err`。成功则 `model, _ := p.resolveModelName(target, parsed.Model)`，返回单元素链 `[]router.Candidate{{Target: target, Model: model, Healthy: true, Reason: "direct"}}`，`Reason` 固定为 `"direct"`（它会经 `attemptUpstream` 落进 `rec.reason`）。
- **读写状态**：只读 `p.router`、`p.upstreams`、请求头与 `parsed`；不写 `rec`。
- **并发/锁**：无锁，依赖被调用方的内部同步。
- **被谁调用**：`serveAttempts`。

### serveAttempts (internal/gateway/proxy.go)

`func (p *Proxy) serveAttempts(w http.ResponseWriter, r *http.Request, rec *record, parsed parsedRequest, body []byte)`

- **作用**：按 `plan` 的顺序逐个候选尝试，直到某次尝试产出答案；全部失败时由它负责**唯一的终局写出口**。源码注释：`// serveAttempts walks the routing plan until one attempt produces an answer.`
- **计划失败路径**：`p.plan` 返回错误 → `rec.status = http.StatusBadRequest`、`rec.outcome = metrics.OutcomeBadRequest`、`rec.reason = "no upstream"`、`writeError(w, http.StatusBadRequest, TypeNoUpstream, err.Error())`。源码注释的观点：`// An unroutable model is the caller's problem, not an upstream outage`；此处答 502 会因一个拼写错误而归咎后端并触发熔断器，400 且带上模型名才是可行动的答案。
- **循环体关键点**：
  - `if i >= p.maxAttempts { rec.reason = "failover budget exhausted"; break }` —— 用独立计数器封顶，防止候选链比配置的尝试上限更长。
  - 进入尝试前检查 `r.Context().Err() != nil`：调用方已离开，则 `rec.outcome = metrics.OutcomeCanceled`、`rec.status = 499`、`rec.reason = "client went away during failover"` 并 `return`。源码注释的观点：给一个没人会读的答案继续烧供应商配额是浪费。
  - **每次尝试都拿完整预算**：`p.attemptUpstream(w, r, rec, cand, parsed.Model, body, i+1, p.upstreamTTL)`，第 8 个实参恒为 `p.upstreamTTL`（`budget time.Duration`），不是"共享剩余预算"。源码注释给出的理由是：`// Each attempt receives the FULL upstream_timeout, not what is left of a shared request deadline.` —— 若用共享截止时间，第一个候选超时后预算归零，之后每一个**健康**的候选都会被塞一个已经过期的 deadline，网关于是在**从未真正询问第二个后端**的情况下答 504，而这恰恰是失败转移存在的场景（慢主 + 快备）。代价是最坏情况下一次请求耗时可到 `max_attempts * upstream_timeout`；源码注释明确说这是运维方的取舍，两个旋钮都在配置里显式存在，替代方案则是"只要第一个后端是慢的那个，这个特性就静默失效"。
  - 返回值处理：`attempt == nil` 时两个分支都 `return`（`cancelled` 为真表示客户端已走或流中途断了，重试要么不可能要么无意义）。`attempt.err == nil` 表示已完整投递，`return`。否则 `last = attempt`；`if !attempt.retryable { return }`（已写过字节，不能再接第二个响应）。
  - 最后一个候选也失败时补一句 `rec.reason = "all " + itoa(len(plan)) + " candidates failed; last: " + attempt.err.Error()`；随后 `rec.failovers++` 并 `p.metrics.ObserveFailover(rec.upstream, metrics.OutcomeUpstreamErr, attempt.status)`。
  - 退避：仅当 `i+1 < len(plan) && i+1 < p.maxAttempts` 才取 `p.backoff(i)`；`delay > 0` 时 `select` 在 `time.After(delay)` 与 `r.Context().Done()` 之间等待，后者触发则 `rec.status = 499`、`rec.outcome = metrics.OutcomeCanceled`、`rec.reason = "client canceled during failover backoff"` 并 `return`。
- **最终写出口的判定条件**：`if last != nil && !last.clientGone`。源码注释的观点逐条保留：
  - `// rec.status currently holds the LAST attempt's status, which was never written to the client (a retryable failure writes nothing by design). It must not be used as a "have we answered yet" flag: a 503 from the last candidate would look like an answer that was already sent.` —— 所以用 `last`（是否发生过失败）与 `clientGone`（调用方是否还在）判定，而不是 `status > 0`。
  - `// Nothing in this loop writes to w before this point, so the only question that matters is whether the caller is still there to receive an error.`
  - 分支一 `last.status > 0`（确有供应商答过）：`rec.status = last.status`、`rec.outcome = metrics.OutcomeUpstreamErr`；若 `last.status == http.StatusGatewayTimeout` 则改判 `rec.outcome = metrics.OutcomeTimeout` —— 源码注释的观点：这个 504 来自网关自己的 deadline 而非供应商，把它归为上游错误会让分类学里最不同的两种失败（"后端说了不" vs "后端根本没开口"）在指标里无法区分，而两者的应对方向相反（改请求 vs 加更快的后端）。随后 `forwardResponseHeaders(w.Header(), last.header)`，设 `HeaderUpstreamName`、`HeaderAttempt = itoa(rec.attempts)`、`HeaderRequestID`，`len(rec.tried) > 1` 时再设 `HeaderTried = strings.Join(rec.tried, ", ")`，最后 `writeRawError(w, last.status, last.header.Get("Content-Type"), last.body)` —— 原样透传供应商的状态码与错误信封；源码注释的观点：把 429 改写成 502 会破坏客户端自己的退避逻辑，而退避是 OpenAI 兼容 SDK 唯一保证具备的东西。
  - 分支二 `last.status == 0`（连状态都没有）：`rec.status = http.StatusBadGateway`、`rec.outcome = metrics.OutcomeUpstreamErr`，`rec.reason` 为空时填 `last.err.Error()`，`writeError(w, http.StatusBadGateway, TypeBadGateway, "no upstream could serve this request: "+last.err.Error())`。
  - 两个分支都不命中（`last == nil` 或 `clientGone`）时**什么都不写**，函数返回。
- **读写状态**：写 `rec.status/outcome/reason/failovers/attempts/tried/upstream`（后四项由 `attemptUpstream` 写）；读 `p.maxAttempts`、`p.upstreamTTL`、`p.retryBackoff`、`p.metrics`。
- **并发/锁**：本函数无锁；`rec` 是请求私有状态，不存在跨 goroutine 竞争。
- **被谁调用**：`serve`。

### attemptError (internal/gateway/proxy.go)

`type attemptError struct { err error; retryable bool; clientGone bool; status int; header http.Header; body []byte }`

- **作用**：记录一次上游尝试为何结束，以及客户端是否已被应答。源码注释的观点：`// The distinction between "the upstream failed before we answered anything" and "we already wrote bytes to the client" is the whole basis of safe failover: once any part of a response has been written, retrying a different backend would splice two responses together and corrupt the stream.`
- **字段语义（照抄注释）**：`retryable` 为真表示这次失败"干净"到没有任何响应字节到达客户端；`clientGone` 为真表示失败来自**调用方**——它断开了，或网关自己的截止时间在等待中触发，该状态下不向连接写任何东西；`status`/`header`/`body` 承载上游自己的失败响应，存在的目的是当所有候选都失败时，让客户端收到供应商的状态码与错误信封，而不是一个通用 502（在 429 上退避、能区分 503 与 500 的 SDK 必须继续看到真实码；供应商的错误消息是能摆到用户面前的最有用的东西）。
- **被谁构造**：`attemptUpstream` 的各失败分支。

### (*attemptError) Error (internal/gateway/proxy.go)

`func (e *attemptError) Error() string`

- **作用**：`e == nil || e.err == nil` 时返回 `""`，否则 `e.err.Error()`。nil 接收者安全。

### (*attemptError) Unwrap (internal/gateway/proxy.go)

`func (e *attemptError) Unwrap() error`

- **作用**：`e == nil` 时返回 `nil`，否则 `e.err`，使 `errors.Is/As` 可穿透到原始 `client.Do` 错误。

### retryableStatus (internal/gateway/proxy.go)

`func retryableStatus(status int) bool`

- **作用**：`return status == http.StatusTooManyRequests || status >= 500` —— **只有 5xx 与 429 值得失败转移**。
- **为什么包含 429（源码注释原文观点）**：`// A 4xx is a verdict about the REQUEST -- a bad model name, a malformed tool schema, a missing parameter -- and every other backend would reject it identically, so retrying it costs latency, burns provider quota and hides the caller's own bug behind a generic gateway error. 429 is included because it is a verdict about the moment, not about the request: another backend may well have capacity.` 即 4xx 是对**请求本身**的判决，换个后端会同样拒绝；429 是对**此刻**的判决，别的后端可能有余量。
- **被谁调用**：`attemptUpstream`（`resp.StatusCode >= 400` 分支内）。

### backoff (internal/gateway/proxy.go)

`func (p *Proxy) backoff(attempt int) time.Duration`

- **作用**：返回"第 n+2 次尝试之前"的等待时长。`p.retryBackoff <= 0` 直接返回 `0`；否则 `base := p.retryBackoff * time.Duration(attempt+1)`，`jitter := time.Duration(rand.Int63n(int64(base/2) + 1))`，返回 `base/2 + jitter`（线性增长 + 全抖动，抖动区间为 `base` 的 `[50%, 100%]`）。
- **理由（源码注释）**：`// Linear growth with full jitter: the linear part keeps a two-backend fleet from waiting seconds to fail over, and the jitter stops every client that was affected by the same outage from retrying in lockstep and re-creating the stampede the breaker just absorbed.` 另一句说明熵来源：测试里抖动只按尝试序号确定，生产路径用时钟，因为它是请求路径上最便宜的熵源。
- **读写状态**：只读 `p.retryBackoff`；使用包级 `math/rand` 的 `rand.Int63n`。
- **被谁调用**：`serveAttempts` 的退避分支（实参是循环下标 `i`，因此第一次重试拿到 `attempt = 0`）。

---

## 13. 单次上游交换

### attemptUpstream (internal/gateway/proxy.go)

`func (p *Proxy) attemptUpstream(w http.ResponseWriter, r *http.Request, rec *record, cand router.Candidate, requestedModel string, body []byte, attemptNo int, budget time.Duration) (*attemptError, bool)`

- **契约（源码注释）**：`// It returns (nil, true) when the client is gone or the response is already committed, and (nil, false) when the exchange succeeded and was fully relayed.` 注意第三种形态 `(err, false)` 未被这句话覆盖，但由下方各失败分支给出。
- **开头记账**：`target := cand.Target`；`rec.upstream = target.Name`；`rec.attempts++`；`rec.tried = append(rec.tried, target.Name)`；`rec.reason = cand.Reason`。
- **熔断检查**：
  - `brk := (*breaker.Breaker)(nil)`；仅当 `p.breakers != nil` 时 `brk = p.breakers.Get(target.Name)`。
  - 源码注释的观点（时机）：`// One decision per attempt, taken immediately before it: asking earlier would let a breaker that tripped meanwhile be ignored, and asking later (after the request is built) would waste the work.` 以及"熔断器只查一次，因为下面归还半开名额与之后上报的每个判决都必须是同一个它"。
  - `d := brk.Allow()`，取 `allowed, probe = d.Allowed, d.Probe`。`!allowed` 时：`rec.trace.addEvent("breaker.reject", map[string]any{"upstream": target.Name, "reason": d.Reason})`，返回 `&attemptError{err: errors.New(target.Name + ": " + d.Reason), retryable: true}, false`。源码注释的观点：拒绝是一个**决策而非一次交换**，所以只记 event 不开 client span——请求根本没离开进程，一个零耗时 span 会暗示它离开过。`d.Reason` 的典型取值（`internal/breaker/breaker.go`）：`"half-open probe already in flight"`。
- **半开名额归还协议**：
  - `reported := false`；`if probe { defer func() { if !reported { brk.ReleaseProbe() } }() }`。
  - `reportSuccess := func()`：`brk == nil` 时直接返回，否则 `reported = true; brk.RecordSuccess()`。
  - `reportFailure := func(timeout bool)`：`brk == nil` 时直接返回，否则 `reported = true; brk.RecordFailure(timeout)`。
  - 源码注释的观点（为什么用两个闭包）：`// reported is set by every line below that tells the breaker how this attempt ended; the two closures exist so that setting it cannot be forgotten at one of the eight verdict sites.`
  - 源码注释的观点（为什么故意不上报的路径也要归还名额）：调用方挂断、请求根本没能构造出来这两类路径**不上报任何后端证据**，但仍必须交回半开名额——半开熔断器只放行**恰好一次**尝试然后等待，一个被放行却从未上报的探测会让该后端在进程余生都被 `"half-open probe already in flight"` 拒绝；取消的客户端不是关于后端的证据，所以这里归还名额而不是记一次失败。
- **client span**：`span := rec.trace.startAttempt(target.Name, attemptNo)` + `defer span.finish(rec)`。源码注释：每次被放行的尝试一个 client span，defer 在下方所有返回路径上都会关闭它——包括那些从未触网的路径，于是 trace 里的尝试数与指标一致。
- **候选级模型改写**：`outBody := body`；若 `cand.Model != "" && cand.Model != requestedModel`，则 `rewritten, rerr := rewriteModel(body, cand.Model)`；`rerr != nil` 时返回 `&attemptError{err: rerr}, false`（注意 `retryable` 为假，直接终结管线）；成功则 `outBody = rewritten`、`rec.model = cand.Model`。源码注释的观点：这个后端不serves 请求名（本地单体模型机器），就按它声明的名字问它；M0 曾在解析后统一做一次，现在放在候选上，因为不同候选可能需要不同的改写决策。
- **构造与超时**：`req, client, cleanup, err := p.buildRequest(r, target, outBody, budget, rec)`；`err != nil` → `return &attemptError{err: err}, false`（`retryable` 为假，半开名额由 defer 归还）。随后 `defer cleanup()`。
- **`client.Do` 错误分类**（`attemptStart := time.Now()`，`resp, err := client.Do(req)`，先 `rec.status = 0`）：
  - `errors.Is(err, context.Canceled) || r.Context().Err() != nil`：调用方挂断。`rec.outcome = metrics.OutcomeCanceled`、`rec.status = 499`（源码注释：非标准，nginx 惯例，表示客户端关闭请求）、`rec.reason = "client canceled"`，返回 `&attemptError{err: err, clientGone: true}, true`。源码注释的观点：什么都不可写——已经没有连接可写——也没有别的候选值得试，答案无处可去。
  - `errors.Is(err, context.DeadlineExceeded)`：网关自己的截止时间触发，而调用方仍在等待。源码注释逐条保留：`// NOTHING is written to the client here, deliberately.` 调用方只要一个答案且一个字节都没收到，因此换一个候选仍可能产出可用答案，而"慢后端"正是需要第二个后端的最常见原因；此刻写 504 会把客户端钉死在一个失败转移本该避免的失败上，而状态码一旦写出就收不回来。该超时以**可重试的** attempt error 形式携带供应商状态上报：`rec.outcome = metrics.OutcomeTimeout`、`rec.status = http.StatusGatewayTimeout`、`rec.reason = "upstream timeout: " + target.Name`，然后 `reportFailure(true)`、`p.observeAttempt(target, http.StatusGatewayTimeout, rec.outcome, time.Since(attemptStart))`，返回的 `attemptError` 为 `err: errors.New(target.Name + ": gateway deadline exceeded")`、`status: http.StatusGatewayTimeout`、`header: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}`、`body: errorBodyBytes(http.StatusGatewayTimeout, TypeTimeout, "no upstream answered before the gateway deadline")`、`retryable: true`。源码注释的观点（后续如何收尾）：若每个候选都超时，`serveAttempts` 原样转发这个 504 与 body；若后面的候选成功，客户端只看到成功，除指标与 `X-InferGate-Tried` 头之外不会知道曾经超时。
  - `default`：`rec.status = http.StatusBadGateway`、`rec.outcome = metrics.OutcomeUpstreamErr`、`rec.reason = target.Name + ": " + err.Error()`，`p.observeAttempt(target, 0, rec.outcome, time.Since(attemptStart))`、`reportFailure(false)`，返回 `&attemptError{err: err, retryable: true}, false`（无 `status`，故最终只能写 502）。
- **上游 ≥400 的两种处置**（`defer resp.Body.Close()`，随后 `rec.status = resp.StatusCode`）：
  - **可重试**（`retryableStatus(resp.StatusCode)`）：`failureBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))` —— 有界读而不是不读就关。源码注释的观点：这既是连接复用的要求，也是"万一所有候选都失败时客户端将看到的错误消息"。随后 `outcome := metrics.OutcomeUpstreamErr`、`p.observeAttempt(target, resp.StatusCode, outcome, time.Since(attemptStart))`、`reportFailure(resp.StatusCode == http.StatusGatewayTimeout)`、`rec.outcome = outcome`、`rec.reason = target.Name + ": upstream status " + resp.Status`，返回携带 `err: errors.New(target.Name + " returned " + resp.Status)`、`status: resp.StatusCode`、`header: resp.Header.Clone()`、`body: failureBody`、`retryable: true` 的 `attemptError`。源码注释的观点（status 为何要随错误一起走）：`// The status travels with the error so that, if every candidate fails, the client still receives the PROVIDER's status code rather than a generic 502. An SDK that retries on 429 must keep seeing 429.`
  - **不可重试 4xx：直接透传且不重试**。`rec.outcome = metrics.OutcomeBadRequest`，依次 `forwardResponseHeaders(w.Header(), resp.Header)`、`w.Header().Set(HeaderUpstreamName, target.Name)`、`w.Header().Set(HeaderAttempt, itoa(attemptNo))`、`w.Header().Set(HeaderRequestID, rec.requestID)`，`len(rec.tried) > 1` 时设 `HeaderTried`，然后 `p.passThroughError(w, resp, rec)`、`p.observeAttempt(target, resp.StatusCode, rec.outcome, time.Since(attemptStart))`、**`reportSuccess()`**，返回 `nil, false`。
    - 为什么透传：源码注释的观点是"调用方错误原样转发且不重试：换任何别的后端都会同样拒绝这个请求"。
    - 为什么上报**成功**：`// A 4xx is charged to the caller, not the backend, so it must not count toward the breaker's failure ratio. Counting it would let a client with a bad payload trip a healthy upstream out of rotation.` 即 4xx 记在调用方账上，计进熔断失败率会让一个坏 payload 的客户端把健康上游踢出轮换。
- **成功后的两条投递路径**（先统一设置 `HeaderUpstreamName`/`HeaderAttempt`/`HeaderRequestID` 及条件性 `HeaderTried`）：
  - **流式**：条件 `rec.stream && isEventStream(resp.Header.Get("Content-Type"))`，`rec.outcome = p.relayStream(w, r, resp, target, rec)`。随后：`rec.outcome == metrics.OutcomeSuccess` → `reportSuccess()`；否则 `rec.outcome != metrics.OutcomeCanceled` → `reportFailure(false)`。源码注释的观点：首帧之后失败的流无法重试——客户端已经握着半个答案，再来一个 `[DONE]` 就是协议违规；只有失败属于上游时才计入熔断器（取消不算）。
  - **整流**：`rec.outcome = p.copyWhole(w, r, resp, rec)`。判定同上，但失败时是 `reportFailure(rec.outcome == metrics.OutcomeTimeout)`，即只有 `OutcomeTimeout` 才作为超时上报。
- **收尾**：两条路径汇合后 `p.observeAttempt(target, resp.StatusCode, rec.outcome, time.Since(attemptStart))`；`!rec.usage.IsEmpty()` 时 `p.metrics.ObserveTokens(target.Name, rec.model, rec.usage.Prompt, rec.usage.Completion, rec.usage.Cached)`；返回 `nil, false`。
  - 源码注释的观点（为什么首 token 与流帧指标不在这里记）：它们只由 `ServeHTTP` 的 deferred recorder 记**一次**，过去两处都记导致两个描述流式的指标族把每个流式请求双计；deferred 处才是正确位置，因为它还覆盖缓存命中、并把样本归因到真正服务的后端上。
- **读写状态**：写 `rec.upstream/attempts/tried/reason/status/outcome/model/usage`（`usage` 由 `relayStream`/`copyWhole` 填）；读 `cand`、`requestedModel`、`budget`、`p.breakers`、`p.metrics`、`p.stream` 相关配置经由 `rec.stream`。
- **并发/锁**：本函数自身不加锁；`brk.Allow/RecordSuccess/RecordFailure/ReleaseProbe` 内部取 `b.mu`；`rec.trace` 与 `p.metrics` 自行同步。`rec` 为单请求私有，`defer span.finish(rec)` 与 `defer cleanup()` 与 `defer resp.Body.Close()` 的 LIFO 顺序保证 span 最后关闭。
- **被谁调用**：`serveAttempts`（每次尝试一次）。

### observeAttempt (internal/gateway/proxy.go)

`func (p *Proxy) observeAttempt(target *upstream.Target, status int, outcome metrics.Outcome, elapsed time.Duration)`

- **作用**：把一次尝试同时记进指标与熔断窗口。先 `p.metrics.ObserveUpstreamAttempt(target.Name, status, outcome, elapsed)`；再在 `p.breakers != nil` 时 `window := p.breakers.Get(target.Name).Stats()`，并按 outcome 三分：`OutcomeSuccess` → `window.RecordSuccess(elapsed)`；`OutcomeTimeout` → `window.RecordFailure(true)`；`outcome != metrics.OutcomeCanceled` → `window.RecordFailure(false)`；`OutcomeCanceled` 什么都不记。
- **为什么在热路径喂窗口（源码注释）**：`// The window is fed here rather than in the deferred recorder because the window is what the breaker and the router read: an attempt that failed must appear in it immediately, or a backend that is failing 100% of requests keeps looking healthy for as long as the router's ranking is stale.`
- **被谁调用**：`attemptUpstream` 的各分支（`client.Do` 错误、可重试状态、4xx 透传、以及两条成功投递路径的收尾）。注意它自身不会调用 `brk.RecordSuccess/RecordFailure`——那两个是 `attemptUpstream` 的闭包在做；本函数只喂 `window`。

---

## 14. request.go 全文

### chatRequestFields (internal/gateway/request.go)

`type chatRequestFields struct { Model string \`json:"model"\`; Stream bool \`json:"stream"\`; Messages json.RawMessage \`json:"messages"\`; MaxTokens json.RawMessage \`json:"max_tokens"\`; MaxCompletionTokens json.RawMessage \`json:"max_completion_tokens"\` }`

- **作用**：InferGate 从 OpenAI 兼容 chat completion 请求里**读取**的字段集合。源码注释的观点：`// Everything else travels through verbatim.` 其余一切都原样通行。
- **为什么两个上限都读（源码注释）**：`max_tokens` 是原始写法且仍最常见，`max_completion_tokens` 是新版 OpenAI 模型要求的写法，两种拼写都真实存在于野外；调用方发了哪个，配额估算就用哪个。
- **并发/锁**：无；纯值类型。

### parsedRequest (internal/gateway/request.go)

`type parsedRequest struct { Model string; Stream bool; MaxTokens int; Messages json.RawMessage; IsJSON bool }`

- **作用**：检视入站 body 的结果。
- **字段语义（照抄注释要点）**：`Model` 是请求的模型，body 省略时为空；`Stream` 是请求的流式开关；`MaxTokens` 是调用方要求的补全上限、未要求时为 0 —— 注释的观点是它用来给配额预留定尺：一个已经自报上界的调用方，理应在**两个方向上**都拿到与之匹配的租约；`Messages` 是请求序列化后的消息数组、body 未携带时为 nil，分层路由读它来估算 prompt 大小，且它保持原始形态，因此定尺**永远不需要第二次解码 body**；`IsJSON` 表示 body 是否按 JSON 对象解析成功 —— 非 JSON 的 body 仍然会被代理，源码注释的观点是 `// A non-JSON body is still proxied: the gateway must not become a validator that rejects traffic the upstream would accept.`
- **并发/锁**：无。

### inspectRequest (internal/gateway/request.go)

`func inspectRequest(body []byte) parsedRequest`

- **作用**：提取 InferGate 需要的字段而不扰动 body。`len(bytes.TrimSpace(body)) == 0` 直接返回零值 `parsedRequest{}`；`json.Unmarshal(body, &fields)` 失败同样返回零值（`IsJSON` 为假）；成功则返回 `parsedRequest{Model: fields.Model, Stream: fields.Stream, MaxTokens: completionCeiling(fields.MaxTokens, fields.MaxCompletionTokens), Messages: fields.Messages, IsJSON: true}`。
- **为什么解进结构体而不是 `map[string]any` / `interface{}`（源码注释原文观点）**：body 被解码进 `map[string]json.RawMessage` 再重新编码，而不是解码成 `interface{}`；两种做法都能往返，但**只有 RawMessage 形式保留每个值的精确表示**：大整数、高精度浮点、供应商特有的嵌套对象都保持原始字节，于是网关从未看一眼的字段不可能被悄悄重规范化（64 位 token 计数变成 `1e+06` 是典型症状）。
- **错误路径**：无错误返回，失败一律退化为零值（不拒绝请求）。
- **被谁调用**：`serve`。

### completionCeiling (internal/gateway/request.go)

`func completionCeiling(maxTokens, maxCompletionTokens json.RawMessage) int`

- **作用**：从两种拼写里挑出调用方的补全上限。按 `[]json.RawMessage{maxCompletionTokens, maxTokens}` 的顺序遍历，跳过 `len(raw) == 0` 或 `string(raw) == "null"` 的项；能 `json.Unmarshal(raw, &n)` 成功且 `n > 0` 就返回 `n`；循环走完返回 `0`。
- **为什么 `max_completion_tokens` 优先（源码注释）**：`// max_completion_tokens wins when both are present: it is the field the models that accept it actually enforce, so honouring the deprecated spelling there would reserve against a number the provider ignores.` 两个都在时 `max_completion_tokens` 胜出，因为接受它的模型真正执行的是这个字段；在那里照顾已废弃的拼写，等于按一个供应商会忽略的数字去预留。
- **被谁调用**：`inspectRequest`。

### rewriteModel (internal/gateway/request.go)

`func rewriteModel(body []byte, model string) ([]byte, error)`

- **作用**：返回把顶层 `"model"` 字段设成 `model` 的 body。
- **分支**：`len(body) == 0` → `return body, nil`（不改）。`json.Unmarshal(body, &fields)` 失败 → `return body, fmt.Errorf("gateway: body is not a JSON object: %w", err)`（错误字符串原文如此，`%w` 包住底层的 `encoding/json` 错误）。若 `fields["model"]` 存在且能解析成字符串、且 `name == model`，则 `return body, nil`。否则 `json.Marshal(model)`；该步出错返回 `body, err`；成功则 `fields["model"] = encoded`，最后 `return json.Marshal(fields)`。
- **为什么无需变更时零分配（源码注释）**：`// Returns the input unchanged when nothing needs to change, so the common path performs no allocation.`
- **为什么输出是确定性的（源码注释）**：`json.Marshal` 按字典序排对象键，所以改写后的 body 是确定性的；OpenAI 兼容服务并不定义字段顺序，确定性输出让测试里的请求体断言稳定。
- **错误路径**：只在 body 不是 JSON 对象或 `json.Marshal` 失败时返回错误，且**始终把原 body 一并返回**（调用方 `attemptUpstream` 在 `rerr != nil` 时直接以不可重试错误终止）。
- **被谁调用**：`attemptUpstream`（候选级模型改写）。

### rewriteMaxTokens (internal/gateway/request.go)

`func rewriteMaxTokens(body []byte, cap int) ([]byte, error)`

- **作用**：返回把补全上限**降到** `cap` 的 body。`len(body) == 0 || cap <= 0` 直接返回原 body；`json.Unmarshal` 失败返回 `fmt.Errorf("gateway: body is not a JSON object: %w", err)`。
- **字段选择**：默认 `field := "max_tokens"`；`raw, ok := fields["max_completion_tokens"]`，不存在再试 `fields["max_tokens"]`。若取到：能解析成 `int` 且 `current > 0 && current <= cap` 就原样返回（已经够低）；否则若 `fields["max_completion_tokens"]` 存在则把 `field` 改成 `"max_completion_tokens"`。最后 `encoded, err := json.Marshal(cap)`，`fields[field] = encoded`，`return json.Marshal(fields)`。
- **为什么只降不升（源码注释原文观点）**：它是降级阶梯的第二根杠杆，且**只向下**：要求 50 token 的调用方继续得到 50；要求 4000（或干脆没写上界、把供应商默认值留在场上）的调用方被拉到 cap。抬高一个请求的上界等于网关花调用方没要求花的钱。
- **为什么改写 `max_completion_tokens` 而不并列补 `max_tokens`（源码注释）**：当 body 用的是 `max_completion_tokens`，就改写那个字段；再并排加一个 `max_tokens` 会送出一个被两个不同供应商做两种解释的请求。
- **显式 `null` 与非正数的读法（源码注释）**：显式 null、以及不是正数的上限，都被当作"调用方没点名限额"，因此**同样被 cap** —— 这与 `completionCeiling` 给预留定尺时的读法一致，从而"预留的量"与"请求的量"保持是同一个请求。
- **被谁调用**：降级阶梯（本册范围外的调用点）。

### routeLabel (internal/gateway/request.go)

`func routeLabel(path string) string`

- **作用**：把请求路径归一化成低基数指标标签。`p := strings.TrimSuffix(path, "/")` 后按 `switch` 判断：`p == "" || p == "/"` → `"/"`；后缀 `/chat/completions` → `"/v1/chat/completions"`；`/completions` → `"/v1/completions"`；`/embeddings` → `"/v1/embeddings"`；`/models` → `"/v1/models"`；`/responses` → `"/v1/responses"`；其余 → `"other"`。
- **理由（源码注释）**：`// Paths carry identifiers, and using a raw path as a metric label is how a metrics backend falls over: one time series per conversation id. Anything containing a segment that is not a known endpoint collapses to "other".` 即路径带标识符，把原始路径当标签会让指标后端崩掉（每个会话 id 一条时间线）；含未知段的路径一律塌缩成 `"other"`。

---

## 15. 待续 / 交接给相邻两册

- `admitQuota`、`prepareCache`、`cacheAttempt`、`idempotencyBegin` 的闸门内部实现不在本册（`serve` 只写它们的调用契约与排序理由）。
- `buildRequest` 的 `budget` 如何落地成 `context.WithTimeout`、`cleanup` 释放了什么、`client` 从何而来，属后一分册。
- `relayStream` / `copyWhole` 如何回填 `rec.usage` 与 `metrics.Outcome*`，以及 `passThroughError` / `writeRawError` 的字节级行为，属后一分册。
- 本册出现的 `HeaderUpstreamName`、`HeaderAttempt`、`HeaderRequestID`、`HeaderTried`、`HeaderSession`、`HeaderUpstream`、`HeaderCapabilities` 常量定义在别处，未在范围内核对。


## 16. 本册范围与阅读约定

本册覆盖「网关核心管线」第一册的第三部分：`internal/gateway/proxy.go` 的后段（从 `readBody` 起）、`internal/gateway/pricing.go`、`internal/gateway/headers.go`、`internal/gateway/sessionpath.go`、`internal/gateway/stream_helpers.go`。`Proxy`、`record`、`Usage`、`ServeHTTP`、`internal/gateway/errors.go` 归第一部分；`serve`、`plan`、`serveAttempts`、`attemptUpstream`、`internal/gateway/request.go` 归第二部分。

锚点统一写成 ``符号名 (相对路径)``，不写行号。标注「（源码注释的观点）」的句子是源码注释的意思转述，不是本册作者的推断。

## 17. buildRequest：出站请求的构造

### buildRequest (internal/gateway/proxy.go)

签名：`func (p *Proxy) buildRequest(r *http.Request, target *upstream.Target, body []byte, timeout time.Duration, rec *record) (*http.Request, *http.Client, context.CancelFunc, error)`
- 作用：把入站请求克隆成发往 `target` 的出站请求，连同应执行它的 `*http.Client` 与一个清理函数一起返回。源码注释的观点：`timeout` 是**本次尝试**的预算而非整条请求的预算——若每次尝试共享一个从请求起点算起的截止时间，第一个慢候选会吃掉全部预算，之后每个健康候选都会瞬间以 deadline 错误失败，把「试下一个后端」变成「放弃」，恰好与失败转移存在的理由相反；调用方按请求的**剩余时间**计算每次尝试的预算，客户端总等待时间仍有界。
- 读写状态：只读 `p.transportFor`、`p.client`；读 `rec.requestID`、`rec.traceparent`、`rec.tracestate`；**不修改 `rec` 的任何字段**。
- URL 拼接：`outURL = target.BaseURL + r.URL.Path`，`r.URL.RawQuery` 非空时再追加 `"?" + raw`（`upstream.Target.BaseURL` 的约定是不带尾斜杠）；逐段相加而非重新解析，路径保持入站形态、查询串原样透传。
- 头复制：先 `copyHeaders(req.Header, r.Header, true)`，即在请求方向丢掉逐跳头、`Connection` 点名的头、以及全部 `X-InferGate-*` 与 `Idempotency-Key`；随后 `req.Header.Del("Host")` 并把 `req.Host = ""`，让 `net/http` 按目标地址重算 Host，不把网关自己的 Host 带给供应商。
- 凭证选择：`target.APIKey != ""` 时 `req.Header.Set("Authorization", "Bearer "+target.APIKey)`；否则若 `r.Header.Get("Authorization") == ""` 就 `req.Header.Del("Authorization")`。源码注释的观点：配置好的后端 key 永远优先（网关才是凭证持有者），否则透传调用方的 Authorization——这正是无 key 的本地 vLLM 或运维自己的 key 不需要重新配置就能用的原因。
- 关联 id：`req.Header.Set("X-Request-Id", rec.requestID)`。源码注释的观点：这里必须用 record 里的 id、不能第二次调用 `requestID(r)`——调用方没带 id 时那个函数会**新造**一个，调两次得到两个不同 id，上游看到的 id 与网关自己的日志行从不对应。
- trace 传播：`rec.traceparent != ""` 时设 `tracing.HeaderTraceparent`，且 `rec.tracestate != ""` 时另设 `tracing.HeaderTracestate`。源码注释的观点：即使本地没在记录 trace 也照传，免得被追踪的调用方在这一跳断链。
- transport 三级回退：`transport := p.transportFor(target)`；为 nil 退到 `target.Transport`；仍为 nil 退到 `http.DefaultTransport`。
- client 构造：`client := *p.client`（值拷贝；`p.client` 由 `New` 建出且注释明说**不设 Timeout**，因为流式 body 不能被 client 级超时截断），然后把 `client.Transport` 换成上面选中的 transport。源码注释的观点：每请求一个 client 是「每后端 transport」的代价，它很小、除自身外不额外分配，底下的 transport（也就是连接池）跨请求共享。
- 错误路径：只有 `http.NewRequestWithContext` 会失败，错误原文来自 `net/http`，本函数不自造字符串；失败分支先 `close(stopWatch)` 再 `release()`，然后返回 `(nil, nil, func() {}, err)`——注意第三个返回值是**空函数而不是 nil**，调用方可以无条件 `defer cleanup()`。
- 并发/锁：无锁。函数内启动一个 watcher goroutine，用 `stopWatch` channel 保证它一定退出。
- 被谁调用：`attemptUpstream (internal/gateway/proxy.go)`（`defer cleanup()` 后 `client.Do(req)`）；`probeOne (internal/gateway/capabilities.go)`（用 `httptest.NewRequest` 造合成请求，随后把请求换成 `req.WithContext(ctx)`，同样 `defer cleanup()`）。源码注释的观点：能力探测借道 `buildRequest`，是因为它是唯一知道「一个后端怎么寻址、怎么带凭证」的地方，在这里重写一遍就会让探测走一条与客户不同的代码路径。
### 为什么不能直接用调用方的 context（源码注释的观点）

- Go 的 `http.Server` 会在三种情况下取消入站 `Request.Context()`：客户端断开、handler 返回、读取请求体失败。
- 第一种正是想要的；第二种发生在工作做完之后；第三种**对流式网关是有敌意的**——客户端把 body 发一半然后干等，会让一次已经在跑的生成被中断。
- 所以出站 context 从 `context.Background()` 派生、带网关自己的 deadline，另用一个 watcher 显式传播客户端断开。结果是「一次上游交换允许跑多久」只有一个决定点，超时也因此可观测、可调。
### cleanup 如何保证没有 goroutine 与 timer 泄漏

- timer：`timeout > 0` 时 `base, deadlineCancel = context.WithTimeout(base, timeout)`；否则 `deadlineCancel` 先被赋成空函数 `context.CancelFunc(func() {})`。`release := func() { cancel(); deadlineCancel() }` 同时释放两者，于是 `WithTimeout` 的 timer 绝不会在请求余生里泄漏。
- goroutine：watcher 体是 `select { case <-clientCtx.Done(): cancel(); case <-stopWatch: }`（`clientCtx := r.Context()`），两个分支都会返回，所以只要 `stopWatch` 被关闭，它必然退出，没有任何 goroutine 活得比它的请求更久。
- 正常出口只有返回给调用方的 `cleanup := func() { close(stopWatch); release() }`：先关 channel 放走 watcher，再释放 context 与 timer；`attemptUpstream` 与 `probeOne` 都以 `defer cleanup()` 兜住。
- 失败出口在 `http.NewRequestWithContext` 错误分支里手写同样的两步。两条路径互斥，`stopWatch` 恰好关闭一次，不存在重复 close 的 panic。

## 18. 请求体读取与响应中继

### readBody (internal/gateway/proxy.go)

签名：`func (p *Proxy) readBody(r *http.Request) (body []byte, tooLarge bool, err error)`
- 作用：把请求体读到配置上限。`r.Body == nil` → `(nil, false, nil)`；否则 `body, err = io.ReadAll(io.LimitReader(r.Body, p.maxBody+1))`。
- 错误路径：`io.ReadAll` 的错误原样返回 `(nil, false, err)`，不包装；`int64(len(body)) > p.maxBody` → `(nil, true, nil)`，超限的 body 被丢弃。源码注释的观点：上限靠「多读 1 字节」而不是信任 `Content-Length`——chunked 请求根本没有长度，而一个说谎的 `Content-Length` 不该能让网关任意分配内存。
- 并发/锁：无锁，只读 `p.maxBody`（由 `New` 归一化，`MaxBodyBytes <= 0` 时为 `8 << 20`）；关闭 `r.Body` 是调用方的责任。本函数不写 `w`、不写 `rec`。
- 被谁调用：`serve (internal/gateway/proxy.go)`，把三类返回值映射为 400 与 413。
### passThroughError (internal/gateway/proxy.go)

签名：`func (p *Proxy) passThroughError(w http.ResponseWriter, resp *http.Response, rec *record)`
- 作用：把上游 4xx/5xx 的响应体原样转发给客户端。读取用 `io.ReadAll(io.LimitReader(resp.Body, p.maxBody))`——注意这里**没有 +1**，与 `copyWhole` 不同，超长上游错误体会被截断在上限处。
- 成功路径：`rec.respBytes = int64(len(body))`；`rec.reason = "upstream status " + resp.Status`；`writeRawError(w, resp.StatusCode, resp.Header.Get("Content-Type"), body)`。
- 错误路径：读失败时 `rec.outcome = metrics.OutcomeUpstreamErr`；`rec.reason = "read upstream error body"`；`writeError(w, http.StatusBadGateway, TypeBadGateway, "could not read upstream error body")`。
- 源码注释的观点：它**不**判断 outcome——调用方已经区分了可重试的 5xx 与属于调用方错误的 4xx、并映射到 metrics 分类，让它再猜一次正是两套分类会漂移的由来。
- 并发/锁：无锁。被谁调用：`attemptUpstream (internal/gateway/proxy.go)` 的不可重试 4xx 透传分支。
### copyWhole (internal/gateway/proxy.go)

签名：`func (p *Proxy) copyWhole(w http.ResponseWriter, r *http.Request, resp *http.Response, rec *record) metrics.Outcome`
- 作用：中继非 SSE 的响应体，并返回这次请求的 metrics outcome。
- JSON 分支（条件 `isJSON(resp.Header.Get("Content-Type"))`）：`io.ReadAll(io.LimitReader(resp.Body, p.maxBody+1))` 一次读完。读失败时 `rec.respBytes = int64(len(body))`、`rec.reason = "read upstream body: " + err.Error()`，若 `r.Context().Err() != nil` 返回 `metrics.OutcomeCanceled`，否则返回 `metrics.OutcomeUpstreamErr`。成功后依次：`rec.respBytes = int64(len(body))`、`p.accountJSON(rec, body)`、`w.WriteHeader(resp.StatusCode)`、`w.Write(body)`；写失败时 `rec.reason = "write to client: " + werr.Error()` 并返回 `metrics.OutcomeCanceled`；随后仅在 `resp.StatusCode == http.StatusOK` 时执行 `p.storeCache(r.Context(), rec, body, resp.Header.Get("Content-Type"))`；最后返回 `metrics.OutcomeSuccess`。
- 非 JSON 分支：先 `w.WriteHeader(resp.StatusCode)`，再 `n, err := io.Copy(w, resp.Body)`，`rec.respBytes = n`；出错时 `rec.reason = "relay body: " + err.Error()`，`r.Context().Err() != nil` → `metrics.OutcomeCanceled`，否则 `metrics.OutcomeUpstreamErr`；成功返回 `metrics.OutcomeSuccess`。源码注释的观点：文件下载、音频流这类 body 直接透传——网关的职责是中继而非重写，为了给它们保住 failover 去缓冲一个无界下载反而更糟。
- 读写状态：读 `p.maxBody`、`p.cache`（经 `storeCache`）；写 `rec.respBytes`、`rec.reason`、`rec.usage`/`rec.model`（经 `accountJSON`）。状态码一旦写出即不可撤，这正是它把状态码写在读完之后的原因。
- 并发/锁：无锁；`w`、`rec`、`resp.Body` 都是单请求私有的，body 的关闭由 `attemptUpstream` 负责。
- 被谁调用：`attemptUpstream (internal/gateway/proxy.go)` 的非流式成功分支，以 `rec.outcome = p.copyWhole(...)` 的形式。
### 为什么先读完 JSON 再写状态码（源码注释的观点）

- 这个顺序本身就是 failover 的前提：上游若死在 body 中间，客户端此刻还什么都没收到，重试就能给出一个干净的答案，而不是把截断的响应藏在 200 后面。
- 代价是一次 JSON 补全要在内存里缓冲，但由 `maxBody` 兜住——超过它的补全本来也无法计量。
- 因此 JSON 路径上「写状态码」严格晚于「读完整 body」，读失败也因此能把失败报成可重试（`metrics.OutcomeUpstreamErr`），让调用方去失败转移；非 JSON 路径刻意反过来（先状态码后 copy），因为它用放弃 failover 换无界中继。
### 为什么只有 200 才存缓存

- 源码注释的观点（M2）：缓存写入发生在答案已经送达**之后**，于是一个慢缓存（一次 Redis 往返、一次 embedding 调用）或一个挂掉的 Redis 不能让调用方付出任何代价——答案已经在线上，写失败唯一影响的是「下一个相同请求贵不贵」。
- 只存 `http.StatusOK`：缓存条目要能被后来的相同问题重放，只有成功答案值得；4xx/5xx 是这次上游或这次调用方的偶发结果，缓存它等于把一次瞬时失败固化成后续请求的答案。非 200 的响应体已在 `passThroughError` 或流式路径处置完毕，不进入缓存语义。
### accountJSON (internal/gateway/proxy.go)

签名：`func (p *Proxy) accountJSON(rec *record, body []byte)`
- 作用：从非流式补全的 body 里抽取 token 用量。`usage, model, ok := usageFromJSON(body)`；`!ok` 直接返回；否则 `rec.usage = usage`，并在 `model != ""` 时 `rec.model = model`。
- 源码注释的观点：**提供商回报的模型名对计价是权威的**——一个别名可能解析到价格不同的快照，所以用供应商回报名覆盖请求名。
- 无返回值、无错误路径（解析不出用量就静默不动 `rec`）、无锁；被谁调用：`copyWhole (internal/gateway/proxy.go)`，写状态码之前。
### isJSON (internal/gateway/proxy.go)

签名：`func isJSON(contentType string) bool`
- `""` → false；`mime.ParseMediaType` 失败 → 退化为 `strings.Contains(strings.ToLower(contentType), "json")`；正常解析时 `mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")`（前者覆盖 OpenAI 的 `application/json; charset=utf-8`，后者覆盖 `application/vnd.x+json` 一类结构化后缀）。
- 无锁、无副作用；被谁调用：`copyWhole (internal/gateway/proxy.go)`，用来决定走缓冲路还是直通路。
### isEventStream (internal/gateway/proxy.go)

签名：`func isEventStream(contentType string) bool`
- 同构判断：`""` → false；解析失败 → `strings.Contains(strings.ToLower(contentType), "event-stream")`；正常解析时仅 `mediaType == "text/event-stream"`。
- 无锁、无副作用；被谁调用：`attemptUpstream (internal/gateway/proxy.go)`（只有 `rec.stream && isEventStream(...)` 才走流式中继）与 `internal/gateway/cachepath.go`（缓存命中回放时判断 SSE）。
### usageFromJSON (internal/gateway/proxy.go)

签名：`func usageFromJSON(body []byte) (Usage, string, bool)`
- 作用：把 OpenAI 兼容响应体里的 `model` 与 `usage` 解出来。匿名结构体是顶层 `model` 加 `usage` 下的 `prompt_tokens`、`completion_tokens`、`total_tokens`、`prompt_tokens_details.cached_tokens`。
- 分支：`json.Unmarshal` 失败**或** `env.Usage == nil` → `(Usage{}, "", false)`；否则返回 `Usage{Prompt: env.Usage.PromptTokens, Completion: env.Usage.CompletionTokens}`，在 `env.Usage.PromptTokensDetails != nil` 时补 `u.Cached`，第二返回值为 `env.Model`、第三为 true。`TotalTokens` 被解码但**从不使用**——用量以 prompt/completion 两者为准，避免与供应商的合计口径打架。
- 源码注释的观点：`usageFromJSON adapts sse.UsageFromResponse into this package's record type`，即把流式包（`internal/sse`）的用量提取适配成本包的 `Usage` 类型，使 `record` 不必 import 流式包。
- 无锁、无副作用；被谁调用：`accountJSON (internal/gateway/proxy.go)`、`internal/gateway/stream_test.go`。
### resolveModelName (internal/gateway/proxy.go)

签名：`func (p *Proxy) resolveModelName(target *upstream.Target, requested string) (string, bool)`
- 作用：决定实际发给 `target` 的模型名；第二个返回值 `changed` 表示是否发生改写。
- 分支顺序：`patterns := target.ModelPatterns()`；`target.IsCatchAll() && len(patterns) == 1` → `(requested, false)`；`patterns` 中存在 `strings.EqualFold(pattern, requested)` → `(requested, false)`；`requested == ""` → `(requested, false)`；否则返回第一个既非 `"/"` 也非 `""` 的 pattern 并置 `changed = true`；都不满足 → `(requested, false)`。
- 源码注释的观点：返回值契约是 `Returns changed=false when the client's own model name is already acceptable`；模型列表恰好是 `{"/"}` 的后端是直通型（vLLM、Ollama），改写会把它弄坏，所以调用方的名字逐字保留；没有精确匹配时挑后端第一个具体模型让请求可服务，而**完全没带模型名**的请求被原样放行给上游拒绝，因为供应商的错误比网关的猜测更清楚。
- 只读 `target`、不改 `rec`、无锁；被谁调用：`plan (internal/gateway/proxy.go)`（改写的 `changed` 在调用处被丢弃、只取名字）。
### forwardResponseHeaders (internal/gateway/proxy.go)

签名：`func forwardResponseHeaders(dst, src http.Header)`
- 作用：把上游响应头复制给客户端。先 `copyHeaders(dst, src, false)`——`dropGatewayHeaders` 为 `false`，因为响应方向的控制头是网关自己随后写进去的，不从上游抄。
- 随后 `dst.Del("Content-Length")`、`dst.Del("Content-Encoding")`、`dst.Del("Transfer-Encoding")`。源码注释的观点：body 由 Go 的 server 重新分帧，所以上游的分帧头必须去掉——一个过期的 `Content-Length` 会截断流式响应，而转发 `Content-Encoding` 会让已经解压过的 body 被二次解码。
- 只写 `dst`、不碰 `rec`、无锁、无错误路径；被谁调用：`attemptUpstream` 与 `serveAttempts (internal/gateway/proxy.go)`（幂等重放响应、上游错误响应、正常响应三类投递之前）。
### requestID (internal/gateway/proxy.go)

签名：`func requestID(r *http.Request) string`
- 作用：取这次请求的关联 id。`strings.TrimSpace(r.Header.Get(HeaderRequestID))` 非空则返回它，否则返回 `newRequestID()`（定义在 `internal/gateway/stream.go`，128 位随机 id）。
- 源码注释的观点：不自己铸造 id 的网关，无法把自己的日志与客户端看到的失败对上。
- 重要契约：调用方没发 id 时它**每次都可能给出新值**，所以只能调用一次；`record.requestID` 在 `ServeHTTP` 里就固化了，`buildRequest` 因此必须用 `rec.requestID` 而不是再调它。
- 只读请求头、无锁，但**有副作用**（生成随机数）；被谁调用：`ServeHTTP (internal/gateway/proxy.go)` 构造 `record` 时。
### isProxiablePath (internal/gateway/proxy.go)

签名：`func isProxiablePath(path string) bool`
- 逻辑：`p := strings.TrimSuffix(path, "/")`；`p == ""` 或 `p == "/"` → true；否则只要以 `/chat/completions`、`/completions`、`/embeddings`、`/models`、`/responses` 之一结尾 → true；其余 false。它用 `HasSuffix`，所以 `/v1/chat/completions` 与任何前缀拼法都通过。
- 源码注释的观点：把任意路径转发到付费 API 的反向代理就是一个开放中继——能到达网关的攻击者就能到达后端暴露的每个端点，而枚举受支持的接口面是最便宜有效的控制。
- 无锁；被谁调用：`serve (internal/gateway/proxy.go)`（仅当 `!isCompletionPath(...)` 时才检查，不通过即 404）。
### itoa (internal/gateway/proxy.go)

签名：`func itoa(n int) string { return strconv.Itoa(n) }`
- 作用：`strconv.Itoa` 的本地别名；源码注释的观点是「免得在本文件里为 import 来回折腾」。
- 无锁、无副作用；被谁调用：`serveAttempts` 与 `attemptUpstream (internal/gateway/proxy.go)`（拼错误消息、写 `HeaderAttempt`）。

## 19. headers.go：逐跳头与网关控制头

### hopByHopHeaders (internal/gateway/headers.go)

声明：`var hopByHopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}`
- 作用：入站请求与上游响应**两个方向**都要剥掉的逐跳头，依据 RFC 7230 第 6.1 节。
- 逐个为什么是逐跳头：`Connection` 与 `Proxy-Connection`（后者的非标准代理变体）是这一跳的连接管理指令，语义只在相邻两跳之间成立；`Keep-Alive` 描述发送方的连接复用意愿，转发会让下一跳按别人的参数行事；`Proxy-Authenticate`/`Proxy-Authorization` 是**代理到代理**的凭证与挑战，属于中间跳而非源/端之间，转发会泄漏或误用代理凭证；`Te` 与 `Trailer` 声明本条消息允许的传输编码与尾随字段，换了收发双方就不再成立；`Transfer-Encoding` 描述上游那一段的帧格式，一经重新分帧即失效（这也是 `forwardResponseHeaders` 另外删它的原因）；`Upgrade` 是对**当前连接**的协议升级协商，跟着消息走会让客户端以为它和上游之间有一条可升级的连接。
- 源码注释的观点：转发它们**不是风格问题**——`Connection` 携带「发送方这一跳承诺消费掉」的头名列表，盲目复制会让客户端把**任意**头（**包括 `Authorization`**）标成逐跳的，从而让网关在下一跳剥掉或重复它。
- 包级不可变切片；被谁使用：`removeHopByHop`、`isHopByHop`。
### removeHopByHop (internal/gateway/headers.go)

签名：`func removeHopByHop(h http.Header)`
- 作用：从 `h` 上删除逐跳头，**包括 `Connection` 字段自己点名的那些**。实现是先 `for _, name := range connectionListed(h) { h.Del(name) }`，再对 `hopByHopHeaders` 逐个 `h.Del(name)`——**顺序有含义**：必须在删掉 `Connection` 本身之前先读它声明的名字，否则那份名单就丢了。
- 错误路径：无；`http.Header.Del` 对不存在的键是空操作且会做规范化。无锁，只操作传入的 `h`。
- 现状：全仓库检索只命中定义处，**当前没有调用点**——入站方向的同一效果由 `copyHeaders` 的 `isHopByHop` 与 `skip` 集合完成。它作为「可直接作用于 `http.Header`」的显式工具保留。
### connectionListed (internal/gateway/headers.go)

签名：`func connectionListed(h http.Header) []string`
- 作用：返回 `Connection` 字段点名的头名。遍历 `h.Values("Connection")`，每个值按 `,` 切分，逐段 `textproto.TrimString`，非空才 `append(names, name)`；保持出现顺序、不去重，可返回 nil。
- 源码注释的观点：这是 RFC 7230 第 6.1 节里「微妙的那一半」——`Connection` 不只自己逐跳，它还**为本条消息声明别的头**也逐跳；只剥静态名单的网关会允许客户端发 `Connection: Authorization`，让一个任意头在下一跳被不一致地处理。这正是「`Connection` 声明了别的头时必须特殊处理」的答案：那份名单是每消息动态的，静态表看不见它。
- 注意大小写：本函数返回**原始字符串**、不做规范化；`http.Header.Del` 会自己规范化，而 `copyHeaders` 显式 `http.CanonicalHeaderKey(name)` 后再比对。
- 无锁、不改入参；被谁调用：`removeHopByHop`、`copyHeaders`。
### copyHeaders (internal/gateway/headers.go)

签名：`func copyHeaders(dst, src http.Header, dropGatewayHeaders bool)`
- 作用：把能活过下一跳的头从 `src` 复制到 `dst`。先建 `skip := make(map[string]struct{})`，把 `connectionListed(src)` 的每一项以 `http.CanonicalHeaderKey(name)` 为键放进集合（`src` 的键已是规范形式，而 `Connection` 里写的是任意大小写）；再遍历 `src`，依次跳过 `isHopByHop(name)` 为真者、键在 `skip` 里者、以及 `dropGatewayHeaders && isGatewayHeader(name)` 为真者；其余用 `dst[name] = append([]string(nil), values...)` 整片替换（**切片拷贝**，不与 `src` 共享底层数组）。
- 源码注释的观点：值原样保留，好让提供商的响应字节级忠实地抵达客户端（速率限制头、请求 id、重试提示、厂商扩展）。
- 调用方向的两处差别：入站→出站时 `dropGatewayHeaders=true`（`buildRequest`），因为控制头必须止步于此；出站→客户端时 `false`（`forwardResponseHeaders`），因为响应方向的控制头由网关自己随后设置。
- 无锁、无错误路径；被谁调用：`buildRequest`、`forwardResponseHeaders`、`internal/gateway/headers_drop_test.go`。
### isHopByHop (internal/gateway/headers.go)

签名：`func isHopByHop(name string) bool`
- 对 `hopByHopHeaders` 逐项 `strings.EqualFold(name, h)`，命中返回 true，否则 false；大小写不敏感是必要的，因此 `connection`、`TE`、`TRANSFER-ENCODING` 都命中。
- 无锁；被谁调用：`copyHeaders`。
### isGatewayHeader (internal/gateway/headers.go)

签名：`func isGatewayHeader(name string) bool`
- 逻辑：`strings.EqualFold(name, HeaderIdempotencyKey)` → true；否则 `strings.HasPrefix(strings.ToLower(name), "x-infergate-")`。因此它覆盖**全部** `X-InferGate-*` 控制头（含由前缀任意拼出的那些）外加 `Idempotency-Key`。
- 源码注释的观点（第一半）：网关**会**对 `Idempotency-Key` 采取行动——认领这个键、一个请求去做事、之后每个同键请求都由存储应答，所以它是控制流量而非载荷，到此为止。
- 源码注释的观点（第二半，解释为什么不能上行）：一个在同名头上实现自家幂等的供应商会**跨租户**去重：本网关的两个租户若碰巧选了同一个键字符串（共享模板里的 UUID、`"retry-1"`、一个日期），第二个租户会拿到**第一个租户的答案**——键在网关里是按租户分作用域的，到了别处会静默地不再分作用域。
- 无锁；被谁调用：`copyHeaders`。
### 网关控制头常量 (internal/gateway/headers.go)

全部定义在一个 `const` 块里，命名规律是 `Header` + 语义名。源码注释的观点：它们由网关**消费**、绝不到达供应商，既为保持供应商 API 干净，也为防止客户端在一个稍后会在别处被重放的请求上选择后端。

请求方向：

- `HeaderUpstream = "X-InferGate-Upstream"`：把请求钉到指定名字的后端；由 `plan` 读取。
- `HeaderRequestID = "X-InferGate-Request-Id"`：调用方提供的关联 id，会被回显；由 `requestID` 读取，响应侧被设为 `rec.requestID`。
- `HeaderCapabilities = "X-InferGate-Capabilities"`：逗号分隔的能力标签（`"tools"`、`"json"`、`"vision"`），路由器只提供声明了全部这些能力的后端。源码注释的观点：它是**请求头而不是从 body 推断**的，因为同一个模型由两个后端服务时支持面可能不同，而只有调用方知道下游 agent 是否真的需要工具调用；从非空 `tools` 数组推断 `"tools"` 是显而易见的猜测，也是错的——agent 框架无论模型会不会用都会发同样的工具 schema。
- `HeaderTenant = "X-InferGate-Tenant"`：隔离命名空间，让一个调用方的缓存答案不落到另一个调用方手里；缺失时网关从凭证派生（哈希，从不存储）或退化为 `"anonymous"`。
- `HeaderSession = "X-InferGate-Session"`（**仅请求方向**）：租户内一个逻辑会话的名字，按会话的 token 预算记在它头上。源码注释的观点：能控制自己 session id 的客户端可以开一个新预算，所以按会话的维度是**协作调用方之间的公平性控制，永远不是安全边界**；真正界定一个撒谎调用方的是每日与每分钟两个维度——没有任何头能重置它们。
- `HeaderCache = "X-InferGate-Cache"`（**双向，同一个头名**）：请求方向取值 `cacheDirectiveBypass = "bypass"`（不要用缓存应答）与 `cacheDirectiveRefresh = "refresh"`（不要用已存答案但要替换它）；两者**仍然会存**新答案，除非请求本身不可缓存——源码注释的观点：一个同时拒绝存储的 bypass 会让「强制刷新」变成一种逐请求永久禁用某作用域缓存的方式。响应方向取值见下。
- `HeaderIdempotencyKey = "Idempotency-Key"`（**仅请求方向，绝不上行**）：调用方给一次逻辑操作起的名字，沿用 OpenAI 与 Stripe SDK 已有的拼写。源码注释的观点：这个键是调用方与 InferGate 之间的契约，让供应商对它做自家去重会把第二个请求变成供应商作用域的重放或冲突，而调用方两样都没要。

响应方向：

- `HeaderUpstreamName = "X-InferGate-Upstream-Name"`：报告是哪个后端服务了这次响应；重放路径上它的值是 `idempotencyUpstream = "replay"`，缓存命中路径上是 `cacheStatusUpstream = "cache"`，因为真正产出答案的后端并没有服务这次请求，把它算作服务方会让按后端的总计宣称一批从未到达它的流量。
- `HeaderAttempt = "X-InferGate-Attempt"`：报告做了多少次上游尝试（M1 重试）。
- `HeaderTried = "X-InferGate-Tried"`：按顺序列出为本次请求尝试过的后端。源码注释的观点：它是 response-only 的，看到两个名字的客户端不需要读网关日志就知道发生过失败转移。
- `HeaderCache` 的响应取值：`cacheStatusHitExact = "hit-exact"`、`cacheStatusHitSemantic = "hit-semantic"`、`cacheStatusMiss = "miss"`、`cacheStatusSkip = "skip"`（请求本身不可缓存）、`cacheStatusDisabled = "disabled"`、`cacheStatusError = "error"`，以及指令被尊重时回显的 `"bypass"`/`"refresh"`。源码注释的观点：同一个头在两个方向都可读是刻意的，好让「我要了什么」与「我得到了什么」在日志或 curl 记录里是同一个字符串。
- `HeaderCacheAge = "X-InferGate-Cache-Age"`：被服务的缓存条目年龄，整数毫秒。源码注释的观点：带年龄的答案与答案本身是两回事，所以命中时总是发送这个数字，而不是留给调用方从 body 反推。
- `HeaderQuota = "X-InferGate-Quota"`：准入判决 `"allow"`/`"degrade"`/`"reject"`。源码注释的观点：像 `HeaderCache` 一样刻意只在一个方向有意义，而且**每个被治理的请求都会**发送它，而不只在拒绝时发送，因为剩余预算不可见的限额无法被规划。
- `HeaderQuotaReason = "X-InferGate-Quota-Reason"`：决定判决的维度名（`"within-budget"`、`"tokens_per_day"`、`"requests_per_minute"` 等）。
- `HeaderQuotaLimit = "X-InferGate-Quota-Limit"` 与 `HeaderQuotaUsed = "X-InferGate-Quota-Used"`：适用限额与该窗口内租户已消耗量。源码注释的观点：没有点名任何单一维度时二者一并省略，从而调用方永远不会把一个 `0` 读成限额。
- `HeaderQuotaModel = "X-InferGate-Quota-Model"` 与 `HeaderQuotaMaxTokens = "X-InferGate-Quota-Max-Tokens"`：报告降级请求被改写成了什么，让调用方看得见答案来自更小的模型或更短的补全上限，而不是对着质量变化发懵。
- `HeaderIdempotentReplay = "X-InferGate-Idempotent-Replay"`：`idempotentTrue = "true"`（从幂等存储重放）/ `idempotentFalse = "false"`（本次请求真的做了工作）。源码注释的观点：它在带键请求上**总是**发送，因为超时后重试的调用方必须能分辨「我的第一次其实落地了」与「我的重试又生成了一个答案」。
- `HeaderIdempotentOrigin = "X-InferGate-Idempotent-Origin"`：最初产出被重放答案的那个请求 id。源码注释的观点：`HeaderRequestID` 继续报告**本次**请求，从而总有一个头与日志行匹配，而调用方同时仍拿得到那个真正做了事、因而持有 trace 的请求 id。
- `HeaderIdempotentUpstream = "X-InferGate-Idempotent-Upstream"`：最初产出被重放答案的后端，因为那条路径上 `HeaderUpstreamName` 报的是 `"replay"`。
- `HeaderIdempotentAge = "X-InferGate-Idempotent-Age"`：被重放答案的产出时间距今多久，整数毫秒，理由与 `HeaderCacheAge` 相同。
- `HeaderIdempotentStore = "X-InferGate-Idempotent-Store"`：报告一个带键请求为什么**不会**可重放，且仅限在答案存在之前就能知道的那一种情况——`idempotentSkip = "skip"`，用于「这条路径没有可重放的响应」。
### 为什么 Idempotent-Store 只会报 skip（源码注释的观点）

- 存储本身的结局不是头，而是设计决定而非遗漏：`idempotentStored = "stored"` 与 `idempotentOversize = "oversize"` 表示一个完成的响应是否装得进存储，而这**只有 body 发完之后才知道**，那一刻设置的头会被 `net/http` 直接丢在地上。
- 为此发明一个 trailer 会迫使每个带键响应都改用分块传输，对「调用方看到什么」的改动比这点信息更贵。
- 它改记在请求日志与根 span 上——那正是运维者诊断「我的重试为什么跑了两次」时已经在看的地方。于是在**发送响应之前**，`X-InferGate-Idempotent-Store` 只可能有 `skip` 这一个值。

## 20. pricing.go：定价表与成本

### pricingUnitTokens (internal/gateway/pricing.go)

声明：`const pricingUnitTokens = 1_000_000`
- 价目表的分母。源码注释的观点：提供商按**每百万 token** 报价，沿用这个约定意味着 `infergate.yaml` 里的数字可以从供应商定价页直接粘贴，没有任何单位换算可写错。
- 唯一使用者：`(*PriceBook).CostUSD`。
### PriceBook (internal/gateway/pricing.go)

声明：`type PriceBook struct { defaultPrice config.ModelPrice; models map[string]config.ModelPrice }`
- 作用：按配置的每模型价格把 token 数换算成美元。两个字段都不导出：`defaultPrice` 是未列出模型的价格，`models` 是**小写、去空白**后的模型名到价格的映射；`config.ModelPrice` 的 `In` 与 `Out` 各是「每 100 万 token 的 USD」。
- 源码注释的观点：构造后只读，所以请求路径上**不需要任何锁**。
- 持有者：`Proxy.pricing (internal/gateway/proxy.go)` 与 `internal/server` 的启动装配。
### NewPriceBook (internal/gateway/pricing.go)

签名：`func NewPriceBook(pc config.PricingConfig) *PriceBook`
- 作用：`models := make(map[string]config.ModelPrice, len(pc.Models))`，对 `pc.Models` 每项 `models[strings.ToLower(strings.TrimSpace(name))] = price`，返回 `&PriceBook{defaultPrice: pc.Default, models: models}`。
- 源码注释的观点：键要小写化，是因为提供商回报模型名时大小写不一致，一次大小写不匹配会**静默**落到默认价并把开销报错。
- 无错误路径、无锁；被谁调用：`internal/server/server.go` 的启动装配、`internal/gateway/pricing_test.go`、`internal/gateway/proxy_test.go`、`internal/gateway/failover_test.go`、`internal/gateway/tracing_test.go`、`cmd/verify-m4/e2e.go`。
### (*PriceBook) Price (internal/gateway/pricing.go)

签名：`func (p *PriceBook) Price(model string) (config.ModelPrice, bool)`
- 作用：返回模型的价格条目以及它是否是一个**显式**匹配。`p == nil` → `(config.ModelPrice{}, false)`（nil 接收者安全）；`key := strings.ToLower(strings.TrimSpace(model))`；若 `strings.LastIndex(key, "/")` 找到前缀且其后还有字符，先按去掉前缀的后半段查表（`"openai/gpt-4o"` → `"gpt-4o"`）；否则按完整 `key` 查；都没有则 `(p.defaultPrice, false)`。
- 源码注释的观点：剥前缀是因为一个指明后端的网关头不应改变适用哪一档价格；而第二个返回值为 false 表示吃的是默认价，未列出的模型正是「成本是一个猜测」的那一类，`Unpriced` 的存在就是让这件事在启动时说一次而不是每请求说一次。
- 并发/锁：无锁，纯读；被谁调用：`(*PriceBook).Unpriced`、`(*PriceBook).CostUSD`、`internal/server/server.go` 的启动报价日志、`cmd/verify-m4/e2e.go`。
### (*PriceBook) Unpriced (internal/gateway/pricing.go)

签名：`func (p *PriceBook) Unpriced(models []string) []string`
- 作用：从给定列表里挑出表中无显式条目的模型，**保持给定顺序**，去掉重复项与 `"/"` 这种 catch-all。空串与 `"/"` 直接跳过；`p.Price(m)` 第二返回值为真也跳过；否则以 `strings.ToLower(strings.TrimSpace(m))` 为键去重后 `append(out, m)`（输出保留**原始拼写**）。全部有价时返回 nil 切片。
- 源码注释的观点：价格的两位消费者对未列出模型的含义意见不一致——路由器把它排到最后，因此一条被遗忘的价格行绝不会让某个后端看起来免费因而被偏好（见 `internal/router` 的 cost 策略）；而 `CostUSD` 没有这道护栏，同一条被遗忘的行会让该模型的流量按默认价计费，在一个从未设置默认价的配置里那就是 `$0`，于是对成本预算完全不可见。启动时报一次这个列表，是第二半事实唯一可以说出口而不把配置缺口变成每请求日志噪声的地方。
- 去重的理由（源码注释）：大小写与前后空白不构成第二个模型——`Price` 会折叠两者，因此只在这两点上不同的重复项否则会被报两次。
- 无错误路径、无锁；被谁调用：`internal/server/server.go` 的启动路径（把未定价模型报一次）。
### (*PriceBook) CostUSD (internal/gateway/pricing.go)

签名：`func (p *PriceBook) CostUSD(model string, prompt, completion int) float64`
- 作用：`price, _ := p.Price(model)`；返回 `(float64(prompt)*price.In + float64(completion)*price.Out) / pricingUnitTokens`。它**忽略** `Price` 的第二返回值，所以未列出的模型静默按默认价计。
- 源码注释的观点：**缓存命中的 prompt token 在这里也按输入价计费**；提供折扣的提供商会单独上报折扣，而对预算闸门来说略微高估是安全方向——它会在真实支出超限**之前**先触发预算。签名里没有 `cached` 参数，正是「按输入价计费」的机械原因。
- 无错误路径、无锁；被谁调用：`recordSession (internal/gateway/sessionpath.go)`、`internal/gateway/quotapath.go`（预留与结算两处）、`ServeHTTP (internal/gateway/proxy.go)` 的成本日志字段、`cmd/verify-m3/checks.go`、`cmd/verify-m4/e2e.go`。

## 21. sessionpath.go：会话落账

### recordSession (internal/gateway/sessionpath.go)

签名：`func (p *Proxy) recordSession(r *http.Request, rec *record)`
- 作用：把本次请求追加到按会话的账本。早退条件是 `if p.sessions == nil || !isCompletionPath(rec.path) { return }`（`isCompletionPath` 定义在 `internal/gateway/request.go`）。
- 随后构造 `sessions.Request{At: rec.start, RequestID: rec.requestID, Route: rec.route, Upstream: rec.upstream, Model: rec.model, RequestedModel: rec.explicitModel, Status: rec.status, Outcome: string(rec.outcome), Stream: rec.stream, PromptTokens: rec.usage.Prompt, CompletionTokens: rec.usage.Completion, CachedTokens: rec.usage.Cached, ElapsedMS: float64(time.Since(rec.start).Microseconds()) / 1000.0, Attempts: rec.attempts, Tried: append([]string(nil), rec.tried...), Cache: rec.cacheStatus, Reason: rec.reason, Replay: rec.servedFromReplay}`。
- 条件字段：`p.pricing != nil` 时补 `req.CostUSD = p.pricing.CostUSD(rec.model, rec.usage.Prompt, rec.usage.Completion)`；`rec.firstTokenSet` 时补 `req.FirstTokenMS = float64(rec.firstToken.Microseconds()) / 1000.0`，否则保持零值（0 即「没有首 token 时刻」）。
- 最后写入：`p.sessions.Record(rec.tenant, rec.session, req)`（`internal/sessions` 的 `func (l *Ledger) Record(tenant, id string, req Request)`）。
- 源码注释的观点：账本回答的是请求日志答不了的问题——一段 agent 对话总共花了多少、用了哪些模型、其中多少来自缓存或幂等重放、有多少请求失败；这些是关于**一段对话**的问题，而一段对话是散落在多个供应商上的若干请求，所以汇总必须在每个请求都会经过的地方做，而不是在任何单条供应商路径里。
- 源码注释的观点（为什么只记补全路由）：把 `/v1/models` 这类列表计入的预算与成本汇总会为零成本流量报开销，而 token 维度在没有 prompt 的请求上纯属虚构。
- 源码注释的观点（空的 session id）：它不是错误，账本把它单独计（`Stats.NoSessionID`）而不是编造一个只含一请求的伪会话，因为满是伪会话的账本会掩盖「调用方从没发过这个头」这一事实。
- 读写状态：只读 `rec`、`p.pricing`、`p.sessions`；写的是账本，**不回写 `rec`**。`Tried` 用 `append([]string(nil), ...)` 深拷贝，防止账本持有 `rec.tried` 的底层数组。
- 并发/锁：本函数无锁；`rec` 为单请求私有，账本自身的并发由 `sessions.Ledger` 负责。
- 被谁调用：`ServeHTTP (internal/gateway/proxy.go)` 的 deferred recorder（在 `rec.trace.finish(rec)` 之后、写日志行之前）。

## 22. stream_helpers.go

### marshalJSON (internal/gateway/stream_helpers.go)

签名：`func marshalJSON(v any) ([]byte, error)`
- 作用：不转义 HTML 地编码 `v`。`var buf bytes.Buffer`；`enc := json.NewEncoder(&buf)`；`enc.SetEscapeHTML(false)`；`enc.Encode(v)` 出错则 `return nil, err`；成功后 `out := buf.Bytes()`，若末尾是 `'\n'` 则 `out = out[:n-1]`，返回 `out, nil`。
- 源码注释的观点（为什么要关 HTML 转义）：`encoding/json` 默认转义 `<`、`>`、`&`；通过带内错误帧回显的错误消息是用户可见文本，转义它们会让响应与供应商对同一消息会发出的响应不同，从而破坏按错误字符串匹配的客户端。
- 源码注释的观点（为什么去掉换行）：`Encoder.Encode` 会附加换行，剥掉它才能把结果直接嵌进 SSE 的 `data` 字段。
- 错误路径：只有 `enc.Encode` 的编码错误（例如 `v` 里有不可编码的值），原样上抛。无锁、无共享状态、不修改 `v`。
- 被谁调用：`internal/gateway/stream.go` 的带内错误帧构造路径。
### isStreamEOF (internal/gateway/stream_helpers.go)

签名：`func isStreamEOF(err error) bool`
- 作用：`err == nil` 返回 true；否则 `errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)`。
- 源码注释的观点：body 因调用方读完而关闭、或传输层在最后一帧之后拆掉连接，都表现为 `io.EOF` 或 `net.ErrClosed`；把它们当失败会把一条健康流报成上游错误，并在 M1 触发一次对**已完成**请求的无意义重试。
- 无锁、无副作用；被谁调用：`internal/gateway/stream.go` 与 `internal/gateway/cachepath.go`。

## 23. 相邻册中出现的符号（本册不写）

- `Proxy`、`Options`、`New`、`record`、`Usage`、`ServeHTTP`、`internal/gateway/errors.go`：见本册前面的对应章节。
- `serve`、`plan`、`serveAttempts`、`attemptUpstream`、`observeAttempt`、`retryableStatus`、`backoff`、`attemptError`、`internal/gateway/request.go`：见本册中段的对应章节。
- `newRequestID` 与 `internal/gateway/stream.go`：02B 册；`storeCache` 与 `internal/gateway/cachepath.go`、`internal/gateway/quotapath.go`、`internal/gateway/idempotency.go`：02C 册。本册只引用它们的签名，未核对幂等/缓存头的**设置点**实现。


## 24. 本册地位：02A 册的收尾与总览

- 本册是「网关核心管线」（02A 册）的第四册，只做两件事：**给 02A 册立一张总览表与一条请求全链路串讲**，再**补掉前三册明确留空、且相邻册也没有认领的残留符号**。
- 走读体系的分册：`00` 配置与启动、`01` HTTP 服务器与中间件、**`02A` 网关核心管线**、`02B` 流式中继 / SSE / 上游注册、`02C` 治理路径（缓存 / 配额 / 幂等 / 能力 / 追踪）、`03` 路由与熔断、`04` 存储与嵌入、`05` 可观测性。
- 锚点统一写作「符号名（所属文件）」，文件一律写成仓库内真实存在的路径，**不含任何行号**。
- 本册写作时 `internal/gateway/` 下**没有测试文件以外的新文件**：非测试文件恰好 14 个，下面表格逐一给出去处。**残留扫描的结论是：前三册唯一未落地的顶层声明是 `Options` 与 `New(opts Options) *Proxy`（都在 `internal/gateway/proxy.go`）**；被反复提到的 `stream.go` 三个符号已经由 02B 册写完，本册只在「已由相邻册覆盖」里点名，不重复展开。

## 25. `internal/gateway/` 文件级地图

| 文件 | 一句话职责 | 由谁负责 |
| --- | --- | --- |
| `proxy.go` | `Proxy`/`record`/`Usage`/`Options`/`New` 与整条请求管线（`ServeHTTP`、`serve`、`serveAttempts`、`attemptUpstream`、`buildRequest`、`copyWhole`、`readBody`、`plan` 等） | 本册第 1 至 9 节（结构体、`ServeHTTP`、finalizer）、第 10 至 15 节（闸门顺序、候选链）、第 16 至 23 节（构造出站请求、中继、小工具）、**第 24 至 31 节（`Options`/`New`）** |
| `errors.go` | OpenAI 形状的错误信封 `ErrorBody`/`ErrorDetail`、`Type*` 常量组、`writeError`/`writeRawError`/`errorBodyBytes` | 本册第 1 至 9 节全文 |
| `request.go` | 入站体解析：`parsedRequest`、`inspectRequest`、`rewriteModel`/`rewriteMaxTokens`、路径谓词 `isCompletionPath`/`isStreamPath`/`routeLabel` | 本册第 10 至 15 节全文 |
| `headers.go` | 逐跳头（RFC 7230）与网关控制头常量、`copyHeaders`/`removeHopByHop`/`connectionListed`/`isHopByHop`/`isGatewayHeader` | 本册第 16 至 23 节全文 |
| `pricing.go` | 价目表 `PriceBook`：`Price`/`Unpriced`/`CostUSD` | 本册第 16 至 23 节全文 |
| `sessionpath.go` | 会话账本入口 `recordSession` | 本册第 16 至 23 节；第 02C 册第 8 节亦有一份 |
| `stream_helpers.go` | `marshalJSON`（关 HTML 转义）与 `isStreamEOF` | 本册第 22 节；第 02B 册第 6 节 |
| `stream.go` | SSE 中继：`heartbeatInterval`、`newRequestID`、`relayStream`、`firstVisibleToken`、`writeStreamError` | **第 02B 册第 5 节**（本册不重写） |
| `cachereq.go` | 缓存**身份**：`chatMessage`/`cacheFields`/`cacheIdentity`、`cacheIdentityFor`、`answerSignature`、`conversationPrompt`、`tenantFor`、`capabilitiesFor` | 第 02C 册第 2 节 |
| `cachepath.go` | 缓存**读写路径**：`prepareCache`、`cacheAttempt`、`serveCached`、`replayCachedStream`、`storeCache`、`cacheRequest` | 第 02C 册第 3 节 |
| `quotapath.go` | 配额准入：`QuotaGate`、`admitQuota`、`quotaEstimate`、`settleQuota`、`finishQuota`、`setQuotaHeaders` 等 | 第 02C 册第 4 节 |
| `idempotency.go` | 幂等闭环：`idempotencyBegin`、`serveReplay`、`idempotencyComplete`、`captureWriter` 系列 | 第 02C 册第 5 节 |
| `capabilities.go` | 能力报告与**主动探测**：`CapabilityReport`、`ProbeCapabilities`、`probeOne`、`classifyProbe` 等 | 第 02C 册第 6 节 |
| `tracing.go` | 每请求 trace：`Tracer`、`requestTrace`、`attemptSpan`、`firstTokenMS` | 第 02C 册第 7 节 |

- 读法提示：`proxy.go` 是唯一被四册切开讲的文件，切分点是**函数边界**而不是行号区间——看到 `serve` 的闸门序列去第 11 节，看到 `attemptUpstream` 里起流 / 不走流的分叉去第 13 节，看到 `New`/`Options` 去第 27 节。
- 另有一个 `internal/gateway` 之外的近邻：`internal/sse/writer.go` 的 `ContentType`、`Writer`、`MirrorSSEHeader`、`WriteUpstream` 等属于 02B 册，`relayStream` 全靠它们落地。

### 25.1 符号归属速查（只列名字，认领分册见上表）

下表的符号名是用 `Select-String '^(func |type |const |var |func \()'` 逐文件抽出的**机器清单**（本册写作时共 137 条顶层/方法声明），不是凭记忆写的：

| 文件 | 顶层符号（按文件内出现顺序） |
| --- | --- |
| `proxy.go` | `Proxy`、`Options`、`New`、`record`、`Usage`、`ServeHTTP`、`IsEmpty`、`serve`、`attemptError`、`attemptError.Error`、`attemptError.Unwrap`、`retryableStatus`、`serveAttempts`、`attemptUpstream`、`observeAttempt`、`backoff`、`plan`、`itoa`、`readBody`、`buildRequest`、`passThroughError`、`copyWhole`、`accountJSON`、`isJSON`、`isEventStream`、`usageFromJSON`、`resolveModelName`、`forwardResponseHeaders`、`requestID`、`isProxiablePath` |
| `errors.go` | `ErrorBody`、`ErrorDetail`、`Type*` 常量组、`writeError`、`writeRawError`、`errorBodyBytes` |
| `request.go` | `chatRequestFields`、`parsedRequest`、`inspectRequest`、`completionCeiling`、`rewriteModel`、`rewriteMaxTokens`、`isStreamPath`、`isCompletionPath`、`routeLabel` |
| `headers.go` | 逐跳头列表 `hopByHopHeaders`、四组控制头常量（含 `idempotencyUpstream = "replay"`、`cacheStatusUpstream = "cache"`）、`removeHopByHop`、`connectionListed`、`copyHeaders`、`isHopByHop`、`isGatewayHeader` |
| `pricing.go` | `pricingUnitTokens`、`PriceBook`、`NewPriceBook`、`Price`、`Unpriced`、`CostUSD` |
| `sessionpath.go` | `recordSession` |
| `stream_helpers.go` | `marshalJSON`、`isStreamEOF` |
| `stream.go` | `heartbeatInterval`、`newRequestID`、`relayStream`、`firstVisibleToken`、`writeStreamError` |
| `cachereq.go` | `chatMessage`、`cacheFields`、`cacheIdentity`、`cacheIdentityFor`、`answerSignature`、`conversationPrompt`、`lastQuestionIndex`、`messagesFingerprint`、`messageText`、`tenantFor`、`normaliseCredential`、`capabilitiesFor`、`jsonPresent` |
| `cachepath.go` | `prepareCache`、`cacheAttempt`、`serveCached`、`replayCachedStream`、`storeCache`、`cacheRequest` |
| `quotapath.go` | `QuotaGate`、接口断言 `var _ QuotaGate = (*quota.Manager)(nil)`、常量组、`admitQuota`、`quotaEstimate`、`settleQuota`、`finishQuota`、`setQuotaHeaders`、`quotaMessage`、`dimensionLabel`、`errQuotaStore`、`quotaStoreError`（及其 `Error` 方法） |
| `idempotency.go` | `idempotencyBegin`、`serveReplay`、`idempotencyComplete`、`storedHeaders`、`storedBody`、`replayableResponse`、`requestHash`、`captureWriter`、`newCaptureWriter`、`WriteHeader`/`Write`/`Flush`/`Unwrap` |
| `capabilities.go` | `ModelInfo`、`CapabilityReport`、`ModelCapability`、`UpstreamCapability`、`sortedTargets`、`upstreamState`、`CapabilityProbeRequest`、`CapabilityProbe`、`probeCapabilities`、`probeTinyPNG`、`ErrUnknownCapability`、`ProbeCapabilities`、`probeModel`、`probeOne`、`classifyProbe`、`probeBody`、`mustJSON`、`setNames`、`ProbeJSONReader` |
| `tracing.go` | 常量组、`firstTokenMS`、`TraceExporter`、`Tracer`、`NewTracer`、`begin`、`shouldRecord`、`requestTrace`、`attemptSpan`、`startAttempt`、`attemptSpan.finish`、`requestTrace.addEvent`、`requestTrace.finish` |

- **写作时的差集结论**：把上面这 137 个符号名逐一到 `docs/WALKTHROUGH` 下全部既有文档（本目录的全部成册文件）做 `(?<![A-Za-z0-9_])NAME(?![A-Za-z0-9_])` 的 token 级匹配，**未命中数为 0**——即 02A 全册 + 02B + 02C 已经把 137 条声明全部点名过，本册**没有第三个残留符号**。
- 推断：把这张表当作**检索索引**用——先在表里定位名字属于哪个文件，再去上表找认领分册；`internal/gateway` 的顶层命名空间很平，跨文件重名只有 `Options`/`New` 这一对（与 `internal/router` 同名但不同包），所以按名字查文件通常没有歧义。
- 顺带纠正一个容易记错的归属（本册初稿的表格就写错过，以机器清单为准）：`isCompletionPath` / `isStreamPath` 属于 `request.go`，不属于 `proxy.go`；`itoa` 在 `proxy.go` 里是一行包装 `strconv.Itoa`，连函数体都在同一行。

### 25.2 `internal/gateway/proxy.go` 的 import 面（说明包的耦合边界）

`proxy.go` 的 import 块是观察这个包依赖方向的最好入口，标准库是 `bytes`、`context`、`encoding/json`、`errors`、`io`、`log/slog`、`math/rand`、`mime`、`net/http`、`runtime/debug`、`strconv`、`strings`、`time`；仓库内是 `internal/breaker`、`internal/cache`、`internal/idempotency`、`internal/metrics`、`internal/quota`、`internal/router`、`internal/sessions`、`internal/tracing`、`internal/upstream`。

- 推断：这份清单**没有** `internal/config`、`internal/server`、`internal/sse`——说明网关核心只依赖「接口化后的治理组件」和上游注册表，配置解析与进程装配都留在外面（`New` 的入参已经是解析好的 `Options`）；而 SSE 的写出能力被隔离在 `stream.go`，所以 `proxy.go` 本身能在没有流式场景时被单纯地读懂。
- 几个标准库 import 各自对应一个具体机制：`math/rand` 是 `backoff` 的退避抖动；`runtime/debug` 是 panic 兜底里取栈；`mime` 用于 content-type 的宽松解析（`isJSON`/`isEventStream` 要能容忍参数段）；`strconv` 就是 `itoa`；`bytes`/`io` 服务于 `copyWhole` 与 `readBody`。

## 26. 一次请求的全链路（每个决策点 + 展开位置）

**入口：`ServeHTTP (internal/gateway/proxy.go)`**（本册第 6 节）

1. 造 `record`：`start`、`route = routeLabel(path)`、`path`、`requestID = requestID(r)`、默认 `status = 200`、默认 `outcome = OutcomeSuccess`（本册第 6 节）。
2. `p.tracer.begin(r, rec)`：**先于一切**开 trace，好让上游那一跳以本请求的 span 为父；入站 `traceparent` 先校验再信任（第 02C 册第 8 节）。
3. 注册唯一的 deferred finalizer（本册第 7 节逐条展开），并用 `recover()` 兜 panic → `rec.reason = "panic"`、`TypeInternal` + `"internal gateway error"`。

**第一层：准入与身份 —— `serve (internal/gateway/proxy.go)`**（本册第 11 节）

4. 路径闸门：非补全路径且 `!isProxiablePath(path)` → 404 `TypeBadRequest`，正文 `"unsupported path "+path+"; InferGate proxies OpenAI-compatible endpoints only"`。
5. `readBody`（本册第 16 至 23 节）：读 `maxBody+1` 字节来**自己**判定超限，不信 `Content-Length`；读失败 → 400 `"could not read request body: "`，超限 → 413 `TypeBodyTooLarge` + `"request body exceeds the configured limit"`。
6. `inspectRequest(body)`（本册第 10 至 15 节）解析出 `parsed`；`rec.stream = parsed.Stream && isStreamPath(path)`。
7. 身份只解析一次：`tenantFor(r)`（第 02C 册第 3 节）与 `HeaderSession` 头；源码注释观点是「即使没有缓存也没有预算，日志行也该点出这两个名字」（本册第 11 节引述）。
8. **准入闸门（M3）**：`p.quota != nil && p.quota.Enabled() && isCompletionPath(path)` 时 `admitQuota`（第 02C 册第 5 节）→ 拒绝即返回；降级请求会在这里被 `rewriteModel`/`rewriteMaxTokens` 改写（本册第 11 节展开 `rewriteMaxTokens`）。作者把顺序写成 load-bearing：缓存命中是免费的但**不是隐形**的，所以先开租约、后以零 token 用量结算。
9. **幂等闸门（M6）**：`idempotencyBegin`（第 02C 册第 6 节）在准入之后、缓存之前——重放是调用方**真做过**的那次操作，比「相似问题的答案」是更强的承诺。
10. **缓存闸门（M2）**：`prepareCache` + `cacheAttempt`（第 02C 册第 3 节）。身份取自**客户端发来的 body**，在任何后端专属的模型改写之前，于是两个只是模型名拼法不同的请求共享同一个精确 key。命中即返回，路由决定直接被跳过。

**第二层：路由与尝试 —— `serveAttempts (internal/gateway/proxy.go)`**（本册第 12 节）

11. `plan(parsed, r)`（本册第 10 至 15 节）：`HeaderUpstream` 钉住的后端 + 能力集 → 有序候选链；`p.router == nil` 时退化为 M0 的单后端。
12. 逐个候选 `attemptUpstream`（本册第 12 节与第 13 节）；每次尝试前看断路器与预算，失败按 `retryableStatus` + `backoff`（本册第 10 至 15 节）决定是否换下一个；`maxAttempts` 是放大回路的上界。
13. 每次尝试内部：`rewriteModel`（候选后端不认请求的模型名时）→ `buildRequest`（第 3 至 5 分册的交接点：出站上下文从 `context.Background()` 派生 + 一个显式传播客户端断开的 watcher）→ `client.Do`。
14. 失败分类（本册第 10 至 15 节）：客户端走了 → 499 / `OutcomeCanceled` / `clientGone`；网关自己超时 → **先一个字节都不写**，带 `TypeTimeout` + `errorBodyBytes` 逃逸给后续候选；其余 → 可重试的上游错误。
15. `resp.StatusCode >= 400`：可重试状态把状态、头、体（上限 64 KiB）打包进 `attemptError`，客户端代码因此仍看到**供应商的**状态码（429 就还是 429）；不可重试的 4xx 直接 `passThroughError`（本册第 16 至 23 节）并**记成功给断路器**——4xx 是调用方的错，不该把健康后端踢出轮换。

**第三层：中继**

16. 200 分支先 `forwardResponseHeaders`（本册第 16 至 23 节）+ `HeaderUpstreamName`/`HeaderAttempt`/`HeaderRequestID`/`HeaderTried`（本册第 19 节的头清单）。
17. `rec.stream && isEventStream(resp.Header.Get("Content-Type"))` → `relayStream (internal/gateway/stream.go)`（**第 02B 册第 5 节**）；否则 `copyWhole`（本册第 16 至 23 节）。
18. 两条中继路径都回填 `rec.usage`、`rec.respBytes`、`rec.model` 与 `metrics.Outcome*`；起流之后就不能再换后端了——客户端手里已经有半条答案，第二个 `[DONE]` 是协议违例（02B 引述的源码注释观点）。

**第四层：结算 —— deferred finalizer**（本册第 7 节展开）

19. `metrics.ObserveRequest` → `settleQuota`（第 02C 册第 5 节；写在 defer 里是为了「拒绝、失败的 failover、panic、流中途消失的客户端」都恰好结算一次）。
20. `rec.frames > 0` → `ObserveStreamFrames`；`rec.firstTokenSet` → `ObserveFirstToken`（**尝试**的观测点刻意不在 defer，而在 `observeAttempt`，否则失败后端的错误率不可见、断路器永远跳不了）。
21. `idempotencyComplete`（第 02C 册第 6 节）→ trace 事件（`idempotency.*`、`cache.*`、`failover`、`stream.complete`）→ `rec.trace.finish`。
22. `recordSession`（本册第 21 节 / 第 02C 册第 9 节）：**最后**更新会话汇总，从而带上最终状态、实测延迟与尝试次数。
23. 结构化请求日志：唯一一条，按 outcome 选级别（成功 Info、取消 Debug、其余 Warn）。

**只读一册的读者该读哪册**：先本册第 1、2 节建立坐标，再按上表跳到具体文件所在的分册。若只能读**一册**，读 **`第 02C 册`**——它自带「共享词汇」一节（`record`、网关控制头、请求谓词），把跨文件复用的名词一次性立住，并且覆盖了六个治理文件的全文；但它**不含** `stream.go`，所以流式请求还要补 **02B**。若目标只是「一次普通（非流式）补全请求怎么走完」，读 **本册第 1 至 22 节**即可闭环。

### 26.1 决策点速查表（同一次请求，换一个视角）

把上表 23 步重新按「触发条件 → 结果 → 展开分册」排一遍，方便对照源码读：

| # | 决策点 | 触发条件 | 命中后的去向 | 展开分册 |
| --- | --- | --- | --- | --- |
| 1 | 路径闸门 | 非补全路径且 `!isProxiablePath` | 404 返回，不进任何治理环节 | 本册第 10 至 15 节 |
| 2 | 读体闸门 | 读失败 / 超过 `maxBody` | 400 或 413 返回 | 本册第 16 至 23 节 |
| 3 | 体解析 | body 不是合法 JSON 或不含 messages | `parsed` 退化为空，走「无身份」路径 | 本册第 10 至 15 节 |
| 4 | 配额准入 | `p.quota != nil && Enabled() && isCompletionPath` | 拒绝即返回；可能改写模型与 max_tokens | 第 02C 册第 5 节 |
| 5 | 幂等查表 | 带 `Idempotency-Key` 且命中在途/已完成条目 | 命中即 serveReplay 或等待，跳过缓存与路由 | 第 02C 册第 6 节 |
| 6 | 缓存查表 | `Cache != nil` 且身份可算 | 命中即返回（流式走 replayCachedStream） | 第 02C 册第 3 节 |
| 7 | 候选规划 | `Router != nil` | 用能力集 + 钉住的后端生成有序候选 | 本册第 10 至 15 节 |
| 8 | 断路器检查 | `Breakers != nil` 且该后端开路 | 跳过该候选，尝试下一个 | 本册第 10 至 15 节 |
| 9 | 单次尝试 | 总在本步执行 | 组装请求 → `client.Do` | 本册第 16 至 23 节 |
| 10 | 客户端断开 | watcher 观察到 `r.Context()` 结束 | 499 + `OutcomeCanceled`，不再换后端 | 本册第 10 至 15 节 |
| 11 | 网关超时 | `budget` 耗尽 | 未写字节时带错误逃逸；写过了就只能收尾 | 本册第 10 至 15 节 |
| 12 | 状态分类 | `status >= 400` | 可重试则打包进 `attemptError` 换候选；不可重试 4xx 直接透传 | 本册第 10 至 15 节 |
| 13 | 中继分叉 | 200 且 `rec.stream && isEventStream` | `relayStream`，否则 `copyWhole` | 第 02B 册第 5 节 / 本册第 16 至 23 节 |
| 14 | 结算 | 总在 defer 执行 | 配额、指标、幂等、trace、会话、日志各一次 | 第 1 至 9 节 / 第 02C 册|

- 推断：这张表的第 4、5、6 行顺序就是整条管线里最容易被改错的一处——任何人把缓存放进配额准入**之前**，都会让缓存命中从「免费但计入请求数」变成「完全隐形」，于是预算系统看不见那部分流量；把幂等放进缓存之后，则会让同一个 `Idempotency-Key` 的第二次请求拿到「相似问题的答案」而不是「上次那个答案」。

### 26.2 deferred finalizer 的精确顺序与它为什么必须是这个顺序

源码里 defer 的那一个闭包按下列顺序执行（本册第 7 节逐个展开语义，这里只讲顺序本身的理由）：

1. `recover()` 兜 panic，把 `rec.reason` 写成 `"panic"`，保证第 2–7 步仍会跑完。
2. `p.metrics.ObserveRequest`——**最先**记请求级指标，因为它是唯一不依赖任何其它结算结果的观测。
3. `p.settleQuota(r.Context(), rec)`——配额结算必须在指标之后、会话之前，因为最终 `usage` 在此时已经定型（中继路径回填完毕）。
4. `if rec.frames > 0 { ObserveStreamFrames }`、`if rec.firstTokenSet { ObserveFirstToken }`——用 `rec` 上的**标志位**而不是时间戳判空，因为零值时间戳无法区分「没有首 token」与「首 token 在时间零点」。
5. `p.idempotencyComplete(rec)`——把响应写进重放存储；放在这里意味着**客户端断连的请求也可能被存**，所以它内部必须先看 `replayableResponse (internal/gateway/idempotency.go)`。
6. trace 事件（`idempotency.*`、`cache.*`、`failover`、`stream.complete`）→ `rec.trace.finish(rec)`——trace 必须最后由自己收尾，且**必须在幂等落库之后**，否则 trace 里看不到重放决策。
7. `p.recordSession(r, rec)` → 唯一一条结构化日志——会话汇总与日志放最后，因为它们是「最终状态」的读者：状态码、实测耗时、尝试次数、`cacheStatus`、`reason`、`firstTokenSet` 在此时才全部确定。

- 推断：把这些副作用串在**一个** defer 里而不是散落在各分支，是为了让「拒绝、失败的 failover、panic、流中途消失的客户端」四类终止路径都恰好结算一次；散落写法在新增第五类终止路径时极易漏掉某一项（例如配额租约永远不归还）。

## 27. 残留符号（前三册未落地、相邻册未认领）

本节的结论经过**两轮**核对，第二轮是机器差集：把 `internal/gateway` 全部非测试文件的 137 条顶层/方法声明逐一到 `docs/WALKTHROUGH` 下所有既有文档里做 token 级匹配，结果是**除下面两个符号外没有任何声明在两份清单之外**。第一轮只扫了本册的一部分章节、漏了另外一些章节，因此一度误判 `stream.go` 的三个符号是残留；第二轮修正后，`stream.go` 归 02B。这一段保留写法差异，是为了说明「残留」这个结论对**扫描面**极度敏感：差集时必须把 `docs/WALKTHROUGH/` 下的全部成册文件一起纳入。

### `Options` (internal/gateway/proxy.go)

`type Options struct` —— `New` 的唯一入参；**值语义**传递，`New` 只读不存（源码注释：字段级注释就写在结构体里，没有包级文档注释）。

| 字段 | 类型 | 源码注释里的「为什么」 |
| --- | --- | --- |
| `Upstreams` | `*upstream.Registry` | 把模型（外加可选 pin）解析成后端；无归一化，直接进 `Proxy.upstreams` |
| `Pricing` | `*PriceBook` | nil 表示「不算成本」 |
| `Metrics` | `metrics.Sink` | nil 即 `metrics.Nop` |
| `Logger` | `*slog.Logger` | nil 即 `slog.Default()` |
| `MaxBodyBytes` | `int64` | 入站体上限；`<= 0` 在 `New` 里取 8 MiB |
| `UpstreamTimeout` | `time.Duration` | 一次上游交换（含流式）的时限；**零值关掉网关级上限**，只留客户端断开作为唯一终止条件 |
| `TransportFor` | `func(*upstream.Target) http.RoundTripper` | 覆盖 transport 选择，「测试用它；生产留 nil，让每个后端有自己的池化 transport」 |
| `Router` | `*router.Router` | nil 即 M0 行为：一个后端、不重试、不感知健康 |
| `Breakers` | `*breaker.Group` | nil 同时关掉熔断与路由排序所依据的时间窗统计 |
| `MaxAttempts` | `int` | 候选链上界；零或一即单次尝试 |
| `RetryBackoff` | `time.Duration` | 尝试间的基础延迟 |
| `Cache` | `*cache.Cache` | nil 完全关闭缓存（M0/M1 行为），所以开启它是**显式配置决定而不是默认** |
| `Quota` | `QuotaGate` | nil 即不强制预算 |
| `QuotaCharsPerToken` | `int` | 把入站 body 估成 prompt token；零即包默认 4 |
| `QuotaCompletionTokens` | `int` | 请求没声明 completion 上限时的预留量；零即包默认 256 |
| `Tracer` | `*Tracer` | nil 关闭追踪 |
| `Idempotency` | `*idempotency.Store` | nil 即忽略 `Idempotency-Key` |
| `Sessions` | `*sessions.Ledger` | nil 即不留会话汇总 |
| `Models` | `map[string]ModelInfo` | 线上不携带的每模型元数据（上下文多大、能吐多少、支持哪些能力标签、运维想记什么）；**只被能力端点读，绝不在请求路径上读** |

- 推断：`QuotaCharsPerToken` / `QuotaCompletionTokens` 的「零即默认」两个默认值**不在 `New` 里**落地，而是在 `quotaEstimate (internal/gateway/quotapath.go)` 里就地回落（`defaultQuotaCharsPerToken` / `defaultQuotaCompletionTokens`）。后果：`Proxy.quotaCharsPerToken` 字段可能长期为 0，任何直接读该字段的代码都必须自己补默认值；只有 `quotaEstimate` 这一个读者，所以这个分散是安全的，但它是新增读者时的陷阱。
- 并发：`Options` 是构造期值，不参与运行期共享；每个 `Proxy` 字段在构造后只读（源码注释的观点）。

### `New(opts Options) *Proxy` (internal/gateway/proxy.go)

`func New(opts Options) *Proxy` —— 唯一构造器；用字段名显式列出**全部** `Proxy` 字段做复合字面量，然后对四类输入做归一化。

- **归一化 1（nil → 默认）**：`logger == nil` → `slog.Default()`；`sink == nil` → `metrics.Nop{}`；`maxBody <= 0` → `8 << 20`（8 MiB）。推断：这三个都在请求路径上被**无条件**使用，所以归一化放在构造期而不是使用点，让热路径没有分支。
- **归一化 2（下界）**：`p.maxAttempts = opts.MaxAttempts`，`if p.maxAttempts < 1 { p.maxAttempts = 1 }`。源码注释关于该字段的观点：它存在的目的是掐断放大回路——三个坏后端各重试三次会把一个客户端请求变成九个 provider 请求，而 provider 的限流器为此买单。
- **归一化 3（nil → 非 nil 传递）**：`p.retryBackoff = opts.RetryBackoff` 原样拷贝（0 合法，由 `backoff` 自己处理）。
- **归一化 4（nil 函数值 → 闭包）**：源码注释原文的观点是「nil 的 `transportFor` 绝不能被触及：这是一条每请求代码路径，这里一个 nil 函数值意味着每一个请求都 panic 成 500」。因此 `transportFor == nil` 时装配 `func(t *upstream.Target) http.RoundTripper { return t.Transport }`，即回落到注册表挂在该 target 上的 transport，让「只用 `Options` 造一个 `Proxy`」永远是安全的。
- **不归一化的一项**：`client: &http.Client{}`，代码注释的理由是 `// no Timeout: streaming bodies must not be cut off by the client`。推断的机制与后果：`http.Client.Timeout` 覆盖**整个**请求（含读 body），一旦设置就会在长生成中途切断流式响应，且切断表现为传输层错误而不是干净的网关超时；因此超时只能由每次尝试的 `buildRequest` 用 `context.WithTimeout` 承担。
- **错误路径**：`New` 不返回 error——所有输入都被归一化或被接受。副作用是**配置错误不会在这里暴露**：一个 `Upstreams == nil` 的 `Proxy` 会构造成功，直到第一个请求走 `plan` 才失败。推断：这是刻意的「构造永不失败」契约，代价是把配置错误的发现推到了请求路径。
- **读写状态**：只读 `opts`，写出并返回一个全新的 `*Proxy`；不做 I/O、不起 goroutine、不加锁。
- **调用关系**：唯一生产调用点是 `gateway.New(gateway.Options{...}) (internal/server/server.go)`（组装见 `01-http-server.md`）；测试里被大量使用，尤其靠 `Options.TransportFor` 注入模拟死后端 / 慢后端 / 流中途断连（这正是它存在的原因，源码注释观点）。

## 28. 已由相邻册覆盖（本册不写）

- `stream.go` 的全部五个顶层声明——`heartbeatInterval`、`newRequestID`、`relayStream`、`firstVisibleToken`、`writeStreamError`——已由 **第 02B 册第 5 节**逐个写完（签名、逐段行为、返回的 outcome 对照、并发契约），本册不重复。补一句只属于总览的定位：`internal/gateway/stream.go` 是 `internal/gateway/proxy.go` 之外**唯一**会在处理一个请求时写出客户端字节的地方（`ServeHTTP` → `serve` → `serveAttempts` → `attemptUpstream` → `relayStream`），它用一个 reader goroutine 独占 `resp.Body`、主 goroutine 独占 `sse.Writer`，两者只通过容量 8 的 channel 与请求上下文交流。
- `Options` / `New` 在本册前面的章节里只以「被引用」形态出现：一处把二者记作「紧邻本节范围但未列入，故未展开」，另一处只在「不在本节」清单里点名，还有一处未提。`03-router-breaker-ledger.md` 里的 `Options`/`New` 是 **`internal/router/router.go` 的同名类型**，与本册无关；`04-cache-quota-memory.md` 里出现的 `Options` 是 `RedisOptions (internal/cache/redis.go)`。本节因此是 `gateway.Options` / `gateway.New` 的首次展开。
- 本册开头提到的「缺口」清单里还有两项**经核对不存在**，不写：`internal/gateway/` 下没有 `DefaultTimeout`、`ErrBodyTooLarge` 之类的顶层声明（`05-observability.md` 的 `Options` 属其它包），故本册不为它们编条目。

## 29. 出站头规则与两条测试面：`failover_test.go` / `headers_drop_test.go`

前面四节把实现读完了，但 `internal/gateway/` 下还有两册**没有任何分册认领过**的文字：`failover_test.go` 与 `headers_drop_test.go`。它们不是"又一个用例清单"，而是把 M1 的几条不变量写成了可执行的断言；读实现时最容易漏掉的恰是这些约束。本节先记录三处本册独立核对过的实现细节，再从这两个测试面反推设计意图。

### 29.1 `stream.go` 中与 `02b-stream-and-upstream.md` 互补的三点（不重复其逐段走读）

`stream.go` 的五个顶层声明已由第 02B 册第 5 节逐段写全，本册不重抄。下面三点属于该节没有展开、而读代码时决定行为正确性的补充：

- **为什么头必须在调用 `relayStream` 之前设好**：`relayStream` 内部做的第一件有副作用的事就是 `sw.WriteHeader(http.StatusOK)`（提交状态行与响应头）。**推断**：`net/http` 在状态行写出去之后，对 `w.Header()` 的后续写入一律静默丢弃，所以 `HeaderUpstreamName`、`HeaderAttempt`、`HeaderRequestID`、`HeaderTried` 与 `forwardResponseHeaders` 复制过来的上游头**只能**在调用之前设好；这也解释了 `TestStreamingFailoverRelaysTheSecondBackend` 为什么能在流式响应上断言 `HeaderUpstreamName`——这些值在失败的第一次尝试之后、第二次尝试起流之前就冻结了。反过来说，`serve`/`serveAttempts` 里任何"事后补一个响应头"的想法在流式路径上都不成立。
- **逐帧 Flush 落在哪一层**：置 `X-Accel-Buffering: no` 与 `rc.Flush()` 都在 `writeHeaderLocked (internal/sse/writer.go)` 里（写状态码之后立刻 flush 一次），后续每帧由 `Writer` 的写路径 flush。**推断**：网关没有设置 `http.Client.Timeout` 是有连带后果的——`Client.Timeout` 覆盖到响应体读取结束，一旦设置就会在长生成中途切断流，而且切断表现为传输层错误而不是网关自己的 504；因此流式请求的时限只能由每次尝试的 context 期限承担（见 `TestStreamingRespectsUpstreamTimeout`）。
- **缓存写入必须是最后一步，且只在干净结束时发生**：`storeCache` 出现在函数最末，前面有 7 条提前 return（写者构造失败、提交状态行失败、客户端断开、心跳失败、读流失败、带内错误、写帧失败）全部绕过它；即便走到末尾，`cacheOverflow` 也会把它换成 `rec.cacheReason`。**推断（机制与后果）**：一条缓存条目在后续请求里会被当作**完整答案**投递——它计入请求数、`X-InferGate-Cache` 报命中、而 provider token 用量为零。若半截流被存下来，重放出去的就是语法合法、语义错误的响应，且网关会在响应头里宣称这是一次成功命中。同一条理由也解释了为什么合成出来的 `[DONE]` 要经过 `capture`：哨兵是网关自己写的**唯一**一帧，不抓进缓存，重放就会让客户端等一个永不到来的结束信号。
- **`writeStreamError` 不经过 `capture`**：`writeStreamError (internal/gateway/stream.go)` 写出的 `TypeBadGateway` 信封不交给抓取闭包，所以失败路径不会留下"带内错误帧"的缓存条目；它的两个错误出口（`marshalJSON` 失败、`WriteData`/`WriteDone` 失败）都被显式丢弃，这是"最好努力"语义——**推断**：调用方紧接着就 `return`，没有第二个可用的错误通道，状态码又已经提交。

### 29.2 `errorBodyBytes` 为什么忽略它的 `status` 形参（细读）

- 第一层原因是结构性的：`ErrorBody (internal/gateway/errors.go)` 没有状态码字段，状态码由 `w.WriteHeader` 单独携带，所以 `errorBodyBytes` 拿到 `status` 也无处安放（函数体第一句就是 `_ = status`）。
- 第二层原因才是真正约束实现的：**两条错误体路径必须逐字节相同**。其一是"立刻写"——`writeError` 用 `json.NewEncoder(w).Encode` 编码，`Encode` 会在结尾追加一个换行；其二是"所有候选都失败"——`attemptUpstream` 把预渲染的 `body` 装进 `attemptError`，由 `serveAttempts` 用 `writeRawError(w, last.status, last.header.Get("Content-Type"), last.body)` 原样写出。若 `errorBodyBytes` 把状态码嵌进 body 或漏掉那个 `'\n'`，同一次超时会在"后面还有候选成功"与"所有候选都失败"两种走向下产出不同的字节，而客户端只看到其中一种、把它当契约。
- 可机械核对的唯一调用点：`attemptUpstream (internal/gateway/proxy.go)` 的期限分支，实参为 `http.StatusGatewayTimeout`、`TypeTimeout`、`"no upstream answered before the gateway deadline"`；形参被传入又被忽略这件事，在这个调用点上一眼可见。

### 29.3 `failover_test.go`：测试装配与它钉住的不变量

- 装配件：`chatCompletionBody`（带 `usage` 的非流式答案）、`failoverOptions`（两个后端 URL 加策略/优先级/模型/能力/`maxAttempts`/`HealthConfig`/缓存的覆盖项）、`testHealthConfig()`（`Window` 一分钟、`Buckets` 6、`MinRequests` 3、`FailureRatio` 0.5、`OpenDuration` 30 秒、`HalfOpenProbes` 1、`MaxFailuresPerRequest` 2、`RetryBackoff` 1 毫秒）、`newFailoverProxy (internal/gateway/failover_test.go)`（装配真实 registry、breaker、router 与 proxy，`maxAttempts` 缺省 2）、`jsonBackend`。
- 源码注释观点（为什么什么都不 stub）：这一层已经产出过的 bug——一个被消费掉的 half-open 探针、一个被误当成"答案已发出"的 503——只存在于四者交互里，stub 掉任何一个都测不到它们。

| 用例 | 它钉住的不变量 | 实现偏了会怎样红 |
| --- | --- | --- |
| `TestFailoverOn5xx` | 首选 503 → 由下一个候选作答，body 与备用后端的答案**逐字节相同**；`HeaderUpstreamName` 为 `backup`、`X-InferGate-Tried` 为 `"primary, backup"`、`HeaderAttempt` 为 `"2"`；每个后端恰好各收到一次请求；原因为下游尝试的熔断窗口里 primary 恰有一次失败、backup 恰有一次成功；`rec.FailoverSnapshot()` 恰一条、`Upstream` 为 `primary`、`Status` 为 503 | 若在写状态行前没把失败体的字节读完、或把 503 的体中继出去，body 相等那条红；`X-InferGate-Tried` 的分隔符或 `tried` 只留最后一个后端时 header 断言红；若每次尝试不上报窗口样本（只在 finalizer 里上报），共享窗口凑不齐证据，窗口断言红 |
| `TestNoFailoverOn4xx` | 400 不重试；客户端拿到的是**供应商信封原文**；备用后端请求数为 0 | 把 4xx 放进可重试集合，或重新编码供应商信封（丢掉厂商字段），两条断言分别红 |
| `TestFailoverOn429` | 429 属于"关于这一刻、不属于这个请求"的状态，必须失败转移 | 429 被移出可重试集合时，客户端会拿到 429 而不是 200 |
| `TestAllCandidatesFailForwardsTheProviderError` | 全部失败时客户端看到的是**最后一个**供应商的状态码与信封（primary 503 + backup 502 → 502），不是合成的网关 502，也不是第一个失败的 503 | 终局分支改用网关自造错误时，状态码与 body 两条同时红 |
| `TestMaxAttemptsCapsTheFailoverBudget` | `maxAttempts = 1` 时绝不碰第二个后端（备用后端请求数为 0），客户端拿到 primary 的 503 | 循环边界写成 `<=` 或候选链归一化多算一个时，备用后端计数从 0 变 1 |
| `TestTimeoutFailsOverWithoutAnswering504ForTheClient` | 网关自己的期限触发时**一个字节都不写**，让下一个候选仍可能作答；客户端最终拿到 200 且 `HeaderUpstreamName` 为 `backup`。源码注释观点：`p.upstreamTTL` 取 2 秒是刻意的宽裕值，因为期限还要覆盖第二次尝试的连接建立，更短的版本单独跑过、全量跑挂 | 期限分支改成先写 504 再重试，客户端会拿到 504，且 upstream 名断言失去意义 |
| `TestLastCandidateTimeoutAnswers504` | 反面：唯一候选超时时必须告知客户端，body 含 `TypeTimeout` | 期限分支永远逃逸给候选链、没有终局兜底时，客户端会拿到一个没有状态码可言的空响应 |
| `TestBreakerStopsRoutingToADeadBackend` | 三次失败后 `breakers.Get("primary").State()` 为 `breaker.StateOpen`；第 4 次请求时 primary 的命中计数**一个都不涨**；backup 服务满 4 次 | 候选规划把开断后端直接删掉而不排到最后、或代理绕过熔断器直接试，命中计数对比立刻红 |
| `TestBreakerHalfOpenAdmitsOneProbeAndRecovers` | 走完整恢复路径：路由器必须能对一个开断后端**排序**而不偷走代理随后要用的唯一探针；恢复后 upstream 名为 `primary`、状态回 `breaker.StateClosed` | 路由阶段的排序决定顺手消费掉探针时，代理拿不到名额，upstream 名不会是 primary |
| `TestCanceledProbeGivesTheHalfOpenSlotBack` | 探针被放行、调用方在后端作答前消失时，下一次请求仍必须能到达该后端（`StateClosed`），且被取消的调用方收到 0 字节 body。源码注释观点：被取消的调用方**不是关于后端的证据**，这条路径不上报判决，half-open 名额必须显式归还；否则 primary 会在进程余生里被 `"half-open probe already in flight"` 拒绝 | 取消路径把判决上报、或忘记归还名额，后续请求的 upstream 名与 `StateClosed` 断言红 |
| `TestBackendReceivesItsOwnModelName` | 逐候选改写：失败转移到声明了具体模型名的后端时，调用方用的别名不能泄漏过去（后端收到的 body 里 `model` 等于后端自己声明的名字） | 改写只在首个候选上做，或改写发生在缓存身份计算之后的方向搞反，`recordedRequest.Model` 断言红 |
| `TestExplicitPinSkipsThePreferredBackend` | `HeaderUpstream` 必须压过路由偏好：upstream 名为 `backup`、primary 请求数为 0 | 钉住的后端只当"优先"处理时，primary 会被先试，计数从 0 变 1 |
| `TestCapabilityHeaderExcludesABackend` / `TestUnmatchedCapabilityIs400NotADowngrade` | `HeaderCapabilities` 是**资格过滤**而不是偏好排序：只有声明了该能力的后端收到请求；没有任何后端声明时返回 400 且上游请求数为 0 | 能力被降级成"尽量满足"时，未声明的后端会被命中；400 改成"挑一个兜底"时两条断言同时红 |
| `TestStreamingFailoverRelaysTheSecondBackend` | 流式路径必须是 failover 逻辑的一部分，不是忘了它的一条独立代码路径：最终 200、body 以 `frameDone` 结尾、upstream 名为 `backup` | 流式响应头从失败的首个后端的 503 响应里取（而不是从成功候选的响应里取）时，`Content-Type` 与 relay 模式判定都会偏，upstream 名断言红 |

### 29.4 `headers_drop_test.go`：逐跳头与网关控制头为什么必须停在这里

- 它钉住的是 `copyHeaders (internal/gateway/headers.go)` 的**双向非对称**契约，而 `copyHeaders` 自身的实现已在 `本册第 16 至 23 节` 逐符号写过，这里只写这条测试钉住什么。
- 请求方向（`dropGatewayHeaders` 为真）：源头上同时放了 `HeaderIdempotencyKey`、`"X-InferGate-Tenant"`、`"X-InferGate-Session"`、`"Connection"`、`"X-Drop-Me"`。断言这五个名字在目标头里**全部为空**，同时 `"Authorization"`、`"Content-Type"`、`"X-Custom"` 被保留且值不变。
  - 两类名字的丢法不同，这正是测试想固化的：`"Connection"` 与它点名的 `"X-Drop-Me"` 靠每消息一份的动态名单丢掉（静态逐跳表看不见被 `Connection` 点名的头），网关控制头则靠 `isGatewayHeader (internal/gateway/headers.go)` 的前缀/白名单判定丢掉。
  - **推断**：`"X-Drop-Me"` 这一条是最锋利的设计——它在静态表里根本不存在，只有在实现真的解析了 `Connection` 头之后才会消失；任何"把连接头删掉了事"的写法都会把它漏出去。
- 响应方向（`dropGatewayHeaders` 为假）：源是"供应商"的一份头，含 `HeaderIdempotencyKey` 与 `"X-Ratelimit-Remaining"`，断言两者都**存活**。源码注释观点：反方向是回给调用方的响应，同名头在这里是网关/供应商自己的答复，必须留下；`X-Ratelimit-Remaining` 代表那类"值必须原样抵达客户端"的供应商元数据（客户端据此自我限速）。
- 源码注释观点（为什么这条不变量重要，`Idempotency-Key` 是最锋利的例子）：一个在**同名头**上实现自家幂等的供应商会跨租户去重。两个租户碰巧选了同一个键字符串（共享模板里的 UUID、`"retry-1"`、一个日期）在网关这里不是碰撞（键按租户分作用域），到供应商那里就是——第二个租户会被服务以**第一个租户的答案**。这不是风格问题，是跨租户数据泄漏。
- **测试盲区**：源头的头名全部是规范首字母大写，因此没有覆盖 `isHopByHop` 的大小写不敏感行为；`http.Header` 的 `Get`/`Set` 在测试里走 canonical key，一个大小写敏感的实现也能让本用例通过。

## 30. 未能确认

- **未跑 `go test`**：本会话的 Go 工具链需要下载 1.26，被沙箱拒绝，因此本册的行号正则自查是**正则人工核对**，不是 `TestProseCitesSymbolsNotLineNumbers` 的实跑结果。
- `Proxy` 的字段清单我只核对了 `Options` → `Proxy` 的**字段名对应关系**；`Options` 是否还有我在 `internal/gateway/proxy.go` 读到的那 19 个字段之外的镜像字段，未逐个做机械化比对（本册第 3 节的字段条目与第 25 节表格是对同一段源码的两次人工阅读）。
- `New` 与 `Options` 的「构造永不失败」契约是否在别处被显式测试断言，未确认：`internal/gateway/proxy_test.go` 存在，但本册未逐条读该测试文件来判断它是否覆盖 nil 归一化。
- `internal/gateway/proxy.go` 里 `attemptUpstream` 的某个注释出现明显的编码损坏（注释正文里混入了 `鈥?` 形状的乱码字节），本册按语义还原为「没有任何连接可写，也没有别的候选值得试」。这是**源文件内容问题**，不是走读问题；若要修，应修源码注释本身。
- 本册没有独立核实「137」这个数字在**测试文件**里是否有额外的顶层声明（扫描面刻意排除了 `*_test.go`），因为走读只覆盖实现文件。
- **写作期间目标文件被本册之外的进程改动过**：`edit` 工具报出 `file changed since it was read`，重新读取后发现开头关于锚点写法的说明、以及 `docs/WALKTHROUGH` 这个路径的写法被改写成了加反引号的版本。成因**未能确认**（推测与 `internal/repofmt/citedpaths_test.go` 里 `proseSurfaces` 那套扫描/规范化面有关，但作者没有读到能证明这一点的代码）。对读者的影响：本册行内提到文件路径时一律加反引号。
- 本册第 2.2 节列的 finalizer 顺序是**对源码闭包体的阅读结论**，未用一次真实请求去观测执行序列（尤其是 panic 路径下第 2 步起是否全部跑完）。
- 本册未逐个核对该机制在 `internal/gateway/` 之外是否还有第二处实现（例如 `internal/server` 里是否另有一层结算），因此「恰好结算一次」是就网关核心包内而言的结论。
- **第 5 节是追加写入，不是整册重写**：本文件的前四节由另一路并行写作产出，第 5 节与「未能确认」由本次追加写入。两部分的锚点风格、术语和结论**没有经过一次整体校对**；如果发现前后对同一符号的说法不一致，以源码为准。
- **分工与本册 brief 不符**：写作 brief 要求本册把 `stream.go` 与 `errors.go` 逐符号写全，但落盘时核对发现 `stream.go` 的五个顶层声明已由 `02b-stream-and-upstream.md` 逐段覆盖、`errors.go` 也已在第 1 节写全。为避免同一段源码出现两份互相漂移的走读，本册改为只写「该册没有展开、而决定行为正确性」的三点（第 5.1 节）与 `errorBodyBytes` 的字节契约（第 5.2 节），并在第 5.3、5.4 节补上两册都没提过的两个测试文件。**这处偏离是按仓库实际覆盖情况做的取舍，不是漏写。**
- 第 5.3 节的用例表是**从测试断言反推设计意图**，不是逐行抄测试；表中「实现偏了会怎样红」一列是**推断**，没有靠改动实现来实测验证（本会话跑不了 `go test`）。
- 第 5.4 节提到的测试盲区（`isHopByHop` 的大小写不敏感没有用例覆盖）是**读用例的源字符串得出的**，未构造一个混合大小写的 `http.Header` 去实跑确认。

## 31. 本册覆盖清单

**本册逐符号覆盖的单元：**

- `internal/gateway`（`proxy.go`、`request.go`、`errors.go`、`headers.go`、`pricing.go`、`sessionpath.go`、`stream_helpers.go`）
- 测试面：`internal/gateway/failover_test.go`、`internal/gateway/headers_drop_test.go`

**本册明确没有覆盖的东西：**

- `internal/gateway` 的 `stream.go`、`cachepath.go`、`quotapath.go`、`idempotency.go`、`tracing.go`、`capabilities.go` 等流式与治理文件（属 02B 册与 02C 册）。
- `internal/config` 的字段定义：本册只引用 `config.*` 的名字，不逐符号走读。
- `internal/router`、`internal/breaker`、`internal/stats`（属第 03 册）；`internal/cache`、`internal/quota`、`internal/idempotency`、`internal/sessions`（属第 04 册）。
- `cmd/` 下的一切可执行体与 `deploy/`、`Dockerfile`（属第 06 册与第 07 册）。
- 本会话未跑 `go test`：所有"由某用例钉住"的表述都是对测试源码的静态阅读结论。

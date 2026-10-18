# 第 02C 册 · 网关治理路径（缓存 · 配额 · 幂等 · 能力探测 · 请求级追踪）

本册逐符号走读 `internal/gateway` 下五条"治理路径"（语义缓存、配额准入、幂等重放、
能力探测、请求级追踪）外加会话账本的入口函数 `recordSession`。走读对象是下列文件：

- `internal/gateway/cachereq.go`
- `internal/gateway/cachepath.go`
- `internal/gateway/quotapath.go`
- `internal/gateway/idempotency.go`
- `internal/gateway/capabilities.go`
- `internal/gateway/tracing.go`
- `internal/gateway/sessionpath.go`

配套阅读：`internal/gateway/proxy.go`（`serve` 的调用顺序、`record` 结构、`buildRequest`）、
`internal/gateway/headers.go`（头常量与取值）、`internal/gateway/errors.go`（错误信封）、
`docs/DESIGN.md` 的 M2/M3/M5/M6 章节。

## 1. 阅读契约

**锚点约定**：每条条目的锚点写成 `符号名 (路径)`，例如 `cacheAttempt (internal/gateway/cachepath.go)`。本册正文一律不写行号，
因为仓库自检测试 `internal/repofmt/citedpaths_test.go` 会把散文里的行号引用判红。需要定位时请用
符号名去搜。

**覆盖约定**：本册把范围内的每个类型、常量、变量、函数、方法各自独立成条，包括只在包内使用的
小函数（如 `jsonPresent`、`mustJSON`），不合并。签名照抄源码，参数名与返回类型不做改动。

**称呼约定**：源码注释里解释"为什么"的句子在条目里以「源码注释观点」标出，是本册对原文的翻译
与保留，不是本册作者的主张。

---

## 2. 共享词汇（跨文件复用，先立一次）

这几件事在所有治理路径里反复出现，先集中说明，后面各节引用即可。

### `record` 结构 (internal/gateway/proxy.go)

一次请求的全部事实都攒在 `*record` 上，由 `ServeHTTP` 的 deferred recorder 统一消费
（一次指标观测、一条日志、一次 trace 收尾）。与治理路径相关的字段有：

- `cacheIdent *cacheIdentity`、`cacheStatus string`、`cacheReason string`：缓存身份与判定结果。
- `tenant string`、`session string`：预算与缓存隔离命名空间、会话 id。
- `quotaRes *quota.Reservation`、`quotaAction`、`quotaReason`、`quotaLimit`、`quotaUsed`、
  `quotaRequested`、`quotaSettleError`、`degradedModel`：配额租约与判定。
- `servedFromCache bool`、`servedFromReplay bool`：结算时"只算请求数、不算 token"的两个标记。
- `idemKey`、`idemScope`、`idemHash`、`idemClaimed`、`idemDecision`、`idemReason`、`idemStore`、
  `idemCapture *captureWriter`：幂等占用与捕获。
- `traceparent string`、`tracestate string`、`trace *requestTrace`：W3C 传播上下文与在建 trace。

**并发**：`record` 只被服务该请求的那一个 goroutine 读写（除 `captureWriter` 这种被写穿到别的
goroutine 的包装需要自己保证），所以它自己没有锁。

### 网关控制头 (internal/gateway/headers.go)

- `HeaderCache = "X-InferGate-Cache"`：双向头。请求侧取值 `cacheDirectiveBypass = "bypass"`、
  `cacheDirectiveRefresh = "refresh"`；响应侧取值 `cacheStatusHitExact = "hit-exact"`、
  `cacheStatusHitSemantic = "hit-semantic"`、`cacheStatusMiss = "miss"`、`cacheStatusSkip = "skip"`、
  `cacheStatusDisabled = "disabled"`、`cacheStatusError = "error"`。
- `HeaderCacheAge = "X-InferGate-Cache-Age"`：命中条目的年龄（整毫秒）。
- `HeaderTenant = "X-InferGate-Tenant"`、`HeaderSession = "X-InferGate-Session"`、
  `HeaderCapabilities = "X-InferGate-Capabilities"`。
- `HeaderQuota = "X-InferGate-Quota"`、`HeaderQuotaReason`、`HeaderQuotaLimit`、`HeaderQuotaUsed`、
  `HeaderQuotaModel`、`HeaderQuotaMaxTokens`。
- `HeaderIdempotencyKey = "Idempotency-Key"`、`HeaderIdempotentReplay`、`HeaderIdempotentOrigin`、
  `HeaderIdempotentUpstream`、`HeaderIdempotentAge`、`HeaderIdempotentStore`，以及取值常量
  `idempotentTrue = "true"`、`idempotentFalse = "false"`、`idempotentStored = "stored"`、
  `idempotentOversize = "oversize"`、`idempotentSkip = "skip"`。
- `idempotencyUpstream = "replay"` 与 `cacheStatusUpstream = "cache"`：两个"冒充上游名"的标签，
  理由是**产生答案的后端不是服务这次请求的后端**（源码注释观点），把它记到真后端账上等于给一个
  从未到达的流量记账。
- `isGatewayHeader(name string) bool`：把 `Idempotency-Key` 与 `x-infergate-` 前缀全部拦在网关内，
  不透传给 provider。

### 请求控制的三个谓词 (internal/gateway/request.go)

- `isCompletionPath(path string) bool`：`TrimSuffix(path, "/")` 后以 `/chat/completions`、
  `/completions` 或 `/embeddings` 结尾即为真。缓存、配额、幂等、会话账本全都以它为门。
- `isStreamPath(path string) bool`：只认 `/chat/completions`。
- `parsedRequest` 的 `Model`、`Stream`、`MaxTokens`、`Messages`、`IsJSON` 五个字段，
  由 `inspectRequest(body []byte) parsedRequest` 填充。

---

## 3. 缓存身份：`internal/gateway/cachereq.go`

文件头注释给出的总纲：路由只需要模型与能力，缓存还需要知道"问的是哪个问题"（好让改写能被认出）
与"哪个答案有资格被交回"（好让改写不被当成另一个问题）。**两种失败模式不对称**：MISS 的代价是
一次本来就要发生的上游调用，而 WRONG HIT 是调用方看不见的缺陷——看起来快速又自信地答了一个
没人问的问题。所以下面每一条规则都在"身份不确定就不缓存、存下的答案可能属于别的问题就不返回"
这一侧收口（源码注释观点）。

### `type chatMessage struct` (internal/gateway/cachereq.go)

```go
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
}
```

- 作用：缓存推理所关心的一条消息的子集。`Content` 保持 raw，因为 OpenAI 的 content 既可能是
  字符串也可能是 parts 数组，而 tool 消息带的是 `tool_call_id` 而不是散文。
- 读写状态：只读的纯数据载体，不缓存、不加锁。
- 错误路径：无（结构体本身没有错误面）。
- 并发：值类型，按值传递进 `conversationPrompt`、`messagesFingerprint`。
- 调用关系：被 `cacheFields.Messages` 持有；被 `lastQuestionIndex`、`messagesFingerprint`、
  `conversationPrompt` 消费。

### `type cacheFields struct` (internal/gateway/cachereq.go)

```go
type cacheFields struct {
	Model               string          `json:"model"`
	Stream              bool            `json:"stream"`
	Prompt              json.RawMessage `json:"prompt"`
	Messages            []chatMessage   `json:"messages"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	N                   *int            `json:"n"`
	Seed                *int            `json:"seed"`
	Stop                json.RawMessage `json:"stop"`
	Tools               json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ResponseFormat      json.RawMessage `json:"response_format"`
	User                string          `json:"user"`
}
```

- 作用：缓存专用的、比 `parsedRequest` 更宽的请求体解码。所有能改变答案的字段都在这里列全，
  包括网关在别处从不解释的 `temperature`、`top_p`、`seed`、`stop`。
- 关键约束（源码注释观点，原文级）：**这个结构体里漏掉的参数，就是两个真正不同的请求会共享
  同一个 key 的那个参数**——这正是错误命中产生的方式。
- 读写状态：只在 `cacheIdentityFor` 内局部构造与读取，不逃逸到 `record`。
- 错误路径：`json.Unmarshal(body, &f)` 的失败在 `cacheIdentityFor` 里被转成
  `"body did not parse: " + err.Error()`。
- 并发：栈上局部值，无锁。
- 调用关系：被 `cacheIdentityFor` 解码，随后传给 `answerSignature`、`conversationPrompt`。

### `type cacheIdentity struct` (internal/gateway/cachereq.go)

```go
type cacheIdentity struct {
	Eligible bool
	Reason   string

	Scope     string
	ExactKey  string
	Signature string
	Prompt    string
	Model     string
	Tenant    string

	HasTools         bool
	Nondeterministic bool
	Bypass           bool
	Refresh          bool
}
```

- 作用：一次请求在缓存眼里的一切，**只算一次**，这样查找、存储、响应头三者不可能互相矛盾。
- 字段语义：`Eligible=false` 表示这条请求绝不能碰缓存，`Reason` 说明原因。`Reason` 是被上报的
  （响应头、日志）而不是被吞掉的，因为"为什么没缓存"是运维问的第一个问题，而静默跳过与缓存
  坏掉无法区分（源码注释观点）。
- 读写状态：`cacheIdentityFor` 按值返回，`prepareCache` 取地址存进 `rec.cacheIdent`，
  之后 `cacheAttempt`、`storeCache` 通过 `*rec.cacheIdent` 读；`serveCached` 通过 `res.Entry`
  写 `rec.cacheStatus` / `rec.servedFromCache`。
- 错误路径：结构体自身无错误；不合法状态用 `Eligible=false` + `Reason` 表达。
- 并发：只在请求 goroutine 内被读写，无锁。
- 调用关系：生产者 `cacheIdentityFor`；消费者 `cacheRequest`（投影成 `cache.Request`）、
  `cacheAttempt`、`storeCache`、`replayCachedStream` 路径上的 `relayStream` 捕获判断。

### `cacheIdentityFor (internal/gateway/cachereq.go)`

```go
func cacheIdentityFor(r *http.Request, body []byte, parsed parsedRequest) cacheIdentity
```

- 作用：从一个请求行、它读到的头、以及请求体推导出缓存身份。它是这些输入的**纯函数**，
  这正是这些规则可以在没有 store 的情况下被测试的原因。
- 输入约束（源码注释观点）：`body` 必须是**客户端发来的** body，在任何模型改写之前。
  `rewriteModel` 会用排序后的 key 重新 marshal JSON，之后算出的精确键会让两个只是模型名大小写
  拼写不同的相同请求得到不同的键。
- 判定顺序与错误字符串原文：
  1. `!isCompletionPath(r.URL.Path)` → `ident.Reason = "path is not a completion route"`，返回。
     （源码注释观点：embedding 与模型列表的响应重算很便宜，向量也不是一个答案，缓存它们等于为了
     省下比成本更少的东西而增加一个 store 和一次查找。）
  2. `!parsed.IsJSON` → `ident.Reason = "body is not JSON"`。
  3. `json.Unmarshal(body, &f)` 失败 → `ident.Reason = "body did not parse: " + err.Error()`。
  4. `f.Model == ""` 时回落到 `parsed.Model`。
  5. `conversationPrompt(f)` 返回 `ok=false` → `ident.Reason = "no user prompt to key on"`。
  6. 成功路径依次填：`Prompt`、`Model`、`Tenant = tenantFor(r)`、
     `HasTools = jsonPresent(f.Tools)`、`Nondeterministic`、`Signature = answerSignature(f, prefix)`、
     `Scope = cache.ScopeFor(ident.Tenant, f.Model, capabilitiesFor(r)...)`、
     `ExactKey = cache.ExactKeyFor(f.Model, body, r.URL.Path, ident.Tenant)`。
- 不确定性判定：`ident.Nondeterministic = (f.Temperature != nil && *f.Temperature > 0) || (f.TopP != nil && *f.TopP < 1)`。
  源码注释观点：temperature 0 意味着"后端能做到的最确定"，这是存下的答案仍然是那个答案的唯一
  体制；top_p 小于 1 是同一句话的另一种说法；其它一律是采样，而采样出来的是一个抽样，不是答案。
- 指令解析：读 `HeaderCache`，`strings.ToLower(strings.TrimSpace(...))` 后分派——
  `cacheDirectiveBypass` 置 `Bypass=true`，`cacheDirectiveRefresh` 置 `Refresh=true`，
  空串什么都不做，**其它任何取值被忽略而不是被猜**（源码注释观点：一个拼写错误绝不能悄悄变成
  "全部绕过"或"返回陈旧值"）。
- 读写状态：只读 `r`；返回值被 `prepareCache` 写入 `rec.cacheIdent`。
- 并发：无共享可变状态，可并发调用。
- 调用关系：被 `prepareCache` 调用（仅当 `p.cache != nil`）。

### `answerSignature (internal/gateway/cachereq.go)`

```go
func answerSignature(f cacheFields, prefix string) string
```

- 作用：给**每一个能改变答案的输入**做指纹，使得一个语义相近的 prompt 只会被"在同样条件下产生"
  的条目服务。参与哈希的字段（按匿名结构体的字段序）：`Model`、`Stream`、`Temperature`、`TopP`、
  `MaxTokens`、`MaxCompletionTokens`、`N`、`Seed`、`Stop`、`Tools`、`ToolChoice`、`ResponseFormat`、
  `Prefix`。`json.Marshal` 后取 `sha256.Sum256`，返回小写 hex 全串。
- 为什么前缀整段入哈希（源码注释观点）：不是只哈希 system 消息。Agent 循环里同一个问题在不同
  tool 结果之后含义不同，只匹配最后一条 user 消息会大方地交回一份基于另一个世界状态算出的计划。
- 错误路径原文：`json.Marshal` 失败时返回 `"uncomputable:" + err.Error()`。源码注释观点：这只能
  发生在某个字段持有非法 JSON 时（而请求解码器本应已经拒绝），但一个算不出来的签名绝不能与任何
  真实签名相撞，所以要返回一个诚实的签名不可能等于的东西。
- 读写状态：纯函数，无副作用。
- 并发：安全。
- 调用关系：被 `cacheIdentityFor` 调用；结果经 `cacheRequest` 进入 `cache.Request.Signature`，
  由 `Lookup` 用来拒绝"形似而条件不同"的命中。

### `conversationPrompt (internal/gateway/cachereq.go)`

```go
func conversationPrompt(f cacheFields) (prompt, prefix string, ok bool)
```

- 作用：抽出"模型要回答的那个问题"以及"它之前一切"的哈希。
- 三条返回路径：
  1. `lastQuestionIndex(f.Messages) >= 0`：`prompt = messageText(f.Messages[idx].Content)`，
     `prompt == ""` 时返回 `("", "", false)`；否则
     `prefix = messagesFingerprint(f.Messages[:idx])`，返回 `(prompt, prefix, true)`。
  2. 否则 `jsonPresent(f.Prompt)` 为真（补全接口的 `prompt` 字段）：
     `prompt = messageText(f.Prompt)`，空则 `false`，否则返回 `(prompt, "", true)`——补全接口没有
     对话前缀，`prefix` 为空串。
  3. 都不满足：`("", "", false)`。
- 源码注释观点：prompt 是"模型必须回应的最后一条消息"，在 chat 里是最后一条 user 消息，在 tool
  循环里是最后一个 tool 结果。更早的消息是上下文不是问题本身：把它们哈希进 prefix 能让两个相同
  问题在不同对话里区分开，而查找本身仍可以自由地匹配问题的改写。
- 读写状态：纯函数。
- 并发：安全。
- 调用关系：被 `cacheIdentityFor` 调用（唯一调用点）。

### `lastQuestionIndex (internal/gateway/cachereq.go)`

```go
func lastQuestionIndex(msgs []chatMessage) int
```

- 作用：找最新的"正在被回答的内容"所在下标。
- 两轮扫描：第一轮从后往前找 `Role` 为 `"user"` 或 `"tool"` 且 `messageText(...) != ""` 的消息；
  第二轮（只有第一轮无果时）从后往前找 `Role == "assistant"` 且文本非空的，这个也算，因为它就是
  调用方要求续写的方式，但**只有在没有别的东西时**才认（源码注释观点）。都没有则返回 `-1`。
- 读写状态：纯函数。
- 并发：安全。
- 调用关系：被 `conversationPrompt` 调用。

### `messagesFingerprint (internal/gateway/cachereq.go)`

```go
func messagesFingerprint(msgs []chatMessage) string
```

- 作用：把消息**按发来的样子**哈希：role、content、tool_calls、tool_call_id，逐字段写入
  `sha256.New()`，字段之间写 `0x00` 分隔，每条消息末尾写 `0xff`；`len(msgs)==0` 时直接返回 `""`；
  否则返回 `hex.EncodeToString(h.Sum(nil))`。
- 源码注释观点：序列化而不是解释，意味着这段代码不认识的新字段仍然会改变指纹——偏向 miss，
  永不偏向错误命中。
- 读写状态：纯函数，使用局部 `hash.Hash`。
- 并发：安全（无共享状态）。
- 调用关系：被 `conversationPrompt` 调用。

### `messageText (internal/gateway/cachereq.go)`

```go
func messageText(raw json.RawMessage) string
```

- 作用：把一条消息内容压平成"人问的那段文字"。
- 分支：trim 后为空或等于 `"null"` 返回 `""`；首字符是 `"` 则按 JSON 字符串 `Unmarshal`，
  失败返回 `""`；首字符不是 `[` 也返回 `""`；是 `[` 则解成 `[]struct{ Type string \`json:"type"\`; Text string \`json:"text"\` }`，
  解失败返回 `""`；随后只拼接**文本部件**——`Type` 为 `""`、`"text"`、`"input_text"`、`"output_text"`
  的非空 `Text`，多个之间用 `'\n'` 连接。
- 源码注释观点：content 数组是多部件形式（text、image_url、input_text），只拼文本部分，因为用图片
  URL 构建的缓存键不是问题的文字。
- 读写状态：纯函数，用 `strings.Builder`。
- 并发：安全。
- 调用关系：被 `conversationPrompt`、`lastQuestionIndex` 调用。

### `tenantFor (internal/gateway/cachereq.go)`

```go
func tenantFor(r *http.Request) string
```

- 作用：决定这次请求的隔离命名空间。
- 三条路径：
  1. `HeaderTenant` 去空白后非空 → 原样返回该值（运维的显式覆盖）。
  2. 否则 `Authorization` 去空白后非空 → `sum := sha256.Sum256([]byte(normaliseCredential(auth)))`，
     返回 `"key-" + hex.EncodeToString(sum[:8])`（前 8 字节 = 16 个 hex 字符）。
  3. 否则返回 `"anonymous"`。
- 源码注释观点：两个不同凭证的调用方绝不能偶然共享缓存答案——同一个 prompt 可能是机密，共享
  scope 会让一个租户的成功请求变成另一个租户的数据泄露；凭证本身被哈希而不是被存储，因为缓存键
  是会被粘进issue 和仪表盘的东西。
- 读写状态：只读请求头，无副作用。
- 并发：安全。
- 调用关系：被 `cacheIdentityFor`、`admitQuota`、`idempotencyBegin`、`proxy.go` 的 `serve` 入口
  调用（`serve` 里 `rec.tenant == ""` 时补一次）。

### `normaliseCredential (internal/gateway/cachereq.go)`

```go
func normaliseCredential(auth string) string
```

- 作用：折叠认证方案的大小写。
- 逻辑：`scheme, rest, found := strings.Cut(auth, " ")`；`!found` 或 `scheme == "Bearer"` 直接返回
  `auth`；`strings.EqualFold(scheme, "Bearer")` 为真则返回 `"Bearer " + rest`；否则返回 `auth`。
- 源码注释观点：`"Bearer X"` 与 `"bearer X"` 是同一个凭证；HTTP 把 scheme 当作大小写不敏感、
  把 token 当作不透明，逐字哈希头会让同一个调用方只因为 HTTP 客户端把一个词大写了，就拿到两个
  缓存 scope 以及（自 M3 起）两份独立预算。只有已知 scheme 的大小写变体会被改写，所以规范拼写
  仍然推出它在此函数存在之前推出的那个键。
- 读写状态：纯函数。
- 并发：安全。
- 调用关系：被 `tenantFor` 调用。

### `capabilitiesFor (internal/gateway/cachereq.go)`

```go
func capabilitiesFor(r *http.Request) []string
```

- 作用：从请求上读出能力约束，作为 scope 的一部分。读 `HeaderCapabilities`，空白返回 `nil`；
  否则按 `,` 切分、逐段 trim、丢弃空段，返回非 nil 切片。
- 源码注释观点：能力约束不同的两个请求可能被路由到不同后端、因而得到不同答案，所以能力属于 scope。
- 读写状态：只读头。
- 并发：安全。
- 调用关系：被 `cacheIdentityFor` 调用（`cache.ScopeFor(tenant, model, capabilitiesFor(r)...)`）。

### `jsonPresent (internal/gateway/cachereq.go)`

```go
func jsonPresent(raw json.RawMessage) bool
```

- 作用：判断一个 raw JSON 字段是否**真的被提供了**。实现：`s := strings.TrimSpace(string(raw)); return s != "" && s != "null"`。
- 源码注释观点：解码器把缺失字段留成 nil、把显式 null 留成四个字节 `"null"`，两者都表示"没给"。
- 读写状态：纯函数。
- 并发：安全。
- 调用关系：被 `cacheIdentityFor`（判 `HasTools`）与 `conversationPrompt`（判补全接口的 `prompt`）调用。

---

## 4. 缓存读写路径：`internal/gateway/cachepath.go`

文件头注释给出的总纲：缓存位于**解析之后、路由之前**，这个顺序就是全部要点——命中意味着没有
东西要路由、没有后端要选、没有 token 要付。因此这里的一切都在关键路径上、必须便宜，而且它的
每一种失败都必须是非致命的：store 坏掉必须退化成"没有缓存"，绝不退化成"没有答案"
（源码注释观点）。

### `prepareCache (internal/gateway/cachepath.go)`

```go
func (p *Proxy) prepareCache(w http.ResponseWriter, r *http.Request, rec *record, body []byte, parsed parsedRequest)
```

- 作用：算出本次请求的缓存身份并上报判定结果。它在上游调用之前运行，这样即使判定是"不要用
  缓存"，响应头也能描述这个决定。
- 逻辑顺序：
  1. `p.cache == nil` 直接返回——**一个头都不设**（因此未装缓存时 M1 表面完全不变，
     测试 `TestCacheDisabledLeavesM1Behaviour` 钉住）。
  2. `ident := cacheIdentityFor(r, body, parsed)`；`rec.cacheIdent = &ident`。
  3. `!ident.Eligible`：若 `isCompletionPath(r.URL.Path)`，则 `rec.cacheStatus = cacheStatusSkip`、
     `rec.cacheReason = ident.Reason`、`w.Header().Set(HeaderCache, cacheStatusSkip)`，然后返回。
  4. 否则 `d := p.cache.Cacheable(cacheRequest(ident))`，按优先级落状态：
     `ident.Bypass` → `cacheStatus = cacheDirectiveBypass`；`ident.Refresh` →
     `cacheDirectiveRefresh`；`!d.Lookup` → `cacheStatusSkip`；否则 `cacheStatusMiss`。
     `rec.cacheReason = d.Reason`；当状态**不是** `miss` 时把状态写进 `HeaderCache`。
- 源码注释观点：策略判定在这里算而不是在 `Lookup` 内部算，是为了让响应头能在任何 store 或
  embedder 被触碰之前就带上它。
- 读写状态：写 `rec.cacheIdent`、`rec.cacheStatus`、`rec.cacheReason` 与响应头；只读 `p.cache`。
- 错误路径：本函数不产生错误（`Cacheable` 不返回 error）；不可缓存的事实通过 `skip` + `Reason` 表达。
- 并发：只在请求 goroutine 内跑；`p.cache` 自身线程安全（由 `internal/cache` 保证）。
- 调用关系：被 `serve` 调用，紧接在 `idempotencyBegin` 之后、`cacheAttempt` 之前，且仅当
  `p.cache != nil`。

### `cacheAttempt (internal/gateway/cachepath.go)`

```go
func (p *Proxy) cacheAttempt(w http.ResponseWriter, r *http.Request, rec *record) bool
```

- 作用：能用缓存答就用缓存答；返回 true 表示响应已经完整，调用方**不得**再调上游。
- 早退条件：`p.cache == nil || rec.cacheIdent == nil || !rec.cacheIdent.Eligible` → false；
  `rec.cacheStatus != cacheStatusMiss`（被策略或显式指令跳过）→ false，没有东西可读。
- 主路径：`res, err := p.cache.Lookup(r.Context(), cacheRequest(*rec.cacheIdent))`。
  - `err != nil`：`rec.cacheStatus = cacheStatusError`、`rec.cacheReason = err.Error()`、
    `w.Header().Set(HeaderCache, cacheStatusError)`，并 `p.log.Debug("cache lookup failed", ...)`
    带 `slog.String("request_id", rec.requestID)` 与 `slog.String("error", err.Error())`，返回 false。
  - `!res.Hit`：`rec.cacheReason = res.Reason`、`w.Header().Set(HeaderCache, cacheStatusMiss)`，返回 false。
  - 命中：`p.serveCached(w, r, rec, res)` 后返回 true。
- 源码注释观点：缓存故障不是请求故障。响应头必须说明这一点，否则运维看到的是一个正常工作的网关
  加上一个没被用过的缓存，却没有任何线索指出两者哪个有问题。
- 读写状态：写 `rec.cacheStatus`、`rec.cacheReason`；命中时进一步写 `rec`（见 `serveCached`）。
- 错误路径：错误被**吞成一个状态值**（`error` + `miss`），请求继续走上游——这是"store 坏掉退化为
  无缓存"的具体实现。
- 并发：请求 goroutine 内；`Lookup` 的阻塞发生在 `r.Context()` 上，客户端断开时可被取消。
- 调用关系：被 `serve` 调用（`if p.cacheAttempt(w, r, rec) { return }`）。

### `serveCached (internal/gateway/cachepath.go)`

```go
func (p *Proxy) serveCached(w http.ResponseWriter, r *http.Request, rec *record, res cache.Result)
```

- 作用：把一个存下的答案写给客户端。
- 状态写入：`rec.cacheStatus = cacheStatusHitExact`，若 `res.Kind == cache.KindSemantic` 改为
  `cacheStatusHitSemantic`；`rec.servedFromCache = true`；`rec.status = e.Status`（0 时回落
  `http.StatusOK`）；`rec.outcome = metrics.OutcomeSuccess`；`rec.model = e.Model`；
  `rec.upstream = cacheStatusUpstream`（即 `"cache"`）；`rec.respBytes = int64(len(e.Body))`；
  `rec.usage = Usage{Prompt: e.PromptTokens, Completion: e.CompletionTokens}`。
- 为什么 `rec.usage` 照填却不算上游消耗（源码注释观点，原文级）：这是**存下的答案自己报告的用量**，
  让请求日志仍能显示答案里有什么（以及它本来会花多少）；它**刻意不加进上游 token 计数器**——那些是
  provider 的事实，只由 `proxy.go` 里关闭一次真实尝试的 `ObserveTokens` 观测，因为把一次重放计入
  会让消耗量恰好按缓存省下的量虚增。
- `rec.cacheReason` 用 `fmt.Sprintf` 生成，两条格式串原文：
  `"semantic match %.4f from %s, age %s"`（`res.Kind == cache.KindSemantic`，
  `res.Similarity`、origin、`res.Age.Round(time.Millisecond)`）与
  `"exact match from %s, age %s"`。`origin := e.Upstream`，为空时取 `"unknown"`。
- 响应头：`HeaderCache`（=`rec.cacheStatus`）、`HeaderCacheAge`（`strconv.FormatInt(res.Age.Milliseconds(), 10)`）、
  `HeaderUpstreamName = cacheStatusUpstream`，以及 `rec.requestID != ""` 时的 `HeaderRequestID`。
- 分发：`isEventStream(e.ContentType)` 为真 → `p.replayCachedStream(w, rec, e)` 并返回；
  否则 `Content-Type` 空时补 `"application/json"`，`w.WriteHeader(rec.status)`，
  `w.Write(e.Body)` 失败则 `rec.reason = "client write: " + err.Error()`。
- 读写状态：大量写 `rec`；读 `res.Entry`。
- 并发：请求 goroutine；写客户端由 `http.ResponseWriter` 自身串行保证。
- 调用关系：被 `cacheAttempt` 调用（命中时）。

### `replayCachedStream (internal/gateway/cachepath.go)`

```go
func (p *Proxy) replayCachedStream(w http.ResponseWriter, rec *record, e cache.Entry)
```

- 作用：把存下的流写给一个要求流式的客户端。存下的字节被**重新解析成帧并一帧一帧重发**。
- 为什么逐帧而不是整块写（源码注释观点）：整块写在 TCP 层也能工作（SSE 分帧不依赖包边界），
  但它会一次吐出一大团，而流的意义就是调用方能在结束前开始读。帧走的是与实时流相同的 reader 与
  writer，所以重放的答案与真实答案**不可区分**。
- 为什么不写首 token 直方图（源码注释观点，原文级）：重放不重跑模型，所以没有首 token 可计时；
  这个直方图被**留着不动**，而不是填一个接近 0 的样本。把一次重放混进一个本该描述生成的延迟分布，
  会让这个指标恰好在缓存用得最多的时候"变好"，而这与"有信息量"正好相反。
- 逻辑：`sw, err := sse.NewWriter(w)`；出错时 `p.log.Warn("cache: streaming unavailable for a cached answer", ...)`，
  设 `Content-Type` 为 `sse.ContentType`，`w.WriteHeader(rec.status)`，`_, _ = w.Write(e.Body)` 后返回
  （源码注释观点：缓存治不好一个不会流式的服务器；把存下的字节当普通 body 交出去，调用方至少还能
  一次拿到帧并解析，而 500 会彻底丢掉答案）。成功则 `sw.MirrorSSEHeader()`，显式
  `w.Header().Set("Content-Type", sse.ContentType)`，`sw.WriteHeader(rec.status)` 失败即
  `rec.reason = "client write: " + err.Error()` 并返回。
- 重放循环：`reader := sse.NewReader(bytes.NewReader(e.Body))`；`reader.NextFrame()` 出错时，
  非 EOF（`!isStreamEOF(rerr)`）记 `rec.reason = "cached stream replay: " + rerr.Error()`，然后 break；
  `sw.WriteUpstream(frame)` 失败记 `rec.reason = "client write: " + werr.Error()` 并返回；成功则
  `rec.frames++`。循环结束 `rec.respBytes = sw.BytesWritten()`。
- 为什么 `MirrorSSEHeader()` 之后还要显式设 Content-Type（源码注释观点）：重放时**没有活的
  上游响应可镜像**，不显式设置的话帧会以无类型 body 到达，SSE 客户端根本不会把答案当流处理。
- 并发：请求 goroutine；`rec.frames`、`rec.respBytes` 就地累加。
- 调用关系：被 `serveCached` 在 `isEventStream(e.ContentType)` 时调用。

### `storeCache (internal/gateway/cachepath.go)`

```go
func (p *Proxy) storeCache(ctx context.Context, rec *record, body []byte, contentType string)
```

- 作用：把一个已经交付的答案写进缓存。
- 时序（源码注释观点）：它在响应写给客户端**之后**被调用，所以一个慢的 store（Redis 往返、
  一次 embedding 调用）永远不可能拖慢调用方；答案无论如何都已经交付，唯一悬而未决的只是下一个
  相同请求便不便宜。
- 早退：`p.cache == nil || rec.cacheIdent == nil || !rec.cacheIdent.Eligible` → 返回；
  `len(body) == 0` → 返回。
- 写入内容：`ident := *rec.cacheIdent`（值拷贝），构造
  `cache.Entry{Body: body, Status: http.StatusOK, ContentType: contentType, Upstream: rec.upstream, PromptTokens: rec.usage.Prompt, CompletionTokens: rec.usage.Completion}`；
  调 `p.cache.StoreResponse(ctx, cacheRequest(ident), entry)`。
- 错误路径原文：`err != nil` 时 `rec.cacheReason = "store failed: " + err.Error()`，
  并 `p.log.Debug("cache store failed", slog.String("request_id", rec.requestID), slog.String("error", err.Error()))`。
  注意它**不改 `cacheStatus`**，因为答案已经交付，store 失败只解释"下一个请求为什么没命中"。
- 调用方传入的 `ctx`：缓冲路径传 `r.Context()`（`copyWhole`），流式路径传 `r.Context()`
  （`relayStream` 尾部）。这一点有实际后果：客户端在写完响应后立刻断开时，写入可能被取消。
- 并发：`*rec.cacheIdent` 是值拷贝后使用，避免与后续写者共享；`p.cache.StoreResponse` 由缓存
  包保证线程安全。
- 调用关系：被 `copyWhole`（仅 `resp.StatusCode == http.StatusOK` 时）与 `relayStream`
  （流干净结束且未超限时）调用。

### `cacheRequest (internal/gateway/cachepath.go)`

```go
func cacheRequest(ident cacheIdentity) cache.Request
```

- 作用：把身份投影成缓存包自己的请求类型。
- 字段映射：`Scope`、`Model`、`Prompt`、`ExactKey`、`Signature`、`HasTools`、`Nondeterministic`、
  `Bypass`、`Refresh` 一一对应。
- 读写状态：纯函数。
- 并发：安全。
- 调用关系：被 `prepareCache`（喂 `Cacheable`）、`cacheAttempt`（喂 `Lookup`）、`storeCache`
  （喂 `StoreResponse`）调用——**三处共用同一个投影**，这是"查找、存储、响应头不可能互相矛盾"的
  实现方式。

---

## 5. 配额：`internal/gateway/quotapath.go`

文件头注释给出的总纲（源码注释观点）：记账住在 `internal/quota`，这个文件是接缝，它只回答三个
问题——这次请求能不能过、以什么形态过（`admitQuota`）；它实际花了多少（`settleQuota`）；调用方
被告知什么（响应头与 429 信封）。**顺序即设计**：准入排在缓存查找之前，因为一次由缓存服务的请求
**仍然是请求**——它占一个每分钟槽位，必须出现在租户报表里，即使它不花 provider token；结算放在
`ServeHTTP` 的 deferred 块里，意味着每条被放行的请求在**任意**退出路径（含 panic）上恰好结算一次。
预留是刻意的**估算**、事后修正：只按可测量值收费的网关会让一个租户在预算为 1 的情况下开出上千条
并发流，因为响应回来之前什么都测不到；先预留上限、结算时退回未用部分，才是预算在并发下守得住的
原因。

### `QuotaGate (internal/gateway/quotapath.go)`

```go
type QuotaGate interface {
	Enabled() bool
	Admit(ctx context.Context, tenant, session string, est quota.Estimate) (*quota.Decision, error)
	Settle(ctx context.Context, res *quota.Reservation, usage quota.Usage) error
	Release(ctx context.Context, res *quota.Reservation) error
}
```

- 作用：网关咨询的准入控制器。四个方法各自独立：
  - `Enabled() bool`：预算是否被强制。源码注释观点：禁用的 gate 永不被咨询，所以它的存在不产生
    每请求成本。
  - `Admit(ctx context.Context, tenant, session string, est quota.Estimate) (*quota.Decision, error)`：
    为请求预留预算。源码注释观点：**非 nil 的 error 永远意味着 store 读不到**，而 Decision 仍然说明
    该怎么办，因为 fail-open 与 fail-closed 都是合法配置。
  - `Settle(ctx context.Context, res *quota.Reservation, usage quota.Usage) error`：按真实用量收费
    并退回 `Admit` 多预留的部分。
  - `Release(ctx context.Context, res *quota.Reservation) error`：原样退回预留。源码注释观点：这是
    "从未到达 provider 的请求"结算所用的方法。
- 为什么是接口而不是直接用 `*quota.Manager`（源码注释观点）：让网关自己的测试能在没有 Redis、
  没有 embedder、没有时钟的情况下驱动每一个分支——allowed、degraded、refused、store failure。
- 读写状态/并发：无实现；线程安全由实现方（`internal/quota`）负责。

### `var _ QuotaGate = (*quota.Manager)(nil)` (internal/gateway/quotapath.go)

- 作用：编译期断言 manager 满足网关的接缝。源码注释观点：两侧任何一侧漂移都会变成编译失败，
  而不是一次部署里的第一个请求才炸。

### `defaultQuotaCharsPerToken (internal/gateway/quotapath.go)`

- `defaultQuotaCharsPerToken = 4`：把入站 body 换算成 prompt token 估算。源码注释观点：4 是英文
  文本与 JSON 框架的常用经验值；CJK 每字符更贵，这正是它是一个旋钮的原因。

### `defaultQuotaCompletionTokens (internal/gateway/quotapath.go)`

- `defaultQuotaCompletionTokens = 256`：调用方没有给出自己的上限时的 completion 预留。

### `quotaSettleTimeout (internal/gateway/quotapath.go)`

- `quotaSettleTimeout = 2 * time.Second`：给响应之后的记账定界。源码注释观点：跑这段时客户端已经
  被答复了，所以慢 store 不得扣住一个 handler goroutine（或一次停机）。

### `admitQuota (internal/gateway/quotapath.go)`

```go
func (p *Proxy) admitQuota(w http.ResponseWriter, r *http.Request, rec *record, body []byte, parsed parsedRequest) ([]byte, parsedRequest, bool)
```

- 作用：为请求预留预算并执行判定。返回（可能被改写的）body 与它的重新解析，以及调用方能否继续；
  一次拒绝**已经写进 w**。
- 提前返回：`p.quota == nil || !p.quota.Enabled()` → `return body, parsed, true`。
- 租户与会话：`tenant := tenantFor(r)`；`session := strings.TrimSpace(r.Header.Get(HeaderSession))`；
  `rec.tenant = tenant`；`session != ""` 时 `rec.session = session`。
- `dec == nil` 兜底：`dec = &quota.Decision{Allowed: false, Action: quota.ActionReject, Reason: quota.ReasonStoreError}`。
  源码注释观点：一个什么都不返回的 gate 是这段代码唯一无法解释的失败模式，所以按 store 失败处理，
  而不是当成同意。
- **fail-closed（默认）**：`!dec.Allowed && dec.Reason == quota.ReasonStoreError` 时，`err == nil`
  就先 `err = errQuotaStore`，然后 `rec.status = http.StatusServiceUnavailable`、
  `rec.outcome = metrics.OutcomeInternal`、`rec.reason = "quota store error"`、`setQuotaHeaders(w, dec)`、
  `p.log.Error("quota: store unavailable, refusing request", "request_id", rec.requestID, "tenant", tenant, "error", err.Error())`、
  `writeError(w, http.StatusServiceUnavailable, TypeQuotaStore, "quota accounting is unavailable, so the request was not sent upstream")`，
  返回 `(body, parsed, false)`。源码注释观点：读不出来的限额不是限额。
- **fail-open**：同一个 reason，但 `dec.Allowed` 为真时只记
  `p.log.Warn("quota: store unavailable, admitting under fail_open", ...)` 然后继续走正常流程。
  这就是 fail-open 的抵达方式——`Allowed=true` 与 `ReasonStoreError` 同时成立。
- 正常记账：`rec.quotaAction = dec.Action`、`rec.quotaReason = dec.Reason`、
  `rec.quotaLimit, rec.quotaUsed, rec.quotaRequested = dec.Limit, dec.Used, dec.Requested`，
  然后 `setQuotaHeaders(w, dec)`（对每一条被治理的请求都设，不只对拒绝）。
- **429**：`rec.status = http.StatusTooManyRequests`、`rec.outcome = metrics.OutcomeRateLimited`、
  `rec.reason = "quota exceeded: " + dec.Reason`；`seconds := 1`，`dec.RetryAfter > 0` 时
  `seconds = int(math.Ceil(dec.RetryAfter.Seconds()))` 且 `seconds < 1` 时回到 `1`，然后
  `w.Header().Set("Retry-After", strconv.Itoa(seconds))`、
  `writeError(w, http.StatusTooManyRequests, TypeQuotaExceeded, quotaMessage(tenant, dec))`，返回 false。
  源码注释观点：永远给一个退避提示，即使 decision 没带窗口——不带 `Retry-After` 的 429 会让每个 SDK
  自己发明一个，而发明出来的通常是"立刻重试"。
- **放行**：`rec.quotaRes = dec.Reservation`——只对放行的请求存，因为一次拒绝的预留已经在 manager
  内部释放了（源码注释观点）；`dec.Action != quota.ActionDegrade` 时直接 `return body, parsed, true`。
- **degrade 第一级（换模型）**：`dec.DowngradeModel != "" && !strings.EqualFold(dec.DowngradeModel, parsed.Model)`
  时调 `rewriteModel(body, dec.DowngradeModel)`；失败 → `rec.quotaReason = dec.Reason + "; downgrade failed: " + rerr.Error()`
  并 `p.log.Warn("quota: could not rewrite the model for a degraded request", "request_id", rec.requestID, "tenant", tenant, "error", rerr.Error())`；
  成功 → `body = rewritten`、`parsed = inspectRequest(body)`、`rec.model = parsed.Model`、
  `rec.degradedModel = dec.DowngradeModel`。
- **degrade 第二级（压上限）**：`dec.MaxTokensCap > 0` 时调 `rewriteMaxTokens(body, dec.MaxTokensCap)`；
  失败 → `rec.quotaReason = dec.Reason + "; max_tokens cap failed: " + rerr.Error()` 与
  `p.log.Warn("quota: could not cap max_tokens for a degraded request", ...)`；成功 → 同样重写
  `body`/`parsed`。源码注释观点：两个杠杆都是可选的，且都作用在操作员即将发给上游的那份 body 上，
  所以真正少花钱的是 provider，而不是网关。
- 错误路径：两条 rewrite 失败都只降级为 reason 与一条 Warn，请求继续——degrade 的目标是继续服务，
  只是小一点。
- 并发/读写：请求 goroutine；写 `rec` 的配额字段，读 `p.quota`、`p.pricing`。

### `quotaEstimate (internal/gateway/quotapath.go)`

```go
func (p *Proxy) quotaEstimate(parsed parsedRequest, body []byte) quota.Estimate
```

- 作用：把一个请求变成它需要的预留（租约）。
- 规则：`charsPerToken := p.quotaCharsPerToken`，`<= 0` 时取 `defaultQuotaCharsPerToken`；
  `prompt := len(body) / charsPerToken`，`< 1` 时取 1；`completion := p.quotaCompletionTokens`，
  `<= 0` 时取 `defaultQuotaCompletionTokens`；`parsed.MaxTokens > 0` 时 `completion = parsed.MaxTokens`；
  `p.pricing != nil` 时 `costMicros = int64(math.Round(p.pricing.CostUSD(parsed.Model, prompt, completion) * 1e6))`；
  返回 `quota.Estimate{PromptTokens: prompt, CompletionTokens: completion, CostMicros: costMicros, Requests: 1}`。
- 源码注释观点（load-bearing）：调用方自己给的上限比任何常数都更接近真实上界，而预留得比请求可能
  花掉的更少，会让一次突发穿过一个名义上已经耗尽的预算；估算在结算时被修正——**这是租约，不是收费**。
- 错误路径：无（整数运算；`p.pricing` 为 nil 时成本为 0）。
- 并发：只读 `p.quotaCharsPerToken`、`p.quotaCompletionTokens`、`p.pricing`，可并发调用。

### `settleQuota (internal/gateway/quotapath.go)`

```go
func (p *Proxy) settleQuota(ctx context.Context, rec *record)
```

- 作用：在每条退出路径上结束 `Admit` 开的租约。
- 早退：`p.quota == nil || rec.quotaRes == nil`。
- **context 的关键一步**：`sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), quotaSettleTimeout)`
  再 `defer cancel()`。源码注释观点：它跑在响应交付之后、挂在一个与调用方脱钩的 context 上——
  **中途断开的客户端仍然必须被记账**，而结算是唯一不能因为一个 socket 关闭就被跳过的写。
- 三分支：
  1. `rec.servedFromCache, rec.servedFromReplay` → `p.finishQuota(sctx, rec, quota.Usage{Requests: 1})`。
     源码注释观点：重放的答案不花 provider 一分钱，所以只结算请求数；这也正是准入排在缓存与幂等
     存储之前的原因——**命中是免费的，但它不是不可见的**。
  2. `rec.attempts == 0` → `p.quota.Release(sctx, rec.quotaRes)`，`err != nil` 时
     `rec.quotaSettleError = err.Error()`。源码注释观点：什么都没到达 provider（模型不可路由、body
     被网关拒绝、调用方先消失了），全部退回，**包括那个槽位**。
  3. default → `usage := quota.Usage{PromptTokens: rec.usage.Prompt, CompletionTokens: rec.usage.Completion, Requests: 1}`，
     `p.pricing != nil` 时补 `usage.CostMicros = int64(math.Round(p.pricing.CostUSD(rec.model, rec.usage.Prompt, rec.usage.Completion) * 1e6))`，
     再 `p.finishQuota(sctx, rec, usage)`。源码注释观点：被中途切断的流不上报用量，所以预留被**退回**
     而不是被收费——网关无法知道 provider 对一次部分生成收了什么，凭空造一个数会污染操作员用来与
     provider 账单对账的那唯一一个数字；这次退回在 `Stats.ReleasedTokens` 里可见。
- 错误路径：所有错误都落到 `rec.quotaSettleError`，不改变响应（响应早就交付了）。
- 并发：请求 goroutine，但 context 已与客户端脱钩，所以它的写不随取消而消失。

### `finishQuota (internal/gateway/quotapath.go)`

```go
func (p *Proxy) finishQuota(ctx context.Context, rec *record, usage quota.Usage)
```

- 作用：调 `p.quota.Settle(ctx, rec.quotaRes, usage)`；`err != nil` 时 `rec.quotaSettleError = err.Error()`。
- 读写状态：写 `rec.quotaSettleError`，读 `rec.quotaRes`。错误路径：如上，被吞成字段，不抛给调用方
  （响应已经在线上了）。并发：只在 `settleQuota` 的两个分支里被调用。

### `setQuotaHeaders (internal/gateway/quotapath.go)`

```go
func setQuotaHeaders(w http.ResponseWriter, dec *quota.Decision)
```

- 作用：把判定描述给调用方。
- 逻辑：`action := dec.Action`，空串回落 `quota.ActionAllow`；`h.Set(HeaderQuota, action)`；
  `dec.Reason != ""` → `HeaderQuotaReason`；`dec.Limit > 0` → `HeaderQuotaLimit` 与 `HeaderQuotaUsed`
  （两个 `strconv.FormatInt(..., 10)`）；`dec.Action == quota.ActionDegrade` 时
  `dec.DowngradeModel != ""` → `HeaderQuotaModel`，`dec.MaxTokensCap > 0` → `HeaderQuotaMaxTokens`
  （`strconv.Itoa`）。
- 源码注释观点：它对**每一条被治理的请求**都设，而不只对拒绝——"放行了，这里是还剩多少预算"正是
  一个操作员能据以规划的限额与一个只能从 429 里发现的限额之间的差别。
- 错误路径：无；并发：请求 goroutine 写响应头。

### `quotaMessage (internal/gateway/quotapath.go)`

```go
func quotaMessage(tenant string, dec *quota.Decision) string
```

- 作用：用调用方能据以行动的说法解释一次拒绝。
- 拼接顺序：`"quota exceeded for tenant "` + `strconv.Quote(tenant)` + `": "` + `dimensionLabel(dec.Reason)`；
  `dec.Limit > 0` 时追加 `" budget of "` + Limit + `" is already at "` + Used + `", and this request asked for "` + Requested；
  `dec.RetryAfter > 0` 时追加 `"; the window resets in "` + `dec.RetryAfter.Round(time.Second).String()`。
- 源码注释观点：维度、限额、用量都被点名；只看到 "quota exceeded" 的操作员得自己去查是四个预算里的
  哪一个，而那一分钟他正好会用来问网关的负责人。
- 错误路径：无（纯字符串拼装）；并发：安全。

### `dimensionLabel (internal/gateway/quotapath.go)`

```go
func dimensionLabel(reason string) string
```

- 映射原文：`quota.ReasonDailyTokens` → `"daily token"`；`quota.ReasonDailyCost` → `"daily spend"`；
  `quota.ReasonSessionTokens` → `"per-session token"`；`quota.ReasonMinuteRPM` → `"per-minute request"`；
  `quota.ReasonStoreError` → `"accounting"`；`default` → `reason` 原样返回。
- 作用：把机器 reason 翻成人读的短语；未知 reason 不被吞掉，原样透出。

### `errQuotaStore (internal/gateway/quotapath.go)`

- `var errQuotaStore = quotaStoreError("quota: store unavailable")`：替一个失败但没说原因的 gate 站台；
  只在 fail-closed 分支里被用作 `err` 的填充值（`err.Error()` 会进日志）。

### `type quotaStoreError string` (internal/gateway/quotapath.go)

```go
type quotaStoreError string

func (e quotaStoreError) Error() string { return string(e) }
```

- 作用：一个字符串型错误，让上面那个哨兵不必额外引入 errors 包；值语义（不可变），可安全并发使用。

---

## 6. 幂等：`internal/gateway/idempotency.go`

文件头注释给出的位置理由（源码注释观点）：认领发生在**准入之后、缓存之前**。在准入之后，因为
一次重放是调用方自己发起的请求，它和缓存命中一样占一个每分钟槽位，却不花 provider token；在缓存
之前，因为对一次**已完成**调用的重放是比缓存命中更强的声明——调用方问的是它自己的那次操作，不是
一个别人已经问过的相似问题。

### `idempotencyBegin (internal/gateway/idempotency.go)`

```go
func (p *Proxy) idempotencyBegin(w http.ResponseWriter, r *http.Request, rec *record, body []byte) (http.ResponseWriter, bool)
```

- 作用：认领调用方的 `Idempotency-Key` 并判定这次请求是首次尝试、已完成的某次的重放，还是冲突。
  返回后续管线必须使用的 writer（本次拥有该 key 时是录制包装）以及响应是否已经写完整。
- 提前返回：`p.idempotency == nil` → `(w, false)`；`raw := strings.TrimSpace(r.Header.Get(HeaderIdempotencyKey))`
  为空 → `(w, false)`；`key := idempotency.KeyOf(raw)` 为空 → `(w, false)`。
- 非补全路径：`!isCompletionPath(r.URL.Path)` → `rec.idemKey = key`、`rec.idemDecision = idempotentSkip`、
  `rec.idemReason = "path has no replayable response"`、`w.Header().Set(HeaderIdempotentStore, idempotentSkip)`，
  返回 `(w, false)`。源码注释观点：给一个清单接口或健康检查带 key 没有任何东西可以重放；**忽略它
  而不是拒绝**，能让一个给每个请求都盖 key 的客户端继续工作，而响应说明了这件事，所以这个无视不是
  静默的。（注意 `isCompletionPath` 把 `/embeddings` 也算作补全路径，所以 embed 请求是会被占用 key 的。）
- scope：`scope := rec.tenant`，为空时 `scope = tenantFor(r)` 并回写 `rec.tenant = scope`；随后
  `rec.idemKey = key`、`rec.idemScope = scope`、`rec.idemHash = requestHash(r.Method, r.URL.Path, body)`，
  再 `dec, entry := p.idempotency.Begin(scope, key, rec.idemHash)`。
- `idempotency.Proceed`：`rec.idemClaimed = true`、`rec.idemDecision = idempotency.Proceed.String()`、
  `w.Header().Set(HeaderIdempotencyKey, key)`、`w.Header().Set(HeaderIdempotentReplay, idempotentFalse)`，
  再 `rec.idemCapture = newCaptureWriter(w, p.idempotency.MaxResponseBytes()+1)`，返回 `(rec.idemCapture, false)`。
  源码注释观点：两个头都在 body 之前就可得知；回显 key 让只读一个流第一个字节的调用方也知道它的
  重试会是一次重放；replay 头显式设成 `"false"`，让调用方能区分"这次尝试干了活"与"你拿到的是上一次
  的答案"，而不必从一个缺失的头去推断；捕获上限是 store 限额**再多一个字节**——writer 必须知道答案
  没装下，而按限额截断的缓冲会交给 store 一个被截断、却看起来仍然足够小、值得记住的 body。
- `idempotency.Replay`：`rec.idemDecision = idempotency.Replay.String()`、`p.serveReplay(w, r, rec, entry)`，
  返回 `(w, true)`。
- `idempotency.ConflictBody`：`rec.idemDecision = idempotency.ConflictBody.String()`、
  `rec.idemReason = "this key is already recorded against a different request body"`、
  `rec.status = http.StatusConflict`、`rec.outcome = metrics.OutcomeBadRequest`、
  `rec.reason = "idempotency conflict"`、`w.Header().Set(HeaderIdempotentReplay, idempotentFalse)`，
  再 `writeError(w, http.StatusConflict, TypeIdempotencyConflict, "idempotency key "+key+" was already used for a different request body")`，
  返回 `(w, true)`。
- `default`（`idempotency.ConflictInFlight`）：`rec.idemReason = "a request with this key is still in flight"`、
  `rec.status = http.StatusConflict`、`rec.outcome = metrics.OutcomeBadRequest`、
  `rec.reason = "idempotency in flight"`、`w.Header().Set(HeaderIdempotentReplay, idempotentFalse)`、
  `w.Header().Set("Retry-After", "1")`，再
  `writeError(w, http.StatusConflict, TypeIdempotencyInFlight, "a request with idempotency key "+key+" is already in flight")`，
  返回 `(w, true)`。源码注释观点：答 409 而不是等待或重复，正是这个 store 的意义——调用方可以稍后用
  **同一个** key 重试并拿到第一次尝试的答案，而与此同时 provider 只被要了一次生成，不是两次。
- 读写状态：写 `rec` 的幂等字段与响应头；读 `p.idempotency`。
- 错误路径：三条 4xx 都由 `writeError` 写成 `TypeIdempotencyConflict` / `TypeIdempotencyInFlight` 信封；
  本函数自身不产生 Go error 返回值。
- 并发：请求 goroutine；`Begin` 的原子性由 `internal/idempotency` 负责（本文件不自己加锁）。

### `serveReplay (internal/gateway/idempotency.go)`

```go
func (p *Proxy) serveReplay(w http.ResponseWriter, r *http.Request, rec *record, e idempotency.Entry)
```

- 作用：把录下的答案写给客户端。源码注释观点：存下的字节**原样**发出——这正是重放之所以是重放、
  而不是重新生成的原因；来源头说明它们从哪来。录下的流按**一个 body** 交付：转录就是答案，给它的帧
  重新计时会造出一个 provider 从未有过的生成速度。参数 `r` 在本函数体内没有被使用。
- 为什么与配额有关（源码注释观点，原文标注"M3"）：一次重放消耗了一个请求槽位、没有 provider token，
  和缓存命中完全一样，所以配额按同样方式结算——对应 `settleQuota` 的第一个分支。
- 状态写入：`rec.servedFromReplay = true`；`rec.status = e.Status`（`0` 时回落 `http.StatusOK`）；
  `rec.outcome = metrics.OutcomeSuccess`；`rec.model = e.Model`；`rec.stream = e.Stream`；
  `rec.upstream = idempotencyUpstream`（`"replay"`）；`rec.respBytes = int64(len(e.Body))`。
  源码注释观点：**最初产生这个答案的 provider 不是服务这次请求的那个**。
- 响应头：`HeaderIdempotentReplay = idempotentTrue`、`HeaderIdempotencyKey = e.Key`、
  `HeaderUpstreamName = idempotencyUpstream`；`e.RequestID != ""` → `HeaderIdempotentOrigin`；
  `e.Upstream != ""` → `HeaderIdempotentUpstream`（源码注释观点：真正干活的 backend 需要一个自己的头，
  因为上面的 upstream 名头现在报的是 `"replay"`）；`!e.CompletedAt.IsZero()` 时算
  `age := time.Since(e.CompletedAt)`，`age < 0` 时置 0，再设 `HeaderIdempotentAge` 为
  `strconv.FormatInt(age.Milliseconds(), 10)`；`rec.requestID != ""` → `HeaderRequestID`。
- body 交付：`contentType` 取自 `http.Header(e.Headers).Get("Content-Type")` 并 trim，空则回落
  `"application/json; charset=utf-8"`；`w.Header().Set("Content-Type", contentType)`、
  `w.WriteHeader(rec.status)`；`len(e.Body) > 0` 时 `w.Write(e.Body)`，写失败则
  `rec.reason = "client write: " + err.Error()` 并返回。
- 尾部：`!e.Stream` 直接返回；否则若 `w` 实现 `http.Flusher` 就 `Flush()`（源码注释观点：一次录下的
  流也必须像一个流那样被交付完）。
- 错误路径：只有客户端写失败一条（字符串原文见上）。并发：请求 goroutine。

### `idempotencyComplete (internal/gateway/idempotency.go)`

```go
func (p *Proxy) idempotencyComplete(rec *record)
```

- 作用：存下这次请求产生的答案，或者释放认领，好让调用方的下一次尝试能做一次真的。
- 时序（源码注释观点）：它从 deferred recorder 里跑，在响应已经上线之后，所以一个慢 store 永远
  不可能拖慢答案。
- 早退：`p.idempotency == nil || !rec.idemClaimed`。
- **不可重放**：`!replayableResponse(rec)` → `p.idempotency.Abort(rec.idemScope, rec.idemKey)`、
  `rec.idemDecision = "aborted"`；`rec.idemReason == ""` 时
  `rec.idemReason = fmt.Sprintf("status %d is not replayable", rec.status)`，返回。
  源码注释观点：5xx 或被取消的流**不可重放**，因为"上一次尝试失败了"不是一个调用方应该永远收到的
  答案，而调用方的重试正是那个必须被允许再试一次的情形。
- 可重放：`status := rec.status`（`0` 时回落 `http.StatusOK`），构造
  `idempotency.Entry{Status: status, Headers: storedHeaders(rec), Body: storedBody(rec), RequestID: rec.requestID, Upstream: rec.upstream, Model: rec.model, Stream: rec.stream, Outcome: string(rec.outcome), CompletedAt: time.Now()}`，
  再 `stored, reason := p.idempotency.Complete(rec.idemScope, rec.idemKey, entry)`。
- 成功：`rec.idemDecision = "stored"`、`rec.idemStore = idempotentStored`。
- 失败（超限/未存下）：`rec.idemDecision = "not-stored"`、`rec.idemStore = idempotentOversize`、
  `rec.idemReason = reason`。源码注释观点：认领已经被 store 释放了，所以调用方下一次重试**真的**会
  再跑一遍——而且它被告知了这件事，而不是被留下自己猜。
- 注意：超限的文案由 store 通过 `Complete` 的第二个返回值 `reason` 给出，本函数不自己拼装。
- 读写状态：写 `rec.idemDecision`、`rec.idemStore`、必要时 `rec.idemReason`；读 `rec.idemCapture`（经
  `storedHeaders`/`storedBody`）。并发：deferred 阶段，仍是请求 goroutine。

### `storedHeaders (internal/gateway/idempotency.go)`

```go
func storedHeaders(rec *record) http.Header
```

- 作用：为一次重放快照响应头。`rec.idemCapture == nil || rec.idemCapture.header == nil` → `nil`；否则
  `h := rec.idemCapture.header.Clone()`，然后 `h.Del("Content-Length")`、`h.Del("Transfer-Encoding")`、
  `h.Del(HeaderIdempotentStore)`，返回 `h`。
- 源码注释观点：故意丢掉 `Content-Length` 与 `Transfer-Encoding`——重放的 body 是被记录下来的字节
  一次写出，而从一次分块生成里抄来的 `Content-Length` 描述的是一种已经不存在的分帧；Go 会为真正
  写出的东西计算正确的分帧。删掉 `HeaderIdempotentStore` 是为了不让上一次的 `stored`/`oversize`
  标记跟着答案一起被回放。
- 错误路径：无。并发：对 Clone 出来的副本操作，不动原头。

### `storedBody (internal/gateway/idempotency.go)`

```go
func storedBody(rec *record) []byte
```

- 作用：返回录下的响应体；`rec.idemCapture == nil` → `nil`；否则 `rec.idemCapture.body.Bytes()`。
- 注意：`bytes.Buffer.Bytes()` 是**别名而非拷贝**，所以这个切片与捕获缓冲共享底层数组；调用方
  （`idempotency.Complete`）必须在缓冲被复用或释放之前用掉它，且不得就地修改。

### `replayableResponse (internal/gateway/idempotency.go)`

```go
func replayableResponse(rec *record) bool
```

- 作用：判断一个答案能否被记住并重放。`rec.outcome == metrics.OutcomeCanceled` → `false`；否则返回
  `rec.status >= 200 && rec.status < 500`。
- 源码注释观点：2xx 与 4xx 是确定性答案——同样的请求会产生同样的东西，把录下的副本交回去正是全部
  要点；5xx、一次超时和一次被取消的流都是"我们不知道"，把其中之一钉到一个 key 上会把一次瞬时失败
  变成那次操作的永久失败。
- 边界：`rec.status == 0`（还没写过响应）落在 200 以下，因此同样**不可重放**，走 `Abort`；真正被存下
  的 entry 的状态由 `idempotencyComplete` 补成 200。
- 错误路径：无。并发：纯函数。

### `requestHash (internal/gateway/idempotency.go)`

```go
func requestHash(method, path string, body []byte) string
```

- 作用：给调用方问的东西做指纹，使"用同一个 key 发一个**不同**的请求"是冲突，而不是静默重放一个
  无关答案。
- 实现：`sum := sha256.New()`，依次 `sum.Write([]byte(method))`、`sum.Write([]byte{0})`、
  `sum.Write([]byte(path))`、`sum.Write([]byte{0})`、`sum.Write(body)`，返回
  `hex.EncodeToString(sum.Sum(nil)[:16])`（前 16 字节 = 32 个十六进制字符）。两个 `0x00` 是分隔符，
  否则 `("GET","/a","bc")` 与 `("GET","/ab","c")` 会撞。
- 源码注释观点：method 与 path 是哈希的一部分——把同一个 body POST 到 `/v1/embeddings` 与
  `/v1/chat/completions` 不是同一个操作。
- 错误路径：无。并发：纯函数（`sha256.New()` 每次新建）。

### `type captureWriter struct` (internal/gateway/idempotency.go)

```go
type captureWriter struct {
	http.ResponseWriter
	limit   int64
	header  http.Header
	status  int
	body    bytes.Buffer
	total   int64
	written bool
}
```

- 作用：在响应被写出的同时把它录下来，好让一次完成的答案日后能逐字节重放。
- 源码注释观点（类型级）：它是一个 tee，不是一个缓冲——每个字节仍然立刻到达客户端，`Flush` 被转发，
  而副本是被限长的。超过上限的响应会被完整交付、然后**故意不被记住**，因为为了服务一次可能永远不
  会来的重试而把一个无界答案留在内存里，正是网关把一次大生成变成一次故障的方式。
- 字段：`ResponseWriter` 是被包装的写者；`limit` 是副本上限；`header` 是首次 `WriteHeader` 时的头快照；
  `status` 是记录下来的状态码；`body` 是副本；`total` 是**实际写出的字节数**（可能超过 `limit`）；
  `written` 是"头已写过"标记。
- 并发：字段自身没有锁。它遵循 `net/http` 的约定——一个响应在同一时刻只由一个 goroutine 写——但
  它比裸 `ResponseWriter` 多了一份可变状态（`written`/`status`/`header`/`body`/`total`），所以任何
  绕过该约定、从别的 goroutine 直接写它的代码都会造成 data race。

### `newCaptureWriter (internal/gateway/idempotency.go)`

```go
func newCaptureWriter(w http.ResponseWriter, limit int64) *captureWriter
```

- 作用：构造一个捕获写者。`limit <= 0` 时用 `idempotency.DefaultMaxResponseBytes`；返回
  `&captureWriter{ResponseWriter: w, limit: limit}`。`header` 故意不初始化——它由首次 `WriteHeader`
  用当时的响应头 `Clone()` 填上。
- 调用关系：只被 `idempotencyBegin` 在 `Proceed` 分支调用，实参是 `p.idempotency.MaxResponseBytes()+1`。

### `(*captureWriter) WriteHeader (internal/gateway/idempotency.go)`

```go
func (c *captureWriter) WriteHeader(status int)
```

- 作用：记下状态码与头快照，然后转发。已 `written` 则直接返回（`http.ResponseWriter` 的语义是第一次
  之后的 `WriteHeader` 调用都是无操作，`superfluous response.WriteHeader call` 由标准库自己打印）。
- 逻辑：`c.written = true`、`c.status = status`、`c.header = c.ResponseWriter.Header().Clone()`，最后
  `c.ResponseWriter.WriteHeader(status)`。
- 源码注释观点：头是在第一个字节被写出**之前**捕获的，因为那是它还可变的最后时刻；一次重放必须
  复现 provider 发来的 content type 与厂商头。
- 错误路径：无返回值。并发：见类型条目。

### `(*captureWriter) Write (internal/gateway/idempotency.go)`

```go
func (c *captureWriter) Write(p []byte) (int, error)
```

- 作用：转发并录制。未 `written` 时先 `c.WriteHeader(http.StatusOK)`；`c.total += int64(len(p))`；
  `room := c.limit - int64(c.body.Len())`，`room > 0` 时若 `int64(len(p)) <= room` 整块 `c.body.Write(p)`，
  否则只 `c.body.Write(p[:room])`；最后**无条件** `return c.ResponseWriter.Write(p)`。
- 源码注释观点：多出一个字节就足以知道答案超限，所以副本的界由上限决定，而不是由响应大小决定。
  返回值来自底层写者（写穿），所以调用方看到的错误与没有这层包装时一致；`total` 记录的是真实交付量，
  这一点让"交付了但没记住"与"交付失败了"能被区分开。
- 错误路径：不自己产生错误，只透传底层 `Write` 的错误。并发：见类型条目。

### `(*captureWriter) Flush (internal/gateway/idempotency.go)`

```go
func (c *captureWriter) Flush()
```

- 作用：转发 flush。未 `written` 时先 `c.WriteHeader(http.StatusOK)`；底层实现 `http.Flusher` 时调
  `f.Flush()`。
- 源码注释观点：把它转发给底层写者，使经由一个被捕获响应的流式交付仍然保持增量。
- 注意：Go 的 `http.Flusher` 接口没有返回值，所以这里也无法上报 flush 失败；一次失败的 flush 会在
  随后的 `Write` 上表现为错误。

### `(*captureWriter) Unwrap (internal/gateway/idempotency.go)`

```go
func (c *captureWriter) Unwrap() http.ResponseWriter
```

- 作用：返回 `c.ResponseWriter`，把底层写者暴露给 `http.ResponseController`。源码注释观点：需要一个
  `Hijacker` 或一个 deadline 的 handler 仍然能穿过这层包装拿到它们。
- 这是 Go 1.20+ 约定的方法名，所以任何用 `http.NewResponseController` 的代码都能穿透捕获层。

---

## 7. 能力探测：`internal/gateway/capabilities.go`

这一节把"声明"与"实测"分开：`CapabilityReport()` 完全不产生 I/O（全是配置加本进程的健康窗口），
`ProbeCapabilities` 花真钱、因此只能被刻意调用（源码注释观点给出了这个分离的理由）。

### `type ModelInfo struct` (internal/gateway/capabilities.go)

```go
type ModelInfo struct {
	ContextWindow   int
	MaxOutputTokens int
	Capabilities    []string
	Notes           string
}
```

- 作用：一个模型**无法从线上观测到**的元数据。源码注释观点：openai 兼容方言报告的是一个模型叫什么、一个上游**服务**什么；里面没有任何东西说上下文有多大、模型能吐多少、它接受哪些可选请求形状；
  一个必须决定是否压缩对话的 agent 运行时需要第一个数字，一个排查"这个后端上视觉为什么失败"的运维
  需要第三个；两者都来自配置并被原样回送。
- 字段：`ContextWindow`（总上下文 token，未知为 0）、`MaxOutputTokens`（最大补全，未知为 0）、
  `Capabilities`（**这个模型自己**支持的标签，与所服务的上游声明的取并集）、`Notes`（运维自有备注）。
- 并发/错误：纯数据，无错误面。

### `type CapabilityReport struct` (internal/gateway/capabilities.go)

```go
type CapabilityReport struct {
	GeneratedAt  time.Time            `json:"generated_at"`
	Capabilities []string             `json:"capabilities"`
	Models       []ModelCapability    `json:"models"`
	Upstreams    []UpstreamCapability `json:"upstreams"`
}
```

- 作用："这个网关能服务什么"的声明式回答。源码注释观点：它不产生 I/O——这里的每一样要么是配置，
  要么是本进程自己的健康窗口；这正是把它与 `ProbeCapabilities` 分开的意义，后者花钱、因此只能被
  刻意运行。

### `type ModelCapability struct` (internal/gateway/capabilities.go)

```go
type ModelCapability struct {
	Model           string   `json:"model"`
	Capabilities    []string `json:"capabilities"`
	ContextWindow   int      `json:"context_window,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Notes           string   `json:"notes,omitempty"`
	Upstreams       []string `json:"upstreams"`
	Available       bool     `json:"available"`
}
```

- `Capabilities`：服务这个模型的上游所声明的与模型自身元数据所声明的**并集**。
- `Upstreams`：能服务这个模型的后端，按路由顺序。
- `Available`：是否至少有一个在服务它的后端当前没有被熔断。源码注释观点：一个没有可用后端的模型
  仍然会被列出——调用方需要知道它存在且它挂了，这与"未知模型"是两个不同的答案。

### `type UpstreamCapability struct` (internal/gateway/capabilities.go)

```go
type UpstreamCapability struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	BaseURL      string   `json:"base_url"`
	Capabilities []string `json:"capabilities"`
	Models       []string `json:"models"`
	CatchAll     bool     `json:"catch_all"`
	Priority     int      `json:"priority"`
	Tier         string   `json:"tier"`
	State        string   `json:"state"`
}
```

- 作用：一个后端声明的能力面与它当前的状态。字段全部直接来自 `*upstream.Target` 的只读访问器
  （`Kind`、`BaseURL`、`Capabilities()`、`ModelPatterns()`、`IsCatchAll()`、`Priority()`、`Tier()`）
  加上 `p.upstreamState(t.Name)`。

### `func (p *Proxy) CapabilityReport() CapabilityReport` (internal/gateway/capabilities.go)

```go
func (p *Proxy) CapabilityReport() CapabilityReport
```

- 作用：组装声明式能力面。全程不加锁、不做 I/O，只读 `p.upstreams`、`p.models`、`p.breakers`。
- 步骤：
  1. `rep := CapabilityReport{GeneratedAt: time.Now().UTC()}`；`targets := p.sortedTargets()`；
     `rep.Upstreams = make([]UpstreamCapability, 0, len(targets))`。
  2. 两个局部索引：`served`（模型名 → 声明它的后端）与 `caps`（模型名 → 标签集合）；`addCap` 闭包
     把标签 `strings.ToLower(strings.TrimSpace(t))` 后跳过空串再塞进集合——**标签大小写被归一**，
     否则 `Vision` 与 `vision` 会在报告里变成两个能力。
  3. 遍历 `targets`：为每个后端追加一条 `UpstreamCapability`（`State: p.upstreamState(t.Name)`），
     并把它的 `ModelPatterns()` 里**不等于 `"/"`** 的每个模式登记进 `served` 与 `caps`。
  4. 只出现在配置里的模型也要存在：遍历 `p.models`，`served` 里没有就登记 `nil` 后端，`caps` 里没有
     就建空集合，再 `addCap(model, p.models[model].Capabilities)`。源码注释观点：一个只在配置里被点名
     的模型仍然存在，在这里声明它，好让它的上下文窗口与能力标签在任何上游见过它的请求之前就能被
     回送。
  5. 对每个模型：把 `served[model]` 拷进 `names`，再把**所有 catch-all 后端**追加进去并 `addCap`——
     源码注释观点：一个 catch-all 后端服务每一个模型，包括它从未点名过的那个。
  6. 去重成 `upNames`（局部 `seen` map，保持顺序），并在遇到任何 `p.upstreamState(t.Name) != "open"`
     的后端时置 `available = true`。
  7. `modelCaps := setNames(caps[model])`，把每个标签并进 `allCaps`，追加一条 `ModelCapability`
     （上下文窗口与备注取自 `p.models[model]`，缺失时为零值）。
  8. 收尾：`sort.Slice(rep.Models, ...)` 按模型名排序；`rep.Capabilities = setNames(allCaps)`；
     `rep.Models == nil` 时置成空切片（保证 JSON 里是 `[]` 而不是 `null`）。
- 错误路径：无返回值错误；`p.upstreams == nil` 时 `sortedTargets` 返回 nil，报告退化为"只有配置里的
  模型、没有上游"。并发：只读，可并发调用（`p.models` 与 `p.upstreams` 在运行期不变）。

### `sortedTargets (internal/gateway/capabilities.go)`

```go
func (p *Proxy) sortedTargets() []*upstream.Target
```

- 作用：按路由顺序（先 `Priority()` 再 `Name`）列出后端，好让同一支车队的两次报告读起来一模一样。
  `p.upstreams == nil` → `nil`；否则取 `p.upstreams.Targets()` 后 `sort.SliceStable`：`Priority()` 不同
  时按优先级升序，相同则按 `Name` 升序。
- 为什么用 `SliceStable`（源码注释观点）：路由顺序里同优先级的后端顺序本身是有意义的，稳定排序
  保证报告不会在两次调用之间抖动。
- 注意：它直接对 `Targets()` 返回的切片就地排序；若该切片是注册表的内部切片，这个排序会**改动
  路由顺序**——这是共享可变状态，故本函数只在启动后的只读视角下使用（报告与探测）。
- 调用关系：被 `CapabilityReport()`、`ProbeCapabilities` 调用。

### `upstreamState (internal/gateway/capabilities.go)`

```go
func (p *Proxy) upstreamState(name string) string
```

- 作用：报告一个后端的熔断状态。`p.breakers == nil` 时返回 `"closed"`（源码注释观点：M0/M1 行为——
  一切健康）；否则返回 `string(p.breakers.Get(name).Report().State)`。
- 返回值是字符串而不是 `State` 类型，因为报告要 JSON 序列化，而 `open` 这个字面量在
  `CapabilityReport()` 里被直接比较（`!= "open"` 即视为可用）。
- 错误路径：无；并发：`p.breakers.Get(name)` 由熔断包保证线程安全。

### `type CapabilityProbeRequest struct` (internal/gateway/capabilities.go)

```go
type CapabilityProbeRequest struct {
	Capabilities []string `json:"capabilities"`
	Model        string   `json:"model"`
	Upstreams    []string `json:"upstreams"`
}
```

- `Capabilities`：要探测的标签；空表示这个网关声明的**每一个**能力——源码注释观点：这是昂贵的选择，
  也是诚实的默认。
- `Model`：探测用的模型；空表示每个后端用它声明的第一个模型探测，而一个只声明 catch-all `"/"` 的
  后端会被跳过（探测需要一个具体的模型名才发得出去）。
- `Upstreams`：把探测限制在点名后端；空表示全部。

### `type CapabilityProbe struct` (internal/gateway/capabilities.go)

```go
type CapabilityProbe struct {
	Capability string  `json:"capability"`
	Upstream   string  `json:"upstream"`
	Model      string  `json:"model"`
	Status     int     `json:"status"`
	Accepted   bool    `json:"accepted"`
	Outcome    string  `json:"outcome"`
	Error      string  `json:"error,omitempty"`
	ElapsedMS  float64 `json:"elapsed_ms"`
	Path       string  `json:"path"`
}
```

- 作用：对**一个后端**的一次实测的结果。`ElapsedMS` 由 `float64(time.Since(start).Microseconds()) / 1000`
  得到；`Path` 是这次探测真正打出去的路径，被回送给调用方，因为"探测了哪个端点"是结果的解释的一部分。

### `ProbeAccepted` 等六个结局常量 (internal/gateway/capabilities.go)

```go
const (
	ProbeAccepted      = "accepted"
	ProbeRejected      = "rejected"
	ProbeUnauthorized  = "unauthorized"
	ProbeIndeterminate = "indeterminate"
	ProbeUnreachable   = "unreachable"
	ProbeSkipped       = "skipped"
)
```

- 源码注释观点：它们故意多于一个布尔——"provider 拒绝了这个请求形状"与"provider 正在限我的流"
  是两个不同的答案，只有前者才是关于能力的。

### `probePathChat` / `probePathEmbeddings` (internal/gateway/capabilities.go)

- `probePathChat = "/v1/chat/completions"`、`probePathEmbeddings = "/v1/embeddings"`：探测只用的两个端点。
  源码注释观点：它们是**常量**，因为探测必须打在与客户流量相同的路径上。

### `ErrUnknownCapability (internal/gateway/capabilities.go)`

- `var ErrUnknownCapability = errors.New("unknown capability")`：标记一个点名了本网关没有请求形状的
  能力的探测请求；这是调用方错误，handler 会把它翻成 400 而不是 500。

### `probeCapabilities (internal/gateway/capabilities.go)`

```go
func probeCapabilities() []string
```

- 作用：列出本网关知道怎么探测的每一个能力，按报告里列出的顺序。返回值原文：
  `[]string{"chat", "stream", "tools", "vision", "json", "embeddings"}`。
- 调用关系：被 `ProbeCapabilities` 两次调用（一次取默认集合、一次取合法集合），也被 `probeBody` 的
  形状集合隐式对应（多一个都会在 `probeBody` 的 default 分支变成 `ProbeSkipped`）。

### `probeTinyPNG (internal/gateway/capabilities.go)`

- `const probeTinyPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="`：
  一个 1x1 透明 PNG 的 data URL。源码注释观点：一次视觉探测必须发一张图片，而这个请求不能依赖网络
  去抓一个 URL，所以它被内联成 data URL；模型不被期望描述它——探测问的是 provider 是否**接受**这个
  content part。

### `ProbeCapabilities (internal/gateway/capabilities.go)`

```go
func (p *Proxy) ProbeCapabilities(ctx context.Context, req CapabilityProbeRequest) ([]CapabilityProbe, error)
```

- 作用：花真实的上游调用，查明一个后端实际接受哪些能力。
- 它能与不能确立什么（源码注释观点，整段保留）：一次探测发出能触发某个能力的最小请求，并按
  **接受度**判断答案——2xx 或任何不是客户端拒绝的状态都意味着 provider 看懂了这个形状。它**不**确立
  模型把这个能力用得好（一个 tool call 不被要求返回），也完全不谈质量。它支撑的声明正是运维在信任
  一个后端承接某类工作负载之前需要的那一个：这个端点不拒绝该请求形状。
- 刻意绕过（源码注释观点）：它**故意绕过缓存、预算、熔断器与重试链**——探测是诊断，不是客户流量，
  它不得消耗一个租户的预算、不得被从缓存作答、也不得扳动一个熔断器。
- 校验：`wanted := req.Capabilities`，为空时取 `probeCapabilities()`；用 `known` map 校验每个标签
  （先 `strings.ToLower(strings.TrimSpace(c))`），不合法时返回
  `fmt.Errorf("%w %q: probe one of %s", ErrUnknownCapability, c, strings.Join(probeCapabilities(), ", "))`
  与 `nil` 结果。
- 过滤：`restrict` map 收集 `req.Upstreams` 里 trim 后非空的项（大小写敏感，按集合里写的样子匹配）。
- 超时：`timeout := p.upstreamTTL`，`<= 0` 或 `> 20*time.Second` 时用 `20 * time.Second`。
- 结果容量：`out := make([]CapabilityProbe, 0, len(wanted)*len(p.sortedTargets()))`。
- 主循环：遍历 `p.sortedTargets()`，先按 `restrict` 过滤；再 `model, ok := probeModel(target, req.Model)`；
  对每个待测能力：`!ok` 时追加一条 `CapabilityProbe{Capability, Upstream: target.Name, Outcome: ProbeSkipped, Error: "backend declares no concrete model name to probe with"}`；
  否则追加 `p.probeOne(ctx, target, capability, model, timeout)`。
- 返回值：`(out, nil)`——**单个后端的失败不会变成整个探测失败**，它只是那一条结果里的 `Outcome`/`Error`。
- 并发：串行遍历（一个探测一次请求）；`ctx` 只用于 `probeOne` 内的 `req.WithContext(ctx)`。

### `probeModel (internal/gateway/capabilities.go)`

```go
func probeModel(t *upstream.Target, requested string) (string, bool)
```

- 作用：挑出要发的模型名。`requested` trim 后非空就直接用它（`(requested, true)`）；否则遍历
  `t.ModelPatterns()`，返回第一个**不是 `"/"` 且 trim 后非空**的模式；都不满足返回 `("", false)`。
- 源码注释观点：catch-all 的 `"/"` 不是 provider 会接受的模型名，所以一个只声明它的后端在不被告知
  该发什么的时候无法被探测。

### `probeOne (internal/gateway/capabilities.go)`

```go
func (p *Proxy) probeOne(ctx context.Context, target *upstream.Target, capability, model string, timeout time.Duration) CapabilityProbe
```

- 作用：执行一次探测并归类答案。
- 形状：`path, body, err := probeBody(capability, model)`；`err != nil` → `probe.Outcome = ProbeSkipped`、
  `probe.Error = err.Error()`，返回。否则 `probe.Path = path`。
- **如何复用 `buildRequest`**（源码注释观点，整段保留）：一个合成的入站请求给 `buildRequest` 提供了它
  需要的一切（路径、方法、一个 context），同时把真实调用方的头与 traceparent 挡在一次诊断之外；
  `buildRequest` 是唯一知道一个后端如何被寻址与授权的地方，在这里重新实现那件事，就是让探测开始
  测试一条与客户流量不同的代码路径。合成请求用 `httptest.NewRequest(http.MethodPost, path, nil)`，
  只设 `Content-Type: application/json`；`rec := &record{requestID: "probe-" + target.Name, path: path}`。
- 调用：`req, client, cleanup, err := p.buildRequest(synthetic, target, body, timeout, rec)`；`err != nil` →
  `ProbeUnreachable` + `err.Error()`；否则 `defer cleanup()`、`req = req.WithContext(ctx)`。
- 计时与发送：`start := time.Now()`；`resp, err := client.Do(req)`；`probe.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000`；
  `err != nil` → `ProbeUnreachable` + `err.Error()`。
- 读取：`defer resp.Body.Close()`；`_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))`——源码注释观点：
  答案被读取但从不保留，探测需要的是状态与 content type，否则一个流式答案会变成一次无界读取。
- 归类：`probe.Status = resp.StatusCode`；
  `probe.Accepted, probe.Outcome = classifyProbe(capability, resp.StatusCode, resp.Header)`；
  若 `!probe.Accepted && probe.Outcome == ProbeRejected`，则 `probe.Error = "provider rejected the request shape"`。
- 注意命名：本函数以值返回 `CapabilityProbe`，**唯一的 error 走 `probe.Error` 字符串**，函数本身不返回
  Go error——这个选择让"一个后端的失败"天然是一条结果，而不是一次中断整个探测的返回。

### `classifyProbe (internal/gateway/capabilities.go)`

```go
func classifyProbe(capability string, status int, header http.Header) (bool, string)
```

- 作用：把状态码翻成一个结局（`accepted bool` + `outcome string`）。源码注释观点：只有客户端侧的拒绝
  才与能力有关——一次限流或一个上游错误对"这个后端是否支持这个形状"什么都没说，把它报成"不支持"
  会让一次探测恰好在它被运行的那场事故里说谎。
- 判定顺序（先到先得）：
  1. `status == http.StatusUnauthorized || status == http.StatusForbidden` → `(false, ProbeUnauthorized)`。
  2. `status == http.StatusTooManyRequests || status == http.StatusRequestTimeout` → `(false, ProbeIndeterminate)`。
  3. `status >= 500` → `(false, ProbeIndeterminate)`。
  4. `status >= 400` → `(false, ProbeRejected)`。
  5. 其余（1xx/2xx/3xx）继续往下。
- 额外的流式判据：`capability == "stream"` 且 `Content-Type` 小写后不含 `"text/event-stream"` 时返回
  `(false, ProbeRejected)`——源码注释观点：一个用缓冲 body 回答流式请求的 provider 接受了参数、没
  接受模式，这值得知道。
- 成功：`(true, ProbeAccepted)`。

### `probeBody (internal/gateway/capabilities.go)`

```go
func probeBody(capability, model string) (string, []byte, error)
```

- 作用：构造能触发一个能力的**最小**请求，返回（路径、body、error）。六个分支的形状原文：
  - `"embeddings"` → `probePathEmbeddings`，`{"model": model, "input": "ping"}`。
  - `"chat"` → `probePathChat`，`model` + 一条 `user` 消息 `"ping"` + `"max_tokens": 1`。
  - `"stream"` → `probePathChat`，同上再加 `"stream": true`。
  - `"tools"` → `probePathChat`，加一个名为 `infergate_probe`、描述 `no-op probe function`、
    `parameters` 为 `{"type":"object","properties":{}}` 的 function 工具，`"max_tokens": 1`。
  - `"vision"` → `probePathChat`，content 是数组：一个 `{"type":"text","text":"ping"}` 与一个
    `{"type":"image_url","image_url":{"url":probeTinyPNG}}`。
  - `"json"` → `probePathChat`，user 内容是 `{"ok":true}` 的字符串，加
    `"response_format": {"type":"json_object"}`。
- `default` → 返回 `("", nil, fmt.Errorf("no probe shape for capability %q", capability))`；在
  `probeOne` 里被翻成 `ProbeSkipped`。
- 全部用 `mustJSON` 序列化，所以返回的 error 只可能来自未知能力这一条路径。

### `mustJSON (internal/gateway/capabilities.go)`

```go
func mustJSON(v any) []byte
```

- 作用：序列化一个探测 body。`err != nil` 时 `panic("gateway: probe body: " + err.Error())`，否则返回
  `json.Marshal` 的字节。源码注释观点：这些值是字面量，所以失败是编程错误，而不是调用方能造成的
  东西——因此用 panic 而不是 error 返回值，让它在测试里立刻炸出来。

### `setNames (internal/gateway/capabilities.go)`

```go
func setNames(set map[string]struct{}) []string
```

- 作用：把集合翻成**有序**切片（`sort.Strings`）。源码注释观点：这让报告在重启之间保持稳定、可 diff。
- 读到一个 nil map 时返回空切片（`make([]string, 0, 0)`），不会返回 nil。

### `ProbeJSONReader (internal/gateway/capabilities.go)`

```go
func ProbeJSONReader(body []byte) (CapabilityProbeRequest, error)
```

- 作用：给测试与 handler 用的宽松解码器：`len(bytes.TrimSpace(body)) == 0` 时返回零值请求与 `nil`
  错误（**空 body 意味着"探测所有已声明的东西"**）；否则 `json.Unmarshal` 并返回其错误。
- 并发：纯函数；导出（首字母大写）是因为 handler 与包外测试都要用。

---

## 8. 请求级追踪：`internal/gateway/tracing.go`

这一层只做"这次请求要不要留一条 trace、上游要带什么传播上下文"的决策，以及把 `record` 上的事实
拼成 span 与 event。真正的存储与导出在 `internal/tracing`。

### `SpanGateway (internal/gateway/tracing.go)`

- `SpanGateway = "gateway.request"`：根 span，一次请求一条，覆盖整条管线，包括每一次故障转移尝试
  与响应拷贝。源码注释观点：它们被写成常量而不是字面量，好让一个仪表盘或一个测试能引用一个 span
  而不必重复一个打错字就会悄悄变成另一个 span 的字符串。

### `SpanUpstreamPrefix (internal/gateway/tracing.go)`

- `SpanUpstreamPrefix = "upstream."`：每次上游尝试一条 client span，后端名被拼在后面
  （`SpanUpstreamPrefix + target`）。源码注释观点：这样"哪个后端慢"在一个 trace 查看器里只看 span
  名字就能回答。

### `firstTokenMS (internal/gateway/tracing.go)`

```go
func firstTokenMS(rec *record) float64
```

- 作用：把观测到的首 token 延迟渲染成毫秒。`rec == nil || !rec.firstTokenSet` → `0`；否则
  `float64(rec.firstToken) / float64(time.Millisecond)`。
- 源码注释观点：做成一个 helper 而不是内联表达式，是因为同一个值会出现在根 span、尝试 span 与流
  事件里，而一个单位换算抄三份正是 trace 之间开始互相矛盾的方式。
- 读写状态：只读 `rec`；无错误路径；并发安全（纯读）。

### `TraceExporter (internal/gateway/tracing.go)`

```go
type TraceExporter interface {
	Export(t *tracing.Trace)
}
```

- 作用：接收已完成的 trace。源码注释观点：`Export` 在请求 goroutine 上被调用，**必须不阻塞**——
  一个写文件或写网络的实现要把 trace 交给自己的 goroutine，并在缓冲满时丢弃；一个因为采集器不可达
  而变慢的网关是更差的网关，而不是更好的可观测性故事。

### `type Tracer struct` (internal/gateway/tracing.go)

```go
type Tracer struct {
	store     *tracing.Store
	exporters []TraceExporter
	ratio     float64
}
```

- 作用：每个被采样的请求记录一条 trace。源码注释观点：**nil 是合法值**——每个方法都容忍 nil 接收者，
  这正是让追踪可以在不在每个调用点分支的情况下成为可选的原因。
- 字段：`store`（本地环形缓冲，可为 nil）、`exporters`（零到多个导出器）、`ratio`（采样比例，
  由 `NewTracer` 夹到 ≤1）。

### `NewTracer (internal/gateway/tracing.go)`

```go
func NewTracer(store *tracing.Store, ratio float64, exporters ...TraceExporter) *Tracer
```

- 作用：在 store 与零到多个 exporter 之上构造一个 tracer。`store == nil && len(exporters) == 0` 时
  返回 `nil`（源码注释观点：所以"追踪开了但什么都没配置"对请求路径零成本）；`ratio > 1` 时置 1；
  返回 `&Tracer{store: store, exporters: exporters, ratio: ratio}`。
- 注意：`ratio < 0` 不被夹到 0，但 `shouldRecord` 里 `t.ratio <= 0` 的分支会把它当成"一律不采样"。

### `(*Tracer) begin (internal/gateway/tracing.go)`

```go
func (t *Tracer) begin(r *http.Request, rec *record)
```

- 作用：开始这条请求的 trace，并算出要向上游传播的 W3C 上下文。
- **被追踪但不采样时仍转发 traceparent**（源码注释观点，原文级）：出站上下文是**即使这次请求不被
  记录也算**的，因为一个正在追踪的调用方必须跨过一个不追踪的网关仍然保持连接——在这一跳丢掉
  traceparent，对调用方来说与网关把请求弄丢了无法区分。
- nil tracer：直接返回。源码注释观点：什么都没铸造，而调用方自己的 traceparent 已经由头拷贝原样
  转发了，所以一个没有追踪的部署既不会破坏、也不会凭空造出一段 trace 链。
- 父上下文：`parent, ok := tracing.Parse(r.Header.Get(tracing.HeaderTraceparent), r.Header.Get(tracing.HeaderTracestate))`；
  `!ok` 时 `parent = tracing.NewRootContext()`。源码注释观点：入站上下文永不被信任——一个畸形的
  上下文是被**替换**而不是被半复用，后者会产生一个父 span 并不存在的 span。
- 采样：`rootID := tracing.NewSpanID()`；`record := t.shouldRecord(parent, ok, rec.requestID)`；
  `sampled := record`，而 `ok` 为真时 `sampled = parent.Sampled || record`。源码注释观点：一个正在
  追踪的调用方即使本网关不记录也保持连接，所以这个标志是继承来的；没有合法入站上下文时网关**就是**
  根，这个标志就只是它自己的决定。
- 出站上下文：`tracing.Context{TraceID: parent.TraceID, SpanID: rootID, Sampled: sampled, TraceState: parent.TraceState}`；
  `rec.traceparent = outbound.Header()`；`outbound.TraceState != ""` 时 `rec.tracestate = outbound.TraceState`。
- 只在 `record` 为真时建 trace：`rt := &requestTrace{trace: tracing.NewTrace(parent.TraceID, rec.requestID, rec.start), parent: parent, rootID: rootID, store: t.store, exporters: t.exporters}`；
  `rt.trace.Attributes = map[string]any{tracing.AttrRoute: rec.route, tracing.AttrRequest: rec.requestID}`；
  `rec.trace = rt`。
- 读写状态：只读 `r`；写 `rec.traceparent`、`rec.tracestate`、`rec.trace`。

### `(*Tracer) shouldRecord (internal/gateway/tracing.go)`

```go
func (t *Tracer) shouldRecord(parent tracing.Context, parentOK bool, requestID string) bool
```

- 作用：执行采样判定。顺序：
  1. `t == nil || (t.store == nil && len(t.exporters) == 0)` → `false`（没有地方放 trace）。
  2. `parentOK && !parent.Sampled` → `false`。源码注释观点：一个发来合法 traceparent、但 sampled 标志
     为 0 的调用方在这里同样不被记录——客户端已经决定这条 trace 不被保留，而一个只留下其中一半的
     网关会产出一条缺了自己下游的 trace。
  3. `t.ratio >= 1` → `true`；`t.ratio <= 0` → `false`。
  4. 否则用 `fnv.New32a()` 哈希 `requestID`，返回 `float64(h.Sum32()%10000)/10000 < t.ratio`。
- 源码注释观点（为什么用哈希而不是随机数）：把 request id 哈希而不是抽一个随机数，判定对给定
  request id 是**稳定的**，所以同一个请求的重试会被同样采样，而一个测试不用播种就能对它下断言。
- 读写状态：只读 `t`；无错误路径（`h.Write` 的错误被显式丢弃：`_, _ = h.Write([]byte(requestID))`）。
- 调用关系：被 `begin` 调用；也被 `tracing_test.go` 的稳定性测试间接依赖。

### `type requestTrace struct` (internal/gateway/tracing.go)

```go
type requestTrace struct {
	trace     *tracing.Trace
	parent    tracing.Context
	rootID    string
	store     *tracing.Store
	exporters []TraceExporter
	spans     []tracing.Span
	events    []tracing.Event
}
```

- 作用：一条请求正在构建中的 trace。源码注释观点：它只被服务该请求的那一个 goroutine 触碰，
  所以它**没有自己的锁**。
- 字段：`trace` 是最终产物；`parent` 用于决定根 span 的 `ParentSpanID`（见 `finish`）；`rootID` 是
  本次铸造的根 span id，同时是每个尝试 span 的父；`store`/`exporters` 从 `Tracer` 拷来（构造后不再
  变，因此并发读安全）；`spans`/`events` 是收尾前累积的中间态。

### `type attemptSpan struct` (internal/gateway/tracing.go)

```go
type attemptSpan struct {
	rt   *requestTrace
	span tracing.Span
}
```

- 作用：一次上游尝试的 client span 句柄。源码注释观点：它的零值是一个 no-op，所以尝试路径从不
  分支判断追踪是否开着；这就是一个特性"可以在生产里一直开着"与"不能"之间的差别。
- 读写状态：`rt == nil` 即零值；所有方法（`finish`）都先判 nil。并发：同 `requestTrace`。

### `(*requestTrace) startAttempt (internal/gateway/tracing.go)`

```go
func (rt *requestTrace) startAttempt(target string, attemptNo int) attemptSpan
```

- 作用：为一次上游尝试开一条 client span。`rt == nil` → `attemptSpan{}`；否则返回一个持有
  `tracing.Span{TraceID: rt.trace.TraceID, SpanID: tracing.NewSpanID(), ParentSpanID: rt.rootID, Name: SpanUpstreamPrefix + target, Kind: tracing.KindClient, StartUnixNano: time.Now().UnixNano(), Status: tracing.StatusUnset, Attributes: map[string]any{tracing.AttrUpstream: target, "attempt": attemptNo}}`
  的 `attemptSpan`。
- 注意：`ParentSpanID` 是**本地的** `rt.rootID`，与 `finish` 里根 span 的 `ParentSpanID`（只在远端
  父时设置）是两个不同的判断，别混淆。

### `(attemptSpan) finish (internal/gateway/tracing.go)`

```go
func (a attemptSpan) finish(rec *record)
```

- 作用：用这次尝试报上来的东西关闭尝试 span。`a.rt == nil` → 返回。
- 源码注释观点（为什么从 `record` 读而不是接收参数）：状态与结局是从 `record` 读的，因为
  `attemptUpstream` 的每一条返回路径本来就已经写了它们，而加参数就得穿过其中八个返回点。
- 逻辑：`a.span.EndUnixNano = time.Now().UnixNano()`；写属性 `"status"`（`rec.status`）、
  `"outcome"`（`string(rec.outcome)`）；`rec.model != ""` → `tracing.AttrModel`；`rec.reason != ""` →
  `"reason"`；`rec.stream` 为真 → `"stream": true`；`rec.frames > 0` → `"frames"` 与 `"response_bytes"`
  （`rec.respBytes`）；`rec.firstTokenSet` → `"first_token_ms"`（`firstTokenMS(rec)`）；
  `rec.outcome == metrics.OutcomeSuccess` → `tracing.StatusOK`，否则 `tracing.StatusError`；
  最后 `a.rt.spans = append(a.rt.spans, a.span)`。
- 源码注释观点：`DurationMS` 由 `Trace.Add` 从整数纳秒对推导；在这里再算一遍就有机会与它漂移，
  所以这里不设时长。
- 并发：请求 goroutine（`rt.spans` 无锁）。

### `(*requestTrace) addEvent (internal/gateway/tracing.go)`

```go
func (rt *requestTrace) addEvent(name string, attrs map[string]any)
```

- 作用：记录请求期间发生、但不是一次自带时长的操作：一个缓存判定、一条流摘要、一次故障转移。
  `rt == nil` → 返回；否则追加 `tracing.Event{Name: name, TimeUnixNano: time.Now().UnixNano(), Attributes: attrs}`。
- 注意：`attrs` 不被拷贝，调用方之后修改它会影响事件内容。

### `(*requestTrace) finish (internal/gateway/tracing.go)`

```go
func (rt *requestTrace) finish(rec *record)
```

- 作用：关闭根 span、存下 trace、交给导出器。`rt == nil` → 返回。
- 时序（源码注释观点）：它从 `ServeHTTP` 的 deferred recorder 里跑，在响应写出**之后**，所以这里
  没有任何东西能拖慢一个答案。
- 状态：`end := time.Now().UnixNano()`；`status := tracing.StatusOK`，`rec.outcome != metrics.OutcomeSuccess`
  时为 `tracing.StatusError`。
- 根属性全集（原文键名）：`tracing.AttrRoute`、`tracing.AttrRequest`、`"path"`、`"stream"`、`"status"`、
  `"outcome"`、`"request_bytes"`、`"response_bytes"`、`"attempts"`；条件追加：`rec.upstream != ""` →
  `tracing.AttrUpstream`；`rec.model != ""` → `tracing.AttrModel`；`rec.tenant != ""` → `"tenant"`；
  `rec.session != ""` → `"session"`；`rec.reason != ""` → `"reason"`；`rec.cacheStatus != ""` → `"cache"`
  与（`rec.cacheReason != ""` 时）`"cache_reason"`；`rec.idemKey != ""` → `"idempotency_key"`、
  `"idempotency"`、`"idempotency_store"`（非空时）、`"idempotency_reason"`（非空时）；`len(rec.tried) > 1`
  → `"tried"`；`rec.frames > 0` → `"frames"`；`rec.firstTokenSet` → `"first_token_ms"`；
  `!rec.usage.IsEmpty()` → `"prompt_tokens"`、`"completion_tokens"`、`"cached_tokens"`。
- 为什么把幂等 key 本身记进 trace（源码注释观点）：一个崩溃后重试的 agent，是靠在一个 key 下找到
  两次尝试来诊断的，而 trace 正是运维翻找这件事的地方；decision 区分了干活的尝试与从 store 得到
  答复的那一次。
- **`ParentSpanID` 只在远端父时设置**：`parentSpanID := rt.parent.SpanID`，`!rt.parent.Remote` 时
  `parentSpanID = ""`。源码注释观点：一个本地铸造的上下文没有父可指——把 `NewRootContext` 发明的那个
  合成 span id 写上去，会渲染成一个在任何 trace 里都不存在的父，那比没有父更糟。
- 组装与输出：根 span 用 `TraceID: rt.trace.TraceID`、`SpanID: rt.rootID`、`Name: SpanGateway`、
  `Kind: tracing.KindServer`、`StartUnixNano: rec.start.UnixNano()`、`EndUnixNano: end`、`Status: status`、
  `Attributes: attrs`、`Events: rt.events`；然后 `rt.trace.Add(root)`，再逐个 `rt.trace.Add(s)`；
  `rt.trace.Status = status`；`rt.store != nil` 时 `rt.store.Add(rt.trace)`；最后遍历 `rt.exporters`，
  **跳过 nil 项**，对每个调 `e.Export(rt.trace)`。
- 错误路径：无返回值错误；导出器被约定为不阻塞（见 `TraceExporter`）。
- 并发：请求 goroutine 收尾阶段；`store.Add`/`Export` 的线程安全由各自实现负责。

---

## 9. 会话账本入口：`internal/gateway/sessionpath.go`

### `recordSession (internal/gateway/sessionpath.go)`

```go
func (p *Proxy) recordSession(r *http.Request, rec *record)
```

- 作用：把这次请求追加到按会话的账本上。源码注释观点（为什么必须有这一层）：账本回答的是请求日志
  答不了的问题——一整段 agent 对话总共花了多少、用了哪些模型、其中多少来自缓存或从幂等 store 重放、
  其中多少次失败了；那些是关于一段**对话**的问题，而一段对话是散在多个 provider 上的多个请求，
  所以汇总必须发生在这里（每个请求都经过的地方），而不是任何单个 provider 路径里。
- 门（源码注释观点）：只记录补全路由。一个把 `/v1/models` 列表也计入的预算与成本汇总会为免费流量
  报出花费，而 token 维度在一个没有 prompt 的请求上纯属虚构。实现即
  `p.sessions == nil || !isCompletionPath(rec.path)` 直接返回。
- `sessions.Request` 字段映射（原文）：`At: rec.start`、`RequestID`、`Route`、`Upstream`、`Model`、
  `RequestedModel: rec.explicitModel`、`Status`、`Outcome: string(rec.outcome)`、`Stream`、
  `PromptTokens: rec.usage.Prompt`、`CompletionTokens: rec.usage.Completion`、`CachedTokens: rec.usage.Cached`、
  `ElapsedMS: float64(time.Since(rec.start).Microseconds()) / 1000.0`、`Attempts`、
  `Tried: append([]string(nil), rec.tried...)`（**拷贝**，避免账本与 `record` 共享底层数组）、
  `Cache: rec.cacheStatus`、`Reason: rec.reason`、`Replay: rec.servedFromReplay`。
- 成本：`p.pricing != nil` 时 `req.CostUSD = p.pricing.CostUSD(rec.model, rec.usage.Prompt, rec.usage.Completion)`。
- 首 token：`rec.firstTokenSet` 时 `req.FirstTokenMS = float64(rec.firstToken.Microseconds()) / 1000.0`。
- 写入：`p.sessions.Record(rec.tenant, rec.session, req)`。
- 源码注释观点（空 session id）：空的 session id 在这里**不是错误**——账本把它单独统计
  （`Stats.NoSessionID`），而不是发明一个只含一条请求的伪会话，因为一本塞满这种伪会话的账本会掩盖
  "调用方根本没发这个头"这件事。
- 错误路径：无返回值；`Record` 的失败由会话包自己处理（本层不感知）。
- 并发：请求 goroutine；`p.sessions` 的线程安全由会话包保证。

---

## 10. 测试反推的意图（不变量清单）

### `cache_test.go`：把"确定性"与"隔离性"钉住

- `TestCacheMissThenExactHit`：两次**完全相同**的请求第二次必须命中，且第二次不得再打上游。
- `TestCacheSemanticHitAcrossWording`：措辞不同的同一问题在相似度阈值之上可以被语义命中——这是
  缓存存在的理由，也是风险所在，所以下面每一条都是在给它划边界。
- `TestCacheSamplingRequestsMatchExactlyOnly`：带采样参数（如 temperature > 0）的请求只能精确命中，
  不得被一个采样出来的旧答案服务。
- `TestCacheToolsAreExactMatchOnly`：带 tools 的请求同样只走精确匹配——工具集合让"相似问题"的语义
  不再成立。
- `TestCacheSignatureBlocksADifferentAnswerShape`：`answerSignature` 必须挡住"形状不同"的复用
  （例如 `response_format` 变了）。
- `TestCacheSignatureSeparatesConversationPrefixes`：同一句问题在不同对话前缀下必须是不同条目。
- `TestCacheSkipsTrivialPrompts`：太短的 prompt 不值得承担语义匹配的风险，宁可跳过。
- `TestCacheBypassSkipsTheLookupButStillStores` 与 `TestCacheRefreshReplacesTheStoredAnswer`：两条
  缓存指令的语义不同——`bypass` 不查但**照存**，`refresh` 覆盖已有条目。
- `TestCacheHitIsNotAttributedToAnUpstream`：命中的 `X-InferGate-Upstream-Name` 必须是 `cache`，
  不得记在真后端头上；它与 `TestCacheStoresOnlySuccessfulAnswers` 一起，保证了"缓存不会让熔断器
  看到一个从未发生的上游调用"。
- `TestCacheReplaysStreamingAnswers` 与 `TestCacheReplayKeepsFramePayloadsValid`：重放的流必须逐帧
  合法——调用方的 SSE 解析器不能因为重放而看到坏帧。
- `TestCacheIsScopedToTenantAndModel`、`TestCacheDoesNotAnswerNonCompletionRoutes`、
  `TestCacheDisabledLeavesM1Behaviour`：三条隔离边界（租户/模型、路径、未装缓存时表面不变）。

### `quota_test.go`：把三条路径与"租约"钉住

- `TestQuotaNilGateIsInvisible` 与 `TestQuotaGateDisabledIsNotConsulted`：没有配额与禁用配额都是
  **零表面**（后者还断言 gate 没被调用过）。
- `TestQuotaAllowAdvertisesBudget`：放行也要带 `X-InferGate-Quota` 与剩余额度。
- `TestQuotaRejectIsAnsweredWithoutUpstream`、`TestQuotaRejectRetryAfterNeverZero`：429 必须在**没有
  上游调用**的情况下给出，且 `Retry-After` 永远有值（对应 `seconds := 1` 的兜底）。
- `TestQuotaStoreErrorFailsClosed` 与 `TestQuotaStoreErrorFailsOpen`：同一份坏 store 在两种配置下
  的行为完全相反（503 拒发 vs 放行），而 fail-open 的判定条件是 `Allowed` 为真**且** `err` 非 nil
  同时成立——这正是 `admitQuota` 里那两个分支的形状。
- `TestQuotaDegradeRewritesModel`、`TestQuotaDegradeModelMatchIsCaseInsensitive`、
  `TestQuotaDegradeTokenCapLowersCeiling`：降级的两级杠杆各自生效，且模型比较是大小写不敏感的
  （对应 `strings.EqualFold`"同一个模型"的判断）。
- `TestQuotaSettleChargesProviderUsage` 与 `TestQuotaSettleCacheHitChargesNoTokens`：结算按真实用量
  收费，而缓存命中只记一次请求、零 token。
- `TestQuotaReleaseWhenNoUpstreamAttempted`：`attempts == 0` 走 `Release` 而不是 `Settle`。
- `TestQuotaEstimateSizing`、`TestQuotaEstimatePromptTokensFloor`、`TestQuotaCharsPerTokenIsHonoured`：
  估算的边界（向下取整后的下限为 1、`quotaCharsPerToken` 可配）。
- `TestQuotaAdmitSeesTenantAndSession`：`tenantFor` 与 `HeaderSession` 的取值真的到了 `Admit`。
- `TestQuotaSkipsNonCompletionPaths`：`/v1/models` 这类路径不进预算。
- `TestRewriteMaxTokens` 与 `TestTenantForFoldsSchemeCase`：两个基础改写/归一函数的独立回归
  （后者钉住 `Bearer` / `bearer` 推出同一个租户）。

### `idempotency_test.go`：把四态与"什么可以被记住"钉住

- `TestIdempotentReplayAvoidsASecondUpstreamCall` 与 `TestIdempotentReplayOfAStreamedAnswer`：重放
  必须省掉第二次上游调用，流式答案也照样重放。
- `TestIdempotencyKeyIsScopedByTenant`：同一个 key 在不同租户下是两次独立操作。
- `TestIdempotentConflictOnADifferentBody` 与 `TestIdempotentConflictOnADifferentPath`：`requestHash`
  的两个组成部分各自被验证——body 变了是冲突，路径变了也是冲突。
- `TestIdempotentInFlightIsAConflict`：并发同 key 的第二个请求得到 409 + `Retry-After`，而不是排队
  或重复生成。
- `TestIdempotentFailureIsNotReplayed`：5xx 不被记住，重试会真的重跑（`replayableResponse` 的核心）。
- `TestIdempotentOversizeIsServedButNotRemembered`：超过上限的响应**完整交付但不被记住**，
  且响应头说明没存下。
- `TestIdempotentKeyOnANonCompletionPathIsIgnored`：非补全路径上的 key 被静默忽略（有头说明），
  不是被拒绝。
- `TestIdempotencyIsOffWithoutAStore`：没有 store 时整条幂等路径不可见。
- `TestReplaySettlesAsOneRequestNoTokens` 与 `TestReplayIsObservedAsItsOwnUpstream`：重放消耗一个
  请求槽位、零 token，且上游名被记成 `"replay"`——这两条正好各自对应 `serveReplay` 里两处最容易被
  漏掉的写入。
- `TestSessionsLedgerRollsUpAConversation`、`TestSessionsLedgerRecordsARequestWithoutASessionID`、
  `TestSessionLedgerIgnoresNonCompletionPaths`、`TestSessionLedgerCapturesTheFailure`：账本的四条
  不变量——按会话汇总、空 session id 单独计入、只记补全路径、失败请求也要落账。

### `tracing_test.go`：把传播与采样钉住

- `TestTraceSamplingZeroStillPropagates`：`ratio = 0`（不记录）时**仍然**要向上游发出 traceparent
  ——这正是 `begin` 里"出站上下文先算、`record` 为假再提前返回"的顺序所保证的。
- `TestTraceHonoursCallerDropDecision`：调用方带了 sampled=0 的合法 traceparent 时，本网关不记录。
- `TestSamplingIsStableForOneRequestID`：同一个 request id 的采样判定必须稳定——它同时锁住了
  `fnv32a(requestID)` 这个实现选择（换成随机数这条测试就会不稳定）。
- `TestTraceContinuesCallerContext`、`TestTraceMintsContextForUntracedCaller`、
  `TestTraceReplacesMalformedCallerContext`：三种入站上下文的处理（延续、铸造、替换）。
- `TestTracerDisabledRecordsNothing`：没有 store 也没有 exporter 时，`NewTracer` 返回 nil，
  一切是 no-op。
- `TestTraceRecordsOneSpanPerFailoverAttempt`：一次故障转移产生两条尝试 span——它钉住的是
  `startAttempt`/`finish` 与 `attemptUpstream` 的配对次数，而不是 span 内容。
- `TestStreamObservedOncePerRequest`：TTFT 与帧数在一次请求里只被观测一次（这是 M5 第一个缺陷的
  回归测试：流媒体被记了两遍）。
- `TestTraceRecordsOneRequestTrace`、`TestTraceExporterReceivesTheTrace`、
  `TestRequestIDIsConsistentAcrossHops`：一条请求一条 trace、导出器真的收到、同一个 request id
  贯穿各跳。

---

## 11. 未能确认

- **`QuotaGate` 的方法数**：任务描述里写的是"`QuotaGate` 三个方法"，但源码里这个接口声明了**四个**
  方法（`Enabled`、`Admit`、`Settle`、`Release`）。本册按源码逐符号覆盖了四个。
- **本机跑不了 `go test`**：`go.mod` 要求 go ≥ 1.26，本机 Go 工具链版本不满足，且自动下载工具链被
  沙箱拒绝。因此本册没有运行任何 `go test`（包括 `internal/repofmt` 的自检测试）。所有"锚点/符号
  存在"的结论都来自对文件的静态比对：路径与符号逐个 grep 过，正文里的行号引用为零。
- **旧稿里的两个符号并不存在**：旧稿提到 `requestTrace.finish` 里有 `TryLock`，以及 `err_rate`、
  `cache_status`、`span_count`、`retry_count` 这些属性名。源码里 `requestTrace` 明确不加锁（注释
  说它只被服务该请求的 goroutine 触碰），属性名实际是 `"cache"`、`"cache_reason"`、`"idempotency"`
  等。本册按源码写，未采用旧稿的这些说法。
- **依赖包内部未读**：`quota.Estimate`、`quota.Decision`、`quota.Reservation`、`quota.Usage`、
  `quota.Action*`/`quota.Reason*` 常量、`idempotency.Decision`、`idempotency.Entry`、
  `idempotency.KeyOf`、`idempotency.DefaultMaxResponseBytes`、`cache.Request`/`Result`/`Entry`/`KindSemantic`、
  `sse.NewWriter`/`NewReader`/`NextFrame`、`tracing.Parse`/`NewRootContext`/`NewSpanID`/`Context.Remote`
  等，只按网关侧的用法与注释推断语义，没有逐个读它们的实现——本册范围限定在 `internal/gateway`。
  例如 `idempotency.Complete` 为什么把超限与"已释放"合并成一个 `stored=false` 的返回值，其内部
  顺序（`Rename`→`Save`→`Release` 之类）在 `internal/idempotency` 里，本册不据以断言。
- **`record` 的字段装配点未展开**：`cacheIdent`、`idemCapture`、`quotaRes` 之外的字段（如
  `explicitModel`、`tried`、`route`）由 `proxy.go` 的 `serve` 与 `attemptUpstream` 写入，本册只在
  必要处引用，没有逐字段走读 `proxy.go`。
- **`sortedTargets` 的就地排序影响未实测**：它调用 `p.upstreams.Targets()` 后 `sort.SliceStable`
  就地排序。若该方法返回的是注册表内部切片，那么在 `CapabilityReport()` 里排序会**改动路由顺序**。
  这一点只从调用形态推断，未运行验证（跑不了测试）。




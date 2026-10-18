# 第 04 册 · 缓存 · 配额 · 向量 · Redis · 账本与统计

## 1. 本册范围

只走读 `internal/cache/cache.go`、`internal/cache/memory.go`、`internal/cache/cache_test.go`。Redis 后端（`internal/cache/redis.go`）属于本册后续部分，这里只在两处必须对照时提及：`Store` 接口的并发契约，以及"内存是 LRU / Redis 是 FIFO"这条被测试钉死的分歧。

## 2. 包注释定调 (internal/cache/cache.go)

包注释把整个包的设计归因于一个不对称：**MISS 无害，而 WRONG HIT 是调用方看不见的缺陷**。

源码注释的观点（逐条翻译）：
- 收到"对另一个问题的貌似合理答案"的 Agent 会照着它行动，所以只要身份不确定，本包每一条规则都宁愿"再问一次模型"。
- 条目按 tenant+model 分域，绝不跨域共享。
- 要求随机性的请求（temperature > 0）根本不做语义匹配，因为它想要的答案按定义就是变化的。
- 带 tools 的请求只允许精确匹配，因为同一句话的两种说法可以合法地选中不同的工具。
- 只存成功响应，错误信封永远不会被当作答案重放。
- 相似度阈值是**实测**出来的而不是猜的（见 `docs/DESIGN.md` 的 M2 一节）。

包无 HTTP、无 config 依赖：网关把解析后的 body 构造成 `Request`，并自行决定怎么把 `Entry` 送出去。

## 3. 默认常量 (internal/cache/cache.go)

源码注释的观点：这些值都放进了 `Config`，运营商可以按部署调整；在这里写成常量，是为了"没有任何东西是隐式的"。

- `DefaultThreshold` (internal/cache/cache.go)：新 prompt 要通过余弦相似度被判为同一问题，必须达到的下限。源码注释明确它是 **MEASURED, not chosen**：`cmd/measure-m2` 拿 `internal/evalset` 的标注语料（26 对人工标注：13 对必须命中、13 对必须不命中）跑离线 embedder 并扫阈值。取值理由（源码注释原文数据的翻译）：
  - 0.86 时该语料命中率 62%（8/13），**零错答**。
  - 更松的 0.84 换来 77% 命中率，代价是它开始匹配三对真错答：「delete the cache entry for tenant alpha」vs「tenant beta」、「write a unit test for the router」vs「the breaker」、「reset my password for the admin console」vs「the user console」——每 13 对多两次命中，付出的是一个错的 tenant、一个错的包、一个错的 console。源码注释的结论句：**Misses cost money; wrong answers cost trust.**（少命中花的是钱，错答案花的是信任。）
  - 之前用的 0.94 来自直觉，同一语料上只有 **31%** 命中率，所以这个常量从此必须引用证据。
  - 天花板是 **EMBEDDER 的属性，不是阈值的属性**：语料里有 5 个释义的分数低于它们最近的 near-miss，任何阈值都分不开。把 `cache.Embedder` 换成真模型才能抬高天花板，然后重跑扫描找新的位置。
  - 0.86 与 0.88 是整条"零错答货架"（两者都是 8/13）；扫描自己的建议落在 0.88 那一端，而这个常量取另一端，因为语料里最紧的真释义测出来是 **0.8819**——是刀刃，不是余量。0.90 会把命中率压到 6/13。
- `DefaultTTL = 15 * time.Minute` (internal/cache/cache.go)：答案最多允许陈旧多久。源码注释的观点：Agent 在一次会话内大量重放、跨天很少，所以有用窗口很短。
- `DefaultMaxEntriesPerScope = 256` (internal/cache/cache.go)：单个 tenant+model 工作集的上界；越过它时 store 驱逐最近最少使用的条目。
- `DefaultMinPromptChars = 12` (internal/cache/cache.go)：跳过 "hi"、"continue" 这类琐碎 prompt——重新问一次很便宜，而且正因为短，它们是最容易被错误匹配的。

### 比较方向
源码里的判断是 `len([]rune(r.Prompt)) < cfg.MinPromptChars`，即阈值比较是**包含式**的：rune 数恰好等于 `DefaultMinPromptChars`（12）**通过**，只有 11 及以下被拒。相似度同理：`MemoryStore.Search` 用 `sim < threshold` 过滤，所以 `sim == threshold` 算命中。

## 4. 命中类型与哨兵错误 (internal/cache/cache.go)

- `KindExact = "exact"` (internal/cache/cache.go)：精确命中的标签。
- `KindSemantic = "semantic"` (internal/cache/cache.go)：语义命中的标签。源码注释的观点：`Kind` 会报给调用方（一个 header 加一个 metric label），好让运营商看见究竟是语义路径在干活，还是精确路径在扛着缓存。
- `ErrDisabled` (internal/cache/cache.go)：`errors.New("cache: disabled")`。源码注释的观点：一个被关掉的 Cache 返回它，好让调用方区分"根本没配缓存"和"缓存答不出来"。**诚实备注**：当前仓库内没有任何地方引用这个值——`Cache.Lookup` 走的是 `Decision.Reason = "cache disabled"` 而不是它，所以它是一个已经声明、尚未接线的预留哨兵。

## 5. `Entry` — 存储单元 (internal/cache/cache.go)

源码注释的观点：`Entry` 是两种后端共用的存储单元，所以内存与 Redis 不会在"存了什么"上发生漂移。

- `Entry.Scope`：隔离边界，tenant 与 model 合起来。不同 scope 的两个请求永远看不见彼此的条目。
- `Entry.Key`：精确匹配身份，canonical 请求体（model + messages + 采样参数）的哈希。两个逐字节相同的请求共享它，同时它也是存储主键。
- `Entry.Model`：产出该答案的模型。之所以记录，是因为一个 scope 通常只覆盖一个模型，但 fallback 可能用另一个模型服务过。
- `Entry.Prompt`：被 embedding 的那段文本。留着是给 `/admin/cache/lookup` 解释"为什么这两句被认为相似"。
- `Entry.Signature`：把 prompt 之外一切能改变答案的输入取指纹——temperature、top_p、seed、stop、tool schema、response format，以及问题之前的对话。语义匹配必须在这个字段上一致。源码注释的例证：「Summarise the design document」在 temperature 0 和 temperature 1 下是不同的请求，不同 tool schema 下也是不同请求，互相顶上就是没人能察觉的静默行为改变。
- `Entry.Vector`：`Prompt` 的 embedding，已归一化。
- `Entry.Body`：客户端本会收到的原始响应载荷，含 OpenAI 信封。逐字节重放，让缓存答案与实时答案不可区分。
- `Entry.Status`：随 body 一起重放的状态码。
- `Entry.ContentType`：随 body 一起重放的内容类型。
- `Entry.Headers`：随 body 一起重放的响应头。
- `Entry.Upstream`：记录当初是哪个后端答的，让重放出来的答案仍能说明自己的来源（重放本身会被标记）。
- `Entry.PromptTokens`：原始响应的提示 token 记账。源码注释的观点：重放一个答案不能把它的成本当成新计费再记一遍，但数字要留着，好让一次命中能报出它省掉的 token。
- `Entry.CompletionTokens`：原始响应的补全 token 记账，与 `Entry.PromptTokens` 一起构成一次命中能申报的节省量。
- `Entry.CreatedAt` 与 `Entry.ExpiresAt`：驱动 TTL。源码注释的观点：存的是绝对 `ExpiresAt` 而不是一个时长，这样 Redis 条目和内存条目是对着同一个墙上时钟过期的。
- `Entry.Hits`：该条目被重放的次数，让运营商看见哪些 prompt 真的在重复。它是 best-effort 的：命中是在服务之后才计数的。
- `Entry.Expired(now time.Time) bool` (internal/cache/cache.go)：`return !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt)`。零值 `ExpiresAt` 永不过期；`now` 恰好等于 `ExpiresAt` 时**算过期**（`!now.Before`）。只读，不加锁，被 `Lookup`、两个后端的 `Get`/`Search`/`sweep` 调用。

## 6. `Match` 与候选上限 (internal/cache/cache.go)

- `Match` (internal/cache/cache.go)：一次搜索结果 = 条目 + 相似度。字段：`Match.Entry`（命中的条目）、`Match.Similarity`（它有多像）。没有任何方法。
- `semanticCandidates = 4` (internal/cache/cache.go)：包级常量，限制应用 signature 过滤之前先查看多少个近邻。源码注释的观点：4 足够扛住一个 scope 里存在几个不同 temperature 或 tool schema 的情况，又不至于把一次查找变成对 scope 全部内容的扫描。它是 `Lookup` 传给 `Store.Search` 的 `limit`。

## 7. `Store` 接口 — 持久化契约 (internal/cache/cache.go)

源码注释的观点：两个实现都必须对并发使用安全，且 `Get`/`Search` **不得返回已过期条目**。`Search` 有意做成对一个 scope 的全量扫描而不是近似最近邻索引：一个 scope 存几百条、向量 512 维、几百个向量的余弦只要几十微秒；引入 ANN 索引会多一个依赖和一个要调的 recall 参数，而在有证据说明需要它之前不该加（见 `docs/DESIGN.md` M2）。

- `Get(ctx context.Context, scope, key string) (Entry, bool, error)`：返回精确 key 下的条目。`bool` 表示是否存在；约定不得返回过期条目。
- `Put(ctx context.Context, e Entry, ttl time.Duration) error`：存入条目，替换同 key 的旧条目。
- `Search(ctx context.Context, scope string, vec []float32, threshold float64, limit int) ([]Match, error)`：返回该 scope 中与 `vec` 相似度**至少** `threshold` 的条目，最相似的在前，最多 `limit` 条。
- `Delete(ctx context.Context, scope, key string) (bool, error)`：删一条，`bool` 报告是否真删掉了东西。
- `Flush(ctx context.Context, scope string) (int, error)`：清空一个 scope；`scope` 为空时清空所有 scope；返回删除条数。
- `Len(ctx context.Context, scope string) (int, error)`：数一个 scope 的条目数（源码注释注明：对内存后端 0 意味着"所有 scope"；Redis 后端在无法廉价计数时返回 -1）。
- `Stats() StoreStats`：给 `/admin/cache` 用的后端计数器。
- `Name() string`：在日志和管理响应里标识后端。
- `Close() error`：释放资源，源码注释明确要求**必须幂等**。
- 实现方：`MemoryStore` (internal/cache/memory.go) 与 `RedisStore` (internal/cache/redis.go)。

## 8. `StoreStats` — 后端自视图 (internal/cache/cache.go)

全部字段 `int64`，全部带 `json` 标签，全部只在持锁时被改：
- `StoreStats.Gets`：`Get` 被调用次数。
- `StoreStats.Puts`：`Put` 被调用次数（含替换）。
- `StoreStats.Deletes`：`Delete` 被调用次数。
- `StoreStats.Flushes`：`Flush` 被调用次数。
- `StoreStats.Searches`：`Search` 被调用次数。
- `StoreStats.Scanned`：搜索期间比较过的条目数（扫描成本可观测）。
- `StoreStats.Evicted`：被 LRU/FIFO 裁剪丢掉的条目。
- `StoreStats.Expired`：因 TTL 到点被丢掉的条目。
- `StoreStats.Errors`：后端失败次数（Redis 挂了会体现在这里）。

## 9. `Config` 与 `Config.WithDefaults` (internal/cache/cache.go)

- `Config.Enabled`：总开关。源码注释的观点：被禁用的 Cache 对每次 `Lookup` 都答 miss，网关就是这么跑"没有 cache: 段"的配置的。注意 `WithDefaults` **不**给它兜底，所以零值即关闭。
- `Config.Threshold`：语义命中的最小余弦相似度。
- `Config.TTL`：条目可用多久。
- `Config.MaxEntriesPerScope`：单个 tenant+model 工作集的上界。
- `Config.MinPromptChars`：短于它就直接跳过，因为不安全。
- `Config.AllowNondeterministic`：是否放 temperature > 0 的请求进语义路径。默认关。源码注释的观点：重放一个随机请求的某一次采样是行为改变，不是优化。
- `Config.AllowTools`：是否放带 tools 的请求进语义匹配。默认关；它们仍然可被精确匹配缓存。
- `Config.Now func() time.Time`：覆盖时钟，供测试用。
- `Config.WithDefaults() Config` (internal/cache/cache.go)：值接收者，逐项兜底——`Threshold <= 0` 取 `DefaultThreshold`，`TTL <= 0` 取 `DefaultTTL`，`MaxEntriesPerScope <= 0` 取 `DefaultMaxEntriesPerScope`，`MinPromptChars <= 0` 取 `DefaultMinPromptChars`，`Now == nil` 取 `time.Now`。被 `New` 在构造时调用一次，被 `Cache.Config()` 间接暴露。

## 10. `Request`、`Decision` (internal/cache/cache.go)

源码注释的观点：`Request` 是网关在问缓存之前所知道的一切。缓存永远看不到 HTTP 请求——body 已解析完、scope 已推导完，于是**策略决策集中在一处（本包），传输决策集中在另一处**。

- `Request.Scope`：隔离域，见 `ScopeFor`。
- `Request.Model`：请求的模型名。
- `Request.Prompt`：要 embedding 的文本。源码注释的观点：调用方只传用户可见内容（system + user 轮次），不传 JSON 信封——哈希信封会让两次语义相同但 key 顺序不同的调用互相 miss。
- `Request.ExactKey`：canonical 请求身份。两个会向上游发出相同字节的请求共享它。
- `Request.Signature`：答案塑形输入的指纹。语义匹配必须一致；精确匹配按构造天然一致。
- `Request.HasTools`：该请求携带 tool 定义或 tool 结果。
- `Request.Nondeterministic`：采样参数在要求变化（temperature > 0、top_p < 1、n > 1）。
- `Request.Bypass`：不读缓存，但仍然允许写。
- `Request.Refresh`：什么都不读，模型答完后替换那条条目。
- `Decision`：解释一个请求到底能不能用缓存。源码注释的观点：之所以把它返回出去，是为了让网关能把原因放进 header 与日志字段——"为什么这个没被缓存"是运营商问的第一个问题。
- `Decision.Lookup`：是否允许读。
- `Decision.Store`：是否允许写。
- `Decision.Semantic`：是否允许相似度匹配，而不只是精确身份。
- `Decision.Reason`：短的机器可读解释（"temperature > 0"、"prompt too short"、"tools present"）。源码注释：为空表示完全可缓存。

## 11. `Cache.Cacheable` — 策略纯函数 (internal/cache/cache.go)

签名 `func (c *Cache) Cacheable(r Request) Decision`。源码注释的观点：它是 request 与 config 的纯函数，所以每条规则都能在没有 store 的情况下单测。只读 `c.cfg`（局部 `cfg := c.cfg` 取一次快照），不加锁，不碰 store，不产生任何计数——错误路径只有"返回一个带 Reason 的零值 Decision"。

短路顺序（顺序本身就是策略，先命中先返回）：
1. `!cfg.Enabled` → `Decision{Reason: "cache disabled"}`。三个 bool 全为 false。
2. `strings.TrimSpace(r.Prompt) == ""` → `Decision{Reason: "empty prompt"}`。注意是 TrimSpace 后的判断，所以纯空白算空。
3. `len([]rune(r.Prompt)) < cfg.MinPromptChars` → `Decision{Reason: fmt.Sprintf("prompt shorter than %d characters", cfg.MinPromptChars)}`。按 **rune** 计数不是字节；默认配置下这条的字符串正是 `prompt shorter than 12 characters`。
4. `r.ExactKey == ""` → `Decision{Reason: "request has no canonical identity"}`。
5. 通过上面全部关卡后，`d := Decision{Lookup: true, Store: true, Semantic: true}`。源码注释的观点：**写默认允许，而且只会被上面这些关卡拿走**——"值得返回的答案就值得记住"。`Lookup` 与 `Store` 是独立标志，这正是 bypass/refresh 有意义的原因：一个知道自己答案变了的客户端，可以在路过时修复缓存，而不只是放弃读取。
6. `switch` 两条互斥分支（只影响 `Semantic` 与 `Reason`，不动 `Lookup`/`Store`）：
   - `r.HasTools && !cfg.AllowTools` → `Semantic = false`，`Reason = "tools present: exact match only"`。源码注释的观点：tools **是可缓存的**，但只有完全相同的请求才允许命中——同一问题的两种说法可以合法地选中不同工具，而重放一个调用方没要过的 tool_call 比付一次调用更糟。
   - `r.Nondeterministic && !cfg.AllowNondeterministic` → `Semantic = false`，`Reason = "temperature > 0: exact match only"`。
   因为是 `switch` 而非两个 `if`，两者同时成立时 Reason 是 **tools 那条**。
7. `if r.Bypass` → `Lookup, Semantic = false, false`，`Reason = "bypass requested"`，**立即 return d**。源码注释的观点：语义匹配也必须关掉——一个说了"别用缓存"的调用方，不会想让别人的问题的释义在它的新答案到达前先答它。
8. `if r.Refresh` → `Lookup, Semantic = false, false`，`Reason = "refresh requested"`（不 return，但已是函数末尾）。
9. 关键不变量：**Bypass 与 Refresh 都保留 `Store = true`**。二者优先级上 Bypass 先判且提前返回，所以同时置位时 Reason 是 `"bypass requested"`。
- 被谁调用：`Cache.Lookup` 与 `Cache.StoreResponse` 各自在开头调用一次（同一个请求在一次读一次写里被判两次，见下文"重跑"）；网关侧由 `internal/gateway/cachepath.go` 直接调用一次用于记录决策。

## 12. `Result` 与 `Stats` (internal/cache/cache.go)

- `Result.Hit bool`：缓存是否回答了。
- `Result.Kind string`：`Hit` 为真时是 `KindExact` 或 `KindSemantic`。
- `Result.Similarity float64`：精确命中恒为 1.0，否则是实测余弦值。
- `Result.Entry Entry`：存下来的答案。
- `Result.Age time.Duration`：该条目是多久以前创建的。
- `Result.Reason string`：解释一次 miss，在有对应 `Decision` 时与 `Decision.Reason` 一致。注意 `Lookup` 遇到 store/embedder 错误时会在这里拼上 `"store error: "` 加原始错误文本。
- `Stats`：缓存自己做了什么的计数。源码注释的观点：网关把它们报成指标，好让缓存的成效就显示在它省下的流量旁边。
- `Stats.Lookups`、`Stats.Hits`、`Stats.ExactHits`、`Stats.SemanticHits`、`Stats.Misses`：查找次数与命中/未命中的分档。
- `Stats.Stores`、`Stats.StoreErrors`、`Stats.LookupErrors`：写入成功数、写入错误数、查找错误数。
- `Stats.StaleEvictions`：因为发现过期而被顺手删掉的条目数。
- `Stats.Collapsed`：等待了同一次在飞调用而合并掉的请求数。
- `Stats.SavedPromptTokens` / `Stats.SavedCompletionTokens`：源码注释的观点是这里最容易被误用的一点——它们是"一次命中**没有**重新生成"的、provider 报出的 token 数。它们计在这里，**绝不**加到上游 token 总量里：从 provider 的视角生成只发生过一次，把重放再计一遍会让消耗量正好被虚增出缓存省下的那部分。

## 13. `Cache` 与 `call` (internal/cache/cache.go)

- `Cache` (internal/cache/cache.go)：语义缓存 = `Store` + `Embedder` + 策略。
- `Cache.cfg Config`：构造时经 `WithDefaults` 定稿，之后只读。
- `Cache.store Store`：后端，可为 nil（`Close` 对 nil 安全）。
- `Cache.emb embed.Embedder`：可为 nil，语义路径会因此整条让路。
- `Cache.log *slog.Logger`：`New` 里 nil 会被换成 `slog.Default()`，所以 `StoreResponse` 可以无条件用。
- `Cache.mu sync.Mutex`：保护 `stats` 与 `inflight` 两样东西。
- `Cache.stats Stats`：一份大锁保护的聚合计数。
- `Cache.inflight map[string]*call`：把相同的并发 miss 合并成一次上游调用。源码注释的观点：没有它，缓存在最要紧的时候预热最差——一串首次请求全部 miss、全部付费。
- `call` (internal/cache/cache.go)：一次在飞调用的会合点。
- `call.done chan struct{}`：由领跑者 `close`，等的人收广播。
- `call.res any` / `call.err error`：领跑者的结果。领跑者在 `close(call.done)` **之前**写这两个字段，等的人在 `<-cl.done` **之后**读，靠 channel 关闭建立 happens-before；结果值是共享的同一个 `any`，不复制。

## 14. 构造与访问器 (internal/cache/cache.go)

- `New(cfg Config, store Store, emb embed.Embedder, log *slog.Logger) *Cache`：先 `cfg.WithDefaults()`，`log == nil` 时换成 `slog.Default()`，然后返回填好 `inflight` map 的 `*Cache`。源码注释的观点：**nil embedder 意味着只做精确匹配**，这是一个合法配置（也是 embedding 端点不可达、而网关必须继续服务时的回退）。被 `internal/server/cache.go` 调用。
- `Cache.Store() Store`：暴露后端，管理面用（`/admin/cache` 报它的 stats，并对它做类型断言找 `Scopes`）。被 `internal/server/cache.go` 与 `internal/server/server.go` 调用。
- `Cache.Config() Config`：返回生效配置。测试用它断言阈值。
- `Cache.Embedder() embed.Embedder`：暴露 embedder，可能为 nil。源码注释的观点：管理面需要它来回答"为什么这个**没**命中"——没有它，唯一诚实的回答就是"相信阈值吧"。被 `internal/server/cache.go` 调用。
- `Cache.Close() error`：`nil` 接收者或 nil store 直接返回 nil，否则转交 `store.Close()`。源码注释的观点：对 nil store 安全、调两次也安全——停机可能跑在一条失败的启动路径之后。被 `internal/server/cache.go` 调用。
- `Cache.Stats() Stats`：加锁读快照，返回值拷贝而非指针。被 `internal/server/server.go` 与 `internal/server/cache.go` 调用。
- `Cache.AddStats(delta Stats)`：加锁，把 delta 逐字段累加进 `c.stats`。**它只合并 10 个字段**——`Lookups`、`Hits`、`ExactHits`、`SemanticHits`、`Misses`、`Stores`、`StoreErrors`、`LookupErrors`、`StaleEvictions`、`Collapsed`；`SavedPromptTokens` 与 `SavedCompletionTokens` **不在其中**，所以外部 token 节省量不会被这条路径并入。源码注释的观点：网关用上报上游 token 节省。当前仓库内除本包定义外没有生产调用方。
- `Cache.count(f func(*Stats))` (internal/cache/cache.go)：加锁、调用 `f(&c.stats)`、解锁。所有 `Stats` 变更都经它，因此不存在散落的裸写。注意 `Cache.Flush` 传进去的是空闭包，等于什么都没记。

## 15. `Cache.Lookup` — 读路径 (internal/cache/cache.go)

签名 `func (c *Cache) Lookup(ctx context.Context, r Request) (Result, error)`。源码注释的观点：**miss 不是错误**——调用方被期望去叫模型，然后 `Store`。因此除了两处 store 错误，它一律返回 `nil` error。

1. `d := c.Cacheable(r)`；`!d.Lookup` → `Lookups++`、`Misses++`，返回 `Result{Reason: d.Reason}, nil`。注意这里把"策略不许查"也算成一次 miss。
2. `Lookups++`；`now := c.cfg.Now()`（**只取一次时钟**，整条路径共用，所以条目年龄与过期判定是同一个时刻）。
3. **精确身份优先**。源码注释的观点：它是一次哈希查找、永远正确，而且是承载会话流量的那条路——Agent 会逐字重问同一件事。
   - `c.store.Get(ctx, r.Scope, r.ExactKey)` 返回 err → `Misses++`、`LookupErrors++`，返回 `Result{Reason: "store error: " + err.Error()}, err`。**这是 rare 的会把错误抛给调用方的地方**。
   - `ok && !e.Expired(now)` → `Hits++`、`ExactHits++`、按条目记下的 token 累加 `SavedPromptTokens`/`SavedCompletionTokens`；`_ = c.bumpHits(...)`（best-effort，错误被丢弃）；返回 `Result{Hit: true, Kind: KindExact, Similarity: 1, Entry: e, Age: now.Sub(e.CreatedAt)}, nil`。
   - `ok` 但已过期 → `c.store.Delete(ctx, r.Scope, r.ExactKey)`（错误同样丢弃）+ `StaleEvictions++`。源码注释的观点：过期但仍在，就顺手删掉，好让后面的 `Search` 不必再检查它，也让 store 自己的计数保持诚实。**注意此时还没算 miss**，控制流继续走向语义路径。
4. **语义身份**。`!d.Semantic || c.emb == nil` → `Misses++`，reason 取 `d.Reason`；若 `d.Reason == ""` 且 `c.emb == nil` 则改成 `"no embedder configured"`，返回 `nil` error。
5. `c.emb.Embed(ctx, []string{r.Prompt})`；`err != nil || len(vecs) == 0` → `Misses++`、`LookupErrors++`，reason 为 `"embedder failed"`，有 err 时拼上 `": " + err.Error()`，返回 `nil` error。源码注释的观点：**embedding 失败把缓存降级为精确匹配；它绝不能把请求弄失败，因为缓存只是优化。**
6. `c.store.Search(ctx, r.Scope, vec, c.cfg.Threshold, semanticCandidates)`。源码注释的观点：要**几个**候选而不是一个——scope 里最像的 prompt 可能是在不同 signature 下问的（不同 temperature、不同 tool schema、不同对话），而我们想要的答案是"以同样方式问过的、最像的那个"，不是"最像的那个，句号"。err → `Misses++`、`LookupErrors++`，返回 `Result{Reason: "store error: " + err.Error()}, err`。
7. 遍历候选，顺序即 `Search` 的 `Rank` 序：`r.Signature != "" && m.Entry.Signature != r.Signature` 则 `continue`（**签名相等过滤**；请求侧 signature 为空时该过滤整体失效）；`m.Entry.Expired(now)` 则最多只删一条（`droppedStale` 布尔哨兵守着 `c.store.Delete`），再 `continue`；否则 `best, found = m, true` 并 `break`——取第一个同时通过签名与新鲜度的候选，不是"分数最高的那个"。
8. `!found` → `Misses++`；`droppedStale` 为真再加 `StaleEvictions++`；reason 为 `"no entry above threshold"`，若 `droppedStale` 则为 `"best match expired"`；返回 `nil` error。
9. 命中 → `Hits++`、`SemanticHits++`、按 `best.Entry` 累加两个 Saved token 计数；`_ = c.bumpHits(ctx, r.Scope, &best.Entry)`；返回 `Result{Hit: true, Kind: KindSemantic, Similarity: best.Similarity, Entry: best.Entry, Age: now.Sub(best.Entry.CreatedAt)}, nil`。
- **每次查找最多删一条过期项**：精确路径删 1 条；语义路径靠 `droppedStale` 也只删 1 条，其余过期候选只是被跳过。这是有意的——查找路径不该承担批量清扫，那是 janitor 的活。
- 并发：`Lookup` 自己不持 `c.mu` 做 I/O，只在 `count` 里短暂持锁；对同一 scope 的并发写由 store 的锁保证。
- 被谁调用：`internal/gateway/cachepath.go`。

## 16. `Cache.bumpHits` (internal/cache/cache.go)

签名 `func (c *Cache) bumpHits(ctx context.Context, scope string, e *Entry) error`。作用：`e.Hits++`，然后按 `ttl := time.Until(e.ExpiresAt)`（`ttl <= 0` 时回退成 `c.cfg.TTL`）把 `*e` 原样 `Put` 回去。读 `c.cfg.TTL` 与 `e.ExpiresAt`，写 store。

关键点：**它不改 `ExpiresAt`，所以命中不延长截止时间**。`Entry` 里带的是绝对到期时刻，而 `Put` 的 `ttl` 只对"按 key 设过期"的后端有意义——它存在的目的是让 Redis 那个 key 的 TTL 与原始截止时刻对齐，不是给条目续命。零值 `ExpiresAt` 的条目会拿到 `c.cfg.TTL`。

源码注释的观点：它是 best-effort 的——计数失败绝不能把一次成功的命中变成调用方的错误。两个调用点都用 `_ =` 丢弃返回值。指针接收者意味着它就地改调用方手里那个 `Entry` 的 `Hits`。

## 17. `Cache.StoreResponse` — 写路径 (internal/cache/cache.go)

签名 `func (c *Cache) StoreResponse(ctx context.Context, r Request, e Entry) error`。

源码注释的观点：调用方把网关的判决传进来（status 是 2xx、body 是完整非流式答案），而**缓存会重跑一遍自己的策略**，所以一个接错线的调用方也没法把错误或随机采样存进去。这就是"重跑 `Cacheable`"的含义。

1. `d := c.Cacheable(r)`；`!d.Store` → **返回 nil**（静默跳过，不报错也不计数）。所以禁用/空 prompt/过短/无 identity 四种情况在这里什么都不写。
2. `now := c.cfg.Now()`；用 `r` 覆写 `e.Scope`、`e.Key`、`e.Prompt`、`e.Model`、`e.Signature`——调用方传进来的 `Entry` 只提供 body/status/headers/upstream/用量，身份一律以 `Request` 为准。
3. `e.CreatedAt = now`；`e.ExpiresAt = now.Add(c.cfg.TTL)`。源码注释的观点：**截止时间锚定在创建时刻，命中时不会刷新**——一个老的答案就是老的，不管它多受欢迎。
4. 向量在**存储时**计算（条件：`e.Vector == nil && c.emb != nil && strings.TrimSpace(r.Prompt) != ""`）。源码注释的观点：一个存了答案却没存向量的缓存永远不可能被语义找到；而且存储时做 embedding 比每次 miss 时做更便宜——miss 会重复，存储只发生一次。成功且 `len(vecs) > 0` 时取 `vecs[0]`；只有 `err != nil` 时打一条日志：`c.log.Debug("cache: embedding for store failed, storing exact-match only", slog.String("error", err.Error()))`。**`err == nil` 但 `len(vecs) == 0` 的退化情况既不报错也不记日志**，条目就这样以"仅精确匹配"入库。
5. `c.store.Put(ctx, e, c.cfg.TTL)`；err → `StoreErrors++` 并把错误**返回给调用方**；成功 → `Stores++`，返回 nil。
- 被谁调用：`internal/gateway/cachepath.go`。
- 与 `Lookup` 的呼应：因为 `Bypass`/`Refresh` 保留 `Store = true`，这两类请求**不读但会写**，缓存因此能在路过时被修复。

## 18. `Cache.Flush` (internal/cache/cache.go)

签名 `func (c *Cache) Flush(ctx context.Context, scope string) (int, error)`。转交 `c.store.Flush(ctx, scope)`；err 为 nil 时调一次 `c.count(func(s *Stats) {})`。那个闭包是**空的**——它上线了锁但目前不改任何字段，所以 `Cache.Flush` 实际上不更新任何 `Stats`（`Stats` 里也没有 Flush 字段）。返回删除条数。被 `internal/server/cache.go` 调用。

## 19. `Cache.Do` — 合并并发未命中 (internal/cache/cache.go)

签名 `func (c *Cache) Do(ctx context.Context, key string, fn func() (any, error)) (any, bool, error)`。返回 `(值, 是否被合并, 错误)`。源码注释给的用法示例是 `v, err := c.Do(ctx, req.ExactKey, func() (any, error) { return callUpstream() })`。

- 源码注释的观点：第一个调用者跑 `fn`，其余的人等着收它的结果，所以 N 个相同请求的突发只花一次上游调用。
- **只在精确 key 上合并**：源码注释明确解释，合并释义需要答案"相似"，而把一个调用方的答案返回给另一个问题，正是本包拒绝引入的那个缺陷。语义相同的不同措辞不会被合并。
- `key == "" || !c.cfg.Enabled` → 直接 `fn()`，返回 `(v, false, err)`，完全绕过合并表。
- 加锁查 `c.inflight[key]`；不存在就建 `&call{done: make(chan struct{})}` 并登记；解锁。
- 已存在（等的人）→ `select`：`<-cl.done` 则 `Collapsed++` 并返回 `(cl.res, true, cl.err)`；`<-ctx.Done()` 则返回 `(nil, false, ctx.Err())`——**注意取消路径报的是 `false`**，即被取消的等待者不算"被合并"，也不加 `Collapsed`。
- 领跑者 → `cl.res, cl.err = fn()`；加锁 `delete(c.inflight, key)`；解锁；`close(cl.done)`；返回 `(cl.res, false, cl.err)`。领跑者本身不计 `Collapsed`，所以 N 个并发里恰好记 N-1 次。
- 错误路径不去毒化 key：`delete` 在 `fn` 返回后无条件执行，所以一次失败的调用不阻塞下一次（由测试钉住）。
- 并发：`c.mu` 只保护 `inflight` 的登记/注销，`fn` 在锁外执行，所以慢的上游调用不会冻结整张表。
- 被谁调用：当前仓库内只有 `internal/cache/cache_test.go` 直接调用，生产路径尚未接入。

## 20. `ScopeFor` 与 `sanitiseModel` (internal/cache/cache.go)

- `ScopeFor(tenant, model string, capabilities ...string) string`：sha256 依次写入 `tenant`、`0` 字节、`model`，然后对每个 capability 写 `0` 字节加它的内容；取十六进制摘要的**前 16 个字符**；返回 `fmt.Sprintf("%s/%s", sanitiseModel(model), sum)`。源码注释的观点：tenant 来自调用方自己的凭证（**绝不**来自客户端提供的 header，否则一个 tenant 就能读到另一个 tenant 的答案），model 也是组成部分，因为同一个 prompt 不该被另一个模型的文本回答。前缀部分让 Redis key 在 redis-cli 里还能被 grep 到，而哈希让 key 有界、不含需要转义的字符。**capability 的顺序参与哈希**，顺序不同即不同 scope。
- `sanitiseModel(model string) string`：`model == ""` 返回 `"any"`；逐 rune 保留 `a-z`、`A-Z`、`0-9`、`-`、`.`、`_`，其余一律替换成 `_`；结果为空再次返回 `"any"`；超过 48 个**字节**则截断到 48。因为输出已被限制为 ASCII，按字节截断是安全的。未被导出，只被 `ScopeFor` 调用。
- 被谁调用：`internal/gateway/cachereq.go` 用 `cache.ScopeFor(ident.Tenant, f.Model, capabilitiesFor(r)...)` 推导隔离域。

## 21. `ExactKeyFor` 与 `Rank` (internal/cache/cache.go)

- `ExactKeyFor(model string, canonicalBody []byte, extra ...string) string`：sha256 依次写入 `model`、`0` 字节、`canonicalBody`，再对每个 `extra` 写 `0` 字节加内容，返回**完整**的 64 字符十六进制摘要（对比 `ScopeFor` 只取 16 字符）。中间插入的 `0` 分隔符是为了让 `("m","{}")` 与别的拼接方式不会撞成同一个摘要。源码注释的观点：会向上游发出相同字节的两个请求必须产出相同 key，而任何可能改变答案的差异都必须让 key 不同。被 `internal/gateway/cachereq.go` 调用（把 `r.URL.Path` 与 tenant 作为 `extra` 一起并入）。
- `Rank(matches []Match)`：用 `sort.SliceStable` 就地排序，主键是 `Similarity` 降序；相等时按 `Entry.CreatedAt` 更晚者优先。源码注释的观点：平手时按近因打破，好让确定性的那个答案赢过非确定性的那个。稳定排序意味着完全平手的候选保持调用方给的相对顺序。被 `MemoryStore.Search` (internal/cache/memory.go) 与 `RedisStore.Search` 在返回前调用。

## 22. `MemoryStore` — 进程内后端 (internal/cache/memory.go)

源码注释的观点：它是默认后端，因为不需要任何依赖，而且单副本网关能从中拿到全部收益；Redis 存在的意义是让缓存活过重启并被多副本共享。驱逐是**真 LRU**（双向链表），不是"随便删一个 key"的偷懒做法——Agent 的对话历史正是近因至关重要的访问模式，驱逐那条马上会被再问一次的条目，会把命中率变成抛硬币。

- `MemoryStore.mu sync.Mutex`：一把大锁，保护 `scopes` 与 `stats` 两样东西（所有方法都在入口拿它）。
- `MemoryStore.scopes map[string]*memScope`：scope 名到该 scope 状态的映射；空的 scope 会被删掉而不是留空壳。
- `MemoryStore.maxPerScope int`：LRU 上界；构造时 `<= 0` 就取 `DefaultMaxEntriesPerScope`。
- `MemoryStore.now func() time.Time`：时钟，nil 时取 `time.Now`，测试用注入时钟。
- `MemoryStore.stats StoreStats`：`StoreStats` 是值类型嵌在这里，所以内存后端有自己的计数器实例。
- `MemoryStore.closeCh chan struct{}`：关掉 janitor 的通道。
- `MemoryStore.once sync.Once`：让 `Close` 幂等。
- `memScope` (internal/cache/memory.go)：一个 scope 的状态。`entries` 负责 O(1) 查找、`lru` 负责顺序，两者必须始终同步增删。
- `memScope.entries map[string]*list.Element`：key 到"持有 `*Entry` 的链表元素"的映射。各方法里的类型断言 `el.Value.(*Entry)` 假定链表里只放 `*Entry`。
- `memScope.lru *list.List`：最近使用顺序，源码注释原文是 `front = most recently used`。`Put` 用 `PushFront` 入列、`Get` 用 `MoveToFront` 提升、驱逐取 `Back()`。
- `MemoryOptions` (internal/cache/memory.go)：`MemoryOptions.MaxEntriesPerScope int`（LRU 上界，0 取 `DefaultMaxEntriesPerScope`）；`MemoryOptions.Now func() time.Time`（覆盖时钟，测试用）；`MemoryOptions.SweepInterval time.Duration`（janitor 扫过期条目的间隔，0 表示一分钟，**负值表示禁用 janitor**——留给想观察惰性过期的测试）。

## 23. `MemoryStore` 的构造与后台 (internal/cache/memory.go)

- `NewMemoryStore(opts MemoryOptions) *MemoryStore`：建 map 与 `closeCh`，`maxPerScope <= 0` 时取 `DefaultMaxEntriesPerScope`，`now == nil` 时取 `time.Now`，`SweepInterval == 0` 时取 `time.Minute`；`interval > 0` 才 `go s.janitor(interval)`。所以负值不是"零间隔疯狂扫"，而是彻底不启后台 goroutine。被 `internal/server/cache.go` 调用。
- `MemoryStore.janitor(interval time.Duration)`：`time.NewTicker(interval)` + `defer t.Stop()`，循环 `select`：`<-s.closeCh` 就 return；`<-t.C` 就 `s.sweep()`。源码注释的观点：过期在每次读时也会惰性检查，所以 **janitor 只影响内存占用，从不影响正确性**；但没有它时，一个存了一千条一次性 prompt 的网关会一直留着它们直到被再次触碰——而那是永远不会发生的。
- `MemoryStore.sweep()`：取 `now := s.now()` 再 `s.mu.Lock()`（defer Unlock），双层遍历：对每个 scope 的每个元素，`e.Expired(now)` 则 `sc.lru.Remove(el)`、`delete(sc.entries, key)`、`s.stats.Expired++`；一个 scope 清空后从 `s.scopes` 里整条删除。全程不返回错误，也没有错误路径。持锁期间遍历整个 store，这是这个后端唯一的全局停顿点。
- `MemoryStore.scope(name string) *memScope`：惰性创建并登记一个 `memScope`。**它假定调用方已持有 `s.mu`**（它直接写 map），当前只有 `Put` 在锁内调用它。

## 24. `MemoryStore` 的方法 (internal/cache/memory.go)

- `Get(_ context.Context, scope, key string) (Entry, bool, error)`：**忽略 ctx**。取 `now := s.now()`，加锁，`s.stats.Gets++`。scope 不存在 → `(Entry{}, false, nil)`；key 不存在 → 同上。存在但 `e.Expired(now)` → `lru.Remove` + `delete` + `stats.Expired++` → `(Entry{}, false, nil)`（**惰性过期，顺手删掉**）。否则 `sc.lru.MoveToFront(el)` 并返回 `cloneEntry(e), true, nil`。**返回值是拷贝**：源码注释的观点是调用方可能保留或改写这个条目（命中计数就是靠重新存回去来加的），所以绝不能让它伸手进 LRU 自己的存储里。永远返回 nil error。
- `Put(_ context.Context, e Entry, ttl time.Duration) error`：**忽略 ctx**。先 `now := s.now()`；`e.ExpiresAt.IsZero()` 时补成 `now.Add(ttl)`（**调用方给的绝对到期时刻优先于 ttl**）；`stored := cloneEntry(&e)`，随后又对 `stored.Vector` 做一次独立拷贝（`cloneEntry` 已经拷过 Vector，这行是重复的防御）。加锁，`s.stats.Puts++`，`sc := s.scope(e.Scope)`。key 已存在 → `el.Value = &stored`、`MoveToFront`、直接 return nil（**替换不触发驱逐**）。新 key → `PushFront` 并登记；然后 `for len(sc.entries) > s.maxPerScope`：取 `back := sc.lru.Back()`，nil 就 break，否则 `victim := back.Value.(*Entry)`、`lru.Remove(back)`、`delete(sc.entries, victim.Key)`、`s.stats.Evicted++`。永远返回 nil error（内存后端没有写失败路径）。注意类型断言 `el.Value.(*Entry)` 与 `back.Value.(*Entry)` 都假定链表里只放 `*Entry`。
- `Search(_ context.Context, scope string, vec []float32, threshold float64, limit int) ([]Match, error)`：**忽略 ctx**。开头 `if embed.IsZero(vec) { return nil, nil }`——源码注释的观点：算不出向量的调用方必须 miss，而不是匹配一切。取 `now := s.now()`、加锁、`s.stats.Searches++`；scope 不存在 → `nil, nil`。为每个元素 `s.stats.Scanned++`；`e.Expired(now) || len(e.Vector) == 0` 则 `continue`——源码注释的观点：没有向量的条目永远不可能被语义找到，在这里跳过它，正是"仅精确匹配"条目表现得如同仅精确匹配的原因。然后 `sim := embed.Cosine(vec, e.Vector)`，`sim < threshold` 则 `continue`（**等于阈值算通过**），否则 append `Match{Entry: cloneEntry(e), Similarity: sim}`（拷贝，保护内部存储）。全部比完后 `Rank(out)`，`limit > 0 && len(out) > limit` 时 `out = out[:limit]`。永远返回 nil error。
- **为什么 `Search` 不做 `MoveToFront`**：源码在这里没有留下解释性注释，下面是阅读代码得出的判断（非源码注释）。`Search` 是一次全量线性扫描，它自己分不清"哪个条目真的被服务了"——`Lookup` 拿到的是一批候选，只有第一个签名与新鲜度都合格的会被用上，其余只是被看一眼。如果扫描顺手把每个被看到的条目都提升为最近使用，那么一次查找就会重写整个 scope 的最近使用顺序，LRU 信号从"谁真的被服务过"退化成"谁恰好落在某次查找的阈值之上"；一个放着冷门高相似条目的 scope 每次查找都会把它们全部刷新一遍，真正的热条目反而被推向后端遭驱逐。`Get` 的 `MoveToFront` 之所以正确，恰恰因为它对应一次真实的服务。真正的淘汰信号只能来自被服务，不能来自被扫过。
- `Delete(_ context.Context, scope, key string) (bool, error)`：忽略 ctx；加锁；`s.stats.Deletes++`；scope 或 key 不存在返回 `false, nil`；否则 `lru.Remove` + `delete`，scope 空了就整条删掉，返回 `true, nil`。
- `Flush(_ context.Context, scope string) (int, error)`：忽略 ctx；加锁；`s.stats.Flushes++`；`scope == ""` 时累加所有 scope 的条目数、用新 map 整体替换 `s.scopes`、返回总数；否则取该 scope，不存在返回 `0, nil`，存在则记录 `len`、删除该 scope、返回它。**没有使用任何 `List.Init()`，整条 scope 直接从 map 摘除**，所以链表一起被回收。
- `Len(_ context.Context, scope string) (int, error)`：忽略 ctx；加锁；`scope != ""` 时返回该 scope 的条目数（不存在为 0）；否则累加所有 scope。只读，不加任何计数。
- `MemoryStore.Scopes(ctx context.Context) (map[string]int, error)`：加锁，构造一个新的 `map[string]int` 返回各 scope 的条目数（ctx 未使用）。源码注释的观点：**它的签名是刻意与 `RedisStore.Scopes` 对齐的**——`/admin/cache` 靠类型断言发现这个能力，所以一个列不出自己 scope 的内存部署，会在缓存明明工作正常的时候报告"缓存是空的"。被 `internal/server/cache.go` 以类型断言 + 接口的方式调用。
- `MemoryStore.Stats() StoreStats`：加锁返回计数器的值拷贝。
- `MemoryStore.Name() string`：常量 `"memory"`。被 `internal/server/cache.go`、`internal/server/server.go` 与测试用来区分后端。
- `MemoryStore.Close() error`：`s.once.Do(func() { close(s.closeCh) })` 后返回 nil，**幂等**，永不报错。源码注释的观点：它只停 janitor。
- `cloneEntry(e *Entry) Entry`：先做结构体浅拷贝 `out := *e`，然后把三个引用类型字段深拷贝——`Headers` 非 nil 时新建同长 map 逐键复制；`Body` 非 nil 时 `append([]byte(nil), e.Body...)`；`Vector` 非 nil 时 `append([]float32(nil), e.Vector...)`。**nil 保持 nil**（不把 nil 变成空切片）。`Scope`/`Key`/`Prompt`/`Signature`/`Model`/`Upstream`/`ContentType` 是 string（不可变），直接共享底层数据是安全的。它被 `Get`、`Put`、`Search` 调用，是内存后端"内部存储永不被外部改写"的唯一手段；`Put` 里那句重复的 `stored.Vector = append(...)` 让 Vector 被拷了两次。

## 25. `cache_test.go` 的意图 (internal/cache/cache_test.go)

文件顶部注释的观点：内存与 Redis 两个后端托着**同一套策略**，所以只在一个后端上成立的行为就是"运营商换后端时才出现的 bug"；因此除了驱逐顺序那个唯一被记录在案的分歧，每个用例都同时跑两个后端。`storeFactories` 用 `MaxEntriesPerScope: 4` 加 `SweepInterval: -1`（关 janitor，专测惰性过期）造内存 store，用一个真的 mockredis 进程造 Redis store。

最值得讲的用例（按它能钉住的不变量挑）：

- `TestStoreConformance` 的 "get put delete" 子用例：往返之后 body/status/prompt 一字不差，`Upstream` 与 `Headers` 这些重放元数据仍在；**向量长度必须原样存活**（否则 Redis 后端会静默退化成仅精确匹配，实测命中率就取决于配了哪个 store）；未知 key 必须 miss；**同 key 换 scope 必须 miss**（scope 就是隔离边界）；`Delete` 返回 true、删完再查 miss、**再删一次必须返回 false**。
- "expiry" 子用例：一个已经过期的条目 `Get` 必须 miss，且 `Search` 也必须跳过它——注释的原话是"一次对陈旧答案的语义命中，与一次对陈旧答案的精确命中是同一个缺陷"。
- "search ranks and gates"：自匹配必须排第一且相似度 ≥ 0.999（自身对自身约等于 1.0）；**提高阈值绝不能放进更多条目**（`len(tight) <= len(loose)`，这就是 M2 扫的那个旋钮）；`limit` 必须被遵守（阈值 0.0、limit 2 恰好返回 2 条）；**零向量什么都匹配不到**。
- "eviction beyond the bound"：往界为 4 的 store 里塞 7 条后 `Len` 必须是 4，而且**最新的 k3..k6 必须全部存活**——注释的原话是"驱逐那条刚写进去的条目会造出一个永远预热不起来的缓存"；只有 `s.Name() == "memory"` 时才断言 k0 已被驱逐（真 LRU 那侧的额外检查）。
- "flush"：`Flush("a")` 必须删掉恰好 2 条且不动 scope b；`Flush("")` 删光全部。
- "concurrent use"：8 个 goroutine × 20 次 Put/Get/Search；`Get` 返回 miss 是**合法**的（并发写者可能刚驱逐了它），只有 error 才算失败。
- `TestStoreStatsSurviveConcurrentReaders`：注释点明了它的来历——`Stats` 会被 `/admin` 在请求飞行中读取，所以它属于并发契约而不是私有计数器；Redis 后端的计数器曾经是从每个请求 goroutine 直接写的裸字段，**竞态检测器在 `internal/cache/redis.go` 里 `Get` 的第一个自增处抓到过这个交错**，此后所有写入点共用一把锁。用例跑 4 个写者打满所有带计数的路径 + 1 个持续 `Stats()` 的读者，最后断言 `Puts`/`Gets`/`Searches` 确实动了。
- `TestMemoryStoreIsLRU` 与 `TestRedisStoreIsFIFOAndDocumentedSo`：这一对把唯一的分歧钉成"决策"而不是"惊喜"。内存侧：写满 3 条、`Get` 一下最老的 k0、再插一条 k3，则 **k0 必须存活、k1 必须被驱逐**。Redis 侧做完全一样的动作，断言**相反**的结果：k0 必须已被驱逐，注释写道"Redis 无法报告读取顺序，所以这次触碰不能救它"。（"对近因搞错的缓存，不如对年龄诚实的缓存"——内存端把近因做对，Redis 端老实承认自己只知道年龄，两边各自都不假装。）
- `TestCacheablePolicy`：一张决策表把八种情形（普通、过短、空、无 identity、temperature > 0、tools、bypass、refresh）的 `Lookup`/`Semantic`/`Reason` 全文比对，其中过短那条的期望 Reason 正是 `prompt shorter than 12 characters`；表外还断言三件事：**refresh 仍然 Store**（"那正是它的意义"）、**bypass 仍然 Store**（好让客户端修复缓存）、**禁用的缓存 Lookup 与 Store 全为 false**；最后用 `AllowNondeterministic, AllowTools = true, true` 验证两个 opt-in 确实把 `Semantic` 放回来。
- `TestExactHitAndSemanticHit`：冷查找必须 miss → `StoreResponse` → 同一请求精确命中（`Kind == KindExact`、`Similarity == 1`、body 逐字节相同、`Upstream` 保留、`Age` 落在 0..TTL 之间）→ 一个**换过 exact key** 的释义语义命中。注释特意交代这对语料的出处：它是 `internal/evalset` 的 pp-en-5，用离线 embedder 实测 **0.8819**，舒服地高于实测默认值 0.86，所以这个测试练的是发布出去的默认值，而不是为测试发明的阈值；并明确说更"难"的释义（pp-en-1，`how do I reset my password` vs `how can I reset my password`，**0.7273**）会在这里失败，而那是词法 embedder 的真实性质、不是这条路径的 bug。接着是无关 prompt 必须 miss、**另一个 tenant 的同一句话必须 miss**，最后统计恰好是 `Hits == 2`、`ExactHits == 1`、`SemanticHits == 1`。
- `TestTTLIsAnchoredAtCreation`：注入一个可变时钟，TTL 一分钟。30 秒时命中；把时钟拨到 61 秒后**必须 miss**——失败信息就是"命中刷新了截止时间"。这是"命中不延长截止时间"的守门测试。
- `TestDoCollapsesIdenticalConcurrentMisses`：8 个 goroutine 拿同一个 key 调 `Do`，用一个 channel 卡住领跑者以保证它们真并发；断言上游函数**只跑了 1 次**、8 个返回值都是同一个答案、**恰好 7 个被标记为合并**、`Stats().Collapsed == 7`；最后断言另一个 key 不会被合并进第一个。配套的 `TestDoPropagatesTheError` 断言错误按 `errors.Is` 原样传回，并且**失败之后那个 key 必须重新可用**，不能被永久毒化。
- `TestScopeAndKeyDerivation`：`ScopeFor` 必须跨进程确定（"这才让共享 Redis 有用"），tenant 不同 / model 不同 / capability 集合不同都必须给出不同 scope，scope 必须以 `gpt-4o/` 开头（redis-cli 可读性），而 `org/model:v1 beta` 这类模型名产出的 scope **不能含 `:` 或空格**（转义安全性）；`ExactKeyFor` 必须确定、body 不同必须不同、model 不同必须不同。
- `TestNilEmbedderMeansExactMatchOnly` 与 `TestEmbedderFailureDegradesToAMiss`：两条退化路径要给出**两种不同的 Reason**——没有 embedder 时是 `no embedder configured` 且精确命中仍然工作；embedder 报错时 Reason 含 `embedder failed`、**不返回 error**、且 `Stats().LookupErrors` 必须不为零。源码注释的观点：embedder 是网络依赖，它挂了只能让缓存降级，不能让调用方失败。
- `TestMemoryJanitorDropsExpiredEntries`：用 5 毫秒的 `SweepInterval` 起真 janitor，轮询到 `Len == 0`，并断言 `Stats().Expired` 不为零。注释把它定性为**正确性问题而非内存问题**：条目活过自己的 TTL，调用方就会收到一个它可能已经不再在问的问题的答案。`SweepInterval` 取负值（其余用例）正是为了把这条路径与惰性过期分开测。
- `TestMemoryStoreStatsAreCounted`：Put/Get/Search/Delete/Flush 各一次后，五个计数器必须**精确等于 1**，且 `Scanned` 不为零——否则扫描成本就不可见了。
- 另有一组 Redis 专属用例（`TestRedisStoreDegradesWhenTheServerGoesAway`、`TestNewRedisStoreRejectsAnEmptyAddress`、`TestNewRedisStoreFailsOnAnUnreachableAddress`）属于后续部分。

## 26. 五个必须记住的判断（汇总）

- **短路顺序就是策略**：禁用 → 空 prompt（TrimSpace 后）→ rune 数 < `DefaultMinPromptChars` → 空 `ExactKey`；四道关卡之后才对 tools / 非确定性 / bypass / refresh 做判断。四个 bool 里 `Store` 只在第一道关卡外才可能变 false，`Semantic` 会被 tools、非确定性、bypass、refresh 四者中的任何一个关掉。
- **阈值包含式且实测**：`>=` 才是命中（`sim == threshold` 通过，rune 数 == 12 通过）。`DefaultThreshold = 0.86` 来自 `cmd/measure-m2` 对 `internal/evalset` 的扫描，取的是"零错答货架"上偏离扫描建议的那一端，因为最紧的真释义是 0.8819。
- **内存真 LRU，Redis 按创建时间 FIFO**：不是实现偷懒，而是 Redis 报不出读取顺序。代码选择是不去猜近因——**对近因搞错的缓存，不如对年龄诚实的缓存**，而这条分歧被两个测试分别钉死。
- **`Do` 只合并精确 key**：语义相同的不同措辞不会被合并，因为把一个调用方的答案交给另一个问题正是本包要避免的缺陷。
- **命中不延长截止时间**：`ExpiresAt` 是创建时刻锚定的绝对值，`bumpHits` 重存时把它原样带回去，`Put` 的 `ttl` 只用来让 Redis key 的过期与它对齐。"一个老的答案就是老的，不管它多受欢迎。"


## 27. 本册范围与衔接

本册是第 04 册的「额度与账本」分册，覆盖 `internal/quota`（quota.go / store.go / redis.go 及其三个测试文件）、`internal/idempotency/store.go`（含测试）、`internal/sessions/ledger.go`（含测试）、`internal/stats/stats.go`，以及 `configs/quota-local.yaml`、`configs/quota-redis.yaml` 中与配额相关的配置键。

与相邻分册的分工：

- `internal/cache` 的一切（含它的 `redis.go` 与内存 store 的 LRU/janitor 写法）归本册前段与后续小节，本册只在需要对照时提一句，不展开。
- `internal/gateway/quotapath.go`、`internal/gateway/idempotency.go`、`internal/gateway/sessionpath.go` 归 02C 册。本册只在「网关侧怎么调用这些包」的地方引用它们的签名与关键判断，不重述它们的实现细节。
- `internal/redis`（手写 RESP2 客户端）与 `internal/mockredis` 也归它册；本册只把它们当作 `quota.RedisStore` 的传输层与测试替身。

锚点约定：正文定位一律写成 `符号名 (相对路径)`，例如 `Admit (internal/quota/quota.go)`；**不写行号**，因为仓库自检会因此变红。签名放在围栏代码块里，与正文的锚点分开。

## 28. quota 包的三个设计主张（来自包注释）

`internal/quota/quota.go` 顶部有一整段设计说明，是理解全包的地图。三条主张：

1. **「先预留、后结算」是唯一同时满足「拒绝没钱可花的请求」和「按真实开销记账」的方案。**源码注释观点：网关在 provider 应答前不可能知道开销——补全长由模型决定，同一个 prompt 上的工具调用型 agent 可能吐一百 token 也可能吐一万。只做事后检查的限额什么都拦不住，因为钱在答案回来时已经花掉了。
2. **不用 Lua、不用 MULTI/EXEC。**源码注释观点：每个「查改增」都是一条 Redis 命令；本网关的 Redis 客户端**按调用借还连接**，所以 MULTI 与 EXEC 会落在不同连接上，事务既不原子也不隔离。单条 `INCRBY` 在真实 Redis 上确实原子，而且够用：计数器里已经含有每一个在途预留，因此买不起自己这份估算的请求会看到「总数已在限额之上」，随即回滚自己的预留。
3. **缓存命中的额度成本是零。**源码注释观点：额度存在的意义是限制「网关付给 provider 的钱」，而被缓存的应答不付钱给任何人。所以命中会把 token 与成本预留结算回零，只保留请求计数。这也是准入发生在缓存查找**之前**的原因：顺序对数字没影响，但只在一处做一次，能让代理的每条路径记账一致。

`推断`：第 2 条决定了整个包的形态——正因为没有事务，才需要「预留即写入」而不是「先读后写」；正因为预留本身就是写入，`overshoot`（真实用量超过预留）才必须成为一个被显式上报的数字，而不是被隐藏起来。

## 29. 三态记账协议：Reserve / Settle / Release

### 为什么不能「先查后扣」

`Admit (internal/quota/quota.go)` 对每个受限维度直接调用 `Store.Add`，把本请求的估算增量写进计数器，再用返回值与限额比较。**检查与扣减是同一条命令的返回值**，中间没有窗口。`推断`：如果改成「先 Get 看余量，再 Add 扣减」，两个并发请求会分别读到「还剩 5」而各自通过，一起把限额超卖——这是典型的 TOCTOU。代码注释在 `checks (internal/quota/quota.go)` 上把这一点说得很直白：跳过某个当前总量很低的维度就意味着先读它，而「读后写」恰恰是单条 INCRBY 要避开的竞态。所以每个受限维度都无条件预留，哪怕本请求离限额还很远。

### 估算为什么故意偏保守

估算值不在 `internal/quota` 内构造，而是在 `internal/gateway/quotapath.go` 的 `quotaEstimate` 里，由它算好后交给 `quota.Estimate`：

```go
func (p *Proxy) quotaEstimate(parsed parsedRequest, body []byte) quota.Estimate
```

- 提示 token = `len(body) / charsPerToken`（下限 1），`charsPerToken` 来自配置键 `estimate_chars_per_token`（默认 4）。源码注释观点：4 chars/token 是英语经验值，对 CJK（约 1–2）与长标识符的代码会**低估**，这类租户应当调低它，代价是会拒掉一些本来放得下的请求（这段观点写在 `configs/quota-local.yaml` 的注释里）。
- 补全 token = 调用方自己给的 `max_tokens`（若有），否则 `estimate_completion_tokens`（默认 256）。源码注释观点：调用方自报的上限比任何常数都更接近真实上界；预留得比请求可能花掉的少，会让一波流量穿过一个名义上已耗尽的预算。
- 成本 = 用配置的价目表乘出来的微美元（`pricing.default` / `pricing.models`），**不是** provider 报的账。源码注释观点（configs）：网关看不到发票，但它能把 token 乘上运营者配置的单价；没有条目的模型回落到 `default`，所以把 `default` 留成 0 会静默关掉成本预算。

`推断`：偏保守是这套机制的必要条件而不是优化。结算只做「真实值 − 预留值」的一次原子加法，所以预留过大只是把额度**临时**占住，结算时退回；预留过小则会让请求在限额之上仍被放行，只能靠下一次请求被拒来追认，这期间的超支就是 `overshoot`。

### Settle：用真实 usage 修正，差额双向

`Settle (internal/quota/quota.go)` 的形态：

```go
func (m *Manager) Settle(ctx context.Context, res *Reservation, usage Usage) error
```

对预留里的每个条目算 `diff := actual - e.reserved`，再 `Store.Add(e.key, diff, e.ttl)`：

- `diff > 0`：请求超出估算。计数器的方向是「偏高」，所以**下一个**请求才会被拒。源码注释观点：这正是「宁可错在这个方向」。
- `diff < 0`：退回多留的部分，同时把 `-diff` 记进 `ReleasedTokens`（token 维度）或 `ReleasedCostMicro`（成本维度）。
- `actualFor (internal/quota/quota.go)` 把计数器映射到结算值：token 维度取 `Usage.Tokens()`（提示+补全），成本维度取 `CostMicros`，请求维度的注释是「请求已经发生了；结算从不改变计数」，返回 `max64(u.Requests, 1)`。
- `Settle`/`Release` 对 `res == nil || res.settled` 直接返回 nil。源码注释观点：代理从 deferred 块里结算，那条路径可能已经释放过，重复扣款比漏掉一次更糟。
- 单个条目写失败只累计 `StoreErrors` 并记日志（`quota: settle failed`），继续处理其余条目，最后返回第一个错误。`推断`：这是「额度是流量控制、不是账务系统」的取舍——一条写失败不该让整个请求结算失败，但错误必须浮出来。
- 结算最后调用 `checkAnomaly`。

### Release：从没打到 provider 的请求怎么退

`Release` / `releaseEntries (internal/quota/quota.go)` 把每个预留条目按 `-e.reserved` 加回去，同样「报告第一个错误但一定把所有条目都试一遍」。源码注释观点：留下一个还在计费的计数器就是永久泄漏。审计计数只在 store 真的退成功之后才加（`e.reserved <= 0` 时跳过），源码注释观点：一个仍然被计费的计数器绝不能被报成已释放。

### 验收恒等式

`Stats (internal/quota/quota.go)` 里写明了三个 token 计数是一本账的三个视角：

```
reserved - released + overshoot == settled
```

各项的确切含义（源码注释观点）：

- `ReservedTokens`：准入时预扣的量；`SettledTokens`：provider 真实报出的量；`ReleasedTokens`：预留中退回来的部分。
- `OvershootTokens`：结算高于预留的那部分，源码注释观点称它是「限额外溢程度最诚实的度量」，也是提示运营者该调高补全估算的信号。
- 三者**按预留条目计数，不按请求计数**：一个租户若同时受日 token 与会话 token 两个维度约束，它的 token 会被计两次（每个被消耗的预算各一次）。源码注释观点：恒等式正是让这件事可读的东西——这些数字描述的是「被消耗的预算」，不是「被放行的请求」。代码里还有一段历史注释：早先按请求计数一次而按条目逐条结算，导致恒等式恰好对那些最难推理的租户不成立。
- `overshoot` 为什么可以非零：因为限额是对**估算**执行的，而估算可能低估（长补全、CJK、缓存未命中且调用方没给 `max_tokens`）。这是机制的属性而不是 bug，所以它被单独上报（`/admin/quota` 里同时有 `overshoot_tokens` 与 `overshoot_cost_micros`）。
- 金额侧没有对应的 `settled` 计数（`Stats` 只有 `OvershootCostMicro` 与 `ReleasedCostMicro`），所以这条恒等式只在 token 维度上完整成立；`Settle` 里对成本条目只记录退款与超支。
- `ReleasedCostMicro` 与 `ReleasedTokens` 分开的原因见 `Settle` 注释：一次实测运行把两者加进了同一个「released tokens」，得出「为一次预留 279 的请求释放了 1014 token」这种读起来像预算异常的单位错误。

### 全有或全无

`Admit` 的循环把每个成功预留的条目记进 `res.entries`。任一条 `Store.Add` 报错，就把已预留的条目全部 `releaseEntries` 回滚，然后把 `res.settled` 置真，返回 `unavailable`。若某个维度越限：

- 非降级（`on_exceed: reject`）：`break` 跳出，随后 `releaseEntries` 回滚，`res.settled = true`，返回 `ActionReject`，`m.stats.rejected` 加一。
- 降级（`on_exceed: degrade` 且配了 `downgrade_model` 或 `max_tokens_cap`）：**继续**把余下维度都预留（一个降级请求仍是请求，仍要结算），返回 `ActionDegrade` 与降级后的模型/上限，预留守住不动，等结算退回廉价形态没用掉的部分。
- 上报的 `Used` 是 `breachTotal - breach.delta`，即「本请求之前」的存量。源码注释观点：`breachTotal` 含了本次增量，报它会自相矛盾。
- 循环只记住**第一个**越限维度（`breach == nil` 时才记录）。

`RetryAfter`：日维度给 `untilDayEnd(now)`，分钟维度给 `time.Minute - now.Second()*time.Second`。`推断`：分钟维度的这个算法忽略了纳秒，最多早退一小截，属于无害的近似。

## 30. 失败关闭（fail-closed）与 `fail_open`

`unavailable (internal/quota/quota.go)` 是所有 store 故障的唯一出口：

```go
func (m *Manager) unavailable(err error) (*Decision, error)
```

- 一律 `StoreErrors` 加一、记 `quota: store unavailable` 日志（带 `fail_open` 字段），并且**返回的 error 永远非 nil**——源码注释观点：调用方无论如何都得记它；不同的只是请求是否被放行。
- `FailOpen == false`（默认，`configs/quota-local.yaml` 里写着「Fail-closed. This is the important default」）：返回 `Allowed: false, Action: ActionReject, Reason: ReasonStoreError`，`rejected` 加一。
- `FailOpen == true`：返回 `Allowed: true, Action: ActionAllow, Reason: ReasonStoreError`，同时 **`allowed` 也加一**。源码注释观点：fail-open 的放行仍然是放行，不计数会让 `allowed+degraded+rejected` 恰好在该被盯着的时刻少算请求。
- 没有 store 而 Enabled 为真（配置错误）走同一条路：`errors.New("quota: no store configured")`，返回的 Reason 仍是 `ReasonStoreError`。

代价（`推断`，并写在配置注释里）：fail-closed 把「额度存储不可用」升级成「网关整体拒服务」——Redis 抖动期间流量全灭；fail-open 则把成本控制权交给运气，正好在没人看的时候不限量地花钱。响应头 `X-InferGate-Quota` 与 `/admin/quota` 总会说出这次是哪一种模式答的，所以这个取舍至少是可观测的。

## 31. 维度、窗口与「日」边界

### 四个维度，各自独立、互不折算

`checks (internal/quota/quota.go)` 按策略里 >0 的维度生成预留项，顺序固定：

| 维度常量 | 窗口 | 计数器 | TTL | 增量 |
| --- | --- | --- | --- | --- |
| `ReasonDailyTokens` | `WindowDay` | `CounterTokens` | 到日终 | `est.Tokens()` |
| `ReasonDailyCost` | `WindowDay` | `CounterCost` | 到日终 | `est.CostMicros` |
| `ReasonMinuteRPM` | `WindowMinute` | `CounterRequests` | `minuteTTL`（2 分钟） | `max64(est.Requests, 1)` |
| `ReasonSessionTokens` | `WindowSession` | `CounterTokens` | `SessionTTL` 或默认 | `est.Tokens()` |

源码注释观点：**四个维度各自独立**，不共享一个额度池——分钟 RPM 与日 token 是两种完全不同的故障形态（一分钟烧完一天的爆发是 bug，是重试循环或丢掉停止条件的 agent），早拒它才能让剩下的时间活下去（这段话写在 `configs/quota-local.yaml` 的 `bursty` 租户注释里）。会话维度只在调用方带了 `X-InferGate-Session` 且策略配了 `tokens_per_session` 时才参与；没有它，一条跑飞的 agent 线程能在被发现前花掉租户的一整天。

- `Policy (internal/quota/quota.go)` 的零值语义：**0 表示「这个维度不设限」，不是「限额为 0」**。源码注释观点：没有办法表达一个「一分钱都不许花」的租户，因为那种租户用「不出现在文件里」表达更合适。
- `Budgeted (internal/quota/quota.go)` 是「这个租户到底有没有被限」的唯一判据，注释特意说明**故意没有**第二个 `unbounded` 标志：同一个问题有两个答案，就有一个会错。无预算的策略在 `Admit` 里直接短路，只花一次 map 查找、零次往返。
- `Policy.SessionTTL` 在生产装配里从未被赋值（`quotaPolicy (internal/server/quota.go)` 不设它），所以会话 TTL 实际永远是 `DefaultSessionTTL`（24 小时）。源码注释观点：会话计数器的存活时间同时也是「被遗弃会话占着预算」的上界，所以取的是一天而不是一小时——长 agent 运行不该在跑到一半时预算被重置。

### 「日」是 UTC 日

`dayBucket` 用 `t.Format("20060102")`，`minuteBucket` 用 `"200601021504"`，而 `Admit` / `Report` 都先做 `m.now().UTC()`。`untilDayEnd` 的注释把理由写全了：

- 桶键取自 UTC 日历日；**先转 UTC 再取日期**，因为从本地时间读日历日会给任何传入带时区时间的调用方造出错误的那一天，那是预算窗口里静默的一天偏移。
- day TTL = 到下一个 UTC 午夜再加**一小时松量**，下限一分钟。源码注释观点：一小时的松量让时钟落后的副本在整个当日结束前仍能读到自己写过的桶。
- `minuteTTL` 是 2 分钟：键必须活得比桶长，10:00:59 建的键到 10:01:00 仍然相关。

用户可见后果（`推断`）：对 UTC+8 的运营者，配额在本地时间早上 8 点重置；`/admin/quota?tenant=` 报的 `day` / `minute` 也是 UTC 字符串。日预算跨零点不是凭 TTL 过期清零的，而是**换了一个键**——`RedisStore` 的注释：窗口是键的一部分，不是固定键上的 TTL；固定在某个键上的 TTL 会被迟到请求在日中重建，那是静默重置预算。

### 键与转义

`key (internal/quota/quota.go)` 拼 `prefix:sanitise(tenant):window:sanitise(bucket):counter`，前缀默认 `ig:quota`（`New` 里补默认值）。`sanitise` 保留 `[a-zA-Z0-9-.]`，`_` 双写成 `__`，其余写成 `_x` + 恰好六位十六进制。

源码注释观点（分两处）：租户名来自客户端可控的头，`"acme:day:20261005:tokens"` 这样的租户名若不转义，就能让一个调用方去花别人的预算；编码必须是**单射**的，「acme:inc」与「acme_inc」折叠成同一个键就是跨租户计费。固定宽度是单射的关键：没有它，U+10FFF 后跟 `ff` 与 U+10FFFF 后跟 `f` 会编成同一个 `_x10fffff`。「配了一个古怪名字的租户因此就是它自己的租户，而不是别人的第二个名字。」

## 32. 存储层：`Store` 接口、内存实现、Redis 实现

```go
type Store interface {
	Add(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error)
	Get(ctx context.Context, key string) (int64, error)
	Close() error
}
```

`Add (internal/quota/store.go)` 的契约（源码注释观点）：key 从 0 起算；**ttl 只在计数器被创建时生效，且绝不能延长已有计数器的寿命**。滑动过期会把日桶重置在租户最后一次请求的那个钟点——正是窗口该关上的时候。

### `MemoryStore`：单副本才正确

- 构造：`NewMemoryStore()` 与 `NewMemoryStoreWithClock(now func() time.Time)`（测试用假时钟）。
- 状态：`values map[string]memoryValue`，`memoryValue` 有值与 `expire`（零值表示永不过期）；一把互斥锁保护整个 map。
- `Add`：取出条目，若已过 `expire` 就 `delete` 再当新键处理；写入时只有 `ttl > 0` 且是新键才设 `expire`（**不改已存在条目的过期时间**）；累计值 `<= 0` 时删除键。`Get` 同理做惰性过期删除。`Len()` 只统计未过期条目。`Close()` 是 no-op。
- 差异对照（写在 `internal/quota/redis_test.go` 的注释里，同样被测试钉住）：Redis 会让计数器停在 `-1` 或 `0` 而不删键；内存实现会删。两者通过 `Get` 读回都是 0，所以从 manager 看不出来（manager 的增量从不把计数器压到 0 以下）。
- **为什么只在单副本下正确**：计数是进程内存。N 个副本各自放行完整日预算，真实上限就是配置值的 N 倍。这条观点同时出现在 `configs/quota-local.yaml` 的 `store: "memory"` 注释、`RedisStore` 的注释和 `buildQuota (internal/server/quota.go)` 里——所以 `buildQuota` 把 `"memory"` 原样报给 `/admin/quota`：在进程内存里执行的预算是**每副本**预算，读这个页面的人需要知道这件事。

### `RedisStore`：共享预算与它唯一的原子性

```go
func NewRedisStore(opts RedisOptions) (*RedisStore, error)
```

`RedisStore` 只含一个 `*redis.Client`。键布局写在类型注释里：

```
ig:quota:<tenant>:day:<YYYYMMDD>:tokens        INCRBY counter, TTL to day end
ig:quota:<tenant>:day:<YYYYMMDD>:cost_micros   INCRBY counter, micro-USD
ig:quota:<tenant>:minute:<YYYYMMDDHHmm>:requests
ig:quota:<tenant>:session:<id>:tokens
```

- `Add` 就是一条 `INCRBY`（`strconv.FormatInt` 成十进制字符串），**只有** `delta > 0 && total == delta && ttl > 0` 时才补一条 `EXPIRE`。源码注释观点：`total == delta` 是「首次增量」的签名；Redis 在 INCRBY 上保留已有 TTL，所以无条件的 EXPIRE 会把窗口往后推。这也是「负增量不武装 TTL」那条测试的行为来源。
- `Get` 是一条 `GET`；键不存在（`reply.Kind` 为 nil）返回 0 和 nil error；非整数回复报 `quota: counter %s is not an integer`。
- 错误字符串原文：`quota: redis addr is empty`、`quota: cannot reach redis at %s: %w`、`quota: INCRBY: %w`、`quota: EXPIRE: %w`、`quota: GET: %w`、`quota: counter %s is not an integer: %w`。
- `NewRedisStore` 在**构造期**就 ping 一次，空地址、纯空白地址、连不上的地址都在这里失败（测试 `internal/quota/redis_test.go` 专门钉住这三点）。源码注释观点：一个「静默什么都不做」的额度存储会在看起来配置正确的情况下不执行任何预算。
- 计数器是**字符串**而不是 hash 字段或列表元素，源码注释观点：字符串上的 INCRBY 是原子的，这就是全部并发故事。窗口在键里而不在 TTL 里：23:59:59 与 00:00:01 的两个请求不会去抢同一次翻页，读「今天」是纯 GET 不做算术，TTL 只是垃圾回收。
- **为什么不用 MULTI/EXEC**：见上文第 2 条主张；同样的理由也写在 `configs/quota-redis.yaml` 的顶部注释里（「a transaction would span several connections and would not be a transaction at all」），以及 `internal/redis` 与 `internal/mockredis` 的「没有 MULTI/EXEC」说明里。测试替身 `mockredis` 干脆不支持 MULTI/EXEC，所以任何试图引入事务的实现都会在测试里直接失败。

## 33. `Manager`：共享状态、并发与只读报表

```go
type Manager struct {
	cfg   Config
	store Store
	now   func() time.Time
	log   Logger
	stats atomics
	mu        sync.Mutex
	anomalies map[string]*anomalyState
}
```

- 热路径只碰 `atomics`：`allowed` / `degraded` / `rejected` / `storeErrors` / `alerts` / `overshoot` / `overshootCost` / `reservedTok` / `settledTok` / `releasedTok` / `releasedCost`，`atomic.Uint64` 与 `atomic.Int64`。源码注释观点：这些是每个请求都会碰的计数，放进原子量是为了让热路径**从不**取管理器的互斥锁。
- `mu` 只保护 `anomalies`（每个租户最近一次异常评估的状态），所以是「低频路径的锁」。
- `Reservation` 是**单请求私有**的：`entries`、`settled` 无锁。源码注释观点（接口约定层面）：`Admit` 返回的 `*Reservation` 归调用方独占，不会被并发共享；`settled` 只是防重复结算的幂等位（`推断`：它不是并发保护，若同一个 `Reservation` 被两个 goroutine 同时 Settle，仍会双写）。
- `New(cfg Config, store Store) *Manager`：`cfg.Now` 为空则用 `time.Now`，`cfg.Prefix` 为空则用 `ig:quota`。源码注释观点：`Enabled` 为真而 store 为 nil 是配置错误，但由 `Admit` 报成 store error 而不是 panic——「一个请求就 panic 的网关一个请求都服务不了」。
- `Enabled` / `Policy`：后者命中 `cfg.Tenants[tenant]`，否则回落 `cfg.DefaultPolicy`。
- `Stats()` 只是把 11 个原子量读成一个值对象。
- `Report(ctx, tenant, session string) (Report, error)`：为 `/admin/quota` 服务，源码注释观点是它「花得起请求路径刻意避开的那些往返」。依次读日 token、日成本、分钟请求数、以及（`session != ""` 时）会话 token，再算 baseline 与 `Ratio = TokensToday / baseline`、`Alerting = AnomalyRatio > 0 && Ratio >= AnomalyRatio`。`store == nil` 时返回零 `Report` 与 nil。
- `Close()` 转发给 `store.Close()`（nil store 安全）。
- 异常检测：`checkAnomaly` 在 `Settle` 末尾调用，`baseline` 读前几日的计数（`anomalyBaselineDays = 3`，评估间隔 `anomalyCheckInterval = time.Minute`），只在**状态翻转**时打日志（`quota: spend anomaly` / `quota: spend back to normal`），并让 `alerts` 加一。源码注释观点（configs）：告警永不拦流量——一次开销尖峰通常就是真实负载，因为「看起来不寻常」而拒服务会把一次发布变成一次故障。

## 34. 配置键与 server 侧装配

`configs/quota-local.yaml` / `configs/quota-redis.yaml` 里与配额相关的键：`quota.enabled`（代码里默认关，这两个文件显式打开，注释说明治理是运营者的显式决定，绝不该被一次升级顺带打开）、`quota.store`（`memory` / `redis`）、`quota.fail_open`、`quota.estimate_chars_per_token`、`quota.estimate_completion_tokens`、`quota.anomaly_ratio`、`quota.default_policy`（`tokens_per_day` / `requests_per_minute` / `on_exceed`）、`quota.tenants[]`（`tenant` / `tokens_per_day` / `cost_per_day_usd` / `requests_per_minute` / `tokens_per_session` / `on_exceed` / `downgrade_model` / `max_tokens_cap`）、`quota.redis.*`（`addr` / `db` / `prefix` / `pool_size` / 三个 timeout / `password`）、以及 `pricing.default` 与 `pricing.models.<name>.{in,out}`。

注意 `estimate_chars_per_token` / `estimate_completion_tokens` / `pricing` **不进入** `quota.Config`：它们由网关侧消费（见 `quotaEstimate (internal/gateway/quotapath.go)`），包 quota 只收到已经算好的 `quota.Estimate`。`quota-redis.yaml` 与 local 版的差别只有监听端口、上游端口、`store`、以及刻意调小的预算（`acme` 300 token/日，好让验证脚本用几个请求就打穿它，从而观察真实的 429 与计数器报错，而不是一个看起来合理的额度）。

装配在 `buildQuota (internal/server/quota.go)`：

- 租户表翻译成 `map[string]quota.Policy`；`quotaPolicy` 负责美元→微美元的一次换算（`math.Round(usd * 1_000_000)`），注释说配置用美元是因为那是运营者做预算的单位，而管理器用微美元是因为那是能在整数算术里活下来的单位。
- `cluster` 模式下 `quota.NewRedisStore` 失败会让 `buildQuota` 返回错误而不是回落到内存：源码注释观点与共享缓存同理——一个配置成集群预算却静默按进程执行部署，会让 N 个副本各花掉整份预算，而超支要到发票来了才可见。
- `store` 不是 `"redis"` 时走默认分支：用 `quota.NewMemoryStore()`，并把 `"memory"` 作为第二个返回值报给管理面。
- 注释说明为什么 manager 在 server 侧而不是在 gateway 里构造：它持有跨请求的计数器、要跟 Redis 说话；网关只拿到 manager 并调用 `Admit` / `Settle` / `Release`，这样请求路径里没有配置翻译，策略判断也能不起服务器就测。
- `handleQuota` 把 `stats` 的每一项摊平成 JSON；注释强调 `?tenant=` 报表是主视图而不是附加项——「租户说被拒了」和「租户说从没接近上限」在没有计数器可读时是同一种说法。`Report` 出错返回 502 与 `infergate_quota_error`，理由与准入失败关闭相同：报表读的就是计数器，读不到就别假装没花钱。
- `CloseQuota` 在监听器停止之后才关 store：一个还在被写的计数器不该被从底下抽掉连接池。

## 35. 测试意图（quota 三个测试文件）

不逐个罗列，只挑说明设计意图的：

- `internal/quota/quota_test.go`：白盒（同包），用假 store 与假时钟。它钉住的核心不变量是**恒等式与三态**——`Admit` 预留了几个键、`Settle` 用真实 usage 修正、`Release` 退净；以及键的转义（租户名 `acme:inc/eu`、会话 `sess/1` 不会撞进别的命名空间）、会话键的 TTL 等于 `quota.DefaultSessionTTL`、降级路径继续为余下维度预留。
- `internal/quota/store_test.go`：把 `Store` 的 TTL 契约钉死为四点——键随自己的 TTL 过期（不早不晚）、**第二次 Add 不延长 TTL**（用一个更长的 TTL 也不行，注释：「窗口在窗口结束时就结束，不管是谁在写它」）、落到 0 或以下就删键（对一个不存在的键做负增量也不能创建它）、`ttl == 0` 永不过期。
- `internal/quota/redis_test.go`：用进程内 `mockredis` 起 RESP2 服务器（监听 `127.0.0.1:0` 而非固定端口，两次运行不会撞），直接 `TTL` 上网读到期时间，验证「TTL 只设一次且不被延长」，以及负增量不武装 TTL。它还钉住构造期校验：空地址、空白地址、不可达地址都必须**在构造时**失败。

## 36. `internal/idempotency`：重放存储

包注释先划清它与缓存的界限：缓存回答「我见过这个问题吗」并且允许未命中；重放存储回答「这个**确切**的请求是否已经执行过」，并且必须答得出「是」，因为调用方是一个 agent——超时后重试不能二次计费、不能二次打到上游、不能产生第二个副作用。四条差异（源码注释观点）：键是调用方的而不是负载的哈希（只差一个时间戳的两个请求**故意**撞在一起）；键在开工**之前**就被占用，所以并发重复是冲突而不是第二次上游调用；同一个键带**不同 body** 是错误而不是命中（服务它等于回答了调用方没问的问题）；条目按租户分域，因为两个都选了键 `"1"` 的租户不是在说同一个请求。

### 状态与迁移

`Decision (internal/idempotency/store.go)` 是四态，`String()` 分别是 `proceed` / `replay` / `conflict_body` / `in_flight`，未知值是 `unknown`：

| 态 | 触发条件 | 副作用 |
| --- | --- | --- |
| `Proceed` | 该 scope 无此键，或键是**已完成且已过期**的条目（会先删掉过期条目并计入 `Expired`） | 写入一个 in-flight 占位（`claim`） |
| `Replay` | 存在已完成条目且 `entry.RequestHash == requestHash` | LRU 提到最前，`Hits` 加一，返回条目的**深拷贝** |
| `ConflictBody` | 已完成条目但哈希不同 | `Conflicts` 加一，返回已存条目（好让错误能说出它持有的是什么） |
| `ConflictInFlight` | 条目存在且 `inflight` | `InFlightRejects` 加一，返回空 `Entry` |

生命周期由三个方法走完：`Begin` 认领 → `Complete` 落库（把 `inflight` 换成真条目，并重设 `ExpiresAt = now + ttl`，即 TTL 从**完成时刻**起算）或 `Abort` 释放认领。互补的不变量：

- `Abort` 对已完成的条目是 no-op（测试注释：「已存的答案不能因为一个迟到的调用方决定不存它而丢失」）。
- `Complete` 对已被淘汰/清空的认领仍会存储：注释说键在调用方手里，答案值得记住。此时它新建一个空 `item`。
- `Complete` 会把认领上的 `RequestHash` 搬到条目上。源码注释观点：哈希属于认领而不属于响应；`Begin` 比较过它，`Complete` 必须继续沿用，否则每次重放都看起来像换了个 body。
- 认领的 TTL 只约束「崩溃的请求能挡住自己的键多久」，`Complete` 会换成完成时刻起算的 TTL。
- janitor（`sweep`）**不会**清掉 in-flight 的认领，即使它早已过期（测试专门钉住：一个还在跑的请求必须保住自己的认领）。

### 为什么存的是响应**字节**

`Entry.Body []byte` 与 `Entry.Headers map[string][]string`，`Replay` 的文档写的是「served verbatim, without touching the upstream」（逐字节原样返回，不碰上游）。`推断`：所以存储的是字节而不是解析后的响应对象——一旦中间隔了一层结构体再序列化，header 顺序、空格、数字格式、流式分块边界都可能变，而调用方要的是「和第一次收到的完全一样」。`Get`、`Replay` 都返回 `clone` 出来的拷贝（body 与 headers 都是新分配的），测试里有一条专门改返回值的用例：调用方改写自己收到的 body/header 不得污染下一次重放。

### `Complete` 的返回值：`stored=false` 只有一条路

```go
func (s *Store) Complete(scopeName, key string, e Entry) (bool, string)
```

今天只有一种「不可重放」的原因：`int64(len(e.Body)) > s.maxResponseBytes` 时删掉条目、`Oversize` 加一、返回 `(false, "oversize")`。源码注释观点：false 返回**仍然释放认领**——调用方拿到的是正确答案，只是没法重放；把键永远留在 in-flight 会把「太大记不住」变成「永久损坏」。`Abort` 是另一个方法、另一条路，它释放的是「没有产生可重放响应」的认领（传输失败，调用方必须能自由重试）。所以「超限」与「已释放」在当前代码里**不是**同一个返回值合并出来的；见「未能确认」。

### 容量、淘汰与它的后果

- `Options`：`Capacity`（默认 `idempotency.DefaultCapacity` = 2048）、`TTL`（`idempotency.DefaultTTL` = 15 分钟）、`MaxResponseBytes`（`idempotency.DefaultMaxResponseBytes` = 1 MiB）、`Now`、`SweepInterval`（零值一分钟，负值关掉 janitor）。`Store` 的注释说它照搬 `internal/cache` 的内存 store 形态（scope、真 LRU、只为体积而非正确性存在的 janitor）。
- **淘汰是 LRU 不是 FIFO**：每个 scope 一个 `container/list`，`Begin` 命中时 `MoveToFront`，`Complete` 时也 `MoveToFront`，`trim` 从尾部淘汰并计入 `Evicted`。
- **淘汰的后果必须明说**：条目被淘汰（或 TTL 过期、或被 `Flush`）之后，同一个 `Idempotency-Key` 再次到来会回到 `Proceed`，也就是**再次打到上游**。重放保证的有效期就是「条目还在」的这段时间；`Capacity` 与 `TTL` 因而是一个「可能重复执行的窗口」参数，而不只是内存参数。旧条目的 `RequestHash` 也随之消失，所以同一个键换成另一个 body 也**不再**报冲突。
- `Len()` 只数可重放的条目，`InFlight()` 是独立的 gauge；测试注释强调「认领不是可重放的答案」，所以一个只有 in-flight 认领的 store `Len()` 是 0 而 `InFlight()` 是 1。`List` 也是「最新在前」且不含 in-flight。
- `KeyOf(raw string)` 只做 `TrimSpace`，不折叠大小写：注释说区分 `K1` 与 `k1` 的两个调用方是认真的。
- 统计字段：`Lookups` / `Hits` / `Misses` / `Conflicts` / `InFlightRejects` / `Stored` / `Oversize` / `Evicted` / `Expired` / `Aborted`，都带 JSON tag，直接喂管理面。

### 请求指纹包含什么

指纹在网关侧算：`requestHash (internal/gateway/idempotency.go)` 取 `sha256(method || 0x00 || path || 0x00 || body)` 的前 16 字节十六进制。源码注释观点：方法与路径是哈希的一部分——同一个 body 发到 `/v1/embeddings` 和 `/v1/chat/completions` 不是同一个操作。**不包含**的东西（`推断`）：头、租户、时间戳都不进指纹——租户靠 scope 隔离，头与时间戳不该让一次重试变成「冲突」。指纹只与**已存的条目**比较，从不作为查找键；查找键就是调用方自己的 header 值。

网关侧调用顺序（只讲衔接，细节归 02C）：先 `KeyOf` 规整键 → `Begin`，`Proceed` 才继续走缓存/上游并把键记在 `idemClaimed`，`Replay` 直接回放，两种 `Conflict*` 变成 4xx/409 类错误；请求失败走 `Abort`，成功且可重放走 `Complete`。

## 37. `internal/sessions`：会话账本

包注释（`internal/sessions/ledger.go`）的观点是：记账的单位是调用方的 `X-InferGate-Session`，不是进程也不是租户；同一个会话 id 在两个租户下是两段对话（`keyOf` 用 `tenant + "\x00" + id` 组合键）。

- `Options`：`Capacity`（`sessions.DefaultCapacity` = 4096）、`TTL`（`sessions.DefaultTTL` = 24 小时，从最后一次请求起算）、`RecentPerSession`（`sessions.DefaultRecentPerSession` = 8）、`Now`、`SweepInterval`（零值一分钟，负值关闭）。
- 结构是 `map[string]*list.Element` + `container/list`（front = 最近活跃），一把互斥锁保护全部状态；`remove` 同时清 map 与 list。与 idempotency 不同的是：**活跃会刷新 TTL**（测试注释：还在进行的对话不能在对话中途消失），这与幂等条目的「完成时刻起算」正好相反，因为两者要的东西不同——账本要的是「活会话不丢」，重放要的是「固定窗口内可重放」。
- `Record (internal/sessions/ledger.go)` 是唯一的写入口，按维度聚合并顺手做 LRU 与上限维护：`Requests` / `Ok` / `Failed`（`Status` 决定）、`Replays`（`Request.Replay`）、`CacheHits`（只有 `Cache` 前缀为 `hit` 才算，测试注释：`miss` 不算）、`PromptTokens` / `CompletionTokens` / `CachedTokens` / `CostUSD`，按模型累进 `Models map[string]ModelTotals`，按上游累进 `Upstreams map[string]int64`，维护 `FirstSeen` / `LastSeen` / `ExpiresAt`。
- 会话 id 为空或纯空白时**不创建会话**，只把 `NoSessionID` 加一——测试注释：「没有会话 id 的请求不能变成一个会话」。`Recorded` 也不加。
- 请求窗口是有界的：`Recent` 只保留最近 `RecentPerSession` 条，被挤掉的计入 `RecentDropped`。测试注释强调窗口**不会缩小汇总**（4 个请求、窗口 2 条，`Requests` 仍是 4）。
- `Get` / `Find`（跨租户按 id 找，最新在前）/ `List(limit)`（最新在前，`limit <= 0` 表示不限）/ `Tenants()`（按成本降序的租户维度汇总，`TenantTotal`）/ `Flush()` / `Len()` / `Stats()`。
- **所有返回都是深拷贝**：`clone` 复制 `Models` / `Upstreams` 映射、`Recent` 切片以及每条 `Request.Tried`。测试专门改写返回值的 map 与 slice，验证内部状态不被污染。
- 统计：`Recorded` / `NewSessions` / `NoSessionID` / `RecentDropped` / `Evicted` / `Expired` / `Flushes`。
- 并发不变量：测试用 8×25 个并发 `Record` 钉住 `Requests`、两个 token 总和与 `Recorded` 都不丢更新——注释说一次丢更新就会藏起真实开销。

## 38. `internal/stats`：router 与 breaker 共享的滑动窗口

- 包注释给出三条设计决定：窗口是**时间桶环**而不是原始样本列表（繁忙网关每小时百万级样本，留着算分位数会把延迟特性变成内存泄漏；桶给 O(1) 记录、O(buckets) 汇报）；桶内用**指数移动平均**而不是 sum/count（跨多个请求的桶不能让一次 30 秒超时主导均值，而且桶滚出窗口时值会平滑衰减）；每个样本都夹到 `maxSample` = 2 分钟（挂十分钟的请求——被杀掉的客户端、休眠的笔记本——否则会污染整个窗口，把健康后端看成灾难）。`EWMAAlpha` = 0.2，注释说明取值理由：约五个请求后均值才有意义，阶跃变化几个请求内反映出来，又不会在桶内完全忘记过去。
- `New(span, buckets)` 把 `buckets` 夹到 1..1024、`span` 下限 1 秒。注释：1ns 的窗口会造出每个请求都重置的环，静默关掉它喂给别人的每项统计。
- `Stats` 有 `mu` 保护 buckets；`RecordSuccess` / `RecordFailure(timeout)` / `RecordFirstToken` 是唯一的写入口，`Snapshot()` 返回一致读（`Attempts` / `Successes` / `Failures` / `Timeouts` / `FailureRatio` / `LatencyMean` / `TTFBMean` / `HasLatency` / `LastFailure`），`Reset()` / `SetClock()`（仅测试）另计。延迟只在成功时采样，源码注释观点：把失败混进去会让一个立刻失败的后端变成「最快」的后端。`FailureRatio` 无流量时是 0 而不是 1（未知不能看起来像坏掉）；
- `HasLatency` 存在的理由：区分「测到 0」与「从没测过」——把没测过当成 0 的路由会把所有流量发给一个从没应答过的后端。
- **共享关系**：每个上游一个 `*stats.Stats`，由 `NewGroup (internal/breaker/group.go)` 创建（`stats.New(cfg.Window.Duration(), cfg.Buckets)`）后交给 `breaker.New(name, cfg, window)`；router 通过 `snapshot (internal/router/router.go)` 里的 `r.breakers.Get(name).Stats().Snapshot()` 读同一个实例，用它排序/挑选上游。
- **谁写谁读的约定**：写方只有代理的请求路径（真实延迟与成败）；读方是 router（`Snapshot` 选目标）与 breaker（判定是否跳闸）。`RecordSuccess (internal/breaker/breaker.go)` 的注释把边界写死了：它**故意不写窗口**——代理已经把真实延迟写进共享的 `stats.Stats` 而 breaker 只读；如果 breaker 也记一次成功，它就得凭空造一个自己没有的延迟值，而造出来的 0 会把 router 对那个后端的延迟估计往下拽。同一段注释还说明 breaker 从 half-open 回到 closed 时刻意**不清空窗口**：清空会抹掉「上游刚恢复」的证据，也会抹掉导致跳闸的那些失败，而两者都是管理面应当展示的；窗口会自己过期。

## 39. 未能确认

- **本册这一节的目标文件原本有另一份内容。**开工时该位置写的是 `internal/cache/redis.go` + `internal/embed/embed.go` + `internal/redis/redis.go` 的走读。按分册约束由本册的另一节覆盖了它：`internal/cache/redis.go`、`internal/redis` 与 `internal/mockredis` 的逐符号内容在本册后半，`internal/embed` 与 `internal/evalset` 的逐符号内容见 `08-supplement-data-plane.md`。
- 任务描述里说 `Complete` 把「超限」与「已释放」合并成同一个 `stored=false` 返回值。**代码里不是这样**：`Complete (internal/idempotency/store.go)` 只有 `oversize` 一条 false 路径，且它仍然释放认领；「没有产生可重放响应」由独立的 `Abort` 处理。可能描述的是网关侧的合并语义，我没有读完整条网关分支来确认。
- 两个已定义但全仓库**没有任何引用**的常量：`ReasonAnomaly` 与 `ReasonNoStore`（都只在 `internal/quota/quota.go` 的常量块里出现）。异常检测走的是日志与 `Alerts` 计数，从不产生 `Action`/`Reason`，所以「异常永不拦流量」在行为上成立，但这两个字符串目前是死代码。
- `Policy.SessionTTL` 只在 `checks` 里被读，`quotaPolicy (internal/server/quota.go)` 从不赋值，配置里也没有对应键；我推断会话 TTL 在生产里恒为 24 小时，但没有找到任何注释确认这是有意留的扩展点还是尚未接线的字段。
- `Reservation.settled` 看起来只做幂等标记而非并发保护；我没有找到关于同一个 `*Reservation` 是否可能被两个 goroutine 同时结算的注释，也没有测试覆盖这一情形。
- 未跑 `go test`（本机沙箱拒绝下载 go 1.26 工具链），以上关于测试意图的结论全部来自读测试源码，不是来自实际运行结果。
- 网关侧（`internal/gateway/quotapath.go`、`idempotency.go`、`sessionpath.go`）的完整分支与错误码映射不在本册范围，本册只引用了它们的签名与关键注释。


## 40. 本册范围与衔接

本册是 04 册第三册，走读四处：

- `internal/cache/redis.go` — `RedisStore` 及其全部方法与选项（内存侧已在本册前段覆盖，本册只在必要时引用签名）。
- `internal/cache/cache_test.go` 里 Redis 专属的用例，尤其 `TestRedisStoreIsFIFOAndDocumentedSo` 与钉住并发计数的 `TestStoreStatsSurviveConcurrentReaders`。
- `internal/redis/redis.go` — 手写 RESP2 客户端。
- `internal/mockredis/mockredis.go` 与 `cmd/miniredis/main.go` — 测试替身与它的独立进程封装。

**关于同册覆盖的说明**：本册对 `internal/cache/redis.go` 与 `internal/redis/redis.go` 的这两节，是**按实现顺序重走一遍并补足机制与后果**：键布局、`EXPIRE` 的 `max(2*ttl, 60)`、`>=` 包含式判定、FIFO vs LRU 的取舍、`Stats` 的历史 race、零向量守卫、连接池借还语义、两条解析器的独立性。本册对这些符号只讲一遍，展开即最终口径。

## 41. RedisStore —— 让缓存成为系统属性 (internal/cache/redis.go)

### 为什么必须有这个后端

源码注释观点：Redis store 让缓存在**网关重启后存活**、并**被每个副本共享**。它是把缓存从"每进程的偶然"变成"系统属性"的东西——两个副本加一个内存 store，一个重复的 prompt 会被轮询打成一半命中一半未命中。

配合的还有 `internal/server/cache.go` 里的一条分支：配置为 Redis 而连不上时，**启动直接失败**（错误 `"server: cache: redis at %s: %w"`），而不是静默退回进程内存——源码注释观点：一个配了共享缓存却悄悄跑在进程内存上的部署，会让不同副本给出不同答案，这比拒绝启动难诊断得多。

### 键布局（源码注释即规格）

一套 scope（一个 tenant + 一个 model，由 `ScopeFor (internal/cache/cache.go)` 决定）在 Redis 里占四个键，另有一张全局表：

- `ig:cache:<scope>:meta` — HASH，field 是条目 key，value 是 `Entry` 的 JSON（**不含向量**）。
- `ig:cache:<scope>:vec` — HASH，field 是条目 key，value 是 base64 的 float32 向量。
- `ig:cache:<scope>:idx` — ZSET，member 是条目 key，score 是 `CreatedAt` 的 unix 毫秒。
- `ig:cache:<scope>:exp` — ZSET，member 是条目 key，score 是 `ExpiresAt` 的 unix 毫秒。
- `ig:cache:scopes` — ZSET，member 是 scope，score 是最后一次写入的 unix 毫秒。

源码注释把三条设计理由写得比代码还长，逐条翻译并补出后果：

1. **向量住在条目旁边，不在条目里面**。一次查找只需要向量：`RedisStore.Search` 读 `vec` 哈希（紧凑、base64），只对胜出者取 JSON。源码注释给的数字：向量放进 JSON 会让每条目从约 1 KiB 涨到约 2.7 KiB，而且**每次查找都要反序列化该 scope 里的每一条**。机制的后果是：JSON 只被 `Get` 解析，且只解析赢家那一条——扫描成本从"反序列化 N 条"降到"base64 解码 N 个向量"，前者比后者贵得多。
2. **过期是逐条目的，在 `exp` ZSET 里，不是哈希上的一个 TTL**。源码注释观点：键级 EXPIRE 会让一个 scope 里的所有条目一起死，**一条繁忙的 prompt 会延长一个不相关的陈旧答案的寿命**。键级 EXPIRE 仍然设置，但只是**安全网（两个 TTL）**，防止"s 再也没人读的 scope"无限积累。
3. **淘汰按创建时间 FIFO，不是 LRU**。源码注释观点：**Redis 不暴露读顺序；一个对"新近程度"判断错误的缓存，比一个对"年龄"诚实的缓存更糟**。内存 store 有真 LRU——这是全项目唯一被文档化的行为分歧。
4. `ig:cache:scopes` 这张表存在的唯一理由是 `Flush("")`：源码注释观点：没有它就只能用 `KEYS` 枚举 `ig:cache:*:meta`，**而 `KEYS` 会阻塞 Redis**。

键名里的 `:` 分隔符与 `RedisOptions.Prefix` 的默认值 `"ig:cache"` 拼成上述形状（`metaKey`/`vecKey`/`idxKey`/`expKey`/`scopesKey` 五个私有方法）。

### 类型与字段

- `RedisStore (internal/cache/redis.go)` — 字段 `cli *redis.Client`、`prefix string`、`opts RedisOptions`、`now func() time.Time`、`mu sync.Mutex`、`stats StoreStats`。
- `RedisStore.mu (sync.Mutex)` — **只保护 `stats`**，不保护 Redis 数据（那是服务端的事）。源码注释在这里交代了一段真实历史：每个请求 goroutine 都会碰计数器，所以它们不是某个调用方的私产；没有这把锁，并发的 `Get` 与 `Search` 会在同一个字上竞争（**race detector 原话就是在 `Get` 的第一处自增**）。源码注释还解释了为什么当初会漏：**Redis 客户端自己的连接池是并发安全的，这让它周围的一切看起来也并发安全**。
- `RedisStore.now (func() time.Time)` — 可注入的时钟。由 `RedisOptions.Now` 决定，为空则 `time.Now`。`Get` 的 `Expired` 判定、`Put` 写 `ExpiresAt`、`pruneExpired` 的 `-inf..now` 上界、`ZADD ig:cache:scopes` 的 score 全部取自它——注入时钟是"过期语义可测"的前提。
- `func (s *RedisStore) bump(f func(st *StoreStats))` — 在 `mu` 内应用一次计数变更。源码注释观点：计数运算散布在**十九个调用点**，一个 helper 让"在新调用点忘记加锁"变得不可能。也就是说，仓库里"计数器只在 `bump` 里改"是一条不变量，任何直接写 `s.stats.X++` 的新代码都是回归。
- `func (s *RedisStore) Name() string` — 返回 `"redis"`。
- `func (s *RedisStore) Client() *redis.Client` — 暴露底层客户端，给 `/admin/cache` 报它的池与错误计数。
- `func (s *RedisStore) Close() error` — 直接 `s.cli.Close()`，因此**幂等性继承自 `redis.Client.Close`**（后者在已关闭时直接返回 nil）。

### RedisOptions 与 NewRedisStore

`RedisOptions (internal/cache/redis.go)` 字段：`Addr`、`Password`、`DB`、`Prefix`、`MaxEntriesPerScope`、`MaxScan`、`Now`、`DialTimeout`、`ReadTimeout`、`WriteTimeout`、`PoolSize`。

- `RedisOptions.Addr` — 源码注释观点：空地址意味着这个 store 不可用，`NewRedisStore` 直接报错，**因为一个静默什么都不做的缓存比一个启动就失败的缓存更糟**。
- `RedisOptions.Prefix` — 覆盖 `"ig:cache"`，让多个网关共用一个 Redis 库而不撞键。
- `RedisOptions.MaxEntriesPerScope` — scope 上界，用 FIFO 裁剪执行。
- `RedisOptions.MaxScan` — 源码注释观点：它**封顶一次 Search 读多少向量**，但超过它**并不致命**（默认无界扫描照跑），只是把数字记进 store 统计，好让运营商看见代价而不是被它惊到。注意实现里超过上限时记的是 `StoreStats.Errors++` 而不是一个专门的字段——这是半截设计，见下文与「未能确认」。
- `RedisOptions.Now` — 测试用时钟。

`func NewRedisStore(opts RedisOptions) (*RedisStore, error) (internal/cache/redis.go)` 的顺序与错误路径：

1. `strings.TrimSpace(opts.Addr) == ""` → `fmt.Errorf("cache: redis addr is empty")`。
2. `Prefix` 空 → `"ig:cache"`。
3. `MaxEntriesPerScope <= 0` → `DefaultMaxEntriesPerScope (internal/cache/cache.go)`（256）。
4. `MaxScan <= 0` → `4096`。
5. `Now == nil` → `time.Now`。
6. `redis.NewClient(redis.Options{...})` 把 `Addr/Password/DB/三个超时/PoolSize` 透传给客户端。
7. `context.WithTimeout(context.Background(), 3*time.Second)` + `cli.Ping(ctx)`；失败时 `_ = cli.Close()` 并返回 `fmt.Errorf("cache: cannot reach redis at %s: %w", opts.Addr, err)`。

源码注释观点：**它 ping 一次**，好让配置错的地址在启动期以一个清晰的错误失败，而不是把每个请求变成一次静默未命中。机制的后果：**构造期必然发生一次真实 I/O**，因此这个构造函数在测试里必须配一个真的监听端（`internal/cache/cache_test.go` 里的 Redis 用例都先起 `mockredis.New(mockredis.Options{Addr: "127.0.0.1:0"})`）。这也解释了为什么 `redis.NewClient` 自己**不连接**：Redis 客户端要允许"起得来、降级为未命中"，而缓存 store 要的是相反的确定性——两条相反的策略分属两层，是刻意的。

### Get

`func (s *RedisStore) Get(ctx context.Context, scope, key string) (Entry, bool, error) (internal/cache/redis.go)`

1. `bump(Gets++)`。
2. `s.cli.Do(ctx, "HGET", s.metaKey(scope), key)`；错误 → `bump(Errors++)` + 返回错误。
3. `reply.Null` → `(Entry{}, false, nil)`，即未命中（RESP2 的 `$-1`）。
4. `decodeEntry(reply.Str)`；失败 → `bump(Errors++)`，**`_, _ = s.Delete(ctx, scope, key)` 后仍返回错误**。源码注释观点：损坏的值当未命中处理，但它同时被删除——留着它会让**此后每一次查找都为这个 prompt 付一遍解析代价**。
5. `e.Expired(s.now())` → `Delete` + `bump(Expired++)` + 返回未命中。源码注释观点：条目在 TTL 过了之后被顺手丢掉，所以**即使没人跑 sweep，store 也是自愈的**。
6. 向量单独读：`HGET vecKey scope key`，只有 `verr == nil && !vreply.Null && embed.DecodeVector 成功` 才填 `e.Vector`。
7. 返回 `(e, true, nil)`。

源码注释观点（第 6 步）：读不到向量时条目仍然可用，它**只能做精确命中**，这是合法状态（embedder 挂掉时存进来的条目），所以读向量失败**不算错误**。机制的后果：`Get` 的"成功"不蕴含"可参与语义检索"，语义路径的可用性完全由 `vec` 哈希决定。

并发：本方法**不读也不写 Redis 共享状态以外的东西**，唯一进程内共享状态是 `stats`（经 `bump`）；`s.cli` 本身并发安全。因此可以任意并发调用。

### Put

`func (s *RedisStore) Put(ctx context.Context, e Entry, ttl time.Duration) error (internal/cache/redis.go)`

1. `now := s.now()`；`e.ExpiresAt.IsZero()` 时填 `now.Add(ttl)`。**注意这是入参拷贝的字段**，所以"没设过期时间的条目由后端补上"这件事对调用方不可见。
2. `vec := e.Vector`；`stored := cloneEntry(&e)` 后 `stored.Vector = nil`，再 `json.Marshal`。源码注释观点：向量存自己的哈希，所以 JSON 里的那份是浪费的字节（约 2.7 KiB → 约 1 KiB）。`Marshal` 失败 → `bump(Errors++)` 并返回该错误（实践中 `Entry` 无法触发，但错误路径在）。
3. 组装命令列表：`HSET meta e.Key <json>`、`ZADD idx <CreatedAt 毫秒> e.Key`、`ZADD exp <ExpiresAt 毫秒> e.Key`、`ZADD scopes <now 毫秒> scope`；然后 `len(vec) > 0` 时 `HSET vec e.Key <base64>`，否则 **`HDEL vec e.Key`**（写入空向量必须删掉旧向量，否则改存"无向量条目"会留下一条能与旧向量匹配的陈旧向量）。
4. 过期安全网：`seconds := int64((2 * ttl).Seconds())`，若 `< 60` 则取 `60`；对 `meta`/`vec`/`idx`/`exp` 四个键各发一条 `EXPIRE <key> <sec>`。源码注释观点：**仅是安全网，两个 TTL，逐条目的 `exp` ZSET 才是权威**。
   - **推断的 why**：`2*ttl` 让键级 TTL 必然晚于任何一条目自己的 TTL，于是正常路径上条目总是先由 `exp`/`Get` 判定死亡，键级 TTL 只在"整个 scope 再也没人写、也没人读"时兜底回收；`60` 秒的下限则挡住 `ttl` 极小（例如秒级）时键比条目还先消失，让 `HSET` 后立刻 `EXPIRE` 的窗口里出现"索引在、数据没了"的悬空状态。
5. `s.cli.Pipeline(ctx, cmds)` 一次往返；失败 → `bump(Errors++)` + 返回错误。源码注释（在 `redis.Pipeline` 上）观点：**没有 MULTI/EXEC**——store 的写各自自洽（每条 HSET 替换整条条目），把它们包进事务会让一次缓存写能阻塞其他副本依赖的 Redis 实例。
6. `bump(Puts++)`，然后 `return s.trim(ctx, scope)`。
   - **机制的后果（重要）**：`Put` 的返回错误**可能来自 `trim` 而不是写入**。也就是说，数据其实写成功了，调用方却看到 `error`。`internal/cache/cache.policy` 一侧把 store 错误降级为"记日志、继续服务"（下文测试用例印证），所以这不会变成用户可见的失败；但读代码时"Put 返回 error"≠"没写进去"。
   - 另一条后果：`Put` **不是原子的**。写入批次与 `trim` 之间、以及 `trim` 的"先问后删"之间，都有并发窗口，见下文 `trim` 的推断段。

### trim —— FIFO 淘汰

`func (s *RedisStore) trim(ctx context.Context, scope string) error (internal/cache/redis.go)`

1. `ZCARD idxKey`，错误 → `Errors++` + 返回。
2. `over := int(reply.Int) - s.opts.MaxEntriesPerScope`；`over <= 0` 直接 `nil`。
3. `ZRANGE idxKey 0 <over-1>` 取"排名最靠前的 over 个成员"。源码注释观点：**先问清楚哪些成员越界，再删**——因为 meta/vec 字段也得跟着删，**只做一次 ZSET 裁剪会把载荷永远留在后面（一个只会在 Redis 内存上显形的泄漏）**。
4. `victims.Strings()`；这个错误路径**没有 `Errors++`**（其余分支都有）。
5. 每个牺牲者拼四条命令：`HDEL meta`、`HDEL vec`、`ZREM exp`、`ZREM idx`；非空则一次 `Pipeline`，失败 → `Errors++` + 返回错误；成功后 `bump(Evicted += len(members))`。

**内容问题（注释与代码不符）**：`RedisStore` 的类型级注释说淘汰是 "FIFO by creation time (ZREMRANGEBYRANK)"，但 `trim` 的实现是 `ZRANGE` + 逐条 `ZREM`，**从未发送 `ZREMRANGEBYRANK`**。全仓库 `ZREMRANGEBYRANK` 只出现在 `internal/mockredis/mockredis.go` 的处理器与 `internal/redis/redis_test.go` 的一条往返用例里。所以那句话描述的是设计意图而非代码路径；语义上等价（都按 `idx` 的创建时间分数从最老者开始），但字符串不匹配。

**推断的 why（为什么 FIFO 而非 LRU）**：Redis 的 ZSET 分数是**写进去的那个数**；读操作不改变任何分数，而且 `Store` 接口的 `Get` 也不打算回写 `idx`（那样每次命中都多一次 ZADD/往返，且把读变成写，放大复制与持久化压力）。**不这样做会发生什么**：如果硬要 LRU，就得在 `Get` 里更新一个"最近使用"分数，于是每一次缓存命中都变成一次写命令——在共享 Redis 上这是把只读的热路径变成写热点；或者引入 `OBJECT IDLETIME` 这类近似，语义变成"大概最近用过"，比诚实的 FIFO 更难解释。因此源码注释的取舍是：**对年龄诚实 > 对新鲜度撒谎**。

**推断的 why（先问后删的并发后果）**：`ZCARD`、`ZRANGE`、最终的 `Pipeline` 是三次独立往返，中间没有事务也没有锁。两个并发 `Put` 落在同一 scope 时，两者可能算出同一批牺牲者并各自删除——`HDEL`/`ZREM` 对同一成员的重复调用是幂等的，所以数据不会错，但 `StoreStats.Evicted` 会**重复计数**（两次 `Evicted += len(members)`）。同理，`trim` 与 `pruneExpired` 可能对同一成员都记一次 `Expired`+`Evicted`。这是统计口径的已知偏差，不是数据损坏。

### Search

`func (s *RedisStore) Search(ctx context.Context, scope string, vec []float32, threshold float64, limit int) ([]Match, error) (internal/cache/redis.go)`

开头就是守卫：`embed.IsZero(vec)` → **`return nil, nil`**，在任何计数与任何 Redis 调用之前。

- 源码注释观点（这里直接引 `embed.IsZero (internal/embed/embed.go)` 的注释）：**"没有方向"必须是未命中，而不是"与阈值恰好相等的所有条目都算命中"**。`embed.IsZero` 的注释给了完整论证：零查询向量不是"什么都不相似"，它是无意义的；算术也同意——`Cosine` 对它返回 0，所以对着一个恰好为 0 的阈值，**每一个已存条目都会合格**，一个算不出 embedding 的调用方于是会拿到一条任意条目——**一个打扮成缓存命中的错答案**。
- **推断的补充**：守卫放在函数最前还有第二个后果——零向量查询**不会**触发 `pruneExpired`、`HGETALL` 与逐条解码。也就是说，调用方反复传零向量时，成本是 O(1) 而不是 O(scope)。这与"报告与冷缓存相同的结果"（`embed.IsZero` 注释原话）是一致的。

其余步骤：

1. `bump(Searches++)`。
2. `pruneExpired(ctx, scope)`；错误时**直接返回该错误**（注意这里**没有** `Errors++`，`pruneExpired` 内部已经记过）。
3. `HGETALL vecKey`；错误 → `Errors++` + 返回错误。`reply.Strings()` 失败同样 `Errors++`。
4. `len(flat)/2 > s.opts.MaxScan` 时 `bump(Errors++)`。源码注释观点：**不致命，扫描照跑，数字被记录下来**，代价因此是可见的而不是突然的。
   - **内容问题**：把"扫描超出配置上限"记进 `Errors` 会让 `/admin/cache` 的 `errors` 把一个纯粹的成本信号报成失败。仓库里没有任何专门字段承接它，所以这是"注释承诺的信息可见性"与"字段语义"之间的错配（详见「未能确认」）。
5. 逐对遍历 `flat`（`for i := 0; i+1 < len(flat); i += 2`，`key, blob := flat[i], flat[i+1]`）：每对 `bump(Scanned++)`；`embed.DecodeVector(blob)` 出错或长度 0 → `continue`（**静默跳过**，不解码失败的向量不参与排名）；`sim := embed.Cosine(vec, v)`；`sim < threshold` → `continue`。
   - **判定是包含式的**：过滤条件是 `sim < threshold` 才剔除，所以 `sim == threshold` **算命中**。这与内存侧 `MemoryStore.Search` 同形，也与 `MinPromptChars` 的包含式比较一致（本册前段记过）。
6. `len(cands) == 0` → `return nil, nil`。
7. `sort.SliceStable` 按 `sim` 降序。
8. `limit > 0 && len(cands) > limit` → 截断到 `limit`。
   - **推断的后果**：截断发生在**取 JSON 之前**，所以 `limit` 同时封顶了后续 `Get` 的次数——这是"只取赢家 JSON"这条设计能真正省下往返的地方。
9. 对每个候选调 `s.Get(ctx, scope, c.key)`；`gerr != nil || !ok` → **`continue`**（这条候选被跳过，错误被吞掉，方法仍可能返回成功）。这是"Redis 抖动时 Search 返回更少结果而不是报错"的具体出处。
10. `Rank(out)`（`internal/cache/cache.go`，按 `Similarity` 降序的稳定排序）后返回。
    - **注意**：`sort.SliceStable` 与 `Rank` 都在排序，`Get` 循环不会改变顺序，所以 `Rank` 在这里是幂等的重申而非必需步骤（本册前段记过 `Rank` 存在的理由是"让 admin 界面与测试对『最近』只有一个定义"）。

并发：本方法对 Redis 只发读命令加 `pruneExpired` 的删除批次，对进程内只经 `bump` 改 `stats`，因此可并发。但**同一 scope 的并发 `Search` 会互相看到对方 `pruneExpired` 的中途状态**——`pruneExpired` 是"先查过期的 key 列表，再批量删四个键"，删到一半时另一个 `Search` 可能读到"`vec` 里还有、`meta` 里已经没了"的成员，此时第 9 步的 `Get` 返回未命中，那条候选被跳过。**不这样做会发生什么**：若改为在读之前加一把全局锁，就把共享 Redis 的并行读串行化了，代价远大于偶尔少一条候选。

### pruneExpired

`func (s *RedisStore) pruneExpired(ctx context.Context, scope string) error (internal/cache/redis.go)`

1. `now := strconv.FormatInt(s.now().UnixMilli(), 10)`，`ZRANGEBYSCORE expKey "-inf" now`；错误 → `Errors++` + 返回。
2. `reply.Strings()` 出错、或成员数为 0 → `return err`（为 0 时返回 nil，**不发空 Pipeline**）。
3. 每个成员四条命令：`HDEL meta`、`HDEL vec`、`ZREM idx`、`ZREM exp`；一次 `Pipeline`；失败 → `Errors++` + 返回。
4. `bump(Expired += len(members))`。

源码注释观点：**用 `exp` ZSET 做这件事，让工作量正比于"过期了多少"而不是"scope 有多大"**。机制的后果：一个 10 万条目的 scope 上每条 TTL 到期，`pruneExpired` 只处理到期者；如果换成"遍历 `idx` 逐条比对 `ExpiresAt`"，每写一条就要扫全量（`Put` → `trim` 虽然不调它，但 `Search` 与 `Len` 都调，等于每次查找都可能退化成全表扫描）。

被调用者：`Search`、`Len`。

### Delete / Flush / flushScope / Len / Scopes

- `func (s *RedisStore) Delete(ctx context.Context, scope, key string) (bool, error)` — `bump(Deletes++)`；一次 `Pipeline` 发 `HDEL meta`、`HDEL vec`、`ZREM idx`、`ZREM exp`；错误 → `Errors++` + `(false, err)`；`len(replies) == 0` → `(false, nil)`（防御性分支，`Pipeline` 对非空 `cmds` 不会返回空切片）；否则**返回 `replies[0].Int > 0`**，即"meta 哈希里到底有没有这个 field"。因为只信第一条回复，索引/过期表里孤立残留（例如只有 `idx` 有而 `meta` 没有）会报"没删掉"。
- `func (s *RedisStore) Flush(ctx context.Context, scope string) (int, error)` — `bump(Flushes++)`；`scope != ""` → `flushScope`。否则 `ZRANGE scopes 0 -1` 列出所有已知 scope，逐个 `flushScope` 并累加；中途出错**返回已累计的 total 与错误**（半途而废是有信息量的）。
  - **机制的后果**：`Flush("")` 只清"注册表里有记录"的 scope。如果某 scope 的键全部由键级 `EXPIRE` 自然死亡，`ig:cache:scopes` 里的成员**仍然在**（`Put` 时写进去，之后没人删），于是 `Flush("")` 会去 `DEL` 已经不存在的键——无害但会计入 `Flush` 的"删了多少"为零。反过来，若某个 scope 从未在 `scopes` 注册过（只可能是外部写入），它不会被清掉。
- `func (s *RedisStore) flushScope(ctx context.Context, scope string) (int, error)` — 先 `ZCARD idxKey` 拿条目数（错误 → `Errors++` + 返回），再一次 `Pipeline` 发 `DEL meta`、`DEL vec`、`DEL idx`、`DEL exp`、`ZREM scopes scope`（错误 → `Errors++` + 返回），最后 `return int(card.Int)`。**先取 card 再删**是为了能报告删了多少条；返回的是请求前的 `ZCARD`，在并发写入下未必等于实际删除数。
- `func (s *RedisStore) Len(ctx context.Context, scope string) (int, error)` — `scope != ""` 时先 `pruneExpired`（源码注释观点：**这样数字与一次查找看到的相符**），再 `ZCARD idxKey`。`scope == ""` 时遍历 `scopes` 注册表**递归**调用自身累加（因此空 scope 的统计也依赖注册表）。签名与 `Store` 接口一致；内存侧对空 scope 也报"全部"。
- `func (s *RedisStore) Scopes(ctx context.Context) (map[string]int, error)` — `ZRANGE scopes 0 -1` 列出 scope，逐个 `Len` 填 `map[string]int`，给 `/admin/cache` 用。中途出错返回**已经填好的部分 map** 与错误。
- `func decodeEntry(blob string) (Entry, error)` — `json.Unmarshal([]byte(blob), &e)`，失败 → `fmt.Errorf("cache: corrupt entry: %w", err)`。**这是本文件里 Redis 键空间之外的第二个错误来源，也是"损坏值当未命中"那条规则的实现点**。

### Stats 与那次真实的 race

`func (s *RedisStore) Stats() StoreStats (internal/cache/redis.go)` 先 `mu.Lock()` 复制 `s.stats`、解锁，再 `out.Errors += s.cli.Stats().Errors` 返回。

源码注释观点：**从运营商视角，"缓存不应答"是一个数字**，所以客户端自己的传输失败被折算进 `Errors`。机制的后果：`/admin/cache` 报的 `errors` = store 内部错误 + 客户端层错误（拨号、写、读、超时各算一次），排查时这两类无法从这一个数字里分开——要看细分得去 `redis.Client.Stats()`（它有 `Dials/Reused/Timeouts/Errors/Waits/Open`）。

修法的痕迹同时留在三处，读源码时应当对照着看：

1. `RedisStore.mu` + `RedisStore.bump`，所有计数器写入必须经 `bump`。
2. `Stats()` 复制后才追加客户端计数，**不在持有 `mu` 时调 `s.cli.Stats()`**（后者要拿 `redis.Client.statMu` 再拿 `redis.Client.mu`）。**推断的 why**：把外部锁拿进自己的临界区会引入锁序约束；这里先解锁再合并，是"不留嵌套锁序"的写法。`redis.Client.Stats` 内部自己定义了固定的 `statMu → mu` 顺序，两层之间因此没有环。
3. `internal/cache/cache_test.go` 的 `TestStoreStatsSurviveConcurrentReaders` 把这件事钉成可执行断言。

该用例的注释明说：Redis store 的计数器**曾经**是从每个请求 goroutine 裸改的普通字段，下边的交错正是 race detector 在 `Get` 的第一处自增抓到的东西，也是"修改点现在共用一把互斥锁"的原因。用例对 `storeFactories()` 里的**两个** store 都跑（内存 store 一直有锁，所以任一后端回归都会红）：4 个 writer 各 25 轮 `Put`/`Get`/`Search`，1 个 reader 循环读 `Stats()` 直到 writer 结束（源码注释观点：这样"读的同时写"才是被测的交错，而不是我们希望出现的调度），最后断言 `Puts`/`Gets`/`Searches` 都动过。

### 与内存侧的三条关键差异（本册必须讲透的对照）

1. **淘汰**：`MemoryStore` 是 LRU（`internal/cache/cache.go` 侧的 `memScope` 与 `janitor` 负责），`RedisStore` 是按 `CreatedAt` 的 FIFO。`TestRedisStoreIsFIFOAndDocumentedSo` 与 `TestMemoryStoreIsLRU (internal/cache/cache_test.go)` 是**同一场景的一对**，源码注释观点：把两者都钉住，"差异就从一个意外变成一个决定"。FIFO 用例的执行顺序是关键：上限 3，写 k0..k2，**先 `Get` 触热点 k0**，再写 k3，然后断言 **k0 被淘汰**——源码注释原话：**Redis 报告不了读顺序，所以触热点不能救它**（内存用例断言 k0 存活、k1 被淘汰）。
2. **故障语义**：`TestRedisStoreDegradesWhenTheServerGoesAway (internal/cache/cache_test.go)` 起 mockredis、建 store、再 `srv.Close()`，然后断言 `Get`、`Put`、`Len` **都必须返回错误**（"让调用方能记日志"），且 `Stats().Errors` 非零。源码注释观点：**Redis 故障必须是未命中，绝不能让请求失败**——缓存是优化，网关必须继续服务。"返回错误"与"网关继续服务"并不矛盾：`internal/cache` 的上层把 store 错误降级为记日志 + 继续走上游。
3. **计数口径**：`Stats()` 在 Redis 侧多折算了 `redis.Client.Stats().Errors`；内存侧没有这一项。

### cache_test.go 里其余 Redis 专属用例

- `TestNewRedisStoreRejectsAnEmptyAddress (internal/cache/cache_test.go)` — 空地址必须在构造期失败，而不是静默把缓存关掉（对应 `RedisOptions.Addr` 的源码注释观点，错误串 `"cache: redis addr is empty"`）。
- `TestNewRedisStoreFailsOnAnUnreachableAddress (internal/cache/cache_test.go)` — `Addr: "127.0.0.1:1"` + 300ms 超时，断言启动即报错；这条钉的是 `NewRedisStore` 里那次 `Ping` 与 `"cache: cannot reach redis at %s: %w"`。
- 另有一处（`internal/cache/cache_test.go` 上一组 Redis 工厂里的 `NewRedisStore` 调用）把两个后端接进 `storeFactories()`，让 `TestStoreConformance` 之类的契约用例在两个后端上同跑——源码注释观点：两个 store 背后是同一套策略，"只在其中一个上成立的行为，就是运维切换后端时才会出现的 bug"。

## 42. internal/redis —— 手写 RESP2 客户端 (internal/redis/redis.go)

### 为什么手写

包注释观点：理由与 `internal/miniyaml` 相同——**网关要能用裸 Go 工具链、零模块下载构建**，为一个主题其实是缓存的里程碑引入 Redis 依赖会变成构建前提。暴露面刻意收窄到缓存 store 的需要：`Do(ctx, args...)` 一条命令一个回复；`Pipeline(ctx, cmds)` N 条命令 N 个回复、一次往返、**不带 MULTI**。RESP2 对 store 用到的类型实现了全套（简单字符串、错误、整数、bulk string、nil bulk、数组），**包括嵌在数组里的回复**。`EVAL` 真实服务器上有，但这里不用——源码注释观点：**Lua 脚本会把 store 的语义对读 Go 代码的人藏起来，与这个项目的目标正相反**。这里没有的：RESP3（hello/attributes/push）、pub/sub、MULTI/EXEC、集群重定向、TLS——每一个都是真实特性，共享缓存都不需要。

### 两个上限各自挡什么

- `maxReplyDepth (internal/redis/redis.go)` — `const maxReplyDepth = 32`。源码注释观点：否则恶意或混乱的服务器能把数组一直嵌下去直到栈溢出，**"被一个回复搞崩的客户端"是最难调的那类失败**。由 `ReadReplyBoundsNesting (internal/redis/redis_test.go)` 钉住（100 层嵌套必须报错而不是崩）。
- `maxReplySize (internal/redis/redis.go)` — `const maxReplySize = 64 << 20`。源码注释观点：向量以 base64 存储（几 KiB），所以到 MB 级就意味着要么 key 错了、要么对面有敌意。**它同时被当成数组的元素个数上限复用**（`'*'` 分支里 `n > maxReplySize` 报 `"redis: array of %d elements exceeds the cap"`）——即 "64 Mi 个元素" 这个上限实际上永远不会靠元素数触发，真正起作用的是每个元素的 bulk 长度上限。这是一处**名义语义与实际语义不同**的地方。

### 错误是值，不是传输失败

- `Error (internal/redis/redis.go)` — 字段 `Message string`；`func (e *Error) Error() string` 返回 `"redis: " + e.Message`。源码注释观点：Redis 既用错误表示真失败（`WRONGTYPE`）也表示预期结果（`NOSCRIPT`），所以它是**值，不是传输失败**。
- `func IsNotFound(err error) bool` — `errors.As` 取 `*Error`，判 `strings.HasPrefix(re.Message, "ERR no such key")`。源码注释观点：让 store 把"key 不存在"当成缓存未命中而不是错误。**注意**：`RedisStore` 的读路径走的是 `HGET` 的 `Null` 回复，所以 `IsNotFound` 在当前 cache store 里**没有调用点**；它服务的是会收到 `ERR no such key` 的命令（如 `GET`/`RENAME` 类）。

### Reply 与它的访问器

- `Reply (internal/redis/redis.go)` — 字段 `Kind byte`、`Str string`、`Int int64`、`Array []Reply`、`Null bool`。
- `func (r Reply) Text() (string, error)` — `'+'`/`'$'` 返回 `r.Str`；`Null` → `errors.New("redis: reply is a null string")`；其他 → `"redis: reply of type %q is not a string"`。被 `Client.Ping` 调用。
- `func (r Reply) Strings() ([]string, error)` — 数组元素转字符串。源码注释观点：这是大多 store 操作想要的形状（HGETALL、ZRANGE），放这里能让类型分支不进缓存代码；**nil 元素变成空串而不是错误**——Redis 在 HGETALL 回复里会为"两步内部操作之间消失的字段"返回 nil。错误路径：非数组 → `"redis: reply of type %q is not an array"`；`Null` → `(nil, nil)`；元素类型不是 `'+'`/`'$'`/`':'` → `"redis: array element of type %q is not a string"`。整数元素按十进制格式化。**这个"nil → 空串"的宽松正是 `RedisStore.Search` 需要 `len(v) == 0` 二次判断的原因**。
- `func (r Reply) Ints() ([]int64, error)` — 非数组 → `"redis: reply of type %q is not an array"`；`':'` 直接取 `Int`，`'$'`/`'+'` 走 `strconv.ParseInt(strings.TrimSpace(el.Str), 10, 64)`，失败 → `"redis: array element %q is not an integer"`；其他类型 → `"redis: array element of type %q is not an integer"`。`Null` **未单独处理**，nil 数组走空循环返回空切片。**本仓库内没有调用点**（如实记录）。
- `func (r Reply) AsError() error` — `Kind != '-'` 返回 nil，否则 `&Error{Message: r.Str}`。源码注释观点：**只关心成功的调用方可以忽略它；需要区分 WRONGTYPE 与超时的调用方不能忽略**。被 `Client.dial`（AUTH/SELECT 之后）与 `Client.Ping` 调用。

### 池的默认值

`Options (internal/redis/redis.go)` 与 `func (o *Options) applyDefaults()`：

- `Addr` 空 → `"127.0.0.1:6379"`；
- `DialTimeout`、`ReadTimeout`、`WriteTimeout` 各自 `<= 0` → `2s`；
- `PoolSize <= 0` → `16`。源码注释观点：**一次缓存查找绝不能排在比请求并发还小的池后面**，客户端会记录等了多久（`Stats.Waits`）；
- `IdleTimeout <= 0` → `5m`。源码注释观点：Redis 自己会关闲置连接（默认 `timeout 0`，即永不），但中间的 NAT 或负载均衡通常会关，**而复用一条死 socket 会变成一次莫名其妙的缓存未命中**；
- `Password` 非空时在**每个新连接上**发 AUTH。源码注释观点：**在连接时而不是按需 AUTH，因为池化连接没有请求上下文可以携带凭据**——这是本包"没有请求级连接状态"的直接推论；
- `DB` 非零时 `SELECT`。源码注释观点：0 是默认值、也是单租户部署该用的值；非零 DB 是两个环境共用一个 Redis 而不共享 key 的办法。

### 客户端结构

`Client (internal/redis/redis.go)`、`conn (internal/redis/redis.go)`、`Stats (internal/redis/redis.go)`：字段与逐条说明见本册 `internal/redis` 一节。本册只补三条机制：

- `Client.sem (chan struct{})` — 容量 `PoolSize` 的信号量。源码注释观点：**用 channel 而不是 WaitGroup，因为获取需要超时和取消**（`select { sem <- ...; <-ctx.Done() }`）。
- `conn.buf (*bufio.Reader)` — 32 KiB 缓冲读取器，**跨命令保留**。这正是"半读回复必须关连接"的物理原因：上一条命令的回复字节可能还躺在这个缓冲区里。
- `Client.closed` — `Close` 置位；在途连接由各自 `release` 的 `closed` 检查销毁。

### 借与还：为什么没有 MULTI/EXEC

`func (c *Client) acquire(ctx context.Context) (*conn, error)`：先非阻塞试 `c.sem <- struct{}{}`，失败则 `Waits++` 并阻塞在 `select`（取消时返回 `ctx.Err()`）。随后在 `mu` 下从 `idle` **取栈顶（LIFO）**；超龄（`time.Since(cn.lastUsed) > IdleTimeout`）则 `open--`、**解锁—关连接—重新加锁**（不持锁做 IO）后继续循环。空闲耗尽且 `closed` → 归还信号量 + `errors.New("redis: client is closed")`；否则 `open++`、解锁、`Dials++`、`c.dial(ctx)`，失败时回退 `open`、归还信号量、`Errors++` 并返回。

**LIFO 的推断**：栈顶是最近释放的连接，最热的那条会被持续复用，其余自然老化到 `IdleTimeout` 被回收；若用 FIFO 轮转，所有空闲连接的使用间隔被拉平，超龄淘汰的收益变小而重连次数变多。

`func (c *Client) release(cn *conn, healthy bool)` —— 本册必须讲透的一条：

- `cn == nil` 直接返回。
- **`healthy == false`：必须关闭，绝不归还**。源码注释观点：**一条半读了回复或已死 socket 的连接必须关闭、绝不能归还：下一个借用者会读到上一条命令的回复，缓存就会把它存到错的 key 下面**。实现是 `open--`、`cn.nc.Close()`、`<-c.sem`。
  - **机制的后果（逐步推演）**：`Pipeline` 在读完所有回复**之前**出错时（写失败、读超时、`readReply` 报格式错），`healthy` 始终是初值 `false`，defer 因此销毁连接。假如放回池里，`conn.buf` 里残留的半个回复字节会被下一位借用者的 `readReply` 当成**它自己命令的第一个字节**；更糟的是如果残留字节恰好构成一个合法回复，借用者会拿到**上一条命令的回复**，而 `RedisStore.Get` 会把它 `decodeEntry` 成一条**属于别的 key 的 Entry**——一个"看起来完全正常"的错误命中。这正是源码注释里"缓存就会把它存到错的 key 下面"的含义。
- 健康路径：刷新 `lastUsed`；若 `c.closed` 则同样关闭而不入池；否则 `append` 进 `idle`。**三条路径最后都要 `<-c.sem`**，而 `acquire` 的失败分支各自显式归还——归还信号量的责任被分散在两侧，读代码时要注意"每条 return 路径是否都还了"。

`func (c *Client) dial(ctx context.Context) (*conn, error)` — `net.Dialer{Timeout: DialTimeout}.DialContext(ctx, "tcp", Addr)`，失败 → `"redis: dial %s: %w"`；成功则 `&conn{nc, bufio.NewReaderSize(nc, 32<<10), lastUsed: time.Now()}`。源码注释观点：**握手在连接上做，而不是每条命令做**——AUTH 和 SELECT 在 Redis 里是连接状态，放这里意味着 store 永远不用考虑它们。`Password != ""` → `roundTrip(cn, []string{"AUTH", Password})`：传输错误 → 关连接 + `"redis: AUTH: %w"`；回复是错误 → 关连接 + `"redis: AUTH rejected: %w"`。`DB != 0` 同理 → `"redis: SELECT %d: %w"` / `"redis: SELECT rejected: %w"`。**所有失败分支都 `nc.Close()`**，不留半初始化连接（此时它还没被 `open++` 计入，由 `acquire` 的失败分支负责回退计数）。

### Do 与 Pipeline

`func (c *Client) Do(ctx context.Context, args ...string) (Reply, error)` — `len(args) == 0` → `errors.New("redis: Do needs at least one argument")`；否则 `c.Pipeline(ctx, [][]string{args})` 后返回 `replies[0]`。**它只是 `Pipeline` 的单命令封装**，所以 `Do` 的开销与 `Pipeline` 完全一样（一次借还、一次写、一次读）。

`func (c *Client) Pipeline(ctx context.Context, cmds [][]string) ([]Reply, error)` — 空 `cmds` → `(nil, nil)`，**不借连接**。之后：

1. `acquire`；`healthy := false` + `defer func() { c.release(cn, healthy) }()`。
2. 写 deadline = `min(now+WriteTimeout, ctx deadline)`；`SetWriteDeadline` 失败**直接返回该错误**（经 defer 归还不健康连接）。
3. 用 `appendCommand` 把全部命令拼进一个 `buf` 后**单次 `Write`**；失败 → `Errors++` + `"redis: write: %w"`。这一次写是"pipeline"这个名词的全部含义：不带 MULTI 的批量只是**一次往返**，服务端仍是逐条执行、每条原子，但**批次整体不原子**。
4. 读 deadline 同理；循环 `len(cmds)` 次 `readReply(cn.buf, 0)`；失败 → `Errors++`，若 `err.(net.Error).Timeout()` 为真再 `Timeouts++`，返回该错误。
5. 全部读完才 `healthy = true` 并返回 replies。

源码注释（`Pipeline` 上）观点：**没有 MULTI/EXEC**——store 的写各自自洽（每条 HSET 替换整条条目），把它们包进事务会让一次缓存写能阻塞其他副本依赖的 Redis 实例。

**本册对"MULTI/EXEC 为什么不能实现"的机制补全**（这是全片最该记住的一条）：连接池是按**一次调用**借还的。`acquire` 在 `Pipeline` 开头借一条、`defer release` 在结尾还一条。因此如果暴露 `MULTI`/`EXEC` 两个独立调用，它们**极可能落在两条不同的连接上**：`MULTI` 在连接 A 上开启的队列，`EXEC` 在连接 B 上执行——那时 B 上根本没有待执行队列，`EXEC` 返回空数组或错误；更坏的情况是 A 被下一个借用者拿走，那个借用者的命令被塞进 A 上仍然开着的 MULTI 队列里，于是**他自己的命令被静默吞掉、直到某次 EXEC 才一起执行**。这不是"没实现"，而是**在这个连接模型下无法正确实现**；要正确实现就得引入"事务期间独占连接"的租借 API，那会污染本包刻意收窄的表面。

**替代品**：需要原子性的地方改用**单条命令**——`INCR` 族（`INCR/INCRBY/DECR/DECRBY/HINCRBY/ZINCRBY`）。单条命令在真实 Redis 上是原子的，整段读-改-写在服务端锁内完成。这条结论同时写在 `internal/mockredis/mockredis.go` 的包注释里（见下文"计数器命令就是 MULTI/EXEC 的替代品"）。

### Ping / Close / roundTrip

- `func (c *Client) Ping(ctx context.Context) error` — `Do(ctx, "PING")` → `AsError()` → `Text()`；文本不是 `"PONG"`（或 `Text` 出错）→ `"redis: PING returned %q"`。源码注释观点：**这正是 /healthz 报告的东西**。被 `NewRedisStore` 与健康检查调用。
- `func (c *Client) Close() error` — 幂等：`mu` 下若已 `closed` 直接返回 nil，否则置位、摘走 `idle` 并置 nil、`open -= len(idle)`，解锁后逐个关闭空闲连接并返回**第一个**错误。**它只关闭空闲连接**，在途连接由各自 `release` 时的 `closed` 检查销毁；新调用会在 `acquire` 拿到 `"redis: client is closed"`。被 `RedisStore.Close` 调用。
- `func (c *Client) roundTrip(cn *conn, args []string) (Reply, error)` — 只在连接握手期使用。源码注释观点：**那时回复必须在连接入池之前读掉**——否则该连接加入池后，缓冲区里躺的 AUTH/SELECT 回复会被第一个借用者当成自己的。错误**不包装**（与 `Pipeline` 不同）。
- `func (c *Client) Stats() Stats`、`func (c *Client) addStat(f func(*Stats))`、`func (c *Client) Addr() string` — 见本册 `internal/redis` 一节；要点是 `statMu` 与 `mu` 分开、`Stats` 快照时锁序固定为 `statMu → mu`。

### 编解码与畸形输入

`func appendCommand(buf []byte, args []string) []byte` — `*<n>\r\n` 后每个参数 `$<len>\r\n<bytes>\r\n`。源码注释观点：**一切都是 bulk string，包括数字**——Redis 会解析它们，而 inline command 形式对含空格的值是个陷阱。`TestAppendCommandEncodesRESP2 (internal/redis/redis_test.go)` 额外钉住"值里的 CRLF 必须留在自己的 bulk string 内"（二进制安全）。

`func readLine(r *bufio.Reader) (string, error)` — `ReadString('\n')`，失败 → `"redis: read line: %w"`；成功则依次 `TrimSuffix("\n")`、`TrimSuffix("\r")`。**注意客户端这一侧只"剥掉" CRLF，不校验它**——如果服务端发 `+OK\n`（缺 `\r`），这里会接受。严格校验在 mockredis 那一侧（见下文）。

`func readReply(r *bufio.Reader, depth int) (Reply, error)` — 防畸形长度前缀的四处都在这里，逐条列出（错误串原文）：

| 输入形状 | 分支 | 结果 |
| --- | --- | --- |
| `depth > maxReplyDepth` | 函数入口 | `"redis: reply nesting exceeds %d levels"`（不是崩栈，`TestReadReplyBoundsNesting`） |
| 未知前缀（如 `?`） | `default` | `"redis: unknown reply type %q"` |
| `:abc` | `':'` 分支 | `"redis: invalid integer reply %q"` |
| `$x` | `'$'` 分支 | `"redis: invalid bulk length %q"` |
| `$-1`（负数） | `'$'` 分支 `n < 0` | 合法：`Reply{Kind: '$', Null: true}`（**负数不是一律拒绝，`-1` 是 RESP2 的 null**） |
| `$99999999999`（超大） | `'$'` 分支 `n > maxReplySize` | `"redis: bulk reply of %d bytes exceeds the %d byte cap"`（**在 `make` 之前挡住分配**，否则一条回复就能让进程 OOM） |
| `$10\r\nabc\r\n`（长度与实际不符） | `io.ReadFull` 失败 | `"redis: read bulk reply: %w"`（`make([]byte, n+2)` 一次读 `n` 字节 + 尾部 CRLF；**这里的 `+2` 就是"吞掉 CRLF"**，但客户端不检查这两个字节是不是 CRLF） |
| `*x` | `'*'` 分支 | `"redis: invalid array length %q"` |
| `*-1` | `'*'` 分支 `n < 0` | 合法：`Reply{Kind: '*', Null: true}` |
| `*2\r\n$1\r\na\r\n`（元素不够） | 递归 `readReply` | 内层 `"redis: read reply: %w"`（EOF）逐层上抛 |

`TestReadReplyRejectsMalformedFraming (internal/redis/redis_test.go)` 的六个用例（unknown type / bad integer / bad bulk length / short bulk / bad array length / truncated array）与上表一一对应。

**推断的 why（为什么客户端不校验 CRLF 而服务端校验）**：客户端的对端是 Redis，畸形回复意味着"对面不是 Redis"，此时唯一正确的动作是**尽快失败并关连接**（`healthy=false` 已经保证这一点）；再多校验一层只会增加热路径成本。而 mockredis 的对端是**本仓库自己的客户端**，那里校验 CRLF 的目的不是防御，是**抓自己的 bug**——见下节。

## 43. internal/mockredis —— 必须独立的第二实现 (internal/mockredis/mockredis.go)

### 包注释的论点

- 为什么存在：源码注释观点：store 的真实风险不是"HSET 能不能往返"，而是"**线协议能不能活着穿过一条 socket**"；一个只能用 `docker run redis` 来演练的 store，就是永远不被演练的 store。它用 TCP 说 RESP2，所以**被测的客户端就是生产客户端，一字未改**。
- **为什么必须独立**（本册重点）：源码注释观点：**这里的解析器是独立于 `internal/redis` 的解码器写的。共用一份实现会让一个错误的长度前缀或缺失的 CRLF 在对话的两边同时通过，而这恰恰是协议级测试替身本该抓住的一类 bug。** 具体到代码：`readCommand` 用 `bufio.Reader` 自己逐字节读 `*`/`$`，自己 `strconv.Atoi` 长度，自己 `io.ReadFull` 读 `l+2` 字节，并且**显式检查 `buf[l] != '\r' || buf[l+1] != '\n'`**（错误 `"bulk string is not terminated by CRLF"`）；`readLine` 甚至要求行尾必须是 `"\r\n"`（否则 `"line is not terminated by CRLF"`）。源码注释观点：**严格检查就是独立解析器的意义——一个忘了尾部 CRLF 的客户端，在这里会被容忍，然后被真实 Redis 拒绝。**
  - **机制的后果**：真实 Redis 对客户端命令的容错度其实不低（它在读命令时对行尾比较宽松），所以这条断言的真正价值在于：**仓库里任何一次"为了省事把解析改成 `TrimSuffix` 式宽容"的改动都会立刻变红**，而不是等到线上换到真 Redis 才暴露。
- 支持与不支持的命令集（源码注释原文范围）：字符串带 `EX/PX/NX/XX` 外加 `SETNX`/`SETEX`；整数计数器 `INCR/INCRBY/DECR/DECRBY`，以及配额需要的按字段/按分数的 `HINCRBY`/`ZINCRBY`；`MGET`、哈希、有序集合（排名与分数区间）、过期、键检视（`TYPE/KEYS/DBSIZE/INFO`）、`SELECT`、配置了密码时的 `AUTH`、`FLUSHDB/FLUSHALL`，以及键上限下的可选 LRU 淘汰——**源码注释观点：那是缓存真正依赖的唯一一种 Redis 淘汰策略**。不支持的：脚本（`EVAL`）、`MULTI/EXEC`、`WATCH`、pub/sub、流水线限制、复制、模块命令、RESP3。

### 计数器命令就是 MULTI/EXEC 的替代品

源码注释观点：**计数器命令被刻意当作 `EVAL` 的原子替代品**——`internal/redis` 每次调用借还一条连接，所以一段 `MULTI/EXEC` 序列会跨好几条连接、比没用还糟；而单条命令在真实服务器上是原子的。因此**一个只建立在 `INCRBY` 之上的配额 store，对"它能保证什么、不能保证什么"是诚实的**。

代码里这条承诺体现为：`cmdIncr`、`cmdHIncrBy`、`cmdZIncrBy` 都在**同一把 `Server.mu` 内**完成"读现值 → 加 → 写回 → 返回新值"，即整个读-改-写原子。`cmdIncr` 的注释还补了一条 TTL 语义：**对一个已存在的 key 自增不会动它的 deadline**，所以窗口计数器的过期不会在流量最大时滑走——源码注释观点：**一个滑动窗口永远不会在流量最重的时候正好重置**。实现上 `expire` 变量从旧 `stringVal` 复制到新 `stringVal` 正是这条语义。

TTL 溢出与类型检查的细节（本册逐条落实）：

- 溢出检查 `if (delta > 0 && next < cur) || (delta < 0 && next > cur)` → `"ERR increment or decrement would overflow"`（`cmdIncr` 与 `cmdHIncrBy` 各一份）。
- 非整数现值 → `"ERR value is not an integer or out of range"`（`cmdIncr`）；哈希字段非整数 → `"ERR hash value is not an integer"`（`cmdHIncrBy`）——两者用不同的错误串，这是有信息量的。
- 对属于 hash/zset 的 key 做 `INCR` → `"WRONGTYPE Operation against a key holding the wrong kind of value"`（`cmdIncr` 里 `keyExistsLocked` 为真但 `s.strs[key]` 缺席时的分支；`cmdHIncrBy` 里反向检查 `s.strs[key]` 存在时同样报 WRONGTYPE）。
- 参数个数不符一律走 `arity(cmd)` → `"ERR wrong number of arguments for '<小写命令名>' command"`，例如 `arity("ZADD")` 得到 `ERR wrong number of arguments for 'zadd' command`。

### 一把大锁与它的纪律

`Server.mu (sync.Mutex)` 保护 `strs`/`hashes`/`zsets`/`expires`/`access`/`clock`/`stats`/`clients`/`authOK` 与 `closed`。**源码注释观点（集合访问器那一节）**：`getHash`/`getZSet` **返回副本**；返回活的 map 会把一个比锁活得久的引用交给调用方，两条连接一读一写同一个哈希就会踩到 Go 的并发 map 写检查——**一个看起来完全像网关自己的 bug 的、发生在测试替身里的崩溃。** 这条纪律的直接后果是：`cmdHGet`/`cmdHLen`/`cmdHKeys`/`cmdHVals` 在拿到副本之后**还要再拿一次锁**（副本是安全的，但读 `len`/遍历期间不应阻塞写；实现上它们确实又 `s.mu.Lock()` 了一次）。

`dispatch` 的形状：先在一小段临界区里 `stats.Commands++`、`clock++`（LRU 时钟），并读本连接的 `authOK`；然后 `AUTH` 单独处理；`RequirePass != "" && !authed` → `"NOAUTH Authentication required."`；`QUIT` → `+OK`。然后是两个 `switch`：第一个处理与缓存无关的通用的命令（`PING`/`ECHO`/`SELECT`/`CONFIG`/`COMMAND`/`INFO`/`DBSIZE`/`FLUSHDB`/`FLUSHALL`），第二个源码注释点名：**"下面这些就是缓存 store 用到的命令"**——`DEL`、`EXISTS`、`EXPIRE`/`PEXPIRE`、`TTL`/`PTTL`、`TYPE`、`KEYS`、`GET`、`SET`、`SETNX`、`SETEX`、`INCR` 族、`MGET`、`HSET`/`HMSET`、`HGET`、`HINCRBY`、`HDEL`、`HGETALL`、`HLEN`、`HKEYS`、`HVALS`、`ZADD`、`ZCARD`、`ZRANGE`/`ZREVRANGE`、`ZRANGEBYSCORE`、`ZSCORE`、`ZINCRBY`、`ZREM`、`ZREMRANGEBYRANK`、`ZREMRANGEBYSCORE`。都不匹配 → `"ERR unknown command '" + args[0] + "'"`。

**已发现的行为差异（如实记录）**：

- **`QUIT` 回 `+OK` 但不关连接**。代码里 `if cmd == "QUIT" { return encodeSimple("OK") }`，之后循环继续读下一条命令。真实 Redis 在回 `+OK` 后**关闭连接**。后果：任何依赖"`QUIT` 之后这条连接不可再用"的测试在这里都会得到相反结论。（本仓库内没有这样的测试。）
- **`SELECT` 是无操作的 `+OK`**。源码注释观点：单库服务器，接受 `SELECT` 只是为了让客户端的握手路径能跑通；多逻辑库是配置问题不是缓存问题。后果：`Options.DB` 的非零值在这里**无法被区分**，`internal/redis/redis_test.go` 的 `TestClientSelectsADatabase` 只能断言握手成功，断言不了"库真的换了"。
- **`KEEPTTL` 被接受但忽略**。源码注释观点：缓存里没有任何东西依赖跨写保留 TTL，**静默接受比让 store 某天可能发的命令失败要好**。
- **`CONFIG`/`COMMAND` 返回空数组**——够真实客户端"探测能力"时不报错。
- **`expires` 表只服务 hash/zset，字符串自带 deadline**。源码注释观点：这镜像 Redis 的存法——deadline 属于 key；而**一个让 hash 过期、却没让它的索引过期（或反过来）的缓存，会留下指向不存在数据的索引**。因此 `cmdExpire` 对存在于 `hashes`/`zsets` 的 key **只写一条 `s.expires[key] = at`**，`expireLocked` 也**同时删 `hashes` 与 `zsets`**（`hadH || hadZ` 才计一次 `Expired`）。这条"哈希与有序集合过期必须同步"就是 `RedisStore` 每条目占四键仍然正确的底层保证。
  - **推断的后果**：正因为 `expireLocked` 同步删两表，`RedisStore` 的键级 `EXPIRE` 安全网一旦触发（例如 scope 无人访问超过 `max(2*ttl, 60)` 秒），`meta`/`vec`/`idx`/`exp` 四个键会**几乎同时**消失，因此不会留下"索引指向空数据"的中间态；真正的中间态只出现在删除批次的中途（上文 `pruneExpired` 一段已讨论）。
- **`KEYS` 过滤 `"\x00expire:"` 前缀**：`add` 里显式跳过。这是一处**实现里存在、但其他地方没有任何代码产生该前缀键**的防御（`expires` 用的是独立 map，不占用键名空间）。也正因为有它，KEYS 不会把内部簿记泄漏成"用户键"。

### 过期、清理与淘汰

- `sweepLoop` 每秒 tick 一次调 `sweepExpired`。源码注释观点：**像 Redis 的主动过期周期那样**丢掉过期键；没有它，一个"写一次、再也没人读"的键会永远占着内存。
- `expired(expire time.Time) bool` — `!expire.IsZero() && time.Now().After(expire)`。
- 读路径的懒过期统一走 `getString`/`getHash`/`getZSet`/`expireLocked`/`keyExistsLocked`。源码注释观点：每个读**都**经过这几个访问器，好让"在某条代码路径上忘记懒过期"变得不可能。
- `touch(key)` — 只在 `MaxKeys > 0` 时写 `s.access[key] = s.clock`。`clock` 在**每条命令**的 `dispatch` 开头自增（不只是写命令），所以"访问顺序"的粒度是命令序号。
- `evictLocked()` — `MaxKeys <= 0` 直接返回；总数（`len(strs)+len(hashes)+len(zsets)`，**键数而非条目数**）不超上限也返回；否则把 `access` 按 `clock` 升序排序，从最老者开始删（同时删它可能处于的三种表），直到回到上限。源码注释观点：**它在一次写之后运行，Redis 自己的淘汰也在那里跑。**
  - **推断的后果**：`access` 里可能残留已被 `DEL`/`EXPIRE` 删除的键（`cmdDel` 会 `delete(s.access, k)`、`expireLocked` 也会），但也存在只在 `strs` 里 `touch` 过、之后被 `evictLocked` 之外的路径清掉的键——排序循环对"键已不存在"的情况用 `removed` 标志处理，不计入 `Evictions`。所以 `Stats().Evictions` 数的是**真正被删掉的键**。
  - **注意与 `RedisStore` 的关系**：`cmd/miniredis` 的 `-max-keys` 默认 4096，而 `RedisStore` 自己还有一套按 scope 的 FIFO 裁剪。两者叠加时，先到上限的那一套生效：mockredis 的 LRU 是**跨 scope 全局**的，Redis 的 FIFO 是**每 scope** 的。**在 mockredis 上调小 `-max-keys` 会让测试看到"随机某个 scope 的条目消失"，与真 Redis 配 `maxmemory-policy allkeys-lru` 的行为同构**——这是刻意的仿真，不是 bug。

### RESP2 编解码（服务端侧）

- `encodeSimple` → `"+" + s + "\r\n"`；`encodeError` → `"-" + s + "\r\n"`；`encodeInt` → `":" + n + "\r\n"`；`encodeBulk` → `"$<len(s)>\r\n<s>\r\n"`；`encodeNilBulk` → `"$-1\r\n"`；`encodeArray(items)` → `*<n>\r\n` + 每个元素 `encodeBulk`。
- `encodeArrayRaw(elems)` — 源码注释观点：它框的元素**已经是 RESP 编码过的**；`MGET` 需要它，因为它的回复混了 bulk string 与 nil，而 `encodeArray` 表达不了——它把每个元素都当 bulk string 框，nil 会变成四个字符 `$-1`。**这是一处很典型的"看起来只是省一层编码、实际是语义错误"的注释**：`cmdMGet` 因此用 `encodeNilBulk()` 与 `encodeBulk(v.value)` 拼 `parts` 再 `encodeArrayRaw(parts)`。
- `readCommand(r)` — 服务端解析一条命令：必须 `*` 开头（否则 `"expected an array, got %q"`）；行长度 `strconv.Atoi` 失败或 `n < 0` → `"invalid array length %q"`；`n > 1<<20` → `"array of %d elements is too large"`；每个元素必须 `$` 开头（否则 `"expected a bulk string, got %q"`）；`l < 0` → `"invalid bulk length %q"`；`l > 64<<20` → `"bulk string of %d bytes is too large"`；`io.ReadFull(buf, l+2)` 后**显式检查尾部两字节是 `\r\n`**，否则 `"bulk string is not terminated by CRLF"`。
- `handle` 循环里，`readCommand` 的错误（除 `io.EOF` 外）会**回一条错误再关连接**：`_ = writeError(w, "ERR Protocol error: "+err.Error())`，源码注释观点：**畸形命令要被回答，而不是被忽略——客户端必须看到错误回复而不是挂住。**
- `readLine(r)` — 要求 `strings.HasSuffix(line, "\r\n")`，否则 `"line is not terminated by CRLF"`；与 `internal/redis` 那个只"剥后缀"的同名函数形成对照。

### 哈希与有序集合的细节

- `cmdHGetAll` 的注释是本册里**信息量最高的一条**：`HGETALL` 返回的是"字段, 值, 字段, 值…"的平铺数组，所以**唯一能排序的是字段列表**；**给平铺数组排序会把所有值挪到所有字段后面，静默地把每个字段配到错的值上——按位置读配对的调用方于是会把一个相似度存到不属于它的 key 下面**。源码注释观点：Redis 不保证顺序，这里排序只是为了测试确定性。
  - 这条注释解释了 `RedisStore.Search` 为什么敢用 `flat[i], flat[i+1]` 成对取值（客户端侧同理依赖"成对"这个不变量），也解释了为什么排序必须**成对**进行（实现：先排 `keys`，再 `append(flat, k, h[k])`）。`cmdHVals` 也用同一手法（排 keys 后按序取值），而 `cmdHKeys` 只排字段。
- `sortedMembers(z, rev)` — 按分数排序；**分数相同时按成员名字典序**。源码注释观点：Redis 对同分成员按字典序，这让顺序确定——**一个依赖"最新条目"的缓存，在两次写入共享同一毫秒时不能拿到随机答案**。这条与 `RedisStore` 的 FIFO 直接相关：`idx` 的 score 是 `CreatedAt` 毫秒，同毫秒的两条写入在 `ZRANGE 0 over-1` 里的相对顺序由字典序固定，因此**淘汰结果可复现**。
- `sliceRange(items, start, stop)` — Redis 的**闭区间**索引与负偏移（从尾部数），`start < 0` 时下钳到 0，`stop >= n` 时上钳，`start > stop || start >= n` 返回 `false`（调用方回空数组/0）。`cmdZRange`、`cmdZRemRangeByRank` 共用。
- `parseScoreBound(s)` — 只做 `strings.TrimPrefix(s, "(")` 后 `ParseFloat`，即支持 `(` 开区间写法。**注意**：它**不处理 `-inf`/`+inf`** 的显式文本，而 `RedisStore.pruneExpired` 恰恰发的是 `ZRANGEBYSCORE expKey "-inf" now`——`strconv.ParseFloat("-inf", 64)` 在 Go 里**是合法的**（返回负无穷并 err=nil），所以这条路径能跑通；但这属于"依赖 `ParseFloat` 的能力"而不是明确实现，见「未能确认」。
- `cmdZRangeByScore` — 无 `LIMIT`/`OFFSET` 支持（源码只认 `WITHSCORES`，其他参数被忽略而不是报错）。`RedisStore` 只用了 `"-inf" now` 这个形状，所以无影响。
- `formatFloat(f)` — `strconv.FormatFloat(f, 'f', -1, 64)`，即不用科学计数法。`cmdZIncrBy`/`cmdZScore`/`WITHSCORES` 都用它。
- `matchGlob(pattern, s)` — 源码注释观点：**只支持 `*` 通配符**，这是诊断用 `KEYS` 需要的全部，也是测试用到的全部。

### 生命周期与统计

- `func New(opts Options) *Server` — `Addr` 空 → `":6399"`（源码注释观点：**默认 6399 而不是 6379，避免影子掉本机真 Redis**）；初始化六张 map 与 `done` channel。
- `func (s *Server) Start() error` — `net.Listen("tcp", Addr)`，失败 → `"mockredis: listen: %w"`；然后 `wg.Add(2)` 起 `serve` 与 `sweepLoop` 两个 goroutine。
- `func (s *Server) Addr() string` — 未监听时返回配置的地址，否则返回 `ln.Addr().String()`；配合 `":0"` 取临时端口（测试都用它）。
- `func (s *Server) Stats() Stats` — 在 `mu` 下复制计数字段，并**现场填** `st.Keys = int64(len(strs)+len(hashes)+len(zsets))`（键数，不是条目数）。源码注释观点（`Stats` 类型上）：这些计数让一条验收检查能证明缓存**确实**跟它说过话——**否则一个空 Redis 和一个坏掉的缓存从外面看一模一样**。
- `func (s *Server) Close() error` — 幂等：`closed` 置位、拷走 `clients`、解锁，然后 `close(s.done)`、**关闭每条客户端连接**、关 listener、`wg.Wait()`。源码注释观点：**关掉所有连接正是测试模拟"Redis 走了"而网关还在跑的方式**——`TestRedisStoreDegradesWhenTheServerGoesAway` 就靠它。
- `Options.Logger` — 设置后每条命令按 `debug` 打一条（源码注释观点：**debug 不是 info，负载下的缓存是消防水龙头**）。
- `Options.RequirePass` — 设置后强制 AUTH，其余命令一律 `"NOAUTH Authentication required."`；错密码 → `"WRONGPASS invalid username-password pair or user is disabled."`；无敌密码时发 AUTH → `"ERR Client sent AUTH, but no password is set"`。`authOK (map[net.Conn]bool)` 是**按连接**的，连接关闭时在 `handle` 的 defer 里删除。

## 44. cmd/miniredis —— 把它变成一个真进程 (cmd/miniredis/main.go)

`main` 的四个 flag（源码注释即文档）：

- `-listen`（默认 `":6399"`）— 注释原文要点：**6399 而不是 6379，这样本机真 Redis 永远不会被影子掉**。
- `-requirepass`（默认空）— 空即不要 AUTH。
- `-max-keys`（默认 `4096`）— "近似键上限；越过后淘汰最近最少使用的键"。
- `-log-level`（默认 `info`）、`-log-format`（默认 `text`）— 交给 `logging.New(os.Stderr, ...)`；坏值 → `fmt.Fprintf(os.Stderr, "miniredis: bad logging options: %v\n", err)` 后 **`os.Exit(2)`**（与下面启动失败的 `os.Exit(1)` 区分）。

流程：`signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)` → `mockredis.New(...)` → `srv.Start()`；失败 → `logger.Error("cannot start", ...)` + **`os.Exit(1)`**。成功后打一条结构化日志（含 `note` 字段：源码注释观点：**启动时就说清楚，好让任何人都不把它当成一个完整的 Redis**），再 `fmt.Printf("miniredis listening on %s\n", srv.Addr())` 往 **stdout** 打一行给脚本抓地址。

然后 `<-ctx.Done()` 阻塞，收到信号后 `logger.Info("shutting down")` → `srv.Close()`（失败只 `logger.Warn("close", ...)`，不影响退出）→ 打一条 `"stopped"` 并带全部 `Stats` 字段。

包注释观点：它存在是因为 **M2 验收路径必须能在一台既没有 Docker daemon 也没有 Redis 服务的机器上跑**，同时仍然演练真实 socket、真实请求/响应协议与真实序列化。它**不是** Redis 的替代品——它只实现缓存 store 会发的命令子集，文档里也这么写；把网关指向现成的 Redis 是配置改动（`cache.redis.addr`），`docker-compose.yml` 会为想要生产依赖进环的人拉起来一个。

**一处值得指出的后果**：`-max-keys` 默认 4096 是**进程级全局键上限**，而 `RedisStore` 的 `MaxEntriesPerScope` 默认 256 是**每 scope**。一个有多租户的验收场景里，先撞到的几乎总是 store 自己的 FIFO 裁剪；只调 `-max-keys` 才能观察到全局 LRU。两个上限的语义不同，日志/统计里分别体现在 `mockredis.Stats.Evictions` 与 `cache.StoreStats.Evicted`。

## 45. 未能确认

1. **`trim` 的注释与实际命令不符**（已在正文标明）：类型级注释写 `ZREMRANGEBYRANK`，实现是 `ZRANGE` + 逐条 `ZREM`；`mockredis` 实现了 `ZREMRANGEBYRANK`，但 cache store 从不发它。无法判断这是注释写到过时、还是实现改过，仓库内没有 git 历史可查（本册未读 git 元数据）。
2. **`MaxScan` 超限记入 `Errors`**：`Search` 在 `len(flat)/2 > MaxScan` 时只做 `bump(Errors++)`，没有专门的"扫描超预算"字段，注释说的"数字被记录下来"在 `/admin/cache` 上表现为 `errors` 变大。无法确认这是有意的（让告警能抓到）还是字段复用的权宜。另注：`RedisOptions.MaxScan` 的注释说"默认无界扫描"，而 `NewRedisStore` 实际给了 4096 的默认值——两处说法不一致。
3. **`RedisStore.trim` 里 `victims.Strings()` 的错误分支没有 `Errors++`**，与同函数其他分支不一致。无法确认是否有意（该错误只可能来自回复类型不符）。
4. **`IsNotFound` 与 `Reply.Ints` 在本仓库内没有调用点**（前者面向会返回 `ERR no such key` 的命令，后者面向分数/计数数组）。属"已声明未接线"，无法确认是预留还是历史遗留。
5. **`mockredis.parseScoreBound` 是否 "支持 `-inf`" 属实现偶然**：它只 TrimPrefix `(` 后交给 `ParseFloat`，而 `ParseFloat("-inf", 64)` 恰好合法。没有测试直接钉住 `ZRANGEBYSCORE -inf <now>` 的形状（该路径由 `RedisStore.pruneExpired` 覆盖，但没找到专门断言 `-inf` 解析的用例）。
6. **`mockredis` 与真实 Redis 的行为差异只确认了代码里能读到的那些**（`QUIT` 不关连接、`SELECT` 无操作、`KEEPTTL` 忽略、`CONFIG`/`COMMAND` 返回空数组、`KEYS` 只支持 `*`、`ZRANGEBYSCORE` 无 `LIMIT`）。没有条件对着真 Redis 做差异比对。
7. **`\|` `HGETALL` 的成对不变量**：`RedisStore.Search` 依赖 `flat` 是"field,value"成对；`mockredis` 保证成对，真实 Redis 也保证成对，但**客户端侧没有任何断言**（`Reply.Strings` 只逐元素转字符串）。若某个回复里出现奇数个元素，`for i := 0; i+1 < len(flat); i += 2` 会静默丢掉最后一个元素而不是报错。属可接受但未文档化的宽松。
8. **未跑 `go test`**：本会话的 Go 工具链需要下载 1.26 toolchain，被沙箱拒绝，因此本册的全部结论来自源码与测试文本的静态阅读，没有执行过 `go test -race`、`go vet` 或任何用例。上文所有"由某用例钉住"的表述都是**对测试源码的阅读结论**，不是实测结果。

## 46. 本册覆盖清单

**本册逐符号覆盖的单元：**

- `internal/cache`（`cache.go`、`memory.go`、`redis.go`，以及 `cache_test.go` 的用例意图）
- `internal/quota`（`quota.go`、`store.go`、`redis.go` 及其三个测试文件）
- `internal/redis`（`redis.go`）
- `internal/mockredis`（`mockredis.go`）
- `internal/idempotency`（`store.go` 及其测试）
- `internal/sessions`（`ledger.go` 及其测试）
- `internal/stats`（`stats.go`）
- `cmd/miniredis`
- `configs/quota-local.yaml`、`configs/quota-redis.yaml` 中与配额相关的配置键

**本册明确没有覆盖的东西：**

- `internal/embed` 与 `internal/evalset`：本册只在引用语义阈值与语料出处时提到，未逐符号走读。
- `internal/gateway` 侧的缓存 / 配额 / 幂等接缝（`cachepath.go`、`quotapath.go`、`idempotency.go`）：属 02C 册，本册只引用签名。
- `internal/router`、`internal/breaker`（属第 03 册）；`internal/server` 的装配与 admin API 面（属第 01 册）。
- `internal/cache/redis.go` 与 `internal/redis/redis.go` 之外的 Redis 协议细节（例如真实 Redis 的差异比对）未经运行验证。
- `cmd/` 下除 `cmd/miniredis` 之外的可执行体与 `deploy/`、`Dockerfile`（属第 06 册与第 07 册）。
- 本会话未跑 `go test`：所有"由某用例钉住"的表述都是对测试源码的静态阅读结论。

# 第 03 册 · 路由与熔断

## 1. 本册范围与三条贯穿全局的不变量

- 本册只走读 [internal/router/router.go](../../internal/router/router.go)、[internal/breaker/breaker.go](../../internal/breaker/breaker.go)、[internal/breaker/group.go](../../internal/breaker/group.go)，以及两个测试文件里最能说明意图的八个用例。锚点统一写作 `符号名 (internal/router/router.go)`，**不含任何行号**。标题里曾经出现过的「账本 · 统计」两部分**不在本册正文内**：会话账本落在第 04 册的一节，随后由第 08 册逐符号补全；统计窗口同理，见第 08 册。
- 不变量一（源码注释的观点，`router` 包首）：router **从不做 I/O、从不改写入参**；它读窗口化健康统计、给候选排序、交回一个计划，由 proxy 执行。选择与执行分离，才让策略可以在没有网络的情况下被测试，也才让 admin API 能在计划被使用之前先展示它。
- 不变量二（源码注释的观点）：**同一份配置 + 同一份统计下路由是确定性的**，唯一例外是显式随机的 weighted。其它策略里加随机 tie-break 会让事故不可复现：同一个请求每次重试都走不同的路径。
- 不变量三（源码注释的观点）：不健康的后端**永远不排第一**；排序决定顺序，熔断器决定资格。注意这条在 weighted 里不是由排序保证的，见第 10 节。

## 2. `router` 的常量面：本文件没有常量块

- **本册范围内 router.go 没有任何 `const` / `var` 声明**：包声明与 import 块之后，第一个声明就是 `type Candidate struct`。所有"常量"都是别包的值。
- 策略判别常量 `config.StrategyPriority` / `config.StrategyWeighted` / `config.StrategyCost` / `config.StrategyLatency` / `config.StrategyScore` / `config.StrategyTiered` (internal/router/router.go) — `Plan` 的 `switch r.cfg.Strategy` 逐一比较它们；`New` 把空字符串归一为 `config.StrategyPriority`，`Plan` 的 `default` 分支同样落到 priority。它们定义在 `internal/config`（不在本册范围，**字面值未能确认**）。
- 层级常量 `config.TierCloud` / `config.TierLocal` (internal/router/router.go) — `tierFor` 返回其中之一，`orderTiered` 与 `tierReason` 比较它们；同样定义在 `internal/config`，**字面值未能确认**。
- import 块 (internal/router/router.go) — 标准库 `fmt`（错误串与 Reason 拼接）、`math/rand`（weighted 用包级 `rand.Float64` / `rand.Intn`）、`sort`（每个策略都用 `sort.SliceStable`）、`strings`（大小写与空白归一）、`time`（`time.Now` 与 `time.Duration`）；内部包 `breaker`、`config`、`stats`、`upstream`。

## 3. 值类型：`Candidate` / `Request` / `PriceFunc`

- `type Candidate struct (internal/router/router.go)` — 一个可能服务该请求的后端，外加要向它索取的模型名。四字段逐条如下。
- `Candidate.Target (internal/router/router.go)` — `*upstream.Target`；后端本身。所有排序都以它为键。
- `Candidate.Model (internal/router/router.go)` — `string`；真正发给上游的模型名。**源码注释的观点**：只要选中的后端不提供调用方点名的那个名字（例如本地单模型机器），它就与请求的模型不同。
- `Candidate.Score (internal/router/router.go)` — `float64`；该策略下的排序值，**越小越好**。**源码注释的观点**：每种策略都会填它，包括不靠它的策略，因为 admin 端点与日志需要一个可比较的数字。实测差别：`orderWeighted` 与 `Plan` 的显式钉住分支都**没有**写 `Score`，那里它保持零值。
- `Candidate.Healthy (internal/router/router.go)` — `bool`；**源码注释的观点**：报告熔断器当前是否放行流量。它由 `Router.healthy` 在构造候选时填一次。
- `Candidate.Reason (internal/router/router.go)` — `string`；**源码注释的观点**：解释这个候选为什么在计划里，写给正在排障的人而不是写给机器。各策略分别用 `reasonFor` 或 `tierReason` 生成。
- `type Request struct (internal/router/router.go)` — router 眼中的入站请求。五个字段逐条如下。
- `Request.Model (internal/router/router.go)` — `string`；调用方要的模型名。
- `Request.Capabilities (internal/router/router.go)` — `[]string`；请求要求的能力标签，交给 `Target.HasCapabilities` 过滤，并在 `tierFor` 里与 `TierPolicy.CloudCapabilities` 比对。
- `Request.Explicit (internal/router/router.go)` — `string`；非空即把请求钉在一个具名后端上。**源码注释的观点**：这是 `X-InferGate-Upstream` 逃生口——运维在生产里用它抽干一个后端，测试用它让路由变得无关。
- `Request.Messages (internal/router/router.go)` — `[]byte`；请求序列化后的 message 数组，保持原始形态是为了让 router 不必解析就能估大小。**源码注释的观点**：只有 tiered 策略读它。
- `Request.MaxTokens (internal/router/router.go)` — `int`；调用方要的 completion 上限，未要求时为 0。同样只有 tiered 读它。
- `type PriceFunc func(model string) (blendedUSD float64, known bool) (internal/router/router.go)` — 报告某模型的单次请求混合成本。**源码注释的观点**：router 只在**同一个请求模型**的候选之间比价，所以一个混合数字够用，也避免在响应还不存在时就假装知道 prompt/completion 的比例；`known=false` 表示价目表里没有这个模型。

## 4. `Router` / `Options` / `New` / `Strategy`

- `type Router struct (internal/router/router.go)` — 选择候选的那台机器。字段逐条如下。
- `Router.registry (internal/router/router.go)` — `*upstream.Registry`；`Plan` 通过 `Target(name)` / `Targets()` 取后端。构造后只读，无锁。
- `Router.breakers (internal/router/router.go)` — `*breaker.Group`；健康与窗口的来源。**源码注释的观点**：nil 表示所有后端永远健康（`healthy` 与 `snapshot` 都显式判 nil）。
- `Router.cfg (internal/router/router.go)` — `config.RoutingConfig`；策略、权重、`FallbackModel`、`TierPolicy` 都从这里读。**源码注释的观点**（由 `TestTieredKeepsTheTierPolicyOnTheSnapshot` 钉住）：它是配置快照，排序期间绝不读活配置。
- `Router.health (internal/router/router.go)` — `config.HealthConfig`；`New` 里由 `Options.Health` 赋入。**本册范围内没有任何读取点**（`OpenDuration`、`MinRequests` 等由 breaker 自己持有），是否在别处被读**未能确认**。
- `Router.price (internal/router/router.go)` — `PriceFunc`；可为 nil，`priceOf` 会显式判 nil 并返回未知价。
- `Router.now (internal/router/router.go)` — `func() time.Time`；`New` 里固定为 `time.Now`。**源码注释的观点**：它可注入，是为了让 half-open 的时序能被确定性地测试。**本册范围内没有读取点**：`latencyOf` 与 `snapshot` 读的是 stats 快照，不走它。
- `Router` 上**刻意没有 rand 字段**（源码注释的观点）：`*rand.Rand` 不能并发使用，而 Router 构造一次后被每个请求 goroutine 调用 `Plan`，过去 weighted 会在并发请求间写共享的生成器状态；改用包级 helper，它们内部取同一把锁。
- `type Options struct (internal/router/router.go)` — `New` 的入参。字段：`Registry *upstream.Registry`、`Breakers *breaker.Group`、`Routing config.RoutingConfig`、`Health config.HealthConfig`、`Price PriceFunc`。
- `Options.Price (internal/router/router.go)` — `PriceFunc`，可选。**源码注释的观点**：没有它时 cost 策略没有可排序的依据，于是回退到 priority 顺序，而不是假装每个后端一样贵。
- `New(opts Options) *Router (internal/router/router.go)` — 构造 Router：逐字段拷贝 Options，`now` 固定为 `time.Now`；若 `r.cfg.Strategy == ""` 则写入 `config.StrategyPriority`。无锁（构造期单 goroutine）。**本册范围内 `New` 不校验 nil**：`breakers` 为 nil 是被支持的，`registry` 为 nil 则 `Plan` 会在 `Targets()` 上崩溃——调用方是否保证非 nil **未能确认**。
- `Strategy (internal/router/router.go)` — `func (r *Router) Strategy() string`；只读 `r.cfg.Strategy`，无锁，无副作用。给 admin/日志报告当前策略。

## 5. `Plan`：显式钉住与完整候选链

- `Plan (internal/router/router.go)` — `func (r *Router) Plan(req Request) ([]Candidate, error)`。**源码注释的观点**：除非设了 `Explicit`，它返回**整支合格舰队、最优在前**，由调用方一路走直到某次尝试成功；返回整条链而不是单一选择，是"故障转移是计划的性质"而非"挂在请求路径上的特例"的原因。
- `Plan` 的显式分支：`r.registry.Target(req.Explicit)` 查不到时错误串原文为 `unknown upstream %q`（`fmt.Errorf`）；查到则**直接返回恰好一个元素的切片**，其中 `Healthy: true` 是**硬编码**的，`Reason` 为 `"pinned by X-InferGate-Upstream"`。
  - 为什么只有一个元素：**运维用这个头主动抽干某个后端时，回退违背他的意图**——计划里再塞备胎，就等于把一次"排水"变成一次隐式故障转移。
  - 这一步**完全不查熔断器**，所以被钉住的候选 `Healthy` 恒为 true、`Score` 恒为 0；真正的资格判定仍然发生在 proxy 调 `Allow()` 时。
- `Plan` 的常规分支：`targets := r.eligible(req)`，空则错误串原文 `no upstream serves model %q`。随后为每个 target 造 `Candidate{Target, Model: r.modelFor(...), Healthy: r.healthy(t)}`，再按 `r.cfg.Strategy` 分派到六个排序函数；`default`（含未知策略）走 `orderByPriority`。两种错误都用 `fmt.Errorf`，`return nil, err`。
- 并发：`Plan` 自身无锁、不写任何 Router 字段；它调用的 `Group.Get` 可能**惰性创建**缺失的 breaker（写组状态，但组内自带锁），`stats.Snapshot` 自带锁。被 `internal/gateway` 的每请求 goroutine 调用。

## 6. 候选链的构造与过滤

- `eligible (internal/router/router.go)` — `func (r *Router) eligible(req Request) []*upstream.Target`；遍历 `r.registry.Targets()`，输出预分配容量 `len(all)`。**源码注释的观点**：第一道过滤是"模型匹配"而不是健康——问一个不提供该模型的后端属于配置错误而非瞬时故障，每请求都重试它会永久浪费一次尝试。
- `eligible` 的两条 `continue`：① 若 `!t.ServesModel(req.Model)`，先看 `r.fallbackFor(t, req.Model)` 是否非空；只有当 fallback 也为空**且** `!(t.IsCatchAll() || len(all) == 1)` 时才剔除。源码注释保留的"为什么"（观点）：配置里的 `fallback_model` 把资格放宽到"服务 fallback 名"的后端；而单后端部署只跑一个量化、agent 框架却发来网关从没见过的别名，M0 把这种情况吸收进了 registry，**router 必须保住它，否则最常见的本地部署在 M1 落地的那一刻就坏掉**。② `!t.HasCapabilities(req.Capabilities)` 直接剔除。
- `healthy (internal/router/router.go)` — `func (r *Router) healthy(t *upstream.Target) bool`；`r.breakers == nil` 时返回 true，否则返回 `r.breakers.Get(t.Name).State() == breaker.StateClosed`。**这就是"路由排序绝不调用 `Allow()`、只读 `State()`"的落点**，源码注释把它写成了一条完整的因果链（观点）：`Allow()` 是一次状态迁移，它还会认领那唯一一个 half-open 探针；在排序时调用它会在 proxy 真正尝试之前把探针吃掉，于是 proxy 自己的 `Allow()` 被拒——**后端就卡在 half-open，没有任何流量能把它关闭**。
- `healthy` 对 half-open 的判定（源码注释的观点）：half-open 在排序意义上算**不健康**——试用期中的上游还没证明自己恢复了，不能排在已知良好的上游前面；但它**仍然留在计划里**（资格由 proxy 决定），只是排到健康候选之后。
- `fallbackFor (internal/router/router.go)` — `func (r *Router) fallbackFor(t *upstream.Target, requested string) string`；`fb := strings.TrimSpace(r.cfg.FallbackModel)`，若 `fb == "" || t.IsCatchAll() || t.ServesModel(requested) || !t.ServesModel(fb)` 返回 `""`，否则返回 `fb`。只读 `r.cfg` 与 target，无锁。**源码注释的观点**：它只对"声明了具体模型名、且两个名字都不服务"的后端生效；catch-all 刻意豁免，因为它本身就是"要什么给什么"，改写它会破坏 catch-all 模式存在的理由；已经服务请求名的后端永不被二次猜测。
- `modelFor (internal/router/router.go)` — `func (r *Router) modelFor(t *upstream.Target, requested string) string`；先问 `fallbackFor`，非空即返回；否则取 `t.ModelPatterns()`，把请求名 `ToLower(TrimSpace(...))` 后在非 `"/"` 的模式里找大小写不敏感的全等匹配，命中就返回**请求名原文**；再否则返回**第一个非 `"/"` 的模式**；都没有就返回请求名。无锁。
  - 源码注释保留的"为什么"（观点）：一个既声明具体模型又声明 catch-all 的后端仍然知道自己的名字，问它一个没听过的名字是本地推理里最常见的 404；`IsCatchAll()` 先判再返回调用方别名，曾把调用方的别名发给了一个只认 `"mock-dear"` 的后端。

## 7. 优先级 / 成本 / 延迟三种排序

- `orderByPriority (internal/router/router.go)` — `func (r *Router) orderByPriority(cands []Candidate) []Candidate`。`sort.SliceStable`，比较键为：`Healthy` 不等时健康者在前，否则 `Target.Priority()` 升序。**源码注释的观点**：配置顺序是并列时的 tie-break，因为它是运维唯一能看见、能从文件预测的东西。收尾写入 `Score = float64(Target.Priority())` 与 `Reason = reasonFor(c, "priority")`。原地排序并返回同一底层数组，无锁。
- `orderByCost (internal/router/router.go)` — `func (r *Router) orderByCost(cands []Candidate, model string) []Candidate`；先为每个候选按 `Target.Name` 建 `map[string]pricedCost`，价用 `r.priceOf(c.Model, model)`（注意**按该候选实际会被索取的模型**比价）。比较键：健康优先 → `known` 者优先 → `usd` 升序 → `Priority()` 升序；**源码注释的观点**：未知价格排在最后而不是当作 0，因为"价目表里缺这个模型"是未知，把它当免费会把每个请求都赶去那个没人定价的后端。收尾 `Score = prices[...].usd`（未知时因此是 0，但排序位置仍最后）与 `Reason = reasonFor(c, "cost")`。
- `orderByLatency (internal/router/router.go)` — `func (r *Router) orderByLatency(cands []Candidate) []Candidate`；键为健康优先 → `r.latencyOf(Target.Name)` 升序 → `Priority()`。收尾 `Score = float64(r.latencyOf(...))`（纳秒数，未测到即为 `1<<62-1`）、`Reason = reasonFor(c, "latency")`。每次调用都会重新问一次窗口（比较函数里就调了 `latencyOf`，因此一次排序会多次读同一快照）。

## 8. `orderByScore`：归一化加权和

- `orderByScore (internal/router/router.go)` — `func (r *Router) orderByScore(cands []Candidate, model string) []Candidate`。**源码注释的观点**：每个项在加权之前都要在候选集内归一到 0..1，因为原始单位互不相容——每百万 token 的美元约 `1e-5`，而延迟约 `1e-2` 秒；不归一化，成本项在数值上无论运维写多大权重都不可见。
- 权重处理：`w := r.cfg.Weights`（局部拷贝，不改配置）；`total := w.Cost + w.Latency + w.Reliability + w.Priority`，`total <= 0` 时取 1，然后把四项**各自除以 total**。**源码注释的观点**：这样运维写"cost: 3, latency: 1"不必凑成 1，抬高一个权重是降低其它项的影响，而不是把所有分数推出 0..1。
- 逐候选取三项原料：`r.priceOf(c.Model, model)` 得 `costs/knownCost`；`r.snapshot(c.Target.Name)` 非 nil 且 `HasLatency` 时 `latencies[i] = float64(snap.LatencyMean.Nanoseconds())`、`knownLat[i] = true`；`snap != nil` 时 `failures[i] = snap.FailureRatio`。
- 归一化：`normCost := normalise(costs, knownCost, len(cands))`、`normLat := normalise(latencies, knownLat, len(cands))`。**源码注释的观点**：reliability 本来就是 0..1 的比例，刻意不归一化、保留绝对含义——一个失败率 60% 的后端即使**是唯一在失败的后端**也必须得分很差。
- 打分：`Score = w.Cost*normCost[i] + w.Latency*normLat[i] + w.Reliability*failures[i] + w.Priority*prio`，其中 `prio = float64(cands[i].Target.Priority())`。**注意**：源码注释说"每个项都归一到 0..1"，但 `Priority` 项直接用原始 priority 数值，没有经过 `normalise`——是否为有意为之**未能确认**。
- 排序键：健康优先 → `Score` 升序 → `Priority()` 升序；最后写 `Reason = reasonFor(c, "score")`。

## 9. `orderTiered` 与 tiered 的辅助符号

- `orderTiered (internal/router/router.go)` — `func (r *Router) orderTiered(cands []Candidate, req Request) []Candidate`；`preferred := r.tierFor(req)`，排序键为健康优先 → `tierRank(Target.Tier(), preferred)` 升序 → `Priority()` 升序；收尾 `Score = float64(tierRank(...))`、`Reason = tierReason(c, preferred)`。
  - 源码注释保留的两条"为什么"（观点）：① **两个层级都不会从计划里被删掉**——硬请求把本地后端留在云端之后，因为"打不通的 provider 仍不如本地盒子"，而简单请求把云后端留作本地故障时的兜底。② **健康压过 tier**：这是该策略唯一向包级不变量让位的地方，否则一个跳闸的本地熔断器会把每个简单请求钉在一个已知在失败的后端上，而一台完好的云后端闲着。
- `tierFor (internal/router/router.go)` — `func (r *Router) tierFor(req Request) string`。判定顺序（前一条命中即返回 `config.TierCloud`）：① `req.Capabilities` 中任一被 `containsFold(tp.CloudCapabilities, need)` 命中；② `tp.LocalMaxPromptTokens > 0 && estimatePromptTokens(req.Messages) > tp.LocalMaxPromptTokens`；③ `tp.LocalMaxCompletionTokens > 0 && req.MaxTokens > tp.LocalMaxCompletionTokens`。三条都不中则返回 `config.TierLocal`。
  - **两个长度阈值都是严格大于**：压线算简单档——恰好等于运维写下的上限，是他已经说过"本地可以服务"的请求（`TestTieredClassificationOrdersTiers` 用 400 字节 / 限 100 与 404 字节 / 限 100 两种输入钉住这一点）。
  - **上限 0 表示禁用该规则**：`LocalMaxPromptTokens` / `LocalMaxCompletionTokens` 为 0 时对应规则不参与，于是未配置的策略不会把整支舰队推向付费档；0 只禁用长度规则，**不影响**能力规则。
  - 源码注释的观点：hard 的定义就是"小模型很可能做不好的事"——云层才有（假定）的能力、比本地层允许问的更大的 prompt、比本地层允许产出的更长的回答；其余都是 simple，这正是该策略存在的理由：便宜的那层应当承担大部分流量。
- `estimatePromptTokens (internal/router/router.go)` — `func estimatePromptTokens(messages []byte) int`，返回 `len(messages) / 4`。**源码注释的观点**：它是估计而非分词器，因为它只决定请求落在运维的圆整阈值哪一侧；差几个百分点只对正好压线的 prompt 有影响，而那种 prompt 两层都能服务。真分词器意味着在路由路径上为每个请求携带词表并运行，那是粗粒度分层决策无法为之辩护的依赖与每请求成本；四字符一 token 也正是配额估算器用的比例（config 的 `EstimateCharsPerToken`），好让"长 prompt"对两个决策意味着同一件事。
- `tierRank (internal/router/router.go)` — `func tierRank(tier, preferred string) int`；相等返回 0，否则返回 1。**源码注释的观点**：这是"候选层级"的排序键，首选层无论是什么都排第一。
- `containsFold (internal/router/router.go)` — `func containsFold(list []string, want string) bool`；两边都 `ToLower(TrimSpace(...))` 后全等比较。**源码注释的观点**：能力标签在所有地方都这样匹配（大小写与首尾空白不敏感）。无锁、纯函数。
- `tierReason (internal/router/router.go)` — `func tierReason(c Candidate, preferred string) string`。四类输出原文：不健康时 `strategy=tiered tier=%s reason=circuit open, only used as a last resort`；`preferred == config.TierCloud` 时 `kind` 为 `"hard"`，否则 `"simple"`；`tier == preferred` 时 `strategy=tiered tier=%s reason=%s request prefers this tier`；否则 `strategy=tiered tier=%s reason=%s request falls back to this tier`。`%s` 的 tier 取 `c.Target.Tier()` 本身（不是 preferred）。**源码注释的观点**：它把分层决定渲染成与其它策略同形的单行文本，并同时点出候选自己的层级与分类，所以一行日志就够看出为什么选了这个后端，不必回读配置。

## 10. `normalise` / `orderWeighted` / `reasonFor`

- `normalise (internal/router/router.go)` — `func normalise(values []float64, known []bool, _ int) []float64`；**第三个形参是匿名占位 `_ int`**（调用方传的是 `len(cands)`，函数体不使用它）。只在 `known[i]` 为真的样本上求 `min`/`max`（用 `first` 标志处理初值），`span := max - min` 后逐项：未知 → `0.5`；`span <= 0` → `0`；否则 `(values[i]-min)/span`。源码注释保留的"为什么"（观点）：未测量值取**中性中点**——当 0 会让未测量的后端看起来完美并饿死已测量的，当 1 又会在它还没服务过一个请求之前就把它移出轮转；全体相等时贡献 0 而不是武断地给切片第一个 1。
- `orderWeighted (internal/router/router.go)` — `func (r *Router) orderWeighted(cands []Candidate) []Candidate`；唯一带随机性的策略，实现是**无放回按 `Target.Weight()` 采样**。循环：把 `cands` 拷进 `remaining`，每轮先求 `total` 为剩余权重之和；`total > 0` 时 `roll := rand.Float64() * total`，累加权重直到 `roll < acc` 选中 `pick`；`total == 0` 时退化为 `rand.Intn(len(remaining))` 均匀取一个。选中项 append 进 `out`，再以 `remaining = append(remaining[:pick], remaining[pick+1:]...)` 从池中摘除，直到池空。最后只写 `Reason = reasonFor(out[i], "weighted")`。
  - **不变量**：每个后端在输出里**恰好出现一次**（无放回），所以 90/10 的权重划分仍然把那个 10% 的后端留作兜底；**源码注释的观点**：无放回而不是各自独立抽样，正是它成其为可用的 failover 链的原因。
  - **它只看 `Weight`，完全不看 `Healthy`**：比较过程不读 `Candidate.Healthy`，因此一个 breaker 处于 open 的后端在 weighted 下**可以排第一**——包级的"不健康永不排第一"在这条路径上由 proxy 执行时的 `Allow()` 兜住，而不是由计划保证。此时 `reasonFor` 仍会写出 `weighted: circuit open, only used as a last resort`，而该候选就在队首；这是否为有意设计**未能确认**。
  - 并发（源码注释的观点）：它用的是包级 `rand.Float64` / `rand.Intn`，它们内部取同一把锁、可以从任何地方安全调用；Router 上刻意没有 `*rand.Rand` 字段。
- `reasonFor (internal/router/router.go)` — `func reasonFor(c Candidate, strategy string) string`；不健康时返回 `%s: circuit open, only used as a last resort`；`c.Target.Priority() == 0` 时只返回策略名本身；否则 `%s (priority %d)`。被除 tiered 之外的五个策略调用（tiered 用自己的 `tierReason`）。

## 11. 读窗口的辅助方法

- `snapshot (internal/router/router.go)` — `func (r *Router) snapshot(name string) *stats.Snapshot`；`r.breakers == nil` 时返回 nil，否则 `s := r.breakers.Get(name).Stats().Snapshot()` 并返回 `&s`。**源码注释的观点**：nil 只表示"没有提供 breaker 组"。它只读窗口，不迁移任何熔断状态——与 `healthy` 同一条纪律。注意 `Group.Get` 可能惰性创建 breaker。
- `latencyOf (internal/router/router.go)` — `func (r *Router) latencyOf(name string) time.Duration`；快照非 nil 且 `HasLatency` 时返回 `s.LatencyMean`，否则返回 `time.Duration(1<<62 - 1)`。**源码注释的观点**：返回一个能"把未测量的后端排到最后"的值；这个哨兵远大于任何真实延迟，且在加权和中也不会溢出。
- `type pricedCost struct (internal/router/router.go)` — 未导出的小结构，字段 `usd float64` 与 `known bool`；用于把"价格是多少"和"价目表里到底有没有它"绑在一起传递。
- `priceOf (internal/router/router.go)` — `func (r *Router) priceOf(candidateModel, requestedModel string) pricedCost`；`r.price == nil` 时返回零值（`known=false`）；否则先问 `candidateModel`，命中即返回；未命中且 `candidateModel != requestedModel` 时再问 `requestedModel`；都没有则返回零值。**源码注释的观点**：当后端以另一个名字服务调用方的模型时，回退到调用方的模型名比价。只读、无锁。

## 12. breaker：三态与 `Breaker` 的字段

- 包首注释（源码注释的观点，internal/breaker/breaker.go）：状态机是经典三态——`closed` 正常放行，窗口失败率在样本足够时达到阈值即跳闸；`open` 完全跳过该上游，`OpenDuration` 过后在下一次请求上转入 half-open；`half-open` 同一时刻只放**一个**探针，连续成功若干次即闭合，任何一次失败立即重开。两条值得记下的决策：① breaker 看的是**窗口**统计，绝不是生命周期计数器——一次性的比例无法恢复，"昨天失败了一千次"的后端会永远"基本失败"，基于它的熔断器要么再也合不上、要么只能人工重置；② half-open 一次只放一个探针，而不是每个等待中的 goroutine 各放一个——否则 `OpenDuration` 之后的第一批请求会重新制造熔断器本身要挡下的那次踩踏。
- `type State string (internal/breaker/breaker.go)` — 熔断器当前状态，底层是字符串。
- `StateClosed (internal/breaker/breaker.go)` — `State = "closed"`；**源码注释的观点**：放行流量并统计结果。
- `StateOpen (internal/breaker/breaker.go)` — `State = "open"`；**源码注释的观点**：不接触上游就拒流。
- `StateHalfOpen (internal/breaker/breaker.go)` — `State = "half-open"`；**源码注释的观点**：只放行单个探针。
- `type Breaker struct (internal/breaker/breaker.go)` — 跟踪一个上游的健康状况，**并发安全**。字段逐条如下。
- `Breaker.name (internal/breaker/breaker.go)` — `string`；上游名，出现在 `Report.Name` 里。
- `Breaker.cfg (internal/breaker/breaker.go)` — `config.HealthConfig`；只读取 `OpenDuration`、`HalfOpenProbes`、`MinRequests`、`FailureRatio`（`Window`/`Buckets` 只在 `New` 造默认窗口时用一次）。
- `Breaker.mu (internal/breaker/breaker.go)` — `sync.Mutex`；保护下面除 `stats` 之外的全部可变字段。`State`/`Allow`/`RecordX`/`Reset`/`Report`/`SetClock` 都持有它；`Stats()` **不**加锁（字段构造后不变）。
- `Breaker.stats (internal/breaker/breaker.go)` — `*stats.Stats`；**共享窗口**。源码注释的观点：breaker 读它来跳闸与闭合，router 读**同一个**窗口来排序，一个窗口意味着 router 与 breaker 永远不会对某上游的状况有分歧。**"读"是关键：本文件不对它写任何一次成功/失败记录**（`Reset` 是唯一例外）。
- `Breaker.state (internal/breaker/breaker.go)` — `State`；当前状态，初值 `StateClosed`。
- `Breaker.openedAt (internal/breaker/breaker.go)` — `time.Time`；最近一次跳闸的时刻。**源码注释的观点**：转 half-open 是基于时间的，所以不需要后台定时器——每个上游一个定时器等于为没有信息增益的东西付出一个 goroutine 与一次唤醒。
- `Breaker.probeInFlight (internal/breaker/breaker.go)` — `bool`；**源码注释的观点**：它把 half-open 状态守住，防止放进一个以上并发探针。
- `Breaker.halfOpenSuccesses (internal/breaker/breaker.go)` — `int`；探针开始以来的连续成功数，与 `cfg.HalfOpenProbes` 比较。
- `Breaker.now (internal/breaker/breaker.go)` — `func() time.Time`；`New` 里为 `time.Now`，`SetClock` 可替换。只在持锁时读写。
- `Breaker.trips (internal/breaker/breaker.go)` — `int64`；生命周期跳闸次数。**源码注释的观点**：它与 `rejections` 和窗口分开放，正是因为它们**必须不衰减**——运维问"这个熔断器到底抖过没有"要的是总数。
- `Breaker.rejections (internal/breaker/breaker.go)` — `int64`；生命周期拒流次数。
- `New (internal/breaker/breaker.go)` — `func New(name string, cfg config.HealthConfig, window *stats.Stats) *Breaker`；`window == nil` 时自建 `stats.New(cfg.Window.Duration(), cfg.Buckets)`——这条 nil 兜底正是 `Group.Get` 惰性创建未知上游时走的路径。返回的 breaker 初态 `StateClosed`、`now = time.Now`。无锁。
- `SetClock (internal/breaker/breaker.go)` — `func (b *Breaker) SetClock(now func() time.Time)`；持 `b.mu` 后替换 `b.now`。**源码注释的观点**：仅用于测试替换时间源。
- `Stats (internal/breaker/breaker.go)` — `func (b *Breaker) Stats() *stats.Stats`；直接返回 `b.stats`，**不加锁**。**源码注释的观点**：把共享窗口暴露出来，好让 router 与 admin 端点读到与 breaker 跳闸依据相同的数字。被 `Router.snapshot`、`Group.Stats` 调用。

## 13. breaker：`Allow` 与结果回报

- `type Decision struct (internal/breaker/breaker.go)` — "我现在可以用这个上游吗"的答案。字段逐条：`Allowed bool`（**源码注释的观点**：open 时为 false，half-open 且已有探针在飞时也为 false）；`State State`（做出该决定时所处的状态）；`Probe bool`（**源码注释的观点**：本次调用是**作为** half-open 探针被放行的，所以调用方即使因无关原因放弃这次尝试，也必须用 `RecordSuccess`/`RecordFailure` 回报结果，否则 breaker 永远卡在 half-open）；`Reason string`（给日志与 admin API 的短解释）。
- `Allow (internal/breaker/breaker.go)` — `func (b *Breaker) Allow() Decision`；**持 `b.mu`**，`now := b.now()`。三条分支与 `Reason` 原文：
  - `StateOpen` 且 `now.Sub(b.openedAt) < b.cfg.OpenDuration.Duration()`：`b.rejections++`，返回 `Decision{State: StateOpen, Reason: "circuit open"}`（`Allowed` 为零值 false）。
  - `StateOpen` 且冷却已过：**降级为 half-open**——`state = StateHalfOpen`、`probeInFlight = false`、`halfOpenSuccesses = 0`，然后落入下一条。
  - `StateHalfOpen` 且 `probeInFlight`：`b.rejections++`，返回 `Decision{State: StateHalfOpen, Reason: "half-open probe already in flight"}`——**这就是"backends 永久卡死"那个失败模式的可见症状**。
  - `StateHalfOpen` 且无探针在飞：`probeInFlight = true`，返回 `Decision{Allowed: true, State: StateHalfOpen, Probe: true, Reason: "half-open probe"}`。
  - 其余（closed）：返回 `Decision{Allowed: true, State: StateClosed, Reason: "closed"}`，不加计数。
- `State (internal/breaker/breaker.go)` — `func (b *Breaker) State() State`；持锁读 `b.state` 原样返回，**不做基于时间的迁移**——`OpenDuration` 过去之后它仍然报 `open`，直到下一次 `Allow()` 才转 half-open（`TestStateDoesNotConsumeTheProbe` 钉住）。源码注释保留的"为什么"（观点）：它存在就是为了让**只需要排序**的调用方读取健康而不吃掉 half-open 探针；`Allow` 是状态迁移而不是查询，为一个请求调它两次（一次排序、一次放行）会让第二次拒掉第一次刚放行的探针，并把 breaker 永久卡在 half-open。被 `Router.healthy` 调用。
- `RecordSuccess (internal/breaker/breaker.go)` — `func (b *Breaker) RecordSuccess()`；持 `b.mu`。先把 `probeInFlight = false`；若状态是 half-open 则 `halfOpenSuccesses++`，达到 `b.cfg.HalfOpenProbes` 时置 `StateClosed`、`halfOpenSuccesses = 0`。**它刻意不写窗口**（源码注释的观点）：真实延迟由 proxy 写进共享的 `stats.Stats`，breaker 只读；如果 breaker 也记一次成功，它就需要一个自己并不拥有的延迟值，而编造一个（0）会把路由对**那个**后端的延迟估计拉向零。源码注释还明确：闭合时**不清空窗口**——清空会抹掉"上游刚刚恢复"的证据，也会抹掉迫使跳闸的那些失败，而 admin 端点正被期望展示这两者；窗口会自己过期。
- `RecordFailure (internal/breaker/breaker.go)` — `func (b *Breaker) RecordFailure(timeout bool)`；持 `b.mu`。先 `probeInFlight = false`，再分派：half-open → `b.trip()` 后 return（源码注释的观点：失败的探针**立即**重开，等失败率越过阈值需要足够流量产生那个比例，而 half-open 按设计没有流量）；open → 直接 return；closed → 取 `snap := b.stats.Snapshot()`，`snap.Attempts < int64(b.cfg.MinRequests)` 就 return，`snap.FailureRatio >= b.cfg.FailureRatio` 才 `b.trip()`。**没有错误串、没有返回值。**
  - **形参 `timeout` 在本册范围内完全没有被使用**（函数体里不出现该标识符）。源码注释说它"为可观测性区分慢上游与打不通的上游"，这条区分在本文件里未见落点——超时的记账显然必须发生在别处（很可能由 proxy 写进同一个窗口），**具体在哪未能确认**。
  - **这解释了测试为什么要成对调用**：`window.RecordFailure(false)` 与 `b.RecordFailure(false)` 各一次。前者是唯一让 `FailureRatio`/`Attempts` 动起来的东西，后者是唯一让状态机评估它们的东西；只调 `b.RecordFailure` 永远达不到 `MinRequests`，只调 `window.RecordFailure` 永远不跳闸。
- `ReleaseProbe (internal/breaker/breaker.go)` — `func (b *Breaker) ReleaseProbe()`；持 `b.mu`，只把 `b.probeInFlight = false`。源码注释保留的完整"为什么"（观点）：half-open 的 breaker 只放行一次尝试并等一个答复，这样它既不会被踩踏、也不会永远半开；当一次尝试结束时**对后端无话可说**——调用方在生成中途挂断，或请求根本没能构造出来——这两个目标就冲突了。此时记一次失败等于用一个健康后端的信用替客户端的行为买单；什么都不记则会让那唯一的探针被永久占用，之后每个调用方都被 `"half-open probe already in flight"` 拒绝，后端再也得不到机会。**交回槽位保住了第二个保证，又不做第一个主张**（`TestReleaseProbeGivesTheHalfOpenSlotBack` 同时钉住这两半）。
- `trip (internal/breaker/breaker.go)` — `func (b *Breaker) trip()`；**调用方必须持有 `b.mu`**。若 `state != StateOpen` 则 `b.trips++`（所以重复跳闸只计一次，而"跳闸 → 探针失败 → 再跳闸"计两次）；随后 `state = StateOpen`、`openedAt = b.now()`、`halfOpenSuccesses = 0`、`probeInFlight = false`。
- `Reset (internal/breaker/breaker.go)` — `func (b *Breaker) Reset()`；持 `b.mu`，置 `StateClosed`、`openedAt = time.Time{}`、`probeInFlight = false`、`halfOpenSuccesses = 0`，并调 `b.stats.Reset()`——**这是本文件唯一一处写共享窗口的地方**。源码注释的观点：它存在是为 admin API；运维修好后端之后，等旧窗口自然过期毫无意义，因为那个窗口描述的是已经不存在的那份坏配置。被 `Group.ResetAll` 调用。

## 14. breaker：`Report`

- `type Report struct (internal/breaker/breaker.go)` — breaker 与其窗口的一致性读快照，字段与 JSON 标签逐条：`Name string` `json:"name"`；`State State` `json:"state"`；`Trips int64` `json:"trips"`；`Rejects int64` `json:"rejections"`（注意字段名与标签不同）；`Attempts int64` `json:"attempts"`；`Failures int64` `json:"failures"`；`Timeouts int64` `json:"timeouts"`；`FailureRatio float64` `json:"failure_ratio"`；`LatencyMeanMS float64` `json:"latency_mean_ms"`；`TTFBMeanMS float64` `json:"ttft_mean_ms"`；`HasLatency bool` `json:"has_latency"`；`OpenedForMS float64` `json:"opened_for_ms,omitempty"`。`Stats` 那组是**源码注释的观点**：它是 breaker 正在据以行动的那幅窗口图景。
- `Report (internal/breaker/breaker.go)` — `func (b *Breaker) Report() Report`；**先持 `b.mu`** 取 `b.stats.Snapshot()`、`state`、`openedAt`、`trips`、`rejections`、`now`，**然后解锁**再做全部换算，因此格式化不占锁。换算规则：`LatencyMeanMS` 只在 `snap.HasLatency` 时由 `float64(snap.LatencyMean) / float64(time.Millisecond)` 得出；`TTFBMeanMS` 只在 `snap.TTFBMean > 0` 时换算（两个门控不同：前者问"有没有测到"，后者问"值是否为正"）；`OpenedForMS` 只在 `state == StateOpen && !openedAt.IsZero()` 时由 `now.Sub(openedAt)` 得出——所以 `omitempty` 的省略同时意味着"没开着"或"从未开过"。被 `Group.Reports` 与 admin 端点调用。注意 `Rejects` 直接取 `b.rejections`。

## 15. `Group`：组与并发安全

- `type Group struct (internal/breaker/group.go)` — 每个上游**一起**创建一个 breaker 和一个共享窗口。源码注释的观点：让 router、breaker、admin 端点永远读同一组数字；把两者放进同一个结构还消除了那个经典缺陷——重载配置时建了新的 breaker，却让 router 继续读旧窗口。字段逐条：`cfg config.HealthConfig`（构造后只读）；`mu sync.RWMutex`；`breakers map[string]*Breaker`；`order []string`（创建顺序，`Names` 用）。
- `NewGroup (internal/breaker/group.go)` — `func NewGroup(cfg config.HealthConfig, names []string) *Group`；map 预分配容量 `len(names)`；对每个 name 先 `stats.New(cfg.Window.Duration(), cfg.Buckets)` 建**独立窗口**，再 `New(name, cfg, window)`，并 append 到 `order`。无锁（构造期）。`names` 里有重复项时会产出重复的 `order` 条目——registry 是否保证唯一**未能确认**。
- `Get (internal/breaker/group.go)` — `func (g *Group) Get(name string) *Breaker`；先 `RLock` 快路径命中即返回；未命中则升级为 `Lock`、**双检**、仍无则 `New(name, g.cfg, nil)`（nil 窗口让 `New` 自建一个）并 append 到 `order`。**源码注释的观点**：它绝不返回 nil——未知名字得到一个新的 closed breaker，因为一个缺失的 breaker 不能让请求路径 panic，也不能静默拒流。被 `Router.healthy`、`Router.snapshot`、`Group.Stats`、`Group.Reports`、`Group.ResetAll` 调用。注意它是**会写组状态**的读方法（惰性创建 + 扩容 `order`）。
- `Stats (internal/breaker/group.go)` — `func (g *Group) Stats(name string) *stats.Stats`；一行转调 `g.Get(name).Stats()`，同样会把未知上游物化出来。
- `Names (internal/breaker/group.go)` — `func (g *Group) Names() []string`；持 `RLock` 拷贝一份 `order` 后返回，调用方拿到的是快照、可安全改动。**源码注释的观点**：按创建顺序列出上游。
- `Reports (internal/breaker/group.go)` — `func (g *Group) Reports() []Report`；先在 `RLock` 下把 breaker 名收集进切片并解锁，再 `sort.Strings(names)` 排序（源码注释的观点：为了稳定输出），最后逐个 `g.Get(name).Report()`。快照语义：名字在收集与取值之间新增的 breaker 不会出现在本次结果里；而且 `Get` 本身还要再次加锁，所以这不是一次原子快照。
- `ResetAll (internal/breaker/group.go)` — `func (g *Group) ResetAll()`；遍历 `g.Names()`（自己的快照），对每个 `g.Get(name).Reset()`。源码注释的观点：存在是为 admin API——运维修好后端之后，等旧窗口过期毫无意义，那个窗口描述的是已经坏掉的那份配置。

## 16. 测试锚点：八个不变量

- `TestStateDoesNotConsumeTheProbe (internal/breaker/breaker_test.go)` — 钉住本册最重的那条修复：时间本身不移动状态（冷却已过，`State()` 仍报 `open`），随后的 `Allow()` 仍能拿到探针。它在注释里点名了那个真实缺陷——router 曾经用 `Allow()` 排序，吃掉了唯一的 half-open 探针，把 breaker 永久卡死。
- `TestReleaseProbeGivesTheHalfOpenSlotBack (internal/breaker/breaker_test.go)` — 钉住"无裁决地结束"这一整类：`ReleaseProbe` 之后下一次 `Allow()` 必须拿到**新的探针**（否则该后端"在进程余生被拒绝"），状态仍是 half-open（交回槽位不对后端做任何主张），且窗口原封不动（`Attempts == 4 && Failures == 4`——交回不是裁决）。同一组用例还覆盖单槽位的反踩踏面：冷却后的第一次调用是被放行的探针，第二次并发调用被拒；而**失败**的探针立即重开，`Report().Trips` 因此记到 2（原本那次跳闸 + 探针失败那次），因为"等失败率再越阈值"在 half-open 下不可能发生。
- `TestBelowMinRequestsNeverTrips` 与 `TestTripsOnFailureRatio (internal/breaker/breaker_test.go)` — 一对边界：3 次失败（样本数 < `MinRequests=4`）绝不跳闸；4 次失败（比值 1.0 ≥ 0.5）跳闸，且这次拒流同时让 `Report().Rejects == 1`、`Trips == 1`，并断言拒流**必须携带非空 `Reason`** 供日志使用。（两条合为一条讲，因为它们钉的是同一条门槛的两侧。）
- `TestUnhealthyBackendSortsLastButSurvives (internal/router/router_test.go)` — 钉住本册的判断之二：把 `preferred` 的 breaker 打到 open 之后，计划仍然是 `"backup", "preferred"`（open 的那个**留在链里、排在最后**，且 `Healthy == false`）。它把理由写进了注释：删掉它就等于把"所有 breaker 都 open"退化成"没有配置上游"，那比"去试那个慢的"更糟。
- `TestExplicitPinWinsAndIsAlone (internal/router/router_test.go)` — 钉住显式钉住链**只含一个元素**（`Explicit: "b"` 得到 `["b"]`）、被钉住的候选 `Reason` 非空（给日志），以及未知的 `Explicit` 必须报错而不是静默回退。
- `TestTieredClassificationOrdersTiers (internal/router/router_test.go)` — 一张表把 `tierFor` 的每条规则与每个边界都钉住：本地层承载小请求；云能力（含 `"  TOOLS "` 这种大小写/空白变体）把请求推向云；**非**云能力不提升层级；404 字节（超 100 一 token）判 hard、400 字节（正好压线）仍判 simple；两个上限都为 0 时两条长度规则都禁用；0 不禁用能力规则；一个 0 不影响另一条。测试刻意给云后端更好的 priority 数字，好让"层级是主键"这件事一旦消失测试就会失败。
- `TestTieredUnhealthyLocalLosesToHealthyCloud (internal/router/router_test.go)` — 钉住"健康压过 tier"：首选本地层的请求在本地 breaker 跳闸后必须排到健康的云后端之后。它同时也是第 12 节那条因果的见证——注释原文写着 breaker 从窗口里读出决定、把窗口留给 proxy，所以测试必须**两边都填**（`window.RecordFailure(false)` 与 `group.Get("box").RecordFailure(false)` 成对调用）。

## 17. 未能确认

- `config.Strategy*`、`config.TierCloud` / `config.TierLocal` 的字面值、以及 `config.RoutingConfig` / `HealthConfig` / `TierPolicyConfig` 的字段全集：定义在 `internal/config`，不在本册四个文件内。
- `Router.health` 与 `Router.now` 在本册范围内没有读取点；它们是否被本册之外的方法（例如未来加入的、按 half-open 冷却时间排序的逻辑）使用，未能确认。
- `RecordFailure` 的 `timeout` 形参在本文件里未被使用，超时观测的落点未能确认。
- `orderByScore` 里 `Priority` 项未归一化，是否符合源码注释对"每一项都归一化"的描述，未能确认。
- `orderWeighted` 不读 `Healthy`，因而可能把 open 的后端排在第一、并给它写一条 "circuit open, only used as a last resort" 的 Reason；是否有意，未能确认。
- `Group.Get` 的惰性创建意味着 `Router.healthy` / `Router.snapshot` 会给未配置的后端物化出 breaker 与窗口；这条路径是否只在测试里被走到，未能确认。

## 18. 本册覆盖清单

**本册逐符号覆盖的单元：**

- `internal/router`（`router.go`）
- `internal/breaker`（`breaker.go`、`group.go`）
- 测试锚点：本册只走读 `internal/router/router_test.go` 与 `internal/breaker/breaker_test.go` 里最能说明意图的那些用例，不逐条列举两个测试文件的全部用例。

**本册明确没有覆盖的东西：**

- `internal/sessions` 的账本实现与 `internal/stats` 的窗口实现：本册只按引用说明它们与 router / breaker 的共享关系，逐符号走读归第 04 册。
- `internal/evalset` 与 `cmd/measure-m2`：本册不覆盖评测集与阈值扫描。
- `internal/config` 的 `Strategy*`、`TierCloud` / `TierLocal`、`RoutingConfig` / `HealthConfig` / `TierPolicyConfig` 字段定义：本册只引用名字。
- `internal/gateway`、`internal/cache`、`internal/quota`、`internal/idempotency`（属 02A/02B/02C/04 册）。
- `cmd/` 下的一切可执行体与 `deploy/`、`Dockerfile`（属第 06 册与第 07 册）。
- 本会话未跑 `go test`：所有"由某用例钉住"的表述都是对测试源码的静态阅读结论。

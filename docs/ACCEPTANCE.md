# InferGate 验收口径

每条能力“以什么为证据”的清单：左边是要证明的事，右边是断言、单测名或原始数据文件。
本页顶部是七个里程碑的门禁总表；M0–M3 的逐项明细在下方，
M4–M6 的验收与实测段落与各自的启动方式写在一起，见 [USAGE.md](USAGE.md) 的 3.13 / 3.14 / 3.15。

## 门禁总表

| 里程碑 | Go 进程内（`cmd/verify*`） | 真实进程 + curl（`scripts/verify-m*.ps1`） | 原始数据 |
| --- | --- | --- | --- |
| M0 透传与 SSE | 38 | 47 | `baseline/m0-baseline.json` |
| M1 路由与熔断 | 64 | 56 | `baseline/m1-summary.json`（+ 9 条压测逐轮文件） |
| M2 语义缓存 | 103 | 157 | `baseline/m2-summary.json`（+ 语料、阈值扫描、6 条压测逐轮文件） |
| M3 配额治理 | 470 | 323 | `baseline/m3-summary.json` |
| M4 分层与量化 | 877 | 125 | `baseline/m4-summary.json` |
| M5 可观测与压测 | 420 | 211 | `baseline/m5-summary.json`（+ 24 条逐轮文件） |
| M6 幂等/账本/能力 | 381 | 149 | `baseline/m6-summary.json` |
| **合计** | **2353** | **1060** | |
| 管理面鉴权（`access`，不属任何里程碑） | 41 | 44 | 无（证据是 `scripts/verify-hardening.ps1` 的输出） |

M5 的 curl 门从 203 条变成 211 条，加的是 8 条解析器自检（`10.0a`–`10.0h`）：
下面那五条只把 p50/p90/p95/p99/max 互相比较，而**一个恒定值满足其中每一条不等式**。
`verify-m5.ps1` 里原来有一个字面 U+00B5（微秒单位）写在 switch 标签上，
Windows PowerShell 5.1 读无 BOM 的 `.ps1` 时用机器代码页解码，那个标签在本机解成了别的字符，
于是 `42ms` 全部落到 `return -1.0`——五个值都是 -1，五条不等式恒真，
门看起来是绿的却在解析它本该解析的东西之外。现在解析器在比较之前先对已知输入自证。
`baseline/m5-summary.json` 仍是 203 条时跑出来的，所以这里出现过一个数字，
上面这张表里是另一个；两者都能对账，因为它们指的是不同的运行。

管理面鉴权默认关闭，所以 M0–M6 的两条证据链一行都没有覆盖它。它有自己的第三条链：
`scripts/verify-hardening.ps1`，**真进程 + 真 curl + 44 条断言**，见下方「管理面鉴权的证据边界」。
它的数字单独列成一行而不是并进合计——把一条 2026 年才加的安全门混进里程碑总数，
会让"1060"这个从 M0 起就写在 README 里的数字变得不可对账。

两条路径相互独立：Go 门用进程内假上游跑得快、断言密度高；
curl 门编译真二进制、拉真进程、用真 `curl.exe` 打真 socket，证的是“部署起来就是这样”。
两条都绿才算这个里程碑完成。复现命令见 [USAGE.md](USAGE.md) 的 3.4。

### 自动化的那一层（`.github/workflows/ci.yml`）

上表两条路径都是**本机手工跑**的；仓库里自动跑的只有一层，边界写清楚：

| 步骤 | 跑什么 | 状态 |
| --- | --- | --- |
| `go build ./...` | 全仓库编译 | 绿 |
| `go vet ./...` | 静态检查 | 绿 |
| `gofmt -l ./cmd ./internal` | 格式门 | 绿（加入这一步时仓库里有 7 个文件不干净，已一并修好） |
| `go test ./... -count=1 -timeout 20m` | 全部单元/集成测试（Linux，无 `-race`） | 绿 |
| `go test -race` **逐个包**（`go list ./...` 循环，失败继续跑下一个） | 竞态检测（**本机做不到**：无 gcc） | **红**（run 37374000997 / 37378602289；这是唯一红的一项） |
| `.\tools\go.cmd run .\cmd\verify*` | 2353 条 Go 端到端断言 | 尚未接入（需要 Windows runner） |
| `.\scripts\verify-m*.ps1` | 1060 条 curl 端到端断言 | **绿**：`curl gates (M0-M6, operator token)` job 在 run 37374000997 上 119 秒跑完，8 个门（M0–M6 + 管理面令牌门）全过 |

两个 `go test` 步骤把整份 transcript 写进 `/tmp`，再由 `scripts/ci-summarize-go-test.sh`
捕出失败测试与**完整的 DATA RACE 报告**，写进**该 job 的 step summary**（`$GITHUB_STEP_SUMMARY`）；
每一步都带 `if: always()`，所以这条路在红的那一次也走得通。

**读这份摘要要登录 GitHub**，这一点必须明说，因为三个"让匿名读者也能读到"的做法都试过、都失败了：

**唯一匿名可读的东西是步骤清单**（`GET /actions/runs/{id}/jobs` 里的 `steps[]`，含每步的名字与
success/failure）。所以 race 那一关**逐个包跑**：`go test -race ./...` 只会给出"整个模块里有一个包
被检测器拒绝"，而按包循环之后，红的那一步**以包名命名**（`for pkg in $(go list ./...)`，失败继续跑下一个，
最后统一 `exit 1`）。代价是两行 shell，换来的是"去哪找"这个匿名读者唯一能拿到的答案。

1. **job 日志**（`GET /actions/jobs/{id}/logs`）与 **artifact 下载**都返回 403（原文
   `{"message":"Must have admin rights to Repository.", "status":403}`），已匿名验证。
   第一版就是为此加 `| grep`，但它只帮到本来就有权限打开日志的人。
2. **step summary 不进 check-runs 的 `output.summary`**：加了它的那次 run，check-run API 与
   匿名拉到的 job 页面 HTML 里都是空的。它确实渲染在 UI 里，但要先登录。
3. **工作流自己 `PATCH /check-runs/{id}`**：写 `output.summary` 会返回 2xx（`gh` 需要显式
   `GH_TOKEN`，否则退出码 4），**但 job 一结束该字段就被清空**。run 37377940124 里一个在 base job
   之后运行、专门 PATCH 并读回该字段的探测 job 证实了这一点：从外部读回来仍是 `null`。

所以现在留在仓库里的说法是诚实的：`curl gates` 与 `go test` 的结论对**登录后的读者**可见（Actions 页）；
失败原因在 job 的 step summary 里，原始 transcript 另存为 `go-test-logs` artifact。不再声称匿名可读。

### 管理面鉴权（`access`）的证据边界

两条独立链，和里程碑一样：Go 进程内（41 条断言，`internal/server/access_test.go` +
`internal/config/config_test.go`）与真进程 + curl（44 条，`scripts/verify-hardening.ps1`）。
后者编译真二进制、起真网关、用真 `curl.exe`，是唯一能证明「401 真的会发出来」的那条链。

| 说的事 | 证据 | 状态 |
| --- | --- | --- |
| 默认行为与 M0–M6 完全一致 | `internal/server/access_test.go`（`TestAccessDisabledMatchesTheM0M6Behaviour`，13 个端点逐个断言 200） | 绿 |
| 打开后 `/admin/*` 与 `/stats` 需要令牌 | 同文件 `TestAccessEnabledProtectsTheOperationalSurface`（GET 10 路径 × 无令牌/错令牌/对令牌，POST 4 条 flush 路径） | 绿 |
| 探针、`/metrics` 与 OpenAI 兼容面不受影响 | 同文件 `TestAccessLeavesTheProbesAndTheProxyOpen`、`TestAccessCustomHeaderAndQueryToken` | 绿 |
| 少带/带错令牌不可区分，且不回显凭证 | 同文件 `TestAccessRejectionShapeIsUsableAndTellsNothing` | 绿 |
| 路径前缀按路径段匹配（`/admin` 不覆盖 `/administrator`） | 同文件 `TestAccessCoversUsesPathSegments` | 绿 |
| 配置错误在加载期报错（启用但无令牌、`protect: ["/"]`） | `internal/config/config_test.go` 的 `TestAccessValidationErrors` | 绿 |
| 令牌从 `${ENV}` 展开，未定义即中止 | 同文件 `TestAccessSectionParsesAndExpands` | 绿 |

真进程 + curl 那一条链（`scripts/verify-hardening.ps1`，44/44）额外证的是：

| 说的事 | 断言 |
| --- | --- |
| 9 条运营路径与 4 条 flush 路径无令牌都是 401 | 段 4「all 9 operational paths…」「POST /admin/cache/flush…」 |
| 401 带 `WWW-Authenticate: Bearer realm="infergate"`，且不回显近似令牌 | 段 4 的响应形状三条 |
| 裸令牌 / `Basic` / 错头名 / `Bearer` 与令牌粘连四种畸形形态都被拒 | 段 4 的 malformed 循环 |
| 两个令牌都可接受（轮换不需硬切换），授权后拿到的是真文档 | 段 5 |
| `/healthz` `/readyz` `/metrics` `/v1/capabilities` 与 `POST /v1/chat/completions` 无凭证都 200 | 段 6（含「调用方的 provider 凭证不会被当成门令牌」） |
| `OPTIONS` 预检不要求令牌 | 段 7 |
| `protect` 是**替换**而非追加：只写 `/stats` 时 `/admin` 变回开放 | 段 8（另起一个网关进程验证） |
| 自定义头逐字比对、`Authorization` 不再被采纳、query 形态默认关闭而开启后可接受 | 段 9（两个网关进程） |
| 配置错误在**真二进制**上也是加载期退出（未定义变量、`protect: ["/"]`、启用却无令牌） | 段 2（各跑一次 `-check`，非零退出） |

三点值得记住的设计原因：① 默认保护集只含 `/admin` 与 `/stats`，探针和 `/metrics` 排除在外——
kubelet、compose healthcheck、Prometheus 抓取器都不带凭证，要求凭证会把一个能用的部署变成永远不健康的部署；
② 少带与带错返回逐字节相同的 401，否则门就成了「猜对了多少」的探测器；
③ 空保护集与 `protect: ["/"]` 都在加载期拒绝，因为门上的静默失效是这里唯一不能接受的失败模式。

这一段在 Go 单测之外补 curl 门的原因很直接：单测走的是 `srv.Handler()`，它证明的是策略正确；
只有真二进制 + 真 socket 能证明 401 真的会从 HTTP 层发出来、且 `WWW-Authenticate` 真的在线上。

---

## M0 验收口径

| 验收项 | 结论 | 证据 |
| --- | --- | --- |
| 单元 / 集成测试 | 全绿 | `go test ./...` exit 0（gateway / sse / miniyaml） |
| 静态检查 | 全绿 | `go vet ./...` exit 0 |
| Go 端到端 | 38/38 断言通过 | `go run ./cmd/verify` |
| curl 端到端 | 47/47 断言通过 | `scripts/verify-m0.ps1` |
| 字节透明 | 响应体与上游逐字节一致 | `TestPassthroughNonStreaming`、verify 断言 |
| 首字延迟可测 | 注入 250ms 停顿，实测首帧 268.0ms | curl `--trace-time` |
| `[DONE]` 补齐 | 上游省略时网关补齐 | `X-Mock-Omit-Done: 1` 断言通过 |
| 成本计量 | 采信 provider usage 并按单价折算 | 日志 `cost_usd=`、`infergate_tokens_total` |
| 字节账目一致 | 日志 `resp_bytes=1718` 等于 curl 落盘 1718 字节 | `tmp\stream.out` 交叉核对 |

---

## M1 验收口径

| 验收项 | 结论 | 证据 |
| --- | --- | --- |
| 单元 / 集成测试 | 全绿 | `go test ./...` exit 0（gateway / router / breaker / stats / sse / miniyaml） |
| 静态检查 | 全绿 | `go vet ./...` exit 0 |
| Go 端到端 | 64/64 断言通过 | `go run ./cmd/verify-m1` |
| curl 端到端 | 56/56 断言通过 | `scripts/verify-m1.ps1` |
| 优先级路由 | 无异常时第一名恒为 priority=1 | `TestPriorityIsTheDefaultOrder`、verify-m1 段 3 |
| 成本排序 | 更便宜的后端胜过更优先的贵后端 | `TestCostOrderingBeatsPriority`、`TestFreeBackendWinsOnCost` |
| 延迟排序 | 按窗口实测均值排，没测过的排最后 | `TestLatencyOrderingUsesTheMeasuredWindow`、`TestUnmeasuredBackendSortsLast` |
| 加权策略 | 按 weight 抽样，两个后端都能分到流量 | `TestWeightedStrategyHandsOutBothBackends` |
| 健康度参与排序 | 不健康的后端排到最后但仍然保留（不是直接剔除） | `TestUnhealthyBackendSortsLastButSurvives`、`TestScoreBlendsReliability` |
| 非流式故障转移 | 上游 5xx / 429 换下一个后端，客户端无感 | `TestFailoverOn5xx`、`TestFailoverOn429`、verify-m1 段 4 |
| 流式故障转移 | 首帧之前可以重试，已出帧不重试（不会拼出两份答案） | `TestStreamingFailoverRelaysTheSecondBackend`、verify-m1 段 4 |
| 4xx 不换后端也不透支健康度 | 上游 4xx 原样回传且记为成功 | `TestNoFailoverOn4xx` |
| 超时也走故障转移 | 单次上游超时只算这一次尝试失败 | `TestTimeoutFailsOverWithoutAnswering504ForTheClient` |
| 全部候选失败 | 回传最后一个 Provider 的状态与响应体 | `TestAllCandidatesFailForwardsTheProviderError`、`TestLastCandidateTimeoutAnswers504` |
| 重试预算有上界 | `max_failures_per_request` 封顶尝试次数 | `TestMaxAttemptsCapsTheFailoverBudget`、verify-m1 `checkMaxAttemptsBudget` |
| 熔断状态机 | closed → open → half-open → closed，半开只放 1 个探针 | `TestHalfOpenAdmitsExactlyOneProbe`、`TestFailedProbeReopensImmediately`、`TestHalfOpenProbesCloseTheBreaker` |
| 样本不足不熔断 | 低于 `min_requests` 永不跳闸：未知 ≠ 坏了 | `TestBelowMinRequestsNeverTrips` |
| 熔断不吞请求 | open 的后端排最后而非剔除；全熔断时给出 502 而不是挂住 | `TestBreakerStopsRoutingToADeadBackend`、verify-m1 段 5 |
| 能力路由 | 只送给声明了该能力的后端；无匹配返回 400 而不降级 | `TestCapabilityHeaderExcludesABackend`、`TestUnmatchedCapabilityIs400NotADowngrade` |
| 显式指定 | `X-InferGate-Upstream` 跳过打分，且是唯一候选 | `TestExplicitPinSkipsThePreferredBackend`、`TestExplicitPinWinsAndIsAlone` |
| 模型名改写 | 后端声明了具体模型就送它自己的名字 | `TestBackendReceivesItsOwnModelName`、`TestCatchAllWithConcreteModelPrefersTheConcreteName` |
| 四条可观测面 | 路由表 / 熔断 / 统计 / 指标都可读且带标签 | `/admin/upstreams`、`/admin/breakers`、`/stats`、`/metrics`，verify-m1 段 8 |
| 故障吸收实测 | 杀副本期间 18000 请求 0 错误 | `docs/baseline/m1-load-faulted-r{1,2,3}.json` |
| 熔断收益实测 | 中位延迟 420.42ms → 17.81ms（23.6×） | `docs/baseline/m1-summary.json` 的 `failover` 块 |

M1 的取舍与已知边界（都写在代码注释里，不是事后找补）：

1. **`upstream_timeout` 是"每次尝试"的预算，不是整条请求的共享预算**。共享预算会让慢的第一个候选
   用光整段时间，后面的健康候选拿到一个已经过期的 deadline，于是客户端拿到 504 而网关其实从未
   问过那个快的后端——那正是故障转移要解决的场景。代价是最坏情况 `上游超时 × 尝试次数`，
   两个旋钮都暴露在配置里，由使用方按自己的超时预算权衡。
2. **熔断失败率的分母是整个窗口，成功也计入**。所以短促故障在繁忙窗口里需要更多失败才会跳闸；
   这是"宁可晚跳闸也不要抖动"的选择，想更激进就调小 `window` 或 `min_requests`。
3. **流式重试只在首帧之前**。一旦有字节发给客户端就不能再换后端，否则会把两个后端的输出拼成
   一份看起来正常、实际自相矛盾的答案。
4. **打分与排序不碰 IO**：`internal/router` 只吃传入的候选与窗口快照，不读网络也不改输入，
   所以策略可以单测、决策可以解释（`reason=` 直接进日志）。

---

## M2 验收口径

| 验收项 | 结论 | 证据 |
| --- | --- | --- |
| 单元 / 集成测试 | 全绿 | `go test ./...` exit 0（cache / gateway / embed / redis / evalset / config 全包含在内） |
| 静态检查 | 全绿 | `go vet ./...` exit 0 |
| Go 端到端 | 103/103 断言通过 | `go run ./cmd/verify-m2` |
| 精确命中 | 同字节重试不再调用上游 | `TestCacheMissThenExactHit`、verify-m2 段 1 |
| 语义命中 | 换一种说法命中（语料实测 0.8819 的真同义对） | `TestCacheSemanticHitAcrossWording`、verify-m2 段 2 |
| 语义不误命中 | 语料里 13 对不该命中的全部不命中 | verify-m2 段 2 逐对检查 `evalset.Cases()` |
| 阈值有据 | 0.86：真命中 8/13、误命中 0/13（0.84 是 3 条误命中） | `go run ./cmd/measure-m2`，见 3.10 |
| 租户与模型隔离 | 不同 tenant / 不同 model 永不互借 | `TestCacheIsScopedToTenantAndModel`、verify-m2 段 3 |
| 采样请求只精确命中 | `temperature > 0` 的重试命中、改写不命中 | `TestCacheSamplingRequestsMatchExactlyOnly`、`internal/cache` `TestNondeterministicRequestsAreNeverSemanticallyMatched` |
| 工具请求只精确命中 | 带 `tools` 的请求只做精确匹配 | `TestCacheToolsAreExactMatchOnly` |
| 答案形状参与身份 | `max_tokens` / 对话前缀不同即不命中 | `TestCacheSignatureBlocksADifferentAnswerShape`、`TestCacheSignatureSeparatesConversationPrefixes` |
| 短提示不缓存 | 低于 `min_prompt_chars` 直接 `skip` | `TestCacheSkipsTrivialPrompts` |
| bypass / refresh | 跳过查询，且**仍然写入**新答案 | `TestCacheBypassSkipsTheLookupButStillStores`、`TestCacheRefreshReplacesTheStoredAnswer` |
| 流式重放 | 逐帧与原流一致（帧数、payload、`[DONE]` 都对） | `TestCacheReplaysStreamingAnswers`、verify-m2 段 6「byte-identical」 |
| 命中不算上游消耗 | 上游 token 计数不变，节省记在 `saved_tokens` | `TestCacheHitIsNotAttributedToAnUpstream`、verify-m2 段 9 |
| 两个 Store 同一套要求 | memory 与 Redis 跑同一个一致性套件 | `TestStoreConformance`（含过期、淘汰、flush、并发） |
| 存储降级 | store 死掉只是不命中，不是报错 | `TestRedisStoreDegradesWhenTheServerGoesAway`、verify-m2 段 8、`/metrics` 仍 200 |
| 只缓存成功的答案 | 上游 4xx 原样回传且不入缓存 | `TestCacheStoresOnlySuccessfulAnswers` |
| 关掉即 M1 | 不命中、不加头、行为与 M1 完全一致 | `TestCacheDisabledLeavesM1Behaviour`、`TestCacheDoesNotAnswerNonCompletionRoutes` |
| 四个管理面 | `/admin/cache`、`/admin/cache/lookup`、`POST /admin/cache/flush`、`/stats` 的 cache 块 | verify-m2 段 9；GET flush 返回 404 |
| 命中率实测 | 请求口径 58.97%（23/39），13/13 对全部覆盖 | `docs/baseline/m2-summary.json` 的 `hit_rate` |
| 零误命中实测 | 13 对近似但不同的语料发 26 次请求，0 次命中 | `docs/baseline/m2-summary.json` `false_hits: 0` |
| 命中延迟实测 | 纯命中 P95 5.5028ms（p50 1.0132） vs 纯未命中 P95 47.3147ms（p50 5.5327） | 同上 `latency.gateway_stats` |
| 命中不产生上游请求 | 上游日志 31 次调用 == 31 次未命中；每轮 3600 请求 = 3598 命中 + 2 未命中 | 同上 `hit_rate.upstream_calls_seen_by_mock`、`latency.rows` |
| 上游挂了仍能服务 | 杀掉上游进程后 26 次请求全部 200（21 精确 + 5 语义），5xx 0；未预热对照请求 502 | 同上 `shield` 块（含 `shield_assertion_passed`） |
| 成本实测 | 每 1000 请求 $0.036692 → $0.016（−56.39%） | 同上 `cost`（按配置价格表外推） |
| 压测无错误 | 缓存开/关各 3 轮 × 8/32 并发，错误数 0 | 同上 `latency.rows[].errors` |

M2 的取舍与已知边界（同样写在代码注释里）：

1. **命中判定必须能解释**：`X-InferGate-Cache` 区分 `hit-exact` 与 `hit-semantic`，
   `/admin/cache/lookup` 给出最近邻与相似度。语义缓存最难的运维问题不是"没命中"，而是
   "命中了错的东西"——这两种情况在响应体里长得一样。
2. **离线 `hashing` embedder 是词法的，天花板偏低**：语料里有 5 对真同义改写的相似度低于
   某个近似对，任何阈值都分不开。要更高的命中率就换成 `provider: http` 的真实 embedding
   模型，并重跑 `cmd/measure-m2`——阈值是配置项，因为它是 embedder 的属性。
3. **`memory` store 是每进程的**：多副本必须用 `redis`，否则两个副本会对同一个问题给出不同
   答案（缓存反而是不一致的来源）。所以 `buildCache` 在 Redis 连不上时直接启动失败，而不是
   静默退回进程内存。
4. **Redis 的淘汰是"按创建时间 FIFO"，不是 LRU**（ZSET 报不出读取顺序）：
   对近期性撒谎的缓存比承认自己按年龄淘汰的缓存更糟。memory store 则是真 LRU。
5. **省略 `temperature` 视为确定性**：这是最常见的情况，不这么判缓存几乎永远不命中；
   想更保守就设 `allow_nondeterministic: false` 并让调用方显式传参。
6. **语义检索是 scope 内的全量扫描**（几百条 512 维向量，几十微秒），不是 ANN 索引；
   `max_entries_per_scope` 就是让这个假设成立的上界。
7. **重放不算生成**：命中时不写 `first_token` 直方图——把接近 0 的重放混进去，会让这个指标
   在缓存用得最狠的时候"变好"。

---

## M3 验收口径

| 验收项 | 结论 | 证据 |
| --- | --- | --- |
| 单元 / 集成测试 | 全绿 | `go test ./internal/... -count=1` exit 0（含 `internal/quota`） |
| 静态检查 | 全绿 | `go vet ./...` exit 0 |
| Go 端到端 | 470/470 断言通过 | `go run ./cmd/verify-m3` |
| curl 端到端 | 323/323 断言通过 | `scripts/verify-m3.ps1`（内存 + 真 Redis 协议服务） |
| 日 token 预算 | 第三次 40-token 请求被拒（`tokens_per_day`），计数器停在已准入的量 | `checkDailyTokenBudget`、`TestDailyTokenBudgetRejects` |
| 日成本预算 | 同一套逻辑走微美元账目，拒绝原因为 `cost_per_day_usd` | `checkCostBudget` |
| 每分钟限流 | 第 N+1 次拒绝（`requests_per_minute`），分钟计数不涨 | `checkMinuteRateLimit` |
| 每会话预算 | 会话独立计量，会话缺失时不预扣 | `checkSessionBudget` |
| 降级仍是一次请求 | 200 + `degrade` + 改写模型/上限，预扣保留、结算返还差额 | `checkDegrade`、`TestDegradeKeepsTheReservation` |
| 账本按条目配平 | 混合流量（日 + 会话两个 token 维度、拒绝、缓存命中）下 `reserved − released + overshoot == settled` | verify-m3 `identityOK`、verify-m3-curl 段 11 |
| 拒绝不产生上游调用 | 拒绝时上游调用计数不变（预算挡下的正是 provider 账单） | `checkUpstreamNotCalled`、实测 `provider_calls_prevented: 39` |
| 缓存命中仍占配额 | 命中按 `Usage{Requests: 1}` 结算，token 记 0 | `checkCacheHitSettlesRequestsOnly` |
| fail-closed 是默认 | store 不可读 → 503 `infergate_quota_unavailable`，且 **Redis 连不上时启动失败** | `checkFailClosedOpen`、`TestNewServerFailsWhenQuotaRedisIsDown` |
| fail-open 可显式打开 | 同一次故障下 5/5 放行，`store_errors` 照记，且决策计数记 `allow` | `checkFailClosedOpen`、`docs/baseline/m3-summary.json` 的 `fail_open` |
| 键隔离与单射转义 | 配置里 `acme:inc` 与 `acme_inc` 各记各的账 | `checkKeyIsolation`、`TestKeyLayoutAndBucketFormats` |
| 三个观测面 | `/admin/quota`、`/stats` 的 `quota` 块、`/metrics` 七个 `infergate_quota_*` 族三者同源 | `checkSurfaces` |
| 非生成路径不受治理 | `/v1/models` 不带任何配额头 | `checkUngovernedRoutes` |
| 配置校验 | 负值、重复租户、`degrade` 却没有任何杠杆、`anomaly_ratio < 1` 都在**加载期**报错 | `internal/config` 的配额用例 |
| 预算挡下多少实测 | 40 个请求：无预算 40 次上游调用；`tokens_per_day: 280` 时 1 次（97.5% 被拒） | `docs/baseline/m3-summary.json` 的 `budget` |
| 预扣准确度实测 | 12 请求：reserved 2872 / settled 320 / released 2608 / overshoot 56 tokens，恒等式成立 | 同上 `accuracy` |
| 故障实测 | fail-closed 20/20 503 且上游 0 调用；重启 store 后同进程 10/10 恢复 | 同上 `fail_closed`、`fail_open` |

M3 的取舍与已知边界（同样写在代码注释里）：

1. **日预算是 UTC 天**：`untilDayEnd` 先转 UTC，所以 UTC+8 的机器上本地 08:00 重置。跨时区团队
   要么接受这一点，要么以后加 `timezone` 配置项——沉默地按本地时间算只会让对账更难受。
2. **放行不带 `-Limit`/`-Used`**：网关只在越界那次知道限额（放行要报余量就得额外读一次 store，
   那是给每个请求加一次开销）。要实时余量请查 `/admin/quota`。
3. **预扣是乐观记账，软限额是软的**：估算高估会让额度被"临时占用"，估算低估（长 CJK、短
   `max_tokens`）会穿透到 `overshoot_tokens`。这个计数器就是用来量"限额到底有多软"的。
4. **预扣不是跨维度事务**：`internal/redis` 没有 `MULTI`/`EVAL`（也没有会话概念），所以保证的是
   "每个维度一次原子 `INCRBY`"，不是"四个维度一起成功"；某维度失败时已扣的维度会被释放回滚。
5. **`memory` store 是每进程的**：多副本共用一份预算必须用 `redis`，否则 N 个副本各发一整份；
   所以 Redis 连不上时**启动失败**，而不是静默退化成进程内存。
6. **异常指纹只告警不拦流量**：花钱突然变多通常是真实业务，拦流量的是预算，不是这个比值。
7. **降级的 token 收益未测**：`cmd/mockupstream` 忽略 `max_tokens`，所以只证明了改写发生了
   （后端收到的模型名与上限），没证明省钱——换真 provider 才能量到。

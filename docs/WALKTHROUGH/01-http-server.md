# 第 01 册 · HTTP 服务层（internal/server）

本册走读 `internal/server` 包：进程如何从一份配置长出一个 HTTP 监听器，一条请求如何穿过中间件、进入某个 handler 或被交给网关代码，以及那些只在这一层才有的运维界面（`/admin/*`、`/metrics`、`/stats`）各自承诺了什么。

阅读约定：锚点一律写作 `符号名 (internal/server/server.go)` 的形式——符号在前，路径在括号里，**不写行号**。所有签名从源码照抄；未经测试或源码确认的判断会明写"未能确认"。

---

## 1. 本册地图

| 文件 | 行数 | 一句话职责 |
| --- | --- | --- |
| `internal/server/server.go` | 1154 | `Server` 类型的家：构造与依赖装配（`NewServer`）、监听与优雅关闭、全部只读运维 handler（健康、上游、断路器、`/stats`、`/metrics`）以及手写 Prometheus exposition 的全部渲染函数 |
| `internal/server/access.go` | 223 | 运维面的令牌门：`accessPolicy` 的构造与归一化、`covers` 路径段匹配、`authorized` 常量时间比较、`credential` 取凭据、`middleware` 包装 |
| `internal/server/cache.go` | 363 | M2 语义缓存的构造（嵌入器 + 存储后端）与 `/admin/cache*` 三个界面 |
| `internal/server/quota.go` | 251 | M3 预算治理的构造（Redis 或内存存储）与 `/admin/quota`；把"美元"配置翻译成"微美元"运行时策略 |
| `internal/server/m6.go` | 415 | M6 agent 平台集成的服务端半边：幂等重放存储、会话台账、能力发现（`/v1/capabilities` 与 `/v1/capabilities/probe`），以及 M6 的 metric 家族 |
| `internal/server/log.go` | 74 | 把三方法 `logAdapter` 桥接到 `*slog.Logger`，以便测试用桩替换日志 |
| `internal/server/tracing.go` | 366 | M5 追踪：`tracePlane`、`buildTracing`、`/admin/traces`、`/admin/traces/{id}`、`/admin/tracing`，以及 trace id 校验、limit 解析、JSONL 流式写出 |
| `internal/server/tracing_export.go` | 96 | 把 `config.TracingConfig` 变成活的导出器（本地 JSONL、OTLP），并保留它们各自的名字与计数器 |
| `internal/server/server_test.go` | 705 | 测试：exposition 家族与格式、`NewServer` 的三条契约（校验、归一化、未定价模型报告）、Redis 配额不可达必须启动失败 |
| `internal/server/access_test.go` | 421 | 测试：默认保护面、明确不保护面、"401 拒绝形状"、缺失/错误令牌不可区分、`covers` 的路径段语义、preflight 放行 |
| `internal/server/tracing_test.go` | 448 | 测试：端到端记录 trace 并读回、禁用时的报告形状、id 校验、环形淘汰计数、JSONL 与 OTLP 负载、坏导出器必须启动失败 |
| `internal/server/m6_test.go` | 567 | 测试：禁用时仍报告配置、真 handler 上的幂等重放（上游只被调一次）、按租户隔离、会话汇总与歧义、能力探测分类、M6 metric 是 gauge |

依赖方向：`internal/server` 依赖 `config`、`upstream`、`breaker`、`router`、`cache`、`quota`、`metrics`、`gateway`、`idempotency`、`sessions`、`tracing`、`traceexport`。反向没有依赖——`gateway` 不认识 `server`，这正是本册反复强调的那条边界："构造好之后把句柄交给网关"。

---

## 2. 数据流

### 2.1 启动阶段

进程启动时先有配置（配置由 `config.Load` 读入、校验并归一化；配置文件、默认值与环境覆盖的细节属于配置册，本册不展开）。本册关心的是随后发生的事：

1. **调用 `NewServer` (internal/server/server.go)**。签名 `func NewServer(cfg *config.Config, logger logAdapter) (*Server, error)`。它按固定顺序装配一切，顺序本身就是设计：
   - `upstream.New(cfg)` (internal/server/server.go) 先建上游注册表。这一步内部会调 `cfg.Validate`，所以**配置不可服务就在构造处失败**，而不是在第一个请求上失败。失败返回 `nil, fmt.Errorf("server: build upstream registry: %w", err)`。
   - `metrics.NewRecorder()` (internal/server/server.go) 建进程内计数器。
   - `gateway.NewPriceBook(cfg.Pricing)` (internal/server/server.go) 建价目表。
   - `reportUnpricedModels(cfg, priceBook, logger)` (internal/server/server.go)：把"声明了但没有价格"的模型一次说清（见 3.1）。
   - `breaker.NewGroup(cfg.Health, registry.Names())` (internal/server/server.go) 给每个后端建一个断路器，即使全部健康也建。注释要求：只在首次失败之后才建的断路器会让第一次失败的请求无处可去。
   - 一个闭包 `price` (internal/server/server.go) 把价目表折成 `(in+out)/2`，同时交给 `router.New` 和后续计费，保证路由与账单用同一把尺子。
   - `router.New(router.Options{...})` (internal/server/server.go)。
   - `buildCache(cfg, logger)` (internal/server/cache.go) 建缓存对象（**可能返回错误**，直接向上传）；只有 `cfg.Cache.Enabled` 才把句柄交给代理。
   - `buildQuota(cfg, logger)` (internal/server/quota.go) 建配额管理器与存储名；只有 `cfg.Quota.Enabled` 才交给代理。
   - `buildTracing(cfg, logger)` (internal/server/tracing.go) 建追踪平面；禁用时环根本不存在。
   - `buildIdempotency` (internal/server/m6.go) 与 `buildSessions` (internal/server/m6.go)：两个都建，只有启用时才交给代理。
   - `gateway.New(gateway.Options{...})` (internal/server/server.go) 组装代理，把上面所有句柄连同 `cfg.Server.MaxBodyBytes`、`cfg.Server.UpstreamTimeout.Duration()`、重试上限与退避、`QuotaCharsPerToken`、`QuotaCompletionTokens`、`Tracer`、`Models: modelInfo(cfg)` 一起装进去。
   - 组装 `&Server{...}` (internal/server/server.go)，其中 `access: newAccessPolicy(cfg.Access)` 与 `started: time.Now()`（见 3.1）。
   - `mux := http.NewServeMux()` (internal/server/server.go) 并注册路由（见第 4 节）。
   - `mux.Handle("/", proxy)` (internal/server/server.go)：其余一切都是 OpenAI 兼容面。
   - `s.http = &http.Server{...}` (internal/server/server.go)：`Addr` 是 `cfg.Server.Listen`，`Handler` 是 `s.access.middleware(withAccessControl(mux, cfg.Server.MaxBodyBytes))` (internal/server/server.go)，只设 `ReadHeaderTimeout` 与 `IdleTimeout`。
   - 返回 `s, nil`。

   `NewServer` 的四条契约写在它的文档注释里，逐条翻译保留：
   - **它校验配置**：注册表构建会调 `cfg.Validate`，所以不可服务的配置在这里失败，不是在首个请求上失败；这让调用者可以放心地把 `nil` error 读作"这份配置可服务"，也避免一个坏配置在三请求之后变成运行时谜案。`TestNewServerValidatesTheConfigItIsGiven` (internal/server/server_test.go) 钉住了这条。
   - **它不做任何 I/O**：不监听、不拨号。Redis 支持的缓存或预算存储只是**构造**，不是被**连接**，所以它能在无网络的单元测试里使用。
   - **它不改配置，除非校验改**：`cfg.Validate` 既是校验器也是归一化器，会在被给的指针上就地填默认值（空的上游 tier 变成 `cloud`，权重 0 变成 1，配额比例为 0 变成该段默认值）。所以调用方传进来的结构在任何未设置项时会被改；这在生产里不可见（`Load` 先校验过一遍，等有人调用这里时工作已经做完，第二遍什么也不改），正因如此这才值得写出来而不是让人去发现。`TestNewServerNormalisesTheConfigItIsGiven` (internal/server/server_test.go) 钉住了形状：未设置项被填、显式设置的项不动、结果是固定点。
   - **它保留被给的指针**（`s.cfg`）：监听地址、admin 面的路由与健康报告、整个 handler 链之后都从它读；而 router、breaker、cache 是用**此刻可见的值**建的。先配置、后构造：构造之后再改配置，只会改变 admin 端点**报告**的内容，而不改变服务**实际做的事**——那是静默不一致，不是配置变更。

2. **`main` 调用 `SetVersion`** 注入链接期版本（`SetVersion (internal/server/server.go)`），然后调用 `ListenAndServe` (internal/server/server.go)。

3. **`ListenAndServe` (internal/server/server.go)**：`net.Listen("tcp", s.cfg.Server.Listen)` (internal/server/server.go)，失败返回 `fmt.Errorf("server: listen on %s: %w", ...)`；成功则打一条 info（`addr` / `version` / `upstreams` 名字），再按 `s.access.enabled` 打一行关于令牌的日志（见第 7 节），最后 `s.http.Serve(ln)` (internal/server/server.go)。`http.ErrServerClosed` 被吞掉，其它错误原样返回。

### 2.2 一条请求的路径

```
TCP accept
  └─ http.Server   (ReadHeaderTimeout / IdleTimeout 生效；无 ReadTimeout、无 WriteTimeout)
       └─ accessPolicy.middleware   (internal/server/access.go)
            ├─ OPTIONS              → 直接放行
            ├─ !covers(path)        → 直接放行
            ├─ credential + authorized 失败 → reject（401）
            └─ withAccessControl    (internal/server/server.go)
                 ├─ ContentLength > maxBody → 413
                 └─ ServeMux
                      ├─ 精确路由 → Server.handleXxx
                      └─ "/"      → *gateway.Proxy.ServeHTTP  (internal/gateway/proxy.go)
```

要点：

- 顺序是"先令牌、后体积"。`withAccessControl` (internal/server/server.go) 是**内层**，`accessPolicy.middleware` (internal/server/access.go) 是**外层**（见 `NewServer` 里 `Handler` 的写法）：未授权请求不该到达会改状态的端点，也不该为一次它无权发起的 body 读取付费。`accessPolicy.middleware` 的注释把这一点写成了硬要求。
- 令牌门在未配置令牌时**整个消失**：`middleware` 直接 `return next`，不产生任何 per-request 成本（`internal/server/access.go`）。
- `"/"` 是兜底模式。Go 1.22 起的 `ServeMux` 偏好更具体的模式，所以上面注册的精确路由优先，`"/"` 只看见没人认领的路径——这条注释就写在注册处旁边（`internal/server/server.go`）。
- 被代理的 `/v1/*` 不经过网关自己的任何鉴权；它的鉴权是调用方持有的 provider key，网关原样转发。访问层注释与 `TestAccessLeavesTheProbesAndTheProxyOpen` (internal/server/access_test.go) 都钉了这一点。
- `/v1/capabilities` 与 `/v1/capabilities/probe` 虽然是 `/v1` 前缀，却是网关自己注册的精确路由，不会被 `mux.Handle("/", proxy)` 抢走。注册处的注释说明了原因：没有哪家 provider 提供这个端点。

### 2.3 关闭阶段

`main` 收到信号后按固定顺序收尾：

1. `Shutdown(ctx) (internal/server/server.go)`：先 `s.http.Shutdown(ctx)`（停止接受新连接、**等待在飞 handler 返回**），**然后** `s.registry.CloseIdleConnections()`。注释给出的理由：顺序很重要——`http.Server.Shutdown` 要等在飞 handler 返回，而流式 handler 只有在上游调用结束时才返回；先释放连接池会掐死正在被 drain 的那些请求。
2. `CloseCache() (internal/server/cache.go)`：监听停止之后调用，理由同上——还在为在飞请求写入的计数器/连接池不能被从底下拆掉。
3. `CloseQuota() (internal/server/quota.go)`：同上。
4. `CloseTracing() (internal/server/tracing.go)`：请求路径是 trace 的唯一生产者，所以此时不会再有新 trace，一次有界 drain 就够了。

未能确认：`main` 调用这四个关闭函数的**具体代码位置与顺序**（`cmd/` 下），本册只从被调函数的注释里读出了设计意图，没有逐行核对 `main`。

---

## 3. 逐符号走读

### 3.1 internal/server/server.go

#### 包级变量与类型

- `var Version = "0.1.0-m1"` (internal/server/server.go)。中文注释：它是 `/healthz` 与根端点报告的构建版本；之所以是 `var`，是为了让发布构建用 `-ldflags` 设置它。
- `type Server struct { ... }` (internal/server/server.go)。字段逐个说明：
  - `cfg *config.Config`：保留的配置指针，之后所有 admin 报告与监听地址都从它读。
  - `log logAdapter`：三方法日志接口。
  - `proxy *gateway.Proxy`：兜底 handler，也是能力报告的数据来源。
  - `registry *upstream.Registry`：已解析的上游表。
  - `breakers *breaker.Group`：每后端一个断路器。
  - `router *router.Router`：路由策略，`handleUpstreams` 读它的 `Strategy()`。
  - `recorder *metrics.Recorder`：进程内计数器，`/stats` 与 `/metrics` 都从这里取数。
  - `cache *cache.Cache`：M2 缓存；禁用时也非 nil（只是一个从未交给代理的对象）。
  - `quota *quota.Manager` 与 `quotaStore string`：M3 管理器与后端名字（`"memory"`、`"redis"`、`"disabled"`）。注释说得很直白：`quotaStore` 存在是为了让 `/admin/quota` 能报告后端名而**不必把配置拆开**。
  - `trace *tracePlane`：M5 追踪平面。注释：**它永远非 nil**，这样 `/admin/traces` 与 `/admin/tracing` 在追踪关闭时也能报告配置。
  - `idempotency *idempotency.Store`、`sessions *sessions.Ledger`：M6 两个状态载体。注释：两者即使禁用也存在，好让 admin 面报告"配了什么"；只有启用的那一个被交给代理，所以禁用的那个每请求零成本。
  - `access accessPolicy`：构造时算一次（进程运行期间答案不会变），未配置令牌时是 no-op。见 access.go。
  - `http *http.Server`、`started time.Time`、`version string`。

#### 版本与日志接口

- `func (s *Server) SetVersion(v string)` (internal/server/server.go)。作用：`strings.TrimSpace(v) != ""` 时才写 `s.version`。空串不覆盖——这样二进制"没有注入版本"时保留包默认值。读 `s.version`，写 `s.version`。无错误路径、无锁。注释（翻译）：二进制用链接期注入的值调用它；不设置则保留包默认。调用者：`main`。
- `func (s *Server) Version() string` (internal/server/server.go)。作用：`s.version == ""` 时返回包级 `Version`，否则返回 `s.version`。纯读。被 `handleHealthz`、`handleReadyz`、`handleMetrics`（`infergate_build_info`）与 `ListenAndServe` 的启动日志调用。
- `type logAdapter interface { Info(msg string, args ...any); Error(msg string, args ...any); Warn(msg string, args ...any) }` (internal/server/server.go)。注释（翻译）：把 slog 收窄到服务器真正需要的三个方法，让服务器对日志的依赖显式，也让测试里替换它变得微不足道。并发：接口本身无状态；实现方负责自己的并发安全。

#### 未定价模型报告

- `func reportUnpricedModels(cfg *config.Config, book *gateway.PriceBook, logger logAdapter)` (internal/server/server.go)。作用：说明哪些上游声明的模型价目表没有覆盖，因为它们的成本等于 `pricing.default`。行为：
  - 遍历 `cfg.Upstreams[].Models`：`""` 跳过（注释翻译：它什么也没命名；`upstream.New` 会拒绝空 allow-list，而 allow-list 里的空项不是一个模型）；`"/"` 记 `catchAll = true`；其余收进 `declared`。
  - `book.Unpriced(declared)`；若既无未定价项也无 catch-all，直接返回。
  - `models` 为空串时替换成提示语 `(none named: the catch-all accepts whatever a client sends)`。
  - `def := cfg.Pricing.Default`；日志键固定为 `models`、`catch_all`、`default_in_usd_per_mtok`、`default_out_usd_per_mtok`。
  - `def.In == 0 && def.Out == 0` → `logger.Warn("pricing: these models have no price and pricing.default is 0, so their cost_usd is 0 and cost budgets cannot see their traffic", args...)`；否则 `logger.Info("pricing: these models have no price and are billed at pricing.default", args...)`。
  - 读状态：`cfg.Upstreams`、`cfg.Pricing.Default`、价目表；写状态：无（只写日志）。无错误返回（日志失败由实现方决定）。无锁：构造期单 goroutine 调用。
  - 注释里"为什么"的部分要保留：两种情况安静程度不同，所以不能用同一种方式记录。非零默认值仍然记录一个真实的数字（一个猜测，也就是默认价格被文档化的行为），值得一条 info。默认值为 0 时记录的是 $0，这是让花费只增长却**永远碰不到成本预算**的方向，所以是一条 warning，并点名要设置哪两个旋钮。另外，`"/"` 兜底单独报告而不是展开成列表：它接受的是这份配置从未提过的模型名，所以它最多只能说"未定价流量会以谁都没提到的模型名到来"。
  - 调用者：`NewServer (internal/server/server.go)`。测试：`TestNewServerReportsModelsItCannotPrice` (internal/server/server_test.go)。

#### 构造

- `func NewServer(cfg *config.Config, logger logAdapter) (*Server, error)` (internal/server/server.go)。见 2.1。错误路径共五条，全部**返回 `nil` 服务器**，绝不返回半个服务器：注册表构造失败、`buildCache` 失败、`buildQuota` 失败、`buildTracing` 失败（`buildTraceExporters` 的错误经它上抛）、`buildIdempotency`/`buildSessions` 无错误。测试：`TestNewServerValidatesTheConfigItIsGiven`、`TestNewServerFailsWhenQuotaRedisIsDown`、`TestNewServerNormalisesTheConfigItIsGiven`、`TestNewServerReportsModelsItCannotPrice` (internal/server/server_test.go)。

#### 生命周期

- `func (s *Server) ListenAndServe() error` (internal/server/server.go)。见 2.1。错误路径：监听失败包成 `server: listen on %s: %w`；`Serve` 返回非 `http.ErrServerClosed` 的原样返回。日志键：启动 info 是 `addr`/`version`/`upstreams`；令牌 info 是 `paths`/`header`/`tokens`（只有数量）。注释（翻译保留）：在启动时说一次，两个方向都说。配了令牌的运维者想要"它生效了"的确认；没配的人不该为了知道 admin 面是敞开的而去把配置读回来。**令牌本身从不被记录**。warning 原文：`no operator token configured; /admin/* and /stats are open to anyone who can reach this port`。
- `func (s *Server) Addr() string` (internal/server/server.go)。返回 `s.cfg.Server.Listen`。纯读。
- `func (s *Server) Shutdown(ctx context.Context) error` (internal/server/server.go)。见 2.3。返回 `s.http.Shutdown(ctx)` 的错误；`CloseIdleConnections()` 无返回值，不影响错误。`ctx` 由调用方给期限。
- `func (s *Server) Handler() http.Handler` (internal/server/server.go)。返回 `s.http.Handler`，即整条中间件链。注释：为测试和"把 InferGate 嵌进另一个服务器"而导出。`access_test.go` 里的 `accessTestServer` 正是用它驱动真实链而不是复制一份。
- `func (s *Server) Breakers() *breaker.Group` (internal/server/server.go)。注释（翻译）：为验收脚本导出，它们必须**直接观察**断路器的状态迁移，而不是从响应体推断："请求成功了"和"请求成功因为跳过了已跳闸的后端"在线上是同样的字节，只有断路器能区分它们。

#### 只读运维 handler

- `func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request)` (internal/server/server.go)。恒 200，体 `{"status":"ok","version":...,"uptime":...}`，其中 uptime 是 `time.Since(s.started).Round(time.Second).String()`。读 `s.started` 与 `Version()`；不读任何下游。注释（翻译）：这是存活探针，**不能依赖任何下游**，否则一次 provider 故障会让编排器重启一个完全健康的网关。`r` 未使用。
- `func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request)` (internal/server/server.go)。`s.registry.Names()` 为空 → 503 体 `{"status":"not ready","reason":"no upstream configured","version":...}`；否则 200 体 `{"status":"ready","version":...,"upstreams":[...]}`。注释（翻译）：它是就绪探针，报告网关能否服务流量，在 M0 里就是"至少配置了一个上游"。注意 uptime 不在就绪体里，这是有意的区分。
- `func (s *Server) handleUpstreams(w http.ResponseWriter, r *http.Request)` (internal/server/server.go)。内部类型 `upstreamView` (internal/server/server.go)，JSON 标签：`Name/name`、`Kind/kind`、`BaseURL/base_url`、`Models/models`、`CatchAll/catch_all`、`HasKey/has_api_key`、`Capabilities/capabilities`、`Priority/priority`、`Weight/weight`、`Tier/tier`。遍历 `s.registry.Names()`，`s.registry.Target(name)` 的 `ok` 为假时 `continue`（静默跳过，不报错）。响应体四段：`upstreams`、`model_index`（`s.registry.ModelIndex()`）、`routing`（`strategy` = `s.router.Strategy()`、`weights`、`fallback_model`、`default_capabilities`、`tier_policy`、`max_attempts` = `cfg.Health.MaxFailuresPerRequest`、`retry_backoff` = `cfg.Health.RetryBackoff.String()`）、`breaker_states`（来自 `breakerStates()`）。注释（翻译）：它回答任何事故的第一问——"这个模型去了哪个后端"；它也发布 tier 策略，因为决定本地还是云端的限额是同一场事故里的下一个问题，而在出问题的那台机器上读配置文件，与看见运行中的进程实际加载的策略不是一回事。注意 `HasKey` 只暴露布尔，不暴露 key 本身。
- `func (s *Server) handleBreakers(w http.ResponseWriter, r *http.Request)` (internal/server/server.go)。取 `s.breakers.Reports()`；`?upstream=` 非空时过滤出名字相等的那一条（保持原顺序）；`summary` 是各 `rep.State` 的出现次数（`map[string]int`）；响应体 `{"upstreams":[...],"summary":{...},"health":{...}}`，`health` 键为 `window`、`min_requests`、`failure_ratio`、`open_duration`、`half_open_probes`、`max_attempts`。注释（翻译）：窗口是被有意包含进来的。一个已经跳闸的断路器只是半个故事；运维者需要看清它是在三次请求里失败了三次（后端死了）还是在两百次里失败了七十次（后端在负载下退化），因为前者是重启，后者是容量决策。
- `func (s *Server) handleBreakerReset(w http.ResponseWriter, r *http.Request)` (internal/server/server.go)。`?upstream=` 为空 → `s.breakers.ResetAll()`、打 `"breaker reset"` 带 `scope=all`、200 体 `{"reset":"all","upstreams":[名字...]}`。名字未知（`s.registry.Target` 查不到）→ 404 体 `{"error":{"message":"unknown upstream <name>","type":"infergate_unknown_upstream"}}`。否则 `s.breakers.Get(name).Reset()`、打 `"breaker reset"` 带 `upstream=<name>`、200 体 `{"reset":"<name>"}`。注释（翻译）：它是 POST 因为它改变状态，而且**故意不暴露成 GET**：一个会爬链接的监控系统否则会把运维者正在看的那些断路器重置掉。
- `func (s *Server) breakerStates() map[string]string` (internal/server/server.go)。把 `Reports()` 折成 `名字 -> 状态字符串`，容量按 `len(s.registry.Names())` 预分配。纯读。调用者：`handleUpstreams`。
- `func (s *Server) handleStats(w http.ResponseWriter, r *http.Request)` (internal/server/server.go)。这是给人（和压测工具）看的 JSON 计数器视图。步骤：
  1. `snap := s.recorder.Snapshot()`，每行摊成 `{route, upstream, model, status, outcome, count, mean_seconds, total_seconds}`（`outcome` 是 `string(row.Outcome)`），同时累加 `requests` 总数。
  2. `samples := s.recorder.RequestLatencySamples()`，**只排序一次**（`sort.Slice`），然后取 p50/p90/p95/p99/max，全部经 `percentileSorted(...).String()`。注释（翻译）：旧形状对每个分位数调用一次 `percentile()`，而 `percentile` 每次调用都复制并重排整个样本集，所以一次 `/stats` 抓取会为一个随运行时长增长的切片付五次排序的钱；在这里排一次、从已排序切片取值，保住了完全相同的最近秩数值，只花五分之一的工作。
  3. `prompt, completion, cached := s.recorder.TokenTotals()`。
  4. `FirstTokenSnapshot()` 摊成 `{upstream, model, count, mean_seconds}`（`row.Mean.Seconds()`）；`StreamSnapshot()` 摊成 `{upstream, frames, bytes}`。
  5. 响应体键：`series`、`requests`、`latency`（`count` = `len(samples)`、`window` = `RequestLatencyWindow()`、`dropped` = `RequestLatencyDropped()`、`p50`/`p90`/`p95`/`p99`/`max`）、`tokens{prompt,completion,cached}`、`first_token_mean`、`streams`、`cache`（`s.statsCache(r.Context())`）、`quota`（`s.statsQuota(r.Context())`）、`uptime`。
  - 第二段注释（翻译保留）：`window` 与 `dropped` 描述的是**总体**，不是延迟本身——"最近 65536 个请求的 p99"与"自启动以来的 p99"是两个不同的断言，只有这两个键能让仪表盘区分它们。
- `func (s *Server) statsQuota(_ context.Context) map[string]any` (internal/server/server.go)。`s.quota == nil || !s.quota.Enabled()` → `{"enabled": false}`；否则返回 `enabled`、`store`（`s.quotaStore`）、`allowed`、`degraded`、`rejected`、`store_errors`、`alerts`、`reserved_tokens`、`settled_tokens`、`released_tokens`、`released_cost_micros`、`overshoot_tokens`、`overshoot_cost_micros`（后八个来自 `s.quota.Stats()`）。注释（翻译）：它报告的是管理器自己的计数器，不是配置——事故期间的问题是"上一小时我们拒绝了多少"，而配置回显回答不了；配置的预算住在 `/admin/quota`。参数被命名为 `_`，因为这个分支确实不用它。
- `func (s *Server) statsCache(ctx context.Context) map[string]any` (internal/server/server.go)。`s.cache == nil || !s.cache.Config().Enabled` → `{"enabled": false}`；否则 `store`、`lookups`、`hits`、`exact_hits`、`semantic_hits`、`misses`、`stores`、`hit_ratio`（`hitRate(cs)`）、`stale_evictions`、`entries`（`cacheEntries(ctx, s.cache)`）、`evictions`（`ss.Evicted`）、`errors`（`cs.LookupErrors + cs.StoreErrors`）、`saved_tokens{prompt,completion,total}`。注释（翻译）：nil 或禁用的缓存报告 `{"enabled": false}` 而不是省掉这个键，这样仪表盘能分辨"关掉"与"还没部署"。
- `func cacheEntries(ctx context.Context, c *cache.Cache) int` (internal/server/server.go)。`c.Store().Len(ctx, "")` 出错返回 `-1`（**哨兵值**，不是错误）；否则返回计数。注释（翻译）：这个取舍是刻意宽容的——它喂的是一个 gauge，一个短暂不可用的存储（Redis 正在重启）不该在响应里其它每个数字仍然有效时让 `/metrics` 变成 500。调用者：`statsCache`、`handleMetrics` 的缓存块。

#### `/metrics` 渲染

- `func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request)` (internal/server/server.go)。先 `w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")`，再用一个 `strings.Builder` 顺序拼出全部家族，最后 `_, _ = w.Write([]byte(b.String()))`（写错误被显式丢弃）。输出规约见第 5 节。注释（翻译）：手写而不是引 `client_golang` 是有意的——M0 没有第三方依赖，而 exposition 格式是一个稳定、很小的契约。M5 会用 OpenTelemetry 导出器替换它，那才是真正集成价值所在（exemplars、OTLP 推送、resource attributes）。
- `type histogramSeries struct { labels []string; count int64; sum float64; buckets []metrics.Bucket }` (internal/server/server.go)。注释（翻译）：一个带标签的直方图，从 Recorder 摊平出来，好让渲染方不必知道是哪个访问器产出的它。
- `func requestDurationSeries(rows []metrics.RequestDurationHistogram) []histogramSeries` (internal/server/server.go)。标签 `[]string{r.Route, r.Upstream, r.Model}`，抄 `Count`/`Sum`/`Buckets()`。
- `func attemptDurationSeries(rows []metrics.AttemptDurationHistogram) []histogramSeries` (internal/server/server.go)。标签 `[]string{r.Upstream}`。
- `func firstTokenSeries(rows []metrics.FirstTokenHistogram) []histogramSeries` (internal/server/server.go)。标签 `[]string{r.Upstream, r.Model}`。
- `func completionTokensSeries(rows []metrics.CompletionTokensHistogram) []histogramSeries` (internal/server/server.go)。标签 `[]string{r.Upstream, r.Model}`。
  - 这四个函数都没有错误路径；都用 `make(..., 0, len(rows))` 预分配，空输入返回长度为 0 的非 nil 切片。并发：只读入参切片。
- `func writeHistogramFamily(b *strings.Builder, name, help string, labelNames []string, series []histogramSeries)` (internal/server/server.go)。先写 `# HELP name help` 与 `# TYPE name histogram`（**即使 series 为空也写**），然后对每条 series：按 `labelNames` 建基础标签对 `ln=%q`；`withLe` 闭包按桶追加 `le=%q`；逐桶输出 `name_bucket{...,le="<边界>"} <count>`；最后 `name_count{...} <count>` 与 `name_sum{...} <sum>`（`%g`）。注释（翻译）：exposition 格式在这里是严格的，所以形状只写一次而不是每个家族写一遍：每个边界一条带 `le` 的 `_bucket` series，然后从同一条 series 派生出 `_count` 与 `_sum`。没有 series 的家族仍然拿得到 HELP 与 TYPE 行，这是 scraper 得知"这个指标存在、只是闲着"的方式——发一条没有标签的裸 bucket 行会是畸形 series。`le` 是按桶现加的而不是预先存的，因为它是唯一逐行变化的标签。
- `func labelSuffix(labels []string) string` (internal/server/server.go)。空切片返回 `""`，否则 `"{" + strings.Join(labels, ",") + "}"`。注释：为进程级指标产生的无标签 series 渲染"什么都没有"。测试：`TestLabelSuffix` (internal/server/server_test.go)。
- `func writeRuntimeMetrics(b *strings.Builder)` (internal/server/server.go)。先 `var ms runtime.MemStats; runtime.ReadMemStats(&ms)`，然后逐条经 `writeProcessMetric` 输出：
  - gauge：`infergate_runtime_goroutines`（`runtime.NumGoroutine()`）、`infergate_runtime_num_cpu`（`runtime.NumCPU()`）、`infergate_runtime_memstats_alloc_bytes`（`ms.Alloc`）、`infergate_runtime_memstats_heap_alloc_bytes`（`ms.HeapAlloc`）、`infergate_runtime_memstats_heap_inuse_bytes`（`ms.HeapInuse`）、`infergate_runtime_memstats_heap_objects`（`ms.HeapObjects`）、`infergate_runtime_memstats_stack_inuse_bytes`（`ms.StackInuse`）、`infergate_runtime_memstats_sys_bytes`（`ms.Sys`）；
  - counter：`infergate_runtime_memstats_total_alloc_bytes`（`ms.TotalAlloc`）、`infergate_runtime_memstats_gc_cycles_total`（`ms.NumGC`）、`infergate_runtime_gc_pause_seconds_total`（`float64(ms.PauseTotalNs)/1e9`，经 `strconv.FormatFloat(..., 'g', -1, 64)`）；
  - gauge `infergate_runtime_gc_last_pause_seconds`：`ms.NumGC > 0` 时取 `ms.PauseNs[(ms.NumGC+255)%256] / 1e9`，否则 `0.0`。注释（翻译）：`PauseNs` 是最近 256 次暂停的环，最新的一次在 `(NumGC+255)%256`；第一次回收之前每个槽都是零，而那里诚实的答案是"还没有暂停"，不是算术恰好指到的那个槽。
  - 最后是唯一带标签的 `infergate_runtime_go_version{version=%q} 1`（`runtime.Version()`），gauge。注释（翻译）：它是这里唯一带标签的指标，因为它是构建事实；当一次运行时升级只滚到一半主机时，全舰队仪表盘需要按它分组。
  - 顶部注释（翻译）：每个值都在这里读一次，而不是逐行惰性读——一次抓取若 goroutine 取自一个瞬间、堆取自另一个瞬间，就会自相矛盾，而这一块的全部意义正是让它们互相印证。手写自 `runtime` 包而不是收集器库，因为本仓库不取第三方依赖，而这段 exposition 小到可以直接读。测试：`TestWriteRuntimeMetricsFamilies` (internal/server/server_test.go)。
- `func writeProcessMetric(b *strings.Builder, name, typ, help, value string)` (internal/server/server.go)。写三行：`# HELP name help`、`# TYPE name typ`、`name value`。注释（翻译）：它取一个**已经格式化好**的值，这样字节计数器能保持精确整数，而不是被 `float64` 舍入一遍。

#### 中间件与通用辅助

- `func withAccessControl(next http.Handler, maxBody int64) http.Handler` (internal/server/server.go)。返回一个 `http.HandlerFunc`：`maxBody > 0 && r.ContentLength > maxBody` 时设 `Content-Type: application/json; charset=utf-8`、`WriteHeader(http.StatusRequestEntityTooLarge)`、用 `json.NewEncoder(w).Encode` 写 `{"error":{"message":"request body exceeds the configured limit","type":"infergate_request_too_large"}}` 并返回；否则 `next.ServeHTTP(w, r)`。注意这里**没有**用 `writeJSON`——它自己设头与状态码。注释（翻译）：在读到一个字节**之前**就拒绝声明过大的请求体。代理在读取时也会再限一次，因为 `Content-Length` 可能撒谎，或在 chunked 编码下缺失。并发：无共享状态，纯函数式包装。调用者：`NewServer (internal/server/server.go)`。
- `func writeJSON(w http.ResponseWriter, status int, v any)` (internal/server/server.go)。设 `Content-Type: application/json; charset=utf-8`，`w.WriteHeader(status)`，用 `json.NewEncoder(w)` 加 `enc.SetIndent("", "  ")` 编码；编码错误被显式丢弃（`_ =`）。为什么丢弃：响应头与状态码已经发出，此时没有别的事可做。调用者：本包几乎所有 handler。无锁；`http.ResponseWriter` 的并发约束由 `net/http` 保证（同一响应不得并发写）。
- `func percentileSorted(sorted []time.Duration, p float64) time.Duration` (internal/server/server.go)。约定：输入必须是**升序**切片，p 在 `[0,1]`。空切片 → 0；`p <= 0` → `sorted[0]`；`p >= 1` → `sorted[len(sorted)-1]`；否则 `idx := int(float64(len(sorted)) * p)`，越界钳到 `len(sorted)-1`，返回 `sorted[idx]`。注释（翻译）两条：①调用方负责排序——一个要报告同一总体五个分位数的 handler 不该为它们排五次序，而那正是上一个"每个分位数排一次"的辅助函数做的事；②选最近秩而不是插值，是因为运维者依据行动的那个数字是"第 99 个用户有多慢"，它必须是**被观测到的某个请求**；两个样本之间的插值是从来没有发生过的延迟。测试：`TestPercentileSortedNearestRank`、`BenchmarkPercentileSorted` (internal/server/server_test.go)。

### 3.2 internal/server/access.go

#### 常量与类型

- `const authRealm = "infergate"` (internal/server/access.go)。注释（翻译）：realm 是客户端想向人要令牌时展示给人的名字，所以它命名**服务**而不是方案。用途：`reject` 里拼 `WWW-Authenticate`。
- `type accessPolicy struct { enabled bool; protects []string; header string; bearer bool; tokens []string; allowQuery bool }` (internal/server/access.go)。字段语义（逐条翻译源码注释）：
  - `enabled`：未配置令牌时为 false。此时网关的行为与这个文件存在**完全相同**——这正是让每份 M0–M6 配置和每个验收脚本保持绿色的原因。
  - `protects`：归一化后的受保护路径前缀列表。
  - `header`：携带凭据的请求头，默认 `Authorization`。可配置，因为一个部署可能坐在已经占用了该头的组件后面（OAuth 代理、ingress），需要把运维令牌放到别处。
  - `bearer`：为真时按 `Bearer <token>` 比较而不是逐字比较。它只对 `Authorization` 生效，因为其它头没有可谈的方案约定。
  - `tokens`：被接受的凭据。多于一个，是为了让轮换成为一次配置编辑而不是硬切换：加上新令牌、滚动调用方、去掉旧的。
  - `allowQuery`：接受 `?access_token=`，用于"浏览器地址栏就是客户端"的情形（Grafana 的数据源探测、runbook 里的 curl 一行）。默认关闭：URL 里的令牌会落进访问日志、代理日志和浏览器历史。
  - 类型级注释说明这个文件为何存在（翻译保留）：到 M6 为止，这个网关的每一个运维面都无需认证——`/admin/*` 能清缓存、重置断路器、清空重放存储与会话台账，`/stats` 暴露按租户的成本与配额细节。在只绑回环的开发机上那是方便；监听一旦绑到别处它就是敞开的门，README 不得不为此加一整段警告。这个文件把那一段变成旋钮。**刻意不保护的面及为什么列表是可配置的而不是硬编码的**：`/healthz` 与 `/readyz` 是探针，kubelet、compose healthcheck 或负载均衡器并不持有凭据，要求一个会把能用的部署变成永久不健康，而连接本身就能知道的事它们也不多说；`/metrics` 是 Prometheus 抓取目标，私有网段上开放是惯例，想关掉的人可以把它列进 `access.protect`。所以默认识别表恰好是能**改状态**或**暴露租户数据**的集合：`/admin` 面与 `/stats`。

#### 构造与归一化

- `func newAccessPolicy(cfg config.AccessConfig) accessPolicy` (internal/server/access.go)。行为：
  - `protect := cfg.Protect`；若为空则 `protect = config.DefaultAccessProtect()`（即 `[]string{"/admin", "/stats"}`，见 `internal/config/config.go`）。注释（翻译）：空列表意思是"默认集合"，不是"什么都不保护"。配了令牌却什么都不保护，从来不是运维者的本意，而这恰好是会**看起来像成功了的**那个错误。
  - 逐项 `p = strings.TrimRight(strings.TrimSpace(p), "/")`，非空才收进 `protects`。
  - `sort.Slice(protects, func(i, j int) bool { return len(protects[i]) > len(protects[j]) })`——**长度降序**。注释（翻译）：最长优先，于是一份同时写了 `/admin` 与 `/admin/cache` 的策略会匹配更具体的那一条，并且在日志行里更好读。
  - `header := strings.TrimSpace(cfg.Header)`，为空则 `"Authorization"`。
  - `tokens` 逐项 `strings.TrimSpace`，非空才收。
  - 返回 `enabled: cfg.Enabled && len(tokens) > 0`（**两个条件都要**）、`bearer: strings.EqualFold(header, "Authorization")`、`allowQuery: cfg.AllowQueryToken`。
  - 读写状态：读 `cfg`，返回新值；不写任何共享状态。无锁：构造期单 goroutine 调用，之后 `accessPolicy` 按值持有、只读。调用者：`NewServer (internal/server/server.go)`。测试：`TestAccessPolicyTreatsEmptyConfigurationAsOff`、`TestAccessCoversUsesPathSegments` (internal/server/access_test.go) 直接调用它。

#### 判定分支

- `func (p accessPolicy) covers(path string) bool` (internal/server/access.go)。对每个前缀判断 `path == prefix || strings.HasPrefix(path, prefix+"/")`，命中即 true，遍历完返回 false。注释（翻译）：前缀语义在路径分隔符处停止——一条写 `/admin` 的策略必须覆盖 `/admin/cache`，但不能覆盖假想的 `/administrator`，而朴素的 `strings.HasPrefix` 会悄悄把它包含进来。因为比较的是 `r.URL.Path`（不是请求行原文），查询串不是路径的一部分；`TestAccessCoversUsesPathSegments` 的注释专门说明了这一点。纯读，无锁。
- `func (p accessPolicy) reject(w http.ResponseWriter)` (internal/server/access.go)。`p.bearer` 为真时 `w.Header().Set("WWW-Authenticate", `Bearer realm="`+authRealm+`"`)`；然后 `writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "this endpoint requires an operator token", "type": "infergate_unauthorized"}})`。错误串原文两条：HTTP 状态 401；`message` = `this endpoint requires an operator token`；`type` = `infergate_unauthorized`。注释（翻译）：两种失败模式都是 401 而不区分出 403，因为那种区分会告诉探测者它是不是猜中了一个真令牌、只是头格式写错了。响应体**故意不回显收到的值**：被拒绝的值可能是另一个服务的有效令牌，而响应体是系统里最容易最终进日志的东西。测试：`TestAccessRejectionShapeIsUsableAndTellsNothing` (internal/server/access_test.go) 断言 `WWW-Authenticate` 必须恰好等于 `Bearer realm="infergate"`、Content-Type 以 `application/json` 开头、body 里不含出示的凭据、`error.type` 为 `infergate_unauthorized`、`error.message` 非空，并且**缺失令牌与错误令牌的状态码与 body 完全相同**。
- `func (p accessPolicy) authorized(presented string) bool` (internal/server/access.go)。`match := 0`；对 `p.tokens` 每一项做 `match |= subtle.ConstantTimeCompare([]byte(presented), []byte(want))`；返回 `match == 1`。注释（翻译）：用常量时间比较而不是 `==`——Go 的字符串比较一遇到不同字节就返回，会泄露匹配前缀的长度，请求足够多就足以重建令牌；命中之后仍然比较**每一个**令牌，让工作量与命中的是哪一项无关。注意 `|=` 的用法：不短路、不早退，这正是它要的性质。纯读，无锁。
- `func (p accessPolicy) credential(r *http.Request) string` (internal/server/access.go)。流程：`raw, fromHeader := r.Header.Get(p.header), true`；`raw == "" && p.allowQuery` 时改成 `raw, fromHeader = r.URL.Query().Get("access_token"), false`；`raw == ""` 返回 `""`；`!p.bearer || !fromHeader` 时原样返回 `raw`；否则用常量前缀 `const prefix = "bearer "` 做长度检查加 `strings.EqualFold(raw[:len(prefix)], prefix)`，不匹配返回 `""`，匹配则返回 `raw[len(prefix):]`。注释（翻译）两条：①方案前缀**只对请求头剥离**。查询参数携带的是令牌本身——`?access_token=abc`，不是 `?access_token=Bearer%20abc`——把请求头的语法套上去会拒绝每一个格式正确的查询令牌，同时看起来还挺对。②除那一个分隔空格外的一切都逐字比较，并且**不做修剪**：那个空格是 bearer 语法的一部分，任何其它前导或尾随空白都是调用方发来的内容的一部分，而配置的令牌从不含它。接受 `" token "` 会让被接受的集合比配置的集合更宽，那不是该慷慨的方向。`TestAccessAcceptsAnyConfiguredToken` 的注释进一步说明：空白用例必须走查询参数，因为 Go 的头解析器在任何 handler 看到之前就剥掉头值的首尾空白，设那个头的测试是在断言 `net/http` 而不是这道门。纯读，无锁。
- `func (p accessPolicy) middleware(next http.Handler) http.Handler` (internal/server/access.go)。`!p.enabled` 直接 `return next`（**零开销**，没有 per-request 闭包）；否则返回一个 `http.HandlerFunc`，分支顺序为：`r.Method == http.MethodOptions` → 放行；`!p.covers(r.URL.Path)` → 放行；`!p.authorized(p.credential(r))` → `p.reject(w)` 并 return；否则 `next.ServeHTTP(w, r)`。注释（翻译）三条：①它在体积检查**之后**运行（它被套在 `withAccessControl` 之内）、在任何 handler 之前：对一个会改状态的端点来说，未认证的请求根本不该到达 handler，也不该为一次 body 读取付费。②OPTIONS 预检按规范不携带凭据：浏览器在预检上会省略 `Authorization`，所以拒绝预检会让每一个受保护端点在浏览器客户端里不可用（具体案例就是 Grafana 的数据源测试）。预检本身不执行任何 handler 逻辑。③放行路径的判定规则见 `covers` 与类型级注释。并发：`accessPolicy` 按值捕获、只读，可被任意多 goroutine 并发调用；`next` 的并发安全由被包装方负责。调用者：`NewServer (internal/server/server.go)` 把它装成最外层。测试：`TestAccessPreflightPassesThrough`、`TestAccessEnabledProtectsTheOperationalSurface` (internal/server/access_test.go)。

### 3.3 internal/server/cache.go

文件顶部注释（翻译保留）：缓存在这里建，而不是在网关内部建，因为它是唯一与另一个进程（Redis）说话、并且可能持有后台 goroutine 的组件。网关收到的是一个已经构建好、已经校验过的值，从而保持无 I/O——这正是它能在测试里被构造的原因。

- `func buildCache(cfg *config.Config, logger logAdapter) (*cache.Cache, error)` (internal/server/cache.go)。注意这里没有 `*Server` 接收者：它是构造期函数。
  - `cc := cfg.Cache`；`!cc.Enabled` 时**仍然返回一个对象**：`cache.New(cache.Config{Enabled: false}, cache.NewMemoryStore(cache.MemoryOptions{MaxEntriesPerScope: cc.MaxEntriesPerScope}), nil, loggerFrom(logger)), nil`。注释（翻译）：禁用的缓存仍然拿到对象，这样 `/admin/cache` 能报告**配置了什么**（运维者问"缓存开着吗、用的哪个存储"必须能在不读配置文件的情况下得到答案）；它只是从不被交给网关，所以禁用的缓存每请求零成本。
  - 启用时先选嵌入器（`var emb embed.Embedder; var embName string`）：
    - `config.EmbedProviderHashing` → `emb = embed.NewHashingEmbedder(cc.Embedding.Dims)`，`embName = emb.Name()`。
    - `config.EmbedProviderHTTP` → `embed.NewHTTPEmbedder(embed.HTTPOptions{BaseURL, Model, APIKey, Timeout: cc.Embedding.Timeout.Duration(), MaxInputChars})`，出错返回 `nil, fmt.Errorf("server: cache embedding: %w", err)`；成功则包一层 `&embed.Fallback{Primary: primary, Secondary: embed.NewHashingEmbedder(cc.Embedding.Dims), OnFallback: func(err error) { logger.Warn("cache: embedding request failed, using lexical fallback", "error", err.Error()) }}`。注释（翻译）：一个宕掉的嵌入端点必须把缓存降级为词法匹配，既不禁用它也不让请求失败。回退用与主嵌入相同的维度，这样已经存下的向量不会被静默错比——它们只是不再匹配，也就是一次 miss。`embName = primary.Name()`。
    - `config.EmbedProviderNone` → 什么都不建，`embName = "none"`。注释（翻译）：纯精确匹配缓存。仍然值得有：一个 agent 循环逐字重问同一个问题，比人们以为的频繁得多。
    - default → `nil, fmt.Errorf("server: cache: unsupported embedding provider %q", cc.Embedding.Provider)`。
  - 再选存储（`var store cache.Store; var storeName string`）：
    - `config.CacheStoreRedis` → `cache.NewRedisStore(cache.RedisOptions{Addr, Password, DB, Prefix, MaxEntriesPerScope, DialTimeout, ReadTimeout, WriteTimeout, PoolSize})`，出错返回 `nil, fmt.Errorf("server: cache: redis at %s: %w", cc.Redis.Addr, err)`。注释（翻译）：启动失败才是诚实的结果——一个为共享缓存配置、却静默跑在进程内存上的部署，会给不同副本不同答案，那比一次拒绝启动难诊断得多。
    - default → `cache.NewMemoryStore(cache.MemoryOptions{MaxEntriesPerScope})`，`storeName = store.Name()`。
  - `cfgCache := cache.Config{Enabled: true, Threshold: cc.Threshold, TTL: cc.TTL.Duration(), MaxEntriesPerScope, MinPromptChars, AllowNondeterministic, AllowTools, Now: time.Now}`。
  - `logger != nil` 时 `logger.Info("semantic cache enabled", "store", storeName, "embedding", embName, "threshold", cc.Threshold, "ttl", cc.TTL.String())`。
  - 返回 `cache.New(cfgCache, store, emb, loggerFrom(logger)), nil`。
  - 并发：构造期调用；Redis 存储内部有自己的连接池与后台 janitor（本册不展开）。
- `func (s *Server) handleCache(w http.ResponseWriter, r *http.Request)` (internal/server/cache.go)。`s.cache == nil` → 200 `{"enabled": false}`；否则取 `cfg := s.cache.Config()`、`stats := s.cache.Stats()`、`storeStats := s.cache.Store().Stats()`，`mode := embedMode(s.cache)`，并用一次接口断言探测存储是否实现 `Scopes(context.Context) (map[string]int, error)`：实现了就调用，`err == nil` 才采用（否则留空 map，**不报错**）。响应体：`enabled`、`store`、`config{threshold, ttl, max_entries_per_scope, min_prompt_chars, allow_nondeterministic, allow_tools, embedding}`、`stats{lookups, hits, exact_hits, semantic_hits, misses, stores, store_errors, lookup_errors, stale_evictions, collapsed, hit_rate, saved_prompt_tokens, saved_completion_tokens}`、`store_stats`、`scopes`。注释（翻译）：一次命中省下的令牌报在这里，而不并进上游 token 总数——后者必须继续描述 provider 真正产出了什么。
- `func (s *Server) handleCacheFlush(w http.ResponseWriter, r *http.Request)` (internal/server/cache.go)。`s.cache == nil || !s.cache.Config().Enabled` → 200 `{"flushed": 0, "enabled": false}`；`scope := r.URL.Query().Get("scope")`；`s.cache.Flush(r.Context(), scope)` 出错 → 502 体 `{"error":{"message": err.Error(),"type":"infergate_cache_error"}}`；成功打 `s.log.Info("cache flushed", "scope", ...)`（空 scope 记 `"all"`，键 `entries`）并 200 `{"flushed": n, "scope": scope}`。注释（翻译）：它是带可选 `?scope=` 的 POST；在 GET 上清空一切会让爬虫或链接预取清空一个共享缓存。
- `func (s *Server) handleCacheLookup(w http.ResponseWriter, r *http.Request)` (internal/server/cache.go)。`q.Get("prompt")` 为空 → 400 体 `{"error":{"message":"prompt is required","type":"infergate_bad_request"}}`；缓存 nil 或未启用 → 200 `{"enabled": false, "prompt": prompt}`；`scope := q.Get("scope")`；`emb := s.cache.Embedder()`，为 nil → 200 `{"enabled": true, "prompt": ..., "scope": ..., "features": embedFeatures(prompt), "matches": []any{}, "note": "no embedder configured: this cache is exact-match only"}`；否则 `emb.Embed(r.Context(), []string{prompt})`，出错或空向量 → 200 带 `"note"` 为 `err.Error()` 或 `"embedder unavailable"`、`"matches": []`（**不报 5xx**：这是一个解释型端点）；`threshold := s.cache.Config().Threshold`，`?threshold=` 能 `parseFloat` 成功才覆盖（失败保留配置值）；`s.cache.Store().Search(r.Context(), scope, vecs[0], threshold, 5)` 出错 → 502 `infergate_cache_error`；命中按 `Similarity` 降序 `sort.SliceStable`；内部类型 `matchView`（JSON 键 `key`、`similarity`、`model`、`prompt`、`age_seconds`、`hits`、`expires_in`），其中 `age_seconds = time.Since(m.Entry.CreatedAt).Seconds()`、`expires_in = time.Until(m.Entry.ExpiresAt).Round(time.Second).String()`；200 体 `{enabled, prompt, scope, threshold, features, matches, note: "matches are limited to one scope: a scope is a tenant plus a model"}`。注释（翻译）：这个端点存在，是因为语义缓存从外面否则不可能调试——"它没命中"和"它匹配错了条目"在响应体里长得一样，而阈值是一个没人能靠读配置选出来的数字。
- `func (s *Server) Cache() *cache.Cache` (internal/server/cache.go)。返回 `s.cache`，为测试与关闭而导出。
- `func (s *Server) CloseCache() error` (internal/server/cache.go)。`s.cache == nil` → nil；否则 `s.cache.Close()`（释放 Redis 池、停掉 janitor goroutine）。注释（翻译）：在监听器停止之后调用，所以没有请求还在用它。
- `func embedMode(c *cache.Cache) map[string]any` (internal/server/cache.go)。`emb == nil` → `{"provider": "none", "dims": 0, "lexical": true}`；嵌入器是 `*embed.Fallback` 且 `Primary != nil` → `{"provider": name, "dims": emb.Dims(), "model": wrapper.Primary.Name(), "fallback": "hashing", "note": "an embedding outage degrades matching to lexical", "lexical": false, "enabled": cfg.Enabled}`；否则 `{"provider": name, "dims": emb.Dims(), "lexical": true}`。
- `func embedFeatures(prompt string) []string` (internal/server/cache.go)。`embed.Features(prompt)`，长度超过 64 时截断到前 64 个。注释（翻译）：暴露词法嵌入器所依据的那些 token/哈希，让一个令人意外的相似度能被解释，而不是被争辩。
- `func hitRate(s cache.Stats) float64` (internal/server/cache.go)。`s.Lookups == 0` → 0；否则 `float64(s.Hits) / float64(s.Lookups)`。无错误路径。被 `statsCache`、`handleCache`、`handleMetrics` 的缓存块调用。
- `func parseFloat(raw string) (float64, error)` (internal/server/cache.go)。`strconv.ParseFloat(strings.TrimSpace(raw), 64)`。注释（翻译）：一个畸形值要被报出来，好让 handler 退回配置值而不是退到 0——退到 0 会静默把每一次查找都变成命中。

### 3.4 internal/server/quota.go

文件顶部注释（翻译保留）：像缓存一样，配额管理器在这里建而不是在网关里建：它是与 Redis 说话、并且持有跨单个请求生命周期的计数器的组件。网关收到管理器并对它调用 Admit / Settle / Release，这让请求路径免于配置翻译，也让每一个策略决策无需服务器即可测试。

- `func buildQuota(cfg *config.Config, logger logAdapter) (*quota.Manager, string, error)` (internal/server/quota.go)。行为：
  - `qc := cfg.Quota`；把 `qc.Tenants` 逐项转成 `policies[t.Tenant] = quotaPolicy(t)`；`def := quotaPolicy(qc.DefaultPolicy)`。
  - `cfgQuota := quota.Config{Enabled: qc.Enabled, Prefix: qc.Redis.Prefix, FailOpen: qc.FailOpen, DefaultPolicy: def, Tenants: policies}`；`logger != nil` 时 `cfgQuota.Log = loggerFrom(logger)`。
  - `!qc.Enabled` → `return quota.New(cfgQuota, nil), "disabled", nil`（**注意存储名是 `"disabled"`**，而 store 是 nil）。
  - `config.QuotaStoreRedis` → `quota.NewRedisStore(quota.RedisOptions{Addr, Password, DB, DialTimeout, ReadTimeout, WriteTimeout, PoolSize})`，出错返回 `nil, "", fmt.Errorf("server: quota: redis at %s: %w", qc.Redis.Addr, err)`。注释（翻译）：理由同共享缓存——一个为集群级预算配置、却静默执行进程级预算的部署，会让 N 个副本各自花掉整份预算，而超支要到发票到了才可见。成功后 `logger.Info("quota enforcement enabled", "store", "redis", "addr", qc.Redis.Addr, "tenants", len(policies), "fail_open", qc.FailOpen)`，返回 `quota.New(cfgQuota, rs), "redis", nil`。
  - default → `logger.Info("quota enforcement enabled", "store", "memory", "tenants", len(policies), "fail_open", qc.FailOpen)`，返回 `quota.New(cfgQuota, quota.NewMemoryStore()), "memory", nil`。注释（翻译）：内存是单进程的诚实存储，也是测试套件用的那个。它在 `/admin/quota` 里被刻意如此报告：在进程内存里执行的预算是**每副本**预算，读这页的运维者需要知道这一点。
  - 构造期调用，无锁。
- `func quotaPolicy(t config.TenantQuotaConfig) quota.Policy` (internal/server/quota.go)。`var micros int64`；`t.CostPerDayUSD > 0` 时 `micros = int64(math.Round(t.CostPerDayUSD * 1_000_000))`；返回 `quota.Policy{Tenant, TokensPerDay, CostPerDayMicros: micros, RequestsPerMinute, TokensPerSession, OnExceed, DowngradeModel, MaxTokensCap, AnomalyRatio}`。注释（翻译）：配置说美元，因为那是运维者做预算的单位；管理器数微美元，因为那是能在整数运算中活下来的单位。转换恰好发生在这里——而且只在这里。调用者：`buildQuota`、`handleQuota`、`quotaTenantsJSON`。
- `func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request)` (internal/server/quota.go)。`s.quota == nil` → 200 `{"enabled": false}`；否则 `stats := s.quota.Stats()`、`cfg := s.cfg.Quota`，组装 `body`：`enabled` = `s.quota.Enabled()`、`store` = `s.quotaStore`、`fail_open`、`config{estimate_completion_tokens, estimate_chars_per_token, anomaly_ratio, default_policy: quotaPolicyJSON(quotaPolicy(cfg.DefaultPolicy)), tenants: quotaTenantsJSON(cfg)}`、`stats{allowed, degraded, rejected, store_errors, alerts, reserved_tokens, settled_tokens, released_tokens, overshoot_tokens, overshoot_cost_micros, released_cost_micros}`。`tenant := r.URL.Query().Get("tenant")` 为空则直接 200 body。否则 `s.quota.Report(r.Context(), tenant, r.URL.Query().Get("session"))`，出错 → 502 体 `{"error":{"message": err.Error(),"type":"infergate_quota_error"}}`；成功写 `body["report"]`：`tenant`、`day`、`tokens_today`、`cost_today_micros`、`cost_today_usd`（`float64(report.CostTodayMicro) / 1_000_000`）、`minute`、`requests_this_minute`、`session`、`session_tokens`、`baseline_tokens`、`ratio`、`alerting`、`policy`，然后 200。注释（翻译）：否则一个预算系统从外面是不可审计的——"租户说它被拒了"与"租户说它远没到限额"在有人能读到做决定的那个计数器之前是同一种说法，所以 `?tenant=` 报表是主视图而不是附加项。另外注释说明：出错之所以报 502，是因为报表读计数器，这里失败与让准入 fail closed 是同一种状况——如实报出，而不是假装该租户没花钱。
- `func quotaTenantsJSON(cfg config.QuotaConfig) []map[string]any` (internal/server/quota.go)。先收集 `names` 并 `sort.Strings`，再用 `byName` map 回查，按名输出 `quotaPolicyJSON`。注释（翻译）：按稳定顺序列出配置的租户，好让 admin 页在两次读取之间可以做 diff。纯读。
- `func quotaPolicyJSON(p quota.Policy) map[string]any` (internal/server/quota.go)。键：`tenant`、`tokens_per_day`、`cost_per_day_usd`（微美元除 1_000_000）、`cost_per_day_micros`、`requests_per_minute`、`tokens_per_session`、`on_exceed`、`downgrade_model`、`max_tokens_cap`、`anomaly_ratio`、`unbounded` = `!p.Budgeted()`、以及 `limits` 子对象四个布尔（`tokens_per_day`、`cost_per_day`、`requests_per_minute`、`tokens_per_session` 各自 `> 0`）。注释（翻译）：这些布尔是让页面可读的东西——一份全零策略与一份加载失败的策略长得一模一样，而"无上限"对一个还没人做预算的租户是正当且必要的答案。
- `func (s *Server) CloseQuota() error` (internal/server/quota.go)。`s.quota == nil` → nil；否则 `s.quota.Close()`。注释（翻译）：在监听器停止之后调用，理由与 CloseCache 相同——一个还在被飞行中请求写入的计数器，不该在它脚下把连接池拆掉。

### 3.5 internal/server/m6.go

文件顶部注释（翻译保留）：这里有三个面，它们回答一个 agent 运行时无法从响应本身回答的三个问题——"我的客户端中途超时了；如果我再发一次，它会跑两次吗？"对应幂等存储，经 `/admin/idempotency` 读回；"这段对话花了多少钱，花在哪些后端上？"对应按会话台账，经 `/admin/sessions` 读；"有没有哪个后端真的能接受工具调用／图像／JSON 模式？"对应 `/v1/capabilities`（声明）与 `/v1/capabilities/probe`（实测）。三者都建在这里而不是网关里，理由同缓存与配额管理器：它们拥有跨请求的状态，问的是进程配置了什么，而不是请求路径。

- `const maxAdminLimit = 500` (internal/server/m6.go)。注释（翻译）：限制单次 admin 列举最多返回多少。存储本来就有界，这只防止调用方把调试端点变成一整块内存转储。
- `func buildIdempotency(cfg *config.Config, logger logAdapter) *idempotency.Store` (internal/server/m6.go)。`ic := cfg.Idempotency`；`idempotency.New(idempotency.Options{Capacity: ic.Capacity, TTL: ic.TTL.Duration(), MaxResponseBytes: ic.MaxResponseBytes})`；`logger != nil` 时按 `ic.Enabled` 取 `state`（`"enabled"`/`"disabled"`），`logger.Info("idempotency replay "+state, "capacity", store.Capacity(), "ttl", store.TTL().String(), "max_response_bytes", store.MaxResponseBytes())`；返回 store。**无错误路径**（这个存储不需要外部连接）。注释（翻译）：禁用的存储仍然存在，这样 `/admin/idempotency` 能报告配置——"重放开着吗、开多久"是重复计费之后的第一问，而回答它绝不能靠读主机上的配置文件。它也从不被交给代理，所以禁用的那个每请求零成本。
- `func buildSessions(cfg *config.Config, logger logAdapter) *sessions.Ledger` (internal/server/m6.go)。`sessions.New(sessions.Options{Capacity: sc.Capacity, TTL: sc.TTL.Duration(), RecentPerSession: sc.RecentPerSession})`；`logger != nil` 时 `logger.Info("session ledger "+state, "capacity", ledger.Capacity(), "ttl", ledger.TTL().String(), "recent_per_session", ledger.RecentPerSession())`。
- `func modelInfo(cfg *config.Config) map[string]gateway.ModelInfo` (internal/server/m6.go)。`len(cfg.Models) == 0` → `nil`（**注意是 nil 而不是空 map**）；否则逐项转 `gateway.ModelInfo{ContextWindow, MaxOutputTokens, Capabilities, Notes}`。注释（翻译）：两个类型在结构上刻意一致——网关不能依赖 config 包，而一个映射函数比一个会把配置拖进请求路径的共享类型便宜。调用者：`NewServer (internal/server/server.go)` 的 `gateway.Options.Models`。
- `func adminLimit(r *http.Request, def int) (int, error)` (internal/server/m6.go)。`raw := strings.TrimSpace(r.URL.Query().Get("limit"))`；空 → `def, nil`；`strconv.Atoi` 出错 → `0, fmt.Errorf("limit must be an integer, got %q", raw)`；`n <= 0` → `0, fmt.Errorf("limit must be positive, got %d", n)`；`n > maxAdminLimit` 则钳到 500；返回 `n, nil`。调用者：`handleIdempotency`（def 50）、`handleSessions`（def 50）。注意它是**钳制而不是报错**，与 `showTraceLimit` 的行为一致。测试侧由 `TestM6AdminSurfacesReportConfigurationWhenDisabled` 与 `TestM6MetricsExposed` 间接覆盖。
- `func (s *Server) handleIdempotency(w http.ResponseWriter, r *http.Request)` (internal/server/m6.go)。`adminLimit(r, 50)` 出错 → `writeError(w, http.StatusBadRequest, "infergate_bad_request", err.Error())`；`entries := store.List(limit)`，为 nil 时替换成 `[]idempotency.Entry{}`（**保证 JSON 是 `[]` 而不是 `null`**）；200 体键：`enabled`（`s.cfg.Idempotency.Enabled`）、`capacity`、`ttl`、`max_response_bytes`、`stored` = `store.Len()`、`in_flight` = `store.InFlight()`、`scopes` = `store.ScopeCount()`、`stats` = `store.Stats()`、`count` = `len(entries)`、`entries`。注释（翻译）：条目被列出来，是因为重复计费是靠找到那个干了活的 key 来诊断的，不是靠猜——响应头里写着原始 request id，而这个端点就是客户端把它丢掉之后还能把它查回来的地方。
- `func (s *Server) handleIdempotencyFlush(w http.ResponseWriter, r *http.Request)` (internal/server/m6.go)。`scope := strings.TrimSpace(r.URL.Query().Get("scope"))`；`flushed := s.idempotency.Flush(scope)`；`target := scope`，为空则 `"all"`；200 `{"flushed": flushed, "scope": target}`。注释（翻译）：`?scope=` 把它收窄到一个租户，这才让这个端点在不驱逐其它租户可重放工作的前提下可用。
- `func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request)` (internal/server/m6.go)。`adminLimit(r, 50)` 出错 → 400 `infergate_bad_request`；`list := ledger.List(limit)`；`tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))` 非空时按 `sess.Tenant == tenant` 过滤（保持原顺序，`make(..., 0, len(list))` 预分配）；`list == nil` 时替换成 `[]sessions.Session{}`；200 体键：`enabled`、`capacity`、`ttl`、`recent_per_session`、`tracked` = `ledger.Len()`、`tenant`、`tenants` = `ledger.Tenants()`、`stats` = `ledger.Stats()`、`count`、`sessions`。注释（翻译）：它按最近活动优先列出被跟踪的对话，并带上按租户的汇总——那正是成本复盘实际开始的地方。
- `func (s *Server) handleSessionByID(w http.ResponseWriter, r *http.Request)` (internal/server/m6.go)。`id := strings.TrimSpace(r.PathValue("id"))`；为空 → 400 `infergate_bad_request` / `session id is required`。`tenant` 非空时走 `s.sessions.Get(tenant, id)`：不中 → 404 `infergate_not_found` / `session not found: <id>`；命中 → 200 整个 session 对象。否则 `matches := s.sessions.Find(id)` 按长度分派：0 → 404 `infergate_not_found` / `session not found: <id>`；1 → 200 `matches[0]`；default → 409 `infergate_ambiguous_session`，消息为 `fmt.Sprintf("session id %q exists under %d tenants (%s); pass ?tenant= to choose one", id, len(matches), strings.Join(tenants, ", "))`。注释（翻译）：光有 id 不是键——台账按租户分区，所以同一个 session id 在两个租户下是两段不同的对话。给 `?tenant=` 能解析它；不给而恰好只找到一个匹配也能解析；找到多个则报成歧义而不是猜，因为猜会把另一个租户的花费递给运维者。
- `func (s *Server) handleSessionsFlush(w http.ResponseWriter, r *http.Request)` (internal/server/m6.go)。`flushed := s.sessions.Flush()`；200 `{"flushed": flushed}`。
- `func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request)` (internal/server/m6.go)。`rep := s.proxy.CapabilityReport()`；200 体 `{generated_at, capabilities, models, upstreams, model_count: len(rep.Models), upstream_count: len(rep.Upstreams)}`。注释（翻译）：它刻意是一个无参数的端点——一个 agent 运行时在构造请求之前先来问它，而 GET 能被一个即将发几千次请求的客户端缓存。
- `func (s *Server) handleCapabilityProbe(w http.ResponseWriter, r *http.Request)` (internal/server/m6.go)。`io.ReadAll(io.LimitReader(r.Body, 1<<20))`（**1 MiB 上限**）出错 → 400 `infergate_bad_request` / `"read request body: " + err.Error()`；`gateway.ProbeJSONReader(body)` 出错 → 400 / `"decode probe request: " + err.Error()`；`s.proxy.ProbeCapabilities(r.Context(), req)` 出错时先判 `errors.Is(err, gateway.ErrUnknownCapability)`：是 → 400 `infergate_bad_request`（注释翻译：未知能力名是调用方的错，不是网关故障，消息里列出了可以探测什么），否则 500 `infergate_internal`；成功统计 `counts[p.Outcome]++` 与 `accepted`；200 体 `{requested: req, count, accepted, outcomes, probes, note: "accepted means the backend did not reject the request shape; it is not a statement about answer quality"}`。注释（翻译）：它是 POST，因为它有 body（探测哪些能力、哪个模型、哪些后端），也因为它在要紧的意义上**不是幂等**的：它会花掉真实的 provider 调用。一个爬虫或预取器能触发的 GET 会成为意外给运维者计费的方式。
- `func writeM6Metrics(b *strings.Builder, store *idempotency.Store, ledger *sessions.Ledger)` (internal/server/m6.go)。`store != nil` 时先写 10 条 counter 的 HELP/TYPE 与样本：`infergate_idempotency_lookups_total`、`_hits_total`、`_misses_total`、`_conflicts_total`、`_in_flight_rejects_total`、`_stored_total`、`_oversize_total`、`_aborted_total`、`_evicted_total`、`_expired_total`；再写 3 条 gauge：`infergate_idempotency_entries`（`store.Len()`）、`_in_flight`（`store.InFlight()`）、`_scopes`（`store.ScopeCount()`），counter 的值来自 `store.Stats()`。`ledger != nil` 时写 4 条 counter：`infergate_session_records_total`、`_no_id_total`、`_evictions_total`、`_expirations_total`；2 条 gauge：`infergate_sessions_tracked`、`infergate_sessions_tenants`；3 条**gauge** `infergate_sessions_requests`、`infergate_sessions_cost_usd`、`infergate_sessions_tokens{kind="prompt"|"completion"|"cached"}`，其值由 `for _, sess := range ledger.List(0)` 累加 `Requests`、`CostUSD`、`PromptTokens`、`CompletionTokens`、`CachedTokens` 得到。注释（翻译）两条：①基数就是这些家族不带租户标签的原因。一个每进程存储上的租户维度会把调用方发明的每一个租户 id 变成一条新的时间序列，而租户头是调用方控制的；按租户的细节在 `/admin/idempotency` 与 `/admin/sessions` 里。②后三条是 gauge 而不是 counter，这个区分是承重的：一个在容量处被驱逐的会话会把它对应的请求数与花费从总和里带走，所以这里一个 counter 会**下降**，而 Prometheus 的 counter 不允许下降。测试：`TestM6MetricsExposed` (internal/server/m6_test.go)。

### 3.6 internal/server/tracing.go

- `const defaultTraceCapacity = 1024` (internal/server/tracing.go)。注释（翻译）：它被复制而不是从 config 包导入，是因为一个在 Go 里构造 Config 的调用方（本包的每个测试）从不跑 `config.Validate`，所以一个零容量能到达这个文件——而容量为 0 的存储会静默丢弃每一条 trace，同时配置上说 tracing 是开的，那正是唯一一种看起来跟成功一模一样的失败模式。被 `buildTracing` 与 `traceCapacity` 用作兜底。
- `const maxTraceLimit = 500` (internal/server/tracing.go)。注释（翻译）：环最多几千条，但每条 trace 都是一个带 span、event 与 attribute 的嵌套 JSON 文档；一个要全部的客户端通常是一个本意 `limit=1` 的脚本，否则会拉下若干兆字节。被 `traceLimit` 用作钳制上限。
- `type tracePlane struct { cfg config.TracingConfig; store *tracing.Store; tracer *gateway.Tracer; exporters []gateway.TraceExporter; closers []namedCloser; report []traceReporter }` (internal/server/tracing.go)。注释（翻译）：它存在的理由与缓存、配额平面相同——在 `NewServer` 里建一次，即使禁用也挂在 `Server` 上，好让 `/admin/traces` 能报告配置，并在监听停止后由 `main` 关闭。store 在 tracing 关闭时**刻意不分配**：空环很便宜，但它仍要占一次容量槽的分配，而且更糟的是，它会让一个配置错误的网关与一个正在录制的网关无法区分。
- `type namedCloser struct { name string; close func() error }` (internal/server/tracing.go)。注释（翻译）：把导出器的名字放在它的关闭器旁边，于是一次 flush 失败能说出是哪个 sink 失败了。
- `type traceReporter struct { name string; stats func() map[string]any }` (internal/server/tracing.go)。注释（翻译）：这是导出器的只读一半。它与 `gateway.TraceExporter` 接口刻意分开——请求路径只调用 `Export()`，给那个接口加报告方法会逼每一个导出器（包括测试替身）去实现没人读的计数器。
- `func (t *tracePlane) exportStats() map[string]any` (internal/server/tracing.go)。逐条 `report`：`stats` 为 nil 跳过，否则 `out[r.name] = r.stats()`。纯读。调用者：`handleTracingConfig`。
- `func buildTracing(cfg *config.Config, logger logAdapter) (*tracePlane, error)` (internal/server/tracing.go)。`tc := cfg.Tracing`；`tp := &tracePlane{cfg: tc}`；`!tc.Enabled` 直接 `return tp, nil`（store 与 tracer 都是 nil——这就是"关闭时免费"的机制）。`capacity := tc.Capacity`，`<= 0` 时取 `defaultTraceCapacity`；`tp.store = tracing.NewStore(capacity)`；`ratio := tc.SampleRatio` 先钳下界 0 再钳上界 1；`buildTraceExporters(tc, logger)` 出错 `return nil, err`。注释（翻译）：建不出导出器对启动是致命的，理由同一个连不上的 Redis 缓存——一个配了 trace sink 的运维者必须在启动时得知它不可用，而不是几小时后看到一块空仪表盘。成功后赋 `tp.exporters`/`tp.closers`/`tp.report`，`tp.tracer = gateway.NewTracer(tp.store, ratio, exporters...)`；`logger != nil` 时 `logger.Info("tracing enabled", "capacity", capacity, "sample_ratio", ratio, "jsonl_path", tc.JSONLPath, "otlp_endpoint", tc.OTLP.Endpoint, "exporters", len(exporters))`。测试：`TestTracingRejectsBadOTLPEndpoint`、`TestTracingUnwritableJSONLPathFailsStartup` (internal/server/tracing_test.go)。
- `func (t *tracePlane) Close() error` (internal/server/tracing.go)。`t == nil` → nil（**接收者可以为 nil**）；遍历 `closers`，`close == nil` 跳过，首个错误用 `fmt.Errorf("%s: %w", c.name, err)` 记住但继续关其余的；返回 `firstErr` 或 nil。注释（翻译）：store 不需要清理。
- `func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request)` (internal/server/tracing.go)。`s.trace == nil || s.trace.store == nil` → 200（**不是错误**）体 `{"enabled": false, "capacity": s.traceCapacity(), "count": 0, "dropped": 0, "traces": []any{}}`。`traceLimit(r)` 出错 → `writeError(w, http.StatusBadRequest, "infergate_bad_request", err.Error())`。`summaries := store.List(limit)`；`?format=jsonl`（`strings.EqualFold`，大小写不敏感）时设 `Content-Type: application/x-ndjson; charset=utf-8`、`WriteHeader(http.StatusOK)`、`out := newFlusher(w)`，逐条 summary `store.Get(sum.TraceID)`：不命中 `continue`（注释翻译：在 List 与 Get 之间被驱逐，静默丢弃），`store.WriteJSONL(out, tr)` 出错直接 `return`（注释翻译：客户端中途走了），成功则 `out.Flush()`。默认渲染 200 体 `{enabled: true, capacity, stored: store.Len(), dropped: store.Dropped(), count: len(summaries), traces: summaries}`。注释（翻译）：两种渲染，而第二种正是这个端点在压测时重要的原因——`?format=jsonl` 把每条 trace 作为一行已 flush 的输出流出去，可以管道给 `jq` 或写进文件而不用缓冲整个环，而默认的 JSON 体是浏览器和验收脚本读的那个。
- `func (s *Server) handleTraceByID(w http.ResponseWriter, r *http.Request)` (internal/server/tracing.go)。`id := strings.TrimSpace(r.PathValue("id"))`；为空 → 400 `infergate_bad_request` / `trace id is required`；`!validTraceID(id)` → 400 `infergate_bad_request` / 原文 `trace id must be 32 hex characters (a trace id) or at most 64 characters from [A-Za-z0-9._:-] (a request id)`；`s.trace == nil || s.trace.store == nil` → 404 `infergate_not_found` / `tracing is disabled`；`store.Get(id)` 不中 → 404 `infergate_not_found` / `"trace not found: " + id`；`?format=jsonl` 时 ndjson 头加一次 `WriteJSONL(w, tr)`（**不经 flusher**，因为没有循环），否则 200 整条 trace。注释（翻译）：同时按 request id 匹配才是重点——一个手里只有 `X-InferGate-Request-Id` 响应头的调用方仍然必须能找到它所属的那条 trace，而要求先有 32 位十六进制 trace id 会让这个面在事故中变得没用。测试：`TestTraceLookupValidation` (internal/server/tracing_test.go)。
- `func (s *Server) handleTracingConfig(w http.ResponseWriter, r *http.Request)` (internal/server/tracing.go)。`var tc config.TracingConfig`，`s.trace != nil` 时 `tc = s.trace.cfg`；`body` 初值 `{enabled, capacity: s.traceCapacity(), sample_ratio, jsonl_path, otlp{endpoint, timeout, service_name, headers: len(tc.OTLP.Headers)}, stored: 0, dropped: 0, exporters: []string{}, export_stats: map[string]any{}}`；`s.trace != nil` 时把 `exporters` 填成 `report` 里的名字列表、`export_stats` = `s.trace.exportStats()`，store 非 nil 时填 `stored` 与 `dropped`；200。注释（翻译）：它与 `/admin/traces` 是两个端点，因为这两个问题在不同时刻被问——"tracing 开着吗、发去哪儿"是配置时问的，"它抓到了什么"是排查时问的。
- `func (s *Server) traceCapacity() int` (internal/server/tracing.go)。`s.trace == nil` → `defaultTraceCapacity`；`c := s.trace.cfg.Capacity`，`c > 0` 返回 `c`，否则 `defaultTraceCapacity`。
- `func (s *Server) CloseTracing() error` (internal/server/tracing.go)。`s.trace == nil` → nil；否则 `s.trace.Close()`。注释（翻译）：由 `main` 在监听停止之后、`CloseCache` 旁边调用：请求路径是 trace 的唯一生产者，所以到那时不会再有新的到来，一次有界 drain 就够了。
- `func traceLimit(r *http.Request) (int, error)` (internal/server/tracing.go)。`raw := strings.TrimSpace(r.URL.Query().Get("limit"))`；空 → `50, nil`；`strconv.Atoi` 出错 → `0, fmt.Errorf("limit must be an integer, got %q", raw)`；`n <= 0` → `0, fmt.Errorf("limit must be positive, got %d", n)`；`n > maxTraceLimit` 钳到 500；返回 `n, nil`。注意 HTTP 默认值是 50 而内部默认容量是 1024，两者不是一个东西。
- `func validTraceID(id string) bool` (internal/server/tracing.go)。`len(id) == 32` 时逐字节只允许 `0-9`、`a-f`、`A-F`，有一个不合规就 false，否则 true；`len(id) > 64` → false；其余长度逐字节限定在 `a-z`、`A-Z`、`0-9` 以及 `-`、`_`、`.`、`:`，其它一律 false。注释（翻译）：接受一个 32 位十六进制 trace id 或一个更松的 request id，并拒绝任何可以用来拿垃圾探测存储的东西。字符集刻意窄：request id 由本网关或由 provider 生成，所以字母、数字、点、短横、冒号与下划线覆盖了本仓库日志里出现过的每一种形状。纯函数，无锁。
- `type flusherWriter struct { w http.ResponseWriter; f http.Flusher }` (internal/server/tracing.go)。注释（翻译）：它是 JSONL 渲染所写的那个 `io.Writer`，让每条 trace 一编码完就到达客户端，而不是等 handler 返回；一份 trace 列表正是有人会 `tail` 的那种响应。
- `func newFlusher(w http.ResponseWriter) *flusherWriter` (internal/server/tracing.go)。`f, _ := w.(http.Flusher)`（**断言失败时 f 为 nil**，不报错）；返回 `&flusherWriter{w: w, f: f}`。
- `func (f *flusherWriter) Write(p []byte) (int, error)` (internal/server/tracing.go)。直接 `return f.w.Write(p)`。
- `func (f *flusherWriter) Flush()` (internal/server/tracing.go)。`f.f != nil` 时才 `f.f.Flush()`——这就是断言失败不报错的原因。并发：`http.ResponseWriter` 不保证并发安全，调用方（`handleTraces`）是单 goroutine 顺序写。
- `func writeError(w http.ResponseWriter, status int, kind, message string)` (internal/server/tracing.go)。`writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": kind}})`。注释（翻译）：发出与 `/admin` 其余部分相同的错误信封。调用者：`handleIdempotency`、`handleSessions`、`handleSessionByID`、`handleCapabilityProbe`、`handleTraces`、`handleTraceByID`。

### 3.7 internal/server/tracing_export.go

- `func buildTraceExporters(tc config.TracingConfig, logger logAdapter) ([]gateway.TraceExporter, []namedCloser, []traceReporter, error)` (internal/server/tracing_export.go)。四个返回值，**三个切片加一个错误**，失败时三个切片都是 nil。
  - 注释（翻译）：两个 sink 都是可选、彼此独立的——一个部署可能想要一份用于调试的本地 JSONL、一个用于共享仪表盘的 OTLP 收集器，或者两个都要。它们各自拥有一个后台 worker，所以这里在请求路径上既不碰网络也不碰文件系统：`Export()` 只入队。一个配好了却建不起来的 sink 是错误而不是警告；另一种选择是一个网关愉快地服务流量，而运维者配置的 trace 管道什么都不写。
  - `tc.JSONLPath != ""` 时 `traceexport.NewJSONL(traceexport.JSONLOptions{Path: path, Logger: loggerFrom(logger)})`，出错 `return nil, nil, nil, fmt.Errorf("server: tracing: jsonl export: %w", err)`；成功追加 exporter、`namedCloser{name: "jsonl", close: exporter.Close}`、以及一个 `traceReporter{name: "jsonl", stats: ...}`，其统计为 `{"path": path, "written": exporter.Written(), "dropped": exporter.Dropped(), "error": errString(exporter.Err())}`。注释（翻译）：`Err()` 是写入器最后一次失败（磁盘满、fd 被关）。它被报出而不是每次丢弃都记日志，因为一个失败的 sink 是成串失败的，每次丢一行本身就是一场洪泛。
  - `tc.OTLP.Endpoint != ""` 时 `traceexport.NewOTLP(traceexport.OTLPOptions{Endpoint, Timeout: tc.OTLP.Timeout.Duration(), ServiceName: tc.OTLP.ServiceName, Headers: tc.OTLP.Headers, Logger: loggerFrom(logger)})`；出错时**先把 `closers` 里已有的全部关掉**，再 `return nil, nil, nil, fmt.Errorf("server: tracing: otlp export: %w", err)`。注释（翻译）：放弃之前把已经存在的东西关掉，否则一个 JSONL worker 会为一个永远不会被建成的服务器继续跑。成功追加 `namedCloser{name: "otlp", close: exporter.Close}` 与 `traceReporter{name: "otlp", stats: ...}`，其统计为 `{"endpoint": endpoint, "exported": st.Exported, "failed": st.Failed, "dropped": st.Dropped, "last_error": st.LastError, "last_status": st.LastStatus}`。
  - 成功返回 `exporters, closers, report, nil`。调用者：`buildTracing`。
- `func errString(err error) string` (internal/server/tracing_export.go)。`err == nil` → `""`；否则 `err.Error()`。**为什么要这个函数**：它的返回值进的是 JSON 体，nil 错误必须变成空字符串而不是 `"<nil>"`，而且不能把 nil 接口解引用。被 JSONL 报告的闭包调用。

### 3.8 internal/server/log.go

- `func loggerFrom(l logAdapter) *slog.Logger` (internal/server/log.go)。`l == nil` → `slog.Default()`；`if s, ok := l.(*slog.Logger); ok` 成功 → **直接返回 s**；否则 `slog.New(&adapterHandler{target: l})`。注释（翻译）：`Options.Logger` 是 `*slog.Logger`，但服务器刻意只依赖三方法的 `logAdapter` 接口，这让服务器可以用桩来测试；`loggerFrom` 桥接两者——一个真的 `*slog.Logger` 直接穿过（用它的 Attr 值而不是 fmt 字符串，好让 JSON 日志保住自己的键），别的东西被包进一个最小的 `slog.Handler`，把属性折进消息成为 key=value 对。调用者：`NewServer`、`buildCache`、`buildQuota`、`buildTraceExporters`（各一次）。并发：返回的 logger 可被并发使用；`adapterHandler` 的方法都在拷贝上操作。
- `type adapterHandler struct { target logAdapter; attrs []any; group string }` (internal/server/log.go)。可变状态：`attrs` 与 `group` 都由 `WithAttrs`/`WithGroup` 通过**复制结构体**扩展，原值从不被改写。
- `func (h *adapterHandler) Enabled(context.Context, slog.Level) bool { return true }` (internal/server/log.go)。恒真：过滤交给被包装的 `logAdapter`。无锁。
- `func (h *adapterHandler) Handle(_ context.Context, r slog.Record) error` (internal/server/log.go)。`args` 预分配 `len(h.attrs) + 2*r.NumAttrs() + 2`；`group != ""` 时先加 `"logger", h.group`；再追加 `h.attrs...`；再 `r.Attrs(func(a slog.Attr) bool { args = append(args, a.Key, a.Value.Any()); return true })`；然后按级别分派：`r.Level >= slog.LevelError` → `target.Error`、`>= slog.LevelWarn` → `target.Warn`、否则 `target.Info`，都带上 `r.Message` 与 `args...`；`return nil`（**永不返回错误**：底层 logger 没有错误返回，硬造一个会让 slog 打一条多余的内部错误）。无锁（只读接收者字段）。
- `func (h *adapterHandler) WithAttrs(attrs []slog.Attr) slog.Handler` (internal/server/log.go)。`next := *h`（复制）；`next.attrs = append(append([]any{}, h.attrs...), flatten(attrs)...)`（**先拷一份再加**，避免与原切片共享底层数组）；返回 `&next`。
- `func (h *adapterHandler) WithGroup(name string) slog.Handler` (internal/server/log.go)。`name == ""` 时原样返回 `h`（不复制）；否则 `next := *h; next.group = name; return &next`。注意 `group` 是**单值**而不是栈：嵌套 group 会覆盖，这是与完整 slog 语义的已知差异。
- `func flatten(attrs []slog.Attr) []any` (internal/server/log.go)。`make([]any, 0, 2*len(attrs))`，逐项 `append(out, a.Key, a.Value.Any())`。纯函数。

TODO: 剩余要点清单（供续写走读员按序补写，均已有源码事实，勿再回读时可核对 internal/server/*.go）——(1) 第 4 节"完整路由总表"：22 条 `mux.HandleFunc` 逐行表格（路径/方法/处理函数/是否被 access 默认保护/说明），加 `mux.Handle("/", proxy)` 兜底；默认受保护面 = `config.DefaultAccessProtect()` 返回的 `/admin` 与 `/stats`，未受保护 = `/healthz`、`/readyz`、`/metrics`、`/v1/capabilities`、`/v1/capabilities/probe`、`/v1/*` 代理面；并说明 `/admin/*` 默认**无鉴权**这一事实与"未配置令牌时整条链消失"的关系。(2) 第 5 节 `/metrics` Prometheus 文本 exposition 规约：`Content-Type: text/plain; version=0.0.4; charset=utf-8`；HELP/TYPE 必须紧贴其家族、TYPE 在同一指标名第一次出现之前（`infergate_stream_frames_total` 与 `infergate_stream_bytes_total` 两个家族先连续声明再交错发样本的原因）；直方图家族 = 每边界一条 `_bucket{...,le="…"}`（含 `+Inf`，来自 `Bucket.Le()` 与 `metrics.cumulativeBuckets`）+ 一条 `_count` + 一条 `_sum`，空 series 仍发 HELP/TYPE；`writeHistogramFamily`/`labelSuffix`/`writeProcessMetric`/`writeRuntimeMetrics` 已在 3.1 逐符号覆盖，此处只补规约与家族清单表；家族清单：`infergate_build_info`、`infergate_requests_total`、`infergate_request_duration_seconds`(hist)、`infergate_upstream_attempt_duration_seconds`(hist)、`infergate_tokens_total{kind=prompt|completion|cached|requests}`、`infergate_completion_tokens_per_request`(hist)、`infergate_stream_frames_total`、`infergate_stream_bytes_total`、`infergate_first_token_seconds_mean`(gauge)、`infergate_first_token_seconds`(hist)、`infergate_failovers_total`、`infergate_breaker_state`（one-hot gauge，对 closed/half-open/open 三态各发一条）、`infergate_breaker_trips_total`、`infergate_breaker_rejections_total`、`infergate_breaker_failure_ratio`、cache 十一条、quota 七条、M6 十三条（见 3.5）、runtime 十三条（见 3.1）；附 `internal/metrics/histogram.go` 的三组默认桶（duration `{0.001,0.0025,0.005,0.01,0.025,0.05,0.1,0.25,0.5,1,2.5,5,10}`、first-token `{0.005,0.01,0.025,0.05,0.1,0.25,0.5,1,2.5}`、tokens-per-request `{1,8,16,32,64,128,256,512,1024,2048,4096}`）与"为何几何间距/为何 2 的幂"的注释翻译；`TestMetricsExpositionIsValid` 检查的重复家族陷阱（旧版曾把 `infergate_request_duration_seconds_sum` 当独立 counter 导出，两者同时存在会让整次 scrape **非法**）。(3) 第 6 节 `/stats` JSON 结构：逐键表格（`series` 八个子键、`requests`、`latency` 八键含 `window`/`dropped` 的语义、`tokens`、`first_token_mean`、`streams`、`cache`、`quota`、`uptime`），并注明 `entries` 在存储不可用时是 `-1` 哨兵、`cache`/`quota` 关闭时是 `{"enabled": false}` 而非缺键；`DefaultLatencyWindow = 65536`（internal/metrics/metrics.go）与 `/stats` 每次抓取只排一次序的取舍。(4) 第 7 节中间件顺序与超时取值：`access.middleware` 外层、`withAccessControl` 内层、`ServeMux` 最内层的三层图与"先令牌后体积"的论证；`ReadHeaderTimeout` 10s（防 Slowloris，只在请求头阶段计时）、`IdleTimeout` 90s（keep-alive 连接在两次请求之间无在飞请求的界限）、`UpstreamTimeout` 10m（按次尝试，属于网关而非 `http.Server`）、`MaxBodyBytes` 8<<20（可由 `INFERGATE_MAX_BODY_BYTES` 覆盖，非正整数报错）四个默认值来自 `config.Defaults() (internal/config/config.go)`；**无 `ReadTimeout`、无 `WriteTimeout`** 的原因逐条翻译保留（LLM 响应合理地要几分钟；`WriteTimeout` 会静默把长流截在半句话中间）。(5) 第 8 节访问控制判定分支汇总表与 401 语义：`credential` 取凭据的五个分支、`authorized` 常量时间比较、`reject` 的响应头与响应体原文；**缺失令牌与错误令牌都是 401 且 body 完全相同**的两个原因（不区分 403 以免告知探测者"你猜中了一个真端点只是格式不对"；body 不回显收到的值因为被拒的值可能是别的服务的有效令牌，而响应体最容易进日志），以及 `WWW-Authenticate` 只在 `bearer` 为真时才发（自定义头不发 Bearer 挑战）。(6) 第 9 节测试反推意图：四个测试文件逐测试函数一句话意图（access_test.go 十一个、server_test.go 九个加两个 benchmark、tracing_test.go 八个、m6_test.go 七个），重点标注 `TestAccessDisabledMatchesTheM0M6Behaviour` 的兼容性承诺、`TestAccessProtects...` 里"带对令牌却期望 400/404"的设计、`TestAccessAcceptsAnyConfiguredToken` 关于"空白用例必须走查询参数，因为 Go 头解析器先剥空白"的注释、`TestNewServerFailsWhenQuotaRedisIsDown` 关于 fail-open/fail-closed 两种都更糟的论证。(7) 未能确认项集中列出（例如 `main` 中关闭四连调用的确切代码位置与顺序、`internal/gateway` 侧对 `access.middleware` 的假设、OTLP/JSONL 导出器内部实现）。




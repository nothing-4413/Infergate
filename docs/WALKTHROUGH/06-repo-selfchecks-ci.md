# 第 06 册 · 命令 · 部署 · 工具链

## 1. 本册范围与衔接

第 00–05 册走读的是 `internal/` 下的库代码：那些代码在请求路径上，被 `server.*` 装配起来，一个字节一个字节地决定网关怎么回话。本册（第 06 册）走读的是**不在请求路径上的可执行体与部署材料**：`cmd/` 下的每一个二进制、`deploy/` 下的观测栈配置、以及把这一切装进容器的 `Dockerfile` 与 `docker-compose.yml`。

为什么这些值得单独一册：这个仓库里**没有任何第三方依赖**，也**没有 vendor 目录**，因此"这个项目是什么"有一半写在 `cmd/` 里——每个 `main.go` 顶部的包注释几乎都在回答"这个命令为什么存在"。这些命令是仓库全部可观测证据的来源：验收门禁跑的是它们，压测数字是它们算的，缓存命中率的语料扫描也是它们跑的。读懂了 `cmd/`，`docs/baseline/` 里那些数字才有口径，而不是一串好看的小数。

范围内、需要提前说清的一处清单差异：本分册派单里提到 `cmd/measure-m3`、`cmd/measure-m4`、`cmd/measure-m5`、`cmd/measure-m6` 四个命令，但按 `glob cmd/*/main.go` 与逐个 `read` 的结果，**这四个目录不存在**（`read` 直接返回 not found）。`cmd/` 下真正存在的可执行目录是 `cmd/infergate`、`cmd/mockupstream`、`cmd/loadtest`、`cmd/mockcollector`、`cmd/miniredis`、`cmd/measure-m2`，外加 `cmd/verify` 与 `cmd/verify-m1` … `cmd/verify-m6`。M3–M6 的测量由 `scripts/measure-m3.ps1` … `scripts/measure-m6.ps1` 承担（本册只按 `Makefile` 的注释交代它们测什么，脚本内部逻辑未逐行走读，见末节）。

## 2. `cmd/` 的目录形状与一条贯穿全册的约束

`cmd/` 下共 36 个 `.go` 文件，落到 13 个可执行目录上：六个"工具/服务"目录（`infergate`、`mockupstream`、`loadtest`、`mockcollector`、`miniredis`、`measure-m2`）与七个门禁目录（`verify`、`verify-m1` … `verify-m6`）。后七个里只有 `cmd/verify` 是单文件，其余都是"一个 `main.go` + 若干 `checks_*.go`/`harness*.go`"的形态——门禁的体量从文件名就能读出来（`cmd/verify-m6` 有八个文件，`cmd/verify-m4` 有五个）。

一条贯穿全册的约束值得先说：这个项目**没有把任何命令产物提交进仓库**。`bin/`、`tmp/`、`dist/`、`*.exe`、`*.test`、`*.out`、`*.log`、`*.pid` 都在 `.gitignore` 里，连 `docs/baseline/` 都只保留那份 JSON、丢掉 `*.txt` 与 `*.log`（注释原文："keep the committed baseline JSON, drop the rest"）。这条约定解释了后面反复出现的分工：**命令负责产生证据，证据里只有结论性文件被留下**，原始日志留在 `tmp/` 里供当场排查。`.dockerignore` 的立场是同一个：它排除 `bin`、`tmp`、`*.exe`、`*.test`，还排除 `docs`、`deploy`、`scripts`——注释说后者"编译用不到，而且 `deploy/` 是被挂进观测容器而不是烘进网关镜像的"。

`configs/` 下 15 份 YAML 也按同一套逻辑成对存在：`infergate.yaml` 是默认入口，`mock.yaml`/`cache-local.yaml`/`cache-redis.yaml`/`quota-local.yaml`/`quota-redis.yaml`/`routing.yaml`+`routing-local.yaml`/`tiered.yaml`+`tiered-local.yaml`/`agent.yaml`+`agent-local.yaml` 是各里程碑的样例画像，`observability.yaml` 与 `docker.yaml` 分别是观测画像与容器画像，`ollama.yaml` 则是"本机一个模型、没有 provider 凭据"时的那份画像（连 127.0.0.1:11434 的 catch-all，不需要任何环境变量）。`Makefile` 的 `run-*` 目标就是把它们逐一点起来的手册——`make run-mock`/`run-gateway`/`run-cache`/`run-quota`/`run-tiered`/`run-observability`/`run-agent`，每条注释都写明了它需要你先在别的 shell 里起哪个 mock 与哪个端口。读 `cmd/` 的命令时配着这份目标清单看，比单读源码更容易看清"谁跟谁配对"。

一张最小的定位表，先回答"这个命令该不该由我来起"：

| 命令 | 给谁用 | 长期占用什么 | 退不出来的方式 |
| --- | --- | --- | --- |
| `cmd/infergate` | 所有人（唯一正式入口） | 监听端口、日志、可选 JSONL、可选 Redis | Ctrl-C / SIGTERM，按 `shutdown_timeout` 排水 |
| `cmd/mockupstream` | 门禁与压测的"上游"，也可手工 curl | 监听端口、进程内计数器（不可清） | Ctrl-C，直接关 listener |
| `cmd/loadtest` | 出数字的人 | 起 N 个 worker 与一个真子进程 mock，写一个 JSON | 命令自己结束；Ctrl-C 会留下子进程需自己确认 |
| `cmd/mockcollector` | `scripts/verify-m5.ps1` | 监听端口、内存里的一摞 body | Ctrl-C，按 5s 超时 `Shutdown` |
| `cmd/miniredis` | 本地/门禁的缓存后端 | 监听端口、键空间与 LRU | Ctrl-C，关闭前打一份统计 |
| `cmd/measure-m2` | 改 embedder 或阈值的人 | 什么都不占（纯算术，可选 HTTP 出网） | 跑完就退，没有服务形态 |
| `cmd/verify` / `verify-m1..m6` | CI 与本地门禁 | 短命；进程内门禁不监听任何端口 | 断言失败即退 1 |

## 3. 六个可执行命令

### 3.1 `cmd/infergate`——唯一的正式网关入口

它是什么：OpenAI 兼容反向代理的进程外壳。包注释说得很直白：任何指向这个地址的 OpenAI SDK 不改一行就能工作；后续里程碑在同一个 listener 后面加路由、语义缓存与成本治理，**wire 契约不变**。给谁用：运维与验收脚本；`docker-compose.yml` 的 gateway 服务跑的就是它。

flag 就三个：`-config` 默认 `configs/infergate.yaml`（"path to the YAML configuration file"）、`-check`（默认 false，"validate the configuration and exit without serving"）、`-version`（默认 false，"print the version and exit"）。版本号来自包级变量 `version`，默认 `0.1.0-m0-dev`，注释给出覆盖方式 `go build -ldflags "-X main.version=1.2.3" ./cmd/infergate`——`docker-compose.yml` 用的正是这个机制（`LDFLAGS=-s -w -X main.version=0.1.0-compose`）。

控制流与退出码（错误字符串原文）：`-version` 打印 `infergate <version>` 后正常返回。`config.Load` 失败 → stderr `infergate: configuration error: %v`，退出 1；`logging.New` 失败 → `infergate: logger error: %v`，退出 1；`-check` 成功时打印 `configuration OK: %d upstream(s) from %s` 后返回 0，**不监听任何端口**——这就是 `deploy/README.md` §7.1 用来证明 `configs/docker.yaml` 真的能被加载器接受的那条命令。`server.NewServer` 失败 → `logger.Error("startup failed")` 并退出 1；`srv.ListenAndServe` 失败 → `logger.Error("server stopped")` 并退出 1。

进程内状态与关闭顺序是本文件最值得看的一段：拿到 `signal.NotifyContext(os.Interrupt, syscall.SIGTERM)` 的 ctx 后，goroutine 里按 `cfg.Server.ShutdownTimeout` 调 `srv.Shutdown`（失败只记 `logger.Error("graceful shutdown failed")`，不影响退出码），主流程随后**按序** `CloseCache` → `CloseQuota` → `CloseTracing`，各自失败只 `Warn`。源码注释观点给了三条理由：listener 停掉之后才关缓存，否则在途请求还握着 Redis 连接；quota 同理，"让在途 settle 能到达计数器"（第 05 册的记账是在请求返回后才结算的）；tracing 最后关，保证已排队的 export 送达——"该请求已经返回给调用方了"。最后 `logger.Info("stopped cleanly")` 再 `time.Sleep(10ms)` 让日志落地。推断：那 10ms 是给 stdout 管道一个排空窗口，牺牲的是每次退出的固定延迟，换来的是容器日志里不会丢掉最后一行。

并发：flag 解析、加载、构造全在单线程里做，之后交给 `http.Server`；它本身不持有任何 per-request 状态，所有请求态都在 `server` 包里。

把三个 flag 与三份"入口文档"对上，就能看出这个命令是被当作**唯一的生产入口**设计的：`-check` 让 CI 与运维能在不占用端口、不连任何下游的情况下验证一份配置——`configs/docker.yaml` 的容器画像就是靠它拿到那份 `configuration OK` 的证书；`-version` 让"这个镜像里到底是哪版代码"变成一个可以问出来的事实（`docker-compose.yml` 用 `-X main.version=0.1.0-compose` 把它烙进镜像）；`-config` 默认指向 `configs/infergate.yaml`，所以直接 `go run ./cmd/infergate` 就能起一个"默认画像"的网关。`Makefile` 里所有的 `run-*` 目标都只是这三个 flag 的不同组合。

推断：`-check` 之所以值得。这个仓库对"配置写错"的立场是**启动即失败**（严格解码，未知键直接点名），而不是宽容地取默认值。这样一来 `-check` 就是同一个判定的一次免费试运行——它能让"配置对不对"这个问题在容器还没起来、端口还没绑之前就有答案。代价是这个命令的退出码语义被压成两档（0 与 1），而门禁命令用的是三档（0/1/2），所以解析 `infergate` 的退出码时不要期待能从 2 上读出"启动期致命"这种区分。

### 3.2 `cmd/mockupstream`——可脚本化的上游替身（本册重点 A）

它是什么：一个 OpenAI 兼容的假上游。包注释交代了两个身份：M0 的退出判据要求"真实 SSE 到达真实客户端"，需要一个真会吐流、真会吐错的上游；M1 起它变成参考实现——路由、故障转移、幂等重放都需要一个"能被远程指挥成任意恶劣形状"的后端。给谁用：curl 门禁那一族脚本（从 `scripts/verify-m0.ps1` 到 `scripts/verify-m6.ps1`）全部起它，`Makefile` 的 `run-mock`/`run-fleet`/`run-agent` 也起它。

flag 原文：`-listen` 默认 `:9000`、`-name` 默认 `mock`（出现在 `X-Mock-Upstream` 响应头与 `/healthz` 里）、`-ttfb` 默认 0、`-token-delay` 默认 15ms（流式帧之间的节奏）、`-script` 默认 `""`。脚本加载失败时**退出码 2**，与运行期错误区分开。

路由形状：`/healthz`、`/models`、`/v1/models`、`/chat/completions`、`/v1/chat/completions`、`/embeddings`、`/v1/embeddings`、`/calls`、`/stats/calls`。`/v1/models` 报 `mock-gpt`、`mock-reasoner`、`mock-tool`，`/v1/embeddings` 回三维向量。`http.Server` 设了 `ReadHeaderTimeout: 10s` 却**故意不设 `WriteTimeout`**（源码注释观点：那会切断故意变慢的流）。信号处理与 infergate 同构：ctx 取消后按 5s 超时 `Shutdown`。

#### 脚本 DSL：`scriptWhen` 与 `scriptRespond`

`script` 的形状是 `{"default": {...}, "rules": [{"name","when","respond"}]}`。`Rules` **自上而下求值，第一个匹配者胜出**（源码注释原文意思）。每个 `scriptRule` 自带一个 `hits atomic.Int64`——它既是计数也是 `fail_first` 的判据，用原子量是因为"运行中服务器的每个 chat 请求都可能碰到它"。

`scriptWhen` 的六个谓词是**合取**：注释原文说 "scriptWhen is a conjunction: every field that is set must match"，所以 `{}` 匹配一切。逐条：

- `has_tool_result` 是 `bool`，**没有"不关心"取值**。`matches` 里写的是 `r.When.HasToolResult != hasToolResult(req.Messages)`——不走 `if != ""` 那套，永远参与比较。于是 `{"has_tool_result": false}` 的语义就是字面的"还没有工具结果"。源码注释观点把它说成"一个两值谓词，总是被应用"，这就是它与其他五个字段形状不同的原因。推断：如果给它一个"任意"值，`false` 这个**默认值**就会让每一条规则都隐式要求"没有工具结果"，agent 场景的第二回合将永远匹配不上；写死两值至少让这个约束显式。
- `tools_present` 同样总是参与，判据是 `len(req.Tools) > 0`。
- `contains` 是**对最后一条 user 消息**的、**大小写不敏感**的子串测试；空串表示"不关心"。
- `turn` 匹配的是"这个进程已经处理过的 chat 请求数的 1 基计数"，**0 表示任意**。关键语义在这句注释：`nextTurn` 的文档说"匹配针对自增值之前的值求值，因此 `when.turn == 1` 指的是这个进程看到的第一个 chat 请求"。但**代码与这句话并不完全一致**：`nextTurn` 的实现就是 `return int(s.turn.Add(1))`，先自增再返回，所以第一个请求拿到的 `turn` 是 1。结果（1 就是第一个请求）与文档一致，机制（"自增前求值"）不一致——这是注释与代码之间一处无害但对读者有误导的偏差，凡是按注释去推"1 是第零个请求之后的那个"都不成立。调用点只有一个：`chat` 里 `s.script.selectResponse(req, req.Stream, r.URL.Path, s.nextTurn())`。而**每次 chat 请求都会消耗一个 turn 值**（`hits` 只统计规则命中，这与 `turn` 是两套独立计数，写脚本时很容易混）。参数求值顺序同时决定了：即使这一轮没有任何规则命中、日志里也看不到 `script match`，`turn` 计数器依然已经前进了。
- `stream` 的类型是 `*bool`。源码注释观点：这样做是为了让 `"stream": false` 与"字段省略"可区分——指针为 nil 时该谓词完全不参与合取（`if r.When.Stream != nil && *r.When.Stream != stream`）。推断：不这么做的话，省略 `stream` 就会等价于"只要非流式"，一条想同时命中两种形态的规则必须写两遍。
- `path` 与 `r.URL.Path` 精确比较，空串表示"不关心"。它存在的意义是把 `/chat/completions` 与 `/v1/chat/completions` 分开命中——两个前缀注册的是同一个 handler，只有 `path` 能把它们区别开。

`scriptRespond` 的默认值语义：

- `content` 空 = "没有文本"，**不等于**"用旧版默认答案"（源码注释观点）。区分点在于 `selectResponse` 的返回：规则命中时 `Respond` 非 nil；脚本存在但无规则命中且无 `default` 时返回一个 `Legacy: true` 的匹配，`chat` 看到 `match.Legacy` 就落到内置答案 `mock answer to %q`。于是"空 content + tool_calls"是一个合法且常见的工具调用答案，不会被误解成走 legacy 分支。
- `tool_calls[].id` 省略时按列表顺序生成 `call_%d`（从 1 起），`arguments` 省略时补 `{}`。注释点出 `id` 的硬约束：**显式 id 必须在重试间稳定**——"否则 agent 运行时就无法把工具结果与请求关联起来"。推断：脚本重放时如果 id 每次新生成，客户端第二轮把 `tool_call_id` 带回来时对不上任何一次请求，agent 循环会退化成"每轮都当新对话"。
- `usage` 为 nil 时保留这个二进制今天为该响应形状算出的数字（`total_tokens` 缺省时补上）；`usage.cached_tokens` 存在是因为网关的记账要区分 cached 与 fresh prompt token。
- `status`：**0 与 200 都表示成功**；`error_body` 非空时被逐字节当作非 2xx 响应的 JSON 体，否则发 OpenAI 形状的信封。
- `delay_ms` 在响应写出前 sleep，而**先于它的 `-ttfb` 停顿依然生效**。
- `drop` 走 `Hijack` 关连接、什么都不写（内置脚本模式下未命中不走这里，走的是 `panic(http.ErrAbortHandler)`）——**`drop` 优先于 `status`**（注释原文 "Drop wins over Status"）。推断：drop 的用途是让网关看见"一个断掉的上游跳"，而不是看见一个 HTTP 状态；如果 status 抢先生效，这个场景就永远造不出来。
- `fail_first`：**前 N 次**命中该规则的请求失败（`status` 省略时默认 502），第 N+1 次起恢复正常。判据在 `effectiveStatus(r, hits)` 里：`r.FailFirst > 0 && hits <= int64(r.FailFirst)`。`hits` 由 `selectResponse` 在命中时 `hits.Add(1)`，所以"前 N 次失败"这个窗口是**按规则**而不是按全局请求数算的——这也是它必须和 `hits` 绑定的原因。
- `echo_tool_result`：true 时用 `toolResultText` 把请求里所有 `role: "tool"` 消息的 content 按顺序**直接拼接**（无分隔符、无前缀）替换 `content`。注释原文说这就是"让确定性的两回合 agent 循环成为可能"的东西。推断：agent 门禁要断言"模型看到的工具返回等于工具真正返回的"，任何装饰（"工具返回："之类）都会把断言变成对 mock 格式的断言；而按请求顺序拼接则让第二回合的答复成为第一回合工具结果的函数，整个循环因此是确定的、可复算的。

#### 故障注入头与 `/calls`

`chat` 处理流里的顺序是刻意排的，源码注释专门解释了为什么：**先解析 `X-Mock-Delay` 与 `X-Mock-TTFB`**（不可解析 → `writeJSON(w, http.StatusBadRequest, errorBody(err.Error(), ...))`），**再处理 `X-Mock-Status`**（非整数 → `X-Mock-Status must be an integer`），**再 sleep delay**，再解码 body（`invalid JSON body: ...`）、再校验 model（`you must provide a model parameter`）。注释的观点是：不可读的时长值必须报 400 而不是静默当作 0——"一个被读成微秒的 '300'（而不是 '300ms'）会让门禁在一个从未被减慢的上游上做测量"。另一句更狠："`X-Mock-TTFB` 过去是在 SSE 头已经提交之后才解析的，那时 400 已经不可能了。"推断：这些注入头是门禁**给自己造场景**的旋钮，旋钮失灵的表现是门禁"在一个从未发生的场景上变绿"——所以它们的错误路径必须比成功路径更响。`durationHeader` 的两条错误串是 `%s must be a duration such as \"250ms\": %q` 与 `%s must not be negative: %q`。

`X-Mock-Omit-Done` 在流式路径里生效：有它就不发 `data: [DONE]`，用来验证网关会给"沉默的后端"合成 `[DONE]`（`cmd/verify` 的 `synthetic [DONE] for silent backends` 就是这个）。

计数为什么集中在一个 `countingWriter` 上。`chat` 在选完规则后构造 `cw := &countingWriter{...}` 并 `defer cw.record()`，注释给了两条理由：一是"每一个 chat 答案——脚本的还是内置的——都经由 counting writer，所以 `/calls` 的计数按构造就是精确的"；二是"在每个响应路径里各自自增需要在六个地方配上对应的自增，而被忘掉的那一个会让**幂等重放看起来真的调用了上游**"。`record` 里的判据是：`!w.written && !w.dropped` 时压根不计（客户端断开、什么都没写出去的请求不算一次上游调用），`failed := w.written && w.code >= http.StatusBadRequest`（4xx/5xx 计入 `failed`），`toolCalls` 计入 `tool_calls_emitted`。`record` 还有一处为了 Windows PowerShell 5.1 的妥协：规则名是空串时改写为 `"default"`——注释说空的 JSON 属性名会让 `ConvertFrom-Json` 抛 "the value of argument \"name\" is not valid"，于是**每一个 PowerShell 写的 `/calls` 读者都会丢掉整个快照并静悄悄地跳过它的 provider-call 断言**。

`GET /calls` 是整套验收体系的**外部见证**：快照里有 `calls`、`by_path`、`by_rule`、`tool_calls_emitted`、`dropped`、`failed`。`record` 的注释把口径说死了：它在响应体写完之后才被调用，所以 "calls" 意味着"mock 真正回答过的上游请求"——而这正是幂等重放**必须不增加**的那个数。推断：网关自己报的 `/metrics` 与 `/stats` 都出自同一条代码路径，让一个实现自证它没有调用上游在逻辑上是空的；上游侧独立记账才是"零次额外上游调用"这个否定命题的见证。`/stats/calls` 是同一份快照的另一个出口。

并发：`callCount`/`turn`/`dropped`/`failed`/`toolCallsEmitted` 都是 `atomic`，`by_path`/`by_rule` 走 `countSink`（`sync.Mutex` + map，`add` 与 `snapshot` 各加锁，`snapshot` 返回拷贝）。所以并发请求下计数不丢；**但 `turn` 是"这个进程见过的第几个 chat 请求"，并发下没有请求间的确定顺序**——多 worker 打它的时候不要对 turn 做断言。

#### 两回合 agent 循环是怎么被这个 DSL 拼出来的

把上面这些机制连起来，一个确定性的两回合工具调用循环需要且只需要三样东西：一条 `{"has_tool_result": false}` 的规则回 `tool_calls`，一条 `{"has_tool_result": true, "echo_tool_result": true}` 的规则回工具结果，以及**稳定的 tool_call id**。`cmd/mockupstream/script_test.go` 里的用例就是最小形状：两条规则分别用 `{"turn": 1}` 与 `{"turn": 2}` 命中，回复文本是 `first turn` 与 `second turn`。之所以要 `has_tool_result` 参与匹配，是因为两回合的请求**只差在消息列表里有没有那条 `role: "tool"` 的消息**——`Content`、`tools`、`path` 在第二回合完全一样，没有这个谓词就只能靠 `turn` 这种"按次数"的脆弱条件分辨。

`echo_tool_result` 与 `hasToolResult` 的判据都只看**角色等于 `"tool"`**，不看 `tool_call_id` 是否对得上；`toolResultText` 把所有工具消息的 content **无分隔符直接拼接**。推断：这两处宽松是刻意的——mock 不需要理解 agent 协议，它只需要保证"我回给模型的东西，逐字节等于工具返回的东西"，把协议正确性的责任全部留给被测的 agent 运行时与网关。代价是脚本作者要自己保证一个请求里最多只有一条工具消息，否则拼接结果会连在一起难以阅读。

#### 进程级状态、并发与两个"容易被误读"的口径

`mockupstream` 的进程内状态全部挂在 `server` 结构体上：`callCount`/`turn`/`toolCallsEmitted`/`dropped`/`failed` 是 `atomic.Int64`，`byPath`/`byRule` 是 `countSink`（`sync.Mutex` + map）。**这些计数器没有任何 reset 入口**，`/calls` 只能读不能清——`internal/mockbackend` 才有 `Reset`（那是给进程内门禁复用的）。所以脚本模式下一次 `/calls` 快照的口径是"**这个进程启动以来**"的全部 chat 请求；同一份脚本里做两段互不相干的断言时，要么重启上游，要么全部按差值判断。

两个容易读错的地方：其一，`hits`（每条规则各自的命中数）与 `turn`（进程级的 chat 请求序号）是两套独立计数，`fail_first` 只看前者，`when.turn` 只看后者；其二，`turn` 在 `chat` 里**无条件**前进——连 `X-Mock-Status` 注入、body 解析失败的请求也会用掉一个 turn 值，因为它是在解析之前就被求值了（`selectResponse` 在 `chat` 的中段被调用）。这两个口径都不是 bug，但它们让"第几个请求"与"第几次匹配"在任何一行脚本里都很容易混。

流式路径还有两个细节：`streamChat` 发送的第一帧是 `role` 加**空 content**，随后**每词一帧**并按 `-token-delay` 拉开；`streamToolCall` 把参数切成三段碎片（`{"city":"Bei` / `jing","units":"met` / `ric"}`），id 固定为 `call_mock_1`。推断：第一帧空 content 是在模拟真实 provider 的行为（先提交 role 再吐第一个字），而它恰好也正是 `cmd/loadtest` 与 `cmd/verify` 必须"跳过 role 帧、等到第一个有内容的 delta 才停表"的原因——如果 mock 把首帧就填满，那个测量口径就永远不会被这些门禁检验。

### 3.3 `cmd/loadtest`——压测与归因工具（本册重点 B）

它是什么：一个用 Go 写的压测器，包注释解释为什么不直接用 `hey`/`wrk`：那些工具的每个数字都要能被这个仓库自己的口径复算，而且它要量的东西不止 QPS。给谁用：`Makefile` 的 `loadtest` 目标（M0 基线，输出 `docs/baseline/m0-baseline.json`）与 `diag` 目标。

flag 原文与默认值：`-url` 默认 `""`；`-urls`（可重复、也可逗号分隔）**覆盖 `-url`**；`-all` 跑进程内基线；`-diag` 做逐层归因；`-c` 默认 `"8,32,128"`（逗号分隔的并发级别）；`-n` 默认 800（每个 phase 的请求数）；`-warmup` 默认 100；`-rounds` 默认 3；`-timeout` 默认 30s；`-ttfb` 默认 0（给进程内 mock 的注入首帧停顿）；`-out` 默认 `""`（写 JSON 报告）。`-all` 与 `-url`/`-urls` **互斥**，错误串 `-all and -url/-urls are mutually exclusive`；三者都不给时 `fatalf` 打 `nothing to do: pass -all for the in-process baseline, -diag to attribute gateway cost, or -url/-urls http://host:port for a running deployment`，前缀统一是 `loadtest: `，退出码 1。并发级别解析失败的错误串：`-c %q: %q is not an integer`、`-c %q: concurrency must be >= 1`、`-c: no concurrency levels given`。

分工与并发模型：`runPhase` 建**一个 jobs channel**，起 `concurrency` 个 worker 从它取活；warmup 走同一套轮转（所以热身与正式的负载形状一致）。请求 i 落到哪个目标由 `targetSet.pick` 决定：锁内 `i := s.next % len(targets)` 后 `s.next++`——**`targets[i%n]` 这种确定性轮转**。推断：随机选目标会让"四实例水平扩展"这个结论依赖于一次抽样的运气；确定性轮转让某一轮少打一个实例这种偏差立刻可见，代价是它**不是**真实客户端的随机分布，所以对"单实例内部怎么退化"不敏感。

分位数用的是**最近秩**：`idx := int(math.Ceil(p*float64(len(sorted)))) - 1`，注释原文说这是刻意的——"插值 P99 可能报出没有任何请求见过的数"。推断：压测报告的价值在于能被别人拿原始数据复算，插值分位数依赖实现细节（哪家的插值），最近秩只依赖排序，而且它给出的总是**真实观测到的一个数**。

TTFT 的测量走 `internal/sse.Reader`：只有 **delta 里出现非空 content 或非空 tool_calls** 的那一刻才停表（`doRequest` 里维护 `firstToken`）。非 200 直接报 `status %d: %s`。推断：把"首字节"当作 TTFT 会把 SSE 的注释帧、role 帧算进去，那种"TTFT"衡量的是网关多久开始写头，不是用户多久看到第一个字；`internal/sse.Reader` 让 loadtest 与网关的解析口径是同一份代码。

口径诚实性的三个痕迹，全都指向同一个立场——不报一个孤立的数：

1. `medianResult` 与 `medianTargets`：每一轮跑完整套，然后**对分位数取轮间中位**，QPS 也取中位并同时记下 `QPSMin`/`QPSMax`。`Makefile` 的 `loadtest` 目标注释把理由写在命令旁边："因为共享笔记本上的单次运行不可复现，所以取 3 轮中位并带上离散度。"
2. `generator-bound` 提示：当单请求耗时低到 250µs 量级时会提示测的可能不是网关而是压测客户端。
3. 归因（`-diag`）不是只量自己：五个臂分别是 direct（1 跳）、`httputil` 普通池（pool=2）、调优 `httputil`（pool=256）、最小直通（pool=256）、InferGate（pool=256）。direct 是 baseline，其余报 **share%**；baseline 自身离散度超过 5% 时打 NOTE；网关所占份额不到 50% 时打印 "the load generator is the slower half here, not the gateway"。进程内基线（`runBaseline`）会**真起一个 mockupstream 子进程**（`-token-delay 0`）再用 `httptest` 起网关，跑四个场景（direct/gateway × 流式/非流式）；每个 phase 前后 `scrapeMetrics` 取差值，算出 `GatewayMean`/`GatewayCount`/`FirstTokenMean`。

子进程这块有一条本机环境的妥协值得记：`startMock` **把子进程输出写文件而不是管道**，因为这台主机的沙箱下无法打开 `exec.Cmd` 的命名管道；TEMP 不可写时回退 `os.DevNull` 并把 `logPath` 置空。相关错误串有 `mock exited during startup: %s`、`mock at %s did not become healthy within 15s (log: %s)`，以及回退时的 `(log unavailable, temp dir not writable)`。找不到二进制且 PATH 上没有 `go` 时给的是带操作指引的长错误：`mockupstream not built and no \`go\` on PATH to build it (%v); run tools/go.cmd build -o bin/mockupstream.exe ./cmd/mockupstream`。HTTP 客户端是 `newClient`（`MaxIdleConns 512`、`MaxIdleConnsPerHost 256`）——它就是 `configs/*.yaml` 里那个"最重要的吞吐旋钮" `max_idle_conns_per_host` 的客户端侧对照。

### 3.4 `cmd/mockcollector`——OTLP 接收端替身

它是什么：一个"一次性"的 OTLP/HTTP trace sink。包注释交代了它与 `cmd/verify-m5` 的分工：verify-m5 已在同进程里用 `httptest` collector 证明了导出器；这个二进制存在的理由是把同一件事**跨真实进程边界**证明一次——`scripts/verify-m5.ps1` 起的网关经真实 socket POST 到它，脚本再回头问它收到了什么。

它**接受任意路径**，注释说明这是刻意的：这样"路径写错"是一个可观察的缺陷，而不是脚本只能猜的 404。每个请求记 `path`、`Content-Type`、字节数与 body 的 SHA-256（`sha256.Sum256` + `hex`）。flag：`-listen` 默认 `127.0.0.1:19999`、`-name` 默认 `mockcollector`、`-max-body-bytes` 默认 `8<<20`、`-quiet`（只记错误）。三个端点：`GET /healthz` → 200 `ok`；`GET /requests` → `{"count":N,"requests":[{path,content_type,bytes,sha256}]}`；`GET /dump` → 各 body 以一行 50 个短划线分隔（注释观点：断言失败时用它展示"实际到了什么"）。收到 POST 时返回 200 `{"partialSuccess":{}}`，注释说这是 OTLP/HTTP 规范示例、真实 collector 也这么回。错误路径：读 body 失败 → 400 `{"error":"read body"}`；超过上限 → 413 `{"error":"body too large"}`；`marshal` 失败 → 500 `{"error":"marshal"}`。并发：`records` 切片由 `sync.Mutex` 保护，`snapshot` 返回浅拷贝；body 留在内存里（注释给了边界：一次 trace 导出是 KB 级、门禁只跑几秒）。生命周期：信号后按 5s 超时 `Shutdown`；`ListenAndServe` 返回非 `ErrServerClosed` 错误时 stderr `collector: %v` 并退出 1。

### 3.5 `cmd/miniredis`——仓库内的 RESP2 服务器替身

它是什么：把 `internal` 里的 RESP2 服务器当独立进程跑。包注释把边界写得很清楚：M2 验收路径必须能在**没有 Docker daemon、没有 Redis 服务**的机器上跑，同时仍然走真实 socket、真实协议、真实序列化；它**不是** Redis 替代品，只实现缓存存储会发的命令子集。给谁用：`scripts/verify-m2.ps1`/`verify-m3.ps1` 与 `Makefile` 的 `run-miniredis`/`run-miniredis-quota`。

flag 原文：`-listen` 默认 `:6399`，帮助文本自己解释了端口选择——`address to listen on (6399, not 6379, so a real Redis on this host is never shadowed)`；`-requirepass` 默认 `""`（"password to require; empty means no AUTH"）；`-max-keys` 默认 4096（"approximate key cap; beyond it the least recently used key is evicted"）；`-log-level` 默认 `info`；`-log-format` 默认 `text`。错误路径：日志构造失败 → stderr `miniredis: bad logging options: %v` 且 **退出码 2**；`Start` 失败 → `logger.Error("cannot start", ...)` 且退出 1。启动时 stdout 打 `miniredis listening on %s`，启动日志带固定 note `in-repo RESP2 subset for verification; not a Redis replacement`。关闭时打 `commands/connections/keys/evictions/expired` 统计再 `logger.Info("stopped")`。

### 3.6 `cmd/measure-m2`——阈值扫描器（含一处注释与代码不符）

它是什么：在**带标签的语料**上扫一遍相似度阈值网格，回答"这个 embedder 能不能把语料分开"。包注释的观点值得原文转述：阈值扫描不需要网关、不需要 Redis、不需要网络，它是"corpus 上的纯算术"，所以它可以当门禁——当默认阈值失去正当性时让构建失败。

flag 原文：`-dims` 默认 `embed.DefaultHashingDims`；`-http` 默认 `""`（"use an HTTP embedding endpoint instead of the offline embedder"）；`-http-model` 默认 `text-embedding-3-small`；`-http-key` 默认 `""`；`-http-timeout` 默认 5s；`-thresholds` 默认 `""`（逗号分隔，空则用 `evalset.DefaultThresholds()` 网格）；`-pairs`（打印每对相似度并排序）；`-max-false-hits` 默认 0（"false hits allowed when recommending a threshold"）；`-json` 与 `-out` 写 JSON 报告（`-out` 优先于 `-json`）。坏阈值的错误串是 `bad threshold %q: %w`（解析方式是 `fmt.Sscanf(s, "%g", &f)`）；所有 fatal 统一走 `measure-m2: %v` 到 stderr、退出 1。

**已知瑕疵（派单点 C 已核实）**：包注释声称 "the LOAD-CLIENT phase (with `-url`) replays a paraphrase workload against a live gateway and reports the hit rate"，但代码里**既没有 `-url` 这个 flag，也没有任何加载/回放阶段**——`main` 只有一条阈值扫描路径，起 HTTP 客户端只在 `-http` 指定 embedding 端点时出现。这是"注释比代码承诺得多"的典型：读者按注释去找加载阶段会一无所获。这不是有害的谎（没有哪条门禁依赖它），但它会让走读的人怀疑自己漏读了一个文件。

`evalset.Sweep` 的输出被 `printCurve` 打成表格（`thresh`/`hit rate`/`false-hit rate`/`false hits`），`-pairs` 时按相似度排序打印每对。`evalset.Recommend` 找到可用阈值时打印 `recommended threshold %.2f: hit rate %.0f%% (%d/%d), false-hit rate %.0f%% (%d/%d)`；找不到时打的是那句很诚实的 `NO threshold in the swept grid keeps false hits within %d: this embedder cannot separate the corpus`。

## 4. `cmd/verify` 家族——两种形态的门禁（重点 D）

`cmd/verify` 与 `cmd/verify-m1` … `cmd/verify-m6` 的组织方式是同一个模子：顶部的 `checker` 类型持有 `io.Writer` 与 pass/fail 计数，断言函数 `assert(ok bool, format string, args ...any)` 打印 `   PASS  %s` / `   FAIL  %s`，`info` 打缩进详情，`tally` 收口；检查本身是一个 `[]struct{name string; run func(*checker, ...)}` 表，按顺序跑并打印 `[i/N] name`；退出码统一三档——**0 全过、1 有断言失败、2 启动期致命错误**（`fatal` 打 `FATAL: ...` 后用 2，与"被测代码不对"区分开）。`cmd/verify` 的包注释还专门解释了为什么不写成满屏 curl：本机 curl 做不了 HTTPS（schannel 没凭据）且缓冲激进，**用它根本量不了 TTFT**；Go 客户端能证明字节透明（包括那个会让拼接流变成非法 SSE 的空行分帧）并能给首帧首字节计时，还能在任何平台的 CI 上零依赖运行。

两者都要存在的理由，可以落到证据类型上：进程内 `cmd/verify*` 能直接驱动 handler、注入假 store、喂**无法从外部 HTTP 触发**的坏配置与边界值，并在同一进程里读内部状态；curl 脚本门禁 `scripts/verify-m*.ps1` 起**真实进程 + 真实 curl + 真实端口**，把进程边界、配置加载、端口绑定、信号关闭全部纳入证据。前者的结论是"逻辑对"，后者的结论是"装起来真能用"。`scripts/verify-m2.ps1` 里有一句把这个立场推到极致的注释：它最有力的单条断言**不看任何计数器**——先把 mock 上游**杀掉**，再问同样的问题，仍然要拿到 200。原文意思是：计数器只能告诉你网关**相信**自己跳过了后端，而一个死掉的后端不会撒谎说自己答过。同一份脚本里另一个反复出现的口径是"从 config 文本里读期望值，而不是在脚本里抄一份字面量"——阈值是运维决策，改 `configs/cache-local.yaml` 应当带着期望一起动，而不是把一个照做的网关判红。

各门禁**声明的**断言数（数字来自 `Makefile` 各目标的注释，本册未逐条复核，也未跑 `go test`）：进程内 `verify` 38、`verify-m1` 64、`verify-m2` 103、`verify-m3` 470、`verify-m4` 879、`verify-m5` 420、`verify-m6` 381；curl 侧 `verify-m0` 48、`verify-m1` 61、`verify-m2` 158、`verify-m3` 323、`verify-m4` 128、`verify-m5` 211、`verify-m6` 153（`Makefile` 另有一句汇总："七个门禁的 curl 行加起来是 1082 条"）。数量差本身就是信息：M4 的 879 条说明配置校验与路由决策是**表驱动穷举**的地方，而 M0 的 38 条说明那时候要证明的只是"线缆契约成立"。

## 5. `deploy/`——观测栈配置（只有配置，没有程序）

`deploy/README.md` 的中文说明交代了这个目录的定位：它只放配置，不放 Go 代码也不放二进制；Prometheus 与 Grafana 都是外部程序，**仓库既没 vendored 也没联网下载**。一句话概括职责边界：网关只在 `GET /metrics` 上吐手写 Prometheus 文本，抓取、存储、查询、画图全是外部的事。

四个文件，两两成对，差别只有一个词：

- `deploy/prometheus/prometheus.yml`（本机形态）两个 job：`infergate-single` 目标 `127.0.0.1:18999`，`infergate-fleet` 目标 `18999`/`19000`/`19001`/`19002`（同一份配置起四个网关进程指向同一个 mock 上游，看板里的"集群聚合 QPS"要的就是这一组能否随实例数线性上涨）。全局 `scrape_interval: 5s`、`scrape_timeout: 4s`、`evaluation_interval: 15s`；注释专门解释了 4s 必须严格小于 5s——"否则一轮抓取超时会把下一轮的开始时间顶掉，表现为面板上每隔一段就断一个点"。`external_labels` 打 `cluster=infergate-local`、`host=windows-dev`、`milestone=m5`。文件里还有一段**刻意注释掉的** mock 上游 job，理由写在旁边：`cmd/mockupstream` 今天**不暴露 `/metrics`**，打开它只会得到 404。
- `deploy/prometheus/docker.yml`（容器形态）只改目标为 `gateway:8080`、`external_labels.host` 改成 `docker-desktop`，让两套数据进同一个 Prometheus 也能分开看。**它的注释里出现了行号引用**（指向 `../prometheus.yml` 的某个行区间，`configs/docker.yaml` 的注释也提到"曾引用的三个行号都已经漂到无关行上，所以现在改成指名 `IdempotencyConfig`/`SessionsConfig` 的 `Capacity` 字段"）——这属于内容问题：仓库自己的散文规范禁止按行号引用，而这两处注释正是"行号会漂"的活证据。
- `deploy/grafana/provisioning/datasources/prometheus.yml` 与 `deploy/grafana/datasources-docker/prometheus.yml`：`uid: PROMETHEUS`、`access: proxy`、`isDefault: true`、`editable: false`，两者只差 `url`（`http://127.0.0.1:9090` vs `http://prometheus:9090`）。uid 固定是这份配置的关键：看板 JSON 里每处数据源引用都写 `${DS_PROMETHEUS}` 模板变量，默认值就是这个 uid，所以看板能在没有"手动选数据源"这一步时直接出图。`access: proxy` 的含义写在注释里——Grafana 服务端去连 Prometheus，浏览器不直连，因此**不需要给 Prometheus 配 CORS**。
- `deploy/grafana/provisioning/dashboards/infergate.yml` 是文件型 provider：`folder: InferGate`、`folderUid: infergate`、`disableDeletion: false`、`updateIntervalSeconds: 30`、`allowUiUpdates: false`、`foldersFromFilesStructure: false`。`options.path` 默认是容器路径 `/etc/grafana/provisioning/dashboards`，注释给出 Windows 本机必须换成的那一行——两者只能留一个，且**本机路径写错时 provider 会安静地扫一个空目录、不报错**，表现为"Grafana 起来了但一个看板都没有"。
- `deploy/grafana/dashboards/infergate.json` 是看板本体：`uid=infergate`、33 个 panel，标题从"怎么读这块看板"到"运行时内存明细（memstats）"，覆盖请求速率/状态码/端到端 P50-P99/上游尝试耗时/首 token 延迟/token 吞吐/流式帧与字节速率/缓存命中率与条目数/熔断状态与 trips/配额决策与账本/故障转移/goroutines/heap/GC。文件**纯 ASCII**，中文全部写成 `\uXXXX`——`deploy/README.md` 给的理由是 Windows PowerShell 5.1 的 `Get-Content` 对无 BOM 文件按本地代码页解码，直接写中文会在"读-改-写"一次之后变成乱码。

`deploy/README.md` §6 的"measured vs not measured"是这一册最该记住的口径：**已验证**的只有"看板 JSON 能被 PS 5.1 与 Node 解析、三份 YAML 与看板里的每一个 `infergate_*` 名字都对过 M5 契约清单、YAML 是按检查单人工看过的（2 空格缩进、无 Tab、无重复键）"；**未验证**的是"本机没有 Prometheus 也没有 Grafana 二进制、网络不通，所以没跑过 `promtool check config`、没起过 scrape、没查过 `up{}`、任何 panel 图形**没有被渲染过**"。它还专门提醒一个只会误导人的现象：**空 panel 与真 0 是两件事**，延迟类 panel 尤其如此。

### 5.1 `scripts/verify-m2.ps1` 里那些"为了在这台机器上不失真"的工程细节

只读一份 curl 门禁就能看出这类脚本的写法有很强的家族相似性，值得把 `scripts/verify-m2.ps1` 里的通用件抽出来讲一次（其余 `verify-m*.ps1` 是否逐字同构**未确认**）。

参数面：`-GatewayPort` 默认 18280、`-UpstreamPort` 默认 19500、`-RedisGatewayPort` 默认 18281、`-RedisUpstreamPort` 默认 19501、`-MiniredisPort` 默认 19399，外加一个 `-KeepRunning`（断言失败时把进程留在原地，让你自己 curl）。**每个端口都不是样例配置里的端口**：脚本在第 1 节会构建三个二进制，然后把 `configs/cache-local.yaml` / `configs/cache-redis.yaml` 的文本里的 `listen` 与上游地址**正则替换成本次运行自己的端口**再落盘到 `tmp/`。注释给的理由是"脚本永远不和 8082/8083/9200/9201 上的陈旧进程打架，也不和另一份同时在跑的自己打架"。推断：这是并行门禁能同时跑的前提，代价是每次运行的期望值都来自"被改写过的文本"，所以脚本必须**从这份文本里读回要断言的值**——这正是它坚持不写字面量的原因。

四个会被别的册忽略、但决定了"门禁会不会骗你"的辅助函数：`Assert-Contains` 用 `String.Contains` 而**不是** `-like`（注释原文：`-like` 会把方括号当字符类，于是含 scope 串、label selector 的 needle 会对着"明明包含它"的文本报假失败）；`Read-Text` 走 `StreamReader` 而不是 `Get-Content`（"Get-Content/Set-Content 的一次往返在这个仓库里产生过乱码"，而带 CJK 的响应体会被控制台代码页静默改掉）；`New-BodyFile` 一律用 `[System.IO.File]::WriteAllText` 加 `UTF8Encoding($false)`（**无 BOM**）并把请求体以 `-d @file` 的形式交给 `curl.exe`——注释里两条理由都很实在：PS 5.1 给原生程序重新加引号时会**把 `-d '{"model":"x"}'` 里的双引号剥掉**（curl 收到的是 `{model:x}`），而 BOM 会让网关把请求体判成"不是 JSON"；`Get-SeriesValue` 用 **lookahead** 做标签包含匹配而不是前缀匹配（导出的各家族 label 顺序不同，前缀匹配会对其中一部分静默返回 0——"一个恰好长得像'缓存什么都没做'的错数"）。

另有两处与"证据强度"直接相关：`Read-OpenLog` 用 `FileShare.ReadWrite` 打开**另一个进程还在写的**日志（普通构造会报 "the process cannot access the file because it is being used by another process"），这样断言失败时能当场看网关自己说了什么；`Invoke-Http` 用 `-o`/`-D` 把 body 与 headers 分别写到**文件**、用 `-w '%{http_code}'` 把状态码留在 stdout（注释给的理由是"`-i` 会把头和体交错，而流式体无法可靠地再切回来"）。推断：这两条是同一件事的两面——门禁必须先在**字节层**拿到原始材料，再做断言；一旦让 PowerShell 参与拼接或解码，失败报告本身就不可信了。

## 6. 容器形态：`Dockerfile`、`docker-compose.yml`、`configs/docker.yaml`

`Dockerfile` 是一个**用 build arg 选目标**的镜像：`ARG CMD=infergate` 决定编译哪个 `cmd/`，三个服务共用同一份。`ARG GO_IMAGE=golang:1.26-alpine` 可以覆盖——注释里给的"为什么"是：模块目标是 Go 1.26 且本项目从本地 module cache 而非 PATH 固定工具链（根 README §4），而新 Go 版本的镜像 tag 出现在 Docker Hub 的时机不定。构建阶段 `CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "${LDFLAGS}" -o /out/app ./cmd/${CMD}`，注释强调 `CGO_ENABLED=0` **不是瘦身技巧**——"本项目全部意义就是零依赖静态二进制"，它同时也是 alpine 运行时阶段（musl）可行的前提；反过来，本机跑不了 `go test -race` 恰恰是因为 race 需要 cgo 而本机没有 gcc（`Makefile` 的 `test` 目标把整段绕行方案写在注释里）。没有 `go.sum` 也没有 `vendor/`，因为模块**没有任何第三方依赖**（只 COPY `go.mod` 就够解析模块路径）；注释顺手留了"将来加依赖要在 COPY 之后补 `go mod download`"。

运行时阶段 `FROM alpine:3.20`，`apk add --no-cache ca-certificates netcat-openbsd`：注释逐项解释——`ca-certificates` 是唯一真正重要的一项（连真实 provider 需要信任库）、`wget` 来自 busybox 是网关自己的 `HEALTHCHECK` 用的、`netcat-openbsd` 是**为了 miniredis 的健康检查**：网关答 HTTP 所以 wget 够用，但 miniredis 说的是 RESP2、wget 探不了，而 compose 的 `depends_on: condition: service_healthy` 需要一个真能打开 socket 的探针；替代方案（再写一个 Tiny Go 探针当第四个命令）被评价为"为了一个布尔值多一个命令、多一份文档、多一个 configs 条目"。非 root 用户 `adduser -D -H -u 10001 infergate`，注释说明为什么不需要可写目录：缓存与配额计数在 Redis（或进程内存）里，trace 走 stdout、OTLP 端点或运维挂载的 JSONL 文件。`COPY configs /app/configs` 把配置烘进镜像（`docker run` 不必挂卷），`HEALTHCHECK` 只探 `/healthz`（存活），并注明 `/readyz` 是更强的探针（看路由表与各 store），compose 给依赖方用的就是它。

`docker-compose.yml` 的形态：`name: infergate`，服务 `gateway`/`mockupstream`/`miniredis`，另有一个 `obs` profile 装 `prometheus` 与 `grafana`。三处设计值得记：

1. **端口故意不是常用默认值**（18080 而非 8080、19090 而非 9090、13000 而非 3000、16399 而非 6379），注释给的理由是这台机器平时以**同机本地进程形态**运行、验收脚本也用 18080/18999，所以容器不能抢默认端口；全部可用环境变量覆盖（`INFERGATE_GATEWAY_PORT=8080 docker compose up -d`）。
2. **没有任何 named volume**：注释说 Prometheus 与 Grafana 的状态"在这个 profile 里刻意是易失的"，`docker compose down` 即干净状态——对"数字应当可复现"的东西这是正确的默认。
3. `depends_on` 用的是 `condition: service_healthy` 而**不是** `service_started`，理由是这是个真实启动竞争：配额默认 fail-closed，读不到存储的网关会**拒绝启动**（不是能慢慢重试的软失败），所以"容器存在"对 mockupstream 与 miniredis 都不够。三个探针分别是 `wget -qO- http://127.0.0.1:8080/readyz`、`wget -qO- http://127.0.0.1:9000/v1/models`、`nc -z 127.0.0.1 6399`。mockupstream 的启动参数是 `-listen :9000 -name docker-mock -token-delay 15ms -ttfb 50ms`，注释提醒 15ms 是默认值、只是让流式肉眼可见，**画任何吞吐结论前要设成 0**，否则压测量的是 mock 不是网关。

`configs/docker.yaml` 是容器画像，它开篇就声明自己"**不是** `configs/mock.yaml` 换个缩进"：容器网络里唯一变了的是"127.0.0.1 指谁"——在网关的 namespace 里它是网关自己。于是每一处本地画像里的 loopback 地址都变成 compose 服务名：`127.0.0.1:9000 → mockupstream:9000`、`127.0.0.1:6399 → miniredis:6399`；写错的后果是"缓存永远 miss、配额永远连不上"。其余（pricing、cache threshold、quota policy）**刻意与本地画像同形**，好让这里量出的数字与 `docs/baseline/` 可比。逐块看：`server.listen: ":8080"`（容器内端口，与宿主机映射 18080 不同）、`upstream_timeout: "10m"`、`shutdown_timeout: "15s"`（注释：容器停止发 SIGTERM 后 docker 还会等，这是给在途流的排水窗口，要**小于** stop grace period，好让进程自己退出而不是被 SIGKILL）、`max_idle_conns_per_host: 256`（注释点名这是"最重要的吞吐旋钮"）；`cache.store: redis` 指向共享 RESP2 服务器（容器画像因此是**多副本形状**而不是每进程内存缓存）；`quota.fail_open: false` 且预算给得很宽——注释说这个画像是用来看面板的，**不是用来复现 M3 的拒绝**的（那件事用 `configs/quota-redis.yaml`）；`tracing.jsonl_path: ""` 并附理由：默认的 JSONL 文件目的地对容器是错的（文件系统是临时的，真正该看的地方是 `docker compose logs`），留空则 ring buffer 与 `/admin/tracing` 仍然可用。`idempotency` 那段注释还留着一条"用字段名而不是行号"的自觉说明：`capacity` 不叫 `max_entries`，写成 `max_entries` 会在启动期被严格解码点名拒绝（`unknown field "max_entries"`），而这段注释"曾经是唯一的防线"。

`deploy/README.md` §7.1 对容器形态的诚实交代必须原样记住：**镜像本身从来没有被构建过，compose 也从来没有被起来过**——写这份文档的机器上 Docker Desktop 装着但**守护进程没运行**。因此 `golang:1.26-alpine`、`alpine:3.20`、`prom/prometheus:v2.53.0`、`grafana/grafana-oss:11.1.0` 这四个 tag 是否存在、多阶段构建是否通过、非 root 用户能否写 OTLP，**一概没验过**；`GO_IMAGE` 做成 build arg 正是为了 tag 换了不用改文件。已验证的只有三件：`docker compose -f docker-compose.yml config --quiet` 通过（不需要 daemon）、`configs/docker.yaml` 过真实加载器（`go run ./cmd/infergate -config configs/docker.yaml -check` → `configuration OK`）、以及 `scripts/verify-docker-profile.ps1` 的 20 项全过——它把 `configs/docker.yaml` 里的服务名换成本地地址，用仓库自己的三个二进制真起一遍，证明 `capacity`、`max_response_bytes`、`recent_per_session`、`tracing.jsonl_path: ""` 这些**值真的生效**。§7.1 还记了一条很有价值的门禁教训：这道门在 2026-10-06 加了三层前置检查（端口空闲、`-check` 通过、两个 store 可达）之后才敢下结论——在此之前它曾在 **miniredis 根本没起来**的运行里报出 `first request is a miss got 'skip'`，**把"环境没准备好"显示成"缓存错了"**。

## 7. 三条贯穿全册的口径

**其一：命令的产物不进仓库，证据进仓库。** `bin/`、`tmp/`、`*.exe`、`*.test`、`*.log`、`*.pid` 全在 `.gitignore` 里，`docs/baseline/` 也只留 JSON、丢掉 `*.txt` 与 `*.log`。这解释了一个反复出现的分工：`cmd/loadtest` 用 `-out` 把结论写成 JSON、"每个 phase 前后的 metric 差值"这类原始材料留在 `tmp/`；`scripts/measure-m*.ps1` 各写一份 `docs/baseline/m*-summary.json`；`cmd/mockcollector` 干脆连磁盘都不落，body 留在内存里只活几秒。**要断言的东西必须落成文件，用来排查的东西可以随时消失**——这条规矩让"哪些数字是仓库自称的"在目录层面就可判定。

**其二：端口是这三层之间的硬接口。** `Makefile` 的 `run-*` 目标、`docker-compose.yml` 的宿主端口映射、`scripts/verify-m*.ps1` 的默认参数各有一套端口，但都遵守同一组约定：网关默认 8080（本地形态多用 8082/8083/8084/8085，门禁用 18999 与 18280/18281），mock 上游在 9000–9301 与 19500/19501 一带，miniredis 用 6399 与 6398（**永不用 6379**），容器把所有对外端口搬到 18080/19000/16399/19090/13000。读这一册时凡遇到"某个端口为什么是这个数"，答案基本都是"不要遮住本机已经在跑的那个"或"不要和并行运行的自己打架"。

**其三：退出码语义分三档，不要跨命令套用。** 门禁命令（`cmd/verify`、`verify-m1` … `verify-m6`）统一是 **0 全过 / 1 有断言失败 / 2 启动期致命**（`fatal` 打 `FATAL: ...` 后退 2，"被测代码不对"与"环境没搭起来"因此可区分）；`cmd/infergate` 只有 0/1（配置错与运行时错都退 1，没有第三档）；`cmd/mockupstream` 的脚本加载失败特意用了 **2**，理由写在它的包注释里——那不是一个"mock 服务跑着但答错了"的问题，而是根本没有可用的 mock；`cmd/miniredis` 的日志选项错误也用 2。推断：这套分布的意图是让"我的门禁为什么红了"在退出码一层就能分流成两类——**2 是环境/输入问题，1 是被测对象问题**。跨命令看时唯一要小心的是 `cmd/infergate` 没有 2，所以"它退了 2"这件事本身不可能发生。

## 8. 未能确认

- **未跑 `go test`，也未跑任何门禁/压测**：本会话的 go 需要下载 1.26 toolchain 且被沙箱拒绝，所以本册全部结论来自源码精读与正则人工核对，没有任何一条是可执行证据。`Makefile` 里那些断言数（38/64/103/470/879/420/381 与 curl 侧的 48/61/158/323/128/211/153）是**目标注释里的声明值**，我没有逐条数过表项；把它们当"仓库自称的口径"，不要当"我复核过的数字"。
- **`scripts/measure-m3.ps1` … `measure-m6.ps1` 的内部逻辑未走读**：本册只按 `Makefile` 各目标的注释交代它们测什么（`measure-m3` 是治理的开销与拦截率、`measure-m4` 是 fp16/AWQ/GPTQ 的三方对比加一次真实分层运行、`measure-m5` 是可观测性的开销、`measure-m6` 是幂等/会话/tracing 开关的代价与收益）。它们各自写 `docs/baseline/m*-summary.json`，也各自声明了自己的断言数（`measure-m3` 是 57），这些都未经我核实。
- **`scripts/verify-m0.ps1` 与其余 `verify-m*.ps1` 只读了 `verify-m2.ps1` 一份**：该份超过 900 行，我只读了前一大半，其余部分（`/admin/cache/lookup` 之后的 flush 段与收尾统计）未读完。所以"curl 门禁的形态"这条结论有单一实例支撑，别的脚本是否同构**未确认**。
- **`deploy/grafana/dashboards/infergate.json` 只做了结构级检查**（panel 数、uid、标题清单），没有验证任何一个 PromQL 表达式的正确性——这本来就是 `deploy/README.md` §6 承认的空白（"没有一次真实查询、任何 panel 图形没有被渲染过"）。
- **`cmd/loadtest` 的 `-all` 基线路径未经运行**：`runBaseline` 里的四场景与"轮间中位 + `QPSMin`/`QPSMax`"我按代码读出来，但它对 mockupstream 子进程的依赖（输出写文件而非管道、TEMP 回退）在这台机器上会走到哪条分支**未确认**。
- **发现的内容问题**（供后续分册或修复参考，非本册结论）：(1) `cmd/verify-m6` 并不使用 `cmd/mockupstream` 的脚本 DSL，它在自己的 harness 里另有一个 `scriptedUpstream`；两个"有状态上游"实现（`cmd/mockupstream` 与 `internal/mockbackend`）对**同一组注入头的解析顺序不同**——`cmd/mockupstream` 先解析 `X-Mock-TTFB` 再看 `X-Mock-Status`，`internal/mockbackend` 的注释则声明它刻意让状态头保持优先；这不是 bug，而是两处各有一句注释在解释自己的顺序，读者很容易把两者当同一份实现。(2) `cmd/mockupstream/script.go` 的 `nextTurn` 文档说"匹配针对自增值**之前**的值求值"，而实现是先 `Add(1)` 再返回；结论（`turn == 1` 是第一个 chat 请求）一致，机制描述不一致。(3) `cmd/measure-m2` 的包注释承诺了一个**不存在的** `-url` 加载/回放阶段（重点 C）。(4) `deploy/prometheus/docker.yml` 与 `configs/docker.yaml` 的注释里存在行号引用；后者自己说明了行号会漂、并已改成指名 `IdempotencyConfig`/`SessionsConfig` 的 `Capacity` 字段，前者还没改。

## 9. 本册覆盖清单

**本册逐符号覆盖的单元：**

- `cmd/infergate`、`cmd/mockupstream`、`cmd/mockcollector`、`cmd/miniredis`、`cmd/measure-m2`
- `cmd/loadtest`：**本册只到机制与口径层**（flag 全集、并发模型、最近秩分位数、TTFT 口径、`-diag` 五臂、子进程处理、轮间中位），其逐函数、逐类型、逐字段的走读见第 09 册。先前版本把它与 `cmd/mockupstream` 并列声明为「逐符号覆盖」，与实际情况不符，现已改准。
- `cmd/verify` 与 `cmd/verify-m1`…`cmd/verify-m6`（只给定位，不展开逐断言）
- `deploy/` 下的观测栈配置
- `Dockerfile`、`docker-compose.yml`、`configs/docker.yaml`

**本册明确没有覆盖的东西：**

- `internal/repofmt`：仓库自检测试包，属第 07 册。
- `internal/sessions` 与 `internal/stats` 的全量符号、`internal/repofmt` 的辅助函数、`internal/embed` 与 `internal/evalset` 的全量符号、`internal/mockredis` 的命令处理器与 `internal/quota` 的字段级语义、以及 `cmd/loadtest` 的逐符号细节：都不在本册，依次见补充分册（第 08、10、11、12、09 册）。
- `Makefile`、`tools/go.cmd`、`scripts/` 全清单与其逐断言逻辑：属第 07 册（本册只按注释转述口径）。
- `internal/` 下所有库代码：属第 00–05 册。
- `cmd/verify-m1`…`cmd/verify-m6` 各自 harness 的逐条断言：不在本册范围。
- 本会话未跑 `go test`，也未跑任何门禁或压测：本册结论来自源码精读与正则人工核对。

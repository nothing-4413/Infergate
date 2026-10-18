# 第 09 册 · 测量工具：cmd/loadtest 与 cmd/measure-m2

## 1. 本册范围与衔接

本册补的是第 07 册与第 07B 册**主动声明**留下的那一块。第 07B 册把 M1–M6 的验收骨架、证明手段与数据来源写全之后，在末尾写明：`cmd/measure-m2` 与 `scripts/measure-m2.ps1` 的分工、以及「前者的包注释承诺了一个 `-url` 加载/回放阶段，而代码里既无该 flag 也无该阶段」这一处注释与实现不一致，留给后续。第 07 册讲脚本层、构建与 CI 的**机制与口径**，也把 `cmd/loadtest` 与 `cmd/measure-m2` 停在机制层。本册就是那一册：把这两个命令的每个顶层符号、每个字段、每条错误路径摊开。

范围与分工，先说清楚：

- 本册逐符号覆盖 `cmd/loadtest/main.go`、`cmd/loadtest/main_test.go`、`cmd/measure-m2/main.go`。
- 本册逐脚本讲 `scripts/measure-m2.ps1`、`scripts/measure-m3.ps1`、`scripts/measure-m4.ps1`、`scripts/measure-m5.ps1`：它量什么、怎么从网关取数、结果写成什么形状、落在 `docs/baseline/` 的哪一份、以及它**不能**证明什么。
- **本册不逐符号展开 `cmd/verify` 与 `cmd/verify-m1`…`cmd/verify-m6` 家族**（约 267 个夹具/断言辅助符号）。那是第 11 册（`docs/WALKTHROUGH/11-verify-harness-fixtures.md`）的题目；本册只在各节末尾用一句话做交叉指引。
- 本册不重复第 07 册已经讲过的构建方式（`tools/go.cmd`、`Makefile`）与 CI 接线。

三份文件的实际形状（都只有源码，没有别的 `.go`）：

| 文件 | 顶层声明的数量 | 读法 |
| --- | --- | --- |
| `[cmd/loadtest/main.go](../../cmd/loadtest/main.go)` | 38 个 `func`、8 个 `type`、四个包级 `var`（含三个正则与两个请求体） | 本册第 2–15 节 |
| `[cmd/loadtest/main_test.go](../../cmd/loadtest/main_test.go)` | 7 个 `func`（全部是 `Test`） | 本册第 16 节 |
| `[cmd/measure-m2/main.go](../../cmd/measure-m2/main.go)` | 5 个 `func` | 本册第 17 节 |

读法约定沿用本目录既有的四条：锚点只写符号名与文件；行号只出现在代码块里；源码注释里解释「为什么」的句子照译保留，并标注为**源码注释观点**；找不到确证的地方明写「未能确认」。

## 2. `cmd/loadtest` 的测量口径

先把口径讲透，后面每个符号才有位置。口径本身第 07 册已述的部分，这里一句话带过并标注来源；本册的篇幅留给符号。

**为什么是 Go 而不是 hey/wrk**——这是 `cmd/loadtest/main.go` 包注释的第一段，也是这个命令存在的理由（源码注释观点）：这台宿主机没有 hey 也没有 wrk，而 curl 完全不能为流的首 token 计时（shell 会缓冲）。一个 Go 生成器既能测流的首字节延迟也能测整段时长，还能把**同一个**负载在「有网关在路径上」与「没有网关在路径上」两种情况下比较——而那是唯一能回答「网关让我多付了什么」的数字。

**为什么一个进程打多个目标**（源码注释观点）：`-urls` 用一个进程瞄准若干个已在运行的部署，请求在它们之间轮转，一次一个请求。这才让横向扩容的测量诚实——跑 N 份生成器会在各臂之间塞进 N-1 份客户端自身的 CPU 成本，于是「2 实例」那一臂会去跟一个客户端更便宜的「1 实例」臂比较。一个进程、N 个目标、一份客户端成本。

**四个口径细节**，逐个说：

1. **并发由 jobs channel 定界，不用信号量**。`runPhase` 恰好起 `concurrency` 个 worker goroutine，它们争抢一个无缓冲的 `jobs chan int`；主协程把 `0..requests-1` 逐个送进去，worker 一退休就立刻从 channel 取下一个。源码注释观点：派发 channel 就是并发上限，所以不需要信号量、队列深度是 1，目标在整个测量窗口保持饱和。
2. **请求 i 去 `targets[i%n]`**。目标选择在**请求**粒度上做，而计数器被所有 worker 共享，所以各目标的计数最多相差 1；没有任何随机、也不依赖请求耗时，所以同一条命令跑两次会把相同的请求交给相同的实例。按 worker 切分会在某个目标比兄弟慢时失效——钉在它上面的 worker 会落后，整趟量的就成了客户端的调度而不是 fleet 的容量。这一点由 `targetSet` 与它的三个测试钉住，见本册第 14、16 节。
3. **百分位是 nearest-rank**，`idx := int(math.Ceil(p*float64(len(sorted)))) - 1`。源码注释观点：nearest-rank（而非插值）永远报一个**真正被观察到**的延迟——插值出来的 P99 可以引用一个没有任何请求见过的数。
4. **TTFT 计在首个有内容的 delta 或首个 tool_call 处**。`doRequest` 只在 `d.Content != "" || len(d.ToolCalls) > 0` 时把 `firstToken` 落成 `time.Since(start)`；一个只有 `role` 的空 delta 不算首 token。
5. **每阶段取 3 轮的中间值**。`-rounds` 默认 3，且 `-rounds` 的 flag 说明本身就是理由：`repeats per phase; the median is reported, because a single pass cannot separate a real effect from drift`。轮次在**外层**、阶段在内层，这样各阶段处在同一热与堆状态，中位数丢弃第一轮离群。

还有一条只属于这个命令的自曝式提醒：`runPhase` 在末尾算 `perGoroutine := wall / time.Duration(max(1, requests/concurrency))`，小于 `250*time.Microsecond` 就打印一句 `is generator-bound` 的 note。源码注释观点：回环负载发生器自己就是嫌疑人；若每个请求都做在不到一个调度量子里，测得的 QPS 是客户端的天花板而不是目标的——那不算失败，只是吞吐数字与服务器无关，而它在表里看不见，所以用话讲出来。

## 3. `main` —— 三种运行模式与参数

```go
func main()
```

**参数面**（全部是 `flag`，因此在 `main` 里定义并解析）：

| flag | 默认值 | flag 说明原文 |
| --- | --- | --- |
| `-url` | `""` | `single target base URL; -urls overrides it when both are given` |
| `-urls` | 空 | `comma-separated target base URLs, repeatable (-urls a,b -urls c); requests are spread round-robin and -urls overrides -url` |
| `-all` | `false` | `run the full in-process baseline (direct and via gateway, stream and non-stream)` |
| `-diag` | `false` | `attribute the gateway's cost: compare a bare reverse proxy, a minimal passthrough hop, and the real gateway` |
| `-c` | `"8,32,128"` | `comma-separated concurrency levels` |
| `-n` | `800` | `requests per phase (per concurrency level)` |
| `-warmup` | `100` | `warmup requests per phase, excluded from the numbers` |
| `-rounds` | `3` | `repeats per phase; the median is reported, because a single pass cannot separate a real effect from drift` |
| `-timeout` | `30*time.Second` | `per-request timeout` |
| `-ttfb` | `0` | `mock's artificial delay before its first stream frame` |
| `-out` | `""` | `write the results as JSON to this path` |

`-urls` 是 `urlList`（一个 `flag.Value`），所以它可重复、且每次出现本身可以是逗号列表。

**控制流**，按源码顺序：

1. `levels, err := parseLevels(*concurrency)`；失败 `fatalf("%v", err)`。
2. `-diag` 优先：调 `runDiagnosis(levels[0], *requests, *warmup, *rounds, *timeout)`，失败 `fatalf`，成功直接 `return`。注意它只用**第一个**并发级别——诊断是一条一条排队比配置，不是并发扫描。
3. 目标列表：`targets := urls.list()`；若为空且 `*target != ""` 则 `targets = []string{*target}`。源码注释观点：`-urls` 一旦点名任何东西就完全胜出；把两个 flag 合并会静默把 `-url A -urls B` 变成一次双目标运行，而记录便再也说不清是哪条指令产生了它。
4. 两个致命守卫：
   - 目标为空且没有 `-all` → `fatalf("nothing to do: pass -all for the in-process baseline, -diag to attribute gateway cost, or -url/-urls http://host:port for a running deployment")`。这条错误信息本身就是这个命令的用法说明。
   - `-all` 与 `-url`/`-urls` 同时给 → `fatalf("-all and -url/-urls are mutually exclusive")`。
5. `-all` → `runBaseline(levels, *requests, *warmup, *rounds, *timeout, *ttfb, *out)`，失败 `fatalf`，成功 `return`。
6. 否则（外部部署模式）：`client := newClient(*timeout)`；`results, err := runMatrix(client, targets, levels, *requests, *warmup, *timeout)`，失败 `fatalf`；`printTable(results)`；`-out` 非空则 `writeJSON(*out, results)`，失败 `fatalf`。

**三种模式各有各的产物**：`-all` 走 `report`（含 `started`/`host`/`go_version`/`rounds`/`phases`）；`-diag` 只打印、**不写文件**（即使给了 `-out` 也不写——`runDiagnosis` 的签名里根本没有 `out` 参数）；`-url`/`-urls` 走 `printTable` 加可选 `writeJSON`，写出的是一个 `[]result` 而不是 `report`。

## 4. 结果类型：`result`、`targetStat`、`report`、`measurements`

这四类里前三类是**产物形状**（有 JSON tag，会落盘），第四类只是内存里的中间量。

### `type result struct` —— 一个被测阶段

`result` 是一个被测阶段：一种负载（stream / 非 stream）在一个并发级别上打一个目标。字段级清单（类型 + JSON tag，逐条）：

| 字段 | 类型 | JSON tag | 含义与源码注释观点 |
| --- | --- | --- | --- |
| `Label` | `string` | `label` | 阶段名，如 `direct non-stream`、`gateway stream`、`stream` |
| `Target` | `string` | `target` | 目标；单目标就是那条 base URL，多目标就是逗号连接列表 |
| `Stream` | `bool` | `stream` | 这一阶段是流式还是非流式 |
| `Concurrency` | `int` | `concurrency` | 并发级别 |
| `Requests` | `int` | `requests` | **成功**请求数（`len(lat)`） |
| `Errors` | `int` | `errors` | 失败请求数 |
| `QPS` | `float64` | `qps` | 成功数除以阶段 wall |
| `Rounds` | `int` | `rounds,omitempty` | 参与归约的轮数 |
| `QPSMin` | `float64` | `qps_min,omitempty` | 各轮 QPS 最小值 |
| `QPSMax` | `float64` | `qps_max,omitempty` | 各轮 QPS 最大值 |
| `P50` | `time.Duration` | `p50` | 中位延迟 |
| `P95` | `time.Duration` | `p95` | P95 延迟 |
| `P99` | `time.Duration` | `p99` | P99 延迟 |
| `Max` | `time.Duration` | `max` | 最大延迟 |
| `Mean` | `time.Duration` | `mean` | 客户端实测均值 |
| `TTFTP50` | `time.Duration` | `ttft_p50,omitempty` | 流式专用：首 token P50 |
| `TTFTP95` | `time.Duration` | `ttft_p95,omitempty` | 流式专用：首 token P95 |
| `TTFTMean` | `time.Duration` | `ttft_mean,omitempty` | 流式专用：首 token 均值 |
| `BytesMean` | `int64` | `bytes_mean,omitempty` | 流式专用：每请求平均字节数 |
| `FramesMean` | `float64` | `frames_mean,omitempty` | 流式专用：每请求平均帧数 |
| `GatewayMean` | `time.Duration` | `gateway_mean,omitempty` | 网关自己的请求时长观 |
| `GatewayCount` | `int` | `gateway_count,omitempty` | 本阶段网关侧计数增量 |
| `FirstTokenMean` | `time.Duration` | `gateway_first_token_mean,omitempty` | 网关自己报的首 token 均值 |
| `Wall` | `time.Duration` | `wall` | 阶段墙钟 |
| `Targets` | `[]targetStat` | `targets` | 逐目标分解 |

三条源码注释观点值得原样留下：

- `Rounds`/`QPSMin`/`QPSMax` 三件套的注释是：「一轮比一轮差得宽，说明这台宿主太吵、这个数字没有意义。」
- `GatewayMean` 的注释是：「`GatewayMean` 是网关对自己请求时长的看法；它与 `Mean` 的差，就是中继开销加上客户端/服务端网络时间。」
- `Targets` 的注释是：它是多目标 `-urls` 运行的逐目标分解，而**单目标运行也恰好持有一个元素**，好让消费者永远不必特判两种形状；字段顺序在这里是重要的——`Targets` 放最后，让旧产物已经有的每个 key 保持位置，单目标记录在结构上仍与它们字节可比。

数量口径要注意一处不对称：`result.Requests` 是**成功数**（`len(lat)`），而 `targetStat.Requests` 是**派发数**。两者不相等时差额就是错误，见下。

### `type targetStat struct` —— 一个目标在阶段里的份额

```go
type targetStat struct {
	URL      string  `json:"url"`
	Requests int     `json:"requests"`
	Errors   int     `json:"errors"`
	QPS      float64 `json:"qps"`
}
```

源码注释观点（这段决定了第 16 节里那些测试断言为什么长成那样）：

- `Requests` 数的是**派发**给这个目标的请求，不是成功的那些——调用方在乎的轮转保证是「目标被均匀派发」，而即便其中一个在失败，这一点也必须成立；平衡测试查的就是它。
- `Errors` 是 `Requests` 的失败子集。
- 每目标 QPS 是成功数除以阶段 wall，所以各 `targetStat.QPS` 之和等于记录的顶层 `QPS`（浮点上）。

### `type report struct` —— `-all` 的产物

```go
type report struct {
	Started time.Time `json:"started"`
	Host    string    `json:"host"`
	Go      string    `json:"go_version"`
	Rounds  int       `json:"rounds"`
	Phases  []result  `json:"phases"`
}
```

`Host` 来自 `hostname`、`Go` 来自 `goVersion`，两者都在第 13 节。这份结构给基线产物留下了「在谁的机器上、用哪个 Go 版本、跑了几轮」这三条复现前提。

### `type measurements struct` —— 一次请求告诉我们的东西

```go
type measurements struct {
	firstToken time.Duration
	frames     int64
	bytes      int64
}
```

三个字段全部未导出，只在 `doRequest` 与 `runPhase` 之间传。`firstToken` 为 0 表示「没有观察到首 token」——它在流式阶段被解释为「不计入 TTFT 样本」，而不是「首 token 延迟是 0」。这一区分由 `runPhase` 里 `if stream && m.firstToken > 0` 这一行落地。

## 5. `runBaseline` —— 进程内基线

```go
func runBaseline(levels []int, requests, warmup, rounds int, timeout, ttfb time.Duration, out string) error
```

**它量什么**：四个各只差一个变量的阶段（源码文档注释逐条列出）：

| 阶段 | 目标 | 这一阶段回答什么（源码注释观点） |
| --- | --- | --- |
| `direct non-stream` | mock 上游 | 地板：后端自己值多少 |
| `gateway non-stream` | 进程内真网关 | 非流式只增加一次代理跳的成本 |
| `direct stream` | mock 上游 | 流式地板，含首 token 延迟 |
| `gateway stream` | 进程内真网关 | 对 agent 框架真正重要的那个数 |

**为什么 mock 是真子进程**（源码注释观点，两条都保留）：mock 跑在一个真 TCP 端口上的**真子进程**里，而不是进程内的 `httptest` handler。理由 (a)：`cmd/mockupstream/main.go` 拥有 OpenAI 兼容夹具，所以「后端长什么样」在全仓库只有一处定义。理由 (b)：回环 socket 让 `direct` 基线保持诚实——进程内 handler 会跳过内核往返，而网关阶段必须付这一笔，那会以**正好是我们要量的那个量**偏袒网关。构造 mock 时传 `-token-delay 0`，理由同样是注释观点：量 mock 的逐 token sleep 说明不了任何关于网关的事。

**端口与配置**：网关监听 `127.0.0.1:0`，外面套一层 `httptest.NewServer(srv.Handler())`。源码注释观点：端口是临时的，所以基线永远不会跟这台宿主机上别的东西撞上——`:8080` 在这里已被一个无关进程占着。配置是一份手写 `config.Config`：

```go
cfg := &config.Config{
	Server: config.ServerConfig{
		Listen:              "127.0.0.1:0",
		ReadHeaderTimeout:   config.Duration(10 * time.Second),
		IdleTimeout:         config.Duration(90 * time.Second),
		UpstreamTimeout:     config.Duration(timeout),
		ShutdownTimeout:     config.Duration(5 * time.Second),
		MaxBodyBytes:        1 << 20,
		MaxIdleConnsPerHost: 256,
	},
	Upstreams: []config.UpstreamConfig{{
		Name:    "mock",
		Kind:    config.KindOpenAI,
		BaseURL: mock.url,
		Models:  []string{"/"},
	}},
	Log: config.LogConfig{Level: "error", Format: "text"},
	Pricing: config.PricingConfig{
		Default: config.ModelPrice{In: 0.15, Out: 0.60},
	},
}
```

`Models: []string{"/"}` 是 catch-all 写法（`upstream.Target.ServesModel` 对模式 `/` 一律放行，见第 02B 册）。

**错误路径**：`cfg.Validate()` 失败 → `fmt.Errorf("baseline config: %w", err)`；`logging.New` 失败直接返回；`server.NewServer` 失败 → `fmt.Errorf("baseline server: %w", err)`；`startMock` 失败直接返回；`runPhase` 失败直接返回；`writeJSON` 失败直接返回。

**日志为什么被丢弃**：`logging.New(io.Discard, "error", "text")`。源码注释观点：每阶段 800 个请求时日志本身会变成一项可测成本，而这条命令要量的是网关。

**轮次结构**（`round` 外层、`scenario`×`level` 内层）与它的理由，源码注释观点：单趟会先跑 `direct`，而期间热起来的东西（CPU 频率、分配器 arena、TCP 栈）会偏袒或惩罚后跑的那个；轮内交错让各阶段处在同一热与堆状态下，中位数丢弃第一轮离群。

**网关增量抓取**是这一段里最细的一处。每阶段开始前先 `scrapeMetrics(gw.URL, client)` 拿 `beforeMean`/`beforeCount`；`runPhase` 跑完、且阶段名里含 `gateway` 时再抓一次：

```go
if after, afterCount, ft, err := scrapeMetrics(gw.URL, client); err == nil {
	delta, deltaCount := after-beforeMean, afterCount-beforeCount
	if deltaCount > 0 {
		r.GatewayMean = time.Duration(float64(delta) / float64(deltaCount))
	}
	r.GatewayCount = deltaCount
	r.FirstTokenMean = ft
}
```

源码注释观点：网关的计数器是累计的，所以一次裸抓取会把此前每一个阶段与每一条流混进同一个均值里；前后各拍一张快照，只留下**本阶段**的增量。注意三条边界：`deltaCount == 0` 时 `GatewayMean` 保持 0（而不是除零）；`r.GatewayCount` 无条件写成 `deltaCount`（可能是 0，且因 `omitempty` 不会落盘）；`FirstTokenMean` 是**未经差分**的原值 `ft`——`scrapeMetrics` 读的 `infergate_first_token_seconds_mean` 本身已经是一个均值，源码里没有对首 token 做增量处理。这是一处口径不对称，源码注释没有解释，本册标注为**未能确认**（见第 22 节）。

**汇总顺序**：`stream` 先 `false` 后 `true`，`scenarios` 按原序，`levels` 升序；每个三元组 `medianResult(group)` 追加进 `rep.Phases`。源码注释观点：`GatewayMean` 与 `FirstTokenMean` 在运行时已按阶段增量捕获，这里没有什么需要重新抓取的。

**收尾**：`printTable(rep.Phases)`、`printOverhead(rep.Phases)`；`out != ""` 时 `writeJSON(out, rep)` 并打印 `wrote <路径>`。

**逐步打印的格式**（写进 stdout、不进产物）：

```text
  round %d/%d  %-20s c=%-4d qps=%9.1f  p50=%-9s p95=%-9s errs=%d
```

以及开头的两行地址与一行参数摘要 `rounds  %d, n=%d per phase, warmup=%d, ttfb=%s`。

## 6. `medianResult` 与 `medianTargets` —— 轮次归约

### `medianResult`

```go
func medianResult(group []result) result
```

行为，逐条：

1. `len(group) == 1` 时**原样返回** `group[0]`——单轮不做任何加工，连 `Rounds` 都不补写（`group[0].Rounds` 会是 0）。
2. 否则以 `group[0]` 为底，拷贝一份 `sorted`。
3. 内部闭包 `med := func(get func(result) time.Duration) time.Duration`：把各轮的该字段收进切片、`sort.Slice` 升序、取 `vals[len(vals)/2]`。注意这是**上中位**（偶数个时取偏大那个），不是两数平均。
4. `QPS` 单独走一趟：`sort.Float64s(qps)` 后 `out.QPS = qps[len(qps)/2]`，同时 `out.QPSMin = qps[0]`、`out.QPSMax = qps[len(qps)-1]`。
5. `P50`、`P95`、`P99`、`Max`、`Mean`、`TTFTP50`、`TTFTP95`、`TTFTMean` 依次走 `med`。
6. `out.Rounds = len(group)`。
7. `out.Targets = medianTargets(sorted)`。

源码文档注释观点（本册照译）：百分位与均值取**各轮值的中位数**，这是对一台无法被安静下来的宿主最诚实的汇总——它报这趟**典型**做了什么，而不是最好或最差的那一趟；同时算出 QPS spread：如果各轮相差超过几个百分点，这个数字就不可信，读者应当照实说，而不是引用它。

有一处细节值得单独指出：`BytesMean` 与 `FramesMean` **没有**参与归约，`out` 保留的是 `group[0]` 的那两个值。也就是说 3 轮运行时，产物的 `bytes_mean`/`frames_mean` 来自**第一轮**，不是中位数。源码里没有任何注释解释这一点，本册标注为**未能确认**。

### `medianTargets`

```go
func medianTargets(rounds []result) []targetStat
```

行为：

1. `len(rounds) == 0 || len(rounds[0].Targets) == 0` → 返回 `nil`。
2. `out := make([]targetStat, len(rounds[0].Targets))`，以**第一轮**的 `Targets` 长度为形状。
3. 对每个下标 `i`：`out[i].URL = rounds[0].Targets[i].URL`；然后遍历各轮，`if i >= len(r.Targets) { continue }`，收集该轮 `Targets[i]` 的 `QPS`/`Requests`/`Errors` 进三个切片。
4. `len(qps) == 0` → `continue`（留一个零值 `targetStat`）。
5. 三个切片各自排序（`sort.Float64s`、两次 `sort.Ints`）后各取 `[len/2]`。

源码文档注释观点：这是**按位置**归约——阶段的每一轮都被交给同一个有序目标列表，所以下标 `i` 在每一轮里都是同一个目标；`Requests` 与 `Errors` 也取中位数，理由与顶层数字相同：一轮噪声不该变成被引用的计数。

`url` 取自第一轮而不是各轮的中位数，这一点是刻意的（一个字符串没有中位数）；第 16 节的测试里三轮给的是同一个 `URL`，所以无法从测试看出「各轮 URL 不一致时会发生什么」——`URL` 恒等于第一轮的写法意味着它会是第一轮的值。源码无注释，本册不额外推断。

## 7. `runMatrix` 与 `runPhase` —— 阶段驱动

### `runMatrix`

```go
func runMatrix(client *http.Client, targets []string, levels []int, requests, warmup int, timeout time.Duration) ([]result, error)
```

**它做什么**：把一个外部部署扫一遍——两种负载 × 每个并发级别 = `2 × len(levels)` 个阶段。文档注释原话：`targets` 是普通 `-url` 运行里的一个 URL，或整个 `-urls` fleet，每个请求在其上轮转。

**行为**：

1. `for _, stream := range []bool{false, true}`，`label` 取 `"non-stream"` 或 `"stream"`。
2. 内层 `for _, c := range levels`：`r, err := runPhase(client, label, targets, stream, c, requests, warmup, timeout)`；`err != nil` 时 `return nil, err`（**整个矩阵中止**，不是跳过这一格）。
3. `if gm, gc, ft, err := scrapeFleet(targets, client); err == nil { r.GatewayMean, r.GatewayCount, r.FirstTokenMean = gm, gc, ft }`。注意这里与 `runBaseline` 不同：**没有前后差分**，因为 `runMatrix` 是打外部部署，脚本自己负责在两次 `loadtest` 调用之间重启网关或读增量（见第 18–21 节各脚本的做法）。
4. `out = append(out, r)`，打印一行：

```text
%-12s c=%-4d qps=%9.1f  p50=%-9s p95=%-9s p99=%-9s errs=%d
```

三个百分位在这一行里用 `r.P50.Round(time.Microsecond)` 渲染（而不是 `ms`），所以外部部署模式的控制台格式与 `-all` 模式的表格不同。

5. `if len(r.Targets) > 1` 时逐目标再打印：

```text
    %-22s reqs=%-6d errs=%-6d qps=%.1f
```

`%-22s` 用 `shortURL(ts.URL)`。源码注释观点：多目标时上面那一行是 fleet 聚合，所以必须打印每个实例实际扛了什么——一个从未应答的实例在聚合里仍然显得健康。

**错误路径**：只有 `runPhase` 的错误，原样向上传；`scrapeFleet` 的错误被**吞掉**（`err == nil` 才赋值），这与 `scrapeMetrics` 的 best-effort 定位一致：目标不是 InferGate 就没有 `/metrics`，阶段照样算数。

### `runPhase`

```go
func runPhase(client *http.Client, label string, targets []string, stream bool, concurrency, requests, warmup int, timeout time.Duration) (result, error)
```

这是全册最重的函数，逐个细节过。

**入口归一**：`if concurrency < 1 { concurrency = 1 }`。所以 `-c 0` 不会死循环，会静默变成 1（`parseLevels` 已经在更上游把 `< 1` 挡掉了，这一行是二道防线）。

**热身**：

```go
for i := 0; i < warmup; i++ {
	_, base := set.pick()
	if _, err := doRequest(client, base, stream, timeout); err != nil {
		return result{}, fmt.Errorf("warmup against %s: %w", base, err)
	}
}
```

源码文档注释观点：热身阶段发生的东西——连接池建立、TLS（如果有）、路由表与日志设置——都只发生一次；把它们算进去会让第一个阶段因为永远不会复现的原因显得慢。热身走与测量阶段**相同**的轮转，好让每个目标的池都热、而不只是第一个目标的池；否则那个从没收到过热身请求的目标，会在每个测量请求列表的头几个请求上显得慢。热身的测量结果被丢弃（`_`），失败则整阶段 `return`。

**共享状态**：

```go
var (
	mu       sync.Mutex
	lat      = make([]time.Duration, 0, requests)
	ttft     = make([]time.Duration, 0, requests)
	bytesN   int64
	framesN  int64
	errs     int
	firstErr string
	perStat  = make([]targetStat, len(set.targets))
)
```

`perStat` 立刻用 `for i, t := range set.targets { perStat[i] = targetStat{URL: t} }` 填上 URL，所以即使某目标零请求，它在产物里也有一条 `url` 正确、计数为零的行。另有 `frames := make([]int64, requests)` —— 一个按请求序号索引的帧数数组，只在 `idx < len(frames)` 时写，**产物里没有任何字段读它**；它是一次只写不读的分配（见本册第 22 节）。

**worker 池**：

```go
jobs := make(chan int)
var wg sync.WaitGroup
wallStart := time.Now()
for i := 0; i < concurrency; i++ {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for idx := range jobs {
			ti, base := set.pick()
			start := time.Now()
			m, err := doRequest(client, base, stream, timeout)
			elapsed := time.Since(start)

			mu.Lock()
			perStat[ti].Requests++
			if err != nil {
				errs++
				perStat[ti].Errors++
				if firstErr == "" {
					firstErr = err.Error()
				}
				mu.Unlock()
				continue
			}
			lat = append(lat, elapsed)
			bytesN += m.bytes
			framesN += m.frames
			if idx < len(frames) {
				frames[idx] = m.frames
			}
			if stream && m.firstToken > 0 {
				ttft = append(ttft, m.firstToken)
			}
			mu.Unlock()
		}
	}()
}
for i := 0; i < requests; i++ {
	jobs <- i
}
close(jobs)
wg.Wait()
wall := time.Since(wallStart)
```

几处要点：

- **目标在 worker 循环里选，不在起 worker 时选**。源码注释观点：目标在这里按请求选，不是按 worker——共享计数器才是「某个目标比兄弟慢时派发仍然均匀」的原因，见 `targetSet`（第 14 节）。
- **`elapsed` 覆盖的是 `doRequest` 全程**（非流式是整段 body 读完，流式是到 `[DONE]` 或 EOF），`wall` 是整个阶段。所以 `QPS = len(lat) / wall`，而 `P50` 等来自 `elapsed` —— 两个量的分母不同，这是刻意的：QPS 是阶段吞吐，延迟分布是单请求耗时。
- **所有计数都在 `mu` 下改**，包括 `bytesN`/`framesN` 这两个 `int64`（没用 `atomic`）。
- **`firstErr` 只记第一个错误**，且是在错误分支里、在 `continue` 之前写的。
- **`idx` 是派发序号**，与 `jobs` 里送进去的 `i` 一一对应；`frames[idx]` 写的就是第 `idx` 个被派发的请求的帧数——注意派发顺序与完成顺序无关，`idx` 只是「第几个被派发」。

**空样本守卫**：

```go
if len(lat) == 0 {
	return result{}, fmt.Errorf("phase %s c=%d: every request failed (first error: %s)", label, concurrency, firstErr)
}
```

这是「全部失败」与「部分失败」的分界：**一个成功都没有**就整体报错（连同第一个错误），有一个成功就继续出产物并在产物里记 `Errors`。

**排序与统计**：`sort.Slice(lat, ...)`、`sort.Slice(ttft, ...)` 升序。然后每目标的 QPS：

```go
for i := range perStat {
	perStat[i].QPS = float64(perStat[i].Requests-perStat[i].Errors) / wall.Seconds()
}
```

最后拼 `result`（字段与第 4 节一致）：`Label`、`Target: set.label()`、`Stream`、`Concurrency`、`Requests: len(lat)`、`Errors: errs`、`QPS: float64(len(lat)) / wall.Seconds()`、`P50: pick(lat, 0.50)`、`P95: pick(lat, 0.95)`、`P99: pick(lat, 0.99)`、`Max: lat[len(lat)-1]`、`Mean: mean(lat)`、`BytesMean: bytesN / int64(len(lat))`、`FramesMean: float64(framesN) / float64(len(lat))`、`Wall: wall`、`Targets: perStat`。`len(ttft) > 0` 才填 `TTFTP50`、`TTFTP95`、`TTFTMean`。

`Max` 直接从已排序的 `lat` 末位取，`BytesMean`/`FramesMean` 只统计**成功**请求（分母是 `len(lat)`），与 `Requests` 的口径一致。

**两条运行时提示**（都只打印、不进产物）：

```go
if r.Errors > 0 {
	fmt.Printf("  warning: %d/%d requests failed in %s c=%d (%s)\n",
		r.Errors, r.Requests+r.Errors, label, concurrency, firstErr)
}
```

分母写作 `r.Requests+r.Errors` 而不是底层派发总数，是因为派发总数正好等于这两个之和（每个请求非成功即失败）。

```go
perGoroutine := wall / time.Duration(max(1, requests/concurrency))
if perGoroutine < 250*time.Microsecond {
	fmt.Printf("  note: %s c=%d is generator-bound (%s per request per goroutine); QPS here measures this client, not the target\n",
		label, c, perGoroutine.Round(time.Microsecond))
}
```

`max(1, ...)` 防的是 `requests < concurrency` 时除零。阈值 `250*time.Microsecond` 是「一个调度量子」这个说法在源码注释里的量化形式（源码注释观点）；注意这一行在**每次** `runPhase` 结束时都可能打，所以它也会出现在 `-diag` 的每一行配置下。

**文档注释里记的两条多目标限制**（本册照译，因为它们是使用这个方法时必须知道的）：其一，对**任何**目标的热身失败都是整个阶段致命，与单目标时完全一样——fleet 里一个死掉的部署会停住整趟，而不是安静地只量幸存者。其二，`warmup` 是**聚合**预算而不是每目标预算——四个目标配 `-warmup 100` 时每个只见到约 25 次热身，所以当某个目标的池需要更久才稳定时，应当按 fleet 规模放大这个 flag。

## 8. `doRequest`、`chatBody`、`streamBody`、`countingReader` —— 一次请求

### 两个请求体

```go
var chatBody   = []byte(`{"model":"mock-gpt","messages":[{"role":"user","content":"hello"}]}`)
var streamBody = []byte(`{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
```

两个包级 `[]byte`，内容只差 `"stream":true`。模型名写死 `mock-gpt`，提示词写死 `hello`——所以这个生成器量的是**固定负载**下的吞吐与延迟，而不是内容相关的工作量；这一点是「同一负载有/无网关比较」这个前提的一部分。两者都在包初始化时分配一次，`doRequest` 只按 `stream` 取其一，不重新构造。

### `doRequest`

```go
func doRequest(client *http.Client, base string, stream bool, timeout time.Duration) (measurements, error)
```

**行为**，按源码顺序：

1. `ctx, cancel := context.WithTimeout(context.Background(), timeout)` + `defer cancel()`。每一次请求都有自己的超时 context——这一点在多目标轮转下很重要：某个目标卡住不会拖住整阶段。
2. `body := chatBody; if stream { body = streamBody }`。
3. `http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))`。路径是**拼**上去的，这就是 `normalizeTarget` 必须去掉尾斜杠的原因。
4. 两个请求头：`Content-Type: application/json`、`Authorization: Bearer loadtest-key`。令牌写死 `loadtest-key`；`-all`/`-diag` 造出的进程内网关不带访问策略，而打外部部署时是否放行取决于那个部署的配置。
5. `resp, err := client.Do(req)`；`defer resp.Body.Close()`。
6. **非 200 一律算错误**，并把 body 前 256 字节带进错误消息：

```go
if resp.StatusCode != http.StatusOK {
	preview, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return measurements{}, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
}
```

`preview` 的读错误被丢弃（`_`）——状态码已经在消息里了。

7. **非流式**：`n, err := io.Copy(io.Discard, resp.Body)`，返回 `measurements{bytes: n}`，`frames` 与 `firstToken` 都是零值。
8. **流式**（这一段是核心）：

```go
start := time.Now()
var m measurements
counting := &countingReader{r: resp.Body}
reader := sse.NewReader(counting)
for {
	frame, err := reader.NextFrame()
	if err != nil {
		if err == io.EOF {
			break
		}
		return measurements{}, err
	}
	if frame.IsComment() || len(frame.Data) == 0 {
		continue
	}
	if frame.IsDone() {
		break
	}
	var payload struct {
		Choices []struct {
			Delta struct {
				Content   string          `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(frame.Data, &payload); err != nil {
		return measurements{}, fmt.Errorf("invalid frame payload %q: %w", truncate(string(frame.Data), 80), err)
	}
	m.frames++
	if m.firstToken == 0 && len(payload.Choices) > 0 {
		d := payload.Choices[0].Delta
		if d.Content != "" || len(d.ToolCalls) > 0 {
			m.firstToken = time.Since(start)
		}
	}
}
m.bytes = counting.n
return m, nil
```

逐点说明：

- **`start` 在第一个 `NextFrame` 之前取**，所以 TTFT 里包含连接复用/建立的开销；它计的是「从开始读到首个有内容的 delta」。
- **注释里的一句源码注释观点**：流式用网关**同一个** frame reader 解析，所以测量看到的就是真实客户端看到的帧；畸形帧是错误、不是被静默跳过的字节——而这一点正是抓「中继在高负载下丢掉空行分隔符」的手段。
- **`m.frames++` 在 JSON 解析成功之后**，所以 frame 计数只算合法数据帧；注释帧、空数据帧与 `[DONE]` 都不计。
- **首 token 判定**：`m.firstToken == 0` 是「还没记过」的哨兵，`len(payload.Choices) > 0` 之后只看 `Choices[0].Delta`；`d.Content != "" || len(d.ToolCalls) > 0` 才落时间戳。一个只有 `role` 的空 delta 不计。
- **`m.bytes = counting.n` 在循环之后取**，包含 `[DONE]` 帧本身与所有分隔符的字节——它是**上线字节**，不是 payload 字节。这与 `countingReader` 的注释「数真正上线交付的字节，用来与网关自己的 stream_bytes 计数器对照」一致。
- **`io.EOF` 与 `[DONE]` 是两条正常出口**；两者都跑完 `m.bytes` 赋值后返回。`NextFrame` 的其他错误（例如帧超过上限）直接向上抛，**丢掉了已经积累的部分测量**——它回到 `runPhase` 里被记为一次错误请求。

### `countingReader`

```go
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
```

行为：把每次 `Read` 实际返回的字节数累加进 `n`，`err` 原样透传。它只被 `doRequest` 用一次，包在 `resp.Body` 外面，再交给 `sse.NewReader`。源码注释观点：**一个** reader 套在计数 reader 上——对同一个 body 再开第二个 `sse.NewReader` 会从第一个那里偷走字节并让分帧失步。也就是说这里的计数与分帧必须是同一条读路径，不能各读一遍。

`countingReader` 只在流式分支里被构造；非流式的字节数来自 `io.Copy` 的返回值，不走这个类型。

## 9. `runDiagnosis` —— 诊断网关成本

```go
func runDiagnosis(concurrency, requests, warmup, rounds int, timeout time.Duration) error
```

**它要回答的问题**（源码文档注释）：基线表提出了一个问题但它定不了——网关比直连后端慢约 3 倍，而「中间多了一个 Go HTTP 服务器」不是解释，是一个有三个竞争变体的**假设**。于是逐个量。

**五个 arm**，注释里逐个定义：

| arm | 构造 | 隔离出什么（源码注释观点） |
| --- | --- | --- |
| `direct (1 hop)` | 直接打 mock | 一跳的地板 |
| `plain httputil pool=2` | `net/http/httputil` 单主机反向代理 + 进程级 `http.DefaultTransport` | 一个额外 TCP 跳的价钱 |
| `tuned httputil pool=256` | 同一个 httputil 代理，但 `Transport` 换成网关用的那个 | 把连接池与代理实现本身隔离开 |
| `minimal passthrough pool=256` | 手写代理，共享真连接池，不解析 body | 池化之后还剩下的「中继本身」 |
| `infergate pool=256` | 真网关 | 网关自身逻辑加上的那一层 |

读法（源码注释观点）：`plain` 减 `direct` 是这台宿主上一个额外跳的价钱（两套 TCP 握手、两套 syscall，因为每一跳有自己的连接）；`tuned` 减 `plain` 是连接池效应；`infergate` 减去 `tuned` 与 `minimal` 中较好的那个，才是网关自身逻辑的成本——**那才是值得优化的数**。

**构造细节**：

- `mock, err := startMock(0)`，`defer mock.stop()`——TTFB 传 0，诊断不注入人造延迟。
- `reg, err := upstream.New(&config.Config{...})`，`defer reg.CloseIdleConnections()`；`target, ok := reg.Target("mock")`，`!ok` 时返回 `fmt.Errorf("registry lost the mock target")`。注意 `minimal` arm 与 `tuned` arm 用的都是 `target.Transport`，也就是**网关自己的**连接池。
- `plainTarget, _ := url.Parse(mock.url)`，解析错误被丢弃（`_`）——`mock.url` 是 `startMock` 拼出来的 `http://` + 地址，解析失败无从处理。
- `plain := httptest.NewServer(httputil.NewSingleHostReverseProxy(plainTarget))`。源码注释观点：`http.DefaultTransport` 的 idle 池是每主机 2 条连接，所以在 c=32 时大多数请求要付一次全新的 TCP 握手。
- `tuned` 就是把 `tunedProxy.Transport = target.Transport` 之后的同一个代理塞进 `httptest.NewServer`。
- `minimal` 是手写 handler，逐行：

```go
minimal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	out := r.Clone(context.Background())
	out.URL.Scheme = "http"
	out.URL.Host = strings.TrimPrefix(mock.url, "http://")
	out.RequestURI = ""
	out.Host = out.URL.Host
	out.ContentLength = r.ContentLength
	resp, err := target.Transport.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}))
```

它 `Clone` 到 `context.Background()`（与网关自己「出站 context 不继承入站」的做法一致，见第 02A 册），把 `RequestURI` 清空好让 `RoundTrip` 接受它，复制全部响应头，然后原样搬 body。`io.Copy` 的错误被丢弃。

- 真网关那一段与 `runBaseline` 几乎一样，唯一差别是**不设 `Pricing`**（诊断不关心成本）。

**执行与输出**：

1. 打印 `diagnosis at c=%d, n=%d per phase, median of %d rounds`。
2. `rows` 是五个 `{label, target}` 的有序切片。
3. `collected := make(map[string][]result, len(rows))`；`round` 外层、`row` 内层，每次 `runPhase(client, row.label, []string{row.target}, false, concurrency, requests, warmup, timeout)` —— **`stream` 恒为 `false`**，诊断只跑非流式。错误直接返回。每行打印 `  round %d/%d  %-32s qps=%9.1f`。
4. 源码注释观点：轮次在外、配置在内——每个配置在同一轮里看到相同的热与堆条件，中位数丢弃热身轮。

**汇总**：遍历 `rows` 顺序取 `medianResult(collected[row.label])`；`baseline` 取**第一行**（即 `direct (1 hop)`）的 `QPS`；`baselineSpread` 只在 `r.QPSMin > 0` 时算成 `(r.QPSMax-r.QPSMin)/r.QPS*100`；`share = r.QPS / baseline * 100`（`baseline > 0` 时）；`spread` 在 `r.QPS > 0 && r.QPSMin > 0` 时算同样的式子。每行打印：

```text
%-32s qps=%9.1f (%5.1f%% of direct, spread %.0f%%)  p50=%-9s p95=%-9s p99=%-9s
```

然后：

```go
if baselineSpread > 5 {
	fmt.Printf("NOTE: the direct baseline itself varied %.0f%% across rounds, so treat differences below that as noise.\n", baselineSpread)
}
```

**5% 这个阈值**是源码里唯一的显式噪声门限：连 `direct` 自己都抖超过 5% 时，比它更小的差异就是噪声。最后两行是给人的读法提示：

```text
read it as: hop cost = plain vs direct; pool effect = tuned vs plain;
            gateway logic cost = infergate vs the better of tuned/minimal.
```

**错误路径汇总**：`startMock`、`upstream.New`、`reg.Target` 未命中、`logging.New`、`cfg.Validate`、`server.NewServer`、任一 `runPhase` —— 全部直接返回错误。`runDiagnosis` **不写任何文件**，即使调用方给了 `-out`。

## 10. `mockProc`、`startMock`、`mockBinary` —— mock 子进程

### `type mockProc struct`

```go
type mockProc struct {
	url string
	cmd *exec.Cmd
	log *os.File
	logPath string
	done    chan struct{}
}
```

源码注释观点（这段解释了 `logPath` 为什么可能是空串）：这个类型是一个运行中的 `cmd/mockupstream` 子进程；`logPath` 是子进程的输出去了哪里，而当临时目录不可写、输出只能被丢弃时它是**空的**——调用方与启动失败信息读它，用来把「子进程什么都没打印」与「根本没有日志可读」区分开。`done` 由一条 `cmd.Wait()` 的 goroutine 在退出时 `close`，所以它既是「进程已退出」的信号，也是 `stop` 用来等待的把手。

### `startMock`

```go
func startMock(ttfb time.Duration) (*mockProc, error)
```

**行为**，按源码顺序：

1. `bin, err := mockBinary()`；失败直接返回。
2. 选端口：`l, err := net.Listen("tcp", "127.0.0.1:0")`，`addr := l.Addr().String()`，然后 `_ = l.Close()`。**先监听再关掉再交给子进程**——这中间有一个理论上的竞态窗口（别的进程可能抢到同一个端口），源码里没有处理它。文档注释观点：端口由 OS 选、并从 `net.Listen` 读回——写死 9000 会撞开发者自己的 mock，而这台宿主机的 `:8080` 已被一个无关进程占用。
3. 参数：`[]string{"-listen", addr, "-name", "loadtest-mock", "-token-delay", "0"}`；`ttfb > 0` 时追加 `"-ttfb", ttfb.String()`。`-token-delay 0` 的理由见第 5 节。
4. **日志走文件而不是管道**——这是本册最重要的一条环境约束，源码注释观点照译：子进程写 FILE，不写 pipe。在这台宿主机的沙箱下，进程无法打开 `exec.Cmd` 用于管道 stdio 的命名管道，而被卡住的子进程看起来与慢后端**一模一样**。文件避开了整类问题，并且还能在启动失败时充当诊断。
5. **日志失败不能拖垮整趟运行**（源码注释观点）：日志是调试辅助，所以它永远不能成为一次运行失败的原因。在受限的 `TEMP` 下 `os.Create` 会被拒，而在那里返回错误会因为一个没人需要的文件把 `-all` 与 `-diag` 一起带下去。于是退回空设备——它仍然是一个真文件句柄、仍然不是管道，所以上面那套沙箱推理仍然成立——并且**把这件事说出来**，因为稍后缺失的日志尾巴必须能与一条空日志区分开。

```go
logPath := filepath.Join(os.TempDir(), fmt.Sprintf("infergate-loadtest-mock-%d.log", os.Getpid()))
logFile, err := os.Create(logPath)
if err != nil {
	fmt.Printf("  note: cannot create mock log %s (%v); the mock's output is discarded\n", logPath, err)
	logPath = ""
	logFile, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("create mock log: %w (and %s: %v)", err, os.DevNull, err)
	}
}
cmd.Stdout = logFile
cmd.Stderr = logFile
```

日志文件名带 `os.Getpid()`，所以同一台机器上并发跑两次 `loadtest` 不会互相覆盖。两路退出码（stdio/stdout 与 stderr）指向**同一个**文件句柄。

6. `if err := cmd.Start(); err != nil { logFile.Close(); return nil, fmt.Errorf("start %s: %w", bin, err) }` —— 启动失败时关掉日志文件（否则句柄泄漏）。
7. `m := &mockProc{url: "http://" + addr, cmd: cmd, log: logFile, logPath: logPath, done: make(chan struct{})}`；随后起 goroutine：

```go
go func() {
	_ = cmd.Wait()
	close(m.done)
}()
```

`cmd.Wait()` 的错误被丢弃——`done` 只表示「结束了」。

8. **健康等待**：`client := &http.Client{Timeout: 500 * time.Millisecond}`，`deadline := time.Now().Add(15 * time.Second)`，循环里：

```go
select {
case <-m.done:
	tail := m.logTail()
	m.stop()
	return nil, fmt.Errorf("mock exited during startup: %s", tail)
default:
}
resp, err := client.Get(m.url + "/healthz")
if err == nil {
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return m, nil
	}
}
time.Sleep(50 * time.Millisecond)
```

也就是：**先看进程是否已经死了**（死了就把日志尾巴带进错误），否则每 50ms 探一次 `/healthz`，每次探测有 500ms 超时。文档注释观点：等到它应答 `/healthz`，负载测试就永远不会与后端启动赛跑。

9. **超时**：`m.stop()` 之后分两种错误文本：

```go
if m.logPath == "" {
	return nil, fmt.Errorf("mock at %s did not become healthy within 15s (log unavailable, temp dir not writable)", m.url)
}
return nil, fmt.Errorf("mock at %s did not become healthy within 15s (log: %s)", m.url, m.logPath)
```

两条文本的差别就是「没有日志」与「日志在这里」——这与 `logTail` 的设计是同一件事的两面。`startMock` 的错误路径共有四种：`mockBinary` 失败、`net.Listen` 失败、`cmd.Start` 失败（带日志文件创建失败的复合错误）、启动期退出或 15s 未健康。

### `logTail`

```go
func (m *mockProc) logTail() string
```

行为三分支：

1. `m.logPath == ""` → 返回字面量 `"log unavailable, temp dir not writable"`。
2. `os.ReadFile(m.logPath)` 失败 → `fmt.Sprintf("log unavailable: %v", err)`。
3. 成功 → `strings.TrimSpace(string(tail))`。

源码文档注释观点：这是 mock 打印过的东西，用于启动失败信息；当临时目录不可写时根本没有文件，而**说出这一点**比一条读起来像「子进程什么都没说」的空尾巴更有用。注意第 3 支不做长度截断——mock 的输出有多长，错误信息就有多长。

这个方法是少数**不需要真进程**就能测的：第 16 节里 `TestMockLogTailDegradesWithoutLogFile` 直接构造 `&mockProc{}` 与一个 `logPath` 指向不存在文件的 `mockProc`，把前两个分支钉住。

### `stop`

```go
func (m *mockProc) stop()
```

行为：

```go
if m.cmd != nil && m.cmd.Process != nil {
	_ = m.cmd.Process.Kill()
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
	}
}
if m.log != nil {
	m.log.Close()
}
```

三处要点：用 `Kill()` 而不是「礼貌地关闭」（这是一个为了测量而起的临时进程，没有优雅退出的契约，而且它可能已经卡住）；等 `done` 但**最多 5 秒**（所以 `stop` 是幂等且不会永久阻塞的）；无条件关日志文件。它对**两次**调用是安全的：第二次 `cmd.Process.Kill()` 会返回错误（被 `_` 丢弃），`done` 已关闭所以立即通过，`m.log.Close()` 第二次返回错误（被忽略）——这也是为什么 `startMock` 在 `m.done` 分支里可以先 `logTail()` 再 `stop()`。

### `mockBinary`

```go
func mockBinary() (string, error)
```

文档注释：找一个预构建的、与网关并排的 `mockupstream`，或者仓库还没构建时把它建进 OS 临时目录。

行为：

1. 按顺序试两个候选路径：`filepath.Join("bin", "mockupstream.exe")` 与 `filepath.Join("bin", "mockupstream")`。每个都要过 `filepath.Abs` + `os.Stat` + `!st.IsDir()`，全通过就返回**绝对路径**（`filepath.Abs` 的错误也被丢弃）。
2. 两个都不在，就现场构建：

```go
out := filepath.Join(os.TempDir(), "infergate-mockupstream.exe")
if os.PathSeparator != '\\' {
	out = filepath.Join(os.TempDir(), "infergate-mockupstream")
}
build := exec.Command("go", "build", "-o", out, "./cmd/mockupstream")
build.Stdout = os.Stderr
build.Stderr = os.Stderr
if err := build.Run(); err != nil {
	return "", fmt.Errorf("mockupstream not built and no `go` on PATH to build it (%v); run tools/go.cmd build -o bin/mockupstream.exe ./cmd/mockupstream", err)
}
return out, nil
```

注意三点：目标路径用 `os.PathSeparator` 判断来决定要不要 `.exe`（Windows 上要，类 Unix 上不要）；构建命令直接叫 `go`（**不是** `tools/go.cmd`），所以本机 `go` 不存在或 toolchain 版本不对时这条会失败，而错误文本明确给出了替代做法 `tools/go.cmd build ...`；构建的 stdio 接 `os.Stderr`，所以编译错误会直接出现在用户眼前而不是被吞掉。还有一处**源码注释与实现的差异**：注释说这是「仓库还没构建时」的一次性兜底，但第 1 步只看 `bin/` 下那两个候选，所以第 2 步建在 OS 临时目录里的那份产物**下一次运行不会被复用**，而是重新构建。源码没有对应注释，本册标注为**未能确认**其是否刻意（见第 22 节）。

## 11. `scrapeMetrics` 与 `scrapeFleet` —— 读网关自己的数

这一节是「客户端实测」与「网关自报」两条口径的交界处。

### 三个包级正则

```go
var (
	reRequests = regexp.MustCompile(`infergate_requests_total\{[^}]*\}\s+(\d+)`)
	reDuration = regexp.MustCompile(`infergate_request_duration_seconds_sum\{[^}]*\}\s+([0-9.eE+-]+)`)
	reFirstTok = regexp.MustCompile(`infergate_first_token_seconds_mean\{[^}]*\}\s+([0-9.eE+-]+)`)
)
```

三条都是「Prometheus 文本 exposition 的逐行抓取」，共同形状是「指标名 + 一串花括号标签 + 空白 + 数值」。三个可注意之处：前者只接受**整数**（`(\d+)`），后两者接受浮点/科学计数（`([0-9.eE+-]+)`）；它们匹配的是**整行**而不是带 `# TYPE` 前缀的行，所以 `infergate_requests_total` 会同时匹配 `# TYPE infergate_requests_total counter` 吗——不会，因为 `# TYPE` 行里指标名后面跟的是空格而不是 `{`，而模式要求紧接着 `{`；标签部分 `[^}]*` 不能为空但可以匹配任意内容，所以**带不带标签的行都会被匹配**。这与第 02C、05 册里 `/metrics` 的实际输出形状一致。

### `scrapeMetrics`

```go
func scrapeMetrics(base string, client *http.Client) (mean time.Duration, count int, firstToken time.Duration, err error)
```

**它读什么**：`client.Get(strings.TrimRight(base, "/") + "/metrics")`，把整个响应体限制在 `1<<20` 字节内读成字符串。

**它算什么**：

- 对 `reRequests` 的**每一个**匹配做 `strconv.Atoi`（忽略错误）并累加进 `totalCount`；
- 对 `reDuration` 的**每一个**匹配做 `strconv.ParseFloat`（忽略错误）并累加进 `totalSeconds`；
- `totalCount > 0 && totalSeconds > 0` 时：

```go
mean = time.Duration(totalSeconds / float64(totalCount) * float64(time.Second))
```

- `reFirstTok` 只取**第一个**匹配（`FindStringSubmatch`，非 `All`）：

```go
if m := reFirstTok.FindStringSubmatch(text); m != nil {
	f, _ := strconv.ParseFloat(m[1], 64)
	firstToken = time.Duration(f * float64(time.Second))
}
```

**它返回什么**：`mean`（网关自报的请求时长均值）、`count`（各 `infergate_requests_total` 之和，也就是**计数器总数**而不是本阶段增量）、`firstToken`（网关自报的首 token 均值）、`err`。

**错误路径**：HTTP 请求失败直接返回该错误；读 body 失败返回该错误。**解析错误不产生错误**——`Atoi`/`ParseFloat` 的错误全被丢弃，一个畸形数字会被当成 0 计入。

**两处口径细节值得单独点出**：

- 注释说这是 **best-effort**：一个不是 InferGate 的目标根本不暴露该端点，而**阶段仍然计数**。调用方（`runMatrix`）正是据此吞掉错误的。
- `count` 是**累计计数**，不是本阶段的数量。`runBaseline` 之所以要前后两次快照相减，就是为了把这个累计量变成阶段增量；而 `scrapeFleet` 的加权正是直接用这个累计量当权重——两者都对，因为加权只关心各实例之间的相对忙闲。

### `scrapeFleet`

```go
func scrapeFleet(targets []string, client *http.Client) (mean time.Duration, count int, firstToken time.Duration, err error)
```

**它解决什么**：一个网关只统计被路由到它的东西，所以一个 fleet 的均值必须按各实例的请求数**加权**。源码注释观点：对每实例均值取简单平均，会让一个空闲实例与一个忙碌实例一样响。

**行为**：

1. **单目标直接透传**：

```go
if len(targets) == 1 {
	return scrapeMetrics(targets[0], client)
}
```

源码注释观点：单目标情形直接透传，好让单目标记录保持旧产物拥有的**确切数字**，而不引入一个 `count/count` 权重会带来的浮点往返。这一行的效果是「单目标与多目标走两条口径」，是刻意为了产物可比性。

2. 多目标时累加：`meanNs += float64(m) * float64(c)`、`count += c`、`ft > 0` 时 `firstNs += float64(ft)` 且 `firstN++`；出错的目标记 `lastErr` 并 `continue`，但只要有一个成功就 `ok = true`。
3. **全部失败**：

```go
if !ok {
	if lastErr == nil {
		lastErr = fmt.Errorf("no targets to scrape")
	}
	return 0, 0, 0, lastErr
}
```

`targets` 为空时循环一次都不进、`ok` 保持 false、`lastErr` 为 nil，于是拿到 `no targets to scrape` 这条兜底错误。

4. 归一：

```go
if count > 0 {
	mean = time.Duration(meanNs / float64(count))
}
if firstN > 0 {
	firstToken = time.Duration(firstNs / float64(firstN))
}
```

`mean` 是**计数加权**的均值（等价于「总秒数 / 总请求数」），而 `firstToken` 是**各实例首 token 均值的简单平均**（分母是 `firstN`，即报出了非零首 token 的实例数）。两者口径不同：一个按请求量加权，一个按实例数平均。源码注释只解释了前者为什么加权，没有解释后者为什么不加权——本册标注为**未能确认**（见第 22 节）。

**错误路径**：单目标时原样透传 `scrapeMetrics` 的错误；多目标时返回**最后一个**错误（`lastErr`），或者在没有目标时报 `no targets to scrape`；部分目标失败**不算**错误，只要有一个成功。

## 12. 呈现层：`shortTarget`、`shortURL`、`printTable`、`printOverhead`、`streamLabel`、`pct`、`ms`、`writeJSON`

### `shortTarget`

```go
func shortTarget(raw string) string
```

行为：`strings.Split(raw, ",")` 之后对**每个**成员调 `shortURL`，再 `strings.Join(parts, ",")`。源码文档注释观点：把一个 base URL 渲染成 `host:port`，好让逐阶段的表保持窄；`"direct"` 这个名字保留给进程内后端基线；而一个多目标记录带的是**整条逗号连接的 fleet**，所以每个成员被分别缩短，而不是让一次 URL 解析在逗号上失败、把两条完整 URL 都打印出来。它只被 `printTable` 调用。

### `shortURL`

```go
func shortURL(raw string) string
```

行为：

```go
u, err := url.Parse(raw)
if err != nil || u.Host == "" {
	return raw
}
return u.Host
```

两种退化情形（解析失败、或解析成功但没有 host）都返回**原字符串**，所以一个写错的地址在表里会原样显眼地出现，而不是变成空字符串。调用者有两个：`shortTarget`（逐成员）与 `printTable`/`runMatrix` 的逐目标行。

### `printTable`

```go
func printTable(phases []result)
```

它打的是一张 markdown 表（`-all` 与 `-url`/`-urls` 两种模式共用）。

1. 表头与分隔行是两条字面量：

```text
| workload | target | c | QPS | P50 | P95 | P99 | TTFT P50 | TTFT P95 | errors |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
```

2. **目标名怎么定**，这一段带注释，逐支：

```go
target := "direct"
switch {
case strings.HasPrefix(r.Label, "direct"):
	target = "direct"
case strings.Contains(r.Label, "gateway"):
	target = "gateway"
case r.Target != "":
	target = shortTarget(r.Target)
}
```

源码注释观点：当标签自己说清它是什么时就从标签取名，否则从 URL 取；旧的规则（「标签里没有 `gateway` 就当 `direct`」）会把 `-url` 模式里**调用方自己的端点**印成 `direct`，而在一张经常被粘进报告的表里，那读起来就是「这是无网关的基线」。所以现在是**白名单式**：只有标签真的以 `direct` 开头才写 `direct`，否则退回真实目标。

3. **TTFT 两列**：`ttft50, ttft95 := "-", "-"`；只有 `r.Stream` 为真才填 `ms(r.TTFTP50)` / `ms(r.TTFTP95)`。所以非流式行的这两列是占位符，而不是 0。
4. 数据行：

```go
fmt.Printf("| %s | %s | %d | %.1f | %s | %s | %s | %s | %s | %d |\n",
	strings.ReplaceAll(r.Label, "gateway ", ""), target, r.Concurrency, r.QPS,
	ms(r.P50), ms(r.P95), ms(r.P99), ttft50, ttft95, r.Errors)
```

注意 `workload` 列写的是 `strings.ReplaceAll(r.Label, "gateway ", "")` —— 因为 `target` 列已经把「gateway」说了一遍，workload 列只留 `non-stream`/`stream`。这也意味着一个名字里含 `gateway ` 的自定义标签会被改写；`-all`/`-url` 两种模式的标签都符合这个约定。

5. `if len(r.Targets) > 1` 时逐目标缩进打印 `    %-22s reqs=%-6d errs=%-6d qps=%.1f`（`shortURL(ts.URL)`、`ts.Requests`、`ts.Errors`、`ts.QPS`）。源码注释观点：上面那一行在多目标时是 fleet 总数，下面的逐目标行说明这个总数是怎么分的——而那正是横向扩容运行的全部要点。这套缩进行在 markdown 里**不是**表格行，所以复制这张表时它们会被当成普通文本带过去。

### `printOverhead`

```go
func printOverhead(phases []result)
```

它只被 `runBaseline` 调用（`-url` 模式的产物里没有配对的 `direct`/`gateway` 阶段，所以不适用）。

**第一段：QPS 与尾延迟的配对差**。先打印 `gateway overhead vs direct:`，然后 `stream` 取 `false`、`true` 两轮，每轮里对每个含 `gateway` 的 `g`，再扫一遍找「同 `stream`、不含 `gateway`、同 `Concurrency`」的 `d`，打印：

```text
  %-9s c=%-4d  QPS %8.1f -> %8.1f (%+.1f%%)   P95 %8s -> %8s (%+s)   P99 %+s
```

实参依次是 `streamLabel(stream)`、`g.Concurrency`、`d.QPS`、`g.QPS`、`pct(g.QPS, d.QPS)`、`ms(d.P95)`、`ms(g.P95)`、`ms(g.P95-d.P95)`、`ms(g.P99-d.P99)`。三处细节：内层配对**没有** `break`，所以一个 direct 阶段若与多个 gateway 阶段同并发同负载，会各配一次（在当前四场景下不会发生）；`P95` 那一列打印的是 `d.P95 -> g.P95` 两个绝对值**加上**差值，而 `P99` 只打差值；`%+s` 与 `%+s` 用于时长差，负差会带 `-` 号。

源码文档注释观点：这才是该引用的数字——「网关花了 X ms 的 P95」，而不是「网关做 Y QPS」（后者完全取决于它背后的后端）。

**第二段：客户端实测均值 vs 网关自报均值**。对每个 `r.GatewayMean > 0` 的阶段：

```go
share := 100 * float64(r.GatewayMean) / float64(r.Mean)
if share < 50 {
	fmt.Printf("  client-observed mean %s at c=%d, of which the gateway's own processing span is only %s (%.1f%%): the load generator is the slower half here, not the gateway\n",
		ms(r.Mean), r.Concurrency, ms(r.GatewayMean), share)
} else {
	fmt.Printf("  gateway-internal mean %s vs client-observed mean %s at c=%d (delta %s: hop + relay overhead)\n",
		ms(r.GatewayMean), ms(r.Mean), r.Concurrency, ms(r.Mean-r.GatewayMean))
}
```

源码注释观点（这段是本册里最值得原样保留的一处自省）：当网关自己的 span 远低于客户端实测时，差额**不是**网关开销——网关早就做完了，回去等了。在这台宿主上，一旦 mock 没有人造延迟，客户端（等价 curl 的 Go HTTP + SSE 解析 + JSON 解码）就是较慢的那一半，所以诚实的读法是「这里大部分是客户端」，而这一行就这么说，而不是暗示网关烧掉了那些时间。

`50%` 是源码里唯一的硬阈值；`r.Mean == 0` 且 `r.GatewayMean > 0` 时这个除法会得到 `+Inf`，走 `else` 分支（因为 `Inf < 50` 为假）。

### `streamLabel`

```go
func streamLabel(stream bool) string
```

`true` → `"stream"`，`false` → `"non-stream"`。只被 `printOverhead` 调用。它与 `runMatrix` 里内联的那三行（`label := "non-stream"; if stream { label = "stream" }`）是同一份映射的两种写法——一处是函数，一处是内联，源码里没有解释为什么没有统一。

### `pct`

```go
func pct(newV, oldV float64) float64
```

`oldV == 0` → 返回 0（避免 `+Inf`）；否则 `(newV - oldV) / oldV * 100`。注意参数顺序：`pct(newV, oldV)`，而 `printOverhead` 调用的是 `pct(g.QPS, d.QPS)` —— 也就是 `pct(网关, 直连)`，正数表示网关更快。只被 `printOverhead` 调用。

### `ms`

```go
func ms(d time.Duration) string
```

把一个 `time.Duration` 渲染成人类可读的短串，三分支：

| 条件 | 格式 | 例子 |
| --- | --- | --- |
| `d == 0` | 字面量 `"0"` | `0` |
| `d < time.Millisecond` | `fmt.Sprintf("%.0fus", float64(d)/float64(time.Microsecond))` | `250us` |
| `d < time.Second` | `fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))` | `1.25ms` |
| 其余 | `fmt.Sprintf("%.3fs", d.Seconds())` | `1.500s` |

它是全册使用面最广的呈现函数：`printTable`、`printOverhead`、`runBaseline` 的逐轮行都调它。注意零值走的是**特例分支**而不是 `%.0fus`——否则 `0` 会被印成 `0us`，而 `omitempty` 留下的空字段在读表的人眼里应当就是「没有这个数」。

### `writeJSON`

```go
func writeJSON(path string, v any) error
```

行为：

```go
raw, err := json.MarshalIndent(v, "", "  ")
if err != nil {
	return err
}
return os.WriteFile(path, append(raw, '\n'), 0o644)
```

三处要点：缩进两个空格、文件末尾补一个换行、文件权限 `0o644`。参数类型是 `any`，所以两种产物形状（`[]result` 与 `report`）共用它。**它不做任何目录创建**——父目录不存在时 `os.WriteFile` 直接返回错误，`main` 与 `runBaseline` 都会 `fatalf` 掉。`cmd/measure-m2/main.go` 里有一个同名同形的函数（第 17 节），两者是各自独立的副本，没有共享。

## 13. `newClient`、`parseLevels`、`fatalf`、`hostname`、`goVersion`

### `newClient`

```go
func newClient(timeout time.Duration) *http.Client
```

返回的客户端：

```go
&http.Client{
	Timeout: timeout,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}
```

源码注释观点两条，逐字翻译：keep-alive 在这里比平时更重要——用默认 transport 时，本进程会表现得和别的任何 Go 客户端一样，**比较才是公平的**；`MaxIdleConnsPerHost` 被抬高，是因为负载发生器自己否则会在 c=128 时变成瓶颈（Go 的默认值是每主机 2 条 idle 连接，在并发下会强制重连抖动）。

注意它把 `http.Client.Timeout` 与每个请求的 context 超时都设成了同一个值：`doRequest` 用 `context.WithTimeout(..., timeout)`，而客户端总超时也是 `timeout`。两者同时生效时谁先到谁生效。调用者有三处：`main`（`-url`/`-urls` 模式）、`runBaseline`、`runDiagnosis`。

### `parseLevels`

```go
func parseLevels(s string) ([]int, error)
```

行为：按 `,` 切分，逐段 `strings.TrimSpace`，空段跳过，`strconv.Atoi` 失败 → `fmt.Errorf("-c %q: %q is not an integer", s, part)`（把**整条**原始字符串与**出错的那一段**都带进消息）；`n < 1` → `fmt.Errorf("-c %q: concurrency must be >= 1", s)`；最后若一个都没解析出来 → `fmt.Errorf("-c: no concurrency levels given")`；成功则 `sort.Ints(out)` 后返回。

两处细节：**输出是排序过的**，所以 `-c 128,8,32` 与 `-c 8,32,128` 产生同一份产物顺序（`runPhase` 的打印与 `report.Phases` 都按这个顺序走）；**每个级别都必须是整数**，所以 `-c 8.5` 会在启动时就 `fatalf` 掉，而不是被截断。它只在 `main` 里被调用一次，但 `-diag` 只用了它的第一个元素——所以 `-diag -c 8,32` 会**静默忽略** 32。

### `fatalf`

```go
func fatalf(format string, args ...any)
```

```go
fmt.Fprintf(os.Stderr, "loadtest: "+format+"\n", args...)
os.Exit(1)
```

行为：把 `format` 前面拼上 `"loadtest: "`、末尾补换行，写 stderr，然后以状态码 1 退出。前缀是这个命令的错误命名空间，与 `cmd/measure-m2/main.go` 的 `fatal`（前缀 `"measure-m2: "`）成对。调用点全部在 `main` 里：`parseLevels` 失败、`runDiagnosis` 失败、三条 `nothing to do` / 互斥守卫、`runBaseline` 失败、`runMatrix` 失败、`writeJSON` 失败。注意它**不返回**——所以 `main` 里那些 `fatalf(...)` 后面的 `return` 是为了让控制流显式，而不是为了逻辑正确。

### `hostname`

```go
func hostname() string
```

`h, err := os.Hostname()`；出错返回字面量 `"unknown"`，否则返回 `h`。只被 `runBaseline` 调用一次，用来填 `report.Host`。源码没有注释解释为什么退化成 `"unknown"` 而不是把错误传出去——理由是产物里少一个字段总好过整趟基线跑不出来。这一行的产物意义是：基线数字绑定在**哪台机器**上。

### `goVersion`

```go
func goVersion() string
```

一行 `return runtime.Version()`，只被 `runBaseline` 调用一次，填 `report.Go`。它与 `Host` 一起构成基线产物的两条复现前提：同一个二进制在不同 Go 版本下的调度与 GC 行为是不同的。注意它返回的是**构建这个二进制时**的 Go 版本，而不是运行时的 toolchain——因为它是 `runtime` 包从二进制里读出来的。

## 14. 目标集合：`urlList`、`normalizeTarget`、`targetSet`

### `type urlList []string` 与它的三个方法

```go
type urlList []string

func (l *urlList) String() string { return strings.Join(*l, ",") }

func (l *urlList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if s := strings.TrimSpace(part); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}
```

**类型文档注释观点**（照译）：它收集 `-urls` flag。它是一个 `flag.Value`，所以这个 flag 可以被给多次，而且每一次出现本身可以是一个逗号分隔的列表：`-urls a,b` 与 `-urls a -urls b` 是同一个请求。重复项会被丢弃，并保留每个目标**首次出现的位置**——因为「请求索引 → 目标」的指派必须是调用方所写列表的一个**纯函数**。否则一个不小心被写了两遍的 URL 会拿到双倍负载，而整趟运行会由于一个与网关无关的原因看起来不平衡。

- `String()`：`flag` 在打印默认值/用法时会调它，实现是逗号连接。注意它**不去重、也不归一**——它照原样回放被 append 进来的原始段。
- `Set`：按 `,` 切、`TrimSpace`、非空才 append，**永远返回 nil**。所以 `-urls` 不会报错；连 `-urls ""`、`-urls ",,,"` 都只是什么都不加。
- `list()`：

```go
func (l urlList) list() []string
```

```go
var out []string
seen := make(map[string]bool, len(l))
for _, raw := range l {
	key := normalizeTarget(raw)
	if key == "" || seen[key] {
		continue
	}
	seen[key] = true
	out = append(out, key)
}
return out
```

注意 receiver 是**值**（`l urlList`，不是 `*urlList`）。去重的键是**归一化之后**的字符串，而放进 `out` 的也是归一化后的值——所以 `http://a:1/` 与 ` http://a:1 ` 会在产物里统一成 `http://a:1`。注释原话：按首次出现顺序返回去重后的目标。

### `normalizeTarget`

```go
func normalizeTarget(raw string) string
```

一行：`strings.TrimRight(strings.TrimSpace(raw), "/")`。源码文档注释观点：它让同一个 base URL 的两种拼法比较相等；**尾斜杠必须去掉**，因为这个生成器是把路径以 `base+"/v1/chat/completions"` 的方式拼上去的。也就是说这一行的存在理由是 `doRequest` 的字符串拼接，而不是审美。注意它只去**尾部**的斜杠（且是全部尾斜杠），不做 scheme 归一、不去端口默认值、不去尾部空白以外的任何东西。

### `type targetSet`

```go
type targetSet struct {
	targets []string

	mu   sync.Mutex
	next int
}
```

**类型文档注释观点**（本册照译，因为它是这套测量口径的核心）：

- 它把请求索引摊到一个固定、有序的目标列表上。
- 选择是按 **REQUEST** 做的，而计数器被所有 worker **共享**，所以无论 worker 怎样交错，各目标收到的计数最多相差 1。
- 按 worker 切分无法承受「一个目标比它的兄弟慢」：钉在它上面的 worker 会落后，整趟量的就成了客户端的调度，而不是 fleet 的容量。
- 同理：这里没有任何随机，也不依赖某个请求花了多久——请求 `i` 永远去 `targets[i%n]`——所以同一条命令跑两次，会把相同的请求交给相同的实例。
- 那把显式的锁是**刻意的**：每个 worker 已经有自己的一份未同步计数器（它们在请求结束时于阶段互斥锁下合并），但目标计数器必须共享，否则轮转又变回 per-worker 的了。

### `newTargetSet`

```go
func newTargetSet(targets []string) *targetSet {
	return &targetSet{targets: targets}
}
```

它不拷贝、不去重、不归一——`targets` 是按引用存下来的。归一与去重发生在上游的 `urlList.list()`；直接调用 `newTargetSet` 的人（例如测试）拿到什么就是什么。`mu` 与 `next` 零值即正确。

### `pick`

```go
func (s *targetSet) pick() (int, string)
```

```go
s.mu.Lock()
i := s.next % len(s.targets)
s.next++
s.mu.Unlock()
return i, s.targets[i]
```

源码文档注释观点：返回下一个请求该用的目标**索引**与它的 **base URL**；计数器在请求被发出**之前**推进，所以一个失败的目标恰好花掉一个请求，永远不能把抽到它的那个 worker 卡住。

三个边界：`len(s.targets) == 0` 时 `s.next % 0` 会 panic（上游有守卫：`main` 在目标为空时已经 `fatalf`，`runPhase` 拿到的 `targets` 也来自那里）；`s.next` 是 `int`，所以到 `math.MaxInt` 之后 `next++` 会溢出成负数，进而在下一次取模时得到负索引并 panic —— 在 `requests` 以千计的测量里到不了，源码没有处理；读 `s.targets[i]` 在**锁外**做，但因为 `targets` 在构造后不再被改，是安全的。

### `label`

```go
func (s *targetSet) label() string { return strings.Join(s.targets, ",") }
```

源码文档注释观点：它是写进记录的人类可读目标——对一个目标来说，它就是那条 URL，拼法与旧产物完全一致；对多个目标来说，它是逗号连接的列表，好让多目标记录的读者能在**一直用来命名目标的那个字段**里看见整个 fleet。它只被 `runPhase` 用来填 `result.Target`。也正因为是逗号连接，`shortTarget` 必须按逗号拆分才能正确缩短（第 12 节）。

## 15. 统计基元：`pick`、`mean`、`truncate`

### `pick`

```go
func pick(sorted []time.Duration, p float64) time.Duration
```

```go
if len(sorted) == 0 {
	return 0
}
idx := int(math.Ceil(p*float64(len(sorted)))) - 1
if idx < 0 {
	idx = 0
}
if idx >= len(sorted) {
	idx = len(sorted) - 1
}
return sorted[idx]
```

**源码文档注释观点**：返回一个**已排序**切片的 nearest-rank 百分位；用 nearest-rank（而不是插值）是因为它永远报一个**真正被观察到**的延迟——一个插值出来的 P99 可能引用一个没有任何请求见过的数。

逐点：

- **前提是调用方已经排好序**。函数签名里的参数名就叫 `sorted`，它自己不排序。`runPhase` 在调用前做了两次 `sort.Slice`。
- 公式 `ceil(p*n)-1` 就是 nearest-rank 的定义。对 `p=0.50, n=800` 得 `idx = 400-1 = 399`；对 `p=0.99, n=800` 得 `idx = 792-1 = 791`。
- 两个边界夹取：`idx < 0` 只在极小 `p` 或空切片时出现（空切片已被第 1 行拦掉），`idx >= len` 只在 `p > 1` 时出现。所以即使传入 `p=1.0` 也不会越界（`ceil(1.0*n)-1 = n-1`）。
- 它被调用了 6 次，全部在 `runPhase` 里：`0.50`/`0.95`/`0.99` 各一次给 `lat`，`0.50`/`0.95` 两次给 `ttft`。`TTFTMean` 走的是 `mean` 而不是 `pick`。

### `mean`

```go
func mean(samples []time.Duration) time.Duration
```

```go
if len(samples) == 0 {
	return 0
}
var total time.Duration
for _, s := range samples {
	total += s
}
return total / time.Duration(len(samples))
```

空切片返回 0；不过在实践中它拿不到空切片——`runPhase` 已经在 `len(lat) == 0` 时早退（见第 7 节），所以这个分支是给直接调用者的防御。**整数除法**：`time.Duration` 相加可能溢出（800 个 30ms 的请求合计 24 秒，远在 `int64` 纳秒容量内，安全）。被 `runPhase` 调用两次：`Mean = mean(lat)`、`TTFTMean = mean(ttft)`。

### `truncate`

```go
func truncate(s string, max int) string
```

```go
if len(s) <= max {
	return s
}
return s[:max] + "..."
```

**按字节**截断（不是按 rune），并且补三个点。它只被 `doRequest` 调用一次，参数是 `truncate(string(frame.Data), 80)` —— 用来把一条畸形的 SSE 帧塞进错误消息里。与 `cmd/measure-m2/main.go` 里的同名函数不同（那边按 rune 截断并补 `…`，第 17 节），两者是各自独立的实现。

## 16. `cmd/loadtest` 的测试

`cmd/loadtest/main_test.go` 里有 7 个测试。它们全部落在**不需要真网关、不需要真 mock 可执行文件**的层面——除了三个用 `httptest` 起真后端的。逐个说。

**`TestURLListParsesCommaListAndRepeats`** —— 把 `urlList` 放在**真 `flag.Value` 路径**上跑。六个 case：逗号分隔、重复 flag、混合且保序、重复去重保首位（`http://a:1`、`http://b:2`、`http://a:1,http://b:2`、`http://a:1` → `[a, b]`）、空格与尾斜杠归一、空项忽略。注释里的理由：`-urls a,b` 与 `-urls a -urls b` 只有在这套 flag 机制是**追加**而不是替换时才同义。

**`TestTargetSetIsRoundRobinStableAndBalanced`** —— 三段断言：前 11 次 `pick` 的索引等于 `i%3` 且 base 正确；两个独立的 `newTargetSet` 各 `pick` 23 次得到相同索引序列（注释：请求索引到目标的指派只是目标列表的性质，两次运行画出同一序列）；`n` 从 0 到 40，每个 `n` 各自新建一个 set 并 pick `n` 次，要求各目标计数极差 `≤ 1`。第三段注释点明它覆盖的是「不是目标数整数倍」的请求数——off-by-one 的派发会在那里露出两个请求的差。

**`TestTargetSetConcurrentPicksStayBalanced`** —— 4 个目标、8 个 worker 各 `pick` 125 次，要求每个目标恰好 250 次。注释：计数器被每个 worker 共享，所以在 `-race` 下这个测试也是「派发不是 per-worker」的证明——per-worker 的版本在某目标变慢时就会漂。注意 `want := workers * each / len(targets)` 用的是**整除**，125×8=1000、4 目标正好 250。

**`TestRunPhaseSpreadsAcrossRealTargets`** —— 起三个真 `httptest` 后端（各自返回一个非流式 JSON），`requests=37`、`warmup=3`、`concurrency=4`，然后逐条核对：`Errors == 0`；`Requests == 37`；`r.Target == strings.Join(targets, ",")`；`len(r.Targets) == 3`；`targets[i].URL` 的顺序**跟随 `-urls` 列表**；per-target 请求数之和等于 37；per-target 错误数之和等于 `r.Errors`；per-target QPS 之和与 `r.QPS` 的差在 `1e-6` 以内；任意两目标的请求数之差不超过 1；以及三个后端一共看到 `requests + warmup` 次请求。最后一条是唯一直接验证「热身确实被打了、且不计入产物」的断言。测试注释点出它存在的理由：一个正确的选择器如果没人接上线，仍然是一个坏掉的功能。

**`TestRunPhaseFailingTargetDoesNotWedgeOthers`** —— 三个后端里中间那个返回 500，`requests=30`、`warmup=0`、`concurrency=4`。期望值是这套口径的直接推论：`Requests/Errors == 20/10`；**每个**目标（包括失败的那个）都被派了 10 次；失败目标报 10 个错误、两个健康目标报 0 个错误；后端一共看到 30 次请求。括号里的注释说明 `warmup` 之所以是 0：热身失败仍然致命，而那一点记在 `runPhase` 的文档注释里（第 7 节）。

**`TestMockLogTailDegradesWithoutLogFile`** —— 两个分支：`&mockProc{}`（`logPath` 为零值）的 `logTail()` 必须等于字面量 `"log unavailable, temp dir not writable"`；`&mockProc{logPath: "loadtest-no-such-log-file.log"}` 的结果必须以 `"log unavailable: "` 开头，且**不得 panic**。这是全册唯一不碰网络的测试。

**`TestMedianResultMediansPerTarget`** —— 三轮 `result`，每轮两个 `targetStat`（`a` 的 QPS 依次 10/20/30、请求数 10/12/11；`b` 的 QPS 依次 2/4/6）。断言 `len(got.Targets) == 2`、`got.Targets[0] == targetStat{URL: "a", Requests: 11, QPS: 20}`、`got.Targets[1] == targetStat{URL: "b", Requests: 11, QPS: 4}`、`got.Rounds == 3`。它钉住的是「取中位而不是取第一轮或最后一轮」：`a` 的 QPS 中位是 20（不是 10 也不是 30），请求数中位是 11（不是 10 也不是 12）；`b` 的 QPS 中位是 4（`[2,4,6]` 的中间）。

**这 7 个测试没有覆盖的东西**，本册点出来：`runBaseline`、`runDiagnosis`、`startMock`、`mockBinary`、`scrapeMetrics`、`scrapeFleet`、`printTable`、`printOverhead`、`ms`、`pct`、`shortURL`、`shortTarget`、`parseLevels`、`newClient`、`medianResult` 的 QPS/百分位路径（只有 per-target 部分被测）、`doRequest` 的流式分帧、`countingReader`、`writeJSON`。也就是说：**被测试覆盖的是目标轮转与轮次归约的口径**，其余部分是靠 `-all`/`-diag` 跑起来时肉眼核对的。

与验收家族的分工：`cmd/verify` 与 `cmd/verify-m1`…`cmd/verify-m6` 那 267 个夹具/断言辅助符号不在这里，逐符号展开见第 11 册（`docs/WALKTHROUGH/11-verify-harness-fixtures.md`）。

## 17. `cmd/measure-m2` 逐符号

`cmd/measure-m2/main.go` 共 5 个顶层符号，全部是 `func`，没有 `type`、`var`、`const`：`main`、`printCurve`、`truncate`、`writeJSON`、`fatal`。逐个说。

### 17.1 包注释与它承诺的两件事

包注释（源码注释观点，照译要点）：这个命令测量语义缓存的**阈值**，以及稍后它在**负载**下的命中率。两件事、两个理由：

1. **THRESHOLD SWEEP**。它在 `internal/evalset` 的带标注语料上跑一个 embedder，并对每个候选阈值报告两个数字：「多少 paraphrase 对会命中」与「多少 near-miss 对会被一个**错误的**缓存响应回答」。**第二个数字决定默认阈值**——阈值不是一个调参旋钮，而是正确性与收益之间的取舍，需要证据。
2. **带 `-url` 的「LOAD-CLIENT 阶段」**。它把 paraphrase 工作负载重放到一个活网关并报告真实调用者看到的命中率，**包括缓存还冷的时候到达的流量**。

注释里还有一句关于**这道闸为什么能存在**的话：sweep 不需要网关、不需要 Redis、不需要网络——它是语料上的纯算术，所以它能成为「默认阈值不再有正当理由时就会失败一个构建」的那道闸。

### 17.2 `func main()`

**参数面**（全部是 `flag`）：

| flag | 默认值 | flag 说明原文 |
| --- | --- | --- |
| `-dims` | `embed.DefaultHashingDims` | `dims for the offline hashing embedder` |
| `-http` | `""` | `use an HTTP embedding endpoint instead of the offline embedder` |
| `-http-model` | `"text-embedding-3-small"` | （未给说明；实测模型名） |
| `-http-key` | `""` | （未给说明；`Authorization` 用的 key） |
| `-http-timeout` | `5*time.Second` | （未给说明） |
| `-thresholds` | `""` | `comma-separated thresholds (default: the evalset grid)` |
| `-json` | `false` | （未给说明；把报告以 JSON 写到 stdout） |
| `-out` | `""` | （未给说明；把报告以 JSON 写到该文件） |
| `-pairs` | `false` | `print every pair's similarity, sorted` |
| `-max-false-hits` | `0` | `false hits allowed when recommending a threshold` |

**embedder 的选择**（一个 `switch`，两支）：

```go
ctx := context.Background()
var emb embed.Embedder
switch {
case *httpURL != "":
	emb, err = embed.NewHTTPEmbedder(embed.HTTPOptions{
		BaseURL: *httpURL, Model: *httpModel, APIKey: *httpKey, Timeout: *httpTO,
	})
default:
	emb = embed.NewHashingEmbedder(*dims)
}
if err != nil {
	fatal(err)
}
```

所以 `-http` 一给，`-dims` 就被忽略（HTTP embedder 的维度由对端决定），而 `-http-model`/`-http-key`/`-http-timeout` 也只在那条分支上被读。默认分支是**离线**的 `embed.NewHashingEmbedder`，这正是「sweep 不需要网络」那句话在代码里的样子。

**阈值的来源**（两条路，后者覆盖前者）：

```go
grid := evalset.DefaultThresholds()
if *thresholds != "" {
	grid = nil
	for _, s := range strings.Split(*thresholds, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		var f float64
		if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
			fatal(fmt.Errorf("bad threshold %q: %w", s, err))
		}
		grid = append(grid, f)
	}
}
```

三处细节：`-thresholds` 一给就**整体替换** `evalset.DefaultThresholds()` 而不是并集；解析用 `fmt.Sscanf(s, "%g", &f)`，所以 `0.9x` 这种会被判错并 `fatal`，但 `0.9abc` **不会**——`%g` 只在第一个字符就无法解析时报错；空段被静默跳过，所以 `-thresholds ""` 实际等价于「给出一个空网格」而不是「回到默认网格」，而空网格会让 `printCurve` 直接索引 `rep.Thresholds[0]` 时 panic（见 17.3）。

**扫描与报告**：

```go
rep, cases, err := evalset.Sweep(ctx, emb, grid, nil)
if err != nil {
	fatal(err)
}
rep.Embedder = fmt.Sprintf("%s(dims=%d)", emb.Name(), emb.Dims())
```

`cases` 被接收但只在 `-pairs` 分支里用到；`evalset.Sweep` 的第四个实参是 `nil`。`rep.Embedder` 被**覆写**成一个把维度也带上的串，理由是产物的读者要能分辨同一个 embedder 的两种维度（源码没有为这行写注释，这是本册的推断，标注为**推断**）。

`-pairs` 分支：先打印 `embedder %s, dims %d, %d labelled pairs`，再打印表头 `%-6s %-11s %-8s %s`（列名 `sim`、`verdict`、`want`、`pair`），然后逐 case：

```go
want := "miss"
if c.ShouldHit() {
	want = "HIT"
}
fmt.Printf("%-6.4f %-11s %-8s %s | %s\n",
	c.Similarity, c.Kind, want, truncate(c.A, 40), truncate(c.B, 40))
```

注意 `want` 是**语料的期望**（`c.ShouldHit()`），与 `sim`/阈值无关——所以这张表能直接看出「期望命中但相似度很低」的对。`c.Kind` 用的是 `evalset` 的 kind 常量（`evalset.KindIdentical`、`evalset.KindParaphrase`、`evalset.KindNearMiss`、`evalset.KindUnrelated` 之一）。

然后是无条件调用的 `printCurve(rep)`，再是推荐阈值：

```go
if rec, ok := evalset.Recommend(rep, *maxFalse); ok {
	fmt.Printf("recommended threshold %.2f: hit rate %.0f%% (%d/%d), false-hit rate %.0f%% (%d/%d)\n",
		rec.Threshold, rec.HitRate()*100, rec.TrueHits, rec.TrueTotal,
		rec.FalseHitRate()*100, rec.FalseHits, rec.FalseTotal)
	if len(rec.FalseHitIDs) > 0 {
		fmt.Printf("  false hits: %s\n", strings.Join(rec.FalseHitIDs, ", "))
	}
} else {
	fmt.Printf("NO threshold in the swept grid keeps false hits within %d: this embedder cannot separate the corpus\n", *maxFalse)
}
```

`ok == false` 的那条消息是本册里最值得引用的一句：它说的不是「没找到更好的阈值」，而是**这个 embedder 分不开这份语料**——把结论从「调参失败」抬到「换一个 embedder」。

**输出的落点**（三条路，互斥优先级从上到下）：

```go
if *out != "" {
	if err := writeJSON(*out, rep); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s\n", *out)
} else if *asJSON {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fatal(err)
	}
}
```

所以 `-out` 压过 `-json`；两者都不给就只有人读的表。`-out` 走 `writeJSON`（整体 `MarshalIndent` 后一次写入），`-json` 走 `Encoder`（流式、缩进同样两格）——两条路的**字节**因此在末尾换行上一致，但错误路径不同（前者错误来自 `os.WriteFile`，后者来自 `Encode`）。

### 17.3 `func printCurve(rep evalset.Report)`

```go
func printCurve(rep evalset.Report)
```

它打的是阈值曲线，一张表：表头 `%-9s %-14s %-14s %s`，四列分别 `thresh`、`hit rate`、`false-hit rate`、`false hits`。数据行：

```go
for _, t := range rep.Thresholds {
	fmt.Printf("%-9.2f %-14s %-14s %s\n", t.Threshold,
		fmt.Sprintf("%.0f%% (%d/%d)", t.HitRate()*100, t.TrueHits, t.TrueTotal),
		fmt.Sprintf("%.0f%% (%d/%d)", t.FalseHitRate()*100, t.FalseHits, t.FalseTotal),
		strings.Join(t.FalseHitIDs, ","))
}
```

最后一行小结：

```go
fmt.Printf("%d pairs: %d should hit, %d should not\n",
	rep.Cases, rep.Thresholds[0].TrueTotal, rep.Thresholds[0].FalseTotal)
```

三处要注意：**它直接索引 `rep.Thresholds[0]`**，所以阈值网格为空时这一行会 panic（`evalset.Report.Thresholds` 是 `[]evalset.ThresholdResult`）；`rep.Cases` 是总对数，而分母取自**第一个**阈值档——这在所有阈值共享同一份语料的前提下才等价；false-hit 列在四个数字里排在最后，说明作者认为读者先看命中率再看它的代价，而 `-max-false-hits` 才是那个有否决权的数。它被 `main` 无条件调用一次。

### 17.4 `func truncate(s string, n int) string`

```go
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
```

**按 rune 而不是按字节**截断，且省略号用的是 `U+2026`（`…`）而不是三个点。这与 `cmd/loadtest/main.go` 里的同名函数（按字节、补 `...`）是两个独立实现——本册第 15 节记了那一份。rune 版本在这里是必须的：语料是人工裁决的自然语言对，可能含非 ASCII，按字节切会把一个多字节字符切成两半、产出无效 UTF-8 并让终端显示乱码。它只被 `-pairs` 分支调用两次，`n` 都是 `40`。

### 17.5 `func writeJSON(path string, v any) error`

```go
raw, err := json.MarshalIndent(v, "", "  ")
if err != nil {
	return err
}
return os.WriteFile(path, append(raw, '\n'), 0o644)
```

与 `cmd/loadtest/main.go` 里的同名函数**逐字同形**（两空格缩进、补尾换行、`0o644`），是两份独立副本而不是共享代码——本册把它记为事实，源码没有解释为什么。它同样**不创建父目录**，所以 `-out tmp\m2-sweep.json` 要求 `tmp` 已经存在（`scripts/measure-m2.ps1` 在使用前自己建它，见第 18 节）。唯一的调用者是 `main`，错误经 `fatal` 变成退出码 1。

### 17.6 `func fatal(err error)`

```go
func fatal(err error) {
	fmt.Fprintf(os.Stderr, "measure-m2: %v\n", err)
	os.Exit(1)
}
```

与 `cmd/loadtest/main.go` 的 `fatalf` 成对：这里是 `error` 参数 + 固定前缀 `measure-m2: `，那边是 `format`/`args` + 前缀 `loadtest: `。它不返回，所以调用点后面不需要 `return`。调用者共 5 处：HTTP embedder 构造失败、阈值解析失败、`evalset.Sweep` 失败、`writeJSON` 失败、`-json` 编码失败。

### 17.7 诚实漂移：包注释承诺的 `-url` 阶段不存在

这是本册必须写清的一处**注释与实现不一致**，第 07B 册也已点过。

- 包注释把「带 `-url` 的 LOAD-CLIENT 阶段」写成这个命令的第 2 件事，并说它「把 paraphrase 工作负载重放到一个活网关、报告真实调用者看到的命中率，包括缓存还冷时到达的流量」。
- 但代码里**没有任何对应实现**：`flag` 集合只有 17.2 表里的十个，其中没有 `-url`（也没有别的 URL/端口类 flag）；`main` 里没有任何 HTTP 客户端的构造，没有 `http.Get`/`http.Post`/`http.NewRequest`，也没有对 `/v1/chat/completions` 或 `/admin/cache` 的调用。整个命令的唯一输入是本地语料与本地 embedder，唯一输出是 stdout 或一个 JSON 文件。
- 于是「缓存冷时到达的流量」这件事**由脚本层承担**：`scripts/measure-m2.ps1` 自己起网关、自己用 `cmd/loadtest` 打负载（第 18 节）。两个程序因此各量一半：`cmd/measure-m2` 量阈值的正确性，脚本量它在负载下的收益。
- 作者的处理方式是**把它记在注释里，而不是删掉注释**。本册照录这个事实，并标为**未能确认**的一点：无法从代码判断这个阶段是被移走了、还是从来只是计划（见第 22 节）。

## 18. `scripts/measure-m2.ps1`

`scripts/measure-m2.ps1` 是 M2（语义缓存）的测量驱动程序，约 1233 行。头部注释把它的四项检查连同各自的诚实框架一起写在最前面——本节按那四项走。

### 18.1 它量什么

**A. 命中率与省下的 token**。语料是仓库里人工裁决的 26 对，落在 `internal/evalset/evalset.go`。对每个 should-hit 对跑三次：A（未命中）、A 再来一次（应当命中）、B（paraphrase，应当命中）；near-miss 与不相关对**永不允许**命中。源码注释观点（脚本头部的原话）：一次错误的命中是一个一等的数字，理由是一个错答案比一次 miss 更糟。省下的 prompt/completion token 从 `GET /admin/cache` 读回，USD 用**同一份配置里的价目**折算。**这四项检查里唯一不是派生的是命中率与 token 计数本身**——USD 是折算。

**B. 延迟与吞吐**。cache ON 与 OFF 两个网关，用 `cmd/loadtest` 三轮**交错**打，并发取 c=8 与 c=32。诚实框架（脚本头部原话的要点）：上游是仓库内的 mock、本地且免费，所以吞吐差主要是**省掉的网关开销**，而不是 provider 延迟。缓存真正买到的是两样东西：(a) 命中路径的 P95，(b) **一次命中完全不发生上游调用**——(b) 用 mock 自己的请求日志数出来，是**实测事实**而不是从网关计数器推断。

**C. 缓存当盾牌**。先预热并标注提示，然后**杀掉上游进程**，再重放 exact 与 paraphrase 两类请求。「provider 硬宕机期间服务了多少请求」才是这个特性的正当理由。脚本里还有一个**未预热的对照提示**，它必须失败——用来证明那次宕机是真的，而不是 mock 还在应答。

**D. 阈值扫描**。它调的是本册第 17 节那个命令：`.\tools\go.cmd run ./cmd/measure-m2 -json -out tmp\m2-sweep.json`。注意路径写的是 `tools/go.cmd`，与本册其它地方引用的同一份构建入口是同一个文件。

### 18.2 它怎么从网关取数

三个不同的表面，按用途分开：

- **`GET /admin/cache`** —— 拿缓存自己的命中/未命中与省下的 prompt/completion token（A 项）。
- **mock 的请求日志** —— 数「有没有真的发生上游调用」（B 的 (b) 与 C）。这是整个脚本里最硬的一个证据来源，因为它**不经过网关**：网关的计数器再怎么说也只是网关自己的说法。
- **客户端墙钟**（`cmd/loadtest` 的产物）—— 拿 P95 与 QPS（B 的 (a)）。

### 18.3 产物形状与落点

| 产物 | 落在哪 | 形状 |
| --- | --- | --- |
| M2 汇总 | `docs/baseline/m2-summary.json` | 四个检查各自的结果块，加上说明每个数字怎么来的字段 |
| 语料快照 | `docs/baseline/m2-corpus.json` | 那 26 对人工裁决语料的形状（pairs 与它们的期望） |
| 阈值曲线 | `docs/baseline/m2-sweep.json` | `cmd/measure-m2 -json` 那份报告（先落 `tmp\m2-sweep.json`，再 `Copy-Item` 进基线） |
| 逐轮负载档 | `docs/baseline/m2-load-on-r1.json`、`docs/baseline/m2-load-off-r1.json` … | 每轮每臂一份 `cmd/loadtest` 产物 |

负载那一行的命令行原文是：

```text
bin\loadtest.exe -url http://127.0.0.1:PORT -c $Concurrency -n $Requests -warmup $Warmup -out docs\baseline\m2-load-<on|off>-r<round>.json
```

注意 `-url` 是**单目标**形式（不是 `-urls`），而且它正是本册第 3 节那张表里的两个 flag 之一。**端口**用的是脚本自己的参数而不是默认值：`CacheOnPort` 18290、`CacheOffPort` 18291、`MockPort` 19510、`MiniRedisPort` 19389。

**配置怎么来的**：`configs/` 下的文件**一个字节都不改**——脚本把配置**实例化**（字面替换）进 `tmp/` 再喂给网关。这与第 20 节里 `scripts/measure-m4.ps1` 用 `scripts/lib/pricing.ps1` 从**实例化后**的配置解析价格是同一套做法的两个面。

### 18.4 它不能证明什么

- **吞吐差不是「缓存省了 provider 时间」的证据**。上游是本机 mock、零成本，所以省下来的只能是网关这一段；脚本头部自己就这么写。要在真 provider 上量这件事，得换掉上游、并重新声明那次的噪声底。
- **USD 数是折算而不是实测**。它来自配置里的价目，所以改价目就会改这个数，而缓存的行为一点没变。
- **命中率不是端到端的**。它是在标注语料上按阈值判定的；真实流量的分布不在语料里，所以这个数说明的是「阈值在这份语料上的行为」，不是「线上会命中多少」。
- **杀上游那一项只证伪了「宕机不是真的」**，它不证明缓存的正确性——正确性归 A 项的 false-hit 数字。
- **一轮 3 次只够压掉本机噪声**，不足以支撑分布性的结论（这正是第 2 节那条口径的边界）。

## 19. `scripts/measure-m3.ps1`

`scripts/measure-m3.ps1` 是 M3（准入与配额）的测量驱动程序，约 1469 行。头部有一段**写入纪律**必须先说：这份脚本**故意只用 ASCII**，因为 Windows PowerShell 5.1 会按 ANSI 读取没有 BOM 的脚本，一个非 ASCII 字符会在脚本跑起来之前就把它弄坏。本册引用它的注释时也要注意这一点——原文里没有中文。

### 19.1 它量什么：五项

1. **准入开销**。同一个负载分别打 `quota.enabled: false` 与 `quota.enabled: true`（memory store）两个网关，3 轮交错，非流式、c=8 与 c=32。它量的是「开配额本身要付多少」。
2. **预留准确性**。用已知配比的请求，把网关的 `reserved`/`settled`/`released`/`overshoot` 计数与 mock **自己报出来的**用量对比。数据源是 `GET /admin/quota`。
3. **预算刹住失控租户**。`tokens_per_day` 设成 280，看超预算后是否真的被拒；上游调用数同样从 **mock 自己的日志**数出来。
4. **Redis 与内存**。同一个负载分别打 memory 与 `store: redis` 的网关，Redis 是 `cmd/miniredis`，监听 `:6397`。日桶键用**裸 RESP2**（`System.Net.Sockets.TcpClient`）直接读回，**不经过网关**。
5. **fail-closed**。停掉 miniredis 之后：每个请求都必须被拒，且**不得发生任何上游调用**；把 miniredis 重启之后，网关必须**不重启**就恢复服务。同时看 `/metrics` 的 `store_errors_total`。

### 19.2 数据来源与它的口径声明

脚本头部有一句话把整份脚本的取材原则讲透了（原话要点）：**每个数字都从一个网关自己写不出来的表面读，只要那可能。** 于是三个来源分工：`GET /admin/quota` 读配额计数（这是网关自己的说法，所以它必须与来源 2 对账）、mock 的请求日志数真实上游调用（网关写不出来）、裸 RESP2 读 Redis 的日桶键（同样绕过网关）。

`docs/baseline/m3-summary.json` 里每个检查都带自己的 **caliber** 说明，其中两条值得在本册里保留：

- `caliber_accuracy`：mock 的 `prompt_tokens` 是**每条消息按空白分词再加 4**、`completion_tokens` 是**答案按空白分词**。这条口径让「真实用量」可以独立于网关被算出来——没有它，第 2 项的对比就只是两个网关计数器的对比。
- `caliber_redis`：日桶计数是**裸 RESP2** 读的，明确区别于「读网关自己报的数」。

### 19.3 参数与产物

| 参数 | 值 |
| --- | --- |
| `GovernedPort` / `UngovernedPort` / `RedisGovernedPort` | 18400 / 18401 / 18402 |
| `MockPort` | 19700 / 19701 / 19702 |
| `MiniRedisPort` | 6397 |
| `Rounds` / `Requests` / `Warmup` | 3 / 800 / 100 |
| `Concurrency` | `'8,32'` |
| `BudgetRequests` / `FailClosedRequests` / `RecoveryRequests` / `FailOpenRequests` | 40 / 20 / 10 / 5 |
| `KeepRunning` | 跑完是否留下进程 |

产物：`docs/baseline/m3-summary.json` 是唯一进基线的文件；逐轮负载档落 `tmp\m3-load-*.json`，**故意不进基线**（它们是中间态，而基线里的数是它们的归约）。网关配置同样是实例化进 `tmp\m3-*.yaml`，`configs/` 不变。

### 19.4 它不能证明什么

- **memory 与 Redis 不是一个公平的吞吐对比**。`cmd/miniredis` 是一个仓库内的替身，不是生产 Redis；脚本只回答「换成外部 store 之后口径是否仍然成立」。
- **fail-closed 那项证明的是策略，不是可用性**。它证明「拒绝 + 零上游调用」，不证明故障期间的延迟或用户体验。
- **预算数字（280）是刻意设小以触发拒绝**，所以第 3 项不构成任何容量结论。
- **准入开销只覆盖非流式 c=8 与 c=32**；流式路径的开销不在其中。

## 20. `scripts/measure-m4.ps1`

`scripts/measure-m4.ps1` 是 M4（量化与分层）的测量驱动程序，约 1808 行。它有三项，前两项要真跑东西，第三项是纯派生。

### 20.1 它量什么

**quantization**。在 WSL 里用**真 vLLM** 依次以 fp16、AWQ、GPTQ-Int4 服务 `Qwen2.5-1.5B-Instruct`，量 TTFT、总延迟、吞吐与显存（load 与 unload 两个方向的占用）。真正的驱动是 `scripts/bench-vllm-quant.sh` 与 `scripts/bench_vllm_quant.py`，PowerShell 这一层负责起停、编排与汇总；中间产物是 `tmp/m4-quant-<variant>.json` 与 `tmp/vllm-<variant>.log`。

**tiering**。同一个真 vLLM 当网关的 local tier，`cmd/mockupstream` 扮 cloud tier，用已知的简单/困难配比穿网关。**取数**用 `GET /stats` 的 per-upstream `series[]` 请求计数与 `tokens{prompt,completion}`，与实际发出去的东西对比。这里有一条必须写明的限制：`/stats` **没有** per-upstream 的延迟百分位，所以 per-tier 的 p50/p95 只能来自**客户端墙钟**，再按响应头 `X-InferGate-Upstream-Name` 归属到层——也就是「延迟是客户端量的，归属是网关给的」这两件事拼起来。

**cost**（派生）。仓库里没有 cost metric；定价按**模型名**，所以本地服务的请求也按该模型的价目计入，而那笔价就是「本地层替代掉的云支出」。脚本的每个 limitations 段都把这个约定重述一遍。

### 20.2 价格从哪来（与自检的关系）

价格用 `. (Join-Path $PSScriptRoot 'lib\pricing.ps1')` 引入 `scripts/lib/pricing.ps1`，再调 `Get-PricingFromConfig $instantiated`——**从实例化之后**的配置解析，而不是从 `configs/` 原文解析。这一点与自检直接相关：`internal/repofmt/m4prices_test.go` 要成立，前提就是价格在脚本里只有这一条解析路径。

### 20.3 参数与产物

| 参数 | 值 |
| --- | --- |
| `GatewayPort` / `CloudPort` / `VllmPort` | 18410 / 19620 / 8000 |
| `Variant` | `auto` / `fp16` / `awq` / `gptq` |
| `Requests` | 8 |
| `AwqWaitSeconds` | 0 |
| `SkipBench` / `SkipTiering` | 跳过对应项 |
| `KeepRunning` / `KeepWork` | 留下进程 / 留下中间文件 |

产物**只有一份**：`docs/baseline/m4-summary.json`（脚本里叫 `$artifact`）。逐轮/逐 variant 的中间产物留在 `tmp/`，不进基线。

关闭纪律：WSL 里的 vLLM 严格按**脚本自己启动的 pid** 关（走进程树），并且只在 `/v1/models` 停止应答之后才认为它关掉了——也就是说「进程没了」不是判定条件，「端口不再应答」才是。

### 20.4 它不能证明什么

脚本的 limitations 明写三条**不声称**，逐条照译要点：

1. **量化之间的一致性不是准确率**。它量的是相对 fp16 参考文本的**漂移**；没有任何东西检查答案对不对。所以「AWQ 的漂移更小」不等于「AWQ 更准」。
2. **cloud tier 是即时的仓库内替身**。`cmd/mockupstream` 不是任何真实云，所以跨层的延迟差**不是与任何真实云的比较**。
3. **显存数是差值**。`nvidia-smi` 报的是**总**显存占用，Windows 桌面在这台 8 GiB 的机器上先占约 1.8 GiB，模型占用是减出来的差值，噪声底约 50 MiB。所以「AWQ 比 fp16 少 200 MiB」这种级别的结论在噪声底之上才算数。

还有一条本册的观察：**cost 那一项的数不是实测的**——它完全由价目与请求计数推出，而请求计数来自 `/stats`。

## 21. `scripts/measure-m5.ps1`

`scripts/measure-m5.ps1` 是 M5（可观测性与横向扩容）的测量驱动程序，约 2222 行，也是这四份脚本里最大的一份。它的核心是**八个 arm**。

### 21.1 八个 arm 各是什么

| arm | 形状 | 它回答什么 |
| --- | --- | --- |
| `direct` | 负载发生器**直接**打 `cmd/mockupstream` | 天花板：见下 |
| `gateway` | 一个未开 tracing 的实例 | 基准网关臂 |
| `traced-otlp` | `sample_ratio 1.0` + OTLP/HTTP，无 JSONL sink | 只导出 OTLP 的代价 |
| `traced-full` | 在 `traced-otlp` 之上再叠 JSONL 文件 sink | 再加文件落地要多少钱 |
| `second` | 第二个未开 tracing 的实例 | 横向臂需要的单实例对照 |
| `horizontal-2` | **同一个客户端进程**分别打 A+B | 两实例 |
| `horizontal-4` | 同一个客户端进程打 A+B+C+D | 四实例，这是仓库文档化的 infergate-fleet 抓取 job 的形状 |
| `gateway-scraped` | A 再跑一次，同时 scraper 每 100ms 轮询 `/stats` 与 `/metrics` | 被抓取的代价的**上界** |

关于 `direct` 的定位，脚本 limitations 里有一句必须保留的话（要点）：它是**见证**而不是网关臂；如果 `direct` 与 `gateway` 在最高并发上落在约 15% 以内，那么 mock 就是天花板，**绝对 QPS 说明不了网关什么**。关于 `gateway-scraped`：100ms 大约是真实 Prometheus 5–15s 间隔的 10 倍，所以那是**上界**而不是 Prometheus 的模型。

### 21.2 测量下限与「SMOKE」纪律

默认参数：

```text
-Concurrency '8,32,128' -Requests 3000 -Warmup 300 -Rounds 3 -Timeout 30s
-MockTokenDelayMs 1 -MockTTFBMs 0 -LogLevel error -HealthTimeoutSeconds 30
-MinRequests 500 -MinRounds 2
```

以及一条**测量下限**：不达标的运行被判定为 SMOKE 运行，产物写 `tmp\m5-summary-smoke.json`，**故意不写 `docs/baseline/`**。源码注释观点：这样它永远不会被误当成一次测量。达标的运行才写 `docs/baseline/m5-summary.json`，内容含 `rows`、10 个 `checks`、`limitations`、bottleneck verdict、scraping verdict 与 provenance，并在写完**之后重新解析校验**一遍；逐臂的原始档被复制成 `docs/baseline/m5-load-<arm>-r<N>.json`，例如 `docs/baseline/m5-load-gateway-r1.json` 与 `docs/baseline/m5-load-horizontal-2-r1.json`，且脚本断言复制出来的份数等于 **arm 数 × `Rounds`**。

### 21.3 编排纪律

- **轮次在最外层**：每一轮里每个 arm 各跑一次。这与 `cmd/loadtest` 的 `-all` 是同一个理由（第 3 节）——把各臂放进同一热与堆状态。
- **所有臂共用同一个 mock**。注释理由：不同后端会把**后端之间**的差异记到网关头上。
- **网关统一跑 `log level error`**，因为日志本身在每臂数千请求下是可测成本。
- **五个网关实例的配置用 `StringBuilder` 现写**，产物首行写着 `# generated by scripts\measure-m5.ps1 -- do not edit; configs\ is never modified`。也就是说第 18 节那条「`configs/` 一个字节不改」的纪律，在这里被**写进了产物本身**。

### 21.4 它不能证明什么（以及它自曝的一处配置错误）

limitations 里有两条特别值得照录：

1. **一条自曝的 HARNESS CONFIGURATION ERROR**。`otlp.endpoint` 是 **base url**——`internal/traceexport/otlp.go` 里的 `OTLPOptions.Endpoint` 文档化过它是 base，`NewOTLP` 会自己把路径拼上去。这次的 harness 配的就是 base。于是结论是：**要修的地方在 harness 配置，不在网关**。这是本册里最干净的一个「量错了先说清楚量错了」的例子。
2. **scraper 是自己造成的干扰**。`gateway-scraped` 里的 scraper 是一个 PowerShell 循环调 `curl.exe`，进程 spawn 的开销把它封在一个上界速率上，所以「抓取成本差」是真实 Prometheus 的**上界**，而且其中一部分是 scraper 自己**在抢同一批 CPU 核**。

此外两项本册点出的边界：`horizontal-2`/`horizontal-4` 的对照臂只有 `second` 一个，所以「4 实例 vs 2 实例」的绝对斜率受限于单实例对照本身的可复现性；JSONL sink 的代价只在 `traced-full` 这一个点上量过，脚本没有做 sink 开/关的多轮交叉。

## 22. 未能确认

以下各条在读源码时找不到确证，本册照实列出，不替源码编理由。

1. **`cmd/measure-m2` 的 `-url` 阶段是被移走了、还是从来只是计划**。包注释写得像已实现，代码里没有对应 flag 与路径（第 17.7 节）。仓库里没有 changelog 或注释记录这次删除。
2. **`mockBinary` 的临时目录缓存行为是否有意**。源码注释把它写成「仓库还没构建时」的兜底，但第 1 步只检查 `bin/` 下那两个候选，所以建在 OS 临时目录里的那份产物下一次运行**不会被复用**，而是重新构建。没有注释说明这是刻意（例如避免陈旧二进制）还是疏漏。
3. **`streamLabel` 与 `runMatrix` 里内联的三行映射为什么没有统一**。两者产出同一对字符串，源码里没有解释。
4. **`hostname` 为什么把错误退化成字面量 `"unknown"` 而不是往外传**（第 13 节）。产物里少一个字段总好过整趟跑不出来是一个合理猜测，但没有注释支持。
5. **`measurements.frame` 的用途**。类型里同时有 `frames`（本请求的帧数）与 `bytes`，而 `runPhase` 把每请求的帧数存进 `frames[idx]` 数组却**从不读它**，只用 `framesN` 累加。这个数组是为将来的分析留的，还是残留，无法从代码判断。
6. **`-diag` 只取 `parseLevels` 的第一个元素**。所以 `-diag -c 8,32` 会静默只用 8；源码没有警告，也没有注释说明这是刻意的「诊断只在单一并发下做」。
7. **`scrapeMetrics` 的三个正则对 Prometheus 文本格式的假设**。它们要求标签集合内不含 `}`、且样本值紧跟空格；如果网关改标签顺序或加 metric 元数据行，这三个正则仍能匹配，但如果值采用科学计数法之外的写法就未必。没有测试覆盖它们（第 16 节末列的未覆盖清单里有 `scrapeMetrics`）。
8. **`printOverhead` 里 `50%` 与 `runDiagnosis` 里 `5%` 两个阈值的来源**。源码各自给了读法（「客户端是较慢的一半」、「direct 基线自己波动超过这个数就当噪声」），但没有引用任何测量来支持这两个具体数字。
9. **`urlList.Set` 永远返回 `nil`**。所以 `-urls` 不可能因输入失败；这与「重复项去重保首位」的注释是配套的，但一个写错的 URL 会一路走到 `doRequest` 才以 `status`/连接错误的形式暴露。
10. **`report.Wall` 与 `result.Wall` 的关系**。`runBaseline` 的 `report` 里没有总墙钟字段，而每个 `result` 有；本册无法确认产物消费者是否需要前者。

## 23. 覆盖清单

本节把任务清单里的每个名字对回它所在的节，便于核对完整性。两张表都是**每个名字至少出现一次**。

### 23.1 `cmd/loadtest` 的函数

| 符号 | 节 |
| --- | --- |
| `runBaseline` | 第 4 节 |
| `medianResult` | 第 5 节 |
| `medianTargets` | 第 5 节 |
| `runMatrix` | 第 6 节 |
| `runPhase` | 第 7 节 |
| `doRequest` | 第 8 节 |
| `runDiagnosis` | 第 9 节 |
| `startMock` | 第 10 节 |
| `mockBinary` | 第 10 节 |
| `logTail`（`mockProc` 的方法） | 第 10 节 |
| `stop`（`mockProc` 的方法） | 第 10 节 |
| `scrapeMetrics` | 第 11 节 |
| `scrapeFleet` | 第 11 节 |
| `countingReader.Read` | 第 8 节 |
| `shortTarget` | 第 12 节 |
| `shortURL` | 第 12 节 |
| `printTable` | 第 12 节 |
| `printOverhead` | 第 12 节 |
| `streamLabel` | 第 12 节 |
| `pct` | 第 12 节 |
| `ms` | 第 12 节 |
| `writeJSON` | 第 12 节 |
| `newClient` | 第 13 节 |
| `parseLevels` | 第 13 节 |
| `fatalf` | 第 13 节 |
| `hostname` | 第 13 节 |
| `goVersion` | 第 13 节 |
| `urlList.String` / `urlList.Set` / `urlList.list` | 第 14 节 |
| `normalizeTarget` | 第 14 节 |
| `newTargetSet` | 第 14 节 |
| `targetSet.pick` / `targetSet.label` | 第 14 节 |
| `pick` | 第 15 节 |
| `mean` | 第 15 节 |
| `truncate` | 第 15 节 |
| `main` | 第 3 节 |

### 23.2 `cmd/loadtest` 的类型、包级变量与字段

| 符号 | 节 |
| --- | --- |
| `result` 及其全部 26 个字段 | 第 3 节（字段级） |
| `targetStat` 及其 4 个字段 | 第 3 节（字段级） |
| `report` | 第 3 节 |
| `measurements` | 第 7 节 |
| `countingReader` | 第 8 节 |
| `mockProc` | 第 10 节 |
| `urlList` | 第 14 节 |
| `targetSet` | 第 14 节 |
| `chatBody` / `streamBody` | 第 7 节 |
| `reRequests` / `reDuration` / `reFirstTok` | 第 11 节 |

`result` 的 26 个字段在**字段级**那一节里逐个给了 JSON tag 与读法：`Label`、`Target`、`Stream`、`Concurrency`、`Requests`、`Errors`、`Round`、`QPSMin`、`QPSMax`、`P50`、`P95`、`P99`、`Max`、`Mean`、`TTFTP50`、`TTFTP95`、`TTFTMean`、`BytesMean`、`FramesMean`、`GatewayMean`、`GatewayCount`、`FirstTokenMean`、`Wall`、`Targets`。注意 `QPS` 也在其中（清单里写作 `QPSMin`/`QPSMax` 的同伴）——本册第 3 节把它单列，因为它是唯一的必填统计量。清单里写的 `Round` 对应源码里的 `Rounds`（`json:"rounds,omitempty"`）；本册按源码拼写写 `Rounds`，并在此注明清单里的 `Round` 即它。

### 23.3 `cmd/measure-m2` 的全部顶层符号

| 符号 | 节 |
| --- | --- |
| `main` | 第 17.2 节 |
| `printCurve` | 第 17.3 节 |
| `truncate` | 第 17.4 节 |
| `writeJSON` | 第 17.5 节 |
| `fatal` | 第 17.6 节 |

这 5 个就是该文件的全部顶层声明（无 `type`/`var`/`const`），所以第 17 节是**全量**覆盖，不是抽样。诚实漂移单列在第 17.7 节。

### 23.4 脚本层

| 文件 | 节 | 它量什么的一句话 |
| --- | --- | --- |
| `scripts/measure-m2.ps1` | 第 18 节 | 语义缓存的命中率/false-hit、命中路径 P95、硬宕机期间的可用性、阈值曲线 |
| `scripts/measure-m3.ps1` | 第 19 节 | 准入开销、预留准确性、预算拒绝、Redis 与内存的等价性、fail-closed |
| `scripts/measure-m4.ps1` | 第 20 节 | 三种量化的 TTFT/吞吐/显存漂移、分层路由、派生的成本替代 |
| `scripts/measure-m5.ps1` | 第 21 节 | 八个 arm 的 tracing 代价、被抓取的代价上界、2/4 实例横向扩容 |

### 23.5 本册刻意不覆盖的东西

- `cmd/verify` 与 `cmd/verify-m1`…`cmd/verify-m6` 家族的全部夹具与断言辅助符号（约 267 个）——见第 11 册。
- 构建、CI 接线与 `Makefile` 目标——见第 07 册。
- M0/M1/M6 的脚本与产物——本册只在自己用到的路径上引用它们（例如 `docs/baseline/m1-summary.json`、`docs/baseline/m6-summary.json`），不做逐脚本展开。
- 网关内部实现（`internal/` 下各包）——本册只在符号引用与数据来源的层面碰它们。

### 23.6 一条格式纪律（写给本册的维护者）

本册的散文里**不出现**带行号的文件引用（也就是「文件名 + 冒号 + 行号」那种写法），也不用段符号加数字去指别册的章节号；要指别的分册就写「第 07 册」「第 11 册」。这条纪律由仓库自检强制，改本册时须一并遵守。

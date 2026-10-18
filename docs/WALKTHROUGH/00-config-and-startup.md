# 第 00 册 · 启动与配置（cmd/infergate · internal/config · internal/miniyaml · internal/logging）

本册走读一个 InferGate 进程从 `main()` 到「可以开始接请求」之间发生的全部事情：命令行参数、YAML 文本读入、`${ENV}` 展开、环境变量覆盖、默认值填充、跨字段校验，以及日志句柄是怎么造出来的。上游连接、路由、缓存、配额、追踪那些代码不在本册，它们从服务层与网关层开始（分册索引见 `docs/WALKTHROUGH/README.md`）。

阅读约定与前几册一致：

- 锚点写成 `符号名 (路径)`，**不写行号**。行号会随编辑腐坏，而符号名由 `internal/repofmt/citedsymbols_test.go` 与 `internal/repofmt/citedpaths_test.go` 校验真实存在。
- 凡标注「源码注释的观点」的句子，其理由是源码注释给出的，不是本册作者的推断。
- 错误串一律照抄原文（含标点与 `%q` 形式），方便直接 grep 定位。

## 1. 启动数据流总览

启动顺序是固定的，且每一步都在**开始监听之前**完成，因此配置错误的表现是进程直接退出，而不是第一个请求神秘地 502：

| 步骤 | 调用 | 失败时的表现 |
| --- | --- | --- |
| 1 | `flag.Parse()` | 参数拼错由 flag 包自己打印用法并 `os.Exit(2)` |
| 2 | `config.Load(configPath)` | `infergate: configuration error: <err>` 写 stderr，退出码 1 |
| 3 | `logging.New(os.Stdout, cfg.Log.Level, cfg.Log.Format)` | `infergate: logger error: <err>` 写 stderr，退出码 1 |
| 4 | `-check` 分支 | 打印 `configuration OK: N upstream(s) from <path>` 后正常返回 0 |
| 5 | `server.NewServer(cfg, logger)` | `logger.Error("startup failed", "err", err)`，退出码 1 |
| 6 | `srv.ListenAndServe()` | 阻塞；返回即进入关闭序列 |

关闭序列在 `main()` 里是**顺序**的，不是并发关闭：先 cache、再 quota、最后 tracing。理由写在 `cmd/infergate/main.go` 的注释里，逐条见下一节。

## 2. cmd/infergate/main.go

### 包注释（cmd/infergate/main.go）

> 「M0 表面：一个讲 SSE 的 OpenAI 兼容反向代理，所以任何指向这个地址的 OpenAI SDK 都不用改就能用。后续里程碑在同一个 listener 后面加上路由、语义缓存与成本治理，而不改 wire contract。」（源码注释的观点。这句话解释了为什么后面十几节配置段会不断加进同一个 `Config`，而监听地址与 API 形状不动。）

### version (cmd/infergate/main.go)

```go
var version = "0.1.0-m0-dev"
```

- 作用：进程版本号，`-version` 直接打印它，同时由 `srv.SetVersion(version)` 交给服务层（服务器会把它当作对外披露的构建标识）。
- 读写状态：只读；包级变量，唯一写入点是链接器。
- 源码注释指出的覆盖方式：`go build -ldflags "-X main.version=1.2.3" ./cmd/infergate`，即 release 构建不靠改源码刷新版本。
- 被谁调用：`main()` 的两处（`-version` 分支的 `fmt.Println`，以及 `srv.SetVersion`）。

### main (cmd/infergate/main.go)

```go
func main()
```

- 作用：解析三个 flag、加载并校验配置、构造日志与服务、安装信号处理、监听并在退出前按顺序关闭三个外部资源。
- 三个 flag（名字、默认值、说明一字不改）：
  - `-config`，默认 `"configs/infergate.yaml"`，说明 `path to the YAML configuration file`；
  - `-check`，默认 `false`，说明 `validate the configuration and exit without serving`；
  - `-version`，默认 `false`，说明 `print the version and exit`。
- 关键顺序与理由（源码注释的观点）：
  - 配置校验放在一切之前——「在别的任何事情之前先校验，这样一个拼写错误会在启动时以清晰的消息失败，而不是在第一个请求上变成一个神秘的 502」；
  - `-version` 最先短路，连配置文件都不读（因此没有配置文件时 `-version` 依然可用）；
  - `-check` 在**日志构造之后**才短路，所以它同时验证了 `log.level` / `log.format` 是受支持的值；它打印的是上游条数 `len(cfg.Upstreams)`，这是「配置里到底有几个后端」最直接的一个数字；
  - 关闭信号用 `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`：SIGINT/SIGTERM 都会触发 `ctx.Done()`，`defer stop()` 保证监听被解除；
  - 关闭 goroutine 用 `cfg.Server.ShutdownTimeout.Duration()` 造出一个带超时的 `shutdownCtx`，所以「优雅关闭最多等这么久」是由配置决定的，不是硬编码；
  - `srv.CloseCache()`、`srv.CloseQuota()`、`srv.CloseTracing()` 的三个理由（源码注释的观点）：cache「在 listener 停止之后才关闭，这样没有请求还握着连接池里的 Redis 连接」；quota「一个还在飞行中的 settle 必须能到达它的计数器」；tracing「listener 停止时仍排在队列里的导出必须到达 sink，因为它描述的那个请求早已返回给调用方」。
- 错误路径与原文：
  - `fmt.Fprintf(os.Stderr, "infergate: configuration error: %v\n", err)` 后 `os.Exit(1)`；
  - `fmt.Fprintf(os.Stderr, "infergate: logger error: %v\n", err)` 后 `os.Exit(1)`；
  - `logger.Error("startup failed", "err", err)` 后 `os.Exit(1)`；
  - `logger.Error("server stopped", "err", err)` 后 `os.Exit(1)`（正常关闭时 `ListenAndServe` 也返回错误，见下）；
  - 三个关闭函数失败只 `logger.Warn`，不改变退出码：`"cache close failed"`、`"quota close failed"`、`"tracing close failed"`。
- 并发：`main()` 只额外起一个 goroutine（等信号并调用 `srv.Shutdown`），它与 `ListenAndServe` 之间靠 `server` 内部同步，`main()` 自己不共享可变状态。
- 最后一个动作是 `time.Sleep(10 * time.Millisecond)`，注释说明是「给上面那行日志一个到达控制台的机会再退出」。这是一个刻意的、写死在源码里的等待，不是配置项。
- 被谁调用：Go 运行时。

## 3. internal/logging/logging.go

### 包注释（internal/logging/logging.go）

> 「InferGate 从第一个请求起就输出结构化日志，因为网关的职责就是解释已经在流动的流量。」（源码注释的观点。）包注释同时说明提供两种 sink：text 给终端，json 给日志管道。

### New (internal/logging/logging.go)

```go
func New(w io.Writer, level, format string) (*slog.Logger, error)
```

- 作用：把「人类写的 level/format 字符串」翻译成 `slog.Handler`，返回一个标准库 `*slog.Logger`。它是本包唯一的导出符号。
- level 的接受值（大小写不敏感，先 `strings.ToLower`）：`debug` → `slog.LevelDebug`；`info` 或空串 `""` → `slog.LevelInfo`；`warn` 或 `warning` → `slog.LevelWarn`；`error` → `slog.LevelError`；其它一律报错。
- format 的接受值：`json` → `slog.NewJSONHandler(w, opts)`；`text` 或空串 `""` → `slog.NewTextHandler(w, opts)`；其它报错。
- 选项：`opts := &slog.HandlerOptions{Level: lvl}`，没有 `AddSource`、没有 `ReplaceAttr` 定制——输出刻意保持标准库默认形状。
- 错误路径与原文：
  - `fmt.Errorf("logging: unsupported level %q", level)`
  - `fmt.Errorf("logging: unsupported format %q", format)`
  两条都在构造 handler 之前返回 `(nil, err)`，因此调用方拿到 nil logger 就一定能看到错误。
- 读写状态：只写 `w`（由调用方传入的 `os.Stdout`），不读任何全局状态，不缓存 handler。
- 并发/锁：本函数无锁；`slog` 的 handler 自身对并发 `Log` 调用是安全的，真正的同步在标准库内部。
- 被谁调用：`cmd/infergate/main.go` 的 `main()` 是唯一调用点，参数是 `os.Stdout`、`cfg.Log.Level`、`cfg.Log.Format`。
- 设计上的代价（源码注释的观点）：输出刻意确定（不截断时间、不加颜色码），这样测试里对日志文本的断言才稳定。

### 与配置的关系

`level` 与 `format` 由 `internal/config` 的 `Validate` 先规范化（小写化、空值填默认），所以 `New` 收到的已经是合法值；`New` 仍然再校验一次，是因为它同时被当作独立的包使用（例如测试或工具直接调用），不能假设调用方一定走过配置校验。

## 4. internal/miniyaml/miniyaml.go

### 包注释（internal/miniyaml/miniyaml.go）

> 「为什么不用完整的 YAML 库？因为 M0 零第三方依赖：网关的每一行都是标准库 Go，这使依赖面可审计、构建在没有 module proxy 的情况下可复现。代价是这个包只实现 InferGate 需要的子集，其余的东西大声拒绝而不是猜。」（源码注释的观点。）

包注释同时给出受支持的语法清单（与 `configs/infergate.yaml` 对得上）：`key: value` 标量、`key:` + 两空格缩进的嵌套映射、块序列（标量与映射两种）、单行流式序列 `key: [a, b]`、空流式映射 `key: {}`（被忽略，保留默认值）、空流式序列 `key: []`、整行注释、行尾注释。明确拒绝并给出精确错误的有：用 tab 缩进、锚点/别名（`&` 与 `*`）、块标量（`|` 与 `>`）、多文档流、重复键。`Unmarshal` 另外会忽略目标结构没有对应字段的键，而 `UnmarshalStrict` 会按名字拒绝它——「一个静默丢掉拼错键的配置文件，就是一个没有说出它到底做什么的配置文件」。

### Kind (internal/miniyaml/miniyaml.go)

```go
type Kind uint8
```

- 作用：区分解析器产出的三种节点形状；底层类型是 `uint8`（一字节，节点树规模小，类型本身不承载语义）。
- 读写状态：无；纯标签。

### KindMap / KindSeq / KindScalar (internal/miniyaml/miniyaml.go)

```go
const (
	KindMap Kind = iota
	KindSeq
	KindScalar
)
```

- 作用：`KindMap` 是映射节点、`KindSeq` 是序列节点、`KindScalar` 是持有字符串的叶子。
- 注意：`KindMap` 是 `iota` 的零值，所以 `Node{}` 天然被当作空映射——这与 `Parse` 在文件为空时返回 `&Node{Kind: KindMap, Map: map[string]*Node{}}` 的形状一致，也解释了为什么 `toJSONValue` 的 `default` 分支能安全地把「非 map 非 seq」都当标量。

### Node (internal/miniyaml/miniyaml.go)

```go
type Node struct {
	Kind Kind
	Map  map[string]*Node
	Seq  []*Node
	Str  string
}
```

- 作用：解析后的 YAML 值。三个字段是三种互斥的载荷，由 `Kind` 决定读哪一个。
- 读写状态：解析阶段由 `parseMap`/`parseSeq`/`parseValue` 写入；之后由 `toJSONValue` 只读遍历。
- 并发：解析期间单 goroutine 独占；产出后不再被修改，因此并发读安全（但本包没有为「并发读同一个 Node」加任何保护，因为它被设计成一次性穿过 `decode`）。
- 被谁调用：`Parse` 的返回值；`internal/config` 通过 `miniyaml.UnmarshalStrict` 间接使用，从不直接持有 `Node`。

### Unmarshal (internal/miniyaml/miniyaml.go)

```go
func Unmarshal(data []byte, v any) error
```

- 作用：把 YAML 字节解析成 `Node`、再经 `encoding/json` 解到 `v`，因此**目标结构体的 json tag 就是字段名与类型的唯一真相**。
- 行为约定：目标没有对应字段的键被忽略；需要「忽略会掩盖错误」的场合请用 `UnmarshalStrict`。
- 实现：一行转发 `decode(data, v, false)`。
- 错误路径：`decode` 的全部错误原样冒泡（前缀见 `decode` 条目）。
- 被谁调用：包内 `decode` 的两个入口之一；仓库外的调用点由 `internal/config` 间接覆盖（`config` 只用 strict 版本）。

### UnmarshalStrict (internal/miniyaml/miniyaml.go)

```go
func UnmarshalStrict(data []byte, v any) error
```

- 作用：`Unmarshal` + 「目标没有字段的映射键是错误，且错误要点出键名」。
- 为什么必须有它（源码注释的观点，且与 `docs/USAGE.md` 的口径一致）：被忽略的键是配置文件最糟糕的结局——`quota:` 拼成 `quotas:` 会让所有配额停在默认值，网关照常启动、照常花钱，而操作者以为设过的限制一处也没生效；同样的错误在普通 decode 里**按构造就是不可见的**，因为 `encoding/json` 没有地方报告它，也无从判断那个键是拼错还是别的版本留下的字段。
- 实现：`decode(data, v, true)`，非严格路径的差别只有 `dec.DisallowUnknownFields()`。
- 被谁调用：`internal/config` 的 `unmarshalYAML` 是唯一调用点，因此「能启动的配置就是加载器读懂的配置」。
- 关联测试：`TestLoadRejectsAKeyWithNoFieldBehindIt` 与 `TestUnmarshalStrictNamesTheKeyItRefuses` 分别从 `internal/config` 与 `internal/miniyaml` 两侧钉住这条行为。

### decode (internal/miniyaml/miniyaml.go)

```go
func decode(data []byte, v any, strict bool) error
```

- 作用：三个入口里真正干活的那个：`Parse` → `json.Marshal(node.toJSONValue())` → 严格或普通 `json.Decode`。
- 读写状态：只读 `data`，只写 `v`；中间产物 `raw []byte` 是重新编码后的 JSON。
- 错误路径与原文：
  - `fmt.Errorf("miniyaml: re-encode: %w", err)`（`json.Marshal` 失败，实际只在节点树出现无法编码的值时才可能）；
  - 严格路径 `fmt.Errorf("miniyaml: decode: %w", err)`，其中 `err` 来自 `dec.Decode`，未知键时标准库的文本里含 `unknown field "xxx"`；
  - 宽松路径同样包一层 `fmt.Errorf("miniyaml: decode: %w", err)`。
- 为什么绕一圈 JSON（源码注释的观点）：解析器负责「形状与类型」的判断（引号让标量保持字符串，裸数字变成 `json.Number`），而字段映射与类型转换交给 `encoding/json`，于是 schema 只有一个真相来源。
- 被谁调用：`Unmarshal` 与 `UnmarshalStrict`。

### Parse (internal/miniyaml/miniyaml.go)

```go
func Parse(data []byte) (*Node, error)
```

- 作用：YAML 字节 → `*Node`。先 `lexLines` 做行级预处理，再驱动 `parser`。
- 三条早期出口与错误原文：
  - 空输入（`len(p.lines) == 0`）返回一个空映射节点，**不报错**——这让「全注释的配置文件」等价于「什么都不写」，由默认值兜底；
  - 首行缩进不为 0：`p.errorf(0, "document must start at column 0, got indent %d", p.lines[0].indent)`；
  - 其余交给 `p.parseNode(0)`。
- 并发：无共享状态，纯函数（除了返回的节点树）。
- 被谁调用：`decode`，以及本包的测试。

### line (internal/miniyaml/miniyaml.go)

```go
type line struct {
	no     int // 1-based source line number
	indent int
	text   string
}
```

- 作用：词法层的单位。`no` 是 1 起的源行号，注释明确写出它是 1-based——因为错误消息里报的就是它；`indent` 是去掉注释与首尾空白后的前导空格数；`text` 是 `strings.TrimSpace` 之后的整行文本。
- 读写状态：由 `lexLines` 构造，被 `parser` 只读消费。
- 为什么保留行号（源码注释的观点，体现在 `parser.errorf`）：错误必须指向源文件里的真实行号，而不是「第几个非空行」。
- 被谁调用：`Parser` 内部的 `p.lines`。

### parser (internal/miniyaml/miniyaml.go)

```go
type parser struct {
	lines []line
	pos   int
}
```

- 作用：递归下降解析器状态。`pos` 是指向 `lines` 的游标，所有 `parse*` 方法通过推进 `pos` 消费输入。
- 读写状态：`pos` 被每个方法读写；`lines` 只读。
- 并发：单 goroutine 使用；`Parse` 每次新建一个实例，因此同一个 `parser` 不会被复用。
- 被谁调用：`Parse` 构造并调用其方法。

### errorf (internal/miniyaml/miniyaml.go)

```go
func (p *parser) errorf(idx int, format string, args ...any) error
```

- 作用：统一构造带行号的错误。`idx` 是 `p.lines` 的下标而不是源行号，函数负责换算。
- 行为细节：若 `idx < len(p.lines)` 取 `p.lines[idx].no`；取不到（典型是「输入意外结束」，游标已经越过末尾）时 `no == 0`，此时输出不带行号的短形式。因此同一个函数会产出两种前缀：
  - `fmt.Errorf("miniyaml: line %d: %s", no, msg)`
  - `fmt.Errorf("miniyaml: %s", msg)`
- 读写状态：只读 `p.lines`。
- 被谁调用：`parseNode`、`parseMap`、`parseSeq`、`parseValue`（后者用于 `next` 位置）。
- 注意：`parseFlowSeq` 与 `lexLines` 不经过它，它们各自 `fmt.Errorf` 出 `miniyaml: …`，因为那两处没有 `parser` 可用。

### parseNode (internal/miniyaml/miniyaml.go)

```go
func (p *parser) parseNode(indent int) (*Node, error)
```

- 作用：消费「从当前位置开始、所有成员缩进恰好为 `indent`」的块，并按首行形状分派：行文本以 `"- "` 开头或恰为 `"-"` 时走 `parseSeq`，否则走 `parseMap`。
- 错误路径：游标已到末尾时报 `p.errorf(p.pos, "unexpected end of input, expected a mapping or sequence")`（此时无行号前缀）。
- 读写状态：推进 `p.pos`（通过被调用的两个方法）。
- 被谁调用：`Parse`、`parseValue`（空值 + 更深缩进时）、`parseSeq`（`-` 单独一行引导的嵌套块）。

### parseMap (internal/miniyaml/miniyaml.go)

```go
func (p *parser) parseMap(indent int) (*Node, error)
```

- 作用：解析一个映射块，逐行 `splitKey` 取键值，值交给 `parseValue`。
- 四种错误与原文：
  - 缩进比 `indent` 深：`p.errorf(p.pos, "unexpected indentation %d, expected %d", ln.indent, indent)`；
  - 在本该出现映射项的位置遇到序列项：`p.errorf(p.pos, "found a sequence item where a mapping entry was expected")`；
  - 行内没有 `key: value`：`p.errorf(p.pos, "expected \"key: value\", got %q", ln.text)`；
  - 同名键第二次出现：`p.errorf(p.pos, "duplicate key %q", key)`。
- 循环终止条件：`ln.indent < indent`（该块结束，交给上层），这是缩进式 YAML 的全部「块边界」机制。
- 重复键为什么是硬错误：包注释把它列在「大声拒绝」一类，因为 YAML 允许重复键，而静默取后者会掩盖一次手误。
- 读写状态：写 `node.Map`；读并推进 `p.pos`。
- 被谁调用：`parseNode`。

### parseSeq (internal/miniyaml/miniyaml.go)

```go
func (p *parser) parseSeq(indent int) (*Node, error)
```

- 作用：解析块序列，支持三种写法：裸 `-` 后跟更深块、`- ` 后跟内联映射项 `- key: value`（后续同缩进的行是它的续键）、`- ` 后跟标量。
- 内联映射项的缩进算法（源码注释用图解释，是这一节最容易看错的地方）：
  - `inner := indent + strings.Index(ln.text, rest)`——续键的对齐列是**键所在的列**，不是 dash 所在的列；
  - 注释给出的例子是 `- name: a` 时 `inner = 0 + 2 = 2`，于是缩进为 2 的 `kind: openai` 归入同一个映射；
  - 若改用 dash 列会算出 0，兄弟键就会被判成「多出来的缩进行」。
- 错误路径与原文：
  - 更深、但不是续键、也不是序列项：`p.errorf(p.pos, "unexpected indentation %d in sequence, expected %d", ln.indent, indent)`；
  - 本行不是序列项：`p.errorf(p.pos, "expected a sequence item (\"- ...\"), got %q", ln.text)`；
  - 续键行不是 `key: value`：`p.errorf(p.pos, "expected \"key: value\", got %q", cln.text)`；
  - 续键重复：`p.errorf(p.pos, "duplicate key %q", ck)`。
- 特殊值：裸 `-` 且后面没有更深行时，追加一个空的 `KindScalar` 节点（而不是报错）。
- 关联测试：`TestSeqItemMappingIndentationStyles`（三种写法都要能解析）、`TestSeqItemNestedBlockValues`（嵌套块 + 注释/空行交错，形状取自 `configs/mock.yaml`）、`TestUnterminatedItemIndentationStillRejected`（更深但不属于任何映射的 `- orphaned sequence item` 必须报错）。
- 被谁调用：`parseNode`。

### parseValue (internal/miniyaml/miniyaml.go)

```go
func (p *parser) parseValue(rest string, ownerIndent, next int) (*Node, error)
```

- 作用：解释 `key:` 之后的那段文本。`ownerIndent` 是键所在行的缩进，`next` 是紧随其后的行下标（用于报错定位与「值是不是下面的块」的判断）。
- `rest` 非空时的分派顺序：`{}` → 空映射；`[]` → 空序列；以 `[` 开头 → `parseFlowSeq`；`|`/`>` 开头 → 报错；`&`/`*` 开头 → 报错；否则一律是标量。
- 错误路径与原文：
  - `p.errorf(next, "block scalars (%q) are not supported", rest)`
  - `p.errorf(next, "anchors and aliases are not supported")`
- `rest` 为空时：若下一行缩进比 `ownerIndent` 深，就把那个块当作值（`p.parseNode(p.lines[next].indent)`）；否则返回空标量。这条规则同时是「`key:` 后面什么都没写」与「`key:` 后面是嵌套结构」的唯一区分点。
- 读写状态：读 `p.lines`，通过 `parseNode` 推进 `p.pos`。
- 被谁调用：`parseMap`、`parseSeq`（两处：内联项的首个键，以及续键）。

### parseFlowSeq (internal/miniyaml/miniyaml.go)

```go
func parseFlowSeq(s string) (*Node, error)
```

- 作用：处理单行 `[a, b, c]`。去掉首尾方括号后按顶层逗号切分，每段成为一个标量节点。
- 错误路径与原文：没有以 `]` 结尾时 `fmt.Errorf("miniyaml: unterminated flow sequence %q", s)`（不带行号，因为它是包级函数）。
- 空序列 `[]` 不会走到这里——`parseValue` 已经先行短路。
- 被谁调用：`parseValue`。
- 十四份样例配置里的实际用法：`capabilities: [chat]`、`models: ["/"]`、`headers: {}`。

### splitTopLevelCommas (internal/miniyaml/miniyaml.go)

```go
func splitTopLevelCommas(s string) []string
```

- 作用：按「顶层」逗号切分流式集合，跟踪三种状态：单/双引号内、`[]`/`{}` 嵌套深度。
- 行为细节：只有 `depth == 0` 且不在引号内时逗号才是分隔符；每一段都过 `unquote` 再 `TrimSpace`；末尾一定追加一段（所以 `[a]` 与 `[a,]` 都能得到合理结果）。
- 读写状态：纯函数。
- 被谁调用：`parseFlowSeq`。

### splitKey (internal/miniyaml/miniyaml.go)

```go
func splitKey(s string) (key, rest string, ok bool)
```

- 作用：把 `"key: value"` 切成键与值，并支持带引号的键。
- 关键规则（源码注释原文的意思）：冒号只有在**后面是空格或行尾**时才终止键，所以 `http://x` 这种带冒号的值仍然是标量而不是被切成 `http` + `//x`。这正是 `base_url: "http://127.0.0.1:9000"` 能被当成一个字符串的原因。
- 返回值：`ok == false` 表示这一行不是键值行；空串直接返回 `("", "", false)`。
- 读写状态：纯函数；键与值都过 `unquote` 与 `TrimSpace`。
- 被谁调用：`parseMap`、`parseSeq`。

### unquote (internal/miniyaml/miniyaml.go)

```go
func unquote(s string) string
```

- 作用：去掉一层成对的引号。双引号走 `strconv.Unquote`（因此支持 `\n` 一类转义），失败则退回原内层文本；单引号按 YAML 规则只把 `''` 还原成 `'`。
- 读写状态：纯函数；长度不足 2 或首尾引号不成对时原样返回。
- 被谁调用：`splitTopLevelCommas`、`splitKey`。

### lexLines (internal/miniyaml/miniyaml.go)

```go
func lexLines(src string) ([]line, error)
```

- 作用：行级预处理。先把 CRLF 与孤立 CR 统一成 `\n`，再逐行：检查制表符、去行尾注释、丢弃空白/纯注释行、量缩进、`TrimSpace` 文本。
- 唯一的错误与原文：`fmt.Errorf("miniyaml: line %d: tab characters are not allowed for indentation, use spaces", no)`——注意报错的是**整行**里是否含 `\t`，不只是缩进部分。
- 为什么拒绝 tab（源码注释的观点）：YAML 本来就禁止用 tab 缩进，而一次静默转换会隐藏真实的错误。
- 副作用很重要：注释行与空行在这里被**物理删除**，所以后面的解析器看不到它们，报出的行号却仍然是源文件里的真实行号（`line.no`）。
- 读写状态：返回新切片；不改输入字符串。
- 被谁调用：`Parse`。
- 关联测试：`TestSeqItemNestedBlockValues` 会刻意在序列项之间插入注释与空行，验证它们不影响缩进判定。

### stripComment (internal/miniyaml/miniyaml.go)

```go
func stripComment(s string) string
```

- 作用：删除不在引号内的行尾 `# …`。
- 规则：`#` 只有在「行首」或「前一字符是空格 / 制表符」时才算注释起始，因此 `base_url: "http://x"` 与 `prefix: "ig:cache#1"` 里的 `#` 不会被误伤。
- 读写状态：纯函数。
- 被谁调用：`lexLines`。

### toJSONValue (internal/miniyaml/miniyaml.go)

```go
func (n *Node) toJSONValue() any
```

- 作用：把 `Node` 树转成 `encoding/json` 能直接 marshal 的 Go 形状：映射 → `map[string]any`，序列 → `[]any`，其余 → `scalarJSONValue(n.Str)`。
- 读写状态：只读节点树，返回新容器。
- `default` 分支把「非 map 非 seq」都当标量，这与 `KindScalar` 是枚举里的最后一个值有关，也与零值节点的语义一致。
- 被谁调用：`decode`。

### scalarJSONValue (internal/miniyaml/miniyaml.go)

```go
func scalarJSONValue(s string) any
```

- 作用：给标量一个「YAML 会给出的 JSON 类型」。源码注释把三条规则说得比代码更重要：
  - **带引号的标量永远是字符串**。YAML 让引号具有权威性，配置依赖这一点：`read_header_timeout: "10s"` 必须能进入一个 `time.Duration` 字段，而模型名永远不会被强制成数字。
  - **解析成数字的裸 token 变成 `json.Number`，不是 `float64`**。`encoding/json` 会原样输出 `json.Number`，所以很大的 `max_body_bytes` 或有很多有效数字的价格能活着过一轮编解码，而不会被 `float64` 重新格式化。
  - **只有完全符合 `strconv` 数字语法的 token 才被转换**，于是 `1.2.3` 与 `v2` 保持字符串，不会变成垃圾数字。
- 具体映射：`""`/`~`/`null` → `nil`；`true`/`yes`/`on` → `true`；`false`/`no`/`off` → `false`；`strconv.ParseFloat` 成功 → `json.Number(s)`；否则原样字符串返回。带引号时：引号不成对则原样返回（注释：宁可把裸文本交给调用方，也不静默编造一个值）；双引号尝试 `json.Unmarshal` 还原转义，失败退回内层；单引号只把 `''` 还原成 `'`。
- 读写状态：纯函数。
- 被谁调用：`toJSONValue`。
- 值得注意的边界：`"yes"` 是布尔真，`"0.27"`（带引号）是字符串，`0.27`（不带引号）是数字——`internal/config` 里价格字段是 `float64`，所以两份样例配置里 `in: 0.27` 不加引号是刻意的。

## 5. internal/config/config.go（前半：加载与校验）

### 包注释（internal/config/config.go）

> 「Package config 定义 InferGate 的进程级配置，以及把它变成一个已校验、不可变值的加载器。M0 刻意让配置保持很小：一个监听地址、一组 OpenAI 兼容上游、日志选项和一本价格表。后续里程碑扩展这个 schema（路由策略、缓存策略、配额策略），而不改加载器契约。」（源码注释的观点。这一句解释了为什么 `Config` 的字段是按里程碑一路加出来的。）

### unmarshalYAML (internal/config/config.go)

```go
func unmarshalYAML(raw []byte, v any) error
```

- 作用：`internal/config` 与 `internal/miniyaml` 之间唯一的接缝，一行转发 `miniyaml.UnmarshalStrict(raw, v)`。
- 为什么是 strict（源码注释的观点）：这套结构体 tag 就是配置 schema；而配置文件是那种「两种读法不等价安全」的输入——`quota:` 拼成 `quotas:` 会让每个限制停在默认值，进程照常启动、照常服务、照常花钱，而且下游没有任何东西能分辨，因为加载器会在校验跑起来之前就把键扔掉。
- 为什么是手写解析器（源码注释的观点）：网关要能从裸工具链构建、零 module 下载；规范化成 JSON 后让 `encoding/json` 负责类型，于是类型定义上的 json tag 是 schema 的唯一真相来源。
- 读写状态：只读 `raw`，写 `v`；无并发保护，`Load` 在单 goroutine 里调用它。
- 被谁调用：`Load`，以及 `TestEveryShippedConfigDecodes`（测试直接拿它逐份解码 `configs/` 下的样例）。
- 未能确认：这段注释里提到 `-print-config` 能原样往返「加载器读懂了什么」，但 `cmd/infergate/main.go` 只定义了 `-config`、`-check`、`-version` 三个 flag，仓库里没有 `-print-config`。注释与实现已经不一致，本册不把它当作可用能力。

### Config (internal/config/config.go)

```go
type Config struct {
	Server      ServerConfig     `json:"server"`
	Upstreams   []UpstreamConfig `json:"upstreams"`
	Routing     RoutingConfig    `json:"routing"`
	Health      HealthConfig     `json:"health"`
	Log         LogConfig        `json:"log"`
	Pricing     PricingConfig    `json:"pricing"`
	Cache       CacheConfig      `json:"cache"`
	Quota       QuotaConfig      `json:"quota"`
	Tracing     TracingConfig    `json:"tracing"`
	Idempotency IdempotencyConfig `json:"idempotency"`
	Sessions    SessionsConfig    `json:"sessions"`
	Access      AccessConfig     `json:"access"`
	Models      map[string]ModelInfo `json:"models"`
}
```

- 作用：完整的进程配置。字段顺序即里程碑顺序，json tag 即配置文件里的顶层段名。
- 结构体注释标记了两处「后期加入」：`Idempotency`/`Sessions` 上方写着「M6：agent 运行时在一个 chat endpoint 之上还需要的东西」；`Access` 上方写着「Access 用一个操作者令牌把运维面（`/admin/*`、`/stats`）挡在后面。除非被配置，否则关闭」。
- 读写状态：由 `Defaults()` 造初值，被 `unmarshalYAML` 覆盖，被 `expandEnv`、`applyEnvOverrides`、`Validate` 原地规范化，最后以 `*Config` 交给服务层；`Load` 之后没有任何代码再改它，这就是包注释里「不可变值」的含义。
- 并发：`Load` 返回后按只读共享；`Validate` 是唯一的原地写入口，且被约定为幂等。
- 谁读哪些字段：`Server`/`Log` 被 `cmd/infergate/main.go` 与 `internal/server` 读；`Upstreams`/`Pricing` 被 `internal/upstream` 与 `internal/gateway` 读；`Cache`/`Quota` 被 `internal/cache`、`internal/quota` 读；`Models` 被能力端点的实现读。
- 未在本册展开：全部 13 个段的字段含义、`Defaults()` 写入的每个值、`Validate()` 的每条错误串，见文末 TODO。

### Duration (internal/config/config.go)

```go
type Duration time.Duration
```

- 作用：把「人写的时长」变成配置字段的类型。定义成 `time.Duration` 的命名类型，只为了给它挂 `UnmarshalJSON`。
- 为什么需要它（源码注释的观点）：`encoding/json` 不能把 `"90s"` 解进裸 `time.Duration`——`Duration` 底层是 `int64`，JSON 字符串就是类型错误；而在配置文件里写 `90000000000` 根本没法用。
- 为什么在 `UnmarshalJSON` 里做而不是预处理 YAML（源码注释的观点）：转换本身可独立测试，schema 也保持成「带 json tag 的普通结构体」。
- 接受的形式：`"90s"`、`"10m"`、`"1h30m"`（即 `time.ParseDuration` 的语法），以及一个裸整数——按纳秒读，这样别的工具产出的 JSON 也能加载。
- 被谁调用：`ServerConfig`、`HealthConfig`、`CacheConfig`、`EmbeddingConfig`、`RedisConfig`、`QuotaConfig`、`TracingConfig`、`OTLPConfig`、`IdempotencyConfig`、`SessionsConfig` 里的所有时长字段；十四份样例配置里的 `upstream_timeout`、`shutdown_timeout`、`ttl` 等值都走这条路。

### UnmarshalJSON (internal/config/config.go)

```go
func (d *Duration) UnmarshalJSON(b []byte) error
```

- 作用：`Duration` 的解析入口，也是整个配置里唯一自己实现的 JSON 解析。
- 分支顺序：`TrimSpace` 后为空或 `"null"` → `*d = 0` 并返回 nil；首字符是 `"` → 解出字符串再 `time.ParseDuration(strings.TrimSpace(text))`；否则按十进制整数当纳秒解。
- 错误路径与原文：
  - `fmt.Errorf("invalid duration %q (use a Go duration such as \"90s\", \"10m\", \"1h30m\")", text)`
  - `fmt.Errorf("invalid duration %s (use a quoted duration such as \"90s\")", s)`
  两条都自带修法提示，这正是两份样例配置顶部的注释反复强调「时间值必须加引号」的原因。
- 读写状态：只写接收者；无并发保护（配置加载在单 goroutine 里完成）。
- 被谁调用：`encoding/json`（经 `miniyaml` 重新编码后的 JSON），以及 `Duration` 字段上的任何解码路径。

### MarshalJSON (internal/config/config.go)

```go
func (d Duration) MarshalJSON() ([]byte, error)
```

- 作用：输出人类可读形式（`json.Marshal(time.Duration(d).String())`），使配置能被往返、被记录日志而不暴露裸纳秒数。
- 值与指针接收者的差别：`MarshalJSON` 是值接收者（`Duration`），`UnmarshalJSON` 是指针接收者（`*Duration`），因此「编码一个 nil 指针」与「解码到一个 nil 指针」是两个不同的问题。
- 读写状态：纯函数。
- 被谁调用：任何把配置编码回 JSON 的路径（`internal/repofmt` 的文档默认值检查会读 `Defaults()` 的字段，但不走 JSON；主要意义是 schema 的可往返性）。

### Duration (internal/config/config.go)

```go
func (d Duration) Duration() time.Duration
```

- 作用：返回标准库的 `time.Duration`，是配置值进入 `context.WithTimeout`、`http.Server` 超时字段等标准库接口的唯一通道。
- 命名上的注意点：方法与类型同名（`Duration.Duration()`），这是 Go 允许的，但在阅读配置消费方时要看清是类型还是方法。
- 读写状态：纯函数。
- 被谁调用：`cmd/infergate/main.go` 里的 `cfg.Server.ShutdownTimeout.Duration()`；`internal/server`、`internal/upstream`、`internal/cache` 等所有需要标准库时长的位置；测试里比较默认值时也用它（例如 `cfg.Redis.ReadTimeout.Duration()`）。

### String (internal/config/config.go)

```go
func (d Duration) String() string
```

- 作用：`time.Duration(d).String()`，让日志与错误消息显示 `"15s"` 而不是纳秒整数。
- 典型调用点是 `logger.Info("shutdown signal received", "timeout", cfg.Server.ShutdownTimeout.String())`。
- 读写状态：纯函数。

TODO: 续写员请接着覆盖以下要点，锚点一律写成 `符号名 (路径)`、不写行号——(1) internal/config/config.go 剩余全部符号：类型 ServerConfig（含 DisableKeepAlives、MaxIdleConnsPerHost 等字段与「这些 deadline 的存在是为了让卡死的上游永远不能钉住一个 goroutine」的理由）、UpstreamConfig（Name/Kind/BaseURL/APIKey/Models/Capabilities/Priority/Weight/Tier 各字段与 "/" 通配的 nginx location-prefix 出处）、LogConfig、PricingConfig、ModelPrice、RoutingConfig、RoutingWeights（Reliability 是唯一能单独把一个后端踢出轮换的项）、TierPolicyConfig（0 表示「不限制」而不是「一切都太大」）、HealthConfig（每个阈值都针对滑动窗口而非生命周期计数器，以及生命周期计数器为什么是经典熔断器 bug）、CacheConfig（默认关闭是运营决策而非谨慎；TTL 锚定创建且不被命中刷新）、EmbeddingConfig（hashing/http/none 三者与「hashing 是离线的、词法的、有测得上限」）、RedisConfig（没有 URL 形式的原因；键形状 <prefix>:<scope>:meta|vec|idx|exp 与 <prefix>:scopes）、QuotaConfig（唯一能拒绝流量的一段，因此默认惰性；FailOpen 默认 closed 的论证）、TenantQuotaConfig（租户身份 = X-InferGate-Tenant 或凭据哈希；degrade 阶梯顺序）、TracingConfig（默认关闭因为它保留每请求的载荷元数据；traceparent 继承）、OTLPConfig（手写编码器而非 OTel SDK）、AccessConfig（默认关闭的原因：M0–M6 的验收脚本都在无凭据下打 /admin/*；/metrics 不在默认保护名单里）、DefaultAccessProtect 及其返回 ["/admin","/stats"]、IdempotencyConfig（为什么 agent 重试需要它；oversize 不是失败）、SessionsConfig（回答 token 计数器回答不了的问题：哪段对话花了钱）、ModelInfo（context_window 是 wire 上观测不到的那个字段）、Defaults() 的全部默认值逐字段清单（Server 64 个空闲连接 vs 样例的 256、Health MinRequests 20 / MaxFailuresPerRequest 3、Cache 0.86/15m/256/12/512dims/5s/8000、Quota 256/4/3/reject、Tracing 1024/1.0/5s/infergate、Idempotency 15m/2048/1<<20、Sessions 24h/4096/8）、Load（BOM 剥离的三字节序列与「Notepad 默认会加」的理由、四层包装错误串 `config: read %s: %w` / `config: parse %s: %w` / `config: %s: %w` / `config: env: %w`）、Validate（幂等；server 超时非负、log level/format 规范化、upstreams 名称必填与重名、kind 只认 openai、base_url 去尾斜杠并校验 http/https 与 host、models 至少要有一项或 "/"、tier 规范化与 `unsupported tier %q (want local or cloud)`、pricing 负价、cache/embedding/redis 全部错误串、quota 与 validateTenantQuota 的阶梯检查、routing.strategy 六值与权重非负、tiered 必须至少有一个 tier: local 的上游、health 各项、权重全为 0 时统一置 1、tracing、access 的 `access.enabled is true but no token is configured` 与 `access.protect: "/" …`、idempotency、sessions、models 能力标签小写化）、validateTenantQuota（def 的继承语义与 `on_exceed is degrade but neither downgrade_model nor max_tokens_cap is set` 的空阶梯论证）、hasCatchAll、nonEmptyStrings、expandEnv（它展开的六个字段清单与「未定义变量是硬错误」的论证；错误串 `undefined environment variable(s): %s`，其中每条形如 `字段 -> $VAR`）、三个未导出常量 envListen/envLogLevel/envMaxBodyBytes 与 applyEnvOverrides（只覆盖标量；`INFERGATE_MAX_BODY_BYTES must be a positive integer, got %q`）、全部导出常量块（CacheStoreMemory/Redis、EmbedProviderHashing/HTTP/None、QuotaStoreMemory/Redis、QuotaActionReject/Degrade、KindOpenAI、TierLocal/TierCloud、StrategyPriority/Cost/Latency/Weighted/Score/Tiered）；(2) internal/config/config_test.go 全部测试（TestLoadCacheSectionFromYAML、TestCacheSectionIsOptionalAndOffByDefault、TestValidateIsIdempotent、TestCacheValidationErrors、TestCacheValidationWhileDisabled、TestAccessSectionParsesAndExpands、TestAccessValidationErrors、TestCacheSecretsExpandFromEnv、TestQuotaValidationErrors、TestQuotaValidationWhileDisabled、TestQuotaInheritance、TestQuotaRedisPasswordExpandsFromEnv、TestUpstreamTierValidation、TestTierPolicyValidation、TestTieredRequiresLocalUpstream、TestOmittedTierLoadsAsCloud、TestTracingDefaultsWhenOmitted、TestTracingSectionParses、TestTracingValidationErrors、TestTracingZeroMeansUnset、TestLoadRejectsAKeyWithNoFieldBehindIt、TestEveryShippedConfigDecodes，以及辅助函数 writeConfig 用 t.TempDir() + 0o600）；(3) internal/config/docdefaults_test.go（只读 docs/USAGE.md，表头须同时含「键」「默认」才算配置表，checked<12 与 rows<12 两道守卫）；(4) internal/miniyaml/miniyaml_test.go 的四个测试各自钉住什么；(5) 完整配置 schema 逐段表（13 段的 key → 类型 → 默认 → 生效条件）；(6) `configs/` 下 14 份 YAML 逐份说明它示范哪一组能力与关键字段值（mock / infergate / routing / routing-local / cache-local / cache-redis / quota-local / quota-redis / tiered / tiered-local / observability / agent-local / agent / docker，含各自的监听端口、演示的响应头、对应的启动命令）；(7) 环境变量展开与覆盖的完整清单，以及「未定义变量是硬错误而不是空串」的三处论证；(8) 坑与不变量清单（BOM、时间值必须加引号、`/` 通配与 first-writer-wins、tier 拼错不会静默降级到 cloud、cache/quota 的 Enabled=false 下仍做结构校验、负值一律报错而零值一律取默认、`configs/docker.yaml` 里 `capacity` 不能写成 `max_entries`）；(9) 本册到哪里为止、其余 11 册从哪些包开始。

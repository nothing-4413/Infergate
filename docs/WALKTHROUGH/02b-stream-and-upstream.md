# 第 02B 册 · 流式中继、SSE 与上游注册（internal/gateway/stream*.go · internal/sse · internal/upstream）

## 1. 本册读法

- 锚点一律写作 `符号名 (internal/sse/writer.go)`：**只给符号名与真实路径，不给行号**。行号是会被下一次提交悄悄证伪的断言，本册一个都不写（代码块例外）。
- 凡标 **【源码注释观点】** 的句子，来自源码里的 `//` 注释或 `docs/DESIGN.md` 的说明，是作者当时的理由，不是可直接验证的行为规格。
- 本册覆盖 `internal/sse/frame.go`、`internal/sse/writer.go`、`internal/gateway/stream.go`、`internal/gateway/stream_helpers.go`、`internal/sse/inspector.go`、`internal/upstream/registry.go`，以及 `forwardResponseHeaders` 一个函数。第 1 至 7 节与第 8 至 14 节出自两位并行写者，本轮只做编号归一，结论不做改写。上一轮的交接项已合并完毕，正文没有 `TODO` 残留。

## 2. 全景：一次流式请求的形状

```
relayStream (internal/gateway/stream.go)
 ├─ sse.NewWriter(w)             要求 ResponseWriter 支持 Flusher，否则 500
 ├─ sw.MirrorSSEHeader()         沿用上游 Content-Type，不被网关覆盖
 ├─ sw.WriteHeader(200)          在任何一帧之前提交状态码
 ├─ reader goroutine             独占 resp.Body ──帧/终局错误──► channel(容量 8)
 └─ 主循环 select
      ├─ r.Context().Done()      → outcome=canceled，reason="client disconnected mid-stream"
      ├─ ticker.C（15s）         → 写注释帧 ": keep-alive"
      └─ 帧
           ├─ inspector.Observe(frame)        用量/模型/流内错误
           ├─ 首个"可见 token"帧 → rec.firstToken 打点
           └─ sw.WriteUpstream(frame)         回放上游原始字节 Raw
 收尾：未见过 [DONE] 则补一个；落 usage/model/frames/respBytes；必要时存缓存
```

三点贯穿全册：**帧按字节回放**、**只有一个 goroutine 写客户端**、**无法解析的流就是无法计费的流**。

## 3. internal/sse/frame.go

包注释给出的动机 **【源码注释观点】**：朴素 `io.Copy` 中继有三个缺陷——不 flush（`net/http` 会缓冲，客户端攒到约 4 KiB 才看到东西，于是首字延迟量到的是缓冲区而不是模型）；没有 TTFB 可观测性；没有协议感知（终止事件、200 OK 里携带的上游错误、用量统计全都不可见）。帧化规则按 WHATWG HTML 的 "Server-sent events"：帧是 `field: value` 行序列，以空行终止；规范把字段名限死为 `[A-Za-z]*`，但真实 provider 会发 `retry:`、`ping:` 和厂商字段，所以读者接受冒号前的任意字节而不是静默丢弃。以 `:` 开头的行是注释，provider 用它做 keep-alive，**必须**到达客户端，否则空闲超时的代理会切断长生成。

### `MaxFrameBytes` (internal/sse/frame.go)
签名：`const MaxFrameBytes = 4 << 20`
- 作用：限制单帧长度的上限，防恶意或损坏的上游做无界分配。
- 状态：编译期常量，不可配置；`Reader.SetMaxFrameBytes` 能在实例上覆盖它。
- 判断依据 **【源码注释观点】**：流帧都是小 JSON 对象，现实里最大的是完整 tool-call 参数增量，4 MiB 远高于任何合法值。
- 调用：`NewReader` 用它初始化 `Reader.maxFrame`；`NextFrame` 用它报错。

### `Frame` (internal/sse/frame.go)
签名：`type Frame struct { Name string; ID string; RetryMillis int; Data []byte; Comment []byte; Raw []byte }`
- 作用：一帧的解析结果与原始字节。`Name` 是可选 `event:` 行；`ID` 是 `id:`；`RetryMillis` 是 `retry:`；`Data` 是所有 `data:` 行按规范拼接后的载荷；`Comment` 是注释文本（不含前导 `:`）；`Raw` 是上游原始字节。
- 状态：`Raw` 含帧的终止空行。**关键语义【源码注释观点】**：回放 `Raw` 才是中继字节透明的来源，`Data` 只是给检视用的；网关自己合成的帧 `Raw` 为 nil。
- 错误：无（纯数据）。
- 调用：`NextFrame` 构造并返回 `*Frame`；`Writer.WriteUpstream` 优先回放 `Raw`。

### `IsComment` (internal/sse/frame.go)
签名：`func (f *Frame) IsComment() bool`
- 作用：判断是不是纯注释帧，实现为 `len(f.Comment) > 0 && len(f.Data) == 0`。
- 状态：只读，不修改接收者。
- 错误：无；接收者为 nil 会 panic（调用方保证非 nil）。
- 调用：尚无调用点，供中继侧识别 keep-alive 帧。

### `IsDone` (internal/sse/frame.go)
签名：`func (f *Frame) IsDone() bool`
- 作用：判断本帧是不是 OpenAI 流的 `[DONE]` 哨兵，实现为 `string(bytes.TrimSpace(f.Data)) == "[DONE]"`。
- 状态：只读。用 `TrimSpace` 是因为载荷里可能带换行。
- 错误：无。
- 调用：`relayStream` 用它置 `wroteDone`，决定收尾是否补发 `[DONE]`；`inspector.Observe` 跳过它。

### `Reader` (internal/sse/frame.go)
签名：`type Reader struct { br *bufio.Reader; accum bytes.Buffer; lineNo int; maxFrame int }`
- 作用：把一个 `io.Reader` 切成一帧一帧的 SSE 解析器。
- 状态：`accum` 累积当前帧的原始字节，取走时清空；`lineNo` 只用于错误信息。
- 并发：**非并发安全**，每个流持有一个 `Reader`，由单个 goroutine 驱动。
- 设计理由 **【源码注释观点】**：不用 `bufio.Scanner` 的按行默认，因为 data 载荷可能跨读边界，仅按换行切分会把一个完整的 JSON 当成两个；而且中继要回放帧的精确字节，这要求自己持有一份拷贝，而不是读者缓冲区里的视图。帧先组装完再检视，载荷永不被部分解释。

### `NewReader` (internal/sse/frame.go)
签名：`func NewReader(r io.Reader) *Reader`
- 作用：构造解析器，内部为 `r` 套一层 32 KiB 的 `bufio.Reader`，并把 `maxFrame` 设为 `MaxFrameBytes`。
- 状态：返回的 `Reader` 立刻可用，但一帧都没读。
- 错误：无错误返回；`r` 为 nil 会在首次读取时 panic。
- 调用：`relayStream` 的 reader goroutine。

### `SetMaxFrameBytes` (internal/sse/frame.go)
签名：`func (r *Reader) SetMaxFrameBytes(n int)`
- 作用：覆盖单帧上限；`n <= 0` 时恢复成 `MaxFrameBytes`。
- 状态：写 `r.maxFrame`，不重置累积缓冲。
- 错误：无；超限的报错发生在 `NextFrame`。
- 调用：测试与将来需要更小上限的场景。

### `NextFrame` (internal/sse/frame.go)
签名：`func (r *Reader) NextFrame() (*Frame, error)`
- 作用：读出一整帧。干净结束返回 `io.EOF`；畸形帧返回错误但**保留流位置** **【源码注释观点】**，让调用方自己决定终止还是重新同步。
- 错误路径原文：
  - 单帧超限：`fmt.Errorf("sse: frame exceeds %d bytes (line %d)", r.maxFrame, r.lineNo)`
  - 读行失败：`readLine` 的错误原样返回
  - 流已尽且无残留字节：`io.EOF`
- 关键细节：终止符用 `bytes.TrimRight(line, "\r\n")` **恰好在一处剥一次**——空行判定依赖剥完为空（否则永远没有帧终止，整条流会塌成一帧），字段值也不能带终止符（否则事件名会变成 `"message\n"`、多行载荷会累积 `"\r\n\n"`）。空行**先写进 `accum` 再取 `Raw`**：`Raw` 必须含终止空行，否则客户端收到的是首尾相接的帧，已经不是合法 SSE，重新解析只会得到一帧。流在没发结尾空行的情况下结束时 **【源码注释观点】** 按规范派发已有内容，但被截断的上游响应应当被报告而不是吞掉。
- 并发：单 goroutine 调用。
- 调用：`relayStream` 的 reader goroutine。

### `takeRaw` (internal/sse/frame.go)
签名：`func (r *Reader) takeRaw() []byte`
- 作用：拷贝 `accum` 的字节并 `Reset`，把"本帧原始字节"的所有权交出去。
- 状态：读写 `accum`（读完全部内容后清空）。
- 错误：无。
- 调用：`NextFrame` 的两条终止路径。必须是拷贝：`accum.Bytes()` 的视图会在下一次写入时被改写。

### `parseLine` (internal/sse/frame.go)
签名：`func (r *Reader) parseLine(frame *Frame, line []byte)`
- 作用：把一行已剥终止符的内容并入 `frame`。以 `:` 开头的行剥掉 `:` 与一个前导空格后追加进 `Comment`；否则按**第一个** `:` 拆成 field/value（value 再剥一个前导空格），按 field 分派：`event`→`Name`，`id`→`ID`，`retry`→`strconv.Atoi` 成功才写 `RetryMillis`，`data`→追加 value 再加一个 `'\n'`。
- 状态：写 `frame` 的字段；`data` 追加换行是规范要求，最后那个尾换行由 `finalizeFrame` 去掉，于是多行载荷的内部换行得以保留。
- 错误：`retry` 解析失败被忽略（不报错、不置位）。
- 厂商字段：只在 `Raw` 回放里保留 **【源码注释观点】**——保持流字节透明比给每个字段建模更重要。
- 调用：`NextFrame`。

### `finalizeFrame` (internal/sse/frame.go)
签名：`func finalizeFrame(f *Frame) *Frame`
- 作用：去掉 `Data` 末尾的一个 `'\n'`（`parseLine` 每条 data 行都会补一个，最后那个是多余的）。
- 状态：原地修改并返回同一个指针。
- 错误：无。
- 调用：`NextFrame` 在返回前调用。

### `readLine` (internal/sse/frame.go)
签名：`func (r *Reader) readLine() ([]byte, error)`
- 作用：读一行，返回**拷贝**；行号在读到非空内容时自增。
- 错误路径原文：`bufio.ErrBufferFull` 时返回 `fmt.Errorf("sse: line %d exceeds the read buffer", r.lineNo+1)`；其余错误原样上抛。
- 为什么必须拷贝 **【源码注释观点】**：`bufio.ReadSlice` 借出的是内部缓冲区的视图，`NextFrame` 会持续引用正在解析的这一行，两个切片互相别名，后一次读会静默改写前一行的内容，表现为后一行的数据泄漏进更早的帧。每行一次分配，在已被网络 I/O 主导的路径上是便宜的价格。
- 调用：`NextFrame`。

## 4. internal/sse/writer.go

### `ContentType` (internal/sse/writer.go)
签名：`const ContentType = "text/event-stream; charset=utf-8"`
- 作用：网关自己生成流时的响应类型，也是存缓存时记录的 content type。
- 状态：常量。
- 为什么带 charset **【源码注释观点】**：缺这个参数时某些客户端会用错误的字符集解码含非 ASCII 的 SSE 流——任何 CJK 响应都中招。
- 调用：`writeHeaderLocked`、`relayStream` 收尾存缓存。

### `Writer` (internal/sse/writer.go)
签名：`type Writer struct { mu sync.Mutex; w http.ResponseWriter; rc *http.ResponseController; wroteHeader bool; mirroredSSE bool; bytesWritten int64; framesWritten int64 }`
- 作用：逐帧写、逐帧 flush 的 SSE 写者。
- 状态：`wroteHeader` 标记状态码是否已提交（提交后不可再改头）；`mirroredSSE` 标记调用方已复制上游 Content-Type；`bytesWritten`/`framesWritten` 供收尾记账。
- 并发：**内部用 `sync.Mutex` 串行化所有写**。设计上中继只有一个写者，但 **【源码注释观点】** `ResponseWriter` 是流式路径上唯一的共享资源，意外的第二个写者（心跳 goroutine、未来的 metrics 旁路）会交错写出半帧；无竞争的锁花纳秒，损坏的流花一次调试。逐帧 flush 是全部意义所在：它把"模型产出了一个 token"变成"客户端能渲染一个 token"，这才是首字延迟真正度量的东西。
- 调用：由 `relayStream` 创建并独占使用。

### `NewWriter` (internal/sse/writer.go)
签名：`func NewWriter(w http.ResponseWriter) (*Writer, error)`
- 作用：校验 `w` 支持 flush 后构造写者，内部包一层 `http.NewResponseController`。
- 错误路径原文：`w` 不实现 `http.Flusher` 时返回 `fmt.Errorf("sse: ResponseWriter does not support flushing (cannot stream)")` **【源码注释观点】**——把无缓冲的流通过不能 flush 的 writer 写出去只会静默退化成缓冲响应，所以大声失败。
- 状态：不写任何响应头；状态码由 `WriteHeader` 或第一次 `write` 提交。
- 调用：`relayStream`，失败时映射成 500 `TypeInternal`。

### `WriteHeader` (internal/sse/writer.go)
签名：`func (s *Writer) WriteHeader(status int) error`
- 作用：加锁后委托 `writeHeaderLocked`。
- 状态：可能置 `wroteHeader`。
- 错误：`writeHeaderLocked` 的 `Flush` 错误原样返回。
- 调用：`relayStream` 以 200 调用。

### `writeHeaderLocked` (internal/sse/writer.go)
签名：`func (s *Writer) writeHeaderLocked(status int) error`
- 作用：一次性提交响应头：`wroteHeader` 已真则空操作；未镜像时设 `Content-Type: ContentType` 与 `Cache-Control: no-cache`；**总是**设 `X-Accel-Buffering: no`；然后 `WriteHeader(status)`、置位、`Flush`。
- 为什么 `X-Accel-Buffering` **【源码注释观点】**：告诉 nginx 之流不要缓冲这个响应，否则网关前面的反向代理会重新引入逐帧 flush 正要避免的缓冲。
- 状态：写 `wroteHeader`；写响应头。
- 错误：返回 `rc.Flush()` 的错误。
- 调用：`WriteHeader` 与 `write`（后者在未写头时以 200 提交，保证任何一帧之前状态码已定）。

### `MirrorSSEHeader` (internal/sse/writer.go)
签名：`func (s *Writer) MirrorSSEHeader()`
- 作用：置 `mirroredSSE = true`，声明调用方已把上游的 Content-Type 复制过来，`writeHeaderLocked` 不得覆盖它。
- 状态：写 `mirroredSSE`，加锁。
- 错误：无返回值。
- 调用：`relayStream` 在 `WriteHeader(200)` 之前调用。

### `WriteUpstream` (internal/sse/writer.go)
签名：`func (s *Writer) WriteUpstream(f *Frame) error`
- 作用：转发上游帧。`len(f.Raw) == 0` 时退化为 `WriteData(f.Data)`（网关合成帧的情形），否则原样写 `f.Raw`。
- 状态：经 `write` 累加字节数与帧数。
- 为什么回放 Raw **【源码注释观点】**：字段顺序、空格、厂商字段、注释与终止符都按 provider 发出的样子到达客户端，中继因此字节透明，而网关仍然检视过 `Data` 用于计费。
- 调用：`relayStream` 主循环的每一帧（含流内错误帧）。

### `WriteData` (internal/sse/writer.go)
签名：`func (s *Writer) WriteData(data []byte) error`
- 作用：按 SSE 语法合成一帧，拼 `"data: "` + data + `'\n','\n'` 后交给 `write`。
- 状态：同 `write`。
- 调用：`WriteUpstream` 的退化分支、`WriteDone`、`writeStreamError`。

### `WriteComment` (internal/sse/writer.go)
签名：`func (s *Writer) WriteComment(text string) error`
- 作用：拼 `": "` + text + `'\n','\n'` 写出一个注释帧。
- 为什么用它做心跳 **【源码注释观点】**：注释是 SSE 正确的 keep-alive 原语——所有合规客户端都忽略它们，而空闲超时的代理仍然看得到流量。
- 调用：`relayStream` 的 ticker 分支，文本固定为 `"keep-alive"`。

### `WriteDone` (internal/sse/writer.go)
签名：`func (s *Writer) WriteDone() error`
- 作用：写 `[DONE]` 哨兵，等价于 `s.WriteData([]byte("[DONE]"))`。
- 状态：同 `write`。
- 调用：`relayStream` 收尾补发、`writeStreamError`。

### `write` (internal/sse/writer.go)
签名：`func (s *Writer) write(p []byte) error`
- 作用：写路径的唯一出口。加锁；未写头则以 200 提交；`s.w.Write(p)`；按实际写入字节累加 `bytesWritten`；成功则 `framesWritten++`；最后 `rc.Flush()`。
- 状态：写 `bytesWritten`、`framesWritten`、可能置 `wroteHeader`。
- 错误路径：`w.Write` 的错误直接返回（不重试、不半帧重发）；`Flush` 的错误同样上抛 **【源码注释观点】**——`Flush` 在古怪 writer 上可能返回 `http.ErrNotSupported`，`NewWriter` 已经验证过 Flusher 支持，所以把失败当传输错误让调用方停下来。
- 调用：`WriteData`、`WriteComment`、`WriteUpstream`。

### `BytesWritten` (internal/sse/writer.go)
签名：`func (s *Writer) BytesWritten() int64`
- 作用：返回已写字节数，供收尾记 `rec.respBytes`。
- 状态：加锁只读。
- 调用：`relayStream` 收尾。

### `FramesWritten` (internal/sse/writer.go)
签名：`func (s *Writer) FramesWritten() int64`
- 作用：返回已写帧数（与 `rec.frames` 的口径可能不同：后者数的是上游帧）。
- 状态：加锁只读。
- 调用：目前主要供测试与观测。

### `SetHeader` (internal/sse/writer.go)
签名：`func (s *Writer) SetHeader(key, value string)`
- 作用：在提交状态码之前设置响应头；已写头则空操作。
- 状态：加锁；写 `w.Header()`。
- 调用：`SetRetryHint`。

### `SetRetryHint` (internal/sse/writer.go)
签名：`func (s *Writer) SetRetryHint(ms int)`
- 作用：把毫秒重试提示折成秒并写成 `Retry-After` 头。
- 为什么这么做 **【源码注释观点】**：它是 SSE 语法的一部分，客户端无需任何应用层约定就会遵守。
- 调用：目前无生产调用点，留给限流/过载路径。

## 5. internal/gateway/stream.go

### `heartbeatInterval` (internal/gateway/stream.go)
签名：`const heartbeatInterval = 15 * time.Second`
- 作用：中继允许上游沉默多久之后，就往客户端写一个 SSE 注释帧。
- 为什么是 15 秒 **【源码注释观点】**：中间代理、负载均衡器和 NAT 网关通常丢弃空闲 30–60 秒的连接；模型在长 prompt 上推理时很容易在首 token 之前沉默更久，失败模式是被截断的生成而不是干净的错误；每 15 秒一个注释帧以 3 字节的代价保持 socket 温暖，而每个 SSE 客户端按规范忽略注释。
- 调用：`relayStream` 里的 `time.NewTicker`。

### `newRequestID` (internal/gateway/stream.go)
签名：`func newRequestID() string`
- 作用：铸造一个 128 位随机的请求关联 id，返回 32 个十六进制字符。
- 为什么用随机而不用计数器 **【源码注释观点】**：关联必须扛住密码管理器式的威胁模型——可猜的 request id 让攻击者读到别的租户的日志；16 字节与 UUIDv4 同宽，但没有格式化依赖。
- 错误路径：`crypto/rand` 失败（实践上不可恢复）时回退成 `"t" + hex.EncodeToString(...)` 形式的时间戳 id，让网关退化成"可关联但可猜"，而不是在请求路径上 panic。
- 调用：请求进入时赋值给 `rec.requestID`，随后出现在日志与 `X-InferGate-Request-ID`。

### `relayStream` (internal/gateway/stream.go)
签名：`func (p *Proxy) relayStream(w http.ResponseWriter, r *http.Request, resp *http.Response, target *upstream.Target, rec *record) metrics.Outcome`
- 作用：把一个已建立的上游 SSE 响应逐帧中继给客户端，同时打首字点、检视用量、按需补 `[DONE]`、必要时把整条流存进缓存。
- 结构 **【源码注释观点】**：帧由**单个 reader goroutine** 产出并经 channel 投递，于是主 select 还能同时服务心跳 ticker；替代方案（心跳 goroutine 直接写同一个 `ResponseWriter`）是对 writer 的数据竞争与半帧交错的来源。**这里恰好一个 goroutine 写客户端。**
- goroutine 契约 **【源码注释观点】**：reader goroutine 按契约独占 `resp.Body`；关闭 body（调用方的 `defer`，或从 transport 传播来的上下文取消）解除读阻塞、关闭 channel、让 goroutine 退出。**两次**向 channel 的发送都由请求上下文守卫：终局错误发送与帧发送同等重要，因为排空它的循环可能恰在前一刻因取消而返回；只有一处守卫漏掉就足够泄漏。
- 逐段行为：
  1. `sse.NewWriter(w)` 失败 → `rec.reason = err.Error()`，回 500 `TypeInternal` 与 `"streaming is not supported by this server"`，outcome 为 `metrics.OutcomeInternal`。
  2. `sw.MirrorSSEHeader()`，再 `sw.WriteHeader(http.StatusOK)`；写头失败 → `rec.reason = err.Error()`，返回 `metrics.OutcomeCanceled`。
  3. `streamStart := time.Now()` **【源码注释观点】**：这是**提交时间**，不是首 token 时间——头在模型产出任何东西之前就已 flush；从这里计量才使该指标意为"用户看到了 token"，而不是"socket 打开了"。
  4. 起 reader goroutine：`defer close(frames)`；循环 `reader.NextFrame()`；出错时 `select { case frames <- item{err: err}: case <-r.Context().Done(): }` 后返回；成功时 `select { case frames <- item{frame: frame}: case <-r.Context().Done(): return }`。channel `frames` 容量为 8。
  5. 建 `inspector := sse.NewInspector()` 与 `ticker := time.NewTicker(heartbeatInterval)`（`defer ticker.Stop()`）。
  6. 缓存捕获闭包 `capture(raw []byte)`：缓存未启用、`rec.cacheIdent` 不可用或已判定不可缓存时直接返回；累计超过 `p.maxBody` 时置 `cacheOverflow` 并清空缓冲。上限语义 **【源码注释观点】**：超出的流宁可不存，也不存成截断的——截断的回放会看起来像一个有效但没写完的答案。
  7. 四路 select：
     - `<-r.Context().Done()`：`rec.reason = "client disconnected mid-stream"`，返回 `metrics.OutcomeCanceled`。**【源码注释观点】** 客户端挂了就别再为没人读的 token 付费；关闭上游 body 会经 transport 传播。
     - `<-ticker.C`：`sw.WriteComment("keep-alive")`；失败则 `rec.reason = "heartbeat failed: " + err.Error()`，返回 `metrics.OutcomeCanceled`。
     - `it, ok := <-frames`：`!ok` → `done = true` 继续（channel 已关）；`it.err != nil` 时若 `isStreamEOF(it.err)` 为真则 `done = true` 继续（干净 EOF 是好行为流的正常结束），否则 `rec.reason = "stream read: " + it.err.Error()`，写一条流内错误帧 `"upstream stream failed: " + it.err.Error()`，返回 `metrics.OutcomeUpstreamErr`。
     - 帧：先 `inspector.Observe(frame)`；若还没打点且 `firstVisibleToken(frame)` 为真，则置 `firstData`/`firstTokenSet` 并记 `rec.firstToken = time.Since(streamStart)`。若 `obs.Err != nil`（provider 在 **200 OK 的体内**报告流中失败）：先原样转发该帧（转发失败则 `rec.reason = werr.Error()` 并返回 Canceled），再 `rec.reason = "in-stream error: " + obs.Err.Error()`，`rec.frames++`，返回 `metrics.OutcomeUpstreamErr` **【源码注释观点】**——状态码已经提交，带内错误帧是剩下的唯一通道，这正是官方 SDK 检测该情形的方式。否则 `sw.WriteUpstream(frame)`（失败则 `rec.reason = "client write: " + err.Error()`，返回 Canceled）、`capture(frame.Raw)`、`rec.frames++`，`frame.IsDone()` 时置 `wroteDone = true`。
  8. 收尾：未见过 `[DONE]` 就补一个（失败则 `rec.reason = "client write: " + err.Error()`，返回 Canceled），并把合成的 `"data: [DONE]\n\n"` 也交给 `capture`。**为什么补【源码注释观点】**：有的后端发完最后一块就关流而不发哨兵，按 OpenAI SDK 写的客户端把 `[DONE]` 当流结束信号，所以合成它而不是让客户端挂到自己的超时；不含 `[DONE]` 的缓存回放会让客户端等一个永不到来的结束。
  9. 记账：把 `inspector.Usage()` 折进 `rec.usage`（Prompt/Completion/Cached），`rec.respBytes = sw.BytesWritten()`，`inspector.Model()` 非空则覆盖 `rec.model` **【源码注释观点】**——provider 自称的模型名才是定价权威。若 `total > 0 && parsed == 0` 则 `rec.reason = "stream carried no parseable frames"`（仍然成功返回，但留下可查的原因）。若 `inspector.Err() != nil` 则 `rec.reason = "in-stream error: " + …` 并返回 `metrics.OutcomeUpstreamErr`。最后 `firstTokenSet` 时置 `rec.firstTokenSet`、打一条 `"stream complete"` debug 日志，并按 `cacheOverflow` 决定是记 `rec.cacheReason` 还是 `p.storeCache(...)`，返回 `metrics.OutcomeSuccess`。
- 状态：写 `rec` 的一大批字段（`reason`、`firstToken`、`firstTokenSet`、`frames`、`usage`、`respBytes`、`model`、`cacheReason`）；写客户端响应；写缓存。**不**修改 `resp`，但通过关闭它解除 reader 阻塞。
- 错误路径与返回的 outcome 对照：服务器不支持 flush → `OutcomeInternal`；写头/写帧/心跳失败 → `OutcomeCanceled`；读流失败 → `OutcomeUpstreamErr`；流内错误 → `OutcomeUpstreamErr`；客户端断开 → `OutcomeCanceled`；正常走完 → `OutcomeSuccess`。
- 并发：主 goroutine 独占 `Writer`；reader goroutine 独占 `resp.Body`；两者只通过容量 8 的 channel 交流。
- 调用：`proxy.go` 在响应 `Content-Type` 是 event-stream 时调用它，否则走 `copyWhole`。

### `firstVisibleToken` (internal/gateway/stream.go)
签名：`func firstVisibleToken(f *sse.Frame) bool`
- 作用：判断这一帧是不是"用户能看见的输出"，用于首字打点。
- 判定：`f == nil || len(f.Data) == 0 || f.IsDone()` → false；否则把 `bytes.TrimSpace(f.Data)` 反序列化进一个只取 `choices[].delta.content`、`choices[].delta.reasoning_content`、`choices[].delta.tool_calls`、`choices[].message.content` 的 envelope；任意一项非 nil 非空即 true。
- 错误路径：**JSON 解析失败返回 true** **【源码注释观点】**——无法解析的体无法判断它是否携带输出，对延迟指标来说计数是更安全的失败模式：它只会高报，永不隐藏真实的停顿。
- 为什么这么判 **【源码注释观点】**：朴素判断"任何 data 非空的帧"会在 provider 的开场 role 帧（`{"delta":{"role":"assistant"}}`，content 为空或缺失）上触发，而该帧在 provider 接受请求时就发出，于是把排队时间报成生成延迟，在快回环上还会测得 0 而看起来像丢了样本。诚实的信号是携带用户可见输出的帧：content delta、reasoning delta 或 tool-call delta；终止符永不算，错误帧也不算（那是失败，不是首 token）。
- 并发：纯函数。
- 调用：`relayStream` 主循环，并且只在还没打过点的时候调用一次。

### `writeStreamError` (internal/gateway/stream.go)
签名：`func (p *Proxy) writeStreamError(sw *sse.Writer, msg string)`
- 作用：在一条已经提交的流内部发一个 OpenAI 形状的错误帧，然后补 `[DONE]`：组 `ErrorBody{Error: ErrorDetail{Message: msg, Type: TypeBadGateway}}`，`marshalJSON` 成功就 `WriteData`，最后 `WriteDone`。
- 为什么需要它 **【源码注释观点】**：HTTP 状态码已经发出去了，这个帧是剩下的唯一通道。
- 错误：两处写错误都被显式丢弃（`_ =`）——此时已经没有别的补救手段，且结果已注定是上游错误。
- 调用：`relayStream` 的"读流失败"分支。

## 6. internal/gateway/stream_helpers.go

### `marshalJSON` (internal/gateway/stream_helpers.go)
签名：`func marshalJSON(v any) ([]byte, error)`
- 作用：编码 JSON，但**关掉 HTML 转义**：`bytes.Buffer` + `json.NewEncoder` + `enc.SetEscapeHTML(false)`，并把 `Encoder.Encode` 追加的换行剥掉，使结果能直接嵌进 SSE 的 `data:` 字段。
- 为什么 **【源码注释观点】**：`encoding/json` 默认转义 `<`、`>` 和 `&`；通过带内错误帧回显的错误消息是**用户可见文本**，转义会产生与 provider 对同一消息本会发出的响应不同的结果，破坏那些按错误字符串匹配的客户端。
- 错误：缓冲写入不会失败，只有编码失败（如不支持的类型）才返回错误；`writeStreamError` 对它做了忽略。
- 调用：`writeStreamError`，以及网关其它自造 JSON 的路径。

### `isStreamEOF` (internal/gateway/stream_helpers.go)
签名：`func isStreamEOF(err error) bool`
- 作用：把"正常的流结束"从"真正的读失败"里分出来，实现为 `err == nil` 返回 true，否则 `errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)`。
- 为什么 **【源码注释观点】**：因为调用方读完而关闭的 body，或 transport 在最后一帧之后拆掉连接，都表现为 `io.EOF` 或 `net.ErrClosed`；把它们当失败会把健康的流报成上游错误，并触发对一个已完成请求的无意义重试。
- 并发：纯函数。
- 调用：`relayStream` 的读流错误分支。

## 7. 横切主题

- **字节透明**：中继的产物必须能被一个真正的 SSE 客户端重新解析成与上游完全相同的帧序列。三个必要条件都在 `internal/sse/frame.go` 里：终止符恰好在一处剥一次（既让空行可判定、又让字段值干净）、`Raw` 里补回终止空行、每行读出来就拷贝。`docs/DESIGN.md` 记的踩坑值得记住：只数帧数的验证会漏掉"首尾相接的帧"这类错误，**必须用真客户端重新解析来验证**。
- **无法解析的流就是无法计费的流**：`NextFrame` 遇到畸形帧选择返回错误，`relayStream` 选择终止整条流并写错误帧，理由是继续中继也拿不到用量、模型名或终止原因——网关会把它报成成功。反过来，`inspector` 对无法解析的帧只增加 `frames` 计数、不 panic、也不假装有遥测，于是"整条流一帧都没解析出来"会留下 `rec.reason = "stream carried no parseable frames"`。
- **单写者与心跳**：`Writer` 内部有锁，但设计上只有一个写者；心跳走的是同一个写者（主循环的 ticker 分支），所以注释帧永远不会插在一帧中间。心跳的存在理由是中间设备的空闲超时。源码注释写「成本是每 15 秒 3 个字节」，但该注释与实现不符：`WriteComment("keep-alive")` 实际写出的是注释帧行 `: keep-alive`，按字节算远不止 3 字节——此处以实现为准，注释数字存疑。
- **起始时间取提交时间**：`streamStart` 在写响应头之后立刻取，因此 `rec.firstToken` 度量的是"客户端从流开始到看见第一个可见 token 的等待"，包含上游排队与推理时间，而不是"socket 建立耗时"。`rec.firstTokenSet` 与 `rec.firstToken` 分开存，是因为合法的首 token 在粗粒度时钟上可能恰好测得 0，用值本身当"是否记录过"的标志会丢掉这个样本。
- **流中途断开不能故障转移**：故障转移只在**首帧之前**成立。已经写进客户端的字节收不回来，重试会把两个后端的输出拼成一份"看起来正常"的答案，而且两份都计费；即使网关想补，第二个 `[DONE]` 也是对协议的违反——客户端在第一个 `[DONE]` 之后就已经结束读取，之后到达的帧只会被丢弃或被当成脏数据。**【源码注释观点】**（同时见 `docs/DESIGN.md` 的 8.4 节：可重试类别是连接错误/超时/429/5xx，4xx 原样回传且不算后端失败。）

## 8. 本册读法与衔接

- 本册由两轮写作合成后统一编号：第 3 至 7 节讲 `internal/sse/frame.go`、`internal/sse/writer.go`、`internal/gateway/stream.go`、`internal/gateway/stream_helpers.go` 与它们的横切主题；第 8 节说明合册后的读法与边界；第 9、10 两节接续讲 `internal/sse/inspector.go` 与 `internal/upstream/registry.go` 全文；第 11 节补 `forwardResponseHeaders (internal/gateway/proxy.go)`，第 12 节汇总横切主题，第 13 节列未能确认项。
- 锚点一律写作 `符号名 (internal/sse/inspector.go)`：只给符号名与真实路径，**不给行号**。标 **【源码注释观点】** 的句子译自源码注释里的"为什么"，是作者当时的理由，不是可验证的行为规格。
- 条目粒度：每个函数、方法、类型、有语义的字段各自独立成条（含签名的紧凑单行条目也算独立条），纯数据字段逐字段列出、不合并、不省略；下文 `## ` 分节之间留空行，条与条之间不留，以守住篇幅上限。

## 9. internal/sse/inspector.go

这个文件是"无法解析的流就是无法计费的流"那句横切主题的落点：`relayStream` 主循环每收到一帧就调一次 `Observe`，收尾时读访问器把用量、模型与解析率折进 `record`；它也是全仓库唯一把 provider 的 `usage`/`tool_calls`/带内错误从 SSE 帧里解出来的地方。

- **Usage` (internal/sse/inspector.go)**
签名：``type Usage struct { PromptTokens int `json:"prompt_tokens"`; CompletionTokens int `json:"completion_tokens"`; TotalTokens int `json:"total_tokens"`; CachedTokens int `json:"cached_tokens"` }``
- 作用：一次请求的 token 记账单位，流式与非流式两条路径共用同一个类型；读写作是 `Observe` 逐帧构造并 `merge` 进累加器，`UsageFromResponse` 为非流式响应构造同一个类型。
- 为什么把缓存 token 单列 **【源码注释观点】**：三个计数器的用途本就不同——prompt tokens 决定输入成本；completion tokens 决定输出成本，而且是**预算策略在流中途唯一能截断的量**；cached tokens 被多数 provider 按折扣计价，把它折进 prompt 总数会**高估花费**。
- 措辞差异：注释说"三个计数器"，结构体实际有四个字段；`TotalTokens` 是 provider 自报的总数，网关从不自己推算，但 `Empty` 与 `merge` 都把它当第四个计数器处理。
- **`PromptTokens int`**（JSON `prompt_tokens`）：输入侧 token 数；参与 `Empty` 判定，由 `merge` 非零覆盖。
- **`CompletionTokens int`**（JSON `completion_tokens`）：输出侧 token 数，预算策略可截断的那一个；参与 `Empty` 判定，由 `merge` 非零覆盖。
- **`TotalTokens int`**（JSON `total_tokens`）：provider 自报的总量；网关从不计算；由 `merge` 非零覆盖。
- **`CachedTokens int`**（JSON `cached_tokens`）：折扣计价的缓存命中量；流式路径上只能来自 `prompt_tokens_details.cached_tokens`；由 `merge` 非零覆盖。

- **Empty` (internal/sse/inspector.go)**
签名：`func (u Usage) Empty() bool`
- 作用：报告"四个计数器**全**为 0"，实现为四个字段的 `&&` 链；值接收者，只读，无锁，无错误路径。
- 边界：它回答的是"看起来没有用量"，**不是**"provider 没报 usage 块"——`Observe` 在 `usage` 块存在时无条件赋值，四个 0 与"没报"在零值上无法区分。
- 调用：除定义处外全仓库只有 `internal/gateway/stream_test.go` 一处使用；生产路径上"这条流有没有用量"由 gateway 自己那份 `(*Usage).IsEmpty` (internal/gateway/proxy.go) 判定。

- **merge` (internal/sse/inspector.go)**
签名：`func (u *Usage) merge(src Usage)`
- 作用：把 `src` 里**非零**的计数器覆盖进 `u`，四个字段各自独立判断；指针接收者只写 `u`、只读 `src`，无错误返回。
- 语义要点：名字叫 merge，做的**不是加法而是"非零覆盖"（后写者胜）**。这是刻意的：provider 会把同一个累计总数在多帧（尤其收尾帧）里重复上报，逐帧相加会让用量翻倍，而"非零才覆盖"对重复上报是幂等的。
- 副作用：零值**不会**清空已有的非零值，所以一条流里先报 100 后报 0 的字段保留 100。
- 并发与调用：无锁，依赖"一个流一个 `Inspector`、单 goroutine 驱动"的契约；非导出，唯一调用点是 `Observe` 里的 `in.usage.merge(u)`。

- **ToolCall` (internal/sse/inspector.go)**
签名：`type ToolCall struct { Index int; ID string; Name string; Args []byte }`（无 JSON 标签，纯内部类型）
- 作用：一次工具/函数调用，由多帧 delta 拼装而成；`Observe` 从 `in.toolCalls` 取槽位续写，并把一份克隆投影进 `Observation.ToolCalls`。
- 为什么解析放在这里 **【源码注释观点】**：OpenAI 风格的 provider 会把一次 tool call 拆到很多帧——首个 delta 带 id 与函数名，后续 delta 带 JSON 参数的**片段**。忽略这一点的中继既无法报告"这个 agent 调用了 search_web"，也无法按工具做预算，所以解析放在这里而不是应用代码里。
- **`Index int`**：该调用在本次响应里的槽位号，也是跨帧聚合的**唯一键**；帧里省略 `index` 时取 0。
- **`ID string`**：provider 给的调用 id，通常只在首个 delta 出现；`Observe` 只在非空时赋值，避免后续空串把首帧的 id 抹掉。
- **`Name string`**：函数名，与 `ID` 同样"只在非空时赋值"。
- **`Args []byte`**：**已经拼接完**的 JSON 参数字节。与 `ID`/`Name` 相反，它**总是追加**（`append(acc.Args, tc.Function.Arguments...)`），因为参数是一段段流式到达的文本；跨帧共享同一个底层数组，因此投影进 `Observation` 时会 `bytes.Clone`。

- **Observation` (internal/sse/inspector.go)**
签名：`type Observation struct { Usage Usage; Model string; ToolCalls map[int]*ToolCall; FinishReason string; Err *StreamError; JSON bool }`
- 作用：`Observe` 对**这一帧**的检视结果（不是累加器）。之所以单帧一份：`relayStream` 要在帧到达的当刻决定"这帧是不是带内错误、要不要立刻终止流"，累加器回答不了这个问题。
- **`Usage Usage`**：本帧 `usage` 块解出的计数器。注意 `env.Usage != nil` 时**无条件**赋值，所以四个字段全 0 与"没报 usage"在值上无法区分。
- **`Model string`**：provider 在本帧自称的模型名。**【源码注释观点】** provider 可能把别名解析成一个具体的快照名，因此响应才是定价查询的权威来源。
- **`ToolCalls map[int]*ToolCall`**：本帧出现的工具调用增量，按索引键；本帧没有工具调用时为 **nil**（懒分配），调用方必须判 nil。
- **`FinishReason string`**：provider 报的终止原因，空串表示本帧没报。
- **`Err *StreamError`**：**【源码注释观点】** 上游错误被装进 200 OK 的流体内。OpenAI 及其克隆体用这种方式报告流中失败而不是用状态码，所以只检查状态码的中继会把一次失败的生成报成成功。
- **`JSON bool`**：本帧 `Data` 是否解析成了 JSON 对象；这是"空帧/`[DONE]`/畸形帧"与"正常帧"之间唯一的区分标志。

- **StreamError` (internal/sse/inspector.go)**
签名：``type StreamError struct { Message string `json:"message"`; Type string `json:"type"`; Code any `json:"code"`; Param any `json:"param"` }``
- 作用：流体内携带的错误信封；由 `Observe` 的 `env.Error` 解出，同时进 `Observation.Err` 与累加器 `streamErr`。
- **`Code any` / `Param any`**：用 `any` 而不是 `string`/`int` 是**推断**（源码未写理由）：provider 对这两个字段时给字符串时给数字，定死类型会让整个 `Unmarshal` 失败并连带丢掉这一帧的错误——对一条"只检查状态码就会误报成功"的流来说代价太高。

- **StreamError.Error` (internal/sse/inspector.go)**
签名：`func (e *StreamError) Error() string`
- 作用：把错误信封折成一行文本，三段分派：`Type != "" && Message != ""` → `e.Type + ": " + e.Message`；`Message != ""` → `e.Message`；否则返回字面量 `"upstream stream error"`。
- nil 安全：接收者为 nil 时返回字面量 `"<nil>"`、不 panic——因为它可能被随手塞进日志或包进别的错误链。
- 状态与调用：只读、无锁、无错误路径；`relayStream` 用它组 `rec.reason`（形如 `"in-stream error: " + obs.Err.Error()`）。

- **Inspector` (internal/sse/inspector.go)**
签名：`type Inspector struct { usage Usage; model string; toolCalls map[int]*ToolCall; finishReason string; streamErr *StreamError; frames int; jsonFrames int }`
- 作用：一条响应跨帧的累加器。
- 为什么必须不分配、不 panic **【源码注释观点】**：它跑在中继路径上，所以对**解析不了**的帧绝不能分配、对畸形输入绝不能 panic——一个遇到截断 JSON delta 就死的网关，比一个报告"这条流没记上账"的网关更糟。
- 精度：严格说"不分配"只覆盖失败路径（那里只 `return obs`）；修复分支补 `{` 时会分配一次新切片，`Observation.ToolCalls` 也会在建 map 时分配。
- 并发：**无锁、无原子**，契约是一帧一帧由单个 goroutine 驱动（`relayStream` 主循环）；访问器同样不加锁，且部分访问器借出内部引用。
- **`usage Usage`**：跨帧累加值；只由 `merge` 写，由 `Usage()` 读。
- **`model string`**：最后一个非空 `model` 胜；由 `Model()` 读。
- **`toolCalls map[int]*ToolCall`**：累加器，由 `NewInspector` 预建；槽位只增不减、承载仍在被 append 的 `Args`。
- **`finishReason string`**：最后一个非空 `finish_reason` 胜；由 `FinishReason()` 读。
- **`streamErr *StreamError`**：**最后一个**错误信封胜（一条流里出现两个错误信封时只留后一个）；由 `Err()` 读。
- **`frames int`**：每调一次 `Observe` 就自增——自增发生在**所有早退之前**，所以 `[DONE]` 帧与空帧也计入。
- **`jsonFrames int`**：只有 `Unmarshal` 成功才自增，因此 `[DONE]` 帧与畸形帧都不计入。

- **NewInspector` (internal/sse/inspector.go)**
签名：`func NewInspector() *Inspector`
- 作用：返回一个可以服务**一条**流的 `Inspector`，实现只有 `&Inspector{toolCalls: map[int]*ToolCall{}}` 一句；其余字段全零值，`frames`/`jsonFrames` 从 0 起。
- 为什么预建 map **（推断，源码未写理由）**：nil map 可读不可写，而 `Observe` 里对 `in.toolCalls[idx]` 有赋值路径，不预建首帧就会 panic——而"绝不 panic"正是这个文件写在类型注释里的硬约束。
- 调用：`relayStream` 每个流构造一个；`internal/gateway/stream_test.go` 直接构造以驱动 `Observe`。

- **Observe` (internal/sse/inspector.go)**
签名：`func (in *Inspector) Observe(f *Frame) Observation`
- 作用：把一帧折进累加状态，并返回**这一帧自己**贡献的观测；无锁，单 goroutine，被 `relayStream` 主循环（每帧一次）与 `internal/gateway/stream_test.go` 调用。
- 第 0 步：`in.frames++` 无条件先执行，因此后面每条早退都仍然计入帧数。
- 早退 1：`len(f.Data) == 0 || f.IsDone()` → 返回零值 `Observation`（`JSON` 为 false）。`[DONE]` 因此既不解析也不计入 `jsonFrames`；`f` 为 nil 会 panic（无 nil 检查，调用方保证）。
- 修复分支：`payload[0] != '{'` 时前置一个 `'{'`。**【源码注释观点】**"OpenAI 兼容 provider 总是发一个 JSON 对象；有些非规范部署会发一个裸 JSON 片段；容忍它，而不是把记账丢在地上。"它**只补左括号、不补右括号**，救的是"丢了开括号但保留了闭括号"的片段；真正的截断 JSON 仍然解析失败。这也是本函数唯一会分配的地方。
- 解析失败：`json.Unmarshal` 出错就 `return obs`——不 panic、不记错误、不假装有遥测；调用方从 `JSON=false` 得知这一帧没记上账。
- 解析成功：置 `obs.JSON = true` 并 `in.jsonFrames++`。
- `model`：非空就同时写 `obs.Model` 与 `in.model`（累加器后写者胜）。
- `usage`：`env.Usage != nil` 时把四个计数器装进局部 `Usage`（`CachedTokens` 只能来自 `prompt_tokens_details.cached_tokens`），赋给 `obs.Usage`，再 `in.usage.merge(u)`。
- `error`：`env.Error != nil` 时同时写 `obs.Err` 与 `in.streamErr`。
- `choices` 循环：`finish_reason` 是指针，非 nil **且非空串**才写（用 `*string` 正是为了区分"字段缺失"与"字段为空串"）；`delta` 为 nil 就 `continue`。
- 工具调用如何跨帧拼装：按 `Index` 聚合——`index` 缺失时 `idx` 取 **0**，于是所有省略 index 的增量会并进 0 号槽（源码注释未说明该退化是否可接受）；每个槽在 `in.toolCalls` 里只有一个 `*ToolCall`，首次出现时新建 `&ToolCall{Index: idx}` 并登记，后续帧原地续写。
- `ID` / `Name` 只在非空时赋值，所以后续 delta 带的空串不会覆盖首帧的值；`Arguments` 则相反，**总是追加**——这一条就是"工具调用参数跨帧拼装"的全部实现。
- 返回前的克隆：`cp := *acc` 浅拷贝（`Index`/`ID`/`Name` 是值或不可变字符串），`cp.Args = bytes.Clone(acc.Args)` 深拷贝参数，最后 `obs.ToolCalls[idx] = &cp`。理由（推断，源码未写理由）：返回的 `Observation` 会被调用方留到下一帧之后，而 `acc.Args` 的底层数组会被随后的 `append` 原地续写——不 Clone 就会让已发出的快照跟着变，与 `internal/sse/frame.go` 里 `bufio.ReadSlice` 返回缓冲视图导致的行别名问题属于同一类 bug。
- 写状态与分配：写 `frames`、`jsonFrames`、`model`、`usage`、`streamErr`、`finishReason`、`toolCalls` 的槽位；分配发生在修复分支的新切片与 `obs.ToolCalls` 的 map（仅当本帧出现工具调用）。

- **访问器**（六个独立条目，均在 `internal/sse/inspector.go`）
- **`func (in *Inspector) Usage() Usage`**：返回**值类型**的累加用量快照；`relayStream` 收尾把它折进 `rec.usage` 的 Prompt/Completion/Cached 三个字段。
- **`func (in *Inspector) Model() string`**：返回 provider 声明过的模型名，没有任何一帧声明过就是空串；`relayStream` 收尾在非空时覆盖 `rec.model`（**【源码注释观点】** provider 自称的模型名才是定价权威）。
- **`func (in *Inspector) ToolCalls() map[int]*ToolCall`**：**直接返回内部 map，不拷贝也不加锁**；元素是累加器里的同一批指针，`Args` 还可能被后续帧继续 append，所以调用方必须只读、且不得跨 goroutine 读。本仓库内没有生产调用点，只有 `internal/gateway/stream_test.go` 使用。
- **`func (in *Inspector) FinishReason() string`**：返回终局原因，没到过终局就是空串；本仓库内没有生产调用点，只有 `internal/gateway/stream_test.go` 使用。
- **`func (in *Inspector) Err() *StreamError`**：返回流体内出现过的错误信封；`relayStream` 收尾用它，非 nil 时写 `rec.reason` 并返回 `metrics.OutcomeUpstreamErr`——即"帧都转发完了，但这条流其实是失败的"。
- **`func (in *Inspector) Frames() (total, jsonFrames int)`**：一次返回两个计数，供"解析率"判断；`relayStream` 收尾在 `total > 0 && parsed == 0` 时记 `rec.reason = "stream carried no parseable frames"`（仍然算成功，但留下可查的原因）。

- **UsageFromResponse` (internal/sse/inspector.go)**
签名：`func UsageFromResponse(body []byte) (Usage, string, bool)`
- 作用：从**非流式**的完整响应体里抽出 token 记账，返回 `(用量, 模型名, 是否成功)`；纯函数，只读入参，无并发问题。
- 失败条件（原文）：`json.Unmarshal` 出错**或** `env.Usage == nil` 时返回 `Usage{}, "", false`。**【源码注释观点】** 返回 false 对 `/v1/models` 这类响应是正常的，不是错误。
- 为什么在流式响应上不可用：它把**整段 body 当作一个 JSON 对象**解。SSE 流是 `data: {...}` 的帧序列，`Unmarshal` 会在第一个 `data:` 字面量处失败；即便某 provider 只发一帧，帧前缀与多帧拼接也都不构成单个合法对象。可用的条件只有一条：`Content-Type` 是 JSON 的**非流式**响应体（网关里由 `isJSON` 判别的那条路径）。
- 调用：**本仓库内没有任何调用点**（生产代码与测试都没有）；唯一提到它的是 `internal/gateway/proxy.go` 里 `usageFromJSON` 上面那句"adapts sse.UsageFromResponse"的注释，而 `usageFromJSON` 其实是一份独立的孪生实现，自己填 `gateway.Usage` 的 Prompt/Completion/Cached。这是本册发现的一处事实性偏差。

## 10. internal/upstream/registry.go

包注释的立意 **【源码注释观点】**：M0 每个请求只解析**一个**后端（显式的 `X-InferGate-Upstream`，或模型到后端的映射）。这层抽象的价值在于解析步骤已经被隔离在接口之后，于是 M1 可以把成本/延迟/健康策略放进同一次调用而不动 proxy。

- **Target` (internal/upstream/registry.go)**
签名：`type Target struct { Name string; BaseURL string; APIKey string; Kind string; Transport http.RoundTripper; catchAll bool; patterns []string; capabilities []string; priority int; weight float64; tier string }`
- 作用：一个**已解析**、可以直接收请求的后端。生命周期上由 `New` 构造一次，之后**不再被任何人修改**；所有访问器都建立在这个不变式上，这也是它们不需要加锁的原因。
- **`Name string`**：配置里的后端名；`byName` 的键、日志与 `X-InferGate-Upstream-Name` 的值。
- **`BaseURL string`**：**【源码注释观点】** 后端根地址，**不含尾部斜杠**；`New` 用 `strings.TrimRight(strings.TrimSpace(...), "/")` 归一化。
- **`APIKey string`**：**【源码注释观点】** bearer 凭据；无鉴权的本地后端为空串，此时调用方的 `Authorization` 会被透传。
- **`Kind string`**：provider 方言；**【源码注释观点】** M0 只存在 `config.KindOpenAI`（配置校验会把空值补成它并拒绝别的值）。
- **`Transport http.RoundTripper`**：**【源码注释观点】** 每个 target 一个 HTTP transport，而不是全局一个，因为连接池大小与 TLS 设置是**按后端**的：本地 vLLM 想要很多空闲连接，远端 provider 想要少量加更长的握手预算。字段类型是接口而不是 `*http.Transport`，这一点让 `CloseIdleConnections` 的类型断言成为可能。
- **`catchAll bool`**（未导出）：**【源码注释观点】** 记录该后端是否在自己的模型列表里声明了 `"/"`；它只是诊断用的记账，**解析永远走 registry 的索引、从不看这个标志**，于是一个请求的代价是一次 map 查找而不是一趟扫描。
- **`patterns []string`**（未导出）：配置里的模型列表，保留原始拼写与顺序供内省（测试断言顺序被保留）。
- **`capabilities []string`**（未导出）：**【源码注释观点】** 该后端声明的特性标签，统一小写，于是匹配不受运维人员大小写习惯影响。
- **`priority int`**（未导出）：`"priority"` 策略下排序用，小者胜。
- **`weight float64`**（未导出）：`"weighted"` 策略下的相对份额。
- **`tier string`**（未导出）：分层部署里属于 local 还是 cloud 那一半；**【源码注释观点】** `config.Validate` 会归一化它，所以它永远是两者之一、绝不为空。

- **Target` 的访问器（七个独立条目，均在 internal/upstream/registry.go）**
- **`func (t *Target) IsCatchAll() bool`**：报告该后端是否接受任意模型名，实现为返回 `t.catchAll`；调用方是 `internal/gateway/capabilities.go` 的内省报告、`(*Proxy).resolveModelName`、`(*Router).fallbackFor`、`internal/server/server.go`。
- **`func (t *Target) ModelPatterns() []string`**：列出该后端能服务的模型名，`"/"` 也在其中；**返回内部切片本身**（不拷贝、不加锁，调用方只能读）；调用方是 `(*Proxy).resolveModelName`、`(*Router).modelFor`、`internal/gateway/capabilities.go`。
- **`func (t *Target) Capabilities() []string`**：按配置顺序列出已声明（且已小写化）的特性标签；同样借出内部切片；调用方是 `internal/gateway/capabilities.go`。
- **`func (t *Target) Priority() int`**：返回配置的优先级，小者胜；调用方是 `internal/router/router.go` 的各排序函数与 `internal/server/server.go` 的内省视图。
- **`func (t *Target) Weight() float64`**：返回加权路由的份额；调用方是 `(*Router).orderWeighted` 与 `internal/server/server.go`。
- **`func (t *Target) Tier() string`**：返回该后端属于分层部署的哪一半；调用方是 `(*Router).tierFor`/`orderTiered`、`internal/gateway/capabilities.go`。
- **`func (t *Target) Tags() string`**：传输层身份串：catch-all 时是 `t.Name + " (catch-all)"`，否则就是 `t.Name`。本仓库内没有生产调用点（注释说它是给日志用的），只有 `internal/upstream/registry_test.go` 断言两种形态。

- **HasCapabilities` (internal/upstream/registry.go)**
签名：`func (t *Target) HasCapabilities(required []string) bool`
- 作用：语义是 **"required 里的每一个标签都被本后端声明"**（AND，不是 OR）；只读，依赖 `Target` 不可变，每次调用分配一个小 set，路径上无锁。
- 规则一：`len(required) == 0` **一律为真**。**【源码注释观点】** 绝大多数请求（普通聊天）不提任何能力要求，仅因为后端什么都没声明就让它们失败，会把一个本来能用的网关下线。
- 规则二：`required` 非空而 `len(t.capabilities) == 0` → **假**。**【源码注释观点】**"我们不知道"不能被当作"我们支持它"：把一次工具调用请求静默发给一个会丢掉 tools 的后端，会得到一个看起来合理、但没有任何工具调用的答案，这比一个 400 难查得多。
- 匹配与调用：先把 `t.capabilities` 摊成一个 set，再对每个 required 项做 `strings.ToLower(strings.TrimSpace(c))` 后查 set——**要求侧在匹配时归一化，声明侧在构造时归一化**；构造期已丢弃空标签，所以拼写为空白的要求项必然失配。调用方是 `(*Router).eligible` (internal/router/router.go) 与 `internal/upstream/registry_test.go`。

- **ServesModel` (internal/upstream/registry.go)**
签名：`func (t *Target) ServesModel(model string) bool`
- 作用：报告该后端是否**自称**能服务这个模型（大小写不敏感）；catch-all 后端服务一切。
- 分支：先 `strings.ToLower(strings.TrimSpace(model))`；`model == ""` 直接为真（空模型名交给上游去拒绝，而不是在这里猜）；然后遍历 `t.patterns`，`p == "/"` 或 `strings.ToLower(p) == model` 即为真。
- 状态与调用：只读，`patterns` 构造后不可变，故无锁；`p == "/"` 未先归一化但小写化对 `"/"` 无影响，且构造期已跳过空 pattern，所以不存在空串误匹配。调用方是 `(*Router).eligible`、`(*Router).fallbackFor` (internal/router/router.go) 与 `internal/upstream/registry_test.go`。

- **Registry` (internal/upstream/registry.go)**
签名：`type Registry struct { mu sync.RWMutex; targets []*Target; byName map[string]*Target; byModel map[string]*Target; catchAll []*Target }`
- 作用：把入站请求解析到后端。
- 并发：`mu` 是 `sync.RWMutex`，所有访问器都在 `RLock` 下工作。**本仓库里 `New` 返回之后没有任何写入者**，所以这把锁目前是纯读锁——它的价值在于给将来的热重载/健康驱动变更留一个位置，并让 `TestRegistryIsSafeForConcurrentReaders` (internal/upstream/registry_test.go) 在 `-race` 下真的能跑到并发路径。
- 不变式：构造完成后四个字段都不再变；大小写归一化只发生在构造期与 `Resolve` 里。
- **`mu sync.RWMutex`**：保护上述四个字段；`Resolve`/`Target`/`Targets`/`Names`/`ModelIndex`/`CloseIdleConnections` 全部 `RLock`。
- **`targets []*Target`**：配置顺序；`Names` 与 `Targets` 都从它派生，顺序即确定性来源。
- **`byName map[string]*Target`**：显式名索引，键是**原样**的 `Name`（大小写敏感）；重名时后写者覆盖索引，但两个 target 都留在 `targets` 里（本函数不做重名检测，交给 `config.Validate`——见"未能确认"）。
- **`byModel map[string]*Target`**：小写模型名 → 后端；**先写者胜**。
- **`catchAll []*Target`**：按配置顺序保存 catch-all 后端；`Resolve` 只读 `catchAll[0]`（第一个），所以这里的多项顺序就是"配置顺序仲裁"。

- **ErrNoTarget` (internal/upstream/registry.go)**
签名：`var ErrNoTarget = errors.New("no upstream is configured for this model")`
- 作用：无法为某个模型找到后端时的哨兵错误；两条报错都用 `%w` 包装它，所以调用方可以 `errors.Is(err, ErrNoTarget)` 而不必匹配字符串。
- 调用：`(*Proxy).plan` 的 M0 回退路径 (internal/gateway/proxy.go)。

- **New` (internal/upstream/registry.go)**
签名：`func New(cfg *config.Config) (*Registry, error)`
- 作用：从**已校验**的配置构造 `Registry`，并在这里一次性建好每个后端的 transport；构造完成后全部只读。
- 错误路径：先 `cfg.Validate()`，出错就 `return nil, err`（原样上抛，不包装）。
- 为什么 transport 在这里建 **【源码注释观点】**：创建 transport 只做这一次，而不是每请求一次。**"每个请求新建一个 `http.Transport`"是最常见的反向代理性能 bug**：它废掉连接池、泄漏空闲连接、并且每个在途请求烧掉一个文件描述符。
- BaseURL 为什么 `TrimRight("/")` **【源码注释观点】**：proxy 把出站 URL 拼成 `BaseURL + r.URL.Path`，所以配置里一个尾部斜杠就会造出 `"//v1/chat/completions"`，于是每个请求都在 provider 那里 404。在构造期修掉，意味着请求路径上没有任何字符串手术。
- 构造顺序：`for i := range cfg.Upstreams` 取 `uc := cfg.Upstreams[i]`；先建 `Target`（含 `Transport: newTransport(cfg.Server)` 与 `priority`/`weight`/`tier` 直拷）；再归一化 `capabilities`（小写化 + 去空 + 保序）；然后 `targets` 追加、`byName[t.Name] = t`；最后遍历 `uc.Models`。
- 模型列表分派：`m == "/"` → 置 `catchAll`、把 `"/"` 追加进 `patterns`、把 target 追加进 `r.catchAll`（**注意 catch-all 不进 `byModel`**，所以它永远不会"抢走"某个精确模型名）；`m != ""` → 追加进 `patterns`，并以 `strings.ToLower(m)` 为键做**先写者胜**登记。
- 先写者胜的理由 **【源码注释观点】**：更早显式声明了某个模型的后端保留它，于是**配置顺序就是仲裁者**，并且**后面再加一个 catch-all 后端也不会静默抢走流量**。
- 调用：`internal/server/server.go` 的启动装配、`internal/gateway/failover_test.go`、`internal/router/router_test.go`、`cmd/loadtest`、`cmd/verify-m4`。

- **newTransport` (internal/upstream/registry.go)**
签名：`func newTransport(sc config.ServerConfig) *http.Transport`
- 作用：建一个为流式 LLM 流量调过的 transport，每个后端一个；纯构造、无副作用，唯一调用点是 `New`。
- `Proxy`：`http.ProxyFromEnvironment`——所以 `HTTPS_PROXY` 之类的环境变量生效。
- `DialContext`：`(&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext`。**【源码注释观点】** 10 秒对数据中心太长、对公网太短，是刻意的中间值，让一个死掉的 provider 不至于吃掉客户端的整个超时。
- `MaxIdleConns: 512`；`MaxIdleConnsPerHost: sc.MaxIdleConnsPerHost`。**【源码注释观点】** 并发下真正要紧的是 `MaxIdleConnsPerHost`：Go 默认的 2 会让大多数并行请求都重新做一次 TCP（外加 TLS）握手，而对一个 LLM 网关来说这**直接体现在首字延迟上**。该值来自配置 `config.ServerConfig.MaxIdleConnsPerHost`，`config.Validate` 会把非正值填成默认（`config.Defaults` 给的是 64）。
- `MaxConnsPerHost: 0`：**【源码注释观点】** 在途连接不设上限，由空闲池来约束。
- `IdleConnTimeout: 90 * time.Second`、`TLSHandshakeTimeout: 10 * time.Second`、`ExpectContinueTimeout: 1 * time.Second`。
- `ForceAttemptHTTP2: true`：即使自定义了 `DialContext` 也仍然尝试 HTTP/2（Go 在自定义 dialer 时默认会关掉它）。
- `DisableKeepAlives: sc.DisableKeepAlives`：配置闸门，关掉连接复用（对调试"复用连接导致的怪现象"有用）。
- **故意不设 `ResponseHeaderTimeout`** **【源码注释观点】**：一次长生成完全可能花上几分钟才产出第一个字节，真正的截止时间由 handler 通过 **context** 持有。后果值得记住：一个"接受连接但永不回响应头"的 provider 在这层没有截止时间，能兜住它的只有请求 context——这正是 `relayStream` 的心跳与上下文取消重要的原因。

- **Resolve` (internal/upstream/registry.go)**
签名：`func (r *Registry) Resolve(model, explicit string) (*Target, error)`
- 作用：返回应该服务 `model` 的后端。整段 `RLock`/`defer RUnlock`，只读；返回的是**活的** `*Target`（与 registry 共享同一个 transport 指针），不是拷贝。
- 查找顺序，高优先在前：
  1. **显式名**：`explicit != ""` 时查 `byName`；命中即返回；**未命中不回落**，直接返回 `fmt.Errorf("%w: unknown upstream %q", ErrNoTarget, explicit)`——钉住流量是权威指令，钉到一个不存在的名字是错误，不是静默回退的理由。
  2. **精确模型匹配**：查 `r.byModel[strings.ToLower(strings.TrimSpace(model))]`。
  3. **第一个 catch-all**：`len(r.catchAll) > 0` 时返回 `r.catchAll[0]`（配置顺序里的第一个，不做负载均衡——那是 M1 router 的事）。
  4. **唯一后端**：`len(r.targets) == 1` 时返回它，**并改写调用方自编的模型名**。
- 全部落空：`fmt.Errorf("%w: model %q", ErrNoTarget, model)`——注意错误里放的是**调用方原始的大小写**，不是归一化后的键。
- 为什么规则 4 **【源码注释观点】**：这是单后端的顺滑场景——本地 vLLM 或 Ollama 只服务一个量化版本，而调用方（一个 agent 框架）被告知的是一个网关从没听过的模型别名。拒绝这个请求会让网关在**最常见的本地部署**里毫无用处，所以一个无歧义的单后端吸收任意模型名，由 proxy 把它改写成该后端真正服务的那个模型。
- 为什么规则 4 必须**最后** **【源码注释观点】**：两个或更多后端时没有无歧义的答案，猜一个会把 agent 的请求静默发到错误的模型上——一个**任何指标都不会暴露的正确性故障**。
- 为什么大小写不敏感 **【源码注释观点】**：provider 对大小写并不一致，而一次大小写差异把请求静默路由到另一个（或没有）后端，是比误匹配严重得多的失败。
- 改写的分工（重要更正）：`Resolve` **只挑后端、不改模型名**。改写由调用方完成——M0 路径上 `(*Proxy).plan` 拿到 target 后再调 `(*Proxy).resolveModelName`，那里对"模型列表恰好只有 `{"/"}`"的后端原样透传调用方名字，否则挑该后端第一个具体模型名；有 router 时走 `(*Router).Plan`，`Resolve` 完全不被调用。
- 能力与健康：`Resolve` **不检查** capabilities、健康或熔断，这些全在 `(*Router).eligible` 与 breaker 里；M0 直连路径因此不做能力过滤。调用方是 `(*Proxy).plan` 的 M0 回退路径 (internal/gateway/proxy.go)、`internal/upstream/registry_test.go`、`ExampleRegistry_Resolve`。

- **Registry` 的其余访问器（五个独立条目，均在 internal/upstream/registry.go）**
- **`func (r *Registry) Target(name string) (*Target, bool)`**：按名取后端；`RLock` 下查 `byName`，未命中返回 `(nil, false)`（测试专门断言"未命中要干净地返回 nil,false"而不是一个零值目标）；调用方是 `internal/server/server.go` 的 `/admin/upstreams` 与上游控制端点、`internal/router/router.go` 的显式名路径、`cmd/loadtest`。
- **`func (r *Registry) Targets() []*Target`**：按配置顺序返回全部后端；**切片是新分配的、按值拷贝，元素仍是活的 `*Target`**（测试断言调用方改自己的切片不影响 registry）。为什么给活的指针 **【源码注释观点】**：调用方拿到活的 `*Target` 而不是拷贝，因为 `Target` 携带那个共享的、按后端建的 `http.Transport`，而连接池正是靠它工作的；每请求拷一个 `Target` 会拷走 transport 指针，却废掉了"只有一个构造点"的价值。调用方是 `(*Proxy).sortedTargets` (internal/gateway/capabilities.go)。
- **`func (r *Registry) Names() []string`**：按配置顺序返回所有后端名，新切片、与 registry 解耦。为什么顺序重要 **【源码注释观点】**：这个顺序也是让**模型解析具备确定性**的顺序保证。调用方是 `internal/server/server.go` 的日志与内省端点、`breaker.NewGroup(cfg.Health, registry.Names())`。
- **`func (r *Registry) ModelIndex() map[string]string`**：给 `/admin/upstreams` 内省的"模型 → 后端名"映射；实现上先把键收集起来 `sort.Strings(keys)` 再填结果——map 本身无序，但**先排序再插入**让 JSON 序列化顺序稳定、可 diff。调用方是 `internal/server/server.go` 的 `"model_index"` 字段。
- **`func (r *Registry) CloseIdleConnections()`**：释放每个后端的池化连接。**【源码注释观点】** 服务器在关机时调用它，好让进程**立刻退出**，而不是等空闲的 keep-alive socket 自然消失。实现是 `RLock`（不是写锁——它不碰 registry 自己的状态），对每个 target 做类型断言 `t.Transport.(interface{ CloseIdleConnections() })`，成立才调用；用断言而不是直接调 `*http.Transport` 的方法，是因为字段类型是 `http.RoundTripper`，而测试里的替身 transport 不一定实现它。幂等：对已排空的池再调一次不会 panic（测试覆盖）。调用方是 `internal/server/server.go` 的关机路径、`cmd/loadtest` 的 `defer`、测试里的 `t.Cleanup`。

## 11. `forwardResponseHeaders` (internal/gateway/proxy.go)

签名：`func forwardResponseHeaders(dst, src http.Header)`
- 作用：把上游响应头复制给客户端，然后删掉三个由 Go 服务器重新分帧的头；无返回值、无锁（只操作两个 `http.Header`）。
- 实现：先 `copyHeaders(dst, src, false)`；再无条件 `dst.Del("Content-Length")`、`dst.Del("Content-Encoding")`、`dst.Del("Transfer-Encoding")`。
- 为什么删这三个 **【源码注释观点】**：响应体由 Go 的 server **重新分帧**，所以上游的分帧头必须走——一个过期的 `Content-Length` 会把流**截断**，而转发过来的 `Content-Encoding` 会让一个已经被解压过的体**再解一次**。
- 增量在哪：三个里 `Transfer-Encoding` 其实**已经被 `copyHeaders` 按 hop-by-hop 丢掉了**（见 `hopByHopHeaders`），显式 `Del` 是重复保险；`Content-Length` 与 `Content-Encoding` 不在那张表里，它们才是这个函数真正多做的事。
- `copyHeaders(dst, src, false)` 丢掉的：静态 hop-by-hop 集合（`Connection`、`Proxy-Connection`、`Keep-Alive`、`Proxy-Authenticate`、`Proxy-Authorization`、`Te`、`Trailer`、`Transfer-Encoding`、`Upgrade`），以及 `Connection` 头**自己列出的**任意头名（`connectionListed`，RFC 7230 第 6.1 节里最微妙的那一半）。**【源码注释观点】** 盲目复制 `Connection` 会让客户端把任意头（包括 `Authorization`）标成 hop-by-hop，在下一跳被剥掉或重复。
- 第三个参数是 `false`：即 `dropGatewayHeaders` 关闭，所以响应方向上 **InferGate 自己的 `x-infergate-*` 控制头与 `Idempotency-Key` 会被照抄出去**（请求方向上 `(*Proxy).buildRequest` 传的是 `true`，会把它们挡在上游之外）。值的风格也是刻意的：`copyHeaders` 原样搬运 value 切片，好让 provider 的 `RateLimit-*`、request id、`Retry-After`、厂商扩展**逐字节**到达客户端。
- 调用：`(*Proxy).serveAttempts`（重试用尽后把**最后一个**失败响应的头交给客户端）与 `(*Proxy).attemptUpstream`（4xx 原样透传路径、以及成功路径在进入 `relayStream`/`copyWhole` 之前）。三处之后都会立刻用 `w.Header().Set` 覆写 `X-InferGate-Upstream-Name`/`-Attempt`/`-Request-Id`/`-Tried`，所以客户端总能看出是谁服务的、有没有故障转移。

## 12. 横切主题

- **两条路径、两套记账实现**：流式靠 `Inspector.Observe` 跨帧累加，非流式靠 `UsageFromResponse` 解一个完整 JSON 体；共同原则是**provider 自报的模型名即定价权威**（`rec.model` 只在非空时被覆盖）。但代码是两份，且 `UsageFromResponse` 目前**无人调用**（gateway 用的是私有孪生 `usageFromJSON`），这是本册唯一发现的实现偏差。
- **"不知道"不等于"支持"**：`HasCapabilities` 对空要求一律放行、对"后端没声明任何能力"的非空要求一律拒绝；`ServesModel` 对**空模型名**返回真（交给上游拒绝）而对不认识的模型名返回假。前者防止把工具调用静默送进会丢 tools 的后端，后者防止把拼错的名字匹配到错误的后端。
- **顺序即仲裁**：`byModel` 的先写者胜、`catchAll[0]` 取第一个、`targets` 与 `Names` 保持配置顺序——三处合起来让 M0 的路由完全确定、可预测、可 diff。catch-all 不进 `byModel` 是这套设计的关键细节：它只做第三级兜底，永远不覆盖精确声明。
- **一个后端一个 transport，构造一次**：这把"最常见的反向代理性能 bug"提前挡掉；`MaxIdleConnsPerHost` 直接决定首字延迟，`ResponseHeaderTimeout` 的缺席则把超时责任整体交给请求 context。
- **借出内部引用是有意的**：`Inspector.ToolCalls`、`Target.ModelPatterns`、`Target.Capabilities`、`Registry.Targets` 都返回内部数据（切片或 map，元素是活指针），换掉逐请求分配；代价是这些 API 全部要求调用方只读，且 `Inspector` 的一切都不允许跨 goroutine 使用。

## 13. 未能确认

- `config.Validate` 是否**拒绝**重名后端：`New` 里 `byName[t.Name] = t` 是后写者覆盖，`targets` 却两个都留。若校验层不拦，重名配置会得到一个"名字指向后一个、`targets` 里有前一个"的 registry。未读 `internal/config/config.go` 的对应校验分支，故不下结论。同一个后端的模型列表里重复写 `"/"` 会把该 target 追加进 `r.catchAll` 两次（`Resolve` 只读 `catchAll[0]` 所以行为不变）；重复写同一个具体模型名会让 `ModelPatterns` 出现重复项（测试断言的是顺序与拼写，不是去重）。
- `Observe` 在 `index` 缺失时把索引取 0：源码没有注释说明这是刻意容忍还是已知退化，多工具调用的 provider 若省略 index 会全部并进 0 号槽。
- `StreamError.Code`/`Param` 用 `any` 的理由是我从"定死类型会让整帧 Unmarshal 失败"推出的**推断**，源码无注释；`NewInspector` 预建 map、`Observe` 返回前 `bytes.Clone` 的理由同样是推断。
- `Content-Encoding` 的删除存在一个我从代码推出的边界风险：`buildRequest` 用 `copyHeaders(..., true)` 会把客户端的 `Accept-Encoding` 透传给上游，而 transport 没有设 `DisableCompression`、也没有任何地方剥 `Accept-Encoding`。按 Go 的 `net/http` 行为，**当请求自带 `Accept-Encoding` 时 Transport 不会自动解压**，此时上游回的 gzip 体被剥掉 `Content-Encoding` 后会以裸 gzip 字节到达客户端。我未运行验证，故只记为风险。
- `Inspector.ToolCalls`/`FinishReason`/`Tags`/`Usage.Empty`/`UsageFromResponse` 在仓库内没有生产调用点这一条，是用 grep 扫 `*.go` 得到的，未覆盖生成代码与 `tools/` 目录下的非 Go 调用方。

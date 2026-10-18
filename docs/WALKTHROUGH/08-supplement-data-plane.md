# 第 08 册 · 数据面补遗：embed、evalset、mockbackend

## 1. 本册范围与读法

本册补齐三个此前只被别的分册顺带提过、没有逐符号走读的包：

- `internal/embed/embed.go` 与 `internal/embed/embed_test.go`：把文本变成向量的两种实现（离线 feature hashing 与 OpenAI 兼容的 HTTP 端点）、向量线格式、`Fallback` 降级包装，以及 `Scored`/`Rank` 小工具。
- `internal/evalset/evalset.go` 与 `internal/evalset/evalset_test.go`：26 对人工标注语料、阈值扫描、推荐函数与 19 点阈值网格。
- `internal/mockbackend/mockbackend.go` 与 `internal/mockbackend/mockbackend_test.go`：进程内假上游。**这部分已被 05 册第 43 至 46 节较完整地覆盖**，本册只补它没讲到的部分，并在第 33 节逐条说明 05 册已覆盖了什么、这里补了什么。

格式约定（全册一致）：所有锚点写成 `` `符号名 (internal/embed/embed.go)` `` 的形式，不写行号；源码注释里解释「为什么」的句子照译并标注「**源码注释观点**」，那不代表本册作者的推断；凡本册没有在源码里找到确证的判断，一律在第 46 节列出，绝不编造。

本册写作的硬约束（来自仓库自检测试的读法）：反引号里出现的包级符号名必须真实存在；反引号里写出的测试名必须在树里有对应的函数定义（占位式的假测试名会被判红）；引用的仓库路径必须真实存在于仓库。

## 2. internal/embed：包注释与 `Embedder` 接口

包注释（**源码注释观点**）：`internal/embed` 把文本变成向量给语义缓存用；存在两个实现，理由是**环境**而不是架构——语义缓存只有在「同义改写落得很近」时才有用，但网关同时必须在无网络、无 embedding 服务的机器上跑（本机既没有 API key 也没有本地模型服务）；所以默认是一个确定性的离线 embedder，而接一个真的 embedding 端点**是配置变更而不是代码变更**。两种实现的分工：`HashingEmbedder` 对词与词 bigram 做 feature hashing，无网络、无模型文件、每个进程输出一致；`HTTPEmbedder` 向任何 OpenAI 兼容端点 POST `/embeddings`（vLLM、Ollama、OpenAI、本地 sentence-transformers 服务）。

包注释还交代了这个离线 embedder 的诚实边界（**源码注释观点**）：它度量的是**词法**重叠，能抓住换序、插入、删除与标点变化，抓不住同义词替换；而这个边界是「量过」而不是「嘴上说说」的——`scripts/measure-m2.ps1` 在一个标注过的释义集上扫相似度阈值，同时记录命中率与误命中率，这才是阈值成为一个量出来的数字而不是猜出来的数字的原因。

- `Embedder (internal/embed/embed.go)` —— `type Embedder interface { Embed(ctx context.Context, texts []string) ([][]float32, error); Dims() int; Name() string }`。作用：把文本转成单位长度向量。契约（**源码注释观点**）：实现必须**每个输入文本返回一个向量、顺序相同**，且所有向量长度一致；调用方两头都依赖——缓存把向量 i 与文本 i 配对，长度不一致是一个静默的打分 bug。
- 接口方法 `Embed` 的注释（**源码注释观点**）：必须尊重 ctx 取消；一次 embedding 调用在客户端请求的关键路径上，一个卡住的 embedder 不能让请求超过自己的截止时间。
- 接口方法 `Dims` 的注释：返回该 embedder 产出的向量长度。
- 接口方法 `Name` 的注释（**源码注释观点**）：在日志与 `/admin/cache` 里标识 embedder，不涉及任何安全敏感信息，存在的意义是让运维能看出是哪个 embedder 填满了缓存。
- 调用者：`internal/cache/memory.go` 与 `internal/cache/redis.go` 通过 `embed.Cosine`/`embed.IsZero` 消费向量；`internal/server/cache.go` 构造具体实现并把 `Embedder` 交给缓存层；`cmd/measure-m2/main.go` 用 `-dims`/`-http` 决定装哪一个。

## 3. internal/embed：`HashingEmbedder` 与 `DefaultHashingDims`

- `HashingEmbedder (internal/embed/embed.go)` —— `type HashingEmbedder struct { dims int }`。作用：离线默认实现。算法是经典的「带符号 feature hashing」，类注释给了四步（**源码注释观点**）：1）分词——小写、按「不是字母也不是数字」切分，并额外为 CJK 串产字符 bigram（中文没有空格，否则整句会是一个 token，任意两句中文互相正交）；2）特征——词 unigram 加词 bigram，使词序在短语尺度上有意义（`dog bites man` 不等于 `man bites dog`），又不至于把向量变成纯 n-gram 汤；3）用 FNV-1a 把每个特征哈希到桶，累加带符号的 sublinear 权重 `1 + ln tf`，符号取第二个哈希位——没有符号的话碰撞只会**增加**相似度，从而把无关文本之间的分数吹高；4）L2 归一化，使余弦相似度就是点积，长文档不会自动比短文档「更接近一切」。
- 并发：`dims` 在构造时写入后只被读，`vector`/`Embed` 只用局部变量，没有锁也没有全局状态——因此同一个 `HashingEmbedder` 可以被并发调用。
- `DefaultHashingDims (internal/embed/embed.go)` —— `const DefaultHashingDims = 512`。注释（**源码注释观点**）：512 是一个带**实测代价**的折中——一条缓存条目的向量以 base64 float32 存储是 2 KiB（512 维），编码后 2.7 KiB；默认每 scope 256 条，一次查找大致要从存储里读 700 KiB，对本地 Redis 没问题，更大规模就需要真正的向量索引；压到 256 维能减半，代价是区分度下降。

## 4. internal/embed：`NewHashingEmbedder`、`Dims`、`Name`

- `NewHashingEmbedder (internal/embed/embed.go)` —— `func NewHashingEmbedder(dims int) *HashingEmbedder`。行为：`if dims <= 0 { dims = DefaultHashingDims }`，返回 `&HashingEmbedder{dims: dims}`。错误路径：无 error 返回。注释（**源码注释观点**）：0 维选默认值；负宽度被钳到默认值，因为「零或负的模数在累加器里是 panic，而不是调用方能恢复的配置错误」。
- `(HashingEmbedder) Dims (internal/embed/embed.go)` —— `func (h *HashingEmbedder) Dims() int`。行为：一行返回 `h.dims`。被谁调用：`cmd/measure-m2/main.go`（打印）、`internal/server/cache.go`（构造缓存时）、`Fallback.Dims`。
- `(HashingEmbedder) Name (internal/embed/embed.go)` —— `func (h *HashingEmbedder) Name() string`。行为：`fmt.Sprintf("hashing-%d", h.dims)`，例如 128 维是 `hashing-128`。被谁调用：`NewHTTPEmbedder` 不涉及；日志与 `/admin/cache`、`cmd/measure-m2/main.go` 的报表。

## 5. internal/embed：`Embed` 与 `vector`

- `(HashingEmbedder) Embed (internal/embed/embed.go)` —— `func (h *HashingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error)`。行为：`out := make([][]float32, len(texts))`，逐个 `out[i] = h.vector(t)`，返回 `out, nil`。错误路径：注释明说它**不可能失败**（无 IO），**永远不返回非 nil 的 error**，但保留签名以便调用方不需要类型开关（**源码注释观点**）。上下文参数被丢弃（形参名是 `_`），所以它不响应取消，也不需要——纯 CPU、有界。
- `(HashingEmbedder) vector (internal/embed/embed.go)` —— `func (h *HashingEmbedder) vector(text string) []float32`。行为：建 `v := make([]float32, h.dims)` 与 `counts := map[string]int{}`；对 `Features(text)` 的每个 token 计数；然后遍历 counts，对每个 `tok` 取 `idx, sign := hashFeature(tok, h.dims)`，算 `w := float32(1 + math.Log(float64(n)))`，执行 `v[idx] += sign * w`；最后 `Normalize(v)` 并返回 v。关键机制：**sublinear tf** 的注释（**源码注释观点**）——重复十次的词并不比一次重要十倍，否则一段灌水或重复的提示会主导向量、从自己的同义改写旁边漂走。读写状态：只读写局部变量。错误路径：无。
- 一个本册的观察（非源码注释）：因为计数按 token 去重，每个 token 只贡献一项，但**不同 token 哈希到同一桶**时 `v[idx] +=` 的先后取决于 Go map 的随机迭代顺序，float32 加法不满足结合律；理论上两个进程仍可能得到不同向量。这条推论未能用测试确认（工具链不可用），列入第 46 节。

## 6. internal/embed：`Features`、`tokenize`、`isCJK`

- `Features (internal/embed/embed.go)` —— `func Features(text string) []string`。行为：`words := tokenize(text)`；`feats := make([]string, 0, len(words)*2)`；先 append 全部 unigram，再对 `i+1 < len(words)` 追加 `words[i]+" "+words[i+1]`；返回 feats。注释说它**有意导出**（**源码注释观点**）：让语义缓存正确的那些阈值只有在分词可检视时才站得住脚——`/admin/cache/lookup` 会显示一条探针提示的特征，于是一个意外的相似度可以从 token 表上读出来而不是靠猜。被谁调用：`(HashingEmbedder) vector`、`internal/server/cache.go`（admin 查找端点）、`internal/embed/embed_test.go` 里的 `TestEmptyTextHasNoFeaturesAndNoNaNs` 与 `TestFeaturesIncludeUnigramsAndBigrams`。
- `tokenize (internal/embed/embed.go)` —— `func tokenize(text string) []string`。行为：用一个 `cur []rune` 缓冲与一个 `flush` 闭包；对 `strings.ToLower(text)` 的每个 rune，`unicode.IsLetter(r) || unicode.IsDigit(r)` 则累积，否则 `flush()`；循环结束后再 `flush()` 一次。`flush` 的行为：空缓冲直接返回；取出 token 后把 `cur` 截回零长；若 `isCJK(tok)` 则把它当作字符 bigram 袋——注释给的例子是 `重置我的密码` → `重置/置我/我的/的密/密码`，并说明**单字符被丢弃**，因为它们太常见无法区分（的、了、是）（**源码注释观点**）；否则整个 token 作为一个词加入。错误路径：无。它不导出，因此「按文档顺序」的语义由 `Features` 转述。
- `isCJK (internal/embed/embed.go)` —— `func isCJK(s string) bool`。行为：空串返回 false；否则遍历 rune，只要命中 `unicode.Han`、`unicode.Hiragana`、`unicode.Katakana`、`unicode.Hangul` 之一就返回 true；全部不命中返回 false。注意它是「含**任一** CJK 字符即真」，不是「整串都是 CJK」。被谁调用：`tokenize` 的 `flush`。

## 7. internal/embed：`hashFeature`、`fnvHash` 与 `hash/maphash` 的取舍

- `hashFeature (internal/embed/embed.go)` —— `func hashFeature(feature string, dims int) (int, float32)`。行为：`idx := fnvHash("i:" + feature)`；`sign := fnvHash("s:" + feature)`；`i := int(idx % uint64(dims))`；`if sign&1 == 0 { return i, 1 }`，否则 `return i, -1`。注释（**源码注释观点**）：用两个独立哈希而不是一个——把同一个哈希复用于下标与符号会让二者相关，于是一次碰撞会系统性地指向同一个方向；符号取自对另一个前缀做 FNV-1a 的**最高位**，这对一个缓存启发式足够，且保持代码可读。`dims` 必须为正，构造函数已经保证；源码在这里带了一条 `#nosec G115` 说明取模结果非负、转换不会溢出。被谁调用：`(HashingEmbedder) vector`。
- `fnvHash (internal/embed/embed.go)` —— `func fnvHash(s string) uint64`。行为：`h := fnv.New64a()`；`_, _ = h.Write([]byte(s))`（注释说 `hash.Hash.Write` 对 fnv 永不返回错误，输入是 string）；返回 `h.Sum64()`。被谁调用：`hashFeature` 两次。
- **为什么不用 `hash/maphash`（源码注释观点，本册在本包里最重要的一条）**：类注释明确写「FNV-1a 是刻意的选择，而不是 `hash/maphash`：maphash 是**每进程随机播种**的，于是两个网关副本会对同一段文本算出不同的向量，共享 Redis 缓存就永远不可能跨副本命中。哈希必须在进程之间、在重启之后都稳定，这正是一个缓存键需要的。」这条理由由 `TestVectorIsIndependentOfProcessState` 钉住：它用两个 `NewHashingEmbedder(64)` 实例代表两个副本，要求 `EncodeVector` 结果逐字节相等。

## 8. internal/embed：`Normalize`

- `Normalize (internal/embed/embed.go)` —— `func Normalize(v []float32) { var sum float64; ... }`。行为：用 float64 累加 `sum += float64(x)*float64(x)`；`if sum == 0 { return }`；否则 `inv := float32(1 / math.Sqrt(sum))`，对每个元素原地 `v[i] *= inv`。读写状态：**原地修改参数切片**。错误路径：无。
- 注释（**源码注释观点**）：零向量原样保留——除以它自己的范数会产生 NaN，而 NaN 与任何阈值的比较都是 false，那会**静默地**把一次缓存命中变成未命中。被谁调用：`(HashingEmbedder) vector`、`(HTTPEmbedder) Embed`（把 provider 返回的向量也归一化）、`internal/embed/embed_test.go` 里的 `TestNormalizeHandlesZeroVector` 显式调用。

## 9. internal/embed：`Cosine`

- `Cosine (internal/embed/embed.go)` —— `func Cosine(a, b []float32) float64`。行为：先 `if len(a) != len(b) || len(a) == 0 { return 0 }`；然后用 **float64** 累加三个量 `dot`、`na`、`nb`（`dot += float64(a[i])*float64(b[i])` 等）；若 `na == 0 || nb == 0` 返回 0；否则返回 `dot / (math.Sqrt(na) * math.Sqrt(nb))`。错误路径：无 error，长度不等或任一侧零范数一律返回 0。
- 注释（**源码注释观点**）：单位长度输入让它退化为点积，但范数**仍然计算**，因此对未归一化的向量也正确（比如旧版本存下的条目，或者一个本身不做归一化的 HTTP embedder）；长度不等返回 0，是因为 0 低于任何可用阈值，维度变化就变成缓存未命中而不是错误命中。**注意实际实现用的是 float64 累加而不是整数或 float32 累加**，这一点对长向量很重要。
- 被谁调用：`internal/cache/memory.go`（`sim := embed.Cosine(vec, e.Vector)`）、`internal/cache/redis.go`（`sim := embed.Cosine(vec, v)`）、`internal/evalset/evalset.go` 的 `Measure`、以及 embed 自己的多个测试。

## 10. internal/embed：`IsZero`

- `IsZero (internal/embed/embed.go)` —— `func IsZero(v []float32) bool`。行为：`len(v) == 0` 返回 true；否则任何 `x != 0` 立即返回 false；全零返回 true。错误路径：无。
- 注释（**源码注释观点**）：零查询向量不是「与什么都不像」，而是**无意义**，算术也同意这一点——`Cosine` 对它返回 0，于是面对一个恰好为 0 的阈值，每一条已存条目都会合格；一个没能算出 embedding 的调用方就会拿回一条任意的条目，那是**打扮成缓存命中的错误答案**。因此 store 直接拒绝零查询向量并报告无匹配，那与冷缓存是同一个结果。
- 被谁调用：`internal/cache/memory.go`、`internal/cache/redis.go`，两处注释都回指本条理由。

## 11. internal/embed：`EncodeVector` 与 `DecodeVector`

- `EncodeVector (internal/embed/embed.go)` —— `func EncodeVector(v []float32) string`。行为：`buf := make([]byte, 4*len(v))`；逐元素 `binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))`；返回 `base64.StdEncoding.EncodeToString(buf)`。错误路径：无。被谁调用：`internal/cache/redis.go`（`HSET` 时写入 `s.vecKey(scope)`），测试用它比较跨进程一致性。
- `DecodeVector (internal/embed/embed.go)` —— `func DecodeVector(s string) ([]float32, error)`。行为：`s == ""` 返回 `nil, nil`；`base64.StdEncoding.DecodeString` 失败返回错误串原文 `embed: decode vector: %w`；`len(raw)%4 != 0` 返回错误串原文 `embed: decode vector: %d bytes is not a whole number of float32`；否则逐 4 字节 `math.Float32frombits(binary.LittleEndian.Uint32(...))` 还原并返回。被谁调用：`internal/cache/redis.go`（读回条目时）。
- 线格式的「为什么」（**源码注释观点**）：把向量存成 JSON 数字数组是显而易见的第一选择，但它**错两次**——体积大约翻倍（每个 float 最多 8 个字符），而且 text→float64→float32 的往返有损，读回来的向量「接近但不等于」写进去的那条；对一个整个工作就是比较向量的缓存来说，「接近」是一个等着发生的 bug。原始 float32 可以逐位往返。

## 12. internal/embed：`HTTPOptions`、`DefaultMaxInputChars`、`HTTPEmbedder`

- `HTTPOptions (internal/embed/embed.go)` —— `type HTTPOptions struct { BaseURL string; Model string; APIKey string; Timeout time.Duration; MaxInputChars int; Client *http.Client }`。逐字段（注释原文的翻译）：
  - `BaseURL` —— API 根，例如 `http://127.0.0.1:8000/v1`；embedder 会追加 `/embeddings`。
  - `Model` —— 请求体里发送的 embedding 模型名。
  - `APIKey` —— 非空时以 `Authorization: Bearer <key>` 发送；对本地 vLLM 或 Ollama，空是常态。
  - `Timeout` —— 界住一次 embedding HTTP 调用的时长；embedding 在请求路径上，所以它必须**小**：一个慢的 embedder 比一次缓存未命中更糟。**源码默认值 5 秒**（在 `NewHTTPEmbedder` 里，`Timeout <= 0` 时填 `5 * time.Second`）。
  - `MaxInputChars` —— 发送前截断每段文本；embedding 端点有自己的 token 上限，而 embedder 返回 413 会变成一个失败的客户端请求；截断把缓存降级为未命中。0 选 `DefaultMaxInputChars`。
  - `Client` —— 可选；不提供时创建一个带合理 transport 的 client。
- `DefaultMaxInputChars (internal/embed/embed.go)` —— `const DefaultMaxInputChars = 8000`。注释：界住单段被嵌入的文本。
- `HTTPEmbedder (internal/embed/embed.go)` —— `type HTTPEmbedder struct { opts HTTPOptions; client *http.Client; dims int }`。作用：调用 OpenAI 兼容的 `/embeddings`。并发：`opts`/`client` 构造后只读；`dims` **没有锁也没有原子量保护**——`Embed` 每次成功调用都写 `e.dims = len(ordered[0])`，`Dims()` 无同步地读它，所以共享一个 `*HTTPEmbedder` 的并发请求在竞态检测器下是一个数据竞争（本册观察，见第 46 节）。

## 13. internal/embed：`NewHTTPEmbedder`、`Dims`、`Name`

- `NewHTTPEmbedder (internal/embed/embed.go)` —— `func NewHTTPEmbedder(opts HTTPOptions) (*HTTPEmbedder, error)`。校验与兜底顺序：`strings.TrimSpace(opts.BaseURL) == ""` → 错误串原文 `embed: http embedder needs a base_url`；`strings.TrimSpace(opts.Model) == ""` → 错误串原文 `embed: http embedder needs a model`；`opts.Timeout <= 0` → 5s；`opts.MaxInputChars <= 0` → 8000；`c := opts.Client`，为 nil 时 `c = &http.Client{Timeout: opts.Timeout}`；返回 `&HTTPEmbedder{opts: opts, client: c}, nil`。注意：提供了 `Client` 时，构造出的 client **不带** `opts.Timeout`——但 `Embed` 里另有 `context.WithTimeout(ctx, e.opts.Timeout)`，所以超时仍然生效。被谁调用：`internal/server/cache.go`（配置里走 HTTP 时）与 `cmd/measure-m2/main.go`（`-http`）。
- `(HTTPEmbedder) Dims (internal/embed/embed.go)` —— `func (e *HTTPEmbedder) Dims() int`。行为：返回 `e.dims`。注释：**0 表示「还没观测到」**——维度在第一次调用之前是未知的，缓存如果需要固定宽度必须从第一个响应里学。
- `(HTTPEmbedder) Name (internal/embed/embed.go)` —— `func (e *HTTPEmbedder) Name() string`。行为：返回 `"http:" + e.opts.Model`，例如 `http:m`。
- 构造函数的注释（**源码注释观点**）：「`Dims` 在第一次调用之前是未知的，所以它在那之前报告 0；一个需要固定宽度的缓存必须从第一个响应里学到它（见 `Cache.observeDims`）。」**本册未能确认 `Cache.observeDims` 存在**——全仓库没有这个函数，见第 46 节。

## 14. internal/embed：`embedRequest` 与 `embedResponse`

- `embedRequest (internal/embed/embed.go)` —— `type embedRequest struct { Model string \`json:"model"\`; Input []string \`json:"input"\` }`。作用：POST 请求体。被谁调用：`(HTTPEmbedder) Embed` 构造它，`internal/embed/embed_test.go` 的 httptest 处理器解码它做断言。
- `embedResponse (internal/embed/embed.go)` —— `type embedResponse struct { Data []struct { Index int \`json:"index"\`; Embedding []float32 \`json:"embedding"\` } \`json:"data"\`; Model string \`json:"model"\`; Usage struct { PromptTokens int \`json:"prompt_tokens"\`; TotalTokens int \`json:"total_tokens"\` } \`json:"usage"\`; Error *struct { Message string \`json:"message"\`; Type string \`json:"type"\` } \`json:"error"\` }`。作用：响应体形状。注意 `Error` 是**指针**，所以「没有 error 字段」与「error 是 null」都能被区分；`Usage` 被解析但**从不使用**（只为了形状兼容）。被谁调用：`(HTTPEmbedder) Embed` 的 `json.Unmarshal` 目标。

## 15. internal/embed：`HTTPEmbedder.Embed` 的请求构造

- `(HTTPEmbedder) Embed (internal/embed/embed.go)` —— `func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error)`。请求侧行为，按顺序：
  1. `if len(texts) == 0 { return nil, nil }`——空输入不发请求，也不动 `dims`。
  2. `inputs[i] = truncateRunes(t, e.opts.MaxInputChars)`，逐段截断。
  3. `body, err := json.Marshal(embedRequest{Model: e.opts.Model, Input: inputs})`；失败 → 错误串原文 `embed: marshal request: %w`（实践中不可能，字段都是可序列化的）。
  4. `ctx, cancel := context.WithTimeout(ctx, e.opts.Timeout)` 加 `defer cancel()`——**配置的超时在这里落实**，与 client 自带的超时相互独立。
  5. `url := strings.TrimRight(e.opts.BaseURL, "/") + "/embeddings"`——这就是「URL 拼接 `/embeddings`」：`BaseURL` 末尾的斜杠先被去掉，所以 `http://host/v1/` 拼成 `http://host/v1/embeddings` 而不是双斜杠。
  6. `http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))`；失败 → 错误串原文 `embed: build request: %w`。
  7. `req.Header.Set("Content-Type", "application/json")`；`APIKey != ""` 时 `req.Header.Set("Authorization", "Bearer "+e.opts.APIKey)`。
  8. `resp, err := e.client.Do(req)`；失败 → 错误串原文 `embed: call %s: %w`（`%s` 是拼好的 url）；`defer func() { _ = resp.Body.Close() }()`。
- 调用者：`internal/server/cache.go` 组装的缓存层（请求路径上）、`cmd/measure-m2/main.go`（离线测量）。错误路径见下一节。

## 16. internal/embed：`HTTPEmbedder.Embed` 的响应解析与三条错误串

响应的读取、校验与重排，按顺序（错误串全部照抄原文）：

1. 读取有上限：`raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))`，失败 → `embed: read response: %w`。注释（**源码注释观点**）：必须给读取设上限——embedding 响应是一张向量表，而一个被配错、指向 chat 端点的 URL 否则会在请求路径上把几兆字节流进内存。
2. `resp.StatusCode != http.StatusOK` → `embed: %s returned %d: %s`（第三个 `%s` 是 `snippet(raw)`，即被裁剪到 200 字符的响应体）。
3. `json.Unmarshal(raw, &parsed)` 失败 → `embed: parse response: %w`。
4. `parsed.Error != nil` → `embed: provider error: %s`（`%s` 是 `parsed.Error.Message`）。注意这一步在 HTTP 状态码检查**之后**：只有「HTTP 200 但 body 里带 error 信封」的 provider 才走到这里。
5. `len(parsed.Data) != len(texts)` → **`embed: asked for %d vectors, got %d`**（第一个 `%d` 是输入条数，第二个是返回条数）。
6. 按 `index` 重排：`ordered := make([][]float32, len(texts))`；遍历 `parsed.Data`，`d.Index < 0 || d.Index >= len(ordered)` → **`embed: response index %d out of range`**；否则 `ordered[d.Index] = d.Embedding`。注释（**源码注释观点**）：API 契约说 `index` 标识位置，但有些服务器**乱序**返回，而把错误的向量与错误的文本配对会用误导性的相似度**毒化缓存**。
7. 逐位检查空洞：`len(v) == 0` → **`embed: response is missing the vector for input %d`**（`%d` 是输入下标）。这一步补的是计数检查抓不住的情况：条数对得上但某个 index 缺失或重复，重排后就会留下空槽。
8. 对每个非空向量 `Normalize(v)`——**HTTP embedder 也做 L2 归一化**，即使 provider 不归一化。
9. `e.dims = len(ordered[0])`，返回 `ordered, nil`。此处 `ordered[0]` 一定非空，因为第 7 步已经保证没有空洞。

任务点名的三条错误串就在第 5、6、7 步。调用关系：`Embed` 的 `dims` 写入是 `Dims()` 唯一的写者。

## 17. internal/embed：`truncateRunes` 与 `snippet`

- `truncateRunes (internal/embed/embed.go)` —— `func truncateRunes(s string, max int) string`。行为：`r := []rune(s)`；`len(r) <= max` 时返回原串；否则返回 `string(r[:max])`。按 **rune** 而不是 byte 截断，因此不会把一个多字节字符劈成半个——`TestHTTPEmbedderTruncatesLongInput` 用 40 个「中」验证截断后正好 10 个 rune。错误路径：无。被谁调用：`(HTTPEmbedder) Embed`。
- `snippet (internal/embed/embed.go)` —— `func snippet(b []byte) string`。行为：`s := strings.TrimSpace(string(b))`；`len(s) > 200` 时返回 `s[:200] + "…"`，否则返回 s。注意截断按 **byte**。错误路径：无。被谁调用：`(HTTPEmbedder) Embed` 的非 200 状态错误串。

## 18. internal/embed：`Fallback` 的三个方法

- `Fallback (internal/embed/embed.go)` —— `type Fallback struct { Primary Embedder; Secondary Embedder; OnFallback func(err error) }`。作用：先试 `Primary`，**任何错误**都退回 `Secondary`。类注释（**源码注释观点**）：方向才是重点——**哪个是 primary**：一个不能 embed 的网关绝不能把客户端请求弄失败；把本地 hashing embedder 当兜底，一次 embedding 故障就把缓存降级为词法匹配，而不是把每个 chat completion 变成 500；反过来把网络依赖当 primary 会让一个纯优化变成承重结构。`OnFallback` 的注释：它收到 primary 的错误，存在的意义是让调用方能统计降级次数，而**不必让这个包 import metrics**。
- `(Fallback) Embed (internal/embed/embed.go)` —— `func (f *Fallback) Embed(ctx context.Context, texts []string) ([][]float32, error)`。行为：调 `f.Primary.Embed`；`err == nil` 直接返回 `vecs, nil`；否则 `if f.OnFallback != nil { f.OnFallback(err) }`；`if f.Secondary == nil { return nil, err }`（**没有兜底时把 primary 的错误原样抛回**——`TestFallbackWithNoSecondaryReturnsThePrimaryError` 钉住）；否则 `return f.Secondary.Embed(ctx, texts)`。注意：兜底成功时 primary 的错误被**丢弃**，只剩 `OnFallback` 这个观测点。无锁、无共享状态。
- `(Fallback) Dims (internal/embed/embed.go)` —— `func (f *Fallback) Dims() int`。行为：`if d := f.Primary.Dims(); d > 0 { return d }`，否则 `return f.Secondary.Dims()`。语义：优先报告 primary 的宽度；primary 还没观测出宽度（HTTP embedder 首次调用前返回 0）时才问兜底。**本册观察**：当 `Primary.Dims() == 0` 且 `Secondary == nil` 时这里会 nil 解引用 panic；`Embed` 与 `Name` 都处理了 nil `Secondary`，只有 `Dims` 没有（列入第 46 节，未用测试确认）。
- `(Fallback) Name (internal/embed/embed.go)` —— `func (f *Fallback) Name() string`。行为：`f.Secondary == nil` 时返回 `f.Primary.Name()`；否则 `f.Primary.Name() + "|" + f.Secondary.Name()`，例如 `http|hashing`。
- 被谁调用：`internal/server/cache.go` 组装 `emb = &embed.Fallback{...}`，并在 admin 路径上用 `emb.(*embed.Fallback)` 取回 `Primary`。

## 19. internal/embed：`Scored` 与 `Rank`

- `Scored (internal/embed/embed.go)` —— `type Scored struct { Key string; Similarity float64; Model string; PromptHash string; CreatedAt time.Time }`。作用：给 admin 查找端点用的条目快照（键、相似度、模型、提示哈希、创建时间）。**源码注释有一处陈旧**：它上方的注释写的是「`SortedBySimilarity` is a small helper used by the admin lookup endpoint.」，而紧跟其后的类型名是 `Scored`——注释里那个名字并不存在（列入第 46 节）。
- `Rank (internal/embed/embed.go)` —— `func Rank(scores []Scored) []Scored`。行为：`sort.SliceStable(scores, func(i, j int) bool { return scores[i].Similarity > scores[j].Similarity })`，返回同一个切片（**原地排序**）。无锁。注释（**源码注释观点**）：它存在的意义是让 admin 界面与测试对「最近邻」只有**一个**定义。
- **本册观察（重要的事实差集）**：全仓库对 `embed.Rank` 的调用只有 `internal/embed/embed_test.go` 里的 `TestRankSortsByDescendingSimilarity`；admin 查找路径实际调用的是 `internal/cache/redis.go` 与 `internal/cache/memory.go` 里那个**另一个** `Rank`——`internal/cache/cache.go` 中的 `func Rank(matches []Match)`。因此注释里「admin 界面与测试共用一个定义」这句今天并不成立（列入第 46 节）。

## 20. internal/embed 测试与 `stubEmbedder`

`internal/embed/embed_test.go` 的包名是 `embed`（内部测试），所以它能直接触碰未导出的 `embedRequest`、`vector` 与 `hashFeature` 的产物。

- `stubEmbedder (internal/embed/embed_test.go)` —— `type stubEmbedder struct { name string; dims int; vec []float32; err error }`。作用：给 `Fallback` 测试用的假 `Embedder`。
- `(stubEmbedder) Embed` —— 有 `err` 就 `return nil, s.err`；否则把同一个 `s.vec` 复制成每个输入一个向量返回。
- `(stubEmbedder) Dims` / `(stubEmbedder) Name` —— 分别返回 `s.dims` 与 `s.name` 的一行访问器。
- `TestIdenticalTextIsExactlyIdentical`：钉住三条默认性质之一——同一文本两次必须 `Cosine == 1`（**精确等于 1**，不是近似），大小写/多余空白/标点差异必须归一化掉（`math.Abs(c-1) > 1e-9` 即失败）。注释（**源码注释观点**）：大小写、空白与标点是分词噪声而不是意义；一个会因此 miss 的缓存报告出的命中率低于真实值，并教会运维不信任这个指标。
- `TestNearDuplicatesBeatUnrelatedText`：契约是**顺序**而不是绝对值——插入、删除、改写三种近似必须都高于无关文本，且近似对必须 `>= 0.5`（否则 measure-m2 的阈值扫描没有可用区间）；无关文本必须 `<= 0.3`。
- `TestWordOrderMattersThroughBigrams`：`dog bites man` 与 `man bites dog` 的相似度必须 `< 1`（bigram 存在的意义就是阻止它成为完美匹配），且超集短语 `dog bites man bites dog` 必须比词序反转更接近原文。
- `TestChineseUsesCharacterBigrams`：两句改密中文必须高于一句无关中文，且相关度 `>= 0.4`；注释说明没有 CJK bigram 的话无空格句子就是一个 token。
- `TestVectorIsIndependentOfProcessState`：两个 64 维实例代表两个副本，`EncodeVector` 结果必须逐字节相等；注释（**源码注释观点**）说这是保护跨副本命中路径的那条测试，因为 per-process 播种会让共享 Redis 永远不可达。
- `TestDimsAndName`：0 维取 `DefaultHashingDims`、负维被钳住、128 维的 `Name` 是 `hashing-128`、输入几个就返回几个向量且每个长度是 128。
- `TestEmptyTextHasNoFeaturesAndNoNaNs`：纯空白 `Features` 必须为空；`""` 与 `"!!!"` 的向量里不许有 NaN/Inf，且与自己比较必须得到 `Cosine == 0`（没有特征可比）。
- `TestFeaturesIncludeUnigramsAndBigrams`：`Reset the Password` 的特征必须含 `reset`、`the`、`password`、`reset the`、`the password`，且**不得**含 `reset the password` 这个 trigram——注释：trigram 会让向量对插入一个词变得脆弱（**源码注释观点**）。
- `TestCosineEdgeCases`：空向量、维度不等、一侧零向量都必须是 0；未归一化的平行向量（`[2,0]` 与 `[5,0]`）必须是 1，正交向量是 0。
- `TestNormalizeHandlesZeroVector`：零向量归一化后必须仍然是零。
- `TestVectorEncodingRoundTripsExactly`：对包含 `-3.4028235e38`、`math.MaxFloat32`、`1.0/3.0` 的输入按 `math.Float32bits` 逐位比较；`DecodeVector("")` 必须是 `(nil, nil)`；非法 base64 与「字节数不是 4 的倍数」必须报错。
- `TestHTTPEmbedderPairsVectorsByIndex`：httptest 故意乱序返回 `index:1` 与 `index:0` 且不归一化，断言路径是 `/v1/embeddings`（base URL 末尾斜杠不重复）、`Authorization` 是 `Bearer secret`、请求体的 `Input[1]` 是 `second`、两个向量分别落到正确的输入上、`Dims()` 从第一个响应学到 3、`Name()` 是 `http:m`。注释（**源码注释观点**）：把错的向量配给错的文本会让缓存「记住」一个从未存在过的相似度。这个测试还带一段并发注释：handler 在 httptest 的 goroutine 上跑而断言在主测试 goroutine 上跑，所以它用 `sync.Mutex` 保护所有共享变量——「通过 socket 响应不是一个竞态检测器认可的 happens-before 边」。
- `TestHTTPEmbedderTruncatesLongInput`：`MaxInputChars: 10` 时必须把 40 个「中」截成 10 个 rune。
- `TestHTTPEmbedderErrorsAreExplicit`：六个子用例（`status` 503、`provider error`、`count mismatch`、`index out of range`、`missing vector`、`not json`）全部必须返回错误——这正对应第 16 节的六条错误路径。
- `TestHTTPEmbedderHonoursContextCancellation`：配置 `Timeout: 50ms`，服务端阻塞在 channel 上；必须返回超时错误，且耗时不夸张。补充：该测试结尾的 `defer func() { close(release); srv.Close() }()` 会先放行 handler 再关服务。
- `TestNewHTTPEmbedderValidatesOptions`：缺 `BaseURL` 与缺 `Model` 都必须在**构造时**被拒（注释：不是等到第一次使用）。
- `TestFallbackDegradesInsteadOfFailing`：primary 报错时 `Embed` 必须成功返回兜底向量、`OnFallback` 必须**恰好**观测到一次 primary 的错误（`errors.Is` 判定）、`Dims()` 优先 primary 的 3、`Name()` 是 `http|hashing`。
- `TestFallbackDoesNotCallTheHookOnSuccess`：primary 成功时钩子绝不能触发。
- `TestFallbackWithNoSecondaryReturnsThePrimaryError`：没有兜底时错误必须浮上来。
- `TestRankSortsByDescendingSimilarity`：`0.5/0.9/0.1` 三条必须排成 `b, a, c`。

## 21. internal/evalset：包注释与四个 `Kind` 常量

包注释（**源码注释观点**，本包存在的全部理由）：`internal/evalset` 保存用来标定语义缓存相似度阈值的标注问题对。**一个阈值就是一次下注**：猜太高，缓存永不命中，功能成了死重；猜太低，缓存会回答一个与所问不同的问题，那比慢更糟，因为调用方看不出来。唯一诚实地押这一注的办法，是拿「正确答案事先已知」的问题对去量它——这就是本包的作用。语料**刻意**在「必须命中」（identical 与 paraphrase）与「必须不命中」（near-miss 与 unrelated）之间平衡：一份只有释义的语料能替任何阈值背书；而 near-miss 对（密码/用户名、法国/德国、令牌桶/漏桶、北京/上海）才是顶住并把阈值从下方界住的东西。它同时也是**刻意小且手写**的：这些是人裁决过的对，不是模型输出，所以 embedder 与标签不一致时是真正的不一致，而不是标注产物。

- `KindIdentical (internal/evalset/evalset.go)` —— `const KindIdentical = "identical"`。注释（**源码注释观点**）：同一段文本两次，命中是强制的，也正是精确匹配路径的用途。
- `KindParaphrase (internal/evalset/evalset.go)` —— `const KindParaphrase = "paraphrase"`。注释：用不同的话问同一个问题，命中正是语义缓存的全部意义。
- `KindNearMiss (internal/evalset/evalset.go)` —— `const KindNearMiss = "near-miss"`。注释：共享大部分词汇但要的是别的东西；**在这里命中就是错误答案**，也正是这套标定要防的失败。
- `KindUnrelated (internal/evalset/evalset.go)` —— `const KindUnrelated = "unrelated"`。注释：最多只是同一话题。
- 被谁调用：`Case.ShouldHit`、`Cases` 的每条语料、`internal/evalset/evalset_test.go` 的 `TestCorpusIsWellFormed` 与 `TestLexicalEmbedderCannotSeparateEverything`、`cmd/measure-m2/main.go` 打印 `c.Kind`。

## 22. internal/evalset：`Case`、`ShouldHit`、`CaseResult`、`Correct`

- `Case (internal/evalset/evalset.go)` —— `type Case struct { ID string \`json:"id"\`; Kind string \`json:"kind"\`; A string \`json:"a"\`; B string \`json:"b"\`; Note string \`json:"note,omitempty"\` }`。作用：一条标注问题对。`Note` 的注释（**源码注释观点**）：记录这个标签为什么是这样，好让后来的读者能**反对**这个标签，而不必去逆向工程它。任务书里把它叫 `Pair`，源码里的实际类型名是 `Case`（本册按源码写）。
- `(Case) ShouldHit (internal/evalset/evalset.go)` —— `func (c Case) ShouldHit() bool { return c.Kind == KindIdentical || c.Kind == KindParaphrase }`。行为：值接收者，只读 `Kind`。注意：未知的 `Kind` 会被当作「不该命中」，函数不做白名单校验。被谁调用：`(CaseResult) Correct`、`Sweep`、`internal/evalset/evalset_test.go`、`cmd/measure-m2/main.go`（`-pairs` 打印 want），`cmd/verify-m2/main.go`。
- `CaseResult (internal/evalset/evalset.go)` —— `type CaseResult struct { Case; Similarity float64 \`json:"similarity"\`; Hit bool \`json:"hit"\` }`。作用：一条语料对一个 embedder 的测量结果；内嵌 `Case` 所以 `ID`/`Kind`/`A`/`B`/`Note` 直接可用。
- `(CaseResult) Correct (internal/evalset/evalset.go)` —— `func (r CaseResult) Correct() bool { return r.Hit == r.ShouldHit() }`。行为：把「实际命中」与「应该命中」比一比。**本册观察**：`Measure` 只填 `Similarity`，**不填 `Hit`**，所以刚 `Measure` 出来的结果上调用 `Correct` 的语义取决于调用方是否按阈值写过 `Hit`；`Sweep` 用的是自己的局部变量 `hit := r.Similarity >= th`，也没有回写 `r.Hit`。全仓库未能找到 `Correct` 的调用点（第 46 节）。

## 23. internal/evalset：`Cases` 的 identical 三对

- `Cases (internal/evalset/evalset.go)` —— `func Cases() []Case`。行为：每次调用**新建并返回**这个 26 条的切片字面量（没有包级变量、没有锁、没有缓存）；调用方可以随意改动返回值而不影响别人。被谁调用：`Measure` 与 `Sweep`（`cs == nil` 时兜底）、`internal/evalset/evalset_test.go` 的 `TestCorpusIsWellFormed`、`cmd/measure-m2/main.go`、`cmd/verify-m2/main.go`。
- identical（3 对，注释：命中不是一个判断题）：
  - `id-en-1` —— `how do I reset my password` / 同一句；Note：逐字节相同，精确匹配路径。
  - `id-en-2` —— 同一段长文 `Explain the sliding window breaker used by the gateway, including how the half-open state is entered.`；Note：长文本，让词频加权有活干。
  - `id-zh-1` —— `如何重置我的密码` / 同一句；Note：CJK 路径，字符 bigram，没有空白可切。

## 24. internal/evalset：`Cases` 的 paraphrase 十对

注释（**源码注释观点**）：这些是同一意图的另一种说法，也就是**从上界**限定一个可用阈值的那些对。

- `pp-en-1` —— `how do I reset my password` / `how can I reset my password`；Note：插入一个词。
- `pp-en-2` —— 同上但带 `for the admin console`；Note：更长句子里的一个插入词。
- `pp-en-3` —— `what is the capital of France` / `what's the capital of France`；Note：缩写，最难的词法情形，`is` 变成 `'s`。
- `pp-en-4` —— `explain the token bucket algorithm` / `can you explain the token bucket algorithm`；Note：礼貌前缀。
- `pp-en-5` —— `summarise the design document` / `please summarise the design document`；Note：只有前置礼貌词。**这一对就是 0.8819 的那一对**：`cmd/verify-m2/main.go` 的常量注释、`internal/cache/cache_test.go` 与 `internal/gateway/cache_test.go` 的常量注释都点名它是 `internal/evalset` 的 `pp-en-5`，用离线 embedder 实测 0.8819。
- `pp-en-6` —— `write a unit test for the router` / 末尾加 `please`；Note：只有后置礼貌词。
- `pp-en-7` —— `what does the failover header mean` / 末尾加问号；Note：只有标点差异。
- `pp-en-8` —— `list the environment variables the gateway reads` / `which environment variables does the gateway read`；Note：重排的问题，同一意图。
- `pp-zh-1` —— `如何重置我的密码` / `我该如何重置我的密码`；Note：CJK，插入字符。
- `pp-zh-2` —— `解释一下令牌桶算法` / `请解释一下令牌桶算法`；Note：CJK，礼貌前缀。

## 25. internal/evalset：`Cases` 的 near-miss 十对

注释（**源码注释观点**）：大部分词汇相同、要的是另一个东西；**在这里命中就是错误答案**，它们从**下界**限定阈值。

- `nm-en-1` —— `how do I reset my password` / `how do I reset my username`；Note：一个名词不同，答案是另一套流程。
- `nm-en-2` —— `...for the admin console` / `...for the user console`；Note：长共享框架里一个名词不同，**最难的 near-miss**。
- `nm-en-3` —— `what is the capital of France` / `what is the capital of Germany`；Note：一个国家不同，答案是另一座城市。
- `nm-en-4` —— `explain the token bucket algorithm` / `explain the leaky bucket algorithm`；Note：相邻但不同的限流算法。
- `nm-en-5` —— `summarise the design document` / `summarise the deployment document`；Note：一个名词不同。
- `nm-en-6` —— `list the environment variables the gateway reads` / `list the command line flags the gateway reads`；Note：一个名词短语不同。
- `nm-en-7` —— `write a unit test for the router` / `write a unit test for the breaker`；Note：一个包不同。
- `nm-en-8` —— `delete the cache entry for tenant alpha` / `delete the cache entry for tenant beta`；Note：一个标识符不同；**对错误的租户动手就是数据丢失**。
- `nm-zh-1` —— `如何重置我的密码` / `如何重置我的邮箱`；Note：CJK，一个名词不同。
- `nm-zh-2` —— `北京有多少人口` / `上海有多少人口`；Note：CJK，一个城市不同。

## 26. internal/evalset：`Cases` 的 unrelated 三对

注释（**源码注释观点**）：任何诚实的阈值都不该匹配它们。

- `un-en-1` —— `how do I reset my password` / `what is the weather in Beijing tomorrow`；Note：只共享 `my`/`the`。
- `un-en-2` —— `explain the token bucket algorithm` / `write a haiku about autumn`；Note：没有共享的实词。
- `un-zh-1` —— `如何重置我的密码` / `推荐几本科幻小说`；Note：CJK，没有共享 bigram。

合计 26 对：3 identical + 10 paraphrase + 10 near-miss + 3 unrelated；其中 `ShouldHit()` 为真的有 13 对，为假的也有 13 对。这与 `internal/cache/cache.go` 中 `cache.DefaultThreshold` 注释里「26 对手工标注：13 对必须命中、13 对必须不命中」一致。

## 27. internal/evalset：`ThresholdResult`、`HitRate`、`FalseHitRate`、`Report`

- `ThresholdResult (internal/evalset/evalset.go)` —— `type ThresholdResult struct { Threshold float64 \`json:"threshold"\`; TrueHits int \`json:"true_hits"\`; TrueTotal int \`json:"true_total"\`; FalseHits int \`json:"false_hits"\`; FalseTotal int \`json:"false_total"\`; MissedIDs []string \`json:"missed_ids,omitempty"\`; FalseHitIDs []string \`json:"false_hit_ids,omitempty"\` }`。逐字段注释（**源码注释观点**）：`TrueHits` 是应该命中且命中的条数；`TrueTotal` 是应该命中的总条数；`FalseHits` 是不该命中却命中的条数——**决定一个阈值能否发布的就是这个数**；`FalseTotal` 是不该命中的总条数；`MissedIDs` 点名「本该命中却没中」的对，好让取舍可检视而不是一个孤零零的数字；`FalseHitIDs` 点名被误匹配的对——**这些是调用方在不知情的情况下会收到的错误答案**。
- `(ThresholdResult) HitRate (internal/evalset/evalset.go)` —— `func (t ThresholdResult) HitRate() float64`。行为：`TrueTotal == 0` 返回 0，否则 `float64(t.TrueHits) / float64(t.TrueTotal)`（0..1 的比例，不是百分数）。被谁调用：`cmd/measure-m2/main.go` 的推荐行与 `printCurve`（都乘 100 打印）。
- `(ThresholdResult) FalseHitRate (internal/evalset/evalset.go)` —— `func (t ThresholdResult) FalseHitRate() float64`。行为：`FalseTotal == 0` 返回 0，否则 `float64(t.FalseHits) / float64(t.FalseTotal)`。被谁调用：同 `HitRate`。
- `Report (internal/evalset/evalset.go)` —— `type Report struct { Embedder string \`json:"embedder"\`; Dims int \`json:"dims"\`; Cases int \`json:"cases"\`; Thresholds []ThresholdResult \`json:"thresholds"\` }`。作用：一次完整扫描的结果。被谁调用：`Sweep` 构造它；`cmd/measure-m2/main.go` 收下它、把 `rep.Embedder` 重写成 `"<name>(dims=<n>)"` 并直接 JSON 序列化出去（`TestCorpusIsWellFormed` 无关，但 `internal/evalset/evalset_test.go` 的 `sweep`/`find` 帮手用 `evalset.Report`）。

## 28. internal/evalset：`Measure`

- `Measure (internal/evalset/evalset.go)` —— `func Measure(ctx context.Context, e embed.Embedder, cs []Case) ([]CaseResult, error)`。行为：`len(cs) == 0` 时 `cs = Cases()`；`out := make([]CaseResult, 0, len(cs))`；对每条 `c` 调 `e.Embed(ctx, []string{c.A, c.B})`；成功则 append `CaseResult{Case: c, Similarity: embed.Cosine(vecs[0], vecs[1])}`；最后用 `sort.SliceStable` 排序——**先按 Similarity 降序，相同再按 ID 升序**；返回 `out, nil`。
- 错误路径（两条，照抄原文）：`e.Embed` 失败 → `evalset: embed %s: %w`（`%s` 是 `c.ID`）；`len(vecs) != 2` → `evalset: embed %s: want 2 vectors, got %d`（`%s` 是 `c.ID`，`%d` 是实际条数）。
- 注释（**源码注释观点**，这是本函数的核心）：排序让 near-miss 与 paraphrase 挨在一起——**这个顺序本身就是结论**：如果一条 near-miss 排在一条 paraphrase 之上，那就没有任何阈值能把它们分开。注意它**不设置** `Hit` 字段。
- 被谁调用：`Sweep`（唯一的生产调用者）、`internal/evalset/evalset_test.go` 的 `TestLexicalEmbedderCannotSeparateEverything`、`cmd/measure-m2/main.go`（经 `Sweep` 间接用）。

## 29. internal/evalset：`Sweep` 与 `similarity >= threshold`

- `Sweep (internal/evalset/evalset.go)` —— `func Sweep(ctx context.Context, e embed.Embedder, thresholds []float64, cs []Case) (Report, []CaseResult, error)`。行为：先 `results, err := Measure(ctx, e, cs)`，失败则 `return Report{}, nil, err`；构造 `rep := Report{Embedder: e.Name(), Dims: e.Dims(), Cases: len(results)}`；对每个 `th` 建 `tr := ThresholdResult{Threshold: th}`，遍历 `results`：
  - 命中判定是 **`hit := r.Similarity >= th`**——**包含式**，`similarity == threshold` 算命中。
  - `r.ShouldHit()` 为真：`tr.TrueTotal++`；`hit` 则 `tr.TrueHits++`，否则把 `r.ID` 追加进 `tr.MissedIDs`；`continue`。
  - 否则：`tr.FalseTotal++`；`hit` 则 `tr.FalseHits++` 并把 `r.ID` 追加进 `tr.FalseHitIDs`。
  - 每个阈值结束后 `rep.Thresholds = append(rep.Thresholds, tr)`。
  返回 `rep, results, nil`。错误路径：只有 `Measure` 那两条；`thresholds` 为空不会报错，只会返回一个 `Thresholds` 为空的 `Report`（`cmd/measure-m2/main.go` 的 `printCurve` 会因此取 `rep.Thresholds[0]` 越界——那是调用方的输入问题）。
- 注释（**源码注释观点**）：阈值是包含式的，`similarity >= threshold` 命中。`>=` 这个事实由 `internal/evalset/evalset_test.go` 的 `TestCurveIsMonotonic` 反证：提高阈值只可能把命中变成未命中，若这条失败，说明比较不是简单的 `>=` 门。
- 被谁调用：`cmd/measure-m2/main.go`（`rep, cases, err := evalset.Sweep(ctx, emb, grid, nil)`）。`internal/evalset/evalset_test.go` 里的 `sweep` 帮手是唯一另一个调用点。
- 第二个返回值 `[]CaseResult` 是**排好序的原始相似度**，`cmd/measure-m2` 用 `-pairs` 打印它。

## 30. internal/evalset：`Recommend`

- `Recommend (internal/evalset/evalset.go)` —— `func Recommend(rep Report, maxFalseHits int) (ThresholdResult, bool)`。行为：`var best ThresholdResult; found := false`；遍历 `rep.Thresholds`，`tr.FalseHits > maxFalseHits` 则 `continue`；否则 `if !found || tr.TrueHits > best.TrueHits || (tr.TrueHits == best.TrueHits && tr.Threshold > best.Threshold) { best, found = tr, true }`；返回 `best, found`。
- 语义与注释（**源码注释观点**）：挑「误命中数在预算内」中命中率最高的阈值；**平手时安全优先**，因为一次未命中要花钱，而一个错误答案要花信任。当没有任何被扫过的阈值满足预算时它返回 `false`——注释强调这是一个**真实可能的结果**，调用方绝不能把它糊过去：那意味着这个 embedder 分不开这份语料，诚实的回应是说清楚，而不是发布一个用错误答案换漂亮数字的阈值。
- 平手规则读法：`TrueHits` 相同则取**更大**的 `Threshold`，也就是更保守的那一端。这正是 `internal/cache/cache.go` 里 `DefaultThreshold = 0.86` 与扫描建议 0.88 之间存在分歧的机制来源：0.86 与 0.88 都是「零错答货架」的两端（各自 8/13），`Recommend` 会推 0.88，而常量取了另一端的 0.86。
- 被谁调用：`cmd/measure-m2/main.go`（`evalset.Recommend(rep, *maxFalse)`，`-max-false-hits` 默认 0）。

## 31. internal/evalset：`DefaultThresholds` 与 0.86 的来历

- `DefaultThresholds (internal/evalset/evalset.go)` —— `func DefaultThresholds() []float64`。行为：每次调用新建并返回一个 19 元素的切片：
  `0.60, 0.65, 0.70, 0.75, 0.80, 0.82, 0.84, 0.86, 0.88, 0.90, 0.91, 0.92, 0.93, 0.94, 0.95, 0.96, 0.97, 0.98, 0.99`。
  注释（**源码注释观点**）：这是测量脚本扫的网格，在 0.8 至 0.95 这个「词法 embedder 真正做决定」的区间里足够细。
- 被谁调用：`cmd/measure-m2/main.go`（`grid := evalset.DefaultThresholds()`，`-thresholds` 非空时整份替换）、`internal/evalset/evalset_test.go` 的 `sweep` 与 `find`（`find` 在网格里找不到目标阈值时会把它打印进失败信息）。
- **0.86 的来历（源码注释观点，出自 `internal/cache/cache.go` 的 `cache.DefaultThreshold`，本册照译）**：它是**量出来的，不是选的**。`cmd/measure-m2` 拿 `internal/evalset` 的 26 对标注语料（13 对必须命中、13 对必须不命中）对着离线 embedder 跑并扫阈值；在 0.86 上那份语料给出 **62% 的释义命中率且零错答**。显而易见的替代方案以一种容易被忽略的方式更糟：**0.84 换来 77% 命中率**，而它开始匹配的三对是「delete the cache entry for tenant alpha」对「tenant beta」、「write a unit test for the router」对「the breaker」、「reset my password for the admin console」对「the user console」——每十三对多两个命中，代价是一个错的租户、一个错的包、一个错的控制台。未命中要花钱，错误答案要花信任。此前那个 0.94 来自直觉，在同一份语料上只有 31% 命中率，这就是这个常量如今引用证据的原因。**天花板是 embedder 的性质而不是阈值的性质**：语料里有五条 paraphrase 得分**低于**它们最近的 near-miss，所以没有任何阈值能把它们分开；把 `cache.embedding` 指向一个真模型能抬高天花板，然后重跑扫描找出新的位置。
- 同一段注释继续说明两端的取舍：**0.86 与 0.88 是整条「零错答货架」**（各自 8/13）；扫描自己的推荐落在 0.88 那一端，而这个常量取另一端，因为语料里最紧的真释义测得 **0.8819**——是刀刃而不是余量；**0.90 会把命中率压到 6/13**。
- 本册补齐的对应关系：那条「最紧的真释义」就是 `pp-en-5`（`summarise the design document` 对 `please summarise the design document`），0.8819 这个数在 `cmd/verify-m2/main.go`、`internal/cache/cache_test.go`、`internal/gateway/cache_test.go` 三处注释里都被点名到 `pp-en-5`（`internal/evalset/evalset.go` 自身的注释不点名具体 ID）。

## 32. internal/evalset 测试

`internal/evalset/evalset_test.go` 的包名是 `evalset_test`（外部测试包），因此它只能通过 `evalset.` 前缀访问，并额外 import `internal/cache` 与 `internal/embed`。

- `sweep (internal/evalset/evalset_test.go)` —— `func sweep(t *testing.T, e embed.Embedder) evalset.Report`。行为：`t.Helper()`；调 `evalset.Sweep(context.Background(), e, evalset.DefaultThresholds(), nil)`（`cs` 传 nil，让 `Sweep` 兜底到 `Cases()`）；出错 `t.Fatalf("Sweep: %v", err)`；返回 `rep`。
- `find (internal/evalset/evalset_test.go)` —— `func find(t *testing.T, rep evalset.Report, threshold float64) evalset.ThresholdResult`。行为：`t.Helper()`；在 `rep.Thresholds` 里找 `tr.Threshold == threshold` 的那一项并返回；找不到 `t.Fatalf`，错误串原文是 `threshold %.2f is not in the swept grid %v`（`%v` 是 `evalset.DefaultThresholds()`），随后返回零值 `evalset.ThresholdResult{}`。
- `TestCorpusIsWellFormed`：注释（**源码注释观点**）说语料是 `cache.DefaultThreshold` 背后的证据，所以它自身的完整性也值得一条测试——一份悄悄丢掉 near-miss 对的语料能替任何阈值背书。断言：条数 `>= 20`；ID 不重复；每边非空；每条 `Note` 非空；`ShouldHit()` 必须等于 `Kind` 推出的期望；四个 `Kind` 都至少出现一次；`KindNearMiss >= 8` 且 `KindParaphrase >= 8`（注释：只有三条 near-miss 对三十条 paraphrase 会把阈值推低）。
- `TestCurveIsMonotonic`：阈值必须严格递增（`cur.Threshold <= prev.Threshold` 即失败），且提高阈值时 `TrueHits` 与 `FalseHits` 都不得上升。注释（**源码注释观点**）：如果这条失败，说明 `Sweep`（或 store 里）的比较不是简单的 `>=` 门。
- `TestShippedThresholdHasNoFalseHits`：在出厂默认阈值上 `FalseHits != 0` 即失败，失败信息会点名会送达的错误答案（`at.FalseHitIDs`）；`TrueHits < 8` 也失败（注释：太严就不值得发布）。随后它查 `0.84` 这个点：如果那里**不再**产生误命中就 `t.Skipf("at %.2f the corpus no longer produces false hits; re-measure and lower the default", ...)`，否则 `t.Logf` 出会被误配的 ID。注释（**源码注释观点**）：这条测试是阻止未来一次编辑「为了让命中率好看而降低阈值」的闸门；同时它把 `cache.DefaultThreshold` 注释里那张取舍表钉成必须为真，一旦不再为真，注释就过时了，应该重新测量而不是默默保留。
- `TestLexicalEmbedderCannotSeparateEverything`：调 `evalset.Measure`，在没有 `ShouldHit` 的类型过滤下（用 `switch c.Kind`）算出最差的 paraphrase（初始 1，取最小）与最近的 near-miss（初始 0，取最大）；若 `nearestNearMiss < worstParaphrase` 则 `t.Skip("this embedder separates the corpus; the offline default could be raised")`，否则 `t.Logf` 出两个数。注释（**源码注释观点**）：词法 embedder 分不开每一条 paraphrase 与每一条 near-miss，这是「离线默认单独用不够好」的**结构性**原因；把它断言下来，是为了没人会花一下午调阈值去修它——修法是换一个真正的 embedding 模型，不是一个数字。

## 33. internal/mockbackend：与 05 册的边界

本包已被 05 册第 43 至 46 节覆盖。为避免重复，本册只做两件事：把 05 册**没讲**的部分逐符号补齐，并在本节末尾列出 05 册已覆盖的清单。

- 05 册第 43 节：包注释（库而不是只做一个二进制、多个不同行为后端、per-backend 请求日志是路由与故障转移的证据）。
- 05 册第 44 节：`HeaderStatus`、`HeaderDelay`、`HeaderTTFB`、`HeaderOmitEnd`、`HeaderUpstreamName` 五个常量的值与英文注释，以及「per-request 因而同一后端可时而健康时而坏掉」的理由。
- 05 册第 45 节：`Options` 逐字段、`Call` 逐字段（含 `Authorization` 单独成字段的理由）、`Backend` 的字段与并发分工、`New` 的作用与 `/calls` 的 `DELETE` 语义、`/v1` 前缀被 `strings.TrimPrefix` 掉的理由、`Calls`/`Count`/`Reset`/`SetFailStatus`/`SetStall`/`stall` 的行为与两条「为什么运行时控制而不是重启」的注释。
- 05 册第 46 节：`chat` 的十步顺序（405、JSON 解析、写日志、解析计时头、停滞、注入状态、永久失败、`delay` 睡眠、空 model、分流）、`streamChat` 的帧序与「这里不再停滞」的理由、`durationHeader` 的两条错误串、`readAll`、`writeJSON`、`errorBody`，以及三个测试名与它们钉住的断言。
- 本册补充（05 册未展开的部分）：`handler` 匿名分发闭包与它的 `default` 404 分支；`/embeddings` 路由的响应体；`/healthz` 与 `/v1/models` 的响应体；**`Options.Methods` 被声明却在 `internal/mockbackend/mockbackend.go` 里从未被读取**；只有 `/v1` 前缀的两个模式被 `mux.HandleFunc` 注册，而源码注释声称「两种形状都被注册」；`X-Mock-Upstream` 只在 SSE 响应上设置；`req.Tools` 被解码却从未使用；`X-Mock-Status` 经 `strconv.Atoi` 接受越界与负值；`TokenDelay` 是「先写帧再睡」；`/v1/models` 恒返回单个 `mock-gpt`。

## 34. internal/mockbackend：常量与 `Options`、`Call`、`Backend`

这一节把符号本身记全，值与前缀细节与 05 册一致；新增的是「谁在读它」这一列的事实。

- `HeaderStatus (internal/mockbackend/mockbackend.go)` —— `const HeaderStatus = "X-Mock-Status"`。注释原文：reply with this HTTP status instead。读者：`chat`。**本册补充**：它是**按请求**生效的，所以同一个永久坏掉的后端也能被要求对单独一次调用返回指定状态。
- `HeaderDelay (internal/mockbackend/mockbackend.go)` —— `const HeaderDelay = "X-Mock-Delay"`。注释原文：sleep this long before answering。读者：`chat`（经 `durationHeader`，再喂给 `stall`）。
- `HeaderTTFB (internal/mockbackend/mockbackend.go)` —— `const HeaderTTFB = "X-Mock-TTFB"`。注释原文：sleep this long before the first stream frame。读者：`chat`。**本册补充**：尽管注释写「first stream frame」，它的停滞在实现上作用于**任何**回答（见 `stall`），这正是非流式上游超时场景能成立的原因。
- `HeaderOmitEnd (internal/mockbackend/mockbackend.go)` —— `const HeaderOmitEnd = "X-Mock-Omit-Done"`。注释原文：stream without the `[DONE]` sentinel。读者：`streamChat`。**本册补充**：判定条件是 `r.Header.Get(HeaderOmitEnd) == ""`，所以**任何非空值**（包括 `"false"`、`"0"`）都会省略哨兵。
- `HeaderUpstreamName (internal/mockbackend/mockbackend.go)` —— `const HeaderUpstreamName = "X-Mock-Upstream"`。作用：让客户端不读 body 也能知道是哪个副本答的。**本册补充**：它只在 `streamChat` 里被 `w.Header().Set`；非流式的 JSON 回答**不设**这个头，副本名只出现在 body 的 `"mock answer from " + b.name` 里，`/healthz` 也不设它。
- `Options (internal/mockbackend/mockbackend.go)` —— `type Options struct { Name string; TTFB time.Duration; TokenDelay time.Duration; FailStatus int; Methods bool }`。逐字段读法：`Name` 出现在 `X-Mock-Upstream` 与 `/healthz` 的 `upstream` 字段里；`TTFB` 是默认停滞（请求头与运行时值可覆盖）；`TokenDelay` 给流式帧之间定速；`FailStatus` 非零让每个 chat 请求以该状态失败（除非请求显式带 `X-Mock-Status`）；`Methods` 的注释说「为真会在模型列表里追加一个 `methods` 能力」——**这在本文件里没有被实现**，`New` 也不读它（见第 36 节）。
- `Call (internal/mockbackend/mockbackend.go)` —— `type Call struct { Path string; Model string; Stream bool; Authorization string; Header http.Header }`。作用：一条被记录的请求。`Path` 记的是**原始** `r.URL.Path`（不去 `/v1` 前缀），`Header` 是 `r.Header.Clone()` 的快照。
- `Backend (internal/mockbackend/mockbackend.go)` —— `type Backend struct { *httptest.Server; name string; ttfb time.Duration; delay time.Duration; failState atomic.Int32; stallFor atomic.Int64; mu sync.Mutex; calls []Call }`。并发保护逐项：`calls` 只被 `mu` 保护（写：`chat`；读：`Calls`/`Count`/`Reset`/`/calls` 处理器）；`failState` 是 `atomic.Int32`，`stallFor` 是 `atomic.Int64`，所以请求进行中也改得动行为；`name`/`ttfb`/`delay` 构造后只读。内嵌的 `*httptest.Server` 在 `New` 末尾被赋值，关闭是调用方的责任（`defer b.Close()`）。

## 35. internal/mockbackend：`New` 与路由注册

- `New (internal/mockbackend/mockbackend.go)` —— `func New(opts Options) *Backend`。行为：`opts.Name == ""` 时取 `"mock"`；建 `b := &Backend{name: opts.Name, ttfb: opts.TTFB, delay: opts.TokenDelay}`（注意 `TokenDelay` 存进字段 `delay`）；`b.failState.Store(int32(opts.FailStatus))`；**`opts.Methods` 在此处及之后都没有被读取**；建 `mux := http.NewServeMux()` 并注册路由；`b.Server = httptest.NewServer(mux)`；返回 b。错误路径：无 error 返回；失败会直接 panic（`httptest.NewServer` 的契约）。
- 注册的五个模式与它们的处理器：
  - `/healthz`（匿名函数）—— `writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "upstream": b.name})`。**本册补充**（05 册只说了「返回 `{"status":"ok","upstream":name}`」）：它不校验方法，也不记入 `calls`。
  - `/v1/models`（匿名函数）—— `writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []map[string]any{{"id": "mock-gpt", "object": "model"}}})`。**本册补充**：返回值是**常量**，不受 `Options.Methods` 或任何配置影响；也不记入 `calls`。
  - `/calls`（匿名函数）—— `r.Method == http.MethodDelete` 时 `b.Reset()` 并 `writeJSON(w, http.StatusOK, map[string]any{"reset": true})`；否则 `writeJSON(w, http.StatusOK, map[string]any{"calls": b.Calls(), "count": len(b.Calls())})`。**本册补充**：那行 `count` 是第二次 `b.Calls()` 调用，所以是**第二个快照**而不是第一次快照的长度——两个快照之间若恰有请求落盘，`count` 与 `calls` 的长度可能不等。这是本册的观察，未用并发测试确认（第 46 节）。
  - `/v1/chat/completions` 与 `/v1/embeddings`（都指向同一个匿名 `handler`）。
- `handler`（`internal/mockbackend/mockbackend.go` 里的匿名函数，本册补 05 册未展开的部分）—— 行为：`switch strings.TrimPrefix(r.URL.Path, "/v1")`，命中 `"/chat/completions"` 走 `b.chat(w, r)`，命中 `"/embeddings"` 走一段内联的 `writeJSON`，**default 分支是 `http.NotFound(w, r)`**。`/embeddings` 的响应体是 `{"object":"list","model":"mock-embed","data":[{"object":"embedding","index":0,"embedding":[0.01,-0.02,0.03]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`——三个固定维度的浮点数，`index` 固定 0，不解析请求、也不看请求里的 `input`。
- 注释（**源码注释观点**，05 册已译）：`/v1` 前缀在分发前被去掉，因为网关逐字转发调用方的路径；早先的版本在完整路径上匹配，于是每个请求都变成 404。注释接着说「两种形状都被注册，因为 base_url 本身可能已经以 `/v1` 结尾」。**本册的读法**：紧随其后的两行 `mux.HandleFunc` 注册的是 `/v1/chat/completions` 与 `/v1/embeddings`，两个模式都带 `/v1` 前缀，注册表里并没有裸形状的模式；因此「两种形状都被注册」这句注释与注册行之间是否等价（对一个不带 `/v1` 的请求，`TrimPrefix` 是否还有机会生效）本册**未能确认**，见第 46 节。

## 36. internal/mockbackend：`/v1/models` 与未被读取的 `Options.Methods`

- 本节单独成条，因为它是 05 册完全没有提、而本册在源码里确认的差异。`Options.Methods` 的注释（**源码注释观点**）是「`Methods`，为真时在模型列表里追加一个额外的 `methods` 能力；能力路由由网关配置，不由后端决定。」但：
  - `/v1/models` 的处理器返回的是写死的 `mock-gpt` 一项，不看 `opts`、不看 `b`；
  - `New` 只读 `opts.Name`、`opts.TTFB`、`opts.TokenDelay`、`opts.FailStatus`，从不读 `opts.Methods`；
  - 对全仓库 Go 文件检索 `Methods`，`internal/mockbackend/mockbackend.go` 里只有**字段声明与它的注释**，没有任何读取点。
- 因此本册的结论是：`Options.Methods` 在 `internal/mockbackend` 里是一个**未被使用的字段**，它的注释描述的行为在这个包里没有实现。它之所以不报错，是因为 Go 允许未使用的结构体字段。这是本册的观察（第 46 节标注确认程度）。
- 顺带记录一个同类事实：`Call.Model` 与 `Call.Stream` 来自 `chat` 里对请求体的 `json.Unmarshal`，而该匿名结构体还声明了 `Tools []any \`json:"tools"\``——**`req.Tools` 被解码但从未被读取**，所以这个 mock 在任何情况下都不会发工具调用帧（真正会发工具调用帧的实现另有其人：`cmd/mockupstream`）。

## 37. internal/mockbackend：`/healthz`、`/calls` 与请求日志访问器

- `(Backend) Calls (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) Calls() []Call`。行为：`b.mu.Lock(); defer b.mu.Unlock()`；`out := make([]Call, len(b.calls)); copy(out, b.calls); return out`——**返回副本**，调用方拥有它，改它不会回头改日志。被谁调用：`/calls` 路由的匿名处理器（两次）；验收器也可通过 HTTP 读同一份数据。
- `(Backend) Count (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) Count() int`。行为：`mu` 下返回 `len(b.calls)`。语义上是「这个后端见过的 chat 请求数」——因为只有 `chat` 会往 `calls` 里追加（`/healthz`、`/v1/models`、`/calls`、`/embeddings` 都不追加）。被谁调用：大量验收器，例如 `cmd/verify-m1/checks.go`、`cmd/verify-m2/main.go`、`cmd/verify-m3/governance.go`、`cmd/verify-m3/governance2.go`、`cmd/verify-m5/checks_traces.go`、`cmd/verify-m6` 的多个检查文件。
- `(Backend) Reset (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) Reset()`。行为：`mu` 下 `b.calls = nil`（不是 `b.calls[:0]`，所以旧底层数组被放开）。被谁调用：`/calls` 的 `DELETE` 分支、`cmd/verify-m1/checks.go`、`cmd/verify-m5/checks_metrics.go`（后者是对另一个 recorder 的同名方法）。
- `/healthz` 与 `/v1/models` 都不记日志这一事实，对「用 `Count()` 证明路由发生了」的验收器很重要：只有真正走到 `chat` 的请求才会被计数。

## 38. internal/mockbackend：`SetFailStatus`、`SetStall`、`stall`

- `(Backend) SetFailStatus (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) SetFailStatus(status int) { b.failState.Store(int32(status)) }`。行为：一行原子写；`status` 为 0 表示恢复。注释（**源码注释观点**）：它让一个后端在一个测试内先坏掉再被修好；替代方案——重启一个后端来「修好」它——会**同时重置它的连接池与它的熔断窗口，而那正是被测状态**。被谁调用：`cmd/verify-m1/checks.go`（`SetFailStatus(http.StatusServiceUnavailable)` 后再 `0`、`http.StatusTooManyRequests` 后再 `0`）、`cmd/verify-m3/governance2.go`（`stB.backend.SetFailStatus(500)`）。
- `(Backend) SetStall (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) SetStall(d time.Duration) { b.stallFor.Store(int64(d)) }`。行为：一行原子写，0 表示恢复。注释（**源码注释观点**）：运行时控制是刻意的——为了改变速度而重启后端会顺带重置连接池与熔断窗口，而一个超时场景的全部要点就是看网关对**已经存在**的状态做了什么。被谁调用：`cmd/verify-m1/checks.go`（配 `defer slow.SetStall(0)`）、`internal/mockbackend/mockbackend_test.go` 的 `TestHeaderStallWinsOverTheRuntimeStall`。
- `(Backend) stall (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) stall(header time.Duration) time.Duration`。行为：`header > 0` 返回 header；否则 `d := time.Duration(b.stallFor.Load())`，`d > 0` 返回 d；否则返回 `b.ttfb`。错误路径：无。注释（**源码注释观点**）明说优先级是「请求头的值，已由 `chat` 解析并校验，胜过运行时值，运行时值胜过配置值」。**本册补充**：因为判定是 `> 0` 而不是「非零」，一个显式的 `0ms` 头不会「取消」停滞，它会掉到运行时值与配置值上——想要零停滞只能保证后两者也是零。

## 39. internal/mockbackend：`chat` 的逐段顺序

- `(Backend) chat (internal/mockbackend/mockbackend.go)` —— 匿名方法 `func (b *Backend) chat(w http.ResponseWriter, r *http.Request)`（源码里没有显式签名行，它是通过 `mux.HandleFunc("/v1/chat/completions", handler)` 的分发到达的）。逐段行为与错误串（全部照抄原文）：
  1. `r.Method != http.MethodPost` → `writeJSON(w, http.StatusMethodNotAllowed, errorBody("method not allowed", "invalid_request_error"))` 并返回。
  2. 声明匿名请求结构 `struct { Model string \`json:"model"\`; Stream bool \`json:"stream"\`; Tools []any \`json:"tools"\` }`；`body, _ := readAll(r)`——**读取错误被忽略**；`json.Unmarshal` 失败 → `writeJSON(w, http.StatusBadRequest, errorBody("invalid JSON body: "+err.Error(), "invalid_request_error"))`。（注意错误串是拼接而不是 `%w` 包装。）
  3. 在 `b.mu` 下追加日志：`Call{Path: r.URL.Path, Model: req.Model, Stream: req.Stream, Authorization: r.Header.Get("Authorization"), Header: r.Header.Clone()}`。
  4. 解析两个计时头，**顺序固定为 TTFB 先、Delay 后**：`ttfb, err := durationHeader(r, HeaderTTFB)`；失败 → 400 + `errorBody(err.Error(), "invalid_request_error")`；接着 `delay, err := durationHeader(r, HeaderDelay)`；同样失败 → 400。注释（**源码注释观点**）：计时头在任何东西被写出**之前**解析，所以一个不可读的值是一个**点名了那个头**的 400，而不是静默的「没有停滞」；`HeaderStatus` 的优先级在这个检查**之后**，即一个注入了状态的请求永远不会走到 sleep，而这个先后顺序比这次检查更早。
  5. 停滞：`if stall := b.stall(ttfb); stall > 0 { time.Sleep(stall) }`。见第 40 节。
  6. 注入状态：`v := r.Header.Get(HeaderStatus)` 非空时 `code, err := strconv.Atoi(v)`；失败 → `writeJSON(w, http.StatusBadRequest, errorBody("X-Mock-Status must be an integer", "invalid_request_error"))`；成功 → `writeJSON(w, code, errorBody(fmt.Sprintf("injected status %d", code), "mock_error"))` 并返回。**本册补充**：`strconv.Atoi` 接受负数与 1–2 位数（如 `-1`、`0`、`99`），而 `writeJSON` 会把它们直接交给 `http.ResponseWriter.WriteHeader`；`net/http` 对不在 100–999 的状态码是 panic 而不是一个干净的 400。这条属于本册的读码推论，未用测试确认（第 46 节）。注释（**源码注释观点**）：一个永久坏掉的后端仍然尊重 per-request 的显式状态，所以一个实例可以既服务「永远 503」（熔断测试）又服务「只 503 一次」（故障转移测试）。
  7. 永久失败：`b.failState.Load() != 0` → `code := int(b.failState.Load())`，`writeJSON(w, code, errorBody(fmt.Sprintf("%s is permanently failing with %d", b.name, code), "mock_error"))` 并返回。
  8. `delay > 0` 则 `time.Sleep(delay)`——注意这个 `delay` 是 `HeaderDelay`，与第 5 步的 `ttfb` 是两笔独立的睡眠。
  9. `req.Model == ""` → `writeJSON(w, http.StatusBadRequest, errorBody("you must provide a model parameter", "invalid_request_error"))`。**本册补充**：这个校验发生在两笔睡眠**之后**，所以一个缺 `model` 的请求也会先被拖慢。
  10. `req.Stream` 为真 → `b.streamChat(w, r, req.Model)` 并返回；否则写完整回答：`{"id":"chatcmpl-mock","object":"chat.completion","created":time.Now().Unix(),"model":req.Model,"choices":[{"index":0,"message":{"role":"assistant","content":"mock answer from "+b.name},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`。

## 40. internal/mockbackend：头解析顺序与「停滞」发生时机

这一节把第 39 节里两个最容易读错的点单独钉住，因为 05 册把它们压缩成了一行。

- **头解析顺序**：`HeaderTTFB` 先于 `HeaderDelay` 被解析。因为两个解析都在任何字节被写出之前，所以两个都畸形时**只有 TTFB 那条错误会浮出来**（Delay 根本没被读到）；body 里点名的是 `X-Mock-TTFB`。
- **停滞的覆盖面**：停滞在 `req.Stream` 分流**之前**发生，所以它对**每个**回答生效——流式与非流式一样。注释（**源码注释观点**）：只把停滞绑在流式路径上，会让一个非流式的「慢后端」在微秒内作答，于是上游超时场景什么也证明不了，网关根本没有理由放弃它。
- **停滞相对注入状态的位置**：`stall` 的 `time.Sleep` 在 `HeaderStatus` 解析**之前**。注释（**源码注释观点**）：在注入状态之前睡同样重要——一个**慢慢地失败**的后端才能让故障转移的代价可测量，而一个快速失败的后端会把这个代价藏起来。这同时意味着「`X-Mock-Status: 503` + `X-Mock-TTFB: 2s`」会真的先睡 2 秒再返回 503。
- **`HeaderDelay` 的位置**：它在 `HeaderStatus` 与永久失败检查**之后**、空 `model` 检查**之前**。所以「注入状态」与「永久失败」都不会被 `X-Mock-Delay` 拖慢，而一个正常回答会被拖慢。05 册把第 8 步记作 `delay > 0 则 time.Sleep(delay)`，位置信息是本册补的。
- **`TokenDelay` 与这两个头的区别**：`HeaderDelay`/`HeaderTTFB` 只影响响应前的那一笔睡眠；`Options.TokenDelay` 是唯一能影响**帧间**节奏的旋钮，而且只有构造时可设、不能按请求覆盖。

## 41. internal/mockbackend：`streamChat` 的 SSE 发射顺序

- `(Backend) streamChat (internal/mockbackend/mockbackend.go)` —— `func (b *Backend) streamChat(w http.ResponseWriter, r *http.Request, model string)`。行为：
  1. `flusher, ok := w.(http.Flusher)`；`!ok` → `http.Error(w, "streaming unsupported", http.StatusInternalServerError)` 并返回。
  2. 设 `Content-Type: text/event-stream`、`Cache-Control: no-cache`、`X-Mock-Upstream: <b.name>`；然后 `w.WriteHeader(http.StatusOK)` 与 `flusher.Flush()`。注释（**源码注释观点**）：这里**不再停滞**——延迟只在 `chat` 里施加一次，在任何 header 或 status 被选中之前；两个地方都睡会把配置的延迟翻倍，并让「首 token 时间」的测量取决于后端恰好查了几次时钟。
  3. `emit` 闭包：`data, _ := json.Marshal(payload)`（**编码错误被忽略**）；`fmt.Fprintf(w, "data: %s\n\n", data)`；`flusher.Flush()`；**然后** `if b.delay > 0 { time.Sleep(b.delay) }`。**本册补充**：睡眠在写帧**之后**，所以第一帧（role 帧）立刻到达，`TokenDelay` 表现为帧间节奏；也正是这个顺序让「按 `TokenDelay` 计的首 token 时间被推迟一次」不会发生。`HeaderTTFB`/`HeaderDelay` 与这个字段互不影响。
  4. 五帧（每帧 `object` 都是 `chat.completion.chunk`，`id` 都是 `chatcmpl-mock`，`model` 是传入的 `model`）：
     - role 帧：`choices: [{"index":0,"delta":{"role":"assistant","content":""}}]`。
     - content 帧 1：`choices: [{"index":0,"delta":{"content":"mock "}}]`。
     - content 帧 2：`choices: [{"index":0,"delta":{"content":"answer"}}]`——两帧拼接后是 `mock answer`（空格在第一帧里）。
     - finish 帧：`choices: [{"index":0,"delta":{},"finish_reason":"stop"}]`。
     - usage 帧：`choices: []`（**空数组而不是省略**），`usage: {"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}`。
  5. `if r.Header.Get(HeaderOmitEnd) == "" { fmt.Fprint(w, "data: [DONE]\n\n"); flusher.Flush() }`——哨兵帧**不走 `emit`**，所以它后面跟着的那次 `TokenDelay` 睡眠不会发生。
- 错误路径：只有第 1 步的 `"streaming unsupported"`（HTTP 500）。其余写操作忽略错误（与 `writeJSON` 一致的风格）。
- 被谁调用：`chat` 当 `req.Stream` 为真时。
- 与文档里五点顺序的对应：role 帧 → 逐词 content 帧（这里恰是两个固定词）→ finish 帧 → usage 帧 → `[DONE]`。要复现任务书说的「逐词」效果，把 `TokenDelay` 设成非零即可——帧本身一直是固定的两片。

## 42. internal/mockbackend：`durationHeader`、`readAll`、`writeJSON`、`errorBody`

这四个辅助函数 05 册已逐条覆盖，本册只补两处精确细节，并把符号记全以免覆盖清单缺项。

- `durationHeader (internal/mockbackend/mockbackend.go)` —— `func durationHeader(r *http.Request, name string) (time.Duration, error)`。行为：`v := r.Header.Get(name)`；`v == ""` 返回 `(0, nil)`；`time.ParseDuration(v)` 失败 → `fmt.Errorf("%s must be a duration such as \"250ms\": %q", name, v)`；`d < 0` → `fmt.Errorf("%s must not be negative: %q", name, v)`；否则 `(d, nil)`。注释（**源码注释观点**）：不可读或负值是错误而不是 0，因为「没有停滞」是对一个被忽略的头的**貌似合理**的读法——一个本想拖慢后端的验收器反而会得到瞬时回答，于是一个超时场景什么也证明不了。**本册补充**：`time.ParseDuration("300")` 失败（缺单位），所以 `"300"` 是 400；`"+40ms"`、`"1.5s"`、`"40ms"` 都能过。
- `readAll (internal/mockbackend/mockbackend.go)` —— `func readAll(r *http.Request) ([]byte, error)`。行为：`r.Body == nil` 返回 `(nil, nil)`；否则 `defer r.Body.Close()` 并 `io.ReadAll`。**本册补充**：`chat` 忽略它的 error（`body, _ := readAll(r)`），所以一个中途断掉的 body 会以一个 JSON 解析错误的形式表现为 400。
- `writeJSON (internal/mockbackend/mockbackend.go)` —— `func writeJSON(w http.ResponseWriter, status int, v any)`。行为：`Content-Type: application/json`；`w.WriteHeader(status)`；`_ = json.NewEncoder(w).Encode(v)`（错误被显式丢弃）。**本册补充**：它**不做状态码校验**，这是第 39 节里 `strconv.Atoi` 能注入越界状态的前提。
- `errorBody (internal/mockbackend/mockbackend.go)` —— `func errorBody(message, typ string) map[string]any`。行为：返回 `map[string]any{"error": map[string]any{"message": message, "type": typ, "param": nil, "code": nil}}`——OpenAI 形状的错误信封，`param` 与 `code` 显式为 nil。

## 43. internal/mockbackend 测试

`internal/mockbackend/mockbackend_test.go` 的包名是 `mockbackend`（内部测试）。文件头的注释（**源码注释观点**）：验收器把这个后端当**测量仪器**，而一个被它忽略的头就是一台静默地什么都没测到的仪器——一个用不可读 `X-Mock-TTFB` 搭起来的「慢后端」场景会立刻作答，网关永不超时，检查就会对一次「被检查的事情从未发生」的运行报告结果；因此不可读的值是一个**点名该头的 400**，这些测试把这一点以及旋钮顺序（头 > 运行时停滞 > 配置值）钉死。

- `postChat (internal/mockbackend/mockbackend_test.go)` —— `func postChat(t *testing.T, b *Backend, headers map[string]string) (int, string)`。行为：`t.Helper()`；向 `b.URL+"/v1/chat/completions"` POST `{"model":"mock-gpt"}`，设 `Content-Type: application/json`，再把 map 里的头逐个 `req.Header.Set`；用 `http.DefaultClient` 发；`defer resp.Body.Close()`；返回 `(resp.StatusCode, string(body))`。
- `TestUnreadableTimingHeadersAreRejected`：五个子用例——`HeaderDelay` 取 `"300"`、`"soon"`、`"-1s"`，`HeaderTTFB` 取 `"300"`、`"-1ms"`；每个都必须 400，且 body 里必须出现该头的名字。**本册补充（与 05 册的一处差异）**：这个文件里的该测试**只**断言状态与 body，不断言 `/calls` 的计数；05 册描述的「并且 `/calls` 的 `Calls` 必须是 0」属于 `cmd/mockupstream/headers_test.go` 里的**同名测试**（全仓库有两个 `TestUnreadableTimingHeadersAreRejected`）。
- `TestReadableTimingHeadersAreHonoured`：对 `HeaderDelay` 与 `HeaderTTFB` 各发一次 `"40ms"`，都必须 200，且耗时 `>= 30ms`。注释（**源码注释观点**）：停滞不是流式专属的旋钮，它对一个普通 JSON 回答也生效，这才让上游超时场景不必依赖 SSE。
- `TestHeaderStallWinsOverTheRuntimeStall`：`New(Options{Name: "precedence", TTFB: 200 * time.Millisecond})` 后 `b.SetStall(300 * time.Millisecond)`；带 `HeaderTTFB: "20ms"` 时必须 200 且耗时 `<= 150ms`（头赢）；随后不带头再发一次，必须 200 且耗时 `>= 250ms`（运行时值赢过 200ms 的配置 TTFB）。注释（**源码注释观点**）：这条的后半正是「从 `stall(r)` 重构成 `stall(header)` 可能弄坏的那一半」。

## 44. 跨包一览：状态、锁与调用者

- `internal/embed`：`HashingEmbedder` 无共享可变状态，构造后并发安全；`HTTPEmbedder.dims` 是唯一被写后读的字段，**没有锁也没有原子量**；`Fallback` 只持有两个接口与一个回调，无锁；`Scored`/`Rank` 是纯函数（`Rank` 原地排序调用方切片）。
- `internal/evalset`：**没有任何包级变量、没有锁、没有缓存**——`Cases()` 与 `DefaultThresholds()` 每次调用都新建切片；`Measure`/`Sweep`/`Recommend` 都是对入参的纯计算（`Sweep` 只读 `e`）。
- `internal/mockbackend`：`calls` 由 `mu` 保护；`failState`（`atomic.Int32`）与 `stallFor`（`atomic.Int64`）是原子量，专门为了「请求进行中改行为」；`name`/`ttfb`/`delay` 构造后只读；`Backend` 内嵌 `*httptest.Server`，生命周期由调用方 `Close()` 管理。
- 调用关系速记：`embed` 被 `internal/cache/memory.go`、`internal/cache/redis.go`、`internal/server/cache.go`、`internal/evalset/evalset.go`、`cmd/measure-m2/main.go` 使用；`evalset` 被 `cmd/measure-m2/main.go`、`cmd/verify-m2/main.go` 与它自己的测试使用；`mockbackend` 被 `cmd/verify-m1/checks.go`、`cmd/verify-m2/main.go`、`cmd/verify-m3/checks.go`、`cmd/verify-m3/governance.go`、`cmd/verify-m3/governance2.go`、`cmd/verify-m5/checks_traces.go`、`cmd/verify-m6` 的多个检查文件使用。
- 三个包对本册主题的共同点是「数据面」，但它们的性质不同：`embed` 与 `evalset` 是**被测对象与它的量具**，`mockbackend` 是**验收器的量具**；三者的注释都把「量具本身可检视」当成一等需求（`embed` 导出 `Features`、`evalset` 把标签理由写进 `Note`、`mockbackend` 把头解析成 400 而不是静默忽略）。

## 45. 本册覆盖清单

- `internal/embed/embed.go`：包注释；`Embedder`（含 `Embed`/`Dims`/`Name` 三个接口方法）；`HashingEmbedder`；`DefaultHashingDims`；`NewHashingEmbedder`；`(HashingEmbedder) Dims`；`(HashingEmbedder) Name`；`(HashingEmbedder) Embed`；`(HashingEmbedder) vector`；`Features`；`tokenize`；`isCJK`；`hashFeature`；`fnvHash`；`Normalize`；`Cosine`；`IsZero`；`EncodeVector`；`DecodeVector`；`HTTPOptions`（`BaseURL`/`Model`/`APIKey`/`Timeout`/`MaxInputChars`/`Client` 六个字段）；`DefaultMaxInputChars`；`HTTPEmbedder`（`opts`/`client`/`dims`）；`NewHTTPEmbedder`；`(HTTPEmbedder) Dims`；`(HTTPEmbedder) Name`；`embedRequest`（`Model`/`Input`）；`embedResponse`（`Data`/`Index`/`Embedding`/`Model`/`Usage`/`Error`）；`(HTTPEmbedder) Embed` 的请求构造与响应解析；`truncateRunes`；`snippet`；`Fallback`（`Primary`/`Secondary`/`OnFallback`）；`(Fallback) Embed`；`(Fallback) Dims`；`(Fallback) Name`；`Scored`（`Key`/`Similarity`/`Model`/`PromptHash`/`CreatedAt`）；`Rank`。共 35 个声明加 20 余个字段。
- `internal/embed/embed_test.go`：`stubEmbedder`（含 `Embed`/`Dims`/`Name` 三个方法）；`TestIdenticalTextIsExactlyIdentical`；`TestNearDuplicatesBeatUnrelatedText`；`TestWordOrderMattersThroughBigrams`；`TestChineseUsesCharacterBigrams`；`TestVectorIsIndependentOfProcessState`；`TestDimsAndName`；`TestEmptyTextHasNoFeaturesAndNoNaNs`；`TestFeaturesIncludeUnigramsAndBigrams`；`TestCosineEdgeCases`；`TestNormalizeHandlesZeroVector`；`TestVectorEncodingRoundTripsExactly`；`TestHTTPEmbedderPairsVectorsByIndex`；`TestHTTPEmbedderTruncatesLongInput`；`TestHTTPEmbedderErrorsAreExplicit`；`TestHTTPEmbedderHonoursContextCancellation`；`TestNewHTTPEmbedderValidatesOptions`；`TestFallbackDegradesInsteadOfFailing`；`TestFallbackDoesNotCallTheHookOnSuccess`；`TestFallbackWithNoSecondaryReturnsThePrimaryError`；`TestRankSortsByDescendingSimilarity`。共 24 个。
- `internal/evalset/evalset.go`：包注释；`KindIdentical`；`KindParaphrase`；`KindNearMiss`；`KindUnrelated`；`Case`（`ID`/`Kind`/`A`/`B`/`Note`）；`(Case) ShouldHit`；`CaseResult`（`Case`/`Similarity`/`Hit`）；`(CaseResult) Correct`；`Cases`（26 对逐对列出：identical 3、paraphrase 10、near-miss 10、unrelated 3）；`ThresholdResult`（`Threshold`/`TrueHits`/`TrueTotal`/`FalseHits`/`FalseTotal`/`MissedIDs`/`FalseHitIDs`）；`(ThresholdResult) HitRate`；`(ThresholdResult) FalseHitRate`；`Report`（`Embedder`/`Dims`/`Cases`/`Thresholds`）；`Measure`；`Sweep`；`Recommend`；`DefaultThresholds`。共 16 个声明加 11 个字面量对与 20 余个字段。
- `internal/evalset/evalset_test.go`：`sweep`；`find`；`TestCorpusIsWellFormed`；`TestCurveIsMonotonic`；`TestShippedThresholdHasNoFalseHits`；`TestLexicalEmbedderCannotSeparateEverything`。共 6 个。
- `internal/mockbackend/mockbackend.go`：包注释；`HeaderStatus`；`HeaderDelay`；`HeaderTTFB`；`HeaderOmitEnd`；`HeaderUpstreamName`；`Options`（`Name`/`TTFB`/`TokenDelay`/`FailStatus`/`Methods`）；`Call`（`Path`/`Model`/`Stream`/`Authorization`/`Header`）；`Backend`（内嵌 `*httptest.Server`、`name`/`ttfb`/`delay`/`failState`/`stallFor`/`mu`/`calls`）；`New`；`handler`（匿名分发闭包）；`/healthz`、`/v1/models`、`/calls`、`/v1/chat/completions`、`/v1/embeddings` 五个模式与它们的匿名处理器；`(Backend) Calls`；`(Backend) Count`；`(Backend) Reset`；`(Backend) SetFailStatus`；`(Backend) SetStall`；`(Backend) stall`；`(Backend) chat`；`(Backend) streamChat`；`durationHeader`；`readAll`；`writeJSON`；`errorBody`。共 21 个命名声明加 5 个匿名处理器。
- `internal/mockbackend/mockbackend_test.go`：`postChat`；`TestUnreadableTimingHeadersAreRejected`；`TestReadableTimingHeadersAreHonoured`；`TestHeaderStallWinsOverTheRuntimeStall`。共 4 个。
- 本册符号条目合计：`internal/embed` 59、`internal/evalset` 22、`internal/mockbackend` 25（含匿名处理器），共 **106** 条左右。

## 46. 补遗：`internal/stats` 的桶与钳位，以及 `internal/idempotency` 的 `BodyLen`

这一节是 04 册第 38 节（`internal/stats` 概述）的符号级补遗。04 册讲了滑动窗口的用途与 EWMA 的意义，但没有逐条展开桶的实现细节；而路由的延迟策略与熔断的后端评分都直接读这些平均值，所以它们的边界条件值得单独写清。

`internal/stats/stats.go` 的 `bucket`（私有类型）与三个辅助符号：

- `type bucket struct { start time.Time; attempts, successes, failures, timeouts int64; latency float64; latencyN int64; ttft float64; ttftN int64 }`。
- `func (b *bucket) reset(start time.Time)`：`*b = bucket{start: start}`，即整体归零只留时间戳。窗口滑动靠的就是「过期就清空」而不是「留着旧数据参与平均」。
- `func (b *bucket) addAvg(dst *float64, n *int64, seconds float64)`：指数滑动平均的落地。`n == 0` 时直接 `*dst = seconds`（第一个样本就是初值），否则 `*dst += EWMAAlpha * (seconds - *dst)`，然后 `*n++`。常量 `EWMAAlpha = 0.2`（`internal/stats/stats.go`）。计数 `n` 与平均值分开保存，是为了让 `Snapshot` 能区分「没样本」与「样本恰好为 0」——这与 04 册讲过的 `HasLatency` 是同一件事的两面。
- `func (s *Stats) bucketFor(now time.Time) *bucket`：环形桶的定位与复用。索引是 `idx := int(now.UnixNano()/int64(s.step)) % len(s.buckets)`，负索引再补一圈（`now` 可能早于 Unix 零点，测试用假时钟时会遇到）。源码注释（**源码注释观点**）说明后半段检查的意图：一个 `start` 不在「距此刻一个桶宽之内」的桶，要么是绕满一圈后被复用、要么是从没写过；两种情况下它的内容都描述另一段时间，必须丢弃，所以条件写成 `b.start.IsZero() || now.Sub(b.start) >= s.step || b.start.After(now)` 时调用 `b.reset(now.Truncate(s.step))`。桶宽 `s.step = span / buckets` 由 `func New(span time.Duration, buckets int) *Stats` 算出，那里把 `span` 钳到至少一秒、`buckets` 钳到 1…1024（源码注释解释：1 纳秒窗口的环会在每个请求上重置，等于把喂给它的每个统计量静默关掉）。
- `func clampSample(d time.Duration) float64`：把时长换算成秒，并给出上下界。`d < 0` 返回 0（负时长是测量误差而不是「很快」）；`d > maxSample` 返回 `maxSample.Seconds()`（`const maxSample = 2 * time.Minute`）；其余返回 `d.Seconds()`。它的作用是防止一个病态样本把 EMA 拽走——窗口里的平均值会被路由与熔断当成后端的速度，一个 30 分钟的超时样本足以让一个健康后端在接下来一段时间里看起来最慢。

`internal/idempotency/store.go` 里的 `BodyLen`：

- `func (e Entry) BodyLen() int { return len(e.Body) }`，值接收者方法。上方注释原文是「BodyLen is the stored body size, for /admin output.」（**源码注释观点**）。它与 `Entry` 的字段 `Body []byte`（存下来的完整响应体）配对使用：管理面报表只要字节数，不需要把整段响应体搬到 JSON 里。这也是本册所知的唯一读取点语义；它在别处的调用情况本册没有逐一核实。

## 47. 未能确认

以下各条都是本册在源码里**找不到确证**或**无法用测试验证**（工具链不可用，本册未运行 `go build`/`go test`）的判断，按包列出，绝不当作结论使用。

- `Cache.observeDims` 不存在。`NewHTTPEmbedder (internal/embed/embed.go)` 的注释写「一个需要固定宽度的缓存必须从第一个响应里学到它（见 `Cache.observeDims`）」，但对全仓库检索 `observeDims` 只命中这一句注释本身——`internal/cache` 里没有这个函数，也没有其他名字的等价物。这是本册确认度最高的一条：**那是一个悬空引用**。
- `embed.Rank` / `embed.Scored` 的注释与事实不符。`Scored` 上方的注释自称是 `SortedBySimilarity`（这个名字不存在），并说「被 admin 查找端点使用」；而全仓库对 `embed.Rank` 的调用只有 `TestRankSortsByDescendingSimilarity`，admin 查找走的是 `internal/cache/cache.go` 里的另一个 `Rank`（参数 `[]Match`）。这两个类型今天是否仍打算被 admin 端点使用，本册未能确认。
- `HTTPEmbedder.dims` 的并发安全性。源码里没有任何锁或原子量保护它，而 `Embed` 每次成功都写、`Dims()` 无同步地读；本册只能读出「这是一个数据竞争」，**是否在实际使用中被规避**（例如每个请求一个 embedder 实例）未能确认。
- `Fallback.Dims` 的 nil `Secondary` 路径。`f.Primary.Dims() == 0 && f.Secondary == nil` 时会 nil 解引用 panic；`Embed` 与 `Name` 都显式处理了 nil，`Dims` 没有。本册未能确认这是有意的「契约要求 Secondary 非 nil」还是遗漏。
- `HashingEmbedder.vector` 的跨进程确定性有一个理论缺口：不同 token 哈希到同一桶时，float32 累加顺序取决于 Go map 的随机迭代顺序，而源码注释与 `TestVectorIsIndependentOfProcessState` 都断言跨进程稳定。本册无法构造反例（未运行测试），因此只能说「理论上存在，未被证实」。
- `isCompletionPath` 不在 `internal/evalset`。任务书把它列在 `internal/evalset` 下并要求「只认 `/chat/completions`、`/completions`、`/embeddings`」；实际定义在 `internal/gateway/request.go`，`internal/evalset` 里没有任何路径判定代码。这个符号已由 02A 与 02C 覆盖，本册不重复走读。
- `isCompletionPath` 是否真的「只认」那三条路径，本册没有逐字符读它的实现（超出本册范围），只把它所在的文件与已确认的三条路径模式记录在此。
- `Options.Methods (internal/mockbackend/mockbackend.go)` 的行为。它的注释描述了「在模型列表里追加 `methods` 能力」，但 `/v1/models` 返回写死的 `mock-gpt`、`New` 不读它、全仓库没有读取点。本册确认这是**未使用的字段**，但**「这个能力是否在别处（例如某个验收脚本或 `cmd/mockupstream`）以别的字段名实现」未能确认**。
- 「两种路径形状都被注册」。源码注释这么说，但 `mux.HandleFunc` 只注册了 `/v1/chat/completions` 与 `/v1/embeddings` 两个带 `/v1` 的模式，注册表里没有裸形状；同时 `handler` 内部又对路径做 `strings.TrimPrefix(r.URL.Path, "/v1")`。一个不带 `/v1` 的请求究竟是被 `ServeMux` 404 掉、还是能靠某种匹配到达 `handler`，本册未能确认（未运行测试）。
- `X-Mock-Status` 注入越界状态码的后果。本册的读法是 `strconv.Atoi` 接受 `-1`/`0`/`99` 并交给 `http.ResponseWriter.WriteHeader`，而 `net/http` 对这些码是 panic；**本册未运行任何测试来复现这一 panic**，所以只能记为读码推论。
- `/calls` 的 `count` 与 `calls` 长度可能不等的竞态。它来自两次独立的 `b.Calls()` 调用；在没有并发请求时当然相等，本册未能确认是否存在压测下观察到不等的情况。
- `internal/evalset` 的 `(CaseResult) Correct` 没有调用点。全仓库检索 `Correct()` 只命中 `internal/evalset/evalset.go` 的定义处；本册未能确认它是否是从别的仓库/历史版本保留下来的 API。
- 05 册与任务书提到的「`/calls` 的 `Calls` 必须是 0」。`internal/mockbackend/mockbackend_test.go` 的 `TestUnreadableTimingHeadersAreRejected` **没有**这个断言；带该断言的是 `cmd/mockupstream/headers_test.go` 的同名测试。本册未能确认 05 册那句话指的是哪一个（很可能是后者），此处只记录两个同名测试并存这一事实。
- 任务书说 `internal/evalset` 的类型叫 `Pair`、分类常量叫 `Label`。源码里的实际名字是 `Case` 与 `KindIdentical`/`KindParaphrase`/`KindNearMiss`/`KindUnrelated`；`Pair` 与 `Label` 在本仓库中不存在。本册按源码的实际名字写。

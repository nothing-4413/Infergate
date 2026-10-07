# InferGate 验收口径

每条能力“以什么为证据”的清单：左边是要证明的事，右边是断言、单测名或原始数据文件。
本页顶部是七个里程碑的门禁总表；M0–M3 的逐项明细在下方，
M4–M6 的验收与实测段落与各自的启动方式写在一起，见 [USAGE.md](USAGE.md) 的 3.13 / 3.14 / 3.15。

## 门禁总表

| 里程碑 | Go 进程内（`cmd/verify*`） | 真实进程 + curl（`scripts/verify-m*.ps1`） | 原始数据 |
| --- | --- | --- | --- |
| M0 透传与 SSE | 38 | 47 | `baseline/m0-baseline.json` |
| M1 路由与熔断 | 64 | 56 | `baseline/m1-summary.json`（+ 9 条压测逐轮文件） |
| M2 语义缓存 | 103 | 158 | `baseline/m2-summary.json`（+ 语料、阈值扫描、6 条压测逐轮文件） |
| M3 配额治理 | 470 | 323 | `baseline/m3-summary.json` |
| M4 分层与量化 | 877 | 127 | `baseline/m4-summary.json` |
| M5 可观测与压测 | 420 | 211 | `baseline/m5-summary.json`（+ 24 条逐轮文件） |
| M6 幂等/账本/能力 | 381 | 149 | `baseline/m6-summary.json` |
| **合计** | **2353** | **1071** | |
| 管理面鉴权（`access`，不属任何里程碑） | 50 | 44 | 无（证据是 `scripts/verify-hardening.ps1` 的输出） |

M5 的 curl 门从 203 条变成 211 条，加的是 8 条解析器自检（`10.0a`–`10.0h`）：
下面那五条只把 p50/p90/p95/p99/max 互相比较，而**一个恒定值满足其中每一条不等式**。
`verify-m5.ps1` 里原来有一个字面 U+00B5（微秒单位）写在 switch 标签上，
Windows PowerShell 5.1 读无 BOM 的 `.ps1` 时用机器代码页解码，那个标签在本机解成了别的字符，
于是 `42ms` 全部落到 `return -1.0`——五个值都是 -1，五条不等式恒真，
门看起来是绿的却在解析它本该解析的东西之外。现在解析器在比较之前先对已知输入自证。
`baseline/m5-summary.json` 仍是 203 条时跑出来的，所以这里出现过一个数字，
上面这张表里是另一个；两者都能对账，因为它们指的是不同的运行。

合计行的两个数字现在有检查：`internal/repofmt/gatecounts_test.go` 的
`TestDocumentedGateTotalsAreTheSumOfTheirRows` 读出这张表和 README 3.3 那张表的每一行，要求两张表
逐行一致、`合计` 等于它上面各行之和、README / 本文 / RESUME 里每一句重述总数的句子都用同一个数。
加它的原因就是这张表：M5 的 curl 门从 203 条涨到 211 条时改了行、没改合计，于是两个 `合计` 行和五句
重述都停在 1060（203 那一版的正确和），差的正是那 8 条自检。
每一句只引用一个门的地方——`docs/DESIGN.md` 的里程碑条目、`docs/USAGE.md` 的命令块、`docs/RESUME.md`
的小结——由同一个文件里的 `TestEveryQuotedGateCountMatchesTheTable` 逐个对表核，数错哪一行就报哪一行。
Makefile 里每个 `verify-*` target 上方那段 `##` 注释也被它读：注释要正好报出该 target 那一行的数字
（`verify-m6-curl` 的那段还多报一个 curl 列合计），所以 `verify-m5-curl` 停在 203、`verify-m6-curl` 写着
"over 5,000" 那两处才被看见（`35612ff`）；`measure-*` 与 `run-*` 的注释不在其中，它们报的是自己那次
运行打印的合计，是测量结果而不是表里的一行。

文档里的**指路**也有一层检查，都在 `internal/repofmt/citedpaths_test.go`：散文里点到的仓库路径必须真实存在
（围栏代码块是引用而不是主张，所以整块跳过）、引用只点名符号而不写行号、点到的测试名必须真的被树里某个
`func Test...` 定义（`TestCitedTestNamesExist`：本文 M3 表里三个名字从来没有存在过，其中两个改到真名、
第三个的名字本次才补上真测试，`acb8cf2`）、每一处 `§N.M` 章节引用都要是某份文档真的用标题拥有的编号
（`TestCitedSectionsExist`：每一处引用都要落在某份文档真的用标题拥有的编号上；引的是哪份文档看它前面的标签，
没写标签就只要任何一份文档有那个编号——正文里 `§12.5` 常出现在上一句已点名 DESIGN 的地方；点 RFC 的行跳过，
那一节属于 RFC 自己。这一条本次才加，加之前每一处引用都可解析，只是没有东西看着它们）、每一句 Markdown
链接连同它的标题锚点都要能在树里解析。扫描面是 README、`docs` 与 `deploy` 下的 markdown、`configs` 下的
yaml，外加 Makefile、compose 文件、Dockerfile 与 `.github` 下的 workflow。还有一类数字说的是树本身的规模：
`internal/repofmt/inventory_test.go` 的 `TestCitedTreeCountsMatchTheTree` 把散文里的"N 个包"与"N 份…配置"
对着 `cmd`、`internal` 下含 Go 文件的目录数和 `configs/*.yaml` 的份数；这一条不跳围栏块，因为 README 里
那份目录树本身就是在主张今天的树。行号这一条还多走一层：
`TestCommentsCiteSymbolsNotLineNumbers` 读 `cmd`、
`internal`、`scripts` 下 Go 与 PowerShell 文件的注释行（块注释和 here-string 算引用，跳过），因为
`cmd/verify-m3/governance.go` 引 `internal/quota/quota.go` 三行、`internal/gateway/cachepath.go` 引
`internal/gateway/proxy.go` 一行的那两处注释全漂了，而当时没有任何检查看得见它们（`741e985`、`96e5e0f`）。
注释里的**路径**仍然不查：脚本可以正当地点名自己马上要写的文件，这是这些检查分工留下的边界。

README 第一屏那几条量化结论（以及 RESUME 里同一批数字）也有对着记录的一层：
`internal/repofmt/baseline_test.go` 的
`TestHeadlineClaimsMatchTheBaseline` 把那些数字（超时场景的两个中位数、缓存命中率与成本降幅、命中与未命中
路径各自的 P95 延迟、AWQ 的吞吐与倍数、M4 那张逐模型表格里的权重大小、TTFT、端到端、吞吐、显存增量、加载秒数
与 token 重合、逐字一致两列、分层省下的比例、导出的 span 条数、与直连相比的 QPS 差、语料对数与
0 误命中、fail-closed 的 20/20 与上游 0 调用、分层那 16 个请求的 8:8 分流、幂等重放的新鲜与重放延迟及两者的
比值、幂等存储臂的容量与驱逐计数、那 256 条答案的字节下界、并发臂的 16 个客户端与恰好一次上游调用、
重放那 20 对没有打到上游（逐对读数求和为 0）、那 20 对本身与单轮省下的 token、
每 1000 请求成本的两次外推、M3 的预扣/结算/返还/超支账本与两处偏差、预算臂那 40 个请求与它挡下的 39 个、
M0 那张八行压测对照表里每一行的中位数、轮间区间、三个分位与两处首字延迟，
M4 那张量化表在 USAGE 与 DESIGN 里的两处复述、DESIGN 成本段里那次运行用的单价（`in 0.27 / out 1.1`），
以及由两个中位数算出来的倍数）
从 `docs/baseline/` 下那次运行的 JSON 里读回来，按各自文档打印的精度重新格式化一遍，哪一侧漂了都会报。跨文件相加的合计（M1 那次压测的
请求总数与错误数）按和核对，整体被改写也会报。同一个键在一个文件里出现多次而值不同时（`m5-summary.json` 的
`direct_minus_gateway_pct` 每个并发一档），这一条要求用同一个对象上的标记键（`workload`、`label`、`concurrency`、
`stream`、`variant`、`dir`）指名读的是哪一个，而不是靠数组下标——数组里插一行不该换掉读的是谁。M0 那张表按行核：
一行里的每个单元都从同一个 phase 读，整行还按一处措辞核对，行里换掉的是哪个单元都看得见。`docs/USAGE.md`
里还有一张窄一些的同源表（六行、只留中位数与 P95、TTFT P95），那六行各自的措辞也各有一条，所以同一批读数
在三份文档里都被看着。幂等重放那条"上游 0 调用"不是记录里的一个字段，而是那 20 对逐对读数
（`mock_calls_replay`）的和，所以"一对都没有"要先过对数那条条目，才可能成立。M4 那列权重大小不是那张逐模型
表里的字段，而是同一次运行在起 vLLM 之前 stat(1) 出来的 checkpoint 大小
（`environment.models.<variant>.safetensors_bytes`），按十进制 GB（1e9 字节）换成 RESUME 印的两位小数。
同一份单价还多一层：`scripts/verify-m4.ps1` 的省钱核算与 `scripts/measure-m4.ps1` 共用一个读价函数
（`scripts/lib/pricing.ps1`），价从网关实际加载的那份配置文本里读，所以把 `configs/tiered-local.yaml` 里
`local-chat` 的价改到高于 `pricing.default`，"本地比云端便宜"那条断言就会红——改动前它按脚本里写死的
0.27/1.1 算，配置怎么改都绿。
M2 的 `/admin/cache` 那一节走的是同一条路，方向相反：它不再拿脚本里写死的 0.86 / 12 / false / false 去比运行中的
管理面，而是先从这次运行交给网关的那份配置文本里解出这几个值（`scripts/verify-m2.ps1` 的 `Get-CachePolicy`），
再断言运行中的进程报的就是它们——把 `configs/cache-local.yaml` 的阈值改成 0.80，这条断言就报 0.8 并通过；
改动前它按写死的 0.86 比，一处有意的阈值调整会让门禁变红，而网关做的是它被要求做的事。
这条链抓到的第一类是四舍五入：`m0-baseline.json` 里 5496.49 与 1095.45 在 RESUME 与 USAGE 里被印成
5497 与 1096，记录本身没错，是那三处字符串错了。

README 里另外两处量化结论不在这条链上，边界各不相同：Warden 端到端的通过数来自拿另一个 Agent 项目 Warden
对着网关跑的那次运行，它的原始输出不在这个仓库里；注入停顿后的首帧延迟由 `scripts/verify-m0.ps1` 每次现场
量、现场打印（`first data frame at ...`），那个读数不落盘，落盘的是断言本身：首帧不早于停顿，且数据帧分
成多个到达时刻（至少两个 mock 帧间隔）陆续到达——所以文档里那个毫秒数允许随机器浮动，被盯住的是"帧是
陆续到的"这个形状。

这条链不止对着 README：大多数数字在 `docs/RESUME.md` 里也要出现，两份文档是不同时候、不同人被催着改的，
一侧漂了另一侧还在就没人看得见。同一批读数还会以别的形状复述：M4 那张量化表在 `docs/USAGE.md` 里是少了
几列的三行，在 `docs/DESIGN.md` 里是一段散文，两处各有各的措辞条目（整行或整句），所以同一张表在三份文档
里都被看着。少数只属于一份文档的——`1758ms → 700ms` 的端到端 P50 只在 RESUME 上讲，
`13 对近似语料`、`0 误命中`、`上游 0 调用` 这类需要上下文才成立的说法只在 README——由每条 claim 自己列出
该出现在哪些文档里。同一句话在两份文档里措辞不同时（README 的 `18000 个请求` 与 RESUME 的 `18000 请求`），
每个文档各写自己的说法，查的是"这个数还在不在"，不是措辞是否一致。只活在 JSON 里、没人引用的读数不在这条
检查的射程内：那是 JSON 自己的事。RESUME 里其余读数也不在——各自只对着自己的 JSON 看，谁改了都得靠人再
读一遍。

M4 那次运行用的单价表是个例外里的一例：它不是写死在记录里的，`scripts/measure-m4.ps1` 在量测时从
`configs/tiered-local.yaml` 的 `pricing:` 块里解出来再写进 `tiering.cost.prices_usd_per_1e6_tokens`，所以那句话、
记录和随仓库发的配置是同一个数的三份拷贝。前两份由上面这条链拉在一起（DESIGN 那段散文里的
`in 0.27 / out 1.1`），第三份由 `TestM4RecordPricesMatchTheShippedConfig` 拉在一起：配置里改了价、或记录里
改了价，都会报出两边各自的值。（改写这句只检验记录的读法：`scripts/measure-m4.ps1` 从不修改 `configs/`，
它跑的是替换了端口与 URL 的 tmp 副本。）

这条链也钉住了散文里引用的那次运行的墙钟时间（`m3-summary.json` 预算臂的 `wall_s`，RESUME 曾把它写成
4.99s 而记录是 4.11s）。重新跑一次量测就会移动它，届时这条链会失败一次，提醒改写散文里的那个数——
这是有意的：读数是实测的，散文里写的就得是实测的那一个。

管理面鉴权默认关闭，所以 M0–M6 的两条证据链一行都没有覆盖它。它有自己的第三条链：
`scripts/verify-hardening.ps1`，**真进程 + 真 curl + 44 条断言**，见下方「管理面鉴权的证据边界」。
它的数字单独列成一行而不是并进合计——把一条 2026 年才加的安全门混进里程碑总数，
会让"1071"这个从 M0 起就写在 README 里的数字变得不可对账。

那一行的 Go 列数的是**检查点**（源码里 `t.Error*`/`t.Fatal*` 的调用点），不是执行到的断言：
`cmd/verify*` 那套会打印自己跑了多少的计数器不覆盖它，而这张表里其余各行的 Go 数字都是它打印出来的。
两个文件是 40 与 10，合起来 50——`internal/repofmt/accesscounts_test.go` 的
`TestDocumentedAccessChecksMatchTheSource` 把上面这三处说法和源码对着核。这里原本写 41：
两个文件都不是这个数，它也不等于任何一次执行的计数。

两条路径相互独立：Go 门用进程内假上游跑得快、断言密度高；
curl 门编译真二进制、拉真进程、用真 `curl.exe` 打真 socket，证的是“部署起来就是这样”。
两条都绿才算这个里程碑完成。复现命令见 [USAGE.md](USAGE.md) 的 3.4。

### 自动化的那一层（`.github/workflows/ci.yml`）

上表两条路径都是**本机手工跑**的；仓库里自动跑的只有一层，边界写清楚：

| 步骤 | 跑什么 | 状态 |
| --- | --- | --- |
| `go build ./...` | 全仓库编译 | 绿（run 37408647484 step 4） |
| `go vet ./...` | 静态检查 | 绿（同 run step 5） |
| `gofmt -l ./cmd ./internal` | 格式门 | 绿（加入这一步时仓库里有 7 个文件不干净，已一并修好） |
| `go test ./... -count=1 -timeout 20m` | 全部单元/集成测试（Linux，无 `-race`） | 绿（同 run step 7） |
| `go test -race` **逐个包**（`go list ./...` 循环，失败继续跑下一个） | 竞态检测（本机配方见下） | **runner 绿**：run 37408647484 step 9 `go test -race` success（整个 job 2 分 28 秒）；本机同样 36 个包全绿（218 秒，0 条 `DATA RACE`） |
| `.\tools\go.cmd run .\cmd\verify*` | 2353 条 Go 端到端断言 | **runner 绿**：run 37413713212（head `6ec3554`）的 `curl gates` job step 12 `go verify gates (M0-M6)` success —— 这是它第一次在 runner 上跑；本机 31 秒跑完七包、2353/2353、写 marker 0 |
| `.\scripts\verify-m*.ps1` + `verify-hardening.ps1` | 1071 条 curl 端到端断言 + 44 条管理面令牌断言 | **绿**：`curl gates (M0-M6, operator token)` job 在 run 37408647484 上 1 分 55 秒跑完，八个门（M0–M6 + operator token）全部 success——那次还没有 `go verify gates (M0-M6)`，也还没有反制步骤；run 37413713212 上是九个门全 success（head `6ec3554`，多了那个 Go 门，该 job 2 分 15 秒） |
| `scripts/lib/summarize-gates.ps1`（job 内**唯一**不允许失败的一步） | 9 个门（八个 curl + 一个 Go）的 marker 文件都在且都写着 0 | **绿**：run 37413713212（head `6ec3554`）step 13 `every gate must have passed` success，它前面**九个门步骤全部 success**——第一次读齐九个 marker；更早的 run 37410701867（head `e248c7a`）step 12 是只有八个门时的同一结论；本机 5 个用例全过（见下） |
| `scripts/verify-docker-profile.ps1` | 20 条容器画像断言（真二进制、真端口、真 miniredis） | 绿（本机 25 秒，见下「容器的证据边界」） |

**这批结论是 run 37408647484（head `29ed38c`）的**，也就是修好 `ci.yml` 之后的第一个真正执行的 run：
`GET /actions/runs/37408647484/jobs` 返回 2 个 job，两个都是 `success`，`GET /commits/29ed38c/check-runs`
同样返回 2 条（匿名可读，`total_count: 2`）。步级结论对匿名读者可见，所以上面每一行都能被外部核对。

**但步级结论要配一句话读。** 九个门步骤每一个都写着 `continue-on-error: true`，而 GitHub 对它的
定义是：

> The result of a completed step after continue-on-error is applied. … When a
> `continue-on-error` step fails, the `outcome` is `failure`, but the final
> `conclusion` is `success`. —— contexts 参考，`steps.<step_id>.conclusion`

也就是说 **`steps[].conclusion` 对红门同样显示 `success`**。这个 flag 是故意留着的：它让一次红 run
把每个门的结果都报出来，而不是停在第一个。代价是 job 自己没有会失败的步骤，所以必须有一个反制步骤
——`every gate must have passed`，它跑 `scripts/lib/summarize-gates.ps1 -Gates m0,…,hardening,go-verify`，
**唯一**不带该 flag 的一步，靠读 `tmp\gate-<name>.exit`（`scripts/lib/run-gate.ps1` 与
`scripts/lib/run-go-verify.ps1` 写的 marker）来判：
marker 缺失、内容不可解析、非 0，三种都算红，把失败门与日志尾部写进 `tmp\gate-failures.md` 再 `exit 1`。
判据是那个步骤的结论，以及 `tmp\gate-failures.md`；`internal/repofmt` 的 `TestEveryCurlGateCanFailTheJob`
把「每个门都带 flag + 恰好一个 checker 不带」钉在源码上，`TestSummarizeGatesFailsClosed` 则真跑脚本
（本机 5 个用例：全绿 0、一门红 1、marker 缺失 1、marker 不可解析 1、`-Gates` 没点出任何门 1）。
`-Gates` 这个参数名原来是 `-Expect`，改名的原因是它让 `go test` 里的 PowerShell 子进程挂 30 秒被
杀掉（命令行里任何以 `-Ex` 开头的参数都能复现，见脚本头部注释）。

**这条反制步骤的绿路径已在 runner 上走过，红路径只有本机证据。** 它跑过两次：run **37410701867**
（head `e248c7a`）的 `curl gates` job 是 `success`，第 12 步 `every gate must have passed`（`conclusion:
success`）前面八个门步骤也是 success（那次还没有第九个门）；run **37413713212**（head `6ec3554`）的同一个
job 是 `success`，第 13 步同样的结论，而它前面**九个**门步骤（第八个是 `operator token gate`，第九个是新的
`go verify gates (M0-M6)`）全部 success —— 也就是说这个步骤真的在 runner 上跑了、读齐了九个 marker、
并且没有误报。
它**真的会红吗**这个问题只有本机证据：`TestSummarizeGatesFailsClosed` 用合成 marker 让
脚本在「一门红 / marker 缺失 / marker 不可解析 / 没点出任何门」四种情形下都 `exit 1`，并检查它留下的
`gate-failures.md` 里写清了是谁红了。（在 runner 上制造一次红门需要故意弄坏一个门，代价是那次 run 的
其余结论全部作废，所以这里选择记录边界而不是制造证据。）

**第九个门里 M5 的偶发已经定根因并修掉。** 症状是 `cmd/verify-m5` 的
`6.29 the root span reports a derived duration (got 0)`：在本机 20 次连跑里红了 **9 次**。它在 runner 上
没有先例，因为进程内的这七个验证器是这一批才第一次进 CI（run 37371614002 红过的那一步是 **curl** 的 M5
门 `verify-m5.ps1`，那是另一回事——curl 门对时长的断言全是 `-ge 0`，从来不要求为正，见 `3.27`、`5.8`、
`5.11`、`9.8`、`10.4`）。根因不是产品，而是这台机器上 `time.Now()` 的分辨率：连续两次读数在
200000 次里有 199999 次**完全相同**，50ms 窗口里只有 70 个不同读数（≈0.71ms 一个 tick，相邻间隔
0.52–1.61ms），用约 440µs 真实忙工作配对出的 300 个跨度里有 **148 个量成 0**（墙钟与 monotonic 都是）。
mock 后端在一个 tick 之内就答完，跨度就只能是 0，所以 `internal/tracing/trace.go` 的 `Trace.Add` 那条
"调用方没给 `DurationMS`、且 `EndUnixNano > StartUnixNano` 时才派生它，否则留 0" 是**如实反映平台**，
flaky 的是验证器里"必须为正"的那三条断言（`4.7` 的 `/stats` max、`6.29`、`10.9` 的首字延迟）。
修法是**给 mock 加 think time**、而不是弱化断言：`cmd/verify-m5/harness.go` 新增
`mockThinkTime = 20 * time.Millisecond`（高于 Windows 最粗的 15.6ms 定时器），`handle` 在写第一个字节之前
sleep 这么多（健康探针不受影响），因此每个被测请求都真的跨过至少一个时钟步进，三条断言**一条没改**、
总数仍是 420 条，测量也才真的测到了东西。同一类问题在单元测试里也有一处：
`internal/gateway/tracing_test.go` 的 `TestTraceRecordsOneRequestTrace` 曾在一个全量 run 里因为
`root.DurationMS <= 0` 变红、单独跑又绿，它的 mock 后端现在同样 `sleep backendThinkTime`（20ms）。
改后本机连跑 **20/20 全绿**（平均 2.96s，此前 2.6s），`internal/gateway` 整包连跑 3/3 绿；
run 37413713212 的 `go verify gates (M0-M6)` 步骤 success 说明这一包在 runner 上也是绿的。

两个 `go test` 步骤把整份 transcript 写进 `/tmp`，再由 `scripts/ci-summarize-go-test.sh`
捕出失败测试与**完整的 DATA RACE 报告**，写进一个分片文件（`/tmp/ci-summary-*.md`）。普通失败的
**原因行**（go test 在 `FAIL` 下面缩进打印的那一行）同样保留，形状是：

```text
script_test.go:462: snapshot = ...
```

run 37414903323 的摘要当时只说了哪个测试红、没说为什么，过滤器就是原因。这个文件随后被
**读两次、内容完全相同**：append 进该 job 的 **step summary**（`$GITHUB_STEP_SUMMARY`，登录可读），
并由 `scripts/ci-publish-failure-check.sh` **创建一个自己的 check run** 写进它的 `output.summary`（**匿名可读**）。
每一步都带 `if: always()` / `if: failure()`，所以这条路在红的那一次也走得通。

**匿名读法**（无需 token、无需下载）：

```
curl -s https://api.github.com/repos/nothing-4413/Infergate/commits/<sha>/check-runs
```

在那份 JSON 里会多出一个名为 `ci failure: build / vet / test / race` 的 check run，`output.summary`
就是失败测试名与 race 报告。这是 run 37379724217 测定出来的：**创建一个 check run** 与
**修改 GitHub 为本 job 创建的那个 check run** 是两回事，后者会被回收。

**这条通路已经在一次真实红 run 上端到端成立**：run **37414903323**（head `97dade6`，2026-10-06T04:41:33Z）
的 `curl gates` job 十四个步骤全 success（九个门 + 反制步骤），而 `build / vet / test / race` job 的
step 7 `go test` failure、step 9 `go test -race` **skipped**、step 11
`publish the failure summary where anonymous readers can reach it` **success**。匿名
`GET /commits/97dade6/check-runs` 返回 **3** 条，其中 id `112111519009`、名为
`ci failure: build / vet / test / race` 的那条带 `output.summary`，正文里是
`--- FAIL: TestDropClosesWithoutAResponse (0.00s)`、`FAIL github.com/infergate/infergate/cmd/mockupstream 0.038s`
以及其余 21 包的 `ok` 行——**这就是当初那个判据**（`output.summary` 非空且能匿名读到）。在这之前
这里写的是"这条通路本身仍未在真实 CI 上触发过，原因不再是阻塞，而是没人红过"；现在它红过了。

那次红暴露的两处问题都修了：摘要丢掉失败原因行（见上），以及
`TestDropClosesWithoutAResponse` 读计数器是**无序读**——`cmd/mockupstream/main.go` 里那句
`defer cw.record()` 在 `cmd/mockupstream/script.go` 那份 drop 路径 `hijack` + `conn.Close()` **之后**
才记账，客户端观察到连接断掉并不等于服务端已经记完，而 `script_test.go` 是同步读 `/calls`（无网络往返）。
断言值没变（`calls=1 dropped=1 failed=0`），只是改成在 2 秒内轮询等待；本机连跑 300 次（含 `-race`）
仍是绿的，所以这条修复的依据是代码里的先后顺序，不是本机复现。如果下一次 Linux 红 run 给出的是
另一条原因行（例如"want a transport error, got status 200"），那说明还有第二个缺陷，而新摘要会直接说出来。
过滤器本身现在有行为测试：`internal/repofmt/summarize_fragment_test.go` 的
`TestSummarizeKeepsWhyATestFailed` **真跑那个脚本**，喂一份 37414903323 形状的 transcript，要求片段里有
失败测试名、它下面那行原因、包行与完整 race 报告，同时不含"没有结果的日志行"（此前只有字符串检查：
`ci_wiring_test.go` 只断言脚本里还有 `DATA RACE` 字样）。它在没有 `bash`/`awk` 时 skip，Linux runner 两样都有。

**修完之后的两次 run（37416449896 / head `3716f18`、37416672028 / head `b45e601`）两个 job 全 success**，
其中 `build / vet / test / race` job 的 step 7 `go test` 与 step 9 `go test -race` 都是 success——
也就是说这个过滤器测试真的在 Linux runner 上跑了（它没有 skip），`TestDropClosesWithoutAResponse`
的轮询断言也在那里绿了；`curl gates` job 的十四个步骤（九个门 + `every gate must have passed` + 上传）
同样全 success。

在它变成"从未执行"之前，这里曾经写的是"GitHub 侧的一次异常"：从 run 37380607350（head `760c303`）
起，`GET /actions/runs/{id}/jobs` 与 `GET /commits/{sha}/check-runs` 都返回 `total_count: 0`，而更早的
run 37374000997 / 37377940124 / 37379724217 仍分别返回 2 / 3 / 4 个 job；同一批 run 的 `name` 也从
`ci` 变成了 `.github/workflows/ci.yml`（`workflow_id` 仍是 375731182），run 的 HTML 页面被重定向到
commit 页。**那些观察都是真的，那个解释是错的**，原因见下。

真正的原因是 `760c303` 加的那个发布步骤，把标题直接写在了 `run:` 行的裸标量里：

```yaml
run: bash scripts/ci-publish-failure-check.sh "ci failure: build / vet / test / race" /tmp/ci-summary-test.md ...
```

YAML 的裸标量（plain scalar）不允许出现"冒号加空格"，所以整份文件从那一刻起就不是合法 YAML：

```
go-yaml load error in scanner at L185.C66: mapping values are not allowed in this context
```

（用 `docker compose -f .github/workflows/ci.yml config` 复现，它用的就是 Go 的 yaml 库；把那个冒号
换成别的字符，同一个文件立刻解析通过。）**GitHub 无法从解析不了的文件里启动 job**，于是
`760c303` 到 `03655b2` 的每一次 push 都产生一个**零 job 的 run**：`completed/failure`、
`updated_at` 等于 `created_at`、没有任何 annotation、`jobs` 与 `check-runs` 自然是 `total_count: 0`——
而 run 的显示名退回文件路径（`.github/workflows/ci.yml` 而不是 `ci`）正是它给出的唯一线索。

所以那六个提交不是"红了"，而是**什么都没跑**：Linux 门、八个 Windows curl 门、check-run relay，
一个都没执行。这条错误的诊断还进了文档（本节与 README §10），已经改掉；`internal/repofmt` 现在有一道
检查钉住这一类错误——见下。

**修法**：把标题移出 `run:` 行，改用环境变量承载（单引号包住的标量里冒号不歧义）：

```yaml
env:
  CHECK_TITLE: 'ci failure: build / vet / test / race'
run: bash scripts/ci-publish-failure-check.sh "$CHECK_TITLE" /tmp/ci-summary-test.md /tmp/ci-summary-race.md
```

`internal/repofmt/workflow_yaml_test.go` 实现的是那条被违反的规则本身：**.github 下每个 `.yml`/`.yaml`
里，映射值若是裸标量就不得含"冒号加空格"**（引号标量、块标量 `|`/`>` 与其内部内容、流式集合都跳过）。
它不假装是 YAML 解析器——这个模块没有依赖，也不会为一个测试引入一个——但它覆盖了整类错误，而不是
这一个实例；`TestPlainScalarColonDetection` 用真正出问题的那一行证明它非空转。

**修复已由一次真实 run 判定**：push `29ed38c` 之后 run **37408647484** 的 `name` 重新变回 `ci`，
`GET /actions/runs/37408647484/jobs` 返回 **2 个 job**（`build / vet / test / race` 与
`curl gates (M0-M6, operator token)`），两个都 `success`，匿名 `check-runs` 接口同样返回 2 条——
也就是说 `760c303` 之后第一次真的跑了东西，而且跑绿了。当时剩下的唯一未验证点是"relay 那一步只在红的
时候执行，所以红的时候 check run 是否真会被创建仍待一个失败的 run"——run 37414903323 就是那个 run，
判据（`GET /commits/{sha}/check-runs` 里出现名为 `ci failure: build / vet / test / race` 的条目且
`output.summary` 非空）已由它满足，见上文。

**为什么 race 那一项有把握说是绿了**：它在 runner 上红，而本机普通 `go test` 全绿、连 `-count=3`
都无抖动。第一轮修的是两处**测试代码**的共享计数器——`internal/embed/embed_test.go` 的
`gotPath`/`gotAuth`/`gotReq`、`internal/gateway/proxy_test.go` 的 `aHits`/`bHits`/`hits`——都由
`httptest` 的 handler goroutine 写、由测试 goroutine 读。往返一个 socket **不是** race detector
承认的 happens-before 边（`httptest` 只在 `Close` 里等 handler，那已在读之后），所以这类代码在
无 `-race` 时永远是绿的、在有 `-race` 的机器上必红。修法是 `atomic.Int64` 与一把 mutex
（`internal/gateway/failover_test.go` 的 `TestBreakerStopsRoutingToADeadBackend` 里
`primaryHits`/`backupHits` 用的就是 `atomic.Int64`）。同一次还修掉一个真实的生产竞态：
`internal/router/router.go` 的 `Router.rand` 是 `*math/rand.Rand`（文档明示不可并发使用），而每个
请求 goroutine 都会经 `Plan` 走到 `orderWeighted`；现在改用包级 `rand.Float64`/`rand.Intn`。

**但那还不是全部，而当时没有任何办法知道。** 2026-10-19 在这台机器上找到了一个一直存在的 C 编译器
（`C:\msys64\ucrt64\bin\gcc.exe`，15.2.0；另有一套 MSVC 14.44 在 Visual Studio 2022 下），于是
`-race` 在本机可跑。配方已写成脚本，与 CI 同形逐包跑：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\run-race.ps1
```

它做三件事：把 `C:\msys64\ucrt64\bin` 加进 `PATH`、设 `CC`/`CGO_ENABLED`、把 `TMP`/`TEMP` 指向仓库内的
`.gotmp`——**最后一条是关键**，否则 cgo 把 gcc 的输入写进 `%LOCALAPPDATA%\Temp` 被拒
（`cgo: open ...cgo-gcc-input-...: Access is denied.`）。`internal/repofmt/race_recipe_test.go` 钉住
这个脚本仍存在、仍是纯 ASCII 无 BOM、仍设这三个变量、仍在失败时于 stdout 点名包并以非零退出，也钉住
README 与本文仍然指向它：**这配方原本只是一段散文，而散文正是漂移掉的东西。**

按 CI 的形状逐包跑一遍，答案立刻清楚了：**36 个包、35 绿、1 红——`internal/cache`**。报告指向的是
**生产代码**而不是测试代码，而且两侧落在同一行：`Read at 0x00c000480950 by goroutine 86` 与
`Previous write at 0x00c000480950 by goroutine 87` 都记在 `cache.(*RedisStore).Get()` 的
`internal/cache/redis.go` 那一行上——修之前那行是直接写在 `s.stats` 上的自增
（`s.stats.Gets++`，`Search()` 里另有 `Searches++`，扫描循环里还有 `Scanned++`）。
`RedisStore.stats` 是一个裸结构体字段，29 处写入点分布在每个请求 goroutine 上，
而 `/admin/cache` 会并发读它；memory store 一直有一把锁（`internal/cache/memory.go` 的 `MemoryStore.mu`），
Redis store 漏了——很可能因为 Redis 客户端的连接池本身并发安全，看起来周围也就都安全。

修法是 29 处写入全部走一个持锁的 helper（`func (s *RedisStore) bump(f func(st *StoreStats))`），
`Stats()` 在同一把锁下拷贝；回归测试 `TestStoreStatsSurviveConcurrentReaders` 对两种 store 各起
4 个 writer 和 1 个 `Stats()` reader，补上的正是旧 conformance 测试缺的那一环（它并发驱动
Put/Get/Search，但**从没有人一边写一边读 Stats()**，而 /admin 正是这么读的）。

**这条现在有 runner 证据了**：本机逐包跑完全树是 **36 个包、0 个失败、0 条 `DATA RACE` 行、218 秒**，
而修好 `ci.yml` 之后的第一个真 run（37408647484）的 step 9 `go test -race` 是 **success**。
也就是说 `ec83c07` 修掉的那条确实是 runner 一直在报的竞态——这也第一次让"runner 上 race 红"
这件事有了闭环：本机用 msys64 的 gcc 复现并定位到 `internal/cache/redis.go`，修完两边都绿。
需要留意的是这个 run 绿过不代表 race 步从此不抖；`-race` 的结论依赖调度，所以真正的保证是
`internal/cache` 的那把锁，以及那 16 个 goroutine 的回归测试。

四条被测定为死路、不要再试的做法：

1. **`grep` 进步骤日志**：job 日志（`GET /actions/jobs/{id}/logs`）与 **artifact 下载**对匿名读者都返回
   403（原文 `{"message":"Must have admin rights to Repository.", "status":403}`，已匿名验证）。
   只帮到本来就有权限打开日志的人。
2. **`$GITHUB_STEP_SUMMARY`**：渲染在 UI 里，但**不进 check-runs 的 `output.summary`**，也不在匿名
   job 页面 HTML 里（那个页面 221KB，只有 UI 外壳，没有日志正文）。
3. **`PATCH` 本 job 自己的 check run**：写 `output.summary` 返回 2xx（`gh` 需显式 `GH_TOKEN`，否则退出码 4），
   **但 job 一结束该字段就被清空**。run 37377940124 里一个在 base job 之后运行、专门 PATCH 并读回该字段的
   探测 job 证实了这一点：从外部读回来仍是 `null`。
4. **指望步骤清单暴露包名**：`GET /actions/runs/{id}/jobs` 的 `steps[]` 只会给出步骤名（`go test -race`），
   循环体内打印的 `=== <pkg>` 只进日志。所以 race 那一步**按包循环**的理由是"一次 run 报告全部被拒的包"，
   不是"匿名读者能看见包名"。

原始 transcript 仍另存为 `go-test-logs` artifact（下载需认证）。

一个查这类问题的坑记在这里：匿名 GitHub API 是 **60 次/小时**，超了之后
`curl.exe` 返回 `403 rate limit exceeded`，而 PowerShell 的 `ConvertFrom-Json` 会把
那个 JSON 错误体解析成一个**空对象**——于是"没有 check run"和"没有额度"看起来一模一样，
`total_count=0` 会被当成"确实没有"。判读前先看 `GET /rate_limit`，或至少检查 HTTP 状态码。

### 管理面鉴权（`access`）的证据边界

两条独立链，和里程碑一样：Go 进程内（50 个检查点，`internal/server/access_test.go` 40 个 +
`internal/config/config_test.go` 的两个 access 用例 10 个）与真进程 + curl（44 条，`scripts/verify-hardening.ps1`）。
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

### 容器的证据边界

`scripts/verify-docker-profile.ps1` 跑的是「容器画像」这一层：把 `configs/docker.yaml` 里的服务名换成
`127.0.0.1`，用**真二进制、真端口、真 miniredis** 起一遍，断言 20 条（`20 passed, 0 failed`，本机 25 秒）。
它证的是一件事：**那份配置里的每个键都真的被加载器读进去了**。这一条现在是**加载器自己的性质**，
不再是这道门替它兜的底：`internal/config` 走严格解码（`miniyaml.UnmarshalStrict` →
`json.Decoder.DisallowUnknownFields`），键名写错在加载期就被点名拒绝——真二进制上是
`configuration error: config: parse …: miniyaml: decode: json: unknown field "quotas"`、退出码 1，
`configs/docker.yaml` 里 `idempotency:` 那段注释（`capacity` 与 `max_entries` 的区别）就是为此改写的。证据是 `internal/config` 的
`TestLoadRejectsAKeyWithNoFieldBehindIt`（未知键被点名）与 `TestEveryShippedConfigDecodes`（14 份出厂配置
逐份过严格解码），以及 `internal/miniyaml` 的 `TestUnmarshalStrictNamesTheKeyItRefuses`。
在此之前它确实只有真跑一次才能证伪：未知键被静默丢掉，`-check` 照样绿。

它**不证**的是：镜像构建、`docker compose up`、服务名解析、容器网络、卷挂载。Docker 守护进程在本机
从未起过，所以这四条一条都没被执行验证过，判据只能来自一台有 Docker 的机器。

这道门本身在 2026-10-06 修过一轮稳健性，原因值得记住：它曾经在一次 **miniredis 根本没起来**的运行里
报出 `first request is a miss got 'skip'` 与 `the replayed body is byte-identical`——**环境没准备好，
看起来却像被测对象错了**。改动是四处：

1. **前置检查放在最前**：三个端口必须空闲（否则打印占用者与改用端口的参数并退出）、派生配置必须先过
   `-check`、两个 store 必须先接受连接，之后才允许出现任何关于缓存的断言。
2. **失败时打印证据**：转储 gateway 日志的 error/warn/listen 行与两份响应文件的前几行，而不是只报文件名。
3. **`catch` 不再 rethrow**：rethrow 会跳过结尾的 pass/fail 汇总，让读者拿不到计数。
4. **清理按端口认进程**：原来只按 `ExecutablePath` 前缀匹配 `tmp/`，**实测漏杀**过活着的
   `infergate.exe` 与 `miniredis.exe`；现在与「三个端口上的 owning process」取并集。

同一次删掉一个**永远返回空**的 `Get-JobPid`：它问 WMI 要 `ParentProcessId = $job.Id` 的进程，而
PowerShell 作业的 `Id` 是**序列号不是进程号**（实测 `Start-Job` 报 `Id=1`，而子进程的真实父进程是另一个
`powershell.exe -s -NoLogo -NoProfile` 宿主）。它唯一的消费者是一行日志，代价是每个作业 30 次 CIM 查询
——门因此白花了两分多钟。**教训：要认自己启动的进程，就用它占的端口，不要用作业 id 猜父子关系。**

还有一个与代码无关、但会让门说谎的环境条件：**磁盘写满**。`C:` 只剩 0.45GB 时
`go build ./...` 大面积失败，报 `link.exe: resize output file failed: truncate ...\a.out.exe:
There is not enough space on the disk.` 与 `compile: writing output: write .\.gotmp\...\_pkg_.a`。
同一个门在那种状态下要跑 145–164 秒（正常 25 秒），因为链接器在反复重试。判读一个"变慢了"或"红了"的
门之前，先看磁盘余量。

---

## M0 验收口径

| 验收项 | 结论 | 证据 |
| --- | --- | --- |
| 单元 / 集成测试 | 全绿 | `go test ./...` exit 0（gateway / sse / miniyaml） |
| 静态检查 | 全绿 | `go vet ./...` exit 0 |
| Go 端到端 | 38/38 断言通过 | `go run ./cmd/verify` |
| curl 端到端 | 48/48 断言通过 | `scripts/verify-m0.ps1` |
| 字节透明 | 响应体与上游逐字节一致 | `TestPassthroughNonStreaming`、verify 断言 |
| 首字延迟可测 | 注入 250ms 停顿，首帧实测 255–268ms，且现场断言首帧不早于停顿 | curl `--trace-time` |
| 帧增量到达 | 数据帧分成多个到达时刻陆续到达（实测 7 个时刻跨 93ms）；把 mock 的帧节奏压成 0ms 后该断言失败 | curl `--trace-time` |
| `[DONE]` 补齐 | 上游省略时网关补齐 | `X-Mock-Omit-Done: 1` 断言通过 |
| 成本计量 | 采信 provider usage 并按单价折算 | 日志 `cost_usd=`、`infergate_tokens_total`；上游声明但未标价的模型在启动时被点名（`reportUnpricedModels`，default 为 0 时 WARN、非 0 时 INFO、全标价时不打），测试 `TestNewServerReportsModelsItCannotPrice`、`TestPriceBookUnpriced` |
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
| 探测名额不会漏 | 探针被放行后若"什么都没说"（客户端断开、请求没构造出来），名额归还而不是记成失败：否则该后端会被 `half-open probe already in flight` 拒到进程结束 | `internal/breaker` 的 `TestReleaseProbeGivesTheHalfOpenSlotBack`、`internal/gateway` 的 `TestCanceledProbeGivesTheHalfOpenSlotBack`（去掉归还后这条会红，实测报文 `no upstream could serve this request: primary: half-open probe already in flight`） |
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
| 日 token 预算 | 第三次 40-token 请求被拒（`tokens_per_day`），计数器停在已准入的量 | `checkDailyTokenBudget`、`TestDailyTokenBudget` |
| 日成本预算 | 同一套逻辑走微美元账目，拒绝原因为 `cost_per_day_usd` | `checkCostBudget` |
| 每分钟限流 | 第 N+1 次拒绝（`requests_per_minute`），分钟计数不涨 | `checkMinuteRateLimit` |
| 每会话预算 | 会话独立计量，会话缺失时不预扣 | `checkSessionBudget` |
| 降级仍是一次请求 | 200 + `degrade` + 改写模型/上限，预扣保留、结算返还差额 | `checkDegrade`、`TestDegradeWithDowngradeModel` |
| 账本按条目配平 | 混合流量（日 + 会话两个 token 维度、拒绝、缓存命中）下 `reserved − released + overshoot == settled` | verify-m3 `identityOK`、verify-m3-curl 段 11 |
| 拒绝不产生上游调用 | 拒绝时上游调用计数不变（预算挡下的正是 provider 账单） | `checkUpstreamNotCalled`、实测 `provider_calls_prevented: 39` |
| 缓存命中仍占配额 | 命中按 `Usage{Requests: 1}` 结算，token 记 0 | `checkCacheHitSettlesRequestsOnly` |
| fail-closed 是默认 | store 不可读 → 503 `infergate_quota_unavailable`，且 **Redis 连不上时启动失败** | `checkFailClosedOpen`、`TestNewRedisStoreRejectsABadAddress`（store 层）、`TestNewServerFailsWhenQuotaRedisIsDown`（启动层） |
| fail-open 可显式打开 | 同一次故障下 5/5 放行，`store_errors` 照记，且决策计数记 `allow` | `checkFailClosedOpen`、`docs/baseline/m3-summary.json` 的 `fail_open` |
| 键隔离与单射转义 | 配置里 `acme:inc` 与 `acme_inc` 各记各的账 | `checkKeyIsolation`、`TestKeyLayoutAndBucketFormats` |
| 三个观测面 | `/admin/quota`、`/stats` 的 `quota` 块、`/metrics` 七个 `infergate_quota_*` 族三者同源 | `checkSurfaces` |
| 非生成路径不受治理 | `/v1/models` 不带任何配额头 | `checkUngovernedRoutes` |
| 配置校验 | 负值、重复租户、`degrade` 却没有任何杠杆、`anomaly_ratio < 1` 都在**加载期**报错 | `internal/config` 的配额用例 |
| 预算挡下多少实测 | 40 个请求：无预算 40 次上游调用；`tokens_per_day: 280` 时 1 次（97.5% 被拒） | `docs/baseline/m3-summary.json` 的 `budget` |
| 预扣准确度实测 | 12 请求：reserved 2872 / settled 320 / released 2608 / overshoot 56 tokens，恒等式成立 | 同上 `accuracy` |
| 故障实测 | fail-closed 20/20 503 且上游 0 调用；重启 store 后同进程 10/10 恢复 | 同上 `fail_closed`、`fail_open` |

`scripts/verify-m3.ps1` 修过两处**不报告错误**的问题，判定口径一条没变，所以上表仍是 323/323：

1. **失败细节被 PowerShell 静默丢弃**：`Assert-Equal` 原来只声明 `($Label, $Expected, $Actual)`，但有 6 个
   调用点多递了一个细节串——清理断言那句 `no process started by this script survived it` 的 pid 列表、
   四处 `… never reached the provider` 的 before/after 计数、以及派生配置那次发现的变量清单。
   PowerShell 对简单函数的多余位置实参**不报错、直接丢掉**，于是清理断言红了也只会打印
   `expected '0', got '1'`，而 pid 列表——判断"真泄漏还是慢清理"唯一有用的东西——从来没到过操作员眼前。
   现在 `Assert-Equal` 声明 `[string]$Extra = ''` 并把它接在失败行后。证据：把 `$alive` 变异成
   `@(Get-TrackedProcess) + @(Get-Process -Id $PID)` 后重跑，`RESULT: 322 passed, 1 FAILED`，失败行首次
   带出 `powershell pid=109776`；还原后的文件 SHA256 与变异前一致。
   `internal/repofmt/asserts_test.go` 的 `TestAssertionCallsDoNotOutrunTheirHelpers` 钉住这一类：它按**每个
   脚本自己**的 `Assert-*` 声明数核对调用点的位置实参个数（带 `-命名` 参数的调用跳过），判定 1260 个调用
   点、39 个声明；把 `Assert-Equal` 改回 3 参即恰好报出那 6 处。
2. **桶名断言本来会跨 UTC 边界误红**：报告里的 `day`/`minute` 是网关读时钟时算出的桶（`internal/quota` 的
   `dayBucket`/`minuteBucket`），而断言是再读一次 `Get-DayBucket`/`Get-MinuteBucket` 比相等——两次读之间
   跨过一次 UTC 分钟（窗口只有几十毫秒）就会红在时钟上、不是红在产品上。现在两次时钟读数夹住报告请求，
   断言改成"报告的桶是这次探测跨过的桶之一"，两处（内存段与 Redis 段）同形。

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

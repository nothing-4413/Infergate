# 第 07 册 · 构建 · 脚本 · 工具链

## 1. 本册范围与衔接

前六册读的是运行时代码（配置与启动、HTTP 服务、流式与上游、治理路径、可观测）。第 07 册换一个面：这个仓库**怎么被构建、怎么被验证、以及它怎么用测试守住自己的文档**。

本册覆盖 `tools/go.cmd` 与 `Makefile`；`scripts/`（先列全清单，再深读 `scripts/run-race.ps1`、`scripts/lib/summarize-gates.ps1` 与 `scripts/verify-m*.ps1` 的公共模式）；`.github/workflows/ci.yml`；`.gitignore`、`.dockerignore`、`.gitattributes`、`Dockerfile`、`docker-compose.yml`；`cmd/verify` 与 `cmd/verify-m1`…`cmd/verify-m6` 的定位（不展开逐断言）；以及重头戏 `internal/repofmt`——**仓库自检测试包**。

锚点形状：一律「符号名或目标名 + 括号里的真实文件路径」，例如 `TestProseCitesSymbolsNotLineNumbers (internal/repofmt/citedpaths_test.go)`、`make verify-m2 (Makefile)`。**本册不写行号引用**：那不只是文风要求，仓库里有一个测试会让违反它的文档在 CI 变红（见 `internal/repofmt` 一节）。

本册是第 07 册第一分册；`Dockerfile` 的镜像从未在本机构建过这件事、以及各 `cmd/verify-mN` 的逐断言内容，分别归 deploy 册与各里程碑册。

## 2. tools/go.cmd —— 被沙箱逼出来的构建方式

调用方式（脚本注释给出的唯一用法）：`tools\go.cmd <go 参数>`，例如 `tools\go.cmd test ./...`。它是一条**分支**，不是包装器：

1. `setlocal`，然后**先**清空 GOROOT（`set "GOROOT="`）。
2. 探测仓库内的 vendored 工具链 `.\gotoolchain\golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64\bin\go.exe`。
3. 不存在（新检出 / CI runner）→ 直接 `go %*` 并 `exit /b %ERRORLEVEL%`，**不做任何缓存重定向**。
4. 存在 → 设 `GOTOOLCHAIN=local`、`GOCACHE=%~dp0..\.gocache`、`GOTMPDIR=%~dp0..\.gotmp`、`GOMODCACHE=%~dp0..\.gomodcache`，必要时 `mkdir` GOTMPDIR，再跑那份 vendored `go.exe`。

**为什么必须重定向三个缓存**（源码注释观点 + `README.md` 第 4 节）：这台机器上 `go` 不在 PATH；默认 `GOCACHE`（`%LOCALAPPDATA%\go-build`）在沙箱下不可写；预置的模块缓存目录同样不可写；于是三个位置加上临时目录全部搬进仓库内，并被 `.gitignore` 挡住。

**为什么模块缓存里的 toolchain 不能用**（源码注释观点）：`toolchain@v0.0.1-go1.26.8` 那个包是**截断**的，缺 `src\unsafe` 与 `src\runtime`，"cannot compile anything"；仓库内的完整发行版是从模块缓存里的 `golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64.zip` 解压出来的。**推断**：这也解释了第 3 支的存在——`.gotoolchain` 是几百 MB 的产物且被 gitignore，新检出本来就没有它，所以 runner 上走的是"用 runner 自己的 go"。

**为什么第 3 支不做缓存重定向**（源码注释观点）：runner 上 `actions/setup-go` 已经导出 GOROOT 并提供自己预热的缓存（按 setup-go 的 key 命中），把缓存指进仓库反而浪费。

**为什么清 GOROOT 必须在 `if` 之前**（源码注释观点）：GOROOT 曾被无条件钉到 vendored 树，结果 curl-gates job 的第一次失败就是 M0 步骤在五秒内死掉、报 `go: cannot find GOROOT directory`（run 37369217148）；两个分支其实都想要"让 go 从自己的可执行文件推导 GOROOT"，而清空必须在 `if` 之前，因为 `setlocal` 会继承父进程环境，runner 上 `actions/setup-go` 恰好导出了 GOROOT。

**推断**：这个脚本把三件互不相干的事（工具链位置、缓存位置、GOROOT 语义）收进一个 `.cmd`，代价是**所有** Go 命令都被它绑住——`Makefile` 的 `GO ?= tools/go.cmd` 与 CI 的门禁脚本都必须经由它或等价的 `go`。

## 3. Makefile —— 便利封装，不是唯一入口

头部两个决定：`GO ?= tools/go.cmd`（所有 Go 命令默认走 shim），以及"GNU make 只是便利封装，`README.md` 第 3 节给了等价的 PowerShell 命令"；`.PHONY` 一行列全目标。逐组说明：

- **构建与检查**：`all: build vet test`；`build` = `$(GO) build ./...` 加两个 `-o bin/...`（`./cmd/infergate`、`./cmd/mockupstream`）；`vet` = `$(GO) vet ./...`；`test` = `$(GO) test ./...`，**没有 `-race`**。
- **race 配方写在 `test` 旁边**（源码注释观点）：需要 cgo 与 C 编译器，所以不进这个目标；配方是 `$env:CGO_ENABLED='1'`、`$env:CC='C:\msys64\ucrt64\bin\gcc.exe'`、PATH 加 msys64、`$env:TMP=$env:TEMP=(Resolve-Path .\.gotmp).Path`；它同时记下了不设 TMP 时的原话错误：`open ...\AppData\Local\Temp\cgo-gcc-input-<n>: Access is denied.`。**推断**：把 race 排除出 `test` 是刻意的，这样"跑全树测试"在一台没有编译器的机器上仍然成立。
- **Go 门禁**：`verify` = `$(GO) run ./cmd/verify`（注释标注 38 条断言）；`verify-m1`（64）、`verify-m2`（103）、`verify-m3`（470）、`verify-m4`（879）、`verify-m5`（420）、`verify-m6`（381）。
- **curl 门禁**：`verify-curl` = `powershell -NoProfile -ExecutionPolicy Bypass -File scripts/verify-m0.ps1`（48 条）；`verify-m1-curl`（61）、`verify-m2-curl`（158）、`verify-m3-curl`（323）、`verify-m4-curl`（128，注释说明它跑两个 mock tier）、`verify-m5-curl`（211）、`verify-m6-curl`（153，注释说七个门的 curl 行合计 1082 条）。
- **测量**：`measure-m2`…`measure-m6` 各自调 `scripts/measure-m2.ps1` … `scripts/measure-m6.ps1` 中对应的那一个，产出 `docs/baseline/mN-summary.json`；`measure-m4` 的注释说明它需要 WSL 里的 vLLM。
- **起栈**（端口写进注释，方便对照）：`run-fleet`（8080 + 三个 mock 9100/9101/9102）、`run-cache`（8082）、`run-tiered`（8080）、`run-miniredis`（6399，**刻意不是 6379**）、`run-cache-redis`（8083）、`run-quota-redis`（8085）、`run-observability`（18999）、`run-quota`（8084）、`run-miniredis-quota`（6398）、`run-agent`（127.0.0.1:18909）、`run-mock`、`run-gateway`。
- **压测与杂项**：`loadtest` = `-all -c 8,32 -n 1500 -warmup 300 -rounds 3 -out docs/baseline/m0-baseline.json`（源码注释观点：取三轮中位数，因为"共享笔记本上单次跑不可复现"，并指向 `docs/RESUME.md`）；`diag` 是同一个工具加 `-diag`；`clean` 删 `bin`；`help` 打印目标表。

注意 Makefile 里**没有** hardening 与 docker-profile 目标：这两个门禁只由 CI 与文档直接调用。**推断**：Makefile 覆盖的是"逐里程碑的日常动作"，而不是"CI 里跑什么"的镜像——后者由 `scripts/lib` 与 ci.yml 定义。

## 4. scripts/ 全清单

`scripts/` 下 20 个脚本 + 6 个 `lib/` 文件。逐条一句话（只深读结构性的那几个）：

- `scripts/verify-m0.ps1` … `scripts/verify-m6.ps1` —— 七个 curl 门（48/61/158/323/128/211/153 条），编译真二进制、起真进程、用真 `curl.exe` 打真 socket。
- `scripts/verify-hardening.ps1` —— 管理面鉴权（operator token）门，44 条断言；CI 里最后一个 curl 门。
- `scripts/verify-docker-profile.ps1` —— 容器画像：把 `configs/docker.yaml` 的服务名换成本地地址，用真二进制跑 20 项检查（文档记录本机 25 秒）。
- `scripts/measure-m1.ps1` … `scripts/measure-m6.ps1` —— 六个测量脚本，产出 `docs/baseline/mN-summary.json`（延迟与缓存命中、配额、分层与量化、可观测与压测、幂等；m4 需要 WSL + vLLM）。
- `scripts/run-race.ps1` —— race 配方（下一节）。
- `scripts/lib/run-gate.ps1`、`scripts/lib/run-go-verify.ps1`、`scripts/lib/summarize-gates.ps1`、`scripts/lib/run-check.ps1` —— 门禁包装与总闸。
- `scripts/lib/config-scalar.ps1` —— `Get-ConfigScalar` 与 `Get-ConfigListScalar`，被 `scripts/verify-m1.ps1` dot-source：curl 门要读配置里的标量来构造断言，不想引入 YAML 解析器。
- `scripts/lib/pricing.ps1` —— `Get-PricingFromConfig`，被 `scripts/measure-m4.ps1` 与 `scripts/verify-m4.ps1` dot-source：价格不是手写的，而是从 `configs/tiered-local.yaml` 解析出来的（这是 `TestM4RecordPricesMatchTheShippedConfig (internal/repofmt/m4prices_test.go)` 能成立的前提）。
- `scripts/ci-summarize-go-test.sh`、`scripts/ci-publish-failure-check.sh` —— Linux 门的可读性管道（见 ci.yml 一节）。
- `scripts/bench_vllm_quant.py`、`scripts/bench-vllm-quant.sh` —— vLLM 量化基准，M4 测量的外部依赖那半边。

其余脚本只在上面的清单里点名，本册不逐行复述——它们的产品是"量出来的数字 + 一行 `RESULT:`"，结构信息已经由公共模式那一节覆盖。

## 5. scripts/run-race.ps1 —— 一条配方，不是一段文字

签名：`run-race.ps1 [-Package <string[]>] [-Timeout <string>='10m'] [-CC <string>='C:\msys64\ucrt64\bin\gcc.exe'] [-Quiet]`。

它做三件事（`README.md` 第 4 节的同一句话）：把 gcc 所在目录（以及它上一级的 `usr\bin`）前置进 PATH；设 `$env:CC` 与 `$env:CGO_ENABLED='1'`；把 `$env:TMP` 与 `$env:TEMP` 指向仓库内 `.gotmp`。

**源码注释观点**：最后一条是所有人都会漏的那条——不设它，cgo 会把输入写到 `%LOCALAPPDATA%\Temp` 并以 `Access is denied` 失败，而这正是让"这台机器跑不了 -race"看起来像结论的原因。它自己把这句话拆开说过：其中一半是真的（gcc 不在 PATH），结论是假的（`C:\msys64\ucrt64\bin\gcc.exe` 一直在，MSVC toolset 也在）。

错误路径：`Test-Path $CC` 为假 → 打印 `no C compiler at $CC` 与 `race needs cgo; install one or pass -CC <path to gcc>`，`exit 2`；未给 `-Package` 时用 `tools\go.cmd list ./...` 枚举，枚举为空 → `go list returned no packages`，`exit 2`。

并发与管道（源码注释观点）：逐包一次 `go test -race`，捕获输出到变量，**绝不用 `Tee-Object`**——管道会吞掉退出码，让失败的门禁报绿；每个包打印 `ok    <pkg> (<n> DATA RACE line(s))` 或 `FAILED(running): <pkg>`，后者刻意留在 stdout，因为 `GET /actions/runs/{id}/jobs` 只暴露步骤名、从不暴露包名；结尾全绿打印 `race clean: N package(s), 0 failures, M DATA RACE line(s)` 并 exit 0，否则打印失败包清单并 exit 1；它前面还会跑一次 `tools\go.cmd version` 当探针，再打印生效的 `CC`、cgo 状态与临时目录。

**这条配方的历史价值**（源码注释观点）：第一次跑起来就抓到了 `RedisStore.stats (internal/cache/redis.go)` 的并发裸字段访问——一条藏在"永久红、且失败原因无法匿名读取"的 CI 步骤后面的生产代码竞态。

`TestRaceRecipeStaysRunnable (internal/repofmt/race_recipe_test.go)` 现在钉住四个前提：脚本存在且是纯 ASCII、无 BOM（PowerShell 5.1 按本地代码页读无 BOM 的 `.ps1`，一个非 ASCII 字节会变成乱码而不是语法错误）；含 `CGO_ENABLED`、`$env:CC`、`$env:TMP`、`$env:TEMP`、`gcc`；含 `FAILED(running):` 与 `exit 1`；`README.md` 与 `docs/ACCEPTANCE.md` 仍然提到这个脚本（否则报"the documented way to run -race has drifted away from the script that works"）。

## 6. scripts/lib —— 包装脚本与"谁弄红这次运行"

四个包装脚本共享一个约定：**marker 文件** `tmp\gate-<name>.exit`（`<name>` 是 `-Name` 的原文）。

### scripts/lib/run-gate.ps1

签名：`run-gate.ps1 -Path <脚本> -Name <mN|access|hardening>`（两个参数都 Mandatory）。

它把 curl 门禁跑在**子进程**里、捕获全部输出、保留 `$LASTEXITCODE`，然后写三样东西：`tmp\<Name>.log`（用 `[System.IO.File]::WriteAllText` 写——源码注释观点：`Out-File`/`Set-Content` 会带 BOM，把日志首行变成噪声）；`tmp\gate-<Name>.exit`（在报告退出码之前写；写失败不吞——marker 缺失在下游算红，这是安全方向）；行为核对结果。

**为什么需要它**（源码注释观点）：curl 门禁曾是仓库里唯一没有可读证据的东西，第一次红运行只产出一条注解 `Process completed with exit code 1.`，而 job log 需要仓库 admin 权限；另外 `powershell ... | Tee-Object` 拿不到退出码，会让红门禁变绿——所以先捕获进变量再自己退出。

**行为核对**（源码注释观点）：每个门禁结尾都会打印断言条数，早先只是捕获、从不比对；M0 的 curl 门在 af4209f 从 47 涨到 48，两张表都还写 47。现在 `run-gate.ps1` 拿 `docs/ACCEPTANCE.md` 的门禁表核对（里程碑按 `mN` 找行、access/hardening 按脚本名找行，取第三格的 curl 数字），`scripts/lib/run-go-verify.ps1` 拿 `README.md` 的表格核对 Go 侧，`internal/repofmt` 保证两张表相等，而**没有行 = 失败**（改名不能悄悄关掉检查）。

四条问题文案原文：`docs/ACCEPTANCE.md could not be read, so its gate table cannot be checked`；`docs/ACCEPTANCE.md's gate table has no row for <label>`；`the gate exited 0 without printing a RESULT line, so how much it checked is unknown`；`the gate ran <printed> assertions, but docs/ACCEPTANCE.md's row for <label> says <documented>`。有问题时打印 `::error title=curl gate <Name>::<problem>`，并在门禁退出码为 0 时强制置 1。

### scripts/lib/run-go-verify.ps1

签名：`run-go-verify.ps1 -Name <名> -Package <逗号分隔的包列表> [-GoCmd <默认 'go'>]`。它是 `cmd/verify` 与 `cmd/verify-m1`…`cmd/verify-m6` 第一次被接进 CI 的通道。

源码注释观点：这 2355 条断言是两条验收链里更大的那条，却一直没有 CI 步骤，因为它们不需要 curl、不需要 PowerShell、也没有平台相关的东西，只是从没被接上；放在 Windows job 是因为那里已有 Go、transcript 也有地方去；一次跑完七个包、最后统一报告，与 curl 门同理——停在第一个红里程碑就看不出另外六个是否也红。

`-Package` 是**逗号分隔字符串**而不是 `[string[]]`：`powershell -File ... -Package a,b` 会把它整体绑成一个字面元素。`scripts/lib/summarize-gates.ps1` 的 `-Gates` 踩过同一个坑——一版 fail-closed 检查因此报出一个叫 `a,b` 的门禁名。

### scripts/lib/summarize-gates.ps1

签名：`summarize-gates.ps1 -Gates <单个字符串> [-MarkerDir] [-LogDir] [-SummaryPath]`。判定逻辑：marker 缺失 → `no marker file at <marker> -- the gate never reported`；`[int]::TryParse` 失败 → `unreadable marker content '<原文>'`；非零 → `exit <code>`。红时打印 `::error title=gate <name>::<reason>`，并把 `## gate <name> : <reason>` 与日志摘录（先取前 20 行含 fail 的行，再取最后 40 行）追加进 `tmp\gate-failures.md`。

三个刻意细节。其一，局部变量叫 `$gateList` 而不是 `$gates`：PowerShell 变量名大小写不敏感，`$gates = @($Gates -split ...)` 会赋给正在读的参数并静默保留未切分的值；源码注释观点的原话是"这个局部的名字是承重的，不是装饰"。其二，参数不叫 `-Expect`：它曾叫这个名，而这个名字让 `internal/repofmt` 的行为测试挂起——一条 PowerShell 孙进程、命令行带以 `-Ex` 开头的参数、stdout 重定向到文件时，恰好 30 秒后被杀且 transcript 为空，而同一条命令行作为 shell 的直接子进程 0.2 秒完成；`-Ex`、`-Expec`、`-Expect` 和字面 `-ExecutionPolicy` 都能复现，`-Gates`/`-Out`/`-MarkerDir` 不能（**推断**：powershell.exe 把 `-Ex...` 读成了自己 `-ExecutionPolicy` 开关的缩写）。其三，`-Gates` 为空也要先写 summary 再 exit 1——源码注释观点：artifact 必须回答的唯一问题是"为什么是红的"，而第一版在写 summary 之前就 exit 了。

### scripts/lib/run-check.ps1

签名：`run-check.ps1 -Binary -Config -OutFile`（三个都 Mandatory）。实现只有两行：`& $Binary -config $Config -check > $OutFile 2>&1; exit $LASTEXITCODE`。

**为什么它必须独立文件、独立进程**（源码注释观点）：Windows PowerShell 5.1 会把原生命令写到 stderr 的任何东西变成 `NativeCommandError`，而在 `$ErrorActionPreference='Stop'`（调用它的门禁设了这个）下那是终止性错误，于是这个门禁**想观察的"拒绝"会中断门禁本身**；`2>&1` 不顶用，因为它并进的是 PowerShell 自己的错误流，仍然是终止性的。契约刻意乏味：退出码就是二进制的退出码，合并输出在 `-OutFile` 里。

## 7. scripts/verify-m*.ps1 的公共模式

读 `scripts/verify-m0.ps1` 就能看清七个 curl 门共有的骨架（其余六个只在断言家族与阶段数上不同）：

1. **参数**：`[CmdletBinding()] param(...)`，都带默认端口（m0 是 `GatewayPort=18080`、`MockPort=19000`、`TtfbMillis=250`、`TokenDelayMillis=15`，还有 `-KeepRunning`）；`$ErrorActionPreference='Stop'`；`$repo = Split-Path -Parent $PSScriptRoot`；日志写 `tmp\`，二进制写 `bin\`。`TokenDelayMillis` 的注释值得记：它被**显式传给 mock**而不是留给默认值，因为某一节的阈值就是从这个数推出来的。
2. **计数器与断言家族**：`$script:passed` / `$script:failed`，加一组文件内定义的 `Assert-*`（`Assert-True`、`Assert-Equal`、`Assert-Contains`、`Assert-Number`）。`Assert-Contains` 用 `.Contains()` 而**不是** `-like`，源码注释观点：`-like` 是通配符匹配，含方括号的 needle 会变成字符类，`'data: [DONE]'` 因此只能匹配 `data: D`/`data: O`/`data: N`/`data: E`，对一条明明以哨兵结尾的流报了假失败。
3. **`Invoke-Curl` 只捕获 stdout**（`2>$null`）。源码注释观点：5.1 会把原生命令的 stderr 变成 `NativeCommandError`，在继承来的 `'Stop'` 下一条 curl 提示（例如 `Unnecessary use of -X or --request`）就会**在某一节中途把整脚本中止**——那一节曾经就是这样静默死掉的。
4. **`New-BodyFile` 把 JSON 写进临时文件并返回 `@path`**。源码注释观点：这不是装饰——5.1 为原生命令重新引号时会剥掉参数里的双引号，`-d '{"model":"x"}'` 到 curl 手上变成 `{model:x}`，每个 provider 都回 400 `invalid character 'm'`；`--data-binary @file` 才是逐字节发送，而这正是 SSE 请求体需要的。
5. **`Get-MetricSum` 一族**把 `/metrics` 文本里**同一 series 的所有样本**相加。源码注释观点：导出序列按 route/upstream/model/status 打标签，只看第一个样本会得到错的均值——或者在第一行恰好是错误行时得到 0。
6. **收尾**：`finally` 里停掉自己起的进程，最后一行固定形状 `RESULT: <passed>/<total> assertions passed`（`scripts/verify-m5.ps1` 用 `$total`，其余用 `$($script:passed)`）。

**推断**：那个 `RESULT:` 行是"包装脚本存在"的直接后果——没有它，`scripts/lib/run-gate.ps1` 就只能去数 PASS 行。脚本各自分配端口、自己起栈、自己收尾（源码注释观点），所以**并行跑会撞端口**，预期是逐个跑；`scripts/verify-hardening.ps1` 还刻意不给自己的 mock 用 gateway+1 这种相邻端口。

## 8. .github/workflows/ci.yml —— 九步 continue-on-error + 最后一道总闸

两个 job。

### job `base`（name: build / vet / test / race）

`runs-on: ubuntu-24.04`，刻意钉版本而不是 `ubuntu-latest`（源码注释引 runner-images#14748 的迁移公告），`timeout-minutes: 30`；权限只要 `contents: read` 与 `checks: write`，后者只有一个用途：发布步骤要自己创建 check run。

步骤依次是：checkout@v5；setup-go@v6（`go-version-file: go.mod`、`cache: false`——模块没有第三方依赖也没有 go.sum）；`go build ./...`；`go vet ./...`；一个 gofmt 步骤跑 `gofmt -l ./cmd ./internal` 并列文件、非空则退出 1（源码注释：加这个门的时候 cmd/ 与 internal/ 下已经有 7 个文件不是 gofmt-clean，没人发现）；`go test` 用 `set -o pipefail` 加 `tee /tmp/go-test.log`；`summarize go test`（`if: always()`）把它变成 fragment 追加进 `$GITHUB_STEP_SUMMARY`；`go test -race` **逐包一次调用**（`go list ./...` 枚举），打印 `ok(running): <pkg>` 或 `FAILED(running): <pkg>`，随后 `summarize go test -race`（同样 `if: always()`）；最后 `publish the failure summary where anonymous readers can reach it`（`if: failure()`、`continue-on-error: true`）调 `scripts/ci-publish-failure-check.sh`。

那个发布步骤的注释是一段失败史，值得保留（源码注释观点）：第一次 race 失败只产出一条注解 `Process completed with exit code 1`，而 job log 需要 admin 权限；管道给 `grep` 只帮到已经能打开日志的读者；写 `$GITHUB_STEP_SUMMARY` 对已登录读者有效，但**不填 check-runs 的 `output.summary`**，匿名 API 与 job 页面都拿不到；PATCH 那个 job 自己的 check run 一度成功，然后在 job 完成时被抹掉（run 37377940124）；结论是给 job 的 GitHub 自有 check run 打补丁，不是工作流能把东西交给匿名读者的通道；修复是 **POST 一个新的 check run**——run 37379724217 创建的探针在 run 结束后从外面仍可读。

### job `curl-gates`（name: curl gates (M0-M6, operator token)）

`runs-on: windows-2022`，`timeout-minutes: 60`。步骤依次是：checkout@v5 与 setup-go@v6；**九个门禁步骤**，每个都是 `shell: powershell`、`working-directory: ${{ github.workspace }}`、`continue-on-error: true`，分别调 `scripts/lib/run-gate.ps1 -Path .\scripts\verify-m0.ps1 -Name m0` …（一直到 m6）与 `-Path .\scripts\verify-hardening.ps1 -Name hardening`；一个 `go verify gates (M0-M6)` 步骤（同样 `continue-on-error: true`）调 `scripts/lib/run-go-verify.ps1 -Name go-verify -Package ./cmd/verify,./cmd/verify-m1,…,./cmd/verify-m6`；`every gate must have passed`（`if: always()`）——整份作业里**唯一没有该标志、与门禁相关的那一步**，调 `scripts/lib/summarize-gates.ps1 -Gates m0,m1,m2,m3,m4,m5,m6,hardening,go-verify`；最后 `Upload gate logs`（`if: always()`，upload-artifact@v4，`tmp/*.log`、`tmp/**/*.log`、`tmp/gate-failures.md`、`docs/baseline/**.json`，保留 7 天）。

这个设计的因果链（源码注释观点 + 推断）：**为什么九个都允许失败**——为了让**一次红灯报出全部九个门禁的结果**；**代价是什么**——job 的结论不再是那九个步骤结论的"与"，GitHub 文档的原话（ci.yml 与总闸脚本都引了）是 "When a continue-on-error step fails, the outcome is failure, but the final conclusion is success."，而 step conclusion 是仓库外读者唯一能看到的表面；**反过来说**——如果**每个**门禁步骤都带这个标志而没有任何一步被允许失败，那么 1082 条 curl 断言与 2355 条进程内断言可以全红，而 job、run、badge、匿名 jobs API 全报成功；**代价由谁承担**——最后那一步，它读每个门禁写下的 `tmp\gate-<name>.exit`，缺失/不可解析/非零都判红，然后 exit 1，这就是"总闸"的字面意思。

**为什么用文件而不是环境变量或 step output**（源码注释观点）：env 不跨步骤；step output 必须由该步骤自己的 shell 写 `$GITHUB_OUTPUT`，而包装脚本是它的子进程，写不了。**为什么 fail-closed**（源码注释观点）：`-Gates` 点名的门禁**必须**上报，marker 缺失是红而不是"不适用"，它捕获的是三种真实情况——包装器在写 marker 之前就死了、步骤根本没跑到包装器、以及只在一侧改名的门禁；期望清单写在 ci.yml 里，`internal/repofmt` 把两张清单钉在一起。**为什么保留 `-NoProfile -ExecutionPolicy Bypass`**（源码注释观点）：runner 的默认执行策略会拒绝这些文件，而 profile 可能改变行为。

**这个设计被谁钉住**：`TestEveryCurlGateCanFailTheJob (internal/repofmt/curl_gates_test.go)`——它要求每个门禁步骤都带 `continue-on-error`、要求**恰好一个**检查步骤且那个步骤**不带**它、还要求它带 `if: always()`（"否则它恰好在它存在的那个场景里被跳过：更早的某个步骤根本没跑、没留下 marker"），并逐字比对"真正运行的门禁清单"与"`-Gates` 里被期待的清单"。

### 那段"六个 commit 什么都没跑"的历史

`TestWorkflowYAMLIsParseable (internal/repofmt/workflow_yaml_test.go)` 捕获的失败模式：发布步骤的标题里含"冒号+空格"，而它被写在 `run:` 行内；YAML plain scalar 不得含这个形状，于是整个文件不再可解析；GitHub 产出**零 job 的运行**——run 是 completed/failure，display name 退回文件路径而不是 `name: ci`，`updated_at` 等于 `created_at`，什么都不执行。源码注释观点：仓库里所有"这些 run 返回 `total_count: 0`"的笔记，描述的其实是自己的坏文件；修复是把标题移进 `env:`。

## 9. 四个忽略/容器文件各自解决什么

- **`.gitignore`** —— 四类：构建产物（`/bin/`、`*.exe`、`*.test`）；本地运行时状态（`/tmp/`、`*.log`、`*.pid`）；密钥（`.env`、`configs/local.yaml`、`configs/*.secret.yaml`）；四个工具链目录（`/.gotoolchain/`、`/.gomodcache/`、`/.gocache/`、`/.gotmp/`）。最后一类的注释就是"为什么"：它们是几百 MB、可从 `tools/go.cmd` 复现的东西，"must never be committed"。另外 `docs/baseline/*.txt` 与 `docs/baseline/*.log` 被忽略而 baseline JSON 保留——源码注释观点：committed 的 baseline 是证据，其余是运行残渣。**推断**：`configs/local.yaml` 被排除，同时是 `TestEveryCitedRepoPathExists (internal/repofmt/citedpaths_test.go)` 需要一条豁免的原因（文档会教读者去创建它）。
- **`.dockerignore`** —— 解决的是**构建上下文与层缓存稳定性**，不是"小一点"本身：没有这些行，第一次 `docker build` 会把仓库内那几百 MB 本地工具链状态上传给 daemon，再拷进一层。它顺带排掉 `.git`、`.github`、`docs`、`deploy`、`scripts`（注释：编译不需要，而 `deploy/` 是挂进观测容器的，不是烤进网关镜像的）。
- **`.gitattributes`** —— 行尾策略：`* text=auto eol=lf` 让仓库 LF 归一化；`*.cmd` 与 `*.bat` 必须是 CRLF；二进制（`*.exe`、图片）不动。注释里的"为什么"很具体：Windows 检出曾把 `*.ps1` 在工作区转成 CRLF，Windows runner 上的 PowerShell 不在乎，但 bash runner 上的 heredoc 与 `source` 在乎。
- **`Dockerfile`** —— 一个文件服务三个二进制：`ARG CMD` 决定编译 `./cmd/${CMD}`，所以 compose 的三个服务共用一个 stage。`ARG GO_IMAGE=golang:1.26-alpine`（源码注释观点：新 Go 版本的镜像 tag 可能早于或晚于这里可用，所以做成可覆盖的 ARG）；`COPY go.mod` 单独一层（没有第三方依赖，所以不需要 go.sum、也不需要 `go mod download`）；构建行 `CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "${LDFLAGS}"`（源码注释观点：`CGO_ENABLED=0` 不是体积技巧，而是"无依赖静态二进制"这个项目目标本身，也正是 alpine/musl 运行阶段可行的前提——而本机跑不了 `-race` 恰好是相反的原因：race 要 cgo 而没有 gcc）；运行阶段 `alpine:3.20` 加 `ca-certificates`（连真实 provider 的 HTTPS 需要信任库）加 `netcat-openbsd`（源码注释观点：miniredis 说 RESP2、wget 探不了，而 compose 的 `condition: service_healthy` 需要一个真能开 socket 的探针；替代方案是再写第四个 Go 命令，那是更多代码加多一条文档）；非 root `adduser -D -H -u 10001`；`COPY configs` 让 `docker run` 不必挂载；`HEALTHCHECK` 只探 `/healthz`（注释：`/readyz` 是更强的探针，compose 用它）。
- **`docker-compose.yml`** —— 三个默认服务（gateway/mockupstream/miniredis）加 `--profile obs` 的 Prometheus/Grafana。三个"为什么"值得记：端口刻意偏离常用值（18080/19090/13000/16399 而不是 8080/9090/3000/6379），源码注释观点是"这个仓库平时就作为本机进程跑在同一台机器上，验收脚本本来就用 18080/18999"；`depends_on` 用 `service_healthy` 而不是 `service_started`，因为配额默认 fail-closed，**一个连不上 store 的网关会拒绝启动**，所以"容器存在"不够；`/readyz` 被当成"真的能服务"的定义，而 `restart: unless-stopped` 配它就是那个语义。配置以 `./configs:/app/configs:ro` 只读挂载（注释：改配置实验不该需要重建，容器也没资格往仓库里写）。Prometheus 挂的是 `deploy/prometheus/docker.yml` 而不是仓库那份 `deploy/prometheus/prometheus.yml`：注释说明后者打 `127.0.0.1:18999`，在这个 network namespace 里什么都没有。Grafana 开匿名 Viewer 并附一句"别带进任何你控制不了网络的地方"。最后一行说明**没有 named volume**：状态刻意短暂，`docker compose down` 是干净状态——对一个"数字要可复现"的东西这是对的默认。

## 10. cmd/verify 与 cmd/verify-m1…m6 —— 进程内那条链

这七个 `main` 包是 Go 门禁（合计 2355 条断言）。调用方式有三种：`.\tools\go.cmd run .\cmd\verify-mN`（`README.md` 3.3）、`make verify-mN`（`Makefile`）、以及 CI 里的 `scripts/lib/run-go-verify.ps1 -Package ./cmd/verify,…`。

`cmd/verify/main.go` 的包注释是这一族里唯一把"为什么不写 curl 脚本"讲全的（源码注释观点）：curl 在这台机器上做不了 HTTPS（schannel 取不到凭证）且缓冲激进，**因此一个 curl 脚本根本无法测首 token 时延**；Go 客户端能证字节透明（包括让拼接流不再合法 SSE 的那个空行框架），还能测请求与第一帧第一个字节之间的间隔；同一个程序在任何平台的 CI 上都能跑、无依赖。它起**真的** mock 上游与**真的** `server.NewServer`（与二进制同一条装配路径），走真 TCP、真 HTTP 客户端，断言返回的字节；退出码 0 就是门禁通过，失败时打印冒犯的字节并非零退出。检查项包括非流式透传、SSE 字节透明、首 token 时延测量、tool_call delta 累积、给沉默后端补 `[DONE]`、用量与成本记账。

`cmd/verify-m1`…`cmd/verify-m6` 各自的包注释一句话定位：M1 路由策略与熔断、M2 语义缓存、M3 token 与成本治理、M4 分层本地/云端与量化、M5 观测面、M6 幂等/账本/能力发现。

两个观察：`cmd/verify-m6/main.go` 的第一行就是 `package main`，没有像其它六个那样的 `// Command verify-mN is ...` 包注释（它后来长成了 `checks.go`/`harness.go`/`helpers.go` 等多文件结构），这一点无法确认是刻意还是历史遗留；`cmd/measure-m2/main.go` 是 Go 写的测量程序，与 `scripts/measure-m2.ps1` 不是一回事——同一个里程碑的测量有两个实现，本册未展开它们的分工。

**两条链为什么要都有**：Go 门在进程内直接驱动 handler 与真实 HTTP 栈，断言密度高、跑得快、平台无关（2355 条）；curl 门编译真二进制、拉真进程、用真 `curl.exe` 打真 socket（1082 条 curl 断言加 44 条 operator token 断言），证的是"部署起来就是这样"；`README.md` 3.3 的说法是两条路径相互独立。**推断**：前者能覆盖很多分支但绕过进程边界与命令行参数解析，后者覆盖 shell、端口、二进制与真实网络栈却难以做高密度断言——所以它们不是彼此的冗余，而是两种失败的检测器；CI 侧把它们放在同一个 job 里、共享 marker 约定与总闸。

## 11. internal/repofmt —— 用测试守住文档与门禁

这是本册的重点。先说这个包的存在方式：`internal/repofmt/` **只有一个包、只有 `*_test.go` 文件**（14 个，合计约 179 KB），没有非测试源码，所以它不产出任何被别的代码 import 的东西；它的产品是"红或不红"。包注释（`internal/repofmt/repofmt_test.go`）自陈：它持有"没有源码可测"的检查——断言整个仓库的性质而不是某个包的性质，并且自成一个包，以免把构建依赖拖进任何东西。公共设施是 `repoRoot(t)`：从测试工作目录向上找 `go.mod`；源码注释观点：测试以自己包目录为 cwd，硬编码 `..` 会在包移动时坏掉，而失败会长得像格式问题而不是路径问题。下面逐文件讲它在守什么。

### internal/repofmt/citedpaths_test.go（6 个测试，包内最大）

- **`TestEveryCitedRepoPathExists`** —— 散文里引用的每个仓库路径必须真实存在于树里。它是被一个真实错误启动的：`docs/USAGE.md` 让读者跑一条引用某个配置文件的命令，而那个文件从来不存在（真正被建的是运行时在 `tmp/` 里的东西，文档记错了目录）；源码注释观点是那句断言为真、但描述的是没人能复现的一次运行，而"没人能复现"正是这个仓库的证据唯一不被允许的东西。它有一张**显式豁免表**（`scripts/verify-mN.ps1`、`docs/baseline/mN-summary.json`、`configs/local.yaml`），三条各自带理由，而且是**双向检查**：豁免的文件必须仍然不存在（否则报 "the exemption for %s is stale"），引用也必须仍然存在（否则报 "never cited any more"）。**推断**：双向检查是为了让"豁免"不能变成一个永久静音的开关。
- **`TestProseCitesSymbolsNotLineNumbers`** —— 散文里禁止出现"路径后跟数字"形状的行号引用，也禁止"at line N"这种用词说出的形式（后者只在同一行同时点了某个仓库文件时才判）。凭据是具体的：`configs/docker.yaml` 曾把 `internal/config/config.go` 里的三处引用写成三个行号，等有人去查时那三行装的是一个 `Threshold float64` 字段、一个访问令牌切片和 OTLP endpoint——三条引用三条全错，而且没有东西报得出来。源码注释观点还否掉了一个显而易见的方案："只检查那一行是否存在"几乎抓不到东西——文件会长大，偏五十行的引用仍落在文件内，而上面那个失败恰恰是存在性检查会判为没问题的情形。围栏代码块被豁免：引用来的命令输出里的行号是那次输出的一部分，不是"某物现在在哪"的断言。
- **`TestCommentsCiteSymbolsNotLineNumbers`** —— 同一条规则施于 cmd/、internal/、scripts/ 下的 Go 与 PowerShell 文件。源码注释观点：对 PowerShell 它检查**整行**而不只是注释，因为门禁的"注记"就是它打印的字符串；例子是 `scripts/verify-m4.ps1` 关于 `X-InferGate-Capabilities` 的注记说 `internal/gateway/proxy.go` 读在某个行号上，而读取处早就移到了 `(*Proxy).plan`，这条在散文检查与行号检查面前都隐形，直到 f1f26b6。
- **`TestEveryMarkdownLinkResolvesInTheTree`** —— markdown 链接的目标必须在树里解析得开，带 `#anchor` 的还要匹配目标文档标题真正产生的 anchor。源码注释观点：上一个检查只匹配带目录的路径，所以把方括号里的标签与圆括号里的目标都写成裸文件名 `USAGE.md` 的那种链接（目标是相对当前目录的一个裸文件名），其中的 typo 对它完全隐形；这不是推测而是验证过的——把 `docs/ACCEPTANCE.md` 里的目标改成不存在的 `USAG.md` 之后 `TestEveryCitedRepoPathExists` 仍然绿。
- **`TestCitedTestNamesExist`** —— 散文用反引号提出的测试名必须真有一个 `func` 定义。凭据：`docs/ACCEPTANCE.md` 的 M3 表引了三个从未以那些名字存在过的测试（真名是 `TestDailyTokenBudget`、`TestDegradeWithDowngradeModel`、`TestNewRedisStoreRejectsABadAddress`）。**推断**：被改名的测试会让句子留在原地、读起来仍像证据，而 grep 那个名字的读者什么也找不到。
- **`TestCitedSectionsExist`** —— `§N.M` 形式的章节引用必须能在某份文档里找到对应编号的 ATX 标题（这类引用散落在文档与 configs/compose/Dockerfile 的注释里，共 36 处）；命名了 RFC 的行跳过，因为 `RFC 7230 §6.1` 是别人的文档。

### internal/repofmt/citedsymbols_test.go

`TestCitedSymbolsExist`：反引号 token 的第一段如果是 `internal/` 下某个包的包名，那么带包限定的符号名、方法名、或"类型.成员"就必须真的存在。凭据是 docs/DESIGN.md 与 docs/RESUME.md 曾把配置估算字段写给一个其实属于另一个配置类型的对象。源码注释观点讲清了它**诚实地能查什么**：只有带包前缀的形状才判，没有包前缀的"类型.成员"天生有歧义、一概放过——所以某类错误它看不见，而这条边界写在 `docs/ACCEPTANCE.md` 里。假阳性护栏：引用的符号总数少于一个下限就 Fatal（"the pattern or the package index is wrong"），所以模式坏掉时测试报红而不是空转。

### internal/repofmt/workflow_yaml_test.go

`TestWorkflowYAMLIsParseable` 加 `TestPlainScalarColonDetection`：不引 YAML parser，只检查"plain scalar 不得含冒号+空格"这一条被破的规则。源码注释观点：模块没有依赖，不会为测试引入一个；这条规则不需要 parser、对 plain scalar 无假阳性、覆盖整类错误而不是那一个实例。第二个测试用真正弄坏 workflow 的那一行证明检查不是空转，并列出一张"必须保持合法"的形状表（引号标量、`run: |` 块标量、折叠块标量、flow sequence、URL、含 `${{ }}` 的表达式）。

### internal/repofmt/curl_gates_test.go

`TestEveryCurlGateCanFailTheJob` 加 `TestSummarizeGatesFailsClosed`：把"哪个步骤带 continue-on-error""被告知要期待哪些门禁""marker 通道还在不在"三件事钉在一起。源码注释观点给了两条性质：红门禁必须能让 job 失败；**被告知要期待的门禁清单必须就是真正运行的门禁清单**——只在一侧改名会永远报红，反方向更糟（悄悄断开接线的门禁会因为没人在等它而被报绿）。第二个测试真的**跑** `scripts/lib/summarize-gates.ps1` 并断言退出码，五个用例分别是全零、一个非零、一个没上报、marker 内容不可解析、`-Gates` 只有逗号（后者注释说明：一个什么都没验证的检查不得报成功）。它造的每个文件都在 `tmp/` 下而不是 `%TEMP%`；注释解释：本机在 `%TEMP%` 下写入被拒，一个被告知去写却写不成的 PowerShell 子进程会变成四十行关于"其实没问题的脚本"的调试。

### internal/repofmt/summarize_fragment_test.go

`TestSummarizeKeepsWhyATestFailed` 加 `TestMSYSStartupFailureMatchesOnlyTheShellsStartup`：把发布给匿名读者的那段 filter 当**程序**来演练。夹具里含 DATA RACE 段、goroutine 帧、失败测试名以及它下面那行原因；断言这些必须留下，而噪声行必须被过滤掉——源码注释观点的原话是 a summary that keeps everything is a log。第二个测试钉住"重试只对 MSYS2 shell 起不来生效"：只有 `NtCreateDirectoryObject` 或 `0xc0000142` 才算，`exit status 127` 与 summary 工具自己的 usage 行都不算。

### internal/repofmt/ci_wiring_test.go

`TestRedRunPublishesAReadableTranscript`：钉"红运行会发布什么"的形状——transcript 落文件、fragment 进 `$GITHUB_STEP_SUMMARY` 与自建的 check run、artifact 上传、`if: always()` 至少四处、`if: failure()` 存在、发布器必须 POST 而不是 PATCH、可执行行里不得再出现 `grep`。源码注释观点自陈这是"刻意浅"的检查（读两个文件找字符串），因为它保护的不是本模块的行为，而是一个关于"红运行会发布什么"的承诺。

### internal/repofmt/asserts_test.go

`TestAssertionCallsDoNotOutrunTheirHelpers` 加 `TestBindingCountsReadNamedParametersConservatively`：PowerShell 对简单函数会**静默丢弃**多余的位置参数，于是六个调用点把详情字符串交给了只声明三个参数的辅助函数，门禁照常打印 PASS 或 FAIL 而操作员丢掉了详情（本该打印进程清单的断言只打印了 expected/got）。它把每个 `Assert-*` 调用解析回同文件里定义的辅助函数、比较参数个数，并配了"判定调用不得少于 400 个""跳过比例不得超过二十分之一"这类下限护栏——**推断**：这些护栏的作用是让解析器失灵时测试报红，而不是安静地什么都不查。

### internal/repofmt/gatecounts_test.go

`TestDocumentedGateTotalsAreTheSumOfTheirRows` 加 `TestEveryQuotedGateCountMatchesTheTable`：`README.md` 3.3 表与 `docs/ACCEPTANCE.md` 门禁表必须逐行相同、每个合计等于上方各行之和、每一句复述总数的句子复述同一个数，且引用某个门禁行的数字必须等于那一行。凭据：curl 列 47+56+157+323+125+211+149 曾写在 1060 的合计下（M5 那一行长到 211 时合计没跟着改），还有四句话重复这个 1060。源码注释观点讲清了它**不查什么**：某行数字是否真是那个门禁跑的断言数，需要真进程与真端口，那是测量而不是单元测试——能在门禁运行时查的就在那时查（两个包装脚本），这里只了结算术能了结的部分。

### internal/repofmt/baseline_test.go（包内最大，约 1110 行）

`TestHeadlineClaimsMatchTheBaseline`：`README.md` 第一屏那几条量化断言（以及 `docs/RESUME.md` 独有的几条）必须与 `docs/baseline/*.json` 里的读数**按打印出来的样子**一致；标度与小数位数是条目的一部分（写成百分比的那个数存的是小数、写成倍数的是三位小数）。它用 marker 点名"是哪个 variant、哪一步 sweep"，所以加一个 variant 不能悄悄搬走被读的数字。源码注释观点：它不判那次运行好不好、也不判基线是否仍描述今天的代码，只了结"句子与记录说的是同一个数字"；只活在 JSON 里、没人引用的读数归 JSON 管。

### internal/repofmt/m4prices_test.go

`TestM4RecordPricesMatchTheShippedConfig`：把 `docs/DESIGN.md` 引用的目录价、记录里的价格块、`configs/tiered-local.yaml` 三份拷贝钉成同一个数字。`scripts/measure-m4.ps1` 的 `Get-PricingFromConfig` 是在测量时从配置解析的，所以改配置会让"记录"与散文一起描述一次没人能复现的运行。

### internal/repofmt/inventory_test.go

`TestCitedTreeCountsMatchTheTree` 加 `TestReadmeTreeNamesEveryPackage`：散文里"多少个包""多少份随仓库发布的配置"必须等于一次目录列举或一次 glob 的结果；`README.md` 的目录树必须点名每个含 Go 文件的 cmd/ 与 internal/ 目录（`internal/repofmt` 自己曾经在另外二十二个 internal 包都被点名时缺席）。这条检查**读围栏代码块**，与别的散文检查相反；源码注释观点解释得很清楚：README 的目录树是"关于今天的树、却打扮成示例输出"的断言，清空围栏会让恰好那一条无人看守（最初确实只检查了 6 条而不是 7 条）。

### internal/repofmt/accesscounts_test.go

`TestDocumentedAccessChecksMatchTheSource`：管理面鉴权那一行的 Go 证据量（点数而非执行次数）必须与两个源文件里检查点语句的数量一致，且三处句子/表行必须同形。源码注释观点：那个数字曾是 41，而仓库里没有任何东西产出它——两个文件分别有 40 与 15 个检查点，41 既不是它们的和也不是任何一半；表里其它每个 Go 数字都来自 `cmd/verify*`，它们会打印自己跑了多少，这一行不在计数器覆盖之内。为什么数"点"而不是执行次数：表驱动循环让一个点跑很多次，而真正会漂移的是点。

### internal/repofmt/repofmt_test.go 与 race_recipe_test.go

- `TestGoFmtClean` —— 钉住 CI 的 gofmt 门。源码注释观点：把门加进 CI 不足以发现漂移——加门时 cmd/ 与 internal/ 下已有 7 个文件不是 gofmt-clean，没人发现，因为 gofmt 只在编辑那个文件时跑；仓库级性质属于仓库级检查，本地一秒出结果而不是等一次 push 与排队。它显式走 `cmd`、`internal`、`packages` 三棵树的目录名而不是对所有东西跑 gofmt，理由也在注释里：`.gotoolchain` 与 `.gomodcache` 就在同目录下且满是故意未格式化的 Go testdata；`packages/` 是旧布局，列出来而不是假设，以免它重新加回来时悄悄掉出检查。
- `TestRaceRecipeStaysRunnable` —— 见 race 那一节。

## 12. 文档即契约：这个设计值多少

一句话：**`internal/repofmt` 把"文档里写的每件事"变成一份可执行契约——路径、符号、测试名、章节号、链接 anchor、门禁条数、合计的算术、基线数字、树的规模，甚至 CI 接线本身；改了代码却忘了改文档，CI 就红。**

它的经济性在于"哪个数字值得钉"被明确分过工：能在门禁运行时核对的（门禁打印的总数）交给两个包装脚本当场对；需要真进程与真端口的（"这一行真的是那么多断言"）交给测量与记录下来的 run id；单元测试只接能便宜了结的那一部分——算术、拷贝之间的一致性、模式是否还匹配得上。

每个检查几乎都带一个"分母护栏"（至少要找到多少条引用、多少个文件、多少个测试名），源码注释反复说明理由：**一个悄悄停止匹配的模式会让被检查的东西无人看守，而测试仍然报成功**。这是这个包最值得学的一条设计，也是它唯一真正的失效模式。

## 13. 未能确认

- **未跑 `go test`**：本会话的 go 需要下载 1.26 toolchain，被沙箱拒绝，所以 `internal/repofmt` 的 25 个测试以及本册对它们的描述都没有经过执行验证；本册读的是源码与注释，自查只用正则人工核对（见回执）。
- 本册没有逐行读 `scripts/measure-m3.ps1`、`scripts/measure-m5.ps1`、`scripts/measure-m6.ps1`、`scripts/verify-m3.ps1` 的内部断言，只读了它们的公共骨架与打印行；这些脚本的逐断言内容属于各自里程碑那一册。
- `internal/repofmt/gatecounts_test.go` 的 `TestEveryQuotedGateCountMatchesTheTable` 的完整条目表、`internal/repofmt/asserts_test.go` 的 `TestBindingCountsReadNamedParametersConservatively` 的用例、`internal/repofmt/baseline_test.go` 我列出的条目之后的后续条目，只读到开头部分，未逐条确认。
- `Dockerfile` 的 `docker build` 从未在本机执行过（`README.md` 3.5 与 `deploy/README.md` 自陈"镜像从未被构建过"），所以构建阶段的实际行为（alpine 上 `CGO_ENABLED=0` 的链接、镜像 tag 可用性）未经验证。
- `tools/go.cmd` 的两条分支我读的是脚本正文与它自己的注释；`.gotoolchain` 内那份完整发行版是否与模块缓存里的 zip 逐字节一致，本册没有核对。
- ci.yml 注释里的历史 run 编号与注解文本来自源码注释，我没有联网核对 GitHub 上的实际 run。
- `cmd/verify-m6/main.go` 缺少包注释这一点，我无法确认是刻意还是历史遗留，本册不作判断。
- `cmd/measure-m2/main.go`（Go 写的测量程序）与 `scripts/measure-m2.ps1` 的分工我只看到"两者都存在"，没有读前者正文，不确认它们是否覆盖同一批指标。
- `scripts/verify-docker-profile.ps1` 与 `scripts/verify-hardening.ps1` 的逐断言细节本册只读了它们的骨架函数名与文档里的条数声明（20 项 / 44 条），没有独立复算。

## 14. 本册覆盖清单

**本册逐符号覆盖的单元：**

- `tools/go.cmd`、`Makefile`
- `scripts/` 全清单，以及 `scripts/run-race.ps1`
- `scripts/lib` 的 `run-gate.ps1`、`run-go-verify.ps1`、`summarize-gates.ps1`、`run-check.ps1`
- `scripts/verify-m*.ps1` 的公共模式
- `.github/workflows/ci.yml`
- `.gitignore`、`.dockerignore`、`.gitattributes`、`Dockerfile`、`docker-compose.yml`
- `cmd/verify` 与 `cmd/verify-m1`…`cmd/verify-m6` 的定位（不展开逐断言）
- `internal/repofmt` 及其各测试文件的检查意图

**本册明确没有覆盖的东西：**

- `cmd/loadtest`、`cmd/measure-m2`、`cmd/miniredis` 的逐符号走读：属第 06 册。
- `scripts/measure-m3.ps1`…`measure-m6.ps1`、`scripts/verify-m3.ps1` 等的逐断言内容：本册只给公共骨架。
- `internal/` 下除 `internal/repofmt` 之外的库代码：属第 00–05 册。
- `deploy/` 下观测栈配置的逐字段走读：属第 06 册。
- 本会话未跑 `go test`：`internal/repofmt` 的自检测试没有执行验证，本册结论来自源码与注释的静态阅读。

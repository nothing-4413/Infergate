# 第 10 册 · 仓库自检的辅助符号（internal/repofmt）

## 1. 本册的范围与读法

本册是「逐符号源码走读」的第 10 册。主角不是产品代码，而是 `internal/repofmt` 这个包里的一批**辅助符号**——收集器、解析器、断言工具。它们本身不被任何东西调用，只在测试内部互相调用；但仓库里「文档写下的每一件事」之所以能变成**机械可判定**的契约，靠的正是它们。

`internal/repofmt/` 下没有非测试文件：目录里只有 14 个 `*_test.go`，包名一律 `package repofmt`。这 14 个文件是：

- `accesscounts_test.go` ([internal/repofmt/accesscounts_test.go](../../internal/repofmt/accesscounts_test.go))
- `asserts_test.go` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))
- `baseline_test.go` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))
- `citedpaths_test.go` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))
- `citedsymbols_test.go` ([internal/repofmt/citedsymbols_test.go](../../internal/repofmt/citedsymbols_test.go))
- `ci_wiring_test.go` ([internal/repofmt/ci_wiring_test.go](../../internal/repofmt/ci_wiring_test.go))
- `curl_gates_test.go` ([internal/repofmt/curl_gates_test.go](../../internal/repofmt/curl_gates_test.go))
- `gatecounts_test.go` ([internal/repofmt/gatecounts_test.go](../../internal/repofmt/gatecounts_test.go))
- `inventory_test.go` ([internal/repofmt/inventory_test.go](../../internal/repofmt/inventory_test.go))
- `m4prices_test.go` ([internal/repofmt/m4prices_test.go](../../internal/repofmt/m4prices_test.go))
- `race_recipe_test.go` ([internal/repofmt/race_recipe_test.go](../../internal/repofmt/race_recipe_test.go))
- `repofmt_test.go` ([internal/repofmt/repofmt_test.go](../../internal/repofmt/repofmt_test.go))
- `summarize_fragment_test.go` ([internal/repofmt/summarize_fragment_test.go](../../internal/repofmt/summarize_fragment_test.go))
- `workflow_yaml_test.go` ([internal/repofmt/workflow_yaml_test.go](../../internal/repofmt/workflow_yaml_test.go))

第 07 册（[07-loadtest-measure-deploy.md](07-loadtest-measure-deploy.md)）已经逐条讲过这批文件里的 25 个测试函数：每个测试在断言什么、为什么存在、历史上的红是什么样子。本册**不重复**那一层，只补 25 个测试函数调用到的那批辅助符号，回答「这些自检究竟怎么工作」。第 06 册（[06-repo-selfchecks-ci.md](06-repo-selfchecks-ci.md)）讲命令、部署与工具链的形状，与本册的交集只有两处：`scripts/` 下的门禁脚本，以及 `.github/workflows/ci.yml`——它们在本册里不是被执行的程序，而是被**当作数据解析**的文本。

读法上有一个必须记住的前提：`docs/WALKTHROUGH/` 里的 `.md` 属于 `proseSurfaces` 的判定面（`docs/` 整棵树，只跳过 `baseline`）。也就是说，**本册自己也在它讲的这套检查里**：本册写下的每一个仓库路径都会被 `internal/repofmt/citedpaths_test.go` 逐条 `os.Stat`，每一个反引号里的 `包.符号` 都会被 `internal/repofmt/citedsymbols_test.go` 拿去和 `go/ast` 索引比对。这不是修辞，是本册能被写出来的前提。

## 2. 通用入口：`repoRoot` 与 `readFile`

`repoRoot` ([internal/repofmt/repofmt_test.go](../../internal/repofmt/repofmt_test.go))

- 签名 `func repoRoot(t *testing.T) string`，定义在 `internal/repofmt/repofmt_test.go`。
- 行为：取 `os.Getwd()` 作为起点，逐级向上检查 `filepath.Join(dir, "go.mod")` 是否存在；命中就返回该目录。走到文件系统根仍没找到，则 `t.Fatalf("no go.mod found above %s", dir)`。
- 调用者：本包全部 14 个测试文件里的每一个测试，几乎都是第一句 `root := repoRoot(t)`。它是这个包唯一「所有测试都依赖」的符号。
- 误报/漏报边界：它用**测试进程的当前目录**而不是源码文件位置做起点。`go test` 从包目录运行，所以向上一定能撞到仓库根的 `go.mod`；但如果哪天这个包被复制到另一棵树下、或测试被从别处驱动，它会一路找到别的 `go.mod` 或直接 Fatal。它不缓存，每次调用重新走一遍——代价可忽略，换来的是没有跨测试的隐藏状态。

`readFile` ([internal/repofmt/ci_wiring_test.go](../../internal/repofmt/ci_wiring_test.go))

- 签名 `func readFile(t *testing.T, path string) string`，定义在 `internal/repofmt/ci_wiring_test.go`，开头 `t.Helper()`。
- 行为：`os.ReadFile(path)`；出错则 `t.Fatalf("reading %s: %v", path, err)`；成功则返回 `string(b)`。
- 调用者：本包最热的辅助符号。文档、脚本、workflow、基线 JSON 的读取全部经过它——`citedpaths_test.go`、`citedsymbols_test.go`、`gatecounts_test.go`、`baseline_test.go`、`curl_gates_test.go`、`inventory_test.go`、`workflow_yaml_test.go`、`m4prices_test.go` 都在用它。
- 误报/漏报边界：它**不做任何编码转换**，读到的字节直接当 UTF-8 文本用，也没有 BOM 剥离。这正是本仓库对「所有被读文件必须是 UTF-8」这条硬约束的技术落点：一个 GBK 文件在这里不会报错，而会在下游把它变成乱码匹配失败，表现为一条看不懂的断言红。它失败时用 `Fatalf` 而不是 `Errorf`——文件读不出来，后面所有断言都失去意义。

## 3. 引用路径检查的辅助符号

这一节对应 `TestEveryCitedRepoPathExists`，即「散文里点到的仓库路径必须真实存在」。

`proseSurfaces` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))

- 签名 `func proseSurfaces(t *testing.T, root string) []string`，`t.Helper()`；返回绝对路径切片，末尾 `sort.Strings`。
- 第一段是四个**固定表面**：`README.md`、`Makefile`、`docker-compose.yml`、`Dockerfile`。每一个都先 `os.Stat`，缺一个就 `t.Fatalf("%s is gone, so this check no longer covers it: %v", name, err)`。这条护栏的意思是：把 `Makefile` 改名成 `GNUmakefile`，检查**不能**静默地少看一个文件。
- 第二段是四棵树，每棵树声明目录、扩展名、要跳过的子目录名：
  - `.github` / `.yml`、`.yaml` / 不跳过；
  - `docs` / `.md` / 跳过 `baseline`；
  - `deploy` / `.md` / 不跳过；
  - `configs` / `.yaml` / 不跳过。
  每棵树先 `os.Stat` 目录本身，再 `filepath.Walk`；`found == 0` 就 Fatal。扩展名比较用 `strings.EqualFold(filepath.Ext(path), ext)`，所以大小写不敏感。
- `docs/baseline/` 被排除的理由写在源码注释里：那些 JSON 是**一次已经发生过的运行**的记录，不是对今天这棵树的描述；改掉其中的一处引用就是伪造记录。
- 调用者：`TestEveryCitedRepoPathExists`、`TestProseCitesSymbolsNotLineNumbers`、`TestCitedSymbolsExist`、`TestCitedSectionsExist`、`TestCitedTreeCountsMatchTheTree`。它是「散文面」这个概念的唯一定义处。
- 误报/漏报边界：它只按**扩展名**收文件，不看内容。因此 `docs/` 下任何 `.md`（包括 `docs/WALKTHROUGH/` 里的分册）都会进入判定面；而 `docs/` 下的非 Markdown 文件不会。想要让一份文档脱离全部散文检查，唯一办法是把它放到不被遍历的目录里。

引用路径的正则本身（内联在测试函数里，不是包级符号）值得单列，因为它的判定面就是「什么算一处引用」：

```text
(?:^|[^\w./\\-])((?:\.\\)?(?:cmd|internal|scripts|configs|deploy|docs|tools|packages|\.github)[\\/][\w.\\/-]*\.(?:yaml|yml|ps1|sh|go|json|md|py|txt|conf|exe))
```

- **前缀目录是必须的**。`cmd`、`internal`、`scripts`、`configs`、`deploy`、`docs`、`tools`、`packages`、`.github` 九者之一必须出现在路径开头（可带一个 `.\` 前缀），所以**裸文件名根本不在判定面内**：写「`USAGE.md` 里说……」不会被判成一处仓库路径引用，只有 `docs/USAGE.md` 才会。
- 前置边界字符 `(?:^|[^\w./\\-])` 是「匹配后丢弃」的替代品：Go 的 `regexp` 是 RE2，没有 lookbehind，所以源码把边界字符也吃进匹配、再用子匹配索引取第 1 组。作用是不把 `https://github.com/example/docs/walkthrough.md` 这种 URL 读成仓库路径——URL 里那个文件名前面紧挨着斜杠，而斜杠本身就落在被排除的边界类里，于是整段都不进入判定面。
- 匹配到的路径先做两步归一：反斜杠 `\` 全部换成 `/`，再 `strings.TrimPrefix(..., "./")`。所以 `docs\USAGE.md` 与 `./docs/USAGE.md` 都会被当成 `docs/USAGE.md` 去 `os.Stat`。
- `.md` 文件在匹配前先过 `withoutFencedCodeBlocks`，**围栏代码块里的伪路径无害**——这也是为什么文档里可以放心用代码示例写不存在的路径。

**豁免名单**（`exempt := map[string]string{...}`）当前恰好三条，每条都带一句自己为什么该被豁免：

- `scripts/verify-mN.ps1`——README.md 把里程碑闸门写成 `mN`，本就不存在这样一个文件；
- `docs/baseline/mN-summary.json`——docs/RESUME.md 里同一种占位形状；
- `configs/local.yaml`——`.gitignore` 把它挡在树外，而 `configs/infergate.yaml` 告诉读者自己去创建它。

**「双向校验」具体指什么**：正向是循环里对每条引用做 `os.Stat`，命中豁免名单就跳过、并把 `used[path] = true`。反向是循环结束之后对 `exempt` 的**每一条**做两件事——

1. 如果这个文件现在**存在**了，报 `the exemption for %s is stale: the file exists now (%s)`：豁免是给「本就不该存在的文件」的，文件一旦真的出现，豁免就从「说明」变成了「掩护」，必须删掉。
2. 如果本轮扫描中这条豁免**从未被引用**（`!used[path]`），报 `the exemption for %s is never cited any more (%s)`：豁免不能再被滥用成一张长期通行证——引用被删掉之后，豁免条目本身也要跟着删。

一个 `map` 同时被「删不得」和「用不得」两个方向夹住，才叫双向。地板是 `total < 100` 就 Fatal（一个什么都没匹配到的检查是因为错误的原因通过的）。

`withoutFencedCodeBlocks` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))

- 签名 `func withoutFencedCodeBlocks(text string) string`。
- 行为：按 `\n` 切行；对每一行先 `strings.TrimSpace`，若以三个反引号 ```` ``` ```` 开头就翻转 `inFence` 并向输出写一个 `\n`；围栏内（`inFence` 为真）一律只写 `\n`。
- 关键性质：**它保留全部换行**，被抹掉的行仍然占位。于是输出与输入的**字节偏移和行号仍然对齐**——上层报错时能用原文本偏移反推行号，不会指到别处。
- 误报/漏报边界：它只认「行首（去空白后）以三个反引号开头」，不校验反引号数量是否配对、不校验闭合围栏是否有语言标注。一个少写了闭合围栏的文档会把后面整段真内容也当成围栏内容而漏检——是**漏报**方向，不会误报。它对非 `.md` 文件根本不会被调用（调用点在 `strings.EqualFold(filepath.Ext(file), ".md")` 判断之后）。
- 调用者：`TestEveryCitedRepoPathExists`、`TestCitedSymbolsExist`、`TestCitedSectionsExist`、`TestCitedTestNamesExist`，以及 `TestProseCitesSymbolsNotLineNumbers` 与 `TestCommentsCiteSymbolsNotLineNumbers` 里的 `.md` 分支。

`withoutHereStrings` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))

- 签名 `func withoutHereStrings(text string) string`。
- 行为：按行处理，先用 `out := make([]string, len(lines))` 建一个**行数相同**的空白数组（同样保持行号对齐）。`terminator` 非空时，若该行 TrimLeft 后以 terminator 开头就清空 `terminator`，然后无条件 `continue`（该行整行丢弃）。开启条件是：把行 `strings.TrimRight(line, " \t")` 之后后缀为 `@"` 或 `@'`，并且 `at := len(trimmed) - 2` 位置的字符前面（`at == 0` 或 `trimmed[at-1]` 是空白/`=`）说明这个 `@` **起了一个词**；随后把 terminator 设为 `` "@ `` 或 `'@`，该行也丢弃。
- 为什么需要它：PowerShell 的 here-string 里可以合法地写任何东西，包括形如仓库路径的字符串；`.ps1` 文件的「注释面」检查要把整行都当注释读，不剥掉 here-string 会把示例路径读成真引用。
- 误报/漏报边界：只有 `@"` / `@'` 两种定界符，**双引号字符串不享受豁免**；bash 的 `"$@"` 因为 `@` 前面是 `"`（不是空白或 `=`）而不触发。若 here-string 的结束符那行带了尾随内容，`terminator` 被清空、该行被丢，之后的行重新参与检查——不会污染后续。

## 4. 引用行号检查的辅助符号

这一节对应 `TestProseCitesSymbolsNotLineNumbers` 与 `TestCommentsCiteSymbolsNotLineNumbers`：散文与注释只许点符号，不许点位置。

- `fileMention` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))：`var fileMention = regexp.MustCompile(`[\w.\\/-]+\.(?:go|md|ps1|sh|yaml|yml|json)`)`。它是**分母**：散文里提到的任何仓库文件名（不带行号、不要求目录前缀）。`TestProseCitesSymbolsNotLineNumbers` 用它累加 `total`，地板 `total < 50` 就 Fatal——防止「没提到任何文件」这种形式上的通过。
- `fileLineCitation` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))：`path/file.go:NNN` 形状。前置字符同样的「匹配后丢弃」写法（RE2 无 lookbehind）。它是误报的主力路径：任何人写下 `internal/repofmt/accesscounts_test.go` 后面跟一个冒号加数字，就会被点名。
- `spelledOutLineNumber` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))：`(?i)` 之外的 `\bat lines? \d+`，专抓**写成文字的行号**——`file.go:NNN` 那种形状看不见的。英文写法之所以要单独一条，是因为历史事故正好长这样。
- `linePinPhrase` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))：签名 `func linePinPhrase(line string) string`。先 `if !fileMention.MatchString(line) { return "" }`，命中才返回 `spelledOutLineNumber.FindString(line)`。也就是说它只在**同一行同时点了仓库文件**时才判定。这个条件来自事故：`scripts/verify-m4.ps1` 里关于某个能力头的一条说明把 `internal/gateway/proxy.go` 写成「at line N」形式，而读取位置已经移到了别的方法上，这句话在两道检查面前都漏掉了，直到后来才被补上。误报边界是「同一行」——跨行的文件名与行号短语不会被判。
- `commentSurfaces` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))：签名 `func commentSurfaces(t *testing.T, root string) []string`。对 `[]string{"cmd", "internal", "scripts"}` 三棵树 `filepath.WalkDir`，按小写扩展名收 `.go` 与 `.ps1`，末尾 `sort.Strings`。它是「注释面」的定义处：**不含 `docs/`**，所以文档不算注释面。调用者：`TestCommentsCiteSymbolsNotLineNumbers` 与 `TestCitedTestNamesExist`（后者从这批文件里筛 `_test.go` 收集 `func Test...` 定义）。
- `lineAround` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))：签名 `func lineAround(text string, offset int) string`。`start := strings.LastIndexByte(text[:offset], '\n') + 1`；`end := strings.IndexByte(text[offset:], '\n')`；`end < 0` 时返回 `text[start:]`（最后一行的收尾），否则返回 `text[start : offset+end]`。它返回**包含该偏移的那一整行，不含换行**。它是「就近上下文」这个概念的原子操作，被 `TestCitedSectionsExist` 用来找 `§` 引用的标签。

两条检查的判定面差异值得记下：`.ps1` 文件**整行都算**（门禁脚本里的 `Add-Note` 字符串会被读者当注释读，所以整行都是承诺），而 `.go` 文件只算**首个非空字符是 `//` 的行**；块注释完全不在范围内。`.ps1` 还会先过 `withoutHereStrings`。

## 5. 相对链接与章节锚点

`resolveLinkTarget` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))

- 签名 `func resolveLinkTarget(fromDir, target string) (string, bool)`。
- 行为：`candidates := []string{target}`；若 `url.PathUnescape(target)` 成功且结果与原串不同，则把解码形式追加为第二个候选；逐个 `filepath.Join(fromDir, filepath.FromSlash(candidate))` 做 `os.Stat`，第一个命中的返回 `(path, true)`；全不中返回 `("", false)`。
- **参数名 `fromDir` 是理解一切的关键**：调用方传的是 `filepath.Dir(file)`，即**做出声明的文档自己所在目录**。链接目标永远相对文档解析，不是相对仓库根。
- 两个候选的顺序说明：先试原样，再试 URL 解码形式。于是含 `%20` 的链接和含空格字面量的链接都能命中。
- 误报/漏报边界：它只判定「是否存在一个同名文件」，**不判定是不是目录**（目录也能通过 `os.Stat`），也不判定链接文字与目标是否相符。`TestEveryMarkdownLinkResolvesInTheTree` 先跳过含 `://` 或以 `mailto:` 开头的目标，再按 `#` 把链接切成 path 与 fragment 两半：path 非空走这个函数，fragment 非空走 `headingAnchors`。

`headingAnchors` ([internal/repofmt/citedpaths_test.go](../../internal/repofmt/citedpaths_test.go))

- 签名 `func headingAnchors(text string) map[string]bool`，返回该文件所有 ATX 标题能产生的锚点集合。
- 行为：标题正则 `(?m)^#{1,6}[ \t]+(.*)$`；对每个标题先 `strings.ToLower(strings.TrimSpace(m[1]))`，再逐 rune 处理：`unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'` 保留，`r == ' '` 写成 `-`，其余字符一律丢弃。
- 这套规则是 GitHub 的锚点规则：小写化、丢掉非字母/数字/下划线/连字符、空格转连字符。所以 `## 3. 快速开始` 会产生锚点 `#3-快速开始`（`.` 被丢弃，空格变 `-`）。
- 误报/漏报边界：它按 **rune** 而不是字节遍历，所以中文标题能正常生成锚点；但它不处理重复标题的 `-1` / `-2` 后缀（GitHub 会加），也不识别非 ATX 形式的标题。`TestEveryMarkdownLinkResolvesInTheTree` 里有一条 `fragments == 0` 的 Errorf，专门防止「锚点检查被悄悄丢掉」——如果哪天所有链接都不带 fragment，这条会红，而不是安静地什么都不查。

**章节号 `§N` 的判定**（`TestCitedSectionsExist`）是这一册最需要讲清的一条，因为它解释了「为什么某些 `§` 引用碰巧能过」。

- 引用正则是「`§` 加可选空白加编号」（编号允许点分，如 `1.2.3`）。标题正则是行首若干 `#` 之后、可选 `§`、再编号、再跟一个点或空白。
- **编号怎么抽**：从标题行里抽编号进该文档的集合 `headings[i]`；全部六份文档的编号总数有一个地板 `headingTotal < 60` 就 Fatal。
- **标签从该行附近的多少字符里找**：先 `line := lineAround(body, m[0])` 得到该行本身；再若 `m[0]-200 > 0`，把**该行之前的 200 个字符**接在该行前面，构成 `context`。源码注释的说法是「足够够到那句点名文档的话，又不至于够到上一句」。
- **找不到标签时去哪几份 canonical 文档里任选其一**：如果 `context` 里一个 canonical 文档的标签都没命中，`candidates` 会被填成**全部六份文档**，随后只要**任意一份**有该编号就算通过。这正是「碰巧能过」的机制——一句没写清是哪份文档的 `§` 引用，会被拿去和六份文档的标题编号全集做交集判定，命中口径宽得多。
- 六份 canonical 文档与它们的标签（标签是「点这份文档时可能写的几种写法」）是：
  - `README.md`——`README.md`、`README`
  - `docs/DESIGN.md`——`docs/DESIGN.md`、`DESIGN`
  - `docs/USAGE.md`——`docs/USAGE.md`、`USAGE`
  - `docs/RESUME.md`——`docs/RESUME.md`、`RESUME`
  - `docs/ACCEPTANCE.md`——`docs/ACCEPTANCE.md`、`ACCEPTANCE`
  - `deploy/README.md`——`deploy/README.md`、`deploy/README`
- 每一份都先 `os.Stat`，缺一份就 Fatal（重命名不该让检查静默失效）。
- 跳过规则：`line` 里出现 `RFC` 加数字的（大小写不敏感、允许中间有空白）整条 `§` 引用被跳过——`RFC 7230 §6.1` 是别人的文档，不是本仓库的章节。
- 地板 `total < 20` 就 Fatal。

## 6. 引用符号检查的辅助符号

这一节对应 `TestCitedSymbolsExist`：散文里写下的 `包.符号` 必须在源码里真实存在。

`symbolPackage` ([internal/repofmt/citedsymbols_test.go](../../internal/repofmt/citedsymbols_test.go))

- `type symbolPackage struct { dir string; names map[string]bool; methods map[string]bool; types map[string]symbolType }`。
- `dir` 是相对 `internal/` 的斜杠路径，也是散文里会用的包名（`internal/repofmt` 的包名就是 `repofmt`）。`names` 收该包顶层导出标识符，`methods` 收所有导出方法名，`types` 按类型名索引成员集合。

`symbolType` ([internal/repofmt/citedsymbols_test.go](../../internal/repofmt/citedsymbols_test.go))

- `type symbolType struct { members map[string]bool }`。成员包括结构体字段名与接口方法名。

`internalPackages` ([internal/repofmt/citedsymbols_test.go](../../internal/repofmt/citedsymbols_test.go))

- 签名 `func internalPackages(t *testing.T, root string) map[string]symbolPackage`。
- 行为：`base := filepath.Join(root, "internal")`，`filepath.WalkDir(base, ...)`；跳过目录本身、非 `.go` 文件、以及 `_test.go`（**测试文件里的符号不算**：测试函数名由另一条检查专门负责）。每个文件用 `parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)` 解析。
- 同一目录里出现两个不同的 package 子句就 `t.Fatalf("%s holds both package %s and package %s", ...)`——一个目录两个包，后面按包名查符号就失去意义，所以宁可在这里停住。
- 每个目录的 `symbolPackage` 先建好（`names`/`methods`/`types` 都初始化为 map），对目录里每个文件跑 `collectSymbols`，最后写回 map。
- 误报/漏报边界：包名取的是**目录相对 `internal/` 的路径最后一段所在的那一层**，而不是 `package` 子句里的名字——源码注释明说，这个包用的是目录名，因为散文里写的就是 `internal/xxx` 这种路径。没有非测试 Go 文件的目录会被自然跳过（它们不会先在 map 里留下空条目）。`parser.SkipObjectResolution` 表示只解析语法、不做标识符解析，代价更低，也避免依赖 GOPATH。

`collectSymbols` ([internal/repofmt/citedsymbols_test.go](../../internal/repofmt/citedsymbols_test.go))

- 签名 `func collectSymbols(pkg symbolPackage, file *ast.File)`。注意它接收 `symbolPackage` **按值**，往里写的都是 map（引用类型），所以修改对调用方可见。
- 行为：遍历 `file.Decls`：
  - `*ast.FuncDecl` 且 `ast.IsExported(d.Name.Name)`：无 receiver 的写进 `pkg.names[name]`；有 receiver 的**同时**写进 `pkg.methods[name]` 和 `pkg.types[receiverName(d.Recv)].members[name]`。源码注释给的理由是：散文里一个方法既可写成 `pkg.Method`，也可写成 `pkg.Type.Method`，两处都得记。
  - `*ast.GenDecl` 里的 `*ast.TypeSpec`：导出的类型名进 `names`；若类型体是 `*ast.StructType`，逐个字段的**名字**进 `members`，匿名字段则用 `typeName(field.Type)` 当成员名；若是 `*ast.InterfaceType`，同理把方法名（或嵌入接口的 `typeName`）收进 `members`。
  - `*ast.GenDecl` 里的 `*ast.ValueSpec`：每个导出的名字进 `names`。
- 误报/漏报边界：只收 `ast.IsExported` 的标识符，所以小写局部变量、小写字段、小写函数永远不会进入判定集合——这正是「小写符号不会误触发」的原因。它不解析泛型类型参数、不区分 var/const/func 的语义差别，只要名字导出就记；漏掉的方向主要是非导出成员（`pkg.someUnexportedMethod` 会报错）。

`receiverName` ([internal/repofmt/citedsymbols_test.go](../../internal/repofmt/citedsymbols_test.go))

- 签名 `func receiverName(recv *ast.FieldList) string`。
- 行为：`recv == nil` 或 `len(recv.List) == 0` 返回 `""`；否则返回 `typeName(recv.List[0].Type)`。
- 它只取第一个 receiver 参数——Go 里也只允许一个。对 `func (c *Cache) Get(...)` 返回 `Cache`。

`typeName` ([internal/repofmt/citedsymbols_test.go](../../internal/repofmt/citedsymbols_test.go))

- 签名 `func typeName(expr ast.Expr) string`。
- 行为：递归剥壳——`*ast.StarExpr` 取 `X`（指针）、`*ast.IndexExpr` 与 `*ast.IndexListExpr` 取 `X`（泛型实例化）、`*ast.ParenExpr` 取 `X`（多余括号）；`*ast.Ident` 返回 `t.Name`；其它一律返回 `""`。
- 于是 `*Cache`、`Cache[string]`、`(*Cache)` 都归一成 `Cache`。这是 Go 语法允许的三种包裹形式，符号索引必须能穿透它们，否则同一个类型会以三种拼写出现在散文里而只认得一种。
- 误报/漏报边界：对 `pkg.Type` 形式的选择器表达式（`ast.SelectorExpr`）返回空串，于是这类接收者索引不到成员；对本仓库（接收者总是本地类型）不构成问题。

匹配规则本身：正则是「反引号包裹、第一段小写标识符、第二段大写开头标识符、可选的第三段大写开头标识符」。三段式是 `pkg.Type.Member`。只有当第一段命中 `internalPackages` 的键（即某个 `internal/` 下的包名）时才进入判定；`!pkg.names[symbol] && !pkg.methods[symbol]` 报错，三段式再看 `!pkg.types[symbol].members[member]`。地板 `total < 8` 则 Fatal。源码注释特意举了两个反例：不带包前缀的 `Stats.Alerts` 与光秃秃的 `Stats` 都不被判定——它们可能属于任何包，判定它们只会产生噪声。

## 7. 引用测试名与「断言站点」计数

`testFuncLine` ([internal/repofmt/accesscounts_test.go](../../internal/repofmt/accesscounts_test.go))

- `var testFuncLine = regexp.MustCompile(`(?m)^func Test`)`。多行模式下匹配行首的顶层测试函数声明。
- 它是**分母护栏**用的：`TestDocumentedAccessChecksMatchTheSource` 先用它数 `internal/server/access_test.go` 里的测试函数个数，若不是 11 就 `t.Fatalf("internal/server/access_test.go has %d test functions, want 11; the file changed shape", tests)`。表驱动地重组测试文件会让这个数字变，检查必须先停下来。
- 误报/漏报边界：它只看行首的 `func Test`，不看签名、不区分是不是真的 `func TestXxx(t *testing.T)`；注释里的 `func Test` 若在行首也会被数进去（在这类文件里不会出现）。

`checkSite` ([internal/repofmt/accesscounts_test.go](../../internal/repofmt/accesscounts_test.go))

- `var checkSite = regexp.MustCompile(`t\.(?:Error|Errorf|Fatal|Fatalf)\(`)`。四个方法名之一后面跟左括号，算「一个断言调用点」。
- 它故意不含 `t.Skip`、`t.Log`、`t.Helper`，也不含 `require.*` 之类。

`countCheckSites` ([internal/repofmt/accesscounts_test.go](../../internal/repofmt/accesscounts_test.go))

- 签名 `func countCheckSites(src string) int`。
- 行为：按 `\n` 切行；对每一行，若 `strings.HasPrefix(strings.TrimSpace(line), "//")` 就整行跳过（**被注释掉的检查不算证据**），否则 `n += len(checkSite.FindAllStringIndex(line, -1))`。
- 它数的是**站点**（源码里的调用点），不是**执行次数**：表驱动循环里一处站点会跑很多遍，真实执行数只有把测试跑起来才知道，这个包看不见。源码注释把这个区别写得很清楚，这也是它与 `gatecounts` 那一套（数脚本里的断言行）的分工。
- 调用者：`TestDocumentedAccessChecksMatchTheSource`，分别对 `internal/server/access_test.go` 整体、以及 `internal/config/config_test.go` 里的两个函数体调用它；两组数相加得 `total`，再与 README.md 与 docs/ACCEPTANCE.md 里印的合计数比对。

`functionBody` ([internal/repofmt/accesscounts_test.go](../../internal/repofmt/accesscounts_test.go))

- 签名 `func functionBody(src, name string) (string, bool)`。
- 行为：`start := strings.Index(src, "\nfunc "+name+"(t *testing.T) {")`；`start < 0` 返回 `("", false)`。否则取 `rest := src[start+1:]`，再 `end := strings.Index(rest[1:], "\nfunc ")`；`end >= 0` 返回 `rest[:end+1], true`，否则返回整个 `rest, true`。即「从声明行到下一个顶层 `func` 的前一行」。
- 误报/漏报边界：它靠**字面** `(t *testing.T) {` 匹配声明行，所以签名一变（加 `t *testing.T` 之外的前缀、换成别的参数名、方法而不是函数）就找不到，返回 `("", false)`。它是纯文本切片，不解析 Go；函数体里出现**行首**的 `func `（比如字符串字面量里的换行）会提前截断，在这类文件里不会出现。调用者只有 `TestDocumentedAccessChecksMatchTheSource`，用来给 `internal/config/config_test.go` 的两个函数分别数站点。

`TestCitedTestNamesExist` 侧的收集不是独立符号：它在测试函数体里内联了两个正则——一个抓反引号里的测试函数名引用（形状是 Test 前缀加双大写驼峰），一个抓行首的测试函数定义；定义从 `commentSurfaces` 里后缀为 `_test.go` 的文件收集，所以**定义面是 `cmd/` 与 `internal/` 的全树**（不含 `scripts/`），引用面是全部散文面并先过 `withoutFencedCodeBlocks`。两道地板：定义数 `len(names) < 200` 与引用数 `len(quoted) < 30` 都 Fatal。

## 8. Assert-* 助手 arity：类型与定义收集

这一节和接下来三节讲的是同一个测试的三层：`TestAssertionCallsDoNotOutrunTheirHelpers` 要回答「门禁脚本里有没有调用点传了助手声明之外的参数」。背景是 PowerShell 的一个行为：**它不拒绝多余的位置参数**——能绑定的绑定，绑不了的**静默丢弃**。`scripts/verify-m3.ps1` 曾有六个调用点给只声明三个形参的 `Assert-Equal` 多传了一个 detail 字符串，于是闸门红灯时那句本该告诉操作员「哪个 pid 活了下来」的话被整段丢掉，打印出来只剩 "expected '0', got '1'"。这个错误在 review 里看不见：不报错、不警告，闸门照样 PASS 或 FAIL。

`assertHelper` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- `type assertHelper struct { arity int; decl string }`。
- `arity` 是该助手 `param(...)` 块的**形参个数**，也就是 PowerShell 会绑定的位置参数上界；`decl` 是形参表被压平后的字符串，失败时引用它，让读者看见「这个助手其实只声明了这几个」。
- `decl` 在两种异常情况下会被写成说明文字而不是形参表：完全不带 `param(...)` 块时写 `"no param(...) block"`；`param (...)` 的括号没配平时写 `"unparsed param(...) block"`。两种情况都**仍然产生一条定义**——一个没有形参的助手也是一条定义，记下它才能让它的调用点不掉进无人比较的 skipped 桶。

`assertHelperLine` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- `var assertHelperLine = regexp.MustCompile(`^function\s+(Assert-[A-Za-z]+)\s*\{\s*$`)`。**要求整行就是函数头**，所以 `function Assert-Equal { # note` 这种带尾注释的行不匹配，`Assert-Equal` 后面直接跟参数的写法也不匹配（本仓库的脚本都写成 `function Assert-Equal {` 换行再 `param(`）。
- 名字形状限制为 `Assert-` 加英文字母，所以助手名里带数字或横线的不会被收。

`assertHelperArities` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- 签名 `func assertHelperArities(src string) map[string]assertHelper`。`lines := strings.Split(src, "\n")`，`out := make(map[string]assertHelper, 8)`。
- 行为：逐行找 `assertHelperLine` 的命中；命中后从下一行开始向下扫，直到找到 `param(...)` 或判定它不存在：
  - 维护一个 `inBlockComment`：为真时遇到含 `#>` 的行就置假然后 continue；遇到以 `<#` 开头的行则把 `inBlockComment` 设成「这一行里没有 `#>`」，然后 continue。源码注释给的理由是**注释型帮助块可以有好几行长**，所以是「跳过注释行」而不是「固定跳几行」。
  - 空行或以 `#` 开头的行 continue（单行注释也跳）。
  - 第一个既不是注释也不是空行的行，若不以 `param` 开头、或去掉 `param` 前缀再 `TrimLeft` 后不是以 `(` 开头，就记 `"no param(...) block"` 并 break。
  - 否则取该行 `strings.Index(lines[j], "(")` 之后的文本，`parenGroup(text, 0)` 求括号组；为了**跨行的形参表**，若没配平就往下拼下一行重试，最多拼到 `j+8` 行。配平成功则 `arity: countParamList(inner)`、`decl: collapse(inner)`；失败记 `"unparsed param(...) block"`。无论哪种，最后都 break。
- 误报/漏报边界：一个助手只记一条定义，同名助手在同一个脚本里重复定义会**后者覆盖前者**；它假定 `param` 块紧跟在函数头之后的第一个非注释行，中间插了别的语句就认不出来（会走 `"no param(...) block"` 分支，arity 为 0，于是该助手的任何调用点都会被判成超绑——是**误报**方向，偏响）。上限 8 行的折行容差对仓库现状足够。

`collapse` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- 签名 `func collapse(decl string) string`。实现是 `strings.Join(strings.Fields(decl), " ")`——把所有连续空白压成单个空格，让跨行折行的形参表在错误消息里占一行。它只做空白归一，不改标点、不解析属性。

## 9. Assert-* 助手 arity：调用点识别

`assertCall` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- `type assertCall struct { name string; line int; args []string; hasNamed bool }`。
- `name` 是匹配到的 `Assert-Xxx` 原文；`line` 是**这条逻辑语句的起始行**（1 基）；`args` 是 `scanStatement` 切出来的参数 token；`hasNamed` 表示这条语句里至少出现过一个 `-Name` 形式的 token。主测试用 `hasNamed` 统计 `byName`，用 `line` 拼错误消息。

`assertCallName` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- `var assertCallName = regexp.MustCompile(`Assert-[A-Za-z]+`)`——只认名字，不认位置。位置由 `findAssertCalls` 的「名字是否开启一条语句」规则判断。

`findAssertCalls` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- 签名 `func findAssertCalls(src string) []assertCall`。
- 行为：按行遍历；对每行里 `assertCallName.FindAllStringIndex` 的每个命中：把命中之前的文本当 `prefix`，若 `prefix` 含 `#` 就 continue（注释里的名字不算）；`strings.TrimSpace(prefix)` 非空**且不以 `{`、`;`、`=` 结尾**也 continue。也就是说，一个名字被认成调用，当且仅当它**开启一条语句**：行首（缩进后）、开括号之后、分号之后、赋值之后。这条规则覆盖 `if ($x) { Assert-Equal ... }` 这种同行写法，也排除 `Write-Host "Assert-Equal ..."` 这种出现在字符串中间的名字。
- 认出之后，从名字结束处取 `text := line[loc[1]:]`，记住起始行号；只要 `needMoreInput(text)` 为真且还有下一行，就吞掉下一行并 `text += "\n" + lines[i]`。然后 `args, named, complete := scanStatement(text)`；若 `!complete` 就 continue（这条语句本身不完整，无法读出参数，宁可跳过）。成功则追加 `assertCall`，随后 **`break`**——这条逻辑语句的剩余部分不再被扫描，位于被消耗续行上的其它 `Assert-*` 名字都属于同一条语句。这个 `break` 是刻意的：不 break 会在多行调用里把参数重复计一遍。
- 误报/漏报边界：漏报方向有三处——`#` 出现在名字之前的行、名字前面是某个奇怪字符、以及 `scanStatement` 判定不完整。误报方向基本没有：名字必须出现在 `Assert-` 加字母的精确形状里。源码注释里那句「一个名字开启一条语句时才认」是全部判定的核心。

`needMoreInput` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- 签名 `func needMoreInput(s string) bool`。
- 行为：先把结尾的空白与换行 `TrimRight` 掉，若以反引号结尾则返回真（PowerShell 的续行符）；否则调用 `scanStatement(s)` 并返回 `!complete`。也就是把「反引号续行」与「引号/括号没闭合」两种信号合在一次调用里报出。
- 误报/漏报边界：末尾反引号的判断是在**已 TrimRight 的字符串**上做的，所以行尾空格不影响；但一个以反引号结尾的完整语句（比如字符串字面量正好以反引号结束）会被误判为需要更多输入——是偏保守的方向，只会让这条语句被跳过。

## 10. Assert-* 助手 arity：单条语句的词法

`scanStatement` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- 签名 `func scanStatement(s string) (args []string, hasNamed bool, complete bool)`。它是本包最长的一个辅助函数，因为决定「一个参数在哪结束」的是 **PowerShell 自己的词法**，而不是任何通用 tokenizer，而这些脚本会在双引号里的子表达式展开中嵌套引号（形如 `"$(if (...) { " >= 1" })"`）。
- 内部结构：`type context struct { kind byte; depth int; subexpr bool }`，初始 `stack := []context{{kind: 'c'}}`——kind 为 `c` 是代码、`'` 是单引号串、`"` 是双引号串。有个闭包 `flush()`：把当前累积缓冲 `strings.TrimSpace` 之后压入 `args`（空则丢弃），并且若 token 长度大于 1、首字符是 `-`、第二字符是字母，则置 `hasNamed = true`。
- 逐 rune 的状态机：
  - 单引号串里：遇到 `''` 是两个单引号表示一个字面单引号，跳过一个字符继续；遇到单个 `'` 弹栈。
  - 双引号串里：遇到反引号吃掉下一个字符（转义）；遇到 `$(` 压入一个 `context{kind: 'c', subexpr: true}`——**这个左括号在此被消耗、不计入 depth**，将来平衡它的那个 `)` 才是弹出该上下文的；遇到 `""` 表示一个字面双引号，跳过；遇到单个 `"` 弹栈。
  - 代码态：`'` 或 `"` 压栈，并把该字符写进缓冲；反引号吃掉下一个字符（若已到文本末尾则直接 `return args, hasNamed, false`）；`(` 增加 depth；`)` 若 depth 大于 0 就递减、若栈顶是 `subexpr` 就弹栈、否则 flush 并返回 complete 为真；`[`、`{` 增加 depth；`]`、`}` 在 depth 为 0 且栈只剩一层时 flush 并返回 complete，否则递减 depth；`#`、`|`、`;` 在 depth 为 0 且栈只剩一层时 flush 并返回 complete；**空白只在 depth 为 0 且栈只剩一层时才 flush**，否则写进缓冲；其余字符写进缓冲。
  - 那句「否则写进缓冲」是这张表里最要紧的一格：`$( )` 子表达式嵌在字符串里时，其中的空白是**字符串内容**，在那里 flush 会把一个参数切成五个。
- 收尾再 flush 一次，返回 `args, hasNamed, len(stack) == 1 && stack[0].depth == 0`——只有回到最外层代码上下文且括号全平衡，才认为这条语句完整。
- 误报/漏报边界：它不处理 here-string、不处理 `@()`／`@{}` 的特殊语义（按普通括号算 depth），也不区分 `-` 开头的参数是 switch 还是取值参数（这一点留给 `namedArg` 与 `bindingCounts` 用保守读法处理）。`complete` 为假时调用方直接跳过这条语句，所以它的漏报是安静的，不会变成假的违规。

`parenGroup` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- 签名 `func parenGroup(s string, open int) (inner string, end int, ok bool)`。
- 行为：先转 `rs := []rune(s)`；若 `open` 越界或 `rs[open] != '('` 返回 `("", 0, false)`。从 `open` 起扫，维护 `depth`、`start` 和一个 `quote`：在引号内时，反引号（仅在双引号串里）吃掉下一字符、成对引号是转义、遇到同一个引号清掉 `quote`；在引号外遇到 `'` 或 `"` 置 `quote`，遇到 `(` 增加 depth 并在 `depth == 1` 时记 `start = i+1`，遇到 `)` 递减并在 `depth == 0` 时返回 `string(rs[start:i]), i + 1, true`。扫完返回 `("", 0, false)`。
- 它被 `assertHelperArities` 用来切出 `param(...)` 的形参表。**引号与 `$( )` 展开里的括号都被尊重**，所以形参默认值里写的括号（比如一个字符串里带 `(`）不会提前闭合这一组。
- 误报/漏报边界：只把双引号串里的反引号当转义（单引号串里反引号是普通字符，符合 PowerShell）；不处理块注释里的括号——但 `assertHelperArities` 已经在调它之前跳过了注释行，所以这里见到的不会带注释。返回的 `end` 是本组的**右括号之后**的位置，调用方目前只关心 `ok`。

`countParamList` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- 签名 `func countParamList(inner string) int`。数一个 `param(...)` 内部有几项形参。
- 行为：`count, depth := 0, 0`，`quote` 记录当前引号，`empty` 表示「当前这一项还是空的」。逐 rune：引号内遇到同引号清 `quote`；引号外遇到 `'` / `"` 置 `quote` 并把 `empty` 置假；遇到 `(`、`[`、`{` 增加 depth 并把 `empty` 置假；遇到 `)`、`]`、`}` 递减 depth；遇到 `,` 时，若 depth 为 0 则「`empty` 为假就 `count++`」，然后 `empty = true` 并 continue，depth 大于 0 时把 `empty` 置假（这是属性列表里的逗号，不是形参分隔符）；其余字符只要非空白就把 `empty` 置假。收尾若 `empty` 仍为假再 `count++`（最后一项后面没有逗号）。
- 源码注释点明了这个数的用途：**有属性的形参仍然只算一个**（比如 `[Parameter(Mandatory)][string]$Label`）。这个数是用作「PowerShell 能绑定多少」的**上界**，多算只会让主测试更安静——即只可能漏报、不会误报。
- 误报/漏报边界：一个空 `param()` 会得到 0；只有一个 `,` 的 `param(,)` 也得到 0（两项都空）；它不识别默认值里的逗号是否在括号内是靠 depth 判断的，所以 `param($a = (1,2), $b)` 会正确得到 2。

## 11. Assert-* 助手 arity：参数绑定计数

`bindingCounts` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- 签名 `func bindingCounts(args []string) (positional, named int)`。给定一条调用语句切出的参数 token 序列，估算 PowerShell 会绑定多少个。
- 行为：`seen := make(map[string]bool, 4)`；逐 token：`name, isNamed := namedArg(args[i])`；不是命名参数就 `positional++` 并 continue；是命名参数且名字首次出现则记入 `seen` 并把 `named++`（**重复写同一个参数名只算一次**）。之后判断它是否消耗下一个 token：若 `strings.Contains(args[i], ":")`（形如 `-Name:value`，值在同一个 token 里）或已是最后一个 token，就不消耗；否则看下一个 token——若下一个**也是命名参数**就不跳，否则 `i++` 把后面那个吃掉。
- 那条「吃掉后面那个」是保守读法的落点：PowerShell 里 switch 参数不取值，但解析器无法从名字区分 switch 与取值参数，于是统一按取值参数处理，**把后面的位置参数读成它的值**。源码注释明说这是刻意的：「switch 参数后跟位置参数会被读成拿那个参数当值——只会让本测试更安静」。于是 `-Verbose $x` 得到 positional 为 0、named 为 1，而不是 1 和 1。
- 误报/漏报边界：**偏安静**是它的设计目标。它可能少算被误吞的位置参数（漏报），不会多算（不会误报）。调用方 `TestAssertionCallsDoNotOutrunTheirHelpers` 用它得到 `bound := positional + named`，只在 `bound > helper.arity` 时报错，所以偏安静直接意味着「不会因为读错而误报红线」。

`namedArg` ([internal/repofmt/asserts_test.go](../../internal/repofmt/asserts_test.go))

- 签名 `func namedArg(tok string) (string, bool)`。
- 行为：`len(tok) < 2 || tok[0] != '-' || !unicode.IsLetter(rune(tok[1]))` 就返回 `("", false)`；否则 `name := tok[1:]`，若含 `:`（`strings.IndexByte`）就把 `:` 之后切掉（**剥掉同一个 token 里的值**），返回 `(name, true)`。
- 两个边界都是刻意的：**仅仅以短横开头的 token 不是参数名**——`-1` 是一个负数、是一个值；用 `unicode.IsLetter` 而不是 ASCII 判断，所以 PowerShell 允许的非 ASCII 参数名（本仓库没有）也不会被误判成值。`-Name:value` 这种粘写形式剥成 `Name`，于是它既算一个命名参数、又不额外消耗后面的 token。
- `TestBindingCountsReadNamedParametersConservatively` 用六个表驱动用例把上面这些读法钉住，**每一处可两读的都钉在更安静的一侧**：全位置参数得 `(2, 0)`；三个命名参数各带自己的值得 `(0, 3)`；`-Detail:3` 加一个字面量的值得 `(1, 1)`；同名重复 `-Min 0 -Min 1` 得 `(0, 1)`；`-1` 与 `$x` 得 `(2, 0)`；`-Verbose $x` 得 `(0, 1)`（switch 无法与取值区分，所以 `$x` 被当成它的值）。

主测试本身还带三道地板，它们界定了这套解析的**判定面是否还在**：助手定义数 `definitions < 8` 就 Fatal（arity 表就是比较对象）；被判定的调用数 `judged < 400` 就 Fatal（脚本调用风格变了以致解析器看不懂）；跳过的调用数 `skipped > judged/20` 就 Fatal（skip 只应在「助手定义在别的脚本、本文件看不到」时发生，今天一个都没有；占比这么大意味着脚本开始从本测试不读的地方解析助手，那些调用全都停止被检查而测试仍报成功）。

## 12. 集合数字护栏与目录树自检

这一节对应 `TestCitedTreeCountsMatchTheTree` 与 `TestReadmeTreeNamesEveryPackage`：散文里印的「包有多少、配置有多少」必须等于真实的目录清单，README 里那棵目录树必须点名每一个包。

`packageDirs` ([internal/repofmt/inventory_test.go](../../internal/repofmt/inventory_test.go))

- 签名 `func packageDirs(t *testing.T, root string) map[string]bool`，`t.Helper()`。返回 `cmd/` 与 `internal/` 下**至少含一个 Go 文件**的目录，键是相对仓库根、斜杠分隔的路径。
- 行为：对 `[]string{"cmd", "internal"}` 两棵树分别 `os.Stat`（目录没了就 Fatal，报「this check no longer covers it」），然后 `filepath.Walk`。walk 回调里：非目录直接返回；**不是根目录本身**时，目录名以 `.` 或 `_` 开头、或等于 `testdata` 就 `filepath.SkipDir`；否则 `os.ReadDir` 看有没有以 `.go` 结尾的条目，有就把该目录的相对路径记进集合（`break`，一个目录只判一次）。
- 末尾地板 `len(packages) < 10` 就 Fatal——**walk 和计数都受检查**，一次提前停止的遍历不能让下游检查看起来「与散文一致」。
- 源码注释解释为什么是这个定义：它是 `go list ./...` 会报告的东西，除去 Go 自己忽略的那些目录。跳过 `.`/`_` 开头与 `testdata` 正是 Go 自己的忽略规则。
- 调用者：`TestCitedTreeCountsMatchTheTree`（拿它当分母）与 `TestReadmeTreeNamesEveryPackage`（拿它当「必须被点名的集合」）。

集合数字的两条正则（内联在测试函数里）是：

```text
(\d+)\s*个包
(\d+)\s*份[^\n]{0,10}配置
```

- 第一条要求数字紧跟（允许空白）「个包」。第二条允许数字与「配置」之间夹最多 10 个非换行字符，所以「数字 + 份 + 若干修饰词 + 配置」这类加了修饰的句子同样在判定面内（被比对的只是那个数字）。
- 两条正则**扫遍全部散文面**（`proseSurfaces` 的返回），包括围栏代码块——这一条与其它所有散文检查相反，源码注释专门解释了原因：别处的规则是「围栏引用的是发生过的事、不是现在的声明」，而这里读者最先遇到的数字恰恰是 README.md 那棵目录树，那是**披着示例输出外衣的、关于今天这棵树的声明**；抹掉围栏会让它成为唯一逃过判定的那个（最初就是这样：只查出六处而不是七处）。
- 为什么「36 个构建单元」这类措辞不会触发：正则要求的字面是「个**包**」和「份……**配置**」。「个」后面跟「构」不匹配第一条；「份」后面跟「构建单元」里没有「配置」二字，也不匹配第二条。同一套护栏里不存在「数字加任意量词」的通配规则——那样会把「七个里程碑」「八个脚本」这类不需要分母的措辞也拖进来。
- 分母本身的地板：`configs/*.yaml` 的 glob 结果少于 5 个就 Fatal（「glob 匹配到零个会让下面每一句话看起来都是错的，所以在用它之前先检查它」）；散文侧的 `claims < 4` 也 Fatal。

`treeNames` ([internal/repofmt/inventory_test.go](../../internal/repofmt/inventory_test.go))

- 签名 `func treeNames(block string) map[string]bool`。返回一个目录树代码块里点到的**名字**集合。
- 行为：先按行处理，`strings.Index(line, "#")` 命中就把该行从 `#` 处截断（**注释先丢掉**——注释里的名字不是树里的名字），把处理后的行拼回整段文本。然后两路收集：
  - `treeName` 命中全部收进集合；
  - `treeRange` 的每个子匹配按区间展开：`prefix`、`from`、`suffix`、`to`；`from` / `to` 解析失败、或 `to <= from`、或 `to-from > 100` 就跳过；若 `suffix` 非空而 `prefix` 不以 `suffix` 结尾也跳过；否则把 `prefix + 序号` 从 `from` 到 `to` 全加进集合。
- 两个包级正则：`treeName = regexp.MustCompile(`[a-z0-9][a-z0-9-]*`)`（目录树里一个词，例如 `repofmt`），`treeRange = regexp.MustCompile(`([a-z][a-z0-9-]*?)(\d+)\.\.([a-z]*)(\d+)`)`（目录树用的区间简写，例如 `verify-m1..m6` 代表 `verify-m1` 到 `verify-m6`）。
- 上限 100 是防呆：一个手滑写成 `m1..m9999` 的区间不会把集合撑爆。
- 误报/漏报边界：`treeName` 只认小写字母、数字、横线组成的词，所以树里的 `README.md`、`ci.yml` 不会被当成一个**包名**收集（也正因如此，把包名写成大写或含下划线会导致漏报）；注释截断用的是**第一个** `#`，所以树里如果真有以 `#` 开头的合法名字会被误截（本仓库的目录树没有这种名字）。

`fencedBlockHolding` ([internal/repofmt/inventory_test.go](../../internal/repofmt/inventory_test.go))

- 签名 `func fencedBlockHolding(body, marker string) string`。返回 `body` 里**第一个**包含 `marker` 的围栏块的正文；没有就返回 `""`。
- 行为：按行遍历，遇到一个围栏起始行就向下找下一个围栏行，两者之间的行拼成一个 block；若 block 含 `marker` 就返回它；否则把外层索引跳到该围栏处继续找下一块（`i = j` 再 `break` 内层）。
- 它是「从一份文档里取那棵目录树」的定位器：`TestReadmeTreeNamesEveryPackage` 用 `"\u251C\u2500\u2500 cmd/"`（即 `├── cmd/`）当 marker。返回空串时测试 Fatal，理由是「README 里不再有 cmd/ 清单，这条检查就不再覆盖它」。
- 误报/漏报边界：它靠 `isFence` 判断围栏行，**任何以三个反引号开头的行都算围栏**，因此「用 ```powershell 标语言的块」不会翻转后续所有块的奇偶性；但它不处理四个反引号的围栏嵌套，也不校验围栏是否闭合——末块没闭合时会一路取到文件尾。

`isFence` ([internal/repofmt/inventory_test.go](../../internal/repofmt/inventory_test.go))

- 签名 `func isFence(line string) bool`，实现是 `strings.HasPrefix(strings.TrimSpace(line), "```")`。判断一行是否开启或关闭一个围栏代码块：**先去掉行首空白再看三个反引号**，所以缩进在列表项里的围栏也能识别。它不检查反引号的个数、不检查是否有语言标注、更不检查开/闭配对——配对是 `fencedBlockHolding` 的循环负责的。

`atoi` 与 `lineOf` ([internal/repofmt/gatecounts_test.go](../../internal/repofmt/gatecounts_test.go))

- `func atoi(t *testing.T, where, digits string) int`：`t.Helper()`；`strconv.Atoi` 出错则 `t.Fatalf("%s: parsing the count %q: %v", where, digits, err)`。它的定位是「解析一个**已经被正则限制为数字**的计数」——所以失败意味着正则与代码不一致，是 Fatal 而不是 Errorf 的合适场合。`where` 通常传文件名，让错误消息能指认是谁的数字坏了。
- `func lineOf(text string, offset int) int`：`strings.Count(text[:offset], "\n") + 1`。把字节偏移变成 1 基行号，供错误消息用。它与 `lineAround` 是一对：一个给出行号，一个给出行内容。

## 13. 闸门条数与合计数字

这一节对应 `TestDocumentedGateTotalsAreTheSumOfTheirRows` 与 `TestEveryQuotedGateCountMatchesTheTable`。要处理的是两张表：README.md 里的表与 docs/ACCEPTANCE.md 里的闸门表，每张表按里程碑一行、下面一行「合计」。历史上它们**并不相等**：curl 列的七个数加起来是一回事，印出来的合计是另一回事，而合计是从某一行更新**之前**的和抄下来的。测试的立场是一句话：**印在加数旁边的数必须等于它们之和。**

`milestoneNames` ([internal/repofmt/gatecounts_test.go](../../internal/repofmt/gatecounts_test.go))

- `var milestoneNames = []string{"M0", "M1", "M2", "M3", "M4", "M5", "M6"}`——两张表都应该描述的集合，**有序**。有序是因为它同时充当「哪一行对应哪个里程碑」的字典序来源；把它当集合用时只看元素。

`readmeMilestoneRow` 与 `acceptanceMilestoneRow` ([internal/repofmt/gatecounts_test.go](../../internal/repofmt/gatecounts_test.go))

- 两个包级正则描述同一行的两种拼法：README.md 在格子里点名 Go 门的命令，docs/ACCEPTANCE.md 在计数后面跟原始数据文件名。两者都要求行首是 `|`、第一个格子是 `M` 加一位数字，然后抓两个数字（Go 侧计数、curl 侧计数）。
- 误报/漏报边界：里程碑限定为**一位数字**（`M\d`），所以 `M10` 这种形状不会被当成行军；两列都是纯数字（允许前后空白），不接受 `~`、`+` 这类修饰。

`milestoneCounts` ([internal/repofmt/gatecounts_test.go](../../internal/repofmt/gatecounts_test.go))

- 签名 `func milestoneCounts(t *testing.T, name, text string, row *regexp.Regexp) map[string][2]int`，`t.Helper()`。
- 行为：对 `row` 的每个命中，若该里程碑名已经出现过就 `t.Errorf("%s appears twice in the gate table", ...)`；否则记 `counts[m[1]] = [2]int{atoi(t, name, m[2]), atoi(t, name, m[3])}`——`[0]` 是 Go 门那一列，`[1]` 是 curl 门那一列。
- **分母护栏**：若 `len(counts) != len(milestoneNames)` 就 `t.Fatalf("found %d milestone rows, want %d; the table changed shape", ...)`，源码注释给的理由是「否则一张被重塑的表会把这个测试变成一句关于空无的绿色断言」；随后逐个要求 `counts[want]` 都存在。
- 调用者：`TestDocumentedGateTotalsAreTheSumOfTheirRows` 对 README.md 与 docs/ACCEPTANCE.md 各调一次，然后逐里程碑双向检查两张表「一方有一方没有」，再对同一里程碑比两边的数值（不等就报「两张表描述同一批闸门，所以其中一张是陈的」）。`TestEveryQuotedGateCountMatchesTheTable` 也用它建出 `rows`，供后面按里程碑比对每一处引用。

`countsKey` ([internal/repofmt/gatecounts_test.go](../../internal/repofmt/gatecounts_test.go))

- 签名 `func countsKey(ns []int) string`。行为：`sorted := append([]int(nil), ns...)`（**先拷贝再排序，不动调用方的切片**），`sort.Ints(sorted)`，返回 `joinInts(sorted)`。
- 它把一组计数渲染成一个**顺序无关的键**，所以同一组数以别的顺序陈述仍然匹配。Makefile 那一半就是靠它比对的：`##` 注释块里出现的数字顺序与测试期望的顺序常常不同。

`joinInts` ([internal/repofmt/gatecounts_test.go](../../internal/repofmt/gatecounts_test.go))

- 签名 `func joinInts(ns []int) string`。逐个 `strconv.Itoa` 后用 `", "` 连接。它的唯一用途是给错误消息渲染计数，所以格式稳定比好看重要。

同一测试里还有几处不算独立符号但界定了判定面的机制，值得在这里记下：扫描面是 README.md、`.github/workflows/ci.yml`、**`internal/repofmt/curl_gates_test.go` 自己**、`scripts/lib/summarize-gates.ps1`、`scripts/lib/run-go-verify.ps1`，再加上 `filepath.Glob(root/docs/*.md)`；面数少于 5 就 Fatal。把「写英文句子的那两个脚本」也算进协议面的理由写在注释里：`summarize-gates.ps1` 的表头曾经在它存在的全部时间里带着过期的两个总数而无人读，因为当时的检查只扫文档。重复总计的那些句子的措辞表有 10 条，每条都配一个正则与「这个数落在 Go 侧还是 curl 侧」的标注，两条护栏是「每种措辞至少命中一次」与「每条措辞至少还有一处面在用它」——后者保证措辞改了之后测试会红，而不是让总数变成没人声明的数字。Makefile 那一半有 14 条声明（七个 Go 目标的 `##` 注释、七个 curl 目标），另有一条覆盖率守卫：任何以 `verify` 开头、且其 `##` 注释里印了断言数的 target 若不在声明表里就报错——**后加的闸门否则会不受检查地陈述自己的计数**。`commentBlock` 从 target 行向上收集所有紧邻的 `##` 行、去掉前缀再拼成一段，因为 Makefile 把一句话折在几行注释里。

## 14. 基线数字：条目类型

这一节与下面两节讲 `TestHeadlineClaimsMatchTheBaseline`：README.md 首屏请读者核对的那些数字，必须与 `docs/baseline/` 里实测记录里的数一致。基线文件是**已经发生过的运行**的记录，正因如此它们豁免于其它所有散文重写；把某个数从 `58.97` 重打成 `58.9`，或用不同结论重生成一份 summary，会让 README 断言一件记录已经不再支持的事，而记录是相信那句话的唯一理由。

`baselineClaim` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

一条声明 = 一个数 + 它的出处 + 哪些文档还得印它。字段如下：

- `quote string`——文档印出的**确切字符串**，已经缩放、已经舍入；
- `file string`——`docs/baseline/` 下的文件名；
- `files []string`——多个记录，当这条声明是它们的总和；
- `sum bool`——把这些值加起来，而不是要求它们彼此一致；
- `phrase string`——承载这个数的措辞；为空时用 `quote` 本身；
- `docs []claimPrint`——必须印这个数的文档；不点名时默认 README.md；
- `find map[string]any`——当 `path` 本身有歧义时，点明要读的那个对象的键值对；
- `path []string`——从该对象下到数字的键路径；开头是 `*` 表示搜遍整个文件；
- `scale float64`——把存下来的比率变成散文里的百分数用 100；0 表示不缩放；
- `digits int`——文档印几位小数。

**刻度与小数位数属于条目本身**，因为记录里存的是 `0.5897`、散文印的是 `58.97%`；记录里存 2.423、散文印 `2.42x`。格式化的责任在声明这一侧，而不是在记录那一侧。

需要点名的地方说清楚：用户列的清单里把这个类型记成「11 字段」，按源码实际只有上面 10 个字段——多出来的那一个很可能是把类型名本身也算了一项。按源码为准。

`claimPrint` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- `type claimPrint struct { file string; phrase string }`，**两个字段**。
- `file` 是要印这个数的文档，为空表示 README.md；`phrase` 是这条声明在该文档里用的措辞，为空则退回 `baselineClaim.phrase`、再退回 `quote`。
- 存在的理由（源码注释）：同一个测量在 README 里是「18000 个请求」，在 RESUME 里是「18000 请求」，重要的是**两处都还在说这个数**。

`prints` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func (claim baselineClaim) prints() []claimPrint`，方法（接收者是值）。
- 行为：`len(claim.docs) > 0` 就返回 `claim.docs`，否则返回 `[]claimPrint{{}}`——一个零值元素，代表「默认文档」。所以「一条声明默认必须出现在 README.md」这条规则只在这一处编码，别处不再重复判断。

`document` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func (want claimPrint) document() string`。`want.file != ""` 就返回它，否则返回 `"README.md"`。它是 `claimPrint.file` 那条「空即默认」规则的唯一落点。

`wording` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func (want claimPrint) wording(claim baselineClaim) string`。三级回退：`want.phrase` 非空用它；否则 `claim.phrase` 非空用它；否则用 `claim.quote`。返回值是拿去 `strings.Contains` 匹配的字符串。
- 误报/漏报边界：匹配是**纯子串包含**，不要求它出现在标题、表格或任何特定位置，也不做空白归一。所以一句被拆行、或在数字与单位之间多插了空格的中文句子会让措辞不再被包含从而报错（误报方向）；反过来，这句话出现在文档任何一个角落都算通过。

`claimDocs` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func claimDocs(claim baselineClaim) []string`。对 `claim.prints()` 逐个取 `want.document()` 收进一个切片。它只给错误消息用：当记录里的数变了，消息要能说出「哪些文档因此不再正确」，而不是只喊一个文件名。

## 15. 基线数字：读取、定位与渲染

`parseBaseline` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func parseBaseline(t *testing.T, root, name string) any`，`t.Helper()`。
- 行为：`raw := readFile(t, filepath.Join(root, "docs", "baseline", name))`；然后 `json.Unmarshal([]byte(raw), &doc)`，出错则 `t.Fatalf("docs/baseline/%s is not JSON: %v", name, err)`；成功返回 `doc`。
- 关键点：它解到 **`any`**，所以 JSON 里的数字全是 `float64`，对象是 `map[string]any`、数组是 `[]any`。整个下游的定位逻辑都建立在这三件事上。
- 误报/漏报边界：`readFile` 已经覆盖文件不存在的情况，所以这里的 Fatal 专指「文件在但不是合法 JSON」；不做 schema 校验，字段缺失留给 `numbersAt` 变成「找不到值」的错误。调用方用一个 `parsed` map 加 `docFor` 闭包缓存结果，同一份基线不会重复解析。

`numbersAt` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func numbersAt(node any, path []string) []float64`。按 `path` 从解出来的文档里取出**所有**匹配的数字。
- 行为：
  - `len(path) == 0`：node 是 `float64` 就返回单元素切片，否则返回 nil。
  - `path[0] == "*"`：`len(path) < 2` 直接返回 nil；否则递归 `walk` 整棵树——每遇到 map，若某个键等于 `path[1]` 就把 `numbersAt(child, path[2:])` 追加进结果；**无论是否命中都继续 `walk(child)`**（这样 `*` 才能真的搜遍文件，而不是只搜第一层命中的子树）；数组逐个元素 walk。
  - 普通段：map 上取 `typed[path[0]]` 递归；数组上把 `path[0]` 用 `strconv.Atoi` 当下标，且要求 `index >= 0 && index < len(typed)`；其它返回 nil。
- 返回 `[]float64` 而不是单个值，是为了让调用方按语义决定「要求一致」还是「求和」。
- 误报/漏报边界：它只认 `float64`，所以记录里以字符串存的数字（例如把 `"58.97"` 写成字符串）永远找不到；`*` 段必须后面还有一段，单独的 `["*"]` 返回 nil 而不是「文件里所有数字」。

`findObjects` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func findObjects(node any, find map[string]any) []map[string]any`。
- 行为：递归闭包 `walk`。遇到 `map[string]any` 时先判定它是否符合 marker——对 `find` 的每一对键值要求 `got, ok := typed[key]; ok && got == want`，**全部命中**才把这个 map 追加进结果；然后不论是否符合，都对它的每个 child 继续 walk。遇到 `[]any` 就逐个 walk。返回所有带齐 marker 全部键值对的对象。
- 比较是 `got == want`，`want` 来自 Go 字面量，所以字符串比字符串、数字比数字（数字比较时 `want` 也是 `any` 里的 `float64`，因为调用方传的是无类型浮点常量装箱后的值）。
- 存在的理由（源码注释）：让声明**点名它的那一块**，而不是去数一个会被新增变体移动下标的数组位置。`m4-summary.json` 每个量化变体一条 ratio、`m2-summary.json` 每个 sweep 步一个命中率，光靠 `path` 会指到「第几个」。

`singleNumber` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func singleNumber(t *testing.T, doc any, find map[string]any, path []string) float64`，`t.Helper()`。
- 行为：`len(find) == 0` 时 `values = numbersAt(doc, path)`；否则对 `findObjects(doc, find)` 的每个对象追加 `numbersAt(object, path)`。若 `len(values) != 1` 就 `t.Fatalf("expected exactly one value at %s, found %d", strings.Join(path, "."), len(values))`；否则返回 `values[0]`。
- 它是「我确定这里只有一个数」的断言式读取，被表后那三段算术（比值、切分、token 求和）使用。误报/漏报边界：找到零个与找到多个都直接 Fatal——一个新增变体让 marker 命中两个对象时，这里会立刻停住，而不是安静地取第一个。

`marker` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func marker(find map[string]any) string`。`len(find) == 0` 返回空串；否则对每一对拼 `key + "=" + strconv.Quote(strings.TrimSpace(toString(value)))`，最后返回 `" where " + strings.Join(parts, " ") + ":"`。
- 它只出现在错误消息里（「记录里没有这个值」时用来告诉读者找的是哪一块）。**注意它按 map 迭代顺序拼，所以多键的 marker 文案顺序不稳定**——同一次运行内部可能不同，错误消息不保证逐字节可复现。

`toString` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func toString(value any) string`。`string` 原样返回；`float64` 用 `strconv.FormatFloat(typed, 'f', -1, 64)`（-1 表示用最短且能往返的位数）；`bool` 用 `strconv.FormatBool`；其它类型返回空串。
- 源码注释给的理由正是这条符号存在的意义：**数字与布尔值必须被渲染出来**——记录里选择行是按并发数做的，一条写着 `concurrency=""` 的消息告诉不了读者它本来匹配的是哪一行。

## 16. 基线数字：派生表

`m0Claims` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func m0Claims() []baselineClaim`。它把 RESUME 的 M0 对比表逐格钉住，是唯一一张**按行形状**生成声明的表。
- 内部有个小类型 `cell struct { claim baselineClaim; readme string }`，以及一个 8 行的 `rows` 表，每行是「标签 + 并发数 + 是否流式」：直连非流式 8、直连非流式 32、经网关非流式 8、经网关非流式 32、直连流式 8、直连流式 32、经网关流式 8、经网关流式 32。
- 每行产生 6 个或 8 个 cell：`qps`、`qps_min`、`qps_max`、`p50`、`p95`、`p99`，流式两行另有 `ttft_p50` 与 `ttft_p95`。延迟类的 cell 带 `scale`（毫秒级用 1e-3、微秒级用 1e-6），因为记录里存的是秒。
- 装配时统一设 `claim.file = "m0-baseline.json"`、`claim.find = {"label": row.label, "concurrency": row.concurrency, "stream": row.stream}`、`claim.phrase = row.phrase`。带 `readme` 措辞的 cell 声明两个文档（README.md 用那句措辞、docs/RESUME.md 用默认措辞），不带的就只有 RESUME——即 `resumeOnly`。
- 另外一份拷贝：每行若有 `usage` 措辞（docs/USAGE.md 用更窄的表印其中六个 phase），就对 `path` 恰为 `["qps"]` 的那个 cell 再生成一条只点名 docs/USAGE.md 的声明。源码注释提醒：**USAGE 显示的格子是中位数**，所以那几行各自要有自己的措辞被检查。
- 误报/漏报边界：因为每行的**措辞是作为一个整字符串**被检查的，所以一行改了形状——即使移动的不是本条目读的那个格子——也会失败。这是刻意的：一张表是一个整体声明。

`m4SecondCopies` ([internal/repofmt/baseline_test.go](../../internal/repofmt/baseline_test.go))

- 签名 `func m4SecondCopies() []baselineClaim`。它钉住 M4 量化读数的**另外两份拷贝**：docs/USAGE.md 用更少的列印同样三行，docs/DESIGN.md 把这些读数印成散文。RESUME 那一份已经在主表里逐格查过了，而这两份此前无人看着——「其中一个数动了正是无人看着的漂移」。
- 内部 `type copyClaim struct { variant string; path []string; quote string; digits int; doc string; phrase string }`，共 6 条：三行（fp16 / awq / gptq）的 `ttft_ms.p50` 对 USAGE 表，三条 DESIGN 句子分别读 `total_ms.p50`（fp16）、`throughput.output_tokens_per_s_request_wall`（awq）、`total_ms.p50`（gptq）。
- 装配成 `baselineClaim` 时统一 `file: "m4-summary.json"`、`find: {"variant": ...}`、`path`、`digits`、`docs: [{doc, phrase}]`。**每条把整行 USAGE 或它所在的那句 DESIGN 当作措辞**，于是列错位、或被改写的句子即使本条目读的是别的字段也会失败。
- 误报/漏报边界：措辞里含 Markdown 表格的竖线与加粗标记，所以表格样式一变（列顺序、对齐空格、加粗）就会红——是刻意的严格：那几行本身就是被声明的产物。

主表本身还有几个不属于辅助符号、但决定判定面的点：`claims` 的条目数少于 152 就 `t.Fatalf("only %d claims; this check has been hollowed out", ...)`；`sum` 为假的声明会用 `distinct := map[float64]bool{}` 判重，**同一个键在它出现的每一处都必须带同一个值**，出现两个不同值就报「carries %d different values …; name the one this claim means」，所以第二份带着不同数的拷贝是失败而不是掷硬币；`sum` 为真的声明则把多个文件（或多处）的值加起来。格式化用 `strconv.FormatFloat(total*factor, 'f', claim.digits, 64)` 与 `quote` 比字符串。表后还有三段**算术**声明，它们是「一句话里含两个读数」的形态：M1 的加速比是两个中位数之比而非独立字段；M4 的「16 个请求分成 8:8」是一个总数加上它怎么分；M6 的「省 11 token/轮」是 prompt 与 completion 两个字段的和。

## 17. CI 接线自检的辅助符号

这一节对应 `TestEveryCurlGateCanFailTheJob`：它把 `.github/workflows/ci.yml` 当数据解析，而不是执行它。源码注释把要保的性质说成两条：

1. **红灯的闸门必须能弄红这个 job**。每个闸门步骤都带 `continue-on-error: true`，好处是一个红灯不掩盖其它灯；代价是 GitHub 文档明说的那句话——「当带 continue-on-error 的步骤失败时，outcome 是失败，但最终 conclusion 是成功」。如果每个闸门步骤都带这个标志、却没有任何一步被允许失败，那么所有断言全红时，job、run、徽章、匿名 jobs API 都会报成功，而那些 step conclusion 恰是仓库外的人唯一能看到的表面。
2. **检查被告知要期待的那串闸门，必须就是真正在跑的那串**。一边改了名字而另一边没改，会导致永远红；反方向更糟——一个悄悄脱线的闸门会因为没有人在等它而被报成绿。

`curlGateSteps` ([internal/repofmt/curl_gates_test.go](../../internal/repofmt/curl_gates_test.go))

- 签名 `func curlGateSteps(t *testing.T, workflow string) []string`，`t.Helper()`。返回 `ci.yml` 里 `curl-gates` 这个 job 的每个 step（原文拼接，含缩进）。
- 行为：以字面 `"\n  curl-gates:"` 定位 job 起点，找不到就 `t.Fatal("ci.yml no longer declares a curl-gates job")`。然后从该 header 之后按行扫，三种终止/切分规则：
  - 非空行且**不以空格开头**，说明到了下一个顶层键，break；
  - 恰好两个空格开头（`  ` 但不是 `   `、也不是 `  #`），说明到了同级的另一个 job 键或 job 级注释，break；
  - `      - `（六个空格加短横线加空格）开头的行是**一个新 step**：先把已经累积的 `current` 收进 `steps`，再 `Reset` 重新开始。
  其余行都累加进当前 step。收尾若 `current` 非空再收一次。`len(steps) == 0` 就 `t.Fatal("the curl-gates job has no steps; this check would otherwise pass on an empty job")`。
- 误报/漏报边界：它是一个**按缩进猜测结构的小状态机，不是 YAML parser**。缩进层级被硬编码（job 两根空格、列表项六根空格加 `- `），所以 workflow 改成四空格缩进、或把 step 写成 flow 形式，都会让切分失效；失效方向是「找到零个 step」→ Fatal，而不是安静地通过。它不做 YAML 解析是有意的：本包无依赖，不为测试引入 `go.yaml`。

同文件里的三个包级正则与一个包级变量界定了「怎么认出一个闸门」：

- `gateNameRe = regexp.MustCompile(`run-(?:gate|go-verify)\.ps1[^\n]*?-Name\s+(\S+)`)`——一个 step 跑了哪个闸门、以什么名字登记自己。两种包装器都匹配：`scripts/lib/run-gate.ps1` 驱动一个 PowerShell 脚本，`scripts/lib/run-go-verify.ps1` 驱动七个 Go 里程碑。
- `gateExpectRe = regexp.MustCompile(`summarize-gates\.ps1[^\n]*?-Gates\s+(\S+)`)`——那个「被告知要期待哪些闸门」的步骤。主测试要求它**有且只有一个**（`checkers != 1` 就 Fatal），并且这一步**不能**带 `continue-on-error`（带了就没有任何步骤能让 job 红），还必须带 `if: always()`（否则在它最该跑的场景——前面某步根本没跑、没留下标记——反而被跳过）。
- `gatePackageRe = regexp.MustCompile(`run-go-verify\.ps1[^\n]*?-Package\s+(\S+)`)`——Go 闸门被要求跑哪些里程碑；值按逗号切开逐个 TrimSpace 收集。
- `goVerifyPackages = []string{"./cmd/verify", "./cmd/verify-m1", … "./cmd/verify-m6"}`——在进程内跑的验收链（七个入口）。源码注释说它**是写死的而不是从目录推导出来的**，理由是：让其中一个掉出列表必须是一次测试失败——一个不再被运行的里程碑，与一个通过的里程碑在没有东西知道它本该运行时是无法区分的。

对每条 `continue-on-error` 的判定是 `strings.Contains(step, "continue-on-error: true")`——**字面匹配**，所以写成 `true` 之外的任何等价形式（`"true"` 加引号、`yes`）都不算数。`names` 少于 9 就 Fatal（八个 curl 脚本加上那个驱动七个里程碑的 Go 闸门步骤）；`strings.Join(names, ",")` 必须等于 `-Gates` 那一串；Go 包列表两侧排序后必须相等（因为 step 里的顺序是里程碑顺序、不是字典序）。最后还有一条跨文件的约定：`scripts/lib/run-gate.ps1`、`scripts/lib/run-go-verify.ps1`、`scripts/lib/summarize-gates.ps1` 三个文件都必须提到 `gate-$name.exit`（大小写不敏感）。那是闸门退出码唯一的传递通道——文件，因为环境变量不跨步骤，子进程也写不了父进程的 `$GITHUB_OUTPUT`。

## 18. 工作流 YAML 与 MSYS 启动失败

`plainScalarColonProblems` ([internal/repofmt/workflow_yaml_test.go](../../internal/repofmt/workflow_yaml_test.go))

- 签名 `func plainScalarColonProblems(content string) []string`。它返回 YAML 里「不加引号的 plain scalar 值含有冒号加空格」的每一条问题（含 1 基行号与该行内容）。
- 背景写在文件顶部的长注释里：一次提交把一个带冒号的英文标题直接写进 `run:` 行，整个 workflow 文件不再是合法 YAML；**GitHub 解析不了的 workflow 会产生零 job 的 run**——状态是 completed/failure、显示名回退成文件路径、创建时间与更新时间相同、什么都不执行。于是连续六个提交里「每次推送看起来都红，其实什么都没跑」。本包没有依赖（没有 `go.sum`），也不会为测试引入依赖，所以只把被破坏的那**一条**规则机械化：未加引号的 plain scalar 值不得含冒号加空格。
- 行为：一个按空白分隔的小状态机，不是 parser。逐行处理，先 `strings.TrimRight(rawLine, "\r")`。空行 continue（处于块标量内部时也 continue）。算 `trimmed` 与 `indent`（行首空白长度）。若在块标量里（`blockIndent >= 0`）：缩进更深则 continue（块标量内部的行），否则退出块标量模式。以 `#` 开头的行 continue。以 `- ` 开头则去掉列表项前缀。找第一个冒号：没有、或在位置 0、或 key 含引号/含冒号，就 continue；**只有 `key:` 或 `key: value` 才是 mapping**——冒号后面若紧跟一个非空白字符（例如 `https://x`），continue。取冒号后的值并 `TrimLeft` 空白：空则 continue；首字符是 `|` 或 `>` 就进入块标量模式（记下当前缩进）并 continue；首字符是 `"`、`'`、`[`、`{`、`&`、`*`、`!`、`?` 之一就 continue（分别是带引号标量、flow collection、锚点、别名、标签、显式 key 指示符）。最后若值里含 `": "` 或以 `:` 结尾，就追加一条问题。
- 误报/漏报边界：它是**按行**的启发式，不认识多行 flow collection，也不认识 YAML 的其它标量风格；被它放过的形状（比如一个未加引号的值里带冒号但冒号后紧跟别的东西）不会报错。它报出的每一条都确实是会让整个文件不可解析的那一类，所以**不误报**；漏报的方向是把「本可以更精确的判定」留给真正的解析器——而真正的解析器在这里不可用（无依赖）。
- 调用者：`TestWorkflowYAMLIsParseable`，对 `.github` 下 walk 出来的每个 `.yml`/`.yaml` 跑一遍，逐条 `t.Errorf("%s: %s", filepath.ToSlash(rel), problem)`；一个文件都没找到就 Fatal。`TestPlainScalarColonDetection` 用一段真实的破损片段（含 `GH_TOKEN: ${{ github.token }}` 和带冒号的 run 标题）与八个合法形状钉住它的判定面：修好后的形态（标题放进 `env:` 的一个带引号变量）、带引号且含冒号空格的值、块标量里的 shell（含 `echo "failure: build"` 与 `for pkg in $(go list ./...); do`）、折叠块标量 `>-`、URL、flow sequence、无冒号空格的表达式（`ci-${{ github.ref }}`、`failure()`）、朴素的无冒号值。

`msysStartupFailure` ([internal/repofmt/summarize_fragment_test.go](../../internal/repofmt/summarize_fragment_test.go))

- 签名 `func msysStartupFailure(out []byte) bool`。实现是 `text := string(out)`，返回 `strings.Contains(text, "NtCreateDirectoryObject") || strings.Contains(text, "0xc0000142")`。
- 它回答的问题：一次 `bash` 调用的失败，是**被调用脚本的行为**还是**shell 自己没能起来**。背景是 MSYS2 的 Git Bash 在多测试二进制并行时会因为 `\BaseNamedObjects` 下的目录对象被拒而启动失败（表现为 `NtCreateDirectoryObject` 与状态码 `0xc0000142`）。两个字符串都接受，因为目录对象名是 MSYS2 的、状态码是 Go 报告的形式，同一次失败可能只留下其中一个。
- 调用者：`TestSummarizeKeepsWhyATestFailed`——脚本失败且这个函数为真时**重跑一次**；`TestMSYSStartupFailureMatchesOnlyTheShellsStartup` 用四个用例钉住它只吸收 shell 启动失败：观察到的 MSYS2 启动错误为真、`exit status 0xc0000142` 为真、脚本不存在时的 `exit status 127` 为假、summarizer 自己打印的用法行为假。
- 误报/漏报边界：它是**字符串包含**，如果一个真的 summarizer 缺陷恰好把这两个串打印出来，会被误当成环境问题而重跑一次——这只会让测试多跑一遍，不会掩盖失败（重跑仍会失败并断言）。反方向：一个只留下其它 MSYS2 痕迹的启动失败不会被吸收。

`transcript` ([internal/repofmt/summarize_fragment_test.go](../../internal/repofmt/summarize_fragment_test.go))

- `const transcript` 是一个多行字符串常量：一次真实摘要任务发布的日志形态。它含 `=== RUN` 行、一条断言失败行、`--- FAIL: TestDropClosesWithoutAResponse (0.00s)`、一段完整的 DATA RACE 报告（写位置与读位置、goroutine 号、包路径、源文件位置、创建栈）、另一条失败测试行、一行必须被过滤掉的 harness 噪声、一行失败包、一行通过包。
- 它不是一个函数，而是两个测试共用的**输入数据**：`TestSummarizeKeepsWhyATestFailed` 把它写成日志文件喂给 `scripts/ci-summarize-go-test.sh`，再断言产出的 fragment 含八项（失败测试名、其下的原因行、失败包行、race 报告的首次访问位置、`Previous read` 行、`Goroutine … created at:` 行、那条锁缺失的原因行、通过包行），并且**不得**含 harness 噪声——一个把全部内容都留下的摘要就是日志本身，摘要的意义就在于它删掉了什么。

## 19. 搬家教训：相对链接相对于文档自己

`resolveLinkTarget` 的调用点是 `resolveLinkTarget(filepath.Dir(file), path)`——**解析基准是做出声明的文档自己所在的目录**。由此有一条必须写下来的教训：

一份文档从更深的一层目录（例如 `docs/WALKTHROUGH/`）移出去、或从别处移进 `docs/WALKTHROUGH/`，它里面所有以相对路径写成的链接都会**同时改变含义**。一份原本放在仓库根下、写着 `../../internal/repofmt/...` 的文档搬进 `docs/WALKTHROUGH/` 之后，同一个字符串会从「仓库根往上两层」变成「越过仓库根两层」，于是全部断链；反方向搬家同样会把原本正确的链接变成指向别处的路径。

`TestEveryMarkdownLinkResolvesInTheTree` 正是靠 `resolveLinkTarget` 的返回值判定，所以搬家导致的断链**不会**被静默放过——它会在链接解析这一步报错，而不是等到读者点开才发现。两个操作性的推论：

- 搬家是一步**同时**要做两件事的操作：移动文件，并按新位置重写它内部所有相对链接的 `../` 层数。写完不能只看链接文字，要看解析结果。
- `docs/WALKTHROUGH/` 下的分册指向仓库根时必须写 `../../`：本册的每个锚点链接都遵守这条，例如 `collectSymbols` ([internal/repofmt/citedsymbols_test.go](../../internal/repofmt/citedsymbols_test.go)) 里的目标从本册所在目录出发正好落在仓库根下的 `internal/`。而指向同一目录里另一册的链接（如 `07-loadtest-measure-deploy.md`）不需要任何 `../`。

## 20. 未能确认清单

- 用户清单里的 `print` 在源码里没有对应符号。`internal/repofmt/baseline_test.go` 里的是**方法** `prints`（接收者 `baselineClaim`，签名 `func (claim baselineClaim) prints() []claimPrint`）。本册按源码的 `prints` 讲。
- 用户清单把 `baselineClaim` 记成「11 字段」，但源码里该结构体只有 10 个字段（`quote`、`file`、`files`、`sum`、`phrase`、`docs`、`find`、`path`、`scale`、`digits`）。本册按源码的 10 个字段讲，并把这处出入记在这里。
- 用户清单的 `treeNames`、`packageDirs`、`isFence`、`fencedBlockHolding` 没有被点名归到某一类（`packageDirs`/`treeNames` 被放到「杂项」）；按源码它们都定义在 `internal/repofmt/inventory_test.go`，本册按实际归属讲（见本册第 12 节）。
- `m4prices_test.go` 与 `race_recipe_test.go` 里**没有**任何辅助符号定义：这两个文件只用 `repoRoot` 与 `readFile`，其余逻辑全部内联在各自的测试函数体里。用户清单里的符号没有遗漏在这两个文件中的。
- `transcript` 常量（`internal/repofmt/summarize_fragment_test.go`）的逐行全文未能确认：本册只按它被断言引用的那些字符串描述它，没有把它当作需要逐字记录的辅助符号展开。
- `curl_gates_test.go` 里三个包级正则（`gateNameRe`、`gateExpectRe`、`gatePackageRe`）与 `goVerifyPackages` 的作用可以从调用处读出，但它们**不是**用户清单点名的符号；本册把它们作为判定面的一部分顺带记录，未逐字段展开。
- 其余用户清单里点到的符号（`collectSymbols`、`internalPackages`、`receiverName`、`typeName`、`symbolPackage`、`symbolType`、`proseSurfaces`、`withoutFencedCodeBlocks`、`withoutHereStrings`、`fencedBlockHolding`、`commentSurfaces`、`linePinPhrase`、`lineAround`、`resolveLinkTarget`、`headingAnchors`、`countCheckSites`、`functionBody`、`testFuncLine`、`checkSite`、`assertHelper`、`assertCall`、`assertHelperArities`、`findAssertCalls`、`scanStatement`、`parenGroup`、`countParamList`、`collapse`、`bindingCounts`、`namedArg`、`needMoreInput`、`parseBaseline`、`findObjects`、`numbersAt`、`singleNumber`、`claimDocs`、`claimPrint`、`m0Claims`、`m4SecondCopies`、`repoRoot`、`readFile`、`document`、`wording`、`marker`、`toString`、`curlGateSteps`、`countsKey`、`joinInts`、`lineOf`、`milestoneCounts`、`atoi`、`packageDirs`、`treeNames`、`msysStartupFailure`、`plainScalarColonProblems`、`isFence`）都已在源码里找到并逐条讲过。

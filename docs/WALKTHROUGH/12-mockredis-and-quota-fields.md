# 第 12 册 · RESP2 模拟服务的命令处理器与额度策略字段

## 1. 本册范围与读法

第 04 册（`04-cache-quota-memory.md`）已经讲过 `internal/mockredis` 的整体形状与 `internal/quota` 的三态记账。它守住了机制，但留下了两类**性质不同**的缺口：

- **第一类：真实存在、但被跳过的函数。** [internal/mockredis/mockredis.go](../../internal/mockredis/mockredis.go) 里的 RESP2 命令处理器与输出函数有一批只以"支持的命令集"清单形式出现过，从没有逐条展开：`cmdAuth`、`cmdGet`、`cmdSet`、`cmdSetNX`、`cmdSetEX`、`cmdExists`、`cmdTTL`、`cmdType`、`cmdKeys`、`cmdHSet`、`cmdHDel`、`cmdZAdd`、`cmdZCard`、`cmdZRem`、`cmdZRemRangeByScore`、`infoText`。这些名字在仓库里**真实存在**，只是第 04 册没有落到它们身上。
- **第二类：名字出现过、但没有语义。** `internal/quota` 的符号大多在第 04 册里被提过名，却塌陷成"一行列举字段"：`quota.Policy` 的导出字段、`atomics` 的计数器、`Reason` 常量族、`RetryAfter` 与 `Overshoot*`、三张账本计数、窗口与桶、`checkAnomaly` 与异常基线、`minuteTTL` 一族。

### 与第 04 册的分工

本册**不重复**第 04 册已经讲透的机制，具体是：

- 不重讲 `internal/mockredis` 的整体 RESP2 解析路径（`readCommand` 的逐字节解析、CRLF 严格校验、`readLine` 的取舍）、单锁模型（`Server.mu` 保护哪些表、`getHash`/`getZSet` 为什么返回副本）、`cmd/miniredis` 的启动参数与 `-max-keys` 的 LRU 语义。
- 不重讲 `internal/quota` 的三态记账流程（Admit 预留 → Settle 结算 / Release 归还）与 fail-closed 的默认取向。

本册只做两件事：**把 16 个 mockredis 符号逐函数摊开**，**把 `internal/quota` 的每个字段、常量、桶与辅助函数逐条落到语义、单位、生命周期与后果上**。

### 读法

- 每个函数一节，签名照抄源码；小节顺序按"服务哪条命令 → 参数怎么解析 → 返回什么形状 → 动了哪张表 → 过期怎么处理 → 非法参数回什么 → 故意不实现什么"。
- 源码注释里解释"为什么"的句子照译，并标注为**源码注释观点**；本册作者的推断标注为**推断**。
- 有疑问的地方一律写"未能确认"，不猜。
- 涉及测试的名字都先在仓库里确认过存在；本册**没有执行任何构建或测试**，所有结论都来自源码与测试文本的静态阅读。

## 2. 十六个符号的共同约定

在逐个展开之前，先把这 16 个符号共享的约定一次性钉住，后面每一节只讲它自己的差异。

**签名清单（照抄源码）：**

```
func (s *Server) cmdAuth(c net.Conn, args []string) string
func (s *Server) cmdGet(args []string) string
func (s *Server) cmdSet(args []string) string
func (s *Server) cmdSetNX(args []string) string
func (s *Server) cmdSetEX(args []string) string
func (s *Server) cmdExists(args []string) string
func (s *Server) cmdTTL(cmd string, args []string) string
func (s *Server) cmdType(args []string) string
func (s *Server) cmdKeys(args []string) string
func (s *Server) cmdHSet(args []string) string
func (s *Server) cmdHDel(args []string) string
func (s *Server) cmdZAdd(args []string) string
func (s *Server) cmdZCard(args []string) string
func (s *Server) cmdZRem(args []string) string
func (s *Server) cmdZRemRangeByScore(args []string) string
func (s *Server) infoText() string
```

**共同约定：**

1. **返回值已经是完整的 RESP2 报文**。除 `infoText` 外，每个处理器的返回值都是一个已经编码好的字符串（`+…\r\n` / `-…\r\n` / `:…\r\n` / `$…\r\n` / `*…\r\n`），由 `dispatch` 的调用方一次性写回连接。`infoText` 是唯一的例外：它返回**未编码的正文**，由 `dispatch` 用 `encodeBulk` 包成一条 bulk string。
2. **参数个数错误一律走 `arity(cmd)`**，产出 `ERR wrong number of arguments for '<小写命令名>' command`。注意 `arity` 收的是**调用方传进来的命令名**，所以 `cmdTTL` 这类"一条函数服务两条命令"的处理器必须把原始 `cmd` 透传进来，否则 `PTTL` 会报成 `ttl`。
3. **锁的边界是函数级**。要么在函数里 `s.mu.Lock()` + `defer s.mu.Unlock()`，要么在**调用访问器**（`getString`/`getHash`/`getZSet`）时让访问器自己拿锁。任何一个处理器都不同时做这两件事——重复加锁会死锁（`sync.Mutex` 不可重入）。
4. **懒过期有三条路径**，每个处理器只能从其中选一条，本册会逐条点名：
   - `getString`：字符串专用，过期就地 `delete(s.strs, key)` 与 `delete(s.access, key)`、`s.stats.Expired++`，命中时 `touch(key)`；
   - `getHash` / `getZSet`：哈希与有序集合专用，先 `expireLocked(key)`，再返回**副本**；
   - `keyExistsLocked`：跨类型存在性判断，会懒过期一个过期字符串，但**只判断不返回数据**；
   - **直接读表**（`s.strs[k]` / `s.hashes[k]` / `s.zsets[k]`）：**不做懒过期**。
   第 4 条的最后一种情况是本册最有信息量的发现来源——`cmdExists`、`cmdType`、`cmdTTL`、`cmdHDel`、`cmdZRem`、`cmdZRemRangeByScore` 都直接读表，因此都能看见"已过期但还没被 `sweepLoop` 扫到"的键。
5. **写命令的两件收尾动作**：`s.touch(key)`（只在 `MaxKeys > 0` 时记 `s.access[key] = s.clock`）与 `s.evictLocked()`（只在写之后跑一次近似 LRU 淘汰）。**读命令一律不调用它们**，`cmdExists`/`cmdTTL`/`cmdType`/`cmdKeys`/`cmdZCard` 都是纯读。
6. **`dispatch` 的分发位置**决定了谁不受鉴权门限制：`AUTH` 在 `NOAUTH Authentication required.` 之前单独处理；`INFO`/`DBSIZE`/`FLUSHDB`/`FLUSHALL`/`PING`/`ECHO`/`SELECT`/`CONFIG`/`COMMAND`/`QUIT` 在第一个 `switch` 里；本册的其余处理器都在第二个 `switch`（源码注释观点：**"下面这些就是缓存 store 用到的命令"**）里，因此**必须已通过鉴权**才可能被走到。

## 3. cmdAuth —— 唯一的按连接状态

**签名：** `func (s *Server) cmdAuth(c net.Conn, args []string) string`

**服务命令：** `AUTH`。它是**唯一接收 `net.Conn` 的处理器**（除 `dispatch` 自己），因为"谁已经通过鉴权"是**按连接**的状态。

**参数与顺序：** 三个判断的先后顺序本身就是语义，照抄：

1. `s.opts.RequirePass == ""` → 直接回 `ERR Client sent AUTH, but no password is set`。**这条判断在最前面**，所以一个没配密码的服务器上，任何 `AUTH` 形状——哪怕参数个数也不对——得到的都是这条错误，而不是 arity 错误。
2. `len(args) != 2` → `arity("AUTH")`。
3. `args[1] != s.opts.RequirePass` → `WRONGPASS invalid username-password pair or user is disabled.`（原文带句点）。

**成功路径：** 在 `s.mu` 下写 `s.authOK[c] = true`，回 `+OK`。`s.authOK` 是 `map[net.Conn]bool`，条目在连接关闭时由 `handle` 的 defer 删除——**源码注释观点（第 04 册已引）**：状态属于连接，不属于服务器。

**键空间与过期：** 都不涉及。AUTH 不碰 `strs`/`hashes`/`zsets`/`expires`/`access`，也不推进 `s.clock`——`s.clock++` 发生在 `dispatch` 开头，对每条命令都发生，与 AUTH 无关。

**故意不实现：**

- **只支持单密码，不支持 ACL 用户名。** 真实 Redis 6+ 的 `AUTH username password` 是三参数形式，这里 `len(args) != 2` 会把两参数的 `AUTH user pass` 判成 arity 错误。源码注释观点：模拟器存在的理由是让 `internal/redis` 的握手路径能跑通，不是复刻权限模型。
- **没有 `AUTH` 之后的权限分级**：通过即全权，没有 `NOPERM`/`NOAUTH` 之外的拒绝理由。

## 4. cmdGet —— 懒过期的样板

**签名：** `func (s *Server) cmdGet(args []string) string`

**服务命令：** `GET`。

**参数：** `len(args) != 2` → `arity("GET")`。

**返回形状：** 命中 → `encodeBulk(v)`（`$<字节数>\r\n<值>\r\n`）；未命中 → `encodeNilBulk()`，即 `$-1\r\n`。注意它**没有**用 `encodeArrayRaw`，因为 GET 的回复不是数组。

**读写状态：** 全部委托给 `getString(args[1])`。这条访问器是懒过期的样板：

- 键不存在 → `("", false)`；
- 键存在但 `s.expired(v.expire)` → 就地 `delete(s.strs, key)`、`delete(s.access, key)`、`s.stats.Expired++`，然后当作未命中；
- 命中 → `s.touch(key)` 之后返回值。

**过期处理：** 走的是上面第 4 条里的第一条路径，也就是说 GET 是**会清理**过期键的。这与下面 `cmdExists`/`cmdType` 形成对照。

**故意不实现 / 与真实 Redis 的差异：**

- **对哈希键或有序集合键做 `GET` 回 nil，而不是 `WRONGTYPE`。** 真实 Redis 会回 `WRONGTYPE Operation against a key holding the wrong kind of value`。`cmdGet` 只看 `s.strs`，不看 `s.hashes`/`s.zsets`。这是"故意不实现类型检查"的直接后果，本仓库内没有依赖该错误的调用方。
- **没有 `GETEX`、没有 `GETDEL`、没有 `STRLEN`/`SUBSTR`**：它们都不在 `dispatch` 的分发表里，会落到 `ERR unknown command '…'`。

## 5. cmdSet —— 选项最多、差异也最多

**签名：** `func (s *Server) cmdSet(args []string) string`

**服务命令：** `SET`。

**参数解析：** `len(args) < 3` → `arity("SET")`；`key, val := args[1], args[2]`；随后从下标 3 开始逐项扫描 `strings.ToUpper(args[i])`：

| 选项 | 行为 | 非法输入的错误 |
| --- | --- | --- |
| `EX` | 缺后继参数 → `ERR syntax error`；`strconv.Atoi` 失败 → `ERR value is not an integer or out of range`；成功则 `expire = time.Now().Add(n 秒)`，并把 `i` 前进一格 | 上述两条 |
| `PX` | 同上，单位换成毫秒 | 上述两条 |
| `NX` | 置 `nx = true` | — |
| `XX` | 置 `xx = true` | — |
| `KEEPTTL` | **接受但完全忽略**，注释原文：**源码注释观点——"缓存里没有任何东西依赖跨写保留 TTL，静默接受比让 store 某天可能发的命令失败要好"** | — |
| 其他 | — | `ERR syntax error` |

**关键顺序**：整个选项扫描发生在**加锁之前**，因此语法错误不会触碰任何表；`expire` 的绝对时刻也是在解析期算出来的。

**存在性判断（锁内）：** 先看 `s.strs[key]`；不存在时再看 `s.hashes[key]` 与 `s.zsets[key]`（源码注释：**"一个键还可能以哈希或有序集合存在，EXISTS 会这么说"**）。这处跨类型判断意味着 `SET key v XX` 会**认**一个同名哈希键的存在。

**返回形状：** `(nx && exists) || (xx && !exists)` → `encodeNilBulk()`，也就是**回复 nil 且不写入**（真实 Redis 的同名语义）；否则写入后回 `+OK`。

**写什么：** `s.strs[key] = &stringVal{value: val, expire: expire}`，然后 `delete(s.hashes, key)`、`delete(s.zsets, key)` —— **一次 SET 会把同名哈希与有序集合整个抹掉**，这正是类型覆盖语义。收尾 `s.touch(key)` + `s.evictLocked()`。

**过期处理：** 新写字符串的 deadline 完全来自本次解析出的 `expire`；没给 `EX`/`PX` 时是零值（永不过期）。因为 `KEEPTTL` 被忽略，**`SET` 永远不会保留旧 deadline**。

**故意不实现：**

- **`KEEPTTL` 被忽略**（见上表）。真实 Redis 会保留旧 TTL。第 04 册已记录这条差异；这里补一条**后果**：任何"用 `SET` 刷新值、但希望窗口不重置"的用法在这个模拟器上都会把 deadline 清掉。本仓库里没有这样的用法——配额窗口一律用 `INCRBY`/`SETEX`，不用 `SET`。
- **没有 `SET … GET`、`EXAT`/`PXAT`、`IDLE`、`GETDEL`**：全部落到 `ERR syntax error`。
- **`NX` 与 `XX` 同时给不会报错**：两者都为真时 `(true && exists) || (true && !exists)`，即**只要键存在或不存在都回 nil**——实际上什么都写不进去。真实 Redis 把 `NX`+`XX` 视为语法错误。这是一个实现里存在、但没有测试钉住的分支，见「未能确认」。

## 6. cmdSetNX —— 唯一的互斥原语

**签名：** `func (s *Server) cmdSetNX(args []string) string`

**服务命令：** `SETNX`。源码注释观点：它是"对一个键的全有或全无的占用，是这个服务器提供的**唯一**互斥原语（这里没有 `MULTI`/`EXEC`，也没有 `WATCH`）"，并且**"过期是被遵守的，所以一个死掉的键是可以被占用的"**。

**参数：** `len(args) != 3` → `arity("SETNX")`。

**锁内顺序：** `s.expireLocked(key)`（先把可能已过期的哈希/有序集合清掉）→ `s.keyExistsLocked(key)`（再判断任意类型的活键是否存在，它是会懒过期字符串的那条路径）→ 存在则 `encodeInt(0)`，否则写入。

**返回值形状：** `:0` / `:1`（整数回复，不是 `+OK`）。

**写什么：** `s.strs[key] = &stringVal{value: val}`（`expire` 为零值，即**永不过期**），并 `delete` 同名的 `s.hashes`/`s.zsets`；收尾 `touch` + `evictLocked`。

**过期处理：** 只体现在**读**的一侧——两种懒过期路径都跑了，所以过期的同名键会被当作不存在从而让出占用权。

**故意不实现：**

- **`SETNX` 不接受任何 TTL**；要带 TTL 只能走 `SETEX` 或 `SET … NX EX n`（后者在这个实现里同样不会过期语义出错，因为 `SET` 自己解析 `EX`）。
- **没有不可重入的持有者语义**：`SETNX` 只回答"此刻有没有"，没有持有者标识、没有过期时间应答、没有原子释放。源码注释观点解释了它为什么够用——`internal/redis` 每次调用借还一条连接，`MULTI/EXEC` 序列会跨好几条连接，而**单条命令在真实服务器上是原子的**。

## 7. cmdSetEX —— 窗口计数器用的那一条

**签名：** `func (s *Server) cmdSetEX(args []string) string`

**服务命令：** `SETEX`。源码注释观点：它是**"配额 store 用来写一个不能活过自己窗口的窗口计数器"**的命令。

**参数与顺序：** `len(args) != 4` → `arity("SETEX")`；然后**先解析 `args[2]`**：

- `strconv.Atoi` 失败 → `ERR value is not an integer or out of range`；
- `n <= 0` → `ERR invalid expire time in 'setex' command`；
- 通过后才有 `key, val := args[1], args[3]`。

顺序上的后果：`SETEX k 0 v` 与 `SETEX k -1 v` 都得到"invalid expire time"，而不是写一个已过期的键。

**返回形状：** 一律 `+OK`（**不是** `SETNX` 那样的整数）。

**写什么：** `s.strs[key] = &stringVal{value: val, expire: time.Now().Add(n 秒)}`，`delete` 同名哈希与有序集合，`touch` + `evictLocked`。**无条件覆盖**，没有 `NX`/`XX` 的余地。

**过期处理：** deadline 由 `setex` 自己设；不像 `cmdSetNX` 那样先 `expireLocked`——因为写覆盖本来就会替换掉旧值。

**故意不实现：**

- **没有 `PSETEX`**（毫秒版），也没有 `SETEX` 的 `NX` 变体。
- **没有 `GET` 选项**。
- **秒数不接受浮点**：`strconv.Atoi` 会拒绝 `1.5`。真实 Redis 也只接受整数，所以这不是差异。

## 8. cmdExists —— 直接读表，因此看得见"未清扫的死键"

**签名：** `func (s *Server) cmdExists(args []string) string`

**服务命令：** `EXISTS`。

**参数：** `len(args) < 2` → `arity("EXISTS")`。**多键形式**：`args[1:]` 全都要数。

**返回形状：** `:n`，`n` 是"存在的键的个数"。

**计数规则（关键）：** 对每个参数依次查 `s.strs`、`s.hashes`、`s.zsets`，命中就 `n++` 并 `continue`（同一个键不会因为同时在两张表里而数两次——实际上也不可能同时存在）。**重复的参数会被数多次**：`EXISTS k k` 回 `:2`，与真实 Redis 3.0.3 之后的行为一致。

**读写状态：** 全程在 `s.mu` 下，**只读**，不 `touch`、不 `evictLocked`、不推进任何计数。

**过期处理（本册发现的一处真实差异）：** `cmdExists` **直接读表**，**不做懒过期**。因此一个已过期、但还没被 `sweepLoop` 的每秒扫描清理掉的字符串键，在这里**仍然算存在**。真实 Redis 的 `EXISTS` 会做惰性过期，所以那一刻回 0。后果：在"写入 → 过期 → 立刻 EXISTS"的测试里，模拟器可能给 1 而真 Redis 给 0，差值窗口最长是一次 `sweepLoop` 的 tick（一秒）。本仓库内没有这样的测试。

**故意不实现：** 没有 `EXISTS` 的按类型计数（Redis 也没有）。

## 9. cmdTTL —— 两条命令共用一条实现

**签名：** `func (s *Server) cmdTTL(cmd string, args []string) string`

**服务命令：** `TTL` 与 `PTTL`，由 `dispatch` 把原始 `cmd` 透传进来区分。这是"参数个数错误要报对命令名"的唯一理由：`len(args) != 2` → `arity(cmd)`，于是 `PTTL k extra` 报的是 `'pttl'`。

**取值优先级（锁内，照抄结构）：**

1. `s.strs[args[1]] != nil` → 取该 `stringVal` 的 `expire`；
2. 否则 `s.hashes[args[1]] != nil || s.zsets[args[1]] != nil` → 取 `s.expires[args[1]]`；
3. 否则 → `:-2`（键不存在）。

**返回形状：**

- `expire.IsZero()` → `:-1`（键存在但没有 TTL）；
- `left := time.Until(expire)`，`left < 0` → `:-2`（**同一条"不存在"编码**——这是真实 Redis 的约定：已过期的键与不存在的键都回 -2）；
- `PTTL` → `left.Milliseconds()`；`TTL` → `int64(left.Seconds())`，**向零截断**，所以剩余 900 毫秒的键 `TTL` 回 0，而 `PTTL` 回 900。

**过期处理：** 同样**直接读表**，不做懒过期。由于第 1 步用指针非空判断、第 3 步用 `left < 0` 兜底，一个已过期的字符串键会走到 `:-2`，与真 Redis 的惰性过期结论**一致**；但一个已过期的哈希/有序集合键，其 `s.expires` 条目还在，所以也会走 `left < 0` → `:-2`，同样一致。**这里的差异方向是安全的**（值只可能从"存在"变成"不存在"，而 -2 就是"不存在"）。

**故意不实现：**

- **没有 `TTL` 的毫秒精度**：秒是截断而不是四舍五入。
- **不报告 `expires` 表中存在、但两张数据表都没有的孤儿条目**：第 2 步要求数据表里真的有键，所以孤儿不会变成"存在的键"。

## 10. cmdType —— 固定三个类型

**签名：** `func (s *Server) cmdType(args []string) string`

**服务命令：** `TYPE`。

**参数：** `len(args) != 2` → `arity("TYPE")`。

**返回形状：** **简单字符串**（`+…`），四选一：`+string`（在 `s.strs`）、`+hash`（在 `s.hashes`）、`+zset`（在 `s.zsets`）、`+none`（都不在）。真实 Redis 也是简单字符串，所以形状一致。

**读写状态：** 锁内只读三张表。

**过期处理：** **不做懒过期**，与 `cmdExists` 同一类问题：已过期但未清扫的键会报出它原来的类型，而不是 `none`。

**故意不实现：** 真实 Redis 还有 `list`/`set`/`stream`，这个模拟器根本没有那三张表，所以永远不可能返回它们。

## 11. cmdKeys —— 一次全表扫描与一处防御性前缀过滤

**签名：** `func (s *Server) cmdKeys(args []string) string`

**服务命令：** `KEYS`。

**参数：** `len(args) != 2` → `arity("KEYS")`。

**返回形状：** `encodeArray(out)`——`*<n>\r\n` 后跟每个键的 `encodeBulk`。空结果是 `*0\r\n`（不是 nil 数组）。

**扫描过程（锁内全程持有）：** 闭包 `add(k)` 对每个候选键做两件事：

1. **跳过 `"\x00expire:"` 前缀的键名**。第 04 册已指出这是"实现里存在、但其他地方没有任何代码产生该前缀键"的防御——`expires` 用的是独立 map，不占键名空间。本册补一条性质说明：这条过滤是**不可达分支**，它唯一的作用是"万一未来某个改动把过期簿记塞进 `strs`，KEYS 也不会把它泄漏成用户键"。
2. `matchGlob(pattern, k)` 命中则追加。

然后遍历 `s.strs`、`s.hashes`、`s.zsets` 三张表，最后 `sort.Strings(out)` 排序，**源码注释观点**：Redis 不保证顺序，这里排序只是为了测试确定性。

**过期处理：** **不做懒过期**，遍历的是三张原始表——过期的字符串与过期的哈希/有序集合都会出现在结果里，直到 `sweepLoop` 扫到。（`matchGlob` 本身只支持 `*`，第 04 册已述，本册不重复。）

**故意不实现：** 没有 `SCAN`/`SCAN` 游标、没有 `?` 与 `[...]` 通配、没有 `NOTIFY`/`TYPE` 过滤选项。

## 12. cmdHSet —— HSET 与 HMSET 共用，但错误串只有 `hset`

**签名：** `func (s *Server) cmdHSet(args []string) string`

**服务命令：** `HSET` 与 `HMSET`（`dispatch` 把两者都送到这里）。

**参数：** `len(args) < 4 || len(args)%2 != 0` → `arity("HSET")`。注意**参数个数是奇数**（`HSET key field` 少了值）走的是同一个 arity 分支。承第 2 节的第 2 条：这里**硬编码了 `"HSET"`**，所以 `HMSET k f` 回的是 `ERR wrong number of arguments for 'hset' command`——一条指向**另一个命令名**的错误。真实 Redis 会报 `'hmset'`。属于有意为之（共用一条实现）还是疏漏，见「未能确认」。

**写什么：** 锁内 `h, ok := s.hashes[key]`；不存在就 `h = map[string]string{}` 并挂进 `s.hashes[key]`；然后 `for i := 2; i+1 < len(args); i += 2` 成对写入。`added` 只统计**新字段**（写之前不存在），已存在字段被覆盖但**不计入**。

**返回形状：** `:added`。**这是与真实 Redis 的一处硬差异**：真实 `HSET` 回新增字段数（一致），但真实 `HMSET` 回 `+OK`；这里 `HMSET` 也回整数。

**过期处理：** **完全不碰 `s.expires`**，也不调用 `expireLocked`。后果有两个方向：(a) 对一个**已过期但未清扫**的哈希键 `HSET`，会把字段写回那张仍然在 `s.hashes` 里的旧 map，而 `s.expires` 里的旧 deadline 还在——它会在下一次懒过期或清扫时**整个消失**；真实 Redis 在过期后会当成新键，新写入的字段能活下来。本仓库内没有这样的用法。(b) 对一个**有 TTL 的活键** `HSET`，deadline 被保留（与真实 Redis 一致，是正确行为）。

**收尾：** `s.touch(key)` + `s.evictLocked()`。

**故意不实现：** 没有 `HSETNX`；没有字段级 TTL（Redis 7.4 起才有）；`HMSET` 的 `+OK` 语义没实现。

## 13. cmdHDel —— 删字段，不删键

**签名：** `func (s *Server) cmdHDel(args []string) string`

**服务命令：** `HDEL`。

**参数：** `len(args) < 3` → `arity("HDEL")`。字段名从 `args[2:]` 取，**多字段**。

**返回形状：** `:n`，`n` 是**真的被删掉的**字段个数。键不存在 → 直接 `encodeInt(0)`。

**读什么表：** 锁内**直接读** `s.hashes[args[1]]`，既没有 `getHash` 也没有 `expireLocked`。因此它承第 2 节第 4 条的第四类：**会看见已过期但未清扫的哈希**，并把字段从那上面删掉。后果：`HDEL` 可能"成功"地从一个逻辑上已经不存在的键里删字段并回 `:1`（真实 Redis 会先惰性过期，回 `:0`）。方向上是差异、不是错误。

**写什么（关键性质）：** 它只 `delete(h, f)`，**从不在 `h` 变空时 `delete(s.hashes, key)`**。也就是说：

- 哈希被删空之后**键仍然存在**：`HSET k f v` → `HDEL k f` → 在这个模拟器上 `EXISTS k` 回 1、`TYPE k` 回 `hash`、`HGETALL k` 回空数组。真实 Redis 会在最后一个字段消失时删掉键，三个答案分别是 0、`none`、空数组。
- 空 map 仍然占 `s.hashes` 的一个条目，而 `Stats().Keys`（`len(s.strs)+len(s.hashes)+len(s.zsets)`）会把它数进去。
- `evictLocked` 的容量压力把它当作一个活键看待（**推断**：第 04 册描述的 LRU 采样按 `s.access` 的顺序淘汰，空哈希也会参与）。

**过期处理：** 不设、不改、不清。`HDEL` 不会延长或缩短 TTL，也不会在删空之后顺手删掉 `s.expires` 里的条目——那会留下一个指向空哈希的 deadline。

**故意不实现：** 没有 `HDEL` 的多键形式（Redis 也只接受单键多字段，一致）。

## 14. cmdZAdd —— 选项接受但忽略，错误路径会留空集合

**签名：** `func (s *Server) cmdZAdd(args []string) string`

**服务命令：** `ZADD`。

**参数：** 两道闸。先 `len(args) < 4 || len(args)%2 != 0` → `arity("ZADD")`；然后从 `start := 2` 起跳过选项：

```
// ZADD key [NX|XX|GT|LT|CH] score member ...
for start < len(args)-1 {
	switch strings.ToUpper(args[start]) {
	case "NX", "XX", "GT", "LT", "CH":
		start++
		continue
	}
	break
}
if (len(args)-start)%2 != 0 {
	return arity("ZADD")
}
```

**选项语义：全部被丢弃。** `NX` 不做"仅新增"、`XX` 不做"仅更新"、`GT`/`LT` 不做分数比较、`CH` 不改变返回值的含义。源码注释只把它们列在语法行里，没有解释为什么忽略。**推断**：缓存里没有任何调用方发这些选项，写假的比较语义比不写更危险。

**一个实现细节值得一提：** 循环条件是 `start < len(args)-1`，所以选项可以被跳到**最后一个参数之前**为止；`ZADD k NX XX 1 m` 会把两个选项都吃掉。选项也会被**重复**跳过，且 `NX` 与 `XX` 同时给不报错（与 `cmdSet` 的 `NX`+`XX` 同一类宽容，见第 5 节）。

**返回值形状：** `:added`，只数**新增成员**。真实 Redis 的 `ZADD … CH` 数的是"新增 + 改动"，这里永远按新增算；而 `:0` 同时表示"分数没变"与"CH 语义被忽略"，调用方无法区分。

**一个真实的副作用（错误路径）：** 创建集合发生在**解析分数之前**：

```
z, ok := s.zsets[key]
if !ok {
	z = map[string]float64{}
	s.zsets[key] = z
}
for i := start; i+1 < len(args); i += 2 {
	score, err := strconv.ParseFloat(args[i], 64)
	if err != nil {
		return encodeError("ERR value is not a valid float")
	}
	…
}
```

于是 `ZADD k notafloat m` 在回 `-ERR value is not a valid float` 的同时，**已经在 `s.zsets` 里留下了一个空的有序集合键**。随后的 `EXISTS k` 回 1、`ZCARD k` 回 0（`getZSet` 会拿到空 map）。真实 Redis 不会创建这个键。这是一个能观察到的状态泄漏，仓库里没有测试钉住它。

**读写状态与过期：** 直接读 `s.zsets`，**没有 `expireLocked`**；创建时也不碰 `s.expires`，所以一个过期的旧集合会被"复活"并保留旧 deadline。收尾 `s.touch(key)` + `s.evictLocked()`。

**故意不实现：** 没有 `INCR` 选项（`ZADD k INCR 1 m` 会把 `INCR` 当成分数解析并报 `ERR value is not a valid float`）；没有 `ZADD` 的多键。

## 15. cmdZCard —— 唯一走 getZSet 再重新加锁的处理器

**签名：** `func (s *Server) cmdZCard(args []string) string`

**服务命令：** `ZCARD`。

**参数：** `len(args) != 2` → `arity("ZCARD")`。

**返回形状：** `:n`；键不存在或已过期 → `:0`。

**读写状态（本节的要点）：** 它分成两段，中间**释放了锁**：

```
z, ok := s.getZSet(args[1])     // 拿锁 → expireLocked → 拷贝一份 map → 放锁
if !ok {
	return encodeInt(0)
}
s.mu.Lock()
defer s.mu.Unlock()
return encodeInt(int64(len(z)))
```

`getZSet` 返回的是**副本**（第 04 册已解释为什么不能返回活 map），所以长度必须等到副本拿到之后才能算，而那时锁已经放了——第二次加锁就是这个原因，不是为了保护副本（副本是函数私有的）。源码没有为这段写注释；上述因果是**推断**。

**代价：** `ZCARD` 会**整表拷贝**一个有序集合，只为读一个长度。对配额窗口这种小集合无所谓，对一个几万成员的大集合就是一次 O(n) 分配——真实 Redis 的 `ZCARD` 是 O(1)。这是一个实现选择，仓库里没有大集合的用例。

**过期处理：** 走 `getZSet` 的路径——先 `expireLocked`，所以**它是这一批读命令里唯一会顺手清理过期有序集合的**（对照 `cmdZRem`、`cmdZRemRangeByScore` 与 `cmdType`，它们直接读表）。

**故意不实现：** 没有 `ZLEXCOUNT` 之类的兄弟命令。

## 16. cmdZRem —— 与 cmdHDel 同构

**签名：** `func (s *Server) cmdZRem(args []string) string`

**服务命令：** `ZREM`。

**参数：** `len(args) < 3` → `arity("ZREM")`。成员从 `args[2:]` 取。

**返回形状：** `:n`，真正被删掉的成员数；键不存在 → `:0`。

**写什么：** 锁内直接读 `s.zsets[args[1]]`，逐个 `delete(z, m)`。与 `cmdHDel` 一样**不删键**：删空之后 `s.zsets` 里留一个空 map，于是 `EXISTS` 回 1、`TYPE` 回 `zset`、`ZCARD` 回 0。差别在真 Redis 上应当是 `none`/0。

**读写状态：** 只读+删除。**不改 `s.touch`、不跑 `evictLocked`、不碰 `s.expires`**——一个写操作却不推进 LRU 时钟，这是本批符号里唯一如此的处理（`cmdHDel` 同样如此）。**推断**：两者都是"从容器里移走元素"，作者把它们归成了读加重排，而不是写。

**过期处理：** **无懒过期**，会看得见已过期的集合。

**故意不实现：** 没有 `ZPOPMIN`/`ZPOPMAX`/`ZREMRANGEBYLEX`。

## 17. cmdZRemRangeByScore —— `(` 前缀被吃掉，闭区间语义

**签名：** `func (s *Server) cmdZRemRangeByScore(args []string) string`

**服务命令：** `ZREMRANGEBYSCORE`。

**参数与错误：** `len(args) != 4` → `arity("ZREMRANGEBYSCORE")`；两个边界都走 `parseScoreBound`，任一失败 → `ERR min or max is not a float`。注意这条错误串与 `cmdZAdd`/`cmdZIncrBy` 的 `ERR value is not a valid float` **不是同一条**——两个浮点解析入口给了两种文案，调用方不能靠一条错误串区分来源。

**`parseScoreBound` 的全貌（照抄）：**

```
func parseScoreBound(s string) (float64, error) {
	s = strings.TrimPrefix(s, "(")
	return strconv.ParseFloat(s, 64)
}
```

它**把开区间标记 `(` 剥掉**，然后**没有任何地方记住"这是开区间"**——因为返回的是裸 `float64`。而过滤条件是：

```
if score >= min && score <= max {
	delete(z, m)
	n++
}
```

**两端都是闭区间。** 所以 `ZREMRANGEBYSCORE k (1 1` 在真实 Redis 上删掉 0 个成员（区间是空的），在这里会删掉分数恰为 1 的成员。同样，`(5 (10` 会变成 `[5,10]`。这是本册发现的**最直接的语义偏离**：类型系统丢掉了信息，而调用点无法补回来。仓库里没有依赖开区间的用例。

**读什么表与过期：** 锁内直接读 `s.zsets[args[1]]`，**没有 `expireLocked`**，会看得见已过期的集合。

**返回形状：** `:n`，被删成员数；空区间或键不存在 → `:0`。

**其他性质：** 删空之后**不删键**（同 `cmdZRem`）；不 `touch`、不 `evictLocked`。

**故意不实现：** 没有 `LIMIT offset count`（Redis 的 `ZREMRANGEBYSCORE` 也没有该选项，一致）；`+inf`/`-inf` 靠 `strconv.ParseFloat` 自身支持（`strconv.ParseFloat("+inf", 64)` 合法），而 `-inf` 同样合法——这一点与真实 Redis 一致，但它不是显式实现的。

## 18. infoText —— 未编码的正文，由 dispatch 包成 bulk

**签名：** `func (s *Server) infoText() string`

**服务命令：** 由 `dispatch` 的 `case "INFO"` 调用，唯一调用点是 `return encodeBulk(s.infoText())`。因此它**不吃 `args`**（`INFO` 的 section 参数被忽略，`INFO server` 与 `INFO` 同答），也**不做 arity 检查**。

**返回值形状（照抄格式串）：**

```
# Server\r\nredis_version:7.0.0-mockredis\r\n# Keyspace\r\ndb0:keys=%d,evictions=%d,expired=%d,commands=%d\r\n
```

四个占位依次是 `st.Keys`、`st.Evictions`、`st.Expired`、`st.Commands`，全部来自 `s.Stats()`。

**`Stats()` 提供的两个计算值（照抄）：**

```
st.Keys = int64(len(s.strs) + len(s.hashes) + len(s.zsets))
st.Connections = int64(s.stats.Connections)
```

`Keys` 会在调用时现算，所以空哈希（第 13 节）与空有序集合（第 16 节）也计入 `db0:keys=`。`Connections` 被算出来却**没有出现在 `infoText` 里**。

**几个可观察的性质：**

- **版本号是伪造的**：`redis_version:7.0.0-mockredis`。这个字符串的唯一作用是通过客户端库可能做的版本探测——`internal/redis` 不做版本解析。把 `-mockredis` 留在里面是为了让任何抓包的人都看得出这不是真 Redis。
- **`commands` 计数包含 `INFO` 自己**：`s.stats.Commands++` 发生在 `dispatch` 开头，在 `case "INFO"` 之前，所以第 n 次 `INFO` 报的 commands 已经把它自己算进去了。
- **没有任何过期清理**：`infoText` 只读计数，`expired` 是累计值（`s.stats.Expired`），不是当前存量。
- **`# Keyspace` 段落永远是 `db0:`**，没有 `# Clients`、`# Memory`、`# Replication`、`# CPU`，也没有 `rdb_*`/`aof_*` 字段。真实 Redis 的 `INFO` 有这些；`internal/redis` 不解析它们，所以省略是安全的。
- **换行是 CRLF**，段落标题以 `#` 开头——`internal/redis` 若按 `\r\n` 切行并按 `:` 取值，能正确解析。

**故意不实现：** `INFO <section>` 的参数过滤；`INFO ALL`/`INFO EVERYTHING`；`LATENCY`/`SLOWLOG`/`COMMAND DOCS` 这些运维面（`COMMAND` 在 `dispatch` 里回空数组）。

---

**第二组：`internal/quota` 的字段级补遗**

## 19. 三个文件的顶层声明清单（照抄源码）

本节先把 [internal/quota/quota.go](../../internal/quota/quota.go)、[internal/quota/store.go](../../internal/quota/store.go)、[internal/quota/redis.go](../../internal/quota/redis.go) 三个文件的顶层声明抄全，作为后面逐条解释的索引。方法体不抄，只抄声明形状；注释一律不抄（它们在第 20 节之后逐条翻译）。

### internal/quota/quota.go 的常量

```
const (
	WindowDay     = "day"
	WindowMinute  = "minute"
	WindowSession = "session"
)

const (
	CounterTokens   = "tokens"
	CounterCost     = "cost_micros"
	CounterRequests = "requests"
)

const (
	ActionAllow   = "allow"
	ActionDegrade = "degrade"
	ActionReject  = "reject"
)

const (
	ReasonDisabled      = "disabled"
	ReasonNoStore       = "no-store"
	ReasonWithinBudget  = "within-budget"
	ReasonStoreError    = "store-error"
	ReasonAnomaly       = "anomaly"
	ReasonDailyTokens   = "tokens_per_day"
	ReasonDailyCost     = "cost_per_day_usd"
	ReasonSessionTokens = "tokens_per_session"
	ReasonMinuteRPM     = "requests_per_minute"
)

const DefaultSessionTTL = 24 * time.Hour
const minuteTTL = 2 * time.Minute
const anomalyCheckInterval = time.Minute
const anomalyBaselineDays = 3
```

### internal/quota/quota.go 的类型

```
type Policy struct {
	Tenant            string
	TokensPerDay      int64
	CostPerDayMicros  int64
	RequestsPerMinute int64
	TokensPerSession  int64
	OnExceed          string
	DowngradeModel    string
	MaxTokensCap      int
	AnomalyRatio      float64
	SessionTTL        time.Duration
}

type Config struct {
	Enabled       bool
	Prefix        string
	FailOpen      bool
	DefaultPolicy Policy
	Tenants       map[string]Policy
	Now           func() time.Time
	Log           Logger
}

type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

type Estimate struct {
	PromptTokens     int
	CompletionTokens int
	CostMicros       int64
	Requests         int64
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CostMicros       int64
	Requests         int64
}

type Decision struct {
	Allowed        bool
	Action         string
	Reason         string
	DowngradeModel string
	MaxTokensCap   int
	Limit          int64
	Used           int64
	Requested      int64
	RetryAfter     time.Duration
	Reservation    *Reservation
}

type Reservation struct {
	Tenant  string
	Session string
	entries []entry
	settled bool
}

type entry struct {
	key      string
	window   string
	bucket   string
	counter  string
	ttl      time.Duration
	reserved int64
}

type Stats struct {
	Allowed            uint64
	Degraded           uint64
	Rejected           uint64
	StoreErrors        uint64
	Alerts             uint64
	OvershootTokens    uint64
	OvershootCostMicro int64
	ReservedTokens     uint64
	SettledTokens      uint64
	ReleasedTokens     uint64
	ReleasedCostMicro  int64
}

type Report struct {
	Tenant string
	Policy Policy

	Day            string
	TokensToday    int64
	CostTodayMicro int64

	Minute             string
	RequestsThisMinute int64

	Session       string
	SessionTokens int64

	BaselineTokens float64
	Ratio          float64
	Alerting       bool
}

type Manager struct {
	cfg       Config
	store     Store
	now       func() time.Time
	log       Logger
	stats     atomics
	mu        sync.Mutex
	anomalies map[string]*anomalyState
}

type atomics struct {
	allowed       atomic.Uint64
	degraded      atomic.Uint64
	rejected      atomic.Uint64
	storeErrors   atomic.Uint64
	alerts        atomic.Uint64
	overshoot     atomic.Uint64
	overshootCost atomic.Int64
	reservedTok   atomic.Uint64
	settledTok    atomic.Uint64
	releasedTok   atomic.Uint64
	releasedCost  atomic.Int64
}

type check struct {
	dimension  string
	key        string
	window     string
	bucket     string
	counter    string
	ttl        time.Duration
	delta      int64
	limit      int64
	retryAfter time.Duration
}

type anomalyState struct {
	day      string
	baseline float64
	ratio    float64
	alerting bool
	checked  time.Time
}
```

### internal/quota/quota.go 的函数与方法

```
func New(cfg Config, store Store) *Manager
func (p Policy) Budgeted() bool
func (e Estimate) Tokens() int64
func (u Usage) Tokens() int64
func (m *Manager) Enabled() bool
func (m *Manager) Policy(tenant string) Policy
func (m *Manager) Admit(ctx context.Context, tenant, session string, est Estimate) (*Decision, error)
func (m *Manager) Settle(ctx context.Context, res *Reservation, usage Usage) error
func (m *Manager) Release(ctx context.Context, res *Reservation) error
func (m *Manager) releaseEntries(ctx context.Context, entries []entry) error
func (m *Manager) unavailable(err error) (*Decision, error)
func (m *Manager) Stats() Stats
func (m *Manager) Report(ctx context.Context, tenant, session string) (Report, error)
func (m *Manager) Close() error
func (m *Manager) checks(p Policy, tenant, session string, now time.Time, est Estimate) []check
func (m *Manager) key(tenant, window, bucket, counter string) string
func (m *Manager) baseline(ctx context.Context, tenant string, now time.Time, p Policy) (float64, error)
func (m *Manager) checkAnomaly(ctx context.Context, tenant string, u Usage)
func (m *Manager) warn(msg string, args ...any)
func sanitise(s string) string
func dayBucket(t time.Time) string
func minuteBucket(t time.Time) string
func untilDayEnd(t time.Time) time.Duration
func actualFor(counter string, u Usage) int64
func max64(a, b int64) int64
```

### internal/quota/store.go

```
type Store interface {
	Add(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error)
	Get(ctx context.Context, key string) (int64, error)
	Close() error
}

type MemoryStore struct {
	mu     sync.Mutex
	now    func() time.Time
	values map[string]memoryValue
}

type memoryValue struct {
	value  int64
	expire time.Time // zero means no expiry
}

func NewMemoryStore() *MemoryStore
func NewMemoryStoreWithClock(now func() time.Time) *MemoryStore
func (s *MemoryStore) Add(_ context.Context, key string, delta int64, ttl time.Duration) (int64, error)
func (s *MemoryStore) Get(_ context.Context, key string) (int64, error)
func (s *MemoryStore) Close() error
func (s *MemoryStore) Len() int
```

### internal/quota/redis.go

```
type RedisStore struct {
	cli *redis.Client
}

type RedisOptions struct {
	Addr         string
	Password     string
	DB           int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
}

func NewRedisStore(opts RedisOptions) (*RedisStore, error)
func (s *RedisStore) Add(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error)
func (s *RedisStore) Get(ctx context.Context, key string) (int64, error)
func (s *RedisStore) Close() error
func (s *RedisStore) Addr() string
func (s *RedisStore) Stats() redis.Stats
```

**读法提示：** 上表里 `Store` 只有三个方法，源码注释解释了为什么——**源码注释观点**："配额需要的一切都能表达成『给这个计数器加这个增量』和『读这个计数器』，更宽的接口会诱使调用方写出一个不是原子的读-改-写。"

## 20. quota.Policy 逐字段

**先说一件容易搞错的事：`Policy` 自己没有任何 json 标签。** 它是**运行时**的策略结构，不是配置结构。配置层是 `internal/config/config.go` 的 `TenantQuotaConfig`，两者由 `internal/server/quota.go` 的 `quotaPolicy` 逐字段搬运。所以下面每个字段都给三个坐标：**Policy 字段 → 配置字段与 json 键 → 单位/零值语义**。

**结构体声明（照抄）：**

```
type Policy struct {
	Tenant            string
	TokensPerDay      int64
	CostPerDayMicros  int64
	RequestsPerMinute int64
	TokensPerSession  int64
	OnExceed          string
	DowngradeModel    string
	MaxTokensCap      int
	AnomalyRatio      float64
	SessionTTL        time.Duration
}
```

**类型级注释（照译，源码注释观点）：** "`Policy` 是一个租户的**生效**预算。零限额表示『这个维度不做预算』，**永远不表示『预算为零』**；没有办法表达一个『什么都不能花』的租户，因为那样的租户用『根本不写进文件』表达更好。"

### Tenant —— 租户标识

- **声明：** `Tenant string`；配置侧 `Tenant string \`json:"tenant"\``。
- **单位：** 无，是一个**身份名**。
- **来源（源码注释观点）：** 就是配额被归到的身份——`X-InferGate-Tenant` 头存在时用它，否则用凭据的哈希；并且"它与缓存分域用的身份相同，所以**租户在整个网关里是一个概念**"。
- **零值语义：** 空字符串是一个合法租户名（默认策略就带空 `Tenant`）。它在 `key` 里被 `sanitise("")` 编码成 `"_"`（见第 29 节的 `sanitise`）——所以"未命名租户"和"名字恰好是空"是同一件事，而恰好叫 `"_"` 的租户会被编码成 `"__"`，两者不会撞。
- **后果：** `Policy.Tenant` 只用于展示与查找，**不参与计数键**——键里的租户名来自 `Admit` 的 `tenant` 参数而不是 `Policy.Tenant`。这是一个容易踩的点：配置里 `tenant: acme` 但请求头给 `acme2` 时，权重键是 `acme2` 的，而 `Report` 里回显的 `Tenant` 也是 `acme2`。

### TokensPerDay —— 每日 token 上限

- **声明：** `TokensPerDay int64`；配置侧 `TokensPerDay int64 \`json:"tokens_per_day"\``。
- **单位：** **token 数**（提示 token + 补全 token 之和）。`int64` 而不是 `int`，因为一个繁忙租户的日 token 量在小机器上仍可能超过 32 位。
- **默认值：** 配置默认里是 **0**（config 包的默认值构造只显式设了 `default_policy.on_exceed`，其余字段取零值）。
- **零值语义：** **不限额**。
- **后果：** `>0` 时 `checks` 才会追加一条日 token 维度，key 形态是 `<prefix>:<tenant>:day:<YYYYMMDD>:tokens`，TTL 是 `untilDayEnd(now)`，增量是 `est.Tokens()`，超限理由串是 `ReasonDailyTokens`（`"tokens_per_day"`）。`<=0` 时该维度**完全不存在**：不读不写 Redis，也不出现在 `Report` 的判断里。

### CostPerDayMicros —— 每日花费上限（微美元）

- **声明：** `CostPerDayMicros int64`；配置侧字段名却是 `CostPerDayUSD float64 \`json:"cost_per_day_usd"\``。
- **单位：** **微美元（1e-6 美元）**，整数。配置里写美元浮点，`internal/server/quota.go` 的 `quotaPolicy` 用 `math.Round(usd * 1_000_000)` 换算成微美元。
- **为什么必须是整数（源码注释观点，见 `CounterCost` 常量处的注释）：** "成本以**微美元**整数保存：一个浮点计数器既不能在 Redis 里原子自增，又会在几百万次 INCRBY 上累积舍入误差。"
- **零值语义：** **不限额**。
- **后果：** `>0` 时产生日 `cost_micros` 维度，增量是 `est.CostMicros`，理由串 `ReasonDailyCost`（`"cost_per_day_usd"`——注意理由串说的是 USD，键与统计说的是 micros，两套单位在同一套 API 里并存）。
- **一处精度提示：** `math.Round(usd*1_000_000)` 对 `0.0000004` 这样的配置会舍成 0，于是**这一维度被当成未配置**（零值=不限额），而不是"限额四舍五入到零"。这是零值语义与浮点配置相乘的一个真实陷阱，见「未能确认」。

### RequestsPerMinute —— 每分钟请求数上限

- **声明：** `RequestsPerMinute int64`；配置侧 `RequestsPerMinute int64 \`json:"requests_per_minute"\``。
- **单位：** **次/分钟**，固定分钟桶而不是滑动窗口。
- **零值语义：** **不限额**。
- **后果：** `>0` 时产生分钟 `requests` 维度，增量是 `max64(est.Requests, 1)`——**至少 1**，所以一个把 `Estimate.Requests` 留成 0 的调用方仍然每个请求计 1 次。TTL 是 `minuteTTL`（2 分钟），重试等待是 `time.Minute - now.Second()*time.Second`，也就是到本分钟结束为止（不含亚秒补偿，见第 23 节）。

### TokensPerSession —— 单个会话的 token 上限

- **声明：** `TokensPerSession int64`；配置侧 `TokensPerSession int64 \`json:"tokens_per_session"\``，配置注释："按 `X-InferGate-Session` 归档，**零禁用会话维度**。"
- **单位：** **token 数**，但作用域是**一个会话**而不是一天。
- **零值语义：** 不限额。
- **一个额外的守卫：** `checks` 里的条件是 `p.TokensPerSession > 0 && session != ""`——**会话标识为空时这一维度也不生效**。也就是说，一个配了会话预算但请求头没带会话 id 的部署会**静默地不执行这条预算**，而不是把它当成一个共享的"空会话"桶。这是一个真实的、可见于源码的取巧；理由应当是"把所有无会话请求算进同一个桶"会把不同调用方混在一起（**推断**）。
- **后果：** 键形态 `<prefix>:<tenant>:session:<session id>:tokens`，TTL 取 `SessionTTL`（下面那条），理由串 `ReasonSessionTokens`。

### OnExceed —— 超限时走哪条阶梯

- **声明：** `OnExceed string`；配置侧 `OnExceed string \`json:"on_exceed"\``。
- **取值：** 严格两个——`ActionReject`（`"reject"`）与 `ActionDegrade`（`"degrade"`）。`internal/config/config.go` 的 `validateTenantQuota` 会 `strings.ToLower` 归一化并拒绝其他值，错误串是 `%s.on_exceed: unsupported action %q (want reject or degrade)`（`%s` 是配置路径前缀，如 `quota.tenants[0]`）。
- **空值语义（继承，不是不限额）：** 留空时先继承 `quota.default_policy.on_exceed`；若那里也空，则落到 `QuotaActionReject`。**注意与 `Policy` 的零值语义之间的一处张力**：`quota.Policy{}` 的 `OnExceed` 是 `""`，而空串既不等于 `ActionReject` 也不等于 `ActionDegrade`；`Admit` 的判断是 `policy.OnExceed == ActionDegrade && (DowngradeModel != "" || MaxTokensCap > 0)`，所以 `""` 走的是**拒绝**路径——配置层保证不会把空串送给运行时的 `Policy`（**推断**：`quotaPolicy` 直接搬运配置值，因此这条保证来自 `validateTenantQuota` 的继承规则，而不是 `quota` 包自己）。
- **`degrade` 的额外前置条件（源码级细节）：** 只有 `OnExceed == ActionDegrade` **且** `DowngradeModel != "" || MaxTokensCap > 0` 时，`Admit` 才把 `degrade` 置真。配置校验里对应的一条错误是 `on_exceed is degrade but neither downgrade_model nor max_tokens_cap is set, so there is nothing to degrade to`。**这意味着一个只配了 `on_exceed: degrade` 的租户会被配置校验挡住，而一个绕过校验直接构造的 `quota.Policy{OnExceed: "degrade"}` 会退化成拒绝。**
- **后果：** 决定 `Admit` 在遇到第一个越界维度时是 `break`（拒绝）还是继续预留完整套维度（降级）。源码注释观点：**"一个降级策略在越界之后继续预留（降级请求仍然是请求，仍然要结算），而拒绝策略就此停下。"**

### DowngradeModel —— 降级用的模型

- **声明：** `DowngradeModel string`；配置侧 `DowngradeModel string \`json:"downgrade_model"\``，配置注释："**显式命名**而不是从价格本猜，因为便宜的模型仍然必须是调用方请求形状能用的那个。"
- **零值语义：** 空 = 不走模型降级；此时若 `MaxTokensCap > 0`，`degrade` 仍成立（只砍长度不换模型）。
- **后果：** 非空时会被写进 `Decision.DowngradeModel`，再由 `internal/gateway` 的配额头写成 `X-InferGate-Quota-Model`（只在 `ActionDegrade` 时下发），并在 `internal/server/quota.go` 的管理端 JSON 里以 `policy.downgrade_model` 出现。

### MaxTokensCap —— 降级时的补全上限

- **声明：** `MaxTokensCap int`；配置侧 `MaxTokensCap int \`json:"max_tokens_cap"\``，配置注释："**它是一个天花板，不是一个值**：本来就请求更少 token 的请求保留它自己的数字。"
- **单位：** token 数（补全侧），`int` 而非 `int64`，因为它最终要落进请求体的 `max_tokens` 字段。
- **零值语义：** ≤0 = 不设上限。配置校验里同样是"0 则继承 `default_policy`"。
- **后果：** 非零时写入 `Decision.MaxTokensCap`，再由网关写成 `X-InferGate-Quota-Max-Tokens`。**注意 `Admit` 从不校验这个值是否小于请求本身**——"天花板"语义由下游（`internal/gateway` 的降级应用逻辑）负责，不在 `quota` 包里。

### AnomalyRatio —— 异常倍数阈值

- **声明：** `AnomalyRatio float64`；配置侧有两层：章节级 `quota.anomaly_ratio`（`QuotaConfig.AnomalyRatio`，json `"anomaly_ratio"`）与租户级 `TenantQuotaConfig.AnomalyRatio \`json:"anomaly_ratio"\``，租户级"覆盖章节级的比值"（配置注释原文）。
- **单位：** **倍数**（今日 token ÷ 基线），不是百分比。默认是 **3**（`Defaults()` 里 `quota.AnomalyRatio = 3`）。
- **零值语义：** `<=0` = **关闭异常检测**。三处都以此为准：`checks` 不涉及它；`baseline` 首行 `if p.AnomalyRatio <= 0 { return 0, nil }`；`checkAnomaly` 首行 `if policy.AnomalyRatio <= 0 || m.store == nil { return }`。
- **配置校验的边界（值得记住）：** `<0` 报错、`==0` 继承、`>0 && <1` 报错（错误串是 `must be at least 1, got %v`），所以合法的显式值要么 ≥1，要么留空继承。**这意味着一份显式配 0 的租户配置校验不通过——"关闭异常检测"只能靠章节级或默认值 0 实现**，见「未能确认」。
- **后果：** 影响 `Report.Alerting`、`checkAnomaly` 的告警发射与 `Stats().Alerts` 计数。**它从不拒绝请求**——源码注释观点："这**只是告警**：这里没有任何东西会拒绝请求。一次花费尖峰通常是一个真实工作负载在做新事情，而一个因为流量看起来不寻常就开始拒绝的网关，会把客户的产品发布变成一次故障。"

### SessionTTL —— 会话计数器的存活时间

- **声明：** `SessionTTL time.Duration`；**配置侧没有对应字段**——`TenantQuotaConfig` 里没有任何 TTL 项，所以这个字段目前只能由代码构造（**推断**：它存在的目的是让测试或未来的配置项能缩短会话桶，而不必改 `DefaultSessionTTL` 常量）。
- **单位：** 时长。
- **零值语义：** `<=0` 时 `checks` 用 `DefaultSessionTTL`（24 小时）兜底。
- **后果：** 只影响**会话**维度的 TTL，也就是"一个会话的预算能记多久"。源码注释观点（在 `DefaultSessionTTL` 处）："它同时是一个被遗弃的会话保留其预算的**上界**，所以是一天而不是一小时：一次长跑 agent 不应该在运行中途被重置预算。"

### Budgeted() —— 唯一的"这个租户到底有没有被限"的答案

```
func (p Policy) Budgeted() bool {
	return p.TokensPerDay > 0 || p.CostPerDayMicros > 0 ||
		p.RequestsPerMinute > 0 || p.TokensPerSession > 0
}
```

源码注释观点（照译）："它是『这个租户到底有没有上界』的**唯一真源**：这里**故意没有**一个单独的 unbounded 标志，因为对同一个问题的第二个答案就是一个可能出错的答案。一个所有维度都是零的策略会**短路准入**——没有什么可预留的，于是它每个请求只花一次 map 查找、零次 Redis 往返。"

**注意它没有把 `AnomalyRatio` 算进去**：一个只配了 `anomaly_ratio` 的租户是 `Budgeted() == false`，`Admit` 会走 `ReasonWithinBudget` 快路径并**完全不碰 store**——于是 `checkAnomaly` 也永远不会被调用（它只在 `Settle` 末尾跑）。**结论：在一个无预算的租户上，异常检测在请求路径上是死代码；只有管理端的 `Report` 会读基线并显示 `Alerting`。** 这是一个从两个函数拼出来的推论，仓库里没有测试钉住它，见「未能确认」。

## 21. atomics 的 11 个计数器

**声明（照抄）：**

```
type atomics struct {
	allowed       atomic.Uint64
	degraded      atomic.Uint64
	rejected      atomic.Uint64
	storeErrors   atomic.Uint64
	alerts        atomic.Uint64
	overshoot     atomic.Uint64
	overshootCost atomic.Int64
	reservedTok   atomic.Uint64
	settledTok    atomic.Uint64
	releasedTok   atomic.Uint64
	releasedCost  atomic.Int64
}
```

**类型级注释（照译，源码注释观点）：** "`atomics` 保存每个请求都会碰到的计数器，这样**热路径永远不用拿 manager 的互斥锁**。"

**它嵌在 `Manager` 里当字段：** `stats atomics`。只有 `Manager.Stats()` 通过 `Load()` 读它，只有 `Admit`/`Settle`/`Release`/`releaseEntries`/`unavailable`/`checkAnomaly` 写它。**没有一个写点持有 `m.mu`**——`m.mu` 只保护 `anomalies` 那张表（第 28 节）。

逐个计数器：

### allowed —— 允许通过的请求数

- **类型：** `atomic.Uint64`。
- **自增点（共 4 处）：** `Admit` 里 `!cfg.Enabled` 的短路；`Admit` 里 `!policy.Budgeted()` 的短路；`Admit` 里没有任何维度越界时的收尾；`unavailable` 里 fail-open 的那一支。
- **不在这里统计的：** **降级请求不计入 `allowed`**（它走 `degraded`）。源码注释观点（在 `unavailable` 的 fail-open 分支）："一次 fail-open 的准入**仍然是一次准入**；把它漏在决策计数器之外，会让 `allowed+degraded+rejected` 少算网关实际服务的请求数，而这恰好发生在 store 挂掉、数字正被人盯着的时候。"
- **对外：** `Stats().Allowed`，再进管理端 JSON 的 `stats.allowed`。

### degraded —— 降级放行的请求数

- **类型：** `atomic.Uint64`。
- **自增点（唯一）：** `Admit` 的降级分支 `m.stats.degraded.Add(1)`，位置在判断 `degrade` 为真、填好 `Decision` 之后、`return` 之前。
- **前提：** 至少要有一个维度真的越界（否则走的是 `allowed`）。
- **对外：** `Stats().Degraded`；网关侧还会把 `X-InferGate-Quota: degrade` 下发给客户端。

### rejected —— 被拒绝的请求数

- **类型：** `atomic.Uint64`。
- **自增点（两处）：** `Admit` 的拒绝分支（先 `releaseEntries` 回滚、把 `res.settled` 置真，再自增）；`unavailable` 的 fail-closed 分支。
- **注意：** store 故障导致的拒绝**同时**自增 `rejected` 与 `storeErrors`，所以这两个数不是互斥分类——`rejected` 里含一部分"因为账本读不出来而拒"的请求。

### storeErrors —— 存储层出错的**事件**数

- **类型：** `atomic.Uint64`。
- **自增点（五处）：** `unavailable` 开头（每次故障转移都算一次）；`Settle` 里**每个**写失败的计数器条目各一次；`releaseEntries` 里**每个**归还失败的条目各一次；`checkAnomaly` 读今日计数失败一次；`checkAnomaly` 读基线失败一次。
- **单位是事件不是请求：** 一个两维度的租户在 `Settle` 时两个条目都失败，这个数加 2，而 `rejected` 不加（Settle 在响应之后）。这是本册从源码读出来的一个"计数器语义不齐"的点：`allowed`/`degraded`/`rejected` 是**每请求**，`storeErrors` 是**每事件**。
- **对外：** `Stats().StoreErrors`；管理端把它呈现为账户系统的健康信号。

### alerts —— 异常报警的**跃迁**次数

- **类型：** `atomic.Uint64`。
- **自增点（唯一）：** `checkAnomaly` 的 `case alerting && !was:` 分支，即**从正常进入报警的那一次**。
- **不统计的：** 报警持续期间不再加（被 `anomalyCheckInterval` 与 `was` 双重挡住）；从报警回到正常也不加（那一支只 `log.Info`）。
- **后果：** 它衡量的是"发生过几次异常开始"，不是"异常持续了多久"。

### overshoot —— 超出预留的 token 总量

- **类型：** `atomic.Uint64`。
- **自增点（唯一）：** `Settle` 中 `diff > 0` 且 `e.counter == CounterTokens` 时 `m.stats.overshoot.Add(uint64(diff))`。
- **对外：** `Stats().OvershootTokens`（见第 24 节）。

### overshootCost —— 超出预留的金额

- **类型：** `atomic.Int64`（**与 `overshoot` 一个无符号、一个有符号**，因为成本计数在结构上就是有符号的）。
- **自增点（唯一）：** `Settle` 中 `diff > 0` 且 `e.counter == CounterCost`。
- **对外：** `Stats().OvershootCostMicro`。

### reservedTok —— 准入时预扣的 token 总量

- **类型：** `atomic.Uint64`。
- **自增点（唯一）：** `Admit` 的预留循环里，**每个** `counter == CounterTokens` 的条目写一次（不是在循环外按请求写一次）。
- **源码注释观点（照译）：** "token 账本是**按预留条目**而不是按请求记的。一个租户可以同时被两个 token 维度计量（日和会话），在这里按请求记一次、结算时却逐条结算它的条目，会让 `reserved - released + overshoot == settled` 恰好对那些计量最难的租户不成立。条目被写下的同时就记一次，恒等式因此**逐条目**成立，对总和也就成立。"

### settledTok —— 结算时实际记账的 token 总量

- **类型：** `atomic.Uint64`。
- **自增点（唯一）：** `Settle` 里 `e.counter == CounterTokens` 且写成功之后，`m.stats.settledTok.Add(uint64(actual))`。
- **一处防下溢：** 在自增之前有 `if actual < 0 { actual = 0 }`，因为 `uint64` 转负会变成一个天文数字。
- **源码注释观点：** "settled 是**真正被记账**的量，所以对每一次预留都有 `reserved - released + overshoot == settled`，`settled` 不再是 `overshoot` 的第二个名字。"

### releasedTok —— 归还回去的 token 总量

- **类型：** `atomic.Uint64`。
- **自增点（两处）：** `Settle` 里 token 条目 `diff < 0` 时加 `-diff`；`releaseEntries` 里成功的 token 条目加 `e.reserved`。
- **严格的先后：** `releaseEntries` 的注释说"**只有在 store 真的把钱还回来之后才记账**：一个仍然被扣着的计数器绝不能被报成已归还"。所以自增发生在 `store.Add(…, -e.reserved, …)` 返回 `nil` 之后；失败则只加 `storeErrors`。

### releasedCost —— 归还回去的金额

- **类型：** `atomic.Int64`。
- **自增点（两处）：** `Settle` 里 cost 条目 `diff < 0` 时加 `-diff`；`releaseEntries` 里成功的 cost 条目加 `e.reserved`。
- **源码注释观点（照译，`Settle` 里那段）：** "审计计数器是**按维度**而不是只按正负拆开的：一个成本条目的贷记是微美元，而一次真实运行显示两者被加进了同一个『released tokens』数字（一个预留了 279 token 的请求显示 released 1014 token），这是一个**读起来像预算异常的单位错误**。token 记在 token 的地方，钱记在钱的地方，两者永不共用计数器。"

## 22. Reason 常量族 —— 9 个取值分别去哪里

**声明与字面值（照抄）：**

```
const (
	ReasonDisabled      = "disabled"
	ReasonNoStore       = "no-store"
	ReasonWithinBudget  = "within-budget"
	ReasonStoreError    = "store-error"
	ReasonAnomaly       = "anomaly"
	ReasonDailyTokens   = "tokens_per_day"
	ReasonDailyCost     = "cost_per_day_usd"
	ReasonSessionTokens = "tokens_per_session"
	ReasonMinuteRPM     = "requests_per_minute"
)
```

**类型级注释（照译，源码注释观点）：** "理由会出现在**响应头、日志和指标**里。它们按构造就是**低基数**的：维度名与几个固定串，**绝不是一个租户名**。"

**两个必须一起看的事实：** 前五条是"决策是怎么产生的"（不与具体维度绑定），后四条是"哪个维度越界了"（同时也是 `check.dimension` 与 `Policy` 字段的语义映射）。`internal/server/quota.go` 的 `quotaPolicy` 把 `Policy.OnExceed` 搬进 `Policy`，而理由串则由 `checks` 直接写入 `check.dimension`，两条线在 `Decision.Reason` 汇合。

| 常量 | 字面值 | 在哪里产生 | 去哪里 |
| --- | --- | --- | --- |
| `ReasonDisabled` | `disabled` | `Admit` 开头 `!cfg.Enabled` 的短路 | `Decision.Reason` → 管理端与 `internal/quota` 的测试（`TestDisabledManagerIsAllowingAndInert`）。**不会变成响应头**：网关在 `p.quota == nil || !p.quota.Enabled()` 时直接放行，根本不调用 `Admit`，因此 `X-InferGate-Quota` 与它的理由头都不会被设置 |
| `ReasonNoStore` | `no-store` | **没有任何地方** | **死常量**：全仓库只在声明处出现。`Enabled` 为真而 `store == nil` 时，`Admit` 走的是 `unavailable(errors.New("quota: no store configured"))`，理由因此是 `ReasonStoreError` 而不是它 |
| `ReasonWithinBudget` | `within-budget` | `Admit` 的无预算快路径与全维度通过路径 | `X-InferGate-Quota-Reason: within-budget`（`setQuotaHeaders` 在 `dec.Reason != ""` 时无条件下发），以及 `internal/gateway` 测试里对允许响应的断言 |
| `ReasonStoreError` | `store-error` | `unavailable` 的两个分支（fail-open 与 fail-closed） | fail-closed：HTTP 503 + 错误类型 `infergate_quota_unavailable`（`TypeQuotaStore`），理由头 `store-error`；fail-open：仍然 200，理由头 `store-error`，另加一条 Warn 日志；另外 `internal/gateway/quotapath.go` 里还有一处 `dec == nil` 的兜底构造，也用这个理由。`dimensionLabel` 把它翻译成 `accounting` |
| `ReasonAnomaly` | `anomaly` | **没有任何地方** | **死常量**：异常检测只写日志与 `Stats().Alerts`，从不构造 `Decision`（源码注释观点：这里"**只是告警**……没有任何东西会拒绝请求"）。全仓库只在声明处出现 |
| `ReasonDailyTokens` | `tokens_per_day` | `checks` 的日 token 维度 | `Decision.Reason`；拒绝时进 429 正文与 `X-InferGate-Quota-Reason`；`dimensionLabel` → `daily token` |
| `ReasonDailyCost` | `cost_per_day_usd` | `checks` 的日成本维度 | 同上；`dimensionLabel` → `daily spend`。**注意单位错位**：理由串说的是美元，而键与统计用的是 `cost_micros` |
| `ReasonSessionTokens` | `tokens_per_session` | `checks` 的会话维度（且仅当 session 非空） | 同上；`dimensionLabel` → `per-session token` |
| `ReasonMinuteRPM` | `requests_per_minute` | `checks` 的每分钟维度 | 同上；`dimensionLabel` → `per-minute request` |

**`dimensionLabel` 的兜底（照抄）：** `default: return reason` —— 一个不在表里的理由会**原样**进入人读的 429 正文。因为只有四个维度可能成为 `Reason`，这条兜底在今天的代码里不可达；它把"未知理由"从崩溃降级成可读字符串。

**低基数为什么重要：** 这 9 个串都**不含租户名、会话 id 或时间戳**。租户只会出现在 429 正文的 `quotaMessage`（`strconv.Quote(tenant)`）与日志字段里，不会进指标标签。

## 23. RetryAfter —— 只在拒绝路径上有值

**声明（`Decision` 的字段，照抄）：**

```
	// RetryAfter is how long until the breached window resets. It is set only
	// on a rejection.
	RetryAfter time.Duration
```

**单位：** 时长；**零值：** "不告诉你"（而不是"立刻重试"）。

**它从哪来：** `check.retryAfter`，由 `checks` 按维度分别算出来：

| 维度 | `retryAfter` 的算法 | 含义 |
| --- | --- | --- |
| 日 token | `untilDayEnd(now)` | 到"次日 UTC 零点再往后一小时的宽限"为止 |
| 日成本 | `untilDayEnd(now)` | 同上（与 token 维度共用同一次调用算出的 `dayTTL`） |
| 每分钟请求 | `time.Minute - time.Duration(now.Second())*time.Second` | 到**本分钟**结束为止。**注意它只用秒字段**，所以亚秒部分被丢掉了，误差最大接近一秒 |
| 会话 token | `p.SessionTTL`，`<=0` 时用 `DefaultSessionTTL` | 一整个会话桶的存活时间——对客户端来说是"这个会话的预算什么时候清空"，而不是"下一条消息什么时候能过" |

**它只在哪儿被填进 `Decision`：** 拒绝分支里的 `RetryAfter: breach.retryAfter`。**降级分支不带它**（降级是放行，客户端没有被要求等待），`unavailable` 的两支也都不带它。

**网关怎么用它（`internal/gateway/quotapath.go`）：**

```
seconds := 1
if dec.RetryAfter > 0 {
	seconds = int(math.Ceil(dec.RetryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
}
w.Header().Set("Retry-After", strconv.Itoa(seconds))
```

**源码注释观点（照译）：** "永远给一个退避提示，**即使决策没有带窗口**：一个没有 `Retry-After` 的 429 会让每个 SDK 自己发明一个，而发明出来的通常是『立刻重试』。"

三个由此而来的性质：

- **`Retry-After` 永远不会是 `0`**：下限被硬编码成 1 秒。源码注释与测试都点了名：一个 `0` 等于告诉客户端马上重试，会把一次限额拒绝变成一次自激重试风暴。用一个 200 毫秒的会话窗口去测，得到的仍是 `"1"`。
- **向上取整**：90 秒的窗口原样变成 `"90"`（测试把"决策的窗口必须原样到达客户端"钉住了），而 0.2 秒会变成 1。
- **429 正文里的等待时间与头**不是同一个数：`quotaMessage` 用的是 `dec.RetryAfter.Round(time.Second).String()`（四舍五入的时长字符串，例如 `"1m30s"`），而头是向上取整的秒数。两个数字在边界上可能差一秒。

**契约侧的意义：** `Retry-After` 是这次拒绝里**唯一**告诉客户端"多久之后再试"的信息；`X-InferGate-Quota-Limit`/`-Used` 告诉它差多少，但不告诉它等到什么时候。

## 24. Overshoot —— 估计之外的超支

**两个字段（`Stats` 里的声明，照抄）：**

```
	// OvershootTokens counts tokens settled above the reserved estimate. It is
	// the honest measure of how much the limit is exceeded by, and it is the
	// number that tells an operator to raise the completion estimate.
	OvershootTokens    uint64
	OvershootCostMicro int64
```

**类型注释（照译，源码注释观点）：** "`OvershootTokens` 数的是**结算时超过预留估计**的 token。它是『限额被超出了多少』的诚实度量，也是**告诉运维该去调高补全估计**的那个数字。"

**来源与数值：** 预留用的是 `Estimate`（网关侧的默认补全预留是 256 token，字符/token 默认 4，两者都在 `internal/gateway/quotapath.go` 里以 `defaultQuotaCompletionTokens` 与 `defaultQuotaCharsPerToken` 命名）；结算用的是供应商真的报回来的 `Usage`。`Settle` 逐条目算 `diff := actual - e.reserved`，只有 `diff > 0` 才记账，并且**按计数器的维度分流**：token 进 `overshoot`，成本进 `overshootCost`，请求数**从不进**（`actualFor` 对 `CounterRequests` 返回 `max64(u.Requests, 1)`，与预留时同一条公式，所以差值恒为 0 或负数）。

**后果（源码注释观点，照译）：** "请求**长过了它的估计**。计数器现在坐在限额之上，于是这个租户的**下一个**请求才是被拒的那个——**这是错误应该偏向的方向**。"

**与账面恒等式的关系：** overshoot 是恒等式里唯一"补正"的项。一次结算把预留调整为实际，超出的部分没有被退回去（它真的被花了），所以 `reserved - released + overshoot == settled` 在**逐预留条目**的层面成立。这条恒等式写在 `Stats` 的字段注释里，`TestSettleKeepsTokensAndMoneyInDifferentCounters` 是钉住它的测试。

**一处签名不齐：** token 侧计数是无符号、成本侧是有符号（`OvershootCostMicro int64`），与第 21 节 `overshoot`/`overshootCost` 的两个原子类型一一对应。对 token 来说 `diff > 0` 保证了转换安全；成本侧保留符号是为了让"退还多于超支"在数据结构上可表达（虽然今天没有代码路径会产生负的 overshoot）。

## 25. ReservedTokens / SettledTokens / ReleasedTokens / ReleasedCostMicro

**四个字段一起看（`Stats` 里的声明，照抄）：**

```
	// ReservedTokens is what admission pre-charged, SettledTokens what the
	// provider actually reported, ReleasedTokens the part of the reservation
	// that came back. They are three views of one ledger rather than three
	// independent counters, so reserved - released + overshoot == settled.
	//
	// All three are counted per reserved ENTRY, so a tenant metered on both a
	// daily and a per-session token budget contributes its tokens twice - once
	// per budget it consumed. The identity is what makes that readable: the
	// numbers describe budget consumed, not requests admitted.
	ReservedTokens uint64
	SettledTokens  uint64
	ReleasedTokens uint64
	// ReleasedCostMicro is the money side of ReleasedTokens. It is kept
	// separate for the same reason the two are separate in the store: summing
	// micro-dollars into a token count produced a "released tokens" figure that
	// had nothing to do with tokens.
	ReleasedCostMicro int64
```

**逐字段语义：**

| 字段 | 单位 | 谁在什么时候加 | 加多少 |
| --- | --- | --- | --- |
| `ReservedTokens` | token | `Admit` 的预留循环，**每个** token 维度条目，写成功后立刻 | `c.delta`（即该维度的 `est.Tokens()`） |
| `SettledTokens` | token | `Settle`，**每个** token 条目写成功后 | `actualFor(CounterTokens, usage)`，负值被夹到 0 |
| `ReleasedTokens` | token | `Settle`（`diff < 0` 时加 `-diff`）与 `releaseEntries`（成功的 token 条目加 `e.reserved`） | 未被花掉/被退回的 token 数 |
| `ReleasedCostMicro` | 微美元 | `Settle`（cost 条目 `diff < 0` 时加 `-diff`）与 `releaseEntries`（成功的 cost 条目加 `e.reserved`） | 被退回的钱 |

**三条必须记住的性质：**

1. **它们全是单调递增的累计量，不是余额。** `ReservedTokens` 在预留被退还之后**不会减少**。所以"当前有多少在途预留"在这套计数器里**读不出来**——要读余额只能去 store 里 `Get` 对应的键（`Report` 就是这么做的）。
2. **`released` 的贷记严格晚于 store 的归还**，见第 21 节 `releasedTok` 那条；这是"计数器可以被信任"与"计数器只是意图的记录"之间的选择，源码明确选了前者。
3. **恒等式是逐条目成立的，而且是三个不同时刻的快照。** `reserved` 在准入时加、`settled`/`released` 在结算时加，所以任何**在读的那一刻**做 `reserved - released + overshoot == settled` 的校验，都会被"正在途中的预留"打破。恒等式说的是**最终**会计关系，不是一个可以随时断言的不变式。源码注释没有点明这一点；这是**推断**。

**`ReleasedCostMicro` 为什么单独存在：** 照译源码注释观点——"与 store 里两者分开的理由相同：**把微美元加进一个 token 计数**产生了一个与 token 毫无关系的『released tokens』数字。" 具体数字是那次真实运行里的 1014 与 279。这条注释解释了第 21 节里"按维度分流"的两处 `switch e.counter` 为什么必须存在。

**管理端映射：** 四个字段都会出现在 `internal/server/quota.go` 的统计 JSON 里（键名形如 `reserved_tokens`、`settled_tokens`、`released_tokens`、`released_cost_micros`），这是它们唯一的消费者；请求路径不读 `Stats()`。

## 26. WindowDay / WindowMinute / WindowSession

**声明（照抄）：**

```
const (
	WindowDay     = "day"
	WindowMinute  = "minute"
	WindowSession = "session"
)
```

**类型级注释（照译，源码注释观点）：** "窗口名。它们**是计数器键的一部分，不是一个时长**。"

**三个取值分别进哪里：**

- **`WindowDay`** —— 进 `key(tenant, WindowDay, dayBucket(now), …)`。被两条日维度使用（token 与成本），桶是 `YYYYMMDD` 字符串，TTL 是 `untilDayEnd`。
- **`WindowMinute`** —— 进分钟维度，桶是 `YYYYMMDDHHmm`，TTL 是 `minuteTTL`（2 分钟，见第 29 节）。
- **`WindowSession`** —— 进会话维度，**桶是会话 id 本身**（经 `sanitise`），TTL 是 `SessionTTL` 或 `DefaultSessionTTL`。它与其他两个不同：桶不是时间，所以 `Report` 里的 `Session` 字段回显的是调用方传进来的那个 id，而不是一个时刻。

**为什么"窗口进键而不是进 TTL"（源码注释观点，照译 `internal/quota/redis.go` 的包内注释）：**

- "每个计数器是一个**由 INCRBY 修改的 STRING**，绝不是哈希字段或列表元素，因为字符串上的 INCRBY 是原子的，而这就是**整个并发故事**。"
- "窗口是**键的一部分**，不是固定键上的 TTL。于是两个窗口永远不会共用一个计数器，23:59:59 的请求与 00:00:01 的请求不可能竞争彼此的翻转，而读『今天』是一次**不带算术的平凡 GET**。TTL 只是垃圾回收。"

**一个直接后果：** `WindowSession` 的键在会话存活期间是同一个，所以"翻窗"这个动作在会话维度上**不存在**——会话预算要么被结算消耗，要么等 TTL 过期。这也是为什么 `RetryAfter` 在会话维度上是整个 TTL（第 23 节）。

**没有的东西：** 没有 `WindowHour`、没有 `WindowWeek`、没有滑动窗口。分钟桶是固定的整分钟边界。

## 27. CounterTokens / CounterCost / CounterRequests

**声明（照抄）：**

```
const (
	CounterTokens   = "tokens"
	CounterCost     = "cost_micros"
	CounterRequests = "requests"
)
```

**类型级注释（照译，源码注释观点）：** "计数器名，同样是键的一部分。成本以**微美元**保存成一个整数：一个浮点计数器**既不能在 Redis 里被原子自增**，又会在几百万次 INCRBY 上累积舍入误差。"

**三者与策略维度的映射（哪个 `Policy` 字段生成哪个计数器）：**

| `Policy` 字段 | `window` | `counter` | 键尾 | `check.dimension` |
| --- | --- | --- | --- | --- |
| `TokensPerDay` | `WindowDay` | `CounterTokens` | `tokens` | `ReasonDailyTokens` |
| `CostPerDayMicros` | `WindowDay` | `CounterCost` | `cost_micros` | `ReasonDailyCost` |
| `RequestsPerMinute` | `WindowMinute` | `CounterRequests` | `requests` | `ReasonMinuteRPM` |
| `TokensPerSession` | `WindowSession` | `CounterTokens` | `tokens` | `ReasonSessionTokens` |

**注意最后两行：** 只有四个维度，但有**三**个计数器——`TokensPerDay` 与 `TokensPerSession` 共用 `CounterTokens`，靠 `window` 区分。

**`actualFor` 的映射（照抄）：**

```
func actualFor(counter string, u Usage) int64 {
	switch counter {
	case CounterTokens:
		return u.Tokens()
	case CounterCost:
		return u.CostMicros
	case CounterRequests:
		// The request happened; settlement never changes the count.
		return max64(u.Requests, 1)
	}
	return 0
}
```

源码注释观点（照译）："请求发生了；结算**从不**改变这个计数。" 这就是为什么请求维度的 `diff` 永不为正（预留时用 `max64(est.Requests, 1)`，结算时用同一条公式），也解释了为什么 429 之后的分钟桶不会因为一次失败的结算而被多加一次。**兜底 `return 0`** 意味着一个未知计数器名会在结算时把预留**全部**退还（`diff = 0 - reserved < 0`），而不是留在账上。

**三条使用上的性质：**

1. **`CounterCost` 是唯一允许浮点报价进来的路径，但它本身永远是整数**：配置写美元浮点 → `quotaPolicy` 用 `math.Round(×1e6)` 换算 → 存进去的是整数微美元。
2. **`CounterRequests` 是唯一不参与超支记账的计数器**，也是唯一用 `minuteTTL` 的。
3. **键里的计数器名与响应头/理由里的名字不同**：键里是 `cost_micros`，理由是 `cost_per_day_usd`；键里是 `tokens`，理由是 `tokens_per_day` 或 `tokens_per_session`。所以**一次 `grep` 找 `cost_per_day_usd` 在 Redis 里是找不到任何键的**。

## 28. checkAnomaly —— 只告警，不拒绝

**签名：** `func (m *Manager) checkAnomaly(ctx context.Context, tenant string, u Usage)`

**唯一调用点：** `Settle` 的最后一行（在逐条目结算之后）。因此它**只在结算路径上**跑，`Admit` 从不调用它。

**参数 `u` 没有被使用。** 函数体从头到尾只读 `m.Policy(tenant)`、`m.now()`、store 里的今日计数与基线；**没有任何一处引用 `u`**。签名暗示它想把"这一次请求的用量"算进去，实际做法是去 store 读那个**已经包含**本次结算的今日总数（`Settle` 刚刚把增量写进去了）。这是一个真实的、可编译的未使用参数（Go 不禁止未使用的形参），见「未能确认」。

**首行的两道闸（照抄）：**

```
	policy := m.Policy(tenant)
	if policy.AnomalyRatio <= 0 || m.store == nil {
		return
	}
```

也就是说：**关掉阈值或没有 store，整个异常检测不存在**，一次 store 往返都不会发生。

**节流：** `anomalyCheckInterval`（1 分钟）与"日变了就重算"两条一起生效：

```
	m.mu.Lock()
	st := m.anomalies[tenant]
	if st == nil {
		st = &anomalyState{}
		m.anomalies[tenant] = st
	}
	if st.day == day && now.Sub(st.checked) < anomalyCheckInterval {
		m.mu.Unlock()
		return
	}
	st.day = day
	st.checked = now
	m.mu.Unlock()
```

**源码注释观点（照译）：** "`anomalyState` 记住一个租户上一次的异常评估，好让基线读取**每个租户每分钟最多发生一次**，也好让告警在**跃迁**时而不是每个请求上触发。"

**关键：两次 store 读发生在锁外**（`m.mu` 在上面那段就放开了）。随后：

```
	today, err := m.store.Get(ctx, m.key(tenant, WindowDay, day, CounterTokens))
	… 失败 → storeErrors++ / warn("quota: anomaly read failed") / return
	base, err := m.baseline(ctx, tenant, now, policy)
	… 失败 → storeErrors++ / warn("quota: anomaly baseline failed") / return
	ratio := 0.0
	if base > 0 { ratio = float64(today) / base }
	alerting := base > 0 && ratio >= policy.AnomalyRatio
```

**三个由此而来的行为：**

- **基线为 0 时永不报警**（`base > 0 &&`）。新租户第一天不会因为"从 0 涨到很大"被判定为异常。
- **比较是 `>=`**，所以恰好等于阈值就报警。
- **阈值读的是当前策略**，而 `Policy` 是每次调用现查的（`m.Policy(tenant)`），所以运行期改配置（如果将来有热更新）会立刻影响下一次评估。

**跃迁、日志与 `Stats().Alerts`（照抄分支结构）：**

```
	m.mu.Lock()
	was := st.alerting
	st.baseline, st.ratio, st.alerting = base, ratio, alerting
	m.mu.Unlock()

	switch {
	case alerting && !was:
		m.stats.alerts.Add(1)
		if m.log != nil {
			m.log.Warn("quota: spend anomaly", "tenant", …, "tokens_today", …, "baseline_tokens", …, "ratio", …, "threshold", …)
		}
	case !alerting && was:
		if m.log != nil {
			m.log.Info("quota: spend back to normal", "tenant", …, "tokens_today", …, "baseline_tokens", …, "ratio", …)
		}
	}
```

- 进入报警：`alerts++` + 一条 **Warn**，字段是 `tenant`、`tokens_today`、`baseline_tokens`、`ratio`、`threshold`。
- 退出报警：**只**一条 **Info**，字段少一个 `threshold`，**不动 `alerts`**。
- 报警持续中：什么都不做（`st.alerting` 仍是真，下一次跃迁检测靠它）。

**`anomalyState` 的五个字段（照抄与逐个说明）：**

```
type anomalyState struct {
	day      string
	baseline float64
	ratio    float64
	alerting bool
	checked  time.Time
}
```

- `day` —— 当前评估对应的日桶（`YYYYMMDD`）；日一变就强制重算，与 `checked` 一起构成节流条件。
- `checked` —— 上一次**通过**节流的那次评估的时刻。
- `alerting` —— 上一次的判定结果，跃迁检测靠它；也是唯一被**读回**的字段。
- `baseline` 与 `ratio` —— **写进去但没有任何地方读**。全仓库只有 `checkAnomaly` 的赋值那一行引用它们（`Report` 用的是自己现算的 `baseline()` 与 `Ratio`，见下）。它们是留给未来观测/调试的字段，今天等于死存储。

**`baseline` 辅助函数：**

```
func (m *Manager) baseline(ctx context.Context, tenant string, now time.Time, p Policy) (float64, error)
```

- `p.AnomalyRatio <= 0` → 立刻返回 `(0, nil)`，**一次 store 读都不做**。
- 否则遍历 `i := 1; i <= anomalyBaselineDays; i++`（`anomalyBaselineDays = 3`），读 `<prefix>:<tenant>:day:<前 i 天>:tokens`；**只有 `v > 0` 的天数才计入 `sum` 与 `days`**。
- 源码注释观点（照译）："**没有流量的日子被跳过，而不是当作零计入**：一个上周二很安静的租户并没有『平均』掉自己一半的日常花费，把零平均进去会让第一个繁忙的周一看起来像一次异常。"
- `days == 0`（三天都静默）→ 返回 `(0, nil)`，于是调用方不会报警。

**测试：** `TestAnomalyAlertsWithoutBlocking` 是钉住"报警但不禁行"的用例。

**与 `Report` 的关系（一个容易误判的点）：** `Report` **不复用** `anomalyState`，它自己调 `m.baseline(...)` 并现算：

```
	rep.BaselineTokens = baseline
	if baseline > 0 {
		rep.Ratio = float64(rep.TokensToday) / baseline
		rep.Alerting = policy.AnomalyRatio > 0 && rep.Ratio >= policy.AnomalyRatio
	}
```

两处的判断条件**一致**（同样 `baseline > 0`、同样 `>=`），但（a）`Report.Alerting` 用的是**读那一刻**的今日值，而 `checkAnomaly` 用的是它自己节流窗口里的值；（b）`Report` 不写 `anomalyState`、也不受节流限制。**所以管理端页面上"正在报警"可以在 `Stats().Alerts` 还是 0 的时候就为真**（例如一个从不结算的租户，或者一个刚重启、异常状态还是空的管理进程）。

## 29. minuteTTL / dayBucket / minuteBucket（以及它们的邻居）

### minuteTTL

**声明：** `const minuteTTL = 2 * time.Minute`（**未导出**）。

**源码注释观点（照译）：** "`minuteTTL` 是一个分钟桶的寿命。它**必须比桶活得更久**：一个在 10:00:59 创建的键一直到 10:01:00 都还是有意义的。"

**唯一使用点：** `checks` 里分钟维度那条 `check` 的 `ttl`。**后果：** 一个在 10:00:59 预留、在 10:01:02 才结算的请求，其增量仍然落在 `…:minute:202610051000:requests` 上，而不是被当成一个新键。如果没有这两分钟，迟到的结算会**创建一个已过期的窗口键**，把上一次的计数凭空重置。`TestKeyLayoutAndBucketFormats` 覆盖了键与桶的格式。

### dayBucket

**声明：** `func dayBucket(t time.Time) string { return t.Format("20060102") }`

**输入：** 一个 `time.Time`；**输出：** 8 位十进制日期，例如 `20261005`。

**它自己不转换时区**——`Format` 用的是 `t` 携带的 location。所有调用点都先把时间转成 UTC 再传进来（`Admit`、`Report`、`checkAnomaly` 都写了 `now := m.now().UTC()`，`baseline` 用的是同一个 `now` 加 `AddDate(0, 0, -i)`），所以实际使用的都是 UTC 日。**这是一种调用方纪律，不是 `dayBucket` 保证的性质**：任何未来新增的调用点只要漏掉 `.UTC()`，日窗口就会按本地日期切，而这个错误**不会**在类型上、也不会在测试上被抓住（注入的测试时钟返回 UTC 时也一样看不出来）。对照 `untilDayEnd`，那里是**函数内部**转的 UTC——同一份代码里两种做法并存。

**使用点：** `checks`（算 `day`）、`Report`（`rep.Day`）、`baseline`（前三天）、`checkAnomaly`（今日）。

### minuteBucket

**声明：** `func minuteBucket(t time.Time) string { return t.Format("200601021504") }`

**输出：** 12 位 `YYYYMMDDHHmm`，同样不转时区。

**使用点只有两处：** `checks` 里算分钟桶、`Report` 里的 `rep.Minute`。**两者必须用同一个时钟**——`Report` 用 `m.now().UTC()`，`Admit` 也用 `m.now().UTC()`，所以管理端读到的正是准入写的那个桶。

**一个边界性质：** 桶是**分钟粒度**，所以 `TTL` 是一条分钟桶从创建到过期的时长，而分钟桶每分钟换一个名字（`…1000`、`…1001`），`minuteTTL` 的 2 分钟只影响旧键何时被回收，不影响计数正确性。

### 同族的其余三个（本册一并钉住，因为第 20 节的字段依赖它们）

**`untilDayEnd`：**

```
func untilDayEnd(t time.Time) time.Duration {
	utc := t.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	d := next.Sub(utc)
	if d < time.Minute {
		return time.Minute
	}
	return d + time.Hour
}
```

- **源码注释观点（照译）：** "它**先**把调用方的时刻转成 UTC。从一个本地时间上读日历日期，会为任何传进来带时区的时间用错误的那一天构造边界，这是一次预算窗口里**静默的差一天**。"
- **最小值 1 分钟**：正好在午夜前调用时不会返回 0（0 会被 store 当成"永不过期"，见 `MemoryStore.Add` 里 `if ttl > 0` 的判断——那会让日桶**永远活着**，而这个键名又永远不会被再次使用，等于一次永久泄漏）。
- **`+ time.Hour` 的宽限**：注释说这是给"时钟落后的副本"的——它写的桶要能被读到那一天结束之后的一小时。**副作用**是每个日桶键在被回收前多活一小时，与 `minuteTTL` 的两倍冗余是同一类选择。

**`DefaultSessionTTL`：** `const DefaultSessionTTL = 24 * time.Hour`（**已导出**，而 `minuteTTL` 未导出）。源码注释观点：它"同时是一个被遗弃的会话保留其预算的**上界**，所以是一天而不是一小时：一次长跑 agent 不应该在运行中途被重置预算。" `Policy.SessionTTL <= 0` 时由 `checks` 用它兜底。

**`sanitise` 与 `key`（第 20 节的 `Tenant` 字段依赖它们）：**

```
func sanitise(s string) string
func (m *Manager) key(tenant, window, bucket, counter string) string
```

- `key` 把 `[]string{m.cfg.Prefix, sanitise(tenant), window, sanitise(bucket), counter}` 用 `":"` 连接。`Prefix` 为空时 `New` 会补成 `"ig:quota"`。
- `sanitise` 的规则：`a-z`/`A-Z`/`0-9`/`-`/`.` 原样保留；`_` 写成 `__`；**其他一切**写成 `_x` 加**恰好 6 位十六进制**；空串变成 `"_"`。
- **为什么必须单射（源码注释观点，照译）：** "这个编码是**单射**的，而这一点在这里正是关键：租户名来自请求头，也就是调用方控制它，而一个多对一的映射（`acme:inc` 与 `acme_inc` 塌成同一个键）会让一个精心构造的名字去扣**另一个租户**的预算。所以一个配在古怪名字下的租户就是它自己的租户，而不是别人的第二个名字。" 固定 6 位宽度的理由也在这里："不然 `U+10FFF` 后跟 `ff` 与 `U+10FFFF` 后跟 `f` 都会编码成 `_x10fffff`。"
- **它把会话桶也一起洗了**（`key` 对 `bucket` 也调 `sanitise`），所以会话 id 里的 `:`、控制字符、Unicode 都不能改变键的层级。**注意 `counter` 与 `window` 不洗**——它们是包内常量。

## 30. 未能确认

本册以下各点**没有**在源码或测试里找到确证，或与任务描述对不上，一律列出而不猜：

1. **字段数量对不上。** 任务描述说 `quota.Policy` 有 13 个导出字段、`atomics` 有 7 个计数器；源码里 `Policy` 是 **10** 个导出字段（`Tenant`、`TokensPerDay`、`CostPerDayMicros`、`RequestsPerMinute`、`TokensPerSession`、`OnExceed`、`DowngradeModel`、`MaxTokensCap`、`AnomalyRatio`、`SessionTTL`），`atomics` 是 **11** 个（`allowed`、`degraded`、`rejected`、`storeErrors`、`alerts`、`overshoot`、`overshootCost`、`reservedTok`、`settledTok`、`releasedTok`、`releasedCost`）。本册按源码全部覆盖，但**没能确认 13 与 7 是怎么数出来的**（是否把 `Config` 字段或 `Stats` 字段混入）。
2. **`Policy` 的 json 配置键。** 任务描述要求写 `quota.Policy` 的 json 键，但 `Policy` 结构体**没有任何 json 标签**。本册给的是配置层 `TenantQuotaConfig` 的标签（已验证）。若原意是别的结构，请指出。
3. **`ReasonNoStore` 与 `ReasonAnomaly` 的意图。** 两者在整个仓库里只出现在声明处，没有任何读取点。无法确认它们是**为未来保留**、还是**重构后的残留**。
4. **`HMSET` 的错误串。** `cmdHSet` 硬编码 `arity("HSET")`，所以 `HMSET` 的参数错误报 `'hset'`。无法确认这是有意共用实现，还是疏漏。
5. **`cmdSet` 的 `NX` + `XX` 同时给出。** 两个标志都为真时任何键都写不进去却回 nil；真实 Redis 会报语法错误。仓库里没有测试覆盖这个组合，无法确认是否为已知取舍。
6. **`cmdSet` 的 `KEEPTTL` 被忽略**是有注释支撑的（源码注释观点明确说"接受并忽略"），但**注释没有说明这是永久决定还是临时妥协**。
7. **删空不删键**（`cmdHDel`、`cmdZRem`）与**错误路径留下空集合**（`cmdZAdd` 解析分数失败）这三处状态泄漏都没有测试钉住；无法确认是有意为之。
8. **`ZREMRANGEBYSCORE` 的 `(` 前缀被吃掉。** `parseScoreBound` 剥掉 `(` 却不记录开闭，而过滤条件两端都是闭区间。无法确认这是实现疏忽还是"调用方不用开区间"的取舍；本册倾向后者，但没有证据。
9. **`cmdExists`/`cmdType`/`cmdKeys` 看得见未清扫死键。** 这是一处与真实 Redis 的可观察差异，差值窗口最长是一次 `sweepLoop`（1 秒）。无法确认是否有意接受。
10. **`CostPerDayUSD` 四舍五入到 0 微美元。** `math.Round(usd * 1_000_000)` 对极小金额会得 0，于是该维度变成"不限额"而不是"限额为零"。无法确认是否有意（第 20 节按零值语义描述）。
11. **`AnomalyRatio` 显式写 0 被配置校验拒绝。** 校验规则是 `<0` 报错、`==0` 继承、`>0 && <1` 报错，所以**显式写 0 无法表达"关闭异常检测"**（只能靠留空继承到 0）。无法确认这是有意（避免把 0 写进配置）还是校验顺序的副作用。
12. **`anomalyState.baseline` 与 `anomalyState.ratio` 只写不读。** 无法确认是为未来观测预留，还是重构后遗留。
13. **`checkAnomaly` 的 `u Usage` 参数未被使用。** 无法确认是刻意（改读 store 里的今日值更准）还是签名的历史残留。
14. **`Budgeted()` 为假时异常检测在请求路径不可达**（第 20 节末尾的推理）：`Admit` 走快路径不碰 store，`checkAnomaly` 因此不会跑。仓库里没有测试钉住这个组合，无法确认是否被意识到。
15. **`checkAnomaly` 的两次 store 读在锁外**，因此两个并发结算可能同时通过节流检查并对同一次跃迁各加一次 `alerts`。没有测试覆盖并发路径，无法确认是否可复现。
16. **管理端 JSON 的具体键名**（第 25 节括号里那组 `reserved_tokens` 之类）来自本册撰写时对 `internal/server/quota.go` 的一次静态阅读，**本册没有在最终定稿前再复核一遍**。
17. **本册没有执行任何构建、测试或仓库脚本**（按任务要求）。所有关于"哪个测试覆盖了什么"的说法都来自 `internal/quota/quota_test.go` 与 `internal/gateway/quota_test.go` 的函数名，而不是执行结果。
18. **本册没有逐条展开的 `internal/quota` 符号：** `check` 与 `entry` 两个结构体只在第 19 节列出、在第 21/25 节间接解释；`Estimate`/`Usage` 的 `Tokens()` 方法、`New`、`Enabled`、`Policy`、`Close`、`warn`、`max64` 只在用到时被提及，没有各自成节。如果第 12 册之后还有第 13 册，这些是它的候选。

# 链上行情预言机聚合组件（本地模拟版）

一个纯本地、可运行、可复现的多来源行情预言机聚合器：数据源与存储全部在
本地模拟，不依赖任何真实链上喂价环境。它演示并保证以下性质：

- 多来源报价按配置的身份与有效期提交，非法提交被明确拒绝并给出可区分原因；
- 同一轮次并发提交、轮次切换确定，不重复计入、不漏计、不跳轮；
- 来源不足、报价陈旧、来源间偏差过大时按明确策略**回退**或**拒绝发布**；
- 即使多数/全部来源串通，结果也无法越过相对上一有效价格的硬区间；
- 规则与来源配置按版本演进，只约束新轮次，不追溯改写历史；
- 所有状态变更先写本地事件日志（每条 `fsync`），进程重启后已确认轮次不丢
  失、不重复发布，未完成轮次按策略一致恢复或作废；
- 每个轮次产出一份包含全部输入报价与逐条判定依据的 `RoundReport`，历史
  结果可通过重放事件日志逐轮复现。

## 目录结构

| 路径 | 说明 |
| --- | --- |
| `internal/oracle/types.go` | 领域类型：报价、提交、配置、轮次报告、拒绝原因 |
| `internal/oracle/config.go` | 版本化配置、默认值与校验 |
| `internal/oracle/auth.go` | HMAC-SHA256 签名/验签与提交格式校验 |
| `internal/oracle/aggregate.go` | 确定性聚合纯函数（去极值、偏差、硬护栏） |
| `internal/oracle/engine.go` | 并发安全引擎：鉴权、轮次、降级、发布 |
| `internal/oracle/recovery.go` | 事件日志重放与崩溃恢复（resume/void） |
| `internal/oracle/storage.go` | 事件日志接口、JSONL 文件存储（fsync、连续编号、完整性校验）、内存存储 |
| `internal/oracle/verify.go` | 自查：日志完整性校验与逐轮重算比对 |
| `internal/sim/source.go` | 本地模拟数据源（用配置密钥对报价签名） |
| `cmd/oracledemo/main.go` | 离线脚本化演示（正常/串通/升级/降级/重启） |
| `cmd/oracleverify/main.go` | 自查命令行入口（退出码 0 = 全部一致） |
| `configs/config.example.json` | 配置示例 |

## 数据源授权规则

1. 每个来源在某一版本配置中有 `id`、`active` 标志与对称密钥 `key`。
   可选的 `not_before` / `not_after`（RFC3339 时间）进一步限定授权时间窗。
2. 报价字段为 `(source_id, asset, round, price_micros, quote_time, nonce)`，
   来源对下列规范串做 HMAC-SHA256，签名以十六进制提交：

   ```
   v1|source_id|asset|round|price_micros|quote_time_unix_nano|nonce
   ```

   `price` 使用整数微单位（`1e-6`），全程不使用浮点数。
3. 引擎对每次提交按顺序检查，失败即拒绝并返回不同的 `reason`：

   | reason | 触发条件 |
   | --- | --- |
   | `bad_format` | 来源/资产 ID 非法、轮次为 0、价格非正、缺时间戳/nonce、签名长度不对 |
   | `round_closed` | 当前无打开轮次，或提交轮次不是当前轮次 |
   | `unauthorized_source` | 来源不存在或在本轮次绑定的配置版本中已停用 |
   | `source_auth_window` | 来源存在且已启用，但提交时刻落在其授权时间窗之外 |
   | `asset_not_configured` | 资产不在当前配置中 |
   | `bad_signature` | 用配置密钥验签失败（含冒用他人身份） |
   | `submission_expired` | 报价时间早于 `now - submission_ttl`，或超前超过 `clock_skew` |
   | `duplicate_submission` | 同一提交内容重放、nonce 已使用、或同来源同资产同轮重复报价 |

4. 授权身份以**轮次打开时的配置版本**为准；提交在该轮次内始终按该版本鉴权。

### 授权时间窗

- 窗口语义固定为 **`[not_before, not_after)`：起点含、终点不含**。提交时刻
  恰等于 `not_before` 被接受，恰等于 `not_after` 被拒绝。该约定全局唯一，
  不随配置变化。
- 窗口按**引擎时钟在提交时刻**判定，判定发生在引擎互斥锁内：窗口边缘上的
  并发提交被串行化，同一时刻的提交要么全部在窗内、要么全部在窗外，不会
  出现有的算有的不算。
- 两个端点都可省略：只写一端表示该侧无界；都不写表示**长期有效**（与
  引入窗口前的行为完全一致，旧配置无需修改）。
- 窗口外提交的拒绝原因是 `source_auth_window`，与 `unauthorized_source`
  （来源不存在/已停用）和 `submission_expired`（报价本身太旧）明确区分。
- 窗口随配置版本演进：升级只对之后新开的轮次生效，已发布的历史结果不被
  追改，重启重放逐轮给出相同结果。

## 聚合与降级语义

对每个资产、每个轮次：

1. **来源数量门槛**：有效（含新鲜度）报价数 `< min_sources` 时拒绝。
2. **陈旧剔除**：轮次关闭时 `now - quote_time > max_quote_staleness` 的报价
   被剔除并在报告中记录 `dropped_stale_quote`；剔除后再走数量门槛。
3. **确定性排序与去极值**：报价按价格升序，去掉 `trim_low`/`trim_high` 个
   极值，对剩余报价取整数均值。报价在排序前按 `来源ID → 报价时间 → 提交ID`
   稳定排序，保证相同输入永远得到相同输出。
4. **偏差门槛**：在**全部**报价（去极值前）上计算
   `(max-min)/median × 10000`（基点），超过 `max_deviation_bp` 判定为来源
   分歧异常并拒绝（极值不会被静默修掉）。
5. **反串通硬护栏**：若存在上一**新发布**（PUBLISHED，FALLBACK 不刷新）
   价格，则结果被钳制在
   `last × (1 ∓ hard_floor/ceiling_bp)` 区间内；即使全部来源一致偏离也无法
   突破，钳制动作写入报告 `clamped=true`。
6. **异常处置策略（按资产配置）**：被拒绝时按该资产的 `on_anomaly`
   处置——
   - `reject`：本轮该资产**不发布任何价格**，状态 `REJECTED`；
   - `fallback`（默认，空值等同）：若存在上一新发布价格且其年龄
     `≤ fallback_staleness`，本轮状态为 `FALLBACK`，复用旧价并记录
     `based_on_round`；超过最大年龄则拒绝发布。`FALLBACK` **不刷新**
     旧价的年龄，无法靠一直沿用把价格续命。
   每轮报告的 `anomaly_policy` 字段与 `checks` 里的
   `anomaly_policy=<策略> trigger=<触发原因>` 记录本轮实际使用的策略与
   依据。策略随配置版本演进，只约束新轮次，历史轮次的判定依据不变。

轮次整体状态：任一资产 `REJECTED` 则整轮 `REJECTED`；否则有 `FALLBACK`
则整轮 `FALLBACK`；全部正常才是 `PUBLISHED`。每个资产的全部判定依据
（数量、偏差基点、排序后价格、去极值后价格、钳制、回退/拒绝原因）都在
`RoundReport.Decisions[].checks` 中，可核验、可解释。

## 配置版本演进

- `InstallConfig` 只接受版本号严格递增的配置；重复/回退版本被拒绝。
- 轮次在打开时绑定当时的配置版本，`RoundReport.config_version` 永久记录；
  升级后仍打开的旧轮次继续按旧版本鉴权与结算。
- 历史轮次报告一经发布即不可变，升级不会追溯改写。

## 崩溃恢复

事件日志（JSONL，每条追加后 `fsync`）按顺序记录：配置安装、开轮、报价
接受、轮次定稿、轮次作废。启动时重放：

- 已定稿轮次连同报告原样恢复，轮次号连续不跳号、不重复发布；
- 崩溃时未完成的轮次按其绑定配置中的 `on_restart_open_round` 处理：
  - `resume`（默认）：连同已接受报价恢复为打开状态，可继续提交后定稿；
    重放旧提交会被识别为重复；
  - `void`：向日志追加一条作废记录，下一轮使用作废轮次号 + 1。
- 对同一条日志再次构造引擎，得到逐轮完全相同的报告（确定性重放）。

## 持久化完整性与定稿原子性

- 每条日志记录携带**严格连续的编号 `seq`**（从 1 开始逐条 +1，由存储在
  追加时分配）。打开/重放日志时逐条校验：缺号、乱序、无法解析的记录都会
  以 `LogIntegrityError` 明确报错，指出行号与偏移，不会把残缺历史当正常
  状态继续往后跑。
- **半条尾记录**：文件末尾若存在一条没有换行结尾的残缺记录，视为进程
  写到一半被杀留下的未确认尾巴（`Append` 只有在完整写入并 `fsync` 后才
  返回成功），打开时**安全截断**并通过 `TruncatedTailBytes()` 报告截掉的
  字节数，日志可继续正常追加；除此之外的任何损坏一律报错。
- **定稿原子性**：轮次报告先完整计算，写入事件日志成功后才应用到内存
  状态。写入失败时本轮保持未定稿（轮次仍打开、最近有效价不变），重启后
  已确认结果不丢、不重复发布、不重复计数。

## 自查入口（oracleverify）

`cmd/oracleverify` 对事件日志做完整审计：

```bash
go run ./cmd/oracleverify -store data/oracle-events.jsonl
```

它依次检查：

1. 记录完整性：`seq` 连续、无损坏记录（半条尾记录按上述规则截断并提示）；
2. 轮次连续性：已定稿/已作废轮次从 1 开始连续，不丢、不重、不跳号；
3. 逐轮重算：用日志中记录的配置版本、接受的报价与时间戳，按同一套确定性
   代码把每一轮重新结算，与记录的报告逐轮比对。

全部一致时输出 `RESULT: OK` 并以 0 退出；任何不一致输出 `RESULT: FAIL`
并非零退出，同时指明是哪一轮、哪个字段、记录值与重算值各是多少。

### 故意制造一个不一致来验证自查

```bash
# 1. 先跑一遍演示，产出事件日志
go run ./cmd/oracledemo -store data/oracle-events.jsonl

# 2. 自查应当通过
go run ./cmd/oracleverify -store data/oracle-events.jsonl   # RESULT: OK

# 3. 篡改第 1 轮定稿记录里的价格（把记录值改大 1000 微单位）
python3 - data/oracle-events.jsonl <<'EOF'
import json, sys
p = sys.argv[1]
lines = open(p).read().splitlines()
for i, ln in enumerate(lines):
    ev = json.loads(ln)
    if ev.get("type") == "round_finalized" and ev.get("round") == 1:
        ev["report"]["decisions"]["BTCUSD"]["price_micros"] += 1000
        lines[i] = json.dumps(ev)
        break
open(p, "w").write("\n".join(lines) + "\n")
EOF

# 4. 再次自查：应当 FAIL 并定位到第 1 轮的 price 字段
go run ./cmd/oracleverify -store data/oracle-events.jsonl
#   MISMATCH round 1:
#     - asset BTCUSD price: recorded=... recomputed=...
# RESULT: FAIL
```

## 可配置项含义

| 配置项 | 含义 |
| --- | --- |
| `version` | 配置版本号，必须严格递增 |
| `sources.*.active` | 来源是否在本版本中被授权 |
| `sources.*.key` | 来源 HMAC 密钥（本地演示用，生产应从密钥系统注入） |
| `sources.*.not_before` / `not_after` | 可选授权时间窗，`[起点, 终点)`，起点含终点不含；不写表示长期有效 |
| `assets.<A>.min_sources` | 发布新结果所需的最少不同来源数 |
| `trim_low` / `trim_high` | 聚合前剔除的最低/最高报价个数 |
| `max_deviation_bp` | 允许的最大报价离散度（基点，100bp = 1%） |
| `hard_floor_bp` / `hard_ceiling_bp` | 相对上一有效价格的硬区间（基点） |
| `min_positive_price` | 聚合阶段剔除低于该值的报价（结算时生效并记入报告） |
| `on_anomaly` | 异常处置策略：`reject`（本轮不发布）或 `fallback`（沿用最近有效结果，默认） |
| `submission_ttl` | 报价自报价时间起的有效时长 |
| `clock_skew` | 允许的报价时间超前量 |
| `max_quote_staleness` | 轮次关闭时允许的最大报价年龄 |
| `fallback_staleness` | 允许复用旧价的最大年龄，超过则拒绝发布 |
| `on_restart_open_round` | 重启时未完成轮次策略：`resume` 或 `void` |

### 新老配置兼容范围

- 不写 `not_before`/`not_after` 的来源长期有效，行为与引入窗口前完全一致；
- 不写 `on_anomaly` 的资产按 `fallback` 处置，与引入策略前的硬编码行为
  完全一致；
- 旧版本日志中的配置记录反序列化后这两个字段为空，重放结果不变；
- 除此之外的非法取值（`not_before >= not_after`、未知 `on_anomaly`、
  非正 `min_positive_price` 等）在 `InstallConfig` 校验阶段直接拒绝。

## 本地验证

需要 Go 1.25+（开发环境为 1.26），仅使用标准库，无外部依赖。

```bash
# 运行全部单测（-v 日志按“业务[…] 用例[…] 通过”输出覆盖情况）
go test -v ./...

# 启用竞态检测（需要 cgo）
CGO_ENABLED=1 go test -race -v ./...

# 脚本化端到端演示：内存存储（不落盘）
go run ./cmd/oracledemo -store :memory:

# 文件存储：运行一次写入事件日志，再运行同一路径演示崩溃恢复
go run ./cmd/oracledemo -store data/oracle-events.jsonl
go run ./cmd/oracledemo -store data/oracle-events.jsonl

# 自查：重算日志里每一轮并逐轮比对（详见“自查入口”一节）
go run ./cmd/oracleverify -store data/oracle-events.jsonl
```

测试覆盖的业务场景：

- 正常聚合（去极值整数均值的确定结果）；
- 并发提交（20 个并发提交、4 来源各计一次、结果确定）；
- 未授权/停用来源、过期/超前报价、错签、非法格式、未知资产、错轮、
  同来源重复、nonce 重放等拒绝路径；
- 授权时间窗端点归属（起点含/终点不含）、窗口外拒绝原因区分、窗口边缘
  并发提交的确定性、窗口随配置版本演进、无窗口来源长期有效；
- 来源不足时回退、回退超期时拒绝；
- 三种异常（来源不足/报价陈旧/偏差过大）× 两种处置策略（reject/fallback）
  的全部组合、沿用年龄恰在上限的边界、沿用不刷新年龄无法续命、策略随
  配置版本演进；
- 偏差超阈值拒绝；轮次长期不结算导致报价陈旧后拒绝；
- 全体来源一致偏离时硬护栏钳制；
- 配置升级只约束新轮次且历史不变、禁止安装旧版本；
- resume/void 两种崩溃恢复、重放复现与不重复发布；
- 日志缺号/乱序/损坏记录明确报错、半条尾记录安全截断后可继续追加；
- 定稿写入失败时内存与磁盘一致保持未定稿，恢复后不丢不重；
- 自查入口对一致日志全项通过，对人为篡改/重复定稿的日志精确定位到
  轮次与字段。

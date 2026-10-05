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
| `internal/oracle/storage.go` | 事件日志接口、JSONL 文件存储（fsync）、内存存储 |
| `internal/sim/source.go` | 本地模拟数据源（用配置密钥对报价签名） |
| `cmd/oracledemo/main.go` | 离线脚本化演示（正常/串通/升级/降级/重启） |
| `configs/config.example.json` | 配置示例 |

## 数据源授权规则

1. 每个来源在某一版本配置中有 `id`、`active` 标志与对称密钥 `key`。
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
   | `asset_not_configured` | 资产不在当前配置中 |
   | `bad_signature` | 用配置密钥验签失败（含冒用他人身份） |
   | `submission_expired` | 报价时间早于 `now - submission_ttl`，或超前超过 `clock_skew` |
   | `duplicate_submission` | 同一提交内容重放、nonce 已使用、或同来源同资产同轮重复报价 |

4. 授权身份以**轮次打开时的配置版本**为准；提交在该轮次内始终按该版本鉴权。

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
6. **降级策略**：被拒绝时若存在上一新发布价格且其年龄
   `≤ fallback_staleness`，本轮状态为 `FALLBACK`，复用旧价并记录
   `based_on_round`；否则状态为 `REJECTED`，**不发布任何价格**。

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

## 可配置项含义

| 配置项 | 含义 |
| --- | --- |
| `version` | 配置版本号，必须严格递增 |
| `sources.*.active` | 来源是否在本版本中被授权 |
| `sources.*.key` | 来源 HMAC 密钥（本地演示用，生产应从密钥系统注入） |
| `assets.<A>.min_sources` | 发布新结果所需的最少不同来源数 |
| `trim_low` / `trim_high` | 聚合前剔除的最低/最高报价个数 |
| `max_deviation_bp` | 允许的最大报价离散度（基点，100bp = 1%） |
| `hard_floor_bp` / `hard_ceiling_bp` | 相对上一有效价格的硬区间（基点） |
| `submission_ttl` | 报价自报价时间起的有效时长 |
| `clock_skew` | 允许的报价时间超前量 |
| `max_quote_staleness` | 轮次关闭时允许的最大报价年龄 |
| `fallback_staleness` | 允许复用旧价的最大年龄，超过则拒绝发布 |
| `on_restart_open_round` | 重启时未完成轮次策略：`resume` 或 `void` |

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
```

测试覆盖的业务场景：

- 正常聚合（去极值整数均值的确定结果）；
- 并发提交（20 个并发提交、4 来源各计一次、结果确定）；
- 未授权/停用来源、过期/超前报价、错签、非法格式、未知资产、错轮、
  同来源重复、nonce 重放等拒绝路径；
- 来源不足时回退、回退超期时拒绝；
- 偏差超阈值拒绝；轮次长期不结算导致报价陈旧后拒绝；
- 全体来源一致偏离时硬护栏钳制；
- 配置升级只约束新轮次且历史不变、禁止安装旧版本；
- resume/void 两种崩溃恢复、重放复现与不重复发布。

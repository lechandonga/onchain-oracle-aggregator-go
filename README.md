# 链上行情预言机聚合组件（本地模拟）

纯 Go 标准库实现的本地可运行预言机聚合器：用 **HMAC 身份的模拟数据源** 代替链下喂价方，
用 **本地追加写事件日志（fsync）** 代替链上存储。目标是让多来源报价在

- 提交乱序 / 并发提交
- 来源越权、过期、重放、格式非法
- 来源不足、报价陈旧、偏差过大、少数来源串通
- 聚合规则与来源配置升级
- 进程崩溃重启

之后仍然给出**确定、可解释、可复现**的结果。

## 快速开始

```bash
# 跑全部单测（带业务分组与用例日志）
go test -v ./...

# 竞态检测
CGO_ENABLED=1 go test -race ./...

# 内置剧情演示：并发/乱序提交、各类拒绝、不足回退、串通钳制、配置升级、崩溃重启
go run ./cmd/oracled demo --data-dir ./data/demo --config-dir ./configs

# 查看事件日志（按事件顺序打印）
go run ./cmd/oracled inspect --data-dir ./data/demo

# 用日志记录的提交与配置版本，逐轮重放复现某轮结论
go run ./cmd/oracled verify --data-dir ./data/demo --config-dir ./configs --round 6
```

## 目录结构

| 路径 | 职责 |
| --- | --- |
| `internal/config` | 版本化配置：来源身份、权重、有效期；聚合规则；配置自校验 |
| `internal/auth` | 提交格式校验、HMAC-SHA256 验签、身份有效期、未来时间戳检查 |
| `internal/aggregator` | 纯函数聚合：陈旧剔除、来源数门槛、加权中位、硬价格带、偏差检查 |
| `internal/store` | 追加写事件日志（JSONL + fsync）、顺序重放、损坏检测 |
| `internal/oracle` | 引擎：并发串行化、轮次生命周期、升级冻结、崩溃恢复 |
| `cmd/oracled` | 本地命令：`demo` / `inspect` / `verify` |
| `configs/v1.json`, `configs/v2.json` | 示例配置（升级前后两版） |

## 数据源授权规则

每个来源在某一版本配置中登记：`id`、HMAC `secret`、正权重 `weight`、
可选的 `not_before` / `not_after`（RFC3339 身份有效期）。

提交信封字段：`source_id, round_id, price, timestamp_ns, nonce, sig`，其中

```
sig = hex(HMAC_SHA256(secret, source_id + "\n" + round_id + "\n"
                              + price + "\n" + timestamp_ns + "\n" + nonce))
```

拒绝原因互不混淆（`internal/auth` 中为机器可读常量）：

| 原因 | 触发条件 |
| --- | --- |
| `malformed` | 空来源、轮次号非正、空 nonce、空签名、价格非有限正数、时间戳非正 |
| `unauthorized` | 来源未在**本轮配置版本**中登记，或签名不匹配 |
| `expired` | 提交时刻不在来源身份的 `[not_before, not_after]` 内 |
| `future_quote` | 报价时间戳超过“当前时刻 + 时钟容忍” |
| `duplicate` | 同一轮次同一来源已成功提交过（按已接受提交去重） |
| `closed_round` | 提交给非当前轮次，或当前没有 open 轮次 |

并发提交由引擎单一互斥锁串行化：合法提交先写持久事件再入内存，
因此同一轮每来源最多计入一次，不会重复计入或漏计；提交顺序不影响聚合结果。

## 聚合与降级语义

关轮时按固定顺序执行（每一步都写入结果的 `checks` 判定依据）：

1. **陈旧剔除**：`quote.timestamp + max_staleness_ns < close_time` 的报价剔除
   （边界 `==` 视为新鲜）。
2. **来源数门槛**：新鲜来源数 `< min_sources` 时不产出本轮价，按
   `insufficient_policy` 处理：`reject`（拒绝发布）或
   `fallback_last_confirmed`（回退到最近一次有效轮）。没有可回退价格时一律拒绝。
3. **加权中位**：按价格排序、累计权重严格过半的第一档价格；权重只影响中位位置，
   来源数门槛按“去重后的来源个数”计。聚合对输入顺序不敏感。
4. **硬价格带**：结果被钳制在 `[hard_min_price, hard_max_price]` 内——
   即使过半数来源串通，也不可能产出区间外价格。
5. **偏差检查**：相对最近一次有效价格的偏差超过 `max_deviation_bps`（基点）时，
   按 `deviation_policy` 拒绝或回退；首轮无参照时跳过。

轮次终局有四种状态：`published`（正常价）、`fallback`（发布但价格来自历史轮，
`derived_from_round` 指出来源）、`rejected`（明确拒绝，不产出价格）、
`voided`（恢复时作废的未完成轮）。**任何异常都不会静默产出价格。**

## 配置项与版本演进

`configs/v1.json` 为完整示例，字段：

- `version`：正整数，必须严格递增；`pair` 升级时不得改变。
- `sources.*`：`secret`、`weight(>0)`、`not_before`、`not_after`。
- `rule.min_sources`、`rule.max_staleness_ns`、`rule.max_deviation_bps`
- `rule.insufficient_policy` / `rule.deviation_policy`：
  `reject` 或 `fallback_last_confirmed`
- `rule.hard_min_price` / `rule.hard_max_price`：硬性价格带（`<=0` 端表示不限）

升级语义：调用 `Engine.Upgrade(v2)` 登记新版本后，**已经 open 的轮次冻结在创建时的
版本**（新来源不能向旧轮提交、新阈值不追溯），之后开启的轮次才使用新版本；
每个已发布结果都记录 `config_version`，历史轮次永远按其版本解释，可随时复现。

## 崩溃恢复

所有状态变化以事件形式追加到 `<data-dir>/events.log`，每条事件 `seq` 连续、
写入后 `fsync`。重启时完整重放：

- 已确认轮次（published/fallback/rejected）原样恢复，事件只追加不重写，
  因此**不丢失、也不会重复发布**（同一轮终局事件 seq 不变）。
- 尾部未完成轮次按策略处理：
  - `void`（默认）：追加 `round_voided` 并开启新一轮，避免“半轮价格”；
  - `resume`：恢复 open 状态与已接受提交，重放检测继续生效，可补齐后关轮。
- 日志损坏（行非法 / seq 断裂）时引擎拒绝启动，而不是在错误状态上继续。

## 本地验证 / 审计

- 每个终局结果包含：状态、价格、回退来源轮、全部已接受提交、
  每条规则检查的 `check/value/limit/detail`，对应 `events.log` 一行 JSON，可直接核验。
- `oracled verify -round N` 用事件日志与当时的配置文件，从第 1 轮逐轮重放到第 N 轮，
  比较状态与价格是否逐字段一致（不一致时非零退出）。

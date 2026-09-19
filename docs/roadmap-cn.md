# WorkBuddy 插件改造路线（三项）

> 目标：把 `workbuddy2api-hub` 里已验证的三个能力搬进 CPA 插件，
> 并让插件内部的模型列表可写、不再只读。
> 状态：**三项改造全部落地**。

---

## 背景：三个诉求

1. **模型维度的冷却** — 某个模型触发 429 只冻结该模型，账号仍能服务其他模型。
2. **倍率从上游同步** — 不再写死静态倍率，从 WorkBuddy 上游接口拉取。
3. **插件内部可配置模型列表** — 不是只读展示，能在插件自己的管理界面里增删改排序。

---

## 现状盘点（已读代码确认）

| 能力 | 现有实现 | 位置 |
|---|---|---|
| 模型列表 | 两个来源：上游动态发现（默认）或插件 config 的 `models` 数组（完整覆盖） | `model_readiness.go:312+` 的 `configuredModels` 分支 |
| 模型数据源 | `model_source_workbuddy.go` 拉上游，`model_source_modelsdev.go` 补元数据 | `models.go` / `model_store.go` |
| 429 判定 | `isSoftRateLimit()` 只做**分类**，分类完就结束，**没有任何冷却动作** | `policy.go:98` |
| 硬错误动作 | `reconcileAfterExecutorError()` 只处理硬积分错误（402 / 余额不足），软限流直接 return | `lifecycle.go:400` |
| 倍率 | 插件拉的是**积分余额**（credits），**没有"模型倍率"概念** | `credits_handler.go` |
| 管理 API | 管理路由，包含 `/accounts` `/credits` `/checkin` 等 | `management.go:161+` |
| 面板 | 单页 HTML，`/v0/resource/plugins/workbuddy/panel` | `panel.go` |

**关键结论**：三个诉求里，**冷却是完全缺失的**（只有错误分类没有动作），
**倍率也是缺失的**（现有只同步积分），**模型列表有基座但只能靠改 YAML 配，没有界面**。

---

## 改造一：模型维度冷却（新增）

### 设计

新增一张冷却表，键是 `(authID, modelID)`：

```go
type modelCooldown struct {
    AuthID    string
    ModelID   string
    Reason    string    // "rate_limit" | "quota"
    Until     time.Time
}
```

**触发点**（现在只有一个分类函数，要接上动作）：

```
executor_http.go  上游返回 429
      ↓
policy.go  isSoftRateLimit() 判定为软限流
      ↓
【新增】markModelCooldown(authID, model, duration)
      ↓
scheduler.go  handleSchedulerPick 过滤掉该 (auth, model) 对
```

**账号级 vs 模型级**的分流规则（照搬 hub 已验证的行为）：

| 上游响应 | 动作 |
|---|---|
| 429 且无积分语义 | **只**冻结 `(auth, model)`，默认 300s |
| 402 / 余额不足 / 额度用尽 | 走现有 `reconcileAfterExecutorError()`，整账号 disable（Global 则 delete） |
| 401 / 403 | 整账号冷却（token 失效是账号级的） |

### 需要动的文件

- **新增** `cooldown.go` — 冷却表 + 读写 + 过期清理
- `policy.go` — 增加 `isAccountLevelFailure()` 区分两类
- `lifecycle.go` — `reconcileAfterExecutorError()` 里接入冷却写入
- `scheduler.go` — `handleSchedulerPick` 里按 `(auth, model)` 过滤候选
- `model_readiness.go` — 快照里带上每个模型的冷却剩余时间
- `panel.go` — 账号行展示各模型冷却倒计时

### 风险

冷却状态只在插件进程内存里，CPA 重启会丢。要不要持久化到 `model_store` 的
缓存目录（复用已有的 `writeModelCacheAtomic`）需要你定。

---

## 改造二：倍率从上游同步（新增）

### 现状

`credits_handler.go` 拉的是 `/v2/billing/meter/get-user-resource` 之类的**余额**。
hub 侧倍率来自 `/v2/enterprises/personal/models` 的 `credits` 字段。

### 设计

1. 在 `model_source_workbuddy.go` 的上游模型拉取里，把每个模型的倍率一起解析出来，
   存进 `modelFacts`（现在有 `ID`/`Name`/`ContextLength` 等字段，加一个 `Credits`）。
2. 落地到 `pluginapi.ModelInfo` 时带上，供 CPA 计费/展示用。
3. **本地覆盖优先级**：上游值 > 插件 config 手填 > 内置默认。
   （对应你在 hub 上说的"本地修改是另一码事"——本地是覆盖层，不是唯一来源。）

### 已确认的约束

`pluginapi.ModelInfo`（CPA SDK v7.2.30）**没有倍率/成本字段**——
它只有 `ID` / `DisplayName` / `ContextLength` / 各类 modality 等描述性字段。

所以倍率**传不到 CPA 侧**，只能在插件内部用：

- 插件自己的面板上展示每个模型的积分消耗倍率
- 插件内部做额度预估（配合改造一的冷却，判断"这个号还够不够跑这个模型"）

如果将来要参与 CPA 的计费，得等上游 SDK 加字段，或者走插件的 usage 上报路径另说。

---

## 改造三：插件内可配置模型列表（增强现有）

### 现状

已有 `models` 配置项，语义是「非空即完整目录，绕过上游发现」。
**问题**：只能改 `config.yaml` 的 YAML 数组，且它是全量覆盖——
想"在上游列表基础上隐藏一个"做不到。

### 设计

把 `models` 从"全量覆盖"升级成**三种模式**，并存到插件自己的状态文件：

| 模式 | `models` 值 | 行为 |
|---|---|---|
| 动态（默认） | 空 / `null` / `[]` | 上游发现，现状不变 |
| 全量覆盖 | 非空数组 | 就用这些，绕过上游（现状不变） |
| **叠加** | `{"hide": [...], "order": [...], "add": [...]}` | 在上游结果上做隐藏/排序/追加 |

新增管理 API：

```text
GET  /v0/management/plugins/workbuddy/models        # 当前生效列表 + 来源 + 每个模型的冷却状态
PUT  /v0/management/plugins/workbuddy/models        # 写入 hide/order/add
POST /v0/management/plugins/workbuddy/models/hide   # 隐藏某个模型
POST /v0/management/plugins/workbuddy/models/restore# 恢复
POST /v0/management/plugins/workbuddy/models/move   # 上移/下移
```

面板上加一个「模型」区块：列出当前模型，每个带隐藏/上移/下移按钮，
以及（改造一完成后）该模型的冷却剩余时间。

### 需要动的文件

- `desensitize.go` — `configuredModels` 类型从 `[]string` 扩成结构体
- `model_readiness.go` — 按新模式合成最终列表
- `management.go` — 注册新路由
- **新增** `model_config.go` — 状态读写
- `panel.go` — 模型区块 UI

---

## 建议的落地顺序

```
改造三（模型可配置）  ← 已完成：model_config.go + 面板「模型列表」+ 三条管理 API
      ↓
改造一（模型冷却）    ← 已完成：cooldown.go + 调度过滤 + 面板徽标与解除按钮
      ↓
改造二（倍率同步）    ← 已完成：model_credits.go + v3/config 富目录合并 + 面板倍率编辑
```

每一步都要过 `go test ./...`（现有测试全绿，是回归基线）。

---

## 已完成：改造一（模型冷却）

`cooldown.go` 提供 `(authID, modelID)` 维度的冷却表，纯内存：

- `recordUpstreamFailure()` 从 `main.go` 的非流路径、`stream.go` 的异步泵、
  `collectUpstreamStream()` 的同步兜底三处接入，覆盖全部 executor 入口；
- 429 无积分语义 → 300s；其他上游错误 → 60s；硬积分错误交回 lifecycle；
- `scheduler.go` 的 `handleSchedulerPick` 按请求模型过滤候选；
  **全部候选都在冷却时仍返回一个**，避免直接 503；
- 管理 API `GET /cooldowns`、`POST /cooldowns/clear`（必须带 `auth_id`）；
- 面板账号卡带「冷却 N」徽标，每个被限流模型一行，带剩余时间和「解除」；
- `buildDashboardExWithCallback` 给每个账号填充 `cooling` 数组。

冷却状态同样只是进程内存：**配置重载不丢，CPA 重启后清空**。
需要跨重启持久化的话再说（可以复用 `writeModelCacheAtomic` 那套缓存目录）。

## 已完成：改造三（模型可配置）

`model_config.go` 提供 overlay（hide / order / add），叠加在基础目录之上：

- `GET /models`、`PUT /models`、`POST /models/action`（hide/restore/move/add）；
- 面板「模型列表」区块：隐藏、上移、下移、添加自定义模型；
- 优先级：`hide` > `order` > `add`，同一个 ID 同时 hide 和 add 时以隐藏为准。

与 YAML `models` 的关系：`models` 是持久化的**完整覆盖**，overlay 是内存里的
**增量调整**，两者互不干扰。

## 已完成：改造二（倍率从上游同步）

**关键发现**：倍率不在 cli agent 的 `models` 列表里（那只是 ID 字符串），
而在 `/v3/config` 的 `data.models` **富目录**里。插件原先只解析 agent 列表，
把整个富目录丢掉了 —— 所以不是"加个字段"，而是要把富目录按 ID 合并回来。

实测（生产 CN 账号，真实凭证）：16 个模型里 15 个带上了倍率。

```text
hy4-preview-f       x0.00     hy3        x0.00     hy3-x        x0.05
deepseek-v4.1-flash x0.03     glm-5.3    x0.79     glm-5.3-flash x0.06
glm-5.2             x0.79     glm-5.1    x0.79
```

`model_credits.go` 提供注册表，优先级 **本地固定 > 上游同步**：

- 上游值从 `parseWorkBuddyV3Config` 的富目录合并、以及 legacy 端点解析得到；
- 上游格式不统一（`x0.29` / `x2.20 credits` / `x0.00`），原样保留字符串，
  同时 `parseModelCreditsRate()` 解析出数值；
- 本地固定两个入口：YAML `model_credits`（持久化，重载时权威）、
  面板「倍率」按钮（进程内存）；
- 管理 API `GET /models/credits`、`POST /models/credits`；
- 面板每个模型一行带倍率徽标，免费促销（`x0.00`）显示为「免费」而不是裸 0。

**倍率传不到 CPA 侧**：`pluginapi.ModelInfo` 确认没有成本/倍率字段
（v7.2.30 SDK 已核对）。所以倍率只服务插件自身：面板展示 + 配合模型维度冷却
做额度预估。要参与 CPA 计费得等上游 SDK 加字段，或者走 usage 上报路径另说。

边界情况要注意：`x0.00` 的数值解析就是 0，必须靠 `source` 字段
（而非数值）区分"促销免费"和"上游没上报"。

---

## 已完成的仓库清理

- 删除 `qoderwork/` 及全部 QoderWork 文档、脚本、模型清单
- `registry.json` 只留 workbuddy，`repository`/`homepage` 指向 `bfSan/workbuddy-cpa-plugin`
- `.github/workflows/build.yml` 矩阵只剩 workbuddy
- 删除 `upstream` remote

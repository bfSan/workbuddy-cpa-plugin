# WorkBuddy 路由策略草案（仅文档）

> 状态：设计记录，0.9.29 不修改 `staged-scheduler`、CPA 宿主或 DSH→CPA 路由。
> 本文只定义后续演进边界与验收条件，不能视为已上线能力。

## 1. 当前边界

- CPA／`staged-scheduler` 负责账号候选、排序、选择、重试与换号。
- WorkBuddy provider 插件负责：解析账号 realm/client profile、模型目录与 readiness、模型/账号级 cooldown、quota 状态和请求 wire 一致性。
- 插件不注册 `scheduler.pick`，也不在 executor 内自行遍历账号池。
- Billing 的 endpoint-specific profile 与聊天 profile 分离；路由层不能为了统一版本而覆盖它。

## 2. 后续路由必须满足的硬约束

### 2.1 Realm 与客户端家族是账号属性

每个 auth 持久化 `clientMode`，并与 token issuer、auth domain 一起形成硬约束：

- Global token 只能走 Global host + WorkBuddy Global profile；
- CN WorkBuddy 账号固定走 CN WorkBuddy profile；
- legacy CN、缺少 `clientMode` 的账号固定回退历史 CLI profile；
- 识别出的 issuer/domain 不一致时 fail closed；
- 一次请求失败不能把同一 token 临时改扮成另一客户端家族或另一 realm。

路由器只能在满足硬约束的账号之间排序，不能把 profile 切换当作重试手段。

### 2.2 会话粘性优先于瞬时最优分

建议把 `(provider, downstreamSessionID)` 绑定到一个 auth lease：

1. 正常情况下，同一对话持续使用同一账号；
2. 模型不支持、明确 quota/cooldown、凭据失效或健康状态失败时才解除；
3. 解除后选择新账号应生成新的 account-scoped conversation identity；
4. lease 有明确 TTL、并发上限和释放原因，不使用永久内存粘性；
5. 进程重启后的恢复只保存不可逆摘要，不保存 prompt、token 或原始 session header。

这样可避免一个对话在多个 token、地域与出口之间跳动，也能减少 provider 侧异常行为指纹。

### 2.3 出口粘性与账号粘性分层

未来由独立 egress gateway 管理 `authID -> egressProfile`，scheduler 只消费健康状态：

- OAuth、refresh、catalog、chat、billing 对同一账号使用同区域、同类型出口；
- 切换只能发生在冷却窗口之后，并记录原因；
- sidecar/egress gateway 删除内部路由 header；
- 网关不可用时 fail closed，不静默改为宿主直连；
- provider 插件不接管代理池调度。

## 3. 建议的 staged pipeline

### Stage A：候选过滤（硬门槛）

过滤：

- provider/realm/clientMode 不匹配；
- auth disabled、refresh 失败或 issuer/domain 冲突；
- 请求模型不在账号的可执行目录；
- account/model cooldown；
- quota hard hold；
- egress profile 不健康；
- 已达到账号并发 lease 上限。

硬门槛不能通过调低分数绕过。

### Stage B：稳定性排序

建议按以下顺序排序，而不是只按余额：

1. 当前 session 已持有的健康 auth lease；
2. 与 session 前序请求相同的 egress profile；
3. 最近成功且失败率低的账号；
4. quota/credits 余量；
5. 当前并发与排队时间；
6. 成本或用户配置权重。

排序输入必须有时间戳和来源，过期状态只能降级为 unknown，不能伪装成 healthy。

### Stage C：租约与并发

- `pick` 与并发占位应原子化，避免多个请求同时选中最后一个可用账号；
- lease 必须在成功、失败、取消、客户端断连和超时路径释放；
- refresh/catalog 辅助请求不应重复占用推理 lease，但要受独立并发与抖动限制；
- 不允许因为 scheduler 自身重试而并行发送同一 user turn。

### Stage D：失败分类与重试

| 失败类别 | 建议动作 |
|---|---|
| 网络连接生命周期错误 | 同账号、同出口、同 profile 有界重试 |
| 429 / model cooldown | 同账号同模型冷却；按策略选择其他健康账号 |
| account quota / 402 / 6004 | 账号级或共享池 hold，不立即换出口试探 |
| 401 / refresh 失败 | 先走一次受控 refresh；失败后禁用并要求重新登录 |
| realm/profile mismatch | fail closed，禁止换 profile 重试 |
| 风控/11128/异常认证页 | 立即停止该账号自动重试，要求人工检查 |
| schema / tool-name 兼容错误 | 修复请求形状后同账号重试；不能通过换号掩盖插件 bug |

每个 user turn 设置总 attempt budget；跨账号重试也计入同一预算。

## 4. 观测与隐私

建议记录脱敏事件：

- auth 的不可逆短摘要、realm、clientMode；
- session lease 命中/解除原因；
- 模型、失败分类、attempt 序号、cooldown scope；
- egress profile ID（不是 IP 凭据）；
- wire profile 版本与配置 generation；
- 目录/readiness 数据年龄。

禁止记录 access/refresh token、cookie、完整 prompt、原始 session ID、tool arguments。内部 routing headers 必须在上游发送前清除。

## 5. 分阶段落地建议

1. **只读 shadow**：计算候选与排序，但不改变当前选择；比较差异。
2. **硬约束先行**：只上线 realm/profile/cooldown 过滤，不改排序权重。
3. **session lease**：小比例启用，验证粘性、释放和并发。
4. **sticky egress**：sidecar/gateway 独立上线后再接入健康信号。
5. **权重优化**：最后调整 credits、成本、延迟等软排序。

每阶段都应有独立开关、回滚路径和指标；回滚不能破坏 auth 文件中的持久化 `clientMode`。

## 6. 本轮明确不做

- 不修改 `staged-scheduler`；
- 不修改 CPA 宿主/二进制；
- 不让 provider 插件自行选择或轮换账号；
- 不以随机 UA、随机出口或跨 realm/profile 重试作为“伪装”；
- 不发送真实 upstream 推理请求做验收。

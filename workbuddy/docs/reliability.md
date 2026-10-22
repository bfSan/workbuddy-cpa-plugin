# WorkBuddy 登录恢复、配额冻结与签到确认

本功能位于 `bfSan/workbuddy-cpa-plugin/workbuddy`，不依赖 Trae 插件。账号排序、成本选择和重试换号继续由 CPA／staged 负责；插件只描述、验证并限制宿主选中的账号／模型，不注册 `scheduler.pick`。

## 1. 登录任务恢复

- 待完成 OAuth 任务按 state 的 SHA-256 分文件保存，支持多个并发登录；文件不使用明文 state 命名。
- 存储目录为宿主 `AuthDir/.workbuddy-oauth-pending`。当前宿主在 model/auth RPC 提供 AuthDir，生命周期 configure 不一定提供；model handler 仅初始化存储，不因 OAuth 存储问题阻断模型服务。
- Unix 使用私有目录 0700、记录 0600、原子写入和 fsync，目录/文件校验、符号链接防护及单进程锁。任务文件有数量（128）和单文件大小（128 KiB）限制。
- 待登录有效期沿用 30 分钟；拿到 token 后有 10 分钟完成/结果恢复窗口。过期或损坏的记录不会继续向上游请求。
- 保存受约束的 region/client profile、必要 cookie、token 和账号查询阶段。不反序列化任意 URL，恢复后重新遵守当前代理策略。
- 已取得 token 的任务恢复时直接继续账号查询或凭据保存，不重新消费 auth/token。面板仅在宿主凭据保存成功后返回成功；保存失败保留可恢复阶段。
- 没有宿主 AuthDir 或非 Unix 平台使用明确的 memory-only 模式（`persistent:false`）。Unix 已配置目录但不可安全写入时登录失败关闭，不假称已经落盘。

### 浏览器行为

面板只在 sessionStorage 保存 `{state,deadline,region}`，不保存 access token、refresh token、完整授权 URL 或凭据。关闭弹窗暂停轮询但不丢任务，同标签刷新可以从“恢复登录”继续；支持多个 state，过期项会清理。存储被禁用时仍可在当前页登录，但不能承诺跨刷新恢复。

**边界：没有后台自主 OAuth 轮询。** 页面关闭期间不会持有过期的宿主请求 callback 继续发请求；未完成任务需要重新打开原标签或提供有效 state 恢复轮询。服务重启恢复不等于重新发起一套授权。成功结果不提供领取客户端来源推断。

## 2. 可解释的配额冻结

复用 `/cooldowns` 和 `/cooldowns/clear`，保留原有 model/seconds 等字段，并增加：

- `reason`：rate_limit、call_quota、credit_quota、shared_credits_exhausted；
- `scope`：account_model 或 account；
- `occurred_at`、`updated_at`、`until`、`next_probe_at`；
- `recovery_condition`：upstream_retry_after、upstream_reset、bounded_ttl_probe；
- `evidence`：有限的分类标签，非原始上游响应；
- `persistent:false`、`storage:process_local`。

规则：

1. 请求频率限制默认 5 分钟；调用额度／积分拒绝默认 15 分钟。这些是探测间隔，不是上游承诺的恢复时间。
2. 有可靠 Retry-After／Reset 信息才使用上游时间，最大接受 24 小时；不猜午夜或时区。
3. 一般额度或积分不足仅影响当前 `(账号,模型)`。明确共享积分范围且明确耗尽才创建账号级冻结。
4. 裸 402/403、余额未知、单纯余额为零均不构成全账号耗尽证据，不能因此封住免费模型。
5. TTL 到期表示允许下一次请求探测，不代表已经确认恢复；不会自动发送模型调用。刚读到正余额只能解除共享积分耗尽，不解除每日次数冻结。
6. 执行入口、真正请求上游之前、异步流调度后都检查。检查只针对宿主选中的账号，不进行换号或排名。
7. HTTP 错误及 HTTP 200 SSE 错误帧都分类。正常模型回答中的“额度”字样不触发。配额错误包装不让上游业务 401/403 被宿主误判成整个凭据失效。
8. 人工清除冻结不修改 `disabled`。人工禁用、失效凭据及未知禁用来源不会自动恢复；仅明确旧自动 `disabled_reason=exhausted` 才允许按已有迁移逻辑恢复。
9. 不因共享余额为零自动删除 Global 凭据或禁用整个账号。

冻结表保留最多 4096 项，重启清空；容量满时淘汰最早到期项。不能撤回已经发出的请求。低层 HTTP 转发仅对已验证上游的精确聊天端点执行冻结检查，账单查询／目录刷新不受影响。

## 3. 签到确认、有限重试和记录

手动、定时与重试使用同一套结果判断：

1. 先查询签到状态，失败不盲目提交。
2. 已签返回 `already`；无活动返回 `no_activity`；不支持区域返回 `global`。
3. 领取后再查询状态。只有明确 `today_checked_in=true` 才返回 `confirmed:true`、更新已签到缓存。
4. POST 成功但状态没更新／验证超时返回 `pending_confirmation`，不制造已签到缓存，不显示成功 toast。
5. POST 网络超时或响应不确定时只安排状态验证，避免重复消费。连续点击也保留“只确认、不重领”的阶段。
6. 明确的鉴权或业务拒绝不自动重试。暂时限流／连接故障安排有限重试；每个账号每天最多 3 次后台重试，间隔为 1、2、5 分钟，不跨自然日。
7. 每次重试重新读取凭据，检查是否已删除／禁用，不保留请求级 callback ID 或凭据。关闭自动签到停止定时来源的重试；用户手动发起的有限验证可继续。
8. 状态与错误信息使用固定诊断，不把上游 body、token、URL 写入运行记录或日志。

### API 和面板

`GET /v0/management/plugins/workbuddy/checkin/status?auth_index=...`

返回开关、时刻、重试上限、`latest` 和 `history`；记录含账号索引、来源、结果、确认状态、发生时间、重试次数及下次重试时间。最多保留 512 个账号的最新状态、最近 100 条记录。

原 `POST /checkin` 保留 `{results,summary}`，结果增加 `confirmed/state/retry_scheduled/next_retry_at`。批量界面只标记确实确认的账号，不因一个账号成功而标记全部账号。

**持久性边界：签到记录／重试计划是进程内状态。** 重启清空，不跨重启保证重试次数，也不会自动补领错过的整点任务。此轮没有引入外部 cron 或启动补签到。请求没有等待退避间隔：重试由既有定时循环唤醒。

## 验证与部署边界

使用本地 mock 测试登录/磁盘/代理故障、并发恢复、配额作用域与有效期、不同执行路径、签到假成功与重试边界，面板使用合成 API 响应。没有执行真实账号的登录、领取、积分消耗或凭据失效测试。

验证命令：

```sh
go test -race -count=1 ./...
go vet ./...
node --test panel.test.js panel.state.test.js
```

以上是源码能力说明，不是线上生效证明。发布需单独审查当前工作区已有的其他改动，编译产物并用临时文件 + 原子 rename 更换已加载动态库，再重启宿主及核对注册版本。不要原地覆盖已加载的 `.so`。

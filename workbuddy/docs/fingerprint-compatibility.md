# WorkBuddy 客户端一致性与传输指纹方案

> 本文中的“伪装”只指与用户实际安装、合法使用的官方客户端保持协议与版本一致，
> 不是绕过访问控制或保证账号不会被风控。任何客户端、网络和行为异常都可能触发服务方策略。

## 1. 目标与证据

本插件的首要目标不是“多加一些随机 Header”，而是避免一个请求同时出现互相矛盾的身份：

- WorkBuddy User-Agent + CLI `X-IDE-*`；
- Global token + CN Origin/Referer；
- OAuth、刷新、目录、聊天分别宣称不同版本；
- 每轮请求都换 conversation ID，却又长期复用同一 access token；
- 工具 schema、消息顺序明显不像官方客户端。

2026-11-13 对用户 Mac 做了只读核查，未启动应用、未访问凭据、未发网络请求：

| Realm | Bundle ID | 安装版本 |
|---|---|---:|
| CN | `com.tencent.workbuddy.mac` | `5.9.3` |
| Global | `com.workbuddy.workbuddy-ai` | `5.9.4` |

两个约 173–174 MiB 的 `app.asar` 含同一套 `buildVlmGatewayHeaders` / `applyGatewayHeaders`：UA 运行时构造为 `WorkBuddy/<appVersion> WorkBuddy/<appVersion>`，只有产品配置提供 `platformVersion` 时才追加可选的 `CLI/<platformVersion>`；IDE type/name 固定为 WorkBuddy，IDE version 来自 app version；企业账号同时发送 tenant ID；聊天还使用 private-data、ACP connection、`x-codebuddy-request`、W3C/B3 trace 和 UUID request/message ID。静态代码命中 `X-Agent-Intent`、`X-Agent-Purpose`，未命中 `X-Agent-Type`，所以插件不会额外发送后者。`platformVersion` 未能从本次静态证据中确定，因此桌面 profile 省略可选 CLI 段，不猜测填入 standalone CLI 版本；独立 CodeBuddy CLI profile 仍使用另行审查过的 `2.172.1`。

插件 0.9.29 将这两个版本作为各自 realm 的单一版本源。CodeBuddy CLI 回退固定为已经审查过的官方包版本 `2.172.1`。版本不会后台自动追 latest：只更新 UA 而没有同步协议行为，比使用较旧但自洽的固定版本更危险。本版也不提供运行时 version override 或独立 profile diagnostics API；版本变更必须基于新证据同步修改中央 profile、测试并重新发布，避免配置把 OAuth/catalog/chat 拆成不一致版本。

## 2. 已实现：应用层 wire profile

### 2.1 Realm 和客户端家族分离

`wire_profile.go` 定义三个完整 profile：

- CN WorkBuddy macOS；
- Global WorkBuddy AI macOS；
- CodeBuddy CLI。

每个 profile 一次性决定 User-Agent、`X-IDE-Type`、`X-IDE-Name`、`X-IDE-Version`、Origin、Referer 和登录 platform。OAuth、token refresh、模型目录和聊天都从这里读取，不能各自硬编码版本。

每个账号在登录时持久化 `clientMode`；Global 强制 WorkBuddy，legacy CN 缺省固定回退历史 CLI，不再随全局配置重载而改变身份。模型目录只使用该账号的单一 profile，不再用同一 token 紧邻扮演 desktop 与 CLI 来求目录并集；防指纹一致性优先于目录完整度，缺项由账号级 cache/static fallback 处理。识别出的 JWT issuer 与 auth domain 必须一致，否则在请求前 fail closed。聊天意图和 conversation headers 只出现在 `/v2/chat/completions`。所有入站 raw session、凭据、旧 profile/trace/provider-owned headers 都先清除，再写入插件生成的 canonical 值。

### 2.2 ID 生命周期

- `X-Conversation-ID`：同一 account + downstream session 稳定；
- `X-Conversation-Request-ID`：同一 user turn 重试稳定，下一 user turn 改变；
- `X-Request-ID`：每个 HTTP 尝试独立；
- `X-Conversation-Message-ID`：每个消息发送独立；
- `acp-connection-id`：同一 downstream session 稳定；
- W3C/B3 trace：每个 HTTP 尝试重新生成。

所有 ID 使用 app 中 `randomUUID()` 一致的 lowercase RFC 4122 v4 外形。稳定 ID 由 SHA-256 前 128 bit 设置 v4/variant 位后格式化；随机 request/message ID 使用系统 CSPRNG，运行期系统随机源异常时使用启动 CSPRNG 密钥驱动的进程内 HMAC counter，启动时无法取得安全熵则拒绝加载。优先使用 CPA 提供的 canonical / execution / derived session metadata，以及显式 session headers；没有时才对第一条 user 内容做 SHA-256 摘要。turn key 同时包含 user message ordinal，因此同一会话内重复完全相同文本也不会复用上一轮 ID。派生出的 conversation/turn/ACP ID 值本身不直接嵌入原始 prompt、session 文本、UID 或 token；正常认证/账号 headers 仍会按协议发送 `Authorization`、`X-User-Id`、`X-Enterprise-Id` 等必要值。

0.9.29 的稳定 ID namespace 尚未引入持久 installation/device secret：它保证同一输入的稳定性和线上的 UUID 外形，但不把本地安装身份作为 HMAC 盐。后续若增加该能力，必须同时定义 secret 的原子持久化、权限、迁移、备份与显式 reset 语义；不能临时生成后导致重启即更换 conversation identity。

### 2.3 请求形状

已实现：

- developer → system 和首条 system；
- 只中和真实 A/B 确认的 DSH channel phrases，不替换整条 system prompt；
- JSON Schema 使用“两层遍历”：generic 层只处理 NUL pattern/tuple items，shape 层只沿标准 schema slots 修复 type/required/enum，避免把普通 payload map、`const/default/readOnly` 布尔值误当 subschema；
- 非法、空或超长 tool name 使用请求级、固定长度、前缀无歧义的可逆 hash 映射，并在同步 SSE、异步 SSE、折叠 completion 与 legacy `function_call` 中恢复；流结束仍有未完成别名时按截断失败；
- `tool → user` 尾部插入空 assistant 边界；
- `max_completion_tokens` → `max_tokens`；
- 强制 upstream stream。

### 2.4 Billing 是有证据的 endpoint-specific profile

签到/积分接口继续使用 `billingHeaders` 的独立 profile（包括已在线验证会影响 403/200 的 `WorkBuddy/5.5.6`、machine/session ID 与浏览器形 headers）。它不是聊天 transport，不能为了“版本看起来统一”而盲目改成 5.9.x。该 profile 不带 conversation/agent/trace headers；以后只有在端点级抓包或最小 A/B 证据更新后才调整。

## 3. 仍需加强：行为与出口一致性

这些比继续堆 Header 更重要，建议按顺序实现。

### 3.1 账号级稳定出口

同一个账号不应在短时间内跨国家/ASN/住宅与机房出口切换。建议在现有代理基础上增加：

1. `authID -> egress profile` 的稳定映射；
2. 失败时只在同区域、同类型出口中切换；
3. 映射变更设冷却窗口并记录审计事件；
4. OAuth、refresh、catalog、chat、billing 对同一账号走同一出口策略；
5. sidecar/网关必须删除内部路由 Header，绝不转发给上游。

当前插件只有 plugin-wide `proxy-url`，无法表达多账号 sticky egress；这应由独立 egress gateway 处理，而不是把代理池调度塞进 provider executor。

### 3.2 客户端行为节奏

建议增加保守的账号级规则：

- 每账号并发上限；
- token refresh、catalog refresh 的抖动窗口，避免所有账号整点齐发；
- catalog/usage 结果缓存，避免一轮推理触发多组辅助请求；
- 429/6004 等按现有 model/account 范围 cooldown，禁止立即换出口重试；
- 登录、刷新失败 fail closed，不自动降级为另一 realm/profile。

## 4. TLS/HTTP2 指纹：插件内目前没有完成

CPA 的 `HostHTTPClient` 最终使用宿主 Go transport。provider 插件能控制 URL、Header 和 body，但不能可靠控制：

- TLS JA3/JA4；
- ALPN 和扩展顺序；
- Chromium/Electron 的 HTTP/2 SETTINGS；
- Header 编码顺序；
- TLS session ticket / connection coalescing 行为。

因此 0.9.29 只解决应用层一致性，不能声称等价于 WorkBuddy macOS 的 TLS 指纹。

### 4.1 推荐的可选 sidecar 架构

若后续证据表明 WorkBuddy 确实校验 TLS/HTTP2，推荐独立的 provider transport sidecar，而不是修改 CPA 宿主：

```text
workbuddy-cpa-plugin
  -> Unix socket / localhost mTLS
  -> workbuddy-transport-sidecar
       - 固定上游 allowlist
       - realm/profile 选择
       - Chromium/BoringSSL 或经验证的 uTLS profile
       - HTTP/2 SETTINGS/profile
       - 账号级 sticky egress
       - 连接池和并发限制
  -> official WorkBuddy upstream
```

安全要求：

- 默认仍使用 CPA HostHTTPClient；sidecar 必须显式启用；
- sidecar 仅允许已编译的 WorkBuddy hosts/path，不接受任意代理目标；
- 本地通信使用 Unix socket 权限或 mTLS；
- 任何内部控制 Header 在出站前强制删除；
- sidecar 不保存 access/refresh token，不记录 request body；
- sidecar 不健康时 fail closed，不能静默直连；
- 提供脱敏请求摘要、profile、出口和 TLS 指纹观测值，弥补绕开 CPA 最终 transport 日志的缺口。

普通 HTTP CONNECT 代理不能解决问题：CONNECT 之后 TLS 握手仍由 CPA 的 Go client 发起。必须由 sidecar 终止本地 HTTP 并重新发起上游 TLS，或由插件使用专用 transport；后者会绕过宿主日志/代理约定，不作为首选。

### 4.2 Sidecar 分发方案（未来可选，尚未实现）

如果抓包证据确认必须做 TLS/HTTP2 profile，再按独立产品分发，不能把未验证的 transport 悄悄塞进 `.so`：

1. 构建 linux/amd64 与 linux/arm64 的固定版本 OCI image，同时产出 SHA-256、SBOM 和签名；镜像 tag 不使用浮动 `latest`。
2. 另提供最小化静态二进制包、systemd unit 与 tmpfiles 配置；默认只监听权限受限的 Unix socket，不监听公网或 LAN TCP。
3. profile、上游 host/path allowlist、egress policy 与插件版本形成兼容矩阵；不兼容时启动失败，禁止自动降级直连。
4. 安装/升级采用“下载到临时路径 → 校验签名/摘要 → 原子切换 → health check → 回滚”的流程；运行进程对应的二进制不得原地覆盖。
5. 先发布 `observe-only`：记录脱敏后的协商 TLS/ALPN/H2 指纹与目标 profile 差异，不承载真实推理；确认后再对单一授权测试账号 canary。
6. CPA plugin 只通过显式配置启用 sidecar；配置关闭时继续使用现有 HostHTTPClient，并明确标注 TLS 指纹未解决。
7. CI 门槛包括 allowlist 绕过、header 清理、socket 权限、无 token/body 日志、断连 fail-closed、签名验证和回滚演练。

该方案解决的是可审计分发与生命周期，不等于已经获得官方 Electron 的等价 TLS 指纹；具体 profile 仍需证据驱动。

## 5. 验证与上线门槛

### 离线（必须）

- golden header matrix：CN / Global / CLI，OAuth / refresh / catalog / chat；
- 同 session、同 turn、下一 turn、跨 account 的 ID 生命周期测试；
- tool schema、历史 tool refs、流式拆分工具名 round trip；
- 非聊天请求不得出现 conversation/agent intent headers；
- 恶意/陈旧 raw session、refresh/cookie、profile、trace headers 不得透传；
- 同一 auth 的 catalog/chat/refresh 只使用持久化的一个 client family；
- 禁止 Global token 使用 CN host/origin/profile，issuer/domain 不一致必须 fail closed；
- race test、go vet、静态 secret scan。

### 在线（需要用户单独授权）

本次改造不发送真实推理请求。若以后做 canary：

1. 先用一个明确授权的测试账号；
2. 固定出口与版本；
3. 只发最小请求并记录脱敏 wire 摘要；
4. 一次只改变一个变量；
5. 出现 11128、认证异常、风控页或账号告警立即停止，不自动换号/换 IP 重试。

## 6. 明确不做

- 随机轮换 UA/version/header；
- 后台自动跟随 npm 最新版本；
- 把整个 DSH system prompt 替换成通用 agent prompt；
- 伪造浏览器 cookie、设备证明或风险上下文；
- 遇到风控后自动换账号、换地区、换出口连续试探；
- 为了 TLS 指纹修改 CPA 宿主源码。

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

插件 0.9.29 将这两个版本作为各自 realm 的单一版本源。CodeBuddy CLI 回退固定为已经审查过的官方包版本 `2.172.1`。0.9.30 在保留这三个默认 pin 的基础上，增加彼此独立的 `wire_version_cli`、`wire_version_cn_desktop`、`wire_version_global_desktop` override；只接受无前后空白、无 `v` 前缀、每段无多余前导零的三段数字 `x.y.z`，空字符串才表示继续使用 built-in pin。它不会查找或后台跟随 latest。override 在一次 immutable runtime snapshot 中校验并发布，同时重建对应 UA / `X-IDE-Version`，OAuth、refresh、catalog 和 chat 继续从同一 resolved profile 读取；进行中的 OAuth flow 会把启动时版本写入 checkpoint，配置重载或重启也不会在 flow 中途换版本。catalog cache identity 包含 resolved version，避免新旧 profile 共用目录结果。只读 management 路由 `/plugins/workbuddy/wire-profiles` 仅返回 realm、client mode、profile、version/source 与 installation identity 的安全状态，不返回 token、UID、enterprise/session ID、raw header、secret 或本地路径。

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

所有 ID 使用 app 中 `randomUUID()` 一致的 lowercase RFC 4122 v4 外形。0.9.30 的 conversation/turn/ACP 稳定 ID 使用 installation secret 驱动的 HMAC-SHA256，包含独立用途域与长度前缀，取前 128 bit 后设置 v4/variant 位；随机 request/message ID 使用系统 CSPRNG，运行期系统随机源异常时使用启动 CSPRNG 密钥驱动的进程内 HMAC counter，启动时无法取得安全熵则拒绝加载。优先使用 CPA 提供的 canonical / execution / derived session metadata，以及显式 session headers；没有时才对第一条 user 内容做 SHA-256 摘要。turn key 的 ordinal 只计 user messages，因此插入 system/developer/tool/assistant wrapper 不会把同一 user turn 重新编号，而同一会话内后续重复完全相同 user 文本仍会得到新 ordinal。派生出的 conversation/turn/ACP ID 值本身不直接嵌入原始 prompt、session 文本、UID 或 token；正常认证/账号 headers 仍会按协议发送 `Authorization`、`X-User-Id`、`X-Enterprise-Id` 等必要值。

### 2.2.1 Installation secret 生命周期

- lifecycle register/reconfigure 不携带 HostConfigSummary，插件不会因此退回 user-global 路径。首次携带绝对 `Host.AuthDir` 的 model/auth RPC 会在该实例的 `Host.AuthDir/.workbuddy-installation-identity/installation.key` 创建 32-byte raw secret；chat 在此之前 fail closed。首次 scope 在进程内固定，后续发现不同 AuthDir 会失败并要求重启，不能热切换 namespace。同一 Unix 用户的独立 CPA AuthDir 因而不会自动共享 key。
- Unix identity 子目录必须是当前 euid 所有的 `0700` 目录，key 与 lock 必须是当前 euid 所有、单硬链接的 `0600` regular file。读取使用 pinned directory + `openat(O_NOFOLLOW)`；创建使用 pinned/no-follow AuthDir、`mkdirat`、parent fsync、exclusive temp、file fsync、atomic rename 和 identity-directory fsync。非 Unix 在有等价 ACL/no-follow/lock 实现前 fail closed，不退化为临时 key。
- key 只驻留本地文件和进程内存，不写 token/auth JSON、日志、upstream header/body 或 diagnostics。携带 AuthDir 的 model/auth RPC 无法安全读取/创建 key 时直接失败；chat header 在 key 未 ready 时拒绝构造，因此不会访问 upstream，也不使用 unsalted 或 process-only fallback。lifecycle configure 本身不携带 AuthDir，文档不把该路径描述为全局 proxy transport blocked。
- 从已发布 0.9.29 升到该实现时，稳定 conversation/turn/ACP ID 会一次性切换到新的 HMAC namespace；access/refresh token、账号文件和 billing machine/session profile 不改变。升级时已存在的 OAuth pending v1 checkpoint 没有 frozen client version，会被安全拒绝并要求重新登录；不会把当前 override 套到旧 flow。升级后保持该文件即可跨重启稳定。
- 备份/恢复必须在 CPA 停止时完成并保持 `0600`；恢复同一个 key 等价于恢复同一 installation namespace。不要把一个 key 共享给多个独立实例，除非明确要克隆该 namespace。
- reset 只支持离线操作：停止 CPA，备份后删除 **仅** `Host.AuthDir/.workbuddy-installation-identity/installation.key`，再启动生成新 key。若迁移 AuthDir，要保持 namespace，必须在停机时连同整个 `.workbuddy-installation-identity` 目录按原权限迁移；否则会视为新实例。不存在在线 reset API。reset 会同时改变该安装的 conversation、turn 与 ACP stable ID，不能用于日常轮换或规避服务端策略；账号凭据不会因此删除。
- `/wire-profiles` 只报告 `ready`、`hmac-sha256-v1`、`owner_only_auth_scope` 与 `offline_only`，不返回 AuthDir、key fingerprint、创建时间或路径，以免额外形成可关联标识。

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

因此 0.9.30 仍只解决应用层一致性，不能声称等价于 WorkBuddy macOS 的 TLS 指纹。

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
- 同 session、同 turn、下一 turn、跨 account/installation secret 的 ID 生命周期测试；
- installation key 的 0700/0600、symlink/weak-file 拒绝、原子重载、entropy failure 与 offline reset 测试；
- user-only ordinal 必须不受 system/developer/tool/assistant wrapper 插入影响；
- 三个 version override 的 strict parse、UA/IDE/OAuth 一致性、in-flight OAuth freeze、cache 隔离与 diagnostics 脱敏测试；
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

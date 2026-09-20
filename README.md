# WorkBuddy CPA Plugin

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 插件：把 **腾讯 CodeBuddy / WorkBuddy**
（`copilot.tencent.com` CN 与 `workbuddy.ai` Global）接成 CPA 的原生 OAuth Provider。

单插件仓库，只做 WorkBuddy 一件事。功能、构建与配置见 [workbuddy/](workbuddy/)。

## 能力

| 项 | 说明 |
|---|---|
| OAuth 登录 | 多账号 `workbuddy-<uid>.json`，CN 与 Global 共用同一插件与同一配置块 |
| 模型目录 | 默认按账号动态发现并缓存；可选 YAML 权威清单覆盖发现；缺失元数据从 models.dev 补全。宿主 alias/exclusion 仍生效，插件另有持久化 `hidden_models` 过滤 |
| Executor | OpenAI 兼容 chat completions，流式（真 SSE）与非流式都支持 |
| 积分生命周期 | CN 账号积分耗尽自动 disable，签到恢复后自动启用；Global 账号耗尽即删除 |
| 每日签到 | CN 账号本地时间 09:00 / 21:00 自动签到，面板可手动「全部签到」 |
| 管理面板 | 账号、积分、模型状态一页可见 |

## 安装

从 Release 下载对应平台产物，放进 CPA 的 `plugins` 目录：

```bash
unzip workbuddy_0.9.3_linux_amd64.zip
cp workbuddy.so /path/to/cliproxyapi/plugins/workbuddy.so
# 或平台子目录布局：plugins/linux/amd64/workbuddy.so
```

在 `config.yaml` 启用：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy:
      enabled: true
```

可选的插件配置项（`models`、`checkin_auto` 等）说明见
[workbuddy/README_CN.md](workbuddy/README_CN.md)。

## 构建

```bash
cd workbuddy
make build      # 产出 dist/workbuddy.<so|dylib|dll>
make test
```

也可以直接用 Go：

```bash
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o workbuddy.so .
```

多架构产物与 Release 由 CI 在 tag `workbuddy-v*` 时构建。

## 许可

MIT，见 [workbuddy/LICENSE](workbuddy/LICENSE)。

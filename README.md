# Mihomo Monitor

本仓库包含两个独立发布物：

- `mihomo-monitor-server`（服务版本 `0.2.4`）：监控、连接列表、Provider、代理组选择和独立登录。
- `mihomo-monitor-v0.2.3.so`：CPA 菜单入口，只注册「Mihomo 监控」并打开监控页面。

监控模块直接连接 Mihomo Controller，不依赖 CPA 或 CPAMP 的内部接口。其他面板只需增加一个指向 `/mihomo-monitor/dashboard` 的入口适配。CPA 插件使用 ABI v1，且不参与模型请求。

运行配置：

| 环境变量 | 用途 |
| --- | --- |
| `MIHOMO_MONITOR_ADMIN_KEY_FILE` | 独立管理密钥文件 |
| `MIHOMO_MONITOR_SECRET_FILE` | Mihomo Controller secret 文件 |
| `MIHOMO_MONITOR_CONTROLLER_URL` | Controller 地址，默认 `http://mihomo:9090` |
| `MIHOMO_MONITOR_SELECTABLE_GROUPS` | 允许手动选择的组，逗号分隔 |

登录成功后使用 12 小时 HttpOnly、Secure、SameSite=Strict 会话。代理组切换仍要求页面确认，并由服务端重新校验实时候选列表。

## 部署边界

1. 将 `mihomo-monitor-server` 放入构建上下文的 `dist/`，用附带的 `Dockerfile` 构建独立容器。仅为它挂载独立管理密钥和 Mihomo Controller secret，Controller 不对公网开放；通过反向代理将 `/mihomo-monitor/` 转发至容器的 `18319` 端口。
2. 将 `mihomo-monitor-v0.2.3.so` 放入 CPA 的 `plugins/linux/amd64/`。启用插件 ID `mihomo-monitor`，将其配置中的 `store.version` 设为 `0.2.3` 以触发版本化热加载。CPA 的插件资源页会重定向至监控页面。
3. 面板只显示 CPA 插件注册的一个菜单入口。当前 CPAMP 前端补丁仅在该插件页面向 Mihomo iframe 传递管理密钥和主题，以保留自动登录；更换面板时需重新应用 `cpamp/mihomo-plugin-auth.patch` 并验证菜单没有重复。

`SHA256SUMS` 随 Release 一起发布。生产环境不应直接公开 `18319` 或 Mihomo Controller，也不要把密钥放进 Git 仓库。

Linux amd64 构建：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/mihomo-monitor-server ./cmd/server
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o dist/mihomo-monitor-v0.2.3.so ./cmd/plugin
```

发布仓库仅包含本模块，不包含生产 Compose、数据库、密钥或 CPAMP 定制面板。线上版本可独立于 GitHub Release 升级；下载新插件后仍需显式设置目标版本，不能直接覆盖正在使用的 `.so`。

## 0.2.4 发布说明

本次版本号对应独立监控服务；菜单入口插件保持 `0.2.3`。当前生产仍使用 `0.2.2` 入口，与服务版本分别管理。

- 代理组健康状态跟随实际出口，区分健康、故障和未知。
- Provider 提供节点状态和健康数量，连接详情注明截断情况。
- 仪表盘保留独立登录、出口选择、亮暗主题及键盘可访问的标签页。
- Release 中的服务二进制与已部署的 `0.2.4` 服务文件一致；`SHA256SUMS` 可用于核对。

源码验证：`go test -count=1 ./...`、`go vet ./...`；Linux 服务使用 Go 1.27.1 构建。入口插件仍提供已发布的 `mihomo-monitor-v0.2.3.so`。

# 架构与维护说明

澄光网关采用 Go 管理服务、Vue 3/Vite 前端、独立 Nginx 数据面与 JSON 持久化。配置文件直接启动时由主程序准备代理；原 systemd 部署仍由两个 unit 管理生命周期。业务请求不经过 Go 管理 HTTP 服务。

## 模块职责

| 目录 | 职责 |
| --- | --- |
| `web` | 管理界面、API 传输、页面生命周期和表单校验 |
| `internal/app` | 管理 API、认证、服务配置、发布任务、证书、监测、通知与备份 |
| `internal/engine` | 配置校验、Nginx/njs 渲染、应用与本机代理验证 |
| `internal/model` | 共享数据模型及支持的状态格式升级 |
| `internal/statecheck` | 离线状态格式与应用账本检查 |
| `internal/atomicfile` | 原子文件写入与持久性处理 |
| `internal/dnsprovider`、`internal/dnspod` | DNS 提供商抽象与客户端 |
| `internal/cloudflare`、`internal/teo`、`internal/ssl` | 边缘服务与云端证书客户端 |
| `internal/traffic`、`internal/globalping`、`internal/notify` | 流量统计、外部探测与通知投递 |
| `deploy`、`scripts` | 安装升级、构建、打包与检查工具 |

## 配置与网络

直接启动支持三项 `config.json`：`gatewayIp`、管理端口 `port` 与首次登录密钥 `initialKey`。显式 `--config PATH` 优先，否则查二进制同目录，再在不存在时查当前工作目录。显式 `--gateway-ip` 且未指定 `--config` 时保留原 CLI 逻辑；显式 CLI 参数可覆盖已加载的配置值。数据/缓存目录仍为 `/var/lib/gateway`、`/var/cache/gateway`。

配置文件模式的正式启动先检查运行依赖，已齐全不调用 APT；缺少时仅在 Linux、root 权限且 APT 可用的环境自动补齐，再验证 Nginx/njs 兼容性和 `www-data` 账户。需要安装 Nginx/njs 而机器已有 Nginx 进程时拒绝操作，避免影响其他站点；安装期间抑制默认服务自启，不覆盖已有系统启动策略。依赖检查或安装失败会停止启动，不以仅管理 HTTP 就绪冒充完整启动。

随后初始化本项目的受管 Nginx 配置并启动独立代理，使用已应用快照或首次空配置，不自动发布草稿；直接启动不创建 systemd unit。原 CLI/env + systemd 路径以及开发 `--dry-run` 不执行此自动准备流程。

`initialKey` 只在首次建立认证时使用，不覆盖已有密码哈希；改密后可清空文件中的密钥。真实 `config.json` 不提交源码，发行包的空模板来自 `config.example.json`。原 systemd 仍从 `/etc/default/gateway` 读取环境变量并传给 CLI。网关 IPv4 由部署者指定，不设固定网段；上游为带端口的 HTTP/HTTPS IPv4 地址，可通过已有路由跨子网访问。

管理监听限定为所配置的网关 IPv4 或 `127.0.0.1`，不使用通配地址。管理请求校验 Host，写请求还需同源与会话校验。Nginx 业务入口按域名的 Host/SNI 或服务端口匹配到明确上游，HTTPS 上游保留证书校验。

## 草稿、应用与云操作

- `state.json` 保存草稿，`applied.json` 保存已应用快照；保存表单不会立即改变线上入口。
- 本机应用先生成并检查候选代理配置，再切换并验证 Nginx 运行代次；失败时按事务状态恢复。
- 本机应用以整份草稿为范围。发布预览应包含其他尚未应用的服务变化，不能将单服务操作误认为只影响一个入口。
- DNS/CDN 变更由发布任务与资源账本记录。云端成功、本机成功和访客可达分别表示不同阶段的结果。
- 云写入超时不等于写入失败。无法确认的操作保留未知状态，先核对提供商资源，避免盲目重试或回滚已完成步骤。
- 配置历史恢复为草稿；文件恢复不会自动撤销 DNS 或边缘平台上的变化。

## 持久化与离线检查

当前状态格式为 **schema 6**，定义在 `internal/model/model.go`。服务拒绝隐式迁移；升级脚本在停机备份后显式处理支持的 schema 4/5 前序格式，未知版本和损坏文件停止处理。

文件写入使用同目录临时文件、`0600` 权限、同步和 rename；Linux 上继续同步父目录。rename 后目录同步失败属于“文件已提交、断电持久性尚未确认”，调用方需保留已提交状态，不能误判为完全未写入。Windows 不保证同等的父目录同步语义。

离线只读检查应在服务停止、文件稳定时执行：

```bash
/opt/gateway/gateway state-check --data-dir /var/lib/gateway
```

该命令检查状态格式、字段、UTF-8、文件大小与应用账本一致性，只输出版本、修订号和计数。它不替代 Nginx、证书、云端资源或业务可达性检查。

账号、监测、通知、配置历史等使用独立文件；证书、私钥和认证材料也属于数据目录。文件权限不等于加密，应把数据目录与源码分开并纳入受保护备份。恢复步骤见 [操作手册](operator-guide.md#10-备份历史和恢复)。

## 前端请求与类型

- 核心传输、页面作用域、服务表单和接口 DTO 使用严格 TypeScript；部分 API 门面和发布 hook 使用 checked JavaScript。
- 类型迁移是渐进式的，部分旧页面和次要诊断响应仍待细化；覆盖范围见 [前端类型说明](../web/TYPING.md)。
- 读取默认超时 20 秒，写入默认超时 90 秒。读取可取消；写入超时只停止等待，不代表服务端取消或回滚，也不自动重试。
- 页面卸载或会话变化后忽略旧请求结果，避免旧响应干扰新页面与新会话。发布任务由服务端持有，前端通过请求 ID 恢复进度。
- TypeScript 固定为 `5.9.3`，升级工具链时需确认 `vue-tsc` 兼容性并通过项目类型检查。

## 后端生命周期

主程序处理 SIGINT/SIGTERM，并使用 12 秒共享停机预算，低于 systemd unit 的 20 秒停止超时。停机开始后拒绝新写入，取消后台任务，并排空管理 HTTP 和本地证书控制 socket。

配置文件直接启动只清理本次新启动的受管 Nginx；启动失败也清理本次创建的代理。对启动前已经存在、且通过身份与配置验证的受管实例不取得停止权，不向其他 Nginx 进程发送退出信号。

已确认完成的步骤保留；无法在预算内确认的步骤标为未知、任务标为中断，晚到结果不得覆盖中断记录。重启通过持久化任务和事务记录核对恢复，不因连接中断自动重试云写入。

## 构建与维护

产品版本来自 `internal/buildinfo/version.txt`。Vite 页面、CLI 和 `/healthz` 共用版本信息；正式构建另记录源码提交、构建时间、dirty 状态及文件摘要。

```bash
node scripts/build.mjs
node scripts/package-release.mjs
```

构建脚本执行前端依赖安装、类型检查、Vite 构建和 Linux ARM64 交叉编译。构建后运行 `bash scripts/check-linux.sh` 进行 Node/Bash 语法检查和 `go vet`；敏感信息扫描与验证范围见 [贡献指南](CONTRIBUTING.md)。这些检查不包含自动化业务或 Nginx 集成测试。

GitHub Actions 的分支/PR 检查与 tag 发行使用相同检查流程；tag 发行生成草稿 Release，审核后再公开。完整说明见 [构建与发行](github-release.md)。

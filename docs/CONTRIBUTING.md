# 贡献指南

欢迎通过 Issue 和 Pull Request 修复问题、改进文档或贡献功能。较大的功能和架构变更建议先讨论使用场景、范围与兼容性，再开始实现。

## 开发环境

- Node.js 22 与 npm，前端依赖使用 `web/package-lock.json`。
- 符合 `go.mod` 的 Go 工具链；构建脚本支持通过 `GO_BINARY` 指定 Go 可执行文件。
- 发行包的目标为 Linux ARM64；Windows/macOS 可用于源码开发和交叉编译，Nginx/systemd 行为需在 Linux 验证。

在仓库根目录安装依赖并构建嵌入资源：

```bash
npm --prefix web ci
npm --prefix web run typecheck
npm --prefix web run build
go build -o .local/gateway .
```

Go 使用 `go:embed` 嵌入 `web/dist`，因此后端构建与静态检查前需先生成前端资源。

## 本地运行

源码开发可将根目录 `config.example.json` 复制为 `config.json`，填写 `gatewayIp`、`port` 和仅用于本机开发的 `initialKey`。无 `--gateway-ip` 参数时默认读取二进制同目录的配置，不存在则查当前工作目录；也可用 `--config PATH` 明确指定。实际配置不提交 Git，已有密码不会被文件中的密钥重置。字段约束见 [操作手册](operator-guide.md#34-配置文件直接启动)。

配置文件模式的正式启动会检查系统依赖，在 Linux + root + APT 下补齐缺包，并启动独立 Nginx。日常源码开发优先使用下面的 CLI `--dry-run` 示例，不会执行该自动准备流程，也不要求开发机安装代理依赖。

以下 Bash 示例保留原 CLI 方式，使用独立开发目录和回环监听。显式 `--gateway-ip` 且未指定 `--config` 时不会自动加载配置文件；地址按开发网络设置，没有固定 VPN 网段要求：

```bash
go run . serve \
  --gateway-ip 10.20.0.9 \
  --listen 127.0.0.1:18080 \
  --data-dir "$PWD/.local/dev-data" \
  --cache-dir "$PWD/.local/dev-cache" \
  --dry-run
```

访问 `http://127.0.0.1:18080/`，初始密码位于 `.local/dev-data/initial-password`。`--dry-run` 跳过系统依赖安装、Nginx 启动及验证/重载，仍会写入开发目录；它不是云 API 的模拟开关。使用专用开发数据，不要指向生产数据目录或填入生产账号。

发行构建和归档命令见 [构建与发行](github-release.md)。

## 构建与静态检查

按改动范围选择类型、构建和静态检查，并在 Pull Request 中说明实际执行的命令与未覆盖范围。

```bash
# 安装前端依赖、类型检查、构建 UI 并交叉编译 Linux ARM64。
node scripts/build.mjs

# Node/Bash 语法与 Go 静态分析；先完成上面的构建。
bash scripts/check-linux.sh

# 扫描待提交源码中的敏感信息。
node scripts/scan-secrets.mjs
```

`check-linux.sh` 仅执行 Node/Bash 语法检查和 `go vet`，不安装系统代理依赖、不启动 Nginx，也不运行自动化测试。构建和静态检查通过不等于实际业务链路已验证；涉及运行行为时，按需要在隔离环境核对对应路径，并如实记录结果。

## 代码与文档约定

- 沿用现有 Go/Vue 结构，优先保持模块职责清楚，避免无关重构。
- 代码与 Markdown 使用 **UTF-8 无 BOM**；中文文案保存为 UTF-8 常量，避免经不确定编码的 shell 重写。
- 后端变更关注输入校验、幂等性和错误恢复；前端变更包含加载、空态、失败与成功反馈。
- 修改状态格式、配置或发布流程时，说明升级兼容性、迁移与回滚影响。
- 增加依赖时同步锁文件和 [第三方声明](../THIRD_PARTY_NOTICES.md)。

## 提交 Pull Request

1. 从功能分支提交范围明确的改动，检查 diff 中是否混入无关文件。
2. 给出问题背景、实现方式、验证命令及结果；涉及 UI 时可附使用示例数据的截图。
3. 不提交运行数据、云凭据、私钥、日志或备份；域名示例使用 `example.com`、`example.net` 等保留名称，VPN 地址明确标为可替换示例。
4. 提交前运行 `node scripts/scan-secrets.mjs`，暂存后运行 `node scripts/scan-secrets.mjs --staged`。扫描失败时核对是否混入真实凭据，不通过豁免整个目录规避检查。
5. 二进制通过发行工作流分发，不加入源码提交。

报告问题时请提供版本、部署平台、复现步骤、期望与实际结果，并对日志和截图脱敏。不要在公开 Issue 附上完整数据目录、Cookie、Token 或证书私钥。

贡献按本项目 [MIT License](../LICENSE) 接收；请确保有权提交相关代码和素材，并保留第三方版权与许可证声明。

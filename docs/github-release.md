# GitHub 构建与发行

源码通过 Git 管理，可执行文件由构建脚本或 GitHub Actions 生成，并通过 GitHub Releases 分发。当前发行目标为 **Linux ARM64 / aarch64**；直接运行支持在 root 权限下通过 APT 自动补齐依赖，不要求 systemd。可选安装脚本面向 Debian 系、APT 和 systemd。

产品版本来自 `internal/buildinfo/version.txt`；当前为 `0.8.0-dev`，对应 tag `v0.8.0-dev`。工作流使用 `github.repository` 确定仓库，无需在代码中填写仓库地址或个人访问令牌。

## 1. 下载发行包

在仓库 Releases 页面选择版本，下载：

- `gateway-<version>-linux-arm64.tar.gz`
- `SHA256SUMS`

带 `-dev` 等后缀的版本属于预发布。Draft Release 需要维护者发布后才对公众可见；页面自动提供的 Source code zip 是源码，不包含已构建程序。

先按 [安装准备](operator-guide.md#2-安装前准备) 准备下载与系统工具。以下 Bash 命令适用于公开仓库中的公开发行版，将 `OWNER/REPO` 和版本替换为目标仓库与版本：

```bash
set -euo pipefail
REPOSITORY=OWNER/REPO
VERSION=0.8.0-dev
ARCHIVE="gateway-${VERSION}-linux-arm64.tar.gz"
URL="https://github.com/${REPOSITORY}/releases/download/v${VERSION}"

WORK="$(mktemp -d)"
cd "$WORK"
curl --proto '=https' --tlsv1.2 -fL --retry 2 -o "$ARCHIVE" "$URL/$ARCHIVE"
curl --proto '=https' --tlsv1.2 -fL --retry 2 -o SHA256SUMS "$URL/SHA256SUMS"
sha256sum --check --strict SHA256SUMS
tar -tzf "$ARCHIVE"
mkdir release
tar -xzf "$ARCHIVE" -C release
cd release
sha256sum --check --strict SHA256SUMS
```

归档应为下表列出的平铺文件，不含绝对路径、`..` 或运行数据。使用普通用户解压，先校验再填写包内 `config.json`。SHA-256 校验用于检查完整性，不是独立数字签名，请从可信仓库获取发行包。

## 2. 首次安装与升级

**直接运行**：填写包内 `config.json` 的 `gatewayIp`、`port`、`initialKey`，执行 `sudo ./gateway-linux-arm64`。程序检查运行依赖，在 Linux + root + APT 环境补齐缺包，然后初始化并启动独立 Nginx 和后台；已安装的依赖不重复安装。用所填密钥在登录页面首次登录，已有密码不会被重置。此方式不创建 systemd 服务，也不安装或配置 EasyTier。路径规则、安装限制和排障见 [配置文件直接启动](operator-guide.md#34-配置文件直接启动)。

**systemd 安装**仍使用以下原有流程，不要求改用配置文件启动。

首次安装必须指定网关已分配的 VPN IPv4，没有内置网关地址。以下 `10.20.0.9` 只是示例，替换为自己的地址；网段不受固定 CIDR 限制。

```bash
sudo env GATEWAY_IP=10.20.0.9 \
  bash ./install.sh "$PWD/gateway-linux-arm64"
```

管理监听默认使用 `GATEWAY_IP:18080`，也可在安装时同时设置 `MANAGEMENT_LISTEN=127.0.0.1:18080` 并通过 SSH 隧道访问。

对于已有安装，使用以下流程代替首次安装：

```bash
sudo bash ./replace.sh "$PWD/gateway-linux-arm64" --dry-run
sudo bash ./replace.sh "$PWD/gateway-linux-arm64"
```

升级默认保留现有配置与数据。有关端口、证书、备份和回滚，见 [操作与部署手册](operator-guide.md)。`--replace-config` 与 `--reset-data` 会改变配置或数据，不是普通升级的必需参数。

## 3. 发行包内容

打包器只收录指定文件，不归档整个源码目录：

| 内容 | 用途 |
| --- | --- |
| `gateway-linux-arm64` | 内嵌前端的 Linux ARM64 ELF 程序 |
| `config.json` | 从源码 `config.example.json` 复制的空白启动模板，不含真实登录密钥 |
| `build-manifest.json` | 版本、源码提交、dirty 状态、构建时间、目标及文件摘要 |
| 包内 `SHA256SUMS` | 二进制、manifest、部署模板与许可文件的校验清单 |
| `install.sh`、`replace.sh`、`verify-release.py` | 首次安装、升级/回滚与本机验证 |
| `gateway.env`、两个 `gateway*.service`、两个 logrotate 模板 | 新安装的启动与日志配置 |
| `w103d-gateway` 对应模板 | 兼容已有安装布局的升级 |
| `LICENSE`、`THIRD_PARTY_NOTICES.md` | 项目许可与第三方声明 |
| `Inter-OFL.txt`、`JetBrainsMono-OFL.txt` | 字体再分发许可 |

外层 `SHA256SUMS` 校验 `.tar.gz`，包内同名文件校验解压内容，应分别在对应目录使用。填写启动配置前完成校验；升级保留现场 `config.json`，不要被新包中的空模板覆盖。发行包不包含实际业务配置、登录密钥、云凭据、证书私钥、日志或备份。

## 4. 从源码构建

准备 Node.js 22、npm、Git、`tar` 和符合 `go.mod` 的 Go 工具链，在仓库根目录执行：

```bash
node scripts/build.mjs
node scripts/package-release.mjs
```

- `build.mjs` 执行 `npm ci`、前端类型检查、Vite 构建和 Linux ARM64 Go 交叉编译，默认输出 `dist/release`。
- 构建使用 `-trimpath`，并在 manifest 中记录构建身份及文件摘要。
- `--skip-install` 适用于已按锁文件安装依赖的本地构建；其他静态检查命令见 [贡献指南](CONTRIBUTING.md)。
- `package-release.mjs` 检查文件清单、哈希、manifest 与 ELF 架构，默认输出 `dist/packages`。
- 本地可以生成带 `dirty=true` 的开发构建；公开发行要求干净、可溯源的源码提交。

输出目录已有其他版本或无关文件时，指定新目录：

```bash
node scripts/build.mjs --out-dir .local/release-build
node scripts/package-release.mjs \
  --input-dir .local/release-build \
  --out-dir .local/release-packages
```

正式发行还需校验提交与 tag：

```bash
node scripts/package-release.mjs \
  --require-clean \
  --expected-commit "$(git rev-parse HEAD)" \
  --tag "v$(cat internal/buildinfo/version.txt)"
```

## 5. GitHub Actions

| 触发方式 | 工作流行为 |
| --- | --- |
| 分支 push、Pull Request、手动运行 `Gateway checks` | 敏感信息扫描、类型检查、前端/ARM64 构建、Linux 静态检查与打包，保存 7 天 Actions artifact |
| 推送 `v*` tag | 校验 tag 与版本文件一致，执行相同检查，通过后创建 Draft Release |

工作流位于 `.github/workflows/check.yml` 和 `.github/workflows/release.yml`。只有最终发行任务获得 `contents: write`，使用 GitHub 的短期 `GITHUB_TOKEN`；构建和检查任务保持只读权限。已存在的 Release 不自动覆盖。

该工作流只构建和发行软件，不连接部署设备，也不需要 DNS/CDN 或其他生产凭据。

### 仓库设置

- 在 **Settings → Actions → General** 允许工作流使用所需 Actions，并允许发行任务申请 `contents: write`。
- 为默认分支设置保护规则与必需检查，将发行 tag 的创建权限限定给维护者。
- 在仓库支持时启用 Secret scanning / Push protection；提交检查也包含项目自带扫描器。

## 6. 发布版本

1. 更新 `internal/buildinfo/version.txt`，说明功能变化、兼容性、配置与 schema 影响，并同步相关文档。
2. 提交改动，确认分支 CI 通过；核对 [许可证](../LICENSE)、[第三方声明](../THIRD_PARTY_NOTICES.md) 及安装/升级说明。
3. 对选定提交创建并推送与版本完全一致的 tag，例如：

   ```bash
   git tag -a v0.8.0-dev -m "Gateway 0.8.0-dev"
   git push origin v0.8.0-dev
   ```

4. 等待 Actions 生成草稿 Release，补充变更说明、适用平台、已知问题及升级注意事项。
5. 检查附件、校验清单和构建身份，并在匹配平台验证安装或升级路径。
6. 点击 **Publish release** 对外发布。带 `-` 后缀的版本由工作流标记为 prerelease。

## 7. 常见构建与发行问题

| 现象 | 检查方向 |
| --- | --- |
| 下载 404 | 仓库、tag、资产名称是否正确，Release 是否已公开 |
| tag 校验失败 | tag 是否严格等于 `v` 加版本文件内容 |
| 敏感信息扫描失败 | 按脱敏路径与规则检查文件；实际密钥需移除并撤销，不通过整体排除目录规避检查 |
| `Public release requires a clean build` | 从目标干净提交重新构建，不手工修改 manifest |
| 打包提示额外文件 | 使用新的输出目录，避免混入旧版本或运行文件 |
| GitHub 权限不足 | 组织 Actions 策略、tag 权限与发行任务的 `contents: write` |
| 相同 tag 已有 Release | 检查现有发行物；需要更正内容时使用新的修订版本 |
| ARM64 程序无法在开发机执行 | 在匹配架构上运行，或为开发机从源码构建 |

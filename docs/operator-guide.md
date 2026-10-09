# 澄光网关：操作与部署手册

本手册介绍首次安装、应用发布、云平台接入和日常维护。网关 `10.20.0.9`、上游 `10.20.0.20` 均为可替换的 VPN 地址示例，不是默认值或网段限制；上游可经路由位于其他子网。示例域名使用保留的 `example.com` / `example.net`，`2001:db8::/32` 仅为文档 IPv6 地址，部署时请使用自己的地址、域名与凭据。

配套文档：

- [项目介绍与配置](../README.md)
- [GitHub Actions / Releases 发行教程](github-release.md)
- [架构与维护说明](technical-optimization.md)

快速定位：[安装与登录](#3-从-github-releases-安装) · [创建完整服务](#4-从零创建第一个完整直连服务) · [EdgeOne](#5-edgeone-边缘加速) · [Cloudflare](#6-cloudflare-for-saas-边缘加速) · [API 权限](#7-api-申请与最小权限) · [多 DNS 与地域访问](#8-多-dns国内国外访问与加速限制) · [备份恢复](#10-备份历史和恢复) · [升级下线](#11-升级回滚和下线) · [故障排查](#12-常用运维命令与故障定位)

## 1. 先判断是否适合你的环境

### 1.1 项目做什么

本项目是 **Go 管理服务 + 内嵌 Vue 页面 + 独立 Nginx 数据平面**：将 EasyTier 网络内的 HTTP/HTTPS 应用，通过网关本机的域名或 HTTP 端口提供访问，并管理相关 DNS、证书和可选 CDN 接入。

```text
管理员 ── EasyTier / 受控 SSH 隧道 ── 管理页面 :18080

访客 ── 直连选定公网地址，或 Cloudflare / EdgeOne ── 网关 Nginx
                                                    │
                                               EasyTier 网络
                                                    │
                                         http://10.20.0.20:8080
```

它不会替你安装业务应用、购买域名、部署 EasyTier、修改 NS 委派、配置路由器/防火墙、开通付费 CDN、申请源站证书或执行业务数据库迁移。

### 1.2 当前支持范围

| 项目 | 当前支持 / 限制 |
| --- | --- |
| 官方安装包 | Linux ARM64；配置文件直接启动不要求 systemd，依赖自动安装需 root + APT。可选安装脚本面向 Debian 系、APT、systemd；此包不适用于 amd64 或 Windows |
| VPN 地址 | 按部署网络选择本机 IPv4，无固定 CIDR 要求；直接启动填写 `config.json` 的 `gatewayIp`，手动安装仍可设置 `GATEWAY_IP` |
| 上游 | 路由可达、带明确端口的 HTTP/HTTPS IPv4 地址，可与网关跨子网；不能是网关自身，不接受 IPv6、域名上游、URL 密码或子路径 |
| 公网源站 | 优先本机原生全球单播 IPv6，无合格 IPv6 时回退到本机接口上的合格公网 IPv4；一次只选一个，不提供 NAT 穿透或自动双栈同时发布 |
| 域名入口 | HTTPS 443，SNI/Host 路由；HTTP 80 跳转 HTTPS；有效证书必需 |
| 端口入口 | 明文 HTTP → HTTP/HTTPS 上游；不是 TCP/UDP、SSH、数据库或 TLS 透传 |
| DNS | 腾讯云 DNSPod、阿里云 DNS、Cloudflare DNS；默认线路、启用、无权重、DNS-only 的记录 |
| 边缘 | EdgeOne 与 Cloudflare for SaaS；每个服务选择一种提供商 |
| 地域分流 | **没有自动国内/国外 GeoDNS、多 CDN 流量调度、测速排名或故障自动切换** |
| 源站证书 | 手工导入或外部 certd 推送；网关本身不是 ACME 签发器 |
| 管理登录 | 单一管理员密码及会话；管理监听绑定所选网关 IPv4 或回环地址，应仅向可信管理网络开放 |

Cloudflare/EdgeOne 可让 IPv4 访客通过边缘访问 IPv6 源站，但仍取决于套餐、区域、源站网络、证书和平台支持；仅添加 API 账号不会自动获得该能力或保证速度。

## 2. 安装前准备

### 2.1 清单

1. 一台专用于该网关或已明确规划端口的 ARM64 Linux 主机，具有本地 root/受控 sudo 权限。
2. 稳定的电源、时间同步、DNS 解析和出站 HTTPS；保留安装/升级/备份所需磁盘空间。
3. 网关与业务应用主机已通过 EasyTier 或其他 VPN 互通；本手册以网关 `10.20.0.9`、上游 `10.20.0.20` 为例。地址按自己的组网设置，跨子网时提前配置可达路由。
4. 上游应用已经启动，监听对应 EasyTier 地址或受防火墙保护的接口；网关能请求它。不要为了网关把上游管理端口直接暴露到公网。
5. 若要公网发布：网关具有可入站的合格公网地址，运营商未封端口，路由器和主机防火墙允许计划中的业务端口。本手册以原生 IPv6 为主例；IPv4 回退要求公网 IPv4 实际配置在网关本机接口上，不只是家用路由器外层显示一个公网地址。
6. 若使用域名 HTTPS：准备自有域名和覆盖服务域名的有效证书；需要自动管理解析时再准备实际权威 DNS 托管商的 API 账号。先用 HTTP 端口发布应用不需要 DNS/CDN 账号。证书管理器若用 DNS-01，还需要它自己的受限 DNS 授权。

EasyTier 安装与组网以 [EasyTier 官方文档](https://easytier.rs/) 为准。本项目只消费已经可用的网络，不替你选择网络密钥、对等节点或开放端口。

干净的 Debian 镜像可能尚无下载或解压工具。在主机上下载发行包时，先准备所需工具（有 root 会话时可省略 `sudo`）；也可以在另一台电脑下载解压后，将二进制与 `config.json` 传到主机：

```bash
sudo apt-get update
sudo apt-get install -y --no-install-recommends ca-certificates curl tar
```

选择 [配置文件直接启动](#34-配置文件直接启动) 时，其余运行依赖由程序检查补齐，无需手工配置 Nginx，也无需安装 Go、Node.js 或数据库。仅选择旧 `install.sh` 系统服务安装流程时，还需预先准备以下工具：

```bash
sudo apt-get install -y --no-install-recommends tar iproute2 procps python3 logrotate
```

此处不要提前启动默认 Nginx；直接启动和首次安装器都会在防止默认服务自动启动的条件下处理 Nginx/njs，不会接管其他 Nginx 站点。

在网关上先只读检查：

```bash
uname -m
systemctl --version
ip -4 -o address show
ip -6 address show scope global
ip -6 route
ss -lntp
curl --noproxy '*' --connect-timeout 5 --max-time 10 \
  http://10.20.0.20:8080/
```

`uname -m` 应为 `aarch64`；上游收到 401/403 也可能只是应用要求登录，应按业务实际状态判断。HTTPS 上游会按系统 CA 和 **上游 IP** 校验，证书缺少 IP SAN 或为未信任自签名时不会自动忽略。可配置受信任 CA，或在受控加密 EasyTier 网络上使用符合安全要求的 HTTP 上游；不要把跳过 TLS 校验当成上线方案。

### 2.2 端口与网络边界

| 端口 | 用途 | 推荐可达范围 |
| --- | --- | --- |
| TCP 18080（可配置） | 管理页面/API | 仅 EasyTier 管理网络，或只监听回环后走 SSH 隧道；直接启动的端口由 `config.json` 设置 |
| TCP 18079 | Nginx 代际验证 | 固定 `127.0.0.1`，不要转发到公网 |
| TCP 443 | 域名业务 HTTPS / CDN 回源 | 经授权的访客或所选 CDN 回源；由部署者配置防火墙 |
| TCP 80 | 域名 HTTP → HTTPS 跳转 | 需要裸域名 HTTP 访问时放行；它不是管理页面 |
| 自选业务端口 | 端口入口的 HTTP 服务 | 只开放实际需要的端口；不能使用 80/443/18079/18080 |
| Unix `control.sock` | 本地证书激活 | 仅有最小本机授权的账户，不是 TCP 端口 |

安装脚本**不会修改防火墙**。直连 IPv6 不需要传统 IPv4 端口转发，但仍受 IPv6 入站防火墙约束。私有/链路本地 IPv6、文档地址、仅可出站的地址不是可用公网源站。未探测到公网地址时，网关可以只应用内网入口，不能据此宣称公网已上线。

公网 IPv4 候选排除私有、保留和运营商级 NAT 地址；本机没有可用公网 IP 时，不会通过外部“查 IP”服务发现并发布路由器地址。当前一次仅选择 IPv6 或 IPv4，不等于同时双栈；也没有 WAN 网卡专属选择策略。地址格式合格不证明来自互联网的访问一定可达。

选择“边缘加速”只改变域名与云接入，不自动给源站加上 CDN 来源 ACL。知道源站地址和正确域名的客户端，在防火墙允许时仍可能绕过 CDN 直接访问。需要防绕过或更细 EasyTier 节点 ACL 时，由管理员另外设计网络与应用权限，不能把本项目当成主机防火墙。

出站按使用功能放行：软件包镜像与 GitHub；腾讯云 DNSPod/EdgeOne/SSL API；Cloudflare API；阿里云 DNS API；所用证书管理器；外部探测及通知接收端。未使用的云功能不需要准备对应账号。

## 3. 从 GitHub Releases 安装

推荐下载校验后按 [第 3.4 节](#34-配置文件直接启动) 填写三项配置并直接启动；第 3.2 节保留给需要 systemd 常驻与开机自启的部署。

### 3.1 下载与校验

在本仓库的 Releases 页面下载 **Release Assets 中的发行包**，不是自动生成的 Source code zip。发行目标为 `linux-arm64`，包含前端、Go 程序、部署脚本、模板、构建清单和 SHA-256 清单。源码构建和版本发布见 [构建与发行](github-release.md)。

以下在 Linux 上执行，先将 `REPO` 和 `VERSION` 改为维护者公开的仓库和版本；预发布需要显式选择，草稿 Release 尚未公开时下载会失败。

```bash
set -euo pipefail
REPO=OWNER/REPO
VERSION=0.8.0-dev
TAG="v${VERSION}"
ASSET="gateway-${VERSION}-linux-arm64.tar.gz"
BASE="https://github.com/${REPO}/releases/download/${TAG}"

WORK="$(mktemp -d)"
cd "$WORK"
curl --proto '=https' --tlsv1.2 -fL --retry 2 -o "$ASSET" "$BASE/$ASSET"
curl --proto '=https' --tlsv1.2 -fL --retry 2 -o SHA256SUMS "$BASE/SHA256SUMS"
sha256sum --check --strict SHA256SUMS
tar -tvzf "$ASSET"
```

先检查归档只包含 [发行包清单](github-release.md#3-发行包内容) 内的平铺普通文件，不含绝对路径、`..`、链接或运行数据。确认后使用普通用户解压：

```bash
mkdir release
tar -xzf "$ASSET" -C release
cd release
sha256sum --check --strict SHA256SUMS
cat build-manifest.json
chmod 0755 gateway-linux-arm64
./gateway-linux-arm64 version --json
```

校验失败立即停止，不继续安装。归档应为平铺白名单文件，不能出现现场 `state.json`、凭据、私钥、日志或用户备份。SHA-256 能发现内容不一致，但不替代对仓库维护者、Release 和工作流来源的信任核查；若整个下载来源遭篡改，同来源校验文件本身也可能被替换。

### 3.2 手动下载校验与安装

在已校验、解压后的发行目录中操作。手动首次安装必须通过 `GATEWAY_IP` 指定网关的 VPN IPv4，没有内置默认地址。该地址须已经配置在网关本机接口上；这里的 `10.20.0.9` 仅作示例。安装前检查包内 `install.sh`、`gateway.env` 和两个 unit，再执行：

```bash
sudo env GATEWAY_IP=10.20.0.9 \
  bash ./install.sh "$PWD/gateway-linux-arm64"
```

省略 `MANAGEMENT_LISTEN` 时，安装器使用 `GATEWAY_IP:18080`。也可指定 `MANAGEMENT_LISTEN=127.0.0.1:18080`，通过第 3.3 节的 SSH 隧道方案管理；不支持 `0.0.0.0` 或 `[::]` 通配监听。安装器将选择写入 `/etc/default/gateway`，初始渲染和验证使用同一配置。

网关和上游地址不受固定 VPN 网段约束，也不要求同一子网。例如使用其他私网或 VPN 分配地址时，只需替换示例并保证路由可达。已有安装保留其原地址和数据；改变启动参数不会自动改写已有状态中的网关身份，迁移网络时应先备份并规划配置与路由调整。

安装器会检查架构、systemd、依赖、既有安装及端口占用；必要时安装 Nginx、匹配的 `libnginx-mod-http-js`、logrotate、Python 3、curl。已有 Nginx 时仍须确认 `/usr/lib/nginx/modules/ngx_http_js_module.so` 与 Nginx 匹配；不会以卸载/停止已有站点来强行接管机器。缺少 `ip`/`ss`/`pgrep` 等基础工具时，先安装 `iproute2`/`procps` 后重新检查。

若已有 `gateway` 或 `w103d-gateway` 的安装痕迹，**不要删数据重跑首次安装**，使用升级流程或先人工核对半完成状态。安装器创建 root 私有数据目录、受管配置、日志轮转与两个 unit，并验证二进制、状态、健康及 Nginx 运行代际。

安装成功只表示管理服务和空的受管代理可运行：不会凭空创建业务服务、上传证书或写入 DNS/CDN。

### 3.3 登录与首次安全设置

1. 选择 VPN 管理时，通过 EasyTier 等 VPN 打开安装器输出的地址，例如 `http://10.20.0.9:18080/`。此 HTTP 管理连接必须位于受控的加密私网，不能经公网明文传输。选择回环管理时先建立下文的 SSH 隧道。
2. [配置文件直接启动](#34-配置文件直接启动) 使用所填 `initialKey` 首次登录；未设置该密钥的手动安装，在网关的受控本地终端私下读取初始密码：

   ```bash
   sudo cat /var/lib/gateway/initial-password
   ```

   不要把输出复制到 Issue、聊天、截图、CI 日志或共享终端记录。
3. 登录后进入 **系统管理 → 安全与会话**，用当前密码设置独立强密码。新密码为 12–128 个字符、最多 512 个 UTF-8 字节。
4. 改密成功后重新登录。程序使用带盐 Argon2id 保存新密码并清理初始明文文件；旧会话失效。
5. 在 **系统管理 → 配置资料** 设置公开可见的项目名、客户端名和 Logo。登录页会读取这些展示资料，不要在名称里写私密标识。

会话固定有效期为 12 小时，管理进程重启后失效；“安全与会话”可撤销单个、其他或全部会话。管理密码与 GitHub/云账号/业务应用密码应彼此独立。

若计划让不在 EasyTier 的客户端通过 SSH 管理，在**首次手动安装时选择回环监听方案**，与第 3.2 节的默认安装二选一；已经安装成功后不要重复执行：

```bash
sudo env GATEWAY_IP=10.20.0.9 MANAGEMENT_LISTEN=127.0.0.1:18080 \
  bash ./install.sh "$PWD/gateway-linux-arm64"
```

随后在管理员客户端建立仅本机可达的 SSH 转发（将 `gateway.example.com` 替换为可通过 SSH 连接的网关主机）：

```bash
ssh -N -L 127.0.0.1:18080:127.0.0.1:18080 admin@gateway.example.com
```

访问客户端 `http://127.0.0.1:18080/`，不要改为 `localhost` 或另一个本地端口。服务器严格要求请求 `Host` 等于其 `MANAGEMENT_LISTEN`，写请求还执行同源校验；**仅将本地 127.0.0.1 转发到远端的 10.20.0.9:18080 会因 Host 不一致被拒绝**。不要关闭 Host/CSRF 检查、增加任意可信 Host 或开放公网管理端口来解决这个问题。已有部署若需改变管理监听，应单独审查实际 env/unit 并安排重启，不能通过重装清空数据。

### 3.4 配置文件直接启动

不安装系统服务、直接运行程序时，在二进制同目录的 `config.json` 中填写三项即可。发行包附带空白模板；源码开发可将仓库的 `config.example.json` 复制为根目录 `config.json`，再填写自己的值。

| 字段 | 含义 |
| --- | --- |
| `gatewayIp` | 网关主机实际使用的 EasyTier/VPN IPv4，不是上游应用地址；不限定网段 |
| `port` | 管理后台端口，整数 `1–65535`，不能使用代理保留的 `80`、`443`、`18079`，并应避开其他已占用端口 |
| `initialKey` | 首次登录的管理密钥，12–128 个字符、最多 512 个 UTF-8 字节；使用独立强密钥 |

先完成发行包校验，再填写配置，并限制其读取权限：

```bash
chmod 0755 gateway-linux-arm64
chmod 0600 config.json
sudo ./gateway-linux-arm64
```

程序会先检查下列依赖；缺少时使用 APT 补齐，安装完成后再次验证，通过后才启动代理和后台。已有依赖不重复安装。

| 依赖 | 用途 |
| --- | --- |
| `nginx`、`libnginx-mod-http-js`、`www-data` 账户 | 业务代理与 njs 模块；同时验证模块能被当前 Nginx 加载 |
| `ca-certificates`、`iproute2` | 系统 HTTPS 信任、IP/路由与监听检测（`ip`、`ss`） |
| `procps`、`iputils-ping`、`logrotate`、`python3`、`curl`、`tar` | 进程/连通诊断、日志轮转及部署维护工具 |

自动安装仅支持 **Linux + root + APT**，缺包时需要正常的 DNS、网络和软件源。其他发行版会列出缺少的依赖后退出；请用其系统包管理器准备依赖。若需安装 Nginx/njs，而机器已有 Nginx 进程，程序会停止并说明原因，不通过停止其他站点强行安装。安装期间抑制系统默认 Nginx 自启，不覆盖已有启动策略；无法安全处理时会明确报错。

依赖就绪后，程序创建所需目录，生成本项目的 `managed-nginx.conf`，验证并启动独立代理，再启动管理服务。首次只生成空业务配置；重启使用已应用快照，不自动发布未应用的草稿。EasyTier 组网、实际分配的 `gatewayIp` 和业务所需网络、证书仍由部署者准备。

终端显示启动成功后，打开 `http://<gatewayIp>:<port>/`，在登录页面使用 `initialKey` 登录。它只在首次创建认证时使用：**已有密码哈希不会因更改配置而被重置**。在“系统管理 → 安全与会话”修改密码后，可将 `initialKey` 清空为 `""`；此后使用新密码登录。实际 `config.json` 含秘密，不提交 Git、不附在公开报障中；仓库只提供无实际密钥的示例文件。

配置查找顺序为：显式 `--config PATH` → 二进制同目录 `config.json` → 当前工作目录 `config.json`（前一默认位置不存在时，便于源码开发）。高级用法中，显式提供 `--gateway-ip` 且未指定 `--config` 时仍走原 CLI 参数逻辑；指定 `--config` 则强制读取文件，并允许显式 CLI 参数覆盖相应值。

此方式沿用 `/var/lib/gateway` 数据目录与 `/var/cache/gateway` 缓存目录，不创建二进制旁的数据目录或 systemd 服务。直接运行需要保持进程运行，正常退出会停止本次启动的受管代理；对原有匹配的受管实例仅验证，不在退出时停止它。已有 systemd/env 安装继续使用其原参数与升级方式，不执行配置文件模式的自动准备流程；开发 `--dry-run` 同样跳过此流程。

启动失败时先看终端中的具体缺项或错误，再处理对应条件并重启：

| 现象 | 处理方向 |
| --- | --- |
| 权限不足 | 使用 `sudo` 或 root 运行，并检查数据目录写入权限 |
| APT 安装失败 / 找不到软件包 | 检查 DNS、出站网络、软件源、包管理器锁及发行版是否提供匹配的 njs 包；不要删除锁文件强行重试 |
| 缺依赖但没有 APT | 使用本系统包管理器按缺项准备依赖，或使用受支持的 Debian 系环境 |
| VPN 地址无法监听 | 先确认 EasyTier 已组网，`gatewayIp` 确实分配在本机 |
| 端口占用 / Nginx 冲突 | 核对占用进程及所属服务；管理端口可修改 `port`，不要杀掉其他站点强行启动 |

填写 `config.json` 后，它与发行包空模板的摘要自然不同；不要将现场配置替换为空模板来通过校验。升级直接运行的程序前先停进程、备份配置和原数据，保留实际配置文件。

## 4. 从零创建第一个完整直连服务

建议先验证直连闭环，再加 CDN，避免同时排查多个提供商。

### 4.1 确定发布方案

本文示例：

| 项目 | 示例 |
| --- | --- |
| 上游应用 | `http://10.20.0.20:8080` |
| 业务域名 | `app.example.com` |
| 独立基站域名 | `origin.example.com` |
| 对外协议 | `https://app.example.com` |
| 源站证书 | 覆盖 `app.example.com`，或合适的 `*.example.com` |

基站域名是公网地址的固定别名，**不是管理页面地址，也不是另一个业务服务**。不要把 `origin.example.com` 同时绑定到服务。基站可选，但有多个域名或准备使用 Cloudflare 时推荐配置。

首次建议使用 `app.example.com` 这类子域名。当前 DNS 写入模型拒绝托管区根域名的 CNAME；把根域名直接用于基站间接层或 CDN CNAME，并不会自动获得 ALIAS/ANAME/根域扁平化支持。根域直连 A/AAAA 与子域 CNAME 是不同方案，先按预览核对。

### 4.2 添加实际 DNS 托管账号

1. 先在注册商处核对当前 NS。在哪里购买域名，不一定就在哪里管理解析。
2. 在 **域名解析 → 管理解析账号** 添加 DNSPod、阿里云 DNS 或 Cloudflare DNS 凭据；申请步骤见第 7 节。
3. 保存时程序只读验证可见托管域；成功不等于已具备全部记录写入权限，也不证明该托管区已经是公网权威 DNS。
4. 系统按域名的最长托管区后缀匹配账号；多个账号匹配同一区域时，在记录旁 **选择解析账号**。选择仅改本地归属，不切 NS、不复制或迁移 DNS。

云密钥只在管理界面提交，不放到 GitHub、shell 参数、Docker 镜像或公开 JSON 示例中。为网关分配专用最小权限账号，而不是日常主账号管理密钥。

### 4.3 配置基站与自动解析

在 **网关概览 → 基站域名** 保存 `origin.example.com` 和 TTL。首次建议 600 秒；60/300 秒是否可用取决于 DNS 套餐。保存设置不代表 DNS 已发布。

直连 DNS 结构：

```text
有基站：app.example.com  CNAME  origin.example.com
        origin.example.com  AAAA  当前已应用的公网 IPv6

无基站：app.example.com  AAAA  当前已应用的公网 IPv6
```

上图是 IPv6 场景。若本机没有合格 IPv6、实际选择公网 IPv4，基站或无基站直连记录会改用 A；有基站时业务域仍是 CNAME。不要手工把 IPv4 写进 AAAA，或误以为程序会同时维护两族公网监听。

自动解析默认启用，维护已应用、已授权记录；新地址先经本机应用验证，再更新对应 DNS，避免把 DNS 指向尚未监听的地址。正常本地地址检查约每分钟一次，云端定期核对约 30 分钟，错误退避约 5 分钟。这些不是故障恢复 SLA，且不保证所有递归 DNS 缓存马上更新。它不会将边缘业务 CNAME 擅自“修复”为源站 AAAA。

### 4.4 准备源站证书

进入 **证书管理 → 上传证书**，选择：

- **上传文件**：分别上传 `fullchain.pem` 与 `privkey.pem`。
- **粘贴证书**：粘贴 PEM 证书链和 PEM 私钥；不适合共享屏幕时操作。
- **通过 certd 部署**：先创建证书槽，复制页面显示的收件目录与应用命令。

证书必须与私钥匹配、在有效期内、覆盖服务的每个域名。`*.example.com` 不覆盖 `example.com` 或 `a.b.example.com`。服务填写具体域名，不能把通配符证书误当成通配符路由。每份上传材料上限 2 MiB。

用于浏览器直连的证书应具有受客户端信任的完整链；“导入成功”不等于所有客户端信任该 CA。Cloudflare Origin CA 等专用源站证书不能当成通用浏览器可信证书使用，切换直连或其他 CDN 前应重新核对信任链。

若采用 certd：

1. 在你自己的 certd 中创建证书任务，申请服务域名。DNS-01/ACME 的账号、CA、续期策略和 API 授权在 certd 中配置，网关不替你签发。
2. 用受信任 SSH/主机部署任务将两份 PEM 放到页面显示的 `/var/lib/gateway/cert-inbox/SLOT_ID/`。目录 `0700`、文件 `0600`，用临时文件完成上传后再替换，避免只上传半份就激活。
3. **两份文件都已落盘后**执行：

   ```bash
   sudo /opt/gateway/gateway certificate-apply SLOT_ID \
     --data-dir /var/lib/gateway
   ```

4. 管理服务必须正在运行；命令走本地 `control.sock`，需要 root 或只允许固定目录和固定激活命令的最小授权。不要给证书任务开放任意公网 root 执行入口。
5. 回到证书页核对指纹、覆盖域和有效期。更新已被运行服务使用的槽位会执行受控本机切换；EdgeOne 同步结果单独核对。不能只凭 certd 的“文件上传成功”判断更新已生效。

`CERTD_URL` 是可选诊断地址，SSH 推送和 `certificate-apply` 不要求设置它，也不需要将 certd 管理密码嵌进 URL。官方参考：[certd 项目](https://github.com/certd/certd)、[主机部署插件](https://github.com/certd/certd/blob/v2/packages/ui/certd-server/src/plugins/plugin-host/plugin/upload-to-host/index.ts)。

### 4.5 创建服务并发布

1. 进入 **服务管理 → 新建服务**，填写名称、上游 `http://10.20.0.20:8080`，选择 **用域名发布 → 本机直连**。
2. 填写 `app.example.com`，选择已就绪的证书槽，保持服务启用。同一域名不能出现在两个服务中。
3. 如应用的正常健康页面不在 `/`，通过支持该字段的接口设置同源 `healthCheck`；当前服务表单未必提供全部高级 API 字段，不要在线直接改 JSON。
4. **保存**只保存草稿；**保存并发布**仍会打开发布范围确认。核对名称、上游、证书、公网地址、本机其他待应用变更和 DNS 目标。
5. 确认本次允许的本机及 DNS 操作。若提示冲突旧记录，不要为了通过流程直接点强制覆盖；先在 DNS 平台确认记录归属并保存原值。
6. 查看发布步骤，区分“本机配置应用成功”“DNS 云端读回一致”和“外部业务可访问”。任一层失败，不等于前面的操作自动撤销。

本机 Nginx 应用以完整草稿为范围，预览可能提示其他服务的未应用改动；不要只看到一个服务名就忽略其他受影响入口。没有 DNS 凭据时可单独应用本机代理，再由受权管理员按预览目标手工配置 DNS，但这不是网关自动维护 DNS 的完整闭环。

### 4.6 检查访问链路

按上游、本机入口、DNS、公网的顺序检查，便于定位故障所在层：

1. **上游**：服务菜单 → 本机与上游探测，确认网关到 EasyTier 应用可达。
2. **本机代理**：服务本机入口检查。域名服务必须带正确的 Host/SNI；直接访问裸 IP 的 443 被拒绝是设计行为。
3. **DNS**：解析页核对云端记录；再从客户端查询 CNAME/AAAA（IPv4 场景查询 A），必要时查询权威服务器。托管商读回正确和所有递归缓存更新是两件事。
4. **公网**：从网关外的 IPv6 网络访问直连域名。IPv4-only 客户端无法直达只发布 AAAA 的源站是预期限制。
5. **真实业务**：验证登录、一个 API/页面，以及实际需要的 WebSocket、上传或下载；不要只访问默认首页判断应用全部正常。

可用命令（按自己的网关 IP 与域名替换）：

```bash
# 绕过外部 DNS，验证 EasyTier 上的域名 HTTPS 入口；不跳过 TLS 校验。
curl --noproxy '*' --resolve app.example.com:443:10.20.0.9 \
  --connect-timeout 5 --max-time 15 https://app.example.com/

# 若未安装 dig，可使用 DNS 平台控制台或其他受信任解析工具。
dig +short CNAME app.example.com
dig +short AAAA origin.example.com

# 在另一条具备 IPv6 的外部网络执行。
curl -6 --connect-timeout 5 --max-time 15 https://app.example.com/
```

不要在公开工单上传带登录 Cookie、Authorization、查询串 Token 的 `curl -v` / HAR。HTTPS 证书链应正常验证，不将 `-k` 的结果当作上线验收。

### 4.7 只需要端口入口时

选择 **用端口发布**，不填域名和证书，可用上游同端口或另选未占用端口。保存并应用后，以 `http://10.20.0.9:端口/` 或 `http://[网关公网IPv6]:端口/` 访问；实际回退为公网 IPv4 时使用 `http://公网IPv4:端口/`，不加 IPv6 方括号。

端口入口不经过 Cloudflare/EdgeOne、不自动拥有 TLS，不能当成 TCP 透明代理。涉及密码或敏感业务时，优先使用域名 HTTPS 入口；如必须保留 HTTP 端口，将可达范围限制在受控加密网络。

## 5. EdgeOne 边缘加速

### 5.1 平台准备

先在腾讯云 EdgeOne 控制台添加你有权使用的站点、完成所有权/接入验证、选择服务区域和套餐。站点创建、开通计费、备案/资质、NS 变更不是本程序自动执行的步骤；涉及中国大陆区域的域名条件以腾讯云当前要求为准。

源站要先按第 4 节具备可用的域名 HTTPS 和公网入口，本节以 IPv6 为例；回退到 IPv4 时源站组使用实际已应用地址。不要把“IPv6 访客加速已开启”与“平台能通过 IPv6 回源到当前服务器”混为一谈。

### 5.2 添加账号与选回源方式

在 **边缘加速 → 管理账号** 添加 EdgeOne 专用 SecretId/SecretKey，保存时读取站点。EdgeOne 凭据与 DNSPod 凭据是两个独立配置，即使来自同一腾讯云账号也不应假定权限相同。

选择：

| 回源方式 | 优点 | 边界 |
| --- | --- | --- |
| 公网地址（默认） | 源站组使用当前已应用公网地址，地址变化时由网关明确更新 | 网关离线或云 API 失败时，EdgeOne 可能仍指向旧地址 |
| 基站域名 | 源站组固定引用 `origin.example.com`，地址变化通过基站 A/AAAA 体现 | 要先发布基站；EdgeOne 的 DNS 缓存/重新解析时延不能由网关保证 |

不要手工选择同名但不属于本网关的源站组。网关只按已记录资源 ID 管理自己创建的组；共享/被其他工具改动的资源会阻止进一步写入。

### 5.3 发布步骤

1. 编辑服务，选择 **用域名发布 → 边缘加速 → EdgeOne**，绑定覆盖业务域名的源站证书。
2. 保存并预览发布，核对匹配的 EdgeOne 站点和账号、源站地址、证书上传及域名变动。
3. 确认后，按计划创建/更新源站组、加速域名、上传证书到腾讯云 SSL、绑定证书；系统读取提供商分配的 CNAME，再维护业务解析。
4. 核对 EdgeOne 站点、加速域名状态、证书绑定和业务 CNAME。加速域名的 CNAME 应使用云端实际返回值，不按字符串规则手工拼接。
5. 在 EdgeOne 控制台检查 **HTTPS 443 回源、业务 Host/SNI、IPv6 回源及域名级规则**。当前客户端请求主要维护源站组引用和 IPv6Status，不能把接入成功提示理解为已经校验所有平台回源协议设置；已有平台规则也可能影响回源。
6. 从外部 IPv4 和实际目标地区验证 HTTPS、登录、WebSocket/下载，确认不是仅 CDN 默认页面可达。

**站点级影响**：若 WebSocket 未开，预览可能包含对整个站点开启 WebSocket，并设置 120 秒空闲超时；这会影响站点内不归本程序管理的域名。已开启但超时较短时只告警，不擅自调大。长连接应有符合应用需要的心跳；Nginx 的更长超时不能消除边缘平台限制。

**私钥流向**：EdgeOne 集成会把本机证书及私钥上传到所选腾讯云账号的 SSL 产品后绑定边缘。确认你的证书保管政策允许这一点；不要误以为只上传公钥。

网关当前腾讯云 SSL 上传客户端另有限制：证书链与私钥总量不得超过 48 KiB。大材料可能本机导入成功而云端上传失败，不能只看本机“证书可用”。

## 6. Cloudflare for SaaS 边缘加速

### 6.1 区分三个角色

- **业务域名与权威 DNS**：例如 `app.example.com`，可以继续由 DNSPod 或阿里云托管。
- **源站基站**：例如 `origin.example.com`，AAAA 指向网关已经应用的公网 IPv6，必须为 DNS-only。
- **SaaS 接入区域**：例如你另一个域名 `example.net`，必须是已经完整接入 Cloudflare、`active` 的 **Full zone**，并已开通 Cloudflare for SaaS。当前程序不接受 Partial zone。

仅添加业务 DNS 的 Cloudflare Token，不等于已经创建 SaaS 接入账号。Cloudflare 边缘证书也不能代替本机源站证书。

下文继续使用 IPv6 源站示例；若网关实际已应用公网 IPv4，基站对应 A 而不是 AAAA。Cloudflare 接入逻辑不因名称叫“基站”就自动证明回源链路可用，仍需实际验收。

### 6.2 首次接入

1. 在 Cloudflare 控制台完成接入区的 NS、Zone 激活和 SaaS 开通，确认账单、Custom Hostnames 配额及所需功能。程序不自动购买或开通产品。
2. 接入区设置 **Full (strict)**。如在网关账号中勾选允许自动设为严格模式，实际发布才执行并读回；它影响整个接入区的其他代理站点，不只是当前服务。
3. 添加业务 DNS 账号，并按第 4 节设置、发布基站 AAAA 和本机业务 HTTPS。
4. 在 **边缘加速 → 管理账号 → 添加账号** 选择 Cloudflare，填写受限 API Token，点击 **读取并自动配置**。多个可用区域时明确选择本次专用接入区。
5. 核对自动建议的 Zone ID、Fallback 域名与标准入口，例如 `fallback.example.net`、`entry.example.net`；它们是区内两个不同的专用子域名，不能使用接入区根域名。
6. 只保存账号不会创建云资源。共享 Fallback 已存在且不符合当前源站时，先核对该区全部用户；如确需替换，到 **预览接入** 显式确认共享 Fallback 替换，普通服务发布不会替你接管。
7. 编辑服务，选择 **边缘加速 → Cloudflare**、接入账号和有效的源站证书。首次先用标准入口，不同时引入第三方优选。
8. **保存并发布**并确认范围。程序处理本机、基站、SaaS 资源及业务 DNS 验证；等待资源就绪后写入业务 CNAME，并从实际托管商读回核对。

### 6.3 正确的 DNS 分层

| 角色 | 示例记录 | Cloudflare DNS 代理模式 |
| --- | --- | --- |
| 基站 | `origin.example.com AAAA 当前公网IPv6` | 若托管在 Cloudflare，必须 DNS-only |
| SaaS Fallback | `fallback.example.net CNAME origin.example.com` | Proxied / 橙云 |
| 标准接入 | `entry.example.net CNAME fallback.example.net` | Proxied / 橙云 |
| 业务入口 | `app.example.com CNAME entry.example.net` | 若业务 DNS 在 Cloudflare，保持 DNS-only / 灰云 |

业务记录为灰云不代表绕过 SaaS CDN；加速由 SaaS 接入区承担。**第三方优选只能替代业务 CNAME 目标，不能写进基站 AAAA 或 Fallback 源站。** 保持源站接收业务域名的 Host/SNI，不把业务上游改成 CDN 域名形成循环。

### 6.4 等待证书不是立即失败

Cloudflare 的 Custom Hostname、边缘 SSL 证书和 Fallback 状态是三项独立条件。正常 pending 会进入发布任务等待，30 秒至 5 分钟退避检查，最长 24 小时；可关闭页面，后台与重启恢复按原授权范围继续。

- 看到“等待”时先查看具体阶段，不重复创建账号/主机名、不反复删除 TXT。
- 检查权威 DNS 上 TXT 是否可见、CAA 是否允许所用 CA、域名是否被其他 SaaS 接入占用、区域是否 active，以及 Token 的权限与配额。
- 网关补充本次验证所需 TXT，不覆盖其他 ACME TXT，不自动修改 CAA。DCV 复验受理不等于证书已经签发。
- 403、明确冲突或写入结果未知不会因为等待时间到了就被自动忽略。未知写入保留账本并阻止盲目重试，先只读核对真实云端资源。
- 当前自动续发只用于匹配原服务配置的可恢复等待；修改域名/提供商后应重新预览，旧未知操作不会自动转成新操作授权。

### 6.5 可选的 HTTP DCV / 委派

非通配符服务长时间等待时，页面可能允许 **尝试自动 HTTP 验证**。该操作会在证书就绪前先创建指向标准 SaaS 入口的业务 CNAME，**HTTPS 可能暂时不可用**。它要求已有合格等待任务、源站 HTTPS、严格模式和相关云资源就绪，并且业务名称没有现存解析或托管记录；不覆盖正在服务的旧站、不走第三方优选。

正常发布不会自动添加 Delegated DCV CNAME。如显式选择 DCV 委派，必须确认它不会破坏 certd/其他 ACME 客户端对 `_acme-challenge` 的使用。一个名称下的 CNAME 与 TXT 不能随意混用。仅当所选区和套餐支持相应能力时操作，不把委派当成跳过域名验证的办法。

### 6.6 第三方优选入口

在服务的 Cloudflare 选项中启用优选并填写你信任的第三方**完整域名**，再预览发布。网关检查 DNS A/CNAME 链、循环与公网 IPv4 条件；它不扫描 IP、不测速、不排名，也不保证该入口与你的 SaaS 区兼容或在中国大陆更快。

界面“配置的优选”表示保存了目标；只有云端 CNAME 真实读回一致，才能说明解析已更新。**发布完成不以新入口实际 HTTPS 检测通过为条件，也不是性能验收。** 发布后独立做外部检查和代表性业务访问。优选不可用时关闭开关并再次发布，可恢复保存的标准入口；不要修改源站基站来回退访客线路。

应区分三种检查，当前实现并没有取消必要的云端或访问保护：

1. **切换前配置/资源门槛**：域名格式、DoH A/CNAME 链、防环、公网 IPv4、Cloudflare 主机名/证书/Fallback 就绪，以及现有 DNS 归属、冲突和未知写入保护。
2. **发布完成证据**：业务 CNAME 写入后，从真实托管商读回并与所选目标一致；不把独立访问检测的结果作为写入条件或自动回滚依据。
3. **独立访问检查**：在 **边缘加速 → 更多操作 → 独立访问检查**，对已应用且有已发布 CNAME 记录的业务域名执行。实际拨号固定为已检查的公网 IPv4，严格校验 TLS、业务 Host/SNI 和新的 nonce/HMAC 源站证明；不跟随跳转、不使用环境代理、不扫描或测速。当前最多对解析中的两个不同地址做有限检查，不代表全部线路可达。失败不自动切 DNS。

独立访问检查可识别“边缘在回答但不是当前源站”的问题，但只反映本机当时解析所见的入口，不代表全球 DNS 缓存已经更新，也不属于自动发布步骤。

优选 DNS 只读核验依次使用固定的 Cloudflare DoH、DNSPod DoH、Google DNS 备用接口，发送公开域名而不是云 Token；网络全部不可达时会拒绝切换并保留旧 DNS。

## 7. API 申请与最小权限

下列动作来自当前客户端的实际调用。平台控制台的权限名称、资源级授权支持和套餐条件可能变化，应结合官方文档和账号实际可见资源核对，**不要为绕过一条错误直接授予 Administrator/FullAccess**。

通用做法：创建网关专用子用户/Token → 限定目标域名/Zone/站点 → 如有稳定出口，增加合适的来源 IP 条件 → 在管理界面保存 → 先只读预览 → 用专用测试域名验证必要写操作 → 再发布业务。密钥只在创建时展示的平台，应立即存入密码管理器。

### 7.1 腾讯云 DNSPod

申请入口：[腾讯云 CAM](https://console.cloud.tencent.com/cam)、[API 密钥管理](https://console.cloud.tencent.com/cam/capi)、[DNSPod API 文档](https://cloud.tencent.com/document/api/1427)。

1. 确认域名在该账号的 DNSPod 中可见，且公网 NS 指向实际托管区。
2. 创建仅供网关使用的 CAM 子用户和 API 密钥，选择程序访问；不要填写旧版 DNSPod 的 `ID,Token`，当前接口使用腾讯云 API 3.0 的 **SecretId / SecretKey**。
3. 建立自定义策略，按本次域名和平台支持的资源范围授权：

| 必要动作 | 用途 |
| --- | --- |
| `DescribeDomainList` | 列出可见托管域，添加账号与新域名归属匹配 |
| `DescribeRecordList` | 预览冲突、核对现有记录和写后读回 |
| `CreateRecord` | 新增 AAAA/CNAME/TXT 等受支持记录 |
| `ModifyRecord` | 更新本程序托管记录 |
| `DeleteRecord` | 经确认撤下、替换或清理托管记录 |

IAM 动作属于 `dnspod` 服务。列举动作是否只能使用 `*`、写动作支持哪些域名资源表达式，应以 CAM 当前说明为准；需要 `*` 的列表权限不意味着必须对所有 DNS 写操作授权。若省略删除权限，首次新增可能成功，但撤下/冲突处理会失败，不要到上线后才发现。

### 7.2 阿里云 DNS（AliDNS）

申请入口：[RAM 控制台](https://ram.console.aliyun.com/)、[云解析 DNS](https://dns.console.aliyun.com/)、[AliDNS API 文档](https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-overview)。

1. 创建专用 RAM 用户，开通所需 OpenAPI 访问，创建 **AccessKeyId / AccessKeySecret**；不要使用阿里云主账号 AccessKey。
2. 在网关选择“阿里云 DNS”。表单中的 SecretId 对应 AccessKeyId，SecretKey 对应 AccessKeySecret。
3. 按 `alidns` API 动作建立自定义权限：

| 必要动作 | 用途 |
| --- | --- |
| `DescribeDomains` | 域名列表与账号校验 |
| `DescribeDomainRecords` | 目标名称的完整记录/冲突查询 |
| `DescribeDomainRecordInfo` | 按 RecordId 核验更新/删除目标归属 |
| `AddDomainRecord` | 新记录 |
| `UpdateDomainRecord` | 更新托管记录 |
| `DeleteDomainRecord` | 撤下或清理托管记录 |

按官方 RAM 资源授权支持范围限定域名；仅对必须全局列举的动作保留必要范围。当前只支持静态 AccessKey 配置，没有 RAM 角色/STS SessionToken 自动刷新入口。密钥到期或轮换后需要在网关账号配置中更新。

### 7.3 Cloudflare DNS-only 账号

申请入口：[用户 API Tokens](https://dash.cloudflare.com/profile/api-tokens)、[创建 Token 官方说明](https://developers.cloudflare.com/fundamentals/api/get-started/create-token/)、[DNS API](https://developers.cloudflare.com/api/resources/dns/subresources/records/)。

创建仅用于业务 DNS 的 API Token，资源限定为实际托管业务域/基站域的 Zone：

- **Zone → Zone → Read**：列区与匹配。
- **Zone → DNS → Edit**：记录读取、新建、更新、删除；若控制台采用不同读写分类，确认这些记录动作都可用。

当前使用 Bearer API Token，不是 Global API Key + Email。业务 DNS 写入统一采用灰云/DNS-only；不要把边缘 SaaS 的权限加到只需要解析的 Token 上。

### 7.4 Cloudflare for SaaS 账号

官方参考：[Cloudflare for SaaS](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/)、[自定义主机名 API](https://developers.cloudflare.com/api/resources/custom_hostnames/)、[Zone 设置 API](https://developers.cloudflare.com/api/resources/zones/subresources/settings/)。

为**专用 SaaS 接入区**另建 Token，至少满足：

| 权限组 / 能力 | 源码实际调用 |
| --- | --- |
| Zone Read | `GET /zones`、`GET /zones/{id}`，检查区域 active/full |
| Zone Settings Read | `GET /zones/{id}/settings/ssl`，核对 Full (strict) |
| Zone Settings Edit（仅授权自动设严格模式时） | `PATCH /zones/{id}/settings/ssl` |
| DNS Edit | 接入区 Fallback 与标准入口 CNAME 的读取、精确复用或新增；已有值不匹配时拒绝覆盖，不自动更新共享 DNS 记录 |
| SSL and Certificates 的 Custom Hostnames 读写能力 | Custom Hostnames 列举/创建/更新/删除，以及 Fallback Origin 读取/设置 |

在当前 Cloudflare Token 控制台中核对 `SSL and Certificates` / `Custom Hostnames` 所对应的官方权限组，不把“DNS Edit”误当成已包含 SaaS 证书权限。`Zone Settings Read` 与 `DNS Read` 也不是一回事。

接入区 Token 不会自动获得 `example.com` 业务 TXT 的写权限；业务验证记录由独立 DNS 账号处理。即使业务 DNS 与 SaaS 都在 Cloudflare，也应把账号角色和资源范围分清楚。Cloudflare 免费/付费套餐的自定义主机名额度、证书选项、上传体积、超时和服务区域限制，以账号当前套餐为准，本项目不作免费或无限额度承诺。

### 7.5 EdgeOne + 腾讯云 SSL

申请入口：[EdgeOne 控制台](https://console.cloud.tencent.com/edgeone)、[EdgeOne API 文档](https://cloud.tencent.com/document/api/1552)、[SSL API 文档](https://cloud.tencent.com/document/api/400)。

专用 CAM 子用户需要目标 EdgeOne 站点的相应权限，以及将证书上传至本账号 SSL 产品的权限：

| 服务 | 当前实际使用的动作 |
| --- | --- |
| `teo` 只读 | `DescribeZones`、`DescribeAccelerationDomains`、`DescribeOriginGroup`、`DescribeL7AccSetting`、`CheckCnameStatus` |
| `teo` 接入/维护 | `CreateOriginGroup`、`ModifyOriginGroup`、`CreateAccelerationDomain`、`ModifyAccelerationDomain`、`ModifyAccelerationDomainStatuses`、`ModifyHostsCertificate`、`ModifyL7AccSetting` |
| `teo` 经确认删除 | `DeleteAccelerationDomains` |
| `ssl` | `UploadCertificate` |

不需要为了网关购买/删除站点而授权站点管理或账单权限；当前也不自动删除共享源站组、整个站点或所有 SSL 证书。资源级限制请按各动作支持范围配置；创建类动作或列表动作可能无法按尚不存在的资源 ID 收紧，优先限制专用账号/站点和动作集合。

只给 EdgeOne 权限、不包含 `ssl:UploadCertificate`，会出现“能列出站点，却不能绑定新证书”的半完成接入。DNSPod 的权限也不包含上述 `teo`/`ssl` 权限。

### 7.6 certd、通知与 Globalping

- **certd / ACME**：在 certd 内单独申请 DNS-01 所需权限。网关不会代管 certd 管理员 Token；SSH 推送只需写指定证书槽收件目录及执行固定激活命令的授权。
- **企业微信、飞书、钉钉**：在目标群按平台规则创建机器人，获取完整 Webhook URL；飞书/钉钉签名密钥另填。URL 本身通常就含秘密。参考 [企业微信群机器人](https://developer.work.weixin.qq.com/document/path/91770)、[飞书自定义机器人](https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot)、[钉钉自定义机器人](https://open.dingtalk.com/document/robots/custom-robot-access)。
- **SMTP**：使用专用发信账号/应用密码，最小化到必要发信权限；填写 SMTP 主机、TLS 模式、端口、用户名、密码、发件人与收件人。只支持 TLS/STARTTLS，不用明文 SMTP；常见是 465/TLS 或 587/STARTTLS，最终以邮件服务商要求为准。
- **通用 Webhook**：使用公网 HTTPS 接收端，可配置可选 Bearer token；无需提供云账号管理密钥。
- **Globalping**：当前实现是匿名、固定中国大陆节点的有界 HTTP 检测，**不接受账号 Token、不消耗用户配置的付费 credits**。无需“申请 API Key 才能启用”，但受匿名额度、节点可用性和平台策略限制。参考 [Globalping API](https://globalping.io/docs/api.globalping.io)。

### 7.7 密钥轮换与失败判断

新密钥在 UI 更新后先验证账号只读访问，再对专用测试记录检查必要权限，确认正常后在提供商撤销旧密钥。磁盘可能保留旧凭据版本和备份；撤销云端旧密钥与清理本地材料是两项不同工作。

- 列表成功：只证明当前读操作可用。
- 403 / 权限拒绝：核对具体产品动作和目标资源，不立即扩大为全账号权限。
- 429 / 配额：等待额度恢复，减少重复操作。
- 超时后“结果未知”：不能推断写入失败，先查云端对象及本机账本。
- 系统时间错误：可能导致腾讯云/阿里云请求签名失败及 TLS 异常，先检查 NTP。

## 8. 多 DNS、国内/国外访问与加速限制

**多 DNS 账号管理不等于地域分流。** 一个域名在公网实际由 NS 委派决定的权威 DNS 回答，不能同时在两个平台创建同名 Zone，就期待自动负载均衡或容灾。

当前可做：

- 基站、业务域名分别在不同平台托管，由各自账号维护；Cloudflare SaaS 的接入域另外完整接入 Cloudflare。
- 不同服务使用不同 CDN；例如 `cn.example.com` 与 `global.example.com` 指向同一个 EasyTier 应用，但分别作为独立服务配置 EdgeOne / Cloudflare，域名和证书都独立核对。
- 根据实测选择标准 Cloudflare 入口或某个受信任第三方入口，必要时手工切回。

当前不能做：

- 为同一个服务域名自动生成“中国大陆 → EdgeOne、境外 → Cloudflare”的地域线路。
- 自动修改 NS、迁移托管商、购买国内 CDN/备案、查询并选择最快节点。
- 自动创建 WAF、缓存规则、鉴权绕过、Origin Rules 或通用多 CDN 调度策略。

如果另行使用 DNS 服务商的 GeoDNS/智能线路，必须把该记录的管理权与网关隔离，并单独设计和验证。当前网关写入要求默认线路、无权重、启用、DNS-only；“强制覆盖”会按该模型替换记录，**不应对外部地域分流记录使用**。不要让两个自动化工具同时维护同一主机名。

国内访问速度还受运营商互联、地区、套餐和备案等条件影响；国外 CDN 的全球节点并不自动等同于中国大陆节点。监测中某一个 CN 节点成功，也不是全国全部运营商已验收。

对于登录、媒体、Range 下载、大文件上传和 WebSocket：网关本机未启用业务缓存，并提供流式代理，但 CDN 可能有额外缓存、请求大小、连接时长和内容政策限制。应在 CDN 控制台按业务要求配置缓存绕过/规则并实际测试；不能以“本网关不限制上传大小”推导整条 CDN 链无限制。

## 9. 日常监测、通知与流量

### 9.1 监测

在 **监测与诊断 → 服务连通检测**：

1. 先手动运行一次连通检测，查看上游、本机入口和外部访问三层结果。
2. 需要周期检测时点击 **启用自动连通检测**。这同时授权向 Globalping 发送公开入口域名/地址；不发送管理员密码、Cookie、云 Token 或内网上游地址。
3. 当前周期固定为 15 分钟，仅检查已应用且启用的服务；旧配置的外部授权未打开时不会因升级自动扩大授权。
4. 连续 2 次完整失败后告警，连续 2 次完整成功后恢复。额度不足、无合适节点、取消或无法确认不会被当作已确认服务故障。
5. 想停止后续检测用 **暂停监测**；刷新页面只是读取结果，不代表重新执行全量检查。

外部每入口每轮通常申请一个 CN 节点，多域名会分别消耗额度；不要反复点击检查耗尽匿名额度。状态及连续计数在管理进程重启后重新累计。本机断电或失联时不能靠本机通知自己报警，需要独立外部监控补足。

### 9.2 通知

进入 **系统管理 → 通知渠道**，添加受信任的公网 HTTPS 机器人/Webhook 或支持 TLS 的 SMTP 接收端。私网通知服务不在当前允许目标范围内。

- 列表只显示脱敏提示；编辑时空地址/密钥一般保留原值，清空秘密需按界面支持的方式或删除重建。
- **发送测试**会真实对外投递，不是只读校验；先核对收件人与群。测试间隔至少 30 秒，不要连续点击。
- 瞬态错误有有限重试，不承诺外部恰好收到一次。Webhook 接收端可按 `X-Gateway-Event-ID` / 事件 `id` 去重。
- 暂停/删除渠道不能撤回已经发出的通知。
- 排障时不公开 Webhook URL、SMTP 密码或签名密钥。

### 9.3 流量统计

**流量统计**来自 Nginx 已完成 HTTP 请求，不是实时网卡流量、EasyTier 总吞吐或 CDN 计费数据。长下载/WebSocket 未结束时不会立即完整计入。日统计按 `Asia/Shanghai`；数据缺口、追赶中等提示应保留，不要通过删 `traffic-state.json` 来制造“恢复正常”。

维护日志轮转，定期检查磁盘空间。受管 `access.log` 是普通访问日志，可能含敏感请求路径；CDN 场景中的直接连接 IP 也可能是边缘地址，不能据此假定已经还原真实访客 IP。

## 10. 备份、历史和恢复

### 10.1 三种东西不要混用

| 类型 | 能做什么 | 不能替代什么 |
| --- | --- | --- |
| 配置导出 | 阅读/核对主配置，不含云密钥和 PEM | 不是完整可运行备份，仍包含私有拓扑 |
| 配置历史 | 最近 30 版服务和基站设置，预览差异后恢复为草稿 | 不恢复证书、凭据、云端资源和运行快照 |
| 加密配置备份 `.enc` | 保存主/已应用配置、当前引用的证书/私钥、云凭据、通知密钥及支持的账本 | 不含二进制、登录密码/哈希、unit/env、日志、流量数据和完整系统；当前没有内置整包恢复工具 |

在 **系统管理 → 备份与历史** 下载 `.enc` 时，输入当前管理密码和两次独立备份口令（12–128 字符、最多 512 UTF-8 字节）。口令不保留，丢失无法找回。格式采用 Argon2id 派生密钥和 AES-256-GCM；即使已加密，也不要上传 GitHub。

历史恢复需先预览，再输入管理密码确认；恢复后自动解析会暂停，运行中的 Nginx 和云端 DNS 不会自动回滚。检查草稿、证书/账号依赖、预览发布，再按需要恢复自动解析。

### 10.2 可用于灾备的完整停机快照

生产至少另有完整设备快照/受保护停机备份，覆盖程序、整个数据目录、环境、unit、定制 drop-in、相关 CA/EasyTier/防火墙配置，并在隔离环境演练恢复。下面仅示范**默认新安装布局**的网关文件备份，不包含整台操作系统或其他应用：

```bash
# 先约定维护窗口；失败时保持服务停止，查明原因后再启动。
sudo -i
set -euo pipefail
umask 077
install -d -m 0700 /var/backups/gateway-private
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP="/var/backups/gateway-private/gateway-full-${STAMP}.tar.gz"

systemctl stop gateway.service
systemctl stop gateway-proxy.service

paths=(opt/gateway var/lib/gateway etc/default/gateway
  etc/systemd/system/gateway.service
  etc/systemd/system/gateway-proxy.service)
for p in etc/systemd/system/gateway.service.d \
  etc/systemd/system/gateway-proxy.service.d \
  etc/logrotate.d/gateway etc/logrotate.d/gateway-traffic; do
  if [[ -e "/$p" ]]; then paths+=("$p"); fi
done
tar --acls --xattrs -czf "$BACKUP" -C / "${paths[@]}"
chmod 0600 "$BACKUP"
tar -tzf "$BACKUP" >/dev/null
sha256sum "$BACKUP" > "$BACKUP.sha256"
chmod 0600 "$BACKUP.sha256"

systemctl start gateway-proxy.service
systemctl start gateway.service
```

这份 tar 是**含明文私钥和密钥的备份**，`0600` 不是加密。按组织策略进一步加密后保存到隔离介质，并将加密密钥分开保管。仅有一个同盘副本不能应对设备损坏。旧安装必须先替换为真实布局；不要同时打包多套混淆的 data-dir。

### 10.3 完整恢复步骤

1. 确认备份来源、SHA-256、版本、架构与恢复目标；检查归档成员只在预期路径内，没有绝对路径/`..`，不以 root 解压未知来源归档。
2. 保存故障现场和日志，停止管理和代理两个 unit，确认没有其他程序正在写同一数据目录。
3. 将现有程序目录与数据目录**整体移到带时间戳的受保护故障保留目录**；env、两个 unit、对应 drop-in 目录和项目拥有的 logrotate 文件也要成组保留并隔离，恢复备份中“原本不存在”的状态。尤其旧快照没有某个 `gateway*.service.d` 时，不能让故障版本新增的 drop-in 残留并覆盖恢复的 unit。只处理本项目的已核对路径，不接管系统默认 Nginx 或其他服务；不要把旧归档直接叠加到新树。
4. 恢复匹配的一组程序、完整数据、env、unit、drop-in 和必要 OS 依赖，保留 root 所属及 `0700`/`0600` 保护；运行 `systemctl daemon-reload`。
5. 先对实际数据目录执行匹配程序的 `state-check`。确认网关 IP、主机 IPv6、Nginx/njs、CA 与目录均符合原配置；恢复到不同网络不是单纯复制文件即可完成。
6. 在隔离/受控条件下核对云端真实记录和未完成任务。恢复可能恢复 `autoResolve=true` 和等待任务，不要在核对前贸然启动能对云端写入的管理进程。
7. 按 **先代理、后管理** 顺序启动，执行第 12 节本机验证和一个代表性业务验证；再恢复计划中的自动维护。

本地恢复不能撤销已生效的 DNS/边缘删除或重建云资源，旧备份中的已撤销 Token 也不会重新有效。若需云端回退，须单独审核原值、资源归属及影响后执行。

## 11. 升级、回滚和下线

### 11.1 升级

从可信 Releases 下载并校验目标版本，使用包内 `replace.sh` 保留原配置和数据升级。升级前阅读版本说明、检查备份可恢复性，并安排短暂停机窗口。

需要分开校验、预检与实际替换时，使用手动流程：

1. 从可信 Release 下载新包并执行内外两层校验；阅读该版本变更、schema 和已知限制。
2. 记录旧 `version --json`、实际 unit/env/data-dir；检查空间和完整备份可恢复性。暂停不必要的发布，避免正在进行云写入时换版本。
3. 在解压后的发行目录先预检：

   ```bash
   sudo bash ./replace.sh "$PWD/gateway-linux-arm64" --dry-run
   ```

4. 核对预检显示的旧布局、版本、数据目录与计划，维护窗口内执行：

   ```bash
   sudo bash ./replace.sh "$PWD/gateway-linux-arm64"
   ```

默认保留现有环境文件和 unit，单独维护该功能拥有的流量日志轮转文件。脚本先停止两个服务并保存程序/完整数据/env/两个 unit 等受保护快照，再替换和校验；备份路径会显示在输出中，通常为数据目录旁的 `*-backup-<UTC时间>.tar.gz`。**此部署快照也含明文秘密**，并不涵盖所有系统 drop-in、自定义 CA、EasyTier 配置和整台机器，额外系统定制需另行备份。

已知 schema 4/5 会在停机备份后显式迁移到当前 schema 6；未知版本或损坏文件停止处理，不重置为新安装。日常升级不要使用 `--reset-data`：它会清空服务、证书和凭据。`--replace-config` 会用模板覆盖设备专属 env/unit，只有明确审查并需要替换时才使用。

### 11.2 回滚

升级失败时脚本会尝试恢复匹配的完整快照并验证；不能验证回滚成功时会停服务、保留现场，需人工处理。输出的回滚命令针对本次实际路径，执行前仍要检查路径与备份。

不要只换回旧二进制：旧程序可能不支持 schema 6、Argon2id 哈希或版本化证书/凭据。必须恢复相同时间点的一组程序、数据、环境和 unit。云端 DNS/CDN 已完成的变化不会因为本地回滚撤销。

### 11.3 删除或停用服务

- **停用**：关闭服务启用状态并保存后，线上仍可能运行；预览并发布草稿后本机入口才撤下。DNS 的撤回按预览显示处理，不凭 UI 灰色开关假定已断开公网。
- **删除**：删除窗口分别提供 DNS、EdgeOne 或 Cloudflare 清理选项。只清理明确勾选、由本网关托管且归该服务独占的资源；不勾选则保留云资源。
- **Cloudflare 清理**：可清理本程序创建的专属 Custom Hostname、关联业务 CNAME 与本程序追加的验证 TXT；共享 Fallback、标准入口、Zone 和严格 SSL 设置保留。
- **EdgeOne 清理**：清理经确认的专属加速域名及相关 CNAME；共享源站组、证书等保留，不强制删除关联资源。
- 云端删除立即产生影响、已完成的删除不会自动回滚；本机服务从草稿删除后仍需要应用最新草稿才能撤下运行入口。
- 已发布域名重命名前，应先停用并撤回旧托管记录；不要直接删字段绕过归属保护。源站基站仍被 EdgeOne/Cloudflare 引用时不能随意更名或清空。

删除证书槽需要草稿与已应用配置都解除引用；磁盘证书材料可能为恢复而保留，删除槽不等于私钥安全销毁。删除账号也不能绕过尚未结束的发布/删除操作；先核对云资源，再撤销云密钥和处理本机保留材料。

## 12. 常用运维命令与故障定位

### 12.1 最小本机检查

以下为 systemd 安装的默认新布局；旧布局按实际 unit 与路径替换。配置文件直接启动时查看启动终端日志，不使用两个 unit 的命令，并将版本命令的路径换成实际 `gateway-linux-arm64`；其余地址和数据目录按实际配置替换。

```bash
sudo systemctl status gateway.service gateway-proxy.service --no-pager
sudo journalctl -u gateway.service -u gateway-proxy.service -n 100 --no-pager
sudo /opt/gateway/gateway version --json
curl --noproxy '*' -fsS --max-time 3 http://10.20.0.9:18080/healthz
curl --noproxy '*' -fsS --max-time 3 \
  http://127.0.0.1:18079/__gateway_generation
sudo /usr/sbin/nginx -t -c /var/lib/gateway/managed-nginx.conf
df -h /var/lib/gateway /var/cache/gateway
```

不要用裸 `nginx -t` 检查错配置，也不要 `systemctl restart nginx` 接管系统默认 Nginx。`/healthz` 只证明管理进程构建/健康，不证明所有业务或云端正常。

systemd 部署可使用发行包的本机验证器，同时检查二进制哈希、状态格式、两个 unit、构建身份和 Nginx 代际（哈希来自已校验下载包，而不是把安装后的文件自己与自己比较）：

```bash
EXPECTED_SHA="$(sha256sum gateway-linux-arm64 | awk '{print $1}')"
sudo python3 ./verify-release.py /opt/gateway/gateway /var/lib/gateway \
  10.20.0.9:18080 "$EXPECTED_SHA"
```

### 12.2 排障表

| 现象 | 优先检查 | 不要做 |
| --- | --- | --- |
| 管理页打不开 / Host 403 | EasyTier 地址是否分配；URL 的 Host:端口是否与实际监听一致（配置文件或旧部署的 MANAGEMENT_LISTEN）；运行日志；SSH 是否为两端一致的回环方案 | 开放公网管理端口、改成通配监听、关闭 Host/同源校验 |
| 安装被拒绝 | 是否 ARM64/systemd；原 Nginx/旧安装/端口占用；依赖和真实网关 IP | 删除旧数据、杀所有 Nginx 强行安装 |
| 本机入口正常，公网超时 | 原生公网 IPv6、路由、防火墙、运营商端口、DNS 缓存、客户端 IPv6 | 把内网成功等同于公网成功 |
| TLS/444/证书不匹配 | URL 域名、SNI/Host、SAN、到期、证书链、源站回源域名 | 用裸 IP 测域名路由、用 `-k` 当合格证据 |
| 上游 HTTPS 失败 | IP SAN、系统 CA、EasyTier 到达性、上游端口 | 关闭全局 TLS 校验 |
| DNS 找不到托管区/账号冲突 | 权威 NS、各账号可见区域、最长后缀/明确归属 | 在另一平台建同名 Zone 当迁移完成 |
| 强制覆盖预览出现已有记录 | 当前记录归属、非默认线路/权重/橙云、其他系统自动维护 | 为通过发布盲目覆盖地域分流或其他业务 |
| Cloudflare 等待证书 | 三项资源状态、TXT/CAA、权限、额度、源站及原任务状态 | 一直删除重建主机名、清空账本 |
| Cloudflare 52x/重定向循环 | Strict、源站 IPv6:443、证书/SNI、Fallback 指向、平台规则 | 降级为 Flexible 或将 Fallback 指向业务 CNAME 自身 |
| EdgeOne 证书上传失败 | `ssl:UploadCertificate`、所选云账号、PEM、绑定状态 | 只看能列站点就排除权限问题 |
| WebSocket 定时断开 | 边缘超时、应用心跳、站点级设置 | 只把 Nginx 超时调大 |
| 外测 429/无节点 | 匿名额度、CN 节点可用性、等待退避 | 连续重测或直接判断源站宕机 |
| 运行事务/锁异常 | 两个 unit 和实际进程、`runtime-transaction.json`、磁盘与完整快照 | 删除 `runtime.lock`、拼接不同版本快照/PEM |
| JSON 无法读取/空间不足 | 空间、权限、文件完整性、支持的 schema、只读 state-check | 将坏文件删掉自动新建并丢失归属 |

对于本机未完成事务，先保存完整现场，停止管理与代理，然后按代理先、管理后的正常启动路径恢复；代理 `render` 在 Nginx 读配置前执行恢复。恢复拒绝时保持停止，按第 10 节保存并核对完整快照，结合日志检查 [状态与应用事务](technical-optimization.md)。不要让混合状态的程序继续自动改 DNS。

### 12.3 忘记管理员密码

下面的离线恢复步骤针对未使用 `config.json` 初始密钥的 CLI/env 部署。配置文件中的 `initialKey` 不是密码重置接口：已有密码哈希存在时，修改该字段不会改变密码；恢复认证前先核对受保护的配置与备份，避免复用遗留的明文密钥。

无公网重置/找回接口。仅由受信任本地管理员处理：停止管理服务；确认真实 data-dir；将 `password.sha256` 与存在的 `initial-password` **一并**移入仅 root 可读的保护目录，不动业务 state、applied、PEM 或云凭据；启动管理服务后在本地读取新初始密码，立即通过安全页面改密。

只移动哈希、留下旧初始密码会重用旧密码；不能这样做。密码恢复不需要 `--reset-data`。旧会话因进程重启失效，旧认证文件不要放回新运行服务。

## 13. 部署检查表

- [ ] 已校验可信 Release，两个受管 unit 正常且构建身份一致。
- [ ] 管理页面仅私网/回环可达，初始密码已替换。
- [ ] 上游、本机 HTTPS、实际权威 DNS、公网访问逐层有证据。
- [ ] 所有域名 SAN/有效期正确，续期任务已完成至少一次端到端推送测试。
- [ ] 若用 CDN，已核对边缘证书、实际回源与至少一个真实登录/上传/下载/WebSocket 场景。
- [ ] 自动解析授权范围、监测外部数据发送及通知收件人已确认。
- [ ] 有受保护的完整备份、升级回退步骤及密钥撤销联系人。

公开报障时提供版本、部署平台、脱敏错误和最小复现，不附带完整数据目录、云密钥、Cookie 或证书私钥。

# Emby-In-One

面向 Emby 客户端的多上游聚合代理，将多台 Emby 服务器整合为统一入口，提供媒体合并、用户授权、独立观看状态和播放管理。

[![GitHub Release](https://img.shields.io/github/v/release/Zkunlun/Emby-In-One?color=green)](https://github.com/Zkunlun/Emby-In-One/releases)
[![License: GPL v3](https://img.shields.io/github/license/Zkunlun/Emby-In-One?color=blue)](LICENSE)

[快速开始](#快速开始) · [文档](docs/README.md) · [更新日志](Update.md) · [安全](SECURITY.md) · [English](README_EN.md)

本项目基于 [ArizeSky/Emby-In-One](https://github.com/ArizeSky/Emby-In-One) 持续开发与维护。

## 适用场景

- 有多台可使用的 Emby 服务器，希望在一个客户端入口浏览、搜索和选择播放来源。
- 为不同使用者分配上游访问权限，并分别保存普通用户的播放进度、已观看和收藏。
- 通过 Web 面板集中管理上游、播放线路、用户和日常运行状态。

使用前需要可访问的 Emby 上游及相应账户或 API Key，以及运行 EIO 的环境。EIO 聚合已有媒体资源，不附带媒体库。

## 核心功能

| 能力 | 说明 |
| --- | --- |
| **多上游聚合与媒体合并** | 汇总媒体库与搜索结果，按作品规则合并电影、剧集和单集，保留可选择的不同版本。 |
| **用户授权与观看状态** | 按普通用户分配来源，独立保存进度、已观看和收藏；合并作品的版本共享同一用户状态，另有授权容量与设备限制。 |
| **播放模式与线路管理** | 提供代理与直连播放模式，管理有序备用推流线路，按模式处理播放前可恢复的线路故障。 |
| **上游接入与客户端身份** | 支持用户名/密码或 API Key，提供身份透传、Infuse/Hills/CapyPlayer 预设和自定义身份。 |
| **Web 管理与媒体库统计** | 面板管理上游、用户、网络代理、设置和日志，提供电影、剧集与集数统计。 |
| **部署与日常运维** | 支持 Release 二进制、Docker 源码构建与 Go 源码运行，提供 SSH 菜单、日志轮转和状态检查。 |

## 界面预览

管理面板集中提供上游、用户、网络代理、设置与日志入口。

### 系统概览

![系统概览界面，展示上游数量、在线节点、ID 映射数和存储引擎](docs/images/system-overview.png)

系统概览：集中查看上游与运行状态。实际运行界面已遮盖敏感字段，并缩放用于展示。

### 上游管理

![上游节点列表，展示来源排序、授权人数、容量限制、状态和操作入口](docs/images/upstream-management.png)

上游管理：实际运行界面已遮盖服务器名称与地址，并缩放用于展示。

查看各上游的状态与已授权人数，调整来源顺序，并进入认证方式、播放模式与推流线路配置。

### 用户与授权

![用户管理列表，展示授权来源、用户状态、创建时间和编辑入口](docs/images/user-permissions.png)

用户管理：实际运行界面已遮盖用户名与服务器名称，并缩放用于展示。

查看普通用户状态与授权来源，通过编辑入口设置可访问的上游及首页媒体库入口显示；隐藏入口不会取消访问授权。操作步骤见[首次使用](docs/getting-started.md)。

## 快速开始

**推荐：Linux Release 二进制 + systemd，无需本地 Go 编译环境。**

准备 Linux 与 systemd、root/sudo 权限及 Bash、curl、grep、sed、sha256sum 等脚本工具，并确保能访问 GitHub API 和 Release 下载。完整架构与部署要求见[安装指南](docs/installation.md)。

首次安装最新正式版：

```bash
curl -fsSL -o release-install.sh https://github.com/Zkunlun/Emby-In-One/releases/latest/download/release-install.sh
sudo bash release-install.sh
```

脚本安装并启动服务，默认项目目录为 `/opt/emby-in-one`。首次初始化时，本地管理员为 `admin`，随机密码在安装输出末尾显示，请保存。

| 入口 | 默认地址 |
| --- | --- |
| 管理面板 | `http://服务器IP:8096/admin` |
| Emby 客户端 | `http://服务器IP:8096` |

安装后继续完成下方的首次接入。指定版本、Docker/Compose 和源码运行见[安装指南](docs/installation.md)；已有实例先阅读[升级与备份](docs/operations.md#版本升级)。

## 首次使用

1. **登录面板**：打开管理地址，使用安装输出中的 EIO 本地管理员账户。
2. **添加上游**：填写 Emby 地址，选择用户名/密码或 API Key 一种认证方式，按上游要求选择客户端身份。保存后检查状态；用户名型 `passthrough` 尚无身份时，需用真实 Emby 客户端以 EIO 管理员登录一次完成采集，详见[接入步骤](docs/getting-started.md#第二步添加上游)。
3. **创建普通用户并授权**：在用户管理中创建本地账户，明确勾选可访问的上游。不勾选任何服务器表示没有上游访问权限。
4. **连接客户端**：填写 EIO 客户端地址，使用该普通用户的本地账户登录。
5. **验证访问和播放**：浏览授权范围内媒体，播放一项内容；有多版本时选择具体来源，异常时查看[排障指南](docs/troubleshooting.md)。

EIO 本地账户用于登录 EIO，上游账户用于 EIO 接入来源。普通用户使用本地独立观看状态；管理员保持上游账户的观看状态语义。完整操作见[首次使用指南](docs/getting-started.md)。

## 播放模式与使用边界

| 模式 | 媒体流路径 | 选择条件 |
| --- | --- | --- |
| `proxy`（默认） | EIO 转发上游媒体流 | EIO 能访问推流源，客户端连接 EIO；可为上游绑定服务端 HTTP 网络代理。 |
| `redirect` | EIO 返回重定向，客户端直连上游流地址 | 客户端必须能访问推流地址，可节省 EIO 媒体带宽；不能绑定服务端 HTTP 网络代理。 |

**直连凭据可见性：** `redirect` 的客户端可见 URL 可包含上游 token/API Key，持有者可能按共享上游账户权限绕过 EIO 访问。使用受限上游账户；不接受这一取舍时使用 `proxy`。线路切换与身份规则见[上游接入与播放](docs/playback-and-upstream.md)。

- **授权与显示不同**：隐藏库入口不取消访问授权，搜索和播放等仍按授权执行。
- **授权容量与播放设备限制不同**：`maxConcurrent` 限制每台上游可授权的普通用户数量；同一普通用户在同一上游的活跃设备另有约束，见[用户与权限](docs/users-and-permissions.md)。
- **统计与合并口径不同**：媒体库统计按授权在线来源的官方数据累加，不做跨来源去重，见[统计说明](docs/media-counts.md)。
- **升级保留配套备份**：配置、数据库与 `user-password.key` 等文件需一致保存；安装回滚不替代完整数据备份，旧用户库还有版本限制，见[运维指南](docs/operations.md#版本升级)。

合并按请求需要发现，候选数量和分页仍有边界，详见[合并规则](docs/media-merge.md)。已有客户端验收与未覆盖事项见[发布验证说明](docs/release-v1.4.9-validation.md)；凭据存储和使用风险见[安全政策](SECURITY.md)。

## 文档与常见问题

| 阅读任务 | 文档 |
| --- | --- |
| 安装与首次使用 | [安装](docs/installation.md) · [首次接入](docs/getting-started.md) |
| 配置、用户与播放 | [配置参考](docs/configuration.md) · [用户与权限](docs/users-and-permissions.md) · [上游与播放](docs/playback-and-upstream.md) |
| 备份、更新与排障 | [运维](docs/operations.md) · [排障](docs/troubleshooting.md) |
| 合并规则与统计口径 | [媒体合并](docs/media-merge.md) · [媒体库统计](docs/media-counts.md) |
| 全部文档与开发参考 | [文档索引](docs/README.md) · [开发与贡献](docs/development.md) |

### 安装后为什么还没有媒体？

首次配置没有上游。添加可访问的 Emby 来源，再为普通用户授予访问权限；按[首次使用指南](docs/getting-started.md)完成接入。

### 客户端连接哪个地址、使用哪个账户？

连接 EIO 的客户端地址，默认 `http://服务器IP:8096`，使用 EIO 本地账户。管理面板位于 `/admin`；上游凭据用于 EIO 接入来源，账户区别见[首次使用](docs/getting-started.md#先分清三个账户)。

### 为什么统计数字与合并后的列表不同？

统计逐源累计官方电影、剧集和集数，重复作品也会累加；合并列表按作品规则归组。缓存及错误行为见[媒体库统计](docs/media-counts.md)。

### 忘记管理员密码怎么办？

管理员密码保存为不可逆哈希，不能查询原明文。通过 SSH 管理菜单或停服后的二进制 CLI 重置，随后重新登录客户端，操作见[密码重置](docs/operations.md#管理员密码重置)。

## 参与项目

欢迎通过 [Issues](https://github.com/Zkunlun/Emby-In-One/issues) 提交问题和兼容性反馈，通过 [Pull Request](https://github.com/Zkunlun/Emby-In-One/pulls) 参与改进。反馈请提供 EIO 与客户端版本、认证/播放模式、复现步骤和脱敏日志。

开发环境、构建及验证入口见[开发指南](docs/development.md)。安全问题按[安全政策](SECURITY.md#reporting-a-vulnerability)私下报告。

## 致谢与许可证

感谢 [ArizeSky](https://github.com/ArizeSky) 创建原项目并完成早期架构与核心功能，感谢原项目及当前仓库的贡献者、问题反馈者和客户端测试参与者。原代码、设计与历史贡献归对应作者和贡献者所有；本仓库持续开发、维护和发布当前 Go 主线。

原 Node.js V1.2.1 保留为[历史参考](legacy/README.md)，不参与当前 Go 构建；[旧版文档](README_V1.2.1.md)单独保留。贡献记录见 [GitHub Contributors](https://github.com/Zkunlun/Emby-In-One/graphs/contributors)。

本项目按 **GNU General Public License v3.0** 发布，修改和再分发请遵守 [GPL-3.0](LICENSE)。

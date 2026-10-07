# 开发与贡献

[项目主页](../README.md) · [文档索引](README.md) · [English](en/development.md)

适用主线：V1.4.9。

## 开发环境与构建

当前主线为 Go 1.23+，使用 CGO 和仓库 third_party/sqlite；需 C 编译链，public/ 为必需内嵌包。完整源码克隆和初始配置见[安装指南](installation.md#go-源码运行)。根 Node package 仅用于面板样式构建，后端服务不是 Node.js 程序。

```bash
go build ./cmd/emby-in-one
go test ./...
go vet ./...
```

构建版本示例：

```bash
CGO_ENABLED=1 go build -ldflags="-X main.Version=V1.4.9" -o emby-in-one ./cmd/emby-in-one
./emby-in-one --version
```

以上是开发者验证入口，不是本轮已经执行/通过的测试记录。变更相关测试先行，公共后端行为按既有 CI 要求回归；全量 race 不作为本文已通过的事实，历史发布证据见[验证说明](release-v1.4.9-validation.md)。

修改 admin.html/admin.js 或样式时，使用 Node/npm 执行 `npm run build:panel` 重建 public/vendor/tailwind.css，并核查嵌入与磁盘覆盖资源的一致性。所需自托管依赖随 public/vendor 保存；不要在面板加入未说明的第三方资源加载。

## 后端模块职责

| 文件 | 职责 |
| --- | --- |
| `cmd/emby-in-one/main.go` | 程序入口 |
| `internal/backend/config.go` | YAML 配置加载/保存/校验/原子写入 |
| `internal/backend/server.go` | HTTP 服务器启动与优雅关机 |
| `internal/backend/routes.go` | 路由注册总表（URL → Handler 映射） |
| `internal/backend/middleware.go` | HTTP 中间件（CORS、日志、状态码捕获、CSP） |
| `internal/backend/ssrf.go` | SSRF 防护策略与安全拨号器（代理测试 / 上游连接） |
| `internal/backend/auth.go` | 密码哈希、校验和认证辅助 |
| `internal/backend/auth_context.go` | 请求级认证上下文注入与提取 |
| `internal/backend/auth_manager.go` | EIO 本地 token 签发、持久化、校验与撤销 |
| `internal/backend/identity.go` | 客户端身份捕获与 Passthrough 五级解析 |
| `internal/backend/identity_persistence.go` | 客户端身份按服务器维度持久化 |
| `internal/backend/identity_lifecycle.go` | 身份缓存归属、迁移与异步发布栅栏 |
| `internal/backend/user_store.go` | 用户 CRUD、密码 hash/secret、授权及 SQLite 索引 |
| `internal/backend/handlers_admin.go` | 管理后台 API 处理器（上游服务器增删改查） |
| `internal/backend/handlers_system.go` | 系统信息接口（/System/Info） |
| `internal/backend/handlers_user.go` | 用户登录限速与用户相关接口处理器 |
| `internal/backend/admin_validation.go` | 管理后台输入校验与辅助工具 |
| `internal/backend/idstore.go` | SQLite 双向 ID 映射（虚拟 ID ↔ 原始 ID） |
| `internal/backend/id_rewriter.go` | 递归 ID 虚拟化/反虚拟化重写 |
| `internal/backend/query_ids.go` | 批量查询 ID 解析 |
| `internal/backend/media.go` | 媒体聚合、去重、元数据优先级选择 |
| `internal/backend/aggregation.go` | 宽恕期聚合；迟到结果只登记 ID/实例，不补写响应 |
| `internal/backend/media_access.go` | 当前授权实例与观看查询快照 |
| `internal/backend/media_request_access.go` | 请求级来源、媒体版本与归属校验 |
| `internal/backend/media_items.go` | 媒体条目查询（多上游扇出合并） |
| `internal/backend/media_resume.go` | "继续观看"接口代理与多上游合并 |
| `internal/backend/media_nextup.go` | "接下来观看"接口代理与多上游合并 |
| `internal/backend/media_playback.go` | PlaybackInfo 与用户/上游单设备 lease 预留 |
| `internal/backend/media_stream.go` | 视频/音频流代理（虚拟 ID 路由解析） |
| `internal/backend/library_image.go` | 图片代理（缓存头） |
| `internal/backend/series_userdata.go` | 系列级观看历史隔离（Resume/NextUp） |
| `internal/backend/session_userdata.go` | Sessions/Playing 进度上报 |
| `internal/backend/watch_store.go` | 每用户观看进度存储与持久化 |
| `internal/backend/watch_visible_store.go` | 授权先过滤的批量观看查询 |
| `internal/backend/watch_lifecycle.go` | 管理事务、待清理日志与启动恢复 |
| `internal/backend/watch_lifecycle_runtime.go` | 精确缓存/lease清理与删除后继承 |
| `internal/backend/watch_lifecycle_requests.go` | 认证与异步状态发布的请求守卫 |
| `internal/backend/watch_playback_store.go` | 播放事件的原子观看状态合并 |
| `internal/backend/playback_watch_state.go` | 播放字段解析、来源匹配与完成判定 |
| `internal/backend/playback_watch_cache.go` | 有界会话时长、位置缓存与终止状态 |
| `internal/backend/playback_watch_owner.go` | 用户/合并影片共享写入权与代际 |
| `internal/backend/playback_watch_events.go` | 播放上报接入、元数据补齐与手动重置 |
| `internal/backend/playback_watch_legacy.go` | 旧 PlayingItems 播放上报兼容 |
| `internal/backend/playback_limiter.go` | 用户/上游单设备 lease（revision、心跳、exact Stop） |
| `internal/backend/playback_routes.go` | 用户归属的播放路由与媒体源会话 |
| `internal/backend/login_limiter.go` | 登录失败限流（按 IP；容量满时逐出最旧记录） |
| `internal/backend/streamproxy.go` | HTTP 流代理（背压、HLS 相对路径重写） |
| `internal/backend/fallback_proxy.go` | 兜底路由：扫描 URL/Query 中的虚拟 ID |
| `internal/backend/healthcheck.go` | 离线来源重新认证及在线推流线路探测 |
| `internal/backend/logger.go` | 分级日志（Console + File 双输出 + 轮转） |
| `internal/backend/scrypt_local.go` | scrypt 密钥派生实现，用于密码哈希 |
| `internal/backend/sqlite_cgo.go` | CGO 嵌入式 SQLite 编译与底层绑定 |
| `internal/backend/upstream.go` | 上游连接池 & 并发请求编排 |
| `public/embed.go` | go:embed 指令（将 admin.html、admin.js 与 vendor/ 编译进二进制） |

普通用户密码加密另由 `internal/backend/password_secret.go` 管理；合并持久关系及 Counts 由当前相应模块实现。完整 tree 以[仓库源码](https://github.com/Zkunlun/Emby-In-One/tree/main)为准，本表为模块入口，不是全文件清单。

## 资源与部署文件

| 路径 | 职责 |
| --- | --- |
| third_party/sqlite/ | CGO 编译依赖 |
| public/admin.html、public/admin.js | Vue 3 管理面板模板与逻辑 |
| public/vendor/ | 自托管 Vue、lucide、Tailwind 产物和字体 |
| assets/panel.css、tailwind.config.js、package.json | 面板样式输入、扫描与 build:panel 脚本 |
| Dockerfile、docker-compose.yml | Go 多阶段构建与运行挂载 |
| install.sh | 源码 Docker 安装入口 |
| release-install.sh | Release 二进制 systemd 安装 |
| emby-in-one-cli.sh | SSH 管理菜单 |
| legacy/ | V1.2.1 Node 参考实现；不参与当前构建/镜像/安装 |

legacy/src 为早期 Express/ID 虚拟化对照；历史 tests 和 Node 依赖只属于 legacy，不能替代当前 Go 验证。

## 贡献与反馈

当前主线为 Go 实现。欢迎通过 Issues 提交可复现的问题、兼容性反馈和功能建议，也欢迎通过 Pull Request 参与修复与改进。

提交代码时建议遵循以下原则：

- Bug 修复尽量附带能够覆盖根因的回归测试，避免只针对单一客户端现象打补丁。
- 保持修改范围聚焦，避免在同一个 PR 中混入无关的架构调整或格式化变更。
- 提交前至少运行与修改范围相关的测试；涉及后端公共行为时建议执行 `go test ./...`。
- 涉及管理面板时同时检查前端资源构建与内嵌资源是否一致；涉及安装或发布流程时同步核对版本号、安装脚本和 Release workflow。
- 客户端兼容问题应尽量附带请求路径、响应差异、日志或可复现步骤，便于定位 Emby API 行为差异。

普通问题通过 [Issues](https://github.com/Zkunlun/Emby-In-One/issues)，代码修正可提交 [Pull Request](https://github.com/Zkunlun/Emby-In-One/pulls)。反馈给出 EIO 版本、客户端版本、认证/播放模式、复现步骤和脱敏日志。安全问题按[安全政策](../SECURITY.md)私下报告。

## 维护与历史

本项目基于 [ArizeSky/Emby-In-One](https://github.com/ArizeSky/Emby-In-One) 持续开发与维护，原代码、设计和历史贡献归对应作者/贡献者。当前维护和发布入口为本仓库；源码许可证仍为 [GPL-3.0](../LICENSE)。

[更新日志](../Update.md) · [更新计划](../Update%20Plan.md) · [版本编号调整](version-numbering.md) · [旧版文档](../README_V1.2.1.md) · [legacy 说明](../legacy/README.md)。上述历史路径保留，不作为当前安装默认入口。

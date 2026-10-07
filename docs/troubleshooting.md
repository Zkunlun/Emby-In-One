# 常见问题排查

[项目主页](../README.md) · [文档索引](README.md) · [English](en/troubleshooting.md)

适用主线：V1.4.9。

## Passthrough 服务器登录失败 (403)

首次安装且没有任何真实客户端身份记录时，使用用户名/密码认证的 `passthrough` 上游会**跳过初始登录并保持 offline**，等待真实客户端身份；不会直接拿 Infuse fallback 去完成首次上游登录：
1. 用真实 Emby 客户端（Infuse、Emby iOS 等）以 **admin** 身份登录一次 Emby-In-One
2. 代理捕获客户端身份后，会自动重试离线的 passthrough 上游
3. 成功登录后，该服务器使用的客户端身份会持久化，后续重启可直接复用
4. 日志中的身份来源可用于排查，例如 `last-success` 表示该服务器上次成功身份，捕获来源表示真实客户端身份；`infuse-fallback` 仍是内部身份解析的最终兜底，但初始登录/管理端验证在只有该兜底时会主动延迟
5. 如果捕获的客户端身份仍被上游拒绝，请改用上游允许的客户端重新以 admin 登录一次以更新身份

## 上游服务器显示离线 / 登录超时

`timeouts.login` 与 `timeouts.healthCheck` 默认各 30 秒。离线可能来自凭据、上游政策、网络、身份或超时，先检查状态和日志，不只根据升级时间推断原因：

- 上游登录耗时超过 `timeouts.login` 即判为登录失败 → 服务器显示离线。登录较慢（10–30 秒）的上游请在「全局设置」调大该值
- 周期重新认证只针对离线 API 来源，受 `timeouts.healthCheck` 约束；未恢复会在后续周期重试。在线来源另做推流线路探测，两种状态不能混为一谈
- 这两个值只能把超时收紧到 `timeouts.api` 以下；若要放宽探测，需同时调大 `api`
- 启动日志里出现 `Timeouts are now enforced` 即表示你的配置中这两个值小于 `api`

## 客户端 UA 采集与清理

真实 Emby 客户端登录成功且携带可用身份头时，管理员和普通用户都可产生 token 范围的 UA / Device 捕获；管理面板登录未必提供这些身份字段。捕获不会改变用户对上游的授权，也不会覆盖选定的固定伪装预设。

可使用目标客户端登录 EIO，并在管理面板「已捕获的客户端信息」确认记录。passthrough 的成功身份缓存按稳定服务器 ID 分隔；改绑、删除、禁用或改密码会清理相关捕获并阻止旧异步结果重新发布。若旧数据归属无法确认，相关清理会保守移除，需要重新登录采集。

## 播放 403 / 401

可能的原因：
- 上游 token 过期 → 在管理面板点击「重连」
- passthrough 服务器的头不完整 → 查看日志中 `Stream headers for [服务器名]` 确认头信息
- 版本或来源授权已变化 → 重新获取当前授权详情和 PlaybackInfo；具体 MediaSourceId 仍必须与所选真实版本匹配，不能把旧 A 源 ID 发到 B

## 首页加载慢 / 媒体库不全

- 默认搜索宽恕期 3 秒——收到第一个服务器的结果后，最多再等 3 秒让其余服务器响应；迟到结果只在后台登记映射/实例，不会补写这次已返回的列表
- 如果上游服务器网络延迟普遍较高，可在管理面板「全局设置」或 `config.yaml` 的 `timeouts` 中调大 `searchGracePeriod`、`metadataGracePeriod`
- `latestGracePeriod` 默认为 0（等待全部服务器），如首页"最新添加"加载慢可设为正数
- 查看日志中 `timeout` 或 `abort` 关键词
- 也可适当调大 `api`（单次请求超时）和 `global`（聚合总超时）值

## 忘记管理员密码

管理员使用不可逆 scrypt 哈希，不能查询明文。使用[停服后的 SSH 菜单或二进制 CLI](operations.md#管理员密码重置)重置，并检查客户端重新登录和服务恢复。不把手工改配置当作相同的 token 清理流程。

## 反向代理用户登录被限速 (429)

如果所有用户在 5 次登录失败后都收到 `429 Too Many Requests`，可能是来源 IP 被合并到反代地址；先确认日志中的来源与实际入口。只有服务仅经可信反代到达时才开启 trustProxy：
1. 在 `config.yaml` 的 `server` 段添加 `trustProxy: true`
2. 重启服务
3. 确认反向代理**覆写**了 `X-Real-IP` 或 `X-Forwarded-For` 头——用追加语义的 `$proxy_add_x_forwarded_for` 会让客户端可以伪造 IP（既能绕过限流，也能定向锁死他人），详见[反向代理信任](configuration.md#反向代理信任-trustproxy)

## Docker 容器无法访问上游服务器

- 检查上游 URL 是否使用了 `localhost` → 容器内 localhost 指向容器本身，应改为宿主机 IP 或域名
- 如需访问宿主机服务，使用 `host.docker.internal`（Docker Desktop）或宿主机实际 IP

## 新安装没有媒体

先检查上游是否添加且认证有效，再确认普通用户有显式授权。无授权不是“默认全来源”。passthrough 待身份采集按前面的登录步骤处理；初始流程见[首次使用](getting-started.md)。

## 统计为零或返回 503

无授权来源或全部明确离线可以返回三项 0；任一授权在线来源缺完整缓存则返回整次 503。重启后内存缓存需要重新准备，客户端反复点击不触发即时采集。不要通过伪造部分总数规避，见[统计接口](media-counts.md)。CapyPlayer 统计 503 专项仍不属于已完成的全部兼容结论，见[验证范围](release-v1.4.9-validation.md)。

## 更新后因旧 schema 或缺密钥无法启动

先保留当前目录及备份，核对原版本、实际 dataDir、mappings.db 和 user-password.key 是否来自同一实例。V1.4.6 及更早旧用户表没有 password_secret，不支持自动无损迁移。已有加密记录但密钥缺失也会拒绝初始化；重新生成密钥不能解开旧记录。按[版本升级与恢复](operations.md#版本升级)处理，不把删除数据库当作通用修复。

## 容量满或第二设备不能播放

409 UPSTREAM_CAPACITY_FULL 是普通用户授权容量；429 PLAYBACK_DEVICE_LIMIT 是该用户在该上游的活跃设备 lease。停播或禁用用户不会自动释放已授予服务器的授权名额。具体释放、心跳与精确停止见[用户与权限](users-and-permissions.md)。

## 清理未完成

管理返回 cleanupPending 不代表已经完整成功或完全回滚；部分普通用户访问会暂停。管理员按[生命周期恢复](users-and-permissions.md#管理变更与恢复)重试对应管理操作；持久清理恢复失败可能导致启动停止，不臆造不存在的后台重试器。

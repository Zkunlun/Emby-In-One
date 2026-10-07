# 媒体库统计接口 / Media library counts API

[项目主页](../README.md) · [文档索引](README.md) · [English](en/media-counts.md)

适用主线：V1.4.9。

> V1.4.9 提供以下统计接口；Hills 携带语言参数的请求已完成兼容修复及用户验收。

客户端通过已认证的 `GET /Items/Counts`（兼容 `/emby/Items/Counts`）读取统计。
成功只返回三个非负整数：`MovieCount`、`SeriesCount`、`EpisodeCount`。
普通用户按当前仍有效的服务器绑定与 Token 授权交集累计，管理员按全部当前配置服务器累计。
只有在线服务器参与；首页隐藏的库仍包含在其所属服务器的统计内。

每个服务器共用一份内存缓存，统计来自配置上游账号可见媒体范围的官方 Counts 接口。
不同服务器上的重复影片逐源累计，不做去重、不输出第二组版本数，也不扫描媒体条目补数。
这与媒体列表的合并展示口径不同。Hills 的六个显示位置不会让接口产生六种独立统计口径；带或不带 UserId 的请求仍返回同一范围的三个字段。

后台在启动准备后初始化，每小时刷新。用户绑定实际改变会登记当前所选在线服务器的额外刷新；
同源请求合并，全局最多两个采集 worker。客户端查看只读取缓存，不触发刷新、登录或探测，没有手动刷新接口。
完整新成功值覆盖旧值；同账号的失败或限流不按缓存年龄清除旧成功值。
删除服务器、改变账号或连接口径会隔离旧值；重新启动后缓存从空开始。

Counts 普通失败后，后台先用同一上游 API/身份入口检查在线情况；确认在线才最多追加一次 Counts。
明确限流或等待时不在该轮追加 Counts，并遵守 `Retry-After`；没有有效等待信息的 429 使用退避等待。
这只约束统计刷新，不暂停客户端播放或其他既有业务；有权在线服务器没有完整当前缓存时返回整份 503。

| 状态 | 客户端结果 |
| --- | --- |
| 有权在线服务器都有完整当前缓存 | 200，三项逐源总数 |
| 无绑定或全部有权服务器明确离线 | 200，三项均为 0 |
| 任一有权在线服务器缺完整缓存 | 503 `COUNTS_UNAVAILABLE`，没有部分计数 |
| 生命周期清理/恢复未完成 | 503 `COUNTS_LIFECYCLE_PENDING` |
| 未认证、Token 失效、用户删除或禁用 | 401 |
| 指定其他用户或上游真实 UserId | 403 `COUNTS_USER_FORBIDDEN` |
| 畸形、重复参数或空 UserId | 400 `INVALID_COUNTS_QUERY` |
| 任意未支持过滤或选源参数 | 400 `COUNTS_FILTER_UNSUPPORTED` |

`UserId` 可以省略，或使用当前本地用户 ID / 兼容旧别名；旧别名仍表示当前 Token 用户。
参数名大小写不敏感，重复的同族参数即使值相同也拒绝。
`IsFavorite=true`、`false` 或空值均不支持；已观看、可续播、库选择、分页和任意额外参数也不支持，
不会悄悄返回总数代替过滤结果。允许的身份元数据（包括 Hills 使用的 `X-Emby-Language`）只作本地消费，不转发或影响统计范围。

`HEAD` 执行相同校验并返回对应状态/JSON 长度，响应体为空；其他方法（含 OPTIONS）认证后返回 405，
`Allow: GET, HEAD`。所有统计结果使用 `Cache-Control: private, no-store`，不发送 ETag/Last-Modified，不返回条件 304。
本地结果不暴露上游凭据、服务器列表、原始错误正文或部分总数。
详见[媒体库统计接口说明](media-counts.md)与[发布验证说明](release-v1.4.9-validation.md)。

## English summary

Read the [full English documentation](en/media-counts.md) for complete instructions, behavior and limits.

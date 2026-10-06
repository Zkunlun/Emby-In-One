# 媒体库统计接口 / Media library counts API

适用版本：V1.4.9。需登录后读取 `GET /Items/Counts` 或 `GET /emby/Items/Counts`。成功响应示例：

```json
{"MovieCount":120,"SeriesCount":30,"EpisodeCount":450}
```

数字仅为示例。接口只提供这三个非负整数字段，按有权在线上游逐源累计官方Counts，不去重、不扫描全库、不输出另一组包含版本/去除版本数。

普通用户范围为当前有效绑定与Token授权交集；管理员范围为当前配置的全部服务器。首页隐藏媒体库不会改变统计范围。

## 参数和错误

`UserId`可省略，或指定当前本地用户ID及其兼容旧别名。另一个用户或上游真实UserId返回403。
参数名不区分大小写，同族重复参数即使值相同也返回400。`X-Emby-Language`作为身份元数据被接受，不影响数字、不转发到统计采集；Hills语言参数兼容问题已修复。

`Fields`、`IncludeItemTypes`、`ServerId`、分页、收藏/已观看/可续播过滤等未支持参数返回400，不用全库总数代替过滤结果。

| 情况 | 响应 |
| --- | --- |
| 有权在线来源都有当前完整缓存 | 200，逐源累加三个字段 |
| 无权限来源或全部明确离线 | 200，三个0 |
| 任一有权在线来源无完整缓存 | 503 COUNTS_UNAVAILABLE，无部分总数 |
| 生命周期清理/恢复未完成 | 503 COUNTS_LIFECYCLE_PENDING |
| 缺认证或用户失效 | 401 |
| 请求其他用户 | 403 COUNTS_USER_FORBIDDEN |
| 畸形、重复或空UserId | 400 INVALID_COUNTS_QUERY |
| 未支持过滤、选源等参数 | 400 COUNTS_FILTER_UNSUPPORTED |

`HEAD`进行相同校验，返回状态和JSON长度，响应体为空。其他方法认证后返回405，`Allow: GET, HEAD`。结果使用`Cache-Control: private, no-store`，不返回条件304。

## 缓存与客户端显示

后台初始化并每小时刷新，实际绑定变更登记额外刷新；同源请求合并，最多两个采集worker。客户端读取只用缓存，不触发采集、登录、探测或手动刷新。

同账号采集失败不按缓存年龄删除旧成功值；改变账号或连接范围会隔离旧值。缓存仅在内存中保存，重启需要重新准备。明确限流遵守Retry-After；普通失败在线确认后最多追加一次Counts。

Hills会发起带/不带UserId的两次请求，两个成功响应各包含上述三个字段。界面六个显示位置不等于六个独立统计指标；当前两次响应没有分别计算“去重”和“包含版本”的两套数值。

## English summary

Authenticated GET/HEAD requests return MovieCount, SeriesCount and EpisodeCount from shared in-memory snapshots of authorized online servers. Counts sum upstream official values, including duplicates across servers; no second version-count group or full-library traversal is implemented.

UserId may be omitted or identify the authenticated local user. Hills' X-Emby-Language is accepted as locally consumed metadata. Duplicate, malformed and unsupported query parameters remain rejected. Client reads never trigger refreshes. Missing complete snapshots for any authorized online server produce a whole-response 503, while an empty or explicitly offline scope yields three zeros.

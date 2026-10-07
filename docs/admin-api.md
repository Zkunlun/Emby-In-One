# 管理 API 参考

[项目主页](../README.md) · [文档索引](README.md) · [English](en/admin-api.md)

适用主线：V1.4.9。

## 认证与请求方式

管理面板使用本地管理员账户通过 `POST /Users/AuthenticateByName` 登录，返回 `AccessToken`；不要用上游 API Key 代替 EIO 本地 token。

以下管理接口除 logout 外要求本地管理员。`POST /admin/api/logout` 在当前路由中使用已认证用户守卫，可撤销当前用户自己的 token，不是其他管理操作的授权入口。

请求可通过 `X-Emby-Token` 头或 `api_key` 查询参数携带 EIO token，优先使用头以减少 URL 中暴露凭据。JSON 请求使用 `Content-Type: application/json`。`/admin/api/*` 不为任意跨域来源放行；面板同源调用。

`{id}` 为实际 ID 占位符，不连同花括号发送。上游使用稳定 serverId；当前部分接口兼容旧索引定位，新调用使用稳定 ID。用户 ID 与网络代理 ID 从相应列表响应取得。

## 接口清单

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| POST | `/admin/api/logout` | 撤销当前代理 token（包括相关捕获）；此路由使用普通认证守卫 |
| GET | `/admin/api/client-info` | 读取已捕获的客户端身份 |
| GET | `/admin/api/status` | 系统状态 |
| GET | `/admin/api/upstream` | 列出上游及其状态/容量 |
| POST | `/admin/api/upstream` | 添加并验证上游 |
| PUT | `/admin/api/upstream/{id}` | 修改指定上游 |
| POST | `/admin/api/upstream/reorder` | 调整服务器顺序 |
| DELETE | `/admin/api/upstream/{id}` | 删除指定上游并按实例/生命周期规则清理 |
| POST | `/admin/api/upstream/{id}/reconnect` | 重连上游 |
| GET | `/admin/api/proxies` | 列出网络代理 |
| POST | `/admin/api/proxies` | 添加网络代理 |
| POST | `/admin/api/proxies/test` | 代理连通性测试（结果还需检查 success） |
| DELETE | `/admin/api/proxies/{id}` | 删除网络代理 |
| GET | `/admin/api/settings` | 读取面板支持的全局设置 |
| PUT | `/admin/api/settings` | 更新面板支持的全局设置 |
| GET | `/admin/api/logs` | 读取有界内存日志 |
| GET | `/admin/api/logs/download` | 下载持久化日志 |
| DELETE | `/admin/api/logs` | 清空应用日志及轮转备份 |
| GET | `/admin/api/users` | 列出普通用户 |
| POST | `/admin/api/users` | 创建普通用户 |
| PUT | `/admin/api/users/{id}` | 更新普通用户、授权或隐藏库设置 |
| DELETE | `/admin/api/users/{id}` | 删除普通用户并清理其状态 |
| GET | `/admin/api/upstream/{id}/libraries` | 读取指定上游媒体库用于配置 |
| GET | `/admin/api/home-libraries` | 读取管理员自身的库入口隐藏配置 |
| PUT | `/admin/api/home-libraries` | 更新管理员自身的库入口隐藏配置 |

日志读取的 `limit` 示例为 `/admin/api/logs?limit=500`。不要将内存日志数量等同于磁盘日志保留范围。

## 上游与全局设置

创建/编辑上游的字段遵循[配置参考](configuration.md)与[认证和播放规则](playback-and-upstream.md)。面板请求的 `authType` 区分 `apiKey` 和 `password`；非空 Key 与用户名认证互斥，用户名认证允许密码为空（用户名必填）。`maxConcurrent` 必须为非负数，redirect 不允许同时有 `proxyId`。

passthrough 尚无可用身份时，保存可成功但响应有 warning，节点保持 offline 待客户端登录采集；不能只看 HTTP 成功就当作上游认证完成。修改管理员用户名/密码需按设置接口提供 currentPassword；改密撤销全部代理 token。

## 用户与容量

创建用户的 `allowedServers` 省略、null 或 [] 均不给授权。更新时省略或 null 保留，[] 清空。password 更新省略或 null 保留，显式空字符串清空。容量满与容量下调冲突返回 409；字段和代码见[用户与权限](users-and-permissions.md#同播数量限制与授权容量)。

生命周期提交后清理未完成可能返回 `cleanupPending`；必须按[恢复说明](users-and-permissions.md#管理变更与恢复)处理，不能自动认为失败后所有状态都已回滚。

## 库入口与显示配置

`GET /admin/api/upstream/{id}/libraries` 获取配置所需库；`GET/PUT /admin/api/home-libraries` 处理管理员自身的入口隐藏，不是对所有普通用户施加全局隐藏。普通用户的 hiddenLibraries 由用户接口管理。显示隐藏不会撤销访问授权，详见[用户与权限](users-and-permissions.md#首页媒体库隐藏)。

## 错误与完成判定

缺失/失效本地认证或无管理员权限时先由认证守卫拒绝。请求格式、输入校验、未知资源、上游验证和持久化失败分别可返回相应 4xx/5xx；具体 body 以接口为准，不假设所有错误使用相同 code 结构。

代理测试即使 HTTP 200 也可能 `success: false`。上游保存后的 warning、409 容量错误和 cleanupPending 分别表示不同情况；自动化调用应检查状态和响应字段。

媒体库 Counts 属于客户端接口，文档见[媒体库统计](media-counts.md)，不混入管理 API 清单。

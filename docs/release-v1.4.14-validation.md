# V1.4.14 发布验证范围

## 复现与根因

2026-10-29 VividPlayer 请求部分终点站 CollectionFolder 详情出现 HTTP 502。diagnostic2 显示 EIO 以 GUID 请求详情，上游却返回属于另一媒体库的数字 Id（例如 12、8、7）；这些数字中部分已经独立映射，直接无条件放过 Id 不匹配会引入错误关联。修复采用原上游用户 /Views 的逐项精确验证，不接受错配详情，不更改现有映射或授权逻辑。

## 回归与实机验收

- 测试：TestUserItemDetailCollectionFolderGUID、TestUserItemDetailMismatchedIDsRequireVerifiedView（完全匹配、错误视图、错误类型、普通电影错配）以及其他详情路由相关用例。发布前再次执行最新清理版测试；CI 全量 Go build/vet/test、面板资源合同与多架构 Release 以 GitHub Actions 记录为准。
- DMIT 已经在 v1.4.13-vivid-test 进行真实 VividPlayer 验收：终点站媒体库列表打开正常、SNTP 和普通媒体详情未回归；同一客户端终点站与 SNTP 视频恢复播放。用户明确确认两项验收通过。
- 该测试版二进制含临时诊断日志，正式版源码删除临时诊断内容并保留兼容代码与测试；正式版本上线后仍应进行基本 HTTP、服务、上游在线状态验证，真实客户端验收只依据上述已确认测试版而不是推测。

## nginx TLS 配置问题单独处理

- 故障在 MMWX 生成的 nginx TLS1.2 ciphers 与 RSA 证书组合，不是 EIO 播放路由；Windows curl TLS1.3 为200，TLS1.2 握手失败，WinHTTP 12175。追加 ECDHE-RSA 套件后 TLS1.2、TLS1.3、WinHTTP 均返回200；无凭据视频路径返回预期401，证明请求进入应用鉴权层。
- DMIT 的 eio-tls12-compat.path/.service 监视 /usr/local/nginx/servers，脚本仅针对 emby-in-one.zkunluncloud.com 的已知生成模板修复；成功后重载 nginx，语法检查失败则恢复文件。手动模拟 MMWX 覆盖后自动复原已通过。
- 当前方案是针对既有 MMWX 模板问题的本地持久化兼容层，MWWX 后续版本如默认加入 ECDHE-RSA 应下线此附加服务。

## 约束

不使用任何实机媒体库 GUID 或数字 ID 写进产品逻辑；不修改 SQLite 数据、用户密码、上游鉴权/UA、播放直连或代理策略；不合并 PR #3。正式二进制 Release 和源代码同步后才能宣称发布完成。
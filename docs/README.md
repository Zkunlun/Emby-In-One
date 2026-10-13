# 文档索引

[项目主页](../README.md) · [安全政策](../SECURITY.md) · [English](en/README.md)

适用主线：V1.4.9。

## 安装与首次使用

- [安装指南](installation.md)：推荐 Release、固定版本、Docker/Compose 和源码部署。
- [首次使用](getting-started.md)：本地管理员、上游接入、普通用户授权和客户端验证。

## 配置与日常管理

- [配置参考](configuration.md)：YAML、默认值、超时、可信反代和数据目录。
- [用户与权限](users-and-permissions.md)：授权、容量、单设备播放、观看状态及重绑。
- [上游接入与播放](playback-and-upstream.md)：认证、身份模式、proxy/redirect 和备用线路。
- [运维指南](operations.md)：启停、升级、SSH 菜单、备份、日志和密码重置。
- [管理 API](admin-api.md)：认证、路由清单和调用边界。
- [安全政策](../SECURITY.md)：风险、凭据存储与漏洞报告。

## 行为规则

- [媒体合并](media-merge.md)：作品身份、多版本、虚拟 ID 与分页边界。
- [媒体库统计](media-counts.md)：逐源 Counts、缓存及错误响应。

## 排障与开发

- [常见问题排查](troubleshooting.md)：离线、403/401、身份采集、429、Docker 网络及加载问题。
- [开发与贡献](development.md)：模块职责、构建和验证入口、反馈要求。

## 发布与历史

- [更新日志](../Update.md)与 [Releases](https://github.com/Zkunlun/Emby-In-One/releases)。
- [V1.4.9 验证范围](release-v1.4.9-validation.md)：已有证据及未覆盖事项。
- [版本编号调整](version-numbering.md)：历史 V1.5.0/V1.5.1/V1.6.0 与当前编号关系。
- [更新计划](../Update%20Plan.md)：既有维护计划，实际已发布功能以 Release 和当前文档为准。
- [旧 Node.js V1.2.1 文档](../README_V1.2.1.md)及 [legacy 说明](../legacy/README.md)：历史参考，不参与当前 Go 构建。

上述操作文档为中文。完整对应的英文文档见 [English documentation](en/README.md)，英文项目入口为 [English README](../README_EN.md)。每份专题页提供同名语言切换入口。

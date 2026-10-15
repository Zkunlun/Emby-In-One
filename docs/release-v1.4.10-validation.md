# V1.4.10 验证范围 / Validation scope

[项目主页](../README.md) · [文档索引](README.md) · [English](en/release-v1.4.10-validation.md)

本页记录 **V1.4.10 的验证证据及边界**。正式发布状态、日期和下载资产以 GitHub Releases 为准。

## 基线

基于 GitHub main 963bbf4e7d4c8a9c62d79bf55ded6c5c2a9e0050，整合 Phase8 十个本地提交（9B HEAD 9c08819cd0066fdadb5cab6a4b55568432536413）。所有后端源码与用户已验收的 Phase8 候选逐字节一致，双语文档更新得以保留。

## 已通过

- Phase8 候选经 DMIT 基础健康检查，用户于2026-10-15确认真实 Emby 客户端验收通过，但这不代表每条上游 API 均独立抓包验证。
- Windows/amd64 同 Git 文件树上，36项 Phase8/SNTP 定向测试通过，cmd 测试、go vet、go build 均成功。
- 保留所有43个后端源码变更，不恢复已撤回的临时工作流。

## 跳过或未验证

- 用户明确允许跳过9B3完整后端回归，不能将其标为通过。Windows 后端尝试300秒超时：488项通过、2项失败、5项跳过，剩余未跑完。
- 两项失败：TestUnifiedCountsPendingCleanupAndRecoveryPublishOnce 与 TestUnifiedTokenPersistenceFailureStaysPendingAndRecoversOnce，模拟 Token 保存失败时预期HTTP500，Windows返回200；Linux上是否同样存在问题尚未验证。
- GitHub Actions PR CI #33 已通过 Linux Go build/vet/完整普通测试、面板与 Shell 检查；全量 race 按配置跳过。六架构发布构建、静态链接、二进制版本及 SHA256 仍须由正式 Release 工作流验证。
- Redirect全部线路Dead时保守返回502；匿名404探测不证明可播。上游登录失败日志仍有未确定根因的历史记录。
- 不包含已撤回的V1.5.0全库扫描器；SNTP按需补全不同于全库扫描。

## 升级

保留配置、Token、user-password.key及SQLite/WAL一致性回滚快照。正式版本及下载地址以GitHub Releases为准。

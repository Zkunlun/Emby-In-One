# 运维指南

[项目主页](../README.md) · [文档索引](README.md) · [English](en/operations.md)

适用主线：V1.4.9。

## 日常管理与服务状态

SSH 菜单：

安装脚本执行完成后，可直接使用：

```bash
emby-in-one
```

可执行：

- 启动 / 重启 / 停止服务
- 在线更新（最新版）/ 下载指定版本
- 查看服务状态、公网 IP
- 查看管理员用户名/密码存储状态、修改用户名或重置密码（哈希后不能查回明文）
- 查看用户列表、添加普通用户、删除普通用户
- 查看日志
- 卸载服务（支持保留配置和数据）

> SSH 菜单自动检测当前部署方式（Binary / Docker），所有操作自动分发到 systemd 或 Docker Compose 对应命令。Docker 模式下更新采用源码重建流程。菜单没有单独的「查看版本」选项——当前版本号直接显示在菜单标题栏上（当前发布为 V1.4.9）。

`/usr/local/bin/emby-in-one` 是安装后的 SSH 菜单脚本；默认 Release 可执行文件是 `/opt/emby-in-one/emby-in-one`。同名不等于同一入口。查看实际二进制版本可在项目目录执行 `./emby-in-one --version`。

Release 使用 `sudo systemctl status emby-in-one` 查看状态，以 `start`、`stop`、`restart` 管理；终端日志用 `sudo journalctl -u emby-in-one -f`。Compose 在项目根目录用 `docker compose ps`、`stop`、`up -d`、`logs -f --tail 50`；网络/端口实际情况另行检查。

## 版本升级

先确认来源版本和部署方式，再按[运行数据与备份](#运行数据与备份)保留一致备份。SSH 菜单可在线更新或下载指定版本；Binary 路线更新 Release，Docker 路线使用对应发布源码包重建。

| 来源 | 数据处理要求 |
| --- | --- |
| V1.4.8 → V1.4.9 | 保留配置、数据库、token 和 user-password.key 等配套数据；升级前备份 |
| 原 V1.5.0/V1.5.1/V1.6.0 → 对应 V1.4.7/V1.4.8/V1.4.9 | 同功能重编号，按[编号说明](version-numbering.md)处理；不因改名要求清空数据 |
| V1.4.6 及更早的旧 users schema | 不支持自动无损迁移；旧 hash 无法生成 password_secret。已有普通用户数据先备份，再使用干净数据目录并重建普通用户，见[既有发布说明](../Update.md#升级注意事项) |

不确定 schema 时先停服检查已有版本和备份，不把指向旧 dataDir 当作迁移步骤。干净目录升级会重新建立数据，不保证旧观看历史无损转换；原目录和备份应保留，不能直接删除以绕过启动错误。

release-install.sh 升级时备份旧二进制和原 systemd unit，并保留原 config/data/log；失败按其流程恢复相关文件、服务及属主。它不是整目录/数据库时间点快照，不能代替下面的备份或保证任意旧 schema 可启动。可选磁盘面板资源和 CLI 也不等于完整数据回滚。

## 运行数据与备份

config/config.yaml 保存本地管理员哈希、上游明文凭据和配置。实际 dataDir 决定其余运行文件的位置，见[配置参考](configuration.md#数据目录-datadir)。

| 文件 | 用途与备份注意 |
| --- | --- |
| mappings.db | SQLite：虚拟 ID、实例、用户/授权版本、观看状态及待清理日志 |
| mappings.db-wal / mappings.db-shm（若存在） | SQLite WAL 相关文件；不要在线仅拷主 db，停服后整体备份 dataDir |
| user-password.key | 普通用户 AES-GCM 密钥，需与数据库配套保存；已有加密密码时丢失会拒绝初始化 |
| tokens.json | EIO 代理 token 与授权版本；认证还核验当前用户状态 |
| captured-headers.json | passthrough 身份缓存，含稳定来源归属信息 |
| emby-in-one.log 及轮转文件 | 应用日志，可能包含需要保护的 URL/头信息 |

默认 Release 项目目录含 config/data/log，但应用文件日志实际在 dataDir 内，不能只备份 log 子目录。源码与非默认路径按自身配置确定，不假设一定是 /opt/emby-in-one。

### 一致备份

1. 保存正在使用的版本/部署方式和配置，确认实际 dataDir。停止 systemd 服务或对应 Compose 服务，确保没有其他 EIO 进程继续写入。
2. 把 config、完整 dataDir 及需要的部署文件备份到受限目录。目录与文件权限、服务属主一并记录；密钥和明文上游凭据不应公开上传。
3. 检查备份是否可读取，至少确认配置、mappings.db 和 user-password.key 已配套保存；启动原服务并检查状态。

默认 Release 路径的示例（实际自定义 dataDir 时调整备份范围）：

```bash
sudo systemctl stop emby-in-one
sudo install -d -m 700 /root/eio-backups
eio_backup="/root/eio-backups/eio-$(date +%Y%m%d-%H%M%S).tar.gz"
sudo tar -C /opt/emby-in-one -czf "$eio_backup" config data log
sudo chmod 600 "$eio_backup"
sudo tar -tzf "$eio_backup"
sudo systemctl start emby-in-one
```

逐步执行并确认前一步成功；备份命令失败时不要开始升级。Compose 的宿主 config/data 同样在停容器后整体备份，不把以上 systemd 命令直接套到 Docker。

### 恢复

停服务，保留当前现场后，从同一份备份恢复配套的配置与完整 dataDir；根据备份选择兼容版本，恢复正确服务属主和文件权限，确认工作目录/挂载路径，再启动验证。不要混合不同时刻的 db、密钥与 token 文件。缺失密钥不能用重新生成一把密钥恢复已有加密密码。

## 管理员密码重置

优先运行 `emby-in-one` SSH 菜单，选择修改管理员密码：菜单先停服务，调用真正二进制的 reset-password，再恢复属主并启动。检查最终服务状态，不能只依赖菜单完成提示。

二进制语法（说明占位符，不直接执行整行）：`./emby-in-one --reset-password <new-password|-> [--force]`。默认 Release 手动重置在 `/opt/emby-in-one` 工作目录使用 `sudo ./emby-in-one --reset-password -`，输入经 stdin 提供；不要运行 SSH 菜单脚本 `emby-in-one --reset-password ...`。

先停 `emby-in-one` 服务；CLI 尝试连接 `127.0.0.1:<配置端口>` 的 TCP，成功则拒绝，不是 HTTP /System/Info/Public 检查。`--force` 仅跳过 TCP 检测，不证明实例已停，也不应作为默认重置参数。

使用 stdin 避免把密码作为命令行参数写入进程列表/历史。密码为 `-` 时从 stdin 读取；终端或调用脚本应安全地提供内容，例如菜单用 `printf '%s' "$new_password" | ./emby-in-one --reset-password -`，不要把真实密码写入示例代码。

Compose 手工重置先停原服务，在原项目目录通过一次性容器执行 `docker compose run --rm --no-deps -T emby-in-one /app/emby-in-one --reset-password -`，stdin 提供密码；完成后 `docker compose up -d` 并验证。其配置/data 挂载必须与原服务相同。只有确认停服且遇到探测冲突时才评估 --force。

CLI 保存新 scrypt 哈希，并原子清空已签发代理 token（保留 _proxyUserId）；所有 EIO 客户端需重新登录。在线实例持有旧 token 内存，后续写回可覆盖清理，所以必须先停服。

手动把配置哈希改成明文并重启虽会重新哈希，但不等同于 CLI 的 token 清理流程；遗忘密码优先用菜单/CLI。正常记得当前密码时可在面板设置更新，需 currentPassword，见[管理 API](admin-api.md)。

## 周期检查与推流线路状态

默认周期为 60 秒，由 timeouts.healthInterval 控制。每轮区分两个对象：

- 在线 API 来源：并发调用推流线路探测，维护线路 unknown/alive/dead；不对全部在线 API 来源定时并行 GET /System/Info/Public。
- 离线 API 来源：按循环逐个尝试重新认证，使用用户名登录或 Key 的 /Users/Me 校验，并受 healthCheck/login/api 超时约束。用户名型 passthrough 没有捕获身份时跳过，等待客户端登录采集。

推流线路状态独立于 API 状态；API 在线不保证每条专用推流 URL 可用。多线路探测的 transport error、502/503/504 记不可用，404/500 等可达响应不等于线路网络故障。Reload 保留未变化推流 URL 的已知状态；dead 不因时间自动变 alive，恢复条件见[播放线路](playback-and-upstream.md#播放模式详解)。

启动/重载/登录可触发在线线路即时探测，状态变化记录日志。关机取消周期定时器。Docker 的容器 HEALTHCHECK 检查 EIO 自身公开端点，与上述上游认证和线路状态是三种不同检查。

## 日志级别

| 级别 | 常见内容 |
| --- | --- |
| DEBUG | 请求详情、ID 解析、身份信息 |
| INFO | 登录、状态、配置变更 |
| WARN | 上游拒绝、掉线和保留边界 |
| ERROR | 请求、登录或持久化失败 |

终端阈值由 LOG_LEVEL 决定，文件阈值由 FILE_LOG_LEVEL 决定，两者默认均为 info。默认不会把所有 DEBUG 写进文件；需显式开启。输出是否出现取决于阈值，不是级别固定绑定文件或终端。

## 日志文件

- 路径：`data/emby-in-one.log`（Release 部署在 `/opt/emby-in-one/data/`）
- Docker 路径：`/app/data/emby-in-one.log`
- 单文件最大 10 MiB，保留 3 个备份（`emby-in-one.log.1` ~ `.3`），自动轮转
- 管理面板可下载和清空（清空会连同备份一起删除）

## 日志配置

默认日志级别为 `info`。排查故障时通过环境变量开启完整调试日志：

```bash
LOG_LEVEL=debug FILE_LOG_LEVEL=debug ./emby-in-one
```

Docker Compose 中设置：

```yaml
environment:
  - LOG_LEVEL=debug
  - FILE_LOG_LEVEL=debug
  - LOG_MAX_SIZE_MB=10   # 单个日志文件上限（MB），默认 10
  - LOG_KEEP=3           # 保留的轮转备份数，默认 3
```

LOG_LEVEL 控制终端阈值，FILE_LOG_LEVEL 控制磁盘阈值；管理面板的有界内存日志缓冲保留各级别，不按这两个输出阈值筛掉 DEBUG。源码运行示例需在完整项目工作目录执行。systemd 设置环境变量时使用对应 unit 的 Environment 或 drop-in，保存后重载并重启；不要期待在另一 shell 中设置变量会改变已经运行的服务。配置环境前可通过 `systemctl cat emby-in-one` 核查实际 unit。

日志用于诊断，不作为完整网络审计证明；分享前移除上游 URL 中的凭据及身份信息。安全机制与使用风险见[安全政策](../SECURITY.md)。

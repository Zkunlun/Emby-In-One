# 安装指南

[项目主页](../README.md) · [文档索引](README.md) · [English](en/installation.md)

适用主线：V1.4.9。

## 选择部署方式

| 方式 | 适合场景 | 要求 |
| --- | --- | --- |
| Release 二进制 + systemd（推荐） | Linux 日常运行 | Linux、systemd、root/sudo、Bash、curl、grep、sed、sha256sum 等脚本工具；能访问 GitHub API 与 Release 下载 |
| 源码安装脚本 / Docker Compose | 希望自行构建容器 | Docker 20.10+、Compose v2、完整源码及构建网络 |
| Go 源码运行 | 开发与调试 | Go 1.23+、C 编译链（CGO SQLite）、完整源码 |

Release 提供 amd64、arm64、arm、mips、mipsle、riscv64 六种 Linux 架构，无需本地 Go 环境。Docker 的 Linux 推荐环境为 Debian 11/12/13、Ubuntu 22/24；其他发行版需自行验证。Windows/macOS 的 Docker 主要作为开发测试方式。

已经安装的实例先阅读[升级与备份](operations.md#版本升级)。下面的首次安装说明不承诺任意旧数据库都可覆盖升级。

## Release 二进制首次安装

```bash
curl -fsSL -o release-install.sh https://github.com/Zkunlun/Emby-In-One/releases/latest/download/release-install.sh
sudo bash release-install.sh
```

脚本从最新正式 Release 下载；不传版本时会再查询 GitHub Latest。脚本下载与版本解析是两个动作，不是固定版本安装。

脚本检测 CPU 架构、下载二进制，初始化 `/opt/emby-in-one/{config,data,log}`，首次生成 admin 随机密码，安装并启动 systemd 的 `emby-in-one` 服务，服务账户为 eio。结束时显示初始密码，请保存。已哈希的管理员密码不能事后查询。

二进制内嵌完整管理面板，包括 public/vendor 依赖。磁盘上可选的面板文件优先覆盖，缺少时按文件回退到内嵌副本；CLI 是另外的管理脚本。Release 提供配套 SHA256 文件；脚本在校验文件下载不到时会警告并跳过校验，不能据此声称安装始终缺校验即拒绝。

默认面板 `http://服务器IP:8096/admin`，客户端地址 `http://服务器IP:8096`。首次无上游，接下来执行[首次使用](getting-started.md)。

## 指定 Release 版本

固定版本示例为 V1.4.9：

```bash
curl -fsSL -o release-install.sh https://github.com/Zkunlun/Emby-In-One/releases/download/V1.4.9/release-install.sh
sudo bash release-install.sh V1.4.9
```

URL 与参数需对应同一版本；仅固定脚本 URL、不传参数仍会解析 Latest。原 V1.5.0/V1.5.1/V1.6.0 地址已调整，参见[编号说明](version-numbering.md)。

## 源码安装脚本

```bash
git clone --branch V1.4.9 https://github.com/Zkunlun/Emby-In-One.git
cd Emby-In-One
sudo bash install.sh
```

此路线执行仓库的 Docker 安装脚本，处理 Docker 环境、挂载目录权限、随机管理员密码和源码镜像构建。安装后通过 SSH 中的 `emby-in-one` 菜单管理。开发当前主线时可自行选择 main，不能把未发布源码与稳定 Release 等同。

## 手动 Docker Compose

1. 克隆完整仓库（可使用上面的 V1.4.9 克隆命令）。必须包含 go.mod、cmd、internal、third_party、public、Dockerfile 和 docker-compose.yml；不能只复制几个 Go 文件。
2. 在仓库根目录创建挂载目录，交给 Compose 的 uid/gid 1000：

   ```bash
   mkdir -p config data
   sudo chown -R 1000:1000 config data
   ```

3. 按[首次启动最小配置](configuration.md#首次启动最小配置)创建 `config/config.yaml`，替换示例管理员密码，确认 uid 1000 可读且可写配置与 data。已有配置或数据不要用初始模板覆盖。
4. 在仓库根目录构建并启动：

   ```bash
   docker compose build
   docker compose up -d
   ```

仓库 Dockerfile 自行构建镜像，本指南不提供官方预构建镜像地址。容器读取 `/app/config/config.yaml`、使用 `/app/data`；Compose 把宿主 ./config 和 ./data 挂载到这些位置。目录或配置权限不匹配会导致配置保存、tokens、密钥或 SQLite 写入失败。

Dockerfile 的 HEALTHCHECK 请求容器自身 `http://127.0.0.1:8096/System/Info/Public`。它不是 EIO 对上游的周期检查；更改容器服务端口后，需同步调整端口映射和 HEALTHCHECK。

## Go 源码运行

在完整仓库中准备 Go 1.23+ 和 C 编译链，Debian/Ubuntu 的 C 工具可通过 `apt install build-essential` 安装。SQLite 使用仓库 third_party 中的源码；public 是构建所需的内嵌资源包。

```bash
mkdir -p config data
# 按配置参考创建 config/config.yaml，并替换示例密码
go run ./cmd/emby-in-one
```

开发验证和版本构建见[开发指南](development.md)。自行修改 Dockerfile 或构建上下文时保留 public/，否则会出现 `package emby-in-one/public is not in std` 等缺包错误。

## 历史 Node.js 版本

legacy/ 保留 V1.2.1 源码供参考，不参与当前 Go 二进制或容器构建。需要历史 Node.js 部署时，参考[旧版文档](../README_V1.2.1.md)、[legacy 说明](../legacy/README.md)和[原项目 Releases](https://github.com/ArizeSky/Emby-In-One/releases)中的 V1.2.1 Source code；不要把当前 Go 的 install.sh 当作旧版安装脚本。

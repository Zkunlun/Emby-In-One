# 配置参考

[项目主页](../README.md) · [文档索引](README.md) · [English](en/configuration.md)

适用主线：V1.4.9。

配置文件位于 `config/config.yaml`（Docker 部署时挂载到容器内 `/app/config/config.yaml`）。

```yaml
# dataDir: "/opt/emby-in-one/data"    # 运行时数据目录（顶层键，默认值见下方「数据目录」说明）

server:
  port: 8096
  name: "Emby-In-One"
  # id: 首次启动自动生成，请勿手动修改
  # trustProxy: true        # 部署在反向代理后面时设为 true（见下方说明）

admin:
  username: "admin"
  password: "your-strong-password"    # 启动时转换为 scrypt 哈希；不能还原明文

playback:
  mode: "proxy"          # "proxy" 或 "redirect"，全局默认值

timeouts:
  api: 30000             # 单次上游 API 请求超时（ms）
  global: 15000          # 聚合请求总超时——等待所有服务器的最大时长（ms）
  login: 30000           # 上游登录超时（ms）——作用于登录与 API Key 校验，超过即判为登录失败
  healthCheck: 30000     # 健康检查超时（ms）——作用于离线服务器的重连探测
  healthInterval: 60000  # 健康检查间隔（ms）
  searchGracePeriod: 3000     # 搜索聚合宽恕期——收到首个成功结果后继续等待的时长（ms）
  metadataGracePeriod: 3000   # 元数据获取宽恕期（ms）
  latestGracePeriod: 0        # "最新添加"宽恕期——0 表示等待全部服务器（ms）

proxies: []
  # - id: "abc123"
  #   name: "日本代理"
  #   url: "http://user:pass@ip:port"

upstream:
  - name: "服务器A"
    url: "https://emby-a.example.com"
    username: "user"
    password: "pass"

  - name: "服务器B"
    url: "https://emby-b.example.com"
    apiKey: "your-api-key"
    playbackMode: "redirect"                   # 覆盖全局播放模式
    spoofClient: "infuse"                      # none | passthrough | infuse | hills | capyplayer | custom
    streamingUrls:                               # 推流线路（可选，有序；单条也可写 streamingUrl: "..."）
      - "https://cdn.example.com"                # 第 1 条为主线路
      - "https://backup.example.com"             # 其余为备用线路
    followRedirects: true                      # 是否跟随上游的 301/302/303/307/308（默认 true；false 时按上游错误处理，不把重定向地址转发给客户端）
    proxyId: null                              # 关联代理池中的代理 ID
    priorityMetadata: false                    # 合并时优先使用此服务器的元数据
    maxConcurrent: 3                           # 同播数量限制：普通用户授权容量，0不限；管理员不占名额

  - name: "服务器C（custom 伪装示例）"
    url: "https://emby-c.example.com"
    apiKey: "your-api-key"
    spoofClient: "custom"
    customUserAgent: "Infuse/7.7.1 (iPhone; iOS 17.4.1; Scale/3.00)"
    customClient: "Infuse"
    customClientVersion: "7.7.1"
    customDeviceName: "iPhone"
    customDeviceId: "your-custom-device-id"

  - name: "服务器D（Hills 预设）"
    url: "https://emby-d.example.com"
    apiKey: "your-api-key"
    spoofClient: "hills"

  - name: "服务器E（CapyPlayer 预设）"
    url: "https://emby-e.example.com"
    apiKey: "your-api-key"
    spoofClient: "capyplayer"
```

## 设置何时生效

| 修改入口或字段 | 生效范围 |
| --- | --- |
| 管理面板支持的上游、用户、代理、名称及超时设置 | 按对应管理接口更新运行状态，不要求为每次面板保存重启 |
| 默认播放模式 `playback.mode` | 修改后作为新增上游的初值；不会覆盖已有上游的 `playbackMode` |
| 某台上游的播放模式 | 在该上游编辑框修改，保存后作用于该上游 |
| `server.port`、`server.trustProxy`、`dataDir` 等启动项 | 面板不提供这些字段；编辑配置文件后重启 |
| 手工编辑 YAML | 不存在通用文件监听重载；重启后重新读取，避免与面板写入同时进行 |

播放模式完整规则见[上游与播放](playback-and-upstream.md#播放模式详解)。管理员改密会撤销所有代理 token；普通用户改密、禁用或修改绑定也会影响其会话，见[用户与权限](users-and-permissions.md)。

## 反向代理信任 (`trustProxy`)

| 配置值 | 行为 | 适用场景 |
|--------|------|----------|
| `false`（默认） | 登录限速使用 TCP 直连 IP（`RemoteAddr`） | 直接暴露在公网，无反向代理 |
| `true` | 登录限速信任 `X-Real-IP` / `X-Forwarded-For` 头 | 部署在 Nginx / Caddy 等反向代理之后 |

> **重要**：如果您的 Emby-In-One 部署在反向代理后面（Nginx、Caddy、Cloudflare 等），应在确认本服务仅可经可信反向代理到达后，在 `server` 段设置 `trustProxy: true`，否则所有客户端请求将被视为来自同一 IP，5 次登录失败后所有用户均会被限速 15 分钟。

> **前提：反向代理必须“覆写”这两个头**。本程序优先取 `X-Real-IP`，没有时取 `X-Forwarded-For` 的**第一段**。而 Nginx 最常见的写法 `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;` 是**追加**语义——客户端自己送的值会留在最前面，于是任何人都能伪造 IP：既可以不断换假 IP 绕过登录限流，也可以反过来把某个 IP 定向锁死 15 分钟。
>
> 推荐改成覆写：
> ```nginx
> proxy_set_header X-Real-IP $remote_addr;
> proxy_set_header X-Forwarded-For $remote_addr;
> ```
> 仅在最后一道可信入口覆写该头且客户端无法绕过入口直达 EIO 时，才可按该代理提供的来源 IP 限流；多层代理需先验证实际来源链。
>
> **反过来说：前面没有可信反向代理时，必须保持 `false`。** `trustProxy: true` 意味着服务端**无条件**采信请求头里的 `X-Real-IP` / `X-Forwarded-For`，不做任何来源校验。如果实例直接暴露在公网（或链路中没有任何一层会覆写这两个头），任何人都能自行填入任意 IP：每次登录失败换一个假 IP，就能绕过 `POST /Users/AuthenticateByName` 的失败计数与 15 分钟锁定；也可以填上别人的 IP，定向把那个 IP 锁死。
>
> 判断标准很简单：**只有当「能访问到本服务的最后一道入口必然是您自己的反向代理」时才开启它**；不能确定就不要开。

## 数据目录 (`dataDir`)

`dataDir` 是配置文件里的**顶层键**（与 `server`、`admin`、`playback` 同级），决定运行时数据的落盘位置。

| 项 | 值 |
|----|-----|
| 默认值 | 若 `/app/data` 存在（仓库 Dockerfile 创建该目录）则用 `/app/data`；否则用进程工作目录下的 `data/` |
| 落盘内容 | `mappings.db`（虚拟 ID 映射、用户数据、观看历史）、`tokens.json`（代理层 token）、`captured-headers.json`（passthrough 客户端头）、`user-password.key`（普通用户密码密钥）、`emby-in-one.log`（日志文件） |

> **该键与配置文件本身无关。** `config.yaml` 始终位于 `config/config.yaml`（Docker 容器内为 `/app/config/config.yaml`），不会随 `dataDir` 移动。各文件的说明见[运行数据与备份](operations.md#运行数据与备份)。

**什么时候需要改**：

- **二进制 / 源码部署**：`data/` 是相对**进程工作目录**解析的。如果服务的启动目录不是项目目录（例如 systemd 的 `WorkingDirectory` 指向 `/opt/emby-in-one`），而你想把数据固定到某个绝对路径、或与 `config/` 分开挂载，就显式指定 `dataDir`。
- **Docker 部署**：容器内默认即 `/app/data`，而 `docker-compose.yml` 已把宿主的 `./data` 挂载到这里，通常**不需要**改；只有自定义挂载点时才需要。
- **迁移 / 复用数据**：仅在 schema 兼容、数据库与密钥等配套文件完整时指向已有目录；旧用户库的限制见[版本升级](operations.md#版本升级)。不能只复制数据库，或承诺任意旧目录可直接复用。

**注意事项**：

- 只写在配置文件里即可（`dataDir: "/opt/emby-in-one/data"`），管理面板不提供此项，修改后需**重启服务**生效；
- 生产环境请使用**绝对路径**——相对路径会随启动时的工作目录变化，可能表现为「数据丢失」（实际是换了个目录读写）；
- 该目录需要进程用户可读写。

## 默认值与边界

所有 timeout/grace 字段以毫秒为单位。配置加载时 `api`、`login`、`healthCheck` 默认 30000，`global` 为 15000，`healthInterval` 为 60000，搜索/元数据宽恕期为 3000，最新添加宽恕期为 0。

`api`、`global`、`login`、`healthCheck`、`healthInterval` 必须为正数，管理输入的范围以当前校验器为准；宽恕期允许非负数。登录与 Key 校验受 `login` 和底层请求超时共同约束；离线周期恢复还受 `healthCheck` 约束。调大其中一项不保证能超越其余超时。

运行中的聚合宽恕期为 0 表示关闭提前返回机制，等待任务完成或超时。但当前配置加载器会把 `searchGracePeriod: 0` 和 `metadataGracePeriod: 0` 重新替换为 3000；面板运行时设为 0 与重启后读文件的结果不同。`latestGracePeriod: 0` 在加载时保留。不要用“写入 0 后重启即可永久禁用搜索宽恕期”的操作说明。

宽恕期到期后的迟到结果仅在后台登记 ID/实例关系，不会补写已经返回客户端的响应；详情见[加载与聚合排障](troubleshooting.md#首页加载慢--媒体库不全)。

`server.id` 和上游 `id` 是持久身份，缺失时生成；不要通过手改 ID 模拟重排序。面板重排序不应改变服务器身份。普通用户、授权和隐藏库数据由运行数据库保存，不添加到上游 YAML 账户列表。

上例 `admin.password` 的明文仅用于初始化；管理员使用 scrypt 哈希，普通用户同时保存哈希与 AES-GCM secret，上游 `username/password/apiKey` 在配置中仍是明文。详见[安全政策](../SECURITY.md#凭据存储与文件权限)。


## 首次启动最小配置

用于手动 Compose/源码首次启动。创建 `config/config.yaml`，替换示例管理员密码；已有安装保留现有配置。

```yaml
server:
  port: 8096
  name: "Emby-In-One"
admin:
  username: "admin"
  password: "replace-this-before-start"
playback:
  mode: "proxy"
proxies: []
upstream: []
```

其余字段使用加载默认值，上游可登录面板后添加。配置文件和实际 dataDir 需对运行账户可写；Release 默认为 eio，Compose 默认 uid/gid 1000。

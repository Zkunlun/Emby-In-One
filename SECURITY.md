# Security Policy

[简体中文](#reporting-a-vulnerability) · [English](#english-security-policy)

## Supported Versions

| Version | Supported |
| ------- | --------- |
| Latest (`main`) | ✅ |
| Older commits | ❌ |

Only the latest version on the `main` branch receives security fixes. Please update before reporting.

---

## Reporting a Vulnerability

**请勿通过 GitHub Issue 公开披露安全漏洞。**

如发现安全问题，请通过以下方式私下联系：

- **GitHub Security Advisories**：[提交私密报告](https://github.com/Zkunlun/Emby-In-One/security/advisories/new)
- 或通过 GitHub 私信联系仓库维护者

报告时请尽量包含：

1. 漏洞类型（如 RCE、XSS、配置泄露等）
2. 复现步骤（最小可复现示例）
3. 影响范围（哪些版本、哪些配置下受影响）
4. 可能的修复建议（可选）

收到报告后，维护者将在 **7 个工作日内**回复确认，并在修复后发布 Advisory 致谢。

---

## Security Considerations

使用本项目前，请了解以下安全事项：

### 管理面板

- `/admin` 提供登录界面；管理 API 和管理操作要求本地管理员认证，管理员由 `config/config.yaml` 的 admin 字段配置。公开登录页面不等于无需认证即可管理。
- **强烈建议**使用强密码，并避免将管理面板直接暴露在公网，推荐通过反向代理限制访问来源 IP。

### 凭据存储与文件权限

- 管理员密码启动时转换为 scrypt 哈希，不可还原明文。普通用户密码同时保存 scrypt hash 与 AES-GCM secret，密钥位于实际 dataDir 的 user-password.key。
- 保护数据库与密钥的配套备份；已有加密密码时丢失密钥会拒绝初始化。配置及 token 文件按私有权限写入，密钥创建为 0600；权限不能替代可信的运行环境。
- 密码重置优先使用[停服后的菜单/CLI](docs/operations.md#管理员密码重置)，手工改配置不等同 token 撤销流程。

### 上游凭据存储

- 上游服务器的用户名、密码及 API Key 以**明文**存储在 `config/config.yaml` 中。
- 请确保配置文件权限设置正确（建议 `chmod 600 config/config.yaml`），避免其他用户读取。
- Docker 部署时，确保挂载目录不对外暴露。

### 代理与网络

- EIO 根据请求路由和当前选定身份向上游发送请求，使用相应上游认证凭据；本地 EIO token 用于本地认证，不应与上游 token/API Key 混为一谈。
- 配置的网络代理（`proxies`）会经手认证凭据，请只使用可信代理。
- 建议在本项目前部署 HTTPS 反向代理（如 Nginx），避免凭据在传输中明文暴露。

### 直连播放与授权边界

- redirect 的客户端可见 URL 可包含上游 token/API Key，拿到凭据可按共享上游账户权限绕过 EIO 入口；请用受限上游账户，不接受这一取舍时用 proxy。
- 隐藏库入口不取消授权；本地撤销无法立即收回已经交给客户端的直连地址或已开始的媒体传输。详细规则见[上游与播放](docs/playback-and-upstream.md)和[用户与权限](docs/users-and-permissions.md)。

### 账号封禁风险

- 本项目通过模拟 Emby 客户端行为与上游通信（UA 伪装、Passthrough），存在被上游服务器识别并**封禁账号或 API Key** 的风险。
- 此为使用层面的已知风险，不属于安全漏洞范畴，不在本政策处理范围内。

### 日志

- 运行日志可能包含请求 URL、部分请求头等信息，请妥善保管日志文件，避免泄露上游地址或 Token 信息。
- 日志文件位于 `data/emby-in-one.log`（Release 部署在 `/opt/emby-in-one/data/`），Docker 部署时挂载到 `/app/data/`。

---

## 已实现机制与已知取舍

- **`data/tokens.json` 权限更严格**：Unix/Linux 上按 `0600` 写入
- **`config.yaml` 安全写入**：原子替换方式保存 + `0600` 权限，减少配置损坏风险并防止其他用户读取密码
- **请求体大小限制**：所有 API 请求体限制 2MB（`http.MaxBytesReader`），防止恶意大请求消耗内存
- **登录速率限制**：同一 IP 连续登录失败 5 次后锁定 15 分钟，返回 `429 Too Many Requests`；原子操作避免 TOCTOU 竞态条件；支持反向代理场景下的真实 IP 识别（`X-Real-IP` / `X-Forwarded-For` / IPv6）
- **`trustProxy` 只在可信反代之后开启**：登录限流按 IP 计数，而 `server.trustProxy: true` 时服务端对来源**不做任何校验**就采信 `X-Real-IP` / `X-Forwarded-For` 的第一段。前面没有可信反向代理却开启它，等于把限流的键交给客户端——攻击者可以不断伪造 IP 绕过 5 次失败锁定，也可以定向锁死他人 IP。配置细节见[反向代理信任](docs/configuration.md#反向代理信任-trustproxy)
- **图片端点免认证（已知取舍，非疏漏）**：`GET /Items/{itemId}/Images/{imageType}` **不要求 token**。原因是客户端会把图片 URL 内嵌进界面并长期缓存，若强制鉴权，token 轮换或缓存失效后客户端会大面积刷不出海报。它的安全性建立在**虚拟 ID 本身就是能力 URL** 之上：URL 里的 `itemId` 是 `crypto/rand` 生成的 128 位随机数，只有真正取到过该媒体元数据的用户才知道它，猜不出来。两点明确含义：(1) **任何拿到该 URL 的人都能取到那张图**，即使他没有 token——因此未认证请求不会经过 `AllowedServers` 白名单校验（已认证请求仍会正常校验）；(2) 取图时使用该上游的共享身份向上游请求。当前没有按用户签发的可撤销图片 URL；需要此控制时应另行设计，不能当作已实现配置
- **优雅关机**：收到 `SIGINT` / `SIGTERM` 信号后，先排空当前活动连接（最多等待 10 秒），再关闭 HTTP 服务器和健康检查定时器
- **管理面板 CSP**：Admin 面板返回严格的 `Content-Security-Policy`——`default-src 'self'`，且 `script-src` / `style-src` / `font-src` / `connect-src` 中不含任何第三方源、不含 `'unsafe-inline'`。Vue、lucide、Tailwind CSS 产物与 Inter 字体全部自托管于 `public/vendor/`，面板既不加载也不外连任何外部地址。仅保留 `'unsafe-eval'`（Vue 运行时编译 DOM 内模板所需）
- **流媒体 URL 缓存自动淘汰**：`IDStore` 中的 `streamURLs` 缓存条目 4 小时后自动过期，每 30 分钟清理一次，防止长期运行后内存无限增长
- **代理连通性测试 SSRF 防护**：管理面板的代理测试接口内置 DNS 重绑定防护，阻止请求连接到私有/保留 IP 地址（`127.x`、`10.x`、`172.16-31.x`、`192.168.x` 等）
- **YAML 注释安全解析**：配置文件解析时正确处理引号内的 `#` 字符，不再错误截断含 `#` 的值

## Scope

以下属于本安全策略的处理范围：

- 管理面板认证绕过
- 配置文件或凭据信息泄露（通过 API 或日志）
- 服务端请求伪造（SSRF）
- 远程代码执行（RCE）
- 任意文件读写

以下**不在**范围内：

- 上游 Emby 服务器自身的安全问题
- 因用户自行配置不当（如弱密码、公网暴露管理面板）导致的问题
- 账号被上游封禁（属已知使用风险）

---

## Acknowledgements

感谢所有负责任地披露安全问题的研究者，修复后将在 Security Advisory 中致谢。

---

## English security policy

### Supported versions in English

| Version | Supported |
| --- | --- |
| Latest (`main`) | ✅ |
| Older commits | ❌ |

Only the latest version on main receives security fixes. Update before reporting.

### Reporting a vulnerability in English

**Do not disclose vulnerabilities publicly in GitHub Issues.**

Contact the maintainer privately:

- **GitHub Security Advisories:** [submit a private report](https://github.com/Zkunlun/Emby-In-One/security/advisories/new).
- Alternatively, contact the repository maintainer privately on GitHub.

Include where possible:

1. Vulnerability type, such as RCE, XSS or configuration disclosure.
2. Reproduction steps with a minimal example.
3. Affected versions, configurations and scope.
4. A suggested fix, optionally.

The maintainer will acknowledge reports within **7 business days** and credit the reporter in an Advisory after remediation.

### Security considerations in English

Review these considerations before using the project.

#### Admin panel

- /admin serves the login page. Admin APIs and management actions require local administrator authentication configured under admin in config/config.yaml. A public login page does not mean unauthenticated administration.
- Use a strong password. Avoid exposing the panel directly to the Internet; a reverse proxy with source-IP restrictions is strongly recommended.

#### Credential storage and file permissions

- Administrator passwords become irreversible scrypt hashes at startup. Regular-user passwords store both a scrypt hash and an AES-GCM secret; the key is user-password.key in the actual dataDir.
- Protect matching database/key backups. Existing encrypted passwords without their key prevent initialization. Configuration/token files are written with private permissions; keys are created as 0600. Permissions do not replace a trusted runtime.
- Prefer the [stopped-service menu/CLI reset](docs/en/operations.md#administrator-password-reset). Manual configuration edits are not the same token-revocation procedure.

#### Upstream credential storage

- Upstream usernames, passwords and API Keys are stored **in plaintext** in config/config.yaml.
- Set appropriate permissions, recommended `chmod 600 config/config.yaml`, to prevent access by other users.
- Do not expose Docker-mounted configuration/data directories.

#### Proxies and networking

- EIO sends upstream requests according to routing and the selected identity, using the relevant upstream credentials. Local EIO tokens authenticate locally and are distinct from upstream tokens/API Keys.
- Configured network proxies handle authentication credentials. Use trusted proxies only.
- An HTTPS reverse proxy, such as Nginx, is recommended in front of EIO to protect credentials in transit.

#### Direct playback and access boundaries

- A client-visible redirect URL can contain an upstream token/API Key. Its holder may bypass EIO with the shared account's own permissions. Use a restricted upstream account, or proxy if the tradeoff is unacceptable.
- Hiding library entries does not remove authorization. Local revocation cannot immediately reclaim URLs already given to clients or media already transmitting. See [playback](docs/en/playback-and-upstream.md) and [permissions](docs/en/users-and-permissions.md).

#### Upstream account bans

- Client simulation, including UA spoofing and passthrough, may be recognized by an upstream and cause an **account or API Key ban**.
- This is a known usage risk, not a vulnerability covered by this policy.

#### Logs

- Logs may contain request URLs and some headers. Protect them against disclosure of upstream addresses/tokens.
- Default file: data/emby-in-one.log, under /opt/emby-in-one/data/ for Release deployments and /app/data/ in Docker.

### Implemented mechanisms and known tradeoffs

- **tokens.json permissions:** written as 0600 on Unix/Linux.
- **Configuration writes:** atomic replacement and 0600 permissions reduce corruption risk and prevent other users reading passwords.
- **Request-body limits:** API request bodies are capped at 2 MB using http.MaxBytesReader.
- **Login rate limits:** five consecutive failures from one IP lock it for 15 minutes with 429 Too Many Requests. Atomic handling avoids TOCTOU races; reverse-proxy client-IP handling supports X-Real-IP, X-Forwarded-For and IPv6.
- **trustProxy only behind trusted ingress:** with server.trustProxy: true, the server trusts X-Real-IP or the first X-Forwarded-For entry **without source validation**. Without a trusted proxy, clients control the rate-limit key and can bypass five-failure lockouts or lock a chosen IP. See [reverse proxy trust](docs/en/configuration.md#reverse-proxy-trust-trustproxy).
- **Unauthenticated image endpoint, a deliberate tradeoff:** `GET /Items/{itemId}/Images/{imageType}` needs no token because clients embed and cache image URLs, and mandatory authentication would break cached posters after token rotation/cache invalidation. The virtual ID acts as a capability URL: crypto/rand creates a 128-bit random item ID known to recipients of its metadata rather than readily guessable. **Anyone who obtains the URL can fetch the image without a token**, so unauthenticated image requests do not pass AllowedServers checks; authenticated ones still do. Image fetching uses the upstream's shared identity. There are no implemented per-user revocable image URLs; those would require a separate design.
- **Graceful shutdown:** SIGINT/SIGTERM drains active connections for at most 10 seconds, then closes the HTTP server and health-check timer.
- **Panel CSP:** strict Content-Security-Policy with default-src 'self'. script-src, style-src, font-src and connect-src contain neither third-party origins nor 'unsafe-inline'. Vue, lucide, Tailwind output and Inter fonts are self-hosted under public/vendor; panel assets do not load or connect externally. 'unsafe-eval' remains for Vue's runtime DOM-template compiler.
- **Stream URL cache eviction:** IDStore streamURLs entries expire after 4 hours, with cleanup every 30 minutes to bound long-running memory use.
- **Proxy-test SSRF protection:** the panel proxy-test endpoint defends against DNS rebinding and blocks private/reserved addresses including 127.x, 10.x, 172.16-31.x and 192.168.x.
- **YAML comment parsing:** quoted # characters are preserved rather than incorrectly truncating values.

### Scope in English

Covered:

- Admin authentication bypass.
- Configuration/credential disclosure through APIs or logs.
- Server-side request forgery (SSRF).
- Remote code execution (RCE).
- Arbitrary file reads/writes.

Not covered:

- Security problems in upstream Emby servers.
- Problems caused by user misconfiguration, such as weak passwords or public panel exposure.
- Upstream account bans, a known usage risk.

### Acknowledgements in English

Thanks to researchers who disclose responsibly. Reporters will be credited in the Security Advisory after remediation.

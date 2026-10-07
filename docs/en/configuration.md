# Configuration

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../configuration.md)

Applies to the V1.4.9 mainline.

The configuration file is `config/config.yaml`, mounted at `/app/config/config.yaml` in Docker.

```yaml
# dataDir: "/opt/emby-in-one/data"    # Top-level runtime data directory; defaults explained below

server:
  port: 8096
  name: "Emby-In-One"
  # id: Generated on first start; do not change manually
  # trustProxy: true        # Only behind a trusted reverse proxy; see below

admin:
  username: "admin"
  password: "your-strong-password"    # Converted to an irreversible scrypt hash at startup

playback:
  mode: "proxy"          # "proxy" or "redirect"; global default

timeouts:
  api: 30000             # One upstream API request (ms)
  global: 15000          # Overall aggregation deadline (ms)
  login: 30000           # Upstream login and API Key validation (ms)
  healthCheck: 30000     # Offline upstream reauthentication (ms)
  healthInterval: 60000  # Periodic check interval (ms)
  searchGracePeriod: 3000     # Wait after the first successful search result (ms)
  metadataGracePeriod: 3000   # Metadata grace period (ms)
  latestGracePeriod: 0        # Latest-items grace period; 0 waits for all sources (ms)

proxies: []
  # - id: "abc123"
  #   name: "Example proxy"
  #   url: "http://user:pass@ip:port"

upstream:
  - name: "Server A"
    url: "https://emby-a.example.com"
    username: "user"
    password: "pass"

  - name: "Server B"
    url: "https://emby-b.example.com"
    apiKey: "your-api-key"
    playbackMode: "redirect"                   # Overrides the global mode
    spoofClient: "infuse"                      # none | passthrough | infuse | hills | capyplayer | custom
    streamingUrls:                             # Optional ordered routes; streamingUrl also accepts one URL
      - "https://cdn.example.com"              # Primary route
      - "https://backup.example.com"           # Backup route
    followRedirects: true                      # Follow upstream 301/302/303/307/308; false treats them as upstream errors without forwarding their Location
    proxyId: null                              # ID in the network proxy pool
    priorityMetadata: false                    # Prefer this source's display metadata when merging
    maxConcurrent: 3                           # Regular-user authorization capacity; 0 is unlimited; administrators are exempt

  - name: "Server C (custom identity example)"
    url: "https://emby-c.example.com"
    apiKey: "your-api-key"
    spoofClient: "custom"
    customUserAgent: "Infuse/7.7.1 (iPhone; iOS 17.4.1; Scale/3.00)"
    customClient: "Infuse"
    customClientVersion: "7.7.1"
    customDeviceName: "iPhone"
    customDeviceId: "your-custom-device-id"

  - name: "Server D (Hills preset)"
    url: "https://emby-d.example.com"
    apiKey: "your-api-key"
    spoofClient: "hills"

  - name: "Server E (CapyPlayer preset)"
    url: "https://emby-e.example.com"
    apiKey: "your-api-key"
    spoofClient: "capyplayer"
```

## When changes take effect

| Setting or edit path | Effect |
| --- | --- |
| Panel-supported upstream, user, proxy, name and timeout settings | Update runtime state through their admin interfaces; each panel save does not require a restart |
| Global `playback.mode` | Initial value for newly added upstreams; does not overwrite existing `playbackMode` values |
| One upstream's playback mode | Change and save in that upstream's editor; applies to that upstream |
| Startup settings such as `server.port`, `server.trustProxy` and `dataDir` | Not exposed by the panel; edit the file and restart |
| Manual YAML edits | There is no general file-watching reload; restart to reread and avoid simultaneous panel writes |

See [playback modes](playback-and-upstream.md#playback-modes) for full rules. Administrator password changes revoke all proxy tokens. Password, enable/disable and binding changes for regular users also affect sessions; see [users and permissions](users-and-permissions.md).

## Reverse proxy trust (`trustProxy`)

| Value | Behavior | Deployment |
| --- | --- | --- |
| `false` (default) | Login rate limiting uses the TCP peer IP (`RemoteAddr`) | Direct exposure without a reverse proxy |
| `true` | Login rate limiting trusts `X-Real-IP` / `X-Forwarded-For` | Behind a trusted reverse proxy such as Nginx or Caddy |

Behind Nginx, Caddy or Cloudflare, enable `server.trustProxy: true` only after ensuring EIO can be reached solely through a trusted reverse proxy. Otherwise all clients can appear to share the proxy IP, and five failed logins can rate-limit everyone for 15 minutes.

**The reverse proxy must overwrite these headers.** EIO prefers `X-Real-IP`, then the **first** entry in `X-Forwarded-For`. The common Nginx setting `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;` appends rather than replaces. A client-supplied first entry remains, allowing forged IPs to bypass login limits or lock out a chosen IP.

Use overwrite semantics:

```nginx
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $remote_addr;
```

IP-based limiting is trustworthy only when the final trusted ingress overwrites the headers and clients cannot bypass it to reach EIO directly. Verify the actual source chain in multi-proxy deployments.

Keep `false` without a trusted reverse proxy. With `true`, EIO accepts those headers **without validating their source**. Directly exposed instances, or chains that never overwrite them, let attackers rotate forged IPs to bypass `POST /Users/AuthenticateByName` failure counts and the 15-minute lockout, or submit another person's IP to lock that address out.

Enable this only when the last entry point capable of reaching EIO is necessarily your trusted reverse proxy. If that cannot be established, leave it disabled.

## Data directory (`dataDir`)

`dataDir` is a **top-level** key alongside `server`, `admin` and `playback`. It controls runtime file storage.

| Item | Value |
| --- | --- |
| Default | `/app/data` when that directory exists (the repository Dockerfile creates it); otherwise `data/` under the process working directory |
| Contents | `mappings.db` (virtual IDs, users and watch state), `tokens.json` (proxy tokens), `captured-headers.json` (passthrough identities), `user-password.key` (regular-user password key), `emby-in-one.log` |

This key does **not** move the configuration file. `config.yaml` remains at `config/config.yaml`, or `/app/config/config.yaml` in Docker. See [runtime data and backups](operations.md#runtime-data-and-backups).

When to change it:

- **Binary/source:** `data/` is relative to the process working directory. If your startup directory differs from the intended project directory, or you want fixed absolute storage or a separate mount from config/, specify dataDir explicitly. Check systemd's `WorkingDirectory`, commonly `/opt/emby-in-one`.
- **Docker:** the default is `/app/data`, already mounted from host ./data by docker-compose.yml. Usually no change is needed unless you customize mounts.
- **Migration/reuse:** point to existing data only with a compatible schema and complete matching database/key files. See [upgrades](operations.md#version-upgrades). Copying only the database or reusing any old directory is not a supported general migration.

Set it in the file, for example `dataDir: "/opt/emby-in-one/data"`; the panel has no dataDir setting. **Restart** to apply. Use an absolute production path: relative paths change with the working directory and may look like lost data when another directory is actually being used. The process account needs read/write access.

## Defaults and limits

All timeout/grace fields use milliseconds. On configuration load, `api`, `login` and `healthCheck` default to 30000, `global` to 15000, `healthInterval` to 60000, search/metadata grace periods to 3000 and latest-items grace to 0.

`api`, `global`, `login`, `healthCheck` and `healthInterval` must be positive; admin input ranges follow the current validator. Grace periods accept nonnegative values. Login and Key validation are constrained by `login` and the underlying request timeout; offline recovery is also constrained by `healthCheck`. Raising one setting does not guarantee it exceeds the others.

At runtime, an aggregation grace period of 0 disables early return and waits for tasks to finish or time out. However, the current loader replaces `searchGracePeriod: 0` and `metadataGracePeriod: 0` with 3000. Setting 0 through the running panel differs from rereading the file after restart. `latestGracePeriod: 0` is preserved on load. Writing 0 and restarting does not permanently disable search grace.

Late results after grace expiry only register IDs/instances in the background; they do not append to responses already returned to clients. See [loading and aggregation troubleshooting](troubleshooting.md#slow-home-screen-or-incomplete-libraries).

`server.id` and upstream `id` are persistent identities, generated when missing. Do not edit IDs to simulate reordering. Panel reordering should not change server identity. Regular users, authorization and hidden libraries are stored in the runtime database, not added as upstream YAML accounts.

The sample admin plaintext password is for initialization. Administrators use scrypt hashes; regular users store both a hash and an AES-GCM secret. Upstream `username/password/apiKey` remain plaintext in configuration. See [credential storage](../../SECURITY.md#credential-storage-and-file-permissions).

## Minimal first-start configuration

For an initial manual Compose/source deployment, create `config/config.yaml` and replace the sample administrator password. Keep existing configuration for an existing installation.

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

Other fields use loader defaults. Add upstreams after panel login. The runtime account must be able to write the configuration and actual dataDir; Release uses eio by default, Compose uid/gid 1000.

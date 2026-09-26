# Emby-In-One

> **Version: V1.4.5**

[![License: GPL v3](https://img.shields.io/github/license/Zkunlun/Emby-In-One?color=blue)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![SQLite](https://img.shields.io/badge/SQLite-3-003B57?logo=sqlite&logoColor=white)](https://www.sqlite.org/)
[![Docker](https://img.shields.io/badge/Docker-20.10+-2496ED?logo=docker&logoColor=white)](https://www.docker.com/)
[![GitHub Release](https://img.shields.io/github/v/release/Zkunlun/Emby-In-One?color=green)](https://github.com/Zkunlun/Emby-In-One/releases)
[![GitHub Stars](https://img.shields.io/github/stars/Zkunlun/Emby-In-One?style=social)](https://github.com/Zkunlun/Emby-In-One)

[Changelog](Update.md) | [中文文档](README.md) | [Security Policy](SECURITY.md) | [Update Plan](Update%20Plan.md) | [V1.2.1 Legacy Docs](README_V1.2.1.md) | [GitHub](https://github.com/Zkunlun/Emby-In-One)

Emby-In-One is a multi-upstream aggregation proxy for standard Emby clients. It combines multiple Emby servers behind one endpoint and provides media aggregation, per-user isolation, playback proxying, access control, and unified administration and operations.

## About This Project

This repository is actively developed and maintained on top of [ArizeSky/Emby-In-One](https://github.com/ArizeSky/Emby-In-One). Many thanks to the original author, [ArizeSky](https://github.com/ArizeSky), for creating Emby-In-One and establishing its early architecture and core functionality.

The current repository continues that work with compatibility fixes, stability improvements, feature development, and ongoing releases. Future maintenance, bug fixes, and releases are tracked here.

The current stable release is **V1.4.5**. The active codebase is primarily implemented in Go; the original Node.js V1.2.1 implementation is retained under [`legacy/`](legacy/) for historical reference and is not part of current builds or installations.

## Table of Contents

- [About This Project](#about-this-project)
- [Features Overview](#features-overview)
- [Quick Installation](#quick-installation)
- [System Requirements](#system-requirements)
- [Configuration Reference](#configuration-reference)
- [Multi-User Management](#multi-user-management)
- [Advanced Config & Core Principles](#advanced-config--core-principles)
- [Health Check](#health-check)
- [Security Hardening](#security-hardening)
- [Logging System](#logging-system)
- [Admin Panel](#admin-panel)
- [SSH Management Menu](#ssh-management-menu)
- [Data Directory Description](#data-directory-description)
- [FAQ](#faq)
- [Disclaimer](#disclaimer)
- [Project Architecture](#project-architecture-developer-reference)
- [Development & Contributions](#development--contributions)
- [Relationship to the Original Project](#relationship-to-the-original-project)
- [Credits](#credits)
- [Star History](#star-history)
- [License](#license)

## Features Overview

| Area | Description |
| --- | --- |
| **Multi-Upstream Aggregation** | Combines media libraries, search results, and media items from multiple Emby servers behind one endpoint. Concurrent fan-out plus configurable grace periods reduce the impact of slow upstreams, while previously aggregated content can fall back to other online `OtherInstances`. |
| **Media Merging & ID Virtualization** | Deduplicates movies, series, seasons, and episodes across servers while retaining multiple MediaSources for the same title. Clients see persistent Virtual IDs, while metadata priority rules select the preferred display metadata. |
| **Multi-User & Independent Watch State** | Supports regular users with independent playback progress, played state, favorites, Resume, and NextUp. `IsFavorite`, `IsPlayed`, `IsResumable`, and `IsUnplayed` filters are also evaluated from the current user's local state, while admins keep upstream-account semantics. |
| **Access Control & Library Visibility** | Admins have access to all upstreams and management features; regular users can be restricted to selected servers. Libraries or entire servers can also be hidden from a user's Emby home screen without affecting search, Latest, or Resume content. |
| **Proxy & Direct Playback** | Supports both `proxy` and `redirect` playback modes. Proxy mode relays video, audio, HLS segments, subtitles, and related requests through EIO; Redirect mode returns a 302 to the upstream to reduce EIO bandwidth usage. |
| **Upstream Authentication & Client Identity** | Upstreams can authenticate with username/password or API Key. Client identity supports `none`, `passthrough`, `infuse`, and `custom` modes, including passthrough/custom Emby identity headers and automatic upstream re-login after session failure. |
| **Network Proxies & Health Checks** | Each upstream can use its own HTTP/HTTPS proxy with built-in connectivity testing. Background health checks retry offline upstreams in parallel and log online/offline transitions. |
| **Concurrent Playback Control** | Each upstream can define a regular-user concurrency limit with `maxConcurrent`. Excess playback requests return `429 Too Many Requests`, and stale occupancy is released through playback heartbeat expiry. |
| **Web Admin & SSH CLI** | Includes a Web admin panel, REST management API, and SSH management menu for upstreams, users, network proxies, global settings, logs, updates, and service lifecycle operations. |
| **Logging & Security** | Includes persistent leveled logs with rotation, login-failure rate limiting, scrypt password storage, protected config/token file permissions, request-body limits, SSRF protections, and a CSP for the admin panel. |
| **Multiple Deployment Options** | Supports GitHub Release binaries with systemd, Docker / Docker Compose, and running from Go source. Releases provide static builds for amd64, arm64, arm, mips, mipsle, and riscv64 with SHA256 checksums. |

> `redirect` mode places upstream access credentials in the client-visible direct URL. Use it only when that security trade-off is acceptable; see [Playback Mode Explained](#playback-mode-explained) for details.

---

## Quick Installation

> **Notice for Legacy Node.js Deployment**: If you wish to deploy the V1.2.1 stable Node.js version, please use the original project's [Releases page](https://github.com/ArizeSky/Emby-In-One/releases) to download the V1.2.1 Source code archive, extract it, and run `bash install.sh`. The `legacy/` directory in this repository keeps the V1.2.1 Node.js source **for reference only** (the Go ID virtualization was written against it); it takes part in no build, image or install of the Go version — see `legacy/README.md`.

This project primarily recommends using Release binaries for V1.4.5 directly on Linux servers (no local Go build required); Docker deployment is suitable for scenarios where you want to build the image yourself.

### Method 1: Release Binary One-Click Install (Primary Recommendation)

```bash
curl -fsSL -o release-install.sh https://raw.githubusercontent.com/Zkunlun/Emby-In-One/main/release-install.sh
sudo bash release-install.sh
```

Optional: install a specific version.

```bash
sudo bash release-install.sh V1.4.5
```

This script will automatically:
- Download the matching Release binary based on your CPU architecture (no local Go compilation needed)
- Initialize `/opt/emby-in-one/{config,data,log}` and generate a random admin password on the first run
- Fetch companion resources `admin.html`, `admin.js` and `emby-in-one-cli.sh` (the binary already embeds the whole admin panel, including the frontend dependencies under `public/vendor/`; any file missing from disk falls back to the embedded copy, so the external files are only an optional override)
- Install and start the `systemd` service (`emby-in-one`), supporting auto-start on boot
- Auto-backup and perform a rollback-safe upgrade if an older version is detected

### Method 2: Source Repo One-Click Install Script (Recommended for developers / local image build)

```bash
git clone https://github.com/Zkunlun/Emby-In-One.git
cd Emby-In-One
bash install.sh
```

The script will automatically install the Docker environment, assign a random admin password, build the Go version image, and start the service. To manage your server later, type `emby-in-one` via SSH to call up the management menu.

> **Note**: The source-repo install script copies `cmd/`, `internal/`, `third_party/`, and `public/` into the builder stage for Go compilation. If you customize the `Dockerfile` or copy files manually, make sure the `public/` directory is also present in the build context, otherwise the build may fail with `package emby-in-one/public is not in std`.

### Method 3: Manual Docker Compose Deployment

1. Create project directories and hand them to the container user (the container runs as uid 1000; unwritable mounts make startup fail when the server cannot write `tokens.json` / `mappings.db`):
```bash
mkdir -p /opt/emby-in-one/{config,data}
chown -R 1000:1000 /opt/emby-in-one/config /opt/emby-in-one/data
cd /opt/emby-in-one
```
2. Copy all core files from this repository (including `go.mod`, `cmd/`, `internal/`, `public/`, `Dockerfile`, `docker-compose.yml`, etc.) to this directory.
3. Create the initial configuration `config/config.yaml`:
```yaml
server:
  port: 8096
  name: "Emby-In-One"
  # trustProxy: true        # Set to true when deployed behind a reverse proxy (Nginx/Caddy etc.)

admin:
  username: "admin"
  password: "your-strong-password" # Automatically encrypted after first boot

playback:
  mode: "proxy"

timeouts:
  api: 30000
  global: 15000
  login: 30000
  healthCheck: 30000
  healthInterval: 60000

proxies: []
upstream: []
```
4. Build and start:
```bash
docker compose build
docker compose up -d
```

### Method 4: Direct Go Source Run (For Developers)

Requirements: Go 1.23+ and a C toolchain (Debian/Ubuntu run `apt install build-essential`).
```bash
mkdir -p config data
# Create config.yaml in the config folder as instructed in Method 3
go test ./...
go run ./cmd/emby-in-one
```

**Default Access URLs**:
- Emby Client Connection Address: `http://Server_IP:8096`
- Admin Panel: `http://Server_IP:8096/admin`

---

## System Requirements

**Release Binary Deployment (Recommended):**
- Linux (amd64 / arm64 / arm / mips / mipsle / riscv64)
- No Go compilation environment needed, directly run pre-compiled binaries

**Docker Deployment:**
- Docker 20.10+, Docker Compose v2
- Linux: Debian 11/12/13, Ubuntu 22/24 (recommended), other distros need self-verification
- Windows / macOS can also run (for dev and testing)

**Go Source Build:**
- Go 1.23+
- C Toolchain (CGO used for SQLite): Debian/Ubuntu run `apt install build-essential`

---

## Configuration Reference

The config file is located at `config/config.yaml` (mounted into the container at `/app/config/config.yaml` when using Docker).

```yaml
# dataDir: "/opt/emby-in-one/data"    # Runtime data directory (top-level key; defaults are in "Data Directory" below)

server:
  port: 8096
  name: "Emby-In-One"
  # id: Auto-generated on first boot, do not modify manually
  # trustProxy: true        # Set to true when behind a reverse proxy (see below)

admin:
  username: "admin"
  password: "your-strong-password"    # Automatically encrypted after first boot

playback:
  mode: "proxy"          # "proxy" or "redirect", global default

timeouts:
  api: 30000             # Single upstream API request timeout (ms)
  global: 15000          # Aggregation request max total timeout — waiting for all servers (ms)
  login: 30000           # Upstream login timeout (ms) — applies to login and API-key validation; exceeding it fails the login
  healthCheck: 30000     # Health check timeout (ms) — applies to reconnect probes of offline servers
  healthInterval: 60000  # Health check interval (ms)
  searchGracePeriod: 3000     # Search aggregation grace period — wait for other servers after first result (ms), 0 disables it
  metadataGracePeriod: 3000   # Metadata fetch grace period (ms), 0 disables it
  latestGracePeriod: 0        # "Latest Added" grace period — 0 means wait for all servers (ms)

proxies: []
  # - id: "abc123"
  #   name: "Japan Proxy"
  #   url: "http://user:pass@ip:port"

upstream:
  - name: "Server A"
    url: "https://emby-a.example.com"
    username: "user"
    password: "pass"

  - name: "Server B"
    url: "https://emby-b.example.com"
    apiKey: "your-api-key"
    playbackMode: "redirect"                   # Overrides global playback mode
    spoofClient: "infuse"                      # none | passthrough | infuse | custom
    streamingUrls:                               # Streaming lines (optional, ordered; a single one can also be written as streamingUrl: "...")
      - "https://cdn.example.com"                # 1st entry is the primary line
      - "https://backup.example.com"             # the rest are fallbacks
    followRedirects: true                      # Follow upstream 301/302/303/307/308 (default true; when false the redirect is reported as an upstream error instead of being forwarded to the client)
    proxyId: null                              # Associate with proxy ID from proxy pool
    priorityMetadata: false                    # Prefer using this server's metadata when merging
    maxConcurrent: 3                           # Max concurrent playbacks, 0 means unlimited (affects regular users only)

  - name: "Server C (custom spoof example)"
    url: "https://emby-c.example.com"
    apiKey: "your-api-key"
    spoofClient: "custom"
    customUserAgent: "Infuse/7.7.1 (iPhone; iOS 17.4.1; Scale/3.00)"
    customClient: "Infuse"
    customClientVersion: "7.7.1"
    customDeviceName: "iPhone"
    customDeviceId: "your-custom-device-id"
```

Settings modified in the admin panel take effect hotly, no service restart required. The one exception is the global default playback mode, which is only the initial value for a new upstream — see Playback Modes.

### Reverse Proxy Trust (`trustProxy`)

| Value | Behavior | Applicable Scenario |
|-------|----------|--------------------|
| `false` (default) | Login rate limiting uses the TCP connection IP (`RemoteAddr`) | Directly exposed to the internet, no reverse proxy |
| `true` | Login rate limiting trusts `X-Real-IP` / `X-Forwarded-For` headers | Deployed behind Nginx / Caddy or other reverse proxies |

> **Important**: If your Emby-In-One instance is behind a reverse proxy (Nginx, Caddy, Cloudflare, etc.), you **must** add `trustProxy: true` under the `server` section in `config.yaml`. Otherwise all client requests will appear to come from the same IP, and after 5 failed login attempts all users will be rate-limited for 15 minutes.

> **Precondition: the reverse proxy must _overwrite_ these headers.** This program prefers `X-Real-IP` and otherwise takes the **first** entry of `X-Forwarded-For`. Nginx's most common form, `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`, **appends** — whatever the client sent stays first, so anyone can forge an IP: rotate fake IPs to bypass the login rate limit, or aim one at a victim to lock that IP out for 15 minutes.
>
> Overwrite instead:
> ```nginx
> proxy_set_header X-Real-IP $remote_addr;
> proxy_set_header X-Forwarded-For $remote_addr;
> ```
> As long as the proxy sets `X-Real-IP` (preferred here, and not forgeable by appending), the rate limit can be trusted.
>
> **Conversely: when there is no trusted reverse proxy in front, it must stay `false`.** `trustProxy: true` makes the server take `X-Real-IP` / `X-Forwarded-For` on faith, with no check of where the request came from. If the instance is directly exposed to the internet (or nothing along the path overwrites those headers), anyone can supply an arbitrary IP: rotate a fake IP on every failed login to bypass the failure counter and the 15-minute lockout on `POST /Users/AuthenticateByName`, or put someone else's IP there to lock that IP out.
>
> The test is simple: **enable it only when the last hop that can reach this service is certain to be your own reverse proxy.** If you are not sure, leave it off.

### Data Directory (`dataDir`)

`dataDir` is a **top-level key** in the config file (a sibling of `server`, `admin` and `playback`) that decides where runtime data is written.

| Item | Value |
|------|-------|
| Default | `/app/data` when that directory exists (the official Docker image creates it); otherwise `data/` under the process working directory |
| Contents | `mappings.db` (virtual ID mappings, user data, watch history), `tokens.json` (proxy-layer tokens), `captured-headers.json` (passthrough client headers), `emby-in-one.log` (log file) |

> **This key has nothing to do with the config file itself.** `config.yaml` always lives at `config/config.yaml` (`/app/config/config.yaml` inside the Docker container) and does not move with `dataDir`. See [Data Directory Description](#data-directory-description) for what each file holds.

**When to change it**:

- **Binary / source deployments**: `data/` is resolved against the **process working directory**. If the service starts from a directory other than the project directory (for example a systemd `WorkingDirectory` of `/opt/emby-in-one`) and you want the data pinned to an absolute path, or mounted separately from `config/`, set `dataDir` explicitly.
- **Docker deployments**: the container already defaults to `/app/data`, and `docker-compose.yml` mounts the host's `./data` there, so you normally **do not** need to change it; only a custom mount point would require it.
- **Migrating / reusing existing data**: point `dataDir` at the directory that already holds your data — no need to move files by hand.

**Notes**:

- Setting it in the config file is enough (`dataDir: "/opt/emby-in-one/data"`); the admin panel does not expose it, and a **service restart** is required;
- Prefer an **absolute path** in production — a relative path follows the startup working directory and can look like data loss when it is really just a different directory being read and written;
- The directory must be readable and writable by the process user.

---

## Multi-User Management

V1.4 adds multi-user support, allowing admins to create multiple regular users, each independently configurable with accessible upstream servers.

### Role Descriptions

| Role | Permissions |
|------|-------------|
| Admin (admin) | Can access all servers, the admin panel, and management APIs; watch state is read directly from the upstream Emby account |
| Regular User (user) | Can only access permitted servers; client-visible playback progress, played state, favorites, Resume, and NextUp are isolated through the local WatchStore |

### Independent Watch History

Because all distributed users share the same upstream Emby account, watch progress, played state, and favorites are naturally shared on the upstream side. Real state changes from a regular user are still written upstream and are also written to that EIO user's WatchStore; reads use the **local record as authoritative state**, so one regular user's changes to the shared upstream account do not overwrite what another regular user sees:

| Feature | Admin | Regular User |
|---------|-------|--------------|
| Resume (Continue Watching) | Upstream server data | Local independent data |
| Next Up | Upstream server data | Calculated based on local progress |
| Played Status | Upstream server data | Local independent record |
| Favorite | Upstream server data | Local independent record |
| UserData in browsing pages | Direct passthrough from upstream | Overlay local state over it |
| List filtering (favorites / played / unplayed / resumable) | Upstream server filtering | Local record filtering |

**List filtering (local since V1.4.4; `IsUnplayed` completed in V1.4.5):**

- `Filters=IsFavorite`, `IsPlayed`, `IsResumable` and `IsUnplayed` are answered from the **local records**: the proxy fetches the candidate set for that container, evaluates it against the current proxy user's WatchStore state, then sorts, pages and recounts locally. For `IsUnplayed`, an item with no local row is naturally unwatched; only a local row with `Played=true` excludes it. Container constraints such as `ParentId`, `Recursive` and `IncludeItemTypes` are still applied upstream.
- Sorting: `SortName`, `DateCreated`, `ProductionYear` and `CommunityRating` are sorted locally; any other sort key (e.g. `DatePlayed`) degrades to the local "recently played / favorited" order.
- Unrecognized filter values (e.g. `IsFolder`) are forwarded upstream untouched, so the client's intent is never silently dropped.

> **Known limitation**: `Likes`, `Dislikes` and `IsFavoriteOrLiked` still use shared upstream semantics because the local WatchStore does not yet record liked/disliked state. These unlocalized filters return the `X-Emby-In-One-Filter-Notice` response header and emit a throttled WARN log.

**Paging:** the aggregated list without a `ParentId` (`GET /Users/{id}/Items` with no container) is paged by the proxy **after** merging and deduplicating, so `TotalRecordCount` is the merged total and `StartIndex` counts in merged order. Local filtering works the same way: the candidate set is fetched first, then sorted and paged locally.

> One more trade-off: both of those paths fetch a **candidate set** from the upstream (rather than letting it filter and return one page), capped at 5000 items per request; past the cap the reported total is only a lower bound and the tail of the list may be unreachable, which is logged.

**Working Principle:**

- Playback events (start, progress, stop) simultaneously write to the upstream server and the local database (dual write)
- Playback completion (progress ≥ 90%) is automatically marked as "played"
- Mark played / favorite and other user operations are also dual written
- When a user is deleted, their local watch data is automatically cleared
- Upon first playback of an item, the system automatically fetches metadata from upstream (series name, seasons, episodes) to support NextUp calculations

### Creating Regular Users

Admins can create and manage regular users through the following ways:

1. **Admin Panel** — Visual operations in the "User Management" page
2. **SSH Menu** — Use the `emby-in-one` command, select "Add Regular User" or "Delete Regular User"
3. **REST API** — `POST /admin/api/users` (requires admin Token)

### Configuring Accessible Servers

Each regular user can restrict upstream access through a list of stable server `serverId` values. When one or more `serverId` values are specified, the user can only browse and play content from those servers. If `allowedServers` is omitted, `null`, or an empty list, the server scope is **unrestricted (all upstreams are accessible)**.

### Concurrent Playback Limits

Each upstream server can independently configure `maxConcurrent` (maximum concurrent playback number):

- `0` (default): No limit
- Positive integer: Limits the number of regular users playing simultaneously on that server
- Admins are not subject to this limit
- Returning `429 Too Many Requests` when limit exceeded
- Based on 3-minute heartbeat timeout for auto-release of occupation

---

## Advanced Config & Core Principles

### Upstream Server Authentication (Complete Mechanism)

Each upstream server supports two authentication methods (choose one):

| Method | Config Fields | Working Principle |
|--------|--------------|-------------------|
| Username/Password | `username` + `password` | The proxy calls upstream's `AuthenticateByName` login interface for a Session Token, then reuses the session for future requests |
| API Key | `apiKey` | Directly carries API Key for requests, no login flow needed (Recommended) |

Authentication decision and fault tolerance logic:
- If both are configured, `apiKey` takes precedence.
- When login fails, an error is recorded and affects health check, but does not block concurrent aggregation of other upstreams.
- Health checking and auto-reconnect reuse the context from the most recent successful authentication for that upstream.

### Playback Mode Explained

`playbackMode` determines how the media stream is delivered to the client.

| Mode | Working Principle | Applicable Scenarios |
|------|-------------------|----------------------|
| `proxy` | Traffic is forwarded via the proxy server. Fragment URLs in HLS manifests (`.m3u8`) are rewritten as relative proxy paths. Supports Range requests, subtitles, and attachments. | Upstream lacks public IP; Need to hide upstream address from clients; Requires reverse proxy/public domain compatibility |
| `redirect` | The client receives a `302` redirect, connecting directly to the upstream stream URL. Traffic does not pass via the proxy after redirection. | Clients can directly connect upstream; Saves proxy server bandwidth |

**Priority**: Single server `playbackMode` > Global `playback.mode` > `"proxy"` (default)

> **The global `playback.mode` is only the initial value for a new upstream.** Once an upstream exists its `playbackMode` has already been written with the value of that moment, so changing the global default later does **not** affect any existing upstream (same for the "default playback mode" field at the top of the panel). To change one upstream's mode, use the playback-mode dropdown in that server's edit dialog — it takes effect immediately.

> ⚠ **Security warning for `redirect` (direct playback) mode**: direct playback places the upstream account credential (`api_key`) in the `302` redirect URL. **Any user who can play can extract that credential** — including users restricted by `AllowedServers` — and bypass EIO with whatever permissions the shared upstream account itself has. If that shared account is an upstream administrator, the leaked credential effectively grants upstream administrator privileges. Therefore:
>
> - Use a **dedicated, restricted account for that upstream** with only the media-library playback permissions it needs; do not reuse an upstream administrator account or a broadly shared account;
> - Once leaked, the credential can only be invalidated by changing the upstream password or revoking the API key;
> - If this risk is unacceptable, keep the default `proxy` mode.

An upstream can configure **multiple streaming lines** (`streamingUrls`, an ordered list): the first entry is the primary line, the rest are fallbacks. All lines must point to the same Emby server (multiple lines are multiple routes to one server, not mirrored servers — transcoding sessions live on the server itself, so switching lines across mirrors causes 404s).

- **Proxy mode**: on a connect-level failure of the primary line (connection refused / timeout / TLS error) the proxy automatically switches to the next fallback, invisibly to the client. Any HTTP status returned by the upstream (including 404/403) is not treated as a line failure.
- **Redirect mode**: the line is chosen by liveness — every health-check cycle probes fallback lines at the connect level (any HTTP response counts as alive, including the 403/404 returned by split-tunnel reverse proxies that only forward `/Videos/` and `/Audio/`). A line marked dead is skipped for 60 seconds, then becomes a candidate again. After the 302 the traffic no longer passes through the proxy; a line failure mid-playback is handled naturally when the player re-fetches the manifest.
- When left empty the stream base equals `url` (the front-end address), same as a single `streamingUrl`.

### UA Spoofing Explained (`spoofClient`)

Controls what client identity the proxy communicates with the upstream server. Affects login, API requests, health checks, and stream proxying.

| Value | User-Agent | X-Emby-Client | Usage Scenario |
|-------|------------|---------------|----------------|
| `none` | Proxy default identity | `Emby Aggregator` | Most servers — no client restrictions |
| `passthrough` | Real client UA | Real client value | Servers with client allowlists; if no real identity has been captured yet, the initial upstream login is deferred until a real client connects |
| `infuse` | `Infuse/7.7.1 (iPhone; iOS 17.4.1; Scale/3.00)` | `Infuse` | Servers strictly allowing Infuse |
| `custom` | Custom value | Custom value | Servers needing complete control over client markings |

> **Note**: The `official` mode from V1.2 has been automatically migrated to `custom` in V1.3, using the original Emby Web official client's default values.
>
> **Current behavior**: In `custom` mode, the configured `User-Agent`, `X-Emby-Client`, `X-Emby-Client-Version`, `X-Emby-Device-Name`, and `X-Emby-Device-Id` are applied to upstream login, normal API requests, health checks, image proxying, and stream proxying. After saving in the admin panel, these values are persisted to the config file and correctly restored when editing the upstream again.

#### Passthrough Mode Principles

Passthrough resolves request-level client identity through five fallback levels. One important exception applies at first startup: if only level 5 (`infuse-fallback`) is available and no real client identity has been captured, username/password passthrough upstreams defer their initial login and admin-side connectivity validation instead of forcing a login with the fallback identity:

1. **Live Request Header** — If the current request carries the `X-Emby-Client` header (a genuine Emby client), direct usage.
2. **Current Token Captured Header** — When a real client (Infuse, Emby iOS, etc.) logs in to Emby-in-One, the proxy captures and stores the client's `User-Agent`, `X-Emby-Client`, `X-Emby-Device-Name`, etc. based on the proxy Token; future requests heavily tied to the same Token will reuse these.
3. **Server's Last Successful Login Header** — Every time a passthrough server successfully logs in, the complete headers used are remembered and persisted. Will be used straight after reboot, without waiting for users to re-login.
4. **Most Recent Captured Header** — If the current request lacks a Token and the server has no historical successful records, it uses the lastly captured header from any Token.
5. **Infuse Fallback** — If there are entirely no captured client headers (e.g. freshly installed first boot), the Infuse identity acts as a safe default.

Captured headers overlay the basic Infuse profile, ensuring even if the client hasn't sent all Emby header fields (like certain third-party Apps), a fully fleshed client identity can still be presented.

When a client logs in, offline passthrough upstreams automatically retry with the newly captured identity. Headers used for a successful upstream login are persisted per server, so health checks and reconnects can reuse that server's last successful identity after a restart. When a proxy token is revoked — for example through logout, an admin password change/reset, or user deletion — its token-scoped captured identity is removed as well. Proxy tokens do not expire automatically based on time.

### Metadata Priority (`priorityMetadata`)

When the exact same movie/episode appears on multiple servers, the proxy needs to pick one server's metadata (title, summary, picture) as the "primary" version. The rules are:

| Priority | Rule | Reason |
|----------|------|--------|
| 1 | Server styled with `priorityMetadata: true` | Manually designated preferred metadata source |
| 2 | Overview contains Chinese characters | Prioritize using Chinese localized metadata |
| 3 | Overview text is longer | A more complete description prioritized |
| 4 | Smaller server index (ordered ahead in config) | Stable fallback rule |

This priority solely affects which metadata to display—all servers' MediaSource versions are uniformly retained and clients can pick flexibly.

### Media Merge Strategy

| Content Type | Dedup Criterion | Behavior |
|--------------|-----------------|----------|
| **Movies** | TMDB ID, or Title + Year | Merged into a solitary entry containing multiple MediaSources |
| **Series** | TMDB ID, or Title + Year | Deduplicated at the series layer |
| **Seasons** | Season Number `IndexNumber` | Deduplication by season number |
| **Episodes** | Season:Episode number | Deduplicated; greatest metadata grabbed by the priority algorithm above |
| **Libraries (Views)** | — | Fully preserved, appending server names as suffixes for distinction |

Cross-server entries are initially interleaved (Round-Robin) before duplicated merging.

### ID Virtualization

Each upstream Item ID is mapped globally to a lone virtual ID — 16 random bytes (128 bits) from `crypto/rand`, rendered as a 32-character lowercase hex string with no dashes. Any IDs visible to clients are virtual.

- **Storage**: Persistent SQLite storage in WAL mode, backed by an in-memory cache for fast lookups
- **Mapping**: `virtualId <-> { originalId, serverId }`, with additional `otherInstances` relationships persisted as well; `serverId` is a stable server identity and does not depend on configuration order
- **Persistence**: Virtual mappings and primary/additional instance relationships survive restarts; legacy `server_index` data is migrated to `server_id`
- **Upstream deletion**: If a removed primary instance still has another upstream instance available, a remaining instance is promoted while preserving the Virtual ID and regular-user WatchStore state. Only truly orphaned items with no remaining instance lose their mapping and associated watch state

---

## Health Check

- Operates `GET /System/Info/Public` **in parallel** across all upstreams every 60 seconds (configurable via `timeouts.healthInterval`)
- Passthrough servers preferentially apply the server's prior successful login headers (persisted storage), relying next on the most recently captured headers guarding against nginx declines
- Traces logs on state alterations (ONLINE → OFFLINE / OFFLINE → ONLINE)
- Timers for health assessment instantly clear during graceful shutdowns

---

## Security Hardening

- **Admin plaintext password auto-hashed on startup**: The Go backend automatically migrates plaintext `admin.password` to scrypt hash format on service startup, without waiting for the first login
- **CLI password reset support**:

```bash
emby-in-one --reset-password <new-password|-> [--force]
# Or use the SSH menu option "Change Admin Password" (the menu stops the service, resets, then starts it again)
```

  - A password of `-` is **read from stdin**, so it never shows up in the process list (`ps`) or the shell history — this is the form the install and management scripts use: `printf '%s' "$pass" | emby-in-one --reset-password -`
  - By default it first probes `127.0.0.1:<port from config.yaml>/System/Info/Public` and **refuses to run while the service is still up**, telling you to `systemctl stop emby-in-one` first (see below for why)
  - `--force` skips that probe; use it only when you are sure you need it
  - The reset clears `tokens.json` with an **atomic write** (keeping `_proxyUserId`), so a truncated file can no longer stop the service from booting; **every issued proxy token is invalidated** and clients must sign in again

  > **Why a running instance must be refused**: a live instance keeps the tokens in memory and writes the whole `tokens.json` back on its next login or logout — restoring every token this command just cleared, making the reset a no-op. The CLI therefore errors out rather than silently "resetting" nothing. For Docker deployments use the SSH menu, or run the command it prints on failure (`docker compose ... run --rm -T emby-in-one /app/emby-in-one --reset-password - --force`).

- **Stricter `data/tokens.json` permissions**: Written with `0600` permissions on Unix/Linux
- **Secure `config.yaml` writes**: Atomic replacement + `0600` permissions, reducing corruption risk and preventing other users from reading passwords
- **Request body size limit**: All API request bodies limited to 2MB (`http.MaxBytesReader`), preventing malicious large requests from consuming memory
- **Login rate limiting**: After 5 consecutive login failures from the same IP, locked for 15 minutes with `429 Too Many Requests`; atomic operations prevent TOCTOU race conditions; supports real IP detection behind reverse proxies (`X-Real-IP` / `X-Forwarded-For` / IPv6)
- **`trustProxy` only behind a trusted reverse proxy**: login rate limiting counts by IP, and with `server.trustProxy: true` the server takes the first entry of `X-Real-IP` / `X-Forwarded-For` **without checking where it came from**. Enabling it with no trusted reverse proxy in front hands the rate-limit key to the client — an attacker can rotate forged IPs to bypass the 5-failure lockout, or aim one at a victim to lock that IP out. See [Reverse Proxy Trust](#reverse-proxy-trust-trustproxy) for the configuration details
- **Unauthenticated image endpoint (a known trade-off, not an oversight)**: `GET /Items/{itemId}/Images/{imageType}` **does not require a token**. Clients embed image URLs in their UI and cache them for a long time, so requiring auth would leave posters broken across the board once a token rotates or a cache entry expires. Its safety rests on the **virtual ID being a capability URL**: the `itemId` in the path is a 128-bit `crypto/rand` value that only a user who actually fetched that item's metadata can know. Two consequences worth stating plainly: (1) **anyone who obtains that URL can fetch that image**, even without a token — so an unauthenticated request skips the `AllowedServers` check (authenticated requests are still checked as usual); (2) the fetch goes upstream under that upstream's shared identity. If per-user, revocable image access is needed, this can move to short-lived signed URLs
- **Graceful shutdown**: On receiving `SIGINT` / `SIGTERM`, drains active connections (up to 10 seconds) before closing the HTTP server and health check timers
- **Admin panel CSP**: the admin panel returns a strict `Content-Security-Policy` - `default-src 'self'`, with no third-party origin and no `'unsafe-inline'` in `script-src` / `style-src` / `font-src` / `connect-src`. Vue, lucide, the compiled Tailwind CSS and the Inter font are all self-hosted under `public/vendor/`, so the panel loads nothing from and connects to nothing on an external origin. Only `'unsafe-eval'` remains, which Vue needs to compile its in-DOM template at runtime
- **Stream URL cache auto-eviction**: `IDStore` stream URL cache entries expire after 4 hours, cleaned every 30 minutes, preventing unbounded memory growth during long-running operation
- **Proxy connectivity test SSRF protection**: The admin panel's proxy test endpoint includes DNS rebinding protection, blocking connections to private/reserved IP addresses (`127.x`, `10.x`, `172.16-31.x`, `192.168.x`, etc.)
- **YAML comment-safe parsing**: Config file parsing correctly handles `#` characters inside quotes, no longer incorrectly truncating values containing `#`

---

## Logging System

### Log Levels

| Level | Output | Content |
|-------|--------|---------|
| DEBUG | File   | All request details, ID resolution, header info |
| INFO  | File + Console | Logins, server status changes, config changes |
| WARN  | File + Console | 401/403 responses, server disconnections |
| ERROR | File + Console | Request failures, login failures, exceptions |

### Log Files

- Path: `data/emby-in-one.log` (Release sets `data/` at `/opt/emby-in-one/data/`)
- Docker path: `/app/data/emby-in-one.log`
- Up to 10MB per file, retaining 3 rotated backups (`emby-in-one.log.1` ~ `.3`), auto-rotation
- Capable of being downloaded and cleared inside the admin panel (clearing also deletes the backups)

### Log Configuration

Default log level is `info`. Enable full debug logging via environment variables when troubleshooting:

```bash
LOG_LEVEL=debug FILE_LOG_LEVEL=debug
```

In Docker Compose:

```yaml
environment:
  - LOG_LEVEL=debug
  - FILE_LOG_LEVEL=debug
  - LOG_MAX_SIZE_MB=10   # per-file size limit (MB), default 10
  - LOG_KEEP=3           # rotated backups to keep, default 3
```

---

## Admin Panel

Access `http://your-ip:8096/admin`, logging in with the admin credentials from the config file.

| Page | Functions |
|------|-----------|
| **System Overview** | Online server count, ID mapping count, storage engine (SQLite) |
| **Upstream Nodes** | Add / edit / delete / reconnect servers, drag-and-drop ordering; Supports configuring maximum concurrent playback (`maxConcurrent`) |
| **User Mgmt** | Create, edit, enable/disable, delete regular users; Visually configure accessible servers |
| **Network Proxies** | HTTP/HTTPS proxy pool management, supports one-click connectivity testing |
| **Global Settings** | System name, default playback mode, admin account, timeout & grace period configuration |
| **Runtime Logs** | Real-time log viewing, supports level filtering (ERROR/WARN/INFO/DEBUG), keyword search, downloading raw log files, and clearing logs |

> The admin panel sidebar displays the current running version number. For adding/editing `spoofClient: passthrough` upstreams, if there is no captured client identity available, the admin API will still save the configuration but return a warning, keeping the upstream as offline until a real client logs in and triggers automatic retry.

### Admin API

All management APIs require authentication (`X-Emby-Token` header or `api_key` query parameter). For security reasons, `/admin/api/*` is exposed only with same-origin CORS behavior and does not grant arbitrary cross-origin access.

---

## SSH Management Menu

After installation, use:

```bash
emby-in-one
```

Available commands:

- Start / restart / stop service
- Online update (latest version) / download specific version
- View service status, public IP
- View admin credentials, modify admin username / password
- View user list, add regular user, delete regular user
- View logs
- Uninstall service (supports preserving config and data)

> The SSH menu auto-detects the current deployment method (Binary / Docker), dispatching all operations to the corresponding systemd or Docker Compose commands. Docker mode updates use a source-rebuild workflow. There is no separate "check version" entry — the current version is shown directly in the menu title bar (e.g. `Emby In One 管理菜单 v1.4.5`).

---

## Data Directory Description

Runtime directories:

- `config/` — Stores config file `config.yaml`
- `data/` — Stores runtime data:
  - `mappings.db` — Virtual ID mappings, additional instances interactions, user data (UserStore), and watch history (WatchStore)
  - `tokens.json` — Proxy layer token storage
  - `captured-headers.json` — Passthrough client headers persistence
  - `emby-in-one.log` — Log file

The actual location of `data/` can be changed with the top-level [`dataDir`](#data-directory-datadir) key in the config file.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/admin/api/status` | System status |
| GET | `/admin/api/upstream` | List upstream servers |
| POST | `/admin/api/upstream` | Add upstream server |
| PUT | `/admin/api/upstream/:id` | Modify upstream server (stable `serverId`; legacy index lookup remains compatible) |
| DELETE | `/admin/api/upstream/:id` | Delete upstream server; preserve Virtual IDs that still have other instances and remove only truly orphaned mappings/state |
| POST | `/admin/api/upstream/:id/reconnect` | Reconnect upstream server |
| POST | `/admin/api/upstream/reorder` | Adjust server ordering |
| GET | `/admin/api/proxies` | List proxies |
| POST | `/admin/api/proxies` | Add proxy |
| POST | `/admin/api/proxies/test` | Test proxy connectivity |
| DELETE | `/admin/api/proxies/:id` | Delete proxy |
| GET | `/admin/api/settings` | Retrieve global settings |
| PUT | `/admin/api/settings` | Modify global settings |
| GET | `/admin/api/logs?limit=500` | Fetch in-memory logs |
| GET | `/admin/api/logs/download` | Download persisted log files |
| DELETE | `/admin/api/logs` | Clear logs |
| GET | `/admin/api/client-info` | Get currently captured client information |
| GET | `/admin/api/users` | List all regular users |
| POST | `/admin/api/users` | Create regular user |
| PUT | `/admin/api/users/:id` | Update regular user |
| DELETE | `/admin/api/users/:id` | Delete regular user (auto-clears watch data) |
| POST | `/admin/api/logout` | Admin logout |

---

## FAQ

### Passthrough Upstream Login Failure (403)

On a fresh installation with no real client identity captured yet, a username/password `passthrough` upstream **skips its initial login and remains offline** until a real client identity becomes available; it does not use the Infuse fallback to force the first upstream login:
1. Sign in to Emby-In-One with a real Emby client (Infuse, Emby iOS, etc.) using the **admin** account
2. After the proxy captures the client identity, it automatically retries offline passthrough upstreams
3. Once login succeeds, the identity used for that server is persisted and can be reused after future restarts
4. Identity-source information in the logs can help with diagnosis; for example, `last-success` means that server's previously successful identity. `infuse-fallback` remains the final internal fallback for identity resolution, but initial login/admin validation is deferred when that is the only available source
5. If the captured client identity is still rejected upstream, sign in again as admin using a client that the upstream accepts

### Upstream Shows Offline / Login Timeout

`timeouts.login` (30s default) and `timeouts.healthCheck` (30s default) used to have no effect — every request inherited `timeouts.api`. Both are enforced now, and this is the most likely reason an upstream goes offline right after an upgrade:

- A login that takes longer than `timeouts.login` fails → the server shows offline. Raise it in "Global Settings" for upstreams that need 10–30s to log in
- Health-check probes only run for **offline** servers and are bounded by `timeouts.healthCheck`; a timeout merely means that round did not recover, and the next round retries — online servers are never marked offline by it
- These values can only tighten the timeout below `timeouts.api`; raise `api` as well to allow longer probes
- `Timeouts are now enforced` in the startup log means your config carries values below `api`

### Only Admin Login Can Capture Client UA

In passthrough mode, the proxy needs to capture the real client's UA / Device and other Emby identity headers. **Only when logging in with the admin account will the proxy capture and store these client header information**. Regular user logins do not trigger UA capture.

Reason: The admin is the only role that maps directly to the upstream Emby account. Only the admin's login session needs to maintain a real client identity to pass through to upstream servers. Regular users' requests are sent by the proxy using the admin's previously captured client identity.

If your passthrough upstream consistently fails to auto-login, please verify:
1. You have logged into Emby-in-One using a real Emby client (Infuse, Emby iOS, etc.) with the **admin** account
2. Check the admin panel "Captured Client Info" page to confirm records exist
3. To change the captured client identity, log in once with the desired client using the admin account

### Playback 403 / 401

Possible causes:
- Upstream token expired → Click "Reconnect" in the admin panel
- Passthrough server headers incomplete → Check the logs querying `Stream headers for [Server Name]` to confirm proper header capture
- Media merge switching → MediaSourceId translates precisely and points correctly to the mapped upstream

### Loading Delay / Incomplete Library Merging

- Default search grace period is 3 seconds — after the first server responds, up to 3 more seconds are allowed for remaining servers; timed-out server data is silently backfilled in the background
- If upstream servers have generally high latency, increase `searchGracePeriod` and `metadataGracePeriod` in the admin panel "Global Settings" or `config.yaml` `timeouts` section
- `latestGracePeriod` defaults to 0 (wait for all servers); set to a positive value if "Latest Added" on the home page loads slowly
- Check logs for `timeout` or `abort` keywords
- You can also increase `api` (single request timeout) and `global` (aggregation total timeout) values

### Admin Password Lost

After first boot, the Admin password automatically hashes (scrypt). Recovery methods:

**Method 1: File Modifications**
1. Edit `config/config.yaml`, swapping the hash after `password:` directly to an explicit plaintext entry 
2. Execute an application restart—the system natively identifies and hashes the plaintext properly.

**Method 2: Command Line Menu**
```bash
emby-in-one
# Opt maneuvering toward the "Change Password" toggles directly
```

### Reverse Proxy Users Rate-Limited (429)

If all users receive `429 Too Many Requests` after 5 failed login attempts, `trustProxy` is not enabled:
1. Add `trustProxy: true` under the `server` section in `config.yaml`
2. Restart the service
3. Verify the reverse proxy **overwrites** `X-Real-IP` or `X-Forwarded-For` — with the appending form `$proxy_add_x_forwarded_for` a client can forge its IP (bypassing the limit, or locking someone else out). See [Reverse Proxy Trust](#reverse-proxy-trust-trustproxy)

### Docker Containers Fail to Reach Upstream

- Check if the upstream URL uses `localhost` → Inside a container, `localhost` points to the container itself. Use the host's actual IP or domain.
- To access host-machine services, use `host.docker.internal` (Docker Desktop) or the actual local IP address.

---

## Disclaimer

> **Notice**: This project communicates with upstream servers by simulating and masking Emby client behavior. There resides inherent risk of upstream operators or associated platforms detecting proxies and enforcing bans against your account or API Key. Utilization of this project equates to your self-assumption of these risks. The author bears zero responsibility for account bans, data loss, or other damages resulting from its use.

---

## Project Architecture (Developer Reference)

```text
Emby-In-One/
├── cmd/emby-in-one/
│   └── main.go                     # Application entrypoint
├── internal/backend/
│   ├── config.go                   # YAML config load/save/validate/atomic write
│   ├── server.go                   # HTTP server startup & graceful shutdown
│   ├── routes.go                   # Route registry (URL → Handler mapping)
│   ├── middleware.go               # HTTP middleware (CORS, logging, status capture, CSP)
│   ├── ssrf.go                     # SSRF policy and safe dialers (proxy probe / upstreams)
│   ├── auth.go                     # Proxy token issuance & validation
│   ├── auth_context.go             # Per-request auth context injection & extraction
│   ├── auth_manager.go             # Upstream auth management (login/session/API Key)
│   ├── identity.go                 # Client identity capture & Passthrough 5-level resolution
│   ├── identity_persistence.go     # Per-upstream client identity persistence
│   ├── user_store.go               # Multi-user storage (CRUD, password hashing, memory index + SQLite)
│   ├── handlers_admin.go           # Admin API handlers (upstream server CRUD)
│   ├── handlers_system.go          # System info endpoints (/System/Info)
│   ├── handlers_user.go            # User login rate limiting & user-related handlers
│   ├── admin_validation.go         # Admin input validation & helper utilities
│   ├── idstore.go                  # SQLite bidirectional ID mapping (virtual ↔ original)
│   ├── id_rewriter.go              # Recursive ID virtualization/devirtualization rewriting
│   ├── query_ids.go                # Batch query ID resolution
│   ├── media.go                    # Media aggregation, dedup, metadata priority selection
│   ├── aggregation.go              # Common aggregation framework (grace period + background backfill)
│   ├── media_items.go              # Media item queries (multi-upstream fan-out merge)
│   ├── media_resume.go             # Resume Items proxy & multi-upstream merge
│   ├── media_nextup.go             # Next Up proxy & multi-upstream merge
│   ├── media_playback.go           # PlaybackInfo query & concurrent playback limit check
│   ├── media_stream.go             # Video/audio stream proxy (virtual ID route resolution)
│   ├── library_image.go            # Image proxy (cache headers)
│   ├── series_userdata.go          # Series-level watch history isolation (Resume/NextUp)
│   ├── session_userdata.go         # Sessions/Playing progress reporting
│   ├── watch_store.go              # Per-user watch progress storage & persistence
│   ├── playback_limiter.go         # Concurrent playback limiter (heartbeat timeout auto-release)
│   ├── login_limiter.go            # Per-IP login failure limiter (evicts instead of blocking)
│   ├── streamproxy.go              # HTTP stream proxy (backpressure, HLS relative path rewriting)
│   ├── fallback_proxy.go           # Fallback route: scan URL/Query for virtual IDs
│   ├── healthcheck.go              # Parallel health checks
│   ├── logger.go                   # Leveled logging (Console + File dual output + rotation)
│   ├── scrypt_local.go             # Admin password scrypt hashing
│   ├── sqlite_cgo.go               # CGO embedded SQLite compilation & low-level bindings
│   └── upstream.go                 # Upstream connection pool & concurrent request orchestration
├── third_party/sqlite/             # SQLite CGO source dependency
├── public/
│   ├── embed.go                    # go:embed directive (compiles admin.html, admin.js and vendor/ into binary)
│   ├── admin.html                  # Vue 3 + Tailwind CSS admin panel template
│   ├── admin.js                    # Vue 3 application logic (extracted from admin.html)
│   └── vendor/                     # Self-hosted frontend dependencies: Vue, lucide, Tailwind output, Inter font
├── assets/panel.css                # Tailwind input (holds the panel styles moved out of admin.html)
├── tailwind.config.js              # Tailwind content config (run npm run build:panel after editing admin.html/admin.js)
├── package.json                    # Root package.json keeps only the build:panel script (Node deps moved to legacy/)
├── Dockerfile                      # Go runtime container build
├── docker-compose.yml
├── install.sh                      # Source repo one-click deploy script (Docker)
├── release-install.sh              # Release binary one-click deploy script (systemd)
├── emby-in-one-cli.sh              # SSH terminal management menu script
└── legacy/                         # V1.2.1 Node.js implementation: reference only, built into nothing
    ├── README.md                   #   Why it is kept, and why it takes part in no build
    ├── src/                        #   Old Express implementation (the reference for the Go ID virtualization)
    ├── tests/                      #   Old Node tests (most no longer pass)
    └── package.json                #   Node dependencies (only used by npm --prefix legacy install)
```

---

## Development & Contributions

The active codebase is implemented in Go. Reproducible bug reports, compatibility feedback, feature requests, and Pull Requests are welcome.

When contributing code, please keep these principles in mind:

- Bug fixes should include regression coverage for the underlying cause whenever practical, rather than patching only one client's visible symptom.
- Keep changes focused and avoid mixing unrelated architectural refactors or formatting churn into the same PR.
- Run tests relevant to the changed area before submitting; for shared backend behavior, `go test ./...` is recommended.
- Changes to the admin panel should also verify frontend assets and embedded resources remain in sync. Changes to installation or release flows should verify versioning, installer behavior, and the Release workflow together.
- Client compatibility reports are most useful when they include request paths, response differences, logs, or clear reproduction steps.

---

## Relationship to the Original Project

Emby-In-One was originally created by [ArizeSky](https://github.com/ArizeSky), with the original repository at [ArizeSky/Emby-In-One](https://github.com/ArizeSky/Emby-In-One). This repository continues development and maintenance on top of that project and retains its core multi-Emby aggregation design.

This repository is not a simple mirror of the original project. Ongoing compatibility fixes, feature maintenance, releases, and the active Go codebase are maintained in [Zkunlun/Emby-In-One](https://github.com/Zkunlun/Emby-In-One). Existing code, design work, and historical contributions from the original project remain attributable to their respective authors and contributors.

The original Node.js V1.2.1 implementation is retained under [`legacy/`](legacy/) for historical reference and is not part of current Go builds, images, or installation flows. For V1.2.1, refer to the original project's historical releases; for currently maintained versions, use this repository's Releases.

This project continues to be distributed under the **GNU General Public License v3.0**. Modifications and redistribution must comply with GPL-3.0.

---

## Credits

- Thanks to [ArizeSky](https://github.com/ArizeSky) for creating Emby-In-One and building the project's early architecture and core functionality.
- Thanks to all contributors to both the original project and this repository, as well as issue reporters and users who helped test client compatibility.
- Contributions to the maintained repository can be reviewed through [GitHub Contributors](https://github.com/Zkunlun/Emby-In-One/graphs/contributors) and the commit history.

---

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=Zkunlun/Emby-In-One&type=Date)](https://star-history.com/#Zkunlun/Emby-In-One&Date)

---

## License

GNU General Public License v3.0

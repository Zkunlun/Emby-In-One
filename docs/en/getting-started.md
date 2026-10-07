# First use

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../getting-started.md)

Applies to the V1.4.9 mainline. The current panel uses Chinese labels; the labels below help locate its controls.

## Distinguish the three accounts

| Account | Purpose |
| --- | --- |
| EIO local administrator | Logs into the panel, manages sources/users and can access every upstream through an Emby client |
| EIO local regular user | Logs into EIO through a client, accesses explicitly authorized upstreams and uses independent local watch state |
| Upstream Emby account or API Key | Lets EIO connect to that server; does not replace local panel or regular-user accounts |

Complete [installation](installation.md) first and prepare an upstream you are entitled to use that EIO can reach. EIO does not include a media library.

## Step 1: log into the panel

Visit `http://server-ip:8096/admin`. The Release/source installers print admin's random password on first installation; manual installations use their own initial configuration. If forgotten, follow the [reset guide](operations.md#administrator-password-reset).

## Step 2: add an upstream

In **上游节点** (upstream nodes), add a name and Emby URL and select either username/password or API Key authentication. Username authentication accepts an empty password; Key authentication validates upstream `/Users/Me`. Establish access with default proxy mode first. Before using direct playback, review the [mode tradeoffs](playback-and-upstream.md#playback-modes).

Choose none, passthrough, infuse, hills, capyplayer or custom according to upstream requirements. Without a client restriction, a fixed identity preset is not a prerequisite. Presets do not establish that every client has been tested.

After saving, inspect status and any warning. A username-authenticated passthrough upstream without a real captured identity is saved offline. Log into EIO once as its local administrator using the intended real Emby client; capture then triggers an automatic retry. Panel login may not provide the relevant headers. See [how passthrough works](playback-and-upstream.md#how-passthrough-works).

## Step 3: create and authorize a regular user

Open **用户管理** (user management), create a local user and explicitly select accessible upstreams. Selecting none grants no upstream access. Create accounts for yourself or other users; regular-user watch state differs from the administrator's direct upstream-account state.

Hiding library entries only changes the home screen. It does not replace access authorization. See [users and permissions](users-and-permissions.md) for passwords, capacity, active devices and unbinding.

## Step 4: connect a client

Add `http://server-ip:8096` as the server in an Emby client and log in with the EIO regular user you just created. With a domain or reverse proxy, use your actual EIO endpoint. Do not use /admin as the client server address or treat shared upstream credentials as a local user's password.

## Step 5: verify access and playback

Confirm that visible media comes from authorized sources, browse an item and play it. Select a specific source when multiple versions are available. proxy requires EIO-to-stream-source connectivity; redirect also requires direct client-to-source connectivity.

Resume and automatic completion require sufficient, verifiable control reports from the client to EIO. Video GET/Range alone does not establish completion. Counts sum official values from authorized online sources and differ from merged work counts. See [troubleshooting](troubleshooting.md) and [library counts](media-counts.md).

## Panel pages

Visit `http://your-ip:8096/admin` and use the local admin account configured for EIO.

| Page label | Purpose |
| --- | --- |
| **系统概览** (overview) | Online server count, ID mapping count and SQLite storage engine |
| **上游节点** (upstream nodes) | Add/edit/delete/reconnect servers, reorder with up/down buttons, show assigned regular users and configure **同播数量限制** (`maxConcurrent`) |
| **用户管理** (users) | Create, edit, enable/disable and delete regular users; select accessible servers |
| **网络代理** (network proxies) | Manage HTTP/HTTPS proxies and test connectivity |
| **全局设置** (settings) | Server name, default playback mode, administrator account, timeouts and grace periods |
| **运行日志** (logs) | View current logs, filter ERROR/WARN/INFO/DEBUG, search, download raw logs and clear logs |

The sidebar footer shows the running version. Adding/editing `spoofClient: passthrough` with no captured client identity still saves configuration, but the admin API returns a warning and keeps the upstream offline until a real client logs in.

The [admin API](admin-api.md) has its own reference. Updates, logs and the SSH menu are covered in [operations](operations.md).

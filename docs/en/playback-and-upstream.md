# Upstream access and playback

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../playback-and-upstream.md)

Applies to the V1.4.9 mainline.

## Upstream authentication

Each upstream supports either of two authentication methods:

| Method | Fields | Behavior |
| --- | --- | --- |
| Username/password | `username` + `password` | EIO calls upstream AuthenticateByName for a session token and reuses the session |
| API Key | `apiKey` | No password login; EIO calls `GET /Users/Me` with the Key to validate it and obtain the upstream user ID, then reuses the Key |

Authentication and recovery rules:

- The panel requires one method. Username authentication allows an empty password but requires a username; Key authentication requires a nonempty Key.
- With authType selected, the admin API clears the other method's fields. Historical hand-written YAML containing both uses Key first in the current runtime login branch. This is neither panel-supported dual input nor a recommended configuration.
- The upstream must let Users/Me return a valid user ID. No password login does not mean no validation or permanent online status.
- Login failures are logged and enter health-check handling without stopping concurrent aggregation from other upstreams.
- Offline recovery uses the same authentication method and relevant identity. passthrough prefers that server's most recent successful identity; see [periodic checks](operations.md#periodic-checks-and-stream-route-state).

## Playback modes

playbackMode determines how media reaches clients.

| Mode | Behavior | Suitable conditions |
| --- | --- | --- |
| `proxy` | EIO forwards media; HLS `.m3u8` segment URLs become relative proxy paths. Supports Range, subtitles and attachments | Upstream lacks a public IP; its address should be hidden from clients; reverse-proxy/domain compatibility is needed |
| `redirect` | Client receives a `302` and connects directly to the upstream stream URL. Media then bypasses EIO | Client can reach the upstream directly; saves EIO bandwidth |

Precedence: per-upstream playbackMode > global playback.mode > `"proxy"` default.

**Global playback.mode only initializes newly created upstreams.** Once created, an upstream stores the mode selected then. Changing the global default, including the panel's default-mode setting, does not change existing upstreams. Change **播放模式** (playback mode) in the individual server editor; saving applies it immediately.

**Direct-playback credential exposure:** redirect puts upstream credentials (`api_key`) in the 302 URL. Any user able to play, including users limited by AllowedServers, can extract them and bypass EIO using the shared upstream account's own permissions. If that account is an upstream administrator, those are administrator permissions.

- Use a dedicated restricted upstream account with only necessary library/playback access, no administration permissions and concurrency restrictions where needed. Do not reuse an upstream administrator or shared multiperson account.
- Once disclosed, invalidate the credential upstream by changing the password or revoking the API Key.
- Keep default proxy mode if this tradeoff is unacceptable.

An upstream can have an **ordered streamingUrls list**: first primary, then backups. Every URL must reach the **same Emby server**. These are routes to one server, not separate mirrors: transcoding sessions are server-local, so switching between mirrors can cause 404.

- **Proxy:** prefer unknown/alive routes in configured order. Connection refusal, timeout, TLS or other transport errors, and 502/503/504 responses mark the route dead and try the next. Business responses such as 404/500 prove reachability and are returned without an inappropriate route switch. Failover occurs before a usable response is obtained; EIO does not splice routes after response-body transmission begins.
- **Redirect:** select by route health; known dead routes are excluded from 302. Background lightweight probes maintain state. Time alone does not change dead to alive. A failed route has a 60-second cooldown before it becomes eligible for probing/recovery. When all routes are dead, the current redirect request performs one controlled concurrent recovery probe of eligible routes, capped at 5 seconds. It returns 302 only after recovery, otherwise 502. Once 302 is sent, ongoing media no longer passes through EIO; subsequent disconnection is the player's responsibility to retry.
- **Network proxy constraint:** redirect clients connect directly, so a server-side HTTP proxy cannot participate. A redirect upstream cannot also set proxyId: the panel disables and clears it, and the backend rejects the combination. proxy mode permits it.
- If unset, the streaming address uses url, as with one streamingUrl. The historical streamingUrl single-value field remains compatible.

## Client identity (`spoofClient`)

This selects EIO's identity when talking to upstreams. It affects EIO-originated login, API, offline reauthentication and media-proxy requests. After a redirect, the client's own media request originates from the client.

| Value | User-Agent | X-Emby-Client | Use |
| --- | --- | --- | --- |
| `none` | Default proxy identity | `Emby Aggregator` | Most servers without client restrictions |
| `passthrough` | Real client UA | Real client value | Client allowlists; initial login is delayed until a real identity is captured |
| `infuse` | `Infuse/7.7.1 (iPhone; iOS 17.4.1; Scale/3.00)` | `Infuse` | Servers allowing only Infuse |
| `hills` | `Hills/1.9.1 (android; 16)` | `Hills` | Fixed Hills 1.9.1 identity |
| `capyplayer` | `CapyPlayer/1.1.6` | `CapyPlayer` | Fixed CapyPlayer 1.1.6 identity |
| `custom` | Custom value | Custom value | Full control of client identifiers |

Hills and CapyPlayer presets retain UA values from actual client-login samples:

| Preset | ClientVersion | DeviceName | Spoofed DeviceId |
| --- | --- | --- | --- |
| `hills` | `1.9.1` | `fuxi` | `hills-spoof-id` |
| `capyplayer` | `1.1.6` | `2211133C` | `capyplayer-spoof-id` |

CapyPlayer's sampled UA gains no extra platform suffix. DeviceId uses EIO's fixed spoofed value, not a real client device ID. Presets apply only when selected for the upstream, do not automatically change existing configurations and do not follow the last client login. One profile supplies HTTP UA, identity headers and identity query parameters on supported endpoints. Internal real-device identification and session limits remain unchanged. Direct client requests after 302 may still carry their own UA.

The historical V1.2 official mode was automatically migrated to custom in V1.3, using the former official Emby Web defaults.

In custom mode, User-Agent, X-Emby-Client, X-Emby-Client-Version, X-Emby-Device-Name and X-Emby-Device-Id apply to upstream login, regular APIs, health checks, images and media proxying. Panel saves persist them and subsequent edits refill them.

### How passthrough works

Request-level identity uses five fallback levels. With only level 5 infuse-fallback and no real captured identity, initial username/password passthrough login and management connectivity validation are deliberately delayed rather than forced through the fallback.

1. **Live request headers:** if the current request has X-Emby-Client, use those real-client headers.
2. **Headers captured for this token:** a real client login captures User-Agent, X-Emby-Client, X-Emby-Device-Name and related headers under the current EIO proxy token. Only requests under that same token reuse them.
3. **This server's last successful login headers:** successful passthrough login retains and persists the complete headers per server, allowing reuse after restart without another client login.
4. **Most recently captured headers:** with no request token and no successful server history, use the last captured headers from any token.
5. **Infuse fallback:** with no captured client headers, use Infuse as the default identity.

Captured headers overlay the Infuse base profile, providing a complete identity even when third-party clients omit some Emby fields.

Client login automatically retries offline passthrough upstreams with newly captured headers. Successful headers are persisted per server and used for health checks/reconnect after restart. Revoking a proxy token, for example at logout, administrator password change/reset or user deletion, also clears its token-scoped captured headers. Proxy tokens do not automatically expire with time.

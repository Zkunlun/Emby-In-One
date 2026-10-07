# Admin API

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../admin-api.md)

Applies to the V1.4.9 mainline.

## Authentication and requests

The panel logs in with a local administrator through `POST /Users/AuthenticateByName`, receiving AccessToken. An upstream API Key is not an EIO local token.

Every admin interface below except logout requires the local administrator. The current `POST /admin/api/logout` route uses an authenticated-user guard to revoke the caller's own token. It does not grant access to other administration.

Supply the EIO token through X-Emby-Token or the api_key query parameter. Prefer the header to reduce URL credential exposure. JSON requests use `Content-Type: application/json`. Admin API routes do not allow arbitrary cross-origin callers; the panel uses same-origin requests.

Replace `{id}` with the actual ID, omitting braces. Upstreams use stable serverId values; some interfaces retain legacy-index compatibility, but new callers should use stable IDs. Obtain user/proxy IDs from their respective list responses.

## Routes

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/admin/api/logout` | Revoke the current proxy token and its captured identity; authenticated-user guard |
| GET | `/admin/api/client-info` | Read captured client identity |
| GET | `/admin/api/status` | System status |
| GET | `/admin/api/upstream` | List upstreams, status and capacity |
| POST | `/admin/api/upstream` | Add and validate an upstream |
| PUT | `/admin/api/upstream/{id}` | Update an upstream |
| POST | `/admin/api/upstream/reorder` | Reorder servers |
| DELETE | `/admin/api/upstream/{id}` | Delete an upstream and clean up under instance/lifecycle rules |
| POST | `/admin/api/upstream/{id}/reconnect` | Reconnect an upstream |
| GET | `/admin/api/proxies` | List network proxies |
| POST | `/admin/api/proxies` | Add a network proxy |
| POST | `/admin/api/proxies/test` | Test connectivity; also inspect success |
| DELETE | `/admin/api/proxies/{id}` | Delete a network proxy |
| GET | `/admin/api/settings` | Read panel-supported global settings |
| PUT | `/admin/api/settings` | Update panel-supported global settings |
| GET | `/admin/api/logs` | Read bounded in-memory logs |
| GET | `/admin/api/logs/download` | Download persistent logs |
| DELETE | `/admin/api/logs` | Clear application logs and rotated backups |
| GET | `/admin/api/users` | List regular users |
| POST | `/admin/api/users` | Create a regular user |
| PUT | `/admin/api/users/{id}` | Update a regular user, authorization or hidden-library settings |
| DELETE | `/admin/api/users/{id}` | Delete a regular user and clean up their state |
| GET | `/admin/api/upstream/{id}/libraries` | Read an upstream's libraries for configuration |
| GET | `/admin/api/home-libraries` | Read the administrator's own hidden home-library settings |
| PUT | `/admin/api/home-libraries` | Update the administrator's own hidden home-library settings |

For example, read logs with `/admin/api/logs?limit=500`. Memory-buffer entry count is not disk-log retention.

## Upstream and global settings

Upstream create/update fields follow [configuration](configuration.md) and [authentication/playback](playback-and-upstream.md). Panel authType distinguishes apiKey and password. A nonempty Key and username authentication are mutually exclusive; a username-authenticated account can have an empty password. maxConcurrent must be nonnegative; redirect cannot also set proxyId.

With no usable passthrough identity, saving can succeed with a warning while the node stays offline pending client capture. HTTP success alone does not establish upstream authentication. Administrator username/password changes require currentPassword according to the settings interface. Password changes revoke all proxy tokens.

## Users and capacity

On creation, omitted, null or [] allowedServers grants no access. On update, omitted or null preserves authorization; [] clears it. Omitted or null password preserves it; an explicit empty string clears it. Capacity exhaustion and reductions below assigned users return 409; see [capacity](users-and-permissions.md#authorization-capacity).

After a lifecycle commit, incomplete cleanup may return cleanupPending. Follow [recovery](users-and-permissions.md#management-changes-and-recovery); do not assume every state was rolled back after a failure.

## Library entries and display settings

GET /admin/api/upstream/{id}/libraries supplies configuration libraries. GET/PUT /admin/api/home-libraries affects the administrator's own hidden entries, not all regular users globally. User interfaces manage each regular user's hiddenLibraries. Hiding display entries does not revoke authorization; see [home-library hiding](users-and-permissions.md#home-library-hiding).

## Errors and completion

Authentication guards reject missing/invalid local authentication or missing administrator permission. Malformed requests, validation, unknown resources, upstream validation and persistence failures have corresponding 4xx/5xx responses. Inspect the particular response body; not every error uses one code schema.

A proxy test can return HTTP 200 with `success: false`. Upstream-save warnings, 409 capacity errors and cleanupPending have different meanings. Automation should inspect both status and response fields.

Counts is a client interface, documented in [library counts](media-counts.md), not an admin route.

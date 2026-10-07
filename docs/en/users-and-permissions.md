# Users and permissions

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../users-and-permissions.md)

Applies to the V1.4.9 mainline.

Administrators create local regular users and authorize stable upstream identities separately. Local user accounts and upstream accounts are managed independently.

## Roles

| Role | Access |
| --- | --- |
| Administrator (admin) | All servers, panel and admin APIs; watch state comes directly from the upstream Emby account |
| Regular user (user) | Assigned servers only; local WatchStore isolates client-visible progress, played state, favorites, Resume and NextUp |

## Independent watch history

Distributed users share one upstream Emby account, whose progress, played state and favorites are inherently shared. Playback reports and explicit user actions still follow the original upstream interfaces. EIO additionally maintains each regular user's own WatchStore. Automatic completion updates local state without an extra upstream played-state request. Client reads treat **local records as authoritative**, so another regular user's changes to the shared upstream account do not overwrite local state.

| Feature | Administrator | Regular user |
| --- | --- | --- |
| Resume | Upstream data | Independent local data |
| NextUp | Upstream data | Calculated from local progress |
| Played | Upstream data | Independent local record |
| Favorite | Upstream data | Independent local record |
| Browse-page UserData | Upstream passthrough | Overlaid local state |
| Favorite/played/unplayed/resumable filters | Upstream filtering | Local-record filtering |

**Local filters**, introduced in V1.4.4 with IsUnplayed completed in V1.4.5:

- Filters=IsFavorite, IsPlayed, IsResumable and IsUnplayed use local records. EIO retrieves candidates for the directory, evaluates the current user's WatchStore, then sorts, paginates and corrects totals locally. With IsUnplayed, an item without a local record is unplayed; only explicit local Played=true excludes it. ParentId, Recursive and IncludeItemTypes directory constraints still execute upstream.
- SortName, DateCreated, ProductionYear and CommunityRating are sorted locally. Other keys such as DatePlayed fall back to local recent-play/favorite time.
- Unknown filters such as IsFolder are forwarded unchanged rather than silently dropped.

**Known limit:** Likes, Dislikes and IsFavoriteOrLiked retain shared upstream semantics because WatchStore does not record likes/dislikes. These filters return X-Emby-In-One-Filter-Notice and produce throttled WARN logs.

**Pagination:** aggregate `GET /Users/{id}/Items` without ParentId paginates **after merging and deduplication**. TotalRecordCount and StartIndex refer to merged order. Local filters likewise collect candidates before local sorting/pagination.

Both paths retrieve candidate sets rather than one upstream-filtered page. Each upstream supplies at most 5000 **raw candidates per request**, not merged works or versions. Beyond this limit, totals are a lower bound, the list tail may be unavailable and logs warn.

**Event handling:**

- Playing/Progress report upstream first. Only a 2xx acknowledgement writes regular-user local progress; failure retains the previous state.
- Valid Progress at 90% of corresponding media duration marks played; Stopped performs a final check. Playing only establishes context and does not complete from its starting position alone.
- Modern Stopped retains terminal semantics and closes the exact device/session lease. Legacy stop handlers with a selected target also save local terminal state; without a selectable target, they preserve the original response.
- Explicit played/unplayed/favorite operations retain their original interfaces. User deletion clears that user's local watch data.
- Item type, series title, season and episode metadata are filled when needed for local state and NextUp.

**Automatic completion:**

A regular user's Movie/Episode is locally marked Played=true, with resume position reset to zero, when valid Progress or Stopped reaches **90%, including exactly 90%**, of a reliable duration. Browsing, fetching playback URLs, video GET/HEAD/Range and disconnections do not trigger it. A valid report after seeking to 90% qualifies: this measures position ratio, not cumulative viewing or credit detection.

Duration comes, in order, from the valid report, a cache matching the same user/real device/session/media source, and metadata for the selected upstream item/media source. Multiple versions must match actual MediaSourceId; EIO does not choose the first source or directly reuse historical duration. If source, item type or duration cannot be established, only confirmed progress is retained. Live media and Failed=true stops do not newly complete. Metadata completion has deadlines and retry limits; sufficiently complete normal reports do not require repeated metadata requests.

If Stopped omits position or sends null, only a fully matching session's last valid position can substitute. Explicit zero or invalid positions are not replaced from cache. Missing session/source identity cannot borrow another playback's position unless a unique source is proven. Cache is bounded and expires; pre-restart session cache is unavailable.

Played state survives normal reports, low-position stops and replay. Replay does not automatically reenter Resume; explicitly marking unplayed clears completion. Manual Played/position changes invalidate old session-cache evidence, though a new valid qualifying event can complete again. Reads display local Played, resume position, completion percentage and last-played time. Completed items leave Resume; NextUp follows existing episode rules, and favorites remain.

Modern Sessions Playing/Progress/Stopped and legacy PlayingItems start/Progress/DELETE stop/POST Delete share the decision. Direct, proxy and STRM/HTTP Path video requests remain supported by their existing paths. Without sufficient control reports to EIO, automatic completion cannot be guaranteed. Administrators retain upstream-account state. Local automatic completion adds no upstream PlayedItems request; original reports and explicit actions still forward through their original interfaces.

## Shared state across upstreams and rebinding

Watch state is keyed by **local user + merged Virtual ID**. Proven merged A/B versions share progress, played state and favorites for one regular user; different users remain independent. The server in a watch row locates resources, rather than making visibility depend on the last-played source.

| Operation or condition | Watch state and playback source |
| --- | --- |
| Unbind A while B remains bound with the same work | Retain shared state; obtain resources/versions from B and resume at the saved raw position |
| Remove every relevant binding | Hide local history, UserData and watch-filter results while retaining records; rebinding the same existing server ID can restore visibility |
| A offline but bound, B online | Retain history; play/update through B; A reads the latest shared state after recovery |
| All authorized instances offline | Retain history; Resume/NextUp omit unavailable items when online metadata is required |
| Delete A while an instance on B survives | Retain Virtual ID/shared state, migrate resource location, remove A's instances, authorization, caches and related hidden-library settings |
| Delete A with no surviving instance | Remove orphan mappings and watch rows; late requests cannot recreate deleted records |
| Re-add A at the same address after deletion | New server ID; no restoration of A-only history/identity cache. If it merges into existing B, use B's retained shared state |
| Delete a regular user | Clear their watch state, favorites, authorization, hidden libraries, tokens and relevant caches without changing other users' state |

Current bindings and proven instances determine visibility. Filter before grouping, sorting, pagination and counting; rows do not receive a permanent hidden marker. Hiding home-library entries does not unbind a server or change watch-state access.

Only currently authorized sources appear as versions. Selecting B always uses B's actual item, media source and session. An old A version is rejected after authorization is lost; its raw ID is not silently sent to B. Duration caches stay version-specific. Shared state uses established merge relations; new associations follow [merge rules](media-merge.md), without content fingerprints or progress conversion between durations.

## Shared-progress write boundaries

The latest successful Started context with provable identity owns shared writes. Progress/Stopped must match its real user, device, session, source/version and generation. Late old events cannot overwrite newer committed positions or manual state. Valid backwards seeks save the new position, not the historical maximum. Explicit Played/position edits invalidate old context; favorite-only changes preserve it.

Without real-device, session or version-ownership evidence, EIO does not guess the latest session and skips local automatic shared writes. Restart/cache eviction requires a new valid Started to establish ownership. When a client fully reuses the same device/source/work/version/session IDs, the protocol has no extra generation field, so not every late newly arriving packet can be distinguished.

Inheritance after deleting A covers only **valid in-flight reports authenticated and admitted before deletion**. Final checks still require an enabled user, authorization to B, a surviving merged identity, version evidence and write generation not superseded by new playback/manual state. It updates only shared state and B's location; it does not send A's raw ID to B. Requests newly started after deletion with revoked tokens remain rejected. Without an authorized inheriting instance or sufficient evidence, discard the write.

## Management changes and recovery

Binding, enable/disable and password updates advance authorization revisions and revoke the user's old tokens, so later control requests may require relogin. Removing one binding cleans only that user's corresponding routes, caches and lease, preserving their B lease and other users' data. Disable/password changes clear the user's playback context. Local cleanup cannot immediately reclaim a direct URL already handed out or media already transmitting.

Destructive management changes fail when database persistence or cleanup-journal storage is unavailable. Token/identity-file cleanup failure after database commit reports cleanupPending, not complete success. If server configuration was persistently removed, restoring A's configuration is not used to fake rollback. Regular-user access pauses during pending cleanup; administrators can retry the management operation. Startup recovers cleanup before upstream login/service availability and stops if recovery fails. There is no background automatic retry worker. Successful cleanup removes the journal.

Legacy users/tokens default to authorization revision 0. The watch unique key remains; unbinding does not clear the watch database. Successful passthrough identity cache uses stable server IDs. Historical address keys migrate only with provable unique ownership; ambiguous related captures are conservatively removed during cleanup and may need a new client login. See [validation scope](release-v1.4.9-validation.md). These behavior rules do not establish exhaustive testing of every recovery boundary or client.

## Create regular users

Administrators can use:

1. **Panel:** 用户管理 (user management).
2. **SSH menu:** `emby-in-one`, then the add/delete regular-user option.
3. **REST API:** `POST /admin/api/users` with an administrator token.

## Configure accessible servers

Regular users access only stable serverId values explicitly listed in allowedServers. No selected server means **no upstream access**. On creation, omitted, null or [] grants none. On update, omitted or null preserves existing authorization, while explicit [] clears it. Granting every existing server requires listing every ID; later additions still need explicit authorization. Administrators always access all upstreams.

## Authorization capacity

The panel label **同播数量限制** maps to maxConcurrent, counting assigned **regular users per upstream**, not general concurrent media streams.

- 0 (default): unlimited authorization capacity; positive integer: assigned-user limit; negative values invalid.
- assignedUsers is the current number of authorized regular users. Administrators use no slot.
- Explicit authorization consumes a slot. Stopping playback, an offline server or disabling a user does not revoke assignment. Unbinding/deleting the user releases the relevant slot.
- Excess new authorization: `409 UPSTREAM_CAPACITY_FULL`.
- Reducing below assigned users: `409 UPSTREAM_CAPACITY_BELOW_ASSIGNED`.
- Conflict responses include code, message, serverId, limit and assigned. The panel retains the unsaved form and refreshes capacity; final enforcement is backend-side.

## One active device per regular user and upstream

Playback leases are isolated by `(UserID, ServerID)`. One active DeviceID holds a regular user's lease on one upstream. The same user can play on different upstreams simultaneously; administrators are exempt. The same device may reuse its lease across item/session changes. Another device while the lease is valid receives `429 PLAYBACK_DEVICE_LIMIT`.

DeviceID precedence is X-Emby-Device-Id, DeviceId in X-Emby-Authorization, DeviceId in Authorization, then DeviceID stored in the current validated token. A regular-user playback lifecycle without valid DeviceID returns `400 PLAYBACK_DEVICE_ID_REQUIRED`. A lease without a heartbeat for 3 minutes can be cleaned or taken over. Stopped releases only the matching device and exact PlaySessionID; an old Stopped cannot release a newer session.

## Playback acknowledgements and errors

Playing/Progress writes local state and refreshes that device's lease heartbeat only after upstream HTTP 200–299. Success returns empty 204; the upstream body need not be JSON. Failure does not update local state.

| Upstream result | Playing / Progress response |
| --- | --- |
| Missing/offline client | 503 `UPSTREAM_SESSION_UNAVAILABLE` |
| Transport/DNS/TCP/TLS/observable cancellation | 502 `UPSTREAM_SESSION_FAILED` |
| Deadline/network timeout | 504 `UPSTREAM_SESSION_TIMEOUT` |
| Non-2xx HTTP, including upstream 401/403 | 502 `UPSTREAM_SESSION_REJECTED` |

Stopped retains terminal semantics: on success, ordinary upstream failure, offline or missing client, it attempts to save local progress proven for the current session/version/write owner, closes the exact lease and returns 204. Request-preparation errors retain original 400/503 responses. Missing DeviceID returns 400 without upstream reporting, lease release or shared automatic writes based on missing identity. Valid Movie/Episode Progress/Stopped at 90% follows the completion rules above. Playing, unknown source/duration, live media and Failed stops do not newly complete. Public errors and lifecycle logs omit upstream bodies, URLs and credentials.

## Password and edit semantics

Regular users may have empty passwords. Nonempty passwords follow current admin-interface validation, **8–128 bytes**. Empty on creation creates an empty-password account. Empty in the panel editor **clears** the password; a value replaces it, rather than empty meaning preserve. For API updates, omitted or null password preserves it; explicit `""` clears it. Administrators do not use this empty-password rule.

Regular-user scrypt hashes and AES-GCM password_secret live in mappings.db, with user-password.key in the actual dataDir. Back them up together. An old hash alone cannot generate the recoverable secret. Upstream username authentication likewise accepts an empty password but still requires a username; see [upstream authentication](playback-and-upstream.md#upstream-authentication).

## Home-library hiding

Administrators can hide a server's or selected libraries' home entries per user. This only affects home-entry display; it does not remove allowedServers authorization. Search, latest items, Resume and counts still follow access and their own rules. Unbind the server to revoke access. See [admin display interfaces](admin-api.md#library-entries-and-display-settings).

# Media library counts API

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../media-counts.md)

Applies to the V1.4.9 mainline.

V1.4.9 provides this interface. Hills requests carrying language parameters received a compatibility fix and user acceptance.

Authenticated clients use `GET /Items/Counts`, also available as `/emby/Items/Counts`. Success returns only three nonnegative integers: MovieCount, SeriesCount and EpisodeCount.

Regular users sum the intersection of current valid server bindings and token authorization; administrators sum all currently configured servers. Only online servers participate. Home-hidden libraries remain included in their server's counts.

Each server has one shared in-memory cache sourced from official Counts for the configured upstream account's visible media. Duplicate works on different servers are summed, not deduplicated. There is no second version-count group or item traversal to fill totals. This differs from merged media lists. Hills' six display locations do not create six counting definitions; requests with or without UserId return the same scoped three fields.

Background initialization follows startup preparation, with hourly refresh. Actual binding changes register additional refreshes for the currently selected online servers. Same-source work is coalesced, with at most two collection workers globally. Client reads only inspect cache: they trigger no refresh, login or probe. No manual refresh interface exists.

A complete successful value replaces the previous one. Failure/rate limiting for the same account does not discard a prior successful value based on age. Server deletion or changes to the account/connection scope isolate old values. Restart begins with empty cache.

After an ordinary Counts failure, background work checks online status through the same upstream API/identity path. Only if confirmed online does it append at most one Counts request. Explicit rate limiting or a wait condition prevents the extra request in that round; Retry-After is honored, with backoff for 429 lacking usable wait information. This governs counts refresh only, without pausing playback or other existing business. An authorized online source without a complete current cache causes a whole-response 503.

| Condition | Client result |
| --- | --- |
| All authorized online sources have complete current cache | 200 with three per-source sums |
| No bindings, or all authorized sources explicitly offline | 200 with three zeros |
| Any authorized online source lacks complete cache | 503 COUNTS_UNAVAILABLE, no partial counts |
| Lifecycle cleanup/recovery incomplete | 503 COUNTS_LIFECYCLE_PENDING |
| Unauthenticated, invalid token, deleted/disabled user | 401 |
| Another user or a real upstream UserId selected | 403 COUNTS_USER_FORBIDDEN |
| Malformed/duplicate parameters or empty UserId | 400 INVALID_COUNTS_QUERY |
| Any unsupported filter/source-selection parameter | 400 COUNTS_FILTER_UNSUPPORTED |

UserId may be omitted or identify the current local user/a compatible legacy alias. The alias still denotes the current token user. Parameter names are case-insensitive; duplicate members of the same parameter family are rejected even with equal values.

IsFavorite=true, false and empty are all unsupported. Played/resumable filters, library selection, pagination and arbitrary extra parameters are unsupported too; EIO does not silently substitute totals for a filtered result. Allowed identity metadata, including Hills' X-Emby-Language, is consumed locally, never forwarded or used to change count scope.

HEAD applies the same validation and status/JSON length with an empty body. Other methods, including OPTIONS, return 405 after authentication with `Allow: GET, HEAD`. All results use `Cache-Control: private, no-store`, without ETag/Last-Modified or conditional 304. Local responses expose no upstream credentials, server lists, raw upstream errors or partial totals.

See [release validation](release-v1.4.9-validation.md) for evidence and limits.

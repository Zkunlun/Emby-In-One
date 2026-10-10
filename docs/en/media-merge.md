# Media merge rules

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../media-merge.md)

Applies to V1.5.0. Merging happens when requests encounter candidates while browsing, searching, listing seasons/episodes or fetching details. Startup and user creation do not scan every upstream's full library.

| Object | Identity evidence |
| --- | --- |
| Movie or series | Same type; compare TMDB / IMDb / TVDB IDs within the same namespace |
| Season | Proven same parent series and explicitly matching season number |
| Episode | Proven same parent series and explicitly matching season/episode numbers, such as S1E1 |
| No valid directly comparable provider ID | Complete name + year, matched exactly after lowercasing English ASCII letters |

Any conflict in a shared provider ID rejects a new merge even if another ID matches. There is no fuzzy matching of translations, punctuation or spaces, or online provider-ID mapping. Missing required name/year/numbers keeps items separate. Explicit season 0 is valid; episode numbers must be positive integers. Episode title/year does not replace parent-series identity, and conflicting shared episode IDs still reject a new relation.

Duration does not gate identity. Different, unknown, zero or unreliable runtimes do not independently prevent one work from being grouped. Each version retains its own duration, quality, codec and audio tracks. There is no content fingerprinting or timeline conversion.

## Version retention and selection

Versions use `(server_id, original ItemID, MediaSourceID)` locators. Distinct items on one server, several sources on one item and cross-upstream versions follow the same identity rules. A with 8 versions and B with 2 retains 10 when identity matches; a conflict keeps the 8 and 2 in separate groups. Equal names, quality or duration do not delete distinct locators.

Repeated identical locators are idempotent. Partial responses add known versions rather than remove historical members. Without a media source, identity evidence can be retained, but no fabricated playback route is created. Lists, details and PlaybackInfo expose currently authorized usable sources from actual responses. Temporary upstream failure does not delete persistent version relationships. Explicit playback selection still routes to the exact chosen version.

## Old links and watch state

Stored relations are not proactively split because metadata changes. Requests proving that formerly separate groups are the same work may coalesce them. Old Virtual IDs remain aliases; historical evidence and watch records remain.

One regular user shares played state, favorites and the raw resume position within a merged group. Different users and different work identities remain isolated. Different cuts share state too, without position conversion.

## V1.5.0 passive discovery and scanner boundaries

- Global search, aggregate lists and passive episode/season discovery of mapped series no longer stop at the former per-upstream 5,000-candidate cap. They page using validated upstream cursors until the requested window or a valid exhaustion condition. Aggregated TotalRecordCount may be provisional; unindexed global sorting can be expensive and concurrent upstream changes prevent a fixed cross-request snapshot.
- Browsing one upstream library remains source-scoped; this release does **not** create a combined A+B virtual library.
- The optional administrator Scanner is **disabled by default**, requires both global and per-source grants and only proactively discovers Movie/Series. It never actively crawls Seasons/Episodes, artwork or playback streams. The 05:00 Asia/Shanghai delta does not catch up missed slots; date fallback is best-effort.
- Indirect contradictory provider evidence quarantines the entire merged work group from unsafe automatic cross-source routing. Source lifecycle fences block late index writes. Library counts still sum official per-upstream values instead of deduplicated works.

## Display metadata priority

For a work found on multiple servers, one server supplies the primary title, overview and images:

| Priority | Rule | Reason |
| --- | --- | --- |
| 1 | Server with priorityMetadata: true | Explicit preferred metadata source |
| 2 | Overview contains Chinese characters | Prefer Chinese-localized metadata |
| 3 | Longer Overview | Prefer a fuller description |
| 4 | Smaller server index / earlier configured order | Stable fallback |

This determines display metadata only. MediaSource versions from every server remain retained and selectable.

## ID virtualization and persistence

Each upstream Item ID maps to a globally unique virtual ID: 16 random bytes (128 bits) from crypto/rand, displayed as 32 lowercase hexadecimal characters without hyphens. Client-visible IDs are virtualized.

- **Storage:** SQLite WAL persistence with a memory cache.
- **Relation:** virtualId <-> { originalId, serverId }, plus persistent otherInstances. serverId is stable and independent of configuration order.
- **Restart:** main/additional instance relationships restore without rebuilding mappings. Historical server_index records migrate to server_id.
- **Upstream deletion:** if proven instances survive in current configuration, deleting a primary or secondary instance retains the Virtual ID and shared watch state, promotes a survivor and relocates watch resources as needed. Only orphans with no remaining instance lose mappings/watch rows. Unbinding changes a user's authorized visibility; see [shared state and rebinding](users-and-permissions.md#shared-state-across-upstreams-and-rebinding).

All library entries remain, with a server-name suffix. Candidates retain existing round-robin interleaving. Merging and version retention do not expand current authorization.

[Release validation](release-v1.5.0-validation.md) · [User watch state](users-and-permissions.md)

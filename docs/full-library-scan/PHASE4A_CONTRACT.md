# Phase 4A — Passive Collection Contract & Source Boundary Freeze

Status: Phase 4A read-only production-code audit; implementation is reserved for Phase 4B–4D.
Baseline: local `dev/full-library-scan-phase3-local` at `17bcf02d4fe603f0a1bcbe193aeed57335113203`.
Authority: Notion 《全库扫描聚合》 Phase 2 Frozen Spec; `PHASE3A_CONTRACT.md`; Phase 3F passed backend regression.
Scope: Passive HTTP collections only. No Scanner, Virtual Library, remote push, production deployment or changes to the 5000 constant in 4A.

## 1. Verified code inventory (as audited, not an implemented Phase 4 fix)

| Endpoint / shape | Current execution path | Current boundary and limitations | Desired contract |
| --- | --- | --- | --- |
| `GET /Users/{userId}/Items?ParentId=A` | `media_items.go:handleUserItems` parent branch | Maps ParentId to one upstream and forwards original StartIndex/Limit; projects already-known merge IDs without querying counterparts. On local user-state filter, the upstream page is filtered locally and **can underfill or misstate totals**. | Remain source-scoped, never fanout to B. Keep raw source paging if no local filter; where filter is EIO-local, filter before final user-visible pagination or explicitly handle its incomplete-count limitation. |
| `GET /Users/{userId}/Items` without real ParentId, including `SearchTerm` | `handleUserItems` global branch → `requestMergedCandidateSet` → `fetchItemsAcrossUpstreams` → `mergedItemsPayload` → `paginateItems` | Query StartIndex forcibly reset to 0, Limit to 5000 (unless local state filter path). Results merged in upstream row-round-robin order, not globally sorted by all client SortBy fields; TotalRecordCount is count of fetched canonical tiles, not complete global cardinality. | Window-driven pageable acquisition, canonical dedup, authoritative global sort/filter before final offset/limit, with **no fixed total-candidate cap**. |
| `GET /Items` without Ids | `handleItemsCollection` | This endpoint is NOT universally a global listing: outside local user-state filter, current code falls back to upstream proxy. | Preserve the routing distinction. Never silently rewrite unrelated `/Items` request semantics into global discovery. |
| `GET /Items?Ids=…` (or ItemIds) | `handleItemsCollection` batch branch | Explicit finite ID set; source-qualified translation, merge and local subset filter; no 5000 path. | Keep ID-scoped, authenticated; never trigger full-page acquisition. |
| `GET /Users/{userId}/Items/Latest` with ParentId | `handleUserItemsLatest` source branch | Single upstream; response is an ARRAY, unlike normal Items object. Current code checks only exact `ParentId` key; aliases and root sentinels differ from `handleUserItems`. | Source-scoped; preserve array output; normalize ParentId aliases and root sentinels consistently, without widening to multi-source by accident. |
| `GET /Users/{userId}/Items/Latest` without ParentId | `handleUserItemsLatest` fanout | Multi-source but upstream window is forwarded and items are row-round-robin folded; no guaranteed unified Latest ordering or global page semantics without local state filter. | Cross-source Latest remains bounded/window-based and ARRAY-compatible, not a full-library hydration. Order/freshness and its best-effort limitations explicit. |
| `GET /Shows/{seriesId}/Seasons`, `/Episodes` | `library_image.go:handleMergeShows` | Only allowed known Series instances visited; per-source `SeasonId` translates to local original. When no local filter, `requestMergedCandidateSet` forcibly sets `Limit=5000`; local Season/Episode sort then local paginate. Source errors currently skipped. | Iterate bounded pages of **only these known source-qualified Series** until the requested window or full semantic result is proven; no fixed 5000 cap, no active Episode scan. |
| `/Items/{itemId}`, `/Users/{userId}/Items/{itemId}`, PlaybackInfo | `media_merge_detail.go`, `media_playback.go` | Uses known/authenticated instances and Phase 3 evidence/route safeguards. | Preserve depth-on-click; don't initiate arbitrary cross-source full-library traversal. |
| `/Search/Hints`, `/Shows/NextUp`, `/Users/{id}/Items/Resume` | `library_image.go`, `media_nextup.go`, `media_resume.go` | Separate endpoints, distinct payloads and user state/ordering. Not all use `requestMergedCandidateSet`. | Inventory/verify as compatibility regression: no blanket replacement of all list handlers with a single Items response protocol. |

### Additional code-level observations
- `media_items.go:requestMergedCandidateSet` currently implements `StartIndex=0; Limit=5000`; `warnTruncatedMerge` reports but does not fix tail loss.
- `media_merge_http.go:mergeHTTPItems` is group ownership/evidence mutation + row-round-robin output projection, **not a general global sorting/paging engine**. The canonical dedup must remain authoritative; a new collection pager must sit above it.
- `media_merge_http.go:hydrateMergeResults` may perform extra `/Items?Ids` metadata hydrations for client-observed entries. Preserve Passive depth semantics, but bound request amplification. Active Scanner remains lightweight and never inherits this hydration path.
- `aggregation.go:aggregateUpstreams` may return partial results at grace deadline and record late results in background. A response produced from partial results cannot be certified as a complete globally sorted window or exact count.
- `media_items.go:paginateItems` currently derives `TotalRecordCount=len(fetched)` and clamps displayed `StartIndex` to the fetched length. This must not be used as proof of global total cardinality.
- `user_filter.go:localItemSort` implements a limited comparator with local user recency tie-break; it is **not** a general equivalent of upstream order for every Emby SortBy key.
- The no-ParentId global branch with EIO-local user-state filters also skips `requestMergedCandidateSet`, forwards the client's window to upstreams, and then filters locally. This can underfill a page and cannot certify a complete filtered TotalRecordCount; fix ownership belongs to 4B/4C, not 4A.
- Representative metadata can change after an additional canonical member is encountered (priorityMetadata/overview selection). A sorted upstream's next-key bound alone may therefore fail to certify the **post-merge** global output prefix; 4B must verify the representative-selection interaction before enabling early termination.

## 2. Frozen request classification

1. Normalize the client query once; distinguish absent/empty/`0`/`root` ParentId from a valid source-qualified ParentId. Treat case variants consistently and fail closed on malformed or ambiguous identifiers.
2. Valid ParentId tied to A → requests only A for the collection; A's tiles may display established virtual/canonical IDs, but B entries must NOT be listed separately. Single-source browse does not use a multi-source index scan to discover counterparts.
3. No ParentId global Items/SearchTerm → fanout **only current authorized** upstreams; use phase-3 shared merge mutation/index, never raw metadata equality as sole canonical truth.
4. ID-qualified `/Items?Ids` and known Series/Detail/Playback → seek only the authorized, specifically known/selected source members; no collection-wide acquisition.
5. `/Items/Latest` and `/Search/Hints` have their own response contracts; do not convert array payloads into Items envelopes.

## 3. Frozen collection window and safety invariants (implemented in 4B–4D)

- Parse `StartIndex=S>=0`, `Limit=L>=0` safely; reject unbounded/overflowing `S+L` rather than overflowing or silently truncating. An omitted or zero Limit follows the endpoint's compatible client semantics; never silently interpret it as permission for uncontrolled upstream fetches.
- For deterministic sortable global collections: acquire bounded pages per source, apply EIO-visible source/user filters, canonical dedup, stable global ordering, then slice `[S:S+L]`. A per-source `S+L` prefix is at most an initial lower bound, **not a sufficient stop rule** under duplicates, deferred identity upgrades, or unsupported upstream sorting.
- A deterministic Top-K prefix can stop early only when all unfinished sources have certified ordering bounds (including source-specific filter/order semantics), so unseen records cannot precede the emitted page. Otherwise continue within request resource budgets or return an explicitly incomplete/error outcome; never silently fabricate a correct complete page.
- Stable tiebreak uses source-qualified item IDs/canonical group identity, not incidental goroutine completion order. Identity/group absorption within a request must cause dedup/revalidation before output pagination.
- `SortBy=Random`, undocumented upstream ordering, and concurrent upstream data edits have explicitly non-snapshot/best-effort semantics. Do not pretend deterministic cross-page pagination where ordering bounds cannot be established; no new background full-library scan is authorized.
- A global `TotalRecordCount` may be labeled exact only once all eligible sources are exhausted and visibility/canonical dedup completed. It must stay a numeric Emby-compatible field; provisional count and client continuation behavior need explicit mock-client regression during 4B, not a fake sum of upstream raw totals or the legacy 5000 value.
- When the request requires an exact deterministic result but a source fails, stalls, or returns inconsistent pagination metadata, fail explicitly rather than mark that source exhausted. Preserve the existing best-effort grace response behavior only for endpoints whose contracts explicitly permit partial results, and do not claim an exact total for partial responses.
- Bound in-flight pages, memory and upstream request rate; repeat pages, nonprogressing cursors, contradictory `Items/TotalRecordCount/StartIndex`, retries without progress, context cancellation, authorization/source-generation revocation all terminate correctly and do not commit false exhausted state.
- All passive writes use `MergeDiscoveryService` (proof, quarantine, WorkIdentityIndex), request-scoped current permission check and `source identity generation` fence. A group `conflict_quarantined` or `needs_revalidate` cannot auto-route playback; explicitly source-qualified single-source playback retains its separate checks.
- Passive collection result caching must not outlive authorization/source-generation or state-filter changes without reliable invalidation. Phase4 introduces no Scanner tasks or scan checkpoints.
- `/Shows` Episode/Season depth pages may iterate an entire **known Series** when a complete Series-level result is truly needed; this is not permission to scan every TV series.

## 4. Phase 4B–4D handoff contracts and test fixtures

**4B, global root**: benchmark three upstream mock datasets with 10k+ items each, overlap/duplicates, source-specific sorted and unsorted lists, `StartIndex=0/24/5000/9990`, Limit 1/20/50, stable ties, user visibility filters, asynchronous late sources, source deletion/recreate during paging. Validate semantic output, request count amplification, cross-page compatibility and non-5000 tail.

**4C, SearchTerm + Latest**: search across allowed sources only, >5000 matches, source failures and grants revoked mid-query, no cross-source automatic strong-provider contradictions; Latest remains an array and doesn't turn a first-page request into an unbounded scan. `/Search/Hints` retains hints envelope and security requirements.

**4D, known depth**: >5000 Episodes belonging to one known Series, SeasonId aliases translated correctly per source, sparse differing seasons, duplicate Episode versions, 401/403/429/5xx and source offline, known Detail/Playback does not expand unrelated libraries.

**4E gate**: no extra startup/active Scanner network requests; tests of source-only browse + authorized global search + Series depth, first-page upstream call counts and relevant user state, full backend regression and selected race tests; real-client tests only in isolated approved environment. Keep zouter production untouched until separately authorized.

## 5. Outstanding implementation choices for Phase 4B (explicitly NOT frozen as truths in 4A)

The exact bounded upstream page size and request pacing, fallback when an upstream cannot provide a compatible sort bound, and the numeric provisional `TotalRecordCount` compatibility strategy require targeted behavior/performance tests. They must be decided and documented **before** 4B starts changing user-visible pagination. None of these choices licenses a fixed total-candidate cap, missing-tail success response, or unsolicited full-library fetch.

## 6. Phase 4A acceptance

- [x] Exact local Phase 3F base verified; no production modification in audit.
- [x] Entrypoint and request semantics audited against existing code and Phase 2 spec.
- [x] Source-scoped / global / ID-known classification preserved.
- [x] Root, Search, Latest, Shows and local-filter risks inventoried with 4B–4D ownership.
- [x] Source-boundary regression tests passed: `go test ./internal/backend -run '^(TestPhase4A|TestTask6Phase4ParentRawPageBoundary|TestShowsEpisodesTranslateSeasonIDAndDeduplicate|TestShowsSeasonsMergeAndPreserveAdditionalInstances|TestPhase3E|TestPhase3D)' -count=1 -timeout=6m` → `ok emby-in-one/internal/backend 5.639s` (exit 0).
- [x] Audit contract committed locally and recorded in Notion. No Phase 4B implementation, GitHub push or deployment.

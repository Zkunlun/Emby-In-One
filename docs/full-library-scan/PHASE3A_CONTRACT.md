# Full-library aggregation — Phase 3A frozen contract

**Status:** Phase 3A contract baseline / review gate; implementation NOT started.  
**Date:** 2026-10-09  
**Base commit:** `cacd226767cbe991aede47bb3926b4b9b1b31738` (Phase 1, 28 commits ahead of main / 0 behind at verification).  
**Branch:** `dev/full-library-scan-phase3-shared-foundation`.  
**Reference:** Notion 《全库扫描聚合》 Phase 2 CLOSED/FROZEN plus the four Phase 3A corrections. This contract refines implementation boundaries; it does not amend the Phase 2 product policy.

## 0. Ownership and exclusions

- **Active Scanner** discovers Movie and Series breadth only (normal library pagination). No Season/Episode scanning, detail hydrate, MediaSources, Images, PlaybackInfo, per-item Detail, or /Items?Ids hydration.
- **Passive HTTP** continues Search/global discovery, Details/Playback/version depth, and Series->Season/Episode on demand. Scanner OFF does not disable Passive.
- Shared MergeStore/IDStore retains authority over final identities, group memberships, canonical IDs, aliases and routes. WorkIdentityIndex is an expendable derived candidate locator, never a second merge store. Scanner-specific run/checkpoint persistence belongs to Phase 5.
- Phase 3A **must not create schema, migrate data, change Go runtime behavior, fetch from upstream, deploy to zouter, or alter Draft PR #1**. 5000-window pagination remains a Phase 4 task; virtual-library collection remains outside this release.

## 1. Branch and review boundary

- The Phase 3 independent development branch starts **exactly** at the Phase 1 HEAD SHA shown above; do not write Phase 3 changes onto the Phase 1 Draft PR branch or rebase Phase 3 onto main to reconstruct SampleCollector.
- Keep 3A commits documentation/contract-only. Review subsequent 3B–3F independently. Draft PR #1 remains the Phase 1 audit boundary.
- Before each implementation phase, verify current HEAD and tree status; if the base or remote history unexpectedly changed, stop and report.
- If Phase 3 receives its own PR before Phase 1 merges, target the Phase 1 branch for a clean Phase 3-only diff; after Phase 1 merges, retarget deliberately.

## 2. WorkIdentityIndex admission and backfill

### 2.1 Unit of identity

- Source item identity is `(server_id,item_id)` and its **typed Movie/Series anchor member**. The `source_id`/MediaSource version members of the same Movie must not create duplicate work rows.
- Only `Type=Movie` or `Type=Series` is indexable, with nonempty configured source/item identifiers and at least one valid match key.
- A valid key is (a) an explicitly recognized nonblank provider namespace+value (Tmdb/Imdb/Tvdb), or (b) a nonblank, non-placeholder normalized Name plus valid ProductionYear [1,9999]. Follow existing `mergeLowerEnglish` semantics (ASCII-case fold only; preserve whitespace/punctuation/Unicode). No fuzzy translation or implicit normalization change.
- Season/Episode, empty/unknown Type, legacy raw locator, untyped/typed placeholder without independent valid identity, bare virtual ID, bare canonical group ownership, and incomplete Name/Year **without** a valid provider key are **not** admissible. A legitimate typed anchor with good identity may be indexed even if there are no MediaSources.
- Use only observed, source-qualified typed identities, never infer identity from LegacyItems, route locators, watch state, alias, or membership co-location.

### 2.2 Backfill semantics

- Traverse existing persisted MergeStore snapshots but filter members by the rules above; `full traversal != full inclusion`.
- If a source item occurs as an anchor and multiple version members, collapse to **one work row**; if their evidence disagree, do not arbitrarily choose one: record ambiguous/needs-revalidation and omit uncertain keys pending reconciliation.
- Current `canonical_group_id` may be derived from `ResolveMergeMember` + canonical alias resolution, but is **not a proof of equality**. Backfill must never merge, split, synthesize proof, or modify watch state.
- For a key mapping to multiple possible groups, return ALL distinct candidates in stable order and `ambiguous=true`, never `first match wins`. Same key is a lookup hint, not a uniqueness guarantee. Strict comparator and group-level audit decide whether an association is permissible.
- Identity changes replace lookup keys atomically; obsolete keys cannot survive as false candidates. Group absorb/alias changes update derived canonical locators. Source delete removes all its rows in the same lifecycle transaction and/or fails closed while cleanup is pending.
- Rebuild must be deterministic and idempotent with no upstream HTTP, and compare results against authoritative MergeStore. A failed or incomplete rebuild disables index-assisted auto-association rather than producing partial trusted matches.

### 2.3 Proposed interface-level contract (not compiled Go API)

`UpsertObservedWork(sourceKey, validatedEvidence, canonicalHint, sourceGeneration) -> changed|unchanged|rejected`  
`FindWorkCandidates(type, providerKeys, nameYearKey) -> {candidateSet, ambiguous}`  
`RebindCanonical(absorbedIDs, canonicalID)` / `RemoveSource(serverID)` / `RebuildFromMergeStore()`.

The 3B design is free to refine signatures or persistence layout without altering the above invariants.

## 3. Evidence model and comparison

Do **not** collapse evidence to one `lightweight/full` bool. It is a set of independent, provenance-bearing dimensions:

1. **NameYearEvidence:** raw/normalized Name, validated Year and per-field provenance (source endpoint/observer, capture time, presence/absence); supports fallback when strict rules permit.
2. **ProviderEvidence:** map of qualified provider namespaces to validated values; track observation provenance/time and explicit contradictions. Provider matches/conflicts compare **within** the same namespace; one matching provider cannot erase a conflict in another.
3. **ParentEvidence:** raw source-qualified parent Series ID, validated parent identity, season/episode coordinates and proof; only relevant to passive Season/Episode paths, not WorkIdentityIndex admission for children.
4. **MediaSourceCompleteness:** the current `SourcesComplete`/`fullSources` completeness/proven single-version routing evidence. It is orthogonal to identity trust; complete MediaSources with absent ProviderIds is **not** stronger work identity evidence.

- Evidence strength comes from **actual identity fields and their provenance/consistency**, not the endpoint name. A list with a reliable Provider ID can contain strong identity evidence; a Detail response with no provider remains Name/Year fallback.
- Absence in a partial observation is not deletion or a rebuttal of previously validated strong evidence. A later explicit, valid contradiction is a revalidation trigger; protect old strong values until reconciled rather than silently clobbering them.
- Persist enough provenance/previous relevant proof to distinguish `new evidence`, `unknown/missing`, `explicit conflict`, `legacy_unknown` and `needs_revalidate`. The persisted representation is designed in 3B/3D, **not** created in 3A.
- `compareMergeWork` / `compareMergeCandidates` remain the semantic baseline for **pairwise** proof, but pairwise acceptance alone is insufficient for group trust. Do not weaken Episode parent requirements or Movie/Series type isolation.

## 4. Association proofs, group-wide revalidation and quarantine

**Chosen granularity:** record evidence and conflict at **proof/association-edge level**; enforce quarantine on the **whole canonical group's automatic cross-source trust and routing**. Do not arbitrarily blame one member and do not auto-split.

- Existing `mergeStoredGroup.Proofs` contains historical pair proofs; older groups may lack them. Missing proof is `legacy_unknown`, **not** invented positive evidence. Negative provider contradictions remain durable until refuted by authoritative refreshed evidence.
- Revalidation traverses **all typed group members** and relevant proofs, not only the two newly observed members; evaluate distinct provider namespace contradictions across the entire group and invalidated historical association edges.
- Example: A(TMDB=111)—B(unknown)—C(TMDB=222) may have been associated through Name+Year. A and C are incompatible; B cannot be allocated automatically. Mark proof conflict and place the entire group in `conflict_quarantined`.
- Group state semantics: `trusted` (eligible under existing policy), `needs_revalidate` (proof/evidence changed or inconclusive), `conflict_quarantined` (explicit strong contradiction or unsafe relation). Both `needs_revalidate` and quarantine **fail closed for automatic cross-source version promotion/selection** whenever safety cannot be established. Unknown historic evidence alone is not a reason to destroy all existing associations; a changed/contradictory relation triggers revalidation.
- Preserve canonical ID, aliases, members, user watch-state and original source locators during quarantine. No implicit detach/split/alias reassignment or deletion; future corrective split is separately planned/approved.
- Quarantined canonical items must not aggregate conflicting MediaSources into a trusted version list; no auto cross-source fallback and no canonical route guessing. Ambiguous canonical playback should fail with an explicit stable error, not choose a source silently.
- **Explicit source-qualified** route/selected version may remain usable when the source is unambiguous, online (where required), validly mapped and currently authorized under `mediaAccessScopeLocked`; route is constrained to that source only. Historical aliases/mediaSource routes cannot bypass quarantine.
- All public resolution surfaces must enforce the group trust check: canonical/alias item detail and playback, Source selection, MediaSource list projections, generic pass-through/rewrites, session control, route hints, and re-resolution after network I/O. A `MergeMemberAllowed` ownership test **alone is insufficient**.
- Transition back to trusted requires a successful group-wide revalidation resolving the contradiction; missing ProviderIds or a single weak observation must never clear an established strong conflict. UI/error response shape may be refined in 3D but must be explicit, non-leaking and consistent.
- Offline/permission missing are not provider conflicts. Do not convert a simple unavailable source into a quarantined group.

## 5. Shared MergeDiscoveryService mutation contract

Single shared **App-layer facade** with caller intent `passive_request` or `scanner_authorized`; `nil reqCtx` is **not** sufficient scanner authorization.

**Input:** source-qualified observation(s), evidence provenance, exact authorized context/scan grant, expected source generation(s), optional existing canonical target.  
**Operations:** observe/register, strict associate, evidence upgrade/revalidate, publish invalidation, source/alias lifecycle coordination.  
**Output:** canonical ID, mutation outcome, evidence/trust changes, typed refusal/conflict/ambiguous errors and publication details.

Required ordered boundary:

1. Acquire no network I/O under `watchLifecycleMu`. Extract raw inputs outside the lock.
2. Under the lifecycle write gate, **recheck** request authentication and current user grants or (for Scanner) current global+source scan authority, in-flight admission rules, configured source and generation fence.
3. Snapshot all affected canonical groups, aliases, evidence/trust status. Do not trust a request-scoped permission snapshot after network I/O.
4. Validate keys and candidates. Candidate index only supplies locators; read MergeStore to verify authority and group-wide identity compatibility; ambiguous candidates cannot silently become associates.
5. In a **single consistent durable mutation boundary**, apply MergeStore, evidence/proof/group-trust state and derived WorkIdentityIndex keys; publish in-memory state only after durable success. Do not expose partial committed state; failures must preserve existing mappings. Scanner page/checkpoint transaction integration is a future Phase 6 concern; it must never checkpoint before identity/mapping commit.
6. Invalidate changed alias/watch playback caches and non-session playback route hints (preserve exact negotiated leases/sessions where safe) including absorbed IDs and trust-state changes, then return with lifecycle gate consistency intact.

**Existing entrypoints that must be migrated/audited:** `registerHTTPMerge`, `associateHTTPMerge`, `bindHTTPMergePlaceholder`, `mergeHTTPItems` in `media_merge_http.go`; `observeMergeDetail` / playback observation; `registerBackgroundIDs` in `aggregation.go` (currently raw `IDStore.RegisterMergeCandidate`); `observeLegacyShowsParent` in `media_merge_queries.go` (direct lock/write); all future Scanner writes. Also audit direct `CaptureLegacyMergeVersions`, `RegisterMergePlaceholder`, `GetOrCreateVirtualID` uses, distinguishing **work-association mutations** from safe raw ID locator registration.

**Projection / acquisition boundary:** `media_merge_http.go` continues HTTP-specific payload hydration, rewriting and projection; `media_items.go` continues source routing/window acquisition; `media_merge_detail.go` and `media_playback.go` keep network behavior; scanner engine (future) is a separate caller of the same facade. No duplicate scanner merge path or per-user canonical truth.

## 6. Source generation fence and lock order

- A source generation is a monotonic, non-reused identity epoch for stable `server_id`. Each in-flight operation captures generation at dispatch, rechecks **inside** lifecycle write gate immediately before commit; deleted/recreated same ID must never accept a stale response (avoid ABA).
- **Delete** invalidates generation and blocks old late results before durable source cleanup. Add WorkIdentityIndex/evidence/proof/source state removal to existing `pending_watch_cleanup` recovery and its atomic delete transaction.
- **Normal config Reload** changes `UpstreamClient` instance/transport/session, **not** work identity or delta cursor; don't invalidate work generation merely because URL/auth/proxy changes for the same logical upstream. Retired-client responses still require authorization, valid source and operation-specific staleness checks; subsequent Scanner pages must resolve `ClientByID(server_id)` again and must not retain clients across run.
- **Disable global/per-source scan or Pause:** no new page; an already authorized in-flight page may finish/commit under the Phase 2 rules, with control-plane revision serialization protecting newer pause/stop state. Distinguish scan command epoch from source-deletion identity generation; do not incorrectly reject a legal in-flight page solely because scan was disabled.
- Lock order must preserve `watchLifecycleMu` (write) -> `watchPlayback.mu` -> `UserStore.mu` -> `IDStore.mu` -> `WatchStore.mu` -> `HiddenLibraries.writeMu` -> shared SQLite transaction where applicable. Never call public store getters/setters while holding their own mutex; never perform upstream I/O under lifecycle or SQLite write locks. Avoid inverse dependencies in playback-route invalidation.
- Crash/failed cleanup: `lifecyclePending` fails closed and recovery completes durable deletion before scanner/index service can resume or accept stale source observations.

## 7. Invariants and 3B–3F gates

1. Canonical ID and existing aliases are stable after group absorb, evidence upgrade, quarantine and attempted repairs; no automatic split.
2. Media source membership and playable route qualification remain distinct from work identity; `SourcesComplete` never automatically upgrades work evidence.
3. All shared persisted mutation paths are permission-gated; global mapping never permits exposure of unauthorized sources.
4. Legacy/placeholder/untyped and Season/Episode entries never pollute WorkIdentityIndex.
5. No conflict can be hidden by a matching different Provider ID or by Name+Year fallback; A111–Bunknown–C222 quarantines without choosing B.
6. Source deletion, alias absorption and evidence key changes are atomically reflected or index is disabled pending safe rebuild.
7. Passive Search / Detail / Episode discovery / Playback continue working without Scanner; Phase 3 introduces no background scan traffic.
8. During Phase 3, do not modify Phase 4's 5000 candidate cap yet, and do not introduce Phase 5 run/scheduler schema.

**3B:** qualified index migration, deterministic historical rebuild and index consistency tests only.  
**3C:** move passive merge write paths into one lifecycle-gated service and retain watch/playback cache publication; regression without scanner.  
**3D:** provenance-bearing evidence upgrade, group-wide proof audit/quarantine, centralized projection + all-route enforcement.  
**3E:** durable delete-generation fence, cleanup recovery, client reload/races.  
**3F:** negative and positive tests for A-B-C transitive provider conflict, weak fallback, alias/watch preservation, stale keys, delete/ABA, concurrent writer/grant revocation, no credential leaks, no new network traffic, and passive Search/Detail/Seasons/Episodes/Playback behavior.

**Phase 3A exit:** reviewable contract committed and linked in Notion, Phase 1 PR unaffected, no implementation/schema changes. Progression to Phase 3B requires **separate explicit user approval**.

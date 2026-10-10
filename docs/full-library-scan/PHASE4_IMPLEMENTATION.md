# Phase 4B–4E — Passive Collection Windowing: implementation & validation

Baseline: Phase 4A `17810ab0b7de596ed6689bdfe14a32eb3eeb671f`, local-only branch `dev/full-library-scan-phase3-local`.
Scope: passive HTTP only; no Scanner/periodic listing, no virtual-library generation, no GitHub push or production zouter deploy.

## 4B: root / Items
- The old `requestMergedCandidateSet()` / `mergedItemsScanLimit=5000` is removed from production code.
- `passive_collection_pager.go` accumulates source-qualified, bounded HTTP pages with one monotonic offset per source, concurrently across **at most four** sources and with finite request deadline (normally 90 seconds).
- Default collection order remains the established per-source row-round-robin; candidate folding uses authoritative Phase 3 `mergeHTTPItems/MergeDiscoveryService` and not raw Name equality. A fixed total-candidate cap is not an acceptable stopping condition.
- If a page is requested, acquisition continues until the client window is populated **after** canonical dedup / visibility filtering, or every source exhausts. Deep paging uses a larger but still bounded 512-entry source page. The final window is sliced **once** after merge.
- For explicit global `SortBy`, all sources must exhaust before sorting and returning a completed result: source sorts plus dynamic representative metadata make early stop unsafe without a proof. This preserves the correctness boundary but means sorted first-page queries may read an entire collection; investigate metadata-stable top-K index and upstream sort-order proof in the next optimization cycle. Do not claim unconditional first-page request-minimization for all sorts.
- `TotalRecordCount` exact only once sources exhaust; while any source remains unexhausted, numeric provisional total at least `StartIndex+Limit+1` allows Emby clients to keep paging. A provisional numeric count is a compatibility mechanism, **not a cardinality estimate or proof**.
- Malformed overflow/negative windows, repeated/nonprogressing source pages, contradictory per-source totals, upstream failures, permission/source-generation revocation and deadlines cause explicit errors, not a fake exhausted success.

## 4C: global search and Latest
- Global SearchTerm uses the same bounded candidate acquisition and strict source-authorization/Phase3 Evidence merge.
- Global Latest remains a JSON array, with source-local ParentId Latest preserved; default ordering is the existing upstream row fold unless a client explicitly requests a supported SortBy. Unspecified cross-source Latest freshness may not be a strict global DateCreated order.
- ID-batched `GET /Items?Ids` stays exact-batch and does not initiate a collection scan; `/Items` without Ids remains fallback proxy unless EIO-local watch filtering requires a local collection.

## 4D: known Series and local watch-filter collections
- `/Shows/{series}/Seasons` and `/Episodes` reuse the pager against **only authorized known Series instances**; SeasonId is translated per source. Items are projected/merged under Phase3 contract, sorted by Season/Episode number and paginated locally.
- ParentId-scoped Items / Latest and source-qualified Similar when EIO-local user-state filtering is required also use progressive source-only candidate pages. Without local filtering, parent lists continue to forward the client's source-scoped pagination directly.
- The former `localFilterScanLimit=5000` and stale misleading truncation warning are removed from applicable production routes.

## 4E: verification record

Test command environment: Go 1.23 backend executable built under Docker and run on layerone with `TMPDIR=/dev/shm` for SQLite test I/O. Source code/branch unchanged from local Phase4A except the Phase4 B–E changes listed here.

- New shallow/deep window tests: source page defaults 128; deep windows use 512; `StartIndex=5010,Limit=12` correctly traverses a mock library with **5,205 media items** without any fixed candidate cutoff; the last page returns exact `TotalRecordCount=5205`. The first 20-item window uses **one upstream request** rather than a full library scan.
- New high-overlap source fixtures: 160 canonical works shared by two servers yield 160 deduplicated results and fill deep windows despite raw overlap.
- Explicit `SortBy=Name` test verifies globally ordered canonical output across source A/B; conservatively materializes the complete matching set rather than emit an incorrect unsorted page. `SortBy=Random` preserves best-effort semantics but reads only bounded first pages, not entire libraries.
- Known Series test: 5,205 episodes accessed through `/Shows/{series}/Episodes` returns correct deep 5000+ page and exact total on exhaustion, while **never querying unrelated upstream B**. Mixed-case `ParentId=ROOT` is treated as a global root sentinel and removed from forwarded upstream query.
- SearchTerm deep paging, Latest JSON array semantics, normal ParentId source-only routing, Series SeasonId source-qualified translation, invalid/overflow windows, repeated/nonprogressing cursors, upstream inconsistent totals, Phase3 source generation / quarantine tests passed in staged suites.
- The test-only `filterStubUpstream` was corrected to echo the requested `StartIndex` (rather than always reporting 0 despite returning a later page). Two old tests expecting an immediately exact `TotalRecordCount=200` on a partially materialized list were updated to assert an Emby-compatible provisional total that permits continuation. This does **not** relax production validation of contradictory upstream metadata.
- Static `go vet ./internal/backend` passed on both the pre-final and final code.
- First full backend on code before final ROOT/Random tweak: **690 test functions passed, exit 0**; selected Phase4/Phase3D/Phase3E race tests: `PHASE4_RACE_PASS` (exit 0) on the pre-final code.
- **Final complete backend regression:** 692 passing top-level tests, zero failing tests, `PHASE4_FINAL_FULL_EXIT=0` / `PASS`. Built final Go backend test binary in Go 1.23 Docker; ran `/tmp/eio-phase4-final-backend.test -test.v -test.failfast -test.timeout 23m` on layerone. Full suite included historic Phase7K/7L lifecycle checks, Phase3A–3F identities, and all new Phase4 mocks.
- **Final static check:** `go vet ./internal/backend` → `PHASE4_FINAL_VET_PASS`, exit 0.
- **Final targeted Race Detector:** `go test -race ./internal/backend -run '^(TestPhase4BCaseInsensitiveRootAndRandomAvoidWholeLibrary|TestPhase4DKnownSeriesEpisodeTailBeyondFiveThousand|TestPhase3E|TestPhase3D)' -count=1 -timeout=14m` → `ok emby-in-one/internal/backend 200.392s`, `PHASE4_FINAL_RACE_PASS`, exit 0. Earlier pre-final broader Phase4/Phase3 race subset also passed (217.365s).
- **Overall 4A–4E status:** coding and automated acceptance complete. Full real-client and production deployment validation remain separate Phase9 work requiring explicit user authorization. No GitHub push, PR, release, production deployment, scanner implementation or Phase5 activity performed.

Real Emby clients including Xbox/Vivid have **not** been tested in this phase; they require user-approved isolated environment and are deferred to Phase 9, not implied by httptest. No production zouter deployment or remote GitHub push.

### Safety and resource considerations
- Source requests at most 4 concurrently per request; cap is per **source HTTP page**, not a media-total limit. Requests are user-triggered/passive only.
- Snapshot-exact global `TotalRecordCount` is impossible without full source exhaustion under canonical overlap; unexhausted totals are explicitly provisional numeric hints to prevent Emby pagination from stopping.
- Partial upstream failures produce an explicit HTTP error rather than falsely asserting a complete page. `watchLifecycleMu`, Source Generation Fence and authorization grant remain authoritative at commit.


### Known limitations not to hide
1. An arbitrary `SortBy` on a previously unindexed, very large global library requires exhaustive materialization to guarantee exact canonical metadata order; deadline or upstream failure returns an explicit error instead of wrong order. Sorted first-page cost needs further tuning and source snapshot proof.
2. Offset pagination cannot guarantee a fixed cross-request snapshot while independent upstream libraries change. Stable input ordering and phase3 generation fences reduce but cannot eliminate cross-request drift.
3. Source `Latest` order without `SortBy` remains source-order best effort, not a globally proven freshness sort; array shape and source isolation are preserved.
4. Provisional `TotalRecordCount` is numeric for client compatibility but exact only when all sources are exhausted. Clients that require an exact count without a full scan cannot be satisfied by this interface.

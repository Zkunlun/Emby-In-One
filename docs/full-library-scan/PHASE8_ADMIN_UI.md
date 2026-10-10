# Phase 8 — Scanner Admin UI

## Contract and scope

Phase 8 adds the **Active Scanner** view to the existing self-hosted Vue management panel. It consumes the frozen Phase 5 control and status endpoints and shows Phase 6/7 progress, without changing scanner persistence, merge identity, scheduler or Passive Aggregation.

* GET /admin/api/scanner/status reads global opt-in, per-source opt-in, latest run, per-library checkpoint and committed cursor, and circuit state. The UI joins names/online flags from the existing admin-only GET /admin/api/upstream. No token, password, provider URL or credential is intentionally rendered in the Scanner view.
* PUT /admin/api/scanner/settings writes an explicit boolean scanEnabled.
* PUT /admin/api/scanner/upstreams/{id} writes an explicit boolean allowScan; the source ID is encoded as a URL path segment.
* POST /admin/api/scanner/upstreams/{id}/commands/{start|pause|resume|stop|force_full} is admitted only from state-specific buttons. The backend remains the authoritative permission, online, circuit and lifecycle gate. UI preconditions are an additional safety layer, not a replacement.

## Safety and usability

* Both opt-in flags remain **OFF by default** in persisted backend SQLite. Merely enabling them neither dispatches a worker nor causes backfill. Commands require explicit confirmation. Force Full has a separate prominent high-request-volume warning.
* Dashboard explicitly separates Active Scanner (Movie/Series lightweight inventory) from always-independent Passive Aggregation (client-driven search/detail). Active Scanner never requests Season/Episode detail, art or playback information.
* Display: source online indicator, run state/type, item/page counts, current run ID/revision, last-success time, circuit state/retry deadline, normalized/safe error summary, per-library initial/inactive state, capability, checkpoints and committed cursor.
* Pauses caused by user activity, upstream 429 and permission revocation are distinguished. Resume is unavailable during the known 429 cooldown. Scanner status refreshes on entry, manually and every 15 seconds **only while the administrator is viewing the page**; no browser polling triggers active scans.
* Unknown status shapes, a failed status/upstream lookup and 401 logout **clear stale state** and disable commands. Out-of-order reads cannot overwrite a later refresh or restore stale status after logout. A mutation invalidates in-flight reads, executes once, then re-reads server truth even when the mutation fails; errors are kept separately so refresh cannot falsely imply success.
* Errors shown to administrators are fixed semantic messages or normalized scanner event codes, not arbitrary upstream error strings. Vue template escaping is used for names, identifiers and dates.
* No interactive controls or data appear in the unauthenticated view; backend admin authentication still covers each API route.
* Styling reuses the project's committed local Tailwind utility CSS, and the panel continues to honor self-hosted CSP.

## Verification and exclusions

* Frontend executable mock-contract suite: node phase8-admin-ui-contract.test.cjs (actual bundled Vue compiler/VNode render, status/API mocks, refusal gates, confirmation, credentials redaction, cancellation, stale-request races, original admin/user features).
* Final frontend mock-contract / real Vue 3.5.42 compiler & VNode render: **63/63 PASS**, command: node phase8-admin-ui-contract.test.cjs (original 44 tests plus 19 Scanner UI contract cases).
* Final admin-asset and CSP regression from a **freshly recompiled embedded Go test executable**, with the final UI markup: **PASS**; command: TMPDIR=/dev/shm /tmp/eio-phase8-validation/backend-phase8-final.test -test.run '^TestAdminPanel' -test.count=1 -test.timeout=5m.
* Phase 7 Scanner Delta simulated-upstream focused Go regression **PASS**; command: TMPDIR=/dev/shm /tmp/eio-phase8-validation/backend.test -test.run '^TestPhase7(Fallback|Reliable|Untrusted|DeltaStop|OverlapIs|NewLibrary|Restart|AllLibraries|Disappeared|StopInFlight)' -test.count=1 -test.timeout=5m.
* Complete backend regression **728 top-level tests, PASS (exit 0)**; command: TMPDIR=/dev/shm /tmp/eio-phase8-validation/backend.test -test.timeout=35m (output in local ephemeral backend-full.log). This validates the original compiled snapshot; the small later HTML labeling update is separately covered by the fresh-embedded targeted suite and 63 Vue contract checks.
* Go 1.23 offline Docker (network=none) go test -c, **go build ./... PASS** and **go vet ./... PASS**. Final node --check public/admin.js **PASS**; git diff --check **PASS**.
* No new backend state or schema implementation; the Phase 7 focused race detector and full regression were separately verified at Phase 7 completion. Phase 8 does **not** claim a new full-suite Go race run.
* A browser screenshot/manual interaction with a real admin session and upstream-driven Scanner are **deferred to Phase 9**, pending explicit production/sandbox approval; static/VNode tests are not a substitute for visual browser acceptance.
* No real Emby requests, production deployment, GitHub push/PR/release, or version bump in Phase 8.

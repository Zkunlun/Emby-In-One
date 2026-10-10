# Phase 7 — Daily Delta Scanner: source-fenced change discovery

**State: PHASE 7A–7F COMPLETE — local implementation and automated validation (2026-10-10).** Based on Phase6 commit `9e2c9ea5e1677149c00bf4f36ae52fe94d6eef8f` in the layerone isolated worktree. No GitHub push, PR, Release, remote deployment, production scan, or Phase8 UI changes. Real Emby/provider acceptance remains Phase9, under separate explicit authorization.

## Frozen product contract (Notion Phase2 spec)

- Automatic Delta: `Asia/Shanghai` 05:00 daily, exactly one durable slot per source/local calendar date. A host offline at that time *skips*, never catches up later. Existing in-progress run recovery is NOT a missed-slot replay.
- A fresh Delta run requires a completed initial Full and global `scan_enabled` + per-upstream `allow_scan`, authenticated online upstream, and no active run; terminal Stop does not resume the old run.
- Scanner **only** discovers Movie / Series identity; NEVER scans Episode / Season, streams, images, PlaybackInfo or full item detail. The existing parent-aware passive Series/Episode route handles episode growth.
- Each library gets a frozen run-level window: `target=run.StartedAt`; `cutoff=prior committedCursor - 5 minutes`. All committed cursors advance in one final transaction **only after all frozen target libraries complete**, not on partial/failed/stopped runs. Replayed pages and saved fallback tail state are idempotent. New libraries require `library_initial` lightweight inventory and cannot borrow another library's committed cursor.
- Full/Force Full safe watermark is the **start** of the full scan. A successful Delta library should retain its last successful Full watermark.
- Filters: use `MinDateLastSaved` only after positive and negative control responses prove the filter is obeyed; check server Date header clock safety (<=2min skew) and `DateLastSaved` evidence. Unknown/unreliable support => use fallback, never claim reliably filtered. No presumption that a server merely accepting query parameter means it applies it.
- Fallback: `SortBy=DateCreated&SortOrder=Descending`; on a page unambiguously older than cutoff with **zero new/changed lightweight items**, start **two additional tail-validation pages**. Any changed item resets tail-validation. Backfilled old dates and old-series episode changes can remain undetected: this path is best-effort, with explicit Reconcile/Force Full escape hatch.
- Repeat known unchanged items within overlap may be locally skipped by stable persisted fingerprint. A new or changed Movie/Series must re-enter Phase3 evidence indexing/strict merge comparator, not trigger detail or hydration.
- All HTTP requires per-source ActivityGate quiet>=60sec and dynamically applied MaxConcurrent lane/page ceilings; each next page follows 5–20s randomized spacing, lane stagger 5–10s. Real users are never queued behind Scanner.
- Authorization/source deletion / Stop fences all subsequent page writes. HTTP 401/403/429 and consecutive 5xx use the existing phase6 circuit/backoff state machine. Recovery never generates a different run.

## Implementation boundaries

- SQLite adds `scanner_delta_windows` with PK `(run_id,library_id)`, frozen `mode` (filtered/fallback/library_initial), cutoff, safe target watermark, and persisted fallback `tail_remaining`. Source deletion clears these rows in the same lifecycle transaction.
- Source `scanner_fingerprints` persists lightweight evidence digests by `(source_id,item_id)`; mapping/index identity must be committed first, then fingerprints, then page checkpoint.
- The Phase6 worker dispatcher now executes persisted Delta runs, but does not run arbitrary network discovery unless the administrator previously opted in and the user ActivityGate permits.
- Scan window and true page count are independently checked, and page rows are type-gated to Movie / Series only.
- A new library discovered in a Delta inventory is scanned with Phase6's full *lightweight Movie/Series* pagination; it does not trigger full scan of older libraries.

## Verification and caveats

### Actual completed validation (2026-10-10)

- **Final 11 new `TestPhase7*` Scanner tests PASS**, including reliable `MinDateLastSaved` positive/negative probe, ignored-filter fallback, two extra fallback pages, five-minute overlap, independent new-library initial inventory, restart/tail-state recovery, missing timestamps, disappeared libraries, Stop/no catch-up, and Stop during an in-flight Delta probe. Selection used `^TestPhase7(Fallback|Reliable|Untrusted|DeltaStop|OverlapIs|NewLibrary|Restart|AllLibraries|Disappeared|StopInFlight)` so it excludes the unrelated historical playback-session `TestPhase7K*` family. Marker: `PHASE7_FINAL_SCANNER_SUITE_PASS`.
- **Full `internal/backend` ordinary test executable PASS** (728 top-level `Test*` entry points in the compiled binary, full process exit 0); marker `PHASE7_FULL_BACKEND_PASS`. The previous baseline Phase6 reported 717; the current binary has 11 additional Scanner tests. These are top-level test entry points, not an assertion that all subtests are counted separately.
- Phase5/6 regression PASS: `go test ./internal/backend -run '^TestPhase(5|6)' -count=1 -timeout=15m`, `ok emby-in-one/internal/backend 197.063s`, marker `PHASE5_PHASE6_REGRESSION_PASS`.
- **Targeted Race Detector PASS** on the 11 final Phase7 Scanner tests, with no race diagnostic and zero exit; built with `go test -race -c` and executed using the same selection, marker `PHASE7_RACE_SCANNER_PASS`. This is NOT full-backend/full-repository `-race` coverage.
- `go build ./...` PASS (`PHASE7_BUILD_PASS`), `go vet ./...` PASS (`PHASE7_VET_ALL_PASS`), plus `go vet ./internal/backend` PASS. Formatting of edited/new Go files and `git diff --check` PASS.
- Verified output through `golang:1.23-bookworm` local network-disabled Docker containers, with `TMPDIR=/dev/shm` for temporary SQLite tests and a separately compiled, executable `/out/backend.test` / `/out/backend-race.test` (because `/dev/shm` is mounted noexec). No real upstream was used.
- Recovered interrupted working tree without resetting it. Corrected `scanner_runtime.go` where assignment to `modeForLibrary` had been accidentally included in a comment; added explicit persisted `scanner_libraries.capability=filtered/fallback` assertions. Added `TestPhase7StopInFlightProbeCannotCommitDelta` to assert Stop prevents late Delta window and committed cursor writes.
- Historical failed or interrupted attempts are retained as environment/test-selection evidence, NOT hidden: an overbroad `^TestPhase7` selection included old `Phase7K` playback tests and timed out; early disk-backed SQLite runs suffered high latency; direct `go test` from noexec `/dev/shm` failed at program start with permission denied. None of these attempts is claimed as the final successful run.

### Known boundaries and Phase8/Phase9 handoff

- The date filter must be positively and negatively proven per library, including server `Date` clock safety; missing/unsupported evidence falls back conservatively. Persisted capability is advisory and re-evaluated for future run creation, not proof of any real upstream's compatibility.
- DateCreated fallback is best effort: historical edits and backdated imports can be missed; authoritative complete changes/reconciliation require an explicitly authorized Force Full. Live page offsets amid concurrent source mutations do not constitute a transactional snapshot.
- First Full and Daily Delta are now implemented and mock-validated locally. No real account anti-abuse or provider behavior, real Xbox/Hills/Vivid player, long-lived high-volume upstream, production performance, or external deployment is certified by this phase.
- Phase8 is the admin UI for control/status. Phase9 requires explicit permission for controlled real Emby tests, deployment, acceptance, and release/version decisions. Do not push this development work or deploy it merely because Phase7 automated validation is green.

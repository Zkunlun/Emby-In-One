# Phase 5: Scanner Persistence and Control Plane

Project: EIO full-library aggregation. Baseline Phase 4 local commit a46b98d12d0688094178508c87192d02ba017b1e. Phase 5 is a **control-only** implementation; it does not perform real upstream media scanning, publishing, or production deployment.

## Safety contract

- Persistent global scan_enabled is false by default; each upstream allow_scan is false by default. Normal user grants, upstream login, configuration reload, EIO start and recovery do not enable active scan automatically.
- Scanner state shares the existing SQLite mappings.db connection so the existing upstream-delete lifecycle transaction also removes scanner runs, checkpoints, fingerprints, sources, circuits and schedule slots and advances source generation atomically.
- A config reload retains source generation and scan cursors. True source deletion invalidates all scanner rows; an old run cannot write to the same reused ID after deletion.
- Scanner has no network page worker and makes ZERO proactive /Views, /Items, /Items?Ids, Details, Playback, images or streams requests in Phase5. A 05:00 Asia/Shanghai timer on App.Run only consumes/queues durable Delta run control records, with no HTTP scanning.
- Crash recovery restores the same nonterminal run and last committed page cursor. A scanning run becomes queued/recovering when permissions still apply; disabled runs move to paused_permission. Paused-admin, stopped and failed runs are not restarted without explicit administrator action.
- Active scanner may scan only Movie and Series in Phase6, never Season, Episode, MediaSources, per-item Details or Items?Ids hydration. Shared media mapping remains owned by Phase3 MergeDiscoveryService.
- Frozen target libraries cannot be changed after first registration (identical replay is idempotent); checkpoints require membership in this frozen list and reject already completed library pages. Individual library completion stages a watermark in scanner_run_libraries.pending_watermark ONLY. All per-library committed cursors and the source-level last-success timestamp advance together atomically in scannerFinishRun after **every frozen library** completes; stopped/failed/partially complete runs cannot advance delta watermarks. Delta completion preserves the last full-safe watermark while advancing committedCursor. Only successful Full/Force Full marks initial_full_completed.
- For Phase6, merge/index identity writes must succeed first, checkpoint update LAST. An in-flight page may complete on Pause/Stop or permission revoke but no NEW page starts. True source deletion must reject any late page using source generation fence.

## SQLite schema, shared database

scanner_meta (global enabled), scanner_sources (allow, initial completion), scanner_runs (type/state/generation/revision and counters, one active per source), scanner_run_libraries (immutable per-run targets, per-library staged completion and pending watermark), scanner_libraries (per-library delta cursor, full watermark, capability, inactive, fingerprint), scanner_checkpoints (per-run/library page cursor), scanner_fingerprints, scanner_circuits, scanner_schedule_slots (unique upstream/date/auto_delta).

Schema migration is idempotent and fail-closed. All mutation methods serialize under watchLifecycleMu then scanner.mu, snapshot IDStore source generation BEFORE entering sqlite writeMu (preventing lock inversion). A source delete transaction clears scanner rows atomically with generation advancement.

## States and commands

Initial source state initial_scan_pending. An admin Start (only with global+per-source permissions and online valid session) creates queued run of type full; after initial full completion, next Start is delta. Force Full explicitly creates force_full. Pause preserves run and cursor and is idempotent; Resume queues the same run and checks permission/online/source generation; Stop is terminal and repeated Stop is idempotent; next Start creates a new run. Disabling permissions moves active runs to paused_permission and re-enabling does not auto Resume.

Internal execution events: queued -> scanning, scanning -> paused_user_activity, user quiet -> queued (Phase6 gate enforces 60sec), 401 -> paused_permission, 429 -> backoff at least 1h (including manual retry), 403 -> circuit_open requiring manual recovery, fatal -> failed, all frozen libraries done -> completed.

Scheduler slot 05:00 Asia/Shanghai is a deterministic, SQLite-atomic tick; one auto_delta slot/upstream/day, even under duplicate concurrent invocation. App.Run installs the live 05:00 timer using that tick; missed slots are not replayed. Phase5 does not install the page executor, so queued records do not generate any active upstream HTTP traffic.

## Admin-only HTTP APIs

GET /admin/api/scanner/status
GET /admin/api/scanner/upstreams/{id}
PUT /admin/api/scanner/settings with JSON scanEnabled bool
PUT /admin/api/scanner/upstreams/{id} with JSON allowScan bool
POST /admin/api/scanner/upstreams/{id}/commands/{action} where action is start, pause, resume, stop, force_full.

Status displays source permission, latest run, pages and items, per-library checkpoints and cursor, circuit state and scan availability; never includes credentials or tokens. Ordinary users cannot call these endpoints.

## Validation and phase boundary

**Phase 5A–5E implementation and automated acceptance: COMPLETE** (2026-10-10). The exact local commit and clean-worktree evidence will be recorded in the final Notion entry.

- Final Go1.23 backend test binary: `/tmp/eio-phase5-final-v4.test` built in a sandboxed Docker environment from final Phase5 source and tested with `TMPDIR=/dev/shm`.
- **Final full `internal/backend` regression: 706 top-level tests passed, 0 failed; `PHASE5_V4_FULL_EXIT=0`**, result `PASS`. Command: `/tmp/eio-phase5-final-v4.test -test.v -test.failfast -test.timeout 25m`. This includes historical Phase 7 playback/lease lifecycle, Phase 3 identity/source-generation fences, Phase 4 global paging, and all Phase 5 scanner tests.
- **Final targeted Race Detector: `PHASE5_FINAL_RACE_PASS`**, `go test -race ./internal/backend -run '^(TestPhase5|TestPhase3E)' -count=1 -timeout=15m`, result `ok emby-in-one/internal/backend 413.455s`, exit 0, no reported race. This is targeted scope, NOT a full repository race run.
- **Final static analysis: `PHASE5_FINAL_V4_VET_PASS`**, `go vet ./internal/backend`, exit 0.
- **Phase 5 focused test suite: 27 top-level test cases passed**, exit 0 (default deny, source/global permission, command idempotence, staged watermarks, delta preservation, immutable target sets, no-skipped checkpoint, restarts and SQLite reopen, Beijing 05:00 exactly-once, 403 circuit/429 backoff, auth isolation, source deletion and rollback recovery, zero proactive upstream scan requests).
- `git diff --check` passed. No production HTTP scanner worker exists, no active full scan occurred, and no zouter production or GitHub remote was touched.
- **Known boundary:** only control-plane tests and mock upstreams were exercised; real-client playback, user activity guard and active initial full scan belong to later explicitly authorized phases. A queued daily delta record is not executed until a future Phase 6/7 scanner worker is installed.

Phase 6 adds the real initial full scanner with source capability/inactivity/permission checks and bounded page fetching. Phase 7 integrates execution of queued daily delta tasks. Phase 8 adds management UI. Phase 9 covers controlled real-client verification and only user-approved deployment/release.

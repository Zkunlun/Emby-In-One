# Phase 6 — Initial Full Scanner Engine

**Status:** Phase 6A–6F code and automated acceptance COMPLETE (2026-10-10). Real remote Emby verification remains separately gated. Baseline local Phase5 commit: `b7083d504261815427d783f36ddae0e7692b1271`. Worktree `/.webcodex-managed-worktrees/emby-in-one-93696ba8`, branch `dev/full-library-scan-phase3-local`. No GitHub push, PR, release, remote installation or production zouter deployment.

## Phase 6A — Authorized library inventory

* Only a previously authorized/queued `full`, `force_full` or `library_initial` run may dispatch the real worker. Global `scan_enabled` and per-source `allow_scan` both default false and are checked against durable SQLite at request admission.
* On a fresh run use `/Users/{actualUpstreamUserId}/Views` once. Exclude music, photos, books, LiveTV, playlists etc. Unknown/mixed libraries may be scanned with strict `IncludeItemTypes=Movie,Series`, but no other media kinds.
* Frozen target library IDs are stored durably, immutable on replay; interrupted runs reuse that exact set rather than enumerating a changed inventory. No library absence is destructive: full successful reconciliation can set `scanner_libraries.inactive=true` for absent libraries without deleting historical MergeStore mappings.
* An empty/malformed library inventory is a visible failure, not false `initial_full_completed`.

## Phase 6B — Safe media-page acquisition

* **Only** `/Users/{upstreamUser}/Items?ParentId=<frozen-library>&Recursive=true&IncludeItemTypes=Movie,Series&Limit=60&StartIndex=...` (with conservative stable sort, lightweight fields) is used for page acquisition. No media item detail, `/Items?Ids`, `/PlaybackInfo`, `/Shows/...`, `MediaSources`, image or stream hydration.
* One library always has at most one HTTP page in flight; source page responses must contain an Items array of <=60 objects and must not contradict StartIndex/TotalRecordCount. Invalid responses fail explicitly, not mark an incomplete scan as successful.
* The first `/Views` inventory must be complete when TotalRecordCount is available. A truncated/contradictory library directory is rejected, never silently frozen and marked Full.
* If upstream omits trustworthy offset metadata and repeats an earlier listing page, a bounded last-8-page identity-signature guard fails the run instead of endlessly advancing a bogus cursor; an `Id`-less media row is rejected.
* Each page success produces lightweight work candidates (Id, Type, Name, ProductionYear, ProviderIds only) for existing authoritative Phase3 MergeDiscoveryService + WorkIdentityIndex. Scanner does **not** create a forged admin/user RequestContext. Source+run+generation scanner-write grant is revalidated **after** outbound HTTP. Ambiguous identity hints do not auto-merge; strict comparator and MergeStore quarantine remain authoritative.
* The page's shared media identity writes happen FIRST; durable Scanner checkpoint is committed LAST. A crash after identity writes but before checkpoint safely replays and revalidates the previous page. A late HTTP response after Stop or upstream deletion cannot publish stale mappings.

## Phase 6C — Source load, lanes and priority

* Maximum simultaneous source library lanes derives from existing `UpstreamConfig.MaxConcurrent`: positive N gives ceiling N, unlimited/<=0 defaults to 2. No new, conflicting `authorized_user_limit` setting. The live **in-flight page HTTP cap** is rechecked before every request, including when admin reduces MaxConcurrent during a run; already inflight requests finish.
* Each lane's subsequent page begins after **5–20 seconds randomized** delay; starting successive lanes is staggered **5–10 seconds**; no catch-up burst.
* An authenticated, real client media interaction updates the per-source ActivityGate. Media browse/search/detail/images/playback/session/progress count; health/media_counts/profile keepalives/system pings and Scanner traffic are excluded. Any new scanner page is blocked until **60 seconds** after the final interactive request. Existing in-flight page may finish.
* A source-scoped ParentId library request pauses only that source. A detail/playback request for an already mapped cross-source work conservatively pauses every known authorized instance. An unscoped global Items/Search query pauses all sources in the current user grant. Other sources stay independent.

## Phase 6D — Error handling and worker lifecycle

* 401 → `paused_permission`, existing upstream authentication recovery; only a recovered authenticated session after a debounce may automatically requeue, never a deliberately revoked permission.
* 403 → `circuit_open`, requires admin action. 429 → `backoff` at least 1h or longer `Retry-After`. 5xx/timeout/transport → fixed 5s retry, 5 consecutive failures → `circuit_open` for 1h; an actually successful page resets consecutive errors.
* State changes, administrative Stop/Pause and source lifecycle fencing are all durable. The worker only executes eligible Full runs; `delta` stays queued until Phase 7. During shutdown worker context is canceled and goroutines joined. Startup recovers the same previously active run and page cursor.

## Phase 6E — Full completion boundary

* At run admission, `run.StartedAt` provides the safe Full watermark. Each library completion stages a watermark; **only when all frozen libraries finish** does one SQLite transaction promote every library's committed Delta cursor, last success and initial Full-complete flag.
* Partial/failed/stopped runs never advance the Delta cursor or trigger wholesale deletion. No stale item membership is deleted on scan absence.

## Phase 6F — Controlled acceptance boundary

* This development iteration uses **local httptest simulated upstreams only**. Phase6 mock suite includes 541 distinct movies across 10 bounded pages; truncated /Views, duplicate-page replay, duplicate work identity, strict source isolation, STOP during HTTP, crash resume, 403/429/5xx and user activity boundaries. No fixed full-library work cap is present. It must not scan any actual user-configured remote Emby until the user explicitly identifies/authorizes a dedicated safe test source in a separately approved acceptance step.
* Real-client playback, real provider anti-abuse behavior, extremely large active scans and production zouter rollout are unverified and **must not be reported as passed**.
* **Final verified automated acceptance:** Go 1.23 final `/tmp/eio-phase6-v2-backend.test` full `internal/backend` suite **717 top-level PASS, 0 FAIL**, `PHASE6_V2_FULL_EXIT=0`. `go vet ./internal/backend` → `PHASE6_V2_VET_PASS`; latest Phase6 mock suite `^TestPhase6` → **32 PASS, 0 FAIL**, exit 0. Latest Phase6 scoped Race Detector `go test -race ./internal/backend -run '^TestPhase6' -count=1 -timeout=15m` → `ok emby-in-one/internal/backend 238.279s`, `PHASE6_V2_FINAL_RACE_PASS`, exit 0. An earlier broader `-race` across `TestPhase6|TestPhase5|TestPhase3E` also passed (`ok emby-in-one/internal/backend 909.515s`), as did the preceding full backend suite (714 tests); these earlier runs are NOT represented as latest-code test coverage.
* All filesystem changes pass `git diff --check`; local-only final commit and Notion final record are tracked separately from these test results. Phase7 Delta/Phase8 UI/Phase9 real-client acceptance and release are out of scope.

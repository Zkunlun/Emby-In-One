# Phase 9 — System Validation, Controlled Rollout, Release Gate

**Entry baseline:** 2026-10-10, branch dev/full-library-scan-phase3-local, Phase 8 commit 7ed2ac00b1cc1e38ae52bab033efce9e50d4a909. Phase 0–8 features are implemented locally. Phase 9 is **not** a blanket authorization to deploy, run a real upstream scan, merge, push, tag or release.

## Important target correction

The Phase 2 planning entry describing "deploy to zouter first" is now stale. As of 2026-10-10, the production EIO instance is **AWS-US (3.138.121.9)**; zouter-HK is retired for EIO and retains only an old-address transitional Caddy forwarder plus its other independent services. AWS holds newer mapping/state data than zouter and **must not be overwritten with the older zouter database**.

Accordingly, Phase 9B starts with a **separate network-disabled disposable smoke instance** and a non-mutating production preflight/rollback plan. Production deployment is expressly gated after a user-approved backup, maintenance window and plan. No deployment to zouter is justified merely because its name appears in the historical Phase 2 spec. Do not touch AWS 8388 SS2022, Komari, WebCodex Runner or zouter Komari/Caddy forwarding.

## 9A — Automated system validation matrix

- Build/vet/frontend contract: Go 1.23 build ./cmd/emby-in-one, vet ./..., node --check public/admin.js, and node phase8-admin-ui-contract.test.cjs (63 expected).
- Full backend: Phase 8 had 728 top-level tests, complete PASS at identical Phase 8 HEAD. Any later Go changes require **new** full regression evidence.
- Scoped Race Detector: scanner phases 5,6,7 source fence and generation, permission state transitions, page/checkpoint idempotence and 403/429/401/circuit/backoff. Do not block indefinitely on full-repo race testing.
- Integration coverage by simulated upstreams: 500+ item unbounded full inventory; per-source authorization isolation; cross-source strict merge; delta reliable filter/fallback; no Season/Episode active crawl; passive Episode entry; 5-min cursor overlap; frozen run target; library disappearance/reappearance; Stop while page/probe in flight; resume/restart; API faults and timer idempotence; no hard 5000-item ceiling.
- Frontend: active/passive distinction; actions and defaults; admin auth, error redaction, stale data rejection and busy controls.
- Storage: migration idempotence, scan opt-in remains false for fresh installs, enabling/disabling one source doesn't silently start/auto-resume other sources, and no old backup overwrites current DB.

All simulation findings must identify which aspects were tested in memory/httptest, which were exercised via actual HTTP against an isolated binary, and which remain real-production-client acceptance. PASS for a mocked source is not a PASS for real Emby.

## 9B — Isolated runtime smoke and production preflight

**Non-destructive smoke workflow:** build developer binary from this commit, run it under Docker --network none, without published ports, with a temporary standalone config and **zero configured upstreams**. Test actual HTTP GET /System/Info/Public, unauthenticated Scanner API (401), self-hosted HTML/JS, ephemeral admin login, GET /admin/api/scanner/status showing scanEnabled=false / upstreams=[] / executorAvailable=true, and invalid start returning 404. Stop container automatically. Do not configure user upstream tokens or import a live database.

**Production preflight before requesting upgrade approval:** read-only identify current AWS binary checksum and version, service configuration, DB path/schema, current grants/cursors/run state, mapping/watch/token data and disk/memory headroom. Record point-in-time backup and rollback checksums in private operator notes **without writing credentials into GitHub, Notion or logs**. Backup **the current AWS authoritative DB**, config, token/user state and current binary; stop or quiesce application for transaction-consistent capture or use SQLite backup API. Do not replace it with a stale zouter backup. Reserve rollback capacity, validate restore using separate disposable data paths and confirm all non-EIO services remain untouched. Phase 9B production deploy requires explicit user approval of exact target, sequence, risk and rollback.

## 9C — User acceptance matrix (real-source gates)

| Scenario | Simulated automated coverage | Real-source/client acceptance |
|---|---|---|
| Open a single upstream library (no cross-server virtual mixing) | Existing Phase4/source-scoped tests | Pending |
| Global search/passive merge and original-source authorization | Existing Phase3/4 tests | Pending |
| Series opened, Passive Episode discovery, playback/metadata/watch state | Existing Phase3/4 playback tests | Pending |
| Active Initial Full, no Season/Episode/images/stream calls, bounded requests | Existing Phase6 mock suite | Pending approval for named safe upstream |
| Pause/Resume/Stop/restart and committed checkpoints | Existing Phase5/6 mock suite | Pending |
| Delta next local 05:00, reliable filter or fallback, Force Full | Existing Phase7 mock suite | Pending |
| 401/403/429/5xx/timeouts/circuit handling | Existing Phase5/6/7 fault injection | Pending controlled fault tests |
| Admin browser UI on desktop/mobile and third-party clients (including Vivid/Hills/Capy if applicable) | Phase8 Vue template/contract tests + isolated HTTP assets | Pending |
| Disk/memory and log redaction after multi-source usage | Unit/resource smoke | Pending AWS operational review |

Do not use real Emby accounts as fault-injection or load-test victims without explicit source-level permission; 403 and 429 are operational/account risks, not a reason to retry faster. Real app acceptance requires the user's manual observation of actual playback and client navigation, not a claim inferred from httptest.

## 9D — Release and migration decision gate

Before a release: confirm acceptance signoff, choose version number, compare refreshed remote main, assemble bilingual README/update notes, review migration/backup/rollback, repeat build/vet/tests from the final source commit, and inspect packaging scripts and bundled admin panel. **Do not push, merge, tag, upload Release or switch production until the user authorizes those actions.** Clean only task-created binaries, mock DBs, temporary test caches and other self-owned artifacts. Leave unrelated VPS services and historical rollback files in place.

## 9D packaging observations (read-only)

- GitHub release workflow is defined in .github/workflows/release.yml; pushing an approved release/V* branch may trigger a **real public GitHub Release**, so never use that trigger as an exploratory test.
- Release assets explicitly include admin.html and admin.js beside the per-architecture Go binary, and the Go binary embeds public/admin.html, admin.js and vendor CSS/JS. The production handler can favor **on-disk public/admin.html and admin.js** over the embedded copies. Therefore a **binary-only** production upgrade may leave an old management UI. The deployment manifest must atomically couple a matching binary and verified admin panel assets, or explicitly validate the embedded fallback when no overriding disk assets exist.
- release-install.sh downloads the version-matched admin assets and their checksums; this mechanism must be checked on the final chosen version and target architecture. Do not invent release tag names/asset URLs. Existing install.sh also contains a historical Docker VERSION: v1.4.9 in its generated composition path, which requires review during new-version documentation/update preparation; do not silently bump it before version approval.
- The CI workflow checks committed Tailwind CSS freshness. Final release preparation should run the project's exact panel build command in an isolated node environment and compare the resulting CSS without downloading code or altering the production panel unexpectedly.

## 9A / isolated-smoke evidence

- Phase8 identical source SHA baseline completed the 728-top-level backend full test with exit 0; Phase9 verifies fresh build, offline runtime, selected concurrency tests and frontend behavior.
- Real process HTTP smoke with standalone config and Docker --network none: startup/health PASS; unauthenticated scanner GET 401; admin.html and admin.js served; ephemeral login successful; scanner disabled with zero upstreams; missing source command 404. **No real Emby reached, no host port published, no production data copied.**
- **9A evidence, final as of 2026-10-10:** standalone Go binary built from Phase8 HEAD **PHASE9_BINARY_BUILD_PASS**, cross-Phase5/6/7 race-enabled backend test binary compiled **PHASE9_RACE_TEST_COMPILE_PASS**, go vet ./... **PHASE9_VET_PASS**; offline Go toolchain used Docker --network none.
- **Scoped Scanner Race Detector (Phase5/6/7): PASS**, exit 0, marker PHASE9_SCANNER_MULTIPHASE_RACE_PASS; isolated compiled race test executable covering durable state machine, source grants, Full/Delta, 403/429 faults and interrupted pages. This does not mean a full-project race suite ran.
- **Scoped Passive Merge/Source Fence/Collection Paging Race Detector (Phase3/4): PASS**, exit 0, marker PHASE9_PASSIVE_MERGE_PAGING_RACE_PASS; selected source-generation and quarantine tests plus single-library/global-search/episode and >5000 paging tests.
- **Actual process isolated HTTP smoke: PASS**, markers PHASE9_NOAUTH_SCANNER_DENIED_401, PHASE9_REAL_HTTP_ADMIN_HTML_PASS, PHASE9_REAL_HTTP_ADMIN_JS_PASS, PHASE9_OFFLINE_ZERO_SOURCE_SCANNER_OFF_PASS, PHASE9_MISSING_SOURCE_START_REJECTED_404 and PHASE9_RUNTIME_NETWORK_NONE_PASS. Container limited to 512 MiB, no published ports or upstreams.
- **Frontend: 63 PASS** including Vue compiler/VNode contract and credential redaction; **node --check public/admin.js PASS**. Installer/CLI shell syntax: bash -n install.sh release-install.sh emby-in-one-cli.sh **PASS**.
- The isolated developer binary reports --version **dev**, as expected because **no new release/version has been approved**. Its local SHA256 was 81b4bd4777a7cfbe1d3604dc6b7baa0e58275255db1f7030d6bd2fbd745c9dcf; this is not a production/release checksum.
- Identical Phase8 Go source HEAD completed all 728 top-level internal/backend tests in the prior phase. Phase9 did **not** independently repeat the full 728-test ordinary run; scoped race tests, fresh build/vet and actual-process isolated HTTP acceptance are its additional evidence.
- **Phase9A automated acceptance complete (within mock/offline scope). Phase9B isolated runtime smoke complete; the production deployment step is blocked pending explicit approval. Phase9C real client/real provider and Phase9D merge/release are NOT COMPLETE.**

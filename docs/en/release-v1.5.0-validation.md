# V1.5.0 release validation and safety boundaries

[Home](../../README_EN.md) · [Changelog](../../Update.md) · [简体中文](../release-v1.5.0-validation.md)

V1.5.0 adds shared work identity indexing, passive paging beyond the former per-source 5,000-candidate cutoff, and an opt-in lightweight Movie/Series full/delta Scanner, **disabled by default**.

## Verified

- The same business source passed **728 top-level backend Go test cases** during Phase 8; Phase 9 reran build, Go Vet and selected Scanner/passive-merge Race Detector checks. A full-repository race run is **not** claimed.
- The Vue Admin panel passed **63/63** contract/template tests plus JS syntax checks.
- Mock upstream tests covered source grants and generation fences, identity-conflict quarantine, deep passive paging, mapped-series passive episode discovery, full/delta checkpointing, resume/stop, activity and request pacing, and error/circuit handling for 401/403/429. These tests **did not actively scan any real Emby upstream**.
- Real-process network-isolated HTTP smoke tests covered the Admin assets, public info, unauthorized Scanner API returning 401 and default scan-disabled state.
- On AWS-US, the development build with identical Go business source (dev-7373aae) was deployed after a separate copy of its live SQLite DB passed migration tests and an offline consistent backup was taken. All **173,004 mappings and 4 users** remained, quick_check=ok, the Scanner tables were added with no grants or runs and the global enable flag was 0. Public and Admin HTTP checks passed.
- AWS has about 1GiB RAM. EIO used roughly 380MiB by systemd accounting, within the existing 500MiB limit, without any real scanner load test. AWS-local Go Vet was cancelled to protect memory; the same Go code passed Vet in the prior isolated environment.
- The GitHub Release workflow will separately run Go Vet, full tests, six-architecture builds and asset checksums. Do not assume that CI has passed until it reports success.

## Pending real-client acceptance

Actual Emby and third-party client browsing, search, passive Episode discovery, playback/watch status and sustained load require explicit user acceptance. Real Initial Full, Daily Delta and Force Full have **not** been run. Administrators must enable both global scanEnabled and per-upstream allowScan, and select a safe test source before starting active scanning. Provider rate limits/account restrictions remain a risk. DateCreated fallback misses some old/backdated changes; missed 05:00 Asia/Shanghai scheduled windows are not replayed.

This release does **not** add a combined A+B virtual library, active Season/Episode/artwork/stream crawling, periodic full reconciliation, or universally exact aggregate TotalRecordCount. Unindexed global sorting can be expensive and cross-request snapshots are not immutable.

## Safe upgrades and rollback

Back up the **current authoritative production database** transactionally (including WAL considerations), config, user/token/password-key data, old binary and disk-based Admin assets. Never overwrite current mappings with a stale database from another host. Update binary and matching Admin HTML/JS together; disk assets may override embedded assets. Keep the Scanner disabled until ordinary browsing/playback works, and review schema compatibility before any rollback.

The official version is identified by GitHub **V1.5.0 tag** assets. AWS's earlier dev-7373aae version string refers to a pre-release validation build, not the published release binary.

# V1.4.13 Release Validation and Scope

## Changes and compatibility

- Based on V1.4.12: Emby-compatible Shows/Seasons and Shows/Episodes aggregation now queries authorized upstream instances concurrently; a slow provider no longer blocks already available seasons.
- A genuine empty collection remains HTTP 200 with empty Items. Total failures, malformed success bodies, and an empty result with unresolved/failed providers are distinguished from a truly empty list. An empty first response does not start the early-return grace period; global timeouts also bound aggregation waiting.
- Season-level UnplayedItemCount uses complete locally visible episode evidence and a 2.5-second maximum for auxiliary requests. When evidence is incomplete, the field is omitted instead of blocking the main season list.
- Regular-user PlayCount and watch state remain local to that EIO user. Requested Fields=Path is hydrated from its actual upstream item, without fabricating MediaSources. Confirmed merged works retain a genuine ProductionYear when the preferred description lacks one.
- No show-title, provider-name or library-specific compatibility exceptions; no changed playback authorization rules, database schema, or per-source aggregation candidate limits.

## Evidence and limits

- Focused Go contracts on Windows passed for user state, Path/MediaSources, merged metadata, Seasons/Episodes, slow/failed/empty/malformed upstreams and aggregation deadlines. Newly introduced regressions were observed failing before the fixes and passing after them.
- A Linux/amd64 CGO candidate phase5-rc-37bdfd2 was built and served by an isolated DMIT test instance. HTTPS, public Emby information, unauthorized-response status and independent server identity were verified. The user explicitly accepted the Hills Android real-device retest, including previously affected series.
- A prior Windows whole-project test exceeded its execution limit; this is not a full-suite pass. Official CI full tests, vet, six-architecture assets and checksums must be verified from the V1.4.13 GitHub Actions runs.

## Boundaries and upgrade

- Unreviewed PR #3 for same-upstream multiple series/season pairing remains separate and is not part of this release.
- The two previously documented Hills Lite Latest and certain upstream library Items errors have not been conclusively shown resolved; intermittent genuine upstream 5xx is not a guaranteed fix.
- Publishing the GitHub release does not itself upgrade the production DMIT service. Before an eventual upgrade preserve config/, data/, tokens, user-password.key, and consistent SQLite WAL state, and verify rollback options.

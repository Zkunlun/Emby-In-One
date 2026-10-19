# V1.4.12 Release Validation Scope

## Changes

- Based on V1.4.11, the update includes Phase 2–8 improvements to admin operations, media list routing, local Resume/NextUp error handling, extended APIs, client-safe error responses, and embedded panel styling.
- The 5,000-candidate-per-source bound, existing SQLite mapping schema, and upstream-confirmation requirement for playback progress remain unchanged.
- The user explicitly accepted two unresolved Hills Lite list failures and authorized release with these known issues.

## Evidence and limits

- Focused cross-phase Windows Go contract tests passed; these are **not** a full race suite.
- Admin panel Vue template contracts passed (45/45), Node interaction tests passed (20/20), and the generated Tailwind CSS matches the committed file.
- An isolated Linux/amd64 Go 1.23.12 CGO build produced a fully static ELF with no interpreter or shared-library dependencies. Native Linux focused contracts and version/hash checks passed.
- The candidate was deployed to DMIT with consistent binary/config/data backups; systemd, Caddy, local HTTP admin/API checks and SQLite quick integrity checks passed, with matching important mapping-table counts.
- The official GitHub Actions full test/build pipeline and six-architecture assets must be assessed from their actual runs, not inferred from these focused tests.

## Known issues (not fixed)

1. Hills Lite may show `Upstream latest items request failed` for certain home-page Latest requests while other content loads.
2. Hills Lite may show `Upstream items request failed` when opening certain upstream libraries, sometimes displaying zero items or a loading failure.

The responsibility of EIO versus the upstream has not been conclusively established for these failures; a client displaying zero results does **not** demonstrate that the source library is empty. The user reported that other functions passed real-client acceptance and chose to ship without fixing these two issues. Third-party danmaku API support and all upstream 500/502/524 failures are not claimed to be fixed.

## Upgrade and rollback

Preserve the complete `config/` and `data/` directories, tokens, `user-password.key`, and SQLite database/WAL state. Take a write-consistent backup before replacement. Preserve post-upgrade watch progress whenever possible; prefer a binary-only rollback over restoring old data. Publishing the GitHub release does not itself replace the running server binary.

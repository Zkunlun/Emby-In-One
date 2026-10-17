# V1.4.11 Release Validation Scope

## Changes

- Automatic completion for movies and episodes occurs on an accepted `Stopped` event at or above 90% of a qualified runtime, not on `Progress` alone. Seeking back below the threshold before stopping does not mark an item watched.
- Rewatching preserves the previous `Played=true` state while recording a fresh resume position. A completed stop clears only the current resume position.
- Expired playback leases may finalize a watch only from confirmed, qualified progress evidence, without overwriting newer sessions.
- Local UserData is overlaid after final filtering, merging and pagination; totals and page membership stay consistent.
- The existing 5,000-candidate per-source bound, SQLite mapping architecture and multi-version identity behavior remain unchanged.

## Verification and limitations

- `go vet ./...` and a Linux binary build passed.
- First full Go regression: 724 top-level tests passed and one obsolete rewatch assertion failed. The assertion was updated to match the new behavior; its focused retest passed.
- Seven selected playback, expiry and UserData tests passed with `-race`, with no reported data races. This is not a full-project race pass.
- The second full regression was interrupted by a layerone Runner disconnect; its result **is not confirmed as passing**. The user explicitly accepted this exception.
- On October 17, 2026, the corresponding candidate binary was deployed to DMIT; SQLite quick-check passed, the local API and Caddy HTTPS returned HTTP 200, and the user accepted real-client testing.

## Upgrade and known boundaries

- Preserve configuration, SQLite data, tokens, and the separate `data/user-password.key`. Make a consistent SQLite backup before changing binaries.
- Sporadic upstream login HTTP 502 responses were observed independently and are not claimed fixed here.
- GitHub Actions multi-platform release assets and checksums must be verified from the actual release workflow results.

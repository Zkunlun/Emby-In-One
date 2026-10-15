# V1.4.10 validation scope

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../release-v1.4.10-validation.md)

This page documents **V1.4.10 verification evidence and its limits**. Formal availability, dates and release assets are determined by GitHub Releases.

## Provenance

Ten Phase8 commits were integrated on GitHub main 963bbf4e7d4c8a9c62d79bf55ded6c5c2a9e0050; Phase9B integration HEAD is 9c08819cd0066fdadb5cab6a4b55568432536413. All backend source files match the client-accepted Phase8 candidate byte for byte, and bilingual documentation is preserved.

## Passed checks

- Phase8 passed basic DMIT health checks and real-client user acceptance on 2026-10-15; this does not prove per-field upstream wire captures.
- On an identical Git tree under Windows/amd64, 36 targeted Phase8/SNTP tests passed; cmd tests, go vet and go build succeeded.
- All 43 backend changes were retained, without restoring withdrawn temporary workflows.

## Skipped or unverified

- The user explicitly waived the complete 9B3 backend regression. It did not pass: a Windows run timed out near 300 seconds with 488 passes, 2 failures and 5 skips before all cases ran.
- TestUnifiedCountsPendingCleanupAndRecoveryPublishOnce and TestUnifiedTokenPersistenceFailureStaysPendingAndRecoversOnce expected HTTP500 for simulated Token persistence failures but got HTTP200 on Windows; Linux behavior is unverified.
- GitHub Actions PR CI #33 passed Linux Go build, vet, the complete ordinary tests, the panel and shell jobs. Full race was skipped by configuration. Six-architecture binaries, static linking and SHA256 still require the formal Release workflow.
- Redirect with every route Dead conservatively returns 502; anonymous 404 probes do not prove playability. Some historic upstream login errors remain unexplained.
- The withdrawn V1.5.0 full-library scanner is not restored; SNTP on-demand hydration is different.

## Upgrade safety

Preserve config, tokens, user-password.key and a consistent SQLite/WAL backup with rollback. GitHub Releases determine formal versions and downloads.

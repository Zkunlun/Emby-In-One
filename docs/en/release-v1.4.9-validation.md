# V1.4.9 validation scope

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../release-v1.4.9-validation.md)

Business-source deployment and user acceptance date: 2026-10-06. Release preparation reused the accepted 262 Go inputs without changing runtime logic. Documentation and packaging additions were checked separately before release.

## Existing evidence

- Ordinary merge regression: grouped runs on the same source accounted for 656 top-level entries and 2026 unique test nodes, with 0 failures and 0 skips. Packages without test files were recorded separately.
- Limited race checks: 4 entries and 7 nodes covering concurrent coalescing, added versions, alias lifecycle and state restart. There is no full-race pass claim.
- Hills joint-candidate checks: 83 entries and 432 nodes passed, covering Counts, task six, language parameters and authorization/error validation.
- Actual Hills requests and user acceptance completed. In a real episode list, matching S1E1 entries became one item and all versions from responding sources were grouped. One upstream-timeout sample does not prove that every source's versions returned.
- The user confirmed merge-revision client acceptance. This does not extend existing pagination or uncovered recovery boundaries into exhaustive verification.

## Release environment

- **Go1.23.12:** the same 262 Go inputs; the full inventory accounted for 658 top-level tests and 2066 unique nodes, with 0 failures and 0 skips in the final coverage set. An earlier full command hit the 15-minute package budget. Its 468 fully passed entries were retained, and a separate run passed the remaining 190. Interrupted entries were not marked passed.
- The initial read-only-workspace attempt stopped because tests needed temporary storage writes. Subsequent checks used isolated writable source copies. Initial environment failure and timeout logs remain recorded; this is not presented as a single uninterrupted full-suite pass.
- Go1.23 vet, the versioned static amd64 build and --version checks passed. Business Go inputs matched the user-accepted deployed source file by file.
- 44 panel contracts passed. Stylesheet rebuild had no differences. Syntax checks for three install/management scripts, Git diff formatting and Actions configuration passed.
- CI and Release continue ordinary full regression with a 45-minute package budget. Full race remains a manual optional check and was not run for this release.
- The dynamic Go1.27.1 development candidate is retained separately. Its binary hash does not identify the six-architecture static Release assets.

## Retained limits

Some list paths cap each source at 5000 raw candidates; ParentId uses upstream raw-item pagination. No indirect three-source conflict audit or timeline conversion is provided. Counts sum the three official per-source fields, with no second deduplicated/version metric group. Dedicated CapyPlayer counts-503 checks remain deferred. There is no full-race, exhaustive recovery-breakpoint or all-client-behavior coverage claim.

# Version numbering

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../version-numbering.md)

**Important:** The historical V1.5.0 dated 2026-09-29 below was renumbered V1.4.7 on 2026-10-06. The **new V1.5.0 (2026-10-11)** is a different release for full-library aggregation. Use dates, functionality and commit IDs to distinguish them.

On 2026-10-06, the maintainer moved three published versions into the continuing 1.4.x sequence:

| Historical label | Current label | Original publication date | Functionality |
| --- | --- | --- | --- |
| V1.5.0 | V1.4.7 | 2026-09-29 | Empty-password authentication and encrypted credential storage |
| V1.5.1 | V1.4.8 | 2026-10-02 | Authorization capacity, one active device and session consistency |
| V1.6.0 | V1.4.9 | 2026-10-06 | Upstream identity, watch state, counts and complete version merging |

This is renumbering, not a functionality downgrade. Each retains its original business source, with Go files byte-identical. Original commits and development/validation history remain. New tags point to commits changing only documentation and build-version information.

GitHub Releases were renamed in place, retaining Release IDs and first publication times. Six-architecture binaries were rebuilt with the new version strings; Docker source packages, CLI version information and SHA256 checksums were refreshed together. V1.4.6 and earlier numbering remains unchanged. Historical V1.5.0/V1.5.1/V1.6.0 tags and old-number assets were removed after verification; replace old download addresses.

Treat an installation's update from an old label to its corresponding new label as a rename of the same functional version. Later online updates follow GitHub Latest. Retain configuration, database, tokens and data/user-password.key. No production instance was operated on during this renumbering.

Original business commits:

- V1.4.7: 9c22aead2eb701f4fb6d7e5d0df907c4a3eefa59.
- V1.4.8: 6e6d3eb42988ce697141ec95578b880219ea9851.
- V1.4.9: 0627ad96c4b04e8cce1ad51fb37f9daf2b8a9251.

Old labels here explain the mapping only. Actual validation scope and deferred work remain governed by each version's records.

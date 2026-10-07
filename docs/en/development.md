# Development and contributing

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../development.md)

Applies to the V1.4.9 mainline.

## Environment and builds

The current mainline uses Go 1.23+, CGO and repository third_party/sqlite. A C toolchain and the public/ embedded package are required. See [source installation](installation.md#run-go-source) for a complete clone and initial configuration. The root Node package only builds panel styles; the backend is not a Node.js program.

```bash
go build ./cmd/emby-in-one
go test ./...
go vet ./...
```

Example versioned build:

```bash
CGO_ENABLED=1 go build -ldflags="-X main.Version=V1.4.9" -o emby-in-one ./cmd/emby-in-one
./emby-in-one --version
```

These are developer verification commands, not a record of tests executed for this documentation work. Run relevant tests first and follow existing CI requirements for shared backend behavior. Full race is not claimed as passed here; historical evidence is in [validation scope](release-v1.4.9-validation.md).

After admin.html/admin.js or style edits, use Node/npm `npm run build:panel` to rebuild public/vendor/tailwind.css and check consistency of embedded and disk-override resources. Self-hosted dependencies live in public/vendor; do not add unexplained third-party loading.

## Backend modules

| File | Responsibility |
| --- | --- |
| `cmd/emby-in-one/main.go` | Program entry |
| `internal/backend/config.go` | YAML loading, saving, validation and atomic writes |
| `internal/backend/server.go` | HTTP startup and graceful shutdown |
| `internal/backend/routes.go` | URL-to-handler registration |
| `internal/backend/middleware.go` | CORS, logging, status capture and CSP |
| `internal/backend/ssrf.go` | SSRF policy and safe dialer for proxy tests/upstream connections |
| `internal/backend/auth.go` | Password hashing, verification and authentication helpers |
| `internal/backend/auth_context.go` | Request authentication context |
| `internal/backend/auth_manager.go` | Local EIO token issuance, persistence, verification and revocation |
| `internal/backend/identity.go` | Identity capture and five-level passthrough resolution |
| `internal/backend/identity_persistence.go` | Per-server identity persistence |
| `internal/backend/identity_lifecycle.go` | Cache ownership, migration and asynchronous publication fences |
| `internal/backend/user_store.go` | User CRUD, hash/secret, authorization and SQLite indexes |
| `internal/backend/handlers_admin.go` | Administration handlers including upstream CRUD |
| `internal/backend/handlers_system.go` | System information, /System/Info |
| `internal/backend/handlers_user.go` | Login rate limits and user handlers |
| `internal/backend/admin_validation.go` | Admin input validation/helpers |
| `internal/backend/idstore.go` | SQLite bidirectional virtual/original ID mappings |
| `internal/backend/id_rewriter.go` | Recursive ID virtualization/reversal |
| `internal/backend/query_ids.go` | Batch query ID resolution |
| `internal/backend/media.go` | Aggregation, deduplication and metadata selection |
| `internal/backend/aggregation.go` | Grace aggregation; late results register IDs/instances without appending responses |
| `internal/backend/media_access.go` | Authorized instances and watch-query snapshots |
| `internal/backend/media_request_access.go` | Request source, version and ownership validation |
| `internal/backend/media_items.go` | Item queries and multi-source fanout |
| `internal/backend/media_resume.go` | Resume forwarding/merging |
| `internal/backend/media_nextup.go` | NextUp forwarding/merging |
| `internal/backend/media_playback.go` | PlaybackInfo and user/upstream device-lease reservations |
| `internal/backend/media_stream.go` | Video/audio proxying with virtual-ID routing |
| `internal/backend/library_image.go` | Image proxying and cache headers |
| `internal/backend/series_userdata.go` | Series watch-state isolation for Resume/NextUp |
| `internal/backend/session_userdata.go` | Sessions/Playing reports |
| `internal/backend/watch_store.go` | Per-user progress storage/persistence |
| `internal/backend/watch_visible_store.go` | Authorization-first batch watch queries |
| `internal/backend/watch_lifecycle.go` | Management transactions, cleanup journal and startup recovery |
| `internal/backend/watch_lifecycle_runtime.go` | Exact cache/lease cleanup and post-deletion inheritance |
| `internal/backend/watch_lifecycle_requests.go` | Authentication and asynchronous state-publication guards |
| `internal/backend/watch_playback_store.go` | Atomic playback-event watch-state updates |
| `internal/backend/playback_watch_state.go` | Field parsing, source matching and completion decisions |
| `internal/backend/playback_watch_cache.go` | Bounded session duration/position caches and terminal state |
| `internal/backend/playback_watch_owner.go` | Shared write ownership/generations per user and merged work |
| `internal/backend/playback_watch_events.go` | Reports, metadata completion and manual resets |
| `internal/backend/playback_watch_legacy.go` | Legacy PlayingItems compatibility |
| `internal/backend/playback_limiter.go` | User/upstream device leases, revision, heartbeat and exact Stop |
| `internal/backend/playback_routes.go` | User-owned playback routes and source sessions |
| `internal/backend/login_limiter.go` | Per-IP failed-login limits; evict oldest records at capacity |
| `internal/backend/streamproxy.go` | HTTP streaming, backpressure and relative HLS rewrites |
| `internal/backend/fallback_proxy.go` | Fallback route scanning virtual IDs in URL/query |
| `internal/backend/healthcheck.go` | Offline reauthentication and online route probes |
| `internal/backend/logger.go` | Console/file levels and rotation |
| `internal/backend/scrypt_local.go` | scrypt derivation for password hashing |
| `internal/backend/sqlite_cgo.go` | Embedded SQLite compilation and CGO bindings |
| `internal/backend/upstream.go` | Connection pools and concurrent request orchestration |
| `public/embed.go` | go:embed for admin.html, admin.js and vendor/ |

Regular-user password encryption also uses `internal/backend/password_secret.go`. Current corresponding modules implement persistent merge relations and Counts. The [repository source](https://github.com/Zkunlun/Emby-In-One/tree/main) is the complete tree; this table supplies entry points, not every file.

## Resources and deployment files

| Path | Responsibility |
| --- | --- |
| third_party/sqlite/ | CGO dependency |
| public/admin.html, public/admin.js | Vue 3 panel template and logic |
| public/vendor/ | Self-hosted Vue, lucide, Tailwind output and fonts |
| assets/panel.css, tailwind.config.js, package.json | Style input, class scanning and build:panel |
| Dockerfile, docker-compose.yml | Go multistage builds and runtime mounts |
| install.sh | Source Docker installer |
| release-install.sh | Release binary/systemd installer |
| emby-in-one-cli.sh | SSH management menu |
| legacy/ | V1.2.1 Node reference; excluded from current builds/images/installers |

legacy/src provides early Express/ID-virtualization reference. Its tests and Node dependencies do not validate the current Go implementation.

## Contributions and reports

The current mainline is Go. Reproducible issues, compatibility feedback, feature suggestions and focused pull requests are welcome.

- Add regression coverage for the root cause of bug fixes where practical, rather than patches for only one client symptom.
- Keep scope focused; avoid unrelated architecture or formatting changes in one PR.
- Run tests relevant to the change before submitting. Shared backend behavior should also be checked with go test ./....
- For panel changes, verify frontend builds and embedded resources agree. For installer/release changes, check version strings, scripts and Release workflows together.
- Client compatibility reports should include request paths, response differences, logs or reproduction steps to identify Emby API differences.

Use [Issues](https://github.com/Zkunlun/Emby-In-One/issues) for ordinary reports and [Pull Requests](https://github.com/Zkunlun/Emby-In-One/pulls) for code changes. Include EIO/client versions, authentication/playback mode, reproduction steps and redacted logs. Report security issues privately under the [security policy](../../SECURITY.md#english-security-policy).

## Maintenance and history

This project continues development and maintenance of [ArizeSky/Emby-In-One](https://github.com/ArizeSky/Emby-In-One). Original code, design and historical contributions belong to their respective authors/contributors. This repository is the current maintenance and release entry point; source remains [GPL-3.0](../../LICENSE).

[Changelog (Chinese)](../../Update.md) · [Update plan (Chinese)](../../Update%20Plan.md) · [Version numbering](version-numbering.md) · [Historical documentation](../../README_EN_V1.2.1.md) · [Legacy notes](../../legacy/README.md).

Historical paths remain available but are not the default current-installation route.

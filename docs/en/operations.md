# Operations

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../operations.md)

Applies to the V1.4.9 mainline.

## Daily management and service status

After the installer finishes, open the SSH menu:

```bash
emby-in-one
```

It supports:

- Start/restart/stop.
- Online update to Latest or download a specified version.
- Service status and public IP.
- Administrator username/password storage status, username changes and password resets; hashed plaintext cannot be recovered.
- User listing, regular-user creation and deletion.
- Logs.
- Uninstallation, with an option to retain configuration and data.

The menu detects Binary or Docker deployments and dispatches to systemd or Docker Compose. Docker updates rebuild from source. There is no separate “view version” option: the menu title shows the version (the current release is V1.4.9).

`/usr/local/bin/emby-in-one` is the installed SSH menu script. The default Release executable is `/opt/emby-in-one/emby-in-one`; sharing a name does not make them the same entry point. In the project directory, `./emby-in-one --version` reads the binary version.

For Release deployments, use `sudo systemctl status emby-in-one` and the `start`, `stop` or `restart` actions; follow service output with `sudo journalctl -u emby-in-one -f`. For Compose, use `docker compose ps`, `stop`, `up -d` or `logs -f --tail 50` at the project root. Check actual networking and ports separately.

## Version upgrades

Identify the source version and deployment method, then make a consistent [runtime-data backup](#runtime-data-and-backups). The SSH menu supports Latest or a specified version. Binary deployments update from Releases; Docker deployments rebuild the corresponding released source package.

| Starting point | Data requirements |
| --- | --- |
| V1.4.8 → V1.4.9 | Back up and retain matching configuration, database, tokens, user-password.key and related files |
| Historical V1.5.0/V1.5.1/V1.6.0 → V1.4.7/V1.4.8/V1.4.9 respectively | Same functionality under new numbering; see [version numbering](version-numbering.md). Renaming itself does not require clearing data |
| Old users schema from V1.4.6 or earlier | No automatic lossless migration: an old hash cannot produce password_secret. Back up existing regular-user data, then use a clean data directory and recreate regular users, as the [published upgrade notice (Chinese)](../../Update.md#升级注意事项) requires |

If the schema is uncertain, stop and inspect the existing version and backup first. Pointing dataDir at old files is not a migration procedure. A clean-directory upgrade rebuilds data and does not promise lossless conversion of old watch history. Retain the original directory and backups; do not delete them simply to bypass startup errors.

During upgrades, release-install.sh backs up the old binary and original systemd unit, retains config/data/log and restores relevant files, service state and ownership on failure according to its procedure. It is not a full-directory/database point-in-time snapshot, does not replace a data backup and does not guarantee arbitrary old schemas can start. Optional panel files and the CLI are not complete data rollback either.

## Runtime data and backups

config/config.yaml contains the local administrator hash, plaintext upstream credentials and configuration. The actual dataDir locates other runtime files; see [configuration](configuration.md#data-directory-datadir).

| File | Purpose and backup considerations |
| --- | --- |
| mappings.db | SQLite virtual IDs, instances, users/authorization revisions, watch state and pending cleanup journal |
| mappings.db-wal / mappings.db-shm, when present | SQLite WAL files; do not copy only the live main database. Stop the service and back up the whole dataDir |
| user-password.key | Regular-user AES-GCM key, paired with the database; losing it with existing encrypted passwords prevents initialization |
| tokens.json | EIO proxy tokens and authorization revisions; authentication also checks current user state |
| captured-headers.json | Passthrough identity cache with stable-source ownership |
| emby-in-one.log and rotated files | Application logs that may contain sensitive URLs/headers |

The default Release project contains config/data/log, but application file logs actually live inside dataDir. Backing up only log/ is insufficient. Source/custom-path installations must follow their configuration, not assume /opt/emby-in-one.

### Consistent backup

1. Record the running version, deployment method and configuration; identify the actual dataDir. Stop the systemd or corresponding Compose service and ensure no other EIO process continues writing.
2. Back up config, the complete dataDir and necessary deployment files to a restricted location. Record permissions and service ownership. Do not publish keys or plaintext upstream credentials.
3. Verify the backup is readable and includes matching configuration, mappings.db and user-password.key. Start the original service and check status.

Example for default Release paths; adjust the scope for a custom dataDir:

```bash
sudo systemctl stop emby-in-one
sudo install -d -m 700 /root/eio-backups
eio_backup="/root/eio-backups/eio-$(date +%Y%m%d-%H%M%S).tar.gz"
sudo tar -C /opt/emby-in-one -czf "$eio_backup" config data log
sudo chmod 600 "$eio_backup"
sudo tar -tzf "$eio_backup"
sudo systemctl start emby-in-one
```

Execute step by step, checking each result. Do not upgrade after a failed backup command. For Compose, stop the container and back up complete host config/data directories; do not apply systemd commands to Docker.

### Restore

Stop the service and retain the current state before restoring matching configuration and the complete dataDir from one backup. Choose a compatible version, restore ownership and permissions, confirm working directory/mounts, then start and verify. Do not mix databases, keys or tokens from different times. A newly generated key cannot decrypt passwords encrypted with a lost key.

## Administrator password reset

Prefer the `emby-in-one` SSH menu's administrator-password action. It stops the service, invokes reset-password on the actual binary, restores ownership and starts the service. Check final service status, not just the menu completion message.

Binary syntax, shown as placeholders rather than a command to execute literally: `./emby-in-one --reset-password <new-password|-> [--force]`. In a default Release installation, work in `/opt/emby-in-one` and use `sudo ./emby-in-one --reset-password -` with input supplied through stdin. Do not invoke the menu script as `emby-in-one --reset-password ...`.

Stop the `emby-in-one` service first. The CLI attempts a TCP connection to `127.0.0.1:<configured-port>` and refuses if it connects; this is not an HTTP /System/Info/Public check. `--force` skips only that TCP check, does not establish that the instance is stopped and should not be the default reset option.

Use stdin to avoid passwords in process arguments/history. With `-`, the password is read from stdin. A terminal or calling script should supply it safely; the menu uses `printf '%s' "$new_password" | ./emby-in-one --reset-password -`. Do not put real passwords in example code.

For a manual Compose reset, stop the original service and run `docker compose run --rm --no-deps -T emby-in-one /app/emby-in-one --reset-password -` from its project directory, supplying stdin. Afterwards use `docker compose up -d` and verify. Configuration/data mounts must match the original service. Consider --force only after confirming the service is stopped and identifying a probe conflict.

The CLI saves a new scrypt hash and atomically clears issued proxy tokens while preserving _proxyUserId. All EIO clients must log in again. An online instance holds old tokens in memory and could overwrite the cleanup later, which is why it must be stopped.

Replacing the configuration hash with plaintext and restarting will rehash it, but is not the same token-cleanup procedure. Prefer the menu/CLI for forgotten passwords. If you know the current password, panel settings can update it using currentPassword; see the [admin API](admin-api.md).

## Periodic checks and stream-route state

The default interval is 60 seconds, controlled by timeouts.healthInterval. Each cycle handles two distinct objects:

- **Online API sources:** concurrently probe stream routes and maintain unknown/alive/dead states. This is not periodic parallel GET /System/Info/Public for every online API source.
- **Offline API sources:** reauthenticate sequentially in the loop, using username login or Key /Users/Me validation, constrained by healthCheck/login/api timeouts. Username-authenticated passthrough sources without a captured identity are skipped until client login captures one.

Stream-route state is independent of API state. An online API does not guarantee every dedicated streaming URL works. Route transport errors and 502/503/504 mean unavailable; reachable 404/500 responses do not establish a network failure. Reload retains known state for unchanged stream URLs. dead does not become alive merely with time; recovery follows [playback rules](playback-and-upstream.md#playback-modes).

Startup/reload/login may trigger immediate probes of online routes. State changes are logged; shutdown cancels the periodic timer. Docker HEALTHCHECK tests EIO's own public endpoint, distinct from both upstream authentication and route state.

## Log levels

| Level | Typical content |
| --- | --- |
| DEBUG | Request details, ID resolution and identities |
| INFO | Logins, state and configuration changes |
| WARN | Upstream rejection, disconnection and retained limits |
| ERROR | Request, login or persistence failures |

LOG_LEVEL controls the console threshold and FILE_LOG_LEVEL the file threshold; both default to info. DEBUG is not all written to files by default. Enable it explicitly. Thresholds determine visibility; levels are not permanently assigned to console or files.

## Log files

- Default path: `data/emby-in-one.log`, or `/opt/emby-in-one/data/` for default Release deployment.
- Docker: `/app/data/emby-in-one.log`.
- Maximum file size: 10 MiB; three rotated backups, `emby-in-one.log.1` through `.3`.
- The panel can download or clear logs; clearing also deletes rotated backups.

## Log configuration

The default level is info. Enable full debug output using environment variables:

```bash
LOG_LEVEL=debug FILE_LOG_LEVEL=debug ./emby-in-one
```

For Docker Compose:

```yaml
environment:
  - LOG_LEVEL=debug
  - FILE_LOG_LEVEL=debug
  - LOG_MAX_SIZE_MB=10   # Maximum file size (MB), default 10
  - LOG_KEEP=3           # Rotated backups, default 3
```

LOG_LEVEL controls console output, FILE_LOG_LEVEL disk output. The panel's bounded memory log buffer retains all levels, including DEBUG, independently of these output thresholds. Run source examples from the complete project working directory. For systemd, set Environment in the unit or a drop-in, then reload and restart. Variables in another shell do not change an already running service. Inspect the actual unit with `systemctl cat emby-in-one`.

Logs aid diagnosis and do not establish complete network-audit coverage. Redact credentials in upstream URLs and identity information before sharing. See the [security policy](../../SECURITY.md#english-security-policy).

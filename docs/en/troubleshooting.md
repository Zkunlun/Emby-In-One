# Troubleshooting

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../troubleshooting.md)

Applies to the V1.4.9 mainline.

## Passthrough login fails with 403

On a fresh installation without a real client identity, username/password passthrough upstreams **skip initial login and remain offline** until capture. Initial login does not simply use the Infuse fallback.

1. Log into EIO once as its local **admin** using a real Emby client such as Infuse or Emby iOS.
2. Capture automatically retries offline passthrough upstreams.
3. A successful server identity is persisted and reusable after restart.
4. Inspect identity-source logs: last-success is that server's last successful identity; captured sources reflect real-client identity. infuse-fallback remains the internal final fallback, but initial login/management validation is delayed when it is the only source.
5. If the upstream still rejects the identity, log in as admin again from a client the upstream permits.

## Upstream offline or login timeout

timeouts.login and timeouts.healthCheck each default to 30 seconds. Credentials, upstream policy, network, identity or deadlines can all cause offline status. Inspect state and logs rather than infer a cause solely from upgrade timing.

- Login exceeding timeouts.login fails and marks the server offline. Increase it in **全局设置** (settings) for upstreams taking 10–30 seconds to log in.
- Periodic reauthentication applies only to offline API sources, constrained by timeouts.healthCheck; later cycles retry failures. Online sources have separate stream-route probes.
- These settings can only tighten below timeouts.api. Increase api too when extending probing time.
- Startup output `Timeouts are now enforced` means these two configured values are below api.

## Client identity capture and cleanup

A successful real Emby-client login with usable headers can capture token-scoped UA/device information for administrators or regular users. Panel login may lack these headers. Capture neither changes source authorization nor overrides a selected fixed preset.

Log into EIO using the intended client and inspect the captured-client card. Successful passthrough identities are isolated by stable server ID. Binding changes, deletion, disablement or password changes clear relevant captures and prevent old asynchronous results from republishing. Ambiguous ownership in legacy data causes conservative cleanup and may require another login.

## Playback 403 or 401

Possible causes:

- Expired upstream token: reconnect in the panel.
- Incomplete passthrough headers: inspect `Stream headers for [server name]` in protected logs.
- Changed version/source authorization: fetch current authorized details and PlaybackInfo. MediaSourceId must still match the selected real version; do not send an old A-source ID to B.

## Slow home screen or incomplete libraries

- Default search grace is 3 seconds after the first result. Late results only register mappings/instances in the background; they do not modify the list already returned.
- With high upstream latency, raise searchGracePeriod and metadataGracePeriod in panel settings or configuration.
- latestGracePeriod defaults to 0, waiting for all sources. A positive value can shorten latest-items waiting.
- Inspect timeout or abort messages.
- Consider increasing api (one request) and global (total aggregation deadline).

## Forgotten administrator password

Administrator passwords use irreversible scrypt hashes. Follow the [stopped-service SSH menu/binary CLI reset](operations.md#administrator-password-reset), then verify client relogin and service recovery. Manual configuration edits do not perform the same token cleanup.

## Reverse-proxy logins return 429

If everyone receives `429 Too Many Requests` after five failures, the proxy address may be collapsing client IPs. Compare logged sources with actual ingress. Enable trustProxy only when EIO is reachable exclusively through a trusted proxy:

1. Set `trustProxy: true` under server in config.yaml.
2. Restart.
3. Ensure the proxy **overwrites** X-Real-IP or X-Forwarded-For. Appending with `$proxy_add_x_forwarded_for` lets clients forge IPs to bypass limits or lock out others. See [reverse proxy trust](configuration.md#reverse-proxy-trust-trustproxy).

## Docker cannot reach an upstream

- An upstream URL using localhost points to the container itself. Use the host IP or domain instead.
- For a host service, use host.docker.internal on Docker Desktop or the actual host IP.

## No media after a fresh installation

Check that an upstream exists and authentication works, then explicitly authorize the regular user. Empty authorization does not mean all sources. Handle pending passthrough capture as above. See [first use](getting-started.md).

## Counts are zero or return 503

No authorized sources, or all explicitly offline, can return three zeros. An authorized online source missing a complete cache makes the whole response 503. Cache starts empty after restart; repeated client reads do not trigger collection. Do not substitute partial invented totals. See [counts](media-counts.md). Dedicated CapyPlayer counts-503 verification remains deferred; see [validation scope](release-v1.4.9-validation.md).

## Startup fails after upgrade: old schema or missing key

Retain current files and backups first. Verify the source version, actual dataDir and whether mappings.db/user-password.key belong to the same instance. Old V1.4.6-and-earlier user tables lack password_secret and have no automatic lossless migration. Existing encrypted records with a missing key also prevent initialization; generating a new key cannot decrypt them. Follow [upgrade/recovery](operations.md#version-upgrades), not general database deletion.

## Capacity full or a second device cannot play

409 UPSTREAM_CAPACITY_FULL concerns regular-user authorization capacity; 429 PLAYBACK_DEVICE_LIMIT concerns that user's active-device lease on that upstream. Stopping playback or disabling a user does not automatically release an assigned authorization slot. See [users and permissions](users-and-permissions.md) for release, heartbeats and exact stops.

## Cleanup incomplete

cleanupPending is neither complete success nor complete rollback. Some regular-user access pauses. Administrators retry the relevant management operation under [lifecycle recovery](users-and-permissions.md#management-changes-and-recovery). Persistent recovery failure can stop startup; there is no background retry worker to rely on.

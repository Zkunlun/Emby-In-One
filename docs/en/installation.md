# Installation

[Project home](../../README_EN.md) · [Documentation](README.md) · [简体中文](../installation.md)

Applies to the V1.4.9 mainline.

## Choose a deployment method

| Method | Intended use | Requirements |
| --- | --- | --- |
| Release binary + systemd (recommended) | Everyday Linux deployment | Linux, systemd, root/sudo, Bash, curl, grep, sed, sha256sum and the other script utilities; access to the GitHub API and Release downloads |
| Source installer / Docker Compose | Building your own container | Docker 20.10+, Compose v2, the complete source tree and build-network access |
| Run Go source | Development and debugging | Go 1.23+, a C toolchain for CGO SQLite and the complete source tree |

Releases provide six Linux architectures: amd64, arm64, arm, mips, mipsle and riscv64. A local Go environment is unnecessary for Release binaries. Recommended Linux environments for Docker are Debian 11/12/13 and Ubuntu 22/24; verify other distributions yourself. Docker on Windows/macOS is primarily a development and test route.

Existing installations should first read [upgrades and backups](operations.md#version-upgrades). The first-install instructions below do not promise in-place upgrades of arbitrary old databases.

## First installation with a Release binary

```bash
curl -fsSL -o release-install.sh https://github.com/Zkunlun/Emby-In-One/releases/latest/download/release-install.sh
sudo bash release-install.sh
```

The script is downloaded from the latest formal Release. With no version argument, it queries GitHub Latest again. Script download and version resolution are separate operations, so this is not a pinned-version installation.

The script detects the CPU architecture, downloads the binary, initializes `/opt/emby-in-one/{config,data,log}`, generates a random initial password for admin, and installs and starts the systemd `emby-in-one` service under the eio account. Save the initial password printed at the end. A hashed administrator password cannot later be recovered as plaintext.

The binary embeds the complete panel, including public/vendor dependencies. Optional files on disk take precedence; missing panel files fall back individually to their embedded copies. The CLI menu is a separate script. Releases include SHA256 files, but the installer warns and skips verification if the checksum file cannot be downloaded; do not assume it always fails closed in that case.

The default panel address is `http://server-ip:8096/admin`; clients connect to `http://server-ip:8096`. There are no upstreams initially. Continue with [first use](getting-started.md).

## Pin a Release version

The pinned example uses V1.4.9:

```bash
curl -fsSL -o release-install.sh https://github.com/Zkunlun/Emby-In-One/releases/download/V1.4.9/release-install.sh
sudo bash release-install.sh V1.4.9
```

The URL and argument must name the same version. Pinning only the script URL still resolves Latest when no argument is supplied. Historical V1.5.0/V1.5.1/V1.6.0 addresses were renumbered; see [version numbering](version-numbering.md).

## Source installer

```bash
git clone --branch V1.4.9 https://github.com/Zkunlun/Emby-In-One.git
cd Emby-In-One
sudo bash install.sh
```

This runs the repository's Docker installer, which handles the Docker environment, mount permissions, random administrator password and source-image build. Manage the installation through the SSH `emby-in-one` menu. Developers may choose main explicitly; unpublished source is not equivalent to a stable Release.

## Manual Docker Compose

1. Clone the complete repository, for example using the V1.4.9 command above. Include go.mod, cmd, internal, third_party, public, Dockerfile and docker-compose.yml; copying a few Go files is insufficient.
2. Create the mount directories at the repository root and assign them to Compose's uid/gid 1000:

   ```bash
   mkdir -p config data
   sudo chown -R 1000:1000 config data
   ```

3. Create `config/config.yaml` using the [minimal first-start configuration](configuration.md#minimal-first-start-configuration). Replace the sample administrator password and ensure uid 1000 can read and write the configuration and data directory. Do not overwrite existing configuration or data with the initial template.
4. Build and start from the repository root:

   ```bash
   docker compose build
   docker compose up -d
   ```

The repository Dockerfile builds the image from source. This guide does not supply an official prebuilt-image address. The container reads `/app/config/config.yaml` and uses `/app/data`; Compose mounts host ./config and ./data there. Incorrect directory or configuration permissions can prevent configuration, tokens, keys or SQLite writes.

The Dockerfile HEALTHCHECK requests the container's own `http://127.0.0.1:8096/System/Info/Public`. It is separate from periodic upstream checks. If you change the container service port, update the port mapping and HEALTHCHECK too.

## Run Go source

Use Go 1.23+ and a C toolchain in the complete repository. On Debian/Ubuntu, C build tools can be installed with `apt install build-essential`. SQLite uses the source bundled in third_party; public is a required embedded-resource package.

```bash
mkdir -p config data
# Create config/config.yaml from the configuration guide and replace the sample password
go run ./cmd/emby-in-one
```

See [development](development.md) for verification and versioned builds. Keep public/ when customizing the Dockerfile or build context; omitting it can cause errors such as `package emby-in-one/public is not in std`.

## Historical Node.js version

legacy/ retains V1.2.1 source for reference and is excluded from current Go binaries and container builds. For historical Node.js deployment, consult the [old documentation](../../README_EN_V1.2.1.md), [legacy notes](../../legacy/README.md) and V1.2.1 Source code in the [original Releases](https://github.com/ArizeSky/Emby-In-One/releases). The current Go install.sh is not the historical installer.

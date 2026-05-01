#!/usr/bin/env bash
set -euo pipefail

ROOTFS_IMAGE="${ROOTFS_IMAGE:-${SANDBOX_ROOTFS:-$(pwd)/sandbox/debian-12-slim-arm64.raw}}"
MCP_SERVER="${MCP_SERVER:-${SANDBOX_MCP_SERVER:-$(pwd)/sandbox/mcp-server-linux-arm64}}"
NODE_VERSION="${NODE_VERSION:-20.18.2}"
DEBIAN_IMAGE_URL="${DEBIAN_IMAGE_URL:-https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-arm64.raw}"
DEBIAN_IMAGE_URLS="${DEBIAN_IMAGE_URLS:-$DEBIAN_IMAGE_URL|https://chuangtzu.ftp.acc.umu.se/images/cloud/bookworm/latest/debian-12-genericcloud-arm64.raw|https://saimei.ftp.acc.umu.se/images/cloud/bookworm/latest/debian-12-genericcloud-arm64.raw}"
DEBIAN_SHA512_URL="${DEBIAN_SHA512_URL:-https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS}"
DEBIAN_SHA512_URLS="${DEBIAN_SHA512_URLS:-$DEBIAN_SHA512_URL|https://chuangtzu.ftp.acc.umu.se/images/cloud/bookworm/latest/SHA512SUMS|https://saimei.ftp.acc.umu.se/images/cloud/bookworm/latest/SHA512SUMS}"
SANDBOX_ROOTFS_SOURCE="${SANDBOX_ROOTFS_SOURCE:-}"
DISK_SIZE="${DISK_SIZE:-12g}"
CLOUD_INIT_DIR="${CLOUD_INIT_DIR:-$(dirname "$ROOTFS_IMAGE")/cloud-init}"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "This sandbox image builder currently supports macOS only." >&2
  exit 1
fi

if [[ ! -x "$MCP_SERVER" ]]; then
  echo "MCP server binary not found at $MCP_SERVER" >&2
  echo "Build it with: GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $MCP_SERVER ./cmd/sandbox-mcp-server" >&2
  exit 1
fi

verify_sha512() {
  local file="$1"
  local expected="$2"
  if [[ ! -f "$file" ]]; then
    return 1
  fi
  local actual
  actual="$(shasum -a 512 "$file" | awk '{print $1}')"
  [[ "$actual" == "$expected" ]]
}

download_with_aria2c() {
  local url="$1"
  local output="$2"
  command -v aria2c >/dev/null 2>&1 || return 127
  aria2c \
    --continue=true \
    --max-connection-per-server=8 \
    --split=8 \
    --min-split-size=16M \
    --max-tries=8 \
    --retry-wait=5 \
    --timeout=60 \
    --connect-timeout=30 \
    --allow-overwrite=true \
    --auto-file-renaming=false \
    --dir="$(dirname "$output")" \
    --out="$(basename "$output")" \
    "$url"
}

download_with_wget() {
  local url="$1"
  local output="$2"
  command -v wget >/dev/null 2>&1 || return 127
  wget \
    --continue \
    --tries=8 \
    --waitretry=5 \
    --timeout=60 \
    --read-timeout=60 \
    --output-document="$output" \
    "$url"
}

download_with_curl() {
  local url="$1"
  local output="$2"
  command -v curl >/dev/null 2>&1 || return 127
  curl \
    -fL \
    --retry 8 \
    --retry-all-errors \
    --retry-delay 5 \
    --connect-timeout 30 \
    --speed-limit 1024 \
    --speed-time 180 \
    --continue-at - \
    "$url" \
    -o "$output"
}

download_large_file() {
  local url="$1"
  local output="$2"
  local downloader
  for downloader in aria2c wget curl; do
    echo "    trying $downloader"
    if "download_with_${downloader}" "$url" "$output"; then
      return 0
    fi
  done
  return 1
}

download_small_file() {
  local url="$1"
  local output="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 8 --retry-all-errors --retry-delay 5 --connect-timeout 30 "$url" -o "$output" && return 0
  fi
  if command -v wget >/dev/null 2>&1; then
    wget --tries=8 --waitretry=5 --timeout=60 --output-document="$output" "$url" && return 0
  fi
  return 1
}

mkdir -p "$(dirname "$ROOTFS_IMAGE")" "$CLOUD_INIT_DIR"
rm -f "$(dirname "$ROOTFS_IMAGE")/efi-variable-store" "$ROOTFS_IMAGE.efi"

download_tmp="${ROOTFS_IMAGE}.download"
sha_tmp="${ROOTFS_IMAGE}.SHA512SUMS"

echo "Verifying Debian image checksum"
IFS='|' read -r -a sha_urls <<<"$DEBIAN_SHA512_URLS"
downloaded_sha_url=""
for sha_url in "${sha_urls[@]}"; do
  echo "  checksum: $sha_url"
  if download_small_file "$sha_url" "$sha_tmp"; then
    downloaded_sha_url="$sha_url"
    break
  fi
  echo "Checksum download failed, trying next Debian mirror." >&2
done
if [[ -z "$downloaded_sha_url" ]]; then
  echo "Failed to download SHA512SUMS from all configured mirrors." >&2
  exit 1
fi
image_name="$(basename "$DEBIAN_IMAGE_URL")"
expected="$(awk -v name="$image_name" '$2 == name {print $1}' "$sha_tmp")"
if [[ -z "$expected" ]]; then
  echo "Checksum for $image_name was not found in $downloaded_sha_url" >&2
  exit 1
fi

if [[ -n "$SANDBOX_ROOTFS_SOURCE" ]]; then
  source_path="${SANDBOX_ROOTFS_SOURCE#file://}"
  if [[ ! -f "$source_path" ]]; then
    echo "SANDBOX_ROOTFS_SOURCE does not point to a file: $SANDBOX_ROOTFS_SOURCE" >&2
    exit 1
  fi
  echo "Using local Debian image source: $source_path"
  cp "$source_path" "$download_tmp"
fi

if [[ -f "$download_tmp" ]]; then
  if verify_sha512 "$download_tmp" "$expected"; then
    echo "Using existing verified image download: $download_tmp"
    downloaded_url="$DEBIAN_IMAGE_URL"
  else
    echo "Existing partial image did not pass checksum yet; resuming download." >&2
    downloaded_url=""
  fi
else
  downloaded_url=""
fi

if [[ -z "$downloaded_url" ]]; then
  echo "Downloading Debian 12 slim arm64 base image:"
  IFS='|' read -r -a image_urls <<<"$DEBIAN_IMAGE_URLS"
  for image_url in "${image_urls[@]}"; do
    echo "  $image_url"
    if download_large_file "$image_url" "$download_tmp"; then
      if verify_sha512 "$download_tmp" "$expected"; then
        downloaded_url="$image_url"
        break
      fi
      echo "Downloaded image from $image_url but checksum did not match; trying next mirror." >&2
    fi
    echo "Download failed, trying next Debian mirror." >&2
  done
fi
if [[ -z "$downloaded_url" ]]; then
  echo "Failed to download and verify Debian image from all configured sources." >&2
  echo "You can manually download $DEBIAN_IMAGE_URL and rerun with SANDBOX_ROOTFS_SOURCE=/path/to/debian-12-genericcloud-arm64.raw" >&2
  exit 1
fi

actual="$(shasum -a 512 "$download_tmp" | awk '{print $1}')"
if [[ "$expected" != "$actual" ]]; then
  echo "Checksum mismatch for $download_tmp" >&2
  exit 1
fi

mv "$download_tmp" "$ROOTFS_IMAGE"
rm -f "$sha_tmp"
truncate -s "$DISK_SIZE" "$ROOTFS_IMAGE"
printf '%s\n' "$DEBIAN_IMAGE_URL" >"$(dirname "$ROOTFS_IMAGE")/image-url"

custom_init_b64="$(base64 <"$(pwd)/sandbox/custom-init.sh" | tr -d '\n')"
cat >"$CLOUD_INIT_DIR/user-data" <<CLOUD
#cloud-config
package_update: true
package_upgrade: false
growpart:
  mode: auto
  devices: ['/']
  ignore_growroot_disabled: false
resize_rootfs: true
runcmd:
  - [bash, -lc, "apt-get update"]
  - [bash, -lc, "DEBIAN_FRONTEND=noninteractive apt-get install -y systemd systemd-sysv dbus kmod isc-dhcp-client iproute2 socat curl ca-certificates bash git procps gnupg python3 python3-pip python3-venv busybox"]
  - [bash, -lc, "curl -fsSL https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-arm64.tar.xz -o /tmp/node.tar.xz"]
  - [bash, -lc, "tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 && rm -f /tmp/node.tar.xz"]
  - [bash, -lc, "echo '${custom_init_b64}' | base64 -d >/usr/local/bin/codingman-custom-init.sh && chmod 0755 /usr/local/bin/codingman-custom-init.sh"]
  - [bash, -lc, "/usr/local/bin/codingman-custom-init.sh"]
  - [bash, -lc, "systemctl daemon-reload && systemctl restart codingman-sandbox-mcp.service codingman-vsock-bridge.service"]
final_message: "CodingMan Debian 12 slim sandbox is ready"
CLOUD

cat >"$CLOUD_INIT_DIR/meta-data" <<META
instance-id: codingman-sandbox
local-hostname: codingman-sandbox
META

echo "Sandbox VM image ready: $ROOTFS_IMAGE"
echo "Cloud-init config ready: $CLOUD_INIT_DIR"

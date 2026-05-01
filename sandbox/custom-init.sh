#!/usr/bin/env bash
set -euo pipefail

mkdir -p /workspace /opt/codingman-sandbox
sandbox_mount_unit="$(systemd-escape -p --suffix=mount /opt/codingman-sandbox)"

cat >/etc/systemd/system/workspace.mount <<'UNIT'
[Unit]
Description=CodingMan virtiofs workspace

[Mount]
What=codingman
Where=/workspace
Type=virtiofs
Options=defaults

[Install]
WantedBy=multi-user.target
UNIT

cat >/etc/systemd/system/"$sandbox_mount_unit" <<'UNIT'
[Unit]
Description=CodingMan sandbox host share

[Mount]
What=codingman-sandbox
Where=/opt/codingman-sandbox
Type=virtiofs
Options=defaults

[Install]
WantedBy=multi-user.target
UNIT

cat >/usr/local/bin/codingman-link-workspace <<'UNIT'
#!/usr/bin/env bash
set -euo pipefail
workspace_file=/opt/codingman-sandbox/host-workspace
if [[ ! -s "$workspace_file" ]]; then
  exit 0
fi
host_workspace="$(head -n 1 "$workspace_file")"
case "$host_workspace" in
  /*) ;;
  *) exit 0 ;;
esac
if [[ "$host_workspace" == "/" || "$host_workspace" == "/workspace" ]]; then
  exit 0
fi
mkdir -p "$(dirname "$host_workspace")"
if [[ ! -e "$host_workspace" ]]; then
  ln -s /workspace "$host_workspace"
fi
UNIT
chmod 0755 /usr/local/bin/codingman-link-workspace

cat >/etc/systemd/system/codingman-workspace-link.service <<'UNIT'
[Unit]
Description=Expose workspace at the host absolute path
After=workspace.mount opt-codingman\x2dsandbox.mount
Requires=workspace.mount opt-codingman\x2dsandbox.mount

[Service]
Type=oneshot
ExecStart=/usr/local/bin/codingman-link-workspace
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
UNIT

cat >/etc/systemd/system/codingman-sandbox-mcp.service <<'UNIT'
[Unit]
Description=CodingMan sandbox MCP server
After=workspace.mount opt-codingman\x2dsandbox.mount codingman-workspace-link.service network.target
Requires=workspace.mount opt-codingman\x2dsandbox.mount

[Service]
Type=simple
Environment=CODINGMAN_WORKSPACE=/workspace
Environment=CODINGMAN_MCP_ADDR=127.0.0.1:8080
ExecStart=/opt/codingman-sandbox/mcp-server-linux-arm64
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT

cat >/etc/systemd/system/codingman-vsock-bridge.service <<'UNIT'
[Unit]
Description=CodingMan vsock to MCP TCP bridge
After=codingman-sandbox-mcp.service
Requires=codingman-sandbox-mcp.service

[Service]
Type=simple
ExecStart=/usr/bin/socat VSOCK-LISTEN:8080,fork,reuseaddr TCP:127.0.0.1:8080
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT

systemctl enable workspace.mount
systemctl enable "$sandbox_mount_unit"
systemctl enable codingman-workspace-link.service
systemctl enable codingman-sandbox-mcp.service
systemctl enable codingman-vsock-bridge.service

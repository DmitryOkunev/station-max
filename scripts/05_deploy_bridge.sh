#!/usr/bin/env bash
# Step 05: Build & Deploy Quasar Bridge with SmartTube/TV Apps & Stream Switcher
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

TARGET_SERVER="${1:-}"
STATION_IP="${2:-}"
SERVER_IP="${3:-}"

if [ -z "${STATION_IP}" ]; then
    read -rp "Enter Station IP address (e.g. 192.168.1.50): " STATION_IP
fi

if [ -z "${SERVER_IP}" ]; then
    SERVER_IP=$(hostname -I 2>/dev/null | awk '{print $1}' || ip route get 1.1.1.1 2>/dev/null | awk '{print $7}' || echo "")
    if [ -z "${SERVER_IP}" ]; then
        read -rp "Enter bridge server IP address (e.g. 192.168.1.1): " SERVER_IP
    fi
fi

if [ -z "${TARGET_SERVER}" ]; then
    read -rp "Deploy locally (localhost) or to remote server via SSH (hostname/IP)? [localhost]: " TARGET_SERVER
    TARGET_SERVER="${TARGET_SERVER:-localhost}"
fi

echo "=== [Step 05] Deploying Quasar Bridge to ${TARGET_SERVER} ==="
echo "    Station IP: ${STATION_IP}"
echo "    Server IP:  ${SERVER_IP}"

IS_LOCAL=false
if [ "${TARGET_SERVER}" = "localhost" ] || [ "${TARGET_SERVER}" = "127.0.0.1" ] || [ "${TARGET_SERVER}" = "$(hostname)" ]; then
    IS_LOCAL=true
fi

deploy_local() {
    echo "[*] Deploying locally on $(hostname)..."

    # 1. Clone repository if not present
    if [ ! -d /opt/quasar-bridge ]; then
        echo "[*] Cloning quasar-bridge upstream repository..."
        sudo git clone https://github.com/skrashevich/quasar-bridge.git /opt/quasar-bridge
    fi

    sudo mkdir -p /opt/quasar-bridge/internal/bridge /opt/quasar-bridge/cmd/quasar-bridge /opt/quasar-bridge/bin /opt/smarttube-bridge /usr/local/bin

    # 2. Copy Go source patches
    echo "[*] Applying SmartTube patches to quasar-bridge..."
    sudo cp -r "${BASE_DIR}/quasar-bridge-patches/internal/bridge/"*.go /opt/quasar-bridge/internal/bridge/
    sudo cp -r "${BASE_DIR}/quasar-bridge-patches/cmd/quasar-bridge/"*.go /opt/quasar-bridge/cmd/quasar-bridge/

    # 3. Copy MCP tool
    sudo cp "${BASE_DIR}/configs/quasar-bridge/smarttube_mcp.py" /opt/smarttube-bridge/
    sudo chmod +x /opt/smarttube-bridge/smarttube_mcp.py

    # 4. Copy station-stream utility
    sudo cp "${BASE_DIR}/scripts/station-stream.py" /usr/local/bin/station-stream
    sudo chmod +x /usr/local/bin/station-stream

    # 5. Copy and configure config.json
    sudo cp "${BASE_DIR}/configs/quasar-bridge/config.json" /opt/quasar-bridge/config.json
    sudo python3 -c "
import json
with open('/opt/quasar-bridge/config.json') as f:
    d = json.load(f)
if 'smarttube' in d:
    d['smarttube']['adb_address'] = '${STATION_IP}:5555'
with open('/opt/quasar-bridge/config.json', 'w') as f:
    json.dump(d, f, indent=2)
"

    # 6. Copy runtime.env if missing
    if [ ! -f /opt/quasar-bridge/runtime.env ]; then
        sudo cp "${BASE_DIR}/configs/quasar-bridge/runtime.env.example" /opt/quasar-bridge/runtime.env
        sudo chmod 600 /opt/quasar-bridge/runtime.env
    fi

    # 7. Build binary or use pre-compiled fallback
    if command -v go >/dev/null 2>&1; then
        echo "[*] Building quasar-bridge binary via go build..."
        cd /opt/quasar-bridge
        go build -o /opt/quasar-bridge/bin/quasar-bridge ./cmd/quasar-bridge
    elif [ -f "${BASE_DIR}/binaries/quasar-bridge" ]; then
        echo "[*] Go compiler not found; installing pre-compiled quasar-bridge binary..."
        sudo cp "${BASE_DIR}/binaries/quasar-bridge" /opt/quasar-bridge/bin/quasar-bridge
        sudo chmod +x /opt/quasar-bridge/bin/quasar-bridge
    else
        echo "[-] Error: Go compiler not found and pre-compiled binary missing!"
        exit 1
    fi

    # 8. Install quasar-bridge.service
    sudo cp "${BASE_DIR}/configs/quasar-bridge/quasar-bridge.service" /etc/systemd/system/
    
    # 9. Install adb-station.service
    sed "s|__STATION_IP__|${STATION_IP}|g" "${BASE_DIR}/configs/quasar-bridge/adb-station.service" | sudo tee /etc/systemd/system/adb-station.service >/dev/null

    sudo systemctl daemon-reload
    sudo systemctl enable quasar-bridge adb-station
    sudo systemctl restart quasar-bridge adb-station

    # 10. Switch station stream to bridge
    echo "[*] Switching Station Max stream to Quasar Bridge..."
    station-stream bridge "${STATION_IP}" "${SERVER_IP}"
}

deploy_remote() {
    echo "[*] Copying assets to ${TARGET_SERVER} via SSH/SCP..."
    ssh "${TARGET_SERVER}" "mkdir -p /tmp/station_max_bridge /tmp/bridge_internal /tmp/bridge_cmd"
    
    scp "${BASE_DIR}/quasar-bridge-patches/internal/bridge/"*.go "${TARGET_SERVER}:/tmp/bridge_internal/"
    scp "${BASE_DIR}/quasar-bridge-patches/cmd/quasar-bridge/"*.go "${TARGET_SERVER}:/tmp/bridge_cmd/"
    scp "${BASE_DIR}/configs/quasar-bridge/smarttube_mcp.py" "${TARGET_SERVER}:/tmp/station_max_bridge/"
    scp "${BASE_DIR}/configs/quasar-bridge/config.json" "${TARGET_SERVER}:/tmp/station_max_bridge/"
    scp "${BASE_DIR}/configs/quasar-bridge/runtime.env.example" "${TARGET_SERVER}:/tmp/station_max_bridge/"
    scp "${BASE_DIR}/configs/quasar-bridge/quasar-bridge.service" "${TARGET_SERVER}:/tmp/station_max_bridge/"
    scp "${BASE_DIR}/configs/quasar-bridge/adb-station.service" "${TARGET_SERVER}:/tmp/station_max_bridge/"
    scp "${BASE_DIR}/scripts/station-stream.py" "${TARGET_SERVER}:/tmp/station_max_bridge/"
    [ -f "${BASE_DIR}/binaries/quasar-bridge" ] && scp "${BASE_DIR}/binaries/quasar-bridge" "${TARGET_SERVER}:/tmp/station_max_bridge/" || true

    ssh "${TARGET_SERVER}" bash -s << REMOTE_EOF
set -euo pipefail
if [ ! -d /opt/quasar-bridge ]; then
    echo "[*] Cloning quasar-bridge upstream repository..."
    sudo git clone https://github.com/skrashevich/quasar-bridge.git /opt/quasar-bridge
fi

sudo mkdir -p /opt/quasar-bridge/internal/bridge /opt/quasar-bridge/cmd/quasar-bridge /opt/smarttube-bridge /opt/quasar-bridge/bin /usr/local/bin

sudo cp /tmp/bridge_internal/*.go /opt/quasar-bridge/internal/bridge/
sudo cp /tmp/bridge_cmd/*.go /opt/quasar-bridge/cmd/quasar-bridge/
sudo cp /tmp/station_max_bridge/smarttube_mcp.py /opt/smarttube-bridge/
sudo chmod +x /opt/smarttube-bridge/smarttube_mcp.py

sudo cp /tmp/station_max_bridge/station-stream.py /usr/local/bin/station-stream
sudo chmod +x /usr/local/bin/station-stream

sudo cp /tmp/station_max_bridge/config.json /opt/quasar-bridge/config.json
sudo python3 -c "
import json
with open('/opt/quasar-bridge/config.json') as f:
    d = json.load(f)
if 'smarttube' in d:
    d['smarttube']['adb_address'] = '${STATION_IP}:5555'
with open('/opt/quasar-bridge/config.json', 'w') as f:
    json.dump(d, f, indent=2)
"

if [ ! -f /opt/quasar-bridge/runtime.env ]; then
    sudo cp /tmp/station_max_bridge/runtime.env.example /opt/quasar-bridge/runtime.env
    sudo chmod 600 /opt/quasar-bridge/runtime.env
fi

if command -v go >/dev/null 2>&1; then
    echo "[*] Building quasar-bridge binary on remote host..."
    cd /opt/quasar-bridge
    go build -o /opt/quasar-bridge/bin/quasar-bridge ./cmd/quasar-bridge
elif [ -f /tmp/station_max_bridge/quasar-bridge ]; then
    echo "[*] Installing pre-compiled quasar-bridge binary..."
    sudo cp /tmp/station_max_bridge/quasar-bridge /opt/quasar-bridge/bin/quasar-bridge
    sudo chmod +x /opt/quasar-bridge/bin/quasar-bridge
fi

sudo cp /tmp/station_max_bridge/quasar-bridge.service /etc/systemd/system/
sed "s|__STATION_IP__|${STATION_IP}|g" /tmp/station_max_bridge/adb-station.service | sudo tee /etc/systemd/system/adb-station.service >/dev/null

sudo systemctl daemon-reload
sudo systemctl enable quasar-bridge adb-station
sudo systemctl restart quasar-bridge adb-station

# Clean tmp
rm -rf /tmp/station_max_bridge /tmp/bridge_internal /tmp/bridge_cmd

# Switch station stream
echo "[*] Switching Station Max stream to bridge..."
/usr/local/bin/station-stream bridge "${STATION_IP}" "${SERVER_IP}"
REMOTE_EOF
}

if [ "$IS_LOCAL" = true ]; then
    deploy_local
else
    deploy_remote
fi

echo "=== [Step 05] Bridge deployment complete! ==="

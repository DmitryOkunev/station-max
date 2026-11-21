#!/bin/bash
# rollback_standalone.sh - Instantly revert Yandex Station Max back to external server setup
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Source environment
if [ -f "${ROOT_DIR}/.station_env" ]; then
    # shellcheck disable=SC1091
    source "${ROOT_DIR}/.station_env"
fi

STATION_IP="${1:-${STATION_IP:-}}"
if [ -z "${STATION_IP}" ]; then
    read -rp "Enter Station Max IP address (e.g. 192.168.1.50): " STATION_IP
    while [ -z "${STATION_IP}" ]; do
        read -rp "Station IP is required. Please enter Station Max IP: " STATION_IP
    done
fi

SERVER_IP="${2:-${SERVER_IP:-}}"
if [ -z "${SERVER_IP}" ]; then
    read -rp "Enter External Server IP address (e.g. 192.168.1.1): " SERVER_IP
    while [ -z "${SERVER_IP}" ]; do
        read -rp "Server IP is required. Please enter Server IP: " SERVER_IP
    done
fi
SSH_PORT="${SSH_PORT:-2222}"
SSH_CMD="ssh -o StrictHostKeyChecking=no -p ${SSH_PORT} root@${STATION_IP}"

echo "=========================================================="
echo ">>> Rolling back Standalone mode -> External Server mode"
echo ">>> Station IP: ${STATION_IP}"
echo ">>> Target External Server IP: ${SERVER_IP}"
echo "=========================================================="

# 1. Stop on-device Quasar Bridge and remove enabled sentinel
echo "[1/4] Stopping on-device Quasar Bridge daemon..."
${SSH_CMD} '
    rm -f /data/quasar-bridge/enabled
    killall quasar-bridge 2>/dev/null || pkill -x quasar-bridge || true
'
echo "[+] On-device Quasar Bridge disabled and stopped."

# 2. Ensure external server Quasar Bridge service is active
echo "[2/4] Ensuring external Quasar Bridge on ${SERVER_IP} is active..."
if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=3 root@"${SERVER_IP}" "true" 2>/dev/null; then
    ssh root@"${SERVER_IP}" "systemctl start quasar-bridge"
elif ssh -o StrictHostKeyChecking=no -o ConnectTimeout=3 "home" "sudo systemctl start quasar-bridge" 2>/dev/null; then
    echo "[+] quasar-bridge.service verified on home server."
fi

# 3. Redirect Station voice stream back to external server
echo "[3/4] Redirecting voice stream on Station Max to ws://${SERVER_IP}:8443/uni.ws..."
python3 "${ROOT_DIR}/scripts/station-stream.py" bridge "${STATION_IP}" "${SERVER_IP}"

# 4. Optional: Re-enable updaterapp if needed
${SSH_CMD} "pm enable com.yandex.launcher.updaterapp 2>/dev/null || true"

echo "=========================================================="
echo ">>> Rollback Complete!"
echo ">>> Voice stream returned to external server: ws://${SERVER_IP}:8443/uni.ws"
echo "=========================================================="

#!/bin/bash
# deploy_standalone.sh - Deploy All-in-One Standalone Quasar Bridge directly onto Yandex Station Max
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
SSH_PORT="${SSH_PORT:-2222}"
SSH_CMD="ssh -o StrictHostKeyChecking=no -p ${SSH_PORT} root@${STATION_IP}"

echo "=========================================================="
echo ">>> Deploying Standalone Quasar Bridge onto Station Max"
echo ">>> Target Station IP: ${STATION_IP}:${SSH_PORT}"
echo "=========================================================="

# 1. Verify SSH connectivity
echo "[1/8] Verifying SSH connectivity..."
if ! ${SSH_CMD} "id" >/dev/null 2>&1; then
    echo "[-] Error: Unable to connect via SSH to root@${STATION_IP}:${SSH_PORT}"
    exit 1
fi
echo "[+] Root SSH connection OK: $(${SSH_CMD} "id")"

# 2. Check SELinux mode
ENFORCE=$(${SSH_CMD} "cat /sys/fs/selinux/enforce 2>/dev/null || echo 1")
if [ "${ENFORCE}" != "0" ]; then
    echo "[!] Warning: SELinux enforce is ${ENFORCE}. Triggering fix-enforce..."
    ${SSH_CMD} "/odm/bin/fix-enforce.sh &"
    sleep 3
fi

# 3. Configure DNS and CA Certificates on Station
echo "[2/8] Ensuring DNS and CA certificates in /data/dropbear/etc..."
${SSH_CMD} '
    mkdir -p /data/dropbear/etc/ssl/certs
    if [ ! -f /data/dropbear/etc/resolv.conf ]; then
        echo -e "nameserver 77.88.8.8\nnameserver 1.1.1.1" > /data/dropbear/etc/resolv.conf
    fi
    find /data/dropbear/etc -type d -exec chmod 755 {} +
    find /data/dropbear/etc -type f -exec chmod 644 {} +
    chcon -R u:object_r:system_file:s0 /data/dropbear/etc
'
echo "[+] DNS and CA certs verified."

# 4. Disable OTA updates directly on Android
echo "[3/8] Disabling OTA updaterapp on Station Max..."
${SSH_CMD} "pm disable com.yandex.launcher.updaterapp 2>/dev/null || true"
echo "[+] OTA updates frozen."

# 5. Push Quasar Bridge ARM64 binary
echo "[4/8] Installing /odm/bin/quasar-bridge..."
if [ ! -f "${ROOT_DIR}/binaries/quasar-bridge-arm64" ]; then
    echo "[-] Error: ${ROOT_DIR}/binaries/quasar-bridge-arm64 not found!"
    exit 1
fi

cat "${ROOT_DIR}/binaries/quasar-bridge-arm64" | ${SSH_CMD} "
    cat > /odm/bin/quasar-bridge.new &&
    chmod 755 /odm/bin/quasar-bridge.new &&
    chcon u:object_r:adbd_exec:s0 /odm/bin/quasar-bridge.new &&
    mv -f /odm/bin/quasar-bridge.new /odm/bin/quasar-bridge
"
echo "[+] Binary installed: $(${SSH_CMD} "ls -lh /odm/bin/quasar-bridge")"

# 6. Deploy configurations and prompts to /data/quasar-bridge
echo "[5/8] Deploying configs and prompts to /data/quasar-bridge..."
${SSH_CMD} "mkdir -p /data/quasar-bridge/prompts"

cat "${ROOT_DIR}/configs/quasar-bridge/config.standalone.json" | ${SSH_CMD} "cat > /data/quasar-bridge/config.json && chmod 644 /data/quasar-bridge/config.json"
cat "${ROOT_DIR}/configs/quasar-bridge/llm.json" | ${SSH_CMD} "cat > /data/quasar-bridge/llm.json && chmod 600 /data/quasar-bridge/llm.json"
cat "${ROOT_DIR}/configs/quasar-bridge/runtime.env" | ${SSH_CMD} "cat > /data/quasar-bridge/runtime.env && chmod 600 /data/quasar-bridge/runtime.env"

if [ -f "${ROOT_DIR}/configs/quasar-bridge/prompts/podskazhi.txt" ]; then
    cat "${ROOT_DIR}/configs/quasar-bridge/prompts/podskazhi.txt" | ${SSH_CMD} "cat > /data/quasar-bridge/prompts/podskazhi.txt && chmod 644 /data/quasar-bridge/prompts/podskazhi.txt"
fi
if [ -f "${ROOT_DIR}/configs/quasar-bridge/prompts/slysh.txt" ]; then
    cat "${ROOT_DIR}/configs/quasar-bridge/prompts/slysh.txt" | ${SSH_CMD} "cat > /data/quasar-bridge/prompts/slysh.txt && chmod 644 /data/quasar-bridge/prompts/slysh.txt"
fi
echo "[+] Configs and prompts deployed."

# 7. Install startup scripts and enable flag
echo "[6/8] Installing startup scripts in /odm/bin/..."
cat "${ROOT_DIR}/configs/odm/start-quasar.sh" | ${SSH_CMD} "
    cat > /odm/bin/start-quasar.sh &&
    chmod 755 /odm/bin/start-quasar.sh &&
    chcon u:object_r:adbd_exec:s0 /odm/bin/start-quasar.sh
"
cat "${ROOT_DIR}/configs/odm/start-dropbear.sh" | ${SSH_CMD} "
    cat > /odm/bin/start-dropbear.sh &&
    chmod 755 /odm/bin/start-dropbear.sh &&
    chcon u:object_r:adbd_exec:s0 /odm/bin/start-dropbear.sh
"
${SSH_CMD} "touch /data/quasar-bridge/enabled"

# 8. Start Quasar Bridge daemon on Station Max
echo "[7/8] Starting Quasar Bridge daemon on Station Max..."
${SSH_CMD} "
    killall quasar-bridge 2>/dev/null || pkill -x quasar-bridge || true
    sleep 1
    /odm/bin/start-quasar.sh
"

# Wait for port 8443 to open
echo "Waiting for on-device Quasar Bridge port 8443..."
STARTED=false
for i in $(seq 1 15); do
    if ${SSH_CMD} "toybox netstat -tlpn 2>/dev/null | grep -q ':8443'" >/dev/null 2>&1; then
        STARTED=true
        break
    fi
    sleep 1
done

if [ "$STARTED" = true ]; then
    echo "[+] Standalone Quasar Bridge is actively listening on port 8443!"
    ${SSH_CMD} "pgrep -fl /odm/bin/quasar-bridge"
else
    echo "[-] Warning: Port 8443 not detected within 15s. Checking logs:"
    ${SSH_CMD} "tail -n 20 /data/quasar-bridge/bridge.log 2>/dev/null || true"
fi

# 9. Redirect stream to local loopback (127.0.0.1)
echo "[8/8] Switching voice stream to 127.0.0.1:8443..."
python3 "${ROOT_DIR}/scripts/station-stream.py" bridge "${STATION_IP}" 127.0.0.1

echo "=========================================================="
echo ">>> Standalone Quasar Bridge deployed successfully!"
echo ">>> Stream: ws://127.0.0.1:8443/uni.ws"
echo ">>> To rollback to external server, run: ./scripts/rollback_standalone.sh"
echo "=========================================================="

#!/usr/bin/env bash
# Master Deployment Orchestrator for Yandex Station Max
# Full automated pipeline from server setup to root, apps, and Quasar bridge.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

STATION_IP="${1:-}"
SERVER_IP="${2:-}"

echo "================================================================="
echo "   Yandex Station Max (YNDX-0008 / YNDX-00052) Master Deployer   "
echo "================================================================="

AUTO_SERVER_IP=$(hostname -I 2>/dev/null | awk '{print $1}' || ip route get 1.1.1.1 2>/dev/null | awk '{print $7}' || echo "")

if [ -z "${STATION_IP}" ]; then
    read -rp "Enter Station Max IP address (e.g. 192.168.1.50): " STATION_IP
    while [ -z "${STATION_IP}" ]; do
        read -rp "Station IP is required. Please enter Station Max IP: " STATION_IP
    done
fi

if [ -z "${SERVER_IP}" ]; then
    if [ -n "${AUTO_SERVER_IP}" ]; then
        read -rp "Enter Bridge Server / Router IP address [${AUTO_SERVER_IP}]: " SERVER_IP
        SERVER_IP="${SERVER_IP:-${AUTO_SERVER_IP}}"
    else
        read -rp "Enter Bridge Server / Router IP address (e.g. 192.168.1.1): " SERVER_IP
        while [ -z "${SERVER_IP}" ]; do
            read -rp "Server IP is required. Please enter Server IP: " SERVER_IP
        done
    fi
fi

echo ""
echo "Target Station: ${STATION_IP}"
echo "Target Server:  ${SERVER_IP}"
echo "Base Directory: ${BASE_DIR}"
echo ""

prompt_step() {
    local step_name="$1"
    local default_ans="${2:-y}"
    echo ""
    echo "-----------------------------------------------------------------"
    read -rp "Run Step [${step_name}]? (y/n) [${default_ans}]: " ans
    ans="${ans:-${default_ans}}"
    [[ "$ans" =~ ^[Yy]$ ]]
}

# -------------------------------------------------------------
# STEP 01: Server Setup (Fakequasar Nginx + Dnsmasq)
# -------------------------------------------------------------
if prompt_step "01: Server Setup (Fakequasar & DNS)" "y"; then
    echo "[*] Executing Step 01..."
    "${SCRIPT_DIR}/01_setup_server.sh" "${SERVER_IP}"
fi

# -------------------------------------------------------------
# STEP 02: Firmware Downgrade
# -------------------------------------------------------------
if prompt_step "02: Firmware Downgrade to 5000000921 (skip if already on 5000000921)" "n"; then
    echo "[*] Executing Step 02..."
    "${SCRIPT_DIR}/02_trigger_downgrade.sh" "${SERVER_IP}" "${STATION_IP}"
fi

# -------------------------------------------------------------
# STEP 03: Exploit Mali GPU, Root & Dropbear SSH
# -------------------------------------------------------------
if prompt_step "03: Exploit Mali GPU, Root & Dropbear SSH" "y"; then
    echo "[*] Executing Step 03..."
    "${SCRIPT_DIR}/03_exploit_root.sh" "${STATION_IP}"
fi

# -------------------------------------------------------------
# STEP 04: Install & Configure TV Applications
# -------------------------------------------------------------
if prompt_step "04: Install Apps (SmartTube, Zona, LazyMedia)" "y"; then
    echo "[*] Executing Step 04..."
    "${SCRIPT_DIR}/04_install_apps.sh" "${STATION_IP}"
fi

# -------------------------------------------------------------
# STEP 05: Build & Deploy Quasar Bridge with Stream Switcher
# -------------------------------------------------------------
if prompt_step "05: Deploy Quasar Bridge & Switch Stream" "y"; then
    echo "[*] Executing Step 05..."
    "${SCRIPT_DIR}/05_deploy_bridge.sh" "${SERVER_IP}" "${STATION_IP}" "${SERVER_IP}"
fi

echo ""
echo "================================================================="
echo "   All selected steps completed successfully!                    "
echo "================================================================="
echo "Quick verification commands:"
echo "  - Check SSH:         ssh -p 2222 root@${STATION_IP}"
echo "  - Check SELinux:     adb -s ${STATION_IP}:5555 shell getenforce"
echo "  - Check Bridge WS:   station-stream status ${STATION_IP}"
echo "  - Stream switch:     station-stream bridge ${STATION_IP} ${SERVER_IP}"
echo "  - Revert to Yandex:  station-stream yandex ${STATION_IP}"
echo "================================================================="

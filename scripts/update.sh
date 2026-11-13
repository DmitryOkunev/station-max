#!/usr/bin/env bash
# Fast Differential Updater for Yandex Station Max & Quasar Bridge
set -euo pipefail

# Resolve symlink to real directory
SOURCE="${BASH_SOURCE[0]}"
while [ -h "$SOURCE" ]; do
    DIR="$(cd -P "$(dirname "$SOURCE")" && pwd)"
    SOURCE="$(readlink "$SOURCE")"
    [[ $SOURCE != /* ]] && SOURCE="$DIR/$SOURCE"
done
SCRIPT_DIR="$(cd -P "$(dirname "$SOURCE")" && pwd)"
BASE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENV_FILE="${BASE_DIR}/.station_env"

# Colors
C_RESET="\033[0m"
C_GREEN="\033[1;32m"
C_YELLOW="\033[1;33m"
C_CYAN="\033[1;36m"
C_RED="\033[1;31m"

# Load saved targets if present
SAVED_HOST=""
SAVED_STATION=""
if [ -f "${ENV_FILE}" ]; then
    # shellcheck disable=SC1090
    source "${ENV_FILE}" 2>/dev/null || true
    SAVED_HOST="${TARGET_HOST:-}"
    SAVED_STATION="${STATION_IP:-}"
fi

# Parsing arguments
TARGET_HOST=""
STATION_IP=""
UPDATE_BRIDGE=false
UPDATE_STATION=false
UPDATE_APPS=false
UPDATE_CONFIGS=false
UPDATE_BOT=false
UPDATE_AIRPLAY=false
EXPLICIT_COMPONENT=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        -b|--bridge)
            UPDATE_BRIDGE=true
            EXPLICIT_COMPONENT=true
            shift
            ;;
        -s|--station)
            UPDATE_STATION=true
            EXPLICIT_COMPONENT=true
            shift
            ;;
        -a|--apps)
            UPDATE_APPS=true
            EXPLICIT_COMPONENT=true
            shift
            ;;
        -c|--configs)
            UPDATE_CONFIGS=true
            EXPLICIT_COMPONENT=true
            shift
            ;;
        -t|--bot)
            UPDATE_BOT=true
            EXPLICIT_COMPONENT=true
            shift
            ;;
        -p|--airplay|--airplay2)
            UPDATE_AIRPLAY=true
            EXPLICIT_COMPONENT=true
            shift
            ;;
        --all)
            UPDATE_BRIDGE=true
            UPDATE_STATION=true
            UPDATE_APPS=true
            UPDATE_CONFIGS=true
            UPDATE_BOT=true
            UPDATE_AIRPLAY=true
            EXPLICIT_COMPONENT=true
            shift
            ;;
        -h|--help)
            echo "Usage: ./update.sh [SSH_HOST] [STATION_IP] [FLAGS]"
            echo ""
            echo "Arguments:"
            echo "  SSH_HOST     Target bridge server (hostname or IP, or 'localhost')"
            echo "  STATION_IP   Yandex Station Max IP address"
            echo ""
            echo "Flags:"
            echo "  -b, --bridge   Update only Quasar Bridge (Go sources, MCP, binary, restart)"
            echo "  -s, --station  Update only Station Max ODM scripts & permissions"
            echo "  -a, --apps     Update / re-install TV APKs (SmartTube, Zona, LazyMedia)"
            echo "  -c, --configs  Update configuration templates (Nginx, DNS, Stream switcher)"
            echo "  -t, --bot      Build and deploy Telegram Control Bot (station-bot)"
            echo "  -p, --airplay  Update and deploy AirPlay 2 stack to Station Max"
            echo "  --all          Update all components"
            echo "  -h, --help     Show this help message"
            echo ""
            echo "Examples:"
            echo "  ./update.sh home 192.168.1.50 --bridge"
            echo "  ./update.sh 192.168.1.1 192.168.1.50"
            echo "  ./update.sh --station"
            echo "  ./update.sh --bot"
            echo "  ./update.sh --airplay"
            exit 0
            ;;
        *)
            if [ -z "${TARGET_HOST}" ]; then
                TARGET_HOST="$1"
            elif [ -z "${STATION_IP}" ]; then
                STATION_IP="$1"
            fi
            shift
            ;;
    esac
done

# Default: update bridge + station scripts if no explicit component flag given
if [ "$EXPLICIT_COMPONENT" = false ]; then
    UPDATE_BRIDGE=true
    UPDATE_STATION=true
    UPDATE_CONFIGS=true
fi

# Fallback to saved environment or prompt
if [ -z "${TARGET_HOST}" ]; then
    TARGET_HOST="${SAVED_HOST}"
fi
if [ -z "${TARGET_HOST}" ]; then
    read -rp "Enter Bridge Server hostname or IP [localhost]: " TARGET_HOST
    TARGET_HOST="${TARGET_HOST:-localhost}"
fi

if [ -z "${STATION_IP}" ]; then
    STATION_IP="${SAVED_STATION}"
fi
if [ -z "${STATION_IP}" ] && { [ "$UPDATE_STATION" = true ] || [ "$UPDATE_APPS" = true ]; }; then
    read -rp "Enter Station Max IP address: " STATION_IP
fi

# Save targets to .station_env for next runs
cat << EOF > "${ENV_FILE}"
TARGET_HOST="${TARGET_HOST}"
STATION_IP="${STATION_IP}"
EOF
chmod 600 "${ENV_FILE}"

echo -e "${C_CYAN}=== Yandex Station Max Differential Updater ===${C_RESET}"
echo -e "Target Server:  ${C_YELLOW}${TARGET_HOST}${C_RESET}"
echo -e "Target Station: ${C_YELLOW}${STATION_IP:-[Not specified]}${C_RESET}"
echo -e "Plan: Bridge=${UPDATE_BRIDGE}, Station=${UPDATE_STATION}, Apps=${UPDATE_APPS}, Configs=${UPDATE_CONFIGS}, Bot=${UPDATE_BOT}"
echo ""

IS_LOCAL=false
if [ "${TARGET_HOST}" = "localhost" ] || [ "${TARGET_HOST}" = "127.0.0.1" ] || [ "${TARGET_HOST}" = "$(hostname)" ]; then
    IS_LOCAL=true
fi

# ==============================================================================
# 1. UPDATE QUASAR BRIDGE
# ==============================================================================
if [ "$UPDATE_BRIDGE" = true ]; then
    echo -e "${C_CYAN}[*] Updating Quasar Bridge on ${TARGET_HOST}...${C_RESET}"
    
    if [ "$IS_LOCAL" = true ]; then
        # Copy Go sources
        sudo cp "${BASE_DIR}/quasar-bridge-patches/internal/bridge/"*.go /opt/quasar-bridge/internal/bridge/
        sudo cp "${BASE_DIR}/quasar-bridge-patches/cmd/quasar-bridge/"*.go /opt/quasar-bridge/cmd/quasar-bridge/
        if [ -f "${BASE_DIR}/configs/quasar-bridge/llm.json" ]; then
            sudo cp "${BASE_DIR}/configs/quasar-bridge/llm.json" /opt/quasar-bridge/llm.json
            sudo chmod 644 /opt/quasar-bridge/llm.json
        fi
        if [ -d "${BASE_DIR}/configs/quasar-bridge/prompts" ]; then
            sudo mkdir -p /opt/quasar-bridge/prompts
            sudo cp "${BASE_DIR}/configs/quasar-bridge/prompts/"*.txt /opt/quasar-bridge/prompts/
            sudo chmod 644 /opt/quasar-bridge/prompts/*.txt
        fi
        sudo chmod -R a+rX /opt/quasar-bridge
        sudo cp "${BASE_DIR}/configs/quasar-bridge/smarttube_mcp.py" /opt/smarttube-bridge/smarttube_mcp.py
        sudo chmod +x /opt/smarttube-bridge/smarttube_mcp.py
        sudo cp "${BASE_DIR}/scripts/station-stream.py" /usr/local/bin/station-stream
        sudo chmod +x /usr/local/bin/station-stream

        # Recompile or update binary
        if command -v go >/dev/null 2>&1; then
            echo "[*] Rebuilding quasar-bridge binary..."
            (cd /opt/quasar-bridge && sudo go build -o /opt/quasar-bridge/bin/quasar-bridge ./cmd/quasar-bridge)
        elif [ -f "${BASE_DIR}/binaries/quasar-bridge" ]; then
            echo "[*] Installing pre-compiled quasar-bridge binary..."
            sudo cp "${BASE_DIR}/binaries/quasar-bridge" /opt/quasar-bridge/bin/quasar-bridge
            sudo chmod +x /opt/quasar-bridge/bin/quasar-bridge
        fi

        # Restart service
        sudo systemctl restart quasar-bridge
        if systemctl list-unit-files quasar-bridge-audio.service >/dev/null 2>&1; then
            sudo systemctl restart quasar-bridge-audio || true
        fi
        sleep 1
        if systemctl is-active quasar-bridge >/dev/null 2>&1; then
            echo -e "${C_GREEN}[+] Quasar Bridge successfully updated and running!${C_RESET}"
        else
            echo -e "${C_RED}[-] Quasar Bridge service failed to start! Check: journalctl -u quasar-bridge -n 20${C_RESET}"
        fi
    else
        # Remote deploy via SSH
        ssh "${TARGET_HOST}" "mkdir -p /tmp/update_bridge_internal /tmp/update_bridge_cmd /tmp/update_pkg"
        scp "${BASE_DIR}/quasar-bridge-patches/internal/bridge/"*.go "${TARGET_HOST}:/tmp/update_bridge_internal/"
        scp "${BASE_DIR}/quasar-bridge-patches/cmd/quasar-bridge/"*.go "${TARGET_HOST}:/tmp/update_bridge_cmd/"
        scp "${BASE_DIR}/configs/quasar-bridge/smarttube_mcp.py" "${TARGET_HOST}:/tmp/update_pkg/"
        scp "${BASE_DIR}/scripts/station-stream.py" "${TARGET_HOST}:/tmp/update_pkg/"
        if [ -f "${BASE_DIR}/configs/quasar-bridge/llm.json" ]; then
            scp "${BASE_DIR}/configs/quasar-bridge/llm.json" "${TARGET_HOST}:/tmp/update_pkg/"
        fi
        if [ -d "${BASE_DIR}/configs/quasar-bridge/prompts" ]; then
            scp "${BASE_DIR}/configs/quasar-bridge/prompts/"*.txt "${TARGET_HOST}:/tmp/update_pkg/"
        fi

        ssh "${TARGET_HOST}" bash -s << 'REMOTE_EOF'
set -euo pipefail
sudo mkdir -p /opt/quasar-bridge/internal/bridge /opt/quasar-bridge/cmd/quasar-bridge /opt/smarttube-bridge /usr/local/bin

sudo cp /tmp/update_bridge_internal/*.go /opt/quasar-bridge/internal/bridge/
sudo cp /tmp/update_bridge_cmd/*.go /opt/quasar-bridge/cmd/quasar-bridge/
sudo chmod -R a+rX /opt/quasar-bridge
sudo cp /tmp/update_pkg/smarttube_mcp.py /opt/smarttube-bridge/smarttube_mcp.py
sudo chmod +x /opt/smarttube-bridge/smarttube_mcp.py
sudo cp /tmp/update_pkg/station-stream.py /usr/local/bin/station-stream
sudo chmod +x /usr/local/bin/station-stream
if [ -f /tmp/update_pkg/llm.json ]; then
    sudo cp /tmp/update_pkg/llm.json /opt/quasar-bridge/llm.json
    sudo chmod 644 /opt/quasar-bridge/llm.json
fi
if ls /tmp/update_pkg/*.txt >/dev/null 2>&1; then
    sudo mkdir -p /opt/quasar-bridge/prompts
    sudo cp /tmp/update_pkg/*.txt /opt/quasar-bridge/prompts/
    sudo chmod 644 /opt/quasar-bridge/prompts/*.txt
fi

if command -v go >/dev/null 2>&1; then
    echo "[*] Rebuilding quasar-bridge binary on remote host..."
    cd /opt/quasar-bridge
    sudo go build -o /opt/quasar-bridge/bin/quasar-bridge ./cmd/quasar-bridge
fi

sudo systemctl restart quasar-bridge
if systemctl list-unit-files quasar-bridge-audio.service >/dev/null 2>&1; then
    sudo systemctl restart quasar-bridge-audio || true
fi
sleep 1
if systemctl is-active quasar-bridge >/dev/null 2>&1; then
    echo "[+] Remote Quasar Bridge successfully updated and running!"
else
    echo "[-] Quasar Bridge service failed to start!"
    systemctl status quasar-bridge --no-pager || true
fi
rm -rf /tmp/update_bridge_internal /tmp/update_bridge_cmd /tmp/update_pkg
REMOTE_EOF
    fi
fi

# ==============================================================================
# 2. UPDATE STATION ODM SCRIPTS & PERMISSIONS
# ==============================================================================
if [ "$UPDATE_STATION" = true ] && [ -n "${STATION_IP}" ]; then
    echo -e "${C_CYAN}[*] Updating scripts on Station Max (${STATION_IP})...${C_RESET}"
    
    # Check if SSH port 2222 is alive
    if nc -zv -w 2 "${STATION_IP}" 2222 >/dev/null 2>&1; then
        echo "[*] Using SSH (port 2222) for instant script sync..."
        
        # Stream files into /data/local/tmp via SSH stdin (avoids sftp-server dependency)
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/fix-enforce.sh" < "${BASE_DIR}/configs/odm/fix-enforce.sh"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/start-dropbear.sh" < "${BASE_DIR}/configs/odm/start-dropbear.sh"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/start-airplay2.sh" < "${BASE_DIR}/configs/odm/start-airplay2.sh"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/su" < "${BASE_DIR}/configs/odm/su"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/start-mediadaemon.sh" < "${BASE_DIR}/configs/odm/start-mediadaemon.sh"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/adbd-watchdog.sh" < "${BASE_DIR}/configs/odm/adbd-watchdog.sh"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/led_brightness" < "${BASE_DIR}/configs/odm/led_brightness"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/media-seek" < "${BASE_DIR}/configs/odm/media-seek"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/mediaseeker.dex" < "${BASE_DIR}/configs/odm/mediaseeker.dex"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/enforce-watchdog.sh" < "${BASE_DIR}/configs/odm/enforce-watchdog.sh"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/rootd.rc" < "${BASE_DIR}/configs/odm/rootd.rc"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/zz-adbd.rc" < "${BASE_DIR}/configs/odm/zz-adbd.rc"
        ssh -p 2222 "root@${STATION_IP}" "cat > /data/local/tmp/zz-permissive.rc" < "${BASE_DIR}/configs/odm/zz-permissive.rc"
        
        ssh -p 2222 "root@${STATION_IP}" '
            mount -o remount,rw /odm
            cp /data/local/tmp/fix-enforce.sh /odm/bin/fix-enforce.sh
            cp /data/local/tmp/start-dropbear.sh /odm/bin/start-dropbear.sh
            cp /data/local/tmp/start-airplay2.sh /odm/bin/start-airplay2.sh
            cp /data/local/tmp/su /odm/bin/su
            cp /data/local/tmp/start-mediadaemon.sh /odm/bin/start-mediadaemon.sh
            cp /data/local/tmp/adbd-watchdog.sh /odm/bin/adbd-watchdog.sh
            cp /data/local/tmp/led_brightness /odm/etc/led_brightness
            cp /data/local/tmp/media-seek /odm/bin/media-seek
            cp /data/local/tmp/mediaseeker.dex /odm/bin/mediaseeker.dex
            cp /data/local/tmp/enforce-watchdog.sh /odm/etc/init/enforce-watchdog.sh
            cp /data/local/tmp/rootd.rc /odm/etc/init/rootd.rc
            cp /data/local/tmp/zz-adbd.rc /odm/etc/init/zz-adbd.rc
            cp /data/local/tmp/zz-permissive.rc /odm/etc/init/zz-permissive.rc
            chmod 755 /odm/bin/* /odm/etc/init/*.sh
            chmod 644 /odm/bin/*.dex /odm/etc/init/*.rc /odm/etc/led_brightness
            chcon u:object_r:adbd_exec:s0 /odm/bin/adbd-watchdog.sh 2>/dev/null || true
            chcon u:object_r:vendor_configs_file:s0 /odm/etc/led_brightness 2>/dev/null || true
            sync
            mount -o remount,ro /odm

            # Ensure start-mediadaemon.sh and adbd-watchdog.sh are in /data/local/tmp/cmdin for boot
            if [ -f /data/local/tmp/cmdin ] && ! grep -q "start-mediadaemon.sh" /data/local/tmp/cmdin; then
                echo "/odm/bin/start-mediadaemon.sh &" >> /data/local/tmp/cmdin
            fi
            if [ -f /data/local/tmp/cmdin ] && ! grep -q "adbd-watchdog.sh" /data/local/tmp/cmdin; then
                echo "/odm/bin/adbd-watchdog.sh &" >> /data/local/tmp/cmdin
            fi
            /odm/bin/start-mediadaemon.sh
            for p in $(ps -A -o PID,ARGS | awk '\''$2 ~ /(^|\/)sh$/ && $3 == "/odm/bin/adbd-watchdog.sh" {print $1}'\''); do
                kill "$p" 2>/dev/null || true
            done
            nohup /odm/bin/adbd-watchdog.sh </dev/null >/dev/null 2>&1 &

            # Guarantee directory permissions on /data/dropbear/etc
            if [ -d /data/dropbear/etc ]; then
                find /data/dropbear/etc -type d -exec chmod 755 {} +
                find /data/dropbear/etc -type f -exec chmod 644 {} +
            fi
            echo "[+] Station ODM scripts and permissions successfully updated!"
        '
    elif ping -c 1 -W 1 "${STATION_IP}" >/dev/null 2>&1; then
        echo "[*] SSH port 2222 not reachable; using ADB (port 5555)..."
        adb connect "${STATION_IP}:5555" >/dev/null 2>&1 || true
        adb -s "${STATION_IP}:5555" push "${BASE_DIR}/configs/odm/fix-enforce.sh" /data/local/tmp/
        adb -s "${STATION_IP}:5555" push "${BASE_DIR}/configs/odm/start-dropbear.sh" /data/local/tmp/
        adb -s "${STATION_IP}:5555" push "${BASE_DIR}/configs/odm/start-airplay2.sh" /data/local/tmp/
        adb -s "${STATION_IP}:5555" push "${BASE_DIR}/configs/odm/su" /data/local/tmp/
        adb -s "${STATION_IP}:5555" push "${BASE_DIR}/configs/odm/adbd-watchdog.sh" /data/local/tmp/
        adb -s "${STATION_IP}:5555" push "${BASE_DIR}/configs/odm/led_brightness" /data/local/tmp/
        adb -s "${STATION_IP}:5555" push "${BASE_DIR}/configs/odm/enforce-watchdog.sh" /data/local/tmp/
        
        adb -s "${STATION_IP}:5555" shell '
            mount -o remount,rw /odm 2>/dev/null || true
            cp /data/local/tmp/fix-enforce.sh /odm/bin/fix-enforce.sh 2>/dev/null || true
            cp /data/local/tmp/start-dropbear.sh /odm/bin/start-dropbear.sh 2>/dev/null || true
            cp /data/local/tmp/start-airplay2.sh /odm/bin/start-airplay2.sh 2>/dev/null || true
            cp /data/local/tmp/su /odm/bin/su 2>/dev/null || true
            cp /data/local/tmp/adbd-watchdog.sh /odm/bin/adbd-watchdog.sh 2>/dev/null || true
            cp /data/local/tmp/led_brightness /odm/etc/led_brightness 2>/dev/null || true
            cp /data/local/tmp/enforce-watchdog.sh /odm/etc/init/enforce-watchdog.sh 2>/dev/null || true
            chmod 755 /odm/bin/* /odm/etc/init/*.sh 2>/dev/null || true
            chmod 644 /odm/bin/*.dex /odm/etc/init/*.rc /odm/etc/led_brightness 2>/dev/null || true
            chcon u:object_r:adbd_exec:s0 /odm/bin/adbd-watchdog.sh 2>/dev/null || true
            chcon u:object_r:vendor_configs_file:s0 /odm/etc/led_brightness 2>/dev/null || true
            sync
            mount -o remount,ro /odm 2>/dev/null || true
            if [ -f /data/local/tmp/cmdin ] && ! grep -q "adbd-watchdog.sh" /data/local/tmp/cmdin; then
                echo "/odm/bin/adbd-watchdog.sh &" >> /data/local/tmp/cmdin
            fi
            for p in $(ps -A -o PID,ARGS | awk '\''$2 ~ /(^|\/)sh$/ && $3 == "/odm/bin/adbd-watchdog.sh" {print $1}'\''); do
                kill "$p" 2>/dev/null || true
            done
            nohup /odm/bin/adbd-watchdog.sh </dev/null >/dev/null 2>&1 &
            if [ -d /data/dropbear/etc ]; then
                find /data/dropbear/etc -type d -exec chmod 755 {} + 2>/dev/null || true
                find /data/dropbear/etc -type f -exec chmod 644 {} + 2>/dev/null || true
            fi
        '
        echo -e "${C_GREEN}[+] Station scripts pushed via ADB!${C_RESET}"
    else
        echo -e "${C_YELLOW}[!] Warning: Station ${STATION_IP} is currently unreachable via network.${C_RESET}"
    fi
fi

# ==============================================================================
# 3. UPDATE TV APPS
# ==============================================================================
if [ "$UPDATE_APPS" = true ] && [ -n "${STATION_IP}" ]; then
    echo -e "${C_CYAN}[*] Updating applications on Station (${STATION_IP})...${C_RESET}"
    "${BASE_DIR}/scripts/04_install_apps.sh" "${STATION_IP}"
fi

# ==============================================================================
# 4. UPDATE CONFIGS
# ==============================================================================
if [ "$UPDATE_CONFIGS" = true ]; then
    echo -e "${C_CYAN}[*] Updating helper configs on server...${C_RESET}"
    if [ "$IS_LOCAL" = true ]; then
        sudo cp "${BASE_DIR}/scripts/station-stream.py" /usr/local/bin/station-stream
        sudo chmod +x /usr/local/bin/station-stream
    else
        scp "${BASE_DIR}/scripts/station-stream.py" "${TARGET_HOST}:/tmp/"
        ssh "${TARGET_HOST}" "sudo mv /tmp/station-stream.py /usr/local/bin/station-stream && sudo chmod +x /usr/local/bin/station-stream"
    fi
fi

# ==============================================================================
# 5. UPDATE TELEGRAM CONTROL BOT (station-bot)
# ==============================================================================
if [ "$UPDATE_BOT" = true ]; then
    echo -e "${C_CYAN}[*] Building & deploying station-bot on ${TARGET_HOST}...${C_RESET}"
    if [ "$IS_LOCAL" = true ]; then
        sudo mkdir -p /opt/station-bot/src /opt/quasar-bridge/bin
        sudo rm -f /opt/station-bot/src/*.go
        sudo cp "${BASE_DIR}/station-bot/go.mod" /opt/station-bot/src/
        sudo cp "${BASE_DIR}/station-bot/"[!.]*.go /opt/station-bot/src/
        sudo cp "${BASE_DIR}/configs/station-bot/bot.env.example" /opt/station-bot/bot.env.example
        if [ ! -f /opt/station-bot/bot.env ]; then
            sudo cp /opt/station-bot/bot.env.example /opt/station-bot/bot.env
            if [ -n "${STATION_IP}" ]; then
                sudo sed -i "s|^STATION_IP=.*|STATION_IP=${STATION_IP}|" /opt/station-bot/bot.env
            fi
            sudo chmod 600 /opt/station-bot/bot.env
        fi
        sudo cp "${BASE_DIR}/configs/station-bot/station-bot.service" /etc/systemd/system/station-bot.service
        sudo chmod 644 /etc/systemd/system/station-bot.service
        (cd /opt/station-bot/src && sudo go test ./... && sudo go build -o /opt/quasar-bridge/bin/station-bot.tmp . && sudo mv -f /opt/quasar-bridge/bin/station-bot.tmp /opt/quasar-bridge/bin/station-bot && sudo chmod 755 /opt/quasar-bridge/bin/station-bot)
        sudo systemctl daemon-reload
        sudo systemctl enable station-bot >/dev/null 2>&1 || true
        sudo systemctl restart station-bot || true
    else
        ssh "${TARGET_HOST}" "rm -rf /tmp/update_station_bot && mkdir -p /tmp/update_station_bot/src /tmp/update_station_bot/cfg"
        scp "${BASE_DIR}/station-bot/go.mod" "${BASE_DIR}/station-bot/"[!.]*.go "${TARGET_HOST}:/tmp/update_station_bot/src/"
        scp "${BASE_DIR}/configs/station-bot/bot.env.example" "${BASE_DIR}/configs/station-bot/station-bot.service" "${TARGET_HOST}:/tmp/update_station_bot/cfg/"

        ssh "${TARGET_HOST}" bash -s -- "${STATION_IP}" << 'REMOTE_BOT_EOF'
set -euo pipefail
ST_IP="${1:-}"
sudo mkdir -p /opt/station-bot/src /opt/quasar-bridge/bin
sudo rm -f /opt/station-bot/src/*.go
sudo cp /tmp/update_station_bot/src/go.mod /opt/station-bot/src/
sudo cp /tmp/update_station_bot/src/*.go /opt/station-bot/src/
sudo cp /tmp/update_station_bot/cfg/bot.env.example /opt/station-bot/bot.env.example
if [ ! -f /opt/station-bot/bot.env ]; then
    sudo cp /opt/station-bot/bot.env.example /opt/station-bot/bot.env
    if [ -n "$ST_IP" ]; then
        sudo sed -i "s|^STATION_IP=.*|STATION_IP=${ST_IP}|" /opt/station-bot/bot.env
    fi
    sudo chmod 600 /opt/station-bot/bot.env
fi
sudo cp /tmp/update_station_bot/cfg/station-bot.service /etc/systemd/system/station-bot.service
sudo chmod 644 /etc/systemd/system/station-bot.service

echo "[*] Running unit tests & building station-bot..."
cd /opt/station-bot/src
sudo go test ./...
sudo go build -o /opt/quasar-bridge/bin/station-bot.tmp .
sudo mv -f /opt/quasar-bridge/bin/station-bot.tmp /opt/quasar-bridge/bin/station-bot
sudo chmod 755 /opt/quasar-bridge/bin/station-bot

sudo systemctl daemon-reload
sudo systemctl enable station-bot >/dev/null 2>&1 || true
sudo systemctl restart station-bot || true

TOK="$(sudo grep -E '^TELEGRAM_BOT_TOKEN=' /opt/station-bot/bot.env | cut -d= -f2- | tr -d '[:space:]\"'\''')"
if [ -z "$TOK" ] || echo "$TOK" | grep -qi "PUT_YOUR"; then
    echo "[!] station-bot built & service installed. Set TELEGRAM_BOT_TOKEN and TELEGRAM_ALLOWED_USERS in /opt/station-bot/bot.env, then run: sudo systemctl restart station-bot"
elif systemctl is-active station-bot >/dev/null 2>&1; then
    echo "[+] station-bot successfully updated and running!"
else
    echo "[-] station-bot failed to stay active!"
    systemctl status station-bot --no-pager || true
fi
rm -rf /tmp/update_station_bot
REMOTE_BOT_EOF
    fi
fi

# ==============================================================================
# 6. UPDATE AIRPLAY 2 STACK
# ==============================================================================
if [ "$UPDATE_AIRPLAY" = true ] && [ -n "${STATION_IP}" ]; then
    echo -e "${C_CYAN}[*] Deploying AirPlay 2 stack to Station Max (${STATION_IP})...${C_RESET}"
    if python3 "${BASE_DIR}/airplay2/install.py" --model YNDX-00052 --root su --voice on --yes; then
        echo -e "${C_GREEN}[+] AirPlay 2 stack successfully deployed!${C_RESET}"
        python3 "${BASE_DIR}/airplay2/tools/airplayctl.py" status --root su --yes || true
    else
        echo -e "${C_RED}[-] Failed to deploy AirPlay 2 stack!${C_RESET}"
    fi
fi

echo ""
echo -e "${C_GREEN}=== Update completed successfully! ===${C_RESET}"

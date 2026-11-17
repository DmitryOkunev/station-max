#!/usr/bin/env bash
# Step 02: Safe Autonomous Downgrade to 5000000921
set -euo pipefail

SERVER_IP="${1:-}"
STATION_IP="${2:-}"

if [ -z "${SERVER_IP}" ]; then
    SERVER_IP=$(hostname -I 2>/dev/null | awk '{print $1}' || ip route get 1.1.1.1 2>/dev/null | awk '{print $7}' || echo "")
    if [ -z "${SERVER_IP}" ]; then
        read -rp "Enter server IP address (e.g. 192.168.1.1): " SERVER_IP
    fi
fi

if [ -z "${STATION_IP}" ]; then
    read -rp "Enter Station IP address (e.g. 192.168.1.50): " STATION_IP
fi
OTA_FILE="/opt/fakequasar/ota/5000000921.zip"

echo "=== [Step 02] Downgrade Yandex Station Max to build 5000000921 ==="

# 1. Check firmware file
if [ ! -f "${OTA_FILE}" ]; then
    echo "[-] Ошибка: Архив прошивки ${OTA_FILE} не найден!"
    echo "    Официальные бинарные образы прошивок Яндекса защищены авторским правом"
    echo "    и не распространяются в составе данного открытого репозитория."
    echo "    Пожалуйста, найдите файл 5000000921.zip самостоятельно (в профильных сообществах"
    echo "    энтузиастов 4PDA / Telegram) и поместите его в /opt/fakequasar/ota/5000000921.zip"
    exit 1
fi

SIZE=$(stat -c%s "${OTA_FILE}" 2>/dev/null || stat -f%z "${OTA_FILE}")
echo "[+] Firmware found: ${OTA_FILE} (${SIZE} bytes)"

# 2. Record initial Nginx log line positions (prevents matching historical downloads)
LOG_QUASAR="/var/log/nginx/quasar.yandex.net.access.log"
LOG_ACCESS="/var/log/nginx/access.log"

sudo touch "${LOG_QUASAR}" "${LOG_ACCESS}" 2>/dev/null || true
START_QUASAR=$(sudo wc -l < "${LOG_QUASAR}" 2>/dev/null || echo 0)
START_ACCESS=$(sudo wc -l < "${LOG_ACCESS}" 2>/dev/null || echo 0)

check_new_logs() {
    local pattern="$1"
    if [ -f "${LOG_QUASAR}" ]; then
        if sudo tail -n +"$((START_QUASAR + 1))" "${LOG_QUASAR}" 2>/dev/null | grep -E -q "${pattern}"; then
            return 0
        fi
    fi
    if [ -f "${LOG_ACCESS}" ]; then
        if sudo tail -n +"$((START_ACCESS + 1))" "${LOG_ACCESS}" 2>/dev/null | grep -E -q "${pattern}"; then
            return 0
        fi
    fi
    return 1
}

# Cleanup handler on abnormal termination
cleanup() {
    local exit_code=$?
    if [ "${exit_code}" -ne 0 ]; then
        echo -e "\n[!] Step 02 interrupted or failed. Cleaning up downgrade override..."
        sudo rm -f /etc/nginx/conf.d/downgrade_override.conf
        sudo nginx -t >/dev/null 2>&1 && sudo systemctl reload nginx >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# 3. Configure Nginx to serve the downgrade update
cat << EOF | sudo tee /etc/nginx/conf.d/downgrade_override.conf > /dev/null
# Downgrade override for /check_updates
server {
    listen 443 ssl;
    server_name quasar.yandex.net quasar.yandex.ru scbh.yandex.net;

    ssl_certificate     /opt/fakequasar/certs/fullchain_with_root.pem;
    ssl_certificate_key /opt/fakequasar/certs/quasar-all.key;
    ssl_protocols TLSv1.2 TLSv1.3;
    http2 off;

    access_log /var/log/nginx/quasar.yandex.net.access.log;
    error_log  /var/log/nginx/quasar.yandex.net.error.log;

    location = /check_updates {
        default_type application/json;
        add_header Cache-Control "no-store";
        return 200 '{"hasUpdate":true,"updateId":"150484","version":"5000000921","downloadUrl":"https://quasar.yandex.net/yandexstation_2/ota/5000000921.zip","fallbackUrl":"http://${SERVER_IP}/yandexstation_2/ota/5000000921.zip","fallbackIp":"${SERVER_IP}","size":${SIZE},"critical":true}';
    }

    location = /yandexstation_2/ota/5000000921.zip {
        alias /opt/fakequasar/ota/5000000921.zip;
    }
}
server {
    listen 80;
    server_name quasar.yandex.net quasar.yandex.ru scbh.yandex.net;

    access_log /var/log/nginx/quasar.yandex.net.access.log;

    location = /yandexstation_2/ota/5000000921.zip {
        alias /opt/fakequasar/ota/5000000921.zip;
    }
}
EOF

sudo nginx -t && sudo systemctl reload nginx
echo "[+] Nginx configured to offer 5000000921 downgrade!"

# 4. Trigger check_updates on station if ADB is currently reachable
if ping -c 1 -W 1 "${STATION_IP}" >/dev/null 2>&1; then
    adb connect "${STATION_IP}:5555" >/dev/null 2>&1 || true
    if adb -s "${STATION_IP}:5555" get-state 2>/dev/null | grep -q "device"; then
        echo "[+] Triggering updaterapp via ADB..."
        adb -s "${STATION_IP}:5555" shell "am startservice -a com.yandex.launcher.updaterapp.CHECK_UPDATES" || true
    fi
fi

echo "[*] Waiting for Station (${STATION_IP}) to check updates and download firmware..."
echo "[*] (Tip: If download doesn't begin within 1-2 minutes, power-cycle / reboot the speaker)"

SEEN_CHECK=0
DOWNLOAD_ACTIVE=0
DOWNLOAD_FINISHED=0

# Monitor for up to 15 minutes (300 iterations * 3s = 900s)
for i in $(seq 1 300); do
    # 1. Check if station polled check_updates
    if [ "${SEEN_CHECK}" -eq 0 ] && check_new_logs "check_updates"; then
        SEEN_CHECK=1
        echo "[+] Station requested update check (/check_updates HTTP 200)!"
    fi

    # 2. Check if station is actively downloading (file opened by web server)
    if sudo lsof "${OTA_FILE}" >/dev/null 2>&1 || sudo fuser "${OTA_FILE}" >/dev/null 2>&1; then
        if [ "${DOWNLOAD_ACTIVE}" -eq 0 ]; then
            DOWNLOAD_ACTIVE=1
            echo "[*] Download in progress: Nginx is transmitting ${OTA_FILE} to station..."
        fi
    fi

    # 3. Check if download completed (HTTP 200 or 206 logged for zip in new lines)
    if check_new_logs "5000000921\.zip.* (200|206) "; then
        echo "[+] Firmware download completed successfully (${SIZE} bytes transfer logged)!"
        DOWNLOAD_FINISHED=1
        break
    fi

    sleep 3
done

if [ "${DOWNLOAD_FINISHED}" -eq 0 ]; then
    echo "[-] Ошибка: Истекло время ожидания загрузки прошивки."
    echo "    Убедитесь, что колонка направляет DNS-запросы на ${SERVER_IP},"
    echo "    а также попробуйте перезагрузить колонку по питанию."
    exit 1
fi

echo ""
echo "[*] Firmware successfully delivered to Station Max!"
echo "[*] Station will now verify OTA signature, reboot to Recovery, flash partitions,"
echo "[*] and reboot into build 5000000921. This typically takes 2-4 minutes."
echo "[*] Waiting for station to reboot and come back online..."

# 5. Wait for station reboot cycle
REBOOTED=0
for i in $(seq 1 60); do
    if ! ping -c 1 -W 1 "${STATION_IP}" >/dev/null 2>&1; then
        echo "[+] Station went offline (rebooting into recovery)..."
        REBOOTED=1
        break
    fi
    sleep 2
done

if [ "${REBOOTED}" -eq 0 ]; then
    echo "[*] Note: Station did not drop ping yet, monitoring for boot completion..."
fi

echo "[*] Waiting for station to boot back up..."
BACK_ONLINE=0
for i in $(seq 1 120); do
    if ping -c 1 -W 1 "${STATION_IP}" >/dev/null 2>&1; then
        adb connect "${STATION_IP}:5555" >/dev/null 2>&1 || true
        if adb -s "${STATION_IP}:5555" get-state 2>/dev/null | grep -q "device"; then
            CURRENT_BUILD=$(adb -s "${STATION_IP}:5555" shell "getprop ro.build.display.id" 2>/dev/null | tr -d '\r\n' || echo "")
            echo "[+] Station is back online! Detected build: ${CURRENT_BUILD}"
            BACK_ONLINE=1
            break
        elif [ "${REBOOTED}" -eq 1 ]; then
            echo "[+] Station is back online (responding to ping)!"
            BACK_ONLINE=1
            break
        fi
    fi
    sleep 3
done

if [ "${BACK_ONLINE}" -eq 0 ]; then
    echo "[!] Station hasn't reconnected yet, but firmware delivery is complete."
    echo "    Check LED indicators on speaker and verify once boot finishes."
fi

# 6. Revert downgrade override now that download & flashing is complete
sudo rm -f /etc/nginx/conf.d/downgrade_override.conf
sudo nginx -t && sudo systemctl reload nginx
echo "[+] Downgrade override removed, OTA blocker restored."
echo "=== Step 02 Complete! Next step: run 03_exploit_root.sh ==="

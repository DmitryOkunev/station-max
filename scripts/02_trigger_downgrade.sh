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

# 2. Configure Nginx to serve the downgrade update
cat << EOF | sudo tee /etc/nginx/conf.d/downgrade_override.conf > /dev/null
# Downgrade override for /check_updates
server {
    listen 443 ssl;
    server_name quasar.yandex.net quasar.yandex.ru scbh.yandex.net;

    ssl_certificate     /opt/fakequasar/certs/fullchain_with_root.pem;
    ssl_certificate_key /opt/fakequasar/certs/quasar-all.key;
    ssl_protocols TLSv1.2 TLSv1.3;
    http2 off;

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

    location = /yandexstation_2/ota/5000000921.zip {
        alias /opt/fakequasar/ota/5000000921.zip;
    }
}
EOF

sudo nginx -t && sudo systemctl reload nginx
echo "[+] Nginx configured to offer 5000000921 downgrade!"

# 3. Trigger check_updates on station if ADB is currently reachable
if ping -c 1 -W 1 "${STATION_IP}" >/dev/null 2>&1; then
    adb connect "${STATION_IP}:5555" >/dev/null 2>&1 || true
    if adb -s "${STATION_IP}:5555" get-state 2>/dev/null | grep -q "device"; then
        echo "[+] Triggering updaterapp via ADB..."
        adb -s "${STATION_IP}:5555" shell "am startservice -a com.yandex.launcher.updaterapp.CHECK_UPDATES" || true
    fi
fi

echo "[*] Station should now detect update and begin downloading."
echo "[*] You can also reboot the speaker to trigger immediate update check."
echo "[*] Watching Nginx access log for OTA download..."

for i in $(seq 1 120); do
    if sudo grep -q "5000000921.zip.*200" /var/log/nginx/access.log /var/log/nginx/quasar.yandex.net.access.log 2>/dev/null; then
        echo "[+] Firmware download detected!"
        break
    fi
    sleep 3
done

echo "[*] Waiting for station to finish download, reboot to Recovery, and boot back into 5000000921..."
echo "[*] Once back online, run 03_exploit_root.sh to obtain root access."

# Revert downgrade override to avoid bootloops after update finishes
sudo rm -f /etc/nginx/conf.d/downgrade_override.conf
sudo nginx -t && sudo systemctl reload nginx
echo "[+] Downgrade override removed, OTA blocker restored."

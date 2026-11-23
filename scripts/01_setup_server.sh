#!/usr/bin/env bash
# Step 01: Setup Server / Router (Fakequasar Nginx + DNSMasq)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

SERVER_IP="${1:-}"
if [ -z "${SERVER_IP}" ]; then
    SERVER_IP=$(hostname -I 2>/dev/null | awk '{print $1}' || ip route get 1.1.1.1 2>/dev/null | awk '{print $7}' || echo "")
    if [ -z "${SERVER_IP}" ]; then
        read -rp "Enter server IP address (e.g. 192.168.1.1): " SERVER_IP
    fi
fi
echo "=== [Step 01] Setting up Server / Router Fakequasar (IP: ${SERVER_IP}) ==="

# 1. Install prerequisites if running on Debian/Ubuntu
if command -v apt-get >/dev/null 2>&1; then
    sudo apt-get update
    sudo apt-get install -y nginx dnsmasq curl openssl python3
fi

# 2. Setup /opt/fakequasar directories and certificates
sudo mkdir -p /opt/fakequasar/certs /opt/fakequasar/ota /var/log/nginx

if [ ! -f "${BASE_DIR}/certs/fullchain_with_root.pem" ] || [ ! -f "${BASE_DIR}/certs/quasar-all.key" ]; then
    echo "[-] Ошибка: Файлы сертификатов не найдены в ${BASE_DIR}/certs/!"
    echo "    В репозитории они не распространяются (авторские права и безопасность)."
    echo "    Пожалуйста, найдите или сгенерируйте и поместите в ${BASE_DIR}/certs/ следующие файлы:"
    echo "      - fullchain_with_root.pem (цепочка сертификатов *.quasar.yandex.net)"
    echo "      - quasar-all.key (приватный ключ)"
    echo "    Подробнее см. certs/README.md"
    exit 1
fi

sudo cp "${BASE_DIR}/certs/fullchain_with_root.pem" /opt/fakequasar/certs/
sudo cp "${BASE_DIR}/certs/quasar-all.key" /opt/fakequasar/certs/
sudo chmod 600 /opt/fakequasar/certs/quasar-all.key
sudo chmod 644 /opt/fakequasar/certs/fullchain_with_root.pem

# 3. Setup real_sync_info.json with active adbd and uniProxyUrl
sudo cp "${BASE_DIR}/configs/nginx/real_sync_info.json" /opt/fakequasar/
sudo python3 -c "
import json
path = '/opt/fakequasar/real_sync_info.json'
with open(path) as f:
    d = json.load(f)
d['config']['system_config']['adbd'] = {'remoteActivation': True, 'network': True, 'fullDurationMs': 31536000000}
d['adbd'] = {'remoteActivation': True, 'network': True, 'fullDurationMs': 31536000000}
d['config']['system_config']['uniProxyUrl'] = 'ws://${SERVER_IP}:8443/uni.ws'
with open(path, 'w') as f:
    json.dump(d, f)
print('real_sync_info.json successfully configured with IP: ${SERVER_IP}')
"

# 4. Deploy Nginx configuration
sudo cp "${BASE_DIR}/configs/nginx/quasar.conf" /etc/nginx/sites-available/quasar
sudo ln -sf /etc/nginx/sites-available/quasar /etc/nginx/sites-enabled/quasar
sudo nginx -t
sudo systemctl reload nginx || sudo systemctl restart nginx

# 5. Deploy DNSMasq configuration
sudo cp "${BASE_DIR}/configs/dnsmasq/station_max.conf" /etc/dnsmasq.d/station_max.conf
sudo sed -i "s|__SERVER_IP__|${SERVER_IP}|g" /etc/dnsmasq.d/station_max.conf
sudo systemctl restart dnsmasq

echo "=== [Step 01] Server setup complete! Testing endpoints... ==="
curl -k -s -o /dev/null -w "  generate_204: HTTP %{http_code}\n" https://127.0.0.1/generate_204 -H "Host: quasar.yandex.net"
curl -k -s -o /dev/null -w "  check_updates: HTTP %{http_code}\n" https://127.0.0.1/check_updates -H "Host: quasar.yandex.net"
curl -k -s -o /dev/null -w "  get_sync_info: HTTP %{http_code}\n" https://127.0.0.1/get_sync_info -H "Host: quasar.yandex.net"
echo "=== Ready for next step! ==="

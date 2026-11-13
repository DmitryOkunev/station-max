#!/usr/bin/env bash
# Step 04: Install & Configure TV Applications (SmartTube, Zona, LazyMedia)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
APPS_DIR="${BASE_DIR}/apps"

STATION_IP="${1:-}"
if [ -z "${STATION_IP}" ]; then
    read -rp "Enter Station IP address (e.g. 192.168.1.50): " STATION_IP
fi
echo "=== [Step 04] Installing TV Applications on Yandex Station Max (${STATION_IP}) ==="

# 1. Connect via ADB
echo "[*] Connecting to ADB (${STATION_IP}:5555)..."
adb connect "${STATION_IP}:5555" || true
adb -s "${STATION_IP}:5555" wait-for-device

# 2. Verify and fix directory permissions to prevent Gray Screen / Permission crashes
echo "[*] Ensuring /system/etc and /data/dropbear/etc directory permissions are 755..."
adb -s "${STATION_IP}:5555" shell '
if [ -d /data/dropbear/etc ]; then
    find /data/dropbear/etc -type d -exec chmod 755 {} + 2>/dev/null || true
    find /data/dropbear/etc -type f -exec chmod 644 {} + 2>/dev/null || true
fi
'

# 3. Install SmartTube (Release has priority over Beta if both present)
ST_APK=""
for pattern in \
    "${APPS_DIR}"/*SmartTube*release*.apk \
    "${APPS_DIR}"/*SmartTube*stable*.apk \
    "${APPS_DIR}"/*smarttube*release*.apk \
    "${APPS_DIR}"/*smarttube*stable*.apk \
    "${APPS_DIR}"/*SmartTube*beta*.apk \
    "${APPS_DIR}"/*smarttube*beta*.apk \
    "${APPS_DIR}"/SmartTube*.apk \
    "${APPS_DIR}"/smarttube*.apk; do
    for f in $pattern; do
        if [ -f "$f" ]; then
            ST_APK="$f"
            break 2
        fi
    done
done

if [ -n "${ST_APK}" ] && [ -f "${ST_APK}" ]; then
    echo "[*] Installing SmartTube ($(basename "${ST_APK}"))..."
    adb -s "${STATION_IP}:5555" install -r -g -d "${ST_APK}"
    echo "[+] SmartTube installed."
else
    echo "[-] Warning: No SmartTube APK found in ${APPS_DIR}!"
fi

# 4. Install Zona
ZONA_APK="${APPS_DIR}/zona_3.0.77.apk"
if [ -f "${ZONA_APK}" ]; then
    echo "[*] Installing Zona TV..."
    adb -s "${STATION_IP}:5555" install -r -g -d "${ZONA_APK}"
    echo "[+] Zona installed."
else
    echo "[-] Warning: ${ZONA_APK} not found!"
fi

# 5. Install LazyMedia Deluxe
LMD_APK="${APPS_DIR}/lazycatsoftware.lmd-3.470.apk"
if [ -f "${LMD_APK}" ]; then
    echo "[*] Installing LazyMedia Deluxe..."
    adb -s "${STATION_IP}:5555" install -r -g -d "${LMD_APK}"
    echo "[+] LazyMedia Deluxe installed."
else
    echo "[-] Warning: ${LMD_APK} not found!"
fi

# 6. Grant runtime permissions and disable battery optimizations
echo "[*] Granting app permissions..."
adb -s "${STATION_IP}:5555" shell '
# SmartTube permissions (all variants: stable, release, beta, legacy)
for pkg in org.smarttube.stable com.teamsmart.videomanager.tv org.smarttube.beta com.liskovsoft.smarttubetv.beta; do
    pm grant $pkg android.permission.RECORD_AUDIO 2>/dev/null || true
    pm grant $pkg android.permission.WRITE_EXTERNAL_STORAGE 2>/dev/null || true
    pm grant $pkg android.permission.READ_EXTERNAL_STORAGE 2>/dev/null || true
    appops set $pkg SYSTEM_ALERT_WINDOW allow 2>/dev/null || true
    dumpsys deviceidle whitelist +$pkg 2>/dev/null || true
done

# Zona permissions
pm grant mobi.zona android.permission.WRITE_EXTERNAL_STORAGE 2>/dev/null || true
pm grant mobi.zona android.permission.READ_EXTERNAL_STORAGE 2>/dev/null || true
pm grant mobi.zona android.permission.ACCESS_FINE_LOCATION 2>/dev/null || true
pm grant mobi.zona android.permission.ACCESS_COARSE_LOCATION 2>/dev/null || true
dumpsys deviceidle whitelist +mobi.zona 2>/dev/null || true

# LazyMedia permissions
pm grant com.lazycatsoftware.lmd android.permission.WRITE_EXTERNAL_STORAGE 2>/dev/null || true
pm grant com.lazycatsoftware.lmd android.permission.READ_EXTERNAL_STORAGE 2>/dev/null || true
dumpsys deviceidle whitelist +com.lazycatsoftware.lmd 2>/dev/null || true
'

# 7. Verification
echo "[*] Verifying installed packages:"
adb -s "${STATION_IP}:5555" shell "pm list packages | grep -E 'org.smarttube|mobi.zona|com.lazycatsoftware.lmd'"

echo "=== [Step 04] Application installation and configuration complete! ==="

#!/system/bin/sh
# Persistent AirPlay 2 startup script for Yandex Station Max
# Waits for SELinux Permissive mode and boot completion before starting supervisor

# 1. Wait for SELinux Permissive (up to 45s)
for i in $(seq 1 45); do
    if [ "$(cat /sys/fs/selinux/enforce 2>/dev/null)" = "0" ]; then
        break
    fi
    sleep 1
done

# 2. Wait for Android framework boot to settle
for i in $(seq 1 30); do
    if [ "$(getprop sys.boot_completed 2>/dev/null)" = "1" ]; then
        break
    fi
    sleep 1
done

# 3. Launch AirPlay 2 stack if installed
AIRPLAY_DIR="/data/local/tmp/ya-airplay"
if [ -f "$AIRPLAY_DIR/start.sh" ]; then
    mkdir -p "$AIRPLAY_DIR/run"
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] Starting AirPlay 2 stack via ODM autostart..." >> "$AIRPLAY_DIR/run/autostart.log"
    /system/bin/sh "$AIRPLAY_DIR/start.sh" >> "$AIRPLAY_DIR/run/autostart.log" 2>&1 &
fi

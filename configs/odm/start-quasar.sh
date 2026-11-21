#!/system/bin/sh
# Persistent Quasar Bridge startup script for Yandex Station Max (Standalone On-Device)
# Waits for SELinux Permissive mode, framework boot, and network before starting

# 1. Wait for SELinux Permissive (up to 45s)
for i in $(seq 1 45); do
    if [ "$(cat /sys/fs/selinux/enforce 2>/dev/null)" = "0" ]; then
        break
    fi
    sleep 1
done

# 2. Wait for Android framework boot to complete
for i in $(seq 1 30); do
    if [ "$(getprop sys.boot_completed 2>/dev/null)" = "1" ]; then
        break
    fi
    sleep 1
done

# 3. Ensure MediaDaemon is running on port 4545
if [ -x /odm/bin/start-mediadaemon.sh ]; then
    /odm/bin/start-mediadaemon.sh
fi

QUASAR_DIR="/data/quasar-bridge"
QUASAR_BIN="/odm/bin/quasar-bridge"

# 4. Check if standalone quasar-bridge is enabled
if [ -f "$QUASAR_DIR/enabled" ] && [ -x "$QUASAR_BIN" ]; then
    mkdir -p "$QUASAR_DIR"
    if ! pgrep -x quasar-bridge >/dev/null 2>&1; then
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] Starting Standalone Quasar Bridge..." >> "$QUASAR_DIR/autostart.log"
        if [ -f "$QUASAR_DIR/runtime.env" ]; then
            set -a
            . "$QUASAR_DIR/runtime.env"
            set +a
        fi
        cd "$QUASAR_DIR"
        "$QUASAR_BIN" -config "$QUASAR_DIR/config.json" </dev/null >> "$QUASAR_DIR/bridge.log" 2>&1 &
    fi
fi

#!/system/bin/sh
# Persistent Watchdog for ADB TCP Port 5555, SELinux Permissive, MediaDaemon,
# and initial front LED display brightness default (40 / 200)
# Runs continuously in background as root

FB4_BRIGHTNESS="/sys/class/backlight/fb4/brightness"
DEFAULT_LED_BRIGHTNESS=40

get_default_led_brightness() {
    VAL="$DEFAULT_LED_BRIGHTNESS"
    if [ -f /odm/etc/led_brightness ]; then
        V="$(tr -cd '0-9' < /odm/etc/led_brightness 2>/dev/null)"
        [ -n "$V" ] && VAL="$V"
    fi
    if [ -f /data/local/tmp/led_brightness ]; then
        V="$(tr -cd '0-9' < /data/local/tmp/led_brightness 2>/dev/null)"
        [ -n "$V" ] && VAL="$V"
    fi
    if [ "$VAL" -lt 0 ] 2>/dev/null; then VAL=0; fi
    if [ "$VAL" -gt 200 ] 2>/dev/null; then VAL=200; fi
    echo "$VAL"
}

apply_default_led_brightness() {
    [ -w "$FB4_BRIGHTNESS" ] || return 0
    TARGET_BR="$(get_default_led_brightness)"
    echo "$TARGET_BR" > "$FB4_BRIGHTNESS" 2>/dev/null
}

# 1. Wait for SELinux Permissive (up to 60s)
for i in $(seq 1 60); do
    if [ "$(cat /sys/fs/selinux/enforce 2>/dev/null)" = "0" ]; then
        break
    fi
    sleep 1
done

# Initial guarantee
settings put global adb_enabled 1 2>/dev/null
setprop service.adb.tcp.port 5555
apply_default_led_brightness

LAST_SDK_PID=""
LED_REAPPLY_TICKS=0

while true; do
    # Guarantee SELinux Permissive
    if [ "$(cat /sys/fs/selinux/enforce 2>/dev/null)" != "0" ]; then
        echo 0 > /sys/fs/selinux/enforce 2>/dev/null
        /vendor/bin/setenforce 0 2>/dev/null
    fi

    # Guarantee adb_enabled in global settings
    if [ "$(settings get global adb_enabled 2>/dev/null)" != "1" ]; then
        settings put global adb_enabled 1 2>/dev/null
    fi

    # Guarantee property is 5555
    if [ "$(getprop service.adb.tcp.port 2>/dev/null)" != "5555" ]; then
        setprop service.adb.tcp.port 5555
    fi

    # Check if adbd is actively listening on TCP port 5555 (15B3 in hex, state 0A = LISTEN)
    if ! grep -qi ":15B3 .* 0A" /proc/net/tcp6 2>/dev/null && ! grep -qi ":15B3 .* 0A" /proc/net/tcp 2>/dev/null; then
        echo "adbd-watchdog: port 5555 not listening, restarting adbd" >> /data/local/tmp/rootd.log
        setprop service.adb.tcp.port 5555
        pkill -9 -f /system/bin/adbd 2>/dev/null
        sleep 1
        if ! pgrep -f /system/bin/adbd >/dev/null 2>&1; then
            /system/bin/adbd --root_seclabel=u:r:su:s0 >/dev/null 2>&1 &
        fi
    fi

    # Guarantee MediaDaemon is running on port 4545
    if ! pgrep -f MediaDaemon >/dev/null 2>&1; then
        if command -v toybox >/dev/null 2>&1; then
            toybox setsid app_process -Djava.class.path=/odm/bin/mediaseeker.dex /odm/bin MediaDaemon </dev/null >/data/local/tmp/mediadaemon.log 2>&1 &
        else
            setsid app_process -Djava.class.path=/odm/bin/mediaseeker.dex /odm/bin MediaDaemon </dev/null >/data/local/tmp/mediadaemon.log 2>&1 &
        fi
    fi

    # Apply default front LED display brightness once per com.yandex.io.sdk start/restart
    # (+10s/+20s ticks cover LightSensor startup sync), allowing ALS auto-brightness afterward.
    CUR_SDK_PID="$(pidof com.yandex.io.sdk 2>/dev/null | awk '{print $1}')"
    if [ -n "$CUR_SDK_PID" ] && [ "$CUR_SDK_PID" != "$LAST_SDK_PID" ]; then
        LAST_SDK_PID="$CUR_SDK_PID"
        apply_default_led_brightness
        LED_REAPPLY_TICKS=2
    elif [ "$LED_REAPPLY_TICKS" -gt 0 ]; then
        LED_REAPPLY_TICKS=$((LED_REAPPLY_TICKS - 1))
        apply_default_led_brightness
    fi

    sleep 10
done

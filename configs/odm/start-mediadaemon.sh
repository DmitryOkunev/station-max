#!/system/bin/sh
# Start MediaDaemon on port 4545 if not already running
if ! pgrep -f MediaDaemon >/dev/null 2>&1; then
    if command -v toybox >/dev/null 2>&1; then
        toybox setsid app_process -Djava.class.path=/odm/bin/mediaseeker.dex /odm/bin MediaDaemon </dev/null >/data/local/tmp/mediadaemon.log 2>&1 &
    else
        setsid app_process -Djava.class.path=/odm/bin/mediaseeker.dex /odm/bin MediaDaemon </dev/null >/data/local/tmp/mediadaemon.log 2>&1 &
    fi
fi

#!/usr/bin/env python3
"""
station-stream: Switch Yandex Station Max voice stream between Quasar Bridge and official Yandex cloud.
Usage:
  station-stream status [STATION_IP]
  station-stream bridge [STATION_IP] [SERVER_IP]
  station-stream yandex [STATION_IP]
"""
import sys
import os
import subprocess
import json
import re
import time

ARGS = [a for a in sys.argv[1:] if not a.startswith("--")]

MODE = ARGS[0].lower() if len(ARGS) > 0 else "status"

# Station IP from argument, config, or prompt
STATION_IP = ARGS[1] if len(ARGS) > 1 else os.environ.get("STATION_IP", "")
if not STATION_IP and MODE in ["bridge", "yandex", "status"]:
    conf_path = "/etc/station-stream.conf"
    if os.path.exists(conf_path):
        try:
            with open(conf_path) as f:
                c = json.load(f)
                STATION_IP = c.get("station_ip", "")
        except Exception:
            pass

if not STATION_IP:
    if sys.stdin.isatty():
        STATION_IP = input("Enter Station IP address: ").strip()
    else:
        STATION_IP = "192.168.1.50"

# Server IP from argument, or detect route to station
SERVER_IP = ARGS[2] if len(ARGS) > 2 else os.environ.get("SERVER_IP", "")
if not SERVER_IP:
    try:
        # Get specific interface IP that routes to the Station (never WAN!)
        out = subprocess.check_output(f"ip route get {STATION_IP} 2>/dev/null", shell=True).decode()
        m = re.search(r"src\s+([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)", out)
        if m:
            SERVER_IP = m.group(1)
        else:
            SERVER_IP = "192.168.1.1"
    except Exception:
        SERVER_IP = "192.168.1.1"

BRIDGE_URL = f"ws://{SERVER_IP}:8443/uni.ws"
YANDEX_URL = "wss://uniproxy.alice.yandex.net/uni.ws"
QUIC_URL = "uniproxy-quic.alice.yandex.net:8443/uni.ws"

SYS_DIR = "/data/data/com.yandex.io.sdk/files/system"
DAT_PATH = "/data/data/com.yandex.io.sdk/files/data/data/user_config.dat"

def run_ssh(cmd):
    return subprocess.check_output([
        "ssh", "-o", "StrictHostKeyChecking=no", "-p", "2222", f"root@{STATION_IP}", cmd
    ]).decode(errors="ignore")

def set_stream(target_url, quic_url):
    print(f">>> Switching stream on {STATION_IP} to: {target_url}")
    
    # 0. Ensure no immutable flag is blocking writes
    try:
        run_ssh(f"toybox chattr -i {SYS_DIR}/quasar.cfg {SYS_DIR}/yandexstation_2_overlay.cfg {DAT_PATH} 2>/dev/null")
    except Exception:
        pass

    # 1. Update quasar.cfg and yandexstation_2_overlay.cfg
    for cfg in ["quasar.cfg", "yandexstation_2_overlay.cfg"]:
        remote = f"{SYS_DIR}/{cfg}"
        raw = run_ssh(f"cat {remote} 2>/dev/null")
        if raw.strip():
            d = json.loads(raw)
            if "aliced" in d:
                d["aliced"]["uniProxyUrl"] = target_url
                d["aliced"]["uniProxyQuicUrl"] = quic_url
                new_json = json.dumps(d, ensure_ascii=False, indent=3)
                p = subprocess.Popen([
                    "ssh", "-o", "StrictHostKeyChecking=no", "-p", "2222", f"root@{STATION_IP}",
                    f"cat > {remote} && chown u0_a39:u0_a39 {remote} && chmod 600 {remote}"
                ], stdin=subprocess.PIPE)
                p.communicate(new_json.encode())
                print(f"  [+] {cfg} updated")

    # 2. Patch user_config.dat
    raw_dat = subprocess.check_output([
        "ssh", "-o", "StrictHostKeyChecking=no", "-p", "2222", f"root@{STATION_IP}", f"cat {DAT_PATH}"
    ])
    
    # Fixed slot size is 55 bytes
    target_stock = b'"uniProxyUrl":"wss://uniproxy.alice.yandex.net/uni.ws"'
    
    # Match any current uniProxyUrl:... in dat file
    match = re.search(rb'"uniProxyUrl":\s*"[^"]*"\s*', raw_dat)
    if match:
        old_span = match.span()
        slot_len = old_span[1] - old_span[0]
        if target_url == BRIDGE_URL:
            new_val = f'"uniProxyUrl":"{target_url}"'.encode()
            replacement = new_val.ljust(slot_len, b' ')
        else:
            replacement = target_stock.ljust(slot_len, b' ')
            
        patched = raw_dat[:old_span[0]] + replacement + raw_dat[old_span[1]:]
        p = subprocess.Popen([
            "ssh", "-o", "StrictHostKeyChecking=no", "-p", "2222", f"root@{STATION_IP}",
            f"cat > {DAT_PATH} && chown u0_a39:u0_a39 {DAT_PATH} && chmod 600 {DAT_PATH}"
        ], stdin=subprocess.PIPE)
        p.communicate(patched)
        print("  [+] user_config.dat patched successfully")
    else:
        print("  [-] Could not find uniProxyUrl pattern in user_config.dat")

    # 3. Restart SDK
    print("  [+] Restarting com.yandex.io.sdk...")
    try:
        run_ssh("kill -9 $(pidof com.yandex.io.sdk) 2>/dev/null")
    except Exception:
        pass
    
    pid = ""
    for _ in range(10):
        time.sleep(1)
        try:
            out = run_ssh("pidof com.yandex.io.sdk 2>/dev/null").strip()
            if out:
                pid = out
                break
        except Exception:
            pass
    print(f"  [+] com.yandex.io.sdk running with PID: {pid if pid else 'starting...'}")

def show_status():
    print(f"=== Current Voice Stream Status ({STATION_IP}) ===")
    out = run_ssh(f"grep -o '\"uniProxyUrl\"[ ]*:[ ]*\"[^\"]*\"' {SYS_DIR}/quasar.cfg; grep -o '\"uniProxyQuicUrl\"[ ]*:[ ]*\"[^\"]*\"' {SYS_DIR}/quasar.cfg; grep -o '\"uniProxyUrl\":[^,}}]*' {DAT_PATH}")
    print(out.strip())
    try:
        pid = run_ssh("pidof com.yandex.io.sdk 2>/dev/null").strip()
    except Exception:
        pid = ""
    print(f"SDK PID: {pid if pid else 'not running'}")

if MODE == "bridge":
    set_stream(BRIDGE_URL, "")
    show_status()
elif MODE in ["yandex", "original", "stock"]:
    set_stream(YANDEX_URL, QUIC_URL)
    show_status()
elif MODE == "status":
    show_status()
else:
    print("Usage: station-stream [status|bridge|yandex] [STATION_IP] [SERVER_IP]")

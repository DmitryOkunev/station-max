#!/usr/bin/env python3
import sys
import json
import subprocess
import urllib.parse
import re

import os

STATION_ADB = os.environ.get("STATION_ADB", "127.0.0.1:5555")
if os.path.exists("/opt/quasar-bridge/config.json"):
    try:
        with open("/opt/quasar-bridge/config.json") as f:
            cfg = json.load(f)
            STATION_ADB = cfg.get("smarttube", {}).get("adb_address", STATION_ADB)
    except Exception:
        pass
_detected_package = None

def get_package():
    global _detected_package
    if _detected_package:
        return _detected_package

    if os.path.exists("/opt/quasar-bridge/config.json"):
        try:
            with open("/opt/quasar-bridge/config.json") as f:
                cfg = json.load(f)
                pkg = cfg.get("smarttube", {}).get("package")
                if pkg and pkg not in ("org.smarttube.beta", "org.smarttube.stable"):
                    _detected_package = pkg
                    return _detected_package
        except Exception:
            pass

    # Probe packages on device with priority: Release/Stable -> Legacy Release -> Beta
    ok, out = run_adb("pm list packages smarttube; pm list packages teamsmart", timeout=3)
    if ok:
        for candidate in ("org.smarttube.stable", "com.teamsmart.videomanager.tv", "org.smarttube.beta", "com.liskovsoft.smarttubetv.beta"):
            if candidate in out:
                _detected_package = candidate
                return _detected_package

    _detected_package = "org.smarttube.stable"
    return _detected_package

def get_splash_activity():
    pkg = get_package()
    return f"{pkg}/com.liskovsoft.smartyoutubetv2.tv.ui.main.SplashActivity"

def run_adb(shell_cmd, timeout=5):
    try:
        res = subprocess.run(
            ["adb", "-s", STATION_ADB, "shell", shell_cmd],
            capture_output=True,
            text=True,
            timeout=timeout
        )
        return res.returncode == 0, res.stdout.strip()
    except Exception as e:
        return False, str(e)

def handle_play(query, add_to_queue=False):
    if not query:
        return "Ошибка: не указан поисковый запрос."
    # Wake up device if sleeping
    run_adb("input keyevent 224", timeout=2)
    
    encoded_query = urllib.parse.quote_plus(query)
    url = f"youtube://search?query={encoded_query}&method=play"
    
    splash = get_splash_activity()
    extra = " --ez add_to_queue true" if add_to_queue else ""
    cmd = f"am start -a android.intent.action.VIEW -d '{url}' -n {splash}{extra}"
    ok, out = run_adb(cmd, timeout=5)
    if ok:
        if add_to_queue:
            return f"Добавила «{query}» в очередь воспроизведения на YouTube."
        return f"Включаю «{query}» на YouTube."
    return f"Не удалось запустить видео: {out}"

def handle_control(action, seconds=15):
    if action == "pause":
        run_adb("input keyevent 127")
        return "Поставила воспроизведение на паузу."
    elif action in ("resume", "play"):
        run_adb("input keyevent 126")
        return "Продолжаю воспроизведение."
    elif action == "next":
        run_adb("input keyevent 87")
        return "Переключила на следующее видео."
    elif action in ("prev", "previous"):
        run_adb("input keyevent 88")
        return "Переключила на предыдущее видео."
    elif action == "fast_forward":
        run_adb("input keyevent 90")
        return f"Перемотала вперед на {seconds} секунд."
    elif action == "rewind":
        run_adb("input keyevent 89")
        return f"Перемотала назад на {seconds} секунд."
    elif action in ("stop", "home", "close"):
        run_adb("input keyevent 127")
        run_adb("input keyevent 3")
        return "Остановила YouTube и вернулась на главный экран."
    elif action == "status":
        ok, out = run_adb("dumpsys media_session")
        if not ok:
            return "Не удалось получить статус медиасессии."
        if PACKAGE not in out:
            return "SmartTube сейчас не запущен."
        
        # Parse current metadata
        desc_match = re.search(r'metadata:size=\d+,\s*description=([^,\n]+)', out)
        desc = desc_match.group(1).strip() if desc_match else "неизвестное видео"
        
        state_match = re.search(r'state=PlaybackState\s*\{state=(\d+)', out)
        state_num = state_match.group(1) if state_match else "0"
        
        status_text = "воспроизводится" if state_num == "3" else ("на паузе" if state_num == "2" else "загружается")
        return f"Сейчас {status_text}: «{desc}»."
    else:
        return f"Неизвестная команда: {action}"

TOOLS = [
    {
        "name": "smarttube_play",
        "description": "Найти и запустить видео, песню, исполнителя или альбом в SmartTube (YouTube без рекламы) на Яндекс Станции. Всегда вызывай этот инструмент, когда пользователь просит включить что-либо на Ютубе / в YouTube / SmartTube.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {
                    "type": "string",
                    "description": "Поисковый запрос или название видео/трека/канала."
                },
                "add_to_queue": {
                    "type": "boolean",
                    "description": "Добавить в очередь воспроизведения вместо прерывания текущего видео."
                }
            },
            "required": ["query"]
        }
    },
    {
        "name": "smarttube_control",
        "description": "Управление воспроизведением в SmartTube (пауза, продолжить, следующее видео, предыдущее, перемотка, остановка, статус). Вызывай, когда пользователь просит управлять просмотром YouTube.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["pause", "resume", "next", "previous", "rewind", "fast_forward", "stop", "status"],
                    "description": "Действие: pause (пауза), resume (продолжить), next (следующее), previous (предыдущее), rewind (перемотка назад), fast_forward (вперед), stop (остановить), status (текущее видео)."
                },
                "seconds": {
                    "type": "integer",
                    "description": "Секунды для перемотки (по умолчанию 15)."
                }
            },
            "required": ["action"]
        }
    }
]

def main():
    while True:
        line = sys.stdin.readline()
        if not line:
            break
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except Exception:
            continue

        method = req.get("method")
        msg_id = req.get("id")

        if method == "initialize":
            resp = {
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {
                    "protocolVersion": "2024-11-05",
                    "capabilities": {
                        "tools": {}
                    },
                    "serverInfo": {
                        "name": "smarttube-bridge",
                        "version": "1.0.0"
                    }
                }
            }
            sys.stdout.write(json.dumps(resp) + "\n")
            sys.stdout.flush()

        elif method == "notifications/initialized":
            # Client notification, no reply needed
            pass

        elif method == "tools/list":
            resp = {
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {
                    "tools": TOOLS
                }
            }
            sys.stdout.write(json.dumps(resp) + "\n")
            sys.stdout.flush()

        elif method == "tools/call":
            params = req.get("params", {})
            tool_name = params.get("name")
            args = params.get("arguments", {})

            result_text = ""
            if tool_name == "smarttube_play":
                query = args.get("query", "")
                add_to_queue = args.get("add_to_queue", False)
                result_text = handle_play(query, add_to_queue)
            elif tool_name == "smarttube_control":
                action = args.get("action", "")
                seconds = args.get("seconds", 15)
                result_text = handle_control(action, seconds)
            else:
                result_text = f"Неизвестный инструмент: {tool_name}"

            resp = {
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {
                    "content": [
                        {
                            "type": "text",
                            "text": result_text
                        }
                    ]
                }
            }
            sys.stdout.write(json.dumps(resp) + "\n")
            sys.stdout.flush()

        elif msg_id is not None:
            resp = {
                "jsonrpc": "2.0",
                "id": msg_id,
                "error": {
                    "code": -32601,
                    "message": f"Method not found: {method}"
                }
            }
            sys.stdout.write(json.dumps(resp) + "\n")
            sys.stdout.flush()

if __name__ == "__main__":
    main()

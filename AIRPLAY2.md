# Apple AirPlay 2 Native Architecture & Engineering Manual
## Yandex Station Max (YNDX-0008 / YNDX-00052)

This document provides the complete architectural, operational, and development guide for the autonomous **Apple AirPlay 2** stack running on Yandex Station Max.

---

## 1. Architectural Overview

The AirPlay 2 subsystem is a fully self-contained, native ARM64 multi-room audio receiver integrated directly into the Station Max firmware without external cloud dependencies.

```
                    ┌──────────────────────────────────────────────┐
                    │               Apple Source                   │
                    │        (iPhone / iPad / Mac / Apple TV)       │
                    └──────────────────────┬───────────────────────┘
                                           │
                 AirPlay 2 Protocol (RTSP :7000, PTP :319/:320, UDP RTP)
                                           │
  ┌────────────────────────────────────────┴────────────────────────────────────────┐
  │ Yandex Station Max (Android TV 9 / API 28 / Linux 4.9.113)                     │
  │                                                                                │
  │  ┌────────────────────────┐                   ┌──────────────────────────────┐  │
  │  │   ya.station.airplay   │                   │           ya-nqptp           │  │
  │  │ (Android NSD Service)  │                   │      (IEEE 1588 PTP)         │  │
  │  │  Advertises mDNS:      │                   │  Sub-microsecond clock sync  │  │
  │  │  _airplay._tcp :7000   │                   │  Shared memory: /nqptp       │  │
  │  │  _raop._tcp :7000      │                   └──────────────┬───────────────┘  │
  │  └────────────────────────┘                                  │                  │
  │                                           Shared Memory Clocks (shm)            │
  │                                                              │                  │
  │                                                              ▼                  │
  │                                               ┌──────────────────────────────┐  │
  │                                               │       ya-shairport           │  │
  │                                               │   (Shairport-Sync 5.6.2)     │  │
  │                                               │  - Buffered Audio (Multiroom)│  │
  │                                               │  - ALAC / AAC Decoders       │  │
  │                                               │  - Hardware AudioTrack Sink  │  │
  │                                               └──────────────┬───────────────┘  │
  │                                                              │                  │
  │                 ┌────────────────────────────────────────────┼───────────────┐  │
  │                 │                                            │               │  │
  │                 ▼                                            ▼               ▼  │
  │  ┌─────────────────────────────┐              ┌───────────────────────────┐     │
  │  │      ya-volume-bridge       │              │         ya-voice          │     │
  │  │  (Android AudioService sync)│              │  (Alice Priority Ducking) │     │
  │  │  Bidirectional vol [0..100] │              │  Ducks AirPlay when voice │     │
  │  └─────────────────────────────┘              │  dialog is active         │     │
  │                                               └───────────────────────────┘     │
  │                                                                                 │
  │  ┌───────────────────────────────────────────────────────────────────────────┐  │
  │  │                        ya-supervisor (Process Monitor)                    │  │
  │  │  Supervises nqptp & shairport, handles crash recovery & bounded logging   │  │
  │  └───────────────────────────────────────────────────────────────────────────┘  │
  └─────────────────────────────────────────────────────────────────────────────────┘
```

### 1.1 Core Components

| Component | Binary / Package | Role & Responsibility |
|---|---|---|
| **Supervisor** | `ya-supervisor` | Root process supervisor. Spawns, monitors, and restarts child daemons (`ya-nqptp`, `ya-shairport`, `ya-voice`), enforces bounded ring logging (< 8 MB per file). |
| **PTP Clock Daemon** | `ya-nqptp` | Implementation of IEEE 1588 Precision Time Protocol (PTP v2) over UDP ports 319 & 320. Computes clock drift and master clock offsets to achieve sub-microsecond synchronization required for Apple Multi-Room audio. Communicates with receiver via POSIX shared memory (`/nqptp`). |
| **AirPlay 2 Receiver** | `ya-shairport` | Shairport-Sync 5.6.2 compiled for Android ARM64 API 28. Manages RTSP signaling on port 7000, FairPlay pairing verification, AES-128/ChaCha20 audio decrypt, ALAC/AAC demuxing, and low-latency audio rendering. |
| **mDNS Discovery** | `ya.station.airplay` (`nsd.apk`) | Native Android foreground service utilizing Android's `NsdManager` (`android.net.nsd.NsdManager`). Bypasses Avahi/mDNSResponder daemon limitations in Android Bionic by registering `_airplay._tcp` and `_raop._tcp` directly into the Android framework NSD daemon. |
| **Volume Bridge** | `volume-bridge.jar` | Java daemon binding to Android `IAudioService` via app_process. Translates iOS logarithmic AirPlay volume steps into linear Android TV stream volume (`STREAM_MUSIC`) and reflects physical volume ring changes back to iOS devices. |
| **Voice Ducking** | `ya-voice` | Background audio monitor inspecting Android audio focus and Alice state. Automatically attenuates or ducks AirPlay 2 playback volume when the user triggers Alice voice interactions. |

---

## 2. Upstream Bug Fixes & Engineering Hardening

During native integration of the upstream codebase into the `station_max` project, two critical stability bugs were identified and fixed:

### 2.1 Critical Fix 1: SIGSEGV Null Pointer Dereference in `ya-nqptp`
* **Symptom:** `ya-nqptp` crashed with SIGSEGV (fault address `0x50` in `get_client_id+540`) immediately upon receiving the initial control packet from `shairport`.
* **Root Cause:** In `airplay2/src/nqptp/nqptp-clock-sources.c`, the `mmap()` call allocating shared memory was conditionally guarded by `#ifdef CONFIG_FOR_LINUX`. However, `config.h` was **not included** in that compilation unit. As a result, `CONFIG_FOR_LINUX` was undefined during build, `mmap()` was skipped, leaving `clients[i].shared_memory` set to `0x0`. The subsequent `memset(clients[i].shared_memory, 0, ...)` immediately caused a null pointer fault.
* **Resolution:** Added `#include "config.h"` and `<stdio.h>` to `airplay2/src/nqptp/nqptp-clock-sources.c`, guaranteeing proper shared memory mapping via standard `MAP_SHARED`.

### 2.2 Critical Fix 2: 100% CPU Busy-Loop Lockup in `ya-nqptp`
* **Symptom:** `ya-nqptp` consumed 100% of a CPU core even when completely idle, draining CPU resources and warming the SoC.
* **Root Cause:** In `airplay2/src/runtime/volume-base/ya-thread.h`, `poll` was macro-redefined as `#define poll ya_poll`. When compiling `ya-thread.c`, passing `-include ya-thread.h` on the command line caused `poll` inside `ya_poll()` to recursively call `ya_poll()`. When called from the main thread (`self == NULL`), `ya_poll()` attempted to fall back to the real `poll()`:
  ```c
  if (!self || self->disabled || self->exiting) return poll(f, n, timeout);
  ```
  Because `poll` was macro-expanded to `ya_poll`, it tail-recursed into itself indefinitely. Clang optimized this with `-O2` into an unconditional jump to itself (`b 0x715c`).
* **Resolution:** Compiled `ya-thread.c` with `-DYA_THREAD_IMPLEMENTATION` and isolated from `-include ya-thread.h`. Inside `ya_poll()`, it now calls the real Bionic libc `poll@plt`. Idle CPU usage dropped to **0.0%**.

### 2.3 Fix 3: Non-Interactive ADB `su` Wrapper Hang
* **Symptom:** Automated scripts invoking `su 0 sh -c ...` hung indefinitely over ADB.
* **Root Cause:** `/odm/bin/su` invoked Dropbear `dbclient`, which held stdin open and dropped trailing positional arguments.
* **Resolution:** Built [configs/odm/su](configs/odm/su) with `< /dev/null` redirection and explicit `"$@"` passing.

---

## 3. Persistence & Boot Ordering

In accordance with **Hard Invariant Rule 1** (Never modify `/system`, `/vendor`, or `/boot`), persistence is achieved entirely within `/odm` and `/data`.

### 3.1 Cold Boot Sequence
1. Kernel boots in SELinux `Enforcing` mode.
2. `/odm/etc/init/rootd.rc` starts `rootd` at `sys.boot_completed=1`.
3. `fix-enforce.sh` flips kernel memory at `0x291352c` via Mali GPU esc-flip exploit until SELinux is `Permissive`.
4. `start-dropbear.sh` runs once `enforce == 0`, binds Dropbear SSH (:2222), and launches `/odm/bin/start-airplay2.sh &`.
5. `/odm/bin/start-airplay2.sh`:
   * Verifies `enforce == 0` (Permissive mode).
   * Verifies `sys.boot_completed == 1`.
   * Executes `/data/local/tmp/ya-airplay/start.sh 0.0.0.0 &`.
6. `start.sh` launches `ya-supervisor`, which starts `ya-nqptp`, `ya-shairport`, and registers `ya.station.airplay` NSD service.

---

## 4. Compilation & Build Recipes

The AirPlay 2 binaries are cross-compiled using the local Android NDK r30 LLVM toolchain for ARM64 Android API 28.

### 4.1 Compiling `ya-nqptp`

```bash
NDK_BIN="${ANDROID_NDK_HOME}/toolchains/llvm/prebuilt/darwin-x86_64/bin"
CC="${NDK_BIN}/aarch64-linux-android28-clang"
STRIP="${NDK_BIN}/llvm-strip"
BUILD_TMP="/tmp/nqptp_build"

mkdir -p "${BUILD_TMP}"

# 1. Compile NQPTP units with thread interceptors
for src in nqptp.c nqptp-clock-sources.c nqptp-message-handlers.c nqptp-utilities.c general-utilities.c debug.c; do
  $CC -std=gnu11 -O2 -g -Wall -Wextra -fPIE \
    -Iairplay2/src/nqptp -Iairplay2/src/compat -Iairplay2/src/runtime/volume-base \
    -include airplay2/src/nqptp/config.h \
    -include linux/if_packet.h \
    -include airplay2/src/runtime/volume-base/ya-thread.h \
    -include airplay2/src/compat/ya-shm.h \
    -c "airplay2/src/nqptp/${src}" -o "${BUILD_TMP}/${src}.o"
done

# 2. Compile compat shared memory unit
$CC -std=gnu11 -O2 -g -Wall -Wextra -fPIE \
  -Iairplay2/src/compat \
  -include airplay2/src/compat/ya-shm.h \
  -c airplay2/src/compat/ya-shm.c -o "${BUILD_TMP}/ya-shm.o"

# 3. Compile cooperative thread implementation (MUST define YA_THREAD_IMPLEMENTATION)
$CC -std=gnu11 -O2 -g -Wall -Wextra -fPIE \
  -DYA_THREAD_IMPLEMENTATION \
  -Iairplay2/src/runtime/volume-base -Iairplay2/src/runtime/base -Iairplay2/src/compat \
  -c airplay2/src/runtime/volume-base/ya-thread.c -o "${BUILD_TMP}/ya-thread.o"

# 4. Compile stop token unit
$CC -std=gnu11 -O2 -g -Wall -Wextra -fPIE \
  -Iairplay2/src/runtime/base -Iairplay2/src/compat \
  -c airplay2/src/compat/ya-stop.c -o "${BUILD_TMP}/ya-stop.o"

# 5. Link executable
$CC -fPIE -pie -pthread "${BUILD_TMP}"/*.o -o binaries/airplay2/bin/ya-nqptp
$STRIP --strip-debug binaries/airplay2/bin/ya-nqptp
rm -rf "${BUILD_TMP}"
```

---

## 5. Operations & CLI Reference

### 5.1 Deployment & Status Commands

```bash
# Check running status of supervisor, nqptp, and receiver:
python3 airplay2/tools/airplayctl.py status --root su --yes

# Deploy or re-deploy AirPlay 2 stack using differential updater:
./scripts/update.sh <STATION_IP> --airplay

# Direct installation via install.py:
python3 airplay2/install.py --model YNDX-00052 --root su --voice on --yes

# Uninstall AirPlay 2 cleanly:
python3 airplay2/uninstall.py --root su --yes
```

### 5.2 Network Discovery Verification (mDNS)

From any Mac on the same Wi-Fi/Ethernet network:

```bash
# Browse AirPlay 2 services:
dns-sd -B _airplay._tcp local

# Expected output:
# Timestamp     A/R  Flags if Domain   Service Type    Instance Name
# 23:42:57.822  Add      2  4 local.   _airplay._tcp.  Яндекс станция Макс

# Browse RAOP audio streaming services:
dns-sd -B _raop._tcp local

# Inspect AirPlay 2 TXT records:
dns-sd -L "Яндекс станция Макс" _airplay._tcp local
```

### 5.3 Device File System Locations

| Path on Station Max | Description |
|---|---|
| `/data/local/tmp/ya-airplay/` | Active AirPlay 2 runtime root |
| `/data/local/tmp/ya-airplay/bin/ya-supervisor` | Root supervisor binary |
| `/data/local/tmp/ya-airplay/bin/ya-nqptp` | PTP timing daemon |
| `/data/local/tmp/ya-airplay/bin/ya-shairport` | Shairport-Sync 5.6.2 AirPlay 2 receiver |
| `/data/local/tmp/ya-airplay/bin/ya-voice` | Alice priority audio ducking daemon |
| `/data/local/tmp/ya-airplay/lib/nsd.apk` | Installed foreground NSD service APK |
| `/data/local/tmp/ya-airplay/lib/volume-bridge.jar` | Android AudioService volume sync agent |
| `/data/local/tmp/ya-airplay/run/` | Runtime PID files, sockets, and logs |
| `/odm/bin/start-airplay2.sh` | Persistent boot trigger executed by `start-dropbear.sh` |
| `/odm/bin/su` | Dropbear-backed su wrapper for non-interactive commands |

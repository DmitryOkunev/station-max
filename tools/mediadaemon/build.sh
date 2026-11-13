#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Find Java
if [ -z "${JAVA_HOME:-}" ]; then
    if [ -d "/Applications/Android Studio.app/Contents/jbr/Contents/Home" ]; then
        export JAVA_HOME="/Applications/Android Studio.app/Contents/jbr/Contents/Home"
    elif [ -d "/Library/Java/JavaVirtualMachines" ]; then
        export JAVA_HOME="$(/usr/libexec/java_home 2>/dev/null || true)"
    fi
fi

if [ -z "${JAVA_HOME:-}" ] || [ ! -x "${JAVA_HOME}/bin/javac" ]; then
    echo "Error: javac not found. Set JAVA_HOME." >&2
    exit 1
fi

# Find android.jar
ANDROID_JAR="$(find ~/Library/Android/sdk/platforms -name "android.jar" 2>/dev/null | sort -V | tail -n 1)"
if [ -z "${ANDROID_JAR}" ]; then
    echo "Error: android.jar not found in ~/Library/Android/sdk/platforms" >&2
    exit 1
fi

# Find d8
D8_BIN="$(find ~/Library/Android/sdk/build-tools -name "d8" 2>/dev/null | sort -V | tail -n 1)"
if [ -z "${D8_BIN}" ]; then
    echo "Error: d8 not found in ~/Library/Android/sdk/build-tools" >&2
    exit 1
fi

echo "[*] Compiling MediaDaemon & MediaSeeker with JDK: ${JAVA_HOME}"
echo "[*] Using android.jar: ${ANDROID_JAR}"
echo "[*] Using d8: ${D8_BIN}"

BUILD_TMP="$(mktemp -d)"
trap 'rm -rf "${BUILD_TMP}"' EXIT

"${JAVA_HOME}/bin/javac" -cp "${ANDROID_JAR}" -d "${BUILD_TMP}" "${SCRIPT_DIR}/MediaDaemon.java" "${SCRIPT_DIR}/MediaSeeker.java"
"${D8_BIN}" --lib "${ANDROID_JAR}" --output "${BUILD_TMP}" "${BUILD_TMP}"/*.class

cp "${BUILD_TMP}/classes.dex" "${BASE_DIR}/binaries/mediaseeker.dex"
cp "${BUILD_TMP}/classes.dex" "${BASE_DIR}/configs/odm/mediaseeker.dex"

echo "[+] Successfully built binaries/mediaseeker.dex and configs/odm/mediaseeker.dex"


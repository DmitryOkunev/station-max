package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	stationDatPath        = "/data/data/com.yandex.io.sdk/files/data/data/user_config.dat"
	stationClockStatePath = "/data/data/com.yandex.io.sdk/files/data/clock_display_state"
	stationSysDir         = "/data/data/com.yandex.io.sdk/files/system"
	stationFB4Path        = "/sys/class/backlight/fb4/brightness"
	stationTmpBrightPath  = "/data/local/tmp/led_brightness"
	stationODMBrightPath  = "/odm/etc/led_brightness"
)

type MediaStatus struct {
	Reachable  bool
	Package    string
	State      int
	PosMs      int64
	DurationMs int64
	Title      string
	Raw        string
}

func (m MediaStatus) StateLabel() string {
	switch m.State {
	case 0:
		return "Нет данных"
	case 1:
		return "Остановлено"
	case 2:
		return "Пауза"
	case 3:
		return "Играет"
	case 4:
		return "Перемотка вперёд"
	case 5:
		return "Перемотка назад"
	case 6:
		return "Буферизация"
	case 7:
		return "Ошибка"
	default:
		return fmt.Sprintf("Код %d", m.State)
	}
}

type StationStatus struct {
	Reachable      bool
	SSHError       string
	SDKPID         string
	MediaDaemonPID string
	WatchdogPID    string
	DropbearPID    string
	SELinuxEnforce string
	ODMMountMode   string
	BrightnessFB4  int
	TmpBrightness  string
	ODMBrightness  string
	ClockState     map[string]any
	VolumeMusic    int
	VolumeAlarm    int
	StreamMode     string
	UniProxyURL    string
	Backups        []string
	Media          MediaStatus
}

type StationOps struct {
	mu  sync.Mutex
	cfg Config
}

func NewStationOps(cfg Config) *StationOps {
	return &StationOps{cfg: cfg}
}

func (s *StationOps) sshArgs(remoteCmd string) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "ConnectTimeout=5",
		"-p", strconv.Itoa(s.cfg.StationSSHPort),
		fmt.Sprintf("%s@%s", s.cfg.StationSSHUser, s.cfg.StationIP),
		remoteCmd,
	}
}

func (s *StationOps) runSSH(ctx context.Context, remoteCmd string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", s.sshArgs(remoteCmd)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("ssh station: %s", errMsg)
	}
	return stdout.Bytes(), nil
}

var volRe = regexp.MustCompile(`volume is (\d+)`)
var uniProxyCfgRe = regexp.MustCompile(`"uniProxyUrl"\s*:\s*"([^"]*)"`)

func parseMediaStatus(raw string) MediaStatus {
	raw = strings.TrimSpace(raw)
	ms := MediaStatus{Reachable: raw != "", Raw: raw}
	if !strings.HasPrefix(raw, "STATUS:") {
		return ms
	}
	rest := strings.TrimPrefix(raw, "STATUS:")
	// Format: <package>:state=<n>:pos=<n>:duration=<n>:title=<title>
	pIdx := strings.IndexByte(rest, ':')
	if pIdx < 0 {
		return ms
	}
	ms.Package = rest[:pIdx]
	rest = rest[pIdx+1:]

	tIdx := strings.Index(rest, ":title=")
	kvPart := rest
	if tIdx >= 0 {
		ms.Title = rest[tIdx+len(":title="):]
		kvPart = rest[:tIdx]
	}
	for _, part := range strings.Split(kvPart, ":") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "state":
			ms.State, _ = strconv.Atoi(v)
		case "pos":
			ms.PosMs, _ = strconv.ParseInt(v, 10, 64)
		case "duration":
			ms.DurationMs, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return ms
}

func (s *StationOps) MediaCmd(ctx context.Context, command string) (string, error) {
	addr := net.JoinHostPort(s.cfg.StationIP, strconv.Itoa(s.cfg.StationMediaPort))
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("MediaDaemon (:4545) недоступен: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, command+"\n"); err != nil {
		return "", err
	}
	reader := bufio.NewReader(io.LimitReader(conn, 8192))
	line, err := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line != "" {
		return line, nil
	}
	if err != nil {
		return "", err
	}
	return "", nil
}

func (s *StationOps) GetStatus(ctx context.Context) StationStatus {
	st := StationStatus{
		BrightnessFB4: -1,
		VolumeMusic:   -1,
		VolumeAlarm:   -1,
	}

	if resp, err := s.MediaCmd(ctx, "get"); err == nil {
		st.Media = parseMediaStatus(resp)
	}

	script := fmt.Sprintf(`
echo ""; echo "===SDK==="; pidof com.yandex.io.sdk 2>/dev/null || true
echo ""; echo "===MD==="; ps -A -o PID,ARGS 2>/dev/null | awk '$0 ~ /MediaDaemon/ && $0 !~ /awk/ {print $1}' | head -n 1
echo ""; echo "===WD==="; ps -A -o PID,ARGS 2>/dev/null | awk '$2 ~ /(^|\/)sh$/ && $3 == "/odm/bin/adbd-watchdog.sh" {print $1}' | head -n 1
echo ""; echo "===DB==="; pidof dropbearmulti 2>/dev/null || ps -A -o PID,ARGS 2>/dev/null | awk '$0 ~ /dropbear/ && $0 !~ /awk/ {print $1}' | head -n 1
echo ""; echo "===ENFORCE==="; cat /sys/fs/selinux/enforce 2>/dev/null || true
echo ""; echo "===ODM==="; awk '$2=="/odm"{print $4}' /proc/mounts 2>/dev/null | head -n 1
echo ""; echo "===FB4==="; cat %s 2>/dev/null || true
echo ""; echo "===TMPBR==="; cat %s 2>/dev/null || true
echo ""; echo "===ODMBR==="; cat %s 2>/dev/null || true
echo ""; echo "===CLOCK==="; cat %s 2>/dev/null || true
echo ""; echo "===VOL3==="; media volume --stream 3 --get 2>/dev/null || true
echo ""; echo "===VOL4==="; media volume --stream 4 --get 2>/dev/null || true
echo ""; echo "===QUASAR==="; grep -m1 '"uniProxyUrl"' %s/quasar.cfg 2>/dev/null || true
echo ""; echo "===BAKS==="; ls -1 %s* 2>/dev/null || true
echo ""
`, stationFB4Path, stationTmpBrightPath, stationODMBrightPath, stationClockStatePath, stationSysDir, stationDatPath)

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := s.runSSH(cctx, script, nil)
	if err != nil {
		st.Reachable = false
		st.SSHError = err.Error()
		return st
	}
	st.Reachable = true

	sections := splitSections(string(out))
	st.SDKPID = firstWord(sections["SDK"])
	st.MediaDaemonPID = firstWord(sections["MD"])
	st.WatchdogPID = firstWord(sections["WD"])
	st.DropbearPID = firstWord(sections["DB"])
	st.SELinuxEnforce = strings.TrimSpace(sections["ENFORCE"])

	odmOpts := strings.TrimSpace(sections["ODM"])
	if strings.HasPrefix(odmOpts, "ro") || strings.Contains(odmOpts, ",ro") {
		st.ODMMountMode = "ro"
	} else if odmOpts != "" {
		st.ODMMountMode = "rw"
	}

	if v, err := strconv.Atoi(strings.TrimSpace(sections["FB4"])); err == nil {
		st.BrightnessFB4 = v
	}
	st.TmpBrightness = strings.TrimSpace(sections["TMPBR"])
	st.ODMBrightness = strings.TrimSpace(sections["ODMBR"])

	if rawClk := strings.TrimSpace(sections["CLOCK"]); rawClk != "" {
		var m map[string]any
		if json.Unmarshal([]byte(rawClk), &m) == nil {
			st.ClockState = m
		}
	}

	if m := volRe.FindStringSubmatch(sections["VOL3"]); len(m) == 2 {
		st.VolumeMusic, _ = strconv.Atoi(m[1])
	}
	if m := volRe.FindStringSubmatch(sections["VOL4"]); len(m) == 2 {
		st.VolumeAlarm, _ = strconv.Atoi(m[1])
	}

	if m := uniProxyCfgRe.FindStringSubmatch(sections["QUASAR"]); len(m) == 2 {
		st.UniProxyURL = m[1]
		switch {
		case strings.Contains(m[1], "uniproxy.alice.yandex.net"):
			st.StreamMode = "yandex"
		case strings.HasPrefix(m[1], "ws://"):
			st.StreamMode = "bridge"
		default:
			st.StreamMode = "custom"
		}
	} else {
		st.StreamMode = "unknown"
	}

	for _, line := range strings.Split(sections["BAKS"], "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			st.Backups = append(st.Backups, line)
		}
	}
	return st
}

func splitSections(s string) map[string]string {
	out := make(map[string]string)
	var curKey string
	var curBuf strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "===") && strings.HasSuffix(line, "===") && len(line) > 6 {
			if curKey != "" {
				out[curKey] = strings.TrimSpace(curBuf.String())
			}
			curKey = line[3 : len(line)-3]
			curBuf.Reset()
			continue
		}
		if curKey != "" {
			curBuf.WriteString(line)
			curBuf.WriteByte('\n')
		}
	}
	if curKey != "" {
		out[curKey] = strings.TrimSpace(curBuf.String())
	}
	return out
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (s *StationOps) SetBrightnessInstant(ctx context.Context, val int) (int, error) {
	val = Clamp(val, 0, 200)
	cmd := fmt.Sprintf("printf '%%d\\n' %d > %s && echo %d > %s && cat %s",
		val, stationTmpBrightPath, val, stationFB4Path, stationFB4Path)
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := s.runSSH(cctx, cmd, nil)
	if err != nil {
		return 0, err
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return val, nil
	}
	return got, nil
}

func (s *StationOps) ResetTmpBrightness(ctx context.Context) (int, error) {
	cmd := fmt.Sprintf(`
rm -f %s
V=40
if [ -f %s ]; then
    T="$(tr -cd '0-9' < %s 2>/dev/null)"
    [ -n "$T" ] && V="$T"
fi
echo "$V" > %s
cat %s
`, stationTmpBrightPath, stationODMBrightPath, stationODMBrightPath, stationFB4Path, stationFB4Path)
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := s.runSSH(cctx, cmd, nil)
	if err != nil {
		return 0, err
	}
	got, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return got, nil
}

// BakeBrightnessToODM persists brightness into /odm/etc/led_brightness while
// strictly guaranteeing /odm is remounted back to ro cleanly.
func (s *StationOps) BakeBrightnessToODM(ctx context.Context, val int) (mountMode string, err error) {
	val = Clamp(val, 0, 200)
	cmd := fmt.Sprintf(`
set -e
cleanup() {
    sync 2>/dev/null || true
    mount -o remount,ro /odm 2>/dev/null || true
}
trap cleanup EXIT
mount -o remount,rw /odm
printf '%%d\n' %d > %s
chmod 644 %s
chcon u:object_r:vendor_configs_file:s0 %s 2>/dev/null || true
rm -f %s
echo %d > %s
sync
mount -o remount,ro /odm
awk '$2=="/odm"{print $4}' /proc/mounts | head -n 1
`, val, stationODMBrightPath, stationODMBrightPath, stationODMBrightPath, stationTmpBrightPath, val, stationFB4Path)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := s.runSSH(cctx, cmd, nil)
	mode := strings.TrimSpace(string(out))
	if err != nil {
		return mode, err
	}
	return mode, nil
}

func (s *StationOps) SetClockFlag(ctx context.Context, key string, val bool) (map[string]any, error) {
	switch key {
	case "clockEnabled", "isClockOn", "showClockDuringDnd":
	default:
		return nil, fmt.Errorf("неизвестный флаг clock_display_state: %s", key)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	raw, err := s.runSSH(cctx, fmt.Sprintf("cat %s", stationClockStatePath), nil)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("разбор clock_display_state: %w", err)
	}
	m[key] = val

	newBytes, err := marshalCompactNoEscape(m)
	if err != nil {
		return nil, err
	}
	writeCmd := fmt.Sprintf(
		"if [ ! -f %s.bak_bot ]; then cp %s %s.bak_bot 2>/dev/null || true; fi && cat > %s.tmp && chown u0_a39:u0_a39 %s.tmp && chmod 600 %s.tmp && mv -f %s.tmp %s",
		stationClockStatePath, stationClockStatePath, stationClockStatePath,
		stationClockStatePath, stationClockStatePath, stationClockStatePath, stationClockStatePath, stationClockStatePath,
	)
	if _, err := s.runSSH(cctx, writeCmd, append(newBytes, '\n')); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *StationOps) GetVolume(ctx context.Context) (music, alarm int, err error) {
	cmd := "echo '===3==='; media volume --stream 3 --get; echo '===4==='; media volume --stream 4 --get"
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := s.runSSH(cctx, cmd, nil)
	if err != nil {
		return -1, -1, err
	}
	sec := splitSections(string(out))
	music, alarm = -1, -1
	if m := volRe.FindStringSubmatch(sec["3"]); len(m) == 2 {
		music, _ = strconv.Atoi(m[1])
	}
	if m := volRe.FindStringSubmatch(sec["4"]); len(m) == 2 {
		alarm, _ = strconv.Atoi(m[1])
	}
	return music, alarm, nil
}

func (s *StationOps) SetVolume(ctx context.Context, stream3, stream4 int) (music, alarm int, err error) {
	var parts []string
	if stream3 >= 0 {
		v := Clamp(stream3, 0, 100)
		parts = append(parts, fmt.Sprintf("media volume --stream 3 --set %d >/dev/null", v))
	}
	if stream4 >= 0 {
		v := Clamp(stream4, 0, 100)
		parts = append(parts, fmt.Sprintf("media volume --stream 4 --set %d >/dev/null", v))
	}
	parts = append(parts, "echo '===3==='; media volume --stream 3 --get; echo '===4==='; media volume --stream 4 --get")

	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := s.runSSH(cctx, strings.Join(parts, "; "), nil)
	if err != nil {
		return -1, -1, err
	}
	sec := splitSections(string(out))
	music, alarm = -1, -1
	if m := volRe.FindStringSubmatch(sec["3"]); len(m) == 2 {
		music, _ = strconv.Atoi(m[1])
	}
	if m := volRe.FindStringSubmatch(sec["4"]); len(m) == 2 {
		alarm, _ = strconv.Atoi(m[1])
	}
	return music, alarm, nil
}

func (s *StationOps) FetchUserConfig(ctx context.Context) (*UserConfigDoc, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := s.runSSH(cctx, fmt.Sprintf("cat %s", stationDatPath), nil)
	if err != nil {
		return nil, err
	}
	return ParseUserConfigDat(raw, s.cfg.StationUID)
}

func (s *StationOps) CommitUserConfig(ctx context.Context, changes []StagedChange) (string, error) {
	if len(changes) == 0 {
		return "", errors.New("черновик пуст")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	raw, err := s.runSSH(cctx, fmt.Sprintf("cat %s", stationDatPath), nil)
	if err != nil {
		return "", fmt.Errorf("чтение user_config.dat: %w", err)
	}
	doc, err := ParseUserConfigDat(raw, s.cfg.StationUID)
	if err != nil {
		return "", err
	}
	outDat, err := doc.ApplyChanges(changes)
	if err != nil {
		return "", fmt.Errorf("сборка user_config.dat: %w", err)
	}

	writeCmd := fmt.Sprintf(`
set -e
DAT="%s"
toybox chattr -i "$DAT" 2>/dev/null || true
if [ ! -f "${DAT}.bak_bot" ]; then cp "$DAT" "${DAT}.bak_bot"; fi
cat > "${DAT}.tmp"
SZ="$(wc -c < "${DAT}.tmp" | tr -d ' ')"
if [ "$SZ" != "%d" ]; then
    rm -f "${DAT}.tmp"
    echo "size mismatch: $SZ != %d" >&2
    exit 1
fi
chown u0_a39:u0_a39 "${DAT}.tmp"
chmod 600 "${DAT}.tmp"
mv -f "${DAT}.tmp" "$DAT"
PIDS="$(pidof com.yandex.io.sdk 2>/dev/null || true)"
if [ -n "$PIDS" ]; then kill -9 $PIDS 2>/dev/null || true; fi
i=0
while [ $i -lt 15 ]; do
    sleep 1
    NPID="$(pidof com.yandex.io.sdk 2>/dev/null || true)"
    if [ -n "$NPID" ]; then
        echo "$NPID"
        exit 0
    fi
    i=$((i + 1))
done
echo "restarting"
`, stationDatPath, len(outDat), len(outDat))

	out, err := s.runSSH(cctx, writeCmd, outDat)
	if err != nil {
		return "", err
	}
	return firstWord(string(out)), nil
}

func (s *StationOps) RestoreUserConfigBackup(ctx context.Context, suffix string) (string, error) {
	switch suffix {
	case ".bak_bot", ".bak_20261110", ".orig":
	default:
		return "", fmt.Errorf("неизвестный бэкап: %s", suffix)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	cmd := fmt.Sprintf(`
set -e
DAT="%s"
SRC="%s%s"
if [ ! -s "$SRC" ]; then
    echo "Бэкап $SRC отсутствует или пуст" >&2
    exit 1
fi
toybox chattr -i "$DAT" 2>/dev/null || true
cp -f "$SRC" "${DAT}.tmp"
chown u0_a39:u0_a39 "${DAT}.tmp"
chmod 600 "${DAT}.tmp"
mv -f "${DAT}.tmp" "$DAT"
PIDS="$(pidof com.yandex.io.sdk 2>/dev/null || true)"
if [ -n "$PIDS" ]; then kill -9 $PIDS 2>/dev/null || true; fi
i=0
while [ $i -lt 12 ]; do
    sleep 1
    NPID="$(pidof com.yandex.io.sdk 2>/dev/null || true)"
    if [ -n "$NPID" ]; then
        echo "$NPID"
        exit 0
    fi
    i=$((i + 1))
done
echo "restarting"
`, stationDatPath, stationDatPath, suffix)

	out, err := s.runSSH(cctx, cmd, nil)
	if err != nil {
		return "", err
	}
	return firstWord(string(out)), nil
}

func (s *StationOps) RunStreamSwitch(ctx context.Context, mode string) (string, error) {
	if s.cfg.StreamScriptPath == "" {
		return "", errors.New("STREAM_SCRIPT_PATH не задан")
	}
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	var args []string
	switch mode {
	case "bridge":
		args = []string{"bridge", s.cfg.StationIP, s.cfg.ServerIP}
	case "yandex", "stock":
		args = []string{"yandex", s.cfg.StationIP}
	case "status":
		args = []string{"status", s.cfg.StationIP}
	default:
		return "", fmt.Errorf("неизвестный режим потока: %s", mode)
	}
	cmd := exec.CommandContext(cctx, s.cfg.StreamScriptPath, args...)
	out, err := cmd.CombinedOutput()
	res := strings.TrimSpace(string(out))
	if err != nil {
		return res, fmt.Errorf("%v: %s", err, res)
	}
	return res, nil
}

func (s *StationOps) RestartService(ctx context.Context, svc string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var cmd string
	switch svc {
	case "sdk":
		cmd = `
PIDS="$(pidof com.yandex.io.sdk 2>/dev/null || true)"
if [ -n "$PIDS" ]; then kill -9 $PIDS 2>/dev/null || true; fi
i=0
while [ $i -lt 12 ]; do
    sleep 1
    NP="$(pidof com.yandex.io.sdk 2>/dev/null || true)"
    if [ -n "$NP" ]; then echo "SDK PID=$NP"; exit 0; fi
    i=$((i + 1))
done
echo "SDK перезапускается..."
`
	case "mediadaemon":
		cmd = `
for p in $(ps -A -o PID,ARGS | awk '$0 ~ /MediaDaemon/ && $0 !~ /awk/ {print $1}'); do
    kill -9 "$p" 2>/dev/null || true
done
/odm/bin/start-mediadaemon.sh
sleep 1
NP="$(ps -A -o PID,ARGS | awk '$0 ~ /MediaDaemon/ && $0 !~ /awk/ {print $1}' | head -n 1)"
echo "MediaDaemon PID=${NP:-не найден}"
`
	case "watchdog":
		cmd = `
for p in $(ps -A -o PID,ARGS | awk '$2 ~ /(^|\/)sh$/ && $3 == "/odm/bin/adbd-watchdog.sh" {print $1}'); do
    kill "$p" 2>/dev/null || true
done
nohup /odm/bin/adbd-watchdog.sh </dev/null >/dev/null 2>&1 &
sleep 1
NP="$(ps -A -o PID,ARGS | awk '$2 ~ /(^|\/)sh$/ && $3 == "/odm/bin/adbd-watchdog.sh" {print $1}' | head -n 1)"
echo "Watchdog PID=${NP:-не найден}"
`
	case "adbd":
		cmd = `
setprop service.adb.tcp.port 5555
for p in $(pidof adbd 2>/dev/null); do kill -9 "$p" 2>/dev/null || true; done
sleep 1
if ! pidof adbd >/dev/null 2>&1; then
    nohup /system/bin/adbd --root_seclabel=u:r:su:s0 </dev/null >/dev/null 2>&1 &
    sleep 1
fi
echo "adbd PID=$(pidof adbd 2>/dev/null || echo 'starting')"
`
	default:
		return "", fmt.Errorf("неизвестная служба: %s", svc)
	}

	out, err := s.runSSH(cctx, cmd, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}


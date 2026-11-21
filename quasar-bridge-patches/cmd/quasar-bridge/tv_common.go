package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/sound"
)

type smartTubeConfig struct {
	Disabled      bool   `json:"disabled"`
	ADBAddress    string `json:"adb_address"`
	Package       string `json:"package"`
	ZonaPackage   string `json:"zona_package"`
	LazyPackage   string `json:"lazy_package"`
	ZonaActivity  string `json:"zona_activity"`
	LazyActivity  string `json:"lazy_activity"`
	LazySearchAct string `json:"lazy_search_activity"`
	IntentDelayMS int    `json:"intent_delay_ms"`
}

func (c *smartTubeConfig) defaults() {
	if c.ADBAddress == "" {
		c.ADBAddress = "127.0.0.1:5555"
	}
	if c.Package == "" {
		c.Package = "org.smarttube.beta"
	}
	if c.ZonaPackage == "" {
		c.ZonaPackage = "mobi.zona"
	}
	if c.LazyPackage == "" {
		c.LazyPackage = "com.lazycatsoftware.lmd"
	}
	if c.ZonaActivity == "" {
		c.ZonaActivity = "mobi.zona/ru.zona.app.android.MainActivity"
	}
	if c.LazyActivity == "" {
		c.LazyActivity = "com.lazycatsoftware.lmd/com.lazycatsoftware.lazymediadeluxe.ActivityStart"
	}
	if c.LazySearchAct == "" {
		c.LazySearchAct = "com.lazycatsoftware.lmd/com.lazycatsoftware.lazymediadeluxe.ui.tv.activities.ActivityTvSearch"
	}
	if c.IntentDelayMS <= 0 {
		c.IntentDelayMS = 600
	}
}

var smartTubeCandidatePackages = []string{
	"org.smarttube.stable",
	"com.teamsmart.videomanager.tv",
	"org.smarttube.beta",
	"com.liskovsoft.smarttubetv.beta",
}

func isSmartTubePackage(pkg string) bool {
	for _, p := range smartTubeCandidatePackages {
		if pkg == p {
			return true
		}
	}
	return false
}

type smartTubeControl struct {
	cfg           smartTubeConfig
	app           context.Context
	mu            sync.RWMutex
	state         int // 3 = Playing, 2 = Paused, 0 = Idle
	lastPlaying   time.Time
	lastCheck     time.Time
	mediaTitle    string
	activePackage string
	detectedSTPkg string
	cancelPoller  context.CancelFunc
}

func newSmartTubeControl(app context.Context, cfg smartTubeConfig) (*smartTubeControl, error) {
	if cfg.Disabled {
		return nil, nil
	}
	cfg.defaults()
	ctrl := &smartTubeControl{
		cfg:           cfg,
		app:           app,
		activePackage: cfg.Package,
	}
	ctrl.ensureMediaDaemon()
	return ctrl, nil
}

func (c *smartTubeControl) isLocal() bool {
	addr := strings.TrimSpace(c.cfg.ADBAddress)
	return addr == "local" || addr == "localhost" || addr == "127.0.0.1" || addr == ""
}

func (c *smartTubeControl) runADB(ctx context.Context, shellCmd string) (string, error) {
	if c.isLocal() {
		shPath := "/system/bin/sh"
		if _, err := os.Stat(shPath); err != nil {
			shPath = "/bin/sh"
		}
		cmd := exec.CommandContext(ctx, shPath, "-c", shellCmd)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	cmd := exec.CommandContext(ctx, "adb", "-s", c.cfg.ADBAddress, "shell", shellCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		sOut := string(out)
		if strings.Contains(sOut, "not found") || strings.Contains(sOut, "offline") || strings.Contains(sOut, "device") {
			connectCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = exec.CommandContext(connectCtx, "adb", "connect", c.cfg.ADBAddress).Run()
			cancel()
			retryCmd := exec.CommandContext(ctx, "adb", "-s", c.cfg.ADBAddress, "shell", shellCmd)
			rOut, rErr := retryCmd.CombinedOutput()
			if rErr == nil {
				return string(rOut), nil
			}
		}
		return string(out), err
	}
	return string(out), nil
}

func (c *smartTubeControl) stationHost() string {
	if c.isLocal() {
		return "127.0.0.1"
	}
	addr := c.cfg.ADBAddress
	if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
		return host
	}
	if strings.Contains(addr, ":") {
		return strings.Split(addr, ":")[0]
	}
	if addr != "" {
		return addr
	}
	return "127.0.0.1"
}

func (c *smartTubeControl) callMediaDaemon(ctx context.Context, cmd string) (string, error) {
	host := c.stationHost()
	target := net.JoinHostPort(host, "4545")

	d := net.Dialer{Timeout: 400 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(1200 * time.Millisecond))
	if !strings.HasSuffix(cmd, "\n") {
		cmd += "\n"
	}
	if _, err := conn.Write([]byte(cmd)); err != nil {
		return "", err
	}

	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		resp := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(resp, "ERR:") {
			return resp, fmt.Errorf("mediadaemon: %s", resp)
		}
		return resp, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("empty response from mediadaemon")
}

func (c *smartTubeControl) ensureMediaDaemon() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = c.runADB(ctx, "/odm/bin/start-mediadaemon.sh")
	}()
}

func parseMediaDaemonStatus(resp string) (pkg string, state int, posMs int64, durationMs int64, title string, ok bool) {
	if !strings.HasPrefix(resp, "STATUS:") {
		return "", 0, -1, -1, "", false
	}
	parts := strings.SplitN(resp, ":", 3)
	if len(parts) < 3 {
		return "", 0, -1, -1, "", false
	}
	pkg = parts[1]
	rest := parts[2]
	posMs = -1
	durationMs = -1

	for _, kv := range strings.Split(rest, ":") {
		if strings.HasPrefix(kv, "state=") {
			state, _ = strconv.Atoi(strings.TrimPrefix(kv, "state="))
		} else if strings.HasPrefix(kv, "pos=") {
			posMs, _ = strconv.ParseInt(strings.TrimPrefix(kv, "pos="), 10, 64)
		} else if strings.HasPrefix(kv, "duration=") {
			durationMs, _ = strconv.ParseInt(strings.TrimPrefix(kv, "duration="), 10, 64)
		} else if strings.HasPrefix(kv, "title=") {
			title = strings.TrimPrefix(kv, "title=")
		}
	}
	return pkg, state, posMs, durationMs, title, true
}

var (
	mediaStateRegex   = regexp.MustCompile(`state=PlaybackState\s*\{state=(\d+)`)
	mediaDescRegex    = regexp.MustCompile(`metadata:size=\d+,\s*description=([^,\n]+)`)
	mediaPosRegex     = regexp.MustCompile(`position=(\d+)`)
	mediaUpdatedRegex = regexp.MustCompile(`updated=(\d+)`)
	mediaSpeedRegex   = regexp.MustCompile(`speed=([\d\.]+)`)
)

func parseMediaSessionPosition(out string, pkg string) int {
	lines := strings.Split(out, "\n")
	inPkgSession := false
	posMs := int64(-1)
	updatedMs := int64(-1)
	speed := 1.0
	uptimeSec := float64(-1)

	for _, line := range lines {
		trim := strings.TrimSpace(line)
		// Check for /proc/uptime output format: "19104.99 50847.13"
		if uptimeSec < 0 && strings.Contains(trim, " ") && !strings.Contains(trim, "=") && !strings.Contains(trim, ":") {
			fields := strings.Fields(trim)
			if len(fields) >= 2 {
				if u, err := strconv.ParseFloat(fields[0], 64); err == nil && u > 0 {
					uptimeSec = u
				}
			}
		}

		if strings.HasPrefix(trim, "package="+pkg) || strings.Contains(line, pkg+"/"+pkg) {
			inPkgSession = true
		} else if inPkgSession && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "      ") {
			inPkgSession = false
		}

		if inPkgSession {
			if m := mediaPosRegex.FindStringSubmatch(trim); len(m) == 2 {
				posMs, _ = strconv.ParseInt(m[1], 10, 64)
			}
			if m := mediaUpdatedRegex.FindStringSubmatch(trim); len(m) == 2 {
				updatedMs, _ = strconv.ParseInt(m[1], 10, 64)
			}
			if m := mediaSpeedRegex.FindStringSubmatch(trim); len(m) == 2 {
				speed, _ = strconv.ParseFloat(m[1], 64)
			}
		}
	}

	if posMs < 0 {
		return -1
	}

	if updatedMs > 0 && uptimeSec > 0 && speed > 0 {
		uptimeMs := int64(uptimeSec * 1000)
		elapsedMs := uptimeMs - updatedMs
		if elapsedMs > 0 && elapsedMs < 24*3600*1000 {
			posMs += int64(float64(elapsedMs) * speed)
		}
	}

	return int(posMs / 1000)
}

func parseSmartTubeStatus(out string, pkg string) (int, string) {
	lines := strings.Split(out, "\n")
	inPkgSession := false
	state := 0
	title := ""

	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "package="+pkg) || strings.Contains(line, pkg+"/"+pkg) {
			inPkgSession = true
		} else if inPkgSession && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "      ") {
			inPkgSession = false
		}

		if inPkgSession {
			if m := mediaStateRegex.FindStringSubmatch(trim); len(m) == 2 {
				state, _ = strconv.Atoi(m[1])
			}
			if m := mediaDescRegex.FindStringSubmatch(trim); len(m) == 2 {
				title = strings.TrimSpace(m[1])
			}
		}
	}
	return state, title
}

func parseTVAppsStatus(out string, packages []string) (string, int, string) {
	type pkgInfo struct {
		state int
		title string
	}
	found := make(map[string]pkgInfo)
	var currentPkg string

	for _, line := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(line)
		for _, pkg := range packages {
			if strings.HasPrefix(trim, "package="+pkg) || strings.Contains(line, pkg+"/"+pkg) {
				currentPkg = pkg
				break
			}
		}
		if currentPkg != "" && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "      ") {
			isSame := false
			for _, pkg := range packages {
				if strings.Contains(line, pkg) {
					currentPkg = pkg
					isSame = true
					break
				}
			}
			if !isSame {
				currentPkg = ""
			}
		}

		if currentPkg != "" {
			info := found[currentPkg]
			if m := mediaStateRegex.FindStringSubmatch(trim); len(m) == 2 {
				info.state, _ = strconv.Atoi(m[1])
			}
			if m := mediaDescRegex.FindStringSubmatch(trim); len(m) == 2 {
				info.title = strings.TrimSpace(m[1])
			}
			found[currentPkg] = info
		}
	}

	audioOrder := ""
	if audioIdx := strings.Index(out, "Audio playback (lastly played comes first)"); audioIdx != -1 {
		audioOrder = out[audioIdx:]
	}

	bestPkg := ""
	bestIdx := -1
	for _, pkg := range packages {
		idx := strings.Index(audioOrder, pkg)
		if idx != -1 && (bestIdx == -1 || idx < bestIdx) {
			bestIdx = idx
			bestPkg = pkg
		}
	}

	if bestPkg != "" {
		info := found[bestPkg]
		st := info.state
		if st == 0 {
			st = 3
		}
		return bestPkg, st, info.title
	}

	for _, pkg := range packages {
		if info, ok := found[pkg]; ok && info.state == 3 {
			return pkg, info.state, info.title
		}
	}
	for _, pkg := range packages {
		if info, ok := found[pkg]; ok && info.state == 2 {
			return pkg, info.state, info.title
		}
	}

	return "", 0, ""
}

func parseCombinedDeviceStatus(out string, packages []string) (activePkg string, state int, title string) {
	parts := strings.Split(out, "---SESSION---")
	actPart := parts[0]
	sessionPart := ""
	if len(parts) > 1 {
		sessionPart = parts[1]
	}

	// 1. Check if any target package is currently the resumed activity on TV screen
	resumedPkg := ""
	for _, pkg := range packages {
		if strings.Contains(actPart, pkg) {
			resumedPkg = pkg
			break
		}
	}

	// 2. Parse session metadata and playback state
	sessionPkg, sessionState, sessionTitle := parseTVAppsStatus(sessionPart, packages)

	// 3. Audio playback check
	audioOrder := ""
	if audioIdx := strings.Index(sessionPart, "Audio playback (lastly played comes first)"); audioIdx != -1 {
		audioOrder = sessionPart[audioIdx:]
	}

	yandexPlaying := false
	topTVIdx := -1
	topTVPkg := ""
	for _, pkg := range packages {
		idx := strings.Index(audioOrder, pkg)
		if idx != -1 && (topTVIdx == -1 || idx < topTVIdx) {
			topTVIdx = idx
			topTVPkg = pkg
		}
	}

	if topTVPkg != "" {
		for _, yxPkg := range []string{"com.yandex.tv.videoplayer", "com.yandex.tv.music", "com.yandex.devices.player"} {
			yxIdx := strings.Index(audioOrder, yxPkg)
			if yxIdx != -1 && yxIdx < topTVIdx {
				yandexPlaying = true
				break
			}
		}
	}

	if yandexPlaying {
		return "", 0, ""
	}

	if topTVPkg != "" {
		st := sessionState
		if st == 0 {
			st = 3 // Audio is actively playing
		}
		return topTVPkg, st, sessionTitle
	}

	if resumedPkg != "" {
		return resumedPkg, 2, sessionTitle
	}

	if sessionPkg != "" && sessionState > 0 {
		return sessionPkg, sessionState, sessionTitle
	}

	return "", 0, ""
}

func (c *smartTubeControl) getSmartTubePackage(ctx context.Context) string {
	c.mu.RLock()
	detected := c.detectedSTPkg
	cfgPkg := c.cfg.Package
	c.mu.RUnlock()

	if detected != "" {
		return detected
	}

	// If explicit custom package configured in config.json, respect it
	if cfgPkg != "" && cfgPkg != "org.smarttube.beta" && cfgPkg != "org.smarttube.stable" {
		c.mu.Lock()
		c.detectedSTPkg = cfgPkg
		c.mu.Unlock()
		return cfgPkg
	}

	// Probe station for installed SmartTube packages
	// Check in strict priority order: Release/Stable -> Legacy Release -> Beta -> Legacy Beta
	checkCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	out, err := c.runADB(checkCtx, "pm list packages smarttube; pm list packages teamsmart")
	if err == nil {
		for _, cand := range smartTubeCandidatePackages {
			if strings.Contains(out, "package:"+cand) || strings.Contains(out, cand) {
				c.mu.Lock()
				c.detectedSTPkg = cand
				c.mu.Unlock()
				return cand
			}
		}
	}

	if cfgPkg != "" {
		return cfgPkg
	}
	return "org.smarttube.stable"
}

func (c *smartTubeControl) allTargetPackages() []string {
	pkgs := []string{c.cfg.ZonaPackage, c.cfg.LazyPackage}
	c.mu.RLock()
	detected := c.detectedSTPkg
	c.mu.RUnlock()

	if detected != "" {
		pkgs = append([]string{detected}, pkgs...)
	} else if c.cfg.Package != "" {
		pkgs = append([]string{c.cfg.Package}, pkgs...)
	}

	for _, cand := range smartTubeCandidatePackages {
		found := false
		for _, p := range pkgs {
			if p == cand {
				found = true
				break
			}
		}
		if !found {
			pkgs = append(pkgs, cand)
		}
	}
	return pkgs
}

func (c *smartTubeControl) checkDeviceStatus(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	resp, err := c.callMediaDaemon(checkCtx, "get")
	cancel()

	if err == nil {
		if pkg, stateNum, _, _, title, ok := parseMediaDaemonStatus(resp); ok && pkg != "" {
			c.mu.Lock()
			c.lastCheck = time.Now()
			c.activePackage = pkg
			if isSmartTubePackage(pkg) {
				c.detectedSTPkg = pkg
			}
			c.state = stateNum
			if stateNum == 3 {
				c.lastPlaying = time.Now()
			} else if stateNum == 2 && c.lastPlaying.IsZero() {
				c.lastPlaying = time.Now()
			}
			if title != "" {
				c.mediaTitle = title
			}
			c.mu.Unlock()
			return
		}
	}

	c.ensureMediaDaemon()

	checkCtx2, cancel2 := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel2()

	cmd := "dumpsys activity activities | grep mResumedActivity; echo '---SESSION---'; dumpsys media_session"
	out, err := c.runADB(checkCtx2, cmd)
	c.mu.Lock()
	c.lastCheck = time.Now()
	if err != nil {
		c.mu.Unlock()
		return
	}

	packages := c.allTargetPackages()
	bestPkg, stateNum, title := parseCombinedDeviceStatus(out, packages)

	if bestPkg != "" {
		c.activePackage = bestPkg
		if isSmartTubePackage(bestPkg) {
			c.detectedSTPkg = bestPkg
		}
	}
	c.state = stateNum
	if stateNum == 3 {
		c.lastPlaying = time.Now()
	} else if stateNum == 2 && c.lastPlaying.IsZero() {
		c.lastPlaying = time.Now()
	}
	if title != "" {
		c.mediaTitle = title
	}
	c.mu.Unlock()
}

func calculateSmartTubeRepeats(sec int) int {
	if sec <= 15 {
		return 1
	}
	if sec <= 30 {
		return 2
	}
	if sec <= 60 {
		return 4
	}
	if sec <= 120 {
		return 8
	}
	if sec <= 180 {
		return 12
	}
	if sec <= 300 {
		return 18
	}
	if sec <= 600 {
		return 28
	}
	return 35
}

func (c *smartTubeControl) getMediaPosition(ctx context.Context, pkg string) int {
	checkCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	resp, err := c.callMediaDaemon(checkCtx, "get")
	cancel()
	if err == nil {
		if targetPkg, _, posMs, _, _, ok := parseMediaDaemonStatus(resp); ok && (pkg == "" || strings.Contains(targetPkg, pkg) || strings.Contains(pkg, targetPkg)) {
			if posMs >= 0 {
				return int(posMs / 1000)
			}
		}
	}

	checkCtx2, cancel2 := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel2()
	out, err := c.runADB(checkCtx2, "dumpsys media_session; cat /proc/uptime")
	if err != nil {
		return -1
	}
	return parseMediaSessionPosition(out, pkg)
}

func (c *smartTubeControl) CanLaunchApp(text string) (string, bool) {
	return bridge.SmartTubeAppCommand(text)
}

func (c *smartTubeControl) LaunchApp(ctx context.Context, app string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var answer string
	var activity string

	switch app {
	case "home":
		_, err := c.runADB(ctx, "input keyevent 127; input keyevent 3")
		c.mu.Lock()
		c.state = 0
		c.mediaTitle = ""
		c.mu.Unlock()
		return "", err

	case "zona":
		c.PauseBackground()
		c.mu.Lock()
		c.state = 2 // active on screen
		c.lastPlaying = time.Now()
		c.activePackage = c.cfg.ZonaPackage
		c.mediaTitle = ""
		c.mu.Unlock()
		answer = "Открываю Зону."
		activity = c.cfg.ZonaActivity

	case "media":
		c.PauseBackground()
		c.mu.Lock()
		c.state = 2 // active on screen
		c.lastPlaying = time.Now()
		c.activePackage = c.cfg.LazyPackage
		c.mediaTitle = ""
		c.mu.Unlock()
		answer = "Открываю медиа."
		activity = c.cfg.LazyActivity

	case "lazy":
		c.PauseBackground()
		c.mu.Lock()
		c.state = 2 // active on screen
		c.lastPlaying = time.Now()
		c.activePackage = c.cfg.LazyPackage
		c.mediaTitle = ""
		c.mu.Unlock()
		answer = "Открываю LazyMedia."
		activity = c.cfg.LazyActivity

	default:
		return "", fmt.Errorf("unknown app: %s", app)
	}

	delay := time.Duration(c.cfg.IntentDelayMS) * time.Millisecond
	go func(targetAct string) {
		time.Sleep(delay)
		bgCtx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()

		_, _ = c.runADB(bgCtx, "input keyevent 224")

		startCmd := fmt.Sprintf("am start -n %s", targetAct)
		_, err := c.runADB(bgCtx, startCmd)
		if err != nil {
			slog.Warn("app_launch_failed", "activity", targetAct, "error", err)
		}
	}(activity)

	return answer, nil
}

func (c *smartTubeControl) CanPlay(text string) (string, bool) {
	return bridge.SmartTubePlayCommand(text)
}

func (c *smartTubeControl) CanSearchApp(text string) (string, string, bool) {
	return bridge.TVAppSearchCommand(text)
}

func (c *smartTubeControl) Play(ctx context.Context, query string) (string, error) {
	return c.playSmartTube(ctx, query)
}

func (c *smartTubeControl) SearchApp(ctx context.Context, app string, query string) (string, error) {
	switch app {
	case "media", "lazy":
		return c.searchLazyMedia(ctx, query)
	case "zona":
		return c.searchZona(ctx, query)
	default:
		return "", fmt.Errorf("unknown app for search: %s", app)
	}
}

func (c *smartTubeControl) Command(text string) string {
	return bridge.SmartTubeMediaCommand(text)
}

func (c *smartTubeControl) Active(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}

	// Check if native Yandex player is currently playing
	cur := sound.CurrentPlayback(ctx)
	if cur.PlayerState == "Playing" {
		return false
	}

	c.mu.RLock()
	st := c.state
	lastP := c.lastPlaying
	lastC := c.lastCheck
	c.mu.RUnlock()

	// If currently playing in memory (state 3)
	if st == 3 {
		return true
	}

	// If paused in memory (state 2) within 2 hours
	if st == 2 && !lastP.IsZero() && time.Since(lastP) < 2*time.Hour {
		return true
	}

	// If state in memory is 0, check device status (throttled by 1 second)
	if time.Since(lastC) > 1*time.Second {
		c.checkDeviceStatus(ctx)
		c.mu.RLock()
		st = c.state
		lastP = c.lastPlaying
		c.mu.RUnlock()

		if st == 3 {
			return true
		}
		if st == 2 && !lastP.IsZero() && time.Since(lastP) < 2*time.Hour {
			return true
		}
	}

	return false
}

func (c *smartTubeControl) ControlMedia(ctx context.Context, action string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	c.mu.RLock()
	slog.Info("smarttube_control", "action", action, "pkg", c.activePackage)
	c.mu.RUnlock()

	var err error
	var answer string

	switch {
	case action == "pause" || action == "stop":
		c.mu.Lock()
		c.state = 2
		activePkg := c.activePackage
		c.mu.Unlock()
		answer = ""

		if isSmartTubePackage(activePkg) {
			if _, dErr := c.callMediaDaemon(ctx, "pause"); dErr == nil {
				return "", nil
			}
		}

		if activePkg == c.cfg.ZonaPackage {
			go func() {
				time.Sleep(300 * time.Millisecond)
				bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_, _ = c.runADB(bgCtx, "input keyevent 85")
			}()
		} else {
			// Immediate pause keyevent
			_, err = c.runADB(ctx, "input keyevent 127")
			// Delayed retry keyevent (once Alice dialog dismisses and focus returns to player)
			go func() {
				time.Sleep(300 * time.Millisecond)
				bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_, _ = c.runADB(bgCtx, "input keyevent 127")
			}()
		}

	case action == "resume" || action == "play":
		c.mu.Lock()
		c.state = 3
		c.lastPlaying = time.Now()
		activePkg := c.activePackage
		c.mu.Unlock()
		answer = ""

		if isSmartTubePackage(activePkg) {
			if _, dErr := c.callMediaDaemon(ctx, "play"); dErr == nil {
				return "", nil
			}
		}

		if activePkg == c.cfg.ZonaPackage {
			go func() {
				time.Sleep(300 * time.Millisecond)
				bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_, _ = c.runADB(bgCtx, "input keyevent 85")
			}()
		} else {
			// Immediate play keyevent
			_, err = c.runADB(ctx, "input keyevent 126")
			// Delayed retry keyevent (once Alice dialog dismisses and focus returns to player)
			go func() {
				time.Sleep(300 * time.Millisecond)
				bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_, _ = c.runADB(bgCtx, "input keyevent 126")
			}()
		}

	case action == "close":
		c.mu.Lock()
		c.state = 0
		c.mediaTitle = ""
		c.mu.Unlock()
		stopCmd := fmt.Sprintf("input keyevent 127; input keyevent 3; am force-stop org.smarttube.stable; am force-stop org.smarttube.beta; am force-stop com.teamsmart.videomanager.tv; am force-stop %s; am force-stop %s",
			c.cfg.ZonaPackage, c.cfg.LazyPackage)
		_, err = c.runADB(ctx, stopCmd)
		answer = ""

	case action == "next":
		answer = ""
		c.mu.Lock()
		c.state = 3
		c.lastPlaying = time.Now()
		activePkg := c.activePackage
		c.mu.Unlock()

		if isSmartTubePackage(activePkg) {
			if _, dErr := c.callMediaDaemon(ctx, "next"); dErr == nil {
				return "", nil
			}
		}

		go func() {
			time.Sleep(250 * time.Millisecond)
			bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = c.runADB(bgCtx, "input keyevent 87")
		}()

	case action == "previous":
		answer = ""
		c.mu.Lock()
		c.state = 3
		c.lastPlaying = time.Now()
		activePkg := c.activePackage
		c.mu.Unlock()

		if isSmartTubePackage(activePkg) {
			if _, dErr := c.callMediaDaemon(ctx, "prev"); dErr == nil {
				return "", nil
			}
		}

		go func() {
			time.Sleep(250 * time.Millisecond)
			bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = c.runADB(bgCtx, "input keyevent 88")
		}()

	case action == "restart":
		answer = ""
		c.mu.Lock()
		c.state = 3
		c.lastPlaying = time.Now()
		activePkg := c.activePackage
		c.mu.Unlock()

		if isSmartTubePackage(activePkg) {
			if resp, dErr := c.callMediaDaemon(ctx, "seek 0s"); dErr == nil && strings.HasPrefix(resp, "SEEK_OK") {
				return "", nil
			}
		}

		go func() {
			time.Sleep(250 * time.Millisecond)
			bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = c.runADB(bgCtx, "input keyevent 88; sleep 0.2; input keyevent 88")
		}()

	case strings.HasPrefix(action, "seek:forward:"):
		secStr := strings.TrimPrefix(action, "seek:forward:")
		sec, _ := strconv.Atoi(secStr)
		if sec <= 0 {
			sec = 15
		}
		c.mu.RLock()
		activePkg := c.activePackage
		c.mu.RUnlock()

		if isSmartTubePackage(activePkg) {
			daemonCmd := fmt.Sprintf("seek +%ds %s", sec, activePkg)
			resp, dErr := c.callMediaDaemon(ctx, daemonCmd)
			if dErr == nil && strings.HasPrefix(resp, "SEEK_OK") {
				slog.Info("mediadaemon_seek_ok", "response", resp)
				return "", nil
			}
		}

		var cmdStr string
		if isSmartTubePackage(activePkg) {
			repeats := calculateSmartTubeRepeats(sec)
			var keys []string
			for i := 0; i < repeats; i++ {
				keys = append(keys, "90")
			}
			cmdStr = fmt.Sprintf("input keyevent %s; sleep 0.2; input keyevent 126", strings.Join(keys, " "))
		} else {
			repeats := max(1, min(10, sec/10))
			var keys []string
			for i := 0; i < repeats; i++ {
				keys = append(keys, "22")
			}
			cmdStr = fmt.Sprintf("input keyevent %s", strings.Join(keys, " "))
		}

		answer = ""
		go func() {
			time.Sleep(300 * time.Millisecond)
			bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = c.runADB(bgCtx, cmdStr)
		}()

	case strings.HasPrefix(action, "seek:rewind:"):
		secStr := strings.TrimPrefix(action, "seek:rewind:")
		sec, _ := strconv.Atoi(secStr)
		if sec <= 0 {
			sec = 15
		}
		c.mu.RLock()
		activePkg := c.activePackage
		c.mu.RUnlock()

		if isSmartTubePackage(activePkg) {
			daemonCmd := fmt.Sprintf("seek -%ds %s", sec, activePkg)
			resp, dErr := c.callMediaDaemon(ctx, daemonCmd)
			if dErr == nil && strings.HasPrefix(resp, "SEEK_OK") {
				slog.Info("mediadaemon_rewind_ok", "response", resp)
				return "", nil
			}
		}

		var cmdStr string
		if isSmartTubePackage(activePkg) {
			repeats := calculateSmartTubeRepeats(sec)
			var keys []string
			for i := 0; i < repeats; i++ {
				keys = append(keys, "89")
			}
			cmdStr = fmt.Sprintf("input keyevent %s; sleep 0.2; input keyevent 126", strings.Join(keys, " "))
		} else {
			repeats := max(1, min(10, sec/10))
			var keys []string
			for i := 0; i < repeats; i++ {
				keys = append(keys, "21")
			}
			cmdStr = fmt.Sprintf("input keyevent %s", strings.Join(keys, " "))
		}

		answer = ""
		go func() {
			time.Sleep(300 * time.Millisecond)
			bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = c.runADB(bgCtx, cmdStr)
		}()

	case strings.HasPrefix(action, "seek:to:"):
		secStr := strings.TrimPrefix(action, "seek:to:")
		targetSec, _ := strconv.Atoi(secStr)
		if targetSec < 0 {
			targetSec = 0
		}
		c.mu.RLock()
		activePkg := c.activePackage
		c.mu.RUnlock()

		if isSmartTubePackage(activePkg) {
			daemonCmd := fmt.Sprintf("seek %ds %s", targetSec, activePkg)
			resp, dErr := c.callMediaDaemon(ctx, daemonCmd)
			if dErr == nil && strings.HasPrefix(resp, "SEEK_OK") {
				slog.Info("mediadaemon_seek_to_ok", "response", resp)
				return "", nil
			}
		}

		if isSmartTubePackage(activePkg) {
			curSec := c.getMediaPosition(ctx, activePkg)
			delta := targetSec
			if curSec >= 0 {
				delta = targetSec - curSec
			}

			key := "90"
			absDelta := delta
			if delta < 0 {
				key = "89"
				absDelta = -delta
			}

			if curSec < 0 || absDelta >= 5 {
				repeats := calculateSmartTubeRepeats(absDelta)
				var keys []string
				for i := 0; i < repeats; i++ {
					keys = append(keys, key)
				}
				cmdStr := fmt.Sprintf("input keyevent %s; sleep 0.2; input keyevent 126", strings.Join(keys, " "))
				go func() {
					time.Sleep(300 * time.Millisecond)
					bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					_, _ = c.runADB(bgCtx, cmdStr)
				}()
			}
		} else {
			repeats := max(1, min(10, targetSec/15))
			var keys []string
			for i := 0; i < repeats; i++ {
				keys = append(keys, "22")
			}
			cmdStr := fmt.Sprintf("input keyevent %s", strings.Join(keys, " "))
			go func() {
				time.Sleep(300 * time.Millisecond)
				bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, _ = c.runADB(bgCtx, cmdStr)
			}()
		}
		answer = ""

	case action == "status":
		c.mu.RLock()
		title := c.mediaTitle
		state := c.state
		pkg := c.activePackage
		c.mu.RUnlock()
		appName := "видеоплеере"
		if isSmartTubePackage(pkg) {
			appName = "YouTube"
		} else if pkg == c.cfg.ZonaPackage {
			appName = "Зоне"
		} else if pkg == c.cfg.LazyPackage {
			appName = "LazyMedia"
		}
		if title != "" {
			stText := "играет"
			if state == 2 {
				stText = "на паузе"
			}
			answer = fmt.Sprintf("Сейчас %s в %s: «%s».", stText, appName, title)
		} else {
			answer = fmt.Sprintf("Сейчас ничего не играет в %s.", appName)
		}

	default:
		return "", fmt.Errorf("unknown TV app media action: %s", action)
	}

	if err != nil {
		slog.Warn("tvapp_control_failed", "action", action, "error", err)
		return "", err
	}
	return answer, nil
}

func (c *smartTubeControl) PauseBackground() {
	c.mu.Lock()
	wasPlaying := c.state == 3
	if wasPlaying {
		c.state = 2
	}
	c.mu.Unlock()

	if wasPlaying {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			_, _ = c.runADB(ctx, "input keyevent 127")
		}()
	}
}

func (c *smartTubeControl) OnSmartHomeCompleted(ctx context.Context, isSmartHome bool) {
	if c == nil || c.cfg.Disabled || !isSmartHome {
		return
	}

	// 1. Dismiss any lingering TV system dialogs / Alice overlays
	go func() {
		for _, delay := range []time.Duration{300 * time.Millisecond, 800 * time.Millisecond, 1500 * time.Millisecond} {
			time.Sleep(delay)
			dismissCtx, dismissCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = c.runADB(dismissCtx, "am broadcast -a android.intent.action.CLOSE_SYSTEM_DIALOGS")
			dismissCancel()
		}
	}()

	// 2. Auto-resume background video playback if it was playing recently
	c.mu.RLock()
	wasPlaying := c.state == 3 || (c.state == 2 && !c.lastPlaying.IsZero() && time.Since(c.lastPlaying) < 2*time.Minute)
	c.mu.RUnlock()

	if !wasPlaying {
		// Quick check on device in case state was 0
		checkCtx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		c.checkDeviceStatus(checkCtx)
		cancel()

		c.mu.RLock()
		wasPlaying = c.state == 3 || (c.state == 2 && !c.lastPlaying.IsZero() && time.Since(c.lastPlaying) < 2*time.Minute)
		c.mu.RUnlock()
	}

	if wasPlaying {
		go func() {
			// Wait for chime playback (~600-800ms) to complete before resuming video
			time.Sleep(900 * time.Millisecond)
			resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer resumeCancel()
			slog.Info("smarttube_auto_resuming_media_after_iot")
			_, _ = c.ControlMedia(resumeCtx, "resume")
		}()
	}
}

func quoteShell(s string) string {
	s = strings.ReplaceAll(s, "'", "'\\''")
	return "'" + s + "'"
}

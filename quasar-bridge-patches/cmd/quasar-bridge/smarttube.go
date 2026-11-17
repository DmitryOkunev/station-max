package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"
)

func (c *smartTubeControl) playSmartTube(ctx context.Context, query string) (string, error) {
	pkg := c.getSmartTubePackage(ctx)

	c.mu.Lock()
	c.state = 3
	c.activePackage = pkg
	c.lastPlaying = time.Now()
	if query != "" {
		c.mediaTitle = query
	}
	c.mu.Unlock()

	answer := "Открываю YouTube."
	if query != "" {
		answer = fmt.Sprintf("Включаю «%s» на YouTube.", query)
	}

	delay := time.Duration(c.cfg.IntentDelayMS) * time.Millisecond
	go func(q string, targetPkg string) {
		time.Sleep(delay)
		bgCtx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()

		_, _ = c.runADB(bgCtx, "input keyevent 224")

		splashActivity := fmt.Sprintf("%s/com.liskovsoft.smartyoutubetv2.tv.ui.main.SplashActivity", targetPkg)
		var startCmd string
		if q == "" {
			startCmd = fmt.Sprintf("am start -n %s", splashActivity)
		} else {
			encoded := url.QueryEscape(q)
			targetURL := fmt.Sprintf("youtube://search?query=%s&method=play", encoded)
			startCmd = fmt.Sprintf("am start -a android.intent.action.VIEW -d '%s' -n %s", targetURL, splashActivity)
		}
		slog.Info("smarttube_launch_intent", "query", q, "package", targetPkg, "cmd", startCmd)
		_, err := c.runADB(bgCtx, startCmd)
		if err != nil {
			slog.Warn("smarttube_start_failed", "package", targetPkg, "error", err)
		}

		// Dismiss any remaining Alice voice interaction dialogs on TV screen
		for _, delaySec := range []float64{1.2, 2.5, 4.0} {
			time.Sleep(time.Duration(delaySec * float64(time.Second)))
			dismissCtx, dismissCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = c.runADB(dismissCtx, "am broadcast -a android.intent.action.CLOSE_SYSTEM_DIALOGS")
			dismissCancel()
		}
	}(query, pkg)

	return answer, nil
}

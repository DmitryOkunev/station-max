package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

func (c *smartTubeControl) searchLazyMedia(ctx context.Context, query string) (string, error) {
	c.PauseBackground()

	c.mu.Lock()
	c.state = 2 // Mark as active/ready in foreground
	c.lastPlaying = time.Now()
	c.activePackage = c.cfg.LazyPackage
	c.mediaTitle = query
	c.mu.Unlock()

	answer := fmt.Sprintf("Ищу «%s» в LazyMedia.", query)
	delay := time.Duration(c.cfg.IntentDelayMS) * time.Millisecond
	go func(q string) {
		time.Sleep(delay)
		bgCtx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()

		_, _ = c.runADB(bgCtx, "input keyevent 224")

		// Search only - user selects stream and plays manually
		startCmd := fmt.Sprintf("am start -n %s --es query %s", c.cfg.LazySearchAct, quoteShell(q))
		_, err := c.runADB(bgCtx, startCmd)
		if err != nil {
			slog.Warn("lazymedia_search_failed", "query", q, "error", err)
		}

		for _, delaySec := range []float64{1.2, 2.5, 4.0} {
			time.Sleep(time.Duration(delaySec * float64(time.Second)))
			dismissCtx, dismissCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = c.runADB(dismissCtx, "am broadcast -a android.intent.action.CLOSE_SYSTEM_DIALOGS")
			dismissCancel()
		}
	}(query)

	return answer, nil
}


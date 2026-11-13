package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

type kpSuggestResult struct {
	ID       int
	IsSeries bool
	Title    string
}

func resolveKinopoisk(ctx context.Context, query string) (*kpSuggestResult, error) {
	apiURL := "https://www.kinopoisk.ru/api/suggest/v2/?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0 Win64 x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/87.0.4280.141 Safari/537.36")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Accept", "application/json, text/plain, */*")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kinopoisk suggest status: %d", resp.StatusCode)
	}

	var data struct {
		Suggest struct {
			Top struct {
				TopResult struct {
					Global struct {
						ID       int    `json:"id"`
						TypeName string `json:"__typename"`
						Title    struct {
							Russian  string `json:"russian"`
							Original string `json:"original"`
						} `json:"title"`
					} `json:"global"`
				} `json:"topResult"`
			} `json:"top"`
		} `json:"suggest"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	res := data.Suggest.Top.TopResult.Global
	if res.ID == 0 {
		return nil, errors.New("movie not found")
	}

	title := res.Title.Russian
	if title == "" {
		title = res.Title.Original
	}
	if title == "" {
		title = query
	}

	return &kpSuggestResult{
		ID:       res.ID,
		IsSeries: res.TypeName == "TvSeries" || res.TypeName == "Series",
		Title:    title,
	}, nil
}

func (c *smartTubeControl) searchZona(ctx context.Context, query string) (string, error) {
	c.PauseBackground()

	c.mu.Lock()
	c.state = 2 // Mark as active/ready in foreground
	c.lastPlaying = time.Now()
	c.activePackage = c.cfg.ZonaPackage
	c.mediaTitle = query
	c.mu.Unlock()

	// Resolve via Kinopoisk Suggest API
	resolveCtx, resolveCancel := context.WithTimeout(ctx, 2*time.Second)
	kp, err := resolveKinopoisk(resolveCtx, query)
	resolveCancel()

	var answer string
	var targetURL string
	if err == nil && kp != nil && kp.ID > 0 {
		answer = fmt.Sprintf("Открываю «%s» в Зоне.", kp.Title)
		if kp.IsSeries {
			targetURL = fmt.Sprintf("https://www.kinopoisk.ru/series/%d", kp.ID)
		} else {
			targetURL = fmt.Sprintf("https://www.kinopoisk.ru/film/%d", kp.ID)
		}
	} else {
		answer = fmt.Sprintf("Открываю Зону для поиска «%s».", query)
	}

	delay := time.Duration(c.cfg.IntentDelayMS) * time.Millisecond
	go func(u string) {
		time.Sleep(delay)
		bgCtx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()

		_, _ = c.runADB(bgCtx, "input keyevent 224")

		var startCmd string
		if u != "" {
			startCmd = fmt.Sprintf("am start -a android.intent.action.VIEW -d '%s' -n %s", u, c.cfg.ZonaActivity)
		} else {
			startCmd = fmt.Sprintf("am start -n %s", c.cfg.ZonaActivity)
		}
		_, aErr := c.runADB(bgCtx, startCmd)
		if aErr != nil {
			slog.Warn("zona_start_failed", "url", u, "error", aErr)
		}

		for _, delaySec := range []float64{1.2, 2.5, 4.0} {
			time.Sleep(time.Duration(delaySec * float64(time.Second)))
			dismissCtx, dismissCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = c.runADB(dismissCtx, "am broadcast -a android.intent.action.CLOSE_SYSTEM_DIALOGS")
			dismissCancel()
		}
	}(targetURL)

	return answer, nil
}


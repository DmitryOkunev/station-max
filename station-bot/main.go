package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	envPath := flag.String("env", "/opt/station-bot/bot.env", "Путь к .env файлу конфигурации")
	checkOnly := flag.Bool("check", false, "Проверить конфигурацию, bridge и доступность станции и выйти")
	flag.Parse()

	cfg, err := LoadConfig(*envPath)
	if err != nil {
		log.Fatalf("Ошибка чтения %s: %v", *envPath, err)
	}

	if *checkOnly {
		app := NewBotApp(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		fmt.Printf("Station: %s:%d (user=%s, uid=%s)\n", cfg.StationIP, cfg.StationSSHPort, cfg.StationSSHUser, cfg.StationUID)
		fmt.Printf("BridgeConfig: %s\n", cfg.BridgeConfigPath)
		fmt.Printf("DraftPath: %s (staged=%d)\n", cfg.DraftPath, len(app.draft.Snapshot()))
		fmt.Printf("TokenConfigured: %v | AllowedUsers: %d\n", !IsPlaceholderToken(cfg.BotToken), len(cfg.AllowedUsers))
		if brData, err := app.bridge.ReadRawMap(); err == nil {
			mode, _ := GetString(brData, "mode")
			model, _ := GetString(brData, "llm_model")
			fmt.Printf("Bridge: mode=%s model=%s rules=%d\n", mode, model, len(ExtractBridgeRules(brData)))
		} else {
			fmt.Printf("BridgeError: %v\n", err)
		}
		st := app.station.GetStatus(ctx)
		fmt.Printf("StationStatus: reachable=%v sdk=%s fb4=%d vol=%d/%d stream=%s odm=%s media=%v\n",
			st.Reachable, st.SDKPID, st.BrightnessFB4, st.VolumeMusic, st.VolumeAlarm, st.StreamMode, st.ODMMountMode, st.Media.Reachable)
		if !st.Reachable {
			os.Exit(1)
		}
		return
	}

	if IsPlaceholderToken(cfg.BotToken) {
		log.Printf("TELEGRAM_BOT_TOKEN не задан в %s (или указан шаблон). Бот ожидает настройки.", *envPath)
		log.Printf("Создайте бота через @BotFather, пропишите TELEGRAM_BOT_TOKEN и TELEGRAM_ALLOWED_USERS в %s и выполните: systemctl restart station-bot", *envPath)
		os.Exit(0)
	}

	if len(cfg.AllowedUsers) == 0 {
		log.Printf("ВНИМАНИЕ: TELEGRAM_ALLOWED_USERS пуст в %s! Бот запущен в fail-closed режиме: никому не даст доступ, но ответит вашим numeric ID при отправке /start.", *envPath)
	}

	app := NewBotApp(cfg)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmdCtx, cmdCancel := context.WithTimeout(rootCtx, 10*time.Second)
	err = app.tg.SetMyCommands(cmdCtx, []TGBotCommand{
		{Command: "menu", Description: "Главное меню управления"},
		{Command: "status", Description: "Состояние станции, служб и моста"},
		{Command: "br", Description: "Яркость дисплея fb4 (0..200)"},
		{Command: "vol", Description: "Громкость (0..100)"},
		{Command: "pause", Description: "Пауза видео (:4545)"},
		{Command: "play", Description: "Возобновить видео (:4545)"},
		{Command: "seek", Description: "Перемотка (+30s, -1m, 12:30)"},
		{Command: "next", Description: "Следующее видео"},
		{Command: "prev", Description: "Предыдущее видео"},
		{Command: "route", Description: "Тест маршрута Quasar Bridge"},
		{Command: "get_uc", Description: "Прочитать ключ user_config.dat"},
		{Command: "set_uc", Description: "Изменить ключ user_config.dat"},
		{Command: "get_br", Description: "Прочитать ключ bridge config.json"},
		{Command: "set_br", Description: "Изменить ключ bridge config.json"},
		{Command: "cancel", Description: "Отменить текущий текстовый ввод"},
	})
	cmdCancel()
	if err != nil {
		log.Printf("SetMyCommands warning: %v", err)
	}

	log.Printf("station-bot запущен (station=%s:%d, allowed_users=%d)", cfg.StationIP, cfg.StationSSHPort, len(cfg.AllowedUsers))

	var offset int64
	backoff := 2 * time.Second
	for {
		if rootCtx.Err() != nil {
			log.Printf("Получен сигнал завершения, остановка...")
			return
		}
		pollCtx, pollCancel := context.WithTimeout(rootCtx, 40*time.Second)
		updates, err := app.tg.GetUpdates(pollCtx, offset, 30)
		pollCancel()
		if err != nil {
			if rootCtx.Err() != nil {
				return
			}
			log.Printf("getUpdates error: %v (повтор через %v)", err, backoff)
			select {
			case <-rootCtx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 20*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 2 * time.Second

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			hCtx, hCancel := context.WithTimeout(rootCtx, 45*time.Second)
			app.HandleUpdate(hCtx, u)
			hCancel()
		}
	}
}


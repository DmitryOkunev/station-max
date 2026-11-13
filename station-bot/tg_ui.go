package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type pendingInput struct {
	Kind       string
	ReturnMenu string
	Extra      string
}

type BotApp struct {
	cfg     Config
	tg      *TGClient
	station *StationOps
	bridge  *BridgeManager
	draft   *DraftStore

	mu           sync.Mutex
	pending      map[int64]pendingInput
	cachedModels map[int64][]string
}

func NewBotApp(cfg Config) *BotApp {
	return &BotApp{
		cfg:          cfg,
		tg:           NewTGClient(cfg.BotToken),
		station:      NewStationOps(cfg),
		bridge:       NewBridgeManager(cfg),
		draft:        NewDraftStore(cfg.DraftPath),
		pending:      make(map[int64]pendingInput),
		cachedModels: make(map[int64][]string),
	}
}

func (a *BotApp) isAllowed(userID int64) bool {
	if len(a.cfg.AllowedUsers) == 0 {
		return false
	}
	return a.cfg.AllowedUsers[userID]
}

func (a *BotApp) HandleUpdate(ctx context.Context, u TGUpdate) {
	if u.CallbackQuery != nil {
		a.handleCallback(ctx, u.CallbackQuery)
		return
	}
	if u.Message != nil {
		a.handleMessage(ctx, u.Message)
	}
}

func (a *BotApp) denyMessageText(userID int64) string {
	return fmt.Sprintf(
		"⛔ <b>Доступ запрещён.</b>\n\nВаш Telegram ID: <code>%d</code>\nДобавьте его в <code>TELEGRAM_ALLOWED_USERS</code> в файле <code>/opt/station-bot/bot.env</code> на сервере и перезапустите сервис <code>station-bot</code>.",
		userID,
	)
}

func (a *BotApp) setPending(chatID int64, p pendingInput) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending[chatID] = p
}

func (a *BotApp) getPending(chatID int64) (pendingInput, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pending[chatID]
	return p, ok
}

func (a *BotApp) clearPending(chatID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pending, chatID)
}

func (a *BotApp) setCachedModels(chatID int64, models []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cachedModels[chatID] = models
}

func (a *BotApp) getCachedModel(chatID int64, idx int) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	arr := a.cachedModels[chatID]
	if idx < 0 || idx >= len(arr) {
		return "", false
	}
	return arr[idx], true
}

func (a *BotApp) handleMessage(ctx context.Context, m *TGMessage) {
	userID := m.From.ID
	if !a.isAllowed(userID) {
		log.Printf("unauthorized message from user=%d (@%s) chat=%d", userID, m.From.Username, m.Chat.ID)
		_, _ = a.tg.SendMessage(ctx, m.Chat.ID, a.denyMessageText(userID), nil)
		return
	}

	txt := strings.TrimSpace(m.Text)
	if txt == "" {
		return
	}

	if strings.HasPrefix(txt, "/") {
		a.clearPending(m.Chat.ID)
		a.handleSlashCommand(ctx, m.Chat.ID, txt)
		return
	}

	if p, ok := a.getPending(m.Chat.ID); ok {
		a.handlePendingInput(ctx, m.Chat.ID, p, txt)
		return
	}

	// Default helpful response: open main menu
	a.sendMenu(ctx, m.Chat.ID, "m:home")
}

func (a *BotApp) handleSlashCommand(ctx context.Context, chatID int64, raw string) {
	cmdWord, arg, _ := strings.Cut(raw, " ")
	// Strip @botname suffix if present
	if at := strings.IndexByte(cmdWord, '@'); at >= 0 {
		cmdWord = cmdWord[:at]
	}
	cmdWord = strings.ToLower(strings.TrimSpace(cmdWord))
	arg = strings.TrimSpace(arg)

	switch cmdWord {
	case "/start", "/menu":
		a.sendMenu(ctx, chatID, "m:home")
	case "/cancel":
		a.clearPending(chatID)
		_, _ = a.tg.SendMessage(ctx, chatID, "✖️ Ввод отменён.", backHomeMarkup())
	case "/status":
		a.sendMenu(ctx, chatID, "m:sys")
	case "/br":
		if arg == "" {
			a.sendMenu(ctx, chatID, "m:disp")
			return
		}
		v, err := strconv.Atoi(arg)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Укажите целое число 0..200, например: <code>/br 40</code>", nil)
			return
		}
		got, err := a.station.SetBrightnessInstant(ctx, v)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Ошибка яркости: <code>"+Esc(err.Error())+"</code>", nil)
			return
		}
		_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("🔆 Яркость дисплея <code>fb4</code> установлена: <b>%d / 200</b> (записано также в <code>led_brightness</code>).", got), backToMarkup("m:disp", "🔆 Дисплей"))
	case "/vol":
		if arg == "" {
			a.sendMenu(ctx, chatID, "m:aud")
			return
		}
		v, err := strconv.Atoi(arg)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Укажите громкость 0..100, например: <code>/vol 45</code>", nil)
			return
		}
		m3, m4, err := a.station.SetVolume(ctx, v, v)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Ошибка громкости: <code>"+Esc(err.Error())+"</code>", nil)
			return
		}
		_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("🔊 Громкость установлена: Музыка <b>%d</b>, Будильник <b>%d</b>", m3, m4), backToMarkup("m:aud", "🔊 Звук"))
	case "/pause", "/stop":
		a.execMediaCommandAndReply(ctx, chatID, "pause", "⏸ Пауза")
	case "/play":
		a.execMediaCommandAndReply(ctx, chatID, "play", "▶️ Воспроизведение")
	case "/next":
		a.execMediaCommandAndReply(ctx, chatID, "next", "⏭ Следующее")
	case "/prev":
		a.execMediaCommandAndReply(ctx, chatID, "prev", "⏮ Предыдущее")
	case "/seek":
		if arg == "" {
			_, _ = a.tg.SendMessage(ctx, chatID, "Использование: <code>/seek +30s</code>, <code>/seek -30s</code>, <code>/seek 5m</code>, <code>/seek 12:30</code>", nil)
			return
		}
		cmd, err := ParseSeekInput(arg)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), nil)
			return
		}
		a.execMediaCommandAndReply(ctx, chatID, cmd, "🎯 Перемотка "+arg)
	case "/route":
		if arg == "" {
			_, _ = a.tg.SendMessage(ctx, chatID, "Использование: <code>/route алиса включи матрицу</code>", nil)
			return
		}
		out, err := a.bridge.ExplainRoute(ctx, arg)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Ошибка проверки маршрута: <code>"+Esc(err.Error())+"</code>", nil)
			return
		}
		_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("🧪 <b>Тест маршрута:</b> <code>%s</code>\n\n%s", Esc(arg), wrapPre(out, 3000)), backToMarkup("m:rules", "🛤 Правила"))
	case "/get_uc":
		a.replyGetUC(ctx, chatID, arg)
	case "/set_uc":
		a.replySetUC(ctx, chatID, arg, "m:raw")
	case "/get_br":
		a.replyGetBR(ctx, chatID, arg)
	case "/set_br":
		a.replySetBR(ctx, chatID, arg, "m:raw")
	default:
		_, _ = a.tg.SendMessage(ctx, chatID, "Неизвестная команда. Нажмите /menu для главного меню.", backHomeMarkup())
	}
}

func (a *BotApp) execMediaCommandAndReply(ctx context.Context, chatID int64, cmd, label string) {
	resp, err := a.station.MediaCmd(ctx, cmd)
	if err != nil {
		_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:tv", "📺 ТВ"))
		return
	}
	_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("%s\nОтвет MediaDaemon: <code>%s</code>", label, Esc(resp)), backToMarkup("m:tv", "📺 ТВ"))
}

func (a *BotApp) handleCallback(ctx context.Context, cq *TGCallbackQuery) {
	userID := cq.From.ID
	if !a.isAllowed(userID) {
		log.Printf("unauthorized callback from user=%d (@%s) data=%q", userID, cq.From.Username, cq.Data)
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, fmt.Sprintf("Доступ запрещён. Ваш ID: %d", userID), true)
		return
	}
	if cq.Message == nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "", false)
		return
	}
	chatID := cq.Message.Chat.ID
	msgID := cq.Message.MessageID
	data := cq.Data

	// Navigation routes
	if strings.HasPrefix(data, "m:") {
		if data == "m:cancel" {
			a.clearPending(chatID)
			data = "m:home"
		} else {
			a.clearPending(chatID)
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "", false)
		a.renderAndEditMenu(ctx, chatID, msgID, data)
		return
	}

	// Prompt initiation: p:<kind>
	if strings.HasPrefix(data, "p:") {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "", false)
		a.startPrompt(ctx, chatID, msgID, strings.TrimPrefix(data, "p:"), "")
		return
	}

	// Rule prompt: rp:<idx>
	if strings.HasPrefix(data, "rp:") {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "", false)
		a.startPrompt(ctx, chatID, msgID, "rule_prompt", strings.TrimPrefix(data, "rp:"))
		return
	}

	// Instant brightness: db:<val|bake|rst>
	if strings.HasPrefix(data, "db:") {
		a.handleBrightnessCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "db:"))
		return
	}

	// Instant clock_display_state flags: dc:<en|on|dnd>
	if strings.HasPrefix(data, "dc:") {
		a.handleClockCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "dc:"))
		return
	}

	// Instant volume: vol:<val|-10|+10|=20...>
	if strings.HasPrefix(data, "vol:") {
		a.handleVolumeCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "vol:"))
		return
	}

	// Instant MediaDaemon commands: mc:<play|pause|next|prev|r30|f30|f300>
	if strings.HasPrefix(data, "mc:") {
		a.handleMediaCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "mc:"))
		return
	}

	// Draft management: uc:<apply|diff|reset>
	if strings.HasPrefix(data, "uc:") {
		a.handleDraftActionCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "uc:"))
		return
	}

	// Command spotter presets: ucp:<std|none>
	if strings.HasPrefix(data, "ucp:") {
		a.handleSpotterPresetCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "ucp:"))
		return
	}

	// Draft toggles in user_config.dat: ut:<key>
	if strings.HasPrefix(data, "ut:") {
		a.handleUserConfigToggleCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "ut:"))
		return
	}

	// Equalizer preset: eq:<id>
	if strings.HasPrefix(data, "eq:") {
		preset := strings.TrimPrefix(data, "eq:")
		if err := a.draft.Set([]string{"device_config", "equalizer", "active_preset_id"}, preset); err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
			return
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "В черновик: preset="+preset, false)
		a.renderAndEditMenu(ctx, chatID, msgID, "m:aud")
		return
	}

	// Visualizer style: dstyle:<name>
	if strings.HasPrefix(data, "dstyle:") {
		style := strings.TrimPrefix(data, "dstyle:")
		if err := a.draft.Set([]string{"device_config", "led", "music_equalizer_visualization", "style"}, style); err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
			return
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "В черновик: style="+style, false)
		a.renderAndEditMenu(ctx, chatID, msgID, "m:disp")
		return
	}

	// Stream switch: st:<bridge|yandex>
	if strings.HasPrefix(data, "st:") {
		mode := strings.TrimPrefix(data, "st:")
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Переключаю поток на "+mode+"...", false)
		_, err := a.station.RunStreamSwitch(ctx, mode)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Ошибка переключения потока:\n"+wrapPre(err.Error(), 1800), backToMarkup("m:br", "🤖 Bridge"))
			return
		}
		a.renderAndEditMenu(ctx, chatID, msgID, "m:br")
		return
	}

	// Bridge mode: bm:<hybrid|bridge|yandex>
	if strings.HasPrefix(data, "bm:") {
		mode := strings.TrimPrefix(data, "bm:")
		err := a.bridge.UpdateAndApply(ctx, false, func(m map[string]any) error {
			m["mode"] = mode
			return nil
		})
		if err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
			return
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Bridge mode -> "+mode, false)
		a.renderAndEditMenu(ctx, chatID, msgID, "m:br")
		return
	}

	// Bridge model select: mo:<idx>
	if strings.HasPrefix(data, "mo:") {
		idx, err := strconv.Atoi(strings.TrimPrefix(data, "mo:"))
		modelID, ok := a.getCachedModel(chatID, idx)
		if err != nil || !ok {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Список моделей устарел, обновите меню", true)
			a.renderAndEditMenu(ctx, chatID, msgID, "m:mod")
			return
		}
		if err := a.bridge.UpdateAndApply(ctx, false, func(m map[string]any) error {
			m["llm_model"] = modelID
			return nil
		}); err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
			return
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "LLM модель -> "+modelID, false)
		a.renderAndEditMenu(ctx, chatID, msgID, "m:br")
		return
	}

	// Bridge SmartTube package select: sp:<0..3>
	if strings.HasPrefix(data, "sp:") {
		a.handleSmartTubePkgCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "sp:"))
		return
	}

	// Bridge toggles: bt:<key>
	if strings.HasPrefix(data, "bt:") {
		a.handleBridgeToggleCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "bt:"))
		return
	}

	// Routing rule views/edits: r:<i>, rt:<i>, rr:<i>
	if strings.HasPrefix(data, "r:") {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "", false)
		a.renderRuleDetail(ctx, chatID, msgID, strings.TrimPrefix(data, "r:"))
		return
	}
	if strings.HasPrefix(data, "rt:") {
		a.handleRuleToggleCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "rt:"))
		return
	}
	if strings.HasPrefix(data, "rr:") {
		a.handleRuleRouteCycleCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "rr:"))
		return
	}

	// System service actions & restore confirmations
	if strings.HasPrefix(data, "sys:") {
		a.handleSystemActionCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "sys:"))
		return
	}
	if strings.HasPrefix(data, "y:") {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "", false)
		a.renderConfirmPrompt(ctx, chatID, msgID, strings.TrimPrefix(data, "y:"))
		return
	}
	if strings.HasPrefix(data, "c:") {
		a.handleConfirmedRestoreCB(ctx, cq, chatID, msgID, strings.TrimPrefix(data, "c:"))
		return
	}

	_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Неизвестная кнопка: "+data, false)
}

func (a *BotApp) handleBrightnessCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, arg string) {
	switch arg {
	case "bake":
		st := a.station.GetStatus(ctx)
		if !st.Reachable || st.BrightnessFB4 < 0 {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Не удалось прочитать текущую яркость fb4", true)
			return
		}
		mode, err := a.station.BakeBrightnessToODM(ctx, st.BrightnessFB4)
		if err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
			return
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, fmt.Sprintf("Запечено %d в /odm (%s)", st.BrightnessFB4, mode), true)
	case "rst":
		got, err := a.station.ResetTmpBrightness(ctx)
		if err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
			return
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, fmt.Sprintf("Сброшено к /odm (%d)", got), false)
	default:
		v, err := strconv.Atoi(arg)
		if err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Некорректное значение", true)
			return
		}
		got, err := a.station.SetBrightnessInstant(ctx, v)
		if err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
			return
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, fmt.Sprintf("fb4 = %d / 200", got), false)
	}
	a.renderAndEditMenu(ctx, chatID, msgID, "m:disp")
}

func (a *BotApp) handleClockCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, arg string) {
	var key string
	switch arg {
	case "en":
		key = "clockEnabled"
	case "on":
		key = "isClockOn"
	case "dnd":
		key = "showClockDuringDnd"
	default:
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Неизвестный флаг", true)
		return
	}
	st := a.station.GetStatus(ctx)
	cur := false
	if st.ClockState != nil {
		if b, ok := st.ClockState[key].(bool); ok {
			cur = b
		}
	}
	if _, err := a.station.SetClockFlag(ctx, key, !cur); err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
		return
	}
	_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, fmt.Sprintf("%s -> %v (применится при перезапуске SDK)", key, !cur), false)
	a.renderAndEditMenu(ctx, chatID, msgID, "m:disp")
}

func (a *BotApp) handleVolumeCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, arg string) {
	if strings.HasPrefix(arg, "=") {
		v, err := strconv.Atoi(strings.TrimPrefix(arg, "="))
		if err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Ошибка числа", true)
			return
		}
		m3, m4, err := a.station.SetVolume(ctx, v, v)
		if err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
			return
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, fmt.Sprintf("Громкость: %d / %d", m3, m4), false)
		a.renderAndEditMenu(ctx, chatID, msgID, "m:aud")
		return
	}

	delta, err := strconv.Atoi(arg)
	if err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Ошибка шага", true)
		return
	}
	cur3, cur4, err := a.station.GetVolume(ctx)
	if err != nil || cur3 < 0 {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Не удалось получить громкость", true)
		return
	}
	if cur4 < 0 {
		cur4 = cur3
	}
	m3, m4, err := a.station.SetVolume(ctx, cur3+delta, cur4+delta)
	if err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
		return
	}
	_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, fmt.Sprintf("Громкость: %d / %d", m3, m4), false)
	a.renderAndEditMenu(ctx, chatID, msgID, "m:aud")
}

func (a *BotApp) handleMediaCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, arg string) {
	var cmd string
	switch arg {
	case "play", "pause", "next", "prev":
		cmd = arg
	case "r30":
		cmd = "seek -30s"
	case "f30":
		cmd = "seek +30s"
	case "f300":
		cmd = "seek +300s"
	default:
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Неизвестная команда плеера", true)
		return
	}
	resp, err := a.station.MediaCmd(ctx, cmd)
	if err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
		return
	}
	_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, cmd+" -> "+resp, false)
	a.renderAndEditMenu(ctx, chatID, msgID, "m:tv")
}

func (a *BotApp) handleDraftActionCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, act string) {
	switch act {
	case "reset":
		if err := a.draft.Clear(); err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
			return
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Черновик очищен", false)
		a.renderAndEditMenu(ctx, chatID, msgID, "m:home")
	case "diff":
		changes := a.draft.Snapshot()
		if len(changes) == 0 {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Черновик пуст", false)
			return
		}
		var sb strings.Builder
		sb.WriteString("<b>📝 Черновик user_config.dat</b>\n\n")
		for _, ch := range changes {
			sb.WriteString(fmt.Sprintf("• <code>%s</code> = <code>%s</code>\n", Esc(strings.Join(ch.Path, ".")), Esc(string(ch.Value))))
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "", false)
		kb := InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "✅ Применить и перезагр. SDK", CallbackData: "uc:apply"},
				{Text: "🗑 Сброс", CallbackData: "uc:reset"},
			},
			{{Text: "🏠 Меню", CallbackData: "m:home"}},
		}}
		_ = a.tg.EditMessageText(ctx, chatID, msgID, sb.String(), &kb)
	case "apply":
		changes := a.draft.Snapshot()
		if len(changes) == 0 {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Нечего применять — черновик пуст", false)
			return
		}
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Записываю user_config.dat и перезагружаю SDK...", false)
		newPID, err := a.station.CommitUserConfig(ctx, changes)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Ошибка применения user_config.dat:\n<code>"+Esc(err.Error())+"</code>\nЧерновик сохранён.", backHomeMarkup())
			return
		}
		_ = a.draft.Clear()
		_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("✅ Применено <b>%d</b> изменений в <code>user_config.dat</code>!\n<code>com.yandex.io.sdk</code> перезапущен (PID: <code>%s</code>).", len(changes), Esc(newPID)), backHomeMarkup())
		a.renderAndEditMenu(ctx, chatID, msgID, "m:home")
	}
}

func (a *BotApp) loadEffectiveUserConfig(ctx context.Context) (merged map[string]any, dirty map[string]bool, err error) {
	doc, err := a.station.FetchUserConfig(ctx)
	if err != nil {
		return nil, nil, err
	}
	merged, dirty = a.draft.Overlay(doc.Parsed)
	return merged, dirty, nil
}

type ucToggleDef struct {
	Path       []string
	ReturnMenu string
	Values     [2]string // empty => bool toggle
}

var ucToggleMap = map[string]ucToggleDef{
	"led_auto":  {Path: []string{"device_config", "led", "brightness", "auto"}, ReturnMenu: "m:disp"},
	"eqv_auto":  {Path: []string{"device_config", "led", "music_equalizer_visualization", "auto"}, ReturnMenu: "m:disp"},
	"t_fmt":     {Path: []string{"device_config", "led", "time_visualization", "format"}, ReturnMenu: "m:disp", Values: [2]string{"24h", "12h"}},
	"t_sz":      {Path: []string{"device_config", "led", "time_visualization", "size"}, ReturnMenu: "m:disp", Values: [2]string{"large", "small"}},
	"spotter":   {Path: []string{"account_config", "spotter"}, ReturnMenu: "m:cmd", Values: [2]string{"alisa", "yandex"}},
	"sh_mic":    {Path: []string{"account_config", "alwaysOnMicForShortcuts"}, ReturnMenu: "m:cmd"},
	"smart_act": {Path: []string{"account_config", "smartActivation"}, ReturnMenu: "m:cmd"},
	"jingle":    {Path: []string{"account_config", "jingle"}, ReturnMenu: "m:cmd"},
	"proact":    {Path: []string{"account_config", "aliceProactivity"}, ReturnMenu: "m:cmd"},
	"adap_vol":  {Path: []string{"account_config", "aliceAdaptiveVolume", "enabled"}, ReturnMenu: "m:cmd"},
	"child_vad": {Path: []string{"account_config", "enableChildVad"}, ReturnMenu: "m:cmd"},
	"hist":      {Path: []string{"account_config", "saveHistoryUsage"}, ReturnMenu: "m:cmd"},
	"cs_tvn":    {Path: []string{"account_config", "enabledCommandSpotters", "tv", "navigation"}, ReturnMenu: "m:cmd"},
	"cs_tvh":    {Path: []string{"account_config", "enabledCommandSpotters", "tv", "backToHome"}, ReturnMenu: "m:cmd"},
	"cs_mp":     {Path: []string{"account_config", "enabledCommandSpotters", "music", "playAndPause"}, ReturnMenu: "m:cmd"},
	"cs_mn":     {Path: []string{"account_config", "enabledCommandSpotters", "music", "navigation"}, ReturnMenu: "m:cmd"},
	"cs_mv":     {Path: []string{"account_config", "enabledCommandSpotters", "music", "volume"}, ReturnMenu: "m:cmd"},
	"cs_mf":     {Path: []string{"account_config", "enabledCommandSpotters", "music", "feedback"}, ReturnMenu: "m:cmd"},
	"cs_mb":     {Path: []string{"account_config", "enabledCommandSpotters", "music", "bluetooth"}, ReturnMenu: "m:cmd"},
	"cs_shl":    {Path: []string{"account_config", "enabledCommandSpotters", "smartHome", "light"}, ReturnMenu: "m:cmd"},
	"cs_sht":    {Path: []string{"account_config", "enabledCommandSpotters", "smartHome", "tv"}, ReturnMenu: "m:cmd"},
	"cs_ca":     {Path: []string{"account_config", "enabledCommandSpotters", "call", "answer"}, ReturnMenu: "m:cmd"},
	"eq_en":     {Path: []string{"device_config", "equalizer", "enabled"}, ReturnMenu: "m:aud"},
	"eq_sm":     {Path: []string{"device_config", "equalizer", "smartEnabled"}, ReturnMenu: "m:aud"},
	"xfade":     {Path: []string{"account_config", "audio_player", "crossfadeEnabled"}, ReturnMenu: "m:aud"},
	"hdmi_aud":  {Path: []string{"device_config", "hdmiAudio"}, ReturnMenu: "m:aud"},
	"sdk_hdmi":  {Path: []string{"system_config", "com.yandex.io.sdk", "hdmiAudioEnabled"}, ReturnMenu: "m:aud"},
	"dnd_en":    {Path: []string{"device_config", "dndMode", "enabled"}, ReturnMenu: "m:aud"},
	"dnd_calls": {Path: []string{"device_config", "dndMode", "features", "allowIncomingCalls"}, ReturnMenu: "m:aud"},
	"dnd_clk":   {Path: []string{"device_config", "dndMode", "platformSettings", "showClock"}, ReturnMenu: "m:aud"},
	"stby":      {Path: []string{"device_config", "standby", "deepStandbyEnabled"}, ReturnMenu: "m:aud"},
}

func (a *BotApp) handleUserConfigToggleCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, key string) {
	def, ok := ucToggleMap[key]
	if !ok {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Неизвестный параметр: "+key, true)
		return
	}
	merged, _, err := a.loadEffectiveUserConfig(ctx)
	if err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Станция недоступна: "+err.Error(), true)
		return
	}

	var nextVal any
	if def.Values[0] != "" {
		cur, _ := GetString(merged, def.Path...)
		if cur == def.Values[0] {
			nextVal = def.Values[1]
		} else {
			nextVal = def.Values[0]
		}
	} else {
		cur, _ := GetBool(merged, def.Path...)
		nextVal = !cur
	}

	if err := a.draft.Set(def.Path, nextVal); err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
		return
	}
	_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, fmt.Sprintf("В черновик: %s = %v", def.Path[len(def.Path)-1], nextVal), false)
	a.renderAndEditMenu(ctx, chatID, msgID, def.ReturnMenu)
}

func (a *BotApp) handleSpotterPresetCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, preset string) {
	items := []struct {
		Path []string
		Val  bool
	}{
		{[]string{"account_config", "enabledCommandSpotters", "tv", "navigation"}, preset == "std"},
		{[]string{"account_config", "enabledCommandSpotters", "tv", "backToHome"}, preset == "std"},
		{[]string{"account_config", "enabledCommandSpotters", "music", "playAndPause"}, preset == "std"},
		{[]string{"account_config", "enabledCommandSpotters", "music", "navigation"}, preset == "std"},
		{[]string{"account_config", "enabledCommandSpotters", "music", "volume"}, preset == "std"},
		{[]string{"account_config", "enabledCommandSpotters", "music", "feedback"}, preset == "std"},
		{[]string{"account_config", "enabledCommandSpotters", "music", "bluetooth"}, preset == "std"},
		{[]string{"account_config", "enabledCommandSpotters", "smartHome", "light"}, preset == "std"},
		{[]string{"account_config", "enabledCommandSpotters", "smartHome", "tv"}, preset == "std"},
		{[]string{"account_config", "enabledCommandSpotters", "call", "answer"}, false},
	}
	for _, it := range items {
		if err := a.draft.Set(it.Path, it.Val); err != nil {
			_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
			return
		}
	}
	if preset == "std" {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Стандартные команды добавлены в черновик", false)
	} else {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Все короткие команды выключены в черновике", false)
	}
	a.renderAndEditMenu(ctx, chatID, msgID, "m:cmd")
}

func (a *BotApp) handleSmartTubePkgCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, idxStr string) {
	pkgs := []string{"", "org.smarttube.stable", "org.smarttube.beta", "com.teamsmart.videomanager.tv"}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(pkgs) {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Некорректный индекс пакета", true)
		return
	}
	pkg := pkgs[idx]
	err = a.bridge.UpdateAndApply(ctx, false, func(m map[string]any) error {
		return SetBridgeDotPath(m, "smarttube.package", pkg)
	})
	if err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
		return
	}
	label := pkg
	if label == "" {
		label = "auto"
	}
	_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "SmartTube package -> "+label, false)
	a.renderAndEditMenu(ctx, chatID, msgID, "m:tv")
}

func (a *BotApp) handleBridgeToggleCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, key string) {
	var dotPath string
	var returnMenu string
	var routingOnly bool
	var cycle []string

	switch key {
	case "lar":
		dotPath, returnMenu = "listen_after_reply", "m:br"
	case "dctx":
		dotPath, returnMenu = "dialogue_context.enabled", "m:br"
	case "prog":
		dotPath, returnMenu = "local_progress.enabled", "m:br"
	case "at_en":
		dotPath, returnMenu = "assistant_tools.enabled", "m:tools"
	case "at_web":
		dotPath, returnMenu = "assistant_tools.web", "m:tools"
	case "at_hist":
		dotPath, returnMenu = "assistant_tools.history", "m:tools"
	case "at_time":
		dotPath, returnMenu = "assistant_tools.time", "m:tools"
	case "at_snd":
		dotPath, returnMenu = "assistant_tools.play_sound", "m:tools"
	case "at_upd":
		dotPath, returnMenu = "assistant_tools.updates", "m:tools"
	case "at_cfg":
		dotPath, returnMenu = "assistant_tools.configuration", "m:tools"
	case "st_dis":
		dotPath, returnMenu = "smarttube.disabled", "m:tv"
	case "ref":
		dotPath, returnMenu, routingOnly = "routing.refusal.enabled", "m:rules", true
	case "def_rt":
		dotPath, returnMenu, routingOnly = "routing.default_route", "m:rules", true
		cycle = []string{"yandex", "llm", "yandex_with_fallback"}
	default:
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Неизвестный флаг bridge: "+key, true)
		return
	}

	var newVal any
	err := a.bridge.UpdateAndApply(ctx, routingOnly, func(m map[string]any) error {
		cur, _ := GetBridgeDotPath(m, dotPath)
		if len(cycle) > 0 {
			s, _ := cur.(string)
			next := cycle[0]
			for i, c := range cycle {
				if c == s {
					next = cycle[(i+1)%len(cycle)]
					break
				}
			}
			newVal = next
		} else {
			b, _ := cur.(bool)
			newVal = !b
		}
		return SetBridgeDotPath(m, dotPath, newVal)
	})
	if err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
		return
	}
	_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, fmt.Sprintf("%s -> %v", dotPath, newVal), false)
	a.renderAndEditMenu(ctx, chatID, msgID, returnMenu)
}

func (a *BotApp) handleRuleToggleCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, idxStr string) {
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Ошибка индекса", true)
		return
	}
	var newDisabled bool
	err = a.bridge.UpdateAndApply(ctx, true, func(m map[string]any) error {
		cur, ok := GetBridgeDotPath(m, fmt.Sprintf("routing.rules.%d.disabled", idx))
		b := false
		if ok {
			b, _ = cur.(bool)
		}
		newDisabled = !b
		return SetBridgeDotPath(m, fmt.Sprintf("routing.rules.%d.disabled", idx), newDisabled)
	})
	if err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
		return
	}
	_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, fmt.Sprintf("Правило #%d disabled=%v", idx, newDisabled), false)
	a.renderRuleDetail(ctx, chatID, msgID, idxStr)
}

func (a *BotApp) handleRuleRouteCycleCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, idxStr string) {
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Ошибка индекса", true)
		return
	}
	routes := []string{"llm", "yandex", "yandex_with_fallback"}
	var nextRoute string
	err = a.bridge.UpdateAndApply(ctx, true, func(m map[string]any) error {
		cur, _ := GetBridgeDotPath(m, fmt.Sprintf("routing.rules.%d.route", idx))
		s, _ := cur.(string)
		nextRoute = routes[0]
		for i, r := range routes {
			if r == s {
				nextRoute = routes[(i+1)%len(routes)]
				break
			}
		}
		return SetBridgeDotPath(m, fmt.Sprintf("routing.rules.%d.route", idx), nextRoute)
	})
	if err != nil {
		_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, err.Error(), true)
		return
	}
	_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, fmt.Sprintf("Правило #%d -> %s", idx, nextRoute), false)
	a.renderRuleDetail(ctx, chatID, msgID, idxStr)
}

func (a *BotApp) handleSystemActionCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, act string) {
	_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Выполняю "+act+"...", false)
	var out string
	var err error
	switch act {
	case "br":
		err = a.bridge.Restart(ctx)
		out = "quasar-bridge перезапущен"
	case "sdk", "mediadaemon", "watchdog", "adbd":
		out, err = a.station.RestartService(ctx, act)
	default:
		err = errors.New("неизвестное действие: " + act)
	}
	if err != nil {
		_, _ = a.tg.SendMessage(ctx, chatID, "❌ Ошибка ("+Esc(act)+"): <code>"+Esc(err.Error())+"</code>", nil)
	} else {
		_, _ = a.tg.SendMessage(ctx, chatID, "✅ "+Esc(out), nil)
	}
	a.renderAndEditMenu(ctx, chatID, msgID, "m:sys")
}

func (a *BotApp) renderConfirmPrompt(ctx context.Context, chatID, msgID int64, target string) {
	var title string
	switch target {
	case "rst_ucb":
		title = "Восстановить <code>user_config.dat</code> из <code>.bak_bot</code> и перезагрузить SDK?"
	case "rst_uco":
		title = "Восстановить <code>user_config.dat</code> из <code>.orig</code> (исходный) и перезагрузить SDK?"
	case "rst_br":
		title = "Восстановить <code>config.json</code> Quasar Bridge из <code>.botbak</code> и перезапустить?"
	default:
		title = "Подтвердите действие: " + target
	}
	kb := InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{
			{Text: "✅ Да, выполнить", CallbackData: "c:" + target},
			{Text: "❌ Отмена", CallbackData: "m:sys"},
		},
	}}
	_ = a.tg.EditMessageText(ctx, chatID, msgID, "⚠️ <b>Подтверждение</b>\n\n"+title, &kb)
}

func (a *BotApp) handleConfirmedRestoreCB(ctx context.Context, cq *TGCallbackQuery, chatID, msgID int64, target string) {
	_ = a.tg.AnswerCallbackQuery(ctx, cq.ID, "Выполняю откат...", false)
	var msg string
	var err error
	switch target {
	case "rst_ucb":
		var pid string
		pid, err = a.station.RestoreUserConfigBackup(ctx, ".bak_bot")
		if err == nil {
			_ = a.draft.Clear()
			msg = "✅ <code>user_config.dat</code> восстановлен из <code>.bak_bot</code> (SDK PID: " + pid + ")"
		}
	case "rst_uco":
		var pid string
		pid, err = a.station.RestoreUserConfigBackup(ctx, ".orig")
		if err == nil {
			_ = a.draft.Clear()
			msg = "✅ <code>user_config.dat</code> восстановлен из <code>.orig</code> (SDK PID: " + pid + ")"
		}
	case "rst_br":
		err = a.bridge.RestoreBackup(ctx)
		if err == nil {
			msg = "✅ <code>config.json</code> восстановлен из <code>.botbak</code> и перезапущен"
		}
	default:
		err = errors.New("неизвестный откат")
	}
	if err != nil {
		_, _ = a.tg.SendMessage(ctx, chatID, "❌ Ошибка отката: <code>"+Esc(err.Error())+"</code>", nil)
	} else {
		_, _ = a.tg.SendMessage(ctx, chatID, msg, nil)
	}
	a.renderAndEditMenu(ctx, chatID, msgID, "m:sys")
}

func (a *BotApp) startPrompt(ctx context.Context, chatID, msgID int64, kind, extra string) {
	var text, ret string
	switch kind {
	case "fb4":
		text, ret = "🔆 Отправьте яркость LED дисплея <code>fb4</code> (целое число от <b>0</b> до <b>200</b>):", "m:disp"
	case "ledv":
		text, ret = "🔆 Отправьте значение <code>device_config.led.brightness.value</code> в черновик (от <b>0.0</b> до <b>1.0</b>, например <code>0.2</code>):", "m:disp"
	case "eqvs":
		text, ret = "🎨 Отправьте <code>music_equalizer_visualization.style</code> в черновик (например <code>barsCenter</code>):", "m:disp"
	case "vol":
		text, ret = "🔊 Отправьте громкость (Музыка + Будильник) от <b>0</b> до <b>100</b>:", "m:aud"
	case "vol3":
		text, ret = "🎵 Отправьте громкость <b>Музыки</b> (stream 3) от <b>0</b> до <b>100</b>:", "m:aud"
	case "vol4":
		text, ret = "⏰ Отправьте громкость <b>Будильника</b> (stream 4) от <b>0</b> до <b>100</b>:", "m:aud"
	case "eqbands":
		text, ret = "🎛 Отправьте 5 полос эквалайзера (от -12 до 12) через пробел или JSON, например:\n<code>5 -6 0 0 0</code>", "m:aud"
	case "dnd_t":
		text, ret = "🌙 Отправьте время начала и конца DND через пробел, например:\n<code>23:00:00+0500 09:00:00+0500</code>", "m:aud"
	case "seek":
		text, ret = "🎯 Отправьте позицию или смещение перемотки (например <code>+30s</code>, <code>-1m</code>, <code>12:30</code>, <code>300s</code>):", "m:tv"
	case "st_del":
		text, ret = "⏱ Отправьте задержку <code>smarttube.intent_delay_ms</code> в миллисекундах (например <code>700</code>):", "m:tv"
	case "b_mod":
		text, ret = "🤖 Отправьте имя модели <code>llm_model</code> (например <code>gemini-3.8-flash-low</code>):", "m:br"
	case "b_tok":
		text, ret = "📝 Отправьте <code>llm_max_output_tokens</code> (например <code>4096</code>):", "m:br"
	case "b_dctx":
		text, ret = "💬 Отправьте <code>ttl_seconds max_turns</code> через пробел (например <code>300 6</code>):", "m:br"
	case "at_tz":
		text, ret = "🌍 Отправьте IANA-таймзону <code>assistant_tools.timezone</code> (например <code>Europe/Moscow</code>):", "m:tools"
	case "route":
		text, ret = "🧪 Отправьте голосовую фразу для проверки маршрутизатора Quasar Bridge:", "m:rules"
	case "rule_prompt":
		text, ret = fmt.Sprintf("📝 Отправьте новый <code>system_prompt</code> для правила #%s (или <code>-</code> чтобы очистить):", Esc(extra)), "r:"+extra
	case "uc_get":
		text, ret = "🔍 Отправьте dot-путь в <code>user_config.dat</code> (например <code>device_config.led</code> или <code>system_config.com.yandex.io.sdk</code>):", "m:raw"
	case "uc_set":
		text, ret = "✏️ Отправьте <code>путь = значение</code> для <code>user_config.dat</code>, например:\n<code>device_config.standby.deepStandbyTimeoutMinutes = 180</code>", "m:raw"
	case "br_get":
		text, ret = "🔍 Отправьте dot-путь в <code>config.json</code> Quasar Bridge (например <code>routing.refusal</code> или <code>tts_replacements</code>):", "m:raw"
	case "br_set":
		text, ret = "✏️ Отправьте <code>путь = значение</code> для Quasar Bridge, например:\n<code>llm_max_output_tokens = 2048</code>", "m:raw"
	default:
		return
	}
	a.setPending(chatID, pendingInput{Kind: kind, ReturnMenu: ret, Extra: extra})
	kb := InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "❌ Отмена", CallbackData: ret}},
	}}
	_, _ = a.tg.SendMessage(ctx, chatID, text, &kb)
}

func (a *BotApp) handlePendingInput(ctx context.Context, chatID int64, p pendingInput, raw string) {
	a.clearPending(chatID)
	raw = strings.TrimSpace(raw)

	switch p.Kind {
	case "fb4":
		v, err := strconv.Atoi(raw)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Нужна цифра 0..200.", backToMarkup(p.ReturnMenu, "↩️ Назад"))
			return
		}
		got, err := a.station.SetBrightnessInstant(ctx, v)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup(p.ReturnMenu, "↩️ Назад"))
			return
		}
		_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("🔆 <code>fb4</code> установлен: <b>%d / 200</b>", got), backToMarkup("m:disp", "🔆 Дисплей"))

	case "ledv":
		f, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", "."), 64)
		if err != nil || f < 0 || f > 1 {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Введите число от 0.0 до 1.0.", backToMarkup("m:disp", "↩️ Назад"))
			return
		}
		_ = a.draft.Set([]string{"device_config", "led", "brightness", "value"}, json.Number(strconv.FormatFloat(f, 'f', -1, 64)))
		a.sendMenu(ctx, chatID, "m:disp")

	case "eqvs":
		_ = a.draft.Set([]string{"device_config", "led", "music_equalizer_visualization", "style"}, raw)
		a.sendMenu(ctx, chatID, "m:disp")

	case "vol", "vol3", "vol4":
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 || v > 100 {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Введите число от 0 до 100.", backToMarkup("m:aud", "↩️ Назад"))
			return
		}
		s3, s4 := v, v
		if p.Kind == "vol3" {
			s4 = -1
		} else if p.Kind == "vol4" {
			s3 = -1
		}
		m3, m4, err := a.station.SetVolume(ctx, s3, s4)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:aud", "↩️ Назад"))
			return
		}
		_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("🔊 Громкость: Музыка <b>%d</b>, Будильник <b>%d</b>", m3, m4), backToMarkup("m:aud", "🔊 Звук"))

	case "eqbands":
		bands, err := ParseEQBands(raw)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:aud", "↩️ Назад"))
			return
		}
		_ = a.draft.Set([]string{"device_config", "equalizer", "custom_preset_bands"}, bands)
		_ = a.draft.Set([]string{"device_config", "equalizer", "active_preset_id"}, "custom")
		a.sendMenu(ctx, chatID, "m:aud")

	case "dnd_t":
		fields := strings.Fields(raw)
		if len(fields) != 2 {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Укажите два времени через пробел, например: <code>23:00:00+0500 09:00:00+0500</code>", backToMarkup("m:aud", "↩️ Назад"))
			return
		}
		_ = a.draft.Set([]string{"device_config", "dndMode", "starts"}, fields[0])
		_ = a.draft.Set([]string{"device_config", "dndMode", "ends"}, fields[1])
		a.sendMenu(ctx, chatID, "m:aud")

	case "seek":
		cmd, err := ParseSeekInput(raw)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:tv", "📺 ТВ"))
			return
		}
		a.execMediaCommandAndReply(ctx, chatID, cmd, "🎯 Перемотка "+raw)

	case "st_del":
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 || v > 15000 {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Укажите число 0..15000 мс.", backToMarkup("m:tv", "↩️ Назад"))
			return
		}
		err = a.bridge.UpdateAndApply(ctx, false, func(m map[string]any) error {
			return SetBridgeDotPath(m, "smarttube.intent_delay_ms", json.Number(strconv.Itoa(v)))
		})
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:tv", "↩️ Назад"))
			return
		}
		a.sendMenu(ctx, chatID, "m:tv")

	case "b_mod":
		err := a.bridge.UpdateAndApply(ctx, false, func(m map[string]any) error {
			m["llm_model"] = raw
			return nil
		})
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:br", "↩️ Назад"))
			return
		}
		a.sendMenu(ctx, chatID, "m:br")

	case "b_tok":
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 65536 {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Укажите целое число 1..65536.", backToMarkup("m:br", "↩️ Назад"))
			return
		}
		err = a.bridge.UpdateAndApply(ctx, false, func(m map[string]any) error {
			m["llm_max_output_tokens"] = json.Number(strconv.Itoa(v))
			return nil
		})
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:br", "↩️ Назад"))
			return
		}
		a.sendMenu(ctx, chatID, "m:br")

	case "b_dctx":
		f := strings.Fields(raw)
		if len(f) != 2 {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Укажите <code>ttl_seconds max_turns</code>, например <code>300 6</code>.", backToMarkup("m:br", "↩️ Назад"))
			return
		}
		ttl, err1 := strconv.Atoi(f[0])
		turns, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || ttl < 0 || turns < 0 {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Некорректные числа.", backToMarkup("m:br", "↩️ Назад"))
			return
		}
		err := a.bridge.UpdateAndApply(ctx, false, func(m map[string]any) error {
			if err := SetBridgeDotPath(m, "dialogue_context.ttl_seconds", json.Number(strconv.Itoa(ttl))); err != nil {
				return err
			}
			return SetBridgeDotPath(m, "dialogue_context.max_turns", json.Number(strconv.Itoa(turns)))
		})
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:br", "↩️ Назад"))
			return
		}
		a.sendMenu(ctx, chatID, "m:br")

	case "at_tz":
		err := a.bridge.UpdateAndApply(ctx, false, func(m map[string]any) error {
			return SetBridgeDotPath(m, "assistant_tools.timezone", raw)
		})
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:tools", "↩️ Назад"))
			return
		}
		a.sendMenu(ctx, chatID, "m:tools")

	case "route":
		out, err := a.bridge.ExplainRoute(ctx, raw)
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:rules", "🛤 Правила"))
			return
		}
		_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("🧪 <b>Тест маршрута:</b> <code>%s</code>\n\n%s", Esc(raw), wrapPre(out, 3000)), backToMarkup("m:rules", "🛤 Правила"))

	case "rule_prompt":
		val := raw
		if val == "-" {
			val = ""
		}
		idx, err := strconv.Atoi(p.Extra)
		if err != nil {
			return
		}
		err = a.bridge.UpdateAndApply(ctx, true, func(m map[string]any) error {
			return SetBridgeDotPath(m, fmt.Sprintf("routing.rules.%d.system_prompt", idx), val)
		})
		if err != nil {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:rules", "🛤 Правила"))
			return
		}
		text, kb := a.buildRuleDetail(ctx, p.Extra)
		_, _ = a.tg.SendMessage(ctx, chatID, text, &kb)

	case "uc_get":
		a.replyGetUC(ctx, chatID, raw)
	case "uc_set":
		a.replySetUC(ctx, chatID, raw, "m:raw")
	case "br_get":
		a.replyGetBR(ctx, chatID, raw)
	case "br_set":
		a.replySetBR(ctx, chatID, raw, "m:raw")
	}
}

func splitPathAndVal(arg string) (path string, val string, ok bool) {
	if p, v, hasEq := strings.Cut(arg, "="); hasEq {
		return strings.TrimSpace(p), strings.TrimSpace(v), strings.TrimSpace(p) != "" && strings.TrimSpace(v) != ""
	}
	if p, v, hasSp := strings.Cut(strings.TrimSpace(arg), " "); hasSp {
		return strings.TrimSpace(p), strings.TrimSpace(v), strings.TrimSpace(p) != "" && strings.TrimSpace(v) != ""
	}
	return "", "", false
}

func (a *BotApp) replyGetUC(ctx context.Context, chatID int64, dotPath string) {
	merged, _, err := a.loadEffectiveUserConfig(ctx)
	if err != nil {
		_, _ = a.tg.SendMessage(ctx, chatID, "❌ Станция недоступна: <code>"+Esc(err.Error())+"</code>", backToMarkup("m:raw", "🧬 JSON"))
		return
	}
	var target any = merged
	if dotPath != "" {
		segs := ResolveDotPath(merged, dotPath)
		v, ok := GetAtPath(merged, segs...)
		if !ok {
			_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("🔍 Ключ <code>%s</code> не найден.", Esc(dotPath)), backToMarkup("m:raw", "🧬 JSON"))
			return
		}
		target = v
	} else {
		dotPath = "(корень, верхние разделы)"
		keys := make([]string, 0, len(merged))
		for k := range merged {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		target = keys
	}
	js, _ := marshalPrettyNoEscape(target)
	_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("🔍 <b>user_config.dat</b> → <code>%s</code>\n\n%s", Esc(dotPath), wrapPre(string(js), 3200)), backToMarkup("m:raw", "🧬 JSON"))
}

func (a *BotApp) replySetUC(ctx context.Context, chatID int64, arg, retMenu string) {
	dotPath, valStr, ok := splitPathAndVal(arg)
	if !ok {
		_, _ = a.tg.SendMessage(ctx, chatID, "Использование: <code>/set_uc путь значение</code> (или через <code>=</code>)", backToMarkup(retMenu, "🧬 JSON"))
		return
	}
	merged, _, err := a.loadEffectiveUserConfig(ctx)
	if err != nil {
		_, _ = a.tg.SendMessage(ctx, chatID, "❌ Станция недоступна: <code>"+Esc(err.Error())+"</code>", nil)
		return
	}
	segs := ResolveDotPath(merged, dotPath)
	for _, s := range segs {
		if s == "uniProxyUrl" || s == "uniProxyQuicUrl" {
			_, _ = a.tg.SendMessage(ctx, chatID, "❌ Для <code>uniProxyUrl</code> используйте переключатель потока (Bridge/Яндекс) в меню <b>🤖 Bridge</b>.", backToMarkup("m:br", "🤖 Bridge"))
			return
		}
	}
	val := ParseUserValue(valStr)
	if err := a.draft.Set(segs, val); err != nil {
		_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), nil)
		return
	}
	rawVal, _ := marshalCompactNoEscape(val)
	kb := InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: fmt.Sprintf("✅ Применить и перезагр. SDK (%d)", len(a.draft.Snapshot())), CallbackData: "uc:apply"}},
		{{Text: "🧬 JSON", CallbackData: "m:raw"}, {Text: "🏠 Меню", CallbackData: "m:home"}},
	}}
	_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("📝 Добавлено в черновик:\n<code>%s</code> = <code>%s</code>", Esc(strings.Join(segs, ".")), Esc(string(rawVal))), &kb)
}

func (a *BotApp) replyGetBR(ctx context.Context, chatID int64, dotPath string) {
	data, err := a.bridge.ReadRawMap()
	if err != nil {
		_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup("m:raw", "🧬 JSON"))
		return
	}
	v, ok := GetBridgeDotPath(data, dotPath)
	if !ok {
		_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("🔍 Ключ <code>%s</code> не найден в config.json.", Esc(dotPath)), backToMarkup("m:raw", "🧬 JSON"))
		return
	}
	js, _ := marshalPrettyNoEscape(v)
	title := dotPath
	if title == "" {
		title = "(весь config.json)"
	}
	_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("🔍 <b>Quasar Bridge</b> → <code>%s</code>\n\n%s", Esc(title), wrapPre(string(js), 3200)), backToMarkup("m:raw", "🧬 JSON"))
}

func (a *BotApp) replySetBR(ctx context.Context, chatID int64, arg, retMenu string) {
	dotPath, valStr, ok := splitPathAndVal(arg)
	if !ok {
		_, _ = a.tg.SendMessage(ctx, chatID, "Использование: <code>/set_br путь значение</code>", backToMarkup(retMenu, "🧬 JSON"))
		return
	}
	val := ParseUserValue(valStr)
	routingOnly := strings.HasPrefix(dotPath, "routing.")
	err := a.bridge.UpdateAndApply(ctx, routingOnly, func(m map[string]any) error {
		return SetBridgeDotPath(m, dotPath, val)
	})
	if err != nil {
		_, _ = a.tg.SendMessage(ctx, chatID, "❌ "+Esc(err.Error()), backToMarkup(retMenu, "🧬 JSON"))
		return
	}
	rawVal, _ := marshalCompactNoEscape(val)
	modeNote := "рестарт quasar-bridge"
	if routingOnly {
		modeNote = "горячая перезагрузка SIGHUP"
	}
	_, _ = a.tg.SendMessage(ctx, chatID, fmt.Sprintf("✅ Применено (%s):\n<code>%s</code> = <code>%s</code>", modeNote, Esc(dotPath), Esc(string(rawVal))), backToMarkup(retMenu, "🧬 JSON"))
}

// --- Menus & Builders ---

func (a *BotApp) sendMenu(ctx context.Context, chatID int64, menu string) {
	text, kb := a.buildMenu(ctx, chatID, menu)
	_, _ = a.tg.SendMessage(ctx, chatID, text, &kb)
}

func (a *BotApp) renderAndEditMenu(ctx context.Context, chatID, msgID int64, menu string) {
	if strings.HasPrefix(menu, "r:") {
		a.renderRuleDetail(ctx, chatID, msgID, strings.TrimPrefix(menu, "r:"))
		return
	}
	text, kb := a.buildMenu(ctx, chatID, menu)
	if err := a.tg.EditMessageText(ctx, chatID, msgID, text, &kb); err != nil {
		_, _ = a.tg.SendMessage(ctx, chatID, text, &kb)
	}
}

func (a *BotApp) buildMenu(ctx context.Context, chatID int64, menu string) (string, InlineKeyboardMarkup) {
	switch menu {
	case "m:disp":
		return a.buildDisplayMenu(ctx)
	case "m:dstyle":
		return a.buildDisplayStyleMenu(ctx)
	case "m:cmd":
		return a.buildCmdMenu(ctx)
	case "m:aud":
		return a.buildAudioMenu(ctx)
	case "m:eq":
		return a.buildEQPresetMenu(ctx)
	case "m:br":
		return a.buildBridgeMenu(ctx)
	case "m:mod":
		return a.buildModelsMenu(ctx, chatID)
	case "m:tools":
		return a.buildToolsMenu(ctx)
	case "m:rules":
		return a.buildRulesMenu(ctx)
	case "m:tv":
		return a.buildTVMenu(ctx)
	case "m:raw":
		return a.buildRawMenu(ctx)
	case "m:sys":
		return a.buildSystemMenu(ctx)
	default:
		return a.buildHomeMenu(ctx)
	}
}

func (a *BotApp) appendDraftRow(rows [][]InlineKeyboardButton) [][]InlineKeyboardButton {
	n := len(a.draft.Snapshot())
	if n == 0 {
		return rows
	}
	return append(rows, []InlineKeyboardButton{
		{Text: fmt.Sprintf("✅ Применить SDK (%d)", n), CallbackData: "uc:apply"},
		{Text: "👁 Изм.", CallbackData: "uc:diff"},
		{Text: "🗑 Сброс", CallbackData: "uc:reset"},
	})
}

func (a *BotApp) draftBannerText() string {
	n := len(a.draft.Snapshot())
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("\n📝 <b>Черновик user_config.dat:</b> %d изм. ожидают применения", n)
}

func boolIcon(b bool) string {
	if b {
		return "✅"
	}
	return "❌"
}

func dirtyMark(dirty map[string]bool, key string) string {
	if dirty != nil && dirty[key] {
		return "🔸"
	}
	return ""
}

func (a *BotApp) buildHomeMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	st := a.station.GetStatus(ctx)
	brData, brErr := a.bridge.ReadRawMap()
	bh, bhErr := a.bridge.Health(ctx)

	var sb strings.Builder
	sb.WriteString("🎛 <b>Управление Яндекс Станцией Макс</b>\n")

	if st.Reachable {
		brTxt := fmt.Sprintf("%d/200", st.BrightnessFB4)
		volTxt := fmt.Sprintf("🎵 %d / ⏰ %d", st.VolumeMusic, st.VolumeAlarm)
		clkTxt := "—"
		if st.ClockState != nil {
			en, _ := st.ClockState["clockEnabled"].(bool)
			on, _ := st.ClockState["isClockOn"].(bool)
			clkTxt = fmt.Sprintf("clk=%s scr=%s", boolIcon(en), boolIcon(on))
		}
		streamLabel := st.StreamMode
		if st.StreamMode == "yandex" && bhErr == nil && bh.Status == "ok" {
			streamLabel = "yandex → Bridge (DNS)"
		}
		sb.WriteString(fmt.Sprintf("\n📡 <b>Станция (%s):</b> онлайн | SDK: <code>%s</code> | SELinux: <code>%s</code> | /odm: <code>%s</code>",
			Esc(a.cfg.StationIP), Esc(st.SDKPID), Esc(st.SELinuxEnforce), Esc(st.ODMMountMode)))
		sb.WriteString(fmt.Sprintf("\n🔀 <b>Поток:</b> <code>%s</code> | 🔆 <b>LED:</b> <code>%s</code> | %s",
			Esc(streamLabel), Esc(brTxt), clkTxt))
		sb.WriteString(fmt.Sprintf("\n🔊 <b>Громкость:</b> %s", volTxt))
	} else {
		sb.WriteString(fmt.Sprintf("\n📡 <b>Станция (%s):</b> ⚠️ недоступна (%s)", Esc(a.cfg.StationIP), Esc(trimRunes(st.SSHError, 100))))
	}

	if st.Media.Reachable {
		title := st.Media.Title
		if title == "" {
			title = "—"
		}
		sb.WriteString(fmt.Sprintf("\n📺 <b>ТВ плеер:</b> %s — %s (%s/%s)",
			Esc(st.Media.StateLabel()), Esc(trimRunes(title, 45)), fmtMs(st.Media.PosMs), fmtMs(st.Media.DurationMs)))
	}

	if brErr == nil {
		mode, _ := GetString(brData, "mode")
		model, _ := GetString(brData, "llm_model")
		hStr := a.bridge.IsServiceActive(ctx)
		if bhErr == nil {
			hStr = fmt.Sprintf("%s (rules=%d)", bh.Status, bh.RoutingRules)
		}
		sb.WriteString(fmt.Sprintf("\n🤖 <b>Quasar Bridge:</b> %s | режим <code>%s</code> | LLM <code>%s</code>",
			Esc(hStr), Esc(mode), Esc(model)))
	}
	sb.WriteString(a.draftBannerText())

	rows := [][]InlineKeyboardButton{
		{
			{Text: "📺 ТВ и плеер", CallbackData: "m:tv"},
			{Text: "🔆 Дисплей и часы", CallbackData: "m:disp"},
		},
		{
			{Text: "🔊 Звук, EQ и DND", CallbackData: "m:aud"},
			{Text: "🎙 Короткие команды", CallbackData: "m:cmd"},
		},
		{
			{Text: "🤖 Bridge, AI и Роутинг", CallbackData: "m:br"},
			{Text: "🧬 JSON-редактор", CallbackData: "m:raw"},
		},
		{
			{Text: "⚙️ Система и службы", CallbackData: "m:sys"},
			{Text: "🔄 Обновить", CallbackData: "m:home"},
		},
	}
	rows = a.appendDraftRow(rows)
	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *BotApp) buildDisplayMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	st := a.station.GetStatus(ctx)
	merged, dirty, ucErr := a.loadEffectiveUserConfig(ctx)

	var sb strings.Builder
	sb.WriteString("🔆 <b>Дисплей, яркость LED и часы</b>\n")
	if st.Reachable {
		sb.WriteString(fmt.Sprintf("\n⚡ <b>Мгновенно fb4:</b> <code>%d / 200</code> (tmp=<code>%s</code>, /odm=<code>%s</code>)",
			st.BrightnessFB4, Esc(orDash(st.TmpBrightness)), Esc(orDash(st.ODMBrightness))))
		if st.ClockState != nil {
			en, _ := st.ClockState["clockEnabled"].(bool)
			on, _ := st.ClockState["isClockOn"].(bool)
			dnd, _ := st.ClockState["showClockDuringDnd"].(bool)
			sb.WriteString(fmt.Sprintf("\n🕒 <b>clock_display_state:</b> Часы=%s | Экран=%s | В DND=%s", boolIcon(en), boolIcon(on), boolIcon(dnd)))
		}
	}

	ledAuto, eqvAuto := true, true
	ledVal := 0.2
	style, tFmt, tSz := "barsCenter", "24h", "large"
	if ucErr == nil {
		if v, ok := GetBool(merged, "device_config", "led", "brightness", "auto"); ok {
			ledAuto = v
		}
		if v, ok := GetFloat(merged, "device_config", "led", "brightness", "value"); ok {
			ledVal = v
		}
		if v, ok := GetBool(merged, "device_config", "led", "music_equalizer_visualization", "auto"); ok {
			eqvAuto = v
		}
		if v, ok := GetString(merged, "device_config", "led", "music_equalizer_visualization", "style"); ok {
			style = v
		}
		if v, ok := GetString(merged, "device_config", "led", "time_visualization", "format"); ok {
			tFmt = v
		}
		if v, ok := GetString(merged, "device_config", "led", "time_visualization", "size"); ok {
			tSz = v
		}
		sb.WriteString(fmt.Sprintf("\n📄 <b>user_config.led (SDK):</b> auto=%s val=<code>%.2f</code> | eqAuto=%s style=<code>%s</code> | %s / %s",
			boolIcon(ledAuto), ledVal, boolIcon(eqvAuto), Esc(style), Esc(tFmt), Esc(tSz)))
	} else {
		sb.WriteString("\n⚠️ <i>user_config.dat недоступен (" + Esc(trimRunes(ucErr.Error(), 80)) + ")</i>")
	}
	sb.WriteString(a.draftBannerText())

	clkEn, clkOn, clkDnd := false, false, false
	if st.ClockState != nil {
		clkEn, _ = st.ClockState["clockEnabled"].(bool)
		clkOn, _ = st.ClockState["isClockOn"].(bool)
		clkDnd, _ = st.ClockState["showClockDuringDnd"].(bool)
	}

	rows := [][]InlineKeyboardButton{
		{
			{Text: "10", CallbackData: "db:10"},
			{Text: "20", CallbackData: "db:20"},
			{Text: "40", CallbackData: "db:40"},
			{Text: "80", CallbackData: "db:80"},
			{Text: "120", CallbackData: "db:120"},
			{Text: "200", CallbackData: "db:200"},
		},
		{
			{Text: "✏️ Своя (0..200)", CallbackData: "p:fb4"},
			{Text: "💾 Запечь в /odm", CallbackData: "db:bake"},
			{Text: "↩️ Сброс tmp", CallbackData: "db:rst"},
		},
		{
			{Text: "🕒 Часы: " + boolIcon(clkEn), CallbackData: "dc:en"},
			{Text: "👁 Экран: " + boolIcon(clkOn), CallbackData: "dc:on"},
			{Text: "🌙 В DND: " + boolIcon(clkDnd), CallbackData: "dc:dnd"},
		},
		{
			{Text: dirtyMark(dirty, "device_config.led.brightness.auto") + "Авто LED: " + boolIcon(ledAuto), CallbackData: "ut:led_auto"},
			{Text: dirtyMark(dirty, "device_config.led.brightness.value") + fmt.Sprintf("SDK val (%.2f)", ledVal), CallbackData: "p:ledv"},
		},
		{
			{Text: dirtyMark(dirty, "device_config.led.music_equalizer_visualization.auto") + "Эквалайзер авто: " + boolIcon(eqvAuto), CallbackData: "ut:eqv_auto"},
			{Text: dirtyMark(dirty, "device_config.led.music_equalizer_visualization.style") + "Стиль: " + style, CallbackData: "m:dstyle"},
		},
		{
			{Text: dirtyMark(dirty, "device_config.led.time_visualization.format") + "Формат: " + tFmt, CallbackData: "ut:t_fmt"},
			{Text: dirtyMark(dirty, "device_config.led.time_visualization.size") + "Размер: " + tSz, CallbackData: "ut:t_sz"},
		},
	}
	rows = a.appendDraftRow(rows)
	rows = append(rows, []InlineKeyboardButton{{Text: "🏠 Главное меню", CallbackData: "m:home"}, {Text: "🔄", CallbackData: "m:disp"}})
	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *BotApp) buildDisplayStyleMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	merged, _, _ := a.loadEffectiveUserConfig(ctx)
	cur, _ := GetString(merged, "device_config", "led", "music_equalizer_visualization", "style")
	text := fmt.Sprintf("🎨 <b>Стиль визуализации LED</b>\nТекущий: <code>%s</code>\n\nВыберите стиль или введите свой для черновика <code>user_config.dat</code>:", Esc(orDash(cur)))
	rows := [][]InlineKeyboardButton{
		{
			{Text: "barsCenter", CallbackData: "dstyle:barsCenter"},
			{Text: "barsSides", CallbackData: "dstyle:barsSides"},
			{Text: "circle", CallbackData: "dstyle:circle"},
		},
		{
			{Text: "✏️ Ввести свой стиль", CallbackData: "p:eqvs"},
			{Text: "↩️ Назад", CallbackData: "m:disp"},
		},
	}
	return text, InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *BotApp) buildCmdMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	merged, dirty, err := a.loadEffectiveUserConfig(ctx)
	if err != nil {
		return "❌ Станция недоступна: <code>" + Esc(err.Error()) + "</code>", *backHomeMarkup()
	}

	gb := func(p ...string) bool {
		v, _ := GetBool(merged, p...)
		return v
	}
	spotter, _ := GetString(merged, "account_config", "spotter")

	var sb strings.Builder
	sb.WriteString("🎙 <b>Короткие команды и поведение Алисы</b>\n")
	sb.WriteString("Все изменения здесь складываются в черновик и применяются кнопкой <b>Применить SDK</b>.\n")
	sb.WriteString(a.draftBannerText())

	rows := [][]InlineKeyboardButton{
		{
			{Text: dirtyMark(dirty, "account_config.spotter") + "Споттер: " + orDash(spotter), CallbackData: "ut:spotter"},
			{Text: dirtyMark(dirty, "account_config.alwaysOnMicForShortcuts") + "Микр. для команд: " + boolIcon(gb("account_config", "alwaysOnMicForShortcuts")), CallbackData: "ut:sh_mic"},
		},
		{
			{Text: dirtyMark(dirty, "account_config.smartActivation") + "Умн. активация: " + boolIcon(gb("account_config", "smartActivation")), CallbackData: "ut:smart_act"},
			{Text: dirtyMark(dirty, "account_config.jingle") + "Джингл: " + boolIcon(gb("account_config", "jingle")), CallbackData: "ut:jingle"},
		},
		{
			{Text: dirtyMark(dirty, "account_config.aliceProactivity") + "Проактивность: " + boolIcon(gb("account_config", "aliceProactivity")), CallbackData: "ut:proact"},
			{Text: dirtyMark(dirty, "account_config.aliceAdaptiveVolume.enabled") + "Адапт. громк.: " + boolIcon(gb("account_config", "aliceAdaptiveVolume", "enabled")), CallbackData: "ut:adap_vol"},
		},
		{
			{Text: dirtyMark(dirty, "account_config.enableChildVad") + "Детский VAD: " + boolIcon(gb("account_config", "enableChildVad")), CallbackData: "ut:child_vad"},
			{Text: dirtyMark(dirty, "account_config.saveHistoryUsage") + "История: " + boolIcon(gb("account_config", "saveHistoryUsage")), CallbackData: "ut:hist"},
		},
		{
			{Text: dirtyMark(dirty, "account_config.enabledCommandSpotters.tv.navigation") + "ТВ навигация: " + boolIcon(gb("account_config", "enabledCommandSpotters", "tv", "navigation")), CallbackData: "ut:cs_tvn"},
			{Text: dirtyMark(dirty, "account_config.enabledCommandSpotters.tv.backToHome") + "ТВ домой: " + boolIcon(gb("account_config", "enabledCommandSpotters", "tv", "backToHome")), CallbackData: "ut:cs_tvh"},
		},
		{
			{Text: dirtyMark(dirty, "account_config.enabledCommandSpotters.music.playAndPause") + "Муз. стоп/плей: " + boolIcon(gb("account_config", "enabledCommandSpotters", "music", "playAndPause")), CallbackData: "ut:cs_mp"},
			{Text: dirtyMark(dirty, "account_config.enabledCommandSpotters.music.navigation") + "Муз. дальше: " + boolIcon(gb("account_config", "enabledCommandSpotters", "music", "navigation")), CallbackData: "ut:cs_mn"},
		},
		{
			{Text: dirtyMark(dirty, "account_config.enabledCommandSpotters.music.volume") + "Муз. громк.: " + boolIcon(gb("account_config", "enabledCommandSpotters", "music", "volume")), CallbackData: "ut:cs_mv"},
			{Text: dirtyMark(dirty, "account_config.enabledCommandSpotters.music.feedback") + "Муз. лайк: " + boolIcon(gb("account_config", "enabledCommandSpotters", "music", "feedback")), CallbackData: "ut:cs_mf"},
		},
		{
			{Text: dirtyMark(dirty, "account_config.enabledCommandSpotters.music.bluetooth") + "Bluetooth: " + boolIcon(gb("account_config", "enabledCommandSpotters", "music", "bluetooth")), CallbackData: "ut:cs_mb"},
			{Text: dirtyMark(dirty, "account_config.enabledCommandSpotters.call.answer") + "Звонки: " + boolIcon(gb("account_config", "enabledCommandSpotters", "call", "answer")), CallbackData: "ut:cs_ca"},
		},
		{
			{Text: dirtyMark(dirty, "account_config.enabledCommandSpotters.smartHome.light") + "Умный дом свет: " + boolIcon(gb("account_config", "enabledCommandSpotters", "smartHome", "light")), CallbackData: "ut:cs_shl"},
			{Text: dirtyMark(dirty, "account_config.enabledCommandSpotters.smartHome.tv") + "Умный дом ТВ: " + boolIcon(gb("account_config", "enabledCommandSpotters", "smartHome", "tv")), CallbackData: "ut:cs_sht"},
		},
		{
			{Text: "⭐ Вкл. стандартные", CallbackData: "ucp:std"},
			{Text: "🔕 Выкл. все короткие", CallbackData: "ucp:none"},
		},
	}
	rows = a.appendDraftRow(rows)
	rows = append(rows, []InlineKeyboardButton{{Text: "🏠 Главное меню", CallbackData: "m:home"}, {Text: "🔄", CallbackData: "m:cmd"}})
	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *BotApp) buildAudioMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	st := a.station.GetStatus(ctx)
	merged, dirty, ucErr := a.loadEffectiveUserConfig(ctx)

	var sb strings.Builder
	sb.WriteString("🔊 <b>Звук, Эквалайзер, HDMI и DND</b>\n")
	if st.Reachable {
		sb.WriteString(fmt.Sprintf("\n⚡ <b>Громкость сейчас:</b> Музыка <code>%d</code> | Будильник <code>%d</code>", st.VolumeMusic, st.VolumeAlarm))
	}

	gb := func(p ...string) bool {
		if ucErr != nil {
			return false
		}
		v, _ := GetBool(merged, p...)
		return v
	}
	eqPreset := "—"
	dndStart, dndEnd := "—", "—"
	if ucErr == nil {
		if s, ok := GetString(merged, "device_config", "equalizer", "active_preset_id"); ok {
			eqPreset = s
		}
		if s, ok := GetString(merged, "device_config", "dndMode", "starts"); ok {
			dndStart = s
		}
		if s, ok := GetString(merged, "device_config", "dndMode", "ends"); ok {
			dndEnd = s
		}
		if bands, ok := GetAtPath(merged, "device_config", "equalizer", "custom_preset_bands"); ok {
			bJS, _ := marshalCompactNoEscape(bands)
			sb.WriteString(fmt.Sprintf("\n🎛 <b>Эквалайзер:</b> %s (пресет <code>%s</code>, полосы <code>%s</code>)",
				boolIcon(gb("device_config", "equalizer", "enabled")), Esc(eqPreset), Esc(string(bJS))))
		}
		sb.WriteString(fmt.Sprintf("\n🌙 <b>DND:</b> %s (%s – %s)", boolIcon(gb("device_config", "dndMode", "enabled")), Esc(dndStart), Esc(dndEnd)))
	} else {
		sb.WriteString("\n⚠️ <i>user_config.dat недоступен (" + Esc(trimRunes(ucErr.Error(), 80)) + ")</i>")
	}
	sb.WriteString(a.draftBannerText())

	rows := [][]InlineKeyboardButton{
		{
			{Text: "🔉 -10", CallbackData: "vol:-10"},
			{Text: "20", CallbackData: "vol:=20"},
			{Text: "40", CallbackData: "vol:=40"},
			{Text: "60", CallbackData: "vol:=60"},
			{Text: "80", CallbackData: "vol:=80"},
			{Text: "🔊 +10", CallbackData: "vol:+10"},
		},
		{
			{Text: "✏️ Оба", CallbackData: "p:vol"},
			{Text: "🎵 Музыка", CallbackData: "p:vol3"},
			{Text: "⏰ Будильник", CallbackData: "p:vol4"},
		},
		{
			{Text: dirtyMark(dirty, "device_config.equalizer.enabled") + "EQ: " + boolIcon(gb("device_config", "equalizer", "enabled")), CallbackData: "ut:eq_en"},
			{Text: dirtyMark(dirty, "device_config.equalizer.smartEnabled") + "Smart EQ: " + boolIcon(gb("device_config", "equalizer", "smartEnabled")), CallbackData: "ut:eq_sm"},
			{Text: "🎛 Пресет (" + eqPreset + ")", CallbackData: "m:eq"},
		},
		{
			{Text: "🎚 5 полос EQ", CallbackData: "p:eqbands"},
			{Text: dirtyMark(dirty, "account_config.audio_player.crossfadeEnabled") + "Кроссфейд: " + boolIcon(gb("account_config", "audio_player", "crossfadeEnabled")), CallbackData: "ut:xfade"},
		},
		{
			{Text: dirtyMark(dirty, "device_config.hdmiAudio") + "HDMI audio: " + boolIcon(gb("device_config", "hdmiAudio")), CallbackData: "ut:hdmi_aud"},
			{Text: dirtyMark(dirty, "system_config.com.yandex.io.sdk.hdmiAudioEnabled") + "SDK HDMI: " + boolIcon(gb("system_config", "com.yandex.io.sdk", "hdmiAudioEnabled")), CallbackData: "ut:sdk_hdmi"},
		},
		{
			{Text: dirtyMark(dirty, "device_config.dndMode.enabled") + "DND: " + boolIcon(gb("device_config", "dndMode", "enabled")), CallbackData: "ut:dnd_en"},
			{Text: dirtyMark(dirty, "device_config.dndMode.platformSettings.showClock") + "Часы DND: " + boolIcon(gb("device_config", "dndMode", "platformSettings", "showClock")), CallbackData: "ut:dnd_clk"},
			{Text: dirtyMark(dirty, "device_config.dndMode.features.allowIncomingCalls") + "Звонки DND: " + boolIcon(gb("device_config", "dndMode", "features", "allowIncomingCalls")), CallbackData: "ut:dnd_calls"},
		},
		{
			{Text: "🕒 Время DND", CallbackData: "p:dnd_t"},
			{Text: dirtyMark(dirty, "device_config.standby.deepStandbyEnabled") + "Deep Standby: " + boolIcon(gb("device_config", "standby", "deepStandbyEnabled")), CallbackData: "ut:stby"},
		},
	}
	rows = a.appendDraftRow(rows)
	rows = append(rows, []InlineKeyboardButton{{Text: "🏠 Главное меню", CallbackData: "m:home"}, {Text: "🔄", CallbackData: "m:aud"}})
	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *BotApp) buildEQPresetMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	merged, _, _ := a.loadEffectiveUserConfig(ctx)
	cur, _ := GetString(merged, "device_config", "equalizer", "active_preset_id")
	text := fmt.Sprintf("🎛 <b>Пресет эквалайзера</b>\nТекущий: <code>%s</code>\n\nВыберите пресет для черновика <code>user_config.dat</code>:", Esc(orDash(cur)))
	rows := [][]InlineKeyboardButton{
		{
			{Text: "morebass", CallbackData: "eq:morebass"},
			{Text: "custom", CallbackData: "eq:custom"},
			{Text: "flat", CallbackData: "eq:flat"},
		},
		{
			{Text: "pop", CallbackData: "eq:pop"},
			{Text: "rock", CallbackData: "eq:rock"},
			{Text: "classic", CallbackData: "eq:classic"},
		},
		{
			{Text: "🎚 Изменить 5 полос custom", CallbackData: "p:eqbands"},
			{Text: "↩️ Назад", CallbackData: "m:aud"},
		},
	}
	return text, InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *BotApp) buildBridgeMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	data, err := a.bridge.ReadRawMap()
	if err != nil {
		return "❌ Ошибка чтения config.json Quasar Bridge: <code>" + Esc(err.Error()) + "</code>", *backHomeMarkup()
	}
	st := a.station.GetStatus(ctx)
	bh, bhErr := a.bridge.Health(ctx)

	mode, _ := GetString(data, "mode")
	model, _ := GetString(data, "llm_model")
	lar, _ := GetBool(data, "listen_after_reply")
	toks, _ := GetFloat(data, "llm_max_output_tokens")
	dctx, _ := GetBool(data, "dialogue_context", "enabled")
	dttl, _ := GetFloat(data, "dialogue_context", "ttl_seconds")
	dturns, _ := GetFloat(data, "dialogue_context", "max_turns")
	prog, _ := GetBool(data, "local_progress", "enabled")

	streamLabel := orDash(st.StreamMode)
	if st.StreamMode == "yandex" && bhErr == nil && bh.Status == "ok" {
		streamLabel = "yandex → Bridge (DNS)"
	}

	var sb strings.Builder
	sb.WriteString("🤖 <b>Quasar Bridge, AI и Роутинг</b>\n")
	sb.WriteString(fmt.Sprintf("\n📡 <b>Поток Станции:</b> <code>%s</code> (%s)", Esc(streamLabel), Esc(orDash(st.UniProxyURL))))
	if bhErr == nil {
		sb.WriteString(fmt.Sprintf("\n💚 <b>Bridge Health:</b> <code>%s</code> | frames: C=%d S=%d | rules=%d",
			Esc(bh.Status), bh.ClientFrames, bh.ServerFrames, bh.RoutingRules))
	} else {
		sb.WriteString(fmt.Sprintf("\n⚙️ <b>Сервис bridge:</b> <code>%s</code>", Esc(a.bridge.IsServiceActive(ctx))))
	}
	sb.WriteString(fmt.Sprintf("\n🧠 <b>Режим:</b> <code>%s</code> | <b>Модель:</b> <code>%s</code> | <b>Max tokens:</b> <code>%.0f</code>",
		Esc(mode), Esc(model), toks))
	sb.WriteString(fmt.Sprintf("\n💬 <b>Контекст диалога:</b> %s (TTL %.0fs, %d ходов) | <b>Слушать после ответа:</b> %s",
		boolIcon(dctx), dttl, int(dturns), boolIcon(lar)))

	rows := [][]InlineKeyboardButton{
		{
			{Text: "🔀 Поток: Bridge", CallbackData: "st:bridge"},
			{Text: "☁️ Поток: Яндекс (сток)", CallbackData: "st:yandex"},
		},
		{
			{Text: markSel(mode == "hybrid") + "hybrid", CallbackData: "bm:hybrid"},
			{Text: markSel(mode == "bridge") + "bridge", CallbackData: "bm:bridge"},
			{Text: markSel(mode == "yandex") + "yandex", CallbackData: "bm:yandex"},
		},
		{
			{Text: "🧠 Модель: " + trimRunes(model, 22), CallbackData: "m:mod"},
			{Text: "✏️ Ввести модель", CallbackData: "p:b_mod"},
		},
		{
			{Text: "Слушать после: " + boolIcon(lar), CallbackData: "bt:lar"},
			{Text: fmt.Sprintf("Токены: %.0f", toks), CallbackData: "p:b_tok"},
		},
		{
			{Text: "Контекст: " + boolIcon(dctx), CallbackData: "bt:dctx"},
			{Text: "TTL/ходы", CallbackData: "p:b_dctx"},
			{Text: "Прогресс: " + boolIcon(prog), CallbackData: "bt:prog"},
		},
		{
			{Text: "🛠 Инструменты AI", CallbackData: "m:tools"},
			{Text: "🛤 Правила роутинга", CallbackData: "m:rules"},
		},
		{
			{Text: "🧪 Тест /route", CallbackData: "p:route"},
			{Text: "🏠 Меню", CallbackData: "m:home"},
		},
	}
	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}
}

func markSel(on bool) string {
	if on {
		return "● "
	}
	return ""
}

func (a *BotApp) buildModelsMenu(ctx context.Context, chatID int64) (string, InlineKeyboardMarkup) {
	data, _ := a.bridge.ReadRawMap()
	curModel, _ := GetString(data, "llm_model")
	models := a.bridge.DiscoverModels(ctx)
	a.setCachedModels(chatID, models)

	var sb strings.Builder
	sb.WriteString("🧠 <b>Выбор LLM модели</b>\n")
	sb.WriteString("Текущая модель: <code>" + Esc(orDash(curModel)) + "</code>\n")
	if len(models) == 0 {
		sb.WriteString("\nСписок <code>/models</code> от локального прокси пуст или недоступен. Введите имя модели вручную.")
	} else {
		sb.WriteString(fmt.Sprintf("\nНайдено моделей (%d):", len(models)))
	}

	var rows [][]InlineKeyboardButton
	for i := 0; i < len(models); i += 2 {
		row := []InlineKeyboardButton{
			{Text: markSel(models[i] == curModel) + trimRunes(models[i], 28), CallbackData: fmt.Sprintf("mo:%d", i)},
		}
		if i+1 < len(models) {
			row = append(row, InlineKeyboardButton{
				Text:         markSel(models[i+1] == curModel) + trimRunes(models[i+1], 28),
				CallbackData: fmt.Sprintf("mo:%d", i+1),
			})
		}
		rows = append(rows, row)
	}
	rows = append(rows, []InlineKeyboardButton{
		{Text: "✏️ Ввести вручную", CallbackData: "p:b_mod"},
		{Text: "↩️ Назад", CallbackData: "m:br"},
	})
	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *BotApp) buildToolsMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	data, err := a.bridge.ReadRawMap()
	if err != nil {
		return "❌ Ошибка чтения config.json", *backToMarkup("m:br", "🤖 Bridge")
	}
	gb := func(p ...string) bool {
		v, _ := GetBool(data, p...)
		return v
	}
	tz, _ := GetString(data, "assistant_tools", "timezone")

	text := fmt.Sprintf("🛠 <b>Инструменты AI (assistant_tools)</b>\nТаймзона: <code>%s</code>\n\nИзменения применяются сразу с перезапуском Quasar Bridge:", Esc(orDash(tz)))
	rows := [][]InlineKeyboardButton{
		{
			{Text: "assistant_tools: " + boolIcon(gb("assistant_tools", "enabled")), CallbackData: "bt:at_en"},
			{Text: "🌍 TZ: " + orDash(tz), CallbackData: "p:at_tz"},
		},
		{
			{Text: "🌐 Web: " + boolIcon(gb("assistant_tools", "web")), CallbackData: "bt:at_web"},
			{Text: "📜 History: " + boolIcon(gb("assistant_tools", "history")), CallbackData: "bt:at_hist"},
			{Text: "🕒 Time: " + boolIcon(gb("assistant_tools", "time")), CallbackData: "bt:at_time"},
		},
		{
			{Text: "🔔 Sound: " + boolIcon(gb("assistant_tools", "play_sound")), CallbackData: "bt:at_snd"},
			{Text: "⬆️ Updates: " + boolIcon(gb("assistant_tools", "updates")), CallbackData: "bt:at_upd"},
			{Text: "⚙️ Config: " + boolIcon(gb("assistant_tools", "configuration")), CallbackData: "bt:at_cfg"},
		},
		{{Text: "↩️ Назад", CallbackData: "m:br"}, {Text: "🏠 Меню", CallbackData: "m:home"}},
	}
	return text, InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *BotApp) buildRulesMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	data, err := a.bridge.ReadRawMap()
	if err != nil {
		return "❌ Ошибка чтения config.json", *backToMarkup("m:br", "🤖 Bridge")
	}
	defRoute, _ := GetString(data, "routing", "default_route")
	refusalOn, _ := GetBool(data, "routing", "refusal", "enabled")
	rules := ExtractBridgeRules(data)

	var sb strings.Builder
	sb.WriteString("🛤 <b>Правила роутинга Quasar Bridge</b>\n")
	sb.WriteString("Применяются мгновенно через <code>SIGHUP</code> без разрыва WebSocket!\n\n")
	sb.WriteString(fmt.Sprintf("Маршрут по умолчанию: <code>%s</code>\n", Esc(orDash(defRoute))))
	sb.WriteString(fmt.Sprintf("Перехват отказов Алисы: %s\n", boolIcon(refusalOn)))

	rows := [][]InlineKeyboardButton{
		{
			{Text: "Default: " + orDash(defRoute), CallbackData: "bt:def_rt"},
			{Text: "Отказы: " + boolIcon(refusalOn), CallbackData: "bt:ref"},
		},
	}
	for _, r := range rules {
		stIcon := "✅"
		if r.Disabled {
			stIcon = "🚫"
		}
		label := fmt.Sprintf("%s %s [%s]", stIcon, r.Name, r.Route)
		rows = append(rows, []InlineKeyboardButton{
			{Text: trimRunes(label, 52), CallbackData: fmt.Sprintf("r:%d", r.Index)},
		})
	}
	rows = append(rows, []InlineKeyboardButton{
		{Text: "🧪 Тест фразы", CallbackData: "p:route"},
		{Text: "↩️ Bridge", CallbackData: "m:br"},
	})
	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *BotApp) buildRuleDetail(ctx context.Context, idxStr string) (string, InlineKeyboardMarkup) {
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		return "❌ Неверный индекс", *backToMarkup("m:rules", "🛤 Правила")
	}
	data, err := a.bridge.ReadRawMap()
	if err != nil {
		return "❌ Ошибка чтения config.json", *backToMarkup("m:rules", "🛤 Правила")
	}
	rules := ExtractBridgeRules(data)
	if idx < 0 || idx >= len(rules) {
		return "❌ Правило не найдено", *backToMarkup("m:rules", "🛤 Правила")
	}
	r := rules[idx]
	rawMatch, _ := GetBridgeDotPath(data, fmt.Sprintf("routing.rules.%d.match", idx))
	matchJS, _ := marshalCompactNoEscape(rawMatch)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🛤 <b>Правило #%d:</b> <code>%s</code>\n", r.Index, Esc(r.Name)))
	sb.WriteString(fmt.Sprintf("Статус: %s (%s) | Маршрут: <code>%s</code>\n", boolIcon(!r.Disabled), map[bool]string{true: "выключено", false: "активно"}[r.Disabled], Esc(r.Route)))
	sb.WriteString(fmt.Sprintf("Match: <code>%s</code>\n", Esc(trimRunes(string(matchJS), 400))))
	sb.WriteString("\n<b>System prompt:</b>\n")
	if r.SystemPrompt == "" {
		sb.WriteString("<i>(не задан)</i>")
	} else {
		sb.WriteString(wrapPre(r.SystemPrompt, 2200))
	}

	toggleTxt := "🚫 Выключить"
	if r.Disabled {
		toggleTxt = "✅ Включить"
	}
	kb := InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{
			{Text: toggleTxt, CallbackData: fmt.Sprintf("rt:%d", r.Index)},
			{Text: "🔀 Маршрут: " + r.Route, CallbackData: fmt.Sprintf("rr:%d", r.Index)},
		},
		{
			{Text: "✏️ Изменить system_prompt", CallbackData: fmt.Sprintf("rp:%d", r.Index)},
		},
		{
			{Text: "↩️ К правилам", CallbackData: "m:rules"},
			{Text: "🏠 Меню", CallbackData: "m:home"},
		},
	}}
	return sb.String(), kb
}

func (a *BotApp) renderRuleDetail(ctx context.Context, chatID, msgID int64, idxStr string) {
	text, kb := a.buildRuleDetail(ctx, idxStr)
	if err := a.tg.EditMessageText(ctx, chatID, msgID, text, &kb); err != nil {
		_, _ = a.tg.SendMessage(ctx, chatID, text, &kb)
	}
}

func (a *BotApp) buildTVMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	st := a.station.GetStatus(ctx)
	data, brErr := a.bridge.ReadRawMap()

	var sb strings.Builder
	sb.WriteString("📺 <b>ТВ плеер (:4545) и SmartTube</b>\n")
	if st.Media.Reachable {
		sb.WriteString(fmt.Sprintf("\n🎬 <b>Пакет:</b> <code>%s</code>\n<b>Статус:</b> %s | <b>Время:</b> <code>%s / %s</code>\n<b>Название:</b> %s",
			Esc(orDash(st.Media.Package)), Esc(st.Media.StateLabel()),
			fmtMs(st.Media.PosMs), fmtMs(st.Media.DurationMs), Esc(orDash(trimRunes(st.Media.Title, 120)))))
	} else {
		sb.WriteString("\n⚠️ MediaDaemon (:4545) не отвечает (или ничего не играет)")
	}

	stDis := false
	stPkg := ""
	stDel := 700.0
	if brErr == nil {
		stDis, _ = GetBool(data, "smarttube", "disabled")
		stPkg, _ = GetString(data, "smarttube", "package")
		if v, ok := GetFloat(data, "smarttube", "intent_delay_ms"); ok {
			stDel = v
		}
		dispPkg := stPkg
		if dispPkg == "" {
			dispPkg = "auto"
		}
		sb.WriteString(fmt.Sprintf("\n\n⚙️ <b>Bridge SmartTube:</b> отключён=%s | пакет=<code>%s</code> | задержка=<code>%.0fмс</code>",
			boolIcon(stDis), Esc(dispPkg), stDel))
	}

	rows := [][]InlineKeyboardButton{
		{
			{Text: "⏮ Пред.", CallbackData: "mc:prev"},
			{Text: "▶️ Play", CallbackData: "mc:play"},
			{Text: "⏸ Пауза", CallbackData: "mc:pause"},
			{Text: "⏭ След.", CallbackData: "mc:next"},
		},
		{
			{Text: "⏪ -30с", CallbackData: "mc:r30"},
			{Text: "⏩ +30с", CallbackData: "mc:f30"},
			{Text: "⏩ +5м", CallbackData: "mc:f300"},
			{Text: "🎯 Таймкод", CallbackData: "p:seek"},
		},
		{
			{Text: "SmartTube откл.: " + boolIcon(stDis), CallbackData: "bt:st_dis"},
			{Text: fmt.Sprintf("Задержка: %.0fмс", stDel), CallbackData: "p:st_del"},
		},
		{
			{Text: markSel(stPkg == "") + "auto", CallbackData: "sp:0"},
			{Text: markSel(stPkg == "org.smarttube.stable") + "stable", CallbackData: "sp:1"},
			{Text: markSel(stPkg == "org.smarttube.beta") + "beta", CallbackData: "sp:2"},
			{Text: markSel(stPkg == "com.teamsmart.videomanager.tv") + "legacy", CallbackData: "sp:3"},
		},
		{
			{Text: "🏠 Главное меню", CallbackData: "m:home"},
			{Text: "🔄 Обновить", CallbackData: "m:tv"},
		},
	}
	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *BotApp) buildRawMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	var sb strings.Builder
	sb.WriteString("🧬 <b>Продвинутый редактор JSON</b>\n\n")
	sb.WriteString("Позволяет читать и менять <b>любой параметр</b> в <code>user_config.dat</code> (через черновик с сохранением Protobuf-выравнивания <code>uniProxyUrl</code>) и <code>config.json</code> Quasar Bridge (с проверкой <code>-check-config</code>).\n\n")
	sb.WriteString("<b>Примеры слэш-команд:</b>\n")
	sb.WriteString("<code>/get_uc device_config.led</code>\n")
	sb.WriteString("<code>/set_uc device_config.standby.deepStandbyTimeoutMinutes 180</code>\n")
	sb.WriteString("<code>/get_br tts_replacements</code>\n")
	sb.WriteString("<code>/set_br llm_max_output_tokens 2048</code>\n")
	sb.WriteString("<code>/route алиса включи интерстеллар в медиа</code>")
	sb.WriteString(a.draftBannerText())

	rows := [][]InlineKeyboardButton{
		{
			{Text: "🔍 Читать user_config", CallbackData: "p:uc_get"},
			{Text: "✏️ Изменить user_config", CallbackData: "p:uc_set"},
		},
		{
			{Text: "🔍 Читать bridge config", CallbackData: "p:br_get"},
			{Text: "✏️ Изменить bridge config", CallbackData: "p:br_set"},
		},
	}
	rows = a.appendDraftRow(rows)
	rows = append(rows, []InlineKeyboardButton{{Text: "🏠 Главное меню", CallbackData: "m:home"}})
	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *BotApp) buildSystemMenu(ctx context.Context) (string, InlineKeyboardMarkup) {
	st := a.station.GetStatus(ctx)
	bh, bhErr := a.bridge.Health(ctx)

	var sb strings.Builder
	sb.WriteString("⚙️ <b>Система и службы</b>\n")
	if st.Reachable {
		sb.WriteString(fmt.Sprintf("\n📡 <b>Station Max (%s):</b>\n", Esc(a.cfg.StationIP)))
		sb.WriteString(fmt.Sprintf("• <code>com.yandex.io.sdk</code>: <code>%s</code>\n", Esc(orDash(st.SDKPID))))
		sb.WriteString(fmt.Sprintf("• <code>MediaDaemon (:4545)</code>: <code>%s</code>\n", Esc(orDash(st.MediaDaemonPID))))
		sb.WriteString(fmt.Sprintf("• <code>adbd-watchdog.sh</code>: <code>%s</code>\n", Esc(orDash(st.WatchdogPID))))
		sb.WriteString(fmt.Sprintf("• <code>Dropbear (:2222)</code>: <code>%s</code>\n", Esc(orDash(st.DropbearPID))))
		sb.WriteString(fmt.Sprintf("• SELinux enforce: <code>%s</code> | /odm: <code>%s</code>\n", Esc(orDash(st.SELinuxEnforce)), Esc(orDash(st.ODMMountMode))))
		sb.WriteString(fmt.Sprintf("• Поток: <code>%s</code>\n", Esc(orDash(st.UniProxyURL))))
		if len(st.Backups) > 0 {
			sb.WriteString("• Файлы user_config: <code>" + Esc(fmt.Sprintf("%d шт.", len(st.Backups))) + "</code>\n")
		}
	} else {
		sb.WriteString("\n📡 <b>Station Max:</b> ⚠️ недоступна (" + Esc(trimRunes(st.SSHError, 120)) + ")\n")
	}

	sb.WriteString(fmt.Sprintf("\n🤖 <b>Quasar Bridge (systemd):</b> <code>%s</code>", Esc(a.bridge.IsServiceActive(ctx))))
	if bhErr == nil {
		sb.WriteString(fmt.Sprintf("\n• HTTP :8443/healthz: <code>%s</code>, mode=<code>%s</code>, rev=<code>%s</code>",
			Esc(bh.Status), Esc(bh.Mode), Esc(trimRunes(bh.RoutingRevision, 12))))
	}
	sb.WriteString(a.draftBannerText())

	rows := [][]InlineKeyboardButton{
		{
			{Text: "🔄 SDK", CallbackData: "sys:sdk"},
			{Text: "🔄 MediaDaemon", CallbackData: "sys:mediadaemon"},
			{Text: "🔄 Watchdog", CallbackData: "sys:watchdog"},
		},
		{
			{Text: "🔄 adbd :5555", CallbackData: "sys:adbd"},
			{Text: "🔄 Quasar Bridge", CallbackData: "sys:br"},
		},
		{
			{Text: "⏪ Откат UC (.bak_bot)", CallbackData: "y:rst_ucb"},
			{Text: "⏪ Откат UC (.orig)", CallbackData: "y:rst_uco"},
		},
		{
			{Text: "⏪ Откат Bridge (.botbak)", CallbackData: "y:rst_br"},
		},
		{
			{Text: "🏠 Главное меню", CallbackData: "m:home"},
			{Text: "🔄 Обновить", CallbackData: "m:sys"},
		},
	}
	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}
}

func backHomeMarkup() *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "🏠 Главное меню", CallbackData: "m:home"}},
	}}
}

func backToMarkup(cb, label string) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: label, CallbackData: cb}, {Text: "🏠 Меню", CallbackData: "m:home"}},
	}}
}

func trimRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// wrapPre HTML-escapes s first, safely trims to maxEscapedRunes without cutting inside
// an HTML entity (&...;), and wraps in balanced <pre>...</pre> tags.
func wrapPre(raw string, maxEscapedRunes int) string {
	esc := Esc(raw)
	r := []rune(esc)
	if len(r) <= maxEscapedRunes {
		return "<pre>" + esc + "</pre>"
	}
	cut := maxEscapedRunes
	// Avoid splitting an HTML entity (&lt;, &amp;, &#34;, etc.)
	for i := cut - 1; i >= 0 && i >= cut-8; i-- {
		if r[i] == ';' {
			break
		}
		if r[i] == '&' {
			cut = i
			break
		}
	}
	return "<pre>" + string(r[:cut]) + "\n…(обрезано)</pre>"
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func fmtMs(ms int64) string {
	if ms < 0 {
		return "0:00"
	}
	sec := ms / 1000
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func ParseSeekInput(raw string) (string, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return "", errors.New("пустое время перемотки")
	}

	prefix := ""
	if s[0] == '+' || s[0] == '-' {
		prefix = string(s[0])
		s = strings.TrimSpace(s[1:])
	}
	if s == "" {
		return "", errors.New("укажите значение после знака")
	}

	// Clock format mm:ss or hh:mm:ss
	if strings.Contains(s, ":") {
		parts := strings.Split(s, ":")
		if len(parts) == 2 || len(parts) == 3 {
			var total int64
			for _, p := range parts {
				v, err := strconv.ParseInt(p, 10, 64)
				if err != nil || v < 0 {
					return "", fmt.Errorf("некорректный таймкод: %s", raw)
				}
				total = total*60 + v
			}
			return fmt.Sprintf("seek %s%ds", prefix, total), nil
		}
		return "", fmt.Errorf("некорректный формат времени: %s", raw)
	}

	// Suffix s / m / h
	mult := int64(1)
	if strings.HasSuffix(s, "s") || strings.HasSuffix(s, "с") {
		s = strings.TrimSuffix(strings.TrimSuffix(s, "s"), "с")
		mult = 1
	} else if strings.HasSuffix(s, "m") || strings.HasSuffix(s, "м") {
		s = strings.TrimSuffix(strings.TrimSuffix(s, "m"), "м")
		mult = 60
	} else if strings.HasSuffix(s, "h") || strings.HasSuffix(s, "ч") {
		s = strings.TrimSuffix(strings.TrimSuffix(s, "h"), "ч")
		mult = 3600
	}

	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v < 0 {
		return "", fmt.Errorf("не удалось разобрать длительность: %s", raw)
	}
	return fmt.Sprintf("seek %s%ds", prefix, v*mult), nil
}

func ParseEQBands(raw string) ([]json.Number, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "[") {
		var arr []json.Number
		dec := json.NewDecoder(bytes.NewReader([]byte(s)))
		dec.UseNumber()
		if err := dec.Decode(&arr); err != nil {
			return nil, fmt.Errorf("некорректный JSON массив: %w", err)
		}
		if len(arr) != 5 {
			return nil, errors.New("нужно ровно 5 чисел (-12..12)")
		}
		for _, num := range arr {
			v, err := num.Int64()
			if err != nil || v < -12 || v > 12 {
				return nil, errors.New("каждая полоса должна быть целым числом от -12 до 12")
			}
		}
		return arr, nil
	}
	fields := strings.Fields(strings.ReplaceAll(s, ",", " "))
	if len(fields) != 5 {
		return nil, errors.New("нужно ровно 5 чисел (например: 5 -6 0 0 0)")
	}
	out := make([]json.Number, 5)
	for i, f := range fields {
		v, err := strconv.Atoi(f)
		if err != nil || v < -12 || v > 12 {
			return nil, errors.New("каждая полоса должна быть целым числом от -12 до 12")
		}
		out[i] = json.Number(strconv.Itoa(v))
	}
	return out, nil
}

package bridge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"os"
	"quasarbridge/internal/configvoice"
	"quasarbridge/internal/dialogue"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/playback"
	"quasarbridge/internal/reply"
	"quasarbridge/internal/webaddress"
)

type Completer interface {
	Complete(context.Context, []llm.Message, bool) (string, error)
}
type HomeHandler interface {
	Handle(context.Context, string) (string, error)
}
type PlaybackController interface {
	Active(context.Context) bool
	Control(context.Context, string) (string, error)
}
type MusicStarter interface {
	CanStartMusic(string) bool
	StartMusic(context.Context, string) (string, error)
}
type dialoguePromptEntry struct {
	prompt string
	rule   string
	at     time.Time
}

type Engine struct {
	Configuration    bool
	AirPlay          AirPlayController
	SmartTube        SmartTubeController
	LLM              Completer
	Assistant        Completer
	Home             HomeHandler
	Playback         PlaybackController
	Music            MusicStarter
	Dialogue         *dialogue.Store
	dialogueOnce     sync.Once
	dialoguePromptMu sync.Mutex
	dialoguePrompts  map[string]dialoguePromptEntry
	routing          atomic.Pointer[RoutingPolicy]
}

const defaultSystemPrompt = "Ты голосовой помощник пользователя. Отвечай по-русски, кратко и по существу, обычно до трёх предложений. Ответ будет прочитан вслух: без Markdown и служебных рассуждений. Выполняй внешние действия только доступными инструментами по явному запросу пользователя. Не заявляй о выполнении действий без успешного результата инструмента; если инструмента нет, прямо скажи об этом. Для Home Assistant есть отдельный обработчик умного дома."

func (e *Engine) dialogue() *dialogue.Store {
	e.dialogueOnce.Do(func() {
		if e.Dialogue == nil {
			e.Dialogue = dialogue.New(dialogue.DefaultConfig())
		}
	})
	return e.Dialogue
}

// Prepare reserves this speaker's request before ASR/classification can finish late.
func (e *Engine) Prepare(ctx context.Context) context.Context {
	if ctx.Err() != nil {
		return ctx
	}
	ctx, _, _ = e.dialogue().Start(ctx)
	return ctx
}

func (e *Engine) Resolve(ctx context.Context, text string) (Resolution, error) {
	return e.ResolveDecision(ctx, text, e.Decide(ctx, text))
}
func (e *Engine) ResolveDecision(ctx context.Context, text string, decision RouteDecision) (Resolution, error) {
	route := decision.Route
	if decision.prepared {
		ctx = e.dialogue().Use(ctx, decision.ticket)
	} else {
		ctx = e.Prepare(ctx)
	}
	_, _, ticket := e.dialogue().Start(ctx)
	if ctx.Err() != nil {
		return Resolution{Route: route}, ctx.Err()
	}

	if !e.dialogue().Current(ctx, ticket) {
		return Resolution{Route: route}, context.Canceled
	}
	if decision.Source == "dialogue_retry" {
		return e.retryQuestion(ctx, ticket)
	}
	if decision.Source == "dialogue_reset" {
		e.dialogue().ClearReplay(ctx, ticket)
		if !e.dialogue().Clear(ctx, ticket) {
			return Resolution{Route: route}, context.Canceled
		}
		e.setDialoguePrompt(ctx, ticket, "")
		return Resolution{Route: "llm", Answer: "Начинаем новый разговор."}, nil
	}
	if route != "llm" {
		if !e.dialogue().Clear(ctx, ticket) {
			return Resolution{Route: route}, context.Canceled
		}
		e.setDialoguePrompt(ctx, ticket, "")
	}

	// If switching to music or AirPlay, pause SmartTube silently
	if route != "smarttube" && e.SmartTube != nil && (route == "music" || route == "airplay") {
		e.SmartTube.PauseBackground()
	}

	result := Resolution{Route: route}
	switch route {
	case "smarttube":
		if e.SmartTube == nil {
			return Resolution{Route: "yandex"}, nil
		}
		if decision.Source == "smarttube_search_app" {
			app, query, _ := e.SmartTube.CanSearchApp(text)
			answer, err := e.SmartTube.SearchApp(ctx, app, query)
			if !e.dialogue().Current(ctx, ticket) {
				return result, context.Canceled
			}
			result.Answer = answer
			return result, err
		}
		if decision.Source == "smarttube_launch" {
			app, _ := e.SmartTube.CanLaunchApp(text)
			answer, err := e.SmartTube.LaunchApp(ctx, app)
			if !e.dialogue().Current(ctx, ticket) {
				return result, context.Canceled
			}
			result.Answer = answer
			return result, err
		}
		if decision.Source == "smarttube_play" {
			query, _ := e.SmartTube.CanPlay(text)
			answer, err := e.SmartTube.Play(ctx, query)
			if !e.dialogue().Current(ctx, ticket) {
				return result, context.Canceled
			}
			result.Answer = answer
			return result, err
		}
		action := e.SmartTube.Command(text)
		if action == "" {
			return result, errors.New("unknown smarttube control")
		}
		answer, err := e.SmartTube.ControlMedia(ctx, action)
		if !e.dialogue().Current(ctx, ticket) {
			return result, context.Canceled
		}
		result.Answer = answer
		return result, err
	case "airplay":
		if e.AirPlay == nil {
			return Resolution{Route: "yandex"}, nil
		}
		action := airPlayCommand(text)
		if decision.Source == "airplay_media" {
			controller, ok := e.AirPlay.(AirPlayMediaController)
			if !ok || !controller.Active(ctx) {
				return Resolution{Route: "yandex"}, nil
			}
			action = airPlayMediaCommand(text)
			if action == "" {
				return result, errors.New("unknown AirPlay media control")
			}
			answer, err := controller.ControlMedia(ctx, action)
			result.Answer = answer
			return result, err
		}
		if action == "" {
			return result, errors.New("unknown AirPlay control")
		}
		answer, err := e.AirPlay.Control(ctx, action)
		if err == nil && action == "start" {
			reply.SuppressListening(ctx)
		}
		result.Answer = answer
		return result, err
	case "music":
		if e.Music == nil {
			return result, errors.New("music playback is unavailable")
		}
		answer, err := e.Music.StartMusic(ctx, text)
		if !e.dialogue().Current(ctx, ticket) {
			return result, context.Canceled
		}
		result.Answer = answer
		return result, err
	case "playback":
		if e.Playback == nil {
			return Resolution{Route: "yandex"}, nil
		}
		action := playback.Command(text)
		if action == "" {
			return result, errors.New("unknown playback control")
		}
		answer, err := e.Playback.Control(ctx, action)
		if !e.dialogue().Current(ctx, ticket) {
			return result, context.Canceled
		}
		result.Answer = answer
		return result, err
	case "home":
		if e.Home == nil {
			return Resolution{Route: "yandex"}, nil
		}
		answer, err := e.Home.Handle(ctx, text)
		result.Answer = answer
		return result, err
	case "llm":
		systemPrompt := decision.SystemPrompt
		rule := decision.Rule
		if decision.Source == "dialogue" {
			if systemPrompt == "" {
				systemPrompt = e.getDialoguePrompt(ctx)
			}
			if rule == "" {
				rule = e.getDialogueRule(ctx)
			}
		}
		return e.fallbackWithPrompt(ctx, text, systemPrompt, rule, decision.Source != "dialogue")
	default:
		return Resolution{Route: "yandex"}, nil
	}
}
func (e *Engine) Fallback(ctx context.Context, text string) (Resolution, error) {
	return e.fallbackWithPrompt(ctx, text, "", "", true)
}

func isToolQuery(rule, text string) bool {
	if rule == "bridge_updates" || rule == "assistant_readonly" || rule == "music_from_site" {
		return true
	}
	lower := strings.ToLower(text)
	return strings.Contains(lower, "веб-страниц") ||
		strings.Contains(lower, "обнови") ||
		strings.Contains(lower, "настройк") ||
		strings.Contains(lower, "конфигураци") ||
		strings.Contains(lower, "прочитай данн") ||
		(strings.Contains(lower, "запрос") && strings.Contains(lower, "нейросет"))
}

// promptFiles maps a routing rule to an editable prompt file. The file is read
// on every request, so the persona can be changed live without a restart.
var promptFiles = map[string]string{
	"ask_llm_slysh":     "prompts/slysh.txt",
	"ask_llm_podskazhi": "prompts/podskazhi.txt",
}

func promptFileForRule(rule string) string { return promptFiles[rule] }

func readPromptFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("prompt_file_unavailable", "path", path, "err", err)
		return ""
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		slog.Warn("prompt_file_empty", "path", path)
	}
	return text
}

func (e *Engine) fallbackWithPrompt(ctx context.Context, text, systemPrompt, rule string, persistPrompt bool) (Resolution, error) {
	if systemPrompt == "" && (strings.Contains(strings.ToLower(text), "сайт") || webaddress.MusicRequest(text)) {
		text = webaddress.Normalize(text)
	}
	r := Resolution{Route: "llm", Rule: rule}
	if e.LLM == nil && e.Assistant == nil {
		return r, errors.New("LLM is not configured")
	}
	// Claude (Assistant) handles ONLY "подскажи" trigger, its dialogue follow-ups, and assistant tools.
	// All other voice requests (including "слышь", general questions, classifier, fallback) go to Gemini (LLM).
	answerer := e.LLM
	if rule == "ask_llm_slysh" {
		answerer = e.LLM
	} else if rule == "ask_llm_podskazhi" && e.Assistant != nil {
		answerer = e.Assistant
	} else if isToolQuery(rule, text) && e.Assistant != nil {
		answerer = e.Assistant
	} else if answerer == nil {
		answerer = e.Assistant
	}
	ctx, snapshot, ticket := e.dialogue().Start(ctx)
	if !e.dialogue().Current(ctx, ticket) {
		return r, context.Canceled
	}
	prompt := defaultSystemPrompt
	if systemPrompt != "" {
		prompt = systemPrompt
	}
	if file := promptFileForRule(rule); file != "" {
		if live := readPromptFile(file); live != "" {
			prompt = live
		}
	}
	messages := []llm.Message{{Role: "system", Content: prompt}}
	messages = append(messages, snapshot.Messages...)
	messages = append(messages, llm.Message{Role: "user", Content: text})
	private := snapshot.PrivateHistory
	var answer string
	var err error
	if contextual, ok := answerer.(interface {
		CompleteDialogue(context.Context, []llm.Message, bool) (string, bool, error)
	}); ok {
		answer, private, err = contextual.CompleteDialogue(ctx, messages, private)
	} else {
		callCtx := ctx
		if private {
			callCtx = llm.WithoutHostedTools(callCtx)
		}
		answer, err = answerer.Complete(callCtx, messages, false)
	}
	if !e.dialogue().Current(ctx, ticket) {
		return r, context.Canceled
	}
	if err == nil {
		if !e.dialogue().Commit(ctx, ticket, text, answer, private) {
			return r, context.Canceled
		}
		if persistPrompt {
			e.setDialogueState(ctx, ticket, systemPrompt, rule)
		}
	}
	r.Answer = answer
	return r, err
}

func (e *Engine) getDialoguePrompt(ctx context.Context) string {
	speaker := dialogue.SpeakerKey(ctx)
	e.dialoguePromptMu.Lock()
	defer e.dialoguePromptMu.Unlock()
	return e.dialoguePrompts[speaker].prompt
}

func (e *Engine) getDialogueRule(ctx context.Context) string {
	speaker := dialogue.SpeakerKey(ctx)
	e.dialoguePromptMu.Lock()
	defer e.dialoguePromptMu.Unlock()
	return e.dialoguePrompts[speaker].rule
}

func (e *Engine) setDialoguePrompt(ctx context.Context, ticket dialogue.Ticket, prompt string) {
	e.setDialogueState(ctx, ticket, prompt, "")
}

func (e *Engine) setDialogueState(ctx context.Context, ticket dialogue.Ticket, prompt, rule string) {
	speaker := dialogue.SpeakerKey(ctx)
	e.dialoguePromptMu.Lock()
	defer e.dialoguePromptMu.Unlock()
	if !e.dialogue().Current(ctx, ticket) {
		return
	}
	if prompt == "" && rule == "" {
		delete(e.dialoguePrompts, speaker)
		return
	}
	if e.dialoguePrompts == nil {
		e.dialoguePrompts = make(map[string]dialoguePromptEntry)
	} else if _, exists := e.dialoguePrompts[speaker]; !exists && len(e.dialoguePrompts) >= 128 {
		var oldest string
		var oldestAt time.Time
		for k, v := range e.dialoguePrompts {
			if oldestAt.IsZero() || v.at.Before(oldestAt) {
				oldest, oldestAt = k, v.at
			}
		}
		if oldest != "" {
			delete(e.dialoguePrompts, oldest)
		}
	}
	e.dialoguePrompts[speaker] = dialoguePromptEntry{prompt: prompt, rule: rule, at: time.Now()}
}
func containsOne(text string, needles []string) bool {
	for _, s := range needles {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}
func (e *Engine) SetRouting(cfg RoutingConfig) error {
	p, err := CompileRouting(cfg)
	if err != nil {
		return err
	}
	e.routing.Store(p)
	return nil
}
func (e *Engine) policy() *RoutingPolicy {
	if p := e.routing.Load(); p != nil {
		return p
	}
	return builtInRouting
}

type RoutingStatus struct {
	Revision          string
	ActiveRules       int
	ClassifierEnabled bool
	DefaultRoute      string
}

func (e *Engine) RoutingStatus() RoutingStatus {
	p := e.policy()
	return RoutingStatus{Revision: p.revision, ActiveRules: len(p.rules), ClassifierEnabled: p.classifierEnabled, DefaultRoute: p.defaultRoute}
}
func (e *Engine) IsRefusal(text string) bool { return e.policy().isRefusal(text) }
func IsRefusal(text string) bool             { return builtInRouting.isRefusal(text) }
func (e *Engine) Decide(ctx context.Context, text string) (decision RouteDecision) {
	ctx, snapshot, ticket := e.dialogue().Start(ctx)
	defer func() {
		// Choose the cloud path before Native can start local progress/TTS.
		if decision.Route == "home" && e.Home == nil {
			decision.Route = "yandex"
			decision.Source = "home_disabled"
		}
		decision.ticket = ticket
		decision.prepared = true
	}()
	p := e.policy()
	if e.Configuration && configvoice.InConversation(text, snapshot.Messages) {
		return RouteDecision{Route: "llm", Source: "bridge_configuration"}
	}
	if e.AirPlay != nil && airPlayCommand(text) != "" {
		return RouteDecision{Route: "airplay", Source: "airplay"}
	}
	if isRetryCommand(text) {
		return RouteDecision{Route: "llm", Source: "dialogue_retry"}
	}
	if dialogue.IsReset(text) {
		return RouteDecision{Route: "llm", Source: "dialogue_reset"}
	}
	if d, ok := p.matchPromptRule(text); ok {
		return d
	}
	if controller, ok := e.AirPlay.(AirPlayMediaController); ok && airPlayMediaCommand(text) != "" && controller.Active(ctx) {
		return RouteDecision{Route: "airplay", Source: "airplay_media"}
	}
	if e.SmartTube != nil {
		if _, _, ok := e.SmartTube.CanSearchApp(text); ok {
			return RouteDecision{Route: "smarttube", Source: "smarttube_search_app"}
		}
		if _, ok := e.SmartTube.CanLaunchApp(text); ok {
			return RouteDecision{Route: "smarttube", Source: "smarttube_launch"}
		}
		if _, ok := e.SmartTube.CanPlay(text); ok {
			return RouteDecision{Route: "smarttube", Source: "smarttube_play"}
		}
		if action := e.SmartTube.Command(text); action != "" && e.SmartTube.Active(ctx) {
			return RouteDecision{Route: "smarttube", Source: "smarttube_media"}
		}
	}
	if e.Playback != nil && playback.Command(text) != "" && e.Playback.Active(ctx) {
		return RouteDecision{Route: "playback", Source: "playback"}
	}
	if d, ok := p.match(text); ok {
		if d.Route == "llm" && e.Music != nil && e.Music.CanStartMusic(text) {
			d.Route = "music"
		}
		return d
	}
	if len(snapshot.Messages) > 0 {
		if !p.classifierEnabled || dialogue.IsFollowup(text) {
			return RouteDecision{Route: "llm", Source: "dialogue", Rule: e.getDialogueRule(ctx), SystemPrompt: e.getDialoguePrompt(ctx)}
		}
	}
	if !p.classifierEnabled {
		return RouteDecision{Route: p.defaultRoute, Source: "default"}
	}
	failed := RouteDecision{Route: p.classifier.OnError, Source: "classifier_error"}
	if e.LLM == nil {
		return failed
	}
	classCtx, cancel := context.WithTimeout(ctx, time.Duration(p.classifier.TimeoutMS)*time.Millisecond)
	defer cancel()
	messages := []llm.Message{{Role: "system", Content: p.classifier.Prompt}}
	messages = append(messages, snapshot.Messages...)
	messages = append(messages, llm.Message{Role: "user", Content: text})
	answer, err := e.LLM.Complete(classCtx, messages, true)
	if err != nil {
		return failed
	}
	var choice struct {
		Route string `json:"route"`
	}
	if json.Unmarshal([]byte(answer), &choice) != nil || !directRoute(choice.Route) {
		return failed
	}
	return RouteDecision{Route: choice.Route, Source: "classifier"}
}

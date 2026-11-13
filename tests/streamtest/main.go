// Command streamtest exercises the streaming LLM route end to end without
// touching the live bridge path: LLM SSE stream -> sentence chunker ->
// TTS replacements -> per-sentence cloud TTS synthesis -> one continuous
// Ogg/Opus stream assembled by the same oggSpeech remuxer the station uses.
//
// Every artifact is written to the output directory:
//
//	full.ogg        the remuxed stream as the station would play it
//	phrase_NN.ogg   the raw cloud TTS for each sentence
//	report.json     timings of every stage
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
)

func main() {
	configDir := flag.String("dir", "/opt/quasar-bridge", "config directory (config.json + llm.json)")
	query := flag.String("q", "ты в курсе что арбуз это дыня", "user query")
	ruleName := flag.String("rule", "ask_llm_slysh", "routing rule name")
	outDir := flag.String("out", "", "output directory (default: ./out/<timestamp>)")
	timeout := flag.Duration("timeout", 3*time.Minute, "overall timeout")
	flag.Parse()

	cfg, err := loadConfig(*configDir)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	rule, ok := ruleByName(cfg, *ruleName)
	if !ok {
		log.Fatalf("rule %q not found in config", *ruleName)
	}
	dir := *outDir
	if dir == "" {
		dir = filepath.Join("out", time.Now().Format("20060102-150405"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("output dir: %v", err)
	}

	client := &llm.Client{
		BaseURL:         cfg.LLM.BaseURL,
		Model:           cfg.LLM.Model,
		APIKey:          cfg.LLM.APIKey,
		API:             cfg.LLM.API,
		MaxOutputTokens: cfg.LLM.MaxTokens,
		ChatParameters:  cfg.LLM.ChatParameters,
	}
	engine := &bridge.Engine{LLM: bridge.NewStreamClient(client), Dialogue: dialogue.New(cfg.Dialogue)}
	replacer := bridge.NewTTSReplacer(cfg.TTSReplacements)
	synth := bridge.NewTTSSynth(cfg.Upstream)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	fmt.Printf("rule   : %s\nmodel  : %s (%s)\nquery  : %s\nout    : %s\n\n", rule.Name, cfg.LLM.Model, cfg.LLM.BaseURL, *query, dir)

	report := &report{Query: *query, Rule: rule.Name, Model: cfg.LLM.Model, Started: time.Now()}
	t0 := time.Now()

	res := engine.StreamFallback(ctx, *query, rule.SystemPrompt, rule.Name)
	ogg := bridge.NewOggMux()
	n := 0
	var spoken []string
	for sentence := range res.Sentences {
		n++
		raw := sentence
		prepared := replacer.Replace(sentence)
		fmt.Printf("[+%6.2fs] phrase %d: %s\n", time.Since(t0).Seconds(), n, prepared)
		pt := time.Now()
		audio, err := synth.Synthesize(ctx, prepared, "ru-RU", "audio/opus", emotionFor(rule.Name))
		elapsed := time.Since(pt)
		if err != nil {
			log.Fatalf("tts phrase %d: %v", n, err)
		}
		name := fmt.Sprintf("phrase_%02d.ogg", n)
		if err := os.WriteFile(filepath.Join(dir, name), audio, 0o644); err != nil {
			log.Fatalf("write %s: %v", name, err)
		}
		if err := ogg.AddRaw(audio); err != nil {
			log.Fatalf("remux phrase %d: %v", n, err)
		}
		report.Phrases = append(report.Phrases, phraseReport{
			N: n, Raw: raw, TTS: prepared, Bytes: len(audio),
			ArrivedS: round(time.Since(t0).Seconds() - elapsed.Seconds()),
			TTSS:     round(elapsed.Seconds()),
		})
		spoken = append(spoken, prepared)
		fmt.Printf("          tts %.2fs, %d bytes\n", elapsed.Seconds(), len(audio))
	}
	if err := *res.Err; err != nil {
		log.Fatalf("stream: %v", err)
	}
	full := ogg.Finish()
	if err := os.WriteFile(filepath.Join(dir, "full.ogg"), full, 0o644); err != nil {
		log.Fatalf("write full.ogg: %v", err)
	}
	report.TotalS = round(time.Since(t0).Seconds())
	report.FullBytes = len(full)
	report.Spoken = strings.Join(spoken, " ")
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0o644); err != nil {
		log.Fatalf("write report: %v", err)
	}
	fmt.Printf("\ndone: %d phrases, full.ogg %d bytes, total %.2fs -> %s\n", n, len(full), report.TotalS, dir)
}

func emotionFor(rule string) string {
	if rule == "ask_llm_slysh" {
		return "evil"
	}
	return ""
}

func round(v float64) float64 { return float64(int(v*1000)) / 1000 }

type report struct {
	Query     string         `json:"query"`
	Rule      string         `json:"rule"`
	Model     string         `json:"model"`
	Started   time.Time      `json:"started"`
	TotalS    float64        `json:"total_s"`
	FullBytes int            `json:"full_bytes"`
	Spoken    string         `json:"spoken"`
	Phrases   []phraseReport `json:"phrases"`
}

type phraseReport struct {
	N        int     `json:"n"`
	Raw      string  `json:"raw"`
	TTS      string  `json:"tts"`
	Bytes    int     `json:"bytes"`
	ArrivedS float64 `json:"arrived_s"`
	TTSS     float64 `json:"tts_s"`
}

type fileConfig struct {
	Upstream        string                `json:"upstream"`
	TTSReplacements map[string]string     `json:"tts_replacements"`
	Dialogue        dialogue.Config       `json:"dialogue_context"`
	Routing         struct {
		Rules []routingRule `json:"rules"`
	} `json:"routing"`
	LLM llmFile `json:"-"`
}

type routingRule struct {
	Name         string `json:"name"`
	SystemPrompt string `json:"system_prompt"`
}

type llmFile struct {
	BaseURL        string         `json:"llm_base_url"`
	Model          string         `json:"llm_model"`
	API            string         `json:"llm_api"`
	APIKey         string         `json:"llm_api_key"`
	MaxTokens      int            `json:"llm_max_output_tokens"`
	ChatParameters map[string]any `json:"llm_chat_parameters"`
}

func loadConfig(dir string) (*fileConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, err
	}
	var cfg fileConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	if cfg.Upstream == "" {
		cfg.Upstream = "wss://uniproxy.alice.yandex.net/uni.ws"
	}
	llmRaw, err := os.ReadFile(filepath.Join(dir, "llm.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(llmRaw, &cfg.LLM); err != nil {
		return nil, fmt.Errorf("llm.json: %w", err)
	}
	if cfg.LLM.API == "" {
		cfg.LLM.API = "chat_completions"
	}
	return &cfg, nil
}

func ruleByName(cfg *fileConfig, name string) (routingRule, bool) {
	for _, r := range cfg.Routing.Rules {
		if r.Name == name {
			return r, true
		}
	}
	return routingRule{}, false
}

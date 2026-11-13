package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/chatgpt"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/llm"
)

type progressConfig struct {
	Enabled    bool `json:"enabled"`
	AfterMS    int  `json:"after_ms"`
	IntervalMS int  `json:"interval_ms"`
}

type config struct {
	AirPlay            airPlayConfig        `json:"airplay"`
	SmartTube          smartTubeConfig      `json:"smarttube"`
	DialogueContext    dialogue.Config      `json:"dialogue_context"`
	LocalProgress      progressConfig       `json:"local_progress"`
	AssistantTools     assistant.Config     `json:"assistant_tools"`
	ConversationLog    journal.Config       `json:"conversation_log"`
	Listen             string               `json:"listen"`
	Upstream           string               `json:"upstream"`
	TLSCert            string               `json:"tls_cert"`
	TLSKey             string               `json:"tls_key"`
	AllowedIPs         []string             `json:"allowed_ips"`
	Mode               string               `json:"mode"`
	LLMConfigFile      string               `json:"llm_config_file,omitempty"`
	LLMBaseURL         string               `json:"llm_base_url,omitempty"`
	LLMModel           string               `json:"llm_model,omitempty"`
	LLMAPI             string               `json:"llm_api,omitempty"`
	LLMAPIKey          string               `json:"llm_api_key,omitempty"`
	LLMChatGPTAuthFile string               `json:"llm_chatgpt_auth_file,omitempty"`
	LLMReasoningEffort string               `json:"llm_reasoning_effort,omitempty"`
	LLMMaxOutputTokens int                  `json:"llm_max_output_tokens,omitempty"`
	LLMHostedTools     []string             `json:"llm_hosted_tools,omitempty"`
	LLMChatParameters  map[string]any       `json:"llm_chat_parameters,omitempty"`
	AssistantBaseURL   string               `json:"assistant_base_url,omitempty"`
	AssistantModel     string               `json:"assistant_model,omitempty"`
	AssistantAPI       string               `json:"assistant_api,omitempty"`
	AssistantAPIKey    string               `json:"assistant_api_key,omitempty"`
	AssistantChatParams map[string]any      `json:"assistant_chat_parameters,omitempty"`
	AssistantMaxTokens int                  `json:"assistant_max_output_tokens,omitempty"`
	HomeWarmArea       string               `json:"home_warm_area"`
	ListenAfterReply   bool                 `json:"listen_after_reply"`
	TTSReplacements    map[string]string    `json:"tts_replacements,omitempty"`
	Routing            bridge.RoutingConfig `json:"routing"`
}

type llmFileConfig struct {
	BaseURL         string         `json:"base_url,omitempty"`
	LLMBaseURL      string         `json:"llm_base_url,omitempty"`
	Model           string         `json:"model,omitempty"`
	LLMModel        string         `json:"llm_model,omitempty"`
	API             string         `json:"api,omitempty"`
	LLMAPI          string         `json:"llm_api,omitempty"`
	APIKey          string         `json:"api_key,omitempty"`
	LLMAPIKey       string         `json:"llm_api_key,omitempty"`
	ChatGPTAuthFile string         `json:"chatgpt_auth_file,omitempty"`
	LLMChatGPTAuth  string         `json:"llm_chatgpt_auth_file,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	LLMReasoning    string         `json:"llm_reasoning_effort,omitempty"`
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"`
	LLMMaxTokens    int            `json:"llm_max_output_tokens,omitempty"`
	HostedTools     []string       `json:"hosted_tools,omitempty"`
	LLMHostedTools  []string       `json:"llm_hosted_tools,omitempty"`
	ChatParameters  map[string]any `json:"chat_parameters,omitempty"`
	LLMChatParams   map[string]any `json:"llm_chat_parameters,omitempty"`
	AssistantBaseURL string         `json:"assistant_base_url,omitempty"`
	AssistantModel   string         `json:"assistant_model,omitempty"`
	AssistantAPI     string         `json:"assistant_api,omitempty"`
	AssistantAPIKey  string         `json:"assistant_api_key,omitempty"`
	AssistantChatParams map[string]any `json:"assistant_chat_parameters,omitempty"`
	AssistantMaxTokens int          `json:"assistant_max_output_tokens,omitempty"`
}

func (c config) apiKey() string {
	if c.LLMAPIKey != "" {
		return c.LLMAPIKey
	}
	return os.Getenv("QUASAR_LLM_API_KEY")
}

func readConfig(path string) (config, error) {
	return readConfigForCommand(path, false)
}

// Discovery and sign-in run before the installer has selected a model. Every
// other setting still receives the same validation as the running service.
func readConfigForCommand(path string, modelOptional bool) (config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return config{}, errors.New("cannot read configuration file")
	}
	return parseConfigWithDir(raw, filepath.Dir(path), modelOptional)
}

func parseConfig(raw []byte, modelOptional bool) (config, error) {
	return parseConfigWithDir(raw, ".", modelOptional)
}

func parseConfigWithDir(raw []byte, dir string, modelOptional bool) (config, error) {
	cfg := config{DialogueContext: dialogue.DefaultConfig(), Routing: bridge.DefaultRoutingConfig(), ConversationLog: journal.DefaultConfig(), AssistantTools: assistant.DefaultConfig(), LocalProgress: progressConfig{AfterMS: 2000, IntervalMS: 4000}, LLMAPI: "chat_completions", LLMMaxOutputTokens: 4096, ListenAfterReply: true}
	if json.Unmarshal(raw, &cfg, json.RejectUnknownMembers(true)) != nil {
		return cfg, errors.New("invalid configuration JSON")
	}
	if value, ok := os.LookupEnv("QUASAR_LLM_CONFIG_FILE"); ok {
		cfg.LLMConfigFile = value
	}
	if cfg.LLMConfigFile != "" {
		llmPath := cfg.LLMConfigFile
		if !filepath.IsAbs(llmPath) {
			llmPath = filepath.Join(dir, llmPath)
		}
		llmRaw, err := os.ReadFile(llmPath)
		if err != nil {
			return cfg, fmt.Errorf("cannot read LLM configuration file: %w", err)
		}
		var lcfg llmFileConfig
		if json.Unmarshal(llmRaw, &lcfg, json.RejectUnknownMembers(true)) != nil {
			return cfg, errors.New("invalid LLM configuration JSON")
		}
		if lcfg.BaseURL != "" {
			cfg.LLMBaseURL = lcfg.BaseURL
		}
		if lcfg.LLMBaseURL != "" {
			cfg.LLMBaseURL = lcfg.LLMBaseURL
		}
		if lcfg.Model != "" {
			cfg.LLMModel = lcfg.Model
		}
		if lcfg.LLMModel != "" {
			cfg.LLMModel = lcfg.LLMModel
		}
		if lcfg.API != "" {
			cfg.LLMAPI = lcfg.API
		}
		if lcfg.LLMAPI != "" {
			cfg.LLMAPI = lcfg.LLMAPI
		}
		if lcfg.APIKey != "" {
			cfg.LLMAPIKey = lcfg.APIKey
		}
		if lcfg.LLMAPIKey != "" {
			cfg.LLMAPIKey = lcfg.LLMAPIKey
		}
		if lcfg.ChatGPTAuthFile != "" {
			cfg.LLMChatGPTAuthFile = lcfg.ChatGPTAuthFile
		}
		if lcfg.LLMChatGPTAuth != "" {
			cfg.LLMChatGPTAuthFile = lcfg.LLMChatGPTAuth
		}
		if lcfg.ReasoningEffort != "" {
			cfg.LLMReasoningEffort = lcfg.ReasoningEffort
		}
		if lcfg.LLMReasoning != "" {
			cfg.LLMReasoningEffort = lcfg.LLMReasoning
		}
		if lcfg.MaxOutputTokens > 0 {
			cfg.LLMMaxOutputTokens = lcfg.MaxOutputTokens
		}
		if lcfg.LLMMaxTokens > 0 {
			cfg.LLMMaxOutputTokens = lcfg.LLMMaxTokens
		}
		if len(lcfg.HostedTools) > 0 {
			cfg.LLMHostedTools = lcfg.HostedTools
		}
		if len(lcfg.LLMHostedTools) > 0 {
			cfg.LLMHostedTools = lcfg.LLMHostedTools
		}
		if len(lcfg.ChatParameters) > 0 {
			cfg.LLMChatParameters = lcfg.ChatParameters
		}
		if len(lcfg.LLMChatParams) > 0 {
			cfg.LLMChatParameters = lcfg.LLMChatParams
		}
		if lcfg.AssistantBaseURL != "" {
			cfg.AssistantBaseURL = lcfg.AssistantBaseURL
		}
		if lcfg.AssistantModel != "" {
			cfg.AssistantModel = lcfg.AssistantModel
		}
		if lcfg.AssistantAPI != "" {
			cfg.AssistantAPI = lcfg.AssistantAPI
		}
		if lcfg.AssistantAPIKey != "" {
			cfg.AssistantAPIKey = lcfg.AssistantAPIKey
		}
		if len(lcfg.AssistantChatParams) > 0 {
			cfg.AssistantChatParams = lcfg.AssistantChatParams
		}
		if lcfg.AssistantMaxTokens > 0 {
			cfg.AssistantMaxTokens = lcfg.AssistantMaxTokens
		}
	}
	for name, target := range map[string]*string{
		"QUASAR_LISTEN": &cfg.Listen, "QUASAR_UPSTREAM": &cfg.Upstream,
		"QUASAR_TLS_CERT": &cfg.TLSCert, "QUASAR_TLS_KEY": &cfg.TLSKey,
		"QUASAR_MODE": &cfg.Mode, "QUASAR_LLM_BASE_URL": &cfg.LLMBaseURL,
		"QUASAR_LLM_MODEL": &cfg.LLMModel, "QUASAR_HOME_WARM_AREA": &cfg.HomeWarmArea,
		"QUASAR_LLM_API": &cfg.LLMAPI, "QUASAR_LLM_REASONING_EFFORT": &cfg.LLMReasoningEffort,
		"QUASAR_LLM_CHATGPT_AUTH_FILE": &cfg.LLMChatGPTAuthFile,
		"QUASAR_ASSISTANT_LLM_BASE_URL": &cfg.AssistantBaseURL,
		"QUASAR_ASSISTANT_LLM_MODEL":    &cfg.AssistantModel,
		"QUASAR_ASSISTANT_LLM_API":      &cfg.AssistantAPI,
	} {
		if value, ok := os.LookupEnv(name); ok {
			*target = value
		}
	}
	if value, ok := os.LookupEnv("QUASAR_LLM_API_KEY"); ok && value != "" && cfg.LLMAPIKey == "" {
		cfg.LLMAPIKey = value
	}
	if value, ok := os.LookupEnv("QUASAR_ASSISTANT_LLM_API_KEY"); ok && value != "" && cfg.AssistantAPIKey == "" {
		cfg.AssistantAPIKey = value
	}
	if value, ok := os.LookupEnv("QUASAR_ALLOWED_IPS"); ok {
		cfg.AllowedIPs = nil
		for item := range strings.SplitSeq(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				cfg.AllowedIPs = append(cfg.AllowedIPs, item)
			}
		}
	}
	if len(cfg.AllowedIPs) == 0 {
		cfg.AllowedIPs = nil
	}
	if cfg.Listen == "" {
		cfg.Listen = "localhost:8443"
	}
	if cfg.Upstream == "" {
		cfg.Upstream = "wss://uniproxy.alice.yandex.net/uni.ws"
	}
	if cfg.Mode == "" {
		cfg.Mode = "observe"
	}
	if cfg.LLMAPI == "" {
		cfg.LLMAPI = "chat_completions"
	}
	if cfg.LLMAPI != "chat_completions" && cfg.LLMAPI != "responses" && cfg.LLMAPI != "chatgpt" {
		return cfg, errors.New("llm_api must be chat_completions, responses or chatgpt")
	}
	if cfg.LLMAPI == "chatgpt" {
		if cfg.LLMBaseURL == "" {
			cfg.LLMBaseURL = "https://api.openai.com/v1"
		}
		if strings.TrimRight(cfg.LLMBaseURL, "/") != "https://api.openai.com/v1" {
			return cfg, errors.New("chatgpt requires llm_base_url https://api.openai.com/v1")
		}
		if cfg.LLMChatGPTAuthFile == "" {
			path, err := chatgpt.DefaultPath()
			if err != nil {
				return cfg, err
			}
			cfg.LLMChatGPTAuthFile = path
		}
	}
	if len(cfg.LLMChatParameters) == 0 {
		cfg.LLMChatParameters = nil
	} else {
		if cfg.LLMAPI != "chat_completions" {
			return cfg, errors.New("llm_chat_parameters requires llm_api chat_completions")
		}
		if err := llm.ValidateChatParameters(cfg.LLMChatParameters); err != nil {
			return cfg, err
		}
	}
	if cfg.LLMMaxOutputTokens <= 0 {
		return cfg, errors.New("llm_max_output_tokens must be positive")
	}
	if cfg.LLMReasoningEffort != "" {
		if cfg.LLMAPI != "responses" && cfg.LLMAPI != "chatgpt" {
			return cfg, errors.New("llm_reasoning_effort requires llm_api responses or chatgpt")
		}
		if !slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, cfg.LLMReasoningEffort) {
			return cfg, errors.New("invalid llm_reasoning_effort")
		}
	}
	if len(cfg.LLMHostedTools) == 0 {
		cfg.LLMHostedTools = nil
	} else {
		if cfg.LLMAPI != "responses" && cfg.LLMAPI != "chatgpt" {
			return cfg, errors.New("llm_hosted_tools requires llm_api responses or chatgpt")
		}
		seen := make(map[string]bool, len(cfg.LLMHostedTools))
		for _, name := range cfg.LLMHostedTools {
			if cfg.LLMAPI == "chatgpt" && name != "web_search" {
				return cfg, errors.New("chatgpt llm_hosted_tools supports only web_search")
			}
			if name != "web_search" && name != "code_interpreter" {
				return cfg, errors.New("llm_hosted_tools supports web_search and code_interpreter")
			}
			if seen[name] {
				return cfg, errors.New("duplicate llm_hosted_tools entry")
			}
			seen[name] = true
		}
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return cfg, errors.New("listen must include host and port")
	}
	if cfg.Mode != "observe" && cfg.Mode != "hybrid" {
		return cfg, errors.New("mode must be observe or hybrid")
	}
	if !validEndpoint(cfg.Upstream, "ws", "wss") {
		return cfg, errors.New("invalid upstream URL")
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return cfg, errors.New("TLS certificate and key must be set together")
	}
	for _, ip := range cfg.AllowedIPs {
		if _, err := netip.ParseAddr(ip); err != nil {
			return cfg, errors.New("allowed_ips must contain IP addresses")
		}
	}
	if cfg.LLMBaseURL != "" && !validEndpoint(cfg.LLMBaseURL, "http", "https") {
		return cfg, errors.New("invalid LLM URL")
	}
	if cfg.AssistantBaseURL != "" && !validEndpoint(cfg.AssistantBaseURL, "http", "https") {
		return cfg, errors.New("invalid assistant LLM URL")
	}
	if cfg.AssistantAPI != "" && cfg.AssistantAPI != "chat_completions" && cfg.AssistantAPI != "responses" && cfg.AssistantAPI != "chatgpt" {
		return cfg, errors.New("assistant_api must be chat_completions, responses or chatgpt")
	}
	if len(cfg.AssistantChatParams) > 0 {
		if err := llm.ValidateChatParameters(cfg.AssistantChatParams); err != nil {
			return cfg, err
		}
	}
	if cfg.Mode == "hybrid" {
		if cfg.LLMBaseURL == "" || (!modelOptional && cfg.LLMModel == "") {
			return cfg, errors.New("hybrid mode requires LLM URL and model")
		}
		if endpoint := os.Getenv("QUASAR_HOME_MCP_URL"); endpoint != "" && !validEndpoint(endpoint, "http", "https") {
			return cfg, errors.New("invalid QUASAR_HOME_MCP_URL")
		}
	}
	if cfg.LocalProgress.AfterMS < 100 || cfg.LocalProgress.AfterMS > 10000 || cfg.LocalProgress.IntervalMS < 1000 || cfg.LocalProgress.IntervalMS > 10000 {
		return cfg, errors.New("invalid local_progress timing")
	}
	if err := cfg.DialogueContext.Validate(); err != nil {
		return cfg, err
	}
	if err := cfg.AssistantTools.Validate(); err != nil {
		return cfg, err
	}
	if err := cfg.ConversationLog.Validate(); err != nil {
		return cfg, err
	}
	cfg.AirPlay.defaults()
	cfg.SmartTube.defaults()
	if _, err := cfg.AirPlay.receiverConfig(); err != nil {
		return cfg, err
	}
	if _, err := bridge.CompileRouting(cfg.Routing); err != nil {
		return cfg, err
	}
	for k, v := range cfg.TTSReplacements {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return cfg, errors.New("tts_replacements entries must be non-empty")
		}
	}
	return cfg, nil
}
func validEndpoint(raw string, schemes ...string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, scheme := range schemes {
		if u.Scheme == scheme {
			return true
		}
	}
	return false
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type BridgeHealth struct {
	Status                string `json:"status"`
	Mode                  string `json:"mode"`
	ClientFrames          int64  `json:"client_frames"`
	ServerFrames          int64  `json:"server_frames"`
	RoutingRevision       string `json:"routing_revision"`
	RoutingRules          int    `json:"routing_rules"`
	RoutingDefaultRoute   string `json:"routing_default_route"`
	AssistantToolsEnabled bool   `json:"assistant_tools_enabled"`
	DialogueContext       bool   `json:"dialogue_context_enabled"`
	MCPServersConfigured  int    `json:"mcp_servers_configured"`
}

type BridgeRuleInfo struct {
	Index        int
	Name         string
	Route        string
	Disabled     bool
	SystemPrompt string
}

type BridgeManager struct {
	mu  sync.Mutex
	cfg Config
}

func NewBridgeManager(cfg Config) *BridgeManager {
	return &BridgeManager{cfg: cfg}
}

func (m *BridgeManager) ReadRawMap() (map[string]any, error) {
	raw, err := os.ReadFile(m.cfg.BridgeConfigPath)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var data map[string]any
	if err := dec.Decode(&data); err != nil {
		return nil, err
	}
	if cfgFile, ok := data["llm_config_file"].(string); ok && cfgFile != "" {
		llmPath := cfgFile
		if !filepath.IsAbs(llmPath) {
			llmPath = filepath.Join(filepath.Dir(m.cfg.BridgeConfigPath), llmPath)
		}
		if llmBytes, err := os.ReadFile(llmPath); err == nil {
			var llmData map[string]any
			dec2 := json.NewDecoder(bytes.NewReader(llmBytes))
			dec2.UseNumber()
			if dec2.Decode(&llmData) == nil {
				for _, k := range []string{"llm_model", "model"} {
					if v, ok := llmData[k]; ok && data["llm_model"] == nil {
						data["llm_model"] = v
					}
				}
				for _, k := range []string{"llm_base_url", "base_url"} {
					if v, ok := llmData[k]; ok && data["llm_base_url"] == nil {
						data["llm_base_url"] = v
					}
				}
				for _, k := range []string{"llm_max_output_tokens", "max_output_tokens"} {
					if v, ok := llmData[k]; ok && data["llm_max_output_tokens"] == nil {
						data["llm_max_output_tokens"] = v
					}
				}
				for _, k := range []string{"llm_api_key", "api_key"} {
					if v, ok := llmData[k]; ok && data["llm_api_key"] == nil {
						data["llm_api_key"] = v
					}
				}
			}
		}
	}
	return data, nil
}

func marshalPrettyNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (m *BridgeManager) ValidateCandidate(ctx context.Context, rawJSON []byte) error {
	if m.cfg.BridgeBinPath == "" {
		return nil
	}
	tmp, err := os.CreateTemp("", "station-bot-bridge-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(rawJSON); err != nil {
		_ = tmp.Close()
		return err
	}
	_ = tmp.Close()

	cmd := exec.CommandContext(ctx, m.cfg.BridgeBinPath, "-config", tmpName, "-check-config")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("quasar-bridge отклонил конфиг: %s", msg)
	}
	return nil
}

// UpdateAndApply mutates the bridge JSON map, validates via `quasar-bridge -check-config`,
// atomically writes config.json, and runs SIGHUP reload (routingOnly) or systemd restart.
func (m *BridgeManager) UpdateAndApply(ctx context.Context, routingOnly bool, mutate func(map[string]any) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	origBytes, err := os.ReadFile(m.cfg.BridgeConfigPath)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(origBytes))
	dec.UseNumber()
	var data map[string]any
	if err := dec.Decode(&data); err != nil {
		return err
	}

	if err := mutate(data); err != nil {
		return err
	}

	newBytes, err := marshalPrettyNoEscape(data)
	if err != nil {
		return err
	}

	if err := m.ValidateCandidate(ctx, newBytes); err != nil {
		return err
	}

	bakPath := m.cfg.BridgeConfigPath + ".botbak"
	if _, err := os.Stat(bakPath); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(bakPath, origBytes, 0644)
	}

	tmpPath := filepath.Join(filepath.Dir(m.cfg.BridgeConfigPath), ".config.json.bottmp")
	if err := os.WriteFile(tmpPath, newBytes, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, m.cfg.BridgeConfigPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	action := "restart"
	if routingOnly {
		action = "reload"
	}
	cmd := exec.CommandContext(ctx, "systemctl", action, m.cfg.BridgeServiceName)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Rollback on failure
		_ = os.WriteFile(m.cfg.BridgeConfigPath, origBytes, 0644)
		_ = exec.CommandContext(context.Background(), "systemctl", "restart", m.cfg.BridgeServiceName).Run()
		return fmt.Errorf("systemctl %s failed (%s), выполнен откат", action, strings.TrimSpace(string(out)))
	}

	if !routingOnly {
		time.Sleep(800 * time.Millisecond)
		chk := exec.CommandContext(ctx, "systemctl", "is-active", m.cfg.BridgeServiceName)
		if out, err := chk.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "active" {
			_ = os.WriteFile(m.cfg.BridgeConfigPath, origBytes, 0644)
			_ = exec.CommandContext(context.Background(), "systemctl", "restart", m.cfg.BridgeServiceName).Run()
			return fmt.Errorf("quasar-bridge не запустился после изменения, выполнен откат")
		}
	}
	return nil
}

func (m *BridgeManager) Restart(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "systemctl", "restart", m.cfg.BridgeServiceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *BridgeManager) RestoreBackup(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	bakPath := m.cfg.BridgeConfigPath + ".botbak"
	bakBytes, err := os.ReadFile(bakPath)
	if err != nil {
		return fmt.Errorf("чтение %s: %w", bakPath, err)
	}
	if err := m.ValidateCandidate(ctx, bakBytes); err != nil {
		return err
	}
	tmpPath := filepath.Join(filepath.Dir(m.cfg.BridgeConfigPath), ".config.json.bottmp")
	if err := os.WriteFile(tmpPath, bakBytes, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, m.cfg.BridgeConfigPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "restart", m.cfg.BridgeServiceName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *BridgeManager) IsServiceActive(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "systemctl", "is-active", m.cfg.BridgeServiceName).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil && s == "" {
		return "unknown"
	}
	return s
}

func (m *BridgeManager) Health(ctx context.Context) (BridgeHealth, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.BridgeHealthURL, nil)
	if err != nil {
		return BridgeHealth{}, err
	}
	cli := &http.Client{Timeout: 2 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return BridgeHealth{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return BridgeHealth{}, err
	}
	var h BridgeHealth
	if err := json.Unmarshal(body, &h); err != nil {
		return BridgeHealth{}, err
	}
	return h, nil
}

func (m *BridgeManager) ExplainRoute(ctx context.Context, phrase string) (string, error) {
	cmd := exec.CommandContext(ctx, m.cfg.BridgeBinPath, "-config", m.cfg.BridgeConfigPath, "-route", phrase)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil && s == "" {
		return "", err
	}
	return s, nil
}

func (m *BridgeManager) DiscoverModels(ctx context.Context) []string {
	data, err := m.ReadRawMap()
	if err != nil {
		return nil
	}
	baseURL, _ := GetString(data, "llm_base_url")
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil
	}
	envPath := filepath.Join(filepath.Dir(m.cfg.BridgeConfigPath), "runtime.env")
	apiKey := readKeyFromEnvFile(envPath, "QUASAR_LLM_API_KEY")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	cli := &http.Client{Timeout: 3 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	var models []string
	for _, item := range parsed.Data {
		id := strings.TrimSpace(item.ID)
		if id != "" && len(id) <= 60 {
			models = append(models, id)
		}
	}
	sort.Strings(models)
	if len(models) > 12 {
		models = models[:12]
	}
	return models
}

func readKeyFromEnvFile(path, key string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if ok && strings.TrimSpace(k) == key {
			v = strings.TrimSpace(v)
			if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
				v = v[1 : len(v)-1]
			}
			return v
		}
	}
	return ""
}

func ExtractBridgeRules(data map[string]any) []BridgeRuleInfo {
	rawRules, ok := GetAtPath(data, "routing", "rules")
	if !ok {
		return nil
	}
	arr, ok := rawRules.([]any)
	if !ok {
		return nil
	}
	out := make([]BridgeRuleInfo, 0, len(arr))
	for i, item := range arr {
		rm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := rm["name"].(string)
		route, _ := rm["route"].(string)
		disabled, _ := rm["disabled"].(bool)
		prompt, _ := rm["system_prompt"].(string)
		out = append(out, BridgeRuleInfo{
			Index:        i,
			Name:         name,
			Route:        route,
			Disabled:     disabled,
			SystemPrompt: prompt,
		})
	}
	return out
}

func SetBridgeDotPath(data map[string]any, dotPath string, val any) error {
	segs := strings.Split(strings.TrimSpace(dotPath), ".")
	if len(segs) == 0 || segs[0] == "" {
		return errors.New("пустой путь")
	}
	var cur any = data
	for i, seg := range segs[:len(segs)-1] {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				newMap := make(map[string]any)
				node[seg] = newMap
				cur = newMap
			} else {
				cur = next
			}
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return fmt.Errorf("некорректный индекс массива %q на сегменте %d", seg, i)
			}
			cur = node[idx]
		default:
			return fmt.Errorf("сегмент %q не является объектом или массивом", seg)
		}
	}
	last := segs[len(segs)-1]
	obj, ok := cur.(map[string]any)
	if !ok {
		return fmt.Errorf("родитель для %q не является объектом", last)
	}
	obj[last] = val
	return nil
}

func GetBridgeDotPath(data map[string]any, dotPath string) (any, bool) {
	segs := strings.Split(strings.TrimSpace(dotPath), ".")
	if len(segs) == 0 || segs[0] == "" {
		return data, true
	}
	var cur any = data
	for _, seg := range segs {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

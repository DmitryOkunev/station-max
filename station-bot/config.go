package main

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	BotToken          string
	AllowedUsers      map[int64]bool
	StationIP         string
	StationSSHPort    int
	StationSSHUser    string
	StationADBPort    int
	StationMediaPort  int
	StationUID        string
	BridgeConfigPath  string
	BridgeBinPath     string
	BridgeHealthURL   string
	StreamScriptPath  string
	ServerIP          string
	BridgeServiceName string
	DraftPath         string
}

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		if k != "" {
			if _, exists := os.LookupEnv(k); !exists {
				_ = os.Setenv(k, v)
			}
		}
	}
	return sc.Err()
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envIntOrDefault(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func LoadConfig(envPath string) (Config, error) {
	if envPath != "" {
		if err := loadEnvFile(envPath); err != nil {
			return Config{}, err
		}
	}

	allowed := make(map[int64]bool)
	for _, part := range strings.Split(os.Getenv("TELEGRAM_ALLOWED_USERS"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err == nil && id != 0 {
			allowed[id] = true
		}
	}

	return Config{
		BotToken:          strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		AllowedUsers:      allowed,
		StationIP:         envOrDefault("STATION_IP", "192.168.1.50"),
		StationSSHPort:    envIntOrDefault("STATION_SSH_PORT", 2222),
		StationSSHUser:    envOrDefault("STATION_SSH_USER", "root"),
		StationADBPort:    envIntOrDefault("STATION_ADB_PORT", 5555),
		StationMediaPort:  envIntOrDefault("STATION_MEDIA_PORT", 4545),
		StationUID:        envOrDefault("STATION_UID", ""),
		BridgeConfigPath:  envOrDefault("BRIDGE_CONFIG_PATH", "/opt/quasar-bridge/config.json"),
		BridgeBinPath:     envOrDefault("BRIDGE_BIN_PATH", "/opt/quasar-bridge/bin/quasar-bridge"),
		BridgeHealthURL:   envOrDefault("BRIDGE_HEALTH_URL", "http://127.0.0.1:8443/healthz"),
		StreamScriptPath:  envOrDefault("STREAM_SCRIPT_PATH", "/usr/local/bin/station-stream"),
		ServerIP:          envOrDefault("BRIDGE_SERVER_IP", "192.168.1.1"),
		BridgeServiceName: envOrDefault("BRIDGE_SERVICE_NAME", "quasar-bridge"),
		DraftPath:         envOrDefault("BOT_DRAFT_PATH", "/opt/station-bot/user_config_draft.json"),
	}, nil
}

func IsPlaceholderToken(token string) bool {
	if token == "" {
		return true
	}
	low := strings.ToLower(token)
	return strings.Contains(low, "put_your") || strings.Contains(low, "your_bot_token") || !strings.Contains(token, ":")
}


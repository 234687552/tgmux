package config

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type TelegramConfig struct {
	Token        string  `yaml:"token"`
	AllowedUsers []int64 `yaml:"allowed_users"`
}

type BackendConfig struct {
	Command       string   `yaml:"command"`
	Args          []string `yaml:"args"`
	LogDirPattern string   `yaml:"log_dir_pattern"`
	Enabled       *bool    `yaml:"enabled"` // pointer for default true
}

type BackendsConfig struct {
	Claude BackendConfig `yaml:"claude"`
	Codex  BackendConfig `yaml:"codex"`
	Gemini BackendConfig `yaml:"gemini"`
	Bash   BackendConfig `yaml:"bash"`
}

type DirsConfig struct {
	Favorites []string `yaml:"favorites"`
	RecentMax int      `yaml:"recent_max"`
}

type SecurityConfig struct {
	RedactSecrets        bool `yaml:"redact_secrets"`
	ConfigPermissionCheck bool `yaml:"config_permission_check"`
}

type WebConfig struct {
	Enabled bool   `yaml:"enabled"`
	Port    int    `yaml:"port"`
	Bind    string `yaml:"bind"`
}

type MonitorConfig struct {
	PollInterval       time.Duration `yaml:"poll_interval"`
	GroupThrottle      time.Duration `yaml:"group_throttle"`
	PrivateThrottle    time.Duration `yaml:"private_throttle"`
	StatusPollInterval time.Duration `yaml:"status_poll_interval"`
}

type Config struct {
	Telegram TelegramConfig `yaml:"telegram"`
	Backends BackendsConfig `yaml:"backends"`
	Dirs     DirsConfig     `yaml:"dirs"`
	Security SecurityConfig `yaml:"security"`
	Web      WebConfig      `yaml:"web"`
	Monitor  MonitorConfig  `yaml:"monitor"`
}

func defaultConfig() *Config {
	t := true
	return &Config{
		Backends: BackendsConfig{
			Claude: BackendConfig{Command: "claude", Enabled: &t, LogDirPattern: "~/.claude/projects/{path_encoded}/"},
			Codex:  BackendConfig{Command: "codex", Enabled: &t, LogDirPattern: "~/.codex/sessions/{date}/"},
			Gemini: BackendConfig{Command: "gemini", Enabled: &t, LogDirPattern: "~/.gemini/tmp/{hash}/"},
			Bash:   BackendConfig{Enabled: &t},
		},
		Dirs:     DirsConfig{RecentMax: 10},
		Security: SecurityConfig{RedactSecrets: true, ConfigPermissionCheck: true},
		Web:      WebConfig{Port: 3030, Bind: "127.0.0.1"},
		Monitor:  MonitorConfig{PollInterval: 500 * time.Millisecond, GroupThrottle: 3 * time.Second, PrivateThrottle: 1 * time.Second},
	}
}

func Load(path string) (*Config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// 环境变量覆盖 token
	if envToken := os.Getenv("TGMUX_BOT_TOKEN"); envToken != "" {
		cfg.Telegram.Token = envToken
	}

	// 校验
	if cfg.Telegram.Token == "" {
		return nil, fmt.Errorf("telegram.token is required (set in config or TGMUX_BOT_TOKEN env)")
	}
	if len(cfg.Telegram.AllowedUsers) == 0 {
		return nil, fmt.Errorf("telegram.allowed_users must not be empty")
	}

	return cfg, nil
}

func CheckFilePermission(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		slog.Warn("config file permission is not 600, consider restricting access", "path", path, "current_perm", fmt.Sprintf("%o", perm))
	}
}

func (b *BackendConfig) IsEnabled() bool {
	if b.Enabled == nil {
		return true
	}
	return *b.Enabled
}

package backend

import "github.com/user/tgmux/config"

type Type string

const (
	TypeClaude Type = "claude"
	TypeCodex  Type = "codex"
	TypeGemini Type = "gemini"
	TypeBash   Type = "bash"
)

type Backend struct {
	Type       Type
	Command    string
	Args       []string
	LogDirFunc func(projectPath string) string // 返回日志监控目录
}

func AllTypes() []Type {
	return []Type{TypeClaude, TypeCodex, TypeGemini, TypeBash}
}

func Get(t Type, cfg *config.Config) Backend {
	switch t {
	case TypeClaude:
		return newClaude(cfg)
	case TypeCodex:
		return newCodex(cfg)
	case TypeGemini:
		return newGemini(cfg)
	case TypeBash:
		return newBash(cfg)
	default:
		return Backend{Type: t}
	}
}

func IsEnabled(t Type, cfg *config.Config) bool {
	switch t {
	case TypeClaude:
		return cfg.Backends.Claude.IsEnabled()
	case TypeCodex:
		return cfg.Backends.Codex.IsEnabled()
	case TypeGemini:
		return cfg.Backends.Gemini.IsEnabled()
	case TypeBash:
		return cfg.Backends.Bash.IsEnabled()
	default:
		return false
	}
}

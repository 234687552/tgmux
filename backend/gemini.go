package backend

import (
	"os"
	"path/filepath"

	"github.com/user/tgmux/config"
)

func newGemini(cfg *config.Config) Backend {
	bc := cfg.Backends.Gemini
	cmd := bc.Command
	if cmd == "" {
		cmd = "gemini"
	}
	return Backend{
		Type:    TypeGemini,
		Command: cmd,
		Args:    bc.Args,
		LogDirFunc: func(projectPath string) string {
			// 返回 ~/.gemini/tmp/ 目录（hash 子目录需运行时动态定位）
			home, _ := os.UserHomeDir()
			return filepath.Join(home, ".gemini", "tmp")
		},
	}
}

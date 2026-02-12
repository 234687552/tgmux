package backend

import (
	"os"
	"path/filepath"
	"time"

	"github.com/user/tgmux/config"
)

func newCodex(cfg *config.Config) Backend {
	bc := cfg.Backends.Codex
	cmd := bc.Command
	if cmd == "" {
		cmd = "codex"
	}
	return Backend{
		Type:    TypeCodex,
		Command: cmd,
		Args:    bc.Args,
		LogDirFunc: func(projectPath string) string {
			if bc.LogDirPattern != "" && bc.LogDirPattern != "~/.codex/sessions/{date}/" {
				return expandHome(bc.LogDirPattern)
			}
			now := time.Now()
			home, _ := os.UserHomeDir()
			return filepath.Join(home, ".codex", "sessions",
				now.Format("2006"), now.Format("01"), now.Format("02"))
		},
	}
}

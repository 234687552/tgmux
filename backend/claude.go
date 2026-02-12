package backend

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/user/tgmux/config"
)

func newClaude(cfg *config.Config) Backend {
	bc := cfg.Backends.Claude
	cmd := bc.Command
	if cmd == "" {
		cmd = "claude"
	}
	return Backend{
		Type:    TypeClaude,
		Command: cmd,
		Args:    bc.Args,
		LogDirFunc: func(projectPath string) string {
			if bc.LogDirPattern != "" && bc.LogDirPattern != "~/.claude/projects/{path_encoded}/" {
				return expandHome(bc.LogDirPattern)
			}
			// 默认: ~/.claude/projects/-Users-foo-project/
			encoded := strings.ReplaceAll(projectPath, "/", "-")
			home, _ := os.UserHomeDir()
			return filepath.Join(home, ".claude", "projects", encoded)
		},
	}
}

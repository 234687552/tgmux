package backend

import "github.com/user/tgmux/config"

func newBash(cfg *config.Config) Backend {
	bc := cfg.Backends.Bash
	return Backend{
		Type:       TypeBash,
		Command:    bc.Command, // 空则使用默认 shell
		Args:       bc.Args,
		LogDirFunc: nil, // bash 使用 capture-pane，无日志路径
	}
}

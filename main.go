package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/user/tgmux/auth"
	tgbot "github.com/user/tgmux/bot"
	"github.com/user/tgmux/config"
	"github.com/user/tgmux/monitor"
	"github.com/user/tgmux/state"
	"github.com/user/tgmux/tmux"
)

func main() {
	homeDir, _ := os.UserHomeDir()
	defaultConfigPath := filepath.Join(homeDir, ".tgmux", "config.yaml")

	configPath := flag.String("c", defaultConfigPath, "config file path")
	webEnabled := flag.Bool("web", false, "enable web UI (P1)")
	webPort := flag.Int("web-port", 0, "web UI port (overrides config)")
	flag.Parse()

	// 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// 配置文件权限检查
	if cfg.Security.ConfigPermissionCheck {
		config.CheckFilePermission(*configPath)
	}

	// CLI 参数覆盖
	if *webEnabled {
		cfg.Web.Enabled = true
	}
	if *webPort > 0 {
		cfg.Web.Port = *webPort
	}

	slog.Info("tgmux starting",
		"allowed_users", cfg.Telegram.AllowedUsers,
		"web_enabled", cfg.Web.Enabled,
	)

	// 创建 State Store
	statePath := filepath.Join(homeDir, ".tgmux", "state.json")
	store := state.New(statePath, cfg.Dirs.RecentMax)

	// 创建 Tmux Manager
	tmuxMgr := tmux.NewManager()
	if err := tmuxMgr.EnsureSession(); err != nil {
		slog.Error("failed to ensure tmux session", "error", err)
		os.Exit(1)
	}

	// 创建 Auth Checker
	authChecker := auth.New(cfg.Telegram.AllowedUsers)

	// 创建 Dispatcher
	dispatcher := monitor.NewDispatcher(cfg, store, tmuxMgr)

	// 创建 Bot
	b, err := tgbot.New(cfg, store, tmuxMgr, authChecker, dispatcher)
	if err != nil {
		slog.Error("failed to create bot", "error", err)
		os.Exit(1)
	}

	// 启动 Bot (Start 内部会先 recoverBindings)
	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go b.Start(ctx)

	slog.Info("tgmux ready")
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig)

	// Graceful shutdown (10s timeout)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// 1. 停止 polling
	cancel()

	// 2. Drain 串行发送 channel
	drainDone := make(chan struct{})
	go func() {
		b.DrainSendChans()
		close(drainDone)
	}()
	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		slog.Warn("drain send chans timed out")
	}

	// 3. 停止所有监控
	b.Dispatcher().StopAll()

	// 4. Flush 所有 pusher
	b.Pushers().FlushAll(shutdownCtx)
	b.Pushers().StopAll()

	// 5. 保存 state
	store.Close()

	slog.Info("tgmux shutdown complete")
}

package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/user/tgmux/backend"
	"github.com/user/tgmux/config"
	"github.com/user/tgmux/state"
	"github.com/user/tgmux/tmux"
)

// ContentType 区分输出内容类型
type ContentType int

const (
	ContentText       ContentType = iota // 普通文本/最终答案
	ContentThinking                      // 思考过程
	ContentToolUse                       // 工具调用
	ContentToolResult                    // 工具结果
)

// OutputHandler 输出回调
type OutputHandler func(topicKey string, content ParsedContent)

// Monitor 输出监控接口
type Monitor interface {
	Start(ctx context.Context) error
	Stop()
}

// Dispatcher 管理所有活跃监控器
type Dispatcher struct {
	mu       sync.Mutex
	monitors map[string]Monitor
	cfg      *config.Config
	store    *state.Store
	tmuxMgr  *tmux.Manager
}

func NewDispatcher(cfg *config.Config, store *state.Store, tmuxMgr *tmux.Manager) *Dispatcher {
	return &Dispatcher{
		monitors: make(map[string]Monitor),
		cfg:      cfg,
		store:    store,
		tmuxMgr:  tmuxMgr,
	}
}

// StartMonitor 根据 backend 类型创建并启动对应监控器
func (d *Dispatcher) StartMonitor(ctx context.Context, topicKey string, binding state.Binding, handler OutputHandler) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// 如已有监控，先停止
	if existing, ok := d.monitors[topicKey]; ok {
		existing.Stop()
		delete(d.monitors, topicKey)
	}

	var mon Monitor
	bt := backend.Type(binding.Backend)
	be := backend.Get(bt, d.cfg)

	switch bt {
	case backend.TypeClaude, backend.TypeCodex:
		if be.LogDirFunc != nil {
			logDir := be.LogDirFunc(binding.ProjectPath)
			offset, _ := d.store.GetOffset(topicKey)
			mon = NewJSONLMonitor(topicKey, bt, logDir, offset.ByteOffset, offset.File, handler, d.store)
		}
	case backend.TypeGemini:
		if be.LogDirFunc != nil {
			logDir := be.LogDirFunc(binding.ProjectPath)
			offset, _ := d.store.GetOffset(topicKey)
			mon = NewJSONDiffMonitor(topicKey, logDir, offset.MessageCount, time.Now(), handler, d.store)
		}
	case backend.TypeBash:
		mon = NewPaneMonitor(topicKey, binding.WindowID, d.tmuxMgr, d.cfg.Monitor.PollInterval, handler)
	}

	if mon == nil {
		slog.Warn("falling back to capture-pane", "key", topicKey, "backend", binding.Backend)
		mon = NewPaneMonitor(topicKey, binding.WindowID, d.tmuxMgr, d.cfg.Monitor.PollInterval, handler)
	}

	if err := mon.Start(ctx); err != nil {
		if bt != backend.TypeBash {
			slog.Warn("log monitor failed, falling back to capture-pane", "key", topicKey, "error", err)
			mon = NewPaneMonitor(topicKey, binding.WindowID, d.tmuxMgr, d.cfg.Monitor.PollInterval, handler)
			if err2 := mon.Start(ctx); err2 != nil {
				return fmt.Errorf("fallback pane monitor: %w", err2)
			}
		} else {
			return fmt.Errorf("pane monitor: %w", err)
		}
	}

	d.monitors[topicKey] = mon
	slog.Info("monitor started", "key", topicKey, "backend", binding.Backend)
	return nil
}

// StopMonitor 停止指定监控器
func (d *Dispatcher) StopMonitor(topicKey string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if mon, ok := d.monitors[topicKey]; ok {
		mon.Stop()
		delete(d.monitors, topicKey)
		slog.Info("monitor stopped", "key", topicKey)
	}
}

// StopAll 停止所有监控器
func (d *Dispatcher) StopAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, mon := range d.monitors {
		mon.Stop()
		slog.Info("monitor stopped", "key", key)
	}
	d.monitors = make(map[string]Monitor)
}

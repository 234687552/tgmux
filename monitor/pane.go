package monitor

import (
	"context"
	"strings"
	"time"

	"github.com/user/tgmux/tmux"
)

// PaneMonitor 通过 capture-pane 轮询监控 bash 输出
type PaneMonitor struct {
	topicKey     string
	windowID     string
	tmuxMgr      *tmux.Manager
	pollInterval time.Duration
	handler      OutputHandler
	cancel       context.CancelFunc
	lastSnapshot string
}

func NewPaneMonitor(topicKey, windowID string, tmuxMgr *tmux.Manager, pollInterval time.Duration, handler OutputHandler) *PaneMonitor {
	return &PaneMonitor{
		topicKey:     topicKey,
		windowID:     windowID,
		tmuxMgr:      tmuxMgr,
		pollInterval: pollInterval,
		handler:      handler,
	}
}

func (p *PaneMonitor) Start(ctx context.Context) error {
	ctx, p.cancel = context.WithCancel(ctx)
	go p.loop(ctx)
	return nil
}

func (p *PaneMonitor) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *PaneMonitor) loop(ctx context.Context) {
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	// 初始快照
	if snapshot, err := p.tmuxMgr.CapturePaneClean(p.windowID); err == nil {
		p.lastSnapshot = snapshot
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll()
		}
	}
}

func (p *PaneMonitor) poll() {
	current, err := p.tmuxMgr.CapturePaneClean(p.windowID)
	if err != nil {
		return
	}
	if current == p.lastSnapshot {
		return
	}

	newContent := diffSnapshots(p.lastSnapshot, current)
	p.lastSnapshot = current

	if newContent != "" {
		p.handler(p.topicKey, newContent)
	}
}

// diffSnapshots 对比两个快照，提取新增行
func diffSnapshots(old, current string) string {
	oldLines := strings.Split(strings.TrimRight(old, "\n"), "\n")
	newLines := strings.Split(strings.TrimRight(current, "\n"), "\n")

	if len(newLines) <= len(oldLines) {
		// 屏幕可能刷新，检查尾部变化
		common := 0
		for i := 0; i < len(oldLines) && i < len(newLines); i++ {
			if oldLines[len(oldLines)-1-i] == newLines[len(newLines)-1-i] {
				common++
			} else {
				break
			}
		}
		if common == len(newLines) {
			return ""
		}
		diffStart := len(newLines) - common
		if diffStart > 0 {
			changed := newLines[:diffStart]
			var nonEmpty []string
			for _, l := range changed {
				if strings.TrimSpace(l) != "" {
					nonEmpty = append(nonEmpty, l)
				}
			}
			if len(nonEmpty) > 0 {
				return strings.Join(nonEmpty, "\n")
			}
		}
		return ""
	}

	// 新行数多于旧行数：找共同前缀
	matchLen := 0
	for i := 0; i < len(oldLines); i++ {
		if i < len(newLines) && oldLines[i] == newLines[i] {
			matchLen++
		} else {
			break
		}
	}

	added := newLines[matchLen:]
	var nonEmpty []string
	for _, l := range added {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) == 0 {
		return ""
	}
	return strings.Join(nonEmpty, "\n")
}

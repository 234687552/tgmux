package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/user/tgmux/state"
	"github.com/user/tgmux/tmux"
)

// StatusEntry tracks the editable status message for one topic
type StatusEntry struct {
	MessageID int    // Telegram message ID for in-place editing
	LastText  string // last displayed status text (avoid redundant edits)
}

// StatusPoller polls tmux panes and maintains editable status messages per topic.
// Only active for JSONL-monitored backends (claude/codex/gemini).
// Bash backend uses PaneMonitor which already captures terminal changes.
// A nil StatusPoller is safe to call — all methods are no-ops.
type StatusPoller struct {
	tgBot   *tgbot.Bot
	tmuxMgr *tmux.Manager
	pushers *PusherManager
	store   *state.Store
	interval time.Duration

	mu       sync.Mutex
	statuses map[string]*StatusEntry // topicKey -> entry
	cancel   context.CancelFunc
}

// NewStatusPoller creates a status poller. Returns nil if interval <= 0 (disabled).
func NewStatusPoller(tgBot *tgbot.Bot, tmuxMgr *tmux.Manager, pushers *PusherManager, store *state.Store, interval time.Duration) *StatusPoller {
	if interval <= 0 {
		slog.Info("status poller disabled (status_poll_interval not configured or <= 0)")
		return nil
	}
	return &StatusPoller{
		tgBot:    tgBot,
		tmuxMgr:  tmuxMgr,
		pushers:  pushers,
		store:    store,
		interval: interval,
		statuses: make(map[string]*StatusEntry),
	}
}

// Start begins the polling loop
func (sp *StatusPoller) Start(ctx context.Context) {
	if sp == nil {
		return
	}
	ctx, sp.cancel = context.WithCancel(ctx)
	go sp.loop(ctx)
	slog.Info("status poller started", "interval", sp.interval)
}

// Stop cancels the polling loop
func (sp *StatusPoller) Stop() {
	if sp == nil {
		return
	}
	if sp.cancel != nil {
		sp.cancel()
	}
}

// RemoveStatus cleans up the status entry when a topic is unbound
func (sp *StatusPoller) RemoveStatus(key string) {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	delete(sp.statuses, key)
	sp.mu.Unlock()
}

func (sp *StatusPoller) loop(ctx context.Context) {
	ticker := time.NewTicker(sp.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sp.pollAll(ctx)
		}
	}
}

func (sp *StatusPoller) pollAll(ctx context.Context) {
	bindings := sp.store.AllBindings()
	for key, binding := range bindings {
		if binding.Status == "disconnected" {
			continue
		}
		// 跳过 bash 后端：bash 使用 PaneMonitor 已在捕获终端变化，状态轮询会重复
		if binding.Backend == "bash" {
			continue
		}

		sp.pollOne(ctx, key, binding)
	}
}

func (sp *StatusPoller) pollOne(ctx context.Context, key string, binding state.Binding) {
	// Skip if the output queue has pending items (monitor output is flowing)
	if sp.pushers.HasPending(key) {
		return
	}

	// Capture pane content
	text, err := sp.tmuxMgr.CapturePaneClean(binding.WindowID)
	if err != nil {
		return
	}

	// Extract last non-empty line as status
	statusText := extractStatusLine(text)
	if statusText == "" {
		return
	}

	sp.mu.Lock()
	entry, ok := sp.statuses[key]
	if !ok {
		entry = &StatusEntry{}
		sp.statuses[key] = entry
	}
	sp.mu.Unlock()

	// Skip if unchanged
	if statusText == entry.LastText {
		return
	}
	entry.LastText = statusText

	chatID, threadID, _ := parseTopicKey(key)
	if chatID == 0 {
		return
	}

	displayText := fmt.Sprintf("📊 %s", statusText)

	if entry.MessageID == 0 {
		params := &tgbot.SendMessageParams{
			ChatID: chatID,
			Text:   displayText,
		}
		if threadID != 0 {
			params.MessageThreadID = threadID
		}
		resp, err := sp.tgBot.SendMessage(ctx, params)
		if err == nil {
			entry.MessageID = resp.ID
		}
	} else {
		params := &tgbot.EditMessageTextParams{
			ChatID:    chatID,
			MessageID: entry.MessageID,
			Text:      displayText,
		}
		_, err := sp.tgBot.EditMessageText(ctx, params)
		if err != nil {
			slog.Debug("status edit failed, will send new next time", "key", key, "error", err)
			entry.MessageID = 0
		}
	}
}

// extractStatusLine returns the last non-empty line from pane content
func extractStatusLine(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n "), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			if len([]rune(line)) > 200 {
				line = string([]rune(line)[:200]) + "..."
			}
			return line
		}
	}
	return ""
}

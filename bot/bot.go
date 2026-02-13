package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/user/tgmux/auth"
	"github.com/user/tgmux/config"
	"github.com/user/tgmux/monitor"
	"github.com/user/tgmux/state"
	"github.com/user/tgmux/tmux"
)

type Bot struct {
	bot          *bot.Bot
	cfg          *config.Config
	auth         *auth.Checker
	store        *state.Store
	tmux         *tmux.Manager
	dispatcher   *monitor.Dispatcher
	pushers      *PusherManager
	statusPoller *StatusPoller
	states       map[string]*TopicState
	statesMu     sync.Mutex
	sendChans    map[string]chan string
	sendMu       sync.Mutex
}

// TopicState 管理每个 topic 的交互状态
type TopicState struct {
	Phase       string // "idle" | "awaiting_dir" | "awaiting_path_input" | "awaiting_backend" | "bound"
	SelectedDir string
	UpdatedAt   time.Time
}

func New(cfg *config.Config, store *state.Store, tmuxMgr *tmux.Manager, authChecker *auth.Checker, dispatcher *monitor.Dispatcher) (*Bot, error) {
	b := &Bot{
		cfg:        cfg,
		auth:       authChecker,
		store:      store,
		tmux:       tmuxMgr,
		dispatcher: dispatcher,
		states:     make(map[string]*TopicState),
		sendChans:  make(map[string]chan string),
	}

	opts := []bot.Option{
		bot.WithDefaultHandler(b.defaultHandler),
		bot.WithCallbackQueryDataHandler("", bot.MatchTypePrefix, b.handleCallback),
		bot.WithMiddlewares(b.authMiddleware),
	}

	tgBot, err := bot.New(cfg.Telegram.Token, opts...)
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}
	b.bot = tgBot
	b.pushers = NewPusherManager(tgBot, cfg.Security.RedactSecrets)
	b.statusPoller = NewStatusPoller(tgBot, tmuxMgr, b.pushers, store, cfg.Monitor.StatusPollInterval)

	// 注册命令
	b.bot.RegisterHandler(bot.HandlerTypeMessageText, "/new", bot.MatchTypeExact, b.handleNew)
	b.bot.RegisterHandler(bot.HandlerTypeMessageText, "/session", bot.MatchTypePrefix, b.handleSession)
	b.bot.RegisterHandler(bot.HandlerTypeMessageText, "/kill", bot.MatchTypeExact, b.handleKill)
	b.bot.RegisterHandler(bot.HandlerTypeMessageText, "/esc", bot.MatchTypeExact, b.handleEsc)
	b.bot.RegisterHandler(bot.HandlerTypeMessageText, "/enter", bot.MatchTypeExact, b.handleEnter)
	b.bot.RegisterHandler(bot.HandlerTypeMessageText, "/screenshot", bot.MatchTypeExact, b.handleScreenshot)
	b.bot.RegisterHandler(bot.HandlerTypeMessageText, "/cmd", bot.MatchTypePrefix, b.handleCmd)
	b.bot.RegisterHandler(bot.HandlerTypeMessageText, "/dir", bot.MatchTypePrefix, b.handleDir)

	return b, nil
}

// Start 启动 bot polling 并恢复已有绑定的监控
func (b *Bot) Start(ctx context.Context) {
	b.recoverBindings(ctx)
	b.statusPoller.Start(ctx)
	slog.Info("bot starting polling")
	b.bot.Start(ctx)
}

// recoverBindings 恢复已有绑定
func (b *Bot) recoverBindings(ctx context.Context) {
	bindings := b.store.AllBindings()
	if len(bindings) == 0 {
		return
	}

	slog.Info("recovering bindings", "count", len(bindings))
	for key, binding := range bindings {
		if !b.tmux.IsWindowAlive(binding.WindowID) {
			slog.Info("window dead during recovery, marking disconnected", "key", key, "window", binding.WindowID)
			binding.Status = "disconnected"
			b.store.SetBinding(key, binding)
			continue
		}

		if !b.tmux.IsBackendAlive(binding.WindowID) {
			slog.Info("backend exited during recovery, removing binding", "key", key, "window", binding.WindowID)
			b.store.DeleteBinding(key)
			b.store.DeleteOffset(key)
			continue
		}

		b.getOrCreateSendChan(binding.WindowID)

		chatID, threadID, isPrivate := parseTopicKey(key)
		if chatID == 0 {
			continue
		}

		handler := b.pushers.OutputHandler(ctx, key, chatID, threadID, isPrivate, binding.WindowID)
		b.dispatcher.StartMonitor(ctx, key, binding, handler)

		b.setPhase(key, "bound")
		slog.Info("binding recovered", "key", key, "window", binding.WindowID)
	}
}

// StartMonitorForBinding 为新创建/绑定的会话启动监控
func (b *Bot) StartMonitorForBinding(ctx context.Context, key string, binding state.Binding, chatID int64, threadID int) {
	isPrivate := strings.HasPrefix(key, "dm:")
	handler := b.pushers.OutputHandler(ctx, key, chatID, threadID, isPrivate, binding.WindowID)
	b.dispatcher.StartMonitor(ctx, key, binding, handler)
}

// Dispatcher returns the dispatcher for external access
func (b *Bot) Dispatcher() *monitor.Dispatcher {
	return b.dispatcher
}

// Pushers returns the pusher manager for external access
func (b *Bot) Pushers() *PusherManager {
	return b.pushers
}

// authMiddleware 鉴权中间件
func (b *Bot) authMiddleware(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
		var userID int64
		if update.Message != nil {
			userID = update.Message.From.ID
		} else if update.CallbackQuery != nil {
			userID = update.CallbackQuery.From.ID
		}
		if userID == 0 || !b.auth.IsAllowed(userID) {
			return
		}
		next(ctx, tgBot, update)
	}
}

// topicKey 生成绑定 key
func topicKey(chatID int64, chatType string, threadID int) string {
	if chatType == "private" {
		if threadID > 0 {
			return fmt.Sprintf("dm:%d:%d", chatID, threadID)
		}
		return fmt.Sprintf("dm:%d", chatID)
	}
	if threadID > 0 {
		return fmt.Sprintf("topic:%d:%d", chatID, threadID)
	}
	return fmt.Sprintf("general:%d", chatID)
}

func topicKeyFromMessage(msg *models.Message) string {
	threadID := 0
	if msg.MessageThreadID != 0 {
		threadID = msg.MessageThreadID
	}
	return topicKey(msg.Chat.ID, string(msg.Chat.Type), threadID)
}

func topicKeyFromCallback(cq *models.CallbackQuery) string {
	if cq.Message.Message == nil {
		return ""
	}
	return topicKeyFromMessage(cq.Message.Message)
}

// parseTopicKey 从 topicKey 中解析 chatID 和 threadID
func parseTopicKey(key string) (chatID int64, threadID int, isPrivate bool) {
	if strings.HasPrefix(key, "dm:") {
		// dm:{chatID} 或 dm:{chatID}:{threadID}
		n, _ := fmt.Sscanf(key, "dm:%d:%d", &chatID, &threadID)
		if n == 1 {
			threadID = 0
		}
		return chatID, threadID, true
	}
	if strings.HasPrefix(key, "topic:") {
		fmt.Sscanf(key, "topic:%d:%d", &chatID, &threadID)
		return chatID, threadID, false
	}
	if strings.HasPrefix(key, "general:") {
		fmt.Sscanf(key, "general:%d", &chatID)
		return chatID, 0, false
	}
	return 0, 0, false
}

// unbind 清理绑定及相关资源
func (b *Bot) unbind(key string, binding state.Binding) {
	b.store.DeleteBinding(key)
	b.store.DeleteOffset(key)
	b.closeSendChan(binding.WindowID)
	b.dispatcher.StopMonitor(key)
	b.pushers.StopPusher(key)
	b.statusPoller.RemoveStatus(key)
	b.setPhase(key, "idle")
}

func (b *Bot) getOrCreateState(key string) *TopicState {
	b.statesMu.Lock()
	defer b.statesMu.Unlock()
	s, ok := b.states[key]
	if !ok {
		s = &TopicState{Phase: "idle", UpdatedAt: time.Now()}
		b.states[key] = s
	}
	if s.Phase != "idle" && s.Phase != "bound" && time.Since(s.UpdatedAt) > 5*time.Minute {
		s.Phase = "idle"
		s.SelectedDir = ""
	}
	return s
}

func (b *Bot) setPhase(key string, phase string) {
	b.statesMu.Lock()
	defer b.statesMu.Unlock()
	s, ok := b.states[key]
	if !ok {
		s = &TopicState{}
		b.states[key] = s
	}
	s.Phase = phase
	s.UpdatedAt = time.Now()
}

func (b *Bot) getOrCreateSendChan(windowID string) chan string {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	ch, ok := b.sendChans[windowID]
	if !ok {
		ch = make(chan string, 100)
		b.sendChans[windowID] = ch
		go b.sendLoop(windowID, ch)
	}
	return ch
}

func (b *Bot) sendLoop(windowID string, ch chan string) {
	for text := range ch {
		if err := b.tmux.SendText(windowID, text); err != nil {
			slog.Error("send to tmux failed", "window", windowID, "error", err)
		}
	}
}

func (b *Bot) closeSendChan(windowID string) {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	if ch, ok := b.sendChans[windowID]; ok {
		close(ch)
		delete(b.sendChans, windowID)
	}
}

// DrainSendChans 优雅关闭所有发送 channel
func (b *Bot) DrainSendChans() {
	b.sendMu.Lock()
	chans := make(map[string]chan string, len(b.sendChans))
	for k, v := range b.sendChans {
		chans[k] = v
	}
	b.sendMu.Unlock()
	for _, ch := range chans {
		close(ch)
	}
}

func (b *Bot) sendReply(ctx context.Context, msg *models.Message, text string) {
	params := &bot.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   text,
	}
	if msg.MessageThreadID != 0 {
		params.MessageThreadID = msg.MessageThreadID
	}
	b.bot.SendMessage(ctx, params)
}

func (b *Bot) sendReplyWithKeyboard(ctx context.Context, msg *models.Message, text string, kb models.InlineKeyboardMarkup) {
	params := &bot.SendMessageParams{
		ChatID:      msg.Chat.ID,
		Text:        text,
		ReplyMarkup: kb,
	}
	if msg.MessageThreadID != 0 {
		params.MessageThreadID = msg.MessageThreadID
	}
	b.bot.SendMessage(ctx, params)
}

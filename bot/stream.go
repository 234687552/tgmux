package bot

import (
	"context"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/user/tgmux/monitor"
	"github.com/user/tgmux/sanitize"
)

// RateLimiter implements global 429 rate limiting across all pushers
type RateLimiter struct {
	pauseUntil atomic.Int64 // unix timestamp ms
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{}
}

// Wait blocks until the 429 pause period expires
func (r *RateLimiter) Wait(ctx context.Context) error {
	for {
		until := r.pauseUntil.Load()
		if until == 0 {
			return nil
		}
		now := time.Now().UnixMilli()
		if now >= until {
			return nil
		}
		wait := time.Duration(until-now) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// BackOff sets the global pause until time with jitter
func (r *RateLimiter) BackOff(retryAfterSec int) {
	if retryAfterSec <= 0 {
		retryAfterSec = 1
	}
	if retryAfterSec > 30 {
		retryAfterSec = 30
	}
	jitter := float64(retryAfterSec) * (0.8 + rand.Float64()*0.4)
	until := time.Now().Add(time.Duration(jitter*1000) * time.Millisecond).UnixMilli()
	r.pauseUntil.Store(until)
}

// MessageTask represents a single message to send to Telegram
type MessageTask struct {
	Text        string
	ContentType monitor.ContentType
}

// StreamPusher sends messages to a Telegram chat via a FIFO queue.
// Each message is sent as a new Telegram message (no editMessage).
type StreamPusher struct {
	chatID      int64
	threadID    int
	tgBot       *tgbot.Bot
	rateLimiter *RateLimiter
	redact      bool

	queue  chan MessageTask
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewStreamPusher(chatID int64, threadID int, tgBot *tgbot.Bot, rl *RateLimiter, redact bool) *StreamPusher {
	return &StreamPusher{
		chatID:      chatID,
		threadID:    threadID,
		tgBot:       tgBot,
		rateLimiter: rl,
		redact:      redact,
		queue:       make(chan MessageTask, 100),
	}
}

// Start begins the queue worker
func (p *StreamPusher) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	p.wg.Add(1)
	go p.worker(ctx)
}

// Stop cancels the worker and waits for it to drain
func (p *StreamPusher) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// Enqueue adds a message task to the queue
func (p *StreamPusher) Enqueue(task MessageTask) {
	select {
	case p.queue <- task:
	default:
		slog.Warn("message queue full, dropping", "chat", p.chatID)
	}
}

func (p *StreamPusher) worker(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// Drain remaining messages
			p.drain()
			return
		case task := <-p.queue:
			p.sendMessage(ctx, task)
		}
	}
}

func (p *StreamPusher) drain() {
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		select {
		case task := <-p.queue:
			p.sendMessage(drainCtx, task)
		default:
			return
		}
	}
}

func (p *StreamPusher) sendMessage(ctx context.Context, task MessageTask) {
	text := sanitize.Redact(task.Text, p.redact)
	if strings.TrimSpace(text) == "" {
		return
	}

	// Split long messages
	chunks := splitMessage(text, 4096)
	for _, chunk := range chunks {
		if err := p.rateLimiter.Wait(ctx); err != nil {
			return
		}

		params := &tgbot.SendMessageParams{
			ChatID: p.chatID,
			Text:   chunk,
		}
		if p.threadID != 0 {
			params.MessageThreadID = p.threadID
		}

		resp, err := p.tgBot.SendMessage(ctx, params)
		if err != nil {
			if is429(err) {
				p.rateLimiter.BackOff(1)
				slog.Warn("429 on sendMessage", "chat", p.chatID)
			} else {
				slog.Error("sendMessage failed", "error", err)
			}
			return
		}
		slog.Info("message sent", "chat", p.chatID, "msgID", resp.ID, "textLen", len(chunk), "type", task.ContentType)
	}
}

// is429 checks if error is a Telegram 429 rate limit error
func is429(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "Too Many Requests")
}

// splitMessage splits text into chunks fitting Telegram's limit, preferring newline boundaries
func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > maxLen {
		splitIdx := findSplitPoint(text, maxLen)
		chunks = append(chunks, text[:splitIdx])
		text = text[splitIdx:]
	}
	if len(text) > 0 {
		chunks = append(chunks, text)
	}
	return chunks
}

// findSplitPoint finds a good split point near maxLen respecting code blocks and newlines
func findSplitPoint(text string, maxLen int) int {
	if maxLen >= len(text) {
		return len(text)
	}

	sub := text[:maxLen]

	// Look for last ``` before maxLen
	lastFence := strings.LastIndex(sub, "```")
	if lastFence > maxLen/2 {
		nlIdx := strings.Index(text[lastFence:], "\n")
		if nlIdx >= 0 {
			return lastFence + nlIdx + 1
		}
		return lastFence
	}

	// Look for last newline before maxLen
	lastNL := strings.LastIndex(sub, "\n")
	if lastNL > maxLen/2 {
		return lastNL + 1
	}

	return maxLen
}

// PusherManager manages all active StreamPushers
type PusherManager struct {
	mu      sync.Mutex
	pushers map[string]*StreamPusher
	tgBot   *tgbot.Bot
	rl      *RateLimiter
	redact  bool
}

func NewPusherManager(tgBot *tgbot.Bot, redact bool) *PusherManager {
	return &PusherManager{
		pushers: make(map[string]*StreamPusher),
		tgBot:   tgBot,
		rl:      NewRateLimiter(),
		redact:  redact,
	}
}

// GetOrCreate returns existing pusher or creates a new one
func (pm *PusherManager) GetOrCreate(ctx context.Context, topicKey string, chatID int64, threadID int) *StreamPusher {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if p, ok := pm.pushers[topicKey]; ok {
		return p
	}

	p := NewStreamPusher(chatID, threadID, pm.tgBot, pm.rl, pm.redact)
	p.Start(ctx)
	pm.pushers[topicKey] = p
	return p
}

// StopPusher stops a specific pusher
func (pm *PusherManager) StopPusher(topicKey string) {
	pm.mu.Lock()
	p, ok := pm.pushers[topicKey]
	if ok {
		delete(pm.pushers, topicKey)
	}
	pm.mu.Unlock()
	if ok {
		p.Stop()
	}
}

// FlushAll is a no-op for queue-based pushers (drain happens in Stop)
func (pm *PusherManager) FlushAll(ctx context.Context) {}

// StopAll stops all active pushers
func (pm *PusherManager) StopAll() {
	pm.mu.Lock()
	all := make(map[string]*StreamPusher, len(pm.pushers))
	for k, v := range pm.pushers {
		all[k] = v
	}
	pm.pushers = make(map[string]*StreamPusher)
	pm.mu.Unlock()

	for _, p := range all {
		p.Stop()
	}
}

// OutputHandler returns a monitor.OutputHandler that routes to the correct pusher
func (pm *PusherManager) OutputHandler(ctx context.Context, topicKey string, chatID int64, threadID int, isPrivate bool, windowID string) monitor.OutputHandler {
	return func(key string, text string, contentType monitor.ContentType) {
		// Check for confirm prompts and send keyboard
		if monitor.DetectConfirmPrompt(text) {
			kb := ConfirmKeyboard(windowID)
			params := &tgbot.SendMessageParams{
				ChatID:      chatID,
				Text:        "🔐 检测到权限确认请求：",
				ReplyMarkup: kb,
			}
			if threadID != 0 {
				params.MessageThreadID = threadID
			}
			pm.tgBot.SendMessage(ctx, params)
		}

		p := pm.GetOrCreate(ctx, topicKey, chatID, threadID)

		switch contentType {
		case monitor.ContentThinking:
			p.Enqueue(MessageTask{Text: "💭 " + text, ContentType: contentType})
		case monitor.ContentText:
			p.Enqueue(MessageTask{Text: text, ContentType: contentType})
		}
	}
}

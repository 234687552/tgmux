package bot

import (
	"context"
	"fmt"
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

// BackOff sets the global pause until time with exponential backoff + jitter
func (r *RateLimiter) BackOff(retryAfterSec int) {
	if retryAfterSec <= 0 {
		retryAfterSec = 1
	}
	if retryAfterSec > 30 {
		retryAfterSec = 30
	}
	// Add jitter +/-20%
	jitter := float64(retryAfterSec) * (0.8 + rand.Float64()*0.4)
	until := time.Now().Add(time.Duration(jitter*1000) * time.Millisecond).UnixMilli()
	r.pauseUntil.Store(until)
}

// StreamPusher manages streaming output to a Telegram chat via editMessage
type StreamPusher struct {
	chatID      int64
	threadID    int
	tgBot       *tgbot.Bot
	rateLimiter *RateLimiter
	redact      bool
	throttle    time.Duration // group: 3s, private: 1s

	mu            sync.Mutex
	buffer        string // overlay buffer for current streaming message
	dirty         bool
	currentMsgID  int    // current message being edited
	currentText   string // current text in the message
	finalQueue    []string
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

func NewStreamPusher(chatID int64, threadID int, tgBot *tgbot.Bot, rl *RateLimiter, redact bool, throttle time.Duration) *StreamPusher {
	return &StreamPusher{
		chatID:      chatID,
		threadID:    threadID,
		tgBot:       tgBot,
		rateLimiter: rl,
		redact:      redact,
		throttle:    throttle,
	}
}

// Start begins the push loop
func (p *StreamPusher) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	p.wg.Add(1)
	go p.pushLoop(ctx)
}

// Stop stops the push loop and flushes remaining content
func (p *StreamPusher) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// Push adds new content to the buffer
func (p *StreamPusher) Push(text string) {
	if text == "" {
		return
	}
	p.mu.Lock()
	if p.currentText != "" {
		p.buffer = p.currentText + "\n" + text
	} else {
		if p.buffer != "" {
			p.buffer = p.buffer + "\n" + text
		} else {
			p.buffer = text
		}
	}
	p.dirty = true
	p.mu.Unlock()
}

// Finalize sends the current content as a final message (no more edits)
func (p *StreamPusher) Finalize(text string) {
	p.mu.Lock()
	if text != "" {
		p.finalQueue = append(p.finalQueue, text)
	}
	if len(p.finalQueue) > 5 {
		// Cap at 5, summarize
		skipped := len(p.finalQueue) - 5
		p.finalQueue = p.finalQueue[skipped:]
		p.finalQueue = append([]string{fmt.Sprintf("[已省略 %d 条中间输出]", skipped)}, p.finalQueue...)
	}
	p.mu.Unlock()
}

// Flush sends all pending content immediately
func (p *StreamPusher) Flush(ctx context.Context) {
	p.mu.Lock()
	buf := p.buffer
	dirty := p.dirty
	finals := p.finalQueue
	p.buffer = ""
	p.dirty = false
	p.finalQueue = nil
	p.mu.Unlock()

	if dirty && buf != "" {
		p.sendOrEdit(ctx, buf)
	}
	for _, text := range finals {
		p.sendNew(ctx, text)
	}
}

func (p *StreamPusher) pushLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.throttle)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final flush
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			p.Flush(flushCtx)
			cancel()
			return

		case <-ticker.C:
			p.mu.Lock()
			buf := p.buffer
			dirty := p.dirty
			finals := p.finalQueue
			p.buffer = ""
			p.dirty = false
			p.finalQueue = nil
			p.mu.Unlock()

			// Send finalized messages first
			for _, text := range finals {
				p.sendNew(ctx, text)
			}

			if dirty && buf != "" {
				p.sendOrEdit(ctx, buf)
			}
		}
	}
}

func (p *StreamPusher) sendOrEdit(ctx context.Context, text string) {
	text = sanitize.Redact(text, p.redact)

	// Check if we need to split (>3800 chars)
	if len(text) > 3800 {
		splitIdx := findCodeBlockBoundary(text, 3800)
		first := text[:splitIdx]
		rest := text[splitIdx:]

		// Finalize current message with first part
		if p.currentMsgID > 0 {
			p.editMessage(ctx, first)
		} else {
			p.sendNew(ctx, first)
		}
		// Start new message for the rest
		p.currentMsgID = 0
		p.currentText = ""
		p.mu.Lock()
		p.buffer = rest
		p.dirty = true
		p.mu.Unlock()
		return
	}

	if p.currentMsgID > 0 {
		p.editMessage(ctx, text)
	} else {
		p.sendNew(ctx, text)
	}
}

func (p *StreamPusher) sendNew(ctx context.Context, text string) {
	text = sanitize.Redact(text, p.redact)
	if text == "" {
		return
	}
	if len(text) > 4096 {
		text = text[:4096]
	}

	if err := p.rateLimiter.Wait(ctx); err != nil {
		return
	}

	params := &tgbot.SendMessageParams{
		ChatID: p.chatID,
		Text:   text,
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

	p.mu.Lock()
	p.currentMsgID = resp.ID
	p.currentText = text
	p.mu.Unlock()
}

func (p *StreamPusher) editMessage(ctx context.Context, text string) {
	if text == "" || text == p.currentText {
		return
	}
	if len(text) > 4096 {
		text = text[:4096]
	}

	if err := p.rateLimiter.Wait(ctx); err != nil {
		return
	}

	params := &tgbot.EditMessageTextParams{
		ChatID:    p.chatID,
		MessageID: p.currentMsgID,
		Text:      text,
	}

	_, err := p.tgBot.EditMessageText(ctx, params)
	if err != nil {
		if is429(err) {
			p.rateLimiter.BackOff(2)
			slog.Warn("429 on editMessage", "chat", p.chatID)
		} else {
			slog.Warn("editMessage failed", "error", err)
		}
		return
	}

	p.mu.Lock()
	p.currentText = text
	p.mu.Unlock()
}

// is429 checks if error is a Telegram 429 rate limit error
func is429(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "Too Many Requests")
}

// findCodeBlockBoundary finds a good split point near maxLen respecting code blocks
func findCodeBlockBoundary(text string, maxLen int) int {
	if maxLen >= len(text) {
		return len(text)
	}

	// Look for last ``` before maxLen
	sub := text[:maxLen]
	lastFence := strings.LastIndex(sub, "```")
	if lastFence > maxLen/2 {
		// Find the end of this code block line
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

// ensureClosedCodeBlocks ensures markdown code blocks are properly closed
func ensureClosedCodeBlocks(text string) string {
	count := strings.Count(text, "```")
	if count%2 != 0 {
		text += "\n```"
	}
	return text
}

// PusherManager manages all active StreamPushers
type PusherManager struct {
	mu       sync.Mutex
	pushers  map[string]*StreamPusher // topicKey -> StreamPusher
	tgBot    *tgbot.Bot
	rl       *RateLimiter
	redact   bool
	groupThr time.Duration
	privThr  time.Duration
}

func NewPusherManager(tgBot *tgbot.Bot, redact bool, groupThrottle, privateThrottle time.Duration) *PusherManager {
	return &PusherManager{
		pushers:  make(map[string]*StreamPusher),
		tgBot:    tgBot,
		rl:       NewRateLimiter(),
		redact:   redact,
		groupThr: groupThrottle,
		privThr:  privateThrottle,
	}
}

// GetOrCreate returns existing pusher or creates a new one
func (pm *PusherManager) GetOrCreate(ctx context.Context, topicKey string, chatID int64, threadID int, isPrivate bool) *StreamPusher {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if p, ok := pm.pushers[topicKey]; ok {
		return p
	}

	throttle := pm.groupThr
	if isPrivate {
		throttle = pm.privThr
	}

	p := NewStreamPusher(chatID, threadID, pm.tgBot, pm.rl, pm.redact, throttle)
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

// FlushAll flushes all active pushers
func (pm *PusherManager) FlushAll(ctx context.Context) {
	pm.mu.Lock()
	all := make(map[string]*StreamPusher, len(pm.pushers))
	for k, v := range pm.pushers {
		all[k] = v
	}
	pm.mu.Unlock()

	for _, p := range all {
		p.Flush(ctx)
	}
}

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
	return func(key string, text string) {
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

		p := pm.GetOrCreate(ctx, topicKey, chatID, threadID, isPrivate)
		p.Push(text)
	}
}

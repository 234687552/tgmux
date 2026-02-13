package bot

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
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
	ToolUseID   string // for tool_result pairing
	ToolName    string // tool name for result stats
}

// StreamPusher sends messages to a Telegram chat via a FIFO queue.
// Each message is sent as a new Telegram message (no editMessage).
type StreamPusher struct {
	chatID      int64
	threadID    int
	tgBot       *tgbot.Bot
	rateLimiter *RateLimiter
	redact      bool

	queue      chan MessageTask
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	toolMsgIDs   map[string]int    // tool_use_id → Telegram message_id for edit pairing
	toolNames    map[string]string // tool_use_id → tool name
	toolMsgTexts map[string]string // tool_use_id → original sent text
}

func NewStreamPusher(chatID int64, threadID int, tgBot *tgbot.Bot, rl *RateLimiter, redact bool) *StreamPusher {
	return &StreamPusher{
		chatID:      chatID,
		threadID:    threadID,
		tgBot:       tgBot,
		rateLimiter: rl,
		redact:      redact,
		queue:       make(chan MessageTask, 100),
		toolMsgIDs:   make(map[string]int),
		toolNames:    make(map[string]string),
		toolMsgTexts: make(map[string]string),
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

// sendWithRetry sends a message with HTML ParseMode, falling back to plain text on format errors, and retrying on 429
func (p *StreamPusher) sendWithRetry(ctx context.Context, params *tgbot.SendMessageParams) (*models.Message, error) {
	resp, err := p.tgBot.SendMessage(ctx, params)
	if err == nil {
		return resp, nil
	}

	retryAfter := parseRetryAfter(err)
	if retryAfter > 0 {
		// 429: wait and retry
		p.rateLimiter.BackOff(retryAfter)
		if waitErr := p.rateLimiter.Wait(ctx); waitErr != nil {
			return nil, waitErr
		}
		return p.tgBot.SendMessage(ctx, params)
	}

	// Non-429 error with ParseMode set: retry as plain text
	if params.ParseMode != "" {
		slog.Warn("send with ParseMode failed, retrying plain text", "error", err)
		params.ParseMode = ""
		return p.tgBot.SendMessage(ctx, params)
	}

	return nil, err
}

// editWithRetry edits a message with retry on 429 and plain text fallback on format errors
func (p *StreamPusher) editWithRetry(ctx context.Context, params *tgbot.EditMessageTextParams) (*models.Message, error) {
	resp, err := p.tgBot.EditMessageText(ctx, params)
	if err == nil {
		return resp, nil
	}

	retryAfter := parseRetryAfter(err)
	if retryAfter > 0 {
		// 429: wait and retry
		p.rateLimiter.BackOff(retryAfter)
		if waitErr := p.rateLimiter.Wait(ctx); waitErr != nil {
			return nil, waitErr
		}
		return p.tgBot.EditMessageText(ctx, params)
	}

	// Non-429 error with ParseMode set: retry as plain text
	if params.ParseMode != "" {
		slog.Warn("editMessageText with ParseMode failed, retrying plain text", "error", err)
		params.ParseMode = ""
		return p.tgBot.EditMessageText(ctx, params)
	}

	return nil, err
}

func (p *StreamPusher) worker(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			p.drain()
			return
		case task := <-p.queue:
			merged, overflow := p.tryMerge(task)
			p.sendMessage(ctx, merged)
			if overflow != nil {
				p.sendMessage(ctx, *overflow)
			}
		}
	}
}

// tryMerge attempts to merge consecutive same-type text messages from the queue
func (p *StreamPusher) tryMerge(first MessageTask) (MessageTask, *MessageTask) {
	// Only merge text and thinking messages
	if first.ContentType != monitor.ContentText && first.ContentType != monitor.ContentThinking {
		return first, nil
	}

	const mergeMax = 3800
	text := first.Text

	for {
		select {
		case next := <-p.queue:
			if next.ContentType != first.ContentType || utf8.RuneCountInString(text)+utf8.RuneCountInString(next.Text)+2 > mergeMax {
				// Can't merge - return overflow
				return MessageTask{Text: text, ContentType: first.ContentType}, &next
			}
			text += "\n\n" + next.Text
		default:
			// No more messages in queue
			return MessageTask{Text: text, ContentType: first.ContentType}, nil
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

	// tool_result: try to edit the paired tool_use message
	if task.ContentType == monitor.ContentToolResult && task.ToolUseID != "" {
		if msgID, ok := p.toolMsgIDs[task.ToolUseID]; ok {
			origText := p.toolMsgTexts[task.ToolUseID]
			delete(p.toolMsgIDs, task.ToolUseID)
			delete(p.toolNames, task.ToolUseID)
			delete(p.toolMsgTexts, task.ToolUseID)
			p.editToolMessage(ctx, msgID, origText, text)
			return
		}
	}

	// Split long messages
	chunks := splitMessage(text, 4096)
	for i, chunk := range chunks {
		if err := p.rateLimiter.Wait(ctx); err != nil {
			return
		}

		// Apply formatting based on content type
		var parseMode models.ParseMode
		switch task.ContentType {
		case monitor.ContentText:
			chunk = toHTML(chunk)
			parseMode = models.ParseModeHTML
		case monitor.ContentThinking:
			// Already has HTML blockquote tags from OutputHandler
			parseMode = models.ParseModeHTML
		case monitor.ContentToolUse, monitor.ContentToolResult:
			chunk = escapeHTML(chunk)
			parseMode = models.ParseModeHTML
		}

		params := &tgbot.SendMessageParams{
			ChatID:             p.chatID,
			Text:               chunk,
			ParseMode:          parseMode,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: boolPtr(true)},
		}
		if p.threadID != 0 {
			params.MessageThreadID = p.threadID
		}

		resp, err := p.sendWithRetry(ctx, params)
		if err != nil {
			slog.Error("sendMessage failed", "error", err)
			return
		}
		slog.Info("message sent", "chat", p.chatID, "thread", p.threadID, "msgID", resp.ID, "textLen", len(chunk), "type", task.ContentType)

		// tool_use: record the last chunk's msg ID + text for later edit pairing
		if task.ContentType == monitor.ContentToolUse && task.ToolUseID != "" && i == len(chunks)-1 {
			p.toolMsgIDs[task.ToolUseID] = resp.ID
			p.toolNames[task.ToolUseID] = task.ToolName
			p.toolMsgTexts[task.ToolUseID] = chunk
		}
	}
}

func (p *StreamPusher) editToolMessage(ctx context.Context, msgID int, origText string, resultText string) {
	if err := p.rateLimiter.Wait(ctx); err != nil {
		return
	}

	newText := origText + "\n" + escapeHTML(resultText)
	if origText == "" {
		newText = escapeHTML(resultText)
	}

	if utf8.RuneCountInString(newText) > 4096 {
		newText = truncateRunes(newText, 4093) + "..."
	}

	params := &tgbot.EditMessageTextParams{
		ChatID:             p.chatID,
		MessageID:          msgID,
		Text:               newText,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: boolPtr(true)},
	}

	_, err := p.editWithRetry(ctx, params)
	if err != nil {
		slog.Warn("editMessageText failed, sending as new message", "error", err)
		sendParams := &tgbot.SendMessageParams{
			ChatID:             p.chatID,
			Text:               escapeHTML(resultText),
			ParseMode:          models.ParseModeHTML,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: boolPtr(true)},
		}
		if p.threadID != 0 {
			sendParams.MessageThreadID = p.threadID
		}
		p.sendWithRetry(ctx, sendParams)
	}
}

// parseRetryAfter extracts RetryAfter seconds from TooManyRequestsError, returns 0 if not a 429
func parseRetryAfter(err error) int {
	var tooMany *tgbot.TooManyRequestsError
	if errors.As(err, &tooMany) {
		return tooMany.RetryAfter
	}
	return 0
}

func boolPtr(b bool) *bool { return &b }

// splitMessage splits text into chunks fitting Telegram's limit (maxLen in runes), preferring newline boundaries
func splitMessage(text string, maxLen int) []string {
	if utf8.RuneCountInString(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for utf8.RuneCountInString(text) > maxLen {
		splitIdx := findSplitPoint(text, maxLen)
		chunks = append(chunks, text[:splitIdx])
		text = text[splitIdx:]
	}
	if len(text) > 0 {
		chunks = append(chunks, text)
	}
	return chunks
}

// findSplitPoint finds a good byte-index split point where the prefix has at most maxLen runes,
// respecting code blocks and newlines
func findSplitPoint(text string, maxLen int) int {
	runeCount := utf8.RuneCountInString(text)
	if maxLen >= runeCount {
		return len(text)
	}

	// Find the byte offset corresponding to maxLen runes
	byteLimit := runeByteOffset(text, maxLen)
	sub := text[:byteLimit]

	// Look for last ``` before byteLimit
	lastFence := strings.LastIndex(sub, "```")
	if lastFence > byteLimit/2 {
		nlIdx := strings.Index(text[lastFence:], "\n")
		if nlIdx >= 0 {
			return lastFence + nlIdx + 1
		}
		return lastFence
	}

	// Look for last newline before byteLimit
	lastNL := strings.LastIndex(sub, "\n")
	if lastNL > byteLimit/2 {
		return lastNL + 1
	}

	return byteLimit
}

// runeByteOffset returns the byte index of the n-th rune in s.
// If n >= rune count, returns len(s).
func runeByteOffset(s string, n int) int {
	i := 0
	for count := 0; count < n && i < len(s); count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return i
}

// truncateRunes returns the first n runes of s as a string.
func truncateRunes(s string, n int) string {
	return s[:runeByteOffset(s, n)]
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

// HasPending checks if a pusher for the given topic has items in its queue
func (pm *PusherManager) HasPending(topicKey string) bool {
	pm.mu.Lock()
	p, ok := pm.pushers[topicKey]
	pm.mu.Unlock()
	return ok && len(p.queue) > 0
}

// OutputHandler returns a monitor.OutputHandler that routes to the correct pusher
func (pm *PusherManager) OutputHandler(ctx context.Context, topicKey string, chatID int64, threadID int, isPrivate bool, windowID string) monitor.OutputHandler {
	return func(key string, content monitor.ParsedContent) {
		// Check for interactive UI (multi-choice menus, selectors)
		if monitor.DetectInteractiveUI(content.Text) {
			kb := InteractiveKeyboard(windowID)
			params := &tgbot.SendMessageParams{
				ChatID:      chatID,
				Text:        "🎮 检测到交互式界面：",
				ReplyMarkup: kb,
			}
			if threadID != 0 {
				params.MessageThreadID = threadID
			}
			pm.tgBot.SendMessage(ctx, params)
		} else if monitor.DetectConfirmPrompt(content.Text) {
			// Check for simple confirm prompts (y/n)
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

		switch content.Type {
		case monitor.ContentThinking:
			formatted := "<blockquote expandable>💭 " + escapeHTML(content.Text) + "</blockquote>"
			p.Enqueue(MessageTask{Text: formatted, ContentType: content.Type})
		case monitor.ContentText:
			p.Enqueue(MessageTask{Text: content.Text, ContentType: content.Type})
		case monitor.ContentToolUse:
			p.Enqueue(MessageTask{
				Text:        "🔧 " + content.Text,
				ContentType: content.Type,
				ToolUseID:   content.ToolUseID,
				ToolName:    content.ToolName,
			})
		case monitor.ContentToolResult:
			p.Enqueue(MessageTask{
				Text:        content.Text,
				ContentType: content.Type,
				ToolUseID:   content.ToolUseID,
			})
		}
	}
}

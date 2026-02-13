package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/user/tgmux/backend"
	"github.com/user/tgmux/state"
)

// fileTracker 跟踪单个 JSONL 文件的读取进度
type fileTracker struct {
	byteOffset int64
}

// JSONLMonitor 通过 fsnotify 监听日志目录，增量读取 JSONL 文件
type JSONLMonitor struct {
	topicKey      string
	backendType   backend.Type
	logDir        string
	handler       OutputHandler
	store         *state.Store
	cancel        context.CancelFunc
	mu            sync.Mutex
	trackedFiles  map[string]*fileTracker // path → tracker，支持多文件并发跟踪
	mainFile      string                  // 主会话文件（用于持久化 offset）
	sessionUUID   string                  // 当前会话的 UUID，用于过滤其他会话的文件
	watchedPaths  map[string]struct{}
	parseErrors   int
	baselineFiles map[string]struct{} // 启动时已存在的文件（仅新会话使用）
	pendingTools  map[string]string   // tool_use_id → tool name，跨 readIncremental 持久化
}

func NewJSONLMonitor(topicKey string, bt backend.Type, logDir string, byteOffset int64, currentFile string, handler OutputHandler, store *state.Store) *JSONLMonitor {
	m := &JSONLMonitor{
		topicKey:     topicKey,
		backendType:  bt,
		logDir:       logDir,
		handler:      handler,
		store:        store,
		trackedFiles: make(map[string]*fileTracker),
		watchedPaths: make(map[string]struct{}),
		pendingTools: make(map[string]string),
	}
	// 恢复已有文件的 offset
	if currentFile != "" {
		m.trackedFiles[currentFile] = &fileTracker{byteOffset: byteOffset}
		m.mainFile = currentFile
		m.sessionUUID = extractSessionUUID(currentFile)
	}
	return m
}

// extractSessionUUID 从文件路径中提取会话 UUID
// 主文件: .../{uuid}.jsonl → uuid
// subagent: .../{uuid}/subagents/agent-xxx.jsonl → uuid
func extractSessionUUID(path string) string {
	dir := filepath.Dir(path)
	base := filepath.Base(dir)

	// subagent 文件: .../{uuid}/subagents/agent-xxx.jsonl
	if base == "subagents" {
		return filepath.Base(filepath.Dir(dir))
	}
	// 主文件: .../{uuid}.jsonl
	name := filepath.Base(path)
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// belongsToSession 检查文件是否属于当前会话
func (m *JSONLMonitor) belongsToSession(path string) bool {
	if m.sessionUUID == "" {
		return true // 尚未确定会话，接受第一个文件
	}
	uuid := extractSessionUUID(path)
	return uuid == m.sessionUUID
}

func (m *JSONLMonitor) Start(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}

	if _, err := os.Stat(m.logDir); os.IsNotExist(err) {
		watcher.Close()
		return fmt.Errorf("log dir not found: %s", m.logDir)
	}

	if err := watcher.Add(m.logDir); err != nil {
		watcher.Close()
		return fmt.Errorf("watch dir: %w", err)
	}
	m.watchedPaths[m.logDir] = struct{}{}

	// Claude: 扫描已有子目录
	if m.backendType == backend.TypeClaude {
		m.scanAndWatchSubdirs(watcher, m.logDir)
	}

	// Codex: 添加前一天目录
	if m.backendType == backend.TypeCodex {
		yesterday := time.Now().AddDate(0, 0, -1)
		yesterdayDir := filepath.Join(
			filepath.Dir(filepath.Dir(filepath.Dir(m.logDir))),
			yesterday.Format("2006"),
			yesterday.Format("01"),
			yesterday.Format("02"),
		)
		if _, err := os.Stat(yesterdayDir); err == nil {
			if err := watcher.Add(yesterdayDir); err == nil {
				m.watchedPaths[yesterdayDir] = struct{}{}
			}
		}
	}

	// 始终记录已有文件作为基线，防止切换到其他 Claude 会话的文件
	m.baselineFiles = m.listExistingJSONLFiles()
	if m.mainFile == "" {
		// 新会话：等待新文件创建
		slog.Info("JSONL monitor waiting for new file", "key", m.topicKey, "baseline_count", len(m.baselineFiles))
	} else {
		// 恢复会话：验证保存的文件存在
		if _, err := os.Stat(m.mainFile); err != nil {
			slog.Warn("saved JSONL file not found, resetting", "key", m.topicKey, "file", m.mainFile)
			delete(m.trackedFiles, m.mainFile)
			m.mainFile = ""
		} else {
			// 保存的文件有效 → 从基线中移除它，允许 WRITE 事件触发读取
			delete(m.baselineFiles, m.mainFile)
			slog.Info("JSONL monitor resuming", "key", m.topicKey, "file", filepath.Base(m.mainFile), "offset", m.trackedFiles[m.mainFile].byteOffset)
		}
	}

	ctx, m.cancel = context.WithCancel(ctx)
	go m.loop(ctx, watcher)
	return nil
}

func (m *JSONLMonitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *JSONLMonitor) loop(ctx context.Context, watcher *fsnotify.Watcher) {
	defer watcher.Close()

	dayCheckTicker := time.NewTicker(1 * time.Hour)
	defer dayCheckTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			m.handleEvent(watcher, event)
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Error("watcher error", "key", m.topicKey, "error", err)
		case <-dayCheckTicker.C:
			if m.backendType == backend.TypeCodex {
				m.checkDateChange(watcher)
			}
		}
	}
}

func (m *JSONLMonitor) handleEvent(watcher *fsnotify.Watcher, event fsnotify.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if event.Has(fsnotify.Create) {
		info, err := os.Stat(event.Name)
		if err != nil {
			return
		}
		if info.IsDir() {
			if m.backendType == backend.TypeClaude {
				m.addDirWatch(watcher, event.Name)
				subagentsDir := filepath.Join(event.Name, "subagents")
				if _, err := os.Stat(subagentsDir); err == nil {
					m.addDirWatch(watcher, subagentsDir)
				}
			}
			return
		}
		if isJSONLFile(event.Name, m.backendType) {
			if m.baselineFiles != nil {
				if _, known := m.baselineFiles[event.Name]; known {
					return // 忽略基线内的已有文件
				}
			}
			m.trackFile(event.Name)
		}
	}

	if event.Has(fsnotify.Write) {
		if isJSONLFile(event.Name, m.backendType) {
			// 已跟踪的文件：直接增量读取
			if _, tracked := m.trackedFiles[event.Name]; tracked {
				m.readIncremental(event.Name)
				return
			}
			// 未跟踪且不在基线中：开始跟踪（新会话首次写入）
			if m.baselineFiles != nil {
				if _, known := m.baselineFiles[event.Name]; known {
					return
				}
			}
			m.trackFile(event.Name)
		}
	}
}

// trackFile 开始跟踪一个新文件，并读取初始内容
func (m *JSONLMonitor) trackFile(path string) {
	if _, exists := m.trackedFiles[path]; exists {
		return
	}

	// 检查是否属于当前会话
	if !m.belongsToSession(path) {
		slog.Debug("ignoring file from different session", "key", m.topicKey, "file", filepath.Base(path))
		return
	}

	slog.Info("tracking JSONL file", "key", m.topicKey, "file", filepath.Base(path))
	m.trackedFiles[path] = &fileTracker{byteOffset: 0}

	// 判断是否为主会话文件（非 subagent）
	if !strings.Contains(path, "/subagents/") {
		m.mainFile = path
		if m.sessionUUID == "" {
			m.sessionUUID = extractSessionUUID(path)
			slog.Info("session UUID locked", "key", m.topicKey, "uuid", m.sessionUUID)
		}
	}

	m.readIncremental(path)
}

func (m *JSONLMonitor) readIncremental(filePath string) {
	tracker, ok := m.trackedFiles[filePath]
	if !ok || filePath == "" {
		return
	}

	f, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer f.Close()

	if tracker.byteOffset > 0 {
		if _, err := f.Seek(tracker.byteOffset, io.SeekStart); err != nil {
			return
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var outputs []ParsedContent
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		contents := m.parseLine(line)
		outputs = append(outputs, contents...)
		if len(contents) > 0 {
			m.parseErrors = 0
		}
	}

	newOffset, _ := f.Seek(0, io.SeekCurrent)
	tracker.byteOffset = newOffset

	// 只持久化主文件的 offset（用于重启恢复）
	if filePath == m.mainFile {
		m.store.SetOffset(m.topicKey, state.Offset{
			File:       m.mainFile,
			ByteOffset: tracker.byteOffset,
		})
	}

	// 逐块发送，保持原始顺序
	for _, c := range outputs {
		switch c.Type {
		case ContentThinking:
			slog.Info("JSONL thinking output", "key", m.topicKey, "len", len(c.Text))
		case ContentText:
			slog.Info("JSONL answer output", "key", m.topicKey, "preview", truncate(c.Text, 80))
		case ContentToolUse:
			slog.Info("JSONL tool_use", "key", m.topicKey, "text", truncate(c.Text, 80))
		case ContentToolResult:
			slog.Info("JSONL tool_result", "key", m.topicKey, "text", truncate(c.Text, 80))
		}
		m.handler(m.topicKey, c)
	}
}

// ParsedContent 解析后的内容块
type ParsedContent struct {
	Type      ContentType
	Text      string
	ToolUseID string // tool_use ID，用于 tool_result 配对
	ToolName  string // 工具名称
}

func (m *JSONLMonitor) parseLine(line string) []ParsedContent {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		m.parseErrors++
		if m.parseErrors >= 3 {
			slog.Warn("too many parse errors", "key", m.topicKey, "errors", m.parseErrors)
		}
		return nil
	}

	switch m.backendType {
	case backend.TypeClaude:
		return m.parseClaudeLine(raw)
	case backend.TypeCodex:
		text := parseCodexLine(raw)
		if text != "" {
			return []ParsedContent{{Type: ContentText, Text: text}}
		}
		return nil
	default:
		return nil
	}
}

func (m *JSONLMonitor) parseClaudeLine(raw map[string]json.RawMessage) []ParsedContent {
	var msgType string
	if t, ok := raw["type"]; ok {
		json.Unmarshal(t, &msgType)
	}
	if msgType != "assistant" && msgType != "user" {
		return nil
	}

	msgData, ok := raw["message"]
	if !ok {
		return nil
	}

	var msg struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msgData, &msg); err != nil {
		return nil
	}

	var results []ParsedContent
	for _, blockRaw := range msg.Content {
		var block struct {
			Type      string                 `json:"type"`
			Text      string                 `json:"text"`
			Thinking  string                 `json:"thinking"`
			ID        string                 `json:"id"`
			Name      string                 `json:"name"`
			Input     map[string]interface{} `json:"input"`
			ToolUseID string                 `json:"tool_use_id"`
			Content   json.RawMessage        `json:"content"`
			IsError   bool                   `json:"is_error"`
		}
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}

		switch block.Type {
		case "thinking":
			if block.Thinking != "" {
				results = append(results, ParsedContent{Type: ContentThinking, Text: block.Thinking})
			}
		case "text":
			if block.Text != "" {
				results = append(results, ParsedContent{Type: ContentText, Text: block.Text})
			}
		case "tool_use":
			if block.Name != "" {
				summary := FormatToolUseSummary(block.Name, block.Input)
				results = append(results, ParsedContent{
					Type:      ContentToolUse,
					Text:      summary,
					ToolUseID: block.ID,
					ToolName:  block.Name,
				})
				m.pendingTools[block.ID] = block.Name
			}
		case "tool_result":
			resultText := extractToolResultText(block.Content)
			var statsText string
			if block.IsError {
				errLine := firstLine(resultText)
				if len(errLine) > 100 {
					errLine = errLine[:100] + "…"
				}
				statsText = "  ⎿  Error: " + errLine
			} else {
				toolName := m.pendingTools[block.ToolUseID]
				delete(m.pendingTools, block.ToolUseID)
				statsText = FormatToolResultStats(resultText, toolName)
			}
			results = append(results, ParsedContent{
				Type:      ContentToolResult,
				Text:      statsText,
				ToolUseID: block.ToolUseID,
			})
		}
	}
	return results
}

func parseCodexLine(raw map[string]json.RawMessage) string {
	var msgType string
	if t, ok := raw["type"]; ok {
		json.Unmarshal(t, &msgType)
	}
	var role string
	if r, ok := raw["role"]; ok {
		json.Unmarshal(r, &role)
	}

	if role != "assistant" && msgType != "assistant" && msgType != "response" {
		return ""
	}

	if content, ok := raw["content"]; ok {
		var text string
		if err := json.Unmarshal(content, &text); err == nil && text != "" {
			return text
		}
		var items []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(content, &items); err == nil {
			var texts []string
			for _, item := range items {
				if item.Text != "" {
					texts = append(texts, item.Text)
				}
			}
			if len(texts) > 0 {
				return strings.Join(texts, "\n")
			}
		}
	}

	if msg, ok := raw["message"]; ok {
		var text string
		if err := json.Unmarshal(msg, &text); err == nil && text != "" {
			return text
		}
	}

	return ""
}

func (m *JSONLMonitor) findLatestJSONL() string {
	return findLatestFile(m.logDir, m.backendType)
}

// listExistingJSONLFiles 列出日志目录中所有已存在的 JSONL 文件
func (m *JSONLMonitor) listExistingJSONLFiles() map[string]struct{} {
	files := make(map[string]struct{})
	filepath.Walk(m.logDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if isJSONLFile(path, m.backendType) {
			files[path] = struct{}{}
		}
		return nil
	})
	return files
}

func findLatestFile(dir string, bt backend.Type) string {
	var latest string
	var latestTime time.Time

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !isJSONLFile(path, bt) {
			return nil
		}
		if info.ModTime().After(latestTime) {
			latest = path
			latestTime = info.ModTime()
		}
		return nil
	})

	return latest
}

func isJSONLFile(path string, bt backend.Type) bool {
	name := filepath.Base(path)
	switch bt {
	case backend.TypeClaude:
		return strings.HasSuffix(name, ".jsonl")
	case backend.TypeCodex:
		return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl")
	default:
		return strings.HasSuffix(name, ".jsonl")
	}
}

func (m *JSONLMonitor) scanAndWatchSubdirs(watcher *fsnotify.Watcher, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			subDir := filepath.Join(dir, e.Name())
			m.addDirWatch(watcher, subDir)
			subagents := filepath.Join(subDir, "subagents")
			if _, err := os.Stat(subagents); err == nil {
				m.addDirWatch(watcher, subagents)
			}
		}
	}
}

func (m *JSONLMonitor) addDirWatch(watcher *fsnotify.Watcher, dir string) {
	if _, ok := m.watchedPaths[dir]; ok {
		return
	}
	if err := watcher.Add(dir); err != nil {
		slog.Warn("failed to watch dir", "dir", dir, "error", err)
		return
	}
	m.watchedPaths[dir] = struct{}{}
	slog.Debug("watching dir", "key", m.topicKey, "dir", dir)
}

func (m *JSONLMonitor) checkDateChange(watcher *fsnotify.Watcher) {
	today := time.Now()
	todayDir := filepath.Join(
		filepath.Dir(filepath.Dir(filepath.Dir(m.logDir))),
		today.Format("2006"),
		today.Format("01"),
		today.Format("02"),
	)
	if _, ok := m.watchedPaths[todayDir]; ok {
		return
	}
	if _, err := os.Stat(todayDir); err == nil {
		m.addDirWatch(watcher, todayDir)
	}
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// extractToolResultText extracts text from a tool_result content field.
// Content can be a string, or an array of {type:"text", text:"..."} objects.
func extractToolResultText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	// Try as plain string
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return text
	}
	// Try as array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

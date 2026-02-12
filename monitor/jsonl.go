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

// JSONLMonitor 通过 fsnotify 监听日志目录，增量读取 JSONL 文件
type JSONLMonitor struct {
	topicKey      string
	backendType   backend.Type
	logDir        string
	byteOffset    int64
	currentFile   string
	handler       OutputHandler
	store         *state.Store
	cancel        context.CancelFunc
	mu            sync.Mutex
	watchedPaths  map[string]struct{}
	parseErrors   int
	baselineFiles map[string]struct{} // 启动时已存在的文件（仅新会话使用）
}

func NewJSONLMonitor(topicKey string, bt backend.Type, logDir string, byteOffset int64, currentFile string, handler OutputHandler, store *state.Store) *JSONLMonitor {
	return &JSONLMonitor{
		topicKey:     topicKey,
		backendType:  bt,
		logDir:       logDir,
		byteOffset:   byteOffset,
		currentFile:  currentFile,
		handler:      handler,
		store:        store,
		watchedPaths: make(map[string]struct{}),
	}
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
	if m.currentFile == "" {
		// 新会话：等待新文件创建
		slog.Info("JSONL monitor waiting for new file", "key", m.topicKey, "baseline_count", len(m.baselineFiles))
	} else {
		// 恢复会话：验证保存的文件存在
		if _, err := os.Stat(m.currentFile); err != nil {
			slog.Warn("saved JSONL file not found, resetting", "key", m.topicKey, "file", m.currentFile)
			m.currentFile = ""
			m.byteOffset = 0
		} else {
			// 保存的文件有效 → 从基线中移除它，允许 WRITE 事件触发读取
			delete(m.baselineFiles, m.currentFile)
			slog.Info("JSONL monitor resuming", "key", m.topicKey, "file", filepath.Base(m.currentFile), "offset", m.byteOffset)
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
			m.switchFile(event.Name)
		}
	}

	if event.Has(fsnotify.Write) {
		if isJSONLFile(event.Name, m.backendType) {
			if m.currentFile == "" {
				// 新会话尚未锁定文件：只接受不在基线中的文件
				if m.baselineFiles != nil {
					if _, known := m.baselineFiles[event.Name]; known {
						return
					}
				}
				m.switchFile(event.Name)
			}
			// WRITE 事件只处理当前文件，不切换到其他文件
			// 文件切换只通过 CREATE 事件触发，避免误读其他 Claude 会话
			if event.Name == m.currentFile {
				m.readIncremental()
			}
		}
	}
}

func (m *JSONLMonitor) switchFile(path string) {
	if path == m.currentFile {
		return
	}
	slog.Info("switching JSONL file", "key", m.topicKey, "file", path)
	m.currentFile = path
	m.byteOffset = 0
	m.readIncremental()
}

func (m *JSONLMonitor) readIncremental() {
	if m.currentFile == "" {
		return
	}

	f, err := os.Open(m.currentFile)
	if err != nil {
		return
	}
	defer f.Close()

	if m.byteOffset > 0 {
		if _, err := f.Seek(m.byteOffset, io.SeekStart); err != nil {
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
	m.byteOffset = newOffset

	m.store.SetOffset(m.topicKey, state.Offset{
		File:       m.currentFile,
		ByteOffset: m.byteOffset,
	})

	// 逐块发送，保持原始顺序（thinking → text → thinking → text）
	for _, c := range outputs {
		switch c.Type {
		case ContentThinking:
			slog.Info("JSONL thinking output", "key", m.topicKey, "len", len(c.Text))
			m.handler(m.topicKey, c.Text, ContentThinking)
		case ContentText:
			slog.Info("JSONL answer output", "key", m.topicKey, "preview", truncate(c.Text, 80))
			m.handler(m.topicKey, c.Text, ContentText)
		}
	}
}

// ParsedContent 解析后的内容块
type ParsedContent struct {
	Type ContentType
	Text string
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
		return parseClaudeLine(raw)
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

func parseClaudeLine(raw map[string]json.RawMessage) []ParsedContent {
	var msgType string
	if t, ok := raw["type"]; ok {
		json.Unmarshal(t, &msgType)
	}
	if msgType != "assistant" {
		return nil
	}

	msgData, ok := raw["message"]
	if !ok {
		return nil
	}

	var msg struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msgData, &msg); err != nil {
		return nil
	}

	var results []ParsedContent
	for _, c := range msg.Content {
		switch c.Type {
		case "thinking":
			if c.Thinking != "" {
				results = append(results, ParsedContent{Type: ContentThinking, Text: c.Thinking})
			}
		case "text":
			if c.Text != "" {
				results = append(results, ParsedContent{Type: ContentText, Text: c.Text})
			}
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

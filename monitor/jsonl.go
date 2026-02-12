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
	topicKey     string
	backendType  backend.Type
	logDir       string
	byteOffset   int64
	currentFile  string
	handler      OutputHandler
	store        *state.Store
	cancel       context.CancelFunc
	mu           sync.Mutex
	watchedPaths map[string]struct{}
	parseErrors  int
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

	if m.currentFile == "" {
		m.currentFile = m.findLatestJSONL()
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
			m.switchFile(event.Name)
		}
	}

	if event.Has(fsnotify.Write) {
		if isJSONLFile(event.Name, m.backendType) {
			if event.Name != m.currentFile {
				latest := m.findLatestJSONL()
				if latest != "" && latest != m.currentFile {
					m.switchFile(latest)
				}
			}
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

	var texts []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		text := m.parseLine(line)
		if text != "" {
			texts = append(texts, text)
			m.parseErrors = 0
		}
	}

	newOffset, _ := f.Seek(0, io.SeekCurrent)
	m.byteOffset = newOffset

	m.store.SetOffset(m.topicKey, state.Offset{
		File:       m.currentFile,
		ByteOffset: m.byteOffset,
	})

	if len(texts) > 0 {
		combined := strings.Join(texts, "\n")
		m.handler(m.topicKey, combined)
	}
}

func (m *JSONLMonitor) parseLine(line string) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		m.parseErrors++
		if m.parseErrors >= 3 {
			slog.Warn("too many parse errors", "key", m.topicKey, "errors", m.parseErrors)
		}
		return ""
	}

	switch m.backendType {
	case backend.TypeClaude:
		return parseClaudeLine(raw)
	case backend.TypeCodex:
		return parseCodexLine(raw)
	default:
		return ""
	}
}

func parseClaudeLine(raw map[string]json.RawMessage) string {
	var msgType string
	if t, ok := raw["type"]; ok {
		json.Unmarshal(t, &msgType)
	}
	if msgType != "assistant" {
		return ""
	}

	msgData, ok := raw["message"]
	if !ok {
		return ""
	}

	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msgData, &msg); err != nil {
		return ""
	}

	var texts []string
	for _, c := range msg.Content {
		if c.Type == "text" && c.Text != "" {
			texts = append(texts, c.Text)
		}
	}
	return strings.Join(texts, "\n")
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

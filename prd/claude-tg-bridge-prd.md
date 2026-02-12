# tgmux - PRD

## 一句话描述

通过 Telegram Bot + Topic 远程控制本地 tmux 中的多种 AI 编程助手会话（Claude Code / Codex / Gemini / bash），Go 实现，可选 Web UI。

## 竞品参考

已有高度相似的开源项目 [six-ddc/ccbot](https://github.com/six-ddc/ccbot)，功能与 tgmux 重合度高（Telegram Topic 映射 tmux 窗口、实时推送、内联键盘权限确认等）。tgmux 的差异化定位：**多后端统一管理**（Claude Code / Codex / Gemini / bash），而非仅支持 Claude Code。

## 核心架构

```
Telegram Message ──┐                    ┌── tmux window @0 ── claude code (项目A)
Telegram Topic A ──┤                    ├── tmux window @1 ── codex (项目B)
Telegram Topic B ──┼──tgmux(Go,Polling) ┼── tmux window @2 ── gemini-cli (项目C)
Telegram Topic C ──┤        │           ├── tmux window @3 ── bash (项目A)
Telegram Topic D ──┘    Web UI (可选)   └── tmux window @4 ── claude code (项目D)
```

**映射关系**: 1 Topic（或 1 对 1 私聊）= 1 tmux window = 1 后端会话（backend + 项目目录）

> 注意：私聊没有 Topic 概念，仅能绑定一个会话。多会话需使用群组 Topic 模式。

## 交互流程

### 消息处理（私聊 / 群聊 / Topic 统一）

```
用户发送消息
  ↓
鉴权检查（allowed_users 白名单，不在白名单内静默丢弃）
  ↓
按 chat_id 或 topic_id 查绑定
  ↓
┌─ 已绑定 ─────────────────────────────────────┐
│  检查 tmux 窗口是否存活                        │
│  ├─ 存活 → 转发到绑定的 tmux 窗口，正常交互    │
│  └─ 已死 → 提示"会话已断开"，自动解绑，         │
│           进入未绑定流程                        │
├─ 未绑定 ─────────────────────────────────────┤
│  提示："该 Topic 尚未绑定会话"                  │
│  检查 tmux 中已有窗口                          │
│  ┌─ 有已有会话 ────────────────────────┐      │
│  │  [claude @ my-project     (@0)]    │      │
│  │  [codex @ other-project   (@1)]    │      │
│  │  ──────────────────────────        │      │
│  │  [➕ 新建会话]                      │      │
│  │                                     │      │
│  │  选已有 → 绑定（不自动转发原始消息）  │      │
│  │  选新建 → /new 流程                 │      │
│  ├─ 无已有会话 ────────────────────────┤      │
│  │  直接进入 /new 流程                 │      │
│  └─────────────────────────────────────┘      │
└───────────────────────────────────────────────┘
```

### `/new` 新建会话

采用两步交互，降低单次认知负担：

```
触发 /new
  ↓
状态标记为 awaiting_dir
  ↓
第一步：选择项目目录（一条消息）
┌─ 📂 选择项目目录 ─────────────────┐
│ [~/project-a]        (最近使用)    │
│ [~/project-b]        (收藏)        │
│ [📁 输入路径...]                   │
└───────────────────────────────────┘

  ↓ 用户选择目录（或输入路径，此时状态为 awaiting_path_input，
  ↓ 下一条文本消息作为路径，不触发绑定流程）
状态标记为 awaiting_backend
  ↓
第二步：选择后端（第二条消息）
┌─ 🚀 选择启动命令 ─────────────────┐
│ [claude] [codex] [gemini] [bash]  │
└───────────────────────────────────┘

  ↓ 用户选择后端
创建 tmux 窗口 → cd 目录 → 启动命令 → 绑定
  ↓
✅ 已创建 claude 会话 @ ~/project-a
```

**状态机定义**：`idle` → `awaiting_dir` → `awaiting_path_input`（可选）→ `awaiting_backend` → `bound`



## 启动命令

| 后端 | tmux 内启动命令 | 日志路径 | 输出监控方式 |
|---|---|---|---|
| `claude` | `claude` | `~/.claude/projects/<path-encoded>/` 目录下最新 `.jsonl` 文件 | JSONL 增量读取（fsnotify 监听目录） |
| `codex` | `codex` | `~/.codex/sessions/YYYY/MM/DD/` 目录下最新 `rollout-*.jsonl` | JSONL 增量读取（fsnotify 监听目录） |
| `gemini` | `gemini` | `~/.gemini/tmp/<hash>/chats/` 目录下最新 `.json` 文件 | JSON 全量解析 + diff（非 JSONL，每次全量重写） |
| `bash` | 直接进入 shell | 无 | tmux capture-pane 轮询 |

> **日志路径说明**：
> - Claude 的 `<path-encoded>` 是项目绝对路径的 `/` 替换为 `-` 并去掉开头 `-`，如 `/Users/foo/project` → `Users-foo-project`
> - 启动后端后无法预知具体 session 文件名（UUID），需监听整个目录，按 mtime 取最新文件
> - Gemini 的 chat 文件是完整 JSON 而非 JSONL，不能用 byte_offset 增量读取，需每次解析整个 JSON 后 diff messages 数组
> - 这些路径是各工具的内部实现细节，无稳定性保证，应作为可配置项，允许用户覆盖
> - 当日志文件监控失败时，自动降级到 capture-pane 轮询

不同后端统一通过 tmux send-keys 发送输入。AI 工具优先使用日志文件监控（更精准），bash 使用 capture-pane 轮询兜底。

## 技术选型

| 组件 | 选择 | 理由 |
|---|---|---|
| 语言 | Go | 单二进制部署，并发原生支持 |
| Telegram SDK | [go-telegram/bot](https://github.com/go-telegram/bot) | Telegram 官网推荐，零依赖，支持 Bot API 9.4，活跃维护 |
| tmux 交互 | exec.Command 调用 tmux CLI | 简单可靠，无需 cgo |
| 会话监控 | fsnotify + 日志文件 / capture-pane 轮询 | AI 工具用日志目录监控（注意：监听目录而非单文件，避免 macOS 原子保存导致监视丢失），bash 用 capture-pane |
| Web UI | 内嵌 embed.FS + WebSocket | 单二进制，可通过配置关闭 |
| 配置 | YAML 单文件 | ~/.tgmux/config.yaml |
| 状态持久化 | JSON 文件 | ~/.tgmux/state.json |

## 功能清单

### P0 - 核心（MVP）

**会话管理**
- `/new` - 在创建topic的时候，或者发送消息发现没有绑定的时候（不主动呈现在选项里头）
- `/sessionInfo` - 查看当前绑定的会话详情（窗口ID、项目目录、后端类型、启动命令、运行时长）
- `/sessionList` - 列出所有 tmux 会话窗口及绑定状态，未绑定的可直接点击绑定到当前 Topic
 

**`/sessionInfo` 输出示例**
```
📋 当前会话信息
├─ 窗口:    @0
├─ 后端:    claude
├─ 目录:    ~/project/my-app
├─ 命令:    claude --dangerously-skip-permissions
├─ 状态:    运行中
└─ 创建于:  2h30m ago
```

**`/sessionList` 输出示例**
```
🖥 所有 tmux 窗口

@0  claude @ my-app        ← 已绑定 Topic "后端开发"
@1  codex @ api-server     ← 已绑定 Topic "API重构"
@2  bash @ scripts         ← 未绑定 [点击绑定]
@3  gemini @ ml-project    ← 未绑定 [点击绑定]
```

**消息交互**
- Topic 内发文本 → `tmux send-keys` 到绑定窗口（所有后端通用）
- 输出监控 → 根据后端类型自动选择：AI 工具用日志文件监控，bash 用 capture-pane
- 支持流式推送（优先使用 Bot API 9.3 的 sendMessageDraft，降级为 editMessage 节流）

**控制命令**
- `/esc` - 发送 Escape 键（中断 Claude）
- `/enter` - 发送回车键
- `/y` `/n` - 快捷回复权限确认
- `/screenshot` - 捕获 tmux 窗口内容，渲染为图片发送
- `/claude` {arg用户输入} - 原生的claude指令透传，如/config、/model等, 在当前绑定是claude的才展示
- `/codex` {arg用户输入} - 原生的codex指令透传，如/config、/model等, 在当前绑定是codex的才展示
- `/gemini` {arg用户输入} - 原生的gemini指令透传，如/config、/model等, 在当前绑定是gemini的才展示


**状态持久化**
- Topic ↔ tmux window 绑定关系（含后端类型）
- 各 Topic 的已读偏移量
- 常用目录列表（收藏 + 最近使用）
- 重启后自动恢复绑定

**常用目录管理**
- 手动收藏：`/dir add <路径>` 添加收藏，`/dir rm <路径>` 移除
- `/dirs-fav` 列出所有收藏 + 最近使用
- `/dir` 列出～根目录下内容，后续逐层点击展示，呈现：【进入，添加收藏】

### P1 - 增强

**Web UI（可关闭）**
- 配置项 `web.enabled: false` 默认关闭
- 开启后监听本地端口，提供：
  - 会话列表和状态总览
  - 实时终端输出（WebSocket）
  - 基础操作（发消息、中断、切换）
- 前端用纯 HTML + vanilla JS，embed 进二进制
- 不做 Mini App，仅作为本地辅助面板

**消息增强**
- 内联键盘：权限确认按钮（Yes/No/Always）
- 长消息自动分割（Telegram 4096 字符限制）
- Markdown 渲染失败自动降级纯文本

**文件交互**
- 发送图片 → 保存到项目临时目录，路径传给 Claude
- 发送文件 → 同上

## 命令速查表

| 命令 | 说明 | 场景 |
|---|---|---|
| `/new` | 新建会话（选目录+选后端） | 未绑定时自动触发，不主动呈现在菜单 |
| `/sessionInfo` | 查看当前绑定会话详情（窗口、目录、后端、命令、时长），未绑定时目录为当前目录其他为空 | 任意时刻 |
| `/sessionList` | 列出所有 tmux 窗口，未绑定的可点击绑定(在当前未绑定前提)，也可点击关闭 | 任意时刻 |
| `/esc` | 发送 Escape 键，中断当前后端 | 已绑定时可用 |
| `/enter` | 发送回车键 | 已绑定时可用 |
| `/y` `/n` | 快捷回复权限确认 | 已绑定时可用 |
| `/screenshot` | 截图 tmux 窗口，渲染为图片发送 | 已绑定时可用 |
| `/claude` {arg} | Claude Code 原生指令透传（如 /config、/model） | 绑定 claude 时展示 |
| `/codex` {arg} | Codex 原生指令透传 | 绑定 codex 时展示 |
| `/gemini` {arg} | Gemini 原生指令透传 | 绑定 gemini 时展示 |
| `/dir` | 从当前工作目录逐层浏览，可【进入】或【添加收藏】 | 任意时刻 |
| `/dirs-fav` | 列出所有收藏目录 + 最近使用目录 | 任意时刻 |

## 项目结构

```
tgmux/
├── main.go                 # 入口，CLI 参数解析，启动 bot + web
├── config/
│   └── config.go           # YAML 配置加载 + CLI flag 合并
├── backend/
│   ├── backend.go          # Backend 接口定义
│   ├── claude.go           # Claude Code：启动命令 + JSONL 监控
│   ├── codex.go            # Codex：启动命令 + JSONL 监控
│   ├── gemini.go           # Gemini：启动命令 + JSON 监控
│   └── bash.go             # Bash：直接 shell + capture-pane 监控
├── bot/
│   ├── bot.go              # Telegram bot 初始化、polling
│   ├── handlers.go         # 命令处理器
│   └── keyboard.go         # 内联键盘构建（后端选择、目录选择）
├── tmux/
│   ├── manager.go          # tmux 窗口创建/销毁/send-keys
│   └── capture.go          # capture-pane + 截图渲染
├── monitor/
│   ├── jsonl.go            # fsnotify 监听 JSONL（Claude 专用）
│   ├── pane.go             # capture-pane 轮询（bash 专用）
│   └── dispatcher.go       # 根据后端类型分发到对应监控器
├── state/
│   └── store.go            # 绑定关系 + 偏移量 + 常用目录持久化
├── web/                    # 可选 Web UI
│   ├── server.go           # HTTP + WebSocket 服务
│   └── static/             # embed.FS 静态资源
│       ├── index.html
│       └── app.js
├── config.example.yaml
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

## 配置文件

```yaml
# ~/.tgmux/config.yaml
telegram:
  token: "your-bot-token"
  allowed_users:
    - 123456789
backends:
  claude:
    command: "claude"        # 启动命令，可自定义参数
    args: []                 # 如 ["--dangerously-skip-permissions"]
  codex:
    command: "codex"
    args: []
  gemini:
    command: "gemini"
    args: []
  bash:
    command: ""              # 空则直接使用默认 shell

dirs:
  favorites: []              # 手动收藏的目录，也可通过 /dir add 管理
  recent_max: 10             # 最近使用目录最大记录数

web:
  enabled: false
  port: 3030

monitor:
  poll_interval: 500ms       # capture-pane 轮询间隔
  message_throttle: 500ms    # Telegram 消息编辑节流
```

## 状态文件

```json
{
  "bindings": {
    "topic_123": {
      "window_id": "@0",
      "backend": "claude",
      "project_path": "/home/user/my-project",
      "display_name": "claude @ my-project",
      "created_at": "2026-02-12T10:00:00Z"
    },
    "topic_456": {
      "window_id": "@1",
      "backend": "codex",
      "project_path": "/home/user/other-project",
      "display_name": "codex @ other-project",
      "created_at": "2026-02-12T11:00:00Z"
    }
  },
  "offsets": {
    "topic_123": {
      "file": "/path/to/session.jsonl",
      "byte_offset": 4096
    }
  },
  "dirs": {
    "favorites": ["/home/user/my-project", "/home/user/work"],
    "recent": ["/home/user/other-project", "/home/user/my-project"]
  }
}
```

## 关键实现要点

**tmux 交互**
```
创建窗口:  tmux new-window -t tgmux -n "claude-my-project"
进入目录:  tmux send-keys -t tgmux:@0 "cd /path/to/project" Enter
启动后端:  tmux send-keys -t tgmux:@0 "claude" Enter
发送文本:  tmux send-keys -t tgmux:@0 -l "user message"
发送回车:  tmux send-keys -t tgmux:@0 Enter
发送ESC:   tmux send-keys -t tgmux:@0 Escape
捕获内容:  tmux capture-pane -t tgmux:@0 -p -e
```

**输出监控（双策略）**

策略一：日志文件监控（Claude / Codex / Gemini）
1. fsnotify 监听对应日志目录
   - Claude: `~/.claude/projects/<hash>/`
   - Codex: `~/.codex/sessions/YYYY/MM/DD/`
   - Gemini: `~/.gemini/tmp/<hash>/chats/`
2. 文件变化时，从上次 byte_offset 开始增量读取
3. 逐行解析 JSON，提取 assistant 消息、tool_use 等
4. 格式化后推送到对应 Topic
5. 更新 byte_offset

策略二：capture-pane 轮询（bash）
1. 定时执行 `tmux capture-pane -p -e` 获取窗口内容
2. 与上次快照 diff，提取新增行
3. 过滤 ANSI 转义序列，格式化后推送到对应 Topic
4. 更新快照

**流式推送策略（双模式）**

方案一（优先）：Bot API 9.3+ `sendMessageDraft`
- 新内容到达 → 调用 sendMessageDraft 追加部分文本
- Telegram 客户端原生渲染流式效果，无需反复编辑消息
- 输出完成 → 发送最终完整消息

方案二（降级）：editMessage 节流
- sendMessageDraft 不可用时自动降级
- 新内容到达 → 追加到 buffer
- 500ms 节流 → editMessageText 更新消息
- 超过 3800 字符 → 定稿当前消息，发新消息继续
- Markdown 解析失败 → 降级纯文本重试

## 构建与运行

```bash
# 构建
make build    # → ./bin/tgmux

# 运行
tgmux                                # 使用默认配置 ~/.tgmux/config.yaml
tgmux -c /path/to/config.yaml        # 指定配置文件
tgmux --web                          # 启用 Web UI
tgmux --web --web-port 9090          # 启用 Web UI 并指定端口

# 开发
make dev      # go run with -race
make lint     # golangci-lint
make test     # go test ./...
```

**CLI 参数优先级**: 命令行参数 > 环境变量 > 配置文件 > 默认值

## 非目标

- 不做 Telegram Mini App（复杂度高，需要 HTTPS）
- 不做 Agent SDK 集成（tmux 桥接已足够，保持简单）
- 不做数据库（JSON 文件足矣）
- 不做多机部署（单机工具）
- 不做自动更新
- 不做后端能力抽象（各后端差异大，统一接口仅限 tmux 层面的输入/输出）

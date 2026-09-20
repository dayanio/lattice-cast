// Package brain 是 cast-agent 的内置大脑：跑在 agent 进程内的 LLM 工具循环，
// 把网页聊天（internal/cast/webchat）的中文自然语言请求翻译成与 MCP 层完全
// 相同的工具调用——经 ToolExecutor 内部直调 mcpserver 的执行核心，不走
// HTTP 自环，审计同路径落行。
//
// 线路协议：三家 provider（glm|claude|ollama）统一走 OpenAI 兼容的 chat
// completions；stream=false（v1 简化：整段补全一次性返回，SSE 帧由 webchat
// 层包装）。api_key 只存在于服务端（以 Bearer 头下发），一切错误文本经
// 脱敏（对齐 resolve.Reflux 源的 redactTransportErr 纪律）。
//
// 会话：sessions 按 sessionID 保留多轮消息历史（含工具结果），互斥保护；
// 历史上限 historyCap 条（不含首条 system），超出从最旧裁起（TTL 清理 v1
// 不做：家庭单用户场景，进程生命周期内会话数有限）。并发约束：同一会话的
// 并发 Chat 语义未定义（v1 单用户，webchat 前端串行发送）。
package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dayanio/lattice-cast/internal/cast/config"
)

// ToolExecutor 是工具执行核心的抽象：由 mcpserver 提供适配器（Go 结构化
// 类型隐式实现，mcpserver 不反向依赖本包），brain 据此直调与 MCP handler
// 完全相同的实现。result 是工具出参的 JSON 字符串；err 非 nil 时其文本即
// 契约错误（与 MCP IsError 的 Content[0].Text 同源）。
type ToolExecutor interface {
	Execute(ctx context.Context, tool string, argsJSON json.RawMessage) (result string, err error)
}

// ChatEvent 是一次聊天回复的事件流。Type 取值：
//   - "delta"：助手伴随工具调用的过程文本（v1 整段一条）；
//   - "tool"：一个工具开始执行（Text 为工具名）；
//   - "final"：最终答复（Text 为完整答复文本，本轮结束）；
//   - "error"：中途失败（Text 为脱敏后的结构化错误文本）。
//
// error 是 delta|tool|final 之外的第四种类型：工具循环跑在后台 goroutine，
// Chat 同步返回之后发生的失败（LLM 不可达/鉴权失败/循环超限）只能经通道
// 送达，SSE 层原样转发。
type ChatEvent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

const (
	// maxToolIterations 单次回复允许的工具轮数上限：防失控循环。
	maxToolIterations = 5

	// historyCap 会话历史上限（条，不含首条 system 消息）。
	historyCap = 20
)

// message 是 OpenAI 兼容 chat completions 的一条消息（请求/响应共用）。
// Content 带 omitempty：assistant 携带 tool_calls 时按惯例省略 content
// （各兼容端点把缺省按 null 处理）。
type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"` // 固定 "function"
	Function funcCall `json:"function"`
}

type funcCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串形态的入参
}

// chatRequest / chatResponse 是 chat completions 的请求/响应外壳
// （只取需要的字段；tools 的入参 schema 与 MCP 工具一一对应，见 toolDefs）。
type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []toolDef `json:"tools"`
	Stream   bool      `json:"stream"` // v1 固定 false：整段返回，SSE 由 webchat 层包装
}

type chatResponse struct {
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
}

// session 是一个聊天会话：msgs[0] 恒为 system 消息（会话创建时注入设备清单）。
type session struct {
	msgs []message
}

// Brain 是内置大脑：LLM 工具循环 + 多轮会话。
type Brain struct {
	cfg      config.Brain
	exec     ToolExecutor
	hc       *http.Client
	endpoint string // chat completions 完整地址（base_url 归一化后拼接）

	mu       sync.Mutex
	sessions map[string]*session
}

// New 构造 Brain。hc 为 nil 时使用 60 秒超时的默认客户端（整段补全的合理
// 上限）；cfg.BaseURL 由 config.Load 按 provider 填好预设端点。
func New(cfg config.Brain, exec ToolExecutor, hc *http.Client) *Brain {
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Brain{
		cfg:      cfg,
		exec:     exec,
		hc:       hc,
		endpoint: strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions",
		sessions: make(map[string]*session),
	}
}

// Chat 追加一轮用户输入并异步驱动工具循环；返回的事件通道在循环结束时
// （final 或 error）关闭。sessionID 首次出现时创建会话并注入系统提示
// （含当前设备清单），多轮历史随后保留。
func (b *Brain) Chat(ctx context.Context, sessionID, userText string) (<-chan ChatEvent, error) {
	if userText == "" {
		return nil, errors.New("brain: empty_message")
	}
	if b.exec == nil {
		return nil, errors.New("brain: tool executor is nil")
	}

	b.mu.Lock()
	sess, ok := b.sessions[sessionID]
	if !ok {
		sess = &session{msgs: []message{{Role: "system", Content: b.systemPrompt(ctx)}}}
		b.sessions[sessionID] = sess
	}
	sess.msgs = append(sess.msgs, message{Role: "user", Content: userText})
	b.mu.Unlock()
	// 快照在 LLM 轮次前完成（含刚追加的 user 消息）；snapshot 自取锁。
	msgs := b.snapshot(sess)

	events := make(chan ChatEvent, 16)
	go b.run(ctx, sess, events, msgs)
	return events, nil
}

// run 驱动工具循环：整段请求 LLM → 有 tool_calls 则逐个经执行核心执行、
// 结果回灌历史并继续 → 直到出现纯文本答复（final）或达到轮数上限（error）。
func (b *Brain) run(ctx context.Context, sess *session, events chan<- ChatEvent, msgs []message) {
	defer close(events)
	for range maxToolIterations {
		msg, err := b.complete(ctx, msgs)
		if err != nil {
			events <- ChatEvent{Type: "error", Text: err.Error()}
			return
		}
		msg.Role = "assistant"
		b.append(sess, msg)
		if len(msg.ToolCalls) == 0 {
			events <- ChatEvent{Type: "final", Text: msg.Content}
			return
		}
		if msg.Content != "" {
			events <- ChatEvent{Type: "delta", Text: msg.Content}
		}
		for _, tc := range msg.ToolCalls {
			events <- ChatEvent{Type: "tool", Text: tc.Function.Name}
			result, execErr := b.exec.Execute(ctx, tc.Function.Name, normalizeArgs(tc.Function.Arguments))
			content := result
			if execErr != nil {
				// 错误文本即契约（与 MCP 的 IsError Content 同源），原样回灌，
				// 让 LLM 向用户转述。resolve/manager 的错误已自带脱敏。
				content = execErr.Error()
			}
			if content == "" {
				content = "{}"
			}
			b.append(sess, message{Role: "tool", Content: content, ToolCallID: tc.ID})
		}
		msgs = b.snapshot(sess) // 下一轮请求携带含工具结果的最新历史（必须持锁：并发轮次同时在 append）
	}
	events <- ChatEvent{Type: "error", Text: "brain: tool_loop_limit_reached"}
}

// snapshot 复制会话历史。自取锁：run 循环内的快照点位于两次 LLM 往返之间，
// 不能跨网络往返持锁，故由本方法自行负责互斥（否则与并发轮次的加锁
// append 构成数据竞争）。
func (b *Brain) snapshot(sess *session) []message {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]message, len(sess.msgs))
	copy(out, sess.msgs)
	return out
}

// append 把一条消息并入会话历史（互斥；超出上限时从最旧裁起）。
func (b *Brain) append(sess *session, m message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess.msgs = append(sess.msgs, m)
	sess.msgs = trimHistory(sess.msgs)
}

// trimHistory 把历史裁到 historyCap 条（不含 system 另计）：从最旧的非
// system 消息裁起，且裁剪后不得以孤儿 tool 消息开头（其配对的 assistant
// 已被裁掉时一并丢弃）。
func trimHistory(msgs []message) []message {
	if len(msgs) <= historyCap+1 {
		return msgs
	}
	tail := msgs[len(msgs)-historyCap:]
	for len(tail) > 0 && tail[0].Role == "tool" {
		tail = tail[1:]
	}
	return append([]message{msgs[0]}, tail...)
}

// normalizeArgs 把 LLM 给出的工具入参字符串规整成 json.RawMessage：空串按
// 空对象处理（无参工具）。
func normalizeArgs(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
}

// complete 发起一次 chat completions（stream=false）并解析出 assistant 消息。
// 非 2xx 归一为结构化错误（401/403 → auth_failed）；传输错误经脱敏改写。
func (b *Brain) complete(ctx context.Context, msgs []message) (message, error) {
	body, err := json.Marshal(chatRequest{Model: b.cfg.Model, Messages: msgs, Tools: toolDefs})
	if err != nil {
		return message{}, fmt.Errorf("brain: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
	if err != nil {
		return message{}, fmt.Errorf("brain: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.cfg.APIKey)
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		// *url.Error 的文本内嵌完整请求 URL：按 reflux 同一纪律去查询串后包裹。
		return message{}, fmt.Errorf("brain: llm: %w", redactTransportErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return message{}, fmt.Errorf("brain: llm status %d: auth_failed", resp.StatusCode)
		}
		return message{}, fmt.Errorf("brain: llm status %d", resp.StatusCode)
	}
	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return message{}, fmt.Errorf("brain: decode llm response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return message{}, errors.New("brain: llm returned no choices")
	}
	return parsed.Choices[0].Message, nil
}

// systemPrompt 构造系统提示：角色设定 + 使用规则 + 当前设备清单（会话创建
// 时经执行核心调 list_cast_devices；失败不致命，占位说明交由 LLM 现场查询）。
func (b *Brain) systemPrompt(ctx context.Context) string {
	devices := "（设备清单暂时不可用，请先调用 list_cast_devices 查询）"
	if res, err := b.exec.Execute(ctx, "list_cast_devices", json.RawMessage("{}")); err == nil && res != "" {
		devices = res
	}
	var sb strings.Builder
	sb.WriteString("你是 LatticeCast 投屏助手：帮用户把 NAS / reflux / 网络媒体投放到家中的投屏设备上播放。\n\n")
	sb.WriteString("当前设备清单（JSON；状态可能变化，拿不准时先调 list_cast_devices 刷新）：\n")
	sb.WriteString(devices)
	sb.WriteString("\n\n使用规则：\n")
	sb.WriteString("1. 始终用中文回复。\n")
	sb.WriteString("2. 用户请求有歧义时必须先反问确认（如命中多个设备或多个媒体），不要替用户擅自选择。\n")
	sb.WriteString("3. 按 media_id 播放前先用 search_media 检索，把结果里的 media_id 原样传给 cast_play，不要杜撰。\n")
	sb.WriteString("4. 工具失败时用中文向用户解释原因（错误文本是英文契约词，转述即可，不要原样粘贴）。\n")
	sb.WriteString("5. search_media 没有命中时，必须如实告诉用户\"没有找到\"并可建议换个关键词；严禁编造文件路径、文件名或 URL 去 cast_play。\n")
	sb.WriteString("6. cast_play 的 url 参数只接受 http(s):// 开头的真实网络直链；本地路径（如 /Volumes/…、/Users/…）一律禁止——本地文件只通过 search_media 返回的 media_id 播放。")
	return sb.String()
}

// redactTransportErr 把传输层错误改写成不含凭证的等价错误（与 resolve 的
// reflux 脱敏同一纪律）：*url.Error 的文本内嵌完整请求 URL，去查询串/
// 用户信息/片段后包裹底层原因（errors.Is/As 判定不受影响）。
func redactTransportErr(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	safeURL := ue.URL
	if parsed, perr := url.Parse(ue.URL); perr == nil {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		parsed.User = nil
		safeURL = parsed.String()
	}
	return fmt.Errorf("%s %s: %w", ue.Op, safeURL, ue.Err)
}

// toolDef / toolFunc 是 OpenAI function calling 的工具声明形态。
type toolDef struct {
	Type     string   `json:"type"` // 固定 "function"
	Function toolFunc `json:"function"`
}

type toolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// toolDefs 是 OpenAI function calling 形态的工具清单：八个工具与 MCP 层
// 一一同形，描述文案与 MCP 完全一致（产品契约），入参 schema 镜像 MCP 的
// 输入结构体（cast_play 仅 device 必填，media_id/url/title/position_ms
// 可选；cast_seek/cast_volume 的数值参数必填）。
var toolDefs = []toolDef{
	{Type: "function", Function: toolFunc{
		Name:        "list_cast_devices",
		Description: "List cast devices with online state and what is playing.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}},
	{Type: "function", Function: toolFunc{
		Name:        "search_media",
		Description: "Search media by title substring: NAS library plus configured reflux server; returns media_id for cast_play (reflux ids carry a reflux: prefix).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"标题子串，大小写不敏感"}},"required":["query"]}`),
	}},
	{Type: "function", Function: toolFunc{
		Name:        "cast_play",
		Description: "Play media on a device by media_id (library) or url (direct/YouTube page); pass exactly one.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"device":{"type":"string","description":"设备名（list_cast_devices 返回的 name）"},"media_id":{"type":"string","description":"search_media 返回的媒体 id"},"url":{"type":"string","description":"直链或页面地址"},"title":{"type":"string","description":"可选展示标题"},"position_ms":{"type":"integer","description":"可选起播位置（毫秒）"}},"required":["device"]}`),
	}},
	{Type: "function", Function: toolFunc{
		Name:        "cast_stop",
		Description: "Stop playback on a device.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"device":{"type":"string"}},"required":["device"]}`),
	}},
	{Type: "function", Function: toolFunc{
		Name:        "cast_pause",
		Description: "Pause playback on a device.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"device":{"type":"string"}},"required":["device"]}`),
	}},
	{Type: "function", Function: toolFunc{
		Name:        "cast_seek",
		Description: "Seek on a device to an absolute position in milliseconds.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"device":{"type":"string"},"position_ms":{"type":"integer"}},"required":["device","position_ms"]}`),
	}},
	{Type: "function", Function: toolFunc{
		Name:        "cast_volume",
		Description: "Set device volume 0-100.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"device":{"type":"string"},"level":{"type":"integer","description":"0-100"}},"required":["device","level"]}`),
	}},
	{Type: "function", Function: toolFunc{
		Name:        "cast_status",
		Description: "Get a device's current playback state.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"device":{"type":"string"}},"required":["device"]}`),
	}},
}

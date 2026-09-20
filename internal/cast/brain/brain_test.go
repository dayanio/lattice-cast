// Package brain 的验收测试：以脚本化 Fake LLM（OpenAI 兼容 chat completions）
// 驱动工具循环——assistant tool_calls → cast_play 经执行核心在 Fake 渲染端
// 真实播放 → final 答复；多轮会话历史逐轮进入上下文；LLM 失败时错误脱敏
// （api_key 不出现在任何事件文本中）；工具失败文本原样回灌下一轮。
package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dayanio/lattice-cast/internal/cast/config"
	"github.com/dayanio/lattice-cast/internal/cast/manager"
	"github.com/dayanio/lattice-cast/internal/cast/mcpserver"
	"github.com/dayanio/lattice-cast/internal/cast/resolve"
	"github.com/dayanio/lattice-cast/internal/cast/testsupport/fakellm"
	"github.com/dayanio/lattice-cast/internal/cast/testsupport/fakerenderer"
)

const (
	testToken = "tok-mcp"
	renderTok = "tok-renderer"
	devName   = "living-room-tv"
	devRoom   = "客厅"
	brainKey  = "sk-secret-brain-key"
	mediaBase = "http://media.example:7810"
)

// stack 是一套完整被测栈：Fake 渲染端 + 静态配置 Manager + 临时媒体库 +
// Resolver + 审计 + mcpserver 执行核心（内部直调，非 HTTP 自环）+ 脚本化
// Fake LLM。
type stack struct {
	brain     *Brain
	llm       *fakellm.Fake
	fake      *fakerenderer.Fake
	lib       *resolve.Library
	auditPath string
}

// newStack 构造被测栈；wrap 可选地装饰执行核心（如并发测试的门控执行器）。
func newStack(t *testing.T, wrap ...func(ToolExecutor) ToolExecutor) *stack {
	t.Helper()

	fake := fakerenderer.New(renderTok)
	t.Cleanup(fake.Close)
	tgt := fake.Target()

	cfg := config.Config{
		AuthToken:    testToken,
		MediaBaseURL: mediaBase,
		Renderers: map[string]config.Renderer{
			devName: {Room: devRoom, Host: tgt.Host, Port: tgt.Port, Token: renderTok},
		},
	}

	libDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "sunset-clip.mp4"), []byte("x"), 0o600))
	lib := resolve.NewLibrary([]string{libDir})
	require.NoError(t, lib.Rescan())
	res := &resolve.Resolver{Lib: lib, Base: cfg.MediaBaseURL}

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := manager.OpenAudit(auditPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = audit.Close() })

	srv := mcpserver.New(manager.New(cfg, lib, res), lib, res, audit, mcpserver.StaticToken(testToken))
	// 编译期钉住：mcpserver 的执行核心适配器满足 brain.ToolExecutor。
	var exec ToolExecutor = srv.Executor()
	for _, w := range wrap {
		exec = w(exec)
	}

	llm := fakellm.New()
	br := New(config.Brain{
		Provider: "glm",
		APIKey:   brainKey,
		Model:    "glm-4.7",
		BaseURL:  llm.URL,
	}, exec, nil)

	return &stack{brain: br, llm: llm, fake: fake, lib: lib, auditPath: auditPath}
}

// ---- OpenAI 兼容响应体模板与 wire 解析 ----

// toolCallBody 构造一条带工具调用的 assistant 响应体（%q 负责全部转义）。
func toolCallBody(content, id, tool, argsJSON string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q,"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}]}`,
		content, id, tool, argsJSON)
}

// textBody 构造一条纯文本 assistant 响应体。
func textBody(content string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q}}]}`, content)
}

type wireMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
}

func parseRequest(t *testing.T, raw []byte) wireRequest {
	t.Helper()
	var req wireRequest
	require.NoError(t, json.Unmarshal(raw, &req), "请求体应为合法 JSON：%s", raw)
	return req
}

// drain 收齐事件直到通道关闭（工具循环结束）。
func drain(t *testing.T, events <-chan ChatEvent) []ChatEvent {
	t.Helper()
	var out []ChatEvent
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(15 * time.Second):
			t.Fatal("等待 ChatEvent 超时（15s）")
		}
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// ---- 工具循环主链路 ----

// TestChat_ToolLoopPlaysMedia 主链路：脚本第一响 = tool_calls（cast_play 按
// media_id），第二响 = 最终答复。断言事件序列 delta → tool → final、工具经
// 执行核心真打到了 Fake 渲染端（内部直调，非 HTTP 自环）、审计与 MCP 同一
// 路径落行（agent=brain）。
func TestChat_ToolLoopPlaysMedia(t *testing.T) {
	s := newStack(t)
	items := s.lib.Search("sunset")
	require.Len(t, items, 1)
	argsJSON := fmt.Sprintf(`{"device":%q,"media_id":%q,"title":"夕阳短片"}`, devName, items[0].ID)
	s.llm.Script(
		fakellm.Resp{Body: toolCallBody("好的，马上为您播放。", "call-1", "cast_play", argsJSON)},
		fakellm.Resp{Body: textBody("已经在客厅播放")},
	)

	events, err := s.brain.Chat(context.Background(), "s1", "把夕阳短片投到客厅")
	require.NoError(t, err)
	got := drain(t, events)

	require.Len(t, got, 3, "事件序列应为 delta → tool → final：%v", got)
	assert.Equal(t, "delta", got[0].Type)
	assert.Equal(t, "好的，马上为您播放。", got[0].Text)
	assert.Equal(t, "tool", got[1].Type)
	assert.Equal(t, "cast_play", got[1].Text)
	assert.Equal(t, "final", got[2].Type)
	assert.Equal(t, "已经在客厅播放", got[2].Text)

	wantURL := s.lib.MediaURL(mediaBase, items[0].ID)
	assert.Equal(t, wantURL, s.fake.PlayedURL(), "cast_play 应经执行核心真打到渲染端")

	// 审计同一路径：system 注入的 list_cast_devices 与工具轮的 cast_play 各一行，
	// agent 署名 brain（区别于 MCP 的 static-token）。
	b, err := os.ReadFile(s.auditPath)
	require.NoError(t, err)
	lines := nonEmptyLines(string(b))
	require.Len(t, lines, 2, "list_cast_devices + cast_play 各一行审计：%q", lines)
	var last manager.AuditEntry
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &last))
	assert.Equal(t, "cast_play", last.Tool)
	assert.Equal(t, "brain", last.Agent, "大脑直调的工具审计应署名 brain")
	assert.True(t, last.OK)
}

// TestChat_HistoryRetainedAcrossTurns 多轮会话：第二轮请求必须携带第一轮的
// 完整往返（user / assistant tool_calls / tool 结果 / assistant 答复）；
// system 设备清单只在会话创建时注入一次；api_key 以 Bearer 头下发。
func TestChat_HistoryRetainedAcrossTurns(t *testing.T) {
	s := newStack(t)
	items := s.lib.Search("sunset")
	require.Len(t, items, 1)
	argsJSON := fmt.Sprintf(`{"device":%q,"media_id":%q}`, devName, items[0].ID)
	s.llm.Script(
		fakellm.Resp{Body: toolCallBody("好的，马上为您播放。", "call-1", "cast_play", argsJSON)},
		fakellm.Resp{Body: textBody("已经在客厅播放")},
		fakellm.Resp{Body: textBody("好的，已调大音量")},
	)

	ctx := context.Background()
	ev1, err := s.brain.Chat(ctx, "s1", "把夕阳短片投到客厅")
	require.NoError(t, err)
	drain(t, ev1)
	ev2, err := s.brain.Chat(ctx, "s1", "声音大一点")
	require.NoError(t, err)
	drain(t, ev2)

	reqs := s.llm.Requests()
	require.Len(t, reqs, 3, "两轮共三次 LLM 调用")

	r1 := parseRequest(t, reqs[0])
	assert.Equal(t, "glm-4.7", r1.Model)
	require.Len(t, r1.Messages, 2, "首轮请求 = system + user")
	assert.Equal(t, "system", r1.Messages[0].Role)
	assert.Contains(t, r1.Messages[0].Content, devName, "system 提示应注入设备清单")
	assert.Equal(t, "user", r1.Messages[1].Role)
	assert.Equal(t, "把夕阳短片投到客厅", r1.Messages[1].Content)

	r2 := parseRequest(t, reqs[1])
	require.Len(t, r2.Messages, 4, "第二轮请求 = system + user + assistant(tool_calls) + tool")
	assert.Equal(t, "assistant", r2.Messages[2].Role)
	require.Len(t, r2.Messages[2].ToolCalls, 1)
	assert.Equal(t, "cast_play", r2.Messages[2].ToolCalls[0].Function.Name)
	assert.Equal(t, "tool", r2.Messages[3].Role)
	assert.Equal(t, "call-1", r2.Messages[3].ToolCallID)
	assert.Contains(t, r2.Messages[3].Content, "playing", "工具结果应回灌上下文")

	r3 := parseRequest(t, reqs[2])
	require.Len(t, r3.Messages, 6, "第三轮请求 = system + 第一轮完整往返 + 第二轮 user")
	assert.Equal(t, "把夕阳短片投到客厅", r3.Messages[1].Content)
	require.Len(t, r3.Messages[2].ToolCalls, 1, "assistant 工具调用应整体保留在历史里")
	assert.Equal(t, "cast_play", r3.Messages[2].ToolCalls[0].Function.Name)
	assert.Equal(t, "已经在客厅播放", r3.Messages[4].Content, "第一轮最终答复应保留")
	assert.Equal(t, "user", r3.Messages[5].Role)
	assert.Equal(t, "声音大一点", r3.Messages[5].Content)

	// 鉴权头：api_key 以 Bearer 头下发（不出现在 URL 与错误文本里）。
	auths := s.llm.AuthHeaders()
	require.NotEmpty(t, auths)
	assert.Equal(t, "Bearer "+brainKey, auths[0])
}

// ---- 错误路径 ----

// TestChat_LLMErrorRedactsAPIKey LLM 鉴权失败（401）与不可达（连接拒绝）：
// 均以 error 事件结构化收尾，文本绝不出现 api_key（与 reflux 同一脱敏纪律）。
func TestChat_LLMErrorRedactsAPIKey(t *testing.T) {
	s := newStack(t)
	s.llm.Script(fakellm.Resp{Status: 401, Body: `{"error":{"message":"Invalid API key"}}`})

	events, err := s.brain.Chat(context.Background(), "s1", "你好")
	require.NoError(t, err)
	got := drain(t, events)
	require.NotEmpty(t, got)

	last := got[len(got)-1]
	assert.Equal(t, "error", last.Type, "LLM 失败应以 error 事件收尾：%v", got)
	assert.Contains(t, last.Text, "401")
	assert.Contains(t, last.Text, "auth_failed")
	assert.NotContains(t, last.Text, brainKey, "错误文本不得泄露 api_key")

	// 连接失败（服务器已关）同样结构化且脱敏。
	s2 := newStack(t)
	s2.llm.Close()
	events2, err := s2.brain.Chat(context.Background(), "s1", "你好")
	require.NoError(t, err)
	got2 := drain(t, events2)
	require.NotEmpty(t, got2)
	assert.Equal(t, "error", got2[len(got2)-1].Type)
	assert.NotContains(t, got2[len(got2)-1].Text, brainKey)
	assert.NotContains(t, got2[len(got2)-1].Text, "?", "传输错误文本不得携带查询串")
}

// TestChat_ToolErrorFedBack 工具失败（未知设备）与未知工具：错误文本作为
// tool 消息回灌下一轮上下文，LLM 据此给出最终答复——与 MCP 的 IsError
// 文本契约同源。
func TestChat_ToolErrorFedBack(t *testing.T) {
	s := newStack(t)
	s.llm.Script(
		fakellm.Resp{Body: toolCallBody("", "call-1", "cast_play", `{"device":"no-such","url":"http://example.com/x.mp3"}`)},
		fakellm.Resp{Body: textBody("没找到这个设备，请确认设备名称。")},
	)

	events, err := s.brain.Chat(context.Background(), "s1", "投到 no-such")
	require.NoError(t, err)
	got := drain(t, events)

	require.Len(t, got, 2, "第一响 content 为空不应有 delta：%v", got)
	assert.Equal(t, "tool", got[0].Type)
	assert.Equal(t, "cast_play", got[0].Text)
	assert.Equal(t, "final", got[1].Type)
	assert.Equal(t, "没找到这个设备，请确认设备名称。", got[1].Text)

	reqs := s.llm.Requests()
	require.Len(t, reqs, 2)
	r2 := parseRequest(t, reqs[1])
	var toolMsg string
	for _, m := range r2.Messages {
		if m.Role == "tool" {
			toolMsg = m.Content
		}
	}
	assert.Contains(t, toolMsg, "unknown_device: no-such", "工具错误文本应原样回灌")

	// 未知工具名同样以错误回灌（不 panic、不中断循环）。
	s2 := newStack(t)
	s2.llm.Script(
		fakellm.Resp{Body: toolCallBody("", "call-1", "nonexistent_tool", `{}`)},
		fakellm.Resp{Body: textBody("抱歉，没有这个能力。")},
	)
	ev, err := s2.brain.Chat(context.Background(), "s1", "随便放点东西")
	require.NoError(t, err)
	got2 := drain(t, ev)
	require.Len(t, got2, 2)
	assert.Equal(t, "tool", got2[0].Type)
	assert.Equal(t, "final", got2[1].Type)

	reqs2 := s2.llm.Requests()
	require.Len(t, reqs2, 2)
	r := parseRequest(t, reqs2[1])
	for _, m := range r.Messages {
		if m.Role == "tool" {
			assert.Contains(t, m.Content, "unknown_tool: nonexistent_tool")
		}
	}
}

// ---- 历史裁剪（单元级）----

// TestTrimHistory 裁剪：保留 system 首条；超出上限从最旧裁起；裁剪后不得
// 以孤儿 tool 消息开头（其配对的 assistant 已被裁掉时一并丢弃）。
func TestTrimHistory(t *testing.T) {
	msgs := []message{{Role: "system", Content: "sys"}}
	for i := range 10 {
		msgs = append(msgs, message{Role: "assistant", Content: "x", ToolCalls: []toolCall{{ID: fmt.Sprintf("c%d", i)}}})
		msgs = append(msgs, message{Role: "tool", ToolCallID: fmt.Sprintf("c%d", i)})
	}
	require.Len(t, msgs, 21)

	same := trimHistory(msgs)
	assert.Equal(t, msgs, same, "未超上限（historyCap+1）时原样保留")

	msgs = append(msgs, message{Role: "user", Content: "再来一首"}) // 22 条
	trimmed := trimHistory(msgs)
	require.Len(t, trimmed, historyCap, "裁到 historyCap（不含 system 另计）")
	assert.Equal(t, "system", trimmed[0].Role, "system 首条必须保留")
	assert.NotEqual(t, "tool", trimmed[1].Role, "裁剪后不得以孤儿 tool 消息开头")
	assert.Equal(t, "再来一首", trimmed[len(trimmed)-1].Content, "最新消息必须保留")
}

// ---- 同会话并发（回归：快照必须在持锁下完成）----

// gateExec 拦截指定工具的首次执行：进入即发 entered 信号并等待 release——
// 把第一轮对话停在工具循环中段，与第二轮对话构造真实的并发窗口。
type gateExec struct {
	inner   ToolExecutor
	tool    string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gateExec) Execute(ctx context.Context, tool string, args json.RawMessage) (string, error) {
	if tool == g.tool {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return g.inner.Execute(ctx, tool, args)
}

// collectEvents 排空事件通道直到关闭（带超时上限）；供非测试 goroutine 使用
// （不能 t.Fatal）。
func collectEvents(events <-chan ChatEvent, timeout time.Duration) []ChatEvent {
	var out []ChatEvent
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(timeout):
			return out
		}
	}
}

// TestChat_ConcurrentSameSession 同一 session_id 上的两次并发 Chat：第一轮
// 在 cast_play 执行中停靠，第二轮于停靠期间发起并跑完，随后放行第一轮。
// 会话历史的每一点访问都必须持 b.mu——此前 run 循环内的快照调用未持锁，
// 与并发轮次的加锁 append 构成数据竞争（-race 必报）。
//
// 注意结构：第二轮必须跑在独立 goroutine，且 release 前不得等它完成——
// 先 join 再放行会形成贯穿两轮历史访问的 happens-before 链，把竞争
// 恰好掩盖掉。
func TestChat_ConcurrentSameSession(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s := newStack(t, func(inner ToolExecutor) ToolExecutor {
		return &gateExec{inner: inner, tool: "cast_play", entered: entered, release: release}
	})
	items := s.lib.Search("sunset")
	require.Len(t, items, 1)
	argsJSON := fmt.Sprintf(`{"device":%q,"media_id":%q}`, devName, items[0].ID)
	s.llm.Script(
		fakellm.Resp{Body: toolCallBody("好的，马上为您播放。", "call-1", "cast_play", argsJSON)},
		fakellm.Resp{Body: textBody("已经在客厅播放")},
		fakellm.Resp{Body: textBody("好的")},
	)

	ctx := context.Background()
	ev1, err := s.brain.Chat(ctx, "s1", "把夕阳短片投到客厅")
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("未进入 cast_play 执行（门控失效）")
	}

	ev2Ch := make(chan []ChatEvent, 1)
	go func() {
		ev2, err := s.brain.Chat(ctx, "s1", "声音大一点")
		if err != nil {
			ev2Ch <- nil
			return
		}
		ev2Ch <- collectEvents(ev2, 15*time.Second)
	}()
	close(release) // 不等待第二轮完成即放行（理由见函数注释）

	ev2 := <-ev2Ch
	got1 := drain(t, ev1)

	// 松散断言（两轮的 LLM 请求指派次序本就不确定）：每轮恰有一个 final、
	// 均无 error，第一轮恰好执行过一次 cast_play。
	for _, got := range [][]ChatEvent{got1, ev2} {
		finals := 0
		for _, ev := range got {
			assert.NotEqual(t, "error", ev.Type, "不应产生 error 事件：%v", got)
			if ev.Type == "final" {
				finals++
			}
		}
		assert.Equal(t, 1, finals, "每轮应恰有一个 final：%v", got)
	}
	tools := 0
	for _, ev := range got1 {
		if ev.Type == "tool" && ev.Text == "cast_play" {
			tools++
		}
	}
	assert.Equal(t, 1, tools, "第一轮应恰执行一次 cast_play：%v", got1)
}

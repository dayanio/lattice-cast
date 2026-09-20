// Package webchat 的验收测试：GET /chat 服务单页（无需令牌）；POST
// /chat/api/message 与 MCP 同一 Bearer 鉴权；端到端：中文请求 → Fake LLM
// tool_calls → 执行核心在 Fake 渲染端真实播放 → SSE 帧 session/delta/tool/
// final 依序到达；第二条消息带 session_id 续接同一会话。
package webchat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dayanio/lattice-cast/internal/cast/brain"
	"github.com/dayanio/lattice-cast/internal/cast/config"
	"github.com/dayanio/lattice-cast/internal/cast/manager"
	"github.com/dayanio/lattice-cast/internal/cast/mcpserver"
	"github.com/dayanio/lattice-cast/internal/cast/resolve"
	"github.com/dayanio/lattice-cast/internal/cast/testsupport/fakellm"
	"github.com/dayanio/lattice-cast/internal/cast/testsupport/fakerenderer"
)

const (
	authToken = "tok-webchat"
	renderTok = "tok-renderer"
	devName   = "living-room-tv"
	devRoom   = "客厅"
	mediaBase = "http://media.example:7810"
)

type harness struct {
	ts   *httptest.Server
	fake *fakerenderer.Fake
	lib  *resolve.Library
	llm  *fakellm.Fake
}

// newHarness 复刻 main.go 的 brain 装配链（Fake 渲染端 + Manager + Resolver +
// 审计 + mcpserver 执行核心 → brain → webchat 路由挂上 mux）。
func newHarness(t *testing.T, resps ...fakellm.Resp) *harness {
	t.Helper()

	fake := fakerenderer.New(renderTok)
	t.Cleanup(fake.Close)
	tgt := fake.Target()

	cfg := config.Config{
		AuthToken:    authToken,
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

	audit, err := manager.OpenAudit(filepath.Join(t.TempDir(), "audit.jsonl"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = audit.Close() })

	srv := mcpserver.New(manager.New(cfg, lib, res), lib, res, audit, mcpserver.StaticToken(authToken))
	llm := fakellm.New(resps...)
	br := brain.New(config.Brain{
		Provider: "glm",
		APIKey:   "sk-webchat",
		Model:    "glm-4.7",
		BaseURL:  llm.URL,
	}, srv.Executor(), nil, nil) // router=nil：webchat 测试只关心 LLM 路径

	// 与 main.go 相同的路由挂载方式（Go 1.22+ 方法 + 路径模式）。
	mux := http.NewServeMux()
	wc := New(br, mcpserver.StaticToken(authToken))
	mux.HandleFunc("GET /chat", wc.Page)
	mux.HandleFunc("POST /chat/api/message", wc.Message)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return &harness{ts: ts, fake: fake, lib: lib, llm: llm}
}

// ---- OpenAI 兼容响应体模板（与 brain_test 同式：%q 负责转义）----

func toolCallBody(content, id, tool, argsJSON string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q,"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}]}`,
		content, id, tool, argsJSON)
}

func textBody(content string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q}}]}`, content)
}

// postMessage 发起一次聊天请求，读完响应体后返回响应（Header 仍可读）与原文。
func postMessage(t *testing.T, baseURL, token, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/chat/api/message", strings.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp, raw
}

// parseSSE 把响应体拆成 data: JSON 帧列表（键值均为 string 形态）。
func parseSSE(t *testing.T, raw []byte) []map[string]string {
	t.Helper()
	var frames []map[string]string
	for _, block := range strings.Split(string(raw), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			payload, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var m map[string]string
			require.NoError(t, json.Unmarshal([]byte(payload), &m), "帧应为 JSON 对象：%q", payload)
			frames = append(frames, m)
		}
	}
	return frames
}

// ---- 页面与鉴权 ----

// TestPageServed GET /chat 无需令牌即可取到单页（token 由页内录入存
// localStorage；页面本身是静态 HTML，不含任何敏感信息）。
func TestPageServed(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.ts.URL + "/chat")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(b), "投屏助手")
	assert.Contains(t, string(b), "/chat/api/message")
	assert.Contains(t, string(b), "lattice-cast-token")
}

// TestMessageUnauthorized POST /chat/api/message 与 MCP 同一 Bearer 鉴权：
// 无 token / 错 token → 401 {"error":"unauthorized"}。
func TestMessageUnauthorized(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"wrong token", "tok-wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := postMessage(t, h.ts.URL, tc.token, `{"text":"你好"}`)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			assert.Contains(t, string(raw), `"error":"unauthorized"`)
		})
	}
}

// TestMessageTextRequired 空 text → 400 text_required（不进 LLM、不建会话）。
func TestMessageTextRequired(t *testing.T) {
	h := newHarness(t)
	resp, raw := postMessage(t, h.ts.URL, authToken, `{"text":"   "}`)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(raw), "text_required")
	assert.Empty(t, h.llm.Requests(), "坏请求不得打到 LLM")
}

// ---- SSE 端到端 ----

// TestMessageSSEEndToEnd 完整链路：POST 消息 → SSE 首帧 session → delta →
// tool → final；渲染端真实收到拉流地址；第二条消息带 session_id 续接会话。
func TestMessageSSEEndToEnd(t *testing.T) {
	h := newHarness(t)
	items := h.lib.Search("sunset")
	require.Len(t, items, 1)
	argsJSON := fmt.Sprintf(`{"device":%q,"media_id":%q}`, devName, items[0].ID)
	h.llm.Script(
		fakellm.Resp{Body: toolCallBody("好的，马上为您播放。", "call-1", "cast_play", argsJSON)},
		fakellm.Resp{Body: textBody("已经在客厅播放")},
	)

	resp, raw := postMessage(t, h.ts.URL, authToken, `{"text":"把夕阳短片投到客厅"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	frames := parseSSE(t, raw)
	require.GreaterOrEqual(t, len(frames), 4, "session + delta + tool + final：%v", frames)
	assert.Equal(t, "session", frames[0]["type"])
	sid := frames[0]["session_id"]
	assert.NotEmpty(t, sid, "首帧应下发会话 id")
	assert.Equal(t, sid, resp.Header.Get("X-Session-Id"), "会话 id 也应写入响应头")

	var gotDelta, gotTool, gotFinal bool
	var finalText string
	for _, f := range frames[1:] {
		switch f["type"] {
		case "delta":
			gotDelta = true
			assert.Equal(t, "好的，马上为您播放。", f["text"])
		case "tool":
			gotTool = true
			assert.Equal(t, "cast_play", f["text"])
		case "final":
			gotFinal = true
			finalText = f["text"]
		case "error":
			t.Fatalf("不应有 error 帧：%v", f)
		}
	}
	assert.True(t, gotDelta && gotTool && gotFinal, "delta/tool/final 帧应齐备：%v", frames)
	assert.Equal(t, "已经在客厅播放", finalText)

	wantURL := h.lib.MediaURL(mediaBase, items[0].ID)
	assert.Equal(t, wantURL, h.fake.PlayedURL(), "渲染端应收下 NAS 直链并进入播放")

	// 第二条消息复用会话：同一 session_id 续接多轮。
	h.llm.Script(fakellm.Resp{Body: textBody("好的，已调大")})
	resp2, raw2 := postMessage(t, h.ts.URL, authToken, fmt.Sprintf(`{"session_id":%q,"text":"声音大一点"}`, sid))
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	frames2 := parseSSE(t, raw2)
	require.GreaterOrEqual(t, len(frames2), 2)
	assert.Equal(t, "session", frames2[0]["type"])
	assert.Equal(t, sid, frames2[0]["session_id"], "应回传同一会话 id")
	last := frames2[len(frames2)-1]
	assert.Equal(t, "final", last["type"])
	assert.Equal(t, "好的，已调大", last["text"])
}

// Package webchat 提供网页聊天入口：GET /chat 单页 UI（原生 JS、无构建
// 工具、见 page.go）+ POST /chat/api/message 的 SSE 事件流。鉴权与 MCP
// 端点同一 Bearer 令牌（Authenticator 接口与 mcpserver.Authenticator 同形，
// main 传入 mcpserver.StaticToken(cfg.AuthToken)）；同源部署，无 CORS。
// 仅当配置启用 brain（provider 非空）时由 main 挂载这两个路由。
//
// SSE 帧格式（data: JSON + 空行分隔）：
//
//	data: {"type":"session","session_id":"…"}   ← 首帧，请求未带 id 时新生成
//	data: {"type":"delta","text":"…"}           ← 过程文本
//	data: {"type":"tool","text":"cast_play"}    ← 工具开始执行
//	data: {"type":"final","text":"…"}           ← 最终答复
//	data: {"type":"error","text":"…"}           ← 中途失败（已脱敏）
package webchat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dayanio/lattice-cast/internal/cast/brain"
)

// Authenticator 校验一条 HTTP 请求：ok=false 时中间件直接 401。与
// mcpserver.Authenticator 同形（结构化类型隐式满足，本包不 import mcpserver）。
type Authenticator interface {
	Validate(r *http.Request) (agent string, ok bool)
}

// Handler 持有网页聊天入口的全部依赖。
type Handler struct {
	brain *brain.Brain
	auth  Authenticator
}

// New 构造 Handler。
func New(br *brain.Brain, auth Authenticator) *Handler {
	return &Handler{brain: br, auth: auth}
}

// Page 服务聊天单页（静态 HTML，不含任何敏感信息：令牌由用户在页内录入，
// 只落浏览器 localStorage）。
func (h *Handler) Page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, pageHTML)
}

// Message 处理 POST /chat/api/message：请求体 {"session_id":"…","text":"…"}
// （session_id 缺省时新生成），应答为 text/event-stream，brain 的事件逐帧
// 转发；客户端断开经请求上下文传导，循环随之收尾。
func (h *Handler) Message(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.auth.Validate(r); !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		Text      string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeJSONError(w, http.StatusBadRequest, "text_required")
		return
	}
	if req.SessionID == "" {
		req.SessionID = newSessionID()
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming_unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Session-Id", req.SessionID)
	w.WriteHeader(http.StatusOK)
	writeFrame(w, flusher, []byte(fmt.Sprintf(`{"type":"session","session_id":%q}`, req.SessionID)))

	events, err := h.brain.Chat(r.Context(), req.SessionID, req.Text)
	if err != nil {
		writeFrame(w, flusher, marshalEvent(brain.ChatEvent{Type: "error", Text: err.Error()}))
		return
	}
	for ev := range events {
		writeFrame(w, flusher, marshalEvent(ev))
	}
}

// marshalEvent 把 ChatEvent 序列化成 SSE data 帧（序列化失败以空对象占位，
// 不中断流）。
func marshalEvent(ev brain.ChatEvent) []byte {
	b, err := json.Marshal(ev)
	if err != nil {
		return []byte(`{"type":"error","text":"brain: encode event"}`)
	}
	return b
}

// writeFrame 写一帧 SSE 并即刻冲洗（事件到达即显示）。
func writeFrame(w http.ResponseWriter, f http.Flusher, data []byte) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	f.Flush()
}

// writeJSONError 以 JSON 错误体结束请求（与 MCP 的 401 体同一形态）。
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// newSessionID 生成 32 位 hex 会话 id；crypto/rand 失败（理论上不可达）时
// 以纳秒时间戳兜底，保证流仍可用。
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("s-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

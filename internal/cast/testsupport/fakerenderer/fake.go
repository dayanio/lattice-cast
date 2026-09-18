// Package fakerenderer 提供内存态 LatticeCast 渲染端测试替身：按协议唯一权威
// 定义 docs/protocol.md v1 实现全部端点，维护 idle →(play)→ playing ⇄(pause/play)
// paused →(stop)→ idle 与 error 状态机（error 仅由 /play 失败进入，粘滞至下一次
// /play，/pause /stop /seek /volume 均不改变 error 态）。
// 位置不随真实时间推进，保证测试确定性；Task 7（发现 e2e）与 Task 10/11/12
// 的测试直接复用，未来 DLNA / 树莓派适配器复用 TestClientConformance 调用序列。
package fakerenderer

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/dayanio/lattice-cast/internal/cast/adapter"
)

// 渲染端四态（protocol.md 第六节）
const (
	stateIdle    = "idle"
	statePlaying = "playing"
	statePaused  = "paused"
	stateError   = "error"
)

// Fake 是内存态渲染端，内嵌 *httptest.Server（监听 127.0.0.1 随机端口）。
type Fake struct {
	*httptest.Server

	token string

	mu         sync.Mutex
	state      string
	url        string
	title      string
	positionMS int64
	durationMS int64
	errMsg     string
	failArmed  bool   // 是否已注入下一次 /play 失败
	failNext   string // 注入的失败原因（如 "source_unreachable"）
}

// New 启动一个监听随机端口的 Fake 渲染端。
func New(token string) *Fake {
	f := &Fake{token: token, state: stateIdle}
	mux := http.NewServeMux()
	mux.HandleFunc("/play", f.auth(f.postOnly(f.handlePlay)))
	mux.HandleFunc("/pause", f.auth(f.postOnly(f.handlePause)))
	mux.HandleFunc("/stop", f.auth(f.postOnly(f.handleStop)))
	mux.HandleFunc("/seek", f.auth(f.postOnly(f.handleSeek)))
	mux.HandleFunc("/volume", f.auth(f.postOnly(f.handleVolume)))
	mux.HandleFunc("/status", f.auth(f.getOnly(f.handleStatus)))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, errResp{Error: "not_found"})
	})
	f.Server = httptest.NewServer(mux)
	return f
}

// Target 返回指向本 Fake 的渲染端地址（含 token）。
func (f *Fake) Target() adapter.Target {
	addr := f.Listener.Addr().(*net.TCPAddr)
	return adapter.Target{Host: addr.IP.String(), Port: addr.Port, Token: f.token}
}

// SetPlaying 预置播放态（测试用）：直接进入 playing 并带上标题与时长；
// 位置保持原值（缺省 0），供测试跳过播放前置步骤。
func (f *Fake) SetPlaying(url, title string, durationMS int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = statePlaying
	f.url = url
	f.title = title
	f.durationMS = durationMS
	f.errMsg = ""
}

// FailNextPlay 注入下一次 /play 的失败原因（如 "source_unreachable"）：
// 该次 /play 返回 ok=false 且渲染端进入 error 态。
func (f *Fake) FailNextPlay(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failArmed = true
	f.failNext = msg
}

// ---- 路由中间件：鉴权 + 方法校验（错误体格式见 protocol.md 第五节）----

// auth 校验 Bearer token；缺失或不符返回 401。
func (f *Fake) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			writeJSON(w, http.StatusUnauthorized, errResp{Error: "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (f *Fake) postOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, errResp{Error: "method_not_allowed"})
			return
		}
		next(w, r)
	}
}

func (f *Fake) getOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, errResp{Error: "method_not_allowed"})
			return
		}
		next(w, r)
	}
}

// ---- 端点处理 ----

// handlePlay 播放指定 URL：成功进入 playing（从 idle/paused/error 任意状态）；
// 注入失败或 url 为空时分别按注入值 / bad_request 报错。
func (f *Fake) handlePlay(w http.ResponseWriter, r *http.Request) {
	var req adapter.PlayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "bad_request"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failArmed { // 注入失败：进入 error 态并清空旧媒体
		f.failArmed = false
		f.enterErrorLocked(f.failNext)
		writeJSON(w, http.StatusOK, cmdResp{State: stateError, Error: f.failNext})
		return
	}
	if req.URL == "" { // url 必填
		writeJSON(w, http.StatusBadRequest, errResp{Error: "bad_request"})
		return
	}
	f.state = statePlaying
	f.url = req.URL
	f.title = req.Title
	f.positionMS = req.PositionMS
	f.durationMS = 0 // Fake 不探测媒体，时长未知记 0（需要时长用 SetPlaying 预置）
	f.errMsg = ""
	writeJSON(w, http.StatusOK, cmdResp{OK: true, State: statePlaying})
}

// handlePause 暂停：playing → paused；其余状态幂等不迁移（含 error）。
func (f *Fake) handlePause(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == statePlaying {
		f.state = statePaused
	}
	writeJSON(w, http.StatusOK, cmdResp{OK: true, State: f.state})
}

// handleStop 停止并清空媒体：playing|paused → idle；idle 幂等；error 态不迁移。
func (f *Fake) handleStop(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == statePlaying || f.state == statePaused {
		f.state = stateIdle
		f.url, f.title, f.errMsg = "", "", ""
		f.positionMS, f.durationMS = 0, 0
	}
	writeJSON(w, http.StatusOK, cmdResp{OK: true, State: f.state})
}

// handleSeek 跳转位置：playing|paused 时生效，不改变状态；其余状态幂等。
func (f *Fake) handleSeek(w http.ResponseWriter, r *http.Request) {
	var req adapter.SeekRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "bad_request"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == statePlaying || f.state == statePaused {
		f.positionMS = req.PositionMS
	}
	writeJSON(w, http.StatusOK, cmdResp{OK: true, State: f.state})
}

// handleVolume 设置音量：校验请求体，不改变播放状态与进度
// （v1 的 /status 不含音量字段，仅回执当前状态）。
func (f *Fake) handleVolume(w http.ResponseWriter, r *http.Request) {
	var req adapter.VolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "bad_request"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, http.StatusOK, cmdResp{OK: true, State: f.state})
}

// handleStatus 返回当前状态快照；200 响应不含 ok 字段，error 为空串而非省略。
func (f *Fake) handleStatus(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, http.StatusOK, statusResp{
		State:      f.state,
		PositionMS: f.positionMS,
		DurationMS: f.durationMS,
		Title:      f.title,
		Error:      f.errMsg,
	})
}

// enterErrorLocked 进入 error 态并清空旧媒体信息（调用方须持有 f.mu），
// 对齐契约夹具 status_error_response.json 的零值形态。
func (f *Fake) enterErrorLocked(msg string) {
	f.state = stateError
	f.url, f.title, f.errMsg = "", "", msg
	f.positionMS, f.durationMS = 0, 0
}

// ---- 响应体（与契约夹具字段一致，小写 snake_case）----

// cmdResp 是 play/pause/stop/seek/volume 的响应体；error 仅失败时出现。
type cmdResp struct {
	OK    bool   `json:"ok"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// statusResp 是 GET /status 的响应体：无 ok 字段，无错误时 error 为 ""（不是 null）。
type statusResp struct {
	State      string `json:"state"`
	PositionMS int64  `json:"position_ms"`
	DurationMS int64  `json:"duration_ms"`
	Title      string `json:"title"`
	Error      string `json:"error"`
}

// errResp 是 401/404/405/400 的通用错误响应体。
type errResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

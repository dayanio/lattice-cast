// Package renderer 实现 LatticeCast 渲染端（macOS/Linux，Go + mpv）：
//
//   - Server：wire protocol v1 的渲染端 HTTP 服务（docs/protocol.md 唯一权威
//     定义），持有 idle →(play)→ playing ⇄(pause/play) paused →(stop)→ idle
//     与 error 状态机（error 仅由 /play 失败进入，粘滞至下一次 /play），
//     播放中若 Controller 自报 idle（播完 EOF）亦迁到 idle——见 handleStatus，
//     与测试替身 internal/cast/testsupport/fakerenderer 行为同构；
//   - Controller：播放后端抽象，位置/时长/标题由其 Status() 供给
//     （生产实现为 MpvController，测试用内存假实现）；
//   - Announce：mDNS 发布 _latticecast._tcp 供 cast-agent 发现。
package renderer

import (
	"context"
	"encoding/json"
	"net/http"
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

// Controller 抽象播放后端（mpv 真实现 + 测试假实现）。
// 状态机归 Server 所有：Controller 不上报协议态，仅供给
// Status() 中的位置/时长/标题供 GET /status 组装。
type Controller interface {
	Load(ctx context.Context, url, title string, positionMS int64) error
	Pause() error
	Stop() error
	SeekTo(ms int64) error
	Volume(level int) error
	Status() adapter.Status
}

// Server 是渲染端 HTTP 服务（protocol.md 五端点 + 401/404/405/400 语义）。
// room/name/port 为渲染端身份元数据（mDNS 发布用同一组值，见 announce.go）。
type Server struct {
	token string
	room  string
	name  string
	port  int
	ctl   Controller

	mu     sync.Mutex
	state  string // idle|playing|paused|error 之一
	title  string // 最近一次成功 /play 的标题（error/idle 时清空）
	errMsg string // error 态的出错原因，其余状态为 ""
}

// NewServer 构造渲染端服务；初始状态 idle。
func NewServer(token, room, name string, port int, ctl Controller) *Server {
	return &Server{token: token, room: room, name: name, port: port, ctl: ctl, state: stateIdle}
}

// Handler 返回协议路由：/play /pause /stop /seek /volume（POST）与
// /status（GET），鉴权与错误体格式见 protocol.md 第二、五节。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/play", s.auth(s.postOnly(s.handlePlay)))
	mux.HandleFunc("/pause", s.auth(s.postOnly(s.handlePause)))
	mux.HandleFunc("/stop", s.auth(s.postOnly(s.handleStop)))
	mux.HandleFunc("/seek", s.auth(s.postOnly(s.handleSeek)))
	mux.HandleFunc("/volume", s.auth(s.postOnly(s.handleVolume)))
	mux.HandleFunc("/status", s.auth(s.getOnly(s.handleStatus)))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, errResp{Error: "not_found"})
	})
	return mux
}

// ---- 路由中间件：鉴权 + 方法校验（错误体格式见 protocol.md 第五节）----

// auth 校验 Bearer token；缺失或不符返回 401。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			writeJSON(w, http.StatusUnauthorized, errResp{Error: "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) postOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, errResp{Error: "method_not_allowed"})
			return
		}
		next(w, r)
	}
}

func (s *Server) getOnly(next http.HandlerFunc) http.HandlerFunc {
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
// Controller.Load 失败时进入 error 态（HTTP 仍 200，应用层报错）。
// 携 position_ms>0 时把续播位置折叠进 Load（渲染端经 mpv loadfile 的
// start=+<sec> 起播；mpv 的 loadfile 异步生效，load 后立即 seek 会打在
// 加载中的文件上被 mpv 拒绝，故不再单独下发 seek）。
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	var req adapter.PlayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "bad_request"})
		return
	}
	if req.URL == "" { // url 必填
		writeJSON(w, http.StatusBadRequest, errResp{Error: "bad_request"})
		return
	}

	if err := s.ctl.Load(r.Context(), req.URL, req.Title, req.PositionMS); err != nil {
		s.mu.Lock()
		s.state, s.title, s.errMsg = stateError, "", err.Error()
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, cmdResp{State: stateError, Error: err.Error()})
		return
	}

	s.mu.Lock()
	s.state, s.title, s.errMsg = statePlaying, req.Title, ""
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, cmdResp{OK: true, State: statePlaying})
}

// handlePause 暂停：playing → paused；其余状态幂等不迁移（含 error）。
func (s *Server) handlePause(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()

	if state == statePlaying {
		if err := s.ctl.Pause(); err != nil {
			writeJSON(w, http.StatusOK, cmdResp{State: state, Error: err.Error()})
			return
		}
		s.mu.Lock()
		s.state = statePaused
		state = s.state
		s.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, cmdResp{OK: true, State: state})
}

// handleStop 停止并清空媒体：playing|paused → idle；idle 幂等；error 态不迁移。
func (s *Server) handleStop(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()

	if state == statePlaying || state == statePaused {
		if err := s.ctl.Stop(); err != nil {
			writeJSON(w, http.StatusOK, cmdResp{State: state, Error: err.Error()})
			return
		}
		s.mu.Lock()
		s.state, s.title, s.errMsg = stateIdle, "", ""
		state = s.state
		s.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, cmdResp{OK: true, State: state})
}

// handleSeek 跳转位置：playing|paused 时生效，不改变状态；其余状态幂等。
func (s *Server) handleSeek(w http.ResponseWriter, r *http.Request) {
	var req adapter.SeekRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "bad_request"})
		return
	}

	s.mu.Lock()
	state := s.state
	s.mu.Unlock()

	if state == statePlaying || state == statePaused {
		if err := s.ctl.SeekTo(req.PositionMS); err != nil {
			writeJSON(w, http.StatusOK, cmdResp{State: state, Error: err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, cmdResp{OK: true, State: state})
}

// handleVolume 设置音量：校验请求体，不改变播放状态与进度
// （v1 的 /status 不含音量字段，仅回执当前状态）。
func (s *Server) handleVolume(w http.ResponseWriter, r *http.Request) {
	var req adapter.VolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "bad_request"})
		return
	}

	s.mu.Lock()
	state := s.state
	s.mu.Unlock()

	if state == statePlaying || state == statePaused {
		if err := s.ctl.Volume(req.Level); err != nil {
			writeJSON(w, http.StatusOK, cmdResp{State: state, Error: err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, cmdResp{OK: true, State: state})
}

// handleStatus 返回当前状态快照：状态机取 Server 记录，位置/时长/标题取
// Controller.Status()；error/idle 态信息清零，对齐契约夹具的零值形态；
// 200 响应不含 ok 字段，error 为空串而非省略。
// 播放中若 Controller 自报 idle（mpv --keep-open 播完 EOF：eof-reached=true、
// 无媒体在播），Server 状态机随之迁移到 idle（进度与标题清零）——否则
// /status 会对着已定格的画面谎报 "playing"；该迁移粘滞，直到下一次 /play。
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	cs := s.ctl.Status()

	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.state {
	case stateError:
		writeJSON(w, http.StatusOK, statusResp{State: stateError, Error: s.errMsg})
	case stateIdle:
		writeJSON(w, http.StatusOK, statusResp{State: stateIdle})
	default: // playing|paused
		if cs.State == stateIdle {
			s.state, s.title, s.errMsg = stateIdle, "", ""
			writeJSON(w, http.StatusOK, statusResp{State: stateIdle})
			return
		}
		writeJSON(w, http.StatusOK, statusResp{
			State:      s.state,
			PositionMS: cs.PositionMS,
			DurationMS: cs.DurationMS,
			Title:      s.title,
		})
	}
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

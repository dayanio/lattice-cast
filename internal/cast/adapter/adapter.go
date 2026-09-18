// Package adapter 定义 cast 适配层的共享类型：各端点的请求/状态结构与渲染端 Target。
// LatticeCast wire protocol v1 的具体客户端实现见子包 latticecast，
// 协议唯一权威定义见 docs/protocol.md。
package adapter

import "fmt"

// PlayRequest 是 POST /play 的请求体。
type PlayRequest struct {
	URL string `json:"url"`
	// Title 可选展示标题（极简 UI / 审计用），空则不下发。
	Title string `json:"title,omitempty"`
	// PositionMS 起播位置（毫秒）。始终下发（契约夹具 play_request.json 含
	// "position_ms":0），渲染端缺省按 0 处理。
	PositionMS int64 `json:"position_ms"`
}

// SeekRequest 是 POST /seek 的请求体。
type SeekRequest struct {
	PositionMS int64 `json:"position_ms"`
}

// VolumeRequest 是 POST /volume 的请求体；Level 取值 0-100。
type VolumeRequest struct {
	Level int `json:"level"`
}

// Status 是渲染端状态。GET /status 响应回填全部字段；
// play/pause/stop/seek/volume 的响应只含 ok/state，映射为仅 State 非零。
type Status struct {
	State      string `json:"state"`
	PositionMS int64  `json:"position_ms"`
	DurationMS int64  `json:"duration_ms"`
	Title      string `json:"title"`
	Error      string `json:"error"`
}

// Target 定位一台渲染端
type Target struct {
	Host  string
	Port  int
	Token string
}

// BaseURL 返回渲染端根地址，形如 http://host:port
func (t Target) BaseURL() string {
	return fmt.Sprintf("http://%s:%d", t.Host, t.Port)
}

// Package manager 是 cast-agent 的设备管理器：把发现（discovery）、配置
// （config）、协议客户端（adapter/latticecast）与媒体解析（resolve）合成
// 设备级操作，供 Task 11 的 MCP 工具层调用。
//
// 职责边界：
//   - 设备身份 = mDNS 实例名 = config.Renderers 的 key；
//   - host/port/token 由配置播种，Refresh 用 discovery.Browse 的输出按
//     Found.Name 逐字更新（token 不在发现结果里，恒取自配置）；
//   - 操作失败时网络类错误（*url.Error）包上 "device_offline:" 前缀供
//     MCP 层甄别；未知设备报 "unknown_device"，音量越界报 "level_out_of_range"；
//   - 本包不做审计：审计是 MCP 层的职责，AuditLog.Record 为其而设。
package manager

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"sync"

	"github.com/dayanio/lattice-cast/internal/cast/adapter"
	"github.com/dayanio/lattice-cast/internal/cast/adapter/latticecast"
	"github.com/dayanio/lattice-cast/internal/cast/config"
	"github.com/dayanio/lattice-cast/internal/cast/discovery"
	"github.com/dayanio/lattice-cast/internal/cast/resolve"
)

// Device 是一台设备对外的清单视图（MCP list_devices 的元素）。
// 离线时 Online=false 且 State/NowPlaying 为空（omitempty 不出现在 JSON 里）；
// Online=true 且 State="error" 表示渲染端应答了但报错（如 token 不符）——
// 设备活着，问题多半在配置，不得误报成离线。
type Device struct {
	Name       string `json:"name"`
	Room       string `json:"room"`
	Online     bool   `json:"online"`
	NowPlaying string `json:"now_playing,omitempty"`
	State      string `json:"state,omitempty"`
}

// dev 是 Manager 内部的每设备记录：身份（map key）+ 当前寻址信息。
type dev struct {
	Room  string
	Host  string // 空 = 尚未寻址（仅组播条目且未发现）
	Port  int
	Token string
}

// Manager 是设备注册表与播放操作的入口。lib/res 供后续任务扩展
// （如按 media_id 播放的辅助方法），当前操作直接消费已解析的 PlayRequest。
type Manager struct {
	cfg    config.Config
	client *latticecast.Client
	lib    *resolve.Library
	res    *resolve.Resolver

	mu   sync.Mutex
	devs map[string]dev
}

// New 构造 Manager：注册表以配置播种（静态条目即刻可寻址，无需等待发现），
// 协议客户端使用默认 HTTP 传输。
func New(cfg config.Config, lib *resolve.Library, res *resolve.Resolver) *Manager {
	m := &Manager{
		cfg:    cfg,
		client: latticecast.NewClient(nil),
		lib:    lib,
		res:    res,
		devs:   make(map[string]dev, len(cfg.Renderers)),
	}
	for name, r := range cfg.Renderers {
		m.devs[name] = dev{Room: r.Room, Host: r.Host, Port: r.Port, Token: r.Token}
	}
	return m
}

// Refresh 浏览一次局域网（discovery.Browse），把发现输出按 Found.Name 逐字
// 并入注册表（host/port/room 以发现为准，token 仍取自配置）。组播不可用时
// Browse 降级为仅静态条目；Browse 出错则保留原注册表不动。
func (m *Manager) Refresh(ctx context.Context) {
	found, err := discovery.Browse(ctx, m.cfg)
	if err != nil {
		return // 发现失败：保留已知寻址信息，交给后续逐台探测判在线
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range found {
		m.devs[f.Name] = dev{Room: f.Room, Host: f.Host, Port: f.Port, Token: m.cfg.Renderers[f.Name].Token}
	}
}

// List 刷新注册表后逐台探测状态：在线设备回填 state/now_playing；网络不可达
// （或尚未寻址）的设备 Online=false 且两字段为空；渲染端应答了但报错（如
// token 不符的 401、ok=false）的设备仍在线，标记 State="error"——设备活着，
// 异常须如实呈现而非误报离线。清单按设备名排序，覆盖配置中全部设备（离线
// 设备不得从清单消失）。探测失败不视为整体失败。
func (m *Manager) List(ctx context.Context) ([]Device, error) {
	m.Refresh(ctx)

	m.mu.Lock()
	names := make([]string, 0, len(m.devs))
	for name := range m.devs {
		names = append(names, name)
	}
	sort.Strings(names)
	snapshot := make(map[string]dev, len(m.devs))
	for name, d := range m.devs {
		snapshot[name] = d
	}
	m.mu.Unlock()

	devices := make([]Device, 0, len(names))
	for _, name := range names {
		d := snapshot[name]
		device := Device{Name: name, Room: d.Room}
		if st, err := m.client.Status(ctx, adapter.Target{Host: d.Host, Port: d.Port, Token: d.Token}); err == nil {
			device.Online = true
			device.State = st.State
			device.NowPlaying = st.Title
		} else if !netUnreachable(err) {
			// 渲染端应答了但报错（如 token 不符的 401、ok=false）：
			// 在线但异常 → State="error"，供 LLM 甄别配置问题。
			device.Online = true
			device.State = "error"
		}
		// 网络不可达（*url.Error）：保持 Online=false 且两字段为空。
		devices = append(devices, device)
	}
	return devices, nil
}

// Play 向指定设备下发播放。未知设备报 unknown_device；
// 网络失败包 device_offline 前缀（底层 *url.Error 仍可 errors.As 解包）。
func (m *Manager) Play(ctx context.Context, name string, r adapter.PlayRequest) (adapter.Status, error) {
	t, err := m.target(name)
	if err != nil {
		return adapter.Status{}, err
	}
	st, err := m.client.Play(ctx, t, r)
	if err != nil {
		return adapter.Status{}, offline(err)
	}
	return st, nil
}

// Stop 停止指定设备并清空当前媒体（渲染端幂等）。
func (m *Manager) Stop(ctx context.Context, name string) (adapter.Status, error) {
	t, err := m.target(name)
	if err != nil {
		return adapter.Status{}, err
	}
	st, err := m.client.Stop(ctx, t)
	if err != nil {
		return adapter.Status{}, offline(err)
	}
	return st, nil
}

// Pause 暂停指定设备播放（渲染端幂等：已在 paused 时原样回执）。
func (m *Manager) Pause(ctx context.Context, name string) (adapter.Status, error) {
	t, err := m.target(name)
	if err != nil {
		return adapter.Status{}, err
	}
	st, err := m.client.Pause(ctx, t)
	if err != nil {
		return adapter.Status{}, offline(err)
	}
	return st, nil
}

// Seek 跳转指定设备播放位置（不改变播放状态；位置范围由渲染端裁决）。
func (m *Manager) Seek(ctx context.Context, name string, positionMS int64) (adapter.Status, error) {
	t, err := m.target(name)
	if err != nil {
		return adapter.Status{}, err
	}
	st, err := m.client.Seek(ctx, t, adapter.SeekRequest{PositionMS: positionMS})
	if err != nil {
		return adapter.Status{}, offline(err)
	}
	return st, nil
}

// Volume 设置指定设备音量。level <0 或 >100 拒绝并报 level_out_of_range
// （不下发网络请求）；0 与 100 为合法边界。
func (m *Manager) Volume(ctx context.Context, name string, level int) (adapter.Status, error) {
	if level < 0 || level > 100 {
		return adapter.Status{}, fmt.Errorf("level_out_of_range: %d", level)
	}
	t, err := m.target(name)
	if err != nil {
		return adapter.Status{}, err
	}
	st, err := m.client.Volume(ctx, t, adapter.VolumeRequest{Level: level})
	if err != nil {
		return adapter.Status{}, offline(err)
	}
	return st, nil
}

// Status 查询指定设备当前状态。
func (m *Manager) Status(ctx context.Context, name string) (adapter.Status, error) {
	t, err := m.target(name)
	if err != nil {
		return adapter.Status{}, err
	}
	st, err := m.client.Status(ctx, t)
	if err != nil {
		return adapter.Status{}, offline(err)
	}
	return st, nil
}

// target 按设备名取当前寻址信息；未知设备报 unknown_device。
func (m *Manager) target(name string) (adapter.Target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devs[name]
	if !ok {
		return adapter.Target{}, fmt.Errorf("unknown_device: %s", name)
	}
	return adapter.Target{Host: d.Host, Port: d.Port, Token: d.Token}, nil
}

// netUnreachable 判定是否网络类失败（*url.Error）：List 逐台探测与各操作
// 共用同一判据——网络不可达即离线；渲染端应答的应用层错误（如
// *latticecast.ProtocolError 的 unauthorized/not_found）不算。
func netUnreachable(err error) bool {
	var ue *url.Error
	return errors.As(err, &ue)
}

// offline 把网络类失败包上 device_offline 前缀供 MCP 层甄别；应用层错误
// （*latticecast.ProtocolError，如 unauthorized/not_found）原样上抛。
func offline(err error) error {
	if netUnreachable(err) {
		return fmt.Errorf("device_offline: %w", err)
	}
	return err
}

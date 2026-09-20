// 本文件：意图快通道的规则实现与共享骨架。每条规则返回 (handled, events)：
// handled=true 时 events 即完整回复（终事件为 final 或 error）；拿不准的情形
// （搜索零命中/搜索工具失败）返回 handled=false 交还 LLM——换词重试正是
// LLM 工具循环的强项，本地不替用户下「没找到」的死刑判决。
package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// 规则句式（正则即文档；数字段交 parseCount 统一解析阿拉伯/中文数字）。
var (
	reCastRoomFirst = regexp.MustCompile(`^把(?P<room>.+?)播放(?P<title>.+)$`)
	reCastToRoom    = regexp.MustCompile(`^把(?P<title>.+)投到(?P<room>.+?)上?$`)
	rePlayBare      = regexp.MustCompile(`^播放(?P<title>.+)$`)
	rePause         = regexp.MustCompile(`^暂停(一下)?$`)
	reResume        = regexp.MustCompile(`^(继续(播放)?|接着播)$`)
	reStop          = regexp.MustCompile(`^(停止|停掉|别播了)$`)
	reSeekTo        = regexp.MustCompile(`^快进到第?(?P<n>[\d一二两三四五六七八九十]+)(?P<unit>分钟|秒)$`)
	reSeekForward   = regexp.MustCompile(`^快进(?P<n>[\d一二两三四五六七八九十]+)(?P<unit>分钟|秒)$`)
	reSeekBackward  = regexp.MustCompile(`^后退(?P<n>[\d一二两三四五六七八九十]+)(?P<unit>分钟|秒)$`)
	reVolumeSet     = regexp.MustCompile(`^音量调到(?P<n>[\d一二两三四五六七八九十]+)$`)
	reVolumeUp      = regexp.MustCompile(`^大点声$`)
	reVolumeDown    = regexp.MustCompile(`^小点声$`)
	reStatus        = regexp.MustCompile(`^(现在播什么|在播什么|现在在播什么|正在播什么|播到哪了|播到哪里了)$`)
)

// multiHitCap 多命中选项的罗列上限（超出部分以省略行交代总数）。
const multiHitCap = 8

// devInfo 是规则引擎视角的设备清单条目（list_cast_devices 出参元素）。
type devInfo struct {
	Name       string `json:"name"`
	Room       string `json:"room"`
	Online     bool   `json:"online"`
	NowPlaying string `json:"now_playing,omitempty"`
	State      string `json:"state,omitempty"`
}

// searchHit 是 search_media 的一条命中（出参元素）。
type searchHit struct {
	ID    string `json:"media_id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
}

// statusBody 是渲染端状态（cast_status / cast_play 出参内嵌的 status）。
type statusBody struct {
	State      string `json:"state"`
	PositionMS int64  `json:"position_ms"`
	DurationMS int64  `json:"duration_ms"`
	Title      string `json:"title"`
	Error      string `json:"error"`
}

// statusOut 是 cast_status / cast_resume 的出参外壳（{"status":{…}}）。
type statusOut struct {
	Status statusBody `json:"status"`
}

// resolveTarget 把话语里的房间名（或设备名）解析成目标设备：房间映射优先，
// 设备名直呼次之；都不认识返回 false（交 LLM 处理，不瞎猜目标）。
func (r *Router) resolveTarget(name string) (devInfo, bool) {
	if dev, ok := r.rooms[name]; ok {
		return devInfo{Name: dev, Room: name}, true
	}
	for room, dev := range r.rooms {
		if dev == name {
			return devInfo{Name: dev, Room: room}, true
		}
	}
	return devInfo{}, false
}

// ---- 共享骨架 ----

// call 发一次工具调用（经 ToolExecutor 执行核心——审计路径自动覆盖），并把
// tool 事件追加进事件流。失败转述（final 还是 error 事件）由调用方决定。
func (r *Router) call(ctx context.Context, events *[]ChatEvent, tool string, args map[string]any) (string, error) {
	*events = append(*events, ChatEvent{Type: evTool, Text: tool})
	out, err := r.exec.Execute(ctx, tool, argsJSON(args))
	if out == "" && err == nil {
		out = "{}"
	}
	return out, err
}

// toolFail 把工具执行失败转成 error 事件：中文标签 + 既有契约错误文本
// （resolve/manager 侧已脱敏，与 MCP IsError 同源）。
func toolFail(events *[]ChatEvent, label string, err error) {
	*events = append(*events, ChatEvent{Type: evError, Text: label + "失败：" + err.Error()})
}

// listDevices 经执行核心拉设备清单。
func (r *Router) listDevices(ctx context.Context, events *[]ChatEvent) ([]devInfo, error) {
	out, err := r.call(ctx, events, "list_cast_devices", nil)
	if err != nil {
		return nil, err
	}
	var devs []devInfo
	if err := json.Unmarshal([]byte(out), &devs); err != nil {
		return nil, fmt.Errorf("list_cast_devices: bad_output: %v", err)
	}
	return devs, nil
}

// pickDevice 在线设备收敛：恰一台 → 返回该设备（ask 为 nil）；零台/多台 →
// 返回 final（「没有在线设备」或反问哪一台），绝不替用户擅自选设备。
// question 是问句主干（不含结尾问号，可含房间名占位之外的完整措辞）。
func pickDevice(devs []devInfo, question string) (devInfo, *ChatEvent) {
	var online []devInfo
	for _, d := range devs {
		if d.Online {
			online = append(online, d)
		}
	}
	switch {
	case len(online) == 0:
		return devInfo{}, &ChatEvent{Type: evFinal, Text: "当前没有在线的投屏设备。"}
	case len(online) > 1:
		rooms := make([]string, 0, len(online))
		for _, d := range online {
			rooms = append(rooms, roomOf(d))
		}
		return devInfo{}, &ChatEvent{Type: evFinal, Text: fmt.Sprintf("有 %d 台设备在线（%s），%s？", len(online), strings.Join(rooms, "、"), question)}
	default:
		return online[0], nil
	}
}

// pickOnline 列设备并收敛到唯一在线设备；不能收敛（零台/多台）或查询失败时
// 已把回复事件追加进 events 并返回 done=true。
func (r *Router) pickOnline(ctx context.Context, events *[]ChatEvent, question string) (devInfo, bool) {
	devs, err := r.listDevices(ctx, events)
	if err != nil {
		toolFail(events, "查询设备", err)
		return devInfo{}, true
	}
	picked, ask := pickDevice(devs, question)
	if ask != nil {
		*events = append(*events, *ask)
		return devInfo{}, true
	}
	return picked, false
}

// roomOf 设备的展示名：房间名优先，缺省退设备名。
func roomOf(d devInfo) string {
	if d.Room != "" {
		return d.Room
	}
	return d.Name
}

// argsJSON 把工具入参 map 序列化成执行核心吃的 JSON；nil 按空对象处理。
func argsJSON(kv map[string]any) json.RawMessage {
	if len(kv) == 0 {
		return json.RawMessage("{}")
	}
	b, err := json.Marshal(kv)
	if err != nil {
		return json.RawMessage("{}") // map[string]any 仅含基本类型，序列化不可能失败
	}
	return b
}

// ---- 投片 ----

// handleCast 播放规则：search_media → 唯一命中即 cast_play（media_id 原样
// 透传，不杜撰 id）；多命中列选项请用户说完整片名；零命中/搜索失败 →
// handled=false 交 LLM 换词重试。target 为 nil 时（裸播放）先按在线设备
// 收敛目标。
func (r *Router) handleCast(ctx context.Context, title string, target *devInfo) (bool, []ChatEvent) {
	var events []ChatEvent
	if target == nil {
		dev, done := r.pickOnline(ctx, &events, "请问要播到哪一台")
		if done {
			return true, events
		}
		target = &dev
	}

	out, err := r.call(ctx, &events, "search_media", map[string]any{"query": title})
	if err != nil {
		return false, nil // 搜索工具失败（如 reflux 不可达）：LLM 的换词重试更擅长
	}
	var hits []searchHit
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		toolFail(&events, "解析搜索结果", err)
		return true, events
	}

	switch {
	case len(hits) == 0:
		return false, nil // 零命中：交 LLM 换词重试
	case len(hits) == 1:
		if _, err := r.call(ctx, &events, "cast_play", map[string]any{
			"device":   target.Name,
			"media_id": hits[0].ID,
			"title":    hits[0].Title,
		}); err != nil {
			toolFail(&events, "播放", err)
			return true, events
		}
		events = append(events, ChatEvent{Type: evFinal, Text: fmt.Sprintf("已在%s播放《%s》。", roomOf(*target), hits[0].Title)})
		return true, events
	default:
		events = append(events, ChatEvent{Type: evFinal, Text: multiHitFinal(title, hits)})
		return true, events
	}
}

// multiHitFinal 列出多命中选项（至多 multiHitCap 条）请用户选择。
func multiHitFinal(title string, hits []searchHit) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "找到 %d 个与「%s」相关的结果，请说完整片名：", len(hits), title)
	for i, h := range hits {
		if i == multiHitCap {
			fmt.Fprintf(&sb, "\n……（其余 %d 个略）", len(hits)-i)
			break
		}
		fmt.Fprintf(&sb, "\n%d. %s", i+1, h.Title)
	}
	return sb.String()
}

// ---- 暂停 / 停止（同款设备选择逻辑）----

func (r *Router) handlePause(ctx context.Context) (bool, []ChatEvent) {
	var events []ChatEvent
	dev, done := r.pickOnline(ctx, &events, "请问要暂停哪一台")
	if done {
		return true, events
	}
	if _, err := r.call(ctx, &events, "cast_pause", map[string]any{"device": dev.Name}); err != nil {
		toolFail(&events, "暂停", err)
		return true, events
	}
	events = append(events, ChatEvent{Type: evFinal, Text: playingFinal("已暂停", dev)})
	return true, events
}

func (r *Router) handleStop(ctx context.Context) (bool, []ChatEvent) {
	var events []ChatEvent
	dev, done := r.pickOnline(ctx, &events, "请问要停止哪一台")
	if done {
		return true, events
	}
	if _, err := r.call(ctx, &events, "cast_stop", map[string]any{"device": dev.Name}); err != nil {
		toolFail(&events, "停止", err)
		return true, events
	}
	events = append(events, ChatEvent{Type: evFinal, Text: playingFinal("已停止", dev)})
	return true, events
}

// playingFinal 用选设备时的清单快照拼「已暂停《X》（客厅）。」式回执；快照
// 无在播信息时退化为「已暂停（客厅）。」。
func playingFinal(verb string, dev devInfo) string {
	if dev.NowPlaying != "" {
		return fmt.Sprintf("%s《%s》（%s）。", verb, dev.NowPlaying, roomOf(dev))
	}
	return fmt.Sprintf("%s（%s）。", verb, roomOf(dev))
}

// ---- 继续（断点续播，经 cast_resume 走 ToolExecutor 审计路径）----

func (r *Router) handleResume(ctx context.Context) (bool, []ChatEvent) {
	var events []ChatEvent
	dev, done := r.pickOnline(ctx, &events, "请问要继续播放哪一台")
	if done {
		return true, events
	}
	if _, err := r.call(ctx, &events, "cast_resume", map[string]any{"device": dev.Name}); err != nil {
		if strings.HasPrefix(err.Error(), "no_last_played") {
			events = append(events, ChatEvent{Type: evFinal, Text: fmt.Sprintf("「%s」没有可继续的播放断点，先说「播放 片名」投一部吧。", roomOf(dev))})
			return true, events
		}
		toolFail(&events, "继续播放", err)
		return true, events
	}
	// 断点的标题/位置经 cast_status 读回拼回复；查询失败不致命，退化为
	// 不带片名的确认。
	out, err := r.call(ctx, &events, "cast_status", map[string]any{"device": dev.Name})
	if err != nil {
		events = append(events, ChatEvent{Type: evFinal, Text: fmt.Sprintf("已继续播放（%s）。", roomOf(dev))})
		return true, events
	}
	var so statusOut
	if err := json.Unmarshal([]byte(out), &so); err != nil {
		events = append(events, ChatEvent{Type: evFinal, Text: fmt.Sprintf("已继续播放（%s）。", roomOf(dev))})
		return true, events
	}
	what := ""
	if so.Status.Title != "" {
		what = fmt.Sprintf("《%s》", so.Status.Title)
	}
	if minute := so.Status.PositionMS / 60000; minute > 0 {
		events = append(events, ChatEvent{Type: evFinal, Text: fmt.Sprintf("继续播放%s（%s），从第 %d 分钟开始。", what, roomOf(dev), minute)})
	} else {
		events = append(events, ChatEvent{Type: evFinal, Text: fmt.Sprintf("继续播放%s（%s）。", what, roomOf(dev))})
	}
	return true, events
}

// ---- 快进 / 后退 ----

// unitMS 把捕获的时间单位换算成毫秒（正则只可能给出 分钟/秒 两种）。
func unitMS(unit string) int64 {
	if unit == "秒" {
		return 1000
	}
	return 60000
}

// seekAbsolute 「快进到第?{N}分钟/秒」：绝对跳转（N × 单位换算毫秒）。
func (r *Router) seekAbsolute(ctx context.Context, msg string) (bool, []ChatEvent) {
	m := reSeekTo.FindStringSubmatch(msg)
	count, ok := parseCount(m[reSeekTo.SubexpIndex("n")])
	if !ok {
		return false, nil
	}
	unit := m[reSeekTo.SubexpIndex("unit")]
	var events []ChatEvent
	dev, done := r.pickOnline(ctx, &events, "请问要在哪一台快进")
	if done {
		return true, events
	}
	if _, err := r.call(ctx, &events, "cast_seek", map[string]any{
		"device": dev.Name, "position_ms": int64(count) * unitMS(unit),
	}); err != nil {
		toolFail(&events, "跳转", err)
		return true, events
	}
	events = append(events, ChatEvent{Type: evFinal, Text: fmt.Sprintf("已快进到第 %d %s。", count, unit)})
	return true, events
}

// seekRelative 「快进{N}分钟/秒 / 后退{N}分钟/秒」：先读当前状态再相对跳转；
// 后退越过片头时钳到 0（负数会被渲染端裁决层拒绝，这里就地收好）。
func (r *Router) seekRelative(ctx context.Context, msg string, re *regexp.Regexp, back bool) (bool, []ChatEvent) {
	m := re.FindStringSubmatch(msg)
	count, ok := parseCount(m[re.SubexpIndex("n")])
	if !ok {
		return false, nil
	}
	unit := m[re.SubexpIndex("unit")]
	var events []ChatEvent
	question := "请问要在哪一台快进"
	if back {
		question = "请问要在哪一台后退"
	}
	dev, done := r.pickOnline(ctx, &events, question)
	if done {
		return true, events
	}
	out, err := r.call(ctx, &events, "cast_status", map[string]any{"device": dev.Name})
	if err != nil {
		toolFail(&events, "查询状态", err)
		return true, events
	}
	var so statusOut
	if err := json.Unmarshal([]byte(out), &so); err != nil {
		toolFail(&events, "解析状态", err)
		return true, events
	}
	delta := int64(count) * unitMS(unit)
	target := so.Status.PositionMS + delta
	if back {
		target = so.Status.PositionMS - delta
		if target < 0 {
			target = 0
		}
	}
	if _, err := r.call(ctx, &events, "cast_seek", map[string]any{
		"device": dev.Name, "position_ms": target,
	}); err != nil {
		toolFail(&events, "跳转", err)
		return true, events
	}
	verb := "已快进"
	if back {
		verb = "已后退"
	}
	events = append(events, ChatEvent{Type: evFinal, Text: fmt.Sprintf("%s %d %s，现在第 %d 分钟。", verb, count, unit, target/60000)})
	return true, events
}

// ---- 音量 ----

// volumeSet 「音量调到{N}」：范围本地先校验（越界不下发工具调用）。
func (r *Router) volumeSet(ctx context.Context, level int) (bool, []ChatEvent) {
	var events []ChatEvent
	if level < 0 || level > 100 {
		events = append(events, ChatEvent{Type: evFinal, Text: "音量范围是 0 到 100，请换个数。"})
		return true, events
	}
	return r.applyVolume(ctx, &events, level)
}

// volumeStepBy 「大点声/小点声」：基线 ±20 后钳到 0-100（基线口径见包注释）。
func (r *Router) volumeStepBy(ctx context.Context, delta int) (bool, []ChatEvent) {
	var events []ChatEvent
	dev, done := r.pickOnline(ctx, &events, "请问要调哪一台的音量")
	if done {
		return true, events
	}
	level := r.baselineVolume(dev.Name) + delta
	if level < 0 {
		level = 0
	}
	if level > 100 {
		level = 100
	}
	return r.applyVolumeOnDevice(ctx, &events, dev, level)
}

// applyVolume 选设备后下发绝对音量（音量调到{N} 用）。
func (r *Router) applyVolume(ctx context.Context, events *[]ChatEvent, level int) (bool, []ChatEvent) {
	dev, done := r.pickOnline(ctx, events, "请问要调哪一台的音量")
	if done {
		return true, *events
	}
	return r.applyVolumeOnDevice(ctx, events, dev, level)
}

// applyVolumeOnDevice 对已选定设备下发音量并更新基线记忆。
func (r *Router) applyVolumeOnDevice(ctx context.Context, events *[]ChatEvent, dev devInfo, level int) (bool, []ChatEvent) {
	if _, err := r.call(ctx, events, "cast_volume", map[string]any{"device": dev.Name, "level": level}); err != nil {
		toolFail(events, "音量调节", err)
		return true, *events
	}
	r.mu.Lock()
	r.volume[dev.Name] = level
	r.mu.Unlock()
	*events = append(*events, ChatEvent{Type: evFinal, Text: fmt.Sprintf("已把%s音量调到 %d。", roomOf(dev), level)})
	return true, *events
}

// baselineVolume 相对调节的当前基线（未知设备按 volumeBaseline 起算）。
func (r *Router) baselineVolume(device string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.volume[device]; ok {
		return v
	}
	return volumeBaseline
}

// ---- 状态 ----

func (r *Router) handleStatus(ctx context.Context) (bool, []ChatEvent) {
	var events []ChatEvent
	dev, done := r.pickOnline(ctx, &events, "请问要看哪一台")
	if done {
		return true, events
	}
	out, err := r.call(ctx, &events, "cast_status", map[string]any{"device": dev.Name})
	if err != nil {
		toolFail(&events, "查询状态", err)
		return true, events
	}
	var so statusOut
	if err := json.Unmarshal([]byte(out), &so); err != nil {
		toolFail(&events, "解析状态", err)
		return true, events
	}
	events = append(events, ChatEvent{Type: evFinal, Text: statusFinal(roomOf(dev), so.Status)})
	return true, events
}

// statusFinal 把渲染端状态转述成中文（「客厅正在播放《三体》，第 12 分钟。」）。
func statusFinal(room string, st statusBody) string {
	what := ""
	if st.Title != "" {
		what = fmt.Sprintf("《%s》", st.Title)
	}
	minute := st.PositionMS / 60000
	switch st.State {
	case "playing":
		if what == "" {
			return fmt.Sprintf("%s正在播放，第 %d 分钟。", room, minute)
		}
		return fmt.Sprintf("%s正在播放%s，第 %d 分钟。", room, what, minute)
	case "paused":
		if what == "" {
			return fmt.Sprintf("%s已暂停，第 %d 分钟。", room, minute)
		}
		return fmt.Sprintf("%s已暂停%s，第 %d 分钟。", room, what, minute)
	case "error":
		if st.Error != "" {
			return fmt.Sprintf("%s播放出错（%s）。", room, st.Error)
		}
		return room + "播放出错。"
	default: // idle 及其他未知状态
		return fmt.Sprintf("%s当前没有在播内容。", room)
	}
}

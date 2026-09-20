// Package intent 是本地规则引擎快通道（LatticeCast v2.1）：跑在 LLM 大脑
// 之前的中文高频指令前置层。家庭投屏的高频话术是一个极小语法（暂停/继续/
// 快进/音量/状态/投片），本地正则匹配 + 工具直执毫秒级完成，而 LLM 一轮
// 10~20 秒——命中即直执，未命中原样落回 LLM 工具循环（零损失）。
//
// 职责边界：
//   - 纯规则引擎：只依赖 ToolExecutor 接口（与 brain.ToolExecutor 同形，
//     mcpserver.Executor() 的适配器隐式满足）——路由命中的执行与 LLM 路径
//     走同一执行核心，审计自动覆盖；
//   - ChatEvent 与 brain.ChatEvent 是同一类型（brain 侧以类型别名复用，
//     避免反向依赖成环），SSE/webchat 层零改动；
//   - 无会话、无历史：每句话语独立裁决；多台在线时反问用户，绝不擅自
//     选设备；搜索零命中/工具失败等拿不准的情形交还 LLM（handled=false）。
//
// 音量口径：协议 GET /status 不含音量，相对调节（大点声/小点声）以快通道
// 最近一次推算值为基线（未知按 50 起算），随每次成功调节滚动更新。
package intent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"unicode/utf8"
)

// ChatEvent 是一次回复的事件流元素（brain.ChatEvent 的本体：brain 侧以
// 类型别名复用）。Type 取值与 LLM 路径同义：
//   - "tool"：一个工具开始执行（Text 为工具名）；
//   - "final"：最终答复（本轮结束）；
//   - "error"：执行失败（Text 为中文标签 + 已脱敏的契约错误文本）。
type ChatEvent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// 事件类型取值（与 brain 的语义一致）。
const (
	evTool  = "tool"
	evFinal = "final"
	evError = "error"
)

// ToolExecutor 是工具执行核心的抽象：与 brain.ToolExecutor 同形（Go 结构化
// 类型隐式实现，mcpserver.Executor() 同时满足两者）。
type ToolExecutor interface {
	Execute(ctx context.Context, tool string, argsJSON json.RawMessage) (result string, err error)
}

// 相对音量调节的步长与未知基线（协议无音量读数，见包注释的口径说明）。
const (
	volumeStep     = 20
	volumeBaseline = 50
)

// Router 是意图快通道：房间名 → 设备名映射 + 工具执行核心 + 音量基线记忆。
type Router struct {
	rooms map[string]string // 房间名 → 设备名（来自配置；由 New 拷贝，防外部改写）
	exec  ToolExecutor

	mu     sync.Mutex
	volume map[string]int // 设备名 → 快通道最近推算的音量（相对调节的基线）
}

// New 构造 Router。rooms 为空时投片规则仅支持裸播放的在线设备收敛；
// exec 为 nil 时 TryHandle 恒返回 handled=false（构造方保证不传 nil）。
func New(rooms map[string]string, exec ToolExecutor) *Router {
	roomsCopy := make(map[string]string, len(rooms))
	for room, dev := range rooms {
		roomsCopy[room] = dev
	}
	return &Router{rooms: roomsCopy, exec: exec, volume: make(map[string]int)}
}

// TryHandle 对一句话语做规则裁决：命中 → 执行工具并返回完整回复事件流
// （handled=true，调用方不再调 LLM）；未命中或拿不准（如搜索零命中）→
// handled=false，调用方落回 LLM。
func (r *Router) TryHandle(ctx context.Context, text string) (handled bool, events []ChatEvent) {
	if r == nil || r.exec == nil {
		return false, nil
	}
	msg := normalize(text)
	if msg == "" {
		return false, nil
	}

	// 投片：房间在前的句式优先（房间名经映射校验，不认识就不瞎猜）。
	if m := reCastRoomFirst.FindStringSubmatch(msg); m != nil {
		target, ok := r.resolveTarget(m[reCastRoomFirst.SubexpIndex("room")])
		if !ok {
			return false, nil
		}
		return r.handleCast(ctx, m[reCastRoomFirst.SubexpIndex("title")], &target)
	}
	if m := reCastToRoom.FindStringSubmatch(msg); m != nil {
		target, ok := r.resolveTarget(m[reCastToRoom.SubexpIndex("room")])
		if !ok {
			return false, nil
		}
		return r.handleCast(ctx, m[reCastToRoom.SubexpIndex("title")], &target)
	}
	if m := rePlayBare.FindStringSubmatch(msg); m != nil {
		return r.handleCast(ctx, m[rePlayBare.SubexpIndex("title")], nil)
	}

	switch {
	case rePause.MatchString(msg):
		return r.handlePause(ctx)
	case reResume.MatchString(msg):
		return r.handleResume(ctx)
	case reStop.MatchString(msg):
		return r.handleStop(ctx)
	case reSeekTo.MatchString(msg):
		return r.seekAbsolute(ctx, msg)
	case reSeekForward.MatchString(msg):
		return r.seekRelative(ctx, msg, reSeekForward, false)
	case reSeekBackward.MatchString(msg):
		return r.seekRelative(ctx, msg, reSeekBackward, true)
	case reVolumeSet.MatchString(msg):
		m := reVolumeSet.FindStringSubmatch(msg)
		n, ok := parseCount(m[reVolumeSet.SubexpIndex("n")])
		if !ok {
			return false, nil
		}
		return r.volumeSet(ctx, n)
	case reVolumeUp.MatchString(msg):
		return r.volumeStepBy(ctx, +volumeStep)
	case reVolumeDown.MatchString(msg):
		return r.volumeStepBy(ctx, -volumeStep)
	case reStatus.MatchString(msg):
		return r.handleStatus(ctx)
	}
	return false, nil
}

// normalize 归一化一句指令：去首尾空白；剥结尾标点（语音/输入法常见）；
// 剥前缀客套词（可叠加，如「麻烦请帮我暂停」）。只做无损收紧，不改写正文。
func normalize(text string) string {
	s := strings.TrimSpace(text)
	// 客套词（长的在前，避免「麻烦你」被「麻烦」截断后残留「你」）。
	for changed := true; changed; {
		changed = false
		for _, filler := range []string{"麻烦你", "请你", "帮我", "劳驾", "麻烦", "请"} {
			if strings.HasPrefix(s, filler) {
				s = strings.TrimSpace(s[len(filler):])
				changed = true
			}
		}
	}
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		switch r {
		case '。', '，', '、', '！', '？', '；', '~', '～', ',', '.', '!', '?', ';', ' ', '\t':
			s = s[:len(s)-size]
			continue
		}
		break
	}
	return strings.TrimSpace(s)
}

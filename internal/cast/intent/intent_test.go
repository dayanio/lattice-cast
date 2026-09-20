// Package intent 的验收测试：以脚本化 Fake 执行核心（按工具名应答 + 全量
// 记录调用）驱动规则引擎——每类规则的正例（参数解析、工具调用序列、final
// 文案）与反例（未命中落回 LLM、零搜索命中落回 LLM、多设备反问、多命中
// 列选项、契约错误的中文转述），外加中文数字解析与音量基线记忆。
package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeExec 是脚本化 ToolExecutor：respond 按工具名应答，全部调用（工具名 +
// 入参 JSON）按序记录供断言。
type fakeExec struct {
	mu      sync.Mutex
	calls   []fakeCall
	respond func(tool string, args string) (string, error)
}

type fakeCall struct {
	tool string
	args string
}

func (f *fakeExec) Execute(_ context.Context, tool string, argsJSON json.RawMessage) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{tool: tool, args: string(argsJSON)})
	f.mu.Unlock()
	if f.respond == nil {
		return "", fmt.Errorf("fakeExec: no script for %s", tool)
	}
	return f.respond(tool, string(argsJSON))
}

func (f *fakeExec) toolsCalled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.tool
	}
	return out
}

// argsOf 返回某工具最后一次调用的入参 JSON（未调用过报错）。
func (f *fakeExec) argsOf(t *testing.T, tool string) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].tool == tool {
			var m map[string]any
			require.NoError(t, json.Unmarshal([]byte(f.calls[i].args), &m))
			return m
		}
	}
	t.Fatalf("工具 %s 未被调用", tool)
	return nil
}

// ---- 工具出参构造（与 mcpserver 执行核心的 JSON 形态逐字对齐）----

type fakeDev struct {
	Name       string `json:"name"`
	Room       string `json:"room"`
	Online     bool   `json:"online"`
	NowPlaying string `json:"now_playing,omitempty"`
	State      string `json:"state,omitempty"`
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func devicesOut(devs ...fakeDev) string { return mustJSON(devs) }

func searchOut(hits ...map[string]string) string {
	items := make([]map[string]string, 0, len(hits))
	for _, h := range hits {
		items = append(items, map[string]string{"media_id": h["id"], "title": h["title"], "kind": h["kind"]})
	}
	return mustJSON(items)
}

func statusResp(state, title string, posMS int64) string {
	return mustJSON(map[string]any{"status": map[string]any{
		"state": state, "position_ms": posMS, "duration_ms": int64(0), "title": title,
	}})
}

func playOut() string {
	return mustJSON(map[string]any{"status": map[string]any{"state": "playing"}, "adapter": "latticecast"})
}

// ---- 被测构造 ----

const (
	tvA   = "tv-a"
	roomA = "客厅"
	tvB   = "tv-b"
	roomB = "卧室"
)

func newRouter(f *fakeExec) *Router {
	return New(map[string]string{roomA: tvA, roomB: tvB}, f)
}

// finalOf 取事件流里 final 的文本（没有则测试失败）。
func finalOf(t *testing.T, events []ChatEvent) string {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "final" {
			return events[i].Text
		}
	}
	t.Fatalf("事件流里没有 final: %+v", events)
	return ""
}

func allText(events []ChatEvent) string {
	var sb strings.Builder
	for _, ev := range events {
		sb.WriteString(ev.Type)
		sb.WriteString(":")
		sb.WriteString(ev.Text)
		sb.WriteString("\n")
	}
	return sb.String()
}

// hasErrorEvent 判定事件流里是否含 error 事件。
func hasErrorEvent(events []ChatEvent) bool {
	for _, ev := range events {
		if ev.Type == "error" {
			return true
		}
	}
	return false
}

// ---- 中文数字解析 ----

func TestParseCount(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"3", 3, true},
		{"0", 0, true},
		{"45", 45, true},
		{"三", 3, true},
		{"两", 2, true},
		{"十", 10, true},
		{"十五", 15, true},
		{"二十", 20, true},
		{"二十五", 25, true},
		{"九十九", 99, true},
		{" 三 ", 3, true},
		{"百", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseCount(c.in)
		if c.ok {
			require.True(t, ok, "%q 应可解析", c.in)
			assert.Equal(t, c.want, got, "%q", c.in)
		} else {
			require.False(t, ok, "%q 不应可解析", c.in)
		}
	}
}

// ---- 投片 ----

// TestCast_ToRoomUniqueHit 把{title}投到{room}：唯一命中 → cast_play 带
// media_id 投到房间对应设备，final 报「已在{room}播放《…》」。
func TestCast_ToRoomUniqueHit(t *testing.T) {
	f := &fakeExec{respond: func(tool, _ string) (string, error) {
		switch tool {
		case "search_media":
			return searchOut(map[string]string{"id": "m-1", "title": "三体", "kind": "movie"}), nil
		case "cast_play":
			return playOut(), nil
		}
		return "", fmt.Errorf("unexpected tool %s", tool)
	}}
	r := newRouter(f)

	handled, events := r.TryHandle(context.Background(), "把三体投到客厅")
	require.True(t, handled)
	assert.Equal(t, []string{"search_media", "cast_play"}, f.toolsCalled())
	play := f.argsOf(t, "cast_play")
	assert.Equal(t, tvA, play["device"])
	assert.Equal(t, "m-1", play["media_id"])
	assert.Equal(t, finalOf(t, events), "已在客厅播放《三体》。")
}

// TestCast_RoomFirstVariant 把{room}播放{title} 变体同样可投。
func TestCast_RoomFirstVariant(t *testing.T) {
	f := &fakeExec{respond: func(tool, _ string) (string, error) {
		if tool == "search_media" {
			return searchOut(map[string]string{"id": "m-9", "title": "流浪地球", "kind": "movie"}), nil
		}
		return playOut(), nil
	}}
	r := newRouter(f)

	handled, events := r.TryHandle(context.Background(), "把卧室播放流浪地球")
	require.True(t, handled)
	play := f.argsOf(t, "cast_play")
	assert.Equal(t, tvB, play["device"])
	assert.Equal(t, "m-9", play["media_id"])
	assert.Contains(t, finalOf(t, events), "已在卧室播放《流浪地球》")
}

// TestCast_UnknownRoomFallsThrough 房间名不认识 → handled=false 落回 LLM
// （不瞎猜目标设备）。
func TestCast_UnknownRoomFallsThrough(t *testing.T) {
	f := &fakeExec{}
	r := newRouter(f)

	handled, _ := r.TryHandle(context.Background(), "把三体投到厨房")
	assert.False(t, handled)
	assert.Empty(t, f.toolsCalled())
}

// TestCast_BareTitleSingleOnline 播放{title}（无房间）：恰一台在线设备 →
// 投到它。
func TestCast_BareTitleSingleOnline(t *testing.T) {
	f := &fakeExec{respond: func(tool, _ string) (string, error) {
		switch tool {
		case "list_cast_devices":
			return devicesOut(fakeDev{Name: tvA, Room: roomA, Online: true, State: "idle"}), nil
		case "search_media":
			return searchOut(map[string]string{"id": "m-1", "title": "三体", "kind": "movie"}), nil
		default:
			return playOut(), nil
		}
	}}
	r := newRouter(f)

	handled, events := r.TryHandle(context.Background(), "播放三体")
	require.True(t, handled)
	play := f.argsOf(t, "cast_play")
	assert.Equal(t, tvA, play["device"])
	assert.Contains(t, finalOf(t, events), "已在客厅播放《三体》")
}

// TestCast_MultiMediaHitListsOptions 多命中 → final 列出选项请用户选择，
// 不替用户选片（不调 cast_play）。
func TestCast_MultiMediaHitListsOptions(t *testing.T) {
	f := &fakeExec{respond: func(tool, _ string) (string, error) {
		if tool == "search_media" {
			return searchOut(
				map[string]string{"id": "m-1", "title": "三体 第一季", "kind": "series"},
				map[string]string{"id": "m-2", "title": "三体 电影版", "kind": "movie"},
			), nil
		}
		return playOut(), nil
	}}
	r := newRouter(f)

	handled, events := r.TryHandle(context.Background(), "把三体投到客厅")
	require.True(t, handled)
	assert.NotContains(t, f.toolsCalled(), "cast_play", "多命中不得擅自播放")
	final := finalOf(t, events)
	assert.Contains(t, final, "三体 第一季")
	assert.Contains(t, final, "三体 电影版")
}

// TestCast_ZeroHitsFallsThrough 零命中 → handled=false 交 LLM 换词重试
// （本地不作「没找到」的死刑判决）。
func TestCast_ZeroHitsFallsThrough(t *testing.T) {
	f := &fakeExec{respond: func(tool, _ string) (string, error) {
		return searchOut(), nil
	}}
	r := newRouter(f)

	handled, _ := r.TryHandle(context.Background(), "把不存在的片子投到客厅")
	assert.False(t, handled)
}

// TestCast_SearchToolErrorFallsThrough 搜索工具本身失败（如 reflux 不可达）
// → handled=false 落回 LLM（其换词重试循环正是为检索麻烦设计的）。
func TestCast_SearchToolErrorFallsThrough(t *testing.T) {
	f := &fakeExec{respond: func(tool, _ string) (string, error) {
		return "", fmt.Errorf("reflux_unreachable")
	}}
	r := newRouter(f)

	handled, _ := r.TryHandle(context.Background(), "把三体投到客厅")
	assert.False(t, handled)
}

// TestCast_MultiDeviceAsk 裸播放 + 多台在线 → final 反问哪一台（列出房间名），
// 不擅自选设备。
func TestCast_MultiDeviceAsk(t *testing.T) {
	f := &fakeExec{respond: func(tool, _ string) (string, error) {
		switch tool {
		case "list_cast_devices":
			return devicesOut(
				fakeDev{Name: tvA, Room: roomA, Online: true},
				fakeDev{Name: tvB, Room: roomB, Online: true},
			), nil
		case "search_media":
			return searchOut(map[string]string{"id": "m-1", "title": "三体", "kind": "movie"}), nil
		default:
			return playOut(), nil
		}
	}}
	r := newRouter(f)

	handled, events := r.TryHandle(context.Background(), "播放三体")
	require.True(t, handled)
	assert.NotContains(t, f.toolsCalled(), "cast_play")
	final := allText(events)
	assert.Contains(t, final, "客厅")
	assert.Contains(t, final, "卧室")
}

// ---- 暂停 / 停止 ----

func pauseScript(idle bool) func(tool, _ string) (string, error) {
	return func(tool, _ string) (string, error) {
		switch tool {
		case "list_cast_devices":
			return devicesOut(fakeDev{Name: tvA, Room: roomA, Online: true, NowPlaying: "三体", State: "playing"}), nil
		default:
			if idle {
				return mustJSON(map[string]any{"status": map[string]any{"state": "idle"}}), nil
			}
			return mustJSON(map[string]any{"status": map[string]any{"state": "paused"}}), nil
		}
	}
}

// TestPause_SingleDevice 暂停/暂停一下：恰一台在线 → cast_pause，final 带
// 片名与房间。
func TestPause_SingleDevice(t *testing.T) {
	for _, text := range []string{"暂停", "暂停一下"} {
		f := &fakeExec{respond: pauseScript(false)}
		r := newRouter(f)

		handled, events := r.TryHandle(context.Background(), text)
		require.True(t, handled, text)
		assert.Equal(t, []string{"list_cast_devices", "cast_pause"}, f.toolsCalled())
		assert.Equal(t, tvA, f.argsOf(t, "cast_pause")["device"])
		final := finalOf(t, events)
		assert.Contains(t, final, "已暂停")
		assert.Contains(t, final, "三体")
		assert.Contains(t, final, "客厅")
	}
}

// TestPause_MultiDeviceAsk 多台在线 → 反问哪一台。
func TestPause_MultiDeviceAsk(t *testing.T) {
	f := &fakeExec{respond: func(tool, _ string) (string, error) {
		return devicesOut(
			fakeDev{Name: tvA, Room: roomA, Online: true},
			fakeDev{Name: tvB, Room: roomB, Online: true, NowPlaying: "夜曲"},
		), nil
	}}
	r := newRouter(f)

	handled, events := r.TryHandle(context.Background(), "暂停")
	require.True(t, handled)
	assert.NotContains(t, f.toolsCalled(), "cast_pause")
	final := finalOf(t, events)
	assert.Contains(t, final, "哪一台")
	assert.Contains(t, final, "客厅")
	assert.Contains(t, final, "卧室")
}

// TestPause_NoDeviceOnline 零台在线 → final「没有在线设备」。
func TestPause_NoDeviceOnline(t *testing.T) {
	f := &fakeExec{respond: func(tool, _ string) (string, error) {
		return devicesOut(fakeDev{Name: tvA, Room: roomA, Online: false}), nil
	}}
	r := newRouter(f)

	handled, events := r.TryHandle(context.Background(), "暂停")
	require.True(t, handled)
	assert.Contains(t, finalOf(t, events), "没有在线")
}

// TestStop_Variants 停止/别播了/停掉 同暂停的设备选择逻辑。
func TestStop_Variants(t *testing.T) {
	for _, text := range []string{"停止", "别播了", "停掉"} {
		f := &fakeExec{respond: pauseScript(false)}
		r := newRouter(f)

		handled, events := r.TryHandle(context.Background(), text)
		require.True(t, handled, text)
		assert.Contains(t, f.toolsCalled(), "cast_stop")
		assert.Equal(t, tvA, f.argsOf(t, "cast_stop")["device"])
		assert.Contains(t, finalOf(t, events), "已停止")
	}
}

// ---- 继续 ----

// TestResume_WithBreakpoint 继续/接着播：经 cast_resume 续播（走 ToolExecutor
// 审计路径），final 报从断点位置继续。
func TestResume_WithBreakpoint(t *testing.T) {
	for _, text := range []string{"继续", "接着播", "继续播放"} {
		f := &fakeExec{respond: func(tool, _ string) (string, error) {
			switch tool {
			case "list_cast_devices":
				return devicesOut(fakeDev{Name: tvA, Room: roomA, Online: true}), nil
			case "cast_resume":
				return mustJSON(map[string]any{"status": map[string]any{"state": "playing"}, "adapter": "latticecast"}), nil
			default:
				return statusResp("playing", "三体", 1500000), nil
			}
		}}
		r := newRouter(f)

		handled, events := r.TryHandle(context.Background(), text)
		require.True(t, handled, text)
		assert.Contains(t, f.toolsCalled(), "cast_resume")
		assert.Equal(t, tvA, f.argsOf(t, "cast_resume")["device"])
		final := finalOf(t, events)
		assert.Contains(t, final, "继续")
		assert.Contains(t, final, "三体")
		assert.Contains(t, final, "25")
	}
}

// TestResume_NoBreakpoint 无断点（契约错误 no_last_played）→ 中文 final
// 说明，不落 error 事件吓用户。
func TestResume_NoBreakpoint(t *testing.T) {
	f := &fakeExec{respond: func(tool, _ string) (string, error) {
		switch tool {
		case "list_cast_devices":
			return devicesOut(fakeDev{Name: tvA, Room: roomA, Online: true}), nil
		default:
			return "", fmt.Errorf("no_last_played: %s", tvA)
		}
	}}
	r := newRouter(f)

	handled, events := r.TryHandle(context.Background(), "继续")
	require.True(t, handled)
	final := allText(events)
	assert.Contains(t, final, "final:")
	assert.Contains(t, final, "断点")
	assert.NotContains(t, final, "error:", "no_last_played 应转述成 final")
}

// ---- 快进 / 后退 ----

// seekScript 造一台位置在 posMS 的在线设备（cast_status 应答）。
func seekScript(posMS int64) func(tool, _ string) (string, error) {
	return func(tool, _ string) (string, error) {
		switch tool {
		case "list_cast_devices":
			return devicesOut(fakeDev{Name: tvA, Room: roomA, Online: true}), nil
		case "cast_status":
			return statusResp("playing", "三体", posMS), nil
		default:
			return mustJSON(map[string]any{"status": map[string]any{"state": "playing"}}), nil
		}
	}
}

// TestSeek_Absolute 快进到{N}分钟（绝对跳转，阿拉伯数字 + 可选「第」+ 变体）。
func TestSeek_Absolute(t *testing.T) {
	cases := []struct {
		text   string
		wantMS int64
	}{
		{"快进到20分钟", 1200000},
		{"快进到第3分钟", 180000},
		{"快进到二十五分钟", 1500000},
	}
	for _, c := range cases {
		f := &fakeExec{respond: seekScript(60000)}
		r := newRouter(f)

		handled, events := r.TryHandle(context.Background(), c.text)
		require.True(t, handled, c.text)
		assert.Equal(t, float64(c.wantMS), f.argsOf(t, "cast_seek")["position_ms"], c.text)
		assert.Contains(t, finalOf(t, events), "快进到")
	}
}

// TestSeek_RelativeForward 快进{N}分钟（相对）：现位置 + N。
func TestSeek_RelativeForward(t *testing.T) {
	f := &fakeExec{respond: seekScript(600000)}
	r := newRouter(f)

	handled, events := r.TryHandle(context.Background(), "快进三分钟")
	require.True(t, handled)
	assert.Equal(t, float64(600000+180000), f.argsOf(t, "cast_seek")["position_ms"])
	final := finalOf(t, events)
	assert.Contains(t, final, "快进 3 分钟")
	assert.Contains(t, final, "13")
}

// TestSeek_Backward 后退{N}分钟：现位置 − N；退过片头钳到 0。
func TestSeek_Backward(t *testing.T) {
	f := &fakeExec{respond: seekScript(600000)}
	r := newRouter(f)
	handled, _ := r.TryHandle(context.Background(), "后退3分钟")
	require.True(t, handled)
	assert.Equal(t, float64(420000), f.argsOf(t, "cast_seek")["position_ms"])

	f2 := &fakeExec{respond: seekScript(600000)}
	r2 := newRouter(f2)
	handled, events := r2.TryHandle(context.Background(), "后退三十分钟")
	require.True(t, handled)
	assert.Equal(t, float64(0), f2.argsOf(t, "cast_seek")["position_ms"], "退过片头应钳到 0")
	assert.Contains(t, finalOf(t, events), "后退 30 分钟")
}

// TestSeek_AbsoluteSeconds 秒形态的绝对跳转（规则③的秒形态，Fix round 1）：
// 阿拉伯与中文数字，final 以「秒」表述。
func TestSeek_AbsoluteSeconds(t *testing.T) {
	cases := []struct {
		text   string
		wantMS int64
	}{
		{"快进到第90秒", 90000},
		{"快进到十五秒", 15000},
	}
	for _, c := range cases {
		f := &fakeExec{respond: seekScript(60000)}
		r := newRouter(f)

		handled, events := r.TryHandle(context.Background(), c.text)
		require.True(t, handled, c.text)
		assert.Equal(t, float64(c.wantMS), f.argsOf(t, "cast_seek")["position_ms"], c.text)
		final := finalOf(t, events)
		assert.Contains(t, final, "快进到", c.text)
		assert.Contains(t, final, "秒", c.text)
	}
}

// TestSeek_RelativeSeconds 秒形态的相对跳转：现位置 ± N 秒；final 单位用
// 「秒」、位置仍以分钟表述。
func TestSeek_RelativeSeconds(t *testing.T) {
	f := &fakeExec{respond: seekScript(600000)}
	r := newRouter(f)
	handled, events := r.TryHandle(context.Background(), "快进30秒")
	require.True(t, handled)
	assert.Equal(t, float64(630000), f.argsOf(t, "cast_seek")["position_ms"])
	final := finalOf(t, events)
	assert.Contains(t, final, "快进 30 秒")
	assert.Contains(t, final, "第 10 分钟", "630000ms 应折算为第 10 分钟")

	f2 := &fakeExec{respond: seekScript(600000)}
	r2 := newRouter(f2)
	handled, events2 := r2.TryHandle(context.Background(), "后退十五秒")
	require.True(t, handled)
	assert.Equal(t, float64(585000), f2.argsOf(t, "cast_seek")["position_ms"])
	assert.Contains(t, finalOf(t, events2), "后退 15 秒")
}

// ---- 音量 ----

func volumeScript(cur int64, state string) func(tool, _ string) (string, error) {
	return func(tool, _ string) (string, error) {
		switch tool {
		case "list_cast_devices":
			return devicesOut(fakeDev{Name: tvA, Room: roomA, Online: true}), nil
		default:
			return mustJSON(map[string]any{"status": map[string]any{"state": state}}), nil
		}
	}
}

// TestVolume_SetAbsolute 音量调到{N}：阿拉伯与中文数字；越界本地拒绝（不发
// 工具调用）。
func TestVolume_SetAbsolute(t *testing.T) {
	f := &fakeExec{respond: volumeScript(0, "playing")}
	r := newRouter(f)
	handled, events := r.TryHandle(context.Background(), "音量调到60")
	require.True(t, handled)
	assert.Equal(t, float64(60), f.argsOf(t, "cast_volume")["level"])
	assert.Contains(t, finalOf(t, events), "60")

	f2 := &fakeExec{respond: volumeScript(0, "playing")}
	r2 := newRouter(f2)
	handled, _ = r2.TryHandle(context.Background(), "音量调到八十")
	require.True(t, handled)
	assert.Equal(t, float64(80), f2.argsOf(t, "cast_volume")["level"])

	f3 := &fakeExec{respond: volumeScript(0, "playing")}
	r3 := newRouter(f3)
	handled, events3 := r3.TryHandle(context.Background(), "音量调到150")
	require.True(t, handled)
	assert.NotContains(t, f3.toolsCalled(), "cast_volume", "越界音量不得下发")
	assert.Contains(t, finalOf(t, events3), "0")
	assert.Contains(t, finalOf(t, events3), "100")
}

// TestVolume_Relative 大点声 +20 / 小点声 −20：协议 status 无音量读数，以
// 快通道最近一次推算值为基线（未知按 50），并随每次成功调节滚动更新；
// 0/100 边界钳制。
func TestVolume_Relative(t *testing.T) {
	f := &fakeExec{respond: volumeScript(0, "playing")}
	r := newRouter(f)

	handled, _ := r.TryHandle(context.Background(), "大点声")
	require.True(t, handled)
	assert.Equal(t, float64(70), f.argsOf(t, "cast_volume")["level"], "未知基线按 50 起算")

	handled, _ = r.TryHandle(context.Background(), "大点声")
	require.True(t, handled)
	assert.Equal(t, float64(90), f.argsOf(t, "cast_volume")["level"], "基线滚动 +20")

	handled, _ = r.TryHandle(context.Background(), "小点声")
	require.True(t, handled)
	assert.Equal(t, float64(70), f.argsOf(t, "cast_volume")["level"])

	// 绝对设定也更新基线：调到 10 后小点声 → 0 钳制下界。
	handled, _ = r.TryHandle(context.Background(), "音量调到10")
	require.True(t, handled)
	handled, _ = r.TryHandle(context.Background(), "小点声")
	require.True(t, handled)
	assert.Equal(t, float64(0), f.argsOf(t, "cast_volume")["level"])
}

// ---- 状态 ----

// TestStatus_Variants 现在播什么/播到哪了：playing / paused / idle 三种
// 转述（房间名 + 片名 + 第几分钟）。
func TestStatus_Variants(t *testing.T) {
	cases := []struct {
		text     string
		state    string
		title    string
		posMS    int64
		contains []string
	}{
		{"现在播什么", "playing", "三体", 720000, []string{"客厅正在播放《三体》", "第 12 分钟"}},
		{"播到哪了", "paused", "三体", 720000, []string{"客厅已暂停《三体》", "第 12 分钟"}},
		{"在播什么", "idle", "", 0, []string{"客厅当前没有在播内容"}},
	}
	for _, c := range cases {
		f := &fakeExec{respond: func(tool, _ string) (string, error) {
			switch tool {
			case "list_cast_devices":
				return devicesOut(fakeDev{Name: tvA, Room: roomA, Online: true}), nil
			default:
				return statusResp(c.state, c.title, c.posMS), nil
			}
		}}
		r := newRouter(f)

		handled, events := r.TryHandle(context.Background(), c.text)
		require.True(t, handled, c.text)
		for _, want := range c.contains {
			assert.Contains(t, finalOf(t, events), want, c.text)
		}
	}
}

// ---- 兜底 ----

// TestNoMatchFallsThrough 规则外的任何话术 → handled=false 且零工具调用
// （LLM 全权处理，零损失）。
func TestNoMatchFallsThrough(t *testing.T) {
	f := &fakeExec{}
	r := newRouter(f)

	for _, text := range []string{"", "今天天气怎么样", "播放", "帮我把音量调到一半左右", "把夕阳短片投到 no-such 房间", "   "} {
		handled, events := r.TryHandle(context.Background(), text)
		assert.False(t, handled, text)
		assert.Empty(t, events, text)
	}
	assert.Empty(t, f.toolsCalled())
}

// TestLeadingFillerStripped 前缀客套词（请/帮我/麻烦）剥除后仍可命中。
func TestLeadingFillerStripped(t *testing.T) {
	f := &fakeExec{respond: pauseScript(false)}
	r := newRouter(f)

	handled, _ := r.TryHandle(context.Background(), "帮我暂停一下")
	assert.True(t, handled)
	assert.Contains(t, f.toolsCalled(), "cast_pause")
}

// TestTrailingPunctuationTolerated 结尾标点（语音/输入常见）不影响命中。
func TestTrailingPunctuationTolerated(t *testing.T) {
	f := &fakeExec{respond: pauseScript(false)}
	r := newRouter(f)

	handled, _ := r.TryHandle(context.Background(), "暂停。")
	assert.True(t, handled)
}

// TestToolFailureEmitsErrorEvent 非搜索类工具执行失败 → error 事件（中文
// 标签 + 既有契约错误文本），handled=true（不再折返 LLM 重复执行）。
func TestToolFailureEmitsErrorEvent(t *testing.T) {
	f := &fakeExec{respond: func(tool, _ string) (string, error) {
		switch tool {
		case "list_cast_devices":
			return devicesOut(fakeDev{Name: tvA, Room: roomA, Online: true}), nil
		default:
			return "", fmt.Errorf("device_offline: Get http://…: connection refused")
		}
	}}
	r := newRouter(f)

	handled, events := r.TryHandle(context.Background(), "暂停")
	require.True(t, handled)
	assert.True(t, hasErrorEvent(events), "应含 error 事件")
	for _, ev := range events {
		if ev.Type == "error" {
			assert.Contains(t, ev.Text, "device_offline")
		}
	}
}

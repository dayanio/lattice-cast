package discovery

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dayanio/lattice-cast/internal/cast/config"
	"github.com/dayanio/lattice-cast/internal/cast/testsupport/fakerenderer"
)

// staticCfg 返回一台静态渲染端（带 host+port）与一台仅组播渲染端（无 host），
// 用于验证静态条目直通与（隐式）无 host 条目不出现在静态结果中。
func staticCfg() config.Config {
	return config.Config{
		AuthToken:    "tok",
		MediaBaseURL: "http://192.168.1.10:7810",
		Renderers: map[string]config.Renderer{
			"living-room-display": {Room: "living-room", Host: "192.168.1.50", Port: 7900, Token: "tok-a"},
			"bedroom-display":     {Room: "bedroom", Token: "tok-b"}, // 无 host：仅组播发现
		},
	}
}

// TestBrowse_StaticEntries 静态条目直通：配置中带 host 的渲染端无条件并入结果
// （供 CI / 无组播环境），Room 取配置；不带 host 的条目不得凭空出现。
// 本测试不依赖组播可用，-short 模式（CI）必跑必过。
func TestBrowse_StaticEntries(t *testing.T) {
	cfg := staticCfg()

	// 静态路径不依赖组播：把浏览窗口收窄到 250ms 让两种模式下都快
	//（Browse 内部窗口取 ctx 截止与 3s 中较早者）。
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	found, err := Browse(ctx, cfg)
	require.NoError(t, err)
	require.Len(t, found, 1, "只应包含带 host 的静态条目，got %+v", found)

	got := found[0]
	assert.Equal(t, "living-room-display", got.Name)
	assert.Equal(t, "192.168.1.50", got.Host)
	assert.Equal(t, 7900, got.Port)
	assert.Equal(t, "living-room", got.Room, "Room 应来自配置")
}

// TestBrowse_Multicast 端到端组播发现：zeroconf 注册真实 _latticecast._tcp 实例
// （端口借用 Task 5 Fake 渲染端 / 临时 listener），Browse 应：
//  1. 只采纳 mDNS 实例名命中 config.Renderers key 的实例；
//  2. 配置 room 为空时取 TXT room=，配置 room 非空时压过 TXT。
//
// 组播在 CI runner 上不可靠，故 -short 跳过本例。
func TestBrowse_Multicast(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过组播 e2e（CI runner 无可靠组播）")
	}

	// 真实渲染端端口（Task 5 Fake），模拟已就绪的 _latticecast._tcp 服务。
	fake := fakerenderer.New("tok-bedroom")
	defer fake.Close()

	strayLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer strayLn.Close()

	bedReg, err := zeroconf.Register("bedroom-display", "_latticecast._tcp", "local.",
		fake.Target().Port, []string{"room=bedroom", "v=1"}, nil)
	require.NoError(t, err)
	defer bedReg.Shutdown()

	liveReg, err := zeroconf.Register("living-room-display", "_latticecast._tcp", "local.",
		portOf(t, strayLn), []string{"room=wrong-room", "v=1"}, nil)
	require.NoError(t, err)
	defer liveReg.Shutdown()

	// 未在配置注册的实例：必须被过滤。
	strayReg, err := zeroconf.Register("stray-device", "_latticecast._tcp", "local.",
		portOf(t, strayLn), []string{"room=stray", "v=1"}, nil)
	require.NoError(t, err)
	defer strayReg.Shutdown()

	time.Sleep(300 * time.Millisecond) // 等 responder 完成探测/通告，降低首查丢失概率

	cfg := config.Config{
		AuthToken:    "tok",
		MediaBaseURL: "http://192.168.1.10:7810",
		Renderers: map[string]config.Renderer{
			"bedroom-display":     {Room: "", Token: "tok-bedroom"},           // 配置无 room → 取 TXT
			"living-room-display": {Room: "living-room", Token: "tok-living"}, // 配置有 room → 压过 TXT
		},
	}

	found, err := Browse(context.Background(), cfg)
	require.NoError(t, err)

	byName := make(map[string]Found, len(found))
	for _, f := range found {
		byName[f.Name] = f
	}
	assert.Len(t, found, 2, "只应采纳两台已配置实例，got %+v", found)

	bed, ok := byName["bedroom-display"]
	require.True(t, ok, "应发现已配置实例 bedroom-display，got %+v", found)
	assert.Equal(t, fake.Target().Port, bed.Port, "Port 应取自 mDNS SRV 记录")
	assert.Equal(t, "bedroom", bed.Room, "配置 room 为空时应取 TXT room=")
	assert.NotEmpty(t, bed.Host, "Host 应取自 entry.AddrIPv4")

	living, ok := byName["living-room-display"]
	require.True(t, ok, "应发现已配置实例 living-room-display，got %+v", found)
	assert.Equal(t, "living-room", living.Room, "配置 room 非空时应压过 TXT room=")

	_, ok = byName["stray-device"]
	assert.False(t, ok, "未在 config.Renderers 注册的实例必须被过滤")
}

// portOf 返回 listener 的端口（测试辅助）。
func portOf(t *testing.T, ln net.Listener) int {
	t.Helper()
	return ln.Addr().(*net.TCPAddr).Port
}

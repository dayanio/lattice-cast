package renderer

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAnnounce_PortZeroRejected 无网络依赖的参数校验：zeroconf 对 port=0
// 直接报错，Announce 原样上抛（CI 无组播环境同样确定）。
func TestAnnounce_PortZeroRejected(t *testing.T) {
	stop, err := Announce(context.Background(), "x", "y", 0)
	require.Error(t, err)
	assert.Nil(t, stop)
}

// TestAnnounce_Discoverable 组播冒烟：Announce 发布本进程渲染端（真实 HTTP
// 服务端口），zeroconf resolver 应在窗口内发现同名实例，且 TXT 携带
// room= 与 v=1（protocol.md 第三节）。组播在 CI runner 上不可靠，-short 跳过。
func TestAnnounce_Discoverable(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过组播冒烟（CI runner 无可靠组播）")
	}

	// 真实渲染端 HTTP 端口：挂上被测 Handler，发现即意味着可直连。
	ctl := &fakeController{}
	srv := NewServer("tok-smoke", "smoke-room", "renderer-smoke", 0, ctl)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	tsURL, err := url.Parse(ts.URL)
	require.NoError(t, err)
	port, err := net.LookupPort("tcp", tsURL.Port())
	require.NoError(t, err)

	// 实例名带 pid：与机器上可能存在的其他渲染端实例区分。
	name := fmt.Sprintf("renderer-smoke-%d", os.Getpid())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stop, err := Announce(ctx, name, "smoke-room", port)
	require.NoError(t, err)
	t.Cleanup(stop)

	// 组播枚举直到发现同名实例（或 8s 窗口超时）。
	resolver, err := zeroconf.NewResolver(nil)
	require.NoError(t, err)
	entries := make(chan *zeroconf.ServiceEntry)
	browseCtx, cancelBrowse := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancelBrowse()
	require.NoError(t, resolver.Browse(browseCtx, "_latticecast._tcp", "local.", entries))

	for entry := range entries {
		if entry.Instance != name {
			continue
		}
		assert.Equal(t, port, entry.Port, "Port 应取自发布端口（SRV 记录）")
		assert.NotEmpty(t, entry.AddrIPv4, "应携带 IPv4 地址供 cast-agent 寻址")
		assert.Contains(t, entry.Text, "room=smoke-room")
		assert.Contains(t, entry.Text, "v=1")
		return
	}
	t.Fatal("8s 窗口内未发现本进程发布的 _latticecast._tcp 实例")
}

// TestAnnounce_StopTwiceSafe stop 必须幂等（main 停机路径 + ctx 收尾各调一次）。
func TestAnnounce_StopTwiceSafe(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过组播冒烟（CI runner 无可靠组播）")
	}

	stop, err := Announce(context.Background(), "renderer-twice", "room", 7822)
	require.NoError(t, err)
	stop()
	stop() // 第二次不得 panic
}

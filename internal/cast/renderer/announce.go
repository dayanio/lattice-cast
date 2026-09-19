package renderer

import (
	"context"
	"fmt"
	"sync"

	"github.com/grandcat/zeroconf"
)

// mDNS 发布约定（protocol.md 第三节）：服务类型 _latticecast._tcp，
// 实例名 = 设备显示名（cast-agent 按 config.Renderers key 采纳），
// TXT 携带房间与协议版本。
const (
	mdnsService = "_latticecast._tcp"
	mdnsDomain  = "local."
	txtRoomFmt  = "room=%s"
	txtVersion  = "v=1"
)

// Announce 经 mDNS 发布本渲染端：实例名 name、服务 _latticecast._tcp、
// 端口 port，TXT 记录 room=<room> 与 v=1。返回的 stop 撤销发布（幂等，
// 可安全多次调用）；ctx 结束时同样自动撤销发布。
//
// 实现说明：用 zeroconf.Register 而非同类 RegisterProxy —— Register 自动
// 取本机主机名与组播接口 IPv4，正是"发布自己"的语义；RegisterProxy 需
// 调用方手工提供 host+ips，用于代理他人服务，两者线上通告等价。
func Announce(ctx context.Context, name, room string, port int) (stop func(), err error) {
	server, err := zeroconf.Register(name, mdnsService, mdnsDomain, port,
		[]string{fmt.Sprintf(txtRoomFmt, room), txtVersion}, nil)
	if err != nil {
		return nil, fmt.Errorf("mdns register %s %s:%d: %w", name, mdnsService, port, err)
	}

	var once sync.Once
	stop = func() { once.Do(server.Shutdown) }
	if done := ctx.Done(); done != nil { // ctx 不可取消（如 Background）时不挂 goroutine
		go func() {
			<-done
			stop()
		}()
	}
	return stop, nil
}

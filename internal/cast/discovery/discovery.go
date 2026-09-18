// Package discovery 通过 mDNS 发现局域网内的 LatticeCast 渲染端并与配置合并：
//
//   - 浏览 _latticecast._tcp（docs/protocol.md §三），仅采纳 mDNS 实例名
//     命中 config.Renderers key 的实例（实例名即设备身份）；
//   - Room 取自 TXT room=，配置中该渲染端的 Room 非空时优先（配置归位为准）；
//   - 配置中带 Host 的条目无条件并入结果（静态覆盖，供 CI / 无组播环境）。
//
// token 不进入发现结果：token 留在 config.Renderers 中，由 Manager 合并。
package discovery

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"

	"github.com/dayanio/lattice-cast/internal/cast/config"
)

// 协议约定常量：服务类型 / 域 / TXT 房间键；单次枚举窗口 3 秒。
const (
	serviceType  = "_latticecast._tcp"
	domain       = "local."
	txtRoomKey   = "room="
	browseWindow = 3 * time.Second
)

// Found 是一次发现得到的一台渲染端地址。
type Found struct {
	Name string // mDNS 实例名（设备身份，须命中 config.Renderers 的 key 才采纳）
	Host string
	Port int
	Room string // TXT room=，配置优先
}

// Browse 阻塞式枚举一次当前可见渲染端（内部 3 秒窗口，ctx 更早截止时以 ctx 为准）；
// cfg 中带 host 的条目无条件并入结果（静态覆盖，供 CI/无组播环境）。
// 组播子系统不可用（resolver 初始化或查询启动失败）不视为致命错误：
// 降级为仅返回静态条目。结果按 Name 排序，保证确定性；Name 唯一
// （静态覆盖优先，重复 mDNS 通告去重）。
func Browse(ctx context.Context, cfg config.Config) ([]Found, error) {
	// 静态条目先行：带 host 的配置条目按 key 有序并入。
	seen := make(map[string]bool, len(cfg.Renderers))
	var found []Found
	for _, name := range sortedKeys(cfg.Renderers) {
		r := cfg.Renderers[name]
		if r.Host == "" {
			continue // 无 host：仅组播发现
		}
		seen[name] = true
		found = append(found, Found{Name: name, Host: r.Host, Port: r.Port, Room: r.Room})
	}

	// 组播枚举：窗口取 3 秒与 ctx 剩余时间的较早者；
	// ctx 截止时 zeroconf 关闭 entries 通道，range 自然结束。
	browseCtx, cancel := context.WithTimeout(ctx, browseWindow)
	defer cancel()

	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return found, nil // 无组播环境：静态覆盖仍有效，降级返回
	}
	entries := make(chan *zeroconf.ServiceEntry)
	if err := resolver.Browse(browseCtx, serviceType, domain, entries); err != nil {
		return found, nil // 同上
	}

	for entry := range entries {
		name := entry.Instance
		if name == "" || seen[name] {
			continue // 已并入（静态或重复通告）
		}
		r, ok := cfg.Renderers[name]
		if !ok {
			continue // 未在配置注册的实例：忽略
		}
		if len(entry.AddrIPv4) == 0 {
			continue // 无 IPv4 地址无法寻址（v1 不解析 IPv6）
		}
		seen[name] = true
		room := r.Room
		if room == "" {
			room = txtValue(entry.Text, txtRoomKey)
		}
		found = append(found, Found{
			Name: name,
			Host: entry.AddrIPv4[0].String(),
			Port: entry.Port,
			Room: room,
		})
	}

	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found, nil
}

// txtValue 从 DNS-SD TXT 记录（"key=value" 列表）取 key 的值；缺失返回空串。
func txtValue(text []string, key string) string {
	for _, kv := range text {
		if v, ok := strings.CutPrefix(kv, key); ok {
			return v
		}
	}
	return ""
}

// sortedKeys 返回渲染端映射的有序 key 列表（结果确定性）。
func sortedKeys(m map[string]config.Renderer) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

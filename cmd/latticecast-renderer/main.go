// Command latticecast-renderer 是 LatticeCast 渲染端（macOS/Linux）的可执行
// 入口：装配 mpv 播放后端、协议 HTTP 服务与 mDNS 发布，阻塞运行至
// SIGINT/SIGTERM 后按 mDNS 发布 → HTTP → mpv 的顺序优雅停机。
//
// 用法：
//
//	latticecast-renderer -name 卧室电视 -room 客厅 -token <shared-secret> [-port 7822] [-mpv mpv]
//
// 依赖 mpv（JSON IPC 控制体）：不在 PATH 时启动即退出并给出安装提示。
// 停机顺序：先撤销 mDNS 发布（不再被 cast-agent 发现），再停 HTTP（等在途
// 请求），最后杀 mpv 进程并清理其 IPC socket。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dayanio/lattice-cast/internal/cast/renderer"
)

// shutdownTimeout 优雅停机等待 HTTP 在途请求完成的上限。
const shutdownTimeout = 5 * time.Second

func main() {
	var (
		name   = flag.String("name", "", "mDNS 实例名（设备身份，须命中 cast-agent 配置 key，必填）")
		room   = flag.String("room", "", "房间名（mDNS TXT room=，必填）")
		token  = flag.String("token", "", "Bearer token（与 cast-agent 配置一致，必填）")
		port   = flag.Int("port", 7822, "HTTP 监听端口")
		mpvBin = flag.String("mpv", "mpv", "mpv 可执行文件（缺省从 PATH 查找）")
	)
	flag.Parse()

	if *name == "" || *room == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "missing required flags: -name, -room, -token are required")
		flag.Usage()
		os.Exit(2)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := run(*name, *room, *token, *port, *mpvBin); err != nil {
		slog.Error("latticecast-renderer exited with error", "err", err)
		os.Exit(1)
	}
}

// run 按依赖顺序装配并阻塞运行：MpvController（拉起常驻 mpv）→ Server →
// TCP 监听 → Announce（mDNS）→ http.Server。任一步失败：关闭已启动组件后
// 返回错误（main 以非零码退出）；mpv 缺失的错误自带安装提示。
func run(name, room, token string, port int, mpvBin string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctl, err := renderer.NewMpvController(mpvBin)
	if err != nil {
		return err
	}
	defer func() {
		if err := ctl.Close(); err != nil {
			slog.Warn("mpv shutdown incomplete", "err", err)
		}
	}()

	srv := renderer.NewServer(token, room, name, port, ctl)

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port)) // 同步绑定：端口占用即刻致命
	if err != nil {
		return fmt.Errorf("listen :%d: %w", port, err)
	}

	announceCtx, cancelAnnounce := context.WithCancel(context.Background())
	defer cancelAnnounce()
	announceStop, err := renderer.Announce(announceCtx, name, room, ln.Addr().(*net.TCPAddr).Port)
	if err != nil {
		return fmt.Errorf("announce: %w", err)
	}

	httpSrv := &http.Server{Handler: srv.Handler()}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	slog.Info("latticecast-renderer started", "name", name, "room", room, "port", port)

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			break // 已在停机路径中，落到下方统一收尾
		}
		announceStop()
		return fmt.Errorf("serve :%d: %w", port, err)
	case <-ctx.Done():
	}

	// 优雅停机：先撤销 mDNS 发布，再停 HTTP（等在途请求），mpv 由 defer 收尾。
	announceStop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http server shutdown incomplete", "err", err)
	}
	slog.Info("latticecast-renderer stopped")
	return nil
}

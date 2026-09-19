// Command lattice-cast 是 cast-agent 的可执行入口：装配配置、审计日志、
// 媒体库与媒体服务、设备管理器、MCP 工具层，并阻塞运行至 SIGINT/SIGTERM。
//
// 用法：
//
//	lattice-cast -config config.yaml
//	lattice-cast -version
//
// 审计日志固定写在工作目录 lattice-cast.audit.jsonl（v1 不暴露配置项）。
// 停机顺序：先停 MCP HTTP 服务（等在途请求，至多 5 秒），再停媒体服务，
// 最后关闭审计文件。
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

	"github.com/dayanio/lattice-cast/internal/cast/config"
	"github.com/dayanio/lattice-cast/internal/cast/manager"
	"github.com/dayanio/lattice-cast/internal/cast/mcpserver"
	"github.com/dayanio/lattice-cast/internal/cast/resolve"
)

// Version 版本号；发布时经 -ldflags "-X main.Version=v0.1.0" 覆写。
var Version = "dev"

// auditFileName 审计文件名，固定写于进程工作目录。
const auditFileName = "lattice-cast.audit.jsonl"

// shutdownTimeout 优雅停机时等待 MCP 在途请求完成的上限。
const shutdownTimeout = 5 * time.Second

func main() {
	configPath := flag.String("config", "config.yaml",
		"YAML 配置文件路径（审计日志固定写在工作目录 "+auditFileName+"）")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if *showVersion {
		fmt.Printf("lattice-cast %s\n", Version)
		return
	}

	if err := run(*configPath); err != nil {
		slog.Error("lattice-cast exited with error", "err", err)
		os.Exit(1)
	}
}

// run 按依赖顺序装配各组件并阻塞运行：
//
//	config.Load → OpenAudit → Library.Rescan（失败仅告警，以空库继续）→
//	MediaServer.Start → Resolver → manager.New → mcpserver.New → http.Server。
//
// 任一步失败：先关闭已启动的组件，再返回错误（main 以非零码退出）。
func run(configPath string) error {
	// 信号上下文先于装配建立：启动期间收到信号同样走优雅停机路径。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	audit, err := manager.OpenAudit(auditFileName)
	if err != nil {
		return err
	}

	lib := resolve.NewLibrary(cfg.MediaLibrary)
	if err := lib.Rescan(); err != nil {
		// 库扫描失败不致命：以空库启动，URL 直链播放仍可用。
		slog.Warn("media library scan failed, starting empty", "err", err)
	}

	media := resolve.NewMediaServer(lib, cfg.MediaListen)
	if err := media.Start(); err != nil {
		_ = audit.Close()
		return fmt.Errorf("media server listen %s: %w", cfg.MediaListen, err)
	}

	var ext resolve.Extractor
	if cfg.YtDlp != "" {
		ext = resolve.NewYtDlp(cfg.YtDlp)
	}
	res := &resolve.Resolver{Lib: lib, Base: cfg.MediaBaseURL, Ext: ext}

	mgr := manager.New(cfg, lib, res)
	srv := mcpserver.New(mgr, lib, res, audit, mcpserver.StaticToken(cfg.AuthToken))

	httpSrv := &http.Server{Handler: srv.HTTP()}
	ln, err := net.Listen("tcp", cfg.MCPListen) // 同步绑定：端口占用即刻致命
	if err != nil {
		_ = media.Close()
		_ = audit.Close()
		return fmt.Errorf("mcp listen %s: %w", cfg.MCPListen, err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	slog.Info("lattice-cast started",
		"version", Version, "mcp", cfg.MCPListen, "media", cfg.MediaListen)

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			break // 已在停机路径中，落到下方统一收尾
		}
		_ = media.Close()
		_ = audit.Close()
		return fmt.Errorf("mcp serve %s: %w", cfg.MCPListen, err)
	case <-ctx.Done():
	}

	// 优雅停机：先停 MCP（等在途请求），再停媒体服务，最后关闭审计。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("mcp server shutdown incomplete", "err", err)
	}
	if err := media.Close(); err != nil {
		slog.Warn("media server close", "err", err)
	}
	if err := audit.Close(); err != nil {
		slog.Warn("audit close", "err", err)
	}
	slog.Info("lattice-cast stopped")
	return nil
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"w103d/gateway/internal/app"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
	"w103d/gateway/internal/runtimeenv"
)

// Only file-configured live startup prepares the environment. Explicit legacy
// CLI/systemd deployments already manage their proxy, and dry-run stays inert.
func preparesRuntime(options app.Options) bool {
	return options.RequireInitialKey && !options.DryRun
}

func preflightRuntime(options app.Options) error {
	if runtime.GOOS != "linux" {
		return errors.New("自动准备运行环境需要 Linux；本机开发请使用 --dry-run 和独立数据目录")
	}
	if os.Geteuid() != 0 {
		return errors.New("请使用 sudo 启动程序，以便安装缺少的依赖并管理本项目的 Nginx")
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return fmt.Errorf("无法读取本机网络地址，未安装依赖：%w", err)
	}
	if !hasGatewayAddress(options.GatewayIP, addresses) {
		return errors.New("config.json 中的 gatewayIp 尚未分配给本机；请先完成 EasyTier 或其他 VPN 组网，程序不会自动创建 IP")
	}
	listener, err := net.Listen("tcp4", options.Listen)
	if err != nil {
		return fmt.Errorf("后台监听地址或端口不可用，未安装依赖：%w", err)
	}
	return listener.Close()
}

func hasGatewayAddress(gateway string, addresses []net.Addr) bool {
	want := net.ParseIP(gateway)
	if want == nil || want.To4() == nil {
		return false
	}
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && want.Equal(ip) {
			return true
		}
	}
	return false
}

type managementServer interface {
	ServeContext(context.Context) error
}

type runningProxy interface {
	Owned() bool
	Done() <-chan error
	Stop(context.Context) error
}

// Package-private seams keep orchestration tests away from APT and processes.
type runtimeOperations struct {
	applied      func(app.Options) (model.State, error)
	dependencies func(context.Context, app.Options, io.Writer) error
	proxy        func(context.Context, app.Options, model.State) (runningProxy, error)
}

func liveRuntimeOperations() runtimeOperations {
	return runtimeOperations{
		applied: app.AppliedState,
		dependencies: func(ctx context.Context, options app.Options, output io.Writer) error {
			return runtimeenv.Ensure(ctx, runtimeenv.Options{NginxBin: options.Nginx}, output)
		},
		proxy: func(ctx context.Context, options app.Options, applied model.State) (runningProxy, error) {
			proxy := &engine.Engine{DataDir: options.DataDir, CacheDir: options.CacheDir, NginxBin: options.Nginx,
				ConfigPath: filepath.Join(options.DataDir, "managed-nginx.conf")}
			return proxy.EnsureRunning(ctx, applied, applied.AppliedPublicAddress)
		},
	}
}

func serveWithRuntime(ctx context.Context, options app.Options, server managementServer, out io.Writer, operations runtimeOperations) (result error) {
	if !preparesRuntime(options) {
		return server.ServeContext(ctx)
	}
	if out == nil {
		out = io.Discard
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Reject unreadable applied state before package installation. The saved
	// draft is never an input to first-start proxy configuration.
	applied, err := operations.applied(options)
	if err != nil {
		return fmt.Errorf("无法读取已应用配置，未准备运行环境：%w", err)
	}
	fmt.Fprintln(out, "正在检查运行依赖……")
	if err := operations.dependencies(ctx, options, out); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fmt.Fprintln(out, "正在准备本项目的独立 Nginx……")
	proxy, err := operations.proxy(ctx, options, applied)
	if err != nil {
		return fmt.Errorf("本项目的 Nginx 未就绪，后台未启动：%w", err)
	}
	defer func() {
		if proxy.Owned() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			result = errors.Join(result, proxy.Stop(stopCtx))
		}
	}()
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- server.ServeContext(serveCtx) }()
	fmt.Fprintf(out, "运行依赖及代理校验通过，管理后台地址：http://%s/\n", options.Listen)
	select {
	case err := <-finished:
		return err
	case err := <-proxy.Done():
		cancel()
		serverErr := <-finished
		if ctx.Err() != nil {
			return serverErr
		}
		if err == nil {
			err = errors.New("Nginx 进程已退出")
		}
		return errors.Join(fmt.Errorf("受管代理意外退出，已停止管理服务：%w", err), serverErr)
	}
}

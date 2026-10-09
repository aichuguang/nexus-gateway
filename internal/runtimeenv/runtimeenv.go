// Package runtimeenv checks and, on APT-based Linux hosts, prepares the runtime
// dependencies. It does not configure VPNs, firewall rules or application services.
package runtimeenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	modulePath = "/usr/lib/nginx/modules/ngx_http_js_module.so"
	caPath     = "/etc/ssl/certs/ca-certificates.crt"
)

type Options struct {
	NginxBin string
}

type commandRunner func(context.Context, string, []string, io.Writer) (string, error)

// host is an internal test seam: tests use a temporary filesystem and fake
// commands, never the host package manager, accounts or service manager.
type host struct {
	goos         string
	root         string
	tempDir      string
	euid         func() int
	lookPath     func(string) (string, error)
	lookupWorker func() (*user.User, error)
	runningNginx func() (bool, error)
	canExecute   func(os.FileInfo) bool
	run          commandRunner
}

func systemHost() host {
	return host{
		goos: runtime.GOOS, root: "/", euid: os.Geteuid,
		lookPath: executablePath,
		lookupWorker: func() (*user.User, error) {
			return user.Lookup("www-data")
		},
		runningNginx: func() (bool, error) { return nginxRunning("/proc") },
		canExecute:   func(info os.FileInfo) bool { return info.Mode().Perm()&0111 != 0 },
		run:          runCommand,
	}
}

func (h host) path(path string) string {
	return filepath.Join(h.root, filepath.FromSlash(strings.TrimPrefix(path, "/")))
}

// Ensure installs only missing dependencies. Existing Nginx instances, service
// configuration, masks and enablement are never taken over or changed.
func Ensure(ctx context.Context, options Options, out io.Writer) error {
	return ensure(ctx, options, out, systemHost())
}

func ensure(ctx context.Context, options Options, out io.Writer, h host) (result error) {
	if out == nil {
		out = io.Discard
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.goos != "linux" {
		return fmt.Errorf("运行依赖检查仅支持 Linux；本机为 %s", h.goos)
	}
	if options.NginxBin == "" {
		options.NginxBin = "/usr/sbin/nginx"
	}
	missing, err := inspect(h, options)
	if err != nil {
		return err
	}
	if len(missing.packages) == 0 {
		return checkModule(ctx, h, options.NginxBin)
	}
	description := strings.Join(missing.labels, ", ")
	if h.euid() != 0 {
		return fmt.Errorf("缺少运行依赖：%s；请使用 sudo 重新运行程序以自动安装", description)
	}
	apt, err := h.lookPath("apt-get")
	if err != nil {
		return fmt.Errorf("缺少运行依赖：%s；自动安装仅支持 APT，请手动安装兼容依赖后重试", description)
	}
	if missing.nginxBinary && options.NginxBin != "nginx" && filepath.ToSlash(options.NginxBin) != "/usr/sbin/nginx" {
		return fmt.Errorf("配置的 Nginx 路径 %q 不存在，APT 不会安装到自定义路径；请修正 --nginx 或手动安装", options.NginxBin)
	}
	if missing.touchesNginx() {
		if err = requireStoppedNginx(h); err != nil {
			return err
		}
	}

	// APT normally starts/enables nginx's default site. Suppress service actions
	// before invoking APT; cleanup restores only state owned by this invocation.
	guard, err := protectPackageServices(h)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, guard.close()) }()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	fmt.Fprintf(out, "缺少运行依赖：%s。正在通过 APT 安装缺少的软件包；不会启动系统默认 Nginx 站点。\n", description)
	if _, err = h.run(ctx, apt, aptArgs("update", nil), out); err != nil {
		return fmt.Errorf("APT 索引更新失败，未继续安装；请检查网络、软件源及 APT 锁后重试：%w", err)
	}
	plan, err := h.run(ctx, apt, aptArgs("simulate", missing.packages), io.Discard)
	if err != nil {
		return fmt.Errorf("APT 无法解析依赖；请检查软件源和 Nginx/njs 版本：%w", err)
	}
	if err = validatePlan(plan); err != nil {
		return err
	}
	if missing.touchesNginx() {
		if err = requireStoppedNginx(h); err != nil {
			return err
		}
	}
	if err = guard.verifyPolicy(); err != nil {
		return err
	}
	if _, err = h.run(ctx, apt, aptArgs("install", missing.packages), out); err != nil {
		return fmt.Errorf("运行依赖安装失败或已取消，程序不会继续启动；请检查 APT 输出，若 dpkg 被中断需先手动修复后重试：%w", err)
	}
	remaining, err := inspect(h, options)
	if err != nil {
		return err
	}
	if len(remaining.packages) != 0 {
		return fmt.Errorf("APT 返回成功但依赖复验未通过，仍缺少：%s；请修复对应安装或 www-data 账户后重试", strings.Join(remaining.labels, ", "))
	}
	if missing.touchesNginx() {
		if err = requireStoppedNginx(h); err != nil {
			return fmt.Errorf("安装后发现意外的 Nginx 进程；未停止该进程，请检查其归属：%w", err)
		}
	}
	if err = checkModule(ctx, h, options.NginxBin); err != nil {
		return err
	}
	if err = guard.close(); err != nil {
		return err
	}
	fmt.Fprintln(out, "运行依赖安装及复验完成。")
	return nil
}

type missingDependencies struct {
	packages    []string
	labels      []string
	nginxBinary bool
}

func (m *missingDependencies) add(pkg, label string) {
	m.labels = append(m.labels, label)
	for _, existing := range m.packages {
		if existing == pkg {
			return
		}
	}
	m.packages = append(m.packages, pkg)
}

func (m missingDependencies) touchesNginx() bool {
	for _, pkg := range m.packages {
		if pkg == "nginx" || pkg == "libnginx-mod-http-js" {
			return true
		}
	}
	return false
}

func inspect(h host, options Options) (missingDependencies, error) {
	var missing missingDependencies
	if _, err := h.lookPath(options.NginxBin); err != nil {
		missing.add("nginx", "nginx")
		missing.nginxBinary = true
	}
	for _, file := range []struct{ path, pkg string }{{modulePath, "libnginx-mod-http-js"}, {caPath, "ca-certificates"}} {
		info, err := os.Stat(h.path(file.path))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return missing, fmt.Errorf("无法检查依赖文件 %s：%w", file.path, err)
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			missing.add(file.pkg, file.pkg)
		}
	}
	for _, dep := range []struct {
		pkg      string
		commands []string
	}{
		{"iproute2", []string{"ip", "ss"}},
		{"procps", []string{"pgrep"}},
		{"iputils-ping", []string{"ping"}},
		{"logrotate", []string{"logrotate"}},
		{"python3", []string{"python3"}},
		{"curl", []string{"curl"}},
		{"tar", []string{"tar"}},
	} {
		for _, command := range dep.commands {
			if _, err := h.lookPath(command); err != nil {
				missing.add(dep.pkg, dep.pkg)
				break
			}
		}
	}
	worker, err := h.lookupWorker()
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		missing.add("nginx", "www-data 账户")
	} else if err != nil {
		return missing, fmt.Errorf("无法检查 www-data 账户：%w", err)
	} else if worker == nil {
		return missing, fmt.Errorf("www-data 账户查询返回空结果")
	} else {
		uid, uidErr := strconv.Atoi(worker.Uid)
		gid, gidErr := strconv.Atoi(worker.Gid)
		if uidErr != nil || gidErr != nil || uid <= 0 || gid <= 0 {
			return missing, fmt.Errorf("www-data 必须是有效的非 root 系统账户；程序不会修改现有账户")
		}
	}
	return missing, nil
}

func requireStoppedNginx(h host) error {
	running, err := h.runningNginx()
	if err != nil {
		return fmt.Errorf("无法确认现有 Nginx 状态，未安装 Nginx/njs：%w", err)
	}
	if running {
		return fmt.Errorf("需要安装 Nginx/njs，但检测到现有 Nginx 正在运行；请先确认其用途并由管理员安排依赖安装，程序不会停止、升级或接管现有站点")
	}
	return nil
}

func aptArgs(action string, packages []string) []string {
	args := []string{"-o", "DPkg::Lock::Timeout=30", "-o", "Acquire::Retries=1", "-o", "Acquire::http::Timeout=30", "-o", "Acquire::https::Timeout=30"}
	if action == "update" {
		return append(args, "-o", "APT::Update::Error-Mode=any", "update")
	}
	args = append(args, "--no-install-recommends", "--no-upgrade", "--no-remove")
	if action == "simulate" {
		args = append(args, "--simulate")
	} else {
		args = append(args, "-y", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold")
	}
	args = append(args, "install")
	return append(args, packages...)
}

// --no-upgrade alone only protects explicitly requested packages. Reject a
// transaction that would upgrade dependencies or configure previously pending
// packages as well, instead of repairing somebody else's host implicitly.
func validatePlan(plan string) error {
	newPackages := make(map[string]bool)
	lines := strings.Split(plan, "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "Remv" || (fields[0] == "Inst" && len(fields) > 2 && strings.HasPrefix(fields[2], "[")) {
			return fmt.Errorf("APT 计划升级或删除已有软件包 %s；自动安装已停止，请管理员先处理版本兼容后重试", fields[1])
		}
		if fields[0] == "Inst" {
			newPackages[fields[1]] = true
		}
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "Conf" && !newPackages[fields[1]] {
			return fmt.Errorf("APT 存在待修复的软件包 %s；请管理员先完成 dpkg 修复后重试", fields[1])
		}
	}
	return nil
}

func checkModule(ctx context.Context, h host, nginx string) error {
	dir, err := os.MkdirTemp(h.tempDir, "gateway-dependency-check-")
	if err != nil {
		return fmt.Errorf("无法创建 Nginx/njs 检查目录：%w", err)
	}
	defer os.RemoveAll(dir)
	// No http{} block: loading the module verifies its ABI without initializing
	// any compiled-in HTTP cache, temporary or access-log paths.
	config := fmt.Sprintf("load_module %q;\nerror_log stderr;\npid %q;\nevents {}\n", filepath.ToSlash(h.path(modulePath)), filepath.ToSlash(filepath.Join(dir, "nginx.pid")))
	path := filepath.Join(dir, "check.conf")
	if err = os.WriteFile(path, []byte(config), 0600); err != nil {
		return err
	}
	bin, err := h.lookPath(nginx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := h.run(ctx, bin, []string{"-t", "-p", dir + string(filepath.Separator), "-c", path}, io.Discard)
	if err != nil {
		if len(output) > 4096 {
			output = output[len(output)-4096:]
		}
		return fmt.Errorf("Nginx/njs 兼容性检查失败；请安装与配置的 Nginx 匹配的 libnginx-mod-http-js，未启动任何代理：%w；%s", err, strings.TrimSpace(output))
	}
	return nil
}

func executablePath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err == nil || strings.ContainsAny(name, "/\\") {
		return path, err
	}
	for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		if path, err = exec.LookPath(filepath.Join(dir, name)); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("executable %s not found", name)
}

func runCommand(ctx context.Context, binary string, args []string, out io.Writer) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive", "DEBCONF_NONINTERACTIVE_SEEN=true", "NEEDRESTART_MODE=l", "UCF_FORCE_CONFFOLD=1", "PATH=/usr/sbin:/usr/bin:/sbin:/bin")
	configureCommand(cmd)
	var output bytes.Buffer
	cmd.Stdout = io.MultiWriter(&output, out)
	cmd.Stderr = cmd.Stdout
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return output.String(), err
}

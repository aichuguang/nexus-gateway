package runtimeenv

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const temporaryPolicy = "#!/bin/sh\n# Temporary gateway dependency-install policy.\nexit 101\n"

type serviceGuard struct {
	host       host
	policyPath string
	policyInfo os.FileInfo
	policyData []byte
	owned      bool
	bootLinks  map[string]bool
	closed     bool
}

func protectPackageServices(h host) (*serviceGuard, error) {
	before, err := h.nginxBootLinks()
	if err != nil {
		return nil, err
	}
	g := &serviceGuard{host: h, policyPath: h.path("/usr/sbin/policy-rc.d"), bootLinks: before}
	file, err := os.OpenFile(g.policyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if errors.Is(err, os.ErrExist) {
		// Container images commonly carry this exact deny-all policy. Reuse a
		// verifiable constant policy, but never execute, overwrite or delete an
		// administrator's unknown policy script.
		g.policyInfo, err = os.Lstat(g.policyPath)
		if err != nil {
			return nil, err
		}
		if !g.policyInfo.Mode().IsRegular() || !h.canExecute(g.policyInfo) || g.policyInfo.Size() > 4096 {
			return nil, fmt.Errorf("已有 %s 不是可安全复用的常量脚本；未覆盖该文件。请管理员手动安装缺少的软件包后重试", g.policyPath)
		}
		g.policyData, err = readPolicy(g.policyPath)
		if err != nil || !denyAllPolicy(g.policyData) {
			return nil, fmt.Errorf("已有 %s 无法安全复用；未覆盖该文件。请管理员手动安装缺少的软件包后重试", g.policyPath)
		}
		if string(g.policyData) == temporaryPolicy {
			return nil, fmt.Errorf("检测到另一个 Gateway 依赖安装的临时保护文件，请等待该进程结束后重试；若进程已异常退出，请管理员检查 %s，程序不会擅自删除", g.policyPath)
		}
		return g, nil
	}
	if err != nil {
		return nil, fmt.Errorf("无法安全阻止软件包自动启动默认 Nginx，未安装依赖：%w", err)
	}
	g.owned = true
	g.policyInfo, err = file.Stat()
	if err == nil {
		_, err = file.WriteString(temporaryPolicy)
	}
	g.policyData = []byte(temporaryPolicy)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		// The file was created exclusively by us, but still verify its identity
		// before cleanup in case an administrator replaced it meanwhile.
		return nil, errors.Join(err, closeErr, g.removePolicy(false))
	}
	return g, nil
}

func denyAllPolicy(data []byte) bool {
	if strings.ContainsAny(string(data), "\r\x00") {
		return false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 || (lines[0] != "#!/bin/sh" && lines[0] != "#!/bin/bash") {
		return false
	}
	exits := 0
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line != "exit 101" {
			return false
		}
		exits++
	}
	return exits == 1
}

func (g *serviceGuard) verifyPolicy() error {
	info, err := os.Lstat(g.policyPath)
	if err != nil || g.policyInfo == nil || !os.SameFile(g.policyInfo, info) || !info.Mode().IsRegular() || !g.host.canExecute(info) {
		return fmt.Errorf("临时服务启动保护文件已被替换或无法读取，停止自动安装")
	}
	data, err := readPolicy(g.policyPath)
	if err != nil || string(data) != string(g.policyData) {
		return fmt.Errorf("服务启动保护文件已被修改，停止自动安装并保留修改后的文件")
	}
	return nil
}

func readPolicy(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return nil, fmt.Errorf("服务启动保护必须是长度不超过 4096 字节的普通脚本")
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if len(data) > 4096 {
		return nil, fmt.Errorf("服务启动保护文件在读取期间被修改")
	}
	return data, err
}

func (g *serviceGuard) removePolicy(checkContents bool) error {
	if !g.owned {
		return nil
	}
	info, err := os.Lstat(g.policyPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || g.policyInfo == nil || !os.SameFile(g.policyInfo, info) {
		return fmt.Errorf("服务启动保护文件已被替换，未删除 %s", g.policyPath)
	}
	if checkContents {
		if err = g.verifyPolicy(); err != nil {
			return err
		}
	}
	if err = os.Remove(g.policyPath); err != nil {
		return fmt.Errorf("无法清理本次创建的 %s，请管理员检查：%w", g.policyPath, err)
	}
	return nil
}

func (g *serviceGuard) close() error {
	if g.closed {
		return nil
	}
	g.closed = true
	// policy-rc.d prevents starts, not enablement. Remove only new, narrowly
	// identified nginx startup symlinks created during this transaction. This
	// also works without a running systemd and preserves all pre-existing masks,
	// units and enablement links. No service stop/reload/disable command is used.
	links, err := g.host.nginxBootLinks()
	if err == nil {
		for path := range links {
			if g.bootLinks[path] {
				continue
			}
			if removeErr := removeNewBootLink(g.host, path); removeErr != nil {
				err = errors.Join(err, removeErr)
			}
		}
	}
	return errors.Join(err, g.removePolicy(true))
}

func (h host) nginxBootLinks() (map[string]bool, error) {
	links := make(map[string]bool)
	for _, root := range []string{"/etc/systemd/system", "/run/systemd/system"} {
		entries, err := os.ReadDir(h.path(root))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".wants") && !strings.HasSuffix(entry.Name(), ".requires") {
				continue
			}
			path := filepath.Join(h.path(root), entry.Name(), "nginx.service")
			if _, err = os.Lstat(path); err == nil {
				links[path] = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	for _, level := range []string{"0", "1", "2", "3", "4", "5", "6", "S"} {
		dir := h.path("/etc/rc" + level + ".d")
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "S") && strings.HasSuffix(entry.Name(), "nginx") {
				links[filepath.Join(dir, entry.Name())] = true
			}
		}
	}
	return links, nil
}

func removeNewBootLink(h host, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("安装期间出现非符号链接的 Nginx 启动项 %s，未修改；请管理员检查", path)
	}
	target, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	allowed := false
	for _, expected := range []string{"/lib/systemd/system/nginx.service", "/usr/lib/systemd/system/nginx.service", "/etc/systemd/system/nginx.service", "/run/systemd/system/nginx.service", "/etc/init.d/nginx"} {
		if filepath.Clean(target) == h.path(expected) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("安装期间出现目标不明的 Nginx 启动链接 %s，未修改；请管理员检查", path)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return fmt.Errorf("Nginx 启动链接在检查期间被替换，未修改 %s", path)
	}
	return os.Remove(path)
}

func nginxRunning(procDir string) (bool, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, err = strconv.Atoi(entry.Name()); err != nil || !entry.IsDir() {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(procDir, entry.Name(), "comm"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(string(comm)) == "nginx" {
			return true, nil
		}
		cmdline, err := os.ReadFile(filepath.Join(procDir, entry.Name(), "cmdline"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		first := strings.SplitN(string(cmdline), "\x00", 2)[0]
		if strings.HasPrefix(first, "nginx: ") || filepath.Base(first) == "nginx" {
			return true, nil
		}
	}
	return false, nil
}

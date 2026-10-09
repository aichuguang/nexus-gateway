package app

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"w103d/gateway/internal/engine"
)

func command(ctx context.Context, name string, args ...string) string {
	child, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, e := exec.CommandContext(child, name, args...).CombinedOutput()
	if e != nil {
		return "不可用：" + e.Error()
	}
	if len(b) > 32768 {
		b = b[:32768]
	}
	return strings.TrimSpace(string(b))
}

func (a *App) diagnostics(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	s := clone(a.state)
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	hostname, _ := os.Hostname()
	system := map[string]string{"hostname": hostname, "arch": runtime.GOARCH}
	var wg sync.WaitGroup
	var mu sync.Mutex
	cmds := map[string][]string{"uptime": {"uptime"}, "memory": {"free", "-h"}, "disk": {"df", "-h", a.opts.DataDir}, "interfaces": {"ip", "-br", "addr"}}
	for key, args := range cmds {
		wg.Add(1)
		go func(k string, v []string) {
			defer wg.Done()
			out := command(ctx, v[0], v[1:]...)
			mu.Lock()
			system[k] = out
			mu.Unlock()
		}(key, args)
	}
	node, peers := "", ""
	wg.Add(2)
	go func() { defer wg.Done(); node = command(ctx, "/opt/easytier/easytier-cli", "node") }()
	go func() { defer wg.Done(); peers = command(ctx, "/opt/easytier/easytier-cli", "peer") }()
	type result struct {
		Name string `json:"name"`
		URL  string `json:"url"`
		engine.ProbeResult
	}
	probes := []result{}
	if s.Settings.CertdURL != "" {
		probes = append(probes, result{Name: "certd（仅 HTTP 可达检查）", URL: s.Settings.CertdURL})
	}
	for _, service := range s.Services {
		if service.Enabled && len(probes) < 9 {
			probes = append(probes, result{Name: service.Name, URL: service.Upstream})
		}
	}
	sem := make(chan struct{}, 4)
	for i := range probes {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				probes[index].Error = "诊断超时"
				return
			}
			probes[index].ProbeResult = engine.Probe(ctx, probes[index].URL)
		}(i)
	}
	wg.Wait()
	_, ngErr := exec.LookPath(a.opts.Nginx)
	// Each service already carries its own external status, and that status is
	// always "unverified" until someone checks from outside. A paragraph saying
	// the same thing is a second place for the two to disagree.
	notes := []string{}
	warning := ""
	if strings.Contains(peers, "relay(") {
		warning = "存在中继路径；不能把可达等同于 P2P 直连。"
	}
	if a.opts.DryRun {
		notes = append(notes, "开发 dry-run 模式：配置应用仅模拟，不操作 Nginx。")
	}
	reply(w, 200, map[string]any{"system": system, "easytier": map[string]string{"node": node, "peers": peers, "warning": warning}, "upstreams": probes, "nginx": map[string]any{"available": ngErr == nil, "enabled": !a.opts.DryRun && s.AppliedRevision > 0, "appliedAt": s.AppliedAt, "appliedAddress": s.AppliedPublicAddress}, "notes": notes})
}

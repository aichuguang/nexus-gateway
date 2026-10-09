package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/globalping"
)

var externalRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

type externalProvider interface {
	Limits(context.Context) (globalping.Quota, error)
	Create(context.Context, globalping.Target) (globalping.Created, error)
	Read(context.Context, string, string) (globalping.Result, string, bool, error)
}

type externalTarget struct {
	Key            string `json:"key"`
	ServiceID      string `json:"serviceId"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Protocol       string `json:"protocol"`
	IPVersions     []int  `json:"ipVersions"`
	DisabledReason string `json:"disabledReason"`
}

type externalInput struct {
	RequestID string `json:"requestId"`
	TargetKey string `json:"targetKey"`
	IPVersion int    `json:"ipVersion"`
	Nodes     int    `json:"nodes"`
	Confirm   bool   `json:"confirm"`
}

type externalJob struct {
	ID             string            `json:"id"`
	Target         externalTarget    `json:"target"`
	IPVersion      int               `json:"ipVersion"`
	RequestedNodes int               `json:"requestedNodes"`
	ProbesCount    int               `json:"probesCount"`
	MeasurementID  string            `json:"measurementId"`
	Status         string            `json:"status"`
	Message        string            `json:"message"`
	StartedAt      time.Time         `json:"startedAt"`
	UpdatedAt      time.Time         `json:"updatedAt"`
	Nodes          []globalping.Node `json:"nodes"`
	HTTPStatus     int               `json:"httpStatus,omitempty"`
}

type externalDisk struct {
	Version int           `json:"version"`
	Jobs    []externalJob `json:"jobs"`
}

type externalStore struct {
	mu                sync.Mutex
	loaded            bool
	err               string
	disk              externalDisk
	quota             *globalping.Quota
	quotaLoading      bool
	client            externalProvider
	lookup            func(context.Context, string) ([]net.IPAddr, error)
	pollInterval      time.Duration // Test seam; production polls at most once a second.
	quotaBlockedUntil time.Time
}

func (a *App) loadExternalLocked() {
	s := &a.external
	if s.loaded {
		return
	}
	s.loaded = true
	s.disk = externalDisk{Version: 1, Jobs: []externalJob{}}
	if s.client == nil {
		s.client = globalping.New()
	}
	path := filepath.Join(a.opts.DataDir, "external-probes.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 512<<10 {
		s.err = "外部探测历史不可安全读取，已停止新建探测"
		return
	}
	data, err := os.ReadFile(path)
	var saved externalDisk
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Version != 1 || len(saved.Jobs) > 32 {
		s.err = "外部探测历史格式异常，已停止新建探测"
		return
	}
	seen := map[string]bool{}
	recovered := false
	for i := range saved.Jobs {
		j := &saved.Jobs[i]
		if !externalRequestID.MatchString(j.ID) || seen[j.ID] || j.StartedAt.IsZero() || j.UpdatedAt.IsZero() || len(j.Nodes) > 2 || j.RequestedNodes < 1 || j.RequestedNodes > 2 {
			s.err = "外部探测历史无效，已停止新建探测"
			return
		}
		seen[j.ID] = true
		switch j.Status {
		case "running":
			j.Status, j.Message = "unknown", "网关已重启，先前探测结果待确认；未自动重新提交"
			recovered = true
		case "ok", "partial", "failed", "unknown", "unavailable":
		default:
			s.err = "外部探测历史状态无效，已停止新建探测"
			return
		}
	}
	s.disk = saved
	if s.disk.Jobs == nil {
		s.disk.Jobs = []externalJob{}
	}
	if recovered {
		a.saveExternalLocked()
	}
}

func (a *App) saveExternalLocked() bool {
	if err := writeJSON(filepath.Join(a.opts.DataDir, "external-probes.json"), a.external.disk); err != nil {
		a.external.err = "外部探测历史保存失败，已停止新建探测；请勿重复提交"
		return false
	}
	return true
}

func publicProbeIP(value string) bool {
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" || ip.Is4In6() {
		return false
	}
	if ip.Is4() {
		return engine.ValidPublicIPv4(value)
	}
	return engine.ValidatePublicAddress(value) == nil
}

func (a *App) externalTargets() []externalTarget {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := []externalTarget{}
	applied, err := a.publicAppliedState()
	if err != nil {
		return result
	}
	for _, service := range applied.Services {
		if !service.Enabled {
			continue
		}
		reason := ""
		if a.opts.DryRun {
			reason = "开发模式不向第三方提交真实探测"
		}
		if engine.ValidateService(service, applied.Settings) != nil {
			reason = "已发布服务配置无效"
		}
		current := a.serviceIndex(service.ID)
		if current < 0 || !a.state.Services[current].Enabled {
			reason = "服务已停用或已移出列表"
		}
		// The opaque selection key includes the running generation, never draft URLs.
		hosts := service.Domains
		protocol, port := "HTTPS", engine.DomainEntryPort
		if !service.UsesDomains() {
			hosts = []string{applied.AppliedPublicAddress}
			protocol, port = "HTTP", service.PublishedPort()
		}
		for _, host := range hosts {
			t := externalTarget{ServiceID: service.ID, Name: service.Name, Host: host, Port: port, Protocol: protocol, IPVersions: []int{4, 6}, DisabledReason: reason}
			if service.UsesDomains() {
				if !engine.ValidDomain(host) {
					t.DisabledReason = "域名格式无效"
				}
				t.URL = "https://" + host + "/"
			} else {
				t.URL = "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
				if !publicProbeIP(host) {
					t.DisabledReason = "没有可供外部节点访问的公网地址；不支持 EasyTier 或其他私网入口"
				}
				if ip, err := netip.ParseAddr(host); err == nil {
					if ip.Is4() {
						t.IPVersions = []int{4}
					} else {
						t.IPVersions = []int{6}
					}
				}
			}
			hash := sha256.Sum256([]byte(service.ID + "\n" + engine.ServiceGeneration(applied, service) + "\n" + t.URL))
			t.Key = hex.EncodeToString(hash[:])
			result = append(result, t)
		}
	}
	return result
}

func (a *App) getExternalProbes(w http.ResponseWriter, r *http.Request) {
	targets := a.externalTargets()
	a.external.mu.Lock()
	defer a.external.mu.Unlock()
	a.loadExternalLocked()
	reply(w, http.StatusOK, map[string]any{"targets": targets, "jobs": a.external.disk.Jobs, "quota": a.external.quota,
		"error": a.external.err, "provider": "Globalping", "country": "CN", "mode": "anonymous", "staleAfterSeconds": 600})
}

func externalMessage(err error, submitting bool) string {
	var e *globalping.Error
	if errors.As(err, &e) {
		switch e.Code {
		case "rate_limited":
			if e.Scope == "read" {
				return "Globalping 结果读取请求受限，请稍后刷新；不代表服务故障"
			}
			return externalQuotaMessage(e.Limit)
		case "no_nodes":
			return "Globalping 没有满足条件的中国大陆节点；未切换到海外节点"
		case "unexpected_country":
			return "返回节点不属于中国大陆，已拒绝使用该结果"
		}
	}
	if submitting {
		return "第三方提交结果未确认，可能已消耗额度；未自动重试，请先核对记录"
	}
	return "暂无法取得可靠的 Globalping 结果；不代表服务故障，未自动重新提交"
}

func externalQuotaMessage(limit int) string {
	if limit <= 0 {
		limit = 250
	}
	return fmt.Sprintf("小时内外部探测次数超限，%d 次/小时；等待额度恢复，不计为服务故障", limit)
}

func (a *App) markExternalRateLimit(job *externalJob, err error) {
	var e *globalping.Error
	if !errors.As(err, &e) || e.Code != "rate_limited" {
		return
	}
	job.HTTPStatus = http.StatusTooManyRequests
	if e.Scope == "read" {
		return
	}
	a.external.mu.Lock()
	defer a.external.mu.Unlock()
	limit, wait := e.Limit, e.RetryAfter
	if q := a.external.quota; q != nil {
		if limit <= 0 {
			limit = q.Limit
		}
		if wait <= 0 {
			wait = int(time.Until(q.CheckedAt.Add(time.Duration(q.Reset) * time.Second)).Seconds())
		}
	}
	if wait <= 0 {
		wait = 60
	}
	if wait > 3600 {
		wait = 3600
	}
	a.external.quotaBlockedUntil = time.Now().Add(time.Duration(wait) * time.Second)
	job.Message = externalQuotaMessage(limit)
}

func (a *App) getExternalLimits(w http.ResponseWriter, r *http.Request) {
	a.external.mu.Lock()
	a.loadExternalLocked()
	if a.external.quota != nil && time.Since(a.external.quota.CheckedAt) < 30*time.Second {
		q := *a.external.quota
		a.external.mu.Unlock()
		reply(w, 200, q)
		return
	}
	if a.external.quotaLoading {
		a.external.mu.Unlock()
		fail(w, 409, "正在读取额度，请稍后刷新")
		return
	}
	a.external.quotaLoading = true
	client := a.external.client
	a.external.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	q, err := client.Limits(ctx)
	a.external.mu.Lock()
	a.external.quotaLoading = false
	if err == nil {
		a.external.quota = &q
		if q.Remaining > 0 {
			a.external.quotaBlockedUntil = time.Time{}
		}
	}
	a.external.mu.Unlock()
	if err != nil {
		status := http.StatusBadGateway
		var providerErr *globalping.Error
		if errors.As(err, &providerErr) && providerErr.Code == "rate_limited" {
			status = http.StatusTooManyRequests
		}
		fail(w, status, externalMessage(err, false))
		return
	}
	reply(w, 200, q)
}

func (a *App) startExternalProbe(w http.ResponseWriter, r *http.Request) {
	var in externalInput
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm || !externalRequestID.MatchString(in.RequestID) || (in.IPVersion != 4 && in.IPVersion != 6) || in.Nodes < 1 || in.Nodes > 2 {
		fail(w, 400, "需确认向第三方提交已发布公网入口，并选择 1–2 个中国大陆节点及 IPv4 或 IPv6")
		return
	}
	var selected *externalTarget
	for _, target := range a.externalTargets() {
		if target.Key == in.TargetKey {
			t := target
			selected = &t
			break
		}
	}
	if selected == nil {
		fail(w, 409, "已发布入口发生变化，请刷新后重新选择")
		return
	}
	if selected.DisabledReason != "" {
		fail(w, 409, selected.DisabledReason)
		return
	}
	if len(selected.IPVersions) == 1 && selected.IPVersions[0] != in.IPVersion {
		fail(w, 400, "地址与所选 IP 协议不匹配")
		return
	}
	a.external.mu.Lock()
	a.loadExternalLocked()
	if a.external.err != "" {
		message := a.external.err
		a.external.mu.Unlock()
		fail(w, 503, message)
		return
	}
	for _, job := range a.external.disk.Jobs {
		if job.ID == in.RequestID {
			a.external.mu.Unlock()
			if job.Target.Key != in.TargetKey || job.IPVersion != in.IPVersion || job.RequestedNodes != in.Nodes {
				fail(w, 409, "请求编号已用于其他探测")
				return
			}
			reply(w, 200, job)
			return
		}
	}
	for _, job := range a.external.disk.Jobs {
		if job.Status == "running" {
			a.external.mu.Unlock()
			fail(w, 409, "已有外部探测正在进行，请等待结果")
			return
		}
		if time.Since(job.StartedAt) < 5*time.Second || (job.Target.Key == in.TargetKey && time.Since(job.StartedAt) < time.Minute) {
			a.external.mu.Unlock()
			fail(w, 429, "为保护免费额度，同一入口需间隔 60 秒；不同入口至少间隔 5 秒")
			return
		}
	}
	ctx, finish, accepted := a.beginWork()
	if !accepted {
		a.external.mu.Unlock()
		fail(w, 503, "管理程序正在停止")
		return
	}
	job := externalJob{ID: in.RequestID, Target: *selected, IPVersion: in.IPVersion, RequestedNodes: in.Nodes,
		Status: "running", Message: "准备核对公网目标及免费额度", StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Nodes: []globalping.Node{}}
	jobs := a.external.disk.Jobs
	previousJobs := jobs
	if len(jobs) >= 32 {
		jobs = jobs[len(jobs)-31:]
	}
	a.external.disk.Jobs = append(jobs, job)
	if !a.saveExternalLocked() {
		a.external.disk.Jobs = previousJobs
		a.external.mu.Unlock()
		finish()
		fail(w, 503, "无法保存探测任务，未向 Globalping 提交")
		return
	}
	a.external.mu.Unlock()
	go func() { defer finish(); a.runExternalProbe(ctx, job) }()
	reply(w, http.StatusAccepted, job)
}

func (a *App) updateExternalJob(job externalJob) bool {
	a.external.mu.Lock()
	defer a.external.mu.Unlock()
	job.UpdatedAt = time.Now().UTC()
	for i := range a.external.disk.Jobs {
		if a.external.disk.Jobs[i].ID == job.ID {
			a.external.disk.Jobs[i] = job
			return a.saveExternalLocked()
		}
	}
	return false
}

func (a *App) validateExternalHost(ctx context.Context, target externalTarget) error {
	if net.ParseIP(target.Host) != nil {
		if publicProbeIP(target.Host) {
			return nil
		}
		return errors.New("private target")
	}
	// Do not disclose internal-only/split-DNS names to the public provider.
	host := strings.ToLower(target.Host)
	for _, suffix := range []string{".local", ".localhost", ".internal", ".lan", ".home.arpa", ".test", ".invalid", ".example"} {
		if strings.HasSuffix(host, suffix) {
			return errors.New("private name")
		}
	}
	lookup := a.external.lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	child, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	addresses, err := lookup(child, target.Host)
	if err != nil || len(addresses) == 0 {
		return errors.New("unresolved target")
	}
	for _, address := range addresses {
		if !publicProbeIP(address.IP.String()) {
			return errors.New("private DNS answer")
		}
	}
	return nil
}

func (a *App) runExternalProbe(parent context.Context, job externalJob) {
	ctx, cancel := context.WithTimeout(parent, 55*time.Second)
	defer cancel()
	failJob := func(message string) { job.Status, job.Message = "unavailable", message; a.updateExternalJob(job) }
	failProvider := func(err error, submitting bool) {
		job.Status, job.Message = "unavailable", externalMessage(err, submitting)
		a.markExternalRateLimit(&job, err)
		a.updateExternalJob(job)
	}
	a.external.mu.Lock()
	blocked := time.Now().Before(a.external.quotaBlockedUntil)
	limit := 250
	if a.external.quota != nil {
		limit = a.external.quota.Limit
	}
	a.external.mu.Unlock()
	if blocked {
		job.HTTPStatus = 429
		failJob(externalQuotaMessage(limit))
		return
	}
	if a.validateExternalHost(ctx, job.Target) != nil {
		failJob("入口未解析到公开地址，未提交第三方；请先核对 DNS 或私网入口")
		return
	}
	a.external.mu.Lock()
	client, interval := a.external.client, a.external.pollInterval
	a.external.mu.Unlock()
	if interval <= 0 {
		interval = time.Second
	}
	q, err := client.Limits(ctx)
	if err != nil {
		failProvider(err, false)
		return
	}
	a.external.mu.Lock()
	a.external.quota = &q
	a.external.mu.Unlock()
	if q.Remaining < job.RequestedNodes {
		failProvider(&globalping.Error{Code: "rate_limited", Limit: q.Limit, RetryAfter: q.Reset, Scope: "create"}, false)
		return
	}
	target := globalping.Target{Host: job.Target.Host, Port: job.Target.Port, Protocol: job.Target.Protocol, IPVersion: job.IPVersion, Nodes: job.RequestedNodes}
	created, err := client.Create(ctx, target) // Never retry a quota-consuming POST.
	if err != nil {
		failProvider(err, true)
		return
	}
	job.MeasurementID, job.ProbesCount, job.Message = created.ID, created.ProbesCount, "中国大陆节点正在检查入口"
	if !a.updateExternalJob(job) {
		return
	}
	// Use the provider's post-submission snapshot when available; some test
	// targets may be exempt, and other applications can share the same exit IP.
	if fresh, quotaErr := client.Limits(ctx); quotaErr == nil {
		q = fresh
	} else {
		q.Remaining -= created.ProbesCount
	}
	a.external.mu.Lock()
	a.external.quota = &q
	a.external.mu.Unlock()
	etag := ""
	for {
		result, next, unchanged, err := client.Read(ctx, created.ID, etag)
		if err != nil {
			failProvider(err, false)
			return
		}
		if !unchanged {
			status, nodes, err := globalping.Summarize(result, target)
			if err != nil {
				failJob(externalMessage(err, false))
				return
			}
			changed := job.Status != status || !reflect.DeepEqual(job.Nodes, nodes)
			job.Status, job.Nodes = status, nodes
			if status != "running" {
				job.Message = "结果仅代表本次所列大陆节点；HEAD 响应不等于业务内容或实时回源正常"
				a.updateExternalJob(job)
				return
			}
			if changed && !a.updateExternalJob(job) {
				return
			}
			etag = next
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			failJob("等待结果超时或网关正在停止，结果待确认；未重新提交探测")
			return
		case <-timer.C:
		}
	}
}

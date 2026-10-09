package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/globalping"
	"w103d/gateway/internal/model"
	"w103d/gateway/internal/notify"
)

const monitorFailures = 2
const monitorRecoveries = 2
const monitorIntervalSeconds = 15 * 60

type monitorSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"intervalSeconds"`
	CheckPing       bool `json:"checkPing"`
	CheckTCP        bool `json:"checkTCP"`
	CheckHTTP       bool `json:"checkHTTP"`
	CheckExternal   bool `json:"checkExternal"`
	serviceID       string
	externalOnly    bool
}
type monitorCheck struct {
	Classification string                 `json:"classification,omitempty"`
	Status         string                 `json:"status"`
	LatencyMS      float64                `json:"latencyMs,omitempty"`
	StatusCode     int                    `json:"statusCode,omitempty"`
	Message        string                 `json:"message,omitempty"`
	CheckedAt      string                 `json:"checkedAt,omitempty"`
	Details        []monitorEndpointCheck `json:"details,omitempty"`
}
type monitorChecks struct {
	Ping     monitorCheck `json:"ping"`
	TCP      monitorCheck `json:"tcp"`
	HTTP     monitorCheck `json:"http"`
	Local    monitorCheck `json:"local"`
	External monitorCheck `json:"external"`
}
type monitoredService struct {
	ServiceID            string        `json:"serviceId"`
	Name                 string        `json:"name"`
	Status               string        `json:"status"`
	LastCheckedAt        string        `json:"lastCheckedAt"`
	ConsecutiveFailures  int           `json:"consecutiveFailures"`
	ConsecutiveSuccesses int           `json:"consecutiveSuccesses"`
	Checks               monitorChecks `json:"checks"`
	generation           string
	stable               string
	scope                string
}
type monitorView struct {
	Enabled           bool               `json:"enabled"`
	Running           bool               `json:"running"`
	LastCheckedAt     string             `json:"lastCheckedAt"`
	Error             string             `json:"error"`
	FailureThreshold  int                `json:"failureThreshold"`
	RecoveryThreshold int                `json:"recoveryThreshold"`
	Services          []monitoredService `json:"services"`
	ExternalQuota     *globalping.Quota  `json:"externalQuota,omitempty"`
}
type monitorStore struct {
	mu            sync.Mutex
	loaded        bool
	settings      monitorSettings
	err           error
	running       bool
	lastStarted   time.Time
	lastCheckedAt string
	lastError     string
	services      []monitoredService
	changed       chan struct{}
	cancel        context.CancelFunc
	probe         func(context.Context, model.Service, monitorSettings) monitorChecks // mock seam; never supplied over HTTP
	externalProbe func(context.Context, model.State, model.Service) monitorCheck
}

func defaultMonitorSettings() monitorSettings {
	return monitorSettings{IntervalSeconds: monitorIntervalSeconds, CheckPing: true, CheckTCP: true, CheckHTTP: true}
}
func validMonitorSettings(s monitorSettings) bool {
	return s.IntervalSeconds >= 30 && s.IntervalSeconds <= 3600 && (s.CheckTCP || s.CheckHTTP)
}

// Legacy cadence and upstream selections no longer customize execution. Keep
// enabled/external consent untouched: normalization is not third-party opt-in.
func normalizedMonitorSettings(s monitorSettings) monitorSettings {
	s.IntervalSeconds = monitorIntervalSeconds
	s.CheckPing, s.CheckTCP, s.CheckHTTP = true, true, true
	return s
}
func (a *App) loadMonitorLocked() {
	if a.monitor.loaded {
		return
	}
	a.monitor.loaded = true
	a.monitor.settings = defaultMonitorSettings()
	a.monitor.services = []monitoredService{}
	a.monitor.changed = make(chan struct{}, 1)
	var settings monitorSettings
	err := readPrivateSettingsJSON(filepath.Join(a.opts.DataDir, "monitor-settings.json"), &settings, 16384)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || !validMonitorSettings(settings) {
		a.monitor.err = errors.New("巡检设置无法读取，自动巡检已停止，原文件未被重置")
		return
	}
	// Preserve old opt-in and file contents. Old upstream-only settings do not
	// authorize sharing domains with a third party; explicit enablement saves it.
	a.monitor.settings = normalizedMonitorSettings(settings)
}
func (a *App) getMonitorSettings(w http.ResponseWriter, r *http.Request) {
	a.monitor.mu.Lock()
	defer a.monitor.mu.Unlock()
	a.loadMonitorLocked()
	if a.monitor.err != nil {
		fail(w, 503, a.monitor.err.Error())
		return
	}
	reply(w, 200, a.monitor.settings)
}
func (a *App) updateMonitorSettings(w http.ResponseWriter, r *http.Request) {
	var in monitorSettings
	if !decode(w, r, &in) {
		return
	}
	if !validMonitorSettings(in) {
		fail(w, 400, "巡检设置不合法；使用固定 900 秒及标准 Ping、TCP、HTTP 检查")
		return
	}
	in = normalizedMonitorSettings(in)
	a.monitor.mu.Lock()
	defer a.monitor.mu.Unlock()
	a.loadMonitorLocked()
	if a.monitor.err != nil {
		fail(w, 503, a.monitor.err.Error())
		return
	}
	pause := !in.Enabled && in.IntervalSeconds == a.monitor.settings.IntervalSeconds && in.CheckPing == a.monitor.settings.CheckPing && in.CheckTCP == a.monitor.settings.CheckTCP && in.CheckHTTP == a.monitor.settings.CheckHTTP && in.CheckExternal == a.monitor.settings.CheckExternal
	if a.monitor.running && !pause {
		fail(w, 409, "巡检正在执行，请结束后再修改设置")
		return
	}
	if writeJSON(filepath.Join(a.opts.DataDir, "monitor-settings.json"), in) != nil {
		fail(w, 500, "无法保存巡检设置")
		return
	}
	if in.CheckTCP != a.monitor.settings.CheckTCP || in.CheckHTTP != a.monitor.settings.CheckHTTP || in.CheckExternal != a.monitor.settings.CheckExternal {
		a.monitor.services = []monitoredService{}
	}
	a.monitor.settings = in
	if !in.Enabled && a.monitor.cancel != nil {
		a.monitor.cancel()
	}
	select {
	case a.monitor.changed <- struct{}{}:
	default:
	}
	reply(w, 200, in)
}
func (a *App) monitorViewLocked() monitorView {
	message := a.monitor.lastError
	if a.monitor.err != nil {
		message = a.monitor.err.Error()
	}
	view := monitorView{Enabled: a.monitor.settings.Enabled, Running: a.monitor.running, LastCheckedAt: a.monitor.lastCheckedAt, Error: message, FailureThreshold: monitorFailures, RecoveryThreshold: monitorRecoveries, Services: append([]monitoredService{}, a.monitor.services...)}
	a.external.mu.Lock()
	if a.external.quota != nil {
		q := *a.external.quota
		view.ExternalQuota = &q
	}
	a.external.mu.Unlock()
	return view
}
func (a *App) getMonitorStatus(w http.ResponseWriter, r *http.Request) {
	a.monitor.mu.Lock()
	defer a.monitor.mu.Unlock()
	a.loadMonitorLocked()
	reply(w, 200, a.monitorViewLocked())
}
func (a *App) checkMonitor(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ConfirmExternal bool `json:"confirmExternal"`
	}
	if !decode(w, r, &in) {
		return
	}
	if a.opts.DryRun {
		fail(w, 403, "开发 dry-run 模式不执行真实网络巡检")
		return
	}
	if err := a.launchMonitorCheckSelected(false, "", false, in.ConfirmExternal); err != nil {
		fail(w, 409, err.Error())
		return
	}
	a.monitor.mu.Lock()
	defer a.monitor.mu.Unlock()
	reply(w, 202, a.monitorViewLocked())
}
func (a *App) launchMonitorCheck() error {
	return a.launchMonitorCheckWithMode(false)
}

func (a *App) launchMonitorCheckWithMode(automatic bool) error {
	return a.launchMonitorCheckSelected(automatic, "", false, false)
}

func (a *App) checkServiceExternal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		fail(w, 400, "需确认通过 Globalping 中国大陆节点检查已发布公网入口")
		return
	}
	if a.opts.DryRun {
		fail(w, 403, "开发模式不执行真实外部探测")
		return
	}
	id := r.PathValue("id")
	applied, err := a.publicAppliedState()
	found := false
	if err == nil {
		for _, s := range applied.Services {
			if s.ID == id && s.Enabled {
				found = true
			}
		}
	}
	if !found {
		fail(w, 409, "此服务没有已发布且启用的入口")
		return
	}
	if err := a.launchMonitorCheckSelected(false, id, true, true); err != nil {
		fail(w, 409, err.Error())
		return
	}
	a.monitor.mu.Lock()
	defer a.monitor.mu.Unlock()
	reply(w, 202, a.monitorViewLocked())
}

func (a *App) launchMonitorCheckSelected(automatic bool, serviceID string, externalOnly, confirmExternal bool) error {
	ctx, finish, accepted := a.beginWork()
	if !accepted {
		return errAppStopping
	}
	a.monitor.mu.Lock()
	a.loadMonitorLocked()
	if a.monitor.err != nil {
		err := a.monitor.err
		a.monitor.mu.Unlock()
		finish()
		return err
	}
	if automatic && !a.monitor.settings.Enabled {
		a.monitor.mu.Unlock()
		finish()
		return nil
	}
	if a.monitor.running {
		a.monitor.mu.Unlock()
		finish()
		return errors.New("已有巡检正在执行")
	}
	if time.Since(a.monitor.lastStarted) < 5*time.Second {
		a.monitor.mu.Unlock()
		finish()
		return errors.New("两次巡检至少间隔 5 秒，请稍后重试")
	}
	a.monitor.lastStarted = time.Now()
	a.monitor.running = true
	ctx, cancel := context.WithCancel(ctx)
	a.monitor.cancel = cancel
	settings := normalizedMonitorSettings(a.monitor.settings)
	settings.serviceID, settings.externalOnly = serviceID, externalOnly
	if !automatic {
		settings.CheckExternal = confirmExternal
	}
	a.monitor.mu.Unlock()
	go func() {
		defer finish()
		defer cancel()
		a.runMonitorCheck(ctx, settings)
	}()
	return nil
}

func (a *App) startMonitor() func() {
	ctx, finish, accepted := a.beginWork()
	if !accepted {
		return func() {}
	}
	ctx, stop := context.WithCancel(ctx)
	a.monitor.mu.Lock()
	a.loadMonitorLocked()
	changed := a.monitor.changed
	a.monitor.mu.Unlock()
	go func() {
		defer finish()
		defer stop()
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				// Settings are read again at launch, so a pause cannot race an
				// already scheduled automatic run into starting new probes.
				timer.Reset(time.Second)
				continue
			case <-timer.C:
			}
			a.monitor.mu.Lock()
			a.loadMonitorLocked()
			settings := a.monitor.settings
			valid := a.monitor.err == nil
			a.monitor.mu.Unlock()
			if settings.Enabled && valid && !a.opts.DryRun {
				_ = a.launchMonitorCheckWithMode(true)
			}
			timer.Reset(time.Duration(monitorIntervalSeconds) * time.Second)
		}
	}()
	return stop
}

func unknownMonitorChecks() monitorChecks {
	unknown := monitorCheck{Status: "unknown", Message: "未执行"}
	return monitorChecks{Ping: unknown, TCP: unknown, HTTP: unknown, Local: unknown, External: unknown}
}
func (a *App) runMonitorCheck(ctx context.Context, settings monitorSettings) {
	settings = normalizedMonitorSettings(settings)
	a.monitor.mu.Lock()
	a.loadMonitorLocked()
	a.monitor.mu.Unlock()
	defer func() {
		a.monitor.mu.Lock()
		a.monitor.running = false
		a.monitor.cancel = nil
		a.monitor.mu.Unlock()
	}()
	if a.opts.DryRun {
		a.monitor.mu.Lock()
		a.monitor.lastError = "开发 dry-run 模式不执行真实网络巡检"
		a.monitor.mu.Unlock()
		return
	}
	applied, err := a.publicAppliedState()
	if err != nil || applied.Revision != applied.AppliedRevision || applied.AppliedRevision == 0 || len(applied.Services) > maxServices {
		a.monitor.mu.Lock()
		a.monitor.lastError = "没有可核对的已应用快照；未探测草稿或任意地址"
		a.monitor.mu.Unlock()
		return
	}
	services := []model.Service{}
	for _, service := range applied.Services {
		if !service.Enabled || (settings.serviceID != "" && service.ID != settings.serviceID) {
			continue
		}
		if engine.ValidateService(service, applied.Settings) != nil {
			a.monitor.mu.Lock()
			a.monitor.lastError = "已应用服务的上游或配置不合法，已停止本轮探测"
			a.monitor.mu.Unlock()
			return
		}
		services = append(services, service)
	}
	a.monitor.mu.Lock()
	previous := map[string]monitoredService{}
	for _, entry := range a.monitor.services {
		previous[entry.ServiceID] = entry
	}
	retained := []monitoredService{}
	if settings.externalOnly {
		for _, entry := range a.monitor.services {
			if entry.ServiceID != settings.serviceID {
				retained = append(retained, entry)
			}
		}
	}
	a.monitor.services = retained
	for _, service := range services {
		entry, ok := previous[service.ID]
		generation := monitorServiceGeneration(applied, service)
		coverage := fmt.Sprintf("%t/%t/%t", settings.CheckTCP, settings.CheckHTTP, settings.CheckExternal)
		if !ok || entry.generation != generation || (!settings.externalOnly && entry.scope != coverage) {
			entry = monitoredService{ServiceID: service.ID, Name: service.Name, Status: "unknown", stable: "unknown", generation: generation, Checks: unknownMonitorChecks()}
		}
		if !settings.externalOnly {
			entry.scope = coverage
		}
		entry.Name = service.Name
		entry.Status = "checking"
		a.monitor.services = append(a.monitor.services, entry)
	}
	probe := a.monitor.probe
	a.monitor.lastError = ""
	a.monitor.mu.Unlock()
	if probe == nil {
		probe = func(ctx context.Context, service model.Service, settings monitorSettings) monitorChecks {
			checks := probeMonitoredService(ctx, service, settings)
			checks.Local = a.monitorLocalEntry(ctx, applied, service)
			return checks
		}
	}
	probeContext, cancelProbes := context.WithTimeout(ctx, 45*time.Second)
	defer cancelProbes()
	results := make([]monitorChecks, len(services))
	completed := make([]bool, len(services))
	semaphore := make(chan struct{}, 4)
	var workers sync.WaitGroup
	for i, service := range services {
		if settings.externalOnly {
			results[i] = unknownMonitorChecks()
			if previous, ok := previous[service.ID]; ok && previous.generation == monitorServiceGeneration(applied, service) {
				results[i] = previous.Checks
			}
			completed[i] = true
			continue
		}
		workers.Add(1)
		go func(index int, service model.Service) {
			defer workers.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-probeContext.Done():
				return
			}
			results[index] = probe(probeContext, service, settings)
			completed[index] = probeContext.Err() == nil
		}(i, service)
	}
	workers.Wait()
	localTimedOut := probeContext.Err() != nil
	cancelProbes()
	if settings.CheckExternal {
		externalContext, cancel := context.WithTimeout(ctx, 240*time.Second)
		externalProbe := a.monitor.externalProbe
		if externalProbe == nil {
			externalProbe = a.monitorExternalEntry
		}
		for i, service := range services {
			if externalContext.Err() != nil {
				results[i].External = monitorCheck{Status: "unknown", Message: "本轮时间预算已用尽，未执行外部检测"}
				continue
			}
			results[i].External = externalProbe(externalContext, applied, service)
			results[i].External.CheckedAt = time.Now().UTC().Format(time.RFC3339)
		}
		cancel()
	} else {
		for i := range results {
			results[i].External = monitorCheck{Status: "unknown", Message: "本轮未获外网检查授权；可明确启用包含外网的自动监测，或确认一次性外网检查"}
		}
	}
	if ctx.Err() != nil {
		for i := range completed {
			completed[i] = false
		}
	}
	// A publish or restore may have replaced the snapshot during network I/O.
	// Do not emit a new incident for a removed or changed service definition.
	latest, latestErr := a.publicAppliedState()
	fresh := map[string]string{}
	if latestErr == nil && latest.Revision == latest.AppliedRevision && latest.AppliedRevision > 0 && len(latest.Services) <= maxServices {
		for _, service := range latest.Services {
			if service.Enabled {
				fresh[service.ID] = monitorServiceGeneration(latest, service)
			}
		}
	}
	changedDuringCheck := false
	changedServices := make([]bool, len(services))
	for index, service := range services {
		if fresh[service.ID] != monitorServiceGeneration(applied, service) {
			completed[index] = false
			changedServices[index] = true
			changedDuringCheck = true
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	events := []notify.Event{}
	a.monitor.mu.Lock()
	for index, service := range services {
		var entry *monitoredService
		for i := range a.monitor.services {
			if a.monitor.services[i].ServiceID == service.ID {
				entry = &a.monitor.services[i]
				break
			}
		}
		if entry == nil {
			continue
		}
		if !completed[index] {
			if changedServices[index] {
				entry.stable = "unknown"
				entry.Checks = unknownMonitorChecks()
				entry.LastCheckedAt = ""
				entry.ConsecutiveFailures, entry.ConsecutiveSuccesses = 0, 0
				entry.generation = ""
			}
			entry.Status = "unknown"
			entry.ConsecutiveFailures, entry.ConsecutiveSuccesses = 0, 0
			continue
		}
		entry.Checks = results[index]
		status := monitorOverall(results[index], settings)
		if settings.externalOnly {
			entry.Status = "unknown"
			if old, ok := previous[service.ID]; ok && old.generation == entry.generation {
				entry.Status = old.Status
			}
			continue
		}
		entry.LastCheckedAt = now
		entry.Checks.Local.CheckedAt = now
		if event := advanceMonitorState(entry, status, now); event != nil {
			events = append(events, *event)
		}
	}
	if !settings.externalOnly {
		a.monitor.lastCheckedAt = now
	}
	for _, checks := range results {
		if checks.External.StatusCode == 429 {
			a.monitor.lastError = checks.External.Message
			break
		}
	}
	if localTimedOut {
		a.monitor.lastError = "本轮巡检被取消或达到时间上限；未完成项不计入故障次数"
	} else if changedDuringCheck {
		a.monitor.lastError = "配置在巡检期间发生变化；受影响服务的旧结果未计入告警"
	}
	a.monitor.mu.Unlock()
	if ctx.Err() == nil {
		a.dispatchMonitorEvents(events)
	}
}

// One bounded summary per pass prevents a shared network failure from creating
// hundreds of simultaneous deliveries. The notification worker has its own
// lifecycle-bound deadline: a completed probe must not lose its transition
// merely because another service exhausted the probe pass's time budget.
func (a *App) dispatchMonitorEvents(events []notify.Event) {
	if len(events) == 0 {
		return
	}
	event := events[0]
	if len(events) > 1 {
		message := "网关整体连通性状态变化：\n"
		status := "ok"
		for i, item := range events {
			if item.Status == "error" {
				status = "error"
			}
			if i < 20 {
				label := "恢复"
				if item.Status == "error" {
					label = "故障"
				}
				message += fmt.Sprintf("%s：%s\n", item.ServiceName, label)
			}
		}
		if len(events) > 20 {
			message += fmt.Sprintf("另有 %d 项变化，请查看管理台。\n", len(events)-20)
		}
		event = notify.Event{ID: NewID(), Kind: "service_summary", Status: status, At: event.At, Message: message}
	}
	ctx, finish, accepted := a.beginWork()
	if !accepted {
		return
	}
	go func() {
		defer finish()
		child, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		a.notifyMonitorEvent(child, event)
	}()
}

func advanceMonitorState(entry *monitoredService, result, at string) *notify.Event {
	if entry.stable == "" {
		entry.stable = "unknown"
	}
	before := entry.stable
	switch result {
	case "ok":
		entry.ConsecutiveFailures = 0
		entry.ConsecutiveSuccesses++
		if entry.stable != "error" || entry.ConsecutiveSuccesses >= monitorRecoveries {
			entry.stable = "ok"
		}
	case "error":
		entry.ConsecutiveSuccesses = 0
		entry.ConsecutiveFailures++
		if entry.ConsecutiveFailures >= monitorFailures {
			entry.stable = "error"
		}
	default:
		entry.ConsecutiveFailures = 0
		entry.ConsecutiveSuccesses = 0
		entry.Status = "unknown"
		return nil
	}
	entry.Status = result
	if result == "ok" && entry.stable == "error" {
		entry.Status = "recovering"
	}
	if before == entry.stable || (entry.stable != "error" && before != "error") {
		return nil
	}
	kind, label := "service_failure", "故障"
	if entry.stable == "ok" {
		kind, label = "service_recovery", "恢复"
	}
	return &notify.Event{ID: NewID(), Kind: kind, ServiceID: entry.ServiceID, ServiceName: entry.Name, Status: entry.stable, At: at, Message: fmt.Sprintf("网关整体连通性\n服务：%s\n状态：%s\n本机：%s\n外部：%s\n时间：%s", entry.Name, label, entry.Checks.Local.Status, entry.Checks.External.Status, at)}
}

func probeMonitoredService(ctx context.Context, service model.Service, settings monitorSettings) monitorChecks {
	checks := unknownMonitorChecks()
	parsed, err := url.Parse(service.Upstream)
	if err != nil {
		return checks
	}
	var workers sync.WaitGroup
	if settings.CheckPing {
		workers.Add(1)
		go func() { defer workers.Done(); checks.Ping = monitorPing(ctx, parsed.Hostname()) }()
	}
	if settings.CheckTCP {
		workers.Add(1)
		go func() {
			defer workers.Done()
			started := time.Now()
			child, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			connection, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(child, "tcp", parsed.Host)
			checks.TCP = monitorCheck{Status: "ok", LatencyMS: float64(time.Since(started).Milliseconds())}
			if err != nil {
				checks.TCP.Status, checks.TCP.Message = "error", "TCP 连接失败或超时"
			} else {
				_ = connection.Close()
			}
		}()
	}
	if settings.CheckHTTP {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result := engine.ProbeHealth(ctx, service.Upstream, service.HealthCheck)
			checks.HTTP = monitorCheck{Status: "ok", LatencyMS: float64(result.LatencyMS), StatusCode: result.Status, Classification: result.Classification}
			if result.Classification == "auth-required" {
				checks.HTTP.Message = "Upstream reachable; authentication required, business health not verified"
			}
			if !result.OK {
				checks.HTTP.Status, checks.HTTP.Message = "error", result.Error
			}
		}()
	}
	workers.Wait()
	return checks
}

func monitorPing(ctx context.Context, host string) monitorCheck {
	if net.ParseIP(host) == nil {
		return monitorCheck{Status: "unknown", Message: "Ping 目标不是已验证地址"}
	}
	program, err := exec.LookPath("ping")
	if err != nil {
		return monitorCheck{Status: "unknown", Message: "系统没有可用的 ping 工具"}
	}
	child, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	args := []string{"-n", "-c", "1", "-W", "2", host}
	if runtime.GOOS == "windows" {
		args = []string{"-n", "1", "-w", "2000", host}
	}
	started := time.Now()
	err = exec.CommandContext(child, program, args...).Run()
	result := monitorCheck{Status: "ok", LatencyMS: float64(time.Since(started).Milliseconds())}
	if err != nil {
		result.Status, result.Message = "error", "Ping 无回应或 ICMP 不可用（不单独判定服务故障）"
	}
	return result
}

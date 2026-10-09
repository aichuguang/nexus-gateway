package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

func clone(s model.State) model.State {
	b, _ := json.Marshal(s)
	var n model.State
	_ = json.Unmarshal(b, &n)
	return n
}

func (a *App) serviceIndex(id string) int {
	for i, s := range a.state.Services {
		if s.ID == id {
			return i
		}
	}
	return -1
}

func (a *App) certIndex(id string) int {
	for i, c := range a.state.Certificates {
		if c.ID == id {
			return i
		}
	}
	return -1
}

// prepareService fills in the shorthand an operator actually types and then
// validates the result. Callers hold mu.
func (a *App) prepareService(s *model.Service) error {
	s.Name = strings.TrimSpace(s.Name)
	s.Icon = strings.TrimSpace(s.Icon)
	switch s.Icon {
	case "", "server", "folder", "film", "music", "globe", "code", "terminal", "shield", "cloud", "grid", "drive", "wrench", "pen", "image", "download", "database":
	default:
		return fmt.Errorf("请选择内置服务图标，或使用自动图标")
	}
	s.Upstream = engine.NormalizeUpstream(s.Upstream)
	s.CertificateID = strings.TrimSpace(s.CertificateID)
	s.EdgeProvider = strings.ToLower(strings.TrimSpace(s.EdgeProvider))
	s.EdgeAccountID = strings.TrimSpace(s.EdgeAccountID)
	s.PreferredDomain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s.PreferredDomain)), ".")
	domains := make([]string, 0, len(s.Domains))
	for _, domain := range s.Domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if domain != "" {
			domains = append(domains, domain)
		}
	}
	s.Domains = domains
	// A service published by port never carries a certificate reference that
	// nothing reads; the entry style is decided by the domains alone. The same
	// goes for the entry mode, which only means anything for a name.
	if len(s.Domains) == 0 {
		s.CertificateID = ""
		s.EntryMode = ""
	}
	if !s.ServesEdge() {
		s.EdgeProvider, s.EdgeAccountID, s.PreferredDomain, s.UsePreferred = "", "", "", false
	}
	if s.ServesCloudflare() {
		if err := cloudflareServiceAccount(a.state, *s); err != nil {
			return err
		}
	}
	return engine.ValidateService(*s, a.state.Settings)
}

func (a *App) createService(w http.ResponseWriter, r *http.Request) {
	var s model.Service
	if !decode(w, r, &s) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.state.Services) >= maxServices {
		fail(w, 400, "服务数量已达上限")
		return
	}
	s.ID = creationResourceID(r)
	if e := a.prepareService(&s); e != nil {
		fail(w, 400, e.Error())
		return
	}
	old := clone(a.state)
	a.state.Services = append(a.state.Services, s)
	a.state.Revision++
	if e := a.saveLocked(); e != nil {
		a.state = old
		fail(w, 500, "无法保存配置")
		return
	}
	a.replyStateLocked(w)
}

func (a *App) updateService(w http.ResponseWriter, r *http.Request) {
	var input struct {
		model.Service
		Icon         *string         `json:"icon"`
		EdgeProvider *string         `json:"edgeProvider"`
		HealthCheck  json.RawMessage `json:"healthCheck"`
	}
	if !decode(w, r, &input) {
		return
	}
	s := input.Service
	if input.EdgeProvider != nil {
		s.EdgeProvider = *input.EdgeProvider
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	id := r.PathValue("id")
	i := a.serviceIndex(id)
	if i < 0 {
		fail(w, 404, "服务不存在")
		return
	}
	if a.state.Services[i].ServesCloudflare() && s.EntryMode == model.EntryEdge && input.EdgeProvider == nil {
		fail(w, 409, "客户端缺少 Cloudflare 入口字段，请刷新新版页面后重试，未改变服务配置")
		return
	}
	if blocked, err := a.serviceDeletionBlocked(id); err != nil {
		fail(w, 409, err.Error())
		return
	} else if blocked {
		fail(w, 409, "该服务已有删除保护记录，不能改动其域名或配置；请保持原删除选项完成删除")
		return
	}
	s.ID = id
	if len(input.HealthCheck) == 0 {
		s.HealthCheck = a.state.Services[i].HealthCheck
	} else if err := json.Unmarshal(input.HealthCheck, &s.HealthCheck); err != nil {
		fail(w, 400, "健康检查配置格式不正确")
		return
	}
	// Older clients omit this decoration; an explicit empty string restores auto.
	s.Icon = a.state.Services[i].Icon
	if input.Icon != nil {
		s.Icon = *input.Icon
	}
	if e := a.prepareService(&s); e != nil {
		fail(w, 400, e.Error())
		return
	}
	// A name that was published and is no longer claimed by this service must be
	// withdrawn before the claim disappears, or the record stays at the provider
	// pointing at an entry nothing serves.
	if err := a.assertNamesWithdrawn(a.state.Services[i], s); err != nil {
		fail(w, 409, err.Error())
		return
	}
	old := clone(a.state)
	a.state.Services[i] = s
	a.state.Revision++
	if e := a.saveLocked(); e != nil {
		a.state = old
		fail(w, 500, "无法保存配置")
		return
	}
	a.replyStateLocked(w)
}

// assertNamesWithdrawn refuses a change that would drop a name while that name is
// still published. Disabling the service is allowed and is exactly how a name
// gets withdrawn: a disabled service still appears in the DNS list with nothing
// wanted for it, which is what makes the deletion previewable.
func (a *App) assertNamesWithdrawn(before, after model.Service) error {
	published, err := a.readDNSJournal()
	if err != nil {
		return errors.New("无法读取 DNS 托管记录，已停止改动；请先修复数据目录")
	}
	if len(published.Managed) == 0 {
		return nil
	}
	if len(before.Domains) == 0 {
		return nil
	}
	// Disabling counts as keeping: a disabled service still claims its names, and
	// that claim is what puts them in the DNS list with nothing wanted for them,
	// which is what makes their withdrawal previewable. Only a name that is being
	// dropped outright has to be withdrawn first.
	kept := map[string]bool{}
	for _, domain := range after.Domains {
		kept[strings.ToLower(domain)] = true
	}
	for _, domain := range before.Domains {
		host := strings.ToLower(domain)
		if _, live := published.Managed[host]; live && !kept[host] {
			return fmt.Errorf("域名 %s 仍有托管 DNS 解析，请先停用该服务并撤回解析，再移除这个域名", host)
		}
	}
	return nil
}

func (a *App) probeService(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	i := a.serviceIndex(r.PathValue("id"))
	if i < 0 {
		a.mu.Unlock()
		fail(w, 404, "服务不存在")
		return
	}
	upstream := a.state.Services[i].Upstream
	a.mu.Unlock()
	reply(w, 200, engine.Probe(r.Context(), upstream))
}

func (a *App) preview(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	publicAddress := a.publicAddressForRender()
	cfg, warnings, e := a.engine.Render(a.state, publicAddress)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	if publicAddress == "" && a.opts.PublicAddress == "" {
		warnings = append(warnings, "尚未探测到可用的公网地址；应用时会重新探测，探测失败则只在内网 EasyTier 地址上提供服务。")
	}
	if a.opts.DryRun {
		warnings = append(warnings, "当前为开发 dry-run 模式，不会启动或重载真实代理。")
	}
	applied, err := a.publicAppliedState()
	if err != nil {
		// Nothing readable is running; every service is about to be created.
		applied = model.State{}
	}
	reply(w, 200, map[string]any{
		"config": cfg, "revision": a.state.Revision, "publicAddress": publicAddress,
		"warnings": warnings, "dryRun": a.opts.DryRun,
		"services":           previewServices(a.state, applied, publicAddress),
		"impact":             previewImpact(a.state, applied, publicAddress),
		"publishFingerprint": publishFingerprint(a.state),
	})
}

// previewServiceItem is one service as the preview sees it. A service carries
// its own "预览并应用", and asking an operator to find their server block in a
// rendered nginx file to check one entry is asking them to read a document they
// did not write. The entry, the upstream and what changes are the whole answer.
//
// Change is create / update / unchanged, derived from the same generation hash
// that decides whether a reload took effect, so "unchanged" here and "nothing to
// do" there cannot disagree.
type previewServiceItem struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Entry       string   `json:"entry"`
	Upstream    string   `json:"upstream"`
	Domains     []string `json:"domains"`
	Certificate string   `json:"certificate"`
	Port        int      `json:"port"`
	// Enabled is what publishing will do, not what the row shows: turning a
	// service off is a change like any other, and the dialog that asks the
	// operator to confirm it has to say that the entry goes away.
	Enabled bool   `json:"enabled"`
	Change  string `json:"change"`
}

func previewServices(draft, applied model.State, publicAddress string) []previewServiceItem {
	live := make(map[string]string, len(applied.Services))
	for _, s := range applied.Services {
		live[s.ID] = engine.ServiceGeneration(applied, s)
	}
	certNames := make(map[string]string, len(draft.Certificates))
	for _, c := range draft.Certificates {
		certNames[c.ID] = c.Name
	}
	items := make([]previewServiceItem, 0, len(draft.Services))
	for _, s := range draft.Services {
		entry, port, domains := entryDescription(s, publicAddress)
		if domains == nil {
			domains = []string{}
		}
		change := "unchanged"
		if g, ok := live[s.ID]; !ok {
			change = "create"
		} else if previous, _ := findService(applied.Services, s.ID); g != engine.ServiceGeneration(draft, s) || !sameEntryPolicy(previous, s) {
			change = "update"
		}
		items = append(items, previewServiceItem{
			ID: s.ID, Name: s.Name, Entry: entry, Upstream: s.Upstream,
			Domains: domains, Certificate: certNames[s.CertificateID], Port: port,
			Enabled: s.Enabled, Change: change,
		})
	}
	return items
}

// publicAddressForRender is the address a preview renders for: the pinned one
// when the operator set it, otherwise the address the running snapshot was
// actually rendered for. It never probes, because a preview is a read-only look
// at what applying would produce rather than an action of its own.
func (a *App) publicAddressForRender() string {
	if a.opts.PublicAddress != "" {
		return a.opts.PublicAddress
	}
	return a.state.AppliedPublicAddress
}

func (a *App) verifyTLS(s model.State, publicAddress string) error {
	if a.opts.DryRun {
		return nil
	}
	var e error
	for i := 0; i < 4; i++ {
		if i > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		e = a.engine.VerifyTLS(s, publicAddress)
		if e == nil {
			return nil
		}
	}
	return e
}

var errApplyNeedsReview = errors.New("本机运行配置与快照可能不一致，请先核对实际状态，不要直接重试发布")

// Always attempt both recoveries: a snapshot failure must not skip the proxy
// rollback. Keep every error, and only claim recovery after local TLS verifies.
func (a *App) rollbackApplyLocked(previous model.State, cause error, restoreSnapshot func() error) error {
	failures := []error{cause}
	if restoreSnapshot != nil {
		if err := restoreSnapshot(); err != nil {
			failures = append(failures, fmt.Errorf("恢复已应用快照失败：%w", err))
		}
	}
	if _, err := a.engine.Apply(previous, previous.AppliedPublicAddress); err != nil {
		failures = append(failures, fmt.Errorf("代理配置回滚失败：%w", err))
	} else if err := a.verifyTLS(previous, previous.AppliedPublicAddress); err != nil {
		failures = append(failures, fmt.Errorf("回滚后的 TLS 生效检查失败：%w", err))
	}
	if len(failures) > 1 {
		return fmt.Errorf("%w：%w", errApplyNeedsReview, errors.Join(failures...))
	}
	return fmt.Errorf("%w；已恢复之前代理配置", cause)
}

func (a *App) applyLocked(ctx context.Context) error {
	if a.runtimeWriter != nil {
		return a.applyWithWriterLocked(ctx, a.runtimeWriter)
	}
	return a.applyWithWriterLocked(ctx, writeRuntimeJSON)
}

// The injected writer allows deterministic disk-failure tests without relying
// on platform-specific directory permissions. Production always uses writeJSON.
func (a *App) applyWithWriterLocked(ctx context.Context, write func(string, any) error) error {
	if err := a.refuseDeletionProtectedState(a.state); err != nil {
		return err
	}
	publicAddress, _, err := a.selectPublicAddress(ctx)
	if err != nil {
		return err
	}
	next := clone(a.state)
	next.AppliedPublicAddress = publicAddress
	return a.commitRuntimeLocked(clone(a.state), next, write)
}

// selectPublicAddress decides which address the gateway publishes on. It is
// re-read on every apply rather than trusted from a draft, because the address
// belongs to the line and not to the operator, and a stale one binds nothing.
//
// IPv6 is preferred when the line has it. IPv4 is accepted only when it is a
// real public address, never a private or carrier-grade NAT one: a name pointing
// at 192.168.x.x resolves perfectly and can never be reached, which is a far
// worse thing to debug than a name that does not exist.
func (a *App) selectPublicAddress(ctx context.Context) (string, []string, error) {
	if a.opts.PublicAddress != "" {
		if err := engine.ValidatePublicAddress(a.opts.PublicAddress); err != nil && !engine.ValidPublicIPv4(a.opts.PublicAddress) {
			return "", nil, fmt.Errorf("指定的公网地址不可用：%w", err)
		}
		return a.opts.PublicAddress, nil, nil
	}
	if a.opts.DryRun {
		return "", []string{"开发 dry-run 模式不探测本机公网地址；可使用 --public-address 指定。"}, nil
	}
	candidates, _, warnings := a.inspectHost(ctx)
	ipv6, ipv4 := "", ""
	for _, candidate := range candidates {
		if !candidate.Eligible {
			continue
		}
		switch candidate.Family {
		case "inet6":
			if ipv6 == "" {
				ipv6 = candidate.Address
			}
		case "inet":
			if ipv4 == "" {
				ipv4 = candidate.Address
			}
		}
	}
	if ipv6 != "" {
		return ipv6, warnings, nil
	}
	if ipv4 != "" {
		return ipv4, warnings, nil
	}
	return "", append(warnings, "没有探测到可用的公网 IPv6 或公网 IPv4 地址；本次只在内网 EasyTier 地址上提供服务。"), nil
}

func (a *App) apply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision int64 `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if in.Revision != a.state.Revision {
		fail(w, 409, "配置版本已变化，请重新预览后应用")
		return
	}
	if e := a.applyLocked(r.Context()); e != nil {
		fail(w, 500, e.Error())
		return
	}
	reply(w, 200, map[string]any{"ok": true, "publicAddress": a.state.AppliedPublicAddress, "state": a.state})
}

func ProbeWithDeadline(upstream string) engine.ProbeResult {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return engine.Probe(ctx, upstream)
}

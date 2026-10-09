package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"w103d/gateway/internal/cloudflare"
	"w103d/gateway/internal/model"
)

const cfDiscoveryMaxZones = 12
const cfDiscoveryNameAttempts = 5

type cfZoneLister interface {
	Zones(context.Context) ([]cloudflare.Zone, error)
}

type cfDiscoveredZone struct {
	ZoneID                 string `json:"zoneId"`
	ZoneName               string `json:"zoneName"`
	Eligible               bool   `json:"eligible"`
	Message                string `json:"message"`
	FallbackDomain         string `json:"fallbackDomain"`
	StandardTarget         string `json:"standardTarget"`
	FallbackMode           string `json:"fallbackMode,omitempty"`
	ExistingFallback       string `json:"existingFallback"`
	ExistingFallbackTarget string `json:"existingFallbackTarget,omitempty"`
	DesiredBaseDomain      string `json:"desiredBaseDomain,omitempty"`
	FallbackStatus         string `json:"fallbackStatus,omitempty"`
	SSLMode                string `json:"sslMode,omitempty"`
	RequiresSSLStrict      bool   `json:"requiresSSLStrict"`
}

type cfDiscoveryResult struct {
	Zones             []cfDiscoveredZone `json:"zones"`
	RecommendedZoneID string             `json:"recommendedZoneId"`
	Warnings          []string           `json:"warnings"`
}

type cfDiscoveryProtection struct {
	zones map[string]bool
	hosts []string
	base  string
}

func (a *App) cfDiscoveryProtectedNames() cfDiscoveryProtection {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := cfDiscoveryProtection{zones: map[string]bool{}, base: cfDomain(a.baseDomain())}
	p.hosts = append(p.hosts, cfDomain(a.baseDomain()))
	for _, account := range a.state.CloudflareAccounts {
		p.zones[account.ZoneID] = true
		p.hosts = append(p.hosts, cfDomain(account.FallbackDomain), cfDomain(account.StandardTarget))
	}
	for _, service := range a.state.Services {
		for _, host := range service.Domains {
			p.hosts = append(p.hosts, cfDomain(host))
		}
		if service.UsePreferred {
			p.hosts = append(p.hosts, cfDomain(service.PreferredDomain))
		}
	}
	return p
}

func (p cfDiscoveryProtection) protects(host string) bool {
	for _, reserved := range p.hosts {
		if host == reserved || (strings.HasPrefix(reserved, "*.") && cfZoneContains(reserved[2:], host)) {
			return true
		}
	}
	return false
}

// Discovery only reads cloud state. In particular it never stages credentials,
// creates a preview receipt, enables SaaS, or changes zone-wide SSL settings.
func (a *App) discoverCloudflareAccounts(w http.ResponseWriter, r *http.Request) {
	var in struct {
		APIToken string `json:"apiToken"`
	}
	if !decode(w, r, &in) {
		return
	}
	c := cloudflare.Credentials{APIToken: strings.TrimSpace(in.APIToken)}
	if !c.Valid() {
		fail(w, http.StatusBadRequest, "请填写有效的 Cloudflare API Token，不使用账号密码或 Global API Key")
		return
	}
	api := a.cfAPI(c)
	lister, ok := api.(cfZoneLister)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "当前 Cloudflare 接口不支持自动读取区域")
		return
	}
	// Leave headroom beneath the server's 45-second HTTP write deadline so
	// completed candidates can still be returned when later zones time out.
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	zones, err := lister.Zones(ctx)
	if err != nil {
		fail(w, http.StatusBadRequest, "无法完整读取 Cloudflare 区域，请检查 Token 的区域读取权限、区域范围或稍后重试")
		return
	}
	// Stable ordering makes the default predictable, without favoring a zone
	// just because Cloudflare happened to return it first.
	sort.SliceStable(zones, func(i, j int) bool { return cfDomain(zones[i].Name) < cfDomain(zones[j].Name) })
	protection := a.cfDiscoveryProtectedNames()
	out := cfDiscoveryResult{
		Zones: []cfDiscoveredZone{},
		Warnings: []string{
			"本次仅检测和生成建议；保存只保存本机账号，云端 DNS 和 SaaS 资源仍需在服务发布时确认创建。",
			"只读检测不修改云端，也不能保证写入权限或套餐额度；发布时仅按账号授权自动设置严格 SSL，不购买套餐或迁移域名 DNS。",
		},
	}
	checked := 0
	limited := false
	for _, zone := range zones {
		item := cfDiscoveredZone{ZoneID: zone.ID, ZoneName: cfDomain(zone.Name)}
		switch {
		case !cfZoneIDPattern.MatchString(zone.ID) || !validCFDomain(item.ZoneName):
			item.Message = "区域资料不完整，不能自动配置"
		case zone.Status != "active" || zone.Type != "full":
			item.Message = "区域尚未激活或不是完整接入区域，请先在 Cloudflare 完成接入"
		case protection.zones[zone.ID]:
			item.Message = "此区域已有本机接入配置，请复用已有账号，避免改动共享回源"
		case checked >= cfDiscoveryMaxZones || ctx.Err() != nil:
			item.Message = "本次检测达到时间或区域数量上限，尚未核对；可缩小 Token 区域范围后重新检测"
			limited = true
		default:
			checked++
			zoneCtx, zoneCancel := context.WithTimeout(ctx, 6*time.Second)
			item = discoverCFZone(zoneCtx, api, zone, protection)
			zoneCancel()
		}
		out.Zones = append(out.Zones, item)
	}
	// Prefer a plan that leaves an existing shared origin untouched. A zone
	// needing an explicit replacement remains selectable, not "unavailable".
	for _, item := range out.Zones {
		if item.Eligible && item.FallbackMode != "replace" && !item.RequiresSSLStrict {
			out.RecommendedZoneID = item.ZoneID
			break
		}
	}
	if out.RecommendedZoneID == "" {
		for _, item := range out.Zones {
			if item.Eligible {
				out.RecommendedZoneID = item.ZoneID
				break
			}
		}
	}
	if limited {
		out.Warnings = append(out.Warnings, "未检查的区域不会标为可用；一个区域失败不影响其他已完成检测的区域。")
	}
	if len(zones) == 0 {
		out.Warnings = append(out.Warnings, "此 Token 下没有可见域名。请先将一个自有域名接入 Cloudflare，或调整 Token 的区域范围；阿里云业务域名无需迁移。")
	}
	reply(w, http.StatusOK, out)
}

func cfReadSSLMode(ctx context.Context, api cloudflareAPI, zoneID string) (string, error) {
	mode, err := api.SSLMode(ctx, zoneID)
	if err != nil {
		if cfRetryableRead(err) {
			return "", fmt.Errorf("暂时无法读取 Cloudflare SSL 设置，请稍后重试：%w", err)
		}
		return "", fmt.Errorf("无法读取 SSL 设置，请检查区域设置（Zone Settings）读取权限：%w", err)
	}
	switch mode {
	case "off", "flexible", "full", "strict":
		return mode, nil
	}
	return "", errors.New("Cloudflare 返回未知 SSL 模式，未修改设置")
}

func cfAutomaticZoneReady(ctx context.Context, api cloudflareAPI, zoneID string, autoStrict ...bool) error {
	mode, err := cfReadSSLMode(ctx, api, zoneID)
	if err != nil {
		return err
	}
	if mode != "strict" && (len(autoStrict) == 0 || !autoStrict[0]) {
		return errors.New("此区域需要 Full (strict)，请在边缘账号中授权自动设置；保存账号不会修改云端")
	}
	return nil
}

func cfReusableOriginRecords(records []cloudflare.Record, target string) bool {
	return target != "" && len(records) == 1 && records[0].Type == "CNAME" && records[0].Proxied && cfDomain(records[0].Content) == target
}

func cfAutomaticNamesAvailable(ctx context.Context, api cloudflareAPI, zoneID, fallback, target, base string) (bool, error) {
	for _, pair := range [][2]string{{fallback, base}, {target, fallback}} {
		name, value := pair[0], pair[1]
		// A missing fallback can be returned as HTTP 404 even when SaaS is not
		// enabled. A successful custom-hostname list is separate proof that the
		// API is accessible; a 404/403 from it must not mean "available".
		host, err := api.Host(ctx, zoneID, name)
		if err != nil {
			return false, errors.New("无法读取 SaaS 自定义主机名，请先启用 Cloudflare for SaaS 并检查权限；未创建任何资源")
		}
		if host != nil {
			return false, nil
		}
		records, err := api.Records(ctx, zoneID, name)
		if err != nil {
			return false, errors.New("无法核对接入子域名的 DNS 记录，请检查 DNS 读取权限")
		}
		if len(records) != 0 && !cfReusableOriginRecords(records, value) {
			return false, nil
		}
	}
	return true, nil
}

func discoverCFZone(ctx context.Context, api cloudflareAPI, zone cloudflare.Zone, protection cfDiscoveryProtection) cfDiscoveredZone {
	out := cfDiscoveredZone{ZoneID: zone.ID, ZoneName: cfDomain(zone.Name), DesiredBaseDomain: protection.base, FallbackMode: "create"}
	mode, err := cfReadSSLMode(ctx, api, zone.ID)
	if err != nil {
		out.Message = err.Error()
		return out
	}
	out.SSLMode = mode
	out.RequiresSSLStrict = out.SSLMode != "strict"
	fallback, err := api.Fallback(ctx, zone.ID)
	if err != nil {
		out.Message = "无法核对 SaaS 回源设置，请检查 Cloudflare for SaaS 和自定义主机名权限"
		return out
	}
	out.ExistingFallback, out.FallbackStatus = cfDomain(fallback.Origin), fallback.Status
	if out.ExistingFallback != "" {
		if !validCFDomain(out.ExistingFallback) {
			out.Message = "Cloudflare 返回的现有 Fallback 名称无效，请核对云端设置"
			return out
		}
		if protection.base == "" {
			out.Message = "此区域已有 Fallback；请先在概览设置基站域名，以判断能否复用现有回源"
			return out
		}
		out.FallbackMode = "replace"
		if cfZoneContains(out.ZoneName, out.ExistingFallback) {
			records, readErr := api.Records(ctx, zone.ID, out.ExistingFallback)
			if readErr != nil {
				out.Message = "现有 Fallback 的 DNS 记录暂时无法读取，请重新检测；未修改任何记录"
				return out
			}
			values := []string{}
			for _, record := range records {
				values = append(values, record.Type+" "+record.Content)
			}
			out.ExistingFallbackTarget = strings.Join(values, "；")
			if out.ExistingFallback != out.ZoneName && !protection.protects(out.ExistingFallback) && cfReusableOriginRecords(records, protection.base) {
				out.FallbackMode = "reuse"
			}
		}
	}
	for attempt := 1; attempt <= cfDiscoveryNameAttempts; attempt++ {
		suffix := ""
		if attempt > 1 {
			suffix = fmt.Sprintf("-%d", attempt)
		}
		fallback := "gateway-fallback" + suffix + "." + out.ZoneName
		target := "gateway-customers" + suffix + "." + out.ZoneName
		if out.FallbackMode == "reuse" {
			fallback = out.ExistingFallback
		}
		if out.FallbackMode == "replace" && fallback == out.ExistingFallback {
			continue
		}
		if !validCFDomain(fallback) || !validCFDomain(target) || fallback == target || protection.protects(fallback) || protection.protects(target) {
			continue
		}
		available, err := cfAutomaticNamesAvailable(ctx, api, zone.ID, fallback, target, protection.base)
		if err != nil {
			out.Message = err.Error()
			return out
		}
		if !available {
			continue
		}
		out.Eligible = true
		out.FallbackDomain, out.StandardTarget = fallback, target
		out.Message = "可自动配置，已核对 SSL、SaaS 接口和专用子域名；保存后仍需发布服务"
		if out.FallbackMode == "reuse" {
			out.Message = "现有 Fallback 已指向本机基站，可直接复用；不修改现有回源和 DNS 记录"
		}
		if out.FallbackMode == "replace" {
			out.Message = "此区域可以接入，但现有 Fallback 不符合本机回源要求；保存后须在边缘加速预览中单独确认共享回源替换，原 DNS 记录保留"
		}
		return out
	}
	out.Message = "专用子域名已被占用或与本机服务冲突，未覆盖任何记录；请改用其他区域或高级配置"
	return out
}

// validateCFAutoConfiguration rechecks the proposed names at save time rather
// than trusting a stale discovery result. Call before taking a.mu; it snapshots
// local protection itself. Publishing still independently checks every write.
type cfFallbackSelection struct{ Mode, Origin string }

func (a *App) validateCFAutoConfiguration(ctx context.Context, api cloudflareAPI, account model.CloudflareAccount, selection ...cfFallbackSelection) error {
	if err := cfProfileValid(account); err != nil {
		return err
	}
	protection := a.cfDiscoveryProtectedNames()
	if protection.zones[account.ZoneID] || protection.protects(account.FallbackDomain) || protection.protects(account.StandardTarget) {
		return errors.New("自动配置与已有账号、基站或业务域名冲突，请重新检测")
	}
	zone, err := api.Zone(ctx, account.ZoneID)
	if err != nil || zone.ID != account.ZoneID || cfDomain(zone.Name) != account.ZoneName || zone.Status != "active" || zone.Type != "full" {
		return errors.New("Cloudflare 区域状态已变化或无法读取，请重新检测")
	}
	if err := cfAutomaticZoneReady(ctx, api, account.ZoneID, account.AutoSSLStrict); err != nil {
		return err
	}
	expected := cfFallbackSelection{Mode: "create"}
	if len(selection) > 0 {
		expected = selection[0]
		if expected.Mode == "" {
			expected.Mode = "create"
		}
	}
	fallback, err := api.Fallback(ctx, account.ZoneID)
	if err != nil {
		return errors.New("无法核对 SaaS 当前 Fallback，请重新检测")
	}
	current := cfDomain(fallback.Origin)
	if current != cfDomain(expected.Origin) {
		return errors.New("现有共享 Fallback 已变化，请重新检测；未保存账号或修改云端")
	}
	switch expected.Mode {
	case "create":
		if current != "" {
			return errors.New("共享 Fallback 已存在，请重新检测复用或替换方案")
		}
	case "reuse":
		if current != account.FallbackDomain || protection.base == "" {
			return errors.New("现有 Fallback 不再满足复用方案，请重新检测")
		}
		records, err := api.Records(ctx, account.ZoneID, current)
		if err != nil || !cfReusableOriginRecords(records, protection.base) {
			return errors.New("现有 Fallback 回源已变化，不能按旧方案复用，请重新检测")
		}
	case "replace":
		if current == "" || current == account.FallbackDomain || protection.base == "" {
			return errors.New("共享 Fallback 替换方案已变化，请重新检测")
		}
	default:
		return errors.New("无效的 Fallback 配置方案，请重新检测")
	}
	available, err := cfAutomaticNamesAvailable(ctx, api, account.ZoneID, account.FallbackDomain, account.StandardTarget, protection.base)
	if err != nil {
		return err
	}
	if !available {
		return errors.New("建议的接入子域名已被占用，请重新检测；未覆盖任何记录")
	}
	return nil
}

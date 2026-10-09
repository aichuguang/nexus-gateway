package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"w103d/gateway/internal/cloudflare"
	"w103d/gateway/internal/model"
)

type cloudflareAPI interface {
	Zone(context.Context, string) (cloudflare.Zone, error)
	SSLMode(context.Context, string) (string, error)
	Records(context.Context, string, string) ([]cloudflare.Record, error)
	CreateRecord(context.Context, string, cloudflare.Record) (cloudflare.Record, error)
	Fallback(context.Context, string) (cloudflare.Fallback, error)
	SetFallback(context.Context, string, string) (cloudflare.Fallback, error)
	Host(context.Context, string, string) (*cloudflare.Hostname, error)
	CreateHost(context.Context, string, string) (cloudflare.Hostname, error)
}

func (a *App) cfAPI(c cloudflare.Credentials) cloudflareAPI {
	if a.cloudflareClient != nil {
		return a.cloudflareClient
	}
	return cloudflare.New(c)
}
func (a *App) cloudflareCredentialPath(id string) string {
	ref := ""
	if index := a.cloudflareAccountIndex(id); index >= 0 {
		ref = a.state.CloudflareAccounts[index].CredentialFile
	}
	return filepath.Join(a.opts.DataDir, "cloudflare-accounts", credentialFilename(id, ref))
}
func (a *App) cloudflareCredentials(id string) (cloudflare.Credentials, error) {
	var c cloudflare.Credentials
	err := readDNSJSON(a.cloudflareCredentialPath(id), &c)
	if err == nil && !c.Valid() {
		err = errors.New("Cloudflare 凭据无效")
	}
	return c, err
}
func (a *App) cloudflareAccountIndex(id string) int {
	for i, v := range a.state.CloudflareAccounts {
		if v.ID == id {
			return i
		}
	}
	return -1
}

var cfZoneIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)
var cfLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func cfDomain(raw string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
}
func validCFDomain(s string) bool {
	if len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if !cfLabelPattern.MatchString(l) {
			return false
		}
	}
	return true
}
func cfZoneContains(zone, host string) bool { return host == zone || strings.HasSuffix(host, "."+zone) }
func cfProfileValid(account model.CloudflareAccount) error {
	if !cfZoneIDPattern.MatchString(account.ZoneID) || !validCFDomain(account.ZoneName) {
		return errors.New("Cloudflare Zone ID 或区域名称无效")
	}
	if !validCFDomain(account.FallbackDomain) || !validCFDomain(account.StandardTarget) || !cfZoneContains(account.ZoneName, account.FallbackDomain) || !cfZoneContains(account.ZoneName, account.StandardTarget) || account.FallbackDomain == account.StandardTarget {
		return errors.New("Fallback 和标准入口必须是 CF 接入区域内两个不同的完整域名")
	}
	if account.FallbackDomain == account.ZoneName || account.StandardTarget == account.ZoneName {
		return errors.New("请使用接入区域下的专用子域名，不使用根域名作为 SaaS 入口")
	}
	return nil
}

func (a *App) listCloudflareAccounts(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	accounts := a.state.CloudflareAccounts
	if accounts == nil {
		accounts = []model.CloudflareAccount{}
	}
	reply(w, 200, map[string]any{"provider": "cloudflare", "accounts": accounts, "baseDomain": a.baseDomain()})
}

func (a *App) createCloudflareAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name             string `json:"name"`
		APIToken         string `json:"apiToken"`
		ZoneID           string `json:"zoneId"`
		FallbackDomain   string `json:"fallbackDomain"`
		StandardTarget   string `json:"standardTarget"`
		Enabled          *bool  `json:"enabled"`
		AutoConfigure    bool   `json:"autoConfigure"`
		AutoSSLStrict    bool   `json:"autoSSLStrict"`
		FallbackMode     string `json:"fallbackMode"`
		ExistingFallback string `json:"existingFallback"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.ZoneID = strings.TrimSpace(in.ZoneID)
	c := cloudflare.Credentials{APIToken: strings.TrimSpace(in.APIToken)}
	if in.Name == "" || len(in.Name) > 100 || !cfZoneIDPattern.MatchString(in.ZoneID) || !c.Valid() {
		fail(w, 400, "请填写名称、32 位 Zone ID 和有效 API Token")
		return
	}
	if a.opts.DryRun {
		fail(w, 403, "dry-run 模式禁止保存云端凭据")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	zone, err := a.cfAPI(c).Zone(ctx, in.ZoneID)
	if err != nil {
		fail(w, 400, "无法读取 Cloudflare 区域，请检查 Zone Read 权限："+err.Error())
		return
	}
	if zone.ID != in.ZoneID || zone.Status != "active" || zone.Type != "full" {
		fail(w, 400, "需要已激活、完整接入 Cloudflare 的 SaaS 区域（非 Partial 区域）")
		return
	}
	account := model.CloudflareAccount{ID: creationResourceID(r), Name: in.Name, ZoneID: zone.ID, ZoneName: cfDomain(zone.Name), FallbackDomain: cfDomain(in.FallbackDomain), StandardTarget: cfDomain(in.StandardTarget), TokenHint: "••••••", CredentialVersion: 1, Enabled: true}
	account.AutoSSLStrict = in.AutoSSLStrict
	if in.Enabled != nil {
		account.Enabled = *in.Enabled
	}
	if err := cfProfileValid(account); err != nil {
		fail(w, 400, err.Error())
		return
	}
	var discoveryRevision int64
	if in.AutoConfigure {
		a.mu.Lock()
		discoveryRevision = a.state.Revision
		a.mu.Unlock()
		if err := a.validateCFAutoConfiguration(ctx, a.cfAPI(c), account, cfFallbackSelection{Mode: in.FallbackMode, Origin: in.ExistingFallback}); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
	}
	a.cloudflareMu.Lock()
	defer a.cloudflareMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if in.AutoConfigure && a.state.Revision != discoveryRevision {
		fail(w, http.StatusConflict, "检测期间本机配置已变化，请重新读取并自动配置")
		return
	}
	if len(a.state.CloudflareAccounts) >= 16 {
		fail(w, 400, "Cloudflare 接入配置最多 16 个")
		return
	}
	for _, v := range a.state.CloudflareAccounts {
		if v.ZoneID == account.ZoneID {
			fail(w, 409, "该 Cloudflare 区域已经添加；每个区域共享唯一 fallback，请复用已有配置")
			return
		}
	}
	if err := writeJSON(a.cloudflareCredentialPath(account.ID), c); err != nil {
		fail(w, 500, "无法安全保存 Cloudflare 凭据")
		return
	}
	old := clone(a.state)
	a.state.CloudflareAccounts = append(a.state.CloudflareAccounts, account)
	a.state.Revision++
	if err := a.saveLocked(); err != nil {
		a.state = old
		_ = os.Remove(a.cloudflareCredentialPath(account.ID))
		fail(w, 500, "无法保存配置")
		return
	}
	a.cloudflarePending = nil
	a.replyStateLocked(w)
}

func (a *App) updateCloudflareAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          *string `json:"name"`
		Enabled       *bool   `json:"enabled"`
		APIToken      *string `json:"apiToken"`
		AutoSSLStrict *bool   `json:"autoSSLStrict"`
	}
	if !decode(w, r, &in) {
		return
	}
	if a.opts.DryRun && in.APIToken != nil {
		fail(w, 403, "dry-run 模式禁止保存云端凭据")
		return
	}
	a.cloudflareMu.Lock()
	defer a.cloudflareMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	idx := a.cloudflareAccountIndex(r.PathValue("id"))
	if idx < 0 {
		fail(w, 404, "Cloudflare 接入配置不存在")
		return
	}
	next := a.state.CloudflareAccounts[idx]
	if in.AutoSSLStrict != nil {
		next.AutoSSLStrict = *in.AutoSSLStrict
	}
	if in.Name != nil {
		next.Name = strings.TrimSpace(*in.Name)
		if next.Name == "" || len(next.Name) > 100 {
			fail(w, 400, "名称不能为空且不能超过 100 字节")
			return
		}
	}
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	if in.APIToken != nil {
		c := cloudflare.Credentials{APIToken: strings.TrimSpace(*in.APIToken)}
		if !c.Valid() {
			fail(w, 400, "API Token 格式无效")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		revision := a.state.Revision
		a.mu.Unlock()
		zone, err := a.cfAPI(c).Zone(ctx, next.ZoneID)
		a.mu.Lock()
		if a.state.Revision != revision {
			fail(w, 409, "Configuration changed during credential validation; retry from the current account")
			return
		}
		if err != nil || zone.ID != next.ZoneID {
			fail(w, 400, "新凭据无法读取当前 Cloudflare 区域")
			return
		}
		next.CredentialFile, err = a.stageCredential("cloudflare", next.ID, c)
		if err != nil {
			fail(w, 500, "无法保存新凭据")
			return
		}
		next.CredentialVersion++
	}
	old := clone(a.state)
	a.state.CloudflareAccounts[idx] = next
	a.state.Revision++
	if err := a.saveLocked(); err != nil {
		a.state = old
		fail(w, 500, "无法保存配置")
		return
	}
	a.cloudflarePending = nil
	a.replyStateLocked(w)
}

func (a *App) deleteCloudflareAccount(w http.ResponseWriter, r *http.Request) {
	a.cloudflareMu.Lock()
	defer a.cloudflareMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	id := r.PathValue("id")
	if pending, err := a.cfDelegationPending("", id); err != nil || pending {
		fail(w, 409, "该账号有待核对的 DCV 委派转换，暂不能删除")
		return
	}
	deletions, err := a.readServiceDeletions()
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	for _, entry := range deletions.Entries {
		if entry.Completed {
			continue
		}
		for _, target := range entry.Cloudflare {
			if target.AccountID == id && !target.Done {
				fail(w, 409, "该账号仍被未完成的服务删除引用，请先完成删除核对")
				return
			}
		}
	}
	idx := a.cloudflareAccountIndex(id)
	if idx < 0 {
		fail(w, 404, "Cloudflare 接入配置不存在")
		return
	}
	for _, s := range a.state.Services {
		if s.ServesCloudflare() && s.EdgeAccountID == id {
			fail(w, 409, "该配置仍被服务引用，请先迁移服务入口")
			return
		}
	}
	if applied, err := a.publicAppliedState(); err == nil {
		for _, s := range applied.Services {
			if s.ServesCloudflare() && s.EdgeAccountID == id {
				fail(w, 409, "已应用配置仍引用该 CF 账号；请先发布迁移后的服务配置")
				return
			}
		}
	}
	j, err := a.readCloudflareLedger()
	if err != nil {
		fail(w, 409, "无法读取 Cloudflare 资源记录")
		return
	}
	// Only forget resources that the operator has already removed in the
	// control panel. This route does not delete shared SaaS resources remotely.
	account := a.state.CloudflareAccounts[idx]
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var api cloudflareAPI
	for k, v := range j.Resources {
		if !strings.HasPrefix(k, id+"/") {
			continue
		}
		if v.Pending {
			fail(w, 409, "该配置仍有结果未知的 CF 写入，请先恢复发布以只读核对结果")
			return
		}
		if api == nil {
			c, e := a.cloudflareCredentials(id)
			if e != nil {
				fail(w, 409, "无法读取凭据以核对剩余 CF 资源")
				return
			}
			api = a.cfAPI(c)
		}
		exists := true
		switch v.Kind {
		case "ssl":
			// A completed settings change is not a resource to delete. Removing
			// credentials must never downgrade the zone's encryption mode.
			exists = false
		case "hostname":
			h, e := api.Host(ctx, account.ZoneID, v.Value)
			err = e
			exists = h != nil
		case "fallback":
			f, e := api.Fallback(ctx, account.ZoneID)
			err = e
			exists = f.Origin != ""
		case "fallback-dns", "target-dns":
			name := account.FallbackDomain
			if v.Kind == "target-dns" {
				name = account.StandardTarget
			}
			records, e := api.Records(ctx, account.ZoneID, name)
			err = e
			exists = len(records) > 0
		default:
			err = errors.New("未知的 CF 资源类型")
		}
		if err != nil {
			fail(w, 409, "无法确认 CF 资源已经移除："+err.Error())
			return
		}
		if exists {
			fail(w, 409, "该配置仍有 Cloudflare 资源；请先在控制台移除本配置的主机名、共享 fallback 和接入 DNS 后再删除账号（本操作不自动删除云资源）")
			return
		}
		delete(j.Resources, k)
	}
	for k := range j.ReadyHosts {
		if strings.HasPrefix(k, id+"/") {
			delete(j.ReadyHosts, k)
		}
	}
	if err := a.saveCloudflareLedger(j); err != nil {
		fail(w, 500, "无法保存 CF 资源核对记录")
		return
	}
	old := clone(a.state)
	a.state.CloudflareAccounts = append(a.state.CloudflareAccounts[:idx], a.state.CloudflareAccounts[idx+1:]...)
	a.state.Revision++
	if err := a.saveLocked(); err != nil {
		a.state = old
		fail(w, 500, "无法保存配置")
		return
	}
	_ = os.Remove(filepath.Join(a.opts.DataDir, "cloudflare-accounts", credentialFilename(id, old.CloudflareAccounts[idx].CredentialFile)))
	a.cloudflarePending = nil
	a.replyStateLocked(w)
}

func (a *App) cloudflareBaseChangeRefusalLocked(next string) string {
	if cfDomain(next) == a.baseDomain() {
		return ""
	}
	if a.baseDomain() != "" {
		for _, account := range a.state.CloudflareAccounts {
			if account.Enabled {
				return "Cloudflare 接入配置正在使用当前基站域名；请先停用并迁移云端回源，再更改基站域名"
			}
		}
	}
	j, err := a.readCloudflareLedger()
	if err != nil {
		return "无法读取 Cloudflare 回源记录，不能安全更改基站域名"
	}
	for _, v := range j.Resources {
		if v.Kind == "fallback-dns" && v.Value == a.baseDomain() {
			return "Cloudflare SaaS 回源仍引用当前基站域名，请先在云端迁移并核对记录"
		}
	}
	for _, v := range j.ReadyHosts {
		if v.BaseDomain == a.baseDomain() {
			return "Cloudflare 接入记录仍引用当前基站域名，请先迁移并清理对应接入配置"
		}
	}
	return ""
}

func (a *App) cfTargetForHostConfigLocked(host string) (string, error) {
	s, ok := a.serviceForHost(cfDomain(host))
	if !ok || !s.ServesCloudflare() {
		return "", errors.New("该域名不是 Cloudflare 服务")
	}
	idx := a.cloudflareAccountIndex(s.EdgeAccountID)
	if idx < 0 || !a.state.CloudflareAccounts[idx].Enabled {
		return "", errors.New("Cloudflare 接入配置不存在或已禁用")
	}
	account := a.state.CloudflareAccounts[idx]
	if err := cfProfileValid(account); err != nil {
		return "", err
	}
	target := account.StandardTarget
	if s.UsePreferred {
		target = cfDomain(s.PreferredDomain)
		if !validCFDomain(target) {
			return "", errors.New("请填写有效的第三方优选域名")
		}
	}
	if target == cfDomain(host) || target == a.baseDomain() || target == account.FallbackDomain {
		return "", errors.New("CF 访问入口不能指向业务域名自身、基站或 fallback，以免形成循环")
	}
	for _, v := range a.state.Services {
		for _, name := range v.Domains {
			if target == cfDomain(name) {
				return "", errors.New("优选入口不能引用本机其他业务域名")
			}
		}
	}
	return target, nil
}

func (a *App) cloudflareTargetForHostLocked(host string) (string, error) {
	target, err := a.cfTargetForHostConfigLocked(host)
	if err != nil {
		return "", err
	}
	s, _ := a.serviceForHost(cfDomain(host))
	j, err := a.readCloudflareLedger()
	if err != nil {
		return "", err
	}
	b, ok := j.ReadyHosts[s.EdgeAccountID+"/"+cfDomain(host)]
	idx := a.cloudflareAccountIndex(s.EdgeAccountID)
	checked, checkErr := time.Parse(time.RFC3339, b.CheckedAt)
	if !ok || idx < 0 || b.AccountVersion != a.state.CloudflareAccounts[idx].CredentialVersion || checkErr != nil || time.Since(checked) > 5*time.Minute || time.Until(checked) > time.Minute || b.BaseDomain != a.baseDomain() || b.Target != target || !b.Ready {
		return "", errors.New("Cloudflare 主机名、边缘证书或回源尚未就绪，请先发布 CF 接入；不会提前修改业务 CNAME")
	}
	return target, nil
}

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"w103d/gateway/internal/cloudflare"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

type cloudflareResource struct {
	StartedAt   string `json:"startedAt,omitempty"`
	ZoneID      string `json:"zoneId,omitempty"`
	Kind        string `json:"kind"`
	ID          string `json:"id,omitempty"`
	Value       string `json:"value"`
	BeforeValue string `json:"beforeValue,omitempty"`
	Pending     bool   `json:"pending,omitempty"`
	Owned       bool   `json:"owned,omitempty"`
}
type cloudflareBinding struct {
	BaseDomain     string `json:"baseDomain"`
	Target         string `json:"target"`
	Ready          bool   `json:"ready"`
	CheckedAt      string `json:"checkedAt,omitempty"`
	AccountVersion int64  `json:"accountVersion,omitempty"`
}
type cloudflareLedger struct {
	Resources           map[string]cloudflareResource  `json:"resources"`
	ReadyHosts          map[string]cloudflareBinding   `json:"readyHosts"`
	FallbackHistory     []cloudflareFallbackHistory    `json:"fallbackHistory,omitempty"`
	ValidationRefreshes map[string]cfValidationRefresh `json:"validationRefreshes,omitempty"`
}

type cloudflareFallbackHistory struct {
	Key      string             `json:"key"`
	Resource cloudflareResource `json:"resource"`
}

func (a *App) readCloudflareLedger() (cloudflareLedger, error) {
	j := cloudflareLedger{}
	err := readDNSJSON(filepath.Join(a.opts.DataDir, "cloudflare-ledger.json"), &j)
	missing := errors.Is(err, os.ErrNotExist)
	if missing {
		err = nil
	}
	if err == nil && !missing && (j.Resources == nil || j.ReadyHosts == nil) {
		return j, errors.New("invalid Cloudflare ledger")
	}
	if j.Resources == nil {
		j.Resources = map[string]cloudflareResource{}
	}
	if j.ReadyHosts == nil {
		j.ReadyHosts = map[string]cloudflareBinding{}
	}
	return j, err
}
func (a *App) saveCloudflareLedger(j cloudflareLedger) error {
	return saveCloudLedger(filepath.Join(a.opts.DataDir, "cloudflare-ledger.json"), j)
}

type cfValidation struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}
type cloudflareHostPlan struct {
	DCVRefreshDue          bool                             `json:"-"`
	Host                   string                           `json:"host"`
	ServiceID              string                           `json:"serviceId"`
	ServiceName            string                           `json:"serviceName"`
	AccountID              string                           `json:"accountId"`
	StandardTarget         string                           `json:"standardTarget"`
	PreferredDomain        string                           `json:"preferredDomain"`
	UsePreferred           bool                             `json:"usePreferred"`
	Target                 string                           `json:"target"`
	HostnameStatus         string                           `json:"hostnameStatus"`
	CertificateStatus      string                           `json:"certificateStatus"`
	OriginStatus           string                           `json:"originStatus"`
	Status                 string                           `json:"status"`
	Message                string                           `json:"message"`
	ValidationRecords      []cfValidation                   `json:"validationRecords"`
	DCVDelegationRecords   []cloudflare.DCVDelegationRecord `json:"dcvDelegationRecords,omitempty"`
	Ready                  bool                             `json:"ready"`
	RemoteChanges          int                              `json:"remoteChanges"`
	RequiresReconciliation bool                             `json:"requiresReconciliation"`
	Steps                  []edgeStep                       `json:"steps"`
	Warnings               []string                         `json:"warnings"`
	Token                  string                           `json:"token,omitempty"`
	Revision               int64                            `json:"revision"`
}
type cloudflarePreview struct {
	Host            string
	Revision        int64
	AppliedRevision int64
	AccountVersion  int64
	Plan            cloudflareHostPlan
	Expires         time.Time
}

func cfValidations(h *cloudflare.Hostname) ([]cfValidation, error) {
	out := []cfValidation{}
	if h == nil {
		return out, nil
	}
	txt := func(name, value string) (cfValidation, error) {
		name = cfDomain(name)
		if !strings.HasSuffix(name, "."+cfDomain(h.Hostname)) || (!strings.HasPrefix(name, "_acme-challenge.") && !strings.HasPrefix(name, "_cf-custom-hostname.")) {
			return cfValidation{}, errors.New("CF 返回了不属于业务域名的验证记录，已拒绝写入")
		}
		if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
			return cfValidation{}, errors.New("CF 验证 TXT 内容无效")
		}
		return cfValidation{Type: "TXT", Name: name, Value: value}, nil
	}
	add := func(name, value string) error {
		v, err := txt(name, value)
		if err != nil {
			return err
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
		return nil
	}
	if h.Ownership.Type != "" {
		if !strings.EqualFold(h.Ownership.Type, "txt") {
			return nil, errors.New("需要 TXT 主机名验证，不支持自动 HTTP 或委派验证")
		}
		if err := add(h.Ownership.Name, h.Ownership.Value); err != nil {
			return nil, err
		}
	}
	for _, r := range h.SSL.ValidationRecords {
		if r.HTTPURL != "" || r.HTTPBody != "" {
			if r.TXTName != "" || r.TXTValue != "" {
				if _, err := txt(r.TXTName, r.TXTValue); err != nil {
					return nil, err
				}
			}
			if err := validateCFHTTPChallenge(h.Hostname, r); err != nil {
				return nil, err
			}
			// Automatic HTTP DCV can appear before ssl.method changes from txt.
			// Cloudflare serves these challenges; never turn them into DNS writes
			// or fetch a URL supplied by the provider response.
			continue
		}
		if err := add(r.TXTName, r.TXTValue); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func validateCFHTTPChallenge(host string, record cloudflare.ValidationRecord) error {
	if len(record.HTTPURL) > 8192 || record.HTTPBody == "" || len(record.HTTPBody) > 16384 || strings.ContainsRune(record.HTTPBody, '\x00') {
		return errors.New("CF HTTP 验证信息不完整或无效")
	}
	u, err := url.Parse(record.HTTPURL)
	if err != nil || u.Scheme != "http" || u.User != nil || cfDomain(u.Hostname()) != cfDomain(host) || (u.Port() != "" && u.Port() != "80") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return errors.New("CF HTTP 验证地址不属于业务域名或格式无效")
	}
	for _, prefix := range []string{"/.well-known/acme-challenge/", "/.well-known/pki-validation/"} {
		if leaf, found := strings.CutPrefix(u.Path, prefix); found && leaf != "" && leaf != "." && leaf != ".." && !strings.ContainsAny(leaf, "/\\") {
			return nil
		}
	}
	return errors.New("CF HTTP 验证地址不是受支持的验证路径")
}

// cfReconcileLocked is called under cloudflareMu, dnsMu and mu, in that order.
// It never changes a foreign DNS record, custom-origin setting or zone-wide SSL
// setting. Replacing shared fallback requires an explicitly approved preview.
// Every attempted cloud mutation is journaled before its one network write.
func (a *App) cfReconcileLocked(ctx context.Context, rawHost string, act, requireApplied bool) (cloudflareHostPlan, error) {
	return a.cfReconcileApprovedLocked(ctx, rawHost, act, requireApplied, nil)
}

func (a *App) cfReconcileApprovedLocked(ctx context.Context, rawHost string, act, requireApplied bool, approved *cloudflareHostPlan) (cloudflareHostPlan, error) {
	host := cfDomain(rawHost)
	p := cloudflareHostPlan{Host: host, Steps: []edgeStep{}, Warnings: []string{}, ValidationRecords: []cfValidation{}, Status: "pending", Revision: a.state.Revision}
	s, ok := a.serviceForHost(host)
	if !ok || !s.ServesCloudflare() {
		return p, errors.New("域名不属于 Cloudflare 服务")
	}
	if pending, err := a.cfDelegationPending(host, ""); err != nil {
		return p, err
	} else if pending {
		return p, errors.New("证书委派转换尚未完成，已阻止普通发布补写 TXT；请先核对原委派操作")
	}
	p.ServiceID, p.ServiceName, p.AccountID = s.ID, s.Name, s.EdgeAccountID
	p.UsePreferred, p.PreferredDomain = s.UsePreferred, s.PreferredDomain
	idx := a.cloudflareAccountIndex(s.EdgeAccountID)
	if idx < 0 || !a.state.CloudflareAccounts[idx].Enabled {
		return p, errors.New("Cloudflare 接入配置不存在或已禁用")
	}
	account := a.state.CloudflareAccounts[idx]
	p.StandardTarget = account.StandardTarget
	target, err := a.cfTargetForHostConfigLocked(host)
	if err != nil {
		return p, err
	}
	p.Target = target
	if s.UsePreferred {
		p.Warnings = append(p.Warnings, "第三方优选域名兼容性尚待真实 IPv4 网络验收；不会扫描 IP 或承诺全国提速。关闭优选后可恢复标准入口。")
	}
	p.Warnings = append(p.Warnings, "使用严格 HTTPS 回源；全区 SSL 模式仅在账号明确授权后自动设置。边缘证书与源站证书独立管理。")
	if !s.Enabled {
		return p, errors.New("服务已禁用")
	}
	if a.baseDomain() == "" || !validCFDomain(a.baseDomain()) {
		return p, errors.New("请先设置基站域名并发布 AAAA 解析")
	}
	if host == account.StandardTarget || host == account.FallbackDomain || a.baseDomain() == account.StandardTarget || a.baseDomain() == account.FallbackDomain {
		return p, errors.New("业务、基站、标准入口和 fallback 必须相互独立")
	}
	if s.UsePreferred {
		if err := a.checkCFPreferredLocked(ctx, target, account.FallbackDomain); err != nil {
			return p, err
		}
	}
	if blocked, err := a.deletionBlocksHost(host); err != nil {
		return p, err
	} else if blocked {
		return p, errors.New("服务删除操作尚未完成，禁止重新接入 CF")
	}
	if requireApplied {
		if _, err := a.appliedPublicAddressLocked(); err != nil {
			return p, err
		}
		if a.state.AppliedAt == "" || a.state.AppliedRevision < 1 {
			return p, errors.New("请先应用本机配置，再接入 Cloudflare")
		}
		applied, err := a.appliedSnapshotLocked()
		if err != nil {
			return p, err
		}
		if servedUnapplied(a.state, applied) {
			return p, errors.New("请先应用当前配置草稿")
		}
		ci := a.certIndex(s.CertificateID)
		if ci < 0 || !a.state.Certificates[ci].Ready {
			return p, errors.New("源站没有可用证书")
		}
		if err := engine.CertificateCoversAll(a.state.Certificates[ci], []string{host}); err != nil {
			return p, err
		}
	}
	if act && a.opts.DryRun {
		return p, errors.New("dry-run 模式禁止 Cloudflare 和 DNS 写入")
	}
	c, err := a.cloudflareCredentials(account.ID)
	if err != nil {
		return p, errors.New("无法读取 Cloudflare 凭据")
	}
	api := a.cfAPI(c)
	changeSSL, err := a.cfEnsureStrictLocked(ctx, api, account, act)
	if err != nil {
		return p, err
	}
	if changeSSL {
		p.Steps = append(p.Steps, edgeStep{Action: "set-ssl-strict", Target: account.ZoneName, To: "strict", Detail: "按账号授权设置整个接入区为 Full (strict)，包括该区其他代理站点"})
	}
	j, err := a.readCloudflareLedger()
	if err != nil {
		return p, errors.New("无法读取 CF 操作记录，已停止")
	}
	base := a.baseDomain()
	prefix := account.ID + "/"
	readyKey := prefix + host
	createdHere := false
	allow := func(step edgeStep) error {
		if approved == nil {
			return nil
		}
		if slices.Contains(approved.Steps, step) {
			return nil
		}
		if createdHere && step.Action == "ensure-validation-txt" {
			return nil
		}
		return errors.New("CF 状态在执行期间变化，拒绝未获确认的操作；请重新预览")
	}
	// Inspect shared fallback before any writes, including creating dedicated DNS
	// records. Ordinary service publication must never authorize its replacement.
	fallback, err := api.Fallback(ctx, account.ZoneID)
	if err != nil {
		return p, err
	}
	var fallbackStep *edgeStep
	if cfDomain(fallback.Origin) != account.FallbackDomain {
		step := edgeStep{Action: "set-fallback", Target: account.ZoneName, To: account.FallbackDomain, Detail: "设置该 SaaS 区域的共享 fallback origin（影响本区域所有自定义主机名）"}
		if fallback.Origin != "" {
			step = edgeStep{Action: "replace-fallback", Target: account.ZoneName, From: cfDomain(fallback.Origin), To: account.FallbackDomain, Detail: "替换本区域共享 Fallback，影响所有使用默认回源的自定义主机名；原回源 DNS 记录不修改、不删除，旧回源保留在本机操作记录中供恢复"}
			p.Warnings = append(p.Warnings, "共享 Fallback 替换可能改变该区域其他业务的回源；请核对影响并在边缘加速预览中单独确认。")
			if act && approved == nil {
				p.Steps = append(p.Steps, step)
				return p, errors.New("该 CF 区域需要替换共享 Fallback；请到边缘加速预览核对旧回源、新回源及影响范围并单独确认，普通发布不会自动替换")
			}
		}
		fallbackStep = &step
		if act {
			if err := allow(step); err != nil {
				return p, err
			}
		}
	}
	if r := j.Resources[prefix+"fallback"]; r.Pending && (r.Value != account.FallbackDomain || r.ZoneID != "" && r.ZoneID != account.ZoneID || cfDomain(fallback.Origin) != r.Value) {
		return p, errors.New("CF 上次 Fallback 写入结果未知且尚未查到目标回源，禁止盲目重试；请先在控制台核对，本机操作记录保留了旧回源")
	}
	// Invalidate earlier readiness before a provisioning run; only a complete
	// fresh read restores it. DNS automatic convergence cannot race past errors.
	if act {
		if fallbackStep != nil {
			// Shared-origin changes invalidate every cached proof in this account,
			// not just the hostname whose preview initiated the change.
			for key, binding := range j.ReadyHosts {
				if strings.HasPrefix(key, prefix) {
					binding.Ready, binding.CheckedAt = false, ""
					j.ReadyHosts[key] = binding
				}
			}
		}
		j.ReadyHosts[readyKey] = cloudflareBinding{BaseDomain: base}
		if err := a.saveCloudflareLedger(j); err != nil {
			return p, err
		}
	}
	write := func(key, kind, value, beforeValue string, fn func() (string, error)) error {
		if previous, ok := j.Resources[key]; ok && previous.Pending {
			return errors.New("CF 上次写入结果未知且尚未查到资源，禁止盲目重试；请先在控制台核对")
		}
		previous, hadPrevious := j.Resources[key]
		// Keep earlier recovery material when a later setup or replacement uses
		// the same shared-origin key. Persist it atomically with the new intent.
		if hadPrevious && kind == "fallback" && previous.BeforeValue != "" {
			entry := cloudflareFallbackHistory{Key: key, Resource: previous}
			if !slices.Contains(j.FallbackHistory, entry) {
				j.FallbackHistory = append(j.FallbackHistory, entry)
			}
		}
		r := cloudflareResource{Kind: kind, Value: value, BeforeValue: beforeValue, Pending: true, StartedAt: time.Now().UTC().Format(time.RFC3339), ZoneID: account.ZoneID}
		j.Resources[key] = r
		if err := reserveCloudLedger(j); err != nil {
			return err
		}
		if err := a.saveCloudflareLedger(j); err != nil {
			return err
		}
		id, err := fn()
		if err == nil && (id == "" || len(id) > 1024) {
			err = errors.New("CF 创建未返回资源 ID，结果未知；请先核对")
		}
		if err != nil {
			if cloudflare.Rejected(err) {
				delete(j.Resources, key)
				if hadPrevious {
					j.Resources[key] = previous
				}
				if saveErr := a.saveCloudflareLedger(j); saveErr != nil {
					return saveErr
				}
			}
			return err
		}
		r.Pending, r.ID, r.Owned = false, id, true
		j.Resources[key] = r
		return a.saveCloudflareLedger(j)
	}
	ensureRecord := func(kind, name, value string) error {
		key := prefix + kind
		records, err := api.Records(ctx, account.ZoneID, name)
		if err != nil {
			return err
		}
		if len(records) > 0 {
			if len(records) != 1 || records[0].Type != "CNAME" || !records[0].Proxied || cfDomain(records[0].Content) != value {
				return fmt.Errorf("CF 记录 %s 与要求不一致；不会覆盖已有记录，请先核对控制台", name)
			}
			r := j.Resources[key]
			if r.Pending && (r.Value != value || records[0].Comment != "w103d:"+account.ID) {
				return errors.New("CF 记录虽已存在但无法核对上次创建归属，仍为未知结果；未继续写入")
			}
			if r.Pending && r.Value == value && records[0].Comment == "w103d:"+account.ID {
				r.Pending = false
				r.ID = records[0].ID
				r.Owned = true
				if act {
					j.Resources[key] = r
					if err := a.saveCloudflareLedger(j); err != nil {
						return err
					}
				}
			}
			return nil
		}
		step := edgeStep{Action: "create-" + kind, Target: name, To: value, Detail: "创建橙云 CNAME（只在记录不存在时创建）"}
		p.Steps = append(p.Steps, step)
		if !act {
			return nil
		}
		if err := allow(step); err != nil {
			return err
		}
		return write(key, kind, value, "", func() (string, error) {
			r, err := api.CreateRecord(ctx, account.ZoneID, cloudflare.Record{Type: "CNAME", Name: name, Content: value, TTL: 1, Proxied: true, Comment: "w103d:" + account.ID})
			return r.ID, err
		})
	}
	if err := ensureRecord("fallback-dns", account.FallbackDomain, base); err != nil {
		return p, err
	}
	if err := ensureRecord("target-dns", account.StandardTarget, account.FallbackDomain); err != nil {
		return p, err
	}
	if fallbackStep != nil {
		step := *fallbackStep
		p.Steps = append(p.Steps, step)
		if act {
			if err := allow(step); err != nil {
				return p, err
			}
			latest, e := api.Fallback(ctx, account.ZoneID)
			if e != nil {
				return p, e
			}
			if cfDomain(latest.Origin) != step.From {
				return p, errors.New("CF 共享 Fallback 在确认后发生变化，未替换回源；请重新预览")
			}
			err = write(prefix+"fallback", "fallback", account.FallbackDomain, latest.Origin, func() (string, error) {
				f, e := api.SetFallback(ctx, account.ZoneID, account.FallbackDomain)
				if e == nil && cfDomain(f.Origin) != account.FallbackDomain {
					e = errors.New("CF Fallback 写入响应与目标不一致，结果待核对；不会自动重试")
				}
				fallback = f
				return account.FallbackDomain, e
			})
			if err != nil {
				return p, err
			}
		}
	} else if r := j.Resources[prefix+"fallback"]; r.Pending && r.Value == account.FallbackDomain && act {
		r.Pending = false
		r.ID = account.FallbackDomain
		r.Owned = false
		j.Resources[prefix+"fallback"] = r
		if err := a.saveCloudflareLedger(j); err != nil {
			return p, err
		}
	}
	p.OriginStatus = fallback.Status
	h, err := api.Host(ctx, account.ZoneID, host)
	if err != nil {
		return p, err
	}
	hostKey := prefix + "host/" + host
	if h == nil {
		step := edgeStep{Action: "create-hostname", Target: host, Detail: "创建 SaaS 自定义主机名和 TXT 验证证书；使用默认 Host/SNI，并补充本次创建返回的业务域名验证 TXT"}
		p.Steps = append(p.Steps, step)
		if act {
			if err := allow(step); err != nil {
				return p, err
			}
			err = write(hostKey, "hostname", host, "", func() (string, error) {
				created, e := api.CreateHost(ctx, account.ZoneID, host)
				if e == nil {
					h = &created
				}
				return created.ID, e
			})
			if err != nil {
				return p, err
			}
			createdHere = true
		}
	} else {
		if h.CustomOriginServer != "" || h.CustomOriginSNI != "" {
			return p, errors.New("该主机名配置了自定义回源或 SNI，不能复用；请先在控制台核对默认 fallback 模式")
		}
		if r := j.Resources[hostKey]; r.Pending && r.Value == host && act {
			r.Pending = false
			r.ID = h.ID
			r.Owned = false
			j.Resources[hostKey] = r
			if err := a.saveCloudflareLedger(j); err != nil {
				return p, err
			}
			p.Warnings = append(p.Warnings, "已重新查到同名主机名；仅观察复用，不将未知创建结果认定为本机所有，也不自动删除。")
		}
	}
	if h != nil {
		if h.ID == "" || cfDomain(h.Hostname) != host || h.CustomOriginServer != "" || h.CustomOriginSNI != "" {
			return p, errors.New("CF 主机名响应与当前服务不匹配，拒绝发布业务 DNS")
		}
		if r, ok := j.Resources[hostKey]; act && ok && r.Owned && !r.Pending && r.Kind == "hostname" && r.ID == h.ID && r.Value == host && r.ZoneID == "" {
			// Upgrade legacy creation metadata only after the exact resource has
			// been read from this zone. Read-only previews never rewrite history.
			r.ZoneID = account.ZoneID
			j.Resources[hostKey] = r
			if err := a.saveCloudflareLedger(j); err != nil {
				return p, err
			}
		}
		p.HostnameStatus = h.Status
		p.CertificateStatus = h.SSL.Status
		if err := cfProvisioningFailure(h, fallback); err != nil {
			p.Status, p.Message = "failed", err.Error()
			return p, err
		}
		p.ValidationRecords, err = cfValidations(h)
		p.DCVDelegationRecords = h.SSL.DCVDelegationRecords
		if err != nil {
			return p, err
		}
	}
	delegated := false
	if h != nil && (len(p.DCVDelegationRecords) > 0 || slices.ContainsFunc(p.ValidationRecords, func(v cfValidation) bool { return v.Name == "_acme-challenge."+host })) {
		delegated, err = a.cfDelegatedValidationLocked(ctx, host, "_acme-challenge."+host, p.DCVDelegationRecords)
		if err != nil {
			return p, err
		}
		if delegated {
			p.Steps = append(p.Steps, edgeStep{Action: "use-dcv-delegation", Target: "_acme-challenge." + host, To: cfDomain(p.DCVDelegationRecords[0].CNAMETarget), Detail: "只读复用匹配的 Cloudflare 证书验证委派，不追加 TXT 或认领外部记录"})
		}
	}
	for _, v := range p.ValidationRecords {
		if delegated && v.Name == "_acme-challenge."+host {
			continue
		}
		step := edgeStep{Action: "ensure-validation-txt", Target: v.Name, To: v.Value, Detail: "通过独立 DNS 账号保留其他 TXT，仅补充当前 CF 验证值"}
		p.Steps = append(p.Steps, step)
		if act {
			if err := allow(step); err != nil {
				return p, err
			}
			if err := a.ensureDNSTXTLocked(ctx, v.Name, v.Value, account.ID, host); err != nil {
				return p, err
			}
		} else if requireApplied {
			if err := a.previewDNSTXTLocked(ctx, v.Name, v.Value); err != nil {
				return p, err
			}
		}
	}
	// TXT publication can immediately advance validation. Read back once, then
	// return pending instead of polling indefinitely or publishing CNAME early.
	if act && h != nil && (h.Status != "active" || h.SSL.Status != "active") {
		latest, e := api.Host(ctx, account.ZoneID, host)
		if e != nil {
			return p, e
		}
		if latest == nil {
			return p, errors.New("CF 创建后尚未查到主机名，已保留进度，请稍后刷新")
		}
		if latest.ID != h.ID || latest.CustomOriginServer != "" || latest.CustomOriginSNI != "" {
			return p, errors.New("CF 主机名在验证期间变化，请重新预览")
		}
		h = latest
		p.HostnameStatus = h.Status
		p.CertificateStatus = h.SSL.Status
		// Cloudflare can rotate/add a validation token in this response. Preserve
		// the fresh set so the waiting worker can discover and publish only the
		// missing values on its next reconciliation, without removing old TXT.
		p.ValidationRecords, err = cfValidations(h)
		p.DCVDelegationRecords = h.SSL.DCVDelegationRecords
		if err != nil {
			return p, err
		}
		if fallback.Status != "active" {
			latestFallback, e := api.Fallback(ctx, account.ZoneID)
			if e != nil {
				return p, e
			}
			if cfDomain(latestFallback.Origin) != account.FallbackDomain {
				return p, errors.New("CF fallback 在验证期间变化，请重新预览")
			}
			fallback = latestFallback
			p.OriginStatus = fallback.Status
		}
	}
	if err := cfProvisioningFailure(h, fallback); err != nil {
		p.Status, p.Message = "failed", err.Error()
		return p, err
	}
	dcvMessage := ""
	h, p.DCVRefreshDue, dcvMessage, err = a.refreshCloudflareValidation(ctx, api, account, h, &j, act && approved == nil)
	if err != nil {
		return p, err
	}
	if delegated && dcvMessage == "" {
		dcvMessage = "证书验证委派已配置，等待 Cloudflare 自动完成验证与签发"
	}
	if h != nil {
		p.HostnameStatus, p.CertificateStatus = h.Status, h.SSL.Status
		p.ValidationRecords, err = cfValidations(h)
		p.DCVDelegationRecords = h.SSL.DCVDelegationRecords
		if err != nil {
			return p, err
		}
	}
	p.Ready = h != nil && h.Status == "active" && h.SSL.Status == "active" && fallback.Status == "active" && cfDomain(fallback.Origin) == account.FallbackDomain
	// Readiness authorizes DNS publication, not reachability. A user-selected
	// entry must not be blocked by this device's single-network HTTPS probe.
	// The explicit access-check endpoint remains independent of publishing.
	p.RemoteChanges = len(p.Steps)
	if delegated {
		p.RemoteChanges--
	}
	p.RequiresReconciliation = p.Ready
	if p.Ready {
		p.Status = "ready"
		p.Message = "CF 接入已就绪，可按所选入口更新业务解析；访问检测不阻止发布"
	} else {
		p.Message = cfWaitingMessage(h, fallback)
		if dcvMessage != "" {
			p.Message = dcvMessage
		}
		if published, e := a.cfHTTPValidationSubmitted(host, account.ID); e != nil {
			return p, e
		} else if published {
			p.Message = cfHTTPWaitingMessage
		}
	}
	if act {
		j.ReadyHosts[readyKey] = cloudflareBinding{BaseDomain: base, Target: target, Ready: p.Ready, CheckedAt: time.Now().UTC().Format(time.RFC3339), AccountVersion: account.CredentialVersion}
		if err := a.saveCloudflareLedger(j); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (a *App) cloudflareHosts(w http.ResponseWriter, r *http.Request) {
	a.cloudflareMu.Lock()
	defer a.cloudflareMu.Unlock()
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	snapshot := a.cloudReadSnapshotLocked()
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	reply(w, 200, map[string]any{"hosts": snapshot.cloudflareHostViewsLocked(ctx), "revision": snapshot.state.Revision})
}

// Callers hold cloudflareMu, dnsMu and mu. No remote or local writes occur.
func (a *App) cloudflareHostViewsLocked(ctx context.Context) []cloudflareHostPlan {
	hosts := []cloudflareHostPlan{}
	for _, s := range a.state.Services {
		if !s.ServesCloudflare() {
			continue
		}
		for _, host := range s.Domains {
			idx := a.cloudflareAccountIndex(s.EdgeAccountID)
			if !s.Enabled || idx >= 0 && !a.state.CloudflareAccounts[idx].Enabled {
				p := cloudflareHostPlan{Host: cfDomain(host), ServiceID: s.ID, ServiceName: s.Name, AccountID: s.EdgeAccountID, Status: "disabled", Message: "服务或 Cloudflare 账号已禁用，未查询云端状态", UsePreferred: s.UsePreferred, PreferredDomain: s.PreferredDomain, Steps: []edgeStep{}, Warnings: []string{}, ValidationRecords: []cfValidation{}}
				if idx >= 0 {
					p.StandardTarget = a.state.CloudflareAccounts[idx].StandardTarget
					p.Target = p.StandardTarget
					if s.UsePreferred {
						p.Target = s.PreferredDomain
					}
				}
				hosts = append(hosts, p)
				continue
			}
			if ctx.Err() != nil {
				hosts = append(hosts, cloudflareHostPlan{Host: cfDomain(host), ServiceID: s.ID, ServiceName: s.Name, AccountID: s.EdgeAccountID, Status: "unknown", Message: "Cloudflare 状态读取超时或取消，未确认就绪"})
				continue
			}
			p, err := a.cfReconcileLocked(ctx, host, false, false)
			if err != nil {
				p.Status = "error"
				p.Message = err.Error()
			}
			hosts = append(hosts, p)
		}
	}
	return hosts
}

func (a *App) previewCloudflareHost(w http.ResponseWriter, r *http.Request) {
	a.cloudflareMu.Lock()
	defer a.cloudflareMu.Unlock()
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	snapshot := a.cloudReadSnapshotLocked()
	appliedBefore, appliedErr := a.appliedSnapshotLocked()
	if appliedErr != nil {
		fail(w, 409, appliedErr.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	a.mu.Unlock()
	p, err := snapshot.cfReconcileLocked(ctx, r.URL.Query().Get("host"), false, true)
	a.mu.Lock()
	appliedAfter, afterErr := a.appliedSnapshotLocked()
	if afterErr != nil || !reflect.DeepEqual(a.state, snapshot.state) || !reflect.DeepEqual(appliedBefore, appliedAfter) {
		fail(w, 409, "Configuration, runtime or credentials changed during CF preview; inspect again")
		return
	}
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	token, err := dnsRandomToken()
	if err != nil {
		fail(w, 500, "无法生成确认令牌")
		return
	}
	if a.cloudflarePending == nil {
		a.cloudflarePending = map[string]cloudflarePreview{}
	}
	for k, v := range a.cloudflarePending {
		if v.Host == p.Host || time.Now().After(v.Expires) {
			delete(a.cloudflarePending, k)
		}
	}
	if len(a.cloudflarePending) >= 16 {
		fail(w, 429, "待确认操作过多")
		return
	}
	idx := a.cloudflareAccountIndex(p.AccountID)
	if idx < 0 {
		fail(w, 409, "Cloudflare account changed during preview")
		return
	}
	a.cloudflarePending[token] = cloudflarePreview{Host: p.Host, Revision: a.state.Revision, AppliedRevision: a.state.AppliedRevision, AccountVersion: a.state.CloudflareAccounts[idx].CredentialVersion, Plan: p, Expires: time.Now().Add(10 * time.Minute)}
	p.Token = token
	reply(w, 200, p)
}

func (a *App) applyCloudflareHost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Host                       string `json:"host"`
		Token                      string `json:"token"`
		Revision                   int64  `json:"revision"`
		Confirm                    bool   `json:"confirm"`
		ConfirmFallbackReplacement bool   `json:"confirmFallbackReplacement"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		fail(w, 400, "必须确认 Cloudflare 变更")
		return
	}
	if a.opts.DryRun {
		fail(w, 403, "dry-run 模式禁止云端写入")
		return
	}
	a.cloudflareMu.Lock()
	defer a.cloudflareMu.Unlock()
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	pending, ok := a.cloudflarePending[in.Token]
	idx := a.cloudflareAccountIndex(pending.Plan.AccountID)
	if !ok || pending.Host != cfDomain(in.Host) || pending.Revision != in.Revision || pending.Revision != a.state.Revision || pending.AppliedRevision != a.state.AppliedRevision || time.Now().After(pending.Expires) || idx < 0 || a.state.CloudflareAccounts[idx].CredentialVersion != pending.AccountVersion {
		delete(a.cloudflarePending, in.Token)
		fail(w, 409, "CF 预览已失效，请重新预览")
		return
	}
	if !in.ConfirmFallbackReplacement && slices.ContainsFunc(pending.Plan.Steps, func(step edgeStep) bool { return step.Action == "replace-fallback" }) {
		fail(w, 400, "必须单独确认共享 Fallback 替换及其对该区域其他业务的影响")
		return
	}
	delete(a.cloudflarePending, in.Token)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Minute))
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	current, err := a.cfReconcileLocked(ctx, in.Host, false, true)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	// Compare the complete remote plan, not just action names. A changed TXT
	// token, target or fallback is a different authorization.
	x, _ := json.Marshal(current)
	y, _ := json.Marshal(pending.Plan)
	if string(x) != string(y) {
		fail(w, 409, "CF 状态在预览后变化，请重新预览；尚未写入")
		return
	}
	p, err := a.cfReconcileApprovedLocked(ctx, in.Host, true, true, &pending.Plan)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	reply(w, 200, map[string]any{"ok": true, "ready": p.Ready, "message": p.Message, "plan": p})
}

func (a *App) publishCloudflareForService(ctx context.Context, s model.Service, report publishProgress) ([]publishStep, map[string]bool, error) {
	steps := []publishStep{}
	ready := map[string]bool{}
	if !s.ServesCloudflare() || !s.Enabled {
		return steps, ready, nil
	}
	a.cloudflareMu.Lock()
	defer a.cloudflareMu.Unlock()
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, host := range s.Domains {
		step := publishStep{Kind: "cloudflare", Host: cfDomain(host), Role: "CF 加速域名"}
		if err := reportPublish(report, step, "running"); err != nil {
			return steps, ready, err
		}
		p, err := a.cfReconcileLocked(ctx, host, true, true)
		step.Steps = p.Steps
		step.Changes = len(p.Steps) > 0
		status := "failed"
		if err != nil {
			step.Message = err.Error()
			if txt, readErr := a.readDNSTXTJournal(); readErr != nil || txt.Attempt != nil {
				status = "unknown"
			}
			if j, e := a.readCloudflareLedger(); e != nil {
				status = "unknown"
			} else {
				for k, v := range j.Resources {
					if strings.HasPrefix(k, s.EdgeAccountID+"/") && v.Pending {
						status = "unknown"
						break
					}
				}
				if status != "unknown" && ctx.Err() == nil && errors.Is(err, errCFProvisioningRead) {
					status = "waiting"
					step.Message = "暂时无法读取 Cloudflare SSL 设置，系统会自动重查并继续发布；本次未提交 Cloudflare 写入"
					_, observed := j.ReadyHosts[s.EdgeAccountID+"/"+cfDomain(host)]
					_, provisioned := j.Resources[s.EdgeAccountID+"/host/"+cfDomain(host)]
					step.RetryProvisioning = !observed && !provisioned
				}
			}
		} else {
			step.OK = p.Ready
			step.Message = p.Message
			status = "waiting"
			if p.Ready {
				ready[cfDomain(host)] = true
				status = "succeeded"
			}
		}
		steps = append(steps, step)
		if err := reportPublish(report, step, status); err != nil {
			return steps, ready, err
		}
		if status == "unknown" {
			return steps, ready, errors.New("CF 写入结果待核对，已停止后续发布")
		}
	}
	return steps, ready, nil
}

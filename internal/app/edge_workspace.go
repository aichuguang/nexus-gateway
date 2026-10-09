package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"w103d/gateway/internal/model"
)

// The workspace unifies presentation, not credentials or cloud operations.
// Provider-qualified keys avoid collisions in migrated account IDs. Existing
// provider handlers retain their authorization, journaling and deletion guards.
type unifiedEdgeAccount struct {
	Key               string `json:"key"`
	ID                string `json:"id"`
	Provider          string `json:"provider"`
	Name              string `json:"name"`
	Enabled           bool   `json:"enabled"`
	CredentialVersion int64  `json:"credentialVersion"`
	SecretIDHint      string `json:"secretIdHint,omitempty"`
	TokenHint         string `json:"tokenHint,omitempty"`
	OriginMode        string `json:"originMode,omitempty"`
	ZoneID            string `json:"zoneId,omitempty"`
	ZoneName          string `json:"zoneName,omitempty"`
	FallbackDomain    string `json:"fallbackDomain,omitempty"`
	StandardTarget    string `json:"standardTarget,omitempty"`
	AutoSSLStrict     bool   `json:"autoSSLStrict,omitempty"`
}

func (a *App) listUnifiedEdgeAccounts(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	accounts := []unifiedEdgeAccount{}
	for _, v := range a.state.EdgeOneAccounts {
		accounts = append(accounts, unifiedEdgeAccount{Key: "edgeone:" + v.ID, ID: v.ID, Provider: "edgeone", Name: v.Name, Enabled: v.Enabled, CredentialVersion: v.CredentialVersion, SecretIDHint: v.SecretIDHint, OriginMode: effectiveOriginMode(v.OriginMode)})
	}
	for _, v := range a.state.CloudflareAccounts {
		accounts = append(accounts, unifiedEdgeAccount{Key: "cloudflare:" + v.ID, ID: v.ID, Provider: "cloudflare", Name: v.Name, Enabled: v.Enabled, CredentialVersion: v.CredentialVersion, TokenHint: v.TokenHint, ZoneID: v.ZoneID, ZoneName: v.ZoneName, FallbackDomain: v.FallbackDomain, StandardTarget: v.StandardTarget, AutoSSLStrict: v.AutoSSLStrict})
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Name == accounts[j].Name {
			return accounts[i].Key < accounts[j].Key
		}
		return accounts[i].Name < accounts[j].Name
	})
	reply(w, 200, map[string]any{"accounts": accounts, "baseDomain": a.baseDomain(), "originModes": []map[string]string{
		{"value": model.EdgeOriginAddress, "label": "直接使用公网地址（地址变化时由本机更新回源）"},
		{"value": model.EdgeOriginBase, "label": "使用基站域名（EdgeOne 自行解析，本机不更新）"},
	}})
}

func validEdgeProvider(provider string) bool {
	return provider == "edgeone" || provider == "cloudflare"
}

func replaceEdgeRequestBody(r *http.Request, value any) {
	b, _ := json.Marshal(value)
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
}

func (a *App) createUnifiedEdgeAccount(w http.ResponseWriter, r *http.Request) {
	var fields map[string]json.RawMessage
	if !decode(w, r, &fields) {
		return
	}
	var provider string
	if json.Unmarshal(fields["provider"], &provider) != nil || !validEdgeProvider(provider) {
		fail(w, 400, "请选择 EdgeOne 或 Cloudflare 账号类型")
		return
	}
	delete(fields, "provider")
	// Strict provider decoders reject mixed/unknown fields before any API call.
	replaceEdgeRequestBody(r, fields)
	if provider == "cloudflare" {
		a.createCloudflareAccount(w, r)
	} else {
		a.createEdgeOneAccount(w, r)
	}
}

func (a *App) updateUnifiedEdgeAccount(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("provider") {
	case "edgeone":
		a.updateEdgeOneAccount(w, r)
	case "cloudflare":
		a.updateCloudflareAccount(w, r)
	default:
		fail(w, 400, "不支持的边缘账号类型")
	}
}

func (a *App) deleteUnifiedEdgeAccount(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("provider") {
	case "edgeone":
		a.deleteEdgeOneAccount(w, r)
	case "cloudflare":
		a.deleteCloudflareAccount(w, r)
	default:
		fail(w, 400, "不支持的边缘账号类型")
	}
}

type unifiedEdgeHost struct {
	Key               string `json:"key"`
	Host              string `json:"host"`
	Provider          string `json:"provider"`
	ServiceID         string `json:"serviceId"`
	ServiceName       string `json:"serviceName"`
	AccountID         string `json:"accountId,omitempty"`
	AccountName       string `json:"accountName,omitempty"`
	Enabled           bool   `json:"enabled"`
	Status            string `json:"status"`
	Message           string `json:"message"`
	Target            string `json:"target"`
	StandardTarget    string `json:"standardTarget,omitempty"`
	UsePreferred      bool   `json:"usePreferred,omitempty"`
	Origin            string `json:"origin,omitempty"`
	OriginMode        string `json:"originMode,omitempty"`
	CertificateStatus string `json:"certificateStatus"`
	OriginStatus      string `json:"originStatus,omitempty"`
	HostnameStatus    string `json:"hostnameStatus,omitempty"`
	RecordSays        string `json:"recordSays,omitempty"`
	Ready             bool   `json:"ready"`
	Detail            any    `json:"detail"`
}

type edgeWorkspaceError struct {
	Provider string `json:"provider"`
	Message  string `json:"message"`
}

// Keep the locally configured rows visible if a provider lock is busy. The
// snapshot never resolves an account from the network or implies readiness.
func (a *App) unifiedEdgeHostSnapshot(ctx context.Context) []unifiedEdgeHost {
	if !lockContext(ctx, &a.mu) {
		return nil
	}
	defer a.mu.Unlock()
	views := []unifiedEdgeHost{}
	for _, service := range a.state.Services {
		if !service.ServesEdge() {
			continue
		}
		provider := service.EffectiveEdgeProvider()
		if !validEdgeProvider(provider) {
			continue
		}
		for _, host := range service.Domains {
			view := unifiedEdgeHost{Host: cfDomain(host), Provider: provider, ServiceID: service.ID, ServiceName: service.Name, AccountID: service.EdgeAccountID, Enabled: service.Enabled, Status: "unknown", CertificateStatus: "unknown", UsePreferred: service.UsePreferred}
			if provider == "cloudflare" {
				if idx := a.cloudflareAccountIndex(service.EdgeAccountID); idx >= 0 {
					account := a.state.CloudflareAccounts[idx]
					view.AccountName, view.StandardTarget, view.Target = account.Name, account.StandardTarget, account.StandardTarget
					view.Enabled = view.Enabled && account.Enabled
					if service.UsePreferred {
						view.Target = service.PreferredDomain
					}
				}
				view.Origin, view.OriginMode = a.baseDomain(), model.EdgeOriginBase
			} else if len(a.state.EdgeOneAccounts) > 0 {
				anyEnabled := false
				for _, account := range a.state.EdgeOneAccounts {
					anyEnabled = anyEnabled || account.Enabled
				}
				view.Enabled = view.Enabled && anyEnabled
			}
			if !view.Enabled {
				view.Status, view.Message = "disabled", "服务或边缘账号已禁用"
			}
			views = append(views, view)
		}
	}
	return views
}

func fallbackEdgeViews(snapshot []unifiedEdgeHost, provider string, err error) []unifiedEdgeHost {
	views := []unifiedEdgeHost{}
	for _, view := range snapshot {
		if view.Provider != provider {
			continue
		}
		if view.Enabled {
			view.Status, view.Message = "unknown", "状态读取未完成，仅显示本地配置："+err.Error()
		}
		view.Detail = map[string]any{"host": view.Host, "status": view.Status, "message": view.Message, "ready": false}
		views = append(views, view)
	}
	return views
}

func (a *App) unifiedEdgeOneViews(ctx context.Context) ([]unifiedEdgeHost, error) {
	if !lockContext(ctx, &a.edgeMu) {
		return nil, ctx.Err()
	}
	defer a.edgeMu.Unlock()
	if !lockContext(ctx, &a.mu) {
		return nil, ctx.Err()
	}
	snapshot := a.cloudReadSnapshotLocked()
	a.mu.Unlock()
	views := []unifiedEdgeHost{}
	for _, p := range snapshot.edgeHostViewsLocked(ctx) {
		cert := "pending"
		if p.CertBound != "" && p.CertLocal != "" && p.CertBound == p.CertLocal {
			cert = "ready"
		} else if p.Status == "error" || p.Status == "unknown" || p.Status == "disabled" {
			cert = "unknown"
		}
		views = append(views, unifiedEdgeHost{Host: p.Host, Provider: "edgeone", ServiceID: p.ServiceID, ServiceName: p.ServiceName, AccountID: p.AccountID, AccountName: p.AccountName, Enabled: p.Enabled, Status: p.Status, Message: p.Message, Target: p.Cname, Origin: p.Origin, OriginMode: p.OriginMode, CertificateStatus: cert, HostnameStatus: p.DomainState, RecordSays: p.RecordSays, Ready: p.Ready, Detail: p})
	}
	return views, nil
}

func (a *App) unifiedCloudflareViews(ctx context.Context) ([]unifiedEdgeHost, error) {
	if !lockContext(ctx, &a.cloudflareMu) {
		return nil, ctx.Err()
	}
	defer a.cloudflareMu.Unlock()
	if !lockContext(ctx, &a.dnsMu) {
		return nil, ctx.Err()
	}
	defer a.dnsMu.Unlock()
	if !lockContext(ctx, &a.mu) {
		return nil, ctx.Err()
	}
	snapshot := a.cloudReadSnapshotLocked()
	a.mu.Unlock()
	journal, journalErr := snapshot.readDNSJournal()
	views := []unifiedEdgeHost{}
	for _, p := range snapshot.cloudflareHostViewsLocked(ctx) {
		view := unifiedEdgeHost{Host: p.Host, Provider: "cloudflare", ServiceID: p.ServiceID, ServiceName: p.ServiceName, AccountID: p.AccountID, Enabled: p.Status != "disabled", Status: p.Status, Message: p.Message, Target: p.Target, StandardTarget: p.StandardTarget, UsePreferred: p.UsePreferred, Origin: snapshot.baseDomain(), OriginMode: model.EdgeOriginBase, CertificateStatus: p.CertificateStatus, OriginStatus: p.OriginStatus, HostnameStatus: p.HostnameStatus, Ready: p.Ready, Detail: p}
		if view.CertificateStatus == "" {
			view.CertificateStatus = "unknown"
		}
		if idx := snapshot.cloudflareAccountIndex(p.AccountID); idx >= 0 {
			view.AccountName = snapshot.state.CloudflareAccounts[idx].Name
		}
		if record, ok := journal.Managed[p.Host]; ok {
			view.RecordSays = record.Record.Value
		}
		// Provider readiness describes SaaS resources, not public reachability.
		// The mixed list also checks the local DNS ledger before declaring ready.
		if p.Ready {
			switch {
			case journalErr != nil:
				view.Ready, view.Status, view.Message = false, "unknown", "CF 资源已就绪，但无法读取本机解析记录；实际访问仍需外部验收"
			case !sameOriginValue(view.RecordSays, p.Target):
				view.Ready, view.Status, view.Message = false, "pending", "CF 资源已就绪，请发布业务 CNAME；实际 IPv4 访问与优选兼容性仍需外部验收"
			}
		}
		views = append(views, view)
	}
	return views, nil
}

func (a *App) unifiedEdgeHosts(w http.ResponseWriter, r *http.Request) {
	// Independent bounded reads prevent one provider from consuming the entire
	// HTTP budget and hiding the other. No lock spans the two providers.
	snapshotCtx, snapshotCancel := context.WithTimeout(r.Context(), 2*time.Second)
	snapshot := a.unifiedEdgeHostSnapshot(snapshotCtx)
	snapshotCancel()
	hosts, problems := []unifiedEdgeHost{}, []edgeWorkspaceError{}
	for _, source := range []struct {
		provider string
		read     func(context.Context) ([]unifiedEdgeHost, error)
	}{{"edgeone", a.unifiedEdgeOneViews}, {"cloudflare", a.unifiedCloudflareViews}} {
		ctx, cancel := context.WithTimeout(r.Context(), 18*time.Second)
		views, err := source.read(ctx)
		cancel()
		if err != nil {
			hosts = append(hosts, fallbackEdgeViews(snapshot, source.provider, err)...)
			problems = append(problems, edgeWorkspaceError{Provider: source.provider, Message: "状态读取未完成：" + err.Error()})
			continue
		}
		hosts = append(hosts, views...)
		for _, view := range views {
			if view.Status == "error" || view.Status == "unknown" {
				problems = append(problems, edgeWorkspaceError{Provider: source.provider, Message: view.Host + "：" + view.Message})
				break
			}
		}
	}
	for i := range hosts {
		hosts[i].Key = hosts[i].Provider + ":" + hosts[i].Host
	}
	sort.Slice(hosts, func(i, j int) bool {
		if hosts[i].Host == hosts[j].Host {
			return hosts[i].Provider < hosts[j].Provider
		}
		return hosts[i].Host < hosts[j].Host
	})
	reply(w, 200, map[string]any{"hosts": hosts, "errors": problems})
}

func (a *App) unifiedHostProvider(w http.ResponseWriter, host, provider string) bool {
	if !validEdgeProvider(provider) {
		fail(w, 400, "请选择有效的边缘提供商")
		return false
	}
	a.mu.Lock()
	service, ok := a.serviceForHost(cfDomain(host))
	valid := ok && service.ServesEdge() && service.EffectiveEdgeProvider() == provider
	a.mu.Unlock()
	if !valid {
		fail(w, 409, "域名的边缘提供商已变化或不匹配，请刷新后重新预览")
	}
	return valid
}

func (a *App) previewUnifiedEdgeHost(w http.ResponseWriter, r *http.Request) {
	provider, host := r.URL.Query().Get("provider"), r.PathValue("host")
	if !a.unifiedHostProvider(w, host, provider) {
		return
	}
	if provider == "cloudflare" {
		q := r.URL.Query()
		q.Set("host", host)
		r.URL.RawQuery = q.Encode()
		a.previewCloudflareHost(w, r)
	} else {
		a.previewEdgeHost(w, r)
	}
}

func (a *App) applyUnifiedEdgeHost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
		Host     string `json:"host"`
		Token    string `json:"token"`
		Revision int64  `json:"revision"`
		Confirm  bool   `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	if cfDomain(r.PathValue("host")) != cfDomain(in.Host) || strings.TrimSpace(in.Host) == "" {
		fail(w, 400, "请求路径和确认域名不一致")
		return
	}
	if !a.unifiedHostProvider(w, in.Host, in.Provider) {
		return
	}
	// Tokens remain in separate provider stores. A valid token from one provider
	// can never authorize another provider, even when account IDs coincide.
	replaceEdgeRequestBody(r, map[string]any{"host": in.Host, "token": in.Token, "revision": in.Revision, "confirm": in.Confirm})
	if in.Provider == "cloudflare" {
		a.applyCloudflareHost(w, r)
	} else {
		a.applyEdgeHost(w, r)
	}
}

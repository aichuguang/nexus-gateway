package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"w103d/gateway/internal/dnspod"
	"w103d/gateway/internal/model"
	"w103d/gateway/internal/ssl"
	"w103d/gateway/internal/teo"
)

// EdgeOne accounts and the sites they can reach.
//
// An EdgeOne account is not a DNS account, even when the same Tencent Cloud key
// backs both. What a key may do is a CAM policy, and a key that can rewrite a
// DNS record cannot necessarily create an acceleration domain — so the two are
// configured, verified and stored separately, and neither is inferred from the
// other.
//
// Nothing in this file creates or changes anything at EdgeOne. It establishes
// which account can see which site, and which origin group this gateway made,
// which is what every write in edgehost.go is resolved against.

// teoAPI is the EdgeOne surface this gateway uses. It is an interface so that
// the plan-and-apply logic in edgehost.go can be exercised without a cloud
// account, and so that "which EdgeOne calls can this program make" has one
// answer in one place.
type teoAPI interface {
	DescribeDomain(ctx context.Context, zoneID, name string) (*teo.Domain, error)
	CreateDomain(ctx context.Context, zoneID, name, originGroupID string) error
	DeleteDomain(ctx context.Context, zoneID, name string) error
	SetDomainOrigin(ctx context.Context, zoneID, name, originGroupID string) error
	SetDomainsOnline(ctx context.Context, zoneID string, names []string, online bool) error
	DescribeOriginGroup(ctx context.Context, zoneID, groupID string) (*teo.OriginGroup, error)
	CreateOriginGroup(ctx context.Context, zoneID, name, record string) (string, error)
	SetOriginGroupRecord(ctx context.Context, zoneID, groupID, record string) error
	BindCertificate(ctx context.Context, zoneID string, hosts []string, certID string) error
	WebSocketSetting(ctx context.Context, zoneID string) (string, int64, error)
	EnableWebSocket(ctx context.Context, zoneID string, timeout int64) error
	Sites(ctx context.Context) ([]teo.Site, error)
}

// sslAPI uploads a certificate so EdgeOne has something to bind.
type sslAPI interface {
	Upload(ctx context.Context, chain, key, alias string) (string, error)
}

func (a *App) edgeAPI(c teo.Credentials) teoAPI {
	if a.edgeClient != nil {
		return a.edgeClient
	}
	return teo.New(c)
}

func (a *App) certAPI(c teo.Credentials) sslAPI {
	if a.sslClient != nil {
		return a.sslClient
	}
	return ssl.New(c)
}

func (a *App) edgeAccountCredentialPath(id string) string {
	ref := ""
	if index := a.edgeAccountIndex(id); index >= 0 {
		ref = a.state.EdgeOneAccounts[index].CredentialFile
	}
	return filepath.Join(a.opts.DataDir, "edgeone-accounts", credentialFilename(id, ref))
}

func (a *App) edgeAccountCredentials(id string) (teo.Credentials, error) {
	var c teo.Credentials
	err := readDNSJSON(a.edgeAccountCredentialPath(id), &c)
	if err == nil && !c.Valid() {
		err = errors.New("invalid EdgeOne credentials")
	}
	return c, err
}

func (a *App) edgeAccountIndex(id string) int {
	for i, account := range a.state.EdgeOneAccounts {
		if account.ID == id {
			return i
		}
	}
	return -1
}

// effectiveOriginMode resolves the empty origin mode. Empty reads as the address
// mode because that is the one whose effect at EdgeOne is certain: changing it
// is an explicit configuration write, whereas how often EdgeOne re-resolves a
// domain origin is documented nowhere in its API.
func effectiveOriginMode(mode string) string {
	if mode == model.EdgeOriginBase {
		return model.EdgeOriginBase
	}
	return model.EdgeOriginAddress
}

// baseStationRefusalLocked reports why a base-mode origin cannot be configured
// yet, or "" when it can. Callers hold mu.
//
// Shared by account creation, account mode changes and origin-value resolution.
// Removing an existing base name has a separate check against recorded origins.
func (a *App) baseStationRefusalLocked() string {
	if a.baseDomain() == "" {
		return "以基站域名回源需要先设置基站域名；请在概览页设置一个专用名称，或把回源方式改为直接使用公网地址"
	}
	return ""
}

func (a *App) baseStationChangeRefusalLocked(next string) string {
	if refusal := a.cloudflareBaseChangeRefusalLocked(next); refusal != "" {
		return refusal
	}
	current := a.baseDomain()
	if next == current || current == "" && next != "" {
		return ""
	}
	// Changing account settings does not move an existing cloud origin. Keep
	// protecting it even after its account is disabled or switched to address mode.
	for _, origin := range a.state.EdgeOrigins {
		if sameOriginValue(origin.Value, current) {
			return "EdgeOne 站点「" + origin.ZoneName + "」的源站组仍引用当前基站域名；请先将相关账号改为公网地址回源并完成接入，再更名或清空基站域名"
		}
	}
	for _, account := range a.state.EdgeOneAccounts {
		if account.Enabled && effectiveOriginMode(account.OriginMode) == model.EdgeOriginBase {
			return "EdgeOne 账号「" + account.Name + "」正在使用基站域名回源；请先改为公网地址回源并完成接入，再更名或清空基站域名"
		}
	}
	return ""
}

// edgeSite is one EdgeOne site as this gateway sees it. The ID is what every teo
// action needs; the name is what the operator recognises.
type edgeSite struct {
	AccountID string
	ZoneID    string
	Name      string
}

// edgeSitesLocked lists every site every enabled account can reach. Callers hold
// mu. As with DNS zones, a failure to read one account does not hide the others:
// a name served by a working account must still resolve when a second account's
// credentials have gone stale.
func (a *App) edgeSitesLocked(ctx context.Context, refresh bool) ([]edgeSite, []string, error) {
	if !refresh && len(a.edgeMemo) > 0 && time.Since(a.edgeMemoAt) < 30*time.Minute {
		return a.edgeMemo, nil, nil
	}
	sites := []edgeSite{}
	warnings := []string{}
	usable := 0
	for _, account := range a.state.EdgeOneAccounts {
		if !account.Enabled {
			continue
		}
		credentials, err := a.edgeAccountCredentials(account.ID)
		if err != nil {
			warnings = append(warnings, "EdgeOne 账号 "+account.Name+" 没有可用凭据，已跳过")
			continue
		}
		list, err := a.edgeAPI(credentials).Sites(ctx)
		if err != nil {
			warnings = append(warnings, "读取 EdgeOne 账号 "+account.Name+" 的站点列表失败："+err.Error())
			continue
		}
		usable++
		for _, site := range list {
			name := strings.ToLower(strings.TrimSpace(site.Name))
			if site.ZoneID == "" || !strings.Contains(name, ".") {
				continue
			}
			sites = append(sites, edgeSite{AccountID: account.ID, ZoneID: site.ZoneID, Name: name})
		}
	}
	if usable == 0 && len(a.state.EdgeOneAccounts) > 0 {
		return nil, warnings, errors.New("没有任何 EdgeOne 账号可用：请检查凭据与网络")
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].Name == sites[j].Name {
			return sites[i].AccountID < sites[j].AccountID
		}
		return sites[i].Name < sites[j].Name
	})
	a.edgeMemo, a.edgeMemoAt = sites, time.Now()
	return sites, warnings, nil
}

// edgeSiteForHost finds the site that owns a host, longest site name first so
// that a delegated sub-site is used instead of its parent.
//
// Callers hold mu.
func (a *App) edgeSiteForHost(ctx context.Context, host string, refresh bool) (edgeSite, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	sites, warnings, err := a.edgeSitesLocked(ctx, refresh)
	if err != nil {
		return edgeSite{}, err
	}
	best := edgeSite{}
	for _, site := range sites {
		if host == site.Name || strings.HasSuffix(host, "."+site.Name) {
			if len(site.Name) > len(best.Name) {
				best = site
			}
		}
	}
	if best.ZoneID == "" {
		if !refresh {
			return a.edgeSiteForHost(ctx, host, true)
		}
		message := "域名 " + host + " 不属于任何 EdgeOne 站点，请先在 EdgeOne 添加该站点，或把该服务改回直接发布"
		if len(warnings) > 0 {
			message += "（" + strings.Join(warnings, "；") + "）"
		}
		return edgeSite{}, errors.New(message)
	}
	return best, nil
}

// edgeOriginRecordLocked finds the origin group this gateway created inside one
// site. Callers hold mu.
func (a *App) edgeOriginRecordLocked(accountID, zoneID string) (model.EdgeOrigin, bool) {
	for _, origin := range a.state.EdgeOrigins {
		if origin.AccountID == accountID && origin.ZoneID == zoneID {
			return origin, true
		}
	}
	return model.EdgeOrigin{}, false
}

// edgeOriginValueLocked returns the origin value EdgeOne should be told to fetch
// from, for one account. Callers hold mu.
//
// The two modes are not interchangeable and the choice is the operator's,
// because they fail in different places:
//
//   - address mode writes the current public address literal. A change is an
//     explicit configuration write, so a new address is certain to be pushed —
//     provided this device is alive and reachable at the moment it changes.
//   - base mode writes the base station name and needs no call when the address
//     changes, because EdgeOne re-resolves it — but how long EdgeOne caches that
//     resolution is not documented anywhere in the teo API, so the switchover
//     window is not something this gateway can promise.
func (a *App) edgeOriginValueLocked(account model.EdgeOneAccount) (string, error) {
	if effectiveOriginMode(account.OriginMode) == model.EdgeOriginBase {
		if refusal := a.baseStationRefusalLocked(); refusal != "" {
			return "", errors.New(refusal)
		}
		return a.baseDomain(), nil
	}
	address := strings.TrimSpace(a.state.AppliedPublicAddress)
	if address == "" {
		return "", errors.New("尚未应用任何公网地址，无法告诉 EdgeOne 回源到哪里")
	}
	return address, nil
}

// baseStationWarningLocked reports whether the base station an EdgeOne origin
// will point at is one this gateway has actually published, and to the address
// it is serving.
//
// Base mode hands resolvability to a name this gateway publishes, so an
// unpublished base station means EdgeOne is told to fetch from a name that
// resolves to nothing. The accelerated name still comes up, still terminates TLS
// and still answers — with errors — and no page would have said why. What can be
// checked here is only what this gateway knows: its own publication ledger. That
// is stated as what it is, without any claim about resolvers it has not asked.
func (a *App) baseStationWarningLocked(origin string, applied model.State) string {
	journal, err := a.readDNSJournal()
	if err != nil {
		return "无法读取本机的解析托管记录，因此无法确认基站域名 " + origin + " 是否已经发布；请在解析页核对。"
	}
	entry, published := journal.Managed[strings.ToLower(origin)]
	if !published {
		return "基站域名 " + origin + " 还没有在本机发布解析。EdgeOne 会自己去解析这个名字，而这个名字现在解析不到任何地址——源站组会指向一个不存在的目标。请先在解析页发布它。"
	}
	if address := strings.TrimSpace(applied.AppliedPublicAddress); address != "" && !sameOriginValue(entry.Record.Value, address) {
		return "基站域名 " + origin + " 当前发布的地址是 " + entry.Record.Value + "，而本机正在服务的地址是 " + address + "；请等地址收敛完成，或手动重新发布这条记录。"
	}
	return ""
}

// ---------------------------------------------------------------------------
// HTTP: accounts and the settings that belong to them

func (a *App) listEdgeOneAccounts(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	accounts := a.state.EdgeOneAccounts
	if accounts == nil {
		accounts = []model.EdgeOneAccount{}
	}
	origins := a.state.EdgeOrigins
	if origins == nil {
		origins = []model.EdgeOrigin{}
	}
	reply(w, 200, map[string]any{
		"provider":   "edgeone",
		"accounts":   accounts,
		"origins":    origins,
		"baseDomain": a.state.Settings.BaseDomain,
		"baseTtl":    a.state.Settings.EffectiveBaseTTL(),
		"ttls":       []int{dnspod.ShortTTL, dnspod.MediumTTL, dnspod.DefaultTTL},
		"originModes": []map[string]string{
			{"value": model.EdgeOriginAddress, "label": "直接使用公网地址（地址变化时由本机更新回源）"},
			{"value": model.EdgeOriginBase, "label": "使用基站域名（EdgeOne 自行解析，本机不更新）"},
		},
	})
}

func (a *App) createEdgeOneAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string `json:"name"`
		SecretID   string `json:"secretId"`
		SecretKey  string `json:"secretKey"`
		OriginMode string `json:"originMode"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.SecretID, in.SecretKey = strings.TrimSpace(in.SecretID), strings.TrimSpace(in.SecretKey)
	if in.Name == "" || len(in.Name) > 100 {
		fail(w, 400, "账号名称不能为空且不能超过100字节")
		return
	}
	mode := effectiveOriginMode(in.OriginMode)
	credentials := teo.Credentials{SecretID: in.SecretID, SecretKey: in.SecretKey}
	if !credentials.Valid() {
		fail(w, 400, "请填写有效的腾讯云 SecretId 和 SecretKey")
		return
	}
	if a.opts.DryRun {
		fail(w, 403, "开发 dry-run 模式不允许校验或保存云端凭据")
		return
	}
	// The credentials are proved against the API before they are stored, with a
	// read-only action: a key that cannot list sites cannot create an acceleration
	// domain either, and finding that out at the first deployment would mean
	// discovering it in the middle of a change to live traffic.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	sites, err := teo.New(credentials).Sites(ctx)
	if err != nil {
		fail(w, 400, "无法用该凭据读取 EdgeOne 站点列表，请检查 SecretId/SecretKey 与 CAM 权限："+err.Error())
		return
	}
	if len(sites) == 0 {
		fail(w, 400, "该凭据可以访问 EdgeOne，但账号下没有任何站点；请先在 EdgeOne 控制台添加站点后再绑定")
		return
	}
	a.edgeMu.Lock()
	defer a.edgeMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	// Checked here rather than before the cloud call: the name it reads is written
	// under this lock, and a check outside it could pass on a name that is gone by
	// the time the account is stored. The cost is one wasted read-only Sites call
	// in a case the operator reaches once.
	if mode == model.EdgeOriginBase {
		if refusal := a.baseStationRefusalLocked(); refusal != "" {
			fail(w, 400, refusal)
			return
		}
	}
	if len(a.state.EdgeOneAccounts) >= maxEdgeOneAccounts {
		fail(w, 400, "EdgeOne 账号数量已达上限")
		return
	}
	id := creationResourceID(r)
	if err := writeJSON(a.edgeAccountCredentialPath(id), credentials); err != nil {
		fail(w, 500, "无法安全保存 EdgeOne 凭据")
		return
	}
	account := model.EdgeOneAccount{ID: id, Name: in.Name, SecretIDHint: credentialHint(in.SecretID), CredentialVersion: 1, OriginMode: mode, Enabled: true}
	old := clone(a.state)
	a.state.EdgeOneAccounts = append(a.state.EdgeOneAccounts, account)
	a.state.Revision++
	if err := a.saveLocked(); err != nil {
		a.state = old
		_ = os.Remove(a.edgeAccountCredentialPath(id))
		fail(w, 500, "无法保存配置")
		return
	}
	a.edgeMemo, a.edgePending = nil, nil
	a.replyStateLocked(w)
}

func (a *App) updateEdgeOneAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SecretID   *string `json:"secretId"`
		SecretKey  *string `json:"secretKey"`
		Enabled    *bool   `json:"enabled"`
		OriginMode *string `json:"originMode"`
		Name       *string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.edgeMu.Lock()
	defer a.edgeMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.edgeAccountIndex(r.PathValue("id"))
	if index < 0 {
		fail(w, 404, "EdgeOne 账号不存在")
		return
	}
	old := clone(a.state)
	account := a.state.EdgeOneAccounts[index]
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 100 {
			fail(w, 400, "账号名称不能为空且不能超过100字节")
			return
		}
		account.Name = name
	}
	if in.OriginMode != nil {
		mode := strings.TrimSpace(*in.OriginMode)
		if mode != model.EdgeOriginAddress && mode != model.EdgeOriginBase {
			fail(w, 400, "回源方式只能是公网地址或基站域名")
			return
		}
		if mode == model.EdgeOriginBase {
			if refusal := a.baseStationRefusalLocked(); refusal != "" {
				fail(w, 400, refusal)
				return
			}
		}
		account.OriginMode = mode
	}
	if in.Enabled != nil {
		account.Enabled = *in.Enabled
	}
	if in.SecretID != nil || in.SecretKey != nil {
		if a.opts.DryRun {
			fail(w, 403, "dry-run prohibits credential rotation")
			return
		}
		if in.SecretID == nil || in.SecretKey == nil {
			fail(w, 400, "Submit both SecretId and SecretKey")
			return
		}
		credentials := teo.Credentials{SecretID: strings.TrimSpace(*in.SecretID), SecretKey: strings.TrimSpace(*in.SecretKey)}
		if !credentials.Valid() {
			fail(w, 400, "Invalid EdgeOne credentials")
			return
		}
		required := map[string]bool{}
		for _, origin := range a.state.EdgeOrigins {
			if origin.AccountID == account.ID {
				required[origin.ZoneID] = true
			}
		}
		revision := a.state.Revision
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		a.mu.Unlock()
		sites, err := a.edgeAPI(credentials).Sites(ctx)
		a.mu.Lock()
		if revision != a.state.Revision {
			fail(w, 409, "Configuration changed during credential validation")
			return
		}
		if err != nil || len(sites) == 0 {
			fail(w, 400, "New credentials cannot read EdgeOne sites")
			return
		}
		for _, site := range sites {
			delete(required, site.ZoneID)
		}
		if len(required) > 0 {
			fail(w, 409, "New credentials cannot manage every existing EdgeOne site")
			return
		}
		account.CredentialFile, err = a.stageCredential("edgeone", account.ID, credentials)
		if err != nil {
			fail(w, 500, "Unable to stage new EdgeOne credentials")
			return
		}
		account.CredentialVersion++
		account.SecretIDHint = credentialHint(credentials.SecretID)
	}
	a.state.EdgeOneAccounts[index] = account
	a.state.Revision++
	if err := a.saveLocked(); err != nil {
		a.state = old
		fail(w, 500, "无法保存配置")
		return
	}
	a.edgeMemo, a.edgePending = nil, nil
	a.replyStateLocked(w)
}

// deleteEdgeOneAccount removes the credentials, and with them any record of the
// origin groups this gateway made at EdgeOne. The groups themselves are left
// alone: deleting a cloud resource that a live acceleration domain may still
// reference would take a site offline, and this gateway cannot prove from local
// state that nothing references them.
func (a *App) deleteEdgeOneAccount(w http.ResponseWriter, r *http.Request) {
	a.edgeMu.Lock()
	defer a.edgeMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	id := r.PathValue("id")
	index := a.edgeAccountIndex(id)
	if index < 0 {
		fail(w, 404, "EdgeOne 账号不存在")
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
		for _, target := range entry.Edge {
			if !target.Done && target.AccountID == id {
				fail(w, 409, "该账号仍被未完成的服务删除操作引用，请先保持原选项完成删除，不能移除核对云端结果所需的凭据")
				return
			}
		}
	}
	for _, origin := range a.state.EdgeOrigins {
		if origin.AccountID == id && sameOriginValue(origin.Value, a.baseDomain()) {
			fail(w, 409, "该账号的源站组仍引用当前基站域名；请先改为公网地址回源并完成接入，再删除账号，不能仅移除本地记录")
			return
		}
	}
	// A service still published through this account would stop being reconcilable
	// the moment the credentials went away, and its name would then be released on
	// the next automatic pass — a silent change to a working site. The check is
	// local: it compares the recorded origin groups' site names against the names
	// this account's services claim, with no cloud call, so it still works when
	// the credentials are the thing that is broken.
	for _, service := range a.state.Services {
		if !service.ServesEdgeOne() {
			continue
		}
		for _, domain := range service.Domains {
			if a.zoneCoversForAccountLocked(id, domain) {
				fail(w, 409, "服务 "+service.Name+" 的域名 "+domain+" 仍在通过该账号发布，请先把该服务改回直接发布或改选其它账号")
				return
			}
		}
	}
	old := clone(a.state)
	a.state.EdgeOneAccounts = append(a.state.EdgeOneAccounts[:index], a.state.EdgeOneAccounts[index+1:]...)
	kept := []model.EdgeOrigin{}
	for _, origin := range a.state.EdgeOrigins {
		if origin.AccountID != id {
			kept = append(kept, origin)
		}
	}
	a.state.EdgeOrigins = kept
	a.state.Revision++
	if err := a.saveLocked(); err != nil {
		a.state = old
		fail(w, 500, "无法保存配置")
		return
	}
	a.edgeMemo, a.edgePending = nil, nil
	// The credential file goes with the account; nothing at EdgeOne is touched.
	_ = os.Remove(filepath.Join(a.opts.DataDir, "edgeone-accounts", credentialFilename(id, old.EdgeOneAccounts[index].CredentialFile)))
	a.replyStateLocked(w)
}

// zoneCoversForAccountLocked reports whether one of this account's recorded
// origin groups sits in a site that owns the name. Callers hold mu.
func (a *App) zoneCoversForAccountLocked(accountID, host string) bool {
	host = strings.ToLower(host)
	for _, origin := range a.state.EdgeOrigins {
		if origin.AccountID != accountID {
			continue
		}
		name := strings.ToLower(origin.ZoneName)
		if name != "" && (host == name || strings.HasSuffix(host, "."+name)) {
			return true
		}
	}
	return false
}

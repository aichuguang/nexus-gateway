package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
	"w103d/gateway/internal/teo"
)

// Making one name work through EdgeOne.
//
// A name published through EdgeOne needs six things to be true, in this order:
// an origin group holding the address EdgeOne should fetch from, an acceleration
// domain fetching from that group, that domain online, the service's certificate
// uploaded to the SSL product, that certificate bound to the domain, and the
// site's WebSocket handling turned on. Every one of them is read before it is
// written, and every write is idempotent, so a half-finished run can be repeated
// without creating a second of anything.
//
// The plan and the actions are produced by one function, in one pass, under one
// `act` flag. Two functions would drift: the preview would promise a step the
// apply did not take, or take one it never showed. Here they cannot disagree,
// because the text of a step and the call behind it are written on the same
// line.
//
// Nothing here writes a DNS record. The name is made live by pointing its DNS
// record at the CNAME EdgeOne assigns, and that write goes through the DNS
// preview and confirmation like every other record this gateway owns.

// The steps. Each is one cloud change, named for what it does rather than for
// the API action behind it, because the operator reads these.
const (
	edgeCreateOrigin = "create-origin-group"
	edgeUpdateOrigin = "update-origin"
	edgeCreateDomain = "create-domain"
	edgeUpdateDomain = "update-domain-origin"
	edgeOnlineDomain = "enable-domain"
	edgeUploadCert   = "upload-certificate"
	edgeBindCert     = "bind-certificate"
	edgeWebSocket    = "enable-websocket"
)

// edgeStep is one change at EdgeOne, and the smallest thing the interface can
// show. From and To are the values either side of a change; both are omitted
// when the step creates something rather than altering it.
type edgeStep struct {
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
}

// edgePlan is what one name needs, and what it currently has.
type edgePlan struct {
	Host        string `json:"host"`
	ServiceID   string `json:"serviceId,omitempty"`
	ServiceName string `json:"serviceName,omitempty"`
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	ZoneID      string `json:"zoneId"`
	ZoneName    string `json:"zoneName"`
	OriginMode  string `json:"originMode"`
	Origin      string `json:"origin"`
	GroupID     string `json:"groupId,omitempty"`
	// Cname is what EdgeOne has assigned this name. It is what the DNS record
	// must say, and it is the only value in this plan that ends up somewhere
	// other than EdgeOne.
	Cname string `json:"cname,omitempty"`
	// DomainState is EdgeOne's own word for the acceleration domain's status.
	DomainState string `json:"domainState,omitempty"`
	CertID      string `json:"certId,omitempty"`
	CertName    string `json:"certName,omitempty"`
	// WebSocketSwitch and WebSocketTimeout describe the site as it is, so the
	// interface can report a timeout shorter than the API's maximum without
	// having to change it.
	WebSocketSwitch  string     `json:"webSocketSwitch,omitempty"`
	WebSocketTimeout int64      `json:"webSocketTimeout,omitempty"`
	Steps            []edgeStep `json:"steps"`
	Warnings         []string   `json:"warnings"`
	// Token is the one-shot confirmation for exactly this plan. It is present only
	// in a preview reply, never in the plan an apply reports back.
	Token string `json:"token,omitempty"`
	// Revision accompanies the token so callers never reconstruct its binding
	// from a separately fetched (and potentially newer) state response.
	Revision int64 `json:"revision,omitempty"`
	// Ready is true when nothing needs doing: every step above is already so.
	Ready bool `json:"ready"`
}

func (p edgePlan) actions() []string {
	list := make([]string, 0, len(p.Steps))
	for _, step := range p.Steps {
		list = append(list, step.Action)
	}
	sort.Strings(list)
	return list
}

func sameActions(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var errEdgeApprovalChanged = errors.New("EdgeOne 状态在执行期间变化，已停止未获确认的操作；此前完成的步骤未回滚，请重新预览核对")

// A second cloud read is not another authorization. Each actual write must
// consume one exact approved step, including its source value. Cloud APIs do
// not offer compare-and-swap, so this bounds our writes, not other operators.
type edgeWriteApproval struct {
	preview        *edgePreview
	used           map[int]bool
	uploadedCertID string
}

func (approval *edgeWriteApproval) allow(plan edgePlan, step edgeStep) error {
	if approval.preview == nil {
		return nil
	}
	p := approval.preview
	if plan.Host != p.Host || plan.AccountID != p.AccountID || plan.ZoneID != p.ZoneID ||
		plan.OriginMode != p.authorization.originMode || !sameOriginValue(plan.Origin, p.Origin) {
		return errEdgeApprovalChanged
	}
	for i, expected := range p.Steps {
		actual := step
		// A preview cannot know the ID returned by its approved upload. Only
		// that upload's result may fill the otherwise exact binding step.
		if expected.Action == edgeBindCert && expected.To == "" && approval.uploadedCertID != "" && actual.To == approval.uploadedCertID {
			actual.To = ""
		}
		if !approval.used[i] && actual == expected {
			approval.used[i] = true
			return nil
		}
	}
	return errEdgeApprovalChanged
}

// sameOriginValue compares two origin addresses, tolerating the case and
// trailing dot a host name may carry. An address is compared as an address, so
// that two spellings of the same IPv6 address are not reported as a change.
func sameOriginValue(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if left, err := netip.ParseAddr(a); err == nil {
		right, err := netip.ParseAddr(b)
		return err == nil && left == right
	}
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

// edgePreview is a single-use authorisation for one reconciliation of one name.
// It is bound to the site it was resolved against, the origin it would write and
// the set of steps it showed, so a confirmation cannot authorise a different
// change than the one the operator read.
type edgePreview struct {
	Host          string     `json:"host"`
	AccountID     string     `json:"accountId"`
	ZoneID        string     `json:"zoneId"`
	Origin        string     `json:"origin"`
	Revision      int64      `json:"revision"`
	Actions       []string   `json:"actions"`
	Steps         []edgeStep `json:"steps"`
	Warnings      []string   `json:"warnings"`
	Token         string     `json:"token"`
	expires       time.Time
	authorization edgeAuthorization
}

// These local inputs can change without changing the rendered proxy snapshot.
// Keep them on the server-side token, never in a response containing secrets.
type edgeAuthorization struct {
	revision               int64
	appliedRevision        int64
	credentialsHash        [32]byte
	certificateID          string
	certificateFingerprint string
	cloudCertificateID     string
	originGroupID          string
	originMode             string
}

func (a *App) edgeAuthorizationLocked(plan edgePlan) (edgeAuthorization, error) {
	credentials, err := a.edgeAccountCredentials(plan.AccountID)
	if err != nil {
		return edgeAuthorization{}, errors.New("EdgeOne 账号凭据不可用，请重新保存该账号的 SecretId / SecretKey")
	}
	service, claimed := a.serviceForHost(plan.Host)
	index := a.certIndex(service.CertificateID)
	if !claimed || index < 0 {
		return edgeAuthorization{}, errors.New("服务或证书配置已变化，请重新预览 EdgeOne")
	}
	return edgeAuthorization{
		revision: a.state.Revision, appliedRevision: a.state.AppliedRevision,
		credentialsHash: sha256.Sum256([]byte(credentials.SecretID + "\x00" + credentials.SecretKey)),
		certificateID:   service.CertificateID, certificateFingerprint: a.state.Certificates[index].Fingerprint,
		cloudCertificateID: plan.CertID, originGroupID: plan.GroupID, originMode: plan.OriginMode,
	}, nil
}

// edgeTargetForHost answers what a name's DNS record must say when EdgeOne
// answers for it, and is the one place that decides whether this gateway can
// publish a name through EdgeOne at all.
//
// It returns "" with no error when there is no EdgeOne account to ask. That
// answer releases the name rather than refusing it: an operator who has moved a
// name onto EdgeOne by hand, or who has not configured an account yet, must not
// be left holding a gateway that reports a conflict forever and stops every name
// queued behind it. When an account does exist, "I cannot see this site" is an
// error and never a release — releasing there would hide a real misconfiguration
// behind a silent drop.
//
// Callers hold mu.
func (a *App) edgeTargetForHost(ctx context.Context, host string, refresh bool) (string, error) {
	enabled := 0
	for _, account := range a.state.EdgeOneAccounts {
		if account.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		return "", nil
	}
	site, err := a.edgeSiteForHost(ctx, host, refresh)
	if err != nil {
		return "", err
	}
	credentials, err := a.edgeAccountCredentials(site.AccountID)
	if err != nil {
		return "", errors.New("EdgeOne 账号凭据不可用，请重新保存该账号的 SecretId / SecretKey")
	}
	domain, err := a.edgeAPI(credentials).DescribeDomain(ctx, site.ZoneID, host)
	if err != nil {
		return "", err
	}
	if domain == nil {
		return "", errors.New("该域名尚未在 EdgeOne 站点 " + site.Name + " 中创建加速域名，请先在 EdgeOne 页面完成接入")
	}
	if strings.TrimSpace(domain.Cname) == "" {
		return "", errors.New("EdgeOne 尚未为该域名分配 CNAME，请稍后在 EdgeOne 页面确认域名状态")
	}
	return strings.TrimSpace(domain.Cname), nil
}

// edgeReconcileLocked reads one name's state at EdgeOne, works out what is
// missing, and — when act is true — makes it so. Callers hold both edgeMu and
// mu.
func (a *App) edgeReconcileLocked(ctx context.Context, host string, refresh, act bool) (edgePlan, error) {
	return a.edgeReconcileApprovedLocked(ctx, host, refresh, act, nil)
}

func (a *App) edgeReconcileApprovedLocked(ctx context.Context, host string, refresh, act bool, approved *edgePreview) (edgePlan, error) {
	var plan edgePlan
	approval := edgeWriteApproval{preview: approved, used: map[int]bool{}}
	addStep := func(step edgeStep) error {
		plan.Steps = append(plan.Steps, step)
		if act {
			return approval.allow(plan, step)
		}
		return nil
	}
	host = strings.ToLower(strings.TrimSpace(host))
	plan.Host = host
	if blocked, err := a.deletionBlocksHost(host); err != nil {
		return plan, err
	} else if blocked {
		return plan, errors.New("该服务已有删除保护记录，已暂停 EdgeOne 发布；请保持原删除选项完成删除")
	}
	service, claimed := a.serviceForHost(host)
	if !claimed || !service.ServesEdgeOne() {
		return plan, errors.New("该域名不属于任何以 EdgeOne 发布的服务；请先把服务的入口方式改为 EdgeOne")
	}
	if !service.Enabled {
		return plan, errors.New("该服务当前已关闭；请先启用并应用配置，再接入 EdgeOne")
	}
	plan.ServiceID, plan.ServiceName = service.ID, service.Name
	// The origin value and the certificate both come from what is actually being
	// served, so a draft that would change either cannot be provisioned for. What
	// decides that is content, not the revision number: turning this very
	// service's entry mode to EdgeOne moves the revision and changes no server
	// block, so refusing it would refuse the one edit that leads to this page.
	if a.state.AppliedAt == "" || a.state.AppliedRevision < 1 {
		return plan, errors.New("还没有应用过任何配置，请先应用配置，再接入 EdgeOne")
	}
	applied, err := a.appliedSnapshotLocked()
	if err != nil {
		return plan, err
	}
	if servedUnapplied(a.state, applied) {
		return plan, errors.New("请先应用当前配置草稿，再接入 EdgeOne")
	}
	certIndex := a.certIndex(service.CertificateID)
	if certIndex < 0 || !a.state.Certificates[certIndex].Ready {
		return plan, errors.New("该服务尚未导入可用证书，请先导入证书再接入 EdgeOne")
	}
	certificate := a.state.Certificates[certIndex]
	// EdgeOne fetches from this device presenting this name, so this device still
	// has to hold a certificate that speaks for it.
	if err := engine.CertificateCoversAll(certificate, []string{host}); err != nil {
		return plan, err
	}
	plan.CertName = certificate.Name

	site, err := a.edgeSiteForHost(ctx, host, refresh)
	if err != nil {
		return plan, err
	}
	accountIndex := a.edgeAccountIndex(site.AccountID)
	if accountIndex < 0 {
		return plan, errors.New("该域名对应的 EdgeOne 账号已被删除")
	}
	account := a.state.EdgeOneAccounts[accountIndex]
	plan.AccountID, plan.AccountName = account.ID, account.Name
	plan.ZoneID, plan.ZoneName = site.ZoneID, site.Name
	plan.OriginMode = effectiveOriginMode(account.OriginMode)
	credentials, err := a.edgeAccountCredentials(account.ID)
	if err != nil {
		return plan, errors.New("EdgeOne 账号凭据不可用，请重新保存该账号的 SecretId / SecretKey")
	}
	api := a.edgeAPI(credentials)
	origin, err := a.edgeOriginValueLocked(account)
	if err != nil {
		return plan, err
	}
	plan.Origin = origin
	if plan.OriginMode == model.EdgeOriginBase {
		plan.Warnings = append(plan.Warnings,
			"使用基站域名回源，由 EdgeOne 解析 "+origin+"。")
		// Choosing this mode hands the origin's resolvability to a name this
		// gateway is the one that publishes. EdgeOne will resolve it itself, so a
		// base station with no record is an origin group pointing at nothing: the
		// accelerated name comes up and serves errors, and nothing on this page
		// would have said why. The gateway holds exactly one honest piece of
		// evidence -- its own publication ledger -- so it says that, and stops
		// short of claiming anything about resolvers it has not asked.
		if warning := a.baseStationWarningLocked(origin, applied); warning != "" {
			plan.Warnings = append(plan.Warnings, warning)
		}
	} else {
		plan.Warnings = append(plan.Warnings,
			"回源方式是公网地址："+origin+"。地址变化时本程序会更新这个源站组；若本机在地址变化时不在线，EdgeOne 会继续回源到旧地址。")
	}

	// --- origin group -------------------------------------------------------
	record, recorded := a.edgeOriginRecordLocked(account.ID, site.ZoneID)
	groupID := ""
	groupNeedsWrite := false
	if recorded {
		groupID = record.GroupID
		group, err := api.DescribeOriginGroup(ctx, site.ZoneID, groupID)
		if err != nil {
			return plan, err
		}
		switch {
		case group == nil:
			// The group is gone from EdgeOne but still in the local ledger. A new
			// one is made rather than the old ID reused, and the stale ID is what
			// the next line replaces.
			groupNeedsWrite, groupID = true, ""
		case len(group.Records) != 1:
			return plan, errors.New("EdgeOne 站点 " + site.Name + " 的源站组记录数量不是 1，可能已被其它配置改动；请先在 EdgeOne 控制台核对")
		case !sameOriginValue(group.Records[0].Record, record.Value):
			return plan, errors.New("EdgeOne 站点 " + site.Name + " 的源站组已被修改（当前 " + group.Records[0].Record + "，本程序记录 " + record.Value + "）；请先在 EdgeOne 控制台核对")
		case !sameOriginValue(record.Value, origin):
			groupNeedsWrite = true
		}
		plan.GroupID = groupID
	} else {
		groupNeedsWrite = true
	}

	if groupNeedsWrite {
		action, from := edgeCreateOrigin, ""
		if groupID != "" {
			action, from = edgeUpdateOrigin, record.Value
		}
		step := edgeStep{Action: action, Target: site.Name, Detail: "源站组回源地址设为 " + origin, From: from, To: origin}
		if err := addStep(step); err != nil {
			return plan, err
		}
		if act {
			if groupID == "" {
				name := "gateway-" + shortToken()
				created, err := api.CreateOriginGroup(ctx, site.ZoneID, name, origin)
				if err != nil {
					return plan, fmt.Errorf("创建 EdgeOne 源站组失败：%w", err)
				}
				groupID = created
				if err := a.recordEdgeOriginLocked(account.ID, site, created, origin); err != nil {
					return plan, err
				}
			} else if err := api.SetOriginGroupRecord(ctx, site.ZoneID, groupID, origin); err != nil {
				return plan, fmt.Errorf("更新 EdgeOne 源站组回源地址失败：%w", err)
			} else if err := a.recordEdgeOriginLocked(account.ID, site, groupID, origin); err != nil {
				return plan, err
			}
			plan.GroupID = groupID
		}
	}

	// --- acceleration domain ------------------------------------------------
	domain, err := api.DescribeDomain(ctx, site.ZoneID, host)
	if err != nil {
		return plan, err
	}
	if domain == nil {
		if err := addStep(edgeStep{Action: edgeCreateDomain, Target: host,
			Detail: "在站点 " + site.Name + " 新建加速域名，回源指向本程序的源站组", To: origin}); err != nil {
			return plan, err
		}
		if act {
			if groupID == "" {
				return plan, errors.New("源站组尚未就绪，已停止创建加速域名")
			}
			if err := api.CreateDomain(ctx, site.ZoneID, host, groupID); err != nil {
				return plan, fmt.Errorf("创建 EdgeOne 加速域名失败：%w", err)
			}
			// The CNAME is assigned by EdgeOne and is not in the create response, so
			// it is read back. It is what the DNS record has to say, and reporting a
			// plan whose whole purpose is that value without it would be useless.
			fresh, err := api.DescribeDomain(ctx, site.ZoneID, host)
			if err != nil {
				return plan, err
			}
			domain = fresh
		}
	} else {
		plan.Cname, plan.DomainState = strings.TrimSpace(domain.Cname), domain.Status
		repoint := groupNeedsWrite || domain.Origin == nil ||
			domain.Origin.OriginType != teo.OriginTypeOriginGroup || domain.Origin.Origin != groupID
		if repoint {
			from := ""
			if domain.Origin != nil {
				from = domain.Origin.Origin
			}
			if err := addStep(edgeStep{Action: edgeUpdateDomain, Target: host,
				Detail: "把加速域名的回源改为本程序的源站组", From: from, To: origin}); err != nil {
				return plan, err
			}
			if act {
				if groupID == "" {
					return plan, errors.New("源站组尚未就绪，已停止修改加速域名")
				}
				if err := api.SetDomainOrigin(ctx, site.ZoneID, host, groupID); err != nil {
					return plan, fmt.Errorf("修改 EdgeOne 加速域名回源失败：%w", err)
				}
			}
		}
	}
	if act && domain != nil {
		plan.Cname, plan.DomainState = strings.TrimSpace(domain.Cname), domain.Status
	}
	if domain != nil {
		switch domain.Status {
		case teo.DomainOnline:
			// EdgeOne is already serving the name; nothing here is left to do.
		case teo.DomainOffline:
			// The one state this gateway has business changing. A domain taken
			// offline was stopped deliberately, and leaving it stopped would look
			// exactly like a broken setup from the outside.
			if err := addStep(edgeStep{Action: edgeOnlineDomain, Target: host,
				Detail: "启用加速域名", From: domain.Status, To: teo.DomainOnline}); err != nil {
				return plan, err
			}
			if act {
				if err := api.SetDomainsOnline(ctx, site.ZoneID, []string{host}, true); err != nil {
					return plan, fmt.Errorf("启用 EdgeOne 加速域名失败：%w", err)
				}
				plan.DomainState = teo.DomainOnline
			}
		default:
			// init and process are EdgeOne's own work, not this gateway's: the
			// domain is deploying, and it comes online once the name resolves to
			// the assigned CNAME. Forcing it online here would buy nothing, and
			// treating it as something left to do would make every preview report
			// the same unfinished step forever — the drift this whole function
			// exists to prevent. It is stated instead, because the operator's next
			// move is on the DNS side and nothing else on this page says so.
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"加速域名当前状态是%s：EdgeOne 正在部署，等 %s 的解析指向下面的 CNAME 之后会自行生效，期间不需要在这里再做操作。",
				domainStatusText(domain.Status), host))
		}
	}

	// --- certificate --------------------------------------------------------
	// The certificate ID is looked up for this account specifically. A certificate
	// ID in the SSL product belongs to one Tencent Cloud account, so a second
	// account needs its own upload even for the same bytes.
	certID := certificate.EdgeOne.CertIDFor(account.ID, certificate.Fingerprint)
	if certID == "" {
		if err := addStep(edgeStep{Action: edgeUploadCert, Target: certificate.Name,
			Detail: "把证书上传到腾讯云 SSL 证书服务，取回 EdgeOne 绑定时使用的证书 ID"}); err != nil {
			return plan, err
		}
		if act {
			chain, key, err := a.certificatePEMs(certificate.ID)
			if err != nil {
				return plan, err
			}
			uploaded, err := a.certAPI(credentials).Upload(ctx, string(chain), string(key), edgeCertAlias(certificate))
			if err != nil {
				return plan, fmt.Errorf("上传证书到腾讯云 SSL 证书服务失败：%w", err)
			}
			if err := a.recordEdgeUploadLocked(certificate.ID, account.ID, certificate.Fingerprint, uploaded); err != nil {
				return plan, err
			}
			certID = uploaded
			approval.uploadedCertID = uploaded
		}
	}
	bound := ""
	if domain != nil {
		bound = domain.BoundCertID()
	}
	if certID == "" || bound != certID {
		if err := addStep(edgeStep{Action: edgeBindCert, Target: host,
			Detail: "把加速域名的证书绑定为本程序管理的证书", From: bound, To: certID}); err != nil {
			return plan, err
		}
		if act {
			if certID == "" {
				return plan, errors.New("证书尚未上传成功，已停止绑定")
			}
			if err := api.BindCertificate(ctx, site.ZoneID, []string{host}, certID); err != nil {
				return plan, fmt.Errorf("绑定 EdgeOne 证书失败：%w", err)
			}
			if err := a.recordEdgeBindingLocked(certificate.ID, model.CertificateBinding{
				AccountID: account.ID, ZoneID: site.ZoneID, ZoneName: site.Name,
				CertID: certID, Fingerprint: certificate.Fingerprint, Hosts: []string{host},
				At: time.Now().UTC().Format(time.RFC3339),
			}); err != nil {
				return plan, err
			}
		}
	}
	plan.CertID = certID

	// --- site WebSocket ------------------------------------------------------
	wsSwitch, wsTimeout, err := api.WebSocketSetting(ctx, site.ZoneID)
	if err != nil {
		return plan, err
	}
	plan.WebSocketSwitch, plan.WebSocketTimeout = wsSwitch, wsTimeout
	if wsSwitch != teo.WebSocketOn {
		if err := addStep(edgeStep{Action: edgeWebSocket, Target: site.Name,
			Detail: fmt.Sprintf("开启站点 WebSocket，空闲超时设为 %d 秒", teo.MaxWebSocketTimeout),
			From:   wsSwitch, To: teo.WebSocketOn}); err != nil {
			return plan, err
		}
		plan.Warnings = append(plan.Warnings,
			"WebSocket 是站点级设置：开启后对站点 "+site.Name+" 下的所有域名生效，包括本程序未管理的域名。")
		if act {
			if err := api.EnableWebSocket(ctx, site.ZoneID, teo.MaxWebSocketTimeout); err != nil {
				return plan, fmt.Errorf("开启 EdgeOne 站点 WebSocket 失败：%w", err)
			}
			plan.WebSocketSwitch, plan.WebSocketTimeout = teo.WebSocketOn, teo.MaxWebSocketTimeout
		}
	} else if wsTimeout < teo.MaxWebSocketTimeout {
		// Deliberately not raised. A shorter timeout may be someone's deliberate
		// choice for the whole site, and this gateway has no business widening a
		// shared setting for its own convenience. It is reported instead.
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"站点 %s 的 WebSocket 已开启，但空闲超时是 %d 秒，低于 EdgeOne 上限 %d 秒；长时间没有数据的连接会被边缘断开，如需更长请在 EdgeOne 控制台调整。",
			site.Name, wsTimeout, teo.MaxWebSocketTimeout))
	}
	plan.Ready = len(plan.Steps) == 0
	return plan, nil
}

// recordEdgeOriginLocked stores the origin group this gateway made, replacing
// any record for the same site. It does not move the revision: what EdgeOne
// holds is not part of the rendered configuration, and bumping the revision
// would invalidate an applied snapshot for a change no server block can see.
//
// Callers hold both edgeMu and mu.
func (a *App) recordEdgeOriginLocked(accountID string, site edgeSite, groupID, value string) error {
	next := []model.EdgeOrigin{}
	replaced := false
	for _, origin := range a.state.EdgeOrigins {
		if origin.AccountID == accountID && origin.ZoneID == site.ZoneID {
			origin.GroupID, origin.Value, origin.ZoneName = groupID, value, site.Name
			replaced = true
		}
		next = append(next, origin)
	}
	if !replaced {
		next = append(next, model.EdgeOrigin{AccountID: accountID, ZoneID: site.ZoneID, ZoneName: site.Name, GroupID: groupID, Value: value})
	}
	old := a.state.EdgeOrigins
	a.state.EdgeOrigins = next
	if err := a.saveLocked(); err != nil {
		a.state.EdgeOrigins = old
		return errors.New("EdgeOne 源站组已创建，但本地记录保存失败；请重新预览（只读）核对")
	}
	return nil
}

// recordCertificateEdgeLocked applies one change to a certificate's EdgeOne
// ledger.
//
// It never fails the operation it describes. A certificate that is live on this
// device and stale at EdgeOne is a working service and a broken one at the same
// time, and refusing to finish the local work because the record of the remote
// work could not be written would turn a reporting problem into an outage. It
// reports the save failure, and the caller decides — the callers below, which
// are mid-provisioning, do propagate it, because there the record is what makes
// the next run idempotent.
//
// Callers hold edgeMu and mu.
func (a *App) recordCertificateEdgeLocked(certID string, edit func(*model.CertificateEdgeOne)) error {
	index := a.certIndex(certID)
	if index < 0 {
		return errors.New("证书槽不存在")
	}
	old := a.state.Certificates[index].EdgeOne
	next := model.CertificateEdgeOne{}
	if old != nil {
		next.Uploads = append(next.Uploads, old.Uploads...)
		next.Bindings = append(next.Bindings, old.Bindings...)
	}
	edit(&next)
	value := next
	a.state.Certificates[index].EdgeOne = &value
	if err := a.saveLocked(); err != nil {
		a.state.Certificates[index].EdgeOne = old
		return errors.New("EdgeOne 已变更，但本地记录保存失败；请重新预览（只读）核对")
	}
	return nil
}

func (a *App) recordEdgeUploadLocked(certID, accountID, fingerprint, uploaded string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return a.recordCertificateEdgeLocked(certID, func(edge *model.CertificateEdgeOne) {
		kept := edge.Uploads[:0]
		for _, upload := range edge.Uploads {
			if upload.AccountID != accountID {
				kept = append(kept, upload)
			}
		}
		edge.Uploads = append(kept, model.CertificateUpload{
			AccountID: accountID, CertID: uploaded, Fingerprint: fingerprint, At: now,
		})
	})
}

func (a *App) recordEdgeBindingLocked(certID string, binding model.CertificateBinding) error {
	return a.recordCertificateEdgeLocked(certID, func(edge *model.CertificateEdgeOne) {
		kept := edge.Bindings[:0]
		for _, existing := range edge.Bindings {
			if existing.AccountID != binding.AccountID || existing.ZoneID != binding.ZoneID {
				kept = append(kept, existing)
			}
		}
		edge.Bindings = append(kept, binding)
	})
}

// recordEdgeBindingFailureLocked notes a failed binding without claiming the
// hosts are covered. It replaces any success recorded for the same site, so a
// certificate that was bound and then failed to rebound reads as failed rather
// than as still good.
func (a *App) recordEdgeBindingFailureLocked(certID string, binding model.CertificateBinding, reason string) {
	binding.Error = reason
	binding.Hosts = nil
	binding.At = time.Now().UTC().Format(time.RFC3339)
	_ = a.recordEdgeBindingLocked(certID, binding)
}

// propagateCertificate pushes a freshly activated certificate to EdgeOne in the
// background.
//
// It is asynchronous and it cannot fail its caller, on purpose. A certificate
// push arrives from certd over HTTP, and certd has its own timeout; making that
// request wait on two Tencent Cloud APIs would turn a slow cloud into a failed
// renewal. More importantly the local side — the certificate nginx is actually
// serving — has already succeeded by then, so there is nothing left to protect by
// waiting.
//
// A failure here is never a rollback. The certificate on this device is correct
// and serving; reverting it because a remote copy could not be updated would
// break a working site to fix a bookkeeping problem. The failure is recorded on
// the certificate's EdgeOne ledger, where the interface reports it.
func (a *App) propagateCertificate(id string) {
	if a.opts.DryRun {
		return
	}
	parent, finish, accepted := a.beginWork()
	if !accepted {
		log.Printf("EdgeOne certificate propagation for %s deferred by shutdown; verify cloud binding after restart", id)
		return
	}
	go func() {
		defer finish()
		ctx, cancel := context.WithTimeout(parent, 150*time.Second)
		defer cancel()
		// A queued propagation waits rather than disappearing behind a publish.
		if !lockContext(ctx, &a.changeMu) {
			log.Printf("EdgeOne certificate propagation for %s canceled before start; verify cloud binding after restart", id)
			return
		}
		defer a.changeMu.Unlock()
		if !lockContext(ctx, &a.edgeMu) {
			return
		}
		defer a.edgeMu.Unlock()
		if !lockContext(ctx, &a.mu) {
			return
		}
		defer a.mu.Unlock()
		if err := a.propagateCertificateLocked(ctx, id); err != nil {
			log.Printf("EdgeOne certificate propagation for %s failed: %v", id, err)
		}
	}()
}

// propagateCertificateLocked uploads this certificate once per account and binds
// it once per site, for every host published through EdgeOne that uses it.
//
// Hosts are grouped by site because EdgeOne's certificate call is site-scoped and
// rejects a batch whose hosts disagree — binding one host at a time would be one
// rejection per host, and would also leave the site in a state where different
// hosts of the same certificate carry different settings.
//
// Callers hold edgeMu and mu.
func (a *App) propagateCertificateLocked(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	index := a.certIndex(id)
	if index < 0 {
		return nil
	}
	certificate := a.state.Certificates[index]
	if !certificate.Ready {
		return nil
	}
	// The applied snapshot, not the draft. A certificate is pushed for the hosts
	// that are actually being published through EdgeOne; a service still sitting
	// in an unapplied draft has no acceleration domain and nothing to bind to.
	applied, err := AppliedState(a.opts)
	if err != nil {
		return err
	}
	type siteGroup struct {
		site    edgeSite
		account model.EdgeOneAccount
		hosts   []string
	}
	groups := map[string]*siteGroup{}
	order := []string{}
	firstError := ""
	// note records the failure against the site it belongs to and keeps the first
	// one reported, so the operator sees the earliest cause rather than the last
	// symptom.
	note := func(accountID, zoneID, zoneName, reason string) {
		a.recordEdgeBindingFailureLocked(id, model.CertificateBinding{
			AccountID: accountID, ZoneID: zoneID, ZoneName: zoneName, Fingerprint: certificate.Fingerprint,
		}, reason)
		if firstError == "" {
			firstError = reason
		}
	}
	for _, service := range applied.Services {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !service.ServesEdgeOne() || service.CertificateID != id {
			continue
		}
		if blocked, err := a.serviceDeletionBlocked(service.ID); err != nil {
			return err
		} else if blocked {
			continue
		}
		for _, host := range service.Domains {
			host = strings.ToLower(strings.TrimSpace(host))
			site, err := a.edgeSiteForHost(ctx, host, false)
			if err != nil {
				if firstError == "" {
					firstError = err.Error()
				}
				continue
			}
			accountIndex := a.edgeAccountIndex(site.AccountID)
			if accountIndex < 0 {
				if firstError == "" {
					firstError = "服务 " + service.Name + " 的域名 " + host + " 对应的 EdgeOne 账号已被删除"
				}
				continue
			}
			key := site.AccountID + "\x00" + site.ZoneID
			group, ok := groups[key]
			if !ok {
				group = &siteGroup{site: site, account: a.state.EdgeOneAccounts[accountIndex]}
				groups[key] = group
				order = append(order, key)
			}
			group.hosts = append(group.hosts, host)
		}
	}
	if len(order) == 0 {
		return nil
	}
	sort.Strings(order)
	for _, key := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		group := groups[key]
		credentials, err := a.edgeAccountCredentials(group.account.ID)
		if err != nil {
			note(group.account.ID, group.site.ZoneID, group.site.Name, "EdgeOne 账号凭据不可用")
			continue
		}
		api := a.edgeAPI(credentials)
		certID := certificate.EdgeOne.CertIDFor(group.account.ID, certificate.Fingerprint)
		if certID == "" {
			chain, privateKey, err := a.certificatePEMs(certificate.ID)
			if err != nil {
				note(group.account.ID, group.site.ZoneID, group.site.Name, err.Error())
				continue
			}
			uploaded, err := a.certAPI(credentials).Upload(ctx, string(chain), string(privateKey), edgeCertAlias(certificate))
			if err != nil {
				note(group.account.ID, group.site.ZoneID, group.site.Name, "上传证书失败："+err.Error())
				continue
			}
			if err := a.recordEdgeUploadLocked(id, group.account.ID, certificate.Fingerprint, uploaded); err != nil {
				note(group.account.ID, group.site.ZoneID, group.site.Name, err.Error())
				continue
			}
			certID = uploaded
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		sort.Strings(group.hosts)
		if err := api.BindCertificate(ctx, group.site.ZoneID, group.hosts, certID); err != nil {
			note(group.account.ID, group.site.ZoneID, group.site.Name, "绑定证书失败："+err.Error())
			continue
		}
		if err := a.recordEdgeBindingLocked(id, model.CertificateBinding{
			AccountID: group.account.ID, ZoneID: group.site.ZoneID, ZoneName: group.site.Name,
			CertID: certID, Fingerprint: certificate.Fingerprint, Hosts: group.hosts,
			At: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			note(group.account.ID, group.site.ZoneID, group.site.Name, err.Error())
		}
	}
	if firstError != "" {
		return errors.New(firstError)
	}
	return nil
}

// certificatePEMs reads the certificate and key this gateway wrote for a slot.
func (a *App) certificatePEMs(id string) ([]byte, []byte, error) {
	i := a.certIndex(id)
	if i < 0 {
		return nil, nil, errors.New("certificate slot not found")
	}
	dir, err := engine.CertificateDirectory(a.opts.DataDir, a.state.Certificates[i])
	if err != nil {
		return nil, nil, err
	}
	chain, err := readLimitedFile(filepath.Join(dir, "fullchain.pem"))
	if err != nil {
		return nil, nil, errors.New("无法读取本机证书文件")
	}
	key, err := readLimitedFile(filepath.Join(dir, "privkey.pem"))
	if err != nil {
		return nil, nil, errors.New("无法读取本机证书私钥文件")
	}
	return chain, key, nil
}

// edgeCertAlias is the remark the certificate carries in the SSL product, so the
// operator can tell which gateway and which slot it came from. It is the only
// piece of local naming that leaves the device, which is why it is built here
// from a fixed prefix and a sanitised name rather than passed through.
func edgeCertAlias(certificate model.Certificate) string {
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, certificate.Name)
	name = strings.TrimSpace(name)
	if runes := []rune(name); len(runes) > 60 {
		name = string(runes[:60])
	}
	if name == "" {
		return "Gateway"
	}
	return "Gateway / " + name
}

func domainStatusText(status string) string {
	switch status {
	case teo.DomainOnline:
		return "已生效"
	case teo.DomainProcess:
		return "部署中"
	case teo.DomainOffline:
		return "已停用"
	case teo.DomainInit:
		return "未生效"
	case "":
		return "未知"
	}
	return status
}

// shortToken is the suffix on a new origin group's name. It keeps a fresh
// installation from colliding with a group left behind by a removed one — and
// the group is deliberately never looked up by name, because an origin group
// carries no ownership marker and one found by name could belong to a domain
// this gateway has never heard of.
func shortToken() string { return NewID()[:6] }

// ---------------------------------------------------------------------------
// HTTP: preview and apply

func (a *App) previewEdgeHost(w http.ResponseWriter, r *http.Request) {
	a.edgeMu.Lock()
	defer a.edgeMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	plan, err := a.edgeReconcileLocked(ctx, r.PathValue("host"), false, false)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	token, err := dnsRandomToken()
	if err != nil {
		fail(w, 500, "无法创建安全确认令牌")
		return
	}
	if plan.Warnings == nil {
		plan.Warnings = []string{}
	}
	if plan.Steps == nil {
		plan.Steps = []edgeStep{}
	}
	authorization, err := a.edgeAuthorizationLocked(plan)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	preview := edgePreview{
		Host: plan.Host, AccountID: plan.AccountID, ZoneID: plan.ZoneID, Origin: plan.Origin,
		Revision: a.state.AppliedRevision, Actions: plan.actions(), Steps: plan.Steps,
		Warnings: plan.Warnings, Token: token, expires: time.Now().Add(10 * time.Minute),
		authorization: authorization,
	}
	if a.opts.DryRun {
		plan.Warnings = append(plan.Warnings, "开发 dry-run 模式禁止任何 EdgeOne 写入。")
		preview.Warnings = plan.Warnings
	}
	if a.edgePending == nil {
		a.edgePending = map[string]edgePreview{}
	}
	for candidate, pending := range a.edgePending {
		if time.Now().After(pending.expires) || pending.Host == plan.Host {
			delete(a.edgePending, candidate)
		}
	}
	if len(a.edgePending) >= 16 {
		fail(w, 429, "待确认预览过多，请稍后重试")
		return
	}
	a.edgePending[token] = preview
	plan.Token = token
	plan.Revision = preview.Revision
	reply(w, 200, plan)
}

func (a *App) applyEdgeHost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Host     string `json:"host"`
		Token    string `json:"token"`
		Revision int64  `json:"revision"`
		Confirm  bool   `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		fail(w, 400, "必须明确确认 EdgeOne 变更")
		return
	}
	if a.opts.DryRun {
		fail(w, 403, "开发 dry-run 模式禁止任何 EdgeOne 写入")
		return
	}
	a.edgeMu.Lock()
	defer a.edgeMu.Unlock()
	pending, ok := a.edgePending[in.Token]
	if !ok || !strings.EqualFold(pending.Host, in.Host) || pending.Revision != in.Revision || time.Now().After(pending.expires) {
		fail(w, 409, "EdgeOne 预览已失效，请重新预览后确认")
		return
	}
	delete(a.edgePending, in.Token)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Revision != pending.authorization.revision || a.state.AppliedRevision != pending.authorization.appliedRevision {
		fail(w, 409, "配置在预览后发生变化，尚未写入；请重新预览 EdgeOne")
		return
	}
	// Validation and application both read the cloud, so this route can outlast
	// the server's normal 45-second write deadline. Keep the longer deadline local
	// to this response, with time left to report the 90-second context timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Minute))
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	plan, err := a.edgeReconcileLocked(ctx, in.Host, false, false)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	// Re-read before acting, and refuse when what is now needed differs from what
	// was authorised. Either side may have moved: the address, the certificate,
	// the site, or the operator next door.
	authorization, err := a.edgeAuthorizationLocked(plan)
	if err != nil || authorization != pending.authorization ||
		plan.ZoneID != pending.ZoneID || plan.AccountID != pending.AccountID ||
		!sameOriginValue(plan.Origin, pending.Origin) || !sameActions(plan.actions(), pending.Actions) ||
		!slices.Equal(plan.Steps, pending.Steps) {
		fail(w, 409, "EdgeOne 状态在预览后发生变化，已停止写入；请重新预览")
		return
	}
	plan, err = a.edgeReconcileApprovedLocked(ctx, in.Host, false, true, &pending)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errEdgeApprovalChanged) {
			status = http.StatusConflict
		}
		fail(w, status, err.Error())
		return
	}
	message := "EdgeOne 已完成接入：源站、加速域名、证书与 WebSocket 均已就绪。"
	if plan.Cname != "" {
		message += " 请把 " + plan.Host + " 的解析指向 " + plan.Cname + " 后，域名才会真正生效。"
	}
	reply(w, 200, map[string]any{"ok": true, "state": "submitted", "cname": plan.Cname, "message": message, "plan": plan})
}

// ---------------------------------------------------------------------------
// HTTP: status

// edgeHostStatus is one name's EdgeOne situation, read-only.
type edgeHostStatus struct {
	Host        string `json:"host"`
	ServiceID   string `json:"serviceId,omitempty"`
	ServiceName string `json:"serviceName,omitempty"`
	AccountID   string `json:"accountId,omitempty"`
	AccountName string `json:"accountName,omitempty"`
	Enabled     bool   `json:"enabled"`
	Status      string `json:"status"`
	Ready       bool   `json:"ready"`
	Site        string `json:"site,omitempty"`
	Origin      string `json:"origin,omitempty"`
	OriginMode  string `json:"originMode,omitempty"`
	Cname       string `json:"cname,omitempty"`
	DomainState string `json:"domainState,omitempty"`
	CertBound   string `json:"certBound,omitempty"`
	CertLocal   string `json:"certLocal,omitempty"`
	WebSocket   string `json:"webSocket,omitempty"`
	// RecordSays is what the local ledger holds for this name, which is what the
	// gateway last wrote — not what DNS currently serves.
	RecordSays string `json:"recordSays,omitempty"`
	Message    string `json:"message,omitempty"`
}

func (a *App) edgeStatus(w http.ResponseWriter, r *http.Request) {
	a.edgeMu.Lock()
	defer a.edgeMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	reply(w, 200, map[string]any{"hosts": a.edgeHostViewsLocked(ctx), "accounts": len(a.state.EdgeOneAccounts)})
}

// Callers hold edgeMu then mu. This is shared by the legacy and unified views;
// it reads cloud state but never applies a plan or writes DNS.
func (a *App) edgeHostViewsLocked(ctx context.Context) []edgeHostStatus {
	journal, journalErr := a.readDNSJournal()
	hosts := []string{}
	for _, service := range a.state.Services {
		if !service.ServesEdgeOne() {
			continue
		}
		hosts = append(hosts, service.Domains...)
	}
	sort.Strings(hosts)
	wsBySite := map[string]string{}
	views := []edgeHostStatus{}
	anyEnabledAccount := slices.ContainsFunc(a.state.EdgeOneAccounts, func(account model.EdgeOneAccount) bool { return account.Enabled })
	for _, host := range hosts {
		host = strings.ToLower(host)
		view := edgeHostStatus{Host: host, Status: "pending"}
		if service, ok := a.serviceForHost(host); ok {
			view.ServiceID, view.ServiceName, view.Enabled = service.ID, service.Name, service.Enabled
		}
		if !view.Enabled {
			view.Status, view.Message = "disabled", "服务已禁用，未查询云端状态"
			views = append(views, view)
			continue
		}
		if len(a.state.EdgeOneAccounts) > 0 && !anyEnabledAccount {
			view.Enabled, view.Status, view.Message = false, "disabled", "EdgeOne 账号均已禁用，未查询云端状态"
			views = append(views, view)
			continue
		}
		if err := ctx.Err(); err != nil {
			view.Status, view.Message = "unknown", "EdgeOne 状态读取超时或取消，未确认就绪"
			views = append(views, view)
			continue
		}
		if entry, published := journal.Managed[host]; published {
			view.RecordSays = entry.Record.Value
		}
		site, err := a.edgeSiteForHost(ctx, host, false)
		if err != nil {
			view.Status, view.Message = "error", err.Error()
			views = append(views, view)
			continue
		}
		view.Site = site.Name
		accountIndex := a.edgeAccountIndex(site.AccountID)
		if accountIndex < 0 {
			view.Status = "error"
			view.Message = "该域名对应的 EdgeOne 账号已被删除"
			views = append(views, view)
			continue
		}
		account := a.state.EdgeOneAccounts[accountIndex]
		view.AccountID, view.AccountName = account.ID, account.Name
		if !account.Enabled {
			view.Enabled, view.Status, view.Message = false, "disabled", "EdgeOne 账号已禁用"
			views = append(views, view)
			continue
		}
		view.OriginMode = effectiveOriginMode(account.OriginMode)
		if origin, err := a.edgeOriginValueLocked(account); err == nil {
			view.Origin = origin
		}
		credentials, err := a.edgeAccountCredentials(account.ID)
		if err != nil {
			view.Status = "error"
			view.Message = "EdgeOne 账号凭据不可用"
			views = append(views, view)
			continue
		}
		api := a.edgeAPI(credentials)
		domain, err := api.DescribeDomain(ctx, site.ZoneID, host)
		if err != nil {
			view.Status, view.Message = "error", err.Error()
			views = append(views, view)
			continue
		}
		if domain == nil {
			view.Message = "尚未在 EdgeOne 创建加速域名"
			views = append(views, view)
			continue
		}
		view.Cname, view.DomainState, view.CertBound = strings.TrimSpace(domain.Cname), domain.Status, domain.BoundCertID()
		if service, ok := a.serviceForHost(host); ok {
			if index := a.certIndex(service.CertificateID); index >= 0 {
				certificate := a.state.Certificates[index]
				view.CertLocal = certificate.EdgeOne.CertIDFor(site.AccountID, certificate.Fingerprint)
			}
		}
		cacheKey := account.ID + "/" + site.ZoneID
		if _, cached := wsBySite[cacheKey]; !cached {
			if wsSwitch, wsTimeout, err := api.WebSocketSetting(ctx, site.ZoneID); err == nil {
				wsBySite[cacheKey] = fmt.Sprintf("%s / %d 秒", wsSwitch, wsTimeout)
			} else {
				wsBySite[cacheKey] = "未知"
			}
		}
		view.WebSocket = wsBySite[cacheKey]
		switch {
		case journalErr != nil:
			view.Status, view.Message = "unknown", "无法读取本机解析记录，未确认入口已发布"
		case view.RecordSays == "":
			view.Message = "解析尚未指向 EdgeOne，请在该域名的解析条目上发布 CNAME"
		case !sameOriginValue(view.RecordSays, view.Cname):
			view.Message = "解析记录与 EdgeOne 分配的 CNAME 不一致，请重新预览该域名的解析"
		case view.DomainState != teo.DomainOnline:
			view.Message = "加速域名状态为 " + domainStatusText(view.DomainState) + "，EdgeOne 尚未开始服务该域名"
		case view.CertBound == "" || view.CertLocal == "" || view.CertBound != view.CertLocal:
			view.Message = "边缘证书尚未绑定本机当前证书，请预览接入"
		case view.WebSocket == "未知":
			view.Status, view.Message = "unknown", "WebSocket 状态读取失败，未确认就绪"
		case !strings.HasPrefix(view.WebSocket, teo.WebSocketOn+" / "):
			view.Message = "WebSocket 尚未开启，请预览接入"
		default:
			view.Status, view.Ready = "ready", true
			view.Message = "已指向 EdgeOne；实际可达性仍需在公网独立验证"
		}
		views = append(views, view)
	}
	return views
}

// ---------------------------------------------------------------------------
// Automatic origin maintenance

// convergeEdge is the EdgeOne half of the automatic pass, and it is a separate
// entry point rather than a step inside converge on purpose.
//
// The two halves hold different locks, and the lock order is always edgeMu
// before mu. Folding this into converge would mean either taking edgeMu for the
// whole 90 second cloud budget of the DNS half -- so that an operator opening
// the EdgeOne page waits behind a slow DNSPod call -- or inverting the order.
// Neither is worth it: the halves have nothing to say to each other.
//
// Callers hold no locks.
func (a *App) convergeEdge(ctx context.Context) (int, error) {
	if !lockContext(ctx, &a.edgeMu) {
		return 0, ctx.Err()
	}
	defer a.edgeMu.Unlock()
	if !lockContext(ctx, &a.mu) {
		return 0, ctx.Err()
	}
	defer a.mu.Unlock()
	if !a.state.Settings.AutoResolve {
		return 0, nil
	}
	return a.convergeEdgeOriginsLocked(ctx)
}

// convergeEdgeOriginsLocked repoints the origin groups this gateway created at
// the address that was just proven to be served. It is the EdgeOne half of the
// automatic path, and it exists because without it every address change would
// quietly break every EdgeOne site: the DNS record would still be correct,
// pointing at EdgeOne, while EdgeOne went on fetching from an address that no
// longer answers. That failure has no DNS symptom to notice.
//
// It reads before it writes, exactly as the DNS path does: a group whose records
// no longer hold what this gateway last wrote was changed by someone else, and
// overwriting that would move traffic for a decision this gateway did not make.
//
// Callers hold both edgeMu and mu.
func (a *App) convergeEdgeOriginsLocked(ctx context.Context) (int, error) {
	updated := 0
	for i := range a.state.EdgeOrigins {
		if err := ctx.Err(); err != nil {
			return updated, err
		}
		origin := a.state.EdgeOrigins[i]
		accountIndex := a.edgeAccountIndex(origin.AccountID)
		if accountIndex < 0 || !a.state.EdgeOneAccounts[accountIndex].Enabled {
			continue
		}
		account := a.state.EdgeOneAccounts[accountIndex]
		// A base-station origin does not follow the address at all, which is the
		// whole reason to choose it.
		if effectiveOriginMode(account.OriginMode) != model.EdgeOriginAddress {
			continue
		}
		want, err := a.edgeOriginValueLocked(account)
		if err != nil {
			return updated, err
		}
		if origin.Value == "" {
			return updated, fmt.Errorf("EdgeOne 站点 %s 的源站组缺少本地记录，无法确认是否被改动；请在 EdgeOne 页面重新接入该域名", origin.ZoneName)
		}
		if sameOriginValue(origin.Value, want) {
			continue
		}
		credentials, err := a.edgeAccountCredentials(account.ID)
		if err != nil {
			return updated, errors.New("EdgeOne 账号 " + account.Name + " 的凭据不可用，已停止更新回源")
		}
		api := a.edgeAPI(credentials)
		group, err := api.DescribeOriginGroup(ctx, origin.ZoneID, origin.GroupID)
		if err != nil {
			return updated, fmt.Errorf("读取 EdgeOne 站点 %s 的源站组失败：%w", origin.ZoneName, err)
		}
		if group == nil {
			return updated, fmt.Errorf("EdgeOne 站点 %s 的源站组已不存在，请在 EdgeOne 页面重新接入该域名", origin.ZoneName)
		}
		if len(group.Records) != 1 || !sameOriginValue(group.Records[0].Record, origin.Value) {
			return updated, fmt.Errorf("EdgeOne 站点 %s 的源站组已被修改，已停止写入；请在 EdgeOne 控制台核对", origin.ZoneName)
		}
		if err := api.SetOriginGroupRecord(ctx, origin.ZoneID, origin.GroupID, want); err != nil {
			return updated, fmt.Errorf("更新 EdgeOne 站点 %s 的回源地址失败：%w", origin.ZoneName, err)
		}
		a.state.EdgeOrigins[i].Value = want
		updated++
		// A later site's failure must not leave this successful write only in
		// memory: after a restart it would look like somebody else's mutation.
		if err := a.saveLocked(); err != nil {
			return updated, errors.New("EdgeOne 回源已更新，但本地记录保存失败；请重新预览（只读）核对")
		}
	}
	return updated, nil
}

// edgeHostsLocked counts the names published through EdgeOne. Callers hold mu.
func (a *App) edgeHostsLocked() int {
	count := 0
	for _, service := range a.state.Services {
		if service.ServesEdgeOne() {
			count += len(service.Domains)
		}
	}
	return count
}

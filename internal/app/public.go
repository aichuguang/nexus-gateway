package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

// publicCandidate is one address this machine holds, with the reasons it is or
// is not usable as the gateway's public entry. Both families are reported: an
// IPv6 line is preferred, but an IPv4-only line must still work, and a private
// or carrier-grade NAT address must never be published as if it were reachable.
type publicCandidate struct {
	Address    string `json:"address"`
	Family     string `json:"family"`
	Interface  string `json:"interface"`
	Temporary  bool   `json:"temporary"`
	Deprecated bool   `json:"deprecated"`
	Tentative  bool   `json:"tentative"`
	Eligible   bool   `json:"eligible"`
	Reason     string `json:"reason"`
}

type publicDNSStatus struct {
	Family  string   `json:"family"`
	Records []string `json:"records"`
	CNAME   string   `json:"cname"`
	Error   string   `json:"error"`
	Matches bool     `json:"matches"`
}

type publicCheckStatus struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// publicServiceStatus keeps the four independent verdicts apart: configuration
// applied, certificate usable, local entry check, DNS observation. None of them
// is evidence of external reachability.
type publicServiceStatus struct {
	ServiceID string   `json:"serviceId"`
	Name      string   `json:"name"`
	Entry     string   `json:"entry"`
	Domains   []string `json:"domains"`
	// EntryMode is "direct" or "edge". It is reported separately from Entry
	// because the two answer different questions: Entry says where a client goes,
	// EntryMode says who answers there and therefore what "correct" means for the
	// DNS observation below.
	EntryMode              model.EntryMode   `json:"entryMode"`
	EdgeProvider           string            `json:"edgeProvider,omitempty"`
	Address                string            `json:"address"`
	Port                   int               `json:"port"`
	DraftRevision          int64             `json:"draftRevision"`
	AppliedRevision        int64             `json:"appliedRevision"`
	ConfigurationApplied   bool              `json:"configurationApplied"`
	CertificateReady       bool              `json:"certificateReady"`
	CertificateError       string            `json:"certificateError"`
	CertificateFingerprint string            `json:"certificateFingerprint"`
	DNS                    publicDNSStatus   `json:"dns"`
	LocalTLS               publicCheckStatus `json:"localTLS"`
	External               publicCheckStatus `json:"external"`
}

type publicPreflightResult struct {
	CheckedAt      string                `json:"checkedAt"`
	AppliedAddress string                `json:"appliedAddress"`
	Candidates     []publicCandidate     `json:"candidates"`
	Routes         string                `json:"routes"`
	Services       []publicServiceStatus `json:"services"`
	Warnings       []string              `json:"warnings"`
	DryRun         bool                  `json:"dryRun"`
}

type ipAddressInfo struct {
	Family            string          `json:"family"`
	Local             string          `json:"local"`
	Scope             string          `json:"scope"`
	Flags             []string        `json:"flags"`
	Temporary         bool            `json:"temporary"`
	Deprecated        bool            `json:"deprecated"`
	Tentative         bool            `json:"tentative"`
	DADFailed         bool            `json:"dadfailed"`
	ValidLifeTime     json.RawMessage `json:"valid_life_time"`
	PreferredLifeTime json.RawMessage `json:"preferred_life_time"`
}

func flagPresent(flags []string, target string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, target) {
			return true
		}
	}
	return false
}

func positiveLifetime(raw json.RawMessage) (positive, known bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, false
	}
	value := strings.Trim(string(raw), `"`)
	if value == "forever" {
		return true, true
	}
	n, err := strconv.ParseUint(value, 10, 64)
	return n > 0, err == nil
}

// parsePublicAddresses reads the kernel's own report of global addresses. The
// interface flags matter as much as the address: a tentative or deprecated
// address accepts no traffic, and publishing one produces a name that resolves
// and never answers.
func parsePublicAddresses(data []byte) ([]publicCandidate, error) {
	var interfaces []struct {
		Name      string          `json:"ifname"`
		Flags     []string        `json:"flags"`
		OperState string          `json:"operstate"`
		Addresses []ipAddressInfo `json:"addr_info"`
	}
	if len(data) > 64<<10 || json.Unmarshal(data, &interfaces) != nil {
		return nil, errors.New("无法解析本机地址状态")
	}
	candidates := []publicCandidate{}
	seen := map[string]bool{}
	for _, iface := range interfaces {
		for _, address := range iface.Addresses {
			if address.Scope != "global" {
				continue
			}
			family := ""
			switch address.Family {
			case "inet6":
				family = "inet6"
			case "inet":
				family = "inet"
			default:
				continue
			}
			parsed, err := netip.ParseAddr(address.Local)
			if err != nil || (family == "inet6") != parsed.Is6() || parsed.Is4In6() {
				continue
			}
			key := iface.Name + "/" + parsed.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			valid, validKnown := positiveLifetime(address.ValidLifeTime)
			preferred, preferredKnown := positiveLifetime(address.PreferredLifeTime)
			candidate := publicCandidate{
				Address: parsed.String(), Family: family, Interface: iface.Name,
				Temporary:  address.Temporary || flagPresent(address.Flags, "temporary") || flagPresent(address.Flags, "secondary"),
				Deprecated: address.Deprecated || flagPresent(address.Flags, "deprecated") || preferredKnown && !preferred,
				Tentative:  address.Tentative || flagPresent(address.Flags, "tentative"),
			}
			usable := engine.ValidatePublicAddress(candidate.Address) == nil
			if family == "inet" {
				usable = engine.ValidPublicIPv4(candidate.Address)
			}
			switch {
			case !usable && family == "inet6":
				candidate.Reason = "不是可用的全球单播 IPv6 地址"
			case !usable:
				candidate.Reason = "不是可用的公网 IPv4 地址（私有、保留或运营商级 NAT 地址无法从外网访问）"
			case !flagPresent(iface.Flags, "UP") || strings.EqualFold(iface.OperState, "DOWN"):
				candidate.Reason = "网络接口未启用"
			case candidate.Temporary:
				candidate.Reason = "临时隐私地址，不适合作为固定公网监听"
			case candidate.Tentative || address.DADFailed || flagPresent(address.Flags, "dadfailed"):
				candidate.Reason = "地址重复检测未完成或失败"
			case candidate.Deprecated || validKnown && !valid:
				candidate.Reason = "地址已弃用或失效"
			case family == "inet6" && (!validKnown || !preferredKnown):
				candidate.Reason = "无法确认地址有效期，不能自动接受"
			default:
				candidate.Eligible = true
				if family == "inet6" {
					candidate.Reason = "本机非临时、有效 IPv6；仍需检查路由器放行和外部可达性"
				} else {
					candidate.Reason = "本机公网 IPv4；如需 IPv4 访问，仍需检查路由器端口转发，且运营商级 NAT 下无法生效"
				}
			}
			candidates = append(candidates, candidate)
			if len(candidates) > 128 {
				return nil, errors.New("本机地址过多，无法安全完成前检")
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Interface != candidates[j].Interface {
			return candidates[i].Interface < candidates[j].Interface
		}
		return candidates[i].Address < candidates[j].Address
	})
	return candidates, nil
}

// The writer limits capture while the process is running, not after allocating
// an unbounded CombinedOutput buffer. CommandContext also bounds process lifetime.
type publicCommandOutput struct {
	bytes.Buffer
	limit int
}

func (w *publicCommandOutput) Write(p []byte) (int, error) {
	remaining := w.limit - w.Len()
	if len(p) > remaining {
		n, _ := w.Buffer.Write(p[:remaining])
		return n, errors.New("command output limit exceeded")
	}
	return w.Buffer.Write(p)
}

func runPublicIP(ctx context.Context, limit int, args ...string) ([]byte, error) {
	child, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(child, "ip", args...)
	output := &publicCommandOutput{limit: limit}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.WaitDelay = 200 * time.Millisecond
	if err := cmd.Run(); err != nil {
		return nil, errors.New("本机 ip 命令不可用、超时或输出超限")
	}
	return output.Bytes(), nil
}

func fallbackPublicAddresses() []publicCandidate {
	candidates := []publicCandidate{}
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil || prefix.Addr().IsLoopback() || prefix.Addr().IsLinkLocalUnicast() {
				continue
			}
			candidates = append(candidates, publicCandidate{
				Address: prefix.Addr().String(), Family: familyOf(prefix.Addr()), Interface: iface.Name,
				Reason: "当前平台无法核实临时、弃用及重复检测标记；请在网关设备上检测",
			})
			if len(candidates) >= 128 {
				return candidates
			}
		}
	}
	return candidates
}

func familyOf(addr netip.Addr) string {
	if addr.Is4() {
		return "inet"
	}
	return "inet6"
}

// inspectPublicHost reports every global address this machine holds, in both
// families, plus the default routes. It never decides which one to publish —
// that is selectPublicAddress's job, and keeping the two apart is what lets the
// interface explain the choice instead of just announcing it.
func inspectPublicHost(ctx context.Context) ([]publicCandidate, string, []string) {
	if runtime.GOOS != "linux" {
		return fallbackPublicAddresses(), "仅 Linux 设备可读取默认路由", []string{"当前不是 Linux 设备，地址仅展示，不作为可应用候选。"}
	}
	warnings := []string{}
	candidates := []publicCandidate{}
	for _, family := range []string{"-6", "-4"} {
		data, err := runPublicIP(ctx, 64<<10, "-j", family, "addr", "show", "scope", "global")
		if err != nil {
			warnings = append(warnings, family+" 地址读取失败："+err.Error())
			continue
		}
		parsed, err := parsePublicAddresses(data)
		if err != nil {
			warnings = append(warnings, err.Error())
			continue
		}
		candidates = append(candidates, parsed...)
	}
	if len(candidates) == 0 {
		warnings = append(warnings, "没有读取到任何全局地址；回退地址不具备可应用资格。")
		candidates = fallbackPublicAddresses()
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Family != candidates[j].Family {
			return candidates[i].Family == "inet6"
		}
		if candidates[i].Interface != candidates[j].Interface {
			return candidates[i].Interface < candidates[j].Interface
		}
		return candidates[i].Address < candidates[j].Address
	})
	routes := []string{}
	for _, family := range []string{"-6", "-4"} {
		output, err := runPublicIP(ctx, 16<<10, family, "route", "show", "default")
		if err != nil {
			warnings = append(warnings, "默认路由读取失败（"+family+"）；不能确认上网路由。")
			continue
		}
		if text := strings.TrimSpace(string(output)); text != "" {
			routes = append(routes, text)
		} else {
			warnings = append(warnings, "没有发现默认路由（"+family+"）。")
		}
	}
	if len(routes) == 0 {
		return candidates, "不可用", warnings
	}
	return candidates, strings.Join(routes, "\n"), warnings
}

// Read the actual snapshot, never AppliedState's fallback to draft state.
func (a *App) publicAppliedState() (model.State, error) {
	var applied model.State
	data, err := readLimitedFile(filepath.Join(a.opts.DataDir, "applied.json"))
	if err != nil || len(data) > 2<<20 {
		return applied, errors.New("尚无可读取的已应用配置快照")
	}
	if err := json.Unmarshal(data, &applied); err != nil {
		return applied, errors.New("已应用配置快照无法解析")
	}
	if err := model.UpgradeSchema(&applied); err != nil {
		return applied, err
	}
	if err := checkSchema(applied); err != nil {
		return applied, err
	}
	return applied, nil
}

func findService(services []model.Service, id string) (model.Service, bool) {
	for _, s := range services {
		if s.ID == id {
			return s, true
		}
	}
	return model.Service{}, false
}

// serviceCheckRefusal answers the one thing a local entry check has to settle
// first: is *this* service the one the running configuration serves? It returns
// "" when it is, and otherwise the sentence to hand the operator.
//
// It used to ask whether the whole draft had been applied, which is a different
// question with a worse answer: one service waiting to be published made every
// other service on the page refuse to be checked, including ones that were
// running perfectly well. The check reads a live listener, so the only thing
// that can invalidate its reading is this service not being there.
//
// "The same service" means the same engine.ServiceGeneration -- the field set
// the generation hash covers, and the same one the row tags are built from. A
// renamed service is not a different service to the proxy, so it must not close
// a check that would still be accurate.
func serviceCheckRefusal(draft, applied model.State, service model.Service) string {
	// A snapshot that contradicts itself is not evidence about anything.
	if applied.Revision <= 0 || applied.AppliedRevision != applied.Revision {
		return "网关还没有一份自洽的已应用配置，先发布一次再看本机入口。"
	}
	saved, ok := findService(applied.Services, service.ID)
	if !ok {
		return "「" + service.Name + "」还没发布，本机现在没有它的入口。先发布这个服务；只想确认上游能不能连上，可以先「探测上游」。"
	}
	if engine.ServiceGeneration(applied, saved) != engine.ServiceGeneration(draft, service) {
		return "「" + service.Name + "」发布之后又改过，本机正在服务的还是上一版。先发布这个服务，检查的才是你现在改的这一版。"
	}
	return ""
}

// This read-only check never opens the private key. Key-pair validation remains
// the responsibility of certificate import and applying the proxy configuration.
func (a *App) publicCertificate(state model.State, service model.Service, now time.Time) (*x509.Certificate, string, error) {
	if !service.UsesDomains() {
		return nil, "", errors.New("按端口发布的服务不使用证书")
	}
	if !validID.MatchString(service.CertificateID) {
		return nil, "", errors.New("需要为该服务选择已导入的证书")
	}
	var metadata *model.Certificate
	for i := range state.Certificates {
		if state.Certificates[i].ID == service.CertificateID {
			metadata = &state.Certificates[i]
			break
		}
	}
	if metadata == nil || !metadata.Ready {
		return nil, "", errors.New("证书尚未成功导入")
	}
	dir, err := engine.CertificateDirectory(a.opts.DataDir, *metadata)
	if err != nil {
		return nil, "", err
	}
	data, err := readLimitedFile(filepath.Join(dir, "fullchain.pem"))
	if err != nil || len(data) > 2<<20 {
		return nil, "", errors.New("公开证书文件不可读取或过大")
	}
	remaining := bytes.TrimSpace(data)
	chain := []*x509.Certificate{}
	for len(remaining) > 0 {
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, "", errors.New("公开证书文件包含无效内容")
		}
		block, rest := pem.Decode(remaining)
		consumed := remaining[:len(remaining)-len(rest)]
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || bytes.Count(consumed, []byte("-----BEGIN ")) != 1 {
			return nil, "", errors.New("公开证书 PEM 格式无效")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil || now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
			return nil, "", errors.New("公开证书无效、尚未生效或已过期")
		}
		chain = append(chain, certificate)
		remaining = bytes.TrimSpace(rest)
	}
	if len(chain) == 0 {
		return nil, "", errors.New("公开证书文件为空")
	}
	for i := 0; i+1 < len(chain); i++ {
		if chain[i].CheckSignatureFrom(chain[i+1]) != nil {
			return nil, "", errors.New("公开证书链顺序或签名无效")
		}
	}
	leaf := chain[0]
	for _, domain := range service.Domains {
		if leaf.VerifyHostname(domain) != nil {
			return nil, "", errors.New("证书不覆盖域名 " + domain)
		}
	}
	sum := sha256.Sum256(leaf.Raw)
	fingerprint := hex.EncodeToString(sum[:])
	if !strings.EqualFold(fingerprint, metadata.Fingerprint) {
		return nil, "", errors.New("证书文件与已导入元数据不一致，请重新导入")
	}
	return leaf, fingerprint, nil
}

type publicDNSResolver interface {
	LookupIP(context.Context, string, string) ([]net.IP, error)
	LookupCNAME(context.Context, string) (string, error)
}

// lookupPublicDNS reports what the world currently resolves a name to.
//
// The two entry styles are checked against different facts, and neither check is
// meaningful for the other style.
//
// A directly published name must resolve to exactly one address — this device's.
// Extra or stale records send clients elsewhere, and because the device also
// terminates TLS here, an address that is not this device's fails visibly.
//
// A name published through EdgeOne must instead be a CNAME to the exact target
// EdgeOne assigned. Its addresses belong to a CDN and change on Tencent's
// schedule, so matching them would be both impossible and meaningless; the CNAME
// is the only local fact that distinguishes a name correctly fronted by this
// site from one pointed at a different EdgeOne site. That second case is the one
// worth catching: it resolves, it serves over valid TLS, and it serves someone
// else's content — the failure this gateway would otherwise never notice.
func lookupPublicDNS(ctx context.Context, resolver publicDNSResolver, domain, address, expectCNAME string) publicDNSStatus {
	status := publicDNSStatus{Family: "AAAA", Records: []string{}}
	network := "ip6"
	configured, parseErr := netip.ParseAddr(address)
	if parseErr != nil {
		status.Error = "已应用快照没有可用的公网地址"
		return status
	}
	if configured.Is4() {
		network, status.Family = "ip4", "A"
	}
	ips, err := resolver.LookupIP(ctx, network, domain)
	if err != nil {
		// For an EdgeOne-fronted name a failed A/AAAA lookup is expected rather
		// than a fault: the record is a CNAME, and some resolvers answer the CNAME
		// without following it. It is recorded as an observation, not an error,
		// because the CNAME check below is the one that decides.
		if expectCNAME == "" {
			status.Error = status.Family + " 解析失败或超时"
		}
	} else {
		seen := map[string]bool{}
		for _, ip := range ips {
			if (network == "ip6") != (ip.To4() == nil) || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			status.Records = append(status.Records, ip.String())
		}
		sort.Strings(status.Records)
		if len(status.Records) == 0 && expectCNAME == "" {
			status.Error = "尚未观察到 " + status.Family + " 记录"
		}
	}
	canonical, cnameErr := resolver.LookupCNAME(ctx, domain)
	if cnameErr == nil && !sameName(canonical, domain) {
		status.CNAME = strings.TrimSuffix(canonical, ".")
	}
	if expectCNAME != "" {
		switch {
		case cnameErr != nil:
			status.Error = "未能读到 CNAME 目标，无法确认该域名是否指向 EdgeOne"
			return status
		case status.CNAME == "":
			status.Error = "该域名不是 CNAME 记录，EdgeOne 分配的 " + expectCNAME + " 没有被使用"
			return status
		case !sameName(status.CNAME, expectCNAME):
			status.Error = "该域名的 CNAME 指向 " + status.CNAME + "，而 EdgeOne 分配的是 " + expectCNAME + "，可能指向了别的站点"
			return status
		}
		status.Matches = true
		return status
	}
	if cnameErr != nil && status.Error == "" {
		status.Error = "CNAME 检查失败或超时"
	}
	status.Matches = parseErr == nil && len(status.Records) == 1 && status.Records[0] == configured.String() && status.Error == ""
	return status
}

// sameName compares two DNS names, tolerating case and the trailing root dot.
func sameName(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(a), "."), strings.TrimSuffix(strings.TrimSpace(b), "."))
}

// entryDescription says where a service is reached. A service published through
// EdgeOne is still reached at https://<name>; what differs is who answers there,
// which is what the entry mode reports — the address this device holds is not
// where a client goes for such a name, and printing it would send the operator
// to a URL that answers only on the device's own network.
func entryDescription(service model.Service, publicAddress string) (string, int, []string) {
	if service.UsesDomains() {
		return "https://" + service.Domains[0], engine.DomainEntryPort, service.Domains
	}
	// Plain http: a port entry carries no certificate, so the rendered server
	// block has no ssl and there is nothing to negotiate. Saying https here sent
	// the operator to a URL that refuses the connection, which reads exactly like
	// a gateway that is down. The local-entry check already probes http.
	port := service.PublishedPort()
	if publicAddress != "" {
		return "http://" + net.JoinHostPort(publicAddress, strconv.Itoa(port)), port, []string{}
	}
	return net.JoinHostPort("", strconv.Itoa(port)), port, []string{}
}

// dnsJournalValue reads the record value the gateway last published for a name.
// For a name published through EdgeOne that value is the CNAME EdgeOne assigned,
// which is the only local record of the target the name must point at — and it
// costs no cloud call to read.
func dnsJournalValue(journal dnsJournal, host string) (string, bool) {
	entry, ok := journal.Managed[strings.ToLower(strings.TrimSpace(host))]
	if !ok || entry.Record.Value == "" {
		return "", false
	}
	return entry.Record.Value, true
}

func (a *App) publicPreflight(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	state := clone(a.state)
	applied, appliedErr := a.publicAppliedState()
	publishedNames, _ := a.readDNSJournal()
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	candidates, routes, warnings := a.inspectHost(ctx)
	result := publicPreflightResult{
		CheckedAt: time.Now().UTC().Format(time.RFC3339), Candidates: candidates,
		Routes: routes, Services: []publicServiceStatus{}, Warnings: warnings, DryRun: a.opts.DryRun,
		AppliedAddress: applied.AppliedPublicAddress,
	}
	if appliedErr != nil {
		result.Warnings = append(result.Warnings, appliedErr.Error())
	}
	if a.opts.DryRun {
		result.Warnings = append(result.Warnings, "开发 dry-run 模式：没有真实公网监听，配置应用记录不代表 Nginx 已运行。")
	}
	services := append([]model.Service{}, state.Services...)
	sort.Slice(services, func(i, j int) bool {
		if services[i].Enabled != services[j].Enabled {
			return services[i].Enabled
		}
		return services[i].ID < services[j].ID
	})
	if len(services) > 12 {
		services = services[:12]
	}
	dnsContext, dnsCancel := context.WithTimeout(ctx, 3*time.Second)
	defer dnsCancel()
	for _, service := range services {
		entry, port, domains := entryDescription(service, applied.AppliedPublicAddress)
		mode := service.EntryMode
		if !service.UsesDomains() {
			mode = ""
		} else if mode == "" {
			mode = model.EntryDirect
		}
		status := publicServiceStatus{
			ServiceID: service.ID, Name: service.Name, Entry: entry, Domains: domains, EntryMode: mode,
			Address: applied.AppliedPublicAddress, Port: port,
			DraftRevision: state.Revision, AppliedRevision: state.AppliedRevision, EdgeProvider: service.EffectiveEdgeProvider(),
			ConfigurationApplied: !a.opts.DryRun && appliedErr == nil && service.Enabled && serviceCheckRefusal(state, applied, service) == "",
			DNS:                  publicDNSStatus{Family: "AAAA", Records: []string{}}, LocalTLS: publicCheckStatus{Status: "not-checked"}, External: publicCheckStatus{Status: "unverified"},
		}
		if !service.UsesDomains() {
			status.DNS.Error = "按端口发布的服务不需要解析记录"
		} else {
			_, fingerprint, err := a.publicCertificate(state, service, time.Now())
			status.CertificateReady = err == nil
			status.CertificateFingerprint = fingerprint
			if err != nil {
				status.CertificateError = err.Error()
			}
			switch {
			case engine.ValidateService(service, state.Settings) != nil:
				status.DNS.Error = "服务草稿配置无效，已跳过 DNS 查询"
			case service.ServesEdge():
				// The name must be a CNAME to whatever EdgeOne assigned, and that
				// value is only known locally once the record has been published.
				// Comparing against this device's address instead would report a
				// correct EdgeOne-fronted name as broken.
				if published, ok := dnsJournalValue(publishedNames, service.Domains[0]); ok {
					status.DNS = lookupPublicDNS(dnsContext, net.DefaultResolver, service.Domains[0], applied.AppliedPublicAddress, published)
					if service.ServesCloudflare() {
						status.DNS.Error = strings.ReplaceAll(status.DNS.Error, "EdgeOne", "Cloudflare")
						// LookupCNAME follows the entire alias chain. A third-party
						// preferred name can itself alias another edge name.
						if !status.DNS.Matches && status.DNS.CNAME != "" {
							if canonical, err := net.DefaultResolver.LookupCNAME(dnsContext, published); err == nil && sameName(canonical, status.DNS.CNAME) {
								status.DNS.Matches, status.DNS.Error = true, ""
							}
						}
					}
				} else {
					status.DNS.Error = "该域名由边缘节点应答，解析尚未发布；发布解析后才能核对 CNAME 目标"
				}
			case applied.AppliedPublicAddress == "":
				status.DNS.Error = "尚无可用的公网地址，已跳过 DNS 查询"
			default:
				status.DNS = lookupPublicDNS(dnsContext, net.DefaultResolver, service.Domains[0], applied.AppliedPublicAddress, "")
			}
		}
		result.Services = append(result.Services, status)
	}
	reply(w, http.StatusOK, result)
}

func checkPublicTLS(ctx context.Context, host, address string, port int, leaf *x509.Certificate, fingerprint string) error {
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	dialer := &tls.Dialer{Config: &tls.Config{
		ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: roots,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("missing peer certificate")
			}
			sum := sha256.Sum256(state.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != fingerprint {
				return errors.New("certificate fingerprint mismatch")
			}
			return nil
		},
	}}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
	if err != nil {
		return errors.New("网关本机公网入口连接或 TLS 证书校验失败；这不是外部可达性检测")
	}
	connection.Close()
	return nil
}

func (a *App) checkPublicService(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := r.PathValue("id")
	index := a.serviceIndex(id)
	if index < 0 {
		fail(w, http.StatusNotFound, "服务不存在")
		return
	}
	service := a.state.Services[index]
	if !service.Enabled {
		fail(w, http.StatusConflict, "该服务尚未启用")
		return
	}
	applied, err := a.publicAppliedState()
	if err != nil {
		fail(w, http.StatusConflict, "网关还没有一份已应用的配置，先发布一次再看本机入口。")
		return
	}
	if refusal := serviceCheckRefusal(a.state, applied, service); refusal != "" {
		fail(w, http.StatusConflict, refusal)
		return
	}
	// A port entry is checked through its own number, so the verdict is reported
	// under the key that names what was checked. A dry run reports the same shape
	// as a real check, with the address the applied snapshot was rendered for and
	// nothing claimed that could not have been observed.
	key := "localTLS"
	if !service.UsesDomains() {
		key = "localEntry"
	}
	if a.opts.DryRun {
		reply(w, http.StatusOK, map[string]any{key: publicCheckStatus{Status: "not-checked", Error: "开发 dry-run 模式没有真实监听"}, "external": publicCheckStatus{Status: "unverified"}, "address": applied.AppliedPublicAddress})
		return
	}
	// The running configuration is what is being checked, so every address it is
	// reached through comes from the snapshot that rendered it. The draft may
	// name a different gateway address; dialing that would be probing a listener
	// that does not exist yet and calling the answer a verdict on the running one.
	if err := engine.ValidateService(service, applied.Settings); err != nil {
		fail(w, http.StatusConflict, "已应用服务配置无效，请重新预览并应用")
		return
	}
	address := applied.AppliedPublicAddress
	if address == "" {
		address = applied.Settings.GatewayIP
	}
	ctx, cancel := context.WithTimeout(r.Context(), 7*time.Second)
	defer cancel()
	var checkErr error
	if service.UsesDomains() {
		leaf, fingerprint, certErr := a.publicCertificate(a.state, service, time.Now())
		checkErr = certErr
		if certErr == nil {
			for _, domain := range service.Domains {
				if err := checkPublicTLS(ctx, domain, applied.Settings.GatewayIP, engine.DomainEntryPort, leaf, fingerprint); err != nil {
					checkErr = err
					break
				}
			}
		}
	} else {
		// A port entry speaks whatever its upstream speaks; a plain request to the
		// gateway's own port proves the whole chain — proxy, tunnel, upstream —
		// answers on the number the operator published.
		target := "http://" + net.JoinHostPort(applied.Settings.GatewayIP, strconv.Itoa(service.PublishedPort())) + "/"
		probe := engine.Probe(ctx, target)
		if !probe.OK {
			checkErr = errors.New("本机端口入口未返回成功响应：" + probe.Error)
		}
	}
	status := publicCheckStatus{Status: "ok"}
	if checkErr != nil {
		status = publicCheckStatus{Status: "error", Error: checkErr.Error()}
	}
	reply(w, http.StatusOK, map[string]any{key: status, "external": publicCheckStatus{Status: "unverified"}, "address": address})
}

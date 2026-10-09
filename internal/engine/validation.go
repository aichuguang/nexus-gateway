package engine

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"w103d/gateway/internal/model"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var publicIPv6Network = netip.MustParsePrefix("2000::/3")

// Conservatively exclude special-purpose allocations inside global unicast.
// IsGlobalUnicast alone also accepts documentation and benchmarking addresses.
var specialIPv6Networks = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"),         // IETF protocol assignments, including Teredo/benchmark/ORCHID.
	netip.MustParsePrefix("2001:db8::/32"),     // Documentation.
	netip.MustParsePrefix("2002::/16"),         // Deprecated 6to4, not a native global listener.
	netip.MustParsePrefix("2620:4f:8000::/48"), // AS112 special-purpose service.
	netip.MustParsePrefix("3fff::/20"),         // Documentation.
}

// Ports the gateway itself occupies. A published service may not take one:
// 443 is where named services are separated by SNI, 80 answers the plain request
// a bare domain produces and sends it to that same name over TLS, and the other
// two belong to the management UI and the activation probe.
const (
	DomainEntryPort  = 443
	HTTPRedirectPort = 80
	ManagementPort   = 18080
	GenerationPort   = 18079
)

func reservedPort(port int) bool {
	return port == DomainEntryPort || port == HTTPRedirectPort || port == ManagementPort || port == GenerationPort
}

// ValidatePublicAddress accepts only a literal native global IPv6 address.
// This does not prove local assignment or reachability from the Internet.
func ValidatePublicAddress(address string) error {
	a, err := netip.ParseAddr(address)
	if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" || !publicIPv6Network.Contains(a) {
		return fmt.Errorf("公网入口需要一个 2000::/3 内的全球单播 IPv6 字面地址，且不带 zone")
	}
	for _, prefix := range specialIPv6Networks {
		if prefix.Contains(a) {
			return fmt.Errorf("公网入口不能使用特殊用途 IPv6 地址")
		}
	}
	return nil
}

// ValidPublicIPv4 reports whether an address could actually receive inbound
// connections from the Internet. This is stricter than "is a valid IPv4 address"
// on purpose: a name pointing at 192.168.x.x or at a carrier-grade NAT address
// resolves perfectly and can never be reached, which is worse to debug than a
// name that simply does not exist.
func ValidPublicIPv4(address string) bool {
	a, err := netip.ParseAddr(address)
	if err != nil || !a.Is4() || a.Is4In6() {
		return false
	}
	for _, blocked := range nonPublicIPv4 {
		if blocked.Contains(a) {
			return false
		}
	}
	return true
}

var nonPublicIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),      // private
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local
	netip.MustParsePrefix("172.16.0.0/12"),   // private
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("192.168.0.0/16"),  // private
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, including 255.255.255.255
}

// ValidVPNIPv4 accepts a unicast IPv4 literal without imposing a deployment
// subnet. The operator controls VPN addressing and routing; without a prefix,
// a final octet of 0 or 255 does not identify a network or broadcast address.
func ValidVPNIPv4(address string) bool {
	a, err := netip.ParseAddr(address)
	if err != nil || !a.Is4() || !a.IsGlobalUnicast() {
		return false
	}
	first := a.As4()[0]
	return first != 0 && first < 224 // Exclude "this network" and reserved ranges.
}

// ValidDomain reports whether s is a syntactically usable DNS name.
func ValidDomain(s string) bool { return validDomain(s) }

func validDomain(s string) bool {
	if len(s) == 0 || len(s) > 253 || strings.TrimSpace(s) != s {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func validateSettings(settings model.Settings) error {
	if !ValidVPNIPv4(settings.GatewayIP) {
		return fmt.Errorf("网关地址必须是有效的 VPN IPv4 单播地址")
	}
	return nil
}

// NormalizeUpstream accepts the shorthand an operator actually types — bare
// "192.168.50.20:5666" — and returns the canonical form. Defaulting the scheme
// rather than rejecting the input is deliberate: the upstreams in this network
// are overwhelmingly plain HTTP, and forcing four extra characters to say so is
// friction with no safety value. The stored value is always explicit.
func NormalizeUpstream(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	if !strings.Contains(raw, "://") {
		return "http://" + raw
	}
	return raw
}

func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(NormalizeUpstream(raw))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
		return nil, fmt.Errorf("上游地址必须以 http 或 https 开头")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("上游地址不能包含账号、查询串或锚点")
	}
	if u.Path != "" && u.Path != "/" || u.RawPath != "" {
		return nil, fmt.Errorf("上游地址不能包含子路径")
	}
	if !ValidVPNIPv4(u.Hostname()) {
		return nil, fmt.Errorf("上游地址必须是有效的 IPv4 单播地址")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Host != u.Hostname()+":"+strconv.Itoa(port) {
		return nil, fmt.Errorf("上游地址必须带 1 到 65535 之间的端口")
	}
	return u, nil
}

// ValidateService checks one published upstream. The entry style is decided by
// whether the service carries domains, so the two are validated as an either/or
// rather than as independent optional fields.
func ValidateService(s model.Service, settings model.Settings) error {
	if err := validateSettings(settings); err != nil {
		return err
	}
	if !identifier.MatchString(s.ID) {
		return fmt.Errorf("服务 ID 必须是 1 到 64 位的字母、数字、下划线或连字符")
	}
	u, err := parseUpstream(s.Upstream)
	if err != nil {
		return err
	}
	if h := s.HealthCheck; h != nil {
		if h.Path != "" {
			p, err := url.Parse(h.Path)
			if err != nil || len(h.Path) > 2048 || !strings.HasPrefix(h.Path, "/") || strings.HasPrefix(h.Path, "//") || strings.ContainsAny(h.Path, "\\\r\n") || p.IsAbs() || p.Host != "" || p.Fragment != "" {
				return fmt.Errorf("health check path must be a same-origin absolute path without a fragment")
			}
		}
		if len(h.ExpectedStatusCodes) > 100 {
			return fmt.Errorf("too many expected health status codes")
		}
		seen := map[int]bool{}
		for _, code := range h.ExpectedStatusCodes {
			if code < 100 || code > 599 || seen[code] {
				return fmt.Errorf("invalid or duplicate expected health status code")
			}
			seen[code] = true
		}
	}
	if u.Hostname() == settings.GatewayIP {
		return fmt.Errorf("上游不能指向网关自己")
	}
	if name := strings.TrimSpace(s.Name); name == "" || len(name) > 60 {
		return fmt.Errorf("服务名不能为空，且不超过 60 字节")
	}

	seen := map[string]bool{}
	for _, domain := range s.Domains {
		d := strings.ToLower(strings.TrimSpace(domain))
		if d != domain {
			return fmt.Errorf("域名 %q 必须是小写且首尾无空格", domain)
		}
		if !validDomain(d) || !strings.Contains(d, ".") {
			return fmt.Errorf("域名 %s 不是完整的 DNS 名称", domain)
		}
		if strings.EqualFold(d, settings.BaseDomain) {
			return fmt.Errorf("域名 %s 已用作基站域名，不能同时绑定服务", domain)
		}
		if seen[d] {
			return fmt.Errorf("同一个服务不能重复绑定域名 %s", d)
		}
		seen[d] = true
	}

	if s.UsesDomains() {
		// A named service shares port 443 with every other named service, so a
		// published port would be meaningless on it — refuse rather than store a
		// field nothing reads.
		if s.Port != 0 {
			return fmt.Errorf("已绑定域名的服务通过域名访问，不需要再占用端口")
		}
		if s.CertificateID != "" && !identifier.MatchString(s.CertificateID) {
			return fmt.Errorf("证书 ID 无效")
		}
		if s.EntryMode != "" && s.EntryMode != model.EntryDirect && s.EntryMode != model.EntryEdge {
			return fmt.Errorf("入口方式只能是直接发布或边缘加速")
		}
		if s.EntryMode == model.EntryEdge && s.CertificateID == "" {
			// EdgeOne fetches from this device over HTTPS and needs a certificate to
			// bind at its own edge, so a service published through it without one
			// cannot be provisioned at all.
			return fmt.Errorf("以边缘加速发布的服务必须绑定源站证书")
		}
		if s.EdgeProvider != "" && s.EdgeProvider != "edgeone" && s.EdgeProvider != "cloudflare" {
			return fmt.Errorf("不支持的边缘提供商")
		}
		if !s.ServesEdge() && (s.EdgeProvider != "" || s.EdgeAccountID != "" || s.UsePreferred || s.PreferredDomain != "") {
			return fmt.Errorf("直接发布不能携带边缘加速配置")
		}
		if s.ServesCloudflare() {
			if !identifier.MatchString(s.EdgeAccountID) {
				return fmt.Errorf("请选择 Cloudflare 边缘账号")
			}
			if settings.BaseDomain == "" {
				return fmt.Errorf("Cloudflare 接入需要先设置基站域名")
			}
			if s.UsePreferred && s.PreferredDomain == "" {
				return fmt.Errorf("启用优选时必须填写第三方优选域名")
			}
			if s.PreferredDomain != "" {
				if s.PreferredDomain != strings.ToLower(strings.TrimSpace(s.PreferredDomain)) || !validDomain(s.PreferredDomain) || !strings.Contains(s.PreferredDomain, ".") {
					return fmt.Errorf("优选入口必须是完整域名，不能包含协议、端口或路径")
				}
				if s.PreferredDomain == settings.BaseDomain {
					return fmt.Errorf("优选入口不能是源站基站域名")
				}
				for _, host := range s.Domains {
					if host == s.PreferredDomain {
						return fmt.Errorf("优选入口不能指向业务域名自身")
					}
				}
			}
		} else if s.UsePreferred || s.PreferredDomain != "" || s.EdgeAccountID != "" {
			return fmt.Errorf("第三方优选和指定边缘账号仅适用于 Cloudflare")
		}
		return nil
	}
	if s.EntryMode != "" {
		return fmt.Errorf("按端口发布的服务没有域名，不能设置入口方式")
	}
	if s.EdgeProvider != "" || s.EdgeAccountID != "" || s.UsePreferred || s.PreferredDomain != "" {
		return fmt.Errorf("端口服务不能设置边缘加速")
	}

	port := s.PublishedPort()
	if port < 1 || port > 65535 {
		return fmt.Errorf("对外端口必须在 1 到 65535 之间")
	}
	if reservedPort(port) {
		return fmt.Errorf("端口 %d 由网关自己占用，请换一个", port)
	}
	if s.CertificateID != "" {
		return fmt.Errorf("没有域名的服务按端口访问，不需要证书")
	}
	return nil
}

// CertificateCovers reports whether a certificate's declared domains cover host,
// including a single leading wildcard label.
func CertificateCovers(cert model.Certificate, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, pattern := range cert.Domains {
		p := strings.ToLower(strings.TrimSpace(pattern))
		if p == host {
			return true
		}
		if strings.HasPrefix(p, "*.") && strings.HasSuffix(host, p[1:]) && strings.Count(host, ".") == strings.Count(p, ".") {
			return true
		}
	}
	return false
}

// CertificateCoversAll reports whether the certificate covers every domain, so a
// service is never published with a name its certificate does not speak for.
func CertificateCoversAll(cert model.Certificate, domains []string) error {
	for _, domain := range domains {
		if !CertificateCovers(cert, domain) {
			return fmt.Errorf("证书不覆盖域名 %s", domain)
		}
	}
	return nil
}

// ValidateDNSAccount checks an account identity record. Secret material is not
// part of the model.
func ValidateDNSAccount(a model.DNSAccount) error {
	if !identifier.MatchString(a.ID) {
		return fmt.Errorf("DNS 账号 ID 必须是 1 到 64 位的字母、数字、下划线或连字符")
	}
	if a.Provider != "dnspod" && a.Provider != "cloudflare" && a.Provider != "alidns" {
		return fmt.Errorf("不支持的 DNS 提供商")
	}
	return nil
}

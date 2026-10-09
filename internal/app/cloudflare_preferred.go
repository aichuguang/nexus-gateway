package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

type cfDNSRecord struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	Data string `json:"data"`
}
type cfDNSAnswer struct {
	Status int           `json:"Status"`
	Answer []cfDNSRecord `json:"Answer"`
}

// Public DoH exposes each CNAME in the answer. net.LookupCNAME only returns the
// final canonical name, hiding preferred -> business -> standard chains that
// become loops when business is changed to preferred. No request ever goes to
// the preferred host or to an address supplied by it; this is not a speed test.
func resolveCFPreferred(ctx context.Context, host string) (cfDNSAnswer, error) {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 3 * time.Second, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return resolveCFPreferredWithClient(ctx, host, client)
}

// Fail over only when a fixed public DoH service cannot provide a valid reply.
// A real negative answer or unsafe DNS chain is not bypassed by trying another
// resolver. No credentials, user-configurable URL or redirect is accepted here.
func resolveCFPreferredWithClient(ctx context.Context, host string, client *http.Client) (cfDNSAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	safeClient := *client
	safeClient.Jar = nil
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, endpoint := range []string{"https://cloudflare-dns.com/dns-query", "https://doh.pub/dns-query", "https://dns.google/resolve"} {
		if ctx.Err() != nil {
			break
		}
		queryCtx, done := context.WithTimeout(ctx, 3*time.Second)
		answer, err := queryCFPreferredDoH(queryCtx, host, endpoint, &safeClient)
		done()
		if err == nil {
			return answer, nil
		}
	}
	return cfDNSAnswer{}, errors.New("优选域名 DNS 查询服务暂不可达或响应无效，尚未完成入口校验；未修改业务 DNS")
}

func queryCFPreferredDoH(ctx context.Context, host, endpoint string, client *http.Client) (cfDNSAnswer, error) {
	var out cfDNSAnswer
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?type=A&name="+url.QueryEscape(host), nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Accept", "application/dns-json")
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, errors.New("DNS query service unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return out, errors.New("DNS query response incomplete or oversized")
	}
	var reply struct {
		Status *int          `json:"Status"`
		TC     bool          `json:"TC"`
		Answer []cfDNSRecord `json:"Answer"`
	}
	if json.Unmarshal(body, &reply) != nil || reply.Status == nil || *reply.Status < 0 || *reply.Status > 15 || reply.TC {
		return out, errors.New("DNS query response invalid")
	}
	return cfDNSAnswer{Status: *reply.Status, Answer: reply.Answer}, nil
}

var cfNonPublicV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
}

func cfPublicIPv4(raw string) bool {
	ip, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range cfNonPublicV4 {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func (a *App) checkCFPreferredLocked(ctx context.Context, target, fallback string) error {
	resolve := a.cloudflareResolve
	if resolve == nil {
		resolve = resolveCFPreferred
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	blocked := map[string]bool{a.baseDomain(): true, cfDomain(fallback): true}
	for _, s := range a.state.Services {
		for _, h := range s.Domains {
			blocked[cfDomain(h)] = true
		}
	}
	current := cfDomain(target)
	seen := map[string]bool{}
	hops := 0
	for query := 0; query < 8; query++ {
		if !validCFDomain(current) || blocked[current] || seen[current] {
			return errors.New("优选 CNAME 链引用业务域名、基站、fallback 或形成循环，拒绝发布")
		}
		answer, err := resolve(ctx, current)
		if err != nil {
			return err
		}
		if answer.Status != 0 {
			if answer.Status == 3 {
				return errors.New("DNS 查询返回 NXDOMAIN，优选域名不存在；未修改业务 DNS")
			}
			if answer.Status == 2 {
				return errors.New("DNS 查询返回 SERVFAIL，尚无法确认优选入口；未修改业务 DNS")
			}
			return errors.New("DNS 查询未成功返回优选域名记录；未修改业务 DNS")
		}
		aliases := map[string]string{}
		addresses := map[string][]string{}
		for _, r := range answer.Answer {
			name := cfDomain(r.Name)
			switch r.Type {
			case 5:
				next := cfDomain(r.Data)
				if old, ok := aliases[name]; ok && old != next {
					return errors.New("优选域名包含不一致的 CNAME")
				}
				aliases[name] = next
			case 1:
				addresses[name] = append(addresses[name], r.Data)
			}
		}
		progressed := false
		for {
			if ips := addresses[current]; len(ips) > 0 {
				if aliases[current] != "" {
					return errors.New("优选名称同时包含 A 和 CNAME，拒绝模糊解析")
				}
				for _, ip := range ips {
					if !cfPublicIPv4(ip) {
						return errors.New("优选域名包含非公网 IPv4 地址，不能用作公网入口")
					}
				}
				return nil
			}
			next := aliases[current]
			if next == "" {
				break
			}
			seen[current] = true
			if !validCFDomain(next) || blocked[next] || seen[next] {
				return errors.New("优选 CNAME 链引用业务域名、基站、fallback 或形成循环，拒绝发布")
			}
			current = next
			hops++
			progressed = true
			if hops >= 8 {
				return errors.New("优选 CNAME 链过长，请更换入口")
			}
		}
		// The response may contain only a CNAME. Query its target once, rather
		// than treating a missing additional A section as successful IPv4 support.
		if !progressed {
			return errors.New("优选域名没有可用公网 IPv4 A 记录")
		}
	}
	return errors.New("优选 CNAME 链过长或没有可用公网 IPv4 A 记录")
}

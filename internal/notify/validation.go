package notify

import (
	"context"
	"errors"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func Validate(c Channel) error {
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 100 || strings.ContainsAny(c.Name, "\r\n\x00") {
		return errors.New("通知渠道名称不能为空、不能超过 100 字节或含换行")
	}
	if len(c.Secret) > 4096 || strings.ContainsAny(c.Secret, "\r\n\x00") || len(c.SMTPPassword) > 4096 {
		return errors.New("通知密钥格式不正确")
	}
	if c.Type == "smtp" {
		if !validHost(c.SMTPHost) || c.SMTPPort < 1 || c.SMTPPort > 65535 || (c.SMTPTLS != "tls" && c.SMTPTLS != "starttls") {
			return errors.New("SMTP 需要有效的公网主机、端口及 tls 或 starttls 模式")
		}
		if !emailAddress(c.SMTPFrom) || len(c.SMTPTo) == 0 || len(c.SMTPTo) > 10 {
			return errors.New("SMTP 需要有效发件地址和 1 至 10 个收件地址")
		}
		for _, recipient := range c.SMTPTo {
			if !emailAddress(recipient) {
				return errors.New("SMTP 收件地址格式不正确")
			}
		}
		if len(c.SMTPUsername) > 254 || strings.ContainsAny(c.SMTPUsername, "\r\n\x00") || (c.SMTPUsername != "" && c.SMTPPassword == "") {
			return errors.New("SMTP 登录账号需要密码，且账号格式必须有效")
		}
		return nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || len(c.Endpoint) > 4096 || u.Scheme != "https" || u.User != nil || u.Fragment != "" || !validHost(u.Hostname()) || strings.ContainsAny(c.Endpoint, "\r\n\x00") {
		return errors.New("通知地址必须是有效的公网 HTTPS URL，不能包含用户信息或片段")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("通知地址端口无效")
		}
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("通知地址查询参数无效")
	}
	switch c.Type {
	case "wecom":
		if u.Host != "qyapi.weixin.qq.com" || u.Path != "/cgi-bin/webhook/send" || len(query) != 1 || len(query["key"]) != 1 || query.Get("key") == "" {
			return errors.New("请填写企业微信机器人完整 Webhook 地址")
		}
	case "feishu":
		if u.Host != "open.feishu.cn" || !strings.HasPrefix(u.Path, "/open-apis/bot/v2/hook/") || strings.TrimPrefix(u.Path, "/open-apis/bot/v2/hook/") == "" || u.RawQuery != "" {
			return errors.New("请填写飞书自定义机器人的完整 Webhook 地址")
		}
	case "dingtalk":
		if u.Host != "oapi.dingtalk.com" || u.Path != "/robot/send" || len(query) != 1 || len(query["access_token"]) != 1 || query.Get("access_token") == "" {
			return errors.New("请填写钉钉机器人完整 Webhook 地址；签名密钥单独填写")
		}
	case "webhook":
	default:
		return errors.New("不支持此通知渠道类型")
	}
	return nil
}

func emailAddress(raw string) bool {
	if len(raw) > 254 || strings.ContainsAny(raw, "\r\n\x00") {
		return false
	}
	address, err := mail.ParseAddress(raw)
	return err == nil && address.Address == raw && strings.Contains(raw, "@")
}
func validHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return publicIP(ip)
	}
	if len(host) > 253 || !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
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

var blockedNetworks = func() []netip.Prefix {
	var out []netip.Prefix
	for _, raw := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3", "::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"} {
		out = append(out, netip.MustParsePrefix(raw))
	}
	return out
}()

func publicIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || (address.Is6() && !netip.MustParsePrefix("2000::/3").Contains(address)) {
		return false
	}
	for _, blocked := range blockedNetworks {
		if blocked.Contains(address) {
			return false
		}
	}
	return true
}

// Resolve at connection time and dial the checked literal, preventing redirects
// or DNS rebinding from turning a notification endpoint into a private scan.
func publicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("通知目标地址无效")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, errors.New("通知目标域名无法解析")
	}
	if len(addresses) == 0 {
		return nil, errors.New("通知目标没有可用地址")
	}
	for _, address := range addresses {
		if !publicIP(address.IP) {
			return nil, errors.New("通知目标不能解析到私网或特殊用途地址")
		}
	}
	for _, address := range addresses {
		conn, err := (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, network, net.JoinHostPort(address.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.New("通知目标连接失败")
}

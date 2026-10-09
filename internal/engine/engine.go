package engine

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"w103d/gateway/internal/atomicfile"
	"w103d/gateway/internal/model"
)

//go:embed cookie_guard.js
var cookieGuardScript []byte

type Engine struct {
	DataDir    string
	NginxBin   string
	ConfigPath string
	CacheDir   string
	DryRun     bool
	// Test seam for fake-nginx activation tests; never configured via the API.
	generationProbe func(expected string) error
	startupHooks    *proxyStartupHooks
}

func safePath(path string) (string, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n\"$;{}") {
		return "", fmt.Errorf("managed paths must be absolute and contain no configuration metacharacters")
	}
	return `"` + filepath.ToSlash(filepath.Clean(path)) + `"`, nil
}

func (e *Engine) paths() (configPath, tempDir string, err error) {
	if _, err = safePath(e.DataDir); err != nil {
		return "", "", err
	}
	configPath = e.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(e.DataDir, "managed-nginx.conf")
	}
	if _, err = safePath(configPath); err != nil {
		return "", "", err
	}
	rel, relErr := filepath.Rel(e.DataDir, configPath)
	if relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("nginx configuration must be inside the managed data directory")
	}
	if strings.HasPrefix(filepath.ToSlash(filepath.Clean(e.DataDir))+"/", "/etc/nginx/") {
		return "", "", fmt.Errorf("system nginx configuration directories cannot be managed")
	}
	tempDir = e.CacheDir
	if tempDir == "" {
		tempDir = "/var/cache/gateway"
	}
	if _, err = safePath(tempDir); err != nil {
		return "", "", err
	}
	return configPath, tempDir, nil
}

// Prepare creates only managed paths. The proxy workers cannot traverse DataDir;
// their temporary files are held in a separate, worker-owned cache directory.
func (e *Engine) Prepare() error {
	configPath, cacheDir, err := e.paths()
	if err != nil {
		return err
	}
	if e.DryRun {
		return nil
	}
	worker, err := user.Lookup("www-data")
	if err != nil {
		return fmt.Errorf("nginx worker account www-data is unavailable")
	}
	uid, uidErr := strconv.Atoi(worker.Uid)
	gid, gidErr := strconv.Atoi(worker.Gid)
	if uidErr != nil || gidErr != nil {
		return fmt.Errorf("nginx worker account IDs are invalid")
	}
	if err = os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		return fmt.Errorf("cannot create managed configuration directory")
	}
	proofKey, err := prepareOriginProofKey(e.DataDir)
	if err != nil {
		return fmt.Errorf("cannot prepare origin proof key: %w", err)
	}
	script := strings.ReplaceAll(string(cookieGuardScript), "__ORIGIN_PROOF_KEY__", proofKey)
	if err = atomicWrite(filepath.Join(e.DataDir, "cookie_guard.js"), []byte(script)); err != nil {
		return fmt.Errorf("cannot prepare required nginx cookie isolation module: %w", err)
	}
	if err = os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("cannot create nginx cache directory")
	}
	if err = os.Chmod(cacheDir, 0755); err != nil {
		return fmt.Errorf("cannot prepare nginx cache directory")
	}
	for _, name := range []string{"client_temp", "proxy_temp"} {
		path := filepath.Join(cacheDir, name)
		if err = os.MkdirAll(path, 0700); err != nil {
			return fmt.Errorf("cannot create nginx worker temporary directory")
		}
		if err = os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("cannot assign nginx worker temporary directory")
		}
		if err = os.Chmod(path, 0700); err != nil {
			return fmt.Errorf("cannot secure nginx worker temporary directory")
		}
	}
	return nil
}

// resolved is the validated view of a state, ready to render. Everything that
// could make two services collide is settled here, before a single line of
// configuration exists — a conflict must be an error the operator sees, never a
// server block that silently wins over another.
type resolved struct {
	services []model.Service
	certs    map[string]model.Certificate
}

func resolve(state model.State, publicAddress string) (*resolved, error) {
	r := &resolved{certs: map[string]model.Certificate{}}
	for _, c := range state.Certificates {
		if _, err := CertificateDirectory(".", c); err != nil {
			return nil, err
		}
		r.certs[c.ID] = c
	}
	for _, s := range state.Services {
		if err := ValidateService(s, state.Settings); err != nil {
			return nil, fmt.Errorf("服务 %s：%w", s.ID, err)
		}
		r.services = append(r.services, s)
	}
	sort.Slice(r.services, func(i, j int) bool { return r.services[i].ID < r.services[j].ID })

	// Every name is answered on the same 443, so uniqueness is global rather than
	// per-endpoint: two services claiming one name is a question the gateway
	// cannot answer, not a routing preference.
	names := map[string]string{}
	ports := map[int]string{}
	for _, s := range r.services {
		if !s.Enabled {
			continue
		}
		if s.UsesDomains() {
			cert, ok := r.certs[s.CertificateID]
			if !ok {
				return nil, fmt.Errorf("服务 %s：所选证书不存在", s.Name)
			}
			if !cert.Ready {
				return nil, fmt.Errorf("服务 %s：证书 %s 尚未导入完成", s.Name, cert.Name)
			}
			if err := CertificateCoversAll(cert, s.Domains); err != nil {
				return nil, fmt.Errorf("服务 %s：%w", s.Name, err)
			}
			for _, domain := range s.Domains {
				if owner, dup := names[domain]; dup {
					return nil, fmt.Errorf("域名 %s 同时被两个服务使用（%s 与 %s）", domain, owner, s.Name)
				}
				names[domain] = s.Name
			}
			continue
		}
		port := s.PublishedPort()
		if owner, dup := ports[port]; dup {
			return nil, fmt.Errorf("端口 %d 同时被两个服务占用（%s 与 %s），请为其中一个换端口", port, owner, s.Name)
		}
		ports[port] = s.Name
	}
	if publicAddress != "" {
		if err := validatePublicAddress(publicAddress); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// validatePublicAddress accepts either family: the gateway publishes on whatever
// address the line actually hands it, and refusing an IPv4-only line would leave
// the whole public half of this program unusable.
func validatePublicAddress(address string) error {
	if ValidPublicIPv4(address) {
		return nil
	}
	return ValidatePublicAddress(address)
}

// Render builds a complete independent nginx configuration. It never writes files.
//
// publicAddress is passed in rather than read from state because it is detected,
// not authored: it changes when the line changes, on a schedule nobody chooses.
// Passing it makes the dependency visible at every call site, so a caller cannot
// quietly render for a stale address.
func (e *Engine) Render(state model.State, publicAddress string) (string, []string, error) {
	if err := validateSettings(state.Settings); err != nil {
		return "", nil, err
	}
	_, tempDir, err := e.paths()
	if err != nil {
		return "", nil, err
	}
	r, err := resolve(state, publicAddress)
	if err != nil {
		return "", nil, err
	}
	quote := func(p string) string { q, _ := safePath(p); return q }
	warnings := []string{}
	// SIGQUIT is nginx's graceful shutdown, and a proxied WebSocket never ends
	// on its own: unbounded, the worker waits forever and systemd SIGKILLs the
	// data plane at TimeoutStopSec. This directive is what bounds that wait.
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by Gateway. Do not edit.\nload_module /usr/lib/nginx/modules/ngx_http_js_module.so;\nuser www-data;\nworker_processes 1;\nworker_shutdown_timeout 10s;\npid %s;\nerror_log %s warn;\nevents { worker_connections 1024; }\nhttp {\n", quote(filepath.Join(e.DataDir, "nginx.pid")), quote(filepath.Join(e.DataDir, "error.log")))
	fmt.Fprintf(&b, "    js_import cookie_guard from %s;\n    js_set $gateway_business_cookie cookie_guard.requestCookie;\n", quote(filepath.Join(e.DataDir, "cookie_guard.js")))
	fmt.Fprintf(&b, "    access_log %s combined;\n    client_body_temp_path %s;\n    proxy_temp_path %s;\n", quote(filepath.Join(e.DataDir, "access.log")), quote(filepath.Join(tempDir, "client_temp")), quote(filepath.Join(tempDir, "proxy_temp")))
	b.WriteString("    server_tokens off;\n    default_type application/octet-stream;\n    sendfile on;\n    keepalive_timeout 65s;\n    client_header_timeout 30s;\n    client_body_timeout 300s;\n    client_max_body_size 0;\n    map $http_upgrade $gateway_connection { default upgrade; '' close; }\n")
	for _, service := range r.services {
		if service.Enabled {
			// IDs have already passed ValidateService. Literal IDs remain defined
			// even when a request is rejected before nginx's rewrite phase.
			fmt.Fprintf(&b, "    log_format gateway_traffic_v1_%s escape=json '{\"v\":1,\"service\":\"%s\",\"time\":\"$time_iso8601\",\"status\":$status,\"received\":$request_length,\"sent\":$bytes_sent}';\n", service.ID, service.ID)
		}
	}
	fmt.Fprintf(&b, "    server {\n        listen 127.0.0.1:%d;\n        server_name localhost;\n        access_log off;\n        location = /__gateway_generation {\n            default_type text/plain;\n            return 200 \"%s\\n\";\n        }\n        location / { return 404; }\n    }\n", GenerationPort, generation(state, publicAddress))

	// The public address is the one thing that turns a local relay into something
	// the Internet can reach. Losing it is not a reason to emit broken
	// configuration, but it must never pass without being said out loud.
	if publicAddress == "" {
		warnings = append(warnings, "尚未确认本机公网地址，本次只在内网 EasyTier 地址上提供服务；公网入口需要先完成一次地址探测")
	}

	named := false
	for _, s := range r.services {
		if s.Enabled && s.UsesDomains() {
			named = true
			break
		}
	}
	if named {
		e.renderRejectDefault(&b, state, DomainEntryPort, publicAddress)
		e.renderHTTPRedirect(&b, state, publicAddress)
		warnings = append(warnings, "域名的明文入口在 80 端口上跳转到 HTTPS，需要路由器/防火墙一并放行；未放行时直接用 https:// 访问不受影响")
	}
	for _, s := range r.services {
		if !s.Enabled {
			continue
		}
		if s.UsesDomains() {
			cert := r.certs[s.CertificateID]
			if expiry, parseErr := time.Parse(time.RFC3339, cert.NotAfter); parseErr == nil && time.Until(expiry) < 30*24*time.Hour {
				warnings = append(warnings, "证书 "+cert.Name+" 将在 30 天内到期："+strings.Join(s.Domains, "、"))
			}
			e.renderNamed(&b, state, s, cert, publicAddress)
			warnings = append(warnings, "域名 "+strings.Join(s.Domains, "、")+" 需要路由器/防火墙放行，并从外网独立验证")
			continue
		}
		port := s.PublishedPort()
		e.renderPort(&b, state, s, port, publicAddress)
		warnings = append(warnings, fmt.Sprintf("端口 %d 需要路由器/防火墙放行，并从外网独立验证", port))
	}
	b.WriteString("}\n")
	return b.String(), warnings, nil
}

// listenTargets lists every address one listener binds: the EasyTier address,
// which is how the rest of the private network reaches the gateway, and the
// public address, which is how the Internet does. Both are emitted when both are
// known, because they are two doors into the same room.
func listenTargets(state model.State, publicAddress string, port int) []string {
	targets := []string{net.JoinHostPort(state.Settings.GatewayIP, strconv.Itoa(port))}
	if publicAddress != "" && publicAddress != state.Settings.GatewayIP {
		targets = append(targets, net.JoinHostPort(publicAddress, strconv.Itoa(port)))
	}
	return targets
}

// renderRejectDefault emits one refusing default server for the shared 443 so
// that a request naming a host nobody serves is never forwarded to a backend.
func (e *Engine) renderRejectDefault(b *strings.Builder, state model.State, port int, publicAddress string) {
	for _, target := range listenTargets(state, publicAddress, port) {
		fmt.Fprintf(b, "    server {\n        listen %s ssl default_server;\n        server_name _;\n        ssl_reject_handshake on;\n        return 444;\n    }\n", target)
	}
}

// hstsMaxAge is how long a browser should refuse to reach a named service in the
// clear after it has once succeeded over TLS. It is deliberately not paired with
// includeSubDomains: that would claim authority over names this gateway never
// serves.
const hstsMaxAge = 31536000

// renderHTTPRedirect answers the plain request a browser sends when someone types
// a bare domain. Without it that request lands on port 80, where nothing listens,
// and a refused connection is indistinguishable from a gateway that is down.
// This is not a second entry: it proxies nothing, and every path it accepts ends
// at the one HTTPS name the service actually answers for.
func (e *Engine) renderHTTPRedirect(b *strings.Builder, state model.State, publicAddress string) {
	for _, target := range listenTargets(state, publicAddress, HTTPRedirectPort) {
		fmt.Fprintf(b, "    server {\n        listen %s default_server;\n        server_name _;\n        return 301 https://$host$request_uri;\n    }\n", target)
	}
}

// renderNamed publishes a service that is reached by name. It shares 443 with
// every other named service; TLS SNI picks the certificate and the Host header
// picks the backend, and both are checked because they can disagree.
func (e *Engine) renderNamed(b *strings.Builder, state model.State, s model.Service, cert model.Certificate, publicAddress string) {
	fmt.Fprintf(b, "    server {\n")
	for _, target := range listenTargets(state, publicAddress, DomainEntryPort) {
		fmt.Fprintf(b, "        listen %s ssl;\n", target)
	}
	names := make([]string, 0, len(s.Domains))
	for _, domain := range s.Domains {
		names = append(names, strings.ToLower(domain))
	}
	fmt.Fprintf(b, "        server_name %s;\n", strings.Join(names, " "))
	e.writeTrafficLog(b, s.ID)
	dir, _ := CertificateDirectory(e.DataDir, cert)
	quote := func(p string) string { q, _ := safePath(p); return q }
	fmt.Fprintf(b, "        ssl_certificate %s;\n        ssl_certificate_key %s;\n        ssl_protocols TLSv1.2 TLSv1.3;\n        ssl_session_cache shared:gatewayTLS:1m;\n        ssl_session_timeout 10m;\n        ssl_session_tickets off;\n", quote(filepath.Join(dir, "fullchain.pem")), quote(filepath.Join(dir, "privkey.pem")))
	// A browser that has once reached this name over TLS should never try it in
	// the clear again. The redirect on 80 covers the first visit; this covers
	// every one after it, without a second round trip.
	fmt.Fprintf(b, "        add_header Strict-Transport-Security \"max-age=%d\" always;\n", hstsMaxAge)
	// One certificate can carry several names, and Server Name Indication is
	// chosen before the Host header arrives. Check both against the names this
	// service actually answers for, including requests that arrive with no SNI or
	// with a direct address, before proxying anything.
	pattern := "(?:" + strings.Join(quoteNames(names), "|") + ")"
	fmt.Fprintf(b, "        if ($ssl_server_name !~* \"^%s$\") { return 444; }\n        if ($http_host !~* \"^%s(?::%d)?$\") { return 444; }\n", pattern, pattern, DomainEntryPort)
	e.writeProxy(b, s.Upstream)
	b.WriteString("    }\n")
}

// renderPort publishes a service that has no name, on the number it occupies.
// Nothing else may share that port, so the block needs no host or SNI guard: the
// port itself is the identity.
func (e *Engine) renderPort(b *strings.Builder, state model.State, s model.Service, port int, publicAddress string) {
	fmt.Fprintf(b, "    server {\n")
	for _, target := range listenTargets(state, publicAddress, port) {
		fmt.Fprintf(b, "        listen %s;\n", target)
	}
	b.WriteString("        server_name _;\n")
	e.writeTrafficLog(b, s.ID)
	e.writeProxy(b, s.Upstream)
	b.WriteString("    }\n")
}

// writeTrafficLog records completed HTTP requests, not live or link-layer bytes.
// A local access_log disables parent inheritance, so retain the original log
// explicitly. Only managed business servers call this; redirects and probes do not.
func (e *Engine) writeTrafficLog(b *strings.Builder, serviceID string) {
	accessPath, _ := safePath(filepath.Join(e.DataDir, "access.log"))
	trafficPath, _ := safePath(filepath.Join(e.DataDir, "traffic.log"))
	fmt.Fprintf(b, "        access_log %s combined;\n        access_log %s gateway_traffic_v1_%s;\n", accessPath, trafficPath, serviceID)
}

func quoteNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, regexp.QuoteMeta(name))
	}
	return out
}

func (e *Engine) writeProxy(b *strings.Builder, upstream string) {
	u, _ := parseUpstream(upstream)
	b.WriteString("        location = /__gateway_origin_proof {\n            access_log off;\n            js_content cookie_guard.originProof;\n        }\n")
	fmt.Fprintf(b, "        location / {\n            proxy_pass %s://%s;\n", u.Scheme, u.Host)
	b.WriteString("            proxy_set_header Cookie $gateway_business_cookie;\n            js_header_filter cookie_guard.responseCookies;\n")
	b.WriteString("            proxy_http_version 1.1;\n            proxy_set_header Host $http_host;\n            proxy_set_header X-Real-IP $remote_addr;\n            proxy_set_header X-Forwarded-For $remote_addr;\n            proxy_set_header X-Forwarded-Host $http_host;\n            proxy_set_header X-Forwarded-Proto $scheme;\n            proxy_set_header X-Forwarded-Port $server_port;\n            proxy_set_header Forwarded '';\n            proxy_set_header Upgrade $http_upgrade;\n            proxy_set_header Connection $gateway_connection;\n            proxy_set_header Range $http_range;\n            proxy_set_header If-Range $http_if_range;\n            proxy_cache off;\n            proxy_buffering off;\n            proxy_request_buffering off;\n            proxy_redirect default;\n            proxy_connect_timeout 5s;\n            proxy_read_timeout 3600s;\n            proxy_send_timeout 3600s;\n            send_timeout 300s;\n")
	if u.Scheme == "https" {
		fmt.Fprintf(b, "            proxy_ssl_server_name on;\n            proxy_ssl_name %s;\n            proxy_ssl_verify on;\n            proxy_ssl_verify_depth 5;\n            proxy_ssl_trusted_certificate /etc/ssl/certs/ca-certificates.crt;\n", u.Hostname())
	}
	b.WriteString("        }\n")
}

func (e *Engine) nginx(args ...string) error {
	bin := e.NginxBin
	if bin == "" {
		bin = "nginx"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("nginx 命令超时")
		}
		message := strings.TrimSpace(string(out))
		if len(message) > 2048 {
			message = message[len(message)-2048:]
		}
		if message == "" {
			return fmt.Errorf("nginx 命令执行失败")
		}
		return fmt.Errorf("nginx 命令执行失败：%s", message)
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
	return atomicfile.WriteCompatible(path, data, atomicfile.Options{})
}

// Apply validates a temporary file before replacing the managed configuration.
// On activation failure it restores the previous bytes and attempts to reload them.
// Callers must serialize Apply with state and certificate changes.
func (e *Engine) Apply(state model.State, publicAddress string) (string, error) {
	config, _, err := e.Render(state, publicAddress)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(config))
	hash := hex.EncodeToString(sum[:])
	if e.DryRun {
		return hash, nil
	}
	configPath, _, err := e.paths()
	if err != nil {
		return "", err
	}
	if err = e.Prepare(); err != nil {
		return "", err
	}
	previous, readErr := os.ReadFile(configPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", fmt.Errorf("无法读取上一份受管配置")
	}
	check, err := os.CreateTemp(filepath.Dir(configPath), "nginx-check-*.conf")
	if err != nil {
		return "", fmt.Errorf("无法暂存 nginx 配置")
	}
	checkPath := check.Name()
	defer os.Remove(checkPath)
	_, writeErr := check.WriteString(config)
	closeErr := check.Close()
	if writeErr != nil || closeErr != nil {
		return "", fmt.Errorf("无法写入暂存的 nginx 配置")
	}
	prefix := filepath.Clean(e.DataDir) + string(filepath.Separator)
	if err = e.nginx("-p", prefix, "-c", checkPath, "-t"); err != nil {
		return "", err
	}
	if err = atomicWrite(configPath, []byte(config)); err != nil {
		return "", fmt.Errorf("无法启用受管配置")
	}
	args := []string{"-p", prefix, "-c", configPath, "-s", "reload"}
	if err = e.nginx(args...); err == nil {
		err = e.waitGeneration(generation(state, publicAddress))
	}
	if err == nil {
		return hash, nil
	}
	activationErr := err
	if readErr == nil {
		if restoreErr := atomicWrite(configPath, previous); restoreErr != nil {
			return "", fmt.Errorf("启用失败且上一份配置无法恢复，需要人工处理")
		}
		recovery := []string{"-p", prefix, "-c", configPath, "-s", "reload"}
		if err = e.nginx(recovery...); err != nil {
			return "", fmt.Errorf("启用失败；上一份配置已恢复但 reload 失败，请检查受管 error.log")
		}
		if match := renderedGeneration.FindSubmatch(previous); len(match) == 2 {
			if err = e.waitGeneration(string(match[1])); err != nil {
				return "", fmt.Errorf("启用失败；上一份配置已恢复，但无法确认回滚后的生效代际，请检查受管 error.log")
			}
		}
		return "", fmt.Errorf("启用失败；上一份配置已恢复：%w", activationErr)
	}
	if removeErr := os.Remove(configPath); removeErr != nil {
		return "", fmt.Errorf("首次启用失败，且暂存的配置无法删除")
	}
	return "", fmt.Errorf("首次启用失败：%w", activationErr)
}

// VerifyTLS locally verifies every named service by name, validity and
// fingerprint. Services that share 443 are each checked through their own SNI
// name, which also proves that name-based selection is in effect.
// Success does not prove public Internet reachability.
func (e *Engine) VerifyTLS(state model.State, publicAddress string) error {
	if e.DryRun {
		return nil
	}
	r, err := resolve(state, publicAddress)
	if err != nil {
		return err
	}
	for _, s := range r.services {
		if !s.Enabled || !s.UsesDomains() {
			continue
		}
		cert := r.certs[s.CertificateID]
		_, leaf, err := e.loadCertificateLeaf(cert)
		if err != nil {
			return err
		}
		address := net.JoinHostPort(state.Settings.GatewayIP, strconv.Itoa(DomainEntryPort))
		for _, domain := range s.Domains {
			if err := verifyTLSListener(domain, address, cert.Fingerprint, leaf); err != nil {
				return fmt.Errorf("本机 HTTPS 校验失败（%s）：%w", domain, err)
			}
		}
	}
	return nil
}

func verifyTLSListener(domain, address, fingerprint string, expectedLeaf *x509.Certificate) error {
	if expectedLeaf == nil {
		return fmt.Errorf("无法读取证书材料")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("监听端口无法连接")
	}
	roots := x509.NewCertPool()
	roots.AddCert(expectedLeaf)
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: domain, MinVersion: tls.VersionTLS12,
		// Trust only the provisioned leaf while retaining normal hostname,
		// lifetime and server-auth checks, including for private CAs.
		RootCAs: roots,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("没有对端证书")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != fingerprint {
				return fmt.Errorf("生效证书指纹不一致")
			}
			return nil
		},
	})
	defer tlsConn.Close()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("生效证书校验失败")
	}
	return nil
}

package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"w103d/gateway/internal/engine"
)

// checkCFCompatibility authenticates the intended target, never an IP scan. Host/SNI
// remain the business name while the actual connection is pinned to a checked
// public IPv4 address. A changing nonce prevents a cached origin response from
// authorizing traffic. Success is preflight evidence, not post-switch acceptance.
func (a *App) checkCFCompatibility(ctx context.Context, host, standard, candidate string) error {
	if a.cloudflareProbe != nil {
		return a.cloudflareProbe(ctx, host, standard, candidate)
	}
	return a.checkCFOriginTargets(ctx, host, cfCompatibilityTargets(standard, candidate))
}

func cfCompatibilityTargets(standard, candidate string) []string {
	// The fresh nonce is checked against this device's secret, so the new
	// target proves the intended origin directly. A broken route being replaced
	// is not a prerequisite for moving to a healthy authenticated alternative.
	return []string{candidate}
}

func (a *App) checkCFOriginTargets(ctx context.Context, host string, targets []string) error {
	key, err := engine.OriginProofKey(a.opts.DataDir)
	if err != nil {
		return errors.New("源站核验凭据不可用；请先应用本机配置，再切换入口")
	}
	return checkCFOriginTargetsWith(ctx, host, targets, key, net.DefaultResolver.LookupIP,
		func(ctx context.Context, host, address, nonce, expected string) error {
			return probeCFOrigin(ctx, host, address, nonce, expected, nil)
		})
}

type cfOriginLookup func(context.Context, string, string) ([]net.IP, error)
type cfOriginProbe func(context.Context, string, string, string, string) error

func checkCFOriginTargetsWith(ctx context.Context, host string, targets []string, key string, lookup cfOriginLookup, probe cfOriginProbe) error {
	if len(targets) == 0 || len(targets) > 2 {
		return errors.New("invalid origin compatibility target count")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	for _, target := range targets {
		ips, err := lookup(ctx, "ip4", target)
		if err != nil || len(ips) == 0 {
			return fmt.Errorf("入口 %s 的 IPv4 解析未查到或查询失败；本次核验未改写 DNS", target)
		}
		if len(ips) > 32 {
			return errors.New("too many preferred IPv4 candidates; compatibility remains unverified")
		}
		addresses := []string{}
		seen := map[string]bool{}
		for _, ip := range ips {
			if !cfPublicIPv4(ip.String()) {
				return errors.New("preferred compatibility destination is not public IPv4")
			}
			address := net.JoinHostPort(ip.String(), "443")
			if !seen[address] {
				seen[address] = true
				addresses = append(addresses, address)
			}
		}
		// Validate the entire DNS answer before probing, including addresses
		// beyond the two-attempt limit. A private answer never gets ignored.
		if len(addresses) > 2 {
			addresses = addresses[:2]
		}
		nonceBytes := make([]byte, 32)
		if _, err = rand.Read(nonceBytes); err != nil {
			return err
		}
		nonce := hex.EncodeToString(nonceBytes)
		if err = probeCFOriginAddresses(ctx, host, addresses, nonce, engine.OriginProof(key, host, nonce), probe); err != nil {
			return fmt.Errorf("入口 %s：%w", target, err)
		}
	}
	return nil
}

type cfOriginProbeStage int32

const (
	cfOriginConnect cfOriginProbeStage = iota
	cfOriginTLS
	cfOriginHeaders
	cfOriginBody
)

// Do not return transport errors verbatim: they can include the nonce URL or
// arbitrary peer data. The stage distinguishes an unreachable edge from an
// authenticated TLS connection which never returned an HTTP response.
func cfOriginTransportError(err error, stage cfOriginProbeStage) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("入口核验已取消，尚未确认可用；本次核验未改写 DNS")
	}
	var certificateErr *tls.CertificateVerificationError
	if errors.As(err, &certificateErr) {
		return errors.New("入口的 HTTPS 证书校验失败（域名、有效期或信任链不符合要求）；本次核验未改写 DNS")
	}
	var netErr net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout()
	message := "连接入口的 TCP 443 失败"
	switch stage {
	case cfOriginTLS:
		message = "已连接入口，但 TLS 握手失败"
	case cfOriginHeaders:
		message = "TLS 证书校验已通过，但读取 HTTP 响应失败，尚未确认回源可用"
	case cfOriginBody:
		message = "已收到 HTTP 响应，但读取源站核验结果失败"
	}
	if timedOut {
		switch stage {
		case cfOriginConnect:
			message = "连接入口的 TCP 443 超时"
		case cfOriginTLS:
			message = "已连接入口，但 TLS 握手超时"
		case cfOriginHeaders:
			message = "TLS 证书校验已通过，但等待 HTTP 响应超时，尚未确认回源可用"
		case cfOriginBody:
			message = "已收到 HTTP 响应，但读取源站核验结果超时"
		}
	}
	return errors.New(message + "；本次核验未改写 DNS")
}

// A single unreachable anycast address must not hide a working DNS alternative.
// Each attempt still requires valid TLS and the current uncached origin proof;
// success establishes one working path per target, not universal reachability.
func probeCFOriginAddresses(ctx context.Context, host string, addresses []string, nonce, expected string, probe cfOriginProbe) error {
	if len(addresses) == 0 || len(addresses) > 2 {
		return errors.New("invalid origin compatibility address count")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(addresses))
	for _, address := range addresses {
		go func(address string) { results <- probe(ctx, host, address, nonce, expected) }(address)
	}
	var last error
	for range addresses {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-results:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				return nil
			}
			last = err
		}
	}
	return last
}

func probeCFOrigin(ctx context.Context, host, address, nonce, expected string, roots *tls.Config) error {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if roots != nil {
		tlsConfig = roots.Clone()
		tlsConfig.ServerName = host
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 3 * time.Second, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, ignored string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp4", address)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/__gateway_origin_proof?nonce="+nonce, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cache-Control", "no-cache, no-store")
	var stage atomic.Int32
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		TLSHandshakeStart: func() { stage.Store(int32(cfOriginTLS)) },
		GotConn:           func(httptrace.GotConnInfo) { stage.Store(int32(cfOriginHeaders)) },
	}))
	response, err := client.Do(req)
	if err != nil {
		return cfOriginTransportError(err, cfOriginProbeStage(stage.Load()))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == 525 {
			return errors.New("入口返回 HTTP 525（Cloudflare 到源站的 TLS 握手失败）；本次核验未改写 DNS")
		}
		if response.StatusCode == 526 {
			return errors.New("入口返回 HTTP 526（Cloudflare 未能通过源站证书校验）；本次核验未改写 DNS")
		}
		return fmt.Errorf("入口返回 HTTP %d，未通过源站核验；本次核验未改写 DNS", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 129))
	if err != nil {
		return cfOriginTransportError(err, cfOriginBody)
	}
	if !hmac.Equal([]byte(strings.TrimSpace(string(body))), []byte(expected)) {
		return errors.New("入口虽返回 HTTP 200，但源站校验值不匹配，尚未确认到达本机源站；本次核验未改写 DNS")
	}
	return nil
}

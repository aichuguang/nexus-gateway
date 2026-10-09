package app

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/globalping"
	"w103d/gateway/internal/model"
)

type monitorEndpointCheck struct {
	Classification string            `json:"classification,omitempty"`
	URL            string            `json:"url"`
	IPVersion      int               `json:"ipVersion,omitempty"`
	Status         string            `json:"status"`
	StatusCode     int               `json:"statusCode,omitempty"`
	Message        string            `json:"message,omitempty"`
	Nodes          []globalping.Node `json:"nodes,omitempty"`
}

func monitorServiceGeneration(state model.State, service model.Service) string {
	policy, _ := json.Marshal(service.HealthCheck)
	sum := sha256.Sum256([]byte(engine.ServiceGeneration(state, service) + "\n" + string(policy)))
	return hex.EncodeToString(sum[:])
}

func monitorOverall(checks monitorChecks, settings monitorSettings) string {
	upstream := checks.TCP
	if settings.CheckHTTP {
		upstream = checks.HTTP
	}
	statuses := []string{upstream.Status, checks.Local.Status}
	if settings.CheckExternal {
		statuses = append(statuses, checks.External.Status)
	}
	// Reaching an authentication wall is not business failure, but also cannot
	// advance recovery counters for an earlier confirmed business failure.
	unknown := upstream.Classification == "auth-required" || checks.Local.Classification == "auth-required"
	for _, status := range statuses {
		if status == "error" {
			return "error"
		}
		if status != "ok" && status != "not-applicable" {
			unknown = true
		}
	}
	if unknown {
		return "unknown"
	}
	return "ok"
}

// A local check exercises HTTP forwarding, not just an open listener. For HTTPS
// the actual gateway address is dialed while Host/SNI still name the service.
func (a *App) monitorLocalEntry(ctx context.Context, applied model.State, service model.Service) monitorCheck {
	result := monitorCheck{Status: "ok"}
	if !service.UsesDomains() {
		u := "http://" + net.JoinHostPort(applied.Settings.GatewayIP, strconv.Itoa(service.PublishedPort())) + "/"
		p := engine.ProbeHealth(ctx, u, service.HealthCheck)
		result.Classification = p.Classification
		result.LatencyMS, result.StatusCode = float64(p.LatencyMS), p.Status
		if !p.OK {
			result.Status, result.Message = "error", "本机 HTTP 代理链路未通过"
		}
		if ctx.Err() != nil {
			result.Status, result.Message = "unknown", "检查被取消或超时，结果未确认"
		}
		return result
	}
	leaf, fingerprint, err := a.publicCertificate(applied, service, time.Now())
	if err != nil {
		return monitorCheck{Status: "error", Message: "本机入口证书不可用"}
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	for _, host := range service.Domains {
		endpoint := monitorEndpointCheck{URL: "https://" + host + "/", Status: "ok"}
		if service.HealthCheck != nil && service.HealthCheck.Path != "" {
			endpoint.URL = "https://" + host + service.HealthCheck.Path
		}
		child, cancel := context.WithTimeout(ctx, 7*time.Second)
		transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 4 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
			DialContext: func(c context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 4 * time.Second}).DialContext(c, "tcp", net.JoinHostPort(applied.Settings.GatewayIP, strconv.Itoa(engine.DomainEntryPort)))
			},
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, RootCAs: roots, VerifyConnection: func(s tls.ConnectionState) error {
				if len(s.PeerCertificates) == 0 {
					return errors.New("missing certificate")
				}
				sum := sha256.Sum256(s.PeerCertificates[0].Raw)
				if hex.EncodeToString(sum[:]) != fingerprint {
					return errors.New("certificate changed")
				}
				return nil
			}},
		}
		client := &http.Client{Transport: transport, Timeout: 7 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		r, _ := http.NewRequestWithContext(child, http.MethodGet, endpoint.URL, nil)
		started := time.Now()
		response, err := client.Do(r)
		result.LatencyMS += float64(time.Since(started).Milliseconds())
		if err != nil {
			endpoint.Status, endpoint.Message = "error", "本机入口 HTTP/TLS 连接失败"
		} else {
			endpoint.StatusCode = response.StatusCode
			response.Body.Close()
			ok, classification := engine.HTTPHealthStatus(response.StatusCode, service.HealthCheck)
			endpoint.Classification = classification
			if classification == "auth-required" {
				result.Classification = classification
			}
			if !ok {
				endpoint.Status, endpoint.Message = "error", "本机入口返回非成功 HTTP 状态"
			}
		}
		transport.CloseIdleConnections()
		cancel()
		if ctx.Err() != nil {
			endpoint.Status, endpoint.Message = "unknown", "检查取消，结果未确认"
		}
		result.Details = append(result.Details, endpoint)
		if endpoint.Status == "error" {
			result.Status = "error"
		} else if endpoint.Status != "ok" && result.Status != "error" {
			result.Status = "unknown"
		}
	}
	if result.Status != "ok" {
		result.Message = "部分本机入口未通过，展开查看详情"
	}
	return result
}

// Integrated monitoring is serialized by monitor.running. It shares the external
// job ledger with explicit API checks but does not impose a 5s gap per service.
func (a *App) integratedExternalJob(ctx context.Context, target externalTarget, ipVersion int) (externalJob, error) {
	a.external.mu.Lock()
	a.loadExternalLocked()
	if a.external.err != "" {
		message := a.external.err
		a.external.mu.Unlock()
		return externalJob{}, errors.New(message)
	}
	for _, job := range a.external.disk.Jobs {
		if job.Status == "running" {
			a.external.mu.Unlock()
			return externalJob{}, errors.New("另一项外部探测正在进行，本轮暂未确认")
		}
	}
	job := externalJob{ID: NewID(), Target: target, IPVersion: ipVersion, RequestedNodes: 1, Status: "running", Message: "整体连通检测：检查大陆访问", StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Nodes: []globalping.Node{}}
	previous := a.external.disk.Jobs
	jobs := previous
	if len(jobs) >= 32 {
		jobs = jobs[len(jobs)-31:]
	}
	a.external.disk.Jobs = append(jobs, job)
	if !a.saveExternalLocked() {
		a.external.disk.Jobs = previous
		a.external.mu.Unlock()
		return externalJob{}, errors.New("无法保存外部探测记录，未提交")
	}
	a.external.mu.Unlock()
	a.runExternalProbe(ctx, job)
	a.external.mu.Lock()
	defer a.external.mu.Unlock()
	for _, saved := range a.external.disk.Jobs {
		if saved.ID == job.ID {
			return saved, nil
		}
	}
	return externalJob{}, errors.New("外部探测记录不可用")
}

func (a *App) monitorExternalEntry(ctx context.Context, applied model.State, service model.Service) monitorCheck {
	result := monitorCheck{Status: "not-applicable", Message: "没有可供公共节点检查的已发布公网入口"}
	for _, target := range a.externalTargets() {
		if target.ServiceID != service.ID {
			continue
		}
		if target.DisabledReason != "" {
			result.Message = target.DisabledReason
			continue
		}
		expected := sha256.Sum256([]byte(service.ID + "\n" + engine.ServiceGeneration(applied, service) + "\n" + target.URL))
		if target.Key != hex.EncodeToString(expected[:]) {
			return monitorCheck{Status: "unknown", Message: "已发布入口在检测期间变化"}
		}
		ipVersion := target.IPVersions[0]
		if len(target.IPVersions) > 1 {
			lookup := a.external.lookup
			if lookup == nil {
				lookup = net.DefaultResolver.LookupIPAddr
			}
			child, cancel := context.WithTimeout(ctx, 4*time.Second)
			addresses, err := lookup(child, target.Host)
			cancel()
			if err != nil || len(addresses) == 0 {
				if result.Status != "error" {
					result.Status = "unknown"
				}
				result.Message = "入口 DNS 未解析到公开地址，未向第三方提交"
				continue
			}
			ipVersion = 6
			safe := true
			for _, address := range addresses {
				if !publicProbeIP(address.IP.String()) {
					safe = false
				}
				if address.IP.To4() != nil {
					ipVersion = 4
				}
			}
			if !safe {
				if result.Status != "error" {
					result.Status = "unknown"
				}
				result.Message = "入口解析包含私网或保留地址，未向第三方提交"
				continue
			}
		}
		job, err := a.integratedExternalJob(ctx, target, ipVersion)
		detail := monitorEndpointCheck{URL: target.URL, IPVersion: ipVersion, Status: "unknown"}
		if err != nil {
			detail.Message = err.Error()
		} else {
			detail.Nodes, detail.Message, detail.StatusCode = job.Nodes, job.Message, job.HTTPStatus
			result.LatencyMS = math.Max(result.LatencyMS, monitorExternalLatency(job))
			if job.HTTPStatus == 429 {
				result.StatusCode = 429
				result.Message = job.Message
			}
			switch job.Status {
			case "ok":
				detail.Status = "ok"
			case "failed":
				detail.Status = "error"
			case "partial", "unknown":
				// A redirect proves an HTTP entry responded. Authentication, WAF
				// rejection and unsupported HEAD remain unknown without assertions.
				if len(job.Nodes) == 1 && (job.Nodes[0].Status == "ok" || (job.Nodes[0].Status == "reachable" && job.Nodes[0].HTTPStatus >= 300 && job.Nodes[0].HTTPStatus < 400)) {
					detail.Status = "ok"
				}
			}
		}
		result.Details = append(result.Details, detail)
		if detail.Status == "error" {
			result.Status = "error"
		} else if detail.Status == "unknown" && result.Status != "error" {
			result.Status = "unknown"
		} else if result.Status == "not-applicable" {
			result.Status = "ok"
		}
		if ctx.Err() != nil {
			result.Status = "unknown"
			result.Message = "外部检查取消或超时，不计为服务故障"
			break
		}
	}
	if result.Status == "ok" {
		result.Message = "所选中国大陆节点可收到入口响应；不代表完整业务、双栈或所有运营商均可用"
	}
	return result
}

// Report the slowest valid completed node measurement across all endpoints,
// not provider polling/queue latency or the sum of sequential checks. A missing
// timing, canceled/provider-failed job, or node without a verified HTTP response
// contributes nothing; zero is omitted from the public monitorCheck JSON.
func monitorExternalLatency(job externalJob) float64 {
	if job.HTTPStatus == http.StatusTooManyRequests {
		return 0
	}
	switch job.Status {
	case "ok", "failed", "partial", "unknown":
	default:
		return 0
	}
	latency := 0.0
	for _, node := range job.Nodes {
		if node.Country != "CN" || node.HTTPStatus < 100 || node.HTTPStatus > 599 || node.LatencyMS <= 0 || math.IsNaN(node.LatencyMS) || math.IsInf(node.LatencyMS, 0) {
			continue
		}
		switch node.Status {
		case "ok", "reachable", "failed":
			latency = math.Max(latency, node.LatencyMS)
		}
	}
	return latency
}

package engine

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"
	"w103d/gateway/internal/model"
)

type ProbeResult struct {
	OK             bool   `json:"ok"`
	Status         int    `json:"status"`
	LatencyMS      int64  `json:"latencyMs"`
	Error          string `json:"error,omitempty"`
	Reachable      bool   `json:"reachable"`
	Classification string `json:"classification,omitempty"`
}

// Probe performs one GET without following redirects or consuming its response body.
// Its caller must validate and authorize the upstream (no arbitrary URL API).
func Probe(ctx context.Context, upstream string) ProbeResult {
	return ProbeHealth(ctx, upstream, nil)
}

// HTTPHealthStatus classifies an actual HTTP response, separately from transport reachability.
func HTTPHealthStatus(code int, health *model.HealthCheck) (bool, string) {
	if health != nil && len(health.ExpectedStatusCodes) > 0 {
		for _, expected := range health.ExpectedStatusCodes {
			if code == expected {
				return true, "healthy"
			}
		}
		return false, "unexpected-status"
	}
	if code == 401 || code == 403 {
		return true, "auth-required"
	}
	if code >= 200 && code <= 399 {
		return true, "healthy"
	}
	return false, "unexpected-status"
}

func ProbeHealth(ctx context.Context, upstream string, health *model.HealthCheck) ProbeResult {
	start := time.Now()
	result := ProbeResult{}
	if health != nil && health.Path != "" {
		base, err := url.Parse(upstream)
		path, pathErr := url.Parse(health.Path)
		if err != nil || pathErr != nil || path.IsAbs() || path.Host != "" || len(health.Path) == 0 || health.Path[0] != '/' {
			result.Error, result.Classification = "invalid health path", "invalid"
			return result
		}
		base.Path, base.RawPath, base.RawQuery, base.Fragment = path.Path, path.RawPath, path.RawQuery, ""
		upstream = base.String()
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 4 * time.Second}).DialContext,
		TLSHandshakeTimeout:   4 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		DisableKeepAlives:     true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport, Timeout: 6 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		result.Error = "invalid upstream URL"
		return result
	}
	req.Header.Set("User-Agent", "Gateway-Probe/1")
	resp, err := client.Do(req)
	result.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		result.Error = "upstream connection failed or timed out"
		result.Classification = "connection-error"
		var certificateError *tls.CertificateVerificationError
		var networkError net.Error
		if errors.As(err, &certificateError) {
			result.Classification = "tls-error"
		} else if errors.As(err, &networkError) && networkError.Timeout() {
			result.Classification = "timeout"
		}
		return result
	}
	resp.Body.Close()
	result.Status = resp.StatusCode
	result.Reachable = true
	result.OK, result.Classification = HTTPHealthStatus(result.Status, health)
	if !result.OK {
		result.Error = "upstream returned a non-success HTTP status"
		result.Classification = "unexpected-status"
	}
	return result
}

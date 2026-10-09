// Package globalping implements bounded, anonymous HTTP measurements. No account
// token is accepted: this mode cannot spend a user's purchased credits.
package globalping

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

const BaseURL = "https://api.globalping.io"

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{6,64}$`)

type Client struct {
	HTTP *http.Client
	URL  string // Only overridden by local test fixtures, never by API input.
}

func New() *Client {
	return &Client{URL: BaseURL, HTTP: &http.Client{
		Timeout: 12 * time.Second,
		Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type Quota struct {
	Type      string    `json:"type"`
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     int       `json:"reset"`
	CheckedAt time.Time `json:"checkedAt"`
}

// Error deliberately excludes provider bodies, headers and request URLs.
type Error struct {
	Code       string
	Limit      int
	RetryAfter int
	Scope      string
}

func (e *Error) Error() string { return e.Code }

func (c *Client) request(ctx context.Context, method, path string, body any, etag string, out any) (string, bool, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return "", false, err
		}
		reader = bytes.NewReader(data)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.URL+path, reader)
	if err != nil {
		return "", false, err
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "Gateway/Globalping-External-Check")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		r.Header.Set("If-None-Match", etag)
	}
	response, err := c.HTTP.Do(r)
	if err != nil {
		return "", false, &Error{Code: "connection"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified && method == http.MethodGet && etag != "" {
		return etag, true, nil
	}
	if response.StatusCode == http.StatusTooManyRequests {
		limit, _ := strconv.Atoi(response.Header.Get("X-RateLimit-Limit"))
		retry, _ := strconv.Atoi(response.Header.Get("Retry-After"))
		if retry <= 0 {
			retry, _ = strconv.Atoi(response.Header.Get("X-RateLimit-Reset"))
		}
		scope := "read"
		if method == http.MethodPost {
			scope = "create"
		}
		return "", false, &Error{Code: "rate_limited", Limit: limit, RetryAfter: retry, Scope: scope}
	}
	if response.StatusCode == http.StatusUnprocessableEntity {
		return "", false, &Error{Code: "no_nodes"}
	}
	expected := http.StatusOK
	if method == http.MethodPost {
		expected = http.StatusAccepted
	}
	if response.StatusCode != expected {
		return "", false, &Error{Code: "provider_error"}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, out) != nil {
		return "", false, &Error{Code: "invalid_response"}
	}
	return response.Header.Get("ETag"), false, nil
}

func (c *Client) Limits(ctx context.Context) (Quota, error) {
	var result struct {
		RateLimit struct {
			Measurements struct {
				Create Quota `json:"create"`
			} `json:"measurements"`
		} `json:"rateLimit"`
	}
	_, _, err := c.request(ctx, http.MethodGet, "/v1/limits", nil, "", &result)
	q := result.RateLimit.Measurements.Create
	if err == nil && (q.Type != "ip" || q.Limit <= 0 || q.Remaining < 0 || q.Remaining > q.Limit || q.Reset < 0) {
		err = &Error{Code: "invalid_response"}
	}
	q.CheckedAt = time.Now().UTC()
	return q, err
}

type Target struct {
	Host      string
	Port      int
	Protocol  string
	IPVersion int
	Nodes     int
}

type Created struct {
	ID          string `json:"id"`
	ProbesCount int    `json:"probesCount"`
}

func (c *Client) Create(ctx context.Context, target Target) (Created, error) {
	if target.Nodes < 1 || target.Nodes > 2 || (target.IPVersion != 4 && target.IPVersion != 6) ||
		target.Port < 1 || target.Port > 65535 || (target.Protocol != "HTTP" && target.Protocol != "HTTPS") || target.Host == "" {
		return Created{}, errors.New("invalid measurement options")
	}
	// Country is a strict ISO filter, not fuzzy "Asia" or "China" matching.
	body := map[string]any{"type": "http", "target": target.Host, "limit": target.Nodes,
		"locations": []map[string]string{{"country": "CN"}}, "timeout": 10, "inProgressUpdates": true,
		"measurementOptions": map[string]any{"protocol": target.Protocol, "port": target.Port, "ipVersion": target.IPVersion,
			"request": map[string]string{"method": "HEAD", "path": "/"}}}
	var result Created
	_, _, err := c.request(ctx, http.MethodPost, "/v1/measurements", body, "", &result)
	if err == nil && (!validID.MatchString(result.ID) || result.ProbesCount < 1 || result.ProbesCount > target.Nodes) {
		err = &Error{Code: "invalid_response"}
	}
	return result, err
}

type Result struct {
	ID                 string `json:"id"`
	Type               string `json:"type"`
	Target             string `json:"target"`
	Status             string `json:"status"`
	ProbesCount        int    `json:"probesCount"`
	MeasurementOptions struct {
		Protocol  string `json:"protocol"`
		Port      int    `json:"port"`
		IPVersion int    `json:"ipVersion"`
	} `json:"measurementOptions"`
	Results []struct {
		Probe struct {
			Country string `json:"country"`
			City    string `json:"city"`
			Network string `json:"network"`
			ASN     int    `json:"asn"`
		} `json:"probe"`
		Result struct {
			Status          string `json:"status"`
			FailureSource   string `json:"failureSource"`
			StatusCode      int    `json:"statusCode"`
			ResolvedAddress string `json:"resolvedAddress"`
			Timings         struct {
				Total float64 `json:"total"`
			} `json:"timings"`
			TLS *struct {
				Authorized *bool `json:"authorized"`
			} `json:"tls"`
		} `json:"result"`
	} `json:"results"`
}

func (c *Client) Read(ctx context.Context, id, etag string) (Result, string, bool, error) {
	if !validID.MatchString(id) {
		return Result{}, "", false, errors.New("invalid measurement id")
	}
	var result Result
	next, unchanged, err := c.request(ctx, http.MethodGet, "/v1/measurements/"+id, nil, etag, &result)
	if err == nil && !unchanged && (result.ID != id || result.Type != "http" || result.ProbesCount < 1 || result.ProbesCount > 2 || len(result.Results) > 2 || (result.Status != "finished" && result.Status != "in-progress")) {
		err = &Error{Code: "invalid_response"}
	}
	return result, next, unchanged, err
}

type Node struct {
	Country    string  `json:"country"`
	City       string  `json:"city"`
	Network    string  `json:"network"`
	ASN        int     `json:"asn"`
	Status     string  `json:"status"`
	HTTPStatus int     `json:"httpStatus"`
	LatencyMS  float64 `json:"latencyMs"`
	Message    string  `json:"message"`
}

// Summarize never promotes an overseas node, unknown outcome or missing TLS
// validation into success. Response headers/body and raw output are not retained.
func Summarize(result Result, target Target) (string, []Node, error) {
	// Globalping omits default options in its response (e.g. HTTPS and IPv4).
	// Check echoed overrides, then verify actual IP family and TLS per node.
	opts := result.MeasurementOptions
	if result.Target != target.Host || (opts.Protocol != "" && opts.Protocol != target.Protocol) || (opts.Port != 0 && opts.Port != target.Port) || (opts.IPVersion != 0 && opts.IPVersion != target.IPVersion) || result.ProbesCount > target.Nodes {
		return "unavailable", nil, &Error{Code: "invalid_response"}
	}
	nodes := make([]Node, 0, len(result.Results))
	ok, failed := 0, 0
	for _, item := range result.Results {
		p, r := item.Probe, item.Result
		if p.Country != "CN" {
			return "unavailable", nil, &Error{Code: "unexpected_country"}
		}
		n := Node{Country: p.Country, City: p.City, Network: p.Network, ASN: p.ASN, Status: "unknown", HTTPStatus: r.StatusCode, LatencyMS: r.Timings.Total, Message: "节点尚未提供可判定结果"}
		if len(n.City) > 128 || len(n.Network) > 256 || n.LatencyMS < 0 {
			return "unavailable", nil, &Error{Code: "invalid_response"}
		}
		switch {
		case r.Status == "failed" && r.FailureSource == "target":
			n.Status, n.Message = "failed", "该节点访问失败，可能涉及目标、DNS 或网络路径；不代表所有用户均不可访问"
		case r.Status == "failed" || r.Status == "offline":
			n.Message = "探针或解析器未完成检查，不能判定服务故障"
		case r.Status == "finished" && r.StatusCode >= 100 && r.StatusCode <= 599:
			address := net.ParseIP(r.ResolvedAddress)
			if address == nil {
				n.Message = "没有可验证的目标 IP 结果"
				nodes = append(nodes, n)
				continue
			}
			if (address.To4() != nil) != (target.IPVersion == 4) {
				return "unavailable", nil, &Error{Code: "invalid_response"}
			}
			switch {
			case target.Protocol == "HTTPS" && (r.TLS == nil || r.TLS.Authorized == nil):
				n.Message = "没有可验证的 TLS 结果"
			case target.Protocol == "HTTPS" && !*r.TLS.Authorized:
				n.Status, n.Message = "failed", "TLS 证书验证失败"
			case r.StatusCode >= 200 && r.StatusCode < 300:
				n.Status, n.Message = "ok", "入口响应成功；未验证业务内容或实时回源"
			case r.StatusCode >= 300 && r.StatusCode < 400:
				n.Status, n.Message = "reachable", "收到重定向，最终页面未验证"
			case r.StatusCode == 401 || r.StatusCode == 403 || r.StatusCode == 405:
				n.Status, n.Message = "reachable", "入口已响应，但认证、访问策略或 HEAD 方法限制需要核对"
			default:
				n.Status, n.Message = "failed", "入口返回非成功 HTTP 状态"
			}
		}
		if n.Status == "ok" {
			ok++
		}
		if n.Status == "failed" {
			failed++
		}
		nodes = append(nodes, n)
	}
	if result.Status == "in-progress" {
		return "running", nodes, nil
	}
	if len(nodes) == target.Nodes && ok == target.Nodes {
		return "ok", nodes, nil
	}
	if len(nodes) == target.Nodes && failed == target.Nodes {
		return "failed", nodes, nil
	}
	if ok > 0 || failed > 0 {
		return "partial", nodes, nil
	}
	return "unknown", nodes, nil
}

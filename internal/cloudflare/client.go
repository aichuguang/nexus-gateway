// Package cloudflare contains the deliberately small Cloudflare for SaaS API
// surface used by the gateway. Tokens and untrusted API error bodies never
// become diagnostic strings.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Credentials struct {
	APIToken string `json:"apiToken"`
}

func (c Credentials) Valid() bool {
	return len(strings.TrimSpace(c.APIToken)) >= 16 && !strings.ContainsAny(c.APIToken, "\r\n\t ")
}

type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Type   string `json:"type"`
}
type Record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
	Comment string `json:"comment,omitempty"`
}
type Fallback struct {
	Origin string `json:"origin"`
	Status string `json:"status"`
}
type ValidationRecord struct {
	TXTName  string `json:"txt_name"`
	TXTValue string `json:"txt_value"`
	HTTPURL  string `json:"http_url,omitempty"`
	HTTPBody string `json:"http_body,omitempty"`
}
type DCVDelegationRecord struct {
	CNAME       string `json:"cname"`
	CNAMETarget string `json:"cname_target"`
}
type ValidationError struct {
	Message string `json:"message"`
}
type SSL struct {
	ID                   string                `json:"id,omitempty"`
	Method               string                `json:"method,omitempty"`
	Type                 string                `json:"type,omitempty"`
	BundleMethod         string                `json:"bundle_method,omitempty"`
	CertificateAuthority string                `json:"certificate_authority,omitempty"`
	Settings             json.RawMessage       `json:"settings,omitempty"`
	Wildcard             *bool                 `json:"wildcard,omitempty"`
	Status               string                `json:"status,omitempty"`
	ValidationRecords    []ValidationRecord    `json:"validation_records,omitempty"`
	ValidationErrors     []ValidationError     `json:"validation_errors,omitempty"`
	DCVDelegationRecords []DCVDelegationRecord `json:"dcv_delegation_records,omitempty"`
}
type Ownership struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}
type Hostname struct {
	ID                 string            `json:"id,omitempty"`
	Hostname           string            `json:"hostname"`
	Status             string            `json:"status,omitempty"`
	SSL                SSL               `json:"ssl"`
	Ownership          Ownership         `json:"ownership_verification,omitempty"`
	VerificationErrors []string          `json:"verification_errors,omitempty"`
	CustomOriginServer string            `json:"custom_origin_server,omitempty"`
	CustomOriginSNI    string            `json:"custom_origin_sni,omitempty"`
	CustomMetadata     map[string]string `json:"custom_metadata,omitempty"`
}

type APIError struct {
	Status   int
	Codes    []int
	Definite bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Cloudflare API request failed (HTTP %d, codes %v)", e.Status, e.Codes)
}

// Rejected is true only when Cloudflare explicitly reports that no write was
// accepted. Transport errors, truncated responses and 5xx are ambiguous.
func Rejected(err error) bool { var e *APIError; return errors.As(err, &e) && e.Definite }

type Client struct {
	credentials Credentials
	http        *http.Client
}

func New(c Credentials) *Client {
	return &Client{credentials: c, http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) call(ctx context.Context, method, path string, input, output any) (int, error) {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return 0, errors.New("cannot encode Cloudflare request")
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4"+path, body)
	if err != nil {
		return 0, errors.New("cannot build Cloudflare request")
	}
	req.Header.Set("Authorization", "Bearer "+c.credentials.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("Cloudflare transport failed; any submitted write requires reconciliation")
	}
	defer resp.Body.Close()
	var envelope struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code int `json:"code"`
		} `json:"errors"`
		Result json.RawMessage `json:"result"`
		Info   struct {
			TotalPages int `json:"total_pages"`
		} `json:"result_info"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return 0, &APIError{Status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !envelope.Success {
		e := &APIError{Status: resp.StatusCode, Definite: resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408}
		for _, v := range envelope.Errors {
			e.Codes = append(e.Codes, v.Code)
		}
		return 0, e
	}
	if output != nil && len(envelope.Result) > 0 && string(envelope.Result) != "null" {
		if err := json.Unmarshal(envelope.Result, output); err != nil {
			return 0, errors.New("invalid Cloudflare response; reconcile before retrying a write")
		}
	}
	return envelope.Info.TotalPages, nil
}
func zonePath(id string) string { return "/zones/" + url.PathEscape(id) }
func (c *Client) Zone(ctx context.Context, id string) (Zone, error) {
	var out Zone
	_, err := c.call(ctx, "GET", zonePath(id), nil, &out)
	return out, err
}

type zonesPageInfo struct {
	Page       *int `json:"page"`
	PerPage    *int `json:"per_page"`
	Count      *int `json:"count"`
	TotalCount *int `json:"total_count"`
	TotalPages *int `json:"total_pages"`
}

// Zones returns a complete snapshot or an error, never a partial account list.
// Discovery must not select a zone from incomplete or inconsistent pagination.
func (c *Client) Zones(ctx context.Context) ([]Zone, error) {
	const perPage, maxPages = 50, 100
	out := []Zone{}
	seen := make(map[string]bool)
	totalCount, totalPages := -1, -1
	invalid := errors.New("incomplete or inconsistent Cloudflare zone listing; refresh account discovery")
	for page := 1; page <= maxPages; page++ {
		batch, info, err := c.zonesPage(ctx, page, perPage)
		if err != nil {
			return nil, err
		}
		if info.Page == nil || info.PerPage == nil || info.Count == nil || info.TotalCount == nil || info.TotalPages == nil {
			return nil, invalid
		}
		if *info.Page != page || *info.PerPage != perPage || *info.Count != len(batch) || *info.TotalCount < 0 || *info.TotalCount > perPage*maxPages {
			return nil, invalid
		}
		expectedPages := (*info.TotalCount + perPage - 1) / perPage
		if *info.TotalPages != expectedPages && !(*info.TotalCount == 0 && *info.TotalPages == 1) {
			return nil, invalid
		}
		if page == 1 {
			totalCount, totalPages = *info.TotalCount, *info.TotalPages
		} else if totalCount != *info.TotalCount || totalPages != *info.TotalPages {
			return nil, invalid
		}
		expectedCount := min(perPage, totalCount-len(out))
		if len(batch) != expectedCount {
			return nil, invalid
		}
		for _, zone := range batch {
			id := strings.TrimSpace(zone.ID)
			if id == "" || id != zone.ID || strings.TrimSpace(zone.Name) == "" || seen[id] {
				return nil, invalid
			}
			seen[id] = true
		}
		out = append(out, batch...)
		if page >= totalPages {
			if len(out) != totalCount {
				return nil, invalid
			}
			return out, nil
		}
	}
	return nil, errors.New("Cloudflare zone pagination exceeded safe limit")
}

func (c *Client) zonesPage(ctx context.Context, page, perPage int) ([]Zone, zonesPageInfo, error) {
	var info zonesPageInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.cloudflare.com/client/v4/zones?per_page=%d&page=%d", perPage, page), nil)
	if err != nil {
		return nil, info, errors.New("cannot build Cloudflare zone listing request")
	}
	req.Header.Set("Authorization", "Bearer "+c.credentials.APIToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, info, ctx.Err()
		}
		return nil, info, errors.New("Cloudflare zone listing transport failed")
	}
	defer resp.Body.Close()
	const maxBody = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return nil, info, &APIError{Status: resp.StatusCode}
	}
	var envelope struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code int `json:"code"`
		} `json:"errors"`
		Result json.RawMessage `json:"result"`
		Info   zonesPageInfo   `json:"result_info"`
	}
	// Unmarshal consumes the entire bounded body, rejecting trailing JSON/data.
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, info, &APIError{Status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !envelope.Success {
		e := &APIError{Status: resp.StatusCode, Definite: resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408}
		for _, v := range envelope.Errors {
			e.Codes = append(e.Codes, v.Code)
		}
		return nil, info, e
	}
	var batch []Zone
	if err := json.Unmarshal(envelope.Result, &batch); err != nil || batch == nil {
		return nil, info, errors.New("invalid Cloudflare zone listing response")
	}
	return batch, envelope.Info, nil
}

func (c *Client) SSLMode(ctx context.Context, id string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	_, err := c.call(ctx, "GET", zonePath(id)+"/settings/ssl", nil, &out)
	return out.Value, err
}

func (c *Client) SetSSLMode(ctx context.Context, id, mode string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	_, err := c.call(ctx, "PATCH", zonePath(id)+"/settings/ssl", map[string]string{"value": mode}, &out)
	return out.Value, err
}
func (c *Client) Records(ctx context.Context, id, name string) ([]Record, error) {
	out := []Record{}
	for page := 1; page <= 100; page++ {
		var batch []Record
		pages, err := c.call(ctx, "GET", zonePath(id)+"/dns_records?name="+url.QueryEscape(name)+fmt.Sprintf("&per_page=100&page=%d", page), nil, &batch)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if pages <= page {
			return out, nil
		}
	}
	return nil, errors.New("Cloudflare DNS pagination exceeded safe limit")
}
func (c *Client) CreateRecord(ctx context.Context, id string, r Record) (Record, error) {
	var out Record
	_, err := c.call(ctx, "POST", zonePath(id)+"/dns_records", r, &out)
	return out, err
}
func (c *Client) Fallback(ctx context.Context, id string) (Fallback, error) {
	var out Fallback
	_, err := c.call(ctx, "GET", zonePath(id)+"/custom_hostnames/fallback_origin", nil, &out)
	var api *APIError
	if errors.As(err, &api) && api.Status == 404 {
		return out, nil
	}
	return out, err
}
func (c *Client) SetFallback(ctx context.Context, id, origin string) (Fallback, error) {
	var out Fallback
	_, err := c.call(ctx, "PUT", zonePath(id)+"/custom_hostnames/fallback_origin", map[string]string{"origin": origin}, &out)
	return out, err
}
func (c *Client) Host(ctx context.Context, id, name string) (*Hostname, error) {
	var found *Hostname
	for page := 1; page <= 100; page++ {
		var list []Hostname
		pages, err := c.call(ctx, "GET", zonePath(id)+"/custom_hostnames?hostname="+url.QueryEscape(name)+fmt.Sprintf("&per_page=50&page=%d", page), nil, &list)
		if err != nil {
			return nil, err
		}
		for _, h := range list {
			if strings.EqualFold(h.Hostname, name) {
				if found != nil {
					return nil, errors.New("multiple Cloudflare custom hostnames match")
				}
				copy := h
				found = &copy
			}
		}
		if pages <= page {
			return found, nil
		}
	}
	return nil, errors.New("Cloudflare hostname pagination exceeded safe limit")
}
func (c *Client) CreateHost(ctx context.Context, id, name string) (Hostname, error) {
	var out Hostname
	// Keep the create payload separate from response models, which contain
	// read-only ownership/status fields and paid issuance options.
	in := struct {
		Hostname string `json:"hostname"`
		SSL      struct {
			Method string `json:"method"`
			Type   string `json:"type"`
		} `json:"ssl"`
	}{Hostname: name}
	in.SSL.Method, in.SSL.Type = "txt", "dv"
	_, err := c.call(ctx, "POST", zonePath(id)+"/custom_hostnames", in, &out)
	return out, err
}

func (c *Client) DeleteHost(ctx context.Context, zoneID, hostnameID string) error {
	_, err := c.call(ctx, "DELETE", zonePath(zoneID)+"/custom_hostnames/"+url.PathEscape(hostnameID), nil, nil)
	return err
}

// Preserve the current method/type/settings for a configuration-neutral DCV
// refresh. Omitted settings can reset non-default TLS options. Issuance options
// and read-only fields must not be replayed as edits.
func (c *Client) RevalidateHost(ctx context.Context, zoneID, hostnameID string, current SSL) (Hostname, error) {
	var out Hostname
	input := SSL{Method: current.Method, Type: current.Type}
	settings := bytes.TrimSpace(current.Settings)
	if len(settings) > 0 && !bytes.Equal(settings, []byte("null")) {
		var object map[string]json.RawMessage
		if json.Unmarshal(settings, &object) != nil || object == nil {
			return out, errors.New("invalid current SSL settings; no DCV refresh submitted")
		}
		input.Settings = append(json.RawMessage{}, settings...)
	}
	_, err := c.call(ctx, "PATCH", zonePath(zoneID)+"/custom_hostnames/"+url.PathEscape(hostnameID), map[string]SSL{"ssl": input}, &out)
	return out, err
}

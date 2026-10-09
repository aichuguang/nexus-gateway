package dnsprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"

	"w103d/gateway/internal/dnspod"
)

type Cloudflare struct {
	token string
	http  *http.Client
}

func NewCloudflare(token string) *Cloudflare                         { return &Cloudflare{token: token, http: httpClient()} }
func (c *Cloudflare) WithTransport(rt http.RoundTripper) *Cloudflare { c.http.Transport = rt; return c }

var cfID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type cfEnvelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Errors  []struct {
		Code int `json:"code"`
	} `json:"errors"`
	ResultInfo struct {
		Page       *int `json:"page"`
		TotalPages *int `json:"total_pages"`
		Count      *int `json:"count"`
		TotalCount *int `json:"total_count"`
	} `json:"result_info"`
}

func (c *Cloudflare) call(ctx context.Context, method, path string, q url.Values, in, out any) (cfEnvelope, error) {
	var env cfEnvelope
	readOnly := method == http.MethodGet
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return env, e
		}
		body = bytes.NewReader(b)
	}
	endpoint := "https://api.cloudflare.com/client/v4" + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	r, e := http.NewRequestWithContext(ctx, method, endpoint, body)
	if e != nil {
		return env, e
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Content-Type", "application/json")
	resp, e := c.http.Do(r)
	if e != nil {
		if readOnly {
			return env, cloudflareReadUnavailable("只读查询网络连接失败")
		}
		return env, errors.New("Cloudflare DNS transport failed; mutation outcome may be unknown")
	}
	defer resp.Body.Close()
	if readOnly && (resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500) {
		return env, cloudflareReadUnavailable("查询超时、请求受限或服务商暂时不可用")
	}
	// A definite HTTP refusal stays non-retryable even if its body is HTML,
	// truncated, or omits Cloudflare's error envelope. No body text is exposed.
	readRejected := readOnly && resp.StatusCode >= 400 && resp.StatusCode < 500
	b, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if e != nil || len(b) > 4<<20 {
		if readRejected {
			return env, &Rejection{Provider: "Cloudflare", Code: "HTTP" + strconv.Itoa(resp.StatusCode)}
		}
		if readOnly {
			return env, cloudflareReadUnavailable("查询响应无法完整读取")
		}
		return env, errors.New("Cloudflare DNS response unreadable")
	}
	if json.Unmarshal(b, &env) != nil {
		if readRejected {
			return env, &Rejection{Provider: "Cloudflare", Code: "HTTP" + strconv.Itoa(resp.StatusCode)}
		}
		if readOnly {
			return env, cloudflareReadUnavailable("查询响应格式无效")
		}
		return env, errors.New("Cloudflare DNS response invalid")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !env.Success {
		// Never license a retry after an internal failure or an ambiguous response.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && len(env.Errors) > 0 {
			return env, &Rejection{Provider: "Cloudflare", Code: strconv.Itoa(env.Errors[0].Code)}
		}
		if readRejected {
			return env, &Rejection{Provider: "Cloudflare", Code: "HTTP" + strconv.Itoa(resp.StatusCode)}
		}
		if readOnly {
			if len(env.Errors) > 0 {
				return env, &Rejection{Provider: "Cloudflare", Code: strconv.Itoa(env.Errors[0].Code)}
			}
			return env, cloudflareReadUnavailable("查询未返回有效结果")
		}
		return env, errors.New("Cloudflare DNS request outcome unknown")
	}
	if len(env.Result) == 0 || string(env.Result) == "null" || json.Unmarshal(env.Result, out) != nil {
		if readOnly {
			return env, cloudflareReadUnavailable("查询结果缺失或格式无效")
		}
		return env, errors.New("Cloudflare DNS result missing")
	}
	return env, nil
}

func cloudflareReadUnavailable(reason string) error {
	return &ReadUnavailableError{Provider: "Cloudflare DNS", Reason: reason}
}

type cfZone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (c *Cloudflare) zones(ctx context.Context, name string) ([]cfZone, error) {
	all := []cfZone{}
	seen := map[string]bool{}
	for page := 1; page <= 40; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {"50"}}
		if name != "" {
			q.Set("name", name)
		}
		var items []cfZone
		env, e := c.call(ctx, "GET", "/zones", q, nil, &items)
		if e != nil {
			return nil, e
		}
		if items == nil {
			return nil, cloudflareReadUnavailable("托管域列表缺失")
		}
		for _, z := range items {
			if !cfID.MatchString(z.ID) || validQuery(z.Name, "@") != nil || seen[z.ID] {
				return nil, cloudflareReadUnavailable("托管域列表不一致")
			}
			seen[z.ID] = true
			if z.Status == "active" && (name == "" || z.Name == name) {
				all = append(all, z)
			}
		}
		pages := 0
		if env.ResultInfo.TotalPages != nil {
			pages = *env.ResultInfo.TotalPages
		}
		if pages > 0 && page >= pages || pages == 0 && len(items) < 50 {
			return all, nil
		}
		if len(items) == 0 {
			return nil, cloudflareReadUnavailable("托管域分页结果不完整")
		}
	}
	return nil, errors.New("Cloudflare zone list exceeds safe inspection limit")
}
func (c *Cloudflare) Zones(ctx context.Context) ([]string, error) {
	z, e := c.zones(ctx, "")
	if e != nil {
		return nil, e
	}
	out := []string{}
	seen := map[string]bool{}
	for _, v := range z {
		if !seen[v.Name] {
			out = append(out, v.Name)
			seen[v.Name] = true
		}
	}
	sort.Strings(out)
	return out, nil
}
func (c *Cloudflare) zoneID(ctx context.Context, zone string) (string, error) {
	if e := validQuery(zone, "@"); e != nil {
		return "", e
	}
	z, e := c.zones(ctx, zone)
	if e != nil {
		return "", e
	}
	if len(z) != 1 {
		return "", errors.New("Cloudflare zone is absent, inactive or ambiguous")
	}
	return z[0].ID, nil
}

type cfRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
}

func (c *Cloudflare) List(ctx context.Context, zone, sub string) ([]dnspod.Record, error) {
	if e := validQuery(zone, sub); e != nil {
		return nil, e
	}
	id, e := c.zoneID(ctx, zone)
	if e != nil {
		return nil, e
	}
	all := []dnspod.Record{}
	seen := map[string]bool{}
	total, totalPages := -1, -1
	for page := 1; page <= 10; page++ {
		q := url.Values{"name": {recordName(zone, sub)}, "page": {strconv.Itoa(page)}, "per_page": {"100"}}
		var items []cfRecord
		env, e := c.call(ctx, "GET", "/zones/"+id+"/dns_records", q, nil, &items)
		if e != nil {
			return nil, e
		}
		if items == nil {
			return nil, cloudflareReadUnavailable("解析记录列表缺失")
		}
		info := env.ResultInfo
		// Never infer completeness from a short page: replacement may delete
		// records, so missing or changing pagination metadata must fail closed.
		if info.Page == nil || info.Count == nil || info.TotalCount == nil || info.TotalPages == nil {
			return nil, cloudflareReadUnavailable("解析记录分页信息缺失")
		}
		if *info.Page != page || *info.Count != len(items) || len(items) > 100 ||
			*info.TotalCount < 0 || *info.TotalCount > 1000 || *info.TotalPages < 0 || *info.TotalPages > 10 ||
			(total >= 0 && (*info.TotalCount != total || *info.TotalPages != totalPages)) ||
			len(all)+len(items) > *info.TotalCount ||
			(*info.TotalCount > 0 && (*info.TotalPages < page || len(items) == 0)) ||
			(*info.TotalCount == 0 && (page != 1 || *info.TotalPages > 1)) {
			return nil, cloudflareReadUnavailable("解析记录分页信息不一致")
		}
		total, totalPages = *info.TotalCount, *info.TotalPages
		for _, r := range items {
			if !cfID.MatchString(r.ID) || canonical(r.Name) != recordName(zone, sub) || r.Type == "" || seen[r.ID] {
				return nil, cloudflareReadUnavailable("解析记录列表不一致")
			}
			seen[r.ID] = true
			v := r.Content
			if r.Type == dnspod.TypeCNAME {
				v = canonical(v)
			}
			// Cloudflare quotes TXT content on read, while callers hold its token.
			if r.Type == dnspod.TypeTXT {
				if plain, e := strconv.Unquote(v); e == nil {
					v = plain
				}
			}
			all = append(all, dnspod.Record{OpaqueID: r.ID, Name: sub, Type: r.Type, Value: v, TTL: r.TTL, Line: dnspod.DefaultLine, LineID: "0", Status: "ENABLE", Remark: r.Comment, Proxied: r.Proxied})
		}
		if page >= totalPages {
			if len(all) != total {
				return nil, cloudflareReadUnavailable("解析记录分页结果不完整")
			}
			sort.Slice(all, func(i, j int) bool { return all[i].Key() < all[j].Key() })
			return all, nil
		}
		if len(all) >= total {
			return nil, cloudflareReadUnavailable("解析记录分页结果不完整")
		}
	}
	return nil, errors.New("Cloudflare record list exceeds safe inspection limit")
}
func (c *Cloudflare) Put(ctx context.Context, zone string, r dnspod.Record) (string, error) {
	if e := validateRecord(zone, r); e != nil {
		return "", e
	}
	if r.ID != 0 {
		return "", errors.New("Cloudflare DNS requires opaque IDs")
	}
	id, e := c.zoneID(ctx, zone)
	if e != nil {
		return "", e
	}
	method, path := "POST", "/zones/"+id+"/dns_records"
	if r.Key() != "" {
		if !cfID.MatchString(r.Key()) {
			return "", errors.New("Cloudflare record ID invalid")
		}
		method = "PUT"
		path += "/" + r.Key()
	}
	in := map[string]any{"name": recordName(zone, r.Name), "type": r.Type, "content": r.Value, "ttl": r.TTL, "proxied": false, "comment": r.Remark}
	var out cfRecord
	if _, e = c.call(ctx, method, path, nil, in, &out); e != nil {
		return "", e
	}
	actualValue := out.Content
	if r.Type == dnspod.TypeCNAME {
		actualValue = canonical(actualValue)
	}
	if r.Type == dnspod.TypeTXT {
		if unquoted, e := strconv.Unquote(actualValue); e == nil {
			actualValue = unquoted
		}
	}
	if !cfID.MatchString(out.ID) || (r.Key() != "" && out.ID != r.Key()) || out.Proxied || canonical(out.Name) != recordName(zone, r.Name) || out.Type != r.Type || out.TTL != r.TTL || actualValue != r.Value || out.Comment != r.Remark {
		return "", fmt.Errorf("Cloudflare DNS write returned inconsistent record")
	}
	return out.ID, nil
}
func (c *Cloudflare) Delete(ctx context.Context, zone, id string) error {
	if !cfID.MatchString(id) {
		return errors.New("Cloudflare record ID invalid")
	}
	z, e := c.zoneID(ctx, zone)
	if e != nil {
		return e
	}
	var out struct {
		ID string `json:"id"`
	}
	if _, e = c.call(ctx, "DELETE", "/zones/"+z+"/dns_records/"+id, nil, nil, &out); e != nil {
		return e
	}
	if out.ID != id {
		return errors.New("Cloudflare DNS delete returned inconsistent ID")
	}
	return nil
}

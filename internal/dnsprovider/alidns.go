package dnsprovider

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"w103d/gateway/internal/dnspod"
)

type AliDNS struct {
	id, key string
	http    *http.Client
}

func NewAliDNS(id, key string) *AliDNS                       { return &AliDNS{id: id, key: key, http: httpClient()} }
func (c *AliDNS) WithTransport(rt http.RoundTripper) *AliDNS { c.http.Transport = rt; return c }

func aliEscape(v string) string { return strings.ReplaceAll(url.QueryEscape(v), "+", "%20") }
func aliSignature(values url.Values, key string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, aliEscape(k)+"="+aliEscape(values.Get(k)))
	}
	canonical := strings.Join(parts, "&")
	mac := hmac.New(sha1.New, []byte(key+"&"))
	mac.Write([]byte("POST&%2F&" + aliEscape(canonical)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (c *AliDNS) call(ctx context.Context, action string, values url.Values, out any) error {
	readOnly := action == "DescribeDomains" || action == "DescribeDomainRecords" || action == "DescribeDomainRecordInfo"
	var nonce [16]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return errors.New("cannot create AliDNS request nonce")
	}
	if values == nil {
		values = url.Values{}
	}
	values.Set("Format", "JSON")
	values.Set("Version", "2015-01-09")
	values.Set("AccessKeyId", c.id)
	values.Set("SignatureMethod", "HMAC-SHA1")
	values.Set("SignatureVersion", "1.0")
	values.Set("SignatureNonce", hex.EncodeToString(nonce[:]))
	values.Set("Timestamp", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	values.Set("Action", action)
	values.Set("Signature", aliSignature(values, c.key))
	r, e := http.NewRequestWithContext(ctx, "POST", "https://alidns.aliyuncs.com/", strings.NewReader(values.Encode()))
	if e != nil {
		return e
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, e := c.http.Do(r)
	if e != nil {
		if readOnly {
			return &ReadUnavailableError{Provider: "AliDNS", Reason: "transport failed"}
		}
		return errors.New("AliDNS transport failed; mutation outcome may be unknown")
	}
	defer resp.Body.Close()
	if readOnly && (resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500) {
		return &ReadUnavailableError{Provider: "AliDNS", Reason: "upstream temporarily unavailable"}
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if e != nil || len(b) > 4<<20 {
		if readOnly {
			return &ReadUnavailableError{Provider: "AliDNS", Reason: "response unreadable"}
		}
		return errors.New("AliDNS response unreadable")
	}
	var status struct {
		Code      string `json:"Code"`
		RequestID string `json:"RequestId"`
	}
	if json.Unmarshal(b, &status) != nil || status.RequestID == "" {
		if readOnly {
			return &ReadUnavailableError{Provider: "AliDNS", Reason: "response incomplete"}
		}
		return errors.New("AliDNS response invalid")
	}
	if status.Code != "" {
		if readOnly && (status.Code == "Throttling" || strings.HasPrefix(status.Code, "Throttling.")) {
			return &ReadUnavailableError{Provider: "AliDNS", Reason: "read rate limited"}
		}
		for _, prefix := range []string{"InvalidAccessKeyId", "SignatureDoesNotMatch", "InvalidParameter", "InvalidDomainName", "InvalidRR", "InvalidTTL", "InvalidRecordId", "MissingParameter", "Forbidden", "Throttling", "QuotaExceeded", "DomainRecordDuplicate", "InvalidProtocol"} {
			if status.Code == prefix || strings.HasPrefix(status.Code, prefix+".") {
				return &Rejection{Provider: "AliDNS", Code: prefix}
			}
		}
		return errors.New("AliDNS request outcome unknown")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || json.Unmarshal(b, out) != nil {
		return errors.New("AliDNS result invalid")
	}
	return nil
}

func (c *AliDNS) Zones(ctx context.Context) ([]string, error) {
	all := []string{}
	seen := map[string]bool{}
	for page := 1; page <= 20; page++ {
		var out struct {
			TotalCount *int `json:"TotalCount"`
			Domains    *struct {
				Domain []struct {
					DomainName string `json:"DomainName"`
				} `json:"Domain"`
			} `json:"Domains"`
		}
		if e := c.call(ctx, "DescribeDomains", url.Values{"PageNumber": {strconv.Itoa(page)}, "PageSize": {"100"}}, &out); e != nil {
			return nil, e
		}
		if out.TotalCount == nil || out.Domains == nil {
			return nil, errors.New("AliDNS domain list missing")
		}
		for _, item := range out.Domains.Domain {
			name := canonical(item.DomainName)
			if validQuery(name, "@") != nil || seen[name] {
				return nil, errors.New("AliDNS domain list inconsistent")
			}
			seen[name] = true
			all = append(all, name)
		}
		if len(all) >= *out.TotalCount {
			sort.Strings(all)
			return all, nil
		}
		if len(out.Domains.Domain) == 0 {
			return nil, errors.New("AliDNS domain pagination incomplete")
		}
	}
	return nil, errors.New("AliDNS domain list exceeds safe inspection limit")
}

type aliRecord struct {
	ID     string `json:"RecordId"`
	RR     string `json:"RR"`
	Type   string `json:"Type"`
	Value  string `json:"Value"`
	TTL    int    `json:"TTL"`
	Line   string `json:"Line"`
	Status string `json:"Status"`
	Remark string `json:"Remark"`
	Weight uint64 `json:"Weight"`
}

func (c *AliDNS) List(ctx context.Context, zone, sub string) ([]dnspod.Record, error) {
	if e := validQuery(zone, sub); e != nil {
		return nil, e
	}
	all := []dnspod.Record{}
	seen := map[string]bool{}
	read := 0
	total := -1
	for page := 1; page <= 10; page++ {
		var out struct {
			TotalCount    *int `json:"TotalCount"`
			DomainRecords *struct {
				Records []aliRecord `json:"Record"`
			} `json:"DomainRecords"`
		}
		// EXACT only applies to KeyWord and ignores RRKeyWord. COMBINATION
		// makes this RR filter exact without hiding other types, lines or states.
		e := c.call(ctx, "DescribeDomainRecords", url.Values{"DomainName": {zone}, "RRKeyWord": {sub}, "SearchMode": {"COMBINATION"}, "PageNumber": {strconv.Itoa(page)}, "PageSize": {"100"}}, &out)
		if e != nil {
			return nil, e
		}
		if out.TotalCount == nil || out.DomainRecords == nil {
			return nil, errors.New("AliDNS record list missing")
		}
		if *out.TotalCount < 0 || total >= 0 && *out.TotalCount != total || read+len(out.DomainRecords.Records) > *out.TotalCount {
			return nil, errors.New("AliDNS record pagination inconsistent")
		}
		total = *out.TotalCount
		for _, r := range out.DomainRecords.Records {
			read++
			if !cfID.MatchString(r.ID) || !strings.EqualFold(r.RR, sub) || r.Type == "" || seen[r.ID] {
				return nil, errors.New("AliDNS record list inconsistent")
			}
			seen[r.ID] = true
			line, lineID := r.Line, r.Line
			if r.Line == "default" {
				line, lineID = dnspod.DefaultLine, "0"
			}
			v := r.Value
			if r.Type == dnspod.TypeCNAME {
				v = canonical(v)
			}
			weight := r.Weight
			// AliDNS reports the default weight as 1. With a single default-line
			// record it is equivalent to unweighted; multiple records still conflict.
			if weight == 1 {
				weight = 0
			}
			all = append(all, dnspod.Record{OpaqueID: r.ID, Name: sub, Type: r.Type, Value: v, TTL: r.TTL, Line: line, LineID: lineID, Status: r.Status, Remark: r.Remark, Weight: weight})
		}
		if read == total {
			sort.Slice(all, func(i, j int) bool { return all[i].Key() < all[j].Key() })
			return all, nil
		}
		if len(out.DomainRecords.Records) == 0 {
			return nil, errors.New("AliDNS record pagination incomplete")
		}
	}
	return nil, errors.New("AliDNS record list exceeds safe inspection limit")
}

func (c *AliDNS) Put(ctx context.Context, zone string, r dnspod.Record) (string, error) {
	if e := validateRecord(zone, r); e != nil {
		return "", e
	}
	if r.ID != 0 {
		return "", errors.New("AliDNS requires opaque IDs")
	}
	in := url.Values{"RR": {r.Name}, "Type": {r.Type}, "Value": {r.Value}, "TTL": {strconv.Itoa(r.TTL)}, "Line": {"default"}}
	action := "AddDomainRecord"
	in.Set("DomainName", zone)
	if r.Key() != "" {
		if !cfID.MatchString(r.Key()) {
			return "", errors.New("AliDNS record ID invalid")
		}
		if err := c.checkRecordZone(ctx, zone, r.Key()); err != nil {
			return "", err
		}
		action = "UpdateDomainRecord"
		in.Del("DomainName")
		in.Set("RecordId", r.Key())
	}
	var out struct {
		ID string `json:"RecordId"`
	}
	if e := c.call(ctx, action, in, &out); e != nil {
		return "", e
	}
	if !cfID.MatchString(out.ID) || (r.Key() != "" && out.ID != r.Key()) {
		return "", errors.New("AliDNS write returned inconsistent ID")
	}
	return out.ID, nil
}

func (c *AliDNS) Delete(ctx context.Context, zone, id string) error {
	if e := validQuery(zone, "@"); e != nil {
		return e
	}
	if !cfID.MatchString(id) {
		return errors.New("AliDNS record ID invalid")
	}
	if err := c.checkRecordZone(ctx, zone, id); err != nil {
		return err
	}
	var out struct {
		ID string `json:"RecordId"`
	}
	if e := c.call(ctx, "DeleteDomainRecord", url.Values{"RecordId": {id}}, &out); e != nil {
		return e
	}
	if out.ID != id {
		return errors.New("AliDNS delete returned inconsistent ID")
	}
	return nil
}

func (c *AliDNS) checkRecordZone(ctx context.Context, zone, id string) error {
	var out struct {
		ID     string `json:"RecordId"`
		Domain string `json:"DomainName"`
	}
	if err := c.call(ctx, "DescribeDomainRecordInfo", url.Values{"RecordId": {id}}, &out); err != nil {
		return err
	}
	if out.ID != id || canonical(out.Domain) != zone {
		return errors.New("AliDNS record does not belong to the requested zone")
	}
	return nil
}

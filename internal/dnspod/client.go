// Package dnspod implements the small, explicit DNSPod API surface used by the
// public-entry pilot. It never retries writes or accepts a configurable endpoint.
//
// The TC3 signature itself lives in internal/tencentcloud, shared with the
// EdgeOne and SSL certificate clients. Only the parts that are DNSPod's own — the
// record model, the write policy, and the actions — are here.
package dnspod

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"w103d/gateway/internal/tencentcloud"
)

const (
	host        = "dnspod.tencentcloudapi.com"
	version     = "2021-03-23"
	DefaultLine = "默认"
	// DefaultTTL is what every record is written with unless it is the one name
	// that follows a changing address.
	DefaultTTL = 600
	// ShortTTL exists for exactly one record: the base station. Every other
	// record here describes something that does not move, and a long TTL on it is
	// free stability. The base station's whole purpose is to be repointed the
	// moment the line's address changes, and 600 seconds of a stale address is ten
	// minutes of an outage that the gateway has already finished repairing.
	//
	// It is offered rather than assumed, because the shortest TTL a DNSPod zone
	// will accept is a property of that zone's plan and nothing in the API states
	// it. DefaultTTL is the one value every plan accepts; 60 and 300 are worth
	// trying, and a real account refuses 60 with LimitExceeded.RecordTtlLimit.
	//
	// A refusal is not self-explaining. That code names neither the floor nor the
	// number to use instead, so translating it into an instruction is the caller's
	// job -- see dnsRefusalText in internal/app/dns.go. An earlier version of this
	// comment claimed the operator could simply read the rejection and act on it;
	// it cannot, and the comment was wrong about the thing it was justifying.
	ShortTTL = 60
	// MediumTTL is offered for a zone whose plan rejects ShortTTL but which still
	// wants something better than the default.
	MediumTTL = 300
)

// Credentials is the shared Tencent Cloud key pair. The alias keeps the DNS
// account API reading as DNSPod's own while the signing has one implementation.
type Credentials = tencentcloud.Credentials

// APIError and Rejected are re-exported so callers that only deal with DNS do
// not have to reach past this package for them.
type APIError = tencentcloud.APIError

func Rejected(err error) bool { return tencentcloud.Rejected(err) }

// Code returns the provider's rejection code, or "" when err is not a proven
// rejection. The upstream message is deliberately never kept — see
// tencentcloud.APIError, which discards it because it may echo request input
// back — so the code is the whole of what a caller can act on. Turning it into
// an instruction is the caller's job rather than this package's: the same code
// means different things on the DNS side and on the EdgeOne side.
func Code(err error) string {
	var e *tencentcloud.APIError
	if !errors.As(err, &e) {
		return ""
	}
	return e.Code
}

func endpoint() tencentcloud.Endpoint {
	return tencentcloud.Endpoint{Product: "DNSPod", Host: host, Service: "dnspod", Version: version}
}

type Record struct {
	ID uint64 `json:"RecordId"`
	// OpaqueID preserves non-numeric IDs without changing existing DNSPod ledgers.
	OpaqueID string `json:"opaqueId,omitempty"`
	Proxied  bool   `json:"proxied,omitempty"`
	Name     string `json:"Name"`
	Type     string `json:"Type"`
	Value    string `json:"Value"`
	TTL      int    `json:"TTL"`
	Line     string `json:"Line"`
	LineID   string `json:"LineId"`
	Status   string `json:"Status"`
	Remark   string `json:"Remark"`
	Weight   uint64 `json:"Weight"`
}

func (r Record) Key() string {
	if r.OpaqueID != "" {
		return r.OpaqueID
	}
	if r.ID == 0 {
		return ""
	}
	return strconv.FormatUint(r.ID, 10)
}

func (r *Record) SetKey(id, provider string) {
	r.ID, r.OpaqueID = 0, ""
	if provider == "" || provider == "dnspod" {
		r.ID, _ = strconv.ParseUint(id, 10, 64)
	} else {
		r.OpaqueID = id
	}
}

type Client struct {
	api *tencentcloud.Client
}

func New(c Credentials) *Client { return &Client{api: tencentcloud.New(c)} }

func (c *Client) call(ctx context.Context, action string, input, output any) error {
	return c.api.Call(ctx, endpoint(), action, input, output)
}

func validLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, r := range label {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func validSubDomain(s string) bool {
	if s == "@" {
		return true
	}
	if s == "" || len(s) > 230 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !validLabel(label) {
			return false
		}
	}
	return true
}

func validRecordName(s string) bool { return validSubDomain(strings.ReplaceAll(s, "_", "a")) }

// ValidateVerificationWrite is separate from the ordinary address editor:
// enabling certificate tokens must not turn that editor into a TXT/SPF editor.
func ValidateVerificationWrite(domain, name, value string, ttl int) error {
	if !validDomain(domain) || !validRecordName(name) || !TTLAllowed(ttl) {
		return errors.New("DNS verification name or TTL is invalid")
	}
	if value == "" || len(value) > 2048 || strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("DNS verification TXT value is invalid")
	}
	return nil
}

// validDomain accepts a canonical lowercase zone. At least two labels are
// required: a single label is a host, not a registrable zone.
func validDomain(s string) bool {
	if s == "" || len(s) > 253 || strings.ToLower(s) != s || !strings.Contains(s, ".") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !validLabel(label) {
			return false
		}
	}
	return true
}

// The record types this client is willing to write. The set is closed on
// purpose: a type nobody has designed for (MX, TXT, NS, SRV) concerns mail and
// zone delegation, and a gateway that rewrites those can take a domain offline
// in ways its own UI would never explain.
const (
	TypeA     = "A"
	TypeAAAA  = "AAAA"
	TypeCNAME = "CNAME"
	TypeTXT   = "TXT"
)

// TTLs this client is willing to write, for the same reason the record types are
// closed: the number is a promise about how long a stale answer may be served,
// and only three of them are promises this gateway has a reason to make.
var allowedTTL = []int{ShortTTL, MediumTTL, DefaultTTL}

func TTLAllowed(ttl int) bool {
	for _, candidate := range allowedTTL {
		if ttl == candidate {
			return true
		}
	}
	return false
}

// Capabilities describes what this provider can actually do, so the interface can
// offer exactly that instead of offering more and refusing later.
type Capabilities struct {
	Provider       string   `json:"provider"`
	RecordTypes    []string `json:"recordTypes"`
	TTLs           []int    `json:"ttls"`
	SupportsDelete bool     `json:"supportsDelete"`
	DefaultTTL     int      `json:"defaultTtl"`
	DefaultLine    string   `json:"defaultLine"`
}

func Supported() Capabilities {
	return Capabilities{
		Provider:       "dnspod",
		RecordTypes:    []string{TypeA, TypeAAAA, TypeCNAME},
		TTLs:           allowedTTL,
		SupportsDelete: true,
		DefaultTTL:     DefaultTTL,
		DefaultLine:    DefaultLine,
	}
}

// ValidateWrite is the single place that decides whether a write is one this
// gateway is allowed to make. The record editor, the preview path and Put all
// call it, so a value can never be accepted into state and then refused by the
// provider — or worse, shown as publishable and silently altered on the way out.
func ValidateWrite(domain, name, recordType, value string, ttl int) error {
	if recordType == TypeCNAME && (name == "_acme-challenge" || strings.HasPrefix(name, "_acme-challenge.")) {
		return ValidateDCVDelegationWrite(domain, name, value, ttl)
	}
	if !validDomain(domain) {
		return errors.New("DNSPod zone name is invalid")
	}
	if !validSubDomain(name) {
		return errors.New("DNSPod subdomain is invalid")
	}
	if !TTLAllowed(ttl) {
		return errors.New("DNSPod write uses an unsupported TTL")
	}
	switch recordType {
	case TypeA, TypeAAAA:
		addr, err := netip.ParseAddr(value)
		if err != nil || addr.Zone() != "" || addr.Is4In6() {
			return errors.New("DNSPod address record needs a bare IP address")
		}
		if (recordType == TypeA) != addr.Is4() {
			return errors.New("DNSPod address record family does not match its type")
		}
	case TypeCNAME:
		// A CNAME at the apex would displace the zone's own NS and MX records.
		if name == "@" {
			return errors.New("DNSPod CNAME is not allowed at the zone apex")
		}
		target := strings.ToLower(strings.TrimSuffix(value, "."))
		if !validDomain(target) {
			return errors.New("DNSPod CNAME target is not a domain name")
		}
		// A name that is equal to, or contained in, its own target resolves in a
		// circle and takes the whole name out of service.
		if fqdn := name + "." + domain; fqdn == target || strings.HasSuffix(fqdn, "."+target) {
			return errors.New("DNSPod CNAME target would point at itself")
		}
	default:
		return errors.New("DNSPod write uses an unsupported record type")
	}
	return nil
}

// Permit only the certificate-validation CNAME supplied by Cloudflare, not
// arbitrary underscore-bearing names in the normal address-record editor.
func ValidateDCVDelegationWrite(domain, name, value string, ttl int) error {
	if err := ValidateVerificationWrite(domain, name, value, ttl); err != nil {
		return err
	}
	if name != "_acme-challenge" && !strings.HasPrefix(name, "_acme-challenge.") {
		return errors.New("invalid DCV delegation label")
	}
	host := domain
	if name != "_acme-challenge" {
		host = strings.TrimPrefix(name, "_acme-challenge.") + "." + domain
	}
	target := strings.ToLower(strings.TrimSuffix(value, "."))
	if !validDomain(target) || !strings.HasPrefix(target, host+".") || !strings.HasSuffix(target, ".dcv.cloudflare.com") {
		return errors.New("invalid Cloudflare DCV delegation target")
	}
	return nil
}

// List fetches every line/type for exactly one name in one explicitly supplied
// zone. The zone is never implicit: a wrong zone would otherwise read or write
// another customer's records.
func (c *Client) List(ctx context.Context, domain, subDomain string) ([]Record, error) {
	if !validDomain(domain) {
		return nil, errors.New("DNSPod zone name is invalid")
	}
	if !validRecordName(subDomain) {
		return nil, errors.New("DNSPod subdomain is invalid")
	}
	const limit = 100
	all := []Record{}
	seen := map[uint64]bool{}
	var total uint64
	for page := 0; page < 10; page++ {
		var out struct {
			Records *[]Record                    `json:"RecordList"`
			Counts  *struct{ ListCount *uint64 } `json:"RecordCountInfo"`
		}
		err := c.call(ctx, "DescribeRecordList", map[string]any{
			"Domain": domain, "SubDomain": subDomain, "Limit": limit, "Offset": page * limit, "ErrorOnEmpty": "no",
		}, &out)
		if err != nil {
			return nil, err
		}
		if out.Counts == nil || out.Counts.ListCount == nil {
			return nil, errors.New("DNSPod record count is missing")
		}
		if *out.Counts.ListCount > 1000 || page > 0 && *out.Counts.ListCount != total {
			return nil, errors.New("DNSPod record count changed or exceeds inspection limit")
		}
		total = *out.Counts.ListCount
		if out.Records == nil {
			// Some Tencent response serializers represent an empty collection as
			// null. Accept it only with explicit zero-count metadata, never by
			// silently treating a missing/incomplete response as an empty zone.
			if out.Counts == nil || out.Counts.ListCount == nil || *out.Counts.ListCount != 0 {
				return nil, errors.New("DNSPod record list is missing")
			}
			empty := []Record{}
			out.Records = &empty
		}
		if len(*out.Records) > limit || uint64(len(all)+len(*out.Records)) > total {
			return nil, errors.New("DNSPod record count is inconsistent")
		}
		for _, record := range *out.Records {
			if record.ID == 0 || record.Name != subDomain || record.Type == "" || seen[record.ID] {
				return nil, errors.New("DNSPod record list is inconsistent")
			}
			seen[record.ID] = true
			all = append(all, record)
		}
		if uint64(len(all)) == total {
			sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
			return all, nil
		}
		if len(*out.Records) < limit {
			return nil, errors.New("DNSPod record pagination is incomplete")
		}
	}
	return nil, errors.New("DNSPod record list exceeds the safe inspection limit")
}

func (c *Client) Put(ctx context.Context, domain string, record Record) (uint64, error) {
	if record.Status != "ENABLE" || record.Weight != 0 || record.Remark == "" || record.OpaqueID != "" || record.Proxied {
		return 0, errors.New("DNSPod write does not match the supported record policy")
	}
	value := record.Value
	if record.Type == TypeCNAME {
		value = strings.ToLower(strings.TrimSuffix(value, "."))
	}
	if record.Type == TypeTXT {
		if !strings.HasPrefix(record.Remark, "W103D verification:") {
			return 0, errors.New("DNSPod TXT write requires verification ownership")
		}
		if err := ValidateVerificationWrite(domain, record.Name, value, record.TTL); err != nil {
			return 0, err
		}
	} else if err := ValidateWrite(domain, record.Name, record.Type, value, record.TTL); err != nil {
		return 0, err
	}
	action := "CreateRecord"
	in := map[string]any{"Domain": domain, "SubDomain": record.Name, "RecordType": record.Type, "RecordLine": DefaultLine, "Value": value, "TTL": record.TTL, "Status": "ENABLE", "Remark": record.Remark}
	if record.ID != 0 {
		action = "ModifyRecord"
		in["RecordId"] = record.ID
	}
	var out struct {
		RecordID uint64 `json:"RecordId"`
	}
	if err := c.call(ctx, action, in, &out); err != nil {
		return 0, err
	}
	if out.RecordID == 0 || (record.ID != 0 && out.RecordID != record.ID) {
		return 0, fmt.Errorf("DNSPod write returned an inconsistent record ID")
	}
	return out.RecordID, nil
}

// Delete removes exactly one record, named by the ID a List of the same zone
// returned. It is the only operation whose damage cannot be spotted by reading
// its own result afterwards, so it accepts nothing but a fully resolved ID: no
// name lookup, no wildcard, no "delete everything that looks like ours".
func (c *Client) Delete(ctx context.Context, domain string, recordID uint64) error {
	if !validDomain(domain) {
		return errors.New("DNSPod zone name is invalid")
	}
	if recordID == 0 {
		return errors.New("DNSPod delete needs a record ID")
	}
	return c.call(ctx, "DeleteRecord", map[string]any{"Domain": domain, "RecordId": recordID}, &struct{}{})
}

// Zones lists the domain names this account can manage. Zone discovery has to be
// asked of the account rather than kept in local state: the operator types a
// host name, and which account and which zone own it is a fact only the provider
// knows. A locally registered list would be one more thing to get wrong — and
// getting it wrong means writing a record into somebody else's zone.
func (c *Client) Zones(ctx context.Context) ([]string, error) {
	const limit = 100
	all := []string{}
	seen := map[string]bool{}
	for page := 0; page < 20; page++ {
		var out struct {
			Domains *[]struct {
				Name string `json:"Name"`
			} `json:"DomainList"`
			Info *struct{ ListCount *uint64 } `json:"DomainCountInfo"`
		}
		if err := c.call(ctx, "DescribeDomainList", map[string]any{"Limit": limit, "Offset": page * limit}, &out); err != nil {
			return nil, err
		}
		if out.Domains == nil {
			if out.Info != nil && out.Info.ListCount != nil && *out.Info.ListCount == 0 && page == 0 {
				return nil, errors.New("DNSPod account manages no domain")
			}
			return nil, errors.New("DNSPod domain list is missing")
		}
		for _, domain := range *out.Domains {
			name := strings.ToLower(strings.TrimSpace(domain.Name))
			if !validDomain(name) || seen[name] {
				continue
			}
			seen[name] = true
			all = append(all, name)
		}
		if len(*out.Domains) < limit {
			sort.Strings(all)
			return all, nil
		}
	}
	return nil, errors.New("DNSPod domain list exceeds the safe inspection limit")
}

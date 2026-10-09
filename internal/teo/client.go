// Package teo drives Tencent Cloud EdgeOne: the acceleration domain that
// answers for a name, the origin group it fetches from, and the two site
// settings this gateway depends on.
//
// The shapes here were taken from the official Go SDK
// (tencentcloud-sdk-go/tencentcloud/teo/v20220901) rather than from the
// documentation, because the API is full of near-misses that fail at runtime
// rather than at compile time: ModifyOriginGroup is identified by "GroupId"
// while CreateOriginGroup answers with "OriginGroupId"; DescribeOriginGroup is
// singular; the origin read back is an OriginDetail while the origin written is
// an OriginInfo; and ServerCertInfo and CertificateInfo differ only in their
// last field. Each of those is spelled out at its use site below.
//
// Nothing here retries or loops. Every method performs one action and reports
// what the API said, because an acceleration domain created twice, or an origin
// rewritten after a lost response, is a change to live traffic.
package teo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"w103d/gateway/internal/tencentcloud"
)

const (
	host    = "teo.tencentcloudapi.com"
	version = "2022-09-01"
)

type Credentials = tencentcloud.Credentials

type APIError = tencentcloud.APIError

func Rejected(err error) bool { return tencentcloud.Rejected(err) }

func endpoint() tencentcloud.Endpoint {
	// EdgeOne takes no region: the Go SDK's client is constructed with an empty
	// region string for this product, and its own source contains no action that
	// accepts one. The header is omitted rather than filled with a plausible
	// value, since a wrong region is a rejection this gateway could not explain.
	return tencentcloud.Endpoint{Product: "EdgeOne", Host: host, Service: "teo", Version: version}
}

// Domain status values. They are lowercase, and the set is closed: a status this
// gateway does not recognise is reported as-is rather than mapped onto one of
// these.
const (
	DomainOnline  = "online"
	DomainProcess = "process"
	DomainOffline = "offline"
	DomainInit    = "init"
)

// Origin type for a plain address or name. The origin group itself is
// ORIGIN_GROUP; the records inside it are IP_DOMAIN.
const (
	OriginTypeIPDomain    = "IP_DOMAIN"
	OriginTypeOriginGroup = "ORIGIN_GROUP"
)

// Certificate binding mode that uses a certificate uploaded to the SSL product.
// The other three modes have EdgeOne obtain its own certificate, which would
// hand the private key of a name this device also serves to a third party.
const CertModeSSLCert = "sslcert"

// WebSocket switch values. Lowercase, and "off" is not "no WebSocket" — it means
// EdgeOne still carries WebSocket but times an idle connection out after 15
// seconds, which is short enough that a client which does not ping will look
// broken.
const (
	WebSocketOn  = "on"
	WebSocketOff = "off"
)

// MaxWebSocketTimeout is the largest idle timeout EdgeOne accepts. The site's
// WebSocket setting is site-wide, so this is chosen for every domain on the site
// at once.
const MaxWebSocketTimeout = 120

type Client struct{ api *tencentcloud.Client }

func New(c Credentials) *Client { return &Client{api: tencentcloud.New(c)} }

func (c *Client) call(ctx context.Context, action string, input, output any) error {
	return c.api.Call(ctx, endpoint(), action, input, output)
}

// OriginRecord is one entry in an origin group. Record must be a bare address or
// host name: the API's own comment says it "does not include port information",
// and there is no port field — origin ports live on the referencing acceleration
// domain instead. A value with ":8080" appended is accepted as a host name and
// never reachable.
type OriginRecord struct {
	Record   string `json:"Record"`
	Type     string `json:"Type,omitempty"`
	RecordID string `json:"RecordId,omitempty"`
}

// OriginInfo is the origin written to an acceleration domain. It is a whole
// replacement, not a field merge: the "omit to keep" guarantee documented on the
// request applies to OriginProtocol, HttpOriginPort, HttpsOriginPort and
// IPv6Status, and the SDK promises nothing of the kind for the fields inside
// this struct. Every write here therefore sends a complete OriginInfo.
type OriginInfo struct {
	OriginType string `json:"OriginType"`
	Origin     string `json:"Origin"`
}

// OriginDetail is the origin read back. It is a different type from OriginInfo
// on purpose — the API has no OriginInfo field on its response.
type OriginDetail struct {
	OriginType      string `json:"OriginType"`
	Origin          string `json:"Origin"`
	OriginGroupName string `json:"OriginGroupName"`
	HostHeader      string `json:"HostHeader"`
}

// CertificateInfo is one certificate bound to an acceleration domain. It is not
// ServerCertInfo: this one carries Status, that one carries CommonName.
type CertificateInfo struct {
	CertID     string `json:"CertId"`
	Type       string `json:"Type"`
	Status     string `json:"Status"`
	ExpireTime string `json:"ExpireTime"`
}

type DomainCertificate struct {
	Mode string            `json:"Mode"`
	List []CertificateInfo `json:"List"`
}

// Domain is one acceleration domain as read back.
type Domain struct {
	ZoneID         string             `json:"ZoneId"`
	Name           string             `json:"DomainName"`
	Status         string             `json:"DomainStatus"`
	Cname          string             `json:"Cname"`
	Identification string             `json:"IdentificationStatus"`
	Origin         *OriginDetail      `json:"OriginDetail"`
	OriginProtocol string             `json:"OriginProtocol"`
	Certificate    *DomainCertificate `json:"Certificate"`
}

// boundCertID is the certificate currently bound, or "" when none is.
func (d Domain) BoundCertID() string {
	if d.Certificate == nil {
		return ""
	}
	for _, info := range d.Certificate.List {
		if info.CertID != "" {
			return info.CertID
		}
	}
	return ""
}

// Site is one EdgeOne site. It is the container every other action needs an ID
// for: an acceleration domain, an origin group and a certificate binding all
// live inside exactly one site, and there is no action in this API that works
// without one.
type Site struct {
	// ZoneID is spelled with a lowercase "d" — the SDK's own tags use ZoneId in
	// 460 places and ZoneID in none — and every request field that carries it
	// spells it the same way.
	ZoneID string `json:"ZoneId"`
	Name   string `json:"ZoneName"`
	// Status is the site's own status: active (NS switched), pending, moved,
	// deactivated or initializing. It is not ActiveStatus, which is about whether
	// the site is paused, and not CnameStatus, which is about verification.
	Status string `json:"Status"`
	Type   string `json:"Type"`
	Area   string `json:"Area"`
	Paused bool   `json:"Paused"`
}

// Sites lists every site the credentials can see, following pagination to the
// end. The API page size is capped at 100, and the loop is bounded so that a
// server which kept answering with a full page would end the listing rather than
// run forever.
//
// Nothing is filtered here. Which site owns which name is decided by the caller
// against this whole list, because a name's site is found by longest-suffix
// match and a filter would hide the shorter site that also matches.
func (c *Client) Sites(ctx context.Context) ([]Site, error) {
	const pageSize = 100
	const maxPages = 10
	sites := []Site{}
	for page := 0; page < maxPages; page++ {
		var out struct {
			TotalCount *int64  `json:"TotalCount"`
			Zones      *[]Site `json:"Zones"`
		}
		if err := c.call(ctx, "DescribeZones", map[string]any{
			"Offset": page * pageSize,
			"Limit":  pageSize,
		}, &out); err != nil {
			return nil, err
		}
		if out.Zones == nil {
			// A missing list is only believed when the count agrees it is empty.
			if out.TotalCount != nil && *out.TotalCount == 0 {
				return sites, nil
			}
			return nil, errors.New("EdgeOne 站点列表缺失")
		}
		if len(*out.Zones) == 0 {
			return sites, nil
		}
		sites = append(sites, *out.Zones...)
		if out.TotalCount != nil && int64(len(sites)) >= *out.TotalCount {
			return sites, nil
		}
		if len(*out.Zones) < pageSize {
			return sites, nil
		}
	}
	return sites, nil
}

// DescribeDomain returns the acceleration domain for one host, or nil when the
// site does not have one. The filter name is hyphenated — "domain-name" — and a
// misspelling is rejected rather than ignored.
func (c *Client) DescribeDomain(ctx context.Context, zoneID, name string) (*Domain, error) {
	var out struct {
		TotalCount *int64    `json:"TotalCount"`
		Domains    *[]Domain `json:"AccelerationDomains"`
	}
	err := c.call(ctx, "DescribeAccelerationDomains", map[string]any{
		"ZoneId": zoneID,
		"Filters": []map[string]any{
			{"Name": "domain-name", "Values": []string{name}},
		},
		"Limit": 20,
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.Domains == nil {
		// A null collection is only trusted alongside explicit zero metadata: a
		// missing list must not read as "this site has no such domain", which is
		// the answer that leads to creating a second one.
		if out.TotalCount != nil && *out.TotalCount == 0 {
			return nil, nil
		}
		return nil, errors.New("EdgeOne 加速域名列表缺失")
	}
	for i := range *out.Domains {
		if strings.EqualFold((*out.Domains)[i].Name, name) {
			return &(*out.Domains)[i], nil
		}
	}
	if out.TotalCount != nil && *out.TotalCount != 0 {
		return nil, errors.New("EdgeOne 加速域名列表与查询条件不一致")
	}
	return nil, nil
}

// CreateDomain creates the acceleration domain for one host, fetching from the
// given origin group over HTTPS only.
//
// OriginProtocol is HTTPS rather than the default FOLLOW because this device
// serves a certificate for the name and its port 80 is only a redirect: telling
// EdgeOne to follow the client's protocol would send every plain-HTTP visitor
// through a redirect that EdgeOne then caches. HttpsOriginPort is left unset,
// which the API reads as 443.
func (c *Client) CreateDomain(ctx context.Context, zoneID, name, originGroupID string) error {
	return c.call(ctx, "CreateAccelerationDomain", map[string]any{
		"ZoneId":     zoneID,
		"DomainName": name,
		"OriginInfo": OriginInfo{OriginType: OriginTypeOriginGroup, Origin: originGroupID},
		// IPv6Status is on: the device's public side is IPv6, and the site's own
		// default is not something this gateway can read back and rely on.
		"IPv6Status": "on",
	}, &struct{}{})
}

// DeleteDomain removes exactly one acceleration domain. The official API is
// plural even for one name; Force stays false so aliases, traffic scheduling
// and other associated resources are never removed implicitly. The caller
// verifies ownership and takes the domain offline before requesting deletion.
// This method neither disables the domain nor retries a rejected/unknown write.
func (c *Client) DeleteDomain(ctx context.Context, zoneID, name string) error {
	if strings.TrimSpace(zoneID) == "" || strings.TrimSpace(name) == "" {
		return errors.New("EdgeOne deletion requires a site and domain name")
	}
	return c.call(ctx, "DeleteAccelerationDomains", map[string]any{
		"ZoneId":      zoneID,
		"DomainNames": []string{name},
		"Force":       false,
	}, &struct{}{})
}

// SetDomainOrigin points an existing acceleration domain at an origin group.
// ModifyAccelerationDomain identifies the domain by name; this API version has
// no DomainId field anywhere.
func (c *Client) SetDomainOrigin(ctx context.Context, zoneID, name, originGroupID string) error {
	return c.call(ctx, "ModifyAccelerationDomain", map[string]any{
		"ZoneId":     zoneID,
		"DomainName": name,
		"OriginInfo": OriginInfo{OriginType: OriginTypeOriginGroup, Origin: originGroupID},
	}, &struct{}{})
}

// SetDomainsOnline enables or disables acceleration domains in one call. An
// offline domain keeps its CNAME, so a name pointed at it resolves and fails —
// which is why nothing here takes a domain offline automatically.
func (c *Client) SetDomainsOnline(ctx context.Context, zoneID string, names []string, online bool) error {
	status := DomainOffline
	if online {
		status = DomainOnline
	}
	return c.call(ctx, "ModifyAccelerationDomainStatuses", map[string]any{
		"ZoneId":      zoneID,
		"DomainNames": names,
		"Status":      status,
	}, &struct{}{})
}

// CnameStatus asks EdgeOne whether each host's DNS record actually points at the
// CNAME it assigned. Values are "active", "moved" or "invalid"; an "invalid"
// means the name is pointed at a CNAME belonging to a different site, which
// serves the wrong content rather than failing.
func (c *Client) CnameStatus(ctx context.Context, zoneID string, names []string) (map[string]string, error) {
	var out struct {
		Status []struct {
			RecordName string `json:"RecordName"`
			Cname      string `json:"Cname"`
			Status     string `json:"Status"`
		} `json:"CnameStatus"`
	}
	if err := c.call(ctx, "CheckCnameStatus", map[string]any{
		"ZoneId": zoneID, "RecordNames": names,
	}, &out); err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, entry := range out.Status {
		result[strings.ToLower(entry.RecordName)] = entry.Status
	}
	return result, nil
}

// DescribeOriginGroup returns one origin group by ID, or nil when the site no
// longer has it. The action is singular even though the response's list is not.
func (c *Client) DescribeOriginGroup(ctx context.Context, zoneID, groupID string) (*OriginGroup, error) {
	var out struct {
		TotalCount *uint64        `json:"TotalCount"`
		Groups     *[]OriginGroup `json:"OriginGroups"`
	}
	err := c.call(ctx, "DescribeOriginGroup", map[string]any{
		"ZoneId": zoneID,
		"Filters": []map[string]any{
			{"Name": "origin-group-id", "Values": []string{groupID}},
		},
		"Limit": 20,
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.Groups == nil {
		if out.TotalCount != nil && *out.TotalCount == 0 {
			return nil, nil
		}
		return nil, errors.New("EdgeOne 源站组列表缺失")
	}
	for i := range *out.Groups {
		if (*out.Groups)[i].GroupID == groupID {
			return &(*out.Groups)[i], nil
		}
	}
	return nil, nil
}

type OriginGroup struct {
	GroupID string         `json:"GroupId"`
	Name    string         `json:"Name"`
	Type    string         `json:"Type"`
	Records []OriginRecord `json:"Records"`
	Refs    []struct {
		InstanceID   string `json:"InstanceId"`
		InstanceType string `json:"InstanceType"`
	} `json:"References"`
}

// CreateOriginGroup makes a new origin group and returns its ID. The answer's
// field is OriginGroupId while every other action calls the same thing GroupId.
//
// The group is created with a name carrying a random suffix and its ID is stored
// in the gateway's own state. Nothing here ever looks a group up by name: an
// origin group has no ownership marker, so a group found by name might belong to
// another operator's domain in the same site, and rewriting its records would
// move traffic for a site this gateway has never heard of. The cost is that a
// group left behind by a removed installation stays in the account; that is
// visible and harmless, which the alternative is not.
func (c *Client) CreateOriginGroup(ctx context.Context, zoneID, name, record string) (string, error) {
	var out struct {
		OriginGroupID *string `json:"OriginGroupId"`
	}
	if err := c.call(ctx, "CreateOriginGroup", map[string]any{
		"ZoneId":  zoneID,
		"Name":    name,
		"Type":    "GENERAL",
		"Records": []OriginRecord{{Record: record, Type: OriginTypeIPDomain}},
	}, &out); err != nil {
		return "", err
	}
	if out.OriginGroupID == nil || *out.OriginGroupID == "" {
		return "", errors.New("EdgeOne 未返回源站组 ID")
	}
	return *out.OriginGroupID, nil
}

// SetOriginGroupRecord replaces the single origin record in a group. The whole
// record list is sent: ModifyOriginGroup merges at the field level, and omitting
// Records would leave the old address in place.
func (c *Client) SetOriginGroupRecord(ctx context.Context, zoneID, groupID, record string) error {
	return c.call(ctx, "ModifyOriginGroup", map[string]any{
		"ZoneId":  zoneID,
		"GroupId": groupID,
		"Records": []OriginRecord{{Record: record, Type: OriginTypeIPDomain}},
	}, &struct{}{})
}

// ServerCertInfo is the certificate written to a domain. It is not
// CertificateInfo: this one has CommonName where that one has Status, and using
// the wrong one silently sends a field the API ignores.
type ServerCertInfo struct {
	CertID     string `json:"CertId"`
	CommonName string `json:"CommonName,omitempty"`
}

// BindCertificate points domains at a certificate from the SSL product.
//
// Hosts are batched per site because they share one certificate, and a batch
// with inconsistent certificate settings is rejected outright — which is the
// API telling us the batch was wrong, not that the certificate was.
func (c *Client) BindCertificate(ctx context.Context, zoneID string, hosts []string, certID string) error {
	return c.call(ctx, "ModifyHostsCertificate", map[string]any{
		"ZoneId": zoneID,
		"Hosts":  hosts,
		"Mode":   CertModeSSLCert,
		"ServerCertInfo": []ServerCertInfo{
			{CertID: certID},
		},
	}, &struct{}{})
}

// WebSocketSetting reads the site's WebSocket configuration. The read nests the
// configuration one level deeper than the write does — ZoneSetting.ZoneConfig
// here, ZoneConfig there — and getting that wrong yields a nil dereference
// rather than a rejection.
func (c *Client) WebSocketSetting(ctx context.Context, zoneID string) (string, int64, error) {
	var out struct {
		Setting *struct {
			Config *struct {
				WebSocket *struct {
					Switch  string `json:"Switch"`
					Timeout int64  `json:"Timeout"`
				} `json:"WebSocket"`
			} `json:"ZoneConfig"`
		} `json:"ZoneSetting"`
	}
	if err := c.call(ctx, "DescribeL7AccSetting", map[string]any{"ZoneId": zoneID}, &out); err != nil {
		return "", 0, err
	}
	if out.Setting == nil || out.Setting.Config == nil || out.Setting.Config.WebSocket == nil {
		// Absent means the site has never had this configured, which is the same
		// situation as "off": EdgeOne carries WebSocket but times it out at 15s.
		return WebSocketOff, 0, nil
	}
	return out.Setting.Config.WebSocket.Switch, out.Setting.Config.WebSocket.Timeout, nil
}

// EnableWebSocket turns on the site's WebSocket timeout and sets it to the
// maximum. This is a site-wide change: it applies to every domain on the site,
// including ones this gateway does not manage, which is why the caller must show
// it to the operator before it happens.
//
// The maximum is 120 seconds, which is the API's documented cap and is far below
// the gateway's own upstream read timeout. A WebSocket that idles for longer
// than this will be closed by EdgeOne's edge regardless of what the device
// behind it would have allowed.
func (c *Client) EnableWebSocket(ctx context.Context, zoneID string, timeout int64) error {
	if timeout < 1 || timeout > MaxWebSocketTimeout {
		return fmt.Errorf("EdgeOne 的 WebSocket 超时上限为 %d 秒", MaxWebSocketTimeout)
	}
	// ModifyL7AccSetting merges: the SDK's own comment promises that settings not
	// passed keep their current values, so sending only WebSocket leaves caching,
	// HTTPS redirection and everything else untouched.
	return c.call(ctx, "ModifyL7AccSetting", map[string]any{
		"ZoneId": zoneID,
		"ZoneConfig": map[string]any{
			"WebSocket": map[string]any{"Switch": WebSocketOn, "Timeout": timeout},
		},
	}, &struct{}{})
}

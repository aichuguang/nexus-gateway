// Package dnsprovider provides the DNS-only APIs used by the gateway. IDs are
// opaque, endpoints are fixed, and no client retries a mutation.
package dnsprovider

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"w103d/gateway/internal/dnspod"
)

type Credentials struct {
	Provider  string `json:"provider,omitempty"`
	SecretID  string `json:"secretId,omitempty"`
	SecretKey string `json:"secretKey,omitempty"`
	APIToken  string `json:"apiToken,omitempty"`
}

func (c Credentials) Kind() string {
	if c.Provider == "" {
		return "dnspod"
	}
	return c.Provider
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{8,256}$`)

func (c Credentials) Valid() bool {
	switch c.Kind() {
	case "dnspod":
		return (dnspod.Credentials{SecretID: c.SecretID, SecretKey: c.SecretKey}).Valid() && c.APIToken == ""
	case "alidns":
		return keyPattern.MatchString(c.SecretID) && keyPattern.MatchString(c.SecretKey) && c.APIToken == ""
	case "cloudflare":
		return keyPattern.MatchString(c.APIToken) && c.SecretID == "" && c.SecretKey == ""
	}
	return false
}

func (c Credentials) Hint() string {
	v := c.SecretID
	if c.Kind() == "cloudflare" {
		v = c.APIToken
	}
	if len(v) < 4 {
		return "••••"
	}
	return "••••" + v[len(v)-4:]
}

func (c Credentials) Fingerprint() [32]byte {
	return sha256.Sum256([]byte(c.Kind() + "\x00" + c.SecretID + "\x00" + c.SecretKey + "\x00" + c.APIToken))
}

type Client interface {
	List(context.Context, string, string) ([]dnspod.Record, error)
	Put(context.Context, string, dnspod.Record) (string, error)
	Delete(context.Context, string, string) error
	Zones(context.Context) ([]string, error)
}

// Legacy is retained for the existing DNSPod mock and numeric API contract.
type Legacy interface {
	List(context.Context, string, string) ([]dnspod.Record, error)
	Put(context.Context, string, dnspod.Record) (uint64, error)
	Delete(context.Context, string, uint64) error
	Zones(context.Context) ([]string, error)
}

type legacyAdapter struct{ Legacy }

func WrapLegacy(c Legacy) Client { return legacyAdapter{c} }
func (c legacyAdapter) Put(ctx context.Context, zone string, r dnspod.Record) (string, error) {
	if r.OpaqueID != "" {
		return "", errors.New("DNSPod record has a foreign ID")
	}
	id, err := c.Legacy.Put(ctx, zone, r)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(id, 10), nil
}
func (c legacyAdapter) Delete(ctx context.Context, zone, id string) error {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n == 0 {
		return errors.New("DNSPod record ID is invalid")
	}
	return c.Legacy.Delete(ctx, zone, n)
}

func New(c Credentials) Client {
	switch c.Kind() {
	case "cloudflare":
		return NewCloudflare(c.APIToken)
	case "alidns":
		return NewAliDNS(c.SecretID, c.SecretKey)
	default:
		return WrapLegacy(dnspod.New(dnspod.Credentials{SecretID: c.SecretID, SecretKey: c.SecretKey}))
	}
}

type Rejection struct{ Provider, Code string }

func (e *Rejection) Error() string { return e.Provider + " rejected DNS request (" + e.Code + ")" }
func Rejected(err error) bool {
	var r *Rejection
	return errors.As(err, &r) || dnspod.Rejected(err)
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func recordName(zone, sub string) string {
	if sub == "@" {
		return zone
	}
	return sub + "." + zone
}

func validQuery(zone, sub string) error {
	// Reuse the same strict name/TTL policy; TXT permits verification labels.
	return dnspod.ValidateVerificationWrite(zone, sub, "check", dnspod.DefaultTTL)
}

func validateRecord(zone string, r dnspod.Record) error {
	if r.Proxied || r.Status != "ENABLE" || r.Weight != 0 || r.Line != dnspod.DefaultLine || r.LineID != "0" {
		return errors.New("DNS write must be enabled, DNS-only, unweighted and on the default line")
	}
	if r.Type == dnspod.TypeTXT {
		return dnspod.ValidateVerificationWrite(zone, r.Name, r.Value, r.TTL)
	}
	return dnspod.ValidateWrite(zone, r.Name, r.Type, r.Value, r.TTL)
}

func canonical(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
}

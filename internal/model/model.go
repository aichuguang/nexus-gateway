package model

import (
	"net/url"
	"strconv"
)

// SchemaVersion is persisted in every state file. A file written by a different
// version is refused explicitly: an unrecognised schema must never be silently
// migrated, reset or mistaken for a fresh installation.
//
// Version 4 adds Service.EntryMode, Settings.BaseDomain and EdgeOneAccount. It
// is a real break rather than a tolerant addition: a build that does not know
// about EntryMode reads an EdgeOne-published service as a directly published
// one and then rewrites its DNS record to point at this device, taking a working
// site off the air. Ignoring an unknown field is only harmless when the field is
// decoration; this one decides whether a cloud write happens.
const SchemaVersion = 6

// EntryMode says who answers for a service's names. It is the difference
// between a name that must follow this device's changing public address and a
// name that must not.
type EntryMode string

const (
	// EntryDirect is the gateway itself: the name resolves to this device's
	// public address and this device terminates TLS.
	EntryDirect EntryMode = "direct"
	// EntryEdge is EdgeOne in front of this device. The name resolves to
	// EdgeOne, EdgeOne fetches from this device, and the name is deliberately
	// NOT pointed at this device's address — so an address change must leave it
	// alone rather than "repair" it into an outage.
	EntryEdge EntryMode = "edge"
)

// Service is one EasyTier upstream and the way it is published. That is the
// whole model: the gateway relays a private address out to its own EasyTier
// address and to its own public address, and a service says which entry styles
// it uses.
//
//   - With Domains, the service is reached as https://<domain>. It shares port
//     443 with every other named service and is separated by SNI, so it occupies
//     no port of its own and needs a certificate.
//   - Without Domains, it is reached as [<gateway address>]:Port. It occupies
//     that port on both the EasyTier address and the public address, needs no
//     certificate, and speaks whatever the upstream speaks.
//
// The two are deliberately exclusive: assigning a name is a decision about how
// the service is reached, not an extra alias bolted onto a numbered entry.
type Service struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Icon is optional UI metadata, not part of the proxy generation or routing.
	Icon        string       `json:"icon,omitempty"`
	Upstream    string       `json:"upstream"`
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"`
	// Port is the published port for a service with no domains. Zero means "the
	// upstream's own port", which is what makes the common case — the relay keeps
	// the number it had — require no typing at all.
	Port          int      `json:"port,omitempty"`
	Domains       []string `json:"domains"`
	CertificateID string   `json:"certificateId,omitempty"`
	// EntryMode only means anything for a service published by name; a port entry
	// has no name for anyone to front.
	EntryMode       EntryMode `json:"entryMode,omitempty"`
	EdgeProvider    string    `json:"edgeProvider,omitempty"`
	EdgeAccountID   string    `json:"edgeAccountId,omitempty"`
	UsePreferred    bool      `json:"usePreferred,omitempty"`
	PreferredDomain string    `json:"preferredDomain,omitempty"`
	Enabled         bool      `json:"enabled"`
}

// HealthCheck changes only the authorized upstream's HTTP health request.
type HealthCheck struct {
	Path                string `json:"path,omitempty"`
	ExpectedStatusCodes []int  `json:"expectedStatusCodes,omitempty"`
}

// UsesDomains reports which of the two entry styles this service uses.
func (s Service) UsesDomains() bool { return len(s.Domains) > 0 }

// ServesEdge reports whether an external accelerator answers for this service's
// names. The zero value is direct: every service written before this field
// existed was published by the gateway itself, and that reading is not a guess.
func (s Service) ServesEdge() bool { return s.UsesDomains() && s.EntryMode == EntryEdge }

// Empty provider preserves the meaning of existing EdgeOne services.
func (s Service) EffectiveEdgeProvider() string {
	if !s.ServesEdge() {
		return ""
	}
	if s.EdgeProvider == "" {
		return "edgeone"
	}
	return s.EdgeProvider
}

func (s Service) ServesCloudflare() bool { return s.EffectiveEdgeProvider() == "cloudflare" }
func (s Service) ServesEdgeOne() bool    { return s.EffectiveEdgeProvider() == "edgeone" }

// PublishedPort is the port this service occupies, or 0 when it is published by
// name instead.
func (s Service) PublishedPort() int {
	if s.UsesDomains() {
		return 0
	}
	if s.Port != 0 {
		return s.Port
	}
	return UpstreamPort(s.Upstream)
}

// UpstreamPort reads the port out of an upstream address, returning 0 when the
// address does not carry one. Validation rejects such an upstream, so 0 here
// only ever means "not yet validated" — never a port the caller should trust.
func UpstreamPort(raw string) int {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return 0
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

// CertificateEdgeOne records what EdgeOne currently holds for this certificate.
// It sits beside the local facts rather than inside them because the two fail
// independently: a certificate that is live on this device and stale at EdgeOne
// is a working service and a broken one at the same time, and a single "ready"
// flag would have to pick one of them to lie about.
//
// Uploads are kept per account rather than as one ID, because a certificate ID
// in the SSL product belongs to one Tencent Cloud account. The same certificate
// offered to two accounts is two uploads with two IDs, and using one account's
// ID in the other's site is rejected — which is the good outcome, but only if
// the ledger ever held the right one.
type CertificateEdgeOne struct {
	Uploads  []CertificateUpload  `json:"uploads,omitempty"`
	Bindings []CertificateBinding `json:"bindings,omitempty"`
}

// CertificateUpload is one certificate ID in one account's SSL product.
type CertificateUpload struct {
	AccountID string `json:"accountId"`
	CertID    string `json:"certId"`
	// Fingerprint is the fingerprint this ID was uploaded for. It is what makes
	// propagation idempotent: the same certificate must not be uploaded again, and
	// a different one must be.
	Fingerprint string `json:"fingerprint"`
	At          string `json:"at,omitempty"`
}

// CertificateBinding is the result of a certificate being bound to the hosts of
// one site. It is per site because EdgeOne's certificate call is: hosts in one
// site share a certificate, and a batch whose hosts disagree is rejected.
type CertificateBinding struct {
	AccountID   string   `json:"accountId"`
	ZoneID      string   `json:"zoneId"`
	ZoneName    string   `json:"zoneName,omitempty"`
	CertID      string   `json:"certId"`
	Fingerprint string   `json:"fingerprint"`
	Hosts       []string `json:"hosts"`
	Error       string   `json:"error,omitempty"`
	At          string   `json:"at,omitempty"`
}

// CertIDFor returns the certificate ID this account was given for this exact
// fingerprint, or "" when it was never uploaded there.
func (e *CertificateEdgeOne) CertIDFor(accountID, fingerprint string) string {
	if e == nil || fingerprint == "" {
		return ""
	}
	for _, upload := range e.Uploads {
		if upload.AccountID == accountID && upload.Fingerprint == fingerprint && upload.CertID != "" {
			return upload.CertID
		}
	}
	return ""
}

// BindingFor returns the last binding recorded for one site, if any.
func (e *CertificateEdgeOne) BindingFor(accountID, zoneID string) (CertificateBinding, bool) {
	if e == nil {
		return CertificateBinding{}, false
	}
	for _, binding := range e.Bindings {
		if binding.AccountID == accountID && binding.ZoneID == zoneID {
			return binding, true
		}
	}
	return CertificateBinding{}, false
}

type Certificate struct {
	MaterialVersion string   `json:"materialVersion,omitempty"`
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Domains         []string `json:"domains"`
	NotBefore       string   `json:"notBefore"`
	NotAfter        string   `json:"notAfter"`
	Fingerprint     string   `json:"fingerprint"`
	Ready           bool     `json:"ready"`
	Error           string   `json:"error,omitempty"`
	InboxPath       string   `json:"inboxPath,omitempty"`
	// EdgeOne is nil until this certificate has been offered to EdgeOne at all.
	EdgeOne *CertificateEdgeOne `json:"edgeOne,omitempty"`
}

// DNSAccount is the addressable identity of a cloud DNS account. The secret
// itself never appears in state, exports or listings; only a masked hint and a
// credential version are kept here.
type DNSAccount struct {
	CredentialFile    string `json:"credentialFile,omitempty"`
	ID                string `json:"id"`
	Name              string `json:"name"`
	Provider          string `json:"provider"`
	SecretIDHint      string `json:"secretIdHint"`
	CredentialVersion int64  `json:"credentialVersion"`
	Enabled           bool   `json:"enabled"`
}

// EdgeOne origin modes. They differ only in what EdgeOne is told to fetch from,
// and the choice is the operator's because the two fail differently.
const (
	// EdgeOriginAddress points EdgeOne at the current public address. Changing it
	// is an explicit configuration write, so a new address is certain to be
	// pushed — but this device has to be alive and reachable at the moment the
	// address changes for that to happen.
	EdgeOriginAddress = "address"
	// EdgeOriginBase points EdgeOne at the base station domain. Nothing needs to
	// call EdgeOne when the address changes, because EdgeOne re-resolves the name
	// — but how long EdgeOne caches that resolution is not documented anywhere in
	// its API, so the switchover window is not something this gateway can promise.
	EdgeOriginBase = "base"
)

// EdgeOneAccount is the addressable identity of a Tencent Cloud EdgeOne account.
// It is separate from DNSAccount even though both are Tencent Cloud keys,
// because the same cloud is not the same credential or the same permission
// scope, and an account that can write DNS records cannot necessarily create an
// acceleration domain.
type EdgeOneAccount struct {
	CredentialFile    string `json:"credentialFile,omitempty"`
	ID                string `json:"id"`
	Name              string `json:"name"`
	SecretIDHint      string `json:"secretIdHint"`
	CredentialVersion int64  `json:"credentialVersion"`
	// OriginMode is EdgeOriginAddress or EdgeOriginBase; empty reads as address,
	// which is the mode that is certain to take effect.
	OriginMode string `json:"originMode,omitempty"`
	Enabled    bool   `json:"enabled"`
}

// EdgeOrigin is the origin group this gateway created inside one EdgeOne site.
// The ID is stored rather than searched for by name: an origin group carries no
// ownership marker, so a group found by name could be someone else's, and
// rewriting its records would move traffic for sites this gateway has never
// heard of.
type EdgeOrigin struct {
	AccountID string `json:"accountId"`
	ZoneID    string `json:"zoneId"`
	// ZoneName is the site's own name, kept so that local decisions — which site
	// a domain belongs to, and whether an account is still in use — can be made
	// without a cloud call. It is a cache of something EdgeOne owns, and it is
	// only ever used to refuse or to explain, never to authorise a write: every
	// write resolves the site live.
	ZoneName string `json:"zoneName,omitempty"`
	GroupID  string `json:"groupId"`
	// Value is the origin record last written, kept so a preview can say what
	// will change rather than only what ought to be true.
	Value string `json:"value,omitempty"`
}

// Settings holds startup-scoped values.
type Settings struct {
	GatewayIP string `json:"gatewayIp"`
	CertdURL  string `json:"certdUrl"`
	// AutoResolve republishes the gateway's changed public address without an
	// operator click. The public address of a home line changes on its own
	// schedule, so a gateway that waits to be told goes dark until someone
	// notices.
	AutoResolve bool `json:"autoResolve"`
	// BaseDomain is the one name whose record follows the public address. Every
	// directly published service then points at this name instead of at the
	// address, so an address change is one write rather than one per service —
	// and the number of writes stops growing with the number of services.
	//
	// It is deliberately not required. A gateway with one service is simpler
	// without it, and every service that names it depends on it existing.
	BaseDomain string `json:"baseDomain,omitempty"`
	// BaseTTL is the TTL written on the base station record alone. It is the one
	// record where a short TTL buys something: everything else here describes
	// something that does not move.
	BaseTTL int `json:"baseTtl,omitempty"`
}

// EffectiveBaseTTL is the TTL to write on the base station record, resolving the
// unset case. Zero must not reach the provider: DNSPod reads it as "no TTL
// given" and rejects the write, which would look like a credentials problem.
func (s Settings) EffectiveBaseTTL() int {
	if s.BaseTTL > 0 {
		return s.BaseTTL
	}
	return DefaultBaseTTL
}

// DefaultBaseTTL is what the base station record is written with before the
// operator chooses otherwise.
const DefaultBaseTTL = 600

// GenerationSettings is the part of Settings that can change rendered
// configuration. Everything that only affects DNS is blanked here: the base
// station name and its TTL change no server block, so changing them must not
// invalidate an applied snapshot or force an unrelated re-apply.
func (s Settings) GenerationSettings() Settings {
	s.AutoResolve = false
	s.BaseDomain = ""
	s.BaseTTL = 0
	return s
}

type State struct {
	SchemaVersion   int    `json:"schemaVersion"`
	Revision        int64  `json:"revision"`
	AppliedRevision int64  `json:"appliedRevision"`
	AppliedAt       string `json:"appliedAt"`
	// AppliedPublicAddress records the address the current applied snapshot was
	// rendered for. Detection changes on the network's schedule, applying changes
	// on the operator's, and DNS must never be pointed at an address the proxy is
	// not actually listening on — so the two are compared, never assumed equal.
	AppliedPublicAddress string              `json:"appliedPublicAddress,omitempty"`
	Settings             Settings            `json:"settings"`
	Services             []Service           `json:"services"`
	Certificates         []Certificate       `json:"certificates"`
	DNSAccounts          []DNSAccount        `json:"dnsAccounts"`
	EdgeOneAccounts      []EdgeOneAccount    `json:"edgeOneAccounts"`
	EdgeOrigins          []EdgeOrigin        `json:"edgeOrigins"`
	CloudflareAccounts   []CloudflareAccount `json:"cloudflareAccounts,omitempty"`
	DNSBindings          []DNSBinding        `json:"dnsBindings,omitempty"`
}

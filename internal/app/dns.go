package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"w103d/gateway/internal/dnspod"
	"w103d/gateway/internal/dnsprovider"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

// The four things that can be done to one name at the provider. They are the
// whole vocabulary: there is no bulk operation and no "sync everything", because
// a bulk operation that half-succeeds cannot be explained to the operator.
const (
	dnsCreate  = "create"
	dnsUpdate  = "update"
	dnsDelete  = "delete"
	dnsNoop    = "noop"
	dnsReplace = "replace"
	// dnsRelease is the one action that touches no cloud at all: a name the
	// gateway has decided it does not own is dropped from its own ledger. It
	// exists because handing a name to another system must be revocable without
	// touching that system's records — deleting a record another system now
	// depends on is how a "cleanup" becomes an outage.
	dnsRelease = "release"
	// dnsAdopt is the other action that reaches no cloud. The provider already
	// holds exactly the record the gateway wants, so the only thing wrong is the
	// gateway's own ledger. It is what lets an operator who repointed a name by
	// hand — or moved it onto another system and back — clear the conflict the
	// interface would otherwise report forever.
	dnsAdopt = "adopt"
)

type dnsAPI interface {
	List(context.Context, string, string) ([]dnspod.Record, error)
	Put(context.Context, string, dnspod.Record) (uint64, error)
	Delete(context.Context, string, uint64) error
	Zones(context.Context) ([]string, error)
}

type dnsPrevious struct {
	RecordID uint64 `json:"recordId"`
	OpaqueID string `json:"opaqueId,omitempty"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	TTL      int    `json:"ttl"`
}

func (p dnsPrevious) Key() string { return (dnspod.Record{ID: p.RecordID, OpaqueID: p.OpaqueID}).Key() }

// dnsDesired is the record this gateway intends to see at the provider for one
// host name, resolved from the applied configuration rather than from whatever
// the draft says.
//
// The value comes from the applied snapshot, never from the draft: a name that
// pointed at an address nothing is serving would be worse than no name at all,
// because it fails slowly and looks like a network problem.
type dnsDesired struct {
	Host      string
	ServiceID string
	Enabled   bool
	// External marks a name the gateway has decided not to own. It is set when a
	// service is published through something the gateway cannot address itself,
	// and its only consequence is that the ledger entry is dropped without a
	// single cloud call. Refusing instead would leave the operator holding a name
	// that is working perfectly and a gateway that reports it as a conflict
	// forever — and it would stop every name queued behind it.
	External bool
	// Base marks the one name whose record follows this device's public address.
	// It is the only name whose TTL is short and the only one whose value is the
	// address rather than a name.
	Base     bool
	Account  model.DNSAccount
	Zone     string
	Sub      string
	Type     string
	Value    string
	TTL      int
	Revision int64
}

// dnsPlan is the single change a preview authorises.
type dnsPlan struct {
	Action   string
	Desired  dnspod.Record
	Previous *dnsPrevious
	// Adopt is the record already at the provider that the gateway is taking
	// into its ledger. It is set only for dnsAdopt, which writes nothing at all.
	Adopt       *dnspod.Record
	Conflicts   []dnspod.Record
	Replacement *dnsReplacement
}

type dnsManaged struct {
	Base                bool          `json:"base,omitempty"`
	ValidationAccountID string        `json:"validationAccountId,omitempty"`
	ValidationHost      string        `json:"validationHost,omitempty"`
	Host                string        `json:"host"`
	Zone                string        `json:"zone"`
	AccountID           string        `json:"accountId"`
	Action              string        `json:"action,omitempty"`
	Record              dnspod.Record `json:"record"`
}

type dnsAttempt struct {
	Base        bool            `json:"base,omitempty"`
	Host        string          `json:"host"`
	Zone        string          `json:"zone"`
	AccountID   string          `json:"accountId"`
	Action      string          `json:"action"`
	Revision    int64           `json:"revision"`
	StartedAt   string          `json:"startedAt"`
	Desired     dnspod.Record   `json:"desired"`
	Replacement *dnsReplacement `json:"replacement,omitempty"`
}

// The journal is written before a cloud mutation. A crash or lost response can
// therefore never trigger an automatic second write on restart.
type dnsJournal struct {
	Attempts map[string]*dnsAttempt `json:"attempts,omitempty"`
	Managed  map[string]dnsManaged  `json:"managed"`
	Attempt  *dnsAttempt            `json:"attempt,omitempty"`
}

// dnsPreview is a single-use authorisation for exactly one cloud change. It is
// bound to the credentials, the configuration revision and the remote snapshot
// observed when it was produced.
type dnsPreview struct {
	Host               string        `json:"host"`
	AccountID          string        `json:"accountId"`
	Zone               string        `json:"zone"`
	SubDomain          string        `json:"subDomain"`
	Type               string        `json:"type"`
	Value              string        `json:"value"`
	TTL                int           `json:"ttl"`
	Action             string        `json:"action"`
	Previous           *dnsPrevious  `json:"previous"`
	Revision           int64         `json:"revision"`
	Token              string        `json:"token"`
	Warnings           []string      `json:"warnings"`
	Conflicts          []dnsConflict `json:"conflicts,omitempty"`
	RequiresOverwrite  bool          `json:"requiresOverwrite"`
	Recovery           *dnsRecovery  `json:"recovery,omitempty"`
	confirmedOverwrite bool
	resumeAttempt      *dnsAttempt
	expires            time.Time
	credentialsHash    [32]byte
	snapshot           []dnspod.Record
}

// dnsManagedView is what the provider currently holds for one name.
type dnsManagedView struct {
	Zone     string `json:"zone"`
	RecordID string `json:"recordId"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	TTL      int    `json:"ttl"`
	Action   string `json:"action,omitempty"`
	Pending  bool   `json:"pending"`
}

// dnsRecordView is one name the gateway knows about, plus what the provider
// currently holds for it. The two are kept side by side and never merged: "what
// I want" and "what is actually published" are different facts, and the operator
// needs both.
//
// A name appears here if a service claims it, or if the journal says it is
// published. The second case is what makes withdrawal possible: dropping a
// domain from a service would otherwise remove the only handle on a record that
// is still live at the provider.
type dnsRecordView struct {
	PublicationState      string `json:"publicationState"`
	Host                  string `json:"host"`
	ServiceID             string `json:"serviceId,omitempty"`
	ServiceName           string `json:"serviceName,omitempty"`
	Enabled               bool   `json:"enabled"`
	AccountID             string `json:"accountId,omitempty"`
	Provider              string `json:"provider,omitempty"`
	Zone                  string `json:"zone,omitempty"`
	EdgeProvider          string `json:"edgeProvider,omitempty"`
	AccountChoiceRequired bool   `json:"accountChoiceRequired"`
	// Base and Edge tell the interface which of the three kinds of name this is.
	// Both are read-only descriptions: they change no behaviour here.
	Base    bool            `json:"base,omitempty"`
	Edge    bool            `json:"edge,omitempty"`
	Type    string          `json:"type"`
	Value   string          `json:"value"`
	Status  string          `json:"status"`
	Message string          `json:"message,omitempty"`
	Managed *dnsManagedView `json:"managed,omitempty"`
}

func (a *App) dnsPath(name string) string { return filepath.Join(a.opts.DataDir, name) }

func readDNSJSON(path string, value any) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	limit := int64(64 << 10)
	switch filepath.Base(path) {
	case "dns-managed.json", "dns-verification.json", "cloudflare-ledger.json":
		limit = cloudLedgerLimit
	case "cloudflare-http-validations.json":
		limit = 4 << 20
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return errors.New("invalid DNS data file")
	}
	// Large legacy JSON is decoded as a bounded stream. The next successful
	// save migrates it to immutable shards without truncating the old source.
	if limit == cloudLedgerLimit && st.Size() > cloudLedgerShardSize {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		decoder := json.NewDecoder(io.LimitReader(f, limit+1))
		if err := decoder.Decode(value); err != nil {
			return errors.New("invalid DNS data file")
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return errors.New("invalid trailing DNS data")
		}
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if limit == cloudLedgerLimit {
		b, err = materializeCloudLedger(path, b)
		if err != nil {
			return err
		}
	}
	if json.Unmarshal(b, value) != nil {
		return errors.New("invalid DNS data file")
	}
	return nil
}

func (a *App) accountCredentialPath(id string) string {
	return filepath.Join(a.opts.DataDir, "dns-accounts", credentialFilename(id, a.accountByID(id).CredentialFile))
}

func (a *App) accountCredentials(id string) (dnsprovider.Credentials, error) {
	var c dnsprovider.Credentials
	err := readDNSJSON(a.accountCredentialPath(id), &c)
	account := a.accountByID(id)
	if account.Provider != "" && c.Kind() != account.Provider {
		return c, errors.New("DNS credential provider does not match account")
	}
	if err == nil && !c.Valid() {
		err = errors.New("invalid DNS credentials")
	}
	return c, err
}

func (a *App) readDNSJournal() (dnsJournal, error) {
	j := dnsJournal{Managed: map[string]dnsManaged{}}
	err := readDNSJSON(a.dnsPath("dns-managed.json"), &j)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err == nil && j.Managed == nil {
		err = errors.New("invalid DNS journal")
	}
	for key, v := range j.Attempts {
		if v == nil || key != dnsAttemptKey(v.Host, v.AccountID, v.Zone) {
			return j, errors.New("invalid scoped DNS attempt")
		}
	}
	promoteDNSAttempt(&j)
	return j, err
}

func (a *App) saveDNSJournal(j dnsJournal) error {
	return saveCloudLedger(a.dnsPath("dns-managed.json"), j)
}

func (a *App) dnsAPI(c dnsprovider.Credentials) dnsprovider.Client {
	if a.dnsClient != nil {
		return dnsprovider.WrapLegacy(a.dnsClient)
	}
	return dnsprovider.New(c)
}

// credentialHint is the masked form of a SecretId shown in the interface. It
// delegates to the shared credentials type so that the DNS account card, the
// EdgeOne account card and the credential file all agree on what a hint is.
func credentialHint(secretID string) string {
	return dnspod.Credentials{SecretID: secretID}.Hint()
}

func (a *App) accountIndex(id string) int {
	for i, account := range a.state.DNSAccounts {
		if account.ID == id {
			return i
		}
	}
	return -1
}

// labelInZone splits a fully qualified name into the sub-domain label the
// provider's API wants and the zone that owns it.
func labelInZone(host, zone string) (string, error) {
	host, zone = strings.ToLower(host), strings.ToLower(zone)
	if host == zone {
		return "@", nil
	}
	if strings.HasSuffix(host, "."+zone) {
		return strings.TrimSuffix(host, "."+zone), nil
	}
	return "", fmt.Errorf("域名 %s 不属于托管域 %s", host, zone)
}

// zoneEntry is one domain an account can manage, cached so that zone discovery
// is not a cloud call per record.
type zoneEntry struct {
	AccountID string
	Zone      string
}

// zoneTableLocked lists every zone every enabled account can manage. Callers
// hold mu. A failure to read one account does not hide the others, so an explicit
// choice that is still verifiable remains usable. Automatic choice, however,
// requires a complete result to avoid silently overlooking a competing account.
func (a *App) zoneTableLocked(ctx context.Context, refresh bool) ([]zoneEntry, []string, error) {
	if !refresh && len(a.zoneMemo) > 0 && time.Since(a.zoneMemoAt) < 30*time.Minute {
		return a.zoneMemo, nil, nil
	}
	entries, warnings, err := a.discoverDNSZonesLocked(ctx)
	// A partial listing cannot establish that a host has only one owner. Never
	// promote one account's outage into a cached, apparently complete result.
	if err != nil || len(warnings) > 0 {
		a.zoneMemo = nil
		return entries, warnings, err
	}
	a.zoneMemo, a.zoneMemoAt = entries, time.Now()
	return entries, warnings, nil
}

// discoverDNSZonesLocked reads provider metadata without changing any local
// state. Callers hold mu; account-choice GETs deliberately use this, not cache.
func (a *App) discoverDNSZonesLocked(ctx context.Context) ([]zoneEntry, []string, error) {
	entries := []zoneEntry{}
	warnings := []string{}
	usable := 0
	var readFailures []error
	permanentFailure := false
	for _, account := range a.state.DNSAccounts {
		if !account.Enabled {
			continue
		}
		credentials, err := a.accountCredentials(account.ID)
		if err != nil {
			warnings = append(warnings, "DNS 账号 "+account.Name+" 没有可用凭据，已跳过")
			permanentFailure = true
			continue
		}
		zones, err := a.dnsAPI(credentials).Zones(ctx)
		if err != nil {
			warnings = append(warnings, "读取 DNS 账号 "+account.Name+" 的域名列表失败："+err.Error())
			if cfRetryableRead(err) {
				readFailures = append(readFailures, err)
			} else {
				permanentFailure = true
			}
			continue
		}
		usable++
		for _, zone := range zones {
			zone = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zone), "."))
			entries = append(entries, zoneEntry{AccountID: account.ID, Zone: zone})
		}
	}
	if usable == 0 && len(a.state.DNSAccounts) > 0 {
		if !permanentFailure && len(readFailures) > 0 {
			return nil, warnings, fmt.Errorf("DNS 域名列表暂时不可用：%w", errors.Join(readFailures...))
		}
		return nil, warnings, errors.New("没有任何 DNS 账号可用：请检查凭据与网络")
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Zone == entries[j].Zone {
			return entries[i].AccountID < entries[j].AccountID
		}
		return entries[i].Zone < entries[j].Zone
	})
	if !permanentFailure && len(readFailures) > 0 {
		return entries, warnings, fmt.Errorf("DNS 域名列表读取不完整：%w", errors.Join(readFailures...))
	}
	return entries, warnings, nil
}

// resolveZone picks the account and the longest managed zone that contains host.
// Longest wins so that a delegated sub-zone is used instead of its parent.
func (a *App) resolveZone(ctx context.Context, host string, refresh bool) (model.DNSAccount, string, string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if account, zone, sub, known, err := a.resolveKnownDNSZone(ctx, host, refresh); known {
		return account, zone, sub, err
	}
	entries, warnings, err := a.zoneTableLocked(ctx, refresh)
	best := zoneEntry{}
	if err != nil {
		return model.DNSAccount{}, "", "", err
	}
	if len(warnings) > 0 {
		return model.DNSAccount{}, "", "", errors.New("部分 DNS 账号读取失败，无法确定解析账号，请先检查凭据与网络（" + strings.Join(warnings, "；") + "）")
	}
	ambiguous := false
	for _, entry := range entries {
		if host == entry.Zone || strings.HasSuffix(host, "."+entry.Zone) {
			if len(entry.Zone) > len(best.Zone) {
				best = entry
				ambiguous = false
			} else if entry.Zone == best.Zone && entry.AccountID != best.AccountID {
				ambiguous = true
			}
		}
	}
	if ambiguous {
		return model.DNSAccount{}, "", "", &dnsAccountChoiceError{message: "多个 DNS 账号可以管理该托管域，请在该记录旁选择解析账号；不会自动选择账号"}
	}
	if best.Zone == "" {
		// A newly added domain is the common reason for a miss, so the table is
		// rebuilt once before giving up.
		if !refresh {
			return a.resolveZone(ctx, host, true)
		}
		message := fmt.Sprintf("域名 %s 不属于任何 DNS 账号管理的域名，请在实际托管商添加该域名或配置正确账号", host)
		if len(warnings) > 0 {
			message += "（" + strings.Join(warnings, "；") + "）"
		}
		return model.DNSAccount{}, "", "", errors.New(message)
	}
	index := a.accountIndex(best.AccountID)
	if index < 0 {
		return model.DNSAccount{}, "", "", errors.New("该域名对应的 DNS 账号已被删除")
	}
	sub, err := labelInZone(host, best.Zone)
	if err != nil {
		return model.DNSAccount{}, "", "", err
	}
	return a.state.DNSAccounts[index], best.Zone, sub, nil
}

// appliedPublicAddressLocked is the address names should point at, together with
// proof that the proxy is really serving it. Everything here reads the applied
// snapshot, not the draft. Callers hold mu.
func (a *App) appliedPublicAddressLocked() (string, error) {
	if a.state.AppliedAt == "" || a.state.AppliedRevision < 1 {
		return "", errors.New("还没有应用过任何配置，请先应用配置，再发布解析")
	}
	applied, err := a.appliedSnapshotLocked()
	if err != nil {
		return "", err
	}
	// What has to hold is that the draft is not asking the proxy to serve
	// something else — not that the two revision numbers are equal. See
	// servedUnapplied for why the difference decides whether an operator can
	// publish at all.
	if servedUnapplied(a.state, applied) {
		return "", errors.New("请先应用当前配置草稿，再发布解析；不能为未应用的地址发布解析")
	}
	address := applied.AppliedPublicAddress
	if address == "" {
		return "", errors.New("已应用快照没有公网地址，请重新应用配置")
	}
	if !a.opts.DryRun {
		if a.engine == nil || a.engine.VerifyApplied(applied, address) != nil {
			return "", errors.New("受管代理未运行在已应用配置上，请先重新应用")
		}
	}
	return address, nil
}

// addressRecordType picks the record family for an address. The value decides,
// not a preference: an A record holding an IPv6 address (or the reverse) is
// accepted by nobody.
func addressRecordType(address string) (string, error) {
	if engine.ValidPublicIPv4(address) {
		return dnspod.TypeA, nil
	}
	if engine.ValidatePublicAddress(address) == nil {
		return dnspod.TypeAAAA, nil
	}
	return "", fmt.Errorf("地址 %s 不是可用的公网地址", address)
}

// serviceForHost finds the service that claims a name, and whether that claim is
// currently wanted. Callers hold mu.
func (a *App) serviceForHost(host string) (model.Service, bool) {
	host = strings.ToLower(host)
	var disabled model.Service
	claimed := false
	for _, service := range a.state.Services {
		for _, domain := range service.Domains {
			if strings.ToLower(domain) == host {
				if service.Enabled {
					return service, true
				}
				if !claimed {
					disabled, claimed = service, true
				}
			}
		}
	}
	return disabled, claimed
}

// baseDomain is the configured base station name, canonicalised. Empty means
// every service answers with its own address.
func (a *App) baseDomain() string {
	return strings.ToLower(strings.TrimSpace(a.state.Settings.BaseDomain))
}

// desiredForBaseLocked resolves the base station's own record. Nothing else in
// this file would ever produce it: it is a name no service claims, published by
// the gateway on its own behalf, and it is the only record whose value is an
// address that moves.
//
// Callers hold mu.
func (a *App) desiredForBaseLocked(ctx context.Context, host string, refresh bool) (dnsDesired, error) {
	var empty dnsDesired
	desired := dnsDesired{Host: host, Base: true, TTL: a.state.Settings.EffectiveBaseTTL()}
	account, zone, sub, err := a.resolveZone(ctx, host, refresh)
	if err != nil {
		return empty, err
	}
	desired.Account, desired.Zone, desired.Sub = account, zone, sub
	address, err := a.appliedPublicAddressLocked()
	if err != nil {
		return empty, err
	}
	recordType, err := addressRecordType(address)
	if err != nil {
		return empty, err
	}
	desired.Enabled, desired.Type, desired.Value = true, recordType, address
	desired.Revision = a.state.AppliedRevision
	return desired, nil
}

// desiredForHostLocked resolves one name to the record the gateway wants at the
// provider. Callers hold mu.
func (a *App) desiredForHostLocked(ctx context.Context, host string, refresh bool) (dnsDesired, error) {
	var empty dnsDesired
	host = strings.ToLower(strings.TrimSpace(host))
	if blocked, err := a.deletionBlocksHost(host); err != nil {
		return empty, err
	} else if blocked {
		return empty, errors.New("该服务已有删除保护记录，已暂停解析发布；请保持原删除选项完成删除")
	}
	if !engine.ValidDomain(host) || !strings.Contains(host, ".") {
		return empty, errors.New("域名格式不正确")
	}
	if host == a.baseDomain() {
		if _, claimed := a.serviceForHost(host); claimed {
			return empty, errors.New("基站域名不能同时是某个服务的域名，请先修改冲突的服务")
		}
		return a.desiredForBaseLocked(ctx, host, refresh)
	}
	desired := dnsDesired{Host: host, TTL: dnspod.DefaultTTL}

	service, claimed := a.serviceForHost(host)
	if !claimed {
		// Not claimed by any service. It is only addressable at all because it is
		// still published; refusing here would leave a live record with no way to
		// withdraw it from the interface.
		journal, err := a.readDNSJournal()
		if err != nil {
			return empty, errors.New("无法读取 DNS 托管记录")
		}
		entry, published := journal.Managed[host]
		if !published {
			return empty, errors.New("没有服务使用该域名")
		}
		desired.Enabled = false
		desired.Account = a.accountByID(entry.AccountID)
		desired.Zone = entry.Zone
		sub, err := labelInZone(host, entry.Zone)
		if err != nil {
			return empty, err
		}
		desired.Sub = sub
		desired.Type = entry.Record.Type
		return desired, nil
	}

	desired.ServiceID = service.ID
	desired.Enabled = service.Enabled
	// The name has to be one the certificate actually speaks for, or the address
	// it resolves to would answer with the wrong certificate. This holds whether
	// the client reaches EdgeOne or this device directly: when EdgeOne fetches
	// from here it presents this name, so this device still has to hold a
	// certificate that speaks for it.
	if service.Enabled {
		index := a.certIndex(service.CertificateID)
		if index < 0 || !a.state.Certificates[index].Ready {
			return empty, errors.New("该服务尚未导入可用证书，请先导入证书再发布解析")
		}
		if err := engine.CertificateCoversAll(a.state.Certificates[index], []string{host}); err != nil {
			return empty, err
		}
	}

	if service.ServesCloudflare() {
		if !service.Enabled {
			journal, err := a.readDNSJournal()
			if err != nil {
				return empty, errors.New("无法读取 DNS 托管记录")
			}
			entry, owned := journal.Managed[host]
			desired.Revision = a.state.AppliedRevision
			if !owned {
				desired.External = true
				return desired, nil
			}
			desired.Account, desired.Zone, desired.Type = a.accountByID(entry.AccountID), entry.Zone, entry.Record.Type
			desired.Sub, err = labelInZone(host, entry.Zone)
			if err != nil {
				return empty, err
			}
			return desired, nil
		}
		target, err := a.cloudflareTargetForHostLocked(host)
		if err != nil {
			return empty, err
		}
		// A remembered grant never substitutes for a fresh read before cutting
		// traffic over. This is read-only; only explicit CF reconciliation grants.
		ready, err := a.cfReconcileLocked(ctx, host, false, true)
		if err != nil {
			return empty, err
		}
		if !ready.Ready || ready.Target != target {
			return empty, errors.New("Cloudflare is not currently ready; DNS was not changed")
		}
		account, zone, sub, err := a.resolveZone(ctx, host, refresh)
		if err != nil {
			return empty, err
		}
		desired.Account, desired.Zone, desired.Sub = account, zone, sub
		desired.Type, desired.Value = dnspod.TypeCNAME, target
		desired.Revision = a.state.AppliedRevision
		return desired, nil
	}
	if service.ServesEdge() {
		// EdgeOne answers for this name. What the record must say is EdgeOne's
		// answer, not this device's address — and if this gateway cannot see
		// EdgeOne, it has no business guessing, so the name is released instead.
		target, err := a.edgeTargetForHost(ctx, host, refresh)
		if err != nil {
			return empty, err
		}
		if target == "" {
			desired.External = true
			// Carried so that the handover token is bound to the same snapshot
			// revision every other token is. Without it the token would be bound to
			// zero, and the apply path's check that it was previewed against this
			// configuration would be comparing two constants.
			desired.Revision = a.state.AppliedRevision
			return desired, nil
		}
		account, zone, sub, err := a.resolveZone(ctx, host, refresh)
		if err != nil {
			return empty, err
		}
		desired.Account, desired.Zone, desired.Sub = account, zone, sub
		if !desired.Enabled {
			desired.Type = dnspod.TypeCNAME
			return desired, nil
		}
		desired.Type, desired.Value = dnspod.TypeCNAME, target
		desired.Revision = a.state.AppliedRevision
		return desired, nil
	}

	account, zone, sub, err := a.resolveZone(ctx, host, refresh)
	if err != nil {
		return empty, err
	}
	desired.Account, desired.Zone, desired.Sub = account, zone, sub
	if !desired.Enabled {
		// Withdrawing a name needs only the name. Demanding that the entry be
		// healthy first would make cleanup impossible after an outage — exactly
		// the moment the operator most wants the name gone.
		desired.Type = dnspod.TypeAAAA
		return desired, nil
	}
	address, err := a.appliedPublicAddressLocked()
	if err != nil {
		return empty, err
	}
	// With a base station configured, this name stops naming an address at all
	// and starts naming the base station. That is the whole point of it: the
	// address then has to be republished in exactly one place.
	if base := a.baseDomain(); base != "" {
		desired.Type, desired.Value = dnspod.TypeCNAME, base
		desired.Revision = a.state.AppliedRevision
		return desired, nil
	}
	recordType, err := addressRecordType(address)
	if err != nil {
		return empty, err
	}
	desired.Type, desired.Value = recordType, address
	desired.Revision = a.state.AppliedRevision
	return desired, nil
}

func (a *App) accountByID(id string) model.DNSAccount {
	if index := a.accountIndex(id); index >= 0 {
		return a.state.DNSAccounts[index]
	}
	return model.DNSAccount{}
}

func dnsAddressRecords(records []dnspod.Record) []dnspod.Record {
	result := []dnspod.Record{}
	for _, record := range records {
		switch record.Type {
		case dnspod.TypeA, dnspod.TypeAAAA, dnspod.TypeCNAME:
			result = append(result, record)
		}
	}
	return result
}

func sameDNSRecord(a, b dnspod.Record) bool {
	// The API supplies the default line ID even though creation uses its name.
	// A different line remains a conflict; both ID and display name are checked.
	return a.Key() == b.Key() && a.Proxied == b.Proxied && strings.EqualFold(a.Name, b.Name) && a.Type == b.Type && sameRecordValue(a.Type, a.Value, b.Value) && a.TTL == b.TTL && a.Status == b.Status && a.Line == b.Line && a.LineID == b.LineID && a.Remark == b.Remark && a.Weight == b.Weight
}

// sameRecordValue compares the published value, tolerating the case and trailing
// dot a provider may add to a CNAME target.
func sameRecordValue(recordType, a, b string) bool {
	if recordType == dnspod.TypeCNAME {
		return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
	}
	if recordType == dnspod.TypeAAAA {
		left, leftErr := netip.ParseAddr(a)
		right, rightErr := netip.ParseAddr(b)
		return leftErr == nil && rightErr == nil && left == right
	}
	return a == b
}

// recordMatchesDesired reports whether what the provider holds is already the
// record the gateway wants. TTL is excluded on purpose: it is the one field the
// gateway deliberately changes on a record it otherwise agrees with, and
// treating a TTL difference as a conflict would make the base station unwritable
// the moment the operator shortened it.
func recordMatchesDesired(record dnspod.Record, d dnsDesired) bool {
	return !record.Proxied && record.Line == dnspod.DefaultLine && record.LineID == "0" && record.Weight == 0 && record.Type == d.Type && record.Status == "ENABLE" && sameRecordValue(d.Type, record.Value, d.Value)
}

// planDNS compares what the gateway wants with what the provider holds. It
// refuses to touch a name it does not already own, and refuses to act at all
// when the remote state has been changed by someone else.
func planDNS(j dnsJournal, d dnsDesired, records []dnspod.Record) (dnsPlan, error) {
	relevant := dnsAddressRecords(records)
	if d.Enabled && !d.External && d.Account.Provider == "cloudflare" {
		for _, r := range records {
			if r.Proxied {
				return dnsPlan{}, errors.New("该名称已有 Cloudflare 橙云代理记录；业务入口和基站必须使用 DNS only，不会自动覆盖代理，请在解析页预览变动后手动强制覆盖")
			}
		}
	}
	key := strings.ToLower(d.Host)
	managed, owned := j.Managed[key]

	if d.External {
		// Nothing here reads the provider, because nothing here touches it. A name
		// the gateway has decided not to own is let go of in its own ledger only;
		// the record out there belongs to whatever now answers for the name, and
		// deleting it would break the very thing being handed over.
		if !owned {
			return dnsPlan{Action: dnsNoop}, nil
		}
		return dnsPlan{Action: dnsRelease}, nil
	}

	if !d.Enabled {
		// Disabling is how a record is withdrawn. Nothing published means there is
		// nothing to withdraw, and that is a success, not an error.
		if !owned {
			return dnsPlan{Action: dnsNoop}, nil
		}
		if managed.Zone == d.Zone && managed.AccountID == d.Account.ID && len(relevant) == 0 {
			return dnsPlan{Action: dnsRelease}, nil
		}
		if managed.Zone != d.Zone || managed.AccountID != d.Account.ID || len(relevant) != 1 || !sameDNSRecord(relevant[0], managed.Record) {
			return dnsPlan{}, errors.New("网关托管的 DNS 记录被修改、移除或存在冲突，已停止写入；请在 DNS 托管商控制台核对")
		}
		old := relevant[0]
		return dnsPlan{Action: dnsDelete, Previous: &dnsPrevious{RecordID: old.ID, OpaqueID: old.OpaqueID, Type: old.Type, Value: old.Value, TTL: old.TTL}}, nil
	}

	if !owned {
		if d.Type == dnspod.TypeCNAME && len(records) > 0 {
			return dnsPlan{}, errors.New("该名称已有记录，CNAME 不能与其他类型共存；自动解析不会覆盖，请在解析页预览变动并处理冲突")
		}
		if len(relevant) != 0 {
			return dnsPlan{}, errors.New("该名称已有 A / AAAA / CNAME 记录，自动解析不会覆盖；请在解析页预览变动后手动强制覆盖")
		}
		return dnsPlan{Action: dnsCreate}, nil
	}
	if d.Type == dnspod.TypeCNAME && len(records) != len(relevant) {
		return dnsPlan{}, errors.New("该名称还有其他类型记录，不能转换为 CNAME；请先核对现有记录")
	}
	if managed.Zone != d.Zone || managed.AccountID != d.Account.ID {
		return dnsPlan{}, errors.New("网关托管的 DNS 记录被修改、移除或存在冲突，已停止写入；请在 DNS 托管商控制台核对")
	}
	// A stale ledger is not a cloud conflict. An absent record must be created
	// with a new ID, not updated using the deleted provider ID.
	if len(relevant) == 0 {
		return dnsPlan{Action: dnsCreate}, nil
	}
	if len(relevant) == 1 && !sameDNSRecord(relevant[0], managed.Record) {
		// The ledger and the provider disagree, which normally stops everything:
		// the record this gateway created has been changed by something else. But
		// when what is out there is already exactly what the gateway wants, the
		// only thing that is wrong is the ledger.
		//
		// Adopting writes nothing, so it cannot overwrite anything. Refusing
		// instead would leave an operator who deliberately moved a name onto
		// another system — or who pointed it at this device by hand while the
		// gateway was off — staring at a conflict the interface gives them no way
		// to clear, and it would stop every name queued behind this one.
		if !recordMatchesDesired(relevant[0], d) {
			return dnsPlan{}, errors.New("网关托管的 DNS 记录被修改、移除或存在冲突，已停止写入；请在 DNS 托管商控制台核对")
		}
		record := relevant[0]
		previous := &dnsPrevious{RecordID: record.ID, OpaqueID: record.OpaqueID, Type: record.Type, Value: record.Value, TTL: record.TTL}
		if record.TTL == d.TTL {
			return dnsPlan{Action: dnsAdopt, Adopt: &record, Previous: previous}, nil
		}
		return dnsPlan{Action: dnsUpdate, Previous: previous}, nil
	}
	if len(relevant) != 1 {
		return dnsPlan{}, errors.New("网关托管的 DNS 记录被修改、移除或存在冲突，已停止写入；请在 DNS 托管商控制台核对")
	}
	old := relevant[0]
	previous := &dnsPrevious{RecordID: old.ID, OpaqueID: old.OpaqueID, Type: old.Type, Value: old.Value, TTL: old.TTL}
	if old.Type == d.Type && old.TTL == d.TTL && sameRecordValue(d.Type, old.Value, d.Value) {
		return dnsPlan{Action: dnsNoop, Previous: previous}, nil
	}
	return dnsPlan{Action: dnsUpdate, Previous: previous}, nil
}

func dnsRandomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// dnsRefusal marks a decision that stopped before anything was sent to the
// provider. The distinction matters to the operator: a refusal means the cloud
// is untouched and the fix is local, while a provider failure means the remote
// state has to be checked by hand.
type dnsRefusal struct{ err error }

func (e dnsRefusal) Error() string { return e.err.Error() }
func (e dnsRefusal) Unwrap() error { return e.err }

func refuse(format string, args ...any) error { return dnsRefusal{fmt.Errorf(format, args...)} }

// Recovery is deliberately read-only. A unique marker plus the full requested
// value proves a timed-out mutation completed; absence never proves it failed.
func (a *App) reconcileDNS(j *dnsJournal, records []dnspod.Record, d dnsDesired) error {
	selectDNSAttempt(j, d.Host, d.Account.ID, d.Zone)
	if j.Attempt == nil {
		return nil
	}
	attempt := j.Attempt
	if attempt.Replacement != nil || attempt.Action == dnsReplace {
		return a.reconcileDNSReplacement(j, records)
	}
	if attempt.Host == strings.ToLower(d.Host) && attempt.Zone == d.Zone {
		relevant := dnsAddressRecords(records)
		if attempt.Action == dnsDelete && len(relevant) == 0 {
			// The record is gone, which is exactly what the delete asked for.
			delete(j.Managed, attempt.Host)
			j.Attempt = nil
			if err := a.saveDNSJournal(*j); err != nil {
				return errors.New("云端结果已核对，但本地记录保存失败；已停止继续写入")
			}
			return nil
		}
		if len(relevant) == 1 {
			got, expected := relevant[0], attempt.Desired
			if got.Key() != "" && (expected.Key() == "" || expected.Key() == got.Key()) && got.LineID == "0" {
				expected.ID, expected.OpaqueID, expected.LineID = got.ID, got.OpaqueID, got.LineID
				if sameDNSRecord(got, expected) {
					j.Managed[attempt.Host] = dnsManaged{Host: attempt.Host, Zone: attempt.Zone, AccountID: attempt.AccountID, Action: attempt.Action, Record: got, Base: attempt.Base}
					j.Attempt = nil
					if err := a.saveDNSJournal(*j); err != nil {
						return errors.New("云端结果已核对，但本地记录保存失败；已停止继续写入")
					}
					return nil
				}
			}
		}
	}
	return errors.New("上次 DNS 写入结果尚不明确，已阻止重复写入；请先在 DNS 托管商控制台核对。再次预览只会只读核对，不会重发写入")
}

// releaseDNS drops the gateway's own claim on a name without touching the
// provider. It is the only mutation that needs no credentials, no zone and no
// cloud call — and it must stay that way: the record out there belongs to
// whatever now answers for the name, and deleting it would break the very thing
// being handed over.
//
// Callers hold both dnsMu and mu.
func (a *App) releaseDNS(j *dnsJournal, d dnsDesired) error {
	if err := a.dnsMigrationBlocksHost(d.Host); err != nil {
		return err
	}
	if dnsUnknownForHost(*j, d.Host) {
		return errors.New("上次 DNS 写入结果待核对，暂不能解除托管；请先重新预览待核对的域名")
	}
	delete(j.Managed, strings.ToLower(d.Host))
	if err := a.saveDNSJournal(*j); err != nil {
		return errors.New("无法保存 DNS 托管记录")
	}
	return nil
}

// dnsRefusalText turns a proven rejection into the sentence an operator can act
// on, and falls back to the raw code when it has nothing to add.
//
// A code is not an instruction. DNSPod's TTL floor is a property of the zone's
// plan, which no API states, so the account answers a too-short TTL with
// LimitExceeded.RecordTtlLimit and nothing else — an operator reading that learns
// neither the floor nor the number to type instead, and has to go and find
// DNSPod's plan table. Naming the change is the whole difference between a
// refusal that is actionable and one that is merely legible.
//
// Only codes this gateway has actually been refused with are translated, each
// with the failure it was observed in. Inventing explanations for codes nobody
// has seen would send the operator to edit the wrong thing, which is worse than
// showing them a code they have to look up.
//
// The TTL advice names the base station because it is the only record whose TTL
// the operator chooses: every other record is written with DefaultTTL, so a TTL
// refusal cannot come from one. A record with no operator-chosen TTL (a delete)
// passes ttl 0 and gets the generic sentence.
func dnsRefusalText(err error, verb string, ttl int) string {
	if dnspod.Code(err) == "LimitExceeded.RecordTtlLimit" && ttl > 0 {
		return fmt.Sprintf("DNSPod 拒绝写入：这个域名的套餐不接受 TTL %d 秒。60 和 300 需要域名套餐支持，"+
			"600 是每个套餐都接受的值——请把基站 TTL 改成 600 秒后重新预览。", ttl)
	}
	// The code stays visible, so an untranslated refusal is still something the
	// operator can look up rather than a failure with no identifying detail.
	return "DNS 托管商已拒绝此次" + verb + "，可修正后重新预览：" + err.Error()
}

// publish is the only place in the program that calls a provider write. It keeps
// the write-ahead discipline: the intent is durable before the call, and the
// outcome is recorded (or explicitly left unknown) after it.
//
// Two of the four actions reach the journal without reaching the provider.
// Releasing a name and adopting a record that already matches both change only
// what the gateway believes about records it did not write, and both are
// therefore incapable of damaging anything.
//
// Callers hold both dnsMu and mu.
func (a *App) publish(ctx context.Context, client dnsprovider.Client, j *dnsJournal, d dnsDesired, plan dnsPlan) error {
	if err := a.dnsMigrationBlocksHost(d.Host); err != nil {
		return err
	}
	selectDNSAttempt(j, d.Host, d.Account.ID, d.Zone)
	if j.Attempt != nil {
		return errors.New("original DNS operation remains unknown; inspect before any new mutation")
	}
	if plan.Action == dnsCreate || plan.Action == dnsUpdate || plan.Action == dnsDelete {
		if err := a.authorizeDNSWrite(d.Account.ID); err != nil {
			return err
		}
		if err := reserveCloudLedger(*j); err != nil {
			return err
		}
	}
	key := strings.ToLower(d.Host)
	attempt := func(action string, desired dnspod.Record) error {
		j.Attempt = &dnsAttempt{Host: key, Zone: d.Zone, AccountID: d.Account.ID, Base: d.Base,
			Action: action, Revision: d.Revision, StartedAt: time.Now().UTC().Format(time.RFC3339), Desired: desired, Replacement: plan.Replacement}
		if err := reserveCloudLedger(*j); err != nil {
			return err
		}
		return a.saveDNSJournal(*j)
	}
	switch plan.Action {
	case dnsRelease:
		return a.releaseDNS(j, d)
	case dnsAdopt:
		j.Managed[key] = dnsManaged{Host: key, Zone: d.Zone, AccountID: d.Account.ID, Action: plan.Action, Record: *plan.Adopt, Base: d.Base}
		j.Attempt = nil
		if err := a.saveDNSJournal(*j); err != nil {
			return errors.New("云端记录与预期一致，但本地托管记录保存失败；请重新预览只读核对")
		}
		return nil
	}
	if plan.Action == dnsDelete {
		// Removing a record is a mutation like any other, so it is journalled
		// before the call. Without that, a lost response would leave the operator
		// unable to tell a completed delete from one that never happened.
		if err := attempt(dnsDelete, dnspod.Record{ID: plan.Previous.RecordID, OpaqueID: plan.Previous.OpaqueID, Name: d.Sub, Type: plan.Previous.Type, Value: plan.Previous.Value, TTL: plan.Previous.TTL}); err != nil {
			return refuse("无法保存 DNS 写入保护记录，未调用云端")
		}
		if err := client.Delete(ctx, d.Zone, plan.Previous.Key()); err != nil {
			if dnsprovider.Rejected(err) {
				j.Attempt = nil
				if a.saveDNSJournal(*j) == nil {
					return errors.New(dnsRefusalText(err, "删除", 0))
				}
			}
			return errors.New("DNS 删除结果不明确，已保存保护记录并禁止自动重试；请重新预览只读核对云端结果")
		}
		delete(j.Managed, key)
		j.Attempt = nil
		if err := a.saveDNSJournal(*j); err != nil {
			return errors.New("云端已删除记录，但本地托管记录保存失败；请重新预览只读核对")
		}
		return nil
	}
	marker, err := dnsRandomToken()
	if err != nil {
		return refuse("无法创建写入标识，未调用云端")
	}
	desired := dnspod.Record{
		Name: d.Sub, Type: d.Type, Value: d.Value, TTL: d.TTL,
		Line: dnspod.DefaultLine, LineID: "0", Status: "ENABLE", Remark: "W103D Gateway:" + marker,
	}
	if plan.Previous != nil {
		desired.ID, desired.OpaqueID = plan.Previous.RecordID, plan.Previous.OpaqueID
	}
	// AliDNS cannot write a remark atomically with the record.
	if d.Account.Provider == "alidns" {
		desired.Remark = ""
		if previous, ok := j.Managed[key]; ok && plan.Previous != nil {
			desired.Remark = previous.Record.Remark
		}
	}
	if err := attempt(plan.Action, desired); err != nil {
		return refuse("无法保存 DNS 写入保护记录，未调用云端")
	}
	recordID, err := client.Put(ctx, d.Zone, desired)
	if err == nil && (recordID == "" || len(recordID) > 1024) {
		err = errors.New("provider returned an invalid record ID")
	}
	if err != nil {
		if dnsprovider.Rejected(err) {
			j.Attempt = nil
			if a.saveDNSJournal(*j) == nil {
				return errors.New(dnsRefusalText(err, "写入", desired.TTL))
			}
		}
		if plan.Replacement != nil {
			return a.replacementFailure("新记录提交失败或结果未知")
		}
		return errors.New("DNS 写入结果不明确，已保存保护记录并禁止自动重试；请重新预览只读核对云端结果")
	}
	desired.SetKey(recordID, d.Account.Provider)
	if desired.Type == dnspod.TypeCNAME && (plan.Action == dnsCreate || plan.Action == dnsUpdate) {
		// An accepted mutation is not proof of the provider's resulting record.
		// Persist its exact returned ID before observing it, so a failed read
		// retains the original write intent and can never trigger another Put.
		j.Attempt.Desired = desired
		if err := a.saveDNSJournal(*j); err != nil {
			return errors.New("CNAME 已提交，但写入结果保护记录保存失败；未确认发布成功，请只读核对，不要重复写入")
		}
		verified, err := client.List(ctx, d.Zone, d.Sub)
		if err != nil || len(verified) != 1 || !sameDNSRecord(verified[0], desired) {
			return errors.New("CNAME 已提交，但云端独立读回失败或记录不一致；已保留写入保护，不会自动重复写入")
		}
	}
	j.Managed[key] = dnsManaged{Host: key, Zone: d.Zone, AccountID: d.Account.ID, Action: plan.Action, Record: desired, Base: d.Base}
	j.Attempt = nil
	if err := a.saveDNSJournal(*j); err != nil {
		if plan.Replacement != nil {
			return a.replacementFailure("云端已接受请求，但本地结果保存失败")
		}
		return errors.New("云端已接受请求，但本地托管记录保存失败；请重新预览只读核对，不要重复创建")
	}
	return nil
}

func (a *App) listDNSAccounts(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	accounts := a.state.DNSAccounts
	if accounts == nil {
		accounts = []model.DNSAccount{}
	}
	reply(w, 200, map[string]any{
		"provider": "multiple", "providers": []string{"dnspod", "cloudflare", "alidns"}, "accounts": accounts,
		"autoResolve":  a.state.Settings.AutoResolve,
		"capabilities": dnspod.Supported(),
	})
}

func (a *App) createDNSAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string `json:"name"`
		Provider  string `json:"provider"`
		APIToken  string `json:"apiToken"`
		SecretID  string `json:"secretId"`
		SecretKey string `json:"secretKey"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.SecretID, in.SecretKey = strings.TrimSpace(in.SecretID), strings.TrimSpace(in.SecretKey)
	if in.Name == "" || len(in.Name) > 100 {
		fail(w, 400, "账号名称不能为空且不能超过100字节")
		return
	}
	credentials := dnsprovider.Credentials{Provider: strings.ToLower(strings.TrimSpace(in.Provider)), SecretID: in.SecretID, SecretKey: in.SecretKey, APIToken: strings.TrimSpace(in.APIToken)}
	if !credentials.Valid() {
		fail(w, 400, "请按 DNS 托管商填写有效凭据：DNSPod SecretId/SecretKey、阿里云 AccessKey ID/Secret 或 Cloudflare API Token")
		return
	}
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.state.DNSAccounts) >= maxDNSAccounts {
		fail(w, 400, "DNS 账号数量已达上限")
		return
	}
	// Only prove that the new credentials can list visible zones. Never write a
	// DNS record as a permission probe, and do not claim that zone reads prove
	// record read/write permissions. Keep the global state lock out of cloud I/O.
	revision := a.state.Revision
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	a.mu.Unlock()
	zones, err := a.dnsAPI(credentials).Zones(ctx)
	a.mu.Lock()
	if a.state.Revision != revision {
		fail(w, 409, "凭据核验期间配置已变化，请重新添加解析账号；未保存凭据")
		return
	}
	if len(a.state.DNSAccounts) >= maxDNSAccounts {
		fail(w, 400, "DNS 账号数量已达上限")
		return
	}
	if err != nil || ctx.Err() != nil {
		fail(w, 400, "无法使用该凭据读取托管域列表，请检查凭据、域名读取权限和网络连接；未保存账号或凭据，未修改云端 DNS")
		return
	}
	if len(zones) == 0 {
		fail(w, 400, "未读取到可管理的托管域，请先在托管商添加域名并检查凭据的域名读取权限和授权范围；未保存账号或凭据")
		return
	}
	id := creationResourceID(r)
	if err := writeJSON(a.accountCredentialPath(id), credentials); err != nil {
		fail(w, 500, "无法安全保存 DNS 凭据")
		return
	}
	account := model.DNSAccount{ID: id, Name: in.Name, Provider: credentials.Kind(), SecretIDHint: credentials.Hint(), CredentialVersion: 1, Enabled: true}
	old := clone(a.state)
	a.state.DNSAccounts = append(a.state.DNSAccounts, account)
	a.state.Revision++
	if err := a.saveLocked(); err != nil {
		a.state = old
		_ = os.Remove(a.accountCredentialPath(id))
		fail(w, 500, "无法保存配置")
		return
	}
	a.zoneMemo, a.dnsPending = nil, nil
	a.replyStateLocked(w)
}

func (a *App) deleteDNSAccount(w http.ResponseWriter, r *http.Request) {
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	id := r.PathValue("id")
	index := a.accountIndex(id)
	if index < 0 {
		fail(w, 404, "DNS 账号不存在")
		return
	}
	if used, err := a.dnsMigrationUsesAccount(id); err != nil || used {
		fail(w, 409, "DNS account is protected by retained legacy ownership records; verify them with the DNS provider before removing credentials")
		return
	}
	for _, binding := range a.state.DNSBindings {
		if binding.AccountID == id {
			fail(w, 409, "该账号仍有显式域名归属，请先移除或调整归属绑定")
			return
		}
	}
	deletions, err := a.readServiceDeletions()
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	for _, entry := range deletions.Entries {
		if entry.Completed {
			continue
		}
		for _, target := range entry.DNS {
			if target.Delete && !target.Done && target.Managed.AccountID == id {
				fail(w, 409, "该账号仍被未完成的服务删除操作引用，请先保持原选项完成删除，不能移除核对云端结果所需的凭据")
				return
			}
		}
		for _, target := range entry.TXT {
			if target.Managed.AccountID == id && !target.Done {
				fail(w, 409, "该账号仍被未完成的服务删除验证 TXT 清理引用，请先完成删除核对")
				return
			}
		}
	}
	journal, journalErr := a.readDNSJournal()
	if journalErr != nil {
		fail(w, 409, "无法读取 DNS 保护记录，暂不能删除账号")
		return
	}
	if dnsUnknownUsesAccount(journal, id) {
		fail(w, 409, "该账号仍有结果未知的 DNS 写入，请先核对")
		return
	}
	if err := a.dnsTXTAccountRemovable(id); err != nil {
		fail(w, 409, err.Error())
		return
	}
	{
		for _, entry := range journal.Managed {
			if entry.AccountID == id {
				fail(w, 409, "该账号下仍有本程序发布的记录，请先在解析列表中撤回后再删除账号")
				return
			}
		}
	}
	old := clone(a.state)
	a.state.DNSAccounts = append(a.state.DNSAccounts[:index], a.state.DNSAccounts[index+1:]...)
	a.state.Revision++
	if err := a.saveLocked(); err != nil {
		a.state = old
		fail(w, 500, "无法保存配置")
		return
	}
	a.zoneMemo, a.dnsPending = nil, nil
	// The credential file is removed with the account; cloud records are untouched.
	_ = os.Remove(filepath.Join(a.opts.DataDir, "dns-accounts", credentialFilename(id, old.DNSAccounts[index].CredentialFile)))
	a.replyStateLocked(w)
}

// dnsHostsLocked is every name the interface knows about: the ones services
// claim, the base station the gateway publishes on its own behalf, plus the ones
// the journal says are still published. Callers hold mu.
//
// The base station is included even though no service claims it, because it is
// the one record that has to be writable — an operator who cannot see it here
// has no way to withdraw it.
func (a *App) dnsHostsLocked(j dnsJournal) ([]string, map[string]model.Service) {
	owners := map[string]model.Service{}
	hosts := []string{}
	seen := map[string]bool{}
	for _, service := range a.state.Services {
		for _, domain := range service.Domains {
			host := strings.ToLower(domain)
			// A disabled draft may share a name with its live replacement. DNS
			// must follow the enabled owner, just as the proxy does.
			if previous, ok := owners[host]; !ok || !previous.Enabled && service.Enabled {
				owners[host] = service
			}
			if seen[host] {
				continue
			}
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	// The base station is published by the gateway on its own behalf, so no
	// service puts it in this list. It still has to be here from the moment it is
	// configured: this listing is the only place the interface offers a preview
	// and a submit, so a name missing from it has no way to ever be published.
	// Leaving it out until the journal already held it would have made the whole
	// base station unusable, and unusable in the quietest possible way -- the
	// setting saves, the page shows nothing, and no error is ever raised.
	if base := a.baseDomain(); base != "" && !seen[base] {
		seen[base] = true
		hosts = append(hosts, base)
	}
	for host := range j.Managed {
		if seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts, owners
}

// dnsHostKind says which of the three kinds of name this is, because the
// interface has to explain each differently and the journal alone cannot tell
// them apart. It reads only local state: a listing that had to reach the cloud
// to describe itself would fail to render exactly when the cloud is the problem.
func (a *App) dnsHostKind(host string, owners map[string]model.Service) (base, edge bool) {
	if base := a.baseDomain(); base != "" && host == base {
		return true, false
	}
	service, claimed := owners[host]
	return false, claimed && (service.ServesEdge() || service.ServesCloudflare())
}

// listDNSRecords returns every name together with what the provider currently
// holds for it. Reading the journal never mutates it.
func (a *App) listDNSRecords(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	journal, journalErr := a.readDNSJournal()
	hosts, owners := a.dnsHostsLocked(journal)
	// Whether a draft that would change what is served is waiting, asked the same
	// way the row tags ask it. The list must not call a name stale while every
	// other page offers no way to publish it: an explanation the operator cannot
	// act on is the same as no explanation.
	stale := false
	if applied, err := a.appliedSnapshotLocked(); err == nil {
		stale = servedUnapplied(a.state, applied)
	}
	views := []dnsRecordView{}
	for _, host := range hosts {
		view := dnsRecordView{Host: host, Status: "未发布", PublicationState: "draft"}
		if journalErr != nil {
			view.PublicationState = "unknown"
		}
		view.AccountChoiceRequired = journalErr == nil && a.dnsAccountChoiceRequiredLocked(host)
		view.AccountID, view.Zone = a.dnsViewOwner(host, journal)
		if account := a.accountByID(view.AccountID); account.ID != "" {
			view.Provider = account.Provider
		}
		base, edge := a.dnsHostKind(host, owners)
		view.Base, view.Edge = base, edge
		if service, ok := owners[host]; ok {
			view.ServiceID, view.ServiceName, view.Enabled = service.ID, service.Name, service.Enabled
			if service.ServesCloudflare() {
				view.EdgeProvider = "cloudflare"
			} else if service.ServesEdge() {
				view.EdgeProvider = "edgeone"
			}
			switch {
			case service.ServesCloudflare():
				view.Message = "该域名经 Cloudflare 应答，解析指向标准入口或第三方优选域名；源站 AAAA 独立更新"
			case edge:
				// An edge name must NOT follow this device's address, and the
				// interface has to say so or the operator will read a stale-looking
				// value as a fault and try to "fix" it into an outage.
				view.Message = "该域名由 EdgeOne 应答，解析指向 EdgeOne 分配的 CNAME，不随本机公网地址变化"
			case service.Enabled && stale:
				view.Message = "有未应用的草稿改动，应用后才能发布或更新解析"
			}
		} else if base {
			view.Enabled = true
			view.Message = "基站域名：本机公网地址变化时只更新这一条记录，其它直接发布的域名都指向它"
			if stale {
				view.Message = "有未应用的草稿改动，应用后才能更新基站解析"
			}
		} else {
			view.Message = "没有服务再使用该域名，关闭并撤回后可从云端删除"
		}
		entry, published := journal.Managed[host]
		if published {
			copy := dnsManagedView{Zone: entry.Zone, RecordID: entry.Record.Key(), Type: entry.Record.Type, Value: entry.Record.Value, TTL: entry.Record.TTL, Action: entry.Action}
			view.Managed = &copy
			view.Type, view.Value = entry.Record.Type, entry.Record.Value
			view.Status = "上次发布"
			view.PublicationState = "published"
		}
		if attempt := dnsAttemptForHost(journal, host); journalErr == nil && attempt != nil {
			if view.Managed == nil {
				view.Managed = &dnsManagedView{Zone: attempt.Zone}
			}
			view.Managed.Pending = true
			view.Status = "待核对"
			view.PublicationState = "unknown"
			view.Message = "上次写入结果尚未核对，请重新预览（只读）"
		}
		views = append(views, view)
	}
	response := map[string]any{"records": views, "autoResolve": a.state.Settings.AutoResolve, "capabilities": dnspod.Supported()}
	if journalErr != nil {
		response["warnings"] = []string{"无法读取本地托管记录，云端状态未知；在修复前不要提交任何 DNS 写入。"}
	}
	reply(w, 200, response)
}

// releasePreviewLocked produces the preview for handing a name over: the
// gateway drops its own claim and writes nothing anywhere. It needs no
// credentials and no zone, because the record at the provider is no longer this
// gateway's to read or change.
//
// Callers hold both dnsMu and mu.
func (a *App) releasePreviewLocked(w http.ResponseWriter, d dnsDesired) {
	journal, err := a.readDNSJournal()
	if err != nil {
		fail(w, 500, "无法读取 DNS 托管记录，已停止云端操作")
		return
	}
	plan, err := planDNS(journal, d, nil)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	token, err := dnsRandomToken()
	if err != nil {
		fail(w, 500, "无法创建安全确认令牌")
		return
	}
	preview := dnsPreview{
		Host: d.Host, Action: plan.Action, Revision: d.Revision, Token: token,
		Warnings: []string{"本操作不会调用 DNS 托管商，也不会改动或删除云端记录；只会解除本程序对该域名的托管。"},
		expires:  time.Now().Add(5 * time.Minute),
	}
	if plan.Action == dnsNoop {
		preview.Warnings = []string{"该域名已不在本程序托管中，无需任何操作。"}
	}
	if a.dnsPending == nil {
		a.dnsPending = map[string]dnsPreview{}
	}
	for candidate, pending := range a.dnsPending {
		if time.Now().After(pending.expires) || pending.Host == d.Host {
			delete(a.dnsPending, candidate)
		}
	}
	if len(a.dnsPending) >= 32 {
		fail(w, 429, "待确认预览过多，请稍后重试")
		return
	}
	a.dnsPending[token] = preview
	reply(w, 200, preview)
}

func (a *App) previewDNSRecord(w http.ResponseWriter, r *http.Request) {
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	host := strings.ToLower(r.PathValue("id"))
	desired, err := a.desiredForHostLocked(r.Context(), host, false)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	if desired.External {
		// The gateway is letting go of this name, which needs no account, no
		// credentials and no cloud call. Reading the provider here would refuse to
		// preview exactly the names whose provider-side record is no longer this
		// gateway's to look after.
		a.releasePreviewLocked(w, desired)
		return
	}
	if desired.Account.ID == "" {
		fail(w, 409, "该域名对应的 DNS 账号已被删除，无法撤回；请在 DNS 托管商控制台手动核对")
		return
	}
	credentials, err := a.accountCredentials(desired.Account.ID)
	if err != nil {
		fail(w, 409, "请先为该 DNS 账号保存可用的凭据")
		return
	}
	j, err := a.readDNSJournal()
	if err != nil {
		fail(w, 500, "无法读取 DNS 托管记录，已停止云端操作")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 18*time.Second)
	defer cancel()
	records, err := a.dnsAPI(credentials).List(ctx, desired.Zone, desired.Sub)
	if err != nil {
		fail(w, 502, "读取 DNS 托管商记录失败；本次预览未修改 DNS。错误详情："+err.Error())
		return
	}
	var resume *dnsAttempt
	if err = a.reconcileDNS(&j, records, desired); err != nil {
		if !canResumeDNSReplacement(j.Attempt, desired) {
			fail(w, 409, err.Error())
			return
		}
		// Ignore only this host's historical attempt while planning. It remains
		// durable until the user explicitly submits this new live preview.
		resume, j.Attempt = j.Attempt, nil
	}
	plan, err := planManualDNS(j, desired, records)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	token, err := dnsRandomToken()
	if err != nil {
		fail(w, 500, "无法创建安全确认令牌")
		return
	}
	warnings := []string{}
	if a.state.Settings.AutoResolve && desired.Enabled {
		warnings = append(warnings, "已开启自动解析：公网地址变化时，本记录会在本机监听核验通过后被自动重新发布。")
	}
	if a.opts.DryRun {
		warnings = append(warnings, "开发 dry-run 模式禁止提交 DNS 写入。")
	}
	if plan.Action == dnsReplace {
		warnings = append(warnings, "强制覆盖只替换本域名列出的冲突记录，不改动其他域名；期间可能短暂中断本域名解析。失败保留快照，不会自动重试，可重新预览后再次手动确认。")
	}
	var recovery *dnsRecovery
	if resume != nil {
		plan = resumableDNSPlan(plan, records)
		recovery = &dnsRecovery{Before: dnsConflictViews(resume.Replacement.Before), Current: dnsConflictViews(records), Phase: resume.Replacement.Phase}
		warnings = append(warnings, "上次覆盖未完成；本次已重新读取云端，历史快照不作为冲突。无需恢复旧记录，可按当前预览手动强制提交；请确认托管商没有仍在执行的旧请求。")
	}
	preview := dnsPreview{
		Host: desired.Host, AccountID: desired.Account.ID, Zone: desired.Zone,
		SubDomain: desired.Sub, Type: desired.Type, Value: desired.Value, TTL: desired.TTL,
		Action: plan.Action, Previous: plan.Previous, Revision: desired.Revision, Token: token, Warnings: warnings,
		Conflicts:         dnsConflictViews(plan.Conflicts),
		RequiresOverwrite: plan.Action == dnsReplace || resume != nil,
		Recovery:          recovery,
		resumeAttempt:     resume,
		expires:           time.Now().Add(5 * time.Minute),
		credentialsHash:   credentials.Fingerprint(),
		snapshot:          records,
	}
	if a.dnsPending == nil {
		a.dnsPending = map[string]dnsPreview{}
	}
	for candidate, pending := range a.dnsPending {
		if time.Now().After(pending.expires) || pending.Host == desired.Host {
			delete(a.dnsPending, candidate)
		}
	}
	if len(a.dnsPending) >= 32 {
		fail(w, 429, "待确认预览过多，请稍后重试")
		return
	}
	a.dnsPending[token] = preview
	reply(w, 200, preview)
}

func (a *App) applyDNSRecord(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Host             string `json:"host"`
		Token            string `json:"token"`
		Revision         int64  `json:"revision"`
		Confirm          bool   `json:"confirm"`
		ConfirmOverwrite bool   `json:"confirmOverwrite"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		fail(w, 400, "必须明确确认 DNS 写入")
		return
	}
	if a.opts.DryRun {
		fail(w, 403, "开发 dry-run 模式禁止提交 DNS 写入")
		return
	}
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	pending, ok := a.dnsPending[in.Token]
	if !ok || !strings.EqualFold(pending.Host, in.Host) || pending.Revision != in.Revision || time.Now().After(pending.expires) {
		fail(w, 409, "DNS 预览已失效，请重新预览后确认")
		return
	}
	if pending.RequiresOverwrite && !in.ConfirmOverwrite {
		fail(w, 400, "必须明确确认覆盖现有 DNS 记录")
		return
	}
	pending.confirmedOverwrite = in.ConfirmOverwrite
	delete(a.dnsPending, in.Token)
	a.mu.Lock()
	defer a.mu.Unlock()
	desired, err := a.desiredForHostLocked(r.Context(), in.Host, false)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	if pending.Action == dnsRelease && pending.AccountID == "" {
		// Handing a name over touches no cloud, so it is re-authorised against
		// local state alone: there are no credentials to compare and no remote
		// snapshot to re-read. It stays bound to the revision it was previewed
		// at, so a configuration change still invalidates it.
		if desired.Revision != pending.Revision || !desired.External {
			fail(w, 409, "配置已变化，请重新预览 DNS")
			return
		}
	} else {
		credentials, err := a.accountCredentials(desired.Account.ID)
		if err != nil || desired.Account.ID != pending.AccountID || desired.Zone != pending.Zone ||
			pending.credentialsHash != credentials.Fingerprint() ||
			desired.Revision != pending.Revision || desired.Value != pending.Value || desired.Sub != pending.SubDomain || desired.Type != pending.Type || desired.TTL != pending.TTL {
			fail(w, 409, "配置、托管域或凭据已变化，请重新预览 DNS")
			return
		}
	}
	if err := a.commitDNS(r.Context(), desired, pending); err != nil {
		var refusal dnsRefusal
		if errors.As(err, &refusal) {
			fail(w, 409, err.Error())
			return
		}
		fail(w, 502, err.Error())
		return
	}
	switch pending.Action {
	case dnsNoop:
		// Nothing was published and nothing needed to be; a record that was never
		// created has no remote ID to report.
		remote := ""
		if pending.Previous != nil {
			remote = pending.Previous.Key()
		}
		reply(w, 200, map[string]any{"ok": true, "action": dnsNoop, "remoteId": remote, "state": "submitted", "message": "云端托管记录已匹配，无需写入；不代表公网已验证。"})
		return
	case dnsRelease:
		reply(w, 200, map[string]any{"ok": true, "action": dnsRelease, "state": "submitted", "message": "已解除本程序对该域名的托管；云端记录未做任何改动。"})
		return
	case dnsAdopt:
		reply(w, 200, map[string]any{"ok": true, "action": dnsAdopt, "state": "submitted", "message": "云端记录已与本程序预期一致，已重新登记托管；本次未向 DNS 托管商发出任何写入。"})
		return
	}
	message := "DNS 托管商已接受 " + pending.Type + " 记录提交；DNS 传播和外部访问仍待独立验证。"
	if pending.Action == dnsDelete {
		message = "DNS 托管商已接受删除请求；传播完成前旧解析可能仍然生效。"
	}
	reply(w, 200, map[string]any{"ok": true, "action": pending.Action, "state": "submitted", "message": message})
}

// commitDNS re-reads the provider, proves nothing changed since the preview and
// then performs exactly the change the preview authorised. Every check that runs
// before the write returns a dnsRefusal, so the caller can tell the operator
// whether the cloud was touched at all.
//
// Callers hold both dnsMu and mu.
func (a *App) commitDNS(ctx context.Context, desired dnsDesired, pending dnsPreview) error {
	j, err := a.readDNSJournal()
	if err != nil {
		return refuse("无法读取 DNS 托管记录，已停止云端操作")
	}
	selectDNSAttempt(&j, desired.Host, desired.Account.ID, desired.Zone)
	if !reflect.DeepEqual(j.Attempt, pending.resumeAttempt) {
		return refuse("上次 DNS 写入结果待核对，请先重新预览；不会重复写入")
	}
	if desired.External {
		// Nothing is read from the provider because nothing is written to it, so
		// the credential and snapshot checks below do not apply.
		plan, err := planDNS(j, desired, nil)
		if err != nil || plan.Action != pending.Action {
			return refuse("DNS 托管状态发生变化，未写入；请重新预览")
		}
		if plan.Action == dnsNoop {
			return nil
		}
		return a.releaseDNS(&j, desired)
	}
	credentials, err := a.accountCredentials(desired.Account.ID)
	if err != nil {
		return refuse("凭据不可用，未调用云端")
	}
	client := a.dnsAPI(credentials)
	records, err := client.List(ctx, desired.Zone, pending.SubDomain)
	if err != nil {
		return refuse("确认前读取 DNS 托管商 失败；尚未发出写请求：%s", err.Error())
	}
	if !sameDNSRecords(records, pending.snapshot) {
		return refuse("云端记录在预览后发生变化，未写入；请重新预览")
	}
	planning := j
	if pending.resumeAttempt != nil {
		if !canResumeDNSReplacement(j.Attempt, desired) {
			return refuse("上次覆盖状态已变化，请重新预览")
		}
		planning.Attempt = nil
	}
	plan, err := planDNS(planning, desired, records)
	if pending.RequiresOverwrite {
		if !pending.confirmedOverwrite {
			return refuse("必须明确确认覆盖现有 DNS 记录")
		}
		plan, err = planManualDNS(planning, desired, records)
	}
	if pending.resumeAttempt != nil && err == nil {
		plan = resumableDNSPlan(plan, records)
	}
	if err != nil || plan.Action != pending.Action {
		return refuse("DNS 托管状态发生变化，未写入；请重新预览")
	}
	if plan.Action == dnsNoop {
		return nil
	}
	if pending.resumeAttempt != nil {
		if err := a.dnsMigrationBlocksHost(desired.Host); err != nil {
			return refuse("无法继续覆盖：%s", err)
		}
		if err := a.authorizeDNSWrite(desired.Account.ID); err != nil {
			return refuse("无法继续覆盖：%s", err)
		}
		if err := a.archiveDNSReplacement(j.Attempt); err != nil {
			return refuse("无法归档上次覆盖快照，未写入：%s", err)
		}
		// Do not save the cleared attempt on its own: publish persists either a
		// new write-ahead attempt or the completed local-only adoption atomically.
		j.Attempt = nil
		if plan.Action == dnsCreate || plan.Action == dnsUpdate {
			plan.Replacement = &dnsReplacement{Before: append([]dnspod.Record(nil), records...), Phase: "create"}
		}
	}
	if plan.Action == dnsReplace {
		return a.publishDNSReplacement(ctx, client, &j, desired, plan)
	}
	return a.publish(ctx, client, &j, desired, plan)
}

// updateSettings changes the runtime preferences that are not part of the
// rendered configuration. The revision deliberately does not move: an applied
// snapshot stays valid, because nothing the proxy renders has changed.
//
// The base station name and its TTL are here rather than in the service model
// because they are not a service: no port, no upstream, no server block. They
// only decide what the DNS records say.
func (a *App) updateSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AutoResolve *bool   `json:"autoResolve"`
		BaseDomain  *string `json:"baseDomain"`
		BaseTTL     *int    `json:"baseTtl"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.AutoResolve == nil && in.BaseDomain == nil && in.BaseTTL == nil {
		fail(w, 400, "没有需要更新的设置")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	base := strings.ToLower(strings.TrimSpace(a.state.Settings.BaseDomain))
	ttl := a.state.Settings.EffectiveBaseTTL()
	if in.BaseDomain != nil {
		base = strings.ToLower(strings.TrimSpace(*in.BaseDomain))
		if refusal := a.baseStationChangeRefusalLocked(base); refusal != "" {
			fail(w, 400, refusal)
			return
		}
		if base != "" {
			if !engine.ValidDomain(base) || !strings.Contains(base, ".") {
				fail(w, 400, "基站域名必须是一个完整的域名，例如 gateway.example.com")
				return
			}
			// A name that a service already claims would be written twice: once as
			// the base station's own address record and once as that service's
			// record. Whichever ran last would win, and the failure would appear as
			// an intermittent one.
			if _, claimed := a.serviceForHost(base); claimed {
				fail(w, 400, "基站域名不能同时是某个服务的域名，请换一个专用名称（例如 gateway 前缀）")
				return
			}
		}
	}
	if in.BaseTTL != nil {
		ttl = *in.BaseTTL
		if !dnspod.TTLAllowed(ttl) {
			fail(w, 400, "基站 TTL 只支持 60、300、600 秒；60 与 300 需要域名套餐支持，600 是每个套餐都接受的值")
			return
		}
	}
	old := clone(a.state)
	a.state.Settings.BaseDomain, a.state.Settings.BaseTTL = base, ttl
	if in.AutoResolve != nil {
		a.state.Settings.AutoResolve = *in.AutoResolve
	}
	if err := a.saveLocked(); err != nil {
		a.state = old
		fail(w, 500, "无法保存配置")
		return
	}
	// The zone memo is unaffected (no account or zone changed), but a pending DNS
	// preview is bound to a revision and a value, so it must not survive a change
	// to what those would now be.
	a.dnsPending = nil
	a.replyStateLocked(w)
}

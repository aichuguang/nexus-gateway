package app

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"time"

	"w103d/gateway/internal/dnsprovider"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

type dnsAccountChoiceError struct{ message string }

func (e *dnsAccountChoiceError) Error() string { return e.message }

type dnsAccountChoice struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Provider    string `json:"provider"`
	Zone        string `json:"zone"`
}

type dnsRecordAccounts struct {
	Host              string             `json:"host"`
	Revision          int64              `json:"revision"`
	Status            string             `json:"status"`
	Choices           []dnsAccountChoice `json:"choices"`
	SelectedAccountID string             `json:"selectedAccountId,omitempty"`
	Message           string             `json:"message"`
}

func (a *App) dnsBindingForHost(host string) *model.DNSBinding {
	var best *model.DNSBinding
	for i := range a.state.DNSBindings {
		b := &a.state.DNSBindings[i]
		if host == b.Host || !b.Exact && strings.HasSuffix(host, "."+b.Host) {
			if best == nil || len(b.Host) > len(best.Host) || len(b.Host) == len(best.Host) && b.Exact {
				best = b
			}
		}
	}
	if best != nil && best.Automatic {
		return nil
	}
	return best
}

func dnsLongestZoneEntries(host string, entries []zoneEntry) []zoneEntry {
	best := []zoneEntry{}
	zone := ""
	seen := map[string]bool{}
	for _, entry := range entries {
		if host != entry.Zone && !strings.HasSuffix(host, "."+entry.Zone) {
			continue
		}
		if len(entry.Zone) > len(zone) {
			zone, best, seen = entry.Zone, []zoneEntry{}, map[string]bool{}
		}
		if entry.Zone == zone && !seen[entry.AccountID] {
			best = append(best, entry)
			seen[entry.AccountID] = true
		}
	}
	return best
}

// Complete means all enabled accounts were read successfully. A timeout or a
// missing credential must not make a competing account disappear silently.
func (a *App) dnsRecordAccountsFromEntries(host string, entries []zoneEntry, complete bool) dnsRecordAccounts {
	out := dnsRecordAccounts{Host: host, Revision: a.state.Revision, Status: "unavailable", Choices: []dnsAccountChoice{}}
	for _, entry := range dnsLongestZoneEntries(host, entries) {
		account := a.accountByID(entry.AccountID)
		if account.ID == "" || !account.Enabled {
			continue
		}
		out.Choices = append(out.Choices, dnsAccountChoice{AccountID: account.ID, AccountName: account.Name, Provider: account.Provider, Zone: entry.Zone})
	}
	binding := a.dnsBindingForHost(host)
	if binding != nil {
		for _, entry := range entries {
			if entry.AccountID != binding.AccountID || entry.Zone != binding.Zone {
				continue
			}
			account := a.accountByID(entry.AccountID)
			if _, err := labelInZone(host, entry.Zone); err == nil && account.ID != "" && account.Enabled {
				out.Status, out.SelectedAccountID = "selected", account.ID
				out.Message = "继续使用已选择的解析账号，无需重复设置"
				return out
			}
		}
	}
	if !complete {
		out.Message = "部分 DNS 账号读取失败，无法确定解析账号，请先检查凭据与网络"
		return out
	}
	if len(out.Choices) == 0 {
		out.Message = "没有可管理该域名的已启用解析账号，请先检查账号和域名托管平台"
		return out
	}
	if binding != nil {
		out.Status, out.Message = "choice-required", "原解析账号或托管域已不可用，请重新选择；不会自动迁移已有云端记录"
		return out
	}
	if len(out.Choices) > 1 {
		out.Status, out.Message = "choice-required", "多个账号可管理同一托管域，请选择实际生效的解析账号"
		return out
	}
	out.Status, out.SelectedAccountID = "automatic", out.Choices[0].AccountID
	out.Message = "已自动匹配唯一解析账号，无需手动设置"
	return out
}

func (a *App) dnsAccountChoiceRequiredLocked(host string) bool {
	if len(a.zoneMemo) == 0 || time.Since(a.zoneMemoAt) >= 30*time.Minute {
		return false
	}
	return a.dnsRecordAccountsFromEntries(host, a.zoneMemo, true).Status == "choice-required"
}

func (a *App) readDNSRecordAccountsLocked(ctx context.Context, host string) dnsRecordAccounts {
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	entries, warnings, err := a.discoverDNSZonesLocked(ctx)
	return a.dnsRecordAccountsFromEntries(host, entries, err == nil && len(warnings) == 0)
}

// The endpoint is intentionally restricted to names already shown in the DNS
// list, rather than creating a second, independent binding-management surface.
func (a *App) knownDNSRecordLocked(w http.ResponseWriter, host string) bool {
	if !engine.ValidDomain(host) || !strings.Contains(host, ".") {
		fail(w, 400, "域名格式不正确")
		return false
	}
	journal, err := a.readDNSJournal()
	if err != nil {
		fail(w, 409, "无法读取本地托管记录，暂不能确定解析账号")
		return false
	}
	hosts, _ := a.dnsHostsLocked(journal)
	for _, candidate := range hosts {
		if candidate == host {
			return true
		}
	}
	fail(w, 404, "解析记录不存在")
	return false
}

func (a *App) listDNSRecordAccounts(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(r.PathValue("id")), "."))
	if !a.knownDNSRecordLocked(w, host) {
		return
	}
	reply(w, 200, a.readDNSRecordAccountsLocked(r.Context(), host))
}

func (a *App) selectDNSRecordAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision  int64  `json:"revision"`
		AccountID string `json:"accountId"`
		Zone      string `json:"zone"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(r.PathValue("id")), "."))
	if !a.knownDNSRecordLocked(w, host) {
		return
	}
	if in.Revision != a.state.Revision {
		fail(w, 409, "配置已变化，请重新读取可选解析账号")
		return
	}
	current := a.readDNSRecordAccountsLocked(r.Context(), host)
	if current.Status != "choice-required" {
		fail(w, 409, current.Message)
		return
	}
	var selected *dnsAccountChoice
	for i := range current.Choices {
		choice := &current.Choices[i]
		if choice.AccountID == in.AccountID && choice.Zone == in.Zone {
			selected = choice
			break
		}
	}
	if selected == nil {
		fail(w, 409, "所选解析账号或托管域已不在当前候选中，请重新读取")
		return
	}
	if err := a.dnsMigrationBlocksHost(host); err != nil {
		fail(w, 409, err.Error())
		return
	}
	journal, err := a.readDNSJournal()
	if err != nil {
		fail(w, 500, "Cannot inspect managed ownership")
		return
	}
	if managed, ok := journal.Managed[host]; ok && (managed.AccountID != selected.AccountID || managed.Zone != selected.Zone) {
		fail(w, 409, "Changing the account or zone of an existing managed DNS record is not supported")
		return
	}
	old := clone(a.state)
	binding := model.DNSBinding{Host: host, AccountID: selected.AccountID, Zone: selected.Zone, Exact: true}
	replaced := false
	for i := range a.state.DNSBindings {
		if a.state.DNSBindings[i].Host == host && a.state.DNSBindings[i].Exact {
			a.state.DNSBindings[i], replaced = binding, true
			break
		}
	}
	if !replaced {
		if len(a.state.DNSBindings) >= 512 {
			fail(w, 409, "解析账号选择数量已达上限，请先检查旧配置")
			return
		}
		a.state.DNSBindings = append(a.state.DNSBindings, binding)
	}
	a.state.Revision++
	if err := a.saveLocked(); err != nil {
		a.state = old
		fail(w, 500, "无法保存解析账号选择")
		return
	}
	a.zoneMemo, a.dnsPending = nil, nil
	current.Status, current.SelectedAccountID, current.Revision = "selected", selected.AccountID, a.state.Revision
	current.Message = "解析账号已选择；未修改或迁移云端记录，发布仍需单独预览确认"
	reply(w, 200, current)
}

func (a *App) updateDNSAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      *string `json:"name"`
		Enabled   *bool   `json:"enabled"`
		SecretID  *string `json:"secretId"`
		SecretKey *string `json:"secretKey"`
		APIToken  *string `json:"apiToken"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	i := a.accountIndex(r.PathValue("id"))
	if i < 0 {
		fail(w, 404, "DNS 账号不存在")
		return
	}
	if in.Name == nil && in.Enabled == nil && in.SecretID == nil && in.SecretKey == nil && in.APIToken == nil {
		fail(w, 400, "没有需要更新的字段")
		return
	}
	account := a.state.DNSAccounts[i]
	if in.Name != nil {
		account.Name = strings.TrimSpace(*in.Name)
		if account.Name == "" || len(account.Name) > 100 {
			fail(w, 400, "账号名称不能为空且不能超过 100 字节")
			return
		}
	}
	if in.Enabled != nil {
		account.Enabled = *in.Enabled
	}
	rotate := in.SecretID != nil || in.SecretKey != nil || in.APIToken != nil
	if rotate {
		credentials := dnsprovider.Credentials{Provider: account.Provider}
		if in.SecretID != nil {
			credentials.SecretID = strings.TrimSpace(*in.SecretID)
		}
		if in.SecretKey != nil {
			credentials.SecretKey = strings.TrimSpace(*in.SecretKey)
		}
		if in.APIToken != nil {
			credentials.APIToken = strings.TrimSpace(*in.APIToken)
		}
		if !credentials.Valid() {
			fail(w, 400, "轮换凭据时必须提交该托管商的一组完整有效凭据")
			return
		}
		journal, err := a.readDNSJournal()
		if err != nil {
			fail(w, 409, "Cannot verify existing DNS ownership")
			return
		}
		txt, err := a.readDNSTXTJournal()
		if err != nil {
			fail(w, 409, "Cannot verify existing TXT ownership")
			return
		}
		required := map[string]bool{}
		for _, binding := range a.state.DNSBindings {
			if binding.AccountID == account.ID {
				required[binding.Zone] = true
			}
		}
		for _, entry := range journal.Managed {
			if entry.AccountID == account.ID {
				required[entry.Zone] = true
			}
		}
		for _, entry := range txt.Managed {
			if entry.AccountID == account.ID {
				required[entry.Zone] = true
			}
		}
		if journal.Attempt != nil && journal.Attempt.AccountID == account.ID {
			required[journal.Attempt.Zone] = true
		}
		if txt.Attempt != nil && txt.Attempt.AccountID == account.ID {
			required[txt.Attempt.Zone] = true
		}
		for _, attempt := range journal.Attempts {
			if attempt.AccountID == account.ID {
				required[attempt.Zone] = true
			}
		}
		for _, attempt := range txt.Attempts {
			if attempt.AccountID == account.ID {
				required[attempt.Zone] = true
			}
		}
		revision := a.state.Revision
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		a.mu.Unlock()
		zones, err := a.dnsAPI(credentials).Zones(ctx)
		a.mu.Lock()
		if a.state.Revision != revision {
			fail(w, 409, "Configuration changed during credential verification")
			return
		}
		if err != nil {
			fail(w, 400, "New credentials could not read DNS zones")
			return
		}
		for _, zone := range zones {
			delete(required, strings.TrimSuffix(strings.ToLower(zone), "."))
		}
		if len(required) > 0 {
			fail(w, 409, "New credentials cannot manage all existing bound zones")
			return
		}
		account.CredentialFile, err = a.stageCredential("dns", account.ID, credentials)
		if err != nil {
			fail(w, 500, "无法安全保存 DNS 凭据")
			return
		}
		account.CredentialVersion++
		account.SecretIDHint = credentials.Hint()
	}
	old := clone(a.state)
	a.state.DNSAccounts[i] = account
	a.state.Revision++
	if err := a.saveLocked(); err != nil {
		a.state = old
		fail(w, 500, "无法保存账号配置")
		return
	}
	a.zoneMemo, a.dnsPending = nil, nil
	a.replyStateLocked(w)
}

// Bindings belong to DNS management, never to an edge provider. A host binding
// applies to descendants, with the most-specific binding winning.
func (a *App) handleDNSBindings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.mu.Lock()
		defer a.mu.Unlock()
		bindings := a.state.DNSBindings
		if bindings == nil {
			bindings = []model.DNSBinding{}
		}
		reply(w, 200, map[string]any{"bindings": bindings, "revision": a.state.Revision})
		return
	}
	var in struct {
		Bindings []model.DNSBinding `json:"bindings"`
		Revision int64              `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Bindings == nil || len(in.Bindings) > 512 {
		fail(w, 400, "请提交不超过 512 条的 bindings 数组，清空请使用空数组")
		return
	}
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if in.Revision != a.state.Revision {
		fail(w, 409, "DNS bindings changed; reload before replacing them")
		return
	}
	seen := map[string]bool{}
	for i := range in.Bindings {
		b := &in.Bindings[i]
		b.Host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(b.Host), "."))
		b.Zone = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(b.Zone), "."))
		bindingKey := b.Host
		if b.Exact {
			bindingKey += ":exact"
		}
		if b.Automatic {
			if !b.Exact || !engine.ValidDomain(b.Host) || seen[bindingKey] || b.AccountID != "" || b.Zone != "" {
				fail(w, 400, "Invalid automatic DNS override")
				return
			}
			seen[bindingKey] = true
			continue
		}
		if !engine.ValidDomain(b.Host) || !strings.Contains(b.Host, ".") || !engine.ValidDomain(b.Zone) || !strings.Contains(b.Zone, ".") || seen[bindingKey] {
			fail(w, 400, "DNS 归属的域名/托管域格式错误或域名重复")
			return
		}
		seen[bindingKey] = true
		if _, e := labelInZone(b.Host, b.Zone); e != nil {
			fail(w, 400, e.Error())
			return
		}
		index := a.accountIndex(b.AccountID)
		if index < 0 || !a.state.DNSAccounts[index].Enabled {
			fail(w, 400, "DNS 归属必须选择存在且启用的账号")
			return
		}
	}
	old := clone(a.state)
	a.state.DNSBindings = in.Bindings
	journal, err := a.readDNSJournal()
	if err != nil {
		a.state = old
		fail(w, 500, "Cannot inspect managed DNS ownership")
		return
	}
	for host, managed := range journal.Managed {
		selected := a.dnsBindingForHost(host)
		oldBindings := a.state.DNSBindings
		a.state.DNSBindings = old.DNSBindings
		previous := a.dnsBindingForHost(host)
		a.state.DNSBindings = oldBindings
		if reflect.DeepEqual(previous, selected) {
			continue
		}
		if selected == nil || selected.AccountID != managed.AccountID || selected.Zone != managed.Zone {
			a.state = old
			fail(w, 409, "Changing the account or zone of an existing managed DNS record is not supported")
			return
		}
	}
	a.state.Revision++
	if err := a.saveLocked(); err != nil {
		a.state = old
		fail(w, 500, "无法保存 DNS 归属")
		return
	}
	a.dnsPending = nil
	reply(w, 200, map[string]any{"bindings": a.state.DNSBindings, "revision": a.state.Revision})
}

// dnsViewOwner uses only local metadata; rendering the list must not perform
// cloud discovery or make the page unavailable during an API outage.
func (a *App) dnsViewOwner(host string, j dnsJournal) (string, string) {
	best := a.dnsBindingForHost(host)
	if best != nil {
		return best.AccountID, best.Zone
	}
	if m, ok := j.Managed[host]; ok {
		return m.AccountID, m.Zone
	}
	var match zoneEntry
	ambiguous := false
	for _, e := range a.zoneMemo {
		if host == e.Zone || strings.HasSuffix(host, "."+e.Zone) {
			if len(e.Zone) > len(match.Zone) {
				match = e
				ambiguous = false
			} else if e.Zone == match.Zone && e.AccountID != match.AccountID {
				ambiguous = true
			}
		}
	}
	if !ambiguous {
		return match.AccountID, match.Zone
	}
	return "", ""
}
